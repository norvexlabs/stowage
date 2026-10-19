//go:build stowage_installer

package installerbin

import _ "embed"

//go:embed stowage-install
var embedded []byte

func init() { Executable = embedded }
