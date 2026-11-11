package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/norvexlabs/stowage/internal/testutil"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
)

func TestGivenIsDecodedStrictly(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, raw, want string }{
		{"unknown field", "project: notes\ninputs: {A: b}\nextra: 1\n", "extra"},
		{"unknown tls field", "project: notes\ntls: {cert: c, key: k, mode: byo}\n", "mode"},
		{"another project", "project: crm\ninputs: {A: b}\n", `holds "crm"'s material`},
		{"no project", "inputs: {A: b}\n", `holds ""'s material`},
		{"nothing", "project: notes\n", "neither an input nor TLS material"},
		{"an empty tls", "project: notes\ntls: {}\n", "neither an input nor TLS material"},
		{"a cert alone", "project: notes\ntls: {cert: c}\n", "cert and key are given together"},
		{"a key alone", "project: notes\ntls: {key: k}\n", "cert and key are given together"},
		{"a CA alone", "project: notes\ntls: {ca: c}\n", "ca is the CA of its cert"},
		{"encrypted", "age-encryption.org/v1\n", "the file is encrypted"},
		{"empty", "", "--given: EOF"},
	} {
		if _, err := parseGiven([]byte(c.raw), "notes"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	head := "project: notes\ninputs: {A: \""
	at := func(n int) []byte { return []byte(head + strings.Repeat("v", n-len(head)-3) + "\"}\n") }
	if _, err := parseGiven(at(maxSecretsFile), "notes"); err != nil {
		t.Errorf("at the bound: %v", err)
	}
	if _, err := parseGiven(at(maxSecretsFile+1), "notes"); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("past the bound: %v", err)
	}
}

func FuzzParseGiven(f *testing.F) {
	f.Add("project: notes\ninputs: {A: b}\ntls: {cert: c, key: k, ca: a, client_ca: d}\n")
	f.Add("project: notes\ntls: [1]\n")
	f.Fuzz(func(t *testing.T, raw string) {
		g, err := parseGiven([]byte(raw), "notes")
		if err != nil {
			return
		}
		if g.Project != "notes" || (g.TLS != nil && (g.TLS.Cert == "") != (g.TLS.Key == "")) {
			t.Errorf("accepted %q: %+v", raw, g)
		}
	})
}

func TestTakeGivenLeavesToTheFlagsWhatTheyDecide(t *testing.T) {
	t.Parallel()
	const whole = "project: notes\ninputs: {API_KEY: given, OTHER: given}\ntls: {cert: C, key: K, ca: A, client_ca: L}\n"
	for _, c := range []struct {
		name                    string
		mod                     func(*Options)
		cert, key, ca, clientCA string
		left                    string
	}{
		{"nothing", func(*Options) {}, givenCert, givenKey, givenCA, givenClientCA, ""},
		{"--tls byo", func(o *Options) { o.TLS = "byo" }, givenCert, givenKey, givenCA, givenClientCA, ""},
		{"--cert", func(o *Options) { o.Cert = "c.pem" }, "c.pem", "", "", givenClientCA, "cert, key, ca"},
		{"--key", func(o *Options) { o.Key = "k.pem" }, "", "k.pem", "", givenClientCA, "cert, key, ca"},
		{"another mode", func(o *Options) { o.TLS = "acme" }, "", "", "", givenClientCA, "cert, key, ca"},
		{"--ca", func(o *Options) { o.CA = "a.pem" }, givenCert, givenKey, "a.pem", givenClientCA, "ca"},
		{"--client-ca", func(o *Options) { o.ClientCA = "none" }, givenCert, givenKey, givenCA, "none", "client_ca"},
	} {
		var log bytes.Buffer
		in := &Installer{R: &Release{Spec: &spec.Spec{Name: "notes"}}}
		in.O.Log = &log
		in.O.Answers = map[string]string{"OTHER": "answered"}
		c.mod(&in.O)
		if err := in.takeGiven([]byte(whole)); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		o := &in.O
		if o.Cert != c.cert || o.Key != c.key || o.CA != c.ca || o.ClientCA != c.clientCA {
			t.Errorf("%s: %q %q %q %q", c.name, o.Cert, o.Key, o.CA, o.ClientCA)
		}
		if o.Answers["API_KEY"] != "given" || o.Answers["OTHER"] != "answered" || o.Given == nil {
			t.Errorf("%s: answers %v", c.name, o.Answers)
		}
		if said := strings.Contains(log.String(), "tls: "+c.left+" left out"); said != (c.left != "") {
			t.Errorf("%s: log %q", c.name, log.String())
		}
	}
}

