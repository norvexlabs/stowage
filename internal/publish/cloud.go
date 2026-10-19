package publish

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/norvexlabs/stowage/internal/cloud"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
)

// CloudUploader registers a disk as an image on a cloud, once: Registered is the id of the image
// already registered ("" when none).
type CloudUploader interface {
	Registered(ctx context.Context, img cloud.Image) (string, error)
	Upload(ctx context.Context, img cloud.Image) (string, error)
}

// publishClouds registers, on each cloud of cloud_publish the publisher has settings for, the disk
// it imports as an image named after the project and the disk's digest: the published disk, read
// back from the registry, so the image is what an install of the release fetches. A cloud without
// settings is named and left out.
func (p *Publisher) publishClouds(ctx context.Context, disks []*artifact) error {
	for _, c := range p.Project.Spec.Shapes.Kairos.CloudPublish {
		vars := p.CloudVars[c]
		if vars == nil {
			p.logf("cloud %s: not uploaded (set %s): its image is published as a disk only", c, strings.Join(cloud.Required(c), ", "))
			continue
		}
		if p.Cloud == nil {
			return fmt.Errorf("cloud %s: no cloud uploader", c)
		}
		f := spec.CloudFormat[c]
		i := slices.IndexFunc(disks, func(a *artifact) bool { return a.format == f })
		if i < 0 {
			return fmt.Errorf("cloud %s: no %s disk", c, f)
		}
		d, err := p.Registry.Digest(p.diskRepo(), disks[i].digest)
		if err != nil {
			return err
		}
		name := p.Project.Spec.Name + "-" + d.DigestStr()[len("sha256:"):][:12]
		img := cloud.Image{
			Cloud: c, Name: name, Vars: vars,
			Description: fmt.Sprintf("%s %s disk %s, published by Stowage", p.Project.Spec.Name, f, d.DigestStr()),
			Fetch: func(ctx context.Context, dir string) (string, string, error) {
				file, h, err := p.Registry.Download(ctx, d, release.DiskArtifactType, release.DiskMediaType(f), dir, name+"."+f, nil)
				return file, h.Hex, err
			},
		}
		id, err := p.Cloud.Registered(ctx, img)
		if err != nil {
			return err
		}
		if id == "" {
			p.logf("cloud %s: register %s", c, name)
			if id, err = p.Cloud.Upload(ctx, img); err != nil {
				return err
			}
		}
		p.logf("cloud %s: image %s", c, id)
	}
	return nil
}
