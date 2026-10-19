package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

// stackAnswers makes e's stack answer as running services, api at 10.1.2.3 on its network, and at
// 10.9.9.9 on an internal one whose name sorts first.
func stackAnswers(e *env, ps string) {
	e.docker.answer = func(a string) ([]byte, bool, error) {
		switch {
		case strings.HasSuffix(a, "ps --all --format json"):
			return []byte(ps), true, nil
		case strings.HasSuffix(a, "ps --quiet api"):
			return []byte("c0ffee\n"), true, nil
		case a == "inspect --format {{json .NetworkSettings.Networks}} c0ffee":
			return []byte(`{"z_other":{"IPAddress":"","NetworkID":"n3"},"notes_default":{"IPAddress":"10.1.2.3","NetworkID":"n2"},"notes_db":{"IPAddress":"10.9.9.9","NetworkID":"n1"}}` + "\n"), true, nil
		case a == "network inspect --format {{.Internal}} n1 n2":
			return []byte("true\nfalse\n"), true, nil
		case strings.Contains(a, " logs "):
			return []byte("a line\n"), true, nil
		case strings.HasSuffix(a, "ps --all"):
			return []byte("NAME STATUS\n"), true, nil
		}
		return nil, false, nil
	}
}

const healthyStack = `{"Service":"api","State":"running","Health":"","Image":"a"}
{"Service":"postgres","State":"running","Health":"healthy","Image":"p"}
{"Service":"migrate","State":"exited","Health":"","Image":"a","ExitCode":0}
`

// prober is the stack's health endpoint: it answers status and body, and records the addresses
// dialed.
type prober struct {
	mu     sync.Mutex
	status int
	body   string
	dialed []string
	asked  int
}

func (p *prober) client(t *testing.T) *http.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.asked++
		if r.URL.Path != "/healthz/ready" && r.URL.Path != "/ready" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(p.status)
		fmt.Fprint(w, p.body)
	}))
	t.Cleanup(srv.Close)
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		p.mu.Lock()
		p.dialed = append(p.dialed, addr)
		p.mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}}}
}

// gateEnv is a VM install of 1.0.0 booted as mode, the boot assessment holding vars, a passive
// system kept when passive.
type gateEnv struct {
	*env
	host  *fakeHost
	probe *prober
	gate  *Gate
}

func newGate(t *testing.T, mode string, vars map[string]string, passive bool) *gateEnv {
	t.Helper()
	e := vmEnv(t, "1.0.0")
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	stackAnswers(e, healthyStack)
	cos, state := t.TempDir(), t.TempDir()
	if mode != "" {
		if err := os.WriteFile(filepath.Join(cos, mode+"_mode"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var kv []string
	for k, v := range vars {
		kv = append(kv, k+"="+v)
	}
	if err := editEnv(filepath.Join(state, assessFile), kv); err != nil {
		t.Fatal(err)
	}
	if passive {
		if err := os.MkdirAll(filepath.Join(state, "cOS"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, passiveImage), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g := &gateEnv{env: e, host: &fakeHost{}, probe: &prober{status: 200, body: `{"status":"ready"}`}}
	g.gate = &Gate{In: e.installer(func(o *Options) { vm(o); o.Cos = cos }), Host: g.host, Probe: g.probe.client(t), State: state, Unit: time.Microsecond,
		Now: func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) }}
	return g
}

func (g *gateEnv) vars() map[string]string {
	g.t.Helper()
	v, err := assessment{dir: g.gate.State}.read()
	if err != nil {
		g.t.Fatal(err)
	}
	return v
}

func (g *gateEnv) rebooted() bool {
	return strings.Contains(fmt.Sprint(g.host.calls()), "systemctl reboot")
}

var onTrial = map[string]string{assessEnable: "yes", assessTentative: "yes"}

func TestTheGateMarksAHealthyBootOnTrialGood(t *testing.T) {
	t.Parallel()
	g := newGate(t, "active", onTrial, true)
	if err := g.gate.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := g.vars(); v[assessEnable] != "" || v[assessTentative] != "" {
		t.Errorf("the boot is not marked good: %v", v)
	}
	if g.rebooted() {
		t.Error("rebooted")
	}
	// Asked at api's address on the stack's network, on the provider's port.
	if fmt.Sprint(g.probe.dialed) != "[10.1.2.3:8000]" {
		t.Errorf("dialed %v", g.probe.dialed)
	}
	if !strings.Contains(g.log.String(), "the upgrade is kept") {
		t.Errorf("log: %s", g.log)
	}
	// The partition is written read-write, then read-only again.
	var mounts []string
	for _, c := range g.host.calls() {
		if c[0] == "mount" {
			mounts = append(mounts, c[2])
		}
	}
	if fmt.Sprint(mounts) != "[remount,rw remount,ro]" {
		t.Errorf("mounts %v", mounts)
	}
}

func TestAnUnhealthyBootOnTrialKeepsDiagnosticsAndFallsBack(t *testing.T) {
	t.Parallel()
	g := newGate(t, "active", onTrial, true)
	g.probe.status = 503
	err := g.gate.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "api:8000/healthz/ready answered 503, not 200") {
		t.Fatalf("%v", err)
	}
	if !g.rebooted() {
		t.Error("no reboot into the passive system")
	}
	if v := g.vars(); v[assessEnable] != "yes" || v[assessTentative] != "yes" {
		t.Errorf("the assessment changed: %v", v)
	}
	dir := filepath.Join(g.dir, FailedBootsDir, "20261010T120000Z-1.0.0-r1")
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("the error does not name %s: %v", dir, err)
	}
	for _, f := range []string{failedBootFile, "boot.log", "ps.txt", "logs-api.txt", "logs-postgres.txt", "logs-migrate.txt"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Error(err)
		}
	}
	fb := lastFailedBoot(g.dir)
	if fb == nil || fb.Release != (Version{"1.0.0", 1}) || !strings.Contains(fb.Reason, "answered 503") || fb.Dir != dir {
		t.Errorf("%+v", fb)
	}
}

