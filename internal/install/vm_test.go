package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/backup"
	"github.com/norvexlabs/stowage/internal/front"
	"github.com/norvexlabs/stowage/internal/kairos"
	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// bundledInstaller builds the <short>-stow an OS bundle of examples/minimal at version carries: its
// release, and the file carrying it.
func bundledInstaller(t *testing.T, version string, revision int, mod func(*spec.Spec)) (*Release, string) {
	t.Helper()
	root := testutil.CopyExample(t, "minimal")
	p, err := project.Open(root, "")
	if err != nil {
		t.Fatal(err)
	}
	view := p.Spec.BundleView()
	if mod != nil {
		mod(&view)
	}
	specJSON, _ := json.Marshal(view)
	images, configs, manifests := map[string]string{}, map[string]string{}, map[string]string{}
	for _, k := range append(p.Spec.ImageNames(), front.Key, backup.Key) {
		images[k], configs[k], manifests[k] = digest, config, manifest
	}
	m := &release.Manifest{SchemaVersion: release.SchemaVersion, Name: "notes", ShortName: "notes", ProjectVersion: version, Revision: revision, Inputs: "sha256:os",
		Scheme: release.Scheme{Provider: "semver"}, Artifacts: release.Artifacts{Images: images, Configs: configs, Manifests: manifests}}
	man, err := m.EncodeBundled()
	if err != nil {
		t.Fatal(err)
	}
	compose, _ := os.ReadFile(filepath.Join(root, "deploy/compose.yaml"))
	var buf bytes.Buffer
	if err := payload.Write(&buf, strings.NewReader("exe"), payload.Files{payload.Bundled: man, payload.Spec: specJSON, payload.Compose: compose}); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "notes-stow")
	_ = os.WriteFile(exe, buf.Bytes(), 0o755)
	files, err := payload.Read(exe)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Load(files)
	if err != nil {
		t.Fatal(err)
	}
	return r, exe
}

// preloadRef is where the OS bundle preloads an image of examples/minimal.
func preloadRef(key string) string {
	return "ghcr.io/acme/notes/notes-" + key + ":" + release.PreloadTag(digest)
}

// vmEnv is a VM whose OS carries examples/minimal at version, its images preloaded.
func vmEnv(t *testing.T, version string) *env {
	t.Helper()
	e := newEnv(t)
	e.r, e.self = bundledInstaller(t, version, 1, nil)
	for _, k := range []string{"api", "web", "postgres", front.Key} {
		e.docker.loaded[preloadRef(k)] = digest
	}
	return e
}

// vm is the options the VM's <short>-stow runs with.
// vm makes an install a VM's; its boot id is unknown (none is journaled) unless a test sets one.
func vm(o *Options) { o.Offline, o.Preloaded, o.BootIDFile = true, true, os.DevNull }

func TestABundledReleaseLoads(t *testing.T) {
	t.Parallel()
	r, _ := bundledInstaller(t, "1.0.0", 2, nil)
	if !r.Manifest.Bundled() || r.Manifest.ProjectVersion != "1.0.0" || r.Manifest.Revision != 2 {
		t.Fatalf("loaded %+v", r.Manifest)
	}
	// A payload carrying both manifests is refused.
	_, exe := bundledInstaller(t, "1.0.0", 1, nil)
	files, err := payload.Read(exe)
	if err != nil {
		t.Fatal(err)
	}
	files[payload.Release] = files[payload.Bundled]
	if _, err := Load(files); err == nil || !strings.Contains(err.Error(), "both") {
		t.Errorf("both manifests: %v", err)
	}
	delete(files, payload.Release)
	delete(files, payload.Bundled)
	if _, err := Load(files); err == nil || !strings.Contains(err.Error(), "no release.json") {
		t.Errorf("no manifest: %v", err)
	}
}

func TestTheBundledInstallerIsTheVMsCommandLine(t *testing.T) {
	t.Parallel()
	r, self := bundledInstaller(t, "1.0.0", 1, nil)
	root := newCommand(r, self, &bytes.Buffer{}, &bytes.Buffer{}, K8sOptions{}, func(bool) Prompter { return nil })
	var verbs []string
	for _, c := range root.Commands() {
		verbs = append(verbs, c.Name())
	}
	for _, want := range []string{"install", "boot", "boot-rollback", "hostd", "status", "ps", "logs", "restart", "stop", "start", "provision", "updates", "upgrade", "reboot", "shutdown",
		"health", "health-gate", "rollback"} {
		if !slices.Contains(verbs, want) {
			t.Errorf("no %s verb: %v", want, verbs)
		}
	}
	for _, not := range []string{"k8s", payload.ScriptCommand} {
		if slices.Contains(verbs, not) {
			t.Errorf("the VM's <short>-stow has %s", not)
		}
	}
	// A published installer keeps the Docker shape's.
	pr, pself := testInstaller(t, "1.0.0")
	if c, _, err := newCommand(pr, pself, &bytes.Buffer{}, &bytes.Buffer{}, K8sOptions{}, nil).Find([]string{"hostd"}); err == nil && c.Name() == "hostd" {
		t.Error("a published installer has the hostd verb")
	}
}

