package install

import (
	"bytes"
	"context"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/spec"
)

// l4TLSRoutes are a pass-through route and the L4 routes the front terminates TLS on: one on a
// domain of its own, one on the app route's domain, behind an allowlist, one asking for a client
// certificate, and one whose allowlist is empty.
func l4TLSRoutes() []spec.L4Route {
	return []spec.L4Route{
		{Name: "plain", Protocol: "tcp", Port: 7000, Upstream: "db:7000"},
		{Name: "db", Protocol: "tcp", Port: 7443, Upstream: "db:5432", TLS: &spec.L4TLS{DomainInput: "DB_DOMAIN"}},
		{Name: "mq", Protocol: "tcp", Port: 8883, Upstream: "mq:1883", AllowInput: "MQ_SOURCES", TLS: &spec.L4TLS{DomainInput: "APP_DOMAIN"}},
		{Name: "admin", Protocol: "tcp", Port: 9443, Upstream: "db:9000", TLS: &spec.L4TLS{DomainInput: "ADMIN_DOMAIN", ClientAuth: true}},
		{Name: "shut", Protocol: "tcp", Port: 9444, Upstream: "db:9001", AllowInput: "SHUT_SOURCES", TLS: &spec.L4TLS{DomainInput: "DB_DOMAIN"}},
	}
}

var l4TLSValues = map[string]string{"MQ_SOURCES": "10.0.0.0/8, 192.0.2.7", "SHUT_SOURCES": ""}

func l4TLSFront() spec.Front {
	return spec.Front{TLSModes: []string{"internal", "byo", "none"}, Routes: []spec.Route{{Name: "app", DomainInput: "APP_DOMAIN", Upstream: "web:8080"}}}
}

// The front terminates TLS on each such route's port with the certificate Caddy holds for its
// domain, a domain no HTTP route has being a site of its own so that Caddy obtains it; the sources
// and the client certificate are the front's to check.
func TestL4TLSCaddyfile(t *testing.T) {
	t.Parallel()
	domains := map[string]string{"app": "app.example.test", "db": "db.example.test", "mq": "app.example.test", "admin": "admin.example.test", "shut": "db.example.test"}
	get := func(k string) string { return l4TLSValues[k] }
	cf := tlsCaddyfile(l4TLSFront(), l4TLSRoutes(), "internal", domains, "", "443", true, get, nil)
	global, sites, _ := strings.Cut(cf, "\n}\n")
	for _, want := range []string{
		"\tlocal_certs\n\tskip_install_trust\n\tlayer4 {\n",
		"\t\t# db, host port 7443\n\t\t:20001 {\n\t\t\troute {\n\t\t\t\ttls {\n\t\t\t\t\tconnection_policy {\n\t\t\t\t\t\tdefault_sni db.example.test\n\t\t\t\t\t\talpn stowage-no-alpn\n\t\t\t\t\t}\n\t\t\t\t}\n\t\t\t\tproxy db:5432\n\t\t\t}\n\t\t}\n",
		"\t\t:20002 {\n\t\t\t@stowage_allowed remote_ip 10.0.0.0/8 192.0.2.7\n\t\t\troute @stowage_allowed {\n\t\t\t\ttls {\n\t\t\t\t\tconnection_policy {\n\t\t\t\t\t\tdefault_sni app.example.test\n",
		"\t\t\t\t\t\tdefault_sni admin.example.test\n\t\t\t\t\t\talpn stowage-no-alpn\n\t\t\t\t\t\tclient_auth {\n\t\t\t\t\t\t\tmode require_and_verify\n\t\t\t\t\t\t\ttrust_pool file " + clientCAPath + "\n",
		"\t\t:20004 {\n\t\t\troute {\n\t\t\t\tclose\n\t\t\t}\n\t\t}\n",
	} {
		if !strings.Contains(global, want) {
			t.Errorf("the global options lack %q:\n%s", want, global)
		}
	}
	if strings.Contains(cf, ":7000") {
		t.Error("a pass-through route reaches the front")
	}
	// One site per domain no HTTP route serves, once; the app's domain is the app route's site.
	for d, n := range map[string]int{"\ndb.example.test {\n\trespond 404\n}\n": 1, "\nadmin.example.test {\n": 1, "\napp.example.test {\n": 1} {
		if c := strings.Count(sites, d); c != n {
			t.Errorf("%q %d times, want %d:\n%s", d, c, n, sites)
		}
	}
	// Without the client CA a route asking for a certificate refuses everyone; byo's site loads
	// its files.
	cf = tlsCaddyfile(l4TLSFront(), l4TLSRoutes(), "byo", domains, "", "443", false, get, nil)
	if !strings.Contains(cf, "\t\t# admin, host port 9443\n\t\t:20003 {\n\t\t\troute {\n\t\t\t\tclose\n") || strings.Contains(cf, "trust_pool") {
		t.Errorf("admin without a client CA:\n%s", cf)
	}
	if !strings.Contains(cf, "\ndb.example.test {\n\ttls "+frontTLS+"/cert.pem "+frontTLS+"/key.pem\n\trespond 404\n}\n") {
		t.Errorf("byo's site:\n%s", cf)
	}
	// No route the front terminates TLS on: no layer4 app.
	if cf := tlsCaddyfile(l4TLSFront(), l4TLSRoutes()[:1], "internal", domains, "", "443", false, get, nil); strings.Contains(cf, "layer4") {
		t.Errorf("layer4 without a TLS route:\n%s", cf)
	}
}