func TestAGateOnTrialKeepsTheSystemWhenGoingBackIsRefused(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		barriers []string
		kept     bool
	}{
		"a barrier":     {[]string{"7 (drop x)"}, true},
		"going back ok": {nil, false},
	} {
		e, d, host := upgradedVM(t)
		stackAnswers(e, healthyStack)
		e.schemaAt("7", c.barriers, true)
		if err := editEnv(filepath.Join(d.State, assessFile), []string{assessEnable + "=yes", assessTentative + "=yes"}); err != nil {
			t.Fatal(err)
		}
		p := &prober{status: 503}
		g := &Gate{In: e.installer(vm), Host: host, Probe: p.client(t), State: d.State, Unit: time.Microsecond}
		err := g.Run(context.Background())
		v, _ := assessment{dir: d.State}.read()
		fb := lastFailedBoot(e.dir)
		if fb == nil {
			t.Fatalf("%s: no diagnostics", name)
		}
		var st Status
		call(t, d.Handler(), "GET", "/v1/status", nil, &st)
		if !c.kept {
			if !strings.Contains(fmt.Sprint(host.calls()), "systemctl reboot") || fb.Kept != "" || st.FailedBoot.Kept != "" {
				t.Errorf("%s: no fallback: %v, kept %q", name, err, fb.Kept)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "this system is kept, as going back is refused: refusing: the database carries 7 (drop x)") {
			t.Errorf("%s: %v", name, err)
		}
		if strings.Contains(fmt.Sprint(host.calls()), "systemctl reboot") {
			t.Errorf("%s: rebooted into a release that cannot start", name)
		}
		// Marked as a good boot: the next boot keeps this system too.
		if v[assessEnable] != "" || v[assessTentative] != "" {
			t.Errorf("%s: the boot is not marked kept: %v", name, v)
		}
		if !strings.Contains(fb.Kept, "the database carries 7 (drop x)") || st.FailedBoot == nil || st.FailedBoot.Kept != fb.Kept {
			t.Errorf("%s: kept %q, status %+v", name, fb.Kept, st.FailedBoot)
		}
		// The install is left as it ran: the dry rollback changed nothing.
		if _, err := os.Stat(filepath.Join(e.dir, PrevDir, prevState)); err != nil {
			t.Errorf("%s: the previous release is gone: %v", name, err)
		}
	}
}

