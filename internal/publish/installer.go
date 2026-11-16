package publish

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/norvexlabs/stowage/internal/graph"
	"github.com/norvexlabs/stowage/internal/installerbin"
	"github.com/norvexlabs/stowage/internal/lock"
	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/version"
)

func (p *Publisher) installerRepo() string {
	return p.Project.Spec.Registry.Prefix + "/" + p.Project.Spec.Name + "-installer"
}

// Graph computes the release's build graph with what this publisher's installer carries.
func (p *Publisher) Graph(v string, revision int, channel string) (*graph.Graph, error) {
	r := graph.Release{Version: v, Revision: revision, Channel: channel, Installer: p.Installer}
	if a := p.Pull; a != nil {
		r.Pull = a.User + ":" + a.Password
	}
	return graph.Build(p.Project, r)
}

// readyToPublish refuses a publisher that cannot make the installer: a build of Stowage without
// it, a project whose stowage.lock does not pin this Stowage (the installer and the front are
// this Stowage's), or a project whose pull account's password is not in the environment.
func (p *Publisher) readyToPublish() error {
	if p.CloudErr != nil {
		return p.CloudErr
	}
	if len(p.Installer) == 0 {
		return errors.New("this stowage carries no installer to publish (a build without it): use a release build of stowage (make build)")
	}
	// The set is complete (installerbin.Read); each executable is checked against its recorded
	// sha256 as a ship first reads it, before anything is written (checkInstallers).
	if err := p.Unpinned(); err != nil {
		return err
	}
	if env, err := p.PullUnset(); err != nil || env != "" {
		if err == nil {
			err = fmt.Errorf("registry.pull: %s is not set: the pull account's password is baked into the installer", env)
		}
		return err
	}
	return nil
}

// checkInstallers reads each executable once (Installer.Verify: decompressed, checked against its
// recorded sha256, which the graph's inputs take, and its recorded size): a ship that makes no
// installer index before it writes anything calls it first, as each goes in the installer image.
func (p *Publisher) checkInstallers() error {
	for _, pl := range installerbin.Platforms {
		inst := p.Installer[pl]
		if inst == nil {
			return fmt.Errorf("this stowage's installers: none for %s: use a release build of stowage (make build)", pl)
		}
		if err := inst.Verify(); err != nil {
			return fmt.Errorf("this stowage's installers: %w", err)
		}
	}
	return nil
}

// Unpinned refuses a publisher whose version is not the one the project's stowage.lock pins.
func (p *Publisher) Unpinned() error {
	if err := (version.SemVer{}).Validate(p.StowageVersion); err != nil {
		return fmt.Errorf("this stowage is not a release build (version %q): publish with a release build (make build)", p.StowageVersion)
	}
	l := p.Project.Lock
	if l == nil {
		return fmt.Errorf("%s is missing: pin this stowage (%s) with stowage lock, then commit the file", lock.FileName, p.StowageVersion)
	}
	order, err := l.Compare(p.StowageVersion)
	switch {
	case err != nil:
		return err
	case order < 0:
		return fmt.Errorf("%s pins stowage %s, this is an older %s: publish with stowage %s", lock.FileName, l.Stowage, p.StowageVersion, l.Stowage)
	case order > 0:
		return fmt.Errorf("%s pins stowage %s, this is %s: publish with stowage %s, or move the pin with stowage lock --update (the installer and the front change with it)",
			lock.FileName, l.Stowage, p.StowageVersion, l.Stowage)
	}
	return nil
}

// PullUnset names the environment variable the project's pull account reads its password from
// when that account is a token and the publisher was given no password; "" otherwise.
func (p *Publisher) PullUnset() (string, error) {
	pull := p.Project.Spec.Registry.Pull
	if pull.Name != "token" || p.Pull != nil {
		return "", nil
	}
	c, err := providers.Token(pull)
	if err != nil {
		return "", fmt.Errorf("registry.pull: %w", err)
	}
	return c.TokenEnv, nil
}

