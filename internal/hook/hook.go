// Package hook runs a project's release from its git post-commit hook: it decides whether the
// commit moved the version, and starts that release's ship detached from the commit, one ship at a
// time per repository, from a checkout of that commit.
package hook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/norvexlabs/stowage/internal/gitcmd"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/version"
)

// markers are the git-dir entries of a history operation in progress: its commits are not
// releases.
var markers = []string{"rebase-merge", "rebase-apply", "sequencer", "CHERRY_PICK_HEAD", "MERGE_HEAD"}

// Repo is the git repository a project lives in.
type Repo struct {
	// GitDir is the working tree's git directory, CommonDir the repository's (they differ in a
	// linked worktree); Top is the working tree's top level; HookFile is the post-commit hook git
	// runs (core.hooksPath applied). All absolute.
	GitDir, CommonDir, Top, HookFile string
}

// Open finds the repository that holds root.
func Open(root string) (Repo, error) {
	out, err := gitcmd.Run(root, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir", "--show-toplevel", "--git-path", "hooks/post-commit")
	if err != nil {
		return Repo{}, err
	}
	f := strings.Split(out, "\n")
	if len(f) != 4 {
		return Repo{}, fmt.Errorf("%s is not in a git working tree", root)
	}
	return Repo{GitDir: f[0], CommonDir: f[1], Top: f[2], HookFile: f[3]}, nil
}

// Dir is where the hook keeps its lock, logs and checkouts: inside the git directory, so it never
// dirties the working tree.
func (r Repo) Dir() string { return filepath.Join(r.CommonDir, "stowage") }

// LockPath is the lock every hook ship of the repository takes.
func (r Repo) LockPath() string { return filepath.Join(r.Dir(), "ship.lock") }

// LogPath is the log of a project's release; tag is the version's tag, a safe file name.
func (r Repo) LogPath(name, tag string) string {
	return filepath.Join(r.Dir(), "log", name+"-"+tag+".log")
}

// objectID matches a full SHA-1 or SHA-256 object id.
var objectID = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// Commit is the full id of the commit rev names in the repository; any other revision is refused,
// since the id names a directory.
func (r Repo) Commit(rev string) (string, error) {
	id, ok, err := gitcmd.Resolve(r.Top, rev+"^{commit}")
	if err != nil {
		return "", err
	}
	if !ok || !objectID.MatchString(id) {
		return "", fmt.Errorf("%s is not a commit of this repository", rev)
	}
	return id, nil
}

// Submodules lists the submodules (gitlinks) commit id records under the directory rel.
func (r Repo) Submodules(id, rel string) ([]string, error) {
	out, err := gitcmd.Run(r.Top, "ls-tree", "-r", "--full-tree", id, "--", rel)
	if err != nil {
		return nil, err
	}
	var subs []string
	for _, line := range strings.Split(out, "\n") {
		if mode, rest, ok := strings.Cut(line, " "); ok && mode == "160000" {
			if _, path, ok := strings.Cut(rest, "\t"); ok {
				subs = append(subs, path)
			}
		}
	}
	return subs, nil
}

// Checkout checks commit id (a full id, from Commit) out, detached, as a linked worktree of its
// own under Dir, and returns its top level and the function that removes it. Whatever a killed
// run left at that path is removed first. Callers hold the lock.
func (r Repo) Checkout(id string) (string, func() error, error) {
	if !objectID.MatchString(id) {
		return "", nil, fmt.Errorf("%q is not a full commit id", id)
	}
	dir := filepath.Join(r.Dir(), "tree", id)
	// Forced twice, worktree remove also takes a checkout a killed `worktree add` left locked,
	// and git's record of one whose directory is gone. When git knows no checkout there, the
	// directory (Stowage's own) is removed. Only this checkout is touched: never a prune, which
	// would also forget the user's own missing worktrees.
	remove := func() error {
		if _, err := gitcmd.Run(r.Top, "worktree", "remove", "-f", "-f", dir); err == nil {
			return nil
		}
		return os.RemoveAll(dir)
	}
	if err := remove(); err != nil {
		return "", nil, err
	}
	if _, err := gitcmd.Run(r.Top, "worktree", "add", "-q", "--detach", "--force", dir, id); err != nil {
		_ = remove()
		return "", nil, err
	}
	return dir, remove, nil
}

// Decision is Detect's answer: Version (and Commit, HEAD's id) is set when the commit is a release,
// Reason says why not otherwise.
type Decision struct {
	Version, Commit string
	Reason          string
}

// Detect decides whether the commit at HEAD is a release: no history operation is in progress,
// HEAD is neither a merge (its first parent is not the history the release follows), a
// cherry-pick, nor an amend of a commit already at its version (that commit's ship was started),
// and the version at HEAD is valid and later than the one at HEAD~1 (or HEAD~1 records none). The versions are read from the commits, not the
// working tree.
func Detect(p *project.Project, r Repo) (Decision, error) {
	for _, m := range markers {
		if _, err := os.Lstat(filepath.Join(r.GitDir, m)); err == nil {
			return Decision{Reason: "skip: " + m + " in progress"}, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return Decision{}, err
		}
	}
	if merge, err := gitcmd.Verify(p.Root, "HEAD^2"); err != nil {
		return Decision{}, err
	} else if merge {
		return Decision{Reason: "skip: HEAD is a merge commit"}, nil
	}
	// The reflog names how HEAD moved; with no reflog, an amend or a cherry-pick cannot be told
	// apart.
	how, err := gitcmd.Run(p.Root, "reflog", "-1", "--format=%gs", "HEAD")
	if err != nil {
		return Decision{}, err
	}
	if strings.HasPrefix(how, "cherry-pick") || strings.HasPrefix(how, "commit (cherry-pick)") {
		return Decision{Reason: "skip: HEAD is a cherry-pick"}, nil
	}
	amend := strings.HasPrefix(how, "commit (amend)")
	src, ok := p.Source.(version.RevisionSource)
	if !ok {
		return Decision{Reason: "skip: the " + p.Spec.Version.Source.Or("changelog").Name + " version source does not move with a commit: publish with stowage ship"}, nil
	}
	cur, err := src.At("HEAD")
	if err != nil {
		return Decision{}, fmt.Errorf("version at HEAD: %w", err)
	}
	if cur == "" {
		return Decision{Reason: "skip: HEAD records no version"}, nil
	}
	if err := p.Scheme.Validate(cur); err != nil {
		return Decision{}, fmt.Errorf("version at HEAD: %w", err)
	}
	prev, err := src.At("HEAD~1")
	if err != nil {
		return Decision{}, fmt.Errorf("version at HEAD~1: %w", err)
	}
	if prev != "" {
		if err := p.Scheme.Validate(prev); err != nil {
			if !version.IsInvalid(err) {
				return Decision{}, fmt.Errorf("version at HEAD~1: %w", err)
			}
			prev = ""
		}
	}
	if prev != "" {
		c, err := p.Scheme.Compare(cur, prev)
		if err != nil {
			return Decision{}, err
		}
		switch {
		case c == 0:
			return Decision{Reason: fmt.Sprintf("no version bump (%s): not a release", cur)}, nil
		case c < 0:
			return Decision{Reason: fmt.Sprintf("version moved back from %s to %s: not a release", prev, cur)}, nil
		}
	}
	// HEAD moves the version. An amend whose amended commit (HEAD@{1}, on the same parent) was
	// already at this version amends a release whose ship was started.
	if amend {
		// The amended commit is the reflog's previous entry; an expired reflog has none.
		ids, err := gitcmd.Run(p.Root, "reflog", "-2", "--format=%H", "HEAD")
		if err != nil {
			return Decision{}, err
		}
		was := ""
		if f := strings.Fields(ids); len(f) == 2 {
			if was, err = src.At(f[1]); err != nil {
				return Decision{}, fmt.Errorf("version at HEAD@{1}: %w", err)
			}
		}
		if was != "" && p.Scheme.Validate(was) == nil {
			if c, err := p.Scheme.Compare(cur, was); err != nil {
				return Decision{}, err
			} else if c == 0 {
				return Decision{Reason: fmt.Sprintf("skip: HEAD amends the commit that started the ship of %s (run stowage ship to publish the amended one)", cur)}, nil
			}
		}
	}
	head, err := gitcmd.Run(p.Root, "rev-parse", "HEAD")
	if err != nil {
		return Decision{}, err
	}
	return Decision{Version: cur, Commit: head}, nil
}
