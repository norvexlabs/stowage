//go:build unix

package install

import (
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// graceful runs cmd in a session of its own — a process group the terminal neither interrupts
// nor stops, with no terminal to read — and makes its cancellation one interrupt of that group: a
// tool that exits at once on a second interrupt (OpenTofu) gets one, and records its state. A
// further interrupt or termination of this run is relayed to the group as a second interrupt (the
// operator's way to stop it at once), and the group — the tool and every process it started — is
// killed once grace has passed. The returned stop ends the relay.
func graceful(cmd *exec.Cmd, grace time.Duration) (stop func()) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	group := func(sig syscall.Signal) {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, sig) // a group already gone is not an error
		}
	}
	var once sync.Once
	done := make(chan struct{})
	var timer *time.Timer
	var mu sync.Mutex
	cmd.Cancel = func() error {
		group(syscall.SIGINT)
		once.Do(func() {
			relay := make(chan os.Signal, 1)
			signal.Notify(relay, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			go func() {
				defer signal.Stop(relay)
				for {
					select {
					case <-relay:
						group(syscall.SIGINT)
					case <-done:
						return
					}
				}
			}()
			mu.Lock()
			timer = time.AfterFunc(grace, func() { group(syscall.SIGKILL) })
			mu.Unlock()
		})
		return nil
	}
	return func() {
		close(done)
		mu.Lock()
		if timer != nil {
			timer.Stop()
		}
		mu.Unlock()
	}
}
