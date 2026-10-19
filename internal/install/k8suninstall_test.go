package install

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/spec"
)

func (e *kubeEnv) uninstall(mod func(*Options)) error {
	return e.installer(mod).K8sUninstall(context.Background())
}

// labelled is an object as `kubectl get --output json` gives it, with these labels.
func labelled(labels map[string]string) string {
	raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	return string(raw)
}

// changes are the calls that change the cluster, in order.
func (f *fakeKube) changes() []string {
	var out []string
	for _, c := range f.ran("") {
		if s := strings.Join(c, " "); strings.HasPrefix(s, "helm uninstall") || strings.HasPrefix(s, "kubectl delete") {
			out = append(out, s)
		}
	}
	return out
}

// uninstallEnv runs release notes 1.0.0 with its StatefulSet's claim, which outlives the release,
// its Secret, the hook's account, and a ServiceAccount of the hook's name another tool made.
func uninstallEnv(t *testing.T) *kubeEnv {
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	// Without the claim the chart makes itself, which helm deletes with the release.
	_, e.f.manifest, _ = strings.Cut(resetManifest, "name: notes-uploads\n")
	e.f.claims = []string{"data-notes-postgres-0", "notes-uploads", "notes-certs", "unrelated"}
	e.f.objects = map[string]string{
		"secret/notes-stowage":                   labelled(ours),
		"secret/notes-stowage-provision":         labelled(ours),
		"role/notes-stowage-provision":           labelled(ours),
		"rolebinding/notes-stowage-provision":    labelled(ours),
		"serviceaccount/notes-stowage-provision": labelled(map[string]string{"app.kubernetes.io/managed-by": "other"}),
		"secret/notes-stowage-pull":              labelled(map[string]string{"app.kubernetes.io/managed-by": "stowage", "app.kubernetes.io/instance": "another"}),
		"job/notes-stowage":                      labelled(ours),
	}
	return e
}

func TestK8sUninstallRemovesTheReleaseAndWhatTheInstallerMadeAndKeepsTheData(t *testing.T) {
	t.Parallel()
	e := uninstallEnv(t)
	if err := e.uninstall(func(o *Options) { o.Yes = true }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	want := []string{
		"helm uninstall notes --wait --timeout 10m0s --namespace notes",
		"kubectl delete job notes-stowage --ignore-not-found --cascade=foreground --wait --timeout 10m0s --namespace notes",
		"kubectl delete rolebinding notes-stowage-provision --ignore-not-found --namespace notes",
		"kubectl delete role notes-stowage-provision --ignore-not-found --namespace notes",
	}
	// Another tool's ServiceAccount and another release's Secret of the names are kept, and the
	// claims with the Secrets that answer for them: --yes never deletes the data.
	if got := e.f.changes(); !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
	out := e.out.String()
	for _, s := range []string{"release notes in notes is uninstalled",
		"its PersistentVolumeClaims data-notes-postgres-0, which hold its data (`kubectl delete persistentvolumeclaims data-notes-postgres-0 --namespace notes` deletes them)",
		"the PersistentVolumeClaims notes-certs, which its chart keeps (`kubectl delete persistentvolumeclaims notes-certs --namespace notes` deletes them)",
		"the Secrets notes-stowage-provision, notes-stowage, which answer for the data kept: `notes-stow k8s install --namespace notes --release notes` takes it back with them (once the data's claims are deleted, `kubectl delete secret notes-stowage-provision notes-stowage --namespace notes` deletes them)",
		"serviceaccount notes-stowage-provision, secret notes-stowage-pull, which notes-stow did not make for this release",
		"the namespace notes"} {
		if !strings.Contains(out, s) {
			t.Errorf("no %q in\n%s", s, out)
		}
	}
}

func TestK8sUninstallDeletesTheDataWhenToldOrAnswered(t *testing.T) {
	t.Parallel()
	claims := "kubectl delete persistentvolumeclaims data-notes-postgres-0 --ignore-not-found --wait --timeout 10m0s --namespace notes"
	for _, c := range []struct {
		name  string
		mod   func(*Options)
		wipes bool
	}{
		{"--data --yes", func(o *Options) { o.K8s.Data, o.Yes = true, true }, true},
		{"--data, confirmed", func(o *Options) {
			o.K8s.Data, o.Prompt = true, &fakePrompter{answers: map[string]string{"confirm": "yes"}}
		}, true},
		{"answered data", func(o *Options) { o.Prompt = &fakePrompter{answers: map[string]string{"confirm": "data"}} }, true},
		{"answered keep", func(o *Options) { o.Prompt = &fakePrompter{answers: map[string]string{"confirm": "keep"}} }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := uninstallEnv(t)
			if err := e.uninstall(c.mod); err != nil {
				t.Fatal(err)
			}
			got := e.f.changes()
			if wiped := slices.Contains(got, claims); wiped != c.wipes || len(e.f.ran("helm uninstall")) != 1 {
				t.Errorf("%q", got)
			}
			if wiped := strings.Contains(e.out.String(), "which hold its data"); wiped == c.wipes {
				t.Errorf("out %s", e.out)
			}
		})
	}
}

