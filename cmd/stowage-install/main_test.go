package main

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/norvexlabs/stowage/internal/install"
)

func TestExitStatus(t *testing.T) {
	t.Parallel()
	refused := install.Refused{Reason: "refusing: x", Fix: "y"}
	for _, c := range []struct {
		name   string
		err    error
		code   int
		stderr string
	}{
		{"done", nil, 0, ""},
		{"failed", errors.New("broken"), 1, "error: broken\n"},
		{"refused", refused, 3, "error: refusing: x\n"},
		{"refused, wrapped", fmt.Errorf("upgrade: %w", refused), 3, "error: upgrade: refusing: x\n"},
		{"aborted", install.ErrAborted, 3, "error: aborted\n"},
		// The installer it ran printed its own error: its status, passed on, a refusal's too.
		{"a child's status", fmt.Errorf("x: %w", install.ExitCode(3)), 3, ""},
		{"a child's failure", install.ExitCode(1), 1, ""},
		{"a child's status over a refusal", fmt.Errorf("%w: %w", install.ExitCode(5), refused), 5, ""},
	} {
		var stderr bytes.Buffer
		if got := exit(c.err, &stderr); got != c.code || stderr.String() != c.stderr {
			t.Errorf("%s: %d %q, want %d %q", c.name, got, stderr.String(), c.code, c.stderr)
		}
	}
}
