package install

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/norvexlabs/stowage/internal/spec"
)

// settableSession makes SESSION_SECRET a settable secret.
func settableSession(s *spec.Spec) {
	for i := range s.Secrets {
		s.Secrets[i].Settable = s.Secrets[i].Key == "SESSION_SECRET"
	}
}

func TestASettableSecretIsSetOnDocker(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	settableSession(e.r.Spec)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	ups := func() int {
		n := 0
		for _, c := range e.docker.calls {
			if slices.Contains(c.Args, "up") {
				n++
			}
		}
		return n
	}
	set := func(key, v string) error {
		e.docker.calls = nil
		return e.installer(nil).SetSecret(context.Background(), key, v)
	}
	before := e.file(".env")
	for _, c := range []struct{ key, v, want string }{
		{"POSTGRES_PASSWORD", "x", "POSTGRES_PASSWORD is no secret with settable: true (settable: SESSION_SECRET)"},
		{"SESSION_SECRET", "", "the value is empty"},
		{"SESSION_SECRET", strings.Repeat("a", maxSetSecret+1), "over 4096 bytes"},
		{"SESSION_SECRET", "it's", "single quote"},
	} {
		if err := set(c.key, c.v); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s=%.10q: %v", c.key, c.v, err)
		}
		if e.file(".env") != before || ups() != 0 {
			t.Fatalf("a refused set changed .env or started the stack")
		}
	}
	chosen := strings.Repeat("b", maxSetSecret)
	if err := set("SESSION_SECRET", chosen); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if get(e.readEnv(), "SESSION_SECRET") != chosen || ups() != 1 || !strings.Contains(e.log.String(), "recorded the new SESSION_SECRET") {
		t.Fatalf("after the set: %d ups\n%s", ups(), e.log)
	}
	// The same value again (the restart failed) brings the stack up, recording nothing.
	after := e.file(".env")
	if err := set("SESSION_SECRET", chosen); err != nil || e.file(".env") != after || ups() != 1 || !strings.Contains(e.log.String(), "holds that value already") {
		t.Fatalf("again: %v, %d ups", err, ups())
	}
	// The export carries it.
	f, err := e.installer(nil).exportSecrets(context.Background(), e.readEnv())
	if err != nil || f.Secrets["SESSION_SECRET"] != chosen {
		t.Fatalf("export: %v %.20q", err, f.Secrets["SESSION_SECRET"])
	}
}

