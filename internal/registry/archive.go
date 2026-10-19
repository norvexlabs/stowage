package registry

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// An archive is an OCI image layout (image-spec v1.1) as one tar file: `oci-layout`, every blob
// under blobs/sha256/, and index.json, whose descriptors are the archive's top-level manifests.
// Each names its repository (RepositoryAnnotation, relative to a registry prefix) and, unless it is
// untagged, its tag (RefNameAnnotation), so skopeo and oras read it as well.
const (
	// RepositoryAnnotation is the repository a top-level manifest goes to, under the prefix it is
	// imported into.
	RepositoryAnnotation = "dev.stowage.repository"
	// RefNameAnnotation is a top-level manifest's tag, the OCI layout's own annotation.
	RefNameAnnotation = "org.opencontainers.image.ref.name"
	// LayoutVersion is the OCI layout version an archive declares.
	LayoutVersion = "1.0.0"

	layoutFile = "oci-layout"
	indexFile  = "index.json"
	blobDir    = "blobs/sha256/"
)

// repoPath is a repository path the distribution spec allows: components of lower-case letters and
// digits, joined by separators, none of them "." or "..".
var repoPath = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)

// maxManifest bounds a manifest, index.json and oci-layout read from an archive or a registry.
var maxManifest int64 = 4 << 20

// Entry is a manifest an archive carries at its top level: its repository under the prefix, its
// digest and, unless it is untagged, its tag.
type Entry struct {
	Repo   string
	Tag    string
	Digest string
}

// ArchiveStats counts what an export wrote or an import pushed: manifests, blobs, and the blobs'
// bytes.
type ArchiveStats struct {
	Manifests, Blobs int
	Bytes            int64
}

// Export writes to w an archive of entries, each read under prefix by its digest: the manifest,
// every manifest an index names, every blob, and the referrers of each entry (its signatures),
// which go at the top level untagged, in the entry's repository. A blob is written once, and each is
// checked against its digest as it is read. logf, when set, tells each entry as it starts.
func (c *Client) Export(ctx context.Context, prefix string, entries []Entry, w io.Writer, logf func(string, ...any)) (ArchiveStats, error) {
	e := &exporter{c: c, ctx: ctx, tw: tar.NewWriter(w), seen: map[v1.Hash]bool{}, manifests: map[v1.Hash]v1.Descriptor{}, top: map[string]bool{}, referred: map[string]bool{}}
	layout, _ := json.Marshal(map[string]string{"imageLayoutVersion": LayoutVersion})
	if err := e.file(layoutFile, layout); err != nil {
		return e.st, err
	}
	for _, d := range []string{"blobs/", blobDir} {
		if err := e.tw.WriteHeader(&tar.Header{Name: d, Typeflag: tar.TypeDir, Mode: 0o755, ModTime: epoch}); err != nil {
			return e.st, err
		}
	}
	for _, en := range entries {
		repo, err := c.Repo(prefix + "/" + en.Repo)
		if err != nil {
			return e.st, err
		}
		h, err := v1.NewHash(en.Digest)
		if err != nil {
			return e.st, fmt.Errorf("%s: %w", repo, err)
		}
		if logf != nil {
			logf("exporting %s:%s (%s)", repo, cmpTag(en.Tag), Short(en.Digest))
		}
		d, err := e.manifest(repo, h)
		if err != nil {
			return e.st, err
		}
		e.add(en.Repo, en.Tag, d)
		if e.referred[en.Repo+"@"+h.String()] {
			continue
		}
		e.referred[en.Repo+"@"+h.String()] = true
		refs, err := remote.Referrers(repo.Digest(h.String()), c.opts(ctx)...)
		if err != nil {
			return e.st, fmt.Errorf("referrers of %s@%s: %w", repo, h, err)
		}
		im, err := refs.IndexManifest()
		if err != nil {
			return e.st, fmt.Errorf("referrers of %s@%s: %w", repo, h, err)
		}
		for _, r := range im.Manifests {
			d, err := e.manifest(repo, r.Digest)
			if err != nil {
				return e.st, err
			}
			e.add(en.Repo, "", d)
		}
	}
	index, err := json.Marshal(v1.IndexManifest{SchemaVersion: 2, MediaType: types.OCIImageIndex, Manifests: e.index})
	if err != nil {
		return e.st, err
	}
	if err := e.file(indexFile, index); err != nil {
		return e.st, err
	}
	return e.st, e.tw.Close()
}

func cmpTag(t string) string {
	if t == "" {
		return "(untagged)"
	}
	return t
}