// On the recovery system an install is wiped by the reset to come, so a Given is refused: the step's
// `&& rm` then keeps the only copy.
func TestTakeGivenRefusesTheRecoverySystem(t *testing.T) {
	t.Parallel()
	cos := t.TempDir()
	in := &Installer{R: &Release{Spec: &spec.Spec{Name: "notes"}}}
	in.O.Log, in.O.Cos = io.Discard, cos
	raw := []byte("project: notes\ninputs: {A: b}\n")
	if err := in.takeGiven(raw); err != nil {
		t.Fatalf("no mark: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cos, "recovery_mode"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := in.takeGiven(raw); err == nil || !strings.Contains(err.Error(), "recovery system") {
		t.Errorf("%v", err)
	}
}

// readPEM reads a Given's PEM by its name, and any other name from the disk, a Given or not.
func TestReadPEMNeverReadsAGivenNameFromTheDisk(t *testing.T) {
	t.Parallel()
	o := &Options{givenTLS: &GivenTLS{Cert: "C", Key: "K", CA: "A", ClientCA: "L"}}
	for name, want := range map[string]string{givenCert: "C", givenKey: "K", givenCA: "A", givenClientCA: "L"} {
		if b, err := o.readPEM(name); err != nil || string(b) != want {
			t.Errorf("%s: %q %v", name, b, err)
		}
	}
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := o.readPEM(path); err != nil || string(b) != "disk" {
		t.Errorf("%q %v", b, err)
	}
}

// The CA and the client CA checks judge a Given's PEM as they judge a file.
func TestTheCAChecksReadAGivensPEM(t *testing.T) {
	t.Parallel()
	ca, cert, key := writeCAChain(t, "notes.example.com")
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	in := &Installer{R: &Release{Spec: &spec.Spec{Name: "notes", Front: spec.Front{Routes: []spec.Route{{Name: "web", ClientAuth: true}}}}}}
	in.O.givenTLS = &GivenTLS{Cert: read(cert), Key: read(key), CA: read(ca), ClientCA: read(ca)}
	in.O.Cert, in.O.Key, in.O.CA, in.O.ClientCA = givenCert, givenKey, givenCA, givenClientCA
	c, err := in.O.readPair(in.O.Cert, in.O.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := in.readCA(c); err != nil || string(c.ca) != read(ca) {
		t.Errorf("the CA: %v, %q", err, c.ca)
	}
	if got, err := in.readClientCA(); err != nil || len(got) == 0 {
		t.Errorf("the client CA: %v", err)
	}
	in.O.givenTLS.ClientCA = "not PEM"
	if _, err := in.readClientCA(); err == nil || !strings.Contains(err.Error(), givenClientCA) {
		t.Errorf("a bad client CA: %v", err)
	}
}

// A whole first install from a Given, its certificate copied in and its secret input recorded, then
// the same file again, as a retry after a failure runs it.
func TestAnInstallTakesAGivenAndARetryTakesItAgain(t *testing.T) {
	t.Parallel()
	cert, key := writeCert(t, time.Now().AddDate(1, 0, 0), "notes.example.com", "api.example.com")
	certPEM, _ := os.ReadFile(cert)
	keyPEM, _ := os.ReadFile(key)
	e := newEnv(t)
	e.r.Spec.Inputs = append(e.r.Spec.Inputs, spec.Input{Key: "API_KEY", Prompt: "API key", Secret: true, Required: true})
	raw := givenYAML(t, &Given{Project: "notes", Inputs: map[string]string{"API_KEY": "the-api-key"}, TLS: &GivenTLS{Cert: string(certPEM), Key: string(keyPEM)}})
	run := func() error {
		in := e.installer(func(o *Options) { tlsDomains(o) })
		if err := in.takeGiven(raw); err != nil {
			t.Fatal(err)
		}
		return in.Install(context.Background())
	}
	for _, attempt := range []string{"first", "again"} {
		if err := run(); err != nil {
			t.Fatalf("%s: %v\n%s", attempt, err, e.log)
		}
		got, err := os.ReadFile(filepath.Join(e.dir, TLSDir, "key.pem"))
		if err != nil || !bytes.Equal(got, keyPEM) {
			t.Errorf("%s: the key: %v", attempt, err)
		}
		env, err := ReadEnvFile(filepath.Join(e.dir, envName))
		if err != nil {
			t.Fatal(err)
		}
		if v, _ := env.Get("API_KEY"); v != "the-api-key" {
			t.Errorf("%s: API_KEY %q", attempt, v)
		}
	}
}

func givenYAML(t *testing.T, g *Given) []byte {
	t.Helper()
	raw, err := marshalGiven(g)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// byoVM is what `vm create` is given for a VM with a certificate and a secret input.
func byoVM(t *testing.T, v *vmCreate) (secretMark string, keyPEM []byte, mod func(*Options)) {
	t.Helper()
	cert, key := writeCert(t, time.Now().AddDate(1, 0, 0), "app.example.org", "api.example.org")
	keyPEM, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	v.r.Spec.Inputs = append(v.r.Spec.Inputs, spec.Input{Key: "API_KEY", Prompt: "API key", Secret: true, Required: true})
	return "the-api-key-value", keyPEM, func(o *Options) {
		o.Cert, o.Key = cert, key
		o.Answers = map[string]string{"API_KEY": "the-api-key-value", "ADMIN_EMAIL": "answered@example.org"}
		delete(o.Set, "ADMIN_EMAIL")
	}
}

func TestVMCreateByHandGivesAByoCertificateAndASecretThroughAFile(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	mark, keyPEM, mod := byoVM(t, v)
	v.out = filepath.Join(v.out, "My VMs") // ssh splits an option's value at a space, unless quoted
	if err := v.create(VMCreate{Platform: "proxmox"}, mod); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	// Never the seed: it says only that the install waits, and names no mode, as the settings refuse byo.
	ud, _, _ := readSeed(t, filepath.Join(v.out, "notes-seed.iso"))
	st, _ := cloudSettings(t, ud)
	if !st.AwaitSecrets || st.TLS != "" || strings.Contains(ud, mark) || strings.Contains(ud, "API_KEY") || strings.Contains(ud, strings.Split(string(keyPEM), "\n")[1]) {
		t.Errorf("seed:\n%s", ud)
	}
	if !strings.Contains(ud, "path: /etc/notes/install.yaml.given") || !strings.Contains(ud, "content: |\n") && !strings.Contains(ud, "byo") {
		t.Errorf("no mark of the wait in the seed:\n%s", ud)
	}
	file := filepath.Join(v.out, "notes.given")
	fi, err := os.Stat(file)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	raw, _ := os.ReadFile(file)
	g, err := parseGiven(raw, "notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Inputs) != 1 || g.Inputs["API_KEY"] != mark || g.TLS.Key != string(keyPEM) {
		t.Errorf("%+v", g)
	}
	// The review names them, never a value; the mode is the certificate's.
	log := v.log.String()
	if !strings.Contains(log, "served: byo;") || !strings.Contains(log, "(never these): the certificate and its key, API_KEY") || strings.Contains(log, mark) {
		t.Errorf("log:\n%s", log)
	}
	if strings.Contains(v.out2(), mark) {
		t.Errorf("out:\n%s", v.out2())
	}
	// Run as printed, against a stand-in ssh: pinned to the key the seed carries, the file on its
	// standard input, removed once it took it, kept when it failed.
	step := ""
	for _, l := range strings.Split(v.out2(), "\n") {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "ssh -o HostKeyAlias=") {
			step = t
		}
	}
	if step == "" {
		t.Fatalf("no step in:\n%s", v.out2())
	}
	step = strings.ReplaceAll(step, "<address>", "192.0.2.9")
	bin, seen := t.TempDir(), filepath.Join(t.TempDir(), "seen")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + seen + ".args\ncat > " + seen + ".stdin\nexit ${SSH_EXIT:-0}\n"
	if err := testutil.WriteExecutable(filepath.Join(bin, "ssh"), []byte(stub)); err != nil {
		t.Fatal(err)
	}
	run := func(exit string) error {
		cmd := exec.Command("/bin/sh", "-c", step)
		cmd.Env = []string{"PATH=" + bin + ":/bin:/usr/bin", "SSH_EXIT=" + exit}
		return cmd.Run()
	}
	if err := run("1"); err == nil {
		t.Error("a failed install reads as done")
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("a failed install took the file away: %v", err)
	}
	if err := run("0"); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(seen + ".args")
	in, _ := os.ReadFile(seen + ".stdin")
	if !bytes.Equal(in, raw) {
		t.Errorf("stdin %q", in)
	}
	for _, want := range []string{"HostKeyAlias=" + seedHostAlias, "StrictHostKeyChecking=yes", `UserKnownHostsFile="` + file + `.known_hosts"`, "@192.0.2.9 -- sudo notes-stow install --given - --no-input"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("no %q in %s", want, args)
		}
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file stays: %v", err)
	}
	if !strings.Contains(v.out2(), givenRetired) {
		t.Errorf("no word of a retired key in:\n%s", v.out2())
	}
	if strings.Contains(step, "BatchMode") {
		t.Errorf("an operator's step refuses a key's passphrase: %s", step)
	}
}

func TestVMCreateWithOpenTofuGivesAByoCertificateOverThePinnedSSH(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	mark, keyPEM, mod := byoVM(t, v)
	r := newTofuRun(t)
	const own = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMttF2bIpr7R386X4GUm0TkSzgnDFwXOzWeDxQGI7oDJ"
	r.ssh.answers = []func() ([]byte, error){refused, statusIn("recovery", false), statusJSON(false), statusJSON(false),
		func() ([]byte, error) { return []byte(own + " root@notes-1\n"), nil }}
	if err := v.create(tofuCreate(), r.mod(func(o *Options) { o.TLS = ""; mod(o) })); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	if len(r.ssh.calls) != 5 || !strings.HasSuffix(strings.Join(r.ssh.calls[3], " "), "sudo notes-stow install --given - --no-input") {
		t.Fatalf("ssh %v", r.ssh.calls)
	}
	g, err := parseGiven([]byte(r.ssh.stdin[3]), "notes")
	if err != nil || g.Inputs["API_KEY"] != mark || g.TLS.Key != string(keyPEM) {
		t.Errorf("%+v %v", g, err)
	}
	for _, call := range r.ssh.calls[1:] {
		if !strings.Contains(strings.Join(call, " "), "StrictHostKeyChecking=yes") {
			t.Errorf("not pinned: %v", call)
		}
	}
	dir := filepath.Join(r.state, "notes", "vms", "notes-1")
	seed, _, _ := readSeed(t, filepath.Join(dir, "seed.iso"))
	if st, _ := cloudSettings(t, seed); !st.AwaitSecrets || strings.Contains(seed, mark) || strings.Contains(seed, strings.Split(string(keyPEM), "\n")[1]) {
		t.Errorf("seed:\n%s", seed)
	}
	if _, err := os.Stat(filepath.Join(dir, "given.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a done install keeps the material on disk: %v", err)
	}
	if !strings.Contains(v.out2(), "The VM notes-1 runs notes 1.0.0") {
		t.Errorf("out:\n%s", v.out2())
	}
}

// A VM that never answers keeps what it waits for, and the error says how to give it.
func TestVMCreateWithOpenTofuNeverAnsweringKeepsWhatItWaitsFor(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	_, _, mod := byoVM(t, v)
	r := newTofuRun(t)
	r.ssh.answers = []func() ([]byte, error){refused}
	c := tofuCreate()
	c.Wait = 50 * time.Millisecond
	err := v.create(c, r.mod(func(o *Options) { o.TLS = ""; mod(o) }))
	file := filepath.Join(r.state, "notes", "vms", "notes-1", "given.yaml")
	if err == nil || !strings.Contains(err.Error(), "not ready after") || !strings.Contains(err.Error(), "< "+file+" && rm ") {
		t.Fatalf("%v", err)
	}
	if fi, serr := os.Stat(file); serr != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("%v %v", fi, serr)
	}
}

