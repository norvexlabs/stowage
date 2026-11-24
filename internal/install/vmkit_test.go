package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/kit"
)

// exitStatus is an error of a program that exited with a status, as exec's ExitError is.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitStatus) ExitCode() int { return int(e) }

// kitSSH is ssh to a VM: each call recorded with its standard input; stdout written, err returned.
type kitSSH struct {
	// prechecks counts the runs of `sudo -n <ctl> --help` that try the link first, which answer
	// precheckErr.
	prechecks   int
	precheckErr error
	calls       [][]string
	stdins      []string
	stdout      string
	err         error
}

func (s *kitSSH) Run(_ context.Context, c Cmd) ([]byte, error) {
	if n := len(c.Args); n > 4 && slices.Equal(c.Args[n-4:], []string{"sudo", "-n", "notes-stow", "--help"}) {
		s.prechecks++
		return nil, s.precheckErr
	}
	s.calls = append(s.calls, c.Args)
	in := ""
	if c.Stdin != nil {
		b, _ := io.ReadAll(c.Stdin)
		in = string(b)
	}
	s.stdins = append(s.stdins, in)
	if c.Stdout != nil {
		_, _ = io.WriteString(c.Stdout, s.stdout)
	}
	return []byte(s.stdout), s.err
}

// vmKitRun is an operator's installer reaching the VM notes-1 over ssh, its stdout out, and its
// XDG state directory.
func vmKitRun(t *testing.T, ssh *kitSSH, env map[string]string) (*Installer, *bytes.Buffer, string) {
	t.Helper()
	state := t.TempDir()
	var out bytes.Buffer
	in := &Installer{R: kitRelease(), O: Options{Out: &out, Log: io.Discard, SSH: ssh, Getenv: func(k string) string {
		switch k {
		case "XDG_STATE_HOME":
			return state
		case "HOME", "USER":
			return ""
		}
		return env[k]
	}}}
	return in, &out, state
}

// testKnownLine is the VM's host key the kit tests give with --known-hosts-line.
const testKnownLine = "notes-1 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEAIlrmYko7n75rw1in9PSsqvQQwoVBxv2WjCA2q6yuS"

// runVMKit runs vm kit as the CLI does: the command built from the Options in holds, before a run
// copies them into a fresh installer (prepare), so nothing it builds reads the installer's own.
func runVMKit(t *testing.T, in *Installer, stdin io.Reader, args ...string) error {
	t.Helper()
	if !slices.Contains(args, "--known-hosts-line") {
		args = append(args, "--known-hosts-line", testKnownLine)
	}
	return runVMKitBare(t, in, stdin, args...)
}

// runVMKitBare is runVMKit without the host key it adds.
func runVMKitBare(t *testing.T, in *Installer, stdin io.Reader, args ...string) error {
	t.Helper()
	o := in.O
	fresh := &Installer{R: in.R}
	c := fresh.vmKitCommand(&o, func() error { fresh.O = o; return nil }, func(bool) Prompter { return nil })
	c.SetArgs(args)
	c.SetIn(stdin)
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	return c.ExecuteContext(context.Background())
}

