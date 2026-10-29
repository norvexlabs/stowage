package install

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/tune"
	"github.com/spf13/cobra"
)

// TuneOptions is a run of `tune`: a report without a flag.
type TuneOptions struct {
	Check, Print, Apply, Revert, RestartDocker bool
	// Profile is the rules' profile; "" is the stack's (the higher of it and the last apply's on
	// a Docker host).
	Profile tune.Profile
}

// tuneSystem is the host's tune.System with its Docker engine read; on an error, the engine is
// left unknown (nil).
func (in *Installer) tuneSystem(ctx context.Context) (tune.System, error) {
	if in.tuned != nil {
		return *in.tuned, nil
	}
	sys := *in.O.Tune
	releaseHost(&sys, in.R.Spec)
	if !sys.Owned {
		get, _ := in.installedEnv()
		roleDevices(&sys, in.R.Spec, func(r string) string { return in.roleDevice(r, get) })
		// The forwarding a start as root applied as the release and the devices now give it (its
		// record holds those rules), through roles that each have a device here, is reported so
		// rather than advised.
		cur, _ := os.ReadFile(in.forwardRecord())
		want, _ := forwardRules(in.R.Spec.Host, in.forwardChain(), func(r string) string { return in.roleDevice(r, get) })
		applied := in.gatewayConsented() && want != nil && bytes.Equal(cur, want)
		for i := range sys.Interfaces {
			ti := &sys.Interfaces[i]
			ti.Applied = applied && !slices.Contains(slices.Collect(maps.Values(ti.Devices)), "")
		}
	}
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
	sys.Docker = &tune.Docker{
		Rootless:    strings.Contains(f[2], "name=rootless"),
		UsernsRemap: strings.Contains(f[2], "name=userns"),
		Desktop:     strings.Contains(f[1], "Docker Desktop"),
	}
	in.tuned = &sys
	return sys, nil
}

// containerSizing is what each container is given from this host, read once: the settings that
// run it as on the host, by the stack's tuning profile as get reads the install, and the memory
// limits of the services' memory shares and their whole cores; none without a host to read. An engine or a host that does not read
// fails the start: settings dropped for one start would recreate every service, and again at the
// next.
func (in *Installer) containerSizing(ctx context.Context, get func(string) string) (tune.Container, hostPlan, error) {
	if in.O.Tune == nil {
		return tune.Container{}, hostPlan{}, nil
	}
	sys, err := in.tuneSystem(ctx)
	if err != nil {
		return tune.Container{}, hostPlan{}, fmt.Errorf("the containers' network settings and memory: %w", err)
	}
	h, err := sys.Host()
	if err != nil {
		return tune.Container{}, hostPlan{}, fmt.Errorf("the containers' network settings and memory: %w", err)
	}
	h.Profile, _ = in.tuneProfile(get)
	plan := in.memoryPlan(sys, h)
	if sys.Docker == nil || !sys.Docker.Desktop {
		if plan.CPUSets, err = cpuSets(in.R.Spec, sys.Cores(h)); err != nil {
			return tune.Container{}, hostPlan{}, err
		}
		plan.Host = h
	}
	return sys.Container(h), plan, nil
}