// A front with L4 TLS routes alone: in every TLS mode, each domain is a site answering 404 that
// makes Caddy obtain its certificate (acme's challenges on the front's ports, dns-cloudflare's
// through DNS, byo's files loaded), and there is no other site.
func TestL4TLSFrontWithoutRoutes(t *testing.T) {
	t.Parallel()
	f := spec.Front{TLSModes: []string{"acme", "dns-cloudflare", "byo", "internal"}}
	routes := []spec.L4Route{{Name: "db", Protocol: "tcp", Port: 7443, Upstream: "db:5432", TLS: &spec.L4TLS{DomainInput: "DB_DOMAIN"}}}
	domains := map[string]string{"db": "db.example.test"}
	for mode, want := range map[string][]string{
		"acme":           {"\tcert_issuer acme {\n\t\tdisable_tlsalpn_challenge\n\t}\n\temail ops@example.test\n", "\ndb.example.test {\n\trespond 404\n}\n"},
		"dns-cloudflare": {"\tdns cloudflare {env.CLOUDFLARE_API_TOKEN}\n", "\ndb.example.test {\n\trespond 404\n}\n"},
		"byo":            {"\ndb.example.test {\n\ttls " + frontTLS + "/cert.pem " + frontTLS + "/key.pem\n\trespond 404\n}\n"},
		"internal":       {"\tlocal_certs\n", "\ndb.example.test {\n\trespond 404\n}\n"},
	} {
		cf := tlsCaddyfile(f, routes, mode, domains, "ops@example.test", "443", false, func(string) string { return "" }, nil)
		for _, w := range append(want, "\t\t\t\t\t\tdefault_sni db.example.test\n") {
			if !strings.Contains(cf, w) {
				t.Errorf("%s: lacks %q:\n%s", mode, w, cf)
			}
		}
		if _, sites, _ := strings.Cut(cf, "\n}\n"); strings.Count(sites, " {\n") != 1 {
			t.Errorf("%s: want the L4 domain's site alone:\n%s", mode, sites)
		}
	}
}

// An install ends with its portal's URL, and with none for a front of L4 routes alone.
func TestRunningLine(t *testing.T) {
	t.Parallel()
	get := func(k string) string { return "https://" + strings.ToLower(k) + ".test" }
	if got := runningLine("notes", "1.0.0", portalURL(spec.Front{Portal: "app"}, get)); got != "notes 1.0.0 is running: https://stowage_url_app.test" {
		t.Errorf("with a portal: %q", got)
	}
	if got := runningLine("lookup", "1.0.2", portalURL(spec.Front{TLSModes: []string{"internal"}}, get)); got != "lookup 1.0.2 is running" {
		t.Errorf("without a portal: %q", got)
	}
}

