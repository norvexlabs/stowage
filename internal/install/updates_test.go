package install

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/spf13/cobra"

	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// point makes the channel's pointer at prefix hold the release manifest under tag, as `ship`
// moves it.
func point(t *testing.T, prefix, channel, tag string) {
	t.Helper()
	c := registry.New(authn.NewMultiKeychain())
	m, _, err := readRelease(context.Background(), c, prefix+"/notes-release", tag)
	if err != nil || m == nil {
		t.Fatalf("%s: %v", tag, err)
	}
	(&pubEnv{env: &env{t: t}, prefix: prefix}).putRelease(channel, m)
}

// updates runs the Docker installer's `updates` with args and returns what it printed.
func (p *pubEnv) updates(args ...string) (string, error) {
	in := p.installer(nil)
	var out bytes.Buffer
	c := updatesCommand("upgrade", nil, nil, func(cmd *cobra.Command, channel, version string) (Update, string, error) {
		u, err := in.Updates(cmd.Context(), channel, version)
		return u, in.O.Dir, err
	}, &out)
	c.SetArgs(args)
	c.SetOut(&out)
	c.SetErr(&out)
	c.SilenceUsage, c.SilenceErrors = true, true
	err := c.ExecuteContext(context.Background())
	return out.String(), err
}

// On Docker, `updates` reads the channel's pointer in the install's registry and offers the
// release it names after the one the install runs, with its changelog; --version names a version
// instead, and nothing newer is said so.
func TestUpdatesOnDockerOfferTheChannelsRelease(t *testing.T) {
	t.Parallel()
	p := published(t, spec.Provider{Name: "none"}, nil)
	p.publish("2.0.0", 1, func(m *release.Manifest) { m.Changelog.Project = "- faster notes" }, nil)
	p.publish("1.0.0", 2, nil, nil)
	p.publish("0.9.0", 1, nil, nil)
	p.publish("2.1.0", 1, func(m *release.Manifest) { m.Channel = "beta" }, nil)
	p.publish("0.8.0", 1, func(m *release.Manifest) { m.Channel = "beta" }, nil)
	point(t, p.prefix, "stable", "2.0.0-1")
	point(t, p.prefix, "beta", "2.1.0-1")

	out, err := p.updates()
	if err != nil {
		t.Fatalf("%v\n%s", err, p.log)
	}
	if want := "2.0.0 (revision 1) on stable: " + p.dir + " runs 1.0.0 (revision 1)\n\n- faster notes\n"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
	out, err = p.updates("--json")
	var u Update
	if err != nil || json.Unmarshal([]byte(out), &u) != nil || !u.Available || u.Target != (Version{"2.0.0", 1}) || u.Current != (Version{"1.0.0", 1}) || u.Channel != "stable" {
		t.Fatalf("--json: %v %s", err, out)
	}
	if out, err := p.updates("--version", "1.0.0"); err != nil || !strings.Contains(out, "1.0.0 (revision 2) on stable") {
		t.Fatalf("--version 1.0.0: %v %q", err, out)
	}
	// An earlier release is found, and not offered.
	if out, err := p.updates("--version", "0.9.0"); err != nil || out != "no upgrade on stable: "+p.dir+" runs 1.0.0 (revision 1), the release found is 0.9.0 (revision 1)\n" {
		t.Fatalf("--version 0.9.0: %v %q", err, out)
	}
	// Another channel's release is shown, not offered: `upgrade --version` reads stable alone.
	out, err = p.updates("--channel", "beta", "--json")
	if u = (Update{}); err != nil || json.Unmarshal([]byte(out), &u) != nil || u.Available || u.Follows != "stable" || u.Target != (Version{"2.1.0", 1}) {
		t.Fatalf("--channel beta: %v %s", err, out)
	}
	if out, _ := p.updates("--channel", "beta"); !strings.HasSuffix(out, p.dir+" follows stable: `upgrade --version` moves it on stable alone\n") {
		t.Fatalf("--channel beta: %q", out)
	}
	// Another channel's earlier release: no note.
	if out, err := p.updates("--channel", "beta", "--version", "0.8.0"); err != nil || out != "no upgrade on beta: "+p.dir+" runs 1.0.0 (revision 1), the release found is 0.8.0 (revision 1)\n" {
		t.Fatalf("beta's 0.8.0: %v %q", err, out)
	}
	// A later revision of the pointer's version is what `upgrade --version 2.0.0` installs: named.
	p.publish("2.0.0", 2, nil, nil)
	if out, err := p.updates(); err != nil || !strings.HasPrefix(out, "2.0.0 (revision 2) on stable: ") {
		t.Fatalf("a later revision than the pointer's: %v %q", err, out)
	}
	// A later release published without an installer is not offered, and the reason said.
	c := registry.New(authn.NewMultiKeychain())
	bare, _, err := readRelease(context.Background(), c, p.prefix+"/notes-release", "2.0.0-2")
	if err != nil || bare == nil {
		t.Fatal(err)
	}
	bare.ProjectVersion, bare.Tag, bare.Artifacts.Installer = "3.0.0", "3.0.0-1", ""
	p.putRelease(bare.Tag, bare)
	point(t, p.prefix, "stable", "3.0.0-1")
	if out, err := p.updates(); err != nil || out != "no upgrade on stable: "+p.dir+" runs 1.0.0 (revision 1); 3.0.0 (revision 2), after it, was published without an installer, which `upgrade --version` fetches\n" {
		t.Fatalf("no installer: %v %q", err, out)
	}
	out, _ = p.updates("--json")
	if u = (Update{}); json.Unmarshal([]byte(out), &u) != nil || u.Available || !u.NoInstaller {
		t.Fatalf("no installer --json: %s", out)
	}
	// The channel names an earlier release (moved back by a yank): nothing newer, said so.
	point(t, p.prefix, "stable", "0.9.0-1")
	if out, err := p.updates(); err != nil || out != "no upgrade on stable: "+p.dir+" runs 1.0.0 (revision 1), the release found is 0.9.0 (revision 1)\n" {
		t.Fatalf("nothing newer: %v %q", err, out)
	}
	if _, err := p.updates("--channel", "edge"); err == nil || !strings.Contains(err.Error(), "edge") {
		t.Fatalf("a channel with no pointer: %v", err)
	}
	if _, err := p.updates("--channel", "Not A Channel"); err == nil || !strings.Contains(err.Error(), "is not a channel name") {
		t.Fatalf("a bad channel: %v", err)
	}
}

