package install

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"golang.org/x/sync/errgroup"

	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/sigverify"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/version"
)

// NextDir prefixes the directory (one per run, NextDir-*) holding the installer `upgrade --version`
// fetched while it runs; it is kept when that run fails, so its hints name a file that exists.
const NextDir = ".stowage-next"

// maxInstaller bounds the installer executable read from an installer image.
var maxInstaller int64 = 512 << 20

// ExitCode is the exit status of an installer run as a child, which printed its own error.
type ExitCode int

func (e ExitCode) Error() string { return "exit status " + strconv.Itoa(int(e)) }

// UpgradeRevision is a plain `upgrade` run by the installer of the release the install runs: when
// the install's channel holds a later revision of that version, it upgrades to it as UpgradeTo does,
// so a fix shipped as a revision reaches the install. It answers false, doing nothing, when this
// installer carries another release (Upgrade moves to it), the install never pulls (made
// --offline), the channel holds no later revision, or it cannot be read (logged): Upgrade then
// says the install already runs the release, as before.
func (in *Installer) UpgradeRevision(ctx context.Context, args []string) (bool, error) {
	env, err := in.open()
	if err != nil {
		return false, err
	}
	m := in.R.Manifest
	name, cur, rev, err := sameProject(in.O.Dir, env, m)
	if err != nil {
		return false, err
	}
	if cur != m.ProjectVersion || rev != strconv.Itoa(m.Revision) || !pinnedImages(env) {
		return false, nil
	}
	latest, c, prefix, _, err := in.channelRelease(ctx, env, name, cur, rev, cur)
	if err != nil {
		in.O.logf("no later revision of %s %s looked for: %v", name, cur, err)
		return false, nil
	}
	// findRelease kept the manifests of this version alone, as its scheme reads them.
	if latest.Revision <= m.Revision {
		return false, nil
	}
	return true, in.runFetched(ctx, c, prefix, name, latest, args)
}

// channelRelease is version v's latest revision not yanked on the channel of the install's
// release (it runs name cur, revision rev), signed by a key the install trusts, read from the
// registry the install pulls from, with that registry's client and prefix.
func (in *Installer) channelRelease(ctx context.Context, env *EnvFile, name, cur, rev, v string) (*release.Manifest, *registry.Client, string, *sigverify.Verifier, error) {
	trust, kept, err := in.installTrust(name, cur, rev)
	if err != nil {
		return nil, nil, "", nil, err
	}
	prefix, _ := env.Get("STOWAGE_REGISTRY")
	c, err := in.registryClient(prefix)
	if err != nil {
		return nil, nil, "", nil, err
	}
	m, err := findRelease(ctx, c, prefix, name, kept.Manifest.Channel, v, trust, in.O.logf)
	return m, c, prefix, trust, err
}

// installTrust is the keys an install trusts (it runs name cur, revision rev), and its kept
// release: the keys of the install's release decide; the installer run's only when it carries
// none (an install of a release from before its project signed): an older installer must not
// bring back a key a later release dropped.
func (in *Installer) installTrust(name, cur, rev string) (*sigverify.Verifier, *Release, error) {
	kept, err := in.keptInstaller(name, cur, rev)
	if err != nil {
		return nil, nil, err
	}
	src, keys := in.ctl(), kept.Keys
	if len(keys) == 0 {
		src, keys = "this installer", in.R.Keys
	}
	trust, err := trusted(src, keys)
	return trust, kept, err
}

// UpgradeTo moves the install to version v of the project, on the channel the install's release
// came from: it fetches that release's installer from the registry the install pulls from (a
// mirror included, never another one), checks it carries v, and runs its `upgrade` with args.
// Among v's revisions the latest one not yanked is taken.
func (in *Installer) UpgradeTo(ctx context.Context, v string, args []string) error {
	env, err := in.open()
	if err != nil {
		return err
	}
	name, cur, rev, err := sameProject(in.O.Dir, env, in.R.Manifest)
	if err != nil {
		return err
	}
	if !pinnedImages(env) {
		return errors.New("--version: this install was made with --offline and never pulls: run the new release's installer, `upgrade --dir " + in.O.Dir + "`")
	}
	m, c, prefix, _, err := in.channelRelease(ctx, env, name, cur, rev, v)
	if err != nil {
		return err
	}
	// Ordered by the scheme the release records, as its own `upgrade` orders it; decided here
	// first, so nothing is fetched to be refused.
	order, err := compareTo(m, cur)
	if err != nil {
		return err
	}
	r, err := strconv.Atoi(rev)
	if err != nil {
		return fmt.Errorf("the install's revision %q is not a number", rev)
	}
	switch {
	case order > 0 || (order == 0 && r > m.Revision):
		return fmt.Errorf("%s runs %s %s (revision %s), after %s %s (revision %d): `%s rollback --dir %s` goes back one upgrade", in.O.Dir, name, cur, rev, name, m.ProjectVersion, m.Revision, in.ctl(), in.O.Dir)
	case order == 0 && r == m.Revision:
		fmt.Fprintf(in.O.Out, "%s already runs %s %s (revision %s)\n", in.O.Dir, name, cur, rev)
		return nil
	}
	return in.runFetched(ctx, c, prefix, name, m, args)
}