// epoch is every archive entry's time: the same release makes the same archive.
var epoch = time.Unix(0, 0)

type exporter struct {
	c    *Client
	ctx  context.Context
	tw   *tar.Writer
	seen map[v1.Hash]bool // blobs written
	// manifests written, by digest: one is read from the registry once.
	manifests map[v1.Hash]v1.Descriptor
	top       map[string]bool
	referred  map[string]bool // repo@digest whose referrers were read
	index     []v1.Descriptor
	st        ArchiveStats
}

func (e *exporter) file(name string, data []byte) error {
	if err := e.tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data)), ModTime: epoch}); err != nil {
		return err
	}
	_, err := e.tw.Write(data)
	return err
}

// add lists d at the top level, once per repository and tag.
func (e *exporter) add(repo, tag string, d v1.Descriptor) {
	key := repo + ":" + tag + "@" + d.Digest.String()
	if e.top[key] {
		return
	}
	e.top[key] = true
	d.Annotations = map[string]string{RepositoryAnnotation: repo}
	if tag != "" {
		d.Annotations[RefNameAnnotation] = tag
	}
	d.Platform, d.URLs, d.Data, d.ArtifactType = nil, nil, nil, ""
	e.index = append(e.index, d)
}

// manifest writes the manifest at repo@h with what it names, and returns its descriptor.
func (e *exporter) manifest(repo name.Repository, h v1.Hash) (v1.Descriptor, error) {
	if h.Algorithm != "sha256" {
		return v1.Descriptor{}, fmt.Errorf("%s@%s: only sha256 digests are archived", repo, h)
	}
	if d, ok := e.manifests[h]; ok {
		return d, nil
	}
	// A read by digest is checked against it.
	r, err := remote.Get(repo.Digest(h.String()), e.c.opts(e.ctx)...)
	if err != nil {
		return v1.Descriptor{}, fmt.Errorf("%s@%s: %w", repo, h, err)
	}
	d := v1.Descriptor{MediaType: r.MediaType, Digest: r.Digest, Size: r.Size}
	children, blobs, err := parts(r.MediaType, r.Manifest)
	if err != nil {
		return v1.Descriptor{}, fmt.Errorf("%s@%s: %w", repo, h, err)
	}
	for _, c := range children {
		if _, err := e.manifest(repo, c.Digest); err != nil {
			return v1.Descriptor{}, err
		}
	}
	for _, b := range blobs {
		if err := e.blob(repo, b); err != nil {
			return v1.Descriptor{}, err
		}
	}
	if !e.seen[h] { // the same bytes may have been written as a blob
		if err := e.file(blobDir+h.Hex, r.Manifest); err != nil {
			return v1.Descriptor{}, err
		}
	}
	e.seen[h], e.manifests[h] = true, d
	e.st.Manifests++
	return d, nil
}

