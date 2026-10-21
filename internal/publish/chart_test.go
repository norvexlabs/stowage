package publish

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/helm"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// chartAt reads the chart under charts/notes-<channel>:<version>: its manifest's media types and
// the files of its archive.
func (e *env) chartAt(channel, version string) map[string]string {
	e.t.Helper()
	ref, _ := name.NewTag(e.host + "/acme/notes/charts/notes-" + channel + ":" + version)
	d, err := remote.Get(ref)
	if err != nil {
		e.t.Fatal(err)
	}
	var m struct {
		ArtifactType string `json:"artifactType"`
		Config       struct{ MediaType string }
		Layers       []struct {
			MediaType string
			Digest    string
		}
	}
	if err := json.Unmarshal(d.Manifest, &m); err != nil {
		e.t.Fatal(err)
	}
	if m.ArtifactType != "" || m.Config.MediaType != helm.ConfigMediaType || len(m.Layers) != 1 || m.Layers[0].MediaType != helm.ContentMediaType {
		e.t.Fatalf("chart manifest: %s", d.Manifest)
	}
	l, err := remote.Layer(ref.Context().Digest(m.Layers[0].Digest))
	if err != nil {
		e.t.Fatal(err)
	}
	rc, err := l.Compressed()
	if err != nil {
		e.t.Fatal(err)
	}
	defer rc.Close()
	zr, err := gzip.NewReader(rc)
	if err != nil {
		e.t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			e.t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			e.t.Fatal(err)
		}
		files[h.Name] = string(b)
	}
}

// chartValues decodes the stowage block of a chart's values.yaml.
func chartValues(t *testing.T, files map[string]string) helm.Values {
	t.Helper()
	var v struct{ Stowage helm.Values }
	if err := yaml.Unmarshal([]byte(files["notes/values.yaml"]), &v); err != nil {
		t.Fatal(err)
	}
	return v.Stowage
}

func TestShipPublishesTheChart(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.mustShip("stable", 1)
	c := r.Manifest.Artifacts.Chart
	if c == nil || c.Repository != "charts/notes-stable" || c.Version != "0.1.0" {
		t.Fatalf("chart = %+v", c)
	}
	eq(t, "chart tag", e.digest("charts/notes-stable", "0.1.0"), c.Digest)
	files := e.chartAt("stable", "0.1.0")
	var meta map[string]any
	if err := yaml.Unmarshal([]byte(files["notes/Chart.yaml"]), &meta); err != nil {
		t.Fatal(err)
	}
	eq(t, "chart version", meta["version"], "0.1.0")
	eq(t, "appVersion", meta["appVersion"], "1.0.0")
	if _, ok := files["notes/templates/web.yaml"]; !ok {
		t.Fatalf("templates not packaged: %v", files)
	}
	v := chartValues(t, files)
	eq(t, "registry", v.Registry, e.host+"/acme/notes")
	eq(t, "release", v.Release, helm.Release{Name: "notes", Version: "1.0.0", Revision: 1, Channel: "stable", Tag: "1.0.0"})
	eq(t, "images", len(v.Images), len(r.Manifest.Artifacts.Images))
	for k, d := range r.Manifest.Artifacts.Images {
		eq(t, "image "+k, v.Images[k], helm.Image{Repository: "notes-" + k, Digest: d})
	}
	eq(t, "no keys for an unsigned project", v.PublicKeys == nil, true)

	e.log.Reset()
	again := e.mustShip("stable", 1)
	eq(t, "the same release makes the same chart", again.Manifest.Artifacts.Chart.Digest, c.Digest)
	if strings.Contains(e.log.String(), "chart:") {
		t.Fatalf("a re-ship wrote the chart:\n%s", e.log)
	}

	// Each channel has its own repository and numbering: the next beta release is 0.2.0 there.
	beta := e.mustShip("beta", 1)
	bc := beta.Manifest.Artifacts.Chart
	eq(t, "beta chart", bc.Repository+":"+bc.Version, "charts/notes-beta:0.1.0")
	e.bump("1.1.0")
	next := e.mustShip("beta", 1).Manifest.Artifacts.Chart
	eq(t, "next beta chart", next.Repository+":"+next.Version, "charts/notes-beta:0.2.0")
	eq(t, "appVersion follows", strings.Contains(e.chartAt("beta", "0.2.0")["notes/Chart.yaml"], `appVersion: "1.1.0"`), true)
	eq(t, "the beta chart's release", chartValues(t, e.chartAt("beta", "0.1.0")).Release.Tag, "beta-1.0.0")
}

