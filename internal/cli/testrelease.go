package cli

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/sigstore/sigstore/pkg/cryptoutils"

	"github.com/norvexlabs/stowage/internal/lint"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/publish"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/spec"
)

// testRelease is a release made for a test run, never the project's: published to another
// registry prefix with the run's own credentials, signed with the run's own key, which its
// installs trust alone, and with only the shapes asked for. Every command that opens the project
// takes it, so a test run's ship, verify, releases and promote all see the same release.
type testRelease struct {
	Registry string   // the registry prefix, never the project's
	Key      string   // the run's cosign private key: a file, env://VAR, or a KMS URI
	Public   string   // its public key, a project file: the one key installs trust
	Shapes   []string // the shapes kept; empty keeps them all
	Push     string   // the publisher's account, USER:ENV (its password's variable)
	Pull     string   // the pull account its installers carry, USER:ENV, or none
}

// TestShapes are the shapes --shapes takes.
var TestShapes = []string{"docker", "kubernetes", "kairos"}

func (t testRelease) on() bool {
	return t.Registry != "" || t.Key != "" || t.Public != "" || len(t.Shapes) > 0 || t.Push != "" || t.Pull != ""
}

// registrySegments is a registry prefix as the registry names it: its host lower-cased, without
// a trailing dot or the default HTTPS port, Docker Hub's host names as one, then its path's
// segments.
func registrySegments(prefix string) []string {
	host, path, _ := strings.Cut(prefix, "/")
	out := []string{foldHost(host)}
	if path != "" {
		out = append(out, strings.Split(path, "/")...)
	}
	return out
}

// foldHost is a registry host as one registry is reached whatever its spelling: the loopback's
// names (localhost, *.localhost, 127.0.0.0/8, ::1) as one, an IP address in its canonical form,
// a name lower-cased without its trailing dot, Docker Hub's names as one, and the port as a
// number, the default ones (80 and 443) as none. It may take two registries for one (one on 80,
// another on 443), never one spelling for two. Another DNS name of the same registry is not
// told: the spelling is all it reads.
func foldHost(host string) string {
	h, port := host, ""
	if sh, sp, err := net.SplitHostPort(host); err == nil {
		h, port = sh, sp
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		h = host[1 : len(host)-1]
	}
	h = strings.ToLower(strings.TrimRight(h, "."))
	switch ip := net.ParseIP(h); {
	case registry.Loopback(h) || strings.HasSuffix(h, ".localhost"):
		// Every name of this host's loopback reaches the one registry listening on the port.
		h = "localhost"
	case ip != nil:
		h = "[" + ip.String() + "]"
	case strings.ContainsAny(h, ":[]"):
		// No IP and no name: kept as written.
		h = "[" + h + "]"
	case h == "docker.io" || h == "registry-1.docker.io" || h == "registry.hub.docker.com":
		h = name.DefaultRegistry
	}
	if n, err := strconv.Atoi(port); err == nil && n != 80 && n != 443 {
		return h + ":" + strconv.Itoa(n)
	}
	return h
}

// overlaps reports whether one prefix is the other or lies within it: the repositories of one
// would be the other's.
func overlaps(a, b []string) bool {
	n := min(len(a), len(b))
	return slices.Equal(a[:n], b[:n])
}

// account reads a test account, USER:ENV, as a token provider.
func account(flag, v string) (spec.Provider, error) {
	user, env, ok := strings.Cut(v, ":")
	p := spec.Provider{Name: "token", Config: map[string]any{"user": user, "token_env": env}}
	if !ok {
		return p, fmt.Errorf("%s %q: USER:ENV, the account and the variable holding its password", flag, v)
	}
	if _, err := providers.Token(p); err != nil {
		return p, fmt.Errorf("%s: %w", flag, err)
	}
	return p, nil
}

