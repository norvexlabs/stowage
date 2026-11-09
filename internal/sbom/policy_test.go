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
	ok := "- { id: CVE-1, artifact: front, reason: no fix, expires: 2027-01-31 }\n- { id: CVE-2, artifact: \"*\", reason: r, expires: 2026-12-01 }\n"
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
		{"- { id: CVE-1, artifact: os, package: p, path: /a, reason: r, expires: 2027-01-01 }\n- { id: CVE-1, artifact: os, package: p, path: /a, reason: s, expires: 2027-01-02 }", "CVE-1 in os (package p) (path /a) is listed twice (entry 1)"},
		// A wider entry beside a narrower one of the same id undoes its narrowing, either order.
		{"- { id: CVE-1, artifact: os, package: p, path: /a, reason: r, expires: 2027-01-01 }\n- { id: CVE-1, artifact: os, reason: s, expires: 2027-01-02 }", "entry 2 (CVE-1 in os) and entry 1 (CVE-1 in os (package p) (path /a)) can both accept one finding"},
		{"- { id: CVE-1, artifact: \"*\", reason: r, expires: 2027-01-01 }\n- { id: CVE-1, artifact: front, reason: s, expires: 2027-01-02 }", "can both accept one finding"},
		{"- { id: CVE-1, artifact: os, package: p, reason: r, expires: 2027-01-01 }\n- { id: CVE-1, artifact: os, package: p, path: /a, reason: s, expires: 2027-01-02 }", "can both accept one finding"},
		{"- { id: CVE-1, artifact: os, path: /a, reason: r, expires: 2027-01-01 }\n- { id: CVE-1, artifact: os, package: p, path: /a, reason: s, expires: 2027-01-02 }", "can both accept one finding"},
		// Partly overlapping: one narrowed by file, the other by package, both match p in /a.
		{"- { id: CVE-1, artifact: os, path: /a, reason: r, expires: 2027-01-01 }\n- { id: CVE-1, artifact: os, package: p, reason: s, expires: 2027-01-02 }", "can both accept one finding"},
		{"- { id: CVE-1, artifact: \"*\", path: /a, reason: r, expires: 2027-01-01 }\n- { id: CVE-1, artifact: os, package: p, path: /b, reason: s, expires: 2027-01-02 }\n- { id: CVE-1, artifact: os, package: p, reason: s, expires: 2027-01-02 }", "entry 3"},
		{"- { id: CVE-1, artifact: os, package: \" p\", reason: r, expires: 2027-01-01 }", "without spaces around it"},
		{"- { id: \"CVE-1 \", artifact: os, reason: r, expires: 2027-01-01 }", "id and artifact are written without spaces around them"},
		{"- { id: CVE-1, artifact: \" os\", reason: r, expires: 2027-01-01 }", "id and artifact are written without spaces around them"},
		{"- { id: CVE-1, artifact: os, path: usr/bin/ctr, reason: r, expires: 2027-01-01 }", "an absolute, clean path"},
		{"- { id: CVE-1, artifact: os, path: /usr/bin/../bin/ctr, reason: r, expires: 2027-01-01 }", "an absolute, clean path"},
		{"- { id: CVE-1, artifact: os, path: /usr/bin/, reason: r, expires: 2027-01-01 }", "an absolute, clean path"},
		{"- { id: CVE-1, artifact: os, path: \"/usr/bin/ctr \", reason: r, expires: 2027-01-01 }", "an absolute, clean path"},
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
	// One id in one artifact, narrowed to a package in two files, another package, and the same id
	// in another artifact whole: none accepts what another does.
	narrowed := "- { id: CVE-1, artifact: os, package: p, path: /usr/bin/ctr, reason: r, expires: 2027-01-01 }\n- { id: CVE-1, artifact: os, package: p, path: /usr/bin/runc, reason: r, expires: 2027-01-01 }\n" +
		"- { id: CVE-1, artifact: os, package: q, reason: r, expires: 2027-01-01 }\n- { id: CVE-1, artifact: api, reason: r, expires: 2027-01-01 }\n"
	if a, err := ParseAllowances([]byte(narrowed), policyNow); err != nil || len(a) != 4 || a[0].Package != "p" || a[0].Path != "/usr/bin/ctr" {
		t.Errorf("narrowed entries: %+v %v", a, err)
	}
}

