package install

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/norvexlabs/stowage/internal/helm"
	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// fakeKube answers the helm and kubectl CLIs the installer runs on Kubernetes, and records them.
type fakeKube struct {
	mu        sync.Mutex
	helm      string         // `helm version`
	listed    []helmRelease  // `helm list`
	history   []helmRevision // `helm history`
	revValues map[int]string // `helm get values --revision N`: the values of revision N
	values    map[string]any
	secret    string // `kubectl get secret` of the release's Secret
	pull      string // `kubectl get secret` of the pull Secret
	namespace bool   // the namespace exists
	nsErr     error  // reading the namespace fails
	// podSecurity is the namespace's Pod Security enforce label.
	podSecurity string
	calls       [][]string
	applied     []map[string]any // the objects kubectl applied
	chart       []byte           // the chart helm was given
	given       []string         // the values files helm was given, read when it ran
	upErr       error            // `helm upgrade` and `helm rollback` fail
	manifest    string           // `helm get manifest`
	claims      []string         // the PersistentVolumeClaims `kubectl get` lists
	deleteErr   error            // `kubectl delete` fails
	uninstErr   error            // `helm uninstall` fails
	// The provision hook's Job.
	outputs string         // `kubectl get secret` of the outputs Secret
	job     string         // `kubectl get job`: a Job an earlier run left; set to the created one
	created map[string]any // the Job kubectl created
	patches []string       // the patch files kubectl patch was given
	// runJob, when set, is the created Job's run: the pod's status it ends with, the outputs Secret
	// it leaves (data, as `kubectl get` gives it) and the steps' logs.
	runJob    func(job map[string]any) (pod string, outputs string, logs map[string]string)
	pods      string            // `kubectl get pods`
	configMap string            // `kubectl get configmap`: the chart's ConfigMap for the hook
	denied    []string          // `kubectl auth can-i` answers no to these "verb resource"
	podsErrs  int               // `kubectl get pods` fails this many times first
	canIErr   string            // `kubectl auth can-i` cannot ask, its stderr saying this
	canI      []string          // what `kubectl auth can-i` was asked
	logs      map[string]string // `kubectl logs`, by container
	objects   map[string]string // `kubectl get <kind> <name>` by "kind/name", before any other answer
	// sql answers the schema pod's query: its pod's phase and its output.
	sql      func(query string) (phase, out string)
	schemaPo map[string][2]string // the schema pods made: name → phase, output
	schemaQs []string             // the queries they ran
}

type kubeTool struct {
	f   *fakeKube
	bin string
}

func (k kubeTool) Run(_ context.Context, c Cmd) ([]byte, error) {
	f := k.f
	f.mu.Lock()
	defer f.mu.Unlock()
	a := c.Args
	f.calls = append(f.calls, append([]string{k.bin}, a...))
	switch k.bin + " " + a[0] {
	case "helm version":
		return []byte(f.helm), nil
	case "helm list":
		// As helm 3 and 4 list: the statuses named, deployed and failed when none is (helm 3).
		shown := map[string]bool{}
		for _, st := range []string{"deployed", "failed", "superseded", "uninstalled", "uninstalling"} {
			shown[st] = slices.Contains(a, "--"+st)
		}
		pending := slices.Contains(a, "--pending")
		if !slices.ContainsFunc(a[1:], func(s string) bool { return shown[strings.TrimPrefix(s, "--")] }) && !pending {
			shown["deployed"], shown["failed"] = true, true
		}
		var rs []helmRelease
		for _, r := range f.listed {
			if shown[r.Status] || (pending && strings.HasPrefix(r.Status, "pending-")) {
				rs = append(rs, r)
			}
		}
		return json.Marshal(rs)
	case "helm history":
		return json.Marshal(f.history)
	case "helm get":
		if a[1] == "manifest" {
			return []byte(f.manifest), nil
		}
		if i := slices.Index(a, "--revision"); i > 0 {
			if !slices.Contains(a, "--all") {
				return []byte(`{}`), nil // the values given, without the chart's: none here
			}
			n, _ := strconv.Atoi(a[i+1])
			v, ok := f.revValues[n]
			if !ok {
				return nil, fmt.Errorf("revision %d not found", n)
			}
			return []byte(v), nil
		}
		return json.Marshal(f.values)
	case "helm upgrade":
		var err error
		if f.chart, err = os.ReadFile(a[3]); err != nil {
			return nil, err
		}
		for i, s := range a {
			if s == "--values" {
				raw, err := os.ReadFile(a[i+1])
				if err != nil {
					return nil, err
				}
				f.given = append(f.given, string(raw))
			}
		}
		return nil, f.upErr
	case "helm rollback":
		return nil, f.upErr
	case "helm uninstall":
		return nil, f.uninstErr
	case "kubectl delete":
		if a[1] == "job" {
			f.job = ""
		}
		return nil, f.deleteErr
	case "kubectl create":
		if f.sql != nil {
			raw, _ := io.ReadAll(c.Stdin)
			var pod struct {
				Kind     string
				Metadata struct{ Name string }
				Spec     struct {
					Containers []struct{ Command []string }
				}
			}
			if json.Unmarshal(raw, &pod) == nil && pod.Kind == "Pod" {
				cmdl := pod.Spec.Containers[0].Command
				q := cmdl[slices.Index(cmdl, "-c")+1]
				f.schemaQs = append(f.schemaQs, q)
				phase, out := f.sql(q)
				if f.schemaPo == nil {
					f.schemaPo = map[string][2]string{}
				}
				f.schemaPo[pod.Metadata.Name] = [2]string{phase, out}
				return nil, nil
			}
			c.Stdin = bytes.NewReader(raw)
		}
		var job map[string]any
		if err := json.NewDecoder(c.Stdin).Decode(&job); err != nil {
			return nil, err
		}
		f.created = job
		raw, _ := json.Marshal(job)
		f.job = string(raw)
		if f.runJob != nil {
			f.pods, f.outputs, f.logs = f.runJob(job)
		}
		return nil, nil
	case "kubectl patch":
		raw, err := os.ReadFile(a[slices.Index(a, "--patch-file")+1])
		if err != nil {
			return nil, err
		}
		f.patches = append(f.patches, string(raw))
		return nil, nil
	case "kubectl auth":
		// As kubectl answers: yes (exit 0) or no (exit 1) on stdout; an error on stderr otherwise.
		if f.canIErr != "" {
			return nil, fmt.Errorf("kubectl auth: %w: %s", exec.Command("false").Run(), f.canIErr)
		}
		f.canI = append(f.canI, strings.Join(a[2:], " "))
		if slices.Contains(f.denied, strings.Join(a[2:4], " ")) {
			_, _ = io.WriteString(c.Stdout, "no - RBAC: role \"x\" not found\n") // a denial with its reason
			return nil, fmt.Errorf("kubectl auth: %w", exec.Command("false").Run())
		}
		_, _ = io.WriteString(c.Stdout, "yes\n")
		return nil, nil
	case "kubectl logs":
		if p, ok := f.schemaPo[a[1]]; ok {
			return []byte(p[1]), nil
		}
		return []byte(f.logs[a[slices.Index(a, "--container")+1]]), nil
	case "helm status":
		_, err := io.WriteString(c.Stdout, "STATUS: deployed\n")
		return nil, err
	case "kubectl version", "kubectl rollout":
		return nil, nil
	case "kubectl config":
		return []byte("kind-current\n"), nil
	case "kubectl get":
		if a[1] == "persistentvolumeclaims" {
			var l struct {
				Items []map[string]any `json:"items"`
			}
			l.Items = []map[string]any{}
			for _, n := range f.claims {
				l.Items = append(l.Items, map[string]any{"metadata": map[string]any{"name": n, "labels": map[string]string{"app": "notes-postgres"}}})
			}
			return json.Marshal(l)
		}
		if a[1] == "namespace" {
			if f.nsErr != nil {
				return nil, f.nsErr
			}
			if slices.ContainsFunc(a, func(s string) bool { return strings.Contains(s, "pod-security") }) {
				return []byte(f.podSecurity), nil
			}
			if f.namespace {
				return []byte("namespace/" + a[2] + "\n"), nil
			}
			return nil, nil
		}
		if obj, ok := f.objects[a[1]+"/"+a[2]]; ok {
			return []byte(obj), nil
		}
		if p, ok := f.schemaPo[a[2]]; ok && a[1] == "pod" {
			return []byte(p[0]), nil
		}
		switch {
		case slices.Contains([]string{"serviceaccount", "role", "rolebinding"}, a[1]):
			return nil, nil
		case a[1] == "job":
			return []byte(f.job), nil
		case a[1] == "configmap":
			return []byte(f.configMap), nil
		case a[1] == "pods" && f.podsErrs > 0:
			f.podsErrs--
			return nil, errors.New("the connection to the server was refused")
		case a[1] == "pods":
			return []byte(cmp.Or(f.pods, `{"items": []}`)), nil
		case strings.HasSuffix(a[2], "-pull"):
			return []byte(f.pull), nil
		case strings.HasSuffix(a[2], "-stowage-provision"):
			return []byte(f.outputs), nil
		}
		return []byte(f.secret), nil
	case "kubectl apply":
		var l struct{ Items []map[string]any }
		if err := json.NewDecoder(c.Stdin).Decode(&l); err != nil {
			return nil, err
		}
		f.applied = append(f.applied, l.Items...)
		for _, o := range l.Items {
			if md, _ := o["metadata"].(map[string]any); o["kind"] == "Secret" && strings.HasSuffix(fmt.Sprint(md["name"]), "-stowage") {
				raw, _ := json.Marshal(o) // as `kubectl get` gives it back
				f.secret = string(raw)
			}
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected %s %q", k.bin, a)
}

func (f *fakeKube) ran(prefix string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			out = append(out, c)
		}
	}
	return out
}