func TestK8sUninstallAsksAndChangesNothingUnconfirmed(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		mod  func(*Options)
		want string
	}{
		{"no terminal", nil, "give --yes"},
		{"no terminal, --data", func(o *Options) { o.K8s.Data = true }, "give --yes"},
		{"not answered", func(o *Options) { o.Prompt = &fakePrompter{} }, "aborted"},
		{"--data, not answered", func(o *Options) { o.K8s.Data, o.Prompt = true, &fakePrompter{} }, "aborted"},
		{"dry run", func(o *Options) { o.DryRun = true }, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := uninstallEnv(t)
			err := e.uninstall(c.mod)
			if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
			if got := e.f.changes(); len(got) != 0 {
				t.Errorf("%q", got)
			}
			if c.want == "" && !strings.Contains(e.out.String(), "a run would uninstall release notes in notes, which runs notes 1.0.0 (revision 1), delete what the installer made beside it (job notes-stowage, rolebinding notes-stowage-provision, role notes-stowage-provision)\nit would keep the PersistentVolumeClaims data-notes-postgres-0 (--data deletes them)\nit would keep the Secrets notes-stowage-provision, notes-stowage, which answer for the data kept\n") {
				t.Errorf("out %q", e.out)
			}
		})
	}
}

func TestK8sUninstallOfAnotherReleaseOrWhatIsLeft(t *testing.T) {
	t.Parallel()
	yes := func(o *Options) { o.Yes = true }

	// A release an older installer of another project name made: removed, and named.
	e := uninstallEnv(t)
	e.f.values["stowage"].(map[string]any)["release"].(map[string]any)["name"] = "jots"
	e.f.values["stowage"].(map[string]any)["release"].(map[string]any)["version"] = "0.9.0"
	if err := e.uninstall(func(o *Options) { o.DryRun = true }); err != nil || !strings.Contains(e.out.String(), "which runs jots 0.9.0 (revision 1)") {
		t.Errorf("%v %s", err, e.out)
	}

	// A release no chart of Stowage made is never touched.
	e = uninstallEnv(t)
	e.f.values = map[string]any{"image": "x"}
	if err := e.uninstall(yes); err == nil || !strings.Contains(err.Error(), "not installed from a chart Stowage published") || len(e.f.changes()) != 0 {
		t.Errorf("%v %q", err, e.f.changes())
	}

	// helm forgot the release, what the installer made is left: it goes but for the data's Secrets,
	// as the claims cannot be told; --data deletes them too.
	for _, status := range []string{"", "uninstalled"} {
		e = uninstallEnv(t)
		e.f.listed = nil
		if status != "" {
			e.f.listed = []helmRelease{{Name: "notes", Status: status}}
		}
		if err := e.uninstall(yes); err != nil {
			t.Fatal(err)
		}
		if got := e.f.changes(); len(got) != 3 || len(e.f.ran("helm uninstall")) != 0 || len(e.f.ran("kubectl get persistentvolumeclaims")) != 0 {
			t.Errorf("%s: %q", status, got)
		}
		if !strings.Contains(e.out.String(), "any PersistentVolumeClaim the release made: helm no longer knows it, so they cannot be told (`kubectl get persistentvolumeclaims --namespace notes` lists them)") ||
			!strings.Contains(e.out.String(), "release notes in notes was uninstalled already") {
			t.Errorf("out %s", e.out)
		}
	}

	// --data too: no claim of it can be deleted, so its data may stay.
	e = uninstallEnv(t)
	e.f.listed = nil
	if err := e.uninstall(func(o *Options) { o.Yes, o.K8s.Data = true, true }); err != nil || len(e.f.changes()) != 3 ||
		!strings.Contains(e.out.String(), "once the data's claims are deleted, `kubectl delete secret notes-stowage-provision notes-stowage --namespace notes`") {
		t.Errorf("--data: %v %q\n%s", err, e.f.changes(), e.out)
	}

	// What a first uninstall kept, the data's Secrets alone: nothing to do, refused naming them.
	e = newKubeEnv(t, spec.Provider{Name: "none"})
	e.f.objects = map[string]string{"secret/notes-stowage": labelled(ours), "secret/notes-stowage-provision": labelled(ours)}
	if err := e.uninstall(yes); err == nil || !strings.Contains(err.Error(), "nothing notes-stow made for it there but the Secrets notes-stowage-provision, notes-stowage") || len(e.f.changes()) != 0 {
		t.Errorf("%v %q", err, e.f.changes())
	}

	// Nothing at all: refused.
	e = newKubeEnv(t, spec.Provider{Name: "none"})
	if err := e.uninstall(yes); err == nil || !strings.Contains(err.Error(), "no release notes in notes, and nothing notes-stow made for it there") {
		t.Errorf("%v", err)
	}
}

