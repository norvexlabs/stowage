package install

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/flock"
	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// pullingEnv is a Docker install of a release pulled with the account pull (token NOTES_PULL),
// pull:b4ked baked into its installer.
func pullingEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.r.Spec.Registry.Pull = spec.Provider{Name: "token", Config: map[string]any{"user": "pull", "token_env": "NOTES_PULL"}}
	e.r.Pull = &payload.Account{User: "pull", Password: "b4ked"}
	e.vars["DOCKER_CONFIG"] = t.TempDir()
	return e
}

// On the release's own registry the password the environment gives beats the login held, which
// beats the baked account; a login for another host is never sent there, and a mirror never gets
// the baked account.
func TestThePullAccountPrecedence(t *testing.T) {
	t.Parallel()
	e := pullingEnv(t)
	in := e.installer(nil)
	own, mirror := e.r.Spec.Registry.Prefix, "mirror.example:5000/acme/notes"
	token := func(prefix string) string {
		t.Helper()
		_, tok, err := in.pullAccount(prefix)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	write := func(raw []byte) {
		t.Helper()
		if err := os.WriteFile(registryLoginFile(e.dir), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := token(own); got != "b4ked" {
		t.Errorf("baked alone: %q", got)
	}
	if got := token(mirror); got != "" {
		t.Errorf("a mirror got %q: the baked account is the release's registry's alone", got)
	}
	write(loginFile(registryHost(own), "inst-1", loginPassword))
	if got := token(own); got != loginPassword {
		t.Errorf("held beats baked: %q", got)
	}
	e.vars["NOTES_PULL"] = "from-env"
	if got := token(own); got != "from-env" {
		t.Errorf("the environment beats held: %q", got)
	}
	if got := token(mirror); got != "" {
		t.Errorf("a mirror got %q", got)
	}
	delete(e.vars, "NOTES_PULL")
	write(loginFile("mirror.example:5000", "inst-1", loginPassword))
	if got := token(own); got != "b4ked" {
		t.Errorf("a login for another host reached the release's registry: %q", got)
	}
	if got := token(mirror); got != loginPassword {
		t.Errorf("the mirror's login: %q", got)
	}
	// Removed, the baked account again.
	if err := os.Remove(registryLoginFile(e.dir)); err != nil {
		t.Fatal(err)
	}
	if got := token(own); got != "b4ked" {
		t.Errorf("removed: %q", got)
	}
	// A login for its host on the registry of public images is taken too.
	pub := *in
	pub.R = &Release{Manifest: e.r.Manifest, Spec: &spec.Spec{Name: "notes", Registry: spec.Registry{Prefix: own, Pull: spec.Provider{Name: "none"}}}}
	write(loginFile(registryHost(own), "inst-1", loginPassword))
	if _, tok, err := pub.pullAccount(own); err != nil || tok != loginPassword {
		t.Errorf("public images: %q %v", tok, err)
	}
}

// A Docker install given --registry-login-file records it beside the install (0600) and pulls
// with it; a re-run with another replaces it; the status names its host, never the secret.
func TestADockerInstallPullsWithItsLoginFile(t *testing.T) {
	t.Parallel()
	e := pullingEnv(t)
	host := registryHost(e.r.Spec.Registry.Prefix)
	f := registryLoginFile(e.dir)
	if err := e.install(func(o *Options) { o.RegistryLogin = loginFile(host, "inst-1", loginPassword) }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if fi, err := os.Stat(f); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte("inst-1:" + loginPassword))
	if !strings.Contains(e.docker.pullConfig, b64) {
		t.Fatalf("pulled with %s", e.docker.pullConfig)
	}
	// A re-run with another replaces it; one without keeps it.
	if err := e.install(func(o *Options) { o.RegistryLogin = loginFile(host, "inst-1", "rotated-pass") }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if l, err := e.installer(nil).heldLogin(""); err != nil || l == nil || l.Password != "rotated-pass" {
		t.Fatalf("a re-run with another: %+v %v", l, err)
	}
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if l, err := e.installer(nil).heldLogin(""); err != nil || l == nil || l.Password != "rotated-pass" {
		t.Errorf("a re-run without the flag: %+v %v", l, err)
	}
	e.out.Reset()
	if err := e.installer(nil).Status(context.Background()); err != nil || !strings.Contains(e.out.String(), "login     held for "+host+"\n") {
		t.Errorf("status: %v\n%s", err, e.out)
	}
	e.out.Reset()
	if err := e.installer(nil).StatusJSON(context.Background()); err != nil {
		t.Fatal(err)
	}
	var st DockerStatus
	if err := json.Unmarshal(e.out.Bytes(), &st); err != nil || st.RegistryLogin != host {
		t.Errorf("status --json: %v %s", err, e.out)
	}
	env, _ := os.ReadFile(filepath.Join(e.dir, ".env"))
	for what, s := range map[string]string{"the log": e.log.String(), "the output": e.out.String(), ".env": string(env)} {
		if strings.Contains(s, loginPassword) || strings.Contains(s, "rotated-pass") || strings.Contains(s, b64) {
			t.Errorf("%s holds the login:\n%s", what, s)
		}
	}
	// A file that does not read is said by the status, never quoted.
	if err := os.WriteFile(f, []byte(`{"auths": {"`+host+`": {"auth": "`+loginPassword+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e.out.Reset()
	if err := e.installer(nil).Status(context.Background()); err != nil || !strings.Contains(e.out.String(), "login     unreadable: ") || !strings.Contains(e.out.String(), "notes-stow registry-login --dir") || strings.Contains(e.out.String(), loginPassword) {
		t.Errorf("unreadable: %v\n%s", err, e.out)
	}
	// uninstall --data deletes it with the directory.
	if err := e.installer(func(o *Options) { o.Yes = true }).Uninstall(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if fileExists(f) {
		t.Error("uninstall --data left the login")
	}
}

// A login for a host the install does not read is refused before anything is written; a dry run
// writes none; a mirror's install takes one for the mirror's host.
func TestADockerInstallRefusesALoginForAnotherHost(t *testing.T) {
	t.Parallel()
	e := pullingEnv(t)
	if err := e.install(func(o *Options) { o.RegistryLogin = loginFile("other.example", "inst-1", loginPassword) }); err == nil || !strings.Contains(err.Error(), "--registry-login-file: the login is for other.example, but this install reads the release's own registry ghcr.io/acme/notes at ghcr.io: give an auths entry for ghcr.io") {
		t.Errorf("%v", err)
	}
	e.nothingWritten()
	if fileExists(registryLoginFile(e.dir)) {
		t.Error("a refused login is recorded")
	}
	if err := e.install(func(o *Options) { o.DryRun = true; o.RegistryLogin = loginFile("ghcr.io", "inst-1", loginPassword) }); err != nil || fileExists(registryLoginFile(e.dir)) || !strings.Contains(e.log.String(), "a run would record the login to ghcr.io") {
		t.Errorf("dry run: %v\n%s", err, e.log)
	}
	// A first install that fails removes the login it wrote.
	e.docker.pullErr = errors.New("unauthorized")
	if err := e.install(func(o *Options) { o.RegistryLogin = loginFile("ghcr.io", "inst-1", loginPassword) }); err == nil {
		t.Fatal("installed")
	}
	if fileExists(registryLoginFile(e.dir)) {
		t.Error("a failed first install left its login")
	}
}

// The Docker registry-login verb: no root needed, an install needed; it records and removes.
func TestTheDockerRegistryLoginVerb(t *testing.T) {
	t.Parallel()
	e := pullingEnv(t)
	run := func(stdin string, args ...string) (string, error) {
		var out bytes.Buffer
		o := e.installer(nil).O
		cmd := registryLoginCommand(&Installer{R: e.r}, &o, &out, nil)
		cmd.SetArgs(args)
		cmd.SetIn(strings.NewReader(stdin))
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		err := cmd.Execute()
		return out.String(), err
	}
	raw := string(loginFile("ghcr.io", "inst-1", loginPassword))
	if _, err := run(raw, "-"); err == nil || !strings.Contains(err.Error(), "holds no install: the first install takes its login with --registry-login-file") {
		t.Errorf("no install: %v", err)
	}
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	out, err := run(raw, "-")
	if err != nil || !strings.Contains(out, "Recorded the login to ghcr.io") || !strings.Contains(out, "the install owner's alone") || strings.Contains(out, loginPassword) {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := run(string(loginFile("other.example", "x", "y")), "-"); err == nil || !strings.Contains(err.Error(), "standard input: the login is for other.example") {
		t.Errorf("another host: %v", err)
	}
	if out, err := run("", "--remove"); err != nil || !strings.Contains(out, "Removed the login held") || fileExists(registryLoginFile(e.dir)) {
		t.Errorf("%q %v", out, err)
	}
	if _, tok, err := e.installer(nil).pullAccount("ghcr.io/acme/notes"); err != nil || tok != "b4ked" {
		t.Errorf("removed, the baked account: %q %v", tok, err)
	}
}

// pullApplied is the pull Secret the last apply wrote: its labels and its one auths entry's
// password; nil labels when none was written.
func pullApplied(t *testing.T, e *kubeEnv) (labels map[string]any, password string) {
	t.Helper()
	for _, o := range e.f.applied {
		md := o["metadata"].(map[string]any)
		if o["kind"] != "Secret" || md["name"] != "notes-stowage-pull" {
			continue
		}
		labels, _ = md["labels"].(map[string]any)
		var cfg struct {
			Auths map[string]struct{ Password string }
		}
		if err := json.Unmarshal([]byte(e.secretData("notes-stowage-pull")[".dockerconfigjson"]), &cfg); err != nil {
			t.Fatal(err)
		}
		for _, a := range cfg.Auths {
			password = a.Password
		}
	}
	return labels, password
}

// On Kubernetes the pull Secret written from --registry-login-file is the login's record: a run
// without the flag keeps it, the pull account's password given replaces it, the flag again
// replaces it.
func TestK8sPullSecretKeepsTheLoginARunGave(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "token", Config: map[string]any{"user": "notes-pull", "token_env": "NOTES_PULL"}})
	e.release.Pull = &payload.Account{User: "notes-pull", Password: "baked"}
	host := registryHost(e.prefix)
	login := func(pass string) func(*Options) {
		return func(o *Options) { o.RegistryLogin = loginFile(host, "inst-1", pass) }
	}
	if err := e.installer(login(loginPassword)).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if labels, pass := pullApplied(t, e); labels[pullLoginLabel] != pullLoginByRun || pass != loginPassword {
		t.Fatalf("install: %v %q", labels, pass)
	}
	if basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("inst-1:"+loginPassword)); !slices.Contains(e.auth, basic) {
		t.Errorf("the chart was not read with the login: %q", e.auth)
	}
	if strings.Contains(e.log.String()+e.out.String(), loginPassword) {
		t.Errorf("the login is printed:\n%s%s", e.log, e.out)
	}
	upgrade := func(mod func(*Options)) {
		t.Helper()
		e.runs("0.9.0", 1)
		e.f.values["stowage"].(map[string]any)["pullSecret"] = "notes-stowage-pull" // as the install named it
		e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
		e.f.applied, e.f.given = nil, nil
		e.log.Reset()
		if err := e.installer(mod).K8sUpgrade(context.Background()); err != nil {
			t.Fatalf("%v\n%s", err, e.log)
		}
	}
	upgrade(nil)
	if labels, _ := pullApplied(t, e); labels != nil || !strings.Contains(e.f.given[0], "pullSecret: notes-stowage-pull\n") || !strings.Contains(e.log.String(), "keeps the login --registry-login-file gave for "+host) {
		t.Errorf("without the flag: %v %q\n%s", labels, e.f.given, e.log)
	}
	upgrade(login("rotated-pass"))
	if labels, pass := pullApplied(t, e); labels[pullLoginLabel] != pullLoginByRun || pass != "rotated-pass" {
		t.Errorf("with the flag: %v %q", labels, pass)
	}
	e.vars["NOTES_PULL"] = "from-env"
	upgrade(nil)
	if labels, pass := pullApplied(t, e); labels == nil || labels[pullLoginLabel] != nil || pass != "from-env" || !strings.Contains(e.log.String(), "the pull account's password, given, replaces it") {
		t.Errorf("with the password: %v %q\n%s", labels, pass, e.log)
	}
	// Replaced, it is the pull account's: a run without either writes the baked one.
	delete(e.vars, "NOTES_PULL")
	upgrade(nil)
	if _, pass := pullApplied(t, e); pass != "baked" {
		t.Errorf("after: %q", pass)
	}
}

func TestK8sRefusesALoginFileItCannotTake(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		mod func(e *kubeEnv, o *Options)
		err string
	}{
		"another host": {func(e *kubeEnv, o *Options) { o.RegistryLogin = loginFile("other.example", "x", loginPassword) },
			"--registry-login-file: the login is for other.example, but the release's images come from "},
		"with --image-pull-login": {func(e *kubeEnv, o *Options) {
			o.RegistryLogin, o.K8s.PullLogin = loginFile(registryHost(e.prefix), "x", loginPassword), true
		}, "--registry-login-file gives the pull Secret its login, --image-pull-login copies the Docker client's: give one"},
		"with the password": {func(e *kubeEnv, o *Options) {
			o.RegistryLogin = loginFile(registryHost(e.prefix), "x", loginPassword)
			e.vars["NOTES_PULL"] = "from-env"
		}, "--registry-login-file and NOTES_PULL both give the pull Secret of "},
		"not a login": {func(e *kubeEnv, o *Options) { o.RegistryLogin = []byte("{}") }, "--registry-login-file: 0 entries in auths"},
	} {
		e := newKubeEnv(t, spec.Provider{Name: "token", Config: map[string]any{"user": "notes-pull", "token_env": "NOTES_PULL"}})
		err := e.installer(func(o *Options) { c.mod(e, o) }).K8sInstall(context.Background())
		if err == nil || !strings.Contains(err.Error(), c.err) || len(e.f.applied) > 0 || strings.Contains(err.Error(), loginPassword) {
			t.Errorf("%s: %v (applied %d)", name, err, len(e.f.applied))
		}
	}
}

// The flag reaches the run: on Docker and on Kubernetes it refuses an empty value and a standard
// input another flag reads, and k8s install writes the pull Secret from the file it names.
func TestTheRegistryLoginFileFlag(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	run := func(args ...string) error {
		cmd := newCommand(e.release, "notes-stow", io.Discard, io.Discard, K8sOptions{Helm: kubeTool{e.f, "helm"}, Kubectl: kubeTool{e.f, "kubectl"}}, func(bool) Prompter { return nil })
		cmd.SetArgs(args)
		cmd.SetIn(strings.NewReader("{}"))
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		return cmd.Execute()
	}
	for _, args := range [][]string{
		{"--dir", filepath.Join(t.TempDir(), "notes"), "--no-input", "--registry-login-file", "-", "--given", "-"},
		{"--dir", filepath.Join(t.TempDir(), "notes"), "--no-input", "--registry-login-file", "-", "--kit", "-"},
		{"k8s", "install", "--no-input", "--registry-login-file", "-", "--given", "-"},
	} {
		if err := run(args...); err == nil || !strings.Contains(err.Error(), "--registry-login-file and another flag both name standard input") {
			t.Errorf("%q: %v", args, err)
		}
	}
	for _, args := range [][]string{{"--registry-login-file", ""}, {"k8s", "install", "--registry-login-file", ""}} {
		if err := run(args...); err == nil || !strings.Contains(err.Error(), "empty: it names a file") {
			t.Errorf("%q: %v", args, err)
		}
	}
	if c, _, err := newCommand(e.release, "notes-stow", io.Discard, io.Discard, K8sOptions{}, nil).Find([]string{"registry-login"}); err != nil || c.Name() != "registry-login" || c.Flags().Lookup("dir") == nil {
		t.Errorf("the Docker verb: %v", err)
	}
	file := writeTestFile(t, "login.json", loginFile(registryHost(e.prefix), "inst-1", loginPassword))
	if err := run("k8s", "install", "--no-input", "--no-provision", "--set", "ADMIN_EMAIL=a@example.org", "--registry-login-file", file); err != nil {
		t.Fatal(err)
	}
	if labels, pass := pullApplied(t, e); labels[pullLoginLabel] != pullLoginByRun || pass != loginPassword {
		t.Errorf("%v %q", labels, pass)
	}
}

// reset-db across a baseline writes the new release's pull account but over a login a run gave,
// unless the pull account's password is given.
func TestK8sResetAcrossABaselineKeepsTheLoginARunGave(t *testing.T) {
	t.Parallel()
	for name, given := range map[string]bool{"kept": false, "the password given": true} {
		e := newKubeEnv(t, spec.Provider{Name: "token", Config: map[string]any{"user": "notes-pull", "token_env": "NOTES_PULL"}})
		e.publishAt("1.0.0", 1)
		e.runs("1.0.0", 1)
		e.release = e.at("2.0.0", 1, atBaseline(2))
		e.release.Pull = &payload.Account{User: "notes-pull", Password: "baked"}
		e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
		e.f.manifest = resetManifest
		e.f.claims = []string{"data-notes-postgres-0"}
		cfg, _ := dockerAuths(registryHost(e.prefix), "inst-1", loginPassword)
		held, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{
			"app.kubernetes.io/managed-by": "stowage", "app.kubernetes.io/part-of": "notes", "app.kubernetes.io/instance": "notes", pullLoginLabel: pullLoginByRun}},
			"data": map[string][]byte{".dockerconfigjson": cfg}})
		e.f.pull = string(held)
		if given {
			e.vars["NOTES_PULL"] = "from-env"
		}
		if err := e.reset(func(o *Options) { o.Yes, o.NewBaseline = true, true }); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, e.log)
		}
		switch _, pass := pullApplied(t, e); {
		case !given && (pass != "" || !strings.Contains(e.log.String(), "keeps the login --registry-login-file gave")):
			t.Errorf("%s: written %q\n%s", name, pass, e.log)
		case given && pass != "from-env":
			t.Errorf("%s: written %q", name, pass)
		}
	}
}

