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
	t.Parallel()
	dir := t.TempDir()
	testutil.Write(t, dir, "ops/stowage.yaml", "name: notes\n")
	for link, target := range map[string]string{"deploy": "ops", "stowage.yaml": "deploy/stowage.yaml", "out.yaml": "../outside.yaml", "loop.yaml": "loop.yaml"} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	testutil.GitRepo(t, dir)
	if b, ok, err := gitcmd.FileAt(dir, "HEAD", "stowage.yaml"); err != nil || !ok || string(b) != "name: notes\n" {
		t.Errorf("through two links: %q %v %v", b, ok, err)
	}
	for _, p := range []string{"out.yaml", "loop.yaml"} {
		if _, _, err := gitcmd.FileAt(dir, "HEAD", p); err == nil || !strings.Contains(err.Error(), "leaves the repository or loops") {
			t.Errorf("%s: %v", p, err)
		}
	}
	// Nothing there: no file, a path through a file, a name with a space, no such revision.
	for _, c := range [][2]string{{"HEAD", "none.yaml"}, {"HEAD", "ops/stowage.yaml/x"}, {"HEAD", "my blob"}, {"HEAD~1", "stowage.yaml"}} {
		if _, ok, err := gitcmd.FileAt(dir, c[0], c[1]); err != nil || ok {
			t.Errorf("%s:%s: %v %v", c[0], c[1], ok, err)
		}
	}
}
