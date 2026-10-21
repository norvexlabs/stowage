package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/spec"
)

// fakeStart is the start of the jobs fake daemons answer.
var fakeStart = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

// served runs h on a socket of its own until the test ends, and returns the socket.
func served(t *testing.T, h *Hostd) string {
	t.Helper()
	// A short path: a unix socket's is bounded (108 bytes), which a test's temporary directory may pass.
	dir, err := os.MkdirTemp("", "hostd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "run", "host.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx, sock) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	for range 2000 {
		if _, err := os.Stat(sock); err == nil {
			return sock
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the daemon did not start")
	return ""
}

// ctl runs the VM's <short>-stow of e with args against the daemon on sock.
func ctl(e *env, sock string, args ...string) (string, error) {
	return ctlCtx(context.Background(), e, sock, args...)
}

// ctlCtx is ctl until ctx ends (an interrupt).
func ctlCtx(ctx context.Context, e *env, sock string, args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := vmCommand(e.r, e.self, &out, &errb, func(bool) Prompter { return nil }, time.Millisecond)
	cmd.SetArgs(append(args, "--socket", sock))
	err := cmd.ExecuteContext(ctx)
	return out.String() + errb.String(), err
}

func TestTheSocketIsRootsAloneWithoutTheGroup(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	sock := served(t, daemon(e, &fakeHost{}))
	for p, want := range map[string]os.FileMode{sock: 0o600, filepath.Dir(sock): 0o700} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s: %v, want %v", p, fi.Mode().Perm(), want)
		}
	}
}

func TestStatusOverTheSocket(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	e.docker.answer = func(a string) ([]byte, bool, error) {
		return []byte(`{"Service":"api","State":"running","Health":"healthy","Image":"a"}`), strings.HasSuffix(a, "ps --all --format json"), nil
	}
	sock := served(t, daemon(e, &fakeHost{}))
	out, err := ctl(e, sock, "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"os         notes 1.0.0 (revision 1)", "installed  notes 1.0.0 (revision 1)", "api", "running (healthy)"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if out, err := ctl(e, sock, "status", "--json"); err != nil || !strings.Contains(out, `"installed": {`) {
		t.Errorf("--json: %v\n%s", err, out)
	}
	// A first install not finished, under way then failed.
	marker := filepath.Join(e.dir, installingFile)
	for body, want := range map[string]string{
		"":                   "not finished: the stack is coming up, or the provision hook runs",
		"failed: it broke\n": "not finished: the first install failed: it broke (`journalctl -u notes` tells more); `sudo notes-stow install` runs it again",
	} {
		if err := os.WriteFile(marker, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := ctl(e, sock, "status"); err != nil || !strings.Contains(out, want) {
			t.Errorf("%q: %v\n%s", body, err, out)
		}
	}
}

func TestUpgradeOverTheSocketAsksThenFollowsTheJob(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.release("2.0.0", 1, nil)
	host := &fakeHost{out: "written"}
	sock := served(t, daemon(p.env, host))
	// No terminal to confirm on, and no --yes: nothing happens.
	if out, err := ctl(p.env, sock, "upgrade", "--channel", "stable"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(host.calls()) > 0 {
		t.Fatalf("ran %v", host.calls())
	}
	out, err := ctl(p.env, sock, "upgrade", "--channel", "stable", "--yes", "--no-reboot")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"2.0.0 (revision 1) on stable", "  written", "next boot runs it"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if out, err := ctl(p.env, sock, "updates", "--channel", "stable", "--version", "1.0.0"); err != nil || !strings.Contains(out, "no upgrade on stable") {
		t.Errorf("%v\n%s", err, out)
	}
	// Nothing newer: upgrade says so and asks the daemon nothing more.
	calls := len(host.calls())
	if out, err := ctl(p.env, sock, "upgrade", "--channel", "stable", "--version", "1.0.0"); err != nil || !strings.Contains(out, "no upgrade on stable") {
		t.Errorf("%v\n%s", err, out)
	}
	if len(host.calls()) != calls {
		t.Errorf("ran %v", host.calls()[calls:])
	}
}

func TestAFailedJobFailsTheCommand(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.release("2.0.0", 1, nil)
	sock := served(t, daemon(p.env, &fakeHost{err: os.ErrPermission}))
	if out, err := ctl(p.env, sock, "upgrade", "--channel", "stable", "--yes"); err == nil || !strings.Contains(err.Error(), "upgrade failed") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestTheDaemonsErrorsReachTheOperator(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	sock := served(t, daemon(e, &fakeHost{}))
	if _, err := ctl(e, sock, "restart", "nope"); err == nil || !strings.Contains(err.Error(), "no service nope") {
		t.Errorf("err = %v", err)
	}
	if _, err := ctl(e, sock, "reboot", "--drain-secs", "601"); err == nil || !strings.Contains(err.Error(), "0 to 600") {
		t.Errorf("err = %v", err)
	}
	if out, err := ctl(e, sock, "restart", "api"); err != nil || !strings.Contains(out, "api: restart done") {
		t.Errorf("%v %s", err, out)
	}
}

func TestAnUnreachableDaemonIsNamed(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	dir, err := os.MkdirTemp("", "hostd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if _, err := ctl(e, filepath.Join(dir, "none.sock"), "status"); err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("no daemon: %v", err)
	}
	// A socket the caller may not open.
	sock := served(t, daemon(e, &fakeHost{}))
	if os.Geteuid() == 0 {
		t.Skip("root opens any socket")
	}
	if err := os.Chmod(sock, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := ctl(e, sock, "status"); err == nil || !strings.Contains(err.Error(), "admin group") {
		t.Errorf("no permission: %v", err)
	}
}

func TestInstallRefusesABadChannel(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	if _, err := ctl(e, "unused", "install", "--dir", e.dir, "--no-input", "--channel", "Stable!"); err == nil || !strings.Contains(err.Error(), "not a channel name") {
		t.Errorf("err = %v", err)
	}
	e.nothingWritten()
}

func TestUpgradeReadsTheChannelsPointerAlone(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.release("2.0.0", 1, nil)
	sock := served(t, daemon(p.env, &fakeHost{}))
	p.mu.Lock()
	p.paths = nil
	p.mu.Unlock()
	if out, err := ctl(p.env, sock, "upgrade", "--channel", "stable", "--yes", "--no-reboot"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, path := range p.paths {
		if strings.Contains(path, "/tags/list") {
			t.Errorf("listed the releases: %s", path)
		}
	}
}

// servedHandler serves h on a socket of its own until the test ends, and returns the socket.
func servedHandler(t *testing.T, h http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hostd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "host.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { srv.Close() })
	return sock
}

func TestUpgradeSendsTheConfirmedReleaseAndNoVersionUnasked(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	var got UpgradeRequest
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/updates", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, Update{Channel: "stable", Current: Version{"1.0.0", 1}, Target: Version{"2.0.0", 3}, Available: true})
	})
	mux.HandleFunc("POST /v1/upgrade", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		reply(w, 202, JobStarted{Job: "upgrade", Target: "2.0.0-3", Started: fakeStart, Events: ptr(int64(0))})
	})
	mux.HandleFunc("GET /v1/job", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, JobStatus{Started: ptr(fakeStart), Kind: "upgrade", Target: "2.0.0-3", State: "done", Log: []string{}})
	})
	sock := servedHandler(t, mux)
	if out, err := ctl(e, sock, "upgrade", "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got.Version != "" || got.Confirm == nil || *got.Confirm != (Version{"2.0.0", 3}) || got.Reboot == nil || !*got.Reboot {
		t.Errorf("sent %+v", got)
	}
}

