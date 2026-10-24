package install

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"filippo.io/age"
	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/backup"
	"github.com/norvexlabs/stowage/internal/providers"
)

// The VM's backups (a pg-age-rclone `backup`): the host daemon dumps the database inside its own
// service (pg_dump of the server's own version), encrypts the dump to the operator's age
// recipients as it streams, and hands it to the backup runner (rclone, preloaded with the
// release), which uploads it to the destination: nothing is written to the VM's disk, and the VM
// holds no key that decrypts a backup. The settings are the operator's, on the persistent
// partition; a systemd timer runs a backup on their schedule.
//
//	GET    /v1/backup/config  the settings, the rclone config only said to be set
//	POST   /v1/backup/config  {destination, rclone_config, recipients, retention_days, schedule,
//	                          profile}: change them; a field left out keeps its value
//	DELETE /v1/backup/config  drop them: no backup runs
//	POST   /v1/backup         {profile, if_configured}: a backup job (202, {job, target}); with
//	                          if_configured, nothing (200, {skipped}) without settings, before
//	                          the install, or when the release runs no backup
//	GET    /v1/backup/status  the settings, the timer's next run, the last backup
//	GET    /v1/backup/list    the destination's backups of this project
//	POST   /v1/backup/verify  write a probe to the destination: the settings reach it; a body of
//	                          /v1/backup/config's fields tries those over the saved settings (or
//	                          none), saving nothing

const (
	defaultBackupSchedule = "*-*-* 03:00:00 UTC"
	defaultRetentionDays  = 30
	maxRetentionDays      = 3650
	maxRecipients         = 16
	maxRcloneConfig       = 32 << 10 // within a request body's 64 KiB
	// backupTimeout bounds a backup (dump, upload, check and pruning); rcloneTimeout one of the
	// runner's short calls (list, verify).
	backupTimeout = time.Hour
	rcloneTimeout = time.Minute
	// scheduledBackupWait is how long, in Unit, the timer's run waits for a running job to end:
	// with the backup's own hour, within the unit's 75 minutes.
	scheduledBackupWait = 10 * 60
	// backupStopWait is how long the daemon's stop waits for a backup's cleanup (two runner
	// calls), within its unit's TimeoutStopSec.
	backupStopWait = 2*rcloneTimeout + 15*time.Second
)

// BackupConfig is the backup settings file, root's alone.
type BackupConfig struct {
	// Destination is where backups go: an rclone remote of RcloneConfig, and a path in it.
	Destination  string `yaml:"destination"`
	RcloneConfig string `yaml:"rclone_config"`
	// Recipients are the age public keys a backup is encrypted to: any one's private key
	// decrypts it.
	Recipients []string `yaml:"recipients"`
	// RetentionDays: a backup of the same profile older than this is deleted after a backup; 0
	// keeps every one.
	RetentionDays int `yaml:"retention_days"`
	// Schedule is the timer's systemd calendar event.
	Schedule string `yaml:"schedule"`
	// Profile is the profile a scheduled backup takes.
	Profile string `yaml:"profile"`
}

// BackupSettings is what GET /v1/backup/config answers: the settings, the rclone config's secrets
// left out.
type BackupSettings struct {
	Configured      bool     `json:"configured"`
	Destination     string   `json:"destination,omitempty"`
	RcloneConfigSet bool     `json:"rclone_config_set"`
	Recipients      []string `json:"recipients"`
	RetentionDays   int      `json:"retention_days"`
	Schedule        string   `json:"schedule"`
	Profile         string   `json:"profile"`
	// Locked: the settings are the operator's; the app socket may not change them.
	Locked bool `json:"locked"`
	// Profiles are the ones the release declares.
	Profiles []string `json:"profiles"`
}

// BackupConfigRequest is POST /v1/backup/config's body: a field left out keeps its value
// (recipients: an empty list too).
type BackupConfigRequest struct {
	Destination   *string  `json:"destination,omitempty"`
	RcloneConfig  *string  `json:"rclone_config,omitempty"`
	Recipients    []string `json:"recipients,omitempty"`
	RetentionDays *int     `json:"retention_days,omitempty"`
	Schedule      *string  `json:"schedule,omitempty"`
	Profile       *string  `json:"profile,omitempty"`
	// Lock locks (true) or unlocks the settings against the app socket; the operator's alone,
	// kept apart from the settings (BackupLockFile).
	Lock *bool `json:"lock,omitempty"`
}

// BackupRequest is POST /v1/backup's body.
type BackupRequest struct {
	Profile string `json:"profile,omitempty"` // default: the settings'
	// IfConfigured: without settings, do nothing rather than refuse (the timer's run).
	IfConfigured bool `json:"if_configured,omitempty"`
}

