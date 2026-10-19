package install

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/spec"
)

func TestPressureLevelsAtTheirBoundaries(t *testing.T) {
	t.Parallel()
	dp := &spec.DiskPressure{SoftPct: 75, HardPct: 90}
	for avail, want := range map[uint64]string{
		10000: "none", // 0%
		2501:  "none", // 74.99%
		2500:  "soft", // 75%
		1001:  "soft", // 89.99%
		1000:  "hard", // 90%
		0:     "hard",
	} {
		if got, _ := pressureOf(&Disk{Total: 10000, Used: 10000 - avail, Available: avail}, dp); got != want {
			t.Errorf("%d available of 10000: %s, want %s", avail, got, want)
		}
	}
	if _, used := pressureOf(&Disk{Total: 10000, Used: 7499, Available: 2501}, dp); used != 74.9 {
		t.Errorf("used %v, want 74.9 (rounded down)", used)
	}
	// As df counts it: the blocks reserved for root are neither used nor available.
	if got, used := pressureOf(&Disk{Total: 1050, Used: 895, Available: 105}, dp); got != "soft" || used != 89.5 {
		t.Errorf("with reserved blocks: %s %v, want soft 89.5", got, used)
	}
	for _, d := range []*Disk{nil, {}} {
		if got, _ := pressureOf(d, dp); got != "" {
			t.Errorf("%+v: %q, want no reading", d, got)
		}
	}
}

// purger is the hook's service: it records what it was sent and answers status.
type purger struct {
	mu     sync.Mutex
	status int
	answer string // the body it answers; empty is a fixed one with control characters
	got    []*http.Request
	bodies []PressureBody
	dialed []string
}

func (p *purger) client(t *testing.T) *http.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b PressureBody
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &b)
		p.mu.Lock()
		defer p.mu.Unlock()
		p.got = append(p.got, r)
		p.bodies = append(p.bodies, b)
		w.WriteHeader(p.status)
		_, _ = io.WriteString(w, cmp.Or(p.answer, "purge\x1b[31m\u009b refused\n"))
	}))
	t.Cleanup(srv.Close)
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		p.mu.Lock()
		p.dialed = append(p.dialed, addr)
		p.mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}}}
}

func (p *purger) asked() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.got)
}

// pressureEnv is an installed VM whose spec has the disk-pressure hook (soft 75, hard 90, the
// session secret as its token), its disk used at *used percent.
type pressureEnv struct {
	*env
	h      *Hostd
	host   *fakeHost
	purge  *purger
	used   *float64
	banner string
}

func newPressure(t *testing.T, install bool) *pressureEnv {
	t.Helper()
	e := vmEnv(t, "1.0.0")
	e.r.Spec.Hooks.Maintenance = &spec.Maintenance{DiskPressure: &spec.DiskPressure{
		SoftPct: 75, HardPct: 90, Post: "http://api:8000/maintenance/disk-pressure?from=host", AuthSecret: "SESSION_SECRET"}}
	if install {
		if err := e.install(vm); err != nil {
			t.Fatal(err)
		}
	}
	stackAnswers(e, healthyStack)
	p := &pressureEnv{env: e, host: &fakeHost{}, purge: &purger{status: 204}, used: new(float64)}
	p.h = daemon(e, p.host)
	p.h.Probe = p.purge.client(t)
	p.h.Statfs = func(string) *Disk {
		if *p.used < 0 {
			return nil // the disk cannot be read
		}
		return &Disk{Path: "/usr/local", Total: 1000, Used: uint64(*p.used * 10), Available: uint64(1000 - *p.used*10)}
	}
	p.banner = filepath.Join(p.h.Issue, "08-notes-disk.issue")
	return p
}

func (p *pressureEnv) check(used float64) bool {
	*p.used = used
	return p.h.pressureCheck(context.Background(), p.h.pressureHook())
}

// waitAsked waits until the service was asked n times, up to a deadline.
func (p *pressureEnv) waitAsked(t *testing.T, n int, within time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(within); p.purge.asked() < n; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("asked %d times within %s, want %d", p.purge.asked(), within, n)
		}
	}
}

