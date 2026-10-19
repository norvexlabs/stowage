package install

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// The VM's network override: the address, gateway and DNS servers an operator sets on a running VM
// (`<short>-stow network`), which outrank the cloud-config's. Its state file, on the persistent
// partition, is the one record of it: the boot renders it into networkd's directory before networkd
// starts (NetBoot), and the host daemon renders it again on each change. An addressing change is
// kept only once confirmed: until then a timer, the daemon's start and the boot each restore the
// settings it replaced.
//
//	GET  /v1/network          {live, source, override, pending}
//	POST /v1/network          {mode, address, gateway, address6, gateway6, dns, revert_secs}: set the override
//	POST /v1/network/confirm  keep the change awaiting confirmation
//	POST /v1/network/revert   undo it now
//	POST /v1/network/reset    {revert_secs}: drop the override, back to the cloud-config's settings

// The auto-revert's window, in seconds: 0 is none.
const (
	defaultRevertSecs = 90
	minRevertSecs     = 30
	maxRevertSecs     = 3600
	// A revert that fails is tried again every revertRetrySecs, revertAttempts times in all: the
	// VM may be unreachable until it succeeds.
	revertRetrySecs = 15
	revertAttempts  = 5
	// How long a static address and its default route have to reach the link after networkd
	// reloads.
	netVerifySecs = 10
	maxDNS        = 4
)

// NetConfig is a VM's network as `<short>-stow network set` gives it.
type NetConfig struct {
	Mode    string `json:"mode" yaml:"mode"`                           // dhcp or static
	Address string `json:"address,omitempty" yaml:"address,omitempty"` // static: the IPv4 address with its prefix
	Gateway string `json:"gateway,omitempty" yaml:"gateway,omitempty"` // static
	// IPv6 turns IPv6 on: off, the link has none (no link-local address, router advertisement
	// or DHCPv6). On, router advertisements (and DHCPv6 in the dhcp mode) address it.
	IPv6 bool `json:"ipv6,omitempty" yaml:"ipv6,omitempty"`
	// Address6 is a static IPv6 address with its prefix, in either mode, beside the advertised
	// ones; it needs IPv6. Gateway6 needs it.
	Address6 string   `json:"address6,omitempty" yaml:"address6,omitempty"`
	Gateway6 string   `json:"gateway6,omitempty" yaml:"gateway6,omitempty"`
	DNS      []string `json:"dns,omitempty" yaml:"dns,omitempty"` // in place of DHCP's, with dhcp
}

// NetRequest is POST /v1/network's body.
type NetRequest struct {
	NetConfig
	// RevertSecs is the window an addressing change must be confirmed in: default 90, 0 for none,
	// else 30 to 3600.
	RevertSecs *int `json:"revert_secs,omitempty"`
}

// NetResetRequest is POST /v1/network/reset's body.
type NetResetRequest struct {
	RevertSecs *int `json:"revert_secs,omitempty"`
}

// Network is what GET /v1/network answers.
type Network struct {
	Live NetLive `json:"live"`
	// Source is what sets the addressing: override (`network set`), install (the network
	// `vm create` gave, DHCP or static) or dhcp (none of them: the OS's default).
	Source   string     `json:"source"`
	Override *NetConfig `json:"override"`
	// Install is the network `vm create` gave, which `network reset` returns to while its
	// networkd file applies it (Source install); absent when the cloud-config does not record it
	// or the record does not read (logged).
	Install *NetConfig  `json:"install,omitempty"`
	Pending *NetPending `json:"pending,omitempty"`
}

// NetLive is the network the VM runs, as the kernel and the resolver report it.
type NetLive struct {
	Interface string   `json:"interface,omitempty"`
	MAC       string   `json:"mac,omitempty"`
	Addresses []string `json:"addresses"` // the interface's global IPv4 and IPv6 addresses, with their prefix
	Gateway   string   `json:"gateway,omitempty"`
	Gateways6 []string `json:"gateways6,omitempty"` // the IPv6 default routes', static or advertised (a router each)
	DNS       []string `json:"dns"`
}

// NetPending is a change awaiting confirmation: past Deadline, Previous comes back (nil: the
// cloud-config's settings).
type NetPending struct {
	Previous *NetConfig `json:"previous"`
	Deadline time.Time  `json:"deadline"`
	SecsLeft int        `json:"secs_left"`
}

// netState is the override's state file.
type netState struct {
	// Version is the record's format: absent (0) is IPv6 on, implicit, which an older release
	// (refusing the version and the ipv6 key) also reads; netStateVersion carries the choice.
	// write uses the format without a version whenever it says the same.
	Version int         `yaml:"version,omitempty"`
	Config  *NetConfig  `yaml:"config,omitempty"` // nil: no override
	Pending *netPending `yaml:"pending,omitempty"`
}

// netStateVersion is the format NetConfig.IPv6 is the choice in, which write records when the
// format without a version (IPv6 on, implicit) cannot say the same. 1 is no format.
const netStateVersion = 2

// ipv6KeyRe finds an ipv6 key in a record: one without a version may not hold it.
var ipv6KeyRe = regexp.MustCompile(`(?m)(^|[\s{,])ipv6\s*:`)

type netPending struct {
	ID       string     `yaml:"id"`
	Deadline time.Time  `yaml:"deadline"`
	Previous *NetConfig `yaml:"previous,omitempty"`
}

// NetworkFile is the override's state file in the install directory dir.
func NetworkFile(dir string) string { return filepath.Join(dir, "network.yaml") }

// netFiles are where the override is rendered: networkd's directory and the console banner's.
type netFiles struct {
	// name is the project's, which names the files; ctl its installer's, which they name to the
	// operator.
	name, ctl, state, networkd, issue string
	// install is the file the cloud-config records the install's network in.
	install string
}

