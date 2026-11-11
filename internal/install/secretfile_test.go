package install

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/spec"
)

// asFileSession makes SESSION_SECRET a settable secret delivered as a file.
func asFileSession(s *spec.Spec) {
	for i := range s.Secrets {
		if s.Secrets[i].Key == "SESSION_SECRET" {
			s.Secrets[i].AsFile, s.Secrets[i].Settable = true, true
		}
	}
}

// A secret delivered as a file is written under the inputs' directory, its path in its variable, a
// new value a new path; the file of the old value goes once the stack started on the new one, and
// neither the value nor the path's change is logged.
func TestASecretDeliveredAsAFileIsWrittenForTheStack(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	asFileSession(e.r.Spec)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	env := e.readEnv()
	value, first := get(env, "SESSION_SECRET"), get(env, spec.SecretFileVar("SESSION_SECRET"))
	salt := get(env, FileSaltVar)
	if value == "" || len(salt) != 32 || first != "./"+InputFilesDir+"/"+(mountedFile{Key: "SESSION_SECRET", secret: true}).name(env, value) || strings.Contains(first, fileInputName("SESSION_SECRET", value)) {
		t.Fatalf("recorded %q at %q", value, first)
	}
	info, err := os.Stat(filepath.Join(e.dir, first))
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("the file: %v, %v", info, err)
	}
	if got, _ := os.ReadFile(filepath.Join(e.dir, first)); string(got) != value {
		t.Fatalf("the file holds %q", got)
	}
	if dir, err := os.Stat(filepath.Join(e.dir, InputFilesDir)); err != nil || dir.Mode().Perm() != 0o700 {
		t.Errorf("the directory: %v, %v", dir, err)
	}
	if strings.Contains(e.log.String(), salt) {
		t.Errorf("the install logs the salt: %s", e.log)
	}
	e.log.Reset()
	if err := e.installer(nil).SetSecret(context.Background(), "SESSION_SECRET", "chosen-value"); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	second := get(e.readEnv(), spec.SecretFileVar("SESSION_SECRET"))
	if second == first {
		t.Fatal("a new value kept its path: compose would not recreate the services mounting it")
	}
	if got, _ := os.ReadFile(filepath.Join(e.dir, second)); string(got) != "chosen-value" {
		t.Fatalf("the new file holds %q", got)
	}
	if names := inputFiles(t, e.dir); len(names) != 1 || "./"+InputFilesDir+"/"+names[0] != second {
		t.Errorf("after the start: %v", names)
	}
	if get(e.readEnv(), FileSaltVar) != salt {
		t.Error("the salt changed")
	}
	if log := e.log.String(); strings.Contains(log, "chosen-value") || strings.Contains(log, spec.SecretFileVarPrefix) || strings.Contains(log, salt) {
		t.Errorf("the log: %s", log)
	}
	// A release no longer delivering it so forgets the path and the file.
	for i := range e.r.Spec.Secrets {
		e.r.Spec.Secrets[i].AsFile = false
	}
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.readEnv().Get(spec.SecretFileVar("SESSION_SECRET")); ok {
		t.Error("the path is still recorded")
	}
	if _, err := os.Stat(filepath.Join(e.dir, InputFilesDir)); !os.IsNotExist(err) {
		t.Errorf("the directory stays: %v", err)
	}
}

// What a dry run shows of a secret delivered as a file: its path holds its digest, so it is redacted.
func TestADryRunRedactsTheFileOfASecret(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	asFileSession(e.r.Spec)
	for k, v := range map[string]string{spec.SecretFileVar("SESSION_SECRET"): "./.stowage-inputs/SESSION_SECRET-abc", FileSaltVar: "00ff"} {
		if got := e.installer(nil).shownValue(k, v, nil); got != "<redacted>" {
			t.Errorf("%s shown %q", k, got)
		}
	}
}