func (p *pressureEnv) reloads() int {
	n := 0
	for _, c := range p.host.calls() {
		if slices.Equal(c, []string{"agetty", "--reload"}) {
			n++
		}
	}
	return n
}

func TestPressurePostsTheLevelWithTheToken(t *testing.T) {
	t.Parallel()
	p := newPressure(t, true)
	if p.check(74.9) || p.purge.asked() != 0 {
		t.Fatal("posted below soft_pct")
	}
	if p.check(80) || p.purge.asked() != 1 {
		t.Fatalf("asked %d times", p.purge.asked())
	}
	r, b := p.purge.got[0], p.purge.bodies[0]
	env := p.readEnv()
	secret, _ := env.Get("SESSION_SECRET")
	if r.Host != "api:8000" {
		t.Errorf("Host %q, want the name post gives", r.Host)
	}
	if r.Method != "POST" || r.URL.Path != "/maintenance/disk-pressure" || r.URL.RawQuery != "from=host" ||
		r.Header.Get("Authorization") != "Bearer "+secret || secret == "" || r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("%s %s auth %q", r.Method, r.URL, r.Header.Get("Authorization"))
	}
	if b != (PressureBody{Level: "soft", UsedPct: 80, SoftPct: 75, HardPct: 90}) {
		t.Errorf("body %+v", b)
	}
	if p.purge.dialed[0] != "10.1.2.3:8000" {
		t.Errorf("dialed %v, want api's address on the stack's network", p.purge.dialed)
	}
	var st Status
	call(t, p.h.Handler(), "GET", "/v1/status", nil, &st)
	if pr := st.Pressure; pr == nil || pr.Level != "soft" || pr.UsedPct != 80 || pr.SoftPct != 75 || pr.HardPct != 90 ||
		pr.Last == nil || !pr.Last.OK || pr.Last.Result != "answered 204" || pr.Last.Level != "soft" {
		t.Fatalf("status %+v", st.Pressure)
	}
	if _, err := os.Stat(p.banner); !os.IsNotExist(err) {
		t.Errorf("a banner below hard_pct: %v", err)
	}
}

func TestPressurePastHardShowsTheBannerUntilItEases(t *testing.T) {
	t.Parallel()
	p := newPressure(t, true)
	p.check(89.9)
	if _, err := os.Stat(p.banner); !os.IsNotExist(err) || p.reloads() != 0 {
		t.Fatalf("banner at 89.9%%: %v, %d reloads", err, p.reloads())
	}
	p.check(90)
	raw, err := os.ReadFile(p.banner)
	if err != nil || !strings.Contains(string(raw), "disk: 90.0% used on /usr/local, past 90%") || p.reloads() != 1 {
		t.Fatalf("%v %q, %d reloads", err, raw, p.reloads())
	}
	if p.purge.bodies[1].Level != "hard" {
		t.Errorf("posted %+v", p.purge.bodies)
	}
	p.check(90) // the same banner: the login screen is not reloaded again
	if p.reloads() != 1 {
		t.Errorf("%d reloads for an unchanged banner", p.reloads())
	}
	p.check(80)
	if _, err := os.Stat(p.banner); !os.IsNotExist(err) || p.reloads() != 2 {
		t.Errorf("banner below hard_pct: %v, %d reloads", err, p.reloads())
	}
}

// No result carries the token: an error that quotes it, before the POST, has it replaced.
func TestPressureErrorNeverShowsTheToken(t *testing.T) {
	t.Parallel()
	p := newPressure(t, true)
	secret, _ := p.readEnv().Get("SESSION_SECRET")
	p.docker.answer = func(a string) ([]byte, bool, error) {
		return nil, strings.HasSuffix(a, "ps --quiet api"), errors.New("invalid value " + secret)
	}
	p.check(80)
	if l := p.h.lastPost; l == nil || !strings.Contains(l.Result, "invalid value <SESSION_SECRET>") || strings.Contains(l.Result, secret) {
		t.Fatalf("last %+v", l)
	}
}

