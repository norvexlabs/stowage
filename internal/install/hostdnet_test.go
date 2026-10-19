package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLink is networkd on one interface, ens3: `networkctl reload` applies the first .network
// file of its directory (none: the DHCP lease), and `ip -j` and `resolvectl` report what it
// applied.
type fakeLink struct {
	dir string
	mu  sync.Mutex
	ran [][]string
	// addr, gw and dns are what the link runs; addr6 and gw6 its static IPv6 address and gateway.
	addr, gw   string
	addr6, gw6 string
	dns        []string
	// ra: a router advertises, so the link has an address of its own (SLAAC) and a route via
	// fe80::1, unless the link's file turns IPv6 off (raOff). taken is an IPv6 address another
	// host has.
	ra, raOff bool
	taken     string
	// stuck: a reload applies nothing. reloadFails fails that many reloads.
	stuck       bool
	reloadFails int
}

const (
	leaseAddr = "10.0.0.50/24"
	leaseGW   = "10.0.0.1"
)

func newLink(t *testing.T) *fakeLink {
	return &fakeLink{dir: t.TempDir(), addr: leaseAddr, gw: leaseGW, dns: []string{leaseGW}}
}

func (f *fakeLink) Run(ctx context.Context, c Cmd) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err // a command run on a cancelled context does not run
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, c.Args)
	switch strings.Join(c.Args, " ") {
	case "networkctl reload":
		if f.reloadFails > 0 {
			f.reloadFails--
			return nil, errors.New("networkd: busy")
		}
		if !f.stuck {
			f.apply()
		}
		return nil, nil
	case "ip -j route show default":
		if f.gw == "" {
			return []byte("[]"), nil
		}
		return fmt.Appendf(nil, `[{"dst":"default","gateway":%q,"dev":"ens3"}]`, f.gw), nil
	case "ip -6 -j route show default":
		// The advertised route and a static one, of one metric, are one multipath route.
		rs := []string{`{"dst":"default","gateway":"fe80::9","dev":"wg0"}`}
		ra := f.ra && !f.raOff
		switch {
		case ra && f.gw6 != "":
			rs = append(rs, fmt.Sprintf(`{"dst":"default","nexthops":[{"gateway":"fe80::1","dev":"ens3"},{"gateway":%q,"dev":"ens3"}]}`, f.gw6))
		case ra:
			rs = append(rs, `{"dst":"default","gateway":"fe80::1","dev":"ens3"}`)
		case f.gw6 != "":
			rs = append(rs, fmt.Sprintf(`{"dst":"default","gateway":%q,"dev":"ens3"}`, f.gw6))
		}
		return []byte("[" + strings.Join(rs, ",") + "]"), nil
	case "ip -j address show":
		local, prefix, _ := strings.Cut(f.addr, "/")
		v6 := `{"family":"inet6","local":"fe80::5054:ff:fe12:3456","prefixlen":64,"scope":"link"}`
		if f.ra && !f.raOff {
			v6 += `,{"family":"inet6","local":"2001:db8::5054:ff:fe12:3456","prefixlen":64,"scope":"global"}`
		}
		if f.addr6 != "" {
			local6, prefix6, _ := strings.Cut(f.addr6, "/")
			v6 += fmt.Sprintf(`,{"family":"inet6","local":%q,"prefixlen":%s,"scope":"global","tentative":%t,"dadfailed":%[3]t}`, local6, prefix6, f.addr6 == f.taken)
		}
		return fmt.Appendf(nil, `[{"ifname":"lo","addr_info":[{"family":"inet","local":"127.0.0.1","prefixlen":8,"scope":"host"}]},
			{"ifname":"ens3","address":"52:54:00:12:34:56","addr_info":[{"family":"inet","local":%q,"prefixlen":%s,"scope":"global"},%s]}]`, local, prefix, v6), nil
	case "resolvectl dns":
		return []byte("Global:\nLink 2 (ens3): " + strings.Join(f.dns, " ") + "\n"), nil
	}
	return nil, nil
}

// apply runs the first .network file, as networkd does.
func (f *fakeLink) apply() {
	files, _ := filepath.Glob(filepath.Join(f.dir, "*.network"))
	sort.Strings(files)
	f.addr, f.gw, f.dns = leaseAddr, leaseGW, []string{leaseGW}
	f.addr6, f.gw6, f.raOff = "", "", false
	if len(files) == 0 {
		return
	}
	raw, _ := os.ReadFile(files[0])
	var dns []string
	useDNS := true
	for l := range strings.SplitSeq(string(raw), "\n") {
		k, v, _ := strings.Cut(l, "=")
		switch k {
		case "Address":
			if strings.Contains(v, ":") {
				f.addr6 = v
			} else {
				f.addr, f.gw = v, ""
			}
		case "Gateway":
			if strings.Contains(v, ":") {
				f.gw6 = v
			} else {
				f.gw = v
			}
		case "DNS":
			dns = append(dns, v)
		case "UseDNS":
			useDNS = false
		case "IPv6AcceptRA":
			f.raOff = v == "no"
		}
	}
	if !useDNS || f.addr != leaseAddr || len(dns) > 0 {
		f.dns = dns
	}
	if useDNS && strings.Contains(string(raw), "DHCP=yes") {
		f.dns = append(dns, leaseGW)
	}
}

func (f *fakeLink) calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ran)
}

func (f *fakeLink) live() (string, string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addr, f.gw, slices.Clone(f.dns)
}

// netDaemon is a host daemon whose network is link's.
func netDaemon(t *testing.T, link *fakeLink) *Hostd {
	h := &Hostd{R: testRelease(t, "1.0.0"), O: Options{Dir: t.TempDir(), Log: io.Discard}, Host: link, Unit: time.Millisecond,
		Networkd: link.dir, Issue: t.TempDir(), InstallNet: filepath.Join(t.TempDir(), "install-network.yaml"), ctx: context.Background()}
	return h
}

// eventually waits up to 5 s for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("never: %s", what)
		}
	}
}

func intp(n int) *int { return &n }

// fresh empties a before an answer is decoded into it.
func fresh(a *NetAnswer) *NetAnswer {
	*a = NetAnswer{}
	return a
}

