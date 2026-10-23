package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/spec"
)

// resettable is an install whose Docker lists one volume per compose volume of its project.
func resettable(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.docker.hookOut = "ADMIN_PASSWORD=first\n"
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	e.docker.answer = volumes(get(e.readEnv(), "COMPOSE_PROJECT_NAME"), nil)
	e.docker.calls = nil
	return e
}

// volumes answers `docker volume ls` with the volumes of a compose project, each with its compose
// key: postgres and uploads; then other answers.
func volumes(project string, other func(string) ([]byte, bool, error)) func(string) ([]byte, bool, error) {
	return func(a string) ([]byte, bool, error) {
		if !strings.HasPrefix(a, "volume ls ") {
			if other != nil {
				return other(a)
			}
			return nil, false, nil
		}
		if a != "volume ls --filter label=com.docker.compose.project="+project+` --format {{.Name}} {{.Label "com.docker.compose.volume"}}` {
			return nil, true, nil
		}
		return []byte(project + "_postgres postgres\n" + project + "_uploads uploads\nother_x x\n"), true, nil
	}
}

func (e *env) reset(mod func(*Options)) error {
	return e.installer(mod).Reset(context.Background())
}

// verbs are the docker calls made, each cut to its words after the compose files.
func (e *env) verbs() []string {
	var out []string
	for _, c := range e.docker.calls {
		a := strings.Join(c.Args, " ")
		if i := strings.LastIndex(a, ContentCompose+" "); i >= 0 {
			a = "compose " + a[i+len(ContentCompose)+1:]
		}
		out = append(out, a)
	}
	return out
}

func TestResetWipesTheResetVolumesAndProvisionsAsFirstInstalled(t *testing.T) {
	t.Parallel()
	e := resettable(t)
	before := read(t, filepath.Join(e.dir, ".env"))
	e.docker.hookOut = "ADMIN_PASSWORD=second\n"
	if err := e.reset(func(o *Options) { o.Yes = true }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	project := get(e.readEnv(), "COMPOSE_PROJECT_NAME")
	v := e.verbs()
	at := func(prefix string) int {
		return slices.IndexFunc(v, func(s string) bool { return strings.HasPrefix(s, prefix) })
	}
	down, rmPG, rmUp, up := at("compose down --remove-orphans"), at("volume rm "+project+"_postgres"), at("volume rm "+project+"_uploads"), at("compose up --detach")
	if down < 0 || rmPG < down || rmUp < down || up < rmPG || up < rmUp {
		t.Fatalf("down %d, rm %d %d, up %d: %q", down, rmPG, rmUp, up, v)
	}
	if n := len(e.docker.ran("volume rm")); n != 2 {
		t.Errorf("%d volumes removed", n)
	}
	// The hook runs as on a first install: given nothing it returned before.
	if len(e.docker.hookEnv) != 0 {
		t.Errorf("the hook was given %v", e.docker.hookEnv)
	}
	if outs, _ := ReadEnvFile(filepath.Join(e.dir, ProvisionEnv)); get(outs, "ADMIN_PASSWORD") != "second" {
		t.Errorf("provision.env: %v", outs.Keys())
	}
	if after := read(t, filepath.Join(e.dir, ".env")); after != before {
		t.Errorf(".env changed:\n%s\n%s", before, after)
	}
}

func TestResetAsksWithoutHoldingTheLock(t *testing.T) {
	t.Parallel()
	e := resettable(t)
	var held error
	p := &fakePrompter{answers: map[string]string{"confirm": "yes"}, onAsk: func() {
		unlock, err := lockFile(filepath.Join(e.dir, lifecycleLock))
		if err == nil {
			unlock()
		}
		held = err
	}}
	if err := e.reset(func(o *Options) { o.Prompt = p }); err != nil {
		t.Fatal(err)
	}
	if held != nil || len(p.forms) != 1 {
		t.Errorf("the lock during the question: %v (%d forms)", held, len(p.forms))
	}
	if n := len(e.docker.ran("volume rm")); n != 2 {
		t.Errorf("%d volumes removed", n)
	}
}

func TestResetWithNoProvisionLeavesTheHookToProvision(t *testing.T) {
	t.Parallel()
	e := resettable(t)
	marker := filepath.Join(e.dir, installingFile)
	if err := os.WriteFile(marker, []byte("failed: x\n"), 0o600); err != nil { // a first install that failed
		t.Fatal(err)
	}
	if err := e.reset(func(o *Options) { o.Yes, o.NoProvision = true, true }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Error("a reset whose stack came up left the first install's marker")
	}
	if n := len(e.hookCalls()); n != 0 {
		t.Errorf("%d hook runs", n)
	}
	if _, err := os.Stat(filepath.Join(e.dir, ProvisionEnv)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("provision.env kept: %v", err)
	}
}

func TestResetAsksAndRefusesWithoutConsent(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		mod    func(*Options)
		want   string
		asked  bool
		dryRun bool
	}{
		{"no terminal", nil, "give --yes", false, false},
		{"answered no", func(o *Options) { o.Prompt = &fakePrompter{answers: map[string]string{"confirm": "no"}} }, "aborted", true, false},
		{"default", func(o *Options) { o.Prompt = &fakePrompter{} }, "aborted", true, false},
		{"dry run", func(o *Options) { o.DryRun = true }, "", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := resettable(t)
			var p *fakePrompter
			err := e.reset(func(o *Options) {
				if c.mod != nil {
					c.mod(o)
				}
				p, _ = o.Prompt.(*fakePrompter)
			})
			if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
			if c.asked && (p == nil || len(p.forms) != 1 || p.forms[0].Questions[0].Title != "Reset notes: stop it, delete its volumes postgres, uploads, forget what its provision hook returned, then start it as first installed") {
				t.Errorf("asked %+v", p)
			}
			if c.dryRun && !strings.Contains(e.out.String(), "a run would stop it, delete its volumes postgres, uploads") {
				t.Errorf("output: %s", e.out)
			}
			if n := len(e.docker.ran("compose")) + len(e.docker.ran("volume")); n != 0 {
				t.Errorf("%d calls: %q", n, e.verbs())
			}
			if _, err := os.Stat(filepath.Join(e.dir, ProvisionEnv)); err != nil {
				t.Errorf("provision.env: %v", err)
			}
		})
	}
}

