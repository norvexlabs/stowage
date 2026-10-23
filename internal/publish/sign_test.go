package publish

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// A stand-in cosign CLI records its arguments and the Docker config it was handed; `sign` of a
// digest naming hang or linger hangs, or succeeds leaving a child holding stderr.
const fakeCosign = `#!/bin/sh
echo "$*" >> "$FAKE_COSIGN_LOG"
cat "$DOCKER_CONFIG/config.json" > "$FAKE_COSIGN_LOG.config"
case "$*" in
*hang*) sleep 30 & sleep 30 ;;
*linger*) sleep 30 & exit 0 ;;
esac
`

// pubKeys writes the PEM public keys of signers as old.pub, new.pub, … and returns their files.
func pubKeys(t *testing.T, signers ...*testutil.KeySigner) []string {
	t.Helper()
	dir := t.TempDir()
	var out []string
	for i, s := range signers {
		f := filepath.Join(dir, []string{"old.pub", "new.pub", "third.pub"}[i])
		if err := os.WriteFile(f, s.PEM, 0o644); err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

func newFakeCosign(t *testing.T, reg string, auth authn.Authenticator) (*CosignSigner, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "cosign")
	if err := os.WriteFile(bin, []byte(fakeCosign), 0o755); err != nil {
		t.Fatal(err)
	}
	logf := filepath.Join(dir, "log")
	t.Setenv("FAKE_COSIGN_LOG", logf)
	r, err := name.NewRegistry(reg, name.StrictValidation)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewCosignSigner(CosignSigner{Cosign: bin, Key: "env://KEY", PublicKeys: pubKeys(t, testutil.NewKeySigner(t)),
		Registry: r, Auth: auth, WorkDir: filepath.Join(dir, "work"), Reader: registry.New(authn.DefaultKeychain)})
	if err != nil {
		t.Fatal(err)
	}
	return s, logf
}

func TestCosignSignerSignsOfflineWithCredentialsOffTheCommandLine(t *testing.T) {
	s, logf := newFakeCosign(t, "registry.example.com", &authn.Basic{Username: "publisher", Password: "s3cret"})
	d, _ := name.NewDigest("registry.example.com/acme/notes-api@sha256:" + strings.Repeat("a", 64))
	if err := s.Sign(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	log := readFile(t, logf)
	eq(t, "args", strings.TrimSpace(log), "sign --key env://KEY --tlog-upload=false --use-signing-config=false --yes "+d.String())
	if strings.Contains(log, "s3cret") {
		t.Fatal("the password reached cosign's command line")
	}
	want := base64.StdEncoding.EncodeToString([]byte("publisher:s3cret"))
	eq(t, "docker config", readFile(t, logf+".config"), `{"auths":{"registry.example.com":{"auth":"`+want+`"}}}`)
	if left, _ := os.ReadDir(s.WorkDir); len(left) != 0 {
		t.Fatalf("the per-call Docker config was left behind: %v", left)
	}
}

func TestCosignSignerTrustsAnyListedKey(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	img, _ := random.Image(64, 1)
	h, _ := img.Digest()
	d, _ := name.NewDigest(strings.TrimPrefix(srv.URL, "http://") + "/acme/notes-api@" + h.String())
	if err := remote.Write(d, img); err != nil {
		t.Fatal(err)
	}
	old, rotated, other := testutil.NewKeySigner(t), testutil.NewKeySigner(t), testutil.NewKeySigner(t)
	s, err := NewCosignSigner(CosignSigner{Registry: d.Registry, PublicKeys: pubKeys(t, old, rotated), Reader: registry.New(authn.DefaultKeychain)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(context.Background(), d); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("no signature: got %v", err)
	}
	other.Sign(t, d)
	if err := s.Verify(context.Background(), d); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("a signature by another key: got %v", err)
	}
	rotated.Sign(t, d)
	if err := s.Verify(context.Background(), d); err != nil {
		t.Fatalf("a signature by the second listed key: %v", err)
	}
	// A registry that cannot be read is a failure to check, never "unsigned".
	srv.Close()
	if err := s.Verify(context.Background(), d); err == nil || errors.Is(err, ErrUnsigned) {
		t.Fatalf("an outage: got %v", err)
	}
}

func TestNewCosignSignerReadsThePublicKeys(t *testing.T) {
	t.Parallel()
	r, _ := name.NewRegistry("registry.example.com")
	bad := filepath.Join(t.TempDir(), "bad.pub")
	if err := os.WriteFile(bad, []byte("not a key"), 0o644); err != nil {
		t.Fatal(err)
	}
	reader := registry.New(authn.DefaultKeychain)
	for _, keys := range [][]string{{filepath.Join(t.TempDir(), "missing.pub")}, {bad}} {
		if _, err := NewCosignSigner(CosignSigner{Registry: r, PublicKeys: keys, Reader: reader}); err == nil || !strings.Contains(err.Error(), "signing.public_keys") {
			t.Errorf("%s: %v", keys[0], err)
		}
	}
	if _, err := NewCosignSigner(CosignSigner{Registry: r, PublicKeys: pubKeys(t, testutil.NewKeySigner(t))}); err == nil || !strings.Contains(err.Error(), "no registry to read signatures") {
		t.Errorf("no reader: %v", err)
	}
}

func TestCosignSignerHTTPSOnlyBeyondLoopback(t *testing.T) {
	keys, reader := pubKeys(t, testutil.NewKeySigner(t)), registry.New(authn.DefaultKeychain)
	for host, ok := range map[string]bool{
		"registry.example.com": true,
		"10.0.0.5:5000":        false,
		"192.168.1.20":         false,
		"nas.local:5000":       true,
		"reg.localhost:5000":   false,
		"localhost:5000":       true,
		"127.0.0.1:5000":       true,
		"[::1]:5000":           true,
	} {
		r, err := name.NewRegistry(host)
		if err != nil {
			t.Fatal(err)
		}
		_, err = NewCosignSigner(CosignSigner{Registry: r, Key: "k.key", PublicKeys: keys, Reader: reader})
		if (err == nil) != ok {
			t.Errorf("%s: %v", host, err)
		}
		if _, err := NewCosignSigner(CosignSigner{Registry: r, PublicKeys: keys, Reader: reader}); err != nil {
			t.Errorf("%s: verify-only: %v", host, err)
		}
	}
	s, logf := newFakeCosign(t, "registry.example.com", authn.Anonymous)
	s.Key = ""
	if err := s.Sign(context.Background(), name.Digest{}); err == nil {
		t.Fatal("a verify-only signer signed")
	}
	if _, err := os.Stat(logf); err == nil {
		t.Fatal("a verify-only signer ran cosign sign")
	}
}

func TestACosignThatHangsIsStopped(t *testing.T) {
	s, _ := newFakeCosign(t, "registry.example.com", authn.Anonymous)
	cosignTimeout, cosignWaitDelay = 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { cosignTimeout, cosignWaitDelay = 5*time.Minute, 10*time.Second })
	d, _ := name.NewDigest("registry.example.com/acme/hang@sha256:" + strings.Repeat("a", 64))
	start := time.Now()
	err := s.Sign(context.Background(), d)
	if err == nil {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %s: a child holding stderr kept the call waiting", time.Since(start))
	}
}

func TestACosignThatSucceedsAndLeavesAChildSucceeds(t *testing.T) {
	s, _ := newFakeCosign(t, "registry.example.com", authn.Anonymous)
	cosignWaitDelay = 200 * time.Millisecond
	t.Cleanup(func() { cosignWaitDelay = 10 * time.Second })
	d, _ := name.NewDigest("registry.example.com/acme/linger@sha256:" + strings.Repeat("a", 64))
	if err := s.Sign(context.Background(), d); err != nil {
		t.Fatal(err)
	}
}

// The public key of the signer's key: cosign public-key, which reaches no registry, is given no
// credentials.
func TestCosignSignerPublicKeyCarriesNoCredentials(t *testing.T) {
	s, logf := newFakeCosign(t, "registry.example.com", &authn.Basic{Username: "publisher", Password: "s3cret"})
	if _, err := s.PublicKey(context.Background()); err != nil {
		t.Fatal(err)
	}
	eq(t, "args", strings.TrimSpace(readFile(t, logf)), "public-key --key env://KEY")
	if c := readFile(t, logf+".config"); strings.Contains(c, "auth") {
		t.Fatalf("cosign public-key was given the registry's credentials: %s", c)
	}
}
