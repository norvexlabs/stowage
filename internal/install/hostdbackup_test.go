package install

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/norvexlabs/stowage/internal/backup"
	"github.com/norvexlabs/stowage/internal/spec"
)

// fakeRclone is the backup runner over an in-memory destination.
type fakeRclone struct {
	mu      sync.Mutex
	objects map[string][]byte
	ran     [][]string // each call's rclone arguments
	conf    string     // the rclone config the last call mounted
	// rcatErr fails an upload, after reading what it was given; deleteErr fails a pruning; short
	// reports an object one byte shorter than it is.
	rcatErr, deleteErr error
	short              bool
	// early fails an upload before it reads anything (bad credentials); delErr fails a deletion.
	early, delErr error
	// quits ends an upload with success before it reads anything.
	quits bool
	// volumes are the Docker volumes' archives, as the runner's tar writes and reads them; staged
	// what a restore extracted beside a volume's data. tarErr fails an archive, stageErr an
	// extraction.
	volumes, staged  map[string][]byte
	tarErr, stageErr error
	swapErr          error // fails the swap of a volume named …swapFails
	swapFails        string
	dropped          []string // the volumes whose former data was deleted
	// modTimes are objects' times other than the fixed one lsjson gives.
	modTimes map[string]time.Time
}

// shell runs the runner's shell on a volume: its archive, a restore's staging and swap.
func (f *fakeRclone) shell(c Cmd) ([]byte, error) {
	if n := slices.Index(c.Args, "--network"); n < 0 || c.Args[n+1] != "none" {
		return nil, fmt.Errorf("the shell is on a network: %v", c.Args)
	}
	mount := c.Args[slices.Index(c.Args, "-v")+1]
	vol, rest, _ := strings.Cut(mount, ":")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.volumes == nil {
		f.volumes, f.staged = map[string][]byte{}, map[string][]byte{}
	}
	switch script := c.Args[len(c.Args)-1]; script {
	case archiveScript:
		if rest != "/v:ro" {
			return nil, fmt.Errorf("archived through %s", mount)
		}
		if _, err := c.Stdout.Write(f.volumes[vol]); err != nil {
			return nil, err
		}
		return nil, f.tarErr
	case stageScript:
		b, err := io.ReadAll(c.Stdin)
		if err != nil {
			return nil, err
		}
		if f.stageErr != nil {
			return nil, f.stageErr
		}
		f.staged[vol] = b
	case unstageScript:
		delete(f.staged, vol)
	case swapVolumeScript:
		if f.swapErr != nil && strings.HasSuffix(vol, f.swapFails) {
			return nil, f.swapErr
		}
		f.volumes[vol] = f.staged[vol]
		delete(f.staged, vol)
	case dropVolumeScript:
		f.dropped = append(f.dropped, vol)
	default:
		return nil, fmt.Errorf("script %q", script)
	}
	return nil, nil
}

// matches is rclone's filter of a name: {{regexp}}, * within a path element.
func matches(pattern, name string) bool {
	var re strings.Builder
	for pattern != "" {
		switch {
		case strings.HasPrefix(pattern, "{{"):
			end := strings.Index(pattern, "}}")
			re.WriteString(pattern[2:end])
			pattern = pattern[end+2:]
		case pattern[0] == '*':
			re.WriteString("[^/]*")
			pattern = pattern[1:]
		default:
			re.WriteString(regexp.QuoteMeta(pattern[:1]))
			pattern = pattern[1:]
		}
	}
	return regexp.MustCompile("^" + re.String() + "$").MatchString(name)
}

func (f *fakeRclone) run(c Cmd) ([]byte, error) {
	if slices.Contains(c.Args, "--entrypoint") {
		return f.shell(c)
	}
	// The runner reaches its destination as the host does, over IPv6 too.
	if n := slices.Index(c.Args, "--network"); n < 0 || c.Args[n+1] != "host" {
		return nil, fmt.Errorf("not on the host's network: %v", c.Args)
	}
	i := slices.Index(c.Args, "--config")
	if i < 0 || c.Args[i+1] != "/config/rclone/rclone.conf" {
		return nil, fmt.Errorf("no config: %v", c.Args)
	}
	var mount string
	for j, a := range c.Args {
		if a == "-v" {
			mount, _, _ = strings.Cut(c.Args[j+1], ":")
		}
	}
	conf, err := os.ReadFile(filepath.Join(mount, "rclone.conf"))
	if err != nil {
		return nil, err
	}
	args := c.Args[i+2:]
	var data []byte
	if args[0] == "rcat" {
		if f.early != nil {
			return nil, f.early
		}
		if f.quits {
			return nil, nil
		}
		// A stdin that ends in an error leaves what was read uploaded, as rclone would.
		data, err = io.ReadAll(c.Stdin)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conf = string(conf)
	f.ran = append(f.ran, args)
	last := args[len(args)-1]
	switch args[0] {
	case "rcat":
		if len(data) > 0 { // a backend may keep what it was sent
			f.objects[last] = data
		}
		if f.rcatErr != nil {
			return nil, f.rcatErr
		}
		if err != nil {
			return nil, err
		}
	case "deletefile":
		if f.delErr != nil {
			return nil, f.delErr
		}
		if _, ok := f.objects[last]; !ok {
			return nil, exitCode(4)
		}
		delete(f.objects, last)
	case "delete":
		return nil, f.deleteErr
	case "cat":
		data, ok := f.objects[last]
		if !ok {
			return nil, exitCode(3)
		}
		_, err := c.Stdout.Write(data)
		return nil, err
	case "lsjson":
		type entry struct {
			Name    string
			Size    int64
			ModTime time.Time
		}
		list := []entry{}
		for k, v := range f.objects {
			n := int64(len(v))
			if f.short {
				n--
			}
			listed := false
			for j, a := range args {
				if a == "--include" && strings.HasPrefix(k, last+"/") && matches(args[j+1], strings.TrimPrefix(k, last+"/")) {
					listed = true
				}
			}
			if k == last || listed {
				at, ok := f.modTimes[k]
				if !ok {
					at = time.Date(2026, 10, 10, 3, 0, 0, 0, time.UTC)
				}
				list = append(list, entry{filepath.Base(k), n, at})
			}
		}
		return json.Marshal(list)
	}
	return nil, nil
}

func (f *fakeRclone) calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.ran...)
}

const rcloneConf = "[store]\ntype = s3\nsecret_access_key = hush\n"

// backupVM is an installed VM whose daemon backs up to a fake destination; key decrypts its
// backups.
func backupVM(t *testing.T) (*env, *Hostd, *fakeRclone, *age.X25519Identity) {
	t.Helper()
	e := vmEnv(t, "1.0.0")
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	f := &fakeRclone{objects: map[string][]byte{}}
	e.docker.runner = f.run
	e.docker.dump = "the database's dump"
	key, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return e, daemon(e, &fakeHost{}), f, key
}

// stackVolumeOf is the Docker volume the install's compose project names key.
func stackVolumeOf(t *testing.T, d *Hostd, key string) string {
	t.Helper()
	env, err := ReadEnvFile(filepath.Join(d.O.Dir, envName))
	if err != nil {
		t.Fatal(err)
	}
	project, _ := env.Get("COMPOSE_PROJECT_NAME")
	return project + "_" + key
}

func configure(t *testing.T, h *Hostd, req BackupConfigRequest) BackupSettings {
	t.Helper()
	var s BackupSettings
	if code := call(t, h.Handler(), "POST", "/v1/backup/config", req, &s); code != 200 {
		var e errOf
		call(t, h.Handler(), "POST", "/v1/backup/config", req, &e)
		t.Fatalf("config: %d %s", code, e.Error)
	}
	return s
}

func ptr[T any](v T) *T { return &v }

func validConfig(key *age.X25519Identity) BackupConfigRequest {
	return BackupConfigRequest{Destination: ptr("store:backups/notes"), RcloneConfig: ptr(rcloneConf), Recipients: []string{key.Recipient().String()}}
}

