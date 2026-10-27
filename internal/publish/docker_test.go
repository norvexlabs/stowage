package publish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/norvexlabs/stowage/internal/kairos"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// A stand-in docker CLI records its arguments and answers `image save` with a prepared archive.
const fakeDocker = `#!/bin/sh
echo "$*" >> "$FAKE_DOCKER_LOG"
if [ "$1 $2" = "image save" ]; then cp "$FAKE_DOCKER_ARCHIVE" "$4"; fi
if [ "$1 $2" = "buildx build" ]; then
  for last; do :; done
  ls -A "$last" > "$FAKE_DOCKER_LOG.context"
  while [ $# -gt 0 ]; do
    if [ "$1" = --file ]; then cp "$2" "$FAKE_DOCKER_LOG.dockerfile"; fi
    shift
  done
fi
`

func TestDockerBuilderPassesTheSpecAndYieldsAnOCIImage(t *testing.T) {
	root := testutil.CopyExample(t, "full")
	p, err := project.Open(root, "")
	if err != nil {
		t.Fatal(err)
	}
	var built string
	for _, n := range p.Spec.ImageNames() {
		if img := p.Spec.Images[n]; img.Built() && img.Target != "" && len(img.Args) > 0 {
			built = n
		}
	}
	if built == "" {
		t.Fatal("the full example has no built image with a target and args")
	}
	dir := t.TempDir()
	src, _ := random.Image(64, 2)
	tag, _ := name.NewTag("saved:1")
	archive := filepath.Join(dir, "saved.tar")
	if err := tarball.WriteToFile(archive, tag, src); err != nil {
		t.Fatal(err)
	}
	docker := filepath.Join(dir, "docker")
	if err := os.WriteFile(docker, []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	logf := filepath.Join(dir, "log")
	t.Setenv("FAKE_DOCKER_LOG", logf)
	t.Setenv("FAKE_DOCKER_ARCHIVE", archive)
	work := filepath.Join(dir, "work")
	b := &DockerBuilder{Docker: docker, WorkDir: work}
	img, cleanup, err := b.Build(context.Background(), p, BuildRequest{Image: built, Version: "2026.10.25", Inputs: "sha256:0123456789abcdef0123"})
	if err != nil {
		t.Fatal(err)
	}
	mt, _ := img.MediaType()
	if mt != types.OCIManifestSchema1 {
		t.Fatalf("manifest media type %s", mt)
	}
	ls, _ := img.Layers()
	srcLs, _ := src.Layers()
	if len(ls) != len(srcLs) {
		t.Fatalf("%d layers, want %d", len(ls), len(srcLs))
	}
	for i, l := range ls {
		lmt, _ := l.MediaType()
		d, _ := l.DiffID()
		sd, _ := srcLs[i].DiffID()
		if lmt != types.OCILayer || d != sd {
			t.Fatalf("layer %d: %s %s, want %s %s", i, lmt, d, types.OCILayer, sd)
		}
	}
	man, _ := img.Manifest()
	for i, l := range man.Layers {
		if l.MediaType != types.OCILayer {
			t.Fatalf("manifest layer %d: %s", i, l.MediaType)
		}
	}
	if man.Config.MediaType != types.OCIConfigJSON {
		t.Fatalf("config media type %s", man.Config.MediaType)
	}
	cf, _ := img.ConfigFile()
	scf, _ := src.ConfigFile()
	if len(cf.RootFS.DiffIDs) != len(scf.RootFS.DiffIDs) {
		t.Fatal("config rootfs changed")
	}
	cleanup()
	if entries, _ := os.ReadDir(work); len(entries) != 0 {
		t.Fatalf("cleanup left %d entries in the work dir", len(entries))
	}

	raw, _ := os.ReadFile(logf)
	calls := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(calls) != 3 {
		t.Fatalf("docker calls:\n%s", raw)
	}
	spec := p.Spec.Images[built]
	buildCall := calls[0]
	for _, want := range []string{
		"buildx build --load --platform linux/amd64 --file " + filepath.Join(root, spec.Dockerfile),
		"--tag stowage-build/" + p.Spec.Name + "-" + built + ":0123456789abcdef-",
		"--target " + spec.Target,
		"--build-arg STOWAGE_PROJECT_VERSION=2026.10.25",
		"--add-host packages.atlas.test=192.0.2.10 --add-host packages.atlas.test=192.0.2.11",
		filepath.Join(root, spec.Context),
	} {
		if !strings.Contains(buildCall, want) {
			t.Errorf("build call lacks %q:\n%s", want, buildCall)
		}
	}
	for k, v := range spec.Args {
		if !strings.Contains(buildCall, "--build-arg "+k+"="+v) {
			t.Errorf("build call lacks arg %s", k)
		}
	}
	if strings.Contains(buildCall, "--network") {
		t.Errorf("network default is BuildKit's own, passed:\n%s", buildCall)
	}
	built1 := regexp.MustCompile(`--tag (\S+)`).FindStringSubmatch(buildCall)[1]
	if !strings.HasPrefix(calls[1], "image save --output ") || !strings.HasSuffix(calls[1], " "+built1) || calls[2] != "image rm --no-prune "+built1 {
		t.Fatalf("save / rm calls do not name the build's tag %s:\n%s", built1, raw)
	}
	// Another build of the same inputs at once gets a tag of its own.
	_, again, err := b.Build(context.Background(), p, BuildRequest{Image: built, Inputs: "sha256:0123456789abcdef0123", Version: "2026.10.25"})
	if err != nil {
		t.Fatal(err)
	}
	again()
	raw, _ = os.ReadFile(logf)
	if other := regexp.MustCompile(`--tag (\S+)`).FindAllStringSubmatch(string(raw), -1); len(other) != 2 || other[1][1] == built1 {
		t.Errorf("two builds of the same inputs share a tag: %v", other)
	}
}

// A recipe is built from its own bytes, with an empty context, whatever the spec holds.
func TestDockerBuilderBuildsARecipe(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	p, err := project.Open(root, "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	src, _ := random.Image(64, 1)
	tag, _ := name.NewTag("saved:1")
	archive := filepath.Join(dir, "saved.tar")
	if err := tarball.WriteToFile(archive, tag, src); err != nil {
		t.Fatal(err)
	}
	docker := filepath.Join(dir, "docker")
	if err := os.WriteFile(docker, []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	logf := filepath.Join(dir, "log")
	t.Setenv("FAKE_DOCKER_LOG", logf)
	t.Setenv("FAKE_DOCKER_ARCHIVE", archive)
	b := &DockerBuilder{Docker: docker, WorkDir: filepath.Join(dir, "work")}
	recipe := []byte("FROM scratch\n")
	_, cleanup, err := b.Build(context.Background(), p, BuildRequest{Image: "front", Inputs: "sha256:0123456789abcdef0123", Recipe: recipe})
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if df, _ := os.ReadFile(logf + ".dockerfile"); string(df) != string(recipe) {
		t.Errorf("built Dockerfile = %q, want the recipe", df)
	}
	if ls, err := os.ReadFile(logf + ".context"); err != nil || len(ls) != 0 {
		t.Errorf("the context is not empty: %q %v", ls, err)
	}
	raw, _ := os.ReadFile(logf)
	if !strings.Contains(string(raw), "--tag stowage-build/"+p.Spec.Name+"-front:0123456789abcdef-") {
		t.Errorf("build call:\n%s", raw)
	}
}

// A build with its own context (the OS bundle) is refused in a work directory in memory, before
// docker runs: its export is gigabytes.
func TestDockerBuilderRefusesTheBundleInMemory(t *testing.T) {
	saved := kairos.OnDisk
	kairos.OnDisk = func(dir string) error { return errors.New(dir + " is in memory") }
	t.Cleanup(func() { kairos.OnDisk = saved })
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	if err := os.WriteFile(docker, []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	logf := filepath.Join(dir, "log")
	t.Setenv("FAKE_DOCKER_LOG", logf)
	b := &DockerBuilder{Docker: docker, WorkDir: filepath.Join(dir, "work")}
	p, err := project.Open(testutil.CopyExample(t, "minimal"), "")
	if err != nil {
		t.Fatal(err)
	}
	_, cleanup, err := b.Build(context.Background(), p, BuildRequest{Image: "os", Inputs: "sha256:00", Recipe: []byte("FROM scratch\n"), Context: t.TempDir(), Large: true})
	if cleanup != nil {
		cleanup()
	}
	if err == nil || !strings.Contains(err.Error(), "is in memory") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(logf); err == nil {
		t.Error("docker ran")
	}
	// A build not marked large, even with a context of its own, is not refused.
	_, cleanup, err = b.Build(context.Background(), p, BuildRequest{Image: "front", Inputs: "sha256:00", Recipe: []byte("FROM scratch\n"), Context: t.TempDir()})
	if err != nil && strings.Contains(err.Error(), "is in memory") {
		t.Errorf("a small build refused: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if _, err := os.Stat(logf); err != nil {
		t.Error("docker did not run for the front")
	}
}
