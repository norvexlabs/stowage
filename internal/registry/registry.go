// Package registry is Stowage's OCI registry client: the reads and writes ship, verify, promote,
// yank and prune need, over go-containerregistry. Every write is by tag; nothing is ever deleted
// by digest, because one digest commonly carries the tags of several channels.
package registry

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// InputsAnnotation carries a pushed artifact's inputs digest.
const InputsAnnotation = "dev.stowage.inputs"

// EmptyMediaType is the OCI empty descriptor's media type, used as an artifact's config.
const EmptyMediaType types.MediaType = "application/vnd.oci.empty.v1+json"

// ErrTooLarge is a blob refused for its size, before it is read.
var ErrTooLarge = errors.New("larger than the read allows")

// MaxArtifactBlob caps the one blob an artifact read here may hold.
const MaxArtifactBlob = 4 << 20

// maxArtifactBlob is the cap in force; tests lower it to probe both sides of the boundary.
var maxArtifactBlob int64 = MaxArtifactBlob

// Client reads and writes one registry with one set of credentials.
type Client struct {
	keychain authn.Keychain
	rt       http.RoundTripper
	roots    *x509.CertPool
	log      io.Writer // a large blob's push: its progress and its retries
}

// Option configures a Client.
type Option func(*Client)

// WithTransport replaces the HTTP transport (tests point it at an in-process registry).
func WithTransport(rt http.RoundTripper) Option { return func(c *Client) { c.rt = rt } }

// WithRootCAs makes the client trust the certificate authorities of pool, and those alone: a
// mirror's own CA together with the system's (CertPool). It applies to the client's own
// transport; one WithTransport gives is used as it is.
func WithRootCAs(pool *x509.CertPool) Option { return func(c *Client) { c.roots = pool } }

// WithLog has the client write a large blob's push progress, and its retries, to w.
func WithLog(w io.Writer) Option { return func(c *Client) { c.log = w } }

// maxCA bounds a CA file.
const maxCA = 1 << 20

// ReadCA reads a file of PEM certificates a registry's certificate chains to: one certificate at
// least, and nothing else — a private key given by mistake is refused, never copied.
func ReadCA(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxCA+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(data) > maxCA {
		return nil, fmt.Errorf("%s: more than %d bytes, not a CA file", path, maxCA)
	}
	if _, err := CertPool(data); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return data, nil
}

// CertPool is the system's certificate authorities and those of data, which must hold PEM
// certificates only.
func CertPool(data []byte) (*x509.CertPool, error) {
	n := 0
	for rest := data; ; {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("a %s block: a CA file holds certificates only", b.Type)
		}
		if _, err := x509.ParseCertificate(b.Bytes); err != nil {
			return nil, err
		}
		n++
	}
	if n == 0 {
		return nil, errors.New("no PEM certificate")
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	pool.AppendCertsFromPEM(data)
	return pool, nil
}

// New returns a client authenticating with keychain.
func New(keychain authn.Keychain, opts ...Option) *Client {
	own := http.DefaultTransport.(*http.Transport).Clone()
	own.ResponseHeaderTimeout = 2 * time.Minute
	c := &Client{keychain: keychain, rt: own}
	for _, o := range opts {
		o(c)
	}
	if c.roots != nil && c.rt == own {
		own.TLSClientConfig = &tls.Config{RootCAs: c.roots, MinVersion: tls.VersionTLS12}
	}
	c.rt = httpsOnly{c.rt}
	return c
}

// httpsOnly refuses plain HTTP to any host but the loopback. go-containerregistry falls back to
// HTTP for private (RFC 1918) and *.localhost registries when HTTPS fails, which would send the
// publisher's credentials in clear to whoever blocked port 443.
type httpsOnly struct{ next http.RoundTripper }

func (h httpsOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme == "http" && !Loopback(r.URL.Hostname()) {
		return nil, fmt.Errorf("refusing plain HTTP to %s: a registry is reached over HTTPS (only a loopback registry may use HTTP)", r.URL.Host)
	}
	return h.next.RoundTrip(r)
}

// Loopback reports whether host names this machine.
func Loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Client) opts(ctx context.Context) []remote.Option {
	return []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(c.keychain), remote.WithTransport(c.rt)}
}

