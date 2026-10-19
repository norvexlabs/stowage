//go:build linux

package command

import (
	"os/exec"

	"golang.org/x/sys/unix"
)

// awaitExit waits for the command to exit WITHOUT reaping it (WNOWAIT). Until cmd.Wait reaps it,
// the exited leader keeps its process-group id reserved, so the supervisor and the clean-up that
// follow can signal the group without any risk of reaching an unrelated process that reused the
// id. It reports false when the wait could not be done; the caller then reaps first.
func awaitExit(cmd *exec.Cmd) bool {
	for {
		var si unix.Siginfo
		err := unix.Waitid(unix.P_PID, cmd.Process.Pid, &si, unix.WEXITED|unix.WNOWAIT, nil)
		if err == unix.EINTR {
			continue
		}
		return err == nil
	}
}
