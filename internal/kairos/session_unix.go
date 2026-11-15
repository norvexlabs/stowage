//go:build unix

package kairos

import (
	"os/exec"
	"syscall"
)

// ownSession runs cmd in a session of its own: a process group the terminal's interrupt does not
// reach, so only its context ends it.
func ownSession(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