// nameOpts: references Stowage builds itself always name their registry.
func (c *Client) nameOpts() []name.Option { return []name.Option{name.StrictValidation} }

// Tag parses repo:tag.
func (c *Client) Tag(repo, tag string) (name.Tag, error) {
	return name.NewTag(repo+":"+tag, c.nameOpts()...)
}

// Digest parses repo@digest.
func (c *Client) Digest(repo, digest string) (name.Digest, error) {
	return name.NewDigest(repo+"@"+digest, c.nameOpts()...)
}

// Repo parses a repository.
func (c *Client) Repo(repo string) (name.Repository, error) { return Repository(repo) }

// Repository parses a repository, which names its registry.
func Repository(repo string) (name.Repository, error) {
	return name.NewRepository(repo, name.StrictValidation)
}

// NotFound reports whether err is the registry saying the reference does not exist. Some
// registries answer 404 to a read they refuse, so a 404 with a body counts only when the body names
// an unknown manifest, repository or blob; a 404 without one (a HEAD) counts as absent.
func NotFound(err error) bool {
	var te *transport.Error
	if !errors.As(err, &te) || te.StatusCode != http.StatusNotFound {
		return false
	}
	if len(te.Errors) == 0 {
		return te.Request != nil && te.Request.Method == http.MethodHead
	}
	for _, d := range te.Errors {
		switch d.Code {
		case transport.ManifestUnknownErrorCode, transport.NameUnknownErrorCode, transport.BlobUnknownErrorCode:
		default:
			return false
		}
	}
	return true
}

// Head returns the descriptor a reference resolves to, or nil when it does not exist.
func (c *Client) Head(ctx context.Context, ref name.Reference) (*v1.Descriptor, error) {
	d, err := remote.Head(ref, c.opts(ctx)...)
	if NotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ref, err)
	}
	return d, nil
}

// Get fetches a manifest, or nil when it does not exist. Its digest is always its content's: a
// digest the registry asserts instead (go-containerregistry takes a schema 1 manifest's from a
// response header) is refused, so a signature checked on the digest covers the bytes read.
func (c *Client) Get(ctx context.Context, ref name.Reference) (*remote.Descriptor, error) {
	d, err := remote.Get(ref, c.opts(ctx)...)
	if NotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ref, err)
	}
	h, _, err := v1.SHA256(bytes.NewReader(d.Manifest))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ref, err)
	}
	if d.Digest != h {
		return nil, fmt.Errorf("%s: the registry gives the digest %s for a manifest whose content is %s", ref, d.Digest, h)
	}
	return d, nil
}

// Referrers lists the manifests that may refer to d with the artifact type t (its signatures,
// for a sigstore bundle type), from the registry's referrers API or its tag fallback. A listed
// type is a claim, not a fact: on a registry without the referrers API the fallback index is
// written by the client that pushed the referrer, and some write the type of the manifest's
// empty config in place of its artifact type, or none. So an entry listed with that type or
// none is kept too, and the caller reads each manifest's own type (GetArtifact refuses another
// with ErrNotArtifact).
func (c *Client) Referrers(ctx context.Context, d name.Digest, t types.MediaType) ([]v1.Descriptor, error) {
	idx, err := remote.Referrers(d, c.opts(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("referrers of %s: %w", d, err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("referrers of %s: %w", d, err)
	}
	var out []v1.Descriptor
	for _, m := range im.Manifests {
		if m.ArtifactType == string(t) || m.ArtifactType == string(EmptyMediaType) || m.ArtifactType == "" {
			out = append(out, m)
		}
	}
	return out, nil
}

// ErrNotArtifact marks a reference that exists but holds something other than the artifact type
// asked for: a definite answer, unlike a failed read.
var ErrNotArtifact = errors.New("not an artifact of the requested type")

// RetiredMediaType is the type of the retired artifact, which a tag is moved onto in place of its
// removal on a registry that deletes no tag alone: GetArtifact reads a tag holding it as absent.
const RetiredMediaType types.MediaType = "application/vnd.stowage.retired.v1+json"

// Retired is the retired artifact. Its one layer is "{}", so it is one manifest, RetiredDigest,
// in every repository.
func Retired() Artifact { return Artifact{Type: RetiredMediaType, Data: []byte("{}")} }

// RetiredDigest is the retired artifact's manifest digest.
func RetiredDigest() (v1.Hash, error) { return retiredDigest() }

var retiredDigest = sync.OnceValues(func() (v1.Hash, error) { return Retired().Digest() })

// Labels are the labels of the image ref names (an index: its linux/amd64 image's).
func (c *Client) Labels(ctx context.Context, ref name.Reference) (map[string]string, error) {
	img, err := remote.Image(ref, append(c.opts(ctx), remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}))...)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ref, err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("the config of %s: %w", ref, err)
	}
	return cf.Config.Labels, nil
}

