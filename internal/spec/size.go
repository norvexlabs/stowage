package spec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Bytes is a size in bytes, written as a number of bytes or with a binary unit: 512M, 1.5G, 16Gi
// and 16GiB are all powers of 1024, a fraction of a byte dropped.
type Bytes int64

var bytesRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*(?:([kKmMgGtTpP])[iI]?[bB]?|[bB])?$`)

// ParseBytes reads a size as Bytes reads it.
func ParseBytes(s string) (Bytes, error) {
	m := bytesRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("%q: a size, e.g. 512M or 16G", s)
	}
	shift := map[string]uint{"": 0, "k": 10, "m": 20, "g": 30, "t": 40, "p": 50}[strings.ToLower(m[2])]
	whole, frac, _ := strings.Cut(m[1], ".")
	n, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || n > math.MaxInt64>>shift {
		return 0, fmt.Errorf("%q: too large", s)
	}
	b := n << shift
	if frac != "" {
		// Exact: a float would round 0.99999999999999999 up to a whole unit.
		r, _ := new(big.Rat).SetString("0." + frac)
		r.Mul(r, new(big.Rat).SetInt64(int64(1)<<shift))
		part := new(big.Int).Quo(r.Num(), r.Denom()).Int64()
		if b > math.MaxInt64-part {
			return 0, fmt.Errorf("%q: too large", s)
		}
		b += part
	}
	return Bytes(b), nil
}

func (b *Bytes) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: a size, e.g. 512M or 16G", n.Line)
	}
	v, err := ParseBytes(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*b = v
	return nil
}

// MiB is the size in whole MiB, rounded down.
func (b Bytes) MiB() int64 { return int64(b) >> 20 }

// String renders the size in the largest binary unit it is a whole number of.
func (b Bytes) String() string {
	for _, u := range []struct {
		shift uint
		name  string
	}{{50, "P"}, {40, "T"}, {30, "G"}, {20, "M"}, {10, "K"}} {
		if b != 0 && b%(1<<u.shift) == 0 {
			return strconv.FormatInt(int64(b)>>u.shift, 10) + u.name
		}
	}
	return strconv.FormatInt(int64(b), 10)
}

// Share is a service's part of the host's RAM: Percent of it, capped at Max when Max is set.
// Written as the percent alone, or as { percent: N, max: 16G }.
type Share struct {
	Percent int
	Max     Bytes
}

func (s *Share) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*s = Share{}
		return n.Decode(&s.Percent)
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: a percent, or { percent: N, max: 16G }", n.Line)
	}
	if err := knownKeys(n, "a memory share", "percent", "max"); err != nil {
		return err
	}
	var v struct {
		Percent int   `yaml:"percent"`
		Max     Bytes `yaml:"max"`
	}
	if err := n.Decode(&v); err != nil {
		return err
	}
	if k := key(n, "max"); k != nil && v.Max == 0 {
		return fmt.Errorf("line %d: max: %s: a size of at least %s; leave max out for none", k.Line, shown(k), MinMemLimit)
	}
	*s = Share{Percent: v.Percent, Max: v.Max}
	return nil
}

// MarshalJSON writes a share without a maximum as its percent alone: the spec's digest is then
// the percent's alone.
func (s Share) MarshalJSON() ([]byte, error) {
	if s.Max == 0 {
		return json.Marshal(s.Percent)
	}
	return json.Marshal(struct{ Percent, Max int64 }{int64(s.Percent), int64(s.Max)})
}

func (s *Share) UnmarshalJSON(raw []byte) error {
	if err := json.Unmarshal(raw, &s.Percent); err == nil {
		s.Max = 0
		return nil
	}
	var v struct{ Percent, Max int64 }
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("a memory share: %w", err)
	}
	*s = Share{Percent: int(v.Percent), Max: Bytes(v.Max)}
	return nil
}

// The least size `vm create` makes a VM with: the memory Kairos and the stack's engine need, and the
// app's own space on its disk (Kairos's is added to it).
const (
	MinVMMemory Bytes = 1 << 30
	MinVMDisk   Bytes = 4 << 30
)

// VMSize is the size a VM of the Kairos shape needs (Min) and runs well at (Recommended).
type VMSize struct {
	Min         *Machine `yaml:"min"`
	Recommended *Machine `yaml:"recommended"`
}

// Machine is a VM's size; a field left out is not stated, and one given is never 0.
type Machine struct {
	CPUs   int   `yaml:"cpus"`
	Memory Bytes `yaml:"memory"`
	Disk   Bytes `yaml:"disk"`
}

func (m *Machine) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: a size: { cpus, memory, disk }", n.Line)
	}
	if err := knownKeys(n, "a VM size", "cpus", "memory", "disk"); err != nil {
		return err
	}
	type plain Machine
	var v plain
	if err := n.Decode(&v); err != nil {
		return err
	}
	if k := key(n, "cpus"); k != nil && v.CPUs < 1 {
		return fmt.Errorf("line %d: cpus: %s: at least 1; leave it out to state none", k.Line, shown(k))
	}
	for _, f := range []struct {
		key string
		b   Bytes
	}{{"memory", v.Memory}, {"disk", v.Disk}} {
		if k := key(n, f.key); k != nil && f.b == 0 {
			return fmt.Errorf("line %d: %s: %s: a size; leave it out to state none", k.Line, f.key, shown(k))
		}
	}
	*m = Machine(v)
	return nil
}

// knownKeys refuses a key of mapping n not in keys, as the strict decoder does elsewhere: a node a
// type decodes itself escapes it.
func knownKeys(n *yaml.Node, what string, keys ...string) error {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if k := n.Content[i].Value; !slices.Contains(keys, k) {
			return fmt.Errorf("line %d: field %s not found in %s (%s)", n.Content[i].Line, k, what, strings.Join(keys, ", "))
		}
	}
	return nil
}

// key is the value of mapping n's key k, nil when n has none.
func key(n *yaml.Node, k string) *yaml.Node {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == k {
			return n.Content[i+1]
		}
	}
	return nil
}

// shown is a scalar as written, "empty" for a null.
func shown(n *yaml.Node) string {
	if n.Tag == "!!null" {
		return "empty"
	}
	return n.Value
}