// runFetched fetches the installer of release m from the registry c reads at prefix and runs
// its `upgrade` with args; the installer is kept when that fails.
func (in *Installer) runFetched(ctx context.Context, c *registry.Client, prefix, name string, m *release.Manifest, args []string) error {
	in.O.logf("fetching the installer of %s %s (revision %d, %s)", name, m.ProjectVersion, m.Revision, m.Tag)
	exe, err := in.fetchInstaller(ctx, c, prefix, m)
	if err != nil {
		return err
	}
	if err := in.O.exec(ctx, exe, append([]string{"upgrade", "--dir", in.O.Dir}, args...)); err != nil {
		in.O.logf("the fetched installer is kept: %s", exe)
		return err
	}
	if err := os.RemoveAll(filepath.Dir(exe)); err != nil {
		in.O.logf("the fetched installer could not be removed: %v", err)
	}
	return nil
}

// scheme is the version scheme a release records; nil for a `command` scheme, the project's own
// program, which an install does not run.
func scheme(m *release.Manifest) (version.Scheme, error) {
	if m.Scheme.Provider == "command" {
		return nil, nil
	}
	s, err := version.NewScheme("", spec.Provider{Name: m.Scheme.Provider, Config: m.Scheme.Config})
	if err != nil {
		return nil, fmt.Errorf("%s: the release's version scheme: %w", m.Tag, err)
	}
	return s, nil
}

// compareTo orders the install's version cur against m's, by m's scheme: 0 when equal, which a
// `command` scheme decides by spelling only (and calls any other version earlier, leaving the
// order to the release's own upgrade).
func compareTo(m *release.Manifest, cur string) (int, error) {
	s, err := scheme(m)
	if err != nil {
		return 0, err
	}
	if s == nil {
		if cur == m.ProjectVersion {
			return 0, nil
		}
		return -1, nil
	}
	if err := s.Validate(cur); err != nil {
		return 0, fmt.Errorf("the install's version %q under %s's scheme: %w", cur, m.Tag, err)
	}
	return s.Compare(cur, m.ProjectVersion)
}

// isVersion reports whether m is version v by m's own scheme (by spelling for a `command` scheme).
func isVersion(m *release.Manifest, v string) bool {
	s, err := scheme(m) // built once by readRelease
	if err != nil || s == nil {
		return err == nil && m.ProjectVersion == v
	}
	c, err := s.Compare(m.ProjectVersion, v)
	return err == nil && c == 0
}

// keychain is the login the installer's registry reads use: the project's pull account on its
// own registry, as the image pulls use it (pullAccount); anonymous on the registry of a project
// whose images are public; else (a mirror included) the Docker client's own login.
func (in *Installer) keychain(prefix string) (authn.Keychain, error) {
	if in.R.Spec.Registry.Pull.Name == "none" && prefix == in.R.Spec.Registry.Prefix {
		return registry.Fixed(authn.Anonymous), nil
	}
	user, token, err := in.pullAccount(prefix)
	if err != nil || token == "" {
		return authn.DefaultKeychain, err
	}
	return registry.Fixed(authn.FromConfig(authn.AuthConfig{Username: user, Password: token})), nil
}

// readers bounds the release manifests findRelease reads at once.
const readers = 8

// findRelease reads the release manifests of the project's release repository and returns
// version v's (by the scheme each records) on channel: its latest revision not yanked, refused
// unless it is signed by a key trust holds (nil: the install trusts no key, its project does not
// sign). A tag holding something else, or a pointer (whose manifest names another tag), is passed
// over; the channel's pointer, the reserved tags and signature tags are not read.
func findRelease(ctx context.Context, c *registry.Client, prefix, name, channel, v string, trust *sigverify.Verifier, logf func(string, ...any)) (*release.Manifest, error) {
	return findRevision(ctx, c, prefix, name, channel, v, 0, trust, logf)
}

