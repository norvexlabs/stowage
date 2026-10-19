package install

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/spec"
)

// hookRuns makes the fake cluster run the provision Job: its pod ends in phase, record's message
// msg (when it failed), and the outputs Secret then holds outs.
func (e *kubeEnv) hookRuns(phase, msg string, outs map[string]string) {
	e.f.runJob = func(map[string]any) (string, string, map[string]string) {
		done := func(name string, code int, msg string) any {
			return map[string]any{"name": name, "state": map[string]any{"terminated": map[string]any{"exitCode": code, "message": msg}}}
		}
		status := map[string]any{"phase": phase, "initContainerStatuses": []any{done("copy", 0, ""), done("check", 0, "")},
			"containerStatuses": []any{done("provision", 0, ""), done("record", 0, "")}}
		if phase == "Failed" {
			status["containerStatuses"] = []any{done("provision", 0, ""), done("record", 1, msg)}
		}
		pod, _ := json.Marshal(map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"name": "notes-stowage-x1"}, "status": status}}})
		return string(pod), outputsJSON(outs), map[string]string{"provision": "registering\n", "record": "recorded\n"}
	}
}

// outputsJSON is the outputs Secret holding outs, as kubectl gives it.
func outputsJSON(outs map[string]string) string {
	enc := map[string]string{}
	for k, v := range outs {
		enc[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": ours}, "data": enc})
	return string(raw)
}

// container is the named container of the created Job's pod.
func (e *kubeEnv) container(name string) map[string]any {
	e.t.Helper()
	pod := e.f.created["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	for _, list := range []string{"initContainers", "containers"} {
		for _, c := range pod[list].([]any) {
			if c := c.(map[string]any); c["name"] == name {
				return c
			}
		}
	}
	e.t.Fatalf("no container %s in %v", name, pod)
	return nil
}

// envOf is a container's environment: a literal value, or "<secret>/<key>".
func envOf(c map[string]any) map[string]string {
	out := map[string]string{}
	for _, v := range c["env"].([]any) {
		v := v.(map[string]any)
		if from, ok := v["valueFrom"].(map[string]any); ok {
			r := from["secretKeyRef"].(map[string]any)
			out[v["name"].(string)] = r["name"].(string) + "/" + r["key"].(string)
		} else {
			out[v["name"].(string)] = v["value"].(string)
		}
	}
	return out
}

func mounts(c map[string]any) []string {
	var out []string
	for _, m := range c["volumeMounts"].([]any) {
		out = append(out, m.(map[string]any)["mountPath"].(string))
	}
	return out
}

// The hook runs after the release is deployed, as a Job of the release's installer image and its
// hook image: it receives the keys its env declares, by reference to the release's Secret, and no
// other; only the record step holds the ServiceAccount's token; the Job is deleted after.
func TestK8sInstallRunsTheHookAsAJobAfterTheRelease(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.hookRuns("Succeeded", "", map[string]string{"ADMIN_PASSWORD": "minted"})
	// --set gives an input here: the hook's own values are not touched.
	if err := e.installer(func(o *Options) { o.NoProvision, o.Set = false, map[string]string{"ADMIN_EMAIL": "set@example.org"} }).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if len(e.f.patches) != 0 {
		t.Errorf("an input's --set patched the hook's outputs: %q", e.f.patches)
	}
	var order []string
	for _, c := range e.f.calls {
		if s := strings.Join(c[:2], " "); slices.Contains([]string{"helm upgrade", "kubectl create", "kubectl delete"}, s) {
			order = append(order, s+" "+c[2])
		}
	}
	if strings.Join(order, ", ") != "helm upgrade --install, kubectl create --filename, kubectl delete job" {
		t.Fatalf("order %q", order)
	}
	job := e.f.created
	md := job["metadata"].(map[string]any)
	if md["name"] != "notes-stowage" || md["namespace"] != "notes" {
		t.Errorf("job %v", md)
	}
	hook, rec, cp := e.container("provision"), e.container("record"), e.container("copy")
	ctl := e.prefix + "/notes-installer@" + ctlDigest
	if hook["image"] != e.prefix+"/notes-api@"+digest || rec["image"] != ctl || cp["image"] != ctl {
		t.Errorf("images %v %v %v", hook["image"], rec["image"], cp["image"])
	}
	if got := strings.Join(toStrings(hook["command"]), " "); got != "/stowage/notes-stow __k8s-hook run --dir /stowage --timeout 30m0s -- notes-api create-admin" {
		t.Errorf("hook command %s", got)
	}
	env := envOf(hook)
	if env["ADMIN_EMAIL"] != "notes-stowage/ADMIN_EMAIL" || env["STOWAGE_NAME"] != "notes" || env["STOWAGE_PROJECT_VERSION"] != "1.0.0" || len(env) != 4 {
		t.Errorf("the hook's environment: %v", env)
	}
	if sc := hook["securityContext"].(map[string]any); sc["runAsNonRoot"] != true || sc["allowPrivilegeEscalation"] != false {
		t.Errorf("the hook's securityContext: %v", sc)
	}
	// The check step proves the way to the Secret before the hook runs; it holds the token too.
	if chk := e.container("check"); !strings.Contains(strings.Join(toStrings(chk["command"]), " "), "record --check --token /var/run/stowage --namespace notes --secret notes-stowage-provision") || !slices.Contains(mounts(chk), tokenMount) {
		t.Errorf("check %v", chk)
	}
	if slices.Contains(mounts(hook), tokenMount) || !slices.Contains(mounts(rec), tokenMount) {
		t.Errorf("token mounts: hook %v, record %v", mounts(hook), mounts(rec))
	}
	recCmd := strings.Join(toStrings(rec["command"]), " ")
	if !strings.Contains(recCmd, "record --dir /stowage --prev /stowage-prev --token /var/run/stowage --namespace notes --secret notes-stowage-provision --reserved ADMIN_EMAIL,POSTGRES_PASSWORD,SECURE_COOKIES,SESSION_SECRET") {
		t.Errorf("record command %s", recCmd)
	}
	pod := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if pod["automountServiceAccountToken"] != false || pod["serviceAccountName"] != "notes-stowage-provision" || pod["restartPolicy"] != "Never" {
		t.Errorf("pod %v", pod)
	}
	// The record step may patch the outputs Secret and nothing else.
	var role map[string]any
	for _, o := range e.f.applied {
		if o["kind"] == "Role" {
			role = o
		}
	}
	rules, _ := json.Marshal(role["rules"])
	if string(rules) != `[{"apiGroups":[""],"resourceNames":["notes-stowage-provision"],"resources":["secrets"],"verbs":["patch"]}]` {
		t.Errorf("role rules %s", rules)
	}
	if !strings.Contains(e.out.String(), "provisioned: ADMIN_PASSWORD (in the Secret notes-stowage-provision)") || !strings.Contains(e.log.String(), "registering") {
		t.Errorf("out %s\nlog %s", e.out, e.log)
	}
}

func toStrings(v any) []string {
	switch v := v.(type) {
	case []string:
		return v
	case []any:
		var out []string
		for _, s := range v {
			out = append(out, s.(string))
		}
		return out
	}
	return nil
}

// What the hook returned before is handed back to the next run, key by key, but for a key the
// release now records, which is dropped from the outputs.
func TestK8sHookGetsWhatItReturnedBefore(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
	e.f.outputs = outputsJSON(map[string]string{"ADMIN_PASSWORD": "minted", "SESSION_SECRET": "stale"})
	e.hookRuns("Succeeded", "", map[string]string{"ADMIN_PASSWORD": "minted"})
	if err := e.installer(nil).K8sProvision(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	env := envOf(e.container("provision"))
	if env["ADMIN_PASSWORD"] != "notes-stowage-provision/ADMIN_PASSWORD" || env["SESSION_SECRET"] != "" {
		t.Errorf("the hook's environment: %v", env)
	}
	if len(e.f.patches) != 1 || e.f.patches[0] != `{"data":{"SESSION_SECRET":null}}` || !strings.Contains(e.log.String(), "dropping SESSION_SECRET") {
		t.Errorf("patches %q\n%s", e.f.patches, e.log)
	}
}

// A hook that fails leaves the upgraded release as it runs: helm rolls nothing back, and the error
// names `k8s provision` and the --set that replaces a value the hook returned.
func TestK8sHookFailureDoesNotUndoTheUpgrade(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org"}, ours)
	e.f.outputs = outputsJSON(map[string]string{"ADMIN_PASSWORD": "minted"})
	e.hookRuns("Failed", "the hook exited 1", map[string]string{"ADMIN_PASSWORD": "minted"})
	err := e.installer(func(o *Options) { o.NoProvision = false }).K8sUpgrade(context.Background())
	for _, want := range []string{"provisioning did not finish: the hook exited 1", "release notes in notes runs notes 1.0.0",
		"`notes-stow k8s provision --namespace notes --release notes` runs it again, and with `--set KEY=VALUE` sets a value it returned or declares it returns (ADMIN_PASSWORD) first"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("no %q in %v", want, err)
		}
	}
	if len(e.f.ran("helm rollback")) != 0 || len(e.f.ran("kubectl delete job")) != 1 {
		t.Errorf("ran %q", e.f.calls)
	}
	if !strings.Contains(e.out.String(), "release notes in notes runs notes 1.0.0") {
		t.Errorf("out %s", e.out)
	}
}

// `k8s provision --set` replaces a value the hook returned, before it runs, from a private file;
// an empty one forgets it; any other key is refused before anything changes.
func TestK8sProvisionSetReplacesAReturnedValue(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org"}, ours)
	e.f.outputs = outputsJSON(map[string]string{"ADMIN_PASSWORD": "minted", "TOKEN": "t"})
	e.hookRuns("Succeeded", "", nil)
	for set, want := range map[string]string{
		"ADMIN_EMAIL": "ADMIN_EMAIL is no value the provision hook returned or declares it returns (ADMIN_PASSWORD, TOKEN)",
		"NOPE":        "NOPE is no value",
	} {
		err := e.installer(func(o *Options) { o.Set = map[string]string{set: "x"} }).K8sProvision(context.Background())
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("--set %s: %v", set, err)
		}
	}
	if err := e.installer(func(o *Options) { o.Set = map[string]string{"TOKEN": "a'b"} }).K8sProvision(context.Background()); err == nil || !strings.Contains(err.Error(), "TOKEN: a value cannot hold") {
		t.Errorf("an unrecordable value: %v", err)
	}
	if e.f.created != nil || len(e.f.patches) != 0 {
		t.Fatal("a refused --set ran the hook or changed the outputs")
	}
	if err := e.installer(func(o *Options) { o.Set = map[string]string{"ADMIN_PASSWORD": "chosen", "TOKEN": ""} }).K8sProvision(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	want := fmt.Sprintf(`{"data":{"ADMIN_PASSWORD":%q,"TOKEN":null}}`, base64.StdEncoding.EncodeToString([]byte("chosen")))
	if len(e.f.patches) != 1 || e.f.patches[0] != want {
		t.Errorf("patches %q", e.f.patches)
	}
	if env := envOf(e.container("provision")); env["ADMIN_PASSWORD"] != "notes-stowage-provision/ADMIN_PASSWORD" || env["TOKEN"] != "" {
		t.Errorf("the hook's environment: %v", env)
	}
	if slices.ContainsFunc(e.f.calls, func(c []string) bool { return strings.Contains(strings.Join(c, " "), "chosen") }) {
		t.Error("a value reached a command line")
	}
}

// `k8s provision --set` sets a key the hook declares it returns although the outputs Secret has
// never held it: patched in before the Job runs, which hands it to the hook. A declared key the
// chart's ConfigMap sets is refused, as is one the hook neither returned nor declares.
func TestK8sProvisionSetGivesADeclaredValueTheHookNeverReturned(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release.Spec.Hooks.Provision.Config["returns"] = []any{"ADMIN_KEY", "API_URL"}
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org"}, ours)
	e.f.configMap = `{"data": {"API_URL": "http://api"}}`
	e.hookRuns("Succeeded", "", nil)
	for set, want := range map[string]string{
		"API_URL": "API_URL is no value the provision hook returned or declares it returns (ADMIN_KEY)",
		"NOPE":    "NOPE is no value the provision hook returned or declares it returns (ADMIN_KEY)",
	} {
		err := e.installer(func(o *Options) { o.Set = map[string]string{set: "x"} }).K8sProvision(context.Background())
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("--set %s: %v", set, err)
		}
	}
	if e.f.created != nil || len(e.f.patches) != 0 {
		t.Fatal("a refused --set ran the hook or changed the outputs")
	}
	if err := e.installer(func(o *Options) { o.Set = map[string]string{"ADMIN_KEY": "given"} }).K8sProvision(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	want := fmt.Sprintf(`{"data":{"ADMIN_KEY":%q}}`, base64.StdEncoding.EncodeToString([]byte("given")))
	if len(e.f.patches) != 1 || e.f.patches[0] != want {
		t.Errorf("patches %q", e.f.patches)
	}
	if env := envOf(e.container("provision")); env["ADMIN_KEY"] != "notes-stowage-provision/ADMIN_KEY" {
		t.Errorf("the hook's environment: %v", env)
	}
}

// The installer image the Job runs is the one the registry's manifest of this release names, signed:
// another revision under the tag, or no manifest, is refused before anything is deployed.
func TestK8sHookImageIsThisReleasesInstaller(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	other := *e.release.Manifest
	other.Revision, other.Artifacts.Installer = 2, ctlDigest
	(&pubEnv{env: &env{t: t}, prefix: e.prefix}).putRelease(other.Tag, &other)
	err := e.installer(func(o *Options) { o.NoProvision = false }).K8sInstall(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is notes 1.0.0 (revision 2), not this installer's 1.0.0 (revision 1)") || len(e.f.ran("helm upgrade")) != 0 {
		t.Fatalf("%v %q", err, e.f.ran("helm upgrade"))
	}
	e2 := newKubeEnv(t, spec.Provider{Name: "none"})
	e2.release = e2.at("1.0.0", 1, func(*spec.Spec) {})
	e2.release.Manifest.Tag = "1.0.0-unpublished"
	err = e2.installer(func(o *Options) { o.NoProvision = false }).K8sInstall(context.Background())
	if err == nil || !strings.Contains(err.Error(), "holds no release manifest 1.0.0-unpublished") {
		t.Fatalf("%v", err)
	}
	// --no-provision needs no installer image.
	if err := e2.installer(nil).K8sInstall(context.Background()); err != nil || !strings.Contains(e2.log.String(), "not provisioned (--no-provision)") {
		t.Fatalf("%v\n%s", err, e2.log)
	}
}

// A Job an earlier run left is deleted once it has ended; one that runs refuses the run.
func TestK8sHookLeftoverJob(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org"}, ours)
	left := func(conditions string) string {
		return `{"metadata": {"name": "notes-stowage", "labels": {"app.kubernetes.io/managed-by": "stowage", "app.kubernetes.io/instance": "notes"}}, "status": {"conditions": ` + conditions + `}}`
	}
	e.f.job = left(`[]`)
	if err := e.installer(nil).K8sProvision(context.Background()); err == nil || !strings.Contains(err.Error(), "the provision Job notes-stowage in notes still runs") {
		t.Fatalf("%v", err)
	}
	if e.f.created != nil {
		t.Fatal("created a Job beside a running one")
	}
	e.f.job = `{"metadata": {"name": "notes-stowage", "labels": {}}}`
	if err := e.installer(nil).K8sProvision(context.Background()); err == nil || !strings.Contains(err.Error(), "was not created by notes-stow") {
		t.Fatalf("%v", err)
	}
	e.f.job = left(`[{"type": "Failed", "status": "True"}]`)
	e.hookRuns("Succeeded", "", nil)
	if err := e.installer(nil).K8sProvision(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if len(e.f.ran("kubectl delete job")) != 2 || !strings.Contains(e.log.String(), "deleting the provision Job notes-stowage an earlier run left") {
		t.Errorf("ran %q\n%s", e.f.ran("kubectl delete"), e.log)
	}
}

// A pod that cannot start is given up, not waited for until the Job's deadline.
func TestK8sHookPodThatCannotStart(t *testing.T) {
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org"}, ours)
	e.f.runJob = func(map[string]any) (string, string, map[string]string) {
		return `{"items": [{"metadata": {"name": "p"}, "status": {"phase": "Pending", "initContainerStatuses": [{"name": "copy", "state": {"waiting": {"reason": "ImagePullBackOff", "message": "pull refused"}}}]}}]}`, "", nil
	}
	poll, stuck := k8sHookPoll, k8sHookStuck
	k8sHookPoll, k8sHookStuck = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { k8sHookPoll, k8sHookStuck = poll, stuck })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // a regression fails, never hangs
	defer cancel()
	err := e.installer(nil).K8sProvision(ctx)
	if err == nil || !strings.Contains(err.Error(), "its container copy cannot start: ImagePullBackOff: pull refused") {
		t.Fatalf("%v", err)
	}
	if len(e.f.ran("kubectl delete job")) != 1 {
		t.Errorf("the Job was not deleted: %q", e.f.calls)
	}
}

// k8s provision is run by the installer of the release that runs, on a release with a hook.
func TestK8sProvisionRefusals(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	if err := e.installer(nil).K8sProvision(context.Background()); err == nil || !strings.Contains(err.Error(), "no release notes in notes") {
		t.Errorf("no release: %v", err)
	}
	e.runs("0.9.0", 1)
	if err := e.installer(nil).K8sProvision(context.Background()); err == nil || !strings.Contains(err.Error(), "provision it with the installer of 0.9.0") {
		t.Errorf("another release: %v", err)
	}
	e.runs("1.0.0", 1)
	if err := e.installer(nil).K8sProvision(context.Background()); err == nil || !strings.Contains(err.Error(), "has no Secret notes-stowage") {
		t.Errorf("no Secret: %v", err)
	}
	e.release.Spec.Hooks.Provision = nil
	if err := e.installer(nil).K8sProvision(context.Background()); err == nil || !strings.Contains(err.Error(), "notes has no provision hook") {
		t.Errorf("no hook: %v", err)
	}
}

// The pod's steps: `run` puts the hook's stdout in a file and records how it ended; `record` reads
// it as the Docker shape reads the hook's stdout and patches the outputs Secret with what changed
// since the run before. Its errors name keys, never a value.
func TestHookRunThenRecord(t *testing.T) {
	t.Parallel()
	dir, prev := t.TempDir(), t.TempDir()
	script := filepath.Join(t.TempDir(), "hook")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho progress >&2\necho ADMIN_PASSWORD=minted\necho KEPT=same\necho SESSION_SECRET=x\nprintf 'PARTIAL=cut'\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	if err := hookRun(context.Background(), dir, time.Minute, []string{script}, &stderr); err != nil {
		t.Fatal(err)
	}
	if stderr.String() != "progress\n" {
		t.Errorf("stderr %q", stderr.String())
	}
	_ = os.WriteFile(filepath.Join(prev, "KEPT"), []byte("same"), 0o644)
	_ = os.WriteFile(filepath.Join(prev, "OLD"), []byte("o"), 0o644)
	_ = os.WriteFile(filepath.Join(prev, "..data"), []byte("a Secret volume's own"), 0o644)
	var got struct {
		method, path, ctype, auth string
		body                      map[string]map[string]string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.ctype, got.auth = r.Method, r.URL.RequestURI(), r.Header.Get("Content-Type"), r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)
	}))
	t.Cleanup(srv.Close)
	tok := t.TempDir()
	_ = os.WriteFile(filepath.Join(tok, "token"), []byte("tok\n"), 0o600)
	var log strings.Builder
	r := &hookRecorder{dir: dir, prev: prev, token: tok, namespace: "notes", secret: "notes-stowage-provision",
		reserved: map[string]bool{"SESSION_SECRET": true}, log: &log, client: srv.Client(), api: srv.URL}
	err := r.record(context.Background())
	for _, want := range []string{"the hook exited 3", "SESSION_SECRET is a value the install records", "not recorded, it has no line break and the run did not finish"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("no %q in %v", want, err)
		}
	}
	if err != nil && (strings.Contains(err.Error(), "minted") || strings.Contains(err.Error(), "cut")) {
		t.Errorf("a value in the error: %v", err)
	}
	if got.method != http.MethodPatch || got.path != "/api/v1/namespaces/notes/secrets/notes-stowage-provision?fieldManager=stowage" ||
		got.ctype != "application/merge-patch+json" || got.auth != "Bearer tok" {
		t.Errorf("request %+v", got)
	}
	// Only what changed: KEPT is the same, OLD untouched, the refused and the cut lines left out.
	if b := got.body["data"]; len(b) != 1 || b["ADMIN_PASSWORD"] != base64.StdEncoding.EncodeToString([]byte("minted")) {
		t.Errorf("patched %v", got.body)
	}
	if log.String() != "recorded ADMIN_PASSWORD in the Secret notes-stowage-provision\n" {
		t.Errorf("log %q", log.String())
	}

	// A hook that exits 0 with its last line unbroken: that line counts, and nothing fails.
	_ = os.WriteFile(script, []byte("#!/bin/sh\nprintf 'TOKEN=t'\n"), 0o755)
	if err := hookRun(context.Background(), dir, time.Minute, []string{script}, io.Discard); err != nil {
		t.Fatal(err)
	}
	got.body = nil
	if err := r.record(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b := got.body["data"]; len(b) != 1 || b["TOKEN"] == "" {
		t.Errorf("patched %v", got.body)
	}
}

