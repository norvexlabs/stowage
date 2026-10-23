package cli

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/lint"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// trProject is the minimal example with the run's public key keys/test.pub beside the project's
// own keys/release.pub; signs, it signs with the latter; tokens, it pushes and pulls with token
// accounts.
func trProject(t *testing.T, signs, tokens bool) *project.Project {
	t.Helper()
	root := testutil.CopyExample(t, "minimal")
	own, run := testutil.NewKeySigner(t), testutil.NewKeySigner(t)
	testutil.Write(t, root, "keys/release.pub", string(own.PEM))
	testutil.Write(t, root, "keys/release-crlf.pub", strings.ReplaceAll(string(own.PEM), "\n", "\r\n"))
	testutil.Write(t, root, "keys/test.pub", string(run.PEM))
	testutil.Write(t, root, "keys/not-a-key.pub", "hello\n")
	testutil.Write(t, root, "keys/release.key", "the project's private key\n")
	testutil.Write(t, root, "copy-of-release.key", "the project's private key\n")
	if signs {
		testutil.Edit(t, root, "stowage.yaml", "images:", "signing: { provider: cosign-key, key: keys/release.key, public_keys: [keys/release.pub] }\nimages:")
	}
	if tokens {
		testutil.Edit(t, root, "stowage.yaml", "pull: { provider: none }", "pull: { provider: token, user: notes-pull, token_env: NOTES_PULL_TOKEN }")
		testutil.Edit(t, root, "stowage.yaml", "push: { provider: docker-config }", "push: { provider: token, user: publisher, token_env: NOTES_PUSH_TOKEN }")
	}
	p, err := project.Open(root, filepath.Join(root, "stowage.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A test release refuses whatever could pass for the project's release, or hand the project's
// credentials to another registry.
func TestTestReleaseRefusals(t *testing.T) {
	t.Parallel()
	const reg = "localhost:5000/notes"
	for _, c := range []struct {
		name          string
		signs, tokens bool
		t             testRelease
		want          string
	}{
		{"shapes alone", false, false, testRelease{Shapes: []string{"docker"}}, "give its registry, --test-registry PREFIX"},
		{"key alone", false, false, testRelease{Key: "k.key", Public: "keys/test.pub"}, "give its registry"},
		{"push alone", false, false, testRelease{Push: "u:V"}, "give its registry"},
		{"the project's registry", false, false, testRelease{Registry: "ghcr.io/acme/notes"}, "is the project's registry (registry.prefix ghcr.io/acme/notes)"},
		{"a trailing slash", false, false, testRelease{Registry: "ghcr.io/acme/notes/"}, "without a scheme or a trailing /"},
		{"upper case", false, false, testRelease{Registry: "GHCR.IO/acme/notes"}, "lower case"},
		{"the project's registry, its port", false, false, testRelease{Registry: "ghcr.io:443/acme/notes"}, "is the project's registry"},
		{"within the project's registry", false, false, testRelease{Registry: "ghcr.io/acme/notes/test"}, "or within it"},
		{"holding the project's registry", false, false, testRelease{Registry: "ghcr.io/acme"}, "or within it"},
		{"a path the prefix grammar refuses", false, false, testRelease{Registry: "localhost:5000/acme//notes"}, "a registry prefix is host[:port][/path]"},
		{"a port past 65535", false, false, testRelease{Registry: "localhost:65536/notes"}, "port 65536 is not 1 to 65535"},
		{"a port that overflows", false, false, testRelease{Registry: "localhost:99999999999999999999/notes"}, "is not 1 to 65535"},
		{"not a prefix", false, false, testRelease{Registry: "Bad Prefix"}, `--test-registry "Bad Prefix"`},
		{"a key without its public key", false, false, testRelease{Registry: reg, Key: "k.key"}, "go together"},
		{"a public key without its key", false, false, testRelease{Registry: reg, Public: "keys/test.pub"}, "go together"},
		{"signed with the project's key", true, false, testRelease{Registry: reg}, "never signed with its key"},
		{"the project's key given", true, false, testRelease{Registry: reg, Key: "ROOT/keys/release.key", Public: "keys/test.pub"}, "is the project's signing key"},
		{"a copy of the project's key given", true, false, testRelease{Registry: reg, Key: "ROOT/copy-of-release.key", Public: "keys/test.pub"}, "is the project's signing key"},
		{"the project's public key given", true, false, testRelease{Registry: reg, Key: "k.key", Public: "keys/release.pub"}, "is the project's key keys/release.pub"},
		{"the project's public key, encoded otherwise", true, false, testRelease{Registry: reg, Key: "k.key", Public: "keys/release-crlf.pub"}, "is the project's key keys/release.pub"},
		{"a public key that is none", false, false, testRelease{Registry: reg, Key: "k.key", Public: "keys/not-a-key.pub"}, "not a PEM public key"},
		{"a public key outside the project", false, false, testRelease{Registry: reg, Key: "k.key", Public: "../k.pub"}, "--test-public-key"},
		{"an unknown shape", false, false, testRelease{Registry: reg, Shapes: []string{"docker", "nomad"}}, `unknown shape "nomad"`},
		{"the project's push token", false, true, testRelease{Registry: reg, Pull: "none"}, "registry.push is the project's token, never sent to a test registry"},
		{"the project's pull account", false, true, testRelease{Registry: reg, Push: "publisher:RUN_PUSH"}, "registry.pull is the project's account"},
		{"an account without its variable", false, false, testRelease{Registry: reg, Push: "publisher"}, `--test-push "publisher": USER:ENV`},
		{"an account's variable not one", false, false, testRelease{Registry: reg, Pull: "pull:not a var"}, "--test-pull: token: token_env"},
	} {
		p := trProject(t, c.signs, c.tokens)
		c.t.Key = strings.Replace(c.t.Key, "ROOT", p.Root, 1)
		if err := c.t.apply(p); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// A push through the project's own Docker config is its account too; the publisher's default one
// is the operator's, read for the test registry's host.
func TestTestReleaseDockerConfig(t *testing.T) {
	t.Parallel()
	p := trProject(t, false, false)
	p.Spec.Registry.Push = spec.Provider{Name: "docker-config", Config: map[string]any{"config": "ci/docker"}}
	if err := (testRelease{Registry: "localhost:5000/notes"}).apply(p); err == nil || !strings.Contains(err.Error(), "registry.push is the project's Docker config") {
		t.Errorf("the project's config: %v", err)
	}
	p = trProject(t, false, false)
	p.Spec.Registry.Push = spec.Provider{Name: "docker-config", Config: map[string]any{"config": "ci/docker"}}
	if err := (testRelease{Registry: "localhost:5000/notes", Push: "u:RUN_PUSH"}).apply(p); err != nil || p.Spec.Registry.Push.Name != "token" {
		t.Errorf("the run's account over it: %v", err)
	}
	p = trProject(t, false, false)
	if err := (testRelease{Registry: "localhost:5000/notes"}).apply(p); err != nil || p.Spec.Registry.Push.Name != "docker-config" {
		t.Errorf("the default config: %v", err)
	}
}

// What a test release accepts: the project at another registry, with the run's accounts, signed
// with the run's key alone, the shapes asked for, its disks registered on no cloud.
func TestTestReleaseApply(t *testing.T) {
	t.Parallel()
	p := trProject(t, true, true)
	p.Spec.Registry.Delete = spec.Provider{Name: "github"}
	p.Spec.Shapes.Kairos.CloudPublish = []string{"gcp"}
	tr := testRelease{Registry: "localhost:5000/notes", Key: "k.key", Public: "keys/test.pub", Push: "publisher:RUN_PUSH", Pull: "pull:RUN_PULL"}
	if err := tr.apply(p); err != nil {
		t.Fatal(err)
	}
	s := p.Spec
	abs, _ := filepath.Abs("k.key")
	if s.Registry.Prefix != "localhost:5000/notes" || s.Signing.Config["key"] != abs || s.Registry.Delete.Name != "" {
		t.Fatalf("applied: %+v %+v", s.Registry, s.Signing)
	}
	if keys := s.Signing.Config["public_keys"].([]any); len(keys) != 1 || keys[0] != "keys/test.pub" {
		t.Fatalf("the trusted keys: %v", keys)
	}
	if s.Registry.Push.Config["token_env"] != "RUN_PUSH" || s.Registry.Pull.Config["user"] != "pull" || s.Registry.Pull.Config["token_env"] != "RUN_PULL" {
		t.Fatalf("the accounts: %+v %+v", s.Registry.Push, s.Registry.Pull)
	}
	if s.Shapes.Kairos == nil || s.Shapes.Kairos.CloudPublish != nil {
		t.Fatalf("the kairos shape: %+v", s.Shapes.Kairos)
	}
	if d := tr.describe(p); d != "test release: registry localhost:5000/notes, signed with k.key, its installs trusting keys/test.pub alone, shapes docker, kubernetes, kairos" {
		t.Errorf("%q", d)
	}
	// The shapes asked for; a key that is no file is kept as given; an unsigned project with
	// host-scoped credentials needs neither key nor account.
	p = trProject(t, false, false)
	if err := (testRelease{Registry: "localhost:5000/notes", Key: "env://RUN_KEY", Public: "keys/test.pub", Shapes: []string{"docker", "kubernetes"}, Pull: "none"}).apply(p); err != nil ||
		p.Spec.Signing.Config["key"] != "env://RUN_KEY" || p.Spec.Shapes.Kairos != nil || p.Spec.Shapes.Docker == nil || p.Spec.Registry.Pull.Name != "none" || p.Spec.Registry.Push.Name != "docker-config" {
		t.Fatalf("env:// key: %v %+v", err, p.Spec.Registry)
	}
	p = trProject(t, false, false)
	if err := (testRelease{Registry: "localhost:5000/notes"}).apply(p); err != nil || p.Spec.Signing != nil {
		t.Fatalf("unsigned: %v", err)
	}
	p = trProject(t, false, false)
	if err := (testRelease{Registry: "localhost:5000"}).apply(p); err != nil || p.Spec.Registry.Prefix != "localhost:5000" {
		t.Fatalf("a registry's host alone: %v", err)
	}
	p = trProject(t, false, false)
	p.Spec.Shapes.Docker = nil
	if err := (testRelease{Registry: "localhost:5000/notes", Shapes: []string{"docker"}}).apply(p); err == nil || !strings.Contains(err.Error(), "the project declares none of them") {
		t.Errorf("no shape left: %v", err)
	}
}

// A test release is published at its registry, which every command of the run reads, and the
// project's registry holds none of it.
func TestShipATestRelease(t *testing.T) {
	t.Parallel()
	root := registryProject(t)
	b := &randomBuilder{}
	env := map[string]string{"NOTES_PUSH_TOKEN": "t0ken"}
	p, err := project.Open(root, filepath.Join(root, "stowage.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	host, _, _ := strings.Cut(p.Spec.Registry.Prefix, "/")
	test := []string{"-C", root, "--test-registry", host + "/test/notes", "--test-push", "publisher:NOTES_PUSH_TOKEN", "--shapes", "docker"}
	out, logs, err := run(t, b, env, append(test, "ship", "--channel", "beta")...)
	if err != nil || !strings.Contains(logs, "test release: registry "+host+"/test/notes, shapes docker\n") || !strings.Contains(out, "notes 1.0.0 revision 1 · channel beta") {
		t.Fatalf("ship: %v\n%s\n%s", err, out, logs)
	}
	if out, _, err := run(t, b, env, append(test, "releases")...); err != nil || !strings.Contains(out, "beta-1.0.0") {
		t.Fatalf("releases of the test run: %v\n%s", err, out)
	}
	if out, _, err := run(t, b, env, "-C", root, "releases"); err != nil || strings.Contains(out, "beta-1.0.0") {
		t.Fatalf("the project's registry holds the test release: %v\n%s", err, out)
	}
	if _, _, err := run(t, b, env, append(test, "hook", "post-commit")...); err == nil || !strings.Contains(err.Error(), "never a test one") {
		t.Errorf("a test hook: %v", err)
	}
}

// The run's key must be its public key's pair: the project's key, in whatever form it is given
// (here env://, cosign deriving the project's public key from it), signs nothing.
func TestTestReleaseKeyIsItsPublicKeysPair(t *testing.T) {
	root := registryProject(t)
	own, theRun := testutil.NewKeySigner(t), testutil.NewKeySigner(t)
	testutil.Write(t, root, "keys/release.pub", string(own.PEM))
	testutil.Write(t, root, "keys/test.pub", string(theRun.PEM))
	testutil.Edit(t, root, "stowage.yaml", "images:", "signing: { provider: cosign-key, key: env://PROJECT_KEY, public_keys: [keys/release.pub] }\nimages:")
	cosign := filepath.Join(t.TempDir(), "cosign")
	testutil.Write(t, filepath.Dir(cosign), "cosign", "#!/bin/sh\n[ \"$1\" = public-key ] && cat \"$FAKE_PUB\"\nexit 0\n")
	if err := os.Chmod(cosign, 0o755); err != nil {
		t.Fatal(err)
	}
	b := &randomBuilder{}
	env := map[string]string{"NOTES_PUSH_TOKEN": "t0ken", "STOWAGE_COSIGN": cosign}
	p, err := project.Open(root, filepath.Join(root, "stowage.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	host, _, _ := strings.Cut(p.Spec.Registry.Prefix, "/")
	test := []string{"-C", root, "--test-registry", host + "/test/notes", "--test-push", "publisher:NOTES_PUSH_TOKEN",
		"--test-key", "env://RUN_KEY", "--test-public-key", "keys/test.pub", "ship"}
	t.Setenv("FAKE_PUB", filepath.Join(root, "keys/release.pub"))
	if _, _, err := run(t, b, env, test...); err == nil || !strings.Contains(err.Error(), "--test-key env://RUN_KEY is not the key of --test-public-key keys/test.pub: not signed") || b.n != 0 {
		t.Fatalf("the project's key: %v (%d builds)", err, b.n)
	}
	t.Setenv("FAKE_PUB", filepath.Join(root, "keys/test.pub"))
	if _, _, err := run(t, b, env, test...); err != nil && strings.Contains(err.Error(), "--test-key") || b.n == 0 {
		t.Fatalf("the run's key: %v, %d builds", err, b.n)
	}
}

// Registry prefixes compare as the registry names them — Docker Hub's hosts as one, the default
// HTTPS port and a trailing dot dropped — and one within the other overlaps it.
func TestRegistryOverlaps(t *testing.T) {
	t.Parallel()
	for _, same := range [][2]string{
		{"docker.io/acme/notes", "registry-1.docker.io/acme/notes"},
		{"index.docker.io/acme", "registry.hub.docker.com/acme"},
		{"ghcr.io/acme/notes", "ghcr.io:443/acme/notes"},
		{"ghcr.io/acme/notes", "ghcr.io./acme/notes"},
		{"GHCR.io/acme", "ghcr.io/acme"},
		{"ghcr.io/acme/notes", "ghcr.io/acme/notes/test"},
		{"ghcr.io/acme/notes", "ghcr.io/acme"},
		{"ghcr.io/acme/notes", "ghcr.io"},
		{"localhost:5000", "localhost:5000/notes"},
		{"ghcr.io/acme/notes", "ghcr.io:0443/acme/notes"},
		{"localhost:5000/notes", "localhost:05000/notes"},
		{"localhost/notes", "localhost:80/notes"},
		{"[fd00::5]/acme", "[fd00::5]:443/acme"},
		{"[fd00:0::5]/acme", "[fd00::5]/acme"},
		{"[::ffff:127.0.0.1]:5000", "127.0.0.1:5000"},
		{"localhost:5000/acme", "127.0.0.1:5000/acme"},
		{"localhost:5000/acme", "[::1]:5000/acme"},
		{"localhost:5000/acme", "notes.localhost:5000/acme"},
		{"127.0.0.2:5000/acme", "localhost:5000/acme"},
	} {
		if !overlaps(registrySegments(same[0]), registrySegments(same[1])) || !overlaps(registrySegments(same[1]), registrySegments(same[0])) {
			t.Errorf("%s and %s do not overlap", same[0], same[1])
		}
	}
	for _, other := range [][2]string{
		{"ghcr.io/acme/notes", "ghcr.io/acme/notes-test"},
		{"ghcr.io/acme/notes", "ghcr.io/acme-test/notes"},
		{"localhost:5000", "localhost:5001"},
		{"localhost:5000/a", "localhost/a"},
		{"docker.io/acme", "ghcr.io/acme"},
		{"[fd00::5]/acme", "[fd00::6]/acme"},
		{"localhost:5000", "localhost:05001"},
	} {
		if overlaps(registrySegments(other[0]), registrySegments(other[1])) {
			t.Errorf("%s and %s overlap", other[0], other[1])
		}
	}
}

// A project key that cannot be read could be the one given: the test release is refused.
func TestTestReleaseRefusesAnUnreadProjectKey(t *testing.T) {
	t.Parallel()
	p := trProject(t, true, false)
	if err := os.Remove(filepath.Join(p.Root, "keys/release.pub")); err != nil {
		t.Fatal(err)
	}
	err := (testRelease{Registry: "localhost:5000/notes", Key: "env://RUN", Public: "keys/test.pub"}).apply(p)
	if err == nil || !strings.Contains(err.Error(), "signing.public_keys") || !strings.Contains(err.Error(), "checked against the project's") {
		t.Fatalf("%v", err)
	}
}

// A project key that does not parse refuses too; a PKCS#1 key, which installs trust, is a key.
func TestTestReleaseKeyForms(t *testing.T) {
	t.Parallel()
	p := trProject(t, true, false)
	p.Spec.Signing.Config["public_keys"] = []any{"keys/not-a-key.pub"}
	if err := (testRelease{Registry: "localhost:5000/notes", Key: "env://RUN", Public: "keys/test.pub"}).apply(p); err == nil || !strings.Contains(err.Error(), "signing.public_keys keys/not-a-key.pub: not a PEM public key") {
		t.Errorf("an unparsed project key: %v", err)
	}
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p = trProject(t, true, false)
	testutil.Write(t, p.Root, "keys/rsa.pub", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&k.PublicKey)})))
	if err := (testRelease{Registry: "localhost:5000/notes", Key: "env://RUN", Public: "keys/rsa.pub"}).apply(p); err != nil {
		t.Errorf("a PKCS#1 key: %v", err)
	}
}

// The overlap of two prefixes is a relation on what they name: reflexive, symmetric, and the same
// whatever spelling of a host (case, a trailing dot, the default port) is given.
func FuzzRegistryOverlaps(f *testing.F) {
	for _, s := range [][2]string{{"ghcr.io/acme/notes", "ghcr.io/acme"}, {"localhost:5000", "127.0.0.1:5000/x"}, {"[fd00::5]:443/a", "docker.io/a"}} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		// apply reads a prefix the grammar accepts, and none other.
		if !lint.RegistryPrefix(a) || !lint.RegistryPrefix(b) {
			return
		}
		sa, sb := registrySegments(a), registrySegments(b)
		if !overlaps(sa, sa) {
			t.Fatalf("%q does not overlap itself", a)
		}
		if overlaps(sa, sb) != overlaps(sb, sa) {
			t.Fatalf("%q and %q: not symmetric", a, b)
		}
		if h, _, _ := strings.Cut(a, "/"); foldHost(foldHost(h)) != foldHost(h) {
			t.Fatalf("%q: folded twice %q, once %q", h, foldHost(foldHost(h)), foldHost(h))
		}
		host, path, _ := strings.Cut(a, "/")
		for _, v := range []string{strings.ToUpper(host), host + ".", host + ":443"} {
			if strings.ContainsAny(host, ":[") || strings.HasSuffix(host, ".") || host == "" {
				break
			}
			if !overlaps(sa, registrySegments(v+"/"+path)) && path != "" {
				t.Fatalf("%q and %q name one registry", a, v+"/"+path)
			}
		}
	})
}
