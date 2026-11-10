package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/spec"
)

// isRefused reports whether err is a refusal the run exits 3 on (Refusal).
func isRefused(err error) bool { _, ok := Refusal(err); return ok }

// Each refusal, a rule saying no before anything changed, is a Refused, wrapped or not; the run
// on the other side of its rule is not refused.
func TestRefusalsAreRefused(t *testing.T) {
	t.Parallel()
	type run func(e *env, refuse bool) error
	for _, c := range []struct {
		name  string
		setup func(t *testing.T) *env
		run   run
		want  string
	}{
		{"--secrets on an install", installed, func(e *env, refuse bool) error {
			return e.install(func(o *Options) {
				if refuse {
					o.Secrets = []byte("x")
				}
			})
		}, "--secrets is for a first install"},
		{"a first-install-only setting on a re-run", installed, func(e *env, refuse bool) error {
			return e.install(func(o *Options) {
				if refuse {
					o.Address = "127.0.0.2"
				}
			})
		}, "--address is recorded at the first install"},
		{"another project's compose file", newEnv, func(e *env, refuse bool) error {
			_ = os.MkdirAll(e.dir, 0o755)
			if refuse {
				_ = os.WriteFile(filepath.Join(e.dir, "compose.yaml"), nil, 0o644)
			}
			return e.install(nil)
		}, "already holds a compose.yaml and no install of notes"},
		{"a missing required input", newEnv, func(e *env, refuse bool) error {
			if refuse {
				e.vars = map[string]string{}
			}
			return e.install(nil)
		}, "missing required input(s)"},
		{"a port in use", newEnv, func(e *env, refuse bool) error {
			e.busy[8080] = refuse
			return e.install(nil)
		}, "already in use on 127.0.0.1: choose others with --ports"},
		{"an operator's no", newEnv, func(e *env, refuse bool) error {
			return e.install(func(o *Options) {
				if refuse {
					o.Prompt = &fakePrompter{err: ErrAborted}
				}
			})
		}, "aborted"},
		{"another release on a re-run", installed, func(e *env, refuse bool) error {
			if refuse {
				e.to("2.0.0", nil)
			}
			return e.install(nil)
		}, "upgrade --dir"},
		{"a recorded input set on an upgrade", installed, func(e *env, refuse bool) error {
			e.to("2.0.0", nil)
			return e.upgrade(func(o *Options) {
				if refuse {
					o.Set = map[string]string{"ADMIN_EMAIL": "b@example.org"}
				}
			})
		}, "ADMIN_EMAIL: recorded at the install, and an upgrade keeps it"},
		{"another run holds the lock", installed, func(e *env, refuse bool) error {
			if refuse {
				unlock, err := lockFile(filepath.Join(e.dir, lifecycleLock))
				if err != nil {
					return err
				}
				defer unlock()
			}
			e.to("2.0.0", nil)
			return e.upgrade(nil)
		}, "already running for this install"},
		{"volumes half restored", installed, func(e *env, refuse bool) error {
			if refuse {
				_ = os.WriteFile(filepath.Join(e.dir, volumesSplitFile), nil, 0o600)
			}
			e.to("2.0.0", nil)
			return e.upgrade(nil)
		}, "volumes half restored"},
		{"no previous release", installed, func(e *env, refuse bool) error {
			if !refuse {
				e.to("2.0.0", nil)
				if err := e.upgrade(nil); err != nil {
					return err
				}
			}
			return e.rollback(nil)
		}, "has no previous release to go back to"},
		{"no install in the directory", installed, func(e *env, refuse bool) error {
			if refuse {
				e.dir = filepath.Join(e.t.TempDir(), "elsewhere")
			}
			return e.upgrade(nil) // the release it runs: nothing to do
		}, "holds no install"},
		{"uninstall without a terminal", installed, func(e *env, refuse bool) error {
			return e.installer(func(o *Options) { o.Yes = !refuse }).Uninstall(context.Background(), false)
		}, "give --yes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := c.run(c.setup(t), true)
			if !isRefused(err) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v, want the refusal %q", err, c.want)
			}
			if err := c.run(c.setup(t), false); err != nil {
				t.Fatalf("the other side: %v", err)
			}
		})
	}
}

// A refused first install may leave the directory it made, holding its lock's file alone: the
// next install takes it.
func TestARefusedFirstInstallIsTakenByTheNext(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.docker.leftover = ProvisionService
	if err := e.install(nil); !isRefused(err) || !strings.Contains(err.Error(), "of an earlier run is still there") {
		t.Fatalf("%v", err)
	}
	if entries, _ := os.ReadDir(e.dir); len(entries) > 1 || len(entries) == 1 && entries[0].Name() != lifecycleLock {
		t.Fatalf("left %v", entries)
	}
	e.docker.leftover = ""
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
}

