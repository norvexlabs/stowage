package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/truenas"
)

func init() { sshPoll = time.Millisecond }

// fakeTofu is OpenTofu: `apply` writes a state into -chdir's directory, and every call is
// recorded with its environment.
type fakeTofu struct {
	mu      sync.Mutex
	calls   [][]string
	envs    [][]string
	missing bool   // `tofu` is not installed
	fail    error  // apply fails
	outputs string // `tofu output -json`; empty is an id alone
}

func (f *fakeTofu) Run(_ context.Context, c Cmd) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.envs = append(f.calls, c.Args), append(f.envs, c.Env)
	if f.missing {
		return nil, errors.New(`exec: "tofu": executable file not found in $PATH`)
	}
	if len(c.Args) < 2 {
		return nil, nil
	}
	dir := strings.TrimPrefix(c.Args[0], "-chdir=")
	switch c.Args[1] {
	case "apply":
		if f.fail != nil {
			return nil, f.fail
		}
		return nil, os.WriteFile(filepath.Join(dir, "terraform.tfstate"), []byte("{}"), 0o600)
	case "destroy":
		return nil, os.WriteFile(filepath.Join(dir, "terraform.tfstate"), []byte(`{"resources":[]}`), 0o600)
	case "output":
		if f.outputs == "" {
			return []byte(`{"id": {"value": "7", "type": "string"}}`), nil
		}
		return []byte(f.outputs), nil
	}
	return nil, nil
}

// fakeSSH is the VM over SSH: each call answers the next of answers (the last one again when
// they run out), as `<short>-stow status --json` there would.
type fakeSSH struct {
	mu      sync.Mutex
	calls   [][]string
	stdin   []string // what each call was given on its standard input
	answers []func() ([]byte, error)
}

func (f *fakeSSH) Run(_ context.Context, c Cmd) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c.Args)
	in := ""
	if c.Stdin != nil {
		b, _ := io.ReadAll(c.Stdin)
		in = string(b)
	}
	f.stdin = append(f.stdin, in)
	// Each answer is a system of its own, with its own host key (the calls past the last answer
	// reach the last one's). As accept-new does: the key taken into the known hosts file when it
	// holds none, and refused when it holds another.
	i := min(len(f.calls), len(f.answers)) - 1
	key := fmt.Sprintf("host key %d\n", i+1)
	for _, a := range c.Args {
		if p, ok := strings.CutPrefix(a, "UserKnownHostsFile="); ok {
			p = strings.Trim(p, `"`) // as ssh reads a quoted value
			if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
				if string(b) != key {
					return nil, errors.New("Host key verification failed.")
				}
				continue
			}
			if err := os.WriteFile(p, []byte(key), 0o600); err != nil {
				return nil, err
			}
		}
	}
	return f.answers[i]()
}

func statusJSON(installed bool) func() ([]byte, error) { return statusIn("active", installed) }

// statusIn is the status of the system boot (active, recovery), installed or not.
func statusIn(boot string, installed bool) func() ([]byte, error) {
	return func() ([]byte, error) {
		st := Status{Name: "notes", OS: Version{"1.0.0", 1}, Job: "idle", Boot: boot}
		if installed {
			st.Installed = &Version{"1.0.0", 1}
		}
		return json.Marshal(st)
	}
}

// statusInstalling is the status of a first install not finished, still running.
func statusInstalling() ([]byte, error) {
	return json.Marshal(Status{Name: "notes", OS: Version{"1.0.0", 1}, Job: "idle", Boot: "active", Installed: &Version{"1.0.0", 1}, Installing: true})
}

// statusFailed is the status of a first install whose run failed.
func statusFailed() ([]byte, error) {
	return json.Marshal(Status{Name: "notes", OS: Version{"1.0.0", 1}, Job: "idle", Boot: "active", Installed: &Version{"1.0.0", 1}, Installing: true, InstallFailed: "provisioning did not finish"})
}

func refused() ([]byte, error) { return nil, errors.New("ssh: connect to host: Connection refused") }

// tofuRun is a `vm create --create opentofu` setting: its environment, OpenTofu and ssh.
type tofuRun struct {
	tofu       *fakeTofu
	ssh        *fakeSSH
	vars       map[string]string
	state, cch string
}

func newTofuRun(t *testing.T) *tofuRun {
	home := t.TempDir()
	r := &tofuRun{tofu: &fakeTofu{}, ssh: &fakeSSH{answers: []func() ([]byte, error){refused, statusIn("recovery", true), statusJSON(false), statusJSON(true)}},
		state: filepath.Join(home, "state"), cch: filepath.Join(home, "cache")}
	r.vars = map[string]string{"TRUENAS_URL": "https://nas.example.org", "TRUENAS_API_KEY": "the-api-key",
		"XDG_STATE_HOME": r.state, "XDG_CACHE_HOME": r.cch, "HOME": home}
	return r
}

