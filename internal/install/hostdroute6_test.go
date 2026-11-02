package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/spec"
)

// routeLink is a primary link whose IPv6 default route comes once routers were solicited
// answerAfter times (0: never), or is there from the start (has).
type routeLink struct {
	mu          sync.Mutex
	has         bool
	solicited   int
	answerAfter int
	ifaces      []string
	// unread: `ip -6 route` fails; garbled: it answers what is not JSON; edgeFirst: the secondary
	// NIC edge's default route is listed before the primary's.
	unread, garbled, edgeFirst bool
}

func (l *routeLink) Run(_ context.Context, c Cmd) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch strings.Join(c.Args, " ") {
	case "ip -j route show default":
		if l.edgeFirst {
			return []byte(`[{"dst":"default","gateway":"192.0.2.1","dev":"edge"},{"dst":"default","gateway":"10.0.0.1","dev":"ens3"}]`), nil
		}
		return []byte(`[{"dst":"default","gateway":"10.0.0.1","dev":"ens3"}]`), nil
	case "ip -6 -j route show default":
		if l.unread {
			return nil, errors.New("Cannot send dump request: Connection refused")
		}
		if l.garbled {
			return []byte("Option \"-j\" is unknown"), nil
		}
		if l.has {
			return []byte(`[{"dst":"default","gateway":"fe80::1","dev":"ens3"}]`), nil
		}
		// A default route on another link is not the primary's.
		return []byte(`[{"dst":"default","gateway":"fe80::9","dev":"wg0"}]`), nil
	case "ip -j address show":
		if l.garbled {
			return []byte("Option \"-j\" is unknown"), nil
		}
		return []byte(`[{"ifname":"ens3","address":"52:54:00:12:34:56","addr_info":[]}]`), nil
	}
	return nil, nil
}

func (l *routeLink) solicit(iface string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.solicited++
	l.ifaces = append(l.ifaces, iface)
	if l.answerAfter > 0 && l.solicited >= l.answerAfter {
		l.has = true
	}
	return nil
}

func (l *routeLink) count() int { l.mu.Lock(); defer l.mu.Unlock(); return l.solicited }

// route6Hostd is a daemon over link whose install network has IPv6 as ipv6 says.
func route6Hostd(t *testing.T, link *routeLink, ipv6 bool) *Hostd {
	t.Helper()
	install := filepath.Join(t.TempDir(), "install-network.yaml")
	body := "mode: dhcp\n"
	if ipv6 {
		body += "ipv6: true\n"
	}
	if err := os.WriteFile(install, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Hostd{R: testRelease(t, "1.0.0"), O: Options{Dir: t.TempDir(), Log: io.Discard}, Host: link, Unit: time.Millisecond,
		Networkd: t.TempDir(), RunNetworkd: t.TempDir(), Issue: t.TempDir(), InstallNet: install, Solicit: link.solicit, ctx: context.Background()}
}

// The boot solicits the routers on the primary link until its IPv6 default route comes, and
// returns then; with IPv6 off, or the route there, it sends nothing; past its bound it warns and
// returns.
func TestTheBootSolicitsUntilTheIPv6DefaultRoute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	link := &routeLink{answerAfter: 3}
	var log bytes.Buffer
	route6Hostd(t, link, true).route6Boot(ctx, time.Minute, &log)
	if link.count() != 3 || !strings.Contains(log.String(), "the IPv6 default route on ens3 came after 3 router solicitations") {
		t.Errorf("%d solicitations: %s", link.count(), log.String())
	}
	if link.ifaces[0] != "ens3" {
		t.Errorf("solicited on %q", link.ifaces)
	}
	for name, l := range map[string]*routeLink{"the route there": {has: true}} {
		route6Hostd(t, l, true).route6Boot(ctx, time.Minute, io.Discard)
		if l.count() != 0 {
			t.Errorf("%s: %d solicitations", name, l.count())
		}
	}
	off := &routeLink{}
	route6Hostd(t, off, false).route6Boot(ctx, time.Minute, io.Discard)
	if off.count() != 0 {
		t.Errorf("IPv6 off: %d solicitations", off.count())
	}
	never := &routeLink{}
	log.Reset()
	start := time.Now()
	route6Hostd(t, never, true).route6Boot(ctx, 20*time.Millisecond, &log)
	if never.count() < 2 || time.Since(start) > 5*time.Second || !strings.Contains(log.String(), "warning: no IPv6 default route on ens3 after 20ms: the services start without one") {
		t.Errorf("past the bound: %d solicitations: %s", never.count(), log.String())
	}
}

