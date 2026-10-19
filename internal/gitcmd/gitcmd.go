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