func TestUpdatesSaysWhenTheInstallWentBackFromTheTarget(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	printUpdate(&b, Update{Channel: "stable", Target: Version{"2.0.0", 1}, LeftByRollback: true})
	if !strings.Contains(b.String(), "went back from 2.0.0 (revision 1)") {
		t.Errorf("%s", b.String())
	}
}

func TestFollowReadsOnFromTheDaemonsTotal(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	var sinces []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/updates", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, Update{Channel: "stable", Current: Version{"1.0.0", 1}, Target: Version{"2.0.0", 1}, Available: true})
	})
	mux.HandleFunc("POST /v1/upgrade", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 202, JobStarted{Job: "upgrade", Target: "2.0.0-1", Started: fakeStart, Events: ptr(int64(0))})
	})
	mux.HandleFunc("GET /v1/job", func(w http.ResponseWriter, r *http.Request) {
		since := r.URL.Query().Get("since")
		sinces = append(sinces, since)
		if since == "0" { // the daemon dropped lines 0 to 7
			reply(w, 200, JobStatus{Started: ptr(fakeStart), Kind: "upgrade", Target: "2.0.0-1", State: "running", Log: []string{"line 8", "line 9"}, LogTotal: 10})
			return
		}
		reply(w, 200, JobStatus{Started: ptr(fakeStart), Kind: "upgrade", Target: "2.0.0-1", State: "done", Log: []string{}, LogTotal: 10})
	})
	sock := servedHandler(t, mux)
	out, err := ctl(e, sock, "upgrade", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Join(sinces, ",") != "0,10" || strings.Count(out, "line 8") != 1 {
		t.Errorf("polled from %v\n%s", sinces, out)
	}
}

func TestFollowStopsAtAnotherJob(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/updates", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, Update{Channel: "stable", Current: Version{"1.0.0", 1}, Target: Version{"2.0.0", 1}, Available: true})
	})
	mux.HandleFunc("POST /v1/upgrade", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 202, JobStarted{Job: "upgrade", Target: "2.0.0-1", Started: fakeStart, Events: ptr(int64(0))})
	})
	// Another upgrade to the same release, started after this one: told apart by its start.
	mux.HandleFunc("GET /v1/job", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, JobStatus{Started: ptr(fakeStart.Add(time.Nanosecond)), Kind: "upgrade", Target: "2.0.0-1", State: "failed", Error: "theirs", Log: []string{}})
	})
	sock := servedHandler(t, mux)
	if _, err := ctl(e, sock, "upgrade", "--yes"); err == nil || !strings.Contains(err.Error(), "not this upgrade 2.0.0-1") {
		t.Errorf("err = %v", err)
	}
}

// appClient asks the daemon's app socket beside sock, with token when set.
func appClient(t *testing.T, sock, token, path string) int {
	t.Helper()
	app := filepath.Join(filepath.Dir(sock), "app", "app.sock")
	for range 2000 {
		if _, err := os.Stat(app); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	req, _ := http.NewRequest("GET", "http://host"+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := socketClient(&app).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestTheAppSocketServesTheAPIToTheInstallsToken(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	sock := served(t, daemon(e, &fakeHost{}))
	if code := appClient(t, sock, "", "/v1/status"); code != http.StatusUnauthorized {
		t.Errorf("before the install: %d", code)
	}
	// Nor an empty token (sent as is: a server trims a header's trailing space).
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/status", nil)
	req.Header["Authorization"] = []string{"Bearer "}
	daemon(e, &fakeHost{}).AppHandler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("an empty token before the install: %d", w.Code)
	}
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	token := get(e.readEnv(), HostTokenVar)
	for _, c := range []struct {
		token string
		want  int
	}{{"", http.StatusUnauthorized}, {"x" + token[1:], http.StatusUnauthorized}, {token + "x", http.StatusUnauthorized}, {token, http.StatusOK}} {
		if code := appClient(t, sock, c.token, "/v1/status"); code != c.want {
			t.Errorf("token %q: %d, want %d", c.token, code, c.want)
		}
	}
	app := filepath.Join(filepath.Dir(sock), "app")
	for p, want := range map[string]os.FileMode{app: 0o755, filepath.Join(app, "app.sock"): 0o666} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %v", p, fi.Mode().Perm(), err, want)
		}
	}
}

