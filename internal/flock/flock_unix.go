//go:build unix

// Package flock takes advisory file locks that wait for another holder while a context lasts.
package flock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// ErrHeld is a lock another process holds, taken without waiting (a poll of 0).
var ErrHeld = errors.New("held by another process")

// Take opens path with open and takes its lock: exclusive for this run alone, otherwise shared
// with other shared takers. With a poll above 0 it waits for another holder, trying again at each
// poll and calling waiting (when not nil) once, until ctx ends (ctx.Err()); with a poll of 0 a lock
// held is ErrHeld at once. The file locked must then still be the one at path: one removed or
// replaced meanwhile excludes no one opening path now, so path is opened again — open decides
// whether a file removed is made again (its error, fs.ErrNotExist for one that makes none, is
// returned). The returned function releases the lock and closes the file; the lock goes with the
// process, however it ends.
func Take(ctx context.Context, path string, open func(string) (*os.File, error), exclusive bool, poll time.Duration, waiting func()) (func(), error) {
	_, unlock, err := TakeFile(ctx, path, open, exclusive, poll, waiting)
	return unlock, err
}

// TakeFile is Take, which also returns the file it locked, for Hand. An exclusive lock on the
// file a parent handed this process (Hand) is that lock, taken at once.
func TakeFile(ctx context.Context, path string, open func(string) (*os.File, error), exclusive bool, poll time.Duration, waiting func()) (*os.File, func(), error) {
	if exclusive {
		if f, unlock := takeHanded(path); f != nil {
			return f, unlock, nil
		}
	}
	said := false
	once := func() {
		if !said && waiting != nil {
			said = true
			waiting()
		}
	}
	for try := range 3 {
		if err := ctx.Err(); err != nil && try > 0 { // a wait ended meanwhile takes no lock again
			return nil, nil, err
		}
		f, err := open(path)
		if err != nil {
			return nil, nil, err
		}
		unlock, err := lock(ctx, f, exclusive, poll, once)
		if err != nil {
			return nil, nil, err
		}
		same, err := Same(f, path)
		if err != nil {
			unlock()
			return nil, nil, err
		}
		if same {
			return f, unlock, nil
		}
		unlock()
	}
	return nil, nil, fmt.Errorf("lock %s: removed or replaced each time it was taken", path)
}

// HandedEnv names, in a child's environment, the descriptor of the exclusive lock its parent
// handed it (Hand).
const HandedEnv = "STOWAGE_HANDED_LOCK"

// Hand gives cmd, before it starts, the exclusive lock held on f (TakeFile): the child inherits
// the open file the lock is on, and an exclusive take of that file's path in the child (Take,
// TakeFile) while it does not hold it already is that lock, never a wait on its parent. The lock
// is one for both processes: the child's release never unlocks it (the child keeps the file for
// its next take, and closes it when it ends), so the lock is held until the parent releases its
// own, which ends it — the parent holds it until it is done, the child's run included.
func Hand(cmd *exec.Cmd, f *os.File) {
	cmd.ExtraFiles = append(cmd.ExtraFiles, f)
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(slices.DeleteFunc(slices.Clone(cmd.Env), func(kv string) bool { return strings.HasPrefix(kv, HandedEnv+"=") }),
		HandedEnv+"="+strconv.Itoa(2+len(cmd.ExtraFiles)))
}

// handed is the lock file a parent handed this process (Hand), nil while a take holds it.
var handed struct {
	mu sync.Mutex
	f  *os.File
}

// The handed descriptor is read once, at start, and kept from every process this one starts:
// neither its environment nor its descriptors pass the lock on.
func init() {
	v, ok := os.LookupEnv(HandedEnv)
	if !ok {
		return
	}
	_ = os.Unsetenv(HandedEnv)
	fd, err := strconv.Atoi(v)
	if err != nil || fd < 3 {
		return
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil {
		return
	}
	unix.CloseOnExec(fd)
	handed.f = os.NewFile(uintptr(fd), "handed lock")
}

// takeHanded takes the handed lock when it is on the file at path: an exclusive flock on the
// handed file, which its parent's hold does not exclude (the same open file). Nil when none was
// handed, a take holds it, or it is another file's. Its release hands the file back for the next
// take, and never unlocks it: the open file is the parent's too, whose lock it would end.
func takeHanded(path string) (*os.File, func()) {
	handed.mu.Lock()
	defer handed.mu.Unlock()
	f := handed.f
	if f == nil {
		return nil, nil
	}
	if same, err := Same(f, path); err != nil || !same {
		return nil, nil
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, nil
	}
	handed.f = nil
	return f, func() {
		handed.mu.Lock()
		defer handed.mu.Unlock()
		handed.f = f
	}
}

// Create opens a lock file read-write, made 0600 when absent: the opener of a lock this process's
// user alone takes.
func Create(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
}

// Same reports whether f is still the file at path: a lock on a file removed (or replaced) since
// it was opened excludes no one who opens path now. A path that does not stat for another reason
// than its absence is an error.
func Same(f *os.File, path string) (bool, error) {
	var held, now unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &held); err != nil {
		return false, fmt.Errorf("lock %s: %w", path, err)
	}
	if err := unix.Stat(path, &now); errors.Is(err, unix.ENOENT) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("lock %s: %w", path, err)
	}
	return held.Dev == now.Dev && held.Ino == now.Ino, nil
}

// lock takes the flock on f (closing f on an error), as Take describes.
func lock(ctx context.Context, f *os.File, exclusive bool, poll time.Duration, waiting func()) (func(), error) {
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	var tick *time.Ticker
	for {
		err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB)
		if err == nil {
			break
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
		}
		if poll <= 0 {
			f.Close()
			return nil, ErrHeld
		}
		if tick == nil {
			waiting()
			if ctx.Done() == nil {
				// A wait no context can end: the kernel wakes it at the release.
				for err = unix.Flock(int(f.Fd()), how); errors.Is(err, unix.EINTR); err = unix.Flock(int(f.Fd()), how) {
				}
				if err != nil {
					f.Close()
					return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
				}
				break
			}
			tick = time.NewTicker(poll)
			defer tick.Stop()
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
	// Unlocked before it is closed: a child forked meanwhile shares the open file, and would hold
	// the lock past the close.
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}