// apply makes p's spec the test release's, or refuses one that could pass for the project's, or
// hand the project's credentials to another registry: on its registry, signed with its key, with
// its push token or its pull account, or a production release with shapes left out.
func (t testRelease) apply(p *project.Project) error {
	s := p.Spec
	var err error
	if t.Registry == "" {
		return errors.New("--test-key, --test-public-key, --test-push, --test-pull and --shapes make a test release: give its registry, --test-registry PREFIX")
	}
	if !lint.RegistryPrefix(t.Registry) {
		return fmt.Errorf("--test-registry %q: a registry prefix is host[:port][/path], lower case, without a scheme or a trailing /", t.Registry)
	}
	host, _, _ := strings.Cut(t.Registry, "/")
	if _, port, err := net.SplitHostPort(host); err == nil {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("--test-registry %q: port %s is not 1 to 65535", t.Registry, port)
		}
	}
	// Compared as the registry names them, and refused within the project's namespace too (or
	// holding it): its repositories would be the project's.
	if overlaps(registrySegments(t.Registry), registrySegments(s.Registry.Prefix)) {
		return fmt.Errorf("--test-registry %s is the project's registry (registry.prefix %s), or within it: a test release goes elsewhere", t.Registry, s.Registry.Prefix)
	}
	// The registry's credentials are the run's: the project's would be sent to, or baked into an
	// installer published on, another registry.
	switch {
	case t.Push != "":
		if s.Registry.Push, err = account("--test-push", t.Push); err != nil {
			return err
		}
	case s.Registry.Push.Name == "token":
		return errors.New("registry.push is the project's token, never sent to a test registry: give the run's account, --test-push USER:ENV")
	case s.Registry.Push.Name == "docker-config" && s.Registry.Push.Config["config"] != nil:
		// The publisher's own Docker config is the operator's; the project's names its account.
		return errors.New("registry.push is the project's Docker config, never used on a test registry: give the run's account, --test-push USER:ENV")
	}
	switch {
	case t.Pull == "none":
		s.Registry.Pull = spec.Provider{Name: "none"}
	case t.Pull != "":
		if s.Registry.Pull, err = account("--test-pull", t.Pull); err != nil {
			return err
		}
	case s.Registry.Pull.Name == "token":
		return errors.New("registry.pull is the project's account, which a test release's installers would carry: give the run's, --test-pull USER:ENV, or --test-pull none")
	}
	// Deleting goes through the test registry's own API, never the project's packages service.
	s.Registry.Delete = spec.Provider{}
	if err := t.signing(p); err != nil {
		return err
	}
	if len(t.Shapes) > 0 {
		for _, sh := range t.Shapes {
			if !slices.Contains(TestShapes, sh) {
				return fmt.Errorf("--shapes: unknown shape %q (%s)", sh, strings.Join(TestShapes, ", "))
			}
		}
		if !slices.Contains(t.Shapes, "docker") {
			s.Shapes.Docker = nil
		}
		if !slices.Contains(t.Shapes, "kubernetes") {
			s.Shapes.Kubernetes = nil
		}
		if !slices.Contains(t.Shapes, "kairos") {
			s.Shapes.Kairos = nil
		}
		if s.Shapes.Docker == nil && s.Shapes.Kubernetes == nil && s.Shapes.Kairos == nil {
			return fmt.Errorf("--shapes %s: the project declares none of them", strings.Join(t.Shapes, ","))
		}
	}
	// A test release's disks are registered on no cloud: the images would reach the project's
	// accounts.
	if k := s.Shapes.Kairos; k != nil {
		c := *k
		c.CloudPublish = nil
		s.Shapes.Kairos = &c
	}
	s.Registry.Prefix = t.Registry
	return nil
}