// The chart is part of the release: a missing or moved chart tag is reported by verify and put back
// by a re-ship.
func TestAMissingChartTagIsPutBack(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	r := e.mustShip("stable", 1)
	tag, _ := e.reg.Tag(e.host+"/acme/notes/charts/notes-stable", "0.1.0")
	if err := e.reg.DeleteTag(ctx, tag); err != nil {
		t.Fatal(err)
	}
	p, _ := e.publisher()
	if _, err := p.Verify(ctx, "stable", "", 1); err == nil || !strings.Contains(err.Error(), "charts/notes-stable:0.1.0 is missing") {
		t.Fatalf("verify of a release without its chart tag: %v", err)
	}
	again := e.mustShip("stable", 1)
	eq(t, "release kept", again.Digest, r.Digest)
	eq(t, "chart tag back", e.digest("charts/notes-stable", "0.1.0"), r.Manifest.Artifacts.Chart.Digest)

	// Moved: the chart tag now holds another manifest of the repository.
	moved := mustPushOther(t, e)
	if err := e.reg.Retag(ctx, moved, tag); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Verify(ctx, "stable", "", 1); err == nil || !strings.Contains(err.Error(), "the release says "+r.Manifest.Artifacts.Chart.Digest) {
		t.Fatalf("verify of a moved chart tag: %v", err)
	}
}

// mustPushOther pushes another chart manifest to charts/notes-stable, by digest.
func mustPushOther(t *testing.T, e *env) name.Digest {
	t.Helper()
	p, _ := e.publisher()
	ptr, err := p.pointer(context.Background(), "stable")
	if err != nil || ptr == nil {
		t.Fatalf("pointer: %v", err)
	}
	m := *ptr.Manifest
	m.ReleaseSeq = 7
	cm, err := p.chartManifest(&m)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := e.reg.Repo(e.host + "/acme/notes/charts/notes-stable")
	h, err := e.reg.PutManifest(context.Background(), repo, cm, nil)
	if err != nil {
		t.Fatal(err)
	}
	return repo.Digest(h.String())
}

// A published release naming another chart than the one it makes (a release of an older stowage,
// or of none) is never rewritten on an immutable channel; a mutable one re-publishes it under its
// number.
func TestAChartThatWouldDifferIsRefusedOnAnImmutableChannel(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	// rewrite publishes r with the chart set by edit, and the installer that manifest makes, so
	// only the chart differs from what a ship makes.
	rewrite := func(r *Ref, edit func(*Ref) string) string {
		t.Helper()
		m := *r.Manifest
		m.Artifacts.Chart = nil
		if d := edit(r); d != "" {
			c := *r.Manifest.Artifacts.Chart
			c.Digest = d
			m.Artifacts.Chart = &c
		}
		p, _ := e.publisher()
		img, err := p.installerImage(&m)
		if err != nil {
			t.Fatal(err)
		}
		if m.Artifacts.Installer, err = p.publishInstaller(ctx, img, m.Tag); err != nil {
			t.Fatal(err)
		}
		if _, err := p.writeRelease(ctx, m.Tag, &m); err != nil {
			t.Fatal(err)
		}
		if m.Artifacts.Chart == nil {
			return "none"
		}
		return m.Artifacts.Chart.Digest
	}
	stable := e.mustShip("stable", 1)
	for name, edit := range map[string]func(*Ref) string{
		"another chart": func(r *Ref) string { return r.Manifest.Artifacts.Images["web"] },
		"no chart":      func(*Ref) string { return "" },
	} {
		had := rewrite(stable, edit)
		_, err := e.ship("stable", 1)
		if err == nil || !strings.Contains(err.Error(), "with the chart "+had+", and it would now be "+stable.Manifest.Artifacts.Chart.Digest) {
			t.Fatalf("%s: an immutable release with another chart: %v", name, err)
		}
	}
	beta := e.mustShip("beta", 1)
	rewrite(beta, func(r *Ref) string { return r.Manifest.Artifacts.Images["web"] })
	again := e.mustShip("beta", 1)
	eq(t, "chart", *again.Manifest.Artifacts.Chart, *beta.Manifest.Artifacts.Chart)
	eq(t, "number kept", again.Manifest.ReleaseSeq, beta.Manifest.ReleaseSeq)
}