func TestResetNamesTheVolumesItKeeps(t *testing.T) {
	t.Parallel()
	e := resettable(t)
	e.to("1.0.0", func(s *spec.Spec) {
		s.Data.Volumes["certs"] = spec.Volume{KeepOnReset: true}
	})
	if err := e.reset(func(o *Options) { o.DryRun = true }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "delete its volumes postgres, uploads (keeping certs),") {
		t.Errorf("output: %s", e.out)
	}
}

func TestResetRefusesNothingToWipeAndAnotherRelease(t *testing.T) {
	t.Parallel()
	e := resettable(t)
	e.to("1.0.0", func(s *spec.Spec) { s.Database = nil })
	if err := e.reset(func(o *Options) { o.Yes = true }); err == nil || !strings.Contains(err.Error(), "notes 1.0.0 has no database: reset-db has nothing to reset") {
		t.Fatalf("no database: %v", err)
	}
	e.to("1.0.0", func(s *spec.Spec) { s.Data.Reset = nil })
	if err := e.reset(func(o *Options) { o.Yes = true }); err == nil || !strings.Contains(err.Error(), "nothing to wipe") {
		t.Fatalf("empty data.reset: %v", err)
	}
	e.to("1.0.1", nil)
	if err := e.reset(func(o *Options) { o.Yes = true }); err == nil || !strings.Contains(err.Error(), "upgrade --dir") {
		t.Fatalf("another release: %v", err)
	}
	if n := len(e.docker.ran("compose")) + len(e.docker.ran("volume")); n != 0 {
		t.Errorf("%d calls: %q", n, e.verbs())
	}
}

func TestAResetThatStopsIsRunAgain(t *testing.T) {
	t.Parallel()
	e := resettable(t)
	ls := e.docker.answer
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if strings.HasPrefix(a, "volume rm ") && strings.HasSuffix(a, "_uploads") {
			return nil, true, errors.New("volume is in use")
		}
		return ls(a)
	}
	err := e.reset(func(o *Options) { o.Yes = true })
	if err == nil || !strings.Contains(err.Error(), "the stack is stopped: reset-db again finishes the reset") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, ProvisionEnv)); err != nil {
		t.Errorf("provision.env forgotten before the wipe ended: %v", err)
	}
	e.docker.answer = ls
	if err := e.reset(func(o *Options) { o.Yes = true }); err != nil {
		t.Fatal(err)
	}
}