// `run` stops a hook that writes too much or runs too long, and records why; one it cannot start
// too. `record` fails each.
func TestHookRunStops(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	script := filepath.Join(t.TempDir(), "hook")
	r := &hookRecorder{dir: dir, prev: "", token: t.TempDir(), log: io.Discard, client: http.DefaultClient, api: "http://127.0.0.1:1"}
	for _, c := range []struct {
		body    string
		timeout time.Duration
		want    string
	}{
		// Past the bound, then it would run on: stopped at once (a bounded write either way).
		{fmt.Sprintf("#!/bin/sh\nhead -c %d /dev/zero | tr '\\0' A\nexec sleep 30\n", maxProvisionOut+10), 5 * time.Second, fmt.Sprintf("it wrote more than %d bytes on stdout", maxProvisionOut)},
		{"#!/bin/sh\nexec sleep 30\n", 50 * time.Millisecond, "stopped after 50ms"},
	} {
		_ = os.WriteFile(script, []byte(c.body), 0o755)
		if err := hookRun(context.Background(), dir, c.timeout, []string{script}, io.Discard); err != nil {
			t.Fatal(err)
		}
		if err := r.record(context.Background()); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v", c.body, err)
		}
	}
	if err := hookRun(context.Background(), dir, time.Minute, []string{"no-such-hook-program"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := r.record(context.Background()); err == nil || !strings.Contains(err.Error(), "the hook did not finish: no-such-hook-program:") {
		t.Errorf("not found: %v", err)
	}
}

// A safe default the hook's env lists is given as its value (the release's Secret holds none); a key
// of the chart's ConfigMap is reserved: dropped from the outputs, and refused to the record step.
func TestK8sHookSafeDefaultAndConfigMapKeys(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release.Spec.Hooks.Provision.Config["env"] = []any{"ADMIN_EMAIL", "SECURE_COOKIES"}
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org"}, ours)
	e.f.configMap = `{"data": {"API_URL": "http://notes-api:8000"}}`
	e.f.outputs = outputsJSON(map[string]string{"API_URL": "http://stale", "TOKEN": "t"})
	e.hookRuns("Succeeded", "", nil)
	if err := e.installer(nil).K8sProvision(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	env := envOf(e.container("provision"))
	if env["SECURE_COOKIES"] != "true" || env["API_URL"] != "" || env["TOKEN"] != "notes-stowage-provision/TOKEN" {
		t.Errorf("the hook's environment: %v", env)
	}
	if len(e.f.patches) != 1 || e.f.patches[0] != `{"data":{"API_URL":null}}` || !strings.Contains(e.log.String(), "dropping API_URL, now a value of the release (its Secret, a safe default) or of the ConfigMap") {
		t.Errorf("patches %q\n%s", e.f.patches, e.log)
	}
	if rec := strings.Join(toStrings(e.container("record")["command"]), " "); !strings.Contains(rec, "--reserved ADMIN_EMAIL,API_URL") {
		t.Errorf("record %s", rec)
	}
}

// An account that may not run the hook's Job is refused before helm runs, naming --no-provision.
func TestK8sHookGrantsAreCheckedFirst(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.f.denied = []string{"create roles.rbac.authorization.k8s.io"}
	err := e.installer(func(o *Options) { o.NoProvision = false }).K8sInstall(context.Background())
	if err == nil || !strings.Contains(err.Error(), "this account may not, in notes: create roles.rbac.authorization.k8s.io (`--no-provision` deploys without it)") || len(e.f.ran("helm upgrade")) != 0 {
		t.Fatalf("%v %q", err, e.f.ran("helm upgrade"))
	}
}

// A read of the Job that fails a moment is tried again: the Job, whose hook may still run, is not
// deleted for it.
func TestK8sHookPollErrorsAreRetried(t *testing.T) {
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org"}, ours)
	e.hookRuns("Succeeded", "", nil)
	e.f.podsErrs = 2
	poll := k8sHookPoll
	k8sHookPoll = time.Millisecond
	t.Cleanup(func() { k8sHookPoll = poll })
	if err := e.installer(nil).K8sProvision(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
}

// `record` follows the hook's stdout while it runs: a pod stopped before the hook ended keeps
// every full line read so far, and fails.
func TestHookRecordKeepsWhatAStoppedPodPrinted(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, hookOutFile), []byte("ADMIN_PASSWORD=minted\nPART"), 0o644)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := map[string]string{} // every value patched, across the patches
	early := false             // patched while the hook ran, before the pod was stopped
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]map[string]string
		_ = json.Unmarshal(raw, &body)
		maps.Copy(got, body["data"])
		if ctx.Err() == nil {
			early = true
			// Printed just before the pod is stopped: the last read must still find it.
			f, _ := os.OpenFile(filepath.Join(dir, hookOutFile), os.O_APPEND|os.O_WRONLY, 0)
			_, _ = f.WriteString("IAL=done\nLAST=late\n")
			_ = f.Close()
		}
		cancel() // the pod is stopped as the first value is recorded
	}))
	t.Cleanup(srv.Close)
	tok := t.TempDir()
	_ = os.WriteFile(filepath.Join(tok, "token"), []byte("tok"), 0o600)
	follow := hookFollow
	hookFollow = time.Millisecond
	t.Cleanup(func() { hookFollow = follow })
	r := &hookRecorder{dir: dir, token: tok, namespace: "notes", secret: "s", log: io.Discard, client: srv.Client(), api: srv.URL}
	err := r.record(ctx)
	if err == nil || !strings.Contains(err.Error(), "the pod was stopped") {
		t.Fatalf("%v", err)
	}
	if b := got; !early || b["ADMIN_PASSWORD"] == "" || b["PARTIAL"] != base64.StdEncoding.EncodeToString([]byte("done")) || b["LAST"] != base64.StdEncoding.EncodeToString([]byte("late")) {
		t.Errorf("patched %v (while the hook ran: %v)", got, early)
	}
}

