package install

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/spec"
)

const (
	pemCA    = "ca input"
	pemOther = "other content"
)

// withFiles declares a file input CA and an optional file input EXTRA, on every shape.
func withFiles(s *spec.Spec) {
	s.Inputs = append(s.Inputs,
		spec.Input{Key: "CA", Prompt: "CA", Type: spec.InputFile, Optional: true},
		spec.Input{Key: "EXTRA", Prompt: "extra", Type: spec.InputFile, Optional: true})
}

// A file input is recorded in the release's Secret and reaches the chart as a file of the files
// Secret, one key per input, empty while the input is, named in the values.
func TestK8sFileInputsReachTheFilesSecret(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, withFiles)
	set := map[string]string{"CA": encodeFileInput([]byte(pemCA))}
	if err := e.installer(func(o *Options) { o.Set = set }).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if got := e.secretData("notes-stowage")["CA"]; got != set["CA"] {
		t.Errorf("recorded %q", got)
	}
	files := e.secretData("notes-stowage-files")
	if files["CA"] != pemCA {
		t.Errorf("files %v", files)
	}
	if v, ok := files["EXTRA"]; !ok || v != "" {
		t.Errorf("an empty input has no empty file: %v", files)
	}
	if len(e.f.given) != 1 || !strings.Contains(e.f.given[0], "files: notes-stowage-files\n") {
		t.Errorf("values %q", e.f.given)
	}
	if !strings.Contains(e.log.String(), "applying the Secrets notes-stowage, notes-stowage-files") {
		t.Errorf("log %s", e.log)
	}
}

// A release with no file input writes no files Secret; one that wrote it is emptied, as the chart
// a rollback brings back mounts it.
func TestK8sFilesSecretOfAReleaseWithoutFileInputs(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	if err := e.installer(nil).K8sInstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.secretData("notes-stowage-files") != nil || strings.Contains(e.f.given[0], "notes-stowage-files") {
		t.Errorf("a files Secret without a file input: %q", e.f.given)
	}

	e = newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.1.0", 1, nil)
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
	e.f.objects = map[string]string{"secret/notes-stowage-files": outputsJSON(map[string]string{"CA": pemCA})}
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if got := e.secretData("notes-stowage-files")["CA"]; got != pemCA {
		t.Errorf("not kept: %v", e.secretData("notes-stowage-files"))
	}
}

// After a dropping upgrade the release's Secret no longer records the input. A retry of that upgrade
// keeps the file for the release still running; a release declaring the input again asks it, as any
// input it adds, and what leaving it empty empties is said.
func TestK8sFilesSecretHoldsADroppedFileUntilAsked(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("2.0.0", 1, nil)
	e.runs("1.0.0", 1)
	rec := map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}
	e.stored(rec, ours)
	e.f.objects = map[string]string{"secret/notes-stowage-files": outputsJSON(map[string]string{"CA": pemCA})}
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if got := e.secretData("notes-stowage-files")["CA"]; got != pemCA {
		t.Errorf("the retry emptied it: %q", got)
	}
	declaring := func(required bool) *kubeEnv {
		e := newKubeEnv(t, spec.Provider{Name: "none"})
		e.release = e.at("1.1.0", 1, func(s *spec.Spec) {
			s.Inputs = append(s.Inputs, spec.Input{Key: "CA", Prompt: "CA", Type: spec.InputFile, Required: required, Optional: !required})
		})
		e.runs("1.0.0", 1)
		e.stored(rec, ours)
		e.f.objects = map[string]string{"secret/notes-stowage-files": outputsJSON(map[string]string{"CA": pemCA})}
		return e
	}
	e = declaring(false)
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if got, ok := e.secretData("notes-stowage-files")["CA"]; !ok || got != "" || !strings.Contains(e.log.String(), "input CA: nothing recorded, so its file in the Secret notes-stowage-files is emptied") {
		t.Errorf("%q\n%s", got, e.log)
	}
	e = declaring(true)
	if err := e.installer(nil).K8sUpgrade(context.Background()); err == nil || !strings.Contains(err.Error(), "missing required input") {
		t.Errorf("required: %v", err)
	}
	e = declaring(true)
	set := map[string]string{"CA": encodeFileInput([]byte(pemOther))}
	if err := e.installer(func(o *Options) { o.Set = set }).K8sUpgrade(context.Background()); err != nil || e.secretData("notes-stowage-files")["CA"] != pemOther {
		t.Errorf("--set-file: %v, %v", err, e.secretData("notes-stowage-files"))
	}
	if strings.Contains(e.log.String(), "emptied") {
		t.Errorf("log %s", e.log)
	}
}