// heldPullSecret is the release's pull Secret holding a login for host, as `kubectl get` gives
// it; labelled when a run wrote it from --registry-login-file (pullLoginLabel).
func heldPullSecret(host, user, pass string, labelled bool) string {
	labels := map[string]string{"app.kubernetes.io/managed-by": "stowage", "app.kubernetes.io/part-of": "notes", "app.kubernetes.io/instance": "notes"}
	if labelled {
		labels[pullLoginLabel] = pullLoginByRun
	}
	cfg, _ := dockerAuths(host, user, pass)
	raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}, "data": map[string][]byte{".dockerconfigjson": cfg}})
	return string(raw)
}

// basicAuth is the Authorization header a registry read with user and pass sends.
func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// A Docker run failing before the images are fetched with its new login puts the login held
// before back (0600); one failing after keeps the new login, which pulled; a first install over a
// login held before does the same, and removes one it wrote where none was.
func TestAFailedRunPutsBackTheLoginHeld(t *testing.T) {
	t.Parallel()
	e := pullingEnv(t)
	host := registryHost(e.r.Spec.Registry.Prefix)
	f := registryLoginFile(e.dir)
	if err := e.install(func(o *Options) { o.RegistryLogin = loginFile(host, "inst-1", loginPassword) }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	held, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	restored := func(what string) {
		t.Helper()
		got, err := os.ReadFile(f)
		fi, serr := os.Stat(f)
		if err != nil || serr != nil || !bytes.Equal(got, held) || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: the login held is not back: %v %v %s\n%s", what, err, serr, got, e.log)
		}
	}
	e.docker.loaded = map[string]string{} // the images are gone: the re-run pulls them
	e.docker.pullErr = errors.New("unauthorized")
	e.log.Reset()
	if err := e.install(func(o *Options) { o.RegistryLogin = loginFile(host, "inst-1", "bad-pass") }); err == nil {
		t.Fatal("the re-run passed")
	}
	restored("a re-run")
	if !strings.Contains(e.log.String(), "the login held before it is restored") {
		t.Errorf("unsaid:\n%s", e.log)
	}
	// Failing after the images are fetched with it, the new login stays.
	e.docker.pullErr, e.docker.upErr = nil, errors.New("the stack did not come up")
	e.log.Reset()
	if err := e.install(func(o *Options) { o.RegistryLogin = loginFile(host, "inst-1", "rotated-pass") }); err == nil {
		t.Fatal("the re-run passed")
	}
	if l, err := e.installer(nil).heldLogin(""); err != nil || l == nil || l.Password != "rotated-pass" || !strings.Contains(e.log.String(), "the login stays") {
		t.Errorf("a re-run failing after the fetch: %+v %v\n%s", l, err, e.log)
	}
	// A first install beside a login held before.
	e2 := pullingEnv(t)
	f = registryLoginFile(e2.dir)
	if err := os.WriteFile(f, held, 0o600); err != nil {
		t.Fatal(err)
	}
	e2.docker.pullErr = errors.New("unauthorized")
	if err := e2.install(func(o *Options) { o.RegistryLogin = loginFile(host, "inst-1", "bad-pass") }); err == nil {
		t.Fatal("the first install passed")
	}
	restored("a first install")
	e2.docker.pullErr, e2.docker.upErr = nil, errors.New("the stack did not come up")
	if err := e2.install(func(o *Options) { o.RegistryLogin = loginFile(host, "inst-1", "rotated-pass") }); err == nil {
		t.Fatal("the first install passed")
	}
	if l, err := e2.installer(nil).heldLogin(""); err != nil || l == nil || l.Password != "rotated-pass" {
		t.Errorf("a first install failing after the fetch: %+v %v\n%s", l, err, e2.log)
	}
}