func TestNoAppSocketWhenNoServiceReachesTheDaemon(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	e.r, e.self = bundledInstaller(t, "1.0.0", 1, func(s *spec.Spec) { s.Shapes.Kairos.HostAPI = nil })
	sock := served(t, daemon(e, &fakeHost{}))
	if _, err := ctl(e, sock, "status"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(sock), "app")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("app directory: %v", err)
	}
}

func TestRollbackOverTheSocketAsksThenFollowsTheJob(t *testing.T) {
	t.Parallel()
	e, d, host := upgradedVM(t)
	sock := served(t, d)
	if out, err := ctl(e, sock, "rollback"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(fmt.Sprint(host.calls()), "grub2-editenv") {
		t.Fatalf("ran %v", host.calls())
	}
	out, err := ctl(e, sock, "rollback", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"the passive system carries notes 1.0.0 (revision 1); the install runs 2.0.0 (revision 1)",
		"rebooting into the passive system in 30 s",
		"rollback done: the VM reboots to run it — after it, `notes-stow status` and `notes-stow events` tell how it went"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if v := assessOf(t, d); v[assessTentative] != "yes" {
		t.Errorf("%v", v)
	}
}

// A job done that scheduled its reboot (or power-off) ends the verb there, upgrade and rollback
// alike: it says so and where the reboot's outcome is read, succeeds, and asks the daemon
// nothing more — whether GET job told the end, or the journal did and GET job then failed in any
// way (the daemon gone, an error answer, a body that does not decode). A job.lost read is the
// answer just as well. The drain is counted down only when there is one and the daemon answers;
// the reboot's own outcome is never waited for.
func TestAVerbEndsAtItsJobsReboot(t *testing.T) {
	t.Parallel()
	const after = " — after it, `notes-stow status` and `notes-stow events` tell how it went"
	for name, c := range map[string]struct {
		verb   string
		action string
		drain  int
		// then is what GET job meets once the journal told the end ("" : GET job answers the
		// end): gone (the daemon hangs up), 5xx, garbled.
		then string
		lost bool // the journal tells job.lost, not job.ended
		want string
		err  string // in the error; empty: success
	}{
		"upgrade":                     {verb: "upgrade", action: "reboot", drain: 30, want: "upgrade done: the VM reboots in 30 s to run it" + after},
		"rollback":                    {verb: "rollback", action: "reboot", want: "rollback done: the VM reboots to run it" + after},
		"upgrade, from its entry":     {verb: "upgrade", action: "reboot", drain: 30, then: "gone", want: "upgrade done: the VM reboots to run it" + after},
		"rollback, from its entry":    {verb: "rollback", action: "reboot", then: "gone", want: "rollback done: the VM reboots to run it" + after},
		"from its entry, then a 5xx":  {verb: "upgrade", action: "reboot", drain: 30, then: "5xx", want: "upgrade done: the VM reboots to run it" + after},
		"from its entry, then garble": {verb: "upgrade", action: "reboot", drain: 30, then: "garbled", want: "upgrade done: the VM reboots to run it" + after},
		"a power-off":                 {verb: "upgrade", action: "poweroff", drain: 30, want: "upgrade done: the VM powers off in 30 s, and its next start runs it — after that start"},
		"a power-off, from its entry": {verb: "upgrade", action: "poweroff", drain: 30, then: "gone", want: "upgrade done: the VM powers off, and its next start runs it"},
		"lost, then gone":             {verb: "upgrade", then: "gone", lost: true, err: "upgrade x lost: the daemon restarted"},
		"lost, then a 5xx":            {verb: "upgrade", then: "5xx", lost: true, err: "upgrade x lost: the daemon restarted"},
		"lost, then garble":           {verb: "upgrade", then: "garbled", lost: true, err: "upgrade x lost: the daemon restarted"},
	} {
		e := vmEnv(t, "1.0.0")
		end := JobStatus{Started: ptr(fakeStart), Kind: c.verb, Target: "x", State: "done", Reboot: &EventReboot{c.action, c.drain}, Log: []string{}}
		var mu sync.Mutex
		var jobs int
		var answered bool // the verb has its answer: any request after it fails the test
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/updates", func(w http.ResponseWriter, _ *http.Request) {
			reply(w, 200, Update{Channel: "stable", Current: Version{"1.0.0", 1}, Target: Version{"2.0.0", 1}, Available: true})
		})
		mux.HandleFunc("GET /v1/rollback", func(w http.ResponseWriter, _ *http.Request) {
			reply(w, 200, RollbackPlan{From: Version{"2.0.0", 1}, To: Version{"1.0.0", 1}})
		})
		start := func(w http.ResponseWriter, _ *http.Request) {
			reply(w, 202, JobStarted{Job: c.verb, Target: "x", Started: fakeStart, Events: ptr(int64(5))})
		}
		mux.HandleFunc("POST /v1/upgrade", start)
		mux.HandleFunc("POST /v1/rollback", start)
		mux.HandleFunc("GET /v1/job", func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			jobs++
			n := jobs
			mu.Unlock()
			if n == 1 {
				running(w, c.verb, fakeStart)
				return
			}
			mu.Lock()
			answered = true
			mu.Unlock()
			switch c.then {
			case "gone":
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
			case "5xx":
				replyErr(w, fail(http.StatusInternalServerError, "the daemon is stopping"))
			case "garbled":
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"state":`)
			default:
				reply(w, 200, end)
			}
		})
		mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
			var evs []Event
			switch {
			case c.lost:
				evs = []Event{{Seq: 6, Kind: evJobLost, Job: ptr(fakeStart), JobKind: c.verb, Target: "x", Reason: lostRestart}}
			case c.then != "":
				evs = []Event{
					{Seq: 6, Kind: evRebootScheduled, Action: c.action, DrainSecs: ptr(c.drain), By: ptr(fakeStart), BootID: "boot-1"},
					{Seq: 7, Kind: evJobEnded, Job: ptr(fakeStart), JobKind: c.verb, Target: "x", State: "done", Reboot: &EventReboot{c.action, c.drain}},
				}
			}
			journalAnswer(w, r, evs)
		})
		sock := servedHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			after := answered
			mu.Unlock()
			if after {
				t.Errorf("%s: %s %s asked after the verb had its answer", name, r.Method, r.URL)
			}
			// A connection per request: the transport retries no request the daemon hung up on.
			w.Header().Set("Connection", "close")
			mux.ServeHTTP(w, r)
		}))
		out, err := bounded(t, func() (string, error) { return ctl(e, sock, c.verb, "--yes") })
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: %v, want %q\n%s", name, err, c.err, out)
			}
			continue
		}
		if err != nil || !strings.Contains(out, c.want) {
			t.Errorf("%s: %v, want %q\n%s", name, err, c.want, out)
		}
		if (c.then != "") != strings.Contains(out, "last log lines were not read") {
			t.Errorf("%s: says its last log lines were not read: %v\n%s", name, c.then == "", out)
		}
	}
}

// A job the daemon lost (it restarted) fails the verb with the journal's reason; a daemon that
// holds no job, or another, says so without an empty kind or target.
func TestAJobLostToARestartFailsTheVerb(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		job  JobStatus
		evs  []Event
		want string
	}{
		"lost": {JobStatus{State: "idle", Log: []string{}}, []Event{
			{Seq: 6, Kind: evJobLost, Job: ptr(fakeStart), JobKind: "upgrade", Target: "2.0.0-1", Reason: lostRestart}},
			"upgrade 2.0.0-1 lost: the daemon restarted"},
		"none": {JobStatus{State: "idle", Log: []string{}}, nil, "the daemon holds no job now, not this upgrade 2.0.0-1"},
		"another": {JobStatus{Started: ptr(fakeStart.Add(time.Hour)), Kind: "backup", Target: "x", State: "running", Log: []string{}}, nil,
			"the daemon's job is now backup x, not this upgrade 2.0.0-1"},
	} {
		e := vmEnv(t, "1.0.0")
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/updates", func(w http.ResponseWriter, _ *http.Request) {
			reply(w, 200, Update{Channel: "stable", Current: Version{"1.0.0", 1}, Target: Version{"2.0.0", 1}, Available: true})
		})
		mux.HandleFunc("POST /v1/upgrade", func(w http.ResponseWriter, _ *http.Request) {
			reply(w, 202, JobStarted{Job: "upgrade", Target: "2.0.0-1", Started: fakeStart, Events: ptr(int64(5))})
		})
		mux.HandleFunc("GET /v1/job", func(w http.ResponseWriter, _ *http.Request) { reply(w, 200, c.job) })
		mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
			after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
			a := EventsAnswer{Events: []Event{}, Last: 5}
			for _, e := range c.evs {
				a.Last = e.Seq
				if e.Seq > after {
					a.Events = append(a.Events, e)
				}
			}
			reply(w, 200, a)
		})
		out, err := ctl(e, servedHandler(t, mux), "upgrade", "--yes")
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "  ") {
			t.Errorf("%s: %v, want %q\n%s", name, err, c.want, out)
		}
	}
}

// A rollback whose reboot then fails ends the verb done, as the job is: the reboot's outcome is
// the journal's (reboot.cancelled), never the verb's.
func TestARollbackEndsAtItsRebootEvenOneThatFails(t *testing.T) {
	t.Parallel()
	e, d, host := upgradedVM(t)
	host.rebootErr = errors.New("inhibited")
	sock := served(t, d)
	out, err := ctl(e, sock, "rollback", "--yes")
	if err != nil || !strings.Contains(out, "rollback done: the VM reboots to run it") {
		t.Fatalf("%v\n%s", err, out)
	}
	if j := waitJob(t, d.Handler()); j.State != "done" || j.Reboot == nil || *j.Reboot != (EventReboot{"reboot", 0}) {
		t.Errorf("%+v", j)
	}
	// The failed reboot is journaled after the job's end; the test ends once it is.
	if ev := waitEvent(t, d.O.Dir, evRebootCancelled); !strings.Contains(ev.Reason, "systemctl reboot failed: inhibited") {
		t.Errorf("the reboot.cancelled's reason %q", ev.Reason)
	}
}

func TestHealthOverTheSocketFailsWhenNotReady(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	stackAnswers(e, healthyStack)
	d := daemon(e, &fakeHost{})
	p := &prober{status: 200}
	d.Probe = p.client(t)
	sock := served(t, d)
	if out, err := ctl(e, sock, "health"); err != nil || out != "ready\n" {
		t.Errorf("%v %q", err, out)
	}
	p.status = 503
	if out, err := ctl(e, sock, "health"); err == nil || !strings.Contains(err.Error(), "not ready: api:8000/healthz/ready answered 503") {
		t.Errorf("%v %q", err, out)
	}
	if out, err := ctl(e, sock, "health", "--json"); err == nil || !strings.Contains(out, `"ready": false`) {
		t.Errorf("--json: %v %q", err, out)
	}
}

func TestStatusOverTheSocketTellsTheFallback(t *testing.T) {
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
	out, err := ctl(g.env, served(t, d), "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"boot       passive (the fallback:", "failed     notes 1.0.0 (revision 1) at 2026-10-10T12:00:00Z: ",
		"answered 503", "diagnostics: " + filepath.Join(g.dir, FailedBootsDir, "20261010T120000Z-1.0.0-r1")} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
}

func TestBackupVerbsOverTheSocket(t *testing.T) {
	t.Parallel()
	e, d, f, key := backupVM(t)
	sock := served(t, d)
	if out, err := ctl(e, sock, "backup", "status"); err != nil || !strings.Contains(out, "no backup is configured") || !strings.Contains(out, "profiles     full (default full)") {
		t.Fatalf("%v\n%s", err, out)
	}
	conf := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(conf, []byte(rcloneConf), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := ctl(e, sock, "backup", "config", "--destination", "store:backups/notes", "--rclone-config", conf, "--recipient", key.Recipient().String(), "--retention-days", "7")
	if err != nil || !strings.Contains(out, "rclone       set (not shown)") || !strings.Contains(out, "retention    7 days") || strings.Contains(out, "hush") {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := ctl(e, sock, "backup", "run"); err != nil || !strings.Contains(out, "backed up: store:backups/notes/notes-full-") {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := ctl(e, sock, "backup", "status"); err != nil || !strings.Contains(out, "next         Sun 2026-10-11 03:04:05 UTC") || !strings.Contains(out, "last         ") || strings.Contains(out, "FAILED") {
		t.Errorf("%v\n%s", err, out)
	}
	if out, err := ctl(e, sock, "backup", "list"); err != nil || !strings.Contains(out, "notes-full-") || !strings.Contains(out, "NAME") {
		t.Errorf("%v\n%s", err, out)
	}
	if out, err := ctl(e, sock, "backup", "verify", "--destination", "store:other", "--rclone-config", conf); err != nil || !strings.Contains(out, "store:other is reachable and writable") || !strings.Contains(out, "proposed, not saved") {
		t.Errorf("verify proposed: %v\n%s", err, out)
	}
	if out, err := ctl(e, sock, "backup", "verify"); err != nil || !strings.Contains(out, "reachable and writable") || strings.Contains(out, "proposed") {
		t.Errorf("%v\n%s", err, out)
	}
	f.rcatErr = errors.New("403")
	if out, err := ctl(e, sock, "backup", "verify"); err == nil || !strings.Contains(err.Error(), "backup-verify failed: writing store:backups/notes/notes-verify.txt") || strings.Contains(out, "reachable") {
		t.Errorf("an unwritable destination: %v\n%s", err, out)
	}
	f.rcatErr = nil
	big := filepath.Join(t.TempDir(), "big.conf")
	if err := os.WriteFile(big, bytes.Repeat([]byte("#"), maxRcloneConfig+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := ctl(e, sock, "backup", "config", "--rclone-config", big); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("a config too big: %v\n%s", err, out)
	}
	if out, err := ctl(e, sock, "backup", "config", "--remove", "--schedule", "daily"); err == nil || !strings.Contains(err.Error(), "--remove") {
		t.Errorf("--remove with a change: %v\n%s", err, out)
	}
	if out, err := ctl(e, sock, "backup", "config", "--remove"); err != nil || !strings.Contains(out, "no backup runs") {
		t.Errorf("%v\n%s", err, out)
	}
	if out, err := ctl(e, sock, "backup", "run", "--if-configured"); err != nil || !strings.Contains(out, "nothing to do") {
		t.Errorf("%v\n%s", err, out)
	}
	if out, err := ctl(e, sock, "backup", "run"); err == nil || !strings.Contains(err.Error(), "no backup is configured") {
		t.Errorf("%v\n%s", err, out)
	}
	if len(f.objects) != 4 { // the backup, its volume's archive, and the two probes
		t.Errorf("objects %v", f.objects)
	}
}

func TestSecretsRotateOverTheSocket(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	e.r, e.self = bundledInstaller(t, "1.0.0", 1, func(s *spec.Spec) {
		for i := range s.Secrets {
			s.Secrets[i].Rotation = s.Secrets[i].Key == "SESSION_SECRET"
		}
	})
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	if out, err := ctl(e, "", "install", "--secrets", "-", "--new-secrets"); err == nil || !strings.Contains(err.Error(), "--new-secrets generates them: give one") {
		t.Errorf("--secrets with --new-secrets: %v\n%s", err, out)
	}
	// Refused before a job: a key without rotation, a pass of none.
	h := daemon(e, &fakeHost{}).Handler()
	for _, req := range []SecretRotateRequest{{Key: "POSTGRES_PASSWORD", Pass: RotateStage}, {Key: "SESSION_SECRET", Pass: "twice"}} {
		var e400 errOf
		if code := call(t, h, "POST", "/v1/secrets/rotate", req, &e400); code != 400 {
			t.Errorf("%+v: %d %s", req, code, e400.Error)
		}
	}
	// Not while the health gate judges the boot.
	var e409 errOf
	if code := call(t, daemon(e, &fakeHost{judge: "activating"}).Handler(), "POST", "/v1/secrets/rotate", SecretRotateRequest{Key: "SESSION_SECRET", Pass: RotateStage}, &e409); code != 409 ||
		!strings.Contains(e409.Error, "has not judged this boot yet") {
		t.Errorf("judging: %d %s", code, e409.Error)
	}
	sock := served(t, daemon(e, &fakeHost{}))
	get := func(k string) string {
		env, err := ReadEnvFile(filepath.Join(e.dir, ".env"))
		if err != nil {
			t.Fatal(err)
		}
		v, _ := env.Get(k)
		return v
	}
	old := get("SESSION_SECRET")
	out, err := ctl(e, sock, "secrets", "rotate", "SESSION_SECRET", "--stage")
	if err != nil || !strings.Contains(out, "staged a new SESSION_SECRET as SESSION_SECRET_NEXT") {
		t.Fatalf("%v\n%s", err, out)
	}
	next := get("SESSION_SECRET_NEXT")
	if next == "" || next == old || get("SESSION_SECRET") != old {
		t.Fatalf("staged %q over %q", next, old)
	}
	if out, err := ctl(e, sock, "secrets", "rotate", "SESSION_SECRET"); err != nil || get("SESSION_SECRET") != next || get("SESSION_SECRET_PREVIOUS") != old {
		t.Fatalf("swap: %v\n%s", err, out)
	}
	if out, err := ctl(e, sock, "secrets", "rotate", "SESSION_SECRET", "--finish"); err != nil || get("SESSION_SECRET_PREVIOUS") != "" {
		t.Fatalf("finish: %v\n%s", err, out)
	}
	for args, want := range map[string]string{
		"POSTGRES_PASSWORD":                 "POSTGRES_PASSWORD is no secret with rotation: true (rotated: SESSION_SECRET)",
		"SESSION_SECRET --stage --finish":   "--stage and --finish are two passes",
		"SESSION_SECRET --finish --stage=x": "invalid argument",
	} {
		if out, err := ctl(e, sock, append([]string{"secrets", "rotate"}, strings.Fields(args)...)...); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v\n%s", args, err, out)
		}
	}
}

func TestRestoreAndExportOverTheSocket(t *testing.T) {
	t.Parallel()
	e, d, _, key, obj := restoreVM(t)
	sock := served(t, d)
	id := filepath.Join(t.TempDir(), "key.txt")
	if err := os.WriteFile(id, []byte(key.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := ctl(e, sock, "restore", obj, "--identity", id); err == nil || !strings.Contains(err.Error(), "--yes restores without asking") {
		t.Errorf("no terminal: %v\n%s", err, out)
	}
	out, err := ctl(e, sock, "restore", obj, "--identity", id, "--yes")
	if err != nil || !strings.Contains(out, "restored "+obj) || e.docker.restored != "the database's dump" {
		t.Fatalf("%v\n%s", err, out)
	}
	file := filepath.Join(t.TempDir(), "notes-secrets.age")
	if out, err := ctl(e, sock, "secrets", "export", "--recipient", key.Recipient().String(), "-o", file); err != nil || !strings.Contains(out, "wrote "+file) {
		t.Fatalf("%v\n%s", err, out)
	}
	raw, _ := os.ReadFile(file)
	var f SecretsFile
	if err := yaml.Unmarshal(decrypt(t, raw, key), &f); err != nil || f.Secrets["SESSION_SECRET"] != get(e.readEnv(), "SESSION_SECRET") {
		t.Errorf("the export does not hold the install's secret: %v %+v", err, f)
	}
}

func TestUpgradeSkipBackupIsSent(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	var got UpgradeRequest
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/updates", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, Update{Channel: "stable", Current: Version{"1.0.0", 1}, Target: Version{"2.0.0", 3}, Available: true})
	})
	mux.HandleFunc("POST /v1/upgrade", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		reply(w, 202, JobStarted{Job: "upgrade", Target: "2.0.0-3", Started: fakeStart, Events: ptr(int64(0))})
	})
	mux.HandleFunc("GET /v1/job", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, JobStatus{Started: ptr(fakeStart), Kind: "upgrade", Target: "2.0.0-3", State: "done", Log: []string{}})
	})
	sock := servedHandler(t, mux)
	if out, err := ctl(e, sock, "upgrade", "--yes"); err != nil || got.SkipBackup {
		t.Fatalf("%v %+v\n%s", err, got, out)
	}
	if out, err := ctl(e, sock, "upgrade", "--yes", "--skip-backup"); err != nil || !got.SkipBackup {
		t.Fatalf("%v %+v\n%s", err, got, out)
	}
}

// The daemon gives the timer the settings' schedule as it starts: the drop-in is under /run.
func TestServingGivesTheTimerTheSchedule(t *testing.T) {
	t.Parallel()
	_, d, _, key := backupVM(t)
	req := validConfig(key)
	req.Schedule = ptr("Mon *-*-* 01:00:00 UTC")
	configure(t, d, req)
	dropIn := filepath.Join(d.Systemd, "notes-backup.timer.d", "schedule.conf")
	if err := os.Remove(dropIn); err != nil {
		t.Fatal(err)
	}
	served(t, d)
	for range 2000 {
		if _, err := os.Stat(dropIn); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Error("the schedule was not given again as the daemon started")
}

func TestEventsOverTheSocket(t *testing.T) {
	t.Parallel()
	g := newGate(t, "passive", nil, false)
	g.probe.status = 503
	if err := g.gate.Run(context.Background()); err == nil {
		t.Fatal("healthy")
	}
	d := daemon(g.env, &fakeHost{})
	boots(t, d, "passive", nil)
	// The install ran before the boot was known; the gate is the first writer that knows it.
	bootID := filepath.Join(t.TempDir(), "boot_id")
	if err := os.WriteFile(bootID, []byte("boot-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.gate.In.O.BootIDFile, d.O.BootIDFile = bootID, bootID
	if err := g.gate.Run(context.Background()); err == nil {
		t.Fatal("healthy")
	}
	sock := served(t, d)
	out, err := ctl(g.env, sock, "events")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "1  ") || !strings.Contains(lines[0], "install.finished release=1.0.0-r1") ||
		!strings.Contains(lines[1], "boot.judged release=1.0.0-r1 on_trial=false verdict=kept_unhealthy") || !strings.Contains(lines[1], `reason="`) ||
		!strings.HasPrefix(lines[2], "3  ") || !strings.Contains(lines[2], "boot.started release=1.0.0-r1 slot=passive fallback=false boot_id=boot-1") ||
		!strings.Contains(lines[3], "boot.judged") || !strings.Contains(lines[3], "boot_id=boot-1") {
		t.Errorf("%s", out)
	}
	out, err = ctl(g.env, sock, "events", "--after", "2", "--json")
	var e Event
	if err != nil || len(strings.Split(strings.TrimSpace(out), "\n")) != 2 || json.Unmarshal([]byte(strings.Split(out, "\n")[0]), &e) != nil || e.Seq != 3 || e.Kind != evBootStarted {
		t.Errorf("%v %+v\n%s", err, e, out)
	}
	if out, err := ctl(g.env, sock, "events", "--after", "9"); err != nil || !strings.Contains(out, "holds nothing after 9") || !strings.Contains(out, "boot.started") {
		t.Errorf("past the last: %v\n%s", err, out)
	}
	var st Status
	if out, err := ctl(g.env, sock, "status", "--json"); err != nil || json.Unmarshal([]byte(out), &st) != nil || st.Events == nil || *st.Events != 4 {
		t.Errorf("%v %+v\n%s", err, st, out)
	}
}

// followDaemon is a fake daemon for a follow: an upgrade started at fakeStart (its 202 carrying
// events), GET job answered by job (its n-th read, from 1), GET events by events (its n-th
// read); a nil handler hangs up.
func followDaemon(t *testing.T, events *int64, job func(n int, w http.ResponseWriter, r *http.Request), evs func(n int, w http.ResponseWriter, r *http.Request)) string {
	t.Helper()
	var mu sync.Mutex
	var jobs, reads int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/updates", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, Update{Channel: "stable", Current: Version{"1.0.0", 1}, Target: Version{"2.0.0", 1}, Available: true})
	})
	mux.HandleFunc("POST /v1/upgrade", func(w http.ResponseWriter, _ *http.Request) {
		st := JobStarted{Job: "upgrade", Target: "2.0.0-1", Started: fakeStart, Events: events}
		if events == nil {
			st.EventsError = "the job's start could not be journaled: full"
		}
		reply(w, 202, st)
	})
	mux.HandleFunc("POST /v1/backup/verify", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 202, JobStarted{Job: "backup-verify", Target: "store:b", Started: fakeStart, Events: events})
	})
	mux.HandleFunc("GET /v1/job", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		jobs++
		n := jobs
		mu.Unlock()
		job(n, w, r)
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reads++
		n := reads
		mu.Unlock()
		if evs == nil {
			t.Errorf("the journal read without a number to read it from")
			reply(w, 500, map[string]string{"error": "no"})
			return
		}
		evs(n, w, r)
	})
	return servedHandler(t, mux)
}

