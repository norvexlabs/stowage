package graph

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/backup"
	"github.com/norvexlabs/stowage/internal/chown"
	"github.com/norvexlabs/stowage/internal/front"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/sbom"
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
	want := []string{"image/api", "image/postgres", "image/web", FrontID, BackupID, ChownID, "chart", "os-bundle", "disks", "installer", "release"}
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
	testutil.Edit(t, root, "stowage.yaml", "disks: opt-in", "disks: always")
	testutil.Edit(t, root, "stowage.yaml", "audit: { fail_on: critical, fixable_only: true, allow: audit-allow.yaml }", "audit: { fail_on: high }")
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
	before = inputs(build(t, root))
	testutil.Edit(t, root, "stowage.yaml", "packages.atlas.test: [192.0.2.10, 192.0.2.11]", "packages.atlas.test: [192.0.2.11, 192.0.2.10]")
	if c := changed(before, inputs(build(t, root))); !c["image/api"] || c["image/ingest"] {
		t.Errorf("an add_hosts change: %v", c)
	}
	before = inputs(build(t, root))
	testutil.Edit(t, root, "stowage.yaml", "network: default", "network: host")
	if c := changed(before, inputs(build(t, root))); !c["image/api"] {
		t.Errorf("a network change: %v", c)
	}
	// default is BuildKit's own network: the same build as none named.
	before = inputs(build(t, root))
	testutil.Edit(t, root, "stowage.yaml", "network: host", "network: default")
	mid := inputs(build(t, root))
	testutil.Edit(t, root, "stowage.yaml", "    network: default\n", "")
	if c := changed(mid, inputs(build(t, root))); c["image/api"] || !changed(before, mid)["image/api"] {
		t.Errorf("network default and none named differ, or host and default do not")
	}
}

