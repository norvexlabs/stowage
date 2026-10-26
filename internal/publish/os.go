package publish

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/norvexlabs/stowage/internal/graph"
	"github.com/norvexlabs/stowage/internal/kairos"
	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/sbom"
	"github.com/norvexlabs/stowage/internal/version"
)

// DiskMaker makes the disk images of an OS bundle (kairos.DiskBuilder).
type DiskMaker interface {
	Build(ctx context.Context, r kairos.DiskRequest) (kairos.Disks, func(), error)
}

// FormatAnnotation names a disk artifact's format.
const FormatAnnotation = "dev.stowage.disk.format"

// RootfsAnnotation is the OS bundle's unpacked size in MiB, which sizes its disks' state
// partition; each disk records release.DiskOSAnnotation, as its build measured it.
const RootfsAnnotation = "dev.stowage.os.rootfs-mib"

// hasOS reports whether the project publishes an OS bundle: it has the kairos shape.
func (p *Publisher) hasOS() bool { return p.Project.Spec.Shapes.Kairos != nil }

func (p *Publisher) osRepo() string {
	return p.Project.Spec.Registry.Prefix + "/" + p.imageName("os")
}

func (p *Publisher) diskRepo() string {
	return p.Project.Spec.Registry.Prefix + "/" + p.imageName("disk")
}

// osVersion is the version the OS bundle carries: the release's, without its channel.
func (p *Publisher) osVersion(v string, revision int) (string, error) {
	st, err := p.Project.Scheme.Tag(v)
	if err != nil {
		return "", err
	}
	return version.ChannelTag("{v}", st, revision)
}

// artifact is a planned artifact kept on a key: an OS bundle or a disk.
type artifact struct {
	what   string // for messages: "os bundle", "disk qcow2"
	format string // a disk's format
	tag    name.Tag
	key    string
	action imageAction // keep, retag or build
	from   name.Digest
	digest string
}

// planKept finds the artifact kept on key: under its tag (kept), or among the digests sources
// recorded (re-tagged). Otherwise it is built, unless a tag of an immutable channel already holds
// another.
func (p *Publisher) planKept(ctx context.Context, a *artifact, mutable bool, candidates []string) error {
	ann, d, exists, err := p.Registry.Annotations(ctx, a.tag)
	if err != nil {
		return err
	}
	if exists {
		if ann[registry.InputsAnnotation] == a.key {
			a.action, a.digest = actKeep, d.String()
			return nil
		} else if !mutable {
			return fmt.Errorf("%s: %s exists with other inputs on an immutable channel", a.what, a.tag)
		}
	}
	repo := a.tag.Context().Name()
	seen := map[string]bool{}
	for _, c := range candidates {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		ref, err := p.Registry.Digest(repo, c)
		if err != nil {
			return err
		}
		ann, _, ok, err := p.Registry.Annotations(ctx, ref)
		if err != nil {
			return err
		}
		if ok && ann[registry.InputsAnnotation] == a.key {
			a.action, a.from, a.digest = actRetag, ref, c
			return nil
		}
	}
	a.action = actBuild
	return nil
}

// tag points the artifact's tag at its digest.
func (p *Publisher) tagArtifact(ctx context.Context, a *artifact) error {
	if a.action == actKeep {
		p.logf("%s: %s unchanged", a.what, a.tag.TagStr())
		return nil
	}
	p.logf("%s: tag %s → %s", a.what, registry.Short(a.digest), a.tag.TagStr())
	return p.Registry.Retag(ctx, a.from, a.tag)
}

// osPlan is the OS bundle and its disks a release publishes.
type osPlan struct {
	os    *artifact
	disks []*artifact
}

