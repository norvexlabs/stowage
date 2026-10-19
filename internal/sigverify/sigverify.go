// Package sigverify checks that an artifact in a registry carries a signature by a trusted key:
// a sigstore bundle (as cosign v3 writes it) stored as an OCI referrer of the artifact's digest,
// verified in-process against the public keys an install or a project trusts, with no
// transparency log — the trust anchor is the keys.
package sigverify

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/norvexlabs/stowage/internal/registry"
)

// BundleType is the artifact type of a sigstore bundle referrer.
const BundleType types.MediaType = "application/vnd.dev.sigstore.bundle.v0.3+json"

// SignPredicate is the in-toto predicate type of a `cosign sign` signature: an attestation the
// same key made over the digest (an SBOM, a scan) does not approve it.
const SignPredicate = "https://sigstore.dev/cosign/sign/v1"

// maxBundles bounds the signatures read for one digest: a registry listing more is refused
// rather than read without end.
var maxBundles = 64

// ErrUnsigned is the answer "no signature by a trusted key", as opposed to a failure to check.
var ErrUnsigned = errors.New("no signature by a trusted key")

// Registry is the part of the registry client a verification reads.
type Registry interface {
	Referrers(ctx context.Context, d name.Digest, t types.MediaType) ([]v1.Descriptor, error)
	GetArtifact(ctx context.Context, ref name.Reference, want types.MediaType) (*registry.Artifact, v1.Hash, error)
}

// Verifier checks signatures against a fixed set of trusted public keys.
type Verifier struct {
	keys []key
}

type key struct {
	name string
	v    *verify.Verifier
}

// New returns a verifier trusting the PEM public keys given, by name (a file name, for errors).
func New(pems map[string][]byte) (*Verifier, error) {
	if len(pems) == 0 {
		return nil, errors.New("no public key to verify with")
	}
	names := make([]string, 0, len(pems))
	for n := range pems {
		names = append(names, n)
	}
	sort.Strings(names)
	out := &Verifier{}
	for _, n := range names {
		pub, err := cryptoutils.UnmarshalPEMToPublicKey(pems[n])
		if err != nil {
			return nil, fmt.Errorf("public key %s: %w", n, err)
		}
		sv, err := signature.LoadDefaultVerifier(pub) // the hash cosign signs with for this key type
		if err != nil {
			return nil, fmt.Errorf("public key %s: %w", n, err)
		}
		ek := root.NewExpiringKey(sv, time.Time{}, time.Time{}) // no validity window: the key is trusted while it is listed
		tm := root.NewTrustedPublicKeyMaterial(func(string) (root.TimeConstrainedVerifier, error) { return ek, nil })
		v, err := verify.NewVerifier(tm, verify.WithNoObserverTimestamps())
		if err != nil {
			return nil, fmt.Errorf("public key %s: %w", n, err)
		}
		out.keys = append(out.keys, key{name: n, v: v})
	}
	return out, nil
}

// Verify returns nil when d carries a signature made by one of the trusted keys over d itself, an
// error wrapping ErrUnsigned when it carries none, and any other error when it could not tell (a
// referrer listed and not read is a failure to check, never "unsigned"; one that is not a bundle
// signs nothing and is passed over).
func (v *Verifier) Verify(ctx context.Context, r Registry, d name.Digest) error {
	h, err := v1.NewHash(d.DigestStr())
	if err != nil {
		return err
	}
	if h.Algorithm != "sha256" {
		return fmt.Errorf("%s: only sha256 digests are verified", d)
	}
	sum, err := hex.DecodeString(h.Hex)
	if err != nil {
		return fmt.Errorf("%s: %w", d, err)
	}
	refs, err := r.Referrers(ctx, d, BundleType)
	if err != nil {
		return fmt.Errorf("%s: cannot check signatures: %w", d, err)
	}
	if len(refs) == 0 {
		return fmt.Errorf("%s: no signature: %w", d, ErrUnsigned)
	}
	if len(refs) > maxBundles {
		return fmt.Errorf("%s: cannot check signatures: %d signatures, more than %d", d, len(refs), maxBundles)
	}
	policy := verify.NewPolicy(verify.WithArtifactDigest("sha256", sum), verify.WithKey())
	// Every referrer is tried: one that cannot be read does not hide a valid signature beside it,
	// and is reported only when none verifies.
	var readErr error
	for _, ref := range refs {
		a, _, err := r.GetArtifact(ctx, d.Context().Digest(ref.Digest.String()), BundleType)
		if errors.Is(err, registry.ErrNotArtifact) || (err == nil && a == nil) {
			continue
		}
		if err != nil {
			readErr = err
			continue
		}
		var b bundle.Bundle
		if err := b.UnmarshalJSON(a.Data); err != nil {
			continue // not a bundle this verifier reads: it signs nothing it can trust
		}
		for _, k := range v.keys {
			if res, err := k.v.Verify(&b, policy); err == nil && res.Statement != nil && res.Statement.PredicateType == SignPredicate {
				return nil
			}
		}
	}
	if readErr != nil {
		return fmt.Errorf("%s: cannot check signatures: %w", d, readErr)
	}
	return fmt.Errorf("%s: %d signature(s), none by a trusted key: %w", d, len(refs), ErrUnsigned)
}
