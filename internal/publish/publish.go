// Package publish puts a project's releases on its registry and manages them there: ship, verify,
// promote, yank, prune and the release listing.
//
// Ship is idempotent. Every image carries its inputs digest as an annotation; an image whose tag
// already carries the same digest is left alone, one found under another release's tag is
// re-tagged, and only the rest is built. On an immutable channel, a tag that exists with other
// inputs is refused before anything is written. The channel pointer moves last, after the release
// verified.
package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"

	"github.com/norvexlabs/stowage/internal/graph"
	"github.com/norvexlabs/stowage/internal/lock"
	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/version"
)

// BuildRequest is one image to build.
type BuildRequest struct {
	Image string // the spec's images key
	// Version is the project version, passed as graph.VersionArg when the Dockerfile reads it.
	Version string
	Inputs  string
	// Recipe is the Dockerfile of an image Stowage itself supplies (the front, the OS bundle);
	// nil for a spec image. It is built with Context, a directory, or with an empty context.
	Recipe  []byte
	Context string
	// Large marks a build whose export is gigabytes (the OS bundle): it is refused in a work
	// directory in memory.
	Large bool
}

// Builder builds a project image. The cleanup function is always safe to call.
type Builder interface {
	Build(ctx context.Context, p *project.Project, r BuildRequest) (v1.Image, func(), error)
}

// Publisher acts on one project's registry.
type Publisher struct {
	Project *project.Project
	// Registry writes with the publisher's credentials; Upstream reads pinned upstream images;
	// Anonymous reads with no account, as an install does when registry.pull is none (nil checks
	// nothing).
	Registry, Upstream, Anonymous *registry.Client
	Builder                       Builder
	// Signer signs and checks every image and release manifest when the project configures
	// signing; it is required then.
	Signer Signer
	// SBOM makes an image's SBOM, which a signing project attests on every image, the installer and
	// the OS bundle; it is required then.
	SBOM SBOMMaker
	// Scanner is the vulnerability scanner a signing release carries, pinned: sbom.Grype when empty.
	Scanner        string
	StowageVersion string
	// Installer is the installer executable every release's installer image is made of; Pull is
	// the pull account baked into it (nil when registry.pull is not a token).
	Installer []byte
	Pull      *payload.Account
	// Disks makes the OS bundle's disk images, and WorkDir (on disk) holds the bundle's build
	// context: both are needed for a project with the kairos shape.
	Disks   DiskMaker
	WorkDir string
	// Cloud registers the disks of cloud_publish's clouds as images; CloudVars are each cloud's
	// settings, a cloud without any not uploaded to.
	Cloud     CloudUploader
	CloudVars map[string]map[string]string
	// CloudErr, when set, refuses a ship or a promote (the clouds' settings could not be read);
	// the other commands never register an image.
	CloudErr error
	// Deleter deletes a manifest by digest (registry.delete); nil is the registry's distribution
	// API.
	Deleter Deleter
	// checkDisksOf is the release tag whose disks the running ship published: verify checks them
	// before the channel's set names them.
	checkDisksOf string
	Now          func() time.Time
	Log          io.Writer
}

func (p *Publisher) logf(format string, a ...any) {
	if p.Log != nil {
		fmt.Fprintf(p.Log, format+"\n", a...)
	}
}

// imageName is an image's repository under the registry prefix.
func (p *Publisher) imageName(image string) string { return p.Project.Spec.Name + "-" + image }

func (p *Publisher) imageRepo(image string) string {
	return p.Project.Spec.Registry.Prefix + "/" + p.imageName(image)
}

func (p *Publisher) releaseRepo() string {
	return p.Project.Spec.Registry.Prefix + "/" + p.Project.Spec.Name + "-release"
}

// Ref is a published release manifest and its digest.
type Ref struct {
	Manifest *release.Manifest
	Digest   v1.Hash
}

func (p *Publisher) readRelease(ctx context.Context, tag string) (*Ref, error) {
	t, err := p.Registry.Tag(p.releaseRepo(), tag)
	if err != nil {
		return nil, err
	}
	a, d, err := p.Registry.GetArtifact(ctx, t, release.MediaType)
	if err != nil || a == nil {
		return nil, err
	}
	m, err := release.Decode(a.Data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %w", t, registry.ErrNotArtifact, err)
	}
	if m.Name != p.Project.Spec.Name {
		return nil, fmt.Errorf("%s: release of %q, not %q: %w", t, m.Name, p.Project.Spec.Name, registry.ErrNotArtifact)
	}
	return &Ref{m, d}, nil
}

// writeRelease pushes m by digest, signs it, and only then moves tag to it: a release tag never
// holds an unsigned manifest of a signed project.
func (p *Publisher) writeRelease(ctx context.Context, tag string, m *release.Manifest) (v1.Hash, error) {
	data, err := m.Encode()
	if err != nil {
		return v1.Hash{}, err
	}
	t, err := p.Registry.Tag(p.releaseRepo(), tag)
	if err != nil {
		return v1.Hash{}, err
	}
	d, err := p.Registry.PushArtifact(ctx, t.Repository, registry.Artifact{
		Type: release.MediaType, Data: data,
		Annotations: map[string]string{registry.InputsAnnotation: m.Inputs},
	})
	if err != nil {
		return v1.Hash{}, err
	}
	if err := p.ensureSigned(ctx, d); err != nil {
		return v1.Hash{}, err
	}
	if err := p.Registry.Retag(ctx, d, t); err != nil {
		return v1.Hash{}, err
	}
	return v1.NewHash(d.DigestStr())
}

// signed reports whether the project signs its releases.
func (p *Publisher) signed() bool { return p.Project.Spec.Signs() }

// ensureSigned signs d unless a trusted key already has, so a re-ship adds no signature, then
// checks the new signature verifies with a trusted key — before the caller moves any tag to d.
func (p *Publisher) ensureSigned(ctx context.Context, d name.Digest) error {
	if !p.signed() {
		return nil
	}
	if p.Signer == nil {
		return errors.New("signing is configured and no signer is set: refusing to publish an unsigned artifact")
	}
	err := p.Signer.Verify(ctx, d)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrUnsigned) {
		return err
	}
	if err := p.Signer.Sign(ctx, d); err != nil {
		return fmt.Errorf("sign %s: %w", d, err)
	}
	if err := p.Signer.Verify(ctx, d); err != nil {
		return fmt.Errorf("signed %s, and the signature does not verify (is the signing key's public half in signing.public_keys?): %w", d, err)
	}
	p.logf("signed %s", d)
	return nil
}

// ensureDiskSBOM attests on the disk d the SBOM of the bundle it is made of, unless a trusted key
// already has: a disk holds the bundle's software, so its SBOM is the bundle's, read back attested.
func (p *Publisher) ensureDiskSBOM(ctx context.Context, d, bundle name.Digest) error {
	if !p.signed() {
		return nil
	}
	_, err := p.Signer.Attestation(ctx, d)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrNoAttestation) {
		return err
	}
	doc, err := p.Signer.Attestation(ctx, bundle)
	if err != nil {
		return fmt.Errorf("the SBOM of the bundle %s, for its disk: %w", bundle, err)
	}
	if err := p.Signer.Attest(ctx, d, doc); err != nil {
		return fmt.Errorf("attest the SBOM of %s: %w", d, err)
	}
	if _, err := p.Signer.Attestation(ctx, d); err != nil {
		return fmt.Errorf("attested the SBOM of %s, and the attestation does not read back by a trusted key: %w", d, err)
	}
	p.logf("attested the SBOM of %s (its bundle's)", d)
	return nil
}