// planOS plans the bundle on the images about to be published, and builds what is missing: the
// bundle, then (with disks) the disks of it no source holds. Everything is pushed by digest and signed; no tag
// moves. existing is the release published under the tag on an immutable channel (nil
// otherwise): it is never republished with another bundle or disk.
func (p *Publisher) planOS(ctx context.Context, o ShipOptions, disks bool, tag string, mutable bool, osInputs string, images, configs, manifests map[string]string, sources []*Ref, immutable *Ref) (*osPlan, error) {
	s := p.Project.Spec
	newRevision := func(what, had string) error {
		if had == "" {
			had = "none"
		}
		return fmt.Errorf("%s: %s %s is published on the immutable channel %s with %s %s, and it would now differ; publish a new revision (--revision %d)",
			what, s.Name, tag, o.Channel, what, had, o.Revision+1)
	}
	t, err := p.Registry.Tag(p.osRepo(), tag)
	if err != nil {
		return nil, err
	}
	pl := &osPlan{os: &artifact{what: "os bundle", tag: t, key: graph.OSKey(osInputs, images)}}
	var cands []string
	for _, src := range sources {
		cands = append(cands, src.Manifest.Artifacts.OS)
	}
	if err := p.planKept(ctx, pl.os, mutable, cands); err != nil {
		return nil, err
	}
	if immutable != nil && (pl.os.action == actBuild || pl.os.digest != immutable.Manifest.Artifacts.OS) {
		return nil, newRevision("os bundle", immutable.Manifest.Artifacts.OS)
	}
	if pl.os.action == actBuild {
		if o.NoBuild {
			return nil, fmt.Errorf("os bundle: no published bundle has these inputs (%s); the tree differs from the release being promoted", pl.os.key)
		}
		if err := p.buildOS(ctx, o, pl.os, images, configs, manifests); err != nil {
			return nil, err
		}
	}
	osRef, err := p.Registry.Digest(p.osRepo(), pl.os.digest)
	if err != nil {
		return nil, err
	}
	if err := p.ensureSigned(ctx, osRef); err != nil {
		return nil, fmt.Errorf("os bundle: %w", err)
	}
	if err := p.ensureSBOM(ctx, osRef); err != nil {
		return nil, fmt.Errorf("os bundle: %w", err)
	}

	if !disks {
		return pl, nil
	}
	var missing []string
	for _, f := range p.Project.Spec.Shapes.Kairos.Formats() {
		dt, err := p.Registry.Tag(p.diskRepo(), release.DiskTag(tag, f))
		if err != nil {
			return nil, fmt.Errorf("disk %s: %w", f, err)
		}
		a := &artifact{what: "disk " + f, format: f, tag: dt, key: graph.DiskKey(pl.os.digest, f)}
		var cands []string
		for _, src := range sources {
			cands = append(cands, src.Manifest.Artifacts.Disks[f])
		}
		if err := p.planKept(ctx, a, mutable, cands); err != nil {
			return nil, err
		}
		if immutable != nil && (a.action == actBuild || a.digest != immutable.Manifest.Artifacts.Disks[f]) {
			return nil, newRevision("disk "+f, immutable.Manifest.Artifacts.Disks[f])
		}
		if a.action == actBuild {
			if o.NoBuild {
				return nil, fmt.Errorf("disk %s: no published disk has these inputs (%s); the tree differs from the release being promoted", f, a.key)
			}
			missing = append(missing, f)
		}
		pl.disks = append(pl.disks, a)
	}
	if len(missing) > 0 {
		if err := p.buildDisks(ctx, o, osRef, missing, pl.disks); err != nil {
			return nil, err
		}
	}
	for _, a := range pl.disks {
		d, err := p.Registry.Digest(p.diskRepo(), a.digest)
		if err != nil {
			return nil, err
		}
		if err := p.ensureSigned(ctx, d); err != nil {
			return nil, fmt.Errorf("%s: %w", a.what, err)
		}
		if err := p.ensureDiskSBOM(ctx, d, osRef); err != nil {
			return nil, fmt.Errorf("%s: %w", a.what, err)
		}
	}
	if err := p.publishClouds(ctx, pl.disks); err != nil {
		return nil, err
	}
	return pl, nil
}

