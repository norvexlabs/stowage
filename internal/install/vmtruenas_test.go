package install

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/truenas"
)

// vm truenas is documented: its three steps, shown; each refuses an environment missing what it
// takes before it dials TrueNAS, naming the variable.
func TestTheTrueNASStepsCheckTheirEnvironment(t *testing.T) {
	t.Parallel()
	full := map[string]string{"IMAGE": "/d/notes.qcow2", "IMAGE_SHA256": strings.Repeat("a", 64), "STAGING_DIR": "/mnt/tank/s", "ZVOL": "tank/vms/x",
		"SEED": "/d/seed.iso", "SEED_PATH": "/mnt/tank/s/seed.iso", "VM_ID": "7"}
	run := func(env map[string]string, args ...string) error {
		c := truenasCommand(func(k string) string { return env[k] })
		c.SetArgs(args)
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		return c.Execute()
	}
	c := truenasCommand(func(string) string { return "" })
	var steps []string
	for _, s := range c.Commands() {
		steps = append(steps, s.Name())
		if s.Hidden {
			t.Errorf("%s is hidden", s.Name())
		}
	}
	if c.Hidden || !slices.Equal(steps, []string{"import-image", "seed", "stop"}) {
		t.Errorf("steps %v", steps)
	}
	for step, keys := range map[string][]string{"import-image": {"IMAGE", "IMAGE_SHA256", "STAGING_DIR", "ZVOL"}, "seed": {"SEED", "SEED_PATH"}, "stop": {"VM_ID"}} {
		for _, k := range keys {
			env := map[string]string{}
			for k, v := range full {
				env[k] = v
			}
			delete(env, k)
			if err := run(env, step); err == nil || err.Error() != k+" is not set" {
				t.Errorf("%s without %s: %v", step, k, err)
			}
		}
		// Its own environment given, the API's missing: named.
		if err := run(full, step); err == nil || !strings.Contains(err.Error(), "TRUENAS_ENDPOINT") || !strings.Contains(err.Error(), "TRUENAS_URL") {
			t.Errorf("%s without the API's address: %v", step, err)
		}
		if err := run(full, step, "--timeout", "0s"); err == nil || !strings.Contains(err.Error(), "--timeout 0s") {
			t.Errorf("%s --timeout 0s: %v", step, err)
		}
	}
	for _, id := range []string{"x", "0", "-3", "7.5"} {
		env := map[string]string{"VM_ID": id, "TRUENAS_URL": "https://nas.example.org", "TRUENAS_API_KEY": "k"}
		if err := run(env, "stop"); err == nil || !strings.Contains(err.Error(), "VM_ID \""+id+"\": the VM's id on TrueNAS") {
			t.Errorf("VM_ID %s: %v", id, err)
		}
	}
}

// A step --timeout cut off is told to run it again with a longer --timeout, which starts it over,
// a lock a failed release left included (its message naming its fix first); one whose convert may
// still run, or a job that FAILED on its own, is not, its own message naming the fix.
func TestTimedOutHintsARunAgainWhereItWorks(t *testing.T) {
	t.Parallel()
	hint := " (--timeout 1h0m0s ran out: the step starts over: run it again with a longer --timeout)"
	err := timedOut(errors.New("upload of x: context deadline exceeded"), time.Hour)
	if want := "upload of x: context deadline exceeded" + hint; err.Error() != want {
		t.Errorf("got %q, want %q", err, want)
	}
	// A lock a failed release left, beside a plain timeout: the lock's fix, then a run again.
	unlock := &truenas.LockLeftError{Err: errors.New("the lock z-stowage-ref not deleted (EOF): once no job of this import runs, delete the zvol z-stowage-ref …, then run this again")}
	timeout := errors.Join(errors.New("upload of x: context deadline exceeded"), unlock)
	if want := "upload of x: context deadline exceeded\nthe lock z-stowage-ref not deleted (EOF): once no job of this import runs, delete the zvol z-stowage-ref …, then run this again" + hint; timedOut(timeout, time.Hour).Error() != want {
		t.Errorf("got %q, want %q", timedOut(timeout, time.Hour), want)
	}
	// A convert that may still run, or one that FAILED: the timeout named, no run again.
	left := &truenas.LockLeftError{Err: fmt.Errorf("%w: the lock z-stowage-ref and the staged file f are left; once …, delete the zvol z-stowage-ref …, then run this again",
		&truenas.ConvertLeftError{Msg: "context deadline exceeded; the convert job 3 is still RUNNING on TrueNAS after 20s"})}
	failed := &truenas.FailedError{Err: errors.New("vm.device.convert of f: job 3 FAILED: no space left")}
	for _, e := range []error{left, fmt.Errorf("wrapped: %w", left), errors.Join(errors.New("FAILED"), left), failed, errors.Join(failed, unlock)} {
		want := e.Error() + " (--timeout 1h0m0s ran out)"
		if err := timedOut(e, time.Hour); err.Error() != want || !errors.Is(err, e) {
			t.Errorf("got %q, want %q", err, want)
		}
	}
}
