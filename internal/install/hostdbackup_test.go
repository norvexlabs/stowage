package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

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
				list = append(list, entry{filepath.Base(k), n, time.Date(2026, 10, 10, 3, 0, 0, 0, time.UTC)})
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
	var started struct{ Job, Target string }
	if code := call(t, h, "POST", "/v1/backup", BackupRequest{IfConfigured: true}, &started); code != 202 || started.Job != "backup" {
		t.Fatalf("%d %+v", code, started)
	}
	j := waitJob(t, h)
	if j.State != "done" || j.Target != started.Target {
		t.Fatalf("%+v", j)
	}
	if len(f.objects) != 1 {
		t.Fatalf("objects %v", f.objects)
	}
	var obj string
	for k := range f.objects {
		obj = k
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
	if st.Last == nil || st.Last.Object != started.Target || st.Last.Error != "" || st.Last.Bytes != int64(len(f.objects[obj])) || st.Last.Release != (Version{"1.0.0", 1}) || st.Running {
		t.Errorf("%+v", st.Last)
	}
	// No retention, no pruning.
	configure(t, d, BackupConfigRequest{RetentionDays: ptr(0)})
	f.ran = nil
	call(t, h, "POST", "/v1/backup", nil, nil)
	if j := waitJob(t, h); j.State != "done" || strings.Contains(fmt.Sprint(f.calls()), "[delete ") {
		t.Errorf("%+v %v", j, f.calls())
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
			case "the dump fails", "the upload fails", "the size is short":
				if len(f.objects) != 0 || !strings.Contains(fmt.Sprint(f.calls()), "[deletefile store:backups/notes/notes-full-") {
					t.Errorf("the cut backup was kept: %v", f.calls())
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
	if j.State != "done" || !strings.Contains(log, "a backup first: notes-full-") || !strings.Contains(log, "backed up: ") || len(f.objects) != 1 {
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
	if code := call(t, h, "POST", "/v1/backup", nil, &e409); code != 409 || !strings.Contains(e409.Error, "backup-verify is running") {
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
	if code := call(t, h, "POST", "/v1/backup", BackupRequest{IfConfigured: true}, &e409); code != 409 || !strings.Contains(e409.Error, "backup-verify is running") {
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
