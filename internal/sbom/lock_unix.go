//go:build unix

package sbom

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// lockPoll is how often a wait for the cache tries the lock again.
var lockPoll = 200 * time.Millisecond

// lockCache takes the scanner's cache for itself: a concurrent audit or ship on the same cache
// would otherwise replace the database while this one scans. It waits for another holder,
// saying so once, until ctx ends. The lock goes with the process, however it ends.
func lockCache(ctx context.Context, dir string, logf func(string, ...any)) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ".lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	var tick *time.Ticker
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if tick == nil {
			// The first wait: said once, then the lock is tried again at each tick.
			if logf != nil {
				logf("waiting for the scanner's cache %s (another audit or ship holds it)", dir)
			}
			tick = time.NewTicker(lockPoll)
			defer tick.Stop()
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("waiting for the scanner's cache %s: %w", dir, ctx.Err())
		case <-tick.C:
		}
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}