// Tune reports how the host differs from what its RAM and CPUs call for, prints the files that
// apply it, writes and applies them, or removes them.
func (in *Installer) Tune(ctx context.Context, t TuneOptions) error {
	if in.O.Tune == nil {
		return errors.New("tune: no host to tune")
	}
	// The gateway's names and its boot unit take the directory absolute, as a start does.
	if in.O.Dir != "" {
		dir, err := filepath.Abs(in.O.Dir)
		if err != nil {
			return err
		}
		in.O.Dir = dir
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
	sys.Profile = t.Profile
	h, err := sys.Host()
	if err != nil {
		return err
	}
	if t.Profile == "" {
		h = in.hostProfile(sys, h)
	} else {
		h.Reserved = in.stackPorts(in.reportEnv(true))
	}
	if !t.Revert {
		if err := sys.CheckProfile(h); err != nil {
			return err
		}
	}
	ctl := in.R.Ctl()
	// A record of another directory refuses an apply of the declarations before the host is changed
	// in anything; a revert there leaves the record alone, as it applied nothing here.
	rec, elsewhere := in.gatewayOtherDir()
	if _, err := in.readGateway(); err != nil && in.gatewayRecorded() && (t.Revert || t.Apply && !sys.Owned && in.gatewayDeclared()) {
		return fmt.Errorf("%w: fix or remove it, then tune again", err)
	}
	if elsewhere && t.Apply && !sys.Owned && in.gatewayDeclared() {
		return errors.New(in.gatewayElsewhere(rec))
	}
	revertGateway := in.gatewayRecorded() && in.gatewayRevertsHere(rec)
	switch {
	case t.Revert:
		if err := sys.CheckRoot(); err != nil {
			return err
		}
		what := "Remove the tuning files and put back what the first --apply found"
		if elsewhere && !revertGateway {
			in.O.logf("note: %s", in.gatewayElsewhere(rec))
		}
		if revertGateway {
			what += ", and remove " + in.gatewayWhat() + " (the consent withdrawn)"
		}
		if err := confirmTune(ctx, in.O.Prompt, in.O.Yes, what); err != nil {
			return err
		}
		// The install's own first: a tuning revert that fails does not keep them.
		var gwErr error
		if revertGateway {
			gwErr = in.lockedRevert(ctx)
		}
		notes, err := sys.Revert(ctx, t.RestartDocker)
		if err != nil {
			return errors.Join(gwErr, err)
		}
		in.notes(notes)
		fmt.Fprintln(in.O.Out, "the tuning files are removed")
		return gwErr
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
			// The services' nftables tables are the operator's to load on a host Stowage does not
			// own: printed, never written.
			if !sys.Owned {
				for _, name := range sortedKeys(in.R.Nftables) {
					fmt.Fprintf(in.O.Out, "# services.%s.nftables (not written: load it with nft -f <file>, the host's firewall is its operator's)\n%s\n", name, in.R.Nftables[name])
				}
			}
			return nil
		}
		fmt.Fprintf(in.O.Out, "this host: %d MiB, %d CPUs, the %s profile\n", h.MemBytes>>20, h.CPUs, h.Profile)
		p.Describe(in.O.Out, sys.Root)
		if !p.Changes() {
			fmt.Fprintln(in.O.Out, "the files hold these values already: the running host is set from them")
		}
		gateway := !sys.Owned && in.gatewayDeclared()
		switch {
		case gateway && !fileExists(in.path(envName)):
			in.O.logf("note: no install in %s (--dir): %s are applied once one is", in.O.Dir, in.gatewayWhat())
			gateway = false
		case gateway:
			fmt.Fprintf(in.O.Out, "and %s: applied by the install's start now, by every start and at every boot (a unit), until --revert\n", in.gatewayWhat())
		}
		if err := confirmTune(ctx, in.O.Prompt, in.O.Yes, "Write these files and apply them to the running host"); err != nil {
			return err
		}
		notes, err := sys.Apply(ctx, p, tune.ApplyOptions{RestartDocker: t.RestartDocker})
		if err != nil {
			return err
		}
		in.notes(notes)
		if gateway {
			if err := in.gatewayApply(ctx); err != nil {
				return err
			}
		}
		fmt.Fprintf(in.O.Out, "tuned: `%s tune` reports what still differs\n", ctl)
		return nil
	}
	items := sys.Inspect(ctx, h)
	if !sys.Owned {
		consented := in.gatewayConsented()
		rec, recErr := in.readGateway()
		revoked := recErr == nil && rec.Revoked && in.gatewayHere(rec)
		var loaded []string
		if consented || revoked {
			loaded = in.loadedTables(ctx)
		}
		for _, name := range sortedKeys(in.R.Nftables) {
			item := tune.Item{State: tune.Advice, Kind: "nftables", Name: "table inet " + name,
				Have: "the service " + name + " asks for its nftables table:", Want: "`" + ctl + " tune --apply` loads it at each start, or nft -f <file> from `" + ctl + " tune --print` (the host's firewall is its operator's)"}
			switch {
			case consented && slices.Contains(loaded, name):
				item.State, item.Have, item.Want = tune.OK, "loaded by each start (tune --apply)", "loaded"
			case consented:
				item.State, item.Have, item.Want = tune.Differs, "not loaded", "loaded by the next start as root (tune --apply)"
			case revoked && slices.Contains(loaded, name):
				item.State, item.Have, item.Want = tune.Differs, "still loaded, the consent withdrawn", "`"+ctl+" tune --revert` again removes it"
			case recErr == nil && in.gatewayRecorded() && !in.gatewayHere(rec):
				item.Want = in.gatewayElsewhere(rec)
			case recErr != nil:
				item.Want = recErr.Error() + ": fix or remove it, then `" + ctl + " tune --apply` loads it at each start"
			}
			items = append(items, item)
		}
	}
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
	h = in.hostProfile(sys, h)
	if n := tune.Drift(sys.Inspect(ctx, h)); n > 0 {
		fmt.Fprintf(in.O.Out, "tuning   %d network settings of this host differ from what the %s profile calls for on its RAM and CPUs: `%s tune` shows them\n", n, h.Profile, in.R.Ctl())
	}
}

