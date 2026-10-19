package version

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/testutil"
)

func write(t *testing.T, dir, name, content string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestChangelogSourceSkipsUnreleased(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "CHANGELOG.md", "# Changelog\n\n## [Unreleased]\n- wip\n\n## [2026.10.3] - 2026-10-08\n- x\n\n## [2026.10.2]\n", 0o644)
	src, err := NewSource(dir, spec.Provider{})
	if err != nil {
		t.Fatal(err)
	}
	v, err := src.Get()
	if err != nil || v != "2026.10.3" {
		t.Errorf("Get = %q, %v", v, err)
	}
}

func TestFileSource(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "VERSION", "\n  1.4.0  \nignored\n", 0o644)
	src, _ := NewSource(dir, spec.Provider{Name: "file"})
	if v, err := src.Get(); err != nil || v != "1.4.0" {
		t.Errorf("Get = %q, %v", v, err)
	}
	write(t, dir, "EMPTY", "\n\n", 0o644)
	src, _ = NewSource(dir, spec.Provider{Name: "file", Config: map[string]any{"file": "EMPTY"}})
	if _, err := src.Get(); err == nil {
		t.Error("empty file accepted")
	}
}

func TestGitTagSource(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	git("commit", "-q", "--allow-empty", "-m", "a")
	git("tag", "v1.2.0")
	git("commit", "-q", "--allow-empty", "-m", "b")
	src, _ := NewSource(dir, spec.Provider{Name: "git-tag", Config: map[string]any{"strip": "v"}})
	if v, err := src.Get(); err != nil || v != "1.2.0" {
		t.Errorf("Get = %q, %v", v, err)
	}
}

func TestCommandSourceAndScheme(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "ver.sh", `#!/bin/sh
req=$(cat)
case "$req" in
  *version.get*) echo '{"version":"7"}' ;;
  *scheme.compare*) echo '{"result":-1}' ;;
  *'"version":"bad"'*) echo '{"valid":false,"reason":"no"}' ;;
  *scheme.validate*) echo '{"valid":true}' ;;
  *) exit 2 ;;
esac
`, 0o755)
	cmd := map[string]any{"cmd": []any{"./ver.sh"}}
	src, err := NewSource(dir, spec.Provider{Name: "command", Config: cmd})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := src.Get(); err != nil || v != "7" {
		t.Errorf("Get = %q, %v", v, err)
	}
	sch, err := NewScheme(dir, spec.Provider{Name: "command", Config: cmd})
	if err != nil {
		t.Fatal(err)
	}
	if c, err := sch.Compare("1", "2"); err != nil || c != -1 {
		t.Errorf("Compare = %d, %v", c, err)
	}
	if err := sch.Validate("7"); err != nil {
		t.Errorf("Validate: %v", err)
	}
	if err := sch.Validate("bad"); !IsInvalid(err) {
		t.Errorf("Validate(bad) = %v, want an InvalidError", err)
	}
	// The tag is the version itself.
	if tag, err := sch.Tag("7.x"); err != nil || tag != "7.x" {
		t.Errorf("Tag = %q, %v", tag, err)
	}
	if _, err := sch.Tag("7+x"); err == nil {
		t.Error("a version an OCI tag cannot spell was rewritten instead of refused")
	}
}

func TestSourcesAtARevision(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, dir, "sub/CHANGELOG.md", "## [1.0.0]\n", 0o644)
	testutil.GitRepo(t, dir)
	root := filepath.Join(dir, "sub")
	cl, _ := NewSource(root, spec.Provider{})
	file, _ := NewSource(root, spec.Provider{Name: "file"})
	at := func(s Source, rev string) string {
		t.Helper()
		v, err := s.(RevisionSource).At(rev)
		if err != nil {
			t.Fatalf("At(%s): %v", rev, err)
		}
		return v
	}
	// A revision that does not exist and a file the commit lacks: no version.
	if v := at(cl, "HEAD~1"); v != "" {
		t.Errorf("before the first commit: %q", v)
	}
	if v := at(file, "HEAD"); v != "" {
		t.Errorf("VERSION absent: %q", v)
	}
	write(t, root, "CHANGELOG.md", "## [1.1.0]\n", 0o644)
	write(t, root, "VERSION", "2.0.0\n", 0o644)
	testutil.Git(t, dir, "commit", "-qam", "b")
	testutil.Git(t, dir, "add", "-A")
	testutil.Git(t, dir, "commit", "-qm", "c")
	// The working tree moves on: At reads the commits, not the tree.
	write(t, root, "CHANGELOG.md", "## [9.9.9]\n", 0o644)
	for _, c := range []struct {
		s         Source
		rev, want string
	}{
		{cl, "HEAD", "1.1.0"}, {cl, "HEAD~2", "1.0.0"},
		{file, "HEAD", "2.0.0"}, {file, "HEAD~1", ""},
	} {
		if v := at(c.s, c.rev); v != c.want {
			t.Errorf("%T At(%s) = %q, want %q", c.s, c.rev, v, c.want)
		}
	}
	// A file with no version is an error, as Get's.
	write(t, root, "VERSION", "\n", 0o644)
	write(t, root, "CHANGELOG.md", "# no release\n", 0o644)
	testutil.Git(t, dir, "commit", "-qam", "d")
	if _, err := file.(RevisionSource).At("HEAD"); err == nil || !strings.Contains(err.Error(), "HEAD:./VERSION is empty") {
		t.Errorf("empty VERSION at HEAD: %v", err)
	}
	if _, err := cl.(RevisionSource).At("HEAD"); err == nil || !strings.Contains(err.Error(), "no release heading") {
		t.Errorf("no heading at HEAD: %v", err)
	}
	// Outside a repository: an error, not "no version".
	out, _ := NewSource(t.TempDir(), spec.Provider{})
	if _, err := out.(RevisionSource).At("HEAD"); err == nil {
		t.Error("At outside a repository gave no error")
	}
	// A tag comes after its commit, and a command reads no commit: neither reads a revision.
	tag, _ := NewSource(root, spec.Provider{Name: "git-tag"})
	cmd, _ := NewSource(root, spec.Provider{Name: "command", Config: map[string]any{"cmd": []string{"true"}}})
	for _, s := range []Source{tag, cmd} {
		if _, ok := s.(RevisionSource); ok {
			t.Errorf("%T claims to read a revision", s)
		}
	}
}