// kubeEnv is a cluster the fake answers for, and a registry holding the release's chart.
type kubeEnv struct {
	t       *testing.T
	f       *fakeKube
	prefix  string
	pull    spec.Provider
	chart   []byte // the chart archive in the registry
	digest  string // its manifest's digest
	vars    map[string]string
	out     *bytes.Buffer
	log     *bytes.Buffer
	dir     string
	auth    []string
	mu      sync.Mutex
	ran     [][]string // the installers upgrade --version ran
	release *Release
}

func newKubeEnv(t *testing.T, pull spec.Provider) *kubeEnv {
	t.Helper()
	e := &kubeEnv{t: t, pull: pull, f: &fakeKube{helm: "v3.15.2"}, chart: []byte("chart archive"),
		vars: map[string]string{"ADMIN_EMAIL": "admin@example.org"}, out: &bytes.Buffer{}, log: &bytes.Buffer{}, dir: t.TempDir()}
	reg := ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.auth = append(e.auth, r.Header.Get("Authorization"))
		e.mu.Unlock()
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	e.prefix = strings.TrimPrefix(srv.URL, "http://") + "/acme/notes"
	c := registry.New(authn.NewMultiKeychain())
	repo, err := c.Repo(e.prefix + "/charts/notes-stable")
	if err != nil {
		t.Fatal(err)
	}
	h, err := c.PutManifest(context.Background(), repo, registry.Manifest{
		Config: registry.Blob{MediaType: helm.ConfigMediaType, Data: []byte(`{"name":"notes"}`)},
		Layers: []registry.Blob{{MediaType: helm.ContentMediaType, Data: e.chart}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.digest = h.String()
	e.release = e.at("1.0.0", 1, nil)
	e.published(e.release)
	return e
}

// ctlDigest is the installer image the release manifests published in a kubeEnv name.
const ctlDigest = "sha256:5555555555555555555555555555555555555555555555555555555555555555"

// published puts r's release manifest in the registry under its tag, naming its installer image.
func (e *kubeEnv) published(r *Release) {
	m := *r.Manifest
	m.Artifacts.Installer = ctlDigest
	(&pubEnv{env: &env{t: e.t}, prefix: e.prefix}).putRelease(m.Tag, &m)
}

func (e *kubeEnv) spec(s *spec.Spec) { s.Registry.Prefix, s.Registry.Pull = e.prefix, e.pull }

// at is the release of version at revision, its chart the one in the registry.
func (e *kubeEnv) at(version string, revision int, mod func(*spec.Spec)) *Release {
	r, _ := testInstallerOf(e.t, version, revision, func(s *spec.Spec) {
		e.spec(s)
		if mod != nil {
			mod(s)
		}
	}, func(m *release.Manifest) {
		m.Artifacts.Chart = &release.Chart{Repository: "charts/notes-stable", Version: "0.1.0", Digest: e.digest}
	})
	return r
}

func (e *kubeEnv) installer(mod func(*Options)) *Installer {
	// The provision hook does not run unless a test asks for it (hookRuns).
	o := Options{Dir: e.dir, Out: e.out, Log: e.log, Getenv: func(k string) string { return e.vars[k] }, NoProvision: true,
		K8s: K8sOptions{Namespace: "notes", Release: "notes", Timeout: 10 * time.Minute, Helm: kubeTool{e.f, "helm"}, Kubectl: kubeTool{e.f, "kubectl"}}}
	if mod != nil {
		mod(&o)
	}
	return &Installer{R: e.release, O: o}
}

// runs makes the cluster run version at revision, as the chart's values record it.
func (e *kubeEnv) runs(version string, revision int) {
	e.f.namespace = true
	e.f.listed = []helmRelease{{Name: "notes", Status: "deployed", AppVersion: version}}
	e.f.values = map[string]any{"stowage": map[string]any{"registry": e.prefix,
		"release": map[string]any{"name": "notes", "version": version, "revision": revision, "channel": "stable", "tag": version}}}
}

// stored makes the release's Secret hold data, as the installer writes it.
func (e *kubeEnv) stored(data map[string]string, labels map[string]string) {
	enc := map[string]string{}
	for k, v := range data {
		enc[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}, "data": enc})
	e.f.secret = string(raw)
}

var ours = map[string]string{"app.kubernetes.io/managed-by": "stowage", "app.kubernetes.io/part-of": "notes", "app.kubernetes.io/instance": "notes"}

// secretData is the data of the Secret kubectl applied under name, decoded.
func (e *kubeEnv) secretData(name string) map[string]string {
	e.t.Helper()
	for _, o := range e.f.applied {
		md := o["metadata"].(map[string]any)
		if o["kind"] == "Secret" && md["name"] == name {
			out := map[string]string{}
			for k, v := range o["data"].(map[string]any) {
				b, err := base64.StdEncoding.DecodeString(v.(string))
				if err != nil {
					e.t.Fatal(err)
				}
				out[k] = string(b)
			}
			return out
		}
	}
	return nil
}

func TestK8sInstallAppliesTheSecretsThenInstallsTheChartByDigest(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	if err := e.installer(nil).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if len(e.f.applied) != 2 || e.f.applied[0]["kind"] != "Namespace" {
		t.Fatalf("applied %v", e.f.applied)
	}
	data := e.secretData("notes-stowage")
	if data["ADMIN_EMAIL"] != "admin@example.org" || len(data["SESSION_SECRET"]) != 64 || len(data["POSTGRES_PASSWORD"]) != 18 || len(data) != 3 {
		t.Fatalf("secret %v", data)
	}
	md := e.f.applied[1]["metadata"].(map[string]any)
	if md["namespace"] != "notes" || md["labels"].(map[string]any)["app.kubernetes.io/managed-by"] != "stowage" {
		t.Errorf("metadata %v", md)
	}
	if e.secretData("notes-stowage-pull") != nil {
		t.Error("a pull Secret for public images")
	}
	if !bytes.Equal(e.f.chart, e.chart) {
		t.Errorf("chart %q", e.f.chart)
	}
	up := e.f.ran("helm upgrade")
	if len(up) != 1 {
		t.Fatalf("ran %q", e.f.calls)
	}
	a := strings.Join(up[0], " ")
	for _, want := range []string{"upgrade --install notes ", " --wait --timeout 10m0s ", " --atomic", " --namespace notes"} {
		if !strings.Contains(a, want) {
			t.Errorf("%s: no %q", a, want)
		}
	}
	if strings.Contains(a, "--reset-then-reuse-values") || strings.Contains(a, "--kube-context") {
		t.Errorf("%s", a)
	}
	want := fmt.Sprintf("stowage:\n    pullSecret: \"\"\n    registry: %s\n    secret: notes-stowage\n    sysctls: []\n", e.prefix)
	if len(e.f.given) != 1 || e.f.given[0] != want {
		t.Errorf("values %q, want %q", e.f.given, want)
	}
	// The Secrets exist before the chart that reads them.
	if i, j := slices.IndexFunc(e.f.calls, func(c []string) bool { return c[0]+c[1] == "kubectlapply" }), slices.IndexFunc(e.f.calls, func(c []string) bool { return c[0]+c[1] == "helmupgrade" }); i > j {
		t.Errorf("order %q", e.f.calls)
	}
	if !strings.Contains(e.out.String(), "release notes in notes runs notes 1.0.0 (revision 1)") {
		t.Errorf("out %s", e.out)
	}
	if !slices.Contains(e.f.ran("kubectl apply")[0], "--server-side") {
		t.Errorf("apply %q", e.f.ran("kubectl apply"))
	}
}

func TestK8sOnHelm4RollsBackWithItsOwnFlag(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.f.helm, e.f.namespace = "v4.0.4+g8650e1d", true
	err := e.installer(func(o *Options) {
		o.K8s.Context, o.K8s.Values = "prod", []string{valuesFile(t, "a: 1\n"), valuesFile(t, "b: 2\n")}
	}).K8sInstall(context.Background())
	if err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	a := strings.Join(e.f.ran("helm upgrade")[0], " ")
	if !strings.Contains(a, " --rollback-on-failure") || strings.Contains(a, "--atomic") || !strings.HasSuffix(a, "--namespace notes --kube-context prod") {
		t.Errorf("%s", a)
	}
	if len(e.f.given) != 3 || e.f.given[1] != "a: 1\n" || e.f.given[2] != "b: 2\n" {
		t.Errorf("values %q: the operator's come after the installer's", e.f.given)
	}
	if e.f.applied[0]["kind"] != "Secret" {
		t.Errorf("an existing namespace applied: %v", e.f.applied[0])
	}
	if c := e.f.ran("kubectl get namespace")[0]; !slices.Contains(c, "--context") {
		t.Errorf("%q", c)
	}
}

func valuesFile(t *testing.T, s string) string {
	f, err := os.CreateTemp(t.TempDir(), "values-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(s)
	_ = f.Close()
	return f.Name()
}

func TestK8sHelmVersionBoundary(t *testing.T) {
	t.Parallel()
	for v, ok := range map[string]bool{"v3.14.0": true, "v3.13.3": false, "v4.0.0": true, "garbage": false} {
		e := newKubeEnv(t, spec.Provider{Name: "none"})
		e.f.helm = v
		err := e.installer(nil).K8sInstall(context.Background())
		if (err == nil) != ok {
			t.Errorf("%s: %v", v, err)
		}
		if !ok && len(e.f.applied) != 0 {
			t.Errorf("%s: applied", v)
		}
	}
}

func TestK8sNamesKubernetesTakes(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	for _, c := range []struct {
		ns, rel string
		ok      bool
	}{
		{"notes", strings.Repeat("a", 53), true},
		{"notes", strings.Repeat("a", 54), false},
		{strings.Repeat("n", 63), "notes", true},
		{strings.Repeat("n", 64), "notes", false},
		{"Notes", "notes", false},
		{"notes", "notes-", false},
		{"notes", "a.b", false},
	} {
		err := e.installer(func(o *Options) { o.K8s.Namespace, o.K8s.Release = c.ns, c.rel }).checkTarget()
		if (err == nil) != c.ok {
			t.Errorf("%s/%s: %v", c.ns, c.rel, err)
		}
	}
}

func TestK8sAStoppedInstallsSecretIsKept(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.stored(map[string]string{"SESSION_SECRET": "kept", "ADMIN_EMAIL": "first@example.org", "LEGACY": "x"}, ours)
	e.vars = nil // nothing given: a recorded input is not asked again
	if err := e.installer(nil).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	data := e.secretData("notes-stowage")
	if data["SESSION_SECRET"] != "kept" || data["ADMIN_EMAIL"] != "first@example.org" || data["LEGACY"] != "x" || len(data["POSTGRES_PASSWORD"]) != 18 {
		t.Fatalf("secret %v", data)
	}
	// A first install takes --set over what an earlier run recorded.
	e.f.applied = nil
	if err := e.installer(func(o *Options) { o.Set = map[string]string{"ADMIN_EMAIL": "fixed@example.org"} }).K8sInstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.secretData("notes-stowage")["ADMIN_EMAIL"]; got != "fixed@example.org" {
		t.Errorf("ADMIN_EMAIL %q", got)
	}
}

func TestK8sNeverWritesOverASecretItDidNotWrite(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.stored(map[string]string{"SESSION_SECRET": "theirs"}, map[string]string{"app.kubernetes.io/managed-by": "Helm"})
	err := e.installer(nil).K8sInstall(context.Background())
	if err == nil || !strings.Contains(err.Error(), "secret notes-stowage in notes was not written by notes-stow for release notes (label app.kubernetes.io/managed-by is \"Helm\", not \"stowage\")") {
		t.Fatalf("%v", err)
	}
	if len(e.f.applied) != 0 || len(e.f.ran("helm upgrade")) != 0 {
		t.Error("changed the cluster")
	}
	// Another project's, or another release's, left under this name.
	for key, v := range map[string]string{"app.kubernetes.io/part-of": "other", "app.kubernetes.io/instance": "other"} {
		labels := maps.Clone(ours)
		labels[key] = v
		e.stored(map[string]string{"SESSION_SECRET": "theirs"}, labels)
		err := e.installer(nil).K8sInstall(context.Background())
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("(label %s is \"other\", not \"notes\")", key)) {
			t.Errorf("%s: %v", key, err)
		}
	}
	// This project's Secret of another release name, under the name this release takes.
	e.stored(map[string]string{"SESSION_SECRET": "theirs"}, ours)
	err = e.installer(func(o *Options) { o.K8s.Release = "notes" }).K8sInstall(context.Background())
	if err != nil {
		t.Fatalf("positive control: %v", err)
	}
	e.f.applied = nil
	e.f.secret = strings.ReplaceAll(e.f.secret, "notes-stowage", "app-stowage")
	err = e.installer(func(o *Options) { o.K8s.Release = "app" }).K8sInstall(context.Background())
	if err == nil || !strings.Contains(err.Error(), `(label app.kubernetes.io/instance is "notes", not "app")`) {
		t.Errorf("another release's: %v", err)
	}
	if len(e.f.applied) != 0 {
		t.Error("changed the cluster")
	}
}

func TestK8sInstallRefusals(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		mod  func(*kubeEnv, *Options)
		want string
	}{
		{"exists", func(e *kubeEnv, _ *Options) { e.runs("1.0.0", 1) }, "release notes in notes exists (deployed, 1.0.0): `notes-stow k8s upgrade --namespace notes --release notes` moves it to 1.0.0"},
		{"unknown input", func(_ *kubeEnv, o *Options) { o.Set = map[string]string{"NOPE": "x"} }, "no input of this release on Kubernetes is named NOPE (inputs: ADMIN_EMAIL)"},
		{"required input", func(e *kubeEnv, _ *Options) { e.vars = nil }, "missing required input(s)"},
		{"no values file", func(_ *kubeEnv, o *Options) { o.K8s.Values = []string{"/nonexistent/values.yaml"} }, "--values: "},
		{"chart not in the mirror", func(e *kubeEnv, o *Options) { o.K8s.Registry = e.prefix + "/mirror" }, "is not in the registry (a mirror holds the charts/ repositories too)"},
		{"no chart", func(e *kubeEnv, _ *Options) {
			e.release, _ = testInstallerOf(e.t, "1.0.0", 1, e.spec, nil)
		}, "notes 1.0.0 has no chart (published without the kubernetes shape)"},
	} {
		e := newKubeEnv(t, spec.Provider{Name: "none"})
		err := e.installer(func(o *Options) { c.mod(e, o) }).K8sInstall(context.Background())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
		if len(e.f.applied) != 0 || len(e.f.ran("helm upgrade")) != 0 {
			t.Errorf("%s: changed the cluster", c.name)
		}
	}
}

func TestK8sFailedInstallKeepsTheSecret(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.f.upErr = errors.New("timed out waiting for the condition")
	err := e.installer(nil).K8sInstall(context.Background())
	if err == nil || !strings.Contains(err.Error(), "helm removed the release; its Secret notes-stowage is kept, and a new `notes-stow k8s install --namespace notes --release notes` reuses its values") {
		t.Fatalf("%v", err)
	}
}

// --unsafe-sysctls gives the pods their network settings, =false takes them away, and an upgrade
// without it keeps the release's; a failed deploy that gave them names the kubelet's setting.
func TestK8sUnsafeSysctls(t *testing.T) {
	t.Parallel()
	on, off := true, false
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.f.upErr = errors.New("timed out waiting for the condition")
	err := e.installer(func(o *Options) { o.K8s.UnsafeSysctls = &on }).K8sInstall(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refuses them (SysctlForbidden), as does a namespace whose Pod Security level is not privileged: `notes-stow k8s tune --namespace notes --release notes` prints the kubelet's setting") {
		t.Fatalf("%v", err)
	}
	want := "    sysctls:\n        - name: net.core.somaxconn\n          value: \"8192\"\n        - name: net.ipv4.tcp_mtu_probing\n          value: \"1\"\n" +
		"        - name: net.ipv4.tcp_notsent_lowat\n          value: \"131072\"\n        - name: net.ipv4.tcp_slow_start_after_idle\n          value: \"0\"\n"
	if !strings.HasSuffix(e.f.given[0], want) {
		t.Errorf("values %q, want %q at the end", e.f.given[0], want)
	}
	for _, c := range []struct {
		name string
		had  bool // the release gives them
		set  *bool
		want string
		hint bool
	}{{"none kept", false, nil, "", false}, {"kept as given", true, nil, "", true}, {"taken away", true, &off, "    sysctls: []\n", false}, {"given", false, &on, want, true}} {
		e := newKubeEnv(t, spec.Provider{Name: "none"})
		e.runs("0.9.0", 1)
		if c.had {
			e.f.values["stowage"].(map[string]any)["sysctls"] = []any{map[string]any{"name": "net.core.somaxconn", "value": "8192"}}
		}
		e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
		e.f.upErr = errors.New("UPGRADE FAILED")
		err := e.installer(func(o *Options) { o.K8s.UnsafeSysctls = c.set }).K8sUpgrade(context.Background())
		if hint := strings.Contains(fmt.Sprint(err), "SysctlForbidden"); hint != c.hint {
			t.Errorf("%s: %v", c.name, err)
		}
		if got := e.f.given[0]; (c.want == "" && strings.Contains(got, "sysctls")) || !strings.HasSuffix(got, c.want) {
			t.Errorf("%s: values %q", c.name, got)
		}
	}
}

// --unsafe-sysctls in a namespace that enforces a Pod Security level below privileged is refused
// before anything changes; privileged, no label, or a namespace that cannot be read, let through.
func TestK8sUnsafeSysctlsPodSecurity(t *testing.T) {
	t.Parallel()
	on := true
	for _, c := range []struct {
		level   string
		nsErr   error
		refused bool
	}{{"baseline", nil, true}, {"restricted", nil, true}, {"privileged", nil, false}, {"", nil, false}, {"baseline", errors.New("forbidden"), false}} {
		e := newKubeEnv(t, spec.Provider{Name: "none"})
		e.f.podSecurity, e.f.nsErr = c.level, c.nsErr
		err := e.installer(func(o *Options) { o.K8s.UnsafeSysctls = &on }).K8sInstall(context.Background())
		refused := err != nil && strings.Contains(err.Error(), "namespace notes enforces the Pod Security level "+c.level+", which refuses pods given those settings: label it privileged (`kubectl label namespace notes pod-security.kubernetes.io/enforce=privileged --overwrite`)")
		if refused != c.refused || refused && (len(e.f.applied) != 0 || e.f.ran("helm upgrade") != nil) {
			t.Errorf("%q (%v): %v, %d applied", c.level, c.nsErr, err, len(e.f.applied))
		}
	}
	// An upgrade that keeps the release's settings (the flag left out), on pods that run with them
	// today, is warned: the cluster may exempt the namespace.
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("0.9.0", 1)
	e.f.values["stowage"].(map[string]any)["sysctls"] = []any{map[string]any{"name": "net.core.somaxconn", "value": "8192"}}
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
	e.f.podSecurity = "baseline"
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil || !strings.Contains(e.log.String(), "warning: --unsafe-sysctls: namespace notes enforces the Pod Security level baseline") {
		t.Errorf("an upgrade keeping the settings: %v\n%s", err, e.log)
	}
}

// k8s tune lists, for the kubelet, every setting --unsafe-sysctls gives.
func TestK8sTuneListsWhatTheKubeletMustAllow(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	if err := e.installer(nil).K8sTune(); err != nil {
		t.Fatal(err)
	}
	out := e.out.String()
	var keys []string
	for _, s := range podSysctls() {
		keys = append(keys, s["name"])
		if !strings.Contains(out, "\n    - "+s["name"]+"\n") {
			t.Errorf("no %s in allowedUnsafeSysctls:\n%s", s["name"], out)
		}
	}
	if !strings.Contains(out, "--allowed-unsafe-sysctls="+strings.Join(keys, ",")+"\n") || !strings.Contains(out, "`notes-stow tune` on a node") {
		t.Errorf("%s", out)
	}
}

func TestK8sPullSecretHoldsTheBakedAccountOnTheProjectsRegistryOnly(t *testing.T) {
	t.Parallel()
	pull := spec.Provider{Name: "token", Config: map[string]any{"user": "notes-pull", "token_env": "NOTES_PULL"}}
	e := newKubeEnv(t, pull)
	e.release.Pull = &payload.Account{User: "notes-pull", Password: "baked"}
	if err := e.installer(nil).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	var cfg struct {
		Auths map[string]struct{ Username, Password, Auth string }
	}
	if err := json.Unmarshal([]byte(e.secretData("notes-stowage-pull")[".dockerconfigjson"]), &cfg); err != nil {
		t.Fatal(err)
	}
	host, _, _ := strings.Cut(e.prefix, "/")
	a := cfg.Auths[host]
	if len(cfg.Auths) != 1 || a.Username != "notes-pull" || a.Password != "baked" || a.Auth != base64.StdEncoding.EncodeToString([]byte("notes-pull:baked")) {
		t.Fatalf("auths %+v", cfg.Auths)
	}
	for _, o := range e.f.applied {
		if o["metadata"].(map[string]any)["name"] == "notes-stowage-pull" && o["type"] != "kubernetes.io/dockerconfigjson" {
			t.Errorf("type %v", o["type"])
		}
	}
	if !strings.Contains(e.f.given[0], "pullSecret: notes-stowage-pull\n") {
		t.Errorf("values %q", e.f.given[0])
	}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("notes-pull:baked"))
	if !slices.Contains(e.auth, basic) {
		t.Errorf("the chart was not read with the pull account: %q", e.auth)
	}
}

