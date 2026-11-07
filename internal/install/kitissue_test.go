package install

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/norvexlabs/stowage/internal/kit"
	"github.com/norvexlabs/stowage/internal/spec"
)

// testHandoff asks SITE, gives TOKEN, and has a check.
func testHandoff() spec.Handoff {
	return spec.Handoff{Name: "client", For: "target", Ask: []spec.HandoffAsk{{Key: "SITE", Prompt: "The site"}},
		Run: spec.HandoffStep{Image: "api", Cmd: []string{"issue"}, Env: []string{"ADMIN_EMAIL"}}, Gives: []string{"TOKEN"},
		Check: &spec.HandoffStep{Image: "api", Cmd: []string{"check"}, Env: []string{}}}
}

func issuing(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.r.Spec.Handoffs = []spec.Handoff{testHandoff()}
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	return e
}

func kitFast(o *Options) { o.kitWork = 4 }

var kitReq = KitRequest{Handoff: "client", Instance: "site-1", Answers: map[string]string{"SITE": "the-site"}}

func TestIssueKitOnDocker(t *testing.T) {
	t.Parallel()
	e := issuing(t)
	e.docker.kitOut = "TOKEN=the-token\n"
	issued, err := e.installer(kitFast).IssueKit(context.Background(), kitReq, kitNow)
	if err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	k, err := kit.Open([]byte(issued.Kit), issued.Code)
	if err != nil {
		t.Fatal(err)
	}
	if k.For != "target" || k.Name != "client" || k.Instance != "site-1" || !k.Issued.Equal(kitNow) || !k.Expires.Equal(kitNow.Add(DefaultKitExpiry)) || k.Inputs["TOKEN"] != "the-token" || k.TLS != nil {
		t.Errorf("%+v", k)
	}
	for _, want := range []string{"STOWAGE_KIT_NAME=client", "STOWAGE_KIT_INSTANCE=site-1", "STOWAGE_KIT_FOR=target", "SITE=the-site"} {
		if !slices.Contains(e.docker.kitEnv, want) {
			t.Errorf("the step has no %s", want)
		}
	}
	if len(issued.Code) < 32 {
		t.Errorf("a code of %d characters", len(issued.Code))
	}
	if _, err := os.Stat(filepath.Join(e.dir, KitCompose)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the step's file stayed: %v", err)
	}
	if strings.Contains(e.log.String(), "the-token") || strings.Contains(e.log.String(), issued.Code) || strings.Contains(e.log.String(), "the-site") {
		t.Errorf("a value in the log:\n%s", e.log)
	}
	// The expiry asked; the name of a key this install records is no bar to giving it.
	req := kitReq
	req.Expires = time.Hour
	issued, err = e.installer(kitFast).IssueKit(context.Background(), req, kitNow)
	if err != nil {
		t.Fatal(err)
	}
	if k, err = kit.Open([]byte(issued.Kit), issued.Code); err != nil || !k.Expires.Equal(kitNow.Add(time.Hour)) {
		t.Errorf("%v %+v", err, k)
	}
	e.r.Spec.Handoffs[0].Gives = []string{"ADMIN_EMAIL"}
	e.docker.kitOut = "ADMIN_EMAIL=site@example.org\n"
	if _, err := e.installer(kitFast).IssueKit(context.Background(), kitReq, kitNow); err != nil {
		t.Errorf("a key this install records: %v", err)
	}
}

// A run's TLS keys reach the kit's tls section as PEM; half a certificate is no kit.
func TestIssueKitOfTLS(t *testing.T) {
	t.Parallel()
	e := issuing(t)
	e.r.Spec.Handoffs[0].Gives = []string{spec.KitTLSCert, spec.KitTLSKey, spec.KitTLSCA, spec.KitTLSClientCA}
	line := func(k, v string) string { return k + "=" + encodeFileInput([]byte(v)) + "\n" }
	e.docker.kitOut = line(spec.KitTLSCert, "C") + line(spec.KitTLSKey, "K") + line(spec.KitTLSCA, "A") + line(spec.KitTLSClientCA, "L")
	issued, err := e.installer(kitFast).IssueKit(context.Background(), kitReq, kitNow)
	if err != nil {
		t.Fatal(err)
	}
	k, err := kit.Open([]byte(issued.Kit), issued.Code)
	if err != nil {
		t.Fatal(err)
	}
	if want := (kit.TLS{Cert: "C", Key: "K", CA: "A", ClientCA: "L"}); k.TLS == nil || *k.TLS != want || len(k.Inputs) != 0 {
		t.Errorf("%+v %+v", k.TLS, k.Inputs)
	}
	e.r.Spec.Handoffs[0].Gives = []string{spec.KitTLSCert}
	e.docker.kitOut = line(spec.KitTLSCert, "C")
	if _, err := e.installer(kitFast).IssueKit(context.Background(), kitReq, kitNow); err == nil || !strings.Contains(err.Error(), "cert and key") {
		t.Errorf("half: %v", err)
	}
	e.docker.kitOut = spec.KitTLSCert + "=not-the-form\n"
	if _, err := e.installer(kitFast).IssueKit(context.Background(), kitReq, kitNow); err == nil || !strings.Contains(err.Error(), "the handoff client's run gave "+spec.KitTLSCert) {
		t.Errorf("the form: %v", err)
	}
}

func TestIssueKitRefusals(t *testing.T) {
	t.Parallel()
	e := issuing(t)
	for name, c := range map[string]struct {
		out  string
		err  error
		mod  func(*KitRequest)
		want string
	}{
		"no handoff":     {mod: func(r *KitRequest) { r.Handoff = "nope" }, want: `no handoff "nope" (handoffs: client)`},
		"bad instance":   {mod: func(r *KitRequest) { r.Instance = "../x" }, want: "the instance"},
		"no instance":    {mod: func(r *KitRequest) { r.Instance = "" }, want: "the instance"},
		"long instance":  {mod: func(r *KitRequest) { r.Instance = strings.Repeat("a", 64) }, want: "the instance"},
		"no answer":      {mod: func(r *KitRequest) { r.Answers = nil }, want: "asks SITE"},
		"answer unasked": {mod: func(r *KitRequest) { r.Answers = map[string]string{"SITE": "x", "OTHER": "y"} }, want: "does not ask OTHER"},
		"bad answer":     {mod: func(r *KitRequest) { r.Answers = map[string]string{"SITE": "a\nb"} }, want: "SITE: "},
		"past":           {mod: func(r *KitRequest) { r.Expires = -time.Second }, want: "expires after now"},
		"run fails":      {out: "TOKEN=the-token\n", err: exitCode(1), want: "did not finish, and no kit is written"},
		"not KEY=VALUE":  {out: "hello\n", want: "KEY=VALUE lines only"},
		"install's key":  {out: "STOWAGE_X=y\nTOKEN=t\n", want: "a value the install records"},
		"nothing given":  {out: "", want: "did not give TOKEN"},
		"empty given":    {out: "TOKEN=\n", want: "did not give TOKEN"},
		"another given":  {out: "TOKEN=t\nOTHER=u\n", want: "gave OTHER, which its gives does not list"},
	} {
		e.docker.kitOut, e.docker.kitErr = c.out, c.err
		req := kitReq
		if c.mod != nil {
			c.mod(&req)
		}
		got, err := e.installer(kitFast).IssueKit(context.Background(), req, kitNow)
		if err == nil || !strings.Contains(err.Error(), c.want) || got != nil {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), "the-token") {
			t.Errorf("%s: a value in the error: %v", name, err)
		}
	}
	// One byte of the expiry's bound: a kit that has just been made is good.
	req := kitReq
	req.Expires = time.Nanosecond
	e.docker.kitOut, e.docker.kitErr = "TOKEN=t\n", nil
	if _, err := e.installer(kitFast).IssueKit(context.Background(), req, kitNow); err != nil {
		t.Errorf("%v", err)
	}
}