// resettableVM is an installed VM whose Docker lists one volume per compose volume of its project.
func resettableVM(t *testing.T) (*env, *Hostd, *fakeHost) {
	t.Helper()
	e := installedVM(t)
	e.docker.answer = volumes(get(e.readEnv(), "COMPOSE_PROJECT_NAME"), e.docker.answer)
	host := &fakeHost{}
	return e, daemon(e, host), host
}

func TestTheDaemonResetsAsAJobOnceConfirmedAndJudged(t *testing.T) {
	t.Parallel()
	e, d, host := resettableVM(t)
	h := d.Handler()
	var refused errOf
	if code := call(t, h, "POST", "/v1/reset-db", ResetRequest{}, &refused); code != 400 || !strings.Contains(refused.Error, "confirm") {
		t.Fatalf("unconfirmed: %d %s", code, refused.Error)
	}
	host.judge = "activating"
	if code := call(t, h, "POST", "/v1/reset-db", ResetRequest{Confirm: &Version{"1.0.0", 1}}, &refused); code != 409 || !strings.Contains(refused.Error, "has not judged this boot") {
		t.Fatalf("before the gate: %d %s", code, refused.Error)
	}
	if n := len(e.docker.ran("volume")); n != 0 {
		t.Fatalf("%d volume calls before a reset ran", n)
	}
	host.judge = "active"
	// Another release than the one confirmed, a job running, a reboot scheduled: refused, the
	// plan too.
	if code := call(t, h, "POST", "/v1/reset-db", ResetRequest{Confirm: &Version{"1.0.0", 2}}, &refused); code != 409 || !strings.Contains(refused.Error, "not 1.0.0 (revision 2), which was confirmed") {
		t.Fatalf("another release: %d %s", code, refused.Error)
	}
	busy, err := d.startJob("upgrade", "notes-1.0.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "POST"} {
		if code := call(t, h, method, "/v1/reset-db", ResetRequest{Confirm: &Version{"1.0.0", 1}}, &refused); code != 409 || !strings.Contains(refused.Error, "an upgrade is running") {
			t.Fatalf("%s during a job: %d %s", method, code, refused.Error)
		}
	}
	d.finish(busy, nil)
	d.mu.Lock()
	d.rebooting = true
	d.mu.Unlock()
	if code := call(t, h, "GET", "/v1/reset-db", nil, &refused); code != 409 || !strings.Contains(refused.Error, "a reboot is scheduled") {
		t.Fatalf("a reboot scheduled: %d %s", code, refused.Error)
	}
	d.mu.Lock()
	d.rebooting = false
	d.mu.Unlock()
	var p ResetPlan
	if code := call(t, h, "GET", "/v1/reset-db", nil, &p); code != 200 || p.Target != (Version{"1.0.0", 1}) || !strings.HasPrefix(p.Plan, "stop it, delete its volumes postgres, uploads") {
		t.Fatalf("plan: %d %+v", code, p)
	}
	hooks := len(e.hookCalls())
	// The token: required, the GET's, once.
	for _, tok := range []string{"", "forged"} {
		if code := call(t, h, "POST", "/v1/reset-db", ResetRequest{Confirm: &Version{"1.0.0", 1}, Token: tok}, &refused); code != 409 || !strings.Contains(refused.Error, "needs the token") {
			t.Fatalf("token %q: %d %s", tok, code, refused.Error)
		}
	}
	if code := call(t, h, "POST", "/v1/reset-db", ResetRequest{Confirm: &Version{"1.0.0", 1}, Token: p.Token, NoProvision: true}, nil); code != 202 {
		t.Fatalf("reset: %d", code)
	}
	j := waitJob(t, h)
	if code := call(t, h, "POST", "/v1/reset-db", ResetRequest{Confirm: &Version{"1.0.0", 1}, Token: p.Token}, &refused); code != 409 || !strings.Contains(refused.Error, "needs the token") {
		t.Fatalf("the same token again: %d %s", code, refused.Error)
	}
	if j.Kind != "reset-db" || j.State != "done" || j.Target != "1.0.0 (revision 1)" {
		t.Fatalf("%+v", j)
	}
	if n := len(e.docker.ran("volume rm")); n != 2 {
		t.Errorf("%d volumes removed", n)
	}
	if !slices.ContainsFunc(j.Log, func(l string) bool { return strings.HasPrefix(l, "deleting the volume ") }) {
		t.Errorf("log %q", j.Log)
	}
	if n := len(e.hookCalls()) - hooks; n != 0 {
		t.Errorf("no_provision: %d hook runs", n)
	}
}