// journalAnswer answers the entries of evs after the request's after.
func journalAnswer(w http.ResponseWriter, r *http.Request, evs []Event) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	a := EventsAnswer{Events: []Event{}, Last: 5}
	for _, e := range evs {
		a.Last = e.Seq
		if e.Seq > after {
			a.Events = append(a.Events, e)
		}
	}
	reply(w, 200, a)
}

// running answers GET job with the upgrade running.
func running(w http.ResponseWriter, kind string, at time.Time) {
	reply(w, 200, JobStatus{Started: ptr(at), Kind: kind, Target: "x", State: "running", Log: []string{}})
}

// hang holds the request until its client goes: a daemon that takes a request and never answers.
func hang(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }

// bounded runs f, failing the test when it has not returned within 10 s.
func bounded(t *testing.T, f func() (string, error)) (string, error) {
	t.Helper()
	type res struct {
		out string
		err error
	}
	done := make(chan res, 1)
	go func() {
		out, err := f()
		done <- res{out, err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-time.After(10 * time.Second):
		t.Fatal("the follow did not end")
		return "", nil
	}
}

var endedWithReboot = []Event{
	{Seq: 6, Kind: evRebootScheduled, Action: "reboot", DrainSecs: ptr(30), By: ptr(fakeStart), BootID: "boot-1"},
	{Seq: 7, Kind: evJobEnded, Job: ptr(fakeStart), JobKind: "upgrade", Target: "2.0.0-1", State: "done", Reboot: &EventReboot{"reboot", 30}},
}

// A follow its context ended (Ctrl-C, a signal) answers from the job's end once read, whatever
// ended it: done is done (with its reboot, rebootNote), failed its error; its end not read is
// interrupted. A job answered from its journaled end says its last log lines were not read.
func TestAnInterruptedFollowAnswersFromTheEndItRead(t *testing.T) {
	t.Parallel()
	done := Event{Seq: 6, Kind: evJobEnded, Job: ptr(fakeStart), JobKind: "upgrade", Target: "2.0.0-1", State: "done"}
	failed := Event{Seq: 6, Kind: evJobEnded, Job: ptr(fakeStart), JobKind: "upgrade", Target: "2.0.0-1", State: "failed", Error: "the slot is full"}
	for name, c := range map[string]struct {
		// first, when set, answers the follow's first journal read, and the context ends as it
		// reads the job again; else the context ends during that first read.
		first []Event
		want  string // in the error; empty: success
		out   string
	}{
		"before its end":                    {want: "interrupted before upgrade ended"},
		"done, read before":                 {first: []Event{done}, out: "upgrade's last log lines were not read"},
		"failed, read before":               {first: []Event{failed}, want: "upgrade failed: the slot is full"},
		"done with its reboot, read before": {first: endedWithReboot, out: "upgrade done: the VM reboots to run it"},
	} {
		e := vmEnv(t, "1.0.0")
		ctx, cancel := context.WithCancel(context.Background())
		sock := followDaemon(t, ptr(int64(5)),
			func(n int, w http.ResponseWriter, r *http.Request) {
				if n == 2 && c.first != nil { // the follow read the end: it ends as it reads the job again
					cancel()
					hang(w, r)
					return
				}
				running(w, "upgrade", fakeStart)
			},
			func(n int, w http.ResponseWriter, r *http.Request) {
				if n == 1 && c.first != nil {
					journalAnswer(w, r, c.first)
					return
				}
				cancel()
				hang(w, r) // the interrupt comes during the long poll
			})
		out, err := bounded(t, func() (string, error) { return ctlCtx(ctx, e, sock, "upgrade", "--yes") })
		cancel()
		if c.want != "" {
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: %v, want %q\n%s", name, err, c.want, out)
			}
			continue
		}
		if err != nil || !strings.Contains(out, c.out) {
			t.Errorf("%s: %v, want %q\n%s", name, err, c.out, out)
		}
	}
}