// installerFiles is the payload the installer carries: the release manifest, the spec's runtime
// view, the compose file, the trusted public keys and the pull account. The manifest is carried
// without the installer's own digest and without a yank, which is read from the registry: a yank
// never changes the installer.
func (p *Publisher) installerFiles(m *release.Manifest) (payload.Files, error) {
	carried := *m
	carried.Artifacts.Installer, carried.Yanked = "", nil
	man, err := carried.Encode()
	if err != nil {
		return nil, err
	}
	return p.payloadFiles(payload.Release, man, p.Project.Spec.RuntimeView())
}

// payloadFiles is an installer's payload with the manifest man at file and the spec's view.
func (p *Publisher) payloadFiles(file string, man []byte, view spec.Spec) (payload.Files, error) {
	s := p.Project.Spec
	viewJSON, err := json.Marshal(view)
	if err != nil {
		return nil, err
	}
	compose, err := p.projectFile(s.Stack.Compose)
	if err != nil {
		return nil, fmt.Errorf("stack.compose: %w", err)
	}
	files := payload.Files{file: man, payload.Spec: viewJSON, payload.Compose: compose}
	for _, name := range slices.Sorted(maps.Keys(s.Services)) {
		t := s.Services[name].Nftables
		if t.File != "" {
			if files[payload.NftablesDir+name+".nft"], err = p.projectFile(t.File); err != nil {
				return nil, fmt.Errorf("services.%s.nftables: %w", name, err)
			}
		}
		for _, v := range t.Values() {
			if files[payload.NftablesDir+name+"/"+v+".nft"], err = p.projectFile(t.Files[v]); err != nil {
				return nil, fmt.Errorf("services.%s.nftables.files.%s: %w", name, v, err)
			}
		}
	}
	if s.Signs() {
		c, err := providers.CosignKey(s.Signing)
		if err != nil {
			return nil, fmt.Errorf("signing: %w", err)
		}
		for _, k := range c.PublicKeys {
			n := payload.KeysDir + path.Base(k)
			if _, dup := files[n]; dup {
				return nil, fmt.Errorf("signing.public_keys: two keys are named %s", path.Base(k))
			}
			if files[n], err = p.projectFile(k); err != nil {
				return nil, fmt.Errorf("signing.public_keys: %w", err)
			}
		}
	}
	if a := p.Pull; a != nil {
		if files[payload.Pull], err = json.Marshal(a); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func (p *Publisher) projectFile(rel string) ([]byte, error) {
	f, err := spec.ProjectPath(p.Project.Root, rel)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(f)
}

// installerIndex is the release's installer image: an OCI index of one image per platform of
// installerbin.Platforms, in that order, each descriptor naming its platform (installerImage); a
// target runs the installerbin.Target one. It is a function of its content alone — no timestamp — so the same release makes the
// same digest. It carries no inputs digest: it embeds the release's outputs (image digests,
// number, date), which no inputs digest covers, so it is never kept on its inputs — ship makes it
// again from the manifest and compares digests.
func (p *Publisher) installerIndex(m *release.Manifest) (v1.ImageIndex, error) {
	files, err := p.installerFiles(m)
	if err != nil {
		return nil, err
	}
	// The payload appended to each executable, made once: the bytes the key hashes are the bytes
	// each image ships.
	var appended bytes.Buffer
	if err := payload.Write(&appended, bytes.NewReader(nil), files); err != nil {
		return nil, err
	}
	key := installerKey(appended.Bytes(), p.Installer)
	if p.lastIndex.idx != nil && p.lastIndex.key == key {
		return p.lastIndex.idx, nil
	}
	var entries []mutate.IndexAddendum
	for _, pl := range installerbin.Platforms {
		platform, err := v1.ParsePlatform(pl)
		if err != nil {
			return nil, err
		}
		inst := p.Installer[pl]
		if inst == nil {
			return nil, fmt.Errorf("no installer for %s", pl)
		}
		img, err := p.installerImage(m, appended.Bytes(), inst, *platform)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", pl, err)
		}
		entries = append(entries, mutate.IndexAddendum{Add: img, Descriptor: v1.Descriptor{Platform: platform}})
	}
	idx := mutate.AppendManifests(mutate.IndexMediaType(empty.Index, types.OCIImageIndex), entries...)
	p.lastIndex.key, p.lastIndex.idx = key, idx
	return idx, nil
}

// installerKey names what an installer index is made of beside the project (<short>-stow): the
// payload appended to each executable (the manifest, whose tag labels its images too) and each
// executable's sha256. The same key makes the index made last.
func installerKey(appended []byte, s installerbin.Set) [sha256.Size]byte {
	h := sha256.New()
	h.Write(appended)
	for _, pl := range installerbin.Platforms {
		fmt.Fprintf(h, "\x00%s=%s", pl, s.Digest(pl))
	}
	var key [sha256.Size]byte
	h.Sum(key[:0])
	return key
}

// installerImage is the installer index's image for platform: one layer holding one file,
// <short>-stow, the platform's installer executable (inst) with the release's payload files
// appended, whose default command prints the bootstrap script.
func (p *Publisher) installerImage(m *release.Manifest, appended []byte, inst *installerbin.Installer, platform v1.Platform) (v1.Image, error) {
	size := inst.Size()
	ctl := providers.Ctl(p.Project.Spec.Short())
	hdr := tar.Header{Name: ctl, Mode: 0o755, Size: size + int64(len(appended)), Typeflag: tar.TypeReg,
		ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
	l, err := p.installerLayer(hdr, inst, appended)
	if err != nil {
		return nil, err
	}
	base := mutate.ConfigMediaType(mutate.MediaType(empty.Image, types.OCIManifestSchema1), types.OCIConfigJSON)
	img, err := mutate.Append(base, mutate.Addendum{Layer: l, MediaType: types.OCILayer})
	if err != nil {
		return nil, err
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	cf.OS, cf.Architecture = platform.OS, platform.Architecture
	cf.Config.Entrypoint = []string{"/" + ctl}
	cf.Config.Cmd = []string{payload.ScriptCommand}
	cf.Config.Labels = map[string]string{"org.opencontainers.image.version": m.Tag}
	return mutate.ConfigFile(img, cf)
}

// installerLayer is the layer of the tar of one file, hdr: inst's executable with appended after
// it. One pass, once per run for one content (p.layers), compresses its tar (no name, no time: one
// content makes one digest), streamed from the executable, which is never kept: that pass makes
// its digests and size and the compressed bytes its push sends.
func (p *Publisher) installerLayer(hdr tar.Header, inst *installerbin.Installer, appended []byte) (v1.Layer, error) {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%d\x00", inst.Digest, hdr.Name, hdr.Size)
	h.Write(appended)
	var key [sha256.Size]byte
	h.Sum(key[:0])
	l := &streamedLayer{open: func() io.ReadCloser { return installerTar(hdr, inst, appended) }}
	p.layers.Lock()
	sums, ok := p.layers.of[key]
	p.layers.Unlock()
	if !ok {
		var err error
		if sums, err = l.read(); err != nil {
			return nil, err
		}
		p.layers.Lock()
		if p.layers.of == nil {
			p.layers.of = map[[sha256.Size]byte]layerSums{}
		}
		p.layers.of[key] = sums
		p.layers.Unlock()
	}
	l.layerSums = sums
	return l, nil
}

// layerSums are a layer's digest (of its compressed stream), diffID (of its tar), size
// (compressed) and compressed stream (zipped), a fraction of its tar's size.
type layerSums struct {
	digest, diffID v1.Hash
	size           int64
	zipped         []byte
}

// streamedLayer is an OCI gzip layer whose tar open streams on each read, compressed at
// gzip.DefaultCompression with no name and no time (compress) by read: the same tar makes the
// same compressed stream, whose sums and bytes it holds.
type streamedLayer struct {
	layerSums
	open func() io.ReadCloser
}

func (l *streamedLayer) Digest() (v1.Hash, error)            { return l.digest, nil }
func (l *streamedLayer) DiffID() (v1.Hash, error)            { return l.diffID, nil }
func (l *streamedLayer) Size() (int64, error)                { return l.size, nil }
func (l *streamedLayer) MediaType() (types.MediaType, error) { return types.OCILayer, nil }
func (l *streamedLayer) Uncompressed() (io.ReadCloser, error) {
	return l.open(), nil
}
func (l *streamedLayer) Compressed() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(l.zipped)), nil
}

// read is the layer's sums and compressed stream, by one pass over its tar.
func (l *streamedLayer) read() (layerSums, error) {
	tr := l.open()
	defer tr.Close()
	raw, zipped := sha256.New(), sha256.New()
	var buf bytes.Buffer
	if err := compress(io.MultiWriter(zipped, &buf), io.TeeReader(tr, raw)); err != nil {
		return layerSums{}, err
	}
	return layerSums{
		digest: v1.Hash{Algorithm: "sha256", Hex: fmt.Sprintf("%x", zipped.Sum(nil))},
		diffID: v1.Hash{Algorithm: "sha256", Hex: fmt.Sprintf("%x", raw.Sum(nil))},
		size:   int64(buf.Len()),
		zipped: buf.Bytes(),
	}, nil
}

// compress writes r to w gzip-compressed at gzip.DefaultCompression, with no name and no time.
func compress(w io.Writer, r io.Reader) error {
	zw, err := gzip.NewWriterLevel(w, gzip.DefaultCompression)
	if err != nil {
		return err
	}
	if _, err := io.Copy(zw, r); err != nil {
		return err
	}
	return zw.Close()
}

// installerTar streams the tar of one file, hdr: inst's executable (checked as it is read,
// installerbin.Installer.Open) followed by appended, its payload.
func installerTar(hdr tar.Header, inst *installerbin.Installer, appended []byte) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		exe, err := inst.Open()
		if err == nil {
			tw := tar.NewWriter(pw)
			err = tw.WriteHeader(&hdr)
			if err == nil {
				_, err = io.Copy(tw, io.MultiReader(exe, bytes.NewReader(appended)))
			}
			if err == nil {
				err = tw.Close()
			}
			exe.Close()
		}
		pw.CloseWithError(err)
	}()
	return pr
}

