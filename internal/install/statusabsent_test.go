package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/spec"
)

// oneStatusRefused decodes the one StatusRefused out holds, strictly.
func oneStatusRefused(t *testing.T, out string) StatusRefused {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	var r StatusRefused
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("%v: %q", err, out)
	}
	if dec.More() || r.Format != MachineFormat {
		t.Fatalf("not one object of format %d: %q", MachineFormat, out)
	}
	return r
}

// status --json on a refusal prints it, absent telling "no install here" from another refusal; the
// verbs' Result carries absent the same way. Both sides, on Docker and on Kubernetes.
func TestStatusJSONTellsAbsentFromAnother(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	tools := K8sOptions{Helm: kubeTool{e.f, "helm"}, Kubectl: kubeTool{e.f, "kubectl"}}
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		cmd := newCommand(e.release, "notes-stow", &out, io.Discard, tools, func(bool) Prompter { return nil })
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(context.Background())
		return out.String(), err
	}
	// Docker: an empty directory is absent.
	empty := t.TempDir()
	out, err := run("status", "--json", "--dir", empty)
	if !isRefused(err) {
		t.Fatalf("%v", err)
	}
	if r := oneStatusRefused(t, out); !r.Refused.Absent || !strings.HasSuffix(r.Refused.Reason, "holds no install") || r.Refused.Fix == "" {
		t.Errorf("%+v", r)
	}
	// The verbs: absent on the empty directory, not on another project's install.
	for _, args := range [][]string{{"uninstall", "--json", "--yes"}, {"upgrade", "--json", "--no-input"}, {"rollback", "--json"}} {
		out, err = run(append(args, "--dir", empty)...)
		if r := oneResult(t, out); !isRefused(err) || r.Refused == nil || !r.Refused.Absent {
			t.Errorf("%v: %v %+v", args, err, r.Refused)
		}
	}
	other := t.TempDir()
	_ = os.WriteFile(filepath.Join(other, envName), []byte("STOWAGE_NAME='atlas'\nSTOWAGE_PROJECT_VERSION='3.0.0'\nSTOWAGE_REVISION='2'\n"), 0o600)
	out, err = run("uninstall", "--json", "--dir", other)
	if r := oneResult(t, out); !isRefused(err) || r.Refused == nil || r.Refused.Absent || strings.Contains(out, `"absent"`) {
		t.Errorf("%v %s", err, out)
	}
	// Kubernetes: no release is absent; a release not installed from Stowage's chart is another
	// refusal, printed too.
	out, err = run("k8s", "status", "--json")
	if r := oneStatusRefused(t, out); !isRefused(err) || !r.Refused.Absent || r.Refused.Reason != "no release notes in notes: `notes-stow k8s install --namespace notes --release notes` installs 1.0.0" {
		t.Errorf("%v %+v", err, r)
	}
	// Every k8s verb finding no release says absent, as Docker's verbs on an empty directory.
	for _, args := range [][]string{{"k8s", "upgrade", "--json", "--no-input"}, {"k8s", "rollback", "--json"}} {
		out, err := run(args...)
		if r := oneResult(t, out); !isRefused(err) || r.Refused == nil || !r.Refused.Absent || !strings.Contains(r.Refused.Reason, "no release notes in notes") {
			t.Errorf("%v: %v %s", args, err, out)
		}
	}
	out, err = run("k8s", "upgrade", "--version", "2.0.0", "--json", "--no-input")
	if r := oneResult(t, out); !isRefused(err) || r.Refused == nil || !r.Refused.Absent || !strings.Contains(r.Refused.Reason, "no release notes in notes") {
		t.Errorf("k8s upgrade --version: %v %s", err, out)
	}
	e.runs("1.0.0", 1)
	e.f.values = map[string]any{"image": "x"}
	out, err = run("k8s", "status", "--json")
	if r := oneStatusRefused(t, out); !isRefused(err) || r.Refused.Absent || !strings.Contains(r.Refused.Reason, "was not installed from a chart Stowage published") {
		t.Errorf("%v %+v", err, r)
	}
}

// A failure is no refusal: status --json prints nothing for it.
func TestStatusRefusedPrintsNothingOnAFailure(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	fail := errors.New("helm: exit status 1")
	if err := printStatusRefused(&out, fail); err != fail || out.Len() != 0 {
		t.Errorf("%v %q", err, out.String())
	}
	if err := printStatusRefused(&out, changed(refuse("x", "after the change"))); err == nil || out.Len() != 0 {
		t.Errorf("a refusal after a change printed %q", out.String())
	}
	// A standard output that cannot be written leaves the refusal one: exit 3.
	if err := printStatusRefused(failingWriter{}, refuse("x", "no install")); !isRefused(err) {
		t.Errorf("the print's failure replaced the refusal: %v", err)
	}
}