// A job whose end the follow read, replaced in the slot by the next job before it read the job
// again, is answered from that end.
func TestAJobEndedThenReplacedIsAnsweredFromItsEnd(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		end  Event
		want string // in the error; empty: success
	}{
		"done":   {end: Event{Seq: 6, Kind: evJobEnded, Job: ptr(fakeStart), JobKind: "backup-verify", Target: "store:b", State: "done", Result: "store:b is reachable"}},
		"failed": {end: Event{Seq: 6, Kind: evJobEnded, Job: ptr(fakeStart), JobKind: "backup-verify", Target: "store:b", State: "failed", Error: "store:b refused"}, want: "backup-verify failed: store:b refused"},
	} {
		e := vmEnv(t, "1.0.0")
		sock := followDaemon(t, ptr(int64(5)),
			func(n int, w http.ResponseWriter, _ *http.Request) {
				if n == 1 {
					running(w, "backup-verify", fakeStart)
					return
				}
				running(w, "backup", fakeStart.Add(time.Hour)) // the timer's backup, woken by the end
			},
			func(_ int, w http.ResponseWriter, r *http.Request) { journalAnswer(w, r, []Event{c.end}) })
		out, err := bounded(t, func() (string, error) { return ctl(e, sock, "backup", "verify") })
		if c.want != "" {
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: %v, want %q\n%s", name, err, c.want, out)
			}
			continue
		}
		if err != nil || !strings.Contains(out, "store:b is reachable") {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
		if !strings.Contains(out, "the backup-verify's last log lines were not read: its end is the journal's (`notes-stow events`)") {
			t.Errorf("%s: the follow does not say the job's last log lines were not read\n%s", name, out)
		}
	}
}