// overrideUnit is the override's networkd file: it sorts before the cloud-config's (staticUnit)
// and Kairos's DHCP default, and networkd takes the first file that matches.
func (f netFiles) overrideUnit() string {
	return filepath.Join(f.networkd, "01-"+f.name+"-override.network")
}

func (f netFiles) banner() string { return filepath.Join(f.issue, "06-"+f.name+"-network.issue") }

// staticUnit is the networkd file the cloud-config writes for the network `vm create` gave, DHCP
// included. Its name stays: the seeds of the VMs already made write it.
func staticUnit(name string) string { return "05-" + name + "-static.network" }

func (h *Hostd) netFiles() netFiles {
	f := newNetFiles(h.R.Spec.Name, h.R.Ctl(), h.O.Dir, h.Networkd, h.Issue)
	f.install = cmp.Or(h.InstallNet, f.install)
	return f
}

// newNetFiles are the override's files of the install in dir; an empty networkd is
// /etc/systemd/network, an empty issue /etc/issue.d.
func newNetFiles(name, ctl, dir, networkd, issue string) netFiles {
	return netFiles{
		name:     name,
		ctl:      ctl,
		state:    NetworkFile(dir),
		networkd: cmp.Or(networkd, "/etc/systemd/network"),
		issue:    cmp.Or(issue, "/etc/issue.d"),
		install:  InstallNetworkFile(name),
	}
}

// readInstall strictly decodes the network the cloud-config records — one NetConfig document,
// its mode one this release knows; nil when it records none. It is a network already applied,
// not a change, so `network set`'s checks (which a later release may tighten) are not made. The
// cloud-config is frozen at `vm create` and every later release reads its record, so NetConfig's
// YAML fields may only be added, never renamed or dropped. A record that does not read is an
// error the caller reports beside an answer without it — never a network the install did not
// set.
func (f netFiles) readInstall() (*NetConfig, error) {
	raw, err := os.ReadFile(f.install)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c NetConfig
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", f.install, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: more than one network", f.install)
	}
	if c.Mode != "dhcp" && c.Mode != "static" {
		return nil, fmt.Errorf("%s: mode %q is neither dhcp nor static", f.install, c.Mode)
	}
	return &c, nil
}

// read strictly decodes the state file; none is no override.
func (f netFiles) read() (netState, error) {
	var st netState
	raw, err := os.ReadFile(f.state)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, unreadable{f, err}
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&st); err != nil && !errors.Is(err, io.EOF) {
		return st, unreadable{f, fmt.Errorf("%s: %w", f.state, err)}
	}
	switch {
	case st.Version > netStateVersion:
		return st, unreadable{f, fmt.Errorf("%s: version %d, from a later release", f.state, st.Version)}
	case st.Version != 0 && st.Version != netStateVersion:
		return st, unreadable{f, fmt.Errorf("%s: version %d: 2, or none", f.state, st.Version)}
	case st.Version == 0 && ipv6KeyRe.Match(raw):
		// Without a version, IPv6 is implicit: a record saying otherwise is not one write makes.
		return st, unreadable{f, fmt.Errorf("%s: ipv6 is set, so the record needs version: %d", f.state, netStateVersion)}
	}
	for _, c := range []*NetConfig{st.Config, previousOf(st.Pending)} {
		if c == nil {
			continue
		}
		if st.Version == 0 {
			if c.Address6 != "" || c.Gateway6 != "" {
				return st, unreadable{f, fmt.Errorf("%s: address6 and gateway6 need version: %d", f.state, netStateVersion)}
			}
			c.IPv6 = true // a record without a version: IPv6 on
		}
		if _, err := validNet(*c); err != nil {
			return st, unreadable{f, fmt.Errorf("%s: %w", f.state, err)}
		}
	}
	return st, nil
}

// unreadable is a state file that does not read: its error names the verb that drops it.
type unreadable struct {
	f   netFiles
	err error
}

func (e unreadable) Error() string {
	return fmt.Sprintf("%v (`%s network reset` drops it)", e.err, e.f.ctl)
}

func (e unreadable) Unwrap() error { return e.err }

func previousOf(p *netPending) *NetConfig {
	if p == nil {
		return nil
	}
	return p.Previous
}

