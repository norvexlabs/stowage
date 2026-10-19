// Package installerbin holds the installer executable (cmd/stowage-install, linux/amd64) that
// `ship` makes each release's installer image of. The release build embeds it (build tag
// stowage_installer, see the Dockerfile); any other build carries none and cannot ship.
package installerbin

// Executable is the embedded installer; nil in a build without it.
var Executable []byte