func TestCheckKitOnDocker(t *testing.T) {
	t.Parallel()
	e := issuing(t)
	for _, c := range []struct {
		err  error
		want int
	}{{nil, KitProven}, {exitCode(2), KitUnknown}, {exitCode(1), 1}, {exitCode(7), 7}} {
		e.docker.kitErr = c.err
		got, err := e.installer(nil).CheckKit(context.Background(), "client", "site-1")
		if err != nil || got != c.want {
			t.Errorf("%v: %d %v", c.err, got, err)
		}
	}
	if !slices.Contains(e.docker.kitEnv, "STOWAGE_KIT_INSTANCE=site-1") {
		t.Errorf("env %v", e.docker.kitEnv)
	}
	// A check that cannot say is no result.
	e.docker.kitErr = errors.New("the daemon is away")
	if _, err := e.installer(nil).CheckKit(context.Background(), "client", "site-1"); err == nil || !strings.Contains(err.Error(), "daemon is away") {
		t.Errorf("%v", err)
	}
	if _, err := e.installer(nil).CheckKit(context.Background(), "client", "../x"); err == nil || !strings.Contains(err.Error(), "the instance") {
		t.Errorf("%v", err)
	}
	e.r.Spec.Handoffs[0].Check = nil
	if _, err := e.installer(nil).CheckKit(context.Background(), "client", "site-1"); err == nil || !strings.Contains(err.Error(), "declares no check") {
		t.Errorf("%v", err)
	}
}

// fakeVerbs are a shape's verbs that answer as told and count their runs.
type fakeVerbs struct {
	issued *IssuedKit
	err    error
	code   int
	reqs   []KitRequest
}

func (f *fakeVerbs) verbs() kitVerbs {
	return kitVerbs{
		flags: func(*cobra.Command) {},
		create: func(_ *cobra.Command, req KitRequest) (*IssuedKit, error) {
			f.reqs = append(f.reqs, req)
			return f.issued, f.err
		},
		check: func(*cobra.Command, string, string) (int, error) { return f.code, f.err },
	}
}

func kitRelease() *Release {
	return &Release{Spec: &spec.Spec{Name: "notes", Handoffs: []spec.Handoff{testHandoff()}}}
}

func runKit(t *testing.T, f *fakeVerbs, prompt Prompter, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	c := kitCommand(kitRelease(), &out, io.Discard, func(bool) Prompter { return prompt }, f.verbs())
	c.SetArgs(args)
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	err := c.ExecuteContext(context.Background())
	return out.String(), err
}

func TestKitCreateWritesItsFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "a.kit")
	f := &fakeVerbs{issued: &IssuedKit{Kit: "SEALED\n", Code: "the-code"}}
	out, err := runKit(t, f, nil, "create", "client", "--name", "site-1", "--set", "SITE=x", "-o", file, "--expires", "2d")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(file)
	fi, serr := os.Stat(file)
	if string(raw) != "SEALED\n" || serr != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("%q %v %v", raw, fi, serr)
	}
	if !strings.Contains(out, "kit   "+file+"\ncode  the-code\n") || !strings.Contains(out, "--kit "+file) || !strings.Contains(out, "of target") {
		t.Errorf("%s", out)
	}
	if r := f.reqs[0]; r.Handoff != "client" || r.Instance != "site-1" || r.Expires != 48*time.Hour || r.Answers["SITE"] != "x" {
		t.Errorf("%+v", r)
	}
	// Never over a file, and no run is spent finding out.
	f.reqs = nil
	if _, err := runKit(t, f, nil, "create", "client", "--name", "site-1", "--set", "SITE=x", "-o", file); err == nil || !strings.Contains(err.Error(), "never replaces a file") || len(f.reqs) != 0 {
		t.Errorf("over a file: %v, runs %d", err, len(f.reqs))
	}
	if raw, _ := os.ReadFile(file); string(raw) != "SEALED\n" {
		t.Errorf("the file was changed: %q", raw)
	}
	// A run that fails leaves no file behind, and no code.
	failed := filepath.Join(dir, "failed.kit")
	f.err = errors.New("no kit")
	out, err = runKit(t, f, nil, "create", "client", "--name", "site-1", "--set", "SITE=x", "-o", failed)
	if err == nil || out != "" {
		t.Errorf("%v %q", err, out)
	}
	if _, err := os.Stat(failed); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("left: %v", err)
	}
}

func TestKitCreateFlags(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	o := filepath.Join(dir, "x.kit")
	ok := &IssuedKit{Kit: "S", Code: "c"}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"handoff":      {[]string{"create", "nope", "-o", o}, `no handoff "nope"`},
		"zero expiry":  {[]string{"create", "client", "--expires", "0d", "--set", "SITE=x", "-o", o}, "expires after now"},
		"negative":     {[]string{"create", "client", "--expires", "-1h", "--set", "SITE=x", "-o", o}, "expires after now"},
		"bad expiry":   {[]string{"create", "client", "--expires", "soon", "--set", "SITE=x", "-o", o}, "a number of days"},
		"long expiry":  {[]string{"create", "client", "--expires", "36501d", "--set", "SITE=x", "-o", o}, "at most 36500d"},
		"not asked":    {[]string{"create", "client", "--set", "SITE=x", "--set", "OTHER=y", "-o", o}, "does not ask OTHER"},
		"nothing asks": {[]string{"create", "client", "-o", o}, "asks SITE"},
		"instance":     {[]string{"create", "client", "--name", "../x", "--set", "SITE=x", "-o", o}, "the instance"},
	} {
		if _, err := runKit(t, &fakeVerbs{issued: ok}, nil, c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
		if _, err := os.Stat(o); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: left a file", name)
		}
	}
	if d, err := parseKitExpiry("1d"); err != nil || d != 24*time.Hour {
		t.Errorf("%v %v", d, err)
	}
	if d, err := parseKitExpiry("36500d"); err != nil || d <= 0 {
		t.Errorf("%v %v", d, err)
	}
	if d, err := parseKitExpiry(""); err != nil || d != 0 {
		t.Errorf("%v %v", d, err)
	}
}