// An override's IPv6 setting is the link's, over the install's.
func TestTheOverrideSaysWhetherIPv6IsOn(t *testing.T) {
	t.Parallel()
	link := &routeLink{}
	h := route6Hostd(t, link, true)
	if !h.primaryIPv6() {
		t.Error("the install's IPv6 is not taken")
	}
	if err := h.netFiles().write(netState{Version: netStateVersion, Config: &NetConfig{Mode: "dhcp"}}); err != nil {
		t.Fatal(err)
	}
	if h.primaryIPv6() {
		t.Error("an override without IPv6 is not taken")
	}
}

// The daemon solicits while the route is missing, warns once past the bound in status and the
// journal, and clears the warning once the route is back.
func TestTheDaemonSolicitsAndWarnsWithoutAnIPv6DefaultRoute(t *testing.T) {
	t.Parallel()
	link := &routeLink{}
	h := route6Hostd(t, link, true)
	var mu sync.Mutex
	var evs []Event
	h.onEvent = func(e []Event) { mu.Lock(); evs = append(evs, e...); mu.Unlock() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.route6Watch(ctx) }()
	defer func() { cancel(); <-done }()
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); {
			if time.Now().After(deadline) {
				t.Fatalf("never: %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitFor("the warning", func() bool { return h.route6Warning() != "" })
	if w := h.route6Warning(); !strings.Contains(w, "no IPv6 default route on ens3 since ") {
		t.Errorf("warning %q", w)
	}
	waitFor("the event", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, e := range evs {
			if e.Kind == evNoRoute6 && strings.Contains(e.Reason, "no IPv6 default route on ens3") {
				return true
			}
		}
		return false
	})
	if link.count() < 2 {
		t.Errorf("%d solicitations", link.count())
	}
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	var st Status
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || !strings.Contains(st.NetworkWarning, "no IPv6 default route on ens3") {
		t.Errorf("status: %v %s", err, w.Body)
	}
	link.mu.Lock()
	link.has = true
	link.mu.Unlock()
	waitFor("the warning cleared", func() bool { return h.route6Warning() == "" })
	mu.Lock()
	n := 0
	for _, e := range evs {
		if e.Kind == evNoRoute6 {
			n++
		}
	}
	mu.Unlock()
	if n != 1 {
		t.Errorf("%d events for one stretch", n)
	}
}

// With IPv6 off the daemon sends nothing and never warns.
func TestTheDaemonLeavesALinkWithoutIPv6Be(t *testing.T) {
	t.Parallel()
	link := &routeLink{}
	h := route6Hostd(t, link, false)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	h.route6Watch(ctx)
	if link.count() != 0 || h.route6Warning() != "" {
		t.Errorf("%d solicitations, warning %q", link.count(), h.route6Warning())
	}
}

// A solicitation that fails is logged once, and the soliciting goes on.
func TestAFailedSolicitationIsLoggedOnce(t *testing.T) {
	t.Parallel()
	link := &routeLink{}
	h := route6Hostd(t, link, true)
	n := 0
	h.Solicit = func(string) error { n++; return errors.New("operation not permitted") }
	var log bytes.Buffer
	h.route6Boot(context.Background(), 20*time.Millisecond, &log)
	if n < 2 || strings.Count(log.String(), "a router solicitation on ens3 failed: operation not permitted") != 1 {
		t.Errorf("%d tries: %s", n, log.String())
	}
}

// Routes that do not read are logged once and neither solicit nor warn; IPv6 turned off while a
// warning is up clears it without saying the route is back.
func TestTheDaemonNeitherWarnsOnAnUnreadRouteNorAfterIPv6IsOff(t *testing.T) {
	t.Parallel()
	link := &routeLink{unread: true}
	h := route6Hostd(t, link, true)
	var log bytes.Buffer
	var mu sync.Mutex
	h.O.Log = writerFunc(func(b []byte) { mu.Lock(); defer mu.Unlock(); log.Write(b) })
	logged := func() string { mu.Lock(); defer mu.Unlock(); return log.String() }
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	h.route6Watch(ctx)
	cancel()
	if link.count() != 0 || h.route6Warning() != "" || strings.Count(logged(), "the IPv6 default route does not read: ip -6 route: Cannot send dump request") != 1 {
		t.Errorf("unread: %d solicitations, warning %q:\n%s", link.count(), h.route6Warning(), logged())
	}
	link.mu.Lock()
	link.unread = false
	link.mu.Unlock()
	ctx, cancel = context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.route6Watch(ctx) }()
	defer func() { cancel(); <-done }()
	for deadline := time.Now().Add(5 * time.Second); h.route6Warning() == ""; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("never warned")
		}
	}
	if err := os.WriteFile(h.netFiles().install, []byte("mode: dhcp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); h.route6Warning() != ""; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the warning stays with IPv6 off")
		}
	}
	if l := logged(); strings.Contains(l, "is back") || !strings.Contains(l, "IPv6 is off on the primary link: its default route is no longer watched") {
		t.Errorf("IPv6 off:\n%s", l)
	}
}