// SBOMMaker makes an image's SBOM (SPDX JSON) from the registry.
type SBOMMaker interface {
	SBOM(ctx context.Context, d name.Digest) ([]byte, error)
}

// ensureSBOM attests d's SBOM unless a trusted key already has, so a re-ship makes none, then
// checks the attestation reads back with a trusted key. A signing project's images, installer
// and OS bundle each carry one; the SBOM of a digest never changes, so a later syft makes no new
// one.
func (p *Publisher) ensureSBOM(ctx context.Context, d name.Digest) error {
	if !p.signed() {
		return nil
	}
	if p.Signer == nil || p.SBOM == nil {
		return errors.New("signing is configured and no signer or SBOM maker is set: refusing to publish an image without its SBOM")
	}
	_, err := p.Signer.Attestation(ctx, d)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrNoAttestation) {
		return err
	}
	doc, err := p.SBOM.SBOM(ctx, d)
	if err != nil {
		return fmt.Errorf("the SBOM of %s: %w", d, err)
	}
	if err := p.Signer.Attest(ctx, d, doc); err != nil {
		return fmt.Errorf("attest the SBOM of %s: %w", d, err)
	}
	if _, err := p.Signer.Attestation(ctx, d); err != nil {
		return fmt.Errorf("attested the SBOM of %s, and the attestation does not read back by a trusted key: %w", d, err)
	}
	p.logf("attested the SBOM of %s", d)
	return nil
}

// pointer reads a channel's pointer; nil when the channel has none yet.
func (p *Publisher) pointer(ctx context.Context, channel string) (*Ref, error) {
	return p.readRelease(ctx, channel)
}

func (p *Publisher) movePointer(ctx context.Context, channel string, to *Ref) error {
	src, err := p.Registry.Digest(p.releaseRepo(), to.Digest.String())
	if err != nil {
		return err
	}
	dst, err := p.Registry.Tag(p.releaseRepo(), channel)
	if err != nil {
		return err
	}
	if err := p.Registry.Retag(ctx, src, dst); err != nil {
		return err
	}
	d, err := p.Registry.Head(ctx, dst)
	if err != nil {
		return err
	}
	if d == nil || d.Digest != to.Digest {
		return fmt.Errorf("channel pointer %s did not move to %s", dst, to.Digest)
	}
	return nil
}

// releaseTag is the tag a version ships under on a channel at a revision.
func (p *Publisher) releaseTag(v, channel string, revision int) (string, error) {
	ch, ok := p.Project.Spec.Channels[channel]
	if !ok {
		return "", fmt.Errorf("channel %q is not declared", channel)
	}
	st, err := p.Project.Scheme.Tag(v)
	if err != nil {
		return "", err
	}
	return version.ChannelTag(ch.Tag, st, revision)
}

// ShipOptions selects what Ship publishes.
type ShipOptions struct {
	Version  string
	Channel  string
	Revision int
	// NoBuild refuses to build: every image must already exist with the same inputs (promote).
	NoBuild bool
	// Sources are releases whose images may be re-tagged, beyond the channels' pointers.
	Sources []*Ref
	// Promoted is the release a promotion re-publishes: its changelog is carried over, and its
	// runtime pins must equal the tree's.
	Promoted *Ref
	// Disks publishes the kairos shape's disks (and registers cloud_publish's images); without
	// it the release publishes none and its installs boot its channel's disk set. A promotion
	// publishes the disks the release it promotes published.
	Disks bool
}

type imageAction int

const (
	actKeep imageAction = iota
	actRetag
	actCopy
	actBuild
)

type imagePlan struct {
	node   graph.Node
	image  string
	tag    name.Tag
	action imageAction
	from   name.Digest // retag / copy source
	digest string      // known up front for keep, retag and copy
}