// vm kit create runs the VM's `kit create --json` over the pinned SSH, the request on its standard
// input (a secret answer never in the command), and writes the kit here.
func TestVMKitCreate(t *testing.T) {
	t.Parallel()
	ssh := &kitSSH{stdout: `{"kit":"SEALED\n","code":"the-code","log":"ran\n"}` + "\n"}
	in, out, _ := vmKitRun(t, ssh, nil)
	in.R.Spec.Handoffs[0].Ask[0].Secret = true
	answers := filepath.Join(t.TempDir(), "answers.yaml")
	if err := os.WriteFile(answers, []byte("SITE: hush-hush\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "a.kit")
	if err := runVMKit(t, in, nil, "create", "notes-1", "client", "--name", "site-1", "--answers", answers, "-o", file, "--user", "ops", "--host", "Notes-1.LAN"); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(file); string(raw) != "SEALED\n" {
		t.Errorf("kit %q", raw)
	}
	if !strings.Contains(out.String(), "kit   "+file+"\ncode  the-code\n") {
		t.Errorf("out %q", out)
	}
	c := ssh.calls[0]
	if len(ssh.calls) != 1 || !slices.Equal(c[len(c)-6:], []string{"sudo", "notes-stow", "kit", "create", "client", "--json"}) ||
		!slices.Contains(c, "ops@Notes-1.LAN") || !slices.Contains(c, "StrictHostKeyChecking=yes") || !slices.Contains(c, "HostKeyAlias=notes-1") ||
		!slices.Contains(c, "GlobalKnownHostsFile=/dev/null") ||
		slices.ContainsFunc(c, func(a string) bool { return strings.Contains(a, "hush-hush") }) {
		t.Fatalf("ssh ran %q", c)
	}
	var req KitRequest
	if err := json.Unmarshal([]byte(ssh.stdins[0]), &req); err != nil || req.Handoff != "client" || req.Instance != "site-1" || req.Answers["SITE"] != "hush-hush" {
		t.Errorf("request %q: %v", ssh.stdins[0], err)
	}

	// A newer VM's answer with a field this release does not know, and a log JSON escapes to six
	// bytes a byte past 8 MiB: the kit is written all the same.
	log := strings.Repeat("\x1b", maxKitLog)
	newer, _ := json.Marshal(map[string]any{"kit": "SEALED\n", "code": "c", "log": log, "expires_at": "2026-12-01T00:00:00Z"})
	in, _, _ = vmKitRun(t, &kitSSH{stdout: string(newer)}, nil)
	newerFile := filepath.Join(t.TempDir(), "newer.kit")
	if err := runVMKit(t, in, nil, "create", "notes-1", "client", "--name", "site-1", "--set", "SITE=x", "-o", newerFile); err != nil || len(newer) <= 6*(1<<20) {
		t.Errorf("a newer, escaped answer (%d bytes): %v", len(newer), err)
	}

	// ssh failing, or an answer that does not read: no kit file, and the loss said.
	big := `{"kit":"` + strings.Repeat("k", maxKitAnswer) + `","code":"c"}`
	for name, s := range map[string]*kitSSH{"ssh fails": {err: exitStatus(255)}, "no kit": {stdout: `{"code":"c"}`}, "not JSON": {stdout: "Welcome!\n"}, "too big": {stdout: big}} {
		in, out, _ := vmKitRun(t, s, nil)
		failed := filepath.Join(t.TempDir(), "failed.kit")
		err := runVMKit(t, in, nil, "create", "notes-1", "client", "--name", "site-1", "--set", "SITE=x", "-o", failed)
		if err == nil || !strings.Contains(err.Error(), "what it issued is lost") && !strings.Contains(err.Error(), "what the handoff issued is lost") || out.Len() != 0 {
			t.Errorf("%s: %v %q", name, err, out)
		}
		if name == "too big" && !strings.Contains(err.Error(), fmt.Sprintf("more than %d bytes", maxKitAnswer)) {
			t.Errorf("too big: %v", err)
		}
		if _, err := os.Stat(failed); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the file is left: %v", name, err)
		}
	}
}

// Without --known-hosts-line every kit verb is refused before any connection, naming where the
// line comes from.
func TestVMKitNeedsTheHostKey(t *testing.T) {
	t.Parallel()
	ssh := &kitSSH{}
	in, _, _ := vmKitRun(t, ssh, nil)
	for _, args := range [][]string{
		{"create", "notes-1", "client", "--name", "site-1", "--set", "SITE=x", "-o", filepath.Join(t.TempDir(), "k")},
		{"check", "notes-1", "client", "site-1"},
		{"take", "notes-1", "-", "--no-input"},
	} {
		err := runVMKitBare(t, in, strings.NewReader("x"), args...)
		if args[0] == "check" { // its status is the answer: unknown, the refusal said on stderr
			if err == nil || len(ssh.calls) != 0 || ssh.prechecks != 0 {
				t.Errorf("check: %v, %d calls", err, len(ssh.calls))
			}
			continue
		}
		if !isRefused(err) || !strings.Contains(err.Error(), "the VM's host key is not known") || !strings.Contains(refusedFix(err), "vm seed --json`'s known_hosts_line") || len(ssh.calls) != 0 || ssh.prechecks != 0 {
			t.Errorf("%s: %v, %d calls", args[0], err, len(ssh.calls))
		}
	}
	if err := runVMKit(t, in, nil, "create", "notes-1", "client", "--name", "site-1", "--set", "SITE=x", "--host", "-oProxyCommand=x"); err == nil || len(ssh.calls) != 0 {
		t.Errorf("an option as the host: %v", err)
	}
	// Refused before anything is asked: a VM it cannot reach costs no answer.
	asked := &askCounter{}
	k := in.vmKitCommand(&in.O, func() error { return nil }, func(bool) Prompter { return asked })
	k.SetArgs([]string{"create", "notes-1", "client", "--name", "site-1"})
	k.SetOut(io.Discard)
	k.SetErr(io.Discard)
	if err := k.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "host key is not known") || asked.n != 0 {
		t.Errorf("%v, asked %d times", err, asked.n)
	}
	// vm kit take tries the link before it asks the kit's code.
	asked = &askCounter{}
	takeRefused := &kitSSH{precheckErr: exitStatus(255)}
	in4, _, _ := vmKitRun(t, takeRefused, nil)
	if err := in4.TakeKitVM(context.Background(), &VMKitTake{VMReach: VMReach{Host: "notes-1", KnownHostsLine: testKnownLine}, Kit: "-"}, strings.NewReader("x"), asked, time.Now()); err == nil || !strings.Contains(err.Error(), "does not answer over SSH") || asked.n != 0 || len(takeRefused.calls) != 0 {
		t.Errorf("take: %v, asked %d", err, asked.n)
	}
	// ssh refusing the link (a key it does not hold for the address, a VM away): refused by the
	// pre-check, before anything is asked or any file made, the handoff never run.
	refusing := &kitSSH{precheckErr: exitStatus(255)}
	in3, _, _ := vmKitRun(t, refusing, nil)
	asked = &askCounter{}
	o := in3.O
	fresh := &Installer{R: in3.R}
	c := fresh.vmKitCommand(&o, func() error { fresh.O = o; return nil }, func(bool) Prompter { return asked })
	file := filepath.Join(t.TempDir(), "k")
	c.SetArgs([]string{"create", "notes-1", "client", "--name", "site-1", "-o", file, "--known-hosts-line", testKnownLine})
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	if err := c.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "does not answer over SSH") || asked.n != 0 || len(refusing.calls) != 0 || refusing.prechecks != 1 {
		t.Errorf("%v, asked %d times, %d runs", err, asked.n, len(refusing.calls))
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a file is left: %v", err)
	}
}

