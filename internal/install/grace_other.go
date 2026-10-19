//go:build !unix

package install

import (
	"os/exec"
	"time"
)

// graceful kills cmd when it is cancelled: no interrupt can be sent to it here.
func graceful(cmd *exec.Cmd, _ time.Duration) (stop func()) {
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	return func() {}
}
