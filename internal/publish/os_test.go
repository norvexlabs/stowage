package publish

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/norvexlabs/stowage/internal/kairos"
	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// TestMain lets the bundle build in the tests' temporary directories, which may be in memory:
// the fake builder writes kilobytes.
func TestMain(m *testing.M) {
	kairos.OnDisk = func(string) error { return nil }
	os.Exit(m.Run())
}

// untar reads a tar's regular files.
func untar(t *testing.T, raw []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out[h.Name] = b
	}
}

func TestShipPublishesTheOSBundleAndItsDisks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.mustShip("stable", 1)
	m := r.Manifest
	eq(t, "os tag", e.digest("notes-os", "1.0.0"), m.Artifacts.OS)
	eq(t, "disks", len(m.Artifacts.Disks), 1)
	eq(t, "disk tag", e.digest("notes-disk", "1.0.0-qcow2"), m.Artifacts.Disks["qcow2"])

	// The bundle: the rendered recipe, built with a context holding the files and one archive per
	// image, the image as published, named by its digest.
	osb := e.builder.built[len(e.builder.built)-1]
	eq(t, "bundle built last", osb.Image, "os")
	p, _ := e.publisher()
	k := p.Project.Spec.Shapes.Kairos
	df, _ := kairos.Dockerfile(kairos.Params{Name: "notes", Ctl: "notes-stow", Base: k.Base, KairosInit: k.KairosInit, OSVersion: "1.0.0"})
	eq(t, "recipe", string(osb.Recipe), string(df))
	if e.builder.context["files/usr/sbin/notes-preload"] == nil {
		t.Errorf("context lacks the bundle's files: %v", slices.Sorted(func(yield func(string) bool) {
			for k := range e.builder.context {
				if !yield(k) {
					return
				}
			}
		}))
	}
	for img, d := range m.Artifacts.Images {
		archive := untar(t, e.builder.context["preload/"+img+".tar"])
		var idx v1.IndexManifest
		if err := json.Unmarshal(archive["index.json"], &idx); err != nil {
			t.Fatalf("%s: index: %v", img, err)
		}
		if len(idx.Manifests) != 1 {
			t.Fatalf("%s: %d manifests", img, len(idx.Manifests))
		}
		desc := idx.Manifests[0]
		eq(t, img+" archive manifest", desc.Digest.String(), m.Artifacts.Manifests[img])
		eq(t, img+" archive name", desc.Annotations["io.containerd.image.name"], e.host+"/acme/notes/notes-"+img+":"+release.PreloadTag(d))
		eq(t, img+" archive tag", desc.Annotations["org.opencontainers.image.ref.name"], release.PreloadTag(d))
		var man v1.Manifest
		if err := json.Unmarshal(archive["blobs/sha256/"+desc.Digest.Hex], &man); err != nil {
			t.Fatalf("%s: manifest blob: %v", img, err)
		}
		for _, l := range append([]v1.Descriptor{man.Config}, man.Layers...) {
			b := archive["blobs/sha256/"+l.Digest.Hex]
			if h, n, _ := v1.SHA256(bytes.NewReader(b)); h != l.Digest || n != l.Size {
				t.Errorf("%s: blob %s missing or altered", img, l.Digest)
			}
		}
		eq(t, img+" layout", string(archive["oci-layout"]), `{"imageLayoutVersion":"1.0.0"}`)
	}

	// <short>-stow: the installer executable with the release as the bundle carries it — no
	// publication, and a spec without what is made from the bundle.
	ctl := e.builder.context["ctl/notes-stow"]
	if !bytes.HasPrefix(ctl, p.Installer) {
		t.Fatal("ctl/notes-stow is not the installer executable")
	}
	exe := filepath.Join(t.TempDir(), "notes-stow")
	if err := os.WriteFile(exe, ctl, 0o755); err != nil {
		t.Fatal(err)
	}
	files, err := payload.Read(exe)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[payload.Release]; ok {
		t.Error("the bundle's ctl carries a published manifest")
	}
	bm, err := release.DecodeBundled(files[payload.Bundled])
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "bundled version", bm.ProjectVersion+"-"+strconv.Itoa(bm.Revision), "1.0.0-1")
	eq(t, "bundled images", fmt.Sprint(bm.Artifacts.Images), fmt.Sprint(m.Artifacts.Images))
	eq(t, "bundled configs", fmt.Sprint(bm.Artifacts.Configs), fmt.Sprint(m.Artifacts.Configs))
	eq(t, "bundled manifests", fmt.Sprint(bm.Artifacts.Manifests), fmt.Sprint(m.Artifacts.Manifests))
	eq(t, "bundled changelog", bm.Changelog.Project, m.Changelog.Project)
	var view spec.Spec
	if err := json.Unmarshal(files[payload.Spec], &view); err != nil {
		t.Fatal(err)
	}
	if view.Shapes.Kairos == nil || view.Shapes.Kairos.DiskFormats != nil || view.Shapes.Kairos.Base != k.Base {
		t.Errorf("the bundled spec's kairos shape: %+v", view.Shapes.Kairos)
	}

	// The disk: made from the bundle by digest, pushed as an artifact titled with its file.
	eq(t, "disks made", len(e.disks.made), 1)
	ref, _ := name.NewDigest(e.host + "/acme/notes/notes-disk@" + m.Artifacts.Disks["qcow2"])
	desc, err := remote.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	var dm v1.Manifest
	if err := json.Unmarshal(desc.Manifest, &dm); err != nil {
		t.Fatal(err)
	}
	eq(t, "artifact type", string(dm.ArtifactType), release.DiskArtifactType)
	eq(t, "format", dm.Annotations[FormatAnnotation], "qcow2")
	eq(t, "layers", len(dm.Layers), 1)
	eq(t, "media type", string(dm.Layers[0].MediaType), "application/vnd.stowage.disk.qcow2")
	eq(t, "title", dm.Layers[0].Annotations["org.opencontainers.image.title"], "notes-1.0.0.qcow2")
	layer, err := remote.Layer(ref.Context().Digest(dm.Layers[0].Digest.String()))
	if err != nil {
		t.Fatal(err)
	}
	rc, _ := layer.Compressed()
	content, _ := io.ReadAll(rc)
	rc.Close()
	eq(t, "disk content", string(content), "qcow2 of "+e.host+"/acme/notes/notes-os@"+m.Artifacts.OS)
	eq(t, "latest-qcow2", e.digest("notes-disk", "latest-qcow2"), m.Artifacts.Disks["qcow2"])

	again := e.mustShip("stable", 1)
	eq(t, "rebuilt", len(e.builder.built), 0)
	eq(t, "disks made again", len(e.disks.made), 0)
	eq(t, "release", again.Digest, r.Digest)

	// Lost tags are put back from the release, without building.
	ctx := context.Background()
	for _, rt := range [][2]string{{"notes-os", "1.0.0"}, {"notes-disk", "1.0.0-qcow2"}} {
		tag, _ := e.reg.Tag(e.host+"/acme/notes/"+rt[0], rt[1])
		if err := e.reg.DeleteTag(ctx, tag); err != nil {
			t.Fatal(err)
		}
	}
	eq(t, "release after a lost tag", e.mustShip("stable", 1).Digest, r.Digest)
	eq(t, "rebuilt after a lost tag", len(e.builder.built)+len(e.disks.made), 0)
	eq(t, "os tag back", e.digest("notes-os", "1.0.0"), m.Artifacts.OS)
	eq(t, "disk tag back", e.digest("notes-disk", "1.0.0-qcow2"), m.Artifacts.Disks["qcow2"])
}

