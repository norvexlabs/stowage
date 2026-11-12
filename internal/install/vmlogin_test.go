package install

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
)

// loginFile is a Docker config holding one auths entry for host.
func loginFile(host, user, password string) []byte {
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
	return []byte(`{"auths": {"` + host + `": {"auth": "` + auth + `"}}}`)
}

const loginPassword = "s3cret-pass"

func TestALoginFileIsOneAuthsEntry(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		raw  string
		want registryLogin
		err  string
	}{
		"auth":              {raw: string(loginFile("mirror.example:5000", "ops", loginPassword)), want: registryLogin{"mirror.example:5000", "ops", loginPassword}},
		"username password": {raw: `{"auths": {"Mirror.Example": {"username": "ops", "password": "` + loginPassword + `"}}}`, want: registryLogin{"mirror.example", "ops", loginPassword}},
		"url key":           {raw: string(loginFile("https://mirror.example/v1/", "ops", loginPassword)), want: registryLogin{"mirror.example", "ops", loginPassword}},
		"a colon in it":     {raw: string(loginFile("m.example", "ops", "a:b")), want: registryLogin{"m.example", "ops", "a:b"}},
		"two entries":       {raw: `{"auths": {"a": {"auth": "eDp5"}, "b": {"auth": "eDp5"}}}`, err: "2 entries in auths"},
		"none":              {raw: `{"auths": {}}`, err: "0 entries in auths"},
		"credential store":  {raw: `{"auths": {"a": {}}, "credsStore": "desktop"}`, err: `unknown field "credsStore"`},
		"identity token":    {raw: `{"auths": {"a": {"identitytoken": "` + loginPassword + `"}}}`, err: `unknown field "identitytoken"`},
		"no password":       {raw: `{"auths": {"a": {"username": "ops"}}}`, err: `auths "a" has no user or no password`},
		"both":              {raw: `{"auths": {"a": {"auth": "eDp5", "password": "` + loginPassword + `"}}}`, err: "has auth and username or password"},
		"not base64":        {raw: `{"auths": {"a": {"auth": "` + loginPassword + `!"}}}`, err: "auth is not base64"},
		"no colon":          {raw: `{"auths": {"a": {"auth": "` + base64.StdEncoding.EncodeToString([]byte(loginPassword)) + `"}}}`, err: "auth is not USER:PASSWORD"},
		"no host":           {raw: `{"auths": {"https://": {"auth": "eDp5"}}}`, err: "an entry with no host"},
		"not JSON":          {raw: loginPassword, err: "not JSON"},
		"two values":        {raw: string(loginFile("a", "x", "y")) + "{}", err: "more than one JSON value"},
	} {
		got, err := parseRegistryLogin([]byte(c.raw))
		switch {
		case c.err == "" && (err != nil || got != c.want):
			t.Errorf("%s: %+v %v", name, got, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: %v, want %q", name, err, c.err)
		case err != nil && strings.Contains(err.Error(), loginPassword):
			t.Errorf("%s: the error quotes the password: %v", name, err)
		}
	}
}