// The provision hook gets a secret its env names and that is delivered as a file as that file, never
// the variable, and only its mount; compose is given the file's path.
func TestTheHookGetsASecretDeliveredAsAFileAsItsFile(t *testing.T) {
	e := newEnv(t)
	asFileSession(e.r.Spec)
	e.r.Spec.Hooks.Provision.Config["env"] = []any{"ADMIN_EMAIL", "SESSION_SECRET"}
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	pc, err := os.ReadFile(filepath.Join(e.dir, ProvisionCompose))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Services map[string]struct {
			Environment, Volumes []string
		}
	}
	if err := yaml.Unmarshal(pc, &f); err != nil {
		t.Fatal(err)
	}
	svc := f.Services[ProvisionService]
	if !slices.Contains(svc.Environment, "STOWAGE_SECRET_FILE_SESSION_SECRET=/run/stowage/inputs/SESSION_SECRET") || slices.Contains(svc.Environment, "SESSION_SECRET") {
		t.Errorf("environment %v", svc.Environment)
	}
	if !slices.Equal(svc.Volumes, []string{"${STOWAGE_SECRET_FILE_SESSION_SECRET}:/run/stowage/inputs/SESSION_SECRET:ro"}) {
		t.Errorf("volumes %v", svc.Volumes)
	}
	path := get(e.readEnv(), spec.SecretFileVar("SESSION_SECRET"))
	if calls := e.hookCalls(); len(calls) != 1 || !slices.Contains(calls[0].Env, spec.SecretFileVar("SESSION_SECRET")+"="+path) {
		t.Errorf("compose was not given the file's path %s: %v", path, calls)
	}
}

// On Kubernetes a secret delivered as a file is in its own Secret, never the release's, which a
// chart may read whole into an environment; the values name it, an upgrade keeps its value, and the
// hook gets it as a file from it.
func TestK8sASecretDeliveredAsAFileIsInItsOwnSecret(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, func(s *spec.Spec) {
		asFileSession(s)
		s.Hooks.Provision.Config["env"] = []any{"ADMIN_EMAIL", "SESSION_SECRET"}
	})
	e.hookRuns("Succeeded", "", nil)
	if err := e.installer(func(o *Options) { o.NoProvision = false }).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if _, ok := e.secretData("notes-stowage")["SESSION_SECRET"]; ok {
		t.Fatalf("the release's Secret holds it: %v", e.secretData("notes-stowage"))
	}
	files := e.secretData("notes-stowage-secret-files")
	value := files["SESSION_SECRET"]
	if len(value) != 64 || len(files) != 1 {
		t.Fatalf("the secret files' Secret %v", files)
	}
	if got := e.f.given[0]; !strings.Contains(got, "secretFiles: notes-stowage-secret-files") || !strings.Contains(got, "asFile:\n        - SESSION_SECRET\n") {
		t.Errorf("values %s", got)
	}
	hook := e.container("provision")
	env := envOf(hook)
	if _, ok := env["SESSION_SECRET"]; ok || env[spec.SecretFileVar("SESSION_SECRET")] != HookInputsDir+"/SESSION_SECRET" {
		t.Errorf("the hook's env %v", env)
	}
	pod := e.f.created["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	var vol map[string]any
	for _, v := range pod["volumes"].([]any) {
		if v := v.(map[string]any); v["name"] == "inputs" {
			vol, _ = v["secret"].(map[string]any)
		}
	}
	if vol == nil || vol["secretName"] != "notes-stowage-secret-files" || len(vol["items"].([]any)) != 1 {
		t.Fatalf("volumes %v", pod["volumes"])
	}
	// An upgrade reads it back: the value is kept, not generated again.
	e.runs("1.0.0", 1)
	e.f.applied = nil
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if got := e.secretData("notes-stowage-secret-files")["SESSION_SECRET"]; got != value {
		t.Errorf("after the upgrade %q, want %q", got, value)
	}
	if _, ok := e.secretData("notes-stowage")["SESSION_SECRET"]; ok {
		t.Error("the upgrade wrote it into the release's Secret")
	}
	if v, err := e.installer(nil).K8sShowSecret(context.Background(), "SESSION_SECRET"); err != nil || v != value {
		t.Errorf("show: %q %v", v, err)
	}
	// A key the release's Secret held while the release that runs reads it there (its values name
	// it in no asFile) stays there with its value: a rollback brings that release back. Once the
	// release that runs delivers it as a file, the next run drops it.
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": value, "POSTGRES_PASSWORD": "p"}, ours)
	e.f.applied = nil
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if got := e.secretData("notes-stowage")["SESSION_SECRET"]; got != value {
		t.Errorf("the held key went from the release's Secret: %q", got)
	}
	// A history with a revision reading it as a variable keeps it; once every revision delivers it
	// as a file, a run drops it.
	e.f.history = []helmRevision{{Revision: 1, Status: "superseded"}, {Revision: 2, Status: "deployed"}}
	e.f.revValues = map[int]string{1: `{"stowage": {"asFile": []}}`}
	e.f.values["stowage"].(map[string]any)["asFile"] = []any{"SESSION_SECRET"}
	e.f.applied = nil
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if got := e.secretData("notes-stowage")["SESSION_SECRET"]; got != value {
		t.Errorf("a revision reading it as a variable, yet dropped: %q", got)
	}
	e.f.revValues = map[int]string{1: `{"stowage": {"asFile": ["SESSION_SECRET"]}}`}
	e.f.applied = nil
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if _, ok := e.secretData("notes-stowage")["SESSION_SECRET"]; ok {
		t.Error("the key stays in the release's Secret after a run delivering it as a file")
	}
	// A release no longer delivering it as a file keeps that Secret current: a rollback to one
	// that does mounts the value the install holds now.
	e.runs("1.0.0", 1)
	e.release = e.at("1.0.0", 1, nil)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "newer", "POSTGRES_PASSWORD": "p"}, ours)
	e.f.applied = nil
	if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if got := e.secretData("notes-stowage-secret-files")["SESSION_SECRET"]; got != "newer" {
		t.Errorf("the secret files' Secret after a release delivering none: %q", got)
	}
}

