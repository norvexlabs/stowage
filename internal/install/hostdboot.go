package install

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The host daemon's side of the boot: the health check, an upgrade's fresh trial, and going back
// to the passive system by hand.

// Health is what GET /v1/health answers: the health gate's check, once.
type Health struct {
	Ready  bool   `json:"ready"`
	Reason string `json:"reason,omitempty"` // why not
}

// RollbackPlan is what GET /v1/rollback answers: the release the install runs, and the one the
// passive system carries, which going back runs.
type RollbackPlan struct {
	From Version `json:"from"`
	To   Version `json:"to"`
}

// RollbackRequest is POST /v1/rollback's body.
type RollbackRequest struct {
	// Confirm, when set, is the release the operator confirmed going back to: another is refused.
	Confirm *Version `json:"confirm,omitempty"`
}

// osWritten, in the daemon's runtime directory (which a reboot empties), marks an OS an upgrade
// wrote that no boot ran yet.
const osWritten = "os-written"

// osWritten refuses while an upgrade began to write a system no boot ran yet: written, or
// failed after kairos-agent may have moved the running system to the passive slot. A mark that
// cannot be read refuses too.
func (h *Hostd) osWritten() error {
	raw, err := os.ReadFile(filepath.Join(h.Run, osWritten))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("whether an upgrade wrote a new system is not known: %w", err)
	}
	return fmt.Errorf("an upgrade wrote (or began to write) %s, which no boot ran yet", strings.TrimSpace(string(raw)))
}

func (h *Hostd) assessment() assessment {
	return assessment{cmp.Or(h.State, cosState), h.Host}
}

func (h *Hostd) health(w http.ResponseWriter, r *http.Request) {
	in, _ := h.run()
	env, err := h.installed(in)
	if err != nil {
		replyErr(w, err)
		return
	}
	ans := Health{Ready: true}
	if err := in.ready(r.Context(), env, h.Probe); err != nil {
		ans = Health{Reason: err.Error()}
	}
	reply(w, http.StatusOK, ans)
}

// retry gives the OS an upgrade wrote a boot on trial of its own: after a boot that fell back (or
// a rollback), the assessment still marks the attempt, and GRUB would skip the new system at once.
func (h *Hostd) retry(j *job) error {
	a := h.assessment()
	vars, err := a.read()
	if err != nil {
		return err
	}
	if vars[assessTentative] == "" {
		return nil
	}
	h.logf(j, "clearing the boot assessment's mark of the last attempt, so the next boot tries the new system")
	ctx, cancel := context.WithTimeout(h.ctx, time.Minute)
	defer cancel()
	if err := a.set(ctx, assessTentative+"="); err != nil {
		return fmt.Errorf("the new system is written, but the next boot would skip it: %w", err)
	}
	return nil
}

// plan checks that going back to the passive system can run, the rollback's own checks run dry
// with their lines written to log: the VM runs the active system, the install the release this
// OS carries, a previous release is kept, and the database lets that release read it.
func (h *Hostd) plan(ctx context.Context, log io.Writer) (RollbackPlan, error) {
	in, _ := h.run()
	if _, err := h.installed(in); err != nil {
		return RollbackPlan{}, err
	}
	m := h.R.Manifest
	from := Version{m.ProjectVersion, m.Revision}
	switch mode := bootMode(h.O.cos()); mode {
	case "active":
	case "passive":
		return RollbackPlan{}, fail(http.StatusConflict, "the VM runs the passive system already: there is no earlier one to go back to")
	default:
		return RollbackPlan{}, fail(http.StatusConflict, "the VM runs the %q system, not the active one: going back chooses the passive system from the active one", mode)
	}
	env, err := in.open()
	if err != nil {
		return RollbackPlan{}, err
	}
	if _, v, rev := runs(env); v != m.ProjectVersion || rev != strconv.Itoa(m.Revision) {
		return RollbackPlan{}, fail(http.StatusConflict, "the install runs %s (revision %s), not %s, which this OS carries: going back to the passive system is for an install of the active system's release", v, rev, from)
	}
	if !fileExists(filepath.Join(h.assessment().dir, passiveImage)) {
		return RollbackPlan{}, fail(http.StatusConflict, "there is no passive system to go back to")
	}
	// An upgrade written but not booted yet (--no-reboot) made the running system the passive one.
	if err := h.osWritten(); err != nil {
		return RollbackPlan{}, fail(http.StatusConflict, "%v: the passive system may now be this one; reboot first", err)
	}
	raw, err := os.ReadFile(filepath.Join(in.path(PrevDir), prevState))
	if errors.Is(err, os.ErrNotExist) {
		return RollbackPlan{}, fail(http.StatusConflict, "%s has no previous release to go back to (an upgrade keeps one)", in.O.Dir)
	}
	if err != nil {
		return RollbackPlan{}, err
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return RollbackPlan{}, fmt.Errorf("%s: %w", filepath.Join(in.path(PrevDir), prevState), err)
	}
	in.O.DryRun, in.O.Log = true, log
	if err := in.Rollback(ctx); err != nil {
		return RollbackPlan{}, fail(http.StatusConflict, "%v", err)
	}
	return RollbackPlan{From: from, To: Version{st.ProjectVersion, st.Revision}}, nil
}

