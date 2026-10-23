package install

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// maxLogTail bounds `logs --tail`, as the VM's daemon does.
const maxLogTail = 5000

// stackService opens the install in O.Dir and checks that name is one of its stack's services
// (the front's among them).
func (in *Installer) stackService(ctx context.Context, name string) (*EnvFile, error) {
	env, err := in.open()
	if err != nil {
		return nil, err
	}
	out, err := in.Compose(ctx, env, nil, "config", "--services")
	if err != nil {
		return nil, err
	}
	names := strings.Fields(string(out))
	if !slices.Contains(names, name) {
		return nil, fmt.Errorf("%q is no service of the stack (%s)", name, strings.Join(names, ", "))
	}
	return env, nil
}

// Ps prints the stack's services, their state and health: Compose's table, or its JSON lines.
func (in *Installer) Ps(ctx context.Context, asJSON bool) error {
	env, err := in.open()
	if err != nil {
		return err
	}
	args := []string{"ps", "--all"}
	if asJSON {
		args = append(args, "--format", "json")
	}
	_, err = in.O.Docker.Run(ctx, Cmd{Args: append(in.composeBase(), args...), Env: env.Environ(in.O.environ()), Stdout: in.O.Out, Stream: in.O.Log})
	return err
}

// Logs prints a service's last tail log lines (1 to maxLogTail), and with follow the ones it
// writes next, until interrupted.
func (in *Installer) Logs(ctx context.Context, service string, tail int, follow bool) error {
	if tail < 1 || tail > maxLogTail {
		return fmt.Errorf("--tail %d: 1 to %d", tail, maxLogTail)
	}
	env, err := in.stackService(ctx, service)
	if err != nil {
		return err
	}
	args := []string{"logs", "--no-color", "--tail", fmt.Sprint(tail)}
	if follow {
		args = append(args, "--follow")
	}
	_, err = in.O.Docker.Run(ctx, Cmd{Args: append(in.composeBase(), append(args, service)...), Env: env.Environ(in.O.environ()), Stdout: in.O.Out, Stream: in.O.Log})
	if follow && errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	return err
}

// ServiceVerbs are the verbs `restart`, `stop` and `start` run on one service.
var ServiceVerbs = []string{"restart", "stop", "start"}