func (r *tofuRun) mod(more func(*Options)) func(*Options) {
	return func(o *Options) {
		o.Tofu, o.SSH = r.tofu, r.ssh
		o.Getenv = func(k string) string { return r.vars[k] }
		o.Environ = []string{"PATH=/bin", "TRUENAS_API_KEY=the-api-key"}
		o.TLS, o.Set = "internal", map[string]string{"APP_DOMAIN": "app.example.org", "API_DOMAIN": "api.example.org", "ADMIN_EMAIL": "admin@example.org"}
		if more != nil {
			more(o)
		}
	}
}

func tofuCreate() VMCreate {
	return VMCreate{Platform: "truenas", Create: CreateTofu, Hostname: "notes-1", User: "ops", Ctl: "/opt/notes-stow",
		TofuVM: TofuVM{Pool: "tank", Dataset: "vms", Bridge: "br0"}, Host: "notes-1.lan", Wait: 5 * time.Second} // a regression fails fast
}

func TestVMCreateWithOpenTofu(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	r := newTofuRun(t)
	if err := v.create(tofuCreate(), r.mod(nil)); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	dir := filepath.Join(r.state, "notes", "vms", "notes-1")
	if got := r.tofu.calls; len(got) != 4 || got[0][0] != "version" ||
		!slices.Equal(got[1], []string{"-chdir=" + dir, "init", "-input=false", "-no-color"}) ||
		!slices.Equal(got[2], []string{"-chdir=" + dir, "apply", "-input=false", "-auto-approve", "-no-color"}) ||
		!slices.Equal(got[3], []string{"-chdir=" + dir, "output", "-json", "-no-color"}) {
		t.Fatalf("tofu ran %q", got)
	}
	// The API key reaches OpenTofu through its environment alone; its steps run this ctl.
	env := r.tofu.envs[2]
	for _, want := range []string{"TRUENAS_API_KEY=the-api-key", CtlVar + "=/opt/notes-stow", "TF_IN_AUTOMATION=1"} {
		if !slices.Contains(env, want) {
			t.Errorf("tofu's environment %q lacks %s", env, want)
		}
	}
	var vars tofuVars
	b, err := os.ReadFile(filepath.Join(dir, "terraform.tfvars.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &vars); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(r.cch, "notes", "disks", strings.TrimPrefix(v.r.Manifest.Artifacts.Disks["qcow2"], "sha256:")[:16], "notes-1.0.0.qcow2")
	seed := filepath.Join(dir, "seed.iso")
	seedSum, _ := truenas.SHA256File(seed)
	diskSum, _ := truenas.SHA256File(disk) // the layer's digest: the disk's bytes, not its manifest
	want := tofuVars{Name: "notes1", Pool: "tank", Dataset: "vms", Bridge: "br0", CPUs: 2, MemoryMiB: 4096, DiskGiB: 40,
		Image: disk, ImageSHA256: diskSum, Seed: seed, SeedSHA256: seedSum,
		Description: "notes 1.0.0 (revision 1), created by notes-stow vm create"}
	if vars != want {
		t.Errorf("variables\n%+v\nwant\n%+v", vars, want)
	}
	if got, _ := os.ReadFile(disk); string(got) != string(v.disks["qcow2"]) {
		t.Errorf("disk %q", got)
	}
	if ud, _, _ := readSeed(t, seed); !strings.Contains(ud, "notes-1") {
		t.Errorf("seed user-data:\n%s", ud)
	}
	// No credential is written: the module, the variables, the seed.
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), "the-api-key") {
				t.Errorf("%s holds the API key", p)
			}
		}
		return nil
	})
	if b, _ := os.ReadFile(filepath.Join(dir, platformFile)); string(b) != "truenas builtin\n" {
		t.Errorf("platform %q", b)
	}
	for _, f := range []string{"main.tf", "variables.tf", "outputs.tf", "versions.tf"} {
		got, _ := os.ReadFile(filepath.Join(dir, f))
		emb, _ := tofuModules.ReadFile("tofu/truenas/" + f)
		if len(emb) == 0 || string(got) != string(emb) {
			t.Errorf("%s is not the embedded module's", f)
		}
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("state dir %v %v", fi, err)
	}
	// Waited for over SSH until it installed: refused, the recovery system (even reporting an
	// install), answering, installed. Its host key is the last attempt's, the active system's.
	if n := len(r.ssh.calls); n != 4 {
		t.Errorf("%d ssh calls, want 4", n)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "known_hosts")); string(b) != "host key 4\n" {
		t.Errorf("known_hosts %q, want the answering system's key", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "known_hosts.attempt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an attempt's file is left: %v", err)
	}
	a := r.ssh.calls[0]
	if !slices.Contains(a, `UserKnownHostsFile="`+filepath.Join(dir, "known_hosts.attempt")+`"`) || !slices.Contains(a, "BatchMode=yes") ||
		!slices.Equal(a[len(a)-5:], []string{"ops@notes-1.lan", "--", "notes-stow", "status", "--json"}) {
		t.Errorf("ssh %q", a)
	}
	if slices.Contains(a, "-i") {
		t.Errorf("ssh %q names a private key that does not exist", a)
	}
	for _, s := range []string{"The VM notes-1 runs notes 1.0.0", "ssh ops@notes-1.lan notes-stow status", "notes-stow vm destroy notes-1"} {
		if !strings.Contains(v.out2(), s) {
			t.Errorf("no %q in:\n%s", s, v.out2())
		}
	}
	if !strings.Contains(v.log.String(), "OpenTofu creates the VM notes1 at https://nas.example.org: 2 CPUs, 4096 MiB, a 40 GiB disk on tank/vms, its network on br0") {
		t.Errorf("review:\n%s", v.log)
	}

	// A second VM of that host name is refused: its state is there.
	err = v.create(tofuCreate(), r.mod(nil))
	if err == nil || !strings.Contains(err.Error(), "notes-stow vm destroy notes-1") {
		t.Fatalf("a second create over the state: %v", err)
	}
	if len(r.tofu.calls) != 5 { // its version check alone
		t.Errorf("tofu ran %q", r.tofu.calls[4:])
	}
}

