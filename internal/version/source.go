package version

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/norvexlabs/stowage/internal/changelog"
	"github.com/norvexlabs/stowage/internal/command"
	"github.com/norvexlabs/stowage/internal/gitcmd"
	"github.com/norvexlabs/stowage/internal/spec"
)

// Source yields the project's current version string.
type Source interface {
	Get() (string, error)
}

// RevisionSource is a Source that can also read the version a git commit records. A source whose
// version does not move with a commit (git-tag: a tag comes after its commit; command) does not
// implement it.
type RevisionSource interface {
	Source
	// At returns the version at git revision rev, "" when rev does not exist or records none (the
	// file is absent there, no tag is reachable from it).
	At(rev string) (string, error)
}

// NewSource builds the version source named by the spec; changelog is the default.
func NewSource(root string, p spec.Provider) (Source, error) {
	p = p.Or("changelog")
	switch p.Name {
	case "changelog":
		var c struct {
			File string `yaml:"file"`
		}
		if err := p.Decode(&c); err != nil {
			return nil, err
		}
		if c.File == "" {
			c.File = "CHANGELOG.md"
		}
		path, err := spec.ProjectPath(root, c.File)
		if err != nil {
			return nil, fmt.Errorf("version.source file: %w", err)
		}
		return changelogSource{root: root, path: path}, nil
	case "file":
		var c struct {
			File string `yaml:"file"`
		}
		if err := p.Decode(&c); err != nil {
			return nil, err
		}
		if c.File == "" {
			c.File = "VERSION"
		}
		path, err := spec.ProjectPath(root, c.File)
		if err != nil {
			return nil, fmt.Errorf("version.source file: %w", err)
		}
		return fileSource{root: root, path: path}, nil
	case "git-tag":
		var c struct {
			Match string `yaml:"match"`
			Strip string `yaml:"strip"`
		}
		if err := p.Decode(&c); err != nil {
			return nil, err
		}
		return gitTagSource{root: root, match: c.Match, strip: c.Strip}, nil
	case "command":
		var c command.Config
		if err := p.Decode(&c); err != nil {
			return nil, err
		}
		if err := c.Validate(); err != nil {
			return nil, err
		}
		return commandSource{command.Runner{Root: root, Config: c}}, nil
	default:
		return nil, fmt.Errorf("version.source: unknown provider %q (changelog, file, git-tag, command)", p.Name)
	}
}

// changelogSource takes the first released heading of a Keep a Changelog file.
type changelogSource struct{ root, path string }

func (s changelogSource) Get() (string, error) {
	doc, err := os.ReadFile(s.path)
	if err != nil {
		return "", err
	}
	return s.parse(doc, s.path)
}

func (s changelogSource) At(rev string) (string, error) {
	return fileAt(s.root, s.path, rev, s.parse)
}

func (changelogSource) parse(doc []byte, name string) (string, error) {
	hs := changelog.ParseKeepAChangelog(doc)
	if len(hs) == 0 {
		return "", fmt.Errorf("%s: no release heading (## [x])", name)
	}
	return hs[0].Version, nil
}

// fileSource takes the first non-empty line of a file.
type fileSource struct{ root, path string }

func (s fileSource) Get() (string, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return "", err
	}
	return s.parse(b, s.path)
}

func (s fileSource) At(rev string) (string, error) {
	return fileAt(s.root, s.path, rev, s.parse)
}

func (fileSource) parse(b []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			return line, nil
		}
	}
	return "", fmt.Errorf("%s is empty", name)
}

// fileAt parses the file at path (inside root) as commit rev records it: "" when rev does not
// exist or has no such file.
func fileAt(root, path, rev string, parse func([]byte, string) (string, error)) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	obj := rev + ":./" + filepath.ToSlash(rel)
	// rev-parse -q exits 1 both when rev does not exist and when it lacks the file.
	if found, err := gitcmd.Verify(root, obj); err != nil || !found {
		return "", err
	}
	b, err := gitcmd.Run(root, "cat-file", "blob", obj)
	if err != nil {
		return "", err
	}
	return parse([]byte(b), obj)
}

// gitTagSource takes the nearest tag reachable from HEAD.
type gitTagSource struct{ root, match, strip string }

func (s gitTagSource) Get() (string, error) {
	args := []string{"describe", "--tags", "--abbrev=0"}
	if s.match != "" {
		args = append(args, "--match", s.match)
	}
	out, err := gitcmd.Run(s.root, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(strings.TrimSpace(out), s.strip), nil
}

// commandSource asks a project command: op version.get → {"version": "..."}.
type commandSource struct{ r command.Runner }

func (s commandSource) Get() (string, error) {
	var out struct {
		Version string `json:"version"`
	}
	err := s.r.Call(context.Background(), map[string]any{"op": "version.get"}, &out)
	if errors.Is(err, command.ErrNoAnswer) {
		return "", errors.New("version.get: the command gave no version")
	}
	if err != nil {
		return "", err
	}
	if out.Version == "" {
		return "", errors.New("version.get: answer has no \"version\"")
	}
	return out.Version, nil
}