// Docker's `updates` offers only a release signed by a key the install trusts, as `upgrade
// --version` reads it: an unsigned pointer is refused, one signed by a trusted key offered.
func TestUpdatesOnDockerNeedASignatureByAKeptKey(t *testing.T) {
	t.Parallel()
	k, other := testutil.NewKeySigner(t), testutil.NewKeySigner(t)
	p := publishedWith(t, spec.Provider{Name: "none"}, nil, payload.Files{payload.KeysDir + "release.pub": k.PEM})
	p.publish("2.0.0", 1, nil, nil)
	point(t, p.prefix, "stable", "2.0.0-1")
	if _, err := p.updates(); err == nil || !strings.Contains(err.Error(), "is not signed by a key this install trusts") {
		t.Fatalf("unsigned: %v", err)
	}
	p.signRelease("stable", other)
	if _, err := p.updates(); err == nil || !strings.Contains(err.Error(), "none by a trusted key") {
		t.Fatalf("signed by another key: %v", err)
	}
	p.signRelease("stable", k)
	if out, err := p.updates(); err != nil || !strings.Contains(out, "2.0.0 (revision 1) on stable") {
		t.Fatalf("signed: %v %q", err, out)
	}
	if _, err := p.updates("--version", "2.0.0"); err != nil {
		t.Fatalf("--version, the tag signed by the same digest: %v", err)
	}
}

