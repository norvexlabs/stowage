//go:build !unix

package hook

import "errors"

var errUnix = errors.New("the post-commit hook needs a unix host")

// Lock is not supported outside unix.
func Lock(string, func()) (func(), error) { return nil, errUnix }

// Launch is not supported outside unix.
func Launch(string, []string, string) error { return errUnix }