// The bundle names no channel: a promotion re-tags it and its disks.
func TestPromoteRetagsTheBundleAndDisks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	beta := e.mustShip("beta", 1)
	// A promotion builds nothing: it needs no pull account for the disk builder.
	e.pullAuth = func(authn.Resource) (authn.AuthConfig, error) { return authn.AuthConfig{}, errors.New("no pull token") }
	p, v := e.publisher()
	e.builder.built, e.disks.made = nil, nil
	st, err := p.Promote(context.Background(), v, "beta", "stable", 1)
	if err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	eq(t, "built", len(e.builder.built)+len(e.disks.made), 0)
	eq(t, "os", st.Manifest.Artifacts.OS, beta.Manifest.Artifacts.OS)
	eq(t, "disk", st.Manifest.Artifacts.Disks["qcow2"], beta.Manifest.Artifacts.Disks["qcow2"])
	eq(t, "os tag", e.digest("notes-os", "1.0.0"), beta.Manifest.Artifacts.OS)
	eq(t, "disk tag", e.digest("notes-disk", "1.0.0-qcow2"), beta.Manifest.Artifacts.Disks["qcow2"])
	eq(t, "latest-qcow2 follows stable", e.digest("notes-disk", "latest-qcow2"), beta.Manifest.Artifacts.Disks["qcow2"])

	// A bundle no registry holds any more is never built by a promotion.
	ctx := context.Background()
	for _, tg := range []string{"beta-1.0.0", "1.0.0"} {
		tag, _ := e.reg.Tag(e.host+"/acme/notes/notes-os", tg)
		if err := e.reg.DeleteTag(ctx, tag); err != nil {
			t.Fatal(err)
		}
	}
	if err := remote.Delete(mustDigest(t, e, "notes-os", beta.Manifest.Artifacts.OS)); err != nil {
		t.Fatal(err)
	}
	p, v = e.publisher()
	if _, err := p.Ship(ctx, ShipOptions{Version: v, Channel: "stable", Revision: 2, NoBuild: true, Sources: []*Ref{beta}}); err == nil ||
		!strings.Contains(err.Error(), "os bundle: no published bundle has these inputs") {
		t.Fatalf("a promotion that would build the bundle: %v", err)
	}
	eq(t, "built", len(e.builder.built), 0)
}