// With a token, the service's answer is reduced to its status: it may echo the token in any form
// (here cut at the bytes kept, and uppercased).
func TestPressureAnswerNeverShowsTheToken(t *testing.T) {
	t.Parallel()
	p := newPressure(t, true)
	secret, _ := p.readEnv().Get("SESSION_SECRET")
	p.purge.status = 401
	p.purge.answer = strings.Repeat("x", 500) + " Bearer " + strings.ToUpper(secret)
	p.check(80)
	l := p.h.lastPost
	if l == nil || l.Result != "answered 401" {
		t.Fatalf("last %+v", l)
	}
	if strings.Contains(p.log.String(), secret) {
		t.Errorf("the token is in the log:\n%s", p.log.String())
	}
}

// A service that does not answer is tried again sooner.
func TestPressureNoAnswerIsTriedSooner(t *testing.T) {
	t.Parallel()
	p := newPressure(t, true)
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.Listener.Addr().String()
	srv.Close() // nothing listens there now
	p.h.Probe = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}}
	if !p.check(80) {
		t.Error("no answer not checked again sooner")
	}
	if l := p.h.lastPost; l == nil || !strings.HasPrefix(l.Result, "no answer: ") {
		t.Fatalf("last %+v", l)
	}
}

// A 4xx is not tried again before the next regular check.
func TestPressureClientErrorWaitsTheInterval(t *testing.T) {
	t.Parallel()
	p := newPressure(t, true)
	p.purge.status = 404
	if p.check(80) {
		t.Error("a 4xx checked again sooner")
	}
	if l := p.h.lastPost; l == nil || l.OK || !strings.HasPrefix(l.Result, "answered 404") {
		t.Fatalf("last %+v", l)
	}
}

func TestPressureNotSent(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		install bool
		edit    func(p *pressureEnv)
		want    string
		again   bool
	}{
		"before the install": {false, nil, "not sent: not installed yet", true},
		"no address": {true, func(p *pressureEnv) {
			p.docker.answer = func(string) ([]byte, bool, error) { return nil, false, nil }
		}, "not sent: api has no container", true},
		"not a service URL": {true, func(p *pressureEnv) {
			p.r.Spec.Hooks.Maintenance.DiskPressure.Post = "http://api/purge"
		}, `not sent: hooks.maintenance.disk_pressure.post "http://api/purge" is not http://<service>:<port>/…`, false},
		"no token in the install": {true, func(p *pressureEnv) {
			env := p.readEnv()
			env.Set("SESSION_SECRET", "")
			if err := env.WriteFile(filepath.Join(p.h.O.Dir, envName)); err != nil {
				t.Fatal(err)
			}
		}, "not sent: the install has no SESSION_SECRET (hooks.maintenance.disk_pressure.auth_secret)", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := newPressure(t, c.install)
			if c.edit != nil {
				c.edit(p)
			}
			if p.check(95) != c.again {
				t.Errorf("checked again sooner: %v, want %v", !c.again, c.again)
			}
			l := p.h.lastPost
			if p.purge.asked() != 0 || l == nil || l.OK || l.Result != c.want {
				t.Fatalf("asked %d, last %+v", p.purge.asked(), l)
			}
		})
	}
}

// Without a token, the answer's first bytes are kept, unprintable characters as spaces.
func TestPressureRefusedIsLoggedSafely(t *testing.T) {
	t.Parallel()
	p := newPressure(t, true)
	p.r.Spec.Hooks.Maintenance.DiskPressure.AuthSecret = ""
	p.purge.status = 503
	if !p.check(80) {
		t.Error("a 5xx not checked again sooner")
	}
	if l := p.h.lastPost; l == nil || l.OK || l.Result != "answered 503: purge [31m  refused" {
		t.Fatalf("last %+v", l)
	}
}

func TestPressureWatchChecksAtOnceThenEveryInterval(t *testing.T) {
	t.Parallel()
	p := newPressure(t, true)
	*p.used = 80
	p.h.Unit = time.Microsecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.h.pressureWatch(ctx)
		close(done)
	}()
	p.waitAsked(t, 2, 10*time.Second)
	cancel()
	<-done
}

