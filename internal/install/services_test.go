package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/spec"
)

// servicesEnv is an install whose stack's services are api, postgres and the front.
func servicesEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if strings.HasSuffix(a, " config --services") {
			return []byte("api\npostgres\nstowage-front\n"), true, nil
		}
		return nil, false, nil
	}
	return e
}

// lastCall is the last command the fake engine ran, after the compose files.
func (e *env) lastCall() string {
	c := e.docker.calls[len(e.docker.calls)-1].Args
	for i := len(c) - 1; i >= 0; i-- {
		if c[i] == "-f" {
			return strings.Join(c[i+2:], " ")
		}
	}
	return strings.Join(c, " ")
}

func TestServiceVerbsOnDocker(t *testing.T) {
	t.Parallel()
	e := servicesEnv(t)
	in := e.installer(nil)
	ctx := context.Background()
	if err := in.Ps(ctx, false); err != nil || e.lastCall() != "ps --all" {
		t.Fatalf("ps: %v %q", err, e.lastCall())
	}
	if err := in.Ps(ctx, true); err != nil || e.lastCall() != "ps --all --format json" {
		t.Fatalf("ps --json: %v %q", err, e.lastCall())
	}
	for _, c := range []struct {
		tail   int
		follow bool
		want   string
	}{{1, false, "logs --no-color --tail 1 api"}, {maxLogTail, true, "logs --no-color --tail 5000 --follow api"}} {
		if err := in.Logs(ctx, "api", c.tail, c.follow); err != nil || e.lastCall() != c.want {
			t.Errorf("logs --tail %d: %v %q", c.tail, err, e.lastCall())
		}
	}
	for _, tail := range []int{0, maxLogTail + 1} {
		if err := in.Logs(ctx, "api", tail, false); err == nil || !strings.Contains(err.Error(), "1 to 5000") {
			t.Errorf("logs --tail %d: %v", tail, err)
		}
	}
	if err := in.Logs(ctx, "nope", 10, false); err == nil || err.Error() != `"nope" is no service of the stack (api, postgres, stowage-front)` {
		t.Errorf("an unknown service: %v", err)
	}
	for _, verb := range ServiceVerbs {
		if err := in.Service(ctx, verb, "stowage-front"); err != nil || e.lastCall() != verb+" stowage-front" {
			t.Errorf("%s: %v %q", verb, err, e.lastCall())
		}
	}
	if err := in.Service(ctx, "kill", "api"); err == nil || !strings.Contains(err.Error(), `unknown verb "kill"`) {
		t.Errorf("kill: %v", err)
	}
	// Never during an upgrade, a rollback, a provision or a reset-db.
	unlock, err := lockFile(filepath.Join(e.dir, lifecycleLock))
	if err != nil {
		t.Fatal(err)
	}
	n := len(e.docker.calls)
	if err := in.Service(ctx, "restart", "api"); err == nil || !strings.Contains(err.Error(), "already running for this install") || strings.Contains(e.lastCall(), "restart") {
		t.Errorf("restart under the lock: %v (%d calls before, %q)", err, n, e.lastCall())
	}
	unlock()
}

func TestExecOnDocker(t *testing.T) {
	t.Parallel()
	e := servicesEnv(t)
	in := e.installer(nil)
	ctx := context.Background()
	if err := in.Exec(ctx, "postgres", nil, nil, false); err == nil || !strings.Contains(err.Error(), "give the command after --") {
		t.Fatalf("no command: %v", err)
	}
	if err := in.Exec(ctx, "postgres", []string{"pg_isready", "-U", "notes"}, nil, false); err != nil || e.lastCall() != "exec -T postgres pg_isready -U notes" {
		t.Fatalf("exec: %v %q", err, e.lastCall())
	}
	if err := in.Exec(ctx, "postgres", []string{"sh"}, nil, true); err != nil || e.lastCall() != "exec postgres sh" {
		t.Fatalf("exec on a terminal: %v %q", err, e.lastCall())
	}
	answer := e.docker.answer
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if strings.HasSuffix(a, " exec -T postgres false") {
			return nil, true, exitCode(3)
		}
		return answer(a)
	}
	var code ExitCode
	if err := in.Exec(ctx, "postgres", []string{"false"}, nil, false); !errors.As(err, &code) || code != 3 {
		t.Fatalf("the command's exit status: %v", err)
	}
}