// vm kit check says the VM's verdict, a status counting only with the verdict line the VM's check
// prints: sudo or ssh failing (1 or 255 without it) is no answer, never not proven.
func TestVMKitCheck(t *testing.T) {
	t.Parallel()
	const proven, notProven = "client site-1: proven\n", "client site-1: not proven (its check, or starting it, exited 1)\n"
	for _, c := range []struct {
		err  error
		said string
		want string
		exit error
	}{
		{nil, proven, proven, nil},
		{exitStatus(1), notProven, notProven, ExitCode(1)},
		{exitStatus(1), "client site-1: not proven (its check, or starting it, exited 3)\n", "client site-1: not proven (its check, or starting it, exited 3)\n", ExitCode(1)},
		{exitStatus(1), "client site-1: not proven (its check, or starting it, exited -1)\n", notProven, ExitCode(1)},
		{exitStatus(1), "", "client site-1: unknown\n", ExitCode(KitUnknown)},
		{exitStatus(2), "client site-1: unknown\n", "client site-1: unknown\n", ExitCode(KitUnknown)},
		{exitStatus(2), "", "client site-1: unknown\n", ExitCode(KitUnknown)},
		{exitStatus(255), "", "client site-1: unknown\n", ExitCode(KitUnknown)},
		{exitStatus(3), notProven, "client site-1: unknown\n", ExitCode(KitUnknown)},
		{nil, "", "client site-1: unknown\n", ExitCode(KitUnknown)},
		{nil, "other site-1: proven\n", "client site-1: unknown\n", ExitCode(KitUnknown)},
		{errors.New("ssh: not found"), "", "client site-1: unknown\n", ExitCode(KitUnknown)},
	} {
		ssh := &kitSSH{err: c.err, stdout: c.said}
		in, out, _ := vmKitRun(t, ssh, nil)
		err := runVMKit(t, in, nil, "check", "notes-1", "client", "site-1")
		if !errors.Is(err, c.exit) && err != c.exit || out.String() != c.want {
			t.Errorf("%v %q: %v %q", c.err, c.said, err, out)
		}
		if a := ssh.calls[0]; !slices.Equal(a[len(a)-6:], []string{"sudo", "notes-stow", "kit", "check", "client", "site-1"}) {
			t.Errorf("ssh ran %q", a)
		}
	}
}

