package install

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/tune"
)

// The stack takes the release's profile until the operator chooses one; release gives it back.
func TestTheOperatorsTuneProfileOverTheReleases(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	status := func() string {
		e.out.Reset()
		if err := e.installer(nil).Status(context.Background()); err != nil {
			t.Fatal(err)
		}
		return e.out.String()
	}
	if _, ok := e.readEnv().Get(TuneProfileVar); ok || !strings.Contains(status(), "\ntune      default (release)\n") {
		t.Errorf("a first install without --tune-profile:\n%s", e.out)
	}
	for _, c := range []struct{ flag, recorded, status, log string }{
		{"network", "network", "network (operator)", "tuning: the network profile, the operator's (was default)"},
		{"network", "network", "network (operator)", ""},
		{"default", "default", "default (operator)", "tuning: the default profile, the operator's (was network)"},
		{"release", "", "default (release)", ""},
	} {
		e.log.Reset()
		flag := c.flag
		if err := e.install(func(o *Options) { o.TuneProfile = &flag }); err != nil {
			t.Fatalf("%v\n%s", err, e.log)
		}
		if v, _ := e.readEnv().Get(TuneProfileVar); v != c.recorded {
			t.Errorf("--tune-profile %s recorded %q, want %q", c.flag, v, c.recorded)
		}
		if got := status(); !strings.Contains(got, "\ntune      "+c.status+"\n") {
			t.Errorf("--tune-profile %s: status\n%s", c.flag, got)
		}
		if strings.Contains(e.log.String(), "tuning: the") != (c.log != "") || !strings.Contains(e.log.String(), c.log) {
			t.Errorf("--tune-profile %s: log\n%s", c.flag, e.log)
		}
	}
	// A re-run without the flag keeps the recorded choice.
	network := "network"
	if err := e.install(func(o *Options) { o.TuneProfile = &network }); err != nil {
		t.Fatal(err)
	}
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.readEnv().Get(TuneProfileVar); v != "network" {
		t.Errorf("a re-run without --tune-profile: %q", v)
	}
}

