package install

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"charm.land/huh/v2"
	"golang.org/x/term"

	"github.com/norvexlabs/stowage/internal/spec"
)

// Prompter asks the operator for the values the run was not given.
type Prompter interface {
	Ask(ctx context.Context, f Form) (map[string]string, error)
}

// Form is what one Ask asks. An empty answer means "take the default", which Defaults computes
// from the answers so far (a default may follow an earlier answer).
type Form struct {
	Questions []Question
	Defaults  func(answers map[string]string) map[string]string
	// Review shows the answers as they will be recorded, then offers to go on, change an answer
	// or quit.
	Review bool
	// Back, when set, adds a review choice that leaves the form with ErrBack to change an earlier
	// step; it names that step.
	Back string
	// Answers are the answers the form starts from (a form opened again after going back).
	Answers map[string]string
	// Note is shown above the questions: why the form is asked again.
	Note string
	// Go names the review's choice to go on; empty is "Install with these answers".
	Go string
}

// Question is one value to ask.
type Question struct {
	Title    string
	Key      string // shown with the title: the input's key, or the flag it stands for
	Required bool
	Secret   bool // never echoed, and its default is never shown
	// Check refuses an answer; nil means the .env rule (spec.CheckEnvValue).
	Check func(v string) error
	// Choices, when set, make the question a choice of one value; the answer is a Value.
	Choices []Choice
}

// Choice is one value a choice question offers.
type Choice struct{ Value, Label string }

// ErrAborted is returned when the operator leaves the form (Ctrl-C, or end of input in
// accessible mode) or quits at the review.
var ErrAborted = errors.New("aborted")

// ErrBack is returned, with the answers so far, when the operator chooses at the review to change
// the step Form.Back names.
var ErrBack = errors.New("back")

// FormPrompter asks through the terminal. The full form shows every question at once, any field
// can be revisited (Shift+Tab), and each default follows the answers above it. Accessible mode
// asks one plain line at a time, with no cursor movement, for screen readers.
type FormPrompter struct {
	in         io.Reader // a terminal; a secret is read with its echo off
	out        io.Writer
	accessible bool
	lines      *linePrompter // one reader for every Ask, so no read-ahead is lost between them
}

// NewFormPrompter returns a prompter on in and out.
func NewFormPrompter(in io.Reader, out io.Writer, accessible bool) *FormPrompter {
	return &FormPrompter{in: in, out: out, accessible: accessible, lines: &linePrompter{in: bufio.NewReader(in), fd: in, out: out}}
}

// Terminal returns a FormPrompter on stdin and stderr when both are terminals, else nil: a run
// with no terminal asks nothing and takes every default.
func Terminal(accessible bool) Prompter {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stderr.Fd())) {
		return nil
	}
	if os.Getenv("ACCESSIBLE") != "" || os.Getenv("TERM") == "dumb" {
		accessible = true
	}
	return NewFormPrompter(os.Stdin, os.Stderr, accessible)
}

const (
	reviewGo = iota + 1
	reviewChange
	reviewQuit
)

const reviewBack = reviewQuit + 1

var reviewChoices = []string{"Install with these answers", "Change an answer", "Quit, writing nothing"}

func (p *FormPrompter) Ask(ctx context.Context, f Form) (map[string]string, error) {
	vals := make([]string, len(f.Questions))
	for n, q := range f.Questions {
		vals[n] = f.Answers[q.Key]
	}
	answers := func() map[string]string {
		m := make(map[string]string, len(vals))
		for n, q := range f.Questions {
			m[q.Key] = vals[n]
		}
		return m
	}
	// def is question n's default for the answers so far, with v as its own answer.
	def := func(n int, v string) string {
		a := answers()
		a[f.Questions[n].Key] = v
		return f.Defaults(a)[f.Questions[n].Key]
	}
	ask, review := p.full, p.fullReview
	if p.accessible {
		ask, review = p.lines.ask, p.lines.review
	}
	for {
		if err := ask(ctx, f.Note, f.Questions, vals, def); err != nil {
			return nil, err
		}
		if !f.Review {
			return answers(), nil
		}
		choices := slices.Clone(reviewChoices)
		if f.Go != "" {
			choices[0] = f.Go
		}
		if f.Back != "" {
			choices = append(choices, "Change "+f.Back)
		}
		choice, err := review(ctx, summary(f.Questions, answers(), f.Defaults(answers())), choices)
		if err != nil {
			return nil, err
		}
		switch choice {
		case reviewGo:
			return answers(), nil
		case reviewQuit:
			return nil, ErrAborted
		case reviewBack:
			return answers(), ErrBack // the answers, for the form opened again
		}
	}
}

