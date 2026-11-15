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
	"github.com/norvexlabs/stowage/internal/version"
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

// heldPrefix starts the tag that marks content whose tag was retired while another tag held it:
// held-<the digest's first 12 hex digits>. The registry is the record, so a run stopped part way
// leaves it for the next sweep. No other tag starts so: the channel checks reserve the prefix.
const heldPrefix = version.ReservedPrefix

func heldTag(d v1.Hash) string { return heldPrefix + d.Hex[:12] }

// untag removes tag. A registry refusing to delete a tag alone has the manifest deleted by digest
// when no other tag of the repository holds it; otherwise the content is marked held (heldTag)
// and the tag retired (moved onto the retired artifact, which reads as absent): the other tags
// hold it, and sweepMarked deletes it once they no longer do. A retired tag is left as it is.
func (p *Publisher) untag(ctx context.Context, tag name.Tag) error {
	err := p.Registry.DeleteTag(ctx, tag)
	if err == nil || !errors.Is(err, registry.ErrDeleteRefused) {
		return err
	}
	have, herr := p.headFloating(ctx, tag)
	if herr != nil || have == nil {
		return herr
	}
	others, herr := p.holders(ctx, tag.Context(), have.Digest, tag.TagStr())
	if herr != nil {
		return herr
	}
	if len(others) > 0 {
		if err := p.markHeld(ctx, tag.Context(), have.Digest); err != nil {
			return err
		}
		if err := p.retire(ctx, tag); err != nil {
			return err
		}
		p.logf("%s retired: the registry deletes no tag alone; its content stays under %s", tag, strings.Join(others, ", "))
		return nil
	}
	return p.remove(ctx, tag.Context().Name(), have.Digest.String())
}

// markHeld tags d in repo heldTag(d), before the tag that names it is retired: once no other tag
// holds d, sweepMarked deletes it.
func (p *Publisher) markHeld(ctx context.Context, repo name.Repository, d v1.Hash) error {
	t, err := p.Registry.Tag(repo.Name(), heldTag(d))
	if err != nil {
		return err
	}
	if err := p.Registry.Retag(ctx, repo.Digest(d.String()), t); err != nil {
		return fmt.Errorf("mark %s held: %w", repo.Digest(d.String()), err)
	}
	p.marked = append(p.marked, t)
	return nil
}

// holders lists the tags of repo but except that hold the manifest d; a held mark is none.
func (p *Publisher) holders(ctx context.Context, repo name.Repository, d v1.Hash, except string) ([]string, error) {
	tags, err := p.Registry.Tags(ctx, repo)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range tags {
		if t == except || strings.HasPrefix(t, heldPrefix) {
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

// sweepMarked deletes by digest, in the release and installer repositories and in repos, the
// content any held mark names once no other tag holds it (the mark goes with it), a stopped run's
// included: prune's sweep. Content already gone is skipped, and an error on one does not stop the
// others. A repository with no mark costs one listing of its tags.
func (p *Publisher) sweepMarked(ctx context.Context, repos ...string) error {
	all := append([]string{p.releaseRepo(), p.installerRepo()}, repos...)
	slices.Sort(all)
	var errs []error
	for _, r := range slices.Compact(all) {
		if err := p.sweepRepo(ctx, r, nil); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// sweepOrWarn is the sweep that ends a ship or a yank: of the marks this run made alone, so it
// never judges content another run is working on; a failure is a warning, the marks staying for
// a prune.
func (p *Publisher) sweepOrWarn(ctx context.Context) {
	byRepo := map[string]map[string]bool{}
	for _, t := range p.marked {
		r := t.Context().Name()
		if byRepo[r] == nil {
			byRepo[r] = map[string]bool{}
		}
		byRepo[r][t.TagStr()] = true
	}
	var errs []error
	for _, r := range slices.Sorted(maps.Keys(byRepo)) {
		if err := p.sweepRepo(ctx, r, byRepo[r]); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		p.logf("warning: held content not swept (a prune sweeps it): %v", err)
	}
}

// sweepRepo deletes by digest the content each held mark of the repository names (of only, when
// set) that no other tag holds. Every tag is read once: one map of digest to the tags holding it
// judges every mark.
func (p *Publisher) sweepRepo(ctx context.Context, repoName string, only map[string]bool) error {
	repo, err := p.Registry.Repo(repoName)
	if err != nil {
		return err
	}
	tags, err := p.Registry.Tags(ctx, repo)
	if err != nil {
		if registry.NotFound(err) {
			return nil
		}
		return err
	}
	var marks []string
	for _, t := range tags {
		if strings.HasPrefix(t, heldPrefix) && (only == nil || only[t]) {
			marks = append(marks, t)
		}
	}
	if len(marks) == 0 {
		return nil
	}
	marked := map[string]v1.Hash{} // mark → the content it names
	held := map[v1.Hash]bool{}     // content a tag other than a mark holds
	for _, t := range tags {
		ref, err := p.Registry.Tag(repoName, t)
		if err != nil {
			return err
		}
		h, err := p.Registry.Head(ctx, ref)
		if err != nil {
			return err
		}
		switch {
		case h == nil:
		case strings.HasPrefix(t, heldPrefix):
			marked[t] = h.Digest
		default:
			held[h.Digest] = true
		}
	}
	var errs []error
	done := map[v1.Hash]bool{}
	for _, m := range marks {
		d, ok := marked[m]
		if !ok || held[d] || done[d] {
			continue
		}
		done[d] = true
		if err := p.remove(ctx, repoName, d.String()); err != nil {
			errs = append(errs, err)
			continue
		}
		p.logf("%s deleted: no tag holds it any more", repo.Digest(d.String()))
	}
	return errors.Join(errs...)
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
