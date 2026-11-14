//go:build unix

package sbom

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/norvexlabs/stowage/internal/flock"
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
	unlock, err := flock.Take(ctx, filepath.Join(dir, ".lock"), flock.Create, true, lockPoll, func() {
		if logf != nil {
			logf("waiting for the scanner's cache %s (another audit or ship holds it)", dir)
		}
	})
	if err != nil && ctx.Err() != nil {
		return nil, fmt.Errorf("waiting for the scanner's cache %s: %w", dir, err)
	}
	if err != nil {
		return nil, err
	}
	return unlock, nil
}
