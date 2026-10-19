package helm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const chartYAML = "apiVersion: v2\n# The chart's name.\nname: notes\nversion: 1.0.0\n"

// chart writes a chart directory of files (path → content) and returns it.
func chart(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

var values = Values{
	Registry: "registry.example.org/notes",
	Release:  Release{Name: "notes", Version: "1.4.0", Revision: 2, Channel: "stable", Tag: "1.4.0-r2"},
	Images:   map[string]Image{"web": {Repository: "notes-web", Digest: "sha256:" + strings.Repeat("a", 64)}, "api": {Repository: "notes-api", Digest: "sha256:" + strings.Repeat("b", 64)}},
}

// unpack reads a chart archive into path → content.
func unpack(t *testing.T, tgz []byte) map[string]string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg || h.Mode != 0o644 || h.ModTime.Unix() != 0 {
			t.Errorf("%s: header %+v", h.Name, h)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = string(data)
	}
}

func TestPackage(t *testing.T) {
	t.Parallel()
	own := "# Settings.\nweb:\n  replicas: 1\nnote: |\n  a block scalar\n  ending the file\n"
	dir := chart(t, map[string]string{
		"Chart.yaml":           chartYAML,
		"values.yaml":          own,
		"templates/web.yaml":   "\xEF\xBB\xBFkind: Deployment\n",
		"templates/.hidden":    "ignored by helm's default rule",
		".helmignore":          "ci/\n*.bak\n",
		"ci/values.yaml":       "x: 1\n",
		"templates/old.bak":    "old",
		"files/config.toml":    "a = 1\n",
		"charts/db/Chart.yaml": "apiVersion: v2\nname: db\nversion: 0.1.0\n",
	})
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range c.Files {
		names = append(names, f.Name)
	}
	if got, want := strings.Join(names, " "), ".helmignore Chart.yaml charts/db/Chart.yaml files/config.toml templates/web.yaml values.yaml"; got != want {
		t.Fatalf("files = %s, want %s", got, want)
	}
	tgz, cfg, err := c.Package("0.7.0", "1.4.0", values)
	if err != nil {
		t.Fatal(err)
	}
	again, cfg2, err := c.Package("0.7.0", "1.4.0", values)
	if err != nil || !bytes.Equal(tgz, again) || !bytes.Equal(cfg, cfg2) {
		t.Fatal("packaging the same chart twice differs")
	}
	files := unpack(t, tgz)
	if len(files) != len(c.Files) {
		t.Errorf("archive holds %d files, want %d", len(files), len(c.Files))
	}
	if files["notes/templates/web.yaml"] != "kind: Deployment\n" {
		t.Errorf("BOM kept: %q", files["notes/templates/web.yaml"])
	}

	var meta map[string]any
	if err := yaml.Unmarshal([]byte(files["notes/Chart.yaml"]), &meta); err != nil {
		t.Fatal(err)
	}
	if meta["version"] != "0.7.0" || meta["appVersion"] != "1.4.0" || meta["name"] != "notes" || !strings.Contains(files["notes/Chart.yaml"], "# The chart's name.") {
		t.Errorf("Chart.yaml:\n%s", files["notes/Chart.yaml"])
	}
	var config map[string]any
	if err := json.Unmarshal(cfg, &config); err != nil || config["version"] != "0.7.0" || config["appVersion"] != "1.4.0" || config["apiVersion"] != "v2" {
		t.Errorf("config = %s, %v", cfg, err)
	}

	got := files["notes/values.yaml"]
	if !strings.HasPrefix(got, own) {
		t.Errorf("the chart's own values.yaml is not kept byte for byte:\n%s", got)
	}
	var v struct {
		Web     map[string]int
		Note    string
		Stowage Values
	}
	if err := yaml.Unmarshal([]byte(got), &v); err != nil {
		t.Fatal(err)
	}
	if v.Web["replicas"] != 1 || v.Note != "a block scalar\nending the file\n" || v.Stowage.Images["web"] != values.Images["web"] ||
		v.Stowage.Registry != values.Registry || v.Stowage.Release != values.Release || v.Stowage.PublicKeys != nil {
		t.Errorf("values:\n%s", got)
	}
}

// A chart without values.yaml gets one holding the block alone.
func TestPackageWithoutValues(t *testing.T) {
	t.Parallel()
	c, err := Load(chart(t, map[string]string{"Chart.yaml": chartYAML}))
	if err != nil {
		t.Fatal(err)
	}
	v := values
	v.PublicKeys = []string{"-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n"}
	tgz, _, err := c.Package("0.1.0", "1.4.0", v)
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Stowage Values }
	if err := yaml.Unmarshal([]byte(unpack(t, tgz)["notes/values.yaml"]), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Stowage.PublicKeys) != 1 || got.Stowage.PublicKeys[0] != v.PublicKeys[0] || got.Stowage.Release.Tag != "1.4.0-r2" {
		t.Errorf("values: %+v", got)
	}
}

