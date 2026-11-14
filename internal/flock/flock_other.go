//go:build !unix

package flock

import (
	"context"
	"errors"
	"os"
	"time"
)

// ErrHeld is a lock another process holds; never, where flock does not exist.
var ErrHeld = errors.New("held by another process")

// Take opens path and takes no lock where flock does not exist.
func Take(_ context.Context, path string, open func(string) (*os.File, error), _ bool, _ time.Duration, _ func()) (func(), error) {
	f, err := open(path)
	if err != nil {
		return nil, err
	}
	return func() { f.Close() }, nil
}

// Create opens a lock file read-write, made 0600 when absent.
func Create(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
}

// Same is true where flock does not exist: no lock to lose.
func Same(*os.File, string) (bool, error) { return true, nil }