// On Kubernetes, `k8s updates` reads the registry the release's values record, with the keys
// `k8s upgrade --version` trusts.
func TestK8sUpdatesOfferTheChannelsRelease(t *testing.T) {
	t.Parallel()
	k := testutil.NewKeySigner(t)
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.publish("2.0.0", 1)
	point(t, e.prefix, "stable", "2.0.0-1")
	in := e.installer(nil)
	if _, err := in.K8sUpdates(context.Background(), "", ""); err == nil || !strings.Contains(err.Error(), "no release notes in notes") {
		t.Fatalf("no release: %v", err)
	}
	e.runs("1.0.0", 1)
	// A later revision than the pointer's is what `k8s upgrade --version 2.0.0` installs: named.
	e.publish("2.0.0", 2)
	if u, err := in.K8sUpdates(context.Background(), "", ""); err != nil || u.Target != (Version{"2.0.0", 2}) {
		t.Fatalf("a later revision than the pointer's: %+v %v", u, err)
	}
	point(t, e.prefix, "stable", "2.0.0-2")
	u, err := in.K8sUpdates(context.Background(), "", "")
	if err != nil || !u.Available || u.Target != (Version{"2.0.0", 2}) || u.Current != (Version{"1.0.0", 1}) {
		t.Fatalf("%+v %v", u, err)
	}
	var b bytes.Buffer
	printUpdateOf(&b, u, "release notes in notes", "k8s upgrade")
	if b.String() != "2.0.0 (revision 2) on stable: release notes in notes runs 1.0.0 (revision 1)\n" {
		t.Fatalf("%q", b.String())
	}
	// Another channel's release is shown, not offered: `k8s upgrade --version` reads stable alone.
	c := registry.New(authn.NewMultiKeychain())
	bm, _, err := readRelease(context.Background(), c, e.prefix+"/notes-release", "2.0.0-1")
	if err != nil || bm == nil {
		t.Fatal(err)
	}
	bm.Channel, bm.Tag = "beta", "beta-2.0.0-1"
	bm.Artifacts.Chart.Repository = "charts/notes-beta"
	(&pubEnv{env: &env{t: t}, prefix: e.prefix}).putRelease(bm.Tag, bm)
	point(t, e.prefix, "beta", bm.Tag)
	bu, err := in.K8sUpdates(context.Background(), "beta", "")
	if err != nil || bu.Available || bu.Follows != "stable" {
		t.Fatalf("--channel beta: %+v %v", bu, err)
	}
	var note bytes.Buffer
	printUpdateOf(&note, bu, "release notes in notes", "k8s upgrade")
	if !strings.HasSuffix(note.String(), "release notes in notes follows stable: `k8s upgrade --version` moves it on stable alone\n") {
		t.Fatalf("%q", note.String())
	}
	e.f.listed[0].Status = "pending-upgrade"
	if _, err := in.K8sUpdates(context.Background(), "", ""); err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("a pending release: %v", err)
	}
	e.runs("2.0.0", 2)
	if u, err := in.K8sUpdates(context.Background(), "", ""); err != nil || u.Available {
		t.Fatalf("the release it runs: %+v %v", u, err)
	}
	in.R.Keys = map[string][]byte{"release.pub": k.PEM}
	if _, err := in.K8sUpdates(context.Background(), "", ""); err == nil || !strings.Contains(err.Error(), "is not signed by a key this install trusts") {
		t.Fatalf("unsigned: %v", err)
	}
}

