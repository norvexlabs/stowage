package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"

	"github.com/norvexlabs/stowage/internal/cloud"
	"github.com/norvexlabs/stowage/internal/graph"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/publish"
	"github.com/norvexlabs/stowage/internal/spec"
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
	if !strings.Contains(out, "notes 1.0.0 revision 1 · channel beta · tag beta-1.0.0 · seq 1") || b.n != 5 {
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
	if b.n != 5 {
		t.Fatalf("promote built %d images", b.n-5)
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

// OpenTofu, which registers the cloud images, gets the clouds' credentials and never the registry
// accounts' tokens, cosign's secrets or Stowage's own settings.
func TestCloudEnvHidesStowagesSecrets(t *testing.T) {
	for k, v := range map[string]string{"REG_PUSH": "p", "REG_PULL": "q", "SIGN_KEY": "k", "COSIGN_PASSWORD": "c",
		"STOWAGE_GCP_BUCKET": "b", "GOOGLE_APPLICATION_CREDENTIALS": "/key.json", "AWS_SECRET_ACCESS_KEY": "s", "TF_DATA_DIR": "/x", "TF_CLI_ARGS_apply": "-target=x",
		"VAULT_TOKEN": "v"} {
		t.Setenv(k, v)
	}
	token := func(env string) spec.Provider {
		return spec.Provider{Name: "token", Config: map[string]any{"user": "u", "token_env": env}}
	}
	p := &project.Project{Spec: &spec.Spec{
		Registry: spec.Registry{Push: token("REG_PUSH"), Pull: token("REG_PULL")},
		Signing:  &spec.Provider{Name: "cosign-key", Config: map[string]any{"key": "env://SIGN_KEY", "public_keys": []string{"k.pub"}}},
	}}
	got := map[string]bool{}
	for _, kv := range cloudEnv(p) {
		k, _, _ := strings.Cut(kv, "=")
		got[k] = true
	}
	// Vault's token is a cloud's own business unless it signs.
	if !got["VAULT_TOKEN"] {
		t.Error("VAULT_TOKEN hidden with an env:// key")
	}
	p.Spec.Signing.Config["key"] = "hashivault://release"
	for _, kv := range cloudEnv(p) {
		if strings.HasPrefix(kv, "VAULT_TOKEN=") {
			t.Error("VAULT_TOKEN given to OpenTofu with a hashivault:// key")
		}
	}
	for _, k := range []string{"REG_PUSH", "REG_PULL", "SIGN_KEY", "COSIGN_PASSWORD", "STOWAGE_GCP_BUCKET", "TF_DATA_DIR", "TF_CLI_ARGS_apply"} {
		if got[k] {
			t.Errorf("%s given to OpenTofu", k)
		}
	}
	for _, k := range []string{"GOOGLE_APPLICATION_CREDENTIALS", "AWS_SECRET_ACCESS_KEY", "PATH"} {
		if !got[k] {
			t.Errorf("%s not given to OpenTofu", k)
		}
	}
}

// A cloud's settings turn its registration on: some only are refused, and so is a missing tofu,
// before anything is built.
func TestCloudSettings(t *testing.T) {
	p := &project.Project{Spec: &spec.Spec{Shapes: spec.Shapes{Kairos: &spec.KairosShape{CloudPublish: []string{"aws", "gcp"}}}}}
	env := map[string]string{"STOWAGE_GCP_PROJECT": "p"}
	o := &options{getenv: func(k string) string { return env[k] }}
	if _, _, err := o.clouds(p, t.TempDir()); err == nil || !strings.Contains(err.Error(), "set STOWAGE_GCP_BUCKET too") {
		t.Errorf("gcp partly set: %v", err)
	}
	env["STOWAGE_GCP_BUCKET"] = "b"
	t.Setenv("PATH", t.TempDir())
	if _, _, err := o.clouds(p, t.TempDir()); err == nil || !strings.Contains(err.Error(), "tofu is not on PATH") {
		t.Errorf("no tofu: %v", err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "tofu"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	up, vars, err := o.clouds(p, t.TempDir())
	if u, ok := up.(*cloud.Uploader); err != nil || !ok || u.Tofu == nil || vars["aws"] != nil || vars["gcp"]["bucket"] != "b" {
		t.Errorf("gcp set: %v, %v, %v", up, vars, err)
	}
	delete(env, "STOWAGE_GCP_PROJECT")
	delete(env, "STOWAGE_GCP_BUCKET")
	if up, _, err := o.clouds(p, t.TempDir()); up != nil || err != nil {
		t.Errorf("none set: %v, %v", up, err)
	}
}