// Every deploy names the files Secret or none: a value reused could name one deleted since.
func TestK8sUpgradeGivesTheFilesValueWhenThereIsNone(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.1.0", 1, nil)
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if e.secretData("notes-stowage-files") != nil || len(e.f.given) != 1 || !strings.Contains(e.f.given[0], `files: ""`) {
		t.Errorf("values %q", e.f.given)
	}
}

// A release keeping a file input it no longer asks here keeps the key's place in the Secret, and
// the hook naming it mounts nothing: its value is no file of this shape.
func TestK8sHookOfAFileInputNoLongerAskedHere(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.1.0", 1, func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "CA", Prompt: "CA", Type: spec.InputFile, Optional: true, Shapes: []string{"docker"}})
		s.Hooks.Provision.Config["env"] = []any{"ADMIN_EMAIL", "CA"}
	})
	e.published(e.release)
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p", "CA": encodeFileInput([]byte(pemCA))}, ours)
	e.hookRuns("Succeeded", "", nil)
	if err := e.installer(func(o *Options) { o.NoProvision = false }).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	env := envOf(e.container("provision"))
	if _, ok := env["CA"]; ok {
		t.Errorf("the recorded form reaches the hook: %v", env)
	}
	if _, ok := env[spec.FileVar("CA")]; ok {
		t.Errorf("a path to no file: %v", env)
	}
	if n := len(e.container("provision")["volumeMounts"].([]any)); n != 1 {
		t.Errorf("mounts %v", e.container("provision")["volumeMounts"])
	}
}

// A file input a release drops keeps its file through the upgrade that drops it, for the chart a
// rollback brings back, though the release's Secret lets it go.
func TestK8sFilesSecretKeepsADroppedInputsFile(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.publishWith("1.0.0", func(s *spec.Spec) {
		atBaseline(1)(s)
		s.Inputs = append(s.Inputs, spec.Input{Key: "CA", Prompt: "CA", Type: spec.InputFile, Optional: true})
	})
	e.runs("1.0.0", 1)
	e.release = e.at("2.0.0", 1, atBaseline(1))
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p", "CA": encodeFileInput([]byte(pemCA))}, ours)
	e.f.objects = map[string]string{"secret/notes-stowage-files": outputsJSON(map[string]string{"CA": pemCA})}
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if _, ok := e.secretData("notes-stowage")["CA"]; ok || !strings.Contains(e.log.String(), "input CA is not in notes 2.0.0: dropped") {
		t.Errorf("not dropped: %v\n%s", e.secretData("notes-stowage"), e.log)
	}
	if got := e.secretData("notes-stowage-files")["CA"]; got != pemCA {
		t.Errorf("files %q", got)
	}
}

// A file input narrowed off this shape keeps its file while the release's Secret records it, for the
// chart a rollback brings back.
func TestK8sFilesSecretKeepsANarrowedInputsFile(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.1.0", 1, func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "CA", Prompt: "CA", Type: spec.InputFile, Optional: true, Shapes: []string{"docker"}})
	})
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p", "CA": encodeFileInput([]byte(pemCA))}, ours)
	e.f.objects = map[string]string{"secret/notes-stowage-files": outputsJSON(map[string]string{"CA": pemCA})}
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if got := e.secretData("notes-stowage-files")["CA"]; got != pemCA {
		t.Errorf("files %q", got)
	}
}

// With no file input declared, another tool's Secret of the name is left alone, not refused.
func TestK8sFilesSecretNameIsFreeWithoutAFileInput(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.f.objects = map[string]string{"secret/notes-stowage-files": `{"metadata":{"labels":{}},"data":{"X":""}}`}
	if err := e.installer(nil).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if e.secretData("notes-stowage-files") != nil {
		t.Error("written over")
	}
}