func TestJudgeNarrowed(t *testing.T) {
	t.Parallel()
	net := func(path string) Finding {
		return Finding{ID: "GO-1", Severity: Critical, Package: "golang.org/x/net", Version: "0.58", Type: "go-module", Fixed: []string{"0.59"}, Path: path}
	}
	r := Report{Subjects: []Subject{
		{Name: "os", Findings: []Finding{net("/usr/bin/containerd"), net("/usr/bin/ctr"), {ID: "GO-1", Severity: Critical, Package: "stdlib", Version: "1.27.1", Type: "go-module", Fixed: []string{"1.27.2"}, Path: "/usr/bin/pebble"}}},
		{Name: "api", Findings: []Finding{net("")}},
	}}
	p := func(a ...Allowance) Policy { return Policy{FailOn: High, On: true, FixableOnly: true, Allow: a} }
	allow := func(art, pkg, path string) Allowance {
		return Allowance{ID: "GO-1", Artifact: art, Package: pkg, Path: path, Reason: "upstream", Expires: "2027-01-31"}
	}
	for _, c := range []struct {
		name, want string
		a          Allowance
	}{
		{"the artifact whole", "api/GO-1", allow("os", "", "")},
		{"its package", "os/GO-1 api/GO-1", allow("os", "golang.org/x/net", "")},
		{"a file of it", "os/GO-1 os/GO-1 api/GO-1", allow("os", "", "/usr/bin/ctr")},
		{"the package in that file", "os/GO-1 os/GO-1 api/GO-1", allow("os", "golang.org/x/net", "/usr/bin/ctr")},
		{"the package, not in that file", "os/GO-1 os/GO-1 os/GO-1 api/GO-1", allow("os", "golang.org/x/net", "/usr/bin/pebble")},
		{"another package", "os/GO-1 os/GO-1 os/GO-1 api/GO-1", allow("os", "golang.org/x/text", "")},
		{"a file of no package", "os/GO-1 os/GO-1 os/GO-1 api/GO-1", allow("os", "", "/usr/bin/docker")},
		{"every artifact's package, one with no file", "os/GO-1", allow("*", "golang.org/x/net", "")},
		{"every artifact's file: a finding with none is outside", "os/GO-1 os/GO-1 api/GO-1", allow("*", "", "/usr/bin/containerd")},
	} {
		if got := ids(r.Judge(p(c.a), policyNow)); got != c.want {
			t.Errorf("%s: blocked %q, want %q", c.name, got, c.want)
		}
	}
	// Narrowed to one file: the id's findings in the other files count, and the note says why; the
	// allowance applied is listed with the findings it accepted.
	g := r.Judge(p(allow("os", "golang.org/x/net", "/usr/bin/ctr"), allow("os", "", "/usr/bin/docker")), policyNow)
	if len(g.Applied) != 1 || g.Applied[0].Path != "/usr/bin/ctr" || g.Applied[0].Accepted != 1 || g.Allowed != 1 {
		t.Errorf("applied %+v, allowed %d", g.Applied, g.Allowed)
	}
	if len(g.Unused) != 1 || g.Unused[0].Path != "/usr/bin/docker" || len(g.Outside) != 2 || g.Outside[0].Entries != 2 {
		t.Errorf("unused %+v, outside %+v", g.Unused, g.Outside)
	}
	notes := strings.Join(g.Notes(), "\n")
	for _, want := range []string{
		"GO-1 in os (golang.org/x/net 0.58 in /usr/bin/containerd) counts: the 2 allowance(s) of it in force are narrowed to other packages or files",
		"GO-1 in os (stdlib 1.27.1 in /usr/bin/pebble) counts: the 2 allowance(s) of it in force are narrowed to other packages or files",
		"allowance GO-1 in os (path /usr/bin/docker) matches no finding: correct its package or path",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes %q lack %q", notes, want)
		}
	}
	if strings.Count(notes, "counts: the") != 2 || strings.Contains(notes, "in /usr/bin/ctr) counts") {
		t.Errorf("the finding an allowance accepted is named as counting: %q", notes)
	}
	acc := g.Accepted()
	if len(acc) != 1 || acc[0] != "allowance GO-1 in os (package golang.org/x/net) (path /usr/bin/ctr) until 2027-01-31 accepts 1 finding(s): upstream" {
		t.Errorf("accepted %q", acc)
	}
	// A narrowed allowance of a finding the policy does not count today (no fix yet) that matches
	// nothing is unused all the same: a typo'd path is reported whatever the policy counts.
	nofix := Report{Subjects: []Subject{{Name: "os", Findings: []Finding{{ID: "GO-1", Severity: Critical, Package: "stdlib", Path: "/usr/bin/pebble"}}}}}
	if g := nofix.Judge(p(allow("os", "stdlib", "/usr/bin/peble")), policyNow); len(g.Unused) != 1 || len(g.Applied) != 0 || len(g.Blocked) != 0 {
		t.Errorf("a typo'd path of an uncounted finding: %+v", g)
	}
	if g := nofix.Judge(p(allow("os", "stdlib", "/usr/bin/pebble")), policyNow); len(g.Unused) != 0 || len(g.Applied) != 1 || g.Applied[0].Accepted != 0 {
		t.Errorf("an allowance of an uncounted finding is applied, accepting none: %+v", g)
	}
	// One narrowed allowance: each finding of its id it does not cover is noted once, naming one.
	if g := r.Judge(p(allow("os", "", "/usr/bin/ctr")), policyNow); len(g.Outside) != 2 || g.Outside[0].Entries != 1 || g.Outside[1].Entries != 1 {
		t.Errorf("one narrowed allowance: outside %+v", g.Outside)
	}
	// An allowance of the id in another artifact is neither applied nor outside: matches nothing.
	if g := r.Judge(p(Allowance{ID: "GO-1", Artifact: "web", Reason: "r", Expires: "2027-01-31"}), policyNow); len(g.Unused) != 1 || len(g.Outside) != 0 || len(g.Applied) != 0 {
		t.Errorf("another artifact: %+v", g)
	}
	// An expired narrowed allowance is not named as outside: the expiry note covers it.
	old := allow("os", "", "/usr/bin/docker")
	old.Expires = "2026-11-01"
	if g := r.Judge(p(old), policyNow); len(g.Expired) != 1 || len(g.Outside) != 0 {
		t.Errorf("expired: %+v", g)
	}
	// A refused finding the SBOM names no file for: only an entry without path accepts it.
	if notes := strings.Join(r.Judge(p(allow("*", "", "/usr/bin/ctr")), policyNow).Notes(), "\n"); !strings.Contains(notes, "GO-1 in api (golang.org/x/net 0.58) counts: the 1 allowance(s) of it in force are narrowed to other packages or files; the SBOM names no file for it, so only an entry without path accepts it") {
		t.Errorf("notes %q", notes)
	}
	// A covering entry narrowed by file to another package: dropping its path would not accept the
	// finding either, so no word of a file.
	if notes := strings.Join(r.Judge(p(allow("*", "stdlib", "/usr/bin/ctr")), policyNow).Notes(), "\n"); !strings.Contains(notes, "GO-1 in api (golang.org/x/net 0.58) counts") || strings.Contains(notes, "names no file") {
		t.Errorf("notes %q", notes)
	}
	// Covering entries narrowed by package alone: no word of a file.
	if notes := strings.Join(r.Judge(p(allow("api", "golang.org/x/text", "")), policyNow).Notes(), "\n"); !strings.Contains(notes, "GO-1 in api (golang.org/x/net 0.58) counts") || strings.Contains(notes, "names no file") {
		t.Errorf("notes %q", notes)
	}
	// The refusal names the files of the finding.
	if err := r.Judge(p(), policyNow).Err(p()); err == nil || !strings.Contains(err.Error(), "critical GO-1 in os (golang.org/x/net 0.58 in /usr/bin/containerd, fixed in 0.59)") {
		t.Errorf("%v", err)
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
	// The grype the recipe builds is the one Grype pins, which the publisher's own audit runs.
	_, tag, _ := strings.Cut(Grype, ":v")
	v, _, _ := strings.Cut(tag, "@")
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(v) {
		t.Fatalf("Grype %s: no vX.Y.Z tag (%q)", Grype, v)
	}
	for _, want := range []string{"go get github.com/anchore/grype/cmd/grype@v" + v + " ", "main.version=" + v + "'", `$2 == "github.com/anchore/grype" && $3 == "v` + v + `"`, `ENTRYPOINT ["/grype"]`} {
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
