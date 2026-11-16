package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
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
	return diskSet{release: tag, disks: disks, stowage: m.BootStowage()}
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

// remove deletes the manifest digest of repo with its signatures, then, an index, each manifest
// it names that nothing else holds (removeAll). The repository's tags are read once for the pass
// (the prune's snapshot when it has one), and only for an index.
func (p *Publisher) remove(ctx context.Context, repo, digest string) error {
	d, err := p.Registry.Digest(repo, digest)
	if err != nil {
		return err
	}
	h, err := v1.NewHash(d.DigestStr())
	if err != nil {
		return err
	}
	r := &removal{name: repo, repo: d.Context(), tags: map[string]v1.Hash{}}
	named, err := p.manifestsOf(ctx, r.repo, h)
	if err != nil {
		return err
	}
	if len(named) > 0 {
		if r.tags, err = p.snap.of(ctx, p, r.repo); err != nil {
			return err
		}
		if r.tags == nil {
			if r.tags, err = p.readTags(ctx, r.repo); err != nil {
				return err
			}
		}
	}
	_, err = p.removeAll(ctx, r, []v1.Hash{h})
	return err
}

// removal is one pass deleting manifests of a repository (removeAll): its tags, each tag's
// manifest digest, kept in step with the pass's own marks and deletions; the manifests it visited
// (done) and those it deleted (gone).
type removal struct {
	name string // the repository as the caller spelled it
	repo name.Repository
	tags map[string]v1.Hash
	done map[v1.Hash]bool
	gone map[v1.Hash]bool
}

// removeOne deletes the manifest h of r's repository with its signatures: each referrer, the
// referrers fallback index, then the manifest itself, and drops every tag on it from r.tags.
func (p *Publisher) removeOne(ctx context.Context, r *removal, h v1.Hash) error {
	d, err := p.Registry.Digest(r.name, h.String())
	if err != nil {
		return err
	}
	refs, err := p.Registry.Referrers(ctx, d, "")
	if err != nil && !registry.NotFound(err) {
		return err
	}
	for _, ref := range refs {
		rd, err := p.Registry.Digest(r.name, ref.Digest.String())
		if err != nil {
			return err
		}
		if err := p.deleter().DeleteManifest(ctx, rd); err != nil {
			return err
		}
		p.snap.dropDigest(rd)
	}
	fallback, err := p.Registry.Tag(r.name, strings.Replace(h.String(), ":", "-", 1))
	if err != nil {
		return err
	}
	if fd, err := p.Registry.Head(ctx, fallback); err != nil {
		return err
	} else if fd != nil {
		fr, err := p.Registry.Digest(r.name, fd.Digest.String())
		if err != nil {
			return err
		}
		if err := p.deleter().DeleteManifest(ctx, fr); err != nil {
			return err
		}
		p.snap.dropDigest(fr)
		dropTagsOn(r.tags, fd.Digest)
	}
	if err := p.deleter().DeleteManifest(ctx, d); err != nil {
		return err
	}
	p.snap.dropDigest(d)
	dropTagsOn(r.tags, h)
	r.gone[h] = true
	return nil
}

// dropTagsOn deletes from tags every tag naming d.
func dropTagsOn(tags map[string]v1.Hash, d v1.Hash) {
	maps.DeleteFunc(tags, func(_ string, h v1.Hash) bool { return h == d })
}

