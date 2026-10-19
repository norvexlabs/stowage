package install

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/spec"
)

// restoreVM is backupVM configured, with one backup taken: its object's name.
func restoreVM(t *testing.T) (*env, *Hostd, *fakeRclone, *age.X25519Identity, string) {
	t.Helper()
	e, d, f, key := backupVM(t)
	configure(t, d, validConfig(key))
	e.docker.answer = func(a string) ([]byte, bool, error) {
		return []byte("api\nweb\npostgres\n"), strings.HasSuffix(a, "config --services"), nil
	}
	h := d.Handler()
	call(t, h, "POST", "/v1/backup", BackupRequest{}, nil)
	j := waitJob(t, h)
	if j.State != "done" {
		t.Fatalf("%+v", j)
	}
	return e, d, f, key, j.Target
}

// composeCalls is the compose verbs e ran from call n on, as their arguments after the project's.
func composeCalls(e *env, n int) []string {
	var out []string
	for _, c := range e.docker.ran("compose")[n:] {
		a := strings.Join(c.Args, " ")
		switch {
		case strings.Contains(a, " stop "):
			out = append(out, "stop")
		case strings.Contains(a, " exec -T postgres sh -c ") && strings.Contains(a, "pg_restore"):
			out = append(out, "restore")
		case strings.Contains(a, " up --detach "):
			out = append(out, "up --detach")
		}
	}
	return out
}

func TestRestoreLoadsABackupOfTheReleaseTheInstallRuns(t *testing.T) {
	t.Parallel()
	e, d, _, key, obj := restoreVM(t)
	h := d.Handler()
	n := len(e.docker.ran("compose"))
	v := &Version{"1.0.0", 1}
	var started JobStarted
	if code := call(t, h, "POST", "/v1/restore", RestoreRequest{Object: obj, Identity: key.String(), Confirm: v}, &started); code != 202 || started.Job != "restore" || started.Target != obj {
		t.Fatalf("%d %v", code, started)
	}
	j := waitJob(t, h)
	if j.State != "done" || j.Kind != "restore" || !j.Started.Equal(started.Started) {
		t.Fatalf("%+v", j)
	}
	if e.docker.restored != "the database's dump" {
		t.Errorf("pg_restore read %q", e.docker.restored)
	}
	if got := strings.Join(composeCalls(e, n), ", "); got != "stop, restore, up --detach" {
		t.Errorf("ran %s", got)
	}
	var stop string
	var load, swap []string
	for _, c := range e.docker.ran("compose")[n:] {
		switch a := strings.Join(c.Args, " "); {
		case strings.Contains(a, " stop "):
			stop = a
		case strings.Contains(a, "pg_restore"):
			load = c.Args
		case strings.Contains(a, "RENAME TO stowage_replaced"):
			swap = c.Args
		}
	}
	if !strings.HasSuffix(stop, "stop --timeout 30 api web") {
		t.Errorf("stopped %s", stop)
	}
	// The load, then the swap, each as the migration role through the maintenance database.
	if k := len(load); k < 5 || load[k-5] != loadScript || strings.Join(load[k-4:], " ") != "restore notes notes postgres" {
		t.Errorf("loaded with %q", load)
	}
	if k := len(swap); k < 5 || swap[k-5] != swapScript || strings.Join(swap[k-4:], " ") != "swap notes notes postgres" {
		t.Errorf("swapped with %q", swap)
	}
	for _, want := range []string{
		"CREATE DATABASE stowage_restore OWNER %I TEMPLATE template0",
		`pg_restore -U "$u" -d stowage_restore --single-transaction --exit-on-error`,
	} {
		if !strings.Contains(loadScript, want) {
			t.Errorf("the load lacks %q", want)
		}
	}
	// Postgres bounds the swap in the container, so nothing of it outlives the job's wait for it.
	// Every psql of each script goes through its bounded helper or carries the bounds itself.
	for sc, bound := range map[string]string{
		swapScript:   "PGCONNECT_TIMEOUT=5 PGOPTIONS='-c lock_timeout=15s -c statement_timeout=20s' psql",
		dropScript:   "PGCONNECT_TIMEOUT=5 PGOPTIONS='-c lock_timeout=30s -c statement_timeout=8min' psql",
		swappedCheck: "PGCONNECT_TIMEOUT=5 PGOPTIONS='-c statement_timeout=10s' psql",
	} {
		if strings.Count(sc, "psql ") != 1 || !strings.Contains(sc, bound) {
			t.Errorf("not every psql is bounded by %q:\n%s", bound, sc)
		}
	}
	if strings.Count(swapScript, "| sql") != 1 || strings.Count(swapScript, "sql <<'SQL'") != 1 {
		t.Error("the swap runs psql other than through its bounded helper")
	}
	if !strings.Contains(swapScript, "BEGIN;\nALTER DATABASE :\"db\" RENAME TO stowage_replaced;\nALTER DATABASE stowage_restore RENAME TO :\"db\";\nCOMMIT;") {
		t.Error("the swap is not one transaction of two renames")
	}
	// The load never touches the install's database; nothing drops owners or object by object.
	if strings.Contains(loadScript, `:"db"`) || strings.Contains(loadScript, "RENAME") {
		t.Error("the load names the install's database in a statement")
	}
	for _, sc := range []string{loadScript, swapScript} {
		if strings.Contains(sc, "--no-owner") || strings.Contains(sc, "--clean") || strings.Contains(sc, "DROP SCHEMA") {
			t.Error("a restore script drops owners, or drops object by object")
		}
	}
	if maintenanceDB("postgres") != "template1" || maintenanceDB("notes") != "postgres" {
		t.Error("the maintenance database is the one renamed")
	}
	// The provision hook ran on the restored data, after the stack came up.
	if !strings.Contains(strings.Join(j.Log, "\n"), "provisioning notes on the restored data") {
		t.Errorf("log: %s", strings.Join(j.Log, "\n"))
	}
	// The identity is never logged.
	if strings.Contains(strings.Join(j.Log, "\n"), key.String()) {
		t.Error("the identity is in the job's log")
	}
}

