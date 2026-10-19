package testutil

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// KeySigner signs registry digests the way cosign v3 signs with a key pair: an in-toto statement
// naming the digest, in a DSSE sigstore bundle (no transparency log entry) pushed as an OCI
// referrer of the digest.
type KeySigner struct {
	key *ecdsa.PrivateKey
	PEM []byte // the public key
	// Predicate is the statement's predicate type: cosign sign's unless a test makes an
	// attestation of another kind.
	Predicate string
}

// NewKeySigner makes a fresh ECDSA P-256 key pair, as `cosign generate-key-pair` does.
func NewKeySigner(t testing.TB) *KeySigner {
	t.Helper()
	return NewKeySignerOn(t, elliptic.P256())
}

// NewKeySignerOn makes a key pair on curve c; it signs with the hash cosign uses for the curve
// (SHA-256, -384, -512 for P-256, P-384, P-521).
func NewKeySignerOn(t testing.TB, c elliptic.Curve) *KeySigner {
	t.Helper()
	k, err := ecdsa.GenerateKey(c, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return &KeySigner{key: k, PEM: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), Predicate: "https://sigstore.dev/cosign/sign/v1"}
}

// Sign pushes a signature of d. opts carry the registry client options (its transport).
func (s *KeySigner) Sign(t testing.TB, d name.Digest, opts ...remote.Option) {
	t.Helper()
	s.SignAs(t, d, d, opts...)
}

// SignAs pushes, as a referrer of d, a signature over the digest of subject: one that does not
// sign d when subject differs.
func (s *KeySigner) SignAs(t testing.TB, d, subject name.Digest, opts ...remote.Option) {
	t.Helper()
	h, err := v1.NewHash(subject.DigestStr())
	if err != nil {
		t.Fatal(err)
	}
	stmt, _ := json.Marshal(map[string]any{
		"_type":         "https://in-toto.io/Statement/v1",
		"subject":       []any{map[string]any{"digest": map[string]string{"sha256": h.Hex}}},
		"predicateType": s.Predicate,
	})
	const payloadType = "application/vnd.in-toto+json"
	pae := fmt.Sprintf("DSSEv1 %d %s %d %s", len(payloadType), payloadType, len(stmt), stmt)
	var sum []byte
	switch s.key.Curve {
	case elliptic.P384():
		h := sha512.Sum384([]byte(pae))
		sum = h[:]
	case elliptic.P521():
		h := sha512.Sum512([]byte(pae))
		sum = h[:]
	default:
		h := sha256.Sum256([]byte(pae))
		sum = h[:]
	}
	sig, err := ecdsa.SignASN1(rand.Reader, s.key, sum)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"mediaType":            "application/vnd.dev.sigstore.bundle.v0.3+json",
		"verificationMaterial": map[string]any{"publicKey": map[string]string{"hint": ""}},
		"dsseEnvelope": map[string]any{
			"payload":     base64.StdEncoding.EncodeToString(stmt),
			"payloadType": payloadType,
			"signatures":  []any{map[string]string{"sig": base64.StdEncoding.EncodeToString(sig)}},
		},
	})
	PushReferrer(t, d, "application/vnd.dev.sigstore.bundle.v0.3+json", b, opts...)
}

// PushReferrer pushes an artifact of type mt holding data, as a referrer of d.
func PushReferrer(t testing.TB, d name.Digest, mt types.MediaType, data []byte, opts ...remote.Option) {
	t.Helper()
	PushReferrerAs(t, d, mt, mt, data, opts...)
}

// PushReferrerAs pushes, as a referrer of d, an artifact of type mt whose one layer, holding
// data, is of type layer.
func PushReferrerAs(t testing.TB, d name.Digest, mt, layerType types.MediaType, data []byte, opts ...remote.Option) {
	t.Helper()
	sh, err := remote.Head(d, opts...)
	if err != nil {
		t.Fatal(err)
	}
	cfg, layer := static.NewLayer([]byte("{}"), "application/vnd.oci.empty.v1+json"), static.NewLayer(data, layerType)
	desc := func(l v1.Layer) v1.Descriptor {
		h, _ := l.Digest()
		n, _ := l.Size()
		lmt, _ := l.MediaType()
		return v1.Descriptor{MediaType: lmt, Digest: h, Size: n}
	}
	for _, l := range []v1.Layer{cfg, layer} {
		if err := remote.WriteLayer(d.Context(), l, opts...); err != nil {
			t.Fatal(err)
		}
	}
	sub := v1.Descriptor{MediaType: sh.MediaType, Digest: sh.Digest, Size: sh.Size}
	m := v1.Manifest{SchemaVersion: 2, MediaType: types.OCIManifestSchema1, ArtifactType: string(mt), Config: desc(cfg), Layers: []v1.Descriptor{desc(layer)}, Subject: &sub}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	h, _, err := v1.SHA256(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Put(d.Context().Digest(h.String()), rawManifest(raw), opts...); err != nil {
		t.Fatal(err)
	}
}

type rawManifest []byte

func (r rawManifest) RawManifest() ([]byte, error)        { return r, nil }
func (r rawManifest) MediaType() (types.MediaType, error) { return types.OCIManifestSchema1, nil }
