package publish

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"

	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/sbom"
	"github.com/norvexlabs/stowage/internal/sigverify"
)

// Signer signs artifacts and checks their signatures, always by digest.
type Signer interface {
	Sign(ctx context.Context, d name.Digest) error
	// Verify returns nil when d carries a signature made by one of the trusted keys, an error
	// wrapping ErrUnsigned when it carries none, and any other error when it could not tell (the
	// registry, the key store or the signing tool failed).
	Verify(ctx context.Context, d name.Digest) error
	// Attest attaches doc, d's SBOM (SPDX JSON), as an attestation signed by the key.
	Attest(ctx context.Context, d name.Digest, doc []byte) error
	// Attestation is d's SBOM attested by a trusted key; an error wrapping ErrNoAttestation when
	// it carries none, any other when it could not tell.
	Attestation(ctx context.Context, d name.Digest) ([]byte, error)
}

// ErrNoAttestation is the answer "no SBOM attested by a trusted key".
var ErrNoAttestation = sigverify.ErrNoAttestation

// ErrUnsigned is the answer "no signature by a trusted key", as opposed to a failure to check.
var ErrUnsigned = sigverify.ErrUnsigned

// cosignTimeout bounds one cosign call: a key store or registry that accepts a connection and never
// answers must not hang a publication. cosignWaitDelay bounds the wait for its output once it was
// stopped. Variables for tests.
var (
	cosignTimeout   = 5 * time.Minute
	cosignWaitDelay = 10 * time.Second
)

// CosignSigner signs with the cosign CLI (v3): a key-pair signature stored as an OCI referrer of
// the signed digest, with no transparency log — the trust anchor is the public keys installs
// carry. cosign reaches the registry with the credentials written to a private Docker config for
// the duration of one call, never on its command line. Signatures are checked in-process, as
// installs check them.
type CosignSigner struct {
	Cosign string // the cosign CLI; "cosign" when empty
	// Key is the private key: a file, env://VAR, or a KMS/Vault URI. Empty for a verify-only signer.
	Key        string
	PublicKeys []string // files; a signature by any of them verifies
	Registry   name.Registry
	Auth       authn.Authenticator
	WorkDir    string             // holds the per-call Docker config; on disk, private to the user
	Reader     sigverify.Registry // reads the signatures
	verifier   *sigverify.Verifier
}

// NewCosignSigner checks that a signing cosign can be held to HTTPS. cosign falls back to plain
// HTTP for the hosts go-containerregistry treats as local (private addresses, *.localhost) when
// HTTPS fails, which would send the publisher's credentials in clear; only a loopback registry may
// use HTTP. A verify-only signer carries the pull account, which installs hold anyway.
func NewCosignSigner(s CosignSigner) (*CosignSigner, error) {
	if s.Key != "" && s.Registry.Scheme() == "http" && !registry.Loopback(hostname(s.Registry.RegistryStr())) {
		return nil, fmt.Errorf("signing: cosign could fall back to plain HTTP for %s and send the registry credentials in clear; reach the registry by a DNS name over HTTPS", s.Registry.RegistryStr())
	}
	if len(s.PublicKeys) == 0 {
		return nil, errors.New("signing: no public key to verify with")
	}
	if s.Reader == nil {
		return nil, errors.New("signing: no registry to read signatures from")
	}
	pems := map[string][]byte{}
	for _, k := range s.PublicKeys {
		b, err := os.ReadFile(k)
		if err != nil {
			return nil, fmt.Errorf("signing.public_keys: %w", err)
		}
		pems[k] = b
	}
	v, err := sigverify.New(pems)
	if err != nil {
		return nil, fmt.Errorf("signing.public_keys: %w", err)
	}
	s.verifier = v
	return &s, nil
}

