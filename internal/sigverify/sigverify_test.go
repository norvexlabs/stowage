package sigverify

import (
	"context"
	"crypto/elliptic"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// env is an in-process registry holding one image. Its referrers are read through the tag
// fallback: the in-process registry's referrers API reports a referrer's config media type as its
// artifact type, where the OCI spec (and zot, which the end-to-end suites run) give the manifest's.
type env struct {
	c *registry.Client
	d name.Digest
}

func newEnv(t *testing.T) *env {
	t.Helper()
	srv := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	return &env{c: registry.New(authn.DefaultKeychain), d: push(t, host+"/acme/notes")}
}

func push(t *testing.T, repo string) name.Digest {
	t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	r, err := name.NewRepository(repo)
	if err != nil {
		t.Fatal(err)
	}
	h, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	d := r.Digest(h.String())
	if err := remote.Write(d, img); err != nil {
		t.Fatal(err)
	}
	return d
}

func verifier(t *testing.T, pems ...[]byte) *Verifier {
	t.Helper()
	m := map[string][]byte{}
	for i, p := range pems {
		m[string(rune('a'+i))+".pub"] = p
	}
	v, err := New(m)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerify(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	trusted, other := testutil.NewKeySigner(t), testutil.NewKeySigner(t)
	ctx := context.Background()
	if err := verifier(t, trusted.PEM).Verify(ctx, e.c, e.d); !errors.Is(err, ErrUnsigned) || !strings.Contains(err.Error(), "no signature") {
		t.Fatalf("unsigned: %v", err)
	}
	other.Sign(t, e.d)
	if err := verifier(t, trusted.PEM).Verify(ctx, e.c, e.d); !errors.Is(err, ErrUnsigned) || !strings.Contains(err.Error(), "1 signature(s), none by a trusted key") {
		t.Fatalf("signed by another key: %v", err)
	}
	// A signature by the trusted key over another digest, pushed as a referrer of this one.
	elsewhere := push(t, e.d.Context().String())
	trusted.SignAs(t, e.d, elsewhere)
	if err := verifier(t, trusted.PEM).Verify(ctx, e.c, e.d); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("a signature of another digest: %v", err)
	}
	// Any listed key verifies (rotation lists old and new).
	if err := verifier(t, trusted.PEM, other.PEM).Verify(ctx, e.c, e.d); err != nil {
		t.Fatalf("other key listed: %v", err)
	}
	// A referrer of the bundle type that holds no bundle signs nothing, and does not keep a
	// signature beside it from verifying.
	testutil.PushReferrerAs(t, e.d, BundleType, "application/json", []byte("{}"))
	if err := verifier(t, trusted.PEM).Verify(ctx, e.c, e.d); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("a referrer that is not a bundle: %v", err)
	}
	trusted.Sign(t, e.d)
	if err := verifier(t, trusted.PEM).Verify(ctx, e.c, e.d); err != nil {
		t.Fatalf("signed: %v", err)
	}
}

// A key on another curve verifies with the hash cosign signs with for it; an attestation the
// trusted key made over the digest is no signature.
func TestVerifyKeyTypesAndPredicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, c := range []elliptic.Curve{elliptic.P384(), elliptic.P521()} {
		e := newEnv(t)
		k := testutil.NewKeySignerOn(t, c)
		k.Sign(t, e.d)
		if err := verifier(t, k.PEM).Verify(ctx, e.c, e.d); err != nil {
			t.Errorf("%s: %v", c.Params().Name, err)
		}
	}
	e := newEnv(t)
	k := testutil.NewKeySigner(t)
	k.Predicate = "https://cosign.sigstore.dev/attestation/vuln/v1"
	k.Sign(t, e.d)
	if err := verifier(t, k.PEM).Verify(ctx, e.c, e.d); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("an attestation: %v", err)
	}
}

