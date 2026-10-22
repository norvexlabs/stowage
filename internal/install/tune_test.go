package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/tune"
)

func TestContentComposeGivesEachOwnNamespaceTheSysctls(t *testing.T) {
	t.Parallel()
	var model composeModel
	if err := json.Unmarshal([]byte(`{"services": {
		"app": {"sysctls": {"net.core.somaxconn": "1024"}},
		"plain": {},
		"bridged": {"network_mode": "bridge"},
		"hosted": {"network_mode": "host"},
		"side": {"network_mode": "service:app"}}}`), &model); err != nil {
		t.Fatal(err)
	}
	set := map[string]string{"net.core.somaxconn": "8192", "net.ipv4.tcp_rmem": "4096 131072 16777216"}
	raw, err := contentCompose(&model, nil, []byte("key"), set)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]struct {
			Sysctls map[string]string `yaml:"sysctls"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	got := doc.Services
	if s := got["app"].Sysctls; len(s) != 1 || s["net.ipv4.tcp_rmem"] != set["net.ipv4.tcp_rmem"] {
		t.Errorf("app, which sets somaxconn itself: %v", s)
	}
	for _, name := range []string{"plain", "bridged"} {
		if s := got[name].Sysctls; len(s) != 2 {
			t.Errorf("%s: %v", name, s)
		}
	}
	for _, name := range []string{"hosted", "side"} {
		if _, ok := got[name]; ok {
			t.Errorf("%s, on another's namespace, given %v", name, got[name])
		}
	}
	again, _ := contentCompose(&model, nil, []byte("key"), set)
	if !bytes.Equal(raw, again) {
		t.Error("the file is not the same from one run to the next")
	}
}

// tuneRoot is a host of 4 GiB with every setting at 1, as an untuned kernel never is.
func tuneRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	put := func(rel, v string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("proc/meminfo", "MemTotal: 4194304 kB\n")
	put("proc/sys/kernel/osrelease", "6.8.0-45-generic\n")
	for _, c := range (tune.Host{MemBytes: 4 << 30, CPUs: 1, PageSize: 4096}).Sysctls() {
		put(c.Path(), "1\n")
	}
	return root
}

func fakeTune(root string, euid int) *tune.System {
	return &tune.System{Root: root, Ctl: "notes-stow", Euid: func() int { return euid },
		Run:  func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
		Look: func(string) (string, error) { return "", errors.New("not found") }}
}

func dockerInfo(f *fakeDocker, info string) {
	f.answer = func(a string) ([]byte, bool, error) {
		if strings.HasPrefix(a, "info --format") {
			if info == "" {
				return nil, true, errors.New("cannot connect")
			}
			return []byte(info + "\n"), true, nil
		}
		return nil, false, nil
	}
}

func TestInstallGivesTheContainersTheHostsSettings(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.docker.model = `{"services": {"api": {}, "web": {"sysctls": {"net.core.somaxconn": "512"}}}}`
	dockerInfo(e.docker, `27.3.1|Ubuntu 24.04 LTS|["name=seccomp,profile=builtin"]`)
	root := tuneRoot(t)
	// A key this kernel lacks is no container's: its start would fail.
	if err := os.Remove(filepath.Join(root, "proc/sys/net/ipv4/tcp_notsent_lowat")); err != nil {
		t.Fatal(err)
	}
	if err := e.install(func(o *Options) { o.Tune = fakeTune(root, 1000) }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	raw, err := os.ReadFile(filepath.Join(e.dir, ContentCompose))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]struct {
			Sysctls map[string]string `yaml:"sysctls"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	api, web := doc.Services["api"].Sysctls, doc.Services["web"].Sysctls
	if api["net.ipv4.tcp_rmem"] != "4096 131072 16777216" || api["net.core.somaxconn"] != "8192" {
		t.Errorf("api: %v\n%s", api, raw)
	}
	if _, ok := api["net.ipv4.tcp_notsent_lowat"]; ok {
		t.Error("a key the kernel lacks is given")
	}
	if _, ok := web["net.core.somaxconn"]; ok || web["net.ipv4.tcp_rmem"] == "" {
		t.Errorf("web, which sets somaxconn itself: %v", web)
	}
	// The model is the project's stack without the installer's file: a second start reads the
	// same model, so the settings stay.
	for _, c := range e.docker.ran("compose") {
		a := strings.Join(c.Args, " ")
		if strings.HasSuffix(a, "config --format json") && strings.Contains(a, ContentCompose) {
			t.Errorf("the model reads %s: %s", ContentCompose, a)
		}
		if strings.Contains(a, "up --detach") && !strings.Contains(a, ContentCompose) {
			t.Errorf("the start leaves %s out: %s", ContentCompose, a)
		}
	}
}