// Asked on a terminal: the instance, when the handoff has a question for it, and what it asks; a secret
// answer never from a command line.
func TestKitCreateAsks(t *testing.T) {
	t.Parallel()
	h := testHandoff()
	h.Instance = "Which site?"
	h.Ask = append(h.Ask, spec.HandoffAsk{Key: "PASS", Prompt: "A password", Secret: true})
	p := &fakePrompter{answers: map[string]string{"--name": "asked", "SITE": "s", "PASS": "p"}}
	got, err := kitInstance(context.Background(), &h, p)
	if err != nil || got != "asked" {
		t.Errorf("%q %v", got, err)
	}
	if _, err := kitInstance(context.Background(), &h, nil); err == nil || !strings.Contains(err.Error(), "--name gives it") {
		t.Errorf("%v", err)
	}
	plain := testHandoff()
	if got, err := kitInstance(context.Background(), &plain, nil); err != nil || got != "client" {
		t.Errorf("no question: %q %v", got, err)
	}
	a, err := kitAnswers(context.Background(), &h, nil, "", io.Discard, p)
	if err != nil || a["SITE"] != "s" || a["PASS"] != "p" {
		t.Errorf("%v %v", a, err)
	}
	var secret bool
	for _, q := range p.forms[len(p.forms)-1].Questions {
		secret = secret || (q.Key == "PASS" && q.Secret)
	}
	if !secret {
		t.Error("a secret answer is asked in the clear")
	}
	if _, err := kitAnswers(context.Background(), &h, []string{"PASS=p"}, "", io.Discard, p); err == nil || !strings.Contains(err.Error(), "a command line never carries") {
		t.Errorf("%v", err)
	}
	answers := writeTestFile(t, "a.yaml", []byte("SITE: from-the-file\nPASS: secret\n"))
	a, err = kitAnswers(context.Background(), &h, []string{"SITE=from-the-flag"}, answers, io.Discard, nil)
	if err != nil || a["SITE"] != "from-the-flag" || a["PASS"] != "secret" {
		t.Errorf("%v %v", a, err)
	}
}

func TestKitCheckSaysAndExits(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		code int
		out  string
		exit error
	}{{0, "client s: proven\n", nil}, {2, "client s: unknown\n", ExitCode(2)}, {1, "client s: not proven (its check, or starting it, exited 1)\n", ExitCode(1)}, {7, "client s: not proven (its check, or starting it, exited 7)\n", ExitCode(1)}} {
		out, err := runKit(t, &fakeVerbs{code: c.code}, nil, "check", "client", "s")
		if out != c.out || !errors.Is(err, c.exit) {
			t.Errorf("%d: %q %v", c.code, out, err)
		}
	}
	// No answer is unknown, never negative; a check stopped said nothing either.
	if out, err := runKit(t, &fakeVerbs{err: errors.New("away")}, nil, "check", "client", "s"); out != "client s: unknown\n" || !errors.Is(err, ExitCode(KitUnknown)) {
		t.Errorf("%q %v", out, err)
	}
	if out, err := runKit(t, &fakeVerbs{code: -1}, nil, "check", "client", "s"); out != "client s: unknown\n" || !errors.Is(err, ExitCode(KitUnknown)) {
		t.Errorf("%q %v", out, err)
	}
	if c := kitCommand(&Release{Spec: &spec.Spec{Name: "notes"}}, io.Discard, io.Discard, nil, kitVerbs{}); c != nil {
		t.Error("a project without a handoff has the verb")
	}
}

// k8sIssuing is a release of a project with a handoff, running.
func k8sIssuing(t *testing.T) *kubeEnv {
	t.Helper()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	e.release = e.at("1.0.0", 1, func(s *spec.Spec) { s.Handoffs = []spec.Handoff{testHandoff()} })
	e.published(e.release)
	e.runs("1.0.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org"}, ours)
	return e
}

func TestIssueKitOnKubernetes(t *testing.T) {
	t.Parallel()
	e := k8sIssuing(t)
	e.hookRuns("Succeeded", "", map[string]string{"TOKEN": "the-token", "SITE": "the-site"})
	in := e.installer(kitFast)
	issued, err := in.K8sIssueKit(context.Background(), kitReq, kitNow)
	if err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	k, err := kit.Open([]byte(issued.Kit), issued.Code)
	if err != nil {
		t.Fatal(err)
	}
	if k.Inputs["TOKEN"] != "the-token" || len(k.Inputs) != 1 || k.For != "target" || k.Instance != "site-1" {
		t.Errorf("%+v", k)
	}
	const name = "notes-stowage-kit-client"
	meta := e.f.created["metadata"].(map[string]any)
	if meta["name"] != name {
		t.Errorf("the Job %v", meta["name"])
	}
	env := envOf(e.container("provision"))
	if env["STOWAGE_KIT_NAME"] != "client" || env["STOWAGE_KIT_INSTANCE"] != "site-1" || env["STOWAGE_KIT_FOR"] != "target" {
		t.Errorf("env %v", env)
	}
	// An answer reaches the pod from the Secret, never in the Job's spec; a recorded key from its own.
	if env["SITE"] != name+"/SITE" || env["ADMIN_EMAIL"] != "notes-stowage/ADMIN_EMAIL" {
		t.Errorf("env %v", env)
	}
	rec := strings.Join(toStrings(e.container("record")["command"]), " ")
	if !strings.Contains(rec, "--secret "+name) || strings.Contains(rec, "--exit-key") || !strings.Contains(rec, `--reserved `) {
		t.Errorf("record %s", rec)
	}
	refs := e.container("provision")["envFrom"].([]any)[0].(map[string]any)["configMapRef"].(map[string]any)
	if refs["name"] != "notes-stowage-provision" {
		t.Errorf("the chart's ConfigMap: %v", refs)
	}
	// The Secret holding what was issued is gone, before and after the run, as the log never held it.
	deletes := e.f.ran("kubectl delete secret,rolebinding,role,serviceaccount " + name)
	if len(deletes) != 2 {
		t.Errorf("deletes %q", e.f.ran("kubectl delete"))
	}
	if strings.Contains(e.log.String(), "the-token") || strings.Contains(e.log.String(), "the-site") {
		t.Errorf("a value in the log:\n%s", e.log)
	}
}

