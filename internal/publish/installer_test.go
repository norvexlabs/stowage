package publish

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// installerAt reads the installer image under tag, whose one file must be ctl: its entrypoint and
// command, and the payload the file carries.
func (e *env) installerAt(tag, ctl string) (entry, cmd []string, files payload.Files) {
	e.t.Helper()
	ref, _ := name.NewTag(e.host + "/acme/notes/notes-installer:" + tag)
	img, err := remote.Image(ref)
	if err != nil {
		e.t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		e.t.Fatal(err)
	}
	if cf.OS != "linux" || cf.Architecture != "amd64" {
		e.t.Fatalf("installer platform %s/%s", cf.OS, cf.Architecture)
	}
	layers, err := img.Layers()
	if err != nil || len(layers) != 1 {
		e.t.Fatalf("installer layers: %d %v", len(layers), err)
	}
	rc, err := layers[0].Uncompressed()
	if err != nil {
		e.t.Fatal(err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	h, err := tr.Next()
	if err != nil {
		e.t.Fatal(err)
	}
	if h.Name != ctl || h.Mode != 0o755 {
		e.t.Fatalf("installer file %s %o", h.Name, h.Mode)
	}
	exe := filepath.Join(e.t.TempDir(), ctl)
	b, err := io.ReadAll(tr)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := tr.Next(); err != io.EOF {
		e.t.Fatalf("the installer layer holds more than one file: %v", err)
	}
	if err := os.WriteFile(exe, b, 0o600); err != nil {
		e.t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), string(e.installer)) {
		e.t.Fatal("the installer file does not start with the installer executable")
	}
	if files, err = payload.Read(exe); err != nil {
		e.t.Fatal(err)
	}
	return cf.Config.Entrypoint, cf.Config.Cmd, files
}

func TestTheShortNameNamesTheInstaller(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	testutil.Write(t, e.root, "stowage.yaml", strings.Replace(readFile(t, e.root+"/stowage.yaml"), "\nname: notes\n", "\nname: notes\nshort_name: nt\n", 1))
	r := e.mustShip("stable", 1)
	eq(t, "manifest short_name", r.Manifest.ShortName, "nt")
	entry, _, _ := e.installerAt("1.0.0", "nt-stow")
	eq(t, "entrypoint", strings.Join(entry, " "), "/nt-stow")
}

// An install keeps its installer under one name, so a release that renames it onto a channel
// already holding a release is refused at ship; the same name ships.
func TestARenamedInstallerIsRefusedAtShip(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mustShip("stable", 1)
	e.bump("1.1.0")
	testutil.Write(t, e.root, "stowage.yaml", strings.Replace(readFile(t, e.root+"/stowage.yaml"), "\nname: notes\n", "\nname: notes\nshort_name: nt\n", 1))
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "does not rename it") {
		t.Fatalf("a renamed installer shipped: %v", err)
	}
	testutil.Write(t, e.root, "stowage.yaml", strings.Replace(readFile(t, e.root+"/stowage.yaml"), "\nshort_name: nt\n", "\nshort_name: notes\n", 1))
	e.mustShip("stable", 1)
}

func TestShipPublishesTheInstallerWithTheRelease(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.mustShip("stable", 1)
	inst := r.Manifest.Artifacts.Installer
	eq(t, "installer tag", e.digest("notes-installer", "1.0.0"), inst)
	eq(t, "installer latest", e.digest("notes-installer", "latest"), inst)
	entry, cmd, files := e.installerAt("1.0.0", "notes-stow")
	eq(t, "entrypoint", strings.Join(entry, " "), "/notes-stow")
	eq(t, "command", strings.Join(cmd, " "), payload.ScriptCommand)
	carried, err := release.Decode(files[payload.Release])
	if err != nil {
		t.Fatal(err)
	}
	want := *r.Manifest
	want.Artifacts.Installer = ""
	got, _ := json.Marshal(carried)
	wantJSON, _ := json.Marshal(&want)
	eq(t, "carried manifest", string(got), string(wantJSON))
	eq(t, "compose", string(files[payload.Compose]), readFile(t, e.root+"/deploy/compose.yaml"))
	if _, ok := files[payload.Spec]; !ok {
		t.Fatal("no spec in the payload")
	}
	if _, ok := files[payload.Pull]; ok {
		t.Fatal("a project without a pull account has one baked in")
	}

	e.log.Reset()
	again := e.mustShip("stable", 1)
	eq(t, "the same release makes the same installer", again.Manifest.Artifacts.Installer, inst)
	if strings.Contains(e.log.String(), "installer:") {
		t.Fatalf("a re-ship wrote the installer:\n%s", e.log)
	}
}

