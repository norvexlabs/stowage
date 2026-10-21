package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/norvexlabs/stowage/internal/hook"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/publish"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/testutil"
)

type launched struct {
	exe     string
	args    []string
	logPath string
}

// runHook runs stowage with a stand-in executable path and launcher, recording what it launches.
func runHook(t *testing.T, b *randomBuilder, env map[string]string, args ...string) (string, string, []launched, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	var l []launched
	o := &options{buildVersion: "0.1.0", getenv: func(k string) string { return env[k] }, builder: b, installer: []byte("exe"),
		now: func() time.Time { return time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC) }, out: &out, err: &errOut,
		executable: func() (string, error) { return "/usr/local/bin/stowage", nil },
		launch: func(exe string, args []string, logPath string) error {
			l = append(l, launched{exe, args, logPath})
			return nil
		}}
	cmd := newRoot(o)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errOut.String(), l, err
}

func TestHookPostCommit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := filepath.Join(dir, "proj")
	if err := os.Rename(testutil.CopyExample(t, "minimal"), root); err != nil {
		t.Fatal(err)
	}
	testutil.GitRepo(t, dir)
	head := strings.TrimSpace(testutil.Git(t, dir, "rev-parse", "HEAD"))
	out, _, l, err := runHook(t, nil, nil, "-C", root, "hook", "post-commit")
	wantLog := filepath.Join(dir, ".git", "stowage", "log", "notes-1.0.0.log")
	if err != nil || len(l) != 1 || l[0].exe != "/usr/local/bin/stowage" || l[0].logPath != wantLog ||
		strings.Join(l[0].args, " ") != "-C "+root+" hook post-commit --channel stable --ship 1.0.0 --commit "+head {
		t.Fatalf("launch: %v %+v", err, l)
	}
	if want := "stowage hook: release 1.0.0: ship to channel stable started in the background\n  follow: tail -f " + wantLog + "\n"; out != want {
		t.Fatalf("output:\n%s", out)
	}
	// The disks: by the hook's flag, or by the commit's environment.
	for _, c := range []struct {
		env  map[string]string
		args []string
	}{{nil, []string{"--disks"}}, {map[string]string{"STOWAGE_SHIP_DISKS": "1"}, nil}, {map[string]string{"STOWAGE_SHIP_DISKS": "TRUE"}, nil}} {
		out, _, l, err := runHook(t, nil, c.env, append([]string{"-C", root, "hook", "post-commit"}, c.args...)...)
		if err != nil || len(l) != 1 || !strings.HasSuffix(strings.Join(l[0].args, " "), "--commit "+head+" --disks") ||
			!strings.HasPrefix(out, "stowage hook: release 1.0.0, with its disks,: ship") {
			t.Fatalf("disks %v %v: %v %+v %q", c.env, c.args, err, l, out)
		}
	}
	if _, _, l, _ := runHook(t, nil, map[string]string{"STOWAGE_SHIP_DISKS": "0"}, "-C", root, "hook", "post-commit"); len(l) != 1 || slices.Contains(l[0].args, "--disks") {
		t.Fatalf("STOWAGE_SHIP_DISKS=0: %+v", l)
	}
	spec := filepath.Join(root, "stowage.yaml")
	if _, _, l, _ := runHook(t, nil, nil, "-C", root, "-f", spec, "hook", "post-commit", "--channel", "beta"); len(l) != 1 ||
		strings.Join(l[0].args, " ") != "-C "+root+" -f "+spec+" hook post-commit --channel beta --ship 1.0.0 --commit "+head {
		t.Fatalf("launch with -f and --channel: %+v", l)
	}
	testutil.Git(t, dir, "commit", "-q", "--allow-empty", "-m", "b")
	if out, _, l, err := runHook(t, nil, nil, "-C", root, "hook", "post-commit"); err != nil || len(l) != 0 || out != "stowage hook: no version bump (1.0.0): not a release\n" {
		t.Fatalf("no bump: %v %+v %q", err, l, out)
	}
	// Every failure is reported and exits 0: a commit never fails on its release.
	for _, c := range []struct{ args, want string }{
		{"--channel nope", `stowage hook: channel "nope" is not declared (beta, stable)`},
		{"", "stowage hook: version at HEAD"},
	} {
		if c.want == "stowage hook: version at HEAD" {
			testutil.Write(t, root, "VERSION", "bad\n")
			testutil.Git(t, dir, "commit", "-qam", "bad")
		}
		args := append([]string{"-C", root, "hook", "post-commit"}, strings.Fields(c.args)...)
		if _, logs, l, err := runHook(t, nil, nil, args...); err != nil || len(l) != 0 || !strings.HasPrefix(logs, c.want) {
			t.Errorf("%s: %v %+v %q", c.want, err, l, logs)
		}
	}
	// A spec outside the repository: a hook ship could not check it out, so none starts.
	outside := filepath.Join(t.TempDir(), "stowage.yaml")
	if b, err := os.ReadFile(spec); err != nil || os.WriteFile(outside, b, 0o644) != nil {
		t.Fatal(err)
	}
	if _, logs, l, err := runHook(t, nil, nil, "-C", root, "-f", outside, "hook", "post-commit"); err != nil || len(l) != 0 || !strings.Contains(logs, "is outside the repository") {
		t.Errorf("spec outside: %v %+v %q", err, l, logs)
	}
	if _, _, _, err := runHook(t, nil, nil, "-C", root, "-f", outside, "hook", "install"); err == nil || !strings.Contains(err.Error(), "is outside the repository") {
		t.Errorf("install with a spec outside: %v", err)
	}
	// A spec the commit does not record is not in the checkout either.
	untracked := filepath.Join(root, "local.yaml")
	if b, err := os.ReadFile(spec); err != nil || os.WriteFile(untracked, b, 0o644) != nil {
		t.Fatal(err)
	}
	if _, logs, l, err := runHook(t, nil, nil, "-C", root, "-f", untracked, "hook", "post-commit"); err != nil || len(l) != 0 || !strings.Contains(logs, "local.yaml is not committed (at HEAD)") {
		t.Errorf("uncommitted spec: %v %+v %q", err, l, logs)
	}
	if _, _, _, err := runHook(t, nil, nil, "-C", root, "-f", untracked, "hook", "install"); err == nil || !strings.Contains(err.Error(), "is not committed") {
		t.Errorf("install with an uncommitted spec: %v", err)
	}
	os.Remove(untracked)
	if _, logs, _, err := runHook(t, nil, nil, "-C", t.TempDir(), "hook", "post-commit"); err != nil || !strings.HasPrefix(logs, "stowage hook: ") {
		t.Errorf("no project: %v %q", err, logs)
	}
}