func TestK8sNeverWritesOverAPullSecretItDidNotWrite(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "token", Config: map[string]any{"user": "notes-pull", "token_env": "NOTES_PULL"}})
	e.release.Pull = &payload.Account{User: "notes-pull", Password: "baked"}
	e.f.pull = `{"metadata":{"labels":{"owner":"someone"}},"data":{}}`
	err := e.installer(nil).K8sInstall(context.Background())
	if err == nil || !strings.Contains(err.Error(), "secret notes-stowage-pull in notes was not written by notes-stow for release notes") {
		t.Fatalf("%v", err)
	}
	if len(e.f.applied) != 0 || len(e.f.ran("helm upgrade")) != 0 {
		t.Error("changed the cluster")
	}
}

// An upgrade with no pull password keeps the pull Secret the release names: it writes no
// pullSecret value, so helm reuses the earlier one.
func TestK8sUpgradeWithoutAPasswordKeepsThePullSecret(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "token", Config: map[string]any{"user": "notes-pull", "token_env": "NOTES_PULL"}})
	e.runs("0.9.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if strings.Contains(e.f.given[0], "pullSecret") || e.secretData("notes-stowage-pull") != nil {
		t.Errorf("values %q, applied %v", e.f.given[0], e.f.applied)
	}
	// A first install with none is refused: the cluster could not pull the images.
	e = newKubeEnv(t, spec.Provider{Name: "token", Config: map[string]any{"user": "notes-pull", "token_env": "NOTES_PULL"}})
	err := e.installer(nil).K8sInstall(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the cluster pulls the images with the pull account notes-pull, whose password this installer does not carry: set NOTES_PULL") {
		t.Fatalf("%v", err)
	}
	if len(e.f.applied) != 0 {
		t.Error("changed the cluster")
	}
	// From a mirror, the operator's own pull Secret is given through --values.
	e.vars["NOTES_PULL"] = ""
	err = e.installer(func(o *Options) { o.K8s.Registry = e.prefix + "/mirror" }).K8sInstall(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is not in the registry") {
		t.Errorf("mirror: %v", err)
	}
}

