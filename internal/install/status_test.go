package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/spec"
)

// statusOf runs the Docker `status --json` on e's install and decodes it, strictly.
func statusOf(t *testing.T, e *env, mod func(*Options)) (DockerStatus, string) {
	t.Helper()
	var out bytes.Buffer
	if err := e.installer(func(o *Options) {
		o.Out = &out
		if mod != nil {
			mod(o)
		}
	}).StatusJSON(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	var st DockerStatus
	d := json.NewDecoder(bytes.NewReader(out.Bytes()))
	d.DisallowUnknownFields()
	if err := d.Decode(&st); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	return st, out.String()
}

// The install's status for a program: the VM's names, the installer image the install recorded,
// the services, and the inputs' keys — never a value recorded, an input's or a secret's.
func TestDockerStatusJSON(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.r, e.self = testInstallerWith(t, "1.0.0", func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "ACCESS_TOKEN", Secret: true}, spec.Input{Key: "WORKERS", Advanced: true})
	})
	e.vars["ACCESS_TOKEN"] = "tok-not-to-be-printed"
	if err := e.install(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if strings.HasSuffix(a, " ps --all --format json") {
			return []byte(`{"ID":"c1","Service":"api","State":"running","Health":"healthy"}` + "\n"), true, nil
		}
		return nil, false, nil
	}
	st, out := statusOf(t, e, nil)
	ctl := testCtl("ghcr.io/acme/notes", e.r.Manifest)
	if !strings.HasPrefix(out, "{\n  \"format\": 1,\n") {
		t.Errorf("format first: %s", out)
	}
	if st.Name != "notes" || *st.Installed != (StatusRelease{"1.0.0", 1, ctl}) || st.Channel != "stable" || st.Installing || st.InstallFailed != "" ||
		st.Previous != nil || st.Inputs == nil || !slices.Equal(*st.Inputs, []string{"ACCESS_TOKEN", "ADMIN_EMAIL"}) {
		t.Errorf("%+v\n%s", st, out)
	}
	if len(st.Services) != 1 || st.Services[0].Service != "api" || st.Services[0].Health != "healthy" || st.ServicesError != "" {
		t.Errorf("services %+v", st.Services)
	}
	if !strings.Contains(out, `"previous": null`) {
		t.Errorf("no previous: %s", out)
	}
	env := e.readEnv()
	for _, k := range []string{"ADMIN_EMAIL", "ACCESS_TOKEN", "SESSION_SECRET", "POSTGRES_PASSWORD"} {
		if v := get(env, k); v == "" || strings.Contains(out, v) {
			t.Errorf("%s=%q is printed, or not recorded:\n%s", k, v, out)
		}
	}
	if get(env, InstallerVar) != ctl {
		t.Errorf(".env records %q", get(env, InstallerVar))
	}
}