// Ship publishes the project's current tree as a release of the channel.
func (p *Publisher) Ship(ctx context.Context, o ShipOptions) (*Ref, error) {
	s := p.Project.Spec
	if p.signed() && p.Signer == nil {
		return nil, errors.New("signing is configured and no signer is set: refusing to publish an unsigned release")
	}
	if err := p.readyToPublish(); err != nil {
		return nil, err
	}
	ch, ok := s.Channels[o.Channel]
	if !ok {
		return nil, fmt.Errorf("channel %q is not declared", o.Channel)
	}
	// The disk builder is checked before anything is built; a promotion builds nothing.
	if p.hasOS() && len(s.Shapes.Kairos.Formats()) > 0 && o.Disks && !o.NoBuild && p.Disks == nil {
		return nil, errors.New("disks: no disk builder")
	}
	tag, err := p.releaseTag(o.Version, o.Channel, o.Revision)
	if err != nil {
		return nil, err
	}
	g, err := p.Graph(o.Version, o.Revision, o.Channel)
	if err != nil {
		return nil, err
	}
	relNode, _ := g.Node(graph.KindRelease)

	existing, err := p.readRelease(ctx, tag)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.Manifest.Inputs != relNode.Inputs && !ch.Mutable {
		return nil, fmt.Errorf("%s %s is already published on the immutable channel %s with other inputs; publish a new revision (--revision %d)",
			s.Name, tag, o.Channel, o.Revision+1)
	}

	// A mutable tag rebuilt with other inputs and no --disks would keep disks of the build it
	// replaces, whose OS bundle is not the new one's: refused before anything is written.
	if p.hasOS() && !o.Disks && existing != nil && existing.Manifest.Inputs != relNode.Inputs && len(existing.Manifest.Artifacts.Disks) > 0 {
		there, err := p.present(ctx, diskSet{disks: existing.Manifest.Artifacts.Disks})
		if err != nil {
			return nil, err
		}
		if there {
			return nil, fmt.Errorf("%s %s is published with disks, and its build is replaced: ship it with --disks, so its disks are the new build's", s.Name, tag)
		}
	}
	sources := append([]*Ref(nil), o.Sources...)
	if existing != nil {
		sources = append(sources, existing)
	}
	for _, c := range s.ChannelNames() {
		r, err := p.pointer(ctx, c)
		if err != nil {
			return nil, err
		}
		if r != nil {
			// An install keeps its installer under one name and refuses a release that renames
			// it, so the rename is refused here, once, rather than by every install after it.
			if r.Manifest.ShortName != s.Short() {
				return nil, fmt.Errorf("short_name: the channel %s's release %s names its installer %s, this spec %s; an install keeps its installer under one name, so a project does not rename it (publish under another registry prefix)",
					c, r.Manifest.ProjectVersion, providers.Ctl(r.Manifest.ShortName), providers.Ctl(s.Short()))
			}
			sources = append(sources, r)
		}
	}

	// Plan every image before writing anything, so a refusal leaves the registry untouched.
	var plans []*imagePlan
	for _, n := range g.Nodes {
		if n.Kind != graph.KindImage {
			continue
		}
		// An image tag with no release under its tag was left by a publication that never
		// completed: nothing was published, so it may be replaced even on an immutable channel.
		pl, err := p.planImage(ctx, n, tag, ch.Mutable || existing == nil, o.NoBuild, sources)
		if err != nil {
			return nil, err
		}
		plans = append(plans, pl)
	}
	// A published release on an immutable channel is never republished with other artifacts: an
	// image that would be rebuilt or point elsewhere (registry retention removed one) is refused.
	if existing != nil && !ch.Mutable {
		for _, pl := range plans {
			if pl.action == actBuild || pl.digest != existing.Manifest.Artifacts.Images[pl.image] {
				return nil, fmt.Errorf("image %s: %s %s is published on the immutable channel %s and its image is no longer %s; publish a new revision (--revision %d)",
					pl.image, s.Name, tag, o.Channel, existing.Manifest.Artifacts.Images[pl.image], o.Revision+1)
			}
		}
	}
	// With registry.pull none, every install reads anonymously: a repository the release writes
	// that refuses it stops the run before anything is built.
	if p.anonymousReads() {
		private, err := p.privateRepos(ctx, p.shipRepos(plans, o.Channel))
		if err != nil {
			return nil, err
		}
		if len(private) > 0 {
			return nil, p.privateError(private, "then ship again")
		}
	}
	// The published release's installer is made again from its manifest, before anything is
	// written: the same release makes the same installer, and on an immutable channel one that
	// would differ is refused here.
	var keptInstaller v1.Image
	if existing != nil && existing.Manifest.Inputs == relNode.Inputs {
		img, err := p.installerImage(existing.Manifest)
		if err != nil {
			return nil, fmt.Errorf("installer: %w", err)
		}
		d, err := img.Digest()
		if err != nil {
			return nil, err
		}
		switch had := existing.Manifest.Artifacts.Installer; {
		case d.String() == had:
			keptInstaller = img
		case !ch.Mutable:
			if had == "" {
				had = "none"
			}
			return nil, fmt.Errorf("installer: %s %s is published on the immutable channel %s with the installer %s, and it would now be %s; publish a new revision (--revision %d)",
				s.Name, tag, o.Channel, had, d, o.Revision+1)
		}
	}
	// The chart likewise: made again from the published manifest, refused on an immutable channel
	// when it would differ.
	var keptChart *registry.Manifest
	if p.hasChart() && existing != nil && existing.Manifest.Inputs == relNode.Inputs {
		cm, err := p.chartManifest(existing.Manifest)
		if err != nil {
			return nil, fmt.Errorf("chart: %w", err)
		}
		_, h, err := cm.Body()
		if err != nil {
			return nil, err
		}
		had := "none"
		if c := existing.Manifest.Artifacts.Chart; c != nil {
			had = c.Digest
		}
		switch {
		case h.String() == had:
			keptChart = &cm
		case !ch.Mutable:
			return nil, fmt.Errorf("chart: %s %s is published on the immutable channel %s with the chart %s, and it would now be %s; publish a new revision (--revision %d)",
				s.Name, tag, o.Channel, had, h, o.Revision+1)
		}
	}
	skipped := skippedNodes(g)
	if len(skipped) > 0 {
		p.logf("not published by this stowage yet: %s", strings.Join(skipped, ", "))
	}

	// Build and mirror first, pushing by digest only: a failed build or an interrupt here leaves
	// every tag — and so the channel — as it was. Tags move once everything is in the registry.
	// Images are signed, and their linux/amd64 manifest and config read, before their tags move: a
	// tag never holds an unsigned image, and a failure here leaves every tag as it was.
	platform, err := v1.ParsePlatform(graph.Platform)
	if err != nil {
		return nil, err
	}
	configs, manifests := map[string]string{}, map[string]string{}
	for _, pl := range plans {
		if err := p.prepareImage(ctx, o.Version, pl); err != nil {
			return nil, err
		}
		d, err := p.Registry.Digest(p.imageRepo(pl.image), pl.digest)
		if err != nil {
			return nil, err
		}
		pm, pc, err := p.Registry.PlatformImage(ctx, d, *platform)
		if err != nil {
			return nil, fmt.Errorf("image %s: %w", pl.image, err)
		}
		manifests[pl.image], configs[pl.image] = pm.String(), pc.String()
		if err := p.ensureSigned(ctx, d); err != nil {
			return nil, fmt.Errorf("image %s: %w", pl.image, err)
		}
		if err := p.ensureSBOM(ctx, d); err != nil {
			return nil, fmt.Errorf("image %s: %w", pl.image, err)
		}
	}
	// A repository the first push created may be private: checked before the bundle and its disks.
	if p.anonymousReads() {
		refs, err := p.imageRefs(plans)
		if err != nil {
			return nil, err
		}
		if private := p.unreadable(ctx, refs); len(private) > 0 {
			return nil, p.privateError(private, "then ship again")
		}
	}
	images := map[string]string{}
	for _, pl := range plans {
		images[pl.image] = pl.digest
	}
	// The OS bundle preloads the images as they are about to be tagged; it and its disks are
	// built, pushed by digest and signed before any tag moves too.
	var osp *osPlan
	var recorded map[string]string // the disks a newer set replaced, which a ship again records
	if p.hasOS() {
		osNode, _ := g.Node(graph.KindOSBundle)
		var immutable *Ref
		if existing != nil && !ch.Mutable {
			immutable = existing
		}
		// A release published with disks keeps them when shipped again with the same inputs while
		// they are there; gone (a newer set replaced them), they are recorded as they were, or made
		// again with --disks.
		disks := o.Disks
		if existing != nil && existing.Manifest.Inputs == relNode.Inputs && len(existing.Manifest.Artifacts.Disks) > 0 {
			there, err := p.present(ctx, diskSet{disks: existing.Manifest.Artifacts.Disks})
			if err != nil {
				return nil, err
			}
			if there {
				disks = true
			} else if !o.Disks {
				recorded = existing.Manifest.Artifacts.Disks
			}
		}
		if osp, err = p.planOS(ctx, o, disks, tag, ch.Mutable || existing == nil, osNode.Inputs, images, configs, manifests, sources, immutable); err != nil {
			return nil, err
		}
	}
	for _, pl := range plans {
		if err := p.tagImage(ctx, pl); err != nil {
			return nil, err
		}
	}
	var osDigest string
	var disks map[string]string
	if osp != nil {
		if err := p.tagArtifact(ctx, osp.os); err != nil {
			return nil, err
		}
		osDigest, disks = osp.os.digest, map[string]string{}
		for _, a := range osp.disks {
			if err := p.tagArtifact(ctx, a); err != nil {
				return nil, err
			}
			disks[a.format] = a.digest
		}
		if len(disks) == 0 {
			disks = recorded
		}
	}
	// A release without disks of its own records the disk set its installs boot: its channel's,
	// or, shipped again with the same inputs, the one it recorded.
	var boot *release.BootDisks
	if osp != nil && disks == nil {
		if existing != nil && existing.Manifest.Inputs == relNode.Inputs && len(existing.Manifest.Artifacts.Disks) == 0 {
			boot = existing.Manifest.Artifacts.BootDisks
		} else {
			cur, err := p.currentDisks(ctx, o.Channel)
			if err != nil {
				return nil, err
			}
			if !cur.empty() && cur.release != tag {
				boot = &release.BootDisks{Release: cur.release, Stowage: cur.stowage, Disks: cur.disks}
			}
		}
		if boot == nil {
			p.logf("no disk published, and the channel %s has none: a VM of this release cannot be created until a ship publishes disks (--disks)", o.Channel)
		} else {
			p.logf("no disk published: a VM of this release boots %s's disks, then takes this release's OS bundle", boot.Release)
		}
	}

	text, _, err := p.Project.Changelog.Section(o.Version)
	if err != nil {
		return nil, fmt.Errorf("changelog: %w", err)
	}
	m := &release.Manifest{
		SchemaVersion:  release.SchemaVersion,
		Name:           s.Name,
		ShortName:      s.Short(),
		ProjectVersion: o.Version,
		Revision:       o.Revision,
		Channel:        o.Channel,
		Tag:            tag,
		Date:           p.Now().UTC(),
		Inputs:         relNode.Inputs,
		Scheme:         release.SchemeOf(s.Version.Scheme),
		StowageVersion: p.StowageVersion,
		Artifacts:      release.Artifacts{Images: images, Configs: configs, Manifests: manifests, OS: osDigest, Disks: disks, BootDisks: boot},
		Changelog:      release.Changelog{Project: text},
	}
	if src := o.Promoted; src != nil {
		m.Changelog = src.Manifest.Changelog
	}
	var rel *Ref
	if keptInstaller != nil && (!p.hasChart() || keptChart != nil) && sameRelease(existing.Manifest, m) {
		p.logf("release %s unchanged", tag)
		// The installer and the chart are put back if they went missing.
		if _, err := p.publishInstaller(ctx, keptInstaller, tag); err != nil {
			return nil, err
		}
		if keptChart != nil {
			if _, err := p.pushChart(ctx, *keptChart, existing.Manifest); err != nil {
				return nil, err
			}
			if err := p.tagChart(ctx, existing.Manifest); err != nil {
				return nil, err
			}
		}
		rel = existing
		d, err := p.Registry.Digest(p.releaseRepo(), existing.Digest.String())
		if err != nil {
			return nil, err
		}
		// A signature lost since (deleted from the registry) is made again.
		if err := p.ensureSigned(ctx, d); err != nil {
			return nil, err
		}
	}
	if rel == nil {
		if existing != nil && existing.Manifest.Inputs == m.Inputs {
			// Same release, other artifacts recorded (a mutable re-push): its number and yank stay.
			m.ReleaseSeq = existing.Manifest.ReleaseSeq
			m.Yanked = existing.Manifest.Yanked
		} else {
			// New content — a first publication, or a mutable tag rebuilt — is a new release with a
			// new number; a yank of the build it replaces does not apply to it.
			if existing != nil && existing.Manifest.Yanked != nil {
				p.logf("release %s replaces a yanked build", tag)
			}
			if m.ReleaseSeq, err = p.claimSeq(ctx, o.Channel); err != nil {
				return nil, err
			}
		}
		if p.hasChart() {
			cm, err := p.chartManifest(m)
			if err != nil {
				return nil, fmt.Errorf("chart: %w", err)
			}
			if m.Artifacts.Chart, err = p.pushChart(ctx, cm, m); err != nil {
				return nil, err
			}
		}
		// The installer carries the manifest as it is about to be published, less its own digest.
		img, err := p.installerImage(m)
		if err != nil {
			return nil, fmt.Errorf("installer: %w", err)
		}
		if m.Artifacts.Installer, err = p.publishInstaller(ctx, img, tag); err != nil {
			return nil, err
		}
		d, err := p.writeRelease(ctx, tag, m)
		if err != nil {
			return nil, err
		}
		if m.Artifacts.Chart != nil {
			if err := p.tagChart(ctx, m); err != nil {
				return nil, err
			}
		}
		rel = &Ref{m, d}
		p.logf("release %s published (seq %d)", tag, m.ReleaseSeq)
	}
	if len(rel.Manifest.Artifacts.Disks) > 0 && recorded == nil {
		p.checkDisksOf = tag
	}
	err = p.verifyRelease(ctx, rel, tag)
	p.checkDisksOf = ""
	if err != nil {
		return nil, fmt.Errorf("verify: %w", err)
	}
	// What the release wrote last (its chart, installer and manifest) is checked before a channel
	// points at it.
	if p.anonymousReads() {
		refs, err := p.releaseRefs(rel)
		if err != nil {
			return rel, err
		}
		if private := p.unreadable(ctx, refs); len(private) > 0 {
			return rel, p.privateError(private, fmt.Sprintf("then ship again: release %s is published, no channel points at it yet, and a run of the same inputs carries on", tag))
		}
	}
	if y := rel.Manifest.Yanked; y != nil {
		err := fmt.Errorf("release %s is yanked (%s): it is not offered again; publish a new revision", tag, y.Reason)
		// The channel never moves forward to it; a pointer still on it (a yank that stopped
		// early) moves back as the yank would have moved it.
		if serr := p.settleLatest(ctx, o.Channel, nil); serr != nil {
			err = errors.Join(err, serr)
		}
		return rel, err
	}
	if err := p.advancePointer(ctx, o.Channel, rel); err != nil {
		return rel, err
	}
	if err := p.settleLatest(ctx, o.Channel, rel); err != nil {
		return rel, err
	}
	p.sweepCharts(ctx, o.Channel)
	if len(rel.Manifest.Artifacts.Disks) > 0 && recorded == nil {
		// A rebuilt tag's earlier disks, which no manifest names any more.
		var replaced diskSet
		if existing != nil && !maps.Equal(existing.Manifest.Artifacts.Disks, rel.Manifest.Artifacts.Disks) {
			replaced = diskSet{release: tag, disks: existing.Manifest.Artifacts.Disks}
		}
		if err := p.cleanDisks(ctx, o.Channel, rel, replaced); err != nil {
			return rel, &CleanupError{Release: tag, Err: err}
		}
	}
	return rel, nil
}