// Each disk is kept on its bundle and format: a format added is the only disk made, and a format
// dropped loses its latest tag.
func TestDiskFormatsAreMadeOneByOne(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := e.mustShip("beta", 1)
	formats := func(f ...string) {
		testutil.SetYAML(t, e.root, "stowage.yaml", []string{"shapes", "kairos", "disk_formats"}, f)
	}
	formats("qcow2", "vhd")
	second := e.mustShip("beta", 1)
	eq(t, "bundle rebuilt", len(e.builder.built), 0)
	eq(t, "disks made", strings.Join(slices.Concat(e.disks.made...), ","), "vhd")
	eq(t, "qcow2 kept", second.Manifest.Artifacts.Disks["qcow2"], first.Manifest.Artifacts.Disks["qcow2"])
	eq(t, "vhd tag", e.digest("notes-disk", "beta-1.0.0-vhd"), second.Manifest.Artifacts.Disks["vhd"])

	e.mustShip("stable", 1)
	eq(t, "stable reuses both", len(e.disks.made), 0)
	eq(t, "latest-vhd", e.digest("notes-disk", "latest-vhd"), second.Manifest.Artifacts.Disks["vhd"])
	e.bump("1.1.0")
	formats("qcow2")
	third := e.mustShip("stable", 1)
	eq(t, "no vhd", third.Manifest.Artifacts.Disks["vhd"], "")
	eq(t, "latest-vhd removed", e.digest("notes-disk", "latest-vhd"), "")
	eq(t, "latest-qcow2 moved", e.digest("notes-disk", "latest-qcow2"), third.Manifest.Artifacts.Disks["qcow2"])
	p, _ := e.publisher()
	if _, err := p.Verify(context.Background(), "stable", "", 1); err != nil {
		t.Fatal(err)
	}
}

