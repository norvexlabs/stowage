package sbom

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/dockerfile"
	"github.com/norvexlabs/stowage/internal/release"
)

var policyNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func TestParseAllowances(t *testing.T) {
	t.Parallel()
	ok := "- { id: CVE-1, artifact: front, reason: no fix, expires: 2027-01-31 }\n- { id: CVE-1, artifact: \"*\", reason: r, expires: 2026-12-01 }\n"
	a, err := ParseAllowances([]byte(ok), policyNow)
	if err != nil || len(a) != 2 || a[0].Artifact != "front" || a[1].Artifact != "*" {
		t.Fatalf("%+v %v", a, err)
	}
	if a, err := ParseAllowances(nil, policyNow); err != nil || len(a) != 0 {
		t.Errorf("an empty file: %+v %v", a, err)
	}
	for _, c := range []struct{ doc, want string }{
		{"- { artifact: front, reason: r, expires: 2027-01-01 }", "id is required"},
		{"- { id: CVE-1, reason: r, expires: 2027-01-01 }", "artifact is required"},
		{"- { id: CVE-1, artifact: front, reason: \" \", expires: 2027-01-01 }", "reason is required"},
		{"- { id: CVE-1, artifact: front, reason: r, expires: 2027-02-30 }", "a date"},
		{"- { id: CVE-1, artifact: front, reason: r }", "a date"},
		{"- { id: CVE-1, artifact: front, reason: r, expires: 2027-01-01, until: x }", "field until not found"},
		{"- { id: CVE-1, artifact: front, reason: r, expires: 2027-01-01 }\n- { id: CVE-1, artifact: front, reason: s, expires: 2027-01-02 }", "listed twice"},
		// A year and a day ahead (2026-11-02 + 366 days ends on 2027-11-03): the last day allowed,
		// then one more.
		{"- { id: CVE-1, artifact: front, reason: r, expires: 2027-11-04 }", "more than a year ahead"},
	} {
		if _, err := ParseAllowances([]byte(c.doc), policyNow); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.doc, err, c.want)
		}
	}
	if _, err := ParseAllowances([]byte("- { id: CVE-1, artifact: front, reason: r, expires: 2027-11-02 }"), policyNow); err != nil {
		t.Errorf("a year ahead: %v", err)
	}
}

func TestOwner(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{"front": "stowage", "backup": "stowage", "chown": "stowage", Key: "stowage",
		"installer": "stowage", "os": "project", "disk-qcow2": "project", "api": "project", "postgres": "project", "disk": "project"} {
		if got := Owner(name); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
}

func policyReport() Report {
	fix := func(id string, s Severity) Finding {
		return Finding{ID: id, Severity: s, Package: "p", Version: "1", Type: "apk", Fixed: []string{"2"}}
	}
	nofix := func(id string, s Severity) Finding {
		return Finding{ID: id, Severity: s, Package: "q", Version: "1", Type: "go-module"}
	}
	return Report{Subjects: []Subject{
		{Name: "api", Owner: "project", Findings: []Finding{fix("CVE-A", Critical), fix("CVE-B", High), nofix("CVE-C", Critical), fix("CVE-D", Medium)}},
		{Name: "front", Owner: "stowage", Findings: []Finding{fix("CVE-E", High), nofix("CVE-F", High)}},
	}}
}

func ids(g Gate) string {
	var out []string
	for _, b := range g.Blocked {
		out = append(out, b.Subject+"/"+b.Finding.ID)
	}
	return strings.Join(out, " ")
}

func TestJudge(t *testing.T) {
	t.Parallel()
	r := policyReport()
	for _, c := range []struct {
		name string
		p    Policy
		want string
	}{
		{"fixable high", Policy{FailOn: High, On: true, FixableOnly: true}, "api/CVE-A api/CVE-B front/CVE-E"},
		{"every high", Policy{FailOn: High, On: true}, "api/CVE-A api/CVE-B api/CVE-C front/CVE-E front/CVE-F"},
		{"fixable critical", Policy{FailOn: Critical, On: true, FixableOnly: true}, "api/CVE-A"},
		{"off", Policy{FailOn: Negligible, FixableOnly: true}, ""},
		{"allowed in its artifact", Policy{FailOn: High, On: true, FixableOnly: true, Allow: []Allowance{{ID: "CVE-E", Artifact: "front", Expires: "2026-11-02"}}}, "api/CVE-A api/CVE-B"},
		{"allowed in another artifact", Policy{FailOn: High, On: true, FixableOnly: true, Allow: []Allowance{{ID: "CVE-E", Artifact: "api", Expires: "2027-01-01"}}}, "api/CVE-A api/CVE-B front/CVE-E"},
		{"allowed everywhere", Policy{FailOn: High, On: true, FixableOnly: true, Allow: []Allowance{{ID: "CVE-B", Artifact: "*", Expires: "2027-01-01"}}}, "api/CVE-A front/CVE-E"},
		// Its last day was the day before: the finding counts again.
		{"expired", Policy{FailOn: High, On: true, FixableOnly: true, Allow: []Allowance{{ID: "CVE-E", Artifact: "front", Expires: "2026-11-01"}}}, "api/CVE-A api/CVE-B front/CVE-E"},
	} {
		if got := ids(r.Judge(c.p, policyNow)); got != c.want {
			t.Errorf("%s: blocked %q, want %q", c.name, got, c.want)
		}
	}
	g := r.Judge(Policy{FailOn: High, On: true, FixableOnly: true, Allow: []Allowance{
		{ID: "CVE-E", Artifact: "front", Expires: "2026-11-01"}, {ID: "CVE-Z", Artifact: "*", Expires: "2027-01-01"}, {ID: "CVE-B", Artifact: "api", Expires: "2027-01-01"},
	}}, policyNow)
	if len(g.Expired) != 1 || g.Expired[0].ID != "CVE-E" || len(g.Unused) != 1 || g.Unused[0].ID != "CVE-Z" || g.Allowed != 1 {
		t.Errorf("expired %+v, unused %+v, allowed %d", g.Expired, g.Unused, g.Allowed)
	}
	// An allowance of a finding the policy does not count today (no fix yet) is in use.
	if g := r.Judge(Policy{FailOn: High, On: true, FixableOnly: true, Allow: []Allowance{{ID: "CVE-F", Artifact: "front", Expires: "2027-01-01"}}}, policyNow); len(g.Unused) != 0 || g.Allowed != 0 {
		t.Errorf("an allowance of an uncounted finding: unused %+v, allowed %d", g.Unused, g.Allowed)
	}
	notes := strings.Join(g.Notes(), "\n")
	if !strings.Contains(notes, "CVE-E in front expired on 2026-11-01") || !strings.Contains(notes, "CVE-Z in * matches no finding") {
		t.Errorf("notes %q", notes)
	}
}

func TestGateErr(t *testing.T) {
	t.Parallel()
	p := Policy{FailOn: High, On: true, FixableOnly: true}
	r := policyReport()
	err := r.Judge(p, policyNow).Err(p)
	if err == nil || !strings.Contains(err.Error(), "audit: 3 fixable vulnerabilities at or above high: critical CVE-A in api (p 1, fixed in 2)") {
		t.Errorf("%v", err)
	}
	if err := r.Judge(Policy{FailOn: Critical, On: true, FixableOnly: true, Allow: []Allowance{{ID: "CVE-A", Artifact: "api", Expires: "2027-01-01"}}}, policyNow).Err(p); err != nil {
		t.Errorf("a release the policy passes: %v", err)
	}
	// A subject not audited is refused whatever the findings.
	r.Subjects = append(r.Subjects, Subject{Name: "installer", Error: "no SBOM"})
	if err := r.Judge(Policy{}, policyNow).Err(Policy{}); err == nil || err.Error() != "audit: not audited: installer" {
		t.Errorf("%v", err)
	}
	// Many findings: the first five named, the rest counted.
	var many Report
	for i := range 7 {
		many.Subjects = append(many.Subjects, Subject{Name: "api", Findings: []Finding{{ID: "CVE-" + string(rune('a'+i)), Severity: High, Fixed: []string{"2"}}}})
	}
	if err := many.Judge(p, policyNow).Err(p); err == nil || !strings.HasSuffix(err.Error(), "; and 2 more") {
		t.Errorf("%v", err)
	}
}

func TestPrintSplitsByOwnerAndFix(t *testing.T) {
	t.Parallel()
	r := policyReport()
	r.Project, r.Version, r.Revision = "notes", "1.0.0", 1
	var b bytes.Buffer
	if err := r.Print(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"2 artifacts, vulnerabilities: 2 critical, 3 high, 1 medium\n",
		"  the project's: fixable 1 critical, 1 high, 1 medium; no fix 1 critical\n",
		"  Stowage's: fixable 1 high; no fix 1 high\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in\n%s", want, b.String())
		}
	}
}