// A named build context enters the image's inputs as BuildKit sends it: named, whole whatever
// `inputs` narrows the main context to, and less its OWN .dockerignore — never the main one's.
func TestNamedBuildContextReachesTheImage(t *testing.T) {
	root := testutil.CopyExample(t, "full")
	testutil.Write(t, root, "vendored/src/a", "a\n")
	testutil.Write(t, root, "vendored/.dockerignore", "target\n")
	before := inputs(build(t, root))
	testutil.Edit(t, root, "stowage.yaml", "contexts: { schema: migrations }", "contexts: { schema: migrations, vendored: vendored }")
	if c := changed(before, inputs(build(t, root))); !c["image/api"] || c["image/ingest"] {
		t.Errorf("declaring a named context: %v", c)
	}
	// Its content, though api's inputs name only server/.
	before = inputs(build(t, root))
	testutil.Write(t, root, "vendored/src/a", "b\n")
	if c := changed(before, inputs(build(t, root))); !c["image/api"] {
		t.Errorf("a change inside the named context: %v", c)
	}
	// What its own ignore file excludes is not sent, so it is not an input.
	before = inputs(build(t, root))
	testutil.Write(t, root, "vendored/target/out", "built\n")
	if c := changed(before, inputs(build(t, root))); c["image/api"] {
		t.Errorf("a file its own .dockerignore excludes changed the image: %v", c)
	}
	// The main context's ignore file does not reach it: excluded there, still an input here.
	testutil.Write(t, root, ".dockerignore", "vendored/src\n")
	before = inputs(build(t, root))
	testutil.Write(t, root, "vendored/src/a", "c\n")
	if c := changed(before, inputs(build(t, root))); !c["image/api"] {
		t.Errorf("the main context's .dockerignore hid a named context's file: %v", c)
	}
	// Its name is an input too: another name is another build.
	before = inputs(build(t, root))
	testutil.Edit(t, root, "stowage.yaml", "contexts: { schema: migrations, vendored: vendored }", "contexts: { schema: migrations, other: vendored }")
	if c := changed(before, inputs(build(t, root))); !c["image/api"] {
		t.Errorf("renaming the named context: %v", c)
	}
	// Another spelling of the same directory is the same build.
	before = inputs(build(t, root))
	testutil.Edit(t, root, "stowage.yaml", "other: vendored }", "other: ./vendored/ }")
	if c := changed(before, inputs(build(t, root))); c["image/api"] {
		t.Errorf("respelling the named context's path: %v", c)
	}
	// A link: the walk would see its text while BuildKit sends the directory, so it is refused.
	if err := os.Symlink("vendored", filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	testutil.Edit(t, root, "stowage.yaml", "other: ./vendored/ }", "other: linked }")
	p, err := project.Open(root, "")
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.Version()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(p, Release{Version: v, Revision: 1, Channel: firstChannel(p)}); err == nil || !strings.Contains(err.Error(), "linked goes through a symbolic link") {
		t.Errorf("a linked named context: %v", err)
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
	for _, id := range []string{"image/api", "image/web", FrontID, BackupID, ChownID} {
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

// Only a release whose installs run a pg-age-rclone or volumes-age-rclone backup carries the runner.
func TestBackupRunnerOnlyWithAnAgeRcloneBackup(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, old, new string
	}{
		{"no backup", "backup:\n  provider: pg-age-rclone\n  profiles: { full: all }\n  volumes: [uploads]", "#"},
		{"command backup", "  provider: pg-age-rclone\n  profiles: { full: all }\n  volumes: [uploads]", "  provider: command\n  image: api\n  cmd: [notes-api, backup]\n"},
		{"neither docker nor kairos", "  docker: {}\n  kubernetes: { chart: deploy/chart, postgres_modes: [external, cnpg] }\n  kairos:\n    base: ubuntu:26.04@sha256:6a06111c3b0a8600feadb270aec771dc239bf407b25625f93f44ab00e11f4d3c\n    kairos_init: quay.io/kairos/kairos-init:v4.4.0@sha256:eb131bd9b4ed5b9b25cf761af9afa7d462bef27c2d0e9453ce884cf7733a6556\n    disk_formats: [qcow2]\n    host_api: [api]\n", "  kubernetes: { chart: deploy/chart, postgres_modes: [external, cnpg] }\n"},
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
	testutil.Edit(t, root, "stowage.yaml", "  kairos:\n    base: ubuntu:26.04@sha256:6a06111c3b0a8600feadb270aec771dc239bf407b25625f93f44ab00e11f4d3c\n    kairos_init: quay.io/kairos/kairos-init:v4.4.0@sha256:eb131bd9b4ed5b9b25cf761af9afa7d462bef27c2d0e9453ce884cf7733a6556\n    disk_formats: [qcow2]\n    host_api: [api]\n", "")
	if _, ok := build(t, root).Node(BackupID); !ok {
		t.Error("a Docker release with a pg-age-rclone backup carries no runner")
	}
	// A project without a database backs its volumes up with the runner too.
	root = testutil.CopyExample(t, "stateless")
	if _, ok := build(t, root).Node(BackupID); !ok {
		t.Error("a release with a volumes-age-rclone backup carries no runner")
	}
	testutil.Edit(t, root, "stowage.yaml", "backup: { provider: volumes-age-rclone, volumes: [state] }", "")
	if _, ok := build(t, root).Node(BackupID); ok {
		t.Error("a release without a backup carries the runner")
	}
}

// The volume helper's recipe is the helper's input, and through it every artifact's that lists
// the images; no project image reads it.
func TestChownRecipeReachesTheHelperOnly(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	a := inputs(build(t, root))
	saved := chown.Dockerfile
	chown.Dockerfile = append([]byte("# another recipe\n"), saved...)
	t.Cleanup(func() { chown.Dockerfile = saved })
	g := build(t, root)
	if n, _ := g.Node(ChownID); string(n.Recipe) != string(chown.Dockerfile) {
		t.Error("the helper's node does not carry the recipe it hashes")
	}
	c := changed(a, inputs(g))
	want := map[string]bool{ChownID: true, "chart": true, "os-bundle": true, "disks": true, "installer": true, "release": true}
	if !maps.Equal(c, want) {
		t.Errorf("changed = %v, want %v", c, want)
	}
}

// Only a release whose Docker or VM installs give a data volume its uid carries the helper.
func TestVolumeHelperOnlyWithAVolumeOfAUID(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, old, new string
	}{
		{"no uid", "    postgres: { uid: 70 }\n    uploads:  { uid: 1000 }\n", "    postgres: {}\n    uploads:  {}\n"},
		{"neither docker nor kairos", "  docker: {}\n  kubernetes: { chart: deploy/chart, postgres_modes: [external, cnpg] }\n  kairos:\n    base: ubuntu:26.04@sha256:6a06111c3b0a8600feadb270aec771dc239bf407b25625f93f44ab00e11f4d3c\n    kairos_init: quay.io/kairos/kairos-init:v4.4.0@sha256:eb131bd9b4ed5b9b25cf761af9afa7d462bef27c2d0e9453ce884cf7733a6556\n    disk_formats: [qcow2]\n    host_api: [api]\n", "  kubernetes: { chart: deploy/chart, postgres_modes: [external, cnpg] }\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			root := testutil.CopyExample(t, "minimal")
			testutil.Edit(t, root, "stowage.yaml", c.old, c.new)
			if _, ok := build(t, root).Node(ChownID); ok {
				t.Error("the release carries the volume helper")
			}
		})
	}
	// Positive control: one volume of a uid is enough.
	root := testutil.CopyExample(t, "minimal")
	testutil.Edit(t, root, "stowage.yaml", "    postgres: { uid: 70 }\n", "    postgres: {}\n")
	if _, ok := build(t, root).Node(ChownID); !ok {
		t.Error("a release with a volume of a uid carries no volume helper")
	}
}

// A project without a front builds no front image (examples/minimal's, with one, is in
// TestGraphShape).
func TestNoFrontImageWithoutAFront(t *testing.T) {
	t.Parallel()
	g := build(t, testutil.CopyExample(t, "stateless"))
	if _, ok := g.Node(FrontID); ok {
		t.Error("a project without a front builds the front")
	}
	if _, ok := g.Node(ChownID); !ok {
		t.Error("examples/stateless's volume of a uid has no volume helper")
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

// The VM shape's settings and the host block reach the bundle and its disks, the disk formats the disks and not the
// bundle, the size neither, and none of them an image. (The chart and the installer carry the whole spec.)
func TestKairosSettingsReachTheBundle(t *testing.T) {
	root := testutil.CopyExample(t, "full")
	base := inputs(build(t, root))
	for _, e := range []struct {
		old, new  string
		os, disks bool
	}{
		{"base: ubuntu:26.04@sha256:6", "base: ubuntu:26.04@sha256:d", true, true},
		{"kairos-init:v4.4.0@sha256:e", "kairos-init:v4.4.0@sha256:f", true, true},
		{"pcspkr: { block: true, reason: a VM has no speaker }", "pcspkr: { block: true, reason: no speaker }", true, true},
		{"redirect_always_follow: \"Y\"", "redirect_always_follow: \"N\"", true, true},
		{"tcpdump: { reason:", "tcpdump-dbg: { reason:", true, true},
		{"disk_formats: [raw, qcow2, ova, vhd, gce, iso]", "disk_formats: [raw, qcow2, vhd, gce]", false, true},
		// The clouds' formats are made whether disk_formats lists them or not.
		{"disk_formats: [raw, qcow2, ova, vhd, gce, iso]", "disk_formats: [qcow2, ova, iso]", false, false},
		{"cloud_publish: [aws, azure, gcp]", "cloud_publish: [aws, azure]", false, false},
		{"disk_formats: [raw, qcow2, ova, vhd, gce, iso]\n    cloud_publish: [aws, azure, gcp]", "disk_formats: [raw, qcow2, ova, vhd]\n    cloud_publish: [aws, azure]", false, true},
		// The order changes no disk; the release follows the spec its installer carries.
		{"disk_formats: [raw, qcow2, ova, vhd, gce, iso]", "disk_formats: [iso, gce, vhd, ova, qcow2, raw]", false, false},
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
	// The VM's size is vm create's, the installer's: no bundle, no disk.
	testutil.Edit(t, root, "stowage.yaml", "recommended: { cpus: 4,", "recommended: { cpus: 8,")
	if c := changed(base, inputs(build(t, root))); c["os-bundle"] || c["disks"] || !c["installer"] || !c["release"] {
		t.Errorf("a size change: changed = %v", c)
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

// The scanner's recipe changes the scanner, the release, its installer and its OS bundle (whose
// <short>-stow names it, though the bundle does not preload it), and no project image.
func TestTheScannerReachesTheRelease(t *testing.T) {
	root := testutil.CopyExample(t, "full")
	a := inputs(build(t, root))
	saved := sbom.Dockerfile
	sbom.Dockerfile = append([]byte("# another recipe\n"), saved...)
	t.Cleanup(func() { sbom.Dockerfile = saved })
	g := build(t, root)
	if n, _ := g.Node(AuditID); string(n.Recipe) != string(sbom.Dockerfile) {
		t.Error("the scanner's node does not carry the recipe it hashes")
	}
	c := changed(a, inputs(g))
	if !c[AuditID] || !c["release"] || !c["installer"] || !c["os-bundle"] || c["image/api"] || c[FrontID] {
		t.Errorf("changed = %v", c)
	}
}

// Each file of a service's table, an input-picked one's every file included, reaches the
// installer, which carries them, and no image.
func TestTheServicesTablesReachTheInstaller(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "full")
	base := inputs(build(t, root))
	for _, e := range []struct{ file, old, new string }{
		{"deploy/vpn.nft", "table inet vpn {", "table inet vpn {\n\tcomment \"x\""},
		{"deploy/ingest.nft", "ip saddr @probes counter", "ip saddr @probes counter comment \"x\""},
		{"deploy/ingest-enforce.nft", "tcp dport 9", "tcp dport 7"},
	} {
		testutil.Edit(t, root, e.file, e.old, e.new)
		now := inputs(build(t, root))
		c := changed(base, now)
		if !c["installer"] {
			t.Errorf("%s: changed = %v", e.file, c)
		}
		for id := range c {
			if strings.HasPrefix(id, "image/") {
				t.Errorf("%s changed %s", e.file, id)
			}
		}
		base = now
	}
}
