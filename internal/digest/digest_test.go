package digest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func put(t *testing.T, root, rel, content string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func tree(t *testing.T, root string, paths []string, ig *Ignore) string {
	t.Helper()
	b := New("test")
	if err := b.Tree(root, paths, ig); err != nil {
		t.Fatal(err)
	}
	return b.Sum()
}

func TestTreeIsContentAddressed(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	// Same files written in a different order.
	put(t, a, "src/x.go", "x", 0o644)
	put(t, a, "src/y.go", "y", 0o644)
	put(t, b, "src/y.go", "y", 0o644)
	put(t, b, "src/x.go", "x", 0o644)
	if tree(t, a, []string{"src"}, nil) != tree(t, b, []string{"src"}, nil) {
		t.Fatal("digest depends on creation order")
	}
	base := tree(t, a, []string{"src"}, nil)
	put(t, a, "src/x.go", "x2", 0o644)
	if tree(t, a, []string{"src"}, nil) == base {
		t.Error("content change not seen")
	}
	put(t, a, "src/x.go", "x", 0o755)
	if tree(t, a, []string{"src"}, nil) == base {
		t.Error("executable bit not seen")
	}
	put(t, a, "src/x.go", "x", 0o644)
	if tree(t, a, []string{"src"}, nil) != base {
		t.Error("restoring the file did not restore the digest")
	}
	if err := os.Rename(filepath.Join(a, "src/y.go"), filepath.Join(a, "src/z.go")); err != nil {
		t.Fatal(err)
	}
	if tree(t, a, []string{"src"}, nil) == base {
		t.Error("rename not seen")
	}
}

func TestTreeSkipsGitAndCountsOverlapOnce(t *testing.T) {
	root := t.TempDir()
	put(t, root, "a/f", "f", 0o644)
	base := tree(t, root, []string{"a"}, nil)
	put(t, root, "a/.git/HEAD", "ref", 0o644)
	if tree(t, root, []string{"a"}, nil) != base {
		t.Error(".git was read")
	}
	// A linked worktree or submodule holds a .git FILE naming an absolute gitdir.
	put(t, root, "a/sub/.git", "gitdir: /somewhere/else", 0o644)
	put(t, root, "a/sub/x", "x", 0o644)
	withSub := tree(t, root, []string{"a"}, nil)
	put(t, root, "a/sub/.git", "gitdir: /another/place", 0o644)
	if tree(t, root, []string{"a"}, nil) != withSub {
		t.Error("a .git file was read")
	}
	if tree(t, root, []string{"a", "a/sub/x"}, nil) != tree(t, root, []string{"a"}, nil) {
		t.Error("a file named twice was hashed twice")
	}
}

func TestTreeRefusesPathsOutsideTheProject(t *testing.T) {
	root := t.TempDir()
	put(t, root, "a/f", "f", 0o644)
	for _, p := range []string{"nope", filepath.Join(root, "a"), "../x", "a/../../x", ""} {
		if err := New("x").Tree(root, []string{p}, nil); err == nil {
			t.Errorf("input %q accepted", p)
		}
	}
	for _, p := range []string{"a", "./a", "a/../a", "."} {
		if err := New("x").Tree(root, []string{p}, nil); err != nil {
			t.Errorf("input %q refused: %v", p, err)
		}
	}
}

func TestDockerIgnore(t *testing.T) {
	root := t.TempDir()
	put(t, root, "ctx/Dockerfile", "FROM scratch", 0o644)
	put(t, root, "ctx/app.go", "a", 0o644)
	put(t, root, "ctx/node_modules/big.js", "b", 0o644)
	put(t, root, "ctx/keep.log", "k", 0o644)
	put(t, root, "ctx/.dockerignore", "node_modules\n*.log\n!keep.log\n", 0o644)
	put(t, root, "Cargo.lock", "lock", 0o644)
	ig, err := DockerIgnore(filepath.Join(root, "ctx"), filepath.Join(root, "ctx/Dockerfile"))
	if err != nil || ig == nil {
		t.Fatalf("DockerIgnore = %v, %v", ig, err)
	}
	base := tree(t, root, []string{"ctx", "Cargo.lock"}, ig)
	put(t, root, "ctx/node_modules/big.js", "changed", 0o644)
	if tree(t, root, []string{"ctx", "Cargo.lock"}, ig) != base {
		t.Error("an ignored file changed the digest")
	}
	put(t, root, "ctx/keep.log", "changed", 0o644)
	if tree(t, root, []string{"ctx", "Cargo.lock"}, ig) == base {
		t.Error("a re-included file was ignored")
	}
	// A declared input outside the context is never ignored by the context's rules.
	before := tree(t, root, []string{"ctx", "Cargo.lock"}, ig)
	put(t, root, "Cargo.lock", "lock2", 0o644)
	if tree(t, root, []string{"ctx", "Cargo.lock"}, ig) == before {
		t.Error("input outside the context ignored")
	}

	// <Dockerfile>.dockerignore wins over the context's file.
	put(t, root, "ctx/Dockerfile.dockerignore", "app.go\n", 0o644)
	ig2, _ := DockerIgnore(filepath.Join(root, "ctx"), filepath.Join(root, "ctx/Dockerfile"))
	b1 := tree(t, root, []string{"ctx"}, ig2)
	put(t, root, "ctx/app.go", "changed", 0o644)
	if tree(t, root, []string{"ctx"}, ig2) != b1 {
		t.Error("Dockerfile-specific ignore file not applied")
	}
	if ig, err := DockerIgnore(t.TempDir(), "Dockerfile"); err != nil || ig == nil || ig.pm != nil {
		t.Errorf("no ignore file: %v, %v", ig, err)
	}
}

func TestFieldsAreFramed(t *testing.T) {
	a := New("d")
	a.Field("ab", "c")
	b := New("d")
	b.Field("a", "bc")
	if a.Sum() == b.Sum() {
		t.Fatal("label/value boundary is ambiguous")
	}
	if New("x").Sum() == New("y").Sum() {
		t.Fatal("domain not hashed")
	}
}

func TestGitInsideTheBuildContext(t *testing.T) {
	root := t.TempDir()
	put(t, root, "Dockerfile", "FROM scratch", 0o644)
	put(t, root, "app.go", "a", 0o644)
	put(t, root, ".git/HEAD", "ref", 0o644)
	ig, err := DockerIgnore(root, filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if err := New("x").Tree(root, []string{"."}, ig); !errors.Is(err, ErrGitInContext) {
		t.Fatalf("Tree = %v, want ErrGitInContext", err)
	}
	if g, _ := GitInContext(ig); g == "" {
		t.Error("GitInContext found nothing")
	}
	// Positive control: excluded, it is accepted and never read.
	put(t, root, ".dockerignore", ".git\n", 0o644)
	ig, _ = DockerIgnore(root, filepath.Join(root, "Dockerfile"))
	before := tree(t, root, []string{"."}, ig)
	put(t, root, ".git/HEAD", "other", 0o644)
	if tree(t, root, []string{"."}, ig) != before {
		t.Error("an excluded .git was read")
	}
	if g, err := GitInContext(ig); g != "" || err != nil {
		t.Errorf("GitInContext = %q, %v", g, err)
	}
	// Outside the context (an input declared next to it) .git is skipped without a rule.
	if err := New("x").Tree(root, []string{"."}, nil); err != nil {
		t.Errorf("no context: %v", err)
	}
}

// The suggested pattern covers a submodule's .git below the context as well as the top level.
func TestSuggestedGitPatternCoversSubmodules(t *testing.T) {
	root := t.TempDir()
	put(t, root, "Dockerfile", "FROM scratch", 0o644)
	put(t, root, ".git/HEAD", "ref", 0o644)
	put(t, root, "vendor/lib/.git", "gitdir: ../../.git/modules/lib", 0o644)
	put(t, root, ".dockerignore", ".git\n", 0o644)
	ig, _ := DockerIgnore(root, filepath.Join(root, "Dockerfile"))
	if g, _ := GitInContext(ig); !strings.HasSuffix(g, "vendor/lib/.git") {
		t.Errorf("top-level pattern: GitInContext = %q, want the submodule", g)
	}
	put(t, root, ".dockerignore", "**/.git\n", 0o644)
	ig, _ = DockerIgnore(root, filepath.Join(root, "Dockerfile"))
	if g, err := GitInContext(ig); g != "" || err != nil {
		t.Errorf("**/.git: GitInContext = %q, %v", g, err)
	}
}