func TestBackupConfigIsValidatedKeptAndMasked(t *testing.T) {
	t.Parallel()
	_, d, _, key := backupVM(t)
	h := d.Handler()
	var s BackupSettings
	if code := call(t, h, "GET", "/v1/backup/config", nil, &s); code != 200 || s.Configured || s.Schedule != defaultBackupSchedule || s.RetentionDays != 30 || s.Profile != "full" || fmt.Sprint(s.Profiles) != "[full]" {
		t.Fatalf("before: %d %+v", code, s)
	}
	other, _ := age.GenerateX25519Identity()
	ok := validConfig(key)
	for name, c := range map[string]struct {
		edit func(*BackupConfigRequest)
		want string
	}{
		"no destination":       {func(r *BackupConfigRequest) { r.Destination = nil }, "destination is required"},
		"a local path":         {func(r *BackupConfigRequest) { r.Destination = ptr("/srv/backups") }, "remote:path"},
		"an unknown remote":    {func(r *BackupConfigRequest) { r.Destination = ptr("elsewhere:b") }, `defines no remote "elsewhere"`},
		"a control character":  {func(r *BackupConfigRequest) { r.Destination = ptr("store:a\nb") }, "no control character"},
		"no rclone config":     {func(r *BackupConfigRequest) { r.RcloneConfig = nil }, "the rclone config is required"},
		"a huge rclone config": {func(r *BackupConfigRequest) { r.RcloneConfig = ptr(rcloneConf + strings.Repeat("#", maxRcloneConfig)) }, "at most"},
		"no recipient":         {func(r *BackupConfigRequest) { r.Recipients = nil }, "recipients: 1 to 16"},
		"too many recipients":  {func(r *BackupConfigRequest) { r.Recipients = slices.Repeat(r.Recipients, maxRecipients+1) }, "recipients: 1 to 16"},
		"a private key":        {func(r *BackupConfigRequest) { r.Recipients = []string{key.String()} }, "not an age public key"},
		"two keys in one": {func(r *BackupConfigRequest) {
			r.Recipients = []string{key.Recipient().String() + "\n" + other.Recipient().String()}
		}, "one age public key"},
		"a negative retention": {func(r *BackupConfigRequest) { r.RetentionDays = ptr(-1) }, "retention_days -1"},
		"a retention too long": {func(r *BackupConfigRequest) { r.RetentionDays = ptr(maxRetentionDays + 1) }, "retention_days 3651"},
		"a percent schedule":   {func(r *BackupConfigRequest) { r.Schedule = ptr("daily %H") }, "without %"},
		"an unknown profile":   {func(r *BackupConfigRequest) { r.Profile = ptr("nightly") }, `profile "nightly"`},
	} {
		req := ok
		req.Recipients = slices.Clone(ok.Recipients)
		c.edit(&req)
		var e errOf
		if code := call(t, h, "POST", "/v1/backup/config", req, &e); code != 400 || !strings.Contains(e.Error, c.want) {
			t.Errorf("%s: %d %s", name, code, e.Error)
		}
	}
	if _, err := os.Stat(BackupFile(d.O.Dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused config was written: %v", err)
	}
	// The boundaries kept: 16 recipients, 3650 days, 0 days.
	req := ok
	req.Recipients = slices.Repeat(ok.Recipients, maxRecipients)
	req.RetentionDays = ptr(maxRetentionDays)
	if s := configure(t, d, req); s.RetentionDays != maxRetentionDays || len(s.Recipients) != maxRecipients {
		t.Errorf("%+v", s)
	}
	s = configure(t, d, BackupConfigRequest{RetentionDays: ptr(0), Recipients: ok.Recipients})
	if !s.Configured || !s.RcloneConfigSet || s.RetentionDays != 0 || s.Destination != "store:backups/notes" || len(s.Recipients) != 1 {
		t.Errorf("a partial change: %+v", s)
	}
	// The file is root's alone; no answer carries the rclone config.
	if fi, err := os.Stat(BackupFile(d.O.Dir)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("settings file: %v %v", fi, err)
	}
	for _, path := range []string{"/v1/backup/config", "/v1/backup/status"} {
		w := call2(t, h, "GET", path)
		if strings.Contains(w, "hush") || strings.Contains(w, "secret_access_key") {
			t.Errorf("%s shows the rclone config: %s", path, w)
		}
	}
}

// call2 is h's raw answer.
func call2(t *testing.T, h http.Handler, method, path string) string {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w.Body.String()
}

func TestBackupScheduleReachesTheTimer(t *testing.T) {
	t.Parallel()
	_, d, _, key := backupVM(t)
	host := d.Host.(*fakeHost)
	dropIn := filepath.Join(d.Systemd, "notes-backup.timer.d", "schedule.conf")
	configure(t, d, validConfig(key))
	if _, err := os.Stat(dropIn); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the default schedule wrote a drop-in: %v", err)
	}
	if ran := fmt.Sprint(host.calls()); !strings.Contains(ran, "[systemctl daemon-reload] [systemctl restart notes-backup.timer]") {
		t.Errorf("the timer was not started: %s", ran)
	}
	host.calendarErr = errors.New("Failed to parse calendar specification")
	var e errOf
	if code := call(t, d.Handler(), "POST", "/v1/backup/config", BackupConfigRequest{Schedule: ptr("someday")}, &e); code != 400 || !strings.Contains(e.Error, "not a systemd calendar event") {
		t.Errorf("a schedule systemd refuses: %d %s", code, e.Error)
	}
	host.calendarErr = nil
	configure(t, d, BackupConfigRequest{Schedule: ptr("Sun *-*-* 04:30:00 UTC")})
	raw, err := os.ReadFile(dropIn)
	if err != nil || string(raw) != "[Timer]\nOnCalendar=\nOnCalendar=Sun *-*-* 04:30:00 UTC\n" {
		t.Fatalf("drop-in %q %v", raw, err)
	}
	ran := fmt.Sprint(host.calls())
	if !strings.Contains(ran, "[systemd-analyze calendar --iterations 1 -- Sun *-*-* 04:30:00 UTC]") || !strings.Contains(ran, "[systemctl daemon-reload] [systemctl restart notes-backup.timer]") {
		t.Errorf("ran %s", ran)
	}
	var st BackupStatus
	call(t, d.Handler(), "GET", "/v1/backup/status", nil, &st)
	if st.Next != "Sun 2026-10-11 03:04:05 UTC" || st.Settings.Schedule != "Sun *-*-* 04:30:00 UTC" {
		t.Errorf("%+v", st)
	}
	// The daemon gives the timer the schedule again as it starts (the drop-in is under /run).
	if err := os.Remove(dropIn); err != nil {
		t.Fatal(err)
	}
	d.backupResume()
	if _, err := os.Stat(dropIn); err != nil {
		t.Errorf("not written again at start: %v", err)
	}
	// Removed, the settings take the drop-in with them.
	var s BackupSettings
	if code := call(t, d.Handler(), "DELETE", "/v1/backup/config", nil, &s); code != 200 || s.Configured {
		t.Fatalf("%d %+v", code, s)
	}
	if _, err := os.Stat(dropIn); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the drop-in outlived the settings: %v", err)
	}
	if ran := host.calls(); fmt.Sprint(ran[len(ran)-1]) != "[systemctl stop notes-backup.timer]" {
		t.Errorf("the timer was not stopped: %v", ran)
	}
	if _, err := os.Stat(BackupFile(d.O.Dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the settings outlived their removal: %v", err)
	}
}

var objectName = regexp.MustCompile(`^backups/notes/notes-full-\d{8}T\d{6}Z-1\.0\.0-r1\.pgdump\.age$`)