func readState(t *testing.T, h *Hostd) netState {
	t.Helper()
	st, err := h.netFiles().read()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestValidNetworks(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   NetConfig
		want string // the error, or the address kept
	}{
		{NetConfig{Mode: "static", Address: "192.0.2.10/24", Gateway: "192.0.2.1"}, "192.0.2.10/24"},
		{NetConfig{Mode: "static", Address: "192.0.2.10/1"}, "192.0.2.10/1"},
		{NetConfig{Mode: "static", Address: "192.0.2.10/0"}, "the prefix is from 1 to 32"},
		{NetConfig{Mode: "static", Address: "192.0.2.10/33"}, "not an address with its prefix"},
		{NetConfig{Mode: "static", Address: "192.0.2.10"}, "not an address with its prefix"},
		{NetConfig{Mode: "static", Address: "192.0.2.0/24"}, "network address of 192.0.2.0/24"},
		{NetConfig{Mode: "static", Address: "192.0.2.255/24"}, "broadcast address of 192.0.2.0/24"},
		{NetConfig{Mode: "static", Address: "192.0.2.254/24"}, "192.0.2.254/24"},
		{NetConfig{Mode: "static", Address: "192.0.2.3/30"}, "broadcast address"},
		{NetConfig{Mode: "static", Address: "192.0.2.2/30"}, "192.0.2.2/30"},
		{NetConfig{Mode: "static", Address: "192.0.2.0/31", Gateway: "192.0.2.1"}, "192.0.2.0/31"},
		{NetConfig{Mode: "static", Address: "192.0.2.5/32", Gateway: "198.51.100.1"}, "192.0.2.5/32"},
		{NetConfig{Mode: "static", Address: "192.0.2.10/24", Gateway: "192.0.3.1"}, "outside 192.0.2.0/24"},
		{NetConfig{Mode: "static", Address: "192.0.2.10/24", Gateway: "192.0.2.10"}, "the VM's own address"},
		{NetConfig{Mode: "static", Address: "2001:db8::5/64"}, "not an IPv4 address"},
		{NetConfig{Mode: "static", Address: "127.0.0.2/8"}, "not an address a host uses"},
		{NetConfig{Mode: "static", Address: "169.254.1.1/16"}, "not an address a host uses"},
		{NetConfig{Mode: "static", Address: "224.0.0.5/4"}, "not an address a host uses"},
		{NetConfig{Mode: "static", Address: "240.0.0.5/8"}, "not an address a host uses"},
		{NetConfig{Mode: "static", Address: "192.0.2.10/24", Gateway: "fe80::1%eth0"}, "interface zone"},
		{NetConfig{Mode: "dhcp", DNS: []string{"fe80::1%eth0"}}, "interface zone"},
		{NetConfig{Mode: "dhcp", IPv6: true, DNS: []string{"2001:db8::53", "1.1.1.1"}}, ""},
		{NetConfig{Mode: "dhcp", DNS: []string{"1.1.1.1", "1.0.0.1", "8.8.8.8", "9.9.9.9"}}, ""},
		{NetConfig{Mode: "dhcp", DNS: []string{"1.1.1.1", "1.0.0.1", "8.8.8.8", "9.9.9.9", "8.8.4.4"}}, "at most 4"},
		{NetConfig{Mode: "dhcp", DNS: []string{"1.1.1.1", "1.1.1.1", "1.0.0.1", "8.8.8.8", "9.9.9.9"}}, ""},
		{NetConfig{Mode: "dhcp", Address: "192.0.2.10/24"}, "for the static mode"},
		{NetConfig{Mode: "dhcp", DNS: []string{"one.one"}}, "not an IP address"},
		{NetConfig{Mode: "manual"}, `mode "manual"`},
		{NetConfig{Mode: "static"}, "not an address with its prefix"},
	} {
		// A want that is an address (or empty) is accepted, with that address.
		got, err := validNet(c.in)
		accept := c.want == "" || strings.Count(c.want, " ") == 0 && strings.Contains(c.want, "/")
		switch {
		case err != nil && (accept || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%+v: %v, want %q", c.in, err, c.want)
		case err == nil && (!accept || c.want != got.Address):
			t.Errorf("%+v: accepted as %+v, want %q", c.in, got, c.want)
		}
	}
	got, _ := validNet(NetConfig{Mode: "dhcp", DNS: []string{" 1.1.1.1", "1.1.1.1"}})
	if !slices.Equal(got.DNS, []string{"1.1.1.1"}) {
		t.Errorf("DNS kept as %v", got.DNS)
	}
}

func TestValidIPv6Networks(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		mode, addr6, gw6 string
		want             string // the error, or the address kept
	}{
		{"dhcp", "2001:db8::10/64", "", "2001:db8::10/64"},
		{"static", "2001:DB8:0::10/64", "2001:db8::1", "2001:db8::10/64"},
		{"static", "fd00::10/64", "fe80::1", "fd00::10/64"},
		{"dhcp", "2001:db8::10/64", "2001:db9::1", "outside 2001:db8::/64"},
		{"dhcp", "2001:db8::11/126", "2001:db9::1", "outside 2001:db8::10/126"},
		{"dhcp", "2001:db8::10/127", "2001:db9::1", "2001:db8::10/127"},
		{"dhcp", "2001:db8::10/128", "2001:db9::1", "2001:db8::10/128"},
		{"dhcp", "2001:db8::/64", "", "anycast address of 2001:db8::/64"},
		{"dhcp", "2001:db8::/126", "", "anycast address of 2001:db8::/126"},
		{"dhcp", "2001:db8::/127", "", "2001:db8::/127"},
		{"dhcp", "2001:db8::10/0", "", "the prefix is from 1 to 128"},
		{"dhcp", "2001:db8::10/1", "", "2001:db8::10/1"},
		{"dhcp", "2001:db8::10", "", "not an IPv6 address with its prefix"},
		{"dhcp", "192.0.2.10/24", "", "not an IPv6 address with its prefix"},
		{"dhcp", "::ffff:192.0.2.10/120", "", "not an IPv6 address with its prefix"},
		{"dhcp", "fe80::10/64", "", "not an address a host uses"},
		{"dhcp", "ff02::10/64", "", "not an address a host uses"},
		{"dhcp", "::1/128", "", "not an address a host uses"},
		{"dhcp", "", "2001:db8::1", "gateway6 is for a static IPv6 address"},
		{"dhcp", "2001:db8::10/64", "192.0.2.1", `gateway6 "192.0.2.1" is not an IPv6 address`},
		{"dhcp", "2001:db8::10/64", "::ffff:192.0.2.1", "is not an IPv6 address"},
		{"dhcp", "2001:db8::10/64", "fe80::1%eth0", "interface zone"},
		{"dhcp", "2001:db8::10/64", "router", "not an IP address"},
		{"dhcp", "2001:db8::10/64", "ff02::2", "not an address a host uses"},
		{"dhcp", "2001:db8::10/64", "2001:db8::10", "the VM's own address"},
	} {
		in := NetConfig{Mode: c.mode, IPv6: true, Address6: c.addr6, Gateway6: c.gw6}
		if c.mode == "static" {
			in.Address = "192.0.2.10/24"
		}
		got, err := validNet(in)
		accept := strings.Count(c.want, " ") == 0 && strings.Contains(c.want, "/")
		switch {
		case err != nil && (accept || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%+v: %v, want %q", in, err, c.want)
		case err == nil && (!accept || c.want != got.Address6):
			t.Errorf("%+v: accepted as %+v, want %q", in, got, c.want)
		}
	}
	if got, _ := validNet(NetConfig{Mode: "dhcp", IPv6: true, Address6: "2001:db8::10/64", Gateway6: "FE80::1"}); got.Gateway6 != "fe80::1" || !got.IPv6 {
		t.Errorf("kept as %+v", got)
	}
	// IPv6 off: every IPv6 setting is refused, IPv4's are not.
	for _, c := range []NetConfig{
		{Mode: "dhcp", Address6: "2001:db8::10/64"},
		{Mode: "dhcp", Gateway6: "fe80::1"},
		{Mode: "static", Address: "192.0.2.10/24", DNS: []string{"1.1.1.1", "2001:db8::53"}},
	} {
		if _, err := validNet(c); err == nil || !strings.Contains(err.Error(), "IPv6, which is off: turn it on (ipv6)") {
			t.Errorf("%+v: %v", c, err)
		}
	}
	if _, err := validNet(NetConfig{Mode: "static", Address: "192.0.2.10/24", DNS: []string{"1.1.1.1"}}); err != nil {
		t.Error(err)
	}
}

func TestRevertWindowBounds(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   *int
		want int
		ok   bool
	}{{nil, 90, true}, {intp(0), 0, true}, {intp(29), 0, false}, {intp(30), 30, true}, {intp(3600), 3600, true}, {intp(3601), 0, false}, {intp(-1), 0, false}} {
		got, err := revertSecs(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("%v: %d %v", c.in, got, err)
		}
	}
}

