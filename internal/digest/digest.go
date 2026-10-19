// Package digest computes inputs digests: a content hash of the files a build node reads, so an
// unchanged node is recognised and skipped.
package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"

	"github.com/norvexlabs/stowage/internal/spec"
)

// Builder accumulates labelled fields and file trees into one sha256. Field order is the caller's
// and is part of the digest; file trees are walked in sorted order, so the result does not depend
// on the filesystem.
type Builder struct {
	h io.Writer
	s interface{ Sum([]byte) []byte }
}

// New starts a digest under a domain label, so two node kinds hashing the same bytes differ.
func New(domain string) *Builder {
	h := sha256.New()
	b := &Builder{h: h, s: h}
	b.Field("domain", domain)
	return b
}

// Field adds a labelled value.
func (b *Builder) Field(label, value string) {
	fmt.Fprintf(b.h, "%d:%s=%d:%s\n", len(label), label, len(value), value)
}

// Sum returns "sha256:<hex>".
func (b *Builder) Sum() string { return "sha256:" + hex.EncodeToString(b.s.Sum(nil)) }

// Ignore excludes files from a tree; nil excludes nothing.
type Ignore struct {
	base string // absolute directory the patterns are relative to
	pm   *patternmatcher.PatternMatcher
}

// DockerIgnore loads the ignore file docker applies to a build: `<dockerfile>.dockerignore` when
// present, else `<context>/.dockerignore`. The result always knows the context, with no pattern
// when neither file exists.
func DockerIgnore(contextDir, dockerfile string) (*Ignore, error) {
	abs, err := filepath.Abs(contextDir)
	if err != nil {
		return nil, err
	}
	for _, p := range []string{dockerfile + ".dockerignore", filepath.Join(contextDir, ".dockerignore")} {
		f, err := os.Open(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		patterns, err := ignorefile.ReadAll(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		pm, err := patternmatcher.New(patterns)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		return &Ignore{base: abs, pm: pm}, nil
	}
	return &Ignore{base: abs}, nil
}

// inContext reports whether abs lies inside the build context.
func (ig *Ignore) inContext(abs string) bool {
	if ig == nil {
		return false
	}
	rel, err := filepath.Rel(ig.base, abs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// skipsExcludedDirs reports whether an excluded directory can be skipped whole: not when a
// pattern re-includes something below it.
func (ig *Ignore) skipsExcludedDirs() bool { return ig == nil || ig.pm == nil || !ig.pm.Exclusions() }

// ErrGitInContext is returned when a build context holds a `.git` entry its ignore file does not
// exclude: docker would send it to the build, but its content (index, logs) changes on every
// commit, so it cannot be part of a stable inputs digest.
var ErrGitInContext = errors.New("the build context holds .git and its .dockerignore does not exclude it (add `**/.git` to it)")

// GitInContext returns the first `.git` entry inside the context that the ignore file does not
// exclude, or "".
func GitInContext(ig *Ignore) (string, error) {
	if ig == nil {
		return "", nil
	}
	found := ""
	err := filepath.WalkDir(ig.base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		skip, err := ig.excluded(path)
		if err != nil {
			return err
		}
		if skip {
			if d.IsDir() && ig.skipsExcludedDirs() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == ".git" {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found, err
}

// excluded reports whether abs (an absolute path) is ignored. Paths outside the base are never
// ignored: docker cannot see them, so an explicit input outside the context counts as declared.
func (ig *Ignore) excluded(abs string) (bool, error) {
	if ig == nil || ig.pm == nil {
		return false, nil
	}
	rel, err := filepath.Rel(ig.base, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, nil
	}
	if rel == "." {
		return false, nil
	}
	return ig.pm.MatchesOrParentMatches(filepath.ToSlash(rel))
}

// Tree adds every regular file and symlink under each path (files or directories, relative to
// root) to the digest. A `.git` entry — the directory, or the file a worktree or submodule holds
// in its place — is never read; inside a build context it must be excluded by the ignore file
// (ErrGitInContext), since docker would otherwise send it. A missing path, an absolute path or one leaving root is an error:
// a declared input must be part of the project.
func (b *Builder) Tree(root string, paths []string, ig *Ignore) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var files []string
	for _, p := range paths {
		if err := spec.InsideRoot(p); err != nil {
			return fmt.Errorf("input %q: %w", p, err)
		}
		start := filepath.Join(absRoot, filepath.FromSlash(p))
		if _, err := os.Lstat(start); err != nil {
			return fmt.Errorf("input %q: %w", p, err)
		}
		err := filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			skip, err := ig.excluded(path)
			if err != nil {
				return err
			}
			if skip {
				if d.IsDir() && ig.skipsExcludedDirs() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Name() == ".git" {
				if ig.inContext(path) {
					return fmt.Errorf("%s: %w", path, ErrGitInContext)
				}
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() || seen[path] {
				return nil
			}
			if d.Type().IsRegular() || d.Type()&fs.ModeSymlink != 0 {
				seen[path] = true
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	sort.Strings(files)
	for _, f := range files {
		rel, err := filepath.Rel(absRoot, f)
		if err != nil {
			return err
		}
		sum, mode, err := fileSum(f)
		if err != nil {
			return err
		}
		b.Field("file", filepath.ToSlash(rel)+"\x00"+mode+"\x00"+sum)
	}
	b.Field("files", fmt.Sprint(len(files)))
	return nil
}

// fileSum hashes a file's content (a symlink's target) and classifies its mode: only the
// executable bit and symlink-ness reach a build reproducibly.
func fileSum(path string) (sum, mode string, err error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", "", err
	}
	h := sha256.New()
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return "", "", err
		}
		h.Write([]byte(target))
		mode = "l"
	default:
		f, err := os.Open(path)
		if err != nil {
			return "", "", err
		}
		defer f.Close()
		if _, err := io.Copy(h, f); err != nil {
			return "", "", err
		}
		mode = "-"
		if fi.Mode()&0o111 != 0 {
			mode = "x"
		}
	}
	return hex.EncodeToString(h.Sum(nil)), mode, nil
}