// The rollback check's refusal stays the type the VM's health gate reads, and is a Refused.
func TestARollbackRefusalIsBoth(t *testing.T) {
	t.Parallel()
	var err error = rollbackRefused("refusing: x")
	var rr rollbackRefused
	var r Refused
	if !errors.As(err, &rr) || !errors.As(err, &r) || r.Reason != "refusing: x" || r.Fix == "" {
		t.Fatalf("%q %+v", rr, r)
	}
	if !errors.As(noPrevious{"none"}, &r) || r.Reason != "none" {
		t.Fatalf("%+v", r)
	}
	if err := refuse("fix", "a %w", ErrBack); !errors.Is(err, ErrBack) || err.Error() != "a back" {
		t.Fatalf("%v", err)
	}
}

// A rollback whose start fails after the files went back is a failure, never the child's status:
// the install changed.
func TestAFailedStartAfterARollbackFails(t *testing.T) {
	t.Parallel()
	e := upgraded(t)
	err := e.rollback(func(o *Options) {
		o.Exec = func(_ context.Context, _ string, args []string) error {
			if args[0] == "--help" {
				return nil // a kept installer read
			}
			return ExitCode(3)
		}
	})
	if err == nil || errors.As(err, new(ExitCode)) || isRefused(err) || !strings.Contains(err.Error(), "the release left is kept in") || !strings.Contains(err.Error(), "exited with status 3") {
		t.Fatalf("%v", err)
	}
}

// machineRun runs verb as a --json verb of kind runs it: what it prints on stdout, and its error.
func machineRun(t *testing.T, in *Installer, kind verbKind, verb func(context.Context) error) (string, error) {
	t.Helper()
	var out bytes.Buffer
	asJSON := true
	c := &cobra.Command{Use: "verb", SilenceErrors: true, SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error { return verb(cmd.Context()) }}
	c.SetArgs([]string{})
	machineVerb(c, &asJSON, &out, in, kind, nil)
	in.O.Out = &out
	in.O.toMachine() // as prepare does
	err := c.ExecuteContext(context.Background())
	return out.String(), err
}

// oneResult is the one Result out holds, and nothing else.
func oneResult(t *testing.T, out string) Result {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	var r Result
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("%v: %q", err, out)
	}
	if rest, _ := io.ReadAll(dec.Buffered()); strings.TrimSpace(string(rest)) != "" || dec.More() {
		t.Fatalf("more than one object: %q", out)
	}
	if r.Format != MachineFormat {
		t.Errorf("format %d", r.Format)
	}
	return r
}

