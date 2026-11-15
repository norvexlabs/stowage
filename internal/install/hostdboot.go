package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/norvexlabs/stowage/internal/providers"
)

// The host daemon's side of the boot: the health check, an upgrade's fresh trial, and going back
// to the passive system by hand.

// Health is what GET /v1/health answers: the health gate's check, once.
type Health struct {
	Ready  bool   `json:"ready"`
	Reason string `json:"reason,omitempty"` // why not
}

// healthReuseSecs is how long GET /v1/health's verdict answers again once its check ended: the
// `command` provider starts a container per check, and the app socket may poll it.
const healthReuseSecs = 5

// healthCheck is one check of GET /v1/health, every request arriving while it runs waiting for it:
// done closes once ans and at, when it ended, are set (under healthMu). gen is the daemon's
// stateGen as it began: a change since (a job, a service verb) makes its verdict stale.
type healthCheck struct {
	done chan struct{}
	ans  Health
	at   time.Time
	gen  uint64
}

// reusable reports whether c's verdict still answers at now: no state change since it began, and
// it is running, or ended within healthReuseSecs (healthMu held).
func (h *Hostd) reusable(c *healthCheck, now time.Time) bool {
	return c != nil && c.gen == h.stateGen.Load() && (c.at.IsZero() || !c.at.Before(now.Add(-healthReuseSecs*h.unit())))
}

// changed marks a change of what the install runs (a job's start or end, a service verb): a
// health verdict read before it no longer answers.
func (h *Hostd) changed() { h.stateGen.Add(1) }

// healthWait bounds, in Unit, one check of GET /v1/health: the `command` provider's own bounds
// (its image's user read, then its run: timeout_s each, 30 s by default) and a minute for the
// rest (the stack's state, the http probe's 10 s). A check that hangs ends at it, unhealthy, and
// the next request starts another.
func healthWait(r *Release) int {
	t := 30
	var c providers.RuntimeCommand
	if r.Spec.Health.Name == "command" && r.Spec.Health.Decode(&c) == nil && c.TimeoutS > 0 {
		t = c.TimeoutS
	}
	return 60 + 30 + 2*t
}

// RollbackPlan is what GET /v1/rollback and a dry run of POST /v1/rollback answer: the release
// the install runs, and the one the passive system carries, which going back runs.
type RollbackPlan struct {
	From Version `json:"from"`
	To   Version `json:"to"`
	// Checks, a dry run's alone: the lines of the install's own rollback dry run, which the job
	// logs.
	Checks []string `json:"checks,omitempty"`
}

// RollbackRequest is POST /v1/rollback's body.
type RollbackRequest struct {
	// Confirm, when set, is the release the operator confirmed going back to: another is refused.
	Confirm *Version `json:"confirm,omitempty"`
	// DryRun runs every check before the job and answers the RollbackPlan: no job, nothing
	// written; Confirm may be left out.
	DryRun bool `json:"dry_run,omitempty"`
}

// nonEmptyLines is s's non-empty lines.
func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
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
	return notNowErr{fmt.Errorf("an upgrade wrote (or began to write) %s, which no boot ran yet", strings.TrimSpace(string(raw))), "reboot first (the next boot runs the system written), then run again"}
}

func (h *Hostd) assessment() assessment {
	return assessmentIn(h.State, h.Host)
}

