package sbom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
)

const grypeOut = `{"matches":[
 {"vulnerability":{"id":"CVE-2026-2","severity":"Medium","fix":{"versions":[]}},"artifact":{"name":"zlib","version":"1.3","type":"apk"}},
 {"vulnerability":{"id":"CVE-2026-1","severity":"Critical","fix":{"versions":["3.1"]}},"artifact":{"id":"Package-b","name":"openssl","version":"3.0","type":"apk"}},
 {"vulnerability":{"id":"CVE-2026-1","severity":"Critical","fix":{"versions":["3.1"]}},"artifact":{"id":"Package-a","name":"openssl","version":"3.0","type":"apk"}},
 {"vulnerability":{"id":"GHSA-x","severity":"weird","fix":{}},"artifact":{"name":"lodash","version":"4","type":"npm"}}
],"descriptor":{"db":{"status":{"schemaVersion":"v6.1.10","built":"2026-10-25T06:27:34Z","valid":true}}}}`

func TestParseGrype(t *testing.T) {
	t.Parallel()
	ev := map[string][]string{"Package-a": {"/usr/bin/a"}, "Package-b": {"/usr/bin/b", "/usr/bin/c"}}
	f, built, err := ParseGrype([]byte(grypeOut), ev)
	if err != nil {
		t.Fatal(err)
	}
	if built != "2026-10-25T06:27:34Z" || len(f) != 5 {
		t.Fatalf("%s %+v", built, f)
	}
	if f[0].ID != "CVE-2026-1" || f[0].Severity != Critical || !slices.Equal(f[0].Fixed, []string{"3.1"}) || f[3].Severity != Medium || f[4].Severity != Unknown {
		t.Errorf("sorted by severity: %+v", f)
	}
	if f[0].Path != "/usr/bin/a" || f[1].Path != "/usr/bin/b" || f[2].Path != "/usr/bin/c" || f[3].Path != "" {
		t.Errorf("one finding per file the package was found in (one SPDX package in two files is two), sorted; none where the SBOM says none: %+v", f)
	}
	// Without evidence the two matches are the same finding: one.
	if f, _, err := ParseGrype([]byte(grypeOut), nil); err != nil || len(f) != 3 {
		t.Errorf("duplicates dropped: %+v %v", f, err)
	}
	// The same match three times, at two severities, without a fix then with them: one finding,
	// at the highest severity, fixed by each version once, in the matches' sorted order.
	two := `{"matches":[
 {"vulnerability":{"id":"CVE-9","severity":"Critical","fix":{"versions":[]}},"artifact":{"name":"p","version":"1","type":"go-module"}},
 {"vulnerability":{"id":"CVE-9","severity":"Medium","fix":{"versions":["1.10"]}},"artifact":{"name":"p","version":"1","type":"go-module"}},
 {"vulnerability":{"id":"CVE-9","severity":"Critical","fix":{"versions":["1.9", "1.10"]}},"artifact":{"name":"p","version":"1","type":"go-module"}}]}`
	if f, _, err := ParseGrype([]byte(two), nil); err != nil || len(f) != 1 || f[0].Severity != Critical || !slices.Equal(f[0].Fixed, []string{"1.9", "1.10"}) {
		t.Errorf("a fix one match names is kept: %+v %v", f, err)
	}
	if _, _, err := ParseGrype([]byte("not json"), nil); err == nil {
		t.Error("not JSON passed")
	}
}