func TestIssueKitOnKubernetesRefusals(t *testing.T) {
	t.Parallel()
	const name = "notes-stowage-kit-client"
	const del = "kubectl delete secret,rolebinding,role,serviceaccount " + name
	// A failed Job: no kit, the step stopped, the Secret kept: it may hold what was issued.
	e := k8sIssuing(t)
	e.hookRuns("Failed", "the hook exited 1", map[string]string{"TOKEN": "partial"})
	_, err := e.installer(kitFast).K8sIssueKit(context.Background(), kitReq, kitNow)
	if err == nil || !strings.Contains(err.Error(), "the hook exited 1") || !strings.Contains(err.Error(), "no kit is written") || !strings.Contains(err.Error(), "the Secret "+name+" is kept") || !strings.Contains(err.Error(), "kubectl get secret "+name+" --namespace notes --output json") || !strings.Contains(err.Error(), "kubectl delete secret "+name+" --namespace notes") {
		t.Errorf("%v", err)
	}
	if n := len(e.f.ran(del)); n != 1 || len(e.f.ran("kubectl delete job")) != 1 {
		t.Errorf("deletes %q", e.f.ran("kubectl delete"))
	}
	// A key beyond its gives: no kit, and what it gave is not lost.
	e = k8sIssuing(t)
	e.hookRuns("Succeeded", "", map[string]string{"TOKEN": "t", "OTHER": "u"})
	if _, err := e.installer(kitFast).K8sIssueKit(context.Background(), kitReq, kitNow); err == nil || !strings.Contains(err.Error(), "gave OTHER") || !strings.Contains(err.Error(), "is kept") {
		t.Errorf("%v", err)
	}
	if n := len(e.f.ran(del)); n != 1 || len(e.f.ran("kubectl delete job")) != 1 {
		t.Errorf("deletes %q", e.f.ran("kubectl delete"))
	}
	// An interruption keeps it too.
	e = k8sIssuing(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.f.runJob = func(map[string]any) (string, string, map[string]string) {
		cancel()
		return "", outputsJSON(map[string]string{"TOKEN": "t"}), nil
	}
	if _, err := e.installer(kitFast).K8sIssueKit(ctx, kitReq, kitNow); err == nil || !strings.Contains(err.Error(), "is kept") {
		t.Errorf("%v", err)
	}
	if n := len(e.f.ran(del)); n != 1 || len(e.f.ran("kubectl delete job")) != 1 {
		t.Errorf("deletes %q", e.f.ran("kubectl delete"))
	}
	// The hints reach the cluster the run was given.
	e = k8sIssuing(t)
	e.hookRuns("Failed", "the hook exited 1", nil)
	if _, err := e.installer(func(o *Options) { kitFast(o); o.K8s.Context = "prod" }).K8sIssueKit(context.Background(), kitReq, kitNow); err == nil || !strings.Contains(err.Error(), "kubectl delete secret "+name+" --namespace notes --context prod") {
		t.Errorf("%v", err)
	}
	// Another release runs.
	e = k8sIssuing(t)
	e.runs("0.9.0", 1)
	if _, err := e.installer(kitFast).K8sIssueKit(context.Background(), kitReq, kitNow); err == nil || !strings.Contains(err.Error(), "issue it with the installer of 0.9.0") {
		t.Errorf("%v", err)
	}
	// A Job's name no pod can carry; just short enough, it runs.
	e = k8sIssuing(t)
	e.release.Spec.Handoffs[0].Name = strings.Repeat("a", 24)
	req := kitReq
	req.Handoff = e.release.Spec.Handoffs[0].Name
	long := func(release string) error {
		in := e.installer(func(o *Options) { kitFast(o); o.K8s.Release = release })
		_, err := in.K8sIssueKit(context.Background(), req, kitNow)
		return err
	}
	if err := long(strings.Repeat("r", maxK8sName-len("-stowage-kit-")-24+1)); err == nil || !strings.Contains(err.Error(), "too long a name") {
		t.Errorf("%v", err)
	}
	if err := long(strings.Repeat("r", maxK8sName-len("-stowage-kit-")-24)); err != nil && strings.Contains(err.Error(), "too long a name") {
		t.Errorf("%v", err)
	}
}

func TestCheckKitOnKubernetes(t *testing.T) {
	t.Parallel()
	e := k8sIssuing(t)
	e.hookRuns("Succeeded", "", map[string]string{kitExitKey: "2"})
	got, err := e.installer(nil).K8sCheckKit(context.Background(), "client", "site-1")
	if err != nil || got != KitUnknown {
		t.Fatalf("%d %v", got, err)
	}
	rec := strings.Join(toStrings(e.container("record")["command"]), " ")
	if !strings.Contains(rec, "--exit-key "+kitExitKey) {
		t.Errorf("record %s", rec)
	}
	for _, c := range []struct {
		outs map[string]string
		want string
	}{{map[string]string{}, "recorded no exit status"}, {map[string]string{kitExitKey: "x"}, "as its exit status"}} {
		e.hookRuns("Succeeded", "", c.outs)
		if _, err := e.installer(nil).K8sCheckKit(context.Background(), "client", "site-1"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v", c.outs, err)
		}
	}
	e.hookRuns("Failed", "the check did not finish", nil)
	if _, err := e.installer(nil).K8sCheckKit(context.Background(), "client", "site-1"); err == nil {
		t.Error("a failed Job is a result")
	}
	if _, err := e.installer(nil).K8sCheckKit(context.Background(), "nope", "site-1"); err == nil {
		t.Error("an unknown handoff")
	}
}

// `k8s status` names a Secret a killed run left; `k8s uninstall` deletes it.
func TestK8sKitLeftovers(t *testing.T) {
	t.Parallel()
	const name = "notes-stowage-kit-client"
	e := k8sIssuing(t)
	in := e.installer(nil)
	if err := in.K8sStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.out.String(), "leftover") {
		t.Errorf("no Secret, yet: %s", e.out)
	}
	e.f.outputs = outputsJSON(map[string]string{"TOKEN": "partial"})
	if err := in.K8sStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "leftover  the Secret "+name+" holds") || !strings.Contains(e.out.String(), "kubectl delete job/"+name+" secret/"+name+" --ignore-not-found --namespace notes") || strings.Contains(e.out.String(), "partial") {
		t.Errorf("%s", e.out)
	}
	e.f.outputs = `{"metadata": {"labels": {"app.kubernetes.io/managed-by": "someone else"}}}`
	before := e.out.Len()
	if err := in.K8sStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.out.String()[before:], "leftover") {
		t.Errorf("another tool's Secret: %s", e.out)
	}
}

