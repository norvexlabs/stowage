package install

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/spec"
)

// l4OnlyFront is a front that terminates TLS on an L4 route and serves no route of its own.
func l4OnlyFront() (spec.Front, []spec.L4Route) {
	return spec.Front{TLSModes: []string{"acme", "internal", "byo", "none"}},
		[]spec.L4Route{{Name: "db", Protocol: "tcp", Port: 7443, Upstream: "db:5432", TLS: &spec.L4TLS{DomainInput: "DB_DOMAIN"}}}
}

// The front opens a port only where something serves on it: its routes behind TLS on both, the
// L4 routes alone on none but acme's HTTP challenge, nothing in loopback mode or without a front.
func TestTheFrontsPortsAreWhatServesOnThem(t *testing.T) {
	t.Parallel()
	l4f, l4 := l4OnlyFront()
	pass := []spec.L4Route{{Name: "db", Protocol: "tcp", Port: 5432, Upstream: "db:5432"}}
	for _, c := range []struct {
		name        string
		f           spec.Front
		l4          []spec.L4Route
		mode        string
		http, https bool
	}{
		{"routes, internal", l4TLSFront(), nil, "internal", true, true},
		{"routes, acme", l4TLSFront(), nil, "acme", true, true},
		{"routes, loopback", l4TLSFront(), nil, Loopback, false, false},
		{"no front", spec.Front{}, l4, "acme", false, false},
		{"L4 alone, internal", l4f, l4, "internal", false, false},
		{"L4 alone, byo", l4f, l4, "byo", false, false},
		{"L4 alone, acme", l4f, l4, "acme", true, false},
		{"L4 alone, loopback", l4f, l4, Loopback, false, false},
		{"plaintext L4 alone, acme", l4f, pass, "acme", false, false},
	} {
		if http, https := frontServed(c.f, c.l4, c.mode); http != c.http || https != c.https {
			t.Errorf("%s: http %v https %v, want %v %v", c.name, http, https, c.http, c.https)
		}
	}
}

// Docker publishes the front's ports something serves on alone.
func TestTheFrontPublishesWhatServesOnIt(t *testing.T) {
	t.Parallel()
	f, l4 := l4OnlyFront()
	get := func(string) string { return "" }
	for _, c := range []struct {
		f          spec.Front
		mode       string
		want, none []string
	}{
		{f, "internal", nil, []string{":80\n", ":443\n"}},
		{f, "acme", []string{"${" + HTTPPortVar + "}:80\n"}, []string{":443\n"}},
		{l4TLSFront(), "internal", []string{"${" + HTTPPortVar + "}:80\n", "${" + HTTPSPortVar + "}:443\n"}, nil},
	} {
		raw, err := frontCompose(c.f, l4, get, c.mode, hostAPI{}, "sha256:x", "", false, false, false, nil, nil, false, FrontUID)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range c.want {
			if !strings.Contains(string(raw), w) {
				t.Errorf("%s %v: no %q in\n%s", c.mode, c.f.Routes, w, raw)
			}
		}
		for _, n := range c.none {
			if strings.Contains(string(raw), n) {
				t.Errorf("%s %v: %q published in\n%s", c.mode, c.f.Routes, n, raw)
			}
		}
	}
}

// The VM's firewall opens the front's ports something serves on alone, and an L4 route's while
// its service runs.
func TestTheVMsFirewallOpensWhatServesOnIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in := &Installer{R: testRelease(t, "1.0.0"), O: Options{Dir: t.TempDir(), Firewall: filepath.Join(t.TempDir(), "nftables.d"), Host: &nftHost{}, Log: io.Discard}}
	f, l4 := l4OnlyFront()
	in.R.Spec.Front, in.R.Spec.L4.Routes = f, append(l4, spec.L4Route{Name: "mq", Protocol: "tcp", Port: 1883, Upstream: "mq:1883"})
	env := NewEnvFile()
	env.Set(HTTPPortVar, "80")
	env.Set(HTTPSPortVar, "443")
	rules := func(mode string, off ...string) string {
		t.Helper()
		env.Set("STOWAGE_TLS_MODE", mode)
		if err := in.openPorts(ctx, env, hostNet{off: off}); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(filepath.Join(in.O.Firewall, "notes-ports.nft"))
		return string(b)
	}
	if got := rules("internal"); strings.Contains(got, "dport {") || !strings.Contains(got, "\ntcp dport 7443 accept\n") || !strings.Contains(got, "\ntcp dport 1883 accept\n") {
		t.Errorf("internal:\n%s", got)
	}
	if got := rules("acme"); !strings.Contains(got, "\ntcp dport { 80 } accept\n") || strings.Contains(got, " 443") {
		t.Errorf("acme:\n%s", got)
	}
	if got := rules("internal", "mq"); strings.Contains(got, "1883") || !strings.Contains(got, "7443") {
		t.Errorf("mq off:\n%s", got)
	}
	// A route the front terminates TLS on is the front's port, whatever its upstream.
	if got := rules("internal", "db"); !strings.Contains(got, "\ntcp dport 7443 accept\n") {
		t.Errorf("db off:\n%s", got)
	}
	in.R.Spec.Front = l4TLSFront()
	if got := rules("internal"); !strings.Contains(got, "\ntcp dport { 80, 443 } accept\n") {
		t.Errorf("with a route:\n%s", got)
	}
}

