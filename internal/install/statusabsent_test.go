package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/norvexlabs/stowage/internal/helm"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/spf13/cobra"
)

// oneStatusRefused decodes the one StatusRefused out holds, strictly.
func oneStatusRefused(t *testing.T, out string) StatusRefused {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	var r StatusRefused
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("%v: %q", err, out)
	}
	if dec.More() || r.Format != MachineFormat {
		t.Fatalf("not one object of format %d: %q", MachineFormat, out)
	}
	return r
}

// status --json on a refusal prints it, absent telling "no install here" from another refusal; the
// verbs' Result carries absent the same way. Both sides, on Docker and on Kubernetes.
func TestStatusJSONTellsAbsentFromAnother(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	tools := K8sOptions{Helm: kubeTool{e.f, "helm"}, Kubectl: kubeTool{e.f, "kubectl"}}
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		cmd := newCommand(e.release, "notes-stow", &out, io.Discard, tools, func(bool) Prompter { return nil })
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(context.Background())
		return out.String(), err
	}
	// Docker: an empty directory is absent.
	empty := t.TempDir()
	out, err := run("status", "--json", "--dir", empty)
	if !isRefused(err) {
		t.Fatalf("%v", err)
	}
	if r := oneStatusRefused(t, out); !r.Refused.Absent || !strings.HasSuffix(r.Refused.Reason, "holds no install") || r.Refused.Fix == "" {
		t.Errorf("%+v", r)
	}
	// The verbs: absent on the empty directory, not on another project's install.
	for _, args := range [][]string{{"uninstall", "--json", "--yes"}, {"upgrade", "--json", "--no-input"}, {"rollback", "--json"}} {
		out, err = run(append(args, "--dir", empty)...)
		if r := oneResult(t, out); !isRefused(err) || r.Refused == nil || !r.Refused.Absent {
			t.Errorf("%v: %v %+v", args, err, r.Refused)
		}
	}
	other := t.TempDir()
	_ = os.WriteFile(filepath.Join(other, envName), []byte("STOWAGE_NAME='atlas'\nSTOWAGE_PROJECT_VERSION='3.0.0'\nSTOWAGE_REVISION='2'\n"), 0o600)
	out, err = run("uninstall", "--json", "--dir", other)
	if r := oneResult(t, out); !isRefused(err) || r.Refused == nil || r.Refused.Absent || strings.Contains(out, `"absent"`) {
		t.Errorf("%v %s", err, out)
	}
	// Kubernetes: no release is absent; a release not installed from Stowage's chart is another
	// refusal, printed too.
	out, err = run("k8s", "status", "--json")
	if r := oneStatusRefused(t, out); !isRefused(err) || !r.Refused.Absent || r.Refused.Reason != "no release notes in notes: `notes-stow k8s install --namespace notes --release notes` installs 1.0.0" {
		t.Errorf("%v %+v", err, r)
	}
	// Every k8s verb finding no release says absent, as Docker's verbs on an empty directory.
	for _, args := range [][]string{{"k8s", "upgrade", "--json", "--no-input"}, {"k8s", "rollback", "--json"}} {
		out, err := run(args...)
		if r := oneResult(t, out); !isRefused(err) || r.Refused == nil || !r.Refused.Absent || !strings.Contains(r.Refused.Reason, "no release notes in notes") {
			t.Errorf("%v: %v %s", args, err, out)
		}
	}
	out, err = run("k8s", "upgrade", "--version", "2.0.0", "--json", "--no-input")
	if r := oneResult(t, out); !isRefused(err) || r.Refused == nil || !r.Refused.Absent || !strings.Contains(r.Refused.Reason, "no release notes in notes") {
		t.Errorf("k8s upgrade --version: %v %s", err, out)
	}
	e.runs("1.0.0", 1)
	e.f.values = map[string]any{"image": "x"}
	out, err = run("k8s", "status", "--json")
	if r := oneStatusRefused(t, out); !isRefused(err) || r.Refused.Absent || !strings.Contains(r.Refused.Reason, "was not installed from a chart Stowage published") {
		t.Errorf("%v %+v", err, r)
	}
}