// A bundle cosign v3 wrote (`cosign sign --key … --tlog-upload=false`) verifies with its key, and
// with no other.
func TestVerifiesCosignBundles(t *testing.T) {
	t.Parallel()
	read := func(f string) []byte {
		b, err := os.ReadFile("testdata/" + f)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	h, err := v1.NewHash(strings.TrimSpace(string(read("cosign-digest"))))
	if err != nil {
		t.Fatal(err)
	}
	r := fixed{bundle: read("cosign-bundle.json")}
	d, _ := name.NewDigest("registry.example/t/img@" + h.String())
	if err := verifier(t, read("cosign.pub")).Verify(context.Background(), r, d); err != nil {
		t.Fatal(err)
	}
	if err := verifier(t, testutil.NewKeySigner(t).PEM).Verify(context.Background(), r, d); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("another key: %v", err)
	}
	h.Hex = strings.Repeat("0", 64)
	d, _ = name.NewDigest("registry.example/t/img@" + h.String())
	if err := verifier(t, read("cosign.pub")).Verify(context.Background(), r, d); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("another digest: %v", err)
	}
}

// fixed lists one referrer holding bundle.
type fixed struct {
	bundle []byte
	err    error
	n      int
}

func (f fixed) Referrers(_ context.Context, d name.Digest, _ types.MediaType) ([]v1.Descriptor, error) {
	if f.err != nil {
		return nil, f.err
	}
	return make([]v1.Descriptor, max(f.n, 1)), nil
}

func (f fixed) GetArtifact(context.Context, name.Reference, types.MediaType) (*registry.Artifact, v1.Hash, error) {
	if f.bundle == nil {
		return nil, v1.Hash{}, errors.New("blob unknown")
	}
	return &registry.Artifact{Type: BundleType, Data: f.bundle}, v1.Hash{}, nil
}

// A failure to read is never "unsigned": an outage must not read as a missing signature.
func TestFailureToReadIsNotUnsigned(t *testing.T) {
	t.Parallel()
	d, _ := name.NewDigest("registry.example/t/img@sha256:" + strings.Repeat("a", 64))
	v := verifier(t, testutil.NewKeySigner(t).PEM)
	for what, r := range map[string]fixed{
		"list":                {err: errors.New("503")},
		"bundle":              {},
		"too many signatures": {bundle: []byte("{}"), n: maxBundles + 1},
	} {
		if err := v.Verify(context.Background(), r, d); err == nil || errors.Is(err, ErrUnsigned) || !strings.Contains(err.Error(), "cannot check signatures") {
			t.Errorf("%s: %v", what, err)
		}
	}
	// One past the bound is refused, the bound itself is read.
	if err := v.Verify(context.Background(), fixed{bundle: []byte("{}"), n: maxBundles}, d); !errors.Is(err, ErrUnsigned) {
		t.Errorf("at the bound: %v", err)
	}
}

// A referrer that cannot be read does not hide a valid signature listed after it.
func TestAnUnreadReferrerDoesNotHideASignature(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	k := testutil.NewKeySigner(t)
	k.Sign(t, e.d)
	r := &unreadFirst{Registry: e.c}
	if err := verifier(t, k.PEM).Verify(context.Background(), r, e.d); err != nil {
		t.Fatal(err)
	}
	if !r.failed {
		t.Fatal("the unread referrer was not tried")
	}
}

// unreadFirst lists a referrer that cannot be read before the registry's own.
type unreadFirst struct {
	Registry
	failed bool
}

func (u *unreadFirst) Referrers(ctx context.Context, d name.Digest, t types.MediaType) ([]v1.Descriptor, error) {
	refs, err := u.Registry.Referrers(ctx, d, t)
	h, _ := v1.NewHash("sha256:" + strings.Repeat("e", 64))
	return append([]v1.Descriptor{{Digest: h, ArtifactType: string(t)}}, refs...), err
}

func (u *unreadFirst) GetArtifact(ctx context.Context, ref name.Reference, want types.MediaType) (*registry.Artifact, v1.Hash, error) {
	if strings.HasSuffix(ref.Identifier(), strings.Repeat("e", 64)) {
		u.failed = true
		return nil, v1.Hash{}, errors.New("blob unknown")
	}
	return u.Registry.GetArtifact(ctx, ref, want)
}

func TestNewRefusesABadKey(t *testing.T) {
	t.Parallel()
	if _, err := New(nil); err == nil {
		t.Error("no key accepted")
	}
	if _, err := New(map[string][]byte{"k.pub": []byte("not a key")}); err == nil || !strings.Contains(err.Error(), "k.pub") {
		t.Errorf("bad key: %v", err)
	}
}