func TestAMissingInstallerTagIsPutBack(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.mustShip("stable", 1)
	tag, _ := e.reg.Tag(e.host+"/acme/notes/notes-installer", "1.0.0")
	if err := e.reg.DeleteTag(context.Background(), tag); err != nil {
		t.Fatal(err)
	}
	p, _ := e.publisher()
	if _, err := p.Verify(context.Background(), "stable", "", 1); err == nil || !strings.Contains(err.Error(), "notes-installer:1.0.0 is missing") {
		t.Fatalf("verify of a release without its installer tag: %v", err)
	}
	again := e.mustShip("stable", 1)
	eq(t, "release kept", again.Digest, r.Digest)
	eq(t, "installer tag back", e.digest("notes-installer", "1.0.0"), r.Manifest.Artifacts.Installer)
}

func TestTheInstallerExecutableIsAReleaseInput(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mustShip("stable", 1)
	e.installer = []byte("#!/bin/sh\necho another installer\n")
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "--revision 2") {
		t.Fatalf("another installer under a published stable tag: %v", err)
	}
	r := e.mustShip("stable", 2)
	_, _, files := e.installerAt("1.0.0-r2", "notes-stow")
	eq(t, "revision carried", strings.Contains(string(files[payload.Release]), `"revision": 2`), true)
	eq(t, "latest follows", e.digest("notes-installer", "latest"), r.Manifest.Artifacts.Installer)

	e.installer = nil
	if _, err := e.ship("stable", 3); err == nil || !strings.Contains(err.Error(), "carries no installer") {
		t.Fatalf("ship without an installer executable: %v", err)
	}
}

func TestThePullAccountIsBakedIntoTheInstaller(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	testutil.SetYAML(t, e.root, "stowage.yaml", []string{"registry", "pull"},
		map[string]any{"provider": "token", "user": "notes-pull", "token_env": "NOTES_PULL_TOKEN"})
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "NOTES_PULL_TOKEN is not set") {
		t.Fatalf("ship without the pull account's password: %v", err)
	}
	if e.digest("notes-release", "1.0.0") != "" || e.digest("notes-installer", "1.0.0") != "" {
		t.Fatal("a refused ship wrote a release or an installer")
	}
	e.pull = &payload.Account{User: "notes-pull", Password: "s3cret"}
	e.mustShip("stable", 1)
	_, _, files := e.installerAt("1.0.0", "notes-stow")
	var a payload.Account
	if err := json.Unmarshal(files[payload.Pull], &a); err != nil || a != *e.pull {
		t.Fatalf("baked account %+v %v", a, err)
	}
	// A rotated password is another installer: a new revision on an immutable channel.
	e.pull = &payload.Account{User: "notes-pull", Password: "rotated"}
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "--revision 2") {
		t.Fatalf("a rotated pull password under a published stable tag: %v", err)
	}
}

func TestLatestFollowsOnlyTheChannelThatMovesIt(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mustShip("beta", 1)
	eq(t, "latest after a beta ship", e.digest("notes-installer", "latest"), "")
	first := e.mustShip("stable", 1)
	e.bump("1.1.0")
	second := e.mustShip("stable", 1)
	eq(t, "latest on the newest", e.digest("notes-installer", "latest"), second.Manifest.Artifacts.Installer)

	p, _ := e.publisher()
	ctx := context.Background()
	if err := p.Yank(ctx, "stable", "1.1.0", 1, "broken"); err != nil {
		t.Fatal(err)
	}
	eq(t, "latest back with the pointer", e.digest("notes-installer", "latest"), first.Manifest.Artifacts.Installer)
	if err := p.Yank(ctx, "stable", "1.0.0", 1, "broken too"); err != nil {
		t.Fatal(err)
	}
	eq(t, "no latest without a release", e.digest("notes-installer", "latest"), "")
}