// spdxDoc is a syft-shaped SPDX document: a Go module found in two binaries, one in a third, a
// package with no file, and relationships Evidence must pass over.
const spdxDoc = `{"spdxVersion":"SPDX-2.3",
 "files":[{"fileName":"usr/bin/containerd","SPDXID":"SPDXRef-File-c"},{"fileName":"/usr/bin/ctr","SPDXID":"SPDXRef-File-t"},{"fileName":"usr/local/../bin/pebble","SPDXID":"SPDXRef-File-p"}],
 "relationships":[
  {"spdxElementId":"SPDXRef-Package-net","relatedSpdxElement":"SPDXRef-File-t","relationshipType":"OTHER","comment":"evident-by: indicates the package's existence is evident by the given file"},
  {"spdxElementId":"SPDXRef-Package-net","relatedSpdxElement":"SPDXRef-File-c","relationshipType":"OTHER","comment":"evident-by: indicates the package's existence is evident by the given file"},
  {"spdxElementId":"SPDXRef-Package-net","relatedSpdxElement":"SPDXRef-File-c","relationshipType":"OTHER","comment":"evident-by: twice"},
  {"spdxElementId":"SPDXRef-Package-std","relatedSpdxElement":"SPDXRef-File-p","relationshipType":"OTHER","comment":"evident-by: indicates the package's existence is evident by the given file"},
  {"spdxElementId":"SPDXRef-Package-std","relatedSpdxElement":"SPDXRef-File-c","relationshipType":"CONTAINS"},
  {"spdxElementId":"SPDXRef-Package-std","relatedSpdxElement":"SPDXRef-File-t","relationshipType":"OTHER","comment":"something else"},
  {"spdxElementId":"SPDXRef-Package-none","relatedSpdxElement":"SPDXRef-File-missing","relationshipType":"OTHER","comment":"evident-by: x"},
  {"spdxElementId":"SPDXRef-","relatedSpdxElement":"SPDXRef-File-c","relationshipType":"OTHER","comment":"evident-by: no package"}
 ]}`

func TestEvidence(t *testing.T) {
	t.Parallel()
	ev, err := Evidence([]byte(spdxDoc))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"Package-net": {"/usr/bin/containerd", "/usr/bin/ctr"}, "Package-std": {"/usr/bin/pebble"}}
	if len(ev) != len(want) {
		t.Fatalf("%v", ev)
	}
	for k, w := range want {
		if !slices.Equal(ev[k], w) {
			t.Errorf("%s: %v, want %v", k, ev[k], w)
		}
	}
	for _, bad := range []string{"not json", `{"files":[{"fileName":1}]}`, `{"relationships":{}}`} {
		if ev, err := Evidence([]byte(bad)); err == nil || ev != nil {
			t.Errorf("%s: %v %v", bad, ev, err)
		}
	}
}

func FuzzEvidence(f *testing.F) {
	f.Add([]byte(spdxDoc))
	f.Add([]byte(`{"files":null}`))
	f.Fuzz(func(t *testing.T, doc []byte) {
		ev, _ := Evidence(doc)
		for id, ps := range ev {
			if !slices.IsSorted(ps) || len(slices.Compact(slices.Clone(ps))) != len(ps) {
				t.Errorf("%s: not sorted once each: %v", id, ps)
			}
			for _, p := range ps {
				if !strings.HasPrefix(p, "/") || filepath.Clean(p) != p {
					t.Errorf("%s: %q is not absolute and clean", id, p)
				}
			}
		}
	})
}

