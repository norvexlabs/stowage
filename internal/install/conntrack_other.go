//go:build !linux

package install

import "errors"

// flushConntrack has no connection-tracking table to flush off Linux.
func flushConntrack() error {
	return errors.New("the connection-tracking table is Linux's")
}

// deleteConntrack has no connection-tracking table to delete from off Linux.
func deleteConntrack(func(ctEntry) bool) (int, error) {
	return 0, errors.New("the connection-tracking table is Linux's")
}
