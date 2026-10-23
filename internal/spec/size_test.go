package spec

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseBytes(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]Bytes{
		"0": 0, "1024": 1024, "1b": 1, "1k": 1 << 10, "512M": 512 << 20, "512m": 512 << 20, "16G": 16 << 30,
		"16Gi": 16 << 30, "16GiB": 16 << 30, "16GB": 16 << 30, "16 G": 16 << 30, "2T": 2 << 40, " 8G ": 8 << 30,
		"8388607T": 8388607 << 40, "1.5g": 3 << 29, "1.5GiB": 3 << 29, "0.5k": 512, "1.0000001b": 1, "2p": 2 << 50, "1.99999999999999999k": 2047, "7.99999999999999999G": 8<<30 - 1,
	} {
		if got, err := ParseBytes(in); err != nil || got != want {
			t.Errorf("%q: %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "G", "-1G", "1.G", ".5G", "1e9", "16X", "16i", "16Gx", "16 GiB B", "8388608T", "8192P", "99999999999999999999"} {
		if got, err := ParseBytes(in); err == nil {
			t.Errorf("%q: %d, want an error", in, got)
		}
	}
}

// A size renders in a unit it parses back from.
func FuzzParseBytes(f *testing.F) {
	for _, s := range []string{"16G", "512m", "1024", "7GiB", "1T", "x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		b, err := ParseBytes(s)
		if err != nil {
			return
		}
		if b < 0 {
			t.Fatalf("%q: %d", s, b)
		}
		if back, err := ParseBytes(b.String()); err != nil || back != b {
			t.Fatalf("%q: %d renders %q, which reads %d, %v", s, b, b.String(), back, err)
		}
	})
}

func TestMemoryShareForms(t *testing.T) {
	t.Parallel()
	s, err := Parse([]byte(base + "stack:\n  memory:\n    shares: { db: { percent: 25, max: 16G }, api: 10 }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Stack.Memory.Shares; got["db"] != (Share{25, 16 << 30}) || got["api"] != (Share{Percent: 10}) {
		t.Errorf("shares: %+v", got)
	}
	for in, want := range map[string]string{
		"{ db: { percent: 25, maximum: 16G } }": "field maximum not found",
		"{ db: { percent: 25, max: lots } }":    `"lots": a size`,
		"{ db: [25] }":                          "a percent, or",
		"{ db: ten }":                           "cannot unmarshal",
		"{ db: { percent: 25, max: 0 } }":       "max: 0: a size of at least 64M",
		"{ db: { percent: 25, max: } }":         "max: empty: a size",
	} {
		if _, err := Parse([]byte(base + "stack:\n  memory:\n    shares: " + in + "\n")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", in, err, want)
		}
	}
}

// The payload's spec is JSON: a share without a maximum renders as its percent alone, its digest
// the percent's, both forms read back, and an unknown field is refused.
func TestMemoryShareJSON(t *testing.T) {
	t.Parallel()
	in := map[string]Share{"api": {Percent: 10}, "db": {Percent: 25, Max: 16 << 30}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"api":10,"db":{"Percent":25,"Max":17179869184}}`; string(raw) != want {
		t.Errorf("%s, want %s", raw, want)
	}
	var back map[string]Share
	if err := json.Unmarshal(raw, &back); err != nil || back["api"] != in["api"] || back["db"] != in["db"] {
		t.Errorf("%+v, %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"db":{"Percent":25,"Maximum":1}}`), &back); err == nil || !strings.Contains(err.Error(), "Maximum") {
		t.Errorf("an unknown field: %v", err)
	}
}

func TestVMSizeDecodes(t *testing.T) {
	t.Parallel()
	s, err := Parse([]byte(base + "shapes:\n  kairos:\n    size: { min: { cpus: 2, memory: 2G, disk: 20G }, recommended: { memory: 8G } }\n"))
	if err != nil {
		t.Fatal(err)
	}
	z := s.Shapes.Kairos.Size
	if *z.Min != (Machine{2, 2 << 30, 20 << 30}) || *z.Recommended != (Machine{Memory: 8 << 30}) {
		t.Errorf("%+v %+v", z.Min, z.Recommended)
	}
	if v := s.BundleView(); v.Shapes.Kairos.Size != nil || s.Shapes.Kairos.Size == nil {
		t.Errorf("the bundle's view carries the size, or the spec lost it")
	}
	for in, want := range map[string]string{
		"{ min: { ram: 2G } }":         "field ram not found",
		"{ min: { cpus: 0 } }":         "cpus: 0: at least 1",
		"{ min: { cpus: -1 } }":        "cpus: -1: at least 1",
		"{ min: { memory: 0 } }":       "memory: 0: a size",
		"{ min: { cpus: } }":           "cpus: empty: at least 1",
		"{ recommended: { disk: 0 } }": "disk: 0: a size",
		"{ min: 2 }":                   "a size: { cpus, memory, disk }",
		"{ max: { cpus: 2 } }":         "field max not found",
	} {
		if _, err := Parse([]byte(base + "shapes:\n  kairos:\n    size: " + in + "\n")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", in, err, want)
		}
	}
	if s, err := Parse([]byte(base + "shapes:\n  kairos:\n    size: { min: { cpus: 1 } }\n")); err != nil || s.Shapes.Kairos.Size.Min.CPUs != 1 {
		t.Errorf("1 CPU: %v", err)
	}
}