// publishInstaller puts idx in the installer repository by digest, signs it, then moves the
// release's tag to it, and returns its digest. What is already in place is left alone.
func (p *Publisher) publishInstaller(ctx context.Context, idx v1.ImageIndex, tag string) (string, error) {
	h, err := p.pushInstaller(ctx, idx)
	if err != nil {
		return "", err
	}
	return h, p.tagInstaller(ctx, h, tag)
}

// pushInstaller pushes the installer index by digest with its images, signs each image and the
// index, and attests each image's SBOM (of its own platform) and the index's (its target image's):
// no tag moves.
func (p *Publisher) pushInstaller(ctx context.Context, idx v1.ImageIndex) (string, error) {
	h, err := idx.Digest()
	if err != nil {
		return "", err
	}
	d, err := p.Registry.Digest(p.installerRepo(), h.String())
	if err != nil {
		return "", err
	}
	if have, err := p.Registry.Head(ctx, d); err != nil {
		return "", err
	} else if have == nil {
		p.logf("installer: push %s", registry.Short(h.String()))
		if err := p.Registry.WriteIndex(ctx, d, idx); err != nil {
			return "", err
		}
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return "", err
	}
	var target *name.Digest
	for _, e := range im.Manifests {
		ed, err := p.Registry.Digest(p.installerRepo(), e.Digest.String())
		if err != nil {
			return "", err
		}
		if e.Platform == nil {
			return "", fmt.Errorf("installer: %s names no platform", ed)
		}
		pl := e.Platform.String()
		if err := p.ensureSigned(ctx, ed); err != nil {
			return "", fmt.Errorf("installer %s: %w", pl, err)
		}
		if err := p.ensureSBOM(ctx, ed, pl); err != nil {
			return "", fmt.Errorf("installer %s: %w", pl, err)
		}
		if pl == installerbin.Target {
			target = &ed
		}
	}
	if target == nil {
		return "", fmt.Errorf("installer: %s has no %s image", d, installerbin.Target)
	}
	if err := p.ensureSigned(ctx, d); err != nil {
		return "", fmt.Errorf("installer: %w", err)
	}
	// The index carries its target image's SBOM: what a reader of the release's installer digest
	// audits is what a target runs.
	if err := p.ensureSBOMOf(ctx, d, *target, "its "+installerbin.Target+" image"); err != nil {
		return "", fmt.Errorf("installer: %w", err)
	}
	return h.String(), nil
}