// OpenTofu failing past the variables may leave a VM waiting: what it waits for stays, for `vm destroy`
// to remove.
func TestVMCreateWithOpenTofuFailingKeepsWhatAVMMayWaitFor(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	_, _, mod := byoVM(t, v)
	r := newTofuRun(t)
	r.tofu.fail = errors.New("the apply failed")
	if err := v.create(tofuCreate(), r.mod(func(o *Options) { o.TLS = ""; mod(o) })); err == nil {
		t.Fatal("no failure")
	}
	dir := filepath.Join(r.state, "notes", "vms", "notes-1")
	for _, f := range []string{"given.yaml", "given.yaml.known_hosts"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s is gone: %v", f, err)
		}
	}
}

// OpenTofu failing before the variables leaves no VM, and no `vm destroy` finds this state: nothing
// of what was to be given stays on this machine.
func TestVMCreateWithOpenTofuFailingBeforeAnyVMKeepsNothing(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	_, _, mod := byoVM(t, v)
	r := newTofuRun(t)
	dir := filepath.Join(r.state, "notes", "vms", "notes-1")
	if err := os.MkdirAll(filepath.Join(dir, platformFile, "in-the-way"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := v.create(tofuCreate(), r.mod(func(o *Options) { o.TLS = ""; mod(o) }))
	if err == nil || !strings.Contains(err.Error(), "no VM was made") {
		t.Fatalf("%v", err)
	}
	for _, f := range []string{"given.yaml", "given.yaml.known_hosts"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: %v", f, err)
		}
	}
}

// The default --out is the working directory, and the step runs from anywhere.
func TestTheGivenStepNamesItsFileWhereverItRuns(t *testing.T) {
	v := newVMCreate(t, "qcow2")
	_, _, mod := byoVM(t, v)
	wd := t.TempDir()
	t.Chdir(wd)
	if err := v.create(VMCreate{Platform: "proxmox", Out: "."}, mod); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	if !strings.Contains(v.out2(), "< "+filepath.Join(wd, "notes.given")+" && rm ") {
		t.Errorf("out:\n%s", v.out2())
	}
}

// The install fails over SSH: the VM stays, and the error's step gives the material again.
func TestVMCreateWithOpenTofuFailingOverSSHSaysHowToGiveItAgain(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	mark, _, mod := byoVM(t, v)
	r := newTofuRun(t)
	r.ssh.answers = []func() ([]byte, error){refused, statusIn("recovery", false), statusJSON(false),
		func() ([]byte, error) { return nil, fmt.Errorf("the install failed") }}
	err := v.create(tofuCreate(), r.mod(func(o *Options) { o.TLS = ""; mod(o) }))
	dir := filepath.Join(r.state, "notes", "vms", "notes-1")
	file := filepath.Join(dir, "given.yaml")
	if err == nil || !strings.Contains(err.Error(), "the install failed") || strings.Contains(err.Error(), mark) ||
		!strings.Contains(err.Error(), "install --given - --no-input < "+file+" && rm ") || !strings.Contains(err.Error(), "vm destroy notes-1") {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(err.Error(), givenRetired) || !strings.Contains(err.Error(), "ssh must show ") {
		t.Errorf("no word of the one-time key: %v", err)
	}
	if !strings.Contains(err.Error(), "-i ") {
		t.Errorf("the step leaves out the --ssh-key every call uses: %v", err)
	}
	fi, serr := os.Stat(file)
	if serr != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, serr)
	}
	raw, _ := os.ReadFile(file)
	if g, err := parseGiven(raw, "notes"); err != nil || g.Inputs["API_KEY"] != mark {
		t.Errorf("%+v %v", g, err)
	}
}