// Each release's installer image follows it: the upgrade records the new one's and keeps the
// previous one's with it, the rollback takes it back.
func TestDockerStatusJSONAcrossUpgradeAndRollback(t *testing.T) {
	t.Parallel()
	e := installed(t)
	v1 := e.r.Manifest
	e.to("2.0.0", nil)
	if err := e.upgrade(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	st, out := statusOf(t, e, nil)
	if *st.Installed != (StatusRelease{"2.0.0", 1, testCtl("ghcr.io/acme/notes", e.r.Manifest)}) || st.Previous == nil ||
		*st.Previous != (StatusRelease{"1.0.0", 1, testCtl("ghcr.io/acme/notes", v1)}) {
		t.Fatalf("%s", out)
	}
	// An image recorded for another release than the kept record's (carried over by an installer
	// that does not record it) names no installer of it; nor of the installed one.
	prev := filepath.Join(e.dir, PrevDir, envName)
	kept, err := ReadEnvFile(prev)
	if err != nil {
		t.Fatal(err)
	}
	kept.Set(InstallerOfVar, "1.0.0/2")
	if err := kept.WriteFile(prev); err != nil {
		t.Fatal(err)
	}
	if st, out = statusOf(t, e, nil); st.Previous == nil || *st.Previous != (StatusRelease{"1.0.0", 1, ""}) {
		t.Fatalf("%s", out)
	}
	kept.Set(InstallerOfVar, "1.0.0/1")
	if err := kept.WriteFile(prev); err != nil {
		t.Fatal(err)
	}
	cur := e.readEnv()
	if get(cur, InstallerOfVar) != "2.0.0/1" {
		t.Fatalf("recorded for %q", get(cur, InstallerOfVar))
	}
	for _, of := range []string{"1.0.0/1", ""} {
		stale := e.readEnv()
		if of == "" {
			stale.Delete(InstallerOfVar)
		} else {
			stale.Set(InstallerOfVar, of)
		}
		if err := stale.WriteFile(filepath.Join(e.dir, envName)); err != nil {
			t.Fatal(err)
		}
		if st, out = statusOf(t, e, nil); st.Installed.Installer != "" {
			t.Fatalf("recorded for %q: %s", of, out)
		}
	}
	if err := cur.WriteFile(filepath.Join(e.dir, envName)); err != nil {
		t.Fatal(err)
	}
	if err := e.rollback(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	st, out = statusOf(t, e, nil)
	if *st.Installed != (StatusRelease{"1.0.0", 1, testCtl("ghcr.io/acme/notes", v1)}) || st.Previous != nil {
		t.Fatalf("%s", out)
	}
}

// A release whose installer image is not read records none: never the one of the release it
// replaced.
func TestDockerStatusJSONNeverGuessesTheInstaller(t *testing.T) {
	t.Parallel()
	e := installed(t)
	e.to("2.0.0", nil)
	unread := func(o *Options) {
		o.InstallerImage = func(context.Context, string) (string, error) { return "", errors.New("no release manifest") }
	}
	if err := e.upgrade(unread); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if _, ok := e.readEnv().Get(InstallerVar); ok {
		t.Error("the release replaced's installer is recorded")
	}
	if !strings.Contains(e.log.String(), "the release's installer image is not recorded (status --json leaves it out): no release manifest") {
		t.Errorf("log %s", e.log)
	}
	st, out := statusOf(t, e, nil)
	if *st.Installed != (StatusRelease{"2.0.0", 1, ""}) || strings.Contains(out, `"installer": ""`) || st.Previous == nil || st.Previous.Installer == "" {
		t.Errorf("%s", out)
	}
}

// An install that never pulls reads no registry, and records no installer image.
func TestDockerStatusJSONOffline(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, im := range []string{"front", "chown", "api", "web", "postgres"} {
		e.docker.loaded["ghcr.io/acme/notes/notes-"+im+":1.0.0"] = manifest
	}
	if err := e.install(func(o *Options) {
		o.Offline = true
		o.InstallerImage = func(context.Context, string) (string, error) {
			t.Error("the registry is read")
			return "", nil
		}
	}); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if st, out := statusOf(t, e, nil); st.Installed.Installer != "" {
		t.Errorf("%s", out)
	}
}

// Only an install that pulls by digest reads its installer image: never a VM's, nor one whose
// images are loaded by tag, nor one with no registry.
func TestRecordInstallerReadsOnlyWhatPulls(t *testing.T) {
	t.Parallel()
	pinned := func(prefix, image string) *EnvFile {
		env := NewEnvFile()
		env.Set("STOWAGE_REGISTRY", prefix)
		env.Set(ImageVar("api"), image)
		env.Set(InstallerVar, "stale")
		return env
	}
	for _, c := range []struct {
		name  string
		o     Options
		env   *EnvFile
		reads bool
	}{
		{"pinned", Options{}, pinned("ghcr.io/acme/notes", "ghcr.io/acme/notes/notes-api@sha256:1111"), true},
		{"offline", Options{Offline: true}, pinned("ghcr.io/acme/notes", "ghcr.io/acme/notes/notes-api@sha256:1111"), false},
		{"preloaded", Options{Preloaded: true}, pinned("ghcr.io/acme/notes", "ghcr.io/acme/notes/notes-api@sha256:1111"), false},
		{"by tag", Options{}, pinned("ghcr.io/acme/notes", "ghcr.io/acme/notes/notes-api:1.0.0"), false},
		{"no registry", Options{}, pinned("", "ghcr.io/acme/notes/notes-api@sha256:1111"), false},
	} {
		read := false
		c.o.Log = &bytes.Buffer{}
		c.o.InstallerImage = func(_ context.Context, prefix string) (string, error) {
			read = true
			return prefix + "/notes-installer@sha256:2222", nil
		}
		in := &Installer{R: testRelease(t, "1.0.0"), O: c.o}
		in.recordInstaller(context.Background(), c.env)
		got, ok := c.env.Get(InstallerVar)
		if read != c.reads || ok != c.reads || c.reads && got != "ghcr.io/acme/notes/notes-installer@sha256:2222" {
			t.Errorf("%s: read %v, recorded %q (%v)", c.name, read, got, ok)
		}
	}
}

// What a program must be told: a first install that failed, services that do not read, a record
// that does not read.
func TestDockerStatusJSONStates(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	e.installer(nil).markInstall(errors.New("the hook failed\nmore"))
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if strings.HasSuffix(a, "ps --all --format json") {
			return nil, true, errors.New("engine restarting")
		}
		return nil, false, nil
	}
	st, out := statusOf(t, e, nil)
	if !st.Installing || st.InstallFailed != "the hook failed" || st.Services != nil || st.ServicesError != "compose ps: engine restarting" {
		t.Errorf("%s", out)
	}
	if err := os.MkdirAll(filepath.Join(e.dir, PrevDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, PrevDir, prevState), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.installer(nil).StatusJSON(context.Background()); err == nil || !strings.Contains(err.Error(), filepath.Join(PrevDir, prevState)+": unexpected end of JSON input") {
		t.Errorf("%v", err)
	}
	env := e.readEnv()
	env.Set("STOWAGE_REVISION", "one")
	if err := env.WriteFile(filepath.Join(e.dir, envName)); err != nil {
		t.Fatal(err)
	}
	if err := e.installer(nil).StatusJSON(context.Background()); err == nil || !strings.Contains(err.Error(), `STOWAGE_REVISION "one" is not a number`) {
		t.Errorf("%v", err)
	}
	none := e.installer(func(o *Options) { o.Dir = filepath.Join(t.TempDir(), "none") })
	if err := none.StatusJSON(context.Background()); err == nil || !strings.Contains(err.Error(), "holds no install") {
		t.Errorf("%v", err)
	}
}

// k8sStatusOf runs `k8s status --json` and decodes it, strictly.
func k8sStatusOf(t *testing.T, e *kubeEnv) (K8sReleaseStatus, string) {
	t.Helper()
	e.out.Reset()
	if err := e.installer(nil).K8sStatusJSON(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	var st K8sReleaseStatus
	d := json.NewDecoder(bytes.NewReader(e.out.Bytes()))
	d.DisallowUnknownFields()
	if err := d.Decode(&st); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	return st, e.out.String()
}

// The release's status for a program: the installer image its values record, helm's revision
// and status, the inputs its Secret records — keys only — and the release a rollback goes back
// to, as that revision's values record it.
func TestK8sStatusJSON(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	ctl := e.prefix + "/notes-installer@" + ctlDigest
	rec := e.f.values["stowage"].(map[string]any)["release"].(map[string]any)
	rec["installer"] = ctl
	rec["installerRelease"] = "1.0.0/1"
	e.f.history = []helmRevision{{Revision: 1, Status: "deployed", Chart: "notes-0.1.0"}}
	e.stored(map[string]string{"ADMIN_EMAIL": "admin@example.org", "SESSION_SECRET": "s3cret-not-printed", "LEFT_OVER": "x"}, ours)
	st, out := k8sStatusOf(t, e)
	if !strings.HasPrefix(out, "{\n  \"format\": 1,\n") {
		t.Errorf("format first: %s", out)
	}
	if st.Name != "notes" || *st.Installed != (StatusRelease{"1.0.0", 1, ctl}) || st.Channel != "stable" || st.Installing ||
		st.Previous != nil || st.Inputs == nil || !slices.Equal(*st.Inputs, []string{"ADMIN_EMAIL"}) || st.Helm != (HelmStatus{1, "deployed"}) {
		t.Errorf("%s", out)
	}
	if strings.Contains(out, "admin@example.org") || strings.Contains(out, "s3cret") || !strings.Contains(out, `"previous": null`) {
		t.Errorf("%s", out)
	}
	// An image reused from another release's values, or recorded for none, names no installer.
	for _, of := range []any{"0.9.0/3", nil} {
		rec["installerRelease"] = of
		if st, out = k8sStatusOf(t, e); st.Installed.Installer != "" {
			t.Errorf("recorded for %v: %s", of, out)
		}
	}
	rec["installerRelease"] = "1.0.0/1"
	// After an upgrade: the revision a rollback goes back to; one recording no installer, none.
	e.f.history = []helmRevision{{Revision: 1, Status: "superseded", Chart: "notes-0.1.0"}, {Revision: 2, Status: "deployed", Chart: "notes-0.2.0"}}
	e.f.revValues = map[int]string{1: `{"stowage":{"release":{"name":"notes","version":"0.9.0","revision":3,"installer":"` + ctl + `","installerRelease":"0.9.0/3"}}}`}
	st, out = k8sStatusOf(t, e)
	if st.Previous == nil || *st.Previous != (StatusRelease{"0.9.0", 3, ctl}) || st.Helm.Revision != 2 {
		t.Errorf("%s", out)
	}
	e.f.revValues[1] = `{"stowage":{"release":{"name":"notes","version":"0.9.0","revision":3,"installer":"` + ctl + `","installerRelease":"0.8.0/1"}}}`
	if st, out = k8sStatusOf(t, e); st.Previous == nil || st.Previous.Installer != "" {
		t.Errorf("%s", out)
	}
	e.f.revValues[1] = `{"stowage":{"release":{"name":"notes","version":"0.9.0","revision":3}}}`
	if st, out = k8sStatusOf(t, e); st.Previous == nil || *st.Previous != (StatusRelease{"0.9.0", 3, ""}) || strings.Contains(out, `"installer": ""`) {
		t.Errorf("%s", out)
	}
	e.f.revValues[1] = `{"other":{}}`
	if st, out = k8sStatusOf(t, e); st.Previous != nil {
		t.Errorf("%s", out)
	}
	e.f.listed[0].Status = "pending-install"
	if st, out = k8sStatusOf(t, e); !st.Installing || st.Helm.Status != "pending-install" {
		t.Errorf("%s", out)
	}
	e.f.listed = nil
	if err := e.installer(nil).K8sStatusJSON(context.Background()); err == nil || !strings.Contains(err.Error(), "no release notes in notes") {
		t.Errorf("%v", err)
	}
}

// Both forms of `k8s status` refuse a namespace without the release (exit 3, as Docker's status
// refuses a directory that holds no install), printing nothing on standard output, the install to
// run in the message and the fix, its --kube-context included; with the release they succeed.
func TestK8sStatusRefusesNoRelease(t *testing.T) {
	t.Parallel()
	for _, kctx := range []string{"", "the cluster"} {
		e := newKubeEnv(t, spec.Provider{Name: "none"})
		e.runs("1.0.0", 1)
		e.f.history = []helmRevision{{Revision: 1, Status: "deployed", Chart: "notes-0.1.0"}}
		e.stored(map[string]string{"ADMIN_EMAIL": "admin@example.org"}, ours)
		in := e.installer(func(o *Options) { o.K8s.Context = kctx })
		forms := []struct {
			name string
			f    func(context.Context) error
		}{{"json", in.K8sStatusJSON}, {"text", in.K8sStatus}}
		for _, form := range forms {
			if err := form.f(context.Background()); err != nil {
				t.Errorf("%s with the release: %v", form.name, err)
			}
		}
		install := in.R.Ctl() + " k8s install --namespace notes --release notes"
		if kctx != "" {
			install += " --kube-context 'the cluster'"
		}
		e.f.listed = nil
		for _, form := range forms {
			e.out.Reset()
			err := form.f(context.Background())
			r, ok := Refusal(err)
			if !ok || r.Reason != "no release notes in notes: `"+install+"` installs 1.0.0" || r.Fix != "install first: `"+install+"`" {
				t.Errorf("%s: %v (refusal %v, fix %q)", form.name, err, ok, r.Fix)
			}
			if e.out.Len() != 0 {
				t.Errorf("%s printed %q", form.name, e.out)
			}
		}
	}
}

// A Secret of the release's name another tool wrote is refused, as every verb refuses it.
func TestK8sStatusJSONRefusesAForeignSecret(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	e.f.history = []helmRevision{{Revision: 1, Status: "deployed", Chart: "notes-0.1.0"}}
	e.stored(map[string]string{"ADMIN_EMAIL": "a"}, map[string]string{"app.kubernetes.io/managed-by": "other"})
	if err := e.installer(nil).K8sStatusJSON(context.Background()); err == nil || !strings.Contains(err.Error(), "was not written by") {
		t.Errorf("%v", err)
	}
}

// A deploy records the release's installer image in its values; a release the registry holds no
// manifest of records it empty, said, so a reused value never names another release's.
func TestK8sDeployRecordsTheInstaller(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 2, nil) // not published
	if err := e.installer(nil).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if len(e.f.given) != 1 || !strings.Contains(e.f.given[0], "    release:\n        installer: \"\"\n") {
		t.Errorf("values %q", e.f.given)
	}
	if !strings.Contains(e.log.String(), "the release's installer image is not recorded (status --json leaves it out): ") {
		t.Errorf("log %s", e.log)
	}
}

// Only a release with no revision to go back to has no previous one: a helm read that fails
// fails the status, as every other helm read does.
func TestK8sStatusJSONFailsOnAHelmError(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a"}, ours)
	// Two revisions of one chart: telling them apart reads their values, which helm fails to give.
	e.f.history = []helmRevision{{Revision: 1, Status: "superseded", Chart: "notes-0.1.0"}, {Revision: 2, Status: "deployed", Chart: "notes-0.1.0"}}
	if err := e.installer(nil).K8sStatusJSON(context.Background()); err == nil || !strings.Contains(err.Error(), "revision 1 not found") {
		t.Errorf("%v", err)
	}
}

// The inputs are told by the spec of the release installed: an installer of another release
// leaves them out (unknown), never lists its own spec's.
func TestK8sStatusJSONInputsOnlyFromTheInstalledRelease(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("0.9.0", 2)
	e.f.history = []helmRevision{{Revision: 1, Status: "deployed", Chart: "notes-0.1.0"}}
	e.stored(map[string]string{"ADMIN_EMAIL": "a"}, ours)
	st, out := k8sStatusOf(t, e)
	if st.Inputs != nil || strings.Contains(out, `"inputs"`) {
		t.Errorf("%s", out)
	}
	e.runs("1.0.0", 1)
	if st, out = k8sStatusOf(t, e); st.Inputs == nil || !slices.Equal(*st.Inputs, []string{"ADMIN_EMAIL"}) {
		t.Errorf("%s", out)
	}
	e.stored(nil, ours)
	if _, out = k8sStatusOf(t, e); !strings.Contains(out, `"inputs": []`) {
		t.Errorf("%s", out)
	}
}

// A revision that ran the same release, its installer image recorded or not (a registry read that
// failed once), is a copy of what runs: a rollback skips it.
func TestK8sPreviousRevisionIgnoresTheRecordedInstaller(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	ran := func(v, ctl, of string) string {
		return `{"stowage":{"registry":"r","release":{"name":"notes","version":"` + v + `","revision":1,"installer":"` + ctl + `","installerRelease":"` + of + `"}}}`
	}
	e.f.history = []helmRevision{{Revision: 1, Status: "superseded", Chart: "notes-0.1.0"}, {Revision: 2, Status: "superseded", Chart: "notes-0.2.0"}, {Revision: 3, Status: "deployed", Chart: "notes-0.2.0"}}
	e.f.revValues = map[int]string{1: ran("0.9.0", "x@sha256:9", "0.9.0/1"), 2: ran("1.0.0", "x@sha256:1", "1.0.0/1"), 3: ran("1.0.0", "", "1.0.0/1")}
	if n, err := e.installer(nil).previousRevision(context.Background()); err != nil || n != 1 {
		t.Errorf("revision %d, %v", n, err)
	}
	// Every value a deploy records about itself is left out of the comparison, each alone.
	for _, path := range selfRecorded {
		with := func(v string) string {
			leaf := map[string]any{path[len(path)-1]: v}
			for i := len(path) - 2; i >= 0; i-- {
				leaf = map[string]any{path[i]: leaf}
			}
			leaf["registry"] = "r"
			raw, _ := json.Marshal(map[string]any{"stowage": leaf})
			return string(raw)
		}
		e.f.revValues = map[int]string{1: `{"stowage":{"registry":"old"}}`, 2: with("one"), 3: with("two")}
		if n, err := e.installer(nil).previousRevision(context.Background()); err != nil || n != 1 {
			t.Errorf("%v: revision %d, %v", path, n, err)
		}
	}
	// Named here too, so a key dropped from selfRecorded fails.
	e.f.revValues = map[int]string{1: `{"stowage":{"registry":"old"}}`, 2: `{"stowage":{"registry":"r","schemaFrom":"5","release":{"installer":"a","installerRelease":"1.0.0/1"}}}`, 3: `{"stowage":{"registry":"r","schemaFrom":"6","release":{"installer":"","installerRelease":"1.0.0/2"}}}`}
	if n, err := e.installer(nil).previousRevision(context.Background()); err != nil || n != 1 {
		t.Errorf("revision %d, %v", n, err)
	}
	// Another value is another run.
	e.f.revValues = map[int]string{1: ran("0.9.0", "x@sha256:9", "0.9.0/1"), 2: ran("1.0.0", "x@sha256:1", "1.0.0/1")}
	e.f.revValues[3] = `{"stowage":{"registry":"other","release":{"name":"notes","version":"1.0.0","revision":1}}}`
	if n, err := e.installer(nil).previousRevision(context.Background()); err != nil || n != 2 {
		t.Errorf("revision %d, %v", n, err)
	}
}

// An uninstall that kept its history: nothing is installed, helm says uninstalled, and previous
// is the latest revision's release, which `rollback` brings back.
func TestK8sStatusJSONAfterAnUninstallThatKeptItsHistory(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	e.stored(nil, ours)
	e.f.listed[0].Status = "uninstalled"
	e.f.history = []helmRevision{{Revision: 1, Status: "superseded", Chart: "notes-0.1.0"}, {Revision: 2, Status: "uninstalled", Chart: "notes-0.1.0"}}
	e.f.revValues = map[int]string{2: `{"stowage":{"release":{"name":"notes","version":"1.0.0","revision":1}}}`}
	st, out := k8sStatusOf(t, e)
	if st.Installed != nil || !strings.Contains(out, `"installed": null`) || st.Previous == nil || *st.Previous != (StatusRelease{"1.0.0", 1, ""}) ||
		st.Helm != (HelmStatus{2, "uninstalled"}) || st.Inputs != nil || st.Channel != "" {
		t.Errorf("%s", out)
	}
	if n, err := e.installer(nil).previousRevision(context.Background()); err != nil || n != 2 {
		t.Errorf("rollback goes to %d, %v", n, err)
	}
}

// Each revision's values and the history are read once.
func TestK8sStatusJSONReadsEachOnce(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	e.stored(nil, ours)
	e.f.history = []helmRevision{{Revision: 1, Status: "superseded", Chart: "notes-0.1.0"}, {Revision: 2, Status: "deployed", Chart: "notes-0.1.0"}}
	e.f.revValues = map[int]string{1: `{"stowage":{"release":{"name":"notes","version":"0.9.0","revision":1}}}`, 2: `{"stowage":{"release":{"name":"notes","version":"1.0.0","revision":1}}}`}
	st, out := k8sStatusOf(t, e)
	if st.Previous == nil || st.Previous.Version != "0.9.0" {
		t.Fatalf("%s", out)
	}
	if n := len(e.f.ran("helm history")); n != 1 {
		t.Errorf("helm history %d times", n)
	}
	if n := len(e.f.ran("helm get values notes --all")); n != 0 {
		t.Errorf("the latest revision's values read again %d times", n)
	}
	for _, rev := range []string{"1", "2"} {
		if n := len(e.f.ran("helm get values notes --revision " + rev + " ")); n != 1 {
			t.Errorf("revision %s's values read %d times", rev, n)
		}
	}
}

// A deploy reads the release's installer image once: the values record it and the hook runs in it.
func TestK8sDeployReadsTheInstallerImageOnce(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.hookRuns("Succeeded", "", map[string]string{"ADMIN_PASSWORD": "minted"})
	reads := 0
	ref := e.prefix + "/notes-installer@" + ctlDigest
	if err := e.installer(func(o *Options) {
		o.NoProvision = false
		o.InstallerImage = func(context.Context, string) (string, error) { reads++; return ref, nil }
	}).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if reads != 1 || e.container("copy")["image"] != ref || len(e.f.given) != 1 || !strings.Contains(e.f.given[0], "installer: "+ref+"\n") {
		t.Errorf("read %d times; values %q", reads, e.f.given)
	}
}

// The record's read speaks of the release manifest alone; the hook's names what a mirror holds
// for it.
func TestTheInstallerImageReadTellsItsOwnUse(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	in := e.installer(nil)
	in.R = e.at("1.0.0", 1, nil)
	in.R.Manifest.Tag = "1.0.0-unpublished"
	_, err := in.releaseInstallerImage(context.Background(), e.prefix)
	if err == nil || !strings.HasSuffix(err.Error(), "holds no release manifest 1.0.0-unpublished") {
		t.Errorf("%v", err)
	}
	if _, err := in.hookImageOf(context.Background(), e.prefix, kitUse); err == nil ||
		!strings.HasSuffix(err.Error(), "holds no release manifest 1.0.0-unpublished: a handoff's Job runs in the release's installer image (a mirror holds the notes-release and notes-installer repositories too)") {
		t.Errorf("%v", err)
	}
	if _, err := in.hookImageOf(context.Background(), e.prefix, hookUse); err == nil ||
		!strings.HasSuffix(err.Error(), "holds no release manifest 1.0.0-unpublished: the provision hook runs in the release's installer image (a mirror holds the notes-release and notes-installer repositories too)") {
		t.Errorf("%v", err)
	}
}

// Every verb reads the installer image one way: provision, reset-db and the kits through
// hookImageOf too.
func TestTheHookReadsTheInstallerImageAsTheRecordDoes(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	in := e.installer(func(o *Options) {
		o.InstallerImage = func(context.Context, string) (string, error) { return "given@sha256:7", nil }
	})
	if ref, err := in.hookImageOf(context.Background(), e.prefix, hookUse); err != nil || ref != "given@sha256:7" {
		t.Errorf("%q %v", ref, err)
	}
}

// A deploy the unread image stops (the hook runs in it) says only why it stopped.
func TestK8sDeployStoppedByTheInstallerImageSaysItOnce(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 2, nil) // not published
	err := e.installer(func(o *Options) { o.NoProvision = false }).K8sInstall(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not this installer's 1.0.0 (revision 2)") || strings.Contains(e.log.String(), "not recorded") {
		t.Errorf("%v\n%s", err, e.log)
	}
}

// The VM's state names, from the marks the text status reads.
func TestDockerStatusJSONRestoreAndVolumes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	st, out := statusOf(t, e, nil)
	if st.RestoreAwaited || st.VolumesSplit || strings.Contains(out, "restore_awaited") || strings.Contains(out, "volumes_split") {
		t.Errorf("%s", out)
	}
	for _, f := range []string{restoreAwaitedFile, volumesSplitFile} {
		if err := os.WriteFile(filepath.Join(e.dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if st, out = statusOf(t, e, nil); !st.RestoreAwaited || !st.VolumesSplit {
		t.Errorf("%s", out)
	}
	// A restore running holds the install's host lock, and the mark until its last volume.
	unlock, err := lockFile(filepath.Join(e.dir, hostLock))
	if err != nil {
		t.Fatal(err)
	}
	st, out = statusOf(t, e, nil)
	unlock()
	if st.VolumesSplit {
		t.Errorf("a restore runs: %s", out)
	}
}

// A kept installer whose spec this Stowage does not load tells its channel, not its inputs, and
// is said once.
func TestDockerStatusJSONKeptSpecUnread(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(e.dir, "notes-stow")
	files, err := payload.Read(kept)
	if err != nil {
		t.Fatal(err)
	}
	files[payload.Spec] = bytes.Replace(files[payload.Spec], []byte(`{`), []byte(`{"Unknown":1,`), 1)
	var buf bytes.Buffer
	if err := payload.Write(&buf, strings.NewReader("exe"), files); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kept, buf.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	e.r, e.self = testInstallerWith(t, "2.0.0", nil)
	e.log.Reset()
	st, out := statusOf(t, e, nil)
	if st.Inputs != nil || st.Channel != "stable" || strings.Count(e.log.String(), "the installer the install keeps, ") != 1 {
		t.Errorf("%s\nlog %q", out, e.log)
	}
}

// The channel is the one the install follows (--channel recorded), else the installed release's
// as an installer carrying it tells it; the inputs come only from such an installer, and with
// none that carries the release they are left out (unknown), as the release's channel is.
func TestDockerStatusJSONChannelAndInputsFromTheInstalledRelease(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if st, out := statusOf(t, e, nil); st.Channel != "stable" || st.Inputs == nil {
		t.Errorf("%s", out)
	}
	// Run by another release's installer: the one the install keeps carries the installed release.
	e.r, e.self = testInstallerWith(t, "2.0.0", nil)
	if st, out := statusOf(t, e, nil); st.Channel != "stable" || st.Inputs == nil || !slices.Equal(*st.Inputs, []string{"ADMIN_EMAIL"}) {
		t.Errorf("%s", out)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "notes-stow"), []byte("not an installer"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.log.Reset()
	if st, out := statusOf(t, e, nil); st.Inputs != nil || strings.Contains(out, `"inputs"`) || strings.Count(e.log.String(), "the installer the install keeps, ") != 1 {
		t.Errorf("%s\nlog %q", out, e.log)
	}
	// No kept installer at all: said by the fields left out alone.
	if err := os.Remove(filepath.Join(e.dir, "notes-stow")); err != nil {
		t.Fatal(err)
	}
	e.log.Reset()
	if st, out := statusOf(t, e, nil); st.Inputs != nil || e.log.String() != "" {
		t.Errorf("%s\nlog %q", out, e.log)
	}
	// A followed channel is the install's own record: told whatever installer runs.
	env := e.readEnv()
	env.Set(ChannelVar, "beta")
	if err := env.WriteFile(filepath.Join(e.dir, envName)); err != nil {
		t.Fatal(err)
	}
	if st, out := statusOf(t, e, nil); st.Channel != "beta" || st.Inputs != nil {
		t.Errorf("%s", out)
	}
}

// A re-run's refusal names each first-install-only setting by its flag, a new one included.
func TestFirstInstallOnlyRefusalNamesEachFlag(t *testing.T) {
	t.Parallel()
	if got := firstInstallOnlyRefusal(firstInstallOnly).Error(); !strings.HasPrefix(got, "--address, --registry, --registry-ca and --channel are recorded at the first install: ") {
		t.Errorf("%s", got)
	}
	more := append(slices.Clone(firstInstallOnly), firstInstallSetting{Name: "new_one"})
	if got := firstInstallOnlyRefusal(more).Error(); !strings.HasPrefix(got, "--address, --registry, --registry-ca, --channel and --new-one are recorded") {
		t.Errorf("%s", got)
	}
	if got := firstInstallOnlyRefusal(firstInstallOnly[2:3]).Error(); !strings.HasPrefix(got, "--registry-ca is recorded at the first install: ") {
		t.Errorf("%s", got)
	}
}

// Each first-install-only setting's flag, derived from its name, is one the installer takes: the
// Docker installer's, or the VM's install (channel).
func TestFirstInstallOnlyFlagsExist(t *testing.T) {
	t.Parallel()
	docker := Command(testRelease(t, "1.0.0"), "", io.Discard, io.Discard)
	r, _ := bundledInstaller(t, "1.0.0", 1, nil)
	vm, _, err := Command(r, "", io.Discard, io.Discard).Find([]string{"install"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range firstInstallOnly {
		name := strings.TrimPrefix(s.flag(), "--")
		if docker.Flags().Lookup(name) == nil && vm.Flags().Lookup(name) == nil {
			t.Errorf("%s: no flag %s", s.Name, s.flag())
		}
	}
}

// A re-run names only the first-install settings it was given.
func TestARerunRefusesOnlyWhatItWasGiven(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	err := e.install(func(o *Options) { o.Registry = "mirror.example.org/notes" })
	if err == nil || !strings.HasPrefix(err.Error(), "--registry is recorded at the first install: ") {
		t.Errorf("%v", err)
	}
	err = e.install(func(o *Options) { o.Address, o.Channel = "127.0.0.9", "beta" })
	if err == nil || !strings.HasPrefix(err.Error(), "--address and --channel are recorded at the first install: ") {
		t.Errorf("%v", err)
	}
}

// An upgrade or a rollback that stopped keeps the release installed in .stowage-prev: `rollback`
// finishes on it, so it is no previous release.
func TestDockerStatusJSONPreviousIsNeverTheInstalled(t *testing.T) {
	t.Parallel()
	e := installed(t)
	e.to("2.0.0", nil)
	if err := e.upgrade(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	file := filepath.Join(e.dir, PrevDir, prevState)
	if err := os.WriteFile(file, []byte(`{"project_version":"2.0.0","revision":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, out := statusOf(t, e, nil); st.Previous != nil {
		t.Errorf("%s", out)
	}
	if err := os.WriteFile(file, []byte(`{"project_version":"2.0.0","revision":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, out := statusOf(t, e, nil); st.Previous == nil || *st.Previous != (StatusRelease{"2.0.0", 2, ""}) {
		t.Errorf("%s", out)
	}
}