// A daemon that takes a follow's request and never answers is one that does not answer: the
// follow's request deadline holds.
func TestAFollowEndsWhenTheDaemonNeverAnswers(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	sock := followDaemon(t, ptr(int64(5)),
		func(n int, w http.ResponseWriter, r *http.Request) {
			if n == 1 {
				running(w, "upgrade", fakeStart)
				return
			}
			hang(w, r)
		}, func(_ int, w http.ResponseWriter, r *http.Request) { hang(w, r) })
	out, err := bounded(t, func() (string, error) { return ctl(e, sock, "upgrade", "--yes") })
	if err == nil || !strings.Contains(err.Error(), "the host daemon is not reachable") {
		t.Errorf("%v\n%s", err, out)
	}
}

// A 202 without the journal's number is followed by the job's log alone, which the follow says;
// its end, a reboot it scheduled included, is GET job's.
func TestAJobStartNotJournaledIsFollowedByItsLogAlone(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		end  JobStatus
		want string // in the error; empty: success
		out  string
	}{
		"done": {end: JobStatus{Started: ptr(fakeStart), Kind: "upgrade", Target: "2.0.0-1", State: "done", Log: []string{}}},
		"done with a reboot": {end: JobStatus{Started: ptr(fakeStart), Kind: "upgrade", Target: "2.0.0-1", State: "done", Reboot: &EventReboot{"reboot", 30}, Log: []string{}},
			out: "upgrade done: the VM reboots in 30 s to run it"},
		"failed": {end: JobStatus{Started: ptr(fakeStart), Kind: "upgrade", Target: "2.0.0-1", State: "failed", Error: "no room", Log: []string{}},
			want: "upgrade failed: no room"},
	} {
		e := vmEnv(t, "1.0.0")
		sock := followDaemon(t, nil,
			func(n int, w http.ResponseWriter, _ *http.Request) {
				if n == 1 {
					running(w, "upgrade", fakeStart)
					return
				}
				reply(w, 200, c.end)
			}, nil)
		out, err := bounded(t, func() (string, error) { return ctl(e, sock, "upgrade", "--yes") })
		if !strings.Contains(out, "the event journal does not hold this upgrade's start (the job's start could not be journaled: full)") {
			t.Errorf("%s: the follow does not say it reads the log alone\n%s", name, out)
		}
		if c.want != "" {
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: %v, want %q\n%s", name, err, c.want, out)
			}
			continue
		}
		if err != nil || !strings.Contains(out, c.out) {
			t.Errorf("%s: %v, want %q\n%s", name, err, c.out, out)
		}
	}
}