func (h *Hostd) rollbackPlan(w http.ResponseWriter, r *http.Request) {
	p, err := h.plan(r.Context(), io.Discard)
	if err != nil {
		replyErr(w, err)
		return
	}
	reply(w, http.StatusOK, p)
}

// rollback goes back to the passive system: once the checks pass (plan), the boot assessment is
// marked as after a failed boot, so GRUB boots the fallback entry from then on, and the VM
// reboots; that boot takes the install back to the passive system's release.
func (h *Hostd) rollback(w http.ResponseWriter, r *http.Request) {
	var req RollbackRequest
	if err := decodeBody(r, &req); err != nil {
		replyErr(w, err)
		return
	}
	// A restore again finishes half restored volumes, onto this release only.
	in, _ := h.run()
	if err := in.volumesSplit(); err != nil {
		replyErr(w, fail(http.StatusConflict, "%v", err))
		return
	}
	if err := h.judged(r.Context()); err != nil {
		replyErr(w, err)
		return
	}
	var checks bytes.Buffer
	p, err := h.plan(r.Context(), &checks)
	if err != nil {
		replyErr(w, err)
		return
	}
	if req.Confirm != nil && *req.Confirm != p.To {
		replyErr(w, fail(http.StatusConflict, "the passive system carries %s, not %s, which was confirmed", p.To, *req.Confirm))
		return
	}
	// Again as the job claims the slot: a restore that just ended may have marked them.
	j, err := h.startJob("rollback", p.To.String(), func() error {
		if err := in.volumesSplit(); err != nil {
			return fail(http.StatusConflict, "%v", err)
		}
		return nil
	})
	if err != nil {
		replyErr(w, err)
		return
	}
	go func() {
		for _, l := range strings.Split(strings.TrimSpace(checks.String()), "\n") {
			if l != "" {
				h.logf(j, "%s", l)
			}
		}
		if err := h.toPassive(j, p); err != nil {
			h.finish(j, err)
		}
	}()
	accepted(w, j)
}

// rollbackDrain is the wait, in Unit, before a rollback reboots.
const rollbackDrain = 30

// toPassive reboots into the passive system after the drain: the boot assessment is marked as
// after a failed attempt right before `systemctl reboot`, so nothing is armed while the job waits.
// When the marks cannot be written whole, they are put back as they were — a later reboot must
// not go back unasked — and the error fails the job. Once they are written, the job ends, done
// with its reboot (endRebooting), before `systemctl reboot` runs, and toPassive answers nil: when
// the reboot then fails, the marks are put back, and the job's log and a reboot.cancelled by the
// job tell it, saying whether putting them back worked; the job's end stands.
func (h *Hostd) toPassive(j *job, p RollbackPlan) error {
	h.logf(j, "rebooting into the passive system in %d s: the boot goes back to %s", rollbackDrain, p.To)
	select {
	case <-h.ctx.Done():
		return h.ctx.Err()
	case <-time.After(time.Duration(rollbackDrain) * cmp.Or(h.Unit, time.Second)):
	}
	// The plan again, as a run during the drain may have changed what it found.
	if again, err := h.plan(h.ctx, io.Discard); err != nil {
		return fmt.Errorf("nothing changed: %w", err)
	} else if again != p {
		return fmt.Errorf("nothing changed: the passive system now carries %s, the install runs %s", again.To, again.From)
	}
	// Never during an operator's run: the install's lock is held to the reboot, kept by the
	// daemon so it is not released before.
	in, _ := h.run()
	unlock, err := lockFile(in.path(lifecycleLock))
	if errors.Is(err, errLocked) {
		return errors.New("nothing changed: the install is busy (an install, upgrade, rollback, provision or service action); run rollback again once it is done")
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), time.Minute)
	defer cancel()
	a := h.assessment()
	was, err := a.read()
	if err != nil {
		unlock()
		return err
	}
	back := func(err error) error {
		defer unlock()
		// A context of its own: the one that ran out may be why it failed.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(h.ctx), time.Minute)
		defer rcancel()
		if rerr := a.set(rctx, assessEnable+"="+was[assessEnable], assessTentative+"="+was[assessTentative]); rerr != nil {
			return fmt.Errorf("%w; putting the boot assessment back failed too, so the next boot may choose the passive system (%s): %v", err, a.file(), rerr)
		}
		return err
	}
	if err := a.set(ctx, assessEnable+"=yes", assessTentative+"=yes"); err != nil {
		return back(err)
	}
	h.mu.Lock()
	h.rebooting, h.held = true, unlock
	h.mu.Unlock()
	h.endRebooting(j, "reboot", 0)
	if _, err := h.Host.Run(ctx, Cmd{Args: []string{"systemctl", "reboot"}}); err != nil {
		reason := fmt.Sprintf("systemctl reboot failed: %v", err)
		h.mu.Lock()
		h.rebooting, h.held = false, nil
		h.mu.Unlock()
		failed := errors.New(reason)
		if err := back(failed); err != failed { // putting the marks back failed too
			h.O.logf("the rollback's reboot: %v", err)
			reason = err.Error()
		} else {
			reason += ": the boot assessment is put back as it was, nothing changed"
		}
		h.logf(j, "%s", reason)
		h.rebootCancelled("reboot", reason, j)
	}
	return nil
}