// What one shape issues, another takes: the kit is the shapes' one format.
func TestAKitCrossesShapes(t *testing.T) {
	t.Parallel()
	d := issuing(t)
	d.r.Spec.Handoffs[0].For = "notes"
	d.docker.kitOut = "TOKEN=from-docker\n"
	issued, err := d.installer(kitFast).IssueKit(context.Background(), kitReq, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e := upgradeEnv(t)
	in := e.installer(func(o *Options) {
		o.Getenv = func(n string) string {
			if n == KitCodeVar {
				return issued.Code
			}
			return ""
		}
	})
	f := givenFlags{kit: "-"}
	if err := f.take(context.Background(), in, strings.NewReader(issued.Kit), nil, time.Now(), in.useK8sGiven); err != nil {
		t.Fatal(err)
	}
	if err := in.K8sUpgrade(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if env := e.secretData("notes-stowage"); env["TOKEN"] != "from-docker" {
		t.Errorf("env %v", env)
	}

	k := k8sIssuing(t)
	k.release.Spec.Handoffs[0].For = "notes"
	k.hookRuns("Succeeded", "", map[string]string{"TOKEN": "from-kubernetes"})
	issued, err = k.installer(kitFast).K8sIssueKit(context.Background(), kitReq, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	v := issuing(t)
	v.r.Spec.Inputs = append(v.r.Spec.Inputs, spec.Input{Key: "TOKEN", Prompt: "t", Optional: true})
	vi := v.installer(func(o *Options) {
		environment := o.Getenv
		o.Getenv = func(n string) string {
			if n == KitCodeVar {
				return issued.Code
			}
			return environment(n)
		}
	})
	f = givenFlags{kit: "-"}
	if err := f.take(context.Background(), vi, strings.NewReader(issued.Kit), nil, time.Now(), func(g *Given, flag string) error { return vi.useGiven(g, flag, vi.shape()) }); err != nil {
		t.Fatal(err)
	}
	if err := vi.Install(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	got, err := ReadEnvFile(filepath.Join(v.dir, envName))
	if err != nil {
		t.Fatal(err)
	}
	if val, _ := got.Get("TOKEN"); val != "from-kubernetes" {
		t.Errorf("TOKEN %q", val)
	}
}

// After an install, a project says what it can issue, as it runs on that shape.
func TestAnInstallSaysWhatItIssues(t *testing.T) {
	t.Parallel()
	e := issuing(t)
	in := e.installer(nil)
	in.kitTips(onDocker)
	if !strings.Contains(e.out.String(), " kit create client --dir "+e.dir+"` issues a kit for target: its install takes it with --kit FILE") {
		t.Errorf("%s", e.out)
	}
	in.kitTips(onVM)
	if !strings.Contains(e.out.String(), "`sudo notes-stow kit create client` issues") {
		t.Errorf("%s", e.out)
	}
	in.kitTips(onK8s)
	if !strings.Contains(e.out.String(), "`notes-stow k8s kit create client") {
		t.Errorf("%s", e.out)
	}
}

// The daemon issues and checks on the operators' socket; the answer is the kit's only copy.
func TestTheDaemonIssuesAKit(t *testing.T) {
	t.Parallel()
	e, d, _, _ := backupVM(t)
	e.r.Spec.Handoffs = []spec.Handoff{testHandoff()}
	d.O.kitWork = 4
	e.docker.kitOut = "TOKEN=the-token\n"
	var issued IssuedKit
	if code := call(t, d.Handler(), "POST", "/v1/kits", kitReq, &issued); code != 200 {
		t.Fatalf("%d %+v", code, issued)
	}
	k, err := kit.Open([]byte(issued.Kit), issued.Code)
	if err != nil || k.Inputs["TOKEN"] != "the-token" {
		t.Fatalf("%v %+v", err, k)
	}
	var e400 errOf
	req := kitReq
	req.Handoff = "nope"
	if code := call(t, d.Handler(), "POST", "/v1/kits", req, &e400); code == 200 || !strings.Contains(e400.Error, `no handoff "nope"`) {
		t.Errorf("%d %s", code, e400.Error)
	}
	e.docker.kitErr = exitCode(2)
	var res KitCheckResult
	if code := call(t, d.Handler(), "POST", "/v1/kits/check", KitCheckRequest{Handoff: "client", Instance: "site-1"}, &res); code != 200 || res.Exit != KitUnknown {
		t.Errorf("%d %+v", code, res)
	}
}

// A check's Job records its exit status alone, whatever its stdout says, and ends well: the result is
// the check's, which a failed Job could not say. A check stopped says -1, never "proven".
func TestRecordExit(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		status hookStatus
		want   string
	}{{hookStatus{Exit: 0}, "0"}, {hookStatus{Exit: 2}, "2"}, {hookStatus{Exit: 9}, "9"}, {hookStatus{Exit: 0, Stopped: "stopped after 1m"}, "-1"}, {hookStatus{Exit: -1, Stopped: "no program"}, "-1"}} {
		dir := t.TempDir()
		raw, _ := json.Marshal(c.status)
		if err := os.WriteFile(filepath.Join(dir, hookStatusFile), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, hookOutFile), []byte("TOKEN=not-a-record\nnot even KEY=VALUE\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var body struct{ Data map[string]string }
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
		}))
		tok := t.TempDir()
		_ = os.WriteFile(filepath.Join(tok, "token"), []byte("tok\n"), 0o600)
		r := &hookRecorder{dir: dir, token: tok, namespace: "notes", secret: "s", exitKey: kitExitKey, log: io.Discard, client: srv.Client(), api: srv.URL}
		err := r.record(context.Background())
		srv.Close()
		if err != nil {
			t.Errorf("%+v: %v", c.status, err)
		}
		if len(body.Data) != 1 || body.Data[kitExitKey] != base64.StdEncoding.EncodeToString([]byte(c.want)) {
			t.Errorf("%+v: %v", c.status, body.Data)
		}
	}
	// The pod stopped first: no result is recorded.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &hookRecorder{dir: t.TempDir(), exitKey: kitExitKey, log: io.Discard}
	if err := r.record(ctx); err == nil || !strings.Contains(err.Error(), "the pod was stopped") {
		t.Errorf("%v", err)
	}
}

// Another tool's Secret of the kit's name is neither deleted nor written over.
func TestK8sKitLeavesAnotherToolsSecret(t *testing.T) {
	t.Parallel()
	e := k8sIssuing(t)
	e.f.outputs = `{"metadata": {"labels": {"app.kubernetes.io/managed-by": "someone else"}}}`
	if _, err := e.installer(kitFast).K8sIssueKit(context.Background(), kitReq, kitNow); err == nil || !strings.Contains(err.Error(), "was not made by") {
		t.Errorf("%v", err)
	}
	if got := e.f.ran("kubectl delete"); len(got) != 0 {
		t.Errorf("deleted %q", got)
	}
	if e.f.created != nil {
		t.Error("a Job ran")
	}
}

// Uninstalling deletes what a handoff's killed run left.
func TestK8sUninstallDeletesAKitsLeftovers(t *testing.T) {
	t.Parallel()
	e := k8sIssuing(t)
	e.f.outputs = outputsJSON(map[string]string{"TOKEN": "partial"})
	in := e.installer(func(o *Options) { o.Yes, o.K8s.Timeout = true, time.Minute })
	if err := in.K8sUninstall(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if got := e.f.ran("kubectl delete secret notes-stowage-kit-client"); len(got) != 1 {
		t.Errorf("%q", e.f.ran("kubectl delete"))
	}
}

// A step is written for the release that runs: another's install is refused, naming the way.
func TestIssueKitOfAnotherRelease(t *testing.T) {
	t.Parallel()
	e := issuing(t)
	e.docker.kitOut = "TOKEN=t\n"
	env, err := ReadEnvFile(filepath.Join(e.dir, envName))
	if err != nil {
		t.Fatal(err)
	}
	env.Set("STOWAGE_PROJECT_VERSION", "0.0.1")
	if err := env.WriteFile(filepath.Join(e.dir, envName)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.installer(kitFast).IssueKit(context.Background(), kitReq, kitNow); err == nil || !strings.Contains(err.Error(), "upgrade") {
		t.Errorf("%v", err)
	}
	if _, err := e.installer(nil).CheckKit(context.Background(), "client", "site-1"); err == nil || !strings.Contains(err.Error(), "upgrade") {
		t.Errorf("%v", err)
	}
}

// The end of an install, on Docker and on Kubernetes, names what it can issue.
func TestTheEndOfAnInstallTips(t *testing.T) {
	t.Parallel()
	e := issuing(t)
	e.out.Reset()
	e.installer(nil).Finished(context.Background(), onDocker)
	if !strings.Contains(e.out.String(), "kit create client") {
		t.Errorf("docker: %s", e.out)
	}
	k := k8sIssuing(t)
	k.installer(nil).k8sFinished(context.Background(), map[string]string{}, "")
	if !strings.Contains(k.out.String(), "k8s kit create client") {
		t.Errorf("kubernetes: %s", k.out)
	}
}

// The account must be able to delete what a run leaves, before the run.
func TestK8sKitNeedsToDelete(t *testing.T) {
	t.Parallel()
	e := k8sIssuing(t)
	e.f.denied = []string{"delete secrets"}
	if _, err := e.installer(kitFast).K8sIssueKit(context.Background(), kitReq, kitNow); err == nil || !strings.Contains(err.Error(), "may not, in notes: delete secrets") || strings.Contains(err.Error(), "--no-provision") {
		t.Errorf("%v", err)
	}
	// As it must read what it might delete, to leave another tool's.
	f := k8sIssuing(t)
	f.f.denied = []string{"get serviceaccounts"}
	if _, err := f.installer(kitFast).K8sIssueKit(context.Background(), kitReq, kitNow); err == nil || !strings.Contains(err.Error(), "may not, in notes: get serviceaccounts") {
		t.Errorf("%v", err)
	}
	if e.f.created != nil {
		t.Error("a Job ran")
	}
}

// Failing to delete the finished Job, which holds no value, costs no kit.
func TestK8sKitSurvivesTheJobsDeletion(t *testing.T) {
	t.Parallel()
	e := k8sIssuing(t)
	e.f.deleteJobErr = errors.New("the API server is away")
	e.hookRuns("Succeeded", "", map[string]string{"TOKEN": "the-token"})
	if _, err := e.installer(kitFast).K8sIssueKit(context.Background(), kitReq, kitNow); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if !strings.Contains(e.log.String(), "could not be deleted, and blocks the next run") {
		t.Errorf("log %s", e.log)
	}
	// A run that failed too: its step may still run, and the error says so.
	e = k8sIssuing(t)
	e.f.deleteJobErr = errors.New("the API server is away")
	e.hookRuns("Failed", "the hook exited 1", nil)
	if _, err := e.installer(kitFast).K8sIssueKit(context.Background(), kitReq, kitNow); err == nil || !strings.Contains(err.Error(), "its step may still be running") || !strings.Contains(err.Error(), "is kept") {
		t.Errorf("%v", err)
	}
}

// While its Job exists no other run starts: its Secret is what that run has yet to read. The Secret
// goes before the Job, so a next run never cleans under a run reading.
func TestK8sKitJobIsItsLock(t *testing.T) {
	t.Parallel()
	const name = "notes-stowage-kit-client"
	labels, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": ours}})
	e := k8sIssuing(t)
	e.f.objects = map[string]string{"job/" + name: string(labels)}
	if _, err := e.installer(kitFast).K8sIssueKit(context.Background(), kitReq, kitNow); err == nil || !strings.Contains(err.Error(), "is there, of a run under way or one interrupted") || !strings.Contains(err.Error(), "kubectl delete job/"+name+" secret/"+name+" --ignore-not-found --namespace notes") {
		t.Errorf("%v", err)
	}
	if got := e.f.ran("kubectl delete"); len(got) != 0 {
		t.Errorf("deleted %q", got)
	}
	e.f.objects = map[string]string{"job/" + name: `{"metadata": {"labels": {"app.kubernetes.io/managed-by": "someone else"}}}`}
	if _, err := e.installer(kitFast).K8sIssueKit(context.Background(), kitReq, kitNow); err == nil || !strings.Contains(err.Error(), "job "+name+" in notes was not made by") {
		t.Errorf("%v", err)
	}
	e = k8sIssuing(t)
	e.hookRuns("Succeeded", "", map[string]string{"TOKEN": "t"})
	if _, err := e.installer(kitFast).K8sIssueKit(context.Background(), kitReq, kitNow); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, c := range e.f.ran("kubectl delete") {
		order = append(order, c[2])
	}
	if strings.Join(order, " ") != "secret,rolebinding,role,serviceaccount secret,rolebinding,role,serviceaccount job" {
		t.Errorf("%q", order)
	}
	// What it gave is read before its Secret goes.
	read, gone := -1, -1
	for n, c := range e.f.ran("kubectl") {
		switch line := strings.Join(c, " "); {
		case strings.HasPrefix(line, "kubectl get secret "+name):
			read = n
		case strings.HasPrefix(line, "kubectl delete secret,"):
			gone = n
		}
	}
	if read < 0 || gone < read {
		t.Errorf("read at %d, deleted at %d", read, gone)
	}
}

// A second run that loses the race for the Job leaves the first's objects alone; any other failure
// to create it does not leave them, nor a Job the server may have taken.
func TestK8sKitOverlap(t *testing.T) {
	t.Parallel()
	const del = "kubectl delete secret,rolebinding,role,serviceaccount notes-stowage-kit-client"
	for _, c := range []struct {
		err           string
		deletes, jobs int
	}{{`Error from server (AlreadyExists): jobs.batch "x" already exists`, 1, 0}, {"the API server is away", 2, 1}} {
		e := k8sIssuing(t)
		e.f.createErr = errors.New(c.err)
		if _, err := e.installer(kitFast).K8sIssueKit(context.Background(), kitReq, kitNow); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%v", err)
		}
		if n := len(e.f.ran(del)); n != c.deletes {
			t.Errorf("%s: deletes %d, want %d", c.err, n, c.deletes)
		}
		if n := len(e.f.ran("kubectl delete job")); n != c.jobs {
			t.Errorf("%s: Job deletes %d, want %d", c.err, n, c.jobs)
		}
	}
}

// Another tool's Role of the kit's name stays too.
func TestK8sKitLeavesAnotherToolsRole(t *testing.T) {
	t.Parallel()
	e := k8sIssuing(t)
	e.f.objects = map[string]string{"role/notes-stowage-kit-client": `{"metadata": {"labels": {"app.kubernetes.io/managed-by": "someone else"}}}`}
	if _, err := e.installer(kitFast).K8sIssueKit(context.Background(), kitReq, kitNow); err == nil || !strings.Contains(err.Error(), "role notes-stowage-kit-client in notes was not made by") {
		t.Errorf("%v", err)
	}
	if got := e.f.ran("kubectl delete"); len(got) != 0 {
		t.Errorf("deleted %q", got)
	}
}

// A check's stdout is thrown away: past every bound of a run's, it still ends by its exit status.
func TestACheckStdoutIsThrownAway(t *testing.T) {
	t.Parallel()
	e := k8sIssuing(t)
	e.hookRuns("Succeeded", "", map[string]string{kitExitKey: "0"})
	if _, err := e.installer(nil).K8sCheckKit(context.Background(), "client", "site-1"); err != nil {
		t.Fatal(err)
	}
	if cmd := strings.Join(toStrings(e.container("provision")["command"]), " "); !strings.Contains(cmd, k8sHookCommand+" run --discard-stdout --dir") {
		t.Errorf("%s", cmd)
	}
	dir := t.TempDir()
	script := writeTestFile(t, "big", []byte("#!/bin/sh\nhead -c 3000000 /dev/zero\n"))
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := hookRunTo(context.Background(), dir, time.Minute, []string{script}, io.Discard, true); err != nil {
		t.Fatal(err)
	}
	st, err := readStatus(dir)
	if err != nil || st == nil || st.Exit != 0 || st.Stopped != "" {
		t.Errorf("%+v %v", st, err)
	}
	// Its stdout goes nowhere, not to the file the record step would read.
	if raw, _ := os.ReadFile(filepath.Join(dir, hookOutFile)); len(raw) != 0 {
		t.Errorf("%d bytes kept", len(raw))
	}
	// A background child holding its stdout open is no reason to wait, nor to fail.
	lingers := writeTestFile(t, "lingers", []byte("#!/bin/sh\nsleep 4 &\nexit 0\n"))
	if err := os.Chmod(lingers, 0o755); err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	stderr, err := os.Create(filepath.Join(dir, "stderr")) // a file, as the pod's stderr is: no pipe either
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	start := time.Now()
	if err := hookRunTo(context.Background(), dir, time.Minute, []string{lingers}, stderr, true); err != nil {
		t.Fatal(err)
	}
	if st, err = readStatus(dir); err != nil || st == nil || st.Exit != 0 || st.Stopped != "" || time.Since(start) > 3*time.Second {
		t.Errorf("%+v %v after %s", st, err, time.Since(start))
	}
}

// A step's stderr past its bound cuts the log, never the answer that holds the kit: bound where the
// docker CLI's stderr reaches it, through exec's copy, as well as by a Write.
func TestABigLogCostsNoKit(t *testing.T) {
	t.Parallel()
	var b boundedLog
	if _, err := b.Write(bytes.Repeat([]byte("x"), maxKitLog)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "cut at") || b.buf.Len() != maxKitLog {
		t.Errorf("at the bound: %d", b.buf.Len())
	}
	if _, err := b.Write([]byte("y")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "cut at") || b.buf.Len() != maxKitLog {
		t.Errorf("past it: %d", b.buf.Len())
	}
	var c boundedLog
	cmd := exec.Command("sh", "-c", "head -c 3000000 /dev/zero >&2")
	cmd.Stderr = &c
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if c.buf.Len() != maxKitLog || !c.over {
		t.Errorf("through exec: %d %v", c.buf.Len(), c.over)
	}
}

// A log the terminal refuses costs no kit; a code it refuses is said.
func TestKitCreateOutputFailures(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := &fakeVerbs{issued: &IssuedKit{Kit: "SEALED\n", Code: "the-code", Log: "said\n"}}
	c := kitCommand(kitRelease(), failingWriter{}, failingWriter{}, func(bool) Prompter { return nil }, f.verbs())
	file := filepath.Join(dir, "a.kit")
	c.SetArgs([]string{"create", "client", "--name", "s", "--set", "SITE=x", "-o", file})
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	if err := c.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "the code could not be shown") {
		t.Errorf("%v", err)
	}
	if raw, _ := os.ReadFile(file); string(raw) != "SEALED\n" {
		t.Errorf("%q", raw)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// hookRun is what a run's step does: its stdout kept, for the record step.
func hookRun(ctx context.Context, dir string, timeout time.Duration, argv []string, stderr io.Writer) error {
	return hookRunTo(ctx, dir, timeout, argv, stderr, false)
}

// The Secret a finished Job leaves may be the only copy of what was issued: a read failing a moment is
// tried again, and one that keeps failing keeps it and its Job, naming them.
func TestK8sKitReadsItsSecretAgain(t *testing.T) {
	poll := k8sHookPoll
	k8sHookPoll = time.Millisecond
	t.Cleanup(func() { k8sHookPoll = poll })
	const name = "notes-stowage-kit-client"
	e := k8sIssuing(t)
	e.hookRuns("Succeeded", "", map[string]string{"TOKEN": "the-token"})
	e.f.kitReadErrs = kitReadTries - 1
	if _, err := e.installer(kitFast).K8sIssueKit(context.Background(), kitReq, kitNow); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	e = k8sIssuing(t)
	e.hookRuns("Succeeded", "", map[string]string{"TOKEN": "the-token"})
	e.f.kitReadErrs = kitReadTries
	_, err := e.installer(kitFast).K8sIssueKit(context.Background(), kitReq, kitNow)
	if err == nil || !strings.Contains(err.Error(), "is kept") || !strings.Contains(err.Error(), "kubectl get secret "+name+" --namespace notes --output json") {
		t.Fatalf("%v", err)
	}
	if n := len(e.f.ran("kubectl delete secret,rolebinding,role,serviceaccount " + name)); n != 1 {
		t.Errorf("deleted after the run: %d deletes in all", n)
	}
}

// What a check said is tried again while the API server is away, as a run's last values are; a refusal
// is not.
func TestRecordExitTriesAgain(t *testing.T) {
	follow, backoff := hookFollow, patchBackoff
	hookFollow, patchBackoff = time.Millisecond, time.Millisecond
	t.Cleanup(func() { hookFollow, patchBackoff = follow, backoff })
	run := func(answer func(n int) int) (int, error) {
		dir := t.TempDir()
		raw, _ := json.Marshal(hookStatus{Exit: 2})
		if err := os.WriteFile(filepath.Join(dir, hookStatusFile), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		n := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n++
			w.WriteHeader(answer(n))
		}))
		defer srv.Close()
		tok := t.TempDir()
		_ = os.WriteFile(filepath.Join(tok, "token"), []byte("tok\n"), 0o600)
		r := &hookRecorder{dir: dir, token: tok, namespace: "notes", secret: "s", exitKey: kitExitKey, log: io.Discard, client: srv.Client(), api: srv.URL}
		err := r.record(context.Background())
		return n, err
	}
	n, err := run(func(n int) int {
		if n < 10 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})
	if err != nil || n != 10 {
		t.Errorf("%d tries: %v", n, err)
	}
	n, err = run(func(int) int { return http.StatusForbidden })
	if err == nil || n > 1 {
		t.Errorf("refused: %d tries: %v", n, err)
	}
}

// A wait that lost the API server may be hiding a run that ended well: its Secret is kept. A check
// keeps nothing, as its Job and Secret would block the next.
func TestK8sKitKeepsWhatItCannotTell(t *testing.T) {
	poll, stuck := k8sHookPoll, k8sHookStuck
	k8sHookPoll, k8sHookStuck = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { k8sHookPoll, k8sHookStuck = poll, stuck })
	const name = "notes-stowage-kit-client"
	const del = "kubectl delete secret,rolebinding,role,serviceaccount " + name
	e := k8sIssuing(t)
	e.f.podsErrs = 1 << 20
	_, err := e.installer(kitFast).K8sIssueKit(context.Background(), kitReq, kitNow)
	if err == nil || !strings.Contains(err.Error(), "is kept") || !strings.Contains(err.Error(), "kubectl get secret "+name) {
		t.Fatalf("%v", err)
	}
	if n := len(e.f.ran(del)); n != 1 {
		t.Errorf("deletes %q", e.f.ran("kubectl delete"))
	}
	c := k8sIssuing(t)
	c.f.podsErrs = 1 << 20
	if _, err := c.installer(nil).K8sCheckKit(context.Background(), "client", "site-1"); err == nil || strings.Contains(err.Error(), "is kept") {
		t.Errorf("%v", err)
	}
	if n := len(c.f.ran(del)); n != 2 || len(c.f.ran("kubectl delete job")) != 1 {
		t.Errorf("deletes %q", c.f.ran("kubectl delete"))
	}
	r := k8sIssuing(t)
	r.hookRuns("Succeeded", "", map[string]string{kitExitKey: "0"})
	r.f.kitReadErrs = kitReadTries
	if _, err := r.installer(nil).K8sCheckKit(context.Background(), "client", "site-1"); err == nil || strings.Contains(err.Error(), "is kept") || len(r.f.ran("kubectl delete job")) != 1 {
		t.Errorf("%v", err)
	}
}

