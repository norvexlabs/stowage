//go:build !linux

package command

import "os/exec"

// awaitExit cannot wait without reaping here, so the caller reaps first and does not signal the
// command's group after it has exited: a leftover child of the command survives the call. A
// supervisor signal that fires in the instant between the exit and the end of supervision is the
// one window that remains on these systems.
func awaitExit(*exec.Cmd) bool { return false }