// preloaded are the images the OS bundle preloads: every one of the release's but the scanner,
// which an operator runs off the VM. Its <short>-stow carries the release with all of them, so
// the bundle's key covers the scanner too.
func preloaded(images map[string]string) map[string]string {
	out := maps.Clone(images)
	delete(out, sbom.Key)
	return out
}

// buildOS builds the bundle with the release's images preloaded and pushes it by digest.
func (p *Publisher) buildOS(ctx context.Context, o ShipOptions, a *artifact, images, configs, manifests map[string]string) error {
	if p.WorkDir == "" {
		return errors.New("os bundle: no work directory to build in")
	}
	k := p.Project.Spec.Shapes.Kairos
	ov, err := p.osVersion(o.Version, o.Revision)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(p.WorkDir, 0o700); err != nil {
		return err
	}
	// The context holds an archive of every image (the builder checks where it exports the bundle).
	if err := kairos.OnDisk(p.WorkDir); err != nil {
		return fmt.Errorf("os bundle: %w", err)
	}
	params := kairos.Params{Name: p.Project.Spec.Name, Ctl: providers.Ctl(p.Project.Spec.Short()), Base: k.Base, KairosInit: k.KairosInit, OSVersion: ov}
	if h := p.Project.Spec.Host; h != nil {
		params.Modules, params.Packages, params.Firmware, params.KernelArgs = h.Modules, h.Packages, h.Firmware, h.KernelArgs
		params.Interfaces = h.Interfaces
	}
	dir, err := os.MkdirTemp(p.WorkDir, "os-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := kairos.WriteContext(dir, params); err != nil {
		return fmt.Errorf("os bundle: %w", err)
	}
	for _, img := range sortedKeys(preloaded(images)) {
		p.logf("os bundle: preload %s", img)
		if err := p.preload(ctx, img, images[img], manifests[img], filepath.Join(dir, kairos.PreloadDir, img+".tar")); err != nil {
			return fmt.Errorf("os bundle: preload %s: %w", img, err)
		}
	}
	ctl, err := p.bundledCtl(o, a.key, images, configs, manifests)
	if err != nil {
		return fmt.Errorf("os bundle: %s: %w", providers.Ctl(p.Project.Spec.Short()), err)
	}
	if err := os.WriteFile(filepath.Join(dir, kairos.CtlDir, providers.Ctl(p.Project.Spec.Short())), ctl, 0o755); err != nil {
		return err
	}
	df, err := kairos.Dockerfile(params)
	if err != nil {
		return err
	}
	p.logf("os bundle: build %s", ov)
	built, cleanup, err := p.Builder.Build(ctx, p.Project, BuildRequest{Image: "os", Inputs: a.key, Recipe: df, Context: dir, Large: true})
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()
	if err != nil {
		return fmt.Errorf("os bundle: build: %w", err)
	}
	rootfs, err := unpackedMiB(built)
	if err != nil {
		return fmt.Errorf("os bundle: its size: %w", err)
	}
	if rootfs <= 0 {
		return errors.New("os bundle: its root filesystem holds no file")
	}
	p.logf("os bundle: %d MiB unpacked", rootfs)
	annotated, ok := mutate.Annotations(built, map[string]string{registry.InputsAnnotation: a.key, RootfsAnnotation: strconv.FormatInt(rootfs, 10)}).(v1.Image)
	if !ok {
		return errors.New("os bundle: cannot annotate")
	}
	d, err := annotated.Digest()
	if err != nil {
		return err
	}
	dst, err := p.Registry.Digest(p.osRepo(), d.String())
	if err != nil {
		return err
	}
	if err := p.Registry.WriteImage(ctx, dst, annotated); err != nil {
		return err
	}
	a.action, a.from, a.digest = actRetag, dst, d.String()
	return nil
}