func TestHookShip(t *testing.T) {
	t.Parallel()
	root := registryProject(t)
	testutil.GitRepo(t, root)
	head := strings.TrimSpace(testutil.Git(t, root, "rev-parse", "HEAD"))
	b := &randomBuilder{}
	env := map[string]string{"NOTES_PUSH_TOKEN": "t0ken"}
	ship := func(args ...string) (string, error) {
		t.Helper()
		out, logs, _, err := runHook(t, b, env, append([]string{"-C", root, "hook", "post-commit"}, args...)...)
		return out + logs, err
	}
	if _, err := ship("--ship", "1.0.0"); err == nil || err.Error() != "--ship and --commit go together" {
		t.Fatalf("--ship alone: %v", err)
	}
	if _, err := ship("--ship", "1.0.1", "--commit", head); err == nil || err.Error() != "the tree is at 1.0.0 now, not 1.0.1: not shipped" || b.n != 0 {
		t.Fatalf("another version: %v (%d builds)", err, b.n)
	}
	// The release is the commit's: built from a checkout of it, whatever the working tree holds
	// and wherever HEAD moved since.
	testutil.Write(t, root, "api/extra.txt", "not committed")
	testutil.Write(t, root, "VERSION", "9.9.9\n")
	testutil.Git(t, root, "add", "api")
	testutil.Git(t, root, "commit", "-q", "-m", "later")
	r, err := hook.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(r.Dir(), "tree", head)
	b.seen = func(p *project.Project) {
		if p.Root != checkout {
			t.Errorf("built from %s, not the checkout %s", p.Root, checkout)
		}
		if _, err := os.Stat(filepath.Join(p.Root, "api", "extra.txt")); !os.IsNotExist(err) {
			t.Errorf("a later file is in the build: %v", err)
		}
	}
	// A second hook ship waits for the one running, then ships.
	unlock, err := hook.Lock(r.LockPath(), func() {})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		out string
		err error
	}
	done := make(chan result)
	go func() {
		out, err := ship("--ship", "1.0.0", "--commit", head, "--channel", "beta")
		done <- result{out, err}
	}()
	select {
	case res := <-done:
		t.Fatalf("shipped while another held the lock: %v\n%s", res.err, res.out)
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	res := <-done
	if res.err != nil || !strings.HasPrefix(res.out, "2026-10-08T00:00:00Z stowage hook: ship 1.0.0 (commit "+head+") to channel beta\nstowage hook: waiting for the hook ship running in this repository\n") ||
		!strings.Contains(res.out, "notes 1.0.0 revision 1 · channel beta") || b.n == 0 {
		t.Fatalf("ship: %v (%d builds)\n%s", res.err, b.n, res.out)
	}
	if _, err := os.Stat(checkout); !os.IsNotExist(err) {
		t.Fatalf("checkout left: %v", err)
	}
	if wt := testutil.Git(t, root, "worktree", "list", "--porcelain"); strings.Count(wt, "worktree ") != 1 {
		t.Fatalf("worktrees left:\n%s", wt)
	}
	if _, err := ship("--ship", "1.0.0", "--commit", "^HEAD"); err == nil || err.Error() != "^HEAD is not a commit of this repository" {
		t.Fatalf("--commit ^HEAD: %v", err)
	}
	// Submodule content would not be in the checkout: refused, naming them.
	lib := t.TempDir()
	testutil.Write(t, lib, "proto.txt", "shared")
	testutil.GitRepo(t, lib)
	testutil.Git(t, root, "submodule", "add", "-q", lib, "api/lib")
	testutil.Git(t, root, "commit", "-qm", "lib")
	withLib := strings.TrimSpace(testutil.Git(t, root, "rev-parse", "HEAD"))
	n := b.n
	if _, err := ship("--ship", "1.0.0", "--commit", withLib); err == nil || err.Error() != "1.0.0 is not shipped: the project holds submodules (api/lib), which a hook ship does not check out; publish with stowage ship" || b.n != n {
		t.Fatalf("submodules: %v", err)
	}
	// A spec outside the repository has no commit to check out.
	outside := filepath.Join(t.TempDir(), "stowage.yaml")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ship("-f", outside, "--ship", "1.0.0", "--commit", head); err == nil || !strings.Contains(err.Error(), "is outside the repository") {
		t.Fatalf("spec outside: %v", err)
	}
}