// With a disk of another release, nothing is sent to the OS the first boot is leaving: it would die
// in that boot's reboot.
func TestVMCreateOnAnotherReleasesDiskGivesToTheReleasesOS(t *testing.T) {
	t.Parallel()
	in := &Installer{R: &Release{Spec: &spec.Spec{Name: "notes"}}}
	calls := 0
	old, want := Status{Name: "notes", OS: Version{"0.9.0", 1}, Boot: "active"}, Status{Name: "notes", OS: Version{"1.0.0", 1}, Boot: "active"}
	in.O.Log, in.O.SSH = io.Discard, sshFunc(func(Cmd) ([]byte, error) {
		calls++
		if calls == 1 {
			return json.Marshal(old)
		}
		return json.Marshal(want)
	})
	release := &OSRelease{Version: "1.0.0", Revision: 1, Tag: "1.0.0-r1"}
	if err := in.awaitVM(context.Background(), t.TempDir(), "", "ops", "192.0.2.9", "", 5*time.Second, false, release); err != nil || calls != 2 {
		t.Errorf("%v after %d calls", err, calls)
	}
}

// On a disk of another release, `vm create` itself gives nothing to the OS it boots away from.
func TestVMCreateWithOpenTofuOnAnotherReleasesDiskWaitsForTheReleasesOS(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	bootVia(t, v, &release.BootDisks{Release: "0.9.0", Stowage: "0.6.0", Disks: maps.Clone(v.r.Manifest.Artifacts.Disks)}, "0.6.0")
	mark, _, mod := byoVM(t, v)
	r := newTofuRun(t)
	old := func() ([]byte, error) {
		return json.Marshal(Status{Name: "notes", OS: Version{"0.9.0", 1}, Job: "idle", Boot: "active"})
	}
	r.ssh.answers = []func() ([]byte, error){old, statusJSON(false), statusJSON(false), func() ([]byte, error) {
		return []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMttF2bIpr7R386X4GUm0TkSzgnDFwXOzWeDxQGI7oDJ root@notes-1\n"), nil
	}}
	if err := v.create(tofuCreate(), r.mod(func(o *Options) { o.TLS = ""; mod(o) })); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	if len(r.ssh.stdin) != 4 || strings.Contains(r.ssh.stdin[0], mark) || strings.Contains(r.ssh.stdin[1], mark) || !strings.Contains(r.ssh.stdin[2], mark) {
		t.Errorf("stdin %q", r.ssh.stdin)
	}
}