func TestARestoreThatFailsBringsTheStackUpAgain(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		wrongKey   bool
		restoreErr error
		want       string
		swapErr    error
	}{
		"another key": {true, nil, "is the identity one of the backup's recipients?", nil},
		// The load writes only its own database: any end of it leaves the install's as it was.
		"not loaded":     {false, exitCode(3), "the database is as it was)", nil},
		"cut short":      {false, errors.New("signal: killed"), "signal: killed (the database is as it was)", nil},
		"not swapped":    {false, nil, "exit status 4 (the database is as it was)", exitCode(4)},
		"swap cut short": {false, nil, "signal: killed (the database is as it was)", errors.New("signal: killed")},
	} {
		e, d, _, key, obj := restoreVM(t)
		if err := os.WriteFile(filepath.Join(e.dir, restoreAwaitedFile), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		e.docker.restoreErr = c.restoreErr
		if c.swapErr != nil {
			prev := e.docker.answer
			e.docker.answer = func(a string) ([]byte, bool, error) {
				if strings.Contains(a, "RENAME TO stowage_replaced") {
					return nil, true, c.swapErr
				}
				return prev(a)
			}
		}
		id := key.String()
		if c.wrongKey {
			other, _ := age.GenerateX25519Identity()
			id = other.String()
		}
		h := d.Handler()
		n := len(e.docker.ran("compose"))
		call(t, h, "POST", "/v1/restore", RestoreRequest{Object: obj, Identity: id, Confirm: &Version{"1.0.0", 1}}, nil)
		j := waitJob(t, h)
		if j.State != "failed" || !strings.Contains(j.Error, c.want) {
			t.Errorf("%s: %+v", name, j)
		}
		// A failed restore never ends the wait a --secrets install began: the data did not change.
		if _, err := os.Stat(filepath.Join(e.dir, restoreAwaitedFile)); err != nil {
			t.Errorf("%s: the marker went with a restore that changed nothing", name)
		}
		if calls := composeCalls(e, n); len(calls) == 0 || calls[len(calls)-1] != "up --detach" {
			t.Errorf("%s: the stack is not brought up again: %v", name, calls)
		}
	}
}

func TestARestoreOfAStackOfItsDatabaseAloneStopsNothing(t *testing.T) {
	t.Parallel()
	e, d, _, key, obj := restoreVM(t)
	e.docker.answer = func(a string) ([]byte, bool, error) {
		return []byte("postgres\n"), strings.HasSuffix(a, "config --services"), nil
	}
	h := d.Handler()
	n := len(e.docker.ran("compose"))
	call(t, h, "POST", "/v1/restore", RestoreRequest{Object: obj, Identity: key.String(), Confirm: &Version{"1.0.0", 1}}, nil)
	if j := waitJob(t, h); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	// `stop` naming no service would stop the database too.
	if got := strings.Join(composeCalls(e, n), ", "); got != "restore, up --detach" {
		t.Errorf("ran %s", got)
	}
}

