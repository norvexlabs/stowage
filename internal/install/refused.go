package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/spf13/cobra"
)

// Refused is a refusal: a rule of the installer said no before the run changed anything. Its
// message is Reason, the text a person reads; Fix is what to run or change, for a program (the
// verbs' `--json` result). The installer exits 3 on one (cmd/stowage-install), wherever it is
// raised: a new refusal is one of these, never a plain error.
type Refused struct {
	Reason, Fix string
	// cause is the error the refusal was made of (refusal), which errors.Is and errors.As reach.
	cause error
	// dockerOnly: Fix holds for a Docker install alone, naming its installer's --dir or the
	// installer it keeps (dockerRefusal); the VM's host daemon answers it as a failure, with no fix.
	dockerOnly bool
}

func (e Refused) Error() string { return e.Reason }

func (e Refused) Unwrap() error { return e.cause }

// ExitRefused is the installer's exit status on a refusal: nothing changed.
const ExitRefused = 3

// changedErr is an error raised once the run changed the install: a failure, whatever it wraps
// (a refusal of a check run again after the change included).
type changedErr struct{ error }

func (e changedErr) Unwrap() error { return e.error }

// changed marks err, raised after the run's first change, as a failure; nil stays nil.
func changed(err error) error {
	if err == nil || errors.As(err, new(changedErr)) {
		return err
	}
	return changedErr{err}
}

// afterChange makes *err a failure once *mutated: deferred by a verb, which sets mutated at its
// first change, so a refusal raised after it (a check run again, a start's) exits as a failure.
func afterChange(mutated *bool, err *error) {
	if *mutated {
		*err = changed(*err)
	}
}

// Refusal is the refusal err ends with, and whether it is one: a Refused raised before the run
// changed anything (never one marked changed).
func Refusal(err error) (Refused, bool) {
	var r Refused
	if err == nil || errors.As(err, new(changedErr)) || !errors.As(err, &r) {
		return Refused{}, false
	}
	return r, true
}

// sameArgs is the fix of a refusal another installer answers: run ctl in this one's place with
// the same arguments, as why says it knows what this one does not; for upgrade --version v when
// v is not empty.
func sameArgs(ctl, why, v string) string {
	fix := "run " + ctl + " instead of this installer, with the same arguments (" + why + ")"
	if v != "" {
		fix = "run " + ctl + " upgrade --version " + v + " instead of this installer, with the same other arguments (" + why + ")"
	}
	return fix
}

// sameFile reports whether the paths a and b name one file.
func sameFile(a, b string) bool {
	x, errA := os.Stat(a)
	y, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(x, y)
}

// laterFix is the fix of an upgrade to a release before the one installed: nothing to do here,
// as the install runs a later release; its installer, ctl, or a later one's moves it on.
func laterFix(ctl string) string {
	return "nothing to do: the install runs a later release; upgrade with " + ctl + " or a later release's installer"
}

// refusal makes err a refusal whose fix is fix, its message unchanged.
func refusal(fix string, err error) error {
	return Refused{Reason: err.Error(), Fix: fix, cause: err}
}

// refuse is a refusal of the message fmt.Errorf formats.
func refuse(fix, format string, a ...any) error {
	return refusal(fix, fmt.Errorf(format, a...))
}

// dockerRefusal is refuse's refusal whose fix holds for a Docker install alone (Refused.dockerOnly).
func dockerRefusal(fix, format string, a ...any) error {
	err := fmt.Errorf(format, a...)
	return Refused{Reason: err.Error(), Fix: fix, cause: err, dockerOnly: true}
}

// vmRefusal is err as a verb run on the VM returns it: a refusal whose fix holds for a Docker
// install alone (Refused.dockerOnly) is a failure there, its message kept.
func vmRefusal(err error) error {
	if r, ok := Refusal(err); ok && r.dockerOnly {
		return errors.New(err.Error())
	}
	return err
}

// As makes a rollback check's refusal a Refused, so it exits 3 as every refusal does.
func (e rollbackRefused) As(target any) bool {
	r, ok := target.(*Refused)
	if ok {
		*r = Refused{Reason: string(e), Fix: "stay on the installed release: going back takes the data from before the upgrade (a restore, or a fresh install)"}
	}
	return ok
}

// As makes a rollback with no revision to go back to a Refused.
func (e noPrevious) As(target any) bool {
	r, ok := target.(*Refused)
	if ok {
		*r = Refused{Reason: e.msg, Fix: "--revision N names the revision to go back to (`helm history` lists them)"}
	}
	return ok
}

// Result is what a verb that changes an install prints under --json, once, at its end: done (it
// changed the install) or nothing (it had nothing to do, was a dry run, or was refused); the
// release installed after it (none after an uninstall, or when nothing is installed); the
// refusal when one said no.
type Result struct {
	Format  int            `json:"format"`
	Result  string         `json:"result"`
	Release *Version       `json:"release,omitempty"`
	Refused *RefusedResult `json:"refused,omitempty"`
}