// removeAll deletes roots from r's repository, each with its signatures (removeOne), whatever holds
// it (its caller judged it), then each manifest a deleted index names that nothing still there
// holds (heldContent) — an installer's images, a mirrored multi-platform image's — returning
// every manifest it deleted, in order. It goes in rounds: the indexes first, then the images, as a
// registry refuses to delete a manifest an index still names (zot); what an index named is judged
// in the next round, once every index of this one is gone, so a manifest two deleted indexes name
// goes once, after both, and one a held mark's index outside the pass names stays. Each manifest
// an index names that is left to go is marked held before the index goes, so a run stopped or
// failing between leaves it to a sweep; one already gone when its round comes is skipped. Each
// manifest is visited once, recorded in r.done (a sweep's passes share it). An error on one
// manifest leaves the others to go.
func (p *Publisher) removeAll(ctx context.Context, r *removal, roots []v1.Hash) ([]v1.Hash, error) {
	if r.done == nil {
		r.done = map[v1.Hash]bool{}
	}
	if r.gone == nil {
		r.gone = map[v1.Hash]bool{}
	}
	var deleted []v1.Hash
	var errs []error
	fail := func(err error) ([]v1.Hash, error) { return deleted, errors.Join(append(errs, err)...) }
	forced, cands := roots, []v1.Hash(nil)
	for len(forced) > 0 || len(cands) > 0 {
		// What holds a candidate, which only a deleted index named; a round of roots alone has none.
		var held map[v1.Hash]bool
		if len(cands) > 0 {
			var err error
			if held, err = p.heldContent(ctx, r.repo, r.tags, r.gone); err != nil {
				return fail(err)
			}
		}
		var indexes, images []v1.Hash
		seen := map[v1.Hash]bool{}
		for i, h := range append(slices.Clone(forced), cands...) {
			candidate := i >= len(forced)
			if r.done[h] || seen[h] || (candidate && held[h]) {
				continue
			}
			seen[h] = true
			if candidate {
				// A candidate already gone is skipped: neither read from what manifestsOf kept of
				// it nor reported deleted.
				have, err := p.Registry.Head(ctx, r.repo.Digest(h.String()))
				if err != nil || have == nil {
					r.done[h] = true
					if err != nil {
						errs = append(errs, err)
					}
					continue
				}
			}
			named, err := p.manifestsOf(ctx, r.repo, h)
			if err != nil {
				return fail(err)
			}
			if len(named) > 0 {
				indexes = append(indexes, h)
			} else {
				images = append(images, h)
			}
		}
		// What holds an index's manifests once the indexes of this round are gone.
		if len(indexes) > 0 {
			except := maps.Clone(r.gone)
			for _, h := range indexes {
				except[h] = true
			}
			var err error
			if held, err = p.heldContent(ctx, r.repo, r.tags, except); err != nil {
				return fail(err)
			}
		}
		var next []v1.Hash
		for _, h := range indexes {
			r.done[h] = true
			named, err := p.manifestsOf(ctx, r.repo, h)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			var orphans []v1.Hash
			var markErr error
			for _, m := range named {
				p.noteManifest(m.Digest, m.MediaType.IsIndex())
				if held[m.Digest] || r.done[m.Digest] || slices.Contains(orphans, m.Digest) {
					continue
				}
				if mark := heldTag(m.Digest); r.tags[mark] != m.Digest {
					if markErr = p.markHeld(ctx, r.repo, m.Digest); markErr != nil {
						break
					}
					r.tags[mark] = m.Digest
				}
				orphans = append(orphans, m.Digest)
			}
			if markErr != nil {
				errs = append(errs, markErr)
				continue
			}
			if err := p.removeOne(ctx, r, h); err != nil {
				errs = append(errs, err)
				continue
			}
			deleted = append(deleted, h)
			next = append(next, orphans...)
		}
		for _, h := range images {
			r.done[h] = true
			if err := p.removeOne(ctx, r, h); err != nil {
				errs = append(errs, err)
				continue
			}
			deleted = append(deleted, h)
		}
		forced, cands = nil, next
	}
	return deleted, errors.Join(errs...)
}

// manifestsOf is the manifests the index d of repo names, every entry (an image, an index, an
// attestation); none for an image, or for nothing. Each index is read once per run.
func (p *Publisher) manifestsOf(ctx context.Context, repo name.Repository, d v1.Hash) ([]v1.Descriptor, error) {
	p.indexes.Lock()
	named, ok := p.indexes.of[d]
	p.indexes.Unlock()
	if ok {
		return named, nil
	}
	isIndex, known := p.knownIndex(d)
	if !known {
		desc, err := p.Registry.Head(ctx, repo.Digest(d.String()))
		if err != nil || desc == nil {
			return nil, err
		}
		isIndex = desc.MediaType.IsIndex()
	}
	if isIndex {
		desc, err := p.Registry.Get(ctx, repo.Digest(d.String()))
		if err != nil || desc == nil {
			return nil, err
		}
		if desc.MediaType.IsIndex() {
			im, err := v1.ParseIndexManifest(bytes.NewReader(desc.Manifest))
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", repo.Digest(d.String()), err)
			}
			named = im.Manifests
		}
	}
	p.indexes.Lock()
	if p.indexes.of == nil {
		p.indexes.of = map[v1.Hash][]v1.Descriptor{}
	}
	p.indexes.of[d] = named
	p.indexes.Unlock()
	return named, nil
}