func TestARestoreReadsTheInstallAgainUnderTheLock(t *testing.T) {
	t.Parallel()
	e, d, _, key, obj := restoreVM(t)
	in, _, err := d.runnerCall(nil)
	if err != nil {
		t.Fatal(err)
	}
	// A run before the lock changed the install: the job reads it again, and refuses.
	changed := e.readEnv()
	changed.Set("STOWAGE_PROJECT_VERSION", "0.9.0")
	if err := changed.WriteFile(filepath.Join(e.dir, ".env")); err != nil {
		t.Fatal(err)
	}
	err = d.runRestore(in, obj, []age.Identity{key}, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "the install runs notes 0.9.0 (revision 1)") || e.docker.restored != "" {
		t.Errorf("%v, restored %q", err, e.docker.restored)
	}
}

func TestRestoreRefusals(t *testing.T) {
	t.Parallel()
	e, d, _, key, obj := restoreVM(t)
	h := d.Handler()
	v := &Version{"1.0.0", 1}
	for name, c := range map[string]struct {
		req  RestoreRequest
		code int
		want string
	}{
		"unconfirmed":      {RestoreRequest{Object: obj, Identity: key.String()}, 400, "confirm the release it runs"},
		"another release":  {RestoreRequest{Object: obj, Identity: key.String(), Confirm: &Version{"0.9.0", 1}}, 409, "not 0.9.0 (revision 1), which was confirmed"},
		"an older backup":  {RestoreRequest{Object: "notes-full-20261009T030000Z-0.9.0-r1.pgdump.age", Identity: key.String(), Confirm: v}, 409, "a backup restores onto the release it was taken from"},
		"another project":  {RestoreRequest{Object: "crm-full-20261009T030000Z-1.0.0-r1.pgdump.age", Identity: key.String(), Confirm: v}, 400, "is not a backup of notes"},
		"a path":           {RestoreRequest{Object: "../notes-full-20261009T030000Z-1.0.0-r1.pgdump.age", Identity: key.String(), Confirm: v}, 400, "is not a backup of notes"},
		"not an identity":  {RestoreRequest{Object: obj, Identity: "AGE-SECRET-KEY-NOPE", Confirm: v}, 400, "is not an age identity"},
		"an unknown field": {RestoreRequest{}, 400, ""},
	} {
		var body any = c.req
		if name == "an unknown field" {
			body = map[string]string{"objet": obj}
		}
		var got errOf
		if code := call(t, h, "POST", "/v1/restore", body, &got); code != c.code || !strings.Contains(got.Error, c.want) {
			t.Errorf("%s: %d %s", name, code, got.Error)
		}
		if strings.Contains(got.Error, key.String()) {
			t.Errorf("%s: the identity is in the answer", name)
		}
	}
	// A release without a database has no backup to restore, nor a database to reset.
	sp := *d.R.Spec
	sp.Database = nil
	r := *d.R
	r.Spec = &sp
	d.R = &r
	var got errOf
	if code := call(t, h, "POST", "/v1/restore", RestoreRequest{Object: obj, Identity: key.String(), Confirm: v}, &got); code != 409 || !strings.Contains(got.Error, "notes 1.0.0 has no database: there is no backup to restore") {
		t.Errorf("no database: %d %s", code, got.Error)
	}
	if code := call(t, h, "GET", "/v1/reset-db", nil, &got); code != 409 || !strings.Contains(got.Error, "has no database: reset-db has nothing to reset") {
		t.Errorf("reset-db, no database: %d %s", code, got.Error)
	}
	if e.docker.restored != "" {
		t.Error("a refused restore ran")
	}
}

func TestRestoreRefusesAProfileLeavingRowsOut(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	e.r, e.self = bundledInstaller(t, "1.0.0", 1, func(s *spec.Spec) {
		s.Backup.Config = map[string]any{"default_profile": "full", "profiles": map[string]any{"full": "all", "config": []any{"users"},
			"lean": map[string]any{"exclude": []any{"events"}}}}
	})
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	e.docker.runner = (&fakeRclone{objects: map[string][]byte{}}).run
	key, _ := age.GenerateX25519Identity()
	d := daemon(e, &fakeHost{})
	configure(t, d, validConfig(key))
	for _, p := range []string{"config", "lean", "gone"} {
		var got errOf
		obj := "notes-" + p + "-20261009T030000Z-1.0.0-r1.pgdump.age"
		if code := call(t, d.Handler(), "POST", "/v1/restore", RestoreRequest{Object: obj, Identity: key.String(), Confirm: &Version{"1.0.0", 1}}, &got); code != 409 ||
			!strings.Contains(got.Error, "does not keep every table's rows: restoring it would empty the others (restore a backup of a profile `all`: full)") {
			t.Errorf("%s: %d %s", p, code, got.Error)
		}
	}
}