// An upgrade asking a required file input recorded empty restarts what mounts it, though nothing
// was given: helm rolls only what its templates change.
func TestK8sUpgradeRestartsWhatReadsARecordedValueItChanges(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.1.0", 1, func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "CA", Prompt: "CA", Type: spec.InputFile, Required: true}, spec.Input{Key: "REGION", Prompt: "r"})
	})
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p", "CA": ""}, ours)
	e.f.manifest = "kind: Deployment\nmetadata: {name: notes-mounts}\nspec: {template: {spec: {volumes: [{name: ca, secret: {secretName: notes-stowage-files}}]}}}\n"
	set := map[string]string{"CA": encodeFileInput([]byte(pemCA)), "REGION": "us"}
	if err := e.installer(func(o *Options) { o.Set = set }).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	// REGION is an input this release adds: its readers are new with it.
	if want := []string{"deployment/notes-mounts"}; !slices.Equal(restarted(e), want) {
		t.Errorf("restarted %q", restarted(e))
	}
}

// Another tool's Secret of the name is never written over.
func TestK8sNeverWritesOverAFilesSecretItDidNotWrite(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, withFiles)
	e.f.objects = map[string]string{"secret/notes-stowage-files": `{"metadata":{"labels":{}},"data":{}}`}
	err := e.installer(nil).K8sInstall(context.Background())
	if err == nil || !strings.Contains(err.Error(), "secret notes-stowage-files in notes was not written by notes-stow") {
		t.Fatalf("%v", err)
	}
	if len(e.f.applied) != 0 {
		t.Errorf("applied %v", e.f.applied)
	}
}

// What --dry-run prints shows a file input by its digest, and names the files Secret's keys.
func TestK8sDryRunShowsAFileInputByItsDigest(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, withFiles)
	set := map[string]string{"CA": encodeFileInput([]byte(pemCA))}
	if err := e.installer(func(o *Options) { o.Set, o.DryRun = set, true }).K8sInstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := e.out.String()
	if strings.Contains(out, set["CA"]) || !strings.Contains(out, "  CA="+fileInputDigest(set["CA"])) || !strings.Contains(out, "secret notes-stowage-files: the file inputs CA, EXTRA\n") {
		t.Errorf("printed\n%s", out)
	}
}

// givenBody is a --given body for the project notes.
func givenBody(inputs string) []byte { return []byte("project: notes\ninputs: {" + inputs + "}\n") }

// upgradeEnv is a release running 1.0.0 with a CA and a secret TOKEN recorded, and three workloads.
func upgradeEnv(t *testing.T) *kubeEnv {
	t.Helper()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, func(s *spec.Spec) {
		withFiles(s)
		s.Inputs = append(s.Inputs, spec.Input{Key: "TOKEN", Prompt: "t", Secret: true}, spec.Input{Key: "REGION", Prompt: "r"})
	})
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p", "TOKEN": "old-token", "REGION": "eu",
		"CA": encodeFileInput([]byte(pemCA)), "EXTRA": ""}, ours)
	e.f.objects = map[string]string{"secret/notes-stowage-files": outputsJSON(map[string]string{"CA": pemCA, "EXTRA": ""})}
	reads := func(s string) string { return "spec: {template: {spec: {containers: [{" + s + "}]}}}\n" }
	e.f.manifest = "kind: Deployment\nmetadata: {name: notes-token}\n" + reads("env: [{name: T, valueFrom: {secretKeyRef: {name: notes-stowage, key: TOKEN}}}]") +
		"---\nkind: Deployment\nmetadata: {name: notes-region}\n" + reads("env: [{name: R, valueFrom: {secretKeyRef: {name: notes-stowage, key: REGION}}}]") +
		"---\nkind: StatefulSet\nmetadata: {name: notes-mounts}\nspec: {template: {spec: {volumes: [{name: ca, secret: {secretName: notes-stowage-files}}]}}}\n"
	return e
}

func restarted(e *kubeEnv) []string {
	var out []string
	for _, c := range e.f.ran("kubectl rollout") {
		if c[2] == "restart" {
			out = append(out, c[3])
		}
	}
	return out
}

