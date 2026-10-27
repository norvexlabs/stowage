package install

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/sbom"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// The VM runs no audit: an operator audits a release from their own machine, from the registry.
func TestTheVMServesNoAudit(t *testing.T) {
	t.Parallel()
	_, d, _, _ := backupVM(t)
	for _, h := range []http.Handler{d.Handler(), d.routes(true)} {
		if code := call(t, h, "POST", "/v1/audit", AuditOptions{}, nil); code != 404 && code != 405 {
			t.Errorf("POST /v1/audit: %d", code)
		}
	}
}

// The scanner: the one loaded by tag for an install made offline (never pulled), else pulled by
// digest when missing.
func TestTheScannerIsTheReleasesOwn(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	// The scanner in a registry: read with the registry client, loaded into the engine.
	srv := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	prefix := strings.TrimPrefix(srv.URL, "http://") + "/acme/notes"
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := img.Digest()
	dst, _ := name.NewDigest(prefix + "/notes-audit@" + h.String())
	if err := remote.Write(dst, img); err != nil {
		t.Fatal(err)
	}
	e.r.Manifest.Artifacts.Images[sbom.Key] = h.String()
	m := e.r.Manifest
	in := e.installer(nil)
	c := registry.New(authn.NewMultiKeychain())
	cf, _ := img.ConfigFile()
	var layers []string
	for _, d := range cf.RootFS.DiffIDs {
		layers = append(layers, `"`+d.String()+`"`)
	}
	asLoaded := `{"ID":"sha256:loaded","Layers":[` + strings.Join(layers, ",") + `],"Config":{}}`
	local := scannerLocal(prefix, "notes", h.Hex)
	if other := scannerLocal(prefix+"x", "notes", h.Hex); other == local || !strings.HasSuffix(local, "/notes-audit:"+h.Hex[:16]) {
		t.Fatalf("local names %s and %s", local, other)
	}
	// test/lib.sh's scanner_fresh computes it so: printf %s <prefix> | sha256sum | cut -c1-8.
	if got := scannerLocal("127.0.0.1:5000/notes", "notes", h.Hex); got != "stowage.local/c29920cd/notes-audit:"+h.Hex[:16] {
		t.Errorf("%s", got)
	}
	src := &auditSource{m: m, prefix: prefix, loaded: prefix, c: c}
	e.docker.loadAs, e.docker.loadInspect = local, asLoaded
	ref, err := in.scannerImage(context.Background(), src)
	if err != nil || ref != "sha256:loaded" || e.docker.loadedBytes == 0 {
		t.Fatalf("loaded: %s %v (%d bytes)", ref, err, e.docker.loadedBytes)
	}
	e.docker.loadedBytes = 0
	if ref, err := in.scannerImage(context.Background(), src); err != nil || ref != "sha256:loaded" || e.docker.loadedBytes != 0 {
		t.Errorf("loaded already: %s %v", ref, err)
	}
	// Another image under its name is loaded over; a load that leaves another one is refused.
	e.docker.loaded[local] = `{"ID":"sha256:other","Layers":["sha256:other"]}`
	if ref, err := in.scannerImage(context.Background(), src); err != nil || ref != "sha256:loaded" || e.docker.loadedBytes == 0 {
		t.Errorf("another image under its name: %s %v", ref, err)
	}
	e.docker.loadInspect = `{"ID":"sha256:other","Layers":["sha256:other"]}`
	e.docker.loaded[local] = e.docker.loadInspect
	if _, err := in.scannerImage(context.Background(), src); err == nil || !strings.Contains(err.Error(), "not the release's scanner") {
		t.Errorf("a load that leaves another image: %v", err)
	}
	// Another command, working directory or user is another image too.
	e.docker.loaded[local] = strings.Replace(asLoaded, `"Config":{}`, `"Config":{"User":"root"}`, 1)
	e.docker.loadInspect = asLoaded
	if ref, err := in.scannerImage(context.Background(), src); err != nil || ref != "sha256:loaded" {
		t.Errorf("another user under its name, loaded over: %s %v", ref, err)
	}
	// A daemon that refuses the load: its reason, not the pipe's.
	delete(e.docker.loaded, local)
	e.docker.loadErr = errors.New("exit status 1: no space left on device")
	if _, err := in.scannerImage(context.Background(), src); err == nil || !strings.Contains(err.Error(), "no space left on device") {
		t.Errorf("a refused load: %v", err)
	}
	e.docker.loadErr = nil
	// Offline: the one loaded by tag under the install's own prefix, whatever --registry reads,
	// checked as well.
	off := &auditSource{m: m, prefix: prefix, loaded: "ghcr.io/acme/notes", c: c, offline: true}
	byTag := "ghcr.io/acme/notes/notes-audit:" + m.Tag
	if _, err := in.scannerImage(context.Background(), off); err == nil || !strings.Contains(err.Error(), "not loaded") {
		t.Errorf("offline, not loaded: %v", err)
	}
	e.docker.loaded[byTag] = asLoaded
	if ref, err := in.scannerImage(context.Background(), off); err != nil || ref != "sha256:loaded" {
		t.Errorf("offline, loaded by tag: %s %v", ref, err)
	}
	e.docker.loaded[byTag] = `{"ID":"sha256:x","Layers":[],"Config":{"Entrypoint":["sh"]}}`
	if _, err := in.scannerImage(context.Background(), off); err == nil || !strings.Contains(err.Error(), "not the release's scanner") {
		t.Errorf("offline, another image under its tag: %v", err)
	}
	e.r.Keys = map[string][]byte{"release.pub": testutil.NewKeySigner(t).PEM}
	offIn := e.installer(func(o *Options) { o.Offline = true })
	if _, err := offIn.Audit(context.Background(), AuditOptions{Version: "1.1.0"}); err == nil || !strings.Contains(err.Error(), "pulls no image") {
		t.Errorf("offline, another release: %v", err)
	}
	e.r.Keys = nil
	if _, err := in.Audit(context.Background(), AuditOptions{}); err == nil || !strings.Contains(err.Error(), "does not sign its releases") {
		t.Errorf("an unsigned project: %v", err)
	}
	// On an operator's machine (no install), --version needs a channel when the installer's release
	// names none (the VM's OS carries one without).
	e.r.Keys = map[string][]byte{"release.pub": testutil.NewKeySigner(t).PEM}
	e.r.Manifest.Channel = ""
	if _, err := in.Audit(context.Background(), AuditOptions{Version: "1.1.0"}); err == nil || !strings.Contains(err.Error(), "--channel is required") {
		t.Errorf("no channel: %v", err)
	}
}