// bundledCtl is the installer the bundle carries as <short>-stow: the installer executable with the
// release as the bundle carries it — the manifest without its publication, which the bundle is
// built before and serves every channel of (release.Manifest.EncodeBundled). Its inputs digest is
// the bundle's key, which covers everything it holds.
func (p *Publisher) bundledCtl(o ShipOptions, key string, images, configs, manifests map[string]string) ([]byte, error) {
	s := p.Project.Spec
	text, _, err := p.Project.Changelog.Section(o.Version)
	if err != nil {
		return nil, fmt.Errorf("changelog: %w", err)
	}
	m := release.Manifest{
		SchemaVersion:  release.SchemaVersion,
		Name:           s.Name,
		ShortName:      s.Short(),
		ProjectVersion: o.Version,
		Revision:       o.Revision,
		Inputs:         key,
		Scheme:         release.SchemeOf(s.Version.Scheme),
		Artifacts:      release.Artifacts{Images: images, Configs: configs, Manifests: manifests},
		Changelog:      release.Changelog{Project: text},
	}
	man, err := m.EncodeBundled()
	if err != nil {
		return nil, err
	}
	files, err := p.payloadFiles(payload.Bundled, man, s.BundleView())
	if err != nil {
		return nil, err
	}
	var exe bytes.Buffer
	if err := payload.Write(&exe, bytes.NewReader(p.Installer), files); err != nil {
		return nil, err
	}
	return exe.Bytes(), nil
}

// preload writes image img's linux/amd64 image (its manifest digest) as an OCI layout archive,
// which `docker load` reads: the manifest and layers as published, named
// <repository>:PreloadTag(digest).
func (p *Publisher) preload(ctx context.Context, img, digest, manifest, path string) (err error) {
	repo := p.imageRepo(img)
	ref, err := p.Registry.Digest(repo, manifest)
	if err != nil {
		return err
	}
	image, err := p.Registry.Image(ctx, ref)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	return writeLayout(f, image, repo+":"+release.PreloadTag(digest))
}

// writeLayout writes img as a single-image OCI layout tar named ref.
func writeLayout(w io.Writer, img v1.Image, ref string) error {
	tw := tar.NewWriter(w)
	file := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	blob := func(h v1.Hash) string { return "blobs/" + h.Algorithm + "/" + h.Hex }
	written := map[v1.Hash]bool{}
	layers, err := img.Layers()
	if err != nil {
		return err
	}
	for _, l := range layers {
		h, err := l.Digest()
		if err != nil {
			return err
		}
		if written[h] {
			continue
		}
		written[h] = true
		n, err := l.Size()
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: blob(h), Mode: 0o644, Size: n, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		rc, err := l.Compressed()
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, rc)
		rc.Close()
		if err != nil {
			return err
		}
	}
	cfg, err := img.RawConfigFile()
	if err != nil {
		return err
	}
	cn, err := img.ConfigName()
	if err != nil {
		return err
	}
	if !written[cn] {
		if err := file(blob(cn), cfg); err != nil {
			return err
		}
	}
	raw, err := img.RawManifest()
	if err != nil {
		return err
	}
	md, err := img.Digest()
	if err != nil {
		return err
	}
	mt, err := img.MediaType()
	if err != nil {
		return err
	}
	if err := file(blob(md), raw); err != nil {
		return err
	}
	_, tagStr, _ := strings.Cut(ref[strings.LastIndex(ref, "/")+1:], ":")
	index, err := json.Marshal(v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     types.OCIImageIndex,
		Manifests: []v1.Descriptor{{MediaType: mt, Digest: md, Size: int64(len(raw)), Annotations: map[string]string{
			"io.containerd.image.name":          ref,
			"org.opencontainers.image.ref.name": tagStr,
		}}},
	})
	if err != nil {
		return err
	}
	if err := file("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)); err != nil {
		return err
	}
	if err := file("index.json", index); err != nil {
		return err
	}
	return tw.Close()
}

// unpackedMiB is the size of img's root filesystem, its layers flattened (a file a later layer
// replaces or deletes is not counted), in MiB rounded up.
func unpackedMiB(img v1.Image) (int64, error) {
	rc := mutate.Extract(img)
	defer rc.Close()
	tr := tar.NewReader(rc)
	var n int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
		if h.Typeflag == tar.TypeReg {
			n += h.Size
		}
	}
	return (n + 1<<20 - 1) >> 20, nil
}