// A hook in a linked worktree runs with git's GIT_DIR (and no GIT_WORK_TREE): detection must not
// read the repository through it.
func TestHookPostCommitInALinkedWorktree(t *testing.T) {
	dir := t.TempDir()
	if err := os.Rename(testutil.CopyExample(t, "minimal"), filepath.Join(dir, "proj")); err != nil {
		t.Fatal(err)
	}
	testutil.GitRepo(t, dir)
	wt := filepath.Join(t.TempDir(), "wt")
	testutil.Git(t, dir, "worktree", "add", "-q", wt)
	testutil.Write(t, wt, "proj/VERSION", "1.0.1\n")
	testutil.Git(t, wt, "commit", "-qam", "bump")
	t.Setenv("GIT_DIR", filepath.Join(dir, ".git", "worktrees", "wt"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(dir, ".git", "worktrees", "wt", "index"))
	t.Setenv("GIT_PREFIX", "")
	out, logs, l, err := runHook(t, nil, nil, "-C", filepath.Join(wt, "proj"), "hook", "post-commit")
	if err != nil || len(l) != 1 || !strings.Contains(out, "release 1.0.1") {
		t.Fatalf("%v %+v\n%s%s", err, l, out, logs)
	}
}

func TestHookInstall(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := filepath.Join(dir, "sub", "proj")
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(testutil.CopyExample(t, "minimal"), root); err != nil {
		t.Fatal(err)
	}
	testutil.GitRepo(t, dir)
	hookFile := filepath.Join(dir, ".git", "hooks", "post-commit")
	if out, _, _, err := runHook(t, nil, nil, "-C", root, "hook", "install"); err != nil || out != "wrote "+hookFile+"\n" {
		t.Fatalf("install: %v %q", err, out)
	}
	if b, _ := os.ReadFile(hookFile); !strings.Contains(string(b), "\n/usr/local/bin/stowage -C sub/proj hook post-commit\n") {
		t.Fatalf("hook:\n%s", b)
	}
	// core.hooksPath is where git looks, so it is where the hook goes.
	testutil.Git(t, dir, "config", "core.hooksPath", ".githooks")
	out, _, _, err := runHook(t, nil, nil, "-C", root, "-f", filepath.Join(root, "stowage.yaml"), "hook", "install", "--channel", "beta")
	hookFile = filepath.Join(dir, ".githooks", "post-commit")
	if err != nil || out != "wrote "+hookFile+"\n" {
		t.Fatalf("install under core.hooksPath: %v %q", err, out)
	}
	if b, _ := os.ReadFile(hookFile); !strings.Contains(string(b), "\n/usr/local/bin/stowage -C sub/proj -f sub/proj/stowage.yaml hook post-commit --channel beta\n") {
		t.Fatalf("hook:\n%s", b)
	}
	// A project reached through a symbolic link is named by its path inside the repository.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := runHook(t, nil, nil, "-C", filepath.Join(link, "sub", "proj"), "hook", "install"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(hookFile); !strings.Contains(string(b), "\n/usr/local/bin/stowage -C sub/proj hook post-commit\n") {
		t.Fatalf("hook through a link:\n%s", b)
	}
	if _, _, _, err := runHook(t, nil, nil, "-C", root, "hook", "install", "--disks"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(hookFile); !strings.Contains(string(b), "\n/usr/local/bin/stowage -C sub/proj hook post-commit --disks\n") {
		t.Fatalf("hook with --disks:\n%s", b)
	}
	if _, _, _, err := runHook(t, nil, nil, "-C", root, "hook", "install", "--channel", "nope"); err == nil || !strings.Contains(err.Error(), `channel "nope" is not declared`) {
		t.Fatalf("undeclared channel: %v", err)
	}
}

// A hook ship publishes from a checkout, which has no untracked key file: a relative signing key
// is read from the project's own directory.
func TestHookShipSigningKey(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "full")
	testutil.Edit(t, root, "stowage.yaml", "key: env://ATLAS_SIGNING_KEY", "key: keys/release.key")
	p, err := project.Open(root, "")
	if err != nil {
		t.Fatal(err)
	}
	plain := &options{getenv: func(string) string { return "" }}
	at := plain.inCheckout("/git/stowage/tree/c0ffee", "proj", "proj/stowage.yaml", "/work/proj")
	if at.root != "/git/stowage/tree/c0ffee/proj" || at.specPath != "/git/stowage/tree/c0ffee/proj/stowage.yaml" || plain.root != "" {
		t.Fatalf("in the checkout: root %s, spec %s (original root %q)", at.root, at.specPath, plain.root)
	}
	for _, c := range []struct {
		o    *options
		want string
	}{
		{plain, filepath.Join(root, "keys", "release.key")},
		{at, "/work/proj/keys/release.key"},
	} {
		s, err := c.o.signer(p, authn.NewMultiKeychain(), writeAccess, registry.New(authn.NewMultiKeychain()))
		if err != nil {
			t.Fatal(err)
		}
		if k := s.(*publish.CosignSigner).Key; k != c.want {
			t.Errorf("keyRoot %q: key %s, want %s", c.o.keyRoot, k, c.want)
		}
	}
}
