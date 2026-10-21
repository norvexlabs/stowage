// Package chown is the recipe of the helper that gives an install's data volumes their uid: a
// busybox image built by `stowage ship` and published as one of the release's images.
package chown

import _ "embed"

// Dockerfile builds the helper. It needs no build context.
//
//go:embed Dockerfile
var Dockerfile []byte

// Key is the helper's key among a release's images: it is published at <prefix>/<name>-chown,
// and no project image may use the key.
const Key = "chown"

// BusyboxVersion is the busybox whose sh, stat and chown the helper runs.
const BusyboxVersion = "1.37.0"
