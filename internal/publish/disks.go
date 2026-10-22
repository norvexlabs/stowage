package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
)

// A channel's disk set is a record in the registry (release.DiskSet, under DiskSetTag of the
// channel, signed): the disks the channel's VMs boot. Only a ship that publishes disks writes it,
// once the channel points at its release; nothing else moves it — no yank, prune or ship without
// disks. A release without disks of its own records the channel's set as the one its installs
// boot (release.BootDisks); the ship that writes a new set then removes the disks the channel's
// releases name that no channel's set holds, so the registry holds one disk set per channel.

// Deleter deletes a manifest by digest, with every tag on it; nothing when it is absent.
// registry.Client is the distribution API's (the oci provider); registry.GitHubPackages is
// GitHub's packages API (the github provider).
type Deleter interface {
	DeleteManifest(ctx context.Context, d name.Digest) error
}

func (p *Publisher) deleter() Deleter {
	if p.Deleter != nil {
		return p.Deleter
	}
	return p.Registry
}

// CleanupError is a release published, and its channel pointing at it, whose cleanup failed
// after: the release is out, so it is not shipped again; the cleanup is done by the next ship
// that publishes disks.
type CleanupError struct {
	Release string
	Err     error
}

func (e *CleanupError) Error() string {
	return fmt.Sprintf("%s is published; the cleanup of the older disk sets failed: %v", e.Release, e.Err)
}

func (e *CleanupError) Unwrap() error { return e.Err }

// diskSet is a set of disks: the release tag they are under, the stowage that published them, and
// each disk's digest by format.
type diskSet struct {
	release string
	disks   map[string]string
	stowage string
}

func (d diskSet) empty() bool { return len(d.disks) == 0 }

// digests are the set's digests, sorted.
func (d diskSet) digests() []string {
	return slices.Sorted(maps.Values(d.disks))
}

// bootSet is the disk set an install of m boots: its own disks, or the set it records.
func bootSet(m *release.Manifest) diskSet {
	tag, disks := m.Artifacts.Boot(m.Tag)
	st := m.StowageVersion
	if b := m.Artifacts.BootDisks; len(m.Artifacts.Disks) == 0 && b != nil {
		st = b.Stowage
	}
	return diskSet{release: tag, disks: disks, stowage: st}
}

// present reports whether every disk of the set is in the registry.
func (p *Publisher) present(ctx context.Context, d diskSet) (bool, error) {
	for _, dg := range d.digests() {
		ref, err := p.Registry.Digest(p.diskRepo(), dg)
		if err != nil {
			return false, err
		}
		have, err := p.Registry.Head(ctx, ref)
		if err != nil {
			return false, err
		}
		if have == nil {
			return false, nil
		}
	}
	return true, nil
}

func (p *Publisher) diskSetTag(channel string) (name.Tag, error) {
	return p.Registry.Tag(p.diskRepo(), release.DiskSetTag(channel))
}

// currentDisks is the channel's disk set, as its record holds it; empty when it has none (no ship
// published disks on it yet).
func (p *Publisher) currentDisks(ctx context.Context, channel string) (diskSet, error) {
	if !p.hasOS() {
		return diskSet{}, nil
	}
	t, err := p.diskSetTag(channel)
	if err != nil {
		return diskSet{}, err
	}
	a, _, err := p.Registry.GetArtifact(ctx, t, release.DiskSetMediaType)
	if err != nil || a == nil {
		return diskSet{}, err
	}
	var r release.DiskSet
	if err := json.Unmarshal(a.Data, &r); err != nil {
		return diskSet{}, fmt.Errorf("%s: %w", t, err)
	}
	if err := r.Check(channel); err != nil {
		return diskSet{}, fmt.Errorf("%s: %w", t, err)
	}
	return diskSet{release: r.Release, disks: r.Disks, stowage: r.Stowage}, nil
}

// writeDisks records set as the channel's disk set: pushed by digest, signed, then tagged.
func (p *Publisher) writeDisks(ctx context.Context, channel string, set diskSet) error {
	data, err := json.Marshal(release.DiskSet{Channel: channel, Release: set.release, Stowage: set.stowage, Disks: set.disks})
	if err != nil {
		return err
	}
	repo, err := p.Registry.Repo(p.diskRepo())
	if err != nil {
		return err
	}
	d, err := p.Registry.PushArtifact(ctx, repo, registry.Artifact{Type: release.DiskSetMediaType, Data: data})
	if err != nil {
		return err
	}
	if err := p.ensureSigned(ctx, d); err != nil {
		return fmt.Errorf("disk set: %w", err)
	}
	t, err := p.diskSetTag(channel)
	if err != nil {
		return err
	}
	if err := p.Registry.Retag(ctx, d, t); err != nil {
		return err
	}
	p.logf("channel %s: its VMs boot %s's disks", channel, set.release)
	return nil
}