// A namespace the operator may not read is taken to exist; an upgrade never reads it.
func TestK8sNamespaceReadOnlyOnInstallAndNeverRequired(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.f.nsErr = errors.New(`namespaces "notes" is forbidden`)
	if err := e.installer(nil).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if e.f.applied[0]["kind"] != "Secret" || !strings.Contains(e.log.String(), "namespace notes could not be read (namespaces \"notes\" is forbidden): taken to exist") {
		t.Errorf("applied %v\n%s", e.f.applied, e.log)
	}
	e = newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("0.9.0", 1)
	e.f.namespace = false
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.f.ran("kubectl get namespace") != nil || e.f.applied[0]["kind"] != "Secret" {
		t.Errorf("upgrade: %q %v", e.f.calls, e.f.applied)
	}
}

func TestK8sDryRunChangesNothingAndRedactsSecrets(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	if err := e.installer(func(o *Options) { o.DryRun = true }).K8sInstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.f.applied) != 0 || len(e.f.ran("helm upgrade")) != 0 {
		t.Fatal("changed the cluster")
	}
	out := e.out.String()
	for _, want := range []string{"namespace notes: created\n", "  ADMIN_EMAIL=admin@example.org\n", "  SESSION_SECRET=<redacted>\n", "  POSTGRES_PASSWORD=<redacted>\n", "helm upgrade --install notes oci://" + e.prefix + "/charts/notes-stable@" + e.digest + " ", "--values <the values above>", "secret: notes-stowage\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
}