// sameBoot reports whether two boot disk sets are the same.
func sameBoot(a, b *release.BootDisks) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Release == b.Release && a.Stowage == b.Stowage && maps.Equal(a.Disks, b.Disks)
}

// sameRelease reports whether a published manifest already describes the release m would: the
// same inputs, images, OS bundle and disks (the installer is compared by making it again). The publishing
// stowage's own version is not compared: what it ships to operators — the installer executable,
// the stowage.lock pins — is an input, so a newer publisher shipping the same never rewrites a
// published release.
func sameRelease(a, b *release.Manifest) bool {
	if a.Inputs != b.Inputs || len(a.Artifacts.Images) != len(b.Artifacts.Images) ||
		a.Artifacts.OS != b.Artifacts.OS || !maps.Equal(a.Artifacts.Disks, b.Artifacts.Disks) ||
		!sameBoot(a.Artifacts.BootDisks, b.Artifacts.BootDisks) {
		return false
	}
	for k, v := range b.Artifacts.Images {
		if a.Artifacts.Images[k] != v {
			return false
		}
	}
	return true
}

func skippedNodes(g *graph.Graph) []string {
	var out []string
	for _, n := range g.Nodes {
		switch n.Kind {
		case graph.KindImage, graph.KindChart, graph.KindOSBundle, graph.KindDisks, graph.KindInstaller, graph.KindRelease:
		default:
			out = append(out, n.ID)
		}
	}
	return out
}