func TestK8sUninstallDeletesTheChartsOwnClaimsOnlyWithTheData(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		mod    func(*Options)
		answer string // what the terminal answers; empty is no terminal
		want   string
	}{
		{"--yes", func(o *Options) { o.Yes = true }, "", "makes the PersistentVolumeClaims notes-uploads, which `helm uninstall` deletes with it: `notes-stow k8s uninstall --namespace notes --release notes --data`"},
		{"dry run", func(o *Options) { o.DryRun = true }, "", "--data"},
		{"answered keep", nil, "keep", "--data"},
		{"--data --yes", func(o *Options) { o.K8s.Data, o.Yes = true, true }, "", ""},
		{"answered data", nil, "data", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := uninstallEnv(t)
			e.f.manifest = resetManifest
			p := &fakePrompter{answers: map[string]string{"confirm": c.answer}}
			err := e.uninstall(func(o *Options) {
				if c.mod != nil {
					c.mod(o)
				}
				if c.answer != "" {
					o.Prompt = p
				}
			})
			if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
			wiped := slices.Contains(e.f.changes(), "kubectl delete persistentvolumeclaims data-notes-postgres-0 notes-uploads --ignore-not-found --wait --timeout 10m0s --namespace notes")
			if wiped != (c.want == "") || c.want != "" && len(e.f.changes()) != 0 {
				t.Errorf("%q", e.f.changes())
			}
			// The terminal offers no uninstall that keeps the data helm deletes.
			for _, f := range p.forms {
				if slices.ContainsFunc(f.Questions[0].Choices, func(ch Choice) bool { return ch.Value == "keep" }) {
					t.Errorf("choices %v", f.Questions[0].Choices)
				}
			}
		})
	}
}

// A claim the chart keeps (helm.sh/resource-policy: keep) outlives the release, --data or not: the
// Secrets that answer for it stay.
func TestK8sUninstallKeepsTheSecretsOfAClaimTheChartKeeps(t *testing.T) {
	t.Parallel()
	e := uninstallEnv(t)
	_, keepOnly, _ := strings.Cut(resetManifest, "name: data\n") // the kept claim and another namespace's
	e.f.manifest = "---\n" + keepOnly
	if err := e.uninstall(func(o *Options) { o.Yes, o.K8s.Data = true, true }); err != nil {
		t.Fatal(err)
	}
	for _, c := range e.f.changes() {
		if strings.Contains(c, "delete secret") || strings.Contains(c, "persistentvolumeclaims") {
			t.Errorf("%s", c)
		}
	}
	if !strings.Contains(e.out.String(), "the PersistentVolumeClaims notes-certs, which its chart keeps") || !strings.Contains(e.out.String(), "the Secrets notes-stowage-provision, notes-stowage, which answer for the data kept") {
		t.Errorf("out %s", e.out)
	}
}
