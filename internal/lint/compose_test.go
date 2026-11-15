package lint

import (
	"reflect"
	"strings"
	"testing"
)

// mergeNetworks merges a service's networks with its extends base's by network and key, a list
// naming networks without settings; a shape it does not read keeps the service's own.
func TestMergeNetworks(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		own, base, want any
	}{
		{nil, []any{"a"}, []any{"a"}},
		{[]any{"a"}, nil, []any{"a"}},
		{[]any{"a"}, []any{"b"}, map[string]any{"a": map[string]any{}, "b": nil}},
		{[]any{"a"}, map[string]any{"a": map[string]any{"ipv4_address": "10.0.0.5"}}, map[string]any{"a": map[string]any{"ipv4_address": "10.0.0.5"}}},
		{map[string]any{"a": map[string]any{"aliases": []any{"x"}}}, map[string]any{"a": map[string]any{"ipv4_address": "10.0.0.5"}},
			map[string]any{"a": map[string]any{"aliases": []any{"x"}, "ipv4_address": "10.0.0.5"}}},
		{[]any{"a"}, "odd", []any{"a"}},
		{[]any{1}, []any{"b"}, []any{1}},
	} {
		if got := mergeNetworks(c.own, c.base); !reflect.DeepEqual(got, c.want) {
			t.Errorf("mergeNetworks(%v, %v) = %v, want %v", c.own, c.base, got, c.want)
		}
	}
}

func TestEnvReadsASecret(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		env  any
		want bool
	}{
		{map[string]any{"P": nil}, true},
		{map[string]any{"P": "literal"}, false},
		{map[string]any{"X": "${P}"}, true},
		{map[string]any{"X": "${P:-x}"}, true},
		{map[string]any{"X": "a$P"}, true},
		{map[string]any{"X": "$${P}"}, false},
		{map[string]any{"X": "${PX}"}, false},
		{[]any{"P"}, true},
		{[]any{"P=literal"}, false},
		{[]any{"X=${P}"}, true},
		{[]any{"PX"}, false},
		{nil, false},
	} {
		if got := envReads(c.env, "P"); got != c.want {
			t.Errorf("%v: %v", c.env, got)
		}
	}
}

// A volume Compose owns passes, its literal name returned; one it does not is refused.
func TestOwnedVolumes(t *testing.T) {
	t.Parallel()
	raw := []byte("services: { a: { image: x } }\nvolumes:\n  plain: {}\n  bare:\n  off: { external: false }\n  no: { external: \"no\" }\n  named: { name: lit }\n  ext: { external: true }\n  yes: { external: \"yes\" }\n  var: { external: \"${E}\" }\n  old: { external: { name: x } }\n  interp: { name: \"${N}\" }\n  esc: { name: \"a$$b\" }\n  num: { name: 123 }\n  mixed: { name: \"$$${N}\" }\n")
	got, err := OwnedVolumes(raw, []string{"plain", "bare", "off", "no", "named", "esc"})
	if err != nil || len(got) != 6 || got["named"] != "lit" || got["plain"] != "" || got["esc"] != "a$b" {
		t.Fatalf("owned: %v %v", got, err)
	}
	for v, want := range map[string]string{
		"absent": `"absent" is no volume of the compose file`,
		"ext":    `"ext" is an external volume`, "yes": `"yes" is an external volume`,
		"var": `"var" is an external volume`, "old": `"old" is an external volume`,
		"interp": `"interp" is named "${N}", which Compose interpolates`,
		"mixed":  `"mixed" is named "$$${N}", which Compose interpolates`,
		"num":    `"num"'s name: 123 is not a string: quote it`,
	} {
		if _, err := OwnedVolumes(raw, []string{"plain", v}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", v, want, err)
		}
	}
}

// A picked table's files: each value a file's name, each file its table alone, the same sets.
func TestCheckPickedTables(t *testing.T) {
	t.Parallel()
	a := "table inet vpn {\n\tset trusted { type ipv4_addr; }\n}\n"
	b := "table inet vpn {\n\tset trusted { type ipv4_addr; }\n\tchain c { }\n}\n"
	if err := CheckPickedTables("vpn", map[string]string{"on": a, "off": b}); err != nil {
		t.Fatal(err)
	}
	for want, bodies := range map[string]map[string]string{
		`files: "bad value" is not a value naming a table's file`:          {"on": a, "bad value": a},
		`files.on: does not start with "table inet vpn {"`:                 {"on": "table inet other { }\n", "off": a},
		"files: its files declare different sets (off: [], on: [trusted])": {"on": a, "off": "table inet vpn { }\n"},
	} {
		if err := CheckPickedTables("vpn", bodies); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
}