func TestLoadRefuses(t *testing.T) {
	t.Parallel()
	tgz := func(name string) string {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		tw := tar.NewWriter(zw)
		data := "apiVersion: v2\nname: " + name + "\nversion: 1.0.0\n"
		_ = tw.WriteHeader(&tar.Header{Name: name + "/Chart.yaml", Mode: 0o644, Size: int64(len(data))})
		_, _ = tw.Write([]byte(data))
		_ = tw.Close()
		_ = zw.Close()
		return b.String()
	}
	deps := chartYAML + "dependencies:\n  - name: db\n    version: 0.1.0\n  - name: cache\n    version: 1.0.0\n"
	for name, tc := range map[string]struct {
		files map[string]string
		want  string
	}{
		"no Chart.yaml":        {map[string]string{"values.yaml": "a: 1\n"}, "Chart.yaml is missing"},
		"ignored Chart.yaml":   {map[string]string{"Chart.yaml": chartYAML, ".helmignore": "Chart.yaml\n"}, "Chart.yaml is missing"},
		"apiVersion v1":        {map[string]string{"Chart.yaml": "apiVersion: v1\nname: notes\n"}, "apiVersion v2"},
		"no name":              {map[string]string{"Chart.yaml": "apiVersion: v2\n"}, "not a chart name"},
		"name with a slash":    {map[string]string{"Chart.yaml": "apiVersion: v2\nname: a/b\n"}, "not a chart name"},
		"name ..":              {map[string]string{"Chart.yaml": "apiVersion: v2\nname: ..\n"}, "not a chart name"},
		"library":              {map[string]string{"Chart.yaml": chartYAML + "type: library\n"}, "library chart"},
		"other type":           {map[string]string{"Chart.yaml": chartYAML + "type: plugin\n"}, "must be application"},
		"Chart.yaml not yaml":  {map[string]string{"Chart.yaml": "apiVersion: [v2\n"}, "Chart.yaml"},
		"Chart.yaml no JSON":   {map[string]string{"Chart.yaml": chartYAML + "annotations: {[a]: b}\n"}, "Chart.yaml: yaml: invalid map key"},
		"values a list":        {map[string]string{"Chart.yaml": chartYAML, "values.yaml": "- a\n"}, "must be a mapping"},
		"values reserved key":  {map[string]string{"Chart.yaml": chartYAML, "values.yaml": "stowage: {}\n"}, "reserved"},
		"dependency missing":   {map[string]string{"Chart.yaml": deps, "charts/db/Chart.yaml": "apiVersion: v2\nname: db\n"}, "cache are not vendored"},
		"dependency by dir":    {map[string]string{"Chart.yaml": deps, "charts/cache/Chart.yaml": "apiVersion: v2\nname: other\n", "charts/db-0.1.0.tgz": tgz("db")}, "cache are not vendored"},
		"bad vendored archive": {map[string]string{"Chart.yaml": chartYAML, "charts/db-0.1.0.tgz": "not gzip"}, "charts/db-0.1.0.tgz"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(chart(t, tc.files))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	// Positive control for the dependency cases: both vendored, one as a directory, one archived.
	if _, err := Load(chart(t, map[string]string{"Chart.yaml": deps, "charts/db/Chart.yaml": "apiVersion: v2\nname: db\n", "charts/cache-1.0.0.tgz": tgz("cache")})); err != nil {
		t.Fatalf("vendored dependencies refused: %v", err)
	}
}

func TestLoadRefusesSymlinksAndBigFiles(t *testing.T) {
	t.Parallel()
	dir := chart(t, map[string]string{"Chart.yaml": chartYAML, "templates/a.yaml": "a: 1\n"})
	if err := os.Symlink("a.yaml", filepath.Join(dir, "templates", "b.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "templates/b.yaml is a symlink") {
		t.Fatalf("symlink: %v", err)
	}
	// An ignored symlink is not packaged, so it is no concern.
	if err := os.WriteFile(filepath.Join(dir, ".helmignore"), []byte("templates/b.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err != nil {
		t.Fatalf("ignored symlink: %v", err)
	}

	big := filepath.Join(dir, "files", "big")
	if err := os.MkdirAll(filepath.Dir(big), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(big, make([]byte, maxFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err != nil {
		t.Fatalf("a file of the largest size: %v", err)
	}
	if err := os.WriteFile(big, make([]byte, maxFile+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "files/big is larger") {
		t.Fatalf("a file one byte too large: %v", err)
	}
}

// The block is appended as text, and checked: a values.yaml where appended text would not be a new
// top-level key is refused rather than published wrong — by Load, so lint reports it.
func TestLoadRefusesValuesItCannotExtend(t *testing.T) {
	t.Parallel()
	for name, own := range map[string]string{
		"flow mapping":    "{web: {replicas: 1}}\n",
		"second document": "web: 1\n---\nother: 2\n",
		"document end":    "web: 1\n...\n",
		"block scalar at the end without a newline": "web: |\n  text",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(chart(t, map[string]string{"Chart.yaml": chartYAML, "values.yaml": own}))
			if err == nil || !strings.Contains(err.Error(), "values.yaml: adding the stowage block") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// A chart's git data is never packaged, whether the chart is a repository (.git directory) or a
// submodule (.git file); a values.yaml holding NaN is extended like any other.
func TestLoadSkipsGitAndKeepsNaN(t *testing.T) {
	t.Parallel()
	for _, git := range []string{".git/HEAD", ".git"} {
		dir := chart(t, map[string]string{"Chart.yaml": chartYAML, "values.yaml": "threshold: .nan\n", git: "ref: refs/heads/main\n"})
		c, err := Load(dir)
		if err != nil {
			t.Fatalf("%s: %v", git, err)
		}
		for _, f := range c.Files {
			if strings.HasPrefix(f.Name, ".git") {
				t.Fatalf("%s: packaged %s", git, f.Name)
			}
		}
		if _, _, err := c.Package("0.1.0", "1.0.0", values); err != nil {
			t.Fatalf("NaN values: %v", err)
		}
	}
}
