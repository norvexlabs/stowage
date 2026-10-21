package graph

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/backup"
	"github.com/norvexlabs/stowage/internal/front"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/testutil"
)

func build(t *testing.T, root string) *Graph {
	t.Helper()
	p, err := project.Open(root, "")
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.Version()
	if err != nil {
		t.Fatal(err)
	}
	g, err := Build(p, Release{Version: v, Revision: 1, Channel: firstChannel(p)})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func inputs(g *Graph) map[string]string {
	m := map[string]string{}
	for _, n := range g.Nodes {
		m[n.ID] = n.Inputs
	}
	return m
}

func changed(a, b map[string]string) map[string]bool {
	out := map[string]bool{}
	for id := range a {
		if a[id] != b[id] {
			out[id] = true
		}
	}
	return out
}

func TestGraphShape(t *testing.T) {
	g := build(t, testutil.CopyExample(t, "minimal"))
	want := []string{"image/api", "image/postgres", "image/web", FrontID, BackupID, "chart", "os-bundle", "disks", "installer", "release"}
	if len(g.Nodes) != len(want) {
		t.Fatalf("nodes = %v", g.Nodes)
	}
	for i, id := range want {
		if g.Nodes[i].ID != id {
			t.Errorf("node %d = %s, want %s", i, g.Nodes[i].ID, id)
		}
	}
	api, _ := g.Node("image/api")
	web, _ := g.Node("image/web")
	if !api.ReadsVersion || web.ReadsVersion {
		t.Errorf("ReadsVersion: api=%v web=%v", api.ReadsVersion, web.ReadsVersion)
	}
	pg, _ := g.Node("image/postgres")
	if pg.Pinned == "" {
		t.Error("postgres not pinned")
	}
}

func TestDeterministic(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	a, b := inputs(build(t, root)), inputs(build(t, testutil.CopyExample(t, "minimal")))
	if c := changed(a, b); len(c) > 0 {
		t.Fatalf("same project in two places differs on %v", c)
	}
}

// A version bump re-tags every image that does not read the version: only the api image (which
// declares STOWAGE_PROJECT_VERSION) and the version-carrying artifacts change.
func TestVersionBumpRebuildsOnlyWhatReadsIt(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	before := inputs(build(t, root))
	testutil.Write(t, root, "VERSION", "1.1.0\n")
	testutil.Write(t, root, "changes/1.1.0.md", "- more\n")
	after := inputs(build(t, root))
	c := changed(before, after)
	for _, id := range []string{"image/api", "chart", "os-bundle", "disks", "installer", "release"} {
		if !c[id] {
			t.Errorf("%s unchanged after a version bump", id)
		}
	}
	for _, id := range []string{"image/web", "image/postgres"} {
		if c[id] {
			t.Errorf("%s changed though it does not read the version", id)
		}
	}
}

func TestSourceChangeTouchesOneImageAndItsDependents(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	before := inputs(build(t, root))
	testutil.Edit(t, root, "web/index.html", "<h1>Notes</h1>", "<h1>Notes!</h1>")
	c := changed(before, inputs(build(t, root)))
	if !c["image/web"] || !c["release"] || !c["installer"] {
		t.Errorf("changed = %v", c)
	}
	if c["image/api"] || c["image/postgres"] {
		t.Errorf("an unrelated image changed: %v", c)
	}
}

func TestDockerIgnoredFileChangesNothing(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	testutil.Write(t, root, "web/.dockerignore", "*.tmp\n")
	before := inputs(build(t, root))
	testutil.Write(t, root, "web/scratch.tmp", "noise")
	if c := changed(before, inputs(build(t, root))); len(c) > 0 {
		t.Errorf("an ignored file changed %v", c)
	}
}

// The compose file is carried by the installer and by the OS bundle's <short>-stow.
func TestComposeChangeReachesInstallerNotImages(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	before := inputs(build(t, root))
	testutil.Edit(t, root, "deploy/compose.yaml", "interval: 5s", "interval: 6s")
	c := changed(before, inputs(build(t, root)))
	if !c["installer"] || !c["release"] || !c["os-bundle"] || !c["disks"] {
		t.Errorf("changed = %v", c)
	}
	for _, id := range []string{"image/api", "image/web", "image/postgres", "chart"} {
		if c[id] {
			t.Errorf("%s changed on a compose edit", id)
		}
	}
}

// Publisher-only fields reach no artifact: fixing push credentials or a channel template must not
// change a release's inputs.
func TestPublisherFieldsChangeNoDigest(t *testing.T) {
	root := testutil.CopyExample(t, "full")
	before := inputs(build(t, root))
	testutil.Edit(t, root, "stowage.yaml", "user: publisher, token_env: STOWAGE_PUSH_TOKEN", "user: ci, token_env: CI_PUSH_TOKEN")
	testutil.Edit(t, root, "stowage.yaml", `dev:    { tag: "v{v}-dev"`, `dev:    { tag: "v{v}-nightly"`)
	testutil.Edit(t, root, "stowage.yaml", "key: env://ATLAS_SIGNING_KEY", "key: hashivault://atlas")
	if c := changed(before, inputs(build(t, root))); len(c) > 0 {
		t.Errorf("publisher-only edits changed %v", c)
	}
	// Positive control: a runtime field does change them.
	testutil.Write(t, root, "keys/release-2027.pub", readKey(t, root))
	testutil.Edit(t, root, "stowage.yaml", "public_keys: [keys/release-2026.pub]", "public_keys: [keys/release-2027.pub]")
	if c := changed(before, inputs(build(t, root))); !c["installer"] || !c["chart"] {
		t.Errorf("a trusted-key change did not reach the artifacts: %v", c)
	}
}

// A Stowage upgrade (stowage.lock) reaches every artifact that carries the runtime, chart included,
// and no application image.
func TestLockReachesRuntimeCarriers(t *testing.T) {
	root := testutil.CopyExample(t, "full")
	before := inputs(build(t, root))
	testutil.Edit(t, root, "stowage.lock", "stowage: 0.1.0", "stowage: 0.2.0")
	c := changed(before, inputs(build(t, root)))
	for _, id := range []string{"chart", "installer", "os-bundle", "disks", "release"} {
		if !c[id] {
			t.Errorf("%s unchanged by a runtime upgrade", id)
		}
	}
	for id := range c {
		if strings.HasPrefix(id, "image/") {
			t.Errorf("%s changed by a runtime upgrade", id)
		}
	}
}

func TestBuildInputsReachTheImage(t *testing.T) {
	root := testutil.CopyExample(t, "full")
	before := inputs(build(t, root))
	testutil.Edit(t, root, "stowage.yaml", `args: { FEATURES: "core,ingest" }`, `args: { FEATURES: "core" }`)
	if c := changed(before, inputs(build(t, root))); !c["image/api"] || c["image/ingest"] {
		t.Errorf("an args change: %v", c)
	}
	before = inputs(build(t, root))
	testutil.Edit(t, root, "stowage.yaml", "target: runtime", "target: build")
	if c := changed(before, inputs(build(t, root))); !c["image/api"] {
		t.Errorf("a target change: %v", c)
	}
}

// A revision repackages what embeds it and re-tags every image.
func TestRevisionReachesWhatEmbedsIt(t *testing.T) {
	root := testutil.CopyExample(t, "full")
	p, err := project.Open(root, "")
	if err != nil {
		t.Fatal(err)
	}
	r1, err := Build(p, Release{Version: "2026.10.1", Revision: 1, Channel: "stable"})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Build(p, Release{Version: "2026.10.1", Revision: 2, Channel: "stable"})
	if err != nil {
		t.Fatal(err)
	}
	c := changed(inputs(r1), inputs(r2))
	for _, id := range []string{"chart", "os-bundle", "disks", "installer", "release"} {
		if !c[id] {
			t.Errorf("%s unchanged across revisions", id)
		}
	}
	for id := range c {
		if strings.HasPrefix(id, "image/") {
			t.Errorf("%s changed across revisions", id)
		}
	}
	if _, err := Build(p, Release{Version: "2026.10.1", Revision: 0, Channel: "stable"}); err == nil {
		t.Error("revision 0 accepted")
	}
	if _, err := Build(p, Release{Version: "2026.10.1", Revision: 1, Channel: "nightly"}); err == nil {
		t.Error("undeclared channel accepted")
	}
	// The channel reaches every artifact that embeds it, and no image or OS bundle: a promotion
	// re-tags those.
	dev, err := Build(p, Release{Version: "2026.10.1", Revision: 1, Channel: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	cc := changed(inputs(r1), inputs(dev))
	for _, id := range []string{"chart", "installer", "release"} {
		if !cc[id] {
			t.Errorf("%s unchanged across channels", id)
		}
	}
	for id := range cc {
		if strings.HasPrefix(id, "image/") || id == "os-bundle" || id == "disks" {
			t.Errorf("%s changed with the channel", id)
		}
	}
}

func firstChannel(p *project.Project) string { return p.Spec.ChannelNames()[0] }

// Image paths are confined to the project like every other spec path.
func TestImagePathsStayInTheProject(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	testutil.Edit(t, root, "stowage.yaml", "dockerfile: web/Dockerfile", "dockerfile: ../web/Dockerfile")
	p, err := project.Open(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(p, Release{Version: "1.0.0", Revision: 1, Channel: "stable"}); err == nil || !strings.Contains(err.Error(), "leaves the project root") {
		t.Errorf("Build = %v", err)
	}
}

// Without the VM shape the installer still follows the channel: its own inputs carry it.
func TestInstallerFollowsTheChannelWithoutVM(t *testing.T) {
	root := testutil.CopyExample(t, "full")
	testutil.SetYAML(t, root, "stowage.yaml", []string{"shapes", "kairos"}, nil)
	p, err := project.Open(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Spec.Shapes.Kairos != nil {
		t.Fatal("the VM shape is still declared")
	}
	stable, err := Build(p, Release{Version: "2026.10.1", Revision: 1, Channel: "stable"})
	if err != nil {
		t.Fatal(err)
	}
	dev, err := Build(p, Release{Version: "2026.10.1", Revision: 1, Channel: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if c := changed(inputs(stable), inputs(dev)); !c["installer"] {
		t.Errorf("the installer ignores the channel: %v", c)
	}
}

func TestPlatformIsABuiltImageInput(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	a := inputs(build(t, root))
	platform = "linux/arm64"
	t.Cleanup(func() { platform = Platform })
	b := inputs(build(t, root))
	for _, id := range []string{"image/api", "image/web", FrontID, BackupID} {
		if a[id] == b[id] {
			t.Errorf("%s: same inputs on another platform", id)
		}
	}
	if a["image/postgres"] != b["image/postgres"] {
		t.Error("a pinned image (every platform mirrored) depends on the build platform")
	}
}

// The front's recipe is the front's input, and through it every artifact's that lists the images;
// no project image reads it.
func TestFrontRecipeReachesTheFrontOnly(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	a := inputs(build(t, root))
	saved := front.Dockerfile
	front.Dockerfile = append([]byte("# another recipe\n"), saved...)
	t.Cleanup(func() { front.Dockerfile = saved })
	g := build(t, root)
	if n, _ := g.Node(FrontID); string(n.Recipe) != string(front.Dockerfile) {
		t.Error("the front's node does not carry the recipe it hashes")
	}
	c := changed(a, inputs(g))
	want := map[string]bool{FrontID: true, "chart": true, "os-bundle": true, "disks": true, "installer": true, "release": true}
	if !maps.Equal(c, want) {
		t.Errorf("changed = %v, want %v", c, want)
	}
}

// The backup runner's recipe is the runner's input, and through it every artifact's that lists
// the images; no project image reads it.
func TestBackupRecipeReachesTheRunnerOnly(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	a := inputs(build(t, root))
	saved := backup.Dockerfile
	backup.Dockerfile = append([]byte("# another recipe\n"), saved...)
	t.Cleanup(func() { backup.Dockerfile = saved })
	g := build(t, root)
	if n, _ := g.Node(BackupID); string(n.Recipe) != string(backup.Dockerfile) {
		t.Error("the runner's node does not carry the recipe it hashes")
	}
	c := changed(a, inputs(g))
	want := map[string]bool{BackupID: true, "chart": true, "os-bundle": true, "disks": true, "installer": true, "release": true}
	if !maps.Equal(c, want) {
		t.Errorf("changed = %v, want %v", c, want)
	}
}

// Only a release whose VM runs a pg-age-rclone backup carries the runner.
func TestBackupRunnerOnlyWithAPgAgeRcloneBackup(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, old, new string
	}{
		{"no backup", "backup:\n  provider: pg-age-rclone\n  profiles: { full: all }\n", ""},
		{"command backup", "  provider: pg-age-rclone\n  profiles: { full: all }\n", "  provider: command\n  image: api\n  cmd: [notes-api, backup]\n"},
		{"neither docker nor kairos", "  docker: {}\n  kubernetes: { chart: deploy/chart, postgres_modes: [external, cnpg] }\n  kairos:\n    base: ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55\n    kairos_init: quay.io/kairos/kairos-init:v0.16.2@sha256:0777266d25e31b653deaaaa084d4e27d98f7a55e82e42f0648e2ba9c756b8664\n    disk_formats: [qcow2]\n    host_api: [api]\n", "  kubernetes: { chart: deploy/chart, postgres_modes: [external, cnpg] }\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			root := testutil.CopyExample(t, "minimal")
			testutil.Edit(t, root, "stowage.yaml", c.old, c.new)
			g := build(t, root)
			if _, ok := g.Node(BackupID); ok {
				t.Error("the release carries the backup runner")
			}
		})
	}
	// Positive control: the Docker shape alone runs backups, with the runner.
	root := testutil.CopyExample(t, "minimal")
	testutil.Edit(t, root, "stowage.yaml", "  kairos:\n    base: ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55\n    kairos_init: quay.io/kairos/kairos-init:v0.16.2@sha256:0777266d25e31b653deaaaa084d4e27d98f7a55e82e42f0648e2ba9c756b8664\n    disk_formats: [qcow2]\n    host_api: [api]\n", "")
	if _, ok := build(t, root).Node(BackupID); !ok {
		t.Error("a Docker release with a pg-age-rclone backup carries no runner")
	}
}

func TestTrustedPublicKeysReachWhatCarriesThemNotImages(t *testing.T) {
	root := testutil.CopyExample(t, "full")
	before := inputs(build(t, root))
	testutil.Edit(t, root, "keys/release-2026.pub", "-----END PUBLIC KEY-----", "-----END PUBLIC KEY-----\n")
	c := changed(before, inputs(build(t, root)))
	if !c["installer"] || !c["chart"] || !c["os-bundle"] || !c["disks"] || !c["release"] {
		t.Errorf("changed = %v", c)
	}
	for id := range c {
		if strings.HasPrefix(id, "image/") {
			t.Errorf("%s changed with a public key", id)
		}
	}
}

func readKey(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "keys", "release-2026.pub"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The installer image, and the OS bundle's <short>-stow, are the executable with the release
// appended: another executable or another pull account is another installer and bundle, and no
// image.
func TestInstallerExecutableAndPullAccountAreInstallerInputs(t *testing.T) {
	t.Parallel()
	p, err := project.Open(testutil.CopyExample(t, "minimal"), "")
	if err != nil {
		t.Fatal(err)
	}
	at := func(r Release) map[string]string {
		r.Version, r.Revision, r.Channel = "1.0.0", 1, "stable"
		g, err := Build(p, r)
		if err != nil {
			t.Fatal(err)
		}
		return inputs(g)
	}
	base := at(Release{Installer: []byte("exe-1"), Pull: "pull:a"})
	for what, r := range map[string]Release{
		"executable": {Installer: []byte("exe-2"), Pull: "pull:a"},
		"pull":       {Installer: []byte("exe-1"), Pull: "pull:b"},
	} {
		c := changed(base, at(r))
		if want := carriers(); !maps.Equal(c, want) {
			t.Errorf("%s: changed %v, want %v", what, c, want)
		}
	}
}

// The installer and the OS bundle carry the release manifest, so what the manifest records
// beyond the tree's files — the changelog section, the version scheme — is one of their inputs.
func TestTheCarriedManifestIsAnInstallerInput(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "minimal")
	before := inputs(build(t, root))
	testutil.Write(t, root, "changes/1.0.0.md", "### Added\n- Rewritten.\n")
	if c := changed(before, inputs(build(t, root))); !maps.Equal(c, carriers()) {
		t.Errorf("changelog: changed %v, want %v", c, carriers())
	}
	p, err := project.Open(root, "")
	if err != nil {
		t.Fatal(err)
	}
	at := func() map[string]string {
		g, err := Build(p, Release{Version: "1.0.0", Revision: 1, Channel: "stable"})
		if err != nil {
			t.Fatal(err)
		}
		return inputs(g)
	}
	a := at()
	p.Spec.Version.Scheme.Config = map[string]any{"format": "other"}
	if c := changed(a, at()); !maps.Equal(c, carriers()) {
		t.Errorf("scheme: changed %v, want %v", c, carriers())
	}
}

// The chart's packaged files are its inputs: an edit reaches the chart and the release only, and a
// file its .helmignore excludes reaches nothing.
func TestChartFilesAreChartInputs(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "minimal")
	before := inputs(build(t, root))
	testutil.Edit(t, root, "deploy/chart/values.yaml", "replicas: 1", "replicas: 2")
	after := inputs(build(t, root))
	want := map[string]bool{"chart": true, "release": true}
	if c := changed(before, after); !maps.Equal(c, want) {
		t.Errorf("a chart edit changed %v, want %v", c, want)
	}
	testutil.Write(t, root, "deploy/chart/.helmignore", "notes.txt\n")
	ignored := inputs(build(t, root))
	testutil.Write(t, root, "deploy/chart/notes.txt", "not packaged\n")
	if c := changed(ignored, inputs(build(t, root))); len(c) > 0 {
		t.Errorf("an ignored chart file changed %v", c)
	}
	// Positive control: the same file under a name the .helmignore does not exclude.
	testutil.Write(t, root, "deploy/chart/notes.md", "packaged\n")
	if c := changed(ignored, inputs(build(t, root))); !c["chart"] {
		t.Errorf("a packaged chart file changed %v", c)
	}
}

func TestAChartThatCannotBePackagedFailsTheGraph(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "minimal")
	testutil.Write(t, root, "deploy/chart/values.yaml", "stowage: {}\n")
	p, err := project.Open(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(p, Release{Version: "1.0.0", Revision: 1, Channel: "stable"}); err == nil || !strings.Contains(err.Error(), "shapes.kubernetes.chart: values.yaml") {
		t.Fatalf("err = %v", err)
	}
}

// The VM shape's settings reach the bundle and its disks, the disk formats the disks and not the
// bundle, and none of them an image. (The chart and the installer carry the whole spec.)
func TestKairosSettingsReachTheBundle(t *testing.T) {
	root := testutil.CopyExample(t, "full")
	base := inputs(build(t, root))
	for _, e := range []struct {
		old, new  string
		os, disks bool
	}{
		{"base: ubuntu:24.04@sha256:5", "base: ubuntu:24.04@sha256:6", true, true},
		{"kairos-init:v0.16.2@sha256:0", "kairos-init:v0.16.2@sha256:1", true, true},
		{"disk_formats: [raw, qcow2, ova, vhd, gce]", "disk_formats: [raw, qcow2, vhd, gce]", false, true},
		// The clouds' formats are made whether disk_formats lists them or not.
		{"disk_formats: [raw, qcow2, ova, vhd, gce]", "disk_formats: [qcow2, ova]", false, false},
		{"cloud_publish: [aws, azure, gcp]", "cloud_publish: [aws, azure]", false, false},
		{"disk_formats: [raw, qcow2, ova, vhd, gce]\n    cloud_publish: [aws, azure, gcp]", "disk_formats: [raw, qcow2, ova, vhd]\n    cloud_publish: [aws, azure]", false, true},
		// The order changes no disk; the release follows the spec its installer carries.
		{"disk_formats: [raw, qcow2, ova, vhd, gce]", "disk_formats: [gce, vhd, ova, qcow2, raw]", false, false},
	} {
		testutil.Edit(t, root, "stowage.yaml", e.old, e.new)
		c := changed(base, inputs(build(t, root)))
		if c["os-bundle"] != e.os || c["disks"] != e.disks || !c["release"] {
			t.Errorf("%s → %s: changed = %v", e.old, e.new, c)
		}
		for id := range c {
			if strings.HasPrefix(id, "image/") {
				t.Errorf("%s → %s changed %s", e.old, e.new, id)
			}
		}
		testutil.Edit(t, root, "stowage.yaml", e.new, e.old)
	}
}

// The bundle's recipe reaches the bundle and the disks; the disks' recipe, and how they are
// pushed, the disks alone.
func TestKairosRecipesReachTheirNodes(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	a := inputs(build(t, root))
	saved := bundleRecipe
	bundleRecipe = func() string { return "sha256:another" }
	c := changed(a, inputs(build(t, root)))
	bundleRecipe = saved
	if !c["os-bundle"] || !c["disks"] || !c["release"] {
		t.Errorf("a bundle recipe change: %v", c)
	}
	savedDisk := diskRecipe
	diskRecipe = func() string { return "sha256:another" }
	t.Cleanup(func() { diskRecipe = savedDisk })
	c = changed(a, inputs(build(t, root)))
	if c["os-bundle"] || !c["disks"] || !c["release"] {
		t.Errorf("a disk recipe change: %v", c)
	}
	other := DiskKey("sha256:os", "raw")
	diskRecipe = savedDisk
	if DiskKey("sha256:os", "raw") == other {
		t.Error("DiskKey ignores the disk recipe")
	}
	savedLayer := diskLayer
	diskLayer = "plain"
	t.Cleanup(func() { diskLayer = savedLayer })
	c = changed(a, inputs(build(t, root)))
	if c["os-bundle"] || !c["disks"] || !c["release"] {
		t.Errorf("a disk layer change: %v", c)
	}
	other = DiskKey("sha256:os", "raw")
	diskLayer = savedLayer
	if DiskKey("sha256:os", "raw") == other {
		t.Error("DiskKey ignores how the disk is pushed")
	}
}

// The bundle's key follows the digests of the images it preloads, a disk's the bundle's and its
// format.
func TestOSAndDiskKeys(t *testing.T) {
	t.Parallel()
	imgs := map[string]string{"api": "sha256:1", "web": "sha256:2"}
	k := OSKey("sha256:in", imgs)
	if OSKey("sha256:in", map[string]string{"web": "sha256:2", "api": "sha256:1"}) != k {
		t.Error("OSKey depends on map order")
	}
	for _, other := range []string{
		OSKey("sha256:other", imgs),
		OSKey("sha256:in", map[string]string{"api": "sha256:1", "web": "sha256:3"}),
		OSKey("sha256:in", map[string]string{"api": "sha256:1"}),
		OSKey("sha256:in", map[string]string{"api": "sha256:1", "web": "sha256:2", "x": "sha256:4"}),
	} {
		if other == k {
			t.Error("OSKey ignores a change")
		}
	}
	d := DiskKey("sha256:os", "qcow2")
	if DiskKey("sha256:os2", "qcow2") == d || DiskKey("sha256:os", "raw") == d {
		t.Error("DiskKey ignores a change")
	}
}

// carriers are the nodes that carry the release manifest in minimal: the installer, the OS
// bundle's <short>-stow (and so its disks), and the release.
func carriers() map[string]bool {
	return map[string]bool{"installer": true, "os-bundle": true, "disks": true, "release": true}
}
