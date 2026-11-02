package sbom

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Allowance accepts one finding (ID) in one artifact (Artifact: a subject's name, or "*" for every
// artifact) until the end of Expires (YYYY-MM-DD, UTC), for Reason.
type Allowance struct {
	ID       string `yaml:"id" json:"id"`
	Artifact string `yaml:"artifact" json:"artifact"`
	// Package, when set, narrows the entry to the finding in that package (its name as the
	// audit prints it); Path to the package found in that file (absolute, as the audit prints
	// it after "in"). Either alone narrows on its own.
	Package string `yaml:"package,omitempty" json:"package,omitempty"`
	Path    string `yaml:"path,omitempty" json:"path,omitempty"`
	Reason  string `yaml:"reason" json:"reason"`
	Expires string `yaml:"expires" json:"expires"`
}

// MaxAllowance is how far ahead an allowance may expire: a finding is accepted for a while,
// never for good.
const MaxAllowance = 366 * 24 * time.Hour

// ExpiryNotice is how long before an allowance's end lint warns of it.
const ExpiryNotice = 30 * 24 * time.Hour

// end is the first instant the allowance no longer holds.
func (a Allowance) end() time.Time {
	d, _ := time.Parse(time.DateOnly, a.Expires)
	return d.Add(24 * time.Hour)
}

// Holds: a is in force at now.
func (a Allowance) Holds(now time.Time) bool { return now.Before(a.end()) }

func (a Allowance) matches(subject string, f Finding) bool {
	return a.covers(subject, f) && (a.Package == "" || a.Package == f.Package) && (a.Path == "" || a.Path == f.Path)
}

// covers: f is a's finding in a's artifact, whatever its package and file.
func (a Allowance) covers(subject string, f Finding) bool {
	return a.ID == f.ID && (a.Artifact == "*" || a.Artifact == subject)
}

// overlaps: one finding can match both a and b (the same id, and artifacts, packages and files
// that are equal or one of them any).
func (a Allowance) overlaps(b Allowance) bool {
	either := func(x, y, any string) bool { return x == any || y == any || x == y }
	return a.ID == b.ID && either(a.Artifact, b.Artifact, "*") && either(a.Package, b.Package, "") && either(a.Path, b.Path, "")
}

// String names the allowance as its file does: the id, the artifact, and the package and file it
// is narrowed to.
func (a Allowance) String() string {
	s := a.ID + " in " + a.Artifact
	if a.Package != "" {
		s += " (package " + a.Package + ")"
	}
	if a.Path != "" {
		s += " (path " + a.Path + ")"
	}
	return s
}

// ParseAllowances reads an allowlist file (a YAML list of id, artifact, package, path, reason,
// expires), strictly: each field but package and path is required, a path absolute and clean,
// the date valid and at most MaxAllowance after now, and no two entries can accept one finding
// (a wider one would undo the other's narrowing).
func ParseAllowances(doc []byte, now time.Time) ([]Allowance, error) {
	dec := yaml.NewDecoder(bytes.NewReader(doc))
	dec.KnownFields(true)
	var out []Allowance
	if err := dec.Decode(&out); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	for i, a := range out {
		at := fmt.Sprintf("entry %d", i+1)
		switch {
		case a.ID == "":
			return nil, fmt.Errorf("%s: id is required", at)
		case a.Artifact == "":
			return nil, fmt.Errorf("%s (%s): artifact is required (an artifact's name, or \"*\")", at, a.ID)
		case strings.TrimSpace(a.Reason) == "":
			return nil, fmt.Errorf("%s (%s): reason is required", at, a.ID)
		case a.ID != strings.TrimSpace(a.ID) || a.Artifact != strings.TrimSpace(a.Artifact):
			return nil, fmt.Errorf("%s (%q in %q): id and artifact are written without spaces around them", at, a.ID, a.Artifact)
		case a.Package != strings.TrimSpace(a.Package):
			return nil, fmt.Errorf("%s (%s): package %q: a package's name as the audit prints it, without spaces around it", at, a.ID, a.Package)
		case a.Path != "" && (!strings.HasPrefix(a.Path, "/") || path.Clean(a.Path) != a.Path || strings.TrimSpace(a.Path) != a.Path):
			return nil, fmt.Errorf("%s (%s): path %q: an absolute, clean path, as the audit prints it after \"in\" (/usr/bin/containerd)", at, a.ID, a.Path)
		}
		if _, err := time.Parse(time.DateOnly, a.Expires); err != nil {
			return nil, fmt.Errorf("%s (%s): expires %q: a date, YYYY-MM-DD", at, a.ID, a.Expires)
		}
		if a.end().After(now.Add(MaxAllowance)) {
			return nil, fmt.Errorf("%s (%s): expires %s: more than a year ahead; an allowance is renewed, never kept for good", at, a.ID, a.Expires)
		}
		for j, b := range out[:i] {
			switch {
			case a.ID == b.ID && a.Artifact == b.Artifact && a.Package == b.Package && a.Path == b.Path:
				return nil, fmt.Errorf("%s: %s is listed twice (entry %d)", at, a, j+1)
			case a.overlaps(b):
				return nil, fmt.Errorf("%s (%s) and entry %d (%s) can both accept one finding: narrow them apart (artifact, package, path) so each finding has one entry, or keep one", at, a, j+1, b)
			}
		}
	}
	return out, nil
}

// Policy is the gate a release passes before a channel points at it: a finding at or above
// FailOn (when On), fixable unless FixableOnly is off, and accepted by no allowance in force.
type Policy struct {
	FailOn      Severity
	On          bool
	FixableOnly bool
	Allow       []Allowance
}

// Blocked is a finding the policy refuses, in its subject.
type Blocked struct {
	Subject string
	Finding Finding
}

// Gate is a report judged by a policy at a time.
type Gate struct {
	// NotAudited are the subjects with no SBOM by a trusted key, or whose scan failed.
	NotAudited []string
	Blocked    []Blocked
	// Allowed counts the findings an allowance accepted; Applied are the allowances in force that
	// matched a finding (in the file's order), with the findings each accepted (none when the
	// policy counts none of them today). Expired and Unused are the allowances out of force and
	// those that matched nothing, which the publisher removes or renews; Outside are the refused
	// findings an allowance names in its artifact but not in its package or file.
	Allowed int
	Applied []Applied
	Expired []Allowance
	Unused  []Allowance
	Outside []Outside
}

// Applied is an allowance in force that matched a finding, and how many it accepted.
type Applied struct {
	Allowance
	Accepted int
}

// Outside is a refused finding that Entries allowances in force name (its id, in its artifact),
// each narrowed to another package or file.
type Outside struct {
	Blocked
	Entries int
	// Pathed: one of them is narrowed to a file alone (its package, if any, the finding's), so
	// dropping its path would accept the finding.
	Pathed bool
}

// Judge applies p to r at now.
func (r Report) Judge(p Policy, now time.Time) Gate {
	var g Gate
	used := make([]bool, len(p.Allow))
	accepted := make([]int, len(p.Allow))
	for _, s := range r.Subjects {
		if s.Error != "" {
			g.NotAudited = append(g.NotAudited, s.Name)
		}
		for _, f := range s.Findings {
			// An allowance matching a finding is in use, whether or not the policy counts the
			// finding today (one with no fix yet counts once a fix is published).
			var in []int
			for i, a := range p.Allow {
				if a.matches(s.Name, f) {
					used[i] = true
					if a.Holds(now) {
						in = append(in, i)
					}
				}
			}
			if !p.On || f.Severity < p.FailOn || (p.FixableOnly && !f.Fixable()) {
				continue
			}
			if len(in) > 0 {
				g.Allowed++
				for _, i := range in {
					accepted[i]++
				}
				continue
			}
			b := Blocked{s.Name, f}
			g.Blocked = append(g.Blocked, b)
			o := Outside{Blocked: b}
			for _, a := range p.Allow {
				if a.Holds(now) && a.covers(s.Name, f) {
					o.Entries++
					o.Pathed = o.Pathed || a.Path != "" && (a.Package == "" || a.Package == f.Package)
				}
			}
			if o.Entries > 0 {
				g.Outside = append(g.Outside, o)
			}
		}
	}
	for i, a := range p.Allow {
		switch {
		case !a.Holds(now):
			g.Expired = append(g.Expired, a)
		case used[i]:
			g.Applied = append(g.Applied, Applied{a, accepted[i]})
		default:
			g.Unused = append(g.Unused, a)
		}
	}
	return g
}

// Err is the gate's refusal, nil when the release passes: an artifact not audited, or a finding
// the policy refuses (the first few named).
func (g Gate) Err(p Policy) error {
	if len(g.NotAudited) > 0 {
		return fmt.Errorf("audit: not audited: %s", strings.Join(g.NotAudited, ", "))
	}
	if len(g.Blocked) == 0 {
		return nil
	}
	const shown = 5
	var names []string
	for _, b := range g.Blocked[:min(len(g.Blocked), shown)] {
		names = append(names, fmt.Sprintf("%s %s in %s (%s %s%s, %s)", b.Finding.Severity, b.Finding.ID, b.Subject, b.Finding.Package, b.Finding.Version, inText(b.Finding), fixText(b.Finding)))
	}
	if more := len(g.Blocked) - shown; more > 0 {
		names = append(names, fmt.Sprintf("and %d more", more))
	}
	what := "vulnerabilities"
	if p.FixableOnly {
		what = "fixable vulnerabilities"
	}
	return fmt.Errorf("audit: %d %s at or above %s: %s", len(g.Blocked), what, p.FailOn, strings.Join(names, "; "))
}

// Notes are the allowances the publisher should look at: expired, matching nothing, or naming a
// refused finding outside the package or file they are narrowed to.
func (g Gate) Notes() []string {
	var out []string
	for _, a := range g.Expired {
		out = append(out, fmt.Sprintf("allowance %s expired on %s: the finding counts again whenever the policy counts it; renew the allowance, fix the finding, or remove the allowance", a, a.Expires))
	}
	for _, a := range g.Unused {
		if a.Package == "" && a.Path == "" {
			out = append(out, fmt.Sprintf("allowance %s matches no finding: remove it", a))
			continue
		}
		out = append(out, fmt.Sprintf("allowance %s matches no finding: correct its package or path (audit prints each finding's package and files), or remove it", a))
	}
	for _, o := range g.Outside {
		n := fmt.Sprintf("%s in %s (%s %s%s) counts: the %d allowance(s) of it in force are narrowed to other packages or files", o.Finding.ID, o.Subject, o.Finding.Package, o.Finding.Version, inText(o.Finding), o.Entries)
		if o.Finding.Path == "" && o.Pathed {
			n += "; the SBOM names no file for it, so only an entry without path accepts it"
		}
		out = append(out, n)
	}
	return out
}

// Accepted are the allowances the gate applied, one line each: what, until when, how many findings
// it accepted, and why.
func (g Gate) Accepted() []string {
	var out []string
	for _, a := range g.Applied {
		out = append(out, fmt.Sprintf("allowance %s until %s accepts %d finding(s): %s", a.Allowance, a.Expires, a.Accepted, strings.TrimSpace(a.Reason)))
	}
	return out
}

func fixText(f Finding) string {
	if f.Fixable() {
		return "fixed in " + strings.Join(f.Fixed, ", ")
	}
	return "no fix"
}