// A Secret holding what an earlier run gave stops the next, which would delete it; a check's exit
// status does not.
func TestK8sKitRefusesWhatAnEarlierRunLeft(t *testing.T) {
	t.Parallel()
	const name = "notes-stowage-kit-client"
	e := k8sIssuing(t)
	e.f.outputs = outputsJSON(map[string]string{"TOKEN": "the-only-copy"})
	_, err := e.installer(kitFast).K8sIssueKit(context.Background(), kitReq, kitNow)
	if err == nil || !strings.Contains(err.Error(), "perhaps the only copy") || !strings.Contains(err.Error(), "kubectl delete secret "+name+" --namespace notes") || strings.Contains(err.Error(), "the-only-copy") {
		t.Errorf("%v", err)
	}
	if got := e.f.ran("kubectl delete"); len(got) != 0 || e.f.created != nil {
		t.Errorf("deleted %q", got)
	}
	if _, err := e.installer(nil).K8sCheckKit(context.Background(), "client", "site-1"); err == nil || !strings.Contains(err.Error(), "perhaps the only copy") {
		t.Errorf("a check: %v", err)
	}
	e = k8sIssuing(t)
	e.f.outputs = outputsJSON(map[string]string{kitExitKey: "1"})
	e.hookRuns("Succeeded", "", map[string]string{kitExitKey: "0"})
	if code, err := e.installer(nil).K8sCheckKit(context.Background(), "client", "site-1"); err != nil || code != 0 {
		t.Errorf("%d %v", code, err)
	}
}