func TestSchemaOnDocker(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.r, e.self = testInstallerWith(t, "1.0.0", func(s *spec.Spec) {
		s.Database.SchemaCompat = spec.Provider{Name: "sql", Config: map[string]any{
			"current":       "SELECT max(version) FROM migrations",
			"barrier_after": "SELECT count(*) > 0 FROM migrations WHERE version > $1 AND barrier"}}
	})
	var asked []string
	e.docker.psql = func(q string) (string, error) {
		asked = append(asked, q)
		if strings.HasPrefix(q, "SELECT max") {
			return "12", nil
		}
		if strings.Contains(q, "version > 10 ") {
			return "t", nil
		}
		return "f", nil
	}
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	in := e.installer(nil)
	ctx := context.Background()
	e.out.Reset()
	if err := in.Schema(ctx, "", false); err != nil || e.out.String() != "migration  12\n" {
		t.Fatalf("no release kept: %v %q", err, e.out)
	}
	e.out.Reset()
	if err := in.Schema(ctx, "11", false); err != nil || e.out.String() != "migration  12\nbarriers   after 11: none\n" {
		t.Fatalf("--after 11: %v %q", err, e.out)
	}
	for _, bad := range []string{"011", "x", "1234567890123456"} {
		if err := in.Schema(ctx, bad, false); err == nil || !strings.Contains(err.Error(), "a migration number") {
			t.Errorf("--after %s: %v", bad, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(e.dir, PrevDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, PrevDir, prevState), []byte(`{"project_version": "0.9.0", "revision": 2, "schema": "10"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e.out.Reset()
	if err := in.Schema(ctx, "", false); err != nil || e.out.String() !=
		"migration  12\nkept       0.9.0 (revision 2), at migration 10 before the upgrade: `rollback` goes back to it\nbarriers   after 10: a rollback barrier after migration 10\n" {
		t.Fatalf("a release kept: %v %q", err, e.out)
	}
	e.out.Reset()
	if err := in.Schema(ctx, "", true); err != nil || strings.TrimSpace(e.out.String()) !=
		`{"migration":"12","kept":{"project_version":"0.9.0","revision":2,"schema":"10"},"after":"10","barriers":"a rollback barrier after migration 10"}` {
		t.Fatalf("--json: %v %q", err, e.out)
	}
	none := newEnv(t)
	if err := none.install(nil); err != nil {
		t.Fatal(err)
	}
	none.r.Spec.Database.SchemaCompat = spec.Provider{Name: "none"}
	if err := none.installer(nil).Schema(ctx, "", false); err == nil || !strings.Contains(err.Error(), "checks no schema") {
		t.Errorf("without schema_compat: %v", err)
	}
}

func TestUninstallOnDocker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	down := func(e *env) string {
		for _, c := range e.docker.calls {
			if a := strings.Join(c.Args, " "); strings.Contains(a, " down ") {
				return a[strings.Index(a, " down ")+1:]
			}
		}
		return ""
	}
	e := servicesEnv(t)
	if err := e.installer(nil).Uninstall(ctx, false); err == nil || !strings.Contains(err.Error(), "give --yes") || down(e) != "" {
		t.Fatalf("no terminal, no --yes: %v", err)
	}
	if err := e.installer(func(o *Options) { o.Prompt = &fakePrompter{answers: map[string]string{"confirm": "no"}} }).Uninstall(ctx, false); !errors.Is(err, ErrAborted) || down(e) != "" {
		t.Fatalf("answered no: %v", err)
	}
	if err := e.installer(func(o *Options) { o.Yes, o.DryRun = true, true }).Uninstall(ctx, true); err != nil || down(e) != "" {
		t.Fatalf("--dry-run: %v", err)
	}
	// The install's timers on the host, as `backup config` and the disk-pressure watch write them.
	units, host := t.TempDir(), &fakeHost{}
	for _, kind := range []string{"backup", "disk"} {
		for _, ext := range []string{".service", ".timer"} {
			if err := os.WriteFile(filepath.Join(units, installUnit("notes", kind, e.dir)+ext), []byte("[Unit]\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := e.installer(func(o *Options) { o.Yes, o.Systemd, o.Host = true, units, host }).Uninstall(ctx, false); err != nil || down(e) != "down --remove-orphans" {
		t.Fatalf("--yes: %v %q", err, down(e))
	}
	if left, _ := os.ReadDir(units); len(left) != 0 || !strings.Contains(fmt.Sprint(host.calls()), "systemctl disable --now "+installUnit("notes", "disk", e.dir)+".timer") {
		t.Fatalf("the timers: %v left, ran %v", left, host.calls())
	}
	if _, err := os.Stat(filepath.Join(e.dir, ".env")); err != nil || !strings.Contains(e.out.String(), "kept: its volumes and "+e.dir) {
		t.Fatalf("the data kept: %v\n%s", err, e.out)
	}
	e.docker.calls = nil
	p := &fakePrompter{answers: map[string]string{"confirm": "delete"}}
	if err := e.installer(func(o *Options) { o.Prompt = p }).Uninstall(ctx, false); err != nil || !strings.HasSuffix(down(e), "--volumes") {
		t.Fatalf("answered delete: %v %q", err, down(e))
	}
	if _, err := os.Stat(e.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the install directory is left: %v", err)
	}
	if len(p.forms) != 1 || len(p.forms[0].Questions[0].Choices) != 3 {
		t.Errorf("the question: %+v", p.forms)
	}
	if err := e.installer(func(o *Options) { o.Yes = true }).Uninstall(ctx, false); err == nil || !strings.Contains(err.Error(), "holds no install") {
		t.Errorf("uninstalled already: %v", err)
	}
}

// An uninstall removes what the operator's consent applied (tune --apply), and refuses without root.
func TestUninstallOnDockerRevertsTheGateway(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := servicesEnv(t)
	e.r.Nftables = map[string][]byte{"vpn": []byte("table inet vpn {\n}\n")}
	if err := os.WriteFile(filepath.Join(e.dir, GatewayFile), []byte(`{"digest":"00000000","dir":"`+e.dir+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	units, host := t.TempDir(), &gwHost{tables: map[string]bool{}}
	opts := func(euid int) func(o *Options) {
		return func(o *Options) {
			o.Yes, o.Systemd, o.Host, o.GatewayRun, o.Euid = true, units, host, t.TempDir(), func() int { return euid }
		}
	}
	if err := e.installer(opts(1000)).Uninstall(ctx, false); err == nil || !strings.Contains(err.Error(), "removing them takes root") {
		t.Fatalf("not root: %v", err)
	}
	if err := e.installer(opts(0)).Uninstall(ctx, false); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(e.dir, GatewayFile)) {
		t.Error("the consent outlived the uninstall")
	}
}

// An uninstall refuses a gateway record that does not read before anything goes, and leaves one
// of another directory as it is.
func TestUninstallOnDockerReadsTheGatewaysRecordFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := servicesEnv(t)
	if err := os.WriteFile(filepath.Join(e.dir, GatewayFile), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.installer(func(o *Options) { o.Yes = true }).Uninstall(ctx, false); err == nil || !strings.Contains(err.Error(), "fix or remove it, then uninstall again") || slices.ContainsFunc(e.docker.calls, func(c Cmd) bool { return slices.Contains(c.Args, "down") }) {
		t.Fatalf("a record that does not read: %v", err)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, GatewayFile), []byte(`{"digest":"00000000","dir":"`+other+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, GatewayFile), []byte(`{"digest":"00000000","dir":"`+other+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	host := &gwHost{tables: map[string]bool{}}
	if err := e.installer(func(o *Options) {
		o.Yes, o.Log, o.Host, o.Systemd, o.Euid = true, &log, host, t.TempDir(), func() int { return 1000 }
	}).Uninstall(ctx, false); err != nil || !strings.Contains(log.String(), "(the directory moved or copied)") || host.did("iptables") {
		t.Errorf("another directory's: %v %q", err, log.String())
	}
}