// By hand, the step says which OS it is for.
func TestVMCreateByHandOnAnotherReleasesDiskSaysWhichOS(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	bootVia(t, v, &release.BootDisks{Release: "0.9.0", Stowage: "0.6.0", Disks: maps.Clone(v.r.Manifest.Artifacts.Disks)}, "0.6.0")
	_, _, mod := byoVM(t, v)
	if err := v.create(VMCreate{Platform: "proxmox"}, mod); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	if !strings.Contains(v.out2(), "Its disk carries 0.9.0") {
		t.Errorf("out:\n%s", v.out2())
	}
}

// A boot that failed before it took the release's OS is said, and is waited past: a unit its own
// reboot stopped reads the same.
func TestAwaitVMSaysAFailureOfTheOSItBootsAwayFrom(t *testing.T) {
	t.Parallel()
	in := &Installer{R: &Release{Spec: &spec.Spec{Name: "notes"}}}
	calls := 0
	in.O.Log, in.O.SSH = io.Discard, sshFunc(func(Cmd) ([]byte, error) {
		calls++
		if calls > 1 {
			return json.Marshal(Status{Name: "notes", OS: Version{"1.0.0", 1}, Boot: "active"})
		}
		return json.Marshal(Status{Name: "notes", OS: Version{"0.9.0", 1}, Boot: "active", InstallFailed: "the bundle would not write"})
	})
	release := &OSRelease{Version: "1.0.0", Revision: 1, Tag: "1.0.0-r1"}
	if err := in.awaitVM(context.Background(), t.TempDir(), "", "ops", "192.0.2.9", "", 5*time.Second, false, release); err != nil || calls != 2 {
		t.Errorf("%v after %d calls", err, calls)
	}
	calls = -1000
	err := in.awaitVM(context.Background(), t.TempDir(), "", "ops", "192.0.2.9", "", 50*time.Millisecond, false, release)
	if err == nil || !strings.Contains(err.Error(), "the bundle would not write") || !strings.Contains(err.Error(), "not ready after") {
		t.Errorf("%v", err)
	}
}

