package version

import (
	"strings"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/spec"
)

func mustCalVer(t *testing.T, c CalVerConfig) *CalVer {
	t.Helper()
	cv, err := NewCalVer(c)
	if err != nil {
		t.Fatal(err)
	}
	return cv
}

func TestCalVerValidate(t *testing.T) {
	cv := mustCalVer(t, CalVerConfig{Format: "YYYY.M.MICRO"})
	for _, ok := range []string{"2026.1.0", "2026.12.317", "2026.10.1"} {
		if err := cv.Validate(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"2026.13.1", "2026.0.1", "2026.01.1", "2026.1.01", "26.1.1", "2026.1", "2026.1.1.1", "2026.1.x", "2026.1.-1", ""} {
		if err := cv.Validate(bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
	padded := mustCalVer(t, CalVerConfig{Format: "0Y.0M.MICRO"})
	if err := padded.Validate("26.09.3"); err != nil {
		t.Error(err)
	}
	if err := padded.Validate("26.9.3"); err == nil {
		t.Error("0M accepted a one-digit month")
	}
}

func TestCalVerCompareIsNumeric(t *testing.T) {
	cv := mustCalVer(t, CalVerConfig{Format: "YYYY.M.MICRO"})
	cases := []struct {
		a, b string
		want int
	}{
		{"2026.9.317", "2026.10.1", -1}, // string order would say the opposite
		{"2026.10.2", "2026.10.10", -1},
		{"2027.1.1", "2026.12.400", 1},
		{"2026.10.5", "2026.10.5", 0},
	}
	for _, c := range cases {
		got, err := cv.Compare(c.a, c.b)
		if err != nil || got != c.want {
			t.Errorf("Compare(%s,%s) = %d, %v; want %d", c.a, c.b, got, err, c.want)
		}
		back, _ := cv.Compare(c.b, c.a)
		if back != -c.want {
			t.Errorf("Compare(%s,%s) not antisymmetric", c.b, c.a)
		}
	}
}

func TestCalVerMonthGuardBoundaries(t *testing.T) {
	cv := mustCalVer(t, CalVerConfig{Format: "YYYY.M.MICRO", MonthGuard: "utc"})
	// 2026-10-01 00:30 in UTC+2 is still 2026-09-30 in UTC: the guard reads UTC.
	paris := time.FixedZone("UTC+2", 2*3600)
	firstOfOctLocal := time.Date(2026, 10, 1, 0, 30, 0, 0, paris)
	if err := cv.Guard("2026.9.10", firstOfOctLocal); err != nil {
		t.Errorf("September version refused while UTC is still September: %v", err)
	}
	if err := cv.Guard("2026.10.1", firstOfOctLocal); err == nil {
		t.Error("October version accepted while UTC is still September")
	}
	lastOfOct := time.Date(2026, 10, 31, 23, 59, 59, 0, time.UTC)
	if err := cv.Guard("2026.10.7", lastOfOct); err != nil {
		t.Error(err)
	}
	if err := cv.Guard("2026.10.7", lastOfOct.Add(time.Second)); err == nil {
		t.Error("October version accepted in November")
	}
	if err := cv.Guard("2025.10.7", lastOfOct); err == nil {
		t.Error("guard ignored the year")
	}
	short := mustCalVer(t, CalVerConfig{Format: "YY.MM.MICRO", MonthGuard: "utc"})
	if err := short.Guard("26.10.0", lastOfOct); err != nil {
		t.Errorf("YY year not mapped to 20YY: %v", err)
	}
	off := mustCalVer(t, CalVerConfig{Format: "YYYY.M.MICRO"})
	if err := off.Guard("2001.1.1", lastOfOct); err != nil {
		t.Errorf("guard off but refused: %v", err)
	}
}

func TestCalVerConfigErrors(t *testing.T) {
	for _, c := range []CalVerConfig{
		{Format: "YYYY.Q.MICRO"},
		{Format: "MAJOR.MICRO", MonthGuard: "utc"},
		{Format: "YYYY.M.MICRO", MonthGuard: "yes"},
	} {
		if _, err := NewCalVer(c); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
}

// The precedence chain is the example from semver.org § 11.
func TestSemVerPrecedence(t *testing.T) {
	chain := []string{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0", "2.0.0", "10.0.0"}
	var s SemVer
	for i := range chain {
		for j := range chain {
			got, err := s.Compare(chain[i], chain[j])
			if err != nil {
				t.Fatal(err)
			}
			want := cmp(i, j)
			if got != want {
				t.Errorf("Compare(%s,%s) = %d, want %d", chain[i], chain[j], got, want)
			}
		}
	}
	if c, _ := s.Compare("1.0.0-a", "1.0.0-a"); c != 0 {
		t.Error("equal versions differ")
	}
}

func TestSemVerValidate(t *testing.T) {
	var s SemVer
	for _, bad := range []string{"v1.0.0", "1.0", "01.0.0", "1.0.0-01", "1.0.0-", "1.0.0+", "1.0.0-a..b", "1.0.0-é"} {
		if err := s.Validate(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := s.Validate("1.2.3+sha.abc"); err == nil {
		t.Error("build metadata accepted in a release version")
	}
	if tag, err := s.Tag("1.2.3-rc.1"); err != nil || tag != "1.2.3-rc.1" {
		t.Errorf("Tag = %q, %v", tag, err)
	}
}

func FuzzSemVerCompareReflexive(f *testing.F) {
	for _, s := range []string{"1.0.0", "1.0.0-alpha.1", "0.0.1-x", "9.9.9-0.a"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		var s SemVer
		if s.Validate(v) != nil {
			return
		}
		if c, err := s.Compare(v, v); err != nil || c != 0 {
			t.Fatalf("Compare(%q,%q) = %d, %v", v, v, c, err)
		}
		// A valid version is its own tag, or is refused only because a tag holds 128 characters.
		tag, err := s.Tag(v)
		switch {
		case err == nil && tag != v:
			t.Fatalf("valid version %q gave tag %q", v, tag)
		case err != nil && len(v) <= 128:
			t.Fatalf("valid version %q refused: %v", v, err)
		}
	})
}

func TestRegexScheme(t *testing.T) {
	r, err := NewRegex(RegexConfig{
		Pattern: `r(?P<major>[0-9]+)(?:-(?P<label>[a-z]+))?`,
		Order:   []string{"major:int", "label:str"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := r.Compare("r9", "r10"); c != -1 {
		t.Error("int key compared as string")
	}
	if c, _ := r.Compare("r10-b", "r10-a"); c != 1 {
		t.Error("str key ignored")
	}
	if err := r.Validate("xr10"); err == nil {
		t.Error("pattern not anchored")
	}
	for _, c := range []RegexConfig{
		{Pattern: `(?P<a>\d+)`, Order: []string{"b:int"}},
		{Pattern: `(?P<a>\d+)`, Order: []string{"a:float"}},
		{Pattern: `(`, Order: []string{"a:int"}},
		{Pattern: `(?P<a>\d+)`},
	} {
		if _, err := NewRegex(c); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
}

func TestChannelTag(t *testing.T) {
	cases := []struct {
		tmpl string
		rev  int
		want string
	}{
		{"{v}", 1, "1.2.0"},
		{"{v}", 2, "1.2.0-r2"},
		{"v{v}-dev", 1, "v1.2.0-dev"},
		{"v{v}-dev", 3, "v1.2.0-r3-dev"},
	}
	for _, c := range cases {
		got, err := ChannelTag(c.tmpl, "1.2.0", c.rev)
		if err != nil || got != c.want {
			t.Errorf("ChannelTag(%q, rev %d) = %q, %v; want %q", c.tmpl, c.rev, got, err, c.want)
		}
	}
	for _, bad := range []string{"latest", "{v}{v}", "{v}/x"} {
		if _, err := ChannelTag(bad, "1.2.0", 1); err == nil {
			t.Errorf("template %q accepted", bad)
		}
	}
	// 1.2.0 at revision 2 would otherwise share a tag with the pre-release 1.2.0-r2.
	if _, err := ChannelTag("{v}", "1.2.0-r2", 1); err == nil {
		t.Error("a version ending like a revision suffix accepted")
	}
	if tag, err := ChannelTag("{v}", "1.2.0-rc2", 1); err != nil || tag != "1.2.0-rc2" {
		t.Errorf("1.2.0-rc2: %q, %v", tag, err)
	}
	if _, err := ChannelTag("{v}", "1.2.0", 0); err == nil {
		t.Error("revision 0 accepted")
	}
}

func TestChartVersion(t *testing.T) {
	if got := ChartVersion(412); got != "0.412.0" {
		t.Errorf("ChartVersion = %s", got)
	}
	// Chart versions order as releases are published, whatever the project version looks like.
	var sv SemVer
	if c, _ := sv.Compare(ChartVersion(9), ChartVersion(10)); c != -1 {
		t.Error("chart versions out of release order")
	}
}

func TestChannelConflicts(t *testing.T) {
	cal := mustCalVer(t, CalVerConfig{Format: "YYYY.M.MICRO"})
	ch := func(kv ...string) []Channel {
		var out []Channel
		for i := 0; i < len(kv); i += 2 {
			out = append(out, Channel{Name: kv[i], Template: kv[i+1]})
		}
		return out
	}
	all := func(chs []Channel, s Scheme, v string, reserved ...string) string {
		t.Helper()
		a, err := TemplateConflicts(chs, s, reserved)
		if err != nil {
			t.Fatal(err)
		}
		b, err := VersionConflicts(chs, s, v)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(append(a, b...), "\n")
	}
	cases := []struct {
		name     string
		channels []Channel
		s        Scheme
		v        string
		want     string // "" = no conflict
	}{
		{"calver suffix is safe", ch("stable", "v{v}", "dev", "v{v}-dev"), cal, "2026.10.1", ""},
		{"semver suffix collides", ch("stable", "{v}", "beta", "{v}-beta"), SemVer{}, "1.0.0", "also channel stable's tag for version 1.0.0-beta"},
		{"semver prefix is safe", ch("stable", "{v}", "beta", "beta-{v}"), SemVer{}, "1.0.0", ""},
		{"same template", ch("stable", "{v}", "dev", "{v}"), cal, "2026.10.1", "share the tag template"},
		{"revision collides", ch("stable", "{v}", "hot", "{v}-r9"), SemVer{}, "1.0.0", "channel hot tag \"1.0.0-r9\""},
	}
	for _, c := range cases {
		got := all(c.channels, c.s, c.v)
		switch {
		case c.want == "" && got != "":
			t.Errorf("%s: unexpected conflict %s", c.name, got)
		case c.want != "" && !strings.Contains(got, c.want):
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	// Pointer and reserved tags, for any version — not only the one being checked.
	words, _ := NewRegex(RegexConfig{Pattern: `(?P<w>[a-z0-9-]+)`, Order: []string{"w:str"}})
	got := all(ch("stable", "{v}", "dev", "dev-{v}"), words, "x1", ReservedTags...)
	for _, want := range []string{"pointer tag of channel stable for version stable", "reserved tag latest for version latest"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	digits, _ := NewRegex(RegexConfig{Pattern: `(?P<n>[0-9]+)`, Order: []string{"n:int"}})
	if got := all(ch("stable", "{v}", "2", "two-{v}"), digits, "1"); !strings.Contains(got, "pointer tag of channel 2 for version 2") {
		t.Errorf("a future version's pointer collision not reported: %q", got)
	}
	// Only the suffix ChannelTag appends counts as a revision: -r1, -r01 are no revision of anything,
	// and a version ending like a revision is never the colliding version (ChannelTag refuses it).
	if got := all(ch("stable", "{v}", "hot", "hot-{v}"), SemVer{}, "1.0.0"); got != "" {
		t.Errorf("unexpected conflict: %q", got)
	}
	if _, ok, _ := versionUnder("{v}", "1.0.0-r1", SemVer{}); ok {
		t.Error("-r1 read as a revision suffix")
	}
	if v, ok, _ := versionUnder("{v}", "1.0.0-r2", SemVer{}); !ok || v != "1.0.0" {
		t.Errorf("1.0.0-r2 under {v} = %q, %v; want 1.0.0 at revision 2", v, ok)
	}
	// Invalid templates are left to CheckTemplate: no duplicate report.
	if got := all(ch("stable", "latest", "dev", "{v}-dev"), cal, "2026.10.1"); got != "" {
		t.Errorf("invalid template reported twice: %v", got)
	}
	for _, bad := range []string{"latest", "{v}{v}", "{v}/x", "{v} x"} {
		if CheckTemplate(bad) == nil {
			t.Errorf("CheckTemplate(%q) accepted", bad)
		}
	}
}

// A scheme that cannot answer makes the check fail loudly: never a passed check, never an
// invented conflict.
func TestChannelConflictsNeedAnAnswer(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "s.sh", "#!/bin/sh\nexit 2\n", 0o755)
	sch, err := NewScheme(dir, spec.Provider{Name: "command", Config: map[string]any{"cmd": []any{"./s.sh"}}})
	if err != nil {
		t.Fatal(err)
	}
	chs := []Channel{{"stable", "{v}"}, {"dev", "{v}-dev"}}
	if conflicts, err := TemplateConflicts(chs, sch, nil); err == nil || len(conflicts) > 0 {
		t.Errorf("TemplateConflicts = %v, %v; want an error and no conflict", conflicts, err)
	}
}

func TestNewSchemeProviders(t *testing.T) {
	if s, err := NewScheme(".", spec.Provider{}); err != nil {
		t.Fatal(err)
	} else if _, ok := s.(*CalVer); !ok {
		t.Errorf("default scheme is %T, want calver", s)
	}
	_, err := NewScheme(".", spec.Provider{Name: "semver", Config: map[string]any{"format": "x"}})
	if err == nil || !strings.Contains(err.Error(), "format") {
		t.Errorf("semver accepted an unknown key: %v", err)
	}
	if _, err := NewScheme(".", spec.Provider{Name: "nope"}); err == nil {
		t.Error("unknown provider accepted")
	}
	if _, err := NewScheme(".", spec.Provider{Name: "command"}); err == nil {
		t.Error("command without cmd accepted")
	}
}

// Property: (version, revision) → tag is injective. Two different releases never share a tag.
func FuzzChannelTagInjective(f *testing.F) {
	f.Add("1.2.0", 2, "1.2.0-r2", 1)
	f.Add("1.2.0-r", 3, "1.2.0-r-r3", 1)
	f.Add("2026.10.1", 1, "2026.10.1", 2)
	f.Fuzz(func(t *testing.T, a string, ra int, b string, rb int) {
		if ra < 1 || rb < 1 || ra > 1000 || rb > 1000 || (a == b && ra == rb) {
			return
		}
		var s SemVer
		ta, errA := s.Tag(a)
		tb, errB := s.Tag(b)
		if errA != nil || errB != nil {
			return
		}
		x, errA := ChannelTag("v{v}-dev", ta, ra)
		y, errB := ChannelTag("v{v}-dev", tb, rb)
		if errA == nil && errB == nil && x == y {
			t.Fatalf("(%s, r%d) and (%s, r%d) share the tag %s", a, ra, b, rb, x)
		}
	})
}

// Properties of every CalVer format: a valid version compares equal to itself and is its own tag.
func FuzzCalVer(f *testing.F) {
	f.Add("YYYY.M.MICRO", "2026.10.1")
	f.Add("YY.0M.MICRO", "26.09.0")
	f.Add("YYYY.0M.0D", "2026.01.31")
	f.Fuzz(func(t *testing.T, format, v string) {
		cv, err := NewCalVer(CalVerConfig{Format: format})
		if err != nil || cv.Validate(v) != nil {
			return
		}
		if c, err := cv.Compare(v, v); err != nil || c != 0 {
			t.Fatalf("Compare(%q,%q) = %d, %v", v, v, c, err)
		}
		if tag, err := cv.Tag(v); err != nil || tag != v {
			t.Fatalf("Tag(%q) = %q, %v", v, tag, err)
		}
	})
}

func FuzzRegexNeverPanics(f *testing.F) {
	f.Add(`r(?P<n>[0-9]+)`, "n:int", "r12", "r9")
	f.Fuzz(func(t *testing.T, pattern, order, a, b string) {
		r, err := NewRegex(RegexConfig{Pattern: pattern, Order: []string{order}})
		if err != nil {
			return
		}
		if r.Validate(a) == nil {
			if c, err := r.Compare(a, a); err != nil || c != 0 {
				t.Fatalf("Compare(%q,%q) = %d, %v", a, a, c, err)
			}
		}
		_, _ = r.Compare(a, b)
	})
}