// Without --registry-login-file a Kubernetes run reads the release's registry with the login its
// pull Secret holds from a run before, for that host alone; the password TOKEN_ENV gives wins,
// and a Secret without the label is never read as a login.
func TestK8sReadsWithTheLoginThePullSecretHolds(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "token", Config: map[string]any{"user": "notes-pull", "token_env": "NOTES_PULL"}})
	e.release.Pull = &payload.Account{User: "notes-pull", Password: "baked"}
	host := registryHost(e.prefix)
	upgrade := func(secret string) {
		t.Helper()
		e.runs("0.9.0", 1)
		e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
		e.f.pull, e.f.applied, e.f.given = secret, nil, nil
		e.mu.Lock()
		e.auth = nil
		e.mu.Unlock()
		e.log.Reset()
		if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
			t.Fatalf("%v\n%s", err, e.log)
		}
	}
	held := basicAuth("inst-1", loginPassword)
	upgrade(heldPullSecret(host, "inst-1", loginPassword, true))
	if !slices.Contains(e.auth, held) || slices.Contains(e.auth, basicAuth("notes-pull", "baked")) {
		t.Errorf("the held login: %q", e.auth)
	}
	e.vars["NOTES_PULL"] = "from-env"
	upgrade(heldPullSecret(host, "inst-1", loginPassword, true))
	if slices.Contains(e.auth, held) || !slices.Contains(e.auth, basicAuth("notes-pull", "from-env")) {
		t.Errorf("the password given: %q", e.auth)
	}
	delete(e.vars, "NOTES_PULL")
	upgrade(heldPullSecret(host, "inst-1", loginPassword, false))
	if slices.Contains(e.auth, held) || !slices.Contains(e.auth, basicAuth("notes-pull", "baked")) {
		t.Errorf("an unlabelled Secret read as a login: %q", e.auth)
	}
	// A login for another host never reaches this one.
	upgrade(heldPullSecret("other.example", "inst-1", loginPassword, true))
	if slices.Contains(e.auth, held) {
		t.Errorf("another host's login: %q", e.auth)
	}
}