// A failure is no refusal: status --json prints nothing for it.
func TestStatusRefusedPrintsNothingOnAFailure(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	fail := errors.New("helm: exit status 1")
	if err := printStatusRefused(&out, fail); err != fail || out.Len() != 0 {
		t.Errorf("%v %q", err, out.String())
	}
	if err := printStatusRefused(&out, changed(refuse("x", "after the change"))); err == nil || out.Len() != 0 {
		t.Errorf("a refusal after a change printed %q", out.String())
	}
	// A standard output that cannot be written leaves the refusal one: exit 3.
	if err := printStatusRefused(failingWriter{}, refuse("x", "no install")); !isRefused(err) {
		t.Errorf("the print's failure replaced the refusal: %v", err)
	}
}

// What a Docker install keeps from its first run: the installer's short name, the loopback
// address, the registry prefix, and the digest of the registry CA, only when it keeps one.
func TestDockerStatusJSONFirstInstallSettings(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(func(o *Options) { o.Address = "127.0.0.9" }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	st, out := statusOf(t, e, nil)
	if st.ShortName != "notes" || st.Address != "127.0.0.9" || st.Registry != "ghcr.io/acme/notes" || st.RegistryCA != "" || strings.Contains(out, `"registry_ca"`) {
		t.Errorf("%+v\n%s", st, out)
	}
	ca := []byte("-----BEGIN CERTIFICATE-----\nthe install's copy\n-----END CERTIFICATE-----\n")
	if err := os.WriteFile(filepath.Join(e.dir, RegistryCAFile), ca, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(ca)
	if st, _ = statusOf(t, e, nil); st.RegistryCA != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Errorf("registry_ca %q", st.RegistryCA)
	}
}

// Kubernetes reports the registry its release's values record.
func TestK8sStatusJSONRegistry(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	e.f.history = []helmRevision{{Revision: 1, Status: "deployed", Chart: "notes-0.1.0"}}
	e.stored(map[string]string{"ADMIN_EMAIL": "admin@example.org"}, ours)
	if err := e.installer(nil).K8sStatusJSON(context.Background()); err != nil {
		t.Fatal(err)
	}
	var st K8sReleaseStatus
	if err := json.Unmarshal(e.out.Bytes(), &st); err != nil || st.Registry != e.prefix || e.prefix == "" {
		t.Errorf("%v %q\n%s", err, st.Registry, e.out)
	}
}

func refusedAbsent(err error) bool { r, _ := Refusal(err); return r.Absent }

// Every Docker refusal of a missing install names the directory as an absolute path, a relative
// --dir included.
func TestNoInstallNamesTheAbsoluteDir(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	in := e.installer(func(o *Options) { o.Dir = "no-such-install-here" })
	_, err := in.ExportSecrets([]string{"age1x"})
	if r, ok := Refusal(err); !ok || !strings.HasPrefix(r.Reason, "/") || !strings.HasSuffix(r.Reason, "/no-such-install-here holds no install") {
		t.Errorf("%v", err)
	}
}

// On the VM, whose host daemon answers its verbs, a directory with no install is an ordinary
// refusal: absent is Docker's and Kubernetes'.
func TestTheVMsNoInstallIsNotAbsent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, preloaded := range []bool{false, true} {
		in := e.installer(func(o *Options) { o.Preloaded = preloaded })
		_, exportErr := in.ExportSecrets([]string{"age1x"})
		_, openErr := in.open()
		// Every verb, the text status and the secrets export refuse it one way.
		for _, err := range []error{openErr, in.Status(context.Background()), exportErr} {
			want := "install first, or give the --dir of the install"
			if preloaded {
				want = "sudo notes-stow install"
			}
			if r, ok := Refusal(err); !ok || r.Absent == preloaded || r.Fix != want {
				t.Errorf("preloaded %v: %v %+v", preloaded, err, r)
			}
		}
	}
}