func TestVMCreateWithOpenTofuLogsInWithThePrivateKey(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	r := newTofuRun(t)
	priv := strings.TrimSuffix(v.key, ".pub")
	if err := os.WriteFile(priv, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := tofuCreate()
	c.Host, c.Network, c.Address = "", "static", "192.0.2.10/24"
	if err := v.create(c, r.mod(nil)); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	a := r.ssh.calls[0]
	if i := slices.Index(a, "-i"); i < 0 || a[i+1] != priv || !slices.Contains(a, "ops@192.0.2.10") {
		t.Errorf("ssh %q: want -i %s, at the static address", a, priv)
	}
	// DHCP and a static IPv6 address: the host name (this machine may have no route to that
	// address; --host says where else); a bracketed --host is taken bare.
	for _, c := range []struct {
		create VMCreate
		want   string
	}{
		{func() VMCreate { c := tofuCreate(); c.Host, c.IPv6, c.Address6 = "", true, "2001:db8::10/64"; return c }(), "ops@notes-1"},
		{func() VMCreate { c := tofuCreate(); c.Host = "[2001:db8::10]"; return c }(), "ops@2001:db8::10"},
	} {
		v, r := newVMCreate(t, "qcow2"), newTofuRun(t)
		if err := v.create(c.create, r.mod(nil)); err != nil {
			t.Fatalf("%v\n%s", err, v.log)
		}
		if a := r.ssh.calls[0]; !slices.Contains(a, c.want) {
			t.Errorf("%+v: ssh %q, want %s", c.create, a, c.want)
		}
	}
}

func TestVMCreateWithOpenTofuRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		c    func(*VMCreate)
		r    func(*tofuRun)
		want string
	}{
		{"no tofu", nil, func(r *tofuRun) { r.tofu.missing = true }, "opentofu.org"},
		{"no URL", nil, func(r *tofuRun) { delete(r.vars, "TRUENAS_URL") }, "TRUENAS_URL is not set"},
		{"no key", nil, func(r *tofuRun) { delete(r.vars, "TRUENAS_API_KEY") }, "TRUENAS_API_KEY is not set"},
		{"plain HTTP", nil, func(r *tofuRun) { r.vars["TRUENAS_URL"] = "http://nas.example.org" }, "https://<host>"},
		{"a % in the state's path", nil, func(r *tofuRun) { r.vars["XDG_STATE_HOME"] += "/100%" }, "cannot hold"},
		{"no pool", func(c *VMCreate) { c.Pool = "" }, nil, "--pool is required"},
		{"bad dataset", func(c *VMCreate) { c.Dataset = "/vms" }, nil, "a dataset path under the pool"},
		{"bad name", func(c *VMCreate) { c.Name = "notes-1" }, nil, "letters and digits only"},
		{"small disk", func(c *VMCreate) { c.DiskGiB = 15 }, nil, "at least 16"},
		{"small memory", func(c *VMCreate) { c.MemoryMiB = 1023 }, nil, "at least 1024"},
		{"bad host", func(c *VMCreate) { c.Host = "not a host" }, nil, "--host"},
		{"unknown way", func(c *VMCreate) { c.Create = "terraform" }, nil, "--create terraform"},
		{"TrueNAS's flags on proxmox", func(c *VMCreate) { c.Platform = "proxmox" }, nil, "--pool, --dataset and --bridge are for TrueNAS's built-in module"},
		{"a module by hand", func(c *VMCreate) {
			c.Create, c.Host, c.Wait, c.TofuVM = CreateManual, "", 0, TofuVM{Module: "/x"}
		}, nil, "--module is for --create opentofu"},
		{"tofu flags by hand", func(c *VMCreate) { c.Create = CreateManual }, nil, "are for --create opentofu"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := newVMCreate(t, "qcow2")
			r := newTofuRun(t)
			c := tofuCreate()
			if tc.c != nil {
				tc.c(&c)
			}
			if tc.r != nil {
				tc.r(r)
			}
			err := v.create(c, r.mod(nil))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			for _, call := range r.tofu.calls {
				if call[0] != "version" {
					t.Errorf("tofu ran %q", call)
				}
			}
		})
	}
	// Both sides of the bounds: the least size taken.
	v := newVMCreate(t, "qcow2")
	r := newTofuRun(t)
	c := tofuCreate()
	c.DiskGiB, c.MemoryMiB, c.CPUs = 16, 1024, 1
	if err := v.create(c, r.mod(nil)); err != nil {
		t.Fatalf("the least sizes: %v", err)
	}
}

