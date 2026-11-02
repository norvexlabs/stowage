package sbom

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Stowage's artifacts in a release: the images Stowage builds from its own recipes (front, backup,
// chown, audit) and the installer. Every other subject is the project's, the OS bundle and its
// disks included: the project's pins (shapes.kairos's base and kairos_init) decide what they hold.
var stowageSubjects = []string{"front", "backup", "chown", Key, "installer"}

// Owner is who fixes a subject's findings: "stowage" for what Stowage supplies, else "project".
func Owner(subject string) string {
	if slices.Contains(stowageSubjects, subject) {
		return "stowage"
	}
	return "project"
}

// Fixable: a fixed version of the package is published.
func (f Finding) Fixable() bool { return len(f.Fixed) > 0 }

// Allowance accepts one finding (ID) in one artifact (Artifact: a subject's name, or "*" for every
// artifact) until the end of Expires (YYYY-MM-DD, UTC), for Reason.
type Allowance struct {
	ID       string `yaml:"id" json:"id"`
	Artifact string `yaml:"artifact" json:"artifact"`
	Reason   string `yaml:"reason" json:"reason"`
	Expires  string `yaml:"expires" json:"expires"`
}

// MaxAllowance is how far ahead an allowance may expire: a finding is accepted for a while,
// never for good.
const MaxAllowance = 366 * 24 * time.Hour

// end is the first instant the allowance no longer holds.
func (a Allowance) end() time.Time {
	d, _ := time.Parse(time.DateOnly, a.Expires)
	return d.Add(24 * time.Hour)
}

// Holds: a is in force at now.
func (a Allowance) Holds(now time.Time) bool { return now.Before(a.end()) }

func (a Allowance) matches(subject string, f Finding) bool {
	return a.ID == f.ID && (a.Artifact == "*" || a.Artifact == subject)
}

// ParseAllowances reads an allowlist file (a YAML list of id, artifact, reason, expires), strictly:
// each field is required, the date valid and at most MaxAllowance after now, and no entry is
// repeated.
func ParseAllowances(doc []byte, now time.Time) ([]Allowance, error) {
	dec := yaml.NewDecoder(bytes.NewReader(doc))
	dec.KnownFields(true)
	var out []Allowance
	if err := dec.Decode(&out); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	seen := map[[2]string]bool{}
	for i, a := range out {
		at := fmt.Sprintf("entry %d", i+1)
		switch {
		case a.ID == "":
			return nil, fmt.Errorf("%s: id is required", at)
		case a.Artifact == "":
			return nil, fmt.Errorf("%s (%s): artifact is required (an artifact's name, or \"*\")", at, a.ID)
		case strings.TrimSpace(a.Reason) == "":
			return nil, fmt.Errorf("%s (%s): reason is required", at, a.ID)
		}
		if _, err := time.Parse(time.DateOnly, a.Expires); err != nil {
			return nil, fmt.Errorf("%s (%s): expires %q: a date, YYYY-MM-DD", at, a.ID, a.Expires)
		}
		if a.end().After(now.Add(MaxAllowance)) {
			return nil, fmt.Errorf("%s (%s): expires %s: more than a year ahead; an allowance is renewed, never kept for good", at, a.ID, a.Expires)
		}
		k := [2]string{a.ID, a.Artifact}
		if seen[k] {
			return nil, fmt.Errorf("%s: %s in %s is listed twice", at, a.ID, a.Artifact)
		}
		seen[k] = true
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
	// Allowed counts the findings an allowance accepted; Expired and Unused are the allowances
	// out of force and those that matched nothing, which the publisher removes or renews.
	Allowed int
	Expired []Allowance
	Unused  []Allowance
}

// Judge applies p to r at now.
func (r Report) Judge(p Policy, now time.Time) Gate {
	var g Gate
	used := make([]bool, len(p.Allow))
	for _, s := range r.Subjects {
		if s.Error != "" {
			g.NotAudited = append(g.NotAudited, s.Name)
		}
		for _, f := range s.Findings {
			// An allowance matching a finding is in use, whether or not the policy counts the
			// finding today (one with no fix yet counts once a fix is published).
			allowed := false
			for i, a := range p.Allow {
				if a.matches(s.Name, f) {
					used[i] = true
					if a.Holds(now) {
						allowed = true
					}
				}
			}
			if !p.On || f.Severity < p.FailOn || (p.FixableOnly && !f.Fixable()) {
				continue
			}
			if allowed {
				g.Allowed++
				continue
			}
			g.Blocked = append(g.Blocked, Blocked{s.Name, f})
		}
	}
	for i, a := range p.Allow {
		switch {
		case !a.Holds(now):
			g.Expired = append(g.Expired, a)
		case !used[i]:
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
		names = append(names, fmt.Sprintf("%s %s in %s (%s %s, %s)", b.Finding.Severity, b.Finding.ID, b.Subject, b.Finding.Package, b.Finding.Version, fixText(b.Finding)))
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

// Notes are the allowances the publisher should look at: expired, or matching nothing.
func (g Gate) Notes() []string {
	var out []string
	for _, a := range g.Expired {
		out = append(out, fmt.Sprintf("allowance %s in %s expired on %s: the finding counts again whenever the policy counts it; renew the allowance, fix the finding, or remove the allowance", a.ID, a.Artifact, a.Expires))
	}
	for _, a := range g.Unused {
		out = append(out, fmt.Sprintf("allowance %s in %s matches no finding: remove it", a.ID, a.Artifact))
	}
	return out
}

func fixText(f Finding) string {
	if f.Fixable() {
		return "fixed in " + strings.Join(f.Fixed, ", ")
	}
	return "no fix"
}