// A registry CA the install keeps but that cannot be read fails status --json (exit 1, nothing
// printed): it is never reported absent.
func TestDockerStatusJSONFailsOnAnUnreadableRegistryCA(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if err := os.Mkdir(filepath.Join(e.dir, RegistryCAFile), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := e.installer(func(o *Options) { o.Out = &out }).StatusJSON(context.Background())
	if err == nil || isRefused(err) || out.Len() != 0 {
		t.Errorf("%v %q", err, out.String())
	}
}

// describe's text names each route, its loopback port when the spec gives one, each L4 route and
// the registry.
func TestDescribeTextRoutes(t *testing.T) {
	t.Parallel()
	d := Description{Name: "notes", Registry: "ghcr.io/acme/notes",
		Routes:   []DescribedRoute{{Name: "app", LoopbackPort: 8080}, {Name: "bare"}},
		L4Routes: []DescribedL4Route{{Name: "db", Protocol: "tcp", Port: 5432}}}
	var b bytes.Buffer
	if err := d.print(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\nroute     app (loopback port 8080)\n", "\nroute     bare\n", "\nl4 route  db (tcp/5432)\n", "\nregistry  ghcr.io/acme/notes\nfirst install only: "} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %q in:\n%s", want, b.String())
		}
	}
}

// dropInputsRecord removes InputsVar from the install's .env, as an install made before it was
// recorded holds it.
func dropInputsRecord(t *testing.T, e *env) {
	t.Helper()
	path := filepath.Join(e.dir, envName)
	env, err := ReadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	next := NewEnvFile()
	for _, k := range env.Keys() {
		if k != InputsVar {
			v, _ := env.Get(k)
			next.Set(k, v)
		}
	}
	if err := next.WriteFile(path); err != nil {
		t.Fatal(err)
	}
}