// noteManifest records whether the manifest d is an index, as a HEAD or an index's entry described
// it: manifestsOf then reads it without a HEAD, and an image not at all.
func (p *Publisher) noteManifest(d v1.Hash, isIndex bool) {
	p.indexes.Lock()
	defer p.indexes.Unlock()
	if p.indexes.index == nil {
		p.indexes.index = map[v1.Hash]bool{}
	}
	p.indexes.index[d] = isIndex
}

// knownIndex is whether the manifest d is an index, as noteManifest recorded it (known false:
// never described).
func (p *Publisher) knownIndex(d v1.Hash) (isIndex, known bool) {
	p.indexes.Lock()
	defer p.indexes.Unlock()
	isIndex, known = p.indexes.index[d]
	return isIndex, known
}

// heldContent is the content the tags of repo (tags: each tag's manifest digest) hold, but a tag
// on a manifest of except (one deleted, or being deleted, by the pass): each tag's manifest, and
// every manifest an index among them names, at any depth (manifestsOf). A held mark holds what its
// index names (a registry refuses to delete it while the index is there), never its own manifest,
// which is the sweep's to judge. A referrers fallback index (sha256-<hex>) holds itself alone:
// what it names are referrers, which go with their subject.
func (p *Publisher) heldContent(ctx context.Context, repo name.Repository, tags map[string]v1.Hash, except map[v1.Hash]bool) (map[v1.Hash]bool, error) {
	held := map[v1.Hash]bool{}
	walked := map[v1.Hash]bool{}
	var walk func(d v1.Hash) error
	walk = func(d v1.Hash) error {
		if walked[d] {
			return nil
		}
		walked[d] = true
		named, err := p.manifestsOf(ctx, repo, d)
		if err != nil {
			return err
		}
		for _, m := range named {
			held[m.Digest] = true
			p.noteManifest(m.Digest, m.MediaType.IsIndex())
			if m.MediaType.IsIndex() {
				if err := walk(m.Digest); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, t := range slices.Sorted(maps.Keys(tags)) {
		d := tags[t]
		switch {
		case except[d]:
		case fallbackTag.MatchString(t):
			held[d] = true
		case strings.HasPrefix(t, heldPrefix):
			if err := walk(d); err != nil {
				return nil, err
			}
		default:
			held[d] = true
			if err := walk(d); err != nil {
				return nil, err
			}
		}
	}
	return held, nil
}

// fallbackTag is a referrers fallback index's tag (the OCI distribution spec's): its subject's
// digest, sha256-<hex>.
var fallbackTag = regexp.MustCompile(`^sha256-[0-9a-f]{64}$`)

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
	if err == nil {
		p.snap.drop(tag)
	}
	if err == nil || !errors.Is(err, registry.ErrDeleteRefused) {
		return err
	}
	have, herr := p.headFloating(ctx, tag)
	if herr != nil || have == nil {
		return herr
	}
	p.snap.set(tag, have.Digest)
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
	p.snap.set(t, d)
	p.marked = append(p.marked, t)
	return nil
}

// holders lists the tags of repo but except that hold the manifest d; a held mark is none. With
// a snapshot (a prune's), the repository's tags are the snapshot's, read once per repository;
// otherwise each call lists and reads them.
func (p *Publisher) holders(ctx context.Context, repo name.Repository, d v1.Hash, except string) ([]string, error) {
	tags, err := p.snap.of(ctx, p, repo)
	if err != nil {
		return nil, err
	}
	if tags == nil {
		if tags, err = p.readTags(ctx, repo); err != nil {
			return nil, err
		}
	}
	var out []string
	for _, t := range slices.Sorted(maps.Keys(tags)) {
		if t == except || strings.HasPrefix(t, heldPrefix) {
			continue
		}
		if tags[t] == d {
			out = append(out, t)
		}
	}
	return out, nil
}

// readTags lists the tags of repo and reads the manifest digest each holds; a tag gone between
// the listing and its read is left out.
func (p *Publisher) readTags(ctx context.Context, repo name.Repository) (map[string]v1.Hash, error) {
	tags, err := p.Registry.Tags(ctx, repo)
	if err != nil {
		return nil, err
	}
	out := map[string]v1.Hash{}
	for _, t := range tags {
		ref, err := p.Registry.Tag(repo.Name(), t)
		if err != nil {
			return nil, err
		}
		h, err := p.Registry.Head(ctx, ref)
		if err != nil {
			return nil, err
		}
		if h != nil {
			out[t] = h.Digest
			p.noteManifest(h.Digest, h.MediaType.IsIndex())
		}
	}
	return out, nil
}

// tagSnapshot is, per repository, each tag and the manifest digest it holds: read once, when a
// repository's holders are first needed, and kept in step with this run's own changes (a tag
// deleted, marked held or retired, a manifest deleted by digest). A prune's tag removals hold one,
// so removing n tags of a repository reads its tags once rather than n times; it is dropped before
// the prune moves its pointer and floating tags, which change tags it does not follow. A nil
// snapshot is none: every method does nothing. Its methods take typed references and reach a
// repository under snapKey alone.
type tagSnapshot map[string]map[string]v1.Hash

// tagsOf is repo's tags when they were read (nil otherwise).
func (s tagSnapshot) tagsOf(repo name.Repository) (map[string]v1.Hash, bool) {
	m, ok := s[snapKey(repo)]
	return m, ok
}

// snapKey is repo's key in a snapshot: its canonical name, whatever the spelling it was made from.
func snapKey(repo name.Repository) string { return repo.Name() }

// of is repo's tags, read on first use; nil without a snapshot.
func (s tagSnapshot) of(ctx context.Context, p *Publisher, repo name.Repository) (map[string]v1.Hash, error) {
	if s == nil {
		return nil, nil
	}
	if m, ok := s.tagsOf(repo); ok {
		return m, nil
	}
	m, err := p.readTags(ctx, repo)
	if err != nil {
		return nil, err
	}
	s[snapKey(repo)] = m
	return m, nil
}

// set records tag holding d, in a repository already read.
func (s tagSnapshot) set(tag name.Tag, d v1.Hash) {
	if m, ok := s.tagsOf(tag.Context()); ok {
		m[tag.TagStr()] = d
	}
}

// drop records tag deleted.
func (s tagSnapshot) drop(tag name.Tag) {
	if m, ok := s.tagsOf(tag.Context()); ok {
		delete(m, tag.TagStr())
	}
}

// dropDigest records the manifest d deleted, with every tag on it.
func (s tagSnapshot) dropDigest(d name.Digest) {
	m, ok := s.tagsOf(d.Context())
	if !ok {
		return
	}
	for t, h := range m {
		if h.String() == d.DigestStr() {
			delete(m, t)
		}
	}
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
	tagged := map[string]v1.Hash{} // every tag → the content it names
	for _, t := range tags {
		ref, err := p.Registry.Tag(repoName, t)
		if err != nil {
			return err
		}
		h, err := p.Registry.Head(ctx, ref)
		if err != nil {
			return err
		}
		if h == nil {
			continue
		}
		tagged[t] = h.Digest
		p.noteManifest(h.Digest, h.MediaType.IsIndex())
	}
	// What a tag other than a mark holds, and what an index any tag holds names (a mark's
	// included): removeAll marks an index's manifests before it deletes the index, and a stopped
	// run leaves them marked under an index a re-ship may have tagged again, or another mark's
	// index outside this sweep may name. A manifest a marked index of this sweep names goes with
	// it (removeAll), once no index left names it.
	held, err := p.heldContent(ctx, repo, tagged, nil)
	if err != nil {
		return err
	}
	var roots []v1.Hash
	for _, m := range marks {
		if d, ok := tagged[m]; ok && !held[d] && !slices.Contains(roots, d) {
			roots = append(roots, d)
		}
	}
	if len(roots) == 0 {
		return nil
	}
	deleted, err := p.removeAll(ctx, &removal{name: repoName, repo: repo, tags: tagged}, roots)
	for _, h := range deleted {
		p.logf("%s deleted: no tag holds it any more", repo.Digest(h.String()))
	}
	return err
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
