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

// routingKinds are the resources that route traffic to the chart's Services: an Ingress, the
// Gateway API's routes, and the routes of Traefik, Istio, OpenShift, Contour, NGINX Inc, Kong,
// APISIX, Emissary, Gloo, Skipper and Kuma.
var routingKinds = []string{
	"Ingress", "HTTPRoute", "GRPCRoute", "TLSRoute", "TCPRoute", "UDPRoute",
	"IngressRoute", "IngressRouteTCP", "IngressRouteUDP", "VirtualService", "Route", "HTTPProxy",
	"VirtualServer", "VirtualServerRoute", "TransportServer", "TCPIngress", "UDPIngress", "ApisixRoute", "Mapping", "RouteTable",
	"RouteGroup", "MeshHTTPRoute", "MeshTCPRoute", "MeshGRPCRoute", "MeshGatewayRoute", "TCPMapping",
}

// routingKind matches a template's kind key — at a line's start, in a flow mapping or JSON
// (`"kind": "Ingress"`), or after an action (`{{ if … }}kind:`), quoted or not, a YAML tag or
// anchor before its value — naming a routing resource (routingKinds), or one a template computes
// (`kind: {{ … }}`), which may be one; on one line.
var routingKind = regexp.MustCompile(`(?m)(^|[ \t{,}])["']?kind["']?[ \t]*:[ \t]*((!!\w+|&\w+)[ \t]+)?["']?((` + strings.Join(routingKinds, "|") + `)["']?([ \t]|[,}#]|\{\{|$)|\{\{)`)

// maxSubchart bounds what is read of one vendored subchart archive and of each template in it.
const maxSubchart = 16 << 20

// mayRoute reports whether a template may route to the chart's Services: outside its comments,
// it names a routing resource (routingKind).
func mayRoute(data []byte) bool {
	return routingKind.Match(uncommented(data))
}

// actionComment is a template comment, which may span lines; yamlComment, a YAML comment to its
// line's end (a # at the line's start or after a blank).
var (
	actionComment = regexp.MustCompile(`(?s)\{\{-?\s*/\*.*?\*/\s*-?\}\}`)
	yamlComment   = regexp.MustCompile(`(?m)(^|[ \t])#.*$`)
)

// uncommented is a template without its comments, a template's and YAML's.
func uncommented(data []byte) []byte {
	return yamlComment.ReplaceAll(actionComment.ReplaceAll(data, nil), []byte("$1"))
}

// chartMayRoute reports whether the chart's templates, or a subchart's under charts/ (a directory
// or a vendored .tgz, nested ones included), may route to its Services (mayRoute). A subchart
// archive that cannot be read counts as one that may.
func chartMayRoute(c *helm.Chart) bool {
	for _, f := range c.Files {
		switch {
		case isTemplate(f.Name):
			if mayRoute(f.Data) {
				return true
			}
		case strings.HasPrefix(f.Name, "charts/") && strings.HasSuffix(f.Name, ".tgz"):
			if archiveMayRoute(f.Data, 0) {
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

// archiveMayRoute is chartMayRoute for a packaged chart: <name>/templates/… and <name>/charts/….
func archiveMayRoute(data []byte, depth int) bool {
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
		if isArchive && archiveMayRoute(b, depth+1) || !isArchive && mayRoute(b) {
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
