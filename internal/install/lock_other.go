//go:build !unix

package install

// lockFile is a no-op where flock does not exist; the installer runs on Unix hosts.
func lockFile(string) (func(), error) { return func() {}, nil }

// flockWait is a no-op where flock does not exist; the installer runs on Unix hosts.
func flockWait(string, bool) (func(), error) { return func() {}, nil }

// lockHeld is false where flock does not exist; the installer runs on Unix hosts.
func lockHeld(string) bool { return false }
