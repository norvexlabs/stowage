package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docker/cli/cli/config"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"

	"github.com/norvexlabs/stowage/internal/graph"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/publish"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/testutil"
)

type randomBuilder struct {
	n    int
	seen func(*project.Project) // when set, sees the project of each build
}

func (b *randomBuilder) Build(_ context.Context, p *project.Project, _ publish.BuildRequest) (v1.Image, func(), error) {
	b.n++
	if b.seen != nil {
		b.seen(p)
	}
	img, err := amd64Image(64, 1)
	return img, func() {}, err
}

// registryProject is the minimal example publishing to an in-process registry with a token, its
// upstream postgres image swapped for one built (the stand-in builder cannot mirror Docker Hub).
func registryProject(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	root := testutil.CopyExample(t, "minimal")
	host := strings.TrimPrefix(srv.URL, "http://")
	testutil.SetYAML(t, root, "stowage.yaml", []string{"registry", "prefix"}, host+"/acme/notes")
	testutil.SetYAML(t, root, "stowage.yaml", []string{"registry", "push"}, map[string]any{"provider": "token", "user": "publisher", "token_env": "NOTES_PUSH_TOKEN"})
	testutil.SetYAML(t, root, "stowage.yaml", []string{"images", "postgres"}, map[string]any{"context": "api", "dockerfile": "api/Dockerfile"})
	// The OS bundle and its disks are publish's tests' (a Docker build of Kairos is no unit test).
	testutil.SetYAML(t, root, "stowage.yaml", []string{"shapes", "kairos"}, nil)
	return root
}

func run(t *testing.T, b *randomBuilder, env map[string]string, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	o := &options{buildVersion: "0.1.0", getenv: func(k string) string { return env[k] }, builder: b, installer: []byte("exe"),
		now: func() time.Time { return time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC) }, out: &out, err: &errOut}
	cmd := newRoot(o)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

