package gitcmd_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/gitcmd"
	"github.com/norvexlabs/stowage/internal/testutil"
)

func TestMain(m *testing.M) {
	cleanup := testutil.IsolateGit()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func TestEnv(t *testing.T) {
	t.Parallel()
	got := gitcmd.Env([]string{"GIT_DIR=/r/.git", "GIT_DIRX=kept", "GIT_INDEX_FILE=.git/index", "GIT_WORK_TREE=/r", "GIT_PREFIX=sub/", "HOME=/h", "GIT_AUTHOR_NAME=a"})
	if strings.Join(got, " ") != "GIT_DIRX=kept HOME=/h GIT_AUTHOR_NAME=a" {
		t.Fatalf("Env = %v", got)
	}
}

func TestRunVerifyResolve(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.Write(t, dir, "f.txt", "x")
	testutil.GitRepo(t, dir)
	head := strings.TrimSpace(testutil.Git(t, dir, "rev-parse", "HEAD"))
	if out, err := gitcmd.Run(dir, "rev-parse", "HEAD"); err != nil || out != head {
		t.Fatalf("Run = %q, %v", out, err)
	}
	// Any failure of Run is an error, exit 1 included (git diff --exit-code with a change).
	testutil.Write(t, dir, "f.txt", "y")
	if _, err := gitcmd.Run(dir, "diff", "--quiet"); err == nil || !strings.Contains(err.Error(), "git diff --quiet: exit status 1") {
		t.Fatalf("Run exit 1: %v", err)
	}
	for _, c := range []struct {
		rev  string
		want bool
	}{{"HEAD", true}, {"HEAD:f.txt", true}, {"HEAD:nope", false}, {"HEAD~1", false}, {"nope", false}} {
		if ok, err := gitcmd.Verify(dir, c.rev); err != nil || ok != c.want {
			t.Errorf("Verify(%s) = %v, %v", c.rev, ok, err)
		}
	}
	// A revision that starts like an option is a revision, not an option.
	if ok, err := gitcmd.Verify(dir, "--git-dir"); err != nil || ok {
		t.Errorf("Verify(--git-dir) = %v, %v", ok, err)
	}
	if id, ok, err := gitcmd.Resolve(dir, "HEAD^{commit}"); err != nil || !ok || id != head {
		t.Errorf("Resolve = %s %v %v", id, ok, err)
	}
	// Outside a repository: an error, not "no object".
	if _, err := gitcmd.Verify(t.TempDir(), "HEAD"); err == nil {
		t.Error("Verify outside a repository gave no error")
	}
}

// A committed file is read as a checkout reads it: through a link at any component, inside the
// repository; a link out of it is refused, and a path or a revision that is not there is none.
func TestFileAt(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, v string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil { // a directory
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("ops/stowage.yaml", "name: notes\n")
	for link, target := range map[string]string{"deploy": "ops", "stowage.yaml": "deploy/stowage.yaml", "out.yaml": "../outside.yaml"} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@example.org", "commit", "-qm", "one"}} {
		if _, err := gitcmd.Run(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	if b, ok, err := gitcmd.FileAt(dir, "HEAD", "stowage.yaml"); err != nil || !ok || string(b) != "name: notes\n" {
		t.Errorf("through two links: %q %v %v", b, ok, err)
	}
	if _, _, err := gitcmd.FileAt(dir, "HEAD", "out.yaml"); err == nil || !strings.Contains(err.Error(), "leaves the repository") {
		t.Errorf("a link out: %v", err)
	}
	if _, ok, err := gitcmd.FileAt(dir, "HEAD", "none.yaml"); err != nil || ok {
		t.Errorf("no such file: %v %v", ok, err)
	}
	if _, ok, err := gitcmd.FileAt(dir, "HEAD~1", "stowage.yaml"); err != nil || ok {
		t.Errorf("no such revision: %v %v", ok, err)
	}
}
