package install

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/norvexlabs/stowage/internal/backup"
	"github.com/norvexlabs/stowage/internal/front"
	"github.com/norvexlabs/stowage/internal/lint"
	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
)

// RegistryCAFile is the install's copy of the CA its registry's certificate chains to (a mirror's
// own), recorded at the first install from --registry-ca.
const RegistryCAFile = "registry-ca.pem"

// dockerCertsDir is where the Docker daemon finds a registry's CA: <dir>/<host[:port]>/ca.crt.
var dockerCertsDir = "/etc/docker/certs.d"

// VMRegistryCA is where a VM's cloud-config writes its registry's CA, at every boot, before
// adding it to the VM's trust store.
func VMRegistryCA(name string) string {
	return "/usr/local/share/ca-certificates/" + name + "-registry.crt"
}

// registryCA is the CA the registry's certificate chains to when the system does not trust it:
// --registry-ca, else the one the install recorded (but for a command that keeps no install,
// givenCA); nil when neither.
func (in *Installer) registryCA() ([]byte, error) {
	if in.O.RegistryCA != "" {
		ca, err := registry.ReadCA(in.O.RegistryCA)
		if err != nil {
			return nil, fmt.Errorf("--registry-ca: %w", err)
		}
		return ca, nil
	}
	if in.O.Dir == "" || in.givenCA {
		return nil, nil
	}
	if _, err := os.Stat(in.path(envName)); err != nil { // no install owns the directory
		return nil, nil
	}
	ca, err := registry.ReadCA(in.path(RegistryCAFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return ca, err
}

// registryClient reads the registry at prefix with the login keychain gives it, trusting the
// registry's CA (registryCA) besides the system's.
func (in *Installer) registryClient(prefix string) (*registry.Client, error) {
	kc, err := in.keychain(prefix)
	if err != nil {
		return nil, err
	}
	return in.clientWith(kc)
}

func (in *Installer) clientWith(kc authn.Keychain) (*registry.Client, error) {
	ca, err := in.registryCA()
	if err != nil || ca == nil {
		return registry.New(kc), err
	}
	pool, err := registry.CertPool(ca)
	if err != nil {
		return nil, err
	}
	return registry.New(kc, registry.WithRootCAs(pool)), nil
}

// dockerCA makes the Docker daemon trust the registry's CA before it pulls from prefix: the
// daemon reads it from dockerCertsDir, which only root writes. A loopback registry needs none
// (the daemon takes it as insecure); a CA already there is kept, and another one is refused, not
// replaced.
func (in *Installer) dockerCA(prefix string) error {
	ca, err := in.registryCA()
	if err != nil || ca == nil {
		return err
	}
	repo, err := registry.Repository(prefix)
	if err != nil {
		return err
	}
	host := repo.RegistryStr()
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = strings.Trim(host, "[]") // an IPv6 address without a port
	}
	if registry.Loopback(h) {
		return nil
	}
	dst := filepath.Join(dockerCertsDir, host, "ca.crt")
	src := cmp.Or(in.O.RegistryCA, in.path(RegistryCAFile))
	switch cur, err := os.ReadFile(dst); {
	case err == nil && holdsAll(cur, ca):
		return nil
	case err == nil:
		return fmt.Errorf("%s holds another CA than %s: the Docker daemon would not trust %s's certificate; replace it (as root) if %s is the right one", dst, src, host, src)
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	var made []string // the directories this run makes, deepest first
	for d := filepath.Dir(dst); ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); !errors.Is(err, os.ErrNotExist) || d == filepath.Dir(d) {
			break
		}
		made = append(made, d)
	}
	err = os.MkdirAll(filepath.Dir(dst), 0o755)
	if err == nil {
		in.madeDockerCADirs = made
		err = writeAtomic(dst, bytes.NewReader(ca), 0o644, nil)
	}
	if err != nil {
		return fmt.Errorf("the Docker daemon trusts %s's CA from %s, which this run could not write (%w): as root, `install -D -m 0644 %s %s`, then run again", host, dst, err, src, dst)
	}
	in.wroteDockerCA = dst
	in.O.logf("wrote %s: the Docker daemon trusts %s's CA", dst, host)
	return nil
}