// A POST answered 5xx is tried again after pressureRetry, not pressureEvery: at 2 ms a unit,
// three asks take about 0.25 s, and 2.4 s at the regular interval.
func TestPressureWatchRetriesSoonerWhenNotTaken(t *testing.T) {
	t.Parallel()
	p := newPressure(t, true)
	p.purge.status = 503
	*p.used = 80
	p.h.Unit = 2 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.h.pressureWatch(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	p.waitAsked(t, 3, 2*time.Second)
}

// Serve returns only once a check under way has ended: nothing of the watch runs after it.
func TestServeWaitsForTheCheckUnderWay(t *testing.T) {
	t.Parallel()
	p := newPressure(t, true)
	entered := make(chan struct{})
	var once sync.Once
	var returned atomic.Bool
	p.h.Statfs = func(string) *Disk {
		once.Do(func() { close(entered) })
		time.Sleep(50 * time.Millisecond)
		returned.Store(true)
		return nil
	}
	dir, err := os.MkdirTemp("", "hostd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.h.Serve(ctx, filepath.Join(dir, "run", "host.sock")) }()
	<-entered
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !returned.Load() {
		t.Fatal("Serve returned while a check was under way")
	}
}

func TestPressureUnreadableDiskKeepsTheLastPost(t *testing.T) {
	t.Parallel()
	p := newPressure(t, true)
	p.check(80)
	if !p.check(-1) {
		t.Error("an unreadable disk not checked again sooner")
	}
	var st Status
	call(t, p.h.Handler(), "GET", "/v1/status", nil, &st)
	if pr := st.Pressure; pr == nil || pr.Level != "unknown" || pr.Last == nil || pr.Last.Result != "answered 204" {
		t.Fatalf("status %+v", st.Pressure)
	}
	out, err := ctl(p.env, served(t, p.h), "status")
	if err != nil || !strings.Contains(out, "pressure   unknown: the disk cannot be read (soft 75%, hard 90%)") || !strings.Contains(out, "answered 204") {
		t.Errorf("%v\n%s", err, out)
	}
}

// Without the hook, the watch removes the banner an earlier release left, and stops.
func TestPressureWatchWithoutTheHookRemovesTheBanner(t *testing.T) {
	t.Parallel()
	p := newPressure(t, true)
	p.r.Spec.Hooks.Maintenance = nil
	if err := os.WriteFile(p.banner, []byte("disk: old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.h.pressureWatch(context.Background()) // returns: nothing to watch
	if _, err := os.Stat(p.banner); !os.IsNotExist(err) || p.reloads() != 1 {
		t.Fatalf("%v, %d reloads", err, p.reloads())
	}
	var st Status
	if call(t, p.h.Handler(), "GET", "/v1/status", nil, &st); st.Pressure != nil {
		t.Errorf("pressure %+v without the hook", st.Pressure)
	}
}

func TestStatusOverTheSocketShowsThePressure(t *testing.T) {
	t.Parallel()
	p := newPressure(t, true)
	p.check(92)
	out, err := ctl(p.env, served(t, p.h), "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pressure   hard: 92.0% used (soft 75%, hard 90%)", "last POST at ", "(hard, 92.0%): answered 204"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
}

func TestServeStartsTheWatch(t *testing.T) {
	t.Parallel()
	p := newPressure(t, true)
	*p.used = 80
	served(t, p.h)
	p.waitAsked(t, 1, 10*time.Second)
}

// A banner that exists but does not read is removed all the same.
func TestPressureBannerUnreadableIsRemoved(t *testing.T) {
	t.Parallel()
	p := newPressure(t, true)
	if err := os.Mkdir(p.banner, 0o755); err != nil { // reading a directory fails
		t.Fatal(err)
	}
	p.check(50)
	if _, err := os.Stat(p.banner); !os.IsNotExist(err) || p.reloads() != 1 {
		t.Fatalf("%v, %d reloads", err, p.reloads())
	}
}
