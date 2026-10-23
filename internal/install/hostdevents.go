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
	"sync"
	"time"
)

// The event journal: what the host daemon and the health gate (a process of its own) did, one
// JSON object per line in EventsFile, in the install directory, which survives daemon restarts
// and reboots. Every entry carries a number (seq) one past the last one written in the journal's
// life — a rotation keeps it — and its time; GET /v1/events reads the entries after a number.
//
// Writers append under an exclusive flock on eventsState, which also records the last number
// given, the last one a rotation dropped and the last boot a boot.started recorded, so two
// processes never interleave lines or give one number twice. The state is written before the
// lines: a crash between leaves a gap in the numbers, never a number used twice.
//
// Each entry follows its boot's boot.started: a writer that knows the boot it runs in (the
// daemon, the health gate, the VM's installer) prepends one to what it appends whenever the last
// boot recorded is another, whichever process writes first in a boot. A boot's boot.started is
// therefore its first entry, before anything a writer appends in it — a starting daemon's
// job.lost and reboot.cancelled for what the boot before left open included.

const (
	// EventsFile, in the install directory, is the journal.
	EventsFile = "events.jsonl"
	// eventsState, beside it, is the lock writers take and the numbers they share.
	eventsState = ".stowage-events"
	// Past eventsMax entries, a writer rewrites the journal with its last eventsKeep.
	eventsKeep = 1000
	eventsMax  = 2000
	// maxEventsAnswer bounds the entries one answer carries; maxEventsWait the long poll, in
	// seconds.
	maxEventsAnswer = 200
	maxEventsWait   = 60
)

// The entries' kinds.
const (
	evJobStarted      = "job.started"
	evJobEnded        = "job.ended"
	evJobLost         = "job.lost"
	evRebootScheduled = "reboot.scheduled"
	evRebootCancelled = "reboot.cancelled"
	evBootStarted     = "boot.started"
	evBootJudged      = "boot.judged"
	evInstallFinished = "install.finished"
	evInstallFailed   = "install.failed"
	evCertExpiring    = "cert.expiring"
)

// Why a job was lost, and a reboot cancelled at the daemon's start.
const (
	lostRestart        = "the daemon restarted"
	lostReboot         = "the system rebooted"
	lostUnknown        = "the daemon restarted, or the system rebooted (a boot id was not read)"
	cancelledByRestart = "the daemon restarted before it"
)

// The health gate's verdicts on a boot.
const (
	verdictHealthy       = "healthy"
	verdictWentBack      = "went_back"
	verdictKeptUnhealthy = "kept_unhealthy"
)

