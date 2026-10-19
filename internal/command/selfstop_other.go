//go:build unix && !linux

package command

import (
	"os"
	"syscall"
	"time"
)

// selfStop stops stowage. Without a thread-directed signal the stop is asynchronous; the pause
// gives it time to take effect before the caller carries on.
func selfStop() {
	_ = syscall.Kill(os.Getpid(), syscall.SIGSTOP)
	time.Sleep(50 * time.Millisecond)
}