// A disk that cannot be made fails the ship before any tag moves.
func TestAFailedDiskMovesNoTag(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.disks.fail = true
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "disks: auroraboot failed") {
		t.Fatalf("got %v", err)
	}
	for _, rt := range [][2]string{{"notes-api", "1.0.0"}, {"notes-os", "1.0.0"}, {"notes-release", "stable"}, {"notes-installer", "latest"}} {
		eq(t, rt[0]+":"+rt[1], e.digest(rt[0], rt[1]), "")
	}
	e.disks.fail = false
	r := e.mustShip("stable", 1)
	eq(t, "disk", e.digest("notes-disk", "1.0.0-qcow2"), r.Manifest.Artifacts.Disks["qcow2"])
}

// On an immutable channel, a published release is never republished with another bundle.
func TestABundleThatWouldDifferIsRefusedOnAnImmutableChannel(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.mustShip("stable", 1)
	ctx := context.Background()
	// Registry retention removed the bundle: its tag and digest are gone, so it would be rebuilt.
	tag, _ := e.reg.Tag(e.host+"/acme/notes/notes-os", "1.0.0")
	if err := e.reg.DeleteTag(ctx, tag); err != nil {
		t.Fatal(err)
	}
	if err := remote.Delete(mustDigest(t, e, "notes-os", r.Manifest.Artifacts.OS)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "os bundle: notes 1.0.0 is published on the immutable channel stable") ||
		!strings.Contains(err.Error(), "--revision 2") {
		t.Fatalf("got %v", err)
	}
	eq(t, "nothing built", len(e.builder.built), 0)
}

// An OS bundle tag holding other inputs on an immutable channel is refused before anything moves.
func TestAnImmutableBundleTagWithOtherInputsIsRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mustShip("stable", 1)
	ctx := context.Background()
	p, _ := e.publisher()
	other, _ := amd64Image(64, 1)
	tag, _ := e.reg.Tag(e.host+"/acme/notes/notes-os", "1.0.0")
	if err := e.reg.WriteImage(ctx, tag, other); err != nil {
		t.Fatal(err)
	}
	_, err := p.Ship(ctx, ShipOptions{Version: "1.0.0", Channel: "stable", Revision: 1})
	if err == nil || !strings.Contains(err.Error(), "os bundle: "+tag.String()+" exists with other inputs on an immutable channel") {
		t.Fatalf("got %v", err)
	}
}

func TestVerifyAndPruneCoverTheBundleAndDisks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mustShip("beta", 1)
	e.bump("1.1.0")
	cur := e.mustShip("beta", 1)
	p, _ := e.publisher()
	ctx := context.Background()
	osTag, _ := e.reg.Tag(e.host+"/acme/notes/notes-os", "beta-1.1.0")
	if err := e.reg.DeleteTag(ctx, osTag); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Verify(ctx, "beta", "", 1); err == nil || !strings.Contains(err.Error(), osTag.String()+" is missing") {
		t.Fatalf("verify with the bundle missing: %v", err)
	}
	if err := e.reg.Retag(ctx, mustDigest(t, e, "notes-os", cur.Manifest.Artifacts.OS), osTag); err != nil {
		t.Fatal(err)
	}
	tag, _ := e.reg.Tag(e.host+"/acme/notes/notes-disk", "beta-1.1.0-qcow2")
	if err := e.reg.DeleteTag(ctx, tag); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Verify(ctx, "beta", "", 1); err == nil || !strings.Contains(err.Error(), tag.String()+" is missing") {
		t.Fatalf("verify with a disk missing: %v", err)
	}
	if err := e.reg.Retag(ctx, mustDigest(t, e, "notes-disk", cur.Manifest.Artifacts.Disks["qcow2"]), tag); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Prune(ctx, "beta", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	eq(t, "os pruned", e.digest("notes-os", "beta-1.0.0"), "")
	eq(t, "disk pruned", e.digest("notes-disk", "beta-1.0.0-qcow2"), "")
	eq(t, "current os kept", e.digest("notes-os", "beta-1.1.0"), cur.Manifest.Artifacts.OS)
	eq(t, "current disk kept", e.digest("notes-disk", "beta-1.1.0-qcow2"), cur.Manifest.Artifacts.Disks["qcow2"])
}