// parts is what a manifest names: the manifests of an index, the config and layers of an image.
func parts(mt types.MediaType, body []byte) (children, blobs []v1.Descriptor, err error) {
	switch {
	case mt.IsIndex():
		im, err := v1.ParseIndexManifest(bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		return im.Manifests, nil, nil
	case mt.IsImage() || mt == types.OCIManifestSchema1 || mt == types.DockerManifestSchema2:
		m, err := v1.ParseManifest(bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		return nil, append([]v1.Descriptor{m.Config}, m.Layers...), nil
	}
	return nil, nil, fmt.Errorf("a manifest of type %q cannot be archived", mt)
}

// blob writes the blob d of repo, checked against its digest and size.
func (e *exporter) blob(repo name.Repository, d v1.Descriptor) error {
	if e.seen[d.Digest] {
		return nil
	}
	if d.Digest.Algorithm != "sha256" {
		return fmt.Errorf("%s@%s: only sha256 digests are archived", repo, d.Digest)
	}
	l, err := remote.Layer(repo.Digest(d.Digest.String()), e.c.opts(e.ctx)...)
	if err != nil {
		return fmt.Errorf("%s@%s: %w", repo, d.Digest, err)
	}
	rc, err := l.Compressed()
	if err != nil {
		return fmt.Errorf("%s@%s: %w", repo, d.Digest, err)
	}
	defer rc.Close()
	if err := e.tw.WriteHeader(&tar.Header{Name: blobDir + d.Digest.Hex, Typeflag: tar.TypeReg, Mode: 0o644, Size: d.Size, ModTime: epoch}); err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(e.tw, h), io.LimitReader(rc, d.Size))
	if err != nil {
		return fmt.Errorf("%s@%s: %w", repo, d.Digest, err)
	}
	if n != d.Size || hex.EncodeToString(h.Sum(nil)) != d.Digest.Hex {
		return fmt.Errorf("%s@%s: the blob read is not the one its manifest names", repo, d.Digest)
	}
	e.seen[d.Digest] = true
	e.st.Blobs++
	e.st.Bytes += n
	return nil
}

// span is where a file's content lies in the archive.
type span struct{ off, size int64 }

// ReadArchive reads an archive's top-level manifests, and where each of its blobs lies. The
// archive is read once, its blobs skipped (they are read when pushed).
func ReadArchive(file string) (*Archive, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	a := &Archive{f: f, blobs: map[string]span{}}
	if err := a.scan(); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return a, nil
}

// Archive is an archive read by ReadArchive; Close releases it.
type Archive struct {
	f *os.File
	// Manifests are the top-level manifests, in the archive's order.
	Manifests []v1.Descriptor
	blobs     map[string]span
}

// Close closes the archive's file.
func (a *Archive) Close() error { return a.f.Close() }

func (a *Archive) scan() error {
	var layout, index []byte
	tr := tar.NewReader(a.f)
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		n := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("%s is not a regular file", h.Name)
		}
		if seen[n] {
			return fmt.Errorf("%s is in the archive twice", n)
		}
		seen[n] = true
		switch {
		case n == layoutFile:
			layout, err = readSmall(tr, h.Size, n)
		case n == indexFile:
			index, err = readSmall(tr, h.Size, n)
		case strings.HasPrefix(n, blobDir):
			// The reader stops at the start of an entry's content: no read buffers past it.
			off, serr := a.f.Seek(0, io.SeekCurrent)
			a.blobs[strings.TrimPrefix(n, blobDir)], err = span{off, h.Size}, serr
		}
		if err != nil {
			return err
		}
	}
	if layout == nil || index == nil {
		return errors.New("not an OCI layout archive: no oci-layout or index.json")
	}
	var l struct {
		Version string `json:"imageLayoutVersion"`
	}
	if err := json.Unmarshal(layout, &l); err != nil || l.Version != LayoutVersion {
		return fmt.Errorf("oci-layout: version %q, not %s", l.Version, LayoutVersion)
	}
	im, err := v1.ParseIndexManifest(bytes.NewReader(index))
	if err != nil {
		return fmt.Errorf("index.json: %w", err)
	}
	a.Manifests = im.Manifests
	return nil
}

func readSmall(r io.Reader, size int64, n string) ([]byte, error) {
	if size > maxManifest {
		return nil, fmt.Errorf("%s: %d bytes, more than %d", n, size, maxManifest)
	}
	return io.ReadAll(r)
}

// open returns a reader of the blob h, which fails at its end unless the content is h.
func (a *Archive) open(h v1.Hash) (io.Reader, int64, error) {
	s, ok := a.blobs[h.Hex]
	if !ok || h.Algorithm != "sha256" {
		return nil, 0, fmt.Errorf("blob %s is not in the archive", h)
	}
	return &verifying{r: io.NewSectionReader(a.f, s.off, s.size), left: s.size, h: sha256.New(), want: h.Hex}, s.size, nil
}

// Read returns the blob h of the archive (a manifest, or a small blob), checked against h: at most
// 4 MiB.
func (a *Archive) Read(h v1.Hash) ([]byte, error) {
	r, size, err := a.open(h)
	if err != nil {
		return nil, err
	}
	if size > maxManifest {
		return nil, fmt.Errorf("manifest %s: %d bytes, more than %d", h, size, maxManifest)
	}
	return io.ReadAll(r)
}

// verifying reads a blob of left bytes, and fails on its last byte when the content is not the one
// its digest names: a reader that stops at the size, never reaching EOF, still gets the error.
type verifying struct {
	r    io.Reader
	left int64
	h    hash.Hash
	want string
}

func (v *verifying) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	v.h.Write(p[:n])
	v.left -= int64(n)
	if v.left == 0 && hex.EncodeToString(v.h.Sum(nil)) != v.want {
		return n, fmt.Errorf("blob sha256:%s: the archive's content is not the one its digest names", v.want)
	}
	return n, err
}

// archiveLayer is an archive's blob as go-containerregistry pushes it.
type archiveLayer struct {
	a *Archive
	d v1.Descriptor
}