func (p *Publisher) planImage(ctx context.Context, n graph.Node, tag string, mutable, noBuild bool, sources []*Ref) (*imagePlan, error) {
	img := strings.TrimPrefix(n.ID, graph.KindImage+"/")
	repo := p.imageRepo(img)
	t, err := p.Registry.Tag(repo, tag)
	if err != nil {
		return nil, err
	}
	pl := &imagePlan{node: n, image: img, tag: t}
	if n.Pinned != "" {
		return p.planPinned(ctx, pl, repo, mutable, sources)
	}
	ann, d, exists, err := p.Registry.Annotations(ctx, t)
	if err != nil {
		return nil, err
	}
	if exists {
		if ann[registry.InputsAnnotation] == n.Inputs {
			pl.action, pl.digest = actKeep, d.String()
			return pl, nil
		} else if !mutable {
			return nil, fmt.Errorf("image %s: %s exists with other inputs on an immutable channel", img, t)
		}
	}
	seen := map[string]bool{}
	for _, src := range sources {
		d := src.Manifest.Artifacts.Images[img]
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		ref, err := p.Registry.Digest(repo, d)
		if err != nil {
			return nil, err
		}
		ann, _, ok, err := p.Registry.Annotations(ctx, ref)
		if err != nil {
			return nil, err
		}
		if ok && ann[registry.InputsAnnotation] == n.Inputs {
			pl.action, pl.from, pl.digest = actRetag, ref, d
			return pl, nil
		}
	}
	if noBuild {
		return nil, fmt.Errorf("image %s: no published image has these inputs (%s); the tree differs from the release being promoted", img, n.Inputs)
	}
	pl.action = actBuild
	return pl, nil
}

func (p *Publisher) planPinned(ctx context.Context, pl *imagePlan, repo string, mutable bool, sources []*Ref) (*imagePlan, error) {
	ud, err := pinnedDigest(pl.node.Pinned)
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", pl.image, err)
	}
	want := ud.DigestStr()
	pl.digest = want
	d, err := p.Registry.Head(ctx, pl.tag)
	if err != nil {
		return nil, err
	}
	if d != nil && d.Digest.String() == want {
		pl.action = actKeep
		return pl, nil
	}
	if d != nil && !mutable {
		return nil, fmt.Errorf("image %s: %s exists with another digest on an immutable channel", pl.image, pl.tag)
	}
	for _, src := range sources {
		if src.Manifest.Artifacts.Images[pl.image] == want {
			ref, err := p.Registry.Digest(repo, want)
			if err != nil {
				return nil, err
			}
			if h, err := p.Registry.Head(ctx, ref); err != nil {
				return nil, err
			} else if h != nil {
				pl.action, pl.from = actRetag, ref
				return pl, nil
			}
		}
	}
	pl.action, pl.from = actCopy, ud
	return pl, nil
}

// pinnedDigest parses an upstream reference pinned by digest, in Docker's usual forms
// (`postgres:17-alpine@sha256:…`, a registry-less name meaning Docker Hub). The tag is ignored:
// the digest is what is mirrored.
func pinnedDigest(ref string) (name.Digest, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return name.Digest{}, err
	}
	d, ok := r.(name.Digest)
	if !ok {
		return name.Digest{}, fmt.Errorf("%s is not pinned by digest", ref)
	}
	return d, nil
}

// prepareImage puts a planned image in the registry under its digest, untagged: a build is pushed,
// a pinned upstream image mirrored. Afterwards the plan is a keep or a re-tag.
func (p *Publisher) prepareImage(ctx context.Context, v string, pl *imagePlan) error {
	repo := pl.tag.Context().Name()
	switch pl.action {
	case actCopy:
		p.logf("image %s: mirror %s", pl.image, pl.node.Pinned)
		dst, err := p.Registry.Digest(repo, pl.digest)
		if err != nil {
			return err
		}
		if err := p.Registry.Copy(ctx, pl.from, dst, p.Upstream); err != nil {
			return err
		}
		d, err := p.Registry.Head(ctx, dst)
		if err != nil {
			return err
		}
		if d == nil || d.Digest.String() != pl.digest {
			return fmt.Errorf("image %s: mirrored digest differs from the pinned %s", pl.image, pl.digest)
		}
		pl.action, pl.from = actRetag, dst
		return nil
	case actBuild:
	default:
		return nil
	}
	p.logf("image %s: build", pl.image)
	req := BuildRequest{Image: pl.image, Inputs: pl.node.Inputs, Recipe: pl.node.Recipe}
	if pl.node.ReadsVersion {
		req.Version = v
	}
	img, cleanup, err := p.Builder.Build(ctx, p.Project, req)
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()
	if err != nil {
		return fmt.Errorf("image %s: build: %w", pl.image, err)
	}
	annotated, ok := mutate.Annotations(img, map[string]string{registry.InputsAnnotation: pl.node.Inputs}).(v1.Image)
	if !ok {
		return fmt.Errorf("image %s: cannot annotate", pl.image)
	}
	d, err := annotated.Digest()
	if err != nil {
		return err
	}
	dst, err := p.Registry.Digest(repo, d.String())
	if err != nil {
		return err
	}
	if err := p.Registry.WriteImage(ctx, dst, annotated); err != nil {
		return err
	}
	pl.action, pl.from, pl.digest = actRetag, dst, d.String()
	return nil
}

// tagImage points the image's release tag at its digest.
func (p *Publisher) tagImage(ctx context.Context, pl *imagePlan) error {
	if pl.action == actKeep {
		p.logf("image %s: %s unchanged", pl.image, pl.tag.TagStr())
		return nil
	}
	p.logf("image %s: tag %s → %s", pl.image, registry.Short(pl.digest), pl.tag.TagStr())
	return p.Registry.Retag(ctx, pl.from, pl.tag)
}

// SeqMediaType is the artifact recording a channel's highest release_seq ever claimed.
const SeqMediaType = "application/vnd.stowage.release-seq.v1+json"

type seqRecord struct {
	Channel string `json:"channel"`
	Seq     int    `json:"seq"`
}

func (p *Publisher) seqTag(channel string) (name.Tag, error) {
	return p.Registry.Tag(p.releaseRepo()+"-seq", channel)
}

// claimSeq returns the channel's next release_seq and records it before the release is written,
// so a number is never handed out twice — not after a prune, not after a failed publication (a
// failure leaves a gap). The record lives in <name>-release-seq:<channel>; without one, the
// highest number among the channel's releases on the registry is the start.
func (p *Publisher) claimSeq(ctx context.Context, channel string) (int, error) {
	t, err := p.seqTag(channel)
	if err != nil {
		return 0, err
	}
	high := 0
	a, _, err := p.Registry.GetArtifact(ctx, t, SeqMediaType)
	if err != nil {
		return 0, err
	}
	if a != nil {
		var r seqRecord
		if err := json.Unmarshal(a.Data, &r); err != nil || r.Channel != channel || r.Seq < 1 {
			return 0, fmt.Errorf("%s: unreadable sequence record", t)
		}
		high = r.Seq
	} else {
		// Every release manifest still readable counts, ordered or not by the current scheme.
		rs, err := p.scan(ctx, channel)
		if err != nil {
			return 0, err
		}
		for _, r := range rs {
			high = max(high, r.Manifest.ReleaseSeq)
		}
	}
	next := high + 1
	data, err := json.Marshal(seqRecord{channel, next})
	if err != nil {
		return 0, err
	}
	if _, err := p.Registry.PutArtifact(ctx, t, registry.Artifact{Type: SeqMediaType, Data: data}); err != nil {
		return 0, err
	}
	return next, nil
}

// advancePointer moves the channel pointer to rel, a release that is not yanked, unless it
// already points at a later release.
func (p *Publisher) advancePointer(ctx context.Context, channel string, rel *Ref) error {
	cur, err := p.livePointer(ctx, channel)
	if err != nil {
		return err
	}
	if cur != nil {
		if cur.Digest == rel.Digest {
			return nil
		}
		later := false
		if err := p.Project.Scheme.Validate(cur.Manifest.ProjectVersion); err == nil {
			if later, err = p.later(cur.Manifest, rel.Manifest); err != nil {
				return err
			}
		} else if !version.IsInvalid(err) {
			return err
		}
		// A pointer whose version the current scheme no longer reads cannot be ordered: it moves.
		if later {
			p.logf("channel %s stays on %s, later than %s", channel, cur.Manifest.Tag, rel.Manifest.Tag)
			return nil
		}
	}
	if err := p.movePointer(ctx, channel, rel); err != nil {
		return err
	}
	p.logf("channel %s → %s", channel, rel.Manifest.Tag)
	return nil
}

