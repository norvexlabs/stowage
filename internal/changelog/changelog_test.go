package changelog

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/spec"
)

const doc = "# Changelog\n\n## [Unreleased]\n- wip\n\n## [1.10.0] - 2026-10-08\n### Added\n- ten\n```\n## [not-a-heading]\n```\n\n## 1.9.0\n- nine\n\n## [1.2.0]\n- two\n"

func TestParseKeepAChangelog(t *testing.T) {
	hs := ParseKeepAChangelog([]byte(doc))
	var vs []string
	for _, h := range hs {
		vs = append(vs, h.Version)
	}
	if want := []string{"1.10.0", "1.9.0", "1.2.0"}; !reflect.DeepEqual(vs, want) {
		t.Fatalf("versions = %v, want %v", vs, want)
	}
	if !strings.Contains(hs[0].Body, "## [not-a-heading]") {
		t.Error("a heading inside a code fence split the section")
	}
	if strings.Contains(hs[0].Body, "wip") {
		t.Error("Unreleased leaked into the next section")
	}
}

// numeric orders "1.10.0" above "1.9.0", unlike string order.
type numeric struct{}

func (numeric) Compare(a, b string) (int, error) {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range pa {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			if x < y {
				return -1, nil
			}
			return 1, nil
		}
	}
	return 0, nil
}

func TestBetweenBounds(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := New(dir, spec.Provider{})
	if err != nil {
		t.Fatal(err)
	}
	versions := func(from, to string) []string {
		es, err := Between(p, numeric{}, from, to)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range es {
			out = append(out, e.Version)
		}
		return out
	}
	// from is exclusive, to inclusive, newest first.
	if got := versions("1.2.0", "1.10.0"); !reflect.DeepEqual(got, []string{"1.10.0", "1.9.0"}) {
		t.Errorf("(1.2.0, 1.10.0] = %v", got)
	}
	if got := versions("1.9.0", "1.9.0"); len(got) != 0 {
		t.Errorf("(1.9.0, 1.9.0] = %v, want empty", got)
	}
	if got := versions("", "1.9.0"); !reflect.DeepEqual(got, []string{"1.9.0", "1.2.0"}) {
		t.Errorf("(, 1.9.0] = %v", got)
	}
}

func TestDirPerVersion(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"1.0.0.md", "1.1.0.md", "README.txt"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("notes "+f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, err := New(dir, spec.Provider{Name: "dir-per-version", Config: map[string]any{"dir": "."}})
	if err != nil {
		t.Fatal(err)
	}
	vs, _ := p.Versions()
	if want := []string{"1.0.0", "1.1.0"}; !reflect.DeepEqual(vs, want) {
		t.Errorf("Versions = %v", vs)
	}
	if text, ok, err := p.Section("1.1.0"); err != nil || !ok || text != "notes 1.1.0.md" {
		t.Errorf("Section = %q, %v, %v", text, ok, err)
	}
	if _, ok, err := p.Section("9.9.9"); err != nil || ok {
		t.Errorf("missing section: ok=%v err=%v", ok, err)
	}
	if _, _, err := p.Section("../etc/passwd"); err == nil {
		t.Error("path traversal accepted")
	}
	if _, err := New(dir, spec.Provider{Name: "dir-per-version", Config: map[string]any{"dir": ".", "pattern": "x.md"}}); err == nil {
		t.Error("pattern without {v} accepted")
	}
}

func FuzzParseKeepAChangelog(f *testing.F) {
	f.Add(doc)
	f.Add("## [\n## ]\n```\n## [1]\n")
	f.Fuzz(func(t *testing.T, src string) {
		for _, h := range ParseKeepAChangelog([]byte(src)) {
			if h.Version == "" || strings.EqualFold(h.Version, "unreleased") {
				t.Fatalf("heading %+v from %q", h, src)
			}
		}
	})
}