// What a Docker install keeps from its first run: the installer's short name, the loopback
// address, the registry prefix, and the digest of the registry CA, only when it keeps one.
func TestDockerStatusJSONFirstInstallSettings(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(func(o *Options) { o.Address = "127.0.0.9" }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	st, out := statusOf(t, e, nil)
	if st.ShortName != "notes" || st.Address != "127.0.0.9" || st.Registry != "ghcr.io/acme/notes" || st.RegistryCA != "" || strings.Contains(out, `"registry_ca"`) {
		t.Errorf("%+v\n%s", st, out)
	}
	ca := []byte("-----BEGIN CERTIFICATE-----\nthe install's copy\n-----END CERTIFICATE-----\n")
	if err := os.WriteFile(filepath.Join(e.dir, RegistryCAFile), ca, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(ca)
	if st, _ = statusOf(t, e, nil); st.RegistryCA != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Errorf("registry_ca %q", st.RegistryCA)
	}
}

// Kubernetes reports the registry its release's values record.
func TestK8sStatusJSONRegistry(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.runs("1.0.0", 1)
	e.f.history = []helmRevision{{Revision: 1, Status: "deployed", Chart: "notes-0.1.0"}}
	e.stored(map[string]string{"ADMIN_EMAIL": "admin@example.org"}, ours)
	if err := e.installer(nil).K8sStatusJSON(context.Background()); err != nil {
		t.Fatal(err)
	}
	var st K8sReleaseStatus
	if err := json.Unmarshal(e.out.Bytes(), &st); err != nil || st.Registry != e.prefix || e.prefix == "" {
		t.Errorf("%v %q\n%s", err, st.Registry, e.out)
	}
}

func refusedAbsent(err error) bool { r, _ := Refusal(err); return r.Absent }

// Every Docker refusal of a missing install names the directory as an absolute path, a relative
// --dir included.
func TestNoInstallNamesTheAbsoluteDir(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	in := e.installer(func(o *Options) { o.Dir = "no-such-install-here" })
	_, err := in.ExportSecrets([]string{"age1x"})
	if r, ok := Refusal(err); !ok || !strings.HasPrefix(r.Reason, "/") || !strings.HasSuffix(r.Reason, "/no-such-install-here holds no install") {
		t.Errorf("%v", err)
	}
}

// On the VM, whose host daemon answers its verbs, a directory with no install is an ordinary
// refusal: absent is Docker's and Kubernetes'.
func TestTheVMsNoInstallIsNotAbsent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, preloaded := range []bool{false, true} {
		in := e.installer(func(o *Options) { o.Preloaded = preloaded })
		_, exportErr := in.ExportSecrets([]string{"age1x"})
		_, openErr := in.open()
		// Every verb, the text status and the secrets export refuse it one way.
		for _, err := range []error{openErr, in.Status(context.Background()), exportErr} {
			want := "install first, or give the --dir of the install"
			if preloaded {
				want = "sudo notes-stow install"
			}
			if r, ok := Refusal(err); !ok || r.Absent == preloaded || r.Fix != want {
				t.Errorf("preloaded %v: %v %+v", preloaded, err, r)
			}
		}
	}
}

// A registry CA the install keeps but that cannot be read fails status --json (exit 1, nothing
// printed): it is never reported absent.
func TestDockerStatusJSONFailsOnAnUnreadableRegistryCA(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if err := os.Mkdir(filepath.Join(e.dir, RegistryCAFile), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := e.installer(func(o *Options) { o.Out = &out }).StatusJSON(context.Background())
	if err == nil || isRefused(err) || out.Len() != 0 {
		t.Errorf("%v %q", err, out.String())
	}
}

// describe's text names each route, its loopback port when the spec gives one, each L4 route and
// the registry.
func TestDescribeTextRoutes(t *testing.T) {
	t.Parallel()
	d := Description{Name: "notes", Registry: "ghcr.io/acme/notes",
		Routes:   []DescribedRoute{{Name: "app", LoopbackPort: 8080}, {Name: "bare"}},
		L4Routes: []DescribedL4Route{{Name: "db", Protocol: "tcp", Port: 5432}}}
	var b bytes.Buffer
	if err := d.print(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\nroute     app (loopback port 8080)\n", "\nroute     bare\n", "\nl4 route  db (tcp/5432)\n", "\nregistry  ghcr.io/acme/notes\nfirst install only: "} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %q in:\n%s", want, b.String())
		}
	}
}
