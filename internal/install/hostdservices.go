package install

import (
	"context"
	"maps"
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
// scheduled draining, the install's lifecycle lock held (an install, upgrade, rollback,
// provision, reset-db or a service verb), the boot unit bringing the stack up again, an install
// or compose ps that does not read, a look past lookWait — forgets it, so a service still down
// after it is journaled again. Skipped before anything is installed; the reads that fail, or
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
	gen, busy := h.busy(in)
	if busy {
		w.down = nil
		return
	}
	look, cancel := context.WithTimeout(ctx, time.Duration(lookWait)*h.unit())
	defer cancel()
	env, err := in.open()
	var list []Service
	if err == nil {
		list, err = in.stackServices(look, env)
	}
	if ctx.Err() != nil {
		return // the daemon stops: no word of a look it cut
	}
	// A job claimed or ended, or a reboot scheduled or cancelled, while the services were read
	// (the daemon's generation moved) cut it as much as one still running. A lock taken and
	// released between the two reads of it, or the boot unit run between them, goes unseen.
	after, busy := h.busy(in)
	if err != nil || busy || after != gen {
		w.down = nil
		if err != nil {
			w.skipping(h, err)
		}
		return
	}
	w.skipping(h, nil)
	down := map[string]Service{}
	for _, s := range downOnes(list) {
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

// busy reports whether a job of the daemon runs, a reboot or power-off it scheduled drains, or the
// install's lifecycle lock is held, the lock read from the kernel's table, never by taking it (a
// run taking it at that instant would find it held); and the daemon's generation, which moves at
// every claim of its job slot, every job's end and every reboot scheduled or cancelled.
func (h *Hostd) busy(in *Installer) (gen uint64, busy bool) {
	h.mu.Lock()
	gen, busy = h.slotGen+h.rebootGen<<32, h.rebooting || h.job != nil && h.job.State == "running"
	h.mu.Unlock()
	return gen, busy || lockHeldPassive(in.path(lifecycleLock))
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