// An install checks free the front's ports something serves on alone: a front of L4 routes alone
// takes a host whose 80 and 443 are held, but in acme mode, whose challenge is on 80.
func TestAnInstallChecksFreeWhatTheFrontServesOn(t *testing.T) {
	t.Parallel()
	in := &Installer{R: testRelease(t, "1.0.0"), O: Options{Dir: t.TempDir(), Log: io.Discard,
		PortFree: func(_ string, port int) bool { return port != 80 && port != 443 }}}
	f, l4 := l4OnlyFront()
	in.R.Spec.Front, in.R.Spec.L4.Routes = f, l4
	in.O.HostAddrs = func() []string { return nil }
	p, err := in.publish("internal")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.portsFree(p); err != nil {
		t.Errorf("internal: %v", err)
	}
	if p.ports["http"] != 80 || p.ports["https"] != 443 {
		t.Errorf("the ports are recorded all the same: %v", p.ports)
	}
	p, _ = in.publish("acme")
	if err := in.portsFree(p); err == nil || !strings.Contains(err.Error(), "80 (http)") || strings.Contains(err.Error(), "443") {
		t.Errorf("acme: %v", err)
	}
	in.R.Spec.Front = l4TLSFront()
	p, _ = in.publish("internal")
	if err := in.portsFree(p); err == nil || !strings.Contains(err.Error(), "80 (http), 443 (https)") {
		t.Errorf("with a route: %v", err)
	}
	// An L4 route on the port a front of L4 routes alone leaves unpublished is no clash.
	in.R.Spec.Front = f
	in.R.Spec.L4.Routes = append(l4, spec.L4Route{Name: "web", Protocol: "tcp", Port: 443, Upstream: "db:443"})
	p, _ = in.publish("internal")
	if err := in.l4Clash(p); err != nil {
		t.Errorf("clash: %v", err)
	}
	in.R.Spec.Front = l4TLSFront()
	p, _ = in.publish("internal")
	if err := in.l4Clash(p); err == nil {
		t.Error("an L4 route on the front's served HTTPS port is no clash")
	}
}

// An upgrade whose release serves the front's ports the running one left unpublished (its first
// route on a front of L4 routes alone) checks them free first; a re-run moving such a front to
// acme checks free the HTTP port the challenge takes.
func TestTheFrontsPortsServedFirstAreCheckedFree(t *testing.T) {
	t.Parallel()
	l4only := func(s *spec.Spec) {
		s.Front.Routes = nil
		s.L4.Routes = append(s.L4.Routes, spec.L4Route{Name: "db", Protocol: "tcp", Port: 7443, Upstream: "web:5432", TLS: &spec.L4TLS{DomainInput: "DB_DOMAIN"}})
	}
	e := newEnv(t)
	e.docker.dump = "DUMP"
	e.schemaAt("5", nil, true)
	e.to("1.0.0", l4only)
	e.vars["DB_DOMAIN"] = "db.example.org"
	e.busy[80], e.busy[443] = true, true
	if err := e.install(func(o *Options) { o.TLS = "internal" }); err != nil {
		t.Fatalf("a front of L4 routes alone takes held 80 and 443: %v\n%s", err, e.log)
	}
	if err := e.installer(func(o *Options) { o.TLS = "acme"; o.Set = map[string]string{ACMEEmailVar: "ops@example.org"} }).Install(context.Background()); err == nil || !strings.Contains(err.Error(), "80 (http)") || strings.Contains(err.Error(), "443") {
		t.Errorf("a re-run to acme: %v", err)
	}
	e.vars["APP_DOMAIN"], e.vars["API_DOMAIN"] = "app.example.org", "api.example.org"
	e.to("2.0.0", func(s *spec.Spec) {
		routes := s.Front.Routes
		l4only(s)
		s.Front.Routes = routes
	})
	if err := e.upgrade(nil); err == nil || !strings.Contains(err.Error(), "80 (http), 443 (https)") {
		t.Errorf("an upgrade serving 80 and 443 first: %v", err)
	}
	e.busy[80], e.busy[443] = false, false
	if err := e.upgrade(nil); err != nil {
		t.Errorf("free: %v\n%s", err, e.log)
	}
}