func TestDockerVerbsUnderJSON(t *testing.T) {
	t.Parallel()
	v1 := &Version{Version: "1.0.0", Revision: 1}
	for _, c := range []struct {
		name    string
		setup   func(t *testing.T) *env
		kind    verbKind
		verb    func(e *env, in *Installer) func(context.Context) error
		result  string
		release *Version
		refused string
	}{
		{"install", newEnv, verbKind{}, func(_ *env, in *Installer) func(context.Context) error { return in.Install }, ResultDone, v1, ""},
		{"re-run", installed, verbKind{}, func(_ *env, in *Installer) func(context.Context) error { return in.Install }, ResultDone, v1, ""},
		{"install --dry-run", newEnv, verbKind{}, func(_ *env, in *Installer) func(context.Context) error {
			in.O.DryRun = true
			return in.Install
		}, ResultNothing, nil, ""},
		{"upgrade", installed, verbKind{}, func(e *env, in *Installer) func(context.Context) error {
			e.to("2.0.0", nil)
			in.R, in.O.Self = e.r, e.self
			return in.Upgrade
		}, ResultDone, &Version{Version: "2.0.0", Revision: 1}, ""},
		{"upgrade to the release it runs", installed, verbKind{}, func(_ *env, in *Installer) func(context.Context) error { return in.Upgrade }, ResultNothing, v1, ""},
		{"upgrade to an older release", installed, verbKind{}, func(e *env, in *Installer) func(context.Context) error {
			e.to("0.9.0", nil)
			in.R, in.O.Self = e.r, e.self
			return in.Upgrade
		}, ResultNothing, v1, "after this installer's 0.9.0"},
		{"rollback", upgraded, verbKind{}, func(_ *env, in *Installer) func(context.Context) error { return in.Rollback }, ResultDone, v1, ""},
		{"uninstall", installed, verbKind{uninstall: true}, func(_ *env, in *Installer) func(context.Context) error {
			in.O.Yes = true
			return func(ctx context.Context) error { return in.Uninstall(ctx, false) }
		}, ResultDone, nil, ""},
		{"uninstall refused", installed, verbKind{uninstall: true}, func(_ *env, in *Installer) func(context.Context) error {
			return func(ctx context.Context) error { return in.Uninstall(ctx, false) }
		}, ResultNothing, v1, "give --yes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := c.setup(t)
			e.out.Reset()
			in := e.installer(nil)
			out, err := machineRun(t, in, c.kind, c.verb(e, in))
			if (err != nil) != (c.refused != "") {
				t.Fatalf("%v\n%s", err, e.log)
			}
			r := oneResult(t, out)
			if r.Result != c.result || (r.Release == nil) != (c.release == nil) || r.Release != nil && *r.Release != *c.release {
				t.Errorf("%+v %+v, want %s %+v", r, r.Release, c.result, c.release)
			}
			switch {
			case c.refused == "" && r.Refused != nil:
				t.Errorf("refused %+v", r.Refused)
			case c.refused != "" && (r.Refused == nil || !strings.Contains(r.Refused.Reason, c.refused) || r.Refused.Reason != err.Error() || r.Refused.Fix == ""):
				t.Errorf("refused %+v, want %q", r.Refused, c.refused)
			}
			// A rollback's start is a child, run in this process (inProcess) on the test's out.
			if e.out.Len() != 0 && c.name != "rollback" {
				t.Errorf("printed beside the result: %q", e.out)
			}
		})
	}
	// A failure prints no result: its error alone.
	e := installed(t)
	e.to("2.0.0", nil)
	e.docker.loaded, e.docker.pullErr = map[string]string{}, errors.New("denied")
	in := e.installer(nil)
	if out, err := machineRun(t, in, verbKind{}, in.Upgrade); err == nil || isRefused(err) || out != "" {
		t.Fatalf("%v %q", err, out)
	}
	// What a run prints for a person goes to the log.
	e = installed(t)
	in = e.installer(nil)
	if _, err := machineRun(t, in, verbKind{}, in.Upgrade); err != nil || !strings.Contains(e.log.String(), "already runs notes 1.0.0 (revision 1)") {
		t.Fatalf("%v\n%s", err, e.log)
	}
}

func TestK8sVerbsUnderJSON(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("0.9.0", 2)
	in := e.installer(func(o *Options) { o.K8s.Revision, o.K8s.Timeout = 3, 6*time.Minute })
	out, err := machineRun(t, in, verbKind{k8s: true}, in.K8sRollback)
	if err != nil {
		t.Fatal(err)
	}
	if r := oneResult(t, out); r.Result != ResultDone || r.Release == nil || *r.Release != (Version{Version: "0.9.0", Revision: 2}) || r.Refused != nil {
		t.Errorf("%+v", r)
	}
	if !strings.Contains(e.log.String(), "release notes in notes runs notes 0.9.0 (revision 2)\n") || e.out.Len() != 0 {
		t.Errorf("out %q, log %s", e.out, e.log)
	}
	// The release it already runs: nothing.
	e = newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	in = e.installer(nil)
	out, err = machineRun(t, in, verbKind{k8s: true}, func(ctx context.Context) error { return in.K8sUpgradeTo(ctx, "1.0.0", nil) })
	if err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if r := oneResult(t, out); r.Result != ResultNothing || r.Release == nil || r.Release.Version != "1.0.0" {
		t.Errorf("%+v", r)
	}
	// Refused: no release to upgrade, so none in the result.
	e = newKubeEnv(t, spec.Provider{Name: "none"})
	in = e.installer(nil)
	out, err = machineRun(t, in, verbKind{k8s: true}, in.K8sUpgrade)
	if !isRefused(err) {
		t.Fatalf("%v", err)
	}
	if r := oneResult(t, out); r.Result != ResultNothing || r.Release != nil || r.Refused == nil || !strings.Contains(r.Refused.Fix, "k8s install") {
		t.Errorf("%+v %+v", r, r.Refused)
	}
	// Uninstalled: no release.
	e = newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	in = e.installer(func(o *Options) { o.Yes = true })
	out, err = machineRun(t, in, verbKind{k8s: true, uninstall: true}, in.K8sUninstall)
	if err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if r := oneResult(t, out); r.Result != ResultDone || r.Release != nil {
		t.Errorf("%+v", r)
	}
}

