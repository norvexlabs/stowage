package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/norvexlabs/stowage/internal/spec"
)

const kind = "application/vnd.example.test.v1+json"

func serve(t *testing.T, wrap func(http.Handler) http.Handler) string {
	t.Helper()
	var h http.Handler = ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0)))
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestArtifactRoundTripAndMisses(t *testing.T) {
	host := serve(t, nil)
	c := New(authn.DefaultKeychain)
	ctx := context.Background()
	tag, _ := c.Tag(host+"/p/rel", "1")
	d, err := c.PutArtifact(ctx, tag, Artifact{Type: kind, Data: []byte(`{"a":1}`), Annotations: map[string]string{InputsAnnotation: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	a, got, err := c.GetArtifact(ctx, tag, kind)
	if err != nil || a == nil {
		t.Fatalf("%v %v", a, err)
	}
	if got != d || string(a.Data) != `{"a":1}` || a.Annotations[InputsAnnotation] != "x" {
		t.Fatalf("round trip: %s %s %q %v", got, d, a.Data, a.Annotations)
	}
	if _, _, err := c.GetArtifact(ctx, tag, "application/other"); err == nil {
		t.Fatal("read an artifact of another type")
	}
	missing, _ := c.Tag(host+"/p/rel", "2")
	if a, _, err := c.GetArtifact(ctx, missing, kind); a != nil || err != nil {
		t.Fatalf("missing tag: %v %v", a, err)
	}
	img, _ := random.Image(32, 1)
	it, _ := c.Tag(host+"/p/rel", "img")
	if err := c.WriteImage(ctx, it, img); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.GetArtifact(ctx, it, kind); err == nil {
		t.Fatal("read an image as an artifact")
	}
	unknown, _ := c.Repo(host + "/nobody/here")
	if tags, err := c.Tags(ctx, unknown); err != nil || len(tags) != 0 {
		t.Fatalf("unknown repo: %v %v", tags, err)
	}
	if err := c.DeleteTag(ctx, missing); err != nil {
		t.Fatalf("deleting a missing tag: %v", err)
	}
}

// On a registry without the referrers API the fallback index is the pushing client's, and an
// entry may carry its manifest's config type in place of its artifact type: such an entry is
// listed, and the manifest's own type decides.
func TestReferrersOfAFallbackIndexAreReadByTheManifestsOwnType(t *testing.T) {
	host := serve(t, nil) // no referrers API: the tag fallback
	c := New(authn.DefaultKeychain)
	ctx := context.Background()
	repo, _ := c.Repo(host + "/p/app")
	img, _ := random.Image(32, 1)
	it, _ := c.Tag(host+"/p/app", "1")
	if err := c.WriteImage(ctx, it, img); err != nil {
		t.Fatal(err)
	}
	subject, _ := img.Digest()
	push := func(typ types.MediaType, n string) name.Digest {
		d, err := c.PushArtifact(ctx, repo, Artifact{Type: typ, Data: []byte(`{"n":"` + n + `"}`)})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	listed := push(kind, "listed")  // listed with its own type
	configT := push(kind, "config") // listed with its config's type
	untyped := push(kind, "none")   // listed with no type
	other := push("application/other", "other")
	posing := push("application/other", "posing") // another type, listed with the config's type
	entry := func(d name.Digest, at types.MediaType) map[string]any {
		return map[string]any{"mediaType": types.OCIManifestSchema1, "digest": d.DigestStr(), "size": 1, "artifactType": at}
	}
	body, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": types.OCIImageIndex, "manifests": []any{
		entry(listed, kind), entry(configT, EmptyMediaType), entry(untyped, ""), entry(other, "application/other"), entry(posing, EmptyMediaType),
	}})
	fallback, _ := c.Tag(host+"/p/app", strings.Replace(subject.String(), ":", "-", 1))
	if err := remote.Put(fallback, rawManifest{body, types.OCIImageIndex}, c.opts(ctx)...); err != nil {
		t.Fatal(err)
	}
	refs, err := c.Referrers(ctx, repo.Digest(subject.String()), kind)
	if err != nil {
		t.Fatal(err)
	}
	var read []string
	for _, r := range refs {
		a, _, err := c.GetArtifact(ctx, repo.Digest(r.Digest.String()), kind)
		switch {
		case errors.Is(err, ErrNotArtifact):
		case err != nil || a == nil:
			t.Fatalf("%s: %v %v", r.Digest, a, err)
		default:
			read = append(read, "sha256:"+r.Digest.Hex)
		}
	}
	want := listed.DigestStr() + " " + configT.DigestStr() + " " + untyped.DigestStr()
	if listed == configT || configT == untyped || len(refs) != 4 || strings.Join(read, " ") != want {
		t.Fatalf("listed %d, read %v; want 4 listed and %s read", len(refs), read, want)
	}
}

// A blob served with other bytes than its digest is refused.
func TestArtifactBlobTamperingIsRefused(t *testing.T) {
	tamper := false
	host := serve(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tamper && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/") {
				_, _ = w.Write([]byte(`{"a":2}`))
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	c := New(authn.DefaultKeychain)
	ctx := context.Background()
	tag, _ := c.Tag(host+"/p/rel", "1")
	if _, err := c.PutArtifact(ctx, tag, Artifact{Type: kind, Data: []byte(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.GetArtifact(ctx, tag, kind); err != nil {
		t.Fatalf("positive control: %v", err)
	}
	tamper = true
	if _, _, err := c.GetArtifact(ctx, tag, kind); err == nil {
		t.Fatal("tampered blob accepted")
	}
}

// A manifest whose digest the registry asserts in a header (go-containerregistry trusts it for a
// schema 1 media type) is refused unless it is its content's: a signature checked on the digest
// must cover the bytes read.
func TestADigestNotTheContentsIsRefused(t *testing.T) {
	var swap atomic.Pointer[[2]string] // tag 1 served with tag 2's body under tag 1's digest
	host := serve(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if sw := swap.Load(); sw != nil && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifests/1") {
				w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v1+prettyjws")
				w.Header().Set("Docker-Content-Digest", sw[0])
				_, _ = w.Write([]byte(sw[1]))
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	c := New(authn.DefaultKeychain)
	ctx := context.Background()
	one, _ := c.Tag(host+"/p/rel", "1")
	two, _ := c.Tag(host+"/p/rel", "2")
	d1, err := c.PutArtifact(ctx, one, Artifact{Type: kind, Data: []byte(`{"a":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PutArtifact(ctx, two, Artifact{Type: kind, Data: []byte(`{"a":2}`)}); err != nil {
		t.Fatal(err)
	}
	if _, got, err := c.GetArtifact(ctx, one, kind); err != nil || got != d1 {
		t.Fatalf("positive control: %s %v", got, err)
	}
	d, _ := c.Get(ctx, two)
	swap.Store(&[2]string{d1.String(), string(d.Manifest)})
	if _, got, err := c.GetArtifact(ctx, one, kind); err == nil || !strings.Contains(err.Error(), "whose content is") {
		t.Fatalf("another manifest accepted under %s: %v", got, err)
	}
}

func TestRetagAndDeleteTagKeepOtherTags(t *testing.T) {
	host := serve(t, nil)
	c := New(authn.DefaultKeychain)
	ctx := context.Background()
	img, _ := random.Image(32, 1)
	a, _ := c.Tag(host+"/p/img", "a")
	b, _ := c.Tag(host+"/p/img", "b")
	if err := c.WriteImage(ctx, a, img); err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()
	dg, _ := c.Digest(host+"/p/img", d.String())
	if err := c.Retag(ctx, dg, b); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteTag(ctx, a); err != nil {
		t.Fatal(err)
	}
	if h, _ := c.Head(ctx, a); h != nil {
		t.Fatal("tag a survived")
	}
	if h, _ := c.Head(ctx, b); h == nil || h.Digest != d {
		t.Fatal("deleting tag a removed tag b")
	}
}

func TestKeychain(t *testing.T) {
	env := map[string]string{"PUSH": "s3cret"}
	get := func(k string) string { return env[k] }
	tok := spec.Provider{Name: "token", Config: map[string]any{"user": "publisher", "token_env": "PUSH"}}
	kc, err := Keychain(tok, get)
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := name.NewRegistry("registry.example")
	a, _ := kc.Resolve(reg)
	cfg, _ := a.Authorization()
	if cfg.Username != "publisher" || cfg.Password != "s3cret" {
		t.Fatalf("token auth: %+v", cfg)
	}
	delete(env, "PUSH")
	if _, err := Keychain(tok, get); err == nil || !strings.Contains(err.Error(), "PUSH is not set") {
		t.Fatalf("unset token: %v", err)
	}
	none, err := Keychain(spec.Provider{Name: "none"}, get)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := none.Resolve(reg); a != authn.Anonymous {
		t.Fatal("none is not anonymous")
	}
	dir := t.TempDir()
	if err := writeFile(dir+"/config.json", `{"auths":{"registry.example":{"auth":"dTpw"}}}`); err != nil {
		t.Fatal(err)
	}
	dc, err := Keychain(spec.Provider{Name: "docker-config", Config: map[string]any{"config": dir}}, get)
	if err != nil {
		t.Fatal(err)
	}
	a, err = dc.Resolve(reg)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ = a.Authorization()
	if cfg.Username != "u" || cfg.Password != "p" {
		t.Fatalf("docker-config auth: %+v", cfg)
	}
	other, _ := name.NewRegistry("elsewhere.example")
	if a, err := dc.Resolve(other); err != nil || a != authn.Anonymous {
		t.Fatalf("docker-config without an entry: %v %v, want anonymous", a, err)
	}
	if err := writeFile(dir+"/config.json", `{"auths":{"https://index.docker.io/v1/":{"auth":"dTpw"}}}`); err != nil {
		t.Fatal(err)
	}
	hub, _ := name.NewRegistry("docker.io")
	a, err = dc.Resolve(hub)
	if err != nil {
		t.Fatal(err)
	}
	if cfg, _ = a.Authorization(); cfg.Username != "u" {
		t.Fatalf("docker hub credentials from docker login not found: %+v", cfg)
	}
}

func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0o600) }

func TestArtifactSizeCapBothSides(t *testing.T) {
	host := serve(t, nil)
	c := New(authn.DefaultKeychain)
	ctx := context.Background()
	maxArtifactBlob = 8
	t.Cleanup(func() { maxArtifactBlob = MaxArtifactBlob })
	at, _ := c.Tag(host+"/p/rel", "at")
	over, _ := c.Tag(host+"/p/rel", "over")
	if _, err := c.PutArtifact(ctx, at, Artifact{Type: kind, Data: []byte("12345678")}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PutArtifact(ctx, over, Artifact{Type: kind, Data: []byte("123456789")}); err != nil {
		t.Fatal(err)
	}
	if a, _, err := c.GetArtifact(ctx, at, kind); err != nil || a == nil {
		t.Fatalf("a blob at the cap was refused: %v", err)
	}
	if _, _, err := c.GetArtifact(ctx, over, kind); err == nil || !strings.Contains(err.Error(), "exceeds 8") {
		t.Fatalf("a blob over the cap: %v", err)
	}
}

func TestPlainHTTPOnlyToTheLoopback(t *testing.T) {
	var reached []string
	next := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reached = append(reached, r.URL.String())
		return &http.Response{StatusCode: 200, Body: http.NoBody, Request: r}, nil
	})
	c := New(authn.DefaultKeychain, WithTransport(next))
	for _, u := range []string{"http://127.0.0.1:5000/v2/", "http://localhost:5000/v2/", "http://[::1]:5000/v2/", "https://10.0.0.5:5000/v2/"} {
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		if _, err := c.rt.RoundTrip(req); err != nil {
			t.Errorf("%s refused: %v", u, err)
		}
	}
	for _, u := range []string{"http://10.0.0.5:5000/v2/", "http://registry.local/v2/", "http://registry.example/v2/"} {
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		if _, err := c.rt.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "refusing plain HTTP") {
			t.Errorf("%s: %v", u, err)
		}
	}
	if len(reached) != 4 {
		t.Fatalf("reached %v", reached)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNotFoundOnlyForAnAbsentReference(t *testing.T) {
	head := &http.Request{Method: http.MethodHead}
	get := &http.Request{Method: http.MethodGet}
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&transport.Error{StatusCode: 404, Request: head}, true},
		{&transport.Error{StatusCode: 404, Request: get, Errors: []transport.Diagnostic{{Code: transport.ManifestUnknownErrorCode}}}, true},
		{&transport.Error{StatusCode: 404, Request: get, Errors: []transport.Diagnostic{{Code: transport.NameUnknownErrorCode}}}, true},
		{&transport.Error{StatusCode: 404, Request: get, Errors: []transport.Diagnostic{{Code: transport.DeniedErrorCode}}}, false},
		{&transport.Error{StatusCode: 404, Request: get}, false},
		{&transport.Error{StatusCode: 401, Request: head}, false},
		{errors.New("boom"), false},
	} {
		if got := NotFound(c.err); got != c.want {
			t.Errorf("%v: got %v", c.err, got)
		}
	}
}

func TestPlatformImageRefusesAnotherPlatform(t *testing.T) {
	host := serve(t, nil)
	c := New(authn.DefaultKeychain)
	ctx := context.Background()
	for arch, ok := range map[string]bool{"amd64": true, "arm64": false} {
		img, _ := random.Image(32, 1)
		cf, _ := img.ConfigFile()
		cf = cf.DeepCopy()
		cf.OS, cf.Architecture = "linux", arch
		img, _ = mutate.ConfigFile(img, cf)
		tag, _ := c.Tag(host+"/p/img", arch)
		if err := c.WriteImage(ctx, tag, img); err != nil {
			t.Fatal(err)
		}
		d, _ := img.Digest()
		ref, _ := c.Digest(host+"/p/img", d.String())
		m, cfg, err := c.PlatformImage(ctx, ref, v1.Platform{OS: "linux", Architecture: "amd64"})
		want, _ := img.ConfigName()
		if ok && (err != nil || m != d || cfg != want) {
			t.Fatalf("amd64: %v %s %s", err, m, cfg)
		}
		if !ok && (err == nil || !strings.Contains(err.Error(), "linux/arm64 image, not linux/amd64")) {
			t.Fatalf("arm64 accepted as amd64: %v", err)
		}
	}
}

func TestGetLayerReadsTheOneLayerOfAManifestByDigest(t *testing.T) {
	var lie []byte // the manifest served for any manifest read, when set
	host := serve(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if lie != nil && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/manifests/") {
				w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
				_, _ = w.Write(lie)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	c := New(authn.DefaultKeychain)
	ctx := context.Background()
	repo, _ := c.Repo(host + "/p/charts/x")
	put := func(m Manifest) name.Digest {
		t.Helper()
		h, err := c.PutManifest(ctx, repo, m, nil)
		if err != nil {
			t.Fatal(err)
		}
		return repo.Digest(h.String())
	}
	cfg, content := Blob{MediaType: "cfg", Data: []byte("{}")}, Blob{MediaType: "tgz", Data: []byte("12345678")}
	ok := put(Manifest{Config: cfg, Layers: []Blob{content}})
	if data, err := c.GetLayer(ctx, ok, "cfg", "tgz", 8); err != nil || string(data) != "12345678" {
		t.Fatalf("at the cap: %q %v", data, err)
	}
	if _, err := c.GetLayer(ctx, ok, "cfg", "tgz", 7); err == nil || !strings.Contains(err.Error(), "exceeds 7") {
		t.Errorf("over the cap: %v", err)
	}
	for _, m := range []Manifest{
		{Config: Blob{MediaType: "other", Data: []byte("{}")}, Layers: []Blob{content}},
		{Config: cfg, Layers: []Blob{content, {MediaType: "tgz", Data: []byte("2")}}},
		{Config: cfg, Layers: []Blob{{MediaType: "other", Data: []byte("1")}}},
		{Config: cfg},
	} {
		if _, err := c.GetLayer(ctx, put(m), "cfg", "tgz", 8); err == nil || !strings.Contains(err.Error(), "is not a cfg manifest with one tgz layer") {
			t.Errorf("%+v: %v", m, err)
		}
	}
	missing := repo.Digest("sha256:" + strings.Repeat("0", 64))
	if data, err := c.GetLayer(ctx, missing, "cfg", "tgz", 8); data != nil || err != nil {
		t.Errorf("missing: %q %v", data, err)
	}
	// Another manifest served for the digest asked: refused, though it is well formed.
	other := put(Manifest{Config: cfg, Layers: []Blob{{MediaType: "tgz", Data: []byte("other")}}})
	d, err := c.Get(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	lie = d.Manifest
	if data, err := c.GetLayer(ctx, ok, "cfg", "tgz", 8); err == nil {
		t.Errorf("another manifest accepted: %q", data)
	}
}

// pushDisk pushes an artifact of one layer data, of type kind, titled title.
func pushDisk(t *testing.T, c *Client, host, title string, data []byte) name.Digest {
	t.Helper()
	repo, _ := c.Repo(host + "/p/disk")
	h, err := c.PutManifest(context.Background(), repo, Manifest{ArtifactType: kind, Config: Blob{MediaType: EmptyMediaType, Data: []byte("{}")},
		Layers: []Blob{{MediaType: kind, Data: data, Annotations: map[string]string{TitleAnnotation: title}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return repo.Digest(h.String())
}

func TestDownloadWritesTheCheckedLayerUnderItsTitle(t *testing.T) {
	var tamper atomic.Bool
	var blobs atomic.Int32
	host := serve(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/") {
				blobs.Add(1)
			}
			if tamper.Load() && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/") {
				_, _ = io.WriteString(w, "a tampered disk!")
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	c := New(authn.DefaultKeychain)
	ctx := context.Background()
	dir := t.TempDir()
	d := pushDisk(t, c, host, "atlas.qcow2", []byte("a qcow2 disk  ..."))
	var seen int64
	p, layer, err := c.Download(ctx, d, kind, kind, dir, "fallback", func(done, total int64) { seen = done })
	if err != nil || p != filepath.Join(dir, "atlas.qcow2") || seen != 17 {
		t.Fatalf("%s %v (progress %d)", p, err, seen)
	}
	if b, _ := os.ReadFile(p); string(b) != "a qcow2 disk  ..." {
		t.Errorf("wrote %q", b)
	}
	if sum := sha256.Sum256([]byte("a qcow2 disk  ...")); layer.String() != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Errorf("layer %s is not the file's digest", layer)
	}
	// Fetched again, the file holding the layer is kept: nothing is read but the manifest.
	blobs.Store(0)
	if again, l2, err := c.Download(ctx, d, kind, kind, dir, "fallback", nil); err != nil || again != p || l2 != layer || blobs.Load() != 0 {
		t.Errorf("again: %s %v, %d blob reads", again, err, blobs.Load())
	}
	// A title that is not a plain file name is not taken.
	for _, title := range []string{"../atlas.qcow2", "/etc/x", "..", ""} {
		bad := pushDisk(t, c, host, title, []byte("disk "+title))
		if p, _, err := c.Download(ctx, bad, kind, kind, dir, "fallback-"+strings.Repeat("x", len(title)), nil); err != nil || filepath.Dir(p) != dir || !strings.HasPrefix(filepath.Base(p), "fallback") {
			t.Errorf("title %q: %s %v", title, p, err)
		}
	}
	if _, _, err := c.Download(ctx, d, "application/other", kind, dir, "f", nil); err == nil {
		t.Error("an artifact of another type was taken")
	}
	missing, _ := c.Digest(host+"/p/disk", "sha256:"+strings.Repeat("0", 64))
	if _, _, err := c.Download(ctx, missing, kind, kind, dir, "f", nil); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing: %v", err)
	}
	// A blob that is not the layer's digest is refused, and leaves no file.
	other := pushDisk(t, c, host, "other.qcow2", []byte("an honest disk!!"))
	tamper.Store(true)
	if _, _, err := c.Download(ctx, other, kind, kind, dir, "f", nil); err == nil {
		t.Error("a tampered blob was taken")
	}
	tamper.Store(false)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "other") || strings.HasPrefix(e.Name(), ".other") {
			t.Errorf("left %s", e.Name())
		}
	}
}

func TestDownloadDecompressesAZstdLayerAndChecksItsFile(t *testing.T) {
	t.Parallel()
	var blobs atomic.Int32
	host := serve(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/") {
				blobs.Add(1)
			}
			h.ServeHTTP(w, r)
		})
	})
	c := New(authn.DefaultKeychain)
	ctx := context.Background()
	src, dir := t.TempDir(), t.TempDir()
	data := bytes.Repeat([]byte("a raw disk, mostly zeros\x00\x00\x00\x00"), 1<<15)
	path := filepath.Join(src, "atlas.raw")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := ZstdFileBlob(kind, path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	zst, _ := os.ReadFile(path + ".zst")
	if zsum := sha256.Sum256(zst); b.digest.String() != "sha256:"+hex.EncodeToString(zsum[:]) || b.size != int64(len(zst)) || b.path != path+".zst" {
		t.Errorf("blob %s, %d bytes: not the compressed file's", b.digest, b.size)
	}
	if b.MediaType != kind+ZstdSuffix || b.Annotations[TitleAnnotation] != "atlas.raw.zst" || b.Annotations[UncompressedAnnotation] != "sha256:"+hex.EncodeToString(sum[:]) || b.size >= int64(len(data))/10 {
		t.Fatalf("blob %s %v, %d bytes of %d", b.MediaType, b.Annotations, b.size, len(data))
	}
	repo, _ := c.Repo(host + "/p/disk")
	push := func(ann map[string]string) name.Digest {
		t.Helper()
		l := b
		l.Annotations = ann
		h, err := c.PutManifest(ctx, repo, Manifest{ArtifactType: kind, Config: Blob{MediaType: EmptyMediaType, Data: []byte("{}")}, Layers: []Blob{l}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return repo.Digest(h.String())
	}
	d := push(b.Annotations)
	p, h, err := c.Download(ctx, d, kind, kind, dir, "fallback", nil)
	if err != nil || p != filepath.Join(dir, "atlas.raw") || h.String() != b.Annotations[UncompressedAnnotation] {
		t.Fatalf("%s %s %v", p, h, err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, data) {
		t.Error("not the file decompressed")
	}
	// Fetched again, the decompressed file is kept: no blob is read.
	blobs.Store(0)
	if again, h2, err := c.Download(ctx, d, kind, kind, dir, "fallback", nil); err != nil || again != p || h2 != h || blobs.Load() != 0 {
		t.Errorf("again: %s %v, %d blob reads", again, err, blobs.Load())
	}
	// A file digest the decompressed bytes are not, or none, is refused, and leaves no file.
	other := map[string]string{TitleAnnotation: "other.raw.zst", UncompressedAnnotation: "sha256:" + strings.Repeat("0", 64)}
	if _, _, err := c.Download(ctx, push(other), kind, kind, dir, "f", nil); err == nil || !strings.Contains(err.Error(), "decompressed, the layer is") {
		t.Errorf("a wrong file digest: %v", err)
	}
	if _, _, err := c.Download(ctx, push(map[string]string{TitleAnnotation: "none.raw.zst"}), kind, kind, dir, "f", nil); err == nil || !strings.Contains(err.Error(), UncompressedAnnotation) {
		t.Errorf("no file digest: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("left %v", entries)
	}
}

// A frame asking for zstd's own window limit (128 MiB) is decoded; one asking for more is refused.
func TestZstdWindowBothSidesOfTheCap(t *testing.T) {
	t.Parallel()
	// A frame of one raw block, "a disk", its header asking for a window of 1<<(10+exponent) bytes.
	frame := func(exponent byte) []byte {
		return append([]byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, exponent << 3, 6<<3 | 1, 0, 0}, "a disk"...)
	}
	for _, c := range []struct {
		exponent byte
		ok       bool
	}{{17, true}, {18, false}} {
		if 1<<(10+c.exponent) > zstdMaxWindow == c.ok {
			t.Fatalf("exponent %d is not on its side of the cap", c.exponent)
		}
		dec, err := unzstd(bytes.NewReader(frame(c.exponent)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(dec)
		dec.Close()
		if (err == nil && string(got) == "a disk") != c.ok {
			t.Errorf("window 1<<%d: %q %v", 10+c.exponent, got, err)
		}
	}
}