// An upgrade with --given replaces the recorded values, says so with a secret redacted and a file by
// its digest, keeps the rest, and restarts what reads a value given and only that.
func TestK8sUpgradeGivenReplacesRecordedValuesAndRestartsTheirReaders(t *testing.T) {
	t.Parallel()
	e := upgradeEnv(t)
	in := e.installer(nil)
	other := encodeFileInput([]byte(pemOther))
	if err := in.takeK8sGiven(givenBody("TOKEN: new-token, CA: " + other)); err != nil {
		t.Fatal(err)
	}
	if err := in.K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	env := e.secretData("notes-stowage")
	if env["TOKEN"] != "new-token" || env["CA"] != other || env["REGION"] != "eu" || env["SESSION_SECRET"] != "s" {
		t.Errorf("env %v", env)
	}
	if got := e.secretData("notes-stowage-files"); got["CA"] != pemOther {
		t.Errorf("files %v", got)
	}
	log := e.log.String()
	if !strings.Contains(log, "changing TOKEN: <redacted> → <redacted>") || !strings.Contains(log, "changing CA: "+fileInputDigest(encodeFileInput([]byte(pemCA)))+" → "+fileInputDigest(other)) {
		t.Errorf("log %s", log)
	}
	if strings.Contains(log, "new-token") || strings.Contains(log, other) || strings.Contains(e.out.String(), "new-token") {
		t.Errorf("a value in the log:\n%s\n%s", log, e.out)
	}
	if want := []string{"deployment/notes-token", "statefulset/notes-mounts"}; !slices.Equal(restarted(e), want) {
		t.Errorf("restarted %q, want %q", restarted(e), want)
	}
	// A text alone restarts no mount of the files Secret.
	e = upgradeEnv(t)
	in = e.installer(nil)
	if err := in.takeK8sGiven(givenBody("REGION: us")); err != nil {
		t.Fatal(err)
	}
	if err := in.K8sUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"deployment/notes-region"}; !slices.Equal(restarted(e), want) {
		t.Errorf("restarted %q, want %q", restarted(e), want)
	}
}

// The same value given again restarts its readers: a failed upgrade leaves the Secret ahead of them.
func TestK8sUpgradeGivenAgainRestarts(t *testing.T) {
	t.Parallel()
	e := upgradeEnv(t)
	in := e.installer(nil)
	if err := in.takeK8sGiven(givenBody("TOKEN: old-token")); err != nil {
		t.Fatal(err)
	}
	if err := in.K8sUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"deployment/notes-token"}; !slices.Equal(restarted(e), want) {
		t.Errorf("restarted %q", restarted(e))
	}
	if strings.Contains(e.log.String(), "changing TOKEN") {
		t.Errorf("no change logged as one: %s", e.log)
	}
}

// Without --given, an upgrade restarts nothing; --set of a recorded value keeps its refusal, and an
// install restarts nothing (its pods start with what is given).
func TestK8sOnlyAnUpgradeGivenRestarts(t *testing.T) {
	t.Parallel()
	e := upgradeEnv(t)
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := restarted(e); got != nil {
		t.Errorf("restarted %q", got)
	}
	e = upgradeEnv(t)
	err := e.installer(func(o *Options) { o.Set = map[string]string{"TOKEN": "x"} }).K8sUpgrade(context.Background())
	if err == nil || !strings.Contains(err.Error(), "TOKEN: recorded at the install, and an upgrade keeps it") || !strings.Contains(err.Error(), "`--given FILE` replaces a recorded input (a file as `stowage-file:<base64>`)") {
		t.Errorf("--set: %v", err)
	}
	// The hint names --given only where --given would replace the value.
	for _, key := range []string{"SESSION_SECRET", "TYPO"} {
		e = upgradeEnv(t)
		err = e.installer(func(o *Options) { o.Set = map[string]string{key: "x"} }).K8sUpgrade(context.Background())
		if err == nil || strings.Contains(err.Error(), "--given") {
			t.Errorf("%s: %v", key, err)
		}
	}
	// --set beside --given of the key: the flag wins, as on an install.
	e = upgradeEnv(t)
	in := e.installer(func(o *Options) { o.Set = map[string]string{"TOKEN": "flag"} })
	if err := in.takeK8sGiven(givenBody("TOKEN: given")); err != nil {
		t.Fatal(err)
	}
	if err := in.K8sUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.secretData("notes-stowage")["TOKEN"]; got != "flag" {
		t.Errorf("TOKEN %q", got)
	}
	e = newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, func(s *spec.Spec) { s.Inputs = append(s.Inputs, spec.Input{Key: "REGION", Prompt: "r"}) })
	e.f.manifest = "kind: Deployment\nmetadata: {name: notes-region}\nspec: {template: {spec: {containers: [{env: [{name: R, valueFrom: {secretKeyRef: {name: notes-stowage, key: REGION}}}]}]}}}\n"
	in = e.installer(nil)
	if err := in.takeK8sGiven(givenBody("REGION: us")); err != nil {
		t.Fatal(err)
	}
	if err := in.K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if got := restarted(e); got != nil || e.secretData("notes-stowage")["REGION"] != "us" {
		t.Errorf("restarted %q, %v", got, e.secretData("notes-stowage"))
	}
}