func FuzzParseGrype(f *testing.F) {
	f.Add([]byte(grypeOut))
	f.Add([]byte(`{"matches":null}`))
	f.Add([]byte(`{"matches":[{"vulnerability":{"id":"X","fix":{"versions":["2","2"]}},"artifact":{"name":"p"}}]}`))
	f.Add([]byte(`{"matches":[{"vulnerability":{"id":"X","severity":"High","fix":{"versions":["2"]}},"artifact":{"id":"a","name":"p","version":"1"}},{"vulnerability":{"id":"X","severity":"Low","fix":{"versions":["3"]}},"artifact":{"id":"b","name":"p","version":"1"}}]}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		got, _, err := ParseGrype(raw, map[string][]string{"a": {"/x", "/y"}, "b": {"/y"}})
		if err != nil {
			return
		}
		type key struct{ id, pkg, version, typ, path string }
		seen := map[key]bool{}
		for _, f := range got {
			k := key{f.ID, f.Package, f.Version, f.Type, f.Path}
			if seen[k] {
				t.Errorf("a match twice: %+v", f)
			}
			seen[k] = true
			for i, v := range f.Fixed {
				if slices.Index(f.Fixed, v) != i {
					t.Errorf("a version twice: %v", f.Fixed)
				}
			}
		}
		// Every fix version of every match survives in its finding.
		var in struct {
			Matches []struct {
				Vulnerability struct {
					ID  string `json:"id"`
					Fix struct {
						Versions []string `json:"versions"`
					} `json:"fix"`
				} `json:"vulnerability"`
				Artifact struct {
					Name    string `json:"name"`
					Version string `json:"version"`
				} `json:"artifact"`
			} `json:"matches"`
		}
		if json.Unmarshal(raw, &in) != nil {
			return
		}
		for _, m := range in.Matches {
			for _, v := range m.Vulnerability.Fix.Versions {
				if !slices.ContainsFunc(got, func(f Finding) bool {
					return f.ID == m.Vulnerability.ID && f.Package == m.Artifact.Name && f.Version == m.Artifact.Version && slices.Contains(f.Fixed, v)
				}) {
					t.Errorf("fix %s of %s in %s dropped: %+v", v, m.Vulnerability.ID, m.Artifact.Name, got)
				}
			}
		}
		if !slices.IsSortedFunc(got, func(a, b Finding) int { return int(b.Severity) - int(a.Severity) }) {
			t.Errorf("not sorted by severity: %+v", got)
		}
	})
}

func TestSeverities(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]Severity{"Critical": Critical, "high": High, "MEDIUM": Medium, "Low": Low, "Negligible": Negligible, "Unknown": Unknown, "": Unknown, "bad": Unknown} {
		if got := ParseSeverity(name); got != want {
			t.Errorf("%q: %v", name, got)
		}
	}
	if _, on, err := FailOn("none"); on || err != nil {
		t.Errorf("none: %v %v", on, err)
	}
	if s, on, err := FailOn("High"); s != High || !on || err != nil {
		t.Errorf("high: %v %v %v", s, on, err)
	}
	for _, bad := range []string{"unknown", "severe", ""} {
		if _, _, err := FailOn(bad); err == nil {
			t.Errorf("--fail-on %q passed", bad)
		}
	}
	// Both sides of the threshold.
	r := Report{Subjects: []Subject{{Name: "web", Findings: []Finding{{ID: "a", Severity: High}}}}}
	if r.Failed(Critical, true) || !r.Failed(High, true) || r.Failed(Low, false) {
		t.Error("the threshold")
	}
	if err := r.Verdict(High, true); err == nil || !strings.Contains(err.Error(), "at or above high") {
		t.Errorf("verdict: %v", err)
	}
	r.Subjects = append(r.Subjects, Subject{Name: "os", Error: "no SBOM"})
	if err := r.Verdict(Low, false); err == nil || !strings.Contains(err.Error(), "not audited: os") {
		t.Errorf("an artifact not audited fails whatever the threshold: %v", err)
	}
}

func TestReportPrint(t *testing.T) {
	t.Parallel()
	r := Report{Project: "notes", Version: "1.0.0", Revision: 1, Channel: "stable", Tag: "stable-1.0.0", DBBuilt: "2026-10-25",
		Subjects: []Subject{
			{Name: "web", Digest: "sha256:1", Findings: []Finding{{ID: "CVE-1", Severity: Low, Package: "zlib", Version: "1", Type: "apk"}}},
			{Name: "os", Digest: "sha256:2", Findings: []Finding{{ID: "CVE-2", Severity: Critical, Package: "openssl", Version: "3", Type: "deb", Fixed: []string{"3.1"}}}},
			{Name: "installer", Digest: "sha256:3", Error: "no SBOM"},
		}}
	var b bytes.Buffer
	if err := r.Print(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"notes 1.0.0 (revision 1, stable, stable-1.0.0), vulnerability database built 2026-10-25\n", "installer  sha256:3  not audited: no SBOM",
		"critical   os        CVE-2", "openssl 3 (deb)", "fixed in 3.1", "no fix", "3 artifacts, vulnerabilities: 1 critical, 1 low\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "CVE-2") > strings.Index(out, "CVE-1") {
		t.Errorf("not highest first:\n%s", out)
	}
	b.Reset()
	if err := r.JSON(&b); err != nil || !strings.Contains(b.String(), `"severity": "critical"`) {
		t.Errorf("JSON: %v %s", err, b.String())
	}
}

// fakeDocker records each run and answers with out (or fails).
type fakeDocker struct {
	runs [][]string
	out  map[string]string // by the word that picks it: "syft", "sbom:", "import"
	err  error
}

func (f *fakeDocker) run(_ context.Context, stdout io.Writer, args ...string) error {
	f.runs = append(f.runs, args)
	if f.err != nil {
		return f.err
	}
	for k, v := range f.out {
		if slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, k) }) {
			_, err := io.WriteString(stdout, v)
			return err
		}
	}
	return nil
}

func TestGenerate(t *testing.T) {
	t.Parallel()
	f := &fakeDocker{out: map[string]string{"syft": `{"spdxVersion":"SPDX-2.3"}`}}
	doc, err := Generate(context.Background(), f.run, Source{Ref: "localhost:5000/a/b@sha256:1", Insecure: true, Config: "/cfg"})
	if err != nil || string(doc) != `{"spdxVersion":"SPDX-2.3"}` {
		t.Fatalf("%s %v", doc, err)
	}
	args := strings.Join(f.runs[0], " ")
	for _, want := range []string{"-e DOCKER_CONFIG=/config --mount type=bind,src=/cfg,dst=/config,readonly", "SYFT_REGISTRY_INSECURE_USE_HTTP=true", Syft + " registry:localhost:5000/a/b@sha256:1 --platform linux/amd64 --select-catalogers +sbom-cataloger,-linux-kernel-cataloger -o spdx-json"} {
		if !strings.Contains(args, want) {
			t.Errorf("lacks %q: %s", want, args)
		}
	}
	f = &fakeDocker{out: map[string]string{"syft": `{"bomFormat":"CycloneDX"}`}}
	if _, err := Generate(context.Background(), f.run, Source{Ref: "r"}); err == nil || !strings.Contains(err.Error(), "not an SPDX") {
		t.Errorf("not SPDX: %v", err)
	}
	if args := strings.Join(f.runs[0], " "); strings.Contains(args, "INSECURE") || strings.Contains(args, "DOCKER_CONFIG") {
		t.Errorf("HTTPS, no config: %s", args)
	}
	f = &fakeDocker{err: errors.New("boom")}
	if _, err := Generate(context.Background(), f.run, Source{Ref: "r"}); err == nil {
		t.Error("a failed syft passed")
	}
}

func TestCappedOutput(t *testing.T) {
	t.Parallel()
	c := &capped{max: 4}
	c.Write([]byte("abc"))
	if c.over {
		t.Error("under the cap")
	}
	c.Write([]byte("de"))
	if !c.over || c.String() != "abcd" {
		t.Errorf("over the cap: %v %q", c.over, c.String())
	}
}

func TestScanner(t *testing.T) {
	t.Parallel()
	cache := t.TempDir()
	f := &fakeDocker{out: map[string]string{"sbom:": grypeOut}}
	s := Scanner{Docker: f.run, Image: "reg/notes-audit@sha256:9", Cache: cache, User: "1000:1000"}
	got, built, err := s.Scan(context.Background(), []byte(`{"spdxVersion":"SPDX-2.3"}`))
	if err != nil || len(got) != 3 || built == "" {
		t.Fatalf("%+v %s %v", got, built, err)
	}
	args := strings.Join(f.runs[0], " ")
	for _, want := range []string{"-e TMPDIR=/db/tmp", "-e GRYPE_DB_CACHE_DIR=/db/cache", "--mount type=bind,src=" + cache + ",dst=/db ", "--user 1000:1000", "reg/notes-audit@sha256:9 sbom:/in/sbom.json -o json"} {
		if !strings.Contains(args, want) {
			t.Errorf("lacks %q: %s", want, args)
		}
	}
	if !strings.Contains(args, "GRYPE_DB_AUTO_UPDATE=false") {
		t.Errorf("a scan checks for a newer database: %s", args)
	}
	if left, _ := os.ReadDir(filepath.Join(cache, "tmp")); len(left) != 0 {
		t.Errorf("left %v", left)
	}
	if err := s.Import(context.Background()); err != nil || len(f.runs) != 2 || !strings.HasSuffix(strings.Join(f.runs[1], " "), " db update") {
		t.Errorf("no --db, one update: %v %v", err, f.runs)
	}
	// grype's archives are named by their time: a colon in the path.
	db := filepath.Join(t.TempDir(), "vulnerability-db_v6_2026-10-25T00:33:23Z.tar.zst")
	if err := os.WriteFile(db, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.DB = db
	if err := s.Import(context.Background()); err != nil {
		t.Fatal(err)
	}
	if args := strings.Join(f.runs[2], " "); !strings.Contains(args, "GRYPE_DB_VALIDATE_AGE=false") || !strings.Contains(args, "--mount type=bind,src="+db+",dst=/in/db,readonly") || !strings.HasSuffix(args, "db import /in/db") || !strings.Contains(args, "GRYPE_DB_AUTO_UPDATE=false") {
		t.Errorf("import: %s", args)
	}
	s.DB = filepath.Join(t.TempDir(), "a,b")
	if err := os.WriteFile(s.DB, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Import(context.Background()); err == nil || !strings.Contains(err.Error(), "comma") {
		t.Errorf("a path with a comma: %v", err)
	}
	s.DB = filepath.Join(t.TempDir(), "missing")
	if err := s.Import(context.Background()); err == nil || !strings.Contains(err.Error(), "--db") {
		t.Errorf("a missing --db: %v", err)
	}
}

func TestTargetsAndAudit(t *testing.T) {
	t.Parallel()
	a := release.Artifacts{Images: map[string]string{"web": "sha256:" + strings.Repeat("b", 64), "api": "sha256:" + strings.Repeat("a", 64)},
		Installer: "sha256:" + strings.Repeat("c", 64), OS: "sha256:" + strings.Repeat("d", 64)}
	targets, err := Targets("reg.example/acme", "notes", a)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, x := range targets {
		names = append(names, x.Name+"="+x.Digest.Context().String())
	}
	if want := []string{"api=reg.example/acme/notes-api", "web=reg.example/acme/notes-web", "installer=reg.example/acme/notes-installer", "os=reg.example/acme/notes-os"}; !slices.Equal(names, want) {
		t.Errorf("%v", names)
	}
	f := &fakeDocker{out: map[string]string{"sbom:": grypeOut}}
	read := func(_ context.Context, d name.Digest) ([]byte, error) {
		switch {
		case strings.Contains(d.String(), "notes-web"):
			return nil, errors.New("no SBOM attested by a trusted key")
		case strings.Contains(d.String(), "notes-installer"):
			return []byte(`{"not":"spdx"}`), nil
		}
		return []byte(`{"spdxVersion":"SPDX-2.3","name":"` + d.Context().String() + `"}`), nil
	}
	r, err := Audit(context.Background(), read, Scanner{Docker: f.run, Cache: t.TempDir()}, targets, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Subjects) != 4 || r.Subjects[1].Error == "" || r.Subjects[2].Error == "" || r.Subjects[0].Error != "" || len(r.Subjects[3].Findings) != 3 || r.DBBuilt == "" {
		t.Fatalf("%+v", r)
	}
	if len(f.runs) != 3 || !strings.HasSuffix(strings.Join(f.runs[0], " "), " db update") {
		t.Errorf("one update, then the two with an SBOM scanned: %v", f.runs)
	}
}

// The scanner's key is reserved: no project image takes it.
func TestTheScannersKeyIsReserved(t *testing.T) {
	t.Parallel()
	if !slices.Contains(spec.ReservedImageKeys, Key) {
		t.Errorf("%q is not in spec.ReservedImageKeys", Key)
	}
}

func TestTailKeepsTheEnd(t *testing.T) {
	t.Parallel()
	var w tail
	w.Write([]byte(strings.Repeat("x", 20<<10)))
	w.Write([]byte("\nError: the last line\n"))
	if s := w.String(); len(s) != 16<<10 || !strings.HasSuffix(s, "Error: the last line\n") {
		t.Errorf("%d bytes, ends %q", len(s), s[len(s)-30:])
	}
}

func TestNothingToScanReadiesNoDatabase(t *testing.T) {
	t.Parallel()
	f := &fakeDocker{}
	read := func(context.Context, name.Digest) ([]byte, error) { return nil, errors.New("no SBOM") }
	targets, _ := Targets("r.example/a", "notes", release.Artifacts{Installer: "sha256:" + strings.Repeat("c", 64)})
	r, err := Audit(context.Background(), read, Scanner{Docker: f.run, Cache: t.TempDir()}, targets, nil)
	if err != nil || len(f.runs) != 0 || r.Subjects[0].Error != "no SBOM" {
		t.Errorf("%+v %v %v", r, err, f.runs)
	}
}

func TestLastLines(t *testing.T) {
	t.Parallel()
	if got := lastLines("a\n\nb\nc\n", 2); got != "b / c" {
		t.Errorf("%q", got)
	}
	if got := lastLines("", 5); got != "" {
		t.Errorf("%q", got)
	}
}

// A database that cannot be made ready leaves the SBOMs read unscanned, named, and the targets
// without one as they were.
func TestADatabaseNotReadyKeepsTheFindings(t *testing.T) {
	t.Parallel()
	f := &fakeDocker{err: errors.New("no route to grype")}
	targets, _ := Targets("r.example/a", "notes", release.Artifacts{Installer: "sha256:" + strings.Repeat("c", 64), OS: "sha256:" + strings.Repeat("d", 64)})
	read := func(_ context.Context, d name.Digest) ([]byte, error) {
		if strings.Contains(d.String(), "notes-os") {
			return nil, errors.New("no SBOM")
		}
		return []byte(`{"spdxVersion":"SPDX-2.3"}`), nil
	}
	r, err := Audit(context.Background(), read, Scanner{Docker: f.run, Cache: t.TempDir()}, targets, nil)
	if err != nil || !strings.Contains(r.Subjects[0].Error, "the scan: grype db update") || r.Subjects[1].Error != "no SBOM" {
		t.Errorf("%+v %v", r, err)
	}
}

// An export writes each verified document and an index; an artifact without one is in the index
// with its error, and the export fails once the rest is written.
func TestExport(t *testing.T) {
	t.Parallel()
	targets, _ := Targets("r.example/a", "notes", release.Artifacts{Images: map[string]string{"api": "sha256:" + strings.Repeat("a", 64)},
		Installer: "sha256:" + strings.Repeat("c", 64), OS: "sha256:" + strings.Repeat("d", 64)})
	read := func(_ context.Context, d name.Digest) ([]byte, error) {
		if strings.Contains(d.String(), "notes-os") {
			return nil, errors.New("no SBOM")
		}
		return []byte(`{"spdxVersion":"SPDX-2.3","name":"` + d.String() + `"}`), nil
	}
	dir := filepath.Join(t.TempDir(), "out")
	idx, err := Export(context.Background(), read, targets, dir, Index{Project: "notes", Version: "1.0.0", Revision: 1})
	if err == nil || !strings.Contains(err.Error(), "for os") {
		t.Fatalf("the OS without one: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "api.spdx.json"))
	if !strings.Contains(string(raw), "notes-api@sha256:aaa") {
		t.Errorf("api: %s", raw)
	}
	var back Index
	b, _ := os.ReadFile(filepath.Join(dir, IndexFile))
	if json.Unmarshal(b, &back) != nil || len(back.Artifacts) != 3 || back.Artifacts[2].Error != "no SBOM" || back.Artifacts[0].File != "api.spdx.json" || back.Format != "spdx-2.3+json" {
		t.Errorf("index: %s", b)
	}
	if len(idx.Artifacts) != 3 {
		t.Errorf("%+v", idx)
	}
}

// Stowage's own release makes its binary's SBOM with the syft every release's SBOMs are made with.
func TestTheReleaseWorkflowTakesThisSyft(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), Syft+" \\\n") {
		t.Errorf("release.yml does not run %s", Syft)
	}
}

// A finding in several artifacts (a bundle and its disks share their SBOM) counts once.
func TestCountsAreDistinct(t *testing.T) {
	t.Parallel()
	f := Finding{ID: "CVE-1", Severity: Critical, Package: "openssl", Version: "3", Type: "deb"}
	r := Report{Subjects: []Subject{{Name: "os", Findings: []Finding{f}}, {Name: "disk-qcow2", Findings: []Finding{f}}, {Name: "web", Findings: []Finding{{ID: "CVE-1", Severity: Critical, Package: "openssl", Version: "1.1", Type: "apk"}}}}}
	if n := r.Counts()[Critical]; n != 2 {
		t.Errorf("%d critical, want 2", n)
	}
}