// RefusedResult is a refusal under --json: the message the run prints, and what to run instead.
type RefusedResult struct {
	Reason string `json:"reason"`
	Fix    string `json:"fix"`
}

// Results of a verb under --json.
const (
	ResultDone    = "done"
	ResultNothing = "nothing"
)

// verbKind is what a verb under --json does: whether it runs on Kubernetes (the release is read
// from helm) and whether it removes the install (no release after it).
type verbKind struct{ k8s, uninstall bool }

// machineVerb makes c print one Result on out at its end when *asJSON: its run (PreRunE, then
// RunE) prints what it prints on the log instead (prepare points Options.Out there), and a
// failure prints no Result, only its error. A verb whose installer ran another for it, which
// printed its own (Installer.delegated), prints none either. ready, when not nil, is run before
// the Result is made: it gives in the options as parsed when the run refused before it took them
// (a flag's refusal, prepare's), so the Result reads the install they name.
func machineVerb(c *cobra.Command, asJSON *bool, out io.Writer, in *Installer, kind verbKind, ready func()) {
	endsOn(c, func(cmd *cobra.Command, err error) error {
		if !*asJSON {
			return err
		}
		if ready != nil {
			ready()
		}
		return in.printResult(cmd.Context(), out, kind, err)
	})
}

// endsOn makes c run its PreRunE, then its RunE when that passed, and return what finish makes of
// the first one's error (nil when both passed): a verb's every outcome, a PreRunE's refusal
// included, reaches finish.
func endsOn(c *cobra.Command, finish func(cmd *cobra.Command, err error) error) {
	pre, run := c.PreRunE, c.RunE
	c.PreRunE = nil
	c.RunE = func(cmd *cobra.Command, args []string) error {
		err := func() error {
			if pre != nil {
				if err := pre(cmd, args); err != nil {
					return err
				}
			}
			return run(cmd, args)
		}()
		return finish(cmd, err)
	}
}

// toMachine points the run's output at the log, its standard output (Out) kept for the Result
// alone (Machine): a verb under --json.
func (o *Options) toMachine() { o.Machine, o.Out = o.Out, o.Log }

// printResult prints the Result of a verb's run that ended with err, and returns err.
func (in *Installer) printResult(ctx context.Context, out io.Writer, kind verbKind, err error) error {
	if in.delegated {
		return err
	}
	return printMachineResult(out, err, in.nothing || in.O.DryRun, func(done bool) *Version {
		switch {
		case kind.uninstall && done:
			return nil
		case kind.k8s:
			return in.k8sReleaseNow(ctx)
		}
		return in.releaseNow()
	})
}

// printMachineResult prints the Result of a run that ended with err, nothing telling a run with
// nothing to do, and returns err: a failure prints none. release is the release installed after
// the run, done telling whether it changed the install.
func printMachineResult(out io.Writer, err error, nothing bool, release func(done bool) *Version) error {
	refused, ok := Refusal(err)
	if err != nil && !ok {
		return err
	}
	res := Result{Format: MachineFormat, Result: ResultDone}
	switch {
	case err != nil:
		res.Result, res.Refused = ResultNothing, &RefusedResult{Reason: err.Error(), Fix: refused.Fix}
	case nothing:
		res.Result = ResultNothing
	}
	res.Release = release(res.Result == ResultDone)
	if perr := printJSON(out, res); perr != nil && err == nil {
		return perr
	}
	return err
}

// releaseNow is the release the Docker install in O.Dir records; nil when it holds none, or an
// install of another project.
func (in *Installer) releaseNow() *Version {
	env, err := ReadEnvFile(in.path(envName))
	if err != nil {
		return nil
	}
	name, v, rev := runs(env)
	n, err := strconv.Atoi(rev)
	if err != nil || v == "" || name != in.R.Manifest.Name {
		return nil
	}
	return &Version{Version: v, Revision: n}
}

// k8sReleaseNow is the release the helm release runs: deployed, as helm lists it; nil when there
// is none, it is not deployed (failed, pending, uninstalled), or it cannot be read (said in the
// log).
func (in *Installer) k8sReleaseNow(ctx context.Context) *Version {
	if in.O.K8s.Helm == nil {
		return nil
	}
	rel, err := in.release(ctx)
	if err != nil || rel == nil || rel.Status != "deployed" {
		if err != nil {
			in.O.logf("the release installed is not known: %v", err)
		}
		return nil
	}
	cur, err := in.installedRelease(ctx)
	if err != nil {
		in.O.logf("the release installed is not known: %v", err)
		return nil
	}
	return &Version{Version: cur.Release.Version, Revision: cur.Release.Revision}
}

// childFailed is the failure of an installer this run started for its own work (a rollback's
// start): its exit status is not this run's, which failed.
func childFailed(exe string, err error) error {
	var code ExitCode
	if errors.As(err, &code) {
		return fmt.Errorf("%s exited with status %d", exe, int(code))
	}
	return err
}
