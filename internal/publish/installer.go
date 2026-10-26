package publish

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"slices"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/norvexlabs/stowage/internal/graph"
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
	r := graph.Release{Version: v, Revision: revision, Channel: channel, Installer: p.Installer, Scanner: p.Scanner}
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
		if f := s.Services[name].Nftables; f != "" {
			if files[payload.NftablesDir+name+".nft"], err = p.projectFile(f); err != nil {
				return nil, fmt.Errorf("services.%s.nftables: %w", name, err)
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

// installerImage is the release's installer image: one linux/amd64 layer holding <short>-stow, the
// installer executable with the release appended, whose default command prints the bootstrap
// script. It is a function of its content alone — no timestamp — so the same release makes the
// same digest. It carries no inputs digest: it embeds the release's outputs (image digests,
// number, date), which no inputs digest covers, so it is never kept on its inputs — ship makes it
// again from the manifest and compares digests.
func (p *Publisher) installerImage(m *release.Manifest) (v1.Image, error) {
	files, err := p.installerFiles(m)
	if err != nil {
		return nil, err
	}
	var exe bytes.Buffer
	if err := payload.Write(&exe, bytes.NewReader(p.Installer), files); err != nil {
		return nil, err
	}
	ctl := providers.Ctl(p.Project.Spec.Short())
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	if err := tw.WriteHeader(&tar.Header{Name: ctl, Mode: 0o755, Size: int64(exe.Len()), Typeflag: tar.TypeReg,
		ModTime: time.Unix(0, 0), Format: tar.FormatPAX}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(exe.Bytes()); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	// Compressed once, here: the gzip stream (no name, no time) is the layer's bytes.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(layer.Bytes()); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(gz.Bytes())), nil
	}, tarball.WithMediaType(types.OCILayer))
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
	platform, err := v1.ParsePlatform(graph.Platform)
	if err != nil {
		return nil, err
	}
	cf.OS, cf.Architecture = platform.OS, platform.Architecture
	cf.Config.Entrypoint = []string{"/" + ctl}
	cf.Config.Cmd = []string{payload.ScriptCommand}
	cf.Config.Labels = map[string]string{"org.opencontainers.image.version": m.Tag}
	return mutate.ConfigFile(img, cf)
}

// publishInstaller puts img in the installer repository by digest, signs it, then moves the
// release's tag to it, and returns its digest. What is already in place is left alone.
func (p *Publisher) publishInstaller(ctx context.Context, img v1.Image, tag string) (string, error) {
	h, err := img.Digest()
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
		if err := p.Registry.WriteImage(ctx, d, img); err != nil {
			return "", err
		}
	}
	if err := p.ensureSigned(ctx, d); err != nil {
		return "", fmt.Errorf("installer: %w", err)
	}
	if err := p.ensureSBOM(ctx, d); err != nil {
		return "", fmt.Errorf("installer: %w", err)
	}
	t, err := p.Registry.Tag(p.installerRepo(), tag)
	if err != nil {
		return "", err
	}
	if have, err := p.Registry.Head(ctx, t); err != nil {
		return "", err
	} else if have == nil || have.Digest != h {
		p.logf("installer: tag %s → %s", registry.Short(h.String()), tag)
		if err := p.Registry.Retag(ctx, d, t); err != nil {
			return "", err
		}
	}
	return h.String(), nil
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
// (currentDisks), which only a ship publishing disks moves. Every disk format is listed, so a
// format the project dropped loses its tag.
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
		for _, f := range spec.DiskFormats {
			t, err := p.Registry.Tag(p.diskRepo(), release.DiskTag(LatestTag, f))
			if err != nil {
				return nil, err
			}
			out = append(out, latest{"disk " + f, t, p.diskRepo(), cur.disks[f], true})
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
// moved onto the retired artifact, which reads as absent. Its manifest is never deleted by
// digest here: a release's own tag, or a disk set, may name it; the disk cleanup removes a disk
// no set keeps.
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
