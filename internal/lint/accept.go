package lint

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/sbom"
	"github.com/norvexlabs/stowage/internal/spec"
)

// Acceptance accepts the warnings at Where (a finding's where, as lint prints it) whose message
// holds Text, until the end of Expires (YYYY-MM-DD, UTC), for Reason.
type Acceptance struct {
	Where   string `yaml:"where" json:"where"`
	Text    string `yaml:"text" json:"text"`
	Reason  string `yaml:"reason" json:"reason"`
	Expires string `yaml:"expires" json:"expires"`
}

// Accepted is a warning an acceptance in force accepted: listed, never counted.
type Accepted struct {
	Finding
	Reason  string `json:"reason"`
	Expires string `json:"expires"`
}

func (a Accepted) String() string {
	return fmt.Sprintf("accepted: %s: %s (%s; until %s)", a.Where, a.Message, a.Reason, a.Expires)
}

// end is the first instant the acceptance no longer holds.
func (a Acceptance) end() time.Time {
	d, _ := time.Parse(time.DateOnly, a.Expires)
	return d.Add(24 * time.Hour)
}

func (a Acceptance) holds(now time.Time) bool { return now.Before(a.end()) }

func (a Acceptance) matches(f Finding) bool {
	return f.Severity == Warning && f.Where == a.Where && strings.Contains(f.Message, a.Text)
}

func (a Acceptance) String() string { return fmt.Sprintf("%s (%q)", a.Where, a.Text) }

// ParseAcceptances reads an acceptance file (a YAML list of where, text, reason, expires),
// strictly: every field is required, the date valid and at most sbom.MaxAllowance after now, and
// no entry is listed twice.
func ParseAcceptances(doc []byte, now time.Time) ([]Acceptance, error) {
	dec := yaml.NewDecoder(bytes.NewReader(doc))
	dec.KnownFields(true)
	var out []Acceptance
	if err := dec.Decode(&out); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	for i, a := range out {
		at := fmt.Sprintf("entry %d", i+1)
		switch {
		case strings.TrimSpace(a.Where) == "":
			return nil, fmt.Errorf("%s: where is required (the warning's where, as lint prints it before its message)", at)
		case a.Where != strings.TrimSpace(a.Where):
			return nil, fmt.Errorf("%s (%q): where is written without spaces around it", at, a.Where)
		case strings.TrimSpace(a.Text) == "":
			return nil, fmt.Errorf("%s (%s): text is required (a part of the warning's message)", at, a.Where)
		case strings.TrimSpace(a.Reason) == "":
			return nil, fmt.Errorf("%s (%s): reason is required", at, a)
		}
		if _, err := time.Parse(time.DateOnly, a.Expires); err != nil {
			return nil, fmt.Errorf("%s (%s): expires %q: a date, YYYY-MM-DD", at, a, a.Expires)
		}
		if a.end().After(now.Add(sbom.MaxAllowance)) {
			return nil, fmt.Errorf("%s (%s): expires %s: more than a year ahead; a warning is accepted for a while, never for good", at, a, a.Expires)
		}
		for j, b := range out[:i] {
			if a.Where == b.Where && a.Text == b.Text {
				return nil, fmt.Errorf("%s: %s is listed twice (entry %d)", at, a, j+1)
			}
		}
	}
	return out, nil
}

// accept moves each warning an acceptance in force matches from the findings to accepted, and
// warns of the file's entries the publisher should look at: expired (the warning counts again),
// expiring within sbom.ExpiryNotice, or matching no warning. It runs once every other check has,
// and warns after it matched, so no warning of its own is ever accepted.
func (l *linter) accept() {
	if l.s.Lint == nil || l.s.Lint.Accept == "" {
		return
	}
	file := l.s.Lint.Accept
	if !l.exists("lint.accept", file, false) {
		return
	}
	doc, err := os.ReadFile(spec.Path(l.root, file))
	var list []Acceptance
	if err == nil {
		list, err = ParseAcceptances(doc, l.now)
	}
	if err != nil {
		l.errf("lint.accept", "%s: %v", file, err)
		return
	}
	used := make([]bool, len(list))
	var kept []Finding
	for _, f := range l.findings {
		taken := false
		for i, a := range list {
			if a.holds(l.now) && a.matches(f) {
				if !taken {
					l.accepted = append(l.accepted, Accepted{f, a.Reason, a.Expires})
				}
				taken, used[i] = true, true
			}
		}
		if !taken {
			kept = append(kept, f)
		}
	}
	l.findings = kept
	for i, a := range list {
		switch {
		case !a.holds(l.now):
			l.warnf("lint.accept", "%s: acceptance of %s expired on %s: the warning counts again; renew it, fix the warning, or remove it", file, a, a.Expires)
		case !used[i]:
			l.warnf("lint.accept", "%s: acceptance of %s matches no warning: correct its where or text (lint prints each warning's where before its message), or remove it", file, a)
		case !a.holds(l.now.Add(sbom.ExpiryNotice)):
			l.warnf("lint.accept", "%s: acceptance of %s expires on %s, within %d days: renew it or fix the warning before then", file, a, a.Expires, int(sbom.ExpiryNotice.Hours()/24))
		}
	}
}