// Image reads an image manifest (not an index); its layers are fetched as they are read.
func (c *Client) Image(ctx context.Context, ref name.Reference) (v1.Image, error) {
	return remote.Image(ref, c.opts(ctx)...)
}

// Annotations returns the annotations and the digest of the manifest (image or index) ref
// resolves to, from one read; ok is false when ref does not exist.
func (c *Client) Annotations(ctx context.Context, ref name.Reference) (map[string]string, v1.Hash, bool, error) {
	d, err := c.Get(ctx, ref)
	if err != nil || d == nil {
		return nil, v1.Hash{}, false, err
	}
	var m struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(d.Manifest, &m); err != nil {
		return nil, v1.Hash{}, false, fmt.Errorf("%s: manifest: %w", ref, err)
	}
	return m.Annotations, d.Digest, true, nil
}

// WriteImage pushes an image under a tag, or untagged under its digest.
func (c *Client) WriteImage(ctx context.Context, tag name.Reference, img v1.Image) error {
	if err := remote.Write(tag, img, c.opts(ctx)...); err != nil {
		return fmt.Errorf("push %s: %w", tag, err)
	}
	return nil
}

// Copy writes the manifest src resolves to (image or index, with every blob) under dst (a tag, or
// a digest for an untagged copy), which may be in another repository or registry. The digest is
// preserved. from authenticates the read when src is elsewhere (nil: this client).
func (c *Client) Copy(ctx context.Context, src name.Reference, dst name.Reference, from *Client) error {
	if from == nil {
		from = c
	}
	d, err := remote.Get(src, from.opts(ctx)...)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	if d.MediaType.IsIndex() {
		idx, err := d.ImageIndex()
		if err != nil {
			return fmt.Errorf("read %s: %w", src, err)
		}
		err = remote.WriteIndex(dst, idx, c.opts(ctx)...)
		if err != nil {
			return fmt.Errorf("push %s: %w", dst, err)
		}
		return nil
	}
	img, err := d.Image()
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	return c.WriteImage(ctx, dst, img)
}

