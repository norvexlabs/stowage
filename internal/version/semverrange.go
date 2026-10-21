package version

import (
	"errors"
	"fmt"
	"strings"
)

// SemVerRange is a set of SemVer versions: comparators separated by spaces, all of which hold —
// ">=0.8 <1", "=0.7.2". A comparator is one of >= > <= < = and a version whose minor and patch
// may be left out (0 then), without a pre-release: "<1" is "<1.0.0", which 1.0.0's pre-releases
// satisfy, as SemVer orders them before it.
type SemVerRange struct {
	src   string
	terms []rangeTerm
}

type rangeTerm struct {
	op string
	v  string
}

// rangeOps lists the operators, the two-character ones first: ">=" is not ">" then "=1".
var rangeOps = []string{">=", "<=", ">", "<", "="}

// ParseSemVerRange reads a range; an empty one is an error.
func ParseSemVerRange(s string) (SemVerRange, error) {
	r := SemVerRange{src: strings.Join(strings.Fields(s), " ")}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return r, errors.New("empty range")
	}
	for _, f := range fields {
		op := ""
		for _, o := range rangeOps {
			if strings.HasPrefix(f, o) {
				op = o
				break
			}
		}
		if op == "" {
			return r, fmt.Errorf("%q: a comparator starts with one of %s", f, strings.Join(rangeOps, " "))
		}
		v, err := fullVersion(strings.TrimPrefix(f, op))
		if err != nil {
			return r, fmt.Errorf("%q: %w", f, err)
		}
		r.terms = append(r.terms, rangeTerm{op, v})
	}
	return r, nil
}

// fullVersion pads MAJOR[.MINOR[.PATCH]] to a SemVer release version.
func fullVersion(v string) (string, error) {
	if strings.ContainsAny(v, "-+") {
		return "", errors.New("a range's version is MAJOR[.MINOR[.PATCH]], without a pre-release or build metadata")
	}
	parts := strings.Split(v, ".")
	if len(parts) > 3 {
		return "", fmt.Errorf("%q is not MAJOR[.MINOR[.PATCH]]", v)
	}
	for len(parts) < 3 {
		parts = append(parts, "0")
	}
	full := strings.Join(parts, ".")
	if _, err := parseSemVer(full); err != nil {
		return "", fmt.Errorf("%q is not MAJOR[.MINOR[.PATCH]]", v)
	}
	return full, nil
}

// Contains reports whether every comparator holds for v, a SemVer version.
func (r SemVerRange) Contains(v string) (bool, error) {
	for _, t := range r.terms {
		c, err := SemVer{}.Compare(v, t.v)
		if err != nil {
			return false, err
		}
		var ok bool
		switch t.op {
		case ">=":
			ok = c >= 0
		case "<=":
			ok = c <= 0
		case ">":
			ok = c > 0
		case "<":
			ok = c < 0
		case "=":
			ok = c == 0
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// String is the range as written, its spaces normalised.
func (r SemVerRange) String() string { return r.src }
