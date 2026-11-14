//go:build unix

package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/norvexlabs/stowage/internal/flock"
)

// lockFile takes an exclusive lock on path, created 0600 if absent, without waiting: another
// process holding it is an error (errLocked). The lock goes with the process, however it ends,
// and is on the file at path when taken (flock.Take).
func lockFile(path string) (func(), error) {
	unlock, err := flock.Take(context.Background(), path, flock.Create, true, 0, nil)
	if errors.Is(err, flock.ErrHeld) {
		return nil, errLocked
	}
	return unlock, err
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
// it until ctx ends: exclusive, or shared when exclusive is false. The lock goes with the process,
// however it ends, and is on the file at path when taken (flock.Take).
func flockWait(ctx context.Context, path string, exclusive bool) (func(), error) {
	return flock.Take(ctx, path, flock.Create, exclusive, 100*time.Millisecond, nil)
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
