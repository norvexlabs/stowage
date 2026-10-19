// Package changelog reads a project's release notes through its changelog provider.
package changelog

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/norvexlabs/stowage/internal/command"
	"github.com/norvexlabs/stowage/internal/spec"
)

// Provider lists the versions a changelog describes and returns one version's section.
type Provider interface {
	// Versions lists every version with a section, in any order.
	Versions() ([]string, error)
	// Section returns the text for v, or ok=false when the changelog has no section for it.
	Section(v string) (text string, ok bool, err error)
}

// Comparer orders versions; a version.Scheme satisfies it.
type Comparer interface {
	Compare(a, b string) (int, error)
}

// New builds the changelog provider named by the spec; keepachangelog is the default.
func New(root string, p spec.Provider) (Provider, error) {
	p = p.Or("keepachangelog")
	switch p.Name {
	case "keepachangelog":
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
			return nil, fmt.Errorf("changelog file: %w", err)
		}
		return KeepAChangelog{Path: path}, nil
	case "dir-per-version":
		var c struct {
			Dir     string `yaml:"dir"`
			Pattern string `yaml:"pattern"`
		}
		if err := p.Decode(&c); err != nil {
			return nil, err
		}
		if c.Dir == "" {
			return nil, errors.New("changelog dir-per-version: dir is required")
		}
		if c.Pattern == "" {
			c.Pattern = "{v}.md"
		}
		pre, suf, ok := strings.Cut(c.Pattern, "{v}")
		if !ok || strings.Contains(suf, "{v}") || strings.ContainsAny(c.Pattern, `/\`) {
			return nil, fmt.Errorf("changelog dir-per-version: pattern %q must hold {v} once and no path separator", c.Pattern)
		}
		dir, err := spec.ProjectPath(root, c.Dir)
		if err != nil {
			return nil, fmt.Errorf("changelog dir: %w", err)
		}
		return DirPerVersion{Dir: dir, prefix: pre, suffix: suf}, nil
	case "command":
		var c command.Config
		if err := p.Decode(&c); err != nil {
			return nil, err
		}
		if err := c.Validate(); err != nil {
			return nil, err
		}
		return Command{command.Runner{Root: root, Config: c}}, nil
	default:
		return nil, fmt.Errorf("changelog: unknown provider %q (keepachangelog, dir-per-version, command)", p.Name)
	}
}

// Heading is one release heading of a Keep a Changelog file.
type Heading struct {
	Version string
	Body    string
}

// ParseKeepAChangelog splits a Keep a Changelog document into its release sections, in file
// order. A level-2 heading `## [x]` or `## x` (anything after the version is ignored) opens a
// section; "Unreleased" is skipped.
func ParseKeepAChangelog(doc []byte) []Heading {
	var out []Heading
	var cur *Heading
	var body strings.Builder
	flush := func() {
		if cur != nil {
			cur.Body = strings.TrimSpace(body.String())
			out = append(out, *cur)
		}
		cur = nil
		body.Reset()
	}
	sc := bufio.NewScanner(bytes.NewReader(doc))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	inFence := false
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
		}
		if !inFence && strings.HasPrefix(line, "## ") {
			flush()
			v := headingVersion(line[3:])
			if v != "" && !strings.EqualFold(v, "unreleased") {
				cur = &Heading{Version: v}
			}
			continue
		}
		if cur != nil {
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	flush()
	return out
}

func headingVersion(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") {
		if end := strings.IndexByte(s, ']'); end > 0 {
			return strings.TrimSpace(s[1:end])
		}
		return ""
	}
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

// KeepAChangelog reads one Markdown file in the Keep a Changelog layout.
type KeepAChangelog struct{ Path string }

func (k KeepAChangelog) read() ([]Heading, error) {
	doc, err := os.ReadFile(k.Path)
	if err != nil {
		return nil, err
	}
	return ParseKeepAChangelog(doc), nil
}

func (k KeepAChangelog) Versions() ([]string, error) {
	hs, err := k.read()
	if err != nil {
		return nil, err
	}
	vs := make([]string, len(hs))
	for i, h := range hs {
		vs[i] = h.Version
	}
	return vs, nil
}

func (k KeepAChangelog) Section(v string) (string, bool, error) {
	hs, err := k.read()
	if err != nil {
		return "", false, err
	}
	for _, h := range hs {
		if h.Version == v {
			return h.Body, true, nil
		}
	}
	return "", false, nil
}

// DirPerVersion reads one file per version from a directory.
type DirPerVersion struct {
	Dir            string
	prefix, suffix string
}

func (d DirPerVersion) Versions() ([]string, error) {
	entries, err := os.ReadDir(d.Dir)
	if err != nil {
		return nil, err
	}
	var vs []string
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && strings.HasPrefix(n, d.prefix) && strings.HasSuffix(n, d.suffix) && len(n) > len(d.prefix)+len(d.suffix) {
			vs = append(vs, n[len(d.prefix):len(n)-len(d.suffix)])
		}
	}
	return vs, nil
}

func (d DirPerVersion) Section(v string) (string, bool, error) {
	if strings.ContainsAny(v, `/\`) || v == "." || v == ".." {
		return "", false, fmt.Errorf("changelog: version %q is not a file name", v)
	}
	b, err := os.ReadFile(filepath.Join(d.Dir, d.prefix+v+d.suffix))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(string(b)), true, nil
}

// Command asks a project command: ops changelog.versions → {"versions": [...]},
// changelog.section {version} → {"text": "..."}; exit 2 on a section means none.
type Command struct{ r command.Runner }

func (c Command) Versions() ([]string, error) {
	var out struct {
		Versions []string `json:"versions"`
	}
	if err := c.r.Call(context.Background(), map[string]any{"op": "changelog.versions"}, &out); err != nil {
		return nil, err
	}
	return out.Versions, nil
}

func (c Command) Section(v string) (string, bool, error) {
	var out struct {
		Text string `json:"text"`
	}
	err := c.r.Call(context.Background(), map[string]any{"op": "changelog.section", "version": v}, &out)
	if errors.Is(err, command.ErrNoAnswer) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return out.Text, true, nil
}

// Entry is one version's section.
type Entry struct {
	Version string `json:"version"`
	Text    string `json:"text"`
}

// Between returns the sections of every version v with from < v <= to, newest first. An empty
// from means "from the beginning". Versions the scheme refuses are skipped: a changelog may
// describe history older than the scheme.
func Between(p Provider, sch Comparer, from, to string) ([]Entry, error) {
	vs, err := p.Versions()
	if err != nil {
		return nil, err
	}
	var keep []string
	for _, v := range vs {
		if c, err := sch.Compare(v, to); err != nil || c > 0 {
			continue
		}
		if from != "" {
			if c, err := sch.Compare(v, from); err != nil || c <= 0 {
				continue
			}
		}
		keep = append(keep, v)
	}
	var sortErr error
	sort.SliceStable(keep, func(i, j int) bool {
		c, err := sch.Compare(keep[i], keep[j])
		if err != nil {
			sortErr = err
		}
		return c > 0
	})
	if sortErr != nil {
		return nil, sortErr
	}
	out := make([]Entry, 0, len(keep))
	for _, v := range keep {
		text, ok, err := p.Section(v)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, Entry{Version: v, Text: text})
		}
	}
	return out, nil
}