func TestVMCreateWithOpenTofuKeepsTheStateOfAFailedApply(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	r := newTofuRun(t)
	r.tofu.fail = errors.New("tofu apply: the pool is full")
	err := v.create(tofuCreate(), r.mod(nil))
	if err == nil || !strings.Contains(err.Error(), "the pool is full") || !strings.Contains(err.Error(), "notes-stow vm destroy notes-1") {
		t.Fatalf("err = %v", err)
	}
	if len(r.ssh.calls) != 0 {
		t.Error("waited for a VM that failed to apply")
	}
}

func TestVMCreateWithOpenTofuGivesUpWaiting(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	r := newTofuRun(t)
	r.ssh.answers = []func() ([]byte, error){statusJSON(false)}
	c := tofuCreate()
	c.Wait = 30 * time.Millisecond
	err := v.create(c, r.mod(nil))
	if err == nil || !strings.Contains(err.Error(), "not ready after 30ms") || !strings.Contains(err.Error(), "not installed yet") || !strings.Contains(err.Error(), "vm destroy notes-1") {
		t.Fatalf("err = %v", err)
	}
}

func TestVMCreateWithOpenTofuWaitsForTheFirstInstallToFinish(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	r := newTofuRun(t)
	r.ssh.answers = []func() ([]byte, error){statusInstalling, statusInstalling, statusJSON(true)}
	if err := v.create(tofuCreate(), r.mod(nil)); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	if len(r.ssh.calls) != 3 {
		t.Errorf("asked the status %d times, want 3: a first install not finished is waited for", len(r.ssh.calls))
	}
	// Never finished: the wait gives up, saying so.
	v = newVMCreate(t, "qcow2")
	r = newTofuRun(t)
	r.ssh.answers = []func() ([]byte, error){statusInstalling}
	c := tofuCreate()
	c.Wait = 30 * time.Millisecond
	err := v.create(c, r.mod(nil))
	if err == nil || !strings.Contains(err.Error(), "the first install has not finished") {
		t.Fatalf("err = %v", err)
	}
	// Failed: no wait, why and the way to run it again named.
	v = newVMCreate(t, "qcow2")
	r = newTofuRun(t)
	r.ssh.answers = []func() ([]byte, error){statusInstalling, statusFailed}
	err = v.create(tofuCreate(), r.mod(nil))
	if err == nil || !strings.Contains(err.Error(), "the first install failed: provisioning did not finish") || !strings.Contains(err.Error(), "sudo notes-stow install` runs it again") ||
		!strings.Contains(err.Error(), "vm destroy notes-1") || len(r.ssh.calls) != 2 {
		t.Fatalf("err = %v after %d calls", err, len(r.ssh.calls))
	}
	dir := filepath.Join(r.state, "notes", "vms", "notes-1")
	if b, _ := os.ReadFile(filepath.Join(dir, "known_hosts")); string(b) != "host key 2\n" {
		t.Errorf("known_hosts %q, want the key of the system that answered", b)
	}
	// Failed before it wrote the install (no install reported): no wait either.
	v = newVMCreate(t, "qcow2")
	r = newTofuRun(t)
	r.ssh.answers = []func() ([]byte, error){func() ([]byte, error) {
		return json.Marshal(Status{Name: "notes", OS: Version{"1.0.0", 1}, Job: "idle", Boot: "active", InstallFailed: "the boot's install failed"})
	}}
	if err := v.create(tofuCreate(), r.mod(nil)); err == nil || !strings.Contains(err.Error(), "the first install failed: the boot's install failed") || len(r.ssh.calls) != 1 {
		t.Fatalf("err = %v after %d calls", err, len(r.ssh.calls))
	}
}

func TestVMCreateWithOpenTofuWaitsForSSHAloneWhenASecretIsDue(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	v.r.Spec.Inputs = append(v.r.Spec.Inputs, spec.Input{Key: "API_KEY", Prompt: "API key", Secret: true, Required: true})
	r := newTofuRun(t)
	r.ssh.answers = []func() ([]byte, error){refused, statusIn("recovery", false), statusJSON(false)}
	if err := v.create(tofuCreate(), r.mod(nil)); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	if len(r.ssh.calls) != 3 {
		t.Errorf("%d ssh calls, want 3: the install waits for API_KEY, past the recovery system", len(r.ssh.calls))
	}
	if !strings.Contains(v.out2(), "Its install waits for API_KEY") || !strings.Contains(v.out2(), "ssh ops@notes-1.lan sudo notes-stow install") {
		t.Errorf("out:\n%s", v.out2())
	}
}

