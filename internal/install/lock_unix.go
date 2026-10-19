//go:build unix

package install

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive lock on path, created 0600 if absent, without waiting: another
// process holding it is an error. The lock goes with the process, however it ends.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// Unlocked before it is closed: a child forked meanwhile (by any goroutine, before its exec
	// closes the descriptor) shares the open file, and would hold the lock past the close.
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}

// flockWait takes a lock on path, created 0600 if absent, waiting for another holder to release
// it: exclusive, or shared when exclusive is false. The lock goes with the process, however it
// ends.
func flockWait(path string, exclusive bool) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	for {
		err = unix.Flock(int(f.Fd()), how)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}