func (p *FormPrompter) full(ctx context.Context, note string, qs []Question, vals []string, def func(int, string) string) error {
	fields := make([]huh.Field, len(qs))
	for n, q := range qs {
		if q.Choices != nil {
			if vals[n] == "" {
				vals[n] = def(n, "")
			}
			opts := make([]huh.Option[string], len(q.Choices))
			for i, c := range q.Choices {
				opts[i] = huh.NewOption(c.Label, c.Value)
			}
			fields[n] = huh.NewSelect[string]().Title(q.Title).Description(q.Key).Options(opts...).Value(&vals[n])
			continue
		}
		in := huh.NewInput().Title(q.Title).Description(q.Key).Value(&vals[n]).
			Validate(func(v string) error { return q.check(v, def(n, v)) })
		if q.Secret {
			in.EchoMode(huh.EchoModePassword)
		} else {
			in.PlaceholderFunc(func() string { return def(n, "") }, &vals)
		}
		fields[n] = in
	}
	return p.run(ctx, huh.NewGroup(fields...).Description(note))
}

func (p *FormPrompter) fullReview(ctx context.Context, summary string, choices []string) (int, error) {
	choice := reviewGo
	opts := make([]huh.Option[int], len(choices))
	for n, c := range choices {
		opts[n] = huh.NewOption(c, n+1)
	}
	err := p.run(ctx, huh.NewGroup(huh.NewSelect[int]().Title("Review").Description(summary).Options(opts...).Value(&choice)))
	return choice, err
}

// run runs a full-screen form. Ctrl-C leaves it; so does a signal that cancels ctx (huh reports
// that as a timeout).
func (p *FormPrompter) run(ctx context.Context, g *huh.Group) error {
	err := huh.NewForm(g).WithInput(p.in).WithOutput(p.out).RunWithContext(ctx)
	if errors.Is(err, huh.ErrUserAborted) || (err != nil && ctx.Err() != nil) {
		return ErrAborted
	}
	return err
}

// linePrompter is accessible mode: one prompt per line, read as typed (nothing is trimmed, as in
// the full form). End of input, or Ctrl-C (which cancels ctx), aborts: it never stands for an
// answer.
type linePrompter struct {
	in  *bufio.Reader
	fd  io.Reader
	out io.Writer
}

func (l *linePrompter) ask(ctx context.Context, note string, qs []Question, vals []string, def func(int, string) string) error {
	if note != "" {
		fmt.Fprintln(l.out, note)
	}
	// A secret that could not be read without echo is refused before the first question.
	for _, q := range qs {
		if q.Secret {
			restore, err := l.noEcho()
			if err != nil {
				return err
			}
			if restore != nil {
				restore()
			}
			break
		}
	}
	for n, q := range qs {
		if q.Choices != nil {
			v, err := l.choose(ctx, q, cmp.Or(vals[n], def(n, "")))
			if err != nil {
				return err
			}
			vals[n] = v
			continue
		}
		for {
			hint := ""
			switch {
			case vals[n] != "" && q.Secret:
				hint = " [Enter keeps the one given]"
			case vals[n] != "":
				hint = " [" + vals[n] + "]"
			case !q.Secret && def(n, "") != "":
				hint = " [" + def(n, "") + "]"
			}
			fmt.Fprintf(l.out, "%s (%s)%s: ", q.Title, q.Key, hint)
			v, err := l.line(ctx, q.Secret)
			if err != nil {
				return err
			}
			if v == "" {
				v = vals[n] // Enter keeps the answer already given, or takes the default
			}
			if err := q.check(v, def(n, v)); err != nil {
				fmt.Fprintf(l.out, "%s: %v\n", q.Key, err)
				continue
			}
			vals[n] = v
			break
		}
	}
	return nil
}