func hostname(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

// Sign implements Signer.
func (s *CosignSigner) Sign(ctx context.Context, d name.Digest) error {
	if s.Key == "" {
		return errors.New("signing: no private key (a read-only command cannot sign)")
	}
	args := []string{"--key", s.Key, "--tlog-upload=false", "--use-signing-config=false", "--yes", d.String()}
	if registry.Loopback(hostname(s.Registry.RegistryStr())) {
		args = append([]string{"--allow-http-registry"}, args...)
	}
	return s.run(ctx, nil, true, "sign", args...)
}

// PublicKey is the PEM public key of the private key s.Key, as cosign derives it: what its
// signatures verify against, whatever form the key is given in.
func (s *CosignSigner) PublicKey(ctx context.Context) ([]byte, error) {
	if s.Key == "" {
		return nil, errors.New("signing: no private key")
	}
	var out bytes.Buffer
	if err := s.run(ctx, &out, false, "public-key", "--key", s.Key); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Verify implements Signer, in-process.
func (s *CosignSigner) Verify(ctx context.Context, d name.Digest) error {
	return s.verifier.Verify(ctx, s.Reader, d)
}

// Attest implements Signer: `cosign attest`, the document in a private file of the call's.
func (s *CosignSigner) Attest(ctx context.Context, d name.Digest, doc []byte) error {
	if s.Key == "" {
		return errors.New("signing: no private key (a read-only command cannot attest)")
	}
	if err := os.MkdirAll(s.WorkDir, 0o700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(s.WorkDir, "sbom-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	pred := filepath.Join(dir, "sbom.spdx.json")
	if err := os.WriteFile(pred, doc, 0o600); err != nil {
		return err
	}
	args := []string{"--key", s.Key, "--type", sbom.CosignType, "--predicate", pred, "--tlog-upload=false", "--use-signing-config=false", "--yes", d.String()}
	if registry.Loopback(hostname(s.Registry.RegistryStr())) {
		args = append([]string{"--allow-http-registry"}, args...)
	}
	return s.run(ctx, nil, true, "attest", args...)
}

// Attestation implements Signer, in-process.
func (s *CosignSigner) Attestation(ctx context.Context, d name.Digest) ([]byte, error) {
	r, ok := s.Reader.(sigverify.AttestationRegistry)
	if !ok {
		return nil, errors.New("signing: the registry reader cannot read attestations")
	}
	return s.verifier.Attestation(ctx, r, d, sbom.PredicateType)
}

// SyftMaker makes an image's SBOM with syft, from the registry, which syft reaches with the
// publisher's credentials in a private Docker config of the call's.
type SyftMaker struct {
	Docker   sbom.Docker
	Registry name.Registry
	Auth     authn.Authenticator
	WorkDir  string // on disk, private to the user
}

// SBOM is d's SPDX document.
func (m *SyftMaker) SBOM(ctx context.Context, d name.Digest) ([]byte, error) {
	if err := os.MkdirAll(m.WorkDir, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(m.WorkDir, "syft-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	var c *authn.AuthConfig
	if m.Auth != nil {
		if c, err = m.Auth.Authorization(); err != nil {
			return nil, fmt.Errorf("sbom: registry credentials: %w", err)
		}
	}
	if err := registry.WriteDockerConfig(dir, m.Registry, c, nil); err != nil {
		return nil, err
	}
	return sbom.Generate(ctx, m.Docker, sbom.Source{Ref: d.String(), Insecure: m.Registry.Scheme() == "http", Config: dir})
}

// run runs cosign's verb, with auth the registry's credentials in a private Docker config (an
// empty one without), its stdout to stdout (nil: discarded).
func (s *CosignSigner) run(ctx context.Context, stdout io.Writer, auth bool, verb string, args ...string) error {
	if err := os.MkdirAll(s.WorkDir, 0o700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(s.WorkDir, "cosign-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if auth {
		if err := s.writeDockerConfig(dir); err != nil {
			return err
		}
	}
	cosign := cmp.Or(s.Cosign, "cosign")
	ctx, cancel := context.WithTimeout(ctx, cosignTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cosign, append([]string{verb}, args...)...)
	cmd.Env = append(withoutEnv(os.Environ(), "DOCKER_CONFIG"), "DOCKER_CONFIG="+dir)
	var out tail
	cmd.Stdout, cmd.Stderr = cmp.Or[io.Writer](stdout, io.Discard), &out
	// A child cosign left behind (a credential or KMS helper) must not keep Run waiting on the
	// stderr pipe once cosign itself was stopped.
	cmd.WaitDelay = cosignWaitDelay
	err = cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) && ctx.Err() == nil && cmd.ProcessState.Success() {
		// cosign succeeded; only a child it left behind still held stderr.
		err = nil
	}
	if err != nil {
		return fmt.Errorf("cosign %s: %w: %s", verb, err, lastLine(out.String()))
	}
	return nil
}

// writeDockerConfig writes the registry credentials where cosign looks for them.
func (s *CosignSigner) writeDockerConfig(dir string) error {
	var c *authn.AuthConfig
	if s.Auth != nil {
		var err error
		if c, err = s.Auth.Authorization(); err != nil {
			return fmt.Errorf("signing: registry credentials: %w", err)
		}
	}
	return registry.WriteDockerConfig(dir, s.Registry, c, nil)
}

// lastLine is cosign's error line: its stderr ends with the error, after warnings.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func withoutEnv(env []string, key string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// tail keeps the last 4 KiB written to it: enough of cosign's stderr to explain a failure.
type tail struct {
	b bytes.Buffer
}

const tailMax = 4 << 10

func (t *tail) Write(p []byte) (int, error) {
	t.b.Write(p)
	if over := t.b.Len() - tailMax; over > 0 {
		t.b.Next(over)
	}
	return len(p), nil
}

func (t *tail) String() string { return t.b.String() }
