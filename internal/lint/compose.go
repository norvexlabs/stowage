package lint

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/docker/go-units"
	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/spec"
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
	// Extends is `extends: base` or `extends: {service: base, file: …}`; after inherit, Profiles,
	// Ports, Volumes and Devices hold the base's too, and NetworkMode is the base's unless set.
	Extends     any    `yaml:"extends"`
	Ports       any    `yaml:"ports"`
	Volumes     any    `yaml:"volumes"`
	Devices     any    `yaml:"devices"`
	NetworkMode string `yaml:"network_mode"`
	// Privileged, CapAdd and Pid are what the service is given of the host's powers.
	Privileged any    `yaml:"privileged"`
	CapAdd     any    `yaml:"cap_add"`
	Pid        string `yaml:"pid"`
	// MemLimit, MemReservation, Scale and Deploy are what the service sets of its memory and its
	// number of containers.
	MemLimit       any `yaml:"mem_limit"`
	MemReservation any `yaml:"mem_reservation"`
	Scale          any `yaml:"scale"`
	Deploy         struct {
		Replicas  any `yaml:"replicas"`
		Resources struct {
			Limits struct {
				Memory any `yaml:"memory"`
			} `yaml:"limits"`
			Reservations struct {
				Memory any `yaml:"memory"`
			} `yaml:"reservations"`
		} `yaml:"resources"`
	} `yaml:"deploy"`
}

// MemFloor is the least memory limit each of the service's containers gets whatever its share:
// the largest of the limits and reservations it writes as a literal size (one from a variable
// only a run knows), and its number of containers (scale, deploy.replicas; 1 when unset or from a
// variable).
func (s Service) MemFloor() (floor spec.Bytes, containers int) {
	for _, v := range []any{s.MemLimit, s.MemReservation, s.Deploy.Resources.Limits.Memory, s.Deploy.Resources.Reservations.Memory} {
		if b, ok := composeSize(v); ok {
			floor = max(floor, b)
		}
	}
	containers = 1
	for _, v := range []any{s.Scale, s.Deploy.Replicas} {
		n, ok := v.(int)
		if q, isText := v.(string); isText {
			var err error
			n, err = strconv.Atoi(q)
			ok = err == nil
		}
		if ok {
			containers = max(containers, n)
		}
	}
	return floor, containers
}

// composeSize reads a size as Compose does (go-units' RAMInBytes): a whole number of bytes, or
// text such as 512m or 1.5GB.
func composeSize(v any) (spec.Bytes, bool) {
	switch n := v.(type) {
	case int:
		return spec.Bytes(n), n >= 0
	case string:
		b, err := units.RAMInBytes(n)
		return spec.Bytes(b), err == nil && b >= 0
	}
	return 0, false
}