// findRevision is findRelease of revision rev of v; 0 is its latest revision.
func findRevision(ctx context.Context, c *registry.Client, prefix, name, channel, v string, rev int, trust *sigverify.Verifier, logf func(string, ...any)) (*release.Manifest, error) {
	repo, err := c.Repo(prefix + "/" + name + "-release")
	if err != nil {
		return nil, err
	}
	tags, err := c.Tags(ctx, repo)
	if err != nil {
		return nil, err
	}
	tags = slices.DeleteFunc(tags, func(t string) bool {
		return t == channel || slices.Contains(version.ReservedTags, t) || strings.HasPrefix(t, "sha256-")
	})
	slices.Sort(tags) // a tie between equal spellings does not depend on the registry's listing
	ms := make([]*release.Manifest, len(tags))
	hs := make([]v1.Hash, len(tags))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(readers)
	for i, t := range tags {
		g.Go(func() (err error) {
			ms[i], hs[i], err = readRelease(gctx, c, repo.String(), t)
			return err
		})
	}
	// Fail closed: a manifest that cannot be read could be V's latest revision.
	if err := g.Wait(); err != nil {
		return nil, err
	}
	// V's candidates in order of preference: the latest revision, among equal revisions the
	// spelling typed, else the first tag in sorted order.
	type candidate struct {
		m *release.Manifest
		h v1.Hash
	}
	var live, yanked []candidate
	for i, t := range tags {
		m := ms[i]
		if m == nil || m.Name != name || m.Tag != t || m.Channel != channel || !isVersion(m, v) || (rev > 0 && m.Revision != rev) {
			continue
		}
		if m.Yanked != nil {
			yanked = append(yanked, candidate{m, hs[i]})
		} else {
			live = append(live, candidate{m, hs[i]})
		}
	}
	prefer := func(a, b candidate) int {
		return cmp.Or(cmp.Compare(b.m.Revision, a.m.Revision), // the latest revision
			cmp.Compare(b2i(b.m.ProjectVersion == v), b2i(a.m.ProjectVersion == v))) // the spelling typed
	}
	slices.SortStableFunc(live, prefer)
	slices.SortStableFunc(yanked, prefer)
	// The first one signed by a trusted key, live ones first: a manifest without that signature
	// (pushed by someone without the key) is passed over — neither offered nor allowed to hide a
	// signed one — and a yank counts only signed too. One whose signatures cannot be read stops
	// the search: it may be the latest signed revision, and an older one must not stand in for
	// it. With none signed, the first refusal is the answer.
	var refused error
	for _, x := range append(live, yanked...) {
		if err := checkSigned(ctx, c, repo, x.h, x.m, trust); err != nil {
			if !errors.Is(err, sigverify.ErrUnsigned) {
				return nil, err
			}
			logf("passed over: %v", err)
			refused = cmp.Or(refused, err)
			continue
		}
		if x.m.Yanked != nil {
			return nil, fmt.Errorf("%s %s is yanked on %s: %s", name, v, channel, x.m.Yanked.Reason)
		}
		return x.m, nil
	}
	if refused != nil {
		return nil, refused
	}
	if rev > 0 {
		return nil, fmt.Errorf("%s %s (revision %d) is not published on %s in %s", name, v, rev, channel, repo)
	}
	return nil, fmt.Errorf("%s %s is not published on %s in %s (a mirror holds the %s-release and %s-installer repositories too)", name, v, channel, repo, name, name)
}

// readRelease reads the release manifest under tag and its digest; nil when the tag holds
// something else or is gone. A manifest this installer cannot read, or whose scheme it cannot
// build, is an error.
func readRelease(ctx context.Context, c *registry.Client, repo, tag string) (*release.Manifest, v1.Hash, error) {
	ref, err := c.Tag(repo, tag)
	if err != nil {
		return nil, v1.Hash{}, err
	}
	a, h, err := c.GetArtifact(ctx, ref, release.MediaType)
	if errors.Is(err, registry.ErrNotArtifact) || (err == nil && a == nil) {
		return nil, v1.Hash{}, nil
	}
	if err != nil {
		return nil, v1.Hash{}, err
	}
	m, err := release.Decode(a.Data)
	if err == nil {
		_, err = scheme(m)
	}
	if err != nil {
		return nil, v1.Hash{}, fmt.Errorf("release manifest %s could not be read (from another Stowage? run that release's installer): %w", tag, err)
	}
	return m, h, nil
}