func TestSecretsSetReadsStandardInput(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"v": "v", "v\n": "v", "v\r\n": "v", "a b": "a b"} {
		if got, err := readSetSecret(strings.NewReader(in)); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for in, want := range map[string]string{"": "empty", "\n": "empty", "v\r": "line break", "v\n\n": "line break", "v\nw": "line break", strings.Repeat("a", maxSetSecret+1): "over"} {
		if _, err := readSetSecret(strings.NewReader(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.10q: %v", in, err)
		}
	}
	if _, err := readSetSecret(strings.NewReader(strings.Repeat("a", maxSetSecret) + "\n")); err != nil {
		t.Errorf("at the bound: %v", err)
	}
}

// The CLI runs the host's own Docker: only what it refuses before is run here (SetSecret's run is
// TestASettableSecretIsSetOnDocker's).
func TestSecretsSetOnTheDockerCLI(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	command := func() *cobra.Command {
		return newCommand(e.r, e.self, io.Discard, io.Discard, K8sOptions{}, func(bool) Prompter { return nil })
	}
	run := func(stdin string, args ...string) error {
		cmd := command()
		cmd.SetIn(strings.NewReader(stdin))
		cmd.SetArgs(append(args, "--dir", e.dir))
		return cmd.ExecuteContext(context.Background())
	}
	if c, _, err := command().Find([]string{"secrets", "set"}); err == nil && c.Name() == "set" {
		t.Error("a release without a settable secret has secrets set")
	}
	settableSession(e.r.Spec)
	if c, _, err := command().Find([]string{"secrets", "set"}); err != nil || c.Name() != "set" {
		t.Fatalf("no secrets set: %v", err)
	}
	if err := run("", "secrets", "set", "SESSION_SECRET"); err == nil || !strings.Contains(err.Error(), "SESSION_SECRET: the value is empty") {
		t.Errorf("no value: %v", err)
	}
	if err := run("v\n", "secrets", "set", "POSTGRES_PASSWORD"); err == nil || !strings.Contains(err.Error(), "settable: SESSION_SECRET") {
		t.Errorf("not settable: %v", err)
	}
}

// ctlIn is ctl with stdin as its standard input.
func ctlIn(e *env, sock, stdin string, args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := vmCommand(e.r, e.self, &out, &errb, func(bool) Prompter { return nil }, clientTimes{unit: time.Millisecond})
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(append(args, "--socket", sock))
	err := cmd.ExecuteContext(context.Background())
	return out.String() + errb.String(), err
}

func TestSecretsSetOverTheSocket(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	e.r, e.self = bundledInstaller(t, "1.0.0", 1, settableSession)
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	h := daemon(e, &fakeHost{}).Handler()
	for _, req := range []SecretSetRequest{{Key: "POSTGRES_PASSWORD", Value: "x"}, {Key: "SESSION_SECRET"}, {Key: "SESSION_SECRET", Value: "a\nb"}} {
		var e400 errOf
		if code := call(t, h, "POST", "/v1/secrets/set", req, &e400); code != 400 {
			t.Errorf("%+v: %d %s", req, code, e400.Error)
		}
	}
	var e409 errOf
	if code := call(t, daemon(e, &fakeHost{judge: "activating"}).Handler(), "POST", "/v1/secrets/set", SecretSetRequest{Key: "SESSION_SECRET", Value: "v"}, &e409); code != 409 ||
		!strings.Contains(e409.Error, "has not judged this boot yet") {
		t.Errorf("judging: %d %s", code, e409.Error)
	}
	// An OS of another release reads another spec: its settable secrets are not the install's.
	other := daemon(e, &fakeHost{})
	other.R, _ = bundledInstaller(t, "2.0.0", 1, settableSession)
	if code := call(t, other.Handler(), "POST", "/v1/secrets/set", SecretSetRequest{Key: "SESSION_SECRET", Value: "v"}, &e409); code != 409 ||
		!strings.Contains(e409.Error, "secrets set runs on the release the OS carries") {
		t.Errorf("another release: %d %s", code, e409.Error)
	}
	sock := served(t, daemon(e, &fakeHost{}))
	out, err := ctlIn(e, sock, "a-chosen-value\n", "secrets", "set", "SESSION_SECRET")
	if err != nil || !strings.Contains(out, "recorded the new SESSION_SECRET") || get(e.readEnv(), "SESSION_SECRET") != "a-chosen-value" {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out, "a-chosen-value") {
		t.Errorf("the job's log shows the value:\n%s", out)
	}
	if out, err := ctlIn(e, sock, "x", "secrets", "set", "POSTGRES_PASSWORD"); err == nil || !strings.Contains(err.Error(), "settable: SESSION_SECRET") {
		t.Errorf("not settable: %v\n%s", err, out)
	}
}

func TestExecOverTheSocket(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	e.docker.exec = func(c Cmd) error {
		switch a := strings.Join(c.Args, " "); {
		case strings.HasSuffix(a, " exec -T api cli rename a b"):
			_, _ = c.Stdout.Write([]byte("renamed\n"))
			_, _ = c.Stream.Write([]byte("a warning\n"))
			return nil
		case strings.HasSuffix(a, " exec -T api false"):
			return exitCode(3)
		case strings.HasSuffix(a, " exec -T api big"):
			_, _ = c.Stdout.Write(bytes.Repeat([]byte("x"), maxExecOutput+1))
			return nil
		}
		return errors.New("docker failed")
	}
	sock := served(t, daemon(e, &fakeHost{}))
	// --socket goes before the command's own arguments.
	ctl := func(_ *env, sock string, args ...string) (string, error) {
		var out bytes.Buffer
		cmd := vmCommand(e.r, e.self, &out, &out, func(bool) Prompter { return nil }, clientTimes{unit: time.Millisecond})
		cmd.SetArgs(append([]string{"--socket", sock}, args...))
		err := cmd.ExecuteContext(context.Background())
		return out.String(), err
	}
	out, err := ctl(e, sock, "exec", "api", "--", "cli", "rename", "a", "b")
	if err != nil || out != "renamed\na warning\n" {
		t.Fatalf("%v\n%q", err, out)
	}
	var code ExitCode
	if _, err := ctl(e, sock, "exec", "api", "--", "false"); !errors.As(err, &code) || code != 3 {
		t.Errorf("the command's exit status: %v", err)
	}
	if out, err := ctl(e, sock, "exec", "api", "--", "big"); err != nil || !strings.Contains(out, "standard output was cut at 4194304 bytes") || strings.Count(out, "x") != maxExecOutput {
		t.Errorf("a long output: %v, %d bytes", err, len(out))
	}
	if _, err := ctl(e, sock, "exec", "api", "--", "other"); err == nil || !strings.Contains(err.Error(), "docker failed") {
		t.Errorf("docker failing: %v", err)
	}
	for args, want := range map[string]string{"exec api cli": "the command goes after --", "exec api --": "the command goes after --", "exec nope -- ls": "the stack has no service nope"} {
		if _, err := ctl(e, sock, strings.Fields(args)...); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", args, err)
		}
	}
	var e400 errOf
	if code := call(t, daemon(e, &fakeHost{}).Handler(), "POST", "/v1/services/api/exec", ExecRequest{}, &e400); code != 400 {
		t.Errorf("no args: %d %s", code, e400.Error)
	}
}

func TestALoginPasswordSetIsOneBcryptTakes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	settableSession(e.r.Spec)
	in := e.installer(nil)
	if err := in.checkSetValue("SESSION_SECRET", strings.Repeat("a", maxSetSecret)); err != nil {
		t.Fatalf("no login: %v", err)
	}
	e.r.Spec.Front.Routes[0].BasicAuth = &spec.BasicAuth{User: "ops", Secret: "SESSION_SECRET"}
	if err := in.checkSetValue("SESSION_SECRET", strings.Repeat("a", maxLoginPassword)); err != nil {
		t.Errorf("at bcrypt's bound: %v", err)
	}
	want := "SESSION_SECRET, route " + e.r.Spec.Front.Routes[0].Name + "'s login password, is 73 bytes, over the 72 bcrypt takes"
	if err := in.checkSetValue("SESSION_SECRET", strings.Repeat("a", maxLoginPassword+1)); err == nil || err.Error() != want {
		t.Errorf("past it: %v", err)
	}
}