// A project with handoffs and no provision hook has what its Job runs.
func TestAHandoffsJobHasItsCommand(t *testing.T) {
	t.Parallel()
	e := k8sIssuing(t)
	e.release.Spec.Hooks.Provision = nil
	if c, _, err := newCommand(e.release, "notes-stow", io.Discard, io.Discard, K8sOptions{}, nil).Find([]string{k8sHookCommand, "run"}); err != nil || c.Name() != "run" {
		t.Errorf("%v", err)
	}
	e.release.Spec.Handoffs = nil
	if c, _, err := newCommand(e.release, "notes-stow", io.Discard, io.Discard, K8sOptions{}, nil).Find([]string{k8sHookCommand}); err == nil && c.Name() == k8sHookCommand {
		t.Error("a project with neither has it")
	}
}

// A check that never ran is no answer: wrong arguments and flags are not "not proven".
func TestKitCheckUsageIsNoAnswer(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"check", "client"}, {"check", "client", "a", "b"}, {"check", "client", "a", "--nope"}} {
		if _, err := runKit(t, &fakeVerbs{}, nil, args...); !errors.Is(err, ExitCode(KitUnknown)) {
			t.Errorf("%q: %v", args, err)
		}
	}
	// A failure to read the leftovers does not stop the status, nor lose those read before it.
	two := k8sIssuing(t)
	second := testHandoff()
	second.Name = "second"
	two.release.Spec.Handoffs = append(two.release.Spec.Handoffs, second)
	two.f.outputs = outputsJSON(map[string]string{"TOKEN": "x"})
	two.f.objects = nil
	two.f.created = map[string]any{}
	two.f.kitReadErrs = 0
	if left, err := two.installer(nil).k8sKitLeftovers(context.Background()); err != nil || len(left) != 2 {
		t.Errorf("%v %v", left, err)
	}
	e := k8sIssuing(t)
	e.f.created = map[string]any{}
	e.f.kitReadErrs = 1
	if err := e.installer(nil).K8sStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.log.String(), "leftovers could not be read") || len(e.f.ran("helm status")) != 1 {
		t.Errorf("%s", e.log)
	}
}