// BackupRun is a backup that ran, the last one kept on the persistent partition.
type BackupRun struct {
	Object   string    `json:"object"`
	Profile  string    `json:"profile"`
	Release  Version   `json:"release"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Bytes    int64     `json:"bytes,omitempty"`
	// Error is why it failed; empty when it succeeded. PruneError is why the pruning of older
	// backups failed, after a backup that succeeded.
	Error      string `json:"error,omitempty"`
	PruneError string `json:"prune_error,omitempty"`
}

// BackupStatus is what GET /v1/backup/status answers.
type BackupStatus struct {
	Settings BackupSettings `json:"settings"`
	// Next is the timer's next run, as systemd prints it; empty when it has none.
	Next    string     `json:"next,omitempty"`
	Running bool       `json:"running"`
	Last    *BackupRun `json:"last,omitempty"`
}

// BackupObject is one backup in the destination.
type BackupObject struct {
	Name string    `json:"name"`
	Size int64     `json:"size"`
	Time time.Time `json:"time"`
}

// BackupFile and BackupLastFile hold the settings and the last backup, in the install's directory.
func BackupFile(dir string) string     { return filepath.Join(dir, "backup.yaml") }
func BackupLastFile(dir string) string { return filepath.Join(dir, "backup-last.json") }

// BackupLockFile marks the backup settings locked against the app socket. It is no part of the
// settings: removing them keeps it.
func BackupLockFile(dir string) string { return filepath.Join(dir, "backup-settings.lock") }

// remoteName is an rclone remote's name: a section of its config.
var remoteName = regexp.MustCompile(`^[A-Za-z0-9_+@][A-Za-z0-9_.+@ -]{0,63}$`)

// pgBackup is the release's pg-age-rclone backup; a 409 when it declares none, or when the OS
// carries no runner.
func (h *Hostd) pgBackup() (*providers.PgAgeRclone, error) {
	s := h.R.Spec
	if !providers.RunsBackups(s) {
		return nil, fail(http.StatusConflict, "%s declares no pg-age-rclone backup: the install runs none", s.Name)
	}
	pg, err := providers.DecodeBackup(s.Backup)
	if err != nil {
		return nil, err
	}
	if h.R.Manifest.Artifacts.Images[backup.Key] == "" {
		return nil, fail(http.StatusConflict, "this release carries no backup runner")
	}
	return pg, nil
}

// readBackup strictly decodes the settings and checks them; none is nil.
func (h *Hostd) readBackup(pg *providers.PgAgeRclone) (*BackupConfig, error) {
	c, err := h.decodeBackup()
	if err != nil || c == nil {
		return c, err
	}
	if _, err := validBackup(*c, pg); err != nil {
		return nil, fmt.Errorf("%s: %w (`%s backup config` changes it)", BackupFile(h.O.Dir), err, h.R.Ctl())
	}
	return c, nil
}

// decodeBackup strictly decodes the settings, unchecked: a change starts from them though a
// newer release no longer has their profile.
func (h *Hostd) decodeBackup() (*BackupConfig, error) {
	path := BackupFile(h.O.Dir)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c BackupConfig
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w (`%s backup config --remove` drops it)", path, err, h.R.Ctl())
	}
	return &c, nil
}

// writeBackup records c, root's alone.
func (h *Hostd) writeBackup(c BackupConfig) error {
	raw, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	head := fmt.Sprintf("# %s's backup settings, which `%s backup config` changes. The rclone config holds the destination's credentials.\n", h.R.Spec.Name, h.R.Ctl())
	return writePrivate(BackupFile(h.O.Dir), append([]byte(head), raw...))
}

// validBackup checks settings complete and well-formed, and returns their recipients. The
// schedule's syntax is systemd's to judge (checkSchedule).
func validBackup(c BackupConfig, pg *providers.PgAgeRclone) ([]age.Recipient, error) {
	remote, path, ok := strings.Cut(c.Destination, ":")
	switch {
	case c.Destination == "":
		return nil, errors.New("destination is required: an rclone remote and a path, remote:path")
	case !ok || !remoteName.MatchString(remote):
		return nil, fmt.Errorf("destination %q must be an rclone remote of the rclone config and a path, remote:path", c.Destination)
	case len(c.Destination) > 1024 || strings.ContainsFunc(path, isControl):
		return nil, errors.New("destination: at most 1024 bytes, no control character")
	case c.RcloneConfig == "":
		return nil, errors.New("the rclone config is required: it defines the destination's remote and its credentials")
	case len(c.RcloneConfig) > maxRcloneConfig || strings.ContainsRune(c.RcloneConfig, 0):
		return nil, fmt.Errorf("the rclone config: at most %d bytes, no NUL", maxRcloneConfig)
	case !slices.Contains(rcloneRemotes(c.RcloneConfig), remote):
		return nil, fmt.Errorf("the rclone config defines no remote %q (its sections: %s)", remote, strings.Join(rcloneRemotes(c.RcloneConfig), ", "))
	case c.RetentionDays < 0 || c.RetentionDays > maxRetentionDays:
		return nil, fmt.Errorf("retention_days %d: 0 (keep every backup) to %d", c.RetentionDays, maxRetentionDays)
	}
	if err := validSchedule(c.Schedule); err != nil {
		return nil, err
	}
	if _, ok := pg.Profiles[c.Profile]; !ok {
		return nil, fmt.Errorf("profile %q is not one of the release's: %s", c.Profile, strings.Join(sortedKeys(pg.Profiles), ", "))
	}
	return parseRecipients(c.Recipients, maxRecipients)
}

// validSchedule refuses a schedule a unit file cannot hold as it is; its syntax is systemd's to
// judge (checkSchedule).
func validSchedule(s string) error {
	if s == "" || len(s) > 256 || strings.ContainsFunc(s, isControl) || strings.ContainsAny(s, `%\`) {
		return fmt.Errorf("schedule %q: a systemd calendar event (e.g. %q), at most 256 bytes, without %% or \\", s, defaultBackupSchedule)
	}
	return nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// rcloneRemotes lists the sections of an rclone config: its remotes.
func rcloneRemotes(conf string) []string {
	var out []string
	for _, l := range strings.Split(conf, "\n") {
		l = strings.TrimSpace(l)
		if len(l) > 2 && l[0] == '[' && l[len(l)-1] == ']' {
			out = append(out, strings.TrimSpace(l[1:len(l)-1]))
		}
	}
	return out
}

// backupSettings is what GET /v1/backup/config shows of c (nil: none).
func backupSettings(c *BackupConfig, pg *providers.PgAgeRclone, locked bool) BackupSettings {
	s := BackupSettings{Recipients: []string{}, RetentionDays: defaultRetentionDays, Schedule: defaultBackupSchedule, Profile: pg.Default, Profiles: sortedKeys(pg.Profiles), Locked: locked}
	if c != nil {
		s = BackupSettings{Configured: true, Destination: c.Destination, RcloneConfigSet: c.RcloneConfig != "", Recipients: c.Recipients,
			RetentionDays: c.RetentionDays, Schedule: c.Schedule, Profile: c.Profile, Profiles: s.Profiles, Locked: locked}
	}
	return s
}

// settingsLocked is whether the operator locked the settings (BackupLockFile): a lock it
// cannot read counts as one.
func (h *Hostd) settingsLocked() bool {
	_, err := os.Stat(BackupLockFile(h.O.Dir))
	return !errors.Is(err, os.ErrNotExist)
}

// setLock sets the lock, or removes it.
func (h *Hostd) setLock(on bool) error {
	if !on {
		if err := os.Remove(BackupLockFile(h.O.Dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return writeAtomic(BackupLockFile(h.O.Dir), strings.NewReader("the backup settings are the operator's: `backup config --unlock` gives them back to the app socket\n"), 0o600, nil)
}

// setsAny is whether req changes a setting (Lock is none).
func (req BackupConfigRequest) setsAny() bool {
	return req.Destination != nil || req.RcloneConfig != nil || len(req.Recipients) > 0 || req.RetentionDays != nil || req.Schedule != nil || req.Profile != nil
}

// sameRecipients is whether a and b are one set of keys, whatever their order.
func sameRecipients(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

func (h *Hostd) backupConfig(w http.ResponseWriter, r *http.Request) {
	pg, err := h.pgBackup()
	if err != nil {
		replyErr(w, err)
		return
	}
	c, err := h.readBackup(pg)
	if err != nil {
		replyErr(w, err)
		return
	}
	reply(w, http.StatusOK, backupSettings(c, pg, h.settingsLocked()))
}

func (h *Hostd) setBackupConfig(w http.ResponseWriter, r *http.Request) {
	var req BackupConfigRequest
	if err := decodeBody(r, &req); err != nil {
		replyErr(w, err)
		return
	}
	pg, err := h.pgBackup()
	if err != nil {
		replyErr(w, err)
		return
	}
	h.backupMu.Lock()
	defer h.backupMu.Unlock()
	cur, err := h.decodeBackup()
	if err != nil {
		replyErr(w, err)
		return
	}
	// Through the app socket, while the operator locked them, the settings stay as they are (a
	// console saving its whole form unchanged passes): the lock is the operator's to set. The
	// operator's lock is written before the settings, an unlock after them, so a request refused
	// or cut halfway leaves them locked.
	app, locked := fromApp(r), h.settingsLocked()
	if req.Lock != nil && app {
		replyErr(w, fail(http.StatusForbidden, "the backup settings' lock is the operator's: backup config --lock over SSH"))
		return
	}
	c := mergeBackup(cur, req, pg)
	if app && locked && backupChanges(cur, c) != "" {
		replyErr(w, fail(http.StatusForbidden, "the backup settings are locked: the operator changes them, backup config over SSH"))
		return
	}
	if req.Lock != nil && !req.setsAny() {
		if err := h.setLock(*req.Lock); err != nil {
			replyErr(w, err)
			return
		}
		reply(w, http.StatusOK, backupSettings(cur, pg, *req.Lock))
		return
	}
	if _, err := validBackup(c, pg); err != nil {
		replyErr(w, fail(http.StatusBadRequest, "%v", err))
		return
	}
	if err := h.checkSchedule(r.Context(), c.Schedule); err != nil {
		replyErr(w, err)
		return
	}
	if req.Lock != nil && *req.Lock {
		if err := h.setLock(true); err != nil {
			replyErr(w, err)
			return
		}
		locked = true
	}
	if err := h.writeBackup(c); err != nil {
		replyErr(w, err)
		return
	}
	if changed := backupChanges(cur, c); app && changed != "" {
		// The operator reads what the app changed in the journal (`events`).
		if _, err := h.event(Event{Kind: evBackupConfig, Reason: "changed through the app socket: " + changed}); err != nil {
			h.O.logf("backup settings: the event: %v", err)
		}
	}
	if err := h.applySchedule(r.Context(), &c); err != nil {
		replyErr(w, fmt.Errorf("the settings are saved, but the timer did not take the schedule: %w", err))
		return
	}
	if req.Lock != nil && !*req.Lock {
		if err := h.setLock(false); err != nil {
			replyErr(w, fmt.Errorf("the settings are saved and applied, but they stay locked: %w", err))
			return
		}
		locked = false
	}
	reply(w, http.StatusOK, backupSettings(&c, pg, locked))
}

// mergeBackup is the settings req makes of cur (nil: none yet, the defaults): a field req leaves
// out keeps its value.
func mergeBackup(cur *BackupConfig, req BackupConfigRequest, pg *providers.PgAgeRclone) BackupConfig {
	c := BackupConfig{RetentionDays: defaultRetentionDays, Schedule: defaultBackupSchedule, Profile: pg.Default}
	if cur != nil {
		c = *cur
	}
	if req.Destination != nil {
		c.Destination = *req.Destination
	}
	if req.RcloneConfig != nil {
		c.RcloneConfig = *req.RcloneConfig
	}
	if len(req.Recipients) > 0 {
		c.Recipients = req.Recipients
	}
	if req.RetentionDays != nil {
		c.RetentionDays = *req.RetentionDays
	}
	if req.Schedule != nil {
		c.Schedule = *req.Schedule
	}
	if req.Profile != nil {
		c.Profile = *req.Profile
	}
	return c
}

// backupChanges names the settings c changes of cur (nil: none yet), the secrets by name only;
// empty when none.
func backupChanges(cur *BackupConfig, c BackupConfig) string {
	var was BackupConfig
	if cur != nil {
		was = *cur
	}
	var out []string
	for _, f := range []struct {
		name    string
		changed bool
	}{
		{"destination", was.Destination != c.Destination}, {"rclone_config", was.RcloneConfig != c.RcloneConfig},
		{"recipients", !sameRecipients(was.Recipients, c.Recipients)}, {"retention_days", was.RetentionDays != c.RetentionDays},
		{"schedule", was.Schedule != c.Schedule}, {"profile", was.Profile != c.Profile},
	} {
		if f.changed {
			out = append(out, f.name)
		}
	}
	return strings.Join(out, ", ")
}

func (h *Hostd) removeBackupConfig(w http.ResponseWriter, r *http.Request) {
	pg, err := h.pgBackup()
	if err != nil {
		replyErr(w, err)
		return
	}
	h.backupMu.Lock()
	defer h.backupMu.Unlock()
	if err := os.Remove(BackupFile(h.O.Dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		replyErr(w, err)
		return
	}
	if err := h.applySchedule(r.Context(), nil); err != nil {
		replyErr(w, err)
		return
	}
	reply(w, http.StatusOK, backupSettings(nil, pg, h.settingsLocked()))
}

// checkSchedule asks systemd whether s is a calendar event it reads.
func (h *Hostd) checkSchedule(ctx context.Context, s string) error {
	if h.Docker && !h.systemdHost() {
		return nil // no timer takes it: kept, unchecked
	}
	if _, err := h.Host.Run(ctx, Cmd{Args: []string{"systemd-analyze", "calendar", "--iterations", "1", "--", s}}); err != nil {
		return fail(http.StatusBadRequest, "schedule %q is not a systemd calendar event (e.g. %q): %v", s, defaultBackupSchedule, err)
	}
	return nil
}

// backupTimer is the timer that runs the scheduled backup.
func (h *Hostd) backupTimer() string {
	if h.Docker {
		return installUnit(h.R.Spec.Name, "backup", h.O.Dir) + ".timer"
	}
	return h.R.Spec.Name + "-backup.timer"
}

// applySchedule runs the timer on c's schedule, or stops it (nil). The timer is never enabled:
// the daemon starts it, at each start and change, once the schedule's runtime drop-in is
// written (the bundle's unit has the default), so no boot runs it on a schedule not the
// operator's.
func (h *Hostd) applySchedule(ctx context.Context, c *BackupConfig) error {
	if h.Docker {
		return h.dockerSchedule(ctx, c)
	}
	// A host without the timer (the daemon off the VM) has none to run.
	if st, err := unitState(ctx, h.Host, h.backupTimer()); err != nil {
		return err
	} else if st == noBootUnit {
		return nil
	}
	dir := filepath.Join(cmp.Or(h.Systemd, "/run/systemd/system"), h.backupTimer()+".d")
	file := filepath.Join(dir, "schedule.conf")
	if c == nil || c.Schedule == defaultBackupSchedule {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(file, []byte("[Timer]\nOnCalendar=\nOnCalendar="+c.Schedule+"\n"), 0o644); err != nil {
			return err
		}
	}
	if _, err := h.Host.Run(ctx, Cmd{Args: []string{"systemctl", "daemon-reload"}}); err != nil {
		return err
	}
	verb := "restart"
	if c == nil {
		verb = "stop"
	}
	_, err := h.Host.Run(ctx, Cmd{Args: []string{"systemctl", verb, h.backupTimer()}})
	return err
}

// backupResume starts the timer on the settings' schedule as the daemon starts; without
// settings, the timer is left stopped.
func (h *Hostd) backupResume() {
	if _, err := h.pgBackup(); err != nil {
		return
	}
	h.backupMu.Lock()
	defer h.backupMu.Unlock()
	// Settings this release no longer reads still start the timer: its runs then fail, and say
	// why, rather than stop unseen.
	c, err := h.decodeBackup()
	if err != nil {
		h.O.logf("backup settings: %v", err)
		c = &BackupConfig{Schedule: defaultBackupSchedule}
	}
	if c != nil && c.Schedule != defaultBackupSchedule {
		err := validSchedule(c.Schedule)
		if err == nil {
			err = h.checkSchedule(h.ctx, c.Schedule)
		}
		if err != nil {
			h.O.logf("backup schedule: %v: the timer runs on the default, %s", err, defaultBackupSchedule)
			c = &BackupConfig{Schedule: defaultBackupSchedule}
		}
	}
	if c != nil {
		if err := h.applySchedule(h.ctx, c); err != nil {
			h.O.logf("backup schedule: %v", err)
		}
	}
}

// backupObject names a backup: the project, the profile, when (UTC) and the release it was taken
// from, which a restore needs.
func backupObject(name, profile string, at time.Time, v Version) string {
	return fmt.Sprintf("%s-%s-%s-%s-r%d.pgdump.age", name, profile, at.UTC().Format("20060102T150405Z"), v.Version, v.Revision)
}

// ownBackups is the rclone filter of a profile's backups: the project's, by their name's
// timestamp (a profile is letters and digits, so another project's name never matches).
// anyProfile is every profile's, those a later release dropped included.
const anyProfile = `{{[a-z0-9]+}}` // no "}}" inside: rclone ends a regexp at the first

func ownBackups(name, profile string) string {
	return name + "-" + profile + `-{{\d{8}T\d{6}Z}}-*.pgdump.age`
}

// composeKV is a compose mapping as key=value entries: a mapping's sorted by key, a list's as
// listed.
func composeKV(v any) []string {
	var out []string
	switch v := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(v) {
			out = append(out, k+"="+fmt.Sprint(cmp.Or(v[k], any(""))))
		}
	case []any:
		for _, e := range v {
			out = append(out, fmt.Sprint(e))
		}
	}
	return out
}

// ownArchives is the rclone filter of a profile's backups' volume archives.
func ownArchives(name, profile string) string {
	return name + "-" + profile + `-{{\d{8}T\d{6}Z}}-*.tar.age`
}

// volumeObject names the archive of the volume vol beside the backup obj: obj's name, the volume
// in place of the dump.
func volumeObject(obj, vol string) string {
	return strings.TrimSuffix(obj, ".pgdump.age") + "." + vol + ".tar.age"
}

// stackVolume is the Docker volume of the stack's compose volume key: the one Compose labelled as
// the project's, else the one named as Compose names it, when it exists and is the stack's. When
// there is none it is "", unless create: then it is made as the compose file declares it.
func (in *Installer) stackVolume(ctx context.Context, env *EnvFile, key string, create bool) (string, error) {
	project, _ := env.Get("COMPOSE_PROJECT_NAME")
	if project == "" {
		return "", fmt.Errorf("%s records no COMPOSE_PROJECT_NAME: its volumes cannot be found", in.path(envName))
	}
	labels := []string{"com.docker.compose.project=" + project, "com.docker.compose.volume=" + key}
	out, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"volume", "ls", "--filter", "label=" + labels[0], "--filter", "label=" + labels[1], "--format", "{{.Name}}"}})
	if err != nil {
		return "", fmt.Errorf("listing the volumes: %w", err)
	}
	switch names := strings.Fields(string(out)); len(names) {
	case 0:
	case 1:
		return names[0], nil
	default:
		return "", fmt.Errorf("the volume %s is %d Docker volumes (%s): remove those the stack no longer mounts", key, len(names), strings.Join(names, ", "))
	}
	var doc struct {
		Volumes map[string]yaml.Node `yaml:"volumes"`
	}
	if err := yaml.Unmarshal(in.R.Compose, &doc); err != nil {
		return "", fmt.Errorf("the release's compose file: %w", err)
	}
	// Only this volume's declaration is read; its labels and driver_opts a mapping or a list of
	// key=value, as Compose takes them.
	var decl struct {
		Name       string `yaml:"name"`
		Driver     string `yaml:"driver"`
		DriverOpts any    `yaml:"driver_opts"`
		Labels     any    `yaml:"labels"`
	}
	if n, ok := doc.Volumes[key]; ok {
		if err := n.Decode(&decl); err != nil {
			return "", fmt.Errorf("the release's compose file: volumes.%s: %w", key, err)
		}
	}
	vol := cmp.Or(decl.Name, project+"_"+key)
	// A volume found by its name alone is the stack's when it carries no project's labels (one
	// Compose adopted) or this project's.
	owner, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"volume", "inspect", "--format", `{{index .Labels "com.docker.compose.project"}}`, vol}})
	switch {
	case err == nil:
		if o := strings.TrimSpace(string(owner)); o != "" && o != "<no value>" && o != project {
			return "", fmt.Errorf("volume %s belongs to the Compose project %s, not %s", vol, o, project)
		}
		return vol, nil
	case !strings.Contains(strings.ToLower(err.Error()), "no such volume"):
		return "", fmt.Errorf("volume %s: %w", vol, err)
	case !create:
		return "", nil
	}
	// Made as Compose would make it: its driver, options and labels, and Compose's own labels.
	args := []string{"volume", "create", "--label", labels[0], "--label", labels[1]}
	if decl.Driver != "" {
		args = append(args, "--driver", decl.Driver)
	}
	for _, kv := range composeKV(decl.DriverOpts) {
		args = append(args, "--opt", kv)
	}
	for _, kv := range composeKV(decl.Labels) {
		args = append(args, "--label", kv)
	}
	for _, a := range args {
		if strings.Contains(a, "$") {
			return "", fmt.Errorf("volume %s: its declaration holds %q, which Compose interpolates: up the stack once, then restore again", vol, a)
		}
	}
	if _, err := in.O.Docker.Run(ctx, Cmd{Args: append(args, vol)}); err != nil {
		return "", fmt.Errorf("creating the volume %s: %w", vol, err)
	}
	return vol, nil
}

// within joins a destination and a name in it.
func within(dest, name string) string {
	if strings.HasSuffix(dest, ":") || strings.HasSuffix(dest, "/") {
		return dest + name
	}
	return dest + "/" + name
}

func (h *Hostd) startBackup(w http.ResponseWriter, r *http.Request) {
	var req BackupRequest
	if err := decodeBody(r, &req); err != nil {
		replyErr(w, err)
		return
	}
	// The timer's run waits for a job running (a backup verify, a restore…) to end rather than
	// lose the day's backup; asked by hand, a busy daemon refuses at once. Everything the backup
	// reads is read again once the slot is free: a job that ran meanwhile (a secrets rotate) or a
	// change of the backup's settings is what the backup sees.
	wait := time.Duration(0)
	if req.IfConfigured {
		wait = scheduledBackupWait * cmp.Or(h.Unit, time.Second)
	}
	var p *backupPlan
	j, err := h.startJobWhenFree(r.Context(), wait, "backup", func() (string, error) {
		var err error
		p, err = h.planBackup(r.Context(), req)
		if err != nil {
			return "", err
		}
		return p.obj, nil
	})
	// The timer's run (if_configured) skips a release without a backup, an install not made yet
	// and a backup not configured rather than fail; every other refusal fails it.
	if s := (backupSkipped{}); req.IfConfigured && errors.As(err, &s) {
		reply(w, http.StatusOK, map[string]string{"skipped": s.reason})
		return
	}
	if err != nil {
		replyErr(w, err)
		return
	}
	h.backups.Add(1)
	go func() {
		defer h.backups.Done()
		run := h.takeBackup(p.in, p.env, *p.c, p.pg, p.profile, p.obj, func(format string, a ...any) { h.logf(j, format, a...) })
		var err error
		if run.Error != "" {
			err = errors.New(run.Error)
		}
		h.finish(j, err)
	}()
	accepted(w, j)
}

// backupPlan is what a backup asked for runs with, read as its job starts.
type backupPlan struct {
	in      *Installer
	env     *EnvFile
	c       *BackupConfig
	pg      *providers.PgAgeRclone
	profile string
	obj     string
}

// backupSkipped is a refusal the timer's run skips rather than fails, reason being what it
// answers.
type backupSkipped struct {
	err    error
	reason string
}

func (e backupSkipped) Error() string { return e.err.Error() }
func (e backupSkipped) Unwrap() error { return e.err }

// skippable is err, which the timer's run skips, answering why.
func skippable(err error, reason string) error {
	return backupSkipped{err: err, reason: cmp.Or(reason, err.Error())}
}

// planBackup reads what a backup asked for by req runs with — the release's backup, the install,
// its settings, the profile — and refuses as the backup would: a refusal the timer's run skips is
// a backupSkipped.
func (h *Hostd) planBackup(ctx context.Context, req BackupRequest) (*backupPlan, error) {
	pg, err := h.pgBackup()
	if err != nil {
		return nil, skippable(err, "")
	}
	in, _ := h.run()
	if notInstalled(in) {
		return nil, skippable(fail(http.StatusConflict, "%s is not installed yet", h.R.Spec.Name), "")
	}
	env, err := h.installed(in)
	if err != nil {
		return nil, err
	}
	// A backup of volumes half restored would pass for a whole one.
	if err := in.volumesSplit(); err != nil {
		return nil, fail(http.StatusConflict, "%v", err)
	}
	h.backupMu.Lock()
	c, err := h.readBackup(pg)
	h.backupMu.Unlock()
	switch {
	case err != nil:
		return nil, err
	case c == nil:
		return nil, skippable(fail(http.StatusConflict, "no backup is configured: `%s backup config` sets the destination", h.R.Ctl()), "no backup is configured")
	}
	profile := cmp.Or(req.Profile, c.Profile)
	if _, ok := pg.Profiles[profile]; !ok {
		return nil, fail(http.StatusBadRequest, "profile %q is not one of the release's: %s", profile, strings.Join(sortedKeys(pg.Profiles), ", "))
	}
	if err := h.booting(ctx); err != nil {
		return nil, err
	}
	m := h.R.Manifest
	obj := backupObject(h.R.Spec.Name, profile, time.Now(), Version{m.ProjectVersion, m.Revision})
	return &backupPlan{in: in, env: env, c: c, pg: pg, profile: profile, obj: obj}, nil
}

// preflightBackup takes a backup before an upgrade, when one is configured: one that fails is
// logged, and the upgrade goes on (the boot still dumps the database before it migrates).
func (h *Hostd) preflightBackup(in *Installer, logf func(string, ...any)) {
	pg, err := h.pgBackup()
	if err != nil {
		return
	}
	h.backupMu.Lock()
	c, err := h.readBackup(pg)
	h.backupMu.Unlock()
	switch {
	case err != nil:
		logf("no backup first: %v", err)
		return
	case c == nil:
		logf("no backup first: none is configured")
		return
	}
	env, err := in.open()
	if err != nil {
		logf("no backup first: %v", err)
		return
	}
	m := h.R.Manifest
	obj := backupObject(h.R.Spec.Name, c.Profile, time.Now(), Version{m.ProjectVersion, m.Revision})
	logf("a backup first: %s", obj)
	h.backingUp.Store(true)
	defer h.backingUp.Store(false)
	h.backups.Add(1)
	defer h.backups.Done()
	if run := h.takeBackup(in, env, *c, pg, c.Profile, obj, logf); run.Error != "" {
		logf("the backup failed, the upgrade goes on: %s", run.Error)
	}
}

// takeBackup runs one backup and records it as the last.
func (h *Hostd) takeBackup(in *Installer, env *EnvFile, c BackupConfig, pg *providers.PgAgeRclone, profile, obj string, logf func(string, ...any)) BackupRun {
	m := h.R.Manifest
	run := BackupRun{Object: obj, Profile: profile, Release: Version{m.ProjectVersion, m.Revision}, Started: time.Now().UTC()}
	ctx, cancel := context.WithTimeout(h.ctx, backupTimeout)
	defer cancel()
	n, prune, err := h.dumpUpload(ctx, in, env, c, pg, profile, obj, logf)
	run.Finished, run.Bytes = time.Now().UTC(), n
	if err != nil {
		run.Error = err.Error()
	} else {
		logf("backed up: %s (%d bytes)", within(c.Destination, obj), n)
		if prune != nil {
			run.PruneError = prune.Error()
			logf("the older backups were not pruned: %v", prune)
		}
	}
	if raw, err := json.Marshal(run); err == nil {
		if err := writePrivate(BackupLastFile(h.O.Dir), raw); err != nil {
			logf("the backup's record: %v", err)
		}
	}
	return run
}

// dumpUpload streams each volume's encrypted archive, then the encrypted dump, to the destination,
// checking the size each reached, then prunes the profile's backups past the retention; it
// returns the bytes uploaded and the pruning's error apart. The dump is the backup's last object:
// one that failed leaves none of them.
func (h *Hostd) dumpUpload(ctx context.Context, in *Installer, env *EnvFile, c BackupConfig, pg *providers.PgAgeRclone, profile, obj string, logf func(string, ...any)) (int64, error, error) {
	recips, err := validBackup(c, pg)
	if err != nil {
		return 0, nil, err
	}
	// A backup of volumes half restored would pass for a whole one (an upgrade's first too).
	if err := in.volumesSplit(); err != nil {
		return 0, nil, err
	}
	conf, done, err := h.rcloneConf(c)
	if err != nil {
		return 0, nil, err
	}
	defer done()
	d := h.R.Spec.DB()
	dump := []string{"exec", "-T", d.Service, "pg_dump", "-U", d.Roles.Migrate, "-d", d.Name, "--format=custom", "--compress=9"}
	if tables := pg.Profiles[profile]; tables != nil {
		ex, missing, err := excludedData(ctx, in, env, tables)
		if err != nil {
			return 0, nil, err
		}
		for _, t := range missing {
			logf("profile %s lists the table %s, which the database lacks", profile, t)
		}
		for _, t := range ex {
			dump = append(dump, "--exclude-table-data="+t)
		}
	}
	// The profile's own patterns: pg_dump matches them, leaving each table's structure in.
	for _, t := range pg.Exclude[profile] {
		dump = append(dump, "--exclude-table-data="+t)
	}
	var sent []string
	discard := func() {
		for _, dest := range sent {
			h.discard(ctx, in, conf, dest, logf)
		}
	}
	var total int64
	var image string
	if len(pg.Volumes) > 0 {
		if image, err = h.runnerImage(ctx, in); err != nil {
			return 0, nil, err
		}
	}
	for _, v := range pg.Volumes {
		vol, err := in.stackVolume(ctx, env, v, false)
		if err != nil {
			discard()
			return 0, nil, err
		}
		dest := within(c.Destination, volumeObject(obj, v))
		logf("archiving the volume %s, encrypted to %d recipient(s), to %s", v, len(recips), dest)
		n, err := h.encryptUpload(ctx, in, conf, recips, dest, func(w io.Writer) error {
			if vol == "" {
				// Never created (no service of it ran): an archive of nothing.
				return tar.NewWriter(w).Close()
			}
			_, err := h.runnerShell(ctx, in, image, vol, true, nil, w, archiveScript)
			if err != nil {
				err = fmt.Errorf("the archive of the volume %s: %w", v, err)
			}
			return err
		}, logf)
		if err != nil {
			discard()
			return 0, nil, err
		}
		sent, total = append(sent, dest), total+n
	}
	dest := within(c.Destination, obj)
	logf("dumping %s's database (profile %s), encrypted to %d recipient(s), to %s", h.R.Spec.Name, profile, len(recips), dest)
	n, err := h.encryptUpload(ctx, in, conf, recips, dest, func(w io.Writer) error {
		_, err := in.O.Docker.Run(ctx, Cmd{Args: append(in.composeBase(), dump...), Env: env.Environ(in.O.environ()), Stdout: w})
		if err != nil {
			err = fmt.Errorf("the dump: %w", err)
		}
		return err
	}, logf)
	if err != nil {
		discard()
		return 0, nil, err
	}
	total += n
	var prune error
	if c.RetentionDays != 0 {
		_, prune = h.rclone(ctx, in, conf, nil, "delete", "--min-age", fmt.Sprintf("%dd", c.RetentionDays), "--max-depth", "1", "--include", ownBackups(h.R.Spec.Name, profile), c.Destination)
	}
	// Orphan archives go whatever the retention and the volumes now listed: a failed backup whose
	// cleanup failed left them, or retention took a dump an earlier release archived volumes with.
	if prune == nil {
		prune = h.pruneArchives(ctx, in, conf, c.Destination, profile)
	}
	return total, prune, nil
}

// pruneArchives deletes the profile's volume archives whose dump is gone: retention goes by the
// dump's age, as an archive, uploaded before its dump, is older than it. An archive younger than
// a backup's limit may be a backup's still running (another install's, in a shared
// destination): it stays.
func (h *Hostd) pruneArchives(ctx context.Context, in *Installer, conf, dest, profile string) error {
	name := h.R.Spec.Name
	out, err := h.rclone(ctx, in, conf, nil, "lsjson", "--files-only", "--no-mimetype", "--max-depth", "1", "--include", ownBackups(name, profile), "--include", ownArchives(name, profile), dest)
	if err != nil {
		return err
	}
	var found []struct {
		Name    string
		ModTime time.Time
	}
	if err := json.Unmarshal(out, &found); err != nil {
		return fmt.Errorf("the destination's list: %w", err)
	}
	var dumps []string
	for _, f := range found {
		if base, ok := strings.CutSuffix(f.Name, ".pgdump.age"); ok {
			dumps = append(dumps, base+".")
		}
	}
	var errs []error
	for _, f := range found {
		if !strings.HasSuffix(f.Name, ".tar.age") || time.Since(f.ModTime) < 2*backupTimeout ||
			slices.ContainsFunc(dumps, func(d string) bool { return strings.HasPrefix(f.Name, d) }) {
			continue
		}
		if _, err := h.rclone(ctx, in, conf, nil, "deletefile", within(dest, f.Name)); err != nil && !rcloneNotFound(err) {
			errs = append(errs, fmt.Errorf("%s: %w", f.Name, err))
		}
	}
	return errors.Join(errs...)
}

// encryptUpload streams what produce writes, encrypted to recips, to dest, and checks the size it
// reached; one that fails leaves nothing under its name.
func (h *Hostd) encryptUpload(ctx context.Context, in *Installer, conf string, recips []age.Recipient, dest string, produce func(io.Writer) error, logf func(string, ...any)) (int64, error) {
	pr, pw := io.Pipe()
	// The producer's own error does not say whether the upload stopped first (the CLI reports
	// its exit, not the broken pipe): its writes do.
	watched := &pipeWatch{w: pw}
	produced := make(chan error, 1)
	go func() {
		enc, err := age.Encrypt(watched, recips...)
		if err == nil {
			if err = produce(enc); err == nil {
				err = enc.Close()
			}
		}
		pw.CloseWithError(err)
		produced <- err
	}()
	sent := &counter{r: pr}
	_, upErr := h.rcloneNamed(ctx, in, conf, h.runnerName(), sent, "rcat", dest)
	// An upload that stopped early leaves the producer nothing to write to.
	pr.CloseWithError(errUploadEnded)
	prodErr := <-produced
	ctxErr := ctx.Err() // before the cleanup, which may outlast the deadline
	// A failed upload leaves nothing under its name: rclone may have taken the stream's end for
	// the file's, or a backend kept what it was sent.
	if prodErr != nil || upErr != nil {
		h.discard(ctx, in, conf, dest, logf)
		switch {
		case errors.Is(ctxErr, context.DeadlineExceeded):
			return 0, fmt.Errorf("the backup did not finish within %s", backupTimeout)
		case ctxErr != nil:
			return 0, errors.New("the backup was stopped: the host daemon is stopping")
		case prodErr != nil && !watched.ended.Load():
			// The producer failed on its own: the upload's error is its echo.
			return 0, prodErr
		}
		// The upload ended first (rclone may exit 0 before reading to the end).
		return 0, fmt.Errorf("the upload: %w", cmp.Or(upErr, errUploadEnded))
	}
	n := sent.n.Load()
	out, err := h.rclone(ctx, in, conf, nil, "lsjson", "--files-only", "--no-mimetype", dest)
	if err != nil {
		h.discard(ctx, in, conf, dest, logf)
		return n, fmt.Errorf("the uploaded backup could not be checked: %w", err)
	}
	var found []struct{ Size int64 }
	if err := json.Unmarshal(out, &found); err != nil || len(found) != 1 || found[0].Size != n {
		h.discard(ctx, in, conf, dest, logf)
		return n, fmt.Errorf("the destination holds %s, not the %d bytes sent", strings.TrimSpace(string(out)), n)
	}
	return n, nil
}

// discard deletes dest, an object a failed backup sent. The run's context may be over (the
// backup's timeout, the daemon stopping): the cleanup has its own.
func (h *Hostd) discard(ctx context.Context, in *Installer, conf, dest string, logf func(string, ...any)) {
	clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), rcloneTimeout)
	defer cancel()
	if _, err := h.rclone(clean, in, conf, nil, "deletefile", dest); err != nil && !rcloneNotFound(err) {
		logf("%s may be left incomplete: deleting it failed (%v)", dest, err)
	}
}

// errUploadEnded is what the dump meets when the upload stopped first.
var errUploadEnded = errors.New("the upload ended")

// pipeWatch notes a write refused because the upload ended.
type pipeWatch struct {
	w     io.Writer
	ended atomic.Bool
}

func (p *pipeWatch) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if errors.Is(err, errUploadEnded) {
		p.ended.Store(true)
	}
	return n, err
}

// rcloneNotFound reports an rclone exit for a file or directory not found (3, 4): nothing to
// delete.
func rcloneNotFound(err error) bool {
	var ec interface{ ExitCode() int }
	return errors.As(err, &ec) && (ec.ExitCode() == 3 || ec.ExitCode() == 4)
}

// counter counts what is read through it.
type counter struct {
	r io.Reader
	n atomic.Int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// excludedData lists the tables, quoted as pg_dump patterns, whose data a profile listing tables
// leaves out: every one it does not name ("table" in any schema, or "schema.table"). It also
// returns the names that match no table.
func excludedData(ctx context.Context, in *Installer, env *EnvFile, tables []string) ([]string, []string, error) {
	d := in.R.Spec.DB()
	q := "SELECT schemaname, tablename FROM pg_tables WHERE schemaname NOT IN ('pg_catalog', 'information_schema') ORDER BY 1, 2"
	out, err := in.O.Docker.Run(ctx, Cmd{Args: append(in.composeBase(), "exec", "-T", d.Service, "psql", "-U", d.Roles.Migrate, "-d", d.Name, "-AtX", "-F", "\x1f", "-R", "\x1e", "-c", q), Env: env.Environ(in.O.environ())})
	if err != nil {
		return nil, nil, fmt.Errorf("the database's tables: %w", err)
	}
	used := map[string]bool{}
	var ex []string
	for _, row := range strings.Split(strings.TrimRight(string(out), "\n"), "\x1e") {
		schema, table, ok := strings.Cut(row, "\x1f")
		if !ok {
			continue
		}
		kept := false
		for _, t := range tables {
			if t == table || t == schema+"."+table {
				used[t], kept = true, true
			}
		}
		if !kept {
			ex = append(ex, pgQuote(schema)+"."+pgQuote(table))
		}
	}
	var missing []string
	for _, t := range tables {
		if !used[t] {
			missing = append(missing, t)
		}
	}
	return ex, missing, nil
}

// pgQuote quotes a name for a pg_dump pattern: taken literally, case kept.
func pgQuote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// rcloneConf writes c's rclone config where the runner mounts it, root's alone; done removes it.
func (h *Hostd) rcloneConf(c BackupConfig) (string, func(), error) {
	dir, err := os.MkdirTemp(cmp.Or(h.Run, os.TempDir()), "backup-")
	if err != nil {
		return "", nil, err
	}
	done := func() { os.RemoveAll(dir) }
	if err := os.WriteFile(filepath.Join(dir, "rclone.conf"), []byte(c.RcloneConfig), 0o600); err != nil {
		done()
		return "", nil, err
	}
	return dir, done, nil
}

// rclone runs the backup runner with the config in dir, stdin given when not nil.
func (h *Hostd) rclone(ctx context.Context, in *Installer, dir string, stdin io.Reader, args ...string) ([]byte, error) {
	return h.rcloneNamed(ctx, in, dir, h.runnerName(), stdin, args...)
}

// runnerName names a runner's container: one of its own, removed by name when its run is cut off.
func (h *Hostd) runnerName() string {
	return h.R.Spec.Name + "-backup-" + strings.ToLower(rand.Text()[:12])
}

// rcloneNamed runs the runner as the container name.
func (h *Hostd) rcloneNamed(ctx context.Context, in *Installer, dir, name string, stdin io.Reader, args ...string) ([]byte, error) {
	return h.rcloneTo(ctx, in, dir, name, stdin, nil, args...)
}

// rcloneTo runs the runner as the container name, its output streamed to stdout when not nil.
func (h *Hostd) rcloneTo(ctx context.Context, in *Installer, dir, name string, stdin io.Reader, stdout io.Writer, args ...string) ([]byte, error) {
	// On the host's network: the destination is reached, and named, as the host reaches it, over
	// IPv6 too.
	a := []string{"run", "--rm", "--pull", "never", "--network", "host", "--name", name, "-v", dir + ":/config/rclone"}
	if stdin != nil {
		a = append(a, "-i")
	}
	image, err := h.runnerImage(ctx, in)
	if err != nil {
		return nil, err
	}
	a = append(append(a, image, "--config", "/config/rclone/rclone.conf"), args...)
	return h.runRunner(ctx, in, name, Cmd{Args: a, Stdin: stdin, Stdout: stdout})
}

// runnerImage is the backup runner's image reference. A Docker install pulls it again when the
// engine lost it (an image prune): with its images, as the install pulls them.
func (h *Hostd) runnerImage(ctx context.Context, in *Installer) (string, error) {
	env, err := in.open()
	if err != nil {
		return in.imageRef("", backup.Key), nil
	}
	prefix, _ := env.Get("STOWAGE_REGISTRY")
	ref := in.imageRef(prefix, backup.Key)
	if h.Docker {
		if _, ierr := in.O.Docker.Run(ctx, Cmd{Args: []string{"image", "inspect", "--format", "{{.Id}}", ref}}); ierr != nil {
			if err := in.fetchImages(ctx, env); err != nil {
				return "", fmt.Errorf("the backup runner: %w", err)
			}
		}
	}
	return ref, nil
}

// runRunner runs c, a runner container named name; a stopped docker CLI leaves its container
// running, which is then removed.
func (h *Hostd) runRunner(ctx context.Context, in *Installer, name string, c Cmd) ([]byte, error) {
	out, err := in.O.Docker.Run(ctx, c)
	if err != nil && ctx.Err() != nil {
		clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), rcloneTimeout)
		defer cancel()
		_, _ = in.O.Docker.Run(clean, Cmd{Args: []string{"rm", "--force", name}})
	}
	return out, err
}

// runnerShell runs script in the runner's shell (image, the reference runnerImage resolved), off any network, with the Docker volume vol
// mounted at /v (read-only when ro), stdin given when not nil and its output streamed to stdout.
func (h *Hostd) runnerShell(ctx context.Context, in *Installer, image, vol string, ro bool, stdin io.Reader, stdout io.Writer, script string) ([]byte, error) {
	mount := vol + ":/v"
	if ro {
		mount += ":ro"
	}
	name := h.runnerName()
	a := []string{"run", "--rm", "--pull", "never", "--network", "none", "--name", name, "-v", mount}
	if stdin != nil {
		a = append(a, "-i")
	}
	a = append(a, "--entrypoint", "sh", image, "-euc", script)
	return h.runRunner(ctx, in, name, Cmd{Args: a, Stdin: stdin, Stdout: stdout})
}

// runnerCall is a short call of the runner with the settings: list and verify.
func (h *Hostd) runnerCall(r *http.Request) (*Installer, *BackupConfig, error) {
	pg, err := h.pgBackup()
	if err != nil {
		return nil, nil, err
	}
	in, _ := h.run()
	if _, err := h.installed(in); err != nil {
		return nil, nil, err
	}
	h.backupMu.Lock()
	c, err := h.readBackup(pg)
	h.backupMu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	if c == nil {
		return nil, nil, fail(http.StatusConflict, "no backup is configured: `%s backup config` sets the destination", h.R.Ctl())
	}
	return in, c, nil
}

// proposedBackup is verify's run with settings proposed over the saved ones (or none), checked
// as a change would be, never saved.
func (h *Hostd) proposedBackup(req BackupConfigRequest) (*Installer, *BackupConfig, error) {
	pg, err := h.pgBackup()
	if err != nil {
		return nil, nil, err
	}
	in, _ := h.run()
	if _, err := h.installed(in); err != nil {
		return nil, nil, err
	}
	h.backupMu.Lock()
	cur, err := h.decodeBackup()
	h.backupMu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	c := mergeBackup(cur, req, pg)
	if _, err := validBackup(c, pg); err != nil {
		return nil, nil, fail(http.StatusBadRequest, "%v", err)
	}
	return in, &c, nil
}

func (h *Hostd) listBackups(w http.ResponseWriter, r *http.Request) {
	in, c, err := h.runnerCall(r)
	if err != nil {
		replyErr(w, err)
		return
	}
	conf, done, err := h.rcloneConf(*c)
	if err != nil {
		replyErr(w, err)
		return
	}
	defer done()
	ctx, cancel := context.WithTimeout(r.Context(), rcloneTimeout)
	defer cancel()
	out, err := h.rclone(ctx, in, conf, nil, "lsjson", "--files-only", "--no-mimetype", "--max-depth", "1", "--include", ownBackups(h.R.Spec.Name, anyProfile), c.Destination)
	if err != nil {
		replyErr(w, fail(http.StatusBadGateway, "the destination: %v", err))
		return
	}
	var found []struct {
		Name    string
		Size    int64
		ModTime time.Time
	}
	if err := json.Unmarshal(out, &found); err != nil {
		replyErr(w, fail(http.StatusBadGateway, "the destination's list: %v", err))
		return
	}
	list := []BackupObject{}
	for _, f := range found {
		list = append(list, BackupObject{f.Name, f.Size, f.ModTime})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	reply(w, http.StatusOK, list)
}

// verifyProbe is the file `backup verify` writes, again each time.
func verifyProbe(name string) string { return name + "-verify.txt" }

// verifyBackup checks the settings — the saved ones, or those proposed over them — synchronously,
// then writes and reads back the probe as a backup-verify job: done, the destination is reachable
// and writable (the job's result says so); failed, its error says why.
func (h *Hostd) verifyBackup(w http.ResponseWriter, r *http.Request) {
	var req BackupConfigRequest
	if err := decodeBody(r, &req); err != nil {
		replyErr(w, err)
		return
	}
	if req.Lock != nil {
		replyErr(w, fail(http.StatusBadRequest, "lock is a setting to save (POST /v1/backup/config), not one verify tries"))
		return
	}
	proposed := req.setsAny()
	var in *Installer
	var c *BackupConfig
	var err error
	if proposed {
		in, c, err = h.proposedBackup(req)
	} else {
		in, c, err = h.runnerCall(r)
	}
	if err != nil {
		replyErr(w, err)
		return
	}
	j, err := h.startJob("backup-verify", c.Destination, nil)
	if err != nil {
		replyErr(w, err)
		return
	}
	h.backups.Add(1)
	go func() {
		defer h.backups.Done()
		msg, err := h.probeDestination(in, *c, func(format string, a ...any) { h.logf(j, format, a...) })
		if err == nil {
			if proposed {
				msg += " (the settings proposed, not saved)"
			}
			h.logf(j, "%s", msg)
			h.mu.Lock()
			j.Result = msg
			h.mu.Unlock()
		}
		h.finish(j, err)
	}()
	accepted(w, j)
}

// probeDestination writes the probe to c's destination and reads it back, each within
// rcloneTimeout: what it found, or why the destination fails.
func (h *Hostd) probeDestination(in *Installer, c BackupConfig, logf func(string, ...any)) (string, error) {
	conf, done, err := h.rcloneConf(c)
	if err != nil {
		return "", err
	}
	defer done()
	ctx, cancel := context.WithTimeout(h.ctx, 2*rcloneTimeout)
	defer cancel()
	probe := within(c.Destination, verifyProbe(h.R.Spec.Name))
	body := fmt.Sprintf("written by %s backup verify at %s\n", h.R.Ctl(), time.Now().UTC().Format(time.RFC3339))
	logf("writing %s", probe)
	wctx, wcancel := context.WithTimeout(ctx, rcloneTimeout)
	_, err = h.rclone(wctx, in, conf, strings.NewReader(body), "rcat", probe)
	wcancel()
	if err != nil {
		return "", fmt.Errorf("writing %s: %v", probe, err)
	}
	logf("reading %s back", probe)
	rctx, rcancel := context.WithTimeout(ctx, rcloneTimeout)
	out, err := h.rclone(rctx, in, conf, nil, "lsjson", "--files-only", "--no-mimetype", probe)
	rcancel()
	var found []struct{ Size int64 }
	if err != nil || json.Unmarshal(out, &found) != nil || len(found) != 1 || found[0].Size != int64(len(body)) {
		return "", fmt.Errorf("%s was written but does not read back: %v %s", probe, err, strings.TrimSpace(string(out)))
	}
	return fmt.Sprintf("%s is reachable and writable: %s was written", c.Destination, probe), nil
}

func (h *Hostd) backupStatus(w http.ResponseWriter, r *http.Request) {
	pg, err := h.pgBackup()
	if err != nil {
		replyErr(w, err)
		return
	}
	h.backupMu.Lock()
	c, err := h.readBackup(pg)
	h.backupMu.Unlock()
	if err != nil {
		replyErr(w, err)
		return
	}
	st := BackupStatus{Settings: backupSettings(c, pg, h.settingsLocked())}
	if out, err := h.Host.Run(r.Context(), Cmd{Args: []string{"systemctl", "show", "--property", "NextElapseUSecRealtime", "--value", h.backupTimer()}}); err == nil {
		if next := strings.TrimSpace(string(out)); next != "" && next != "n/a" {
			st.Next = next
		}
	}
	if raw, err := os.ReadFile(BackupLastFile(h.O.Dir)); err == nil {
		var last BackupRun
		if json.Unmarshal(raw, &last) == nil {
			st.Last = &last
		}
	}
	h.mu.Lock()
	st.Running = h.job != nil && h.job.State == "running" && (h.job.Kind == "backup" || h.backingUp.Load())
	h.mu.Unlock()
	// A Docker install's reads are answered beside the process running its jobs.
	if h.Docker && h.Run == "" && !st.Running {
		st.Running = hostBusy(h.O.Dir)
	}
	reply(w, http.StatusOK, st)
}
