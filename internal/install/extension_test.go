package install

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
)

// withExtensions gives s an endpoint and five extensions: notes-search (with a key) and
// notes-ocr on inputs of every shape, notes-kube on an input of Kubernetes only, notes-dock on
// one of Docker only (the Docker tests leave it unrecorded), notes-set on one the tests set.
func withExtensions(s *spec.Spec) {
	s.Endpoint = "api"
	s.Inputs = append(s.Inputs,
		spec.Input{Key: "SEARCH_URL", Prompt: "x", Optional: true}, spec.Input{Key: "SEARCH_KEY", Prompt: "x", Optional: true, Secret: true},
		spec.Input{Key: "OCR_URL", Prompt: "x", Optional: true}, spec.Input{Key: "SET_URL", Prompt: "x", Optional: true},
		spec.Input{Key: "KUBE_URL", Prompt: "x", Optional: true, Shapes: []string{"kubernetes"}},
		spec.Input{Key: "DOCK_URL", Prompt: "x", Optional: true, Shapes: []string{"docker"}})
	s.Extensions = []spec.Extension{
		{Name: "notes-search", Input: "SEARCH_URL", KeyInput: "SEARCH_KEY", Prompt: "Search"},
		{Name: "notes-ocr", Input: "OCR_URL", Prompt: "Text in images"},
		{Name: "notes-kube", Input: "KUBE_URL", Prompt: "Cluster only"},
		{Name: "notes-set", Input: "SET_URL", Prompt: "Configured"},
		{Name: "notes-dock", Input: "DOCK_URL", Prompt: "Docker only"},
	}
}

// extensionInstall is an install of a release with extensions, its registry holding a release of
// notes-search on stable (tagged 2.0.0-1) and none of the others.
func extensionInstall(t *testing.T) *pubEnv {
	p := published(t, spec.Provider{Name: "none"}, nil)
	withExtensions(p.r.Spec)
	env := p.readEnv()
	for k, v := range map[string]string{"SEARCH_URL": "", "SEARCH_KEY": "", "OCR_URL": "", "SET_URL": "http://set:80"} {
		env.Set(k, v)
	}
	if err := env.WriteFile(filepath.Join(p.dir, envName)); err != nil {
		t.Fatal(err)
	}
	m := *p.r.Manifest
	m.Name, m.Tag = "notes-search", "2.0.0-1"
	putRelease(t, p.prefix+"/notes-search-release", "stable", &m)
	return p
}

func TestFinishedPrintsTheEndpointAndATipPerExtensionOff(t *testing.T) {
	t.Parallel()
	p := extensionInstall(t)
	in := p.installer(nil)
	in.Finished(context.Background(), onDocker)
	out := p.out.String()
	url := get(p.readEnv(), "STOWAGE_URL_API")
	ctl, dir := filepath.Join(p.dir, "notes-stow"), p.dir
	for _, want := range []string{
		"endpoint  " + url + " (loopback: this machine only; another compose project's containers cannot reach it, a project using notes as an extension reaches it through a domain: a new install with --tls)\n",
		"tip: the extension notes-search (Search) is off: SEARCH_URL is empty\n" +
			"  install it: docker run --rm " + p.prefix + "/notes-search-installer:2.0.0-1 > notes-search.sh && sh notes-search.sh\n" +
			"  then give its endpoint: " + ctl + " --dir " + dir + " --set SEARCH_URL=<its endpoint> --answers <a file: SEARCH_KEY: <its key>>\n",
		"tip: the extension notes-ocr (Text in images) is off: OCR_URL is empty\n" +
			"  " + p.prefix + "/notes-ocr-installer is not published on the stable channel\n" +
			"  install it: docker run --rm " + p.prefix + "/notes-ocr-installer:latest > notes-ocr.sh && sh notes-ocr.sh\n" +
			"  then give its endpoint: " + ctl + " --dir " + dir + " --set OCR_URL=<its endpoint>\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "notes-kube") || strings.Contains(out, "notes-set") || strings.Count(out, "not published") != 1 {
		t.Errorf("a tip for an extension configured, or not of this shape:\n%s", out)
	}
	// Behind a TLS front, the URL is the one another project is given.
	env := p.readEnv()
	env.Set("STOWAGE_TLS_MODE", "internal")
	env.Set("STOWAGE_URL_API", "https://api.notes.test")
	if err := env.WriteFile(filepath.Join(p.dir, envName)); err != nil {
		t.Fatal(err)
	}
	p.out.Reset()
	in.Finished(context.Background(), onDocker)
	if !strings.Contains(p.out.String(), "endpoint  https://api.notes.test (the URL a project using notes as an extension is given)\n") {
		t.Errorf("TLS:\n%s", p.out)
	}
	url = "https://api.notes.test"
	// status shows the endpoint recorded.
	p.out.Reset()
	if err := in.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.out.String(), "endpoint  "+url+"\n") {
		t.Errorf("status:\n%s", p.out)
	}
	// On the VM the re-run is the VM's installer's.
	p.out.Reset()
	in.Finished(context.Background(), onVM)
	if want := "  install it: docker run --rm " + p.prefix + "/notes-ocr-installer:latest > notes-ocr.sh && sh notes-ocr.sh (Docker) or sh notes-ocr.sh vm create (a VM)\n" +
		"  then give its endpoint: sudo notes-stow install --set OCR_URL=<its endpoint>\n"; !strings.Contains(p.out.String(), want) {
		t.Errorf("no %q in:\n%s", want, p.out)
	}
}