func TestVMCreateWithOpenTofuGivesTheSecretsOverSSH(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	v.r.Spec.Inputs = append(v.r.Spec.Inputs, spec.Input{Key: "API_KEY", Prompt: "API key", Secret: true, Required: true})
	r := newTofuRun(t)
	// The install's call reaches the system that answered last (the same host key).
	r.ssh.answers = []func() ([]byte, error){refused, statusIn("recovery", false), statusJSON(false)}
	plain := "project: notes\nsecrets:\n  SESSION_SECRET: kept\n  API_KEY: k\n"
	c := tofuCreate()
	c.Secrets = []byte(plain)
	if err := v.create(c, r.mod(nil)); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	// The VM answers, waiting: the secrets go over SSH, the API_KEY among them, never in the
	// cloud-config.
	if len(r.ssh.calls) != 4 || r.ssh.stdin[3] != plain || !strings.HasSuffix(strings.Join(r.ssh.calls[3], " "), "ops@notes-1.lan -- sudo systemctl start notes.service && sudo notes-stow install --secrets - --no-input") {
		t.Fatalf("ssh %v", r.ssh.calls)
	}
	seed, _, _ := readSeed(t, filepath.Join(r.state, "notes", "vms", "notes-1", "seed.iso"))
	if st, _ := cloudSettings(t, seed); !st.AwaitSecrets || strings.Contains(seed, "kept") || strings.Contains(seed, "API_KEY") {
		t.Errorf("seed:\n%s", seed)
	}
	if !strings.Contains(v.out2(), "The VM notes-1 runs notes 1.0.0") {
		t.Errorf("out:\n%s", v.out2())
	}
	// Another project's file is refused before anything is made.
	v = newVMCreate(t, "qcow2")
	r = newTofuRun(t)
	c.Secrets = []byte("project: crm\nsecrets: {}\n")
	if err := v.create(c, r.mod(nil)); err == nil || !strings.Contains(err.Error(), `holds "crm"'s secrets`) || len(r.tofu.calls) > 0 {
		t.Errorf("err = %v, tofu %v", err, r.tofu.calls)
	}
}

