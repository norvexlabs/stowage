package install

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// publishAt puts version (revision 1) in the kubeEnv's registry at schema baseline b, tagged as
// `runs` records it, with its installer image carrying the spec that declares the baseline.
func (e *kubeEnv) publishAt(version string, b int) {
	t := e.t
	r, exe := testInstallerOf(t, version, 1, func(s *spec.Spec) {
		e.spec(s)
		atBaseline(b)(s)
	}, func(m *release.Manifest) {
		m.Artifacts.Chart = &release.Chart{Repository: "charts/notes-stable", Version: "0.1.0", Digest: e.digest}
	})
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	p := &pubEnv{env: &env{t: t}, prefix: e.prefix}
	m := *r.Manifest
	m.Artifacts.Installer = p.pushInstaller("notes-stow", raw)
	p.putRelease(m.Tag, &m)
}

// baselineKube runs 1.0.0 at schema baseline from, with this installer carrying 2.0.0 at to.
func baselineKube(t *testing.T, from, to int) *kubeEnv {
	t.Helper()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.publishAt("1.0.0", from)
	e.runs("1.0.0", 1)
	e.release = e.at("2.0.0", 1, atBaseline(to))
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
	e.f.manifest = resetManifest
	e.f.claims = []string{"data-notes-postgres-0"}
	return e
}

func TestK8sUpgradeAcrossASchemaBaselineIsRefused(t *testing.T) {
	t.Parallel()
	e := baselineKube(t, 1, 2)
	err := e.installer(nil).K8sUpgrade(context.Background())
	if err == nil || !strings.Contains(err.Error(), "notes 2.0.0 starts schema baseline 2, and release notes in notes runs notes 1.0.0, at baseline 1") ||
		!strings.Contains(err.Error(), "`notes-stow k8s reset-db --namespace notes --release notes --new-baseline` wipes it") {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if len(e.f.applied) != 0 || len(e.f.ran("helm upgrade")) != 0 {
		t.Error("changed the cluster")
	}
	// Positive control: the same baseline upgrades.
	e = baselineKube(t, 2, 2)
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("same baseline: %v\n%s", err, e.log)
	}
	if len(e.f.ran("helm upgrade")) != 1 {
		t.Errorf("same baseline did not upgrade")
	}
}

// A baseline that cannot be told (the running release's manifest is not in the registry) is
// logged and not checked, as on Docker.
func TestK8sUpgradeWithAnUnknownBaselineIsNotRefused(t *testing.T) {
	t.Parallel()
	e := baselineKube(t, 1, 2)
	e.runs("1.0.1", 1) // never published
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if !strings.Contains(e.log.String(), "the schema baseline of notes 1.0.1 is not known") {
		t.Errorf("log %s", e.log)
	}
}

func TestK8sResetCrossesASchemaBaselineOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	yes := func(o *Options) { o.Yes = true }
	e := baselineKube(t, 1, 2)
	if err := e.reset(yes); err == nil || !strings.Contains(err.Error(), "at schema baseline 1, and this installer carries 2.0.0 (revision 1), at baseline 2: `notes-stow k8s reset-db --namespace notes --release notes --new-baseline` wipes it and installs 2.0.0") {
		t.Fatalf("without the flag: %v", err)
	}
	if n := len(e.f.ran("helm uninstall")); n != 0 {
		t.Fatalf("%d uninstalls without the flag", n)
	}
	// The dry run names the release it installs instead.
	if err := e.reset(func(o *Options) { o.K8s.NewBaseline, o.DryRun = true, true }); err != nil ||
		!strings.Contains(e.out.String(), "then install notes 2.0.0, at the new schema baseline, with the same values") {
		t.Fatalf("dry run: %v %s", err, e.out)
	}
	e.out.Reset()
	if err := e.reset(func(o *Options) { o.Yes, o.K8s.NewBaseline = true, true }); err != nil {
		t.Fatalf("with the flag: %v\n%s", err, e.log)
	}
	if len(e.f.ran("helm uninstall")) != 1 || len(e.f.ran("helm upgrade")) != 1 || string(e.f.chart) != string(e.chart) {
		t.Errorf("uninstalls %d, installs %d", len(e.f.ran("helm uninstall")), len(e.f.ran("helm upgrade")))
	}
	if !strings.Contains(e.out.String(), "runs notes 2.0.0 (revision 1), as first installed") {
		t.Errorf("out %s", e.out)
	}
}

