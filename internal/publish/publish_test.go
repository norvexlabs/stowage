package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/norvexlabs/stowage/internal/backup"
	"github.com/norvexlabs/stowage/internal/front"
	"github.com/norvexlabs/stowage/internal/kairos"
	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// fakeBuilder returns a fresh random image per build and records what it built, and the files of
// a build context (the OS bundle's) by their path in it.
type fakeBuilder struct {
	built   []BuildRequest
	context map[string][]byte
}

func (f *fakeBuilder) Build(_ context.Context, _ *project.Project, r BuildRequest) (v1.Image, func(), error) {
	f.built = append(f.built, r)
	if r.Context != "" {
		f.context = map[string][]byte{}
		err := filepath.WalkDir(r.Context, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(r.Context, path)
			f.context[filepath.ToSlash(rel)], err = os.ReadFile(path)
			return err
		})
		if err != nil {
			return nil, func() {}, err
		}
	}
	img, err := amd64Image(256, 1)
	return img, func() {}, err
}

func (f *fakeBuilder) names() []string {
	var out []string
	for _, r := range f.built {
		out = append(out, r.Image)
	}
	return out
}

// fakeDisks writes a small file per format, different for each bundle, and records what it made.
type fakeDisks struct {
	dir      string // a test's temporary directory
	made     [][]string
	requests []kairos.DiskRequest
	fail     bool
}

func (f *fakeDisks) Build(_ context.Context, r kairos.DiskRequest) (map[string]string, func(), error) {
	if f.fail {
		return nil, func() {}, errors.New("auroraboot failed")
	}
	f.made, f.requests = append(f.made, r.Formats), append(f.requests, r)
	// As auroraboot, the bundle is read from the source with the request's account.
	ref, err := name.NewDigest(r.Source)
	if err != nil {
		return nil, nil, err
	}
	if _, err := remote.Head(ref, remote.WithAuth(authn.FromConfig(r.Auth))); err != nil {
		return nil, nil, fmt.Errorf("read the bundle: %w", err)
	}
	dir, err := os.MkdirTemp(f.dir, "disks-")
	if err != nil {
		return nil, nil, err
	}
	out := map[string]string{}
	for _, fm := range r.Formats {
		out[fm] = filepath.Join(dir, r.Base+"."+kairos.Ext[fm])
		if err := os.WriteFile(out[fm], []byte(fm+" of "+ref.DigestStr()), 0o600); err != nil {
			return nil, nil, err
		}
	}
	return out, func() { os.RemoveAll(dir) }, nil
}

type env struct {
	t       *testing.T
	disks   *fakeDisks
	root    string
	host    string
	reg     *registry.Client
	builder *fakeBuilder
	log     *bytes.Buffer
	now     time.Time
	// failBlobs makes the registry answer 403 to every blob read; failDelete refuses deleting
	// tags containing it.
	failBlobs  bool
	failDelete string
	// private are path fragments of repositories that refuse a request without an account; set,
	// the registry asks for one (Basic), and anon is the client that reads with none.
	private []string
	anon    *registry.Client
	// privateAs404 answers a refused read as an unknown repository, as some registries do.
	privateAs404 bool
	signer       *fakeSigner // set by sign()
	// installer is the installer executable; pull the baked pull account.
	installer []byte
	pull      *payload.Account
}

// fakeSigner records signatures by digest reference.
type fakeSigner struct {
	sigs  map[string]bool
	signs int
	fail  bool
	// untrusted signs with a key whose public half is not trusted: nothing verifies.
	untrusted bool
	// down makes every check fail without an answer (registry or key store outage).
	down bool
	// downOn makes checks of the digests containing it fail the same way.
	downOn string
	onSign func(d name.Digest) // runs before a signature is recorded
}

func (f *fakeSigner) Sign(_ context.Context, d name.Digest) error {
	if f.fail {
		return errors.New("key unavailable")
	}
	if f.onSign != nil {
		f.onSign(d)
	}
	if !f.untrusted {
		f.sigs[d.String()] = true
	}
	f.signs++
	return nil
}

func (f *fakeSigner) Verify(_ context.Context, d name.Digest) error {
	if f.down || (f.downOn != "" && strings.Contains(d.String(), f.downOn)) {
		return errors.New("registry unreachable")
	}
	if !f.sigs[d.String()] {
		return fmt.Errorf("%s: %w", d, ErrUnsigned)
	}
	return nil
}

// sign turns signing on in the project and gives the publisher a signer.
func (e *env) sign() {
	e.t.Helper()
	testutil.Write(e.t, e.root, "keys/release.pub", readFile(e.t, "../../examples/full/keys/release-2026.pub"))
	testutil.Write(e.t, e.root, "stowage.yaml", strings.Replace(readFile(e.t, e.root+"/stowage.yaml"), "images:",
		"signing: { provider: cosign-key, key: env://SIGNING_KEY, public_keys: [keys/release.pub] }\nimages:", 1))
	e.signer = &fakeSigner{sigs: map[string]bool{}}
}

func (e *env) signedRef(repo, digest string) bool {
	return e.signer.sigs[e.host+"/acme/notes/"+repo+"@"+digest]
}

