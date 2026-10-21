package version

import (
	"strings"
	"testing"
)

func TestSemVerRange(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		r, v string
		in   bool
	}{
		{">=0.1 <1", "0.1.0", true},
		{">=0.1 <1", "0.0.9", false},
		{">=0.1 <1", "0.10.0", true}, // numeric, never string order
		{">=0.1 <1", "0.99.99", true},
		{">=0.1 <1", "1.0.0", false},
		{">=0.1 <1", "1.0.0-rc.1", true}, // a pre-release of 1.0.0 orders before it
		{">=0.8", "0.8.0-rc.1", false},
		{">0.7.2", "0.7.2", false},
		{">0.7.2", "0.7.3", true},
		{"<=0.7", "0.7.0", true},
		{"<=0.7", "0.7.1", false},
		{"=0.7.2", "0.7.2", true},
		{"=0.7.2", "0.7.3", false},
		{"  >=1   <2  ", "1.5.0", true},
	} {
		r, err := ParseSemVerRange(c.r)
		if err != nil {
			t.Fatalf("%q: %v", c.r, err)
		}
		if in, err := r.Contains(c.v); err != nil || in != c.in {
			t.Errorf("%q contains %s = %v, %v; want %v", c.r, c.v, in, err, c.in)
		}
	}
	if r, _ := ParseSemVerRange("  >=1   <2 "); r.String() != ">=1 <2" {
		t.Errorf("String = %q", r.String())
	}
	if _, err := (SemVerRange{terms: []rangeTerm{{">=", "1.0.0"}}}).Contains("1.0"); err == nil {
		t.Error("a version that is not SemVer was ordered")
	}
}

func TestSemVerRangeRefusals(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "  ", "0.1", "~0.1", "^1", ">=v1", ">=1.0.0-rc.1", ">=1+b", ">=1.2.3.4", ">=01", ">=", "=>1", ">=1,<2", "<1 || >2"} {
		if _, err := ParseSemVerRange(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// FuzzSemVerRange: a range that parses holds the bound it names and never errs on a valid version.
func FuzzSemVerRange(f *testing.F) {
	for _, s := range []string{">=0.1 <1", "=1.2.3", ">0", "<=9.9"} {
		f.Add(s, "0.7.0")
	}
	f.Fuzz(func(t *testing.T, s, v string) {
		r, err := ParseSemVerRange(s)
		if err != nil {
			return
		}
		for _, term := range r.terms {
			in, err := (SemVerRange{terms: []rangeTerm{term}}).Contains(term.v)
			if err != nil || in != strings.Contains(term.op, "=") {
				t.Fatalf("%s%s on %s: %v, %v", term.op, term.v, term.v, in, err)
			}
		}
		if (SemVer{}).Validate(v) == nil {
			if _, err := r.Contains(v); err != nil {
				t.Fatalf("%q on valid %q: %v", s, v, err)
			}
		}
	})
}