func (h *Hostd) health(w http.ResponseWriter, r *http.Request) {
	h.healthMu.Lock()
	c := h.healthRun
	if !h.reusable(c, time.Now()) {
		// A new check reads the install as it is now; a verdict reused reads nothing.
		in, _ := h.run()
		env, err := h.installed(in)
		if err != nil {
			h.healthMu.Unlock()
			replyErr(w, err)
			return
		}
		c = &healthCheck{done: make(chan struct{}), gen: h.stateGen.Load()}
		h.healthRun = c
		// The check is every waiting request's, on the daemon's context: none's going away cuts
		// it; its own bound does.
		go func() {
			ctx, cancel := context.WithTimeout(h.ctx, time.Duration(healthWait(in.R))*h.unit())
			defer cancel()
			ans := Health{Ready: true}
			if err := in.ready(ctx, env, h.Probe, nil); err != nil {
				ans = Health{Reason: err.Error()}
			}
			h.healthMu.Lock()
			c.ans, c.at = ans, time.Now()
			h.healthMu.Unlock()
			close(c.done)
		}()
	}
	h.healthMu.Unlock()
	select {
	case <-c.done:
	case <-r.Context().Done():
		return
	}
	h.healthMu.Lock()
	ans := c.ans
	h.healthMu.Unlock()
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
	ctx, cancel := context.WithTimeout(h.ctx, assessWrite*h.unit())
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
		return RollbackPlan{}, refuseAs(http.StatusConflict, "nothing to go back to: the VM runs the earlier system already", "the VM runs the passive system already: there is no earlier one to go back to")
	default:
		if mode == "" {
			return RollbackPlan{}, fail(http.StatusInternalServerError, "the system the VM runs is not known (no *_mode file in %s)", h.O.cos())
		}
		return RollbackPlan{}, refuseAs(http.StatusConflict, "boot the active system (GRUB's default entry), then run again", "the VM runs the %q system, not the active one: going back chooses the passive system from the active one", mode)
	}
	env, err := in.open()
	if err != nil {
		return RollbackPlan{}, err
	}
	if _, v, rev := runs(env); !isAt(m.ProjectVersion, m.Revision, v, rev) {
		return RollbackPlan{}, refuseAs(http.StatusConflict, "", "the install runs %s (revision %s), not %s, which this OS carries: going back to the passive system is for an install of the active system's release", v, rev, from)
	}
	if !fileExists(filepath.Join(h.assessment().dir, passiveImage)) {
		return RollbackPlan{}, refuseAs(http.StatusConflict, "nothing to go back to: an upgrade leaves the system it replaces", "there is no passive system to go back to")
	}
	// An upgrade written but not booted yet (--no-reboot) made the running system the passive one.
	if err := h.osWritten(); err != nil {
		return RollbackPlan{}, sameKind(err, fail(http.StatusConflict, "%v: the passive system may now be this one; reboot first", err))
	}
	st, err := in.readPrevState()
	if err != nil {
		return RollbackPlan{}, err
	}
	if st == nil {
		return RollbackPlan{}, refuseAs(http.StatusConflict, "nothing to go back to: an upgrade keeps the release it replaces", "%s has no previous release to go back to (an upgrade keeps one)", in.O.Dir)
	}
	in.O.DryRun, in.O.Log = true, log
	// Rollback runs as on the VM (O.Preloaded), so each refusal carries the fix it chose for the
	// VM where it was raised, or none where no advice holds there; failAs keeps it, answered 409.
	if err := in.Rollback(ctx); err != nil {
		return RollbackPlan{}, failAs(http.StatusConflict, err)
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
		replyErr(w, failAs(http.StatusConflict, err))
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
		replyErr(w, refuseAs(http.StatusConflict, "look again, then confirm the release the passive system carries", "the passive system carries %s, not %s, which was confirmed", p.To, *req.Confirm))
		return
	}
	// Again as the job claims the slot: a restore that just ended may have marked them.
	split := func() error {
		if err := in.volumesSplit(); err != nil {
			return failAs(http.StatusConflict, err)
		}
		return nil
	}
	if req.DryRun {
		if err := h.slotFree(split); err != nil {
			replyErr(w, err)
			return
		}
		p.Checks = nonEmptyLines(checks.String())
		reply(w, http.StatusOK, p)
		return
	}
	j, err := h.startJob("rollback", p.To.String(), func() error {
		if err := split(); err != nil {
			return err
		}
		// Counted as the job is claimed (under mu, after claimJob's stopping check; nothing
		// after this guard fails the claim): Serve's wait never misses it.
		h.rollbacks.Add(1)
		return nil
	})
	if err != nil {
		replyErr(w, err)
		return
	}
	go func() {
		defer h.rollbacks.Done()
		for _, l := range nonEmptyLines(checks.String()) {
			h.logf(j, "%s", l)
		}
		if err := h.toPassive(j, p); err != nil {
			h.finish(j, err)
		}
	}()
	accepted(w, j)
}

// assessWrite bounds, in Unit, a write of the boot assessment's marks (remount, grub2-editenv,
// remount): a write that hangs fails rather than hold the job.
const assessWrite = 60

// rebootGrace is the wait, in Unit, between a rollback's end and its marks and `systemctl reboot`.
const rebootGrace = 5

// rollbackDrain is the wait, in Unit, before a rollback reboots.
const rollbackDrain = 30

