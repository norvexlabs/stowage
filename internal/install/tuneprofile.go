package install

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"

	"github.com/norvexlabs/stowage/internal/tune"
	"github.com/spf13/pflag"
)

// TuneProfileVar records --tune-profile: the operator's tuning profile, over the release's
// tune.profile; absent follows the release.
const TuneProfileVar = "STOWAGE_TUNE_PROFILE"

// tuneProfileRelease is --tune-profile's value that gives the choice back to the release.
const tuneProfileRelease = "release"

// tuneProfileValue is --tune-profile: a profile, or release.
type tuneProfileValue struct{ v **string }

func (t tuneProfileValue) String() string {
	if *t.v == nil {
		return ""
	}
	return **t.v
}

func (t tuneProfileValue) Set(s string) error {
	if _, err := tune.ParseProfile(s); err != nil && s != tuneProfileRelease {
		return fmt.Errorf("%q: default, network or release", s)
	}
	*t.v = &s
	return nil
}

func (tuneProfileValue) Type() string { return "profile" }

// profileValue is a flag that takes a profile alone (`vm create --tune-profile`).
type profileValue struct{ v *string }

func (p profileValue) String() string { return *p.v }

func (p profileValue) Set(s string) error {
	if _, err := tune.ParseProfile(s); err != nil {
		return err
	}
	*p.v = s
	return nil
}

func (profileValue) Type() string { return "profile" }

// tuneProfileFlag adds --tune-profile to f.
func tuneProfileFlag(f *pflag.FlagSet, v **string, usage string) {
	f.Var(tuneProfileValue{v}, "tune-profile", usage)
}

// tuneVar records --tune-profile: a profile, or release, which removes the record; and says when
// the stack's profile changes.
func (in *Installer) tuneVar(env *EnvFile) {
	get := func(k string) string { v, _ := env.Get(k); return v }
	was, _ := in.tuneProfile(get)
	switch p := in.O.TuneProfile; {
	case p == nil:
		return
	case *p == tuneProfileRelease:
		env.Delete(TuneProfileVar)
	default:
		env.Set(TuneProfileVar, *p)
	}
	if now, by := in.tuneProfile(get); now != was {
		in.O.logf("tuning: the %s profile, the %s's (was %s)", now, by, was)
	}
}

// tuneProfile is the stack's tuning profile as get reads the install: the operator's
// (TuneProfileVar), else the release's; and whose it is, "operator" or "release".
func (in *Installer) tuneProfile(get func(string) string) (tune.Profile, string) {
	if p, err := tune.ParseProfile(get(TuneProfileVar)); err == nil {
		return p, "operator"
	}
	if p, err := tune.ParseProfile(in.R.Spec.TuneProfile()); err == nil {
		return p, "release"
	}
	return tune.Default, "release"
}

// stackPorts are the host ports the stack listens on, as get reads the install: the front's and
// each L4 route's; the ports the network profile keeps out of the outgoing connections' range.
func (in *Installer) stackPorts(get func(string) string) []int {
	var keys []string
	if in.R.Spec.HasFront() {
		keys = append(keys, HTTPPortVar, HTTPSPortVar)
		for _, r := range in.R.Spec.Front.Routes {
			keys = append(keys, PortVar(r.Name))
		}
	}
	var out []int
	for _, k := range keys {
		if p, err := strconv.Atoi(get(k)); err == nil && p > 0 {
			out = append(out, p)
		}
	}
	for _, r := range in.R.Spec.L4.Routes {
		out = append(out, r.Port)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// installedEnv reads the install's recorded values; none before an install, or when the record
// does not read (it is root's: 0600), which err says.
func (in *Installer) installedEnv() (get func(string) string, err error) {
	env, err := ReadEnvFile(in.path(envName))
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if env == nil {
		return func(string) string { return "" }, err
	}
	return func(k string) string { v, _ := env.Get(k); return v }, nil
}

// reportEnv is installedEnv for a report: a record that does not read is said, as the
// release's profile (unless profileGiven) and no port of the stack's are then judged.
func (in *Installer) reportEnv(profileGiven bool) func(string) string {
	get, err := in.installedEnv()
	switch {
	case err != nil && profileGiven:
		in.O.logf("note: the install's record does not read (%v): none of the stack's ports is taken (run as root)", err)
	case err != nil && in.O.TunedAtBoot:
		in.O.logf("note: the install's record does not read (%v): the release's tuning profile and none of the stack's ports are taken (run as root)", err)
	case err != nil:
		in.O.logf("note: the install's record does not read (%v): the release's tuning profile and none of the stack's ports are taken (run as root, or give --profile)", err)
	}
	return get
}

// hostProfile is the profile this host's tune judges and applies by default: on a VM the one its
// boot applied (a lower one the operator chose since takes over at the next boot), else the
// stack's; on a Docker host the higher of the stack's and the last apply's, the host being the
// admin's.
func (in *Installer) hostProfile(sys tune.System, h tune.Host) tune.Host {
	if booted, ports, ok := sys.Booted(); in.O.TunedAtBoot && ok {
		// What the boot applied: the profile and the ports it reserved.
		h.Profile, h.Reserved = booted, ports
		return h
	}
	get := in.reportEnv(false)
	stack, _ := in.tuneProfile(get)
	if in.O.TunedAtBoot {
		h.Profile = stack
	} else {
		h.Profile = tune.Higher(h.Profile, stack)
	}
	h.Reserved = in.stackPorts(get)
	return h
}