// An input only a newer release declares stays listed after the rollback that keeps its value:
// the install records its input keys, whichever release declared them. A key no release declares
// is never listed.
func TestDockerStatusJSONInputsOfEveryRelease(t *testing.T) {
	t.Parallel()
	e := installed(t)
	e.to("2.0.0", func(s *spec.Spec) {
		// WORKERS, advanced and not given, is declared but not recorded: never listed.
		s.Inputs = append(s.Inputs, spec.Input{Key: "SMTP_HOST", Default: "smtp.example.org"}, spec.Input{Key: "WORKERS", Advanced: true})
	})
	if err := e.upgrade(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if got := get(e.readEnv(), InputsVar); got != "ADMIN_EMAIL,SMTP_HOST" {
		t.Fatalf("the upgrade recorded %q", got)
	}
	if err := e.rollback(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	env := e.readEnv()
	if get(env, "SMTP_HOST") != "smtp.example.org" || get(env, InputsVar) != "ADMIN_EMAIL,SMTP_HOST" {
		t.Fatalf("the rollback kept %q, recorded %q", get(env, "SMTP_HOST"), get(env, InputsVar))
	}
	st, out := statusOf(t, e, nil)
	if st.Installed.Version != "1.0.0" || st.Inputs == nil || !slices.Equal(*st.Inputs, []string{"ADMIN_EMAIL", "SMTP_HOST"}) {
		t.Errorf("%s", out)
	}
	if slices.Contains(*st.Inputs, "SESSION_SECRET") || slices.Contains(*st.Inputs, "STOWAGE_NAME") {
		t.Errorf("a key no release declares as an input: %v", *st.Inputs)
	}
}

// --version names the release the installer carries and the Stowage release that built it, on
// the Docker installer and on the VM's.
func TestInstallerVersion(t *testing.T) {
	t.Parallel()
	r, self := testInstaller(t, "1.0.0")
	r.Manifest.StowageVersion = "0.35.0"
	for _, cmd := range []func(io.Writer) *cobra.Command{
		func(w io.Writer) *cobra.Command { return Command(r, self, w, io.Discard) },
		func(w io.Writer) *cobra.Command {
			return vmCommand(r, self, w, io.Discard, func(bool) Prompter { return nil }, clientTimes{})
		},
	} {
		var out bytes.Buffer
		c := cmd(&out)
		c.SetArgs([]string{"--version"})
		if err := c.Execute(); err != nil || out.String() != "notes-stow version 1.0.0 (revision 1), stowage 0.35.0\n" {
			t.Errorf("%v %q", err, out.String())
		}
	}
	if got := Describe(r).StowageVersion; got != "0.35.0" {
		t.Errorf("describe: %q", got)
	}
	r.Manifest.StowageVersion = ""
	if got := r.versionLine(); got != "1.0.0 (revision 1)" {
		t.Errorf("none recorded: %q", got)
	}
}

// mirrored pushes the chart under the mirror prefix e.prefix/mirror, as a mirror holds it.
func mirrored(t *testing.T, e *kubeEnv) string {
	t.Helper()
	mirror := e.prefix + "/mirror"
	c := registry.New(authn.NewMultiKeychain())
	repo, err := c.Repo(mirror + "/charts/notes-stable")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PutManifest(context.Background(), repo, registry.Manifest{
		Config: registry.Blob{MediaType: helm.ConfigMediaType, Data: []byte(`{"name":"notes"}`)},
		Layers: []registry.Blob{{MediaType: helm.ContentMediaType, Data: e.chart}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	return mirror
}

// dockerLogin is a Docker client's login holding cfg for every registry.
type dockerLogin authn.AuthConfig

func (l dockerLogin) Resolve(authn.Resource) (authn.Authenticator, error) {
	if l == (dockerLogin{}) {
		return authn.Anonymous, nil
	}
	return authn.FromConfig(authn.AuthConfig(l)), nil
}

// An input the upgrade dropped comes back with the rollback, listed again: the rolled-back
// release's record counts.
func TestRollbackListsAnInputTheUpgradeDropped(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.r, e.self = testInstallerWith(t, "1.0.0", func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "OLD_HOST", Default: "old.example.org"})
	})
	if err := e.install(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	e.to("2.0.0", nil) // declares no OLD_HOST: the upgrade drops it
	if err := e.upgrade(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if env := e.readEnv(); get(env, "OLD_HOST") != "" || get(env, InputsVar) != "ADMIN_EMAIL" {
		t.Fatalf("upgraded: %q %q", get(env, "OLD_HOST"), get(env, InputsVar))
	}
	if err := e.rollback(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if env := e.readEnv(); get(env, "OLD_HOST") != "old.example.org" || get(env, InputsVar) != "ADMIN_EMAIL,OLD_HOST" {
		t.Errorf("rolled back: %q %q", get(env, "OLD_HOST"), get(env, InputsVar))
	}
}

// A dry run prints the input keys the run would record.
func TestDryRunShowsTheInputsRecord(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(func(o *Options) { o.DryRun = true }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if !strings.Contains(e.out.String(), InputsVar+"=ADMIN_EMAIL\n") {
		t.Errorf("%s", e.out)
	}
}

// A rollback to a release whose record lists no inputs (kept before the list was recorded) lists
// that release's own inputs, as its spec declares them.
func TestRollbackListsTheRolledBackReleasesOwnInputs(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.r, e.self = testInstallerWith(t, "1.0.0", func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "OLD_HOST", Default: "old.example.org"})
	})
	if err := e.install(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	dropInputsRecord(t, e)
	e.to("2.0.0", nil)
	if err := e.upgrade(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if err := e.rollback(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if env := e.readEnv(); get(env, InputsVar) != "ADMIN_EMAIL,OLD_HOST" {
		t.Errorf("rolled back: %q", get(env, InputsVar))
	}
}

// --image-pull-login writes the cluster's pull Secret for the mirror's host from the Docker
// client's login, which the installer read the mirror with; a login it cannot write one from is
// refused before the cluster changes. Without the flag none is written, whatever the login.
func TestK8sImagePullLogin(t *testing.T) {
	t.Parallel()
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, c := range []struct {
		name   string
		login  dockerLogin
		secret string // the pull Secret's auth for the host
		refuse string
	}{
		{"user and password", dockerLogin{Username: "mirror-pull", Password: "pw"}, "mirror-pull:pw", ""},
		{"auth alone", dockerLogin{Auth: b64("u:p")}, "u:p", ""},
		{"a user and its auth", dockerLogin{Username: "u", Auth: b64("u:p")}, "u:p", ""},
		{"a password and its auth", dockerLogin{Password: "p", Auth: b64("u:p")}, "u:p", ""},
		{"none", dockerLogin{}, "", "--image-pull-login: no Docker login for "},
		{"a token alone", dockerLogin{IdentityToken: "tok"}, "", "holds no user and password a cluster's nodes pull with"},
		{"a user alone", dockerLogin{Username: "u"}, "", "holds no user and password a cluster's nodes pull with"},
		{"a password alone", dockerLogin{Password: "pat"}, "", "holds no user and password a cluster's nodes pull with"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newKubeEnv(t, spec.Provider{Name: "none"})
			mirror := mirrored(t, e)
			err := e.installer(func(o *Options) { o.K8s.Registry, o.K8s.PullLogin, o.Login = mirror, true, c.login }).K8sInstall(context.Background())
			if c.refuse != "" {
				if !isRefused(err) || !strings.Contains(err.Error(), c.refuse) || len(e.f.applied) != 0 {
					t.Fatalf("%v %v", err, e.f.applied)
				}
				return
			}
			if err != nil {
				t.Fatalf("%v\n%s", err, e.log)
			}
			var cfg struct {
				Auths map[string]struct{ Auth string }
			}
			if err := json.Unmarshal([]byte(e.secretData("notes-stowage-pull")[".dockerconfigjson"]), &cfg); err != nil {
				t.Fatal(err)
			}
			host, _, _ := strings.Cut(mirror, "/")
			if len(cfg.Auths) != 1 || cfg.Auths[host].Auth != b64(c.secret) || !strings.Contains(e.f.given[0], "pullSecret: notes-stowage-pull\n") ||
				!strings.Contains(e.log.String(), "note: the pull Secret notes-stowage-pull holds the Docker login for") {
				t.Errorf("%+v %q\n%s", cfg, e.f.given[0], e.log)
			}
		})
	}
	// Without the flag: no Secret from the login, even one holding a user and password.
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	mirror := mirrored(t, e)
	if err := e.installer(func(o *Options) { o.K8s.Registry, o.Login = mirror, dockerLogin{Username: "u", Password: "p"} }).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if e.secretData("notes-stowage-pull") != nil || !strings.Contains(e.f.given[0], "pullSecret: \"\"\n") {
		t.Errorf("not asked: %q", e.f.given[0])
	}
	// On the host of the release's own registry, whose pull account (token) the installer carries:
	// refused before the cluster changes, a mirror's path on that host included.
	token := spec.Provider{Name: "token", Config: map[string]any{"user": "notes-pull", "token_env": "NOTES_PULL"}}
	for _, mirror := range []bool{false, true} {
		e = newKubeEnv(t, token)
		e.vars["NOTES_PULL"] = "t"
		err := e.installer(func(o *Options) {
			o.K8s.PullLogin, o.Login = true, dockerLogin{Username: "me", Password: "personal"}
			if mirror {
				o.K8s.Registry = mirrored(t, e)
			}
		}).K8sInstall(context.Background())
		if !isRefused(err) || !strings.Contains(err.Error(), "the host of the release's own registry") || len(e.f.applied) != 0 {
			t.Errorf("own registry's host (mirror %v): %v", mirror, err)
		}
	}
	// A registry read with the Docker login (docker-config, none) takes it.
	e = newKubeEnv(t, spec.Provider{Name: "none"})
	if err := e.installer(func(o *Options) { o.K8s.PullLogin, o.Login = true, dockerLogin{Username: "u", Password: "p"} }).K8sInstall(context.Background()); err != nil || e.secretData("notes-stowage-pull") == nil {
		t.Errorf("own registry without a pull account: %v", err)
	}
	// A login whose auth is not base64 fails the run before the cluster changes.
	e = newKubeEnv(t, spec.Provider{Name: "none"})
	mirror = mirrored(t, e)
	err := e.installer(func(o *Options) { o.K8s.Registry, o.K8s.PullLogin, o.Login = mirror, true, dockerLogin{Auth: "%%%"} }).K8sInstall(context.Background())
	if err == nil || isRefused(err) || !strings.Contains(err.Error(), "its auth is not base64") || len(e.f.applied) != 0 {
		t.Errorf("unreadable: %v", err)
	}
}

// An upgrade writes the pull Secret from the login when asked, from the mirror given or the one
// the release records; without the flag it keeps the Secret the release names (helm reuses it).
func TestK8sUpgradeImagePullLogin(t *testing.T) {
	t.Parallel()
	pull := dockerLogin{Username: "mirror-pull", Password: "pw"}
	for _, given := range []bool{true, false} {
		e := newKubeEnv(t, spec.Provider{Name: "none"})
		mirror := mirrored(t, e)
		e.runs("0.9.0", 1)
		e.f.values["stowage"].(map[string]any)["registry"] = mirror
		e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
		if err := e.installer(func(o *Options) {
			o.K8s.PullLogin, o.Login = true, pull
			if given {
				o.K8s.Registry = mirror
			}
		}).K8sUpgrade(context.Background()); err != nil {
			t.Fatalf("%v\n%s", err, e.log)
		}
		if e.secretData("notes-stowage-pull") == nil || !strings.Contains(e.f.given[0], "pullSecret: notes-stowage-pull\n") {
			t.Errorf("asked, --image-registry given %v: %q", given, e.f.given[0])
		}
	}
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	mirror := mirrored(t, e)
	e.runs("0.9.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
	if err := e.installer(func(o *Options) { o.K8s.Registry, o.Login = mirror, pull }).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if e.secretData("notes-stowage-pull") != nil || strings.Contains(e.f.given[0], "pullSecret") {
		t.Errorf("not asked: %q", e.f.given[0])
	}
}

// An upgrade's dry run prints the change of the input keys the run would record.
func TestUpgradeDryRunShowsTheInputsRecord(t *testing.T) {
	t.Parallel()
	e := installed(t)
	e.to("2.0.0", func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "SMTP_HOST", Default: "smtp.example.org"})
	})
	e.out.Reset()
	if err := e.upgrade(func(o *Options) { o.DryRun = true }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if !strings.Contains(e.out.String()+e.log.String(), InputsVar) {
		t.Errorf("out %s\nlog %s", e.out, e.log)
	}
}

// k8s upgrade --version of the release that runs applies nothing, --image-pull-login included,
// and says so.
func TestK8sUpgradeToTheRunningReleaseSaysThePullLoginIsNotApplied(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org"}, ours)
	if err := e.installer(func(o *Options) { o.K8s.PullLogin = true }).K8sUpgradeTo(context.Background(), "1.0.0", nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if !strings.Contains(e.out.String(), "--image-pull-login is not applied: an upgrade to the release that runs changes nothing") {
		t.Errorf("%s", e.out)
	}
}

// askedPrompter fails the test when asked: a refusal that must come before any input.
type askedPrompter struct{ t *testing.T }

func (p askedPrompter) Ask(context.Context, Form) (map[string]string, error) {
	p.t.Error("asked an input before the pull checks")
	return nil, errors.New("asked")
}

// The pull checks run before any input is asked: a refusal costs the operator no answer.
func TestK8sImagePullLoginRefusesBeforeAsking(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	mirror := mirrored(t, e)
	delete(e.vars, "ADMIN_EMAIL") // the form would ask it
	err := e.installer(func(o *Options) {
		o.K8s.Registry, o.K8s.PullLogin, o.Login, o.Prompt = mirror, true, dockerLogin{}, askedPrompter{t}
	}).K8sInstall(context.Background())
	if !isRefused(err) || !strings.Contains(err.Error(), "no Docker login for") {
		t.Errorf("%v", err)
	}
}

// A dry run reads the login and notes the Secret a run writes, writing nothing.
func TestK8sImagePullLoginDryRun(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	mirror := mirrored(t, e)
	if err := e.installer(func(o *Options) {
		o.K8s.Registry, o.K8s.PullLogin, o.Login, o.DryRun = mirror, true, dockerLogin{Username: "u", Password: "p"}, true
	}).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if !strings.Contains(e.log.String(), "note: a run writes the pull Secret notes-stowage-pull from the Docker login for") || len(e.f.applied) != 0 {
		t.Errorf("%s %v", e.log, e.f.applied)
	}
}