func TestVMCreateWarnsWhenSSHSourcesLeaveThisMachineOut(t *testing.T) {
	t.Parallel()
	const warn = "--ssh-allow leaves out 192.0.2.1, the address this machine reaches "
	for _, c := range []struct {
		src, host string
		warns     bool
	}{
		{"192.0.2.1", "198.51.100.7", true},
		{"10.1.2.3", "198.51.100.7", false},
		{"192.0.2.1", "vm.example.org", true}, // resolved first
	} {
		v := newVMCreate(t, "qcow2")
		r := newTofuRun(t)
		vc := tofuCreate()
		vc.Host, vc.SSHAllow = c.host, []string{"10.0.0.0/8"}
		err := v.create(vc, r.mod(func(o *Options) {
			o.SourceAddr = func(dst netip.Addr) (netip.Addr, error) {
				if dst.String() != "198.51.100.7" {
					return netip.Addr{}, fmt.Errorf("asked about %s", dst)
				}
				return netip.MustParseAddr(c.src), nil
			}
			o.LookupIP = func(h string) ([]netip.Addr, error) {
				if h != "vm.example.org" {
					return nil, fmt.Errorf("no such host %s", h)
				}
				return []netip.Addr{netip.MustParseAddr("198.51.100.7")}, nil
			}
		}))
		// A warning: the VM may see this machine at another address.
		if err != nil || strings.Contains(v.log.String(), warn) != c.warns {
			t.Errorf("%+v: %v\n%s", c, err, v.log)
		}
	}
	// A name with an IPv6 address too: ssh may take it, so it is checked as well.
	v := newVMCreate(t, "qcow2")
	r := newTofuRun(t)
	vc := tofuCreate()
	vc.Host, vc.SSHAllow = "vm.example.org", []string{"10.0.0.0/8"}
	if err := v.create(vc, r.mod(func(o *Options) {
		o.LookupIP = func(string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("198.51.100.7"), netip.MustParseAddr("2001:db8::7")}, nil
		}
		o.SourceAddr = func(dst netip.Addr) (netip.Addr, error) {
			if dst.Is6() {
				return netip.MustParseAddr("2001:db8::1"), nil
			}
			return netip.MustParseAddr("10.1.2.3"), nil
		}
	})); err != nil || !strings.Contains(v.log.String(), "--ssh-allow leaves out 2001:db8::1, the address this machine reaches vm.example.org (2001:db8::7) from") ||
		strings.Contains(v.log.String(), "leaves out 10.1.2.3") {
		t.Errorf("%v\n%s", err, v.log)
	}
	// Two addresses reached from one source: said once.
	v = newVMCreate(t, "qcow2")
	r = newTofuRun(t)
	vc = tofuCreate()
	vc.Host, vc.SSHAllow = "vm.example.org", []string{"10.0.0.0/8"}
	if err := v.create(vc, r.mod(func(o *Options) {
		o.LookupIP = func(string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("198.51.100.7"), netip.MustParseAddr("198.51.100.8")}, nil
		}
		o.SourceAddr = func(netip.Addr) (netip.Addr, error) { return netip.MustParseAddr("192.0.2.1"), nil }
	})); err != nil || strings.Count(v.log.String(), "--ssh-allow leaves out 192.0.2.1") != 1 {
		t.Errorf("%v\n%s", err, v.log)
	}
	// Static addresses of both families, no --host: each is checked.
	v = newVMCreate(t, "qcow2")
	r = newTofuRun(t)
	vc = tofuCreate()
	vc.Host, vc.Network, vc.Address, vc.IPv6, vc.Address6, vc.SSHAllow = "", "static", "10.1.2.10/24", true, "2001:db8::10/64", []string{"10.0.0.0/8"}
	if err := v.create(vc, r.mod(func(o *Options) {
		o.SourceAddr = func(dst netip.Addr) (netip.Addr, error) {
			if dst.Is6() {
				return netip.MustParseAddr("2001:db8::1"), nil
			}
			return netip.MustParseAddr("10.1.2.3"), nil
		}
	})); err != nil || !strings.Contains(v.log.String(), "--ssh-allow leaves out 2001:db8::1, the address this machine reaches 2001:db8::10") {
		t.Errorf("%v\n%s", err, v.log)
	}
	// A link-local source carries its zone: checked without it.
	v = newVMCreate(t, "qcow2")
	r = newTofuRun(t)
	vc = tofuCreate()
	vc.Host, vc.SSHAllow = "fe80::7%eth0", []string{"fe80::/10"}
	if err := v.create(vc, r.mod(func(o *Options) {
		o.SourceAddr = func(netip.Addr) (netip.Addr, error) { return netip.MustParseAddr("fe80::1%eth0"), nil }
	})); err != nil || strings.Contains(v.log.String(), "leaves out") {
		t.Errorf("%v\n%s", err, v.log)
	}
	// A name that does not resolve yet is not checked.
	v = newVMCreate(t, "qcow2")
	r = newTofuRun(t)
	vc = tofuCreate()
	vc.SSHAllow = []string{"10.0.0.0/8"}
	if err := v.create(vc, r.mod(func(o *Options) {
		o.LookupIP = func(string) ([]netip.Addr, error) { return nil, errors.New("no such host") }
	})); err != nil || !strings.Contains(v.log.String(), "--ssh-allow: notes-1.lan does not resolve yet") {
		t.Errorf("%v\n%s", err, v.log)
	}
}

func TestVMCreateAsksHowAndWhere(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	r := newTofuRun(t)
	p := &fakePrompter{answers: map[string]string{"--platform": "truenas", "--create": CreateTofu, "--hostname": "asked", "--user": "ops", "--ssh-key": v.key,
		"--network": "dhcp", "--tls": "internal", "APP_DOMAIN": "app.example.org", "API_DOMAIN": "api.example.org", "ADMIN_EMAIL": "a@example.org",
		"--vm-name": "", "--pool": "tank", "--dataset": "vms/notes", "--bridge": "br1", "--cpus": "4", "--memory": "", "--disk": "64", "confirm": "yes"}}
	in := v.installer(r.mod(func(o *Options) { o.Prompt, o.TLS, o.Set = p, "", nil }))
	if err := in.CreateVM(context.Background(), &VMCreate{Ctl: "/opt/notes-stow", Wait: 5 * time.Second}); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	var asked []string
	for _, f := range p.forms {
		for _, q := range f.Questions {
			asked = append(asked, q.Key)
		}
	}
	want := []string{"--platform", "--create", "--hostname", "--user", "--ssh-key", "--network", "--tls", "APP_DOMAIN", "API_DOMAIN", "ADMIN_EMAIL",
		"--vm-name", "--pool", "--dataset", "--bridge", "--cpus", "--memory", "--disk", "confirm"}
	if !slices.Equal(asked, want) {
		t.Errorf("asked\n%v\nwant\n%v", asked, want)
	}
	var vars tofuVars
	b, _ := os.ReadFile(filepath.Join(r.state, "notes", "vms", "asked", "terraform.tfvars.json"))
	_ = json.Unmarshal(b, &vars)
	if vars.Name != "asked" || vars.Dataset != "vms/notes" || vars.Bridge != "br1" || vars.CPUs != 4 || vars.MemoryMiB != 4096 || vars.DiskGiB != 64 {
		t.Errorf("variables %+v", vars)
	}
	if a := r.ssh.calls[0]; !slices.Contains(a, "ops@asked") {
		t.Errorf("ssh %q: want the host name", a)
	}
	// Where the built-in module is a preview, by hand is the default.
	q := &fakePrompter{answers: map[string]string{"--platform": "vmware", "--network": "dhcp", "--tls": "internal", "--ssh-key": v.key,
		"APP_DOMAIN": "a.example.org", "API_DOMAIN": "b.example.org", "ADMIN_EMAIL": "a@example.org", "confirm": "yes"}}
	v2 := newVMCreate(t, "ova")
	if err := v2.create(VMCreate{}, func(o *Options) { o.Prompt = q }); err != nil {
		t.Fatalf("%v\n%s", err, v2.log)
	}
	for _, f := range q.forms {
		if f.Questions[0].Key == "--create" && f.Defaults(nil)["--create"] != CreateManual {
			t.Error("OpenTofu is the default on vmware, a preview")
		}
	}
}