func TestABackupStreamsTheEncryptedDumpThenChecksAndPrunes(t *testing.T) {
	t.Parallel()
	e, d, f, key := backupVM(t)
	h := d.Handler()
	var e409 errOf
	if code := call(t, h, "POST", "/v1/backup", nil, &e409); code != 409 || !strings.Contains(e409.Error, "no backup is configured") {
		t.Errorf("unconfigured: %d %s", code, e409.Error)
	}
	var skipped struct{ Skipped string }
	if code := call(t, h, "POST", "/v1/backup", BackupRequest{IfConfigured: true}, &skipped); code != 200 || skipped.Skipped == "" {
		t.Errorf("unconfigured, if configured: %d %+v", code, skipped)
	}
	configure(t, d, validConfig(key))
	f.volumes, f.staged = map[string][]byte{stackVolumeOf(t, d, "uploads"): []byte("the uploads' archive")}, map[string][]byte{}
	// An archive whose dump retention removed goes; one beside its dump stays.
	const orphan, kept = "store:backups/notes/notes-full-20200101T000000Z-1.0.0-r1.uploads.tar.age", "store:backups/notes/notes-full-20200102T000000Z-1.0.0-r1.uploads.tar.age"
	f.objects[orphan], f.objects[kept], f.objects[strings.TrimSuffix(kept, ".uploads.tar.age")+".pgdump.age"] = []byte("x"), []byte("x"), []byte("x")
	// One just uploaded, without its dump yet, may be a running backup's (another install's).
	const running = "store:backups/notes/notes-full-20261021T000000Z-1.0.0-r1.uploads.tar.age"
	f.objects[running], f.modTimes = []byte("x"), map[string]time.Time{running: time.Now()}
	var started struct{ Job, Target string }
	if code := call(t, h, "POST", "/v1/backup", BackupRequest{IfConfigured: true}, &started); code != 202 || started.Job != "backup" {
		t.Fatalf("%d %+v", code, started)
	}
	j := waitJob(t, h)
	if j.State != "done" || j.Target != started.Target {
		t.Fatalf("%+v", j)
	}
	if _, ok := f.objects[orphan]; ok || f.objects[kept] == nil || f.objects[running] == nil {
		t.Errorf("pruned %v", f.calls())
	}
	delete(f.objects, kept)
	delete(f.objects, running)
	delete(f.objects, strings.TrimSuffix(kept, ".uploads.tar.age")+".pgdump.age")
	if len(f.objects) != 2 {
		t.Fatalf("objects %v", f.objects)
	}
	var obj string
	for k := range f.objects {
		if strings.HasSuffix(k, ".pgdump.age") {
			obj = k
		}
	}
	// The volume's archive beside it, encrypted too, read through a read-only mount.
	archive := strings.TrimSuffix(obj, ".pgdump.age") + ".uploads.tar.age"
	if r, err := age.Decrypt(bytes.NewReader(f.objects[archive]), key); err != nil {
		t.Errorf("%s: %v", archive, err)
	} else if plain, _ := io.ReadAll(r); string(plain) != "the uploads' archive" {
		t.Errorf("archived %q", plain)
	}
	if !objectName.MatchString(strings.TrimPrefix(obj, "store:")) || filepath.Base(obj) != started.Target {
		t.Fatalf("object %s, target %s", obj, started.Target)
	}
	r, err := age.Decrypt(bytes.NewReader(f.objects[obj]), key)
	if err != nil {
		t.Fatal(err)
	}
	if plain, _ := io.ReadAll(r); string(plain) != "the database's dump" {
		t.Errorf("decrypted %q", plain)
	}
	if f.conf != rcloneConf {
		t.Errorf("the runner had the config %q", f.conf)
	}
	// The rclone config does not outlive the run.
	if left, _ := filepath.Glob(filepath.Join(d.Run, "backup-*")); len(left) > 0 {
		t.Errorf("left %v", left)
	}
	dump := fmt.Sprint(e.docker.ran("compose")[len(e.docker.ran("compose"))-1].Args)
	if !strings.Contains(dump, "exec -T postgres pg_dump -U notes -d notes --format=custom --compress=9]") {
		t.Errorf("dumped with %s", dump)
	}
	calls := fmt.Sprint(f.calls())
	for _, want := range []string{
		"[rcat " + obj + "]",
		"[lsjson --files-only --no-mimetype " + obj + "]",
		`[delete --min-age 30d --max-depth 1 --include notes-full-{{\d{8}T\d{6}Z}}-*.pgdump.age store:backups/notes]`,
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("rclone ran %s, not %s", calls, want)
		}
	}
	var st BackupStatus
	call(t, h, "GET", "/v1/backup/status", nil, &st)
	if st.Last == nil || st.Last.Object != started.Target || st.Last.Error != "" || st.Last.Bytes != int64(len(f.objects[obj])+len(f.objects[archive])) || st.Last.Release != (Version{"1.0.0", 1}) || st.Running {
		t.Errorf("%+v", st.Last)
	}
	// No retention, no pruning.
	configure(t, d, BackupConfigRequest{RetentionDays: ptr(0)})
	f.ran = nil
	f.objects[orphan] = []byte("x")
	call(t, h, "POST", "/v1/backup", nil, nil)
	if j := waitJob(t, h); j.State != "done" || strings.Contains(fmt.Sprint(f.calls()), "[delete ") {
		t.Errorf("%+v %v", j, f.calls())
	}
	// An orphan archive goes all the same.
	if _, ok := f.objects[orphan]; ok {
		t.Errorf("kept %s with no retention", orphan)
	}
}

func TestAFailedBackupLeavesNoObjectAndIsRecorded(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		set  func(e *env, f *fakeRclone)
		want string
	}{
		"the dump fails":   {func(e *env, _ *fakeRclone) { e.docker.dumpErr = errors.New("pg_dump: connection refused") }, "the dump: pg_dump: connection refused"},
		"the upload fails": {func(_ *env, f *fakeRclone) { f.rcatErr = errors.New("403 Forbidden") }, "the upload: 403 Forbidden"},
		"the dump fails at once": {func(e *env, _ *fakeRclone) {
			e.docker.dump, e.docker.dumpErr = "", errors.New("pg_dump: connection refused")
		}, "the dump: pg_dump: connection refused"},
		"the upload fails, its rest kept": {func(_ *env, f *fakeRclone) {
			f.rcatErr, f.delErr = errors.New("timeout"), errors.New("unreachable")
		}, "the upload: timeout"},
		"the upload quits at once": {func(e *env, f *fakeRclone) {
			f.quits = true
			e.docker.dump = strings.Repeat("x", 1<<20)
		}, "the upload: the upload ended"},
		"the upload is refused at once": {func(e *env, f *fakeRclone) {
			f.early = errors.New("401 Unauthorized")
			e.docker.dump = strings.Repeat("x", 1<<20) // more than the pipe holds
		}, "the upload: 401 Unauthorized"},
		"the size is short":  {func(_ *env, f *fakeRclone) { f.short = true }, "the destination holds"},
		"the archive fails":  {func(_ *env, f *fakeRclone) { f.tarErr = exitCode(2) }, "the archive of the volume uploads: exit status 2"},
		"the pruning fails?": {func(_ *env, f *fakeRclone) { f.deleteErr = errors.New("no delete") }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, d, f, key := backupVM(t)
			configure(t, d, validConfig(key))
			c.set(e, f)
			h := d.Handler()
			call(t, h, "POST", "/v1/backup", nil, nil)
			j := waitJob(t, h)
			var st BackupStatus
			call(t, h, "GET", "/v1/backup/status", nil, &st)
			if c.want == "" {
				if j.State != "done" || st.Last.Error != "" || st.Last.PruneError != "no delete" {
					t.Errorf("%+v %+v", j, st.Last)
				}
				return
			}
			if j.State != "failed" || !strings.HasPrefix(j.Error, c.want) || st.Last == nil || !strings.HasPrefix(st.Last.Error, c.want) {
				t.Errorf("%+v %+v", j, st.Last)
			}
			log := strings.Join(j.Log, "\n")
			switch name {
			case "the dump fails", "the upload fails", "the size is short", "the archive fails":
				if len(f.objects) != 0 || !strings.Contains(fmt.Sprint(f.calls()), "[deletefile store:backups/notes/notes-full-") {
					t.Errorf("the cut backup was kept: %v", f.calls())
				}
				// The volumes go first: a backup whose archive failed dumps nothing.
				if name == "the archive fails" && strings.Contains(fmt.Sprint(e.docker.ran("compose")), "pg_dump") {
					t.Error("dumped after the archive failed")
				}
			case "the upload fails, its rest kept":
				if !strings.Contains(log, "may be left incomplete: deleting it failed (unreachable)") {
					t.Errorf("a cut backup left is not told: %s", log)
				}
			}
			if name != "the upload fails, its rest kept" && strings.Contains(log, "may be left incomplete") {
				t.Errorf("an object not there is told incomplete: %s", log)
			}
		})
	}
}

func TestAProfileOfTablesKeepsTheirDataOnly(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	e.r, e.self = bundledInstaller(t, "1.0.0", 1, func(s *spec.Spec) {
		s.Backup.Config = map[string]any{"default_profile": "full", "profiles": map[string]any{"full": "all", "config": []any{"users", "audit.Settings"}, "x": []any{"gone"},
			"lean": map[string]any{"exclude": []any{"events", "audit.log_*"}}}}
	})
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	f := &fakeRclone{objects: map[string][]byte{}}
	e.docker.runner = f.run
	e.docker.psql = func(q string) (string, error) {
		if !strings.Contains(q, "FROM pg_tables") {
			return "", fmt.Errorf("query %s", q)
		}
		return "audit\x1fSettings\x1eaudit\x1fevents\x1epublic\x1fusers\x1epublic\x1fsessions", nil
	}
	key, _ := age.GenerateX25519Identity()
	d := daemon(e, &fakeHost{})
	configure(t, d, validConfig(key))
	h := d.Handler()
	call(t, h, "POST", "/v1/backup", BackupRequest{Profile: "config"}, nil)
	if j := waitJob(t, h); j.State != "done" || !strings.Contains(j.Target, "notes-config-") {
		t.Fatalf("%+v", j)
	}
	dump := e.docker.ran("compose")
	args := strings.Join(dump[len(dump)-1].Args, " ")
	if !strings.HasSuffix(args, ` --compress=9 --exclude-table-data="audit"."events" --exclude-table-data="public"."sessions"`) {
		t.Errorf("dumped with %s", args)
	}
	// An exclude profile dumps every table, the rows of those its patterns match left out.
	call(t, h, "POST", "/v1/backup", BackupRequest{Profile: "lean"}, nil)
	if j := waitJob(t, h); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	dump = e.docker.ran("compose")
	if args := strings.Join(dump[len(dump)-1].Args, " "); !strings.HasSuffix(args, " --compress=9 --exclude-table-data=events --exclude-table-data=audit.log_*") {
		t.Errorf("dumped with %s", args)
	}
	call(t, h, "POST", "/v1/backup", BackupRequest{Profile: "x"}, nil)
	if j := waitJob(t, h); !strings.Contains(strings.Join(j.Log, "\n"), "profile x lists the table gone, which the database lacks") {
		t.Errorf("%+v", j.Log)
	}
	var e400 errOf
	if code := call(t, h, "POST", "/v1/backup", BackupRequest{Profile: "nope"}, &e400); code != 400 {
		t.Errorf("an unknown profile: %d %s", code, e400.Error)
	}
}