// keptDisks are the digests of every channel's disk set, which no cleanup or prune removes.
func (p *Publisher) keptDisks(ctx context.Context) (map[string]bool, error) {
	kept := map[string]bool{}
	for _, c := range p.Project.Spec.ChannelNames() {
		set, err := p.currentDisks(ctx, c)
		if err != nil {
			return nil, err
		}
		for _, d := range set.digests() {
			kept[d] = true
		}
	}
	return kept, nil
}

// pruneDiskSets removes, with its signatures, every disk a release of the channel names — the
// disks it published, or the set it boots — that no channel's set holds: the channel's older disk
// sets — and the disks of the build a rebuilt tag replaced (replaced). Only disks a release
// manifest of the channel names are candidates, and none of a release later than keep's, so a
// disk being pushed by a ship, its manifest not written yet or its pointer not moved, is never
// touched; a disk no release names (a failed ship's) is left to the registry.
func (p *Publisher) pruneDiskSets(ctx context.Context, channel string, keep *Ref, replaced diskSet) error {
	kept, err := p.keptDisks(ctx)
	if err != nil {
		return err
	}
	rs, err := p.scan(ctx, channel)
	if err != nil {
		return err
	}
	byDigest := map[string][]string{}
	for f, d := range replaced.disks {
		if !kept[d] {
			byDigest[d] = append(byDigest[d], release.DiskTag(replaced.release, f))
		}
	}
	for _, r := range rs {
		// A later release than the one whose set this is: a ship still running, kept.
		if r.Manifest.ReleaseSeq > keep.Manifest.ReleaseSeq {
			continue
		}
		set := bootSet(r.Manifest)
		for f, d := range set.disks {
			if !kept[d] {
				byDigest[d] = append(byDigest[d], release.DiskTag(set.release, f))
			}
		}
	}
	var errs []error
	for _, d := range slices.Sorted(maps.Keys(byDigest)) {
		ref, err := p.Registry.Digest(p.diskRepo(), d)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if have, err := p.Registry.Head(ctx, ref); err != nil {
			errs = append(errs, err)
			continue
		} else if have == nil {
			continue // removed already
		}
		if err := p.remove(ctx, p.diskRepo(), d); err != nil {
			errs = append(errs, err)
			continue
		}
		tags := slices.Compact(slices.Sorted(slices.Values(byDigest[d])))
		p.logf("disk %s removed (%s)", registry.Short(d), strings.Join(tags, ", "))
	}
	return errors.Join(errs...)
}

// remove deletes the manifest digest of repo with its signatures: each referrer, the referrers
// fallback index, then the manifest itself.
func (p *Publisher) remove(ctx context.Context, repo, digest string) error {
	d, err := p.Registry.Digest(repo, digest)
	if err != nil {
		return err
	}
	refs, err := p.Registry.Referrers(ctx, d, "")
	if err != nil && !registry.NotFound(err) {
		return err
	}
	for _, r := range refs {
		rd, err := p.Registry.Digest(repo, r.Digest.String())
		if err != nil {
			return err
		}
		if err := p.deleter().DeleteManifest(ctx, rd); err != nil {
			return err
		}
	}
	fallback, err := p.Registry.Tag(repo, strings.Replace(digest, ":", "-", 1))
	if err != nil {
		return err
	}
	if fd, err := p.Registry.Head(ctx, fallback); err != nil {
		return err
	} else if fd != nil {
		fr, err := p.Registry.Digest(repo, fd.Digest.String())
		if err != nil {
			return err
		}
		if err := p.deleter().DeleteManifest(ctx, fr); err != nil {
			return err
		}
	}
	return p.deleter().DeleteManifest(ctx, d)
}

