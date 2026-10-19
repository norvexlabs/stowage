package lock

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func load(t *testing.T, content string) (*Lock, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestLoad(t *testing.T) {
	if l, err := Load(filepath.Join(t.TempDir(), FileName)); l != nil || err != nil {
		t.Errorf("missing file: %v, %v", l, err)
	}
	l, err := load(t, "stowage: 1.4.2\n")
	if err != nil || l.Stowage != "1.4.2" {
		t.Fatalf("Load = %+v, %v", l, err)
	}
	for content, want := range map[string]string{
		"stowage: \"\"\n":               "stowage version is missing",
		"stowage: 1.0.0\nruntime: {}\n": "runtime",
		"stowage: 1.0.0\nextra: 1\n":    "extra",
		"stowage: 1.0.0+build.7\n":      "no build metadata",
		"stowage: dev\n":                "MAJOR.MINOR.PATCH",
	} {
		if _, err := load(t, content); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", content, err, want)
		}
	}
}

func TestWriteRoundTrips(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), FileName)
	in := &Lock{Stowage: "1.4.2"}
	if err := Write(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := Load(path)
	if err != nil || !reflect.DeepEqual(out, in) {
		t.Errorf("Load(Write(l)) = %+v, %v", out, err)
	}
	if ents, _ := os.ReadDir(filepath.Dir(path)); len(ents) != 1 {
		t.Errorf("Write left %d files", len(ents))
	}
}