func TestK8sUpgradeOrdersTheReleases(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		version  string
		revision int
		want     string // "" upgrades
	}{
		{"0.9.0", 3, ""},
		{"1.0.0", 1, ""}, // the same release: applied again
		{"1.0.0", 2, "release notes in notes runs notes 1.0.0 (revision 2), after this installer's 1.0.0 (revision 1): `notes-stow k8s rollback --namespace notes --release notes` goes back one upgrade"},
		{"1.1.0", 1, "runs notes 1.1.0 (revision 1), after"},
		{"not.a.version", 1, "the install's version \"not.a.version\""},
	} {
		e := newKubeEnv(t, spec.Provider{Name: "none"})
		e.runs(c.version, c.revision)
		e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
		err := e.installer(nil).K8sUpgrade(context.Background())
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: %v", c.version, err)
				continue
			}
			a := strings.Join(e.f.ran("helm upgrade")[0], " ")
			if !strings.Contains(a, "--reset-then-reuse-values") {
				t.Errorf("%s: %s", c.version, a)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.version, err, c.want)
		}
		if len(e.f.applied) != 0 || len(e.f.ran("helm upgrade")) != 0 {
			t.Errorf("%s: changed the cluster", c.version)
		}
	}
}

func TestK8sUpgradeRefusals(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		mod  func(*kubeEnv, *Options)
		want string
	}{
		{"absent", func(e *kubeEnv, _ *Options) { e.f.listed = nil }, "no release notes in notes: `notes-stow k8s install --namespace notes --release notes` installs 1.0.0"},
		{"pending", func(e *kubeEnv, _ *Options) { e.f.listed[0].Status = "pending-upgrade" }, "is pending-upgrade: another helm operation runs on it, or one stopped before it finished (`helm rollback notes --namespace notes` ends it)"},
		{"pending install", func(e *kubeEnv, _ *Options) { e.f.listed[0].Status = "pending-install" }, "(`helm uninstall notes --namespace notes` ends it)"},
		{"pending on helm 4", func(e *kubeEnv, _ *Options) { e.f.helm, e.f.listed[0].Status = "v4.0.4", "pending-upgrade" }, "is pending-upgrade"},
		{"not ours", func(e *kubeEnv, _ *Options) { e.f.values = map[string]any{"image": "x"} }, "was not installed from a chart Stowage published"},
		{"a block of no release", func(e *kubeEnv, _ *Options) { e.f.values = map[string]any{"stowage": map[string]any{"registry": "x"}} }, "was not installed from a chart Stowage published"},
		{"another project", func(e *kubeEnv, _ *Options) {
			e.f.values["stowage"].(map[string]any)["release"].(map[string]any)["name"] = "other"
		}, `runs "other", not "notes"`},
		{"recorded input", func(_ *kubeEnv, o *Options) { o.Set = map[string]string{"ADMIN_EMAIL": "b@example.org"} }, "ADMIN_EMAIL: recorded at the install, and an upgrade keeps it"},
	} {
		e := newKubeEnv(t, spec.Provider{Name: "none"})
		e.runs("0.9.0", 1)
		e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
		err := e.installer(func(o *Options) { c.mod(e, o) }).K8sUpgrade(context.Background())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
		if len(e.f.applied) != 0 || len(e.f.ran("helm upgrade")) != 0 {
			t.Errorf("%s: changed the cluster", c.name)
		}
	}
}