func TestTheGateFallsBackOnlyFromABootOnTrial(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		mode    string
		vars    map[string]string
		passive bool
		want    string
	}{
		// GRUB's side of the assessment did not mark the attempt: a reboot would boot this again.
		"not marked":     {"active", map[string]string{assessEnable: "yes"}, true, "not healthy"},
		"no upgrade":     {"active", nil, true, "not healthy"},
		"passive":        {"passive", onTrial, true, "not healthy"},
		"recovery":       {"recovery", onTrial, true, "not healthy"},
		"no passive":     {"active", onTrial, false, "no passive system to go back to"},
		"always enabled": {"active", map[string]string{assessAlways: "yes", assessTentative: "yes"}, true, ""},
	} {
		g := newGate(t, c.mode, c.vars, c.passive)
		g.probe.status = 503
		err := g.gate.Run(context.Background())
		if err == nil {
			t.Fatalf("%s: healthy", name)
		}
		if c.want == "" {
			if !g.rebooted() {
				t.Errorf("%s: no reboot", name)
			}
			continue
		}
		if g.rebooted() || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: rebooted %v, %v", name, g.rebooted(), err)
		}
		if lastFailedBoot(g.dir) == nil {
			t.Errorf("%s: no diagnostics", name)
		}
	}
}

func TestAHealthyBootOffTrialChangesNothing(t *testing.T) {
	t.Parallel()
	g := newGate(t, "passive", onTrial, true)
	if err := g.gate.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := g.vars(); v[assessEnable] != "yes" || v[assessTentative] != "yes" {
		t.Errorf("the fallback's assessment changed: %v", v)
	}
	if len(g.host.calls()) > 0 {
		t.Errorf("ran %v", g.host.calls())
	}
}

func TestTheGateWaitsUntilTheDeadline(t *testing.T) {
	t.Parallel()
	g := newGate(t, "active", onTrial, true)
	g.gate.In.R.Spec.Shapes.Kairos = &spec.KairosShape{HealthGate: &spec.HealthGate{DeadlineS: 10_000_000, DeadlineAfterResetS: 10_000_000, PollS: 1}}
	g.probe.status = 503
	go func() {
		// Healthy once the gate asked twice.
		for {
			time.Sleep(time.Millisecond)
			g.probe.mu.Lock()
			if g.probe.asked >= 2 {
				g.probe.status = 200
				g.probe.mu.Unlock()
				return
			}
			g.probe.mu.Unlock()
		}
	}()
	if err := g.gate.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.rebooted() || g.vars()[assessTentative] != "" {
		t.Error("not marked good")
	}
	if !strings.Contains(g.log.String(), "not healthy yet") {
		t.Errorf("log: %s", g.log)
	}
}

func TestAFailedBootFailsTheGateAtOnce(t *testing.T) {
	t.Parallel()
	g := newGate(t, "active", onTrial, true)
	g.gate.Unit = time.Hour // waiting would never end
	g.host.boot = "failed"
	err := g.gate.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "notes.service failed") || !g.rebooted() {
		t.Fatalf("%v, rebooted %v", err, g.rebooted())
	}
}

func TestTheGateJudgesTheOSsRelease(t *testing.T) {
	t.Parallel()
	g := newGate(t, "active", onTrial, true)
	g.gate.In.R, _ = bundledInstaller(t, "2.0.0", 1, nil)
	err := g.gate.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the install runs 1.0.0 (revision 1), not 2.0.0 (revision 1)") {
		t.Fatalf("%v", err)
	}
}

