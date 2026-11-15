//go:build unix

package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// flockWait takes a lock on path, created 0600 if absent, waiting for another holder to release
// it until ctx ends: exclusive, or shared when exclusive is false. The lock goes with the process,
// however it ends, and is on the file at path when taken (flock.Take).
func flockWait(ctx context.Context, path string, exclusive bool) (func(), error) {
	return flock.Take(ctx, path, flock.Create, exclusive, 100*time.Millisecond, nil)
}

// lockHeld reports whether a process holds a flock on path, read from the kernel's table
// (/proc/locks) and never by taking the lock, which a run taking it at that instant would find
// held. False where the table does not read, or path does not exist; and where the file's device
// is not the one the table names (a btrfs subvolume, an overlay), or the holder is in another PID
// namespace: the host daemon reads it on the VM, whose install is on ext4, run in the host's.
func lockHeld(path string) bool {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return false
	}
	// "1: FLOCK  ADVISORY  WRITE 1234 fd:01:5678 0 EOF": the device's major and minor in hex, the
	// inode in decimal.
	want := fmt.Sprintf("%02x:%02x:%d", unix.Major(uint64(st.Dev)), unix.Minor(uint64(st.Dev)), st.Ino)
	// The table reads a page at a time, each from the entry at its offset then: a lock released
	// between two reads shifts the later ones down, and one held all along can fall between the
	// pages. Each read misses it only while other locks come and go across that boundary, so a
	// held lock found in none of lockTableReads reads is taken as free.
	for range lockTableReads {
		raw, err := os.ReadFile("/proc/locks")
		if err != nil {
			return false
		}
		for _, line := range strings.Split(string(raw), "\n") {
			f := strings.Fields(line)
			if len(f) >= 6 && f[1] == "FLOCK" && strings.EqualFold(f[5], want) {
				return true
			}
		}
	}
	return false
}

// keepFromChildren marks the descriptor a path names close-on-exec (inheritedFD): a descriptor a
// parent handed this run (childLogin's login) reaches no process this run starts (helm, kubectl,
// a hook), whether its read then succeeds or not. Any other path is left as it is.
func keepFromChildren(path string) {
	if fd, ok := inheritedFD(path); ok {
		unix.CloseOnExec(fd)
	}
}

// fdPath is a path naming one of this process's descriptors: /dev/fd/N, /proc/self/fd/N,
// /proc/thread-self/fd/N, /proc/PID/fd/N and /proc/PID/task/TID/fd/N (PID this process's or one
// of its threads', ownProcess).
var fdPath = regexp.MustCompile(`^/(?:dev|proc/(self|thread-self|[0-9]+)(?:/task/[0-9]+)?)/fd/([0-9]+)$`)

// inheritedFD is the descriptor path names (fdPath, once resolveFDPath resolved it: a symlink to
// /dev/fd/N names N), past standard error: those three (/dev/stdin among their spellings) are the
// run's own.
func inheritedFD(path string) (int, bool) {
	m := fdPath.FindStringSubmatch(resolveFDPath(path))
	if m == nil || !ownProcess(m[1]) {
		return 0, false
	}
	fd, err := strconv.Atoi(m[2])
	return fd, err == nil && fd > 2
}

// ownProcess reports whether id, fdPath's process part, names this process: empty (/dev/fd),
// self, thread-self, this process's id or one of its threads' (/proc/self/task/<id>).
func ownProcess(id string) bool {
	switch id {
	case "", "self", "thread-self", strconv.Itoa(os.Getpid()):
		return true
	}
	_, err := os.Stat("/proc/self/task/" + id)
	return err == nil
}

// resolveFDPath is path with its symlinks resolved up to the descriptor it names: each directory
// through filepath.EvalSymlinks (/dev/fd to /proc/<pid>/fd), a link it ends in followed until the
// path is one fdPath reads, whose own link (pipe:[…], a file's path) is never followed. A path
// that cannot be resolved is taken cleaned.
func resolveFDPath(path string) string {
	p := filepath.Clean(path)
	for range 40 { // the kernel's own bound on a chain of links
		dir, err := filepath.EvalSymlinks(filepath.Dir(p))
		if err != nil {
			return p
		}
		p = filepath.Join(dir, filepath.Base(p))
		if fdPath.MatchString(p) {
			return p
		}
		target, err := os.Readlink(p)
		if err != nil {
			return p
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(dir, target)
		}
		p = filepath.Clean(target)
	}
	return p
}

// lockTableReads is how many times lockHeld reads the kernel's lock table before it judges a
// lock free (each read can skip a held entry while other locks change).
const lockTableReads = 3
