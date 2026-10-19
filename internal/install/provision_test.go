package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/spec"
)

// hookCalls are the provision hook's runs.
func (e *env) hookCalls() []Cmd {
	return e.docker.ran("compose --project-directory " + e.dir + " -f " + filepath.Join(e.dir, "compose.yaml") + " -f " + filepath.Join(e.dir, FrontCompose) + " -f " + filepath.Join(e.dir, ProvisionCompose))
}

func TestTheHookRunsAfterTheStackAndItsOutputsAreRecorded(t *testing.T) {
	e := newEnv(t)
	e.docker.hookOut = "ADMIN_PASSWORD=s3cret$x#y\n\nTOKEN=abc\r\n"
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if n := len(e.hookCalls()); n != 1 {
		t.Fatalf("%d hook runs", n)
	}
	outs, err := ReadEnvFile(filepath.Join(e.dir, ProvisionEnv))
	if err != nil {
		t.Fatal(err)
	}
	if get(outs, "ADMIN_PASSWORD") != "s3cret$x#y" || get(outs, "TOKEN") != "abc" {
		t.Fatalf("provision.env: %v", outs.Keys())
	}
	if fi, _ := os.Stat(filepath.Join(e.dir, ProvisionEnv)); fi.Mode().Perm() != 0o600 {
		t.Errorf("provision.env mode %v", fi.Mode().Perm())
	}
	if !strings.Contains(e.out.String(), "provisioned: ADMIN_PASSWORD, TOKEN") || strings.Contains(e.out.String(), "s3cret") {
		t.Errorf("output: %s", e.out)
	}

	// A re-run runs the hook again and gives it what it returned before.
	e.docker.hookOut = "TOKEN=def\n"
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(e.docker.hookEnv, " ") != "ADMIN_PASSWORD='s3cret$x#y' TOKEN='abc'" {
		t.Fatalf("the hook was given %v", e.docker.hookEnv)
	}
	outs, _ = ReadEnvFile(filepath.Join(e.dir, ProvisionEnv))
	if get(outs, "ADMIN_PASSWORD") != "s3cret$x#y" || get(outs, "TOKEN") != "def" {
		t.Fatalf("provision.env after a re-run: %v %v", get(outs, "ADMIN_PASSWORD"), get(outs, "TOKEN"))
	}
}

