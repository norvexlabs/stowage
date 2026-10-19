//go:build !unix

package command

import (
	"os"
	"os/exec"
)

// Where process groups are not available the command shares stowage's: the supervisor kills the
// command on timeout, cmd.WaitDelay bounds the wait for inherited pipes, and an interrupt reaches
// it directly.

func isolate(*exec.Cmd) {}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	_ = cmd.Process.Kill()
	return nil
}

func stopGroup(*exec.Cmd)     {}
func continueGroup(*exec.Cmd) {}
func selfStop()               {}

func inTerminalForeground() bool { return false }

type signals struct {
	end  chan os.Signal
	tstp chan os.Signal
}

func armSignals() *signals { return &signals{} }

func (*signals) disarm() {}

func endBy(os.Signal) { os.Exit(1) }
