//go:build unix

package command

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// isolate starts the command in its own session — a process group of its own, with no controlling
// terminal — so the supervisor can kill the command and every process it started, and a command
// that tries to prompt on /dev/tty fails at once instead of stopping as a background job.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// signalGroup sends sig to the command's process group; a group already gone, or not started, is
// not an error.
func signalGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

func killGroup(cmd *exec.Cmd) error { return signalGroup(cmd, syscall.SIGKILL) }
func stopGroup(cmd *exec.Cmd)       { _ = signalGroup(cmd, syscall.SIGSTOP) }
func continueGroup(cmd *exec.Cmd)   { _ = signalGroup(cmd, syscall.SIGCONT) }

// endSignals are the signals that would have ended stowage and the command together had the
// command stayed in stowage's process group: the terminal's Ctrl-C and Ctrl-\, a SIGTERM or a
// hangup.
var endSignals = []os.Signal{os.Interrupt, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP}

// signals carries what the supervisor relays. A nil channel is never selected.
type signals struct {
	end  chan os.Signal
	tstp chan os.Signal
}

// armSignals catches the ending signals stowage is not ignoring — a signal ignored when stowage
// started (nohup, a background job) stays ignored — and SIGTSTP, which the supervisor follows only
// while stowage holds the terminal's foreground (see supervisor.start).
func armSignals() *signals {
	s := &signals{end: make(chan os.Signal, 1)}
	var watched []os.Signal
	for _, sg := range endSignals {
		if !signal.Ignored(sg) {
			watched = append(watched, sg)
		}
	}
	if len(watched) > 0 {
		signal.Notify(s.end, watched...)
	}
	if !signal.Ignored(syscall.SIGTSTP) {
		s.tstp = make(chan os.Signal, 1)
		signal.Notify(s.tstp, syscall.SIGTSTP)
	}
	return s
}

// inTerminalForeground reports whether stowage's process group is the foreground group of the
// terminal on stdin, stdout or stderr. Tests may force it (forceForeground).
func inTerminalForeground() bool {
	if forceForeground {
		return true
	}
	own := syscall.Getpgrp()
	for _, fd := range []int{0, 1, 2} {
		if pg, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP); err == nil && pg == own {
			return true
		}
	}
	return false
}

// forceForeground is set only by this package's job-control test, which has no terminal.
var forceForeground bool

func (s *signals) disarm() {
	signal.Stop(s.end)
	if s.tstp != nil {
		signal.Stop(s.tstp)
	}
}

// endBy ends stowage the way sig would have. SIGQUIT is not re-raised: the Go runtime's own
// handler would answer it with a goroutine dump and exit 2, not the conventional 131.
func endBy(sig os.Signal) {
	ss, ok := sig.(syscall.Signal)
	if !ok {
		os.Exit(1)
	}
	if ss != syscall.SIGQUIT {
		_ = syscall.Kill(os.Getpid(), ss)
		// The default action ends the process; should it not, exit with the shell's code.
		time.Sleep(time.Second)
	}
	os.Exit(128 + int(ss))
}