func TestResetOverTheSocketAsksThenFollowsTheJob(t *testing.T) {
	t.Parallel()
	e, d, host := resettableVM(t)
	sock := served(t, d)
	// The daemon's refusal comes before the question.
	host.mu.Lock()
	host.judge = "activating"
	host.mu.Unlock()
	if out, err := ctl(e, sock, "reset-db"); err == nil || !strings.Contains(err.Error(), "has not judged this boot") {
		t.Fatalf("%v\n%s", err, out)
	}
	host.mu.Lock()
	host.judge = ""
	host.mu.Unlock()
	if out, err := ctl(e, sock, "reset-db"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("%v\n%s", err, out)
	}
	if n := len(e.docker.ran("volume")); n != 0 {
		t.Fatalf("%d volume calls without consent", n)
	}
	out, err := ctl(e, sock, "reset-db", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "deleting the volume ") || !strings.Contains(out, "notes 1.0.0 is running") {
		t.Errorf("output:\n%s", out)
	}
}

const resetManifest = `---
# Source: notes/templates/data.yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: notes-uploads
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: notes-postgres
spec:
  selector:
    matchLabels: { app: notes-postgres }
  volumeClaimTemplates:
    - metadata:
        name: data
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: notes-certs
  annotations: { helm.sh/resource-policy: keep }
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: cache
  namespace: shared
---
apiVersion: v1
kind: Service
metadata:
  name: notes-web
`

func TestReleaseClaimsAreTheManifestsAndItsStatefulSets(t *testing.T) {
	t.Parallel()
	pg := `,"labels":{"app":"notes-postgres"}`
	listed := []byte(`{"items":[{"metadata":{"name":"data-notes-postgres-0"` + pg + `}},{"metadata":{"name":"data-notes-postgres-12"` + pg + `}},
		{"metadata":{"name":"notes-uploads"}},{"metadata":{"name":"data-notes-postgres-x"` + pg + `}},{"metadata":{"name":"data-notes-postgres-"` + pg + `}},
		{"metadata":{"name":"data-notes-postgres-backup-0"` + pg + `}},{"metadata":{"name":"other"}},
		{"metadata":{"name":"data-notes-postgres-3","labels":{"app":"another-set"}}},{"metadata":{"name":"data-notes-postgres-4"}},
		{"metadata":{"name":"notes-certs"}},{"metadata":{"name":"cache"}}]}`)
	got, kept, byHelm, err := releaseClaims([]byte(resetManifest), listed, "notes")
	if err != nil {
		t.Fatal(err)
	}
	// Another set's claim of the same name shape, a claim of another namespace's name and one the
	// chart keeps are not taken.
	if want := []string{"data-notes-postgres-0", "data-notes-postgres-12", "notes-uploads"}; !slices.Equal(got, want) {
		t.Errorf("%q, want %q", got, want)
	}
	if !slices.Equal(kept, []string{"notes-certs"}) {
		t.Errorf("kept %q", kept)
	}
	if !slices.Equal(byHelm, []string{"notes-uploads"}) {
		t.Errorf("deleted by helm %q", byHelm)
	}
	// In the namespace the manifest names, its claim is the release's.
	if got, _, _, _ := releaseClaims([]byte(resetManifest), listed, "shared"); !slices.Contains(got, "cache") {
		t.Errorf("in shared: %q", got)
	}
	if _, _, _, err := releaseClaims([]byte("kind: [\n"), listed, "notes"); err == nil {
		t.Error("a broken manifest passed")
	}
}

func (e *kubeEnv) reset(mod func(*Options)) error {
	return e.installer(mod).K8sReset(context.Background())
}

