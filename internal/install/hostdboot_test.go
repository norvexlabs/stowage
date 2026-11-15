package install

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/command"
	"github.com/norvexlabs/stowage/internal/spec"
)

func TestHealthOverTheAPI(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	stackAnswers(e, healthyStack)
	d := daemon(e, &fakeHost{})
	p := &prober{status: 200}
	d.Probe = p.client(t)
	h := d.Handler()
	var got Health
	if code := call(t, h, "GET", "/v1/health", nil, &got); code != 200 || !got.Ready || got.Reason != "" {
		t.Errorf("%d %+v", code, got)
	}
	p.status = 503
	// Within its reuse, the verdict is the last check's; after it, a check of its own.
	if code := call(t, h, "GET", "/v1/health", nil, &got); code != 200 || !got.Ready {
		t.Errorf("reused: %d %+v", code, got)
	}
	time.Sleep(healthReuseSecs * d.Unit)
	if code := call(t, h, "GET", "/v1/health", nil, &got); code != 200 || got.Ready || !strings.Contains(got.Reason, "answered 503") {
		t.Errorf("%d %+v", code, got)
	}
	// Before the install.
	var e409 errOf
	if code := call(t, daemon(vmEnv(t, "1.0.0"), &fakeHost{}).Handler(), "GET", "/v1/health", nil, &e409); code != 409 {
		t.Errorf("before the install: %d %s", code, e409.Error)
	}
}

// Requests arriving while a check runs share it: the app socket polling GET /v1/health starts no
// check per request (with the command provider, a container each).
func TestHealthChecksOnceForConcurrentRequests(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	stackAnswers(e, healthyStack)
	d := daemon(e, &fakeHost{})
	d.Unit = time.Hour // every request within the verdict's reuse, however slow the machine
	p := &prober{status: 200}
	d.Probe = p.client(t)
	h := d.Handler()
	checks := func() (n int) {
		for _, c := range e.docker.ran("compose") {
			if strings.HasSuffix(strings.Join(c.Args, " "), " ps --all --format json") {
				n++
			}
		}
		return n
	}
	n := checks()
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var got Health
			if code := call(t, h, "GET", "/v1/health", nil, &got); code != 200 || !got.Ready {
				t.Errorf("%d %+v", code, got)
			}
		}()
	}
	wg.Wait()
	if c := checks() - n; c != 1 {
		t.Errorf("%d checks for 10 requests", c)
	}
}

// A verdict is not reused across a change of what the install runs (a service verb, a job's
// start or end), and one reused reads nothing of the install.
func TestHealthIsCheckedAgainAfterAChange(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	stackAnswers(e, healthyStack)
	stack := e.docker.answer
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if strings.HasSuffix(a, "config --services") {
			return []byte("api\npostgres\nmigrate\n"), true, nil
		}
		return stack(a)
	}
	d := daemon(e, &fakeHost{})
	d.Unit = time.Hour // every request within the verdict's reuse
	p := &prober{status: 200}
	d.Probe = p.client(t)
	h := d.Handler()
	asked := func() int {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.asked
	}
	var got Health
	call(t, h, "GET", "/v1/health", nil, &got)
	// Reused: the install is not read again, even gone.
	env := filepath.Join(e.dir, envName)
	if err := os.Rename(env, env+".away"); err != nil {
		t.Fatal(err)
	}
	if code := call(t, h, "GET", "/v1/health", nil, &got); code != 200 || !got.Ready || asked() != 1 {
		t.Errorf("reused: %d %+v, %d checks", code, got, asked())
	}
	if err := os.Rename(env+".away", env); err != nil {
		t.Fatal(err)
	}
	var e404 errOf
	if code := call(t, h, "POST", "/v1/services/api/restart", nil, &e404); code != 200 {
		t.Fatalf("restart: %d %s", code, e404.Error)
	}
	p.mu.Lock()
	p.status = 503
	p.mu.Unlock()
	if code := call(t, h, "GET", "/v1/health", nil, &got); code != 200 || got.Ready || asked() != 2 {
		t.Errorf("after a service verb: %d %+v, %d checks", code, got, asked())
	}
	j, err := d.startJob("backup-verify", "store:b", nil)
	if err != nil {
		t.Fatal(err)
	}
	d.finish(j, nil)
	call(t, h, "GET", "/v1/health", nil, &got)
	if asked() != 3 {
		t.Errorf("after a job: %d checks", asked())
	}
}

// A check that hangs ends at its bound, unhealthy: the next request starts another.
func TestAHangingHealthCheckEnds(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	stackAnswers(e, healthyStack)
	d := daemon(e, &fakeHost{}) // Unit 1 ms: the bound is healthWait ms
	d.Probe = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}}
	start := time.Now()
	var got Health
	if code := call(t, d.Handler(), "GET", "/v1/health", nil, &got); code != 200 || got.Ready {
		t.Errorf("%d %+v", code, got)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the check ran %s", took)
	}
}