// A failed restart names the run that restarts again, which that run then does.
func TestK8sUpgradeGivenFailedRestartNamesTheRetry(t *testing.T) {
	t.Parallel()
	e := upgradeEnv(t)
	e.f.rolloutErr = errors.New("timed out")
	in := e.installer(nil)
	if err := in.takeK8sGiven(givenBody("TOKEN: new-token")); err != nil {
		t.Fatal(err)
	}
	err := in.K8sUpgrade(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the new values of TOKEN") || !strings.Contains(err.Error(), "`kubectl rollout restart deployment/notes-token --namespace notes --context kind-current` restarts what reads them") || strings.Contains(err.Error(), "provision hook") {
		t.Fatalf("%v", err)
	}
	// With a hook to run, it says the hook has not run; the release's endpoint and tips are printed.
	e = upgradeEnv(t)
	e.f.rolloutErr = errors.New("timed out")
	e.release.Spec.Handoffs = []spec.Handoff{testHandoff()}
	e.published(e.release)
	e.f.manifest += "---\nkind: Deployment\nmetadata: {name: notes-api}\n"
	e.hookRuns("Succeeded", "", nil)
	in = e.installer(func(o *Options) { o.NoProvision = false })
	if err := in.takeK8sGiven(givenBody("TOKEN: new-token")); err != nil {
		t.Fatal(err)
	}
	err = in.K8sUpgrade(context.Background())
	if err == nil || !strings.Contains(err.Error(), "k8s provision") || !strings.Contains(err.Error(), "which has not run") {
		t.Fatalf("%v", err)
	}
	if e.f.created != nil {
		t.Error("the hook ran")
	}
	if !strings.Contains(e.out.String(), "k8s kit create client") {
		t.Errorf("no tip after a failed restart: %s", e.out)
	}
}

// A dry run says what it would restart, and restarts nothing.
func TestK8sUpgradeGivenDryRun(t *testing.T) {
	t.Parallel()
	e := upgradeEnv(t)
	in := e.installer(func(o *Options) { o.DryRun = true })
	if err := in.takeK8sGiven(givenBody("TOKEN: new-token")); err != nil {
		t.Fatal(err)
	}
	if err := in.K8sUpgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := e.out.String()
	if !strings.Contains(out, "then a restart of what reads TOKEN\n") || strings.Contains(out, "new-token") || restarted(e) != nil || e.f.applied != nil {
		t.Errorf("printed\n%s", out)
	}
}