func TestK8sResetUninstallsDeletesTheClaimsAndInstallsAgain(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	e.f.manifest = resetManifest
	e.f.claims = []string{"data-notes-postgres-0", "notes-uploads", "unrelated"}
	if err := e.reset(func(o *Options) { o.Yes = true }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	var order []string
	for _, c := range e.f.calls {
		if s := strings.Join(c, " "); strings.HasPrefix(s, "helm uninstall") || strings.HasPrefix(s, "kubectl delete") || strings.HasPrefix(s, "helm upgrade") {
			order = append(order, s)
		}
	}
	// What the provision hook returned is forgotten, between the wipe and the install.
	if len(order) != 4 || order[0] != "helm uninstall notes --wait --timeout 10m0s --namespace notes" ||
		order[1] != "kubectl delete persistentvolumeclaims data-notes-postgres-0 notes-uploads --ignore-not-found --wait --timeout 10m0s --namespace notes" ||
		order[2] != "kubectl delete secret notes-stowage-provision --ignore-not-found --namespace notes" ||
		!strings.HasPrefix(order[3], "helm upgrade --install notes ") || !strings.Contains(order[3], " --atomic ") {
		t.Fatalf("%q", order)
	}
	if string(e.f.chart) != string(e.chart) {
		t.Errorf("chart %q", e.f.chart)
	}
	// The values given are the release's own, as helm returned them.
	if len(e.f.given) != 1 || !strings.Contains(e.f.given[0], `"registry":"`+e.prefix+`"`) {
		t.Errorf("values %q", e.f.given)
	}
	if !strings.Contains(e.out.String(), "runs notes 1.0.0 (revision 1), as first installed") {
		t.Errorf("out %s", e.out)
	}
	if len(e.f.applied) != 0 {
		t.Errorf("the Secrets were applied: %v", e.f.applied)
	}
}

func TestK8sResetAsksAndChangesNothingUnconfirmed(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		mod  func(*Options)
		want string
	}{
		{"no terminal", nil, "give --yes"},
		{"answered no", func(o *Options) { o.Prompt = &fakePrompter{} }, "aborted"},
		{"dry run", func(o *Options) { o.DryRun = true }, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newKubeEnv(t, spec.Provider{Name: "none"})
			e.runs("1.0.0", 1)
			e.f.manifest = resetManifest
			e.f.claims = []string{"notes-uploads"}
			err := e.reset(c.mod)
			if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
			if c.want == "" && e.out.String() != "a run would uninstall release notes in notes, delete its PersistentVolumeClaims notes-uploads, forget what the provision hook returned (the Secret notes-stowage-provision), then install notes 1.0.0 again with the same values\n" {
				t.Errorf("out %q", e.out)
			}
			if n := len(e.f.ran("helm uninstall")) + len(e.f.ran("kubectl delete")) + len(e.f.ran("helm upgrade")); n != 0 {
				t.Errorf("%d changes", n)
			}
		})
	}
}

func TestK8sResetRefusals(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	yes := func(o *Options) { o.Yes = true }
	if err := e.reset(yes); err == nil || !strings.Contains(err.Error(), "no release notes in notes") {
		t.Errorf("no release: %v", err)
	}
	e.runs("1.0.1", 1)
	if err := e.reset(yes); err == nil || !strings.Contains(err.Error(), "reset it with the installer of 1.0.1") {
		t.Errorf("another release: %v", err)
	}
	e.runs("1.0.0", 1)
	e.f.listed[0].Status = "pending-upgrade"
	if err := e.reset(yes); err == nil || !strings.Contains(err.Error(), "pending-upgrade") {
		t.Errorf("pending: %v", err)
	}
	e.runs("1.0.0", 1)
	e.release = e.at("1.0.0", 1, func(s *spec.Spec) { s.Data.Reset = nil })
	if err := e.reset(yes); err == nil || !strings.Contains(err.Error(), "nothing to wipe") {
		t.Errorf("empty data.reset: %v", err)
	}
	e.release = e.at("1.0.0", 1, func(s *spec.Spec) { s.Database = nil })
	if err := e.reset(yes); err == nil || !strings.Contains(err.Error(), "notes 1.0.0 has no database: reset-db has nothing to reset") {
		t.Errorf("no database: %v", err)
	}
	if n := len(e.f.ran("helm uninstall")); n != 0 {
		t.Errorf("%d uninstalls", n)
	}
}