func TestK8sUpgradeAsksOnlyWhatTheReleaseAdds(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.1.0", 1, func(s *spec.Spec) {
		s.Inputs = append(s.Inputs,
			spec.Input{Key: "REGION", Prompt: "Region", Default: "eu"},
			spec.Input{Key: "DOCKER_ONLY", Prompt: "x", Required: true, Shapes: []string{"docker"}},
			spec.Input{Key: "SUPPORT", Prompt: "x", Required: true, When: "tls"})
		s.Secrets = append(s.Secrets, spec.Secret{Key: "NEW_KEY", Gen: "hex16"})
	})
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
	e.vars = map[string]string{"ADMIN_EMAIL": "ignored@example.org"}
	if err := e.installer(func(o *Options) { o.DryRun = true }).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	out := e.out.String()
	if !strings.Contains(out, "  NEW_KEY=<redacted>\n  REGION=eu\n") || strings.Contains(out, "ADMIN_EMAIL") || strings.Contains(out, "SESSION_SECRET") || strings.Contains(out, "namespace notes: created") {
		t.Errorf("printed only what changes?\n%s", out)
	}
	if e.f.ran("helm upgrade") != nil {
		t.Error("dry run upgraded")
	}
}

func TestK8sFailedUpgradePointsAtStatus(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("0.9.0", 1)
	e.f.upErr = errors.New("UPGRADE FAILED")
	err := e.installer(nil).K8sUpgrade(context.Background())
	if err == nil || !strings.Contains(err.Error(), "helm rolled the release back to what it ran: `notes-stow k8s status --namespace notes --release notes` shows it") {
		t.Fatalf("%v", err)
	}
}

func TestK8sRollbackAndStatus(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("0.9.0", 2)
	in := e.installer(func(o *Options) { o.K8s.Revision, o.K8s.Timeout = 3, 6*time.Minute })
	if err := in.K8sRollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(e.f.ran("helm rollback")[0], " "); got != "helm rollback notes 3 --wait --timeout 6m0s --namespace notes" {
		t.Errorf("%s", got)
	}
	if !strings.Contains(e.out.String(), "release notes in notes runs notes 0.9.0 (revision 2)\n") {
		t.Errorf("out %s", e.out)
	}
	in.O.DryRun, in.O.K8s.Revision = true, 0
	e.f.history = []helmRevision{{Revision: 1, Status: "superseded", Chart: "notes-0.1.0"}, {Revision: 2, Status: "deployed", Chart: "notes-0.2.0"}}
	if err := in.K8sRollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(e.f.ran("helm rollback")[1], " "); got != "helm rollback notes 1 --wait --timeout 6m0s --dry-run --namespace notes" {
		t.Errorf("%s", got)
	}
	e.out.Reset()
	if err := in.K8sStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.out.String(); got != "notes 0.9.0 (revision 2, stable)\nregistry  "+e.prefix+"\nSTATUS: deployed\n" {
		t.Errorf("status %q", got)
	}
	e.f.listed = nil
	for _, f := range []func(context.Context) error{in.K8sRollback, in.K8sStatus} {
		if err := f(context.Background()); err == nil || !strings.Contains(err.Error(), "no release notes in notes") {
			t.Errorf("%v", err)
		}
	}
}

func TestK8sRollbackGoesBackOneUpgrade(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("0.9.0", 2)
	// rev: a revision of the chart notes-0.1.0 that ran the values named by its last field.
	rev := func(n int, status, desc, values string) helmRevision {
		return helmRevision{Revision: n, Status: status, Chart: "notes-0.1.0", Description: desc + "|" + values}
	}
	for _, c := range []struct {
		name    string
		history []helmRevision
		want    int // 0: refused
	}{
		{"after an upgrade", []helmRevision{rev(1, "superseded", "Install complete", "a"), rev(2, "superseded", "Upgrade complete", "b"), rev(3, "deployed", "Upgrade complete", "c")}, 2},
		{"after an upgrade that changed nothing", []helmRevision{rev(1, "superseded", "Install complete", "a"), rev(2, "superseded", "Upgrade complete", "b"), rev(3, "deployed", "Upgrade complete", "b")}, 1},
		{"after a failed upgrade helm rolled back", []helmRevision{rev(1, "superseded", "Install complete", "a"), rev(2, "superseded", "Upgrade complete", "b"), rev(3, "failed", "Upgrade failed", "x"), rev(4, "deployed", "Rollback to 2", "b")}, 1},
		{"after a rollback", []helmRevision{rev(1, "superseded", "Install complete", "a"), rev(2, "superseded", "Upgrade complete", "b"), rev(3, "superseded", "Upgrade complete", "c"), rev(4, "deployed", "Rollback to 2", "b")}, 1},
		{"after rollbacks of rollbacks", []helmRevision{rev(1, "superseded", "Install complete", "a"), rev(2, "superseded", "Upgrade complete", "b"), rev(3, "superseded", "Upgrade complete", "c"), rev(4, "superseded", "Rollback to 3", "c"), rev(5, "deployed", "Rollback to 4", "c")}, 2},
		{"a failed upgrade left as it is", []helmRevision{rev(1, "deployed", "Install complete", "a"), rev(2, "failed", "Upgrade failed", "x")}, 1},
		{"a failed upgrade after an upgrade", []helmRevision{rev(1, "superseded", "Install complete", "a"), rev(2, "deployed", "Upgrade complete", "b"), rev(3, "failed", "Upgrade failed", "x")}, 2},
		{"a failed rollback", []helmRevision{rev(1, "superseded", "Install complete", "a"), rev(2, "superseded", "Upgrade complete", "b"), rev(3, "deployed", "Upgrade complete", "c"), rev(4, "failed", "Rollback to 2", "b")}, 3},
		{"a failed rollback to the first install", []helmRevision{rev(1, "superseded", "Install complete", "a"), rev(2, "deployed", "Upgrade complete", "b"), rev(3, "failed", "Rollback to 1", "a")}, 2},
		{"another chart, the same values", []helmRevision{{Revision: 1, Status: "superseded", Chart: "notes-0.0.9", Description: "Install complete|b"}, rev(2, "deployed", "Upgrade complete", "b")}, 1},
		{"an uninstall kept in the history", []helmRevision{rev(1, "uninstalled", "Uninstallation complete", "a"), rev(2, "deployed", "Install complete", "b")}, 0},
		{"a copy of a revision helm no longer keeps", []helmRevision{rev(7, "superseded", "Upgrade complete", "a"), rev(8, "deployed", "Rollback to 3", "b")}, 0},
		{"a first install", []helmRevision{rev(1, "deployed", "Install complete", "a")}, 0},
		{"back to the first install", []helmRevision{rev(1, "superseded", "Install complete", "a"), rev(2, "deployed", "Rollback to 1", "a")}, 0},
		{"nothing deployed", []helmRevision{rev(1, "failed", "Install failed", "a")}, 0},
		{"numbers beyond a float64's precision", []helmRevision{rev(1, "superseded", "Install complete", "a"), rev(2, "superseded", "Upgrade complete", "b"), rev(3, "deployed", "Upgrade complete", "b")}, 2},
		{"uninstalled, its history kept", []helmRevision{rev(1, "superseded", "Install complete", "a"), rev(2, "uninstalled", "Uninstallation complete", "b")}, 2},
	} {
		e.f.history, e.f.revValues = nil, map[int]string{}
		big := c.name == "numbers beyond a float64's precision"
		for _, h := range c.history {
			desc, values, _ := strings.Cut(h.Description, "|")
			h.Description = desc
			e.f.history = append(e.f.history, h)
			// Each revision written its own way, as helm may: the same values compare equal.
			e.f.revValues[h.Revision] = fmt.Sprintf(`{"v": "%s", "same": 1, "n": 9007199254740993}`, values)
			if big && h.Revision == 3 {
				e.f.revValues[3] = `{"v": "b", "same": 1, "n": 9007199254740992}` // a float64 rounds it to rev 2's
			} else if h.Revision%2 == 0 {
				e.f.revValues[h.Revision] = fmt.Sprintf(`{"n":9007199254740993,"same":1,"v":"%s"}`, values)
			}
		}
		got, err := e.installer(nil).previousRevision(context.Background())
		if c.want == 0 {
			if err == nil || !strings.Contains(err.Error(), "helm history notes --namespace notes` lists them") {
				t.Errorf("%s: %d %v", c.name, got, err)
			}
		} else if err != nil || got != c.want {
			t.Errorf("%s: %d %v, want %d", c.name, got, err, c.want)
		}
	}
}