// An allowance holds through the whole of its last day, UTC, and not an instant after.
func TestAllowanceHoldsThroughItsLastDay(t *testing.T) {
	t.Parallel()
	a := Allowance{ID: "CVE-1", Artifact: "*", Expires: "2026-11-01"}
	last := time.Date(2026, 11, 1, 23, 59, 59, 0, time.UTC)
	if !a.Holds(last) || a.Holds(last.Add(time.Second)) {
		t.Errorf("holds at %s: %v, a second later: %v", last, a.Holds(last), a.Holds(last.Add(time.Second)))
	}
}

// The scanner's recipe builds grype with Go, its base pinned by digest, onto an empty image, its
// version checked.
func TestScannerRecipe(t *testing.T) {
	t.Parallel()
	f, err := dockerfile.Parse(Dockerfile)
	if err != nil {
		t.Fatal(err)
	}
	froms := f.Froms()
	if len(froms) != 2 || !regexp.MustCompile(`^golang:[^@\s]+@sha256:[0-9a-f]{64}$`).MatchString(froms[0].Image) || froms[1].Image != "scratch" {
		t.Fatalf("FROMs = %v", froms)
	}
	for _, want := range []string{"go get github.com/anchore/grype/cmd/grype@v0.121.0 ", `$2 == "github.com/anchore/grype" && $3 == "v0.121.0"`, `ENTRYPOINT ["/grype"]`} {
		if !strings.Contains(string(Dockerfile), want) {
			t.Errorf("the recipe lacks %q", want)
		}
	}
}

// A disk is marked as one by its repository, never by its name: an image keyed disk-… is an image.
func TestTargetsMarkTheDisks(t *testing.T) {
	t.Parallel()
	a := release.Artifacts{Images: map[string]string{"disk-tools": "sha256:" + strings.Repeat("a", 64)}, OS: "sha256:" + strings.Repeat("b", 64),
		Disks: map[string]string{"qcow2": "sha256:" + strings.Repeat("c", 64)}}
	targets, err := Targets("reg.example/acme", "notes", a)
	if err != nil {
		t.Fatal(err)
	}
	disks := map[string]bool{}
	for _, x := range targets {
		disks[x.Name] = x.Disk
	}
	if disks["disk-tools"] || disks["os"] || !disks["disk-qcow2"] || len(disks) != 3 {
		t.Errorf("%v", disks)
	}
}