func TestK8sResetThatFailsFromTheUninstallOnKeepsTheValues(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name       string
		uninstall  error
		delete     error
		install    error
		installs   int
		claimsHint bool
	}{
		{"the uninstall's wait", errors.New("timed out waiting for the condition"), nil, nil, 0, true},
		{"the claims' delete", nil, errors.New("forbidden"), nil, 0, true},
		{"the install", nil, nil, errors.New("not ready"), 1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newKubeEnv(t, spec.Provider{Name: "none"})
			e.runs("1.0.0", 1)
			e.f.manifest = resetManifest
			e.f.claims = []string{"notes-uploads"}
			e.f.uninstErr, e.f.deleteErr, e.f.upErr = c.uninstall, c.delete, c.install
			// An operator's own values file of the obvious name is never written over.
			mine := filepath.Join(e.dir, "notes-values.yaml")
			if err := os.WriteFile(mine, []byte("# mine\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			err := e.reset(func(o *Options) { o.Yes, o.K8s.Context = true, "prod" })
			m := regexp.MustCompile(`its values are in ([^\s;:]+)[;:] .* --values (\S+)` + "`").FindStringSubmatch(fmt.Sprint(err))
			if !strings.Contains(err.Error(), "`notes-stow k8s install --namespace notes --release notes --kube-context prod --timeout 10m0s --values ") {
				t.Errorf("the install named: %v", err)
			}
			if m == nil || m[1] != m[2] || filepath.Dir(m[1]) != e.dir {
				t.Fatalf("%v", err)
			}
			if hint := strings.Contains(err.Error(), "`kubectl delete persistentvolumeclaims notes-uploads --namespace notes --context prod` first"); hint != c.claimsHint {
				t.Errorf("claims hint %v: %v", hint, err)
			}
			if raw := read(t, m[1]); !strings.Contains(raw, `"registry":"`+e.prefix+`"`) {
				t.Errorf("kept %q", raw)
			}
			if fi, _ := os.Stat(m[1]); fi.Mode().Perm() != 0o600 {
				t.Errorf("mode %v", fi.Mode().Perm())
			}
			if read(t, mine) != "# mine\n" {
				t.Errorf("the operator's file was written over")
			}
			if n := len(e.f.ran("helm upgrade")); n != c.installs {
				t.Errorf("%d installs", n)
			}
		})
	}
}

func TestResetRefusesBeforeTheStackStops(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		mod  func(e *env)
		want string
	}{
		{"a volume the compose file lacks", func(e *env) {
			e.to("1.0.0", func(s *spec.Spec) {
				s.Data.Volumes["spool"] = spec.Volume{}
				s.Data.Reset = append(s.Data.Reset, "spool")
			})
		}, `"spool" is no volume of the compose file`},
		{"an external volume", func(e *env) {
			r := *e.r
			r.Compose = []byte(strings.Replace(string(r.Compose), "  uploads: {}", "  uploads: { external: true }", 1))
			if string(r.Compose) == string(e.r.Compose) {
				e.t.Fatal("the compose file has no uploads: {}")
			}
			e.r = &r
		}, `"uploads" is an external volume`},
		{"an interpolated name", func(e *env) {
			r := *e.r
			r.Compose = []byte(strings.Replace(string(r.Compose), "  uploads: {}", "  uploads: { name: \"${COMPOSE_PROJECT_NAME}-up\" }", 1))
			e.r = &r
		}, "which Compose interpolates"},
		{"a hook that does not decode", func(e *env) {
			r, sp := *e.r, *e.r.Spec
			sp.Hooks.Provision = &spec.Provider{Name: "nope"}
			r.Spec = &sp
			e.r = &r
		}, "hooks.provision"},
		{"no project name", func(e *env) {
			f := e.readEnv()
			f.Set("COMPOSE_PROJECT_NAME", "")
			if err := f.WriteFile(filepath.Join(e.dir, ".env")); err != nil {
				e.t.Fatal(err)
			}
		}, "records no COMPOSE_PROJECT_NAME"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := resettable(t)
			c.mod(e)
			if err := e.reset(func(o *Options) { o.Yes = true }); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
			if n := len(e.docker.ran("compose")) + len(e.docker.ran("volume")); n != 0 {
				t.Errorf("%d calls: %q", n, e.verbs())
			}
		})
	}
}

