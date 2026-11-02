//go:build !linux

package install

import "errors"

// flushConntrack has no connection-tracking table to flush off Linux.
func flushConntrack() error {
	return errors.New("the connection-tracking table is Linux's")
}