func TestAVMInstallRunsThePreloadedImagesByTheirDigestTag(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	if err := e.install(func(o *Options) { vm(o); o.Channel = "stable" }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	f := e.readEnv()
	for _, k := range []string{"api", "web", "postgres"} {
		if got := get(f, ImageVar(k)); got != preloadRef(k) {
			t.Errorf("%s = %s, want %s", ImageVar(k), got, preloadRef(k))
		}
	}
	if got := get(f, FrontImageVar); got != preloadRef(front.Key) {
		t.Errorf("front = %s", got)
	}
	if get(f, ChannelVar) != "stable" {
		t.Errorf("channel = %q", get(f, ChannelVar))
	}
	if len(e.docker.ran("pull")) > 0 {
		t.Error("a VM install pulled")
	}
}

func TestAVMInstallRefusesAnImageThatIsNotTheReleases(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	e.docker.loaded[preloadRef("api")] = "sha256:" + strings.Repeat("e", 64)
	if err := e.install(vm); err == nil || !strings.Contains(err.Error(), "not the release's image") {
		t.Fatalf("err = %v", err)
	}
	e.nothingWritten()
}

func TestAVMInstallMissingAnImageNamesThePreloadUnit(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	delete(e.docker.loaded, preloadRef("api"))
	err := e.install(vm)
	if err == nil || !strings.Contains(err.Error(), "which notes-preload.service loads from the OS") || strings.Contains(err.Error(), "offline install") {
		t.Fatalf("err = %v", err)
	}
	e.nothingWritten()
}

// preloadHost is systemd with the preload unit in state (empty: no such unit; showErr: no
// systemctl), whose `systemctl start` answers err; it records each command.
type preloadHost struct {
	state   string
	showErr error
	err     error
	// notBooted: systemctl is there, systemd is not the init system.
	notBooted bool
	ran       [][]string
}

func (h *preloadHost) Run(_ context.Context, c Cmd) ([]byte, error) {
	h.ran = append(h.ran, c.Args)
	if c.Args[0] == "test" { // sd_booted
		if h.notBooted {
			return nil, errors.New("exit status 1")
		}
		return nil, nil
	}
	if c.Args[1] == "show" {
		if h.showErr != nil {
			return nil, h.showErr
		}
		if h.state == "" {
			return []byte("LoadState=not-found\nActiveState=inactive\n"), nil
		}
		return []byte("LoadState=loaded\nActiveState=" + h.state + "\n"), nil
	}
	return nil, h.err
}

func TestAnOperatorsInstallWaitsForThePreloadUnit(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	await := func(h *preloadHost) error {
		return e.installer(func(o *Options) { vm(o); o.Host = h }).awaitPreload(context.Background())
	}
	start := []string{"systemctl", "start", "notes-preload.service"}
	for _, state := range []string{"activating", "active"} {
		h := &preloadHost{state: state}
		if err := await(h); err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		if len(h.ran) != 2 || !slices.Equal(h.ran[1], start) {
			t.Errorf("%s: ran %q, want the state then %q", state, h.ran, start)
		}
	}
	if !strings.Contains(e.log.String(), "waiting for notes-preload.service (loading the release's images)…") {
		t.Errorf("log: %s", e.log)
	}
	// Failed already: with every image loaded it is not run again, saying so; with one missing it is.
	h := &preloadHost{state: "failed"}
	if err := await(h); err != nil || len(h.ran) != 1 || !strings.Contains(e.log.String(), "notes-preload.service failed, but every image of the release is loaded: it is not run again") {
		t.Errorf("failed, every image loaded: %v, ran %q\n%s", err, h.ran, e.log)
	}
	delete(e.docker.loaded, preloadRef("web"))
	h = &preloadHost{state: "failed"}
	if err := await(h); err != nil || len(h.ran) != 2 || !slices.Equal(h.ran[1], start) {
		t.Errorf("failed, an image missing: %v, ran %q", err, h.ran)
	}
	// A preload that fails again does not refuse by itself: the images decide (below).
	if err := await(&preloadHost{state: "failed", err: errors.New("exit status 1")}); err != nil {
		t.Errorf("a preload that fails: %v", err)
	}
	// No such unit, or no systemd: no boot loads the images there, and nothing is started.
	for name, h := range map[string]*preloadHost{"no unit": {}, "no systemd": {showErr: fmt.Errorf("systemctl show: %w", exec.ErrNotFound)}} {
		if err := await(h); err != nil || len(h.ran) != 1 {
			t.Errorf("%s: err %v, ran %q", name, err, h.ran)
		}
	}
	// Another failure to read the state is said, where systemd runs the host.
	if err := await(&preloadHost{showErr: errors.New("exit status 4")}); err == nil || !strings.Contains(err.Error(), "the state of notes-preload.service") {
		t.Errorf("an unreadable state: %v", err)
	}
	// systemctl, but not systemd as init (a container): no boot loads the images, nothing is waited for.
	h = &preloadHost{showErr: errors.New("System has not been booted with systemd as init system (PID 1). Can't operate."), notBooted: true}
	if err := await(h); err != nil || slices.ContainsFunc(h.ran, func(a []string) bool { return a[1] == "start" }) {
		t.Errorf("not booted with systemd: %v, ran %q", err, h.ran)
	}
}

func TestBootWithoutAnInstallDoesNothing(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	none := filepath.Join(t.TempDir(), "install.yaml")
	if err := e.installer(func(o *Options) { vm(o); o.Settings = none }).Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.docker.calls) > 0 || !strings.Contains(e.log.String(), "holds no install yet") {
		t.Errorf("calls %v, log %s", e.docker.calls, e.log)
	}
	e.nothingWritten()
}