// inherit fills each service's image, build, network mode, privileged, pid and memory settings
// from the service it extends, where it sets none, and joins its profiles, ports, volumes, devices
// and cap_add with the base's, as compose does. It refuses an extends compose would resolve outside this file (a
// file the release does not carry), a service the file does not declare, and a cycle.
func (c *Compose) inherit() map[string]error {
	errs := map[string]error{}
	base := map[string]string{}
	for name, svc := range c.Services {
		var from string
		switch e := svc.Extends.(type) {
		case nil:
			continue
		case string:
			from = e
		case map[string]any:
			if _, ok := e["file"]; ok {
				errs[name] = errors.New("file: names another compose file, which the release does not carry: extend a service of this file")
				continue
			}
			from, _ = e["service"].(string)
		}
		switch _, ok := c.Services[from]; {
		case from == "":
			errs[name] = errors.New("expected a service name, or {service: <name>}")
		case !ok:
			errs[name] = fmt.Errorf("extends %q, which is not a service of this file", from)
		default:
			base[name] = from
		}
	}
	// A loop is reported once, on its first service in name order.
	for _, name := range sortedKeys(base) {
		path := []string{name}
		for from := base[name]; from != ""; from = base[from] {
			if i := slices.Index(path, from); i >= 0 {
				loop := path[i:]
				first := slices.Index(loop, slices.Min(loop))
				loop = append(slices.Clone(loop[first:]), loop[:first]...)
				errs[loop[0]] = fmt.Errorf("extends loops: %s", strings.Join(append(loop, loop[0]), " → "))
				for _, n := range loop {
					delete(base, n) // nothing inherits around a loop
				}
				break
			}
			path = append(path, from)
		}
	}
	var resolve func(name string) Service
	resolve = func(name string) Service {
		svc := c.Services[name]
		from, ok := base[name]
		if !ok {
			return svc // nothing to inherit, or an error its own extends reports
		}
		b := resolve(from)
		if svc.Image == "" {
			svc.Image = b.Image
		}
		if svc.Build == nil {
			svc.Build = b.Build
		}
		if svc.NetworkMode == "" {
			svc.NetworkMode = b.NetworkMode
		}
		if svc.Privileged == nil {
			svc.Privileged = b.Privileged
		}
		if svc.Pid == "" {
			svc.Pid = b.Pid
		}
		svc.CapAdd = joinList(svc.CapAdd, b.CapAdd)
		// The memory a share's maximum is checked against: each setting the service leaves unset.
		for _, f := range []struct{ own, base *any }{
			{&svc.MemLimit, &b.MemLimit}, {&svc.MemReservation, &b.MemReservation}, {&svc.Scale, &b.Scale},
			{&svc.Deploy.Replicas, &b.Deploy.Replicas}, {&svc.Deploy.Resources.Limits.Memory, &b.Deploy.Resources.Limits.Memory},
			{&svc.Deploy.Resources.Reservations.Memory, &b.Deploy.Resources.Reservations.Memory},
		} {
			if *f.own == nil {
				*f.own = *f.base
			}
		}
		// Compose joins the lists: the service's profiles, ports, volumes and devices are its own
		// and its base's.
		for _, p := range b.Profiles {
			if !slices.Contains(svc.Profiles, p) {
				svc.Profiles = append(slices.Clone(svc.Profiles), p)
			}
		}
		svc.Ports, svc.Volumes, svc.Devices = joinList(svc.Ports, b.Ports), joinList(svc.Volumes, b.Volumes), joinList(svc.Devices, b.Devices)
		return svc
	}
	out := map[string]Service{}
	for name := range c.Services {
		out[name] = resolve(name)
	}
	c.Services = out
	return errs
}

// mountsVolume reports whether a service's volumes: mount the named volume key, in the short
// syntax (key:/path[:mode]) or the long one ({type: volume, source: key}).
func mountsVolume(vols any, key string) bool {
	list, _ := vols.([]any)
	for _, m := range list {
		switch m := m.(type) {
		case string:
			if src, _, ok := strings.Cut(m, ":"); ok && src == key {
				return true
			}
		case map[string]any:
			if t, _ := m["type"].(string); (t == "" || t == "volume") && m["source"] == key {
				return true
			}
		}
	}
	return false
}

// dockerSockets are where a host's Docker engine listens: a container mounting one drives the
// engine, so holds the host.
var dockerSockets = []string{"/var/run/docker.sock", "/run/docker.sock"}

// MountsDockerSocket is whether vols bind-mounts the host's Docker socket, or a directory
// holding it (/var/run, /run, /).
func MountsDockerSocket(vols any) bool {
	list, _ := vols.([]any)
	for _, m := range list {
		var src string
		switch m := m.(type) {
		case string:
			src = shortSource(m)
		case map[string]any:
			// A bind, or an entry without a type whose source is a path, as compose reads it.
			if t, _ := m["type"].(string); t == "bind" || t == "" {
				src, _ = m["source"].(string)
			}
		}
		if !strings.HasPrefix(src, "/") {
			continue // a named volume, or a path lint cannot read
		}
		src = path.Clean(src)
		// The socket, or a directory holding it.
		if slices.ContainsFunc(dockerSockets, func(s string) bool { return s == src || strings.HasPrefix(s, strings.TrimSuffix(src, "/")+"/") }) {
			return true
		}
	}
	return false
}