// PlatformImage returns the manifest and config digests of ref's image for platform (an index
// resolves to that platform's image; an image is its own, and must be for that platform).
func (c *Client) PlatformImage(ctx context.Context, ref name.Digest, platform v1.Platform) (manifest, config v1.Hash, err error) {
	img, err := remote.Image(ref, append(c.opts(ctx), remote.WithPlatform(platform))...)
	if err != nil {
		return v1.Hash{}, v1.Hash{}, fmt.Errorf("read %s: %w", ref, err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return v1.Hash{}, v1.Hash{}, fmt.Errorf("read %s: %w", ref, err)
	}
	if cf.OS != platform.OS || cf.Architecture != platform.Architecture {
		return v1.Hash{}, v1.Hash{}, fmt.Errorf("%s is a %s/%s image, not %s/%s", ref, cf.OS, cf.Architecture, platform.OS, platform.Architecture)
	}
	if manifest, err = img.Digest(); err != nil {
		return v1.Hash{}, v1.Hash{}, err
	}
	config, err = img.ConfigName()
	return manifest, config, err
}

// Retag points tag at the manifest digest already holds in the same repository: no blob moves.
func (c *Client) Retag(ctx context.Context, digest name.Digest, tag name.Tag) error {
	d, err := remote.Get(digest, c.opts(ctx)...)
	if err != nil {
		return fmt.Errorf("read %s: %w", digest, err)
	}
	if err := remote.Tag(tag, d, c.opts(ctx)...); err != nil {
		return fmt.Errorf("tag %s: %w", tag, err)
	}
	return nil
}

// ErrDeleteRefused is a registry refusing a delete through the distribution API: it deletes no
// tag (only a manifest by digest), or nothing at all (a registry deleting through an API of its
// own).
var ErrDeleteRefused = errors.New("the registry refuses this delete")

// refused reports a delete the registry does not support: 405, or an UNSUPPORTED error.
func refused(err error) bool {
	var te *transport.Error
	if !errors.As(err, &te) {
		return false
	}
	if te.StatusCode == http.StatusMethodNotAllowed {
		return true
	}
	for _, d := range te.Errors {
		if d.Code == transport.UnsupportedErrorCode {
			return true
		}
	}
	return false
}

// DeleteTag removes one tag, nothing when it is absent. A registry that can only delete by digest
// refuses it with an error wrapping ErrDeleteRefused: deleting by digest would also remove every
// other tag on that manifest.
func (c *Client) DeleteTag(ctx context.Context, tag name.Tag) error {
	err := remote.Delete(tag, c.opts(ctx)...)
	switch {
	case err == nil, NotFound(err):
		return nil
	case refused(err):
		return fmt.Errorf("delete %s: %w (%v)", tag, ErrDeleteRefused, err)
	}
	return fmt.Errorf("delete %s: %w", tag, err)
}

// DeleteManifest removes a manifest, and every tag on it, by digest; nothing when it is absent. A
// registry that deletes nothing through the distribution API refuses it with an error wrapping
// ErrDeleteRefused.
func (c *Client) DeleteManifest(ctx context.Context, d name.Digest) error {
	err := remote.Delete(d, c.opts(ctx)...)
	switch {
	case err == nil, NotFound(err):
		return nil
	case refused(err):
		return fmt.Errorf("delete %s: %w (%v)", d, ErrDeleteRefused, err)
	}
	return fmt.Errorf("delete %s: %w", d, err)
}

// Tags lists a repository's tags; an unknown repository has none.
func (c *Client) Tags(ctx context.Context, repo name.Repository) ([]string, error) {
	tags, err := remote.List(repo, c.opts(ctx)...)
	if NotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", repo, err)
	}
	return tags, nil
}

// Artifact is an OCI artifact: one blob of an artifact type, with annotations.
type Artifact struct {
	Type        types.MediaType
	Data        []byte
	Annotations map[string]string
}

// rawManifest is a manifest pushed as its bytes are: its digest is theirs.
type rawManifest struct {
	body []byte
	mt   types.MediaType
}

func (r rawManifest) RawManifest() ([]byte, error)        { return r.body, nil }
func (r rawManifest) MediaType() (types.MediaType, error) { return r.mt, nil }

// PutArtifact pushes an artifact under tag and returns its manifest digest.
func (c *Client) PutArtifact(ctx context.Context, tag name.Tag, a Artifact) (v1.Hash, error) {
	return c.putArtifact(ctx, tag.Repository, a, &tag)
}

// PushArtifact pushes an artifact by digest only: no tag moves.
func (c *Client) PushArtifact(ctx context.Context, repo name.Repository, a Artifact) (name.Digest, error) {
	h, err := c.putArtifact(ctx, repo, a, nil)
	if err != nil {
		return name.Digest{}, err
	}
	return repo.Digest(h.String()), nil
}

func (c *Client) putArtifact(ctx context.Context, repo name.Repository, a Artifact, tag *name.Tag) (v1.Hash, error) {
	return c.PutManifest(ctx, repo, a.manifest(), tag)
}

func (a Artifact) manifest() Manifest {
	return Manifest{ArtifactType: a.Type, Config: Blob{MediaType: EmptyMediaType, Data: []byte("{}")}, Layers: []Blob{{MediaType: a.Type, Data: a.Data}}, Annotations: a.Annotations}
}

// Digest is the manifest digest the artifact is pushed under, the same in every repository.
func (a Artifact) Digest() (v1.Hash, error) {
	_, h, err := a.manifest().Body()
	return h, err
}

// Blob is a blob of a manifest: its media type and content, in memory or, from FileBlob, in a
// file read as it is pushed.
type Blob struct {
	MediaType types.MediaType
	Data      []byte
	// Annotations go on the blob's descriptor in the manifest.
	Annotations map[string]string
	path        string
	digest      v1.Hash
	size        int64
}

