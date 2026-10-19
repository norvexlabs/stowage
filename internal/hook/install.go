package hook

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// marker is the line that identifies a hook file Install wrote.
const marker = "# Written by stowage hook install"

// Command is argv as one shell command line.
func Command(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

// Script is the post-commit hook that runs argv (the stowage executable and its arguments) from
// the working tree's top level, where git runs hooks. It always exits 0: a commit is never failed
// by its release.
func Script(argv []string) string {
	return "#!/bin/sh\n" + marker + ": it ships a release when a commit moves the version.\n" +
		"# Run stowage hook install again to update it.\n" +
		Command(argv) + "\nexit 0\n"
}

// Install writes the hook that runs argv at path. It replaces a file only Install wrote, and
// reports whether there was one.
func Install(path string, argv []string) (replaced bool, err error) {
	script := Script(argv)
	old, err := os.ReadFile(path)
	switch {
	case err == nil && !bytes.Contains(old, []byte(marker)):
		return false, fmt.Errorf("%s exists and was not written by stowage: add this line to it instead:\n  %s",
			path, Command(argv))
	case err == nil:
		replaced = true
	case !os.IsNotExist(err):
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".post-commit-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(script); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return replaced, os.Rename(tmp.Name(), path)
}

func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:@+,") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
