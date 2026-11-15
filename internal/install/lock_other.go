//go:build !unix

package install

import "context"

// lockFile is a no-op where flock does not exist; the installer runs on Unix hosts.
func lockFile(string) (func(), error) { return func() {}, nil }

// flockWait is a no-op where flock does not exist; the installer runs on Unix hosts.
func flockWait(context.Context, string, bool) (func(), error) { return func() {}, nil }

// lockHeld is false where flock does not exist; the installer runs on Unix hosts.
func lockHeld(string) bool { return false }

// keepFromChildren is a no-op where descriptors are not inherited by number.
func keepFromChildren(string) {}