// FileBlob is the file at path as a blob. It is hashed once, here: the file must not change
// until it is pushed.
func FileBlob(mt types.MediaType, path string) (Blob, error) {
	f, err := os.Open(path)
	if err != nil {
		return Blob{}, err
	}
	defer f.Close()
	h, n, err := v1.SHA256(f)
	if err != nil {
		return Blob{}, fmt.Errorf("hash %s: %w", path, err)
	}
	return Blob{MediaType: mt, path: path, digest: h, size: n}, nil
}

// layer is the blob as go-containerregistry pushes it.
func (b Blob) layer() v1.Layer {
	if b.path == "" {
		return static.NewLayer(b.Data, b.MediaType)
	}
	return fileLayer{b}
}

// fileLayer is a file blob pushed as it is: its digest is the file's.
type fileLayer struct{ b Blob }

func (l fileLayer) Digest() (v1.Hash, error)             { return l.b.digest, nil }
func (l fileLayer) DiffID() (v1.Hash, error)             { return l.b.digest, nil }
func (l fileLayer) Compressed() (io.ReadCloser, error)   { return os.Open(l.b.path) }
func (l fileLayer) Uncompressed() (io.ReadCloser, error) { return os.Open(l.b.path) }
func (l fileLayer) Size() (int64, error)                 { return l.b.size, nil }
func (l fileLayer) MediaType() (types.MediaType, error)  { return l.b.MediaType, nil }

// Manifest is an OCI image manifest of a config blob and layers.
type Manifest struct {
	// ArtifactType is set for an artifact; empty for a manifest that declares its type through its
	// config (a Helm chart).
	ArtifactType types.MediaType
	Config       Blob
	Layers       []Blob
	Annotations  map[string]string
}

func describe(b Blob) (v1.Descriptor, error) {
	if b.path != "" {
		return v1.Descriptor{MediaType: b.MediaType, Digest: b.digest, Size: b.size, Annotations: b.Annotations}, nil
	}
	h, n, err := v1.SHA256(bytes.NewReader(b.Data))
	return v1.Descriptor{MediaType: b.MediaType, Digest: h, Size: n, Annotations: b.Annotations}, err
}

// Body is the manifest's JSON and its digest: a function of its content alone.
func (m Manifest) Body() ([]byte, v1.Hash, error) {
	cfg, err := describe(m.Config)
	if err != nil {
		return nil, v1.Hash{}, err
	}
	layers := make([]v1.Descriptor, len(m.Layers))
	for i, l := range m.Layers {
		if layers[i], err = describe(l); err != nil {
			return nil, v1.Hash{}, err
		}
	}
	body, err := json.Marshal(struct {
		SchemaVersion int               `json:"schemaVersion"`
		MediaType     types.MediaType   `json:"mediaType"`
		ArtifactType  types.MediaType   `json:"artifactType,omitempty"`
		Config        v1.Descriptor     `json:"config"`
		Layers        []v1.Descriptor   `json:"layers"`
		Annotations   map[string]string `json:"annotations,omitempty"`
	}{2, types.OCIManifestSchema1, m.ArtifactType, cfg, layers, m.Annotations})
	if err != nil {
		return nil, v1.Hash{}, err
	}
	h, _, err := v1.SHA256(bytes.NewReader(body))
	return body, h, err
}

// PutManifest pushes m's blobs, then m under tag, or by digest only when tag is nil.
func (c *Client) PutManifest(ctx context.Context, repo name.Repository, m Manifest, tag *name.Tag) (v1.Hash, error) {
	body, h, err := m.Body()
	if err != nil {
		return v1.Hash{}, err
	}
	for _, b := range append([]Blob{m.Config}, m.Layers...) {
		push := func() error { return remote.WriteLayer(repo, b.layer(), c.opts(ctx)...) }
		if b.path != "" {
			push = func() error { return c.pushFile(ctx, repo, b) }
		}
		if err := push(); err != nil {
			return v1.Hash{}, fmt.Errorf("push %s: %w", repo, err)
		}
	}
	var ref name.Reference = repo.Digest(h.String())
	if tag != nil {
		ref = *tag
	}
	if err := remote.Put(ref, rawManifest{body, types.OCIManifestSchema1}, c.opts(ctx)...); err != nil {
		return v1.Hash{}, fmt.Errorf("push %s: %w", ref, err)
	}
	return h, nil
}