// newEnv copies the minimal example, points its registry at an in-process registry and pins its
// upstream postgres image to one pushed there.
func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, installer: []byte("#!/bin/sh\necho installer\n")}
	reg := ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e.failDelete != "" && r.Method == http.MethodDelete && strings.Contains(r.URL.Path, e.failDelete) {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		if e.private != nil && r.Header.Get("Authorization") == "" &&
			(r.URL.Path == "/v2/" || slices.ContainsFunc(e.private, func(f string) bool { return strings.Contains(r.URL.Path, f) })) {
			if e.privateAs404 && r.URL.Path != "/v2/" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"errors":[{"code":"NAME_UNKNOWN","message":"repository name not known to registry"}]}`)
				return
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if e.failBlobs && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/") {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	root := testutil.CopyExample(t, "minimal")
	testutil.SetYAML(t, root, "stowage.yaml", []string{"registry", "prefix"}, host+"/acme/notes")
	up, err := amd64Image(128, 1)
	if err != nil {
		t.Fatal(err)
	}
	upTag, _ := name.NewTag(host + "/upstream/postgres:17")
	if err := remote.Write(upTag, up); err != nil {
		t.Fatal(err)
	}
	d, _ := up.Digest()
	testutil.SetYAML(t, root, "stowage.yaml", []string{"images", "postgres"}, map[string]any{"ref": host + "/upstream/postgres@" + d.String()})
	e.root, e.host, e.reg, e.builder, e.log = root, host, registry.New(authn.NewMultiKeychain()) /* no keychain: never the host's Docker login */, &fakeBuilder{}, &bytes.Buffer{}
	e.disks = &fakeDisks{dir: t.TempDir()}
	e.now = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	return e
}

func (e *env) publisher() (*Publisher, string) {
	e.t.Helper()
	p, err := project.Open(e.root, "")
	if err != nil {
		e.t.Fatal(err)
	}
	v, err := p.Version()
	if err != nil {
		e.t.Fatal(err)
	}
	pub := &Publisher{Project: p, Registry: e.reg, Upstream: e.reg, Builder: e.builder, StowageVersion: "0.1.0",
		Installer: e.installer, Pull: e.pull, Disks: e.disks, WorkDir: e.t.TempDir(), Anonymous: e.anon, Now: func() time.Time { return e.now }, Log: e.log}
	if e.signer != nil {
		pub.Signer = e.signer
	}
	return pub, v
}

func (e *env) ship(channel string, revision int) (*Ref, error) {
	e.t.Helper()
	e.builder.built, e.disks.made = nil, nil
	p, v := e.publisher()
	return p.Ship(context.Background(), ShipOptions{Version: v, Channel: channel, Revision: revision})
}

func (e *env) mustShip(channel string, revision int) *Ref {
	e.t.Helper()
	r, err := e.ship(channel, revision)
	if err != nil {
		e.t.Fatalf("ship %s: %v\n%s", channel, err, e.log)
	}
	return r
}

func (e *env) changeWeb() {
	e.t.Helper()
	testutil.Write(e.t, e.root, "web/index.html", readFile(e.t, e.root+"/web/index.html")+"<!-- changed -->\n")
}

func (e *env) bump(v string) {
	e.t.Helper()
	testutil.Write(e.t, e.root, "VERSION", v+"\n")
	testutil.Write(e.t, e.root, "changes/"+v+".md", "### Fixed\n- Release "+v+".\n")
}

func (e *env) digest(repo, tag string) string {
	e.t.Helper()
	t, _ := name.NewTag(e.host + "/acme/notes/" + repo + ":" + tag)
	d, err := e.reg.Head(context.Background(), t)
	if err != nil {
		e.t.Fatal(err)
	}
	if d == nil {
		return ""
	}
	return d.Digest.String()
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func TestShipPublishesThenSkipsEverything(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.mustShip("stable", 1)
	eq(t, "built", strings.Join(e.builder.names(), ","), "api,web,front,backup,os")
	for _, r := range e.builder.built[:4] {
		eq(t, "recipe of "+r.Image, string(r.Recipe), map[string]string{front.Key: string(front.Dockerfile), backup.Key: string(backup.Dockerfile)}[r.Image])
	}
	m := r.Manifest
	eq(t, "tag", m.Tag, "1.0.0")
	eq(t, "seq", m.ReleaseSeq, 1)
	eq(t, "channel", m.Channel, "stable")
	eq(t, "changelog", strings.Contains(m.Changelog.Project, "First release"), true)
	eq(t, "images", len(m.Artifacts.Images), 5)
	for img, d := range m.Artifacts.Images {
		ref, _ := name.NewDigest(e.host + "/acme/notes/notes-" + img + "@" + d)
		pulled, err := remote.Image(ref)
		if err != nil {
			t.Fatal(err)
		}
		cfg, _ := pulled.ConfigName()
		eq(t, "config of "+img, m.Artifacts.Configs[img], cfg.String())
		md, _ := pulled.Digest()
		eq(t, "manifest of "+img, m.Artifacts.Manifests[img], md.String())
		eq(t, "image "+img, e.digest("notes-"+img, "1.0.0"), d)
	}
	eq(t, "pointer", e.digest("notes-release", "stable"), r.Digest.String())
	eq(t, "build passed the version to the image that reads it", e.builder.built[0].Version, "1.0.0")
	eq(t, "and not to the one that does not", e.builder.built[1].Version, "")
	if strings.Contains(e.log.String(), "not published by this stowage yet") {
		t.Fatalf("a node reported skipped:\n%s", e.log)
	}

	again := e.mustShip("stable", 1)
	eq(t, "rebuilt", len(e.builder.built), 0)
	eq(t, "release digest", again.Digest, r.Digest)
	eq(t, "date kept", again.Manifest.Date.Equal(m.Date), true)
}

func TestVersionBumpRebuildsOnlyWhatReadsIt(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := e.mustShip("stable", 1)
	e.bump("1.1.0")
	e.now = e.now.Add(time.Hour)
	second := e.mustShip("stable", 1)
	eq(t, "built", strings.Join(e.builder.names(), ","), "api,os")
	eq(t, "web re-tagged", second.Manifest.Artifacts.Images["web"], first.Manifest.Artifacts.Images["web"])
	eq(t, "postgres re-tagged", second.Manifest.Artifacts.Images["postgres"], first.Manifest.Artifacts.Images["postgres"])
	eq(t, "api rebuilt", second.Manifest.Artifacts.Images["api"] != first.Manifest.Artifacts.Images["api"], true)
	eq(t, "seq", second.Manifest.ReleaseSeq, 2)
	eq(t, "pointer", e.digest("notes-release", "stable"), second.Digest.String())

	// Re-shipping the older version leaves the pointer on the later release.
	e.bump("1.0.0")
	testutil.Write(t, e.root, "changes/1.0.0.md", "### Added\n- First release: notes with attachments, one administrator created at install.\n")
	old := e.mustShip("stable", 1)
	eq(t, "older release unchanged", old.Digest, first.Digest)
	eq(t, "pointer not moved back", e.digest("notes-release", "stable"), second.Digest.String())
}

func TestImmutableChannelRefusesNewInputsUnderAnExistingTag(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := e.mustShip("stable", 1)
	before := e.digest("notes-web", "1.0.0")
	e.changeWeb()
	_, err := e.ship("stable", 1)
	if err == nil || !strings.Contains(err.Error(), "--revision 2") {
		t.Fatalf("want a refusal naming --revision 2, got %v", err)
	}
	eq(t, "nothing built", len(e.builder.built), 0)
	eq(t, "web tag untouched", e.digest("notes-web", "1.0.0"), before)
	eq(t, "release untouched", e.digest("notes-release", "1.0.0"), first.Digest.String())

	r2 := e.mustShip("stable", 2)
	eq(t, "tag", r2.Manifest.Tag, "1.0.0-r2")
	eq(t, "built", strings.Join(e.builder.names(), ","), "web,os")
	eq(t, "pointer on the revision", e.digest("notes-release", "stable"), r2.Digest.String())
	eq(t, "seq", r2.Manifest.ReleaseSeq, 2)
}

// An image tag left by a publication that never completed (no release under its tag) is replaced.
func TestALeftoverImageTagWithoutAReleaseIsReplaced(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	img, _ := amd64Image(64, 1)
	stray, _ := name.NewTag(e.host + "/acme/notes/notes-web:1.0.0")
	if err := remote.Write(stray, img); err != nil {
		t.Fatal(err)
	}
	r := e.mustShip("stable", 1)
	eq(t, "web replaced", e.digest("notes-web", "1.0.0"), r.Manifest.Artifacts.Images["web"])
}

// Under a published immutable release, an image tag that moved is refused, and nothing is written:
// every image is planned first.
func TestImmutableImageTagConflictWritesNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := e.mustShip("stable", 1)
	img, _ := amd64Image(64, 1)
	stray, _ := name.NewTag(e.host + "/acme/notes/notes-web:1.0.0")
	if err := remote.Write(stray, img); err != nil {
		t.Fatal(err)
	}
	strayDigest := e.digest("notes-web", "1.0.0")
	_, err := e.ship("stable", 1)
	if err == nil || !strings.Contains(err.Error(), "notes-web:1.0.0 exists with other inputs") {
		t.Fatalf("got %v", err)
	}
	eq(t, "nothing built", len(e.builder.built), 0)
	eq(t, "web not restored", e.digest("notes-web", "1.0.0"), strayDigest)
	eq(t, "release untouched", e.digest("notes-release", "1.0.0"), first.Digest.String())
}

func TestMutableChannelOverwrites(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := e.mustShip("beta", 1)
	eq(t, "tag", first.Manifest.Tag, "beta-1.0.0")
	e.changeWeb()
	second := e.mustShip("beta", 1)
	eq(t, "built", strings.Join(e.builder.names(), ","), "web,os")
	eq(t, "new content, new number", second.Manifest.ReleaseSeq, first.Manifest.ReleaseSeq+1)
	eq(t, "pointer", e.digest("notes-release", "beta"), second.Digest.String())
	eq(t, "web overwritten", e.digest("notes-web", "beta-1.0.0"), second.Manifest.Artifacts.Images["web"])
}

func TestVerifyCatchesAMovedTag(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mustShip("stable", 1)
	p, _ := e.publisher()
	if _, err := p.Verify(context.Background(), "stable", "", 1); err != nil {
		t.Fatalf("positive control: %v", err)
	}
	img, _ := amd64Image(64, 1)
	moved, _ := name.NewTag(e.host + "/acme/notes/notes-web:1.0.0")
	if err := remote.Write(moved, img); err != nil {
		t.Fatal(err)
	}
	_, err := p.Verify(context.Background(), "stable", "", 1)
	if err == nil || !strings.Contains(err.Error(), "notes-web:1.0.0 is sha256:") {
		t.Fatalf("got %v", err)
	}
	_, err = p.Verify(context.Background(), "stable", "1.0.0", 1)
	if err == nil {
		t.Fatal("verify by version passed on a moved tag")
	}
}

func TestPromoteRetagsWithoutBuilding(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	beta := e.mustShip("beta", 1)
	p, v := e.publisher()
	e.builder.built = nil
	st, err := p.Promote(context.Background(), v, "beta", "stable", 1)
	if err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	eq(t, "built", len(e.builder.built), 0)
	eq(t, "tag", st.Manifest.Tag, "1.0.0")
	for img, d := range beta.Manifest.Artifacts.Images {
		eq(t, "image "+img, st.Manifest.Artifacts.Images[img], d)
		eq(t, "tag "+img, e.digest("notes-"+img, "1.0.0"), d)
	}
	eq(t, "stable pointer", e.digest("notes-release", "stable"), st.Digest.String())

	// A tree that differs from the promoted release cannot be promoted: nothing would match.
	e.changeWeb()
	p, v = e.publisher()
	_, err = p.Promote(context.Background(), v, "beta", "stable", 2)
	if err == nil {
		t.Fatal("promote of a missing revision passed")
	}
	if _, err := p.Ship(context.Background(), ShipOptions{Version: v, Channel: "stable", Revision: 2, NoBuild: true, Sources: []*Ref{beta}}); err == nil ||
		!strings.Contains(err.Error(), "no published image has these inputs") {
		t.Fatalf("got %v", err)
	}
}

func TestYankMovesThePointerBackAndNeverDeletes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := e.mustShip("stable", 1)
	e.bump("1.1.0")
	second := e.mustShip("stable", 1)
	p, _ := e.publisher()
	ctx := context.Background()
	if err := p.Yank(ctx, "stable", "1.1.0", 1, "data loss on upgrade"); err != nil {
		t.Fatal(err)
	}
	eq(t, "pointer back", e.digest("notes-release", "stable"), first.Digest.String())
	eq(t, "images kept", e.digest("notes-api", "1.1.0"), second.Manifest.Artifacts.Images["api"])
	rs, err := p.Releases(ctx, "stable")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "releases", len(rs), 2)
	eq(t, "order", rs[1].Manifest.ProjectVersion, "1.1.0")
	eq(t, "yank recorded", rs[1].Manifest.Yanked != nil && rs[1].Manifest.Yanked.Reason == "data loss on upgrade", true)

	// Shipping the yanked release again does not offer it again, and says so.
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "is yanked") {
		t.Fatalf("re-ship of a yanked release: %v", err)
	}
	eq(t, "pointer stays back", e.digest("notes-release", "stable"), first.Digest.String())

	if err := p.Yank(ctx, "stable", "1.0.0", 1, "also bad"); err != nil {
		t.Fatal(err)
	}
	eq(t, "pointer removed", e.digest("notes-release", "stable"), "")
	if _, err := p.Verify(ctx, "stable", "", 1); err == nil {
		t.Fatal("verify passed on a channel without a pointer")
	}
}

func TestPruneDeletesOnlyMutableTags(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mustShip("stable", 1)
	old := e.mustShip("beta", 1)
	e.bump("1.1.0")
	cur := e.mustShip("beta", 1)
	p, _ := e.publisher()
	ctx := context.Background()
	if _, err := p.Prune(ctx, "stable", ""); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("got %v", err)
	}
	pruned, err := p.Prune(ctx, "beta", "")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "pruned", strings.Join(pruned, ","), "beta-1.0.0")
	eq(t, "beta image gone", e.digest("notes-api", "beta-1.0.0"), "")
	eq(t, "stable image kept (same digest)", e.digest("notes-web", "1.0.0"), old.Manifest.Artifacts.Images["web"])
	eq(t, "pointer kept", e.digest("notes-release", "beta"), cur.Digest.String())

	pruned, err = p.Prune(ctx, "beta", "1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "pruned", strings.Join(pruned, ","), "beta-1.1.0")
	eq(t, "pointer removed", e.digest("notes-release", "beta"), "")
	eq(t, "stable untouched", e.digest("notes-release", "stable") != "", true)
}

func TestSigningConfiguredRefusesToShipUnsigned(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.sign()
	e.signer = nil
	_, err := e.ship("stable", 1)
	if err == nil || !strings.Contains(err.Error(), "unsigned") {
		t.Fatalf("got %v", err)
	}
	eq(t, "nothing pushed", e.digest("notes-api", "1.0.0"), "")
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPinnedDigestAcceptsDockerForms(t *testing.T) {
	t.Parallel()
	const d = "sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
	for ref, repo := range map[string]string{
		"busybox:1.37@" + d:                  "index.docker.io/library/busybox",
		"busybox@" + d:                       "index.docker.io/library/busybox",
		"ghcr.io/acme/base:1@" + d:           "ghcr.io/acme/base",
		"localhost:5000/up/postgres:17@" + d: "localhost:5000/up/postgres",
	} {
		got, err := pinnedDigest(ref)
		if err != nil || got.DigestStr() != d || got.Context().Name() != repo {
			t.Errorf("%s: %v %v", ref, got, err)
		}
	}
	if _, err := pinnedDigest("busybox:1.37"); err == nil {
		t.Error("a tag alone was accepted as pinned")
	}
}

// A stowage other than the one stowage.lock pins publishes nothing: its installer and front differ.
func TestANewerPublisherDoesNotRewriteARelease(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := e.mustShip("stable", 1)
	p, v := e.publisher()
	p.StowageVersion = "9.9.9"
	if _, err := p.Ship(context.Background(), ShipOptions{Version: v, Channel: "stable", Revision: 1}); err == nil ||
		!strings.Contains(err.Error(), "stowage.lock pins stowage 0.1.0, this is 9.9.9") || !strings.Contains(err.Error(), "stowage lock --update") {
		t.Fatalf("got %v", err)
	}
	if _, err := p.Promote(context.Background(), v, "stable", "beta", 1); err == nil || !strings.Contains(err.Error(), "this is 9.9.9: promote with stowage 0.1.0") {
		t.Fatalf("promote: %v", err)
	}
	p.StowageVersion = "0.0.9"
	if _, err := p.Ship(context.Background(), ShipOptions{Version: v, Channel: "stable", Revision: 1}); err == nil ||
		!strings.Contains(err.Error(), "this is an older 0.0.9: publish with stowage 0.1.0") || strings.Contains(err.Error(), "--update") {
		t.Fatalf("an older stowage: %v", err)
	}
	p.StowageVersion = "9.9.9"
	// Moving the pin changes the release's inputs: the immutable channel asks for a new revision.
	p.Project.Lock.Stowage = "9.9.9"
	if _, err := p.Ship(context.Background(), ShipOptions{Version: v, Channel: "stable", Revision: 1}); err == nil || !strings.Contains(err.Error(), "--revision 2") {
		t.Fatalf("moved pin: %v", err)
	}
	p.StowageVersion = "dev"
	if _, err := p.Ship(context.Background(), ShipOptions{Version: v, Channel: "stable", Revision: 1}); err == nil || !strings.Contains(err.Error(), "not a release build") {
		t.Fatalf("a dev build: %v", err)
	}
	p.StowageVersion = "9.9.9"
	p.Project.Lock = nil
	if _, err := p.Ship(context.Background(), ShipOptions{Version: v, Channel: "stable", Revision: 1}); err == nil || !strings.Contains(err.Error(), "stowage.lock is missing") {
		t.Fatalf("without a lock: %v", err)
	}
	p, _ = e.publisher()
	again, err := p.Verify(context.Background(), "stable", v, 1)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "release digest", again.Digest, first.Digest)
	eq(t, "no beta release", e.digest("notes-api", "beta-1.0.0"), "")
}

type failingBuilder struct{ fakeBuilder }

func (f *failingBuilder) Build(ctx context.Context, p *project.Project, r BuildRequest) (v1.Image, func(), error) {
	if r.Image == "web" {
		return nil, func() {}, errors.New("boom")
	}
	return f.fakeBuilder.Build(ctx, p, r)
}

// A build that fails leaves every tag where it was, even on a mutable channel that overwrites.
func TestAFailedBuildMovesNoTag(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := e.mustShip("beta", 1)
	e.changeWeb()
	testutil.Write(t, e.root, "api/notes-api", readFile(t, e.root+"/api/notes-api")+"# changed\n")
	p, v := e.publisher()
	p.Builder = &failingBuilder{}
	if _, err := p.Ship(context.Background(), ShipOptions{Version: v, Channel: "beta", Revision: 1}); err == nil {
		t.Fatal("ship passed with a failing build")
	}
	eq(t, "api tag", e.digest("notes-api", "beta-1.0.0"), first.Manifest.Artifacts.Images["api"])
	if _, err := p.Verify(context.Background(), "beta", "", 1); err != nil {
		t.Fatalf("the channel no longer verifies: %v", err)
	}
}

func TestYankWorksOnABrokenRelease(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mustShip("stable", 1)
	gone, _ := name.NewTag(e.host + "/acme/notes/notes-web:1.0.0")
	if err := remote.Delete(gone); err != nil {
		t.Fatal(err)
	}
	p, _ := e.publisher()
	if err := p.Yank(context.Background(), "stable", "1.0.0", 1, "broken"); err != nil {
		t.Fatal(err)
	}
	eq(t, "pointer removed", e.digest("notes-release", "stable"), "")
}

func TestNewContentUnderAMutableTagIsNotYanked(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mustShip("beta", 1)
	p, _ := e.publisher()
	if err := p.Yank(context.Background(), "beta", "1.0.0", 1, "broken"); err != nil {
		t.Fatal(err)
	}
	e.changeWeb()
	r := e.mustShip("beta", 1)
	eq(t, "yank dropped", r.Manifest.Yanked == nil, true)
	eq(t, "pointer on the fix", e.digest("notes-release", "beta"), r.Digest.String())
}

func TestFallbackSkipsAnIncompleteRelease(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mustShip("stable", 1)
	e.bump("1.1.0")
	e.mustShip("stable", 1)
	gone, _ := name.NewTag(e.host + "/acme/notes/notes-web:1.0.0")
	if err := remote.Delete(gone); err != nil {
		t.Fatal(err)
	}
	p, _ := e.publisher()
	if err := p.Yank(context.Background(), "stable", "1.1.0", 1, "bad"); err != nil {
		t.Fatal(err)
	}
	eq(t, "no fallback to the incomplete 1.0.0", e.digest("notes-release", "stable"), "")
}

func TestPruneOfAnUnknownVersionFails(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mustShip("beta", 1)
	p, _ := e.publisher()
	if _, err := p.Prune(context.Background(), "beta", "2.0.0"); err == nil || !strings.Contains(err.Error(), "no release of 2.0.0") {
		t.Fatalf("got %v", err)
	}
	if _, err := p.Prune(context.Background(), "beta", "v1.0.0"); err == nil {
		t.Fatal("prune accepted a string the scheme refuses")
	}
}

func TestReleasesSkipsAForeignTag(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mustShip("stable", 1)
	img, _ := amd64Image(16, 1)
	stray, _ := name.NewTag(e.host + "/acme/notes/notes-release:sha256-abc.sig")
	if err := remote.Write(stray, img); err != nil {
		t.Fatal(err)
	}
	p, _ := e.publisher()
	rs, err := p.Releases(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "releases", len(rs), 1)
	eq(t, "warned", strings.Contains(e.log.String(), "skipping"), true)
	e.bump("1.1.0")
	eq(t, "seq still counted", e.mustShip("stable", 1).Manifest.ReleaseSeq, 2)
}

func TestASeqIsNeverReusedAfterAPrune(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mustShip("beta", 1)
	e.bump("1.1.0")
	r := e.mustShip("beta", 1)
	eq(t, "seq", r.Manifest.ReleaseSeq, 2)
	p, _ := e.publisher()
	if _, err := p.Prune(context.Background(), "beta", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	e.bump("1.2.0")
	eq(t, "seq after prune", e.mustShip("beta", 1).Manifest.ReleaseSeq, 3)
}

// A pointer an interrupted yank left on the yanked release is replaced by the next ship, even by
// an earlier version.
func TestShipReplacesAPointerLeftOnAYankedRelease(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mustShip("stable", 1)
	e.bump("1.1.0")
	stale := e.mustShip("stable", 1)
	p, _ := e.publisher()
	m := *stale.Manifest
	m.Yanked = &release.Yank{Reason: "bad", Date: e.now}
	if _, err := p.writeRelease(context.Background(), m.Tag, &m); err != nil { // yank, then "crash"
		t.Fatal(err)
	}
	eq(t, "pointer still stale", e.digest("notes-release", "stable"), stale.Digest.String())
	e.bump("1.0.1")
	fix := e.mustShip("stable", 1)
	eq(t, "pointer on the fix", e.digest("notes-release", "stable"), fix.Digest.String())
}

func TestReleasesSkipsAVersionTheSchemeNoLongerReads(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := e.mustShip("stable", 1)
	p, _ := e.publisher()
	m := *first.Manifest
	m.ProjectVersion, m.Tag = "not-a-version", "not-a-version"
	if _, err := p.writeRelease(context.Background(), m.Tag, &m); err != nil {
		t.Fatal(err)
	}
	rs, err := p.Releases(context.Background(), "stable")
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "releases", len(rs), 1)
}

// After a scheme change, a pointer the new scheme cannot read does not keep the channel stuck.
func TestPointerOnAVersionTheSchemeNoLongerReadsMoves(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := e.mustShip("stable", 1)
	p, _ := e.publisher()
	m := *first.Manifest
	m.ProjectVersion, m.Tag = "legacy", "legacy"
	old, err := p.writeRelease(context.Background(), m.Tag, &m)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.movePointer(context.Background(), "stable", &Ref{&m, old}); err != nil {
		t.Fatal(err)
	}
	e.bump("1.1.0")
	r := e.mustShip("stable", 1)
	eq(t, "pointer moved", e.digest("notes-release", "stable"), r.Digest.String())
}

// A release that could not be read fails the listing: skipping it could hand out its number again
// or fall back past it.
func TestReleasesFailsOnAReadError(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mustShip("stable", 1)
	p, _ := e.publisher()
	e.failBlobs = true
	if _, err := p.Releases(context.Background(), ""); err == nil {
		t.Fatal("a release that could not be read was skipped")
	}
}

// Promote refuses a tree that differs from the published release in anything — here the
// changelog, which no image reads — before writing anything.
func TestPromoteRefusesATreeThatChangedSinceTheRelease(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(e *env){
		"changelog": func(e *env) {
			testutil.Write(e.t, e.root, "changes/1.0.0.md", "### Added\n- Rewritten after beta.\n")
		},
		"installer executable": func(e *env) { e.installer = []byte("another stowage") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.mustShip("beta", 1)
			change(e)
			p, v := e.publisher()
			if _, err := p.Promote(context.Background(), v, "beta", "stable", 1); err == nil || !strings.Contains(err.Error(), "the release inputs differ from beta-1.0.0 as published") ||
				!strings.Contains(err.Error(), "check out its commit; the installer's executable") {
				t.Fatalf("got %v", err)
			}
			eq(t, "no stable image tag", e.digest("notes-api", "1.0.0"), "")
			p.StowageVersion = "0.2.0"
			p.Project.Lock.Stowage = "0.2.0" // as stowage lock --update leaves it
			if _, err := p.Promote(context.Background(), v, "beta", "stable", 1); err == nil || !strings.Contains(err.Error(), "published by stowage 0.1.0, this is 0.2.0") {
				t.Fatalf("another stowage: %v", err)
			}
		})
	}
}

// A published immutable release whose image disappeared (registry retention) is not republished
// with a rebuilt image.
func TestImmutableReleaseIsNeverRepublishedWithOtherImages(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := e.mustShip("stable", 1)
	gone, _ := name.NewTag(e.host + "/acme/notes/notes-web:1.0.0")
	gc, _ := name.NewDigest(e.host + "/acme/notes/notes-web@" + first.Manifest.Artifacts.Images["web"])
	for _, ref := range []name.Reference{gone, gc} { // retention removed the tag and the digest
		if err := remote.Delete(ref); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "--revision 2") {
		t.Fatalf("got %v", err)
	}
	eq(t, "release untouched", e.digest("notes-release", "1.0.0"), first.Digest.String())
}

// A prune that fails midway still moves the pointer off a release that lost tags.
func TestAFailedPruneStillMovesThePointer(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mustShip("beta", 1)
	cur := e.mustShip("beta", 2)
	e.failDelete = "notes-release/manifests/beta-1.0.0-r2"
	p, _ := e.publisher()
	// Revision 1 is pruned first (order of the listing), then revision 2 — the pointer's — fails.
	if _, err := p.Prune(context.Background(), "beta", "1.0.0"); err == nil {
		t.Fatal("prune passed a refused delete")
	}
	eq(t, "pointer off the half-pruned release", e.digest("notes-release", "beta") != cur.Digest.String(), true)
}

func TestASignedProjectSignsEveryArtifactBeforeItsTagMoves(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.sign()
	e.signer.onSign = func(d name.Digest) {
		repo := strings.TrimPrefix(d.Context().RepositoryStr(), "acme/notes/")
		tag := "1.0.0"
		if strings.HasPrefix(repo, "charts/") {
			tag = "0.1.0"
		}
		if repo == "notes-disk" {
			tag = "1.0.0-qcow2"
		}
		if got := e.digest(repo, tag); got == d.DigestStr() {
			t.Errorf("%s:%s already held %s when it was signed", repo, tag, d.DigestStr())
		}
	}
	r := e.mustShip("stable", 1)
	for img, d := range r.Manifest.Artifacts.Images {
		eq(t, "image "+img+" signed", e.signedRef("notes-"+img, d), true)
	}
	eq(t, "installer signed", e.signedRef("notes-installer", r.Manifest.Artifacts.Installer), true)
	eq(t, "chart signed", e.signedRef("charts/notes-stable", r.Manifest.Artifacts.Chart.Digest), true)
	eq(t, "manifest signed", e.signedRef("notes-release", r.Digest.String()), true)
	eq(t, "os bundle signed", e.signedRef("notes-os", r.Manifest.Artifacts.OS), true)
	eq(t, "disk signed", e.signedRef("notes-disk", r.Manifest.Artifacts.Disks["qcow2"]), true)
	eq(t, "signatures", e.signer.signs, 10)

	e.mustShip("stable", 1)
	eq(t, "a re-ship signs nothing", e.signer.signs, 10)
	e.signer.onSign = nil
	delete(e.signer.sigs, e.host+"/acme/notes/notes-release@"+r.Digest.String())
	eq(t, "a lost signature is made again", e.mustShip("stable", 1).Digest, r.Digest)
	eq(t, "signatures", e.signer.signs, 11)
	p, _ := e.publisher()
	if _, err := p.Verify(context.Background(), "stable", "", 1); err != nil {
		t.Fatal(err)
	}

	delete(e.signer.sigs, e.host+"/acme/notes/notes-web@"+r.Manifest.Artifacts.Images["web"])
	_, err := p.Verify(context.Background(), "stable", "", 1)
	if err == nil || !strings.Contains(err.Error(), "notes-web@") || !strings.Contains(err.Error(), "no signature") {
		t.Fatalf("verify of an unsigned image: %v", err)
	}
	e.signer.sigs[e.host+"/acme/notes/notes-web@"+r.Manifest.Artifacts.Images["web"]] = true
	delete(e.signer.sigs, e.host+"/acme/notes/notes-installer@"+r.Manifest.Artifacts.Installer)
	if _, err := p.Verify(context.Background(), "stable", "", 1); err == nil || !strings.Contains(err.Error(), "notes-installer@") {
		t.Fatalf("verify of an unsigned installer: %v", err)
	}
	e.signer.sigs[e.host+"/acme/notes/notes-installer@"+r.Manifest.Artifacts.Installer] = true
	delete(e.signer.sigs, e.host+"/acme/notes/charts/notes-stable@"+r.Manifest.Artifacts.Chart.Digest)
	if _, err := p.Verify(context.Background(), "stable", "", 1); err == nil || !strings.Contains(err.Error(), "charts/notes-stable@") {
		t.Fatalf("verify of an unsigned chart: %v", err)
	}
	// The chart carries the trusted keys.
	keys := chartValues(t, e.chartAt("stable", "0.1.0")).PublicKeys
	eq(t, "public keys", len(keys) == 1 && keys[0] == readFile(t, e.root+"/keys/release.pub"), true)
}

func TestASigningFailureMovesNoTag(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.sign()
	e.signer.fail = true
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "key unavailable") {
		t.Fatalf("got %v", err)
	}
	for _, repo := range []string{"notes-api", "notes-web", "notes-postgres", "notes-release"} {
		eq(t, repo+" tag", e.digest(repo, "1.0.0"), "")
	}
	eq(t, "pointer", e.digest("notes-release", "stable"), "")
}

func TestTurningSigningOnNeedsANewRevision(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := e.mustShip("stable", 1)
	e.sign()
	// The public keys are inputs of what installs, so the release changes: a new revision.
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "--revision 2") {
		t.Fatalf("got %v", err)
	}
	r := e.mustShip("stable", 2)
	eq(t, "images kept", r.Manifest.Artifacts.Images["api"], first.Manifest.Artifacts.Images["api"])
	eq(t, "kept image signed", e.signedRef("notes-api", r.Manifest.Artifacts.Images["api"]), true)
	eq(t, "manifest signed", e.signedRef("notes-release", r.Digest.String()), true)
	eq(t, "pointer", e.digest("notes-release", "stable"), r.Digest.String())
}

func TestAYankedManifestIsSignedBeforeItsTagMoves(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.sign()
	e.mustShip("stable", 1)
	p, v := e.publisher()
	if err := p.Yank(context.Background(), "stable", v, 1, "bad"); err != nil {
		t.Fatal(err)
	}
	d := e.digest("notes-release", "1.0.0")
	eq(t, "yanked manifest signed", e.signedRef("notes-release", d), true)

	e.signer.fail = true
	before := e.digest("notes-release", "1.0.0")
	e.bump("1.1.0")
	e.signer.fail = false
	r := e.mustShip("stable", 1)
	e.signer.fail = true
	if err := p.Yank(context.Background(), "stable", "1.1.0", 1, "bad"); err == nil {
		t.Fatal("a yank that cannot be signed succeeded")
	}
	eq(t, "1.1.0 tag unchanged", e.digest("notes-release", "1.1.0"), r.Digest.String())
	eq(t, "1.0.0 untouched", e.digest("notes-release", "1.0.0"), before)
}

func TestAKeyOutsideThePublicKeysMovesNoTag(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.sign()
	e.signer.untrusted = true
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "signing.public_keys") {
		t.Fatalf("got %v", err)
	}
	for _, repo := range []string{"notes-api", "notes-web", "notes-postgres", "notes-release"} {
		eq(t, repo+" tag", e.digest(repo, "1.0.0"), "")
	}
}

func TestAFailureToCheckSignaturesNeverMovesThePointer(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.sign()
	e.mustShip("stable", 1)
	e.bump("1.1.0")
	r := e.mustShip("stable", 1)
	// The live release cannot be checked; the earlier one could be fallen back to.
	e.signer.downOn = r.Digest.String()
	p, _ := e.publisher()
	if err := p.retreatPointer(context.Background(), "stable"); err == nil || !strings.Contains(err.Error(), "registry unreachable") {
		t.Fatalf("retreat during an outage: %v", err)
	}
	eq(t, "pointer kept", e.digest("notes-release", "stable"), r.Digest.String())
	if _, err := p.Verify(context.Background(), "stable", "", 1); err == nil || errors.Is(err, ErrUnsigned) {
		t.Fatalf("verify during an outage: %v", err)
	}
	// Positive control: a real missing signature does move it back.
	e.signer.downOn = ""
	delete(e.signer.sigs, e.host+"/acme/notes/notes-api@"+r.Manifest.Artifacts.Images["api"])
	if err := p.retreatPointer(context.Background(), "stable"); err != nil {
		t.Fatal(err)
	}
	eq(t, "pointer back on 1.0.0", e.digest("notes-release", "stable"), e.digest("notes-release", "1.0.0"))
}

func TestTheFallbackNeverMovesToAReleaseItCouldNotCheck(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.sign()
	first := e.mustShip("stable", 1)
	e.bump("1.1.0")
	r := e.mustShip("stable", 1)
	delete(e.signer.sigs, e.host+"/acme/notes/notes-api@"+r.Manifest.Artifacts.Images["api"])
	e.signer.downOn = first.Digest.String()
	p, _ := e.publisher()
	if err := p.retreatPointer(context.Background(), "stable"); err == nil || !strings.Contains(err.Error(), "registry unreachable") {
		t.Fatalf("got %v", err)
	}
	eq(t, "pointer kept", e.digest("notes-release", "stable"), r.Digest.String())
}

func TestAnOutageWhileShippingSignsNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.sign()
	e.signer.down = true
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "registry unreachable") {
		t.Fatalf("got %v", err)
	}
	eq(t, "signatures", e.signer.signs, 0)
}

func TestAVanishedImageIsIncompleteNotAFailureToCheck(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.sign()
	first := e.mustShip("stable", 1)
	e.bump("1.1.0")
	r := e.mustShip("stable", 1)
	api := r.Manifest.Artifacts.Images["api"]
	tag, _ := name.NewTag(e.host + "/acme/notes/notes-api:1.1.0")
	if err := remote.Delete(tag); err != nil {
		t.Fatal(err)
	}
	// cosign cannot read a digest the registry no longer has: that must not abort the fallback.
	e.signer.downOn = api
	p, _ := e.publisher()
	if err := p.retreatPointer(context.Background(), "stable"); err != nil {
		t.Fatal(err)
	}
	eq(t, "pointer back on 1.0.0", e.digest("notes-release", "stable"), first.Digest.String())
}

func TestAManifestNamingNoReferenceIsIncomplete(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.mustShip("stable", 1)
	m := *r.Manifest
	m.Artifacts.Images = map[string]string{"Not A Key": "sha256:x"}
	p, _ := e.publisher()
	if err := p.verifyRelease(context.Background(), &Ref{&m, r.Digest}, m.Tag); !incomplete(err) {
		t.Fatalf("got %v", err)
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