// A safe default the chart's ConfigMap names is the chart's for the cluster: no literal shadows it.
// A safe default the hook returned is dropped, as Docker refuses it.
func TestK8sHookSafeDefaultTheChartSets(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release.Spec.Hooks.Provision.Config["env"] = []any{"ADMIN_EMAIL", "SECURE_COOKIES"}
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org"}, ours)
	e.f.configMap = `{"data": {"SECURE_COOKIES": "false"}}`
	e.f.outputs = outputsJSON(map[string]string{"SECURE_COOKIES": "x"})
	e.hookRuns("Succeeded", "", nil)
	if err := e.installer(nil).K8sProvision(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if env := envOf(e.container("provision")); env["SECURE_COOKIES"] != "" {
		t.Errorf("the hook's environment: %v", env)
	}
	if len(e.f.patches) != 1 || e.f.patches[0] != `{"data":{"SECURE_COOKIES":null}}` {
		t.Errorf("patches %q", e.f.patches)
	}
}

// A can-i that cannot ask (its stderr says why) is an error, never read as a refused grant.
func TestK8sHookGrantCheckThatCannotAsk(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.f.canIErr = "Unable to connect to the server"
	err := e.installer(func(o *Options) { o.NoProvision = false }).K8sInstall(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Unable to connect to the server") || strings.Contains(err.Error(), "may not") {
		t.Fatalf("%v", err)
	}
}

// A pod that cannot be scheduled, or whose hook step ended without its record, is given up.
func TestK8sHookPodGivenUp(t *testing.T) {
	poll, stuck, unsched := k8sHookPoll, k8sHookStuck, k8sHookUnschedulable
	k8sHookPoll, k8sHookStuck, k8sHookUnschedulable = time.Millisecond, 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { k8sHookPoll, k8sHookStuck, k8sHookUnschedulable = poll, stuck, unsched })
	for pod, want := range map[string]string{
		`{"phase": "Pending", "conditions": [{"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": "0/1 nodes are available"}]}`:                                                                 "its pod cannot be scheduled: 0/1 nodes are available",
		`{"phase": "Running", "containerStatuses": [{"name": "provision", "state": {"terminated": {"exitCode": 137, "reason": "OOMKilled"}}}, {"name": "record"}]}`:                                                          "its hook step ended without recording how the hook ended: OOMKilled (exit 137)",
		`{"phase": "Running", "containerStatuses": [{"name": "provision", "state": {"running": {}}}, {"name": "record", "state": {"terminated": {"exitCode": 1, "reason": "Error", "message": "the hook's status: bad"}}}]}`: "its record step ended while the hook ran: Error (exit 1) the hook's status: bad",
	} {
		e := newKubeEnv(t, spec.Provider{Name: "none"})
		e.runs("1.0.0", 1)
		e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org"}, ours)
		e.f.runJob = func(map[string]any) (string, string, map[string]string) {
			return `{"items": [{"metadata": {"name": "p"}, "status": ` + pod + `}]}`, "", nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := e.installer(nil).K8sProvision(ctx)
		cancel()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
		if strings.Contains(want, " step ") && len(e.f.ran("kubectl logs")) == 0 {
			t.Errorf("%s: the steps' logs were not copied before the Job went", want)
		}
	}
}

// A line past the line bound stops the hook, as on Docker: nothing it prints after is recorded.
func TestHookRunStopsALongLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	script := filepath.Join(t.TempDir(), "hook")
	_ = os.WriteFile(script, []byte(fmt.Sprintf("#!/bin/sh\nhead -c %d /dev/zero | tr '\\0' A\nexec sleep 30\n", maxProvisionLine+10)), 0o755)
	if err := hookRun(context.Background(), dir, 5*time.Second, []string{script}, io.Discard); err != nil {
		t.Fatal(err)
	}
	st, err := readStatus(dir)
	if err != nil || st == nil || st.Stopped != fmt.Sprintf("it wrote a stdout line longer than %d bytes", maxProvisionLine) {
		t.Fatalf("%+v %v", st, err)
	}
}