func TestPublishCommands(t *testing.T) {
	root := registryProject(t)
	b := &randomBuilder{}
	env := map[string]string{"NOTES_PUSH_TOKEN": "t0ken"}

	testutil.Edit(t, root, "deploy/compose.yaml", "${LOG_LEVEL:-info}", "${LOG_LEVEL}")
	if _, logs, err := run(t, b, env, "-C", root, "ship"); err == nil || !strings.Contains(err.Error(), "lint: 1 error(s)") || !strings.Contains(logs, "LOG_LEVEL") || b.n != 0 {
		t.Fatalf("ship with a lint error: %v (%d builds)\n%s", err, b.n, logs)
	}
	testutil.Edit(t, root, "deploy/compose.yaml", "${LOG_LEVEL}", "${LOG_LEVEL:-info}")
	if _, _, err := run(t, b, map[string]string{}, "-C", root, "ship"); err == nil || !strings.Contains(err.Error(), "NOTES_PUSH_TOKEN is not set") {
		t.Fatalf("ship without a token: %v", err)
	}
	out, logs, err := run(t, b, env, "-C", root, "ship", "--channel", "beta")
	if err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	if !strings.Contains(out, "notes 1.0.0 revision 1 · channel beta · tag beta-1.0.0 · seq 1") || b.n != 4 {
		t.Fatalf("ship output (%d builds):\n%s", b.n, out)
	}
	// Read-only commands use the pull account (here anonymous): no publisher token needed.
	if out, _, err := run(t, b, map[string]string{}, "-C", root, "verify", "--channel", "beta"); err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	if _, _, err := runNoBuilder(t, "-C", root, "releases"); err != nil {
		t.Fatalf("releases without HOME: %v", err)
	}
	if _, _, err := run(t, b, map[string]string{}, "-C", root, "releases"); err != nil {
		t.Fatalf("releases without the publisher token: %v", err)
	}
	if _, _, err := run(t, b, env, "-C", root, "promote", "1.0.0", "--from", "beta", "--to", "stable"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if b.n != 4 {
		t.Fatalf("promote built %d images", b.n-4)
	}
	if _, _, err := run(t, b, env, "-C", root, "promote", "0.9.0", "--from", "beta", "--to", "stable"); err == nil || !strings.Contains(err.Error(), "the tree is at 1.0.0") {
		t.Fatalf("promote of another version: %v", err)
	}
	out, _, err = run(t, b, env, "-C", root, "releases", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rs []struct {
		Manifest struct {
			Channel string `json:"channel"`
			Tag     string `json:"tag"`
		} `json:"manifest"`
	}
	if err := json.Unmarshal([]byte(out), &rs); err != nil || len(rs) != 2 || rs[0].Manifest.Tag != "beta-1.0.0" || rs[1].Manifest.Tag != "1.0.0" {
		t.Fatalf("releases: %v\n%s", err, out)
	}
	if _, _, err := run(t, b, env, "-C", root, "yank", "1.0.0"); err == nil || !strings.Contains(err.Error(), "--reason") {
		t.Fatalf("yank without a reason: %v", err)
	}
	if _, _, err := run(t, b, env, "-C", root, "yank", "1.0.0", "--channel", "stable", "--reason", "bad"); err != nil {
		t.Fatalf("yank: %v", err)
	}
	if _, _, err := run(t, b, env, "-C", root, "verify", "--channel", "stable"); err == nil {
		t.Fatal("verify passed on a channel whose only release is yanked")
	}
	if _, _, err := run(t, b, env, "-C", root, "prune"); err == nil || !strings.Contains(err.Error(), "--channel") {
		t.Fatalf("prune without a channel: %v", err)
	}
	if out, _, err := run(t, b, env, "-C", root, "prune", "--channel", "beta", "1.0.0"); err != nil || strings.TrimSpace(out) != "beta-1.0.0" {
		t.Fatalf("prune: %v\n%s", err, out)
	}
}

// runNoBuilder runs with the production builder selection (none injected).
func runNoBuilder(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	o := &options{buildVersion: "0.1.0", getenv: func(string) string { return "" }, now: time.Now, out: &out, err: &errOut}
	cmd := newRoot(o)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

func TestKeyURIsAreNotProjectFiles(t *testing.T) {
	for key, uri := range map[string]bool{
		"env://SIGNING_KEY": true, "hashivault://release": true, "awskms:///alias/release": true,
		"pkcs11:token=release;slot-id=0": true, "keys/release.key": false, "release.key": false,
	} {
		if keyURI.MatchString(key) != uri {
			t.Errorf("%s: uri = %v", key, !uri)
		}
	}
}

func TestReleasesNeedsNoSigner(t *testing.T) {
	root := registryProject(t)
	b := &randomBuilder{}
	if _, logs, err := run(t, b, map[string]string{"NOTES_PUSH_TOKEN": "t0ken"}, "-C", root, "ship", "--channel", "beta"); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	// Signing on, with no public key on disk, no cosign and no cache directory for its config:
	// listing still works, checking does not.
	testutil.Edit(t, root, "stowage.yaml", "images:", "signing: { provider: cosign-key, key: env://K, public_keys: [keys/gone.pub] }\nimages:")
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	if out, _, err := run(t, b, map[string]string{"STOWAGE_COSIGN": "/nonexistent/cosign"}, "-C", root, "releases"); err != nil || !strings.Contains(out, "beta-1.0.0") {
		t.Fatalf("releases on a signed project: %v\n%s", err, out)
	}
	if _, _, err := run(t, b, map[string]string{"STOWAGE_COSIGN": "/nonexistent/cosign"}, "-C", root, "verify", "--channel", "beta"); err == nil {
		t.Fatal("verify passed without being able to check a signature")
	}
}

// amd64Image is a random image whose config says linux/amd64, like every image ship builds.
func amd64Image(size, layers int64) (v1.Image, error) {
	img, err := random.Image(size, layers)
	if err != nil {
		return nil, err
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture = "linux", "amd64"
	return mutate.ConfigFile(img, cf)
}

// The pull account's password comes from the publisher's environment: ship refuses without it,
// and it is one of the installer's inputs.
func TestShipBakesThePullAccountFromTheEnvironment(t *testing.T) {
	root := registryProject(t)
	testutil.SetYAML(t, root, "stowage.yaml", []string{"registry", "pull"}, map[string]any{"provider": "token", "user": "pull", "token_env": "NOTES_PULL_TOKEN"})
	b := &randomBuilder{}
	if _, _, err := run(t, b, map[string]string{"NOTES_PUSH_TOKEN": "t0ken"}, "-C", root, "ship", "--channel", "beta"); err == nil ||
		!strings.Contains(err.Error(), "NOTES_PULL_TOKEN is not set: the pull account's password is baked into the installer") {
		t.Fatalf("ship without the pull password: %v", err)
	}
	installer := func(pw string) string {
		t.Helper()
		out, logs, err := run(t, b, map[string]string{"NOTES_PULL_TOKEN": pw}, "-C", root, "plan", "--json")
		if err != nil {
			t.Fatalf("%v\n%s", err, logs)
		}
		var g graph.Graph
		if err := json.Unmarshal([]byte(out), &g); err != nil {
			t.Fatal(err)
		}
		n, _ := g.Node(graph.KindInstaller)
		return n.Inputs
	}
	if installer("a") == installer("b") {
		t.Fatal("the pull password does not reach the installer's inputs")
	}
	if _, logs, err := run(t, b, nil, "-C", root, "plan"); err != nil || !strings.Contains(logs, "NOTES_PULL_TOKEN is not set: the installer's inputs leave out") {
		t.Fatalf("plan without the pull password: %v\n%s", err, logs)
	}
	if _, logs, err := runNoBuilder(t, "-C", root, "plan"); err != nil || !strings.Contains(logs, "this stowage carries no installer") {
		t.Fatalf("plan without an installer: %v\n%s", err, logs)
	}
	if _, logs, err := run(t, b, nil, "-C", root, "plan"); err != nil || strings.Contains(logs, "stowage.lock") {
		t.Fatalf("plan with the pinned stowage: %v\n%s", err, logs)
	}
	testutil.Edit(t, root, "stowage.lock", "stowage: 0.1.0", "stowage: 0.0.9")
	if _, logs, err := run(t, b, nil, "-C", root, "plan"); err != nil || !strings.Contains(logs, "warning: stowage.lock pins stowage 0.0.9, this is 0.1.0") {
		t.Fatalf("plan with another pin: %v\n%s", err, logs)
	}
	if _, _, err := run(t, b, map[string]string{"NOTES_PUSH_TOKEN": "t0ken", "NOTES_PULL_TOKEN": "a"}, "-C", root, "ship", "--channel", "beta"); err == nil || !strings.Contains(err.Error(), "pins stowage 0.0.9") {
		t.Fatalf("ship with another pin: %v", err)
	}
	testutil.Edit(t, root, "stowage.lock", "stowage: 0.0.9", "stowage: 0.1.0")
	if _, logs, err := run(t, b, map[string]string{"NOTES_PUSH_TOKEN": "t0ken", "NOTES_PULL_TOKEN": "a"}, "-C", root, "ship", "--channel", "beta"); err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
}

// The disk builder's account is registry.pull's, for the bundle's repository, resolved by ship
// (never by opening a publisher, so yank and prune need no pull token); never the user's default
// Docker config, nor an account sharing a secret with the publisher's.
func TestPullAuthForTheDiskBuilder(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	auths := func(entries string) string {
		dir := t.TempDir()
		testutil.Write(t, dir, "config.json", `{"auths":{`+entries+`}}`)
		return dir
	}
	entry := func(key, user, pw string) string {
		return `"` + key + `":{"auth":"` + base64.StdEncoding.EncodeToString([]byte(user+":"+pw)) + `"}`
	}
	setPush := func(v map[string]any) { testutil.SetYAML(t, root, "stowage.yaml", []string{"registry", "push"}, v) }
	setPull := func(v map[string]any) { testutil.SetYAML(t, root, "stowage.yaml", []string{"registry", "pull"}, v) }
	repo, _ := name.NewRepository("registry.example.org/acme/notes-os")
	env := map[string]string{}
	o := &options{getenv: func(k string) string { return env[k] }}
	open := func() *project.Project {
		t.Helper()
		p, err := project.Open(root, "")
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	auth := func() (authn.AuthConfig, error) {
		t.Helper()
		p := open()
		push, err := registry.Keychain(p.Spec.Registry.Push, o.getenv)
		if err != nil {
			t.Fatal(err)
		}
		return o.pullAuth(p, push)(repo)
	}
	refused := func(what, want string) {
		t.Helper()
		if _, err := auth(); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", what, err)
		}
	}

	pushDir := auths(entry("registry.example.org", "publisher", "s3cret"))
	setPush(map[string]any{"provider": "docker-config", "config": pushDir})
	setPull(map[string]any{"provider": "docker-config", "config": pushDir})
	refused("the same Docker config", "holds a secret of the publisher's account")

	// The publisher's secret under another user name: a registry may ignore the name.
	setPull(map[string]any{"provider": "docker-config", "config": auths(entry("registry.example.org", "pull", "s3cret"))})
	refused("the push secret under another user", "holds a secret of the publisher's account")
	env["PUSH_TOKEN"], env["PULL_TOKEN"] = "t0k", "t0k"
	setPush(map[string]any{"provider": "token", "user": "publisher", "token_env": "PUSH_TOKEN"})
	setPull(map[string]any{"provider": "token", "user": "pull", "token_env": "PULL_TOKEN"})
	refused("two variables holding one token", "holds a secret of the publisher's account")

	// Push credentials scoped to the repository are the ones compared.
	setPush(map[string]any{"provider": "docker-config", "config": auths(entry("registry.example.org/acme/notes-os", "publisher", "scoped") + "," + entry("registry.example.org", "other", "host"))})
	setPull(map[string]any{"provider": "docker-config", "config": auths(entry("registry.example.org", "pull", "scoped"))})
	refused("a repository-scoped push secret", "holds a secret of the publisher's account")

	// The user's default Docker config, unnamed or named: refused.
	setPull(map[string]any{"provider": "docker-config"})
	refused("the default Docker config", "registry.pull is the default Docker config")
	saved := config.Dir()
	def := auths(entry("registry.example.org", "me", "mine"))
	config.SetDir(def)
	t.Cleanup(func() { config.SetDir(saved) })
	setPull(map[string]any{"provider": "docker-config", "config": def})
	refused("the default Docker config by its path", "registry.pull is the default Docker config")
	config.SetDir(saved)

	// A read-only account of its own: accepted.
	setPull(map[string]any{"provider": "docker-config", "config": auths(entry("registry.example.org", "reader", "r"))})
	if a, err := auth(); err != nil || a.Username != "reader" {
		t.Fatalf("a pull docker-config of its own: %+v, %v", a, err)
	}
	env["PULL_TOKEN"] = "r34d"
	setPull(map[string]any{"provider": "token", "user": "pull", "token_env": "PULL_TOKEN"})
	if a, err := auth(); err != nil || a.Username != "pull" || a.Password != "r34d" {
		t.Fatalf("token pull account: %+v, %v", a, err)
	}
	delete(env, "PULL_TOKEN")
	refused("an unset pull token", "PULL_TOKEN is not set")
	setPull(map[string]any{"provider": "none"})
	if a, err := auth(); err != nil || a != (authn.AuthConfig{}) {
		t.Fatalf("anonymous: %+v, %v", a, err)
	}

	// Opening a publisher needs no pull token.
	setPull(map[string]any{"provider": "token", "user": "pull", "token_env": "PULL_TOKEN"})
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if _, err := o.publisher(open(), writeAccess); err != nil {
		t.Fatalf("publisher without a pull token: %v", err)
	}
}

func TestSecretsReadEveryField(t *testing.T) {
	t.Parallel()
	got := secrets(authn.AuthConfig{Password: "p", IdentityToken: "i", RegistryToken: "r", Auth: base64.StdEncoding.EncodeToString([]byte("u:a"))})
	if strings.Join(got, ",") != "p,i,r,a" {
		t.Errorf("secrets = %v", got)
	}
	if got := secrets(authn.AuthConfig{Username: "u"}); len(got) != 0 {
		t.Errorf("a user name alone: %v", got)
	}
}