func TestTheNetworkFileNetworkdReads(t *testing.T) {
	t.Parallel()
	static := renderNetwork("notes", "notes-stow", NetConfig{Mode: "static", Address: "192.0.2.10/24", Gateway: "192.0.2.1", DNS: []string{"1.1.1.1"}})
	for _, want := range []string{"[Match]\nName=en* eth*\n", "Address=192.0.2.10/24\n", "Gateway=192.0.2.1\n", "DNS=1.1.1.1\n"} {
		if !strings.Contains(static, want) {
			t.Errorf("no %q in\n%s", want, static)
		}
	}
	if strings.Contains(static, "DHCP=") {
		t.Errorf("a static file asks DHCP:\n%s", static)
	}
	// Its DNS servers replace DHCPv6's and the router advertisements' too.
	if !strings.HasSuffix(static, "\n[DHCPv6]\nUseDNS=no\n\n[IPv6AcceptRA]\nUseDNS=no\n") {
		t.Errorf("a static file keeps the router advertisements' DNS:\n%s", static)
	}
	if noDNS := renderNetwork("notes", "notes-stow", NetConfig{Mode: "static", Address: "192.0.2.10/24"}); strings.Contains(noDNS, "UseDNS") {
		t.Errorf("no DNS given, the advertised servers are dropped:\n%s", noDNS)
	}
	// IPv6 off: DHCPv4 alone, no link-local address, no router advertisement.
	dhcp := renderNetwork("notes", "notes-stow", NetConfig{Mode: "dhcp"})
	if !strings.HasSuffix(dhcp, "[Network]\nDHCP=ipv4\nLinkLocalAddressing=no\nIPv6AcceptRA=no\n") {
		t.Errorf("dhcp:\n%s", dhcp)
	}
	if on := renderNetwork("notes", "notes-stow", NetConfig{Mode: "dhcp", IPv6: true}); !strings.HasSuffix(on, "[Network]\nDHCP=yes\n\n[DHCPv6]\nWithoutRA=solicit\n") {
		t.Errorf("dhcp with IPv6:\n%s", on)
	}
	own := renderNetwork("notes", "notes-stow", NetConfig{Mode: "dhcp", DNS: []string{"9.9.9.9"}})
	for _, want := range []string{"DNS=9.9.9.9\n", "[DHCPv4]\nUseDNS=no\n", "[DHCPv6]\nUseDNS=no\n", "[IPv6AcceptRA]\nUseDNS=no\n"} {
		if !strings.Contains(own, want) {
			t.Errorf("dhcp with its own DNS servers keeps others: no %q in\n%s", want, own)
		}
	}
	// A gateway outside the prefix is reached on the link; one inside is a plain Gateway=.
	onLink := renderNetwork("notes", "notes-stow", NetConfig{Mode: "static", Address: "192.0.2.5/32", Gateway: "198.51.100.1"})
	if !strings.Contains(onLink, "\n[Route]\nGateway=198.51.100.1\nGatewayOnLink=yes\n") || strings.Contains(onLink, "[Network]\nAddress=192.0.2.5/32\nGateway=") {
		t.Errorf("/32:\n%s", onLink)
	}
	if p2p := renderNetwork("notes", "notes-stow", NetConfig{Mode: "static", Address: "192.0.2.0/31", Gateway: "192.0.2.1"}); strings.Contains(p2p, "OnLink") || !strings.Contains(p2p, "Gateway=192.0.2.1\n") {
		t.Errorf("/31 with its gateway in the prefix:\n%s", p2p)
	}
	if strings.Contains(static, "OnLink") {
		t.Errorf("an on-link route for a gateway in the subnet:\n%s", static)
	}
	// An IPv6 address beside either mode: its gateway as IPv4's is, but a link-local one is on the
	// link already.
	for _, c := range []struct {
		in   NetConfig
		want string
	}{
		{NetConfig{IPv6: true, Mode: "static", Address: "192.0.2.10/24", Gateway: "192.0.2.1", Address6: "2001:db8::10/64", Gateway6: "2001:db8::1"},
			"[Network]\nAddress=192.0.2.10/24\nGateway=192.0.2.1\nAddress=2001:db8::10/64\nGateway=2001:db8::1\n"},
		{NetConfig{IPv6: true, Mode: "dhcp", Address6: "2001:db8::10/64", Gateway6: "fe80::1"}, "[Network]\nDHCP=yes\nAddress=2001:db8::10/64\nGateway=fe80::1\n\n[DHCPv6]\nWithoutRA=solicit\n"},
		{NetConfig{IPv6: true, Mode: "dhcp", Address6: "2001:db8::10/128", Gateway6: "2001:db9::1"}, "[Network]\nDHCP=yes\nAddress=2001:db8::10/128\n\n[DHCPv6]\nWithoutRA=solicit\n\n[Route]\nGateway=2001:db9::1\nGatewayOnLink=yes\n"},
		{NetConfig{IPv6: true, Mode: "static", Address: "192.0.2.5/32", Gateway: "198.51.100.1", Address6: "2001:db8::10/128", Gateway6: "2001:db9::1"},
			"[Network]\nAddress=192.0.2.5/32\nAddress=2001:db8::10/128\n\n[Route]\nGateway=198.51.100.1\nGatewayOnLink=yes\n\n[Route]\nGateway=2001:db9::1\nGatewayOnLink=yes\n"},
	} {
		if got := renderNetwork("notes", "notes-stow", c.in); !strings.HasSuffix(got, "\n\n"+c.want) {
			t.Errorf("%+v:\n%s", c.in, got)
		}
	}
}

func TestAStaticAddressConfirmedStays(t *testing.T) {
	t.Parallel()
	link := newLink(t)
	h := netDaemon(t, link)
	api := h.Handler()
	var a NetAnswer
	set := NetRequest{NetConfig: NetConfig{Mode: "static", Address: "10.0.0.60/24", Gateway: "10.0.0.1", DNS: []string{"10.0.0.1"}}, RevertSecs: intp(maxRevertSecs)}
	if code := call(t, api, http.MethodPost, "/v1/network", set, fresh(&a)); code != http.StatusOK {
		t.Fatalf("%d %+v", code, a)
	}
	if a.Pending == nil || a.Pending.Previous != nil || !strings.Contains(a.Message, "network confirm") {
		t.Fatalf("the change does not await confirmation: %+v", a)
	}
	if addr, gw, _ := link.live(); addr != "10.0.0.60/24" || gw != "10.0.0.1" {
		t.Fatalf("the link runs %s via %s", addr, gw)
	}
	if !slices.ContainsFunc(link.calls(), func(c []string) bool { return slices.Equal(c, []string{"networkctl", "reconfigure", "ens3"}) }) {
		t.Errorf("the interface was not reconfigured: %v", link.calls())
	}
	var n Network
	call(t, api, http.MethodGet, "/v1/network", nil, &n)
	if n.Source != "override" || n.Pending == nil || n.Live.Interface != "ens3" || !slices.Equal(n.Live.Addresses, []string{"10.0.0.60/24"}) || n.Live.MAC == "" {
		t.Fatalf("%+v", n)
	}
	banner, _ := os.ReadFile(filepath.Join(h.Issue, "06-notes-network.issue"))
	if !strings.Contains(string(banner), "network: 10.0.0.60/24 via 10.0.0.1, no IPv6, DNS 10.0.0.1; set with `notes-stow network set`") || !strings.Contains(string(banner), "not confirmed") {
		t.Errorf("banner:\n%s", banner)
	}
	if code := call(t, api, http.MethodPost, "/v1/network", set, &errOf{}); code != http.StatusConflict {
		t.Errorf("a second change while one awaits confirmation: %d", code)
	}
	if code := call(t, api, http.MethodPost, "/v1/network/confirm", nil, fresh(&a)); code != http.StatusOK || a.Pending != nil || a.Message != "confirmed" {
		t.Fatalf("%d %+v", code, a)
	}
	st := readState(t, h)
	if st.Pending != nil || st.Config == nil || st.Config.Address != "10.0.0.60/24" {
		t.Fatalf("recorded %+v", st)
	}
	banner, _ = os.ReadFile(filepath.Join(h.Issue, "06-notes-network.issue"))
	if strings.Contains(string(banner), "not confirmed") {
		t.Errorf("banner after confirm:\n%s", banner)
	}
	if code := call(t, api, http.MethodPost, "/v1/network/revert", nil, &errOf{}); code != http.StatusConflict {
		t.Errorf("a revert with nothing awaiting confirmation: %d", code)
	}
	// A DNS change alone cannot move the VM: no window.
	set.DNS, set.RevertSecs = []string{"9.9.9.9"}, nil
	if code := call(t, api, http.MethodPost, "/v1/network", set, fresh(&a)); code != http.StatusOK || a.Pending != nil || a.Message != "applied" {
		t.Fatalf("%d %+v", code, a)
	}
	if _, _, dns := link.live(); !slices.Equal(dns, []string{"9.9.9.9"}) {
		t.Errorf("DNS %v", dns)
	}
}

func TestAChangeNotConfirmedComesUndone(t *testing.T) {
	t.Parallel()
	link := newLink(t)
	h := netDaemon(t, link)
	api := h.Handler()
	var a NetAnswer
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{NetConfig: NetConfig{Mode: "static", Address: "10.0.0.60/24", Gateway: "10.0.0.1"}, RevertSecs: intp(minRevertSecs)}, fresh(&a)); code != http.StatusOK || a.Pending == nil {
		t.Fatalf("%d %+v", code, a)
	}
	eventually(t, "the lease back", func() bool { addr, _, _ := link.live(); return addr == leaseAddr })
	eventually(t, "the override gone", func() bool { return !fileExists(NetworkFile(h.O.Dir)) })
	if fileExists(filepath.Join(link.dir, "01-notes-override.network")) {
		t.Error("the override's networkd file is left")
	}
	// Read once the revert, which holds netMu, is over.
	h.netMu.Lock()
	banner, _ := os.ReadFile(filepath.Join(h.Issue, "06-notes-network.issue"))
	h.netMu.Unlock()
	if string(banner) != "network: the VM's cloud-config (DHCP, or its static address)\n" {
		t.Errorf("banner:\n%s", banner)
	}
}

func TestARevertNowAndOneThatFailsIsTriedAgain(t *testing.T) {
	t.Parallel()
	link := newLink(t)
	h := netDaemon(t, link)
	api := h.Handler()
	confirmed := NetRequest{NetConfig: NetConfig{Mode: "static", Address: "10.0.0.60/24", Gateway: "10.0.0.1"}, RevertSecs: intp(0)}
	var a NetAnswer
	if code := call(t, api, http.MethodPost, "/v1/network", confirmed, fresh(&a)); code != http.StatusOK || a.Pending != nil || a.Message != "applied, without a revert window" {
		t.Fatalf("%d %+v", code, a)
	}
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{NetConfig: NetConfig{Mode: "dhcp"}, RevertSecs: intp(maxRevertSecs)}, fresh(&a)); code != http.StatusOK || a.Pending == nil || a.Pending.Previous == nil {
		t.Fatalf("%d %+v", code, a)
	}
	if addr, _, _ := link.live(); addr != leaseAddr {
		t.Fatalf("dhcp: the link runs %s", addr)
	}
	// The revert's first reload fails: it is tried again, and the static address comes back.
	link.mu.Lock()
	link.reloadFails = 1
	link.mu.Unlock()
	if code := call(t, api, http.MethodPost, "/v1/network/revert", nil, &errOf{}); code != http.StatusInternalServerError {
		t.Fatalf("a failed revert answered %d", code)
	}
	eventually(t, "the static address back", func() bool { addr, _, _ := link.live(); return addr == "10.0.0.60/24" })
	st := readState(t, h)
	if st.Pending != nil || st.Config == nil || st.Config.Mode != "static" {
		t.Fatalf("recorded %+v", st)
	}
	// Retries stop after revertAttempts.
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{NetConfig: NetConfig{Mode: "dhcp"}, RevertSecs: intp(maxRevertSecs)}, fresh(&a)); code != http.StatusOK {
		t.Fatal(code)
	}
	link.mu.Lock()
	link.reloadFails = revertAttempts
	link.mu.Unlock()
	call(t, api, http.MethodPost, "/v1/network/revert", nil, &errOf{})
	eventually(t, "the retries spent", func() bool { link.mu.Lock(); defer link.mu.Unlock(); return link.reloadFails == 0 })
	time.Sleep(3 * revertRetrySecs * time.Millisecond)
	if addr, _, _ := link.live(); addr != leaseAddr {
		t.Errorf("a revert ran past %d attempts", revertAttempts)
	}
}

func TestARetryNeverUndoesALaterChange(t *testing.T) {
	t.Parallel()
	link := newLink(t)
	h := netDaemon(t, link)
	api := h.Handler()
	// The change fails, and so does restoring the settings before it: restoring is tried again.
	link.reloadFails = 2
	var e errOf
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{NetConfig: NetConfig{Mode: "static", Address: "10.0.0.60/24"}, RevertSecs: intp(0)}, &e); code != http.StatusInternalServerError || !strings.Contains(e.Error, "tried again") {
		t.Fatalf("%d %s", code, e.Error)
	}
	// A change made before the retry is the one kept.
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{NetConfig: NetConfig{Mode: "static", Address: "10.0.0.70/24"}, RevertSecs: intp(0)}, &e); code != http.StatusOK {
		t.Fatalf("%d %s", code, e.Error)
	}
	time.Sleep(3 * revertRetrySecs * time.Millisecond)
	h.netMu.Lock()
	defer h.netMu.Unlock()
	if addr, _, _ := link.live(); addr != "10.0.0.70/24" {
		t.Errorf("the link runs %s", addr)
	}
	if st := readState(t, h); st.Config == nil || st.Config.Address != "10.0.0.70/24" {
		t.Errorf("recorded %+v", st.Config)
	}
}

