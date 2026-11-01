package spec

import (
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// services.<name>.nftables is a file, or an input and a file by each of its values; anything
// else is refused, naming the form.
func TestATableIsAFileOrOnePickedByAnInput(t *testing.T) {
	t.Parallel()
	var one, picked NftTable
	if err := yaml.Unmarshal([]byte("deploy/a.nft"), &one); err != nil || !one.Set() || !slices.Equal(one.Paths(), []string{"deploy/a.nft"}) || one.Values() != nil {
		t.Errorf("a file: %+v %v", one, err)
	}
	if err := yaml.Unmarshal([]byte("{ input: P, files: { observe: o.nft, enforce: e.nft } }"), &picked); err != nil || !picked.Set() ||
		!slices.Equal(picked.Values(), []string{"enforce", "observe"}) || !slices.Equal(picked.Paths(), []string{"e.nft", "o.nft"}) {
		t.Errorf("picked: %+v %v", picked, err)
	}
	for text, want := range map[string]string{
		"[a.nft]":                   "a table: a file, or { input: KEY, files: { <value>: <file> } }",
		"{ input: P }":              "names its input and a file by each of its values",
		"{ files: { a: a.nft } }":   "names its input and a file by each of its values",
		"{ input: P, file: a.nft }": "field file not found in a table an input picks (input, files)",
	} {
		var v NftTable
		if err := yaml.Unmarshal([]byte(text), &v); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", text, err, want)
		}
	}
	if (NftTable{}).Set() {
		t.Error("no table is set")
	}
}
