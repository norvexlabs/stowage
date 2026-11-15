//go:build !unix

package kairos

import "os/exec"

// ownSession leaves cmd in this process's group: there are no sessions to give it.
func ownSession(*exec.Cmd) {}