// publishFile gives the file at tmp the name file, never replacing one: a link fails when the name
// exists. A filesystem without links (FAT, exFAT: a USB stick) gets a rename after a last check.
func publishFile(tmp, file string) error {
	err := os.Link(tmp, file)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s exists: move it away, or give another --output", file)
	}
	if err == nil {
		return nil
	}
	if _, lerr := os.Lstat(file); lerr == nil {
		return fmt.Errorf("%s exists: move it away, or give another --output", file)
	}
	return os.Rename(tmp, file)
}

// holdsAll reports whether the PEM bundle have holds every certificate of want.
func holdsAll(have, want []byte) bool {
	certs := func(data []byte) map[string]bool {
		m := map[string]bool{}
		for rest := data; ; {
			var b *pem.Block
			if b, rest = pem.Decode(rest); b == nil {
				return m
			}
			if b.Type == "CERTIFICATE" {
				m[string(b.Bytes)] = true
			}
		}
	}
	h, w := certs(have), certs(want)
	for c := range w {
		if !h[c] {
			return false
		}
	}
	return len(w) > 0
}

// MirrorExportRequest is what `mirror export` exports: the channel's latest release, or version
// Version (at Revision, 0 the latest), into File; the VM disks of Formats (nil all, empty none),
// and only what Shapes run (nil: every shape of the project).
type MirrorExportRequest struct {
	Channel, File string
	Version       string
	Revision      int
	Formats       []string
	Shapes        []string
}