func TestTheDaemonExportsTheSecretsEncrypted(t *testing.T) {
	t.Parallel()
	e, d, _, key := backupVM(t)
	var got SecretsExport
	if code := call(t, d.Handler(), "POST", "/v1/secrets/export", SecretsExportRequest{Recipients: []string{key.Recipient().String()}}, &got); code != 200 {
		t.Fatalf("%d %+v", code, got)
	}
	var f SecretsFile
	if err := yaml.Unmarshal(decrypt(t, []byte(got.File), key), &f); err != nil {
		t.Fatal(err)
	}
	if f.Project != "notes" || f.Secrets["SESSION_SECRET"] != get(e.readEnv(), "SESSION_SECRET") || f.Secrets[HostTokenVar] == "" {
		t.Errorf("%+v", f)
	}
	var e400 errOf
	if code := call(t, d.Handler(), "POST", "/v1/secrets/export", SecretsExportRequest{}, &e400); code != 400 || !strings.Contains(e400.Error, "recipients: 1 to 16") {
		t.Errorf("no recipient: %d %s", code, e400.Error)
	}
	// An OS of another release reads another spec: its list of secrets is not the install's.
	d.R, _ = bundledInstaller(t, "2.0.0", 1, nil)
	var e409 errOf
	if code := call(t, d.Handler(), "POST", "/v1/secrets/export", SecretsExportRequest{Recipients: []string{key.Recipient().String()}}, &e409); code != 409 ||
		!strings.Contains(e409.Error, "secrets export runs on the release the OS carries") {
		t.Errorf("another release: %d %s", code, e409.Error)
	}
}

