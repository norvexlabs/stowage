package install

import (
	"fmt"
	"io"

	"github.com/norvexlabs/stowage/internal/tune"
	"github.com/spf13/cobra"
)

// tuneBootCmd tunes the VM at boot, before its network and Docker start.
const tuneBootCmd = "tune-boot"

// vmTune is the VM's tune.System: the network profile, its engine the image's (rootful, 24 or
// later), whose daemon file the boot writes before Docker starts.
func vmTune(ctl string) *tune.System {
	s := hostTune(ctl, runHost, tune.SystemLook, tune.Network)
	s.Docker = &tune.Docker{NetworkOpts: true}
	return s
}

// tuneBootCommand is the hidden verb the boot runs as root: the network profile for this boot's
// RAM and CPUs.
func tuneBootCommand(ctl string, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:    tuneBootCmd,
		Short:  "Tune the network for this boot's RAM and CPUs, before the network and Docker start (run at boot)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sys := vmTune(ctl)
			h, err := sys.Host()
			if err != nil {
				return err
			}
			notes, err := sys.Boot(cmd.Context(), h)
			for _, n := range notes {
				fmt.Fprintf(stderr, "note: %s\n", n)
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(stderr, "tuned for %d MiB and %d CPUs: `%s tune` reports\n", h.MemBytes>>20, h.CPUs, ctl)
			return nil
		},
	}
}