// What --given brings on Kubernetes: another shape's input and tls left out and said, an unknown
// input and another project refused, an answers file winning.
func TestTakeK8sGiven(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "REGION", Prompt: "r"}, spec.Input{Key: "OTHER", Prompt: "r"},
			spec.Input{Key: "DOCKER_ONLY", Prompt: "d", Shapes: []string{"docker"}}, spec.Input{Key: "IN_TLS", Prompt: "d", When: "tls"})
	})
	in := e.installer(func(o *Options) { o.Answers = map[string]string{"OTHER": "answered"} })
	raw := "project: notes\ninputs: {REGION: us, OTHER: given, DOCKER_ONLY: x, IN_TLS: y}\ntls: {client_ca: \"" + "x" + "\"}\n"
	if err := in.takeK8sGiven([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if a := in.O.Answers; a["REGION"] != "us" || a["OTHER"] != "answered" || len(a) != 2 {
		t.Errorf("answers %v", a)
	}
	if got := sortedKeys(in.O.givenInputs()); !slices.Equal(got, []string{"OTHER", "REGION"}) {
		t.Errorf("given %v", got)
	}
	log := e.log.String()
	if !strings.Contains(log, "--given: DOCKER_ONLY, IN_TLS left out: not an input on Kubernetes") || !strings.Contains(log, "--given: tls left out: on Kubernetes the chart's ingress owns the certificate") {
		t.Errorf("log %s", log)
	}
	in = e.installer(nil)
	if err := in.takeK8sGiven(givenBody("NOPE: x, ALSO: y")); err == nil || !strings.Contains(err.Error(), "declares no input ALSO, NOPE") {
		t.Errorf("unknown: %v", err)
	}
	if err := in.takeK8sGiven([]byte("project: other\ninputs: {REGION: us}\n")); err == nil {
		t.Error("another project's passed")
	}
	if in.O.Given != nil || in.O.Answers != nil {
		t.Errorf("a refusal kept %v %v", in.O.Given, in.O.Answers)
	}
}

// An upgrade restarts the readers of what is given, changed or not, and of a recorded input it
// changes; never of a value it adds, or of one no input holds, as a rotation's.
func TestK8sRestartList(t *testing.T) {
	t.Parallel()
	e := upgradeEnv(t)
	in := e.installer(nil)
	in.recorded = map[string]string{"REGION": "eu", "TOKEN": "t", "SESSION_SECRET_ACCEPTED": "x"}
	in.O.Given = &Given{Inputs: map[string]string{"TOKEN": "t"}}
	got := in.k8sRestartList([]string{"CA", "REGION", "TOKEN", "SESSION_SECRET_ACCEPTED"})
	if !slices.Equal(got, []string{"REGION", "TOKEN"}) {
		t.Errorf("%v", got)
	}
}

// A value in the recorded form shows by its digest, though the release declares it as text.
func TestShownValueOfARecordedFile(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	in := e.installer(nil)
	v := encodeFileInput([]byte(pemCA))
	if got := in.shownValue("ADMIN_EMAIL", v, nil); got != fileInputDigest(v) {
		t.Errorf("%q", got)
	}
	if got := in.shownValue("ADMIN_EMAIL", "a@example.org", nil); got != "a@example.org" {
		t.Errorf("%q", got)
	}
}

// reset-db takes a file by --set-file across a baseline alone, and its refusal of a value it cannot
// read names no flag it does not take.
func TestK8sResetSetFile(t *testing.T) {
	t.Parallel()
	e := upgradeEnv(t)
	err := e.installer(func(o *Options) { o.Yes, o.Set = true, map[string]string{"CA": "x"} }).K8sReset(context.Background())
	if err == nil || !strings.Contains(err.Error(), "only --new-baseline asks") {
		t.Errorf("%v", err)
	}
}

// A release that cannot read what is recorded is refused before anything is written, unless the
// value is being replaced; a required file input recorded empty is asked as an input it adds.
func TestK8sRecordedForms(t *testing.T) {
	t.Parallel()
	fileRelease := func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "CA", Prompt: "CA", Type: spec.InputPEM, Optional: true})
	}
	start := func(data map[string]string) *kubeEnv {
		e := newKubeEnv(t, spec.Provider{Name: "none"})
		e.release = e.at("1.1.0", 1, fileRelease)
		e.runs("1.0.0", 1)
		data["ADMIN_EMAIL"], data["SESSION_SECRET"], data["POSTGRES_PASSWORD"] = "a@example.org", "s", "p"
		e.stored(data, ours)
		return e
	}
	e := start(map[string]string{"CA": "text"})
	err := e.installer(nil).K8sUpgrade(context.Background())
	if err == nil || !strings.Contains(err.Error(), "CA: notes 1.1.0 declares it as") || strings.Contains(err.Error(), "--given") || len(e.f.applied) != 0 {
		t.Errorf("%v, applied %v", err, e.f.applied)
	}
	// A value given does not get past it: the release a rollback brings back reads it too.
	e = start(map[string]string{"CA": "text"})
	in := e.installer(nil)
	if err := in.takeK8sGiven(givenBody("CA: " + encodeFileInput([]byte("-----BEGIN X-----\n-----END X-----\n")))); err != nil {
		t.Fatal(err)
	}
	if err := in.K8sUpgrade(context.Background()); err == nil || !strings.Contains(err.Error(), "CA: notes 1.1.0 declares it as") || len(e.f.applied) != 0 {
		t.Errorf("%v", err)
	}
	// A given value replaces one of the right form this release's checks refuse: a rollback reads it.
	e = start(map[string]string{"CA": encodeFileInput([]byte("not pem"))})
	in = e.installer(nil)
	good := encodeFileInput([]byte("-----BEGIN X-----\n-----END X-----\n"))
	if err := in.takeK8sGiven(givenBody("CA: " + good)); err != nil {
		t.Fatal(err)
	}
	if err := in.K8sUpgrade(context.Background()); err != nil || e.secretData("notes-stowage")["CA"] != good {
		t.Errorf("%v\n%s", err, e.log)
	}
	// Kept, it is refused.
	e = start(map[string]string{"CA": encodeFileInput([]byte("not pem"))})
	if err := e.installer(nil).K8sUpgrade(context.Background()); err == nil || !strings.Contains(err.Error(), "CA: notes 1.1.0 declares it as") {
		t.Errorf("%v", err)
	}
	// A given value of the right form is checked as this release takes it.
	e = start(map[string]string{"CA": ""})
	in = e.installer(nil)
	if err := in.takeK8sGiven(givenBody("CA: " + encodeFileInput([]byte("nope")))); err != nil {
		t.Fatal(err)
	}
	if err := in.K8sUpgrade(context.Background()); err == nil || !strings.Contains(err.Error(), "CA: no PEM block") {
		t.Errorf("%v", err)
	}
	// An install over a kept Secret replaces what it is given, there being no release to go back to.
	e = newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, fileRelease)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p", "CA": "text"}, ours)
	if err := e.installer(func(o *Options) { o.Set = map[string]string{"CA": good} }).K8sInstall(context.Background()); err != nil {
		t.Errorf("install giving it: %v", err)
	}
	e = newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, fileRelease)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p", "CA": "text"}, ours)
	if err := e.installer(nil).K8sInstall(context.Background()); err == nil || !strings.Contains(err.Error(), "CA: notes 1.0.0 declares it as") || !strings.Contains(err.Error(), "or give it a new value with this install") {
		t.Errorf("install keeping it: %v", err)
	}

	// A text input recorded as a file.
	e = newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.1.0", 1, func(s *spec.Spec) { s.Inputs = append(s.Inputs, spec.Input{Key: "REGION", Prompt: "r"}) })
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p", "REGION": encodeFileInput([]byte("x"))}, ours)
	if err := e.installer(nil).K8sUpgrade(context.Background()); err == nil || !strings.Contains(err.Error(), "REGION: notes 1.1.0 declares it as") {
		t.Errorf("text: %v", err)
	}

	// Required now, recorded empty: asked, so refused with nothing to ask with.
	e = newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.1.0", 1, func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "CA", Prompt: "CA", Type: spec.InputFile, Required: true})
	})
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p", "CA": ""}, ours)
	if err := e.installer(nil).K8sUpgrade(context.Background()); err == nil || !strings.Contains(err.Error(), "missing required input") || !strings.Contains(err.Error(), "CA") {
		t.Errorf("required: %v", err)
	}
}

