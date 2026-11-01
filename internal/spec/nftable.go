package spec

import (
	"fmt"
	"maps"
	"regexp"
	"slices"

	"go.yaml.in/yaml/v3"
)

// NftTable is services.<name>.nftables: a file of the project holding the service's table (a
// scalar), or the table an input picks ({ input: KEY, files: { <value>: <file> } }), a file by
// each value the input may hold.
type NftTable struct {
	File  string
	Input string
	Files map[string]string
}

// NftChoice is a value of an input picking a table, which names its file in the installer: a
// letter or digit, then letters, digits and _ . -, at most 64.
var NftChoice = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func (t *NftTable) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*t = NftTable{File: n.Value}
		return nil
	case yaml.MappingNode:
	default:
		return fmt.Errorf("line %d: a table: a file, or { input: KEY, files: { <value>: <file> } }", n.Line)
	}
	if err := knownKeys(n, "a table an input picks", "input", "files"); err != nil {
		return err
	}
	var v struct {
		Input string            `yaml:"input"`
		Files map[string]string `yaml:"files"`
	}
	if err := n.Decode(&v); err != nil {
		return err
	}
	if v.Input == "" || len(v.Files) == 0 {
		return fmt.Errorf("line %d: a table an input picks names its input and a file by each of its values (input, files)", n.Line)
	}
	*t = NftTable{Input: v.Input, Files: v.Files}
	return nil
}

// Set reports whether the service has a table.
func (t NftTable) Set() bool { return t.File != "" || t.Input != "" }

// Values are the values of the input picking the table, sorted; none for a table of one file.
func (t NftTable) Values() []string { return slices.Sorted(maps.Keys(t.Files)) }

// Paths are the table's files: its one, or each value's in the order of Values.
func (t NftTable) Paths() []string {
	if t.File != "" {
		return []string{t.File}
	}
	var out []string
	for _, v := range t.Values() {
		out = append(out, t.Files[v])
	}
	return out
}