// k8s upgrade --version reads the release and its installer with --registry-login-file's login
// and gives the installer it runs the login on a pipe it inherits (descriptor 3), never a file on
// disk; a failure says what an installer before the flag needs.
func TestK8sUpgradeVersionTakesTheLoginFile(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.publish("2.0.0", 1)
	e.runs("1.0.0", 1)
	host := registryHost(e.prefix)
	var file string
	var childErr error
	in := e.installer(func(o *Options) { o.RegistryLogin = loginFile(host, "inst-1", loginPassword) })
	in.O.Exec = func(_ context.Context, _ string, args []string) error {
		e.ran = append(e.ran, args)
		file = ""
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "--registry-login-file="); ok {
				file = v
			}
		}
		if file != "/dev/fd/3" || len(in.O.childFiles) != 1 {
			t.Fatalf("the child's login %q, descriptors %v", file, in.O.childFiles)
		}
		raw, err := io.ReadAll(in.O.childFiles[0])
		if l, perr := parseRegistryLogin(raw); err != nil || perr != nil || l.Host != host || l.Password != loginPassword {
			t.Errorf("the child's login: %+v %v %v", l, err, perr)
		}
		return childErr
	}
	if err := in.K8sUpgradeTo(context.Background(), "2.0.0", []string{"--namespace=notes"}); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if !slices.Contains(e.auth, basicAuth("inst-1", loginPassword)) {
		t.Errorf("the release was not read with the login: %q", e.auth)
	}
	if len(e.ran) != 1 || in.O.childFiles != nil {
		t.Errorf("ran %q, descriptors left %v", e.ran, in.O.childFiles)
	}
	childErr = errors.New("unknown flag: --registry-login-file")
	if err := in.K8sUpgradeTo(context.Background(), "2.0.0", nil); err == nil || !strings.Contains(e.log.String(), "note: if it refused --registry-login-file as an unknown flag, it was made before it") || in.O.childFiles != nil {
		t.Errorf("an installer before the flag: %v (descriptors left %v)\n%s", err, in.O.childFiles, e.log)
	}
	if strings.Contains(e.log.String()+e.out.String(), loginPassword) {
		t.Errorf("the login is printed:\n%s%s", e.log, e.out)
	}
}