func TestVMCreateRefusesWhatItCouldNotGive(t *testing.T) {
	t.Parallel()
	otherCert, otherKey := writeCert(t, time.Now().AddDate(1, 0, 0), "other.example.org")
	expiredCert, expiredKey := writeCert(t, time.Now().AddDate(-1, 0, 0), "app.example.org", "api.example.org")
	goodCert, goodKey := writeCert(t, time.Now().AddDate(1, 0, 0), "app.example.org", "api.example.org")
	secret := func(required bool) func(*spec.Spec) {
		return func(s *spec.Spec) {
			s.Inputs = append(s.Inputs, spec.Input{Key: "API_KEY", Prompt: "API key", Secret: true, Required: required})
		}
	}
	for _, c := range []struct {
		name   string
		mod    func(*spec.Spec)
		create VMCreate
		opts   func(*Options)
		want   string
	}{
		{"a certificate not covering the domains", nil, VMCreate{}, func(o *Options) { o.Cert, o.Key = otherCert, otherKey }, "the certificate does not cover"},
		{"an expired certificate", nil, VMCreate{}, func(o *Options) { o.Cert, o.Key = expiredCert, expiredKey }, "the certificate expired"},
		{"half a pair", nil, VMCreate{}, func(o *Options) { o.Cert = goodCert }, "--tls byo needs --cert FILE and --key FILE"},
		{"--set of a secret", secret(true), VMCreate{}, func(o *Options) { o.TLS = "internal"; o.Set = map[string]string{"API_KEY": "v"} }, "a command line (--set, --set-file) never carries"},
		{"--secrets beside a certificate", nil, VMCreate{Secrets: []byte("project: notes\nsecrets: {}\n")}, func(o *Options) { o.Cert, o.Key = goodCert, goodKey }, "--secrets restores an install, and the certificate and its key would go with it"},
		{"--secrets beside a secret input", secret(false), VMCreate{Secrets: []byte("project: notes\nsecrets: {}\n")}, func(o *Options) { o.TLS = "internal"; o.Answers = map[string]string{"API_KEY": "v"} }, "--secrets restores an install, and API_KEY would go with it"},
		{"a required secret nothing gives", secret(true), VMCreate{}, func(o *Options) { o.Cert, o.Key = goodCert, goodKey }, "also needs API_KEY"},
		{"a secret its check refuses", secret(false), VMCreate{}, func(o *Options) { o.TLS = "internal"; o.Answers = map[string]string{"API_KEY": "a\nb"} }, "API_KEY: "},
		{"more than the install takes", func(s *spec.Spec) {
			for n := range 20 {
				s.Inputs = append(s.Inputs, spec.Input{Key: fmt.Sprintf("BIG_%d", n), Prompt: "big", Secret: true})
			}
		}, VMCreate{}, func(o *Options) {
			o.TLS, o.Answers = "internal", map[string]string{}
			for n := range 20 {
				o.Answers[fmt.Sprintf("BIG_%d", n)] = strings.Repeat("v", 60000)
			}
		}, "the install over SSH takes"},
		{"a mode without a certificate", nil, VMCreate{}, func(o *Options) { o.TLS = "internal"; o.Cert, o.Key = goodCert, goodKey }, "--cert and --key are for --tls byo"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			v := newVMCreate(t, "qcow2")
			if c.mod != nil {
				c.mod(v.r.Spec)
			}
			create := c.create
			create.Platform = "proxmox"
			err := v.create(create, c.opts)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v, want %q", err, c.want)
			}
			if _, serr := os.Stat(filepath.Join(v.out, "notes-seed.iso")); !errors.Is(serr, os.ErrNotExist) {
				t.Errorf("a seed was written: %v", serr)
			}
		})
	}
	// ssh would read a token in the pin's path, so the step could never run.
	v0 := newVMCreate(t, "qcow2")
	_, _, mod0 := byoVM(t, v0)
	if err := v0.create(VMCreate{Platform: "proxmox", Out: filepath.Join(t.TempDir(), "100%")}, mod0); err == nil || !strings.Contains(err.Error(), "--out: ") {
		t.Errorf("%%: %v", err)
	}
	// A file of a VM waiting is never replaced, nor is a seed written beside it.
	v := newVMCreate(t, "qcow2")
	_, _, mod := byoVM(t, v)
	if err := os.MkdirAll(v.out, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(v.out, "notes.given")
	if err := os.WriteFile(file, []byte("waiting"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := v.create(VMCreate{Platform: "proxmox"}, mod); err == nil || !strings.Contains(err.Error(), file+" exists") {
		t.Errorf("%v", err)
	}
	if b, _ := os.ReadFile(file); string(b) != "waiting" {
		t.Errorf("replaced: %q", b)
	}
	// Nor is the pin of one whose file was moved away.
	if err := os.Rename(file, file+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file+".known_hosts", []byte("pin"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := v.create(VMCreate{Platform: "proxmox"}, mod); err == nil || !strings.Contains(err.Error(), file+".known_hosts exists") {
		t.Errorf("%v", err)
	}
	if _, err := os.Stat(filepath.Join(v.out, "notes-seed.iso")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a seed was written: %v", err)
	}
}

// A project serving byo alone can make a VM once its certificate is given, and is told so before.
func TestAByoOnlyProjectMakesAVMWithItsCertificate(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	v.r.Spec.Front.TLSModes = []string{"byo"}
	if err := v.create(VMCreate{Platform: "proxmox"}, nil); err == nil || !strings.Contains(err.Error(), "give --cert and --key") {
		t.Errorf("%v", err)
	}
	v = newVMCreate(t, "qcow2")
	v.r.Spec.Front.TLSModes = []string{"byo"}
	cert, key := writeCert(t, time.Now().AddDate(1, 0, 0), "app.example.org", "api.example.org")
	if err := v.create(VMCreate{Platform: "proxmox"}, func(o *Options) { o.Cert, o.Key = cert, key }); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
}

type sshFunc func(Cmd) ([]byte, error)

func (f sshFunc) Run(_ context.Context, c Cmd) ([]byte, error) { return f(c) }

// The VM's own command takes --given, before anything touches the host, and alone.
func TestTheVMInstallTakesGiven(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	run := func(stdin string, args ...string) (string, error) {
		var log bytes.Buffer
		cmd := vmCommand(e.r, e.self, io.Discard, &log, func(bool) Prompter { return nil }, clientTimes{unit: time.Millisecond})
		cmd.SetIn(strings.NewReader(stdin))
		cmd.SetArgs(append([]string{"install", "--no-input", "--dir", filepath.Join(t.TempDir(), "install")}, args...))
		err := cmd.Execute()
		return log.String(), err
	}
	if _, err := run("", "--given", "-", "--secrets", "-"); err == nil || !strings.Contains(err.Error(), "give one") {
		t.Errorf("both: %v", err)
	}
	if _, err := run("project: crm\ninputs: {A: b}\n", "--given", "-"); err == nil || !strings.Contains(err.Error(), `holds "crm"'s material`) {
		t.Errorf("another project's: %v", err)
	}
	if _, err := run("", "--given", filepath.Join(t.TempDir(), "gone")); err == nil || !strings.Contains(err.Error(), "--given: ") {
		t.Errorf("a missing file: %v", err)
	}
	log, err := run("project: notes\ntls: {cert: c, key: k}\n", "--given", "-", "--tls", "internal", "--channel", "Bad Channel")
	if err == nil || !strings.Contains(err.Error(), "is not a channel name") || !strings.Contains(log, "--given: tls: cert, key left out") {
		t.Errorf("%v, log %q", err, log)
	}
}

// Each wait is met by what it waits for alone: a Given stands in for no restored secrets, as the
// install would generate new ones the backups do not match.
func TestAGivenMeetsItsOwnWaitAlone(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	e.vars = nil
	waits := func(mark string) func(*Options) {
		with := settings(t, "await_secrets: true\ninputs: { ADMIN_EMAIL: ops@example.org }\n")
		var o Options
		with(&o)
		if mark != "" {
			if err := os.WriteFile(AwaitGivenFile(o.Settings), []byte(mark+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return with
	}
	given := func(with func(*Options)) func(*Options) {
		return func(o *Options) { with(o); o.Given = &Given{Project: "notes"} }
	}
	if err := e.installer(given(waits(""))).withSettings(); err == nil || !strings.Contains(err.Error(), "--secrets FILE` gives them") {
		t.Errorf("a Given on a --secrets VM: %v", err)
	}
	if err := e.installer(func(o *Options) { waits("given")(o); o.Secrets = []byte("x") }).withSettings(); err == nil || !strings.Contains(err.Error(), "--given FILE` gives them") {
		t.Errorf("--secrets on a Given wait: %v", err)
	}
	if err := e.installer(func(o *Options) { waits("")(o); o.Secrets = []byte("x") }).withSettings(); err != nil {
		t.Errorf("--secrets on its own wait: %v", err)
	}
	if err := e.installer(given(waits("given"))).withSettings(); err != nil {
		t.Errorf("a Given on its own wait: %v", err)
	}
	if err := e.installer(waits("given")).withSettings(); err == nil || !strings.Contains(err.Error(), "`sudo notes-stow install --given FILE` gives them") {
		t.Errorf("nothing on the Given wait: %v", err)
	}
	if err := e.installer(func(o *Options) { waits("given")(o); o.NewSecrets = true }).withSettings(); err != nil {
		t.Errorf("--new-secrets: %v", err)
	}
	// A VM made to serve its own certificate stays in that mode, the material lost or not, so the
	// install asks for it rather than moving to another mode on its domains alone.
	in := e.installer(func(o *Options) { waits("byo")(o); o.NewSecrets = true })
	if err := in.withSettings(); err != nil || in.O.TLS != "byo" {
		t.Errorf("byo mark: %v, mode %q", err, in.O.TLS)
	}
	in = e.installer(func(o *Options) { waits("byo")(o); o.NewSecrets = true; o.TLS = "internal" })
	if err := in.withSettings(); err != nil || in.O.TLS != "internal" {
		t.Errorf("a flag over the mark: %v, mode %q", err, in.O.TLS)
	}
	var boot bytes.Buffer
	if err := e.installer(func(o *Options) { waits("given")(o); o.Log = &boot }).Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(boot.String(), "`sudo notes-stow install --given FILE` gives them") {
		t.Errorf("boot: %s", boot.String())
	}
	byo := settings(t, "tls: byo\n")
	if err := e.installer(func(o *Options) { byo(o); o.Given = &Given{Project: "notes"} }).withSettings(); err == nil || !strings.Contains(err.Error(), "tls byo copies") {
		t.Errorf("byo in the settings: %v", err)
	}
}