// Docker's installer has `updates`, with --dir, and `k8s updates`, with the registry's flags.
func TestUpdatesIsADockerAndAKubernetesVerb(t *testing.T) {
	t.Parallel()
	r, self := testInstaller(t, "1.0.0")
	if r.Spec.Shapes.Kubernetes == nil {
		t.Fatal("the test installer has no kubernetes shape: the test proves nothing")
	}
	root := newCommand(r, self, &bytes.Buffer{}, &bytes.Buffer{}, K8sOptions{}, nil)
	for _, path := range [][]string{{"updates"}, {"k8s", "updates"}} {
		c, _, err := root.Find(path)
		if err != nil || c.Name() != "updates" {
			t.Fatalf("%v: %v", path, err)
		}
		flags := []string{"channel", "version", "json", "dir"}
		if path[0] == "k8s" {
			flags = []string{"channel", "version", "json", "image-registry", "registry-ca", "namespace"}
		}
		for _, f := range flags {
			if c.Flag(f) == nil {
				t.Errorf("%v has no --%s", path, f)
			}
		}
	}
}

// An install made with --offline never pulls: `updates` refuses, as `upgrade --version` does,
// naming the new release's own installer.
func TestUpdatesRefuseAnInstallThatNeverPulls(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, im := range []string{"front", "chown", "api", "web", "postgres"} {
		e.docker.loaded["ghcr.io/acme/notes/notes-"+im+":1.0.0"] = manifest
	}
	if err := e.install(func(o *Options) { o.Offline = true }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	_, err := e.installer(nil).Updates(context.Background(), "", "")
	if r, ok := Refusal(err); !ok || !strings.Contains(err.Error(), "made with --offline and never pulls") || !strings.Contains(r.Fix, "upgrade --dir") {
		t.Fatalf("%v", err)
	}
}

// Where the kept installer is another Stowage's, `updates --version`'s fix re-runs `updates` with
// that installer, never a plain `upgrade --version`, which would upgrade: one without `updates`
// checks a version with a dry run.
func TestUpdatesFixNeverUpgrades(t *testing.T) {
	t.Parallel()
	e := installed(t)
	anotherStowages(t, e)
	_, err := e.installer(nil).Updates(context.Background(), "", "2.0.0")
	r, ok := Refusal(err)
	kept := filepath.Join(e.dir, "notes-stow")
	if !ok || !strings.Contains(err.Error(), "the keys the install trusts cannot be told") || !strings.HasPrefix(r.Fix, "run `"+kept+" updates --dir "+e.dir+" --version 2.0.0`") || !strings.Contains(r.Fix, "`"+kept+" upgrade --version 2.0.0 --dry-run --dir "+e.dir+"`") {
		t.Fatalf("%v (fix %q)", err, r.Fix)
	}
}

// An install kept by an installer of another short name is handed to it: its `updates`, or a
// dry run for one from before `updates`, each command exact.
func TestUpdatesHandAnotherShortNameToItsInstaller(t *testing.T) {
	t.Parallel()
	e := installed(t)
	env := e.readEnv()
	env.Set("STOWAGE_SHORT_NAME", "old")
	if err := env.WriteFile(filepath.Join(e.dir, envName)); err != nil {
		t.Fatal(err)
	}
	_, err := e.installer(nil).Updates(context.Background(), "beta", "")
	kept := filepath.Join(e.dir, "old-stow")
	r, ok := Refusal(err)
	if !ok || !strings.Contains(err.Error(), "kept by old-stow, not notes-stow") || r.Fix != "run `"+kept+" updates --dir "+e.dir+" --channel beta` (it knows this install); an installer from before `updates` has none: `"+kept+" upgrade --version <version> --dry-run --dir "+e.dir+"` checks that version" {
		t.Fatalf("%v (fix %q)", err, r.Fix)
	}
}

// A kept installer that does not read is reported for what `updates` needs of it, never as a
// rollback's.
func TestUpdatesSayWhyTheyNeedTheKeptInstaller(t *testing.T) {
	t.Parallel()
	e := installed(t)
	if err := testutil.WriteExecutable(filepath.Join(e.dir, "notes-stow"), []byte("not an installer")); err != nil {
		t.Fatal(err)
	}
	_, err := e.installer(nil).Updates(context.Background(), "", "")
	if err == nil || !strings.Contains(err.Error(), "`updates` reads the keys the install trusts from it") || strings.Contains(err.Error(), "rollback") {
		t.Fatalf("%v", err)
	}
}