// vm kit take opens the kit here and gives what it holds to the VM's install on standard input:
// the code never reaches the VM.
func TestVMKitTake(t *testing.T) {
	t.Parallel()
	ssh := &kitSSH{}
	in, _, _ := vmKitRun(t, ssh, map[string]string{KitCodeVar: "the-code"})
	in.R.Spec.Handoffs[0].For = "notes"
	sealed, err := kit.Seal(&kit.Kit{Format: kit.Format, For: "notes", Name: "client", Instance: "site-1", Expires: time.Now().Add(time.Hour), Inputs: map[string]string{"TOKEN": "t0k3n"}}, "the-code", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := runVMKit(t, in, bytes.NewReader(sealed), "take", "notes-1", "-", "--no-input"); err != nil {
		t.Fatal(err)
	}
	c := ssh.calls[0]
	if len(ssh.calls) != 1 || !slices.Equal(c[len(c)-6:], []string{"sudo", "notes-stow", "install", "--given", "-", "--no-input"}) ||
		slices.ContainsFunc(c, func(a string) bool { return strings.Contains(a, "the-code") }) {
		t.Fatalf("ssh ran %q", c)
	}
	g, err := parseGiven([]byte(ssh.stdins[0]), "notes")
	if err != nil || g.Inputs["TOKEN"] != "t0k3n" || strings.Contains(ssh.stdins[0], "the-code") {
		t.Errorf("given %q: %v", ssh.stdins[0], err)
	}
	// A wrong code: nothing reaches the VM.
	ssh.calls = nil
	in2, _, _ := vmKitRun(t, ssh, map[string]string{KitCodeVar: "another"})
	if err := runVMKit(t, in2, bytes.NewReader(sealed), "take", "notes-1", "-", "--no-input"); err == nil || len(ssh.calls) != 0 {
		t.Errorf("a wrong code: %v, %d calls", err, len(ssh.calls))
	}
}

// A failed vm kit take names the ssh command over the same strict link that tells where the
// VM's install stopped: no BatchMode, every other option the run's.
func TestVMKitTakeFailureNamesTheLink(t *testing.T) {
	t.Parallel()
	ssh := &kitSSH{err: exitStatus(1)}
	in, _, _ := vmKitRun(t, ssh, map[string]string{KitCodeVar: "the-code"})
	in.R.Spec.Handoffs[0].For = "notes"
	sealed, err := kit.Seal(&kit.Kit{Format: kit.Format, For: "notes", Expires: time.Now().Add(time.Hour), Inputs: map[string]string{"TOKEN": "t"}}, "the-code", 10)
	if err != nil {
		t.Fatal(err)
	}
	err = runVMKit(t, in, bytes.NewReader(sealed), "take", "notes-1", "-", "--no-input", "--user", "ops")
	if err == nil || !strings.Contains(err.Error(), "`ssh -o HostKeyAlias=notes-1 -o ConnectTimeout=10 -o StrictHostKeyChecking=yes -o ") ||
		!strings.Contains(err.Error(), " -o GlobalKnownHostsFile=/dev/null -o HashKnownHosts=no ops@notes-1 -- sudo notes-stow status` tells where it stopped") ||
		strings.Contains(err.Error(), "BatchMode") || strings.Contains(err.Error(), "-o -o") ||
		!strings.HasSuffix(err.Error(), "and the same `vm kit take` command, with the same flags, gives the kit again") {
		t.Errorf("err = %v", err)
	}
}

// kit create --json, on every shape but `vm kit`, reads its request strictly from --request and
// answers the kit as JSON, no file.
func TestKitCreateJSON(t *testing.T) {
	t.Parallel()
	f := &fakeVerbs{issued: &IssuedKit{Kit: "SEALED\n", Code: "c", Log: "ran\n"}}
	var host *string
	rel := kitRelease()
	run := func(stdin string, args ...string) (string, error) {
		var out bytes.Buffer
		v := f.verbs()
		v.host = host
		c := kitCommand(rel, &out, io.Discard, func(bool) Prompter { return nil }, v)
		c.SetArgs(args)
		c.SetIn(strings.NewReader(stdin))
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		err := c.ExecuteContext(context.Background())
		return out.String(), err
	}
	out, err := run(`{"instance":"site-1","answers":{"SITE":"x"},"expires":3600000000000}`, "create", "client", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got IssuedKit
	if err := json.Unmarshal([]byte(out), &got); err != nil || got != *f.issued {
		t.Errorf("out %q: %v", out, err)
	}
	if r := f.reqs[0]; r.Handoff != "client" || r.Instance != "site-1" || r.Answers["SITE"] != "x" || r.Expires != time.Hour {
		t.Errorf("request %+v", r)
	}
	for stdin, want := range map[string]string{
		`{"instance":"s","extra":1}`:                                            "unknown field",
		`{"instance":"s","answers":{"SITE":"x"}}{"instance":"t"}`:               "more than one object",
		`{"handoff":"other","instance":"s"}`:                                    `--request: the request names the handoff "other"`,
		strings.Repeat(" ", maxKitRequest+1) + "{}":                             "--request: the request is over 1048576 bytes",
		`{"instance":"s"}`:                                                      "--request: the handoff client asks SITE (The site): answers.SITE gives it",
		`{"instance":"s","answers":{"SITE":"x"},"expires":-1}`:                  "--request: expires: a kit expires after now",
		`{"instance":"s","answers":{"SITE":"x"},"expires":3153600000000000001}`: "--request: expires: at most 36500d",
	} {
		f.reqs = nil
		if _, err := run(stdin, "create", "client", "--json"); err == nil || !strings.Contains(err.Error(), want) || len(f.reqs) != 0 {
			t.Errorf("%.40q: %v", stdin, err)
		}
	}
	for _, flag := range [][]string{{"--name", "s"}, {"--expires", "1d"}, {"-o", "x.kit"}, {"--set", "SITE=x"}, {"--answers", "a.yaml"}} {
		f.reqs = nil
		if _, err := run(`{"instance":"s","answers":{"SITE":"x"}}`, append([]string{"create", "client", "--json"}, flag...)...); err == nil || !strings.Contains(err.Error(), " with it is refused") || len(f.reqs) != 0 {
			t.Errorf("%s with --json: %v", flag[0], err)
		}
	}
	// log is always there, "" where the run wrote it on standard error (Docker, Kubernetes).
	g := &fakeVerbs{issued: &IssuedKit{Kit: "SEALED\n", Code: "c"}}
	var raw bytes.Buffer
	c := kitCommand(kitRelease(), &raw, io.Discard, func(bool) Prompter { return nil }, g.verbs())
	c.SetArgs([]string{"create", "client", "--json"})
	c.SetIn(strings.NewReader(`{"instance":"s","answers":{"SITE":"x"}}`))
	if err := c.ExecuteContext(context.Background()); err != nil || !strings.Contains(raw.String(), `"log":""`) {
		t.Errorf("no log: %v %s", err, raw.String())
	}
	// --json never asks: --no-input and --accessible with it change nothing, and are taken.
	f.reqs = nil
	if _, err := run(`{"instance":"s","answers":{"SITE":"x"}}`, "create", "client", "--json", "--no-input", "--accessible"); err != nil || len(f.reqs) != 1 {
		t.Errorf("--no-input with --json: %v", err)
	}
	// An instance left out is the handoff's name, as without --json; a handoff asking one refuses it.
	f.reqs = nil
	if _, err := run(`{"answers":{"SITE":"x"}}`, "create", "client", "--json"); err != nil || len(f.reqs) != 1 || f.reqs[0].Instance != "client" {
		t.Errorf("no instance: %v %+v", err, f.reqs)
	}
	rel.Spec.Handoffs[0].Instance = "The site"
	f.reqs = nil
	if _, err := run(`{"answers":{"SITE":"x"}}`, "create", "client", "--json"); err == nil || !strings.Contains(err.Error(), `--request: the handoff client asks "The site": the request's instance gives it`) || len(f.reqs) != 0 {
		t.Errorf("no instance, one asked: %v", err)
	}
	rel.Spec.Handoffs[0].Instance = ""
	// The other side of each bound: a request of exactly 1 MiB, good for exactly 36500 days.
	f.reqs = nil
	at := `{"instance":"s","answers":{"SITE":"x"},"expires":3153600000000000000}`
	if _, err := run(at+strings.Repeat(" ", maxKitRequest-len(at)), "create", "client", "--json"); err != nil || len(f.reqs) != 1 || f.reqs[0].Expires != maxKitExpiry {
		t.Errorf("at the bounds: %v", err)
	}
	f.reqs = nil
	if _, err := run(`{}`, "create", "nope", "--json"); err == nil || !strings.Contains(err.Error(), `declares no handoff "nope" (handoffs: client)`) || len(f.reqs) != 0 {
		t.Errorf("no handoff: %v", err)
	}
	// --request names a file, standard input left unread.
	req := filepath.Join(t.TempDir(), "req.json")
	if err := os.WriteFile(req, []byte(`{"instance":"from-file","answers":{"SITE":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f.reqs = nil
	if _, err := run("not json", "create", "client", "--json", "--request", req); err != nil || len(f.reqs) != 1 || f.reqs[0].Instance != "from-file" {
		t.Errorf("--request FILE: %v %+v", err, f.reqs)
	}
	f.reqs = nil
	if _, err := run("", "create", "client", "--json", "--request", req+".gone"); err == nil || !strings.HasPrefix(err.Error(), "--request: ") || len(f.reqs) != 0 {
		t.Errorf("--request missing: %v", err)
	}
	f.reqs = nil
	if _, err := run("", "create", "client", "--request", req, "--name", "s"); err == nil || !strings.Contains(err.Error(), "give --json with it") || len(f.reqs) != 0 {
		t.Errorf("--request without --json: %v", err)
	}
	// `vm kit create` writes its kit on the operator's machine: no --json.
	host = new(string)
	if _, err := run("", "create", "client", "--json"); err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("--json on vm kit: %v", err)
	}
}

// askCounter is a terminal counting what it is asked, answering nothing.
type askCounter struct{ n int }

func (a *askCounter) Ask(context.Context, Form) (map[string]string, error) {
	a.n++
	return nil, errors.New("asked")
}