func (l archiveLayer) Digest() (v1.Hash, error) { return l.d.Digest, nil }
func (l archiveLayer) DiffID() (v1.Hash, error) { return l.d.Digest, nil }
func (l archiveLayer) Compressed() (io.ReadCloser, error) {
	r, size, err := l.a.open(l.d.Digest)
	if err != nil {
		return nil, err
	}
	if size != l.d.Size {
		return nil, fmt.Errorf("blob %s: %d bytes in the archive, its manifest says %d", l.d.Digest, size, l.d.Size)
	}
	return io.NopCloser(r), nil
}
func (l archiveLayer) Uncompressed() (io.ReadCloser, error) { return l.Compressed() }
func (l archiveLayer) Size() (int64, error)                 { return l.d.Size, nil }
func (l archiveLayer) MediaType() (types.MediaType, error)  { return l.d.MediaType, nil }

// Import pushes every top-level manifest of a, with what it names, into its repository under
// prefix, under its tag (untagged: by digest alone); a repository allow refuses stops it before
// anything is pushed. Manifests are pushed as their bytes are, so
// every digest, and every signature over one, is the same as where they were exported from. A blob
// or manifest whose content is not its digest's is refused. logf, when set, tells each manifest
// as it starts.
func (c *Client) Import(ctx context.Context, a *Archive, prefix string, allow func(repo string) bool, logf func(string, ...any)) (ArchiveStats, error) {
	im := &importer{c: c, ctx: ctx, a: a, manifests: map[string]bool{}, blobs: map[string]bool{}}
	// Every name is checked before anything is pushed.
	repos := make([]name.Repository, len(a.Manifests))
	refs := make([]name.Reference, len(a.Manifests))
	for i, d := range a.Manifests {
		rel := d.Annotations[RepositoryAnnotation]
		if !repoPath.MatchString(rel) {
			return im.st, fmt.Errorf("manifest %s: %q is not a repository path (%s)", d.Digest, rel, RepositoryAnnotation)
		}
		if allow != nil && !allow(rel) {
			return im.st, fmt.Errorf("manifest %s goes to %s, a repository this import does not write", d.Digest, rel)
		}
		repo, err := c.Repo(prefix + "/" + rel)
		if err != nil {
			return im.st, err
		}
		repos[i], refs[i] = repo, repo.Digest(d.Digest.String())
		if t := d.Annotations[RefNameAnnotation]; t != "" {
			if refs[i], err = c.Tag(repo.String(), t); err != nil {
				return im.st, err
			}
		}
	}
	for i, d := range a.Manifests {
		repo, ref := repos[i], refs[i]
		if logf != nil {
			logf("pushing %s (%s)", ref, Short(d.Digest.String()))
		}
		if err := im.manifest(repo, d, ref); err != nil {
			return im.st, err
		}
	}
	return im.st, nil
}

type importer struct {
	c   *Client
	ctx context.Context
	a   *Archive
	// manifests and blobs pushed, by repo@digest: the same bytes may be both.
	manifests, blobs map[string]bool
	st               ArchiveStats
}

// manifest pushes the manifest d, with what it names, into repo, then puts it under ref.
func (im *importer) manifest(repo name.Repository, d v1.Descriptor, ref name.Reference) error {
	body, err := im.a.Read(d.Digest)
	if err != nil {
		return err
	}
	var head struct {
		MediaType types.MediaType `json:"mediaType"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return fmt.Errorf("manifest %s: %w", d.Digest, err)
	}
	if head.MediaType != "" && head.MediaType != d.MediaType {
		return fmt.Errorf("manifest %s is a %s, its descriptor says %s", d.Digest, head.MediaType, d.MediaType)
	}
	key := repo.String() + "@" + d.Digest.String()
	if !im.manifests[key] {
		children, blobs, err := parts(d.MediaType, body)
		if err != nil {
			return fmt.Errorf("manifest %s: %w", d.Digest, err)
		}
		for _, c := range children {
			if err := im.manifest(repo, c, repo.Digest(c.Digest.String())); err != nil {
				return err
			}
		}
		for _, b := range blobs {
			bk := repo.String() + "@" + b.Digest.String()
			if im.blobs[bk] {
				continue
			}
			if err := remote.WriteLayer(repo, archiveLayer{im.a, b}, im.c.opts(im.ctx)...); err != nil {
				return fmt.Errorf("push %s@%s: %w", repo, b.Digest, err)
			}
			im.blobs[bk] = true
			im.st.Blobs++
			im.st.Bytes += b.Size
		}
		im.st.Manifests++
	}
	if err := remote.Put(ref, rawManifest{body, d.MediaType}, im.c.opts(im.ctx)...); err != nil {
		return fmt.Errorf("push %s: %w", ref, err)
	}
	im.manifests[key] = true
	return nil
}