// vmMirrorEnv is a VM whose settings name the mirror mirror.example:5000/acme/notes.
func vmMirrorEnv(t *testing.T) (*env, *Installer) {
	t.Helper()
	e := vmEnv(t, "1.0.0")
	settings := filepath.Join(t.TempDir(), "install.yaml")
	if err := os.WriteFile(settings, []byte("registry: mirror.example:5000/acme/notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return e, e.installer(func(o *Options) { vm(o); o.Settings = settings })
}

func TestTheVMRecordsItsMirrorsLoginForItsHostAlone(t *testing.T) {
	t.Parallel()
	_, in := vmMirrorEnv(t)
	f := registryLoginFile(in.O.Dir)
	if _, err := in.recordRegistryLogin(loginFile("other.example", "ops", loginPassword)); err == nil || !strings.Contains(err.Error(), "the login is for other.example, but this VM reads its mirror mirror.example:5000/acme/notes at mirror.example:5000") {
		t.Errorf("another host: %v", err)
	}
	if fileExists(f) {
		t.Fatal("a refused login is recorded")
	}
	host, err := in.recordRegistryLogin(loginFile("mirror.example:5000", "ops", "first"))
	if err != nil || host != "mirror.example:5000" {
		t.Fatalf("%q %v", host, err)
	}
	if fi, err := os.Stat(f); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	// A re-run replaces it.
	if _, err := in.recordRegistryLogin(loginFile("mirror.example:5000", "ops", loginPassword)); err != nil {
		t.Fatal(err)
	}
	// The mirror's reads take it; the release's own registry and another host never do.
	for prefix, want := range map[string]string{
		"mirror.example:5000/acme/notes": loginPassword,
		"MIRROR.example:5000/x":          loginPassword,
		"other.example/acme/notes":       "",
		in.R.Spec.Registry.Prefix:        "",
	} {
		if _, token, err := in.pullAccount(prefix); err != nil || token != want {
			t.Errorf("%s: %q %v", prefix, token, err)
		}
	}
	// Off the VM nothing reads it.
	off := *in
	off.O.Preloaded = false
	if _, token, err := off.pullAccount("mirror.example:5000/acme/notes"); err != nil || token != "" {
		t.Errorf("off the VM: %q %v", token, err)
	}
	// The registry client of the mirror signs in with it.
	kc, err := in.keychain("mirror.example:5000/acme/notes")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := name.NewRegistry("mirror.example:5000")
	if err != nil {
		t.Fatal(err)
	}
	a, err := kc.Resolve(reg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err := a.Authorization(); err != nil || cfg.Username != "ops" || cfg.Password != loginPassword {
		t.Errorf("%+v %v", cfg, err)
	}
	// A file that does not read is said, never quoted, and fails the reads.
	if err := os.WriteFile(f, []byte(`{"auths": {"mirror.example:5000": {"auth": "`+loginPassword+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := in.pullAccount("mirror.example:5000/acme/notes"); err == nil || !strings.Contains(err.Error(), "registry-login FILE` replaces it") || strings.Contains(err.Error(), loginPassword) {
		t.Errorf("%v", err)
	}
}

func TestALoginIsRefusedOnAVMReadingTheReleasesRegistry(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	in := e.installer(func(o *Options) { vm(o); o.Settings = filepath.Join(t.TempDir(), "none.yaml") })
	if _, err := in.recordRegistryLogin(loginFile("ghcr.io", "ops", loginPassword)); err == nil || !strings.Contains(err.Error(), "a login is for a mirror") {
		t.Errorf("%v", err)
	}
	// An install's recorded registry decides once it exists.
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	if _, err := in.recordRegistryLogin(loginFile("ghcr.io", "ops", loginPassword)); err == nil || !strings.Contains(err.Error(), "a login is for a mirror") {
		t.Errorf("installed: %v", err)
	}
}

func TestTheRegistryLoginVerb(t *testing.T) {
	t.Parallel()
	e, in := vmMirrorEnv(t)
	run := func(euid int, stdin string, args ...string) (string, error) {
		var out bytes.Buffer
		o := in.O
		cmd := registryLoginCommand(&Installer{R: e.r}, &o, &out, func() int { return euid })
		cmd.SetArgs(args)
		cmd.SetIn(strings.NewReader(stdin))
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		err := cmd.Execute()
		return out.String(), err
	}
	raw := string(loginFile("mirror.example:5000", "ops", loginPassword))
	if _, err := run(1000, raw, "-"); err == nil || !strings.Contains(err.Error(), "sudo notes-stow registry-login") {
		t.Errorf("not root: %v", err)
	}
	if _, err := run(0, ""); err == nil || !strings.Contains(err.Error(), "records a login file") {
		t.Errorf("no file: %v", err)
	}
	out, err := run(0, raw, "-")
	if err != nil || !strings.Contains(out, "Recorded the login to mirror.example:5000") || strings.Contains(out, loginPassword) {
		t.Fatalf("%q %v", out, err)
	}
	// The status says a login is held, never its value.
	d := daemon(e, &fakeHost{})
	d.O = in.O
	var st Status
	call(t, d.Handler(), "GET", "/v1/status", nil, &st)
	raw2, _ := json.Marshal(st)
	if st.RegistryLogin != "mirror.example:5000" || strings.Contains(string(raw2), loginPassword) || strings.Contains(string(raw2), base64.StdEncoding.EncodeToString([]byte("ops:"+loginPassword))) {
		t.Errorf("status: %s", raw2)
	}
	if out, err := ctl(e, served(t, d), "status"); err != nil || !strings.Contains(out, "login      held for the mirror mirror.example:5000") || strings.Contains(out, loginPassword) {
		t.Errorf("%v\n%s", err, out)
	}
	if err := os.WriteFile(registryLoginFile(in.O.Dir), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	st = Status{}
	if call(t, d.Handler(), "GET", "/v1/status", nil, &st); st.RegistryLogin != "" || !strings.Contains(st.RegistryLoginError, "--remove` removes it") {
		t.Errorf("an unreadable login: %+v", st)
	}
	if _, err := run(0, "", "--remove", "x"); err == nil {
		t.Error("--remove with a file")
	}
	if out, err := run(0, "", "--remove"); err != nil || !strings.Contains(out, "Removed the login") || fileExists(registryLoginFile(in.O.Dir)) {
		t.Errorf("%q %v", out, err)
	}
	if out, err := run(0, "", "--remove"); err != nil || !strings.Contains(out, "nothing to remove") {
		t.Errorf("again: %q %v", out, err)
	}
	if out, err := run(0, raw[:10], "-"); err == nil || !strings.Contains(err.Error(), "standard input: not JSON") {
		t.Errorf("%q %v", out, err)
	}
}

// vm create gives the login over SSH once the VM answers, never in the cloud-config or a log; a
// login for another host, without a mirror or on another release's disk, is refused first.
func TestVMCreateGivesTheMirrorsLoginOverSSH(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	v.r.Spec.Registry.Prefix = "ghcr.io/acme/notes" // the test registry is the mirror
	host := registryHost(v.prefix)
	mirror := func(o *Options) { o.Registry = v.prefix }
	r := newTofuRun(t)
	c := tofuCreate()
	c.RegistryLogin = loginFile(host, "ops", loginPassword)
	if err := v.create(c, r.mod(mirror)); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	i := slices.IndexFunc(r.ssh.calls, func(call []string) bool {
		return strings.HasSuffix(strings.Join(call, " "), "-- sudo notes-stow registry-login -")
	})
	if i < 0 || !strings.Contains(r.ssh.stdin[i], `"`+host+`"`) {
		t.Fatalf("ssh %v", r.ssh.calls)
	}
	if l, err := parseRegistryLogin([]byte(r.ssh.stdin[i])); err != nil || l.Password != loginPassword {
		t.Errorf("given %+v %v", l, err)
	}
	if !slices.Contains(r.ssh.calls[i], "StrictHostKeyChecking=yes") {
		t.Errorf("not pinned: %v", r.ssh.calls[i])
	}
	// No install over SSH follows: the login's call is the last on the seed's key, retired then.
	if n := len(r.ssh.calls); n != i+2 || !strings.Contains(strings.Join(r.ssh.calls[n-1], " "), "ssh_host_ed25519_key.pub") {
		t.Errorf("the seed's key is not retired after the login: %v", r.ssh.calls)
	}
	dir := filepath.Join(r.state, "notes", "vms", "notes-1")
	seed, _, _ := readSeed(t, filepath.Join(dir, "seed.iso"))
	b64 := base64.StdEncoding.EncodeToString([]byte("ops:" + loginPassword))
	for what, s := range map[string]string{"the cloud-config": seed, "the log": v.log.String(), "the output": v.out2()} {
		if strings.Contains(s, loginPassword) || strings.Contains(s, b64) {
			t.Errorf("%s holds the login:\n%s", what, s)
		}
	}
	// By hand, the step is printed; nothing is given.
	v2 := newVMCreate(t, "qcow2")
	v2.r.Spec.Registry.Prefix = "ghcr.io/acme/notes"
	if err := v2.create(VMCreate{Platform: "truenas", Hostname: "notes-1", User: "ops", RegistryLogin: loginFile(host, "ops", loginPassword)}, func(o *Options) { o.TLS = "internal"; mirror(o) }); err != nil {
		t.Fatalf("%v\n%s", err, v2.log)
	}
	if out := v2.out2(); !strings.Contains(out, "ssh ops@<address> sudo notes-stow registry-login - < FILE") || strings.Contains(out, loginPassword) {
		t.Errorf("out:\n%s", out)
	}
	for name, cc := range map[string]struct {
		mod   func(*Options)
		login []byte
		err   string
	}{
		"another host": {mirror, loginFile("other.example", "ops", loginPassword), "the login is for other.example"},
		"no mirror":    {nil, loginFile(host, "ops", loginPassword), "--registry-login-file is a mirror's login"},
		"bad file":     {mirror, []byte("{}"), "0 entries in auths"},
	} {
		v := newVMCreate(t, "qcow2")
		v.r.Spec.Registry.Prefix = "ghcr.io/acme/notes"
		if cc.mod == nil {
			v.r.Spec.Registry.Prefix = v.prefix
		}
		r := newTofuRun(t)
		c := tofuCreate()
		c.RegistryLogin = cc.login
		if err := v.create(c, r.mod(cc.mod)); err == nil || !strings.Contains(err.Error(), cc.err) || len(r.tofu.calls) > 0 {
			t.Errorf("%s: %v (tofu %d calls)", name, err, len(r.tofu.calls))
		}
	}
}

// vm seed checks the login and names its host alone; a VM booting another release's disks is
// refused a login, as its first boot reads that release's OS from the mirror before the login.
func TestVMSeedNamesTheMirrorsLoginByItsHost(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	v.r.Spec.Registry.Prefix = "ghcr.io/acme/notes"
	host := registryHost(v.prefix)
	mirror := func(o *Options) { o.Registry = v.prefix }
	s, err := seedOf(t, v, mirror, SeedOptions{RegistryLogin: loginFile(host, "ops", loginPassword)})
	if err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	raw, _ := json.Marshal(s)
	if s.RegistryLogin != host || strings.Contains(string(raw), loginPassword) || strings.Contains(string(raw), base64.StdEncoding.EncodeToString([]byte("ops:"+loginPassword))) {
		t.Errorf("seed: %s", raw)
	}
	if s, err := seedOf(t, v, mirror, SeedOptions{}); err != nil || s.RegistryLogin != "" {
		t.Errorf("no login: %+v %v", s, err)
	}
	m := v.r.Manifest
	m.Artifacts.BootDisks = &release.BootDisks{Release: "1.0.0-r0", Stowage: "0.34.0", Disks: m.Artifacts.Disks}
	m.Artifacts.Disks = nil
	if _, err := seedOf(t, v, mirror, SeedOptions{RegistryLogin: loginFile(host, "ops", loginPassword)}); err == nil || !strings.Contains(err.Error(), "its first boot reads 1.0.0-1's OS bundle from the mirror before the login reaches it") {
		t.Errorf("another release's disks: %v", err)
	}
	if _, err := seedOf(t, v, mirror, SeedOptions{}); err != nil {
		t.Errorf("another release's disks without a login: %v", err)
	}
}

// An upgrade reads the mirror's OS bundle with the login the VM holds for it.
func TestAnUpgradeReadsTheMirrorWithTheLoginHeld(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.release("2.0.0", 1, nil)
	host := &fakeHost{}
	d := daemon(p.env, host)
	d.R.Spec.Registry.Prefix = "ghcr.io/acme/notes" // the registry the install reads is a mirror
	raw := loginFile(registryHost(p.prefix), "ops", loginPassword)
	if err := os.WriteFile(registryLoginFile(p.env.dir), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	hh := d.Handler()
	no := false
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", Reboot: &no}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	if j := waitJob(t, hh); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if want := base64.StdEncoding.EncodeToString([]byte("ops:" + loginPassword)); !strings.Contains(host.config, want) {
		t.Errorf("kairos-agent pulled with %q", host.config)
	}
}

// With secrets still due over SSH the seed's key stays for them after the login; a login that
// cannot be given names what the install still waits for.
func TestVMCreateKeepsTheSeedKeyForTheSecretsAfterTheLogin(t *testing.T) {
	t.Parallel()
	for name, fail := range map[string]bool{"given": false, "refused": true} {
		v := newVMCreate(t, "qcow2")
		v.r.Spec.Registry.Prefix = "ghcr.io/acme/notes"
		v.r.Spec.Inputs = append(v.r.Spec.Inputs, spec.Input{Key: "API_KEY", Prompt: "API key", Secret: true, Required: true})
		r := newTofuRun(t)
		last := statusJSON(false)
		if fail {
			last = func() ([]byte, error) { return nil, errors.New("connection reset") }
		}
		r.ssh.answers = []func() ([]byte, error){refused, statusIn("recovery", false), statusJSON(false), last}
		c := tofuCreate()
		c.Secrets = []byte("project: notes\nsecrets:\n  SESSION_SECRET: kept\n")
		c.RegistryLogin = loginFile(registryHost(v.prefix), "ops", loginPassword)
		err := v.create(c, r.mod(func(o *Options) { o.Registry = v.prefix }))
		switch {
		case fail && (err == nil || !strings.Contains(err.Error(), "then its install waits for its secrets and API_KEY: chmod 600 FILE")):
			t.Errorf("%s: %v", name, err)
		case !fail && err != nil:
			t.Fatalf("%s: %v\n%s", name, err, v.log)
		}
		for _, call := range r.ssh.calls {
			if strings.Contains(strings.Join(call, " "), "ssh_host_ed25519_key.pub") {
				t.Errorf("%s: the seed's key retired before the secrets: %v", name, call)
			}
		}
	}
}

// A login that cannot be given names the secret inputs the install still waits for.
func TestALoginNotGivenNamesTheInputsTheInstallWaitsFor(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	v.r.Spec.Registry.Prefix = "ghcr.io/acme/notes"
	v.r.Spec.Inputs = append(v.r.Spec.Inputs, spec.Input{Key: "API_KEY", Prompt: "API key", Secret: true, Required: true})
	r := newTofuRun(t)
	r.ssh.answers = []func() ([]byte, error){refused, statusIn("recovery", false), statusJSON(false), func() ([]byte, error) { return nil, errors.New("connection reset") }}
	c := tofuCreate()
	c.RegistryLogin = loginFile(registryHost(v.prefix), "ops", loginPassword)
	if err := v.create(c, r.mod(func(o *Options) { o.Registry = v.prefix })); err == nil || !strings.Contains(err.Error(), "then its install waits for API_KEY: ssh -t ops@notes-1.lan sudo notes-stow install") {
		t.Errorf("%v", err)
	}
}

// A mirror that is the release's own registry takes no login: the VM would refuse it.
func TestVMCreateRefusesALoginForTheReleasesOwnRegistry(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	r := newTofuRun(t)
	c := tofuCreate()
	c.RegistryLogin = loginFile(registryHost(v.prefix), "ops", loginPassword)
	if err := v.create(c, r.mod(func(o *Options) { o.Registry = v.prefix })); err == nil || !strings.Contains(err.Error(), "the release's own registry") || len(r.tofu.calls) > 0 {
		t.Errorf("%v", err)
	}
}