// Service restarts, stops or starts one service of the stack, under the install's lifecycle lock:
// never in the middle of an upgrade, a rollback, a provision or a reset-db. A service it stops
// stays stopped until `start`, or until a run brings the stack up (a re-run, an upgrade).
func (in *Installer) Service(ctx context.Context, verb, service string) error {
	if !slices.Contains(ServiceVerbs, verb) {
		return fmt.Errorf("unknown verb %q (%s)", verb, strings.Join(ServiceVerbs, ", "))
	}
	if _, err := in.stackService(ctx, service); err != nil {
		return err
	}
	env, unlock, err := in.openLocked(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := in.volumesSplit(); err != nil {
		return err
	}
	_, err = in.Compose(ctx, env, in.O.Log, verb, service)
	return err
}

// Exec runs args in a running service's container, as `docker compose exec` does: with a
// terminal when stdin is one, and the command's exit status as the run's.
func (in *Installer) Exec(ctx context.Context, service string, args []string, stdin io.Reader, terminal bool) error {
	if len(args) == 0 {
		return errors.New("exec: give the command after --, as exec SERVICE -- psql")
	}
	env, err := in.stackService(ctx, service)
	if err != nil {
		return err
	}
	c := []string{"exec"}
	if !terminal {
		c = append(c, "-T")
	}
	_, err = in.O.Docker.Run(ctx, Cmd{Args: append(in.composeBase(), append(append(c, service), args...)...), Env: env.Environ(in.O.environ()), Stdin: stdin, Stdout: in.O.Out, Stream: in.O.Log})
	var ee interface{ ExitCode() int }
	if errors.As(err, &ee) && ee.ExitCode() > 0 {
		return ExitCode(ee.ExitCode())
	}
	return err
}

// SchemaReport is what `schema` prints: the database's migration, the release the install keeps
// for `rollback` and the migration it recorded before the upgrade, and the rollback barriers
// after a migration (After: the kept one's, or the one asked).
type SchemaReport struct {
	Migration string `json:"migration"`
	Kept      *state `json:"kept,omitempty"`
	After     string `json:"after,omitempty"`
	Barriers  string `json:"barriers,omitempty"`
}

// Schema reports the database's schema as the rollback check reads it, on the running stack.
// after, when set, is the migration whose later barriers it reports; else the one the kept
// release recorded, when there is one.
func (in *Installer) Schema(ctx context.Context, after string, asJSON bool) error {
	if !in.schemaOn() {
		return fmt.Errorf("%s checks no schema: database.schema_compat is not set", in.R.Spec.Name)
	}
	if after != "" && !schemaVersion.MatchString(after) {
		return fmt.Errorf("--after %q: a migration number (at most 15 digits, no leading zero)", after)
	}
	env, err := in.open()
	if err != nil {
		return err
	}
	s := schema{in: in, stack: env}
	var rep SchemaReport
	if rep.Migration, err = s.current(ctx, env); err != nil {
		return fmt.Errorf("the database's migration: %w", err)
	}
	if raw, err := os.ReadFile(filepath.Join(in.path(PrevDir), prevState)); err == nil {
		var st state
		if json.Unmarshal(raw, &st) == nil {
			rep.Kept = &st
		}
	}
	rep.After = after
	if after == "" && rep.Kept != nil && schemaVersion.MatchString(rep.Kept.Schema) {
		rep.After = rep.Kept.Schema
	}
	if rep.After != "" {
		if rep.Barriers, err = s.barriers(ctx, rep.After); err != nil {
			return fmt.Errorf("the rollback barriers after migration %s: %w", rep.After, err)
		}
	}
	if asJSON {
		return json.NewEncoder(in.O.Out).Encode(rep)
	}
	w := in.O.Out
	fmt.Fprintf(w, "migration  %s\n", rep.Migration)
	if k := rep.Kept; k != nil {
		at := "its migration not recorded"
		if k.Schema != "" {
			at = "at migration " + k.Schema + " before the upgrade"
		}
		fmt.Fprintf(w, "kept       %s (revision %d), %s: `rollback` goes back to it\n", k.ProjectVersion, k.Revision, at)
	}
	if rep.After != "" {
		fmt.Fprintf(w, "barriers   after %s: %s\n", rep.After, cmp.Or(rep.Barriers, "none"))
	}
	return nil
}

// Uninstall removes the Docker install in O.Dir: its containers and networks and its host timers,
// and with data its volumes and the install directory (its secrets, the dumps, the release kept for rollback).
// Without data, both are kept, and a re-run of the installer with --dir brings the install back
// on them. It asks on a terminal (default no), and refuses without one unless O.Yes.
func (in *Installer) Uninstall(ctx context.Context, data bool) error {
	env, err := in.open()
	if err != nil {
		return err
	}
	unlock, err := lockFile(in.path(lifecycleLock))
	if errors.Is(err, errLocked) {
		return errors.New("a provision, upgrade, rollback or reset-db is running for this install: wait for it to finish")
	}
	if err != nil {
		return err
	}
	defer unlock()
	name, version, _, err := sameProject(in.O.Dir, env, in.R.Manifest)
	if err != nil {
		return err
	}
	dir := in.O.Dir
	project, _ := env.Get("COMPOSE_PROJECT_NAME")
	what := fmt.Sprintf("uninstall %s %s from %s (Compose project %s)", name, version, dir, project)
	if in.O.DryRun {
		in.O.logf("a run would %s: its containers and networks, and its backup and disk-pressure timers", what)
		if data {
			in.O.logf("and delete its volumes and %s", dir)
		} else {
			in.O.logf("and keep its volumes and %s", dir)
		}
		return nil
	}
	switch {
	case in.O.Yes:
	case in.O.Prompt == nil:
		return fmt.Errorf("uninstall asks first, and there is no terminal to answer: give --yes (with --data to delete the data)")
	default:
		choices := []Choice{{"no", "No"}, {"keep", "Yes, and keep the data"}, {"delete", "Yes, and delete the data"}}
		if data {
			choices = []Choice{{"no", "No"}, {"delete", "Yes, and delete the data"}}
		}
		a, err := in.O.Prompt.Ask(ctx, Form{
			Questions: []Question{{Title: strings.ToUpper(what[:1]) + what[1:] + "?", Key: "confirm", Choices: choices}},
			Defaults:  func(map[string]string) map[string]string { return map[string]string{"confirm": "no"} },
		})
		if err != nil {
			return err
		}
		switch a["confirm"] {
		case "keep":
		case "delete":
			data = true
		default:
			return ErrAborted
		}
	}
	// The host's timers of the install (the scheduled backup, the disk-pressure watch) would run
	// on against a stack that is gone: one this run cannot remove refuses it, before anything goes.
	o := in.O
	if o.Host == nil {
		o.Host = HostTool{}
	}
	h := &Hostd{R: in.R, O: o, Host: o.Host, Docker: true, Systemd: cmp.Or(o.Systemd, systemdDir)}
	if !h.systemdHost() {
		for _, kind := range []string{"backup", "disk"} {
			if timer := filepath.Join(h.Systemd, installUnit(in.R.Spec.Name, kind, dir)+".timer"); fileExists(timer) {
				return fmt.Errorf("%s is the install's, and removing it takes root: run `sudo %s uninstall --dir %s`", timer, in.ctl(), dir)
			}
		}
	}
	args := []string{"down", "--remove-orphans"}
	if data {
		args = append(args, "--volumes")
	}
	if _, err := in.Compose(ctx, env, in.O.Log, args...); err != nil {
		return err
	}
	for _, kind := range []string{"backup", "disk"} {
		if err := h.dockerUnits(ctx, kind, "", ""); err != nil {
			return fmt.Errorf("the stack is down, but its %s timer: %w", kind, err)
		}
	}
	if data {
		unlock()
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("the stack is down and its volumes deleted, but %s: %w", dir, err)
		}
		fmt.Fprintf(in.O.Out, "%s in %s is uninstalled, its data deleted\n", name, dir)
		return nil
	}
	fmt.Fprintf(in.O.Out, "%s in %s is uninstalled\nkept: its volumes and %s (its secrets): `%s --dir %s` installs it again on them, `%s uninstall --data --dir %s` deletes them\n",
		name, dir, dir, in.ctl(), dir, in.ctl(), dir)
	return nil
}
