package install

import (
	"bytes"
	"context"
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
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
)

// caFile is a CA certificate file, as --registry-ca takes one.
func caFile(t *testing.T) string {
	t.Helper()
	cert, _ := writeCert(t, time.Now().Add(24*time.Hour), "ca.example.org")
	return cert
}

// withDisks publishes v's release again with its disks, under its tag and its channel's pointer.
func withDisks(t *testing.T, v *vmCreate) *release.Manifest {
	t.Helper()
	c := registry.New(authn.NewMultiKeychain())
	ref, _ := c.Tag(v.prefix+"/notes-release", v.r.Manifest.Tag)
	a, _, err := c.GetArtifact(context.Background(), ref, release.MediaType)
	if err != nil || a == nil {
		t.Fatalf("%v %v", a, err)
	}
	m, err := release.Decode(a.Data)
	if err != nil {
		t.Fatal(err)
	}
	m.Artifacts.Disks = v.r.Manifest.Artifacts.Disks
	osRepo, _ := c.Repo(v.prefix + "/notes-os")
	osd, err := c.PushArtifact(context.Background(), osRepo, registry.Artifact{Type: "application/vnd.example.os", Data: []byte("an OS")})
	if err != nil {
		t.Fatal(err)
	}
	m.Artifacts.OS = osd.DigestStr()
	// The fixture's images name digests no registry holds: real ones, pushed.
	for k := range m.Artifacts.Images {
		img, _ := random.Image(64, 1)
		h, _ := img.Digest()
		ref, _ := c.Digest(v.prefix+"/notes-"+k, h.String())
		if err := remote.Write(ref, img); err != nil {
			t.Fatal(err)
		}
		m.Artifacts.Images[k] = h.String()
	}
	v.putRelease(m.Tag, m)
	v.putRelease(m.Channel, m)
	return m
}

func mirrorRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://") + "/site/notes"
}

func TestMirrorExportImportCarriesTheChannelsLatestRelease(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2", "raw")
	m := withDisks(t, v)
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "notes.oci.tar")
	in := v.installer(nil)
	// A CA file in the directory is an install's, which an export never reads.
	if err := os.WriteFile(filepath.Join(v.dir, RegistryCAFile), []byte("not a CA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := in.MirrorExport(ctx, "", file, []string{"qcow2"}); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	// The release comes last, its channel's tag after it.
	a, err := registry.ReadArchive(file)
	if err != nil {
		t.Fatal(err)
	}
	ms := a.Manifests
	a.Close()
	last := ms[len(ms)-1].Annotations
	if last[registry.RepositoryAnnotation] != "notes-release" || last[registry.RefNameAnnotation] != m.Channel {
		t.Errorf("last: %v", last)
	}
	for i, d := range ms {
		if d.Annotations[registry.RepositoryAnnotation] == "notes-release" {
			for _, e := range ms[i:] {
				if e.Annotations[registry.RepositoryAnnotation] != "notes-release" {
					t.Fatalf("%v comes after the release", e.Annotations)
				}
			}
			break
		}
	}
	if !strings.Contains(v.out2(), "Exported notes 1.0.0 (revision 1, 1.0.0-1) to "+file) {
		t.Errorf("out: %s", v.out)
	}
	if err := in.MirrorExport(ctx, "", file, nil); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("an existing archive: %v", err)
	}
	to := mirrorRegistry(t)
	if err := in.MirrorImport(ctx, file, to+"/", false); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	c := registry.New(authn.NewMultiKeychain())
	for repo, tag := range map[string]string{"notes-release": m.Tag, "notes-installer": m.Tag, "notes-os": m.Tag, "notes-disk": release.DiskTag(m.Tag, "qcow2")} {
		ref, _ := c.Tag(to+"/"+repo, tag)
		if d, err := c.Head(ctx, ref); err != nil || d == nil {
			t.Errorf("%s: %v %v", ref, d, err)
		}
	}
	// The channel's pointer reads as the release; the disk left out is not there.
	ptr, _ := c.Tag(to+"/notes-release", m.Channel)
	if a, _, err := c.GetArtifact(ctx, ptr, release.MediaType); err != nil || a == nil {
		t.Errorf("pointer: %v %v", a, err)
	}
	raw, _ := c.Tag(to+"/notes-disk", release.DiskTag(m.Tag, "raw"))
	if d, err := c.Head(ctx, raw); err != nil || d != nil {
		t.Errorf("the raw disk was exported: %v %v", d, err)
	}
	// The installer is the one published, by digest.
	inst, _ := c.Digest(to+"/notes-installer", m.Artifacts.Installer)
	if d, err := c.Head(ctx, inst); err != nil || d == nil {
		t.Errorf("installer by digest: %v %v", d, err)
	}
}