func TestVerifyComparesLatestByDigest(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	beta := e.mustShip("beta", 1)
	stable := e.mustShip("stable", 1)
	ctx := context.Background()
	src, _ := e.reg.Digest(e.host+"/acme/notes/notes-installer", beta.Manifest.Artifacts.Installer)
	latest, _ := e.reg.Tag(e.host+"/acme/notes/notes-installer", "latest")
	if err := e.reg.Retag(ctx, src, latest); err != nil {
		t.Fatal(err)
	}
	p, _ := e.publisher()
	if _, err := p.Verify(ctx, "stable", "", 1); err == nil || !strings.Contains(err.Error(), "notes-installer:latest is "+beta.Manifest.Artifacts.Installer) {
		t.Fatalf("verify of a stale latest: %v", err)
	}
	if _, err := p.Verify(ctx, "beta", "", 1); err != nil {
		t.Fatalf("beta does not move latest: %v", err)
	}
	e.mustShip("stable", 1)
	eq(t, "a re-ship puts latest back", e.digest("notes-installer", "latest"), stable.Manifest.Artifacts.Installer)
}

func TestPruneDeletesTheInstallerTag(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mustShip("beta", 1)
	p, _ := e.publisher()
	e.bump("1.1.0")
	e.mustShip("beta", 1)
	if _, err := p.Prune(context.Background(), "beta", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	eq(t, "pruned installer tag", e.digest("notes-installer", "beta-1.0.0"), "")
	if e.digest("notes-installer", "beta-1.1.0") == "" {
		t.Fatal("prune deleted the pointer release's installer")
	}
}

// A published release naming another installer than the one it makes (a release of an older
// stowage, a hand edit) is never rewritten on an immutable channel; a mutable one re-publishes it
// under its number.
func TestAnInstallerThatWouldDifferIsRefusedOnAnImmutableChannel(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	other := func(r *Ref) {
		t.Helper()
		m := *r.Manifest
		m.Artifacts.Installer = r.Manifest.Artifacts.Images["web"]
		p, _ := e.publisher()
		if _, err := p.writeRelease(ctx, m.Tag, &m); err != nil {
			t.Fatal(err)
		}
	}
	stable := e.mustShip("stable", 1)
	other(stable)
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "would now be "+stable.Manifest.Artifacts.Installer) {
		t.Fatalf("an immutable release with another installer: %v", err)
	}
	beta := e.mustShip("beta", 1)
	other(beta)
	again := e.mustShip("beta", 1)
	eq(t, "installer", again.Manifest.Artifacts.Installer, beta.Manifest.Artifacts.Installer)
	eq(t, "number kept", again.Manifest.ReleaseSeq, beta.Manifest.ReleaseSeq)
}

func TestLatestIsRepairedAfterAnInterruptedRun(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	first := e.mustShip("stable", 1)
	e.bump("1.1.0")
	second := e.mustShip("stable", 1)

	// A ship stopped between the pointer and latest; any later ship, of any release, repairs it.
	src, _ := e.reg.Digest(e.host+"/acme/notes/notes-installer", first.Manifest.Artifacts.Installer)
	latest, _ := e.reg.Tag(e.host+"/acme/notes/notes-installer", "latest")
	if err := e.reg.Retag(ctx, src, latest); err != nil {
		t.Fatal(err)
	}
	e.bump("1.0.0")
	e.mustShip("stable", 2)
	eq(t, "latest repaired by a ship of an earlier release", e.digest("notes-installer", "latest"), second.Manifest.Artifacts.Installer)

	// A yank of the last releases stopped after removing the pointer: yanking again removes latest.
	p, _ := e.publisher()
	for _, v := range []struct {
		v   string
		rev int
	}{{"1.1.0", 1}, {"1.0.0", 2}} {
		if err := p.Yank(ctx, "stable", v.v, v.rev, "broken"); err != nil {
			t.Fatal(err)
		}
	}
	e.failDelete = "notes-installer"
	if err := p.Yank(ctx, "stable", "1.0.0", 1, "broken"); err == nil {
		t.Fatal("the yank removed latest through a refused delete")
	}
	eq(t, "pointer removed", e.digest("notes-release", "stable"), "")
	if _, err := p.Verify(ctx, "stable", "", 1); err == nil || !strings.Contains(err.Error(), "yank or prune again") {
		t.Fatalf("verify of a channel with no release and a latest left: %v", err)
	}
	e.failDelete = ""
	if err := p.Yank(ctx, "stable", "1.0.0", 1, "broken"); err != nil {
		t.Fatal(err)
	}
	eq(t, "latest removed by the second yank", e.digest("notes-installer", "latest"), "")
}

