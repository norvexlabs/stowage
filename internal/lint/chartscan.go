package lint

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/helm"
)

// httpRouting matches a template's kind key — at a line's start, in a flow mapping or JSON
// (`"kind": "Ingress"`), quoted or not — naming a resource that routes HTTP to the chart's Services
// by host (an Ingress, a Gateway API HTTPRoute or GRPCRoute), or one a template computes
// (`kind: {{ … }}`), which may be one.
var httpRouting = regexp.MustCompile(`(?m)(^|[\s{,])["']?kind["']?\s*:\s*(["']?(Ingress|HTTPRoute|GRPCRoute)["']?\s*([,}#]|$)|[^\n]*\{\{)`)

// maxSubchart bounds what is read of one vendored subchart archive and of each template in it.
const maxSubchart = 16 << 20

// routesHTTP reports whether the chart's templates, or a subchart's under charts/ (a directory or
// a vendored .tgz, nested ones included), may route HTTP to its Services (httpRouting). A subchart
// archive that cannot be read counts as one that may.
func routesHTTP(c *helm.Chart) bool {
	for _, f := range c.Files {
		switch {
		case isTemplate(f.Name):
			if httpRouting.Match(f.Data) {
				return true
			}
		case strings.HasPrefix(f.Name, "charts/") && strings.HasSuffix(f.Name, ".tgz"):
			if archiveRoutesHTTP(f.Data, 0) {
				return true
			}
		}
	}
	return false
}

// isTemplate is a chart file helm renders: under templates/, the chart's or a subchart's.
func isTemplate(name string) bool {
	return strings.HasPrefix(name, "templates/") || strings.HasPrefix(name, "charts/") && strings.Contains(name, "/templates/")
}

// archiveRoutesHTTP is routesHTTP for a packaged chart: <name>/templates/… and <name>/charts/….
func archiveRoutesHTTP(data []byte, depth int) bool {
	if depth > 4 {
		return true
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return true
	}
	tr := tar.NewReader(io.LimitReader(zr, maxSubchart))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return false
		}
		if err != nil {
			return true
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		_, inner, _ := strings.Cut(strings.TrimPrefix(h.Name, "./"), "/")
		isArchive := strings.HasPrefix(inner, "charts/") && strings.HasSuffix(inner, ".tgz")
		if !isTemplate(inner) && !isArchive {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxSubchart))
		if err != nil {
			return true
		}
		if isArchive && archiveRoutesHTTP(b, depth+1) || !isArchive && httpRouting.Match(b) {
			return true
		}
	}
}

// templateAction is a template action on one line, {{ … }}.
var templateAction = regexp.MustCompile(`\{\{.*?\}\}`)

// provisionConfigMapKeys are the data keys of a ConfigMap <release>-stowage-provision the chart's
// templates (a subchart's directory included) make, as far as they read without rendering: each
// action stands for a value, a line holding nothing but actions is dropped, and a key an action
// computes is not seen; a template that does not read so is skipped. Sorted.
func provisionConfigMapKeys(c *helm.Chart) []string {
	var keys []string
	for _, f := range c.Files {
		if !isTemplate(f.Name) || !bytes.Contains(f.Data, []byte(provisionConfigMap)) {
			continue
		}
		var lines []string
		for _, line := range strings.Split(string(f.Data), "\n") {
			bare := templateAction.ReplaceAllString(line, "")
			if bare != line && strings.TrimSpace(bare) == "" {
				continue
			}
			lines = append(lines, templateAction.ReplaceAllString(line, "x"))
		}
		d := yaml.NewDecoder(strings.NewReader(strings.Join(lines, "\n")))
		for {
			var doc struct {
				Kind     string `yaml:"kind"`
				Metadata struct {
					Name string `yaml:"name"`
				} `yaml:"metadata"`
				Data map[string]any `yaml:"data"`
			}
			if err := d.Decode(&doc); err != nil {
				break
			}
			if doc.Kind != "ConfigMap" || !strings.HasSuffix(doc.Metadata.Name, provisionConfigMap) {
				continue
			}
			for k := range doc.Data {
				if !slices.Contains(keys, k) {
					keys = append(keys, k)
				}
			}
		}
	}
	slices.Sort(keys)
	return keys
}

// provisionConfigMap ends the name of the ConfigMap a chart gives the provision hook through:
// <release>-stowage-provision.
const provisionConfigMap = "-stowage-provision"