func TestK8sResetNewBaselineRefusals(t *testing.T) {
	t.Parallel()
	flag := func(o *Options) { o.Yes, o.K8s.NewBaseline = true, true }
	// Another release at the same baseline: an upgrade moves it, the flag wipes nothing.
	e := baselineKube(t, 2, 2)
	if err := e.reset(flag); err == nil || !strings.Contains(err.Error(), "--new-baseline only crosses a baseline; `notes-stow k8s upgrade --namespace notes --release notes` moves it") {
		t.Errorf("same baseline: %v", err)
	}
	// The release this installer carries: the flag has nothing to install instead.
	e = baselineKube(t, 1, 2)
	e.publishAt("2.0.0", 2)
	e.runs("2.0.0", 1)
	if err := e.reset(flag); err == nil || !strings.Contains(err.Error(), "already runs notes 2.0.0 (revision 1): --new-baseline installs another release") {
		t.Errorf("same release: %v", err)
	}
	// Another release whose baseline cannot be told: refused, never wiped on a guess.
	e = baselineKube(t, 1, 2)
	e.runs("1.0.1", 1)
	if err := e.reset(flag); err == nil || !strings.Contains(err.Error(), "the schema baseline of notes 1.0.1, which release notes in notes runs, cannot be told") {
		t.Errorf("unknown baseline: %v", err)
	}
	// A running release signed by no key this installer trusts reads as not known: refused too.
	e = baselineKube(t, 1, 2)
	e.release.Keys = map[string][]byte{"release.pub": testutil.NewKeySigner(t).PEM}
	if err := e.reset(flag); err == nil || !strings.Contains(err.Error(), "cannot be told") {
		t.Errorf("unsigned: %v", err)
	}
	if n := len(e.f.ran("helm uninstall")); n != 0 {
		t.Errorf("%d uninstalls", n)
	}
}

// The new release's Secrets are written before the wipe: a secret it adds reaches the cluster.
func TestK8sResetAcrossABaselineWritesWhatTheNewReleaseAdds(t *testing.T) {
	t.Parallel()
	e := baselineKube(t, 1, 2)
	e.release = e.at("2.0.0", 1, func(s *spec.Spec) {
		atBaseline(2)(s)
		s.Secrets = append(s.Secrets, spec.Secret{Key: "NEW_KEY", Gen: "hex16"})
	})
	if err := e.reset(func(o *Options) { o.K8s.NewBaseline, o.DryRun = true, true }); err != nil ||
		!strings.HasPrefix(e.out.String(), "a run would write the secrets and inputs notes 2.0.0 adds (NEW_KEY), uninstall release notes") {
		t.Fatalf("dry run: %v %s", err, e.out)
	}
	if err := e.reset(func(o *Options) { o.Yes, o.K8s.NewBaseline = true, true }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if got := e.secretData("notes-stowage"); len(got["NEW_KEY"]) != 32 || got["SESSION_SECRET"] != "s" {
		t.Errorf("secret %v", got)
	}
	// Written before the uninstall, so a failure after it never leaves the key missing.
	var applied, uninstalled int
	for i, c := range e.f.calls {
		s := strings.Join(c, " ")
		if strings.HasPrefix(s, "kubectl apply") && applied == 0 {
			applied = i + 1
		}
		if strings.HasPrefix(s, "helm uninstall") {
			uninstalled = i + 1
		}
	}
	if applied == 0 || uninstalled == 0 || applied > uninstalled {
		t.Errorf("apply %d, uninstall %d", applied, uninstalled)
	}
}