// write records st, root's alone; an empty state removes the file.
func (f netFiles) write(st netState) error {
	if st.Config == nil && st.Pending == nil {
		if err := os.Remove(f.state); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	// A record an older release reads the same — every config, the pending change's previous one
	// included, with IPv6 on and no static IPv6 address (which read refuses without a version) —
	// is written without the version or the ipv6 key its strict decoding refuses: a rollback then
	// keeps the override.
	st.Version = 0
	legacy := true
	for _, c := range []*NetConfig{st.Config, previousOf(st.Pending)} {
		legacy = legacy && (c == nil || c.IPv6 && c.Address6 == "" && c.Gateway6 == "")
	}
	if legacy {
		st = legacyState(st)
	} else {
		st.Version = netStateVersion
	}
	raw, err := yaml.Marshal(st)
	if err != nil {
		return err
	}
	head := fmt.Sprintf("# %s's network override, which `%s network` changes: the boot renders it.\n", f.name, f.ctl)
	return writePrivate(f.state, append([]byte(head), raw...))
}

// legacyState is st with IPv6 left implicit in each config, as a record without a version reads.
func legacyState(st netState) netState {
	strip := func(c *NetConfig) *NetConfig {
		if c == nil {
			return nil
		}
		cp := *c
		cp.IPv6 = false
		return &cp
	}
	st.Config = strip(st.Config)
	if st.Pending != nil {
		p := *st.Pending
		p.Previous = strip(p.Previous)
		st.Pending = &p
	}
	return st
}

// render writes the networkd file of c, or removes it when c is nil (the cloud-config's settings),
// and the console banner of st.
func (f netFiles) render(c *NetConfig, st netState) error {
	if c == nil {
		if err := os.Remove(f.overrideUnit()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else {
		if err := os.MkdirAll(f.networkd, 0o755); err != nil {
			return err
		}
		if err := writeAtomic(f.overrideUnit(), strings.NewReader(renderNetwork(f.name, f.ctl, *c)), 0o644, nil); err != nil {
			return err
		}
	}
	return f.writeBanner(st)
}

// writeBanner tells, on the console's login screen, what sets the network and whether a change
// awaits confirmation; the line before it gives the address (\4).
func (f netFiles) writeBanner(st netState) error {
	ctl := f.ctl
	var b strings.Builder
	switch {
	case st.Config != nil:
		fmt.Fprintf(&b, "network: %s; set with `%s network set`\n", describeNet(st.Config), ctl)
	default:
		// At boot the cloud-config's static file is not written yet: the banner cannot tell.
		b.WriteString("network: the VM's cloud-config (DHCP, or its static address)\n")
	}
	if st.Pending != nil {
		fmt.Fprintf(&b, "         not confirmed: it goes back at %s unless `%s network confirm` keeps it\n", st.Pending.Deadline.UTC().Format("15:04:05 UTC"), ctl)
	}
	if err := os.MkdirAll(f.issue, 0o755); err != nil {
		return err
	}
	return writeAtomic(f.banner(), strings.NewReader(b.String()), 0o644, nil)
}

// renderNetwork is the networkd file of a valid c: on the VM's one wired interface, whatever its
// name.
func renderNetwork(name, ctl string, c NetConfig) string {
	return fmt.Sprintf("# %s's network override, rendered from %s at every boot: `%s network` changes it.\n", name, NetworkFile(VMDir(name)), ctl) +
		"[Match]\nName=en* eth*\n\n" + networkSections(c)
}

// networkSections is a .network file's sections after [Match]: c's addressing and DNS servers.
// A gateway outside its address's prefix (an IPv4 /31 or /32, an IPv6 /127 or /128) is reached
// on the link: a route of its own says so. An IPv6 link-local gateway is on the link already.
func networkSections(c NetConfig) string {
	var b strings.Builder
	b.WriteString("[Network]\n")
	var onLink []string
	gateway := func(addr, gw string) {
		if gw == "" {
			return
		}
		p, _ := netip.ParsePrefix(addr)
		g, _ := netip.ParseAddr(gw)
		if netOnLink(p, g) && !g.IsLinkLocalUnicast() {
			onLink = append(onLink, gw)
			return
		}
		fmt.Fprintf(&b, "Gateway=%s\n", gw)
	}
	switch {
	case c.Mode == "static":
		fmt.Fprintf(&b, "Address=%s\n", c.Address)
		gateway(c.Address, c.Gateway)
	case c.IPv6:
		b.WriteString("DHCP=yes\n")
	default:
		b.WriteString("DHCP=ipv4\n")
	}
	if !c.IPv6 {
		b.WriteString("LinkLocalAddressing=no\nIPv6AcceptRA=no\n")
	}
	if c.Address6 != "" {
		fmt.Fprintf(&b, "Address=%s\n", c.Address6)
		gateway(c.Address6, c.Gateway6)
	}
	for _, d := range c.DNS {
		fmt.Fprintf(&b, "DNS=%s\n", d)
	}
	// The servers given replace those of DHCP (v4 and v6) and of router advertisements: networkd
	// would add them.
	if len(c.DNS) > 0 {
		b.WriteString("\n[DHCPv4]\nUseDNS=no\n")
	}
	// With DHCP and IPv6, the DHCPv6 client starts at once, not only once a router advertisement
	// sets its managed flag: the clouds (AWS, Azure, Google Cloud) give the address by DHCPv6.
	dhcp6 := c.IPv6 && c.Mode != "static"
	if len(c.DNS) > 0 || dhcp6 {
		b.WriteString("\n[DHCPv6]\n")
		if dhcp6 {
			b.WriteString("WithoutRA=solicit\n")
		}
		if len(c.DNS) > 0 {
			b.WriteString("UseDNS=no\n")
		}
	}
	if len(c.DNS) > 0 {
		b.WriteString("\n[IPv6AcceptRA]\nUseDNS=no\n")
	}
	for _, g := range onLink {
		fmt.Fprintf(&b, "\n[Route]\nGateway=%s\nGatewayOnLink=yes\n", g)
	}
	return b.String()
}

// netOnLink reports whether the gateway g is outside the address's prefix p: reached on the link,
// which validNet takes for a /31 or /32 only.
func netOnLink(p netip.Prefix, g netip.Addr) bool { return !p.Masked().Contains(g) }

// hostAddr parses an address a VM may use as its own, its gateway or a DNS server: no zone, and
// unicast (not loopback, link-local, multicast, unspecified or 240.0.0.0/4).
func hostAddr(field, v string, v6 bool) (netip.Addr, error) {
	a, err := netip.ParseAddr(v)
	switch {
	case err != nil:
		return a, fmt.Errorf("%s %q is not an IP address", field, v)
	case a.Zone() != "":
		return a, fmt.Errorf("%s %q carries an interface zone", field, v)
	case !a.Is4() && !v6:
		return a, fmt.Errorf("%s %q is not an IPv4 address", field, v)
	case a.IsLoopback(), a.IsMulticast(), a.IsUnspecified(), a.IsLinkLocalUnicast(),
		a.Is4() && netip.MustParsePrefix("240.0.0.0/4").Contains(a):
		return a, fmt.Errorf("%s %s is not an address a host uses", field, v)
	}
	return a, nil
}

// validNet checks c and returns it in canonical form. The static mode's address is IPv4; an IPv6
// address beside it, in either mode, is checked by validNet6; DNS servers may be either. Every
// IPv6 setting needs IPv6 on. A static
// address up to /30 is neither its subnet's network nor broadcast address, and its gateway is in
// its subnet; /31 and /32 reach their gateway on the link.
func validNet(c NetConfig) (NetConfig, error) {
	out := NetConfig{Mode: c.Mode}
	switch c.Mode {
	case "dhcp":
		if c.Address != "" || c.Gateway != "" {
			return out, errors.New("an address and a gateway are for the static mode")
		}
	case "static":
		p, err := netip.ParsePrefix(c.Address)
		if err != nil {
			return out, fmt.Errorf("address %q is not an address with its prefix (192.0.2.10/24)", c.Address)
		}
		a, err := hostAddr("address", p.Addr().String(), false)
		if err != nil {
			return out, err
		}
		if p.Bits() < 1 {
			return out, fmt.Errorf("address %s: the prefix is from 1 to 32", c.Address)
		}
		subnet := p.Masked()
		if p.Bits() <= 30 {
			bcast := subnet.Addr().As4()
			for i := p.Bits(); i < 32; i++ {
				bcast[i/8] |= 1 << (7 - i%8)
			}
			if a == subnet.Addr() {
				return out, fmt.Errorf("address %s is the network address of %s", c.Address, subnet)
			}
			if a == netip.AddrFrom4(bcast) {
				return out, fmt.Errorf("address %s is the broadcast address of %s", c.Address, subnet)
			}
		}
		out.Address = p.String()
		if c.Gateway != "" {
			g, err := hostAddr("gateway", c.Gateway, false)
			if err != nil {
				return out, err
			}
			if g == a {
				return out, fmt.Errorf("gateway %s is the VM's own address", g)
			}
			if p.Bits() <= 30 && netOnLink(p, g) {
				return out, fmt.Errorf("gateway %s is outside %s: nothing would route past the link", g, subnet)
			}
			out.Gateway = g.String()
		}
	default:
		return out, fmt.Errorf("mode %q: dhcp or static", c.Mode)
	}
	out.IPv6 = c.IPv6
	if !c.IPv6 && (c.Address6 != "" || c.Gateway6 != "") {
		return out, errors.New("address6 and gateway6 are for IPv6, which is off: turn it on (ipv6)")
	}
	var err error
	if out.Address6, out.Gateway6, err = validNet6(c.Address6, c.Gateway6); err != nil {
		return out, err
	}
	for _, d := range c.DNS {
		a, err := hostAddr("DNS server", strings.TrimSpace(d), true)
		if err != nil {
			return out, err
		}
		if a.Is6() && !c.IPv6 {
			return out, fmt.Errorf("DNS server %s is IPv6, which is off: turn it on (ipv6)", a)
		}
		if !slices.Contains(out.DNS, a.String()) {
			out.DNS = append(out.DNS, a.String())
		}
	}
	if len(out.DNS) > maxDNS {
		return out, fmt.Errorf("%d DNS servers: at most %d", len(out.DNS), maxDNS)
	}
	return out, nil
}

// validNet6 checks a static IPv6 address and its gateway, both optional, and returns them in
// canonical form. The address is a global or unique local unicast address with a prefix of 1 to
// 128 and, up to /126, not its subnet's anycast address (all zeros after the prefix). The gateway
// is link-local (on the link whatever the prefix) or in the address's subnet; /127 and /128 reach
// any gateway on the link. A gateway alone is refused: router advertisements give one with the
// address they give.
func validNet6(address, gateway string) (string, string, error) {
	if address == "" {
		if gateway != "" {
			return "", "", errors.New("gateway6 is for a static IPv6 address (address6): without one, the router advertisements give the gateway")
		}
		return "", "", nil
	}
	p, err := netip.ParsePrefix(address)
	if err != nil || !p.Addr().Is6() || p.Addr().Is4In6() {
		return "", "", fmt.Errorf("address6 %q is not an IPv6 address with its prefix (2001:db8::10/64)", address)
	}
	a, err := hostAddr("address6", p.Addr().String(), true)
	if err != nil {
		return "", "", err
	}
	if p.Bits() < 1 {
		return "", "", fmt.Errorf("address6 %s: the prefix is from 1 to 128", address)
	}
	subnet := p.Masked()
	if p.Bits() <= 126 && a == subnet.Addr() {
		return "", "", fmt.Errorf("address6 %s is the subnet-router anycast address of %s", address, subnet)
	}
	if gateway == "" {
		return p.String(), "", nil
	}
	g, err := netip.ParseAddr(gateway)
	switch {
	case err != nil:
		return "", "", fmt.Errorf("gateway6 %q is not an IP address", gateway)
	case g.Zone() != "":
		return "", "", fmt.Errorf("gateway6 %q carries an interface zone", gateway)
	case !g.Is6() || g.Is4In6():
		return "", "", fmt.Errorf("gateway6 %q is not an IPv6 address", gateway)
	case !g.IsLinkLocalUnicast():
		if _, err := hostAddr("gateway6", gateway, true); err != nil {
			return "", "", err
		}
		if p.Bits() <= 126 && netOnLink(p, g) {
			return "", "", fmt.Errorf("gateway6 %s is outside %s: nothing would route past the link (a link-local gateway, fe80::…, is on any link)", g, subnet)
		}
	}
	if g == a {
		return "", "", fmt.Errorf("gateway6 %s is the VM's own address", g)
	}
	return p.String(), g.String(), nil
}

// revertSecs checks the window asked for: nil is the default.
func revertSecs(v *int) (int, error) {
	if v == nil {
		return defaultRevertSecs, nil
	}
	if s := *v; s != 0 && (s < minRevertSecs || s > maxRevertSecs) {
		return 0, fmt.Errorf("revert_secs %d: 0 (no revert) or %d to %d", s, minRevertSecs, maxRevertSecs)
	}
	return *v, nil
}

// addressing reports whether going from cur to next may move the VM: a DNS change alone cannot.
// From no override, any change may (the lease or the cloud-config's address may differ).
func addressing(cur *NetConfig, next NetConfig) bool {
	return cur == nil || cur.Mode != next.Mode || cur.Address != next.Address || cur.Gateway != next.Gateway ||
		cur.IPv6 != next.IPv6 || cur.Address6 != next.Address6 || cur.Gateway6 != next.Gateway6
}

// NetBoot renders the override of the install in dir for the boot, before networkd starts: a
// change still awaiting confirmation is undone first, as a reboot inside its window means it was
// never confirmed.
func NetBoot(name, ctl, dir string, log io.Writer) error {
	return newNetFiles(name, ctl, dir, "", "").boot(log)
}

func (f netFiles) boot(log io.Writer) error {
	st, err := f.read()
	if err != nil {
		// Unreadable, it sets nothing: the cloud-config's settings bring the VM up.
		return errors.Join(fmt.Errorf("the network override: %w (the cloud-config's settings apply)", err), f.render(nil, netState{}))
	}
	if st.Pending != nil {
		fmt.Fprintf(log, "the network change awaiting confirmation was not confirmed before the reboot: the settings before it come back\n")
		st = netState{Config: st.Pending.Previous}
		if err := f.write(st); err != nil {
			return errors.Join(err, f.render(nil, netState{}))
		}
	}
	return f.render(st.Config, st)
}

// netLive reads the network the VM runs: the interface of the default route (else the first
// wired one), its addresses, the gateway, the DNS servers.
func (h *Hostd) netLive(ctx context.Context) (NetLive, error) {
	live := NetLive{Addresses: []string{}, DNS: []string{}}
	routes, err := h.Host.Run(ctx, Cmd{Args: []string{"ip", "-j", "route", "show", "default"}})
	if err != nil {
		return live, fmt.Errorf("ip route: %w", err)
	}
	links, err := h.Host.Run(ctx, Cmd{Args: []string{"ip", "-j", "address", "show"}})
	if err != nil {
		return live, fmt.Errorf("ip address: %w", err)
	}
	var rs []ipRoute
	_ = json.Unmarshal(routes, &rs)
	var ls []ipLink
	_ = json.Unmarshal(links, &ls)
	for _, r := range rs {
		if r.Dev != "" && (r.Dst == "default" || r.Dst == "") {
			live.Interface, live.Gateway = r.Dev, r.Gateway
			break
		}
	}
	if live.Interface == "" {
		for _, l := range ls {
			if strings.HasPrefix(l.Name, "en") || strings.HasPrefix(l.Name, "eth") {
				live.Interface = l.Name
				break
			}
		}
	}
	// The IPv6 default routes on that interface, static or advertised: none where IPv6 is off.
	if out, err := h.Host.Run(ctx, Cmd{Args: []string{"ip", "-6", "-j", "route", "show", "default"}}); err == nil {
		var rs6 []ipRoute
		_ = json.Unmarshal(out, &rs6)
		for _, r := range rs6 {
			for _, n := range append([]ipHop{r.ipHop}, r.Nexthops...) {
				if n.Gateway != "" && n.Dev == live.Interface && !slices.Contains(live.Gateways6, n.Gateway) {
					live.Gateways6 = append(live.Gateways6, n.Gateway)
				}
			}
		}
	}
	for _, l := range ls {
		if l.Name != live.Interface {
			continue
		}
		live.MAC = l.MAC
		live.Addresses = append(live.Addresses, l.global()...)
	}
	if out, err := h.Host.Run(ctx, Cmd{Args: []string{"resolvectl", "dns"}}); err == nil {
		live.DNS = parseResolvectl(string(out), live.Interface)
	}
	return live, nil
}

// ipRoute and ipLink are what `ip -j` prints of a route and of a link.
type ipRoute struct {
	Dst string `json:"dst"`
	ipHop
	// Nexthops are a multipath route's, which has no gateway of its own: the kernel merges IPv6
	// default routes of one metric (a static one and an advertised one) into one.
	Nexthops []ipHop `json:"nexthops"`
}

// ipHop is a route's next hop.
type ipHop struct {
	Gateway string `json:"gateway"`
	Dev     string `json:"dev"`
}

type ipLink struct {
	Name  string `json:"ifname"`
	MAC   string `json:"address"`
	Addrs []struct {
		Family string `json:"family"`
		Local  string `json:"local"`
		Prefix int    `json:"prefixlen"`
		Scope  string `json:"scope"`
		// An IPv6 address is tentative until duplicate address detection passes; dadfailed when
		// another host has it.
		Tentative bool `json:"tentative"`
		DADFailed bool `json:"dadfailed"`
	} `json:"addr_info"`
}

// global is the link's global IPv4 and IPv6 addresses, with their prefix: an IPv6 address
// another host has, or not yet checked for one, is not the link's.
func (l ipLink) global() []string {
	var out []string
	for _, a := range l.Addrs {
		if (a.Family == "inet" || a.Family == "inet6") && a.Scope == "global" && a.Local != "" && !a.Tentative && !a.DADFailed {
			out = append(out, fmt.Sprintf("%s/%d", a.Local, a.Prefix))
		}
	}
	return out
}

// parseResolvectl reads `resolvectl dns`: the interface's servers, else the global ones.
func parseResolvectl(out, iface string) []string {
	link, global := []string{}, []string{}
	for l := range strings.SplitSeq(out, "\n") {
		head, servers, ok := strings.Cut(strings.TrimSpace(l), ":")
		if !ok {
			continue
		}
		switch {
		case head == "Global":
			global = strings.Fields(servers)
		case strings.HasPrefix(head, "Link ") && strings.HasSuffix(head, "("+iface+")"):
			link = strings.Fields(servers)
		}
	}
	if len(link) > 0 {
		return link
	}
	return global
}

// netApply renders c (nil: the cloud-config's settings) and the banner of st, has networkd reload
// and reconfigure iface, then, for static addresses (IPv4, IPv6 or both), waits for them and their
// default routes to reach iface: a change networkd took but did not apply is an error, as is an
// IPv6 address another host has.
func (h *Hostd) netApply(ctx context.Context, c *NetConfig, st netState, iface string) error {
	f := h.netFiles()
	if err := f.render(c, st); err != nil {
		return err
	}
	if _, err := h.Host.Run(ctx, Cmd{Args: []string{"networkctl", "reload"}}); err != nil {
		return fmt.Errorf("networkctl reload: %w", err)
	}
	if iface != "" {
		if _, err := h.Host.Run(ctx, Cmd{Args: []string{"networkctl", "reconfigure", iface}}); err != nil {
			h.O.logf("networkctl reconfigure %s: %v", iface, err)
		}
	}
	h.reloadBanner(ctx)
	if c == nil || c.Mode != "static" && c.Address6 == "" {
		return nil
	}
	var want []string // the static addresses, IPv4 then IPv6
	if c.Mode == "static" {
		want = append(want, c.Address)
	}
	if c.Address6 != "" {
		want = append(want, c.Address6)
	}
	deadline := time.Now().Add(netVerifySecs * h.unit())
	for {
		live, err := h.netLive(ctx)
		if err == nil {
			// missing is the first static setting the link does not run: an address (an IPv6
			// one only once duplicate address detection passed), then a default route.
			missing := ""
			for _, a := range want {
				w, _ := netip.ParsePrefix(a)
				if !slices.ContainsFunc(live.Addresses, func(s string) bool {
					p, err := netip.ParsePrefix(s)
					return err == nil && p == w
				}) {
					missing = "the address " + a
					break
				}
			}
			if missing == "" && live.Interface != "" && iface != "" && live.Interface != iface {
				missing = "the default route on " + iface // another link took it
			}
			switch {
			case missing != "":
			case c.Gateway != "" && live.Gateway != c.Gateway:
				missing = "the default route via " + c.Gateway
			case c.Gateway6 != "" && !slices.Contains(live.Gateways6, c.Gateway6):
				missing = "the IPv6 default route via " + c.Gateway6
			}
			if missing == "" {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s did not reach the interface within %d s: networkd did not apply the change", missing, netVerifySecs)
			}
		} else if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(h.unit() / 2):
		}
	}
}

// reloadBanner has the console's login screen show the banner again.
func (h *Hostd) reloadBanner(ctx context.Context) {
	if _, err := h.Host.Run(ctx, Cmd{Args: []string{"agetty", "--reload"}}); err != nil {
		h.O.logf("agetty --reload: %v", err)
	}
}

func (h *Hostd) unit() time.Duration {
	if h.Unit == 0 {
		return time.Second
	}
	return h.Unit
}

// armRevert runs undo at deadline, as attempt (1 for a window's end); when undo fails, it is tried
// again every revertRetrySecs, revertAttempts times in all: the VM may be unreachable until it
// succeeds. Any later change to the network disarms it (netDisarm).
func (h *Hostd) armRevert(deadline time.Time, attempt int, why string, undo func(context.Context) error) {
	h.netDisarm()
	gen := h.netGen
	h.netTimer = time.AfterFunc(time.Until(deadline), func() {
		h.netFired.Store(int64(gen)) // a confirmation from now on comes too late
		h.netMu.Lock()
		defer h.netMu.Unlock()
		if h.netGen != gen {
			return // disarmed while this waited for the lock
		}
		h.netTimer = nil
		_ = h.undo(attempt, why, undo)
	})
}

// undo runs undo now, as attempt; when it fails, armRevert tries it again. netMu is held.
func (h *Hostd) undo(attempt int, why string, undo func(context.Context) error) error {
	err := undo(h.ctx)
	switch {
	case err == nil:
		h.O.logf("network change undone: %s", why)
	case h.ctx.Err() != nil:
		h.O.logf("undoing the network change (%s): %v; the daemon stops: its start undoes it", why, err)
	case attempt < revertAttempts:
		h.O.logf("undoing the network change (%s, attempt %d of %d): %v; trying again in %d s", why, attempt, revertAttempts, err, revertRetrySecs)
		h.armRevert(time.Now().Add(revertRetrySecs*h.unit()), attempt+1, why, undo)
	default:
		h.O.logf("undoing the network change (%s): %v; given up after %d attempts", why, err, revertAttempts)
	}
	return err
}

// netDisarm stops the armed revert, one whose timer already fired included. netMu is held.
func (h *Hostd) netDisarm() {
	h.netGen++
	if h.netTimer != nil {
		h.netTimer.Stop()
		h.netTimer = nil
	}
}

// revertWindow undoes the change awaiting confirmation in the window id: the settings before it are
// recorded, then rendered and applied. Once recorded, a later attempt renders and applies the state
// again. netMu is held.
func (h *Hostd) revertWindow(id string) func(context.Context) error {
	return func(ctx context.Context) error {
		f := h.netFiles()
		st, err := f.read()
		if err != nil {
			return err
		}
		if st.Pending != nil {
			if st.Pending.ID != id {
				return nil // another window, which its own timer ends
			}
			st = netState{Config: st.Pending.Previous}
			if err := f.write(st); err != nil {
				return err
			}
		}
		live, _ := h.netLive(ctx)
		return h.netApply(ctx, st.Config, st, live.Interface)
	}
}

// restore records c (nil: the cloud-config's settings) as the override, with no window, and
// applies it on iface. netMu is held.
func (h *Hostd) restore(c *NetConfig, iface string) func(context.Context) error {
	return func(ctx context.Context) error {
		st := netState{Config: c}
		if err := h.netFiles().write(st); err != nil {
			return err
		}
		return h.netApply(ctx, c, st, iface)
	}
}

// NetResume undoes, as the daemon starts, a change still awaiting confirmation: a restart inside
// its window means it was not confirmed.
func (h *Hostd) NetResume() {
	h.netMu.Lock()
	defer h.netMu.Unlock()
	h.netResume()
}

// netResume is NetResume with netMu held.
func (h *Hostd) netResume() {
	f := h.netFiles()
	st, err := f.read()
	if err != nil {
		h.O.logf("the network override: %v", err)
		return
	}
	if st.Pending != nil {
		_ = h.undo(1, "the host daemon restarted before it was confirmed", h.revertWindow(st.Pending.ID))
		return
	}
	// A daemon stopped between recording a change and applying it left networkd on another: the
	// record is applied again.
	want := ""
	if st.Config != nil {
		want = renderNetwork(f.name, f.ctl, *st.Config)
	}
	if got, _ := os.ReadFile(f.overrideUnit()); string(got) != want {
		_ = h.undo(1, "the networkd file differs from the record", func(ctx context.Context) error {
			live, _ := h.netLive(ctx)
			return h.netApply(ctx, st.Config, st, live.Interface)
		})
	}
}

func (h *Hostd) network(w http.ResponseWriter, r *http.Request) {
	h.netMu.Lock()
	defer h.netMu.Unlock()
	f := h.netFiles()
	st, err := f.read()
	if err != nil {
		replyErr(w, err)
		return
	}
	live, err := h.netLive(r.Context())
	if err != nil {
		replyErr(w, err)
		return
	}
	// The live network and the override answer whatever the record holds: they are what an
	// operator recovers with.
	install, err := f.readInstall()
	if err != nil && h.installNetLogged.Swap(err.Error()) != any(err.Error()) {
		h.O.logf("the install's network is not answered: %v", err)
	}
	n := Network{Live: live, Override: st.Config, Install: install, Source: "dhcp"}
	switch {
	case st.Config != nil:
		n.Source = "override"
	// The unit sets the network, the record only describes it: without the unit networkd runs
	// the OS's DHCP default, whatever the record says.
	case fileExists(filepath.Join(f.networkd, staticUnit(f.name))):
		n.Source = "install"
	}
	n.Pending = h.pending(st)
	reply(w, http.StatusOK, n)
}

// netChange checks a change may start: no reboot scheduled, no change awaiting confirmation. It
// returns the state. netMu is held.
func (h *Hostd) netChange() (netState, error) {
	if err := h.noReboot(); err != nil {
		return netState{}, err
	}
	st, err := h.netFiles().read()
	if err != nil {
		return st, err
	}
	return st, h.noPending(st)
}

// noReboot refuses a change once a reboot is scheduled.
func (h *Hostd) noReboot() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rebooting {
		return fail(http.StatusConflict, "a reboot is scheduled")
	}
	return nil
}

// noPending refuses a change while another awaits confirmation.
func (h *Hostd) noPending(st netState) error {
	if st.Pending != nil {
		return fail(http.StatusConflict, "a network change awaits confirmation: `%s network confirm` keeps it, `%s network revert` undoes it", h.R.Ctl(), h.R.Ctl())
	}
	return nil
}

// netSet makes next (nil: the cloud-config's settings) the override. When it may move the VM and
// secs is not 0, it is recorded as awaiting confirmation, and its window armed, before networkd
// sees it. A change that fails is undone at once. netMu is held.
func (h *Hostd) netSet(cur, next *NetConfig, secs int, arm bool) (netState, error) {
	f := h.netFiles()
	// A revert still being tried belongs to the change this one replaces.
	h.netDisarm()
	live, _ := h.netLive(h.ctx)
	st := netState{Config: next}
	if arm && secs > 0 {
		st.Pending = &netPending{ID: rand.Text(), Deadline: time.Now().Add(time.Duration(secs) * h.unit()), Previous: cur}
	}
	if err := f.write(st); err != nil {
		return st, err
	}
	if st.Pending != nil {
		h.armRevert(st.Pending.Deadline, 1, "not confirmed in time", h.revertWindow(st.Pending.ID))
	}
	// The daemon's context, not the request's: a client that goes away (its SSH session cut by
	// the change itself) does not make a change that worked fail.
	if err := h.netApply(h.ctx, next, st, live.Interface); err != nil {
		h.netDisarm()
		if uerr := h.undo(1, "it could not be applied", h.restore(cur, live.Interface)); uerr != nil {
			return st, fmt.Errorf("%w; restoring the settings before it failed too, and is tried again every %d s: %v", err, revertRetrySecs, uerr)
		}
		return st, fmt.Errorf("%w: the settings before it are back", err)
	}
	return st, nil
}

// dropUnreadable removes the state file that did not read and applies the cloud-config's
// settings. netMu is held.
func (h *Hostd) dropUnreadable(why error) error {
	h.netDisarm()
	f := h.netFiles()
	if err := os.Remove(f.state); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	h.O.logf("the network override did not read (%v): dropped", why)
	live, _ := h.netLive(h.ctx)
	return h.netApply(h.ctx, nil, netState{}, live.Interface)
}

// NetAnswer is what the network changes answer.
type NetAnswer struct {
	Message  string      `json:"message"`
	Override *NetConfig  `json:"override"`
	Pending  *NetPending `json:"pending,omitempty"`
}

func (h *Hostd) answer(st netState, msg string) NetAnswer {
	return NetAnswer{Message: msg, Override: st.Config, Pending: h.pending(st)}
}

// pending is st's change awaiting confirmation, as the API tells it.
func (h *Hostd) pending(st netState) *NetPending {
	p := st.Pending
	if p == nil {
		return nil
	}
	return &NetPending{Previous: p.Previous, Deadline: p.Deadline, SecsLeft: max(int(time.Until(p.Deadline)/h.unit()), 0)}
}

func (h *Hostd) setNetwork(w http.ResponseWriter, r *http.Request) {
	var req NetRequest
	if err := decodeBody(r, &req); err != nil {
		replyErr(w, err)
		return
	}
	next, err := validNet(req.NetConfig)
	if err != nil {
		replyErr(w, fail(http.StatusBadRequest, "%v", err))
		return
	}
	secs, err := revertSecs(req.RevertSecs)
	if err != nil {
		replyErr(w, fail(http.StatusBadRequest, "%v", err))
		return
	}
	h.netMu.Lock()
	defer h.netMu.Unlock()
	cur, err := h.netChange()
	if err != nil {
		replyErr(w, err)
		return
	}
	arm := addressing(cur.Config, next)
	st, err := h.netSet(cur.Config, &next, secs, arm)
	if err != nil {
		replyErr(w, err)
		return
	}
	msg := "applied"
	switch {
	case st.Pending != nil:
		msg = fmt.Sprintf("applied: `%s network confirm` keeps it within %d s, else the settings before it come back", h.R.Ctl(), secs)
	case arm:
		msg = "applied, without a revert window"
	}
	reply(w, http.StatusOK, h.answer(st, msg))
}

func (h *Hostd) confirmNetwork(w http.ResponseWriter, r *http.Request) {
	h.netMu.Lock()
	defer h.netMu.Unlock()
	a, err := h.confirm()
	if err != nil {
		replyErr(w, err)
		return
	}
	reply(w, http.StatusOK, a)
}

// confirm keeps the change awaiting confirmation. One whose window ended — its timer fired, and
// waits for netMu — is undone instead. netMu is held.
func (h *Hostd) confirm() (NetAnswer, error) {
	f := h.netFiles()
	st, err := f.read()
	if err != nil {
		return NetAnswer{}, err
	}
	if st.Pending == nil {
		return NetAnswer{}, fail(http.StatusConflict, "nothing awaits confirmation: a change not confirmed in its window, or before a restart or a reboot, is undone (`%s network` shows the network)", h.R.Ctl())
	}
	if p := st.Pending; h.netGen != 0 && h.netFired.Load() == int64(h.netGen) {
		h.netDisarm()
		if err := h.undo(1, "not confirmed in time", h.revertWindow(p.ID)); err != nil {
			return NetAnswer{}, fail(http.StatusConflict, "the change's window ended: undoing it failed, and is tried again every %d s: %v", revertRetrySecs, err)
		}
		return NetAnswer{}, fail(http.StatusConflict, "the change's window ended: the settings before it are back")
	}
	// Recorded first: a confirmation that is not recorded leaves the window armed.
	st.Pending = nil
	if err := f.write(st); err != nil {
		return NetAnswer{}, err
	}
	h.netDisarm()
	if err := f.writeBanner(st); err != nil {
		h.O.logf("the console banner: %v", err)
	}
	h.reloadBanner(h.ctx)
	return h.answer(st, "confirmed"), nil
}

func (h *Hostd) revertNetwork(w http.ResponseWriter, r *http.Request) {
	h.netMu.Lock()
	defer h.netMu.Unlock()
	st, err := h.netFiles().read()
	if err != nil {
		replyErr(w, err)
		return
	}
	if st.Pending == nil {
		replyErr(w, fail(http.StatusConflict, "no network change awaits confirmation: `%s network set` or `%s network reset` changes it", h.R.Ctl(), h.R.Ctl()))
		return
	}
	h.netDisarm()
	if err := h.undo(1, "asked", h.revertWindow(st.Pending.ID)); err != nil {
		replyErr(w, fmt.Errorf("%w (tried again every %d s)", err, revertRetrySecs))
		return
	}
	st, _ = h.netFiles().read()
	reply(w, http.StatusOK, h.answer(st, "reverted"))
}

func (h *Hostd) resetNetwork(w http.ResponseWriter, r *http.Request) {
	var req NetResetRequest
	if err := decodeBody(r, &req); err != nil {
		replyErr(w, err)
		return
	}
	secs, err := revertSecs(req.RevertSecs)
	if err != nil {
		replyErr(w, fail(http.StatusBadRequest, "%v", err))
		return
	}
	h.netMu.Lock()
	defer h.netMu.Unlock()
	if err := h.noReboot(); err != nil {
		replyErr(w, err)
		return
	}
	cur, err := h.netFiles().read()
	if bad := (unreadable{}); errors.As(err, &bad) {
		// A record that does not read sets nothing at boot already: reset drops it, with no
		// window, as there is nothing to go back to.
		if err := h.dropUnreadable(bad.err); err != nil {
			replyErr(w, err)
			return
		}
		reply(w, http.StatusOK, h.answer(netState{}, fmt.Sprintf("the override did not read (%v): dropped, the cloud-config's settings apply", bad.err)))
		return
	}
	if err := h.noPending(cur); err != nil {
		replyErr(w, err)
		return
	}
	if cur.Config == nil {
		reply(w, http.StatusOK, h.answer(cur, "no override: the cloud-config's settings apply"))
		return
	}
	st, err := h.netSet(cur.Config, nil, secs, true)
	if err != nil {
		replyErr(w, err)
		return
	}
	msg := "back to the cloud-config's settings"
	if st.Pending != nil {
		msg += fmt.Sprintf(": `%s network confirm` keeps them within %d s, else the override comes back", h.R.Ctl(), secs)
	}
	reply(w, http.StatusOK, h.answer(st, msg))
}
