// Package backup is the recipe of the runner of the Kairos VM's backups: an rclone image built by
// `stowage ship` and published as one of the release's images.
package backup

import _ "embed"

// Dockerfile builds the runner. It needs no build context.
//
//go:embed Dockerfile
var Dockerfile []byte

// Key is the runner's key among a release's images: it is published at <prefix>/<name>-backup,
// and no project image may use the key.
const Key = "backup"

// RcloneVersion is the rclone the runner runs.
const RcloneVersion = "1.75.1"