// livePointer returns the channel pointer's release when it is still offered: its own tag still
// holds the pointer's digest and it is not yanked. A pointer left on a release that was yanked or
// rewritten since (an interrupted yank) counts as no pointer, so the next ship replaces it.
func (p *Publisher) livePointer(ctx context.Context, channel string) (*Ref, error) {
	ptr, err := p.pointer(ctx, channel)
	if err != nil || ptr == nil {
		return nil, err
	}
	cur, err := p.readRelease(ctx, ptr.Manifest.Tag)
	if err != nil {
		return nil, err
	}
	if cur == nil || cur.Digest != ptr.Digest || cur.Manifest.Yanked != nil {
		p.logf("channel %s: pointer is on %s, which is no longer offered", channel, ptr.Manifest.Tag)
		return nil, nil
	}
	return cur, nil
}

// later reports whether a is a later release than b: higher version, or the same version at a
// higher revision.
func (p *Publisher) later(a, b *release.Manifest) (bool, error) {
	c, err := p.Project.Scheme.Compare(a.ProjectVersion, b.ProjectVersion)
	if err != nil {
		return false, err
	}
	return c > 0 || (c == 0 && a.Revision > b.Revision), nil
}

// sameVersion reports whether two valid versions are the same version for the project's scheme.
func (p *Publisher) sameVersion(a, b string) (bool, error) {
	c, err := p.Project.Scheme.Compare(a, b)
	return c == 0, err
}

// incompleteError is verifyRelease's answer "the release is not complete": a tag missing or moved,
// or a signature missing. Any other error from it is a failure to check.
type incompleteError struct{ problems []string }

func (e *incompleteError) Error() string { return strings.Join(e.problems, "; ") }

func incomplete(err error) bool {
	var ie *incompleteError
	return errors.As(err, &ie)
}

// verifyRelease checks the release tag holds rel and every image tag holds its recorded digest, and,
// for a signed project, that the manifest and every image carry a signature by a trusted key.
func (p *Publisher) verifyRelease(ctx context.Context, rel *Ref, tag string) error {
	var problems []string
	// A missing signature is a problem of the release; a failure to check aborts, so a registry or
	// key store outage never reads as an incomplete release (and never moves a pointer back).
	checkSig := func(repo, digest string) error {
		if !p.signed() {
			return nil
		}
		if p.Signer == nil {
			return errors.New("signing is configured and no signer is set to check signatures")
		}
		d, err := p.Registry.Digest(repo, digest)
		if err != nil {
			return err
		}
		if err := p.Signer.Verify(ctx, d); errors.Is(err, ErrUnsigned) {
			problems = append(problems, err.Error())
		} else if err != nil {
			return err
		}
		return nil
	}
	if err := checkSig(p.releaseRepo(), rel.Digest.String()); err != nil {
		return err
	}
	t, err := p.Registry.Tag(p.releaseRepo(), tag)
	if err != nil {
		return err
	}
	d, err := p.Registry.Head(ctx, t)
	if err != nil {
		return err
	}
	if d == nil || d.Digest != rel.Digest {
		problems = append(problems, fmt.Sprintf("%s does not hold %s", t, rel.Digest))
	}
	// checkTag: tag in repo holds want, signed. Only an artifact in place is checked for a
	// signature: one already missing is a problem, and its digest may be gone too.
	checkTag := func(repo, tag, want string) error {
		it, err := p.Registry.Tag(repo, tag)
		if err != nil {
			problems = append(problems, err.Error())
			return nil
		}
		d, err := p.Registry.Head(ctx, it)
		if err != nil {
			return err
		}
		switch {
		case d == nil:
			problems = append(problems, fmt.Sprintf("%s is missing", it))
		case d.Digest.String() != want:
			problems = append(problems, fmt.Sprintf("%s is %s, the release says %s", it, d.Digest, want))
		default:
			return checkSig(repo, want)
		}
		return nil
	}
	for _, img := range sortedKeys(rel.Manifest.Artifacts.Images) {
		if err := checkTag(p.imageRepo(img), rel.Manifest.Tag, rel.Manifest.Artifacts.Images[img]); err != nil {
			return err
		}
	}
	if inst := rel.Manifest.Artifacts.Installer; inst != "" {
		if err := checkTag(p.installerRepo(), rel.Manifest.Tag, inst); err != nil {
			return err
		}
	}
	if c := rel.Manifest.Artifacts.Chart; c != nil {
		if err := checkTag(p.chartRepo(rel.Manifest.Channel), c.Version, c.Digest); err != nil {
			return err
		}
	}
	if o := rel.Manifest.Artifacts.OS; o != "" {
		if err := checkTag(p.osRepo(), rel.Manifest.Tag, o); err != nil {
			return err
		}
	}
	// A release's own disks are checked while they are its channel's set, and by the ship that
	// publishes them; others' a newer set may have replaced: its installs boot the channel's set.
	if own := (diskSet{release: rel.Manifest.Tag, disks: rel.Manifest.Artifacts.Disks}); !own.empty() {
		check := p.checkDisksOf == rel.Manifest.Tag
		if !check {
			cur, err := p.currentDisks(ctx, rel.Manifest.Channel)
			if err != nil {
				return err
			}
			check = cur.release == rel.Manifest.Tag
		}
		for _, f := range sortedKeys(own.disks) {
			if !check {
				break
			}
			if err := checkTag(p.diskRepo(), release.DiskTag(rel.Manifest.Tag, f), own.disks[f]); err != nil {
				return err
			}
		}
	}
	// The set a release without disks boots, while it is the channel's: there, or reported.
	if b := rel.Manifest.Artifacts.BootDisks; len(rel.Manifest.Artifacts.Disks) == 0 && b != nil {
		cur, err := p.currentDisks(ctx, rel.Manifest.Channel)
		if err != nil {
			return err
		}
		if cur.release == b.Release {
			for _, f := range sortedKeys(b.Disks) {
				ref, err := p.Registry.Digest(p.diskRepo(), b.Disks[f])
				if err != nil {
					return err
				}
				if d, err := p.Registry.Head(ctx, ref); err != nil {
					return err
				} else if d == nil {
					problems = append(problems, fmt.Sprintf("%s, the %s disk it boots (%s's), is missing", ref, f, b.Release))
				}
			}
		}
	}
	if len(problems) > 0 {
		return &incompleteError{problems}
	}
	return nil
}

// Verify checks a channel: its pointer (when version is empty) or one of its releases.
func (p *Publisher) Verify(ctx context.Context, channel, v string, revision int) (*Ref, error) {
	if _, ok := p.Project.Spec.Channels[channel]; !ok {
		return nil, fmt.Errorf("channel %q is not declared", channel)
	}
	if v == "" {
		ptr, err := p.pointer(ctx, channel)
		if err != nil {
			return nil, err
		}
		if ptr == nil {
			err := fmt.Errorf("channel %s has no release", channel)
			if lerr := p.verifyLatest(ctx, channel, nil); lerr != nil {
				err = fmt.Errorf("%w; %w", err, lerr)
			}
			return nil, err
		}
		if ptr.Manifest.Channel != channel {
			return nil, fmt.Errorf("channel pointer %s holds a release of channel %s", channel, ptr.Manifest.Channel)
		}
		if err := p.verifyRelease(ctx, ptr, ptr.Manifest.Tag); err != nil {
			return ptr, err
		}
		return ptr, p.verifyLatest(ctx, channel, ptr)
	}
	tag, err := p.releaseTag(v, channel, revision)
	if err != nil {
		return nil, err
	}
	r, err := p.readRelease(ctx, tag)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("%s %s is not published on %s", p.Project.Spec.Name, tag, channel)
	}
	return r, p.verifyRelease(ctx, r, tag)
}