// A file input and a secret delivered as a file the hook both gets share its one directory.
func TestK8sHookGetsBothKindsOfFileInOneDirectory(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, func(s *spec.Spec) {
		withFiles(s)
		asFileSession(s)
		s.Hooks.Provision.Config["env"] = []any{"ADMIN_EMAIL", "CA", "SESSION_SECRET"}
	})
	e.hookRuns("Succeeded", "", nil)
	if err := e.installer(func(o *Options) { o.NoProvision = false }).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	pod := e.f.created["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	var sources []any
	for _, v := range pod["volumes"].([]any) {
		if v := v.(map[string]any); v["name"] == "inputs" {
			p, _ := v["projected"].(map[string]any)
			sources, _ = p["sources"].([]any)
		}
	}
	var names []string
	for _, src := range sources {
		names = append(names, src.(map[string]any)["secret"].(map[string]any)["name"].(string))
	}
	if !slices.Equal(names, []string{"notes-stowage-files", "notes-stowage-secret-files"}) {
		t.Fatalf("volumes %v", pod["volumes"])
	}
}

// A first install refuses a Secret of the secret files' name another tool wrote, before writing
// over it; a key no release declares any more stays in that Secret, never in the release's.
func TestK8sSecretFilesSecretOfAnotherToolAndADroppedKey(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, asFileSession)
	raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{"app.kubernetes.io/managed-by": "other"}}, "data": map[string]string{}})
	e.f.objects = map[string]string{"secret/notes-stowage-secret-files": string(raw)}
	if err := e.installer(nil).K8sInstall(context.Background()); err == nil || !strings.Contains(err.Error(), "secret notes-stowage-secret-files in notes was not written by") {
		t.Fatalf("another tool's: %v", err)
	}
	if len(e.f.applied) != 0 {
		t.Fatalf("applied %v", e.f.applied)
	}
	delete(e.f.objects, "secret/notes-stowage-secret-files")
	if err := e.installer(nil).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	// A release dropping SESSION_SECRET: it stays in the secret files' Secret alone.
	e.runs("1.0.0", 1)
	e.release = e.at("1.0.0", 1, func(s *spec.Spec) {
		s.Secrets = slices.DeleteFunc(s.Secrets, func(sec spec.Secret) bool { return sec.Key == "SESSION_SECRET" })
	})
	for range 2 {
		e.f.applied = nil
		if err := e.installer(nil).K8sUpgrade(context.Background()); err != nil {
			t.Fatalf("%v\n%s", err, e.log)
		}
		if _, ok := e.secretData("notes-stowage")["SESSION_SECRET"]; ok {
			t.Fatal("a dropped key delivered as a file reached the release's Secret")
		}
		if e.secretData("notes-stowage-secret-files")["SESSION_SECRET"] == "" {
			t.Error("a dropped key left the secret files' Secret, which a rollback mounts")
		}
	}
}