// MirrorExport writes one release of a channel into one archive (an OCI image layout, as a tar):
// its release manifest under its tag and the channel's, its images, installer, chart (the
// kubernetes shape), OS bundle and the disks of the formats given (the kairos shape: the disk set
// it boots, its own or its channel's), each by digest with its signatures. It reads the project's
// registry with the pull account. An existing file is refused.
func (in *Installer) MirrorExport(ctx context.Context, r MirrorExportRequest) error {
	o, name := &in.O, in.R.Spec.Name
	in.givenCA = true
	channel := cmp.Or(r.Channel, in.R.Manifest.Channel)
	if !lint.ChannelName(channel) {
		return fmt.Errorf("%q is not a channel name", channel)
	}
	shapes := map[string]bool{}
	for _, sh := range in.R.Spec.Shapes.Names() {
		shapes[sh] = r.Shapes == nil
	}
	for _, sh := range r.Shapes {
		if _, ok := shapes[sh]; !ok {
			return fmt.Errorf("--shapes: %s has no shape %s (it has: %s)", name, sh, strings.Join(in.R.Spec.Shapes.Names(), ", "))
		}
		shapes[sh] = true
	}
	if !shapes["kairos"] && len(r.Formats) > 0 {
		return errors.New("--disks: the kairos shape is not exported (--shapes), so no disk is")
	}
	prefix := in.R.Spec.Registry.Prefix
	c, err := in.registryClient(prefix)
	if err != nil {
		return err
	}
	relRepo := name + "-release"
	var m *release.Manifest
	var d v1.Hash
	if r.Version == "" {
		ptr, err := c.Tag(prefix+"/"+relRepo, channel)
		if err != nil {
			return err
		}
		a, _, err := c.GetArtifact(ctx, ptr, release.MediaType)
		if err != nil {
			return err
		}
		if a == nil {
			return fmt.Errorf("the channel %s has no release in %s", channel, prefix)
		}
		if m, err = release.Decode(a.Data); err != nil {
			return fmt.Errorf("the release on %s could not be read (from another Stowage? use that release's installer): %w", channel, err)
		}
		if m.Name != name || m.Channel != channel {
			return fmt.Errorf("the release on %s is %s's on %s, not %s's", channel, m.Name, m.Channel, name)
		}
	} else {
		trust, err := trusted("this installer", in.R.Keys)
		if err != nil {
			return err
		}
		if m, err = findRevision(ctx, c, prefix, name, channel, r.Version, r.Revision, trust, o.logf); err != nil {
			return err
		}
	}
	tag, err := c.Tag(prefix+"/"+relRepo, m.Tag)
	if err != nil {
		return err
	}
	hd, err := c.Head(ctx, tag)
	if err != nil {
		return err
	}
	if hd == nil {
		return fmt.Errorf("%s: the channel's release has no tag of its own", tag)
	}
	d = hd.Digest
	var entries []registry.Entry
	for _, k := range sortedKeys(m.Artifacts.Images) {
		entries = append(entries, registry.Entry{Repo: name + "-" + k, Tag: m.Tag, Digest: m.Artifacts.Images[k]})
	}
	if m.Artifacts.Installer != "" {
		entries = append(entries, registry.Entry{Repo: name + "-installer", Tag: m.Tag, Digest: m.Artifacts.Installer})
	}
	if ch := m.Artifacts.Chart; ch != nil && shapes["kubernetes"] {
		entries = append(entries, registry.Entry{Repo: ch.Repository, Tag: ch.Version, Digest: ch.Digest})
	}
	if m.Artifacts.OS != "" && shapes["kairos"] {
		entries = append(entries, registry.Entry{Repo: name + "-os", Tag: m.Tag, Digest: m.Artifacts.OS})
	}
	bootTag, disks := m.Artifacts.Boot(m.Tag)
	all := sortedKeys(disks)
	formats := r.Formats
	if formats == nil && shapes["kairos"] {
		formats = all
	}
	for _, f := range formats {
		if !slices.Contains(all, f) {
			return fmt.Errorf("--disks: %s %s has no %s disk (it has: %s)", name, m.Tag, f, cmp.Or(strings.Join(all, ", "), "none"))
		}
		ref, err := c.Digest(prefix+"/"+name+"-disk", disks[f])
		if err != nil {
			return err
		}
		if have, err := c.Head(ctx, ref); err != nil {
			return err
		} else if have == nil {
			return fmt.Errorf("--disks: the %s disk %s %s boots (%s's) is no longer published: a newer disk set replaced it; export the channel's latest release for a VM, or give --disks none", f, name, m.Tag, bootTag)
		}
		entries = append(entries, registry.Entry{Repo: name + "-disk", Tag: release.DiskTag(bootTag, f), Digest: disks[f]})
	}
	// The release last, its channel's tag after it: an import pushes in this order, so a mirror
	// never names a release whose artifacts it does not hold yet. The channel's tag holds the
	// release exported.
	entries = append(entries,
		registry.Entry{Repo: relRepo, Tag: m.Tag, Digest: d.String()},
		registry.Entry{Repo: relRepo, Tag: channel, Digest: d.String()})
	file := r.File
	file = cmp.Or(file, fmt.Sprintf("%s-%s.oci.tar", name, m.Tag))
	if _, err := os.Lstat(file); err == nil {
		return fmt.Errorf("%s exists: move it away, or give another --output", file)
	}
	o.logf("exporting %s %s (revision %d, %s) from %s", name, m.ProjectVersion, m.Revision, m.Tag, prefix)
	// Written beside its name and linked to it once whole: a run that stops leaves no archive.
	f, err := os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+".part-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // once linked, the name stays
	st, err := c.Export(ctx, prefix, entries, f, o.logf)
	// FAT and exFAT refuse a mode change, and need none.
	if cerr := f.Chmod(0o644); errors.Is(cerr, fs.ErrPermission) {
		o.logf("%s: its mode could not be set (%v): the filesystem keeps none, or the archive is yours alone", file, cerr)
	} else {
		err = errors.Join(err, cerr)
	}
	err = errors.Join(err, f.Sync(), f.Close())
	if err == nil {
		err = publishFile(f.Name(), file)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "Exported %s %s (revision %d, %s) to %s: %d manifests, %d blobs, %s\n", name, m.ProjectVersion, m.Revision, m.Tag, file, st.Manifests, st.Blobs, sizeOf(st.Bytes))
	return nil
}