// An installer run for this one under --json prints the result on this run's standard output;
// this run prints none, and passes its exit status on.
func TestAChildPrintsTheResult(t *testing.T) {
	t.Parallel()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	e := installed(t)
	in := e.installer(func(o *Options) { o.Exec = nil })
	var out bytes.Buffer
	in.O.Out = &out
	in.O.toMachine()
	err = in.runChild(context.Background(), sh, []string{"-c", `echo '{"format":1}'; echo progress >&2; exit 3`})
	if code := ExitCode(0); !errors.As(err, &code) || code != 3 || out.String() != "{\"format\":1}\n" || !strings.Contains(e.log.String(), "progress") {
		t.Fatalf("%v %q", err, out.String())
	}
	if perr := in.printResult(context.Background(), &out, verbKind{}, err); perr != err || out.String() != "{\"format\":1}\n" {
		t.Fatalf("%v %q", perr, out.String())
	}
	// A child that did the verb: its result is the one printed.
	out.Reset()
	in = e.installer(func(o *Options) { o.Exec = nil })
	in.O.Out = &out
	in.O.toMachine()
	if err := in.runChild(context.Background(), sh, []string{"-c", "exit 0"}); err != nil {
		t.Fatal(err)
	}
	if err := in.printResult(context.Background(), &out, verbKind{}, nil); err != nil || out.Len() != 0 {
		t.Fatalf("%v %q", err, out.String())
	}
	// Without --json the child's output is this run's.
	in = e.installer(func(o *Options) { o.Exec = nil })
	in.O.Out = &out
	out.Reset()
	if err := in.runChild(context.Background(), sh, []string{"-c", "echo hi"}); err != nil || out.String() != "hi\n" || in.delegated {
		t.Fatalf("%v %q", err, out.String())
	}
}

// The verbs that change an install take --json, the result alone on standard output, and pass it
// to the installer upgrade --version runs.
func TestTheVerbsTakeJSON(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	tools := K8sOptions{Helm: kubeTool{e.f, "helm"}, Kubectl: kubeTool{e.f, "kubectl"}}
	root := newCommand(e.release, "notes-stow", io.Discard, io.Discard, tools, func(bool) Prompter { return nil })
	for _, path := range [][]string{{}, {"upgrade"}, {"rollback"}, {"uninstall"}, {"k8s", "install"}, {"k8s", "upgrade"}, {"k8s", "rollback"}, {"k8s", "uninstall"}} {
		c, _, err := root.Find(path)
		if err != nil || c.Flags().Lookup("json") == nil {
			t.Errorf("%v: no --json (%v)", path, err)
		}
	}
	up, _, _ := root.Find([]string{"upgrade"})
	if err := up.ParseFlags([]string{"--json", "--version", "2.0.0"}); err != nil {
		t.Fatal(err)
	}
	if args := passed(up, "version"); strings.Join(args, " ") != "--json=true" {
		t.Errorf("passed %v", args)
	}
	// Through the command: a refusal prints one result, and nothing else.
	var out, log bytes.Buffer
	cmd := newCommand(e.release, "notes-stow", &out, &log, tools, func(bool) Prompter { return nil })
	cmd.SetArgs([]string{"k8s", "rollback", "--json"})
	if err := cmd.ExecuteContext(context.Background()); !isRefused(err) {
		t.Fatalf("%v\n%s", err, &log)
	}
	if r := oneResult(t, out.String()); r.Refused == nil || r.Refused.Reason != "no release notes in notes" || r.Release != nil {
		t.Errorf("%+v", r)
	}
	// Done: what the run prints for a person goes to standard error.
	e.runs("0.9.0", 2)
	out.Reset()
	log.Reset()
	cmd = newCommand(e.release, "notes-stow", &out, &log, tools, func(bool) Prompter { return nil })
	cmd.SetArgs([]string{"k8s", "rollback", "--json", "--revision", "3"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, &log)
	}
	if r := oneResult(t, out.String()); r.Result != ResultDone || r.Release == nil || r.Release.Version != "0.9.0" || !strings.Contains(log.String(), "release notes in notes runs notes 0.9.0 (revision 2)") {
		t.Errorf("%+v\n%s", r, &log)
	}
	// A refusal before the verb runs (its flags): one result too.
	out.Reset()
	cmd = newCommand(e.release, "notes-stow", &out, &log, tools, func(bool) Prompter { return nil })
	cmd.SetArgs([]string{"upgrade", "--json", "--kit", "x"})
	if err := cmd.ExecuteContext(context.Background()); !isRefused(err) {
		t.Fatalf("%v", err)
	}
	if r := oneResult(t, out.String()); r.Refused == nil || !strings.HasPrefix(r.Refused.Reason, "--kit: an upgrade takes none of it") {
		t.Errorf("%+v", r)
	}
	// A Docker install of another project: refused before Docker is asked anything.
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, envName), []byte("STOWAGE_NAME='atlas'\nSTOWAGE_PROJECT_VERSION='3.0.0'\nSTOWAGE_REVISION='2'\n"), 0o600)
	out.Reset()
	cmd = newCommand(e.release, "notes-stow", &out, &log, tools, func(bool) Prompter { return nil })
	cmd.SetArgs([]string{"uninstall", "--json", "--dir", dir})
	if err := cmd.ExecuteContext(context.Background()); !isRefused(err) {
		t.Fatalf("%v", err)
	}
	if r := oneResult(t, out.String()); r.Result != ResultNothing || r.Refused == nil || !strings.Contains(r.Refused.Reason, `an install of "atlas", not "notes"`) || r.Release != nil {
		t.Errorf("%+v %+v", r, r.Refused)
	}
}

