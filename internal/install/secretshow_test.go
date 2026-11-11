package install

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/spec"
)

// onTerminal makes `secrets show` see a terminal (on) or none, for the test's length.
func onTerminal(t *testing.T, on bool) {
	t.Helper()
	was := isTerminal
	isTerminal = func(io.Writer) bool { return on }
	t.Cleanup(func() { isTerminal = was })
}

func TestSecretsShowOnTheDockerCLI(t *testing.T) {
	onTerminal(t, true)
	e := newEnv(t)
	settableSession(e.r.Spec)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	e.out.Reset()
	e.installer(nil).Finished(context.Background(), onDocker)
	if !strings.Contains(e.out.String(), "logins    SESSION_SECRET: `") || !strings.Contains(e.out.String(), " secrets show KEY --dir "+e.dir+"` shows one, on a terminal") {
		t.Errorf("the install's report names no login:\n%s", e.out)
	}
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		cmd := newCommand(e.r, e.self, &out, io.Discard, K8sOptions{}, func(bool) Prompter { return nil })
		cmd.SetArgs(append(args, "--dir", e.dir))
		err := cmd.ExecuteContext(context.Background())
		return out.String(), err
	}
	want := get(e.readEnv(), "SESSION_SECRET")
	if out, err := run("secrets", "show", "SESSION_SECRET"); err != nil || out != want+"\n" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := run("secrets", "show", "POSTGRES_PASSWORD"); err == nil || !strings.Contains(err.Error(), "POSTGRES_PASSWORD is no secret with settable: true (settable: SESSION_SECRET)") {
		t.Errorf("not settable: %v", err)
	}
	// Another release's installer: its spec does not say what the install's makes settable.
	other := e.installer(nil)
	other.R, _ = testInstallerOf(t, "2.0.0", 1, settableSession, nil)
	if _, err := other.ShowSecret(context.Background(), "SESSION_SECRET"); err == nil || !isRefused(err) {
		t.Errorf("another release: %v", err)
	}
	onTerminal(t, false)
	if out, err := run("secrets", "show", "SESSION_SECRET"); err == nil || !strings.Contains(err.Error(), "standard output is no terminal") || strings.Contains(out, want) {
		t.Errorf("off a terminal: %q %v", out, err)
	}
}

func TestNoSecretsShowWithoutASettableSecret(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	e.installer(nil).Finished(context.Background(), onDocker)
	if strings.Contains(e.out.String(), "logins") {
		t.Errorf("a report names logins with none settable:\n%s", e.out)
	}
	cmd := newCommand(e.r, e.self, io.Discard, io.Discard, K8sOptions{}, func(bool) Prompter { return nil })
	if c, _, err := cmd.Find([]string{"secrets", "show"}); err == nil && c.Name() == "show" {
		t.Error("a release without a settable secret has secrets show")
	}
}

func TestSecretsShowOverTheSocket(t *testing.T) {
	onTerminal(t, true)
	e := vmEnv(t, "1.0.0")
	e.r, e.self = bundledInstaller(t, "1.0.0", 1, settableSession)
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	want := get(e.readEnv(), "SESSION_SECRET")
	var report bytes.Buffer
	in := e.installer(nil)
	in.O.Out = &report
	in.Finished(context.Background(), onVM)
	if !strings.Contains(report.String(), "logins    SESSION_SECRET: `sudo "+e.r.Ctl()+" secrets show KEY` shows one") {
		t.Errorf("the report names no login:\n%s", report.String())
	}
	d := daemon(e, &fakeHost{})
	var got SecretShown
	if code := call(t, d.Handler(), "POST", "/v1/secrets/show", SecretShowRequest{Key: "SESSION_SECRET"}, &got); code != 200 || got.Value != want {
		t.Fatalf("%d %q", code, got.Value)
	}
	var e400, e409 errOf
	if code := call(t, d.Handler(), "POST", "/v1/secrets/show", SecretShowRequest{Key: "POSTGRES_PASSWORD"}, &e400); code != 400 || !strings.Contains(e400.Error, "settable: SESSION_SECRET") {
		t.Errorf("not settable: %d %s", code, e400.Error)
	}
	other := daemon(e, &fakeHost{})
	other.R, _ = bundledInstaller(t, "2.0.0", 1, settableSession)
	if code := call(t, other.Handler(), "POST", "/v1/secrets/show", SecretShowRequest{Key: "SESSION_SECRET"}, &e409); code != 409 ||
		!strings.Contains(e409.Error, "secrets show runs on the release the OS carries") {
		t.Errorf("another release: %d %s", code, e409.Error)
	}
	sock := served(t, daemon(e, &fakeHost{}))
	if out, err := ctlIn(e, sock, "", "secrets", "show", "SESSION_SECRET"); err != nil || out != want+"\n" {
		t.Fatalf("%q %v", out, err)
	}
	onTerminal(t, false)
	if out, err := ctlIn(e, sock, "", "secrets", "show", "SESSION_SECRET"); err == nil || strings.Contains(out, want) {
		t.Errorf("off a terminal: %q %v", out, err)
	}
}

func TestSecretsShowOnKubernetes(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, settableSession)
	in := e.installer(nil)
	if _, err := in.K8sShowSecret(context.Background(), "SESSION_SECRET"); err == nil || !strings.Contains(err.Error(), "no release notes in notes") {
		t.Errorf("no release: %v", err)
	}
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s3", "POSTGRES_PASSWORD": "p"}, ours)
	if v, err := in.K8sShowSecret(context.Background(), "SESSION_SECRET"); err != nil || v != "s3" {
		t.Fatalf("%q %v", v, err)
	}
	if _, err := in.K8sShowSecret(context.Background(), "POSTGRES_PASSWORD"); err == nil || !strings.Contains(err.Error(), "settable: SESSION_SECRET") {
		t.Errorf("not settable: %v", err)
	}
	in.k8sFinished(context.Background(), nil, "")
	if !strings.Contains(e.out.String(), "logins    SESSION_SECRET: `"+in.k8sCtl("secrets show KEY")+"` shows one") {
		t.Errorf("the report names no login:\n%s", e.out)
	}
	e.runs("0.9.0", 1)
	if _, err := in.K8sShowSecret(context.Background(), "SESSION_SECRET"); !isRefused(err) || !strings.Contains(err.Error(), "show with the installer of 0.9.0") {
		t.Errorf("another release: %v", err)
	}
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org"}, ours)
	if _, err := in.K8sShowSecret(context.Background(), "SESSION_SECRET"); err == nil || !strings.Contains(err.Error(), "the Secret notes-stowage in notes holds no SESSION_SECRET") {
		t.Errorf("none recorded: %v", err)
	}
}