// Event is one entry of the journal. Seq, At and Kind are always set; the other fields are the
// kind's.
type Event struct {
	Seq  int64     `json:"seq"`
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	// Job identifies a job by its start (JobStatus's started); JobKind is its kind.
	Job     *time.Time `json:"job,omitempty"`
	JobKind string     `json:"job_kind,omitempty"`
	Target  string     `json:"target,omitempty"`
	State   string     `json:"state,omitempty"`
	Error   string     `json:"error,omitempty"`
	// Result, on a job's end, is what a job that is done found (GET /v1/job's result).
	Result string `json:"result,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Action is reboot or poweroff; DrainSecs the wait before it; By the job that scheduled it.
	Action    string     `json:"action,omitempty"`
	DrainSecs *int       `json:"drain_secs,omitempty"`
	By        *time.Time `json:"by,omitempty"`
	// BootID is the boot the entry was written in (a job's start, a reboot's scheduling, a boot).
	BootID  string   `json:"boot_id,omitempty"`
	Release *Version `json:"release,omitempty"`
	// Slot is the system Kairos booted: active, passive or recovery; absent off Kairos.
	Slot     string `json:"slot,omitempty"`
	Fallback *bool  `json:"fallback,omitempty"`
	OnTrial  *bool  `json:"on_trial,omitempty"`
	Verdict  string `json:"verdict,omitempty"`
	// Kept is why a boot found unhealthy keeps running: going back is refused, or impossible.
	Kept string `json:"kept,omitempty"`
	// Reboot, on a job's end, is the reboot or power-off the job scheduled as its last step.
	Reboot *EventReboot `json:"reboot,omitempty"`
}

// EventReboot is the reboot a job scheduled: the same action and drain as its reboot.scheduled.
type EventReboot struct {
	Action    string `json:"action"`
	DrainSecs int    `json:"drain_secs"`
}

// EventsAnswer is what GET /v1/events answers.
type EventsAnswer struct {
	Events []Event `json:"events"`
	// Last is the last entry's number (0 before the first).
	Last int64 `json:"last"`
	// Dropped is the journal's floor: every entry up to it is gone (0 when none is).
	Dropped int64 `json:"dropped"`
	// Truncated: entries after the number asked from are gone — a rotation dropped them (the
	// number is below Dropped), or the number is past Last (the journal started again). A reader
	// goes on after Dropped, or from 0 past Last (nextAfter).
	Truncated bool `json:"truncated"`
}

// nextAfter is where a reader that asked after after goes on from a truncated answer a: from the
// start when after is past the last entry (then after the floor), else after the floor. A
// truncated answer is never asked for again from the same number, so it cannot spin a reader.
func nextAfter(after int64, a EventsAnswer) int64 {
	if after > a.Last {
		after = 0
	}
	return max(after, a.Dropped)
}

// journal is the event journal of the install in dir.
type journal struct {
	dir string
	// keep and max are the rotation's bounds; 0 is eventsKeep and eventsMax.
	keep, max int64
	// notify, when set, is told of each append.
	notify func()
	// boot, when set, is the boot.started of the boot the writer runs in (nil: its id cannot be
	// read, or the writer is not on the VM): update prepends it when the last boot recorded is
	// another, and gives its id to the entries that carry the boot they were written in. A
	// long-lived writer keeps it once a read succeeds (bootCache): a boot's id and marks do not
	// change.
	boot *Event
}

// journalState is what eventsState records.
type journalState struct {
	Last    int64 `json:"last"`
	Dropped int64 `json:"dropped"` // every entry up to it is gone
	// Boot is the boot id the last boot.started carried: kept through a rotation that drops it.
	Boot string `json:"boot,omitempty"`
}

func (j *journal) path() string      { return filepath.Join(j.dir, EventsFile) }
func (j *journal) statePath() string { return filepath.Join(j.dir, eventsState) }

// append writes evs, numbered from the journal's last entry on; it returns the last number
// written.
func (j *journal) append(evs ...Event) (int64, error) {
	return j.update(false, func([]Event) []Event { return evs })
}

// update appends what f returns, under the exclusive lock; f is given the entries the journal
// holds when read is set. When the writer knows its boot (boot) and the last one recorded is
// another, a boot.started of its boot comes first (written even when f returns nothing): update
// alone writes a boot's entry. A job.started or reboot.scheduled without a boot_id is given the
// id of that same record (none when it is nil), so an entry never names another boot than the
// boot.started its append leads with. It returns the journal's last number once written.
func (j *journal) update(read bool, f func(held []Event) []Event) (int64, error) {
	if err := os.MkdirAll(j.dir, 0o755); err != nil {
		return 0, err
	}
	unlock, err := flockWait(j.statePath(), true)
	if err != nil {
		return 0, err
	}
	defer unlock()
	cur := j.boot
	st, ok := j.readState()
	var held []Event
	if read || !ok || (cur != nil && st.Boot == "") {
		if held, err = j.read(); err != nil {
			return 0, err
		}
	}
	if !ok {
		st = stateOf(held)
	} else if st.Boot == "" {
		st.Boot = stateOf(held).Boot
	}
	evs := f(held)
	if cur != nil && cur.BootID != st.Boot {
		evs = append([]Event{*cur}, evs...)
	}
	if len(evs) == 0 {
		return st.Last, nil
	}
	var buf bytes.Buffer
	now := time.Now().UTC()
	for _, e := range evs {
		st.Last++
		e.Seq = st.Last
		if e.At.IsZero() {
			e.At = now
		}
		if (e.Kind == evJobStarted || e.Kind == evRebootScheduled) && e.BootID == "" {
			e.BootID = bootIDOf(cur)
		}
		if e.Kind == evBootStarted && e.BootID != "" {
			st.Boot = e.BootID
		}
		line, err := json.Marshal(e)
		if err != nil {
			return 0, err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := j.writeState(st); err != nil {
		return 0, err
	}
	if err := j.write(buf.Bytes()); err != nil {
		return 0, err
	}
	if j.notify != nil {
		defer j.notify()
	}
	if st.Last-st.Dropped > cmp.Or(j.max, eventsMax) {
		if err := j.rotate(&st); err != nil {
			return st.Last, fmt.Errorf("rotating %s: %w", j.path(), err)
		}
	}
	return st.Last, nil
}

// write appends lines to the journal, after a line feed when a crash left its last line cut.
func (j *journal) write(lines []byte) error {
	f, err := os.OpenFile(j.path(), os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
			lines = append([]byte{'\n'}, lines...)
		}
	}
	_, err = f.Write(lines)
	return errors.Join(err, f.Sync(), f.Close())
}

// rotate rewrites the journal with the entries after the last keep numbers. The floor is
// recorded first: a crash before the rewrite claims entries gone that are still held, which a
// reader only skips, never the reverse.
func (j *journal) rotate(st *journalState) error {
	held, err := j.read()
	if err != nil {
		return err
	}
	from := st.Last - cmp.Or(j.keep, eventsKeep)
	st.Dropped = from
	if err := j.writeState(*st); err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, e := range held {
		if e.Seq > from {
			line, err := json.Marshal(e)
			if err != nil {
				return err
			}
			buf.Write(line)
			buf.WriteByte('\n')
		}
	}
	return writeAtomic(j.path(), &buf, 0o600, nil)
}

// readState reads the numbers; not ok when there are none (a journal made before it, or a
// crash cut its write): the caller reads them off the journal.
func (j *journal) readState() (journalState, bool) {
	var st journalState
	raw, err := os.ReadFile(j.statePath())
	if err != nil || len(raw) == 0 || json.Unmarshal(raw, &st) != nil {
		return journalState{}, false
	}
	return st, true
}

func (j *journal) writeState(st journalState) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(j.statePath(), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(raw, '\n'))
	return errors.Join(err, f.Sync(), f.Close())
}

// stateOf is the numbers and the last boot the entries held tell.
func stateOf(held []Event) journalState {
	var st journalState
	for _, e := range held {
		st.Last = max(st.Last, e.Seq)
		if e.Kind == evBootStarted && e.BootID != "" {
			st.Boot = e.BootID
		}
	}
	if len(held) > 0 {
		st.Dropped = held[0].Seq - 1
	}
	return st
}

// read is the entries the journal holds; a line that does not read (cut by a crash) is skipped.
func (j *journal) read() ([]Event, error) {
	raw, err := os.ReadFile(j.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var held []Event
	for line := range bytes.SplitSeq(raw, []byte("\n")) {
		var e Event
		if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &e) != nil || e.Seq <= 0 {
			continue
		}
		held = append(held, e)
	}
	return held, nil
}

// since is the entries after the number after, at most limit (none when 0), under the shared
// lock.
func (j *journal) since(after int64, limit int) (EventsAnswer, error) {
	a := EventsAnswer{Events: []Event{}}
	if !fileExists(j.statePath()) && !fileExists(j.path()) {
		a.Truncated = after > 0
		return a, nil
	}
	unlock, err := flockWait(j.statePath(), false)
	if err != nil {
		return a, err
	}
	defer unlock()
	st, ok := j.readState()
	var held []Event
	if !ok || (limit > 0 && st.Last > after) {
		if held, err = j.read(); err != nil {
			return a, err
		}
	}
	if !ok {
		st = stateOf(held)
	}
	a.Last, a.Dropped = st.Last, st.Dropped
	// The entries held tell the floor too: a journal rewritten without its state (a crash)
	// starts past it; one emptied holds nothing up to the last number.
	if held != nil || (limit > 0 && st.Last > after) {
		floor := st.Last
		if len(held) > 0 {
			floor = held[0].Seq - 1
		}
		a.Dropped = max(a.Dropped, floor)
	}
	a.Truncated = after > st.Last || (after >= 0 && after < a.Dropped)
	from := after
	if a.Truncated && after <= st.Last {
		from = a.Dropped // an entry still held at or under the floor is answered as gone
	}
	for _, e := range held {
		if len(a.Events) == limit {
			break
		}
		if e.Seq > from {
			a.Events = append(a.Events, e)
		}
	}
	return a, nil
}

// recoverEvents is what a starting daemon appends to the entries held, in the boot boot (its
// boot.started; nil when its id cannot be read): each job started and never ended is lost (with
// the daemon, with the boot it ran in, or either when a boot id is not known); a reboot scheduled
// in this same boot and never done nor cancelled is cancelled, by the job that scheduled it. Two
// boots are the same, or others, only by two ids read (sameBoot): an empty id decides neither.
// The boot itself, when not recorded yet, is update's: it comes before these.
func recoverEvents(held []Event, boot *Event) []Event {
	id := bootIDOf(boot)
	var order []string
	jobs := map[string]Event{}
	var sched *Event
	for _, e := range held {
		switch e.Kind {
		case evJobStarted:
			if e.Job != nil {
				k := e.Job.UTC().Format(time.RFC3339Nano)
				if _, ok := jobs[k]; !ok {
					order = append(order, k)
				}
				jobs[k] = e
			}
		case evJobEnded, evJobLost:
			if e.Job != nil {
				delete(jobs, e.Job.UTC().Format(time.RFC3339Nano))
			}
		case evRebootScheduled:
			s := e
			sched = &s
		case evRebootCancelled:
			sched = nil
		case evBootStarted:
			if sched != nil && otherBoot(e.BootID, sched.BootID) {
				sched = nil
			}
		}
	}
	var out []Event
	for _, k := range order {
		e, ok := jobs[k]
		if !ok {
			continue
		}
		why := lostUnknown
		switch {
		case sameBoot(e.BootID, id):
			why = lostRestart
		case otherBoot(e.BootID, id):
			why = lostReboot
		}
		out = append(out, Event{Kind: evJobLost, Job: e.Job, JobKind: e.JobKind, Target: e.Target, Reason: why})
	}
	if sched != nil && sameBoot(sched.BootID, id) {
		out = append(out, Event{Kind: evRebootCancelled, Action: sched.Action, By: sched.By, Reason: cancelledByRestart})
	}
	return out
}

// sameBoot and otherBoot tell two boot ids name one boot, or two: only ids both read decide
// either — an id not read (empty) is neither the same boot as another nor another boot.
func sameBoot(a, b string) bool  { return a != "" && b != "" && a == b }
func otherBoot(a, b string) bool { return a != "" && b != "" && a != b }

// bootRecord is the boot.started of the boot a writer runs in: its id and the system Kairos
// booted, read where o says (o.BootIDFile, o.Cos; the system is none off Kairos), and the
// release rel the writer carries. Nil when the id cannot be read: such a boot is not recorded.
func bootRecord(o *Options, rel Version) *Event {
	id := o.bootID()
	if id == "" {
		return nil
	}
	fallback := fileExists(filepath.Join(o.cos(), upgradeFailure))
	return &Event{Kind: evBootStarted, BootID: id, Release: &rel, Slot: bootMode(o.cos()), Fallback: &fallback}
}

// bootIDOf is the id of the boot b records; empty for none.
func bootIDOf(b *Event) string {
	if b == nil {
		return ""
	}
	return b.BootID
}

// kernelBootID is where the kernel publishes this boot's id.
const kernelBootID = "/proc/sys/kernel/random/boot_id"

// journal is the install's event journal, which tells the daemon's long polls of each append.
func (h *Hostd) journal() *journal {
	return &journal{dir: h.O.Dir, notify: h.eventsAppended, boot: h.boot()}
}

// bootCache holds a boot's boot.started once one read of it succeeds: a read that fails (the
// boot id cannot be read) is not kept, and the next asks again once every has passed since it —
// never on every append.
type bootCache struct {
	mu  sync.Mutex
	rec *Event
	// next is when a read may be tried again after one failed.
	next time.Time
}

// bootRetry is how long, in the writer's seconds, a writer waits after a failed read of its
// boot's record before it reads again.
const bootRetry = 60

// get is the boot.started held, or read reads it, when every has passed (at now) since the last
// read that failed.
func (b *bootCache) get(now time.Time, every time.Duration, read func() *Event) *Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rec == nil && !now.Before(b.next) {
		if b.rec = read(); b.rec == nil {
			b.next = now.Add(every)
		}
	}
	return b.rec
}

// boot is the boot.started of the boot the daemon runs in, read until a read succeeds
// (bootRecord), at most once every bootRetry seconds.
func (h *Hostd) boot() *Event {
	return h.bootRec.get(time.Now(), bootRetry*cmp.Or(h.Unit, time.Second), func() *Event {
		m := h.R.Manifest
		return bootRecord(&h.O, Version{m.ProjectVersion, m.Revision})
	})
}

// event appends evs to the journal, in one append, and returns the journal's last number once
// they are written; a failure is logged (what happened stands) and returned.
func (h *Hostd) event(evs ...Event) (int64, error) {
	if h.onEvent != nil {
		h.onEvent(evs)
	}
	j := h.journal()
	last, err := j.append(evs...)
	if err != nil {
		h.O.logf("the event journal: %v", err)
	}
	return last, err
}

// eventsAppended wakes the long polls waiting.
func (h *Hostd) eventsAppended() {
	h.evMu.Lock()
	defer h.evMu.Unlock()
	if h.evCh != nil {
		close(h.evCh)
		h.evCh = nil
	}
}

// eventsWait is closed at the daemon's next append.
func (h *Hostd) eventsWait() <-chan struct{} {
	h.evMu.Lock()
	defer h.evMu.Unlock()
	if h.evCh == nil {
		h.evCh = make(chan struct{})
	}
	return h.evCh
}

// bootID is this boot's id; empty when it cannot be read.
func (h *Hostd) bootID() string { return bootIDOf(h.boot()) }

// startJournal records, as the daemon starts, this boot (first) and what the journal left open.
func (h *Hostd) startJournal() {
	j := h.journal()
	_, err := j.update(true, func(held []Event) []Event {
		return recoverEvents(held, j.boot)
	})
	if err != nil {
		h.O.logf("the event journal: %v", err)
	}
}

// events answers the entries after ?after=N (default 0), waiting up to ?wait=S seconds (0 to
// maxEventsWait) for one when there is none: the daemon's own appends wake it at once, the
// health gate's are found by reading the journal again every eventsPoll.
func (h *Hostd) events(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var after int64
	if s := q.Get("after"); s != "" {
		var err error
		if after, err = strconv.ParseInt(s, 10, 64); err != nil || after < 0 {
			replyErr(w, fail(http.StatusBadRequest, "after %q is not an event number", s))
			return
		}
	}
	wait := 0
	if s := q.Get("wait"); s != "" {
		var err error
		if wait, err = strconv.Atoi(s); err != nil || wait < 0 || wait > maxEventsWait {
			replyErr(w, fail(http.StatusBadRequest, "wait %q is not a number of seconds from 0 to %d", s, maxEventsWait))
			return
		}
	}
	unit := cmp.Or(h.Unit, time.Second)
	deadline := time.NewTimer(time.Duration(wait) * unit)
	defer deadline.Stop()
	tick := time.NewTicker(unit / 2)
	defer tick.Stop()
	for {
		appended := h.eventsWait()
		a, err := h.journal().since(after, maxEventsAnswer)
		if err != nil {
			replyErr(w, err)
			return
		}
		if len(a.Events) > 0 || a.Truncated || wait == 0 {
			reply(w, http.StatusOK, a)
			return
		}
		select {
		case <-appended:
		case <-tick.C:
		case <-deadline.C:
			reply(w, http.StatusOK, a)
			return
		case <-h.ctx.Done():
			reply(w, http.StatusOK, a)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// events prints the journal's entries after after; following, it then waits for more until ctx
// ends. Following, a daemon that cannot be reached (it restarts) is asked
// again from the same number, after a wait that doubles from one unit to thirty. Every request is
// bounded by its wait and the client's patience: a daemon that takes one and never answers is one that
// cannot be reached.
func (c *hostClient) events(ctx context.Context, errw io.Writer, after int64, follow, asJSON bool) error {
	backoff := time.Duration(0)
	for {
		wait := 0
		if follow {
			wait = maxEventsWait
		}
		var a EventsAnswer
		if err := c.within(ctx, wait, http.MethodGet, fmt.Sprintf("/v1/events?after=%d&wait=%d", after, wait), nil, &a); err != nil {
			if follow && ctx.Err() != nil {
				return nil
			}
			if !follow || !errors.As(err, new(unreachable)) {
				return err
			}
			if backoff == 0 {
				fmt.Fprintf(errw, "%v: asking again after %d\n", err, after)
			}
			backoff = min(max(2*backoff, c.second()), 30*c.second())
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			continue
		}
		if backoff != 0 {
			fmt.Fprintln(errw, "the host daemon answers again")
			backoff = 0
		}
		if a.Truncated {
			if after > a.Last {
				fmt.Fprintf(errw, "the journal holds nothing after %d (its last entry is %d): reading it from its start\n", after, a.Last)
				after = 0
			}
			if next := nextAfter(after, a); next > after {
				fmt.Fprintf(errw, "entries %d to %d are no longer kept\n", after+1, next)
				after = next
			}
		}
		for _, e := range a.Events {
			if asJSON {
				raw, err := json.Marshal(e)
				if err != nil {
					return err
				}
				fmt.Fprintf(c.out, "%s\n", raw)
			} else {
				printEvent(c.out, e)
			}
			after = e.Seq
		}
		if !follow && len(a.Events) == 0 && !a.Truncated {
			return nil
		}
	}
}

// printEvent prints e on one line: its number, time and kind, then its fields.
func printEvent(w io.Writer, e Event) {
	var b strings.Builder
	fmt.Fprintf(&b, "%d  %s  %s", e.Seq, e.At.UTC().Format(time.RFC3339), e.Kind)
	field := func(k, v string) {
		if v == "" {
			return
		}
		if strings.ContainsAny(v, " \t\n\"") {
			v = strconv.Quote(v)
		}
		fmt.Fprintf(&b, " %s=%s", k, v)
	}
	if e.Job != nil {
		field("job", e.Job.UTC().Format(time.RFC3339Nano))
	}
	field("job_kind", e.JobKind)
	field("target", e.Target)
	field("state", e.State)
	field("action", e.Action)
	if e.DrainSecs != nil {
		field("drain_secs", strconv.Itoa(*e.DrainSecs))
	}
	if e.Reboot != nil {
		field("reboot", e.Reboot.Action)
		field("drain_secs", strconv.Itoa(e.Reboot.DrainSecs))
	}
	if e.By != nil {
		field("by", e.By.UTC().Format(time.RFC3339Nano))
	}
	if e.Release != nil {
		field("release", e.Release.Version+"-r"+strconv.Itoa(e.Release.Revision))
	}
	field("slot", e.Slot)
	if e.Fallback != nil {
		field("fallback", strconv.FormatBool(*e.Fallback))
	}
	if e.OnTrial != nil {
		field("on_trial", strconv.FormatBool(*e.OnTrial))
	}
	field("verdict", e.Verdict)
	field("boot_id", e.BootID)
	field("reason", e.Reason)
	field("kept", e.Kept)
	field("error", e.Error)
	fmt.Fprintln(w, b.String())
}