func TestTheHookServiceIsConfinedAndGetsTheRecordedValues(t *testing.T) {
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(e.dir, ProvisionCompose))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]struct {
			Image       string   `yaml:"image"`
			Entrypoint  []string `yaml:"entrypoint"`
			Profiles    []string `yaml:"profiles"`
			CapDrop     []string `yaml:"cap_drop"`
			SecurityOpt []string `yaml:"security_opt"`
			Environment []string `yaml:"environment"`
			EnvFile     []string `yaml:"env_file"`
			Volumes     []string `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	s := doc.Services[ProvisionService]
	if s.Image != "${STOWAGE_IMAGE_API}" || strings.Join(s.Entrypoint, " ") != "notes-api create-admin" ||
		strings.Join(s.CapDrop, ",") != "ALL" || strings.Join(s.SecurityOpt, ",") != "no-new-privileges:true" ||
		strings.Join(s.Profiles, ",") != ProvisionService || len(s.Volumes) != 0 || strings.Join(s.EnvFile, ",") != "./"+ProvisionEnv {
		t.Fatalf("service: %+v", s)
	}
	// What its env declares (ADMIN_EMAIL), the URLs and the values Stowage provides: no secret
	// it did not ask for.
	env := strings.Join(s.Environment, " ")
	for _, k := range []string{"ADMIN_EMAIL", "STOWAGE_URL_APP", "STOWAGE_NAME"} {
		if !strings.Contains(" "+env+" ", " "+k+" ") {
			t.Errorf("the hook does not receive %s: %s", k, env)
		}
	}
	for _, k := range []string{"SESSION_SECRET", "POSTGRES_PASSWORD", "SECURE_COOKIES", "STOWAGE_IMAGE_API", "COMPOSE_PROJECT_NAME", "STOWAGE_PORT_APP", "STOWAGE_REGISTRY"} {
		if strings.Contains(env, k) {
			t.Errorf("the hook receives %s", k)
		}
	}
	// The values come from the recorded file, never the shell.
	run := e.hookCalls()[0]
	if envs := strings.Join(run.Env, "\n"); strings.Contains(envs, "from-the-shell") {
		t.Fatalf("hook environment:\n%s", envs)
	}
}

func TestADollarInTheHooksCmdStaysLiteral(t *testing.T) {
	out, err := provisionCompose(&providers.ProvisionHook{Image: "api", Cmd: []string{"sh", "-c", "echo $HOME ${X}"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "echo $$HOME $${X}") {
		t.Fatalf("%s", out)
	}
}

func TestRefusedOutputLinesAreReportedAndTheRestKept(t *testing.T) {
	e := newEnv(t)
	e.docker.hookOut = "starting\nGOOD=1\nADMIN_EMAIL=x\nSTOWAGE_OWN=x\nlower=1\nQ=it's\n"
	err := e.install(nil)
	if err == nil {
		t.Fatal("refused lines passed")
	}
	for _, want := range []string{"line 1: not KEY=VALUE", "line 3: ADMIN_EMAIL is a value the install records", "line 4: STOWAGE_OWN", "line 5: not KEY=VALUE", "line 6: Q:", "notes-stow provision --dir " + e.dir} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	outs, _ := ReadEnvFile(filepath.Join(e.dir, ProvisionEnv))
	if strings.Join(outs.Keys(), ",") != "GOOD" {
		t.Fatalf("recorded %v", outs.Keys())
	}
}

func TestAFailedHookKeepsWhatItReturnedAndTheOutputsFile(t *testing.T) {
	e := newEnv(t)
	e.r.Spec.Hooks.Provision.Config["outputs"] = "credentials.txt"
	e.docker.hookOut, e.docker.hookErr = "ADMIN_PASSWORD=minted\nPARTIAL=no line break", errors.New("exit status 1")
	err := e.install(nil)
	if e.docker.hookSaved != "ADMIN_PASSWORD='minted'\n" {
		t.Fatalf("a printed line was not saved as it arrived: %q", e.docker.hookSaved)
	}
	if err == nil || !strings.Contains(err.Error(), "provisioning did not finish") || !strings.Contains(err.Error(), "the stack is running") ||
		!strings.Contains(err.Error(), "stdout line 2: not recorded, it has no line break") {
		t.Fatalf("got %v", err)
	}
	raw, rerr := os.ReadFile(filepath.Join(e.dir, "credentials.txt"))
	if rerr != nil || string(raw) != "ADMIN_PASSWORD=minted\n" { // a failed run may have cut its last line
		t.Fatalf("credentials.txt: %q %v", raw, rerr)
	}
	if fi, _ := os.Stat(filepath.Join(e.dir, "credentials.txt")); fi.Mode().Perm() != 0o600 {
		t.Errorf("credentials.txt mode %v", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(e.dir, ".env")); err != nil {
		t.Fatal("a failed hook removed the install")
	}
	if raw, _ := os.ReadFile(filepath.Join(e.dir, installingFile)); !strings.HasPrefix(string(raw), "failed: provisioning did not finish") {
		t.Fatalf("the first install's marker: %q", raw)
	}
	e.out.Reset()
	if err := e.installer(nil).Status(context.Background()); err != nil || !strings.Contains(e.out.String(), "not finished: the first install failed (provisioning did not finish") ||
		!strings.Contains(e.out.String(), "a re-run of the installer with --dir "+e.dir+" finishes it") {
		t.Fatalf("status: %v\n%s", err, e.out)
	}
	// A re-run that fails again records it again (it runs the install as under way first).
	e.docker.hookOut = "ADMIN_PASSWORD=minted\n"
	if err := e.install(nil); err == nil {
		t.Fatal("the re-run passed with the hook failing")
	} else if !strings.Contains(err.Error(), "continues it, and with `--set KEY=VALUE` sets a value it returned or declares it returns (ADMIN_PASSWORD) first, `--set KEY=` forgets one") {
		t.Fatalf("the failure does not name --set: %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(e.dir, installingFile)); !strings.HasPrefix(string(raw), "failed: provisioning did not finish") {
		t.Fatalf("the failed re-run's marker: %q", raw)
	}
	e.docker.hookOut = "PARTIAL=no line break"

	if len(e.docker.ran("rm")) != 0 {
		t.Error("a hook that exited on its own had its container removed")
	}

	// The provision verb continues it, with what it minted; a finished run's last line counts.
	e.docker.hookOut, e.docker.hookErr = "PARTIAL=no line break", nil
	if err := e.installer(nil).Provision(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(e.docker.hookEnv, " ") != "ADMIN_PASSWORD='minted'" {
		t.Fatalf("the hook was given %v", e.docker.hookEnv)
	}
	if raw, _ := os.ReadFile(filepath.Join(e.dir, "credentials.txt")); string(raw) != "ADMIN_PASSWORD=minted\nPARTIAL=no line break\n" {
		t.Fatalf("credentials.txt: %q", raw)
	}
	// The stack had come up and the hook failed: the verb, which runs it, finishes the install.
	if _, err := os.Stat(filepath.Join(e.dir, installingFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the provision verb that finished the first install left its marker")
	}
}

func TestProvisionDoesNotFinishAFirstInstallWhoseStackNeverCameUp(t *testing.T) {
	e := newEnv(t)
	e.docker.upErr = errors.New("unhealthy")
	if err := e.install(nil); err == nil {
		t.Fatal("the install passed")
	}
	marker := filepath.Join(e.dir, installingFile)
	if raw, _ := os.ReadFile(marker); !strings.HasPrefix(string(raw), "failed: ") || strings.Contains(string(raw), stackUp) {
		t.Fatalf("the marker: %q", raw)
	}
	e.docker.upErr = nil
	if err := e.installer(nil).Provision(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the provision verb finished a first install whose stack never came up")
	}
	if err := e.install(nil); err != nil { // a re-run brings it up
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the re-run that brought the stack up left the marker")
	}
}

func TestADryRunOrARefusedReRunKeepsTheRecordedFailure(t *testing.T) {
	e := newEnv(t)
	e.docker.upErr = errors.New("unhealthy")
	if err := e.install(nil); err == nil {
		t.Fatal("the install passed")
	}
	marker := filepath.Join(e.dir, installingFile)
	before, _ := os.ReadFile(marker)
	e.docker.upErr = nil
	if err := e.install(func(o *Options) { o.DryRun = true }); err != nil {
		t.Fatal(err)
	}
	if err := e.install(func(o *Options) { o.TLS = "internal" }); err == nil {
		t.Fatal("a re-run changing the TLS mode passed")
	}
	if after, _ := os.ReadFile(marker); string(after) != string(before) {
		t.Fatalf("the marker went from %q to %q", before, after)
	}
}

func TestAFirstInstallCutRecordsNoFailure(t *testing.T) {
	e := newEnv(t)
	e.docker.hookErr = context.Canceled
	if err := e.install(nil); err == nil {
		t.Fatal("the install passed")
	}
	if raw, err := os.ReadFile(filepath.Join(e.dir, installingFile)); err != nil || len(raw) != 0 {
		t.Fatalf("a run cut by a reboot or a stop: marker %q %v, want an empty one", raw, err)
	}
}

func TestReasonKeepsTheFirstLineUpToItsCap(t *testing.T) {
	t.Parallel()
	exact := strings.Repeat("a", maxReason)
	for name, c := range map[string]struct{ in, want string }{
		"first line":   {"one\ntwo", "one"},
		"at the cap":   {exact, exact},
		"past the cap": {exact + "b", exact},
		"a split rune": {strings.Repeat("a", maxReason-1) + "é", strings.Repeat("a", maxReason-1)},
		"a whole rune": {strings.Repeat("a", maxReason-2) + "é", strings.Repeat("a", maxReason-2) + "é"},
	} {
		if got := reason(errors.New(c.in)); got != c.want || !utf8.ValidString(got) {
			t.Errorf("%s: %d bytes %q", name, len(got), got[max(0, len(got)-4):])
		}
	}
}

func TestAStoppedRunRemovesItsContainer(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.docker.hookOut, e.docker.hookStop = "A=1\nB=cut", cancel
	err := e.installer(nil).Install(ctx)
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("got %v", err)
	}
	name := get(e.readEnv(), "COMPOSE_PROJECT_NAME") + "-" + ProvisionService
	if rm := e.docker.ran("rm"); len(rm) != 1 || strings.Join(rm[0].Args, " ") != "rm --force "+name {
		t.Fatalf("rm calls: %v", rm)
	}
	outs, _ := ReadEnvFile(filepath.Join(e.dir, ProvisionEnv))
	if strings.Join(outs.Keys(), ",") != "A" {
		t.Fatalf("recorded %v", outs.Keys())
	}
}

func TestAContainerLeftBehindIsNamedNeverRunAlongside(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.docker.hookStop, e.docker.rmErr = cancel, errors.New("daemon busy")
	err := e.installer(nil).Install(ctx)
	name := get(e.readEnv(), "COMPOSE_PROJECT_NAME") + "-" + ProvisionService
	if err == nil || !strings.Contains(err.Error(), "may still be running: `docker rm --force "+name+"`") {
		t.Fatalf("a failed removal: %v", err)
	}
	e.docker.hookStop, e.docker.leftover = nil, ProvisionService
	e.docker.calls = nil
	err = e.installer(nil).Provision(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is still there") || len(e.hookCalls()) != 0 {
		t.Fatalf("a leftover container: %v", err)
	}
	envBefore, _ := os.ReadFile(filepath.Join(e.dir, ".env"))
	err = e.installer(func(o *Options) { o.Set = map[string]string{"ADMIN_EMAIL": "new@example.org"} }).Provision(context.Background())
	if envAfter, _ := os.ReadFile(filepath.Join(e.dir, ".env")); err == nil || string(envAfter) != string(envBefore) {
		t.Fatalf("a --set refused for a leftover container changed .env: %v", err)
	}
	e.docker.leftover = ""
	if err := e.installer(nil).Provision(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOneRunAtATime(t *testing.T) {
	e := newEnv(t)
	if err := e.install(func(o *Options) { o.NoProvision = true }); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockFile(filepath.Join(e.dir, lifecycleLock))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(e.dir, ProvisionEnv))
	err = e.installer(nil).Provision(context.Background())
	if err == nil || !strings.Contains(err.Error(), "already running") || len(e.hookCalls()) != 0 {
		t.Fatalf("while another run holds the lock: %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(e.dir, ProvisionEnv)); string(after) != string(before) {
		t.Fatal("a refused run rewrote provision.env")
	}
	envBefore, _ := os.ReadFile(filepath.Join(e.dir, ".env"))
	err = e.installer(func(o *Options) { o.Set = map[string]string{"ADMIN_EMAIL": "new@example.org"} }).Provision(context.Background())
	if envAfter, _ := os.ReadFile(filepath.Join(e.dir, ".env")); err == nil || string(envAfter) != string(envBefore) {
		t.Fatalf("a refused --set changed .env: %v", err)
	}
	unlock()
	if err := e.installer(nil).Provision(context.Background()); err != nil {
		t.Fatalf("once released: %v", err)
	}
}

func TestAnOutputThatBecameARecordedValueIsDropped(t *testing.T) {
	e := newEnv(t)
	e.docker.hookOut = "TOKEN=1\n"
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, ProvisionEnv), []byte("ADMIN_EMAIL='old'\nTOKEN='1'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(e.docker.hookEnv, " ") != "TOKEN='1'" || !strings.Contains(e.log.String(), "dropping ADMIN_EMAIL") {
		t.Fatalf("the hook was given %v\n%s", e.docker.hookEnv, e.log)
	}
}

func TestEveryFileTheInstallWritesIsRefusedAsOutputs(t *testing.T) {
	e := installed(t)
	e.docker.hookOut = "A=1\n"
	names := []string{TLSDir} // a byo install's
	list := func() {
		entries, err := os.ReadDir(e.dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range entries {
			names = append(names, d.Name())
		}
	}
	list()
	e.to("2.0.0", nil)
	if err := e.upgrade(nil); err != nil {
		t.Fatal(err)
	}
	list()
	if err := e.rollback(nil); err != nil {
		t.Fatal(err)
	}
	list()
	for _, name := range names {
		p := spec.Provider{Name: "container", Config: map[string]any{"image": "api", "cmd": []any{"x"}, "outputs": name}}
		if err := providers.Provision("notes", &p, map[string]spec.Image{"api": {}}); err == nil {
			t.Errorf("outputs: %s passed", name)
		}
	}
}

func newOutputWriter() *outputWriter {
	return &outputWriter{outs: NewEnvFile(), reserved: map[string]bool{}, save: func() error { return nil }}
}

func TestOutputBounds(t *testing.T) {
	w := newOutputWriter()
	if _, err := w.Write([]byte(strings.Repeat("x", maxProvisionLine))); err != nil {
		t.Fatalf("a line of the bound: %v", err)
	}
	stopped := false
	w.stop = func() { stopped = true }
	if _, err := w.Write([]byte("x")); err == nil || !w.stopped || !stopped {
		t.Fatal("a line past the bound passed, or did not stop the run")
	}
	// The docker CLI, killed, reports its signal only: the reason is the writer's.
	if err := w.close(false); err == nil || !strings.Contains(err.Error(), "longer than") || strings.Contains(err.Error(), "no line break") {
		t.Fatalf("close after a refusal: %v", err)
	}
	// A whole line in one write is bounded too, on both sides.
	w = newOutputWriter()
	ok, long := "A="+strings.Repeat("v", maxProvisionLine-2), "B="+strings.Repeat("v", maxProvisionLine-1)
	if _, err := w.Write([]byte(ok + "\n" + long + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.close(true); err == nil || !strings.Contains(err.Error(), "line 2: longer than") {
		t.Fatalf("got %v", err)
	}
	if strings.Join(w.outs.Keys(), ",") != "A" {
		t.Fatalf("recorded %v", w.outs.Keys())
	}
	// The longest line recorded reads back.
	path := filepath.Join(t.TempDir(), ProvisionEnv)
	if err := w.outs.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	if back, err := ReadEnvFile(path); err != nil || get(back, "A") != ok[2:] {
		t.Fatalf("read back: %v", err)
	}

	w = newOutputWriter()
	line := []byte(strings.Repeat("A", 1023) + "\n")
	for range maxProvisionOut / len(line) {
		if _, err := w.Write(line); err != nil {
			t.Fatalf("within the bound: %v", err)
		}
	}
	if _, err := w.Write(line); err == nil || !w.stopped {
		t.Fatal("output past the bound passed")
	}

	w = newOutputWriter()
	saves := 0
	w.save = func() error { saves++; return nil }
	for i := range maxProvisionKeys + 1 {
		fmt.Fprintf(w, "K%d=v\nK%d=v\n", i, i)
	}
	if err := w.close(true); err == nil || !strings.Contains(err.Error(), "more than 256 outputs") || len(w.outs.Keys()) != maxProvisionKeys {
		t.Fatalf("got %v, %d keys", err, len(w.outs.Keys()))
	}
	if saves != maxProvisionKeys {
		t.Fatalf("%d saves for %d values: an unchanged value was saved again", saves, maxProvisionKeys)
	}

	// The total outputs recorded, across runs: up to the bound, then refused.
	w = newOutputWriter()
	chunk := strings.Repeat("v", maxProvisionLine-16)
	n := 0
	for ; (n+1)*(len(chunk)+3) <= maxProvisionTotal; n++ {
		fmt.Fprintf(w, "K%02d=%s\n", n, chunk)
	}
	room := maxProvisionTotal - w.size() - 3 // the key, K<nn>
	fmt.Fprintf(w, "K%02d=%s\n", n, strings.Repeat("v", room))
	if err := w.close(true); err != nil || w.size() != maxProvisionTotal {
		t.Fatalf("up to the bound: %v, %d bytes", err, w.size())
	}
	fmt.Fprintf(w, "K%02d=%sv\n", n, strings.Repeat("v", room))
	if err := w.close(true); err == nil || !strings.Contains(err.Error(), "would pass") {
		t.Fatalf("past the bound: %v", err)
	}
}

// FuzzOutputRoundTrip: whatever the hook prints, every value recorded reads back unchanged from
// the saved file.
func FuzzOutputRoundTrip(f *testing.F) {
	for _, s := range []string{"A=1\n", "A='x'\n", "A=$x#y\r\nB=\n", "a=1\n", "=\n", "A=\x00\n", "A=" + strings.Repeat("z", maxProvisionLine)} {
		f.Add(s)
	}
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, out string) {
		w := newOutputWriter()
		_, _ = w.Write([]byte(out))
		_ = w.close(true)
		path := filepath.Join(dir, ProvisionEnv)
		if err := w.outs.WriteFile(path); err != nil {
			t.Fatalf("a recorded value cannot be saved: %v", err)
		}
		back, err := ReadEnvFile(path)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		for _, k := range w.outs.Keys() {
			if get(back, k) != get(w.outs, k) {
				t.Fatalf("%s: %q read back as %q", k, get(w.outs, k), get(back, k))
			}
		}
	})
}

func TestNoProvisionSkipsTheHook(t *testing.T) {
	e := newEnv(t)
	if err := e.install(func(o *Options) { o.NoProvision = true }); err != nil {
		t.Fatal(err)
	}
	if n := len(e.hookCalls()); n != 0 {
		t.Fatalf("%d hook runs", n)
	}
	if !strings.Contains(e.log.String(), "notes-stow provision --dir") {
		t.Errorf("log: %s", e.log)
	}
	// Not provisioned by choice: the install is finished all the same.
	if _, err := os.Stat(filepath.Join(e.dir, installingFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an install not provisioned by choice is marked as not finished")
	}
}

func TestProvisionSetCorrectsOnlyAnInputOnlyTheHookReads(t *testing.T) {
	e := newEnv(t)
	e.r.Spec.Inputs = append(e.r.Spec.Inputs,
		spec.Input{Key: "LOG_LEVEL", Default: "info"}, // the compose file reads ${LOG_LEVEL:-info}
		spec.Input{Key: "BARE", Default: "x"})
	e.r.Compose = append(e.r.Compose, []byte("# api passes it by name:\n#   environment: [BARE]\n")...)
	if err := e.install(func(o *Options) { o.NoProvision = true }); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(e.dir, ".env"))
	for set, want := range map[string]string{
		"POSTGRES_PASSWORD": "is neither an input only the provision hook reads (ADMIN_EMAIL) nor a value it returned or declares it returns (none)",
		"SECURE_COOKIES":    "neither an input only",
		"NOPE":              "neither an input only",
		"LOG_LEVEL":         "neither an input only",
		"BARE":              "neither an input only",
	} {
		err := e.installer(func(o *Options) { o.Set = map[string]string{set: "x"} }).Provision(context.Background())
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("--set %s: %v", set, err)
		}
	}
	for v, want := range map[string]string{"": "ADMIN_EMAIL is required", "a'b": "ADMIN_EMAIL:"} {
		err := e.installer(func(o *Options) { o.Set = map[string]string{"ADMIN_EMAIL": v} }).Provision(context.Background())
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("--set ADMIN_EMAIL=%q: %v", v, err)
		}
	}
	if after, _ := os.ReadFile(filepath.Join(e.dir, ".env")); string(after) != string(before) {
		t.Fatal("a refused --set rewrote .env")
	}
	if n := len(e.hookCalls()); n != 0 {
		t.Fatalf("a refused --set ran the hook %d times", n)
	}
	if err := e.installer(func(o *Options) { o.Set = map[string]string{"ADMIN_EMAIL": "fixed@example.org"} }).Provision(context.Background()); err != nil {
		t.Fatal(err)
	}
	if get(e.readEnv(), "ADMIN_EMAIL") != "fixed@example.org" || len(e.hookCalls()) != 1 {
		t.Fatal("--set was not recorded before the hook ran")
	}
	// An env_file, or a mount of .env or of the install directory, may hand the stack any input;
	// a mount of a directory inside it does not. extra goes in as one more service.
	base := []byte(strings.Replace(string(e.r.Compose), "\nvolumes:\n", "\n%s\nvolumes:\n", 1))
	set := func(extra string) error {
		e.r.Compose = []byte(fmt.Sprintf(string(base), extra))
		return e.installer(func(o *Options) { o.Set = map[string]string{"ADMIN_EMAIL": "again@example.org"} }).Provision(context.Background())
	}
	svc := func(body string) string { return "  extra:\n    image: ${STOWAGE_IMAGE_API}\n" + body }
	for _, extra := range []string{
		svc("    env_file: x\n"),
		svc("    volumes: [./.env:/app/config.env:ro]\n"),
		svc("    volumes: ['.:/etc/app']\n"),
		svc("    volumes: [./:/etc/app]\n"),
		svc("    volumes: [./.:/etc/app]\n"),
		svc("    volumes: [..:/host]\n"),
		svc("    volumes: [/:/host]\n"),
		svc("    volumes: ['${PWD}:/x']\n"),
		svc("    volumes: [~/x:/x]\n"),
		svc("    volumes: [" + e.dir + ":/x]\n"),
		svc("    volumes: [{type: bind, source: ., target: /x}]\n"),
		svc("    volumes:\n      - type: bind\n        source: . # the config\n        target: /etc/app\n"),
	} {
		if err := set(extra); err == nil || !strings.Contains(err.Error(), "a re-run, `") {
			t.Errorf("%q: %v", extra, err)
		}
	}
	base = []byte(strings.Replace(string(base), "\nvolumes:\n", "\nvolumes:\n  odd: { driver_opts: { o: bind, device: . } }\n", 1))
	if err := set(""); err == nil || !strings.Contains(err.Error(), "driver_opts") {
		t.Errorf("driver_opts: %v", err)
	}
	base = []byte(strings.Replace(string(base), "  odd: { driver_opts: { o: bind, device: . } }\n", "", 1))
	for _, extra := range []string{
		svc("    volumes: [./data:/data, uploads:/u]\n"),
		svc("    volumes: [{type: bind, source: ./data, target: /data}, {type: volume, source: uploads, target: /u}]\n"),
		svc("    # reads app.env from its image\n"),
	} {
		if err := set(extra); err != nil {
			t.Errorf("%q: %v", extra, err)
		}
	}
}