// The front selects no ALPN protocol on an L4 route it terminates TLS on, unless the route
// declares its upstream's: the Caddyfile names noALPNMark, which the front's start, adapting it to
// JSON, makes the empty list (a Caddyfile cannot write one); loopback mode writes no TLS.
func TestL4TLSALPN(t *testing.T) {
	t.Parallel()
	routes := []spec.L4Route{
		{Name: "db", Protocol: "tcp", Port: 7443, Upstream: "db:5432", TLS: &spec.L4TLS{DomainInput: "DB_DOMAIN", ALPN: []string{"postgresql"}}},
		{Name: "mq", Protocol: "tcp", Port: 8883, Upstream: "mq:1883", TLS: &spec.L4TLS{DomainInput: "MQ_DOMAIN"}},
	}
	domains := map[string]string{"db": "db.example.test", "mq": "mq.example.test"}
	cf := tlsCaddyfile(spec.Front{TLSModes: []string{"internal"}}, routes, "internal", domains, "", "443", false, func(string) string { return "" }, nil)
	for _, want := range []string{"default_sni db.example.test\n\t\t\t\t\t\talpn postgresql\n", "default_sni mq.example.test\n\t\t\t\t\t\talpn " + noALPNMark + "\n"} {
		if !strings.Contains(cf, want) {
			t.Errorf("lacks %q:\n%s", want, cf)
		}
	}
	adapt := `caddy adapt --config /data/stowage-Caddyfile --adapter caddyfile >/tmp/stowage-caddy.json && sed -i 's/"alpn":\["stowage-no-alpn"\]/"alpn":[]/g' /tmp/stowage-caddy.json && if grep -q stowage-no-alpn /tmp/stowage-caddy.json; then echo "stowage-no-alpn left in /tmp/stowage-caddy.json: the front's ALPN cannot be cleared" >&2; exit 1; fi && exec caddy run --config /tmp/stowage-caddy.json`
	f := spec.Front{TLSModes: []string{"internal"}}
	if got := frontCommand(f, "", true); !slices.Equal(got, []string{"sh", "-c", adapt}) {
		t.Errorf("noALPN: %q", got)
	}
	if got := frontCommand(f, "", false); !slices.Equal(got, []string{"caddy", "run", "--config", "/data/stowage-Caddyfile", "--adapter", "caddyfile"}) {
		t.Errorf("without: %q", got)
	}
	f.PublishCA = &spec.PublishCA{Volume: "ca"}
	if got := frontCommand(f, localRoot, true); len(got) != 3 || !strings.HasPrefix(got[2], "(until [ -s "+localRoot+" ]") || !strings.HasSuffix(got[2], ") & "+adapt) {
		t.Errorf("publish_ca and noALPN: %q", got)
	}
	// byo without a CA removes the published one, then starts adapted.
	if got := frontCommand(f, "", true); len(got) != 3 || got[2] != "rm -f "+caMount+"/ca.crt; "+adapt {
		t.Errorf("publish_ca without a CA and noALPN: %q", got)
	}
	// The compose file runs the adapted start when the Caddyfile names the mark: in a TLS mode,
	// not in loopback mode, nor when every route declares its protocols.
	get := func(string) string { return "" }
	declared := []spec.L4Route{routes[0]}
	for name, c := range map[string]struct {
		mode   string
		routes []spec.L4Route
		want   bool
	}{"tls": {"internal", routes, true}, "loopback": {Loopback, routes, false}, "declared": {"internal", declared, false}} {
		front := spec.Front{TLSModes: []string{"internal", "none"}}
		var cf string
		if c.mode == Loopback {
			cf = loopbackCaddyfile(front, c.routes, get, nil)
		} else {
			cf = tlsCaddyfile(front, c.routes, c.mode, domains, "", "443", false, get, nil)
		}
		fc, err := frontCompose(front, c.routes, get, c.mode, hostAPI{}, "d", "", strings.Contains(cf, noALPNMark), false, false, nil, nil, false, FrontUID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(fc), "caddy adapt") != c.want {
			t.Errorf("%s: adapted start %v, want %v:\n%s", name, !c.want, c.want, fc)
		}
	}
}

// In loopback mode the front passes such a route in the clear: TLS is what is in front of the
// install's; one asking for a client certificate refuses everyone, as plain TCP carries none.
func TestL4TLSLoopbackCaddyfile(t *testing.T) {
	t.Parallel()
	cf := loopbackCaddyfile(l4TLSFront(), l4TLSRoutes(), func(k string) string { return l4TLSValues[k] }, nil)
	for _, want := range []string{
		"\t\t:20001 {\n\t\t\troute {\n\t\t\t\tproxy db:5432\n\t\t\t}\n\t\t}\n",
		// No allowlist: published on the loopback address alone, its sources are the proxy's.
		"\t\t:20002 {\n\t\t\troute {\n\t\t\t\tproxy mq:1883\n",
		"\t\t# admin, host port 9443\n\t\t:20003 {\n\t\t\troute {\n\t\t\t\tclose\n",
	} {
		if !strings.Contains(cf, want) {
			t.Errorf("lacks %q:\n%s", want, cf)
		}
	}
	if strings.Contains(cf, "tls {") || strings.Contains(cf, "respond 404") {
		t.Errorf("TLS in loopback mode:\n%s", cf)
	}
}