func TestListAndVerifyUseTheDestination(t *testing.T) {
	t.Parallel()
	_, d, f, key := backupVM(t)
	h := d.Handler()
	var e409 errOf
	if code := call(t, h, "GET", "/v1/backup/list", nil, &e409); code != 409 {
		t.Errorf("unconfigured: %d", code)
	}
	configure(t, d, validConfig(key))
	f.objects["store:backups/notes/notes-full-20261009T030000Z-1.0.0-r1.pgdump.age"] = []byte("abc")
	f.objects["store:backups/notes/notes-crm-full-20261009T030000Z-1.0.0-r1.pgdump.age"] = []byte("another project's")
	f.objects["store:backups/notes/notes-lite-20261008T030000Z-0.9.0-r1.pgdump.age"] = []byte("a profile gone")
	f.objects["store:backups/notes/notes-verify.txt"] = []byte("probe")
	var list []BackupObject
	if code := call(t, h, "GET", "/v1/backup/list", nil, &list); code != 200 || len(list) != 2 || list[0].Name != "notes-full-20261009T030000Z-1.0.0-r1.pgdump.age" || list[0].Size != 3 || list[1].Name != "notes-lite-20261008T030000Z-0.9.0-r1.pgdump.age" {
		t.Errorf("%d %+v", code, list)
	}
	if j := verifyJob(t, h, nil); j.State != "done" || j.Target != "store:backups/notes" || !strings.Contains(j.Result, "reachable and writable") || strings.Contains(j.Result, "proposed") {
		t.Errorf("%+v", j)
	}
	if _, ok := f.objects["store:backups/notes/notes-verify.txt"]; !ok {
		t.Errorf("no probe: %v", f.calls())
	}
	f.rcatErr = errors.New("403")
	if j := verifyJob(t, h, nil); j.State != "failed" || !strings.Contains(j.Error, "writing store:backups/notes/notes-verify.txt") || j.Result != "" {
		t.Errorf("an unwritable destination: %+v", j)
	}
	// A job like the others: journaled, and one at a time.
	var a EventsAnswer
	call(t, h, "GET", "/v1/events", nil, &a)
	var ended []string
	for _, e := range a.Events {
		if e.Kind == evJobEnded && e.JobKind == "backup-verify" {
			ended = append(ended, e.State)
		}
	}
	if fmt.Sprint(ended) != "[done failed]" {
		t.Errorf("journaled %v: %+v", ended, a.Events)
	}
	d.mu.Lock()
	d.job = &job{Kind: "upgrade", State: "running"}
	d.mu.Unlock()
	if code := call(t, h, "POST", "/v1/backup/verify", nil, nil); code != 409 {
		t.Errorf("during another job: %d", code)
	}
}

// verifyJob starts a backup-verify job with body and waits for its end.
func verifyJob(t *testing.T, h http.Handler, body any) JobStatus {
	t.Helper()
	var started JobStarted
	if code := call(t, h, "POST", "/v1/backup/verify", body, &started); code != 202 || started.Job != "backup-verify" || started.Started.IsZero() {
		t.Fatalf("%d %v", code, started)
	}
	j := waitJob(t, h)
	if j.Kind != "backup-verify" || j.Target != started.Target || j.Started == nil || !j.Started.Equal(started.Started) {
		t.Fatalf("%+v, not the job started: %v", j, started)
	}
	return j
}

