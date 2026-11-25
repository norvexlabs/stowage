package install

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"filippo.io/age/armor"
	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/spec"
)

// bothSecretInputs adds a secret input on every shape and one on Docker alone.
func bothSecretInputs(s *spec.Spec) {
	withSecretInput(s)
	s.Inputs = append(s.Inputs, spec.Input{Key: "RELAY_TOKEN", Prompt: "Relay token", Secret: true, Shapes: []string{"docker"}})
}

// noValueIn fails when what holds one of values in the clear.
func noValueIn(t *testing.T, what string, b []byte, values ...string) {
	t.Helper()
	for _, v := range values {
		if bytes.Contains(b, []byte(v)) {
			t.Errorf("%s holds %q in the clear:\n%s", what, v, b)
		}
	}
}

// changedCluster is the calls of the fake cluster that write something.
func changedCluster(f *fakeKube) [][]string {
	var out [][]string
	for _, p := range []string{"kubectl apply", "kubectl patch", "kubectl create", "kubectl delete", "helm upgrade"} {
		out = append(out, f.ran(p)...)
	}
	return out
}

func TestK8sSecretsExportComesBackOnDocker(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, bothSecretInputs)
	e.runs("1.0.0", 1)
	// RELAY_TOKEN is an input on Docker alone, the Cloudflare token the Docker front's: neither is a
	// secret of the release on Kubernetes.
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "k8s-session", "POSTGRES_PASSWORD": "k8s-pg", "SMTP_PASSWORD": "k8s-smtp", "RELAY_TOKEN": "stray", CloudflareTokenVar: "stray"}, ours)
	e.f.outputs = outputsJSON(map[string]string{"ADMIN_PASSWORD": "k8s-hook"})
	key, _ := age.GenerateX25519Identity()
	raw, err := e.installer(nil).K8sExportSecrets(context.Background(), []string{key.Recipient().String()})
	if err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	values := []string{"k8s-session", "k8s-pg", "k8s-smtp", "k8s-hook", base64.StdEncoding.EncodeToString([]byte("k8s-session"))}
	if !bytes.HasPrefix(raw, []byte(armor.Header)) {
		t.Fatalf("not an armored age file:\n%s", raw)
	}
	noValueIn(t, "the file", raw, values...)
	noValueIn(t, "the log", e.log.Bytes(), values...)
	noValueIn(t, "the output", e.out.Bytes(), values...)
	if len(changedCluster(e.f)) > 0 {
		t.Errorf("the export changed the cluster: %q", changedCluster(e.f))
	}
	plain := decrypt(t, raw, key)
	if !bytes.Contains(plain, []byte("written by `notes-stow k8s secrets export`")) {
		t.Errorf("header:\n%s", plain)
	}
	var f SecretsFile
	if err := yaml.Unmarshal(plain, &f); err != nil {
		t.Fatal(err)
	}
	if f.Project != "notes" || len(f.Secrets) != 3 || f.Secrets["SESSION_SECRET"] != "k8s-session" || f.Secrets["POSTGRES_PASSWORD"] != "k8s-pg" ||
		f.Secrets["SMTP_PASSWORD"] != "k8s-smtp" || len(f.Provision) != 1 || f.Provision["ADMIN_PASSWORD"] != "k8s-hook" || f.FrontCA != nil {
		t.Fatalf("exported %+v", f)
	}
	// Docker's install takes the file back.
	d := newEnv(t)
	d.r, d.self = testInstallerWith(t, "1.0.0", bothSecretInputs)
	if err := d.install(func(o *Options) { o.Secrets = plain }); err != nil {
		t.Fatalf("%v\n%s", err, d.log)
	}
	now := d.readEnv()
	for k, want := range map[string]string{"SESSION_SECRET": "k8s-session", "POSTGRES_PASSWORD": "k8s-pg", "SMTP_PASSWORD": "k8s-smtp"} {
		if get(now, k) != want {
			t.Errorf("%s = %q, want %q", k, get(now, k), want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(d.dir, ProvisionEnv)); string(b) != "ADMIN_PASSWORD='k8s-hook'\n" {
		t.Errorf("provision.env %q", b)
	}
}

func TestK8sSecretsExportRefusals(t *testing.T) {
	t.Parallel()
	key, _ := age.GenerateX25519Identity()
	rcpt := []string{key.Recipient().String()}
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	// A key refused before the cluster is read.
	if _, err := e.installer(nil).K8sExportSecrets(context.Background(), []string{"age1nope"}); err == nil || !strings.Contains(err.Error(), `recipient "age1nope" is not an age public key`) || !isRefused(err) || len(e.f.calls) > 0 {
		t.Errorf("a bad key: %v %q", err, e.f.calls)
	}
	if _, err := e.installer(nil).K8sExportSecrets(context.Background(), rcpt); err == nil || !strings.Contains(err.Error(), "no release notes in notes") {
		t.Errorf("no release: %v", err)
	}
	e.runs("0.9.0", 1)
	e.stored(map[string]string{"SESSION_SECRET": "s"}, ours)
	if _, err := e.installer(nil).K8sExportSecrets(context.Background(), rcpt); !isRefused(err) || !strings.Contains(err.Error(), "export with the installer of 0.9.0") {
		t.Errorf("another release: %v", err)
	}
	e.runs("1.0.0", 1)
	e.f.outputs = `{"metadata":{"labels":{"app.kubernetes.io/managed-by":"other"}},"data":{}}`
	if _, err := e.installer(nil).K8sExportSecrets(context.Background(), rcpt); err == nil || !strings.Contains(err.Error(), "secret notes-stowage-provision in notes was not written by notes-stow") {
		t.Errorf("another tool's outputs Secret: %v", err)
	}
	if len(changedCluster(e.f)) > 0 {
		t.Errorf("a refused export changed the cluster: %q", changedCluster(e.f))
	}
}

// dockerExport is a Docker install's decrypted `secrets export`, its hook having returned hook,
// and the values the install recorded.
func dockerExport(t *testing.T, hook string) ([]byte, *EnvFile) {
	t.Helper()
	d := newEnv(t)
	d.r, d.self = testInstallerWith(t, "1.0.0", bothSecretInputs)
	d.vars["SMTP_PASSWORD"], d.vars["RELAY_TOKEN"] = "smtp-pw", "relay-pw"
	if err := d.install(nil); err != nil {
		t.Fatalf("%v\n%s", err, d.log)
	}
	if hook != "" {
		if err := os.WriteFile(filepath.Join(d.dir, ProvisionEnv), []byte("ADMIN_PASSWORD='"+hook+"'\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	key, _ := age.GenerateX25519Identity()
	raw, err := d.installer(nil).ExportSecrets([]string{key.Recipient().String()})
	if err != nil {
		t.Fatal(err)
	}
	return decrypt(t, raw, key), d.readEnv()
}

func TestK8sInstallTakesADockerExportAndWaitsForItsData(t *testing.T) {
	t.Parallel()
	plain, was := dockerExport(t, "hook-pw")
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, bothSecretInputs)
	e.hookRuns("Succeeded", "", map[string]string{"ADMIN_PASSWORD": "minted"})
	if err := e.installer(func(o *Options) { o.Secrets, o.NoProvision = plain, false }).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	data := e.secretData("notes-stowage")
	for _, k := range []string{"SESSION_SECRET", "POSTGRES_PASSWORD", "SMTP_PASSWORD"} {
		if data[k] == "" || data[k] != get(was, k) {
			t.Errorf("%s = %q, want %q", k, data[k], get(was, k))
		}
	}
	if _, ok := data["RELAY_TOKEN"]; ok || !strings.Contains(e.log.String(), "--secrets: RELAY_TOKEN is no secret of notes 1.0.0: left out") {
		t.Errorf("a Docker input reached the release: %v\n%s", sortedKeys(data), e.log)
	}
	// The hook's values are recorded, marked, before helm; the hook waits for the backup.
	patches := e.f.ran("kubectl patch")
	if len(patches) != 1 || patches[0][3] != "notes-stowage-provision" {
		t.Fatalf("patches %q", patches)
	}
	if !strings.Contains(e.f.patches[0], `"`+restoreAwaitedLabel+`":"true"`) || !strings.Contains(e.f.patches[0], base64.StdEncoding.EncodeToString([]byte("hook-pw"))) {
		t.Errorf("patch %s", e.f.patches[0])
	}
	var order []string
	for _, c := range e.f.calls {
		if s := strings.Join(c[:2], " "); s == "kubectl patch" || s == "helm upgrade" {
			order = append(order, s)
		}
	}
	if strings.Join(order, ", ") != "kubectl patch, helm upgrade" {
		t.Errorf("order %q", order)
	}
	if e.f.created != nil || !strings.Contains(e.log.String(), "not provisioned: the provision hook's values in the Secret notes-stowage-provision came with --secrets") {
		t.Errorf("the hook ran on the empty database: %v\n%s", e.f.created != nil, e.log)
	}
	noValueIn(t, "the log", e.log.Bytes(), get(was, "SESSION_SECRET"), get(was, "POSTGRES_PASSWORD"), "smtp-pw", "hook-pw")
	noValueIn(t, "the output", e.out.Bytes(), get(was, "SESSION_SECRET"), get(was, "POSTGRES_PASSWORD"), "smtp-pw", "hook-pw")
	// A later deploy still waits.
	e.runs("1.0.0", 1)
	if err := e.installer(func(o *Options) { o.NoProvision = false }).K8sUpgrade(context.Background()); err != nil || e.f.created != nil {
		t.Fatalf("the upgrade ran the hook: %v\n%s", err, e.log)
	}
	// `k8s provision` (the backup loaded) runs it, then ends the wait.
	if err := e.installer(nil).K8sProvision(context.Background()); err != nil || e.f.created == nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if last := e.f.patches[len(e.f.patches)-1]; last != `{"metadata":{"labels":{"`+restoreAwaitedLabel+`":null}}}` {
		t.Errorf("last patch %s", last)
	}
	if awaited, err := e.installer(nil).k8sRestoreAwaited(context.Background()); err != nil || awaited {
		t.Errorf("still awaited: %v %v", awaited, err)
	}
}

func TestK8sInstallWithSecretsWithoutHookValuesRunsTheHook(t *testing.T) {
	t.Parallel()
	plain, was := dockerExport(t, "")
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, bothSecretInputs)
	e.hookRuns("Succeeded", "", map[string]string{"ADMIN_PASSWORD": "minted"})
	if err := e.installer(func(o *Options) { o.Secrets, o.NoProvision = plain, false }).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if e.f.created == nil || len(e.f.ran("kubectl patch")) != 0 || e.secretData("notes-stowage")["SESSION_SECRET"] != get(was, "SESSION_SECRET") {
		t.Errorf("job %v, patches %q\n%s", e.f.created != nil, e.f.patches, e.log)
	}
}

func TestK8sInstallSecretsRefusals(t *testing.T) {
	t.Parallel()
	key, _ := age.GenerateX25519Identity()
	enc, err := encryptSecrets(&SecretsFile{Project: "notes", Secrets: map[string]string{"SESSION_SECRET": "x"}}, "notes-stow", []string{key.Recipient().String()})
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		file    string
		cluster func(e *kubeEnv)
		upgrade bool
		want    string
	}{
		"another project's": {file: "project: crm\nsecrets: {}\n", want: `the file holds "crm"'s secrets, not notes's`},
		"still encrypted":   {file: string(enc), want: "the file is still encrypted"},
		"an unknown field":  {file: "project: notes\nsecret: {}\n", want: "field secret not found"},
		"too big":           {file: strings.Repeat("#", maxSecretsFile+1), want: "more than"},
		"a quote":           {file: "project: notes\nsecrets: {}\nprovision:\n  ADMIN_PASSWORD: \"a'b\"\n", want: "--secrets: provision: ADMIN_PASSWORD: a value cannot hold a single quote"},
		"no variable name":  {file: "project: notes\nsecrets: {}\nprovision:\n  admin-password: x\n", want: "--secrets: provision: admin-password: not a variable name"},
		"a kept Secret":     {file: "project: notes\nsecrets: {}\n", cluster: func(e *kubeEnv) { e.stored(map[string]string{"SESSION_SECRET": "kept"}, ours) }, want: "the Secret notes-stowage in notes holds release notes's secrets"},
		"another's outputs": {file: "project: notes\nsecrets: {}\nprovision:\n  ADMIN_PASSWORD: x\n", cluster: func(e *kubeEnv) { e.f.outputs = `{"metadata":{"labels":{}},"data":{}}` }, want: "secret notes-stowage-provision in notes was not written by"},
		"an upgrade":        {file: "project: notes\nsecrets: {}\n", upgrade: true, cluster: func(e *kubeEnv) { e.runs("1.0.0", 1); e.stored(map[string]string{"SESSION_SECRET": "s"}, ours) }, want: "--secrets is for a first install"},
	} {
		e := newKubeEnv(t, spec.Provider{Name: "none"})
		if c.cluster != nil {
			c.cluster(e)
		}
		in := e.installer(func(o *Options) { o.Secrets = []byte(c.file) })
		run := in.K8sInstall
		if c.upgrade {
			run = in.K8sUpgrade
		}
		if err := run(context.Background()); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
		if len(changedCluster(e.f)) > 0 {
			t.Errorf("%s: the cluster changed: %q", name, changedCluster(e.f))
		}
	}
}

func TestK8sSecretsFlags(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	run := func(stdin string, args ...string) error {
		cmd := newCommand(e.release, "notes-stow", io.Discard, io.Discard, e.tools(), nil)
		cmd.SetArgs(args)
		cmd.SetIn(strings.NewReader(stdin))
		return cmd.Execute()
	}
	file := filepath.Join(t.TempDir(), "secrets.yaml")
	if err := os.WriteFile(file, []byte("project: notes\nsecrets: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run("", "k8s", "install", "--no-input", "--secrets", file, "--given", file); err == nil || !strings.Contains(err.Error(), "--secrets restores an install, --given gives one") {
		t.Errorf("with --given: %v", err)
	}
	if err := run("", "k8s", "install", "--no-input", "--secrets", "-", "--registry-login-file", "-"); err == nil || !strings.Contains(err.Error(), "both name standard input") {
		t.Errorf("two standard inputs: %v", err)
	}
	if len(e.f.calls) > 0 {
		t.Errorf("a refused flag reached the cluster: %q", e.f.calls)
	}
	root := newCommand(e.release, "notes-stow", io.Discard, io.Discard, K8sOptions{}, nil)
	if c, _, err := root.Find([]string{"k8s", "upgrade"}); err != nil || c.Flags().Lookup("secrets") != nil {
		t.Errorf("k8s upgrade takes --secrets: %v", err)
	}
	if c, _, err := root.Find([]string{"k8s", "secrets", "export"}); err != nil || c.Name() != "export" || c.Flags().Lookup("recipient") == nil {
		t.Errorf("no k8s secrets export: %v", err)
	}
}

// A rotation under way when the secrets were exported goes on from where it was.
func TestK8sInstallWithSecretsKeepsARotationUnderWay(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, func(s *spec.Spec) {
		for i := range s.Secrets {
			s.Secrets[i].Rotation = s.Secrets[i].Key == "SESSION_SECRET"
		}
	})
	plain := []byte("project: notes\nsecrets:\n  SESSION_SECRET: cur\n  SESSION_SECRET_NEXT: nxt\n  POSTGRES_PASSWORD: pg\n")
	if err := e.installer(func(o *Options) { o.Secrets = plain }).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	data := e.secretData("notes-stowage")
	if data["SESSION_SECRET"] != "cur" || data["SESSION_SECRET_NEXT"] != "nxt" || data["SESSION_SECRET_ACCEPTED"] != "nxt" || data["POSTGRES_PASSWORD"] != "pg" {
		t.Errorf("data %v", data)
	}
	if v, ok := data["SESSION_SECRET_PREVIOUS"]; !ok || v != "" {
		t.Errorf("SESSION_SECRET_PREVIOUS %q %v", v, ok)
	}
}

// The file's front CA is left out on Kubernetes, said; a key the outputs Secret held before (an
// install that stopped) is removed with the values the file gives.
func TestK8sInstallWithSecretsLeavesTheFrontCAAndReplacesTheOutputs(t *testing.T) {
	t.Parallel()
	cert, key := testRoot(t, true)
	plain, err := yaml.Marshal(SecretsFile{Project: "notes", Secrets: map[string]string{}, Provision: map[string]string{"ADMIN_PASSWORD": "hook-pw"}, FrontCA: &FrontCA{Cert: string(cert), Key: string(key)}})
	if err != nil {
		t.Fatal(err)
	}
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.f.outputs = outputsJSON(map[string]string{"STALE": "x"})
	if err := e.installer(func(o *Options) { o.Secrets = plain }).K8sInstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if !strings.Contains(e.log.String(), "--secrets: front_ca left out: on Kubernetes the chart's ingress owns the certificate") {
		t.Errorf("log:\n%s", e.log)
	}
	if len(e.f.patches) != 1 || !strings.Contains(e.f.patches[0], `"STALE":null`) || !strings.Contains(e.f.patches[0], `"ADMIN_PASSWORD":"`) {
		t.Errorf("patches %q", e.f.patches)
	}
}
