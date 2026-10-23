package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/norvexlabs/stowage/internal/tune"
	"github.com/spf13/cobra"
)

// MinDockerNetworkOpts is the engine that reads default-network-opts, where the host's MTU is
// given to the networks Compose makes.
var MinDockerNetworkOpts = [3]int{24, 0, 0}

// TuneOptions is a run of `tune`: a report without a flag.
type TuneOptions struct {
	Check, Print, Apply, Revert, RestartDocker bool
	// Profile is the rules' profile; "" is the one the last apply recorded, else shared.
	Profile tune.Profile
}

// tuneSystem is the host's tune.System with its Docker engine read; on an error, the engine is
// left unknown (nil).
func (in *Installer) tuneSystem(ctx context.Context) (tune.System, error) {
	if in.tuned != nil {
		return *in.tuned, nil
	}
	sys := *in.O.Tune
	out, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"info", "--format", "{{.ServerVersion}}|{{.OperatingSystem}}|{{json .SecurityOptions}}"}})
	if err != nil {
		// Docker Desktop stopped is still Docker Desktop: its CLI context says so.
		if c, cerr := in.O.Docker.Run(ctx, Cmd{Args: []string{"context", "show"}}); cerr == nil && strings.Contains(string(c), "desktop") {
			sys.Docker = &tune.Docker{Desktop: true}
			return sys, nil
		}
		return sys, fmt.Errorf("the Docker engine is not reachable: %w", err)
	}
	f := strings.SplitN(strings.TrimSpace(string(out)), "|", 3)
	if len(f) != 3 {
		return sys, fmt.Errorf("docker info: %q", out)
	}
	v, ok := parseVersion(f[0])
	sys.Docker = &tune.Docker{
		Rootless:    strings.Contains(f[2], "name=rootless"),
		UsernsRemap: strings.Contains(f[2], "name=userns"),
		Desktop:     strings.Contains(f[1], "Docker Desktop"),
		NetworkOpts: ok && !older(v, MinDockerNetworkOpts),
	}
	in.tuned = &sys
	return sys, nil
}

// containerSizing is what each container is given from this host, read once: the network
// settings, and the memory limits of stack.memory; none without a host to read. An engine or a
// host that does not read fails the start: settings dropped for one start would recreate every
// service, and again at the next.
func (in *Installer) containerSizing(ctx context.Context) (map[string]string, memoryPlan, error) {
	if in.O.Tune == nil {
		return nil, memoryPlan{}, nil
	}
	sys, err := in.tuneSystem(ctx)
	if err != nil {
		return nil, memoryPlan{}, fmt.Errorf("the containers' network settings and memory: %w", err)
	}
	h, err := sys.Host()
	if err != nil {
		return nil, memoryPlan{}, fmt.Errorf("the containers' network settings and memory: %w", err)
	}
	return sys.Container(h), in.memoryPlan(sys, h), nil
}

// Tune reports how the host differs from what its RAM and CPUs call for, prints the files that
// apply it, writes and applies them, or removes them.
func (in *Installer) Tune(ctx context.Context, t TuneOptions) error {
	if in.O.Tune == nil {
		return errors.New("tune: no host to tune")
	}
	if in.O.TunedAtBoot && (t.Print || t.Apply || t.Revert) {
		return fmt.Errorf("this VM tunes itself at each boot, before its network and Docker start: `%s tune` reports, --check checks", in.R.Ctl())
	}
	// Only the daemon's file needs the engine: without one it is left out (a revert reads what
	// the apply recorded), so a host whose engine does not start, or that the user's CLI does not
	// reach, is still tuned and reverted.
	sys, err := in.tuneSystem(ctx)
	if err != nil {
		sys.Docker = nil
		if !t.Revert {
			in.O.logf("note: %v: the Docker daemon's file is left out", err)
		}
	}
	if t.Profile != "" {
		sys.Profile = t.Profile
	}
	h, err := sys.Host()
	if err != nil {
		return err
	}
	if !t.Revert {
		if err := sys.CheckProfile(h); err != nil {
			return err
		}
	}
	ctl := in.R.Ctl()
	switch {
	case t.Revert:
		if err := sys.CheckRoot(); err != nil {
			return err
		}
		if err := confirmTune(ctx, in.O.Prompt, in.O.Yes, "Remove the tuning files and put back what the first --apply found"); err != nil {
			return err
		}
		notes, err := sys.Revert(ctx, t.RestartDocker)
		if err != nil {
			return err
		}
		in.notes(notes)
		fmt.Fprintln(in.O.Out, "the tuning files are removed")
		return nil
	case t.Print || t.Apply:
		if t.Apply {
			if err := sys.CheckRoot(); err != nil {
				return err
			}
		}
		p, err := sys.Plan(h)
		if err != nil {
			return err
		}
		if t.Print {
			for _, f := range append(p.Files, deref(p.Daemon)...) {
				fmt.Fprintf(in.O.Out, "# %s\n%s\n", f.Path, f.Content)
			}
			for _, n := range p.Notes {
				fmt.Fprintf(in.O.Out, "# note: %s\n", n)
			}
			return nil
		}
		fmt.Fprintf(in.O.Out, "this host: %d MiB, %d CPUs, the %s profile\n", h.MemBytes>>20, h.CPUs, h.Profile)
		p.Describe(in.O.Out, sys.Root)
		if !p.Changes() {
			fmt.Fprintln(in.O.Out, "the files hold these values already: the running host is set from them")
		}
		if err := confirmTune(ctx, in.O.Prompt, in.O.Yes, "Write these files and apply them to the running host"); err != nil {
			return err
		}
		notes, err := sys.Apply(ctx, p, tune.ApplyOptions{RestartDocker: t.RestartDocker})
		if err != nil {
			return err
		}
		in.notes(notes)
		fmt.Fprintf(in.O.Out, "tuned: `%s tune` reports what still differs\n", ctl)
		return nil
	}
	items := sys.Inspect(ctx, h)
	fmt.Fprintf(in.O.Out, "this host: %d MiB, %d CPUs, the %s profile\n", h.MemBytes>>20, h.CPUs, h.Profile)
	if err := tune.Print(in.O.Out, items); err != nil {
		return err
	}
	n := tune.Drift(items)
	if n == 0 {
		fmt.Fprintln(in.O.Out, "nothing differs")
		return nil
	}
	if in.O.TunedAtBoot {
		name := in.R.Spec.Name
		fmt.Fprintf(in.O.Out, "%d settings differ: the boot tunes this VM, and a reboot tunes it again; `sudo journalctl -b -u %s-tune -u %s-tune-online` says what it could not set\n", n, name, name)
	} else {
		fmt.Fprintf(in.O.Out, "%d settings differ: `%s tune --apply` (as root) writes and applies them, `%s tune --print` prints the files\n", n, ctl, ctl)
	}
	if t.Check {
		return fmt.Errorf("%d settings differ", n)
	}
	return nil
}

