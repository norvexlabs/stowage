package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// repo is the minimal example as a git repository's subdirectory, committed at version 1.0.0.
func repo(t *testing.T) (dir, root string) {
	t.Helper()
	dir = t.TempDir()
	root = filepath.Join(dir, "proj")
	if err := os.Rename(testutil.CopyExample(t, "minimal"), root); err != nil {
		t.Fatal(err)
	}
	testutil.GitRepo(t, dir)
	return dir, root
}

func detect(t *testing.T, root string) Decision {
	t.Helper()
	p, err := project.Open(root, "")
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Detect(p, r)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func commitVersion(t *testing.T, dir, root, v string) {
	t.Helper()
	testutil.Write(t, root, "VERSION", v+"\n")
	testutil.Git(t, dir, "commit", "-qam", v)
}

func TestDetect(t *testing.T) {
	t.Parallel()
	dir, root := repo(t)
	// The first commit: HEAD~1 records no version, so the version moved.
	if d := detect(t, root); d.Version != "1.0.0" {
		t.Fatalf("first commit: %+v", d)
	}
	testutil.Write(t, root, "web/note.txt", "x")
	testutil.Git(t, dir, "add", "-A")
	testutil.Git(t, dir, "commit", "-qm", "no bump")
	if d := detect(t, root); d.Version != "" || d.Reason != "no version bump (1.0.0): not a release" {
		t.Fatalf("no bump: %+v", d)
	}
	commitVersion(t, dir, root, "1.0.1")
	// The working tree is not what is read: a later edit does not change the decision.
	testutil.Write(t, root, "VERSION", "1.0.0\n")
	head := strings.TrimSpace(testutil.Git(t, dir, "rev-parse", "HEAD"))
	if d := detect(t, root); d.Version != "1.0.1" || d.Commit != head {
		t.Fatalf("bump: %+v (HEAD %s)", d, head)
	}
	// An amend of the release commit does not start it again; an amend that makes the bump does.
	testutil.Git(t, dir, "commit", "-q", "--amend", "-m", "1.0.1 again")
	if d := detect(t, root); d.Reason != "skip: HEAD amends the commit that started the ship of 1.0.1 (run stowage ship to publish the amended one)" {
		t.Fatalf("amend: %+v", d)
	}
	testutil.Write(t, root, "VERSION", "1.0.2\n")
	testutil.Git(t, dir, "commit", "-q", "--amend", "-am", "1.0.2 instead")
	if d := detect(t, root); d.Version != "1.0.2" {
		t.Fatalf("amend to another version: %+v", d)
	}
	testutil.Write(t, root, "web/note.txt", "y")
	testutil.Git(t, dir, "commit", "-qam", "no bump")
	// Amending a commit that was no release does not claim a ship was started.
	testutil.Git(t, dir, "commit", "-q", "--amend", "-m", "still no bump")
	if d := detect(t, root); d.Reason != "no version bump (1.0.2): not a release" {
		t.Fatalf("amend of a non-release: %+v", d)
	}
	testutil.Write(t, root, "VERSION", "1.0.3\n")
	testutil.Git(t, dir, "commit", "-q", "--amend", "-am", "now a bump")
	if d := detect(t, root); d.Version != "1.0.3" {
		t.Fatalf("amend that makes the bump: %+v", d)
	}
	// With the reflog expired, the amended commit is unknown: the amend is judged on its bump.
	testutil.Git(t, dir, "reflog", "expire", "--expire=now", "--all")
	testutil.Git(t, dir, "commit", "-q", "--amend", "-m", "1.0.3 again")
	if d := detect(t, root); d.Version != "1.0.3" {
		t.Fatalf("amend with no reflog: %+v", d)
	}
	commitVersion(t, dir, root, "1.0.0")
	if d := detect(t, root); d.Version != "" || d.Reason != "version moved back from 1.0.3 to 1.0.0: not a release" {
		t.Fatalf("back: %+v", d)
	}
	commitVersion(t, dir, root, "1.0.10")
	// Ordered by the scheme, not as strings ("1.0.10" < "1.0.9" as text).
	commitVersion(t, dir, root, "1.0.9")
	if d := detect(t, root); d.Version != "" || !strings.Contains(d.Reason, "moved back") {
		t.Fatalf("1.0.10 → 1.0.9: %+v", d)
	}
	// A merge brings in another history's version: its tree is not that release's.
	testutil.Git(t, dir, "checkout", "-q", "-b", "side")
	commitVersion(t, dir, root, "3.0.0")
	testutil.Git(t, dir, "checkout", "-q", "-")
	testutil.Git(t, dir, "merge", "-q", "--no-ff", "--no-edit", "side")
	if d := detect(t, root); d.Reason != "skip: HEAD is a merge commit" {
		t.Fatalf("merge: %+v", d)
	}
	// A cherry-pick brings another history's commit.
	testutil.Git(t, dir, "checkout", "-q", "side")
	commitVersion(t, dir, root, "5.0.0")
	pick := strings.TrimSpace(testutil.Git(t, dir, "rev-parse", "HEAD"))
	testutil.Git(t, dir, "checkout", "-q", "-")
	testutil.Git(t, dir, "cherry-pick", pick)
	if d := detect(t, root); d.Reason != "skip: HEAD is a cherry-pick" {
		t.Fatalf("cherry-pick: %+v", d)
	}
	commitVersion(t, dir, root, "6.0.0")
	gitDir := filepath.Join(dir, ".git")
	for _, m := range markers {
		testutil.Write(t, gitDir, m, "")
		if d := detect(t, root); d.Reason != "skip: "+m+" in progress" {
			t.Errorf("%s: %+v", m, d)
		}
		os.Remove(filepath.Join(gitDir, m))
	}
	if d := detect(t, root); d.Version != "6.0.0" {
		t.Fatalf("after the markers: %+v", d)
	}
}

func TestDetectEdgeCases(t *testing.T) {
	t.Parallel()
	dir, root := repo(t)
	// A previous commit whose version the scheme refuses counts as none.
	commitVersion(t, dir, root, "one")
	commitVersion(t, dir, root, "1.0.0")
	if d := detect(t, root); d.Version != "1.0.0" {
		t.Fatalf("after an invalid version: %+v", d)
	}
	// An invalid version at HEAD is an error, not a release.
	commitVersion(t, dir, root, "two")
	p, _ := project.Open(root, "")
	r, _ := Open(root)
	if _, err := Detect(p, r); err == nil || !strings.Contains(err.Error(), "version at HEAD") {
		t.Fatalf("invalid HEAD version: %v", err)
	}
	// HEAD without the version file.
	testutil.Git(t, dir, "rm", "-q", "proj/VERSION")
	testutil.Git(t, dir, "commit", "-qm", "rm")
	if d := detect(t, root); d.Reason != "skip: HEAD records no version" {
		t.Fatalf("no file: %+v", d)
	}
	// A version source that cannot read a commit.
	testutil.Edit(t, root, "stowage.yaml", "source: { provider: file, file: VERSION }", "source: { provider: command, cmd: [./v.sh] }")
	if d := detect(t, root); d.Reason != "skip: the command version source does not move with a commit: publish with stowage ship" {
		t.Fatalf("command source: %+v", d)
	}
}

func TestOpenOutsideARepository(t *testing.T) {
	t.Parallel()
	if _, err := Open(t.TempDir()); err == nil {
		t.Fatal("no error outside a repository")
	}
}

func TestOpenInALinkedWorktree(t *testing.T) {
	t.Parallel()
	dir, _ := repo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	testutil.Git(t, dir, "worktree", "add", "-q", wt)
	r, err := Open(filepath.Join(wt, "proj"))
	if err != nil {
		t.Fatal(err)
	}
	main, _ := Open(dir)
	// One lock per repository, the markers of the worktree's own git directory.
	if r.LockPath() != main.LockPath() || r.GitDir == main.GitDir || r.Top != wt {
		t.Fatalf("worktree %+v, main %+v", r, main)
	}
}

func TestCheckout(t *testing.T) {
	t.Parallel()
	dir, root := repo(t)
	first := strings.TrimSpace(testutil.Git(t, dir, "rev-parse", "HEAD"))
	commitVersion(t, dir, root, "1.0.1")
	testutil.Write(t, root, "VERSION", "uncommitted\n")
	r, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	top, remove, err := r.Checkout(first)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(top, "proj", "VERSION")); string(b) != "1.0.0\n" || !strings.HasPrefix(top, r.Dir()) {
		t.Fatalf("checkout %s: VERSION %q", top, b)
	}
	// A checkout a killed run left behind is replaced.
	testutil.Write(t, top, "proj/VERSION", "left\n")
	top2, remove2, err := r.Checkout(first)
	if err != nil || top2 != top {
		t.Fatalf("again: %v %s", err, top2)
	}
	if b, _ := os.ReadFile(filepath.Join(top, "proj", "VERSION")); string(b) != "1.0.0\n" {
		t.Fatalf("left checkout kept: %q", b)
	}
	if err := remove2(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(top); !os.IsNotExist(err) {
		t.Fatalf("checkout not removed: %v", err)
	}
	if err := remove(); err != nil {
		t.Fatalf("second remove: %v", err)
	}
	if wt := testutil.Git(t, dir, "worktree", "list", "--porcelain"); strings.Count(wt, "worktree ") != 1 {
		t.Fatalf("worktrees left:\n%s", wt)
	}
	// A checkout deleted by hand leaves git's record of it, which remove clears.
	_, remove3, err := r.Checkout(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(top); err != nil {
		t.Fatal(err)
	}
	if err := remove3(); err != nil {
		t.Fatalf("remove after a manual delete: %v", err)
	}
	if wt := testutil.Git(t, dir, "worktree", "list", "--porcelain"); strings.Count(wt, "worktree ") != 1 {
		t.Fatalf("record left:\n%s", wt)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "VERSION")); string(b) != "uncommitted\n" {
		t.Fatalf("working tree touched: %q", b)
	}
	// A checkout is named by the commit's full id, whatever names it.
	full, err := r.Commit("HEAD")
	if err != nil || full != strings.TrimSpace(testutil.Git(t, dir, "rev-parse", "HEAD")) {
		t.Fatalf("Commit(HEAD) = %s, %v", full, err)
	}
	for _, c := range []string{"no-such-commit", ":/../../../wt", "^HEAD", "HEAD:proj"} {
		if _, err := r.Commit(c); err == nil || err.Error() != c+" is not a commit of this repository" {
			t.Errorf("Commit(%s): %v", c, err)
		}
	}
	if _, _, err := r.Checkout("HEAD"); err == nil || !strings.Contains(err.Error(), "is not a full commit id") {
		t.Fatalf("Checkout(HEAD): %v", err)
	}
	// A checkout a killed `worktree add` left locked is replaced, with or without its directory.
	top4, remove4, err := r.Checkout(full)
	if err != nil {
		t.Fatal(err)
	}
	testutil.Git(t, dir, "worktree", "lock", top4)
	if _, _, err := r.Checkout(full); err != nil {
		t.Fatalf("over a locked checkout: %v", err)
	}
	testutil.Git(t, dir, "worktree", "lock", top4)
	if err := os.RemoveAll(top4); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Checkout(full); err != nil {
		t.Fatalf("over a locked checkout with no directory: %v", err)
	}
	if err := remove4(); err != nil {
		t.Fatal(err)
	}
	// Another worktree whose directory is gone keeps its record.
	gone := filepath.Join(t.TempDir(), "gone")
	testutil.Git(t, dir, "worktree", "add", "-q", "--detach", gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	// A failing post-checkout hook fails the checkout, and leaves nothing.
	testutil.Write(t, filepath.Join(dir, ".git", "hooks"), "post-checkout", "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(filepath.Join(dir, ".git", "hooks", "post-checkout"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Checkout(full); err == nil {
		t.Fatal("a failed post-checkout hook passed")
	}
	if _, err := os.Stat(filepath.Join(r.Dir(), "tree", full)); !os.IsNotExist(err) {
		t.Fatalf("failed checkout left: %v", err)
	}
	if wt := testutil.Git(t, dir, "worktree", "list", "--porcelain"); strings.Count(wt, "worktree ") != 2 || !strings.Contains(wt, gone) {
		t.Fatalf("worktrees (want the main one and the user's gone one):\n%s", wt)
	}
}

func TestSubmodules(t *testing.T) {
	t.Parallel()
	lib := t.TempDir()
	testutil.Write(t, lib, "proto.txt", "shared")
	testutil.GitRepo(t, lib)
	dir, _ := repo(t)
	testutil.Git(t, dir, "submodule", "add", "-q", lib, "proj/api/lib")
	testutil.Git(t, dir, "commit", "-qm", "lib")
	testutil.Git(t, dir, "submodule", "add", "-q", lib, "other/lib")
	testutil.Git(t, dir, "commit", "-qm", "other")
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := r.Commit("HEAD")
	if subs, err := r.Submodules(id, "proj"); err != nil || strings.Join(subs, ",") != "proj/api/lib" {
		t.Fatalf("under proj: %v %v", subs, err)
	}
	if subs, err := r.Submodules(id, "proj/web"); err != nil || len(subs) != 0 {
		t.Fatalf("under proj/web: %v %v", subs, err)
	}
}

func TestLock(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "d", "ship.lock")
	unlock, err := Lock(path, func() { t.Error("waited for a free lock") })
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	got := make(chan error)
	go func() {
		again, err := Lock(path, func() { close(waiting) })
		if err == nil {
			again()
		}
		got <- err
	}()
	<-waiting
	select {
	case err := <-got:
		t.Fatalf("took a held lock: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	unlock()
	if err := <-got; err != nil {
		t.Fatalf("after unlock: %v", err)
	}
}

func TestLaunch(t *testing.T) {
	t.Setenv("GIT_INDEX_FILE", "/gone/index")
	t.Setenv("STOWAGE_TEST_KEPT", "kept")
	log := filepath.Join(t.TempDir(), "log", "x.log")
	testutil.Write(t, filepath.Dir(log), "x.log", "earlier run\n")
	script := `echo "index=$GIT_INDEX_FILE kept=$STOWAGE_TEST_KEPT"; set -- $(cat /proc/$$/stat); [ "$6" -eq $$ ] && echo own-session; read x || echo stdin-empty; echo done`
	if err := Launch("/bin/sh", []string{"-c", script}, log); err != nil {
		t.Fatal(err)
	}
	var b []byte
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if b, _ = os.ReadFile(log); strings.HasSuffix(string(b), "done\n") {
			break
		}
	}
	if string(b) != "earlier run\nindex= kept=kept\nown-session\nstdin-empty\ndone\n" {
		t.Fatalf("log:\n%s", b)
	}
	if err := Launch(filepath.Join(t.TempDir(), "missing"), nil, log); err == nil {
		t.Fatal("a missing executable started")
	}
}

func TestInstall(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hooks", "post-commit")
	argv := []string{"/opt/my tools/stowage", "-C", "it's", "hook", "post-commit"}
	if replaced, err := Install(path, argv); err != nil || replaced {
		t.Fatalf("first: %v %v", replaced, err)
	}
	b, _ := os.ReadFile(path)
	if want := `'/opt/my tools/stowage' -C 'it'\''s' hook post-commit` + "\nexit 0\n"; !strings.HasSuffix(string(b), want) || !strings.HasPrefix(string(b), "#!/bin/sh\n") {
		t.Fatalf("script:\n%s", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if replaced, err := Install(path, argv[:1]); err != nil || !replaced {
		t.Fatalf("again: %v %v", replaced, err)
	}
	testutil.Write(t, filepath.Dir(path), "post-commit", "#!/bin/sh\nmine\n")
	if _, err := Install(path, argv); err == nil || !strings.Contains(err.Error(), "not written by stowage: add this line to it instead:\n  '/opt/my tools/stowage' -C") {
		t.Fatalf("foreign hook: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "#!/bin/sh\nmine\n" {
		t.Fatalf("foreign hook changed:\n%s", b)
	}
}