// fixRuns fails unless each flag fix names is one of the command it names (`<short>-stow` and
// its verb), else of the verb that raised it (raising, its path from the root): a fix is run as
// written. A fix naming another program's command (helm, kubectl, docker) is not checked.
func fixRuns(t *testing.T, root *cobra.Command, raising []string, fix string) {
	t.Helper()
	words := strings.Fields(fix)
	path := raising
	for i, w := range words {
		if w == "helm" || w == "kubectl" || w == "docker" {
			return
		}
		if strings.HasSuffix(w, "-stow") {
			path = nil
			for _, v := range words[i+1:] {
				if strings.HasPrefix(v, "-") {
					break
				}
				path = append(path, v)
			}
			// The verbs alone: a word that is no subcommand ends the path.
			for len(path) > 0 {
				if c, rest, err := root.Find(path); err == nil && len(rest) == 0 && c.Name() == path[len(path)-1] {
					break
				}
				path = path[:len(path)-1]
			}
			break
		}
	}
	c, _, err := root.Find(path)
	if err != nil {
		t.Fatalf("fix %q: %v", fix, err)
	}
	for _, w := range words {
		if !strings.HasPrefix(w, "--") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(w, "--"), "=")
		name = strings.TrimRight(name, ",.;:)`")
		if c.Flag(name) == nil {
			t.Errorf("fix %q: %s has no --%s", fix, c.CommandPath(), name)
		}
	}
}

func rootOf(e *env) *cobra.Command {
	return newCommand(e.r, e.self, io.Discard, io.Discard, K8sOptions{}, func(bool) Prompter { return nil })
}

// What a fix names runs: the flags of the verb that raised it, or of the command it names.
func TestEachFixRuns(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		version string
		mod     func(*spec.Spec)
		opts    func(*Options)
		want    string // in the fix
	}{
		// The front's recorded port: changed by a re-run alone, never by the upgrade's --ports.
		"l4 on the front's port": {"2.0.0", func(s *spec.Spec) {
			s.L4.Routes = []spec.L4Route{{Name: "data", Protocol: "tcp", Port: 8000, Upstream: "api:7000"}}
		}, nil, "notes-stow --dir "},
		// An L4 route bound to an address the host does not have, by an input the release adds: the
		// upgrade's own --set gives it.
		"l4 bound elsewhere": {"2.0.0", bindRoute, func(o *Options) { o.Set = map[string]string{"DATA_BIND": "203.0.113.9"} }, "--set KEY=<address> on this upgrade"},
		// By a recorded input: a re-run of the installer the install runs changes it.
		"l4 bound elsewhere, recorded": {"2.0.0", bindRoute, nil, "notes-stow --dir "},
		// An L4 route's port in use, no bind input: the port is freed.
		"l4 port in use": {"2.0.0", func(s *spec.Spec) {
			s.L4.Routes = []spec.L4Route{{Name: "data", Protocol: "tcp", Port: 9100, Upstream: "api:7000"}}
		}, nil, "or free the ports the reason names"},
		"older":          {"0.9.0", nil, nil, ""},
		"tls mode":       {"2.0.0", func(s *spec.Spec) { s.Front.TLSModes = []string{"acme"} }, nil, ""},
		"recorded input": {"2.0.0", nil, func(o *Options) { o.Set = map[string]string{"ADMIN_EMAIL": "b@example.org"} }, ""},
		"new route":      {"2.0.0", docsRoute(0), nil, ""},
		"--ports":        {"2.0.0", docsRoute(9000), func(o *Options) { o.Ports = map[string]int{"app": 1} }, ""},
		"offline":        {"2.0.0", nil, func(o *Options) { o.Offline = true }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := installed(t)
			if name == "l4 bound elsewhere, recorded" {
				env := e.readEnv()
				env.Set("DATA_BIND", "203.0.113.9")
				_ = env.WriteFile(filepath.Join(e.dir, envName))
			}
			e.to(c.version, c.mod)
			e.busy[9100] = true
			r, ok := Refusal(e.upgrade(c.opts))
			if !ok {
				t.Fatal("not refused")
			}
			fixRuns(t, rootOf(e), []string{"upgrade"}, r.Fix)
			if !strings.Contains(r.Fix, c.want) {
				t.Errorf("fix %q, want %q in it", r.Fix, c.want)
			}
		})
	}
}