// The hook gets the file of each file input its env names, read-only, at the path its variable
// holds, never the recorded form; nor can it return that variable.
func TestK8sHookGetsAFileInputAsItsFile(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, func(s *spec.Spec) {
		withFiles(s)
		s.Hooks.Provision.Config["env"] = []any{"ADMIN_EMAIL", "CA"}
	})
	e.hookRuns("Succeeded", "", nil)
	if err := e.installer(func(o *Options) { o.NoProvision = false }).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	hook := e.container("provision")
	env := envOf(hook)
	if _, ok := env["CA"]; ok {
		t.Errorf("the recorded form reaches the hook: %v", env)
	}
	if env[spec.FileVar("CA")] != HookInputsDir+"/CA" {
		t.Errorf("env %v", env)
	}
	if _, ok := env[spec.FileVar("EXTRA")]; ok {
		t.Errorf("a file its env does not name: %v", env)
	}
	mounted := false
	for _, m := range hook["volumeMounts"].([]any) {
		m := m.(map[string]any)
		if m["name"] == "inputs" {
			mounted = m["mountPath"] == HookInputsDir && m["readOnly"] == true
		}
	}
	if !mounted {
		t.Errorf("mounts %v", hook["volumeMounts"])
	}
	pod := e.f.created["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	var vol map[string]any
	for _, v := range pod["volumes"].([]any) {
		if v := v.(map[string]any); v["name"] == "inputs" {
			vol = v["secret"].(map[string]any)
		}
	}
	if vol == nil || vol["secretName"] != "notes-stowage-files" {
		t.Fatalf("volumes %v", pod["volumes"])
	}
	items := vol["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["key"] != "CA" {
		t.Errorf("items %v", items)
	}
	reserved := toStrings(e.container("record")["command"])
	if !strings.Contains(strings.Join(reserved, " "), spec.FileVar("CA")) || !strings.Contains(strings.Join(reserved, " "), spec.FileVar("EXTRA")) {
		t.Errorf("reserved %v", reserved)
	}
}