// boots marks d's boot as mode, with the boot assessment vars and a passive system.
func boots(t *testing.T, d *Hostd, mode string, vars map[string]string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(d.O.Cos, mode+"_mode"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var kv []string
	for k, v := range vars {
		kv = append(kv, k+"="+v)
	}
	if err := editEnv(filepath.Join(d.State, assessFile), kv); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d.State, "cOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.State, passiveImage), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assessOf(t *testing.T, d *Hostd) map[string]string {
	t.Helper()
	v, err := assessment{dir: d.State}.read()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAnUpgradeAfterAFallbackGivesTheNewSystemATrial(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		vars   map[string]string
		edit   error
		marked bool // the job clears the mark
		want   string
	}{
		"after a fallback": {onTrial, nil, true, ""},
		"never fell back":  {map[string]string{assessEnable: ""}, nil, false, ""},
		"cannot clear":     {onTrial, fmt.Errorf("read-only"), false, "the next boot would skip it"},
	} {
		p := vmPublished(t, spec.Provider{Name: "none"}, nil)
		p.release("2.0.0", 1, nil)
		host := &fakeHost{editErr: c.edit}
		d := daemon(p.env, host)
		boots(t, d, "passive", c.vars)
		hh := d.Handler()
		if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, nil); code != 202 {
			t.Fatalf("%s: %d", name, code)
		}
		j := waitJob(t, hh)
		v := assessOf(t, d)
		if c.want != "" {
			if j.State != "failed" || !strings.Contains(j.Error, c.want) {
				t.Errorf("%s: %+v", name, j)
			}
			time.Sleep(50 * time.Millisecond)
			if strings.Contains(fmt.Sprint(host.calls()), "systemctl reboot") {
				t.Errorf("%s: rebooted", name)
			}
			continue
		}
		if j.State != "done" {
			t.Errorf("%s: %+v", name, j)
		}
		if !fileExists(filepath.Join(d.Run, osWritten)) {
			t.Errorf("%s: the written OS is not marked", name)
		}
		if c.marked != (v[assessTentative] == "" && c.vars[assessTentative] != "") || v[assessEnable] != c.vars[assessEnable] {
			t.Errorf("%s: %v", name, v)
		}
		if !c.marked && strings.Contains(fmt.Sprint(host.calls()), "grub2-editenv") {
			t.Errorf("%s: wrote the assessment: %v", name, host.calls())
		}
	}
}

