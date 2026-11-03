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

// Share is a service's part of the host's RAM: Percent of it, capped at Max when Max is set, and
// never under Min when Min is set (Min wins over Max: lint refuses a Min over the Max). Written as
// the percent alone, or as { percent: N, min: 256M, max: 16G }.
type Share struct {
	Percent int
	Max     Bytes
	Min     Bytes
}

// Limit is one container's memory limit from the share on a host whose kernel shows ram bytes, the
// service run as n containers: its part of the percent, capped at its part of Max, in whole MiB;
// never under its part of Min (rounded up to the byte, so the parts together are never under
// Min), MinMemLimit, or floor (what the compose file gives the container itself). The install and
// lint both compute it here.
func (s Share) Limit(ram uint64, n int, floor Bytes) Bytes {
	n = max(n, 1)
	pct := uint64(max(s.Percent, 0))
	// Multiplied first, as hosts are: divided first only where the product would overflow.
	var whole uint64
	if ram > math.MaxUint64/100 {
		whole = ram / 100 * pct
	} else {
		whole = ram * pct / 100
	}
	part := Bytes(min(whole/uint64(n), math.MaxInt64))
	if s.Max > 0 {
		part = min(part, s.Max/Bytes(n))
	}
	const mib = Bytes(1 << 20)
	// The min's part rounded up to the byte, never to the MiB: the parts together are never
	// under Min, and a Min equal to Max gives Max's part, within a byte.
	least := s.Min / Bytes(n)
	if s.Min%Bytes(n) != 0 {
		least++
	}
	return max(part&^(mib-1), least, MinMemLimit, floor)
}

func (s *Share) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*s = Share{}
		return n.Decode(&s.Percent)
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: a percent, or { percent: N, min: 256M, max: 16G }", n.Line)
	}
	if err := knownKeys(n, "a memory share", "percent", "min", "max"); err != nil {
		return err
	}
	var v struct {
		Percent int   `yaml:"percent"`
		Min     Bytes `yaml:"min"`
		Max     Bytes `yaml:"max"`
	}
	if err := n.Decode(&v); err != nil {
		return err
	}
	for _, b := range []struct {
		name string
		v    Bytes
	}{{"min", v.Min}, {"max", v.Max}} {
		if k := key(n, b.name); k != nil && b.v == 0 {
			return fmt.Errorf("line %d: %s: %s: a size of at least %s; leave %s out for none", k.Line, b.name, shown(k), MinMemLimit, b.name)
		}
	}
	*s = Share{Percent: v.Percent, Max: v.Max, Min: v.Min}
	return nil
}

// MarshalJSON writes a share with neither a minimum nor a maximum as its percent alone, and one
// without a minimum without the Min field: a share's digest depends on the keys it states alone.
func (s Share) MarshalJSON() ([]byte, error) {
	if s.Max == 0 && s.Min == 0 {
		return json.Marshal(s.Percent)
	}
	// Min omitted when unset: a share stating a maximum alone is written as one.
	return json.Marshal(struct {
		Percent, Max int64
		Min          int64 `json:",omitempty"`
	}{int64(s.Percent), int64(s.Max), int64(s.Min)})
}

func (s *Share) UnmarshalJSON(raw []byte) error {
	if err := json.Unmarshal(raw, &s.Percent); err == nil {
		s.Max, s.Min = 0, 0
		return nil
	}
	var v struct{ Percent, Max, Min int64 }
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("a memory share: %w", err)
	}
	*s = Share{Percent: int(v.Percent), Max: Bytes(v.Max), Min: Bytes(v.Min)}
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
