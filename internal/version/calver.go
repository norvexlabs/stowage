package version

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CalVerConfig configures the calver scheme. Format tokens follow calver.org (YYYY, YY, 0Y, MM,
// 0M, WW, 0W, DD, 0D, MAJOR, MINOR, MICRO), with M and D accepted as MM and DD. Tokens are
// separated by '.'.
type CalVerConfig struct {
	Format string `yaml:"format"`
	// MonthGuard refuses a version whose year/month is not the current one: "utc", "local" or ""
	// (off).
	MonthGuard string `yaml:"month_guard"`
}

type calToken struct {
	name   string
	padded bool // exactly two digits, leading zero kept
	min    int
	max    int // 0 = unbounded
	kind   byte
}

const (
	kYear  = 'y'
	kMonth = 'm'
	kWeek  = 'w'
	kDay   = 'd'
	kCount = 'n'
)

var calTokens = map[string]calToken{
	"YYYY":  {name: "YYYY", min: 1000, max: 9999, kind: kYear},
	"YY":    {name: "YY", min: 0, kind: kYear},
	"0Y":    {name: "0Y", padded: true, min: 0, max: 99, kind: kYear},
	"MM":    {name: "MM", min: 1, max: 12, kind: kMonth},
	"M":     {name: "MM", min: 1, max: 12, kind: kMonth},
	"0M":    {name: "0M", padded: true, min: 1, max: 12, kind: kMonth},
	"WW":    {name: "WW", min: 0, max: 53, kind: kWeek},
	"0W":    {name: "0W", padded: true, min: 0, max: 53, kind: kWeek},
	"DD":    {name: "DD", min: 1, max: 31, kind: kDay},
	"D":     {name: "DD", min: 1, max: 31, kind: kDay},
	"0D":    {name: "0D", padded: true, min: 1, max: 31, kind: kDay},
	"MAJOR": {name: "MAJOR", min: 0, kind: kCount},
	"MINOR": {name: "MINOR", min: 0, kind: kCount},
	"MICRO": {name: "MICRO", min: 0, kind: kCount},
}

// CalVer is a calendar-versioning scheme.
type CalVer struct {
	tokens []calToken
	guard  string
	format string
}

// NewCalVer validates the configuration.
func NewCalVer(c CalVerConfig) (*CalVer, error) {
	if c.Format == "" {
		c.Format = "YYYY.M.MICRO"
	}
	cv := &CalVer{format: c.Format}
	for _, part := range strings.Split(c.Format, ".") {
		t, ok := calTokens[part]
		if !ok {
			return nil, fmt.Errorf("calver: unknown format token %q in %q", part, c.Format)
		}
		cv.tokens = append(cv.tokens, t)
	}
	switch c.MonthGuard {
	case "", "utc", "local":
		cv.guard = c.MonthGuard
	default:
		return nil, fmt.Errorf("calver: month_guard must be utc, local or empty, not %q", c.MonthGuard)
	}
	if cv.guard != "" && (cv.index(kYear) < 0 || cv.index(kMonth) < 0) {
		return nil, fmt.Errorf("calver: month_guard needs a year and a month in the format %q", c.Format)
	}
	return cv, nil
}

func (c *CalVer) index(kind byte) int {
	for i, t := range c.tokens {
		if t.kind == kind {
			return i
		}
	}
	return -1
}

func (c *CalVer) parse(v string) ([]int, error) {
	parts := strings.Split(v, ".")
	if len(parts) != len(c.tokens) {
		return nil, fmt.Errorf("version %q does not match calver format %s", v, c.format)
	}
	out := make([]int, len(parts))
	for i, p := range parts {
		t := c.tokens[i]
		if p == "" || strings.TrimLeft(p, "0123456789") != "" {
			return nil, fmt.Errorf("version %q: %s field %q is not a number", v, t.name, p)
		}
		if t.padded && len(p) != 2 {
			return nil, fmt.Errorf("version %q: %s field %q must have two digits", v, t.name, p)
		}
		if !t.padded && len(p) > 1 && p[0] == '0' {
			return nil, fmt.Errorf("version %q: %s field %q has a leading zero", v, t.name, p)
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("version %q: %s field %q: %w", v, t.name, p, err)
		}
		if n < t.min || (t.max > 0 && n > t.max) {
			return nil, fmt.Errorf("version %q: %s field %d out of range", v, t.name, n)
		}
		out[i] = n
	}
	return out, nil
}

func (c *CalVer) Validate(v string) error { _, err := c.parse(v); return invalid(err) }

func (c *CalVer) Compare(a, b string) (int, error) {
	pa, err := c.parse(a)
	if err != nil {
		return 0, err
	}
	pb, err := c.parse(b)
	if err != nil {
		return 0, err
	}
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1, nil
			}
			return 1, nil
		}
	}
	return 0, nil
}

func (c *CalVer) Tag(v string) (string, error) {
	if err := c.Validate(v); err != nil {
		return "", err
	}
	return identityTag(v)
}

// Guard implements the month guard: the version's year and month must be today's.
func (c *CalVer) Guard(v string, now time.Time) error {
	if c.guard == "" {
		return nil
	}
	p, err := c.parse(v)
	if err != nil {
		return err
	}
	if c.guard == "utc" {
		now = now.UTC()
	} else {
		now = now.Local()
	}
	year := p[c.index(kYear)]
	switch c.tokens[c.index(kYear)].name {
	case "YY", "0Y":
		year += 2000
	}
	month := p[c.index(kMonth)]
	if year != now.Year() || month != int(now.Month()) {
		return fmt.Errorf("version %s is not in the current month (%d.%d, %s): start a new month's series", v, now.Year(), int(now.Month()), strings.ToUpper(c.guard))
	}
	return nil
}
