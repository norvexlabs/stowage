package lint

import (
	"reflect"
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
