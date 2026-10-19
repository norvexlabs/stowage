package version

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SemVer is Semantic Versioning 2.0.0, strict (no leading "v"). A release version carries no build
// metadata: SemVer gives it no precedence, so two releases differing only by it could not be
// ordered, and an OCI tag cannot spell '+'.
type SemVer struct{}

type semver struct {
	core [3]uint64
	pre  []string
}

func isNumeric(s string) bool { return s != "" && strings.TrimLeft(s, "0123456789") == "" }

func validIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func parseSemVer(v string) (semver, error) {
	var s semver
	rest := v
	if i := strings.IndexByte(rest, '+'); i >= 0 {
		for _, id := range strings.Split(rest[i+1:], ".") {
			if !validIdent(id) {
				return s, fmt.Errorf("version %q: invalid build metadata", v)
			}
		}
		rest = rest[:i]
	}
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		for _, id := range strings.Split(rest[i+1:], ".") {
			if !validIdent(id) || (isNumeric(id) && len(id) > 1 && id[0] == '0') {
				return s, fmt.Errorf("version %q: invalid pre-release identifier %q", v, id)
			}
			s.pre = append(s.pre, id)
		}
		rest = rest[:i]
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return s, fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", v)
	}
	for i, p := range parts {
		if !isNumeric(p) || (len(p) > 1 && p[0] == '0') {
			return s, fmt.Errorf("version %q: %q is not a valid numeric field", v, p)
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return s, fmt.Errorf("version %q: %w", v, err)
		}
		s.core[i] = n
	}
	return s, nil
}

func (SemVer) Validate(v string) error {
	if strings.Contains(v, "+") {
		return invalid(fmt.Errorf("version %q: a release version carries no build metadata ('+…')", v))
	}
	_, err := parseSemVer(v)
	return invalid(err)
}

func cmp[T int | uint64](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (SemVer) Compare(a, b string) (int, error) {
	x, err := parseSemVer(a)
	if err != nil {
		return 0, err
	}
	y, err := parseSemVer(b)
	if err != nil {
		return 0, err
	}
	for i := range 3 {
		if c := cmp(x.core[i], y.core[i]); c != 0 {
			return c, nil
		}
	}
	// A version without pre-release ranks above one with.
	switch {
	case len(x.pre) == 0 && len(y.pre) == 0:
		return 0, nil
	case len(x.pre) == 0:
		return 1, nil
	case len(y.pre) == 0:
		return -1, nil
	}
	for i := 0; i < len(x.pre) && i < len(y.pre); i++ {
		p, q := x.pre[i], y.pre[i]
		pn, qn := isNumeric(p), isNumeric(q)
		switch {
		case pn && qn:
			a, _ := strconv.ParseUint(p, 10, 64)
			b, _ := strconv.ParseUint(q, 10, 64)
			if c := cmp(a, b); c != 0 {
				return c, nil
			}
		case pn:
			return -1, nil
		case qn:
			return 1, nil
		default:
			if c := strings.Compare(p, q); c != 0 {
				return c, nil
			}
		}
	}
	return cmp(len(x.pre), len(y.pre)), nil
}

func (s SemVer) Tag(v string) (string, error) {
	if err := s.Validate(v); err != nil {
		return "", err
	}
	return identityTag(v)
}

func (SemVer) Guard(string, time.Time) error { return nil }
