// Package helm packages a project's Helm chart for a release, as `helm package` would, without the
// helm binary: the chart directory less what its .helmignore excludes, Chart.yaml's version and
// appVersion set, and the release's values block added to values.yaml. The archive is a function
// of its content alone (no timestamp), so the same release makes the same chart digest.
package helm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
	"helm.sh/helm/v4/pkg/ignore"
)

// Media types of a chart in an OCI registry, as helm pushes and pulls it.
const (
	ConfigMediaType  = "application/vnd.cncf.helm.config.v1+json"
	ContentMediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
)

const chartFile = "Chart.yaml"

// ValuesKey is the top-level key of values.yaml that ship writes; a chart must not define it.
const ValuesKey = "stowage"

// maxFile is the largest file helm loads from a chart archive.
const maxFile = 5 << 20

var bom = []byte{0xEF, 0xBB, 0xBF}

// File is one file of a chart, by its slash-separated path inside the chart directory.
type File struct {
	Name string
	Data []byte
}

// Chart is a chart directory as it is packaged: its files sorted by name, .helmignore applied.
type Chart struct {
	Name  string
	Files []File
}

// meta is the part of Chart.yaml Stowage checks.
type meta struct {
	APIVersion   string `yaml:"apiVersion"`
	Name         string `yaml:"name"`
	Type         string `yaml:"type"`
	Dependencies []struct {
		Name string `yaml:"name"`
	} `yaml:"dependencies"`
}

