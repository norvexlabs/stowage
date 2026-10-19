package registry

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// referrers serves a registry with the Referrers API.
func referrers(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0)), ggcrregistry.WithReferrersSupport(true)))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// published pushes under prefix what a release holds: an index of two images (tagged), an
// artifact (tagged) and a signature-like referrer of the index. It returns the entries to export
// and the referrer's digest.
func published(t *testing.T, c *Client, prefix string) ([]Entry, v1.Hash) {
	t.Helper()
	ctx := context.Background()
	a, _ := random.Image(64, 2)
	b, _ := random.Image(64, 1)
	idx := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: a}, mutate.IndexAddendum{Add: b})
	tag, _ := c.Tag(prefix+"/notes-api", "beta-1.0.0")
	if err := remote.WriteIndex(tag, idx, c.opts(ctx)...); err != nil {
		t.Fatal(err)
	}
	ih, _ := idx.Digest()
	rtag, _ := c.Tag(prefix+"/notes-release", "beta-1.0.0")
	rh, err := c.PutArtifact(ctx, rtag, Artifact{Type: kind, Data: []byte(`{"r":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	// A referrer: an artifact manifest whose subject is the index.
	isz, _ := idx.Size()
	imt, _ := idx.MediaType()
	sig := Manifest{ArtifactType: kind, Config: Blob{MediaType: EmptyMediaType, Data: []byte("{}")}, Layers: []Blob{{MediaType: kind, Data: []byte(`{"sig":1}`)}}}
	body, _, err := sig.Body()
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	raw["subject"] = v1.Descriptor{MediaType: imt, Digest: ih, Size: isz}
	body, _ = json.Marshal(raw)
	repo, _ := c.Repo(prefix + "/notes-api")
	for _, bl := range append([]Blob{sig.Config}, sig.Layers...) {
		if err := remote.WriteLayer(repo, bl.layer(), c.opts(ctx)...); err != nil {
			t.Fatal(err)
		}
	}
	sh, _, _ := v1.SHA256(bytes.NewReader(body))
	if err := remote.Put(repo.Digest(sh.String()), rawManifest{body, "application/vnd.oci.image.manifest.v1+json"}, c.opts(ctx)...); err != nil {
		t.Fatal(err)
	}
	return []Entry{
		{Repo: "notes-api", Tag: "beta-1.0.0", Digest: ih.String()},
		{Repo: "notes-release", Tag: "beta-1.0.0", Digest: rh.String()},
		{Repo: "notes-release", Tag: "beta", Digest: rh.String()},
	}, sh
}

func export(t *testing.T, c *Client, prefix string, entries []Entry) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "r.oci.tar")
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Export(context.Background(), prefix, entries, f, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestExportImportKeepsDigestsTagsAndReferrers(t *testing.T) {
	src, dst := referrers(t), referrers(t)
	c := New(authn.DefaultKeychain)
	ctx := context.Background()
	entries, sig := published(t, c, src+"/p")
	file := export(t, c, src+"/p", entries)
	a, err := ReadArchive(file)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// Three tagged entries and the referrer, untagged.
	if len(a.Manifests) != 4 || a.Manifests[1].Annotations[RefNameAnnotation] != "" || a.Manifests[1].Digest != sig {
		t.Fatalf("top level: %+v", a.Manifests)
	}
	st, err := c.Import(ctx, a, dst+"/site/notes", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The index, its two images, the release artifact and the referrer.
	if st.Manifests != 5 {
		t.Fatalf("stats %+v", st)
	}
	for _, e := range entries {
		ref, _ := c.Tag(dst+"/site/notes/"+e.Repo, e.Tag)
		d, err := c.Head(ctx, ref)
		if err != nil || d == nil || d.Digest.String() != e.Digest {
			t.Fatalf("%s: %v %v, want %s", ref, d, err, e.Digest)
		}
	}
	idx, _ := name.NewDigest(dst + "/site/notes/notes-api@" + entries[0].Digest)
	if img, err := remote.Index(idx); err != nil {
		t.Fatal(err)
	} else if _, err := img.IndexManifest(); err != nil {
		t.Fatal(err)
	}
	refs, err := remote.Referrers(idx)
	if err != nil {
		t.Fatal(err)
	}
	im, _ := refs.IndexManifest()
	if len(im.Manifests) != 1 || im.Manifests[0].Digest != sig {
		t.Fatalf("referrers at the mirror: %+v", im.Manifests)
	}
	// Again: everything is there, and nothing changes.
	if _, err := c.Import(ctx, a, dst+"/site/notes", nil, nil); err != nil {
		t.Fatal(err)
	}
}

// rewrite copies the archive at file, passing each entry's content through edit.
func rewrite(t *testing.T, file string, edit func(h *tar.Header, data []byte) []byte) string {
	t.Helper()
	in, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out := filepath.Join(t.TempDir(), "edited.tar")
	f, _ := os.Create(out)
	tw := tar.NewWriter(f)
	tr := tar.NewReader(in)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		data = edit(h, data)
		h.Size = int64(len(data))
		_ = tw.WriteHeader(h)
		_, _ = tw.Write(data)
	}
	_ = tw.Close()
	_ = f.Close()
	return out
}

func TestImportRefusesTamperedOrUnnamedContent(t *testing.T) {
	src := referrers(t)
	c := New(authn.DefaultKeychain)
	ctx := context.Background()
	entries, _ := published(t, c, src+"/p")
	file := export(t, c, src+"/p", entries)
	// The control: the archive as written imports.
	ok, err := ReadArchive(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Import(ctx, ok, referrers(t)+"/m", nil, nil); err != nil {
		t.Fatal(err)
	}
	ok.Close()
	for name, edit := range map[string]func(*tar.Header, []byte) []byte{
		"a layer's byte": func(h *tar.Header, d []byte) []byte {
			if strings.HasPrefix(h.Name, blobDir) && len(d) > 0 && d[0] == 0x1f { // a gzip layer
				d = bytes.Clone(d)
				d[0] ^= 1
			}
			return d
		},
		"no repository": func(h *tar.Header, d []byte) []byte {
			if h.Name == indexFile {
				return bytes.ReplaceAll(d, []byte(RepositoryAnnotation), []byte("dev.other"))
			}
			return d
		},
		"an escaping repository": func(h *tar.Header, d []byte) []byte {
			if h.Name == indexFile {
				return bytes.ReplaceAll(d, []byte(`"notes-api"`), []byte(`"../notes-api"`))
			}
			return d
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, err := ReadArchive(rewrite(t, file, edit))
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			if _, err := c.Import(ctx, a, referrers(t)+"/m", nil, nil); err == nil {
				t.Fatal("imported")
			}
		})
	}
	// The archive's own check, before any registry's: a tampered blob fails as it is read.
	tampered, err := ReadArchive(rewrite(t, file, func(h *tar.Header, d []byte) []byte {
		if strings.HasPrefix(h.Name, blobDir) && len(d) > 0 && d[0] == 0x1f {
			d = bytes.Clone(d)
			d[len(d)-1] ^= 1
		}
		return d
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer tampered.Close()
	checked := 0
	for hexd := range tampered.blobs {
		h := v1.Hash{Algorithm: "sha256", Hex: hexd}
		r, size, err := tampered.open(h)
		if err != nil {
			t.Fatal(err)
		}
		// Read as an upload reads it: to the size, never to EOF.
		if _, err := io.Copy(io.Discard, io.LimitReader(r, size)); err != nil {
			if !strings.Contains(err.Error(), "not the one its digest names") {
				t.Fatal(err)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Error("a tampered blob read without error")
	}
	if _, err := ReadArchive(rewrite(t, file, func(h *tar.Header, d []byte) []byte {
		if h.Name == layoutFile {
			return []byte(`{"imageLayoutVersion":"2.0.0"}`)
		}
		return d
	})); err == nil {
		t.Fatal("read a layout of another version")
	}
}

func TestRootCAsLeaveAGivenTransportAlone(t *testing.T) {
	t.Parallel()
	caPEM, _ := testCA(t)
	pool, _ := CertPool(caPEM)
	given := &http.Transport{}
	New(authn.DefaultKeychain, WithRootCAs(pool), WithTransport(given))
	New(authn.DefaultKeychain, WithTransport(given), WithRootCAs(pool))
	if given.TLSClientConfig != nil {
		t.Error("a given transport was changed")
	}
}

func TestImportPushesADigestThatIsBothABlobAndAManifest(t *testing.T) {
	t.Parallel()
	src, dst := referrers(t), referrers(t)
	c := New(authn.DefaultKeychain)
	ctx := context.Background()
	img, _ := random.Image(32, 1)
	raw, _ := img.RawManifest()
	ih, _ := img.Digest()
	// An artifact whose one layer is the image's manifest, byte for byte; exported first.
	at, _ := c.Tag(src+"/p/x", "a")
	ah, err := c.PutArtifact(ctx, at, Artifact{Type: kind, Data: raw})
	if err != nil {
		t.Fatal(err)
	}
	it, _ := c.Tag(src+"/p/x", "b")
	if err := c.WriteImage(ctx, it, img); err != nil {
		t.Fatal(err)
	}
	// In either order, the blob and the manifest are both pushed.
	for i, entries := range [][]Entry{
		{{Repo: "x", Tag: "a", Digest: ah.String()}, {Repo: "x", Tag: "b", Digest: ih.String()}},
		{{Repo: "x", Tag: "b", Digest: ih.String()}, {Repo: "x", Tag: "a", Digest: ah.String()}},
	} {
		a, err := ReadArchive(export(t, c, src+"/p", entries))
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		to := fmt.Sprintf("%s/m%d", dst, i)
		if _, err := c.Import(ctx, a, to, nil, nil); err != nil {
			t.Fatal(err)
		}
		art, _ := c.Tag(to+"/x", "a")
		if got, _, err := c.GetArtifact(ctx, art, kind); err != nil || got == nil || !bytes.Equal(got.Data, raw) {
			t.Fatalf("order %d: the artifact's layer: %v", i, err)
		}
	}
	dst += "/m0"
	b, _ := c.Tag(dst+"/x", "b")
	got, err := c.Image(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	ls, _ := got.Layers()
	for _, l := range ls {
		rc, err := l.Compressed()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, rc); err != nil {
			t.Fatalf("a layer of the image is missing: %v", err)
		}
		rc.Close()
	}
}

func TestImportWritesOnlyTheRepositoriesAllowed(t *testing.T) {
	t.Parallel()
	src, dst := referrers(t), referrers(t)
	c := New(authn.DefaultKeychain)
	entries, _ := published(t, c, src+"/p")
	a, err := ReadArchive(export(t, c, src+"/p", entries))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx := context.Background()
	_, err = c.Import(ctx, a, dst+"/m", func(repo string) bool { return repo != "notes-api" }, nil)
	if err == nil || !strings.Contains(err.Error(), "goes to notes-api") {
		t.Fatalf("err = %v", err)
	}
	// Nothing was pushed, not even what was allowed.
	ref, _ := c.Tag(dst+"/m/notes-release", "beta-1.0.0")
	if d, err := c.Head(ctx, ref); err != nil || d != nil {
		t.Errorf("pushed before refusing: %v %v", d, err)
	}
}

// testCA makes a CA and a server certificate it signs for the loopback.
func testCA(t *testing.T) (caPEM []byte, srv *httptest.Server) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	ca, _ = x509.ParseCertificate(der)
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	srv = httptest.NewUnstartedServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshakes
	srv.StartTLS()
	t.Cleanup(srv.Close)
	// Replace the server's own certificate with one the CA signs.
	skey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, IPAddresses: srv.TLS.Certificates[0].Leaf.IPAddresses, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if srv.TLS.Certificates[0].Leaf == nil {
		t.Fatal("no leaf")
	}
	lder, _ := x509.CreateCertificate(rand.Reader, leaf, ca, &skey.PublicKey, key)
	srv.TLS.Certificates[0].Certificate = [][]byte{lder}
	srv.TLS.Certificates[0].PrivateKey = skey
	srv.TLS.Certificates[0].Leaf, _ = x509.ParseCertificate(lder)
	return caPEM, srv
}

func TestRootCAsTrustTheMirrorsCA(t *testing.T) {
	caPEM, srv := testCA(t)
	host := strings.TrimPrefix(srv.URL, "https://")
	ctx := context.Background()
	repo := host + "/m/x"
	tag, _ := name.NewTag(repo + ":1")
	if _, err := New(authn.DefaultKeychain).Head(ctx, tag); err == nil {
		t.Fatal("trusted an unknown CA")
	}
	pool, err := CertPool(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(authn.DefaultKeychain, WithRootCAs(pool)).Head(ctx, tag); err != nil {
		t.Fatalf("with the CA: %v", err)
	}
}

func TestReadCARefusesAnythingButCertificates(t *testing.T) {
	t.Parallel()
	caPEM, _ := testCA(t)
	dir := t.TempDir()
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1}})
	for name, c := range map[string]struct {
		data []byte
		ok   bool
	}{
		"a certificate":        {caPEM, true},
		"two":                  {append(bytes.Clone(caPEM), caPEM...), true},
		"a key with it":        {append(bytes.Clone(caPEM), key...), false},
		"nothing":              {[]byte("hello"), false},
		"a broken certificate": {pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1}}), false},
		"more than the cap":    {append(bytes.Clone(caPEM), make([]byte, maxCA)...), false},
		"just under the cap":   {append(bytes.Clone(caPEM), make([]byte, maxCA-len(caPEM))...), true},
	} {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		_ = os.WriteFile(p, c.data, 0o644)
		_, err := ReadCA(p)
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
		if name == "a key with it" && (err == nil || !strings.Contains(err.Error(), "a PRIVATE KEY block: a CA file holds certificates only")) {
			t.Errorf("a key: %v", err)
		}
	}
}