// bundleMiB is the OS bundle's unpacked size, as its build recorded it: a bundle recording none
// makes no disk.
func (p *Publisher) bundleMiB(ctx context.Context, osRef name.Digest) (int64, error) {
	ann, _, ok, err := p.Registry.Annotations(ctx, osRef)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("disks: the OS bundle %s is not in the registry", osRef)
	}
	n, perr := strconv.ParseInt(ann[RootfsAnnotation], 10, 64)
	if perr != nil || n <= 0 {
		return 0, fmt.Errorf("disks: the OS bundle %s does not record its size (%s): it was built by an earlier Stowage; publish a new revision", osRef.DigestStr(), RootfsAnnotation)
	}
	return n, nil
}

// buildDisks makes the disks of formats from the bundle at osRef and pushes each by digest.
func (p *Publisher) buildDisks(ctx context.Context, o ShipOptions, osRef name.Digest, formats []string, plans []*artifact) error {
	if p.Disks == nil {
		return errors.New("disks: no disk builder")
	}
	ov, err := p.osVersion(o.Version, o.Revision)
	if err != nil {
		return err
	}
	base := p.Project.Spec.Name + "-" + ov
	// The disk builder runs a third-party image, privileged: it reads the bundle from a loopback
	// proxy of that one image, with the proxy's own one-run account (in its Docker config file),
	// while the proxy holds the publisher's; so it never holds a registry credential, and a disk
	// never depends on who else may read the registry.
	rootfs, err := p.bundleMiB(ctx, osRef)
	if err != nil {
		return err
	}
	source, acct, stop, err := p.Registry.ReadProxy(ctx, osRef)
	if err != nil {
		return fmt.Errorf("disks: %w", err)
	}
	defer stop()
	host, _, _ := strings.Cut(source, "/")
	p.logf("disks: make %s", strings.Join(formats, ", "))
	made, cleanup, err := p.Disks.Build(ctx, kairos.DiskRequest{Source: source, Registry: host, Auth: acct, Formats: formats, Base: base, RootfsMiB: rootfs})
	files := made.Files
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()
	if err != nil {
		return fmt.Errorf("disks: %w", err)
	}
	if made.OSMiB <= 0 {
		return fmt.Errorf("disks: the builder measured no size of what Kairos takes of a disk (%d MiB)", made.OSMiB)
	}
	repo, err := p.Registry.Repo(p.diskRepo())
	if err != nil {
		return err
	}
	for _, a := range plans {
		if a.action != actBuild {
			continue
		}
		f := a.format
		path := files[f]
		if path == "" {
			return fmt.Errorf("disk %s: the disk builder made none", f)
		}
		// The layer is the file compressed with zstd, titled with the name `oras pull` writes it to.
		p.logf("disk %s: compress %s", f, filepath.Base(path))
		b, err := registry.ZstdFileBlob(release.DiskMediaType(f), path)
		if err != nil {
			return fmt.Errorf("disk %s: %w", f, err)
		}
		// Nothing reads the file again (a cloud upload downloads the disk back): its space goes.
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("disk %s: %w", f, err)
		}
		m := registry.Manifest{
			ArtifactType: release.DiskArtifactType,
			Config:       registry.Blob{MediaType: registry.EmptyMediaType, Data: []byte("{}")},
			Layers:       []registry.Blob{b},
			Annotations: map[string]string{
				registry.InputsAnnotation: a.key,
				FormatAnnotation:          f,
				release.DiskOSAnnotation:  strconv.FormatInt(made.OSMiB, 10),
			},
		}
		p.logf("disk %s: push %s", f, b.Annotations[registry.TitleAnnotation])
		h, err := p.Registry.PutManifest(ctx, repo, m, nil)
		if err != nil {
			return fmt.Errorf("disk %s: %w", f, err)
		}
		a.action, a.from, a.digest = actRetag, repo.Digest(h.String()), h.String()
	}
	return nil
}