// MirrorImport pushes an archive MirrorExport wrote into the registry under prefix (a mirror), with
// the Docker client's login for it and trusting --registry-ca: every manifest by digest, under
// the tags it was published with, so its signatures still verify there.
func (in *Installer) MirrorImport(ctx context.Context, file, prefix string, older bool) error {
	o, name := &in.O, in.R.Spec.Name
	in.givenCA = true
	prefix = strings.TrimSuffix(prefix, "/")
	if _, err := registry.Repository(prefix); err != nil {
		return fmt.Errorf("--to: %w", err)
	}
	a, err := registry.ReadArchive(file)
	if err != nil {
		return err
	}
	defer a.Close()
	m, h, err := archivedRelease(a, name)
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	if err := releaseTags(a, m, h); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	// The release's own repositories, and no other project's: its images are the ones this
	// installer's project names (an image another release adds needs that release's installer).
	ours := map[string]bool{name + "-release": true, name + "-installer": true, name + "-os": true, name + "-disk": true}
	known := map[string]bool{front.Key: true, backup.Key: true}
	for _, k := range in.R.Spec.ImageNames() {
		known[k] = true
	}
	for _, k := range sortedKeys(m.Artifacts.Images) {
		if !known[k] {
			return fmt.Errorf("%s: its release has the image %s, which this %s does not know: import it with the %s of %s %s (in the archive's %s-installer)", file, k, in.R.Ctl(), providers.Ctl(m.ShortName), name, m.ProjectVersion, name)
		}
		ours[name+"-"+k] = true
	}
	if ch := m.Artifacts.Chart; ch != nil {
		if ch.Repository != release.ChartRepository(name, m.Channel) {
			return fmt.Errorf("%s: its chart is in %s, not %s", file, ch.Repository, release.ChartRepository(name, m.Channel))
		}
		ours[ch.Repository] = true
	}
	c, err := in.clientWith(authn.DefaultKeychain)
	if err != nil {
		return err
	}
	// The mirror's channel never goes back to an earlier publication, nor a yanked release
	// forward again.
	relRepo := prefix + "/" + name + "-release"
	cur, _, err := readRelease(ctx, c, relRepo, m.Channel)
	if err != nil {
		return err
	}
	switch {
	case cur != nil && (cur.Name != name || cur.Channel != m.Channel):
		return fmt.Errorf("%s's %s channel holds %s's release on %s, not %s's: nothing imported", prefix, m.Channel, cur.Name, cur.Channel, name)
	case cur != nil && cur.ReleaseSeq > m.ReleaseSeq && !older:
		return fmt.Errorf("%s's %s channel holds %s %s (revision %d, publication %d), after the archive's %s (revision %d, publication %d): nothing imported (--older imports it, when the channel went back where it was published: a yank)", prefix, m.Channel, name, cur.ProjectVersion, cur.Revision, cur.ReleaseSeq, m.ProjectVersion, m.Revision, m.ReleaseSeq)
	}
	// The same publication, yanked in the mirror, is not published there again; a rebuild of its
	// tag is another publication.
	if was, _, err := readRelease(ctx, c, relRepo, m.Tag); err != nil {
		return err
	} else if was != nil && was.Yanked != nil && m.Yanked == nil && was.ReleaseSeq == m.ReleaseSeq {
		return fmt.Errorf("%s %s is yanked in %s (%s): nothing imported", name, m.Tag, prefix, was.Yanked.Reason)
	}
	st, err := c.Import(ctx, a, prefix, func(repo string) bool { return ours[repo] }, o.logf)
	if err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "Imported %s into %s: %d manifests, %d blobs, %s\n", file, prefix, st.Manifests, st.Blobs, sizeOf(st.Bytes))
	return nil
}