func (l *linePrompter) review(ctx context.Context, summary string, choices []string) (int, error) {
	fmt.Fprintf(l.out, "\nReview:\n%s\n", summary)
	return l.pick(ctx, choices, 0)
}

// choose asks a choice question; Enter keeps cur.
func (l *linePrompter) choose(ctx context.Context, q Question, cur string) (string, error) {
	fmt.Fprintf(l.out, "%s (%s):\n", q.Title, q.Key)
	labels := make([]string, len(q.Choices))
	keep := 0
	for n, c := range q.Choices {
		labels[n] = c.Label
		if c.Value == cur {
			keep = n + 1
		}
	}
	n, err := l.pick(ctx, labels, keep)
	if err != nil {
		return "", err
	}
	return q.Choices[n-1].Value, nil
}

// pick prints numbered choices and reads one, asking again until it gets one; keep, when not 0,
// is the choice Enter takes.
func (l *linePrompter) pick(ctx context.Context, choices []string, keep int) (int, error) {
	for n, c := range choices {
		fmt.Fprintf(l.out, "%d. %s\n", n+1, c)
	}
	for {
		if keep > 0 {
			fmt.Fprintf(l.out, "Choose 1-%d [%d]: ", len(choices), keep)
		} else {
			fmt.Fprintf(l.out, "Choose 1-%d: ", len(choices))
		}
		v, err := l.line(ctx, false)
		if err != nil {
			return 0, err
		}
		if v == "" && keep > 0 {
			return keep, nil
		}
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 1 && n <= len(choices) {
			return n, nil
		}
	}
}

// line reads one line without its line ending; a secret is read with the terminal's echo off,
// put back on every way out, Ctrl-C included.
func (l *linePrompter) line(ctx context.Context, secret bool) (string, error) {
	if ctx.Err() != nil {
		return "", ErrAborted
	}
	if secret {
		restore, err := l.noEcho()
		if err != nil {
			return "", err
		}
		if restore != nil {
			defer restore()
		}
	}
	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		s, err := l.in.ReadString('\n')
		// A last line without a line ending is still an end of input: never an answer.
		ch <- result{strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r"), err}
	}()
	select {
	case <-ctx.Done():
		fmt.Fprintln(l.out)
		return "", ErrAborted
	case r := <-ch:
		if errors.Is(r.err, io.EOF) {
			fmt.Fprintln(l.out)
			return "", ErrAborted
		}
		if secret {
			fmt.Fprintln(l.out) // the Enter that echo did not show
		}
		return r.s, r.err
	}
}

// noEcho turns echo off when the input is a terminal; restore is nil when it is not.
func (l *linePrompter) noEcho() (restore func(), err error) {
	f, ok := l.fd.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return nil, nil
	}
	return noEcho(int(f.Fd()))
}

// summary lists each answer as it will be recorded; a secret shows only whether it is set.
func summary(qs []Question, answers, defaults map[string]string) string {
	var b strings.Builder
	for _, q := range qs {
		v := cmp.Or(answers[q.Key], defaults[q.Key])
		for _, c := range q.Choices {
			if c.Value == v {
				v = c.Label
			}
		}
		switch {
		case q.Secret && v != "":
			v = "(set, hidden)"
		case v == "":
			v = "(empty)"
		}
		fmt.Fprintf(&b, "%s: %s\n", q.Title, v)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// check refuses an answer that cannot be recorded, and an empty one that no default fills.
func (q Question) check(v, def string) error {
	v = cmp.Or(v, def)
	if v == "" && q.Required {
		return errors.New("required")
	}
	if q.Check != nil {
		return q.Check(v)
	}
	return spec.CheckEnvValue(v)
}
