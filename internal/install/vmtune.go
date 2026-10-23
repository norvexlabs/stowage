package install

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/norvexlabs/stowage/internal/tune"
	"github.com/spf13/cobra"
)

// tuneBootCmd tunes the VM at boot, before its network and Docker start.
const tuneBootCmd = "tune-boot"

// vmTune is the VM's tune.System: its engine the image's (rootful), whose daemon file the boot
// writes before Docker starts; its profile is the stack's (vmProfile).
func vmTune(ctl string) *tune.System {
	s := hostTune(ctl, runHost, tune.SystemLook, "")
	s.Docker = &tune.Docker{}
	s.Owned = true
	return s
}

// vmProfile is the profile the VM's boot applies: the stack's as the install records it; before
// the first install, the settings file's (`vm create --tune-profile`), else the release's.
func (in *Installer) vmProfile() tune.Profile {
	get, _ := in.installedEnv()
	if _, err := os.Stat(in.path(envName)); err != nil {
		if st, err := ReadSettings(cmp.Or(in.O.Settings, SettingsFile(in.R.Spec.Name))); err == nil && st != nil && st.TuneProfile != tuneProfileRelease {
			get = func(k string) string {
				if k == TuneProfileVar {
					return st.TuneProfile
				}
				return ""
			}
		}
	}
	p, _ := in.tuneProfile(get)
	return p
}

// bootTune tunes the VM now as its boot does, by its stack's profile.
func (in *Installer) bootTune(ctx context.Context, sys *tune.System, stderr io.Writer) error {
	sys.Profile = in.vmProfile()
	h, err := sys.Host()
	if err != nil {
		return err
	}
	get, _ := in.installedEnv()
	h.Reserved = in.stackPorts(get)
	notes, err := sys.Boot(ctx, h)
	for _, n := range notes {
		fmt.Fprintf(stderr, "note: %s\n", n)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "tuned by the %s profile for %d MiB and %d CPUs: `%s tune` reports\n", h.Profile, h.MemBytes>>20, h.CPUs, in.R.Ctl())
	return nil
}

// tuneBootCommand is the hidden verb the boot runs as root: the stack's profile for this boot's
// RAM and CPUs.
func tuneBootCommand(r *Release, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:    tuneBootCmd,
		Short:  "Tune the network for this boot's RAM and CPUs, before the network and Docker start (run at boot)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in := &Installer{R: r, O: Options{Dir: VMDir(r.Spec.Name), TunedAtBoot: true}}
			return in.bootTune(cmd.Context(), vmTune(r.Ctl()), stderr)
		},
	}
}

// retune tunes the VM at once when its install's profile is higher than the one the last boot
// applied (BootProfileFile), or on the network profile reserves other ports; a lower profile
// takes over at the next reboot, the higher one's settings staying until then. Judged against
// what the VM holds, so a re-run after one that failed before it tuned still tunes it. The
// install is done by then: what fails here is a note, and the next boot tunes the VM again.
func (in *Installer) retune(ctx context.Context, sys *tune.System, stderr io.Writer) {
	applied, ports, ok := sys.Booted()
	if !ok {
		return // no boot of this Stowage tuned the VM: its next boot does
	}
	get, _ := in.installedEnv()
	now, want := in.vmProfile(), in.stackPorts(get)
	switch {
	case now == applied && (now != tune.Network || slices.Equal(want, ports)):
		return
	case tune.Higher(applied, now) == now:
		if err := in.bootTune(ctx, sys, stderr); err != nil {
			in.O.logf("note: tuning the VM now failed (%v): its next boot tunes it", err)
		}
		return
	}
	in.O.logf("tuning: the %s profile takes over at the next reboot (`%s reboot`): the %s profile's settings stay until then", now, in.R.Ctl(), applied)
}