func TestProvisionSetRefusesARouteDomain(t *testing.T) {
	e := newEnv(t)
	d := e.r.Spec.Front.Routes[0].DomainInput // also a project input
	e.r.Spec.Inputs = append(e.r.Spec.Inputs, spec.Input{Key: "NOTE", Default: "x"}, spec.Input{Key: d, Prompt: "Domain", When: "tls"})
	e.r.Spec.Hooks.Provision.Config["env"] = []any{"ADMIN_EMAIL", "NOTE", d}
	if err := e.install(func(o *Options) {
		o.NoProvision = true
		o.TLS = "internal"
		o.Domains = map[string]string{"app": "app.example.org", "api": "api.example.org"}
	}); err != nil {
		t.Fatal(err)
	}
	err := e.installer(func(o *Options) { o.Set = map[string]string{d: "new.example.org"} }).Provision(context.Background())
	if err == nil || !strings.Contains(err.Error(), "neither an input only") {
		t.Fatalf("--set %s: %v", d, err)
	}
	if err := e.installer(func(o *Options) { o.Set = map[string]string{"NOTE": "y"} }).Provision(context.Background()); err != nil {
		t.Fatalf("--set NOTE: %v", err)
	}
}

// An input the hook's env does not list is not one only the hook reads: --set refuses it.
func TestProvisionSetRefusesAnInputTheHookDoesNotRead(t *testing.T) {
	e := newEnv(t)
	e.r.Spec.Inputs = append(e.r.Spec.Inputs, spec.Input{Key: "NOTE", Default: "x"})
	if err := e.install(func(o *Options) { o.NoProvision = true }); err != nil {
		t.Fatal(err)
	}
	err := e.installer(func(o *Options) { o.Set = map[string]string{"NOTE": "y"} }).Provision(context.Background())
	if err == nil || !strings.Contains(err.Error(), "NOTE is neither an input only the provision hook reads (ADMIN_EMAIL)") {
		t.Fatalf("--set NOTE: %v", err)
	}
}

