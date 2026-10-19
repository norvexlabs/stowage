//go:build linux

package command

import (
	"runtime"
	"syscall"
)

// selfStop stops stowage and returns once it has been continued. A SIGSTOP sent to the calling
// thread is acted on before the syscall returns, so the stop is synchronous — kill(2) on the
// process is not: it can return before the stop takes effect.
func selfStop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	_ = syscall.Tgkill(syscall.Getpid(), syscall.Gettid(), syscall.SIGSTOP)
}
