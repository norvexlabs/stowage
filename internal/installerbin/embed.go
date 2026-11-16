//go:build stowage_installer

package installerbin

import "embed"

//go:embed stowage-install-*.gz stowage-install-*.sha256 stowage-install-*.size
var files embed.FS

func init() { embedded = files }
