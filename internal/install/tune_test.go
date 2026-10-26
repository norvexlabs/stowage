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
	raw, err := contentCompose(&model, nil, []byte("key"), tune.Container{Sysctls: set}, hostPlan{}, hostNet{})
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
	again, _ := contentCompose(&model, nil, []byte("key"), tune.Container{Sysctls: set}, hostPlan{}, hostNet{})
	if !bytes.Equal(raw, again) {
		t.Error("the file is not the same from one run to the next")
	}
}

// Each service is given the open files the host allows, unless it sets its own; each network the
// stack makes as a bridge the host's MTU, unless it sets its own.
func TestContentComposeGivesTheHostsOpenFilesAndMTU(t *testing.T) {
	t.Parallel()
	var model composeModel
	if err := json.Unmarshal([]byte(`{"services": {
		"app": {},
		"own": {"ulimits": {"nofile": {"soft": 1024, "hard": 4096}}},
		"hosted": {"network_mode": "host"}},
		"networks": {
		"default": {"name": "p_default"},
		"edge": {"driver": "bridge"},
		"set": {"driver_opts": {"com.docker.network.driver.mtu": "1400"}},
		"shared": {"external": true},
		"over": {"driver": "overlay"}}}`), &model); err != nil {
		t.Fatal(err)
	}
	raw, err := contentCompose(&model, nil, []byte("key"), tune.Container{Nofile: 524288, MTU: 9001}, hostPlan{}, hostNet{})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]struct {
			Ulimits map[string]map[string]int `yaml:"ulimits"`
		} `yaml:"services"`
		Networks map[string]struct {
			DriverOpts map[string]string `yaml:"driver_opts"`
		} `yaml:"networks"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"app", "hosted"} {
		if n := doc.Services[name].Ulimits["nofile"]; n["soft"] != tune.NofileSoft || n["hard"] != 524288 {
			t.Errorf("%s's open files: %v\n%s", name, n, raw)
		}
	}
	if _, ok := doc.Services["own"]; ok {
		t.Errorf("own, which sets its open files: %v", doc.Services["own"])
	}
	if len(doc.Networks) != 2 || doc.Networks["default"].DriverOpts[mtuOpt] != "9001" || doc.Networks["edge"].DriverOpts[mtuOpt] != "9001" {
		t.Errorf("networks: %+v\n%s", doc.Networks, raw)
	}
	// A hard limit below the soft cap: the soft limit is the hard one.
	raw, _ = contentCompose(&model, nil, []byte("key"), tune.Container{Nofile: 4096}, hostPlan{}, hostNet{})
	if !strings.Contains(string(raw), "hard: 4096\n                soft: 4096\n") {
		t.Errorf("a hard limit of 4096:\n%s", raw)
	}
	if raw, _ := contentCompose(&model, nil, []byte("key"), tune.Container{}, hostPlan{}, hostNet{}); string(raw) != contentPlaceholder {
		t.Errorf("nothing to give:\n%s", raw)
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
	put("proc/sys/fs/nr_open", "1048576\n")
	for _, c := range (tune.Host{MemBytes: 4 << 30, CPUs: 1, PageSize: 4096, Profile: tune.Network}).Sysctls() {
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
	network := "network"
	if err := e.install(func(o *Options) { o.Tune = fakeTune(root, 1000); o.TuneProfile = &network }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if v, _ := e.readEnv().Get(TuneProfileVar); v != "network" {
		t.Errorf("--tune-profile network recorded as %q", v)
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
	if api["net.ipv4.tcp_rmem"] != "4096 131072 33554432" || api["net.core.somaxconn"] != "8192" || api["net.ipv4.tcp_tw_reuse"] != "1" {
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
	for _, want := range []string{"# " + tune.SysctlFile + "\n", "# note: ethtool is not installed"} {
		if !strings.Contains(e.out.String(), want) {
			t.Errorf("--print lacks %q:\n%s", want, e.out)
		}
	}
	if strings.Contains(e.out.String(), "# "+tune.DaemonFile) || strings.Contains(e.out.String(), "rmem_max") {
		t.Errorf("the default profile prints the network one's:\n%s", e.out)
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
	if _, err := os.Stat(filepath.Join(root, tune.DaemonFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the default profile wrote Docker's file")
	}
	if err := in.Tune(ctx, TuneOptions{Apply: true, Profile: tune.Network}); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(root, tune.DaemonFile)); err != nil || !strings.Contains(string(raw), `"max-size": "20m"`) {
		t.Errorf("daemon.json after --apply --profile network: %v\n%s", err, raw)
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
	cmd := tuneCommand(in, func() error { return nil }, func(bool) Prompter { return nil }, false)
	cmd.SetArgs([]string{"--check", "--apply"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Error("--check with --apply runs")
	}
}

func TestTheVMsTuneOnlyReports(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	dockerInfo(e.docker, `27.3.1|Ubuntu 24.04 LTS|[]`)
	root := tuneRoot(t)
	sys := fakeTune(root, 0)
	if err := os.MkdirAll(e.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, envName), []byte(TuneProfileVar+"='network'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := e.installer(func(o *Options) { o.Tune = sys; o.TunedAtBoot = true; o.Yes = true })
	ctx := context.Background()
	for _, o := range []TuneOptions{{Apply: true}, {Print: true}, {Revert: true}} {
		if err := in.Tune(ctx, o); err == nil || !strings.Contains(err.Error(), "tunes itself at each boot") {
			t.Errorf("%+v on a VM: %v", o, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, tune.SysctlFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a VM's tune wrote")
	}
	if err := in.Tune(ctx, TuneOptions{}); err != nil || !strings.Contains(e.out.String(), "the network profile") || !strings.Contains(e.out.String(), "net.core.netdev_budget") ||
		!strings.Contains(e.out.String(), "a reboot tunes it again; `sudo journalctl -b -u notes-tune -u notes-tune-online`") || strings.Contains(e.out.String(), "--apply") {
		t.Errorf("the VM's report: %v\n%s", err, e.out)
	}
	// The profile the boot applied is the one the VM holds until its next boot: a lower one the
	// operator chose since is not judged yet, nor needs the install's record, which only root reads.
	if err := os.MkdirAll(filepath.Join(root, "run/stowage"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, tune.BootProfileFile), []byte("default\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.out.Reset()
	if err := in.Tune(ctx, TuneOptions{}); err != nil || !strings.Contains(e.out.String(), "the default profile") {
		t.Errorf("the VM's report after a boot of the default profile: %v\n%s", err, e.out)
	}
	cmd := tuneCommand(in, func() error { return nil }, func(bool) Prompter { return nil }, true)
	cmd.SetArgs([]string{"--apply"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("the VM's tune --apply: %v", err)
	}
}

func TestTuneProfileFlag(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	dockerInfo(e.docker, `27.3.1|Ubuntu 24.04 LTS|[]`)
	in := e.installer(func(o *Options) { o.Tune = fakeTune(tuneRoot(t), 1000) })
	run := func(args ...string) error {
		cmd := tuneCommand(in, func() error { return nil }, func(bool) Prompter { return nil }, false)
		cmd.SetArgs(args)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		return cmd.Execute()
	}
	if err := run("--profile", "fast"); err == nil || !strings.Contains(err.Error(), "--profile") {
		t.Errorf("--profile fast: %v", err)
	}
	if err := run("--profile", "network"); err != nil || !strings.Contains(e.out.String(), "the network profile") {
		t.Errorf("--profile network: %v\n%s", err, e.out)
	}
	e.out.Reset()
	if err := run(); err != nil || !strings.Contains(e.out.String(), "the default profile") {
		t.Errorf("no --profile: %v\n%s", err, e.out)
	}
}

func TestTuneRefusesTheDefaultProfileOnANetworkHost(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	dockerInfo(e.docker, `27.3.1|Ubuntu 24.04 LTS|[]`)
	root := tuneRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "etc/stowage"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, tune.StateFile), []byte(`{"sysctls": {}, "wrote": {}, "daemon": [], "profile": "network"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	in := e.installer(func(o *Options) { o.Tune = fakeTune(root, 1000) })
	if err := in.Tune(context.Background(), TuneOptions{Check: true, Profile: tune.Default}); err == nil || !strings.Contains(err.Error(), "tune --revert` first") {
		t.Errorf("--check --profile default on a network host: %v", err)
	}
	if err := in.Tune(context.Background(), TuneOptions{}); err != nil || !strings.Contains(e.out.String(), "the network profile") {
		t.Errorf("the report follows the recorded profile: %v\n%s", err, e.out)
	}
}
