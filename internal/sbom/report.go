package sbom

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
)

// Severity is a finding's, as grype rates it; ordered, Unknown lowest.
type Severity int

const (
	Unknown Severity = iota
	Negligible
	Low
	Medium
	High
	Critical
)

var severityNames = []string{"unknown", "negligible", "low", "medium", "high", "critical"}

func (s Severity) String() string {
	if s < 0 || int(s) >= len(severityNames) {
		return "unknown"
	}
	return severityNames[s]
}

func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

func (s *Severity) UnmarshalText(b []byte) error { *s = ParseSeverity(string(b)); return nil }

// ParseSeverity reads grype's name of a severity, in any case; another is Unknown.
func ParseSeverity(name string) Severity {
	if i := slices.Index(severityNames, strings.ToLower(name)); i >= 0 {
		return Severity(i)
	}
	return Unknown
}

// FailOn reads --fail-on: a severity's name, or "none".
func FailOn(name string) (Severity, bool, error) {
	if name == "none" {
		return 0, false, nil
	}
	i := slices.Index(severityNames, strings.ToLower(name))
	if i < 1 {
		return 0, false, fmt.Errorf("--fail-on %q: none, %s", name, strings.Join(severityNames[1:], ", "))
	}
	return Severity(i), true, nil
}

// Finding is a vulnerability grype matched to a package.
type Finding struct {
	ID       string   `json:"id"`
	Severity Severity `json:"severity"`
	Package  string   `json:"package"`
	Version  string   `json:"version"`
	Type     string   `json:"type"`
	Fixed    []string `json:"fixed_in,omitempty"`
}

func sortFindings(f []Finding) {
	slices.SortStableFunc(f, func(a, b Finding) int {
		return cmp.Or(cmp.Compare(b.Severity, a.Severity), cmp.Compare(a.ID, b.ID), cmp.Compare(a.Package, b.Package), cmp.Compare(a.Version, b.Version))
	})
}

// Subject is one artifact of the release audited: its SBOM's findings, or why it has none.
type Subject struct {
	Name     string    `json:"name"` // the image key, installer or os
	Digest   string    `json:"digest"`
	Error    string    `json:"error,omitempty"`
	Findings []Finding `json:"findings"`
}

// Report is an audit of one release.
type Report struct {
	Project  string    `json:"project"`
	Version  string    `json:"version"`
	Revision int       `json:"revision"`
	Channel  string    `json:"channel"`
	Tag      string    `json:"tag"` // the release manifest's
	DBBuilt  string    `json:"db_built,omitempty"`
	Subjects []Subject `json:"subjects"`
}

// Counts are the distinct findings by severity: one in several artifacts (a bundle and its disks
// share their SBOM) counts once.
func (r Report) Counts() map[Severity]int {
	n := map[Severity]int{}
	seen := map[[4]string]bool{}
	for _, s := range r.Subjects {
		for _, f := range s.Findings {
			k := [4]string{f.ID, f.Package, f.Version, f.Type}
			if !seen[k] {
				seen[k] = true
				n[f.Severity]++
			}
		}
	}
	return n
}

// Failed: a subject could not be audited (no SBOM by a trusted key, or no scan), or a finding is
// at or above min (when on).
func (r Report) Failed(min Severity, on bool) bool {
	for _, s := range r.Subjects {
		if s.Error != "" {
			return true
		}
		for _, f := range s.Findings {
			if on && f.Severity >= min {
				return true
			}
		}
	}
	return false
}

// Verdict is the audit's failure: an artifact not audited, or a finding at or above min (when on).
func (r Report) Verdict(min Severity, on bool) error {
	if !r.Failed(min, on) {
		return nil
	}
	var not []string
	for _, s := range r.Subjects {
		if s.Error != "" {
			not = append(not, s.Name)
		}
	}
	if len(not) > 0 {
		return fmt.Errorf("audit: not audited: %s", strings.Join(not, ", "))
	}
	return fmt.Errorf("audit: vulnerabilities at or above %s", min)
}

// JSON writes r as JSON.
func (r Report) JSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Print writes r for a terminal: the findings by severity, highest first, then a summary.
func (r Report) Print(w io.Writer) error {
	// A VM's OS carries its release without a channel: what is not known is left out.
	about := []string{fmt.Sprintf("revision %d", r.Revision)}
	for _, s := range []string{r.Channel, r.Tag} {
		if s != "" {
			about = append(about, s)
		}
	}
	fmt.Fprintf(w, "%s %s (%s)", r.Project, r.Version, strings.Join(about, ", "))
	if r.DBBuilt != "" {
		fmt.Fprintf(w, ", vulnerability database built %s", r.DBBuilt)
	}
	fmt.Fprintln(w)
	type row struct {
		subject string
		f       Finding
	}
	var rows []row
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, s := range r.Subjects {
		if s.Error != "" {
			fmt.Fprintf(tw, "%s\t%s\tnot audited: %s\n", s.Name, s.Digest, s.Error)
		}
		for _, f := range s.Findings {
			rows = append(rows, row{s.Name, f})
		}
	}
	slices.SortStableFunc(rows, func(a, b row) int {
		return cmp.Or(cmp.Compare(b.f.Severity, a.f.Severity), cmp.Compare(a.subject, b.subject), cmp.Compare(a.f.ID, b.f.ID))
	})
	for _, x := range rows {
		fixed := "no fix"
		if len(x.f.Fixed) > 0 {
			fixed = "fixed in " + strings.Join(x.f.Fixed, ", ")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s %s (%s)\t%s\n", x.f.Severity, x.subject, x.f.ID, x.f.Package, x.f.Version, x.f.Type, fixed)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	n := r.Counts()
	var parts []string
	for s := Critical; s >= Unknown; s-- {
		if n[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n[s], s))
		}
	}
	if len(parts) == 0 {
		parts = []string{"none"}
	}
	_, err := fmt.Fprintf(w, "%d artifacts, vulnerabilities: %s\n", len(r.Subjects), strings.Join(parts, ", "))
	return err
}