func TestFinishedSaysNothingOfTheRegistryUnlessItAnswers(t *testing.T) {
	t.Parallel()
	p := extensionInstall(t)
	// Offline: the registry is not asked.
	n := len(p.paths)
	p.installer(func(o *Options) { o.Offline = true }).Finished(context.Background(), onDocker)
	if len(p.paths) != n || strings.Contains(p.out.String(), "not published") || !strings.Contains(p.out.String(), "notes-search-installer:latest") {
		t.Errorf("offline: %d requests\n%s", len(p.paths)-n, p.out)
	}
	// A dry run prints nothing.
	p.out.Reset()
	p.installer(func(o *Options) { o.DryRun = true }).Finished(context.Background(), onDocker)
	if p.out.Len() != 0 {
		t.Errorf("dry run: %s", p.out)
	}
	// A registry refusing the reads: the tips, nothing said of what it publishes.
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "denied", http.StatusForbidden) }))
	t.Cleanup(refused.Close)
	env := p.readEnv()
	env.Set("STOWAGE_REGISTRY", strings.TrimPrefix(refused.URL, "http://")+"/acme/notes")
	if err := env.WriteFile(filepath.Join(p.dir, envName)); err != nil {
		t.Fatal(err)
	}
	p.out.Reset()
	p.installer(nil).Finished(context.Background(), onDocker)
	if out := p.out.String(); strings.Contains(out, "not published") || strings.Count(out, "tip: ") != 2 || !strings.Contains(out, "notes-search-installer:latest") {
		t.Errorf("refused:\n%s", out)
	}
}

func TestFinishedBoundsTheRegistryCheck(t *testing.T) {
	p := extensionInstall(t)
	defer func(d time.Duration) { extensionCheckTime = d }(extensionCheckTime)
	extensionCheckTime = 50 * time.Millisecond
	stuck := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(stuck.Close)
	env := p.readEnv()
	env.Set("STOWAGE_REGISTRY", strings.TrimPrefix(stuck.URL, "http://")+"/acme/notes")
	if err := env.WriteFile(filepath.Join(p.dir, envName)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	p.installer(nil).Finished(context.Background(), onDocker)
	if d := time.Since(start); d > 2*time.Second || strings.Count(p.out.String(), "tip: ") != 2 || strings.Contains(p.out.String(), "not published") {
		t.Errorf("after %s:\n%s", d, p.out)
	}
}

func TestK8sDeployPrintsTheServiceEndpointAndTips(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, withExtensions)
	e.published(e.release)
	e.vars["SET_URL"] = "http://set:80"
	if err := e.installer(nil).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	out := e.out.String()
	for _, want := range []string{
		"endpoint  http://notes-api.notes.svc:8000 (the URL a project using notes as an extension is given)\n",
		"  install it: docker run --rm " + e.prefix + "/notes-search-installer:latest > notes-search.sh && sh notes-search.sh k8s install\n" +
			"  then give its endpoint: the release takes it at a new install: `helm uninstall notes --namespace notes` (its Secret is kept), then SEARCH_KEY=<its key> notes-stow k8s install --namespace notes --release notes --set SEARCH_URL=<its endpoint>\n",
		"tip: the extension notes-kube (Cluster only) is off: KUBE_URL is empty\n" +
			"  " + e.prefix + "/notes-kube-installer is not published on the stable channel\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "notes-set") || strings.Contains(out, "notes-dock") {
		t.Errorf("a tip for an extension configured, or not of this shape:\n%s", out)
	}
	e.runs("1.0.0", 1)
	e.out.Reset()
	if err := e.installer(nil).K8sStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "\nendpoint  http://notes-api.notes.svc:8000\n") {
		t.Errorf("status %q", e.out)
	}
}

// putRelease puts m in the registry's repo under tag.
func putRelease(t *testing.T, repo, tag string, m *release.Manifest) {
	t.Helper()
	data, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	c := registry.New(authn.NewMultiKeychain())
	ref, err := c.Tag(repo, tag)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PutArtifact(context.Background(), ref, registry.Artifact{Type: release.MediaType, Data: data}); err != nil {
		t.Fatal(err)
	}
}

// The release runs once helm settled it: its endpoint and tips are printed when the hook fails too.
func TestTheEndpointIsPrintedWhenTheHookFails(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, withExtensions)
	e.published(e.release)
	e.hookRuns("Failed", "boom", nil)
	err := e.installer(func(o *Options) { o.NoProvision = false }).K8sInstall(context.Background())
	if err == nil || !strings.Contains(err.Error(), "provisioning did not finish") {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(e.out.String(), "endpoint  http://notes-api.notes.svc:8000") {
		t.Errorf("out %s", e.out)
	}
}
