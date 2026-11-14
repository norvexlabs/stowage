//go:build unix

// Package flock takes advisory file locks that wait for another holder while a context lasts.
package flock

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	said := false
	once := func() {
		if !said && waiting != nil {
			said = true
			waiting()
		}
	}
	for try := range 3 {
		if err := ctx.Err(); err != nil && try > 0 { // a wait ended meanwhile takes no lock again
			return nil, err
		}
		f, err := open(path)
		if err != nil {
			return nil, err
		}
		unlock, err := lock(ctx, f, exclusive, poll, once)
		if err != nil {
			return nil, err
		}
		same, err := Same(f, path)
		if err != nil {
			unlock()
			return nil, err
		}
		if same {
			return unlock, nil
		}
		unlock()
	}
	return nil, fmt.Errorf("lock %s: removed or replaced each time it was taken", path)
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