func TestK8sRollbackRefusesAPendingRelease(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("0.9.0", 2)
	e.f.listed[0].Status = "pending-upgrade"
	err := e.installer(func(o *Options) { o.K8s.Revision = 1 }).K8sRollback(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is pending-upgrade") || len(e.f.ran("helm rollback")) != 0 {
		t.Errorf("%v %q", err, e.f.ran("helm rollback"))
	}
}

func TestK8sRollbackRefusesANegativeRevision(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("0.9.0", 2)
	err := e.installer(func(o *Options) { o.K8s.Revision = -1 }).K8sRollback(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--revision -1: a helm revision is 1 or more") || len(e.f.ran("helm")) != 0 {
		t.Errorf("%v %q", err, e.f.ran("helm"))
	}
}

// publishInstaller puts version's installer and its release manifest in the registry.
func (e *kubeEnv) publish(version string, revision int) {
	t := e.t
	r, exe := testInstallerOf(t, version, revision, e.spec, func(m *release.Manifest) {
		m.Tag = fmt.Sprintf("%s-%d", version, revision)
		m.Artifacts.Chart = &release.Chart{Repository: "charts/notes-stable", Version: "0.1.0", Digest: e.digest}
	})
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	p := &pubEnv{env: &env{t: t}, prefix: e.prefix}
	m := *r.Manifest
	m.Artifacts.Installer = p.pushInstaller("notes-stow", raw)
	p.putRelease(m.Tag, &m)
}

// On Kubernetes the keys the installer run carries decide (not the release's values, which an
// operator can override): an upgrade's release must be signed by one of them.
func TestK8sUpgradeVersionNeedsASignatureByTheInstallersKeys(t *testing.T) {
	t.Parallel()
	k := testutil.NewKeySigner(t)
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.publish("2.0.0", 1)
	e.runs("1.0.0", 1)
	e.f.values["stowage"].(map[string]any)["publicKeys"] = []any{} // overridden values decide nothing
	in := e.installer(nil)
	in.R.Keys = map[string][]byte{"release.pub": k.PEM}
	in.O.Exec = func(_ context.Context, exe string, args []string) error {
		e.ran = append(e.ran, args)
		return nil
	}
	if err := in.K8sUpgradeTo(context.Background(), "2.0.0", nil); err == nil || !strings.Contains(err.Error(), "is not signed by a key this install trusts") {
		t.Fatalf("unsigned: %v", err)
	}
	in.R.Keys = nil
	e.f.values["stowage"].(map[string]any)["publicKeys"] = []any{string(k.PEM)}
	if err := in.K8sUpgradeTo(context.Background(), "2.0.0", nil); err == nil || !strings.Contains(err.Error(), "trusts signing keys this notes-stow does not carry") {
		t.Fatalf("an installer without keys: %v", err)
	}
	// An installer carrying a key the release no longer lists (dropped since) trusts it no more.
	dropped := testutil.NewKeySigner(t)
	in.R.Keys = map[string][]byte{"release.pub": dropped.PEM}
	p0 := &pubEnv{env: &env{t: t}, prefix: e.prefix}
	p0.signRelease("2.0.0-1", dropped)
	if err := in.K8sUpgradeTo(context.Background(), "2.0.0", nil); err == nil || !strings.Contains(err.Error(), "trusts signing keys this notes-stow does not carry") {
		t.Fatalf("a dropped key: %v", err)
	}
	in.R.Keys = map[string][]byte{"dropped.pub": dropped.PEM, "release.pub": k.PEM}
	if err := in.K8sUpgradeTo(context.Background(), "2.0.0", nil); err == nil || !strings.Contains(err.Error(), "is not signed by a key this install trusts") {
		t.Fatalf("signed by a dropped key the installer still carries: %v", err)
	}
	in.R.Keys = map[string][]byte{"release.pub": k.PEM}
	p := &pubEnv{env: &env{t: t}, prefix: e.prefix}
	p.signRelease("2.0.0-1", k)
	if err := in.K8sUpgradeTo(context.Background(), "2.0.0", nil); err != nil || len(e.ran) != 1 {
		t.Fatalf("signed: %v, ran %q", err, e.ran)
	}
}

func TestK8sUpgradeVersionRunsThatReleasesInstaller(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.publish("1.0.0", 1)
	e.publish("2.0.0", 1)
	e.publish("0.5.0", 1)
	e.runs("1.0.0", 1)
	in := e.installer(nil)
	in.O.Exec = func(_ context.Context, exe string, args []string) error {
		files, err := payload.Read(exe)
		if err != nil {
			return err
		}
		r, err := Load(files)
		if err != nil || r.Manifest.ProjectVersion != "2.0.0" {
			return fmt.Errorf("ran %v %v", r, err)
		}
		e.ran = append(e.ran, args)
		return nil
	}
	if err := in.K8sUpgradeTo(context.Background(), "2.0.0", []string{"--namespace=notes"}); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if len(e.ran) != 1 || !slices.Equal(e.ran[0], []string{"k8s", "upgrade", "--namespace=notes"}) {
		t.Fatalf("ran %q", e.ran)
	}
	e.f.listed[0].Status = "pending-rollback"
	e.log.Reset()
	if err := in.K8sUpgradeTo(context.Background(), "2.0.0", nil); err == nil || !strings.Contains(err.Error(), "is pending-rollback") || strings.Contains(e.log.String(), "fetching the installer") {
		t.Errorf("pending: %v\n%s", err, e.log)
	}
	e.f.listed[0].Status = "deployed"
	e.log.Reset()
	for v, want := range map[string]string{
		"1.0.0": "", // already runs
		"0.5.0": "release notes in notes runs notes 1.0.0 (revision 1), after notes 0.5.0 (revision 1)",
		"9.0.0": "notes 9.0.0 is not published on stable",
	} {
		err := in.K8sUpgradeTo(context.Background(), v, nil)
		if want == "" {
			if err != nil || !strings.Contains(e.out.String(), "release notes in notes already runs notes 1.0.0 (revision 1)") {
				t.Errorf("%s: %v %s", v, err, e.out)
			}
		} else if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", v, err, want)
		}
	}
	if len(e.ran) != 1 {
		t.Errorf("ran %q", e.ran)
	}
	// --unsafe-sysctls to the release it runs is not applied, and said so, naming that release's
	// installer, whichever this one is.
	on := true
	in.O.K8s.UnsafeSysctls = &on
	for _, run := range []string{"1.0.0", "2.0.0"} {
		e.runs(run, 1)
		e.out.Reset()
		if err := in.K8sUpgradeTo(context.Background(), run, nil); err != nil || !strings.Contains(e.out.String(), "--unsafe-sysctls is not applied: `notes-stow k8s upgrade --namespace notes --release notes --unsafe-sysctls=true`, without --version, by the installer of notes "+run+" (revision 1), applies it") {
			t.Errorf("%s: %v %s", run, err, e.out)
		}
	}
	// A release that already has what was asked is told nothing more.
	e.f.values["stowage"].(map[string]any)["sysctls"] = podSysctls()
	e.out.Reset()
	if err := in.K8sUpgradeTo(context.Background(), "2.0.0", nil); err != nil || strings.Contains(e.out.String(), "not applied") {
		t.Errorf("%v %s", err, e.out)
	}
}

