package command

import (
	"context"
	"os"
	"os/exec"
	"time"
)

// outcome is how a supervised call ended, besides the command's own exit.
type outcome struct {
	timedOut bool
	canceled bool
	signal   os.Signal // an ending signal stowage received while the command ran
}

// supervisor owns a running command: its timeout, the caller's context, and the signals stowage
// receives meant for both. The timeout is a timer the supervisor pauses while job control has
// stowage and the command stopped, so time spent suspended is not counted against the command.
type supervisor struct {
	ctx     context.Context
	timeout time.Duration
	sig     *signals
	done    chan struct{}
	result  chan outcome
	started bool
}

// supervise arms the signal watch; it must be called BEFORE the command starts, so no signal
// falls between start and watch.
func supervise(ctx context.Context, timeout time.Duration) *supervisor {
	return &supervisor{ctx: ctx, timeout: timeout, sig: armSignals(), done: make(chan struct{}), result: make(chan outcome, 1)}
}

// start supervises cmd, which has started.
func (s *supervisor) start(cmd *exec.Cmd) {
	s.started = true
	go func() {
		deadline := time.Now().Add(s.timeout)
		timer := time.NewTimer(s.timeout)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				_ = killGroup(cmd)
				s.result <- outcome{timedOut: true}
				return
			case <-s.ctx.Done():
				_ = killGroup(cmd)
				s.result <- outcome{canceled: true}
				return
			case sg := <-s.sig.end:
				_ = killGroup(cmd)
				s.result <- outcome{signal: sg}
				return
			case <-s.sig.tstp:
				// Followed only while stowage is the terminal's foreground group — checked now, so
				// a run brought to the foreground with fg is covered. Any other SIGTSTP (an
				// orphaned group, a supervisor's kill) is dropped: stopping on it could leave
				// stowage stopped with nobody to continue it.
				if !inTerminalForeground() {
					continue
				}
				remaining := time.Until(deadline)
				timer.Stop()
				stopGroup(cmd)
				selfStop() // returns once stowage is continued
				continueGroup(cmd)
				deadline = time.Now().Add(remaining)
				timer.Reset(remaining)
			case <-s.done:
				s.result <- outcome{}
				return
			}
		}
	}()
}

// finish ends supervision once the command has exited. When an ending signal arrived — while the
// command ran, before it started, or as it ended — stowage ends the way that signal would have
// ended it: finish does not return into a caller that would carry on.
func (s *supervisor) finish() outcome {
	var o outcome
	if s.started {
		close(s.done)
		o = <-s.result
	}
	// Stop delivery first, then drain: a signal can no longer slip in between unseen.
	s.sig.disarm()
	select {
	case <-s.sig.tstp: // a Ctrl-Z as the command ended: stowage stops, as it would have
		if inTerminalForeground() {
			selfStop()
		}
	default:
	}
	if o.signal == nil {
		select {
		case o.signal = <-s.sig.end:
		default:
		}
	}
	if o.signal != nil {
		endBy(o.signal)
	}
	return o
}