// roleDevices gives each interface the devices of the roles it names (dev), where roles are not
// their interfaces' names: a tun role's is its own, a NIC role's the one dev knows, if any.
func roleDevices(sys *tune.System, s *spec.Spec, dev func(string) string) {
	for i := range sys.Interfaces {
		ti := &sys.Interfaces[i]
		ti.Devices = map[string]string{}
		for _, r := range append([]string{ti.Role}, ti.Forward...) {
			ti.Devices[r] = dev(r)
		}
	}
}

// hostTune is the machine's tune.System, of profile ("" is the last apply's).
func hostTune(ctl string, run func(ctx context.Context, name string, args ...string) ([]byte, error), look func(string) (string, error), profile tune.Profile) *tune.System {
	return &tune.System{Root: "/", Run: run, Look: look, Euid: os.Geteuid, Ctl: ctl, Profile: profile}
}

// releaseModules are the kernel modules the release's host.modules declares, by name.
func releaseModules(s *spec.Spec) []tune.Module {
	if s.Host == nil {
		return nil
	}
	mods := s.Host.Modules
	var out []tune.Module
	for _, name := range slices.Sorted(maps.Keys(mods)) {
		m := mods[name]
		out = append(out, tune.Module{Name: name, Load: m.Load, Block: m.Block, Options: m.Options, Reason: m.Reason})
	}
	return out
}

// releasePackages are the distribution packages the release's host.packages declares, by name.
func releasePackages(s *spec.Spec) []tune.Package {
	if s.Host == nil {
		return nil
	}
	var out []tune.Package
	for _, name := range slices.Sorted(maps.Keys(s.Host.Packages)) {
		out = append(out, tune.Package{Name: name, Reason: s.Host.Packages[name].Reason})
	}
	return out
}

// releaseHost gives sys what the release's host block asks of the host: its modules, packages,
// firmware, interfaces and kernel arguments, which tune judges; its settings and power posture, which it also applies.
func releaseHost(sys *tune.System, s *spec.Spec) {
	sys.Modules, sys.Packages = releaseModules(s), releasePackages(s)
	sys.ServiceCores = coreRequests(s)
	sys.Firmware, sys.Interfaces, sys.KernelArgs, sys.Settings, sys.Power = nil, nil, nil, nil, ""
	if s.Host == nil {
		return
	}
	for _, r := range slices.Sorted(maps.Keys(s.Host.Interfaces)) {
		i := s.Host.Interfaces[r]
		ti := tune.Interface{Role: r, Kind: cmp.Or(i.Kind, spec.InterfaceNIC), MTU: i.MTU,
			RoutingPolicyRules: i.RoutingPolicyRules, Routes: i.Routes, Forward: i.Forward, Reason: i.Reason}
		for _, svc := range s.Services {
			ti.Transparent = ti.Transparent || slices.Contains(svc.Transparent, r)
		}
		if e := i.Ethtool; e != nil {
			ti.NIC = &tune.NICSettings{Features: e.Features, EEE: e.EEE}
			if e.Channels != nil {
				ti.NIC.Combined = e.Channels.Combined
			}
			if e.Rings != nil {
				ti.NIC.RX, ti.NIC.TX = e.Rings.RX, e.Rings.TX
			}
		}
		sys.Interfaces = append(sys.Interfaces, ti)
	}
	for _, p := range slices.Sorted(maps.Keys(s.Host.Firmware)) {
		sys.Firmware = append(sys.Firmware, tune.Firmware{Path: p, Reason: s.Host.Firmware[p].Reason})
	}
	for _, k := range slices.Sorted(maps.Keys(s.Host.KernelArgs)) {
		a := s.Host.KernelArgs[k]
		sys.KernelArgs = append(sys.KernelArgs, tune.KernelArg{Key: k, Value: a.Value, Reason: a.Reason})
	}
	for _, k := range slices.Sorted(maps.Keys(s.Host.Sysctls)) {
		c := s.Host.Sysctls[k]
		sys.Settings = append(sys.Settings, tune.Sysctl{Key: k, Want: c.Value, Why: c.Reason, Rule: tune.Exact})
	}
	sys.Power = s.Host.Power
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
	f.StringVar(&profile, "profile", "", "default (Docker's path as fast as the host's network) or network (a host serving a heavy network load: larger ceilings, BBR, many connections' ports and neighbours, NIC rings, interrupts and flow steering, the CPUs at full speed); default: the stack's, or the last --apply's when it is higher")
	f.BoolVar(&t.RestartDocker, "restart-docker", false, "--apply, --revert: restart Docker when its daemon file changed (its containers restart with it)")
	f.BoolVarP(&yes, "yes", "y", false, "--apply, --revert: change the host without asking")
	f.BoolVar(&noInput, "no-input", false, "never ask: without --yes, refuse")
	f.BoolVar(&accessible, "accessible", false, "confirm with a plain line prompt")
	cmd.MarkFlagsMutuallyExclusive("check", "print", "apply", "revert")
	return cmd
}