// Promote publishes, on channel to, the release the tree's version has on channel from, without
// building: every image must already be published with the same inputs.
func (p *Publisher) Promote(ctx context.Context, v, from, to string, revision int) (*Ref, error) {
	if from == to {
		return nil, errors.New("promote: source and target channel are the same")
	}
	// What this publisher can publish is known locally: checked before the registry is read. The
	// tree is the release's, so its pin names the stowage that published it.
	if l := p.Project.Lock; l != nil {
		if order, err := l.Compare(p.StowageVersion); err == nil && order != 0 {
			return nil, fmt.Errorf("promote: %s pins stowage %s, this is %s: promote with stowage %s, from the release's commit", lock.FileName, l.Stowage, p.StowageVersion, l.Stowage)
		}
	}
	if err := p.readyToPublish(); err != nil {
		return nil, err
	}
	src, err := p.Verify(ctx, from, v, revision)
	if err != nil {
		return nil, fmt.Errorf("promote: source: %w", err)
	}
	if src.Manifest.Yanked != nil {
		return nil, fmt.Errorf("promote: %s is yanked (%s)", src.Manifest.Tag, src.Manifest.Yanked.Reason)
	}
	// The tree must be exactly what was published: its release inputs for the source channel —
	// images (the front's recipe included), compose, spec, changelog, stowage.lock, and the
	// installer's executable and pull account — equal the source release's. Checked before anything is written.
	g, err := p.Graph(v, revision, from)
	if err != nil {
		return nil, err
	}
	if rn, _ := g.Node(graph.KindRelease); rn.Inputs != src.Manifest.Inputs {
		hint := "check out its commit; the installer's executable and the front's recipe (both this stowage's) and the pull account's password are inputs too"
		if src.Manifest.StowageVersion != p.StowageVersion {
			hint = fmt.Sprintf("it was published by stowage %s, this is %s; promote with that stowage, from its commit", src.Manifest.StowageVersion, p.StowageVersion)
		}
		return nil, fmt.Errorf("promote: the release inputs differ from %s as published (inputs %s, the release has %s): %s",
			src.Manifest.Tag, registry.Short(rn.Inputs), registry.Short(src.Manifest.Inputs), hint)
	}
	// The disks it published, while they are there: a newer set replaced them otherwise, and the
	// promotion boots the target channel's.
	disks := len(src.Manifest.Artifacts.Disks) > 0
	if disks {
		there, err := p.present(ctx, diskSet{disks: src.Manifest.Artifacts.Disks})
		if err != nil {
			return nil, err
		}
		disks = there
	}
	return p.Ship(ctx, ShipOptions{Version: v, Channel: to, Revision: revision, NoBuild: true, Sources: []*Ref{src}, Promoted: src, Disks: disks})
}

// scan reads every release manifest of the repository (of one channel when channel is set),
// skipping tags that hold something else.
func (p *Publisher) scan(ctx context.Context, channel string) ([]*Ref, error) {
	return p.scanReleases(ctx, channel, false)
}

// scanReleases is scan; strict fails on a tag it does not read instead of skipping it, for a
// caller that deletes what no release names.
func (p *Publisher) scanReleases(ctx context.Context, channel string, strict bool) ([]*Ref, error) {
	repo, err := p.Registry.Repo(p.releaseRepo())
	if err != nil {
		return nil, err
	}
	tags, err := p.Registry.Tags(ctx, repo)
	if err != nil {
		return nil, err
	}
	pointers := map[string]bool{}
	for _, c := range p.Project.Spec.ChannelNames() {
		pointers[c] = true
	}
	for _, r := range version.ReservedTags {
		pointers[r] = true
	}
	var out []*Ref
	for _, t := range tags {
		if pointers[t] {
			continue
		}
		r, err := p.readRelease(ctx, t)
		if err != nil {
			// A tag holding something this stowage does not read (another artifact, a newer
			// manifest format) is skipped, so one stray tag cannot block every ship, yank and
			// prune; any failure to read fails the listing, so a release is never missed.
			if strict || !errors.Is(err, registry.ErrNotArtifact) {
				return nil, err
			}
			p.logf("warning: skipping %s: %v", t, err)
			continue
		}
		if r == nil || (channel != "" && r.Manifest.Channel != channel) || r.Manifest.Tag != t {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// Releases lists the published releases, of one channel when channel is set, oldest first. A
// release whose version the project's current scheme does not read cannot be ordered and is left
// out with a warning.
func (p *Publisher) Releases(ctx context.Context, channel string) ([]*Ref, error) {
	all, err := p.scan(ctx, channel)
	if err != nil {
		return nil, err
	}
	var out []*Ref
	for _, r := range all {
		if err := p.Project.Scheme.Validate(r.Manifest.ProjectVersion); err != nil {
			if !version.IsInvalid(err) {
				return nil, err
			}
			p.logf("warning: skipping %s: version %s is not one of the project's current scheme", r.Manifest.Tag, r.Manifest.ProjectVersion)
			continue
		}
		out = append(out, r)
	}
	var sortErr error
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Manifest, out[j].Manifest
		if a.Channel != b.Channel {
			return a.Channel < b.Channel
		}
		l, err := p.later(b, a)
		if err != nil && sortErr == nil {
			sortErr = err
		}
		return l
	})
	return out, sortErr
}

// Yank marks a release so consumers refuse to upgrade to it. When the channel pointer is on it,
// the pointer moves back to the latest release that is not yanked, or is removed when none is
// left. Nothing is deleted.
func (p *Publisher) Yank(ctx context.Context, channel, v string, revision int, reason string) error {
	// A release is yanked whatever state its tags are in: a broken one is what needs it most.
	tag, err := p.releaseTag(v, channel, revision)
	if err != nil {
		return err
	}
	r, err := p.readRelease(ctx, tag)
	if err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("%s %s is not published on %s", p.Project.Spec.Name, tag, channel)
	}
	if r.Manifest.Yanked == nil {
		m := *r.Manifest
		m.Yanked = &release.Yank{Reason: reason, Date: p.Now().UTC()}
		d, err := p.writeRelease(ctx, m.Tag, &m)
		if err != nil {
			return err
		}
		p.logf("release %s yanked (now %s)", m.Tag, registry.Short(d.String()))
		return p.retreatPointer(ctx, channel)
	}
	p.logf("release %s was already yanked", r.Manifest.Tag)
	return p.retreatPointer(ctx, channel)
}