// A check step that cannot reach the outputs Secret says why, from its termination message; the
// steps that never ran are not asked for logs. The grants asked include the log subresource.
func TestK8sHookCheckStepFailureSaysWhy(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org"}, ours)
	e.f.runJob = func(map[string]any) (string, string, map[string]string) {
		return `{"items": [{"metadata": {"name": "p"}, "status": {"phase": "Failed",
			"initContainerStatuses": [{"name": "copy", "state": {"terminated": {"exitCode": 0}}}, {"name": "check", "state": {"terminated": {"exitCode": 1, "message": "the outputs Secret notes-stowage-provision cannot be written, so the hook does not run: 403"}}}],
			"containerStatuses": [{"name": "provision", "state": {"waiting": {"reason": "PodInitializing"}}}, {"name": "record", "state": {"waiting": {"reason": "PodInitializing"}}}]}}]}`, "", map[string]string{"check": "check log\n"}
	}
	err := e.installer(nil).K8sProvision(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot be written, so the hook does not run: 403") {
		t.Fatalf("%v", err)
	}
	if logs := e.f.ran("kubectl logs"); len(logs) != 1 || !strings.Contains(e.log.String(), "check log") {
		t.Errorf("logs %q\n%s", logs, e.log)
	}
	if !slices.Contains(e.f.canI, "get pods --subresource=log --namespace notes") {
		t.Errorf("asked %q", e.f.canI)
	}
}

