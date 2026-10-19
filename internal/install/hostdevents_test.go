package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/spec"
)

// held reads the journal in dir, failing the test when it does not read.
func held(t *testing.T, dir string) []Event {
	t.Helper()
	evs, err := (&journal{dir: dir}).read()
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// kinds is the kinds of evs, in order.
func kinds(evs []Event) string {
	var k []string
	for _, e := range evs {
		k = append(k, e.Kind)
	}
	return strings.Join(k, " ")
}

// ofKind is the entries of evs of kind.
func ofKind(evs []Event, kind string) []Event {
	var out []Event
	for _, e := range evs {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// waitEvent waits for an entry of kind in the journal in dir, and answers it.
func waitEvent(t *testing.T, dir, kind string) Event {
	t.Helper()
	for range 2000 {
		if e := ofKind(held(t, dir), kind); len(e) > 0 {
			return e[len(e)-1]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no %s in the journal", kind)
	return Event{}
}

// appends records the batches a daemon's event appends.
type appends struct {
	mu      sync.Mutex
	batches []string
}

func (a *appends) record(evs []Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.batches = append(a.batches, kinds(evs))
}

func (a *appends) has(batch string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Contains(a.batches, batch)
}

func TestTheJournalNumbersAcrossRotationAndWriters(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var wg sync.WaitGroup
	for w := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 60 {
				// A journal of its own per append: separate file handles, as two processes have.
				j := &journal{dir: dir, keep: 5, max: 10}
				if _, err := j.append(Event{Kind: "test", Target: fmt.Sprintf("%d-%d", w, i)}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	evs := held(t, dir)
	if len(evs) < 5 || len(evs) > 10 {
		t.Errorf("%d entries kept, not 5 to 10", len(evs))
	}
	for i, e := range evs {
		if e.At.IsZero() || e.Kind != "test" {
			t.Errorf("%+v", e)
		}
		if i > 0 && e.Seq != evs[i-1].Seq+1 {
			t.Errorf("entry %d after %d", e.Seq, evs[i-1].Seq)
		}
	}
	if last := evs[len(evs)-1].Seq; last != 120 {
		t.Errorf("the last entry is %d, not 120: a number was given twice", last)
	}
	// A rotation keeps the numbers: the next entry follows the last.
	j := &journal{dir: dir, keep: 5, max: 10}
	if _, err := j.append(Event{Kind: "next"}); err != nil {
		t.Fatal(err)
	}
	evs = held(t, dir)
	if e := evs[len(evs)-1]; e.Seq != 121 || e.Kind != "next" {
		t.Errorf("%+v", e)
	}
	// State lost: the numbers are read off the journal again, never given twice.
	if err := os.WriteFile(filepath.Join(dir, eventsState), []byte("{cut"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := j.append(Event{Kind: "after"}); err != nil {
		t.Fatal(err)
	}
	evs = held(t, dir)
	if e := evs[len(evs)-1]; e.Seq != 122 {
		t.Errorf("after a lost state: %+v", e)
	}
	// A line a crash cut is skipped, and the next entry is not glued to it.
	f, err := os.OpenFile(filepath.Join(dir, EventsFile), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"seq":123,"kind":"cu`)
	f.Close()
	if _, err := j.append(Event{Kind: "whole"}); err != nil {
		t.Fatal(err)
	}
	evs = held(t, dir)
	if e := evs[len(evs)-1]; e.Seq != 123 || e.Kind != "whole" {
		t.Errorf("after a cut line: %+v", e)
	}
}

func TestTheEventsEndpointAnswersAfterANumber(t *testing.T) {
	t.Parallel()
	d := daemon(installedVM(t), &fakeHost{})
	d.O.Dir = filepath.Join(t.TempDir(), "none") // a journal not made yet
	h := d.Handler()
	var a EventsAnswer
	if code := call(t, h, "GET", "/v1/events", nil, &a); code != 200 || len(a.Events) != 0 || a.Last != 0 || a.Truncated {
		t.Fatalf("an empty journal: %d %+v", code, a)
	}
	if code := call(t, h, "GET", "/v1/events?after=3", nil, &a); code != 200 || !a.Truncated {
		t.Errorf("past an empty journal: %d %+v", code, a)
	}
	j := &journal{dir: d.O.Dir, keep: 5, max: 10}
	for i := range 12 {
		if _, err := j.append(Event{Kind: "test", Target: fmt.Sprint(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	// 12 written: the 11th went past 10, and the journal kept its last 5 (7 to 11), then 12.
	var st Status
	call(t, h, "GET", "/v1/status", nil, &st)
	if st.Events == nil || *st.Events != 12 || st.EventsError != "" {
		t.Errorf("status events %v %q", st.Events, st.EventsError)
	}
	for after, want := range map[int]struct {
		first, n  int64
		truncated bool
	}{
		0:  {7, 6, true},
		5:  {7, 6, true},
		6:  {7, 6, false},
		10: {11, 2, false},
		12: {0, 0, false},
		13: {0, 0, true},
	} {
		var a EventsAnswer
		if code := call(t, h, "GET", fmt.Sprintf("/v1/events?after=%d", after), nil, &a); code != 200 {
			t.Fatalf("after %d: %d", after, code)
		}
		if a.Last != 12 || a.Dropped != 6 || a.Truncated != want.truncated || int64(len(a.Events)) != want.n || (want.n > 0 && a.Events[0].Seq != want.first) {
			t.Errorf("after %d: %+v", after, a)
		}
	}
	for _, q := range []string{"wait=61", "wait=-1", "wait=x", "after=-1", "after=x"} {
		var e400 errOf
		if code := call(t, h, "GET", "/v1/events?"+q, nil, &e400); code != 400 || e400.Error == "" {
			t.Errorf("%s: %d %s", q, code, e400.Error)
		}
	}
	if code := call(t, h, "GET", "/v1/events?wait=60", nil, &a); code != 200 || len(a.Events) != 6 {
		t.Errorf("wait at its bound, entries waiting: %d %+v", code, a)
	}
}

func TestTheEventsLongPoll(t *testing.T) {
	t.Parallel()
	d := daemon(installedVM(t), &fakeHost{})
	d.Unit = 50 * time.Millisecond // wait=60: 3 s at most
	d.O.Dir = t.TempDir()
	h := d.Handler()
	poll := func() (EventsAnswer, time.Duration) {
		start := time.Now()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/events?after=0&wait=60", nil))
		var a EventsAnswer
		if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil || w.Code != 200 {
			t.Errorf("%d %v %s", w.Code, err, w.Body)
		}
		return a, time.Since(start)
	}
	// The daemon's own append wakes it.
	go func() {
		time.Sleep(20 * time.Millisecond)
		d.event(Event{Kind: "test"})
	}()
	if a, took := poll(); len(a.Events) != 1 || took > 2*time.Second {
		t.Errorf("the daemon's append: %+v after %s", a, took)
	}
	// Another process's (the gate's) is found by reading the journal again.
	go func() {
		time.Sleep(20 * time.Millisecond)
		if _, err := (&journal{dir: d.O.Dir}).append(Event{Kind: "test"}); err != nil {
			t.Error(err)
		}
	}()
	start := time.Now()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/events?after=1&wait=60", nil))
	var a EventsAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil || len(a.Events) != 1 || a.Events[0].Seq != 2 || time.Since(start) > 2*time.Second {
		t.Errorf("another writer's append: %v %+v after %s", err, a, time.Since(start))
	}
	// Nothing appended: an empty answer at the deadline.
	d.Unit = time.Millisecond
	start = time.Now()
	if code := call(t, d.Handler(), "GET", "/v1/events?after=2&wait=5", nil, &a); code != 200 || len(a.Events) != 0 || a.Last != 2 || a.Truncated {
		t.Errorf("the deadline: %d %+v", code, a)
	}
	if time.Since(start) < 5*time.Millisecond {
		t.Errorf("answered before the deadline: %s", time.Since(start))
	}
	// The daemon stopping ends it.
	ctx, cancel := context.WithCancel(context.Background())
	d.Unit, d.ctx = time.Hour, ctx
	time.AfterFunc(20*time.Millisecond, cancel)
	if code := call(t, d.Handler(), "GET", "/v1/events?after=2&wait=60", nil, &a); code != 200 || len(a.Events) != 0 {
		t.Errorf("the stop: %d %+v", code, a)
	}
}

func TestAJobAndItsRebootAreJournaled(t *testing.T) {
	t.Parallel()
	_, d, host := upgradedVM(t)
	d.O.BootIDFile = filepath.Join(t.TempDir(), "boot_id")
	if err := os.WriteFile(d.O.BootIDFile, []byte("boot-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	host.rebootErr = errors.New("inhibited")
	var ap appends
	d.onEvent = ap.record
	d.startJournal() // the boot is recorded
	before := len(held(t, d.O.Dir))
	boots(t, d, "active", map[string]string{assessEnable: "no", assessTentative: "no"})
	hh := d.Handler()
	if code := call(t, hh, "POST", "/v1/rollback", RollbackRequest{}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	j := waitJob(t, hh)
	waitEvent(t, d.O.Dir, evRebootCancelled)
	evs := held(t, d.O.Dir)[before:]
	// The job ends, done with its reboot, before the reboot runs; the reboot that fails then is
	// the job's.
	if kinds(evs) != "job.started reboot.scheduled job.ended reboot.cancelled" {
		t.Fatalf("%s", kinds(evs))
	}
	if !ap.has("reboot.scheduled job.ended") {
		t.Errorf("the reboot and the job's end are not one append: %v", ap.batches)
	}
	start, sched, end, cancelled := evs[0], evs[1], evs[2], evs[3]
	if !start.Job.Equal(*j.Started) || start.JobKind != "rollback" || start.Target != j.Target || start.BootID != "boot-1" {
		t.Errorf("job.started %+v, job %+v", start, j)
	}
	if sched.Action != "reboot" || sched.By == nil || !sched.By.Equal(*j.Started) || sched.DrainSecs == nil || *sched.DrainSecs != 0 || sched.BootID != "boot-1" {
		t.Errorf("reboot.scheduled %+v", sched)
	}
	if !end.Job.Equal(*j.Started) || end.State != "done" || end.Error != "" || end.JobKind != "rollback" || end.Reboot == nil || *end.Reboot != (EventReboot{"reboot", 0}) {
		t.Errorf("job.ended %+v", end)
	}
	if cancelled.Action != "reboot" || cancelled.Reason != "systemctl reboot failed: inhibited: the boot assessment is put back as it was, nothing changed" || cancelled.By == nil || !cancelled.By.Equal(*j.Started) {
		t.Errorf("reboot.cancelled %+v", cancelled)
	}
	if j := waitJob(t, hh); j.State != "done" {
		t.Errorf("the job's state is not its journaled end: %+v", j)
	}
	// A reboot asked for, whose systemctl fails: scheduled by no job, then cancelled.
	if code := call(t, hh, "POST", "/v1/reboot", PowerRequest{DrainSecs: ptr(0)}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	for range 2000 {
		if len(held(t, d.O.Dir)) == before+6 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	evs = held(t, d.O.Dir)[before+4:]
	if kinds(evs) != "reboot.scheduled reboot.cancelled" || evs[0].By != nil || evs[0].Action != "reboot" || evs[1].By != nil || !strings.Contains(evs[1].Reason, "systemctl reboot failed: inhibited") {
		t.Errorf("%+v", evs)
	}
}

func TestAnUpgradesEndAndItsRebootReachALongPollTogether(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.release("2.0.0", 1, nil)
	host := &fakeHost{gate: make(chan struct{})}
	d := daemon(p.env, host)
	d.Unit = time.Hour // the poll wakes only on an append; the reboot never runs
	var ap appends
	d.onEvent = ap.record
	hh := d.Handler()
	var started JobStarted
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", SkipBackup: true}, &started); code != 202 {
		t.Fatalf("%d", code)
	}
	if started.Events == nil {
		t.Fatalf("no events: %+v", started)
	}
	after := *started.Events // job.started's number
	if a, err := (&journal{dir: d.O.Dir}).since(after-1, 1); err != nil || len(a.Events) != 1 || a.Events[0].Kind != evJobStarted {
		t.Fatalf("the 202's events is not its job.started: %v %+v", err, a)
	}
	answer := make(chan EventsAnswer, 1)
	go func() {
		w := httptest.NewRecorder()
		hh.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/v1/events?after=%d&wait=60", after), nil))
		var a EventsAnswer
		if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil || w.Code != 200 {
			t.Errorf("%d %v %s", w.Code, err, w.Body)
		}
		answer <- a
	}()
	time.Sleep(20 * time.Millisecond) // the poll waits
	close(host.gate)
	a := <-answer
	if kinds(a.Events) != "reboot.scheduled job.ended" {
		t.Fatalf("the waiting poll answered %s", kinds(a.Events))
	}
	if !ap.has("reboot.scheduled job.ended") {
		t.Errorf("the reboot and the job's end are not one append: %v", ap.batches)
	}
	j := waitJob(t, hh)
	sched, end := a.Events[0], a.Events[1]
	if sched.By == nil || !sched.By.Equal(*j.Started) || sched.DrainSecs == nil || *sched.DrainSecs != 30 || sched.Action != "reboot" {
		t.Errorf("reboot.scheduled %+v", sched)
	}
	if end.State != "done" || end.JobKind != "upgrade" || !end.Job.Equal(*j.Started) || end.Reboot == nil || *end.Reboot != (EventReboot{"reboot", 30}) {
		t.Errorf("job.ended %+v", end)
	}
	var st Status
	call(t, hh, "GET", "/v1/status", nil, &st)
	if j.State != "done" || !st.RebootPending {
		t.Errorf("%+v %+v", j, st)
	}
}

func TestAnUpgradesRebootThatFailsIsCancelledByTheJob(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.release("2.0.0", 1, nil)
	host := &fakeHost{rebootErr: errors.New("inhibited")}
	d := daemon(p.env, host)
	hh := d.Handler()
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", SkipBackup: true}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	j := waitJob(t, hh)
	c := waitEvent(t, d.O.Dir, evRebootCancelled)
	if c.By == nil || !c.By.Equal(*j.Started) || c.Reason != "systemctl reboot failed: inhibited" {
		t.Errorf("reboot.cancelled %+v", c)
	}
	if e := ofKind(held(t, d.O.Dir), evJobEnded); len(e) != 1 || e[0].State != "done" || e[0].Reboot == nil {
		t.Errorf("job.ended %+v", e)
	}
	j = waitJob(t, hh)
	var st Status
	call(t, hh, "GET", "/v1/status", nil, &st)
	if j.State != "done" || st.RebootPending || !strings.Contains(strings.Join(j.Log, "\n"), "systemctl reboot failed: inhibited: the system keeps running") {
		t.Errorf("%+v %+v", j, st)
	}
}

func TestARollbackStoppedByItsRebootIsNotLost(t *testing.T) {
	t.Parallel()
	e, d, host := upgradedVM(t)
	host.rebootHold = make(chan struct{})
	t.Cleanup(func() { close(host.rebootHold) })
	d.O.BootIDFile = filepath.Join(t.TempDir(), "boot_id")
	if err := os.WriteFile(d.O.BootIDFile, []byte("boot-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hh := d.Handler()
	if code := call(t, hh, "POST", "/v1/rollback", RollbackRequest{}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	for range 2000 {
		if strings.Contains(fmt.Sprint(host.calls()), "systemctl reboot") {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(fmt.Sprint(host.calls()), "systemctl reboot") {
		t.Fatalf("no reboot: %v", host.calls())
	}
	// systemctl reboot has not returned: the shutdown stops the daemon here. The job's end is
	// journaled already.
	end := ofKind(held(t, d.O.Dir), evJobEnded)
	if len(end) != 1 || end[0].State != "done" || end[0].JobKind != "rollback" || end[0].Reboot == nil || *end[0].Reboot != (EventReboot{"reboot", 0}) {
		t.Fatalf("job.ended before the reboot: %+v", end)
	}
	// The next boot's daemon finds nothing lost.
	next := daemon(e, &fakeHost{})
	next.O.BootIDFile = filepath.Join(t.TempDir(), "boot_id")
	if err := os.WriteFile(next.O.BootIDFile, []byte("boot-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := len(held(t, d.O.Dir))
	next.startJournal()
	if added := held(t, d.O.Dir)[before:]; kinds(added) != "boot.started" {
		t.Errorf("the next boot added %s", kinds(added))
	}
}

func TestAStartingDaemonRecordsItsBootOnce(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	d := daemon(e, &fakeHost{})
	file := filepath.Join(t.TempDir(), "boot_id")
	d.O.BootIDFile = file
	bootID := func(id string) {
		if err := os.WriteFile(file, []byte(id+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bootID("boot-1")
	boots(t, d, "passive", nil)
	if err := os.WriteFile(filepath.Join(d.O.Cos, upgradeFailure), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.startJournal()
	d = daemon(e, &fakeHost{}) // the daemon restarted: the same boot
	d.O.BootIDFile = file
	d.startJournal()
	bootID("boot-2")
	d = daemon(e, &fakeHost{}) // the next boot's
	d.O.BootIDFile = file
	d.startJournal()
	b := ofKind(held(t, d.O.Dir), evBootStarted)
	if len(b) != 2 || b[0].BootID != "boot-1" || b[1].BootID != "boot-2" {
		t.Fatalf("%+v", b)
	}
	if b[0].Release == nil || *b[0].Release != (Version{"1.0.0", 1}) || b[0].Slot != "passive" || b[0].Fallback == nil || !*b[0].Fallback {
		t.Errorf("%+v", b[0])
	}
}

func TestAStartingDaemonClosesWhatThePreviousOneLeftOpen(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		now        string
		lost       string
		cancelled  bool
		bootLogged bool
	}{
		"the daemon restarted": {"boot-1", lostRestart, true, false},
		"the system rebooted":  {"boot-2", lostReboot, false, true},
	} {
		d := daemon(installedVM(t), &fakeHost{})
		d.O.BootIDFile = filepath.Join(t.TempDir(), "boot_id")
		if err := os.WriteFile(d.O.BootIDFile, []byte(c.now+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		before := len(held(t, d.O.Dir))
		lost, ended := time.Date(2026, 10, 1, 0, 0, 0, 1, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		if _, err := (&journal{dir: d.O.Dir}).append(
			Event{Kind: evBootStarted, BootID: "boot-1"},
			Event{Kind: evJobStarted, Job: &ended, JobKind: "backup", Target: "a", BootID: "boot-1"},
			Event{Kind: evJobEnded, Job: &ended, JobKind: "backup", Target: "a", State: "done"},
			Event{Kind: evJobStarted, Job: &lost, JobKind: "upgrade", Target: "2.0.0-1", BootID: "boot-1"},
			Event{Kind: evRebootScheduled, Action: "poweroff", By: &lost, BootID: "boot-1"},
		); err != nil {
			t.Fatal(err)
		}
		d.startJournal()
		added := held(t, d.O.Dir)[before+5:]
		// This boot's boot.started, when not recorded yet, comes first: before what the boot
		// before left open.
		want := "job.lost"
		if c.bootLogged {
			want = "boot.started job.lost"
		}
		if c.cancelled {
			want += " reboot.cancelled"
		}
		if kinds(added) != want {
			t.Fatalf("%s: %s", name, kinds(added))
		}
		all := added
		if c.bootLogged {
			if added[0].BootID != "boot-2" {
				t.Errorf("%s: %+v", name, added[0])
			}
			added = added[1:]
		}
		if l := added[0]; !l.Job.Equal(lost) || l.JobKind != "upgrade" || l.Target != "2.0.0-1" || l.Reason != c.lost {
			t.Errorf("%s: %+v", name, l)
		}
		if c.cancelled && (added[1].Action != "poweroff" || added[1].Reason != cancelledByRestart || added[1].By == nil || !added[1].By.Equal(lost)) {
			t.Errorf("%s: %+v", name, added[1])
		}
		// Started again, nothing is left open.
		d.startJournal()
		if n := len(held(t, d.O.Dir)); n != before+5+len(all) {
			t.Errorf("%s: the second start added %d entries", name, n-before-5-len(all))
		}
	}
}

func TestTheGateJournalsItsVerdict(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		mode     string
		vars     map[string]string
		passive  bool
		status   int
		verdict  string
		onTrial  bool
		kept     string
		rebooted bool
	}{
		"healthy on trial":     {"active", onTrial, true, 200, verdictHealthy, true, "", false},
		"healthy off trial":    {"active", nil, true, 200, verdictHealthy, false, "", false},
		"unhealthy, goes back": {"active", onTrial, true, 503, verdictWentBack, true, "", true},
		"unhealthy, no passive": {"active", onTrial, false, 503, verdictKeptUnhealthy, true,
			"there is no passive system to go back to", false},
		"unhealthy off trial": {"passive", onTrial, true, 503, verdictKeptUnhealthy, false, "the boot is not on trial", false},
	} {
		g := newGate(t, c.mode, c.vars, c.passive)
		g.probe.status = c.status
		g.gate.In.O.BootIDFile = filepath.Join(t.TempDir(), "boot_id")
		if err := os.WriteFile(g.gate.In.O.BootIDFile, []byte("boot-9\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = g.gate.Run(context.Background())
		j := ofKind(held(t, g.dir), evBootJudged)
		if len(j) != 1 {
			t.Fatalf("%s: %+v", name, j)
		}
		e := j[0]
		if e.Verdict != c.verdict || e.OnTrial == nil || *e.OnTrial != c.onTrial || e.BootID != "boot-9" || e.Release == nil || *e.Release != (Version{"1.0.0", 1}) || !strings.HasPrefix(e.Kept, c.kept) || (c.kept == "") != (e.Kept == "") {
			t.Errorf("%s: %+v", name, e)
		}
		if (c.status != 200) != strings.Contains(e.Reason, "answered 503") {
			t.Errorf("%s: reason %q", name, e.Reason)
		}
		if g.rebooted() != c.rebooted {
			t.Errorf("%s: rebooted %v", name, g.rebooted())
		}
	}
}

func TestAVMsFirstInstallIsJournaled(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	bootID := filepath.Join(t.TempDir(), "boot_id")
	if err := os.WriteFile(bootID, []byte("boot-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The installer reads Kairos's marks where its options say, as the daemon and the gate do.
	if err := os.WriteFile(filepath.Join(e.cos, "active_mode"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	inBoot := func(o *Options) { vm(o); o.BootIDFile = bootID }
	e.docker.upErr = errors.New("unhealthy")
	if err := e.install(inBoot); err == nil {
		t.Fatal("installed")
	}
	e.docker.upErr = nil
	if err := e.install(inBoot); err != nil {
		t.Fatal(err)
	}
	if err := e.install(inBoot); err != nil { // a re-run finishes nothing
		t.Fatal(err)
	}
	// The installer is the boot's first writer: its boot.started comes first, once.
	evs := held(t, e.dir)
	if kinds(evs) != "boot.started install.failed install.finished" {
		t.Fatalf("%s", kinds(evs))
	}
	if b := evs[0]; b.BootID != "boot-1" || b.Release == nil || *b.Release != (Version{"1.0.0", 1}) || b.Slot != "active" {
		t.Errorf("%+v", b)
	}
	evs = evs[1:]
	if evs[0].Release == nil || *evs[0].Release != (Version{"1.0.0", 1}) || !strings.Contains(evs[0].Reason, "unhealthy") || evs[1].Reason != "" {
		t.Errorf("%+v", evs)
	}
	// Off the VM, nothing is journaled.
	d := newEnv(t)
	if err := d.install(func(*Options) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.dir, EventsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a Docker install wrote a journal: %v", err)
	}
}

// The last boot recorded survives a rotation that drops its boot.started: a daemon restarted in
// the same boot records it once, a later boot again.
func TestTheBootRecordedSurvivesRotation(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	d := daemon(e, &fakeHost{})
	file := filepath.Join(t.TempDir(), "boot_id")
	d.O.BootIDFile = file
	bootID := func(id string) {
		if err := os.WriteFile(file, []byte(id+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bootID("boot-1")
	d.startJournal()
	j := &journal{dir: d.O.Dir, keep: 2, max: 4}
	for i := range 6 {
		if _, err := j.append(Event{Kind: "test", Target: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if b := ofKind(held(t, d.O.Dir), evBootStarted); len(b) != 0 {
		t.Fatalf("the rotation kept %+v", b)
	}
	d.startJournal() // the daemon restarted
	if b := ofKind(held(t, d.O.Dir), evBootStarted); len(b) != 0 {
		t.Errorf("the same boot recorded again: %+v", b)
	}
	// A state from before it records no boot: the entries held tell none, so the boot is
	// recorded.
	if err := os.WriteFile(filepath.Join(d.O.Dir, eventsState), []byte(`{"last":7,"dropped":5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bootID("boot-2")
	d = daemon(e, &fakeHost{})
	d.O.BootIDFile = file
	if _, err := d.journal().append(Event{Kind: "test"}); err != nil {
		t.Fatal(err)
	}
	if b := ofKind(held(t, d.O.Dir), evBootStarted); len(b) != 1 || b[0].BootID != "boot-2" {
		t.Errorf("%+v", b)
	}
}

// Every writer that knows its boot puts the boot's boot.started before its first entry in it,
// once, whichever writes first.
func TestAnyWriterRecordsItsBootFirst(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := filepath.Join(t.TempDir(), "boot_id")
	cos := t.TempDir()
	if err := os.WriteFile(filepath.Join(cos, "active_mode"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	writer := func() *journal {
		return &journal{dir: dir, boot: bootRecord(&Options{BootIDFile: id, Cos: cos}, Version{"1.0.0", 1})}
	}
	for _, b := range []string{"boot-1", "boot-1", "", "boot-2", "boot-2"} {
		if err := os.WriteFile(id, []byte(b+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := writer().append(Event{Kind: evBootJudged}); err != nil {
			t.Fatal(err)
		}
	}
	evs := held(t, dir)
	// A boot whose id cannot be read is not recorded.
	if kinds(evs) != "boot.started boot.judged boot.judged boot.judged boot.started boot.judged boot.judged" {
		t.Fatalf("%s", kinds(evs))
	}
	if b := evs[0]; b.BootID != "boot-1" || b.Slot != "active" || b.Fallback == nil || *b.Fallback || b.Release == nil || *b.Release != (Version{"1.0.0", 1}) {
		t.Errorf("%+v", b)
	}
	if evs[4].BootID != "boot-2" {
		t.Errorf("%+v", evs[4])
	}
}

// A journal rewritten past the floor its state records (a crash between the two) still answers
// the entries it dropped as gone, and an entry under a floor recorded first is answered as gone.
func TestTruncationFollowsTheEntriesHeld(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		state   string
		seqs    []int64
		dropped int64
	}{
		"rewritten, state not":             {`{"last":6,"dropped":0}`, []int64{5, 6}, 4},
		"state written, not yet rewritten": {`{"last":6,"dropped":4}`, []int64{3, 4, 5, 6}, 4},
		"emptied":                          {`{"last":6,"dropped":0}`, nil, 6},
	} {
		dir := t.TempDir()
		var lines []string
		for _, s := range c.seqs {
			lines = append(lines, fmt.Sprintf(`{"seq":%d,"at":"2026-10-14T00:00:00Z","kind":"test"}`, s))
		}
		if err := os.WriteFile(filepath.Join(dir, EventsFile), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, eventsState), []byte(c.state), 0o600); err != nil {
			t.Fatal(err)
		}
		a, err := (&journal{dir: dir}).since(2, 10)
		if err != nil {
			t.Fatal(err)
		}
		if !a.Truncated || a.Dropped != c.dropped || (len(a.Events) > 0 && a.Events[0].Seq != c.dropped+1) {
			t.Errorf("%s: %+v", name, a)
		}
		// The reader goes on after the floor, and is not truncated there.
		if next := nextAfter(2, a); next != c.dropped {
			t.Errorf("%s: goes on after %d", name, next)
		}
		if a, _ := (&journal{dir: dir}).since(c.dropped, 10); a.Truncated {
			t.Errorf("%s: truncated at the floor: %+v", name, a)
		}
	}
	// Past the last entry, a reader starts again from the floor.
	if next := nextAfter(9, EventsAnswer{Last: 6, Dropped: 4, Truncated: true}); next != 4 {
		t.Errorf("past the last: %d", next)
	}
}

// A truncated answer with no entries moves the reader to the floor: it never asks the same
// number again.
func TestEventsMoveToTheFloorOfATruncatedAnswer(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	var mu sync.Mutex
	var afters []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		afters = append(afters, r.URL.Query().Get("after"))
		n := len(afters)
		mu.Unlock()
		if n > 5 {
			replyErr(w, fmt.Errorf("asked %d times", n))
			return
		}
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		reply(w, 200, EventsAnswer{Events: []Event{}, Last: 10, Dropped: 10, Truncated: after < 10 || after > 10})
	})
	sock := servedHandler(t, mux)
	out, err := ctl(e, sock, "events", "--after", "3")
	if err != nil || strings.Join(afters, ",") != "3,10" || !strings.Contains(out, "entries 4 to 10 are no longer kept") {
		t.Errorf("%v %v\n%s", err, afters, out)
	}
}

// Following, a daemon that cannot be reached is asked again from the same number.
func TestFollowingEventsOutlivesTheDaemon(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var afters []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		afters = append(afters, r.URL.Query().Get("after"))
		n := len(afters)
		mu.Unlock()
		switch n {
		case 1, 2: // the daemon restarts
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		case 3:
			reply(w, 200, EventsAnswer{Events: []Event{{Seq: 1, Kind: evBootStarted, BootID: "boot-1"}}, Last: 1})
		default:
			cancel()
			reply(w, 200, EventsAnswer{Events: []Event{}, Last: 1})
		}
	})
	sock := servedHandler(t, mux)
	var out, errb bytes.Buffer
	c := &hostClient{http: socketClient(&sock), out: &out, unit: time.Millisecond}
	if err := c.events(ctx, &errb, 0, true, false); err != nil {
		t.Fatal(err)
	}
	if strings.Join(afters, ",") != "0,0,0,1" || !strings.Contains(out.String(), "boot.started boot_id=boot-1") ||
		strings.Count(errb.String(), "asking again after 0") != 1 || !strings.Contains(errb.String(), "answers again") {
		t.Errorf("%v\n%s\n%s", afters, out.String(), errb.String())
	}
	// Not following, it is an error.
	mu.Lock()
	afters = nil
	mu.Unlock()
	if err := c.events(context.Background(), &errb, 0, false, false); err == nil {
		t.Error("no error")
	}
}

// Following, a daemon that takes a request and never answers is one that cannot be reached: the
// request's deadline (its wait and requestMargin) ends it, and the next asks from the same number.
func TestFollowingEventsBoundsEachRequest(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var afters []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		afters = append(afters, r.URL.Query().Get("after"))
		n := len(afters)
		mu.Unlock()
		switch n {
		case 1:
			hang(w, r)
		case 2:
			reply(w, 200, EventsAnswer{Events: []Event{{Seq: 1, Kind: evBootStarted, BootID: "boot-1"}}, Last: 1})
		default:
			cancel()
			reply(w, 200, EventsAnswer{Events: []Event{}, Last: 1})
		}
	})
	sock := servedHandler(t, mux)
	var out, errb bytes.Buffer
	c := &hostClient{http: socketClient(&sock), out: &out, unit: time.Millisecond}
	if _, err := bounded(t, func() (string, error) { return "", c.events(ctx, &errb, 0, true, false) }); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(afters, ",") != "0,0,1" || !strings.Contains(out.String(), "boot.started boot_id=boot-1") ||
		!strings.Contains(errb.String(), "asking again after 0") {
		t.Errorf("%v\n%s\n%s", afters, out.String(), errb.String())
	}
}

// A journal that cannot be read gives no cursor: the status's events is null, events_error says
// why — never a 0 a client would replay the journal from.
func TestAnUnreadableJournalGivesNoCursor(t *testing.T) {
	t.Parallel()
	d := daemon(installedVM(t), &fakeHost{})
	h := d.Handler()
	var st map[string]any
	call(t, h, "GET", "/v1/status", nil, &st)
	if v, ok := st["events"].(float64); !ok || v < 0 || st["events_error"] != nil {
		t.Fatalf("a journal not made yet: %v", st)
	}
	// The writers' lock cannot be taken.
	if err := os.Remove(filepath.Join(d.O.Dir, eventsState)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d.O.Dir, eventsState), 0o755); err != nil {
		t.Fatal(err)
	}
	st = nil
	if code := call(t, h, "GET", "/v1/status", nil, &st); code != 200 {
		t.Fatalf("%d %v", code, st)
	}
	if v, ok := st["events"]; !ok || v != nil {
		t.Errorf("events %v (present %v), want null", v, ok)
	}
	if e, _ := st["events_error"].(string); !strings.Contains(e, "the event journal cannot be read") {
		t.Errorf("events_error %q", e)
	}
}

// A job whose start cannot be journaled answers its 202 without a number (null) and says why;
// one journaled answers its number.
func TestA202GivesNoNumberTheJournalDidNotGive(t *testing.T) {
	t.Parallel()
	d := daemon(installedVM(t), &fakeHost{})
	start := func() map[string]any {
		t.Helper()
		j, err := d.startJob("backup-verify", "store:b", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer d.finish(j, nil)
		w := httptest.NewRecorder()
		accepted(w, j)
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := start(); got["events"] == nil || got["events_error"] != nil {
		t.Errorf("journaled: %v", got)
	}
	// The writers' lock cannot be opened: nothing is journaled.
	state := filepath.Join(d.O.Dir, eventsState)
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state, 0o755); err != nil {
		t.Fatal(err)
	}
	got := start()
	if v, ok := got["events"]; !ok || v != nil {
		t.Errorf("events %v (present %v), want null", v, ok)
	}
	if msg, _ := got["events_error"].(string); !strings.Contains(msg, "the job's start could not be journaled") {
		t.Errorf("events_error %q", msg)
	}
}

// A job's end carries what a job that is done found, as GET job's result gives it.
func TestAJobsEndCarriesItsResult(t *testing.T) {
	t.Parallel()
	d := daemon(installedVM(t), &fakeHost{})
	j, err := d.startJob("backup-verify", "store:b", nil)
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	j.Result = "store:b is reachable"
	d.mu.Unlock()
	d.finish(j, nil)
	if e := ofKind(held(t, d.O.Dir), evJobEnded); len(e) != 1 || e[0].Result != "store:b is reachable" {
		t.Errorf("%+v", e)
	}
	// A job that ends with a reboot carries its result too.
	if j, err = d.startJob("rollback", "passive", nil); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	j.Result = "the passive system is next"
	d.mu.Unlock()
	d.endRebooting(j, "reboot", 0)
	if e := ofKind(held(t, d.O.Dir), evJobEnded); len(e) != 2 || e[1].Result != "the passive system is next" || e[1].Reboot == nil {
		t.Errorf("%+v", e)
	}
}

// A job's start and a reboot's scheduling carry the boot of the record their append leads with —
// never one read apart from it — and none when the writer's boot is not known.
func TestAnEntrysBootIsItsAppends(t *testing.T) {
	t.Parallel()
	for name, boot := range map[string]*Event{"known": {Kind: evBootStarted, BootID: "boot-1"}, "unread": nil} {
		dir := t.TempDir()
		at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		if _, err := (&journal{dir: dir, boot: boot}).append(
			Event{Kind: evJobStarted, Job: &at, JobKind: "upgrade", Target: "x"},
			Event{Kind: evRebootScheduled, Action: "reboot", By: &at},
			Event{Kind: evJobEnded, Job: &at, JobKind: "upgrade", Target: "x", State: "done"},
		); err != nil {
			t.Fatal(err)
		}
		want := bootIDOf(boot)
		for _, e := range held(t, dir) {
			got := e.BootID
			if e.Kind == evJobEnded {
				want = ""
			}
			if got != want {
				t.Errorf("%s: %s carries %q, want %q", name, e.Kind, got, want)
			}
		}
	}
	d := daemon(installedVM(t), &fakeHost{})
	d.O.BootIDFile = filepath.Join(t.TempDir(), "boot_id")
	if err := os.WriteFile(d.O.BootIDFile, []byte("boot-7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	j, err := d.startJob("backup-verify", "store:b", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.finish(j, nil)
	if e := ofKind(held(t, d.O.Dir), evJobStarted); len(e) != 1 || e[0].BootID != "boot-7" {
		t.Errorf("%+v", e)
	}
}

// A starting daemon tells a boot from another only by two ids read: an empty one decides neither
// a job lost to a reboot nor to a restart, nor a reboot's end or its cancel.
func TestRecoveryDecidesNothingFromAnUnreadBootID(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		held []Event
		now  string
		want string // the kinds and reasons appended
	}{
		"a job of this boot":          {[]Event{{Kind: evJobStarted, Job: &at, BootID: "boot-1"}}, "boot-1", "job.lost:" + lostRestart},
		"a job of another boot":       {[]Event{{Kind: evJobStarted, Job: &at, BootID: "boot-1"}}, "boot-2", "job.lost:" + lostReboot},
		"a job of a boot not read":    {[]Event{{Kind: evJobStarted, Job: &at}}, "boot-2", "job.lost:" + lostUnknown},
		"a job in a boot not read":    {[]Event{{Kind: evJobStarted, Job: &at, BootID: "boot-1"}}, "", "job.lost:" + lostUnknown},
		"a reboot of this boot":       {[]Event{{Kind: evRebootScheduled, By: &at, BootID: "boot-1"}}, "boot-1", "reboot.cancelled:" + cancelledByRestart},
		"a reboot that ran":           {[]Event{{Kind: evRebootScheduled, By: &at, BootID: "boot-1"}, {Kind: evBootStarted, BootID: "boot-2"}}, "boot-2", ""},
		"a reboot of a boot not read": {[]Event{{Kind: evRebootScheduled, By: &at}, {Kind: evBootStarted, BootID: "boot-1"}}, "boot-1", ""},
		"a reboot, no id read":        {[]Event{{Kind: evRebootScheduled, By: &at}}, "", ""},
		// The boot recorded late is no reboot: the reboot stays open, and a restart in the same
		// boot read then cancels it.
		"a reboot, its boot recorded late": {[]Event{{Kind: evRebootScheduled, By: &at, BootID: "boot-1"}, {Kind: evBootStarted, BootID: "boot-1"}}, "boot-1", "reboot.cancelled:" + cancelledByRestart},
	} {
		var boot *Event
		if c.now != "" {
			boot = &Event{Kind: evBootStarted, BootID: c.now}
		}
		var got []string
		for _, e := range recoverEvents(c.held, boot) {
			got = append(got, e.Kind+":"+e.Reason)
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// A boot record whose read failed is read again once its retry's wait passed, never before; one
// read is kept.
func TestABootReadThatFailedIsRetriedAfterItsWait(t *testing.T) {
	t.Parallel()
	var b bootCache
	reads := 0
	rec := (*Event)(nil)
	read := func() *Event { reads++; return rec }
	at := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	if b.get(at, time.Minute, read) != nil || reads != 1 {
		t.Fatalf("first read: %d", reads)
	}
	rec = &Event{Kind: evBootStarted, BootID: "boot-1"}
	if b.get(at.Add(time.Minute-time.Nanosecond), time.Minute, read) != nil || reads != 1 {
		t.Errorf("read again before its wait: %d", reads)
	}
	if got := b.get(at.Add(time.Minute), time.Minute, read); got != rec || reads != 2 {
		t.Errorf("not read again once its wait passed: %d %+v", reads, got)
	}
	if b.get(at.Add(time.Hour), time.Minute, read) != rec || reads != 2 {
		t.Errorf("a read that succeeded is read again: %d", reads)
	}
}