func TestDestroyVM(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	r := newTofuRun(t)
	in := v.installer(r.mod(nil))
	ctx := context.Background()
	if err := in.DestroyVM(ctx, &VMDestroy{Host: "notes-1", Yes: true, Ctl: "/opt/notes-stow"}); err == nil || !strings.Contains(err.Error(), "holds no state") {
		t.Fatalf("no VM: %v", err)
	}
	if err := in.DestroyVM(ctx, &VMDestroy{Host: "../x", Yes: true}); err == nil || !strings.Contains(err.Error(), "not a host name") {
		t.Fatalf("a path: %v", err)
	}
	if err := v.create(tofuCreate(), r.mod(nil)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(r.state, "notes", "vms", "notes-1")
	r.tofu.calls = nil
	if err := in.DestroyVM(ctx, &VMDestroy{Host: "notes-1", Ctl: "/opt/notes-stow"}); err == nil || !strings.Contains(err.Error(), "give --yes") {
		t.Fatalf("no terminal, no --yes: %v", err)
	}
	p := &fakePrompter{answers: map[string]string{"confirm": "no"}}
	in.O.Prompt = p
	if err := in.DestroyVM(ctx, &VMDestroy{Host: "notes-1", Ctl: "/opt/notes-stow"}); err != ErrAborted {
		t.Fatalf("answered no: %v", err)
	}
	if p.forms[0].Defaults(nil)["confirm"] != "no" {
		t.Error("destroying is the default")
	}
	for _, c := range r.tofu.calls {
		if c[0] != "version" {
			t.Fatalf("tofu ran %q before the operator said yes", c)
		}
	}
	p.answers["confirm"] = "yes"
	if err := in.DestroyVM(ctx, &VMDestroy{Host: "notes-1", Ctl: "/opt/notes-stow-moved"}); err != nil {
		t.Fatal(err)
	}
	last := r.tofu.calls[len(r.tofu.calls)-1]
	if !slices.Equal(last, []string{"-chdir=" + dir, "destroy", "-input=false", "-auto-approve", "-no-color"}) {
		t.Errorf("tofu ran %q", last)
	}
	if !slices.Contains(r.tofu.envs[len(r.tofu.envs)-1], CtlVar+"=/opt/notes-stow-moved") {
		t.Error("destroy's steps do not run this ctl")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the state is left: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.cch, "notes", "disks", strings.TrimPrefix(v.r.Manifest.Artifacts.Disks["qcow2"], "sha256:")[:16], "notes-1.0.0.qcow2")); err != nil {
		t.Errorf("the disk cache went: %v", err)
	}
}

func TestTrueNASStepsNeedTheirEnvironment(t *testing.T) {
	t.Parallel()
	for step, missing := range map[string]string{"import-image": "IMAGE", "seed": "SEED", "start": "VM_ID", "stop": "VM_ID", "drop-snapshots": "DATASET"} {
		env := map[string]string{"TRUENAS_URL": "https://127.0.0.1:1", "TRUENAS_API_KEY": "k"}
		cmd := truenasCommand(func(k string) string { return env[k] })
		cmd.SetArgs([]string{step})
		cmd.SetOut(&strings.Builder{})
		cmd.SetErr(&strings.Builder{})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), missing+" is not set") {
			t.Errorf("%s: %v", step, err)
		}
	}
	env := map[string]string{"VM_ID": "x", "TRUENAS_URL": "ftp://h", "TRUENAS_API_KEY": "k"}
	cmd := truenasCommand(func(k string) string { return env[k] })
	cmd.SetArgs([]string{"start"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "TRUENAS_URL") {
		t.Errorf("a bad URL: %v", err)
	}
}