// --set replaces a value the hook returned before, recorded before the hook runs; an empty one
// forgets it. A key the install records never counts as one, even when provision.env holds it.
func TestProvisionSetReplacesAValueTheHookReturned(t *testing.T) {
	e := newEnv(t)
	e.docker.hookOut = "ADMIN_PASSWORD=minted\nTOKEN=t1\n"
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	e.docker.hookOut = ""
	envBefore, _ := os.ReadFile(filepath.Join(e.dir, ".env"))
	err := e.installer(func(o *Options) { o.Set = map[string]string{"TOKEN": "", "ADMIN_PASSWORD": "chosen"} }).Provision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(e.docker.hookEnv, " ") != "ADMIN_PASSWORD='chosen'" {
		t.Fatalf("the hook was given %v", e.docker.hookEnv)
	}
	if envAfter, _ := os.ReadFile(filepath.Join(e.dir, ".env")); string(envAfter) != string(envBefore) {
		t.Error("--set of a returned value rewrote .env")
	}
	if !strings.Contains(e.log.String(), "recorded ADMIN_PASSWORD, TOKEN in ") {
		t.Errorf("log: %s", e.log)
	}
	for set, want := range map[string]string{
		"TOKEN":          "TOKEN is neither an input only the provision hook reads (ADMIN_EMAIL) nor a value it returned or declares it returns (ADMIN_PASSWORD)",
		"SESSION_SECRET": "SESSION_SECRET is neither",
	} {
		err := e.installer(func(o *Options) { o.Set = map[string]string{set: "x"} }).Provision(context.Background())
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("--set %s: %v", set, err)
		}
	}
	err = e.installer(func(o *Options) { o.Set = map[string]string{"ADMIN_PASSWORD": "a'b"} }).Provision(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ADMIN_PASSWORD: a value cannot hold a single quote") {
		t.Errorf("an unrecordable value: %v", err)
	}
	// A key the install records that provision.env holds too (left by an edit) is not the hook's.
	raw, _ := os.ReadFile(filepath.Join(e.dir, ProvisionEnv))
	_ = os.WriteFile(filepath.Join(e.dir, ProvisionEnv), append(raw, []byte("SESSION_SECRET='x'\n")...), 0o600)
	err = e.installer(func(o *Options) { o.Set = map[string]string{"SESSION_SECRET": "y"} }).Provision(context.Background())
	if err == nil || !strings.Contains(err.Error(), "SESSION_SECRET is neither") {
		t.Errorf("--set of a recorded key: %v", err)
	}
}