func TestPruneDeletesTheChartTag(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mustShip("beta", 1)
	e.bump("1.1.0")
	e.mustShip("beta", 1)
	p, _ := e.publisher()
	if _, err := p.Prune(context.Background(), "beta", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	eq(t, "pruned chart tag", e.digest("charts/notes-beta", "0.1.0"), "")
	if e.digest("charts/notes-beta", "0.2.0") == "" {
		t.Fatal("prune deleted the pointer release's chart")
	}
}

// A promoted release gets its own chart in the target channel's repository, numbered there.
func TestPromotePublishesTheChartOnTheTargetChannel(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mustShip("beta", 1)
	e.bump("1.1.0")
	beta := e.mustShip("beta", 1)
	p, v := e.publisher()
	r, err := p.Promote(context.Background(), v, "beta", "stable", 1)
	if err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	c := r.Manifest.Artifacts.Chart
	eq(t, "chart", c.Repository+":"+c.Version, "charts/notes-stable:0.1.0")
	vals := chartValues(t, e.chartAt("stable", "0.1.0"))
	eq(t, "channel", vals.Release.Channel, "stable")
	eq(t, "the promoted images", vals.Images["web"].Digest, beta.Manifest.Artifacts.Images["web"])
}

// A project without the kubernetes shape publishes no chart.
func TestNoKubernetesShapeNoChart(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	testutil.SetYAML(t, e.root, "stowage.yaml", []string{"shapes"}, map[string]any{"docker": map[string]any{}})
	r := e.mustShip("stable", 1)
	eq(t, "chart", r.Manifest.Artifacts.Chart == nil, true)
	eq(t, "chart tag", e.digest("charts/notes-stable", "0.1.0"), "")
	eq(t, "re-ship", e.mustShip("stable", 1).Digest, r.Digest)
}

// A publication that fails after the chart is pushed leaves no chart version: its tag moves only
// once the release manifest naming it is written.
func TestAFailedPublicationLeavesNoChartVersion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.sign()
	e.signer.downOn = "notes-installer"
	if _, err := e.ship("stable", 1); err == nil {
		t.Fatal("ship passed a failed installer")
	}
	eq(t, "chart tag", e.digest("charts/notes-stable", "0.1.0"), "")
	e.signer.downOn = ""
	r := e.mustShip("stable", 1)
	eq(t, "the next number", r.Manifest.Artifacts.Chart.Version, "0.2.0")
	eq(t, "chart tag", e.digest("charts/notes-stable", "0.2.0"), r.Manifest.Artifacts.Chart.Digest)
}

// A mutable release rebuilt with new content is a new release number, so a new chart version; the
// replaced build's version is removed with it.
func TestARebuiltMutableReleaseRemovesTheReplacedChart(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := e.mustShip("beta", 1)
	e.changeWeb()
	r := e.mustShip("beta", 1)
	eq(t, "new number", r.Manifest.Artifacts.Chart.Version, "0.2.0")
	eq(t, "replaced chart removed", e.digest("charts/notes-beta", first.Manifest.Artifacts.Chart.Version), "")
	eq(t, "new chart", e.digest("charts/notes-beta", "0.2.0"), r.Manifest.Artifacts.Chart.Digest)
	p, _ := e.publisher()
	if _, err := p.Verify(context.Background(), "beta", "", 1); err != nil {
		t.Fatal(err)
	}
}

// A registry that refuses to delete the replaced build's chart version fails nothing: the release
// is published and the channel settled first, and the version left is named in a warning.
func TestARefusedChartDeletionIsAWarning(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mustShip("beta", 1)
	e.changeWeb()
	e.failDelete = "charts/notes-beta/manifests/0.1.0"
	r := e.mustShip("beta", 1)
	if !strings.Contains(e.log.String(), "notes-beta:0.1.0 names no release and is left") {
		t.Fatalf("no warning:\n%s", e.log)
	}
	eq(t, "pointer on the new build", e.digest("notes-release", "beta"), r.Digest.String())
	p, _ := e.publisher()
	if _, err := p.Verify(context.Background(), "beta", "", 1); err != nil {
		t.Fatal(err)
	}
}

// tagChartAs tags a channel's published chart digest under another version, as a run stopped
// before its sweep would leave one.
func (e *env) tagChartAs(channel, digest, version string) {
	e.t.Helper()
	repo := e.host + "/acme/notes/charts/notes-" + channel
	d, _ := e.reg.Digest(repo, digest)
	t, _ := e.reg.Tag(repo, version)
	if err := e.reg.Retag(context.Background(), d, t); err != nil {
		e.t.Fatal(err)
	}
}