// A hook without a file input has no such mount.
func TestK8sHookWithoutAFileInputMountsNone(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, withFiles)
	e.hookRuns("Succeeded", "", nil)
	if err := e.installer(func(o *Options) { o.NoProvision = false }).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if strings.Contains(strings.Join(toStrings(e.container("provision")["command"]), " "), "inputs") || len(e.container("provision")["volumeMounts"].([]any)) != 1 {
		t.Errorf("mounts %v", e.container("provision")["volumeMounts"])
	}
}

// The values a reset across a baseline installs with name the files Secret and the secret files' one.
func TestWithFilesValue(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ in, want string }{
		{"", "stowage:\n    asFile:\n        - K\n    files: x\n    secretFiles: z\n"},
		{"stowage: {secret: s}\npostgres: {mode: external}\n", "postgres:\n    mode: external\nstowage:\n    asFile:\n        - K\n    files: x\n    secret: s\n    secretFiles: z\n"},
		{"stowage: {files: old, secretFiles: old}\n", "stowage:\n    asFile:\n        - K\n    files: x\n    secretFiles: z\n"},
	} {
		got, err := withFilesValue([]byte(c.in), "x", "z", []string{"K"})
		if err != nil || string(got) != c.want {
			t.Errorf("%q: %q, %v", c.in, got, err)
		}
	}
	if _, err := withFilesValue([]byte("a: [\n"), "x", "z", []string{"K"}); err == nil {
		t.Error("broken values passed")
	}
}

// uninstall removes the files Secret, the release's Secret being what brings it back.
func TestK8sOwnedObjectsHoldTheFilesSecret(t *testing.T) {
	t.Parallel()
	k := K8sOptions{Release: "notes"}
	if !slices.Contains(k.k8sOwnedObjects(), k8sOwned{"secret", "notes-stowage-files"}) {
		t.Errorf("%v", k.k8sOwnedObjects())
	}
}

// A run of --version onto what already runs says the inputs given are not applied.
func TestK8sUpgradeVersionToTheRunningReleaseSaysInputsAreNotApplied(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.publish("1.0.0", 1)
	e.runs("1.0.0", 1)
	if err := e.installer(func(o *Options) { o.Set = map[string]string{"CA": "x"} }).K8sUpgradeTo(context.Background(), "1.0.0", nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if !strings.Contains(e.out.String(), "the inputs given are not applied: without --version") {
		t.Errorf("%s", e.out)
	}
}

// k8s install and upgrade take --set-file and --given; k8s upgrade refuses --given with --version, as the installer it
// fetches may not know the flag and standard input is read once.
func TestK8sCommandFlags(t *testing.T) {
	t.Parallel()
	r, self := testInstaller(t, "1.0.0")
	for _, verb := range []string{"install", "upgrade"} {
		c, _, err := Command(r, self, io.Discard, io.Discard).Find([]string{"k8s", verb})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"set-file", "given"} {
			if c.Flags().Lookup(f) == nil {
				t.Errorf("k8s %s has no --%s", verb, f)
			}
		}
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "given")
	if err := os.WriteFile(file, []byte("project: notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := Command(r, self, io.Discard, io.Discard)
	cmd.SetArgs([]string{"k8s", "upgrade", "--version", "2.0.0", "--given", file, "--namespace", "prod", "--release", "a"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--given with --version") || !strings.Contains(err.Error(), "--namespace prod") || !strings.Contains(err.Error(), "--release a") || !strings.Contains(err.Error(), "-installer:<its tag>") {
		t.Errorf("%v", err)
	}
	// reset-db takes --set-file across a baseline alone.
	c, _, err := Command(r, self, io.Discard, io.Discard).Find([]string{"k8s", "reset-db"})
	if err != nil || c.Flags().Lookup("set-file") == nil {
		t.Errorf("reset-db: %v", err)
	}
}
