package publish

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/norvexlabs/stowage/internal/graph"
	"github.com/norvexlabs/stowage/internal/kairos"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/spec"
)

// DockerBuilder builds with the local Docker engine (`docker buildx build --load`, so any builder
// driver hands its result to the engine) for graph.Platform, exports the result with `docker save`
// and rewrites it with OCI media types, which every OCI registry accepts. The export lands in
// WorkDir, on disk: an image can be gigabytes.
type DockerBuilder struct {
	Docker  string // the docker CLI; "docker" when empty
	WorkDir string
	Stdout  io.Writer
	Stderr  io.Writer
}

func (b *DockerBuilder) run(ctx context.Context, args ...string) error {
	docker := b.Docker
	if docker == "" {
		docker = "docker"
	}
	cmd := exec.CommandContext(ctx, docker, args...)
	cmd.Stdout, cmd.Stderr = b.Stdout, b.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w", strings.Join(args[:2], " "), err)
	}
	return nil
}

// Build implements Builder. A request with a Recipe builds it with its Context, or an empty one;
// any other builds the spec's image.
func (b *DockerBuilder) Build(ctx context.Context, p *project.Project, r BuildRequest) (v1.Image, func(), error) {
	var img spec.Image
	var df, ctxDir string
	if r.Recipe == nil {
		var ok bool
		if img, ok = p.Spec.Images[r.Image]; !ok || !img.Built() {
			return nil, nil, fmt.Errorf("image %s is not built by stowage", r.Image)
		}
		var err error
		if ctxDir, err = spec.ProjectPath(p.Root, img.Context); err != nil {
			return nil, nil, err
		}
		if df, err = spec.ProjectPath(p.Root, img.Dockerfile); err != nil {
			return nil, nil, err
		}
	}
	if err := os.MkdirAll(b.WorkDir, 0o700); err != nil {
		return nil, nil, err
	}
	if r.Large {
		if err := kairos.OnDisk(b.WorkDir); err != nil {
			return nil, nil, err
		}
	}
	dir, err := os.MkdirTemp(b.WorkDir, "build-")
	if err != nil {
		return nil, nil, err
	}
	id := strings.TrimPrefix(r.Inputs, "sha256:")
	if len(id) > 16 {
		id = id[:16]
	}
	// The build directory's random suffix too: two builds of the same inputs at once (two ships on
	// one host) must not share the tag one of them removes.
	tmpTag := fmt.Sprintf("stowage-build/%s-%s:%s-%s", p.Spec.Name, r.Image, id, strings.TrimPrefix(filepath.Base(dir), "build-"))
	cleanup := func() {
		_ = os.RemoveAll(dir)
		_ = b.run(context.WithoutCancel(ctx), "image", "rm", "--no-prune", tmpTag)
	}
	if r.Recipe != nil {
		df, ctxDir = filepath.Join(dir, "Dockerfile"), r.Context
		if err := os.WriteFile(df, r.Recipe, 0o600); err != nil {
			_ = os.RemoveAll(dir)
			return nil, nil, err
		}
		if ctxDir == "" {
			ctxDir = filepath.Join(dir, "context")
			if err := os.Mkdir(ctxDir, 0o700); err != nil {
				_ = os.RemoveAll(dir)
				return nil, nil, err
			}
		}
	}
	args := []string{"buildx", "build", "--load", "--platform", graph.Platform, "--file", df, "--tag", tmpTag}
	if img.Target != "" {
		args = append(args, "--target", img.Target)
	}
	keys := make([]string, 0, len(img.Args))
	for k := range img.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+img.Args[k])
	}
	if r.Version != "" {
		args = append(args, "--build-arg", graph.VersionArg+"="+r.Version)
	}
	args = append(args, ctxDir)
	if err := b.run(ctx, args...); err != nil {
		return nil, cleanup, err
	}
	archive := filepath.Join(dir, "image.tar")
	if err := b.run(ctx, "image", "save", "--output", archive, tmpTag); err != nil {
		return nil, cleanup, err
	}
	saved, err := tarball.ImageFromPath(archive, nil)
	if err != nil {
		return nil, cleanup, fmt.Errorf("read the saved image: %w", err)
	}
	out, err := toOCI(saved)
	return out, cleanup, err
}

// toOCI rewrites an image with OCI media types; layer bytes and the config are unchanged.
func toOCI(img v1.Image) (v1.Image, error) {
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	layers, err := img.Layers()
	if err != nil {
		return nil, err
	}
	base := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	base = mutate.ConfigMediaType(base, types.OCIConfigJSON)
	adds := make([]mutate.Addendum, len(layers))
	for i, l := range layers {
		adds[i] = mutate.Addendum{Layer: ociLayer{l}, MediaType: types.OCILayer}
	}
	out, err := mutate.Append(base, adds...)
	if err != nil {
		return nil, err
	}
	return mutate.ConfigFile(out, cf)
}

// ociLayer is a gzip layer presented with the OCI media type (the bytes are the same).
type ociLayer struct{ v1.Layer }

func (ociLayer) MediaType() (types.MediaType, error) { return types.OCILayer, nil }