// The flag reaches k8s upgrade --version: a login for another host is refused before anything
// is fetched, as standard input another flag names.
func TestK8sUpgradeVersionReadsTheLoginFileFlag(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	cmd := newCommand(e.release, "notes-stow", io.Discard, io.Discard, K8sOptions{Helm: kubeTool{e.f, "helm"}, Kubectl: kubeTool{e.f, "kubectl"}}, func(bool) Prompter { return nil })
	cmd.SetArgs([]string{"k8s", "upgrade", "--version", "2.0.0", "--registry-login-file", "-"})
	cmd.SetIn(bytes.NewReader(loginFile("other.example", "inst-1", loginPassword)))
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--registry-login-file: the login is for other.example, but the release's images come from ") {
		t.Errorf("%v", err)
	}
}

// A login a run gave for another host than the images' is never named again for them: an
// upgrade and a reset across a baseline drop the pods' reference to it and say how to give one,
// never that it is replaced; the pull account, when the run has one, replaces it.
func TestK8sALoginForAnotherHostIsNotNamedAgain(t *testing.T) {
	t.Parallel()
	other := heldPullSecret("other.example", "inst-1", loginPassword, true)
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("0.9.0", 1)
	e.f.values["stowage"].(map[string]any)["pullSecret"] = "notes-stowage-pull"
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
	e.f.pull = other
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if len(e.f.given) == 0 || !strings.Contains(e.f.given[0], "pullSecret: \"\"\n") || !strings.Contains(e.log.String(), "the pods no longer name it and pull from") || strings.Contains(e.log.String(), "is replaced") {
		t.Errorf("upgrade: %q\n%s", e.f.given, e.log)
	}
	// With the pull account, it is written over.
	e = newKubeEnv(t, spec.Provider{Name: "token", Config: map[string]any{"user": "notes-pull", "token_env": "NOTES_PULL"}})
	e.release.Pull = &payload.Account{User: "notes-pull", Password: "baked"}
	e.runs("0.9.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
	e.f.pull = other
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if labels, pass := pullApplied(t, e); labels == nil || labels[pullLoginLabel] != nil || pass != "baked" || !strings.Contains(e.log.String(), "the pull account replaces it") {
		t.Errorf("with the pull account: %v %q\n%s", labels, pass, e.log)
	}
	// reset-db across a baseline applies the same rule.
	e = newKubeEnv(t, spec.Provider{Name: "none"})
	e.publishAt("1.0.0", 1)
	e.runs("1.0.0", 1)
	e.f.values["stowage"].(map[string]any)["pullSecret"] = "notes-stowage-pull"
	e.release = e.at("2.0.0", 1, atBaseline(2))
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
	e.f.manifest = resetManifest
	e.f.claims = []string{"data-notes-postgres-0"}
	e.f.pull = other
	if err := e.reset(func(o *Options) { o.Yes, o.NewBaseline = true, true }); err != nil {
		t.Fatalf("reset: %v\n%s", err, e.log)
	}
	if len(e.f.given) == 0 || strings.Contains(e.f.given[len(e.f.given)-1], "notes-stowage-pull") || !strings.Contains(e.log.String(), "the pods no longer name it and pull from") {
		t.Errorf("reset: %q\n%s", e.f.given, e.log)
	}
}

// The installer a run starts reads the login childLogin hands it at the path it names, from a
// pipe, beside the lock it is handed; nothing is written to disk for it.
func TestTheChildReadsTheLoginFromItsDescriptor(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	exe := filepath.Join(dir, "child")
	if err := testutil.WriteExecutable(exe, []byte("#!/bin/sh\ncat \"$1\"\n")); err != nil {
		t.Fatal(err)
	}
	lock, err := os.Create(filepath.Join(dir, "lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	in := &Installer{k8sLogin: &registryLogin{Host: "ghcr.io", User: "inst-1", Password: loginPassword}}
	in.O.handLock = lock
	path, done, err := in.childLogin()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	in.O.Log = io.Discard
	err = in.O.execTo(context.Background(), exe, []string{path}, &out)
	done()
	if l, perr := parseRegistryLogin(out.Bytes()); err != nil || perr != nil || l.Password != loginPassword {
		t.Errorf("the child read %q: %v %v", out.String(), err, perr)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 2 || in.O.childFiles != nil {
		t.Errorf("left %v, descriptors %v", ents, in.O.childFiles)
	}
}

// A Kubernetes run's registry read before k8sLogins took the pull Secret's login is refused:
// no verb reads without the login the release's pull Secret holds.
func TestK8sReadsWaitForThePullSecretsLogin(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	in := e.installer(nil)
	if err := in.checkTarget(); err != nil {
		t.Fatal(err)
	}
	if _, err := in.registryClient(e.prefix); err == nil || !strings.Contains(err.Error(), "before it takes the pull Secret's login") {
		t.Errorf("read before k8sLogins: %v", err)
	}
	if err := in.k8sLogins(context.Background(), e.prefix, false); err != nil {
		t.Fatal(err)
	}
	if _, err := in.registryClient(e.prefix); err != nil {
		t.Errorf("after k8sLogins: %v", err)
	}
}

// An upgrade keeping the login a run gave leaves the pods naming the pull Secret the operator's
// values name, never the installer's in its place.
func TestK8sKeptLoginLeavesTheOperatorsPullSecret(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("0.9.0", 1)
	e.f.values["stowage"].(map[string]any)["pullSecret"] = "my-registry"
	e.f.objects = map[string]string{"secret/my-registry": "secret/my-registry\n"}
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
	e.f.pull = heldPullSecret(registryHost(e.prefix), "inst-1", loginPassword, true)
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if len(e.f.given) == 0 || !strings.Contains(e.f.given[0], "pullSecret: my-registry\n") {
		t.Errorf("values %q\n%s", e.f.given, e.log)
	}
}

// pullUnreadLine is what a run logs, once, of a labelled pull Secret whose login does not read.
const pullUnreadLine = "the pull Secret notes-stowage-pull: the login --registry-login-file gave does not read ("
const pullUnreadHint = "); this run reads the registry without it (`notes-stow k8s upgrade --namespace notes --release notes --registry-login-file FILE` gives one again)"

// errPullForbidden is kubectl's read of the pull Secret failing, as an API error.
var errPullForbidden = errors.New(`secrets "notes-stowage-pull" is forbidden`)

// pullUnreadOnce reports whether log says once that the pull Secret's login does not read, with
// the way to give one again.
func pullUnreadOnce(log string) bool {
	return strings.Count(log, pullUnreadLine) == 1 && strings.Count(log, pullUnreadHint) == 1
}

// A labelled pull Secret whose login does not read is logged once, with the way to give a login
// again, and every verb goes on as one holding no login: an upgrade without an account keeps the
// Secret the running release names, and the updates are read without it.
func TestK8sAPullSecretThatDoesNotReadIsReadWithout(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.publish("2.0.0", 1)
	point(t, e.prefix, "stable", "2.0.0-1")
	e.runs("0.9.0", 1)
	e.f.values["stowage"].(map[string]any)["pullSecret"] = "notes-stowage-pull"
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
	e.f.pull = garbledPullSecret()
	err := e.installer(nil).K8sUpgrade(context.Background())
	log := e.log.String()
	if labels, _ := pullApplied(t, e); err != nil || labels != nil || e.f.ran("helm upgrade") == nil || len(e.f.given) == 0 || !strings.Contains(e.f.given[0], "pullSecret: notes-stowage-pull\n") {
		t.Errorf("upgrade: %v (written %v, values %q)\n%s", err, labels, e.f.given, log)
	}
	if !pullUnreadOnce(log) || strings.Contains(log, loginPassword) {
		t.Errorf("upgrade's log:\n%s", log)
	}
	e.log.Reset()
	if u, err := e.installer(nil).K8sUpdates(context.Background(), "", ""); err != nil || !u.Available || !pullUnreadOnce(e.log.String()) {
		t.Errorf("updates: %+v %v\n%s", u, err, e.log)
	}
}

// A pull Secret kubectl cannot read refuses a run that writes it before anything is asked or
// applied, as it would write over a login it cannot see; the Secret keeps its label and login. A
// run that only reads logs it and goes on without the login. Neither names
// --registry-login-file, which cannot help a read that fails.
func TestK8sAPullSecretThatCannotBeReadRefusesAWrite(t *testing.T) {
	t.Parallel()
	held := heldPullSecret("ghcr.io", "inst-1", loginPassword, true)
	e := newKubeEnv(t, spec.Provider{Name: "token", Config: map[string]any{"user": "notes-pull", "token_env": "NOTES_PULL"}})
	e.release.Pull = &payload.Account{User: "notes-pull", Password: "baked"}
	e.publish("2.0.0", 1)
	point(t, e.prefix, "stable", "2.0.0-1")
	e.runs("0.9.0", 1)
	e.f.values["stowage"].(map[string]any)["pullSecret"] = "notes-stowage-pull"
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
	e.f.pull, e.f.pullErr = held, errPullForbidden
	err := e.installer(nil).K8sUpgrade(context.Background())
	want := `the pull Secret notes-stowage-pull could not be read (secrets "notes-stowage-pull" is forbidden): run again once it can be read (the run needs ` + "`get`" + ` on that Secret in notes) — a run that cannot see it would overwrite the login --registry-login-file gave`
	if err == nil || err.Error() != want || len(e.f.applied) != 0 || e.f.ran("helm upgrade") != nil || e.f.pull != held {
		t.Errorf("upgrade: %v (applied %d)\n%s", err, len(e.f.applied), e.log)
	}
	if strings.Contains(e.log.String(), "FILE") {
		t.Errorf("upgrade's log names --registry-login-file:\n%s", e.log)
	}
	e.log.Reset()
	e.f.pullErr = errPullForbidden
	u, err := e.installer(nil).K8sUpdates(context.Background(), "", "")
	log := e.log.String()
	if err != nil || !u.Available || strings.Count(log, `the pull Secret notes-stowage-pull could not be read (secrets "notes-stowage-pull" is forbidden); this run reads the registry without the login it may hold`) != 1 || strings.Contains(log, "FILE") {
		t.Errorf("updates: %+v %v\n%s", u, err, log)
	}
	// A dry run refuses as the run it previews would.
	e.f.pullErr = errPullForbidden
	if err := e.installer(func(o *Options) { o.DryRun = true }).K8sUpgrade(context.Background()); err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Errorf("a dry run: %v", err)
	}
}

// garbledPullSecret is the release's pull Secret, labelled as holding a login a run gave, whose
// login does not read.
func garbledPullSecret() string {
	raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{
		"app.kubernetes.io/managed-by": "stowage", "app.kubernetes.io/part-of": "notes", "app.kubernetes.io/instance": "notes", pullLoginLabel: pullLoginByRun}},
		"data": map[string][]byte{".dockerconfigjson": []byte(`{"auths": {"ghcr.io": {"auth": "` + loginPassword + `"}}}`)}})
	return string(raw)
}