// signing gives the spec the run's key pair, never the project's: its public key, a PEM public
// key, is the one the release trusts.
func (t testRelease) signing(p *project.Project) error {
	s := p.Spec
	switch {
	case (t.Key == "") != (t.Public == ""):
		return errors.New("--test-key and --test-public-key go together: the run's key pair")
	case t.Key == "" && s.Signs():
		return errors.New("the project signs its releases, and a test release is never signed with its key: give the run's own, --test-key and --test-public-key")
	case t.Key == "":
		return nil
	}
	path, err := spec.ProjectPath(p.Root, t.Public)
	if err != nil {
		return fmt.Errorf("--test-public-key: %w", err)
	}
	pub, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("--test-public-key: %w", err)
	}
	der, err := publicKeyDER(pub)
	if err != nil {
		return fmt.Errorf("--test-public-key %s: %w", t.Public, err)
	}
	key := t.Key
	if !keyURI.MatchString(key) {
		if key, err = filepath.Abs(key); err != nil {
			return err
		}
	}
	if s.Signs() {
		c, err := providers.CosignKey(s.Signing)
		if err != nil {
			return fmt.Errorf("signing: %w", err)
		}
		own := c.Key
		if !keyURI.MatchString(own) && !filepath.IsAbs(own) {
			own = filepath.Join(p.Root, own)
		}
		if key == own || sameFile(key, own) {
			return fmt.Errorf("--test-key %s is the project's signing key", t.Key)
		}
		// Compared as keys, so another encoding of the project's key is it too; a key of the
		// project's that cannot be read refuses, as it could be the one given.
		for _, k := range c.PublicKeys {
			f, err := spec.ProjectPath(p.Root, k)
			if err != nil {
				return fmt.Errorf("signing.public_keys: %w: a test release's key is checked against the project's", err)
			}
			raw, err := os.ReadFile(f)
			if err != nil {
				return fmt.Errorf("signing.public_keys: %w: a test release's key is checked against the project's", err)
			}
			own, err := publicKeyDER(raw)
			if err != nil {
				return fmt.Errorf("signing.public_keys %s: %w: a test release's key is checked against the project's", k, err)
			}
			if bytes.Equal(own, der) {
				return fmt.Errorf("--test-public-key %s is the project's key %s: a test release is never signed with it", t.Public, k)
			}
		}
	}
	s.Signing = &spec.Provider{Name: "cosign-key", Config: map[string]any{"key": key, "public_keys": []any{t.Public}}}
	return nil
}

// sameKey refuses the run's private key unless it is the pair of its public key, trusted (the
// release's one key): checked before anything is signed.
func (t testRelease) sameKey(ctx context.Context, s *publish.CosignSigner, trusted []string) error {
	derived, err := s.PublicKey(ctx)
	if err != nil {
		return fmt.Errorf("--test-key: %w", err)
	}
	der, err := publicKeyDER(derived)
	if err != nil {
		return fmt.Errorf("--test-key: its public key: %w", err)
	}
	for _, f := range trusted {
		raw, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("--test-public-key: %w", err)
		}
		want, err := publicKeyDER(raw)
		if err != nil {
			return fmt.Errorf("--test-public-key %s: %w", f, err)
		}
		if bytes.Equal(want, der) {
			return nil
		}
	}
	return fmt.Errorf("--test-key %s is not the key of --test-public-key %s: not signed", t.Key, t.Public)
}

// publicKeyDER is a PEM public key's key, as DER: every form installs trust (PKIX or PKCS#1).
func publicKeyDER(raw []byte) ([]byte, error) {
	k, err := cryptoutils.UnmarshalPEMToPublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("not a PEM public key: %w", err)
	}
	der, err := x509.MarshalPKIXPublicKey(k)
	if err != nil {
		return nil, fmt.Errorf("a key of a type cosign does not sign with: %w", err)
	}
	return der, nil
}

// describe is the line every command of a test run prints first.
func (t testRelease) describe(p *project.Project) string {
	var b strings.Builder
	fmt.Fprintf(&b, "test release: registry %s", t.Registry)
	if t.Key != "" {
		fmt.Fprintf(&b, ", signed with %s, its installs trusting %s alone", t.Key, t.Public)
	}
	var kept []string
	for _, sh := range TestShapes {
		if p.Spec.Shapes.Docker != nil && sh == "docker" || p.Spec.Shapes.Kubernetes != nil && sh == "kubernetes" || p.Spec.Shapes.Kairos != nil && sh == "kairos" {
			kept = append(kept, sh)
		}
	}
	fmt.Fprintf(&b, ", shapes %s", strings.Join(kept, ", "))
	return b.String()
}

// sameFile reports whether a and b are one file, or two files holding the same bytes.
func sameFile(a, b string) bool {
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	if errA != nil || errB != nil {
		return false
	}
	if os.SameFile(fa, fb) {
		return true
	}
	ra, errA := os.ReadFile(a)
	rb, errB := os.ReadFile(b)
	return errA == nil && errB == nil && bytes.Equal(ra, rb)
}