// A cloud module's firewall opens the front's ports something serves on in the mode vm create
// gives, both when it gives none (the VM's install may take any).
func TestTheModulesPortsAreWhatServesOnThem(t *testing.T) {
	t.Parallel()
	in := &Installer{R: testRelease(t, "1.0.0")}
	f, l4 := l4OnlyFront()
	in.R.Spec.Front, in.R.Spec.L4.Routes = f, l4
	ports := func(mode string) []modulePort { return in.openedPorts(&Settings{TLS: mode}) }
	if got := ports("internal"); len(got) != 1 || got[0].Port != 7443 {
		t.Errorf("internal: %v", got)
	}
	if got := ports("acme"); len(got) != 2 || got[0].Port != 80 {
		t.Errorf("acme: %v", got)
	}
	if got := ports(""); len(got) != 3 || got[0].Port != 80 || got[1].Port != 443 {
		t.Errorf("no mode given: %v", got)
	}
}

// An L4 route's port is opened to what its allowlist holds as vm create gives it: the value given,
// each address a prefix of its own, else the input's default; none for an empty list (the VM's
// firewall refuses every source then), as the VM's firewall reads it (an IPv4-mapped address as
// IPv4, host bits masked, one another holds left out); every source for
// a route without a list, one the front terminates TLS on (the front filters its port; in
// loopback mode it is not opened), and a secret list, which vm create is not given.
func TestTheModulesPortsTakeTheRoutesAllowlists(t *testing.T) {
	t.Parallel()
	in := &Installer{R: testRelease(t, "1.0.0")}
	in.R.Spec.Front = spec.Front{}
	in.R.Spec.Inputs = append(in.R.Spec.Inputs, spec.Input{Key: "GIVEN"}, spec.Input{Key: "DEFAULTED", Default: "192.0.2.0/24"},
		spec.Input{Key: "EMPTY"}, spec.Input{Key: "SECRET", Secret: true})
	in.R.Spec.L4.Routes = []spec.L4Route{{Name: "given", Protocol: "tcp", Port: 1, AllowInput: "GIVEN"}, {Name: "defaulted", Protocol: "udp", Port: 2, AllowInput: "DEFAULTED"},
		{Name: "empty", Protocol: "tcp", Port: 3, AllowInput: "EMPTY"}, {Name: "none", Protocol: "tcp", Port: 4}, {Name: "secret", Protocol: "tcp", Port: 5, AllowInput: "SECRET"},
		{Name: "tls", Protocol: "tcp", Port: 6, AllowInput: "GIVEN", TLS: &spec.L4TLS{DomainInput: "DB_DOMAIN"}}}
	got := fmt.Sprint(in.openedPorts(&Settings{TLS: "none", Inputs: map[string]string{"GIVEN": "198.51.100.0/24, 2001:db8::/32 203.0.113.5,2001:db8::1 203.0.113.5/32 198.51.100.7/24 ::ffff:203.0.113.9", "EMPTY": ""}}))
	if want := "[{tcp 1 [198.51.100.0/24 203.0.113.5/32 203.0.113.9/32 2001:db8::/32]} {udp 2 [192.0.2.0/24]} {tcp 3 []} {tcp 4 [0.0.0.0/0 ::/0]} {tcp 5 [0.0.0.0/0 ::/0]}]"; got != want {
		t.Errorf("ports %s\nwant  %s", got, want)
	}
	// Another mode than loopback: the front serves the route it terminates TLS on, its sources its own.
	got = fmt.Sprint(in.openedPorts(&Settings{TLS: "internal", Inputs: map[string]string{"GIVEN": "", "EMPTY": ""}})[5])
	if want := "{tcp 6 [0.0.0.0/0 ::/0]}"; got != want {
		t.Errorf("a terminated route: %s, want %s", got, want)
	}
}

// A served port's recorded value that is no TCP port fails, naming it; an unserved one's is never
// read, whatever it holds.
func TestOnlyServedFrontPortsAreRead(t *testing.T) {
	t.Parallel()
	in := &Installer{R: testRelease(t, "1.0.0")}
	f, l4 := l4OnlyFront()
	in.R.Spec.Front, in.R.Spec.L4.Routes = f, l4
	vals := map[string]string{HTTPPortVar: "80", HTTPSPortVar: "STOWAGE_HTTP_PORT nonsense"}
	get := func(k string) string { return vals[k] }
	if ports, err := in.frontServedPorts("acme", get); err != nil || len(ports) != 1 || ports[0] != 80 {
		t.Errorf("acme, HTTP alone: %v %v", ports, err)
	}
	vals[HTTPPortVar] = "x"
	if _, err := in.frontServedPorts("acme", get); err == nil || !strings.Contains(err.Error(), HTTPPortVar+` "x" is not a TCP port`) {
		t.Errorf("a served one bad: %v", err)
	}
}
