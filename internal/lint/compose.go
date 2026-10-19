package lint

import (
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

// VarRef is one interpolation in the compose file: `${NAME}`, `${NAME<op>word}` or `$NAME`.
type VarRef struct {
	Name string
	// HasDefault is set for `${VAR:-x}` / `${VAR-x}`: compose never fails on it unset.
	HasDefault bool
	// Op is ":-", "-", ":?", "?", ":+", "+" or ""; Default is the word after it, as written
	// (it may itself hold interpolations, which are reported as their own VarRefs).
	Op      string
	Default string
	Line    int
}

// Compose is what the linter needs from the project's compose file.
type Compose struct {
	TopLevel map[string]any
	Services map[string]Service
	Vars     []VarRef
}

// Service is the part of a compose service the linter reads.
type Service struct {
	Image    string   `yaml:"image"`
	Build    any      `yaml:"build"`
	Profiles []string `yaml:"profiles"`
}

// ParseCompose reads a compose file and collects every interpolation in its keys and scalar
// values — the strings compose interpolates. Comments are not values, so they are never read; a
// `#` inside a block scalar is text, so it is.
func ParseCompose(path string) (*Compose, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Compose{}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := root.Decode(&c.TopLevel); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var doc struct {
		Services map[string]Service `yaml:"services"`
	}
	if err := root.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Services = doc.Services
	var walk func(n *yaml.Node) error
	walk = func(n *yaml.Node) error {
		if n.Kind == yaml.ScalarNode {
			refs, err := Interpolations(n.Value)
			if err != nil {
				return fmt.Errorf("%s:%d: %w", path, n.Line, err)
			}
			for _, r := range refs {
				r.Line = n.Line
				c.Vars = append(c.Vars, r)
			}
		}
		for _, ch := range n.Content {
			if err := walk(ch); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(&root); err != nil {
		return nil, err
	}
	return c, nil
}

func isNameStart(b byte) bool { return b == '_' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' }
func isNameChar(b byte) bool  { return isNameStart(b) || b >= '0' && b <= '9' }

// Interpolations parses compose interpolation in s: `$$` is a literal dollar, `$NAME` and
// `${NAME}` substitute, `${NAME<op>word}` substitutes with a modifier (`:-`, `-`, `:?`, `?`, `:+`,
// `+`), and the word may nest further interpolations. An unterminated or malformed `${` is an
// error, as compose refuses it too.
func Interpolations(s string) ([]VarRef, error) {
	var out []VarRef
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) {
			continue
		}
		switch next := s[i+1]; {
		case next == '$':
			i++
		case isNameStart(next):
			j := i + 1
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			out = append(out, VarRef{Name: s[i+1 : j]})
			i = j - 1
		case next == '{':
			end, err := closingBrace(s, i+2)
			if err != nil {
				return nil, err
			}
			ref, nested, err := braced(s[i+2 : end])
			if err != nil {
				return nil, err
			}
			out = append(out, ref)
			out = append(out, nested...)
			i = end
		}
	}
	return out, nil
}

// closingBrace finds the '}' closing a `${` whose body starts at from, counting nested `${`.
func closingBrace(s string, from int) (int, error) {
	depth := 0
	for j := from; j < len(s); j++ {
		switch {
		case s[j] == '$' && j+1 < len(s) && s[j+1] == '$':
			j++
		case s[j] == '$' && j+1 < len(s) && s[j+1] == '{':
			depth++
			j++
		case s[j] == '}':
			if depth == 0 {
				return j, nil
			}
			depth--
		}
	}
	return 0, fmt.Errorf("unterminated ${ in %q", s)
}

func braced(body string) (VarRef, []VarRef, error) {
	if body == "" || !isNameStart(body[0]) {
		return VarRef{}, nil, fmt.Errorf("invalid interpolation ${%s}", body)
	}
	j := 0
	for j < len(body) && isNameChar(body[j]) {
		j++
	}
	ref := VarRef{Name: body[:j]}
	rest := body[j:]
	if rest == "" {
		return ref, nil, nil
	}
	for _, op := range []string{":-", ":?", ":+", "-", "?", "+"} {
		if strings.HasPrefix(rest, op) {
			ref.Op, ref.Default = op, rest[len(op):]
			ref.HasDefault = op == ":-" || op == "-"
			nested, err := Interpolations(ref.Default)
			if err != nil {
				return VarRef{}, nil, err
			}
			return ref, nested, nil
		}
	}
	return VarRef{}, nil, fmt.Errorf("invalid interpolation ${%s}", body)
}

// NeedsValue reports whether compose needs the variable set: `${X}` and `$X` would run with an empty
// value and `${X:?msg}` / `${X?msg}` fail; `${X:-v}`, `${X-v}`, `${X:+v}` and `${X+v}` are defined for
// an unset X.
func (v VarRef) NeedsValue() bool { return v.Op == "" || v.Op == ":?" || v.Op == "?" }

// ImageVar is the variable Stowage sets to an image's pinned reference (`<prefix>/<name>-<key>@
// sha256:…`); compose names its images only through these.
func ImageVar(key string) string {
	return "STOWAGE_IMAGE_" + strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
}

// ProvidedVars are the variables Stowage itself sets on every shape, besides the image variables.
var ProvidedVars = map[string]string{
	"STOWAGE_NAME":             "the project name",
	"STOWAGE_PROJECT_VERSION":  "the running project version",
	"STOWAGE_REVISION":         "the running package revision",
	"STOWAGE_LOOPBACK_ADDRESS": "a loopback address, 127.x or ::1 (in loopback mode, the one the front's ports bind to)",
	"STOWAGE_IPV6":             "true with --ipv6: IPv6 on the stack's network",
	"STOWAGE_TLS_MODE":         "how the front serves: none (loopback), acme, dns-cloudflare, byo or internal",
}