// A stowage.publicKeys value of any shape decodes: only a list holding something lists keys.
func TestListedKeys(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]bool{``: false, `null`: false, `[]`: false, `""`: false, `["PEM"]`: true, `[1]`: true, `"none"`: false, `{"a":1}`: false, `false`: false} {
		var d deployed
		if err := json.Unmarshal([]byte(`{"publicKeys":`+cmp.Or(raw, "null")+`}`), &d); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got := len(listedKeys(d.PublicKeys)) > 0; got != want {
			t.Errorf("%s: %v", raw, got)
		}
	}
}

// sqlSchema gives the release the sql schema-compat provider and the Kubernetes schema check.
func sqlSchema(s *spec.Spec) {
	s.Database.SchemaCompat = spec.Provider{Name: "sql", Config: map[string]any{
		"current":           "SELECT max(version) FROM m",
		"barrier_after":     "SELECT string_agg(d, '; ') FROM m WHERE version > $1 AND d LIKE 'rollback barrier%'",
		"reads_newer_label": "com.example.reads-newer",
		"reads_newer_image": "api",
	}}
	s.Shapes.Kubernetes.Schema = &spec.K8sSchema{URLEnv: "DATABASE_URL", Image: "postgres"}
}

// The rollback reads the database in a pod before helm: across a barrier, or onto a release that
// does not read the newer schema, it is refused; a release reading it goes back.
func TestK8sRollbackChecksTheSchema(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, barrier, label, want string
		recorded                   string
		rolledBack                 bool
	}{
		{"a barrier since", "rollback barrier: x moved", "true", "the database carries rollback barrier: x moved, which removed something notes 0.9.0 (revision 1) may still use", "3", false},
		{"a release that predates the newer schema", "", "", "the database is at 5, and notes 0.9.0 (revision 1) predates reading a newer schema", "3", false},
		{"a release that reads it", "", "true", "", "3", true},
		{"nothing recorded", "rollback barrier: x moved", "", "", "", true},
		{"the schema did not move", "rollback barrier: x moved", "", "", "5", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newKubeEnv(t, spec.Provider{Name: "none"})
			e.release = e.at("1.0.0", 1, sqlSchema)
			// 0.9.0's api image, labelled as the case says, in its published release manifest.
			cl := registry.New(authn.NewMultiKeychain())
			img, _ := random.Image(64, 1)
			if c.label != "" {
				img, _ = mutate.Config(img, v1.Config{Labels: map[string]string{"com.example.reads-newer": c.label}})
			}
			h, _ := img.Digest()
			ref, _ := cl.Digest(e.prefix+"/notes-api", h.String())
			if err := remote.Write(ref, img); err != nil {
				t.Fatal(err)
			}
			old := e.at("0.9.0", 1, sqlSchema)
			old.Manifest.Artifacts.Images["api"] = h.String()
			e.published(old)
			e.runs("1.0.0", 1)
			e.f.manifest = "kind: Deployment\nmetadata: {name: notes-api}\nspec:\n  template:\n    spec:\n      imagePullSecrets: [{name: notes-stowage-pull}]\n      containers:\n        - name: api\n          image: " + e.prefix + "/notes-api@" + digest + "\n          env: [{name: DATABASE_URL, value: postgres://x}]\n"
			e.f.history = []helmRevision{{Revision: 1, Status: "superseded", Chart: "notes-0.1.0", Description: "Install complete"}, {Revision: 2, Status: "deployed", Chart: "notes-0.1.0", Description: "Upgrade complete"}}
			rel := func(v string) string {
				return `"release": {"name": "notes", "version": "` + v + `", "revision": 1, "channel": "stable", "tag": "` + v + `"}`
			}
			e.f.revValues = map[int]string{
				1: `{"stowage": {"registry": "` + e.prefix + `", ` + rel("0.9.0") + `}}`,
				2: `{"stowage": {"registry": "` + e.prefix + `", "schemaFrom": "` + c.recorded + `", ` + rel("1.0.0") + `}}`,
			}
			e.f.sql = func(q string) (string, string) {
				if strings.HasPrefix(q, "SELECT max") {
					return "Succeeded", "5\n"
				}
				return "Succeeded", c.barrier + "\n"
			}
			err := e.installer(func(o *Options) { o.K8s.Revision = 1 }).K8sRollback(context.Background())
			if rolled := len(e.f.ran("helm rollback")) == 1; rolled != c.rolledBack || (c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want))) || (c.want == "" && err != nil) {
				t.Fatalf("rolled back %v: %v\n%s", rolled, err, e.log)
			}
			if c.recorded != "" {
				if len(e.f.schemaQs) == 0 {
					t.Fatal("no schema pod ran")
				}
				if c.recorded == "3" && c.rolledBack && !slices.ContainsFunc(e.f.schemaQs, func(q string) bool { return strings.Contains(q, "version > 3") }) {
					t.Errorf("queries %q", e.f.schemaQs)
				}
			}
		})
	}
}

// An upgrade records the schema it read before helm migrates; one it could not read is recorded
// empty, so an earlier upgrade's value is never reused.
func TestK8sUpgradeRecordsTheSchema(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ phase, out, want string }{{"Succeeded", "7\n", `schemaFrom: "7"`}, {"Failed", "could not connect", `schemaFrom: ""`}} {
		e := newKubeEnv(t, spec.Provider{Name: "none"})
		e.release = e.at("1.0.0", 1, sqlSchema)
		e.published(e.release)
		e.runs("0.9.0", 1)
		e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
		e.f.manifest = "kind: Deployment\nmetadata: {name: notes-api}\nspec:\n  template:\n    spec:\n      containers:\n        - name: api\n          image: " + e.prefix + "/notes-api:1.0.0\n          envFrom: [{secretRef: {name: notes-db}}]\n"
		e.f.sql = func(string) (string, string) { return c.phase, c.out }
		if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
			t.Fatalf("%v\n%s", err, e.log)
		}
		if !strings.Contains(e.f.given[0], c.want) {
			t.Errorf("%s: values %q", c.phase, e.f.given[0])
		}
	}
}

// An installer of the release asked that fails with --unsafe-sysctls given says an older one does
// not take it; without the flag, nothing more is said.
func TestK8sUpgradeVersionWithUnsafeSysctlsToAnOlderInstaller(t *testing.T) {
	t.Parallel()
	on := true
	for _, flag := range []*bool{&on, nil} {
		e := newKubeEnv(t, spec.Provider{Name: "none"})
		e.publish("2.0.0", 1)
		e.runs("1.0.0", 1)
		in := e.installer(func(o *Options) { o.K8s.UnsafeSysctls = flag })
		in.O.Exec = func(context.Context, string, []string) error { return ExitCode(1) }
		err := in.K8sUpgradeTo(context.Background(), "2.0.0", []string{"--unsafe-sysctls"})
		noted := strings.Contains(e.log.String(), "note: if it refused --unsafe-sysctls as an unknown flag, it was made before it: run `notes-stow k8s upgrade --namespace notes --release notes --version 2.0.0` without it")
		if err == nil || noted != (flag != nil) {
			t.Errorf("--unsafe-sysctls %v: %v\n%s", flag, err, e.log)
		}
	}
}
