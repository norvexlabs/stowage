package install

import (
	"context"
	"maps"
	"os"
	"slices"
	"time"
)

// serviceEvery is how often, in Hostd.Unit, the host daemon looks at the stack's services.
const serviceEvery = 60

// serviceWatch looks at the stack's services every serviceEvery once the health gate judged the
// boot (the gate judges the services before), and journals a service.down event when a service
// becomes down: one of its containers is down (downOnes) at this look and none was at the last
// whole one. A stretch down is one event, a crash loop's included, while the looks are whole: a
// look that is not — the daemon's restart, a job of the daemon, a reboot or power-off it
// scheduled draining, the install's lifecycle lock held or taken while the look reads (an install,
// upgrade, rollback, provision, reset-db or a service verb; the boot unit bringing the stack up
// again takes it too), an install or compose ps that does not read, a look past lookWait — forgets
// it, so a service still down after it is journaled again. docker inspect reads only the
// services seen down. Skipped before anything is installed; the reads that fail, or
// outlive lookWait, are logged once a stretch; an event the journal refuses is journaled at the
// next look.
func (h *Hostd) serviceWatch(ctx context.Context) {
	w := &serviceLooks{}
	for {
		h.lookAtServices(ctx, w)
		t := time.NewTimer(time.Duration(serviceEvery) * h.unit())
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// serviceLooks is what the last look saw: the services down (each its first container down), and
// whether it could read them.
type serviceLooks struct {
	down  map[string]Service
	blind bool
}

// lookWait bounds each of a look's reads, in Hostd.Unit (the gate's state; compose ps and docker
// inspect): an engine or a systemd that does not answer stalls no watch.
const lookWait = 30

func (h *Hostd) lookAtServices(ctx context.Context, w *serviceLooks) {
	in, _ := h.run()
	if notInstalled(in) {
		return
	}
	// A look that is not whole forgets what the last one saw: across it the services may have
	// been stopped and started (a job, a run under the lock, a reboot's drain, the boot unit), so
	// a service down after it is journaled again.
	gate, cancel := context.WithTimeout(ctx, time.Duration(lookWait)*h.unit())
	err := h.judged(gate)
	cancel()
	if err != nil {
		w.down = nil
		if ctx.Err() == nil {
			w.skipping(h, err)
		}
		return
	}
	before := h.busy(in)
	if before.busy {
		w.down = nil
		return
	}
	look, cancel := context.WithTimeout(ctx, time.Duration(lookWait)*h.unit())
	defer cancel()
	env, err := in.open()
	var downs []Service
	if err == nil {
		var list []Service
		if list, err = in.stackList(look, env, h.declared(in, env)); err == nil {
			// What docker inspect adds is told of the services down alone.
			downs = downOnes(list)
			in.serviceFacts(look, downs)
		}
	}
	if ctx.Err() != nil {
		return // the daemon stops: no word of a look it cut
	}
	// A job claimed or ended, a reboot scheduled or cancelled, or the lifecycle lock taken (the
	// boot unit's run included), while the services were read cut the look as much as one still
	// under way: the daemon's generation or the lock's stamp moved.
	if err != nil || !before.same(h.busy(in)) {
		w.down = nil
		if err != nil {
			w.skipping(h, err)
		}
		return
	}
	w.skipping(h, nil)
	down := map[string]Service{}
	for _, s := range downs {
		if _, seen := down[s.Service]; !seen {
			down[s.Service] = s
		}
	}
	var evs []Event
	for _, name := range slices.Sorted(maps.Keys(down)) {
		if _, was := w.down[name]; !was {
			s := down[name]
			evs = append(evs, Event{Kind: evServiceDown, Target: name, State: s.State, Reason: s.Why()})
		}
	}
	if len(evs) > 0 {
		if _, err := h.event(evs...); err != nil {
			// Not journaled (the journal logs why): the next look journals them again.
			for _, e := range evs {
				delete(down, e.Target)
			}
		} else {
			for _, e := range evs {
				h.O.logf("%s %s: %s", e.Kind, e.Target, e.Reason)
			}
		}
	}
	w.down = down
}

// busy reads the lookState: whether the lifecycle lock is held is read from the kernel's table,
// never by taking it (a run taking it at that instant would find it held).
func (h *Hostd) busy(in *Installer) lookState {
	var st lookState
	h.mu.Lock()
	st.gen, st.busy = h.slotGen+h.rebootGen<<32, h.rebooting || h.job != nil && h.job.State == "running"
	h.mu.Unlock()
	path := in.path(lifecycleLock)
	st.lock = lockStamp(path)
	st.busy = st.busy || lockHeld(path)
	return st
}

// lookState is what a look reads before and after it lists the services (busy): the daemon's
// generation, which moves at every claim of its job slot, every job's end and every reboot or
// power-off scheduled or cancelled; whether a job runs, a reboot drains or the lifecycle lock is
// held; and the lock file's stamp, which moves at every take of the lock.
type lookState struct {
	gen  uint64
	busy bool
	lock os.FileInfo
}

// same reports whether nothing that cuts a look happened between two reads of it.
func (a lookState) same(b lookState) bool {
	return a.gen == b.gen && a.busy == b.busy && sameLock(a.lock, b.lock)
}

// skipping logs, once a stretch, that the watch cannot look (err), and that it looks again (nil).
func (w *serviceLooks) skipping(h *Hostd, err error) {
	switch {
	case err != nil && !w.blind:
		h.O.logf("the service watch skips its looks: %v", err)
		w.blind = true
	case err == nil && w.blind:
		h.O.logf("the service watch looks again")
		w.blind = false
	}
}