// shortSource is the host source of a short-syntax volume, SOURCE:TARGET[:MODE]; a source
// ${VAR:-default} or ${VAR-default} reads as its default. Empty for a bare TARGET (an anonymous
// volume) or a source lint cannot read.
func shortSource(v string) string {
	if strings.HasPrefix(v, "${") {
		end := strings.Index(v, "}")
		if end < 0 || !strings.HasPrefix(v[end+1:], ":") {
			return ""
		}
		// A variable's name holds no '-': the first one starts its default (after ':' or not).
		_, def, ok := strings.Cut(v[2:end], "-")
		if !ok {
			return ""
		}
		return def
	}
	src, _, ok := strings.Cut(v, ":")
	if !ok {
		return ""
	}
	return src
}

// joinCaps is a cap_add list joined with ", ", or its value as written when it is no list.
func joinCaps(v any) string {
	list, ok := v.([]any)
	if !ok {
		if v == nil {
			return ""
		}
		return fmt.Sprint(v)
	}
	var caps []string
	for _, c := range list {
		caps = append(caps, fmt.Sprint(c))
	}
	return strings.Join(caps, ", ")
}

// joinList is a service's list joined with its base's; one that is no list is kept as an entry,
// which no reader takes for a valid one.
func joinList(own, base any) any {
	o, ownList := own.([]any)
	b, baseList := base.([]any)
	switch {
	case own == nil:
		return base
	case ownList && baseList:
		return append(slices.Clone(o), b...)
	case base != nil:
		return []any{own, base}
	}
	return own
}

// PublishesBeyondLoopback reports whether a service's ports: publish one past this host: any
// entry not bound to a loopback address or to ${STOWAGE_LOOPBACK_ADDRESS} (a bare port is
// published on every address; an entry it cannot read counts as published).
func PublishesBeyondLoopback(ports any) bool {
	list, ok := ports.([]any)
	if !ok {
		return ports != nil
	}
	loopback := func(host string) bool {
		// The variable, which is always recorded: a default or an error word never applies, an
		// alternate value (:+, +) would.
		if host == "${STOWAGE_LOOPBACK_ADDRESS}" || host == "$STOWAGE_LOOPBACK_ADDRESS" {
			return true
		}
		for _, op := range []string{":-", "-", ":?", "?"} {
			if strings.HasPrefix(host, "${STOWAGE_LOOPBACK_ADDRESS"+op) && strings.HasSuffix(host, "}") {
				return true
			}
		}
		a, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
		return err == nil && a.IsLoopback()
	}
	for _, p := range list {
		switch p := p.(type) {
		case string:
			var host string
			if rest, ok := strings.CutPrefix(p, "["); ok {
				if h, ports, closed := strings.Cut(rest, "]"); closed {
					if f := portFields(ports); len(f) == 3 && f[0] == "" {
						host = h
					}
				}
			} else if parts := portFields(p); len(parts) >= 3 {
				host = strings.Join(parts[:len(parts)-2], ":") // a bare IPv6 address holds colons too
			}
			if !loopback(host) {
				return true
			}
		case map[string]any:
			host, _ := p["host_ip"].(string)
			if !loopback(host) {
				return true
			}
		default:
			return true
		}
	}
	return false
}