// A check that refuses after the upgrade's first change fails the run: the install changed.
func TestARefusalAfterTheFirstChangeFails(t *testing.T) {
	t.Parallel()
	e := installed(t)
	e.to("2.0.0", nil)
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if strings.Contains(a, " pg_dump ") { // a restore starts as the database is dumped
			_ = os.WriteFile(filepath.Join(e.dir, volumesSplitFile), nil, 0o600)
		}
		return nil, false, nil
	}
	err := e.upgrade(nil)
	if err == nil || !strings.Contains(err.Error(), "volumes half restored") || !errors.As(err, new(Refused)) {
		t.Fatalf("%v", err)
	}
	if _, ok := Refusal(err); ok {
		t.Fatal("a refusal after the change")
	}
	if r := oneResultOf(t, e, err); r != "" {
		t.Fatalf("printed %q", r)
	}
}

// oneResultOf is what a --json run ending with err prints.
func oneResultOf(t *testing.T, e *env, err error) string {
	t.Helper()
	var out bytes.Buffer
	in := e.installer(nil)
	_ = in.printResult(context.Background(), &out, verbKind{}, err)
	return out.String()
}

// The kept installer refuses an upgrade when it carries another release or none; when it cannot
// be read, the upgrade fails.
func TestAKeptInstallerThatDoesNotReadFails(t *testing.T) {
	t.Parallel()
	e := installed(t)
	e.to("2.0.0", nil)
	ctl := filepath.Join(e.dir, "notes-stow")
	_ = os.WriteFile(ctl, []byte("no payload"), 0o755)
	if err := e.upgrade(nil); !isRefused(err) || !strings.Contains(err.Error(), "is not the installer of notes 1.0.0") {
		t.Fatalf("carries none: %v", err)
	}
	_ = os.Remove(ctl)
	_ = os.Symlink(filepath.Join(e.dir, "gone"), ctl) // read fails, for root too
	if err := e.upgrade(nil); err == nil || isRefused(err) || !strings.Contains(err.Error(), "is not the installer of notes 1.0.0") {
		t.Fatalf("does not read: %v", err)
	}
}

// The result's release on Kubernetes is the one helm has deployed, never a failed or pending one.
func TestTheK8sResultNamesTheDeployedRelease(t *testing.T) {
	t.Parallel()
	for status, want := range map[string]bool{"deployed": true, "failed": false, "pending-install": false, "pending-upgrade": false} {
		e := newKubeEnv(t, spec.Provider{Name: "none"})
		e.runs("1.0.0", 1)
		e.f.listed[0].Status = status
		if got := e.installer(nil).k8sReleaseNow(context.Background()); (got != nil) != want {
			t.Errorf("%s: %+v", status, got)
		}
	}
	// No tools (a run that refused before it read its options): none, never a panic.
	if got := (&Installer{}).k8sReleaseNow(context.Background()); got != nil {
		t.Errorf("%+v", got)
	}
}

// A refusal before the verb takes its options (a flag's, the answers file's) names the release of
// the install --dir names, and never panics on Kubernetes.
func TestARefusalBeforeTheOptionsNamesTheirInstall(t *testing.T) {
	t.Parallel()
	e := installed(t)
	answers := filepath.Join(t.TempDir(), "answers.yaml")
	_ = os.WriteFile(answers, []byte("tls:\n  mode: internal\n"), 0o600)
	for _, args := range [][]string{
		{"upgrade", "--json", "--kit", "x", "--dir", e.dir},
		{"upgrade", "--json", "--answers", answers, "--dir", e.dir},
		{"k8s", "install", "--json", "--answers", answers},
	} {
		var out bytes.Buffer
		cmd := newCommand(e.r, e.self, &out, io.Discard, K8sOptions{}, func(bool) Prompter { return nil })
		cmd.SetArgs(args)
		if err := cmd.ExecuteContext(context.Background()); !isRefused(err) {
			t.Fatalf("%v: %v", args, err)
		}
		r := oneResult(t, out.String())
		if args[0] == "k8s" {
			if r.Release != nil || r.Refused == nil {
				t.Errorf("%v: %+v", args, r)
			}
			continue
		}
		if r.Refused == nil || r.Release == nil || *r.Release != (Version{"1.0.0", 1}) {
			t.Errorf("%v: %+v", args, r)
		}
	}
}