func TestUserNamespacedContainersGetNoSettings(t *testing.T) {
	t.Parallel()
	for _, opt := range []string{"name=rootless", "name=userns"} {
		e := newEnv(t)
		e.docker.model = `{"services": {"api": {}}}`
		dockerInfo(e.docker, `27.3.1|Ubuntu 24.04 LTS|["name=seccomp,profile=builtin","`+opt+`"]`)
		if err := e.install(func(o *Options) { o.Tune = fakeTune(tuneRoot(t), 1000) }); err != nil {
			t.Fatalf("%v\n%s", err, e.log)
		}
		if raw, err := os.ReadFile(filepath.Join(e.dir, ContentCompose)); err != nil || strings.Contains(string(raw), "sysctls") {
			t.Errorf("%s: the containers given settings: %v\n%s", opt, err, raw)
		}
	}
}

func TestTuneRevertsWithoutAnEngine(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	dockerInfo(e.docker, "")
	in := e.installer(func(o *Options) { o.Tune = fakeTune(tuneRoot(t), 0); o.Yes = true })
	if err := in.Tune(context.Background(), TuneOptions{}); err != nil || strings.Contains(e.out.String(), "docker ") || !strings.Contains(e.log.String(), "the Docker daemon's file is left out") {
		t.Errorf("a report without an engine: %v\n%s%s", err, e.out, e.log)
	}
	if err := in.Tune(context.Background(), TuneOptions{Print: true}); err != nil || strings.Contains(e.out.String(), tune.DaemonFile) {
		t.Errorf("--print without an engine: %v\n%s", err, e.out)
	}
	if err := in.Tune(context.Background(), TuneOptions{Revert: true}); err != nil {
		t.Errorf("--revert without an engine: %v", err)
	}
}

