package lint

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"regexp"
	"strings"

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