// The lock's refusal keeps errLocked as its cause: a first install leaves another run's lock alone.
func TestTheLocksRefusalIsAnotherRunsLock(t *testing.T) {
	t.Parallel()
	e := installed(t)
	unlock, err := lockFile(filepath.Join(e.dir, lifecycleLock))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	in := e.installer(nil)
	if _, err := in.lockRun(context.Background(), e.readEnv()); !isRefused(err) || !errors.Is(err, errLocked) {
		t.Fatalf("%v", err)
	}
}

// A probe of the kept installer that did not run is a failure; one that ran and said no, a refusal.
func TestAKeptInstallersProbeThatDidNotRunFails(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		probe   error
		refused bool
	}{{ExitCode(1), true}, {errors.New("fork/exec notes-stow: permission denied"), false}, {ExitCode(-1), false}, {ExitCode(2), false}, {ExitCode(127), false}} {
		e := installed(t)
		anotherStowages(t, e)
		e.to("2.0.0", nil)
		err := e.upgrade(func(o *Options) {
			o.Exec = func(_ context.Context, _ string, args []string) error {
				if args[0] == "--help" {
					return c.probe
				}
				return nil
			}
		})
		if err == nil || isRefused(err) != c.refused {
			t.Errorf("%v: %v, refused %t", c.probe, err, c.refused)
		}
	}
}

// A boot changes the install from its start: a refusal it raises is a failure.
func TestABootsRefusalFails(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	env := e.readEnv()
	env.Set("STOWAGE_NAME", "atlas")
	if err := env.WriteFile(filepath.Join(e.dir, envName)); err != nil {
		t.Fatal(err)
	}
	err := e.installer(vm).Boot(context.Background())
	if err == nil || !errors.As(err, new(Refused)) {
		t.Fatalf("%v", err)
	}
	if _, ok := Refusal(err); ok {
		t.Fatal("refused")
	}
}

// A refusal another installer answers names it, run with the same arguments; an older
// release's fix is never a rollback.
func TestTheFixNamesTheInstallerToRun(t *testing.T) {
	t.Parallel()
	e := installed(t)
	env := e.readEnv()
	env.Set("STOWAGE_SHORT_NAME", "nt")
	_ = env.WriteFile(filepath.Join(e.dir, envName))
	r, ok := Refusal(e.rollback(nil))
	if want := "run " + filepath.Join(e.dir, "nt-stow") + " instead of this installer, with the same arguments (it knows this install), for the release the install runs; or install this release afresh, notes-stow --dir <another directory>"; !ok || r.Fix != want {
		t.Errorf("%+v, want %q", r, want)
	}
	e = installed(t)
	e.to("0.9.0", nil)
	if r, ok := Refusal(e.upgrade(nil)); !ok || strings.Contains(r.Fix, "rollback") || !strings.Contains(r.Fix, "nothing to do") {
		t.Errorf("%+v", r)
	}
}

// Uninstall's lock refusal is the lifecycle lock's: one wording, another run's lock as its cause.
func TestUninstallRefusesUnderTheLock(t *testing.T) {
	t.Parallel()
	e := installed(t)
	unlock, err := lockFile(filepath.Join(e.dir, lifecycleLock))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	err = e.installer(func(o *Options) { o.Yes = true }).Uninstall(context.Background(), false)
	if err != lockedRefusal || !errors.Is(err, errLocked) {
		t.Fatalf("%v", err)
	}
}

// bindRoute is a release whose L4 route data binds to the address its input DATA_BIND holds.
func bindRoute(s *spec.Spec) {
	s.Inputs = append(s.Inputs, spec.Input{Key: "DATA_BIND", Prompt: "x"})
	s.L4.Routes = []spec.L4Route{{Name: "data", Protocol: "tcp", Port: 9200, Upstream: "api:7000", BindInput: "DATA_BIND"}}
}

// The trust refusal of upgrade --version names the version asked.
func TestTheTrustFixNamesTheVersion(t *testing.T) {
	t.Parallel()
	e := installed(t)
	anotherStowages(t, e)
	in := e.installer(func(o *Options) {
		o.Exec = func(context.Context, string, []string) error { return nil } // it reads itself
	})
	r, ok := Refusal(in.UpgradeTo(context.Background(), "2.0.0", nil))
	if !ok || r.Fix != "run "+filepath.Join(e.dir, "notes-stow")+" upgrade --version 2.0.0 instead of this installer, with the same other arguments (it knows the keys the install trusts)" {
		t.Fatalf("%+v %t", r, ok)
	}
}