// checkInstallerImages checks with checkSig the signature of each image of the installer index
// inst, when it is in place and signatures are checked: each is signed on its own digest. An
// installer a Stowage before indexes made is one image, checked as the index is; one missing is
// reported by the index's check.
func (p *Publisher) checkInstallerImages(ctx context.Context, inst string, checkSig func(repo, digest string) error) error {
	if !p.signed() {
		return nil
	}
	d, err := p.Registry.Digest(p.installerRepo(), inst)
	if err != nil {
		return err
	}
	entries, _, err := p.Registry.Platforms(ctx, d)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := checkSig(p.installerRepo(), e.Digest.String()); err != nil {
			return err
		}
	}
	return nil
}

// tagInstaller points the installer's release tag at the installer h, pushed by pushInstaller.
func (p *Publisher) tagInstaller(ctx context.Context, h, tag string) error {
	d, err := p.Registry.Digest(p.installerRepo(), h)
	if err != nil {
		return err
	}
	t, err := p.Registry.Tag(p.installerRepo(), tag)
	if err != nil {
		return err
	}
	if have, err := p.Registry.Head(ctx, t); err != nil {
		return err
	} else if have == nil || have.Digest.String() != h {
		p.logf("installer: tag %s → %s", registry.Short(h), tag)
		if err := p.Registry.Retag(ctx, d, t); err != nil {
			return err
		}
	}
	return nil
}