// retreatPointer moves the channel pointer back when the release it is on is no longer offered
// (yanked, deleted, or rewritten since the pointer moved) or no longer complete.
func (p *Publisher) retreatPointer(ctx context.Context, channel string) error {
	ptr, err := p.pointer(ctx, channel)
	if err != nil {
		return err
	}
	if ptr == nil {
		// A run stopped after removing the pointer removes the installer's latest tag now.
		return p.dropLatest(ctx, channel)
	}
	live, err := p.livePointer(ctx, channel)
	if err != nil {
		return err
	}
	if live != nil {
		verr := p.verifyRelease(ctx, live, live.Manifest.Tag)
		if verr == nil {
			return p.syncLatest(ctx, channel, live)
		}
		if !incomplete(verr) {
			return verr
		}
		p.logf("channel %s: %s is incomplete: %v", channel, live.Manifest.Tag, verr)
	}
	rs, err := p.Releases(ctx, channel)
	if err != nil {
		return err
	}
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].Manifest.Yanked == nil {
			if err := p.verifyRelease(ctx, rs[i], rs[i].Manifest.Tag); incomplete(err) {
				p.logf("channel %s: not falling back to %s: %v", channel, rs[i].Manifest.Tag, err)
				continue
			} else if err != nil {
				return err
			}
			if err := p.movePointer(ctx, channel, rs[i]); err != nil {
				return err
			}
			p.logf("channel %s → %s", channel, rs[i].Manifest.Tag)
			return p.syncLatest(ctx, channel, rs[i])
		}
	}
	t, err := p.Registry.Tag(p.releaseRepo(), channel)
	if err != nil {
		return err
	}
	p.logf("channel %s has no complete release left: pointer removed", channel)
	if _, err := p.untag(ctx, t); err != nil {
		return err
	}
	return p.dropLatest(ctx, channel)
}

// Prune deletes the tags of a mutable channel's releases: of version v (every revision), or of
// every release but the one its pointer is on. Image, installer, chart, OS bundle and disk tags go first, the release tag last, so an
// interrupted prune leaves a release that verify reports rather than orphan images.
func (p *Publisher) Prune(ctx context.Context, channel, v string) (pruned []string, err error) {
	ch, ok := p.Project.Spec.Channels[channel]
	if !ok {
		return nil, fmt.Errorf("channel %q is not declared", channel)
	}
	if !ch.Mutable {
		return nil, fmt.Errorf("channel %s is immutable: its releases are never deleted (yank one instead)", channel)
	}
	rs, err := p.Releases(ctx, channel)
	if err != nil {
		return nil, err
	}
	ptr, err := p.pointer(ctx, channel)
	if err != nil {
		return nil, err
	}
	if v != "" {
		if err := p.Project.Scheme.Validate(v); err != nil {
			return nil, fmt.Errorf("%s: %w", v, err)
		}
	}
	pointerGone := false
	// Which releases go, decided first: a pruned release's disk is kept whole while a release the
	// prune keeps names it (its own, or the set it boots).
	prunes := map[string]bool{}
	for _, r := range rs {
		onPointer := ptr != nil && ptr.Manifest.Tag == r.Manifest.Tag
		if v == "" {
			prunes[r.Manifest.Tag] = !onPointer
			continue
		}
		same, err := p.sameVersion(r.Manifest.ProjectVersion, v)
		if err != nil {
			return nil, err
		}
		prunes[r.Manifest.Tag] = same
	}
	// Every channel's releases count, and one the current scheme does not order: a promotion
	// shares a disk across channels.
	everyRelease, err := p.scan(ctx, "")
	if err != nil {
		return nil, err
	}
	named := map[string]bool{}
	for _, r := range everyRelease {
		if r.Manifest.Channel != channel || !prunes[r.Manifest.Tag] {
			for _, d := range bootSet(r.Manifest).digests() {
				named[d] = true
			}
		}
	}
	var kept map[string]bool // the other channels' disk sets, read once a pruned release has disks
	var held []name.Digest   // what untag left to other tags: the pointer, latest, another release
	// Whatever happens below, a pointer whose release lost tags is moved off it.
	defer func() {
		// Content whose tag was retired while another tag held it (the pointer, latest, another
		// release) goes once no tag does, whatever happened: what a tag still holds stays.
		defer func() {
			if serr := p.sweepHeld(ctx, held); serr != nil {
				err = errors.Join(err, serr)
			}
		}()
		if pointerGone {
			if rerr := p.retreatPointer(ctx, channel); rerr != nil {
				err = errors.Join(err, rerr)
				return
			}
		}
		// Also what a prune stopped earlier left: a pointer on a deleted release, a latest tag.
		if serr := p.settleLatest(ctx, channel, nil); serr != nil {
			err = errors.Join(err, serr)
		}
		p.sweepCharts(ctx, channel)
	}()
	for _, r := range rs {
		if !prunes[r.Manifest.Tag] {
			continue
		}
		if ptr != nil && ptr.Manifest.Tag == r.Manifest.Tag {
			pointerGone = true
		}
		type repoTag struct{ repo, tag string }
		var tags []repoTag
		for _, img := range sortedKeys(r.Manifest.Artifacts.Images) {
			tags = append(tags, repoTag{p.imageRepo(img), r.Manifest.Tag})
		}
		if r.Manifest.Artifacts.Installer != "" {
			tags = append(tags, repoTag{p.installerRepo(), r.Manifest.Tag})
		}
		if c := r.Manifest.Artifacts.Chart; c != nil {
			tags = append(tags, repoTag{p.chartRepo(r.Manifest.Channel), c.Version})
		}
		if r.Manifest.Artifacts.OS != "" {
			tags = append(tags, repoTag{p.osRepo(), r.Manifest.Tag})
		}
		for _, rt := range tags {
			t, err := p.Registry.Tag(rt.repo, rt.tag)
			if err != nil {
				return pruned, err
			}
			d, err := p.untag(ctx, t)
			if err != nil {
				return pruned, err
			}
			if d != nil {
				held = append(held, *d)
			}
		}
		// The disks the release boots (its own, or another's set) go, by digest, unless a release
		// left boots them, or they are another channel's set.
		if rd := bootSet(r.Manifest); !rd.empty() {
			if kept == nil {
				if kept, err = p.keptDisks(ctx); err != nil {
					return pruned, err
				}
			}
			for _, f := range sortedKeys(rd.disks) {
				d := rd.disks[f]
				switch {
				case named[d]:
					// A release left boots it: kept whole, tag included.
					p.logf("disk %s of %s kept: a release left boots it", registry.Short(d), r.Manifest.Tag)
					continue
				case !kept[d]:
					if err := p.remove(ctx, p.diskRepo(), d); err != nil {
						return pruned, err
					}
					continue
				}
				// Another channel's set (a promotion re-tags the same disk): only this tag goes,
				// where the registry deletes a tag alone.
				t, err := p.Registry.Tag(p.diskRepo(), release.DiskTag(rd.release, f))
				if err != nil {
					return pruned, err
				}
				if err := p.Registry.DeleteTag(ctx, t); errors.Is(err, registry.ErrDeleteRefused) {
					p.logf("disk %s of %s kept: it is a channel's disk set, and the registry deletes no tag alone", registry.Short(d), r.Manifest.Tag)
				} else if err != nil {
					return pruned, err
				}
			}
		}
		t, err := p.Registry.Tag(p.releaseRepo(), r.Manifest.Tag)
		if err != nil {
			return pruned, err
		}
		d, err := p.untag(ctx, t)
		if err != nil {
			return pruned, err
		}
		if d != nil {
			held = append(held, *d)
		}
		p.logf("pruned %s", r.Manifest.Tag)
		pruned = append(pruned, r.Manifest.Tag)
	}
	if v != "" && len(pruned) == 0 {
		return nil, fmt.Errorf("channel %s has no release of %s", channel, v)
	}
	return pruned, nil
}

func sortedKeys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }
