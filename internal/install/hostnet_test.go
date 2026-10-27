package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/tune"
)

// hostNetStack is a stack with an L4 route's service (echo) depending on two services exposing
// ports (db in its compose file, cache in its image) and on one exposing none (migrate), beside a
// service sharing echo's namespace (side) and one depending on echo (data).
const hostNetStack = `{"services":{
	"stowage-front":{"image":"caddy"},
	"echo":{"image":"ingest","depends_on":{"db":{},"cache":{},"migrate":{}},"expose":["9100"]},
	"db":{"image":"postgres","expose":["5432","5433/udp"]},
	"cache":{"image":"redis"},
	"migrate":{"image":"tool"},
	"side":{"image":"x","network_mode":"service:echo"},
	"data":{"image":"d","depends_on":{"echo":{}}}}}`

// hostNetInstaller is an installer of hostNetStack's routes, on the VM when vm, whose engine
// answers each image's EXPOSE.
func hostNetInstaller(t *testing.T, vm bool) (*Installer, *composeModel, *bytes.Buffer) {
	t.Helper()
	r, _ := testInstallerWith(t, "1.0.0", func(s *spec.Spec) {
		s.L4.Routes = []spec.L4Route{
			{Name: "echo", Protocol: "tcp", Port: 17007, Upstream: "echo:17007"},
			{Name: "echo-udp", Protocol: "udp", Port: 17007, Upstream: "echo:17007"},
		}
		s.Front.Routes = append(s.Front.Routes, spec.Route{Name: "metrics", Upstream: "echo:9090"})
	})
	exposes := map[string]string{"postgres": `{"5432/tcp":{}}`, "redis": `{"6379/tcp":{}}`, "tool": "null", "ingest": `{"8080/tcp":{}}`}
	d := &fakeDocker{answer: func(a string) ([]byte, bool, error) {
		if img, ok := strings.CutPrefix(a, "image inspect --format {{json .Config.ExposedPorts}} "); ok {
			return []byte(exposes[img] + "\n"), true, nil
		}
		return nil, false, nil
	}}
	var log bytes.Buffer
	in := &Installer{R: r, O: Options{Docker: d, Log: &log}}
	if vm {
		in.O.Firewall = t.TempDir()
	}
	var m composeModel
	if err := json.Unmarshal([]byte(hostNetStack), &m); err != nil {
		t.Fatal(err)
	}
	return in, &m, &log
}

// The VM runs an L4 route's service on the host's network: it reaches the services it depends on
// by name at an address of each one's own, where their exposed ports are published; every service of a network of
// its own reaches it by name at the host's gateway; the firewall opens to the bridges the routes'
// ports, the front's routes' and the ports it exposes. It keeps no namespace's sysctls.
func TestTheVMRunsTheL4RoutesServicesOnTheHostsNetwork(t *testing.T) {
	t.Parallel()
	in, m, log := hostNetInstaller(t, true)
	hn, err := in.hostNetwork(context.Background(), m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []peerPort{{8080, "tcp"}, {9090, "tcp"}, {9100, "tcp"}, {17007, "tcp"}, {17007, "udp"}}; !slices.Equal(hn.bridged(), want) {
		t.Errorf("bridged %v, want %v", hn.bridged(), want)
	}
	if !strings.Contains(log.String(), "echo (an L4 route's, on the host's network) depends on migrate, which exposes no port it can be published on: it cannot reach migrate by name (give migrate's ports in its expose:)") {
		t.Errorf("log %q", log)
	}
	raw, err := contentCompose(m, nil, []byte("key"), tune.Container{Sysctls: map[string]string{"net.core.somaxconn": "4096"}}, hostPlan{}, hn)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]struct {
			NetworkMode string            `yaml:"network_mode"`
			ExtraHosts  []string          `yaml:"extra_hosts"`
			Sysctls     map[string]string `yaml:"sysctls"`
			Ports       []struct {
				HostIP    string `yaml:"host_ip"`
				Target    int    `yaml:"target"`
				Published string `yaml:"published"`
				Protocol  string `yaml:"protocol"`
			} `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	echo := doc.Services["echo"]
	if echo.NetworkMode != "host" || !slices.Equal(echo.ExtraHosts, []string{"cache:" + hn.addrs["cache"], "db:" + hn.addrs["db"]}) || hn.addrs["cache"] == hn.addrs["db"] || echo.Sysctls != nil || echo.Ports != nil {
		t.Errorf("echo %+v", echo)
	}
	db := doc.Services["db"]
	if len(db.Ports) != 2 || db.Ports[0].HostIP != hn.addrs["db"] || db.Ports[0].Target != 5432 || db.Ports[0].Published != "5432" || db.Ports[0].Protocol != "tcp" ||
		db.Ports[1].Target != 5433 || db.Ports[1].Protocol != "udp" || !slices.Equal(db.ExtraHosts, []string{"echo:host-gateway"}) {
		t.Errorf("db %+v", db)
	}
	if c := doc.Services["cache"]; len(c.Ports) != 1 || c.Ports[0].Target != 6379 || c.Ports[0].HostIP != hn.addrs["cache"] {
		t.Errorf("cache %+v", c)
	}
	if mg := doc.Services["migrate"]; mg.Ports != nil {
		t.Errorf("migrate %+v", mg)
	}
	for _, name := range []string{"stowage-front", "data"} {
		if s := doc.Services[name]; !slices.Equal(s.ExtraHosts, []string{"echo:host-gateway"}) || s.Sysctls["net.core.somaxconn"] != "4096" {
			t.Errorf("%s %+v", name, s)
		}
	}
	if side := doc.Services["side"]; side.ExtraHosts != nil || side.Sysctls != nil {
		t.Errorf("side %+v", side)
	}
}

// Docker publishes the routes' ports from the stack's network: nothing of the host's network.
func TestDockerRunsTheL4RoutesServicesOnTheStacksNetwork(t *testing.T) {
	t.Parallel()
	in, m, _ := hostNetInstaller(t, false)
	hn, err := in.hostNetwork(context.Background(), m, nil)
	if err != nil || hn.targets != nil || hn.bridged() != nil {
		t.Fatalf("%+v %v", hn, err)
	}
	raw, err := contentCompose(m, nil, []byte("key"), tune.Container{}, hostPlan{}, hn)
	if err != nil || strings.Contains(string(raw), "network_mode") || strings.Contains(string(raw), "extra_hosts") || string(raw) != contentPlaceholder {
		t.Errorf("%s %v", raw, err)
	}
}

// An L4 route's service another profile turned off is not run: nothing is planned for it.
func TestAnL4RoutesServiceLeftOutPlansNothing(t *testing.T) {
	t.Parallel()
	in, m, _ := hostNetInstaller(t, true)
	delete(m.Services, "echo")
	if hn, err := in.hostNetwork(context.Background(), m, nil); err != nil || hn.targets != nil {
		t.Errorf("%+v %v", hn, err)
	}
}

func TestExposedPorts(t *testing.T) {
	t.Parallel()
	for s, want := range map[string][]peerPort{
		"5432":      {{5432, "tcp"}},
		"53/UDP":    {{53, "udp"}},
		"1/tcp":     {{1, "tcp"}},
		"65535":     {{65535, "tcp"}},
		"8000-8001": {{8000, "tcp"}, {8001, "tcp"}},
		"7-7":       {{7, "tcp"}},
	} {
		if got, err := exposedPorts(s); err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: %v %v, want %v", s, got, err, want)
		}
	}
	if got, err := exposedPorts("1000-1063"); err != nil || len(got) != maxExposedRange {
		t.Errorf("a range of %d: %d %v", maxExposedRange, len(got), err)
	}
	for _, s := range []string{"", "0", "65536", "x", "80/sctp", "9-8", "1000-1064", "80-", "-80", "80/tcp/x"} {
		if got, err := exposedPorts(s); err == nil {
			t.Errorf("%q: %v", s, got)
		}
	}
}

// A port read from an image's EXPOSE that is not one fails the start, naming the service.
func TestABrokenExposeIsNamed(t *testing.T) {
	t.Parallel()
	in, m, _ := hostNetInstaller(t, true)
	svc := m.Services["db"]
	svc.Expose = []string{"5432/sctp"}
	m.Services["db"] = svc
	if _, err := in.hostNetwork(context.Background(), m, nil); err == nil || err.Error() != `services.db.expose: "5432/sctp": tcp or udp` {
		t.Errorf("%v", err)
	}
}

// In a TLS mode each route's domain names the front on the stack's network, once; loopback mode
// has no domain.
func TestTheFrontAnswersToItsRoutesDomainsInside(t *testing.T) {
	t.Parallel()
	f := spec.Front{Routes: []spec.Route{
		{Name: "app", DomainInput: "APP_DOMAIN", Upstream: "web:80"},
		{Name: "api", DomainInput: "API_DOMAIN", Upstream: "api:8000"},
		{Name: "idp", DomainInput: "IDP_DOMAIN", Upstream: "idp:5556"},
	}}
	vals := map[string]string{"APP_DOMAIN": "notes.example.com", "API_DOMAIN": "api.example.com", HTTPSPortVar: "443"}
	get := func(k string) string { return vals[k] }
	aliases := func(mode string) []string {
		t.Helper()
		raw, err := frontCompose(f, nil, get, mode, hostAPI{}, "sha256:x", "", false, false, false)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Services map[string]struct {
				Networks map[string]struct {
					Aliases []string `yaml:"aliases"`
				} `yaml:"networks"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Services[FrontService].Networks["default"].Aliases
	}
	if got := aliases("internal"); !slices.Equal(got, []string{"api.example.com", "notes.example.com"}) {
		t.Errorf("aliases %v", got)
	}
	if got := aliases(Loopback); got != nil {
		t.Errorf("loopback: %v", got)
	}
	// A moved port: the front answers its URLs inside all the same, on 443 in its container (acme's
	// URLs) or on the URL's port (the site's other address).
	vals[HTTPSPortVar] = "8443"
	for _, mode := range []string{"internal", "acme"} {
		if got := aliases(mode); len(got) != 2 {
			t.Errorf("%s on 8443: %v", mode, got)
		}
	}
}

// On the host's network an L4 route's service reaches each route's domain at the loopback, where
// the front publishes its ports: the alias the stack's network gives the front is not there.
func TestATargetReachesTheRoutesDomainsAtTheLoopback(t *testing.T) {
	t.Parallel()
	hn := hostNet{targets: map[string]bool{"idp": true, "dns": true}, domains: []string{"api.example.com", "notes.example.com"}}
	extra := map[string]any{}
	hn.give(extra, "idp", "")
	if got := extra["extra_hosts"]; !slices.Equal(got.([]string), []string{"dns:127.0.0.1", "api.example.com:127.0.0.1", "notes.example.com:127.0.0.1"}) {
		t.Errorf("a target's hosts %v", got)
	}
	extra = map[string]any{}
	hn.give(extra, "web", "")
	if got := extra["extra_hosts"]; !slices.Equal(got.([]string), []string{"dns:host-gateway", "idp:host-gateway"}) {
		t.Errorf("another service's hosts %v", got)
	}
	f := spec.Front{Routes: []spec.Route{{Name: "a", DomainInput: "A"}, {Name: "b", DomainInput: "A", Path: "/b"}}}
	vals := map[string]string{"A": "x.example.com", HTTPSPortVar: "443"}
	get := func(k string) string { return vals[k] }
	if got := hostDomains(f, "acme", get); !slices.Equal(got, []string{"x.example.com"}) {
		t.Errorf("one domain of two routes: %v", got)
	}
	// acme on a moved port: its URLs name 443, which nothing on the host holds.
	vals[HTTPSPortVar] = "8443"
	if got := hostDomains(f, "acme", get); got != nil {
		t.Errorf("acme on 8443: %v", got)
	}
	if got := hostDomains(f, "internal", get); len(got) != 1 {
		t.Errorf("internal on 8443: %v", got)
	}
}

// Two L4 routes' services sharing a peer reach it at one address; each peer has its own, up to
// maxPeers.
func TestEachPeerHasAnAddressOfItsOwn(t *testing.T) {
	t.Parallel()
	model := func(peers int) *composeModel {
		var m composeModel
		svcs := map[string]any{}
		deps := map[string]any{}
		for i := range peers {
			name := fmt.Sprintf("p%03d", i)
			svcs[name] = map[string]any{"expose": []string{"8080"}}
			deps[name] = map[string]any{}
		}
		svcs["a"] = map[string]any{"depends_on": deps}
		svcs["b"] = map[string]any{"depends_on": map[string]any{"p000": map[string]any{}}}
		raw, _ := json.Marshal(map[string]any{"services": svcs})
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return &m
	}
	r, _ := testInstallerWith(t, "1.0.0", func(s *spec.Spec) {
		s.L4.Routes = []spec.L4Route{{Name: "a", Protocol: "tcp", Port: 7000, Upstream: "a:7000"}, {Name: "b", Protocol: "tcp", Port: 7001, Upstream: "b:7001"}}
	})
	in := &Installer{R: r, O: Options{Docker: &fakeDocker{}, Firewall: t.TempDir(), Log: &bytes.Buffer{}}}
	hn, err := in.hostNetwork(context.Background(), model(maxPeers), nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, a := range hn.addrs {
		if seen[a] || !strings.HasPrefix(a, "127.0.4.") || a == "127.0.4.0" || a == "127.0.4.255" {
			t.Errorf("address %s twice or outside 127.0.4.1-254", a)
		}
		seen[a] = true
	}
	if len(hn.addrs) != maxPeers {
		t.Errorf("%d addresses", len(hn.addrs))
	}
	a, b := map[string]any{}, map[string]any{}
	hn.give(a, "a", "")
	hn.give(b, "b", "")
	if ah, bh := a["extra_hosts"].([]string), b["extra_hosts"].([]string); !slices.Contains(ah, "p000:"+hn.addrs["p000"]) || !slices.Contains(bh, "p000:"+hn.addrs["p000"]) {
		t.Errorf("a %v, b %v", ah, bh)
	}
	if _, err := in.hostNetwork(context.Background(), model(maxPeers+1), nil); err == nil || !strings.Contains(err.Error(), "more than 254 services") {
		t.Errorf("one peer too many: %v", err)
	}
	// A peer more moves no other's address (these names do not clash).
	few, err := in.hostNetwork(context.Background(), model(10), nil)
	if err != nil {
		t.Fatal(err)
	}
	// One sorting first, as a release adding a dependency named before the others.
	m := model(10)
	m.Services["a00"] = m.Services["p000"]
	svc := m.Services["a"]
	svc.DependsOn["a00"] = map[string]any{}
	m.Services["a"] = svc
	more, _ := in.hostNetwork(context.Background(), m, nil)
	for p, a := range few.addrs {
		if more.addrs[p] != a {
			t.Errorf("%s moved from %s to %s", p, a, more.addrs[p])
		}
	}
}

// An image the engine cannot read leaves its EXPOSE out of this start, logged, rather than fail
// the start before it pulls the image again.
func TestAnImageGoneLeavesItsExposeOut(t *testing.T) {
	t.Parallel()
	in, m, log := hostNetInstaller(t, true)
	in.O.Docker.(*fakeDocker).answer = func(a string) ([]byte, bool, error) {
		if strings.HasPrefix(a, "image inspect ") {
			return nil, true, errors.New("No such image")
		}
		return nil, false, nil
	}
	hn, err := in.hostNetwork(context.Background(), m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(hn.peers["db"], []peerPort{{5432, "tcp"}, {5433, "udp"}}) || len(hn.peers["cache"]) != 0 {
		t.Errorf("peers %v", hn.peers)
	}
	if !strings.Contains(log.String(), "the ports db's image exposes are not read (No such image): this start leaves them out") {
		t.Errorf("log %q", log)
	}
}

// A peer is published on none of the ports the host holds on every address, nor at all when its
// network is another service's or the host's; an image's EXPOSE the start cannot take is left
// out, logged, where a compose `expose:` fails.
func TestPeersPortsTheHostHoldsAreNotPublished(t *testing.T) {
	t.Parallel()
	in, m, log := hostNetInstaller(t, true)
	held := map[peerPort]bool{{5432, "tcp"}: true}
	// db exposes a port the target echo listens on itself, on every address, too.
	svc := m.Services["db"]
	svc.Expose = append(svc.Expose, "9100")
	m.Services["db"] = svc
	hn, err := in.hostNetwork(context.Background(), m, held)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(hn.peers["db"], []peerPort{{5433, "udp"}}) || !strings.Contains(log.String(), "db's tcp/5432 is not published for echo: the host holds it on every address") ||
		!strings.Contains(log.String(), "db's tcp/9100 is not published for echo") || held[peerPort{9100, "tcp"}] {
		t.Errorf("db %v, log %q", hn.peers["db"], log)
	}
	for _, mode := range []string{"service:vpn", "host"} {
		svc = m.Services["db"]
		svc.NetworkMode = mode
		m.Services["db"] = svc
		log.Reset()
		hn, err := in.hostNetwork(context.Background(), m, nil)
		if err != nil || slices.Contains(hn.reach["echo"], "db") || hn.addrs["db"] != "" || !strings.Contains(log.String(), "depends on db, whose network is "+mode) {
			t.Errorf("%s: %+v %v %q", mode, hn, err, log)
		}
	}
	in.O.Docker.(*fakeDocker).answer = func(a string) ([]byte, bool, error) {
		if strings.HasPrefix(a, "image inspect ") && strings.HasSuffix(a, " redis") {
			return []byte(`{"3478/udp":{},"49152-65535/udp":{},"5000/sctp":{}}`), true, nil
		}
		return nil, false, nil
	}
	log.Reset()
	hn, err = in.hostNetwork(context.Background(), m, nil)
	if err != nil || !slices.Contains(hn.peers["cache"], peerPort{3478, "udp"}) || !strings.Contains(log.String(), `cache's image exposes "49152-65535/udp": at most 64 ports in a range: left out`) ||
		!strings.Contains(log.String(), `"5000/sctp": tcp or udp: left out`) {
		t.Errorf("%+v %v %q", hn.peers, err, log)
	}
}

// The VM holds SSH's port, the TLS front's and the L4 routes' on every address; loopback mode's
// front holds none there.
func TestHeldPorts(t *testing.T) {
	t.Parallel()
	in, _, _ := hostNetInstaller(t, true)
	env := NewEnvFile()
	env.Set("STOWAGE_TLS_MODE", "internal")
	env.Set(HTTPPortVar, "80")
	env.Set(HTTPSPortVar, "8443")
	want := map[peerPort]bool{{22, "tcp"}: true, {80, "tcp"}: true, {8443, "tcp"}: true, {17007, "tcp"}: true, {17007, "udp"}: true}
	if got := in.heldPorts(env); !maps.Equal(got, want) {
		t.Errorf("TLS: %v", got)
	}
	env.Set("STOWAGE_TLS_MODE", Loopback)
	delete(want, peerPort{80, "tcp"})
	delete(want, peerPort{8443, "tcp"})
	if got := in.heldPorts(env); !maps.Equal(got, want) {
		t.Errorf("loopback: %v", got)
	}
}
