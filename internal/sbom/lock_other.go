//go:build !unix

package sbom

import "context"

// lockCache is a no-op where flock does not exist.
func lockCache(context.Context, string, func(string, ...any)) (func(), error) {
	return func() {}, nil
}
