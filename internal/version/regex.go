package version

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RegexConfig configures the regex scheme: a pattern with named groups, and the ordering keys
// (`name:int` or `name:str`) compared in turn.
type RegexConfig struct {
	Pattern string   `yaml:"pattern"`
	Order   []string `yaml:"order"`
}

type regexKey struct {
	group   int
	numeric bool
}

// Regex orders versions by named groups of a pattern.
type Regex struct {
	re   *regexp.Regexp
	keys []regexKey
}

// NewRegex validates the configuration. The pattern is anchored at both ends.
func NewRegex(c RegexConfig) (*Regex, error) {
	if c.Pattern == "" {
		return nil, fmt.Errorf("regex scheme: pattern is required")
	}
	if len(c.Order) == 0 {
		return nil, fmt.Errorf("regex scheme: order is required")
	}
	re, err := regexp.Compile(`^(?:` + c.Pattern + `)$`)
	if err != nil {
		return nil, fmt.Errorf("regex scheme: %w", err)
	}
	r := &Regex{re: re}
	for _, o := range c.Order {
		name, typ, ok := strings.Cut(o, ":")
		if !ok || (typ != "int" && typ != "str") {
			return nil, fmt.Errorf("regex scheme: order key %q must be name:int or name:str", o)
		}
		idx := re.SubexpIndex(name)
		if idx < 0 {
			return nil, fmt.Errorf("regex scheme: order key %q names no group of the pattern", name)
		}
		r.keys = append(r.keys, regexKey{group: idx, numeric: typ == "int"})
	}
	return r, nil
}

func (r *Regex) match(v string) ([]string, error) {
	m := r.re.FindStringSubmatch(v)
	if m == nil {
		return nil, fmt.Errorf("version %q does not match the scheme pattern", v)
	}
	for _, k := range r.keys {
		if k.numeric {
			if _, err := strconv.ParseUint(m[k.group], 10, 64); err != nil && m[k.group] != "" {
				return nil, fmt.Errorf("version %q: group %d is not a number", v, k.group)
			}
		}
	}
	return m, nil
}

func (r *Regex) Validate(v string) error { _, err := r.match(v); return invalid(err) }

func (r *Regex) Compare(a, b string) (int, error) {
	ma, err := r.match(a)
	if err != nil {
		return 0, err
	}
	mb, err := r.match(b)
	if err != nil {
		return 0, err
	}
	for _, k := range r.keys {
		x, y := ma[k.group], mb[k.group]
		if k.numeric {
			// An absent optional group ranks below any number.
			nx, ex := strconv.ParseUint(x, 10, 64)
			ny, ey := strconv.ParseUint(y, 10, 64)
			switch {
			case ex != nil && ey != nil:
				continue
			case ex != nil:
				return -1, nil
			case ey != nil:
				return 1, nil
			}
			if c := cmp(nx, ny); c != 0 {
				return c, nil
			}
			continue
		}
		if c := strings.Compare(x, y); c != 0 {
			return c, nil
		}
	}
	return 0, nil
}

func (r *Regex) Tag(v string) (string, error) {
	if err := r.Validate(v); err != nil {
		return "", err
	}
	return identityTag(v)
}

func (*Regex) Guard(string, time.Time) error { return nil }