func TestTune(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	dockerInfo(e.docker, `27.3.1|Ubuntu 24.04 LTS|["name=apparmor","name=seccomp,profile=builtin"]`)
	root := tuneRoot(t)
	in := e.installer(func(o *Options) { o.Tune = fakeTune(root, 0) })
	ctx := context.Background()
	if err := in.Tune(ctx, TuneOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "this host: 4096 MiB") || !strings.Contains(e.out.String(), "settings differ: `notes-stow tune --apply`") {
		t.Errorf("report:\n%s", e.out)
	}
	if err := in.Tune(ctx, TuneOptions{Check: true}); err == nil || !strings.Contains(err.Error(), "settings differ") {
		t.Errorf("--check on an untuned host: %v", err)
	}
	e.out.Reset()
	if err := in.Tune(ctx, TuneOptions{Print: true}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# " + tune.SysctlFile + "\n", "# " + tune.DaemonFile + "\n", "# note: ethtool is not installed"} {
		if !strings.Contains(e.out.String(), want) {
			t.Errorf("--print lacks %q:\n%s", want, e.out)
		}
	}
	if _, err := os.Stat(filepath.Join(root, tune.SysctlFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("--print wrote")
	}
	if err := in.Tune(ctx, TuneOptions{Apply: true}); err == nil || !strings.Contains(err.Error(), "give --yes") {
		t.Errorf("--apply without a terminal or --yes: %v", err)
	}
	in.O.Yes = true
	if err := in.Tune(ctx, TuneOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(root, tune.DaemonFile)); err != nil || !strings.Contains(string(raw), `"Soft": 1048576`) {
		t.Errorf("daemon.json after --apply: %v\n%s", err, raw)
	}
	if !strings.Contains(e.log.String(), "systemctl restart docker") {
		t.Errorf("no word of the restart:\n%s", e.log)
	}
	if err := in.Tune(ctx, TuneOptions{Revert: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, tune.SysctlFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("--revert left the settings file")
	}
}

func TestTuneOnDockerDesktopAndAsAUser(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	dockerInfo(e.docker, `28.0.1|Docker Desktop|[]`)
	root := tuneRoot(t)
	in := e.installer(func(o *Options) { o.Tune = fakeTune(root, 0); o.Yes = true })
	// Nothing here to change: the settings are Docker Desktop's VM's.
	if err := in.Tune(context.Background(), TuneOptions{Check: true}); err != nil {
		t.Errorf("--check on Docker Desktop: %v", err)
	}
	if !strings.Contains(e.out.String(), "n/a  docker Docker Desktop") {
		t.Errorf("report:\n%s", e.out)
	}
	if err := in.Tune(context.Background(), TuneOptions{Apply: true}); err == nil || !strings.Contains(err.Error(), "Docker Desktop runs the containers") {
		t.Errorf("--apply on Docker Desktop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, tune.SysctlFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("an apply on Docker Desktop wrote")
	}
	// As a user: refused before anything is asked.
	dockerInfo(e.docker, `27.3.1|Ubuntu|[]`)
	p := &fakePrompter{answers: map[string]string{"confirm": "yes"}}
	in = e.installer(func(o *Options) { o.Tune = fakeTune(root, 1000); o.Prompt = p })
	for _, o := range []TuneOptions{{Apply: true}, {Revert: true}} {
		if err := in.Tune(context.Background(), o); err == nil || !strings.Contains(err.Error(), "run it as root") {
			t.Errorf("%+v as a user: %v", o, err)
		}
	}
	if len(p.forms) != 0 {
		t.Errorf("a user asked %d times before the refusal", len(p.forms))
	}
}

func TestAStoppedDockerDesktopIsStillDockerDesktop(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	dockerInfo(e.docker, "")
	answer := e.docker.answer
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if a == "context show" {
			return []byte("desktop-linux\n"), true, nil
		}
		return answer(a)
	}
	root := tuneRoot(t)
	in := e.installer(func(o *Options) { o.Tune = fakeTune(root, 0); o.Yes = true })
	if err := in.Tune(context.Background(), TuneOptions{Apply: true}); err == nil || !strings.Contains(err.Error(), "Docker Desktop") {
		t.Errorf("--apply with Docker Desktop stopped: %v", err)
	}
}

func TestRootIsCheckedBeforeTheHostIsRead(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	dockerInfo(e.docker, `27.3.1|Ubuntu|[]`)
	root := tuneRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "etc/docker"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, tune.DaemonFile), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := e.installer(func(o *Options) { o.Tune = fakeTune(root, 1000) })
	if err := in.Tune(context.Background(), TuneOptions{Apply: true}); err == nil || !strings.Contains(err.Error(), "run it as root") {
		t.Errorf("--apply as a user: %v", err)
	}
}

func TestOneDockerInfoPerRun(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	dockerInfo(e.docker, `27.3.1|Ubuntu|[]`)
	in := e.installer(func(o *Options) { o.Tune = fakeTune(tuneRoot(t), 1000) })
	if err := in.Install(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	in.tuneHint(context.Background())
	if n := len(e.docker.ran("info")); n != 1 {
		t.Errorf("%d docker info, want 1", n)
	}
}

func TestAnEngineThatDoesNotAnswerFailsTheStart(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	dockerInfo(e.docker, "")
	if err := e.install(func(o *Options) { o.Tune = fakeTune(tuneRoot(t), 1000) }); err == nil || !strings.Contains(err.Error(), "the containers' network settings") {
		t.Errorf("a start without docker info: %v", err)
	}
}

func TestTuneHint(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	root := tuneRoot(t)
	dockerInfo(e.docker, "")
	e.installer(func(o *Options) { o.Tune = fakeTune(root, 1000) }).tuneHint(context.Background())
	e.installer(nil).tuneHint(context.Background())
	if e.out.Len() != 0 {
		t.Errorf("a hint without an engine or a host:\n%s", e.out)
	}
	dockerInfo(e.docker, `27.3.1|Ubuntu|[]`)
	e.installer(func(o *Options) { o.Tune = fakeTune(root, 1000) }).tuneHint(context.Background())
	if !strings.HasPrefix(e.out.String(), "tuning   ") || !strings.Contains(e.out.String(), "`notes-stow tune` shows them") {
		t.Errorf("hint:\n%s", e.out)
	}
}

func TestTuneFlagsExcludeEachOther(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	in := e.installer(nil)
	cmd := tuneCommand(in, func() error { return nil }, func(bool) Prompter { return nil })
	cmd.SetArgs([]string{"--check", "--apply"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Error("--check with --apply runs")
	}
}
