// Package version resolves a project's version and orders versions through the project's scheme.
// The version itself is opaque to Stowage: only the scheme provider interprets it.
package version

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/norvexlabs/stowage/internal/command"
	"github.com/norvexlabs/stowage/internal/spec"
)

// Scheme validates and orders project versions and renders them as OCI tags.
type Scheme interface {
	// Validate refuses a string that is not a version of this scheme with an InvalidError; any
	// other error means the scheme could not answer.
	Validate(v string) error
	// Compare returns -1, 0 or 1. Both arguments must be valid.
	Compare(a, b string) (int, error)
	// Tag renders v as an OCI tag. Distinct versions render distinct tags.
	Tag(v string) (string, error)
	// Guard is the scheme's commit-time check (e.g. CalVer's month guard); nil when it has none.
	Guard(v string, now time.Time) error
}

// InvalidError is a scheme's definite answer that a string is not one of its versions — as
// opposed to a failure to answer (a command that crashed, timed out or gave no answer).
type InvalidError struct{ Err error }

func (e InvalidError) Error() string { return e.Err.Error() }
func (e InvalidError) Unwrap() error { return e.Err }

// IsInvalid reports whether err is a definite "not a version".
func IsInvalid(err error) bool {
	var ie InvalidError
	return errors.As(err, &ie)
}

func invalid(err error) error {
	if err == nil {
		return nil
	}
	return InvalidError{err}
}

// tagRe is the OCI distribution spec's tag grammar.
var tagRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// ValidTag reports whether t is a legal OCI tag.
func ValidTag(t string) bool { return tagRe.MatchString(t) }

// identityTag uses the version itself as its tag. Nothing is rewritten, so distinct versions
// always give distinct tags; a version an OCI tag cannot spell is refused.
func identityTag(v string) (string, error) {
	if !ValidTag(v) {
		return "", fmt.Errorf("version %q is not a legal OCI tag ([A-Za-z0-9_][A-Za-z0-9._-]{0,127})", v)
	}
	return v, nil
}

// NewScheme builds the scheme named by the spec; calver is the default.
func NewScheme(root string, p spec.Provider) (Scheme, error) {
	p = p.Or("calver")
	switch p.Name {
	case "calver":
		var c CalVerConfig
		if err := p.Decode(&c); err != nil {
			return nil, err
		}
		return NewCalVer(c)
	case "semver":
		var c struct{}
		if err := p.Decode(&c); err != nil {
			return nil, err
		}
		return SemVer{}, nil
	case "regex":
		var c RegexConfig
		if err := p.Decode(&c); err != nil {
			return nil, err
		}
		return NewRegex(c)
	case "command":
		var c command.Config
		if err := p.Decode(&c); err != nil {
			return nil, err
		}
		if err := c.Validate(); err != nil {
			return nil, err
		}
		return commandScheme{command.Runner{Root: root, Config: c}}, nil
	default:
		return nil, fmt.Errorf("version.scheme: unknown provider %q (calver, semver, regex, command)", p.Name)
	}
}

// commandScheme delegates to a project command: ops scheme.validate, scheme.compare.
type commandScheme struct{ r command.Runner }

// Validate needs a definite answer, {"valid": true} or {"valid": false, "reason": "..."}: a
// version check that did not run must not pass as valid.
func (c commandScheme) Validate(v string) error {
	var out struct {
		Valid  *bool  `json:"valid"`
		Reason string `json:"reason"`
	}
	err := c.r.Call(context.Background(), map[string]any{"op": "scheme.validate", "version": v}, &out)
	if errors.Is(err, command.ErrNoAnswer) {
		return errors.New("scheme.validate: the command gave no answer; it must answer {\"valid\": true|false}")
	}
	if err != nil {
		return err
	}
	if out.Valid == nil {
		return errors.New("scheme.validate: answer must be {\"valid\": true|false, \"reason\": \"...\"}")
	}
	if !*out.Valid {
		if out.Reason == "" {
			out.Reason = "not a version of the scheme"
		}
		return InvalidError{fmt.Errorf("version %q: %s", v, out.Reason)}
	}
	return nil
}

func (c commandScheme) Compare(a, b string) (int, error) {
	var out struct {
		Result *int `json:"result"`
	}
	if err := c.r.Call(context.Background(), map[string]any{"op": "scheme.compare", "a": a, "b": b}, &out); err != nil {
		return 0, err
	}
	if out.Result == nil || *out.Result < -1 || *out.Result > 1 {
		return 0, errors.New("scheme.compare: answer must be {\"result\": -1|0|1}")
	}
	return *out.Result, nil
}

// Tag is the version itself, as for every scheme: a tag a command could rewrite would defeat the
// checks that keep releases and channels from sharing tags.
func (commandScheme) Tag(v string) (string, error) { return identityTag(v) }

func (commandScheme) Guard(string, time.Time) error { return nil }