// A boot never passes the status of an installer it runs on: its refusal there, a 3, is the
// boot's failure.
func TestABootFailsWhateverItsChildExits(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	e.r, e.self = bundledInstaller(t, "2.0.0", 1, nil)
	in := e.installer(vm)
	in.O.Exec = func(context.Context, string, []string) error { return ExitCode(3) }
	err := in.Boot(context.Background())
	if err == nil || errors.As(err, new(ExitCode)) || isRefused(err) || !strings.Contains(err.Error(), "exited with status 3") {
		t.Fatalf("%v", err)
	}
}

// A rollback by another installer than the one kept is refused as such, before the kept release
// is read; a kept installer whose manifest does not decode fails the rollback, unrefused.
func TestARollbackChecksItsInstallerFirst(t *testing.T) {
	t.Parallel()
	e := upgraded(t)
	_ = os.WriteFile(filepath.Join(e.dir, PrevDir, "notes-stow"), []byte("no payload"), 0o755)
	e.to("3.0.0", nil)
	if err := e.rollback(nil); !isRefused(err) || !strings.Contains(err.Error(), "a rollback is run by the installer the install keeps") {
		t.Fatalf("%v", err)
	}
	e = upgraded(t)
	ctl := filepath.Join(e.dir, PrevDir, "notes-stow")
	files, err := payload.Read(ctl)
	if err != nil {
		t.Fatal(err)
	}
	files[payload.Release] = []byte("{")
	var buf bytes.Buffer
	if err := payload.Write(&buf, strings.NewReader("exe"), files); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(ctl, buf.Bytes(), 0o755)
	if err := e.rollback(nil); err == nil || isRefused(err) || !strings.Contains(err.Error(), "the previous release's installer") {
		t.Fatalf("%v", err)
	}
}

// boot-rollback's first change is the database it starts: a refusal before it is one, after it a
// failure.
func TestABootRollbackChangesAsItStartsTheDatabase(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	if err := os.Remove(filepath.Join(e.dir, envName)); err != nil {
		t.Fatal(err)
	}
	if err := e.installer(vm).BootRollback(context.Background()); !isRefused(err) {
		t.Fatalf("before: %v", err)
	}
	e = installedVM(t)
	err := e.installer(vm).BootRollback(context.Background())
	if _, ok := Refusal(err); err == nil || !strings.Contains(err.Error(), "has no previous release to go back to") || ok {
		t.Fatalf("after: %v", err)
	}
}

// A rollback by another installer names the one kept, run with the same arguments.
func TestAWrongInstallersRollbackNamesTheKeptOne(t *testing.T) {
	t.Parallel()
	e := upgraded(t)
	e.to("3.0.0", nil)
	r, ok := Refusal(e.rollback(nil))
	if want := "run " + filepath.Join(e.dir, "notes-stow") + " instead of this installer, with the same arguments (a rollback is run by the installer the install keeps)"; !ok || r.Fix != want {
		t.Fatalf("%+v", r)
	}
}

// tune --apply's start after the consent is this run's: a failure, never the child's status.
func TestTheGatewaysStartFailsAsItsOwn(t *testing.T) {
	t.Parallel()
	in, _, _ := gateway(t, 0)
	in.O.Exec = func(context.Context, string, []string) error { return ExitCode(3) }
	err := in.gatewayApply(context.Background())
	if err == nil || errors.As(err, new(ExitCode)) || !strings.Contains(err.Error(), "exited with status 3") {
		t.Fatalf("%v", err)
	}
}

// A rollback run by the kept installer itself, of another release than the one installed (an
// upgrade stopped before it kept its own): the advice names the installed release's installer.
func TestTheKeptInstallerNeverAdvisesItself(t *testing.T) {
	t.Parallel()
	e := upgraded(t)
	e.to("3.0.0", nil)
	ctl := filepath.Join(e.dir, "notes-stow")
	raw, err := os.ReadFile(e.self)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(ctl, raw, 0o755)
	r, ok := Refusal(e.rollback(func(o *Options) { o.Self = ctl }))
	if !ok || strings.Contains(r.Fix, ctl) || !strings.HasPrefix(r.Fix, "run the installer of notes 2.0.0 (revision 1) instead of this one") {
		t.Fatalf("%+v", r)
	}
}

// An install that records no short name: the advice names the installer it keeps, generically.
func TestNoShortNameIsAdvisedGenerically(t *testing.T) {
	t.Parallel()
	e := installed(t)
	env := e.readEnv()
	env.Delete("STOWAGE_SHORT_NAME")
	_ = env.WriteFile(filepath.Join(e.dir, envName))
	r, ok := Refusal(e.rollback(nil))
	if !ok || !strings.HasPrefix(r.Fix, "run the installer the install keeps in "+e.dir+" instead") || strings.Contains(r.Fix, "/-stow") {
		t.Fatalf("%+v", r)
	}
}