// A fallback to a release that has no installer removes latest rather than leave the yanked one's.
func TestAFallbackToAReleaseWithoutAnInstallerRemovesLatest(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	old := e.mustShip("stable", 1)
	m := *old.Manifest
	m.Artifacts.Installer = ""
	p, _ := e.publisher()
	if _, err := p.writeRelease(ctx, m.Tag, &m); err != nil {
		t.Fatal(err)
	}
	e.bump("1.1.0")
	e.mustShip("stable", 1)
	if err := p.Yank(ctx, "stable", "1.1.0", 1, "broken"); err != nil {
		t.Fatal(err)
	}
	eq(t, "latest", e.digest("notes-installer", "latest"), "")
	if _, err := p.Verify(ctx, "stable", "", 1); err != nil {
		t.Fatalf("verify of a pointer on a release without an installer: %v", err)
	}
	src, _ := e.reg.Digest(e.host+"/acme/notes/notes-installer", e.digest("notes-installer", "1.1.0"))
	latest, _ := e.reg.Tag(e.host+"/acme/notes/notes-installer", "latest")
	if err := e.reg.Retag(ctx, src, latest); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Verify(ctx, "stable", "", 1); err == nil || !strings.Contains(err.Error(), "has no installer") {
		t.Fatalf("verify of a latest left on a yanked release: %v", err)
	}
}

// A ship of a yanked release moves nothing forward, and puts the channel right as a yank would.
func TestAShipOfAYankedReleaseRepairsLatest(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	first := e.mustShip("stable", 1)
	e.bump("1.1.0")
	second := e.mustShip("stable", 1)
	p, _ := e.publisher()
	if err := p.Yank(ctx, "stable", "1.1.0", 1, "broken"); err != nil {
		t.Fatal(err)
	}
	src, _ := e.reg.Digest(e.host+"/acme/notes/notes-installer", second.Manifest.Artifacts.Installer)
	latest, _ := e.reg.Tag(e.host+"/acme/notes/notes-installer", "latest")
	if err := e.reg.Retag(ctx, src, latest); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "is yanked") {
		t.Fatalf("ship of a yanked release: %v", err)
	}
	eq(t, "latest back on the pointer's release", e.digest("notes-installer", "latest"), first.Manifest.Artifacts.Installer)
}

// When the pointer stays on a later release that is incomplete, an earlier release still ships;
// latest is left for verify to report.
func TestAnIncompleteLaterPointerDoesNotFailAShip(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.bump("1.1.0")
	e.mustShip("stable", 1)
	tag, _ := e.reg.Tag(e.host+"/acme/notes/notes-installer", "1.1.0")
	if err := e.reg.DeleteTag(context.Background(), tag); err != nil {
		t.Fatal(err)
	}
	e.bump("1.0.0")
	e.mustShip("stable", 1)
	if !strings.Contains(e.log.String(), "warning: :latest tags not checked: 1.1.0") {
		t.Fatalf("no warning:\n%s", e.log)
	}
}

// A ship of a yanked release never moves the pointer, even off a later release that is incomplete.
func TestAShipOfAYankedReleaseMovesNoPointer(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	e.mustShip("stable", 1)
	e.bump("1.1.0")
	later := e.mustShip("stable", 1)
	p, _ := e.publisher()
	if err := p.Yank(ctx, "stable", "1.0.0", 1, "broken"); err != nil {
		t.Fatal(err)
	}
	tag, _ := e.reg.Tag(e.host+"/acme/notes/notes-installer", "1.1.0")
	if err := e.reg.DeleteTag(ctx, tag); err != nil {
		t.Fatal(err)
	}
	testutil.Write(t, e.root, "VERSION", "1.0.0\n")
	e.noDisks = true // its disks a newer set replaced
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "is yanked") {
		t.Fatalf("ship of a yanked release: %v", err)
	}
	eq(t, "pointer", e.digest("notes-release", "stable"), later.Digest.String())
	eq(t, "latest left for verify", e.digest("notes-installer", "latest"), later.Manifest.Artifacts.Installer)
}