func TestMirrorRefusals(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	withDisks(t, v)
	ctx := context.Background()
	in := v.installer(nil)
	dir := t.TempDir()
	if err := in.MirrorExport(ctx, "", filepath.Join(dir, "a.tar"), []string{"vhd"}); err == nil || !strings.Contains(err.Error(), "no vhd disk (it has: qcow2)") {
		t.Errorf("unknown format: %v", err)
	}
	if err := in.MirrorExport(ctx, "edge", filepath.Join(dir, "a.tar"), nil); err == nil || !strings.Contains(err.Error(), "the channel edge has no release") {
		t.Errorf("empty channel: %v", err)
	}
	if err := in.MirrorExport(ctx, "Not A Channel", filepath.Join(dir, "a.tar"), nil); err == nil {
		t.Error("exported a channel of no valid name")
	}
	if _, err := os.Stat(filepath.Join(dir, "a.tar")); err == nil {
		t.Error("a refused export left a file")
	}
	file := filepath.Join(dir, "ok.tar")
	if err := in.MirrorExport(ctx, "", file, []string{}); err != nil {
		t.Fatal(err)
	}
	if err := in.MirrorImport(ctx, file, "not a prefix", false); err == nil || !strings.Contains(err.Error(), "--to") {
		t.Errorf("bad --to: %v", err)
	}
	// The mirror's channel never goes back: an archive of an earlier release is refused.
	to := mirrorRegistry(t)
	if err := in.MirrorImport(ctx, file, to, false); err != nil {
		t.Fatal(err)
	}
	c := registry.New(authn.NewMultiKeychain())
	ptr, _ := c.Tag(to+"/notes-release", "stable")
	a, _, err := c.GetArtifact(ctx, ptr, release.MediaType)
	if err != nil || a == nil {
		t.Fatal(err)
	}
	pub, _ := release.Decode(a.Data)
	put := func(tag string, m *release.Manifest) {
		data, err := m.Encode()
		if err != nil {
			t.Fatal(err)
		}
		ref, _ := c.Tag(to+"/notes-release", tag)
		if _, err := c.PutArtifact(ctx, ref, registry.Artifact{Type: release.MediaType, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	// The channel at the same publication: imported again (above). At a later one: refused.
	later := *pub
	later.ProjectVersion, later.Tag, later.ReleaseSeq = "1.1.0", "1.1.0-1", pub.ReleaseSeq+1
	put("stable", &later)
	if err := in.MirrorImport(ctx, file, to, false); err == nil || !strings.Contains(err.Error(), "holds notes 1.1.0 (revision 1, publication 2), after the archive's 1.0.0 (revision 1, publication 1)") {
		t.Errorf("an earlier publication: %v", err)
	}
	// --older imports it all the same (the channel went back where it is published).
	if err := in.MirrorImport(ctx, file, to, true); err != nil {
		t.Errorf("--older: %v", err)
	}
	// Back at the same publication: imported again.
	put("stable", pub)
	if err := in.MirrorImport(ctx, file, to, false); err != nil {
		t.Errorf("the same publication: %v", err)
	}
	// A release the mirror records as yanked is not published there again.
	yanked := *pub
	yanked.Yanked = &release.Yank{Reason: "broken", Date: pub.Date}
	put(pub.Tag, &yanked)
	if err := in.MirrorImport(ctx, file, to, false); err == nil || !strings.Contains(err.Error(), "is yanked in "+to+" (broken)") {
		t.Errorf("a yanked release: %v", err)
	}
	// A yank of another publication of the tag (a rebuild) does not apply.
	yanked.ReleaseSeq = pub.ReleaseSeq + 1
	put(pub.Tag, &yanked)
	if err := in.MirrorImport(ctx, file, to, false); err != nil {
		t.Errorf("a yank of another publication: %v", err)
	}
	// A channel holding another project's release is not moved.
	foreign := *pub
	foreign.Name = "other"
	put("stable", &foreign)
	if err := in.MirrorImport(ctx, file, to, false); err == nil || !strings.Contains(err.Error(), "holds other's release on stable") {
		t.Errorf("another project's channel: %v", err)
	}
	// An archive that also writes another project's repository, whose name starts with ours.
	admin, _ := c.Tag(v.prefix+"/notes-admin-release", "stable")
	ad, err := c.PutArtifact(ctx, admin, registry.Artifact{Type: "application/vnd.example.x", Data: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	src, _ := registry.ReadArchive(file)
	entries := []registry.Entry{{Repo: "notes-admin-release", Digest: ad.String()}}
	for _, d := range src.Manifests {
		entries = append(entries, registry.Entry{Repo: d.Annotations[registry.RepositoryAnnotation], Tag: d.Annotations[registry.RefNameAnnotation], Digest: d.Digest.String()})
	}
	src.Close()
	crafted := filepath.Join(dir, "crafted.tar")
	f, _ := os.Create(crafted)
	if _, err := c.Export(ctx, v.prefix, entries, f, nil); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := in.MirrorImport(ctx, crafted, mirrorRegistry(t), false); err == nil || !strings.Contains(err.Error(), "goes to notes-admin-release") {
		t.Errorf("another project's repository: %v", err)
	}
	// An archive whose release-repository holds another tag besides its two.
	src2, _ := registry.ReadArchive(file)
	var extra []registry.Entry
	for _, d := range src2.Manifests {
		e := registry.Entry{Repo: d.Annotations[registry.RepositoryAnnotation], Tag: d.Annotations[registry.RefNameAnnotation], Digest: d.Digest.String()}
		extra = append(extra, e)
		if e.Repo == "notes-release" && e.Tag == "stable" {
			extra = append(extra, registry.Entry{Repo: e.Repo, Tag: "beta", Digest: e.Digest})
		}
	}
	src2.Close()
	twoTags := filepath.Join(dir, "tags.tar")
	f2, _ := os.Create(twoTags)
	if _, err := c.Export(ctx, v.prefix, extra, f2, nil); err != nil {
		t.Fatal(err)
	}
	f2.Close()
	if err := in.MirrorImport(ctx, twoTags, mirrorRegistry(t), false); err == nil || !strings.Contains(err.Error(), "must hold the tags 1.0.0-1 and stable alone") {
		t.Errorf("another release tag: %v", err)
	}
	// A tag the release does not give: another release's, moved.
	src3, _ := registry.ReadArchive(file)
	var retag []registry.Entry
	for _, d := range src3.Manifests {
		e := registry.Entry{Repo: d.Annotations[registry.RepositoryAnnotation], Tag: d.Annotations[registry.RefNameAnnotation], Digest: d.Digest.String()}
		retag = append(retag, e)
		if e.Repo == "notes-installer" && e.Tag != "" {
			retag = append(retag, registry.Entry{Repo: e.Repo, Tag: "0.9.0-1", Digest: e.Digest})
		}
	}
	src3.Close()
	moved := filepath.Join(dir, "moved.tar")
	f3, _ := os.Create(moved)
	if _, err := c.Export(ctx, v.prefix, retag, f3, nil); err != nil {
		t.Fatal(err)
	}
	f3.Close()
	if err := in.MirrorImport(ctx, moved, mirrorRegistry(t), false); err == nil || !strings.Contains(err.Error(), "it tags notes-installer:0.9.0-1@") {
		t.Errorf("a tag of another release: %v", err)
	}
	// Another project's archive.
	other := v.installer(nil)
	r := *other.R
	s := *r.Spec
	s.Name = "other"
	r.Spec = &s
	other.R = &r
	if err := other.MirrorImport(ctx, file, mirrorRegistry(t), false); err == nil || !strings.Contains(err.Error(), "holds no other release") {
		t.Errorf("another project's archive: %v", err)
	}
	// A CA file that holds a key.
	_, key := writeCert(t, time.Now().Add(time.Hour), "x.example.org")
	ca := v.installer(func(o *Options) { o.RegistryCA = key })
	if err := ca.MirrorImport(ctx, file, mirrorRegistry(t), false); err == nil || !strings.Contains(err.Error(), "--registry-ca") {
		t.Errorf("a key as the CA: %v", err)
	}
}

func TestMirrorImportRefusesAnImageThisCtlDoesNotKnow(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	m := withDisks(t, v)
	ctx := context.Background()
	c := registry.New(authn.NewMultiKeychain())
	// The next publication names an image notes-admin-release: another project's repository.
	img, _ := random.Image(16, 1)
	h, _ := img.Digest()
	ref, _ := c.Digest(v.prefix+"/notes-admin-release", h.String())
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	next := *m
	next.Artifacts.Images = map[string]string{"admin-release": h.String()}
	for k, d := range m.Artifacts.Images {
		next.Artifacts.Images[k] = d
	}
	cfg, _ := img.ConfigName()
	next.Artifacts.Configs, next.Artifacts.Manifests = map[string]string{"admin-release": cfg.String()}, map[string]string{"admin-release": h.String()}
	for k := range m.Artifacts.Configs {
		next.Artifacts.Configs[k], next.Artifacts.Manifests[k] = m.Artifacts.Configs[k], m.Artifacts.Manifests[k]
	}
	next.ReleaseSeq++
	v.putRelease(next.Tag, &next)
	v.putRelease(next.Channel, &next)
	file := filepath.Join(t.TempDir(), "next.tar")
	in := v.installer(nil)
	if err := in.MirrorExport(ctx, "", file, []string{}); err != nil {
		t.Fatal(err)
	}
	if err := in.MirrorImport(ctx, file, mirrorRegistry(t), false); err == nil || !strings.Contains(err.Error(), "has the image admin-release, which this notes-stow does not know") {
		t.Errorf("err = %v", err)
	}
}

// The Docker daemon's CA directory is a package variable: these tests do not run in parallel.
func TestDockerCATrustsTheMirrorOnceAndNeverReplacesAnother(t *testing.T) {
	certs := t.TempDir()
	defer func(old string) { dockerCertsDir = old }(dockerCertsDir)
	dockerCertsDir = certs
	ca := caFile(t)
	in := &Installer{O: Options{RegistryCA: ca, Log: io.Discard}}
	want, _ := os.ReadFile(ca)
	dst := filepath.Join(certs, "mirror.example.org:5000", "ca.crt")
	if err := in.dockerCA("mirror.example.org:5000/site/notes"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dst); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s: %v", dst, err)
	}
	if err := in.dockerCA("mirror.example.org:5000/site/notes"); err != nil {
		t.Fatalf("the same CA again: %v", err)
	}
	// The loopback is trusted by the daemon without one.
	if err := in.dockerCA("127.0.0.1:5000/site/notes"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(certs, "127.0.0.1:5000")); err == nil {
		t.Error("wrote a CA for the loopback")
	}
	// The IPv6 loopback too, with or without a port; another IPv6 address gets its CA.
	for _, r := range []string{"[::1]/site/notes", "[::1]:5000/site/notes"} {
		if err := in.dockerCA(r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(certs, "[::1]")); err == nil {
		t.Error("wrote a CA for the IPv6 loopback")
	}
	if err := in.dockerCA("[fd00::5]:5000/site/notes"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(certs, "[fd00::5]:5000", "ca.crt")); err != nil || !bytes.Equal(got, want) {
		t.Errorf("an IPv6 mirror's CA: %v", err)
	}
	// A bundle holding it (and another) trusts it too.
	bundle := filepath.Join(certs, "bundle.example.org", "ca.crt")
	_ = os.MkdirAll(filepath.Dir(bundle), 0o755)
	otherPEM, _ := os.ReadFile(caFile(t))
	_ = os.WriteFile(bundle, append(otherPEM, want...), 0o644)
	if err := in.dockerCA("bundle.example.org/site/notes"); err != nil {
		t.Errorf("a bundle holding the CA: %v", err)
	}
	// Another CA there is the operator's: refused, kept.
	other := &Installer{O: Options{RegistryCA: caFile(t), Log: io.Discard}}
	if err := other.dockerCA("mirror.example.org:5000/site/notes"); err == nil || !strings.Contains(err.Error(), "another CA") {
		t.Fatalf("another CA: %v", err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, want) {
		t.Error("the CA there was replaced")
	}
	// A directory this run cannot write: the command to run as root.
	ro := filepath.Join(t.TempDir(), "ro")
	_ = os.Mkdir(ro, 0o500)
	dockerCertsDir = ro
	if os.Getuid() != 0 {
		if err := in.dockerCA("mirror.example.org/site/notes"); err == nil || !strings.Contains(err.Error(), "install -D -m 0644 "+ca+" "+filepath.Join(ro, "mirror.example.org", "ca.crt")) {
			t.Errorf("unwritable: %v", err)
		}
	}
	// No CA, nothing to do.
	if err := (&Installer{}).dockerCA("mirror.example.org/site/notes"); err != nil {
		t.Error(err)
	}
}

func TestInstallRecordsTheRegistryCAAndGivesItToTheDaemon(t *testing.T) {
	certs := t.TempDir()
	defer func(old string) { dockerCertsDir = old }(dockerCertsDir)
	dockerCertsDir = certs
	ca := caFile(t)
	e := newEnv(t)
	// A key given as the CA: refused before anything is written.
	_, key := writeCert(t, time.Now().Add(time.Hour), "x.example.org")
	if err := e.install(func(o *Options) { o.Registry, o.RegistryCA = "mirror.example.org/site/notes", key }); err == nil || !strings.Contains(err.Error(), "--registry-ca") {
		t.Fatalf("a key: %v", err)
	}
	e.nothingWritten()
	// A pull that fails: nothing stays, the daemon's CA included.
	e.docker.pullErr = errors.New("pull refused")
	if err := e.install(func(o *Options) { o.Registry, o.RegistryCA = "mirror.example.org/site/notes", ca }); err == nil {
		t.Fatal("installed with a failing pull")
	}
	e.nothingWritten()
	if _, err := os.Stat(filepath.Join(certs, "mirror.example.org", "ca.crt")); err == nil {
		t.Error("a failed install left the daemon's CA")
	}
	e.docker.pullErr = nil
	// A CA file in a directory no install owns is not trusted without --registry-ca.
	_ = os.MkdirAll(e.dir, 0o755)
	_ = os.WriteFile(filepath.Join(e.dir, RegistryCAFile), []byte("stray"), 0o644)
	if ca, err := e.installer(nil).registryCA(); err != nil || ca != nil {
		t.Errorf("a stray CA file: %q %v", ca, err)
	}
	_ = os.RemoveAll(e.dir)
	if err := e.install(func(o *Options) { o.Registry, o.RegistryCA = "mirror.example.org/site/notes", ca }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	want, _ := os.ReadFile(ca)
	if got, err := os.ReadFile(filepath.Join(e.dir, RegistryCAFile)); err != nil || !bytes.Equal(got, want) {
		t.Errorf("recorded CA: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(certs, "mirror.example.org", "ca.crt")); err != nil || !bytes.Equal(got, want) {
		t.Errorf("the daemon's CA: %v", err)
	}
	if get(e.readEnv(), "STOWAGE_REGISTRY") != "mirror.example.org/site/notes" {
		t.Errorf("registry %q", get(e.readEnv(), "STOWAGE_REGISTRY"))
	}
	if err := e.install(func(o *Options) { o.RegistryCA = ca }); err == nil || !strings.Contains(err.Error(), "--registry-ca") {
		t.Errorf("--registry-ca again: %v", err)
	}
}

func TestVMCreateFromAMirrorCarriesItAndItsCA(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	mirror := v.prefix
	v.r.Spec.Registry.Prefix = "own.invalid/acme/notes" // never reached: the disk comes from the mirror
	ca := caFile(t)
	if err := v.create(VMCreate{Platform: "proxmox", Hostname: "vm1"}, func(o *Options) { o.Registry, o.RegistryCA = mirror+"/", ca }); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	ud, _, _ := readSeed(t, filepath.Join(v.out, "vm1-seed.iso"))
	st, cc := cloudSettings(t, ud)
	if st.Registry != mirror || st.RegistryCA != "/usr/local/share/ca-certificates/notes-registry.crt" {
		t.Errorf("settings %+v", st)
	}
	want, _ := os.ReadFile(ca)
	wrote, updated := false, false
	for _, s := range cc.Stages["boot"] {
		for _, f := range s.Files {
			wrote = wrote || (f.Path == st.RegistryCA && f.Content == string(want) && f.Permissions == 0o644)
		}
		for _, c := range s.Commands {
			updated = updated || (wrote && c == "update-ca-certificates")
		}
	}
	if !wrote || !updated {
		t.Errorf("the CA in the cloud-config: written %v, then trusted %v\n%s", wrote, updated, ud)
	}
	if !strings.Contains(v.log.String(), "registry: "+mirror+", trusting the CA of "+ca) {
		t.Errorf("summary: %s", v.log)
	}
	// Without a mirror the cloud-config carries none.
	plain := newVMCreate(t, "qcow2")
	// A CA file in the directory is an install's, which vm create never reads.
	if err := os.WriteFile(filepath.Join(plain.dir, RegistryCAFile), []byte("not a CA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := plain.create(VMCreate{Platform: "proxmox", Hostname: "vm2"}, nil); err != nil {
		t.Fatal(err)
	}
	ud, _, _ = readSeed(t, filepath.Join(plain.out, "vm2-seed.iso"))
	if st, _ := cloudSettings(t, ud); st.Registry != "" || st.RegistryCA != "" || strings.Contains(ud, "update-ca-certificates") {
		t.Errorf("a mirror without one: %+v", st)
	}
	bad := newVMCreate(t, "qcow2")
	if err := bad.create(VMCreate{Platform: "proxmox", Hostname: "vm3"}, func(o *Options) { o.Registry = "Not/A/Prefix" }); err == nil || !strings.Contains(err.Error(), "--registry") {
		t.Errorf("a bad --registry: %v", err)
	}
}

func TestTheSettingsGiveTheMirror(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	in := e.installer(settings(t, "registry: mirror.example.org/site/notes\nregistry_ca: /usr/local/share/ca-certificates/notes-registry.crt\n"))
	if err := in.withSettings(); err != nil {
		t.Fatal(err)
	}
	if in.O.Registry != "mirror.example.org/site/notes" || in.O.RegistryCA != "/usr/local/share/ca-certificates/notes-registry.crt" {
		t.Errorf("options %q %q", in.O.Registry, in.O.RegistryCA)
	}
}

func TestAVMInstallFromAMirrorRunsThePreloadedImagesAndRecordsTheMirror(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	ca := caFile(t)
	in := e.installer(settings(t, "registry: mirror.example.org/site/notes\nregistry_ca: "+ca+"\n"))
	if err := in.withSettings(); err != nil {
		t.Fatal(err)
	}
	if err := in.Install(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	f := e.readEnv()
	if get(f, "STOWAGE_REGISTRY") != "mirror.example.org/site/notes" {
		t.Errorf("registry %q", get(f, "STOWAGE_REGISTRY"))
	}
	if get(f, ImageVar("api")) != preloadRef("api") {
		t.Errorf("the api image is %q, not the preloaded %q", get(f, ImageVar("api")), preloadRef("api"))
	}
	want, _ := os.ReadFile(ca)
	if got, err := os.ReadFile(filepath.Join(e.dir, RegistryCAFile)); err != nil || !bytes.Equal(got, want) {
		t.Errorf("recorded CA: %v", err)
	}
}
