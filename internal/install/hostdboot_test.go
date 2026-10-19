package install

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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
	if code := call(t, h, "GET", "/v1/health", nil, &got); code != 200 || got.Ready || !strings.Contains(got.Reason, "answered 503") {
		t.Errorf("%d %+v", code, got)
	}
	// Before the install.
	var e409 errOf
	if code := call(t, daemon(vmEnv(t, "1.0.0"), &fakeHost{}).Handler(), "GET", "/v1/health", nil, &e409); code != 409 {
		t.Errorf("before the install: %d %s", code, e409.Error)
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
	if v := assessOf(t, d); v[assessEnable] != "yes" || v[assessTentative] != "yes" {
		t.Errorf("the next boot does not choose the passive system: %v", v)
	}
	var st Status
	call(t, hh, "GET", "/v1/status", nil, &st)
	if !st.RebootPending {
		t.Error("no reboot pending")
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
		var got errOf
		if code := call(t, d.Handler(), "POST", "/v1/rollback", c.body, &got); code != 409 || !strings.Contains(got.Error, c.want) {
			t.Errorf("%s: %d %q, want %q", name, code, got.Error, c.want)
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
	if code := call(t, daemon(p.env, host).Handler(), "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, &got); code != 409 || !strings.Contains(got.Error, "not judged this boot yet") {
		t.Errorf("%d %s", code, got.Error)
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
	if !st.Fallback || st.Boot != "passive" || st.FailedBoot == nil || st.FailedBoot.Release != (Version{"1.0.0", 1}) {
		t.Errorf("%+v", st)
	}
	var none Status
	call(t, daemon(installedVM(t), &fakeHost{}).Handler(), "GET", "/v1/status", nil, &none)
	if none.Fallback || none.FailedBoot != nil {
		t.Errorf("%+v", none)
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
		"read-only again": {func(h *fakeHost) { h.roErr = fmt.Errorf("busy") }, "failed", "", true},
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
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, &got); code != 409 || !strings.Contains(got.Error, "which no boot ran yet: reboot first") {
		t.Errorf("%d %s", code, got.Error)
	}
	// A mark that cannot be read refuses too.
	if err := os.Remove(filepath.Join(d.Run, osWritten)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(d.Run, osWritten), 0o700); err != nil {
		t.Fatal(err)
	}
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, &got); code != 409 || !strings.Contains(got.Error, "is not known") {
		t.Errorf("unreadable: %d %s", code, got.Error)
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