func TestAChangeThatDoesNotReachTheLinkIsUndoneAtOnce(t *testing.T) {
	t.Parallel()
	link := newLink(t)
	h := netDaemon(t, link)
	api := h.Handler()
	link.stuck = true
	var e errOf
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{NetConfig: NetConfig{Mode: "static", Address: "10.0.0.60/24", Gateway: "10.0.0.1"}}, &e); code != http.StatusInternalServerError {
		t.Fatalf("%d %s", code, e.Error)
	}
	if !strings.Contains(e.Error, "the address 10.0.0.60/24 did not reach the interface") || !strings.Contains(e.Error, "the settings before it are back") {
		t.Errorf("%s", e.Error)
	}
	if fileExists(NetworkFile(h.O.Dir)) || fileExists(filepath.Join(link.dir, "01-notes-override.network")) {
		t.Error("the change is left recorded")
	}
	// A gateway that never comes, on an address that is there already.
	link.stuck = false
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{NetConfig: NetConfig{Mode: "static", Address: leaseAddr, Gateway: "10.0.0.1"}, RevertSecs: intp(0)}, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	link.stuck = true
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{NetConfig: NetConfig{Mode: "static", Address: leaseAddr, Gateway: "10.0.0.254"}, RevertSecs: intp(0)}, &e); code != http.StatusInternalServerError || !strings.Contains(e.Error, "the default route via 10.0.0.254") {
		t.Fatalf("%d %s", code, e.Error)
	}
	if st := readState(t, h); st.Config == nil || st.Config.Gateway != "10.0.0.1" {
		t.Errorf("recorded %+v", st.Config)
	}
}

func TestAStaticIPv6AddressBesideDHCP(t *testing.T) {
	t.Parallel()
	link := newLink(t)
	link.ra = true
	h := netDaemon(t, link)
	api := h.Handler()
	var a NetAnswer
	// IPv6 off: the advertised address and route are not the link's.
	off := NetRequest{NetConfig: NetConfig{Mode: "dhcp"}, RevertSecs: intp(0)}
	var n Network
	if code := call(t, api, http.MethodPost, "/v1/network", off, fresh(&a)); code != http.StatusOK {
		t.Fatalf("%d %+v", code, a)
	}
	call(t, api, http.MethodGet, "/v1/network", nil, &n)
	if !slices.Equal(n.Live.Addresses, []string{leaseAddr}) || len(n.Live.Gateways6) > 0 {
		t.Fatalf("IPv6 off: %+v", n.Live)
	}
	// Turning IPv6 on, nothing else, moves the VM: it awaits confirmation.
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{NetConfig: NetConfig{Mode: "dhcp", IPv6: true}}, fresh(&a)); code != http.StatusOK || a.Pending == nil {
		t.Fatalf("IPv6 on: %d %+v", code, a)
	}
	call(t, api, http.MethodPost, "/v1/network/confirm", nil, fresh(&a))
	set := NetRequest{NetConfig: NetConfig{Mode: "dhcp", IPv6: true, Address6: "2001:db8::10/64", Gateway6: "2001:db8::1"}, RevertSecs: intp(maxRevertSecs)}
	if code := call(t, api, http.MethodPost, "/v1/network", set, fresh(&a)); code != http.StatusOK || a.Pending == nil {
		t.Fatalf("an IPv6 address is an addressing change: %d %+v", code, a)
	}
	call(t, api, http.MethodGet, "/v1/network", nil, &n)
	// The lease's, the advertised and the static address; the advertised and the static route,
	// not another link's.
	if !slices.Equal(n.Live.Addresses, []string{leaseAddr, "2001:db8::5054:ff:fe12:3456/64", "2001:db8::10/64"}) || n.Live.Gateway != leaseGW ||
		!slices.Equal(n.Live.Gateways6, []string{"fe80::1", "2001:db8::1"}) {
		t.Fatalf("%+v", n.Live)
	}
	banner, _ := os.ReadFile(filepath.Join(h.Issue, "06-notes-network.issue"))
	if !strings.Contains(string(banner), "network: DHCP, 2001:db8::10/64 via 2001:db8::1; set with `notes-stow network set`") {
		t.Errorf("banner:\n%s", banner)
	}
	if code := call(t, api, http.MethodPost, "/v1/network/confirm", nil, fresh(&a)); code != http.StatusOK {
		t.Fatalf("%d %+v", code, a)
	}
	// Another IPv6 gateway alone moves the VM too.
	set.Gateway6 = "fe80::1"
	if code := call(t, api, http.MethodPost, "/v1/network", set, fresh(&a)); code != http.StatusOK || a.Pending == nil {
		t.Fatalf("%d %+v", code, a)
	}
	call(t, api, http.MethodPost, "/v1/network/confirm", nil, fresh(&a))
	// An address another host has is undone at once, as is a gateway that never comes.
	link.taken = "2001:db8::11/64"
	var e errOf
	set.Address6, set.RevertSecs = "2001:db8::11/64", intp(0)
	if code := call(t, api, http.MethodPost, "/v1/network", set, &e); code != http.StatusInternalServerError ||
		!strings.Contains(e.Error, "the address 2001:db8::11/64 did not reach the interface") || !strings.Contains(e.Error, "the settings before it are back") {
		t.Fatalf("%d %s", code, e.Error)
	}
	if st := readState(t, h); st.Config == nil || st.Config.Address6 != "2001:db8::10/64" || st.Config.Gateway6 != "fe80::1" {
		t.Errorf("recorded %+v", st.Config)
	}
	link.stuck = true
	set.Address6, set.Gateway6 = "2001:db8::10/64", "2001:db8::2"
	if code := call(t, api, http.MethodPost, "/v1/network", set, &e); code != http.StatusInternalServerError || !strings.Contains(e.Error, "the IPv6 default route via 2001:db8::2") {
		t.Fatalf("%d %s", code, e.Error)
	}
}

func TestResetGoesBackToTheCloudConfigOnTheSameTimer(t *testing.T) {
	t.Parallel()
	link := newLink(t)
	h := netDaemon(t, link)
	api := h.Handler()
	var a NetAnswer
	if code := call(t, api, http.MethodPost, "/v1/network/reset", nil, fresh(&a)); code != http.StatusOK || a.Message != "no override: the cloud-config's settings apply" {
		t.Fatalf("%d %+v", code, a)
	}
	static := NetConfig{Mode: "static", Address: "10.0.0.60/24", Gateway: "10.0.0.1"}
	call(t, api, http.MethodPost, "/v1/network", NetRequest{NetConfig: static, RevertSecs: intp(0)}, nil)
	if code := call(t, api, http.MethodPost, "/v1/network/reset", NetResetRequest{RevertSecs: intp(minRevertSecs)}, fresh(&a)); code != http.StatusOK || a.Pending == nil || a.Override != nil {
		t.Fatalf("%d %+v", code, a)
	}
	if addr, _, _ := link.live(); addr != leaseAddr {
		t.Fatalf("the reset left %s", addr)
	}
	eventually(t, "the override back", func() bool { addr, _, _ := link.live(); return addr == "10.0.0.60/24" })
	// Confirmed, the reset leaves no state.
	if code := call(t, api, http.MethodPost, "/v1/network/reset", NetResetRequest{RevertSecs: intp(maxRevertSecs)}, fresh(&a)); code != http.StatusOK {
		t.Fatal(code)
	}
	if code := call(t, api, http.MethodPost, "/v1/network/confirm", nil, fresh(&a)); code != http.StatusOK {
		t.Fatal(code)
	}
	if fileExists(NetworkFile(h.O.Dir)) {
		t.Error("a confirmed reset keeps a state file")
	}
}

func TestTheDaemonStartingUndoesAChangeNotConfirmed(t *testing.T) {
	t.Parallel()
	link := newLink(t)
	h := netDaemon(t, link)
	f := h.netFiles()
	prev := &NetConfig{Mode: "static", Address: "10.0.0.60/24", Gateway: "10.0.0.1"}
	if err := f.write(netState{Config: &NetConfig{Mode: "dhcp"}, Pending: &netPending{ID: "w", Deadline: time.Now().Add(time.Hour), Previous: prev}}); err != nil {
		t.Fatal(err)
	}
	h.NetResume()
	if addr, _, _ := link.live(); addr != "10.0.0.60/24" {
		t.Fatalf("the link runs %s", addr)
	}
	if st := readState(t, h); st.Pending != nil || !reflect.DeepEqual(st.Config, prev) {
		t.Errorf("recorded %+v", st)
	}
}

func TestTheBootUndoesAChangeNotConfirmed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := netFiles{name: "notes", ctl: "notes-stow", state: NetworkFile(dir), networkd: t.TempDir(), issue: t.TempDir()}
	prev := &NetConfig{Mode: "static", Address: "10.0.0.60/24", Gateway: "10.0.0.1"}
	if err := f.write(netState{Config: &NetConfig{Mode: "dhcp"}, Pending: &netPending{ID: "w", Deadline: time.Now().Add(time.Hour), Previous: prev}}); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	if err := f.boot(&log); err != nil {
		t.Fatal(err)
	}
	unit, _ := os.ReadFile(f.overrideUnit())
	if !strings.Contains(string(unit), "Address=10.0.0.60/24") || !strings.Contains(log.String(), "not confirmed before the reboot") {
		t.Errorf("unit:\n%s\nlog: %s", unit, log.String())
	}
	st, err := f.read()
	if err != nil || st.Pending != nil || !reflect.DeepEqual(st.Config, prev) {
		t.Errorf("%+v %v", st, err)
	}
	// A confirmed override is rendered as it is; none removes the file.
	if err := f.boot(io.Discard); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(f.overrideUnit()); !bytes.Equal(again, unit) {
		t.Errorf("rendered again as\n%s", again)
	}
	if err := f.write(netState{}); err != nil {
		t.Fatal(err)
	}
	if err := f.boot(io.Discard); err != nil || fileExists(f.overrideUnit()) {
		t.Errorf("%v: the override's file is left", err)
	}
	// The cloud-config's static file is written after the boot renders: the banner does not tell.
	if b, _ := os.ReadFile(f.banner()); string(b) != "network: the VM's cloud-config (DHCP, or its static address)\n" {
		t.Errorf("banner: %q", b)
	}
}

func TestAStateFileThatDoesNotReadSetsNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := netFiles{name: "notes", ctl: "notes-stow", state: NetworkFile(dir), networkd: t.TempDir(), issue: t.TempDir()}
	if err := os.WriteFile(f.overrideUnit(), []byte("[Network]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"config: {mode: static, address: 10.0.0.60/24, gatway: 10.0.0.1}\n", "config: {mode: static, address: 10.0.0.0/24}\n"} {
		if err := os.WriteFile(f.state, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := f.boot(io.Discard); err == nil || !strings.Contains(err.Error(), "the cloud-config's settings apply") {
			t.Errorf("%q: %v", bad, err)
		}
		if fileExists(f.overrideUnit()) {
			t.Errorf("%q: the override's file is left", bad)
		}
	}
	// A record without a version reads with IPv6 on, its previous settings too, an IPv6 DNS
	// server kept.
	for _, legacy := range []string{
		"config: {mode: static, address: 10.0.0.60/24, gateway: 10.0.0.1, dns: [\"2001:db8::53\"]}\npending: {previous: {mode: dhcp}, deadline: 2030-01-01T00:00:00Z, id: x}\n",
		"config: {mode: static, address: 10.0.0.60/24, dns: [\"10.0.0.53\"]}\n",
	} {
		if err := os.WriteFile(f.state, []byte(legacy), 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := f.read()
		if err != nil || st.Config == nil || !st.Config.IPv6 || st.Config.Address != "10.0.0.60/24" || st.Pending != nil && !st.Pending.Previous.IPv6 {
			t.Errorf("%q: %+v %v", legacy, st.Config, err)
		}
	}
	// IPv6 on without a static IPv6 address: written as an older release writes it, which a
	// rollback reads; its previous settings too.
	on := netState{Config: &NetConfig{Mode: "static", Address: "10.0.0.60/24", IPv6: true}, Pending: &netPending{ID: "x", Deadline: time.Now().Add(time.Hour), Previous: &NetConfig{Mode: "dhcp", IPv6: true}}}
	if err := f.write(on); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(f.state); strings.Contains(string(raw), "version") || strings.Contains(string(raw), "ipv6") {
		t.Errorf("an older release's record holds the new keys:\n%s", raw)
	}
	if st, err := f.read(); err != nil || !st.Config.IPv6 || !st.Pending.Previous.IPv6 || !on.Config.IPv6 {
		t.Errorf("read back %+v %v", st, err)
	}
	// A record of this format keeps its choice; one of a later format does not read.
	if err := f.write(netState{Config: &NetConfig{Mode: "dhcp"}}); err != nil {
		t.Fatal(err)
	}
	if st, err := f.read(); err != nil || st.Config.IPv6 || st.Version != netStateVersion {
		t.Errorf("IPv6 off kept: %+v %v", st, err)
	}
	if err := os.WriteFile(f.state, []byte("version: 3\nconfig: {mode: dhcp}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.read(); err == nil || !strings.Contains(err.Error(), "from a later release") {
		t.Errorf("a later format: %v", err)
	}
	// No other version; without one, none of the keys only a version carries.
	for bad, want := range map[string]string{
		"version: 1\nconfig: {mode: dhcp}\n":                                           "version 1: 2, or none",
		"version: -1\nconfig: {mode: dhcp}\n":                                          "version -1: 2, or none",
		"config: {mode: dhcp, ipv6: false}\n":                                          "ipv6 is set",
		"config: {mode: dhcp}\npending: {id: x, previous: {mode: dhcp, ipv6: true}}\n": "ipv6 is set",
		"config: {mode: dhcp, address6: 2001:db8::10/64}\n":                            "address6 and gateway6 need version: 2",
	} {
		if err := os.WriteFile(f.state, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := f.read(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", bad, err, want)
		}
	}
	// One config with IPv6 off, the pending change's previous one: the version is written, and
	// that config read back off.
	mixed := netState{Config: &NetConfig{Mode: "dhcp", IPv6: true}, Pending: &netPending{ID: "x", Deadline: time.Now().Add(time.Hour), Previous: &NetConfig{Mode: "dhcp"}}}
	if err := f.write(mixed); err != nil {
		t.Fatal(err)
	}
	if st, err := f.read(); err != nil || st.Version != netStateVersion || st.Pending.Previous.IPv6 || !st.Config.IPv6 {
		t.Errorf("mixed: %+v %v", st, err)
	}
}

func TestAClientGoneDoesNotUndoAChangeThatWorked(t *testing.T) {
	t.Parallel()
	link := newLink(t)
	h := netDaemon(t, link)
	raw, _ := json.Marshal(NetRequest{NetConfig: NetConfig{Mode: "static", Address: "10.0.0.60/24", Gateway: "10.0.0.1"}, RevertSecs: intp(0)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the SSH session the change cut
	h.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/network", bytes.NewReader(raw)).WithContext(ctx))
	if addr, _, _ := link.live(); addr != "10.0.0.60/24" {
		t.Errorf("the link runs %s", addr)
	}
	if st := readState(t, h); st.Config == nil || st.Config.Address != "10.0.0.60/24" {
		t.Errorf("recorded %+v", st.Config)
	}
}

func TestResetDropsAnOverrideThatDoesNotRead(t *testing.T) {
	t.Parallel()
	link := newLink(t)
	h := netDaemon(t, link)
	api := h.Handler()
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{NetConfig: NetConfig{Mode: "static", Address: "10.0.0.60/24"}, RevertSecs: intp(0)}, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	if err := os.WriteFile(NetworkFile(h.O.Dir), []byte("config: {mode: static, adress: 10.0.0.60/24}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var e errOf
	if code := call(t, api, http.MethodGet, "/v1/network", nil, &e); code != http.StatusInternalServerError || !strings.Contains(e.Error, "`notes-stow network reset` drops it") {
		t.Fatalf("%d %s", code, e.Error)
	}
	var a NetAnswer
	if code := call(t, api, http.MethodPost, "/v1/network/reset", nil, &a); code != http.StatusOK || !strings.Contains(a.Message, "dropped") || a.Pending != nil {
		t.Fatalf("%d %+v", code, a)
	}
	if strings.Contains(a.Message, "network reset` drops it") {
		t.Errorf("the reset tells to reset: %s", a.Message)
	}
	if fileExists(NetworkFile(h.O.Dir)) || fileExists(filepath.Join(link.dir, "01-notes-override.network")) {
		t.Error("the override is left")
	}
	if addr, _, _ := link.live(); addr != leaseAddr {
		t.Errorf("the link runs %s", addr)
	}
}

// FuzzValidNetwork: whatever validNet accepts is its own canonical form, renders no line of its
// own (no newline), and keeps a static address with its prefix.
func FuzzValidNetwork(f *testing.F) {
	f.Add("static", "192.0.2.10/24", "192.0.2.1", "", "", "1.1.1.1")
	f.Add("dhcp", "", "", "2001:db8::10/64", "fe80::1", "2001:db8::53,9.9.9.9")
	f.Add("static", "::ffff:192.0.2.10/120", "192.0.2.1", "::ffff:192.0.2.11/120", "", "")
	f.Add("static", "192.0.2.5/32", "198.51.100.1", "2001:DB8::10/128", "2001:db9::1", " 1.1.1.1 ")
	f.Fuzz(func(t *testing.T, mode, addr, gw, addr6, gw6, dns string) {
		in := NetConfig{Mode: mode, Address: addr, Gateway: gw, IPv6: addr6 != "" || strings.Contains(dns, ":"), Address6: addr6, Gateway6: gw6}
		if dns != "" {
			in.DNS = strings.Split(dns, ",")
		}
		out, err := validNet(in)
		if err != nil {
			return
		}
		again, err := validNet(out)
		if err != nil || !reflect.DeepEqual(again, out) {
			t.Fatalf("%+v → %+v → %+v %v", in, out, again, err)
		}
		if strings.ContainsAny(out.Address+out.Gateway+out.Address6+out.Gateway6+strings.Join(out.DNS, ""), "\r\n%") {
			t.Fatalf("%+v", out)
		}
		if out.Mode == "static" {
			if p, err := netip.ParsePrefix(out.Address); err != nil || !p.Addr().Is4() || p.Bits() < 1 {
				t.Fatalf("%+v", out)
			}
		}
		if out.Address6 != "" {
			if p, err := netip.ParsePrefix(out.Address6); err != nil || !p.Addr().Is6() || p.Addr().Is4In6() || p.Bits() < 1 {
				t.Fatalf("%+v", out)
			}
		}
		if out.Gateway6 != "" && out.Address6 == "" {
			t.Fatalf("%+v", out)
		}
		if len(out.DNS) > maxDNS {
			t.Fatalf("%+v", out)
		}
	})
}

func TestTheDaemonUndoesAChangeBeforeItServesOne(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	link := newLink(t)
	h := daemon(e, &fakeHost{})
	h.Host, h.Networkd = link, link.dir
	prev := &NetConfig{Mode: "static", Address: "10.0.0.60/24", Gateway: "10.0.0.1"}
	if err := h.netFiles().write(netState{Config: &NetConfig{Mode: "dhcp"}, Pending: &netPending{ID: "w", Deadline: time.Now().Add(time.Hour), Previous: prev}}); err != nil {
		t.Fatal(err)
	}
	sock := served(t, h)
	// The first request, a confirmation, comes after the restart undid the change: it fails.
	if _, err := ctl(e, sock, "network", "confirm"); err == nil || !strings.Contains(err.Error(), "nothing awaits confirmation: a change not confirmed in its window, or before a restart or a reboot, is undone") {
		t.Fatalf("%v", err)
	}
	if addr, _, _ := link.live(); addr != "10.0.0.60/24" {
		t.Errorf("the link runs %s", addr)
	}
}

func TestAConfirmationAfterTheWindowUndoesTheChange(t *testing.T) {
	t.Parallel()
	link := newLink(t)
	h := netDaemon(t, link)
	api := h.Handler()
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{NetConfig: NetConfig{Mode: "static", Address: "10.0.0.60/24", Gateway: "10.0.0.1"}, RevertSecs: intp(minRevertSecs)}, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	// The window ends while a request holds the lock: its timer fires and waits; a confirmation
	// that has the lock first comes too late.
	h.netMu.Lock()
	eventually(t, "the timer fired", func() bool { return h.netFired.Load() == int64(h.netGen) })
	_, err := h.confirm()
	h.netMu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "the change's window ended: the settings before it are back") {
		t.Fatalf("%v", err)
	}
	if addr, _, _ := link.live(); addr != leaseAddr {
		t.Errorf("the link runs %s", addr)
	}
	if fileExists(NetworkFile(h.O.Dir)) {
		t.Error("the change is left recorded")
	}
	// The timer that waited does nothing more.
	time.Sleep(10 * time.Millisecond)
	if addr, _, _ := link.live(); addr != leaseAddr {
		t.Errorf("the link runs %s", addr)
	}
}

func TestTheDaemonStartingAppliesTheRecord(t *testing.T) {
	t.Parallel()
	link := newLink(t)
	h := netDaemon(t, link)
	// Recorded, but the daemon stopped before networkd had it.
	if err := h.netFiles().write(netState{Config: &NetConfig{Mode: "static", Address: "10.0.0.60/24", Gateway: "10.0.0.1"}}); err != nil {
		t.Fatal(err)
	}
	h.NetResume()
	if addr, _, _ := link.live(); addr != "10.0.0.60/24" {
		t.Fatalf("the link runs %s", addr)
	}
	// Rendered already, it is left alone: no reload.
	n := len(link.calls())
	h.NetResume()
	if len(link.calls()) != n {
		t.Errorf("ran %v", link.calls()[n:])
	}
}

func TestNoRetryOnceTheDaemonStops(t *testing.T) {
	t.Parallel()
	link := newLink(t)
	h := netDaemon(t, link)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.ctx = ctx
	h.netMu.Lock()
	defer h.netMu.Unlock()
	if err := h.undo(1, "a test", h.restore(nil, "ens3")); err == nil {
		t.Fatal("undone on a stopped daemon")
	}
	if h.netTimer != nil {
		t.Error("a retry armed on a stopped daemon")
	}
}

func TestNoNetworkChangeOnceARebootIsScheduled(t *testing.T) {
	t.Parallel()
	h := netDaemon(t, newLink(t))
	h.rebooting = true
	api := h.Handler()
	var e errOf
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{NetConfig: NetConfig{Mode: "dhcp"}}, &e); code != http.StatusConflict || !strings.Contains(e.Error, "reboot") {
		t.Errorf("%d %s", code, e.Error)
	}
	// Even to drop a state file that does not read.
	if err := os.WriteFile(NetworkFile(h.O.Dir), []byte("garbage: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := call(t, api, http.MethodPost, "/v1/network/reset", nil, &e); code != http.StatusConflict || !fileExists(NetworkFile(h.O.Dir)) {
		t.Errorf("reset during a scheduled reboot: %d %s", code, e.Error)
	}
	if code := call(t, api, http.MethodPost, "/v1/network", map[string]any{"mode": "dhcp", "revert": 30}, &e); code != http.StatusBadRequest {
		t.Errorf("an unknown field: %d", code)
	}
}

// The answer says what the install set, DHCP included, and what sets the network now: the
// install's record when the cloud-config wrote one, its static unit alone when it predates it.
func TestTheNetworkAnswersWhatTheInstallSet(t *testing.T) {
	t.Parallel()
	dhcp := &NetConfig{Mode: "dhcp", IPv6: true}
	static := &NetConfig{Mode: "static", Address: "10.0.0.5/24", Gateway: "10.0.0.1", IPv6: true, Address6: "2001:db8::5/64", Gateway6: "fe80::1", DNS: []string{"10.0.0.1"}}
	for name, c := range map[string]struct {
		record  string // the file's content; empty writes none
		unit    bool
		source  string
		install *NetConfig
	}{
		"a DHCP install":                    {record: "mode: dhcp\nipv6: true\n", unit: true, source: "install", install: dhcp},
		"a static install":                  {record: "mode: static\naddress: 10.0.0.5/24\ngateway: 10.0.0.1\nipv6: true\naddress6: 2001:db8::5/64\ngateway6: fe80::1\ndns: [10.0.0.1]\n", unit: true, source: "install", install: static},
		"a record without its unit":         {record: "mode: dhcp\nipv6: true\n", source: "dhcp", install: dhcp}, // the unit sets the network
		"a cloud-config without its record": {unit: true, source: "install"},
		"no install network":                {source: "dhcp"},
		// A record that does not read, or is not a network, is left out: the rest still answers.
		"a record a newer release wrote": {record: "mode: dhcp\nmtu: 1400\n", unit: true, source: "install"},
		"an empty record":                {record: " ", unit: true, source: "install"},
		"a record of an unknown mode":    {record: "mode: statc\n", unit: true, source: "install"},
		"two records":                    {record: "mode: dhcp\n---\nmode: static\naddress: 10.0.0.5/24\n", unit: true, source: "install"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			link := newLink(t)
			h := netDaemon(t, link)
			if c.record != "" {
				if err := os.WriteFile(h.InstallNet, []byte(c.record), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if c.unit {
				if err := os.WriteFile(filepath.Join(link.dir, staticUnit(h.R.Spec.Name)), []byte("[Match]\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var n Network
			if code := call(t, h.Handler(), http.MethodGet, "/v1/network", nil, &n); code != http.StatusOK {
				t.Fatalf("%d", code)
			}
			if n.Source != c.source || !reflect.DeepEqual(n.Install, c.install) || n.Live.Interface == "" {
				t.Fatalf("source %q install %+v live %+v; want %q %+v", n.Source, n.Install, n.Live, c.source, c.install)
			}
		})
	}
}

func TestNetworkOverTheSocket(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	link := newLink(t)
	h := daemon(e, &fakeHost{})
	link.ra = true
	h.Host, h.Networkd = link, link.dir
	sock := served(t, h)
	out, err := ctl(e, sock, "network", "set", "--mode", "static", "--address", "10.0.0.60/24", "--gateway", "10.0.0.1", "--ipv6", "--address6", "2001:db8::10/64", "--gateway6", "fe80::1", "--dns", "1.1.1.1,9.9.9.9", "--revert-secs", "3600")
	if err != nil || !strings.Contains(out, "`notes-stow network confirm` keeps it within 3600 s") {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err = ctl(e, sock, "network"); err != nil {
		t.Fatal(err)
	}
	// The advertised route and the static one are one gateway.
	for _, want := range []string{"interface  ens3 52:54:00:12:34:56", "address    10.0.0.60/24, 2001:db8::5054:ff:fe12:3456/64, 2001:db8::10/64\n", "gateway    10.0.0.1, fe80::1\n", "dns        1.1.1.1, 9.9.9.9",
		"set by     `notes-stow network set`: 10.0.0.60/24 via 10.0.0.1, 2001:db8::10/64 via fe80::1, DNS 1.1.1.1, 9.9.9.9", "the cloud-config's network comes back unless `notes-stow network confirm`"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if out, err = ctl(e, sock, "network", "confirm"); err != nil || out != "confirmed\n" {
		t.Fatalf("%v %q", err, out)
	}
	if _, err = ctl(e, sock, "network", "reset", "--no-revert", "--revert-secs", "60"); err == nil || !strings.Contains(err.Error(), "one or the other") {
		t.Errorf("%v", err)
	}
	if out, err = ctl(e, sock, "network", "reset", "--no-revert"); err != nil || out != "back to the cloud-config's settings\n" {
		t.Fatalf("%v %q", err, out)
	}
	if _, err = ctl(e, sock, "network", "set", "--address", "10.0.0.60/24"); err == nil || !strings.Contains(err.Error(), `"mode" not set`) {
		t.Errorf("no --mode: %v", err)
	}
	if out, err = ctl(e, sock, "network", "--json"); err != nil {
		t.Fatal(err)
	}
	var n Network
	if err := json.Unmarshal([]byte(out), &n); err != nil || n.Source != "dhcp" || n.Override != nil {
		t.Errorf("%v %+v", err, n)
	}
}

// The CLI names the network the install set in its source line, and without the record says
// only where it comes from.
func TestNetworkPrintsTheInstallsNetwork(t *testing.T) {
	t.Parallel()
	in := &NetConfig{Mode: "static", Address: "10.0.0.5/24", Gateway: "10.0.0.1"}
	var b strings.Builder
	printNetwork(&b, "notes-stow", Network{Source: "install", Install: in})
	if !strings.Contains(b.String(), "set by     the VM's cloud-config, as `vm create` gave it: 10.0.0.5/24 via 10.0.0.1, no IPv6\n") {
		t.Errorf("%s", b.String())
	}
	// Without the record, as before.
	b.Reset()
	printNetwork(&b, "notes-stow", Network{Source: "install"})
	if !strings.Contains(b.String(), "set by     the VM's cloud-config: the network `vm create` gave\n") {
		t.Errorf("%s", b.String())
	}
}

// A bad record is logged once, however often the network is asked for.
func TestABadInstallRecordIsLoggedOnce(t *testing.T) {
	t.Parallel()
	link := newLink(t)
	h := netDaemon(t, link)
	var log strings.Builder
	h.O.Log = &log // the handler logs before it answers: no write races the read
	if err := os.WriteFile(h.InstallNet, []byte("mode: statc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		var n Network
		if code := call(t, h.Handler(), http.MethodGet, "/v1/network", nil, &n); code != http.StatusOK || n.Install != nil {
			t.Fatalf("%d %+v", code, n.Install)
		}
	}
	if got := strings.Count(log.String(), "the install's network is not answered"); got != 1 {
		t.Errorf("logged %d times:\n%s", got, log.String())
	}
	// Another error is logged in its turn.
	if err := os.WriteFile(h.InstallNet, []byte("mode: dhcp\nmtu: 1400\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	call(t, h.Handler(), http.MethodGet, "/v1/network", nil, &Network{})
	if got := strings.Count(log.String(), "the install's network is not answered"); got != 2 {
		t.Errorf("logged %d times:\n%s", got, log.String())
	}
}