func TestVMCreateWithOpenTofuRefusesATrueNASNameTaken(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	r := newTofuRun(t)
	c := tofuCreate()
	c.Hostname = "notes1"
	if err := v.create(c, r.mod(nil)); err != nil {
		t.Fatal(err)
	}
	// notes-1's default name is notes1's too, whatever its case: its zvol would be the first's.
	c = tofuCreate()
	err := v.create(c, r.mod(nil))
	if err == nil || !strings.Contains(err.Error(), "the VM notes1, of host notes1, is named notes1 on its platform already") {
		t.Fatalf("err = %v", err)
	}
	c.Name = "NOTES1"
	if err := v.create(c, r.mod(nil)); err == nil || !strings.Contains(err.Error(), "--vm-name") {
		t.Fatalf("another case: %v", err)
	}
	c.Name = "notes1b"
	if err := v.create(c, r.mod(nil)); err != nil {
		t.Fatalf("another name: %v", err)
	}
}

func TestVMCreateWithOpenTofuIgnoresRelativeXDGDirectories(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	r := newTofuRun(t)
	r.vars["XDG_STATE_HOME"], r.vars["XDG_CACHE_HOME"] = "state", ".cache"
	if err := v.create(tofuCreate(), r.mod(nil)); err != nil {
		t.Fatal(err)
	}
	home := r.vars["HOME"]
	for _, p := range []string{filepath.Join(home, ".local", "state", "notes", "vms", "notes-1", varsFile), filepath.Join(home, ".cache", "notes", "disks", strings.TrimPrefix(v.r.Manifest.Artifacts.Disks["qcow2"], "sha256:")[:16], "notes-1.0.0.qcow2")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("not under HOME: %v", err)
		}
	}
	r.vars["HOME"] = "relative"
	c := tofuCreate()
	c.Hostname = "notes-2"
	if err := v.create(c, r.mod(nil)); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("a relative HOME: %v", err)
	}
}

func TestDestroyVMAfterAFailedApply(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	r := newTofuRun(t)
	r.tofu.fail = errors.New("interrupted")
	if err := v.create(tofuCreate(), r.mod(nil)); err == nil {
		t.Fatal("the apply did not fail")
	}
	// No state was written, but what the apply made may exist: a second create is refused, and
	// destroy runs.
	r.tofu.fail = nil
	if err := v.create(tofuCreate(), r.mod(nil)); err == nil || !strings.Contains(err.Error(), "vm destroy notes-1") {
		t.Fatalf("a second create: %v", err)
	}
	in := v.installer(r.mod(nil))
	if err := in.DestroyVM(context.Background(), &VMDestroy{Host: "notes-1", Yes: true, Ctl: "/opt/notes-stow"}); err != nil {
		t.Fatal(err)
	}
}

func TestExecToolInterruptsWithGrace(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	out := &lockedBuf{}
	done := make(chan error)
	go func() {
		_, err := ExecTool{Bin: "sh", Grace: 10 * time.Second}.Run(ctx, Cmd{
			Args: []string{"-c", `trap 'echo state written; exit 3' INT; echo ready; while :; do sleep 0.01; done`}, Stdout: out})
		done <- err
	}()
	deadline := time.After(5 * time.Second)
	for !strings.Contains(out.String(), "ready") {
		select {
		case err := <-done:
			t.Fatalf("ended before it was interrupted: %v %q", err, out.String())
		case <-deadline:
			t.Fatal("never started")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
	if !strings.Contains(out.String(), "state written") {
		t.Errorf("not interrupted: %q", out.String())
	}
}

// lockedBuf is a buffer one goroutine writes while another reads it.
type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestVMCreateWithOpenTofuInterruptedKeepsTheVM(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	r := newTofuRun(t)
	ctx, cancel := context.WithCancel(context.Background())
	r.ssh.answers = []func() ([]byte, error){func() ([]byte, error) { cancel(); return nil, errors.New("interrupted") }}
	c := tofuCreate()
	c.SSHKey, c.Out = v.key, v.out
	in := v.installer(r.mod(func(o *Options) {
		o.Set = map[string]string{"APP_DOMAIN": "app.example.org", "API_DOMAIN": "api.example.org", "ADMIN_EMAIL": "admin@example.org"}
	}))
	err := in.CreateVM(ctx, &c)
	if err == nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "notes-stow vm destroy notes-1") {
		t.Fatalf("err = %v", err)
	}
}

func TestExecToolKillsTheGroupAfterTheGrace(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		// The background sleep ignores the interrupt, as a non-interactive shell's jobs do, and the
		// shell waits for it: only the group's kill ends it.
		_, err := ExecTool{Bin: "sh", Grace: 50 * time.Millisecond}.Run(ctx, Cmd{Args: []string{"-c", `sleep 30 & echo $! >` + pidFile + `; wait`}})
		done <- err
	}()
	deadline := time.After(5 * time.Second)
	for {
		if b, _ := os.ReadFile(pidFile); len(b) > 0 && b[len(b)-1] == '\n' {
			break
		}
		select {
		case <-deadline:
			t.Fatal("never started")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run outlived its grace")
	}
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	for range 500 {
		if p, err := os.FindProcess(pid); err != nil || p.Signal(syscall.Signal(0)) != nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Errorf("the tool's child %d outlived the grace", pid)
}