// The hook's last values are recorded even when the API server refuses them a moment.
func TestHookRecordRetriesTheLastValues(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, hookOutFile), []byte("ADMIN_PASSWORD=minted\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, hookStatusFile), []byte(`{"exit": 0}`), 0o644)
	calls, refuse := 0, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch {
		case refuse:
			w.WriteHeader(http.StatusForbidden)
		case calls < 5: // more than one patch's own tries
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)
	tok := t.TempDir()
	_ = os.WriteFile(filepath.Join(tok, "token"), []byte("tok"), 0o600)
	follow, backoff := hookFollow, patchBackoff
	hookFollow, patchBackoff = time.Millisecond, time.Millisecond
	t.Cleanup(func() { hookFollow, patchBackoff = follow, backoff })
	r := &hookRecorder{dir: dir, token: tok, namespace: "notes", secret: "s", log: io.Discard, client: srv.Client(), api: srv.URL}
	if err := r.record(context.Background()); err != nil || calls != 5 {
		t.Fatalf("%v after %d patches", err, calls)
	}
	// A refusal is final: not tried again.
	calls, refuse = 0, true
	if err := r.record(context.Background()); err == nil || !strings.Contains(err.Error(), "403") || calls != 1 {
		t.Fatalf("%v after %d patches", err, calls)
	}
}