func TestVerifyTriesProposedSettingsWithoutSavingThem(t *testing.T) {
	t.Parallel()
	_, d, f, key := backupVM(t)
	h := d.Handler()
	// Nothing saved yet: the whole settings proposed.
	try := validConfig(key)
	try.Destination = ptr("store:elsewhere")
	if j := verifyJob(t, h, try); j.State != "done" || !strings.HasSuffix(j.Result, "(the settings proposed, not saved)") {
		t.Fatalf("%+v", j)
	}
	if _, ok := f.objects["store:elsewhere/notes-verify.txt"]; !ok {
		t.Errorf("no probe: %v", f.calls())
	}
	if _, err := os.Stat(BackupFile(d.O.Dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the proposed settings were saved: %v", err)
	}
	// Saved settings: a destination alone is tried over them, and they stay as they were.
	configure(t, d, validConfig(key))
	before := read(t, BackupFile(d.O.Dir))
	if j := verifyJob(t, h, BackupConfigRequest{Destination: ptr("store:third")}); j.State != "done" || j.Target != "store:third" {
		t.Fatalf("%+v", j)
	}
	if _, ok := f.objects["store:third/notes-verify.txt"]; !ok {
		t.Errorf("no probe: %v", f.calls())
	}
	if read(t, BackupFile(d.O.Dir)) != before {
		t.Error("verify changed the saved settings")
	}
	// Proposed settings are checked as a change is.
	var e400 errOf
	if code := call(t, h, "POST", "/v1/backup/verify", BackupConfigRequest{Destination: ptr("nowhere:x")}, &e400); code != 400 || !strings.Contains(e400.Error, `defines no remote "nowhere"`) {
		t.Errorf("%d %s", code, e400.Error)
	}
}

func TestAReleaseWithoutABackupRefuses(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	e.r, e.self = bundledInstaller(t, "1.0.0", 1, func(s *spec.Spec) { s.Backup = nil })
	h := daemon(e, &fakeHost{}).Handler()
	var e409 errOf
	if code := call(t, h, "GET", "/v1/backup/config", nil, &e409); code != 409 || !strings.Contains(e409.Error, "declares no pg-age-rclone backup") {
		t.Errorf("%d %s", code, e409.Error)
	}
	// The timer's run skips it, saying why; a run asked for is refused.
	var skipped struct{ Skipped string }
	if code := call(t, h, "POST", "/v1/backup", BackupRequest{IfConfigured: true}, &skipped); code != 200 || !strings.Contains(skipped.Skipped, "declares no pg-age-rclone backup") {
		t.Errorf("%d %+v", code, skipped)
	}
	if code := call(t, h, "POST", "/v1/backup", nil, nil); code != 409 {
		t.Errorf("%d", code)
	}
}

func TestAnUpgradeBacksUpFirstUnlessSkipped(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.release("2.0.0", 1, nil)
	f := &fakeRclone{objects: map[string][]byte{}}
	p.env.docker.runner = f.run
	key, _ := age.GenerateX25519Identity()
	d := daemon(p.env, &fakeHost{})
	configure(t, d, validConfig(key))
	h := d.Handler()
	no := false
	if code := call(t, h, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", Reboot: &no}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	j := waitJob(t, h)
	log := strings.Join(j.Log, "\n")
	if j.State != "done" || !strings.Contains(log, "a backup first: notes-full-") || !strings.Contains(log, "backed up: ") || len(f.objects) != 2 {
		t.Fatalf("%+v", j)
	}
	if i, k := strings.Index(log, "backed up: "), strings.Index(log, "next boot runs it"); i > k {
		t.Errorf("the backup came after the OS: %s", log)
	}
	// A failed backup does not stop the upgrade.
	f.rcatErr = errors.New("403")
	os.Remove(filepath.Join(d.Run, osWritten))
	call(t, h, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", Reboot: &no}, nil)
	if j := waitJob(t, h); j.State != "done" || !strings.Contains(strings.Join(j.Log, "\n"), "the backup failed, the upgrade goes on") {
		t.Errorf("%+v", j)
	}
	f.ran = nil
	os.Remove(filepath.Join(d.Run, osWritten))
	call(t, h, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", Reboot: &no, SkipBackup: true}, nil)
	if j := waitJob(t, h); j.State != "done" || len(f.calls()) != 0 {
		t.Errorf("skipped: %+v %v", j, f.calls())
	}
}

// Settings whose profile a newer release no longer has are refused until changed, and a change
// starts from them.
func TestSettingsOfAProfileGoneAreChangedNotStuck(t *testing.T) {
	t.Parallel()
	_, d, _, key := backupVM(t)
	configure(t, d, validConfig(key))
	raw, err := os.ReadFile(BackupFile(d.O.Dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(BackupFile(d.O.Dir), bytes.Replace(raw, []byte("profile: full"), []byte("profile: nightly"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	// The daemon's start still starts the timer: its runs fail, saying why.
	host := d.Host.(*fakeHost)
	n := len(host.calls())
	d.backupResume()
	if ran := fmt.Sprint(host.calls()[n:]); !strings.Contains(ran, "[systemctl restart notes-backup.timer]") {
		t.Errorf("the timer was left stopped: %s", ran)
	}
	h := d.Handler()
	var e errOf
	if code := call(t, h, "GET", "/v1/backup/config", nil, &e); code != 500 || !strings.Contains(e.Error, `profile "nightly"`) || !strings.Contains(e.Error, "backup config` changes it") {
		t.Errorf("%d %s", code, e.Error)
	}
	if s := configure(t, d, BackupConfigRequest{Profile: ptr("full")}); s.Profile != "full" || s.Destination != "store:backups/notes" {
		t.Errorf("%+v", s)
	}
}

// The timer's run skips a VM not installed yet; a run asked for is refused.
func TestTheTimersRunSkipsAVMNotInstalled(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	h := daemon(e, &fakeHost{}).Handler()
	var skipped struct{ Skipped string }
	if code := call(t, h, "POST", "/v1/backup", BackupRequest{IfConfigured: true}, &skipped); code != 200 || !strings.Contains(skipped.Skipped, "not installed yet") {
		t.Errorf("%d %+v", code, skipped)
	}
	if code := call(t, h, "POST", "/v1/backup", nil, nil); code != 409 {
		t.Errorf("%d", code)
	}
}

// The timer's run fails on an install it cannot read: only one not made yet is skipped.
func TestTheTimersRunFailsOnAnUnreadableInstall(t *testing.T) {
	t.Parallel()
	_, d, _, key := backupVM(t)
	configure(t, d, validConfig(key))
	if err := os.WriteFile(filepath.Join(d.O.Dir, envName), []byte("not an env line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var e errOf
	if code := call(t, d.Handler(), "POST", "/v1/backup", BackupRequest{IfConfigured: true}, &e); code < 400 {
		t.Errorf("%d %s", code, e.Error)
	}
}

// Settings that do not decode still start the timer at the daemon's start.
func TestUndecodableSettingsStillStartTheTimer(t *testing.T) {
	t.Parallel()
	_, d, _, _ := backupVM(t)
	if err := os.WriteFile(BackupFile(d.O.Dir), []byte("destination: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.backupResume()
	if ran := fmt.Sprint(d.Host.(*fakeHost).calls()); !strings.Contains(ran, "[systemctl restart notes-backup.timer]") {
		t.Errorf("the timer was left stopped: %s", ran)
	}
}

// A schedule the timer would refuse starts it on the default.
func TestAStoredScheduleRefusedStartsTheTimerOnTheDefault(t *testing.T) {
	t.Parallel()
	_, d, _, key := backupVM(t)
	req := validConfig(key)
	req.Schedule = ptr("Mon *-*-* 01:00:00 UTC")
	configure(t, d, req)
	raw, _ := os.ReadFile(BackupFile(d.O.Dir))
	if err := os.WriteFile(BackupFile(d.O.Dir), bytes.Replace(raw, []byte("Mon *-*-* 01:00:00 UTC"), []byte("someday"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	d.Host.(*fakeHost).calendarErr = errors.New("Failed to parse calendar specification")
	d.backupResume()
	if _, err := os.Stat(filepath.Join(d.Systemd, "notes-backup.timer.d", "schedule.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused schedule reached the timer: %v", err)
	}
	if ran := d.Host.(*fakeHost).calls(); fmt.Sprint(ran[len(ran)-1]) != "[systemctl restart notes-backup.timer]" {
		t.Errorf("the timer was not started: %v", ran)
	}
}

// The daemon's stop waits for a running backup's cleanup.
func TestStoppingWaitsForABackup(t *testing.T) {
	t.Parallel()
	_, d, _, _ := backupVM(t)
	dir, err := os.MkdirTemp("", "hostd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	d.backups.Add(1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx, filepath.Join(dir, "host.sock")) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
		t.Fatal("the daemon stopped before the backup ended")
	case <-time.After(50 * time.Millisecond):
	}
	d.backups.Done()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon did not stop once the backup ended")
	}
}

// Off the VM (no backup timer), the settings are saved and no systemd call is made.
func TestSettingsWithoutATimerTouchNoUnit(t *testing.T) {
	t.Parallel()
	_, d, _, key := backupVM(t)
	host := d.Host.(*fakeHost)
	host.noTimer = true
	req := validConfig(key)
	req.Schedule = ptr("Mon *-*-* 01:00:00 UTC")
	configure(t, d, req)
	for _, c := range host.calls() {
		if c[0] == "systemctl" {
			t.Errorf("ran %v", c)
		}
	}
	if _, err := os.Stat(filepath.Join(d.Systemd, "notes-backup.timer.d")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a drop-in was written: %v", err)
	}
}

// The timer's run waits for a job running to end, at most scheduledBackupWait, then backs up; a
// backup asked by hand is refused at once.
func TestTheTimersRunWaitsForARunningJob(t *testing.T) {
	t.Parallel()
	_, d, _, key := backupVM(t)
	configure(t, d, validConfig(key))
	h := d.Handler()
	running, err := d.startJob("backup-verify", "store:backups/notes", nil)
	if err != nil {
		t.Fatal(err)
	}
	var e409 errOf
	if code := call(t, h, "POST", "/v1/backup", nil, &e409); code != 409 || !strings.Contains(e409.Error, "a job (backup-verify) is running") {
		t.Errorf("by hand: %d %s", code, e409.Error)
	}
	// The job's end wakes the run at once: no poll of the slot (a Unit of an hour would show it).
	d.Unit = time.Hour
	time.AfterFunc(20*time.Millisecond, func() { d.finish(running, nil) })
	var started JobStarted
	if code := call(t, h, "POST", "/v1/backup", BackupRequest{IfConfigured: true}, &started); code != 202 || started.Job != "backup" || !started.Started.After(running.Started) {
		t.Fatalf("the timer's run: %d %+v", code, started)
	}
	if j := waitJob(t, h); j.State != "done" || j.Kind != "backup" || j.Target != started.Target {
		t.Errorf("%+v", j)
	}
	// What the backup reads is read once the slot is free: settings removed during the wait
	// skip the run, rather than back up with the settings read before it.
	running, err = d.startJob("backup-verify", "store:backups/notes", nil)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(20*time.Millisecond, func() {
		if code := call(t, h, "DELETE", "/v1/backup/config", nil, nil); code != 200 {
			t.Errorf("removing the settings: %d", code)
		}
		d.finish(running, nil)
	})
	var skipped struct{ Skipped string }
	if code := call(t, h, "POST", "/v1/backup", BackupRequest{IfConfigured: true}, &skipped); code != 200 || skipped.Skipped != "no backup is configured" {
		t.Fatalf("settings removed during the wait: %d %+v", code, skipped)
	}
	configure(t, d, validConfig(key))
	d.Unit = time.Millisecond
	// A job that does not end within the wait: the timer's run fails, saying why.
	if _, err := d.startJob("backup-verify", "store:backups/notes", nil); err != nil {
		t.Fatal(err)
	}
	d.Unit = time.Microsecond // scheduledBackupWait is then 0.6 ms
	if code := call(t, h, "POST", "/v1/backup", BackupRequest{IfConfigured: true}, &e409); code != 409 || !strings.Contains(e409.Error, "a job (backup-verify) is running") {
		t.Errorf("past the wait: %d %s", code, e409.Error)
	}
}

// A job that claims the slot and ends between a waiting run's read of what it runs with and
// its claim makes the run read again: it never runs on values read before that job's end.
func TestARunWaitingItsTurnReadsAgainAfterAJobBetween(t *testing.T) {
	t.Parallel()
	d := daemon(installedVM(t), &fakeHost{})
	var reads int
	j, err := d.startJobWhenFree(context.Background(), time.Hour, "backup", func() (string, error) {
		reads++
		if reads == 1 { // a short job claims and ends once this read is done
			other, err := d.startJob("backup-verify", "store:b", nil)
			if err != nil {
				return "", err
			}
			d.finish(other, nil)
		}
		return fmt.Sprintf("read %d", reads), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if reads != 2 || j.Target != "read 2" {
		t.Errorf("read %d times, the job's target %q", reads, j.Target)
	}
	// A job running as the run read, which ends before its claim: the run reads again.
	d.finish(j, nil)
	other, err := d.startJob("backup-verify", "store:b", nil)
	if err != nil {
		t.Fatal(err)
	}
	reads = 0
	if j, err = d.startJobWhenFree(context.Background(), time.Hour, "backup", func() (string, error) {
		if reads++; reads == 1 {
			d.finish(other, nil)
		}
		return "", nil
	}); err != nil || reads != 2 {
		t.Errorf("%v: read %d times around a job's end", err, reads)
	}
	// Positive control: with no job between, one read.
	d.finish(j, nil)
	reads = 0
	if _, err = d.startJobWhenFree(context.Background(), 0, "backup", func() (string, error) { reads++; return "once", nil }); err != nil || reads != 1 {
		t.Errorf("%v: read %d times", err, reads)
	}
}

// A Docker install runs the VM's backup code in the installer's process: its settings, a backup
// streamed through the runner, the schedule written as the install's own systemd units.
// withRunner gives e's release a backup runner, as ship publishes one for a project with a
// pg-age-rclone backup.
func withRunner(e *env) {
	a := &e.r.Manifest.Artifacts
	a.Images[backup.Key], a.Configs[backup.Key], a.Manifests[backup.Key] = digest, config, manifest
}

func TestADockerInstallBacksUpThroughItsOwnDaemon(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	withRunner(e)
	if err := e.install(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	f := &fakeRclone{objects: map[string][]byte{}}
	e.docker.runner = f.run
	e.docker.dump = "the database's dump"
	key, _ := age.GenerateX25519Identity()
	units, host := t.TempDir(), &fakeHost{}
	in := e.installer(func(o *Options) { o.Systemd, o.Host = units, host })
	d, done, err := in.dockerHostd(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { done() }() // the daemon made last
	req := validConfig(key)
	req.Schedule = ptr("Mon *-*-* 04:00:00")
	configure(t, d, req)
	unit := installUnit("notes", "backup", e.dir)
	timer, err := os.ReadFile(filepath.Join(units, unit+".timer"))
	if err != nil || !strings.Contains(string(timer), "OnCalendar=Mon *-*-* 04:00:00\n") {
		t.Fatalf("timer %q %v", timer, err)
	}
	svc, _ := os.ReadFile(filepath.Join(units, unit+".service"))
	if !strings.Contains(string(svc), "ExecStart="+filepath.Join(e.dir, "notes-stow")+" backup run --if-configured --dir "+e.dir+"\n") {
		t.Errorf("service %s", svc)
	}
	if !strings.Contains(fmt.Sprint(host.calls()), "systemctl enable --now "+unit+".timer") {
		t.Errorf("ran %v", host.calls())
	}
	// No app socket on Docker: a config without the operator's digest is the operator's.
	if err := os.Remove(BackupRcloneFile(d.O.Dir)); err != nil {
		t.Fatal(err)
	}
	h := d.Handler()
	call(t, h, "POST", "/v1/backup", nil, nil)
	if j := waitJob(t, h); j.State != "done" || len(f.objects) != 2 {
		t.Fatalf("%+v %v", j, f.objects)
	}
	// Units gone (an uninstall that kept the data), an install writes them again from the settings.
	for _, ext := range []string{".timer", ".service"} {
		_ = os.Remove(filepath.Join(units, unit+ext))
	}
	done()
	rerun := e.installer(func(o *Options) { o.Systemd, o.Host = units, host })
	if err := rerun.Install(context.Background()); err != nil {
		t.Fatalf("the re-run: %v\n%s", err, e.log)
	}
	rerun.Finished(context.Background(), onDocker)
	if again, err := os.ReadFile(filepath.Join(units, unit+".timer")); err != nil || string(again) != string(timer) {
		t.Fatalf("the timer again: %q %v", again, err)
	}
	d, done, err = in.dockerHostd(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h = d.Handler()
	// Removing the settings removes the units.
	if code := call(t, h, "DELETE", "/v1/backup/config", nil, nil); code != 200 {
		t.Fatalf("remove: %d", code)
	}
	if _, err := os.Stat(filepath.Join(units, unit+".timer")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the timer is left: %v", err)
	}
	if installUnit("notes", "backup", e.dir) == installUnit("notes", "backup", e.dir+"2") {
		t.Error("two installs share a unit")
	}
}

// A Docker install's disk-pressure watch: units at the install, a check that POSTs past soft_pct
// with the hook's bearer and records it, which status shows.
func TestADockerInstallWatchesItsDisk(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.to("1.0.0", func(s *spec.Spec) {
		s.Hooks.Maintenance = &spec.Maintenance{DiskPressure: &spec.DiskPressure{SoftPct: 75, HardPct: 90, Post: "http://api:8080/purge"}}
	})
	if err := e.install(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	units := t.TempDir()
	in := e.installer(func(o *Options) { o.Systemd, o.Host = units, &fakeHost{} })
	if err := in.InstallWatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	unit := installUnit("notes", "disk", e.dir)
	if timer, err := os.ReadFile(filepath.Join(units, unit+".timer")); err != nil || !strings.Contains(string(timer), "OnUnitActiveSec=600s") {
		t.Fatalf("timer %q %v", timer, err)
	}
	if svc, _ := os.ReadFile(filepath.Join(units, unit+".service")); !strings.Contains(string(svc), " disk-check --dir "+e.dir) {
		t.Errorf("service %s", svc)
	}
}

// One process at a time runs a Docker install's jobs: a second daemon waits for the first.
func TestADockerInstallsDaemonsTakeTurns(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	in := e.installer(nil)
	_, done, err := in.dockerHostd(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !hostBusy(e.dir) {
		t.Error("the lock is not seen held")
	}
	got := make(chan struct{})
	go func() {
		_, d2, err := e.installer(nil).dockerHostd(context.Background())
		if err == nil {
			d2()
		}
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("a second daemon ran beside the first")
	case <-time.After(200 * time.Millisecond):
	}
	done()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the second daemon did not take its turn")
	}
	if left, _ := filepath.Glob(filepath.Join(e.dir, ".host-*")); len(left) > 0 {
		t.Errorf("left %v", left)
	}
	if hostBusy(e.dir) {
		t.Error("the lock is seen held once released")
	}
}

// A read goes to the daemon that takes no lock; a job, and every request after it, to the one
// that runs jobs.
func TestTheDockerTransportRoutesReadsAndJobs(t *testing.T) {
	t.Parallel()
	var made []string
	handler := func(name string) func() (http.Handler, error) {
		return func() (http.Handler, error) {
			made = append(made, name)
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, name) }), nil
		}
	}
	c := &http.Client{Transport: &lazyTransport{read: handler("read"), jobs: handler("jobs")}}
	ask := func(method, path string) string {
		req, _ := http.NewRequest(method, "http://host"+path, nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	got := []string{ask(http.MethodGet, "/v1/backup/status"), ask(http.MethodPost, "/v1/backup/config"), ask(http.MethodPost, "/v1/backup"), ask(http.MethodGet, "/v1/job")}
	if strings.Join(got, ",") != "read,read,jobs,jobs" || strings.Join(made, ",") != "read,jobs" {
		t.Fatalf("answered %v, made %v", got, made)
	}
}

// A request without a body reaches a handler as a server gives it: an empty body.
func TestTheInProcessTransportGivesAnEmptyBody(t *testing.T) {
	t.Parallel()
	tr := handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil || len(b) != 0 {
			t.Errorf("%q %v", b, err)
		}
	})}
	req, _ := http.NewRequest(http.MethodPost, "http://host/v1/backup/verify", nil)
	req.Body = nil
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
}

// A backup's volume is the one Compose labelled as the project's, else the one of its name when no
// other project's; a restore makes a missing one as the compose file declares it.
func TestTheStacksVolume(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	in := e.installer(nil)
	in.R = &Release{Spec: e.r.Spec, Manifest: e.r.Manifest, Compose: []byte("volumes:\n  keys:\n    driver: local\n    driver_opts: { type: nfs, o: \"addr=192.0.2.9\" }\n    labels: { tier: data, empty: }\n  listed: { labels: [\"tier=cache\"] }\n")}
	env := NewEnvFile()
	env.Set("COMPOSE_PROJECT_NAME", "notes")
	var owner string
	var inspectErr error
	e.docker.answer = func(a string) ([]byte, bool, error) {
		switch {
		case strings.HasPrefix(a, "volume ls "):
			if strings.Contains(a, "volume=labelled") {
				return []byte("notes_labelled\n"), true, nil
			}
			return nil, true, nil
		case strings.HasPrefix(a, "volume inspect "):
			return []byte(owner + "\n"), true, inspectErr
		case strings.HasPrefix(a, "volume create "):
			return nil, true, nil
		}
		return nil, false, nil
	}
	ctx := context.Background()
	if v, err := in.stackVolume(ctx, env, "labelled", false); v != "notes_labelled" || err != nil {
		t.Errorf("labelled: %q %v", v, err)
	}
	if v, err := in.stackVolume(ctx, env, "keys", false); v != "notes_keys" || err != nil {
		t.Errorf("adopted: %q %v", v, err)
	}
	owner = "other"
	if _, err := in.stackVolume(ctx, env, "keys", false); err == nil || !strings.Contains(err.Error(), "belongs to the Compose project other, not notes") {
		t.Errorf("another project's: %v", err)
	}
	owner, inspectErr = "", errors.New("Error: No such volume: notes_keys")
	if v, err := in.stackVolume(ctx, env, "keys", false); v != "" || err != nil {
		t.Errorf("never created: %q %v", v, err)
	}
	if v, err := in.stackVolume(ctx, env, "keys", true); v != "notes_keys" || err != nil {
		t.Fatalf("created: %q %v", v, err)
	}
	created := e.docker.ran("volume create")
	if len(created) != 1 || strings.Join(created[0].Args, " ") != "volume create --label com.docker.compose.project=notes --label com.docker.compose.volume=keys --driver local --opt o=addr=192.0.2.9 --opt type=nfs --label empty= --label tier=data notes_keys" {
		t.Errorf("created with %v", created)
	}
	// Labels as a list, as Compose takes them too.
	if _, err := in.stackVolume(ctx, env, "listed", true); err != nil {
		t.Fatal(err)
	}
	if created := e.docker.ran("volume create"); len(created) != 2 || !strings.HasSuffix(strings.Join(created[1].Args, " "), " --label tier=cache notes_listed") {
		t.Errorf("created with %v", created)
	}
}

// The volume scripts, run by a shell on a directory standing in for the volume (/v): the archive
// leaves a staging directory out, the swap brings every entry back (hidden ones, spaces) with the
// root's mode and drops what came after, and an empty archive keeps the root's mode.
func TestTheVolumeScripts(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"sh", "tar", "rm", "mkdir", "rmdir", "mv", "chown", "chmod", "stat"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("no %s here", bin)
		}
	}
	v := t.TempDir()
	run := func(script string, stdin io.Reader) []byte {
		t.Helper()
		cmd := exec.Command("sh", "-euc", strings.ReplaceAll(script, "/v", v))
		cmd.Stdin = stdin
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: %v", script, err)
		}
		return out
	}
	for name, body := range map[string]string{"keep.txt": "kept", ".hidden": "h", "a b/c": "c", "-dash": "d", restoreStage + "/junk": "j", restoreReplaced + "/old": "o"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(v, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(v, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(v, 0o750); err != nil {
		t.Fatal(err)
	}
	archive := run(archiveScript, nil)
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(h.Name, restoreStage) || strings.Contains(h.Name, restoreReplaced) {
			t.Errorf("archived %s", h.Name)
		}
	}
	// Since the backup: a file gone, one new, the root's mode changed.
	_ = os.Remove(filepath.Join(v, "keep.txt"))
	_ = os.WriteFile(filepath.Join(v, "later.txt"), nil, 0o600)
	_ = os.Chmod(v, 0o755)
	// A leftover of an earlier restore's former data goes as the archive is staged; an archive
	// holding the restore's own names never extracts them.
	_ = os.MkdirAll(filepath.Join(v, restoreReplaced), 0o755)
	_ = os.WriteFile(filepath.Join(v, restoreReplaced, "leftover"), nil, 0o600)
	var crafted bytes.Buffer
	tw := tar.NewWriter(&crafted)
	tr = tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		body, _ := io.ReadAll(tr)
		_ = tw.WriteHeader(h)
		_, _ = tw.Write(body)
	}
	_ = tw.WriteHeader(&tar.Header{Name: "./" + restoreReplaced + "/planted", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("p"))
	_ = tw.Close()
	run(stageScript, &crafted)
	if _, err := os.Stat(filepath.Join(v, restoreReplaced)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging kept the former leftover: %v", err)
	}
	run(swapVolumeScript, nil)
	for _, planted := range []string{"planted", restoreReplaced + "/planted"} {
		if _, err := os.Stat(filepath.Join(v, planted)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the archive's %s was extracted: %v", planted, err)
		}
	}
	// The former data, moved aside whole, until the restore is done.
	if b, err := os.ReadFile(filepath.Join(v, restoreReplaced, "later.txt")); err != nil || len(b) != 0 {
		t.Errorf("replaced: %q %v", b, err)
	}
	run(dropVolumeScript, nil)
	for name, body := range map[string]string{"keep.txt": "kept", ".hidden": "h", "a b/c": "c", "-dash": "d"} {
		if b, err := os.ReadFile(filepath.Join(v, name)); err != nil || string(b) != body {
			t.Errorf("%s: %q %v", name, b, err)
		}
	}
	for _, gone := range []string{"later.txt", restoreStage, restoreReplaced} {
		if _, err := os.Stat(filepath.Join(v, gone)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is there: %v", gone, err)
		}
	}
	if fi, _ := os.Stat(v); fi.Mode().Perm() != 0o750 {
		t.Errorf("root mode %v", fi.Mode().Perm())
	}
	// An empty archive (a volume never created at the backup) empties it, its root's mode kept.
	_ = os.Chmod(v, 0o700)
	var empty bytes.Buffer
	_ = tar.NewWriter(&empty).Close()
	run(stageScript, &empty)
	run(swapVolumeScript, nil)
	run(dropVolumeScript, nil)
	if entries, _ := os.ReadDir(v); len(entries) != 0 {
		t.Errorf("left %v", entries)
	}
	if fi, _ := os.Stat(v); fi.Mode().Perm() != 0o700 {
		t.Errorf("root mode after an empty archive %v", fi.Mode().Perm())
	}
}

// Through the app socket the rclone config — remotes, credentials, the programs rclone's options
// name — is the operator's: one given is refused, saved or verified, the operator's own included,
// and so is a destination carrying a connection string; while the settings are locked, the app
// verifies the saved ones alone.
func TestTheRcloneConfigIsTheOperators(t *testing.T) {
	t.Parallel()
	_, d, _, key := backupVM(t)
	app := d.routes(true)
	var e errOf
	other := func(conf string) BackupConfigRequest {
		req := validConfig(key)
		req.RcloneConfig = ptr(conf)
		return req
	}
	const s3 = "[store]\ntype = s3\nenv_auth = true\nshared_credentials_file = /config/rclone/rclone.conf\nprofile = p\n[profile p]\ncredential_process = /bin/sh -c id\n"
	for _, req := range []BackupConfigRequest{validConfig(key), other(s3)} {
		if code := call(t, app, "POST", "/v1/backup/config", req, &e); code != 403 || !strings.Contains(e.Error, "rclone_config is the operator's") {
			t.Errorf("the app's config, none saved: %d %s", code, e.Error)
		}
		if code := call(t, app, "POST", "/v1/backup/verify", req, &e); code != 403 || !strings.Contains(e.Error, "rclone_config is the operator's") {
			t.Errorf("the app verifying its config: %d %s", code, e.Error)
		}
	}
	configure(t, d, validConfig(key))
	var s BackupSettings
	if code := call(t, app, "POST", "/v1/backup/config", validConfig(key), &e); code != 403 {
		t.Errorf("the operator's config posted back through the app: %d", code)
	}
	for _, dest := range []string{"store,ssh='sh -c id':x", ":sftp,host=h,ssh=id:x", ":local:x"} {
		if code := call(t, app, "POST", "/v1/backup/config", BackupConfigRequest{Destination: ptr(dest)}, &e); code != 400 {
			t.Errorf("the app's destination %s: %d", dest, code)
		}
		if code := call(t, app, "POST", "/v1/backup/verify", BackupConfigRequest{Destination: ptr(dest)}, &e); code != 400 {
			t.Errorf("the app verifying the destination %s: %d", dest, code)
		}
	}
	if code := call(t, app, "POST", "/v1/backup/config", BackupConfigRequest{Destination: ptr("store:elsewhere")}, &s); code != 200 || s.Destination != "store:elsewhere" {
		t.Errorf("the app moving the destination within the operator's remotes: %d %+v", code, s)
	}
	if code := call(t, app, "POST", "/v1/backup/config", other(s3), &e); code != 403 {
		t.Errorf("the app replacing the operator's config: %d", code)
	}
	// Locked: verifying proposed settings is refused, the saved ones pass.
	configure(t, d, BackupConfigRequest{Lock: ptr(true)})
	if code := call(t, app, "POST", "/v1/backup/verify", BackupConfigRequest{Destination: ptr("store:x")}, &e); code != 403 || !strings.Contains(e.Error, "verify the saved ones") {
		t.Errorf("the app verifying a proposed destination, locked: %d %s", code, e.Error)
	}
	if j := verifyJob(t, app, nil); j.State != "done" {
		t.Errorf("the app verifying the saved settings, locked: %+v", j)
	}
}

// A saved rclone config whose digest is not the one the operator's last save kept (saved before
// the digest was, maybe by an app) is never run, on any path that runs rclone, until the operator
// saves one again.
func TestAnRcloneConfigNotKnownToBeTheOperatorsIsNotRun(t *testing.T) {
	t.Parallel()
	_, d, f, key := backupVM(t)
	h, app := d.Handler(), d.routes(true)
	if s := configure(t, d, validConfig(key)); s.RcloneConfigFrom != rcloneOperator {
		t.Fatalf("the operator's save: %+v", s)
	}
	if j := verifyJob(t, h, nil); j.State != "done" {
		t.Fatalf("the operator's config: %+v", j)
	}
	// As a release that kept no digest left it.
	if err := os.Remove(BackupRcloneFile(d.O.Dir)); err != nil {
		t.Fatal(err)
	}
	var st BackupStatus
	if call(t, h, "GET", "/v1/backup/status", nil, &st); st.Settings.RcloneConfigFrom != rcloneUnknown || !st.Settings.RcloneConfigSet {
		t.Errorf("status: %+v", st.Settings)
	}
	var s BackupSettings
	if call(t, app, "GET", "/v1/backup/config", nil, &s); s.RcloneConfigFrom != rcloneUnknown {
		t.Errorf("the app's settings: %+v", s)
	}
	var out bytes.Buffer
	printBackupSettings(&out, d.R.Ctl(), s)
	if !strings.Contains(out.String(), "not run until saved over SSH") {
		t.Errorf("printed %s", out.String())
	}
	before := len(f.calls())
	const refused = "not known to be the operator's"
	call(t, h, "POST", "/v1/backup", nil, nil)
	if j := waitJob(t, h); j.State != "failed" || !strings.Contains(j.Error, refused) {
		t.Errorf("a backup: %+v", j)
	}
	if j := verifyJob(t, h, nil); j.State != "failed" || !strings.Contains(j.Error, refused) {
		t.Errorf("verify of the saved settings: %+v", j)
	}
	if j := verifyJob(t, app, BackupConfigRequest{Destination: ptr("store:elsewhere")}); j.State != "failed" || !strings.Contains(j.Error, refused) {
		t.Errorf("the app verifying a destination in it: %+v", j)
	}
	var e errOf
	if code := call(t, h, "GET", "/v1/backup/list", nil, &e); code != 409 || !strings.Contains(e.Error, refused) {
		t.Errorf("list: %d %s", code, e.Error)
	}
	if n := len(f.calls()); n != before {
		t.Errorf("rclone ran: %v", f.calls()[before:])
	}
	// The operator trying one without saving it runs it; saving another field keeps the doubt.
	if j := verifyJob(t, h, validConfig(key)); j.State != "done" {
		t.Errorf("the operator's proposed config: %+v", j)
	}
	if s := configure(t, d, BackupConfigRequest{RetentionDays: ptr(3)}); s.RcloneConfigFrom != rcloneUnknown {
		t.Errorf("another setting saved: %+v", s)
	}
	// The digest kept, the config edited on disk.
	configure(t, d, validConfig(key))
	raw, _ := os.ReadFile(BackupFile(d.O.Dir))
	if err := os.WriteFile(BackupFile(d.O.Dir), bytes.Replace(raw, []byte("hush"), []byte("other"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if call(t, h, "GET", "/v1/backup/status", nil, &st); st.Settings.RcloneConfigFrom != rcloneUnknown {
		t.Errorf("edited on disk: %+v", st.Settings)
	}
	if s := configure(t, d, validConfig(key)); s.RcloneConfigFrom != rcloneOperator {
		t.Errorf("saved again: %+v", s)
	}
	call(t, h, "POST", "/v1/backup", nil, nil)
	if j := waitJob(t, h); j.State != "done" {
		t.Errorf("a backup, saved again: %+v", j)
	}
	if code := call(t, h, "DELETE", "/v1/backup/config", nil, nil); code != 200 {
		t.Fatalf("remove: %d", code)
	}
	if _, err := os.Stat(BackupRcloneFile(d.O.Dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the digest kept after a remove: %v", err)
	}
}

// Through the app socket the settings change as through the operator's, unless the operator locked
// them: then the app saves them unchanged alone, and the lock is the operator's. Each change the
// app makes is in the journal, and a request refused leaves the lock as it was.
func TestTheAppChangesTheBackupSettingsUnlessLocked(t *testing.T) {
	t.Parallel()
	_, d, _, key := backupVM(t)
	app := d.routes(true)
	other, _ := age.GenerateX25519Identity()
	var s BackupSettings
	var e errOf
	if s := configure(t, d, validConfig(key)); s.Locked {
		t.Fatalf("the operator's first settings: %+v", s)
	}
	if code := call(t, app, "POST", "/v1/backup/config", BackupConfigRequest{Destination: ptr("store:elsewhere")}, &s); code != 200 || s.Destination != "store:elsewhere" {
		t.Errorf("the app's change, unlocked: %d %+v", code, s)
	}
	if code := call(t, app, "POST", "/v1/backup/config", BackupConfigRequest{Lock: ptr(true)}, &e); code != 403 || !strings.Contains(e.Error, "lock is the operator's") {
		t.Errorf("the app locking: %d %s", code, e.Error)
	}
	// A request refused (by validation, or by systemd's calendar) sets no lock, nor drops one;
	// verify tries none.
	if code := call(t, d.Handler(), "POST", "/v1/backup/config", BackupConfigRequest{Lock: ptr(true), RetentionDays: ptr(-3)}, &e); code != 400 || d.settingsLocked() {
		t.Errorf("a refused lock: %d, locked %v", code, d.settingsLocked())
	}
	if code := call(t, d.Handler(), "POST", "/v1/backup/verify", BackupConfigRequest{Lock: ptr(true)}, &e); code != 400 || d.settingsLocked() {
		t.Errorf("verify with a lock: %d", code)
	}
	if s := configure(t, d, BackupConfigRequest{Lock: ptr(true)}); !s.Locked || s.Destination != "store:elsewhere" {
		t.Fatalf("the operator's lock: %+v", s)
	}
	if code := call(t, d.Handler(), "POST", "/v1/backup/config", BackupConfigRequest{Lock: ptr(false), RetentionDays: ptr(-3)}, &e); code != 400 || !d.settingsLocked() {
		t.Errorf("a refused unlock: %d, locked %v", code, d.settingsLocked())
	}
	// Locked: the whole form saved unchanged passes, its recipients in any order; every change is
	// refused, and so are an unlock and a removal.
	same := validConfig(key)
	same.Destination, same.RcloneConfig = ptr("store:elsewhere"), nil // a console's form: GET shows no config
	if code := call(t, app, "POST", "/v1/backup/config", same, &s); code != 200 || !s.Locked {
		t.Errorf("the settings saved unchanged, locked: %d %+v", code, s)
	}
	for _, req := range []BackupConfigRequest{
		{Recipients: []string{other.Recipient().String()}},
		{Destination: ptr("store:backups/notes")},
		{RetentionDays: ptr(1)},
		{Schedule: ptr("monthly")},
		{Lock: ptr(false)},
	} {
		if code := call(t, app, "POST", "/v1/backup/config", req, &e); code != 403 {
			t.Errorf("%+v through the app, locked: %d", req, code)
		}
	}
	if code := call(t, app, "DELETE", "/v1/backup/config", nil, &e); code != 403 {
		t.Errorf("the app removing the settings: %d", code)
	}
	if s := configure(t, d, BackupConfigRequest{Recipients: []string{other.Recipient().String(), key.Recipient().String()}}); len(s.Recipients) != 2 || !s.Locked {
		t.Errorf("the operator's change, locked: %+v", s)
	}
	reordered := same
	reordered.Recipients = []string{key.Recipient().String(), other.Recipient().String()}
	if code := call(t, app, "POST", "/v1/backup/config", reordered, &s); code != 200 {
		t.Errorf("the recipients reordered: %d", code)
	}
	// The lock outlives the settings: the app cannot set them again.
	if code := call(t, d.Handler(), "DELETE", "/v1/backup/config", nil, &s); code != 200 || !s.Locked {
		t.Errorf("the operator's removal: %d %+v", code, s)
	}
	if code := call(t, app, "POST", "/v1/backup/config", validConfig(key), &e); code != 403 {
		t.Errorf("the app's settings after a removal, locked: %d", code)
	}
	// A lock that cannot be read is one.
	if err := os.Remove(BackupLockFile(d.O.Dir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(BackupLockFile(d.O.Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(BackupLockFile(d.O.Dir), "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !d.settingsLocked() {
		t.Error("an odd lock file is no lock")
	}
	if err := os.RemoveAll(BackupLockFile(d.O.Dir)); err != nil {
		t.Fatal(err)
	}
	configure(t, d, validConfig(key))
	if code := call(t, app, "POST", "/v1/backup/config", BackupConfigRequest{Destination: ptr("store:again")}, &s); code != 200 || s.Locked {
		t.Errorf("the app's change, unlocked: %d %+v", code, s)
	}
	var a EventsAnswer
	if code := call(t, d.Handler(), "GET", "/v1/events?after=0", nil, &a); code != 200 {
		t.Fatalf("events: %d", code)
	}
	var reasons []string
	for _, ev := range a.Events {
		if ev.Kind == evBackupConfig {
			reasons = append(reasons, ev.Reason)
		}
	}
	if want := []string{"changed through the app socket: destination", "changed through the app socket: destination"}; !slices.Equal(reasons, want) {
		t.Errorf("events %q", reasons)
	}
}