func TestARestoreProvisionsTheRestoredDataAndEndsTheWait(t *testing.T) {
	t.Parallel()
	e, d, _, key, obj := restoreVM(t)
	if err := os.WriteFile(filepath.Join(e.dir, restoreAwaitedFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h := d.Handler()
	var st Status
	call(t, h, "GET", "/v1/status", nil, &st)
	if !st.RestoreAwaited {
		t.Fatalf("%+v", st)
	}
	hooks := len(e.hookCalls())
	call(t, h, "POST", "/v1/restore", RestoreRequest{Object: obj, Identity: key.String(), Confirm: &Version{"1.0.0", 1}}, nil)
	if j := waitJob(t, h); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if n := len(e.hookCalls()); n != hooks+1 {
		t.Fatalf("%d hook runs after the restore, want 1", n-hooks)
	}
	st = Status{}
	call(t, h, "GET", "/v1/status", nil, &st)
	if st.RestoreAwaited {
		t.Fatal("status still awaits a restore")
	}
	// A hook that fails on the restored data fails the job, naming provision; the data stays, and
	// the wait is over: the data is loaded.
	if err := os.WriteFile(filepath.Join(e.dir, restoreAwaitedFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e.docker.hookErr = errors.New("exit status 1")
	call(t, h, "POST", "/v1/restore", RestoreRequest{Object: obj, Identity: key.String(), Confirm: &Version{"1.0.0", 1}}, nil)
	if j := waitJob(t, h); j.State != "failed" || !strings.Contains(j.Error, "restored "+obj+", but the provision hook failed") || !strings.Contains(j.Error, "`sudo notes-stow provision`") {
		t.Fatalf("%+v", j)
	}
	if _, err := os.Stat(filepath.Join(e.dir, restoreAwaitedFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("status still awaits the backup the restore loaded")
	}
}

func TestARestoreRefusesBeforeTouchingTheDataWhileAnEarlierRunsContainerIsThere(t *testing.T) {
	t.Parallel()
	e, d, _, key, obj := restoreVM(t)
	prev := e.docker.answer
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if strings.HasPrefix(a, "container inspect --format {{.State.Status}} ") && strings.HasSuffix(a, "-provision") {
			return []byte("exited\n"), true, nil
		}
		return prev(a)
	}
	h := d.Handler()
	n := len(e.docker.ran("compose"))
	call(t, h, "POST", "/v1/restore", RestoreRequest{Object: obj, Identity: key.String(), Confirm: &Version{"1.0.0", 1}}, nil)
	if j := waitJob(t, h); j.State != "failed" || !strings.Contains(j.Error, "of an earlier run is still there") {
		t.Fatalf("%+v", j)
	}
	if got := composeCalls(e, n); len(got) != 0 || e.docker.restored != "" {
		t.Errorf("ran %v, restored %q", got, e.docker.restored)
	}
}

func TestASwapThatEndedBadlyButCommittedIsARestore(t *testing.T) {
	t.Parallel()
	// Cut short, or exit 4 after a COMMIT the client did not see: stowage_replaced tells.
	for _, swapErr := range []error{errors.New("signal: killed"), exitCode(4)} {
		swapCommitted(t, swapErr)
	}
}

func swapCommitted(t *testing.T, swapErr error) {
	t.Helper()
	e, d, _, key, obj := restoreVM(t)
	if err := os.WriteFile(filepath.Join(e.dir, restoreAwaitedFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	prev := e.docker.answer
	e.docker.answer = func(a string) ([]byte, bool, error) {
		switch {
		case strings.Contains(a, "RENAME TO stowage_replaced"):
			return nil, true, swapErr
		case strings.Contains(a, "SELECT count(*) FROM pg_database WHERE datname = 'stowage_replaced'"):
			return []byte("1\n"), true, nil
		}
		return prev(a)
	}
	h := d.Handler()
	hooks := len(e.hookCalls())
	call(t, h, "POST", "/v1/restore", RestoreRequest{Object: obj, Identity: key.String(), Confirm: &Version{"1.0.0", 1}}, nil)
	j := waitJob(t, h)
	if j.State != "done" || !strings.Contains(strings.Join(j.Log, "\n"), "but committed: the backup is in place") {
		t.Fatalf("%+v", j)
	}
	if _, err := os.Stat(filepath.Join(e.dir, restoreAwaitedFile)); !errors.Is(err, os.ErrNotExist) || len(e.hookCalls()) != hooks+1 {
		t.Fatalf("marker %v, %d hook runs", err, len(e.hookCalls())-hooks)
	}
}

func TestASwapWhoseOutcomeCannotBeReadNamesProvision(t *testing.T) {
	t.Parallel()
	e, d, _, key, obj := restoreVM(t)
	prev := e.docker.answer
	e.docker.answer = func(a string) ([]byte, bool, error) {
		switch {
		case strings.Contains(a, "RENAME TO stowage_replaced"):
			return nil, true, errors.New("signal: killed")
		case strings.Contains(a, "datname = 'stowage_replaced'\""):
			return nil, true, errors.New("exit status 2")
		}
		return prev(a)
	}
	h := d.Handler()
	call(t, h, "POST", "/v1/restore", RestoreRequest{Object: obj, Identity: key.String(), Confirm: &Version{"1.0.0", 1}}, nil)
	if j := waitJob(t, h); j.State != "failed" || !strings.Contains(j.Error, "could not be read") || !strings.Contains(j.Error, "stowage_replaced") || !strings.Contains(j.Error, "`sudo notes-stow provision`") {
		t.Fatalf("%+v", j)
	}
}

func TestADropThatFailsAfterTheSwapIsStillARestore(t *testing.T) {
	t.Parallel()
	e, d, _, key, obj := restoreVM(t)
	prev := e.docker.answer
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if strings.Contains(a, "'DROP DATABASE IF EXISTS stowage_replaced WITH (FORCE);' |") {
			return nil, true, errors.New("exit status 1")
		}
		return prev(a)
	}
	h := d.Handler()
	hooks := len(e.hookCalls())
	call(t, h, "POST", "/v1/restore", RestoreRequest{Object: obj, Identity: key.String(), Confirm: &Version{"1.0.0", 1}}, nil)
	j := waitJob(t, h)
	if j.State != "done" || !strings.Contains(strings.Join(j.Log, "\n"), "the next restore drops stowage_replaced") || len(e.hookCalls()) != hooks+1 {
		t.Fatalf("%+v", j)
	}
	// The drop runs once the stack is up again, never holding it back.
	drop, up := -1, -1
	for i, c := range e.docker.ran("compose") {
		a := strings.Join(c.Args, " ")
		if strings.Contains(a, "'DROP DATABASE IF EXISTS stowage_replaced WITH (FORCE);' |") {
			drop = i
		}
		if strings.Contains(a, " up --detach ") {
			up = i
		}
	}
	if drop < up {
		t.Errorf("the drop ran at call %d, the stack up at %d", drop, up)
	}
}
