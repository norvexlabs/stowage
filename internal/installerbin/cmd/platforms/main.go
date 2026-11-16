// Command platforms prints the platforms the installer is built for (installerbin.Platforms), one
// per line, in their order: the list the Dockerfile's build stage builds, taken from Go alone.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/norvexlabs/stowage/internal/installerbin"
)

func main() {
	if err := write(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// write prints installerbin.Platforms to w, one per line.
func write(w io.Writer) error {
	for _, p := range installerbin.Platforms {
		if _, err := fmt.Fprintln(w, p); err != nil {
			return err
		}
	}
	return nil
}