func TestProvisionNeedsAnInstallAndAHook(t *testing.T) {
	e := newEnv(t)
	if err := e.installer(nil).Provision(context.Background()); err == nil || !strings.Contains(err.Error(), "holds no install") {
		t.Fatalf("no install: %v", err)
	}
	e.r.Spec.Hooks.Provision = nil
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, ProvisionCompose)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a project without a hook got its service")
	}
	if err := e.installer(nil).Provision(context.Background()); err == nil || !strings.Contains(err.Error(), "no provision hook") {
		t.Fatalf("no hook: %v", err)
	}
}

// A CRLF line at the line bound, its '\n' arriving in the next write, is recorded: the parse trims
// the '\r', so the buffer one past the bound by it alone is not refused.
func TestACRLFLineAtTheBoundSplitBeforeItsLF(t *testing.T) {
	t.Parallel()
	w := newOutputWriter()
	line := "K=" + strings.Repeat("v", maxProvisionLine-2) + "\r"
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.close(true); err != nil {
		t.Fatal(err)
	}
	if v, _ := w.outs.Get("K"); len(v) != maxProvisionLine-2 {
		t.Fatalf("recorded %d bytes", len(v))
	}
}

// --set sets a key the hook declares it returns although it never returned it, recorded in
// provision.env before the hook runs and handed to the next run; an empty one leaves it forgotten.
// A declared key the install records is refused: the install's value is the one it holds.
func TestProvisionSetGivesADeclaredValueTheHookNeverReturned(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.r.Spec.Hooks.Provision.Config["returns"] = []any{"ADMIN_KEY", "SESSION_SECRET"}
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if err := e.installer(func(o *Options) { o.Set = map[string]string{"ADMIN_KEY": "given"} }).Provision(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(e.docker.hookEnv, " ") != "ADMIN_KEY='given'" {
		t.Fatalf("the hook was given %v", e.docker.hookEnv)
	}
	if err := e.installer(nil).Provision(context.Background()); err != nil || strings.Join(e.docker.hookEnv, " ") != "ADMIN_KEY='given'" {
		t.Fatalf("the next run was given %v (%v)", e.docker.hookEnv, err)
	}
	if err := e.installer(func(o *Options) { o.Set = map[string]string{"ADMIN_KEY": ""} }).Provision(context.Background()); err != nil || len(e.docker.hookEnv) != 0 {
		t.Fatalf("--set ADMIN_KEY= gave the hook %v (%v)", e.docker.hookEnv, err)
	}
	if err := e.installer(func(o *Options) { o.Set = map[string]string{"ADMIN_KEY": ""} }).Provision(context.Background()); err != nil || len(e.docker.hookEnv) != 0 {
		t.Fatalf("--set ADMIN_KEY= of a forgotten key: %v (%v)", e.docker.hookEnv, err)
	}
	err := e.installer(func(o *Options) { o.Set = map[string]string{"SESSION_SECRET": "x"} }).Provision(context.Background())
	if err == nil || !strings.Contains(err.Error(), "SESSION_SECRET is neither an input only the provision hook reads (ADMIN_EMAIL) nor a value it returned or declares it returns (ADMIN_KEY)") {
		t.Errorf("--set of a declared key the install records: %v", err)
	}
	// A failure names --set for the declared key, which the hook has not returned.
	e.docker.hookErr = errors.New("exit status 1")
	err = e.installer(nil).Provision(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sets a value it returned or declares it returns (ADMIN_KEY) first") {
		t.Errorf("the failure: %v", err)
	}
}