// A writing run with a pull account writes it over a labelled pull Secret whose login does not
// read: the password TOKEN_ENV gives, else the one baked in.
func TestK8sAPullSecretThatDoesNotReadIsReplaced(t *testing.T) {
	t.Parallel()
	for want, mod := range map[string]func(*kubeEnv){
		"from-env": func(e *kubeEnv) { e.vars["NOTES_PULL"] = "from-env" },
		"baked":    nil,
	} {
		e := newKubeEnv(t, spec.Provider{Name: "token", Config: map[string]any{"user": "notes-pull", "token_env": "NOTES_PULL"}})
		e.release.Pull = &payload.Account{User: "notes-pull", Password: "baked"}
		e.runs("0.9.0", 1)
		e.f.values["stowage"].(map[string]any)["pullSecret"] = "notes-stowage-pull"
		e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
		e.f.pull = garbledPullSecret()
		if mod != nil {
			mod(e)
		}
		if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil || !pullUnreadOnce(e.log.String()) {
			t.Errorf("%s: %v\n%s", want, err, e.log)
		}
		if labels, pass := pullApplied(t, e); labels == nil || labels[pullLoginLabel] != nil || pass != want {
			t.Errorf("%s: written %v %q\n%s", want, labels, pass, e.log)
		}
	}
}