func TestAVolumeNeverCreatedIsNamedNotAFailure(t *testing.T) {
	t.Parallel()
	e := resettable(t)
	project := get(e.readEnv(), "COMPOSE_PROJECT_NAME")
	e.docker.answer = func(a string) ([]byte, bool, error) {
		switch {
		case strings.HasPrefix(a, "volume ls "):
			return []byte(project + "_postgres postgres\n"), true, nil
		case strings.HasPrefix(a, "volume inspect "):
			return nil, true, errors.New("docker volume inspect: exit status 1: Error response from daemon: get x: no such volume")
		}
		return nil, false, nil
	}
	if err := e.reset(func(o *Options) { o.Yes = true }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.log.String(), "the volume uploads was never created: nothing to delete") {
		t.Errorf("log: %s", e.log)
	}
	if rm := e.docker.ran("volume rm"); len(rm) != 1 || strings.Join(rm[0].Args, " ") != "volume rm "+project+"_postgres" {
		t.Errorf("removed %v", rm)
	}
}

func TestTheLookupByNameNeverDeletesAnotherProjectsOrGuesses(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		inspect func() ([]byte, error)
		want    string
	}{
		{"another project's", func() ([]byte, error) { return []byte("other\n"), nil }, "belongs to the Compose project other"},
		{"an unreadable volume", func() ([]byte, error) { return nil, errors.New("docker volume: permission denied") }, "permission denied"},
		{"one the stack never mounted", func() ([]byte, error) { return []byte("<no value>\n"), nil }, "no container of the stack mounts it"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := resettable(t)
			project := get(e.readEnv(), "COMPOSE_PROJECT_NAME")
			e.docker.answer = func(a string) ([]byte, bool, error) {
				switch {
				case strings.HasPrefix(a, "volume ls "):
					return []byte(project + "_postgres postgres\n"), true, nil
				case strings.HasPrefix(a, "volume inspect "):
					out, err := c.inspect()
					return out, true, err
				}
				return nil, false, nil
			}
			err := e.reset(func(o *Options) { o.Yes = true })
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "reset-db again finishes the reset") {
				t.Fatalf("%v", err)
			}
			if n := len(e.docker.ran("volume rm " + project + "_uploads")); n != 0 {
				t.Errorf("deleted")
			}
		})
	}
}

func TestAVolumeComposeAdoptedIsFoundByItsName(t *testing.T) {
	t.Parallel()
	e := resettable(t)
	project := get(e.readEnv(), "COMPOSE_PROJECT_NAME")
	e.docker.answer = func(a string) ([]byte, bool, error) {
		switch {
		case strings.HasPrefix(a, "volume ls "):
			return []byte(project + "_postgres postgres\n"), true, nil // uploads made by hand: no labels
		case strings.HasPrefix(a, "ps --all "):
			return []byte(project + "_postgres\n/srv," + project + "_uploads\n"), true, nil
		case a == `volume inspect --format {{index .Labels "com.docker.compose.project"}} `+project+"_uploads":
			return []byte("<no value>\n"), true, nil
		case strings.HasPrefix(a, "volume inspect "):
			return nil, true, errors.New("no such volume")
		}
		return nil, false, nil
	}
	if err := e.reset(func(o *Options) { o.Yes = true }); err != nil {
		t.Fatal(err)
	}
	if n := len(e.docker.ran("volume rm " + project + "_uploads")); n != 1 {
		t.Errorf("the adopted volume was removed %d times", n)
	}
}

func TestTheDaemonRefusesAResetOfAnotherReleaseAtOnce(t *testing.T) {
	t.Parallel()
	e, d, _ := resettableVM(t)
	m := *d.R.Manifest
	m.Revision = 2
	r := *d.R
	r.Manifest = &m
	d.R = &r
	e.docker.calls = nil
	for _, method := range []string{"GET", "POST"} {
		var refused errOf
		if code := call(t, d.Handler(), method, "/v1/reset-db", ResetRequest{Confirm: &Version{"1.0.0", 1}}, &refused); code != 409 ||
			!strings.Contains(refused.Error, "the install runs notes 1.0.0 (revision 1), not this OS's notes 1.0.0 (revision 2)") {
			t.Fatalf("%s: %d %s", method, code, refused.Error)
		}
	}
	if n := len(e.docker.ran("compose")) + len(e.docker.ran("volume")); n != 0 {
		t.Errorf("%d docker calls", n)
	}
}

