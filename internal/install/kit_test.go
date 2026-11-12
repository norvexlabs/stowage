package install

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/kit"
	"github.com/norvexlabs/stowage/internal/spec"
)

const testKitCode = "the-code"

// takeGiven and takeK8sGiven are what a --given of raw gives an install of this shape, and on Kubernetes.
func (in *Installer) takeGiven(raw []byte) error {
	g, err := parseGiven(raw, in.R.Spec.Name)
	if err != nil {
		return err
	}
	return in.useGiven(g, "--given", in.shape())
}

func (in *Installer) takeK8sGiven(raw []byte) error {
	g, err := parseGiven(raw, in.R.Spec.Name)
	if err != nil {
		return err
	}
	return in.useK8sGiven(g, "--given")
}

var kitNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// sealKit is a kit for notes with those inputs, valid at kitNow, sealed with testKitCode.
func sealKit(t *testing.T, mod func(*kit.Kit)) []byte {
	t.Helper()
	k := &kit.Kit{For: "notes", Name: "search", Instance: "site-1", Issued: kitNow, Expires: kitNow.Add(time.Hour), Inputs: map[string]string{"API_KEY": "from-the-kit"}}
	if mod != nil {
		mod(k)
	}
	raw, err := kit.Seal(k, testKitCode, 4)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeTestFile(t *testing.T, name string, raw []byte) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func kitInstaller(env map[string]string) (*Installer, *bytes.Buffer) {
	var log bytes.Buffer
	in := &Installer{R: &Release{Spec: &spec.Spec{Name: "notes"}}}
	in.O.Log, in.O.Cos = &log, "/nonexistent"
	in.O.Getenv = func(k string) string { return env[k] }
	return in, &log
}

func takeInto(t *testing.T, in *Installer, f *givenFlags, stdin io.Reader, prompt Prompter) (*Given, string, error) {
	t.Helper()
	var got *Given
	var flag string
	err := f.take(context.Background(), in, stdin, prompt, kitNow, func(g *Given, name string) error {
		got, flag = g, name
		return nil
	})
	return got, flag, err
}

// The code comes from --kit-code-file, else the variable, else the terminal; each wins over the next.
func TestAKitsCodeSources(t *testing.T) {
	t.Parallel()
	file := writeTestFile(t, "kit", sealKit(t, nil))
	codeFile := writeTestFile(t, "code", []byte(testKitCode+"\n"))
	wrong := writeTestFile(t, "wrong", []byte("wrong\n"))
	asks := func() *fakePrompter { return &fakePrompter{answers: map[string]string{"--kit-code": testKitCode}} }
	for _, c := range []struct {
		name     string
		codeFile string
		env      string
		ask      bool
		answers  string
		asked    int
		want     string
	}{
		{name: "the file", codeFile: codeFile, env: "wrong", ask: true},
		{name: "the variable", env: testKitCode + "\n", ask: true},
		{name: "the terminal", ask: true, asked: 1},
		{name: "a blank variable asks", env: "  ", ask: true, asked: 1},
		{name: "nothing, no terminal", want: "nothing gives its code"},
		{name: "a wrong file", codeFile: wrong, ask: true, want: "does not open it"},
		{name: "a missing file", codeFile: filepath.Join(t.TempDir(), "gone"), want: "--kit-code-file: "},
		{name: "an empty file", codeFile: writeTestFile(t, "empty", []byte(" \n")), want: "is empty"},
		{name: "a long file", codeFile: writeTestFile(t, "long", bytes.Repeat([]byte("x"), maxKitCodeFile+1)), want: "more than"},
	} {
		in, _ := kitInstaller(map[string]string{KitCodeVar: c.env})
		var prompt Prompter
		p := asks()
		if c.ask {
			prompt = p
		}
		g, flag, err := takeInto(t, in, &givenFlags{kit: file, codeFile: c.codeFile}, nil, prompt)
		if c.want != "" {
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: %v, want %q", c.name, err, c.want)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if flag != "--kit" || g.Inputs["API_KEY"] != "from-the-kit" || g.Project != "notes" {
			t.Errorf("%s: %s %+v", c.name, flag, g)
		}
		if len(p.forms) != c.asked {
			t.Errorf("%s: asked %d times", c.name, len(p.forms))
		}
		if c.asked > 0 && !p.forms[0].Questions[0].Secret {
			t.Errorf("%s: the code is asked in the clear", c.name)
		}
	}
	// The terminal's refusal is the run's.
	in, _ := kitInstaller(nil)
	if _, _, err := takeInto(t, in, &givenFlags{kit: file}, nil, &fakePrompter{err: ErrAborted}); !errors.Is(err, ErrAborted) {
		t.Errorf("aborted: %v", err)
	}
}

func TestAKitsRefusals(t *testing.T) {
	t.Parallel()
	env := map[string]string{KitCodeVar: testKitCode}
	open := func(raw []byte, f givenFlags) error {
		in, _ := kitInstaller(env)
		f.kit = "-"
		_, _, err := takeInto(t, in, &f, bytes.NewReader(raw), nil)
		return err
	}
	for name, c := range map[string]struct {
		raw  []byte
		want string
	}{
		"another project": {sealKit(t, func(k *kit.Kit) { k.For = "crm" }), `this kit is for "crm", not notes`},
		"at its expiry":   {sealKit(t, func(k *kit.Kit) { k.Expires = kitNow }), "expired at"},
		"nothing in it":   {sealKit(t, func(k *kit.Kit) { k.Inputs = nil }), "--kit: the file holds neither"},
		"half a cert":     {sealKit(t, func(k *kit.Kit) { k.TLS = &kit.TLS{Cert: "c"} }), "--kit: tls: cert and key"},
		"clear text":      {[]byte("project: notes\n"), "--kit: not a kit"},
		"too large":       {bytes.Repeat([]byte("x"), kit.MaxSize+2), "--kit: more than"},
	} {
		if err := open(c.raw, givenFlags{}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	// Just before its expiry, it opens.
	if err := open(sealKit(t, func(k *kit.Kit) { k.Expires = kitNow.Add(time.Nanosecond) }), givenFlags{}); err != nil {
		t.Errorf("before its expiry: %v", err)
	}
	in, _ := kitInstaller(env)
	if _, _, err := takeInto(t, in, &givenFlags{kit: filepath.Join(t.TempDir(), "gone")}, nil, nil); err == nil || !strings.Contains(err.Error(), "--kit: ") {
		t.Errorf("a missing file: %v", err)
	}
	if _, _, err := takeInto(t, in, &givenFlags{given: "-", kit: "-"}, nil, nil); err == nil || !strings.Contains(err.Error(), "give one") {
		t.Errorf("both: %v", err)
	}
	if _, _, err := takeInto(t, in, &givenFlags{given: "-", codeFile: "c"}, nil, nil); err == nil || !strings.Contains(err.Error(), "give one") {
		t.Errorf("--given with a code: %v", err)
	}
	if _, _, err := takeInto(t, in, &givenFlags{codeFile: "c"}, nil, nil); err == nil || !strings.Contains(err.Error(), "none is") {
		t.Errorf("a code alone: %v", err)
	}
	if g, _, err := takeInto(t, in, &givenFlags{}, nil, nil); g != nil || err != nil {
		t.Errorf("no flag: %v %v", g, err)
	}
}

// What it logs names the kit, and neither its code nor a value.
func TestAKitIsLoggedByItsNameAlone(t *testing.T) {
	t.Parallel()
	in, log := kitInstaller(map[string]string{KitCodeVar: testKitCode})
	if _, _, err := takeInto(t, in, &givenFlags{kit: "-"}, bytes.NewReader(sealKit(t, nil)), nil); err != nil {
		t.Fatal(err)
	}
	s := log.String()
	if !strings.Contains(s, `--kit: "search", instance "site-1", until 2026-11-06T13:00:00Z`) || strings.Contains(s, testKitCode) || strings.Contains(s, "from-the-kit") {
		t.Error(s)
	}
}

// An input the project declares for other shapes alone is left out and said, here and for `vm create`;
// what the project does not declare stays, for the inputs' check to refuse.
func TestAGivenLeavesOutAnotherShapesInputs(t *testing.T) {
	t.Parallel()
	in, log := kitInstaller(nil)
	in.R.Spec.Inputs = []spec.Input{
		{Key: "DOCKER_ONLY", Shapes: []string{"docker"}},
		{Key: "VM_ONLY", Shapes: []string{"kairos"}},
		{Key: "KUBE_ONLY", Shapes: []string{"kubernetes"}},
		{Key: "EVERYWHERE"},
	}
	whole := func() *Given {
		return &Given{Project: "notes", Inputs: map[string]string{"DOCKER_ONLY": "d", "VM_ONLY": "v", "KUBE_ONLY": "k", "EVERYWHERE": "e", "UNKNOWN": "u"}}
	}
	if err := in.useGiven(whole(), "--kit", "docker"); err != nil {
		t.Fatal(err)
	}
	if a := in.O.Answers; len(a) != 3 || a["DOCKER_ONLY"] != "d" || a["EVERYWHERE"] != "e" || a["UNKNOWN"] != "u" {
		t.Errorf("docker: %v", a)
	}
	if !strings.Contains(log.String(), "--kit: KUBE_ONLY, VM_ONLY left out: not an input of the docker shape") {
		t.Error(log)
	}
	if len(in.O.Given.Inputs) != 3 {
		t.Errorf("the Given keeps what it left out: %v", in.O.Given.Inputs)
	}
	in.O.Answers = nil
	if err := in.useGiven(whole(), "--given", "kairos"); err != nil {
		t.Fatal(err)
	}
	if a := in.O.Answers; len(a) != 3 || a["VM_ONLY"] != "v" {
		t.Errorf("kairos: %v", a)
	}
	if !strings.Contains(log.String(), "--given: DOCKER_ONLY, KUBE_ONLY left out: not an input of the kairos shape") {
		t.Error(log)
	}
}

// A kit's material reaches a Docker install, first and on a re-run: a newer kit replaces the recorded
// value, logged; a flag wins over it.
func TestAKitOnDocker(t *testing.T) {
	t.Parallel()
	cert, key := writeCert(t, time.Now().AddDate(1, 0, 0), "notes.example.com", "api.example.com")
	certPEM, _ := os.ReadFile(cert)
	keyPEM, _ := os.ReadFile(key)
	e := newEnv(t)
	e.r.Spec.Inputs = append(e.r.Spec.Inputs,
		spec.Input{Key: "API_KEY", Prompt: "API key", Secret: true, Required: true},
		spec.Input{Key: "KUBE_ONLY", Prompt: "k", Shapes: []string{"kubernetes"}})
	run := func(k []byte, mod func(*Options)) error {
		in := e.installer(func(o *Options) {
			tlsDomains(o)
			environment := o.Getenv
			o.Getenv = func(n string) string {
				if n == KitCodeVar {
					return testKitCode
				}
				return environment(n)
			}
			if mod != nil {
				mod(o)
			}
		})
		f := givenFlags{kit: "-"}
		if err := f.take(context.Background(), in, bytes.NewReader(k), nil, kitNow, func(g *Given, flag string) error { return in.useGiven(g, flag, in.shape()) }); err != nil {
			t.Fatal(err)
		}
		return in.Install(context.Background())
	}
	first := sealKit(t, func(k *kit.Kit) {
		k.Inputs["KUBE_ONLY"] = "elsewhere"
		k.TLS = &kit.TLS{Cert: string(certPEM), Key: string(keyPEM)}
	})
	if err := run(first, nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	got, err := os.ReadFile(filepath.Join(e.dir, TLSDir, "key.pem"))
	if err != nil || !bytes.Equal(got, keyPEM) {
		t.Errorf("the key: %v", err)
	}
	value := func(k string) string {
		t.Helper()
		env, err := ReadEnvFile(filepath.Join(e.dir, envName))
		if err != nil {
			t.Fatal(err)
		}
		v, _ := env.Get(k)
		return v
	}
	if value("API_KEY") != "from-the-kit" || value("KUBE_ONLY") != "" {
		t.Errorf("API_KEY %q KUBE_ONLY %q", value("API_KEY"), value("KUBE_ONLY"))
	}
	if !strings.Contains(e.log.String(), "--kit: KUBE_ONLY left out") {
		t.Errorf("log %s", e.log)
	}
	newer := sealKit(t, func(k *kit.Kit) { k.Inputs["API_KEY"] = "newer" })
	if err := run(newer, nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if value("API_KEY") != "newer" {
		t.Errorf("a newer kit: %q", value("API_KEY"))
	}
	if !strings.Contains(e.log.String(), "changing API_KEY: <redacted> → <redacted>") || strings.Contains(e.log.String(), "from-the-kit") || strings.Contains(e.log.String(), "newer\n") {
		t.Errorf("the change is not logged, or a value is:\n%s", e.log)
	}
	if err := run(sealKit(t, nil), func(o *Options) { o.Set = map[string]string{"API_KEY": "the-flag"} }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if value("API_KEY") != "the-flag" {
		t.Errorf("a flag over the kit: %q", value("API_KEY"))
	}
}

// `vm create --kit` sends what the kit holds as any Given goes: never in the seed, and by hand through
// the file its step takes.
func TestVMCreateTakesAKit(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	mark, keyPEM, mod := byoVM(t, v)
	v.r.Spec.Inputs = append(v.r.Spec.Inputs, spec.Input{Key: "KUBE_ONLY", Prompt: "k", Shapes: []string{"kubernetes"}})
	err := v.create(VMCreate{Platform: "proxmox"}, func(o *Options) {
		mod(o)
		certPEM, err := os.ReadFile(o.Cert)
		if err != nil {
			t.Fatal(err)
		}
		k := sealKit(t, func(k *kit.Kit) {
			k.Inputs = map[string]string{"API_KEY": mark, "KUBE_ONLY": "elsewhere"}
			k.TLS = &kit.TLS{Cert: string(certPEM), Key: string(keyPEM)}
		})
		o.Cert, o.Key, o.TLS, o.Answers = "", "", "", map[string]string{"ADMIN_EMAIL": "answered@example.org"}
		o.Getenv = func(n string) string {
			if n == KitCodeVar {
				return testKitCode
			}
			return ""
		}
		in := &Installer{R: v.r}
		in.O = *o
		f := givenFlags{kit: "-"}
		if err := f.take(context.Background(), in, bytes.NewReader(k), nil, kitNow, func(g *Given, flag string) error { return in.useGiven(g, flag, "kairos") }); err != nil {
			t.Fatal(err)
		}
		*o = in.O
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	ud, _, _ := readSeed(t, filepath.Join(v.out, "notes-seed.iso"))
	if strings.Contains(ud, mark) || strings.Contains(ud, "elsewhere") || strings.Contains(ud, strings.Split(string(keyPEM), "\n")[1]) {
		t.Errorf("the seed holds the kit's material:\n%s", ud)
	}
	raw, err := os.ReadFile(filepath.Join(v.out, "notes.given"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := parseGiven(raw, "notes")
	if err != nil {
		t.Fatal(err)
	}
	if g.Inputs["API_KEY"] != mark || g.TLS.Key != string(keyPEM) || g.Inputs["KUBE_ONLY"] != "" {
		t.Errorf("%+v", g)
	}
	if !strings.Contains(v.log.String(), "--kit: KUBE_ONLY left out") {
		t.Errorf("log %s", v.log)
	}
}

// On Kubernetes a kit goes as --given does: it replaces a recorded value on an upgrade and restarts
// what reads it, and its tls is left out and said under its own flag.
func TestK8sUpgradeKit(t *testing.T) {
	t.Parallel()
	e := upgradeEnv(t)
	file := writeTestFile(t, "kit", func() []byte {
		k := &kit.Kit{For: "notes", Expires: time.Now().Add(time.Hour), Inputs: map[string]string{"TOKEN": "from-the-kit"}, TLS: &kit.TLS{ClientCA: "c"}}
		raw, err := kit.Seal(k, testKitCode, 4)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}())
	code := writeTestFile(t, "code", []byte(testKitCode))
	var log bytes.Buffer
	cmd := newCommand(e.release, "notes-stow", io.Discard, &log, K8sOptions{Helm: kubeTool{e.f, "helm"}, Kubectl: kubeTool{e.f, "kubectl"}}, func(bool) Prompter { return nil })
	cmd.SetArgs([]string{"k8s", "upgrade", "--kit", file, "--kit-code-file", code, "--no-input", "--no-provision"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if env := e.secretData("notes-stowage"); env["TOKEN"] != "from-the-kit" {
		t.Errorf("env %v", env)
	}
	if want := "deployment/notes-token"; len(restarted(e)) != 1 || restarted(e)[0] != want {
		t.Errorf("restarted %q", restarted(e))
	}
	if !strings.Contains(log.String(), "--kit: tls left out") {
		t.Errorf("log %s", log.String())
	}

	// An input it does not declare is refused under the flag; so is --version with a kit.
	e = upgradeEnv(t)
	in := e.installer(nil)
	if err := in.useK8sGiven(&Given{Project: "notes", Inputs: map[string]string{"NOPE": "x"}}, "--kit"); err == nil || !strings.Contains(err.Error(), "--kit: notes 1.0.0 declares no input NOPE") {
		t.Errorf("%v", err)
	}
	cmd = newCommand(e.release, "notes-stow", io.Discard, io.Discard, K8sOptions{Helm: kubeTool{e.f, "helm"}, Kubectl: kubeTool{e.f, "kubectl"}}, func(bool) Prompter { return nil })
	cmd.SetArgs([]string{"k8s", "upgrade", "--version", "2.0.0", "--kit", file, "--no-input"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--kit with --version") || !strings.Contains(err.Error(), "takes `k8s upgrade --kit FILE`") {
		t.Errorf("%v", err)
	}
	cmd = newCommand(e.release, "notes-stow", io.Discard, io.Discard, K8sOptions{Helm: kubeTool{e.f, "helm"}, Kubectl: kubeTool{e.f, "kubectl"}}, func(bool) Prompter { return nil })
	cmd.SetArgs([]string{"k8s", "upgrade", "--version", "2.0.0", "--kit-code-file", code, "--no-input"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--kit with --version") {
		t.Errorf("a code alone: %v", err)
	}
	if c, _, err := newCommand(e.release, "notes-stow", io.Discard, io.Discard, K8sOptions{}, nil).Find([]string{"k8s", "install"}); err != nil || c.Flags().Lookup("kit") == nil || c.Flags().Lookup("kit-code-file") == nil {
		t.Errorf("k8s install: %v", err)
	}
}

// Each shape's upgrade refuses them, naming the re-run; each command that takes a kit has its flags.
func TestUpgradeRefusesAKit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, flag := range []string{"--kit", "--given", "--kit-code-file"} {
		cmd := newCommand(e.r, e.self, io.Discard, io.Discard, K8sOptions{}, func(bool) Prompter { return nil })
		cmd.SetArgs([]string{"upgrade", flag, "x", "--dir", e.dir})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), flag+": an upgrade") || !strings.Contains(err.Error(), "notes-stow --dir <the install> --kit FILE") {
			t.Errorf("docker %s: %v", flag, err)
		}
	}
	v := vmEnv(t, "1.0.0")
	cmd := vmCommand(v.r, v.self, io.Discard, io.Discard, func(bool) Prompter { return nil }, clientTimes{unit: time.Millisecond})
	cmd.SetArgs([]string{"upgrade", "--kit", "x"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--kit: an upgrade") || !strings.Contains(err.Error(), "sudo notes-stow install --kit FILE") {
		t.Errorf("vm: %v", err)
	}
	root := newCommand(e.r, e.self, io.Discard, io.Discard, K8sOptions{}, func(bool) Prompter { return nil })
	if root.Flags().Lookup("kit") == nil || root.Flags().Lookup("given") == nil || root.Flags().Lookup("kit-code-file") == nil {
		t.Error("the Docker install's flags")
	}
	create, _, err := root.Find([]string{"vm", "create"})
	if err != nil || create.Flags().Lookup("kit") == nil || create.Flags().Lookup("kit-code-file") == nil {
		t.Errorf("vm create: %v", err)
	}
}

// --secrets takes standard input or restores an install, which a kit does not join.
func TestSecretsAndAKitAreOne(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cmd := newCommand(e.r, e.self, io.Discard, io.Discard, K8sOptions{}, func(bool) Prompter { return nil })
	cmd.SetArgs([]string{"--dir", e.dir, "--no-input", "--secrets", "s", "--kit", "k"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--kit gives one") {
		t.Errorf("docker: %v", err)
	}
	cmd = newCommand(e.r, e.self, io.Discard, io.Discard, K8sOptions{}, func(bool) Prompter { return nil })
	cmd.SetArgs([]string{"--dir", e.dir, "--no-input", "--secrets", "s", "--given", "k"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--given gives one") {
		t.Errorf("docker: %v", err)
	}
	v := vmEnv(t, "1.0.0")
	vc := vmCommand(v.r, v.self, io.Discard, io.Discard, func(bool) Prompter { return nil }, clientTimes{unit: time.Millisecond})
	vc.SetArgs([]string{"install", "--no-input", "--dir", filepath.Join(t.TempDir(), "install"), "--secrets", "s", "--kit", "k"})
	if err := vc.Execute(); err == nil || !strings.Contains(err.Error(), "--kit gives a new one") {
		t.Errorf("vm: %v", err)
	}
	cmd = newCommand(e.r, e.self, io.Discard, io.Discard, K8sOptions{}, func(bool) Prompter { return nil })
	cmd.SetArgs([]string{"vm", "create", "--no-input", "--secrets", "-", "--kit", "-"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "read once") {
		t.Errorf("vm create: %v", err)
	}
}

// The command opens the kit, for the shape the VM's install takes, before it creates anything.
func TestVMCreateCommandOpensTheKit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.r.Spec.Inputs = append(e.r.Spec.Inputs, spec.Input{Key: "KUBE_ONLY", Prompt: "k", Shapes: []string{"kubernetes"}}, spec.Input{Key: "DOCKER_ONLY", Prompt: "d", Shapes: []string{"docker"}})
	raw, err := kit.Seal(&kit.Kit{For: "notes", Expires: time.Now().Add(time.Hour), Inputs: map[string]string{"KUBE_ONLY": "k", "DOCKER_ONLY": "d", "ADMIN_EMAIL": "a@example.org"}}, testKitCode, 4)
	if err != nil {
		t.Fatal(err)
	}
	file := writeTestFile(t, "kit", raw)
	code := writeTestFile(t, "code", []byte(testKitCode))
	run := func(codeFile string) (string, error) {
		var log bytes.Buffer
		cmd := newCommand(e.r, e.self, io.Discard, &log, K8sOptions{}, func(bool) Prompter { return nil })
		cmd.SetArgs([]string{"vm", "create", "--no-input", "--kit", file, "--kit-code-file", codeFile, "--platform", "no-such-platform"})
		err := cmd.Execute()
		return log.String(), err
	}
	log, err := run(code)
	if err == nil || strings.Contains(err.Error(), "--kit") {
		t.Errorf("past the kit, the platform is refused: %v", err)
	}
	if !strings.Contains(log, "--kit: DOCKER_ONLY, KUBE_ONLY left out: not an input of the kairos shape") {
		t.Errorf("log %q", log)
	}
	if _, err := run(writeTestFile(t, "wrong", []byte("wrong"))); err == nil || !strings.Contains(err.Error(), "--kit: this code does not open it") {
		t.Errorf("a wrong code: %v", err)
	}
}

// What another shape's inputs leave empty is refused: it would meet a VM's wait for what vm create gives.
func TestAGivenLeftWithNothingIsRefused(t *testing.T) {
	t.Parallel()
	in, log := kitInstaller(nil)
	in.R.Spec.Inputs = []spec.Input{{Key: "KUBE_ONLY", Shapes: []string{"kubernetes"}}}
	err := in.useGiven(&Given{Project: "notes", Inputs: map[string]string{"KUBE_ONLY": "k"}}, "--kit", "kairos")
	if err == nil || !strings.Contains(err.Error(), "--kit: nothing in the file is for this install") || in.O.Given != nil {
		t.Errorf("%v, Given %+v", err, in.O.Given)
	}
	if !strings.Contains(log.String(), "--kit: KUBE_ONLY left out") {
		t.Error(log)
	}
}

// What this install's mode does not ask is refused, given or not, naming the mode that asks it; the
// mode that asks it takes it.
func TestAGivenInputOfAnotherModeIsRefused(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		when, mode string
		refused    bool
	}{
		{"tls", Loopback, true},
		{"tls", "acme", false},
		{"loopback", "acme", true},
		{"loopback", Loopback, false},
	} {
		in, _ := kitInstaller(nil)
		in.R.Spec.Inputs = []spec.Input{{Key: "ONE_MODE", When: c.when}}
		if err := in.useGiven(&Given{Project: "notes", Inputs: map[string]string{"ONE_MODE": "x"}}, "--kit", "docker"); err != nil {
			t.Fatal(err)
		}
		err := in.knownInputs(c.mode)
		if (err != nil) != c.refused {
			t.Errorf("when %s in %s: %v", c.when, c.mode, err)
		}
		if c.refused && (!strings.Contains(err.Error(), "no input of this install is named ONE_MODE") || !strings.Contains(err.Error(), "ONE_MODE is asked in a "+c.when+" mode alone, and this is "+c.mode)) {
			t.Errorf("when %s in %s: %v", c.when, c.mode, err)
		}
	}
}

// On Kubernetes too, what is left of a kit once the shape's rules left the rest out must hold something.
func TestK8sAGivenLeftWithNothingIsRefused(t *testing.T) {
	t.Parallel()
	in, log := kitInstaller(nil)
	in.R.Spec.Inputs = []spec.Input{{Key: "DOCKER_ONLY", Shapes: []string{"docker"}}}
	for name, g := range map[string]*Given{
		"another shape's input": {Project: "notes", Inputs: map[string]string{"DOCKER_ONLY": "d"}},
		"tls alone":             {Project: "notes", TLS: &GivenTLS{ClientCA: "c"}},
	} {
		if err := in.useK8sGiven(g, "--kit"); err == nil || !strings.Contains(err.Error(), "--kit: nothing in the file is for this install") || in.O.Given != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !strings.Contains(log.String(), "--kit: DOCKER_ONLY left out") || !strings.Contains(log.String(), "--kit: tls left out") {
		t.Error(log)
	}
}

// An empty value is refused for each flag, on each command, so an unset variable gives no silent run.
func TestAnEmptyGivenFlagIsRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, args := range [][]string{
		{"--kit", ""}, {"--given", ""}, {"--kit-code-file", ""},
		{"k8s", "upgrade", "--kit", ""}, {"k8s", "install", "--given", ""},
		{"vm", "create", "--kit", ""},
	} {
		cmd := newCommand(e.r, e.self, io.Discard, io.Discard, K8sOptions{}, func(bool) Prompter { return nil })
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "empty: it names a file") {
			t.Errorf("%q: %v", args, err)
		}
	}
	v := vmEnv(t, "1.0.0")
	vc := vmCommand(v.r, v.self, io.Discard, io.Discard, func(bool) Prompter { return nil }, clientTimes{unit: time.Millisecond})
	vc.SetArgs([]string{"install", "--kit", ""})
	if err := vc.Execute(); err == nil || !strings.Contains(err.Error(), "empty: it names a file") {
		t.Errorf("vm: %v", err)
	}
}

// What is no kit is refused before the code is asked.
func TestNoCodeIsAskedForWhatIsNoKit(t *testing.T) {
	t.Parallel()
	in, _ := kitInstaller(nil)
	p := &fakePrompter{answers: map[string]string{"--kit-code": testKitCode}}
	if _, _, err := takeInto(t, in, &givenFlags{kit: "-"}, strings.NewReader("project: notes\n"), p); err == nil || !strings.Contains(err.Error(), "not a kit") {
		t.Fatal(err)
	}
	if len(p.forms) != 0 {
		t.Error("the code was asked")
	}
}

// Each install's command asks the code on its terminal, and takes its own shape's inputs.
func TestTheInstallCommandsAskTheCode(t *testing.T) {
	t.Parallel()
	seal := func() string {
		return writeTestFile(t, "kit", sealKit(t, func(k *kit.Kit) {
			k.Expires = time.Now().Add(time.Hour)
			k.Inputs = map[string]string{"VM_ONLY": "v", "DOCKER_ONLY": "d", "ADMIN_EMAIL": "a@example.org"}
		}))
	}
	shaped := []spec.Input{{Key: "VM_ONLY", Prompt: "v", Shapes: []string{"kairos"}}, {Key: "DOCKER_ONLY", Prompt: "d", Shapes: []string{"docker"}}}
	asks := &fakePrompter{answers: map[string]string{"--kit-code": "wrong"}}
	e := newEnv(t)
	cmd := newCommand(e.r, e.self, io.Discard, io.Discard, K8sOptions{}, func(bool) Prompter { return asks })
	cmd.SetArgs([]string{"--dir", e.dir, "--kit", seal()})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--kit: this code does not open it") || len(asks.forms) != 1 {
		t.Errorf("docker: %v, asked %d", err, len(asks.forms))
	}
	v := vmEnv(t, "1.0.0")
	v.r.Spec.Inputs = append(v.r.Spec.Inputs, shaped...)
	var log bytes.Buffer
	vc := vmCommand(v.r, v.self, io.Discard, &log, func(bool) Prompter { return &fakePrompter{answers: map[string]string{"--kit-code": testKitCode}} }, clientTimes{unit: time.Millisecond})
	vc.SetArgs([]string{"install", "--dir", filepath.Join(t.TempDir(), "install"), "--kit", seal(), "--channel", "Bad Channel"})
	if err := vc.Execute(); err == nil || !strings.Contains(err.Error(), "is not a channel name") {
		t.Errorf("vm: %v", err)
	}
	if !strings.Contains(log.String(), "--kit: DOCKER_ONLY left out: not an input of the kairos shape") {
		t.Errorf("vm log %q", log.String())
	}
}

// The variable leaves the environment once read, whichever source gives the code.
func TestTheCodesVariableLeavesTheEnvironment(t *testing.T) {
	t.Setenv(KitCodeVar, testKitCode)
	var log bytes.Buffer
	in := &Installer{R: &Release{Spec: &spec.Spec{Name: "notes"}}}
	in.O.Log = &log
	code, err := in.kitCode(context.Background(), "", nil)
	if err != nil || code != testKitCode {
		t.Fatalf("%q %v", code, err)
	}
	if _, set := os.LookupEnv(KitCodeVar); set {
		t.Error("still in the environment")
	}
	t.Setenv(KitCodeVar, "other")
	if code, err := in.kitCode(context.Background(), writeTestFile(t, "code", []byte(testKitCode)), nil); err != nil || code != testKitCode {
		t.Fatalf("%q %v", code, err)
	}
	if _, set := os.LookupEnv(KitCodeVar); set {
		t.Error("still in the environment, the file giving the code")
	}
}

// A kit's key the install declares a secret input delivered as a file (as_file) is recorded as the
// install's and delivered as its file, never as a variable: by --kit on Docker, by what `vm kit take`
// sends the VM's install (`install --given -`), and by --kit on Kubernetes, in the secret files' own
// Secret, never the release's.
func TestAKitsKeyDeliveredAsAFile(t *testing.T) {
	t.Parallel()
	asFile := spec.Input{Key: "API_KEY", Prompt: "API key", Secret: true, AsFile: true, Required: true}
	code := func(o *Options) {
		environment := o.Getenv
		o.Getenv = func(n string) string {
			if n == KitCodeVar {
				return testKitCode
			}
			return environment(n)
		}
	}
	docker := func(t *testing.T, take func(in *Installer) error) {
		t.Helper()
		e := newEnv(t)
		e.r.Spec.Inputs = append(e.r.Spec.Inputs, asFile)
		in := e.installer(code)
		if err := take(in); err != nil {
			t.Fatal(err)
		}
		if err := in.Install(context.Background()); err != nil {
			t.Fatalf("%v\n%s", err, e.log)
		}
		env := e.readEnv()
		path := get(env, spec.SecretFileVar("API_KEY"))
		if get(env, "API_KEY") != "from-the-kit" || path == "" {
			t.Fatalf("recorded %q at %q", get(env, "API_KEY"), path)
		}
		if got, err := os.ReadFile(filepath.Join(e.dir, path)); err != nil || string(got) != "from-the-kit" {
			t.Errorf("the file holds %q: %v", got, err)
		}
		if strings.Contains(e.log.String(), "from-the-kit") {
			t.Errorf("the value in the log:\n%s", e.log)
		}
	}
	t.Run("--kit", func(t *testing.T) {
		docker(t, func(in *Installer) error {
			f := givenFlags{kit: "-"}
			return f.take(context.Background(), in, bytes.NewReader(sealKit(t, nil)), nil, kitNow, func(g *Given, flag string) error { return in.useGiven(g, flag, in.shape()) })
		})
	})
	t.Run("vm kit take", func(t *testing.T) {
		docker(t, func(in *Installer) error {
			g, err := in.openKit(context.Background(), "-", "", bytes.NewReader(sealKit(t, nil)), nil, kitNow)
			if err != nil {
				return err
			}
			raw, err := marshalGiven(g) // what TakeKitVM sends the VM's `install --given -`
			if err != nil {
				return err
			}
			return in.takeGiven(raw)
		})
	})
	t.Run("kubernetes --kit", func(t *testing.T) {
		e := newKubeEnv(t, spec.Provider{Name: "none"})
		e.release = e.at("1.0.0", 1, func(s *spec.Spec) { s.Inputs = append(s.Inputs, asFile) })
		in := e.installer(code)
		f := givenFlags{kit: "-"}
		if err := f.take(context.Background(), in, bytes.NewReader(sealKit(t, nil)), nil, kitNow, in.useK8sGiven); err != nil {
			t.Fatal(err)
		}
		if err := in.K8sInstall(context.Background()); err != nil {
			t.Fatalf("%v\n%s", err, e.log)
		}
		if _, ok := e.secretData("notes-stowage")["API_KEY"]; ok {
			t.Errorf("the release's Secret holds it: %v", e.secretData("notes-stowage"))
		}
		if got := e.secretData("notes-stowage-secret-files")["API_KEY"]; got != "from-the-kit" {
			t.Errorf("the secret files' Secret holds %q", got)
		}
		if strings.Contains(e.log.String(), "from-the-kit") {
			t.Errorf("the value in the log:\n%s", e.log)
		}
	})
}
