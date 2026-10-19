// Package testutil holds helpers shared by the test suites.
package testutil

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/gitcmd"
)

// RepoRoot is the Stowage repository root: the nearest directory above the test's working
// directory (its package directory) holding go.mod. It does not read source paths from the
// binary, which -trimpath rewrites.
func RepoRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}

// CopyExample copies examples/<name> into a fresh temporary directory and returns it, so a test
// may mutate the project freely.
func CopyExample(t testing.TB, name string) string {
	t.Helper()
	src := filepath.Join(RepoRoot(t), "examples", name)
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// Edit replaces old with new in root/rel exactly once, failing the test when old is not found
// once: a mutation that silently does nothing would make its negative test pass for the wrong
// reason.
func Edit(t testing.TB, root, rel, old, new string) {
	t.Helper()
	p := filepath.Join(root, rel)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), old); n != 1 {
		t.Fatalf("%s: %q occurs %d times, want 1", rel, old, n)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), old, new, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Write creates root/rel with content.
func Write(t testing.TB, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// SetYAML sets the key at path in root/rel to value, rewriting the whole document through its
// parsed form (comments are lost). Every key of the path must exist.
func SetYAML(t testing.TB, root, rel string, path []string, value any) {
	t.Helper()
	p := filepath.Join(root, rel)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	m := doc
	for i, k := range path {
		if _, ok := m[k]; !ok {
			t.Fatalf("%s: no key %s", rel, strings.Join(path[:i+1], "."))
		}
		if i == len(path)-1 {
			m[k] = value
			break
		}
		next, ok := m[k].(map[string]any)
		if !ok {
			t.Fatalf("%s: %s is not a mapping", rel, strings.Join(path[:i+1], "."))
		}
		m = next
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Git runs git in dir with a fixed identity and returns its output; the package's TestMain calls
// IsolateGit. The test skips when git is not installed.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// GitRepo makes dir a git repository with everything in it committed.
func GitRepo(t testing.TB, dir string) {
	t.Helper()
	Git(t, dir, "init", "-q")
	Git(t, dir, "add", "-A")
	Git(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
}

// IsolateGit points git at a global configuration of its own and none of the system's, and drops
// git's per-hook variables, for a package's TestMain: git that the code under test runs then reads
// no hook, setting or repository of the host. The configuration allows file:// submodules, which
// tests use. It returns the cleanup.
func IsolateGit() func() {
	dir, err := os.MkdirTemp("", "stowage-git-")
	if err != nil {
		panic(err)
	}
	cfg := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(cfg, []byte("[protocol \"file\"]\n\tallow = always\n"), 0o644); err != nil {
		panic(err)
	}
	os.Setenv("GIT_CONFIG_GLOBAL", cfg)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// Run from a git hook, the tests would otherwise act on the hook's repository and index.
	for _, k := range gitcmd.HookVars {
		os.Unsetenv(k)
	}
	return func() { os.RemoveAll(dir) }
}