func TestBootBringsTheSameReleaseUpAgain(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	e.docker.calls = nil
	in := e.installer(vm)
	in.O.Exec = func(context.Context, string, []string) error { t.Error("ran another installer"); return nil }
	if err := in.Boot(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if len(e.docker.ran("compose")) == 0 {
		t.Error("the stack was not brought up")
	}
}

func TestAVMReRunChangesAnInputThatEveryBootKeeps(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	if err := e.install(func(o *Options) { vm(o); o.Set = map[string]string{"ADMIN_EMAIL": "ops@example.org"} }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	in := e.installer(vm)
	in.O.Exec = func(context.Context, string, []string) error { t.Error("ran another installer"); return nil }
	if err := in.Boot(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if got := get(e.readEnv(), "ADMIN_EMAIL"); got != "ops@example.org" {
		t.Fatalf("ADMIN_EMAIL after a boot = %q", got)
	}
}

// vmUpgraded is a VM installed at 1.0.0, then booted on an OS carrying 2.0.0.
func vmUpgraded(t *testing.T) (*env, [][]string) {
	t.Helper()
	e := vmEnv(t, "1.0.0")
	e.docker.dump = "DUMP"
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	ctl := filepath.Join(e.dir, "notes-stow")
	e.r, e.self = bundledInstaller(t, "2.0.0", 1, nil)
	var ran [][]string
	in := e.installer(vm)
	in.O.Exec = func(_ context.Context, exe string, args []string) error {
		if exe != ctl {
			t.Errorf("ran %s, want the install's own %s", exe, ctl)
		}
		ran = append(ran, args)
		return nil
	}
	if err := in.Boot(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	return e, ran
}

func TestBootOnANewerOSUpgradesTheInstall(t *testing.T) {
	t.Parallel()
	e, ran := vmUpgraded(t)
	// The install's own installer brought its stack up first, for the dump.
	if want := [][]string{{"boot", "--dir", e.dir}}; !slices.EqualFunc(ran, want, slices.Equal) {
		t.Errorf("ran %v, want %v", ran, want)
	}
	if v := get(e.readEnv(), "STOWAGE_PROJECT_VERSION"); v != "2.0.0" {
		t.Errorf("the install runs %s", v)
	}
	if read(t, filepath.Join(e.dir, "notes-stow")) != read(t, e.self) {
		t.Error("the install does not keep the OS's installer")
	}
	if _, err := os.Stat(filepath.Join(e.dir, PrevDir, prevState)); err != nil {
		t.Errorf("the replaced release is not kept: %v", err)
	}
}

func TestBootOnTheReplacedReleaseGoesBackWithTheInstallsInstaller(t *testing.T) {
	t.Parallel()
	e, _ := vmUpgraded(t)
	e.r, e.self = bundledInstaller(t, "1.0.0", 1, nil) // Kairos booted the passive slot
	before := read(t, filepath.Join(e.dir, ".env"))
	var ran [][]string
	in := e.installer(vm)
	in.O.Exec = func(_ context.Context, exe string, args []string) error {
		if exe != filepath.Join(e.dir, "notes-stow") {
			t.Errorf("ran %s", exe)
		}
		ran = append(ran, args)
		return nil
	}
	if err := in.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := [][]string{{"boot-rollback", "--dir", e.dir}}; !slices.EqualFunc(ran, want, slices.Equal) {
		t.Errorf("ran %v, want %v", ran, want)
	}
	if read(t, filepath.Join(e.dir, ".env")) != before {
		t.Error("the OS's installer changed the install itself")
	}
}

func TestBootStopsWhenTheInstallsStackDoesNotComeUp(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	e.r, e.self = bundledInstaller(t, "2.0.0", 1, nil)
	in := e.installer(vm)
	in.O.Exec = func(context.Context, string, []string) error { return errors.New("compose up failed") }
	if err := in.Boot(context.Background()); err == nil || !strings.Contains(err.Error(), "before the upgrade") {
		t.Fatalf("err = %v", err)
	}
	if v := get(e.readEnv(), "STOWAGE_PROJECT_VERSION"); v != "1.0.0" {
		t.Errorf("the install runs %s", v)
	}
}

func TestBootRefusesAnotherProjectsInstall(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	e.r, e.self = bundledInstaller(t, "2.0.0", 1, nil) // an upgrade would run the install's installer first
	f := e.readEnv()
	f.Set("STOWAGE_NAME", "other")
	if err := f.WriteFile(filepath.Join(e.dir, ".env")); err != nil {
		t.Fatal(err)
	}
	in := e.installer(vm)
	in.O.Exec = func(context.Context, string, []string) error { t.Error("ran the install's installer"); return nil }
	if err := in.Boot(context.Background()); err == nil || !strings.Contains(err.Error(), `"other"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestBootDoesNotUpgradeAgainToTheReleaseARollbackLeft(t *testing.T) {
	t.Parallel()
	e, _ := vmUpgraded(t)
	// 2.0.0's installer goes back (as boot-rollback runs it).
	if err := e.installer(vm).Rollback(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if v := get(e.readEnv(), "STOWAGE_PROJECT_VERSION"); v != "1.0.0" {
		t.Fatalf("the rollback left %s", v)
	}
	// The VM boots the 2.0.0 slot again: the install stays on 1.0.0, brought up by its own installer.
	var ran [][]string
	in := e.installer(vm)
	in.O.Exec = func(_ context.Context, _ string, args []string) error { ran = append(ran, args); return nil }
	if err := in.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := [][]string{{"boot", "--dir", e.dir}}; !slices.EqualFunc(ran, want, slices.Equal) {
		t.Errorf("ran %v, want %v", ran, want)
	}
	if v := get(e.readEnv(), "STOWAGE_PROJECT_VERSION"); v != "1.0.0" || !strings.Contains(e.log.String(), "went back from notes 2.0.0") {
		t.Errorf("the install runs %s\n%s", v, e.log)
	}
}

func TestBootFinishesAnUpgradeARebootStoppedBeforeItSwitched(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	// The upgrade kept 1.0.0 in PrevDir, then the VM went down before .env switched.
	in := e.installer(vm)
	env, err := in.open()
	if err != nil {
		t.Fatal(err)
	}
	if err := in.keepPrev(env, state{ProjectVersion: "1.0.0", Revision: 1}, &keptRelease{Manifest: in.R.Manifest, Release: in.R}); err != nil {
		t.Fatal(err)
	}
	e.r, e.self = bundledInstaller(t, "2.0.0", 1, nil)
	var ran [][]string
	boot := e.installer(vm)
	boot.O.Exec = inProcess(e, vm, &ran)
	e.docker.calls = nil
	if err := boot.Boot(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	// Finishing starts the kept release with its own installer: a VM's boots it.
	if fmt.Sprint(ran) != fmt.Sprintf("[[boot-rollback --dir %s] [boot --dir %s] [boot --dir %s]]", e.dir, e.dir, e.dir) {
		t.Errorf("ran %v", ran)
	}
	// Finishing checks Docker first, and nothing else, so it brings no database up first.
	if first := e.docker.calls[0].Args; first[0] != "version" {
		t.Errorf("the finishing run's first call: %v", first)
	}
	for _, c := range e.docker.ran("compose") {
		if strings.HasSuffix(strings.Join(c.Args, " "), "up --detach --wait postgres") {
			t.Error("finishing brought the database up for a check")
		}
	}
	if v := get(e.readEnv(), "STOWAGE_PROJECT_VERSION"); v != "2.0.0" {
		t.Errorf("the install runs %s", v)
	}
}

func TestBootFinishesARollbackARebootStoppedAfterItSwitched(t *testing.T) {
	t.Parallel()
	e, _ := vmUpgraded(t)
	v2 := read(t, e.self)
	// The rollback switched .env to 1.0.0, then the VM went down: 2.0.0's installer is still kept.
	prevEnv := read(t, filepath.Join(e.dir, PrevDir, ".env"))
	if err := os.WriteFile(filepath.Join(e.dir, ".env"), []byte(prevEnv), 0o600); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(e.dir, "notes-stow")) != v2 {
		t.Fatal("not 2.0.0's installer")
	}
	e.r, e.self = bundledInstaller(t, "1.0.0", 1, nil) // the VM booted the passive slot
	var ran [][]string
	boot := e.installer(vm)
	boot.O.Exec = inProcess(e, vm, &ran)
	if err := boot.Boot(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if fmt.Sprint(ran) != fmt.Sprintf("[[boot-rollback --dir %s] [boot --dir %s]]", e.dir, e.dir) {
		t.Errorf("ran %v", ran)
	}
	if v := get(e.readEnv(), "STOWAGE_PROJECT_VERSION"); v != "1.0.0" {
		t.Errorf("the install runs %s", v)
	}
	if read(t, filepath.Join(e.dir, "notes-stow")) == v2 {
		t.Error("2.0.0's installer is still kept")
	}
	if _, err := os.Stat(filepath.Join(e.dir, PrevDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("PrevDir is left: %v", err)
	}
}

func TestBootNeverRunsAnInstallerThatIsNotTheInstalls(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	e.r, e.self = bundledInstaller(t, "2.0.0", 1, nil)
	// The install's installer is 2.0.0's, though it runs 1.0.0: running it would boot again.
	if err := os.WriteFile(filepath.Join(e.dir, "notes-stow"), []byte(read(t, e.self)), 0o755); err != nil {
		t.Fatal(err)
	}
	in := e.installer(vm)
	in.O.Exec = func(context.Context, string, []string) error { t.Error("ran an installer"); return nil }
	if err := in.Boot(context.Background()); err == nil || !strings.Contains(err.Error(), "is not the installer of notes 1.0.0") {
		t.Fatalf("err = %v", err)
	}
}

func TestBootRollbackBringsTheDatabaseUpFirst(t *testing.T) {
	t.Parallel()
	e, _ := vmUpgraded(t)
	e.docker.calls = nil
	if err := e.installer(vm).BootRollback(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	var ups []string
	for _, c := range e.docker.ran("compose") {
		if a := strings.Join(c.Args, " "); strings.Contains(a, " up ") {
			ups = append(ups, a)
		}
	}
	if len(ups) < 2 || !strings.HasSuffix(ups[0], " up --detach --wait postgres") {
		t.Errorf("compose up calls: %q", ups)
	}
	if v := get(e.readEnv(), "STOWAGE_PROJECT_VERSION"); v != "1.0.0" {
		t.Errorf("the install runs %s", v)
	}

	// A database that does not come up refuses before anything changes.
	e, _ = vmUpgraded(t)
	e.docker.upErr = errors.New("unhealthy")
	if err := e.installer(vm).BootRollback(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot be checked") {
		t.Fatalf("err = %v", err)
	}
	if v := get(e.readEnv(), "STOWAGE_PROJECT_VERSION"); v != "2.0.0" {
		t.Errorf("the install runs %s", v)
	}
}

func TestARerunRefusesAChannel(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	if err := e.install(func(o *Options) { vm(o); o.Channel = "stable" }); err == nil || !strings.Contains(err.Error(), "--channel") {
		t.Fatalf("err = %v", err)
	}
}

func TestTheBootWaitsForTheLockAnOperatorsRunHolds(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockFile(filepath.Join(e.dir, lifecycleLock))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.installer(vm).Boot(context.Background()); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("without a wait: %v", err)
	}
	released := make(chan struct{})
	go func() { time.Sleep(50 * time.Millisecond); unlock(); close(released) }()
	in := e.installer(vm)
	in.O.LockWait = 5 * time.Second
	if err := in.Boot(context.Background()); err != nil {
		t.Fatalf("waiting: %v", err)
	}
	<-released
}

// settings writes a settings file as a VM's cloud-config does, and returns the options a run reads
// it with.
func settings(t *testing.T, content string) func(*Options) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "install.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return func(o *Options) { vm(o); o.Settings = path }
}

func TestTheFirstBootInstallsWithTheCloudConfigsSettings(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	e.vars = nil // nothing from the environment: the settings give every input
	with := settings(t, "channel: beta\ntls: internal\nhttps_port: 8443\ninputs:\n  ADMIN_EMAIL: ops@example.org\n  APP_DOMAIN: app.example.org\n  API_DOMAIN: api.example.org\n")
	if err := e.installer(with).Boot(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	f := e.readEnv()
	for k, want := range map[string]string{ChannelVar: "beta", "STOWAGE_TLS_MODE": "internal", HTTPSPortVar: "8443", "ADMIN_EMAIL": "ops@example.org", "APP_DOMAIN": "app.example.org"} {
		if got := get(f, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	// Booting again brings the install up; the settings are not read again.
	e.docker.calls = nil
	in := e.installer(settings(t, "tls: none\n"))
	in.O.Exec = func(context.Context, string, []string) error { t.Error("ran another installer"); return nil }
	if err := in.Boot(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if get(e.readEnv(), "STOWAGE_TLS_MODE") != "internal" {
		t.Error("a later boot read the settings again")
	}
}

func TestTheSettingsFileIsStrictAndCarriesNoSecret(t *testing.T) {
	t.Parallel()
	secret := func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "API_KEY", Prompt: "API key", Secret: true, Required: true},
			spec.Input{Key: "TLS_KEY", Prompt: "TLS key", Secret: true, When: "tls"})
	}
	for _, c := range []struct{ name, content, want string }{
		{"unknown key", "chanel: beta\n", "field chanel not found"},
		{"bad channel", "channel: Beta Channel\n", "is not a channel name"},
		{"byo", "tls: byo\n", "give it over SSH"},
		{"secret input", "inputs: { API_KEY: k }\n", "API_KEY is a secret input"},
		{"secret of another mode", "inputs: { STOWAGE_CLOUDFLARE_API_TOKEN: t }\n", "STOWAGE_CLOUDFLARE_API_TOKEN is a secret input"},
		{"secret input asked with TLS", "inputs: { TLS_KEY: k, APP_DOMAIN: a.example.org }\n", "TLS_KEY is a secret input"},
		{"unknown input", "inputs: { ADMIN_MAIL: a@example.org }\n", "no input of this install is named ADMIN_MAIL"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := vmEnv(t, "1.0.0")
			e.r, e.self = bundledInstaller(t, "1.0.0", 1, secret)
			err := e.installer(settings(t, c.content)).Boot(context.Background())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			e.nothingWritten()
		})
	}
}

func TestTheFirstBootWaitsForASecretInputOverSSH(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	e.r, e.self = bundledInstaller(t, "1.0.0", 1, func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "API_KEY", Prompt: "API key", Secret: true, Required: true})
	})
	e.vars = nil
	with := settings(t, "inputs: { ADMIN_EMAIL: ops@example.org }\n")
	if err := e.installer(with).Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.log.String(), "needs API_KEY") || !strings.Contains(e.log.String(), "sudo notes-stow install") {
		t.Errorf("log: %s", e.log)
	}
	e.nothingWritten()
	// The operator's install takes the settings, and is given the secret.
	in := e.installer(func(o *Options) { with(o); o.Set = map[string]string{"API_KEY": "k"} })
	if err := in.withSettings(); err != nil {
		t.Fatal(err)
	}
	if err := in.Install(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if f := e.readEnv(); get(f, "API_KEY") != "k" || get(f, "ADMIN_EMAIL") != "ops@example.org" {
		t.Errorf("recorded API_KEY=%q ADMIN_EMAIL=%q", get(f, "API_KEY"), get(f, "ADMIN_EMAIL"))
	}
}

func TestTheFirstBootWaitsForTheSecretsVMCreateWasGiven(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	e.vars = nil // nothing from the environment: the settings give every input
	with := settings(t, "await_secrets: true\ninputs: { ADMIN_EMAIL: ops@example.org }\n")
	if err := e.installer(with).Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.log.String(), "the first install waits for its secrets") {
		t.Errorf("log: %s", e.log)
	}
	e.nothingWritten()
	// An operator's install without them is refused: it would generate secrets the backups do not
	// match.
	if err := e.installer(with).withSettings(); err == nil || !strings.Contains(err.Error(), "`sudo notes-stow install --secrets FILE` gives them") {
		t.Errorf("without --secrets: %v", err)
	}
	// The file lost: --new-secrets generates them.
	if err := e.installer(func(o *Options) { with(o); o.NewSecrets = true }).withSettings(); err != nil {
		t.Errorf("--new-secrets: %v", err)
	}
	in := e.installer(func(o *Options) { with(o); o.Secrets = []byte("project: notes\nsecrets:\n  SESSION_SECRET: kept\n") })
	if err := in.withSettings(); err != nil {
		t.Fatal(err)
	}
	if err := in.Install(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if f := e.readEnv(); get(f, "SESSION_SECRET") != "kept" || get(f, "ADMIN_EMAIL") != "ops@example.org" {
		t.Errorf("recorded SESSION_SECRET=%q ADMIN_EMAIL=%q", get(f, "SESSION_SECRET"), get(f, "ADMIN_EMAIL"))
	}
}

func TestTheOperatorsFlagsWinOverTheSettings(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	in := e.installer(settings(t, "channel: beta\ntls: internal\ninputs: { ADMIN_EMAIL: ops@example.org, APP_DOMAIN: a.example.org, API_DOMAIN: b.example.org }\n"))
	in.O.Channel, in.O.TLS = "stable", "none"
	in.O.Answers = map[string]string{"ADMIN_EMAIL": "me@example.org"}
	if err := in.withSettings(); err != nil {
		t.Fatal(err)
	}
	if in.O.Channel != "stable" || in.O.TLS != "none" || in.O.Answers["ADMIN_EMAIL"] != "me@example.org" || in.O.Answers["APP_DOMAIN"] != "" {
		t.Errorf("options %+v", in.O)
	}
	// IPv6 from the file, unless --ipv6 says otherwise.
	v6 := e.installer(settings(t, "ipv6: true\n"))
	if err := v6.withSettings(); err != nil || v6.O.IPv6 == nil || !*v6.O.IPv6 {
		t.Errorf("ipv6 %v, %v", v6.O.IPv6, err)
	}
	off := false
	v6 = e.installer(settings(t, "ipv6: true\n"))
	v6.O.IPv6 = &off
	if err := v6.withSettings(); err != nil || *v6.O.IPv6 {
		t.Errorf("--ipv6=false over the file: %v", err)
	}
	// Over the file's HTTPS mode its domains are taken.
	https := e.installer(settings(t, "tls: internal\ninputs: { APP_DOMAIN: a.example.org }\n"))
	if err := https.withSettings(); err != nil || https.O.Answers["APP_DOMAIN"] != "a.example.org" {
		t.Errorf("answers %v, %v", https.O.Answers, err)
	}
	// A certificate decides the mode, and so does a domain given over loopback, however it is
	// given; a domain over the file's HTTPS mode keeps it.
	for _, c := range []struct {
		file, want string
		mod        func(*Options)
	}{
		{"tls: none\n", "", func(o *Options) { o.Domains = map[string]string{"app": "x.example.org"} }},
		{"tls: none\n", "", func(o *Options) { o.Set = map[string]string{"APP_DOMAIN": "x.example.org"} }},
		{"tls: none\n", "", func(o *Options) { o.Answers = map[string]string{"API_DOMAIN": "x.example.org"} }},
		{"tls: internal\n", "", func(o *Options) { o.Cert, o.Key = "c.pem", "k.pem" }},
		{"tls: internal\n", "internal", func(o *Options) { o.Domains = map[string]string{"app": "x.example.org"} }},
	} {
		in := e.installer(settings(t, c.file))
		c.mod(&in.O)
		if err := in.withSettings(); err != nil || in.O.TLS != c.want {
			t.Errorf("%s: tls %q, %v", c.file, in.O.TLS, err)
		}
	}
	// An install that exists keeps what it recorded: the settings are not applied.
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	again := e.installer(settings(t, "channel: beta\n"))
	if err := again.withSettings(); err != nil || again.O.Channel != "" {
		t.Errorf("settings applied to an existing install: %v, %q", err, again.O.Channel)
	}
}

func TestAVMInputTakesTheKairosShapes(t *testing.T) {
	t.Parallel()
	only := func(s *spec.Spec) {
		s.Inputs = append(s.Inputs,
			spec.Input{Key: "VM_ONLY", Prompt: "x", Required: true, Shapes: []string{"kairos"}},
			spec.Input{Key: "DOCKER_ONLY", Prompt: "y", Required: true, Shapes: []string{"docker"}})
	}
	e := vmEnv(t, "1.0.0")
	e.r, e.self = bundledInstaller(t, "1.0.0", 1, only)
	e.vars["VM_ONLY"] = "v"
	if err := e.install(vm); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if f := e.readEnv(); get(f, "VM_ONLY") != "v" {
		t.Error("the kairos input was not recorded")
	} else if _, ok := f.Get("DOCKER_ONLY"); ok {
		t.Error("the docker input was recorded on a VM")
	}
}

func TestAVMStackReachesTheHostDaemonThroughTheServicesNamed(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	token := get(e.readEnv(), HostTokenVar)
	if len(token) != 64 {
		t.Fatalf("token %q", token)
	}
	var fc struct {
		Services map[string]struct {
			Volumes     []string          `yaml:"volumes"`
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	raw, _ := os.ReadFile(filepath.Join(e.dir, FrontCompose))
	if err := yaml.Unmarshal(raw, &fc); err != nil {
		t.Fatal(err)
	}
	api := fc.Services["api"]
	if !slices.Equal(api.Volumes, []string{"/run/notes/app:/run/stowage-host:ro"}) ||
		api.Environment[HostSocketVar] != "/run/stowage-host/app.sock" || api.Environment[HostTokenVar] != "${STOWAGE_HOST_TOKEN}" {
		t.Errorf("api: %+v", api)
	}
	if _, ok := fc.Services["web"]; ok {
		t.Error("a service host_api does not name reaches the daemon")
	}
	// A Docker install of the same release gives no service the daemon.
	d := newEnv(t)
	d.r, d.self = e.r, e.self
	if err := d.install(nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.readEnv().Get(HostTokenVar); ok {
		t.Error("a Docker install recorded a host token")
	}
	raw, _ = os.ReadFile(filepath.Join(d.dir, FrontCompose))
	if strings.Contains(string(raw), "stowage-host") {
		t.Errorf("a Docker install mounts the daemon:\n%s", raw)
	}
}

func TestAnUpgradeToAReleaseNamingHostAPIGeneratesItsToken(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	e.r, e.self = bundledInstaller(t, "1.0.0", 1, func(s *spec.Spec) { s.Shapes.Kairos.HostAPI = nil })
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.readEnv().Get(HostTokenVar); ok {
		t.Fatal("a release naming no service recorded a token")
	}
	e.r, e.self = bundledInstaller(t, "2.0.0", 1, nil)
	if err := e.installer(vm).Upgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if len(get(e.readEnv(), HostTokenVar)) != 64 {
		t.Error("the upgrade generated no token")
	}
}

// nftHost runs nft as the VM's firewall would, failing with err.
type nftHost struct {
	ran [][]string
	err error
	// listErr fails `nft list`; failOn fails a command whose last argument ends with it.
	listErr error
	failOn  string
}

func (h *nftHost) Run(_ context.Context, c Cmd) ([]byte, error) {
	h.ran = append(h.ran, c.Args)
	switch {
	case len(c.Args) > 1 && c.Args[1] == "list":
		return nil, h.listErr
	case h.failOn != "" && strings.HasSuffix(c.Args[len(c.Args)-1], h.failOn):
		return nil, errors.New("syntax error")
	}
	return nil, h.err
}

func TestEveryStartOpensTheFrontsPortsInTheFirewall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := &nftHost{}
	in := &Installer{R: testRelease(t, "1.0.0"), O: Options{Dir: t.TempDir(), Firewall: filepath.Join(t.TempDir(), "nftables.d"), Host: host}}
	file := filepath.Join(in.O.Firewall, "notes-ports.nft")
	env := NewEnvFile()
	env.Set("STOWAGE_TLS_MODE", "internal")
	env.Set(HTTPPortVar, "80")
	env.Set(HTTPSPortVar, "8443")
	reloads := func(n int, when string) {
		t.Helper()
		if len(host.ran) != n || !slices.Equal(host.ran[n-1], []string{"nft", "-f", "/etc/nftables.conf"}) {
			t.Fatalf("%s: ran %v, want %d reloads", when, host.ran, n)
		}
	}
	if err := in.openPorts(ctx, env, hostNet{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); string(b) != "# notes's ports, which every start of the install writes.\ntcp dport { 80, 8443 } accept\n" {
		t.Errorf("rule %q", b)
	}
	reloads(1, "first start")
	// The same ports: nothing written or loaded again.
	if err := in.openPorts(ctx, env, hostNet{}); err != nil {
		t.Fatal(err)
	}
	reloads(1, "same ports")
	env.Set(HTTPSPortVar, "443")
	if err := in.openPorts(ctx, env, hostNet{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), "{ 80, 443 }") {
		t.Errorf("rule %q", b)
	}
	reloads(2, "new port")
	// Loopback publishes nothing outside: the rule goes, once.
	env.Set("STOWAGE_TLS_MODE", Loopback)
	for range 2 {
		if err := in.openPorts(ctx, env, hostNet{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the rule is left: %v", err)
	}
	reloads(3, "loopback")
	env.Set("STOWAGE_TLS_MODE", "internal")
	env.Set(HTTPPortVar, "80 ; tcp dport 22 accept")
	if err := in.openPorts(ctx, env, hostNet{}); err == nil || !strings.Contains(err.Error(), "is not a TCP port") {
		t.Errorf("err = %v", err)
	}
	env.Set(HTTPPortVar, "80")
	host.err = errors.New("nft: no such table")
	if err := in.openPorts(ctx, env, hostNet{}); err == nil || !strings.Contains(err.Error(), "loading the firewall with the install's ports: nft: no such table") {
		t.Errorf("err = %v", err)
	}
	// The next start loads it again.
	host.err = nil
	if err := in.openPorts(ctx, env, hostNet{}); err != nil {
		t.Fatal(err)
	}
	reloads(5, "after a failed load")
	// Off the VM: no firewall, nothing written or run.
	off := &Installer{R: in.R, O: Options{Host: host}}
	if err := off.openPorts(ctx, env, hostNet{}); err != nil || len(host.ran) != 5 {
		t.Errorf("err = %v, ran %v", err, host.ran)
	}
}

// The L4 routes' ports are opened in every mode, in the input chain (their services run on the
// host's network): from every source without an allowlist, else from its sources alone, per
// family, and on the bind address alone when there is one; an empty allowlist refuses all. The
// ports the stack's own services reach the routes' services on are opened from its bridges. The
// forward chain's file of releases that published the routes through Docker's DNAT goes.
func TestEveryStartOpensTheL4PortsFromTheirSources(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := &nftHost{}
	r, _ := testInstallerWith(t, "1.0.0", func(s *spec.Spec) {
		s.L4.Routes = []spec.L4Route{
			{Name: "db", Protocol: "tcp", Port: 5432, Upstream: "postgres:5432"},
			{Name: "feed", Protocol: "udp", Port: 17007, Upstream: "api:17007", AllowInput: "FEED_SOURCES"},
		}
	})
	in := &Installer{R: r, O: Options{Dir: t.TempDir(), Firewall: filepath.Join(t.TempDir(), "nftables.d"), Host: host}}
	ports, l4 := filepath.Join(in.O.Firewall, "notes-ports.nft"), filepath.Join(in.O.Dir, ForwardRules, "notes-l4.nft")
	old := []byte("# notes's L4 routes' allowlists, which every start of the install writes.\nmeta nfproto ipv6 meta l4proto udp drop\n")
	if err := os.MkdirAll(filepath.Dir(l4), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l4, old, 0o644); err != nil {
		t.Fatal(err)
	}
	env := NewEnvFile()
	env.Set("STOWAGE_TLS_MODE", Loopback)
	env.Set("FEED_SOURCES", "192.0.2.7, 10.1.2.3/8 2001:db8::1 10.9.9.9 ::ffff:198.51.100.4 192.0.2.7/32")
	hn := hostNet{targets: map[string]bool{"postgres": true, "api": true}, open: []peerPort{{17007, "udp"}, {9090, "tcp"}, {5432, "tcp"}, {9090, "tcp"}}}
	// A load that fails leaves the old allowlists, which the next boot loads, and no ports file.
	host.err = errors.New("nft: syntax error")
	if err := in.openPorts(ctx, env, hn); err == nil {
		t.Fatal("a failed load passed")
	}
	if b, _ := os.ReadFile(l4); !bytes.Equal(b, old) {
		t.Errorf("allowlists after a failed load %q", b)
	}
	if _, err := os.Stat(ports); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the ports file is left: %v", err)
	}
	host.err = nil
	if err := in.openPorts(ctx, env, hn); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(ports); string(b) != "# notes's ports, which every start of the install writes.\n"+
		"iifname \"br-*\" tcp dport { 5432, 9090 } accept\n"+
		"iifname \"br-*\" udp dport { 17007 } accept\n"+
		"tcp dport 5432 accept\n"+
		"udp dport 17007 ip saddr { 10.0.0.0/8, 192.0.2.7/32, 198.51.100.4/32 } accept\n"+
		"udp dport 17007 ip6 saddr { 2001:db8::1/128 } accept\n" {
		t.Errorf("ports %q", b)
	}
	if _, err := os.Stat(l4); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the forward chain's allowlists are left: %v", err)
	}
	if len(host.ran) != 2 {
		t.Fatalf("ran %v", host.ran)
	}
	// IPv4 alone: no IPv6 source is accepted.
	env.Set("FEED_SOURCES", "192.0.2.7")
	if err := in.openPorts(ctx, env, hn); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(ports); strings.Contains(string(b), "ip6") || !strings.Contains(string(b), "udp dport 17007 ip saddr { 192.0.2.7/32 } accept\n") {
		t.Errorf("ports %q", b)
	}
	// Bound to one address: reached on it alone, over its family alone.
	in.R.Spec.L4.Routes[0].BindInput, in.R.Spec.L4.Routes[1].BindInput = "DB_BIND", "FEED_BIND"
	env.Set("DB_BIND", "::ffff:192.0.2.1")
	env.Set("FEED_BIND", "2001:db8::5")
	env.Set("FEED_SOURCES", "192.0.2.7 2001:db8::1")
	if err := in.openPorts(ctx, env, hn); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(ports); !strings.Contains(string(b), "\nip daddr 192.0.2.1 tcp dport 5432 accept\nip6 daddr 2001:db8::5 udp dport 17007 ip6 saddr { 2001:db8::1/128 } accept\n") || strings.Contains(string(b), "udp dport 17007 ip saddr") {
		t.Errorf("ports %q", b)
	}
	// The wildcard binds every address.
	env.Set("DB_BIND", "0.0.0.0")
	env.Set("FEED_BIND", "")
	// Empty: every source refused, nothing opened but from the bridges.
	env.Set("FEED_SOURCES", "")
	if err := in.openPorts(ctx, env, hn); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(ports); strings.Count(string(b), "17007") != 1 || !strings.Contains(string(b), "\ntcp dport 5432 accept\n") {
		t.Errorf("ports %q", b)
	}
	env.Set("FEED_SOURCES", "192.0.2.7; tcp dport 22 accept")
	if err := in.openPorts(ctx, env, hn); err == nil || !strings.Contains(err.Error(), "FEED_SOURCES: ") {
		t.Errorf("err = %v", err)
	}
}

// iptHost records each host command as a line and what it reads from stdin; a command starting
// with fail fails, and so does a check of a rule (-C) while noJump.
type iptHost struct {
	ran, stdin []string
	fail       string
	noJump     bool
}

func (h *iptHost) Run(_ context.Context, c Cmd) ([]byte, error) {
	line := strings.Join(c.Args, " ")
	h.ran = append(h.ran, line)
	if c.Stdin != nil {
		b, _ := io.ReadAll(c.Stdin)
		h.stdin = append(h.stdin, string(b))
	}
	if h.fail != "" && strings.HasPrefix(line, h.fail) || h.noJump && slices.Contains(c.Args, "-C") {
		return nil, errors.New("iptables: no chain/target/match by that name")
	}
	return nil, nil
}

// A service's interception is accepted on its roles alone, and a role's forwarding past Docker's
// FORWARD policy in a chain DOCKER-USER jumps to, for IPv4 and IPv6: the role's traffic out, the
// replies back. A start that changes nothing runs no iptables; a failed one applies again at the
// next start; a release that forwards nothing empties the chain once, then leaves iptables alone.
// A kernel without IPv6 has no ip6tables chain of Docker's: the forwarding is IPv4's alone. It
// sets ipv6Proc, so it runs alone.
func TestForwardingWithoutIPv6IsIPv4s(t *testing.T) {
	ipv6Proc = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { ipv6Proc = "/proc/sys/net/ipv6" })
	host := &iptHost{}
	r, _ := testInstallerWith(t, "1.0.0", func(s *spec.Spec) {
		s.Host = &spec.Host{Interfaces: map[string]spec.HostInterface{"uplink": {Primary: true}, "lan": {Forward: []string{"uplink"}}}}
	})
	in := &Installer{R: r, O: Options{Firewall: t.TempDir(), Host: host}}
	if err := in.forwardRoles(context.Background()); err != nil || !slices.Equal(host.ran, []string{"iptables-restore -w --noflush", "iptables -w -C DOCKER-USER -j STOWAGE-FORWARD"}) {
		t.Errorf("ran %q %v", host.ran, err)
	}
}

func TestEveryStartForwardsAndInterceptsOnTheRoles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := &iptHost{noJump: true}
	r, _ := testInstallerWith(t, "1.0.0", func(s *spec.Spec) {
		s.Host = &spec.Host{Interfaces: map[string]spec.HostInterface{
			"uplink": {Primary: true},
			"vpn0":   {Kind: spec.InterfaceTUN, Forward: []string{"uplink"}},
		}}
		s.Services = map[string]spec.Service{"proxy": {HostNetwork: true, Transparent: []string{"vpn0"}}, "api": {CPUs: 1}}
	})
	in := &Installer{R: r, O: Options{Dir: t.TempDir(), Firewall: filepath.Join(t.TempDir(), "nftables.d"), Host: host}}
	env := NewEnvFile()
	env.Set("STOWAGE_TLS_MODE", Loopback)
	hn := hostNet{targets: map[string]bool{"proxy": true}}
	host.fail = "ip6tables-restore"
	if err := in.openPorts(ctx, env, hn); err == nil || !strings.Contains(err.Error(), "ip6tables") {
		t.Fatalf("a failed restore: %v", err)
	}
	host.fail, host.ran, host.stdin = "", nil, nil
	if err := in.openPorts(ctx, env, hn); err != nil {
		t.Fatal(err)
	}
	rules := "*filter\n:STOWAGE-FORWARD - [0:0]\n" +
		"-A STOWAGE-FORWARD -i vpn0 -o uplink -j ACCEPT\n" +
		"-A STOWAGE-FORWARD -i uplink -o vpn0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\n" +
		"COMMIT\n"
	want := []string{
		"iptables-restore -w --noflush", "iptables -w -C DOCKER-USER -j STOWAGE-FORWARD", "iptables -w -I DOCKER-USER -j STOWAGE-FORWARD",
		"ip6tables-restore -w --noflush", "ip6tables -w -C DOCKER-USER -j STOWAGE-FORWARD", "ip6tables -w -I DOCKER-USER -j STOWAGE-FORWARD",
		"nft -f /etc/nftables.conf",
	}
	if !slices.Equal(host.ran, want) || !slices.Equal(host.stdin, []string{rules, rules}) {
		t.Errorf("ran %q\nstdin %q", host.ran, host.stdin)
	}
	if b, _ := os.ReadFile(filepath.Join(in.O.Firewall, "notes-ports.nft")); !strings.HasSuffix(string(b), "\niifname \"vpn0\" meta l4proto { tcp, udp } socket transparent 1 accept\n") {
		t.Errorf("ports %q", b)
	}
	// Nothing changed: no iptables, no reload.
	host.ran, host.noJump = nil, false
	if err := in.openPorts(ctx, env, hn); err != nil || host.ran != nil {
		t.Errorf("ran %q %v", host.ran, err)
	}
	// No role forwards: the chain is emptied once, its jump kept.
	in.R.Spec.Host.Interfaces["vpn0"] = spec.HostInterface{Kind: spec.InterfaceTUN}
	host.stdin = nil
	if err := in.openPorts(ctx, env, hn); err != nil {
		t.Fatal(err)
	}
	empty := "*filter\n:STOWAGE-FORWARD - [0:0]\nCOMMIT\n"
	if !slices.Equal(host.ran, []string{"iptables-restore -w --noflush", "iptables -w -C DOCKER-USER -j STOWAGE-FORWARD", "ip6tables-restore -w --noflush", "ip6tables -w -C DOCKER-USER -j STOWAGE-FORWARD"}) || !slices.Equal(host.stdin, []string{empty, empty}) {
		t.Errorf("ran %q\nstdin %q", host.ran, host.stdin)
	}
	host.ran = nil
	if err := in.openPorts(ctx, env, hn); err != nil || host.ran != nil {
		t.Errorf("ran %q %v", host.ran, err)
	}
	// The intercepting service off (a profile): nothing is accepted on its role.
	if err := in.openPorts(ctx, env, hostNet{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(in.O.Firewall, "notes-ports.nft")); strings.Contains(string(b), "socket transparent") {
		t.Errorf("ports %q", b)
	}
}

// The OS bundle's ruleset includes the rule files openPorts writes where the VM's install writes
// them: the ports in the input chain, the allowlists in the forward chain.
func TestTheBundlesFirewallIncludesTheInstallsRules(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := kairos.WriteContext(dir, kairos.Params{Name: "notes", Ctl: "notes-stow"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "files", FirewallConf))
	if err != nil {
		t.Fatal(err)
	}
	conf := string(raw)
	input, forward, ok := strings.Cut(conf, "chain forward {")
	if !ok || !strings.Contains(input, `include "`+PortRules+`/*.nft"`) ||
		!strings.Contains(forward, `include "`+VMDir("notes")+"/"+ForwardRules+`/*.nft"`) || !strings.Contains(forward, "hook forward") {
		t.Errorf("ruleset:\n%s", conf)
	}
}

// A preload unit that fails: the install goes on when every image is loaded, saying the unit
// failed; with one missing it is refused naming the unit, its failure and its journal.
func TestAFailedPreloadRefusesOnlyAMissingImage(t *testing.T) {
	t.Parallel()
	run := func(e *env) (*Installer, error) {
		in := e.installer(func(o *Options) { vm(o); o.Host = &preloadHost{state: "activating", err: errors.New("exit status 1")} })
		if err := in.awaitPreload(context.Background()); err != nil {
			return in, err
		}
		return in, in.Install(context.Background())
	}
	e := vmEnv(t, "1.0.0")
	if _, err := run(e); err != nil {
		t.Fatalf("every image loaded: %v\n%s", err, e.log)
	}
	if !strings.Contains(e.log.String(), "notes-preload.service did not finish (exit status 1): the install goes on if every image is loaded") {
		t.Errorf("log: %s", e.log)
	}
	e = vmEnv(t, "1.0.0")
	delete(e.docker.loaded, preloadRef("api"))
	_, err := run(e)
	if err == nil || !strings.Contains(err.Error(), "which notes-preload.service loads from the OS — it failed (exit status 1) —") || !strings.Contains(err.Error(), "journalctl -u notes-preload.service") {
		t.Fatalf("an image missing: %v", err)
	}
	e.nothingWritten()
}

func TestTheVMInstallChecksItsDirectoryThenWaits(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	h := &preloadHost{state: "activating"}
	in := e.installer(func(o *Options) { vm(o); o.Host = h })
	var probed string
	no := func(d string) bool { probed = d; return false }
	yes := func(string) bool { return true }
	// The install directory does not exist yet: the nearest one above it is probed.
	if err := in.vmInstallReady(context.Background(), 1000, no); err == nil || !strings.Contains(err.Error(), "is not writable by this user: the VM's install runs as root, `sudo notes-stow install`") || len(h.ran) != 0 || probed != filepath.Dir(e.dir) {
		t.Errorf("not writable: %v, probed %s, ran %q", err, probed, h.ran)
	}
	// Root that cannot write: not a matter of sudo.
	if err := in.vmInstallReady(context.Background(), 0, no); err == nil || !strings.Contains(err.Error(), "is not writable, by root either (a read-only filesystem?)") || strings.Contains(err.Error(), "sudo") {
		t.Errorf("root, not writable: %v", err)
	}
	// A dry run checks the directory too, and waits for nothing.
	in.O.DryRun = true
	if err := in.vmInstallReady(context.Background(), 1000, no); err == nil || !strings.Contains(err.Error(), "is not writable by this user") {
		t.Errorf("dry run, not writable: %v", err)
	}
	if err := in.vmInstallReady(context.Background(), 1000, yes); err != nil || len(h.ran) != 1 || h.ran[0][1] != "show" || !strings.Contains(e.log.String(), "is writable; a run would first wait for notes-preload.service") {
		t.Errorf("dry run: %v, ran %q, log %s", err, h.ran, e.log)
	}
	// With no preload unit, a dry run says a run waits for none.
	none := e.installer(func(o *Options) { vm(o); o.Host, o.DryRun = &preloadHost{}, true })
	if err := none.vmInstallReady(context.Background(), 0, yes); err != nil || !strings.Contains(e.log.String(), "a run would wait for no preload unit") {
		t.Errorf("dry run, no unit: %v, log %s", err, e.log)
	}
	in.O.DryRun, h.ran = false, nil
	if err := in.vmInstallReady(context.Background(), 0, yes); err != nil || len(h.ran) != 2 {
		t.Errorf("writable: %v, ran %q", err, h.ran)
	}
	if d := t.TempDir(); !dirWritable(d) {
		t.Errorf("%s is not writable", d)
	} else if entries, _ := os.ReadDir(d); len(entries) != 0 {
		t.Errorf("the probe left %v", entries)
	}
}

// A first boot on another release's OS writes the settings' release's OS bundle and reboots into
// it, installing nothing; a boot still on another OS after that stops.
func TestTheFirstBootTakesTheReleasesOSFirst(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	os1 := "sha256:" + strings.Repeat("a", 64)
	with := settings(t, "tls: internal\nrelease:\n  version: 1.1.0\n  revision: 1\n  tag: 1.1.0\n  os: "+os1+"\n")
	host := &fakeHost{}
	in := e.installer(func(o *Options) { with(o); o.Host = host })
	if err := in.Boot(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if _, err := os.Stat(filepath.Join(e.dir, envName)); err == nil {
		t.Fatal("installed on the other release's OS")
	}
	ran := host.calls()
	if len(ran) != 2 || strings.Join(ran[0], " ") != "kairos-agent upgrade --source oci:"+in.R.Spec.Registry.Prefix+"/notes-os@"+os1 ||
		strings.Join(ran[1], " ") != "systemctl reboot" {
		t.Fatalf("ran %v", ran)
	}
	in = e.installer(func(o *Options) { with(o); o.Host = host })
	if err := in.Boot(context.Background()); err == nil || !strings.Contains(err.Error(), "after 1.1.0's OS bundle was written and booted") {
		t.Fatalf("second boot: %v", err)
	}
	// The release's own OS installs as ever.
	same := settings(t, "tls: internal\nrelease:\n  version: 1.0.0\n  revision: 1\n  tag: 1.0.0\n  os: "+os1+"\ninputs:\n  APP_DOMAIN: app.example.org\n  API_DOMAIN: api.example.org\n")
	if err := e.installer(same).Boot(context.Background()); err != nil {
		t.Fatalf("own OS: %v\n%s", err, e.log)
	}
}

// The front, and an L4 route, on a host.interfaces role are opened on that interface alone.
func TestThePortsOfAnInterfaceAreOpenedOnItAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	host := &nftHost{}
	r, _ := testInstallerWith(t, "1.0.0", func(s *spec.Spec) {
		s.Front.Interface = "uplink"
		s.L4.Routes = []spec.L4Route{
			{Name: "db", Protocol: "tcp", Port: 5432, Upstream: "postgres:5432"},
			{Name: "feed", Protocol: "udp", Port: 17007, Upstream: "api:17007", AllowInput: "FEED_SOURCES", Interface: "edge"},
		}
	})
	in := &Installer{R: r, O: Options{Dir: t.TempDir(), Firewall: filepath.Join(t.TempDir(), "nftables.d"), Host: host}}
	env := NewEnvFile()
	env.Set("STOWAGE_TLS_MODE", "internal")
	env.Set(HTTPPortVar, "80")
	env.Set(HTTPSPortVar, "443")
	env.Set("FEED_SOURCES", "192.0.2.7")
	if err := in.openPorts(ctx, env, hostNet{}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(in.O.Firewall, "notes-ports.nft"))
	for _, want := range []string{"\niifname \"uplink\" tcp dport { 80, 443 } accept\n", "\ntcp dport 5432 accept\n", "\niifname \"edge\" udp dport 17007 ip saddr { 192.0.2.7/32 } accept\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("no %q in:\n%s", want, b)
		}
	}
}

// A VM made by a release that named no NIC has none named by its role: after an upgrade to one
// that names them, a port on a NIC role keeps its match, which no interface meets (fail closed),
// warned; the network override keeps matching what the VM's cloud-config named.
func TestARoleWithoutItsNICFailsClosed(t *testing.T) {
	dir := t.TempDir()
	saved := networkdDir
	networkdDir = dir
	t.Cleanup(func() { networkdDir = saved })
	if err := os.WriteFile(filepath.Join(dir, "10-notes-uplink.link"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	host := &nftHost{}
	r, _ := testInstallerWith(t, "1.0.0", func(s *spec.Spec) {
		s.Host = &spec.Host{Interfaces: map[string]spec.HostInterface{"uplink": {Primary: true}, "edge": {}, "vpn0": {Kind: spec.InterfaceTUN}}}
		s.Front.Interface = "uplink"
		s.L4.Routes = []spec.L4Route{
			{Name: "feed", Protocol: "udp", Port: 17007, Upstream: "api:17007", Interface: "edge"},
			{Name: "peer", Protocol: "udp", Port: 17008, Upstream: "api:17008", Interface: "vpn0"},
		}
	})
	var log bytes.Buffer
	in := &Installer{R: r, O: Options{Dir: t.TempDir(), Firewall: filepath.Join(t.TempDir(), "nftables.d"), Host: host, Log: &log}}
	env := NewEnvFile()
	env.Set("STOWAGE_TLS_MODE", "internal")
	env.Set(HTTPPortVar, "80")
	env.Set(HTTPSPortVar, "443")
	if err := in.openPorts(ctx, env, hostNet{}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(in.O.Firewall, "notes-ports.nft"))
	for _, want := range []string{"\niifname \"uplink\" tcp dport { 80, 443 } accept\n", "\niifname \"edge\" udp dport 17007 accept\n", "\niifname \"vpn0\" udp dport 17008 accept\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("no %q in:\n%s", want, b)
		}
	}
	if !strings.Contains(log.String(), "warning: no NIC of this VM is named edge") || strings.Contains(log.String(), "named uplink") || strings.Contains(log.String(), "named vpn0") {
		t.Errorf("log %q", log.String())
	}
	if got := seedMatch(dir, "notes"); got != "Name=uplink" {
		t.Errorf("no seed file, one .link: %s", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "10-notes-edge.link"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := seedMatch(dir, "notes"); got != "Name=en* eth*" {
		t.Errorf("no seed file, two .links: %s", got)
	}
	if err := os.WriteFile(filepath.Join(dir, staticUnit("notes")), []byte("[Match]\nName=uplink\n\n[Network]\nDHCP=ipv4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := seedMatch(dir, "notes"); got != "Name=uplink" {
		t.Errorf("the seed's: %s", got)
	}
}
