package payload

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var padding = strings.Repeat("# padding\n", 500)

// The script must run its binary the same whether saved and run, or piped into sh.
func TestScriptRunsItsBinarySavedOrPiped(t *testing.T) {
	bin := "#!/bin/sh\ncase \"$0\" in ./.notes-stow.*) ;; *) echo \"extracted to $0\"; exit 1;; esac\necho \"ran $(basename \"$0\") with $*\"\n" + padding
	var script bytes.Buffer
	if err := Script(&script, "notes", "notes-stow", strings.NewReader(bin)); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(script.String(), "\n") {
		if len(line) > 200 {
			t.Fatalf("a %d-character line", len(line))
		}
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "notes.sh")
	if err := os.WriteFile(p, script.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	run := exec.Command("sh", p, "--dir", "x y")
	run.Dir = dir
	saved, err := run.CombinedOutput()
	if err != nil || strings.TrimSpace(string(saved)) != "ran notes-stow with --dir x y" {
		t.Fatalf("saved: %v %q", err, saved)
	}
	cmd := exec.Command("sh", "-s", "--", "--offline")
	cmd.Stdin = bytes.NewReader(script.Bytes())
	cmd.Dir = dir
	piped, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(piped)) != "ran notes-stow with --offline" {
		t.Fatalf("piped: %v %q", err, piped)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".notes-stow.*")); len(left) != 0 {
		t.Fatalf("the extracted binary was left behind: %v", left)
	}
}

func TestScriptRefusesAName(t *testing.T) {
	if err := Script(&bytes.Buffer{}, "x; rm -rf /", "x-stow", strings.NewReader("")); err == nil {
		t.Fatal("accepted")
	}
}

// A working directory the operator cannot write falls back to the temporary directory.
func TestScriptFallsBackWhenTheDirectoryIsReadOnly(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes anywhere")
	}
	tmp, ro := t.TempDir(), t.TempDir()
	bin := "#!/bin/sh\ncase \"$0\" in \"$TMPDIR\"/.notes-stow.*) echo \"ok $(umask)\";; *) echo \"extracted to $0\"; exit 1;; esac\n" + padding
	var script bytes.Buffer
	if err := Script(&script, "notes", "notes-stow", strings.NewReader(bin)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o555); err != nil { // a directory
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) }) // a directory
	cmd := exec.Command("sh", "-c", "umask 022; exec sh -s")
	cmd.Dir, cmd.Stdin = ro, bytes.NewReader(script.Bytes())
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp, "XDG_CACHE_HOME="+ro)
	out, err := cmd.CombinedOutput()
	// The installer runs with the operator's umask: the files it writes keep their modes.
	if err != nil || strings.TrimSpace(string(out)) != "ok 0022" {
		t.Fatalf("%v %q", err, out)
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, ".notes-stow.*")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}

// Run from its file, standard input stays the operator's, whatever the arguments; arriving on standard
// input, the installer gets the terminal. Proven on a terminal, which `script` gives a test.
func TestStandardInputIsTheOperatorsUnlessItIsTheScript(t *testing.T) {
	t.Parallel()
	pty, err := exec.LookPath("script")
	if err != nil {
		t.Skip("no script(1) to give a terminal")
	}
	dir := t.TempDir()
	run := func(cmd string) (string, error) {
		c := exec.Command(pty, "-qec", cmd, "/dev/null")
		c.Dir = dir
		out, err := c.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if got, err := run("[ -t 0 ] && echo terminal"); err != nil || got != "terminal" {
		t.Skipf("script(1) gives no terminal here: %v %q", err, got)
	}
	bin := "#!/bin/sh\nif [ -t 0 ]; then echo terminal; else cat; fi\n"
	var s bytes.Buffer
	if err := Script(&s, "notes", "notes-stow", strings.NewReader(bin)); err != nil {
		t.Fatal(err)
	}
	file, in := filepath.Join(dir, "s.sh"), filepath.Join(dir, "in")
	if err := os.WriteFile(file, s.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(in, []byte("the-pipe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for cmd, want := range map[string]string{
		"sh " + file + " --kit - < " + in:            "the-pipe",
		"sh " + file + " secrets set KEY < " + in:    "the-pipe",
		"cat " + in + " | sh " + file + " --given -": "the-pipe",
		"sh " + file + " --no-input":                 "terminal",
		"cat " + file + " | sh -s":                   "terminal",
		"cat " + file + " | sh -s -- --kit -":        "terminal",
		"sh -s -- --no-input < " + file:              "terminal",
	} {
		got, err := run(cmd)
		if err != nil {
			t.Errorf("%s: %v %q", cmd, err, got)
			continue
		}
		if got != want {
			t.Errorf("%s: %q, want %q", cmd, got, want)
		}
	}
}
