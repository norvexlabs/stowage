//go:build unix

package flock

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func create(p string) (*os.File, error)   { return os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600) }
func noCreate(p string) (*os.File, error) { return os.OpenFile(p, os.O_RDWR, 0) }

// A second taker waits, says so once however many polls go by, and takes the lock once the first
// releases it; a free lock is taken without a word; a poll of 0 reports a held lock at once.
func TestTakeWaitsForTheHolder(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "l")
	said := 0
	unlock, err := Take(context.Background(), path, create, true, time.Millisecond, func() { said++ })
	if err != nil || said != 0 {
		t.Fatalf("a free lock: %v, said %d", err, said)
	}
	if _, err := Take(context.Background(), path, create, true, 0, nil); !errors.Is(err, ErrHeld) {
		t.Fatalf("no wait: %v", err)
	}
	waiting := make(chan struct{})
	done := make(chan error)
	go func() {
		u, err := Take(context.Background(), path, create, true, time.Millisecond, func() { said++; close(waiting) })
		if err == nil {
			u()
		}
		done <- err
	}()
	<-waiting
	select {
	case err := <-done:
		t.Fatalf("taken while held: %v", err)
	default:
	}
	unlock()
	if err := <-done; err != nil || said != 1 {
		t.Fatalf("after the release: %v, said %d", err, said)
	}
}

// Shared takers share; an exclusive one waits for them.
func TestTakeShared(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "l")
	a, err := Take(context.Background(), path, create, false, time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Take(context.Background(), path, create, false, 0, nil)
	if err != nil {
		t.Fatalf("a second shared: %v", err)
	}
	if _, err := Take(context.Background(), path, create, true, 0, nil); !errors.Is(err, ErrHeld) {
		t.Fatalf("exclusive beside shared: %v", err)
	}
	a()
	b()
}

// A wait ends with its context, the lock left to its holder.
func TestTakeEndsWithItsContext(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "l")
	unlock, err := Take(context.Background(), path, create, true, time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Take(ctx, path, create, true, time.Millisecond, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled wait: %v", err)
	}
}

// A lock file removed while a taker waits is opened again: made again by an opener that makes
// it, fs.ErrNotExist from one that does not; one replaced is taken at its new file.
func TestTakeChecksTheFileIsStillAtItsPath(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		open    func(string) (*os.File, error)
		replace bool
		gone    bool
	}{
		{"removed, made again", create, false, false},
		{"removed, none made", noCreate, false, true},
		{"replaced", noCreate, true, false},
	} {
		path := filepath.Join(t.TempDir(), "l")
		hold, err := Take(context.Background(), path, create, true, time.Millisecond, nil)
		if err != nil {
			t.Fatal(err)
		}
		waiting := make(chan struct{})
		done := make(chan error)
		go func() {
			u, err := Take(context.Background(), path, c.open, true, time.Millisecond, func() { close(waiting) })
			if err == nil {
				// The waiter's lock is on the file at path now: no other take of it.
				if _, herr := Take(context.Background(), path, noCreate, true, 0, nil); !errors.Is(herr, ErrHeld) {
					err = errors.New("the lock is not on the file at its path")
				}
				u()
			}
			done <- err
		}()
		<-waiting
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if c.replace {
			if f, err := os.Create(path); err != nil {
				t.Fatal(err)
			} else {
				f.Close()
			}
		}
		hold()
		err = <-done
		if c.gone != errors.Is(err, fs.ErrNotExist) || !c.gone && err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
}

// A path that no longer stats for another reason than its absence is that error, not a file
// "replaced": the lock is released and the cause named.
func TestTakeNamesAStatError(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root stats through any directory")
	}
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "l")
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // a directory
	opens := 0
	open := func(p string) (*os.File, error) {
		opens++
		if opens > 1 { // an open again would find the file: only Same's own error refuses it
			if err := os.Chmod(dir, 0o700); err != nil { // a directory
				return nil, err
			}
			return create(p)
		}
		f, err := create(p)
		if err == nil {
			err = os.Chmod(dir, 0o600) // the directory no longer searched once the file is open
		}
		return f, err
	}
	if _, err := Take(context.Background(), path, open, true, time.Millisecond, nil); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v", err)
	}
}