// A CRLF line at the line bound is not stopped: the parse trims its '\r'.
func TestCapWriterTakesACRLFLineAtTheBound(t *testing.T) {
	t.Parallel()
	stopped := false
	c := &capWriter{w: io.Discard, left: 1 << 30, over: func() { stopped = true }}
	_, _ = c.Write([]byte(strings.Repeat("A", maxProvisionLine) + "\r\n"))
	if stopped {
		t.Fatal("a CRLF line at the bound stopped the hook")
	}
	_, _ = c.Write([]byte(strings.Repeat("A", maxProvisionLine+1) + "\n")) // past it by one, with no '\r
	if !stopped {
		t.Fatal("a line past the bound did not stop the hook")
	}
}

// A pod stopped after the hook ended still gets one last try at its last values, in its grace.
func TestHookRecordLastTryWhenStoppedAfterTheHook(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, hookOutFile), []byte("ADMIN_PASSWORD=minted\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, hookStatusFile), []byte(`{"exit": 0}`), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	after := 0 // patches answered once the pod was stopped
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if ctx.Err() == nil {
			cancel() // the pod is stopped while the API server is away
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		after++
	}))
	t.Cleanup(srv.Close)
	tok := t.TempDir()
	_ = os.WriteFile(filepath.Join(tok, "token"), []byte("tok"), 0o600)
	follow, backoff := hookFollow, patchBackoff
	hookFollow, patchBackoff = time.Millisecond, time.Millisecond
	t.Cleanup(func() { hookFollow, patchBackoff = follow, backoff })
	r := &hookRecorder{dir: dir, token: tok, namespace: "notes", secret: "s", log: io.Discard, client: srv.Client(), api: srv.URL}
	if err := r.record(ctx); err != nil || after != 1 {
		t.Fatalf("%v, %d patches after the stop", err, after)
	}
}