// An installer that would differ is refused before anything is written, images included.
func TestAnInstallerRefusalWritesNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	r := e.mustShip("stable", 1)
	m := *r.Manifest
	m.Artifacts.Installer = ""
	p, _ := e.publisher()
	if _, err := p.writeRelease(ctx, m.Tag, &m); err != nil {
		t.Fatal(err)
	}
	tag, _ := e.reg.Tag(e.host+"/acme/notes/notes-postgres", "1.0.0")
	if err := e.reg.DeleteTag(ctx, tag); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "with the installer none, and it would now be") {
		t.Fatalf("an immutable release recording no installer: %v", err)
	}
	eq(t, "image tag not put back", e.digest("notes-postgres", "1.0.0"), "")
}

// A prune stopped after removing the pointer of the channel that moves latest: the next prune
// removes latest.
func TestAPruneWithNoPointerLeftRemovesLatest(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	testutil.SetYAML(t, e.root, "stowage.yaml", []string{"channels", "stable", "latest"}, false)
	testutil.SetYAML(t, e.root, "stowage.yaml", []string{"channels", "beta"}, map[string]any{"tag": "beta-{v}", "mutable": true, "latest": true})
	e.mustShip("beta", 1)
	eq(t, "latest on beta", e.digest("notes-installer", "latest") != "", true)
	p, _ := e.publisher()
	ctx := context.Background()
	e.failDelete = "manifests/latest"
	if _, err := p.Prune(ctx, "beta", "1.0.0"); err == nil {
		t.Fatal("prune removed latest through a refused delete")
	}
	eq(t, "pointer removed", e.digest("notes-release", "beta"), "")
	e.failDelete = ""
	if _, err := p.Prune(ctx, "beta", ""); err != nil {
		t.Fatal(err)
	}
	eq(t, "latest removed", e.digest("notes-installer", "latest"), "")
}

// A yank that stopped after writing the yanked manifest left the pointer and latest on it: a ship
// of that release moves them back as the yank would have.
func TestAShipOfAYankedReleaseFinishesAnInterruptedYank(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	first := e.mustShip("stable", 1)
	e.bump("1.1.0")
	second := e.mustShip("stable", 1)
	m := *second.Manifest
	m.Yanked = &release.Yank{Reason: "broken", Date: e.now}
	p, _ := e.publisher()
	if _, err := p.writeRelease(ctx, m.Tag, &m); err != nil {
		t.Fatal(err)
	}
	eq(t, "pointer still on the yanked release", e.digest("notes-release", "stable"), second.Digest.String())
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "is yanked") {
		t.Fatalf("ship of a yanked release: %v", err)
	}
	eq(t, "pointer back", e.digest("notes-release", "stable"), first.Digest.String())
	eq(t, "latest back", e.digest("notes-installer", "latest"), first.Manifest.Artifacts.Installer)
}

// A prune killed after deleting the pointer's release, before moving the pointer: the next prune
// moves it, and latest with it.
func TestAPruneFinishesAPointerLeftOnADeletedRelease(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	testutil.SetYAML(t, e.root, "stowage.yaml", []string{"channels", "stable", "latest"}, false)
	testutil.SetYAML(t, e.root, "stowage.yaml", []string{"channels", "beta"}, map[string]any{"tag": "beta-{v}", "mutable": true, "latest": true})
	first := e.mustShip("beta", 1)
	e.bump("1.1.0")
	e.mustShip("beta", 1)
	ctx := context.Background()
	for _, repo := range []string{"notes-installer", "notes-release"} {
		tag, _ := e.reg.Tag(e.host+"/acme/notes/"+repo, "beta-1.1.0")
		if err := e.reg.DeleteTag(ctx, tag); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := e.publisher()
	if _, err := p.Prune(ctx, "beta", "1.1.0"); err == nil {
		t.Fatal("pruned a version with no release left")
	}
	eq(t, "pointer moved", e.digest("notes-release", "beta"), first.Digest.String())
	eq(t, "latest moved", e.digest("notes-installer", "latest"), first.Manifest.Artifacts.Installer)
}