// An answer of `ip` that is not JSON is an error, not a missing route.
func TestAGarbledRouteAnswerIsAnError(t *testing.T) {
	t.Parallel()
	h := route6Hostd(t, &routeLink{garbled: true}, true)
	if _, ok, err := h.route6(context.Background()); ok || err == nil || !strings.Contains(err.Error(), "ip -6 route: ") {
		t.Errorf("%v, %v", ok, err)
	}
}

// With the solicitations' backoff grown long, a route that comes back is seen at the next check
// (route6Every), not at the next solicitation.
func TestARouteBackClearsTheWarningWhateverTheBackoff(t *testing.T) {
	t.Parallel()
	link := &routeLink{}
	h := route6Hostd(t, link, true)
	// Unit 1 ms: route6Every 30 ms; past 9 solicitations (about 0.5 s) the backoff is 512 ms.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.route6Watch(ctx) }()
	defer func() { cancel(); <-done }()
	for deadline := time.Now().Add(5 * time.Second); link.count() < 9; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d solicitations", link.count())
		}
	}
	if h.route6Warning() == "" {
		t.Fatal("no warning")
	}
	link.mu.Lock()
	link.has = true
	link.mu.Unlock()
	back := time.Now()
	for h.route6Warning() != "" {
		if time.Since(back) > 2*time.Second {
			t.Fatalf("the warning stays %s after the route came back", time.Since(back))
		}
		time.Sleep(time.Millisecond)
	}
}

// The watch checks within its period whatever the solicitations' backoff, sooner when a
// solicitation is due first, never under a unit.
func TestRoute6Next(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	for _, c := range []struct {
		nextRS time.Time
		want   time.Duration
	}{
		{time.Time{}, 30 * time.Second},
		{now.Add(time.Hour), 30 * time.Second},
		{now.Add(4 * time.Second), 4 * time.Second},
		{now.Add(-time.Second), time.Second},
		{now, time.Second},
	} {
		if got := route6Next(30*time.Second, c.nextRS, now, time.Second); got != c.want {
			t.Errorf("next solicitation %s: %s, want %s", c.nextRS.Sub(now), got, c.want)
		}
	}
}

// Past its bound with the routes unread, the boot says so rather than that no route is there;
// errors that alternate are each logged once.
func TestTheBootSaysTheRoutesDidNotRead(t *testing.T) {
	t.Parallel()
	var log bytes.Buffer
	route6Hostd(t, &routeLink{unread: true}, true).route6Boot(context.Background(), 20*time.Millisecond, &log)
	if l := log.String(); !strings.Contains(l, "warning: the routes did not read for 20ms (ip -6 route: Cannot send dump request") || strings.Contains(l, "no IPv6 default route") || strings.Count(l, "the routes do not read:") != 1 {
		t.Errorf("%s", l)
	}
}

// A secondary NIC's default route, listed first, is not the primary link's.
func TestTheSecondaryNICsDefaultRouteIsNotThePrimarys(t *testing.T) {
	t.Parallel()
	link := &routeLink{edgeFirst: true, has: true}
	h := route6Hostd(t, link, true)
	h.R.Spec.Host = &spec.Host{Interfaces: map[string]spec.HostInterface{"uplink": {Primary: true}, "edge": {}}}
	if iface, ok, err := h.route6(context.Background()); iface != "ens3" || !ok || err != nil {
		t.Errorf("%q %v %v", iface, ok, err)
	}
}