// Load reads the chart in dir and checks it can be packaged: Chart.yaml is an apiVersion v2
// application chart with a name, values.yaml (when present) is a mapping without ValuesKey that the
// values block can be appended to, every dependency is vendored under charts/, and every file is a
// regular file (a symlink is refused: what is packaged is what the release's inputs digest hashed).
// A .git entry is never read.
func Load(dir string) (*Chart, error) {
	rules := ignore.Empty()
	if f := filepath.Join(dir, ignore.HelmIgnore); fileExists(f) {
		r, err := ignore.ParseFile(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ignore.HelmIgnore, err)
		}
		rules = r
	}
	rules.AddDefaults()
	var files []File
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		n := filepath.ToSlash(rel)
		// A git directory (or the file a submodule or worktree holds in its place) is never
		// packaged: it would publish the chart's history.
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if rules.Ignore(n, fi) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case d.IsDir():
			return nil
		case fi.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s is a symlink: a chart is packaged from regular files only", n)
		case !fi.Mode().IsRegular():
			return fmt.Errorf("%s is not a regular file", n)
		case fi.Size() > maxFile:
			return fmt.Errorf("%s is larger than %d bytes, the most helm loads", n, maxFile)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files = append(files, File{Name: n, Data: bytes.TrimPrefix(data, bom)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	c := &Chart{Files: files}
	raw, ok := c.file(chartFile)
	if !ok {
		return nil, fmt.Errorf("%s is missing", chartFile)
	}
	var m meta
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", chartFile, err)
	}
	switch {
	case m.APIVersion != "v2":
		return nil, fmt.Errorf("%s: apiVersion is %q; Stowage packages apiVersion v2 charts (Helm 3 and later)", chartFile, m.APIVersion)
	case m.Name == "" || m.Name == "." || m.Name == ".." || strings.ContainsAny(m.Name, `/\`):
		return nil, fmt.Errorf("%s: name %q is not a chart name", chartFile, m.Name)
	case m.Type == "library":
		return nil, fmt.Errorf("%s: a library chart cannot be installed", chartFile)
	case m.Type != "" && m.Type != "application":
		return nil, fmt.Errorf("%s: type %q must be application", chartFile, m.Type)
	}
	c.Name = m.Name
	vendored, err := c.vendored()
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, d := range m.Dependencies {
		if !vendored[d.Name] {
			missing = append(missing, d.Name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s: dependencies %s are not vendored under charts/ (run helm dependency build and commit charts/)", chartFile, strings.Join(missing, ", "))
	}
	// What Package refuses (a Chart.yaml with no JSON form, a values.yaml the block cannot be
	// appended to) is refused here, so lint reports it before a ship.
	if _, _, err := c.chartYAML("0.1.0", "0"); err != nil {
		return nil, err
	}
	if _, err := c.withValues(Values{}); err != nil {
		return nil, err
	}
	return c, nil
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func (c *Chart) file(name string) ([]byte, bool) {
	for _, f := range c.Files {
		if f.Name == name {
			return f.Data, true
		}
	}
	return nil, false
}

// values decodes values.yaml: a mapping (nil when the file is absent or empty) without ValuesKey.
func (c *Chart) values() (map[string]any, error) {
	raw, ok := c.file("values.yaml")
	if !ok {
		return nil, nil
	}
	var v map[string]any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("values.yaml must be a mapping: %w", err)
	}
	if _, ok := v[ValuesKey]; ok {
		return nil, fmt.Errorf("values.yaml: the key %s is reserved: ship writes it", ValuesKey)
	}
	return v, nil
}

// vendored names the charts under charts/: directories holding a Chart.yaml and .tgz archives,
// by the name their Chart.yaml gives, as helm matches dependencies.
func (c *Chart) vendored() (map[string]bool, error) {
	names := map[string]bool{}
	for _, f := range c.Files {
		rest, ok := strings.CutPrefix(f.Name, "charts/")
		if !ok {
			continue
		}
		var raw []byte
		switch dir, file, nested := strings.Cut(rest, "/"); {
		case nested && file == chartFile && dir != "":
			raw = f.Data
		case !nested && strings.HasSuffix(rest, ".tgz"):
			var err error
			if raw, err = archivedChartYAML(f.Data); err != nil {
				return nil, fmt.Errorf("%s: %w", f.Name, err)
			}
		default:
			continue
		}
		var m meta
		if err := yaml.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("%s: Chart.yaml: %w", f.Name, err)
		}
		names[m.Name] = true
	}
	return names, nil
}

// archivedChartYAML reads <chart>/Chart.yaml from a packaged chart.
func archivedChartYAML(tgz []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("no Chart.yaml")
		}
		if err != nil {
			return nil, err
		}
		if dir, file, ok := strings.Cut(h.Name, "/"); ok && dir != "" && file == chartFile {
			return io.ReadAll(io.LimitReader(tr, maxFile))
		}
	}
}

// Values is the release's block of values.yaml, under ValuesKey. A chart's templates name an
// image as {{ .Values.stowage.registry }}/{{ $img.repository }}@{{ $img.digest }}.
type Values struct {
	// Registry is the project's registry prefix; an operator mirroring the release sets it.
	Registry string           `yaml:"registry"`
	Release  Release          `yaml:"release"`
	Images   map[string]Image `yaml:"images"`
	// PublicKeys are the trusted release keys (PEM), when the project signs.
	PublicKeys []string `yaml:"publicKeys,omitempty"`
	// L4 are the project's L4 routes, for the chart to publish as it chooses (a Service each).
	L4 []L4Route `yaml:"l4,omitempty"`
}

// L4Route is an L4 route: its protocol (TCP or UDP, as a Service names it), its port, and the
// compose service and port it reaches.
type L4Route struct {
	Name       string `yaml:"name"`
	Protocol   string `yaml:"protocol"`
	Port       int    `yaml:"port"`
	Service    string `yaml:"service"`
	TargetPort int    `yaml:"targetPort"`
}

// Release identifies the release a chart belongs to.
type Release struct {
	Name     string `yaml:"name"`
	Version  string `yaml:"version"`
	Revision int    `yaml:"revision"`
	Channel  string `yaml:"channel"`
	Tag      string `yaml:"tag"`
}

// Image is one published image: its repository under the registry prefix, and its digest.
type Image struct {
	Repository string `yaml:"repository"`
	Digest     string `yaml:"digest"`
}

// Package returns the chart archive with Chart.yaml's version and appVersion set and the values
// block added to values.yaml, and the chart's OCI config (Chart.yaml as JSON, as helm pushes it).
func (c *Chart) Package(version, appVersion string, v Values) (archive, config []byte, err error) {
	chartYAML, cfg, err := c.chartYAML(version, appVersion)
	if err != nil {
		return nil, nil, err
	}
	values, err := c.withValues(v)
	if err != nil {
		return nil, nil, err
	}
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	put := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: path.Join(c.Name, name), Mode: 0o644, Size: int64(len(data)),
			Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	wroteValues := false
	for _, f := range c.Files {
		data := f.Data
		switch f.Name {
		case chartFile:
			data = chartYAML
		case "values.yaml":
			data, wroteValues = values, true
		}
		if err := put(f.Name, data); err != nil {
			return nil, nil, err
		}
	}
	if !wroteValues {
		if err := put("values.yaml", values); err != nil {
			return nil, nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, nil, err
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(tb.Bytes()); err != nil {
		return nil, nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, nil, err
	}
	return gz.Bytes(), cfg, nil
}

// chartYAML sets version and appVersion in Chart.yaml (comments and the rest kept) and returns it
// with its JSON form.
func (c *Chart) chartYAML(version, appVersion string) ([]byte, []byte, error) {
	raw, _ := c.file(chartFile)
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", chartFile, err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("%s: not a mapping", chartFile)
	}
	root := doc.Content[0]
	set := func(key, value string) {
		v := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: yaml.DoubleQuotedStyle}
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == key {
				root.Content[i+1] = v
				return
			}
		}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	}
	set("version", version)
	set("appVersion", appVersion)
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(out.Bytes(), &m); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", chartFile, err)
	}
	cfg, err := json.Marshal(m)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", chartFile, err)
	}
	return out.Bytes(), cfg, nil
}

// withValues appends the values block to values.yaml as text, so the chart's own file is kept byte
// for byte, then checks the result reads as the chart's values plus the block alone.
func (c *Chart) withValues(v Values) ([]byte, error) {
	before, err := c.values()
	if err != nil {
		return nil, err
	}
	block, err := yaml.Marshal(map[string]Values{ValuesKey: v})
	if err != nil {
		return nil, fmt.Errorf("values.yaml: the %s block: %w", ValuesKey, err)
	}
	raw, _ := c.file("values.yaml")
	out := append([]byte(nil), raw...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, "\n# Written by stowage ship: the release's images by digest.\n"...)
	out = append(out, block...)

	var after, want map[string]any
	if err := yaml.Unmarshal(out, &after); err != nil {
		return nil, fmt.Errorf("values.yaml: cannot add the %s block: %w", ValuesKey, err)
	}
	if err := yaml.Unmarshal(block, &want); err != nil {
		return nil, err
	}
	// Compared by their YAML form, in which NaN equals itself.
	var merr error
	equal := func(a, b any) bool {
		x, errx := yaml.Marshal(a)
		y, erry := yaml.Marshal(b)
		if merr == nil {
			merr = errors.Join(errx, erry)
		}
		return bytes.Equal(x, y)
	}
	same := len(after) == len(before)+1 && equal(after[ValuesKey], want[ValuesKey])
	for k, val := range before {
		_, kept := after[k]
		same = same && kept && equal(after[k], val)
	}
	if merr != nil {
		return nil, fmt.Errorf("values.yaml: %w", merr)
	}
	if !same {
		return nil, fmt.Errorf("values.yaml: adding the %s block at its end would change the chart's own values: write it as one block-style mapping document ending with a newline", ValuesKey)
	}
	return out, nil
}
