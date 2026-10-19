//go:build unix

package hook

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/norvexlabs/stowage/internal/gitcmd"
)

// Lock takes an exclusive lock on path, calling wait first when another process holds it and
// then waiting for it. The lock goes with the process, however it ends.
func Lock(path string, wait func()) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		wait()
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { f.Close() }, nil
}

// Launch starts exe in a session of its own, appending its output to logPath, with stdin from
// /dev/null, and does not wait for it: it outlives the hook and the terminal. Git's per-hook
// variables are dropped from its environment: the index file they name is gone once the commit
// ends.
func Launch(exe string, args []string, logPath string) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = f, f
	cmd.Env = gitcmd.Env(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