// A yank moves latest-<format> back with the pointer; verify reports one that is off.
func TestLatestDiskFollowsThePointer(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := e.mustShip("stable", 1)
	e.bump("1.1.0")
	second := e.mustShip("stable", 1)
	eq(t, "latest-qcow2", e.digest("notes-disk", "latest-qcow2"), second.Manifest.Artifacts.Disks["qcow2"])
	p, _ := e.publisher()
	ctx := context.Background()
	if err := p.Yank(ctx, "stable", "1.1.0", 1, "bad"); err != nil {
		t.Fatal(err)
	}
	eq(t, "latest-qcow2 back", e.digest("notes-disk", "latest-qcow2"), first.Manifest.Artifacts.Disks["qcow2"])
	tag, _ := e.reg.Tag(e.host+"/acme/notes/notes-disk", "latest-qcow2")
	if err := e.reg.Retag(ctx, mustDigest(t, e, "notes-disk", second.Manifest.Artifacts.Disks["qcow2"]), tag); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Verify(ctx, "stable", "", 1); err == nil || !strings.Contains(err.Error(), tag.String()+" is "+second.Manifest.Artifacts.Disks["qcow2"]) {
		t.Fatalf("verify with latest-qcow2 off: %v", err)
	}
	if err := p.Yank(ctx, "stable", "1.0.0", 1, "bad too"); err != nil {
		t.Fatal(err)
	}
	eq(t, "latest-qcow2 removed with the pointer", e.digest("notes-disk", "latest-qcow2"), "")
}

func mustDigest(t *testing.T, e *env, repo, d string) name.Digest {
	t.Helper()
	ref, err := e.reg.Digest(e.host+"/acme/notes/"+repo, d)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// The disk builder, a privileged third-party image, gets the pull account, never the publisher's.
func TestDisksGetThePullAccount(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var asked []string
	e.pullAuth = func(r authn.Resource) (authn.AuthConfig, error) {
		asked = append(asked, r.String())
		return authn.AuthConfig{Username: "pull", Password: "read-only"}, nil
	}
	e.mustShip("stable", 1)
	eq(t, "disks made", len(e.disks.requests), 1)
	eq(t, "account", e.disks.requests[0].Auth.Username+":"+e.disks.requests[0].Auth.Password, "pull:read-only")
	// Resolved for the bundle's repository (credentials may be scoped to it), before building and
	// again before the disks.
	eq(t, "resolved for", strings.Join(asked, ","), e.host+"/acme/notes/notes-os,"+e.host+"/acme/notes/notes-os")

	// Without one, or one refused, nothing at all is built.
	e.bump("1.1.0")
	e.pullAuth = nil
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "no pull account") {
		t.Fatalf("disks without a pull account: %v", err)
	}
	eq(t, "built", len(e.builder.built), 0)
	e.pullAuth = func(authn.Resource) (authn.AuthConfig, error) {
		return authn.AuthConfig{}, errors.New("NOTES_PULL_TOKEN is not set")
	}
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "disks: NOTES_PULL_TOKEN is not set") {
		t.Fatalf("a refused pull account: %v", err)
	}
	eq(t, "built", len(e.builder.built), 0)
}

// A work directory in memory is refused before the bundle's archives are written there.
func TestTheBundleIsNotBuiltInMemory(t *testing.T) {
	saved := kairos.OnDisk
	kairos.OnDisk = func(dir string) error { return errors.New(dir + " is in memory") }
	t.Cleanup(func() { kairos.OnDisk = saved })
	e := newEnv(t)
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "os bundle: ") || !strings.Contains(err.Error(), "is in memory") {
		t.Fatalf("got %v", err)
	}
	eq(t, "bundle built", slices.Contains(e.builder.names(), "os"), false)
}