func TestNoInstallNothingToJudge(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	host := &fakeHost{}
	if err := (&Gate{In: e.installer(vm), Host: host, State: t.TempDir()}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(host.calls()) > 0 {
		t.Errorf("ran %v", host.calls())
	}
}

func TestMarkingGoodFailsLoudly(t *testing.T) {
	t.Parallel()
	for name, set := range map[string]func(*fakeHost){
		"editenv": func(h *fakeHost) { h.editErr = errors.New("no space") },
		"remount": func(h *fakeHost) { h.remountErr = errors.New("busy") },
	} {
		g := newGate(t, "active", onTrial, true)
		set(g.host)
		err := g.gate.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "the next boot would fall back") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestReadyJudgesEveryService(t *testing.T) {
	t.Parallel()
	for ps, want := range map[string]string{
		healthyStack: "",
		`{"Service":"api","State":"running","Health":"unhealthy","Image":"a"}`: "api is unhealthy",
		`{"Service":"api","State":"running","Health":"starting","Image":"a"}`:  "api is starting",
		`{"Service":"api","State":"restarting","Health":"","Image":"a"}`:       "api is restarting",
		`{"Service":"api","State":"created","Health":"","Image":"a"}`:          "api is created",
		`{"Service":"web","State":"exited","Health":"","Image":"w","ExitCode":1}` + "\n" +
			`{"Service":"api","State":"running","Health":"","Image":"a"}`: "web exited with code 1",
		"": "no service of the stack runs",
	} {
		g := newGate(t, "", nil, false)
		stackAnswers(g.env, ps)
		err := g.gate.In.ready(context.Background(), g.readEnv(), g.gate.Probe)
		if want == "" {
			if err != nil {
				t.Errorf("%q: %v", ps, err)
			}
			continue
		}
		if err == nil || err.Error() != want {
			t.Errorf("%q: %v, want %q", ps, err, want)
		}
	}
}

func TestTheHTTPJSONProbeComparesFields(t *testing.T) {
	t.Parallel()
	jsonHealth := func(s *spec.Spec) {
		s.Health = spec.Provider{Name: "http-json", Config: map[string]any{"service": "api", "port": 9090, "path": "/ready",
			"expect": map[string]any{"status": "ready", "workers": 2}}}
	}
	for body, want := range map[string]string{
		`{"status":"ready","workers":2,"other":true}`: "",
		`{"status":"ready","workers":2.0}`:            "",
		`{"status":"starting","workers":2}`:           `api:9090/ready answered status: "starting", not "ready"`,
		`{"status":"ready"}`:                          "api:9090/ready answered workers: nothing, not 2",
		`[1]`:                                         "did not answer a JSON object",
		strings.Repeat(" ", maxProbeAnswer+1) + "{}":  "more than",
	} {
		g := newGate(t, "", nil, false)
		g.gate.In.R, _ = bundledInstaller(t, "1.0.0", 1, jsonHealth)
		g.probe.body = body
		err := g.gate.In.ready(context.Background(), g.readEnv(), g.gate.Probe)
		if want == "" {
			if err != nil {
				t.Errorf("%.40s: %v", body, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.40s: %v, want %q", body, err, want)
		}
	}
	g := newGate(t, "", nil, false)
	g.gate.In.R, _ = bundledInstaller(t, "1.0.0", 1, jsonHealth)
	g.probe.status = 500
	if err := g.gate.In.ready(context.Background(), g.readEnv(), g.gate.Probe); err == nil || !strings.Contains(err.Error(), "answered 500") {
		t.Errorf("500: %v", err)
	}
}

func TestTheProbeNeedsAnAddress(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ id, nets, internal, want string }{
		"no container":  {"", "", "", "api has no container"},
		"no address":    {"c0ffee", `{"n":{"IPAddress":"","NetworkID":"i"}}`, "", "api has no address on a network"},
		"not an ip":     {"c0ffee", `{"n":{"IPAddress":"x","NetworkID":"i"}}`, "", "api has no address on a network"},
		"internal only": {"c0ffee", `{"b":{"IPAddress":"10.0.0.2","NetworkID":"i2"},"a":{"IPAddress":"10.0.0.1","NetworkID":"i1"}}`, "true\ntrue\n", "api is only on internal networks (a, b), which the host does not reach"},
		"short answer":  {"c0ffee", `{"a":{"IPAddress":"10.0.0.1","NetworkID":"i1"}}`, "", "inspecting api's networks: 0 answers for 1 networks"},
	} {
		g := newGate(t, "", nil, false)
		g.docker.answer = func(a string) ([]byte, bool, error) {
			switch {
			case strings.HasSuffix(a, "ps --all --format json"):
				return []byte(healthyStack), true, nil
			case strings.HasSuffix(a, "ps --quiet api"):
				return []byte(c.id), true, nil
			case strings.HasPrefix(a, "inspect "):
				return []byte(c.nets), true, nil
			case strings.HasPrefix(a, "network inspect "):
				return []byte(c.internal), true, nil
			}
			return nil, false, nil
		}
		if err := g.gate.In.ready(context.Background(), g.readEnv(), g.gate.Probe); err == nil || err.Error() != c.want {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTheProbeTakesAnIPv6AddressWithoutIPv4(t *testing.T) {
	t.Parallel()
	for nets, want := range map[string]string{
		`{"n":{"IPAddress":"","GlobalIPv6Address":"fd00:db8::5","NetworkID":"i"}}`:                                                          "fd00:db8::5",
		`{"n":{"IPAddress":"10.0.0.5","GlobalIPv6Address":"fd00:db8::5","NetworkID":"i"}}`:                                                  "10.0.0.5",
		`{"a":{"IPAddress":"","GlobalIPv6Address":"","NetworkID":"i0"},"n":{"IPAddress":"","GlobalIPv6Address":"fd00::7","NetworkID":"i"}}`: "fd00::7",
	} {
		g := newGate(t, "", nil, false)
		g.docker.answer = func(a string) ([]byte, bool, error) {
			switch {
			case strings.HasSuffix(a, "ps --quiet api"):
				return []byte("c0ffee"), true, nil
			case strings.HasPrefix(a, "inspect "):
				return []byte(nets), true, nil
			case strings.HasPrefix(a, "network inspect "):
				return []byte("false\n"), true, nil
			}
			return nil, false, nil
		}
		if got, err := g.gate.In.address(context.Background(), g.readEnv(), "api"); err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", nets, got, err, want)
		}
	}
}

func TestTheHealthCommandAnswersReady(t *testing.T) {
	t.Parallel()
	cmdHealth := func(s *spec.Spec) {
		s.Health = spec.Provider{Name: "command", Config: map[string]any{"image": "api", "cmd": []any{"notes-api", "ready"}}}
	}
	for name, c := range map[string]struct {
		out  string
		err  error
		want string
	}{
		"ready":     {`{"ready": true}`, nil, ""},
		"not ready": {`{"ready": false, "reason": "migrating"}`, nil, "the health command answers not ready: migrating"},
		"no reason": {`{"ready": false}`, nil, "not ready: no reason given"},
		"no ready":  {`{}`, nil, "carries no ready"},
		"no answer": {"", exitCode(2), "the health command has no answer"},
		"fails":     {"", exitCode(1), "the health command: health.ready: exit status 1"},
	} {
		g := newGate(t, "", nil, false)
		g.gate.In.R, _ = bundledInstaller(t, "1.0.0", 1, cmdHealth)
		var asked []any
		g.docker.health = func(req map[string]any) (string, error) {
			asked = append(asked, req["op"])
			return c.out, c.err
		}
		err := g.gate.In.ready(context.Background(), g.readEnv(), nil)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
		if fmt.Sprint(asked) != "[health.ready]" {
			t.Errorf("%s: asked %v", name, asked)
		}
		// Each call's compose file is its own, and removed after.
		if left, _ := filepath.Glob(filepath.Join(g.dir, ".stowage-health-*.yaml")); len(left) > 0 {
			t.Errorf("%s: left %v", name, left)
		}
	}
}

func TestOnlyTheLastFailedBootsAreKept(t *testing.T) {
	t.Parallel()
	g := newGate(t, "", nil, false)
	g.probe.status = 503
	for i := range keepFailedBoots + 2 {
		g.gate.Now = func() time.Time { return time.Date(2026, 10, 10, 12, 0, i, 0, time.UTC) }
		if err := g.gate.Run(context.Background()); err == nil {
			t.Fatal("healthy")
		}
	}
	entries, err := os.ReadDir(filepath.Join(g.dir, FailedBootsDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != keepFailedBoots || entries[0].Name() != "20261010T120002Z-1.0.0-r1" {
		t.Errorf("kept %v", entries)
	}
	if fb := lastFailedBoot(g.dir); fb == nil || !strings.HasSuffix(fb.Dir, "20261010T120006Z-1.0.0-r1") {
		t.Errorf("last %+v", fb)
	}
	raw, _ := os.ReadFile(filepath.Join(g.dir, FailedBootsDir, "20261010T120006Z-1.0.0-r1", failedBootFile))
	var fb FailedBoot
	if err := json.Unmarshal(raw, &fb); err != nil || fb.At.IsZero() {
		t.Errorf("%s: %v", raw, err)
	}
}

// The gate reads its boot's record until a read succeeds, then keeps it for its run: a boot's id
// does not change.
func TestTheGateReadsItsBootOnceItCan(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	id := filepath.Join(t.TempDir(), "boot_id")
	g := &Gate{In: e.installer(func(o *Options) { vm(o); o.BootIDFile = id })}
	write := func(s string) {
		if err := os.WriteFile(id, []byte(s+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g.judged(false, verdictHealthy, "", "") // no id yet
	write("boot-1")
	g.boot.next = time.Time{} // the retry's wait passed
	g.judged(false, verdictHealthy, "", "")
	write("boot-2")
	g.judged(false, verdictHealthy, "", "")
	var ids []string
	for _, ev := range ofKind(held(t, g.In.O.Dir), evBootJudged) {
		ids = append(ids, ev.BootID)
	}
	if strings.Join(ids, ",") != ",boot-1,boot-1" {
		t.Errorf("judged in boots %q", ids)
	}
}