// A chart version no release names is swept from a mutable channel by its next ship and by prune;
// an immutable channel's chart repository is never swept.
func TestUnnamedChartVersionsAreSwept(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	beta := e.mustShip("beta", 1)
	e.tagChartAs("beta", beta.Manifest.Artifacts.Chart.Digest, "0.9.0")
	e.mustShip("beta", 1)
	eq(t, "swept by ship", e.digest("charts/notes-beta", "0.9.0"), "")
	eq(t, "named kept", e.digest("charts/notes-beta", "0.1.0"), beta.Manifest.Artifacts.Chart.Digest)

	e.tagChartAs("beta", beta.Manifest.Artifacts.Chart.Digest, "0.8.0")
	e.bump("1.1.0")
	e.mustShip("beta", 1)
	e.tagChartAs("beta", beta.Manifest.Artifacts.Chart.Digest, "0.7.0")
	p, _ := e.publisher()
	if _, err := p.Prune(context.Background(), "beta", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	eq(t, "swept by prune", e.digest("charts/notes-beta", "0.7.0"), "")

	stable := e.mustShip("stable", 1)
	e.tagChartAs("stable", stable.Manifest.Artifacts.Chart.Digest, "0.9.0")
	e.mustShip("stable", 1)
	eq(t, "an immutable channel is not swept", e.digest("charts/notes-stable", "0.9.0"), stable.Manifest.Artifacts.Chart.Digest)
}

// The chart of the release the channel pointer holds is kept even when its tag was rewritten
// since (a rebuild stopped before the pointer moved): the pointer still offers it.
func TestThePointersChartIsNeverSwept(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	a := e.mustShip("beta", 1)
	m := *a.Manifest
	c := *m.Artifacts.Chart
	c.Version = "0.2.0"
	m.ReleaseSeq, m.Artifacts.Chart = 2, &c
	p, _ := e.publisher()
	if _, err := p.writeRelease(ctx, m.Tag, &m); err != nil {
		t.Fatal(err)
	}
	p.sweepCharts(ctx, "beta")
	eq(t, "the pointer's chart", e.digest("charts/notes-beta", "0.1.0"), a.Manifest.Artifacts.Chart.Digest)
}

// The sweep deletes chart versions only: a signature's fallback tag, or any other tag, is left.
// And it stops, deleting nothing, when a manifest of the repository cannot be read: that
// release's chart cannot be told unnamed.
func TestTheSweepDeletesOnlyChartVersionsItCanTellUnnamed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	beta := e.mustShip("beta", 1)
	d := beta.Manifest.Artifacts.Chart.Digest
	for _, tag := range []string{"sha256-" + strings.Repeat("a", 64), "pinned", "1.0.0", "0.01.0"} {
		e.tagChartAs("beta", d, tag)
	}
	e.tagChartAs("beta", d, "0.9.0")
	p, _ := e.publisher()
	p.sweepCharts(ctx, "beta")
	for _, tag := range []string{"sha256-" + strings.Repeat("a", 64), "pinned", "1.0.0", "0.01.0"} {
		eq(t, "tag "+tag+" kept", e.digest("charts/notes-beta", tag), d)
	}
	eq(t, "unnamed version swept", e.digest("charts/notes-beta", "0.9.0"), "")

	e.tagChartAs("beta", d, "0.9.0")
	foreign, _ := e.reg.Tag(e.host+"/acme/notes/notes-release", "from-a-newer-stowage")
	if _, err := e.reg.PutArtifact(ctx, foreign, registry.Artifact{Type: "application/vnd.example.other+json", Data: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	e.log.Reset()
	p.sweepCharts(ctx, "beta")
	eq(t, "nothing swept past an unreadable manifest", e.digest("charts/notes-beta", "0.9.0"), d)
	eq(t, "warned", strings.Contains(e.log.String(), "warning: chart versions of"), true)
}

// The chart's values give each L4 route its protocol as a Service names it and its upstream's
// service and port.
func TestL4Values(t *testing.T) {
	t.Parallel()
	got, err := l4Values([]spec.L4Route{{Name: "data", Protocol: "udp", Port: 443, Upstream: "relay:4430"}})
	if err != nil || len(got) != 1 || got[0] != (helm.L4Route{Name: "data", Protocol: "UDP", Port: 443, Service: "relay", TargetPort: 4430}) {
		t.Fatalf("%+v %v", got, err)
	}
}