func TestK8sResetKeepsTheValuesInItsOwnDirectoryWhenTheCurrentOneRefuses(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	e.f.manifest = resetManifest
	e.f.deleteErr = errors.New("forbidden")
	e.f.claims = []string{"notes-uploads"}
	var kept string
	t.Cleanup(func() {
		if strings.HasPrefix(filepath.Base(kept), "stowage-chart-") {
			os.RemoveAll(kept)
		}
	})
	err := e.reset(func(o *Options) { o.Yes, o.Dir = true, filepath.Join(e.dir, "gone") })
	m := regexp.MustCompile(`its values are in ([^\s;:]+)`).FindStringSubmatch(fmt.Sprint(err))
	if m == nil {
		// The kept directory, named in the log, still goes.
		if l := regexp.MustCompile(`they stay in (\S+)`).FindStringSubmatch(e.log.String()); l != nil {
			kept = filepath.Dir(l[1])
		}
		t.Fatalf("%v", err)
	}
	kept = filepath.Dir(m[1])
	if raw := read(t, m[1]); !strings.Contains(raw, `"registry":"`+e.prefix+`"`) {
		t.Errorf("kept %q", raw)
	}
}

func TestAVolumeOfALiteralComposeNameIsFoundByIt(t *testing.T) {
	t.Parallel()
	e := resettable(t)
	r := *e.r
	r.Compose = []byte(strings.Replace(string(r.Compose), "  uploads: {}", "  uploads: { name: notes-up }", 1))
	e.r = &r
	project := get(e.readEnv(), "COMPOSE_PROJECT_NAME")
	e.docker.answer = func(a string) ([]byte, bool, error) {
		switch {
		case strings.HasPrefix(a, "volume ls "):
			return []byte(project + "_postgres postgres\n"), true, nil
		case strings.HasPrefix(a, "ps --all "):
			return []byte("notes-up\n"), true, nil
		case a == `volume inspect --format {{index .Labels "com.docker.compose.project"}} notes-up`:
			return []byte("\n"), true, nil
		case strings.HasPrefix(a, "volume inspect "):
			return nil, true, errors.New("Error: No such volume")
		}
		return nil, false, nil
	}
	if err := e.reset(func(o *Options) { o.Yes = true }); err != nil {
		t.Fatal(err)
	}
	if n := len(e.docker.ran("volume rm notes-up")); n != 1 {
		t.Errorf("notes-up removed %d times: %q", n, e.verbs())
	}
}

func TestK8sResetHintsNameTheContextItRanOn(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ given, want string }{
		{"", "--context kind-current` first"},
		{"my cluster", "--context 'my cluster'` first"},
	} {
		e := newKubeEnv(t, spec.Provider{Name: "none"})
		e.runs("1.0.0", 1)
		e.f.manifest = resetManifest
		e.f.claims = []string{"notes-uploads"}
		e.f.deleteErr = errors.New("forbidden")
		err := e.reset(func(o *Options) { o.Yes, o.K8s.Context = true, c.given })
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v", c.given, err)
		}
		if ctx := strings.TrimSuffix(strings.TrimPrefix(c.want, "--context "), "` first"); !strings.Contains(fmt.Sprint(err), "--kube-context "+ctx+" --timeout") {
			t.Errorf("%q: the install named: %v", c.given, err)
		}
	}
}

func TestTheVMAsksNamingTheReleaseItResets(t *testing.T) {
	t.Parallel()
	e, d, _ := resettableVM(t)
	sock := served(t, d)
	p := &fakePrompter{answers: map[string]string{"confirm": "no"}}
	var out bytes.Buffer
	cmd := vmCommand(e.r, e.self, &out, &out, func(bool) Prompter { return p }, clientTimes{unit: time.Millisecond})
	cmd.SetArgs([]string{"reset-db", "--socket", sock})
	if err := cmd.ExecuteContext(context.Background()); !errors.Is(err, ErrAborted) {
		t.Fatalf("%v\n%s", err, &out)
	}
	if len(p.forms) != 1 || !strings.HasPrefix(p.forms[0].Questions[0].Title, "Reset notes 1.0.0 (revision 1): stop it, delete its volumes postgres, uploads") {
		t.Errorf("asked %+v", p.forms)
	}
	if n := len(e.docker.ran("volume")); n != 0 {
		t.Errorf("%d volume calls", n)
	}
}