// reset-db with a labelled pull Secret whose login does not read goes on as one holding no
// login: a plain reset writes no pull Secret, and across a baseline the pull account replaces it.
// One kubectl cannot read refuses it before anything goes; one refused on its flags reads none.
func TestK8sResetWithAPullSecretThatDoesNotRead(t *testing.T) {
	t.Parallel()
	reset := func(across bool, token string, pullErr error, mod func(*Options)) (*kubeEnv, error) {
		e := newKubeEnv(t, spec.Provider{Name: "token", Config: map[string]any{"user": "notes-pull", "token_env": "NOTES_PULL"}})
		e.release.Pull = &payload.Account{User: "notes-pull", Password: "baked"}
		e.runs("1.0.0", 1)
		if across {
			e.publishAt("1.0.0", 1)
			e.release = e.at("2.0.0", 1, atBaseline(2))
			e.release.Pull = &payload.Account{User: "notes-pull", Password: "baked"}
		}
		e.f.values["stowage"].(map[string]any)["pullSecret"] = "notes-stowage-pull"
		e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
		e.f.manifest = resetManifest
		e.f.claims = []string{"data-notes-postgres-0"}
		e.f.pull, e.f.pullErr = garbledPullSecret(), pullErr
		if token != "" {
			e.vars["NOTES_PULL"] = token
		}
		return e, e.reset(func(o *Options) {
			o.Yes, o.NewBaseline = true, across
			if mod != nil {
				mod(o)
			}
		})
	}
	if e, err := reset(false, "", nil, nil); err != nil || !pullUnreadOnce(e.log.String()) || !strings.Contains(e.log.String(), "installing notes 1.0.0 (chart 0.1.0) in notes again") {
		t.Errorf("a plain reset: %v\n%s", err, e.log)
	}
	for _, token := range []string{"from-env", ""} {
		e, err := reset(true, token, nil, nil)
		if _, pass := pullApplied(t, e); err != nil || pass != cmp.Or(token, "baked") || !pullUnreadOnce(e.log.String()) {
			t.Errorf("across a baseline, token %q: %v %q\n%s", token, err, pass, e.log)
		}
	}
	// A plain reset writes no pull Secret: a read that fails is logged and the reset goes on.
	if e, err := reset(false, "", errPullForbidden, nil); err != nil || !strings.Contains(e.log.String(), "could not be read (secrets \"notes-stowage-pull\" is forbidden); this run reads the registry without the login it may hold") {
		t.Errorf("a plain reset whose read fails: %v\n%s", err, e.log)
	}
	// A crossing writes it: refused before anything is applied.
	if e, err := reset(true, "", errPullForbidden, nil); err == nil || !strings.Contains(err.Error(), "could not be read (secrets \"notes-stowage-pull\" is forbidden): run again once it can be read") || len(e.f.applied) != 0 || e.f.ran("helm uninstall") != nil {
		t.Errorf("a crossing whose read fails: %v\n%s", err, e.log)
	}
	e, err := reset(false, "", nil, func(o *Options) { o.NewBaseline = true })
	if err == nil || !strings.Contains(err.Error(), "--new-baseline installs another release") || e.f.ran("kubectl get secret notes-stowage-pull") != nil {
		t.Errorf("--new-baseline on the release it runs: %v, read %q", err, e.f.ran("kubectl get secret notes-stowage-pull"))
	}
}

// A login dropped for another host's images is named again once they come from its host: an
// install with the login (host A), an upgrade from B drops it, an upgrade back from A names it,
// the release then naming none.
func TestK8sADroppedLoginIsNamedAgainForItsHost(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	a := e.prefix
	b := strings.Replace(a, "127.0.0.1:", "localhost:", 1) // the same registry, another host
	if registryHost(a) == registryHost(b) {
		t.Fatalf("one host: %s", a)
	}
	if err := e.installer(func(o *Options) { o.RegistryLogin = loginFile(registryHost(a), "inst-1", loginPassword) }).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if len(e.f.given) == 0 || !strings.Contains(e.f.given[0], "pullSecret: notes-stowage-pull\n") {
		t.Fatalf("install: %q", e.f.given)
	}
	upgrade := func(from, to, named string) string {
		t.Helper()
		e.runs("0.9.0", 1)
		st := e.f.values["stowage"].(map[string]any)
		st["registry"], st["pullSecret"] = from, named
		e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
		e.f.applied, e.f.given = nil, nil
		e.mu.Lock()
		e.auth = nil
		e.mu.Unlock()
		e.log.Reset()
		if err := e.installer(func(o *Options) { o.K8s.Registry = to }).K8sUpgrade(context.Background()); err != nil {
			t.Fatalf("%v\n%s", err, e.log)
		}
		if len(e.f.given) == 0 {
			t.Fatal("no values given")
		}
		return e.f.given[0]
	}
	if got := upgrade(a, b, "notes-stowage-pull"); !strings.Contains(got, "pullSecret: \"\"\n") {
		t.Errorf("from B: %q\n%s", got, e.log)
	}
	got := upgrade(b, a, "")
	if labels, _ := pullApplied(t, e); labels != nil || !strings.Contains(got, "pullSecret: notes-stowage-pull\n") || !slices.Contains(e.auth, basicAuth("inst-1", loginPassword)) {
		t.Errorf("back from A: %q (written %v, auth %q)\n%s", got, labels, e.auth, e.log)
	}
}