// untag removes tag. A registry refusing to delete a tag alone has the manifest deleted by digest
// when no other tag of the repository holds it; otherwise the tag is retired (moved onto the
// retired artifact, which reads as absent) and the content returned: the other tags hold it, and
// sweepHeld deletes it once they no longer do. A retired tag is left as it is.
func (p *Publisher) untag(ctx context.Context, tag name.Tag) (*name.Digest, error) {
	err := p.Registry.DeleteTag(ctx, tag)
	if err == nil || !errors.Is(err, registry.ErrDeleteRefused) {
		return nil, err
	}
	have, herr := p.headFloating(ctx, tag)
	if herr != nil || have == nil {
		return nil, herr
	}
	others, herr := p.holders(ctx, tag.Context(), have.Digest, tag.TagStr())
	if herr != nil {
		return nil, herr
	}
	if len(others) > 0 {
		if err := p.retire(ctx, tag); err != nil {
			return nil, err
		}
		p.logf("%s retired: the registry deletes no tag alone; its content stays under %s", tag, strings.Join(others, ", "))
		d := tag.Context().Digest(have.Digest.String())
		return &d, nil
	}
	return nil, p.remove(ctx, tag.Context().Name(), have.Digest.String())
}

// holders lists the tags of repo but except that hold the manifest d.
func (p *Publisher) holders(ctx context.Context, repo name.Repository, d v1.Hash, except string) ([]string, error) {
	tags, err := p.Registry.Tags(ctx, repo)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range tags {
		if t == except {
			continue
		}
		ref, err := p.Registry.Tag(repo.Name(), t)
		if err != nil {
			return nil, err
		}
		h, err := p.Registry.Head(ctx, ref)
		if err != nil {
			return nil, err
		}
		if h != nil && h.Digest == d {
			out = append(out, t)
		}
	}
	return out, nil
}

// sweepHeld deletes by digest the content untag left to other tags, once none holds it; content
// already gone is skipped, and an error on one digest does not stop the others.
func (p *Publisher) sweepHeld(ctx context.Context, held []name.Digest) error {
	var errs []error
	seen := map[name.Digest]bool{}
	for _, d := range held {
		if seen[d] {
			continue
		}
		seen[d] = true
		if err := p.sweepOne(ctx, d); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// sweepOne deletes d by digest unless it is gone or a tag of its repository holds it.
func (p *Publisher) sweepOne(ctx context.Context, d name.Digest) error {
	have, err := p.Registry.Head(ctx, d)
	if err != nil || have == nil {
		return err
	}
	others, err := p.holders(ctx, d.Context(), have.Digest, "")
	if err != nil || len(others) > 0 {
		return err
	}
	if err := p.remove(ctx, d.Context().Name(), d.DigestStr()); err != nil {
		return err
	}
	p.logf("%s deleted: no tag holds it any more", d)
	return nil
}

// cleanDisks makes rel's disks its channel's set and removes the channel's older disk sets, once
// the channel's pointer is on rel and, on the channel moving latest, each latest-<format> tag of
// its formats is on its disk (syncLatest moves them from the record): a release the pointer is not
// on (a later one is) publishes its disks and leaves the channel's set as it is.
func (p *Publisher) cleanDisks(ctx context.Context, channel string, rel *Ref, replaced diskSet) error {
	ptr, err := p.pointer(ctx, channel)
	if err != nil {
		return err
	}
	if ptr == nil || ptr.Digest != rel.Digest {
		p.logf("the channel %s does not point at %s: its disk set stays as it is", channel, rel.Manifest.Tag)
		return nil
	}
	set := diskSet{release: rel.Manifest.Tag, disks: rel.Manifest.Artifacts.Disks, stowage: rel.Manifest.StowageVersion}
	if err := p.writeDisks(ctx, channel, set); err != nil {
		return err
	}
	if err := p.settleLatest(ctx, channel, rel); err != nil {
		return err
	}
	if p.Project.Spec.Channels[channel].Latest {
		for _, f := range sortedKeys(set.disks) {
			t, err := p.Registry.Tag(p.diskRepo(), release.DiskTag(LatestTag, f))
			if err != nil {
				return err
			}
			d, err := p.Registry.Head(ctx, t)
			if err != nil {
				return err
			}
			if d == nil || d.Digest.String() != set.disks[f] {
				return fmt.Errorf("%s is not on %s's disk: the older disk sets are kept", t, rel.Manifest.Tag)
			}
		}
	}
	return p.pruneDiskSets(ctx, channel, rel, replaced)
}