// archivedRelease is the release an archive carries: <name>-release holds two tags, the
// release's own and its channel's, both on the one manifest, which must be name's release.
func archivedRelease(a *registry.Archive, name string) (*release.Manifest, v1.Hash, error) {
	tags := map[string]v1.Hash{}
	var h v1.Hash
	for _, d := range a.Manifests {
		if t := d.Annotations[registry.RefNameAnnotation]; t != "" && d.Annotations[registry.RepositoryAnnotation] == name+"-release" {
			tags[t], h = d.Digest, d.Digest
		}
	}
	if len(tags) == 0 {
		return nil, h, fmt.Errorf("it holds no %s release: it is not an export of %s", name, name)
	}
	for t, d := range tags {
		if d != h {
			return nil, h, fmt.Errorf("its %s-release tags name different manifests (%s)", name, t)
		}
	}
	body, err := a.Read(h)
	if err != nil {
		return nil, h, err
	}
	var mf struct {
		ArtifactType string          `json:"artifactType"`
		Layers       []v1.Descriptor `json:"layers"`
	}
	if err := json.Unmarshal(body, &mf); err != nil || mf.ArtifactType != release.MediaType || len(mf.Layers) != 1 || mf.Layers[0].MediaType != release.MediaType {
		return nil, h, fmt.Errorf("%s-release@%s is not a release manifest", name, h)
	}
	data, err := a.Read(mf.Layers[0].Digest)
	if err != nil {
		return nil, h, err
	}
	m, err := release.Decode(data)
	if err != nil {
		return nil, h, fmt.Errorf("its release could not be read (from another Stowage? use that release's installer): %w", err)
	}
	if m.Name != name {
		return nil, h, fmt.Errorf("it carries %s's release, not %s's", m.Name, name)
	}
	if _, ok := tags[m.Tag]; !ok || len(tags) != 2 || tags[m.Channel] != h {
		return nil, h, fmt.Errorf("its %s-release must hold the tags %s and %s alone", name, m.Tag, m.Channel)
	}
	return m, h, nil
}

// releaseTags checks every tagged manifest of the archive against its release m (at h): each tag
// is one the release gives an artifact, on the digest it records — as an export writes them — so
// an import moves no tag of another release.
func releaseTags(a *registry.Archive, m *release.Manifest, h v1.Hash) error {
	name := m.Name
	want := map[string]string{name + "-release:" + m.Tag: h.String(), name + "-release:" + m.Channel: h.String()}
	for k, d := range m.Artifacts.Images {
		want[name+"-"+k+":"+m.Tag] = d
	}
	if m.Artifacts.Installer != "" {
		want[name+"-installer:"+m.Tag] = m.Artifacts.Installer
	}
	if m.Artifacts.OS != "" {
		want[name+"-os:"+m.Tag] = m.Artifacts.OS
	}
	bootTag, disks := m.Artifacts.Boot(m.Tag)
	for f, d := range disks {
		want[name+"-disk:"+release.DiskTag(bootTag, f)] = d
	}
	if ch := m.Artifacts.Chart; ch != nil {
		want[ch.Repository+":"+ch.Version] = ch.Digest
	}
	for _, d := range a.Manifests {
		t := d.Annotations[registry.RefNameAnnotation]
		if t == "" {
			continue // by digest alone: it moves no tag
		}
		ref := d.Annotations[registry.RepositoryAnnotation] + ":" + t
		if w, ok := want[ref]; !ok || w != d.Digest.String() {
			return fmt.Errorf("it tags %s@%s, which its release %s does not name", ref, registry.Short(d.Digest.String()), m.Tag)
		}
	}
	return nil
}

// sizeOf is a byte count for people.
func sizeOf(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d bytes", n)
}