// The API server's throttling is no refusal.
func TestRecordExitWaitsOutThrottling(t *testing.T) {
	follow, backoff := hookFollow, patchBackoff
	hookFollow, patchBackoff = time.Millisecond, time.Millisecond
	t.Cleanup(func() { hookFollow, patchBackoff = follow, backoff })
	for _, status := range []int{http.StatusTooManyRequests, http.StatusRequestTimeout} {
		dir := t.TempDir()
		raw, _ := json.Marshal(hookStatus{Exit: 0})
		if err := os.WriteFile(filepath.Join(dir, hookStatusFile), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		n := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n++
			if n < 4 {
				w.WriteHeader(status)
			}
		}))
		tok := t.TempDir()
		_ = os.WriteFile(filepath.Join(tok, "token"), []byte("tok\n"), 0o600)
		r := &hookRecorder{dir: dir, token: tok, namespace: "notes", secret: "s", exitKey: kitExitKey, log: io.Discard, client: srv.Client(), api: srv.URL}
		err := r.record(context.Background())
		srv.Close()
		if err != nil || n != 4 {
			t.Errorf("%d: %d tries: %v", status, n, err)
		}
	}
}

// A release pulled from a mirror of its own CA can run a kit.
func TestK8sKitTakesTheRegistryCA(t *testing.T) {
	t.Parallel()
	e := k8sIssuing(t)
	for _, verb := range []string{"create", "check"} {
		c, _, err := newCommand(e.release, "notes-stow", io.Discard, io.Discard, K8sOptions{}, nil).Find([]string{"k8s", "kit", verb})
		if err != nil || c.Name() != verb || c.Flags().Lookup("registry-ca") == nil {
			t.Errorf("%s: %v", verb, err)
		}
	}
}