// LatestTag is the installer tag the bootstrap one-liner pulls.
const LatestTag = "latest"

// latest is a floating tag that follows the pointer of the channel that sets latest: the
// installer's LatestTag and, for the kairos shape, each disk format's latest-<format>.
type latest struct {
	what string // "installer", "disk qcow2"
	tag  name.Tag
	repo string
	want string // the pointer release's artifact; "" when it has none: the tag is removed
	disk bool   // a disk's: it follows the channel's disk set, not the pointer
}

// latestTags are channel's floating tags for rel (nil: no release), none when channel does not
// set latest: the installer's follows rel, each disk format's the channel's disk set
// (currentDisks), which only a ship publishing disks moves. The disk formats are the set's and
// those a latest-<format> tag of the disk repository names (latestFormats), so a format the
// project dropped, or one only a newer stowage makes, loses its tag once the set has none of it.
func (p *Publisher) latestTags(ctx context.Context, channel string, rel *Ref) ([]latest, error) {
	if !p.Project.Spec.Channels[channel].Latest {
		return nil, nil
	}
	var a release.Artifacts
	if rel != nil {
		a = rel.Manifest.Artifacts
	}
	t, err := p.Registry.Tag(p.installerRepo(), LatestTag)
	if err != nil {
		return nil, err
	}
	out := []latest{{"installer", t, p.installerRepo(), a.Installer, false}}
	if p.hasOS() {
		cur, err := p.currentDisks(ctx, channel)
		if err != nil {
			return nil, err
		}
		have, err := p.latestFormats(ctx)
		if err != nil {
			return nil, err
		}
		formats := append(slices.Collect(maps.Keys(cur.disks)), have...)
		slices.Sort(formats)
		for _, f := range slices.Compact(formats) {
			t, err := p.Registry.Tag(p.diskRepo(), release.DiskTag(LatestTag, f))
			if err != nil {
				return nil, err
			}
			out = append(out, latest{"disk " + f, t, p.diskRepo(), cur.disks[f], true})
		}
	}
	return out, nil
}