// `provision --set` on the VM reaches the host daemon's run: a value the hook returned is replaced
// before it runs, a key that is neither an input only it reads nor one it returned is refused.
func TestProvisionSetOverTheSocket(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	e.docker.hookOut = "ADMIN_PASSWORD=minted\n"
	sock := served(t, daemon(e, &fakeHost{}))
	if out, err := ctl(e, sock, "provision"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	e.docker.hookOut = ""
	if out, err := ctl(e, sock, "provision", "--set", "ADMIN_PASSWORD=chosen"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Join(e.docker.hookEnv, " ") != "ADMIN_PASSWORD='chosen'" {
		t.Fatalf("the hook was given %v", e.docker.hookEnv)
	}
	if out, err := ctl(e, sock, "provision", "--set", "SESSION_SECRET=x"); err == nil || !strings.Contains(err.Error(), "SESSION_SECRET is neither an input only the provision hook reads") {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := ctl(e, sock, "provision", "--set", "nokey"); err == nil || !strings.Contains(err.Error(), "is not KEY=VALUE") {
		t.Fatalf("%v\n%s", err, out)
	}
}

// `provision --set` on the VM sets a key the hook declares it returns although it never returned
// it: the host daemon's run records it, and the next run is handed it.
func TestProvisionSetOfADeclaredValueOverTheSocket(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	e.r.Spec.Hooks.Provision.Config["returns"] = []any{"ADMIN_KEY"}
	sock := served(t, daemon(e, &fakeHost{}))
	if out, err := ctl(e, sock, "provision", "--set", "ADMIN_KEY=given"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Join(e.docker.hookEnv, " ") != "ADMIN_KEY='given'" {
		t.Fatalf("the hook was given %v", e.docker.hookEnv)
	}
	if out, err := ctl(e, sock, "provision"); err != nil || strings.Join(e.docker.hookEnv, " ") != "ADMIN_KEY='given'" {
		t.Fatalf("the next run was given %v: %v\n%s", e.docker.hookEnv, err, out)
	}
	if out, err := ctl(e, sock, "provision", "--set", "OTHER_KEY=x"); err == nil || !strings.Contains(err.Error(), "nor a value it returned or declares it returns (ADMIN_KEY)") {
		t.Fatalf("%v\n%s", err, out)
	}
}
