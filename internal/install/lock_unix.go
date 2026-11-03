//go:build unix

package install

import (
	"errors"
	"fmt"
	"os"
	"strings"

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

// lockHeld reports whether another process holds the lock on path, without taking it or making
// the file.
func lockHeld(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.Is(err, unix.EWOULDBLOCK)
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return false
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

// lockHeldPassive reports whether a process holds a flock on path, read from the kernel's table
// (/proc/locks) and never by taking the lock, which a run taking it at that instant would find
// held. False where the table does not read, or path does not exist; and where the file's device
// is not the one the table names (a btrfs subvolume, an overlay), or the holder is in another PID
// namespace: the host daemon reads it on the VM, whose install is on ext4, run in the host's.
func lockHeldPassive(path string) bool {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return false
	}
	raw, err := os.ReadFile("/proc/locks")
	if err != nil {
		return false
	}
	// "1: FLOCK  ADVISORY  WRITE 1234 fd:01:5678 0 EOF": the device's major and minor in hex, the
	// inode in decimal.
	want := fmt.Sprintf("%02x:%02x:%d", unix.Major(uint64(st.Dev)), unix.Minor(uint64(st.Dev)), st.Ino)
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) >= 6 && f[1] == "FLOCK" && strings.EqualFold(f[5], want) {
			return true
		}
	}
	return false
}