// k8sLogins reads the pull Secret once per run and prefix: every reader of the run takes the
// one login, read again only for another prefix.
func TestK8sLoginsReadThePullSecretOncePerPrefix(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	host := registryHost(e.prefix)
	e.f.pull = heldPullSecret(host, "inst-1", loginPassword, true)
	in := e.installer(nil)
	if err := in.checkTarget(); err != nil {
		t.Fatal(err)
	}
	reads := func() int { return len(e.f.ran("kubectl get secret notes-stowage-pull")) }
	if err := in.k8sLogins(context.Background(), e.prefix, false); err != nil {
		t.Fatal(err)
	}
	e.f.pull = heldPullSecret(host, "inst-1", "rotated-pass", true)
	if err := in.k8sLogins(context.Background(), e.prefix, false); err != nil {
		t.Fatal(err)
	}
	if l, err := in.heldLogin(e.prefix); err != nil || l == nil || l.Password != loginPassword || reads() != 1 {
		t.Errorf("one prefix: %+v %v, %d reads", l, err, reads())
	}
	if err := in.k8sLogins(context.Background(), e.prefix+"/mirror", false); err != nil {
		t.Fatal(err)
	}
	if l, err := in.heldLogin(e.prefix + "/mirror"); err != nil || l == nil || l.Password != "rotated-pass" || reads() != 2 {
		t.Errorf("another prefix: %+v %v, %d reads", l, err, reads())
	}
	if err := in.k8sLogins(context.Background(), e.prefix, false); err != nil {
		t.Fatal(err)
	}
	if l, err := in.heldLogin(e.prefix); err != nil || l == nil || l.Password != loginPassword || reads() != 2 {
		t.Errorf("the first prefix again: %+v %v, %d reads", l, err, reads())
	}
	if _, err := in.heldLogin(e.prefix + "/other"); err == nil || !strings.Contains(err.Error(), "before it takes the pull Secret's login") {
		t.Errorf("a prefix not taken: %v", err)
	}
}

// The child's descriptors: the login childLogin hands is at the path it gives the child, and the
// lock handed is at the number the child's environment names, whatever their order.
func TestTheChildsDescriptorsAreTheOnesItIsTold(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lock, err := os.Create(filepath.Join(dir, "lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	in := &Installer{k8sLogin: &registryLogin{Host: "ghcr.io", User: "inst-1", Password: loginPassword}}
	in.O.handLock = lock
	path, done, err := in.childLogin()
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	cmd := in.O.childCmd(context.Background(), "child", []string{path}, io.Discard)
	at := func(fd string) *os.File {
		t.Helper()
		n, err := strconv.Atoi(fd)
		if err != nil || n < 3 || n-3 >= len(cmd.ExtraFiles) {
			t.Fatalf("descriptor %q of %d", fd, len(cmd.ExtraFiles))
		}
		return cmd.ExtraFiles[n-3]
	}
	if fd, ok := strings.CutPrefix(path, "/dev/fd/"); !ok || at(fd) != in.O.childFiles[0] {
		t.Errorf("the login's path %s names another descriptor", path)
	}
	handed := ""
	for _, kv := range cmd.Env {
		if v, ok := strings.CutPrefix(kv, flock.HandedEnv+"="); ok {
			handed = v
		}
	}
	if at(handed) != lock {
		t.Errorf("the lock's descriptor %s is another file", handed)
	}
}

// reset-db across a baseline keeping the login a run gave for the images' host names it where
// the release named none (a run before dropped it), and leaves another Secret the values name.
func TestK8sResetAcrossABaselineNamesTheKeptLogin(t *testing.T) {
	t.Parallel()
	for named, want := range map[string]string{"": "notes-stowage-pull", "my-registry": "my-registry"} {
		e := newKubeEnv(t, spec.Provider{Name: "none"})
		e.publishAt("1.0.0", 1)
		e.runs("1.0.0", 1)
		e.f.values["stowage"].(map[string]any)["pullSecret"] = named
		e.release = e.at("2.0.0", 1, atBaseline(2))
		e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
		e.f.manifest = resetManifest
		e.f.claims = []string{"data-notes-postgres-0"}
		e.f.pull = heldPullSecret(registryHost(e.prefix), "inst-1", loginPassword, true)
		if err := e.reset(func(o *Options) { o.Yes, o.NewBaseline = true, true }); err != nil {
			t.Fatalf("%q: %v\n%s", named, err, e.log)
		}
		if len(e.f.given) == 0 || !strings.Contains(e.f.given[len(e.f.given)-1], "pullSecret: "+want+"\n") {
			t.Errorf("%q: %q\n%s", named, e.f.given, e.log)
		}
	}
}

// An upgrade keeping the login a run gave, whose release names another pull Secret that is gone,
// names the installer's Secret holding that login rather than none.
func TestK8sKeptLoginReplacesAGoneOperatorsPullSecret(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("0.9.0", 1)
	e.f.values["stowage"].(map[string]any)["pullSecret"] = "my-registry"
	e.f.objects = map[string]string{"secret/my-registry": ""} // gone
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
	e.f.pull = heldPullSecret(registryHost(e.prefix), "inst-1", loginPassword, true)
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if len(e.f.given) == 0 || !strings.Contains(e.f.given[0], "pullSecret: notes-stowage-pull\n") || !strings.Contains(e.log.String(), "the pull Secret my-registry the release names is gone: the pods name notes-stowage-pull") {
		t.Errorf("values %q\n%s", e.f.given, e.log)
	}
}