func TestTuneProfileFlagValues(t *testing.T) {
	t.Parallel()
	var v *string
	f := tuneProfileValue{&v}
	for _, ok := range []string{"default", "network", "release"} {
		if err := f.Set(ok); err != nil || *v != ok {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "shared", "Network"} {
		if err := f.Set(bad); err == nil {
			t.Errorf("%q taken", bad)
		}
	}
	var vm string
	if err := (profileValue{&vm}).Set("network"); err != nil || vm != "network" {
		t.Errorf("vm create --tune-profile network: %v", err)
	}
	if err := (profileValue{&vm}).Set("release"); err == nil {
		t.Error("vm create --tune-profile release taken: a VM has no record to give back yet")
	}
	in := &Installer{R: &Release{Spec: &spec.Spec{}}}
	none := func(string) string { return "" }
	if p, by := in.tuneProfile(none); p != tune.Default || by != "release" {
		t.Errorf("no tune: %s %s", p, by)
	}
	in.R.Spec.Tune = &spec.Tune{Profile: "network"}
	if p, by := in.tuneProfile(none); p != tune.Network || by != "release" {
		t.Errorf("tune.profile network: %s %s", p, by)
	}
	if p, by := in.tuneProfile(func(k string) string { return map[string]string{TuneProfileVar: "default"}[k] }); p != tune.Default || by != "operator" {
		t.Errorf("the operator's default over the release's network: %s %s", p, by)
	}
	if p, _ := in.tuneProfile(func(k string) string { return map[string]string{TuneProfileVar: "fast"}[k] }); p != tune.Network {
		t.Errorf("a record that does not read: %s", p)
	}
}

// The ports the network profile reserves: the front's and the L4 routes'.
func TestStackPorts(t *testing.T) {
	t.Parallel()
	in := &Installer{R: &Release{Spec: &spec.Spec{}}}
	in.R.Spec.Front.Routes = []spec.Route{{Name: "app"}, {Name: "api"}}
	in.R.Spec.L4.Routes = []spec.L4Route{{Name: "db", Port: 15432}, {Name: "vpn", Port: 51820}, {Name: "dup", Port: 443}}
	get := func(k string) string {
		return map[string]string{HTTPPortVar: "80", HTTPSPortVar: "443", PortVar("app"): "x"}[k]
	}
	if got := in.stackPorts(get); !slices.Equal(got, []int{80, 443, 15432, 51820}) {
		t.Errorf("ports %v", got)
	}
}

// A network the installer gives the host's MTU is made again when the host's MTU changed since.
func TestTheStacksNetworksTakeTheHostsMTU(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.docker.model = `{"services": {"api": {}}, "networks": {"default": {"name": "notes_default"}, "outside": {"external": true}}}`
	dockerInfo(e.docker, `27.3.1|Ubuntu 24.04 LTS|[]`)
	root := tuneRoot(t)
	for rel, v := range map[string]string{
		"proc/net/route":         "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\neth0\t00000000\t0100A8C0\t0003\t0\t0\t100\t00000000\n",
		"sys/class/net/eth0/mtu": "1450\n",
	} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	made := ""
	answer := e.docker.answer
	e.docker.answer = func(a string) ([]byte, bool, error) {
		switch {
		case strings.HasPrefix(a, "network ls --quiet --filter label=com.docker.compose.project="):
			return []byte(map[bool]string{true: "", false: "n1\nn2\n"}[made == ""]), true, nil
		case strings.HasPrefix(a, "network inspect --format") && strings.HasSuffix(a, " n1 n2"):
			return []byte(made), true, nil
		}
		return answer(a)
	}
	downs := func() int {
		n := 0
		for _, c := range e.docker.ran("compose") {
			if strings.HasSuffix(strings.Join(c.Args, " "), " down --remove-orphans") {
				n++
			}
		}
		return n
	}
	start := func() {
		t.Helper()
		e.log.Reset()
		if err := e.install(func(o *Options) { o.Tune = fakeTune(root, 1000) }); err != nil {
			t.Fatalf("%v\n%s", err, e.log)
		}
	}
	start()
	if raw, _ := os.ReadFile(filepath.Join(e.dir, ContentCompose)); !strings.Contains(string(raw), mtuOpt+`: "1450"`) || strings.Contains(string(raw), "outside") {
		t.Errorf("the content file:\n%s", raw)
	}
	if downs() != 0 {
		t.Error("a first start stopped the stack")
	}
	for _, c := range []struct {
		made string
		down int
	}{
		{"default 1450\noutside \n", 0}, // made by the daemon's own default MTU: the same
		{"default \n", 1},
		{"default 9001\n", 2},
		{"outside 9001\n", 2}, // not the installer's
	} {
		made = c.made
		start()
		if downs() != c.down {
			t.Errorf("networks %q: %d downs, want %d\n%s", c.made, downs(), c.down, e.log)
		}
	}
	// No default route this moment: the networks keep the MTU the last start gave them.
	if err := os.WriteFile(filepath.Join(root, "proc/net/route"), []byte("Iface\tDestination\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	made = "default 1450\n"
	start()
	if raw, _ := os.ReadFile(filepath.Join(e.dir, ContentCompose)); downs() != 2 || !strings.Contains(string(raw), mtuOpt+`: "1450"`) {
		t.Errorf("an MTU that does not read: %d downs\n%s\n%s", downs(), raw, e.log)
	}
	// Back on 1500, Docker's own: a network made with 1450 is made again.
	if err := os.WriteFile(filepath.Join(root, "proc/net/route"), []byte("Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\neth0\t00000000\t0100A8C0\t0003\t0\t0\t100\t00000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sys/class/net/eth0/mtu"), []byte("1500\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	made = "default 1450\n"
	start()
	if downs() != 3 {
		t.Errorf("1500 over a network of 1450: %d downs\n%s", downs(), e.log)
	}
	made = "default \n"
	start()
	if downs() != 3 {
		t.Errorf("1500 over Docker's own: %d downs", downs())
	}
}

// The VM's boot takes the stack's profile: the install's record, before it the settings file's,
// else the release's; a re-run that raises it tunes the VM at once.
func TestTheVMsProfile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	settings := filepath.Join(t.TempDir(), "install.yaml")
	in := e.installer(func(o *Options) { o.Settings = settings; o.TunedAtBoot = true })
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if p := in.vmProfile(); p != tune.Default {
		t.Errorf("no settings, no install: %s", p)
	}
	write(settings, "tune_profile: network\n")
	if p := in.vmProfile(); p != tune.Network {
		t.Errorf("the settings file's: %s", p)
	}
	envPath := filepath.Join(e.dir, envName)
	write(envPath, "STOWAGE_NAME='notes'\n")
	if p := in.vmProfile(); p != tune.Default {
		t.Errorf("an install that records none: %s", p)
	}
	root := tuneRoot(t)
	sys := fakeTune(root, 0)
	sys.Docker = &tune.Docker{}
	budget := filepath.Join(root, "proc/sys/net/core/netdev_budget")
	var stderr bytes.Buffer
	record := filepath.Join(root, tune.BootProfileFile)
	// No boot of this Stowage tuned the VM yet: its next boot does.
	write(envPath, TuneProfileVar+"='network'\n"+HTTPSPortVar+"='15443'\n")
	in.retune(context.Background(), sys, &stderr)
	if stderr.Len() != 0 {
		t.Errorf("no boot record: %s", stderr.String())
	}
	write(record, "default\n")
	in.retune(context.Background(), sys, &stderr)
	if !strings.Contains(stderr.String(), "tuned by the network profile") {
		t.Errorf("raised: %s\n%s", stderr.String(), e.log)
	}
	if raw, _ := os.ReadFile(budget); string(raw) != "600\n" {
		t.Errorf("netdev_budget after the raise: %q", raw)
	}
	if p, ports, ok := sys.Booted(); !ok || p != tune.Network || !slices.Contains(ports, 15443) {
		t.Errorf("the raise's record: %s %v %v", p, ports, ok)
	}
	reserved := filepath.Join(root, "proc/sys/net/ipv4/ip_local_reserved_ports")
	if raw, _ := os.ReadFile(reserved); !strings.Contains(string(raw), "15443") {
		t.Errorf("the front's port not reserved: %q", raw)
	}
	// Nothing changed: nothing done.
	stderr.Reset()
	in.retune(context.Background(), sys, &stderr)
	if stderr.Len() != 0 {
		t.Errorf("no change: %s", stderr.String())
	}
	// The network profile kept, the front's port changed: reserved at once.
	write(envPath, TuneProfileVar+"='network'\n"+HTTPSPortVar+"='16443'\n")
	in.retune(context.Background(), sys, &stderr)
	if raw, _ := os.ReadFile(reserved); !strings.Contains(string(raw), "16443") || !strings.Contains(stderr.String(), "tuned by the network profile") {
		t.Errorf("a changed port not reserved: %q %s", raw, stderr.String())
	}
	write(envPath, TuneProfileVar+"='default'\n")
	write(budget, "1\n")
	stderr.Reset()
	in.retune(context.Background(), sys, &stderr)
	if stderr.Len() != 0 || !strings.Contains(e.log.String(), "takes over at the next reboot") {
		t.Errorf("lowered: %s\n%s", stderr.String(), e.log)
	}
	if raw, _ := os.ReadFile(budget); string(raw) != "1\n" {
		t.Errorf("a lowered profile tuned at once: %q", raw)
	}
	// A tuning that fails is a note: the install is done.
	write(record, "default\n")
	write(envPath, TuneProfileVar+"='network'\n")
	e.log.Reset()
	in.retune(context.Background(), fakeTune(root, 1000), &stderr)
	if !strings.Contains(e.log.String(), "note: tuning the VM now failed") {
		t.Errorf("a failed tuning:\n%s", e.log)
	}
}

// `vm create --tune-profile` reaches the VM's first install, which records it.
func TestTheSettingsFilesTuneProfileIsTheFirstInstalls(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	in := e.installer(settings(t, "tune_profile: network\ninputs: { ADMIN_EMAIL: ops@example.org }\n"))
	if err := in.withSettings(); err != nil {
		t.Fatal(err)
	}
	if err := in.Install(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if v := get(e.readEnv(), TuneProfileVar); v != "network" {
		t.Errorf("recorded %q", v)
	}
	bad := e.installer(settings(t, "tune_profile: fast\n"))
	if err := os.Remove(filepath.Join(e.dir, envName)); err != nil {
		t.Fatal(err)
	}
	if err := bad.withSettings(); err == nil || !strings.Contains(err.Error(), "tune_profile") {
		t.Errorf("a settings file's unknown profile: %v", err)
	}
}

// The containers take the profile the install records, never one the operator's shell exports.
func TestTheShellsTuneProfileIsNotTheInstalls(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.docker.model = `{"services": {"api": {}}}`
	dockerInfo(e.docker, `27.3.1|Ubuntu 24.04 LTS|[]`)
	root := tuneRoot(t)
	if err := e.install(func(o *Options) {
		o.Tune = fakeTune(root, 1000)
		o.Environ = append(o.Environ, TuneProfileVar+"=network")
	}); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if raw, _ := os.ReadFile(filepath.Join(e.dir, ContentCompose)); !strings.Contains(string(raw), "net.core.somaxconn: \"1\"") {
		t.Errorf("the shell's profile reached the containers:\n%s", raw)
	}
}

// A record a report cannot read is said: the release's profile is then judged.
func TestAReportSaysTheRecordDoesNotRead(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	dockerInfo(e.docker, `27.3.1|Ubuntu 24.04 LTS|[]`)
	if err := os.MkdirAll(e.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, envName), []byte("not a record\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := e.installer(func(o *Options) { o.Tune = fakeTune(tuneRoot(t), 1000) })
	if err := in.Tune(context.Background(), TuneOptions{}); err != nil || !strings.Contains(e.log.String(), "note: the install's record does not read") || !strings.Contains(e.out.String(), "the default profile") {
		t.Errorf("%v\n%s%s", err, e.log, e.out)
	}
}
