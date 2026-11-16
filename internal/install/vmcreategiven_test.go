package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	"github.com/norvexlabs/stowage/internal/testutil"
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

// Every platform names where its VM's boot is watched: a deadline's message points there.
func TestEveryPlatformNamesItsConsole(t *testing.T) {
	t.Parallel()
	for _, p := range Platforms {
		if p.Console == "" {
			t.Errorf("%s names no console", p.Key)
		}
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
		{"--secrets beside a certificate", nil, VMCreate{Secrets: []byte("project: notes\nsecrets: {}\n")}, func(o *Options) { o.Cert, o.Key = goodCert, goodKey }, "--secrets and the certificate and its key: the first install waits for one of them over SSH"},
		{"--secrets beside a secret input", secret(false), VMCreate{Secrets: []byte("project: notes\nsecrets: {}\n")}, func(o *Options) { o.TLS = "internal"; o.Answers = map[string]string{"API_KEY": "v"} }, "--secrets and API_KEY: the first install waits for one of them over SSH"},
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
	// --secrets beside --tls byo: the fix leaves the mode out, not its certificate alone.
	vb := newVMCreate(t, "qcow2")
	err := vb.create(VMCreate{Platform: "proxmox", Secrets: []byte("project: notes\nsecrets: {}\n")}, func(o *Options) { o.TLS, o.Cert, o.Key = "byo", goodCert, goodKey })
	if r, ok := Refusal(err); !ok || r.Fix != "leave out --tls byo (choose another --tls mode), or leave out --secrets" {
		t.Errorf("--secrets and --tls byo: %v (fix %q)", err, r.Fix)
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