// GetArtifact reads an artifact of type want. It returns nil when ref does not exist or is a tag
// holding the retired artifact, and an error when ref holds anything else.
func (c *Client) GetArtifact(ctx context.Context, ref name.Reference, want types.MediaType) (*Artifact, v1.Hash, error) {
	return c.GetArtifactMax(ctx, ref, want, maxArtifactBlob)
}

// GetArtifactMax is GetArtifact with its blob capped at limit bytes rather than MaxArtifactBlob:
// an attestation's document runs larger than a signature.
func (c *Client) GetArtifactMax(ctx context.Context, ref name.Reference, want types.MediaType, limit int64) (*Artifact, v1.Hash, error) {
	d, err := c.Get(ctx, ref)
	if err != nil || d == nil {
		return nil, v1.Hash{}, err
	}
	if _, tag := ref.(name.Tag); tag && want != RetiredMediaType {
		r, err := RetiredDigest()
		if err != nil {
			return nil, v1.Hash{}, fmt.Errorf("retired artifact: %w", err)
		}
		if d.Digest == r {
			return nil, v1.Hash{}, nil
		}
	}
	var m struct {
		ArtifactType types.MediaType   `json:"artifactType"`
		Layers       []v1.Descriptor   `json:"layers"`
		Annotations  map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(d.Manifest, &m); err != nil {
		return nil, v1.Hash{}, fmt.Errorf("%s: manifest: %w: %w", ref, ErrNotArtifact, err)
	}
	if m.ArtifactType != want || len(m.Layers) != 1 || m.Layers[0].MediaType != want {
		return nil, v1.Hash{}, fmt.Errorf("%s is not a %s artifact: %w", ref, want, ErrNotArtifact)
	}
	data, err := c.readLayer(ctx, ref, m.Layers[0], limit)
	if err != nil {
		return nil, v1.Hash{}, err
	}
	return &Artifact{Type: want, Data: data, Annotations: m.Annotations}, d.Digest, nil
}

// readLayer reads the blob a layer descriptor of ref's manifest names, refused when it is larger
// than max, and checks it is the size and digest the descriptor gives.
func (c *Client) readLayer(ctx context.Context, ref name.Reference, l v1.Descriptor, max int64) ([]byte, error) {
	if l.Size > max {
		return nil, fmt.Errorf("%s: blob of %d bytes exceeds %d: %w", ref, l.Size, max, ErrTooLarge)
	}
	rl, err := remote.Layer(ref.Context().Digest(l.Digest.String()), c.opts(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ref, err)
	}
	rc, err := rl.Compressed()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ref, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ref, err)
	}
	if int64(len(data)) != l.Size {
		return nil, fmt.Errorf("%s: blob size %d, manifest says %d", ref, len(data), l.Size)
	}
	if h, _, err := v1.SHA256(bytes.NewReader(data)); err != nil || h != l.Digest {
		return nil, fmt.Errorf("%s: blob digest mismatch", ref)
	}
	return data, nil
}

// GetLayer reads the one layer of the manifest at d, which must have a config of type config and
// a single layer of type layer no larger than max: the manifest is checked against d's digest (by
// the read itself), the layer against the manifest. It returns nil when d does not exist.
func (c *Client) GetLayer(ctx context.Context, d name.Digest, config, layer types.MediaType, max int64) ([]byte, error) {
	desc, err := c.Get(ctx, d)
	if err != nil || desc == nil {
		return nil, err
	}
	var m struct {
		Config v1.Descriptor   `json:"config"`
		Layers []v1.Descriptor `json:"layers"`
	}
	if err := json.Unmarshal(desc.Manifest, &m); err != nil {
		return nil, fmt.Errorf("%s: manifest: %w", d, err)
	}
	if m.Config.MediaType != config || len(m.Layers) != 1 || m.Layers[0].MediaType != layer {
		return nil, fmt.Errorf("%s is not a %s manifest with one %s layer", d, config, layer)
	}
	return c.readLayer(ctx, d, m.Layers[0], max)
}

// Short is a digest cut for display: its algorithm and 12 hex digits.
func Short(d string) string {
	if len(d) > len("sha256:")+12 {
		return d[:len("sha256:")+12]
	}
	return d
}
