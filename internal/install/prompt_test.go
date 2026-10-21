package install

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/spec"
)

// fakePrompter answers from a map and records what it was asked.
type fakePrompter struct {
	answers  map[string]string
	forms    []Form
	defaults map[string]string // what the last form's Defaults returned for the answers
	err      error
	onAsk    func()
}

func (f *fakePrompter) Ask(_ context.Context, form Form) (map[string]string, error) {
	f.forms = append(f.forms, form)
	if f.onAsk != nil {
		f.onAsk()
	}
	// Like the terminal, the answers start from the form's own.
	a := map[string]string{}
	for k, v := range form.Answers {
		a[k] = v
	}
	for k, v := range f.answers {
		a[k] = v
	}
	if errors.Is(f.err, ErrBack) {
		return a, f.err
	}
	if f.err != nil {
		return nil, f.err
	}
	f.defaults = form.Defaults(a)
	return a, nil
}

func TestOnlyWhatNothingGaveIsAskedAndAnAnswerFeedsTheNextDefault(t *testing.T) {
	e := newEnv(t)
	delete(e.vars, "ADMIN_EMAIL")
	e.r.Spec.Inputs = append(e.r.Spec.Inputs,
		inputWithDefault("OWNER_EMAIL", "${ADMIN_EMAIL}"),
		inputWithDefault("TEAM", "core"),
		spec.Input{Key: "API_TOKEN", Prompt: "Token", Secret: true})
	p := &fakePrompter{answers: map[string]string{"ADMIN_EMAIL": "asked@x"}}
	if err := e.install(func(o *Options) { o.Prompt = p; o.TLS = Loopback; o.Set = map[string]string{"TEAM": "from-set"} }); err != nil {
		t.Fatal(err)
	}
	if len(p.forms) != 1 || !p.forms[0].Review || p.forms[0].Back != "" {
		t.Fatalf("want one reviewed form, got %+v", p.forms)
	}
	qs := p.forms[0].Questions
	var keys []string
	for _, q := range qs {
		keys = append(keys, q.Key)
	}
	if got := strings.Join(keys, ","); got != "ADMIN_EMAIL,OWNER_EMAIL,API_TOKEN" {
		t.Fatalf("asked %s", got)
	}
	if d := p.defaults["OWNER_EMAIL"]; d != "asked@x" || qs[1].Secret {
		t.Errorf("OWNER_EMAIL asked as %+v, default %q", qs[1], d)
	}
	if !qs[0].Required || qs[0].Title != "Administrator email" || !qs[2].Secret {
		t.Errorf("asked as %+v", qs)
	}
	f := e.readEnv()
	for k, want := range map[string]string{"ADMIN_EMAIL": "asked@x", "OWNER_EMAIL": "asked@x", "TEAM": "from-set", "API_TOKEN": ""} {
		if got := get(f, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestLeavingTheFormWritesNothing(t *testing.T) {
	e := newEnv(t)
	delete(e.vars, "ADMIN_EMAIL")
	err := e.install(func(o *Options) { o.Prompt = &fakePrompter{err: ErrAborted} })
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("err = %v", err)
	}
	e.nothingWritten()
	if len(e.docker.ran("pull")) != 0 || len(e.docker.ran("compose -")) != 0 {
		t.Errorf("docker ran: %v", e.docker.calls)
	}
}

func TestTheHostIsCheckedBeforeAnythingIsAsked(t *testing.T) {
	e := newEnv(t)
	e.docker.compose = "2.10.0"
	p := &fakePrompter{}
	if err := e.install(func(o *Options) { o.Prompt = p }); err == nil || !strings.Contains(err.Error(), "too old") || len(p.forms) != 0 {
		t.Fatalf("an old Compose: %v, %d forms", err, len(p.forms))
	}
	e.docker.compose = "2.29.1"
	// The ports depend on the TLS mode: they are checked once it is known, before the inputs.
	e.busy[8080] = true
	if err := e.install(func(o *Options) { o.Prompt = p }); err == nil || !strings.Contains(err.Error(), "8080") {
		t.Fatalf("err = %v", err)
	}
	if len(p.forms) != 1 || p.forms[0].Questions[0].Key != "--tls" {
		t.Errorf("the inputs were asked before the ports were refused: %+v", p.forms)
	}
	p.forms = nil
	e.busy = map[int]bool{}
	delete(e.vars, "ADMIN_EMAIL")
	p.answers = map[string]string{"ADMIN_EMAIL": "a@x"}
	// A port taken while the form is open is refused before anything is pulled or written.
	p.onAsk = func() { e.busy[8000] = true }
	if err := e.install(func(o *Options) { o.Prompt = p; o.TLS = Loopback }); err == nil || !strings.Contains(err.Error(), "8000") || len(p.forms) != 1 {
		t.Fatalf("a port taken during the form: %v, %d forms", err, len(p.forms))
	}
	e.nothingWritten()
	if len(e.docker.ran("pull")) != 0 {
		t.Error("pulled after a port was taken")
	}
	e.busy, p.onAsk = map[int]bool{}, nil
	if err := e.install(func(o *Options) { o.Prompt = p; o.TLS = Loopback }); err != nil || len(p.forms) != 2 {
		t.Fatalf("a free host: %v, %d forms", err, len(p.forms))
	}
}

func TestAnAnswerTheEnvFileCannotHoldIsRefused(t *testing.T) {
	q := Question{Key: "K", Required: true}
	for _, bad := range []string{"", "it's", `a\`} {
		if q.check(bad, "") == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := q.check("fine $x #y", ""); err != nil {
		t.Errorf("a plain value refused: %v", err)
	}
	if err := q.check("", "d"); err != nil {
		t.Errorf("an empty answer with a default refused: %v", err)
	}
	if err := (Question{Key: "K"}).check("", ""); err != nil {
		t.Errorf("an empty optional answer refused: %v", err)
	}
	if err := (Question{Key: "--dir", Check: checkDir}).check("/home/o'brien/app", ""); err != nil {
		t.Errorf("a directory is held to the .env rule: %v", err)
	}
}

var reviewForm = Form{
	Questions: []Question{{Title: "Owner", Key: "OWNER"}, {Title: "Team", Key: "TEAM", Required: true}},
	Defaults: func(a map[string]string) map[string]string {
		return map[string]string{"TEAM": cmp.Or(a["OWNER"], "core") + "-team"}
	},
	Review: true,
}

func askLines(ctx context.Context, f Form, input string) (map[string]string, string, error) {
	var out bytes.Buffer
	a, err := NewFormPrompter(strings.NewReader(input), &out, true).Ask(ctx, f)
	return a, out.String(), err
}

func TestTheAccessibleReviewInstallsChangesOrQuits(t *testing.T) {
	ctx := context.Background()
	// Empty answers take the defaults, which the prompt and the review show; 1 installs.
	a, out, err := askLines(ctx, reviewForm, "\n\n1\n")
	if err != nil || a["OWNER"] != "" || a["TEAM"] != "" {
		t.Fatalf("defaults: %v %v", a, err)
	}
	if !strings.Contains(out, "Team (TEAM) [core-team]: ") || !strings.Contains(out, "Team: core-team") || !strings.Contains(out, "Owner: (empty)") {
		t.Errorf("the defaults are not shown:\n%s", out)
	}
	// 2 goes back through every question; Enter keeps an answer, a new one moves the default after it.
	a, out, err = askLines(ctx, reviewForm, "ann\n\n2\n\n\n2\nbob\n\n1\n")
	if err != nil || a["OWNER"] != "bob" || a["TEAM"] != "" {
		t.Fatalf("change: %v %v", a, err)
	}
	if strings.Count(out, "Team: ann-team") != 2 || !strings.Contains(out, "Owner (OWNER) [ann]: ") || !strings.Contains(out, "Team: bob-team") {
		t.Errorf("the review does not follow the change:\n%s", out)
	}
	// A refused answer and a review choice out of range are asked again; 3 quits.
	if _, out, err = askLines(ctx, reviewForm, "it's\nok\n\n9\nx\n3\n"); !errors.Is(err, ErrAborted) || strings.Count(out, "Owner (OWNER)") != 2 || strings.Count(out, "Choose") != 3 {
		t.Fatalf("quit: %v\n%s", err, out)
	}
	// Answers are recorded as typed, never trimmed.
	if a, _, err = askLines(ctx, reviewForm, " ann \n\n1\n"); err != nil || a["OWNER"] != " ann " {
		t.Fatalf("trimmed: %q %v", a["OWNER"], err)
	}
}

func TestTheReviewNamesItsGoOnChoice(t *testing.T) {
	f := reviewForm
	f.Go = "Go on with these answers"
	_, out, err := askLines(context.Background(), f, "\n\n1\n")
	if err != nil || !strings.Contains(out, "Go on with these answers") || strings.Contains(out, "Install with these answers") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, out, _ = askLines(context.Background(), reviewForm, "\n\n1\n"); !strings.Contains(out, "Install with these answers") {
		t.Errorf("the default choice:\n%s", out)
	}
}

func TestEndOfInputOrCtrlCNeverAnswers(t *testing.T) {
	ctx := context.Background()
	// End of input at a question, after a refused answer, and at the review: each aborts.
	for _, in := range []string{"", "ann", "ann\n\n", "ann\n\n9\n", "it's\n"} {
		if a, _, err := askLines(ctx, reviewForm, in); !errors.Is(err, ErrAborted) {
			t.Errorf("input %q: %v, %v", in, a, err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := askLines(cancelled, reviewForm, "ann\n\n1\n"); !errors.Is(err, ErrAborted) {
		t.Errorf("a cancelled context answered: %v", err)
	}
}

func TestADirectoryTakenDuringTheFormIsRefused(t *testing.T) {
	e := newEnv(t)
	delete(e.vars, "ADMIN_EMAIL")
	p := &fakePrompter{answers: map[string]string{"ADMIN_EMAIL": "a@x"}, onAsk: func() {
		_ = os.MkdirAll(e.dir, 0o755)
		_ = os.WriteFile(filepath.Join(e.dir, ".env"), []byte("X='other run'\n"), 0o600)
	}}
	if err := e.install(func(o *Options) { o.Prompt = p }); err == nil || !strings.Contains(err.Error(), "now holds an install") {
		t.Fatalf("err = %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(e.dir, ".env")); string(raw) != "X='other run'\n" || len(e.docker.ran("pull")) != 0 {
		t.Errorf("the other run's .env was replaced, or images pulled: %q", raw)
	}
}

func TestWithNothingToAskMissingInputsComeBeforeTheHost(t *testing.T) {
	e := newEnv(t)
	delete(e.vars, "ADMIN_EMAIL")
	e.docker.compose = "2.10.0" // too old: the host would be refused too
	if err := e.install(nil); err == nil || !strings.Contains(err.Error(), "ADMIN_EMAIL") {
		t.Fatalf("err = %v", err)
	}
	if len(e.docker.calls) != 0 {
		t.Errorf("docker ran before the inputs were reported: %v", e.docker.calls)
	}
}

func TestAnAdvancedInputIsNeverAskedAndRecordedOnlyWhenGiven(t *testing.T) {
	e := newEnv(t)
	e.r.Spec.Inputs = append(e.r.Spec.Inputs, spec.Input{Key: "WORKERS", Prompt: "Worker processes", Advanced: true},
		spec.Input{Key: "TEAM", Prompt: "Team"})
	p := &fakePrompter{answers: map[string]string{"TEAM": "core"}}
	e.vars["WORKERS"] = "16" // an exported variable never records an advanced input
	if err := e.install(func(o *Options) { o.Prompt = p; o.TLS = Loopback }); err != nil {
		t.Fatal(err)
	}
	for _, f := range p.forms {
		for _, q := range f.Questions {
			if q.Key == "WORKERS" {
				t.Fatal("the advanced input was asked")
			}
		}
	}
	if _, ok := e.readEnv().Get("WORKERS"); ok {
		t.Fatalf("an advanced input nobody gave was recorded:\n%s", e.file(".env"))
	}
	status := func() string {
		e.out.Reset()
		if err := e.installer(nil).Status(context.Background()); err != nil {
			t.Fatal(err)
		}
		return e.out.String()
	}
	if s := status(); !strings.Contains(s, "tunable   WORKERS=(not set)\n") {
		t.Fatalf("status:\n%s", s)
	}
	// A re-run sets it, then gives it back to the compose file's default.
	if err := e.install(func(o *Options) { o.Set = map[string]string{"WORKERS": "8"} }); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.readEnv().Get("WORKERS"); v != "8" || !strings.Contains(status(), "tunable   WORKERS=8\n") {
		t.Fatalf("WORKERS = %q", v)
	}
	if err := e.install(func(o *Options) { o.Set = map[string]string{"WORKERS": ""} }); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.readEnv().Get("WORKERS"); ok || !strings.Contains(e.log.String(), "removing WORKERS") {
		t.Fatalf("WORKERS stayed recorded:\n%s", e.file(".env"))
	}
}

// An L4 route's port taken while the form is open is refused before anything is written.
func TestAnL4PortTakenDuringTheForm(t *testing.T) {
	e := newEnv(t)
	e.r.Spec.L4.Routes = []spec.L4Route{{Name: "data", Protocol: "tcp", Port: 7443, Upstream: "api:7000"}}
	delete(e.vars, "ADMIN_EMAIL")
	p := &fakePrompter{answers: map[string]string{"ADMIN_EMAIL": "a@x"}}
	p.onAsk = func() { e.busy[7443] = true }
	if err := e.install(func(o *Options) { o.Prompt = p; o.TLS = Loopback }); err == nil || !strings.Contains(err.Error(), "7443 (data) already in use") {
		t.Fatalf("err = %v", err)
	}
	e.nothingWritten()
}
