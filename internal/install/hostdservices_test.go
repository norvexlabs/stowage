package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// watched is a VM install whose stack stackOf answers, its daemon, and the service.down events it
// journaled, each "<service>: <reason>".
func watched(t *testing.T) (*env, *stackOf, *Hostd, func() []string) {
	t.Helper()
	e := vmEnv(t, "1.0.0")
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	st := &stackOf{}
	e.docker.answer = st.answer
	h := daemon(e, &fakeHost{})
	return e, st, h, func() (out []string) {
		t.Helper()
		a, err := h.journal().since(0, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range a.Events {
			if ev.Kind == evServiceDown {
				out = append(out, ev.Target+": "+ev.Reason)
			}
		}
		return out
	}
}

// A service is journaled down when it becomes down, once a stretch (a crash loop's too), and again
// once it was up between; an operator's stop that exits non-zero is one too; a one-shot exited 0
// is none.
func TestServiceWatch(t *testing.T) {
	t.Parallel()
	_, st, h, downs := watched(t)
	ctx := context.Background()
	w := &serviceLooks{}
	const (
		up      = `{"ID":"c1","Service":"api","State":"running"}` + "\n" + `{"ID":"c9","Service":"seed","State":"exited","ExitCode":0}` + "\n"
		oom     = `{"ID":"c1","Service":"api","State":"exited","ExitCode":137}` + "\n"
		looping = `{"ID":"c1","Service":"api","State":"restarting","ExitCode":1}` + "\n"
		crashed = `{"ID":"c1","Service":"api","State":"exited","ExitCode":1}` + "\n"
		stopped = `{"ID":"c1","Service":"api","State":"exited","ExitCode":143}` + "\n"
	)
	st.set(up, "")
	h.lookAtServices(ctx, w)
	st.set(oom, "c1ffff true 100663296 0\n")
	h.lookAtServices(ctx, w)
	h.lookAtServices(ctx, w)
	if got := downs(); !slices.Equal(got, []string{"api: exited 137 (SIGKILL), OOM-killed (memory limit 96 MiB)"}) {
		t.Fatalf("down, then still down: %q", got)
	}
	// A crash loop between its states is one stretch.
	for _, ps := range []string{looping, crashed, looping} {
		st.set(ps, "")
		h.lookAtServices(ctx, w)
	}
	if got := downs(); len(got) != 1 {
		t.Fatalf("a crash loop: %q", got)
	}
	st.set(up, "")
	h.lookAtServices(ctx, w)
	st.set(stopped, "")
	h.lookAtServices(ctx, w)
	if got := downs(); len(got) != 2 || got[1] != "api: exited 143 (SIGTERM)" {
		t.Errorf("up, then stopped: %q", got)
	}
}

// Nothing is looked at before anything is installed, under the lifecycle lock, during a job of
// the daemon, or before the health gate judged the boot: a service down meanwhile is journaled at
// the next look.
func TestServiceWatchSkips(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	st := &stackOf{}
	e.docker.answer = st.answer
	host := &fakeHost{}
	h := daemon(e, host)
	ctx := context.Background()
	w := &serviceLooks{}
	h.lookAtServices(ctx, w)
	if st.looks != 0 || strings.Contains(e.log.String(), "service watch") {
		t.Fatalf("looked before an install: %d\n%s", st.looks, e.log)
	}
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	st.set(`{"ID":"c1","Service":"api","State":"running"}`+"\n", "")
	h.lookAtServices(ctx, w)
	st.set(`{"ID":"c1","Service":"api","State":"exited","ExitCode":1}`+"\n", "")
	before := st.looks
	unlock, err := lockFile(e.installer(vm).path(lifecycleLock))
	if err != nil {
		t.Fatal(err)
	}
	h.lookAtServices(ctx, w)
	unlock()
	host.judge = "activating"
	h.lookAtServices(ctx, w)
	host.judge = ""
	h.mu.Lock()
	h.job = &job{State: "running"}
	h.mu.Unlock()
	h.lookAtServices(ctx, w)
	h.mu.Lock()
	h.job = nil
	h.mu.Unlock()
	if st.looks != before {
		t.Errorf("looked %d times under the lock, before the gate or during a job", st.looks-before)
	}
	h.lookAtServices(ctx, w)
	a, _ := h.journal().since(0, 100)
	if !slices.ContainsFunc(a.Events, func(ev Event) bool { return ev.Kind == evServiceDown && ev.Target == "api" }) {
		t.Error("the look after: no service.down")
	}
}

// A look skipped for a job or the lock forgets what the last one saw: a service still down after
// the run is journaled again (a restart under the lock that crashed again is not missed).
func TestServiceWatchAfterARunAnnouncesAgain(t *testing.T) {
	t.Parallel()
	_, st, h, downs := watched(t)
	ctx := context.Background()
	w := &serviceLooks{}
	st.set(`{"ID":"c1","Service":"api","State":"exited","ExitCode":1}`+"\n", "")
	h.lookAtServices(ctx, w)
	h.mu.Lock()
	h.job = &job{State: "running"}
	h.mu.Unlock()
	h.lookAtServices(ctx, w)
	h.mu.Lock()
	h.job = nil
	h.mu.Unlock()
	h.lookAtServices(ctx, w)
	if got := downs(); len(got) != 2 {
		t.Errorf("after a job: %q", got)
	}
	// A job starting while a look reads the services: that look journals nothing, and forgets
	// what the last one saw (api is journaled again after the job).
	st.during = func() {
		h.mu.Lock()
		h.job = &job{State: "running"}
		h.mu.Unlock()
	}
	h.lookAtServices(ctx, w)
	if got := downs(); len(got) != 2 {
		t.Errorf("a look a job cut journaled: %q", got)
	}
	st.during = nil
	h.mu.Lock()
	h.job = nil
	h.mu.Unlock()
	h.lookAtServices(ctx, w)
	if got := downs(); len(got) != 3 {
		t.Errorf("after the job that cut a look: %q", got)
	}
	// A reboot the daemon scheduled drains: no look, and the services' stop is no service down.
	h.mu.Lock()
	h.rebooting = true
	h.mu.Unlock()
	before := st.looks
	h.lookAtServices(ctx, w)
	if got := downs(); len(got) != 3 || st.looks != before {
		t.Errorf("during a reboot's drain: %q, %d looks", got, st.looks-before)
	}
	h.mu.Lock()
	h.rebooting = false
	h.mu.Unlock()
	h.lookAtServices(ctx, w)
	if got := downs(); len(got) != 4 || got[3] != "api: exited 1" {
		t.Errorf("api, still down after a cancelled reboot: %q", got)
	}
	// A reboot scheduled and cancelled while the services were read: that look journals nothing.
	st.during = func() {
		h.mu.Lock()
		h.rebootGen += 2
		h.mu.Unlock()
	}
	h.lookAtServices(ctx, w)
	st.during = nil
	if got := downs(); len(got) != 4 {
		t.Errorf("a look a reboot cut journaled: %q", got)
	}
	h.lookAtServices(ctx, w)
	if got := downs(); len(got) != 5 {
		t.Fatalf("after the reboot that cut a look: %q", got)
	}
	// A job claimed and ended while the services were read (the generation moved): that look
	// journals nothing, and forgets.
	st.during = func() {
		h.mu.Lock()
		h.slotGen += 2
		h.mu.Unlock()
	}
	h.lookAtServices(ctx, w)
	st.during = nil
	if got := downs(); len(got) != 5 {
		t.Errorf("a look a job cut journaled: %q", got)
	}
	h.lookAtServices(ctx, w)
	if got := downs(); len(got) != 6 {
		t.Errorf("after the job that cut a look: %q", got)
	}
}

// A look skipped while the boot unit brings the stack up again forgets what the last one saw.
func TestServiceWatchForgetsAcrossTheBootUnit(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	st := &stackOf{}
	e.docker.answer = st.answer
	host := &fakeHost{}
	h := daemon(e, host)
	ctx := context.Background()
	w := &serviceLooks{}
	st.set(`{"ID":"c1","Service":"api","State":"exited","ExitCode":1}`+"\n", "")
	h.lookAtServices(ctx, w)
	host.boot = "activating"
	h.lookAtServices(ctx, w)
	host.boot = ""
	h.lookAtServices(ctx, w)
	a, _ := h.journal().since(0, 100)
	n := 0
	for _, ev := range a.Events {
		if ev.Kind == evServiceDown {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d service.down, want 2 (again after the boot unit's run)", n)
	}
}

// A gate that refuses (the boot not judged yet, its unit stopped) is logged once a stretch, its
// refusal naming why; the look after it says it looks again.
func TestServiceWatchLogsTheGatesRefusal(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	st := &stackOf{}
	e.docker.answer = st.answer
	host := &fakeHost{judge: "inactive"}
	h := daemon(e, host)
	ctx := context.Background()
	w := &serviceLooks{}
	h.lookAtServices(ctx, w)
	h.lookAtServices(ctx, w)
	host.judge = ""
	h.lookAtServices(ctx, w)
	log := e.log.String()
	if strings.Count(log, "the service watch skips its looks: ") != 1 || strings.Count(log, "the service watch looks again") != 1 {
		t.Errorf("the watch's log:\n%s", log)
	}
}

// "looks again" is said of a whole look alone: one a job cuts says nothing; a look's events are
// logged once, once journaled.
func TestServiceWatchLogsWholeLooks(t *testing.T) {
	t.Parallel()
	e, st, h, _ := watched(t)
	ctx := context.Background()
	w := &serviceLooks{}
	st.psErr = errors.New("engine restarting")
	h.lookAtServices(ctx, w)
	st.psErr = nil
	st.during = func() {
		h.mu.Lock()
		h.job = &job{State: "running"}
		h.mu.Unlock()
	}
	h.lookAtServices(ctx, w)
	if strings.Contains(e.log.String(), "looks again") {
		t.Errorf("a look a job cut says it looks again:\n%s", e.log)
	}
	st.during = nil
	h.mu.Lock()
	h.job = nil
	h.mu.Unlock()
	// A refused event, then journaled: its line is logged once.
	journal := filepath.Join(e.installer(vm).O.Dir, EventsFile)
	raw, _ := os.ReadFile(journal)
	if err := os.Remove(journal); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	st.set(`{"ID":"c1","Service":"api","State":"exited","ExitCode":1}`+"\n", "")
	h.lookAtServices(ctx, w)
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	h.lookAtServices(ctx, w)
	if n := strings.Count(e.log.String(), "service.down api: exited 1"); n != 1 {
		t.Errorf("the event logged %d times:\n%s", n, e.log)
	}
}

// An event the journal refuses is journaled at the next look.
func TestServiceWatchRetriesARefusedEvent(t *testing.T) {
	t.Parallel()
	e, st, h, downs := watched(t)
	ctx := context.Background()
	w := &serviceLooks{}
	journal := filepath.Join(e.installer(vm).O.Dir, EventsFile)
	raw, _ := os.ReadFile(journal)
	if err := os.Remove(journal); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(journal, 0o700); err != nil { // an append fails, root or not
		t.Fatal(err)
	}
	st.set(`{"ID":"c1","Service":"api","State":"exited","ExitCode":1}`+"\n", "")
	h.lookAtServices(ctx, w)
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	h.lookAtServices(ctx, w)
	if got := downs(); len(got) != 1 {
		t.Fatalf("not journaled after a refusal: %q", got)
	}
}

// A run taking the lifecycle lock while a look reads the services: that look journals nothing,
// and the next one does.
func TestServiceWatchDropsALookTheLockCut(t *testing.T) {
	t.Parallel()
	e, st, h, downs := watched(t)
	ctx := context.Background()
	var unlock func()
	st.set(`{"ID":"c1","Service":"api","State":"exited","ExitCode":1}`+"\n", "")
	st.during = func() {
		var err error
		if unlock, err = lockFile(e.installer(vm).path(lifecycleLock)); err != nil {
			t.Error(err)
		}
	}
	w := &serviceLooks{}
	h.lookAtServices(ctx, w)
	unlock()
	if got := downs(); len(got) != 0 {
		t.Fatalf("journaled on a look the lock cut: %q", got)
	}
	st.during = nil
	h.lookAtServices(ctx, w)
	if got := downs(); len(got) != 1 {
		t.Errorf("the look after: %q", got)
	}
}

// A compose ps that fails is logged once a stretch, and the look after it says it looks again; a
// look the daemon's stop cuts says nothing.
func TestServiceWatchLogsALookItCannotMake(t *testing.T) {
	t.Parallel()
	e, st, h, _ := watched(t)
	ctx := context.Background()
	w := &serviceLooks{}
	st.psErr = errors.New("engine restarting")
	h.lookAtServices(ctx, w)
	h.lookAtServices(ctx, w)
	st.psErr = nil
	h.lookAtServices(ctx, w)
	h.lookAtServices(ctx, w)
	log := e.log.String()
	if strings.Count(log, "the service watch skips its looks: compose ps: engine restarting") != 1 || strings.Count(log, "the service watch looks again") != 1 {
		t.Errorf("the watch's log:\n%s", log)
	}
	cut, cancel := context.WithCancel(ctx)
	st.during = cancel
	st.psErr = context.Canceled
	e.log.Reset()
	h.lookAtServices(cut, w)
	if strings.Contains(e.log.String(), "service watch") {
		t.Errorf("a look the stop cut logged:\n%s", e.log)
	}
}

// The lock's holder is read from the kernel's table, never by taking it.
func TestLockHeldPassive(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "lock")
	if lockHeldPassive(p) {
		t.Error("held without a file")
	}
	unlock, err := lockFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !lockHeldPassive(p) {
		t.Error("not held, while it is")
	}
	unlock()
	if lockHeldPassive(p) {
		t.Error("held once released")
	}
}