// b2i is 1 for true.
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// trusted is the verifier of the public keys a release carries, named by src (where they come
// from, for errors); nil when it carries none, its project not signing.
func trusted(src string, keys map[string][]byte) (*sigverify.Verifier, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	named := map[string][]byte{}
	for n, k := range keys {
		named[src+": "+n] = k
	}
	v, err := sigverify.New(named)
	if err != nil {
		return nil, fmt.Errorf("trusted public keys: %w", err)
	}
	return v, nil
}

// checkSigned refuses the release m, read from repo with the digest h, unless h carries a
// signature by a key trust holds; with no trust (the install's project does not sign) nothing is
// checked. Once an install trusts a key, an unsigned release is refused: no silent downgrade.
func checkSigned(ctx context.Context, c *registry.Client, repo name.Repository, h v1.Hash, m *release.Manifest, trust *sigverify.Verifier) error {
	if trust == nil {
		return nil
	}
	err := trust.Verify(ctx, c, repo.Digest(h.String()))
	if errors.Is(err, sigverify.ErrUnsigned) {
		return fmt.Errorf("%s %s (revision %d, %s) is not signed by a key this install trusts: refused (%w)", m.Name, m.ProjectVersion, m.Revision, m.Tag, err)
	}
	if err != nil {
		return fmt.Errorf("%s %s (revision %d, %s): its signature could not be checked: %w", m.Name, m.ProjectVersion, m.Revision, m.Tag, err)
	}
	return nil
}

// fetchInstaller pulls m's installer image by digest, writes its executable in NextDir and returns
// its path once its payload is checked to carry m: the manifest as published, but for the
// installer's own digest and a yank, which the payload never records.
func (in *Installer) fetchInstaller(ctx context.Context, c *registry.Client, prefix string, m *release.Manifest) (string, error) {
	if m.Artifacts.Installer == "" {
		return "", fmt.Errorf("%s %s (%s) has no installer", m.Name, m.ProjectVersion, m.Tag)
	}
	ref, err := c.Digest(prefix+"/"+m.Name+"-installer", m.Artifacts.Installer)
	if err != nil {
		return "", err
	}
	img, err := c.Image(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ref, err)
	}
	layers, err := img.Layers()
	if err != nil {
		return "", fmt.Errorf("%s: %w", ref, err)
	}
	if len(layers) != 1 {
		return "", fmt.Errorf("%s: %d layers, an installer image has one", ref, len(layers))
	}
	dir, err := os.MkdirTemp(in.O.Dir, NextDir+"-") // 0700, one per run
	if err != nil {
		return "", err
	}
	exe := filepath.Join(dir, providers.Ctl(m.ShortName))
	if err := extract(layers[0], providers.Ctl(m.ShortName), exe); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("%s: %w", ref, err)
	}
	if err := carries(exe, m); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("%s: %w", ref, err)
	}
	return exe, nil
}

// layer is the part of an image layer extract reads.
type layer interface {
	Uncompressed() (io.ReadCloser, error)
}

// extract writes the file called file of a layer to dst, executable by its owner only. The layer
// is read to its end, so its digest is checked.
func extract(l layer, file, dst string) error {
	rc, err := l.Uncompressed()
	if err != nil {
		return err
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Name != file {
			continue
		}
		if found || h.Typeflag != tar.TypeReg {
			return fmt.Errorf("%s is not one regular file", file)
		}
		if h.Size > maxInstaller {
			return fmt.Errorf("%s: %d bytes, more than %d", file, h.Size, maxInstaller)
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		found = true
	}
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no %s in the layer", file)
	}
	return nil
}

// carries checks that the installer at exe carries the release m, by its release manifest alone:
// the installer of a release a newer Stowage built carries a spec this one may not read, and its
// own run reads it.
func carries(exe string, m *release.Manifest) error {
	files, err := payload.Read(exe)
	if err != nil {
		return err
	}
	got, err := payloadRelease(files)
	if err != nil {
		return err
	}
	want := *m
	want.Artifacts.Installer, want.Yanked = "", nil
	a, err := want.Encode()
	if err != nil {
		return err
	}
	b, err := got.Encode()
	if err != nil {
		return err
	}
	if !bytes.Equal(a, b) {
		return fmt.Errorf("the installer carries %s %s (revision %d, %s), not the release %s", got.Name, got.ProjectVersion, got.Revision, got.Tag, m.Tag)
	}
	return nil
}

// exec runs an installer as a child on this run's terminal. An interrupt reaches it (a terminal
// sends it to both); a stop of this run alone is passed on, and the child is waited for.
func (o *Options) exec(ctx context.Context, exe string, args []string) error {
	if o.Exec != nil {
		return o.Exec(ctx, exe, args)
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, o.Out, o.Log
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() > 0 {
		return ExitCode(ee.ExitCode())
	}
	return err
}
