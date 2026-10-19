// Package command runs the `command` provider: the escape hatch every project-specific concern
// offers.
//
// Contract: the executable receives one JSON request on stdin (`{"op": "...", ...}`), writes one
// JSON answer on stdout and exits 0 on success, 2 for "no answer" (the caller treats the question
// as unknown, never as a negative), anything else on error.
package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrNoAnswer is returned when the command exits 2: it does not know. Callers must not read it as
// a negative answer.
var ErrNoAnswer = errors.New("command gave no answer (exit 2)")

// waitDelay bounds how long Call waits for the command's output pipes once it has been killed.
const waitDelay = 2 * time.Second

// MaxOutput caps what a command may write on stdout; a runaway command fails instead of filling
// memory.
const MaxOutput = 1 << 20

// Config is the `command` provider's configuration, shared by every concern.
type Config struct {
	Cmd      []string `yaml:"cmd"`
	TimeoutS int      `yaml:"timeout_s"`
}

// Validate checks the configuration is usable.
func (c Config) Validate() error {
	if len(c.Cmd) == 0 || c.Cmd[0] == "" {
		return errors.New("command provider: cmd is required")
	}
	if c.TimeoutS < 0 {
		return errors.New("command provider: timeout_s must be positive")
	}
	return nil
}

// Runner runs a command provider from a project root. A cmd[0] containing a slash is resolved
// against the root; a bare name is looked up on PATH.
type Runner struct {
	Root   string
	Config Config
	// unit is what one timeout_s counts; zero is a second. The package's tests shorten it.
	unit time.Duration
}

func (r Runner) second() time.Duration {
	if r.unit > 0 {
		return r.unit
	}
	return time.Second
}

// Call sends req (which must carry "op") and decodes the answer into resp.
func (r Runner) Call(ctx context.Context, req map[string]any, resp any) error {
	if err := r.Config.Validate(); err != nil {
		return err
	}
	timeout := 30 * r.second()
	if r.Config.TimeoutS > 0 {
		timeout = time.Duration(r.Config.TimeoutS) * r.second()
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	// An absolute root keeps cmd[0] and cmd.Dir agreeing: Go resolves a relative Path against Dir,
	// so a relative root would be applied twice.
	root, err := filepath.Abs(r.Root)
	if err != nil {
		return err
	}
	name := r.Config.Cmd[0]
	if strings.Contains(name, "/") && !filepath.IsAbs(name) {
		name = filepath.Join(root, name)
	}
	cmd := exec.Command(name, r.Config.Cmd[1:]...)
	cmd.Dir = root
	cmd.Stdin = bytes.NewReader(body)
	// The command runs in its own session: the supervisor kills the whole group on timeout, on
	// the caller's cancellation or on an interrupt; Wait stops waiting for output shortly after (a
	// child that inherited stdout must not hold the call); whatever the command left running is
	// killed once it has answered.
	isolate(cmd)
	cmd.WaitDelay = waitDelay
	var stdout capped
	var stderr capped
	stdout.limit, stderr.limit = MaxOutput, 4096
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	op, _ := req["op"].(string)
	sv := supervise(ctx, timeout)
	if err := cmd.Start(); err != nil {
		sv.finish()
		return fmt.Errorf("command %s (op %s): %w", r.Config.Cmd[0], op, err)
	}
	sv.start(cmd)
	var out outcome
	if awaitExit(cmd) {
		// The exited leader is not reaped yet, so its group id cannot have been reused: end
		// supervision, kill what the command left running, then reap.
		out = sv.finish()
		_ = killGroup(cmd)
		err = cmd.Wait()
	} else {
		err = cmd.Wait()
		out = sv.finish()
	}
	// The command exited cleanly but a child kept its stdout open: the answer it wrote is whole.
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		err = nil
	}
	// A deadline or cancellation that fired as the command was exiting by itself did not end it:
	// its own answer stands.
	exitedByItself := cmd.ProcessState != nil && cmd.ProcessState.Exited()
	if out.timedOut && !exitedByItself {
		return fmt.Errorf("command %s (op %s): timed out after %s", r.Config.Cmd[0], op, timeout)
	}
	if out.canceled && !exitedByItself {
		return fmt.Errorf("command %s (op %s): %w", r.Config.Cmd[0], op, ctx.Err())
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		if exitErr.ExitCode() == 2 {
			return ErrNoAnswer
		}
		return fmt.Errorf("command %s (op %s): exit %d: %s", r.Config.Cmd[0], op, exitErr.ExitCode(), strings.TrimSpace(stderr.buf.String()))
	}
	if err != nil {
		return fmt.Errorf("command %s (op %s): %w", r.Config.Cmd[0], op, err)
	}
	if stdout.overflow {
		return fmt.Errorf("command %s (op %s): answer exceeds %d bytes", r.Config.Cmd[0], op, MaxOutput)
	}
	if resp == nil {
		return nil
	}
	if err := json.Unmarshal(stdout.buf.Bytes(), resp); err != nil {
		return fmt.Errorf("command %s (op %s): answer is not JSON: %w", r.Config.Cmd[0], op, err)
	}
	return nil
}

// capped keeps the first limit bytes and remembers whether more arrived.
type capped struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (c *capped) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	if room <= 0 {
		c.overflow = c.overflow || len(p) > 0
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.overflow = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

var _ io.Writer = (*capped)(nil)