// latestFormats are the formats a latest-<format> tag of the disk repository names, a retired one
// included. A format holds no '-' (spec.DiskFormats) while a release's disk tag <tag>-<format>
// starting with latest- holds two or more, so none is taken for a format's.
func (p *Publisher) latestFormats(ctx context.Context) ([]string, error) {
	repo, err := p.Registry.Repo(p.diskRepo())
	if err != nil {
		return nil, err
	}
	tags, err := p.Registry.Tags(ctx, repo)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range tags {
		if f, ok := strings.CutPrefix(t, LatestTag+"-"); ok && f != "" && !strings.Contains(f, "-") {
			out = append(out, f)
		}
	}
	return out, nil
}

// syncLatest points channel's floating tags at rel's artifacts, and checks each holds its digest
// afterwards: an existence check would pass on a stale tag. An artifact rel lacks removes its tag:
// none is offered rather than another release's.
func (p *Publisher) syncLatest(ctx context.Context, channel string, rel *Ref) error {
	ls, err := p.latestTags(ctx, channel, rel)
	if err != nil {
		return err
	}
	for _, l := range ls {
		if err := p.syncTag(ctx, l, rel.Manifest.Tag); err != nil {
			return err
		}
	}
	return nil
}

func (p *Publisher) syncTag(ctx context.Context, l latest, relTag string) error {
	if l.want == "" {
		return p.dropTag(ctx, l)
	}
	if have, err := p.Registry.Head(ctx, l.tag); err != nil {
		return err
	} else if have != nil && have.Digest.String() == l.want {
		return nil
	}
	d, err := p.Registry.Digest(l.repo, l.want)
	if err != nil {
		return err
	}
	if err := p.Registry.Retag(ctx, d, l.tag); err != nil {
		return err
	}
	if have, err := p.Registry.Head(ctx, l.tag); err != nil {
		return err
	} else if have == nil || have.Digest.String() != l.want {
		return fmt.Errorf("%s did not move to %s", l.tag, l.want)
	}
	p.logf("%s :%s → %s", l.what, l.tag.TagStr(), relTag)
	return nil
}

// dropLatest removes (or retires, as dropTag does) channel's installer latest tag; its disk tags follow its disk set, which a
// channel left without a release keeps.
func (p *Publisher) dropLatest(ctx context.Context, channel string) error {
	ls, err := p.latestTags(ctx, channel, nil)
	if err != nil {
		return err
	}
	for _, l := range ls {
		if l.disk {
			if err := p.syncTag(ctx, l, "the channel's disk set"); err != nil {
				return err
			}
			continue
		}
		if err := p.dropTag(ctx, l); err != nil {
			return err
		}
	}
	return nil
}

// dropTag removes a floating tag. A registry that deletes no tag alone has it retired instead:
// moved onto the retired artifact, which reads as absent. Its manifest is never deleted here: a
// release's own tag, or a disk set, may name it. A non-disk tag's content is marked held first,
// so sweepMarked deletes it once no tag holds it; a disk's is the disk cleanup's, which removes a
// disk no set keeps.
// A retired tag is deleted where the registry deletes tags alone, and left as it is elsewhere.
func (p *Publisher) dropTag(ctx context.Context, l latest) error {
	r, err := registry.RetiredDigest()
	if err != nil {
		return fmt.Errorf("retired artifact: %w", err)
	}
	have, err := p.Registry.Head(ctx, l.tag)
	if err != nil || have == nil {
		return err
	}
	if err := p.Registry.DeleteTag(ctx, l.tag); errors.Is(err, registry.ErrDeleteRefused) {
		if have.Digest == r {
			return nil
		}
		if !l.disk {
			if err := p.markHeld(ctx, l.tag.Context(), have.Digest); err != nil {
				return err
			}
		}
		if err := p.retire(ctx, l.tag); err != nil {
			return err
		}
		p.logf("%s :%s retired: the registry deletes no tag alone", l.what, l.tag.TagStr())
		return nil
	} else if err != nil {
		return err
	}
	p.logf("%s :%s removed", l.what, l.tag.TagStr())
	return nil
}