// toPassive reboots into the passive system after the drain. Once the checks pass again and the
// install's lock is taken, the job ends, done with its reboot rebootGrace out (endRebooting), so a
// client following the job (`rollback` run on the VM over SSH) reads its end and prints its Result
// before the shutdown stops its session. Only then are the boot assessment's marks written (as
// after a failed attempt), right before `systemctl reboot`: nothing is armed while the job or the
// grace waits, so a daemon stopping or dying meanwhile leaves the next boot as it was (a start in
// this boot journals the reboot as not come, recoverEvents). When the marks cannot be written
// whole, or the reboot fails, they are put back as they were — a later reboot must not go back
// unasked — and the job's log and a reboot.cancelled by the job tell it, saying whether putting
// them back worked; the job's end stands.
func (h *Hostd) toPassive(j *job, p RollbackPlan) error {
	h.logf(j, "rebooting into the passive system in %d s: the boot goes back to %s", rollbackDrain+rebootGrace, p.To)
	if h.onWait != nil {
		h.onWait("rollback drain")
	}
	select {
	case <-h.ctx.Done():
		return fmt.Errorf("nothing changed: the host daemon stopped: %w", h.ctx.Err())
	case <-time.After(time.Duration(rollbackDrain) * h.unit()):
	}
	// The plan again, as a run during the drain may have changed what it found.
	if again, err := h.plan(h.ctx, io.Discard); err != nil {
		return fmt.Errorf("nothing changed: %w", err)
	} else if again.From != p.From || again.To != p.To {
		return fmt.Errorf("nothing changed: the passive system now carries %s, the install runs %s", again.To, again.From)
	}
	// Never during an operator's run: the install's lock is held to the reboot, kept by the
	// daemon so it is not released before.
	in, _ := h.run()
	unlock, err := in.lockNow()
	if errors.Is(err, errLocked) {
		return errors.New("nothing changed: the install is busy (an install, upgrade, rollback, provision or service action); run rollback again once it is done")
	}
	if err != nil {
		return err
	}
	// The marks as they are, read before the job ends: one that cannot be read fails the job.
	a := h.assessment()
	was, err := a.read()
	if err != nil {
		unlock()
		return fmt.Errorf("nothing changed: the boot assessment: %w", err)
	}
	if err := h.claimReboot(false, unlock); err != nil {
		unlock()
		return fmt.Errorf("nothing changed: %w", err)
	}
	h.endRebooting(j, "reboot", rebootGrace)
	// cancelled ends the reboot that does not come: the lock released, the reboot unclaimed, the
	// job's log and the journal told why.
	cancelled := func(reason string) error {
		unlock()
		h.unclaimReboot()
		h.O.logf("the rollback's reboot: %s", reason)
		h.logf(j, "%s", reason)
		h.rebootCancelled("reboot", reason, j)
		return nil
	}
	if h.onWait != nil {
		h.onWait("reboot grace")
	}
	select {
	case <-h.ctx.Done():
	case <-time.After(time.Duration(rebootGrace) * h.unit()):
	}
	if h.ctx.Err() != nil { // a stop, even one as the grace ends, arms nothing
		return cancelled("the daemon stopped before the reboot: nothing changed")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), assessWrite*h.unit())
	defer cancel()
	// back answers reason with the marks put back as they were, and whether that worked.
	back := func(reason string) string {
		// A context of its own: the one that ran out may be why it failed.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(h.ctx), assessWrite*h.unit())
		defer rcancel()
		switch rerr := a.set(rctx, assessEnable+"="+was[assessEnable], assessTentative+"="+was[assessTentative]); {
		case errors.As(rerr, new(leftWritable)):
			return fmt.Sprintf("%s: the boot assessment is put back as it was, its partition left read-write (%v)", reason, rerr)
		case rerr != nil:
			return fmt.Sprintf("%s; putting the boot assessment back failed too, so the next boot may choose the passive system (%s): %v", reason, a.file(), rerr)
		}
		return reason + ": the boot assessment is put back as it was, nothing changed"
	}
	switch err := a.set(ctx, assessEnable+"=yes", assessTentative+"=yes"); {
	case errors.As(err, new(leftWritable)): // the marks are written: the reboot needs no more
		h.logf(j, "the boot assessment is marked, its partition left read-write (%v)", err)
	case err != nil:
		return cancelled(back(fmt.Sprintf("marking the boot assessment failed: %v", err)))
	}
	// A stop that came while the marks were written: the reboot it would override is not run.
	if h.ctx.Err() != nil {
		return cancelled(back("the daemon stopped before the reboot"))
	}
	// Stopped as the system goes down, the reboot comes: the marks stay for it.
	if down, err := powerHost(ctx, h.Host, "reboot"); err != nil && !down {
		return cancelled(back(fmt.Sprintf("systemctl reboot failed: %v", err)))
	}
	return nil
}
