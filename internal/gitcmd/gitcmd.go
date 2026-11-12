// Package gitcmd runs git for Stowage. Git's per-hook variables are dropped from every run: inside
// a hook they point at the commit in progress (a linked worktree's GIT_DIR, a temporary index),
// and git -C would read another repository layout through them.
package gitcmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// HookVars are the variables git sets for a hook that point into the commit in progress.
var HookVars = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_PREFIX"}

// Env is env less HookVars.
func Env(env []string) []string {
	var out []string
next:
	for _, kv := range env {
		for _, k := range HookVars {
			if strings.HasPrefix(kv, k+"=") {
				continue next
			}
		}
		out = append(out, kv)
	}
	return out
}

// Run runs git in dir and returns its output less trailing line breaks. Any failure is an error
// carrying git's message.
func Run(dir string, args ...string) (string, error) {
	out, _, err := run(dir, args...)
	return out, err
}

// Verify reports whether rev (a revision, or rev:path) names an object of the repository at dir:
// `rev-parse --verify -q`, whose exit 1 means it does not.
func Verify(dir, rev string) (bool, error) {
	_, ok, err := Resolve(dir, rev)
	return ok, err
}

// Resolve is the object id rev names in the repository at dir, ok false when it names none.
func Resolve(dir, rev string) (id string, ok bool, err error) {
	out, exit1, err := run(dir, "rev-parse", "--verify", "-q", "--end-of-options", rev)
	if exit1 {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return out, true, nil
}

// FileAt is the content commit rev records at path (relative to the top level of the repository
// at dir), a symbolic link followed as a checkout follows it — at any component, inside the
// repository — by `cat-file --batch --follow-symlinks`; a path starting ./ is relative to dir.
// ok is false when rev (or the path) does not exist there, or a link there leads to nothing; a
// link that leaves the repository, or loops, is an error.
func FileAt(dir, rev, path string) (content []byte, ok bool, err error) {
	var stderr bytes.Buffer
	cmd := exec.Command("git", "-C", dir, "cat-file", "--batch", "--follow-symlinks")
	cmd.Env = Env(os.Environ())
	cmd.Stdin = strings.NewReader(rev + ":" + path + "\n")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, false, fmt.Errorf("git cat-file %s:%s: %v: %s", rev, path, err, strings.TrimSpace(stderr.String()))
	}
	head, body, _ := bytes.Cut(out, []byte("\n"))
	f := strings.Fields(string(head))
	switch {
	case len(f) == 3 && f[1] == "blob":
		size, err := strconv.Atoi(f[2])
		if err != nil || size > len(body) {
			return nil, false, fmt.Errorf("git cat-file %s:%s: %q", rev, path, head)
		}
		return body[:size], true, nil
	case strings.HasSuffix(string(head), " missing"), len(f) == 2 && f[0] == "dangling":
		return nil, false, nil
	case len(f) == 2 && (f[0] == "symlink" || f[0] == "loop"):
		return nil, false, fmt.Errorf("%s at %s is a symbolic link that leaves the repository or loops", path, rev)
	}
	return nil, false, fmt.Errorf("git cat-file %s:%s: %s is no file", rev, path, strings.TrimSpace(string(head)))
}

func run(dir string, args ...string) (out string, exit1 bool, err error) {
	var stderr bytes.Buffer
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = Env(os.Environ())
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		return "", errors.As(err, &exit) && exit.ExitCode() == 1,
			fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimRight(string(b), "\n"), false, nil
}

// IsAncestor reports whether commit a is an ancestor of commit b (or b itself) in the repository
// at dir: `merge-base --is-ancestor`, whose exit 1 means it is not.
func IsAncestor(dir, a, b string) (bool, error) {
	_, exit1, err := run(dir, "merge-base", "--is-ancestor", "--end-of-options", a, b)
	if exit1 {
		return false, nil
	}
	return err == nil, err
}