// retire moves tag onto the retired artifact, which reads as absent, and reads it back.
func (p *Publisher) retire(ctx context.Context, tag name.Tag) error {
	if _, err := p.Registry.PutArtifact(ctx, tag, registry.Retired()); err != nil {
		return err
	}
	if rd, err := registry.RetiredDigest(); err == nil {
		p.snap.set(tag, rd)
	}
	have, err := p.headFloating(ctx, tag)
	if err != nil {
		return err
	}
	if have != nil {
		return fmt.Errorf("%s was not retired: it is %s", tag, have.Digest)
	}
	return nil
}

// headFloating is the descriptor a floating tag holds; nil when it is absent or retired.
func (p *Publisher) headFloating(ctx context.Context, tag name.Tag) (*v1.Descriptor, error) {
	r, err := registry.RetiredDigest()
	if err != nil {
		return nil, fmt.Errorf("retired artifact: %w", err)
	}
	d, err := p.Registry.Head(ctx, tag)
	if err != nil || d == nil || d.Digest == r {
		return nil, err
	}
	return d, nil
}

// settleLatest puts the channel's floating tags right for its pointer, once a command has moved
// what it moves: removed when there is no pointer, the pointer's artifacts when its release is
// offered and complete. A pointer on a release no longer offered (a yank or prune that stopped
// early) moves back as they would have moved it; one on an incomplete release leaves the tags for
// verify to report. checked is a release verified just before (nil: none), not checked again.
func (p *Publisher) settleLatest(ctx context.Context, channel string, checked *Ref) error {
	if !p.Project.Spec.Channels[channel].Latest {
		return nil
	}
	ptr, err := p.pointer(ctx, channel)
	if err != nil {
		return err
	}
	if ptr == nil {
		return p.dropLatest(ctx, channel)
	}
	live, err := p.livePointer(ctx, channel)
	if err != nil {
		return err
	}
	if live == nil {
		return p.retreatPointer(ctx, channel)
	}
	if checked == nil || checked.Digest != live.Digest {
		if err := p.verifyRelease(ctx, live, live.Manifest.Tag); incomplete(err) {
			p.logf("warning: :%s tags not checked: %s: %v", LatestTag, live.Manifest.Tag, err)
			return nil
		} else if err != nil {
			return err
		}
	}
	return p.syncLatest(ctx, channel, live)
}

// verifyLatest checks, on the channel that sets latest, that each floating tag holds the artifact
// of rel, the pointer's release, and is absent when there is no release (rel nil) or it lacks
// that artifact.
func (p *Publisher) verifyLatest(ctx context.Context, channel string, rel *Ref) error {
	ls, err := p.latestTags(ctx, channel, rel)
	if err != nil {
		return err
	}
	var problems []string
	for _, l := range ls {
		d, err := p.headFloating(ctx, l.tag)
		if err != nil {
			return err
		}
		what := "the channel has no release"
		switch {
		case l.disk:
			what = "the channel has no disk set of that format"
		case rel != nil:
			what = "the channel's release " + rel.Manifest.Tag + " has no " + l.what
		}
		switch {
		case l.want == "" && d != nil:
			problems = append(problems, fmt.Sprintf("%s is %s, and %s: yank or prune again to remove it", l.tag, d.Digest, what))
		case l.want == "":
		case d == nil:
			problems = append(problems, fmt.Sprintf("%s is missing", l.tag))
		case d.Digest.String() != l.want:
			problems = append(problems, fmt.Sprintf("%s is %s, the channel's %s is %s", l.tag, d.Digest, l.what, l.want))
		}
	}
	if len(problems) > 0 {
		return &incompleteError{problems}
	}
	return nil
}