func deref(f *tune.File) []tune.File {
	if f == nil {
		return nil
	}
	return []tune.File{*f}
}

func (in *Installer) notes(notes []string) {
	for _, n := range notes {
		in.O.logf("note: %s", n)
	}
}

// confirmTune asks before tune changes the host, unless --yes; without a terminal it refuses.
func confirmTune(ctx context.Context, p Prompter, yes bool, what string) error {
	if yes {
		return nil
	}
	if p == nil {
		return errors.New("tune asks before it changes the host, and there is no terminal to answer: give --yes")
	}
	a, err := p.Ask(ctx, Form{
		Questions: []Question{{Title: what + "?", Key: "confirm", Choices: []Choice{{"no", "No"}, {"yes", "Yes"}}}},
		Defaults:  func(map[string]string) map[string]string { return map[string]string{"confirm": "no"} },
	})
	if err != nil {
		return err
	}
	if a["confirm"] != "yes" {
		return ErrAborted
	}
	return nil
}

// tuneHint says, in one line, that the host is not tuned; it never fails the run.
func (in *Installer) tuneHint(ctx context.Context) {
	if in.O.Tune == nil {
		return
	}
	sys, err := in.tuneSystem(ctx)
	if err != nil {
		return
	}
	h, err := sys.Host()
	if err != nil {
		return
	}
	if n := tune.Drift(sys.Inspect(ctx, h)); n > 0 {
		fmt.Fprintf(in.O.Out, "tuning   %d network settings of this host differ from what its RAM and CPUs call for: `%s tune` shows them\n", n, in.R.Ctl())
	}
}

// hostTune is the machine's tune.System, of profile ("" is the last apply's).
func hostTune(ctl string, run func(ctx context.Context, name string, args ...string) ([]byte, error), look func(string) (string, error), profile tune.Profile) *tune.System {
	return &tune.System{Root: "/", Run: run, Look: look, Euid: os.Geteuid, Ctl: ctl, Profile: profile}
}

// runHost runs a host program, its error carrying what it wrote to stderr.
func runHost(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return out, err
}

// tuneCommand is `tune`; on a VM, which tunes itself at each boot, the report and --check alone.
func tuneCommand(in *Installer, prepare func() error, terminal func(bool) Prompter, vm bool) *cobra.Command {
	var t TuneOptions
	var noInput, accessible, yes bool
	var profile string
	cmd := &cobra.Command{
		Use:   "tune",
		Short: "Report how this host's network settings differ from what its RAM and CPUs call for; --apply writes and applies them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := prepare(); err != nil {
				return err
			}
			if cmd.Flags().Changed("profile") {
				p, err := tune.ParseProfile(profile)
				if err != nil {
					return fmt.Errorf("--profile: %w", err)
				}
				t.Profile = p
			}
			in.O.Yes = yes
			if !noInput && (t.Apply || t.Revert) {
				in.O.Prompt = terminal(accessible)
			}
			return in.Tune(cmd.Context(), t)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&t.Check, "check", false, "report, and exit non-zero when a setting differs")
	if vm {
		cmd.Short = "Report how this VM's network settings differ from what its RAM and CPUs call for (it tunes itself at each boot)"
		return cmd
	}
	f.BoolVar(&t.Print, "print", false, "print the files --apply writes, for a host configured by other means")
	f.BoolVar(&t.Apply, "apply", false, "write the files and apply them to the running host (root; asks first)")
	f.BoolVar(&t.Revert, "revert", false, "remove the files and put back what the first --apply found (root; asks first)")
	f.StringVar(&profile, "profile", "", "shared (a host the admin runs other processes on) or network (a host that runs the stack alone: larger ceilings, NIC rings, flow steering); default: the last --apply's, else shared")
	f.BoolVar(&t.RestartDocker, "restart-docker", false, "--apply, --revert: restart Docker when its daemon file changed (its containers restart with it)")
	f.BoolVarP(&yes, "yes", "y", false, "--apply, --revert: change the host without asking")
	f.BoolVar(&noInput, "no-input", false, "never ask: without --yes, refuse")
	f.BoolVar(&accessible, "accessible", false, "confirm with a plain line prompt")
	cmd.MarkFlagsMutuallyExclusive("check", "print", "apply", "revert")
	return cmd
}