// An OS bundle or a disk the registry does not hold (a mirror exported without them) is left
// out, logged; any other artifact is read whether or not it is there.
func TestAnAuditReadsWhatAMirrorCarries(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	srv := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	prefix := strings.TrimPrefix(srv.URL, "http://") + "/acme/notes"
	push := func(repo string) string {
		img, err := random.Image(64, 1)
		if err != nil {
			t.Fatal(err)
		}
		h, _ := img.Digest()
		dst, _ := name.NewDigest(prefix + "/notes-" + repo + "@" + h.String())
		if err := remote.Write(dst, img); err != nil {
			t.Fatal(err)
		}
		return h.String()
	}
	m := *e.r.Manifest
	gone := "sha256:" + strings.Repeat("e", 64)
	m.Artifacts.Images = map[string]string{"api": gone}
	m.Artifacts.Installer, m.Artifacts.OS = gone, push("os")
	m.Artifacts.Disks = map[string]string{"qcow2": push("disk"), "raw": gone}
	src := &auditSource{m: &m, prefix: prefix, c: registry.New(authn.NewMultiKeychain())}
	in := e.installer(nil)
	targets, err := in.carried(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tg := range targets {
		names = append(names, tg.Name)
	}
	if got := strings.Join(names, " "); got != "api installer os disk-qcow2" {
		t.Errorf("targets %s", got)
	}
	if !strings.Contains(e.log.String(), "disk-raw sha256:eeeeeeeeeeee is not in "+prefix+" (a mirror carries the shapes and the disk formats its export named): not read") {
		t.Errorf("log %s", e.log)
	}
	m.Artifacts.OS = gone
	if targets, err = in.carried(context.Background(), src); err != nil || len(targets) != 3 {
		t.Errorf("without the OS bundle: %v %v", targets, err)
	}
	// The project's own registry holds the whole release: nothing is left out, its error reported.
	e.r.Spec.Registry.Prefix = prefix
	if targets, err = in.carried(context.Background(), src); err != nil || len(targets) != 5 {
		t.Errorf("the project's registry: %v %v", targets, err)
	}
}

// An install recorded by tag was made offline; the VM, preloaded, is not; another project's
// directory is refused.
func TestAuditOfflineAndProject(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	in := e.installer(nil)
	env := func(lines ...string) *EnvFile {
		path := filepath.Join(t.TempDir(), ".env")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := ReadEnvFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	byTag, byDigest := env("STOWAGE_IMAGE_API='r/notes-api:stable-1.0.0'"), env("STOWAGE_IMAGE_API='r/notes-api@sha256:"+strings.Repeat("a", 64)+"'")
	if !in.auditOffline(byTag, nil) || in.auditOffline(byDigest, nil) || in.auditOffline(nil, os.ErrNotExist) {
		t.Error("offline as the install's images are recorded")
	}
	if !e.installer(func(o *Options) { o.Offline = true }).auditOffline(nil, os.ErrNotExist) {
		t.Error("--offline")
	}
	if err := os.MkdirAll(e.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, envName), []byte("STOWAGE_NAME='atlas'\nSTOWAGE_PROJECT_VERSION='1'\nSTOWAGE_REVISION='1'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Audit(context.Background(), AuditOptions{}); err == nil || !strings.Contains(err.Error(), `holds an install of "atlas"`) {
		t.Errorf("another project's directory: %v", err)
	}
}

// A VM's disk is the app's space and Kairos's; an OVA's never under the capacity it declares.
func TestDiskGiB(t *testing.T) {
	t.Parallel()
	b := bootDisk{osMiB: map[string]int64{"qcow2": 23000, "ova": 23000}}
	if got := b.diskGiB("qcow2", 20); got != 43 {
		t.Errorf("qcow2: %d", got)
	}
	if got := b.diskGiB("ova", 20); got != 64 {
		t.Errorf("ova: %d", got)
	}
	if got := b.diskGiB("ova", 50); got != 73 {
		t.Errorf("ova, larger: %d", got)
	}
	// An OVA declaring more than 64 GiB (Kairos's 60000 MiB and 8 GiB beyond): never under it.
	big := bootDisk{osMiB: map[string]int64{"ova": 60000}}
	if got := big.diskGiB("ova", 4); got != (60000+8192+1023)/1024 {
		t.Errorf("ova, a large bundle: %d", got)
	}
}

// The trust of an audit: an install whose record does not read is refused (its keys unknown),
// and so is a --registry that names no registry.
func TestAuditRefusesWhatItCannotTrust(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.r.Keys = map[string][]byte{"release.pub": testutil.NewKeySigner(t).PEM}
	if err := os.MkdirAll(e.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, envName), []byte("not KEY=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.installer(nil).Audit(context.Background(), AuditOptions{}); err == nil || !strings.Contains(err.Error(), e.dir) {
		t.Errorf("an unreadable record: %v", err)
	}
	f := newEnv(t)
	f.r.Keys = e.r.Keys
	for _, bad := range []string{"mirror", "mirror.example/Bad"} {
		in := f.installer(func(o *Options) { o.Registry = bad })
		if _, err := in.Audit(context.Background(), AuditOptions{}); err == nil || !strings.Contains(err.Error(), "--registry") {
			t.Errorf("--registry %s: %v", bad, err)
		}
	}
}

// The OS's share of a disk rounds up, with the slack, on both sides of a GiB.
func TestOSGiBRoundsUp(t *testing.T) {
	t.Parallel()
	for mib, want := range map[int64]int{1024 - diskSlackMiB: 1, 1025 - diskSlackMiB: 2, 2048 - diskSlackMiB: 2, 2049 - diskSlackMiB: 3} {
		b := bootDisk{osMiB: map[string]int64{"raw": mib}}
		if got := b.osGiB("raw"); got != want {
			t.Errorf("%d MiB: %d GiB, want %d", mib, got, want)
		}
	}
}