// CheckL4Address accepts what an L4 route's endpoint is made from: nothing, an IP address (no
// zone), or a lowercase host name whose last label is not all digits (192.168.1 is no address) —
// never a port or a scheme, which the endpoint adds or lacks.
func CheckL4Address(v string) error {
	if v == "" {
		return nil
	}
	if a, err := netip.ParseAddr(v); err == nil {
		if a.Zone() != "" {
			return fmt.Errorf("%q: an address clients dial carries no zone", v)
		}
		return nil
	}
	labels := strings.Split(v, ".")
	if len(v) > 253 || strings.Trim(labels[len(labels)-1], "0123456789") == "" || slices.ContainsFunc(labels, func(l string) bool { return !hostLabel.MatchString(l) }) {
		return fmt.Errorf("%q is not a host name or an IP address (lowercase, no port: like data.example.com or 192.0.2.7)", v)
	}
	return nil
}

// CheckL4Bind accepts the host address an L4 route's port is published on: nothing (every
// address) or an IP address of the host, without a zone.
func CheckL4Bind(v string) error {
	if v == "" {
		return nil
	}
	a, err := netip.ParseAddr(v)
	if err != nil || a.Zone() != "" {
		return fmt.Errorf("%q is not an IP address without a zone (like 192.0.2.7 or 2001:db8::7)", v)
	}
	return nil
}

// hostLabel is a label of a lowercase DNS name.
var hostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// portFields splits a short-syntax port entry at its colons, leaving those inside a ${…}
// interpolation (`${PORT:-5432}`) in their field.
func portFields(p string) []string {
	var fields []string
	depth, start := 0, 0
	for i := 0; i < len(p); i++ {
		switch {
		case strings.HasPrefix(p[i:], "${"):
			depth++
			i++
		case p[i] == '}' && depth > 0:
			depth--
		case p[i] == ':' && depth == 0:
			fields = append(fields, p[start:i])
			start = i + 1
		}
	}
	return append(fields, p[start:])
}

// ParseCompose reads a compose file and collects every interpolation in its keys and scalar
// values — the strings compose interpolates. Comments are not values, so they are never read; a
// `#` inside a block scalar is text, so it is.
func ParseCompose(path string) (*Compose, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseCompose(raw, path)
}

// ServiceProfiles are the profiles of each service of a compose file that has some, inherited
// through extends as compose does.
func ServiceProfiles(raw []byte) (map[string][]string, error) {
	c, err := parseCompose(raw, "compose.yaml")
	if err != nil {
		return nil, err
	}
	if errs := c.inherit(); len(errs) > 0 {
		name := sortedKeys(errs)[0]
		return nil, fmt.Errorf("compose.yaml: services.%s.extends: %w", name, errs[name])
	}
	p := map[string][]string{}
	for name, svc := range c.Services {
		if len(svc.Profiles) > 0 {
			p[name] = svc.Profiles
		}
	}
	return p, nil
}

// ServiceDevices maps each service of a compose file mapping host devices (devices:, its own and
// those it extends) to their host paths, in the order it lists them.
func ServiceDevices(raw []byte) (map[string][]string, error) {
	c, err := parseCompose(raw, "compose.yaml")
	if err != nil {
		return nil, err
	}
	if errs := c.inherit(); len(errs) > 0 {
		name := sortedKeys(errs)[0]
		return nil, fmt.Errorf("compose.yaml: services.%s.extends: %w", name, errs[name])
	}
	out := map[string][]string{}
	for name, svc := range c.Services {
		list, _ := svc.Devices.([]any)
		for _, d := range list {
			var host string
			switch d := d.(type) {
			case string:
				host, _, _ = strings.Cut(d, ":")
			case map[string]any:
				host, _ = d["source"].(string)
			}
			if host != "" {
				out[name] = append(out[name], host)
			}
		}
	}
	return out, nil
}

func parseCompose(raw []byte, path string) (*Compose, error) {
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
	"STOWAGE_TLS_MODE":         "how the front serves: none (loopback), acme, dns-cloudflare, byo or internal (never set without a front)",
}

// privilegedText is whether a privileged: compose reads as a string may turn it on: "true", or an
// interpolation lint cannot read.
func privilegedText(v any) bool {
	s, ok := v.(string)
	return ok && (strings.EqualFold(strings.TrimSpace(s), "true") || strings.Contains(s, "${"))
}
