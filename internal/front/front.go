// Package front is the recipe of the front Stowage adds to every Docker install: a Caddy image
// built by `stowage ship` and published as one of the release's images.
package front

import _ "embed"

// Dockerfile builds the front. It needs no build context.
//
//go:embed Dockerfile
var Dockerfile []byte

// Key is the front's key among a release's images: it is published at <prefix>/<name>-front, and
// no project image may use the key.
const Key = "front"

// CaddyVersion is the Caddy the front runs.
const CaddyVersion = "2.11.7"