// Such a route's port is the front's, to a port of its container of its own (by the route's index),
// on the bind address when it has one, on the loopback address alone in loopback mode; its
// upstream publishes nothing.
func TestL4TLSFrontCompose(t *testing.T) {
	t.Parallel()
	routes := l4TLSRoutes()
	routes[1].BindInput = "DB_BIND"
	for mode, hostIP := range map[string]string{"internal": "192.0.2.9", Loopback: "${STOWAGE_LOOPBACK_ADDRESS}"} {
		get := func(k string) string { return map[string]string{"DB_BIND": "192.0.2.9"}[k] }
		raw, err := frontCompose(l4TLSFront(), routes, get, mode, hostAPI{}, "sha256:x", "", false, false, false, []string{"db"}, nil, false, FrontUID)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Services map[string]struct {
				Ports []any `yaml:"ports"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		var db map[string]any
		for _, p := range doc.Services[FrontService].Ports {
			if m, ok := p.(map[string]any); ok && m["published"] == "7443" {
				db = m
			}
		}
		if db == nil || db["target"] != 20001 || db["protocol"] != "tcp" || db["host_ip"] != hostIP {
			t.Errorf("%s: the front's ports %v", mode, doc.Services[FrontService].Ports)
		}
		if p := doc.Services["mq"].Ports; len(p) != 0 {
			t.Errorf("%s: the upstream publishes %v", mode, p)
		}
	}
}

// The front filters such a route's sources: a Docker host's allowlists leave it out, and status
// says who decides them, for which domain.
func TestL4TLSExposure(t *testing.T) {
	t.Parallel()
	in := &Installer{R: &Release{Spec: &spec.Spec{Name: "notes", Front: l4TLSFront(), L4: spec.L4{Routes: append(l4TLSRoutes(), spec.L4Route{Name: "raw", Protocol: "tcp", Port: 7001, Upstream: "db:7001", AllowInput: "MQ_SOURCES"})}}}}
	var names []string
	for _, r := range in.allowed() {
		names = append(names, r.Name)
	}
	if !slices.Equal(names, []string{"raw"}) {
		t.Errorf("allowed %v", names)
	}
	in.O.Dir = t.TempDir()
	vals := map[string]string{"STOWAGE_TLS_MODE": "internal", "MQ_SOURCES": "10.0.0.0/8", "APP_DOMAIN": "app.example.test", "ADMIN_DOMAIN": "admin.example.test"}
	get := func(k string) string { return vals[k] }
	r := in.R.Spec.L4.Routes
	// What the front closes to everyone is said so, never as served.
	for i, want := range map[int]string{
		1: "closed to every source: its domain DB_DOMAIN is empty",
		2: "on every address, TLS by the front for app.example.test, from the sources MQ_SOURCES holds (the front closes any other)",
		3: "closed to every source: it asks for a client certificate, and the install holds no client CA (--client-ca)",
		4: "closed to every source: SHUT_SOURCES is empty",
	} {
		if got := in.l4Exposure(r[i], get, nil, in.readHeldCA()); got != want {
			t.Errorf("%s: %q", r[i].Name, got)
		}
	}
	if err := os.MkdirAll(filepath.Join(in.O.Dir, ClientCADir), 0o755); err != nil {
		t.Fatal(err)
	}
	// A file that holds no CA is none, as the front reads it: still closed. (Each step reads it as a
	// run does.)
	if err := os.WriteFile(filepath.Join(in.O.Dir, ClientCAFile), []byte("not PEM"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := in.l4Exposure(r[3], get, nil, in.readHeldCA()); !strings.HasPrefix(got, "closed to every source: it asks for a client certificate") {
		t.Errorf("admin with a file holding no CA: %q", got)
	}
	ca, _ := testRoot(t, true)
	if err := os.WriteFile(filepath.Join(in.O.Dir, ClientCAFile), ca, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := in.l4Exposure(r[3], get, nil, in.readHeldCA()); got != "on every address, TLS by the front for admin.example.test with a client certificate" {
		t.Errorf("admin with a client CA: %q", got)
	}
	vals["STOWAGE_TLS_MODE"], vals["DB_DOMAIN"] = Loopback, ""
	for _, i := range []int{1, 2, 4} { // its allowlist, empty or not, is not the front's there
		if got := in.l4Exposure(r[i], get, nil, in.readHeldCA()); got != "on the loopback address alone, in the clear: TLS and the sources are the proxy's in front of this install" {
			t.Errorf("loopback %s: %q", r[i].Name, got)
		}
	}
	// A client CA that cannot be read is said so, never as none.
	if err := os.Chmod(filepath.Join(in.O.Dir, ClientCAFile), 0); err != nil {
		t.Fatal(err)
	}
	vals["STOWAGE_TLS_MODE"] = "internal"
	if got := in.l4Exposure(r[3], get, nil, in.readHeldCA()); os.Geteuid() != 0 && !strings.HasPrefix(got, "unknown: the client CA cannot be read: ") {
		t.Errorf("an unreadable client CA: %q", got)
	}
	vals["STOWAGE_TLS_MODE"] = Loopback
	if err := os.Chmod(filepath.Join(in.O.Dir, ClientCAFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := in.l4Exposure(r[3], get, nil, in.readHeldCA()); !strings.HasPrefix(got, "closed to every source: it asks for a client certificate, which loopback") {
		t.Errorf("loopback client_auth: %q", got)
	}
	// On the VM, a route opened on some interfaces alone says so.
	vals["STOWAGE_TLS_MODE"], vals["DB_DOMAIN"] = "internal", "db.example.test"
	in.O.Firewall = t.TempDir()
	db := r[1]
	db.Interfaces = []string{"lan"}
	if got := in.l4Exposure(db, get, nil, in.readHeldCA()); got != "on the interfaces lan alone, TLS by the front for db.example.test" {
		t.Errorf("on an interface: %q", got)
	}
}

// On the VM such a route's port is opened like the front's, from every source (the front filters
// them), none in loopback mode; its upstream's port is reached from the stack's bridges alone.
func TestL4TLSOnTheVMsFirewall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in := &Installer{R: testRelease(t, "1.0.0"), O: Options{Dir: t.TempDir(), Firewall: filepath.Join(t.TempDir(), "nftables.d"), Host: &nftHost{}, Log: io.Discard}}
	in.R.Spec.L4.Routes = []spec.L4Route{{Name: "db", Protocol: "tcp", Port: 7443, Upstream: "api:5432", AllowInput: "DB_SOURCES", TLS: &spec.L4TLS{DomainInput: "DB_DOMAIN"}}}
	env := NewEnvFile()
	env.Set("STOWAGE_TLS_MODE", "internal")
	env.Set(HTTPPortVar, "80")
	env.Set(HTTPSPortVar, "443")
	env.Set("DB_SOURCES", "192.0.2.0/24")
	if err := in.openPorts(ctx, env, hostNet{}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(in.O.Firewall, "notes-ports.nft")
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), "\ntcp dport 7443 accept\n") || strings.Contains(string(b), "saddr") {
		t.Errorf("rules %q", b)
	}
	env.Set("STOWAGE_TLS_MODE", Loopback)
	if err := in.openPorts(ctx, env, hostNet{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); strings.Contains(string(b), "7443") {
		t.Errorf("loopback rules %q", b)
	}
}

// --domains names an L4 route the front terminates TLS on; the install asks its domain as a
// route's when the project does not declare the input.
func TestL4TLSDomains(t *testing.T) {
	t.Parallel()
	in := &Installer{R: &Release{Spec: &spec.Spec{Name: "notes", Front: l4TLSFront(), L4: spec.L4{Routes: l4TLSRoutes()}}}}
	in.O.Domains = map[string]string{"db": "db.example.test"}
	if err := in.applyDomains(); err != nil || in.O.Set["DB_DOMAIN"] != "db.example.test" {
		t.Errorf("%v %v", err, in.O.Set)
	}
	in.O.Domains, in.O.Set = map[string]string{"plain": "x.example.test"}, nil
	if err := in.applyDomains(); err == nil || !strings.Contains(err.Error(), `"plain" is no route (admin, app, db, mq, shut)`) {
		t.Errorf("a pass-through route: %v", err)
	}
	var keys []string
	for _, i := range in.frontInputs("internal") {
		keys = append(keys, i.Key)
	}
	if !slices.Equal(keys, []string{"APP_DOMAIN", "DB_DOMAIN", "ADMIN_DOMAIN"}) {
		t.Errorf("asked %v", keys)
	}
	if got := in.frontInputs(Loopback); got != nil {
		t.Errorf("loopback asks %v", got)
	}
}

// On the VM the stack's bridges reach a TLS route's upstream at its own port, never the route's,
// which is the front's.
func TestL4TLSUpstreamPortFromTheBridges(t *testing.T) {
	t.Parallel()
	in, m, _ := hostNetInstaller(t, true)
	in.R.Spec.L4.Routes = append(in.R.Spec.L4.Routes, spec.L4Route{Name: "echo-tls", Protocol: "tcp", Port: 17443, Upstream: "echo:17009", TLS: &spec.L4TLS{DomainInput: "ECHO_DOMAIN"}})
	hn, err := in.hostNetwork(context.Background(), m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := hn.bridged(); !slices.Contains(b, peerPort{17009, "tcp"}) || slices.Contains(b, peerPort{17443, "tcp"}) {
		t.Errorf("bridged %v", b)
	}
}

// An allowlist of an L4 route the front filters is warned as a front route's when every IPv6
// client passes it.
func TestL4TLSIPv6Allowlist(t *testing.T) {
	t.Parallel()
	var log bytes.Buffer
	s := &spec.Spec{L4: spec.L4{Routes: []spec.L4Route{
		{Name: "raw", Protocol: "tcp", Port: 7000, Upstream: "db:7000", AllowInput: "SOURCES"},
		{Name: "db", Protocol: "tcp", Port: 7443, Upstream: "db:5432", AllowInput: "SOURCES", TLS: &spec.L4TLS{DomainInput: "DB_DOMAIN"}},
	}}}
	in := &Installer{R: &Release{Spec: s}, O: Options{Log: &log, HostAddrs: func() []string { return []string{"2001:db8::10"} }}}
	gw, _ := netip.ParseAddr("172.18.0.1")
	in.warnIPv6Allowlists(func(k string) string { return map[string]string{"SOURCES": "172.18.0.0/16"}[k] }, gw)
	if !strings.HasPrefix(log.String(), "warning: route db: SOURCES=172.18.0.0/16: ") || strings.Contains(log.String(), "route raw") {
		t.Errorf("log %q", log.String())
	}
	// In loopback mode the route takes no allowlist: nothing to warn of.
	log.Reset()
	in.warnIPv6Allowlists(func(k string) string {
		return map[string]string{"SOURCES": "172.18.0.0/16", "STOWAGE_TLS_MODE": Loopback}[k]
	}, gw)
	if log.Len() != 0 {
		t.Errorf("loopback: %q", log.String())
	}
}

// A domain input the project declares itself is checked as a route's domain when an L4 route the
// front terminates TLS on reads it: it reaches the front's configuration.
func TestL4TLSDeclaredDomainIsChecked(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.r.Spec.Inputs = append(e.r.Spec.Inputs, spec.Input{Key: "DB_DOMAIN", Prompt: "Database domain"})
	e.r.Spec.L4.Routes = append(e.r.Spec.L4.Routes, spec.L4Route{Name: "db", Protocol: "tcp", Port: 7443, Upstream: "web:5432", TLS: &spec.L4TLS{DomainInput: "DB_DOMAIN"}})
	err := e.install(func(o *Options) {
		o.TLS = "internal"
		o.Set = map[string]string{"DB_DOMAIN": "db.example.org {"}
	})
	if err == nil || !strings.Contains(err.Error(), `"db.example.org {" is not a domain name`) {
		t.Errorf("err = %v", err)
	}
}

// An upgrade to a release that terminates TLS on an L4 route refuses a domain nothing gives, and
// serves the route once one is given.
func TestL4TLSUpgrade(t *testing.T) {
	t.Parallel()
	e := tlsInstalled(t)
	db := func(s *spec.Spec) {
		s.L4.Routes = append(s.L4.Routes, spec.L4Route{Name: "db", Protocol: "tcp", Port: 7443, Upstream: "web:5432", TLS: &spec.L4TLS{DomainInput: "DB_DOMAIN"}})
	}
	e.to("2.0.0", db)
	if err := e.upgrade(nil); err == nil || !strings.Contains(err.Error(), "DB_DOMAIN (Domain for db)") {
		t.Errorf("no domain: %v", err)
	}
	// A domain input the project declares for another shape alone: refused, naming it.
	e.to("2.0.0", func(s *spec.Spec) {
		db(s)
		s.Inputs = append(s.Inputs, spec.Input{Key: "DB_DOMAIN", Prompt: "Database domain", Shapes: []string{"kairos"}})
	})
	if err := e.upgrade(nil); err == nil || !strings.Contains(err.Error(), "notes 2.0.0 terminates TLS on the L4 route db, whose domain DB_DOMAIN nothing gives: --set DB_DOMAIN=<domain>") {
		t.Errorf("a domain this shape never asks: %v", err)
	}
	e.to("2.0.0", db)
	if err := e.upgrade(func(o *Options) { o.Set = map[string]string{"DB_DOMAIN": "db.example.org"} }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if cf := read(t, filepath.Join(e.dir, Caddyfile)); !strings.Contains(cf, "default_sni db.example.org") || !strings.Contains(cf, "\ndb.example.org {\n") {
		t.Errorf("the front does not serve db:\n%s", cf)
	}
}

// An L4 TLS route's domain is read by the front: a command provider's container gets it, as a
// front route's, and the provision hook never holds it alone (provision --set would leave the front
// serving the old domain).
func TestL4TLSDomainIsTheFronts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.r.Spec.Inputs = append(e.r.Spec.Inputs, spec.Input{Key: "DB_DOMAIN", Prompt: "Database domain", Optional: true})
	e.r.Spec.L4.Routes = append(e.r.Spec.L4.Routes, spec.L4Route{Name: "db", Protocol: "tcp", Port: 7443, Upstream: "web:5432", TLS: &spec.L4TLS{DomainInput: "DB_DOMAIN"}})
	in := e.installer(nil)
	f := NewEnvFile()
	f.Set("DB_DOMAIN", "db.example.org")
	f.Set("MQ_DOMAIN", "mq.example.org")
	// A domain the project does not declare, which the install asks as a route's.
	e.r.Spec.L4.Routes = append(e.r.Spec.L4.Routes, spec.L4Route{Name: "mq", Protocol: "tcp", Port: 8883, Upstream: "web:1883", TLS: &spec.L4TLS{DomainInput: "MQ_DOMAIN"}})
	if keys := in.recordedEnv(f); !slices.Contains(keys, "DB_DOMAIN") || !slices.Contains(keys, "MQ_DOMAIN") {
		t.Errorf("recordedEnv %v", keys)
	}
	env := []string{"DB_DOMAIN"}
	h := &providers.ProvisionHook{Image: "api", Env: &env}
	if got := in.hookInputs(f, h); len(got) != 0 {
		t.Errorf("hook-only inputs %v", got)
	}
}

// In loopback mode an L4 TLS route's domain given is refused as a front route's is, with its hint.
func TestL4TLSDomainInLoopbackIsHinted(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.r.Spec.L4.Routes = append(e.r.Spec.L4.Routes, spec.L4Route{Name: "db", Protocol: "tcp", Port: 7443, Upstream: "web:5432", TLS: &spec.L4TLS{DomainInput: "DB_DOMAIN"}})
	if err := e.install(func(o *Options) { o.TLS = Loopback; o.Set = map[string]string{"DB_DOMAIN": "db.example.org"} }); err == nil || !strings.Contains(err.Error(), "a domain was given: loopback mode serves no domain") {
		t.Errorf("a first install: %v", err)
	}
	if err := e.install(func(o *Options) { o.TLS = Loopback }); err != nil {
		t.Fatal(err)
	}
	if err := e.install(func(o *Options) { o.Set = map[string]string{"DB_DOMAIN": "db.example.org"} }); err == nil || !strings.Contains(err.Error(), "this install is in loopback mode, which serves no domain") {
		t.Errorf("a re-run: %v", err)
	}
}

// In byo the certificate must cover an L4 TLS route's domain, at the install and when a re-run
// changes it.
func TestL4TLSByoCoversTheDomain(t *testing.T) {
	t.Parallel()
	cert, key := writeCert(t, time.Now().Add(90*24*time.Hour), "*.example.com")
	e := newEnv(t)
	e.r.Spec.L4.Routes = append(e.r.Spec.L4.Routes, spec.L4Route{Name: "db", Protocol: "tcp", Port: 7443, Upstream: "web:5432", TLS: &spec.L4TLS{DomainInput: "DB_DOMAIN"}})
	byo := func(o *Options) {
		tlsDomains(o)
		o.Domains["db"] = "db.example.com"
		o.TLS, o.Cert, o.Key = "byo", cert, key
	}
	if err := e.install(byo); err != nil {
		t.Fatal(err)
	}
	if err := e.install(func(o *Options) { o.Set = map[string]string{"DB_DOMAIN": "db.example.org"} }); err == nil || !strings.Contains(err.Error(), "the certificate does not cover db.example.org") {
		t.Errorf("--set: %v", err)
	}
	if err := e.install(func(o *Options) { o.Domains = map[string]string{"db": "db.example.org"} }); err == nil || !strings.Contains(err.Error(), "the certificate does not cover db.example.org") {
		t.Errorf("--domains: %v", err)
	}
	if err := e.install(func(o *Options) { o.Domains = map[string]string{"db": "pg.example.com"} }); err != nil {
		t.Errorf("a covered domain: %v", err)
	}
}

// A re-run's new certificate is checked against every route's domain, an L4 TLS route's included,
// one refusal per domain input naming it and the routes it serves.
func TestL4TLSNewCertificateCoversItsDomains(t *testing.T) {
	t.Parallel()
	cert, key := writeCert(t, time.Now().Add(90*24*time.Hour), "*.example.com")
	e := newEnv(t)
	e.r.Spec.L4.Routes = append(e.r.Spec.L4.Routes,
		spec.L4Route{Name: "db", Protocol: "tcp", Port: 7443, Upstream: "web:5432", TLS: &spec.L4TLS{DomainInput: "DB_DOMAIN"}},
		spec.L4Route{Name: "mq", Protocol: "tcp", Port: 8883, Upstream: "web:1883", TLS: &spec.L4TLS{DomainInput: "APP_DOMAIN"}})
	if err := e.install(func(o *Options) {
		tlsDomains(o)
		o.Domains["db"] = "db.example.com"
		o.TLS, o.Cert, o.Key = "byo", cert, key
	}); err != nil {
		t.Fatal(err)
	}
	narrow, narrowKey := writeCert(t, time.Now().Add(90*24*time.Hour), "api.example.com")
	err := e.install(func(o *Options) { o.Cert, o.Key = narrow, narrowKey })
	if err == nil || !strings.Contains(err.Error(), "the certificate does not cover notes.example.com (APP_DOMAIN: app, mq)") ||
		!strings.Contains(err.Error(), "the certificate does not cover db.example.com (DB_DOMAIN: db)") || strings.Count(err.Error(), "notes.example.com") != 1 {
		t.Errorf("err = %v", err)
	}
}

// An L4 TLS route whose domain is empty is never served in the clear: the install refuses it, as
// a route's domain, even from an input the project declares optional, and the front's
// configuration closes such a route in a TLS mode whatever reaches it.
func TestL4TLSEmptyDomainIsNeverPlaintext(t *testing.T) {
	t.Parallel()
	domains := map[string]string{"app": "app.example.test", "mq": "app.example.test", "admin": "admin.example.test"}
	cf := tlsCaddyfile(l4TLSFront(), l4TLSRoutes(), "internal", domains, "", "443", true, func(string) string { return "" }, nil)
	if !strings.Contains(cf, "\t\t# db, host port 7443\n\t\t:20001 {\n\t\t\troute {\n\t\t\t\tclose\n") || strings.Contains(cf, "\n {\n") {
		t.Errorf("a route without its domain, or a site without an address:\n%s", cf)
	}
	e := newEnv(t)
	e.r.Spec.Inputs = append(e.r.Spec.Inputs, spec.Input{Key: "DB_DOMAIN", Prompt: "Database domain", Optional: true})
	e.r.Spec.L4.Routes = append(e.r.Spec.L4.Routes, spec.L4Route{Name: "db", Protocol: "tcp", Port: 7443, Upstream: "web:5432", TLS: &spec.L4TLS{DomainInput: "DB_DOMAIN"}})
	err := e.install(func(o *Options) { o.TLS = "internal"; tlsDomains(o) })
	if err == nil || !strings.Contains(err.Error(), `DB_DOMAIN: "" is not a domain name`) {
		t.Errorf("err = %v", err)
	}
}

// A project whose one route asking for a client certificate is an L4 route takes --client-ca: the
// front mounts it and the route asks for a certificate it issued, instead of closing everyone.
func TestL4TLSClientAuthTakesTheClientCA(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.r.Spec.L4.Routes = append(e.r.Spec.L4.Routes, spec.L4Route{Name: "db", Protocol: "tcp", Port: 7443, Upstream: "web:5432", TLS: &spec.L4TLS{DomainInput: "DB_DOMAIN", ClientAuth: true}})
	if names := clientAuthRoutes(e.r.Spec); !slices.Equal(names, []string{"db"}) {
		t.Fatalf("clientAuthRoutes %v", names)
	}
	ca, _ := testRoot(t, true)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, ca, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.install(func(o *Options) {
		o.TLS = "internal"
		tlsDomains(o)
		o.Domains["db"] = "db.example.com"
		o.ClientCA = caFile
	}); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	cf, fc := e.file(Caddyfile), e.file(FrontCompose)
	if !strings.Contains(cf, "default_sni db.example.com\n\t\t\t\t\t\talpn stowage-no-alpn\n\t\t\t\t\t\tclient_auth {") || strings.Contains(cf, "\t\t\t\tclose\n") {
		t.Errorf("Caddyfile:\n%s", cf)
	}
	if !strings.Contains(fc, "./"+ClientCADir+":"+clientCADir+":ro") {
		t.Errorf("the front does not mount the client CA:\n%s", fc)
	}
}