// upgradedVM is a VM install upgraded from 1.0.0 to 2.0.0 at schema 5, whose daemon carries 2.0.0
// and boots the active system on a passive one.
func upgradedVM(t *testing.T) (*env, *Hostd, *fakeHost) {
	t.Helper()
	e := vmEnv(t, "1.0.0")
	e.docker.dump = "DUMP"
	e.schemaAt("5", nil, true)
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	e.r, e.self = bundledInstaller(t, "2.0.0", 1, nil)
	in := e.installer(vm)
	in.O.Exec = func(context.Context, string, []string) error { return nil }
	if err := in.Boot(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	host := &fakeHost{}
	d := daemon(e, host)
	boots(t, d, "active", nil)
	return e, d, host
}

func TestRollbackChoosesThePassiveSystemThenReboots(t *testing.T) {
	t.Parallel()
	e, d, host := upgradedVM(t)
	e.schemaAt("6", nil, true)
	hh := d.Handler()
	var p RollbackPlan
	if code := call(t, hh, "GET", "/v1/rollback", nil, &p); code != 200 || p.From != (Version{"2.0.0", 1}) || p.To != (Version{"1.0.0", 1}) {
		t.Fatalf("%d %+v", code, p)
	}
	before := read(t, filepath.Join(e.dir, ".env"))
	var e409 errOf
	e409 = errOf{}
	if code := call(t, hh, "POST", "/v1/rollback", RollbackRequest{Confirm: &p.From, DryRun: true}, &e409); code != 409 || !strings.Contains(e409.Error, "which was confirmed") || !e409.Refused {
		t.Fatalf("a dry run confirming another release: %d %s", code, e409.Error)
	}
	// A dry run refuses volumes a restore left half restored, as the rollback does.
	split := filepath.Join(e.dir, volumesSplitFile)
	if err := os.WriteFile(split, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e409 = errOf{}
	if code := call(t, hh, "POST", "/v1/rollback", RollbackRequest{DryRun: true}, &e409); code != 409 || !strings.Contains(e409.Error, "half restored") || !e409.Refused {
		t.Fatalf("a dry run with volumes half restored: %d %s", code, e409.Error)
	}
	if err := os.Remove(split); err != nil {
		t.Fatal(err)
	}
	// The boot not judged yet: going back is refused until it is.
	host.mu.Lock()
	host.judge = "activating"
	host.mu.Unlock()
	e409 = errOf{}
	if code := call(t, hh, "POST", "/v1/rollback", RollbackRequest{DryRun: true}, &e409); code != 409 || !strings.Contains(e409.Error, "not judged this boot yet") || !e409.Refused {
		t.Fatalf("a dry run before the judgement: %d %+v", code, e409)
	}
	host.mu.Lock()
	host.judge = ""
	host.mu.Unlock()
	// A system written that no boot ran yet: going back is refused until a reboot.
	if err := os.WriteFile(filepath.Join(d.Run, osWritten), []byte("notes:3.0.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e409 = errOf{}
	if code := call(t, hh, "POST", "/v1/rollback", RollbackRequest{DryRun: true}, &e409); code != 409 || !strings.Contains(e409.Error, "reboot first") || !e409.Refused {
		t.Fatalf("a dry run with an OS written: %d %+v", code, e409)
	}
	if err := os.Remove(filepath.Join(d.Run, osWritten)); err != nil {
		t.Fatal(err)
	}
	// A dry run refuses what claiming the job slot refuses.
	d.mu.Lock()
	d.rebooting = true
	d.mu.Unlock()
	e409 = errOf{}
	if code := call(t, hh, "POST", "/v1/rollback", RollbackRequest{Confirm: &p.To, DryRun: true}, &e409); code != 409 || !strings.Contains(e409.Error, "a reboot is scheduled") || !e409.Refused {
		t.Fatalf("a dry run with a reboot scheduled: %d %s", code, e409.Error)
	}
	d.mu.Lock()
	d.rebooting = false
	d.mu.Unlock()
	// A dry run answers the plan and starts nothing.
	var dry RollbackPlan
	if code := call(t, hh, "POST", "/v1/rollback", RollbackRequest{Confirm: &p.To, DryRun: true}, &dry); code != 200 || dry.From != p.From || dry.To != p.To || len(dry.Checks) == 0 {
		t.Fatalf("the dry run: %d %+v", code, dry)
	}
	var idle JobStatus
	if call(t, hh, "GET", "/v1/job", nil, &idle); idle.State != "idle" || len(host.calls()) > 0 || read(t, filepath.Join(e.dir, ".env")) != before {
		t.Fatalf("a dry run started %+v, ran %v", idle, host.calls())
	}
	if code := call(t, hh, "POST", "/v1/rollback", RollbackRequest{Confirm: &p.To}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	j := waitJob(t, hh)
	if j.State != "done" || j.Kind != "rollback" || j.Target != "1.0.0 (revision 1)" {
		t.Fatalf("%+v", j)
	}
	log := strings.Join(j.Log, "\n")
	if !strings.Contains(log, "the database is at 6; 1.0.0 reads past the newer migrations") || !strings.Contains(log, "a run would go back") {
		t.Errorf("the checks are not in the log: %s", log)
	}
	// The dry run answered the lines the job logs.
	for _, l := range dry.Checks {
		if !strings.Contains(log, l) {
			t.Errorf("the dry run's check %q is not in the job's log", l)
		}
	}
	var st Status
	call(t, hh, "GET", "/v1/status", nil, &st)
	if !st.RebootPending {
		t.Error("no reboot pending")
	}
	// The job ends the grace before its marks and its reboot.
	waitCall(t, host, "systemctl reboot")
	if v := assessOf(t, d); v[assessEnable] != "yes" || v[assessTentative] != "yes" {
		t.Errorf("the next boot does not choose the passive system: %v", v)
	}
	calls := fmt.Sprint(host.calls())
	if !strings.Contains(calls, "systemctl reboot") || strings.Index(calls, "grub2-editenv") > strings.Index(calls, "systemctl reboot") {
		t.Errorf("the marks are not written before the reboot: %s", calls)
	}
	// The install's lock is held to the reboot.
	runtime.GC()
	if _, err := lockFile(filepath.Join(e.dir, lifecycleLock)); !errors.Is(err, errLocked) {
		t.Errorf("the lock is free after the rollback: %v", err)
	}
	runtime.KeepAlive(d) // the daemon, which holds it, runs on
	// The boot of the passive system goes back; the daemon changes nothing of the install.
	if read(t, filepath.Join(e.dir, ".env")) != before {
		t.Error("the daemon changed the install")
	}
}

// A daemon stopping in a rollback's grace, the job ended, writes no mark and runs no reboot, and
// journals that the reboot does not come before Serve returns (a shutdown has no later start to
// tell it): the next boot is as it was, and the install's lock is released.
func TestARollbackWhoseDaemonStopsInItsGraceArmsNothing(t *testing.T) {
	t.Parallel()
	e, d, host := upgradedVM(t)
	reached, release := make(chan struct{}), make(chan struct{})
	d.onWait = func(what string) {
		if what == "reboot grace" {
			close(reached)
			<-release
		}
	}
	boots(t, d, "active", map[string]string{assessEnable: "no", assessTentative: "no"})
	dir, err := os.MkdirTemp("", "hostd") // a unix socket's path is bounded
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "run", "host.sock")
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	withBootID(t, d)
	go func() { done <- d.Serve(ctx, sock) }()
	waitSocket(t, sock)
	if out, err := ctl(e, sock, "rollback", "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	<-reached
	stop()
	select {
	case err := <-done:
		t.Fatalf("Serve returned before the rollback in its grace ended: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if e := ofKind(held(t, d.O.Dir), evRebootCancelled); len(e) != 1 || e[0].Reason != "the daemon stopped before the reboot: nothing changed" {
		t.Errorf("the reboot.cancelled %+v", e)
	}
	if calls := fmt.Sprint(host.calls()); strings.Contains(calls, "grub2-editenv") || strings.Contains(calls, "systemctl reboot") {
		t.Errorf("a stopped daemon armed its reboot: %s", calls)
	}
	if v := assessOf(t, d); v[assessEnable] != "no" || v[assessTentative] != "no" {
		t.Errorf("the marks changed: %v", v)
	}
	unlock, err := lockFile(filepath.Join(d.O.Dir, lifecycleLock))
	if err != nil {
		t.Fatalf("the lock is held after the stop: %v", err)
	}
	unlock()
}

// Marks written whose partition stays read-write are written: the reboot runs, the job's log
// telling the partition's state.
func TestARollbackWhosePartitionStaysWritableReboots(t *testing.T) {
	t.Parallel()
	_, d, host := upgradedVM(t)
	host.roErr = fmt.Errorf("busy")
	boots(t, d, "active", map[string]string{assessEnable: "no", assessTentative: "no"})
	hh := d.Handler()
	if code := call(t, hh, "POST", "/v1/rollback", RollbackRequest{}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	waitCall(t, host, "systemctl reboot")
	if v := assessOf(t, d); v[assessEnable] != "yes" || v[assessTentative] != "yes" {
		t.Errorf("the marks are not kept for the reboot: %v", v)
	}
	if j := waitJob(t, hh); !strings.Contains(strings.Join(j.Log, "\n"), "the boot assessment is marked, its partition left read-write") {
		t.Errorf("%v", j.Log)
	}
	if e := ofKind(held(t, d.O.Dir), evRebootCancelled); len(e) != 0 {
		t.Errorf("cancelled: %+v", e)
	}
}

// A stop that comes as the marks are written runs no reboot: the marks are put back.
func TestARollbackStoppedAsItMarksPutsThemBack(t *testing.T) {
	t.Parallel()
	_, d, host := upgradedVM(t)
	ctx, stop := context.WithCancel(context.Background())
	d.ctx = ctx
	host.onEdit = func(vars []string) {
		if slices.Contains(vars, assessEnable+"=yes") {
			stop()
		}
	}
	boots(t, d, "active", map[string]string{assessEnable: "no", assessTentative: "no"})
	hh := d.Handler()
	if code := call(t, hh, "POST", "/v1/rollback", RollbackRequest{}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	if e := waitEvent(t, d.O.Dir, evRebootCancelled); e.Reason != "the daemon stopped before the reboot: the boot assessment is put back as it was, nothing changed" {
		t.Errorf("the reboot.cancelled's reason %q", e.Reason)
	}
	if strings.Contains(fmt.Sprint(host.calls()), "systemctl reboot") {
		t.Errorf("a stopped daemon rebooted: %v", host.calls())
	}
	if v := assessOf(t, d); v[assessEnable] != "no" || v[assessTentative] != "no" {
		t.Errorf("the marks are not put back: %v", v)
	}
}

// A stopping daemon starts no job: a rollback asked then is refused, never counted after Serve's
// wait began.
func TestAStoppingDaemonStartsNoJob(t *testing.T) {
	t.Parallel()
	_, d, host := upgradedVM(t)
	d.mu.Lock()
	d.stopping = true
	d.mu.Unlock()
	var body errOf
	if code := call(t, d.Handler(), "POST", "/v1/rollback", RollbackRequest{}, &body); code != http.StatusServiceUnavailable || !strings.Contains(body.Error, "the host daemon is stopping") ||
		!body.Refused || body.Fix != "wait for the host daemon to start again, then run again" {
		t.Fatalf("%d %+v", code, body)
	}
	if strings.Contains(fmt.Sprint(host.calls()), "grub2-editenv") {
		t.Errorf("a refused rollback wrote: %v", host.calls())
	}
	waited := make(chan struct{})
	go func() { d.rollbacks.Wait(); close(waited) }()
	select { // nothing counted
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("a refused rollback was counted")
	}
}

func TestRollbackRefusals(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		setup func(t *testing.T, e *env, d *Hostd, h *fakeHost)
		body  RollbackRequest
		want  string
	}{
		"barrier": {func(t *testing.T, e *env, d *Hostd, h *fakeHost) { e.schemaAt("7", []string{"7 (drop x)"}, true) }, RollbackRequest{},
			"the database carries 7 (drop x)"},
		"passive": {func(t *testing.T, e *env, d *Hostd, h *fakeHost) {
			_ = os.Remove(filepath.Join(d.O.Cos, "active_mode"))
			boots(t, d, "passive", nil)
		}, RollbackRequest{}, "runs the passive system already"},
		"recovery": {func(t *testing.T, e *env, d *Hostd, h *fakeHost) {
			_ = os.Remove(filepath.Join(d.O.Cos, "active_mode"))
			boots(t, d, "recovery", nil)
		}, RollbackRequest{}, `runs the "recovery" system`},
		"no passive": {func(t *testing.T, e *env, d *Hostd, h *fakeHost) { _ = os.Remove(filepath.Join(d.State, passiveImage)) }, RollbackRequest{},
			"no passive system to go back to"},
		"no previous": {func(t *testing.T, e *env, d *Hostd, h *fakeHost) { _ = os.RemoveAll(filepath.Join(e.dir, PrevDir)) }, RollbackRequest{},
			"no previous release"},
		"another os": {func(t *testing.T, e *env, d *Hostd, h *fakeHost) { d.R, _ = bundledInstaller(t, "3.0.0", 1, nil) }, RollbackRequest{},
			"the install runs 2.0.0 (revision 1), not 3.0.0 (revision 1)"},
		"not confirmed": {func(t *testing.T, e *env, d *Hostd, h *fakeHost) {}, RollbackRequest{Confirm: &Version{"0.9.0", 1}},
			"carries 1.0.0 (revision 1), not 0.9.0 (revision 1)"},
		"upgrade written": {func(t *testing.T, e *env, d *Hostd, h *fakeHost) {
			_ = os.WriteFile(filepath.Join(d.Run, osWritten), nil, 0o600)
		},
			RollbackRequest{}, "an upgrade wrote (or began to write)"},
		"gate queued": {func(t *testing.T, e *env, d *Hostd, h *fakeHost) { h.judge = "inactive" }, RollbackRequest{},
			"has not judged this boot yet (notes-health.service is inactive): wait for it, or `systemctl start notes-health.service`"},
		"judging": {func(t *testing.T, e *env, d *Hostd, h *fakeHost) { h.judge = "activating" }, RollbackRequest{},
			"the health gate has not judged this boot yet"},
		"booting": {func(t *testing.T, e *env, d *Hostd, h *fakeHost) { h.boot = "activating" }, RollbackRequest{},
			"the boot is still bringing the install up"},
	} {
		e, d, host := upgradedVM(t)
		c.setup(t, e, d, host)
		// Each is a refusal (nothing changed), its plan's GET and a dry run's too.
		dry := c.body
		dry.DryRun = true
		for _, how := range []struct {
			method string
			body   any
		}{{"POST", c.body}, {"POST", dry}, {"GET", nil}} {
			if how.method == "GET" && (c.body.Confirm != nil || slices.Contains([]string{"gate queued", "judging", "booting"}, name)) {
				continue // the GET confirms nothing, and shows the plan whatever the boot's state
			}
			var got errOf
			if code := call(t, d.Handler(), how.method, "/v1/rollback", how.body, &got); code != 409 || !strings.Contains(got.Error, c.want) || !got.Refused {
				t.Errorf("%s (%s %+v): %d %+v, want %q", name, how.method, how.body, code, got, c.want)
			}
		}
		if v := assessOf(t, d); v[assessTentative] != "" {
			t.Errorf("%s: the assessment changed: %v", name, v)
		}
		if strings.Contains(fmt.Sprint(host.calls()), "systemctl reboot") {
			t.Errorf("%s: rebooted", name)
		}
	}
}

func TestAnUpgradeWaitsForTheGatesJudgement(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.release("2.0.0", 1, nil)
	host := &fakeHost{judge: "activating"}
	var got errOf
	if code := call(t, daemon(p.env, host).Handler(), "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, &got); code != 409 || !strings.Contains(got.Error, "not judged this boot yet") || !got.Refused {
		t.Errorf("%d %+v", code, got)
	}
	// The gate's state that cannot be read is a failure, not a refusal: dry run or not.
	for _, dry := range []bool{false, true} {
		got = errOf{}
		bus := daemon(p.env, &fakeHost{judge: "<error>"}).Handler()
		if code := call(t, bus, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", DryRun: dry}, &got); code < 500 || got.Refused || !strings.Contains(got.Error, "Failed to connect to bus") {
			t.Errorf("an unreadable gate (dry run %t): %d %+v", dry, code, got)
		}
	}
	// Once judged (failed is judged too), it runs.
	host.judge = "failed"
	h := daemon(p.env, host).Handler()
	if code := call(t, h, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, &got); code != 202 {
		t.Errorf("%d %s", code, got.Error)
	}
	waitJob(t, h) // its files are in the test's directories, removed after it
}

func TestStatusTellsTheFallbackAndTheLastFailedBoot(t *testing.T) {
	t.Parallel()
	g := newGate(t, "", nil, false)
	g.probe.status = 503
	yes, no := true, false
	// A healthy verdict on trial before the failure, in the journal: it does not hide it, whatever
	// its clock says.
	if _, err := (&journal{dir: g.dir}).append(Event{Kind: evBootJudged, BootID: "an-earlier-boot", Verdict: verdictHealthy, OnTrial: &yes, At: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := g.gate.Run(context.Background()); err == nil {
		t.Fatal("healthy")
	}
	d := daemon(g.env, &fakeHost{})
	boots(t, d, "passive", onTrial)
	if err := os.WriteFile(filepath.Join(d.O.Cos, upgradeFailure), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	var st Status
	call(t, d.Handler(), "GET", "/v1/status", nil, &st)
	if !st.Fallback || st.Boot != "passive" || st.FailedBoot == nil || st.FailedBoot.Release != (Version{"1.0.0", 1}) || st.FailedBoot.Seq < 1 {
		t.Errorf("%+v", st)
	}
	// Told until a later boot on trial is judged healthy: the fallback's healthy boot, or one
	// judged otherwise, keeps it, and the clock decides nothing.
	fb := st.FailedBoot
	for _, c := range []struct {
		verdict string
		onTrial *bool
		told    bool
	}{
		{verdictHealthy, &no, true}, // the passive system the boot fell back to
		{verdictHealthy, nil, true},
		{verdictWentBack, &yes, true},
		{verdictHealthy, &yes, false}, // a later upgrade's boot
	} {
		if _, err := (&journal{dir: d.O.Dir}).append(Event{Kind: evBootJudged, BootID: "another-boot", Verdict: c.verdict, OnTrial: c.onTrial, At: fb.At.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		var st Status
		call(t, d.Handler(), "GET", "/v1/status", nil, &st)
		if (st.FailedBoot != nil) != c.told {
			t.Errorf("%s on trial %v: failed boot %+v", c.verdict, c.onTrial, st.FailedBoot)
		}
	}
	var none Status
	call(t, daemon(installedVM(t), &fakeHost{}).Handler(), "GET", "/v1/status", nil, &none)
	if none.Fallback || none.FailedBoot != nil {
		t.Errorf("%+v", none)
	}
}

// A failure whose own verdict the journal lacks is still told over a healthy verdict on trial the
// journal holds before it: the order is the journal's.
func TestAFailedBootIsToldOverAnEarlierHealthyVerdict(t *testing.T) {
	t.Parallel()
	g := newGate(t, "", nil, false)
	yes := true
	if _, err := (&journal{dir: g.dir}).append(Event{Kind: evBootJudged, BootID: "an-earlier-boot", Verdict: verdictHealthy, OnTrial: &yes, At: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.gate.keep(context.Background(), errors.New("api is down"), ""); err != nil {
		t.Fatal(err)
	}
	var st Status
	call(t, daemon(g.env, &fakeHost{}).Handler(), "GET", "/v1/status", nil, &st)
	if st.FailedBoot == nil || st.FailedBoot.Reason != "api is down" {
		t.Errorf("%+v", st.FailedBoot)
	}
}

func TestStatusTellsABootOnTrial(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		mode  string
		marks map[string]string
		want  bool
	}{
		"new system, not judged": {"active", onTrial, true},
		"judged":                 {"active", map[string]string{assessEnable: "", assessTentative: ""}, false},
		"fallback":               {"passive", onTrial, false},
	} {
		d := daemon(installedVM(t), &fakeHost{})
		boots(t, d, c.mode, c.marks)
		var st Status
		call(t, d.Handler(), "GET", "/v1/status", nil, &st)
		if st.OnTrial != c.want {
			t.Errorf("%s: on_trial %v", name, st.OnTrial)
		}
	}
}

func TestAFailedRollbackPutsTheMarksBack(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		set func(*fakeHost)
		// state is the job's: a reboot that fails comes after the job's end, which stands.
		state string
		// cancelled is in the job's log and the reboot.cancelled's reason (state done); putBack:
		// the marks are as they were.
		cancelled string
		putBack   bool
	}{
		"reboot": {func(h *fakeHost) { h.rebootErr = fmt.Errorf("inhibited") }, "done",
			"systemctl reboot failed: inhibited: the boot assessment is put back as it was", true},
		"reboot, the marks kept": {func(h *fakeHost) { h.rebootErr, h.editAfterReboot = fmt.Errorf("inhibited"), fmt.Errorf("read-only") }, "done",
			"systemctl reboot failed: inhibited; putting the boot assessment back failed too, so the next boot may choose the passive system", false},
		// The marks put back, the partition alone left read-write: said so, not as a failure.
		"reboot, read-only again": {func(h *fakeHost) { h.rebootErr, h.roErr = fmt.Errorf("inhibited"), fmt.Errorf("busy") }, "done",
			"systemctl reboot failed: inhibited: the boot assessment is put back as it was, its partition left read-write (remounting", true},
	} {
		_, d, host := upgradedVM(t)
		c.set(host)
		boots(t, d, "active", map[string]string{assessEnable: "no", assessTentative: "no"})
		hh := d.Handler()
		if code := call(t, hh, "POST", "/v1/rollback", RollbackRequest{}, nil); code != 202 {
			t.Fatalf("%s: %d", name, code)
		}
		if j := waitJob(t, hh); j.State != c.state {
			t.Errorf("%s: %+v", name, j)
		}
		if c.state == "done" {
			if e := waitEvent(t, d.O.Dir, evRebootCancelled); !strings.Contains(e.Reason, c.cancelled) {
				t.Errorf("%s: the reboot.cancelled's reason %q, want %q", name, e.Reason, c.cancelled)
			}
			if j := waitJob(t, hh); !strings.Contains(strings.Join(j.Log, "\n"), c.cancelled) {
				t.Errorf("%s: the job's log does not tell the reboot failed: %v", name, j.Log)
			}
		}
		if v := assessOf(t, d); c.putBack != (v[assessEnable] == "no" && v[assessTentative] == "no") {
			t.Errorf("%s: the marks put back %v: %v", name, !c.putBack, v)
		}
		var st Status
		call(t, hh, "GET", "/v1/status", nil, &st)
		if st.RebootPending {
			t.Errorf("%s: a reboot is still pending", name)
		}
	}
}

// A write of the boot assessment that hangs ends at its bound (assessWrite, in Unit): the
// rollback's arming fails, and the marks are put back under a context of their own, as the one
// that ran out would fail them too; an upgrade's clearing of the last attempt fails the same way.
func TestAHangingAssessmentWriteEndsAtItsBound(t *testing.T) {
	t.Parallel()
	_, d, host := upgradedVM(t)
	boots(t, d, "active", map[string]string{assessEnable: "no", assessTentative: "no"})
	host.mu.Lock()
	host.editHang = 1
	host.mu.Unlock()
	hh := d.Handler()
	if code := call(t, hh, "POST", "/v1/rollback", RollbackRequest{}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	if j := waitJob(t, hh); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	want := "marking the boot assessment failed: grub2-editenv " + filepath.Join(d.State, assessFile) + ": context deadline exceeded: the boot assessment is put back as it was"
	if e := waitEvent(t, d.O.Dir, evRebootCancelled); !strings.Contains(e.Reason, want) {
		t.Errorf("reason %q, want %q", e.Reason, want)
	}
	if v := assessOf(t, d); v[assessEnable] != "no" || v[assessTentative] != "no" {
		t.Errorf("not put back: %v", v)
	}
	if strings.Contains(fmt.Sprint(host.calls()), "systemctl reboot") {
		t.Error("rebooted")
	}
	// The upgrade's clearing of the last attempt's mark.
	boots(t, d, "active", map[string]string{assessTentative: "yes"})
	host.mu.Lock()
	host.editHang = 1
	host.mu.Unlock()
	if err := d.retry(&job{}); err == nil || !strings.Contains(err.Error(), "the next boot would skip it") || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("retry: %v", err)
	}
}

func TestTheProbeFollowsNoRedirectAndNoProxy(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ready" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	resp, err := probes.Get(srv.URL + "/ready")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("answered %d", resp.StatusCode)
	}
	if tr := probes.Transport.(*http.Transport); tr.Proxy != nil || !tr.DisableKeepAlives {
		t.Error("the probes use a proxy or keep connections")
	}
}

func TestTheGateRemovesLeftHealthFiles(t *testing.T) {
	t.Parallel()
	g := newGate(t, "", nil, false)
	left, live := filepath.Join(g.dir, ".stowage-health-dead.yaml"), filepath.Join(g.dir, ".stowage-health-live.yaml")
	for _, f := range []string{left, live} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// No run of the longest health command (and its container's removal) still holds a file the
	// gate takes for left.
	if longest := time.Duration(command.MaxTimeoutS)*time.Second + 30*time.Second; staleHealthFile <= longest {
		t.Errorf("staleHealthFile %s is within a health command's run, %s", staleHealthFile, longest)
	}
	old := time.Now().Add(-staleHealthFile - time.Minute)
	if err := os.Chtimes(left, old, old); err != nil {
		t.Fatal(err)
	}
	if err := g.gate.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(left); err == nil {
		t.Error("an interrupted run's file is left")
	}
	if _, err := os.Stat(live); err != nil {
		t.Error("a running call's file is removed")
	}
}

func TestASecondUpgradeWaitsForTheBootOfTheFirst(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.release("2.0.0", 1, nil)
	host := &fakeHost{}
	d := daemon(p.env, host)
	hh := d.Handler()
	no := false
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", Reboot: &no}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	waitJob(t, hh)
	var got errOf
	for _, dry := range []bool{false, true} {
		got = errOf{}
		if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", DryRun: dry}, &got); code != 409 || !strings.Contains(got.Error, "which no boot ran yet: reboot first") || !got.Refused {
			t.Errorf("dry run %t: %d %+v", dry, code, got)
		}
	}
	// A mark that cannot be read refuses too.
	if err := os.Remove(filepath.Join(d.Run, osWritten)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(d.Run, osWritten), 0o700); err != nil {
		t.Fatal(err)
	}
	// A failure to read it, not a refusal: dry run or not.
	for _, dry := range []bool{false, true} {
		got = errOf{}
		if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", DryRun: dry}, &got); code != 409 || !strings.Contains(got.Error, "is not known") || got.Refused {
			t.Errorf("unreadable (dry run %t): %d %+v", dry, code, got)
		}
	}
}

func TestRollbackArmsNothingDuringItsDrainAndYieldsToALock(t *testing.T) {
	t.Parallel()
	e, d, host := upgradedVM(t)
	d.Unit = 30 * time.Millisecond // a 900 ms drain
	hh := d.Handler()
	if code := call(t, hh, "POST", "/v1/rollback", RollbackRequest{}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	time.Sleep(50 * time.Millisecond)
	if v := assessOf(t, d); v[assessTentative] != "" || v[assessEnable] != "" {
		t.Errorf("armed during the drain: %v", v)
	}
	// An operator's run starts during the drain.
	unlock, err := lockFile(filepath.Join(e.dir, lifecycleLock))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	j := waitJob(t, hh)
	if j.State != "failed" || !strings.Contains(j.Error, "nothing changed: ") || !strings.Contains(j.Error, "already running") {
		t.Errorf("%+v", j)
	}
	if v := assessOf(t, d); v[assessTentative] != "" {
		t.Errorf("armed: %v", v)
	}
	if strings.Contains(fmt.Sprint(host.calls()), "systemctl reboot") {
		t.Error("rebooted")
	}
}

func TestAFailedUpgradeStillMarksTheSlotsAsMoved(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.release("2.0.0", 1, nil)
	d := daemon(p.env, &fakeHost{err: fmt.Errorf("after-upgrade failed")})
	hh := d.Handler()
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	if j := waitJob(t, hh); j.State != "failed" {
		t.Fatalf("%+v", j)
	}
	if d.osWritten() == nil {
		t.Error("a failed upgrade is not marked")
	}
	var got errOf
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, &got); code != 409 || !strings.Contains(got.Error, "reboot first") {
		t.Errorf("an upgrade after a failed one: %d %s", code, got.Error)
	}
}

func TestStatusCarriesTheBootID(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	d := daemon(e, &fakeHost{})
	d.Unit = time.Hour // the retry's wait is not over until the test says so
	d.O.BootIDFile = filepath.Join(t.TempDir(), "boot_id")
	var st Status
	call(t, d.Handler(), "GET", "/v1/status", nil, &st)
	if st.BootID != "" {
		t.Errorf("no boot id file, but %q", st.BootID)
	}
	if err := os.WriteFile(d.O.BootIDFile, []byte("6f1c2d3e-0000-4000-8000-000000000001\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A read that failed is tried again only once bootRetry passed: never at every call.
	call(t, d.Handler(), "GET", "/v1/status", nil, &st)
	if st.BootID != "" {
		t.Errorf("read again before its retry: %q", st.BootID)
	}
	// A read that failed is not kept: the same daemon reads the id once it can.
	d.bootRec.next = time.Time{}
	call(t, d.Handler(), "GET", "/v1/status", nil, &st)
	if st.BootID != "6f1c2d3e-0000-4000-8000-000000000001" {
		t.Errorf("not read again: %q", st.BootID)
	}
	// A read that succeeded is kept: a boot's id does not change.
	if err := os.WriteFile(d.O.BootIDFile, []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	call(t, d.Handler(), "GET", "/v1/status", nil, &st)
	if st.BootID != "6f1c2d3e-0000-4000-8000-000000000001" {
		t.Errorf("read again: %q", st.BootID)
	}
}
