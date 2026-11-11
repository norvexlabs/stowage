package install

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/norvexlabs/stowage/internal/kairos"
	"github.com/norvexlabs/stowage/internal/spec"
)

// A Docker host's firewall and routing are its operator's: the stack's network declarations (the
// services' nftables tables, the forwarding between host.interfaces roles, the networkd file of
// each tun device a service opens) are applied there only once the operator consented, with `tune
// --apply`, which records it (GatewayFile). From then on each start as root applies them as the
// VM's does, and a boot unit runs the install's start at every boot (the kernel keeps no table or
// chain across one); `tune --revert` removes them all. What stays the operator's: a NIC's MTU and
// policy routing (its own NIC, configured by its own files: `tune` gives networkd's settings) and
// a transparent role's input accept (an accept in a table of the install's cannot override a drop
// in the host's own input chain).

// GatewayFile records, in a Docker install's directory, the operator's consent (`tune --apply`) to
// the stack's network declarations, and the networkd files they wrote.
const GatewayFile = ".stowage-gateway"

// gatewayRecord is GatewayFile's content.
type gatewayRecord struct {
	// Digest names the install's chain, unit and files (dirDigest of the directory --apply took),
	// so they are found again from the record wherever the directory is now.
	Digest string `json:"digest"`
	// Dir is the install directory the record is of (absolute). At another one (the directory
	// moved or copied) it applies nothing: its consent is that directory's.
	Dir string `json:"dir,omitempty"`
	// Networkd are the networkd files the install wrote, by path.
	Networkd map[string]networkdFile `json:"networkd,omitempty"`
	// Revoked: a revert withdrew the consent, but did not remove all it applied: no start applies
	// anything, and a revert again removes the rest.
	Revoked bool `json:"revoked,omitempty"`
}

// networkdFile is a tun role's networkd file the install wrote.
type networkdFile struct {
	Role string `json:"role"`
	// Pending: recorded before it was written, not yet seen loaded by networkd.
	Pending bool `json:"pending,omitempty"`
}

// gatewayDeclared reports whether the release declares what a Docker host applies on consent: a
// service's nftables table, forwarding between roles, or a tun device.
func (in *Installer) gatewayDeclared() bool {
	return len(in.R.Tables()) > 0 || len(in.gatewayRoles()) > 0 || forwards(in.R.Spec.Host) || len(in.allowRoutes()) > 0 || len(in.heldNames()) > 0
}

// gatewayRoles are the tun roles, whose whole networkd file a Docker host is given.
func (in *Installer) gatewayRoles() []string {
	h := in.R.Spec.Host
	if h == nil {
		return nil
	}
	var roles []string
	for _, role := range sortedKeys(h.Interfaces) {
		if h.Interfaces[role].Kind == spec.InterfaceTUN {
			roles = append(roles, role)
		}
	}
	return roles
}

// nicSettings are the NIC roles that set an MTU or policy routing: the operator's on a Docker host.
func (in *Installer) nicSettings() []string {
	h := in.R.Spec.Host
	if h == nil {
		return nil
	}
	var roles []string
	for _, role := range sortedKeys(h.Interfaces) {
		i := h.Interfaces[role]
		if i.Kind != spec.InterfaceTUN && (i.MTU != 0 || len(i.RoutingPolicyRules)+len(i.Routes) > 0) {
			roles = append(roles, role)
		}
	}
	return roles
}

// gatewayConsented reports whether the operator of this Docker host consented (GatewayFile) and
// did not revoke it since. A record that does not read is no consent: nothing is applied from it
// (a start and status say so; a revert refuses until it is fixed or removed).
func (in *Installer) gatewayConsented() bool {
	if !in.gatewayRecorded() {
		return false
	}
	rec, err := in.readGateway()
	return err == nil && !rec.Revoked && in.gatewayHere(rec)
}

// gatewayHere reports whether rec is this directory's: recorded for it, by file identity (a link
// to it, or another spelling of it, is it). A record naming no directory is no directory's.
func (in *Installer) gatewayHere(rec gatewayRecord) bool {
	if rec.Dir == "" {
		return false
	}
	a, err := os.Stat(rec.Dir)
	if err != nil {
		return false
	}
	b, err := os.Stat(in.gatewayDir())
	return err == nil && os.SameFile(a, b)
}

// gatewayOtherDir reports whether this install keeps a record that is another directory's (or
// names none): nothing was applied from it here.
func (in *Installer) gatewayOtherDir() (gatewayRecord, bool) {
	if !in.gatewayRecorded() {
		return gatewayRecord{}, false
	}
	rec, err := in.readGateway()
	return rec, err == nil && !in.gatewayHere(rec)
}

// gatewayMoved reports whether rec is of a directory that is gone: this install moved from it,
// the one left to revert what it applied.
func gatewayMoved(rec gatewayRecord) bool {
	if rec.Dir == "" {
		return false
	}
	_, err := os.Stat(rec.Dir)
	return errors.Is(err, os.ErrNotExist)
}

// gatewayRevertsHere reports whether a revert here removes what rec applied: rec is this
// directory's, or of one gone (this install moved).
func (in *Installer) gatewayRevertsHere(rec gatewayRecord) bool {
	return in.gatewayHere(rec) || gatewayMoved(rec)
}

// gatewayElsewhere is what a record of another directory says: applied nothing here.
func (in *Installer) gatewayElsewhere(rec gatewayRecord) string {
	ctl := in.R.Ctl()
	switch {
	case rec.Dir == "":
		return fmt.Sprintf("this install's %s names no directory: nothing of the stack's network declarations is applied from it; remove it, then `%s tune --apply` here", GatewayFile, ctl)
	case gatewayMoved(rec):
		return fmt.Sprintf("this install's %s is %s's, which is gone (the directory moved): nothing of the stack's network declarations is applied here until `%s tune --revert` here removes what it applied, then `%s tune --apply` here", GatewayFile, rec.Dir, ctl, ctl)
	}
	return fmt.Sprintf("this install's %s is %s's (the directory moved or copied): nothing of the stack's network declarations is applied here; `%s tune --revert --dir %s` removes what it applied, from there, or remove the file and `%s tune --apply` here", GatewayFile, rec.Dir, in.R.Ctl(), rec.Dir, in.R.Ctl())
}

// gatewayUnitName is the boot unit of the install whose names digest gives.
func (in *Installer) gatewayUnitName(digest string) string {
	return in.R.Spec.Name + "-gateway-" + digest + ".service"
}

// gatewayTablesUnitName is the tables' boot unit of the install whose names digest gives.
func (in *Installer) gatewayTablesUnitName(digest string) string {
	return in.R.Spec.Name + "-gateway-" + digest + "-tables.service"
}

// gatewayRecorded reports whether this Docker install keeps a gateway record: consented, or revoked
// with something left to remove (a revert takes it from there).
func (in *Installer) gatewayRecorded() bool {
	return in.O.Firewall == "" && fileExists(in.path(GatewayFile))
}

// gatewayDir is the install directory's real path: absolute, its links resolved, so the record
// and the boot unit name it as it is, whatever spelling of it a run was given.
func (in *Installer) gatewayDir() string {
	dir, err := filepath.Abs(in.O.Dir)
	if err != nil {
		return in.O.Dir
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return real
	}
	return dir
}

// digestOf names the install's chain, unit and networkd files: rec's digest, else the directory's.
func (in *Installer) digestOf(rec gatewayRecord) string {
	return cmp.Or(rec.Digest, dirDigest(in.gatewayDir()))
}

// gatewayDigest names the install's chain, unit and networkd files: the record's, else the
// directory's (dirDigest).
func (in *Installer) gatewayDigest() string {
	rec, _ := in.readGateway()
	return in.digestOf(rec)
}

// gatewayApplies reports whether this run applies the network declarations: always on the VM; on
// a Docker host once consented, as root.
func (in *Installer) gatewayApplies() bool {
	return in.O.Firewall != "" || in.gatewayConsented() && in.O.euid() == 0
}

// forwardChain is the iptables chain DOCKER-USER jumps to for the install's forwarding: the VM's
// one name (it holds one install), a Docker host's of the install directory's digest.
func (in *Installer) forwardChain() string {
	if in.O.Firewall != "" {
		return ForwardChain
	}
	return "STOWAGE-FWD-" + in.gatewayDigest()
}

// forwardRecord is where forwardRoles records the rules it gave the chain: under /run, which a
// boot empties as it does iptables's rules.
func (in *Installer) forwardRecord() string {
	if in.O.Firewall != "" {
		return filepath.Join(in.O.Firewall, in.R.Spec.Name+"-forward.rules")
	}
	return filepath.Join(in.O.gatewayRun(), in.R.Spec.Name+"-gateway-"+in.gatewayDigest()+"-forward.rules")
}

// allowChain is the iptables chain DOCKER-USER jumps to for a Docker host's L4 source allowlists
// (allowRules), named after the install directory's digest as its forwarding chain is.
func (in *Installer) allowChain() string { return "STOWAGE-ALW-" + in.gatewayDigest() }

// allowRecord is where allowSources records the rules it gave allowChain, beside forwardRecord.
func (in *Installer) allowRecord() string {
	return filepath.Join(in.O.gatewayRun(), in.R.Spec.Name+"-gateway-"+in.gatewayDigest()+"-allow.rules")
}

// allowHeld marks the allowlists held: written by a start as root once it applied them, saw
// Docker's FORWARD jump to DOCKER-USER and brought the stack up (never by a boot, before Docker),
// removed as any apply begins, holding the digest of the rules it applied (heldMark). status and the L4 lines say a
// list holds only while it is the one the release and the recorded values give (allowHolds).
func (in *Installer) allowHeld() string {
	return filepath.Join(in.O.gatewayRun(), in.R.Spec.Name+"-gateway-"+in.gatewayDigest()+"-allow.held")
}

// markAllowHeld writes the mark of the lists this start applied (allowHeld), once the stack is up:
// its ports are then published as the lists need, on IPv4 alone.
func (in *Installer) markAllowHeld() error {
	if in.allowApplied == "" {
		return nil
	}
	return writeAtomic(in.allowHeld(), strings.NewReader(in.allowApplied), 0o644, nil)
}

// heldMark is allowHeld's content for rules: their digest.
func heldMark(rules map[string][]byte) string {
	return fmt.Sprintf("sha256:%x\n", sha256.Sum256(chainRecord(rules)))
}

// allowHolds reports whether the allowlists the release and get give are those a start as root
// applied and saw hold (allowHeld): a start not as root, which applies nothing, after an upgrade
// or a change of values leaves another mark.
func (in *Installer) allowHolds(get func(string) string) bool {
	rules, err := in.heldRules(in.allowChain(), get)
	if err != nil || rules == nil {
		return false
	}
	mark, err := os.ReadFile(in.allowHeld())
	return err == nil && string(mark) == heldMark(rules)
}

// allowRoutes are the names of the L4 routes with a source allowlist (allow_input), which a
// consented Docker host enforces (allowRules).
func (in *Installer) allowRoutes() []string {
	var names []string
	for _, r := range in.allowed() {
		names = append(names, r.Name)
	}
	return names
}

// allowed are the L4 routes a Docker host's allowlists hold: those with an allow_input whose
// upstream Docker publishes (one on the host's network listens itself, untranslated: its list is
// the host's input chain's, the operator's), but those the front terminates TLS on, whose sources
// the front filters.
func (in *Installer) allowed() []spec.L4Route {
	var out []spec.L4Route
	onHost := in.hostServices()
	for _, r := range in.R.Spec.L4.Routes {
		if name, _, _ := strings.Cut(r.Upstream, ":"); r.AllowInput != "" && !r.Terminated() && !slices.Contains(onHost, name) {
			out = append(out, r)
		}
	}
	return out
}

// frontPorts are the front's two host ports, as recorded (get), and an error naming a value that is
// no TCP port; such a value is the spec's port (a boot before the start: values that do not read),
// which is the front's unless the install moved it (--http-port, --https-port).
func (in *Installer) frontPorts(get func(string) string) ([]int, error) {
	f := in.R.Spec.Front
	var ports []int
	var errs []error
	for _, p := range []struct {
		key  string
		spec int
	}{{HTTPPortVar, cmp.Or(f.HTTPPort, 80)}, {HTTPSPortVar, cmp.Or(f.HTTPSPort, 443)}} {
		v := get(p.key)
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			errs = append(errs, fmt.Errorf("%s %q is not a TCP port", p.key, v))
			n = p.spec
		}
		ports = append(ports, n)
	}
	return ports, errors.Join(errs...)
}

// interfaced are the L4 routes a Docker host holds to their interfaces (l4.routes[].interfaces):
// those Docker publishes, by the route's upstream or, for one the front terminates TLS on, by the
// front.
func (in *Installer) interfaced() []spec.L4Route {
	var out []spec.L4Route
	onHost := in.hostServices()
	for _, r := range in.R.Spec.L4.Routes {
		if name, _, _ := strings.Cut(r.Upstream, ":"); len(r.Interfaces) > 0 && (r.Terminated() || !slices.Contains(onHost, name)) {
			out = append(out, r)
		}
	}
	return out
}

// frontHeld reports whether a Docker host holds the front's ports to its interfaces
// (front.interfaces).
func (in *Installer) frontHeld() bool {
	return in.R.Spec.Front.Declared() && len(in.R.Spec.Front.Interfaces) > 0
}

// heldNames are what a consented Docker host holds to interfaces: "front", then the L4 routes'
// names (interfaced).
func (in *Installer) heldNames() []string {
	var names []string
	if in.frontHeld() {
		names = append(names, "front")
	}
	for _, r := range in.interfaced() {
		names = append(names, r.Name)
	}
	return names
}

// heldNamesIn are heldNames as the recorded values (get) hold them (held): the front, and an L4
// route the front terminates TLS on, are on the loopback address alone in loopback mode, held
// nowhere.
func (in *Installer) heldNamesIn(get func(string) string) []string {
	var names []string
	for _, h := range in.held(get) {
		if !slices.Contains(names, h.name) {
			names = append(names, h.name)
		}
	}
	return names
}

// portHold is a published port a consented Docker host holds in its allow chain: to the sources
// an input holds (allow, an L4 route's allowlist) and to the devices of host.interfaces roles
// (roles), each when given.
type portHold struct {
	name, protocol string
	service        string // the service whose published port it is
	port           int
	bind           string // the address it is published on alone; "" every address
	allow          string
	roles          []string
	v6             bool // held for IPv6 too: the front with the stack's IPv6 on
}

// holds are the ports allowRules holds on a Docker host: each route's of allowed and interfaced,
// and the front's two, while it is held (frontHeld) and not on the loopback address.
func (in *Installer) holds(get func(string) string) []portHold {
	var out []portHold
	if in.frontHeld() && get("STOWAGE_TLS_MODE") != Loopback {
		ports, _ := in.frontPorts(get)
		for _, n := range ports {
			out = append(out, portHold{name: "front", service: FrontService, protocol: "tcp", port: n, roles: in.R.Spec.Front.Interfaces, v6: get(IPv6Var) == "true"})
		}
	}
	allow, ifaced := in.allowed(), in.interfaced()
	for _, r := range in.R.Spec.L4.Routes {
		upstream, _, _ := strings.Cut(r.Upstream, ":")
		if r.Terminated() {
			upstream = FrontService // the front's port
		}
		h := portHold{name: r.Name, service: upstream, protocol: r.Protocol, port: r.Port, bind: bindAddr(r, get)}
		if slices.ContainsFunc(allow, func(a spec.L4Route) bool { return a.Name == r.Name }) {
			h.allow = r.AllowInput
		}
		if slices.ContainsFunc(ifaced, func(a spec.L4Route) bool { return a.Name == r.Name }) {
			if r.Terminated() && get("STOWAGE_TLS_MODE") == Loopback {
				continue // on the loopback address alone
			}
			h.roles = r.Interfaces
		}
		if h.allow != "" || len(h.roles) > 0 {
			out = append(out, h)
		}
	}
	return out
}

// allowV4Only are the L4 routes a consented Docker host publishes on IPv4 alone: those with an
// allowlist or interfaces, whose IPv6 clients Docker's userland proxy may carry past DOCKER-USER,
// unfiltered (a stack without IPv6, an upstream on a network without it, an engine with ip6tables
// off).
func (in *Installer) allowV4Only() []string {
	if in.O.Firewall != "" || !in.gatewayConsented() {
		return nil
	}
	names := in.allowRoutes()
	for _, r := range in.interfaced() {
		if !slices.Contains(names, r.Name) {
			names = append(names, r.Name)
		}
	}
	return names
}

// frontV4Only reports whether a consented Docker host publishes the front's ports on IPv4 alone:
// held to its interfaces while the stack's IPv6 is off, whose IPv6 clients Docker's userland proxy
// would carry past DOCKER-USER (with it on, Docker translates them, and the hold is IPv6's too).
func (in *Installer) frontV4Only(get func(string) string) bool {
	return in.O.Firewall == "" && in.gatewayConsented() && in.frontHeld() && get(IPv6Var) != "true"
}

// holdRules are chain's rules for the ports holds names (a Docker host's L4 allowlists and interface
// holds; the original destination is the published port Docker translates), each held to its
// sources (an empty list drops every one) and to its roles' devices (dev): a connection to its
// published port from one of those sources (any, without an allowlist) arriving on one of those
// devices (any interface, without roles) returns to DOCKER-USER, and any other is dropped — for a
// hold with roles, of the connections Docker forwards to a container bridge (br-*, docker0). A role
// no device plays (dev "") adds none: fail closed. The rules are iptables's, and ip6tables's too
// where a hold is IPv6's (portHold.v6) — that one without sources. Nil when nothing is held.
func holdRules(holds []portHold, chain string, get func(string) string, dev func(string) string) (map[string][]byte, error) {
	var v4, v6 []string
	for _, h := range holds {
		sources := []string{""}
		if h.allow != "" {
			v := get(h.allow)
			if err := checkSources(v); err != nil {
				return nil, fmt.Errorf("l4 route %s: %s: %w", h.name, h.allow, err)
			}
			sources, _ = familySources(v)
		}
		ifaces := []string{""}
		if len(h.roles) > 0 {
			ifaces = nil // none named: every connection dropped (closed)
			for _, role := range h.roles {
				if d := dev(role); d != "" {
					ifaces = append(ifaces, d)
				}
			}
		}
		base := fmt.Sprintf("-A %s -p %s -m conntrack --ctstate DNAT --ctdir ORIGINAL --ctorigdstport %d", chain, h.protocol, h.port)
		// Bound to one address: another install's port on another address is not this one's. An
		// IPv6 one is applied by no start (allowBound refuses it).
		if h.bind != "" {
			base += " --ctorigdst " + h.bind
		}
		// Held to roles: what Docker forwards to a container bridge alone (a compose network's,
		// whose bridge lint keeps named br-*, or the default one), so another translation to the
		// port (a cluster's service address) is not judged here.
		outs := []string{""}
		if len(h.roles) > 0 {
			outs = []string{"br-+", "docker0"}
		}
		var rules []string
		for _, o := range outs {
			match := base
			if o != "" {
				match += " -o " + o
			}
			for _, i := range ifaces {
				for _, s := range sources {
					r := match
					if i != "" {
						r += " -i " + i
					}
					if s != "" {
						r += " -s " + s
					}
					rules = append(rules, r+" -j RETURN")
				}
			}
			rules = append(rules, match+" -j DROP")
		}
		v4 = append(v4, rules...)
		if h.v6 {
			v6 = append(v6, rules...)
		}
	}
	if len(v4) == 0 {
		return nil, nil
	}
	table := func(rules []string) []byte {
		return fmt.Appendf(nil, "*filter\n:%s - [0:0]\n%s\nCOMMIT\n", chain, strings.Join(rules, "\n"))
	}
	out := map[string][]byte{"iptables": table(v4)}
	if len(v6) > 0 {
		out["ip6tables"] = table(v6)
	}
	return out, nil
}

// emptiedV6 gives rules an empty ip6tables chain when they hold nothing for IPv6 and the record of
// the last apply gave ip6tables rules (an earlier start held the front for IPv6): applyChain leaves
// a table it is not given alone, which would keep that hold. Every apply of the allow chain goes
// through it, so the record says what ip6tables holds.
func (in *Installer) emptiedV6(rules map[string][]byte) map[string][]byte {
	if rules == nil || rules["ip6tables"] != nil {
		return rules
	}
	rec, err := os.ReadFile(in.allowRecord())
	if err != nil || bytes.Count(rec, []byte("*filter\n")) < 2 {
		return rules
	}
	out := maps.Clone(rules)
	out["ip6tables"] = fmt.Appendf(nil, "*filter\n:%s - [0:0]\nCOMMIT\n", in.allowChain())
	return out
}

// held are the holds (holds, as the recorded values get give them) to interfaces.
func (in *Installer) held(get func(string) string) []portHold {
	var out []portHold
	for _, h := range in.holds(get) {
		if len(h.roles) > 0 {
			out = append(out, h)
		}
	}
	return out
}

// heldServices are the services whose published ports a start holds to interfaces (held): the
// front's, and each interfaced route's upstream's, once each.
func (in *Installer) heldServices(get func(string) string) []string {
	var svcs []string
	for _, h := range in.held(get) {
		if !slices.Contains(svcs, h.service) {
			svcs = append(svcs, h.service)
		}
	}
	return svcs
}

// heldNetworks refuses, before the stack stops or starts anything, a held service (heldServices)
// the holds would not judge, as the stack's compose model m and the host give it: one with an
// address of its own on a NIC (services.<name>.interface), sharing another namespace
// (network_mode, the default bridge's included), or on a network spec.UnheldNetwork refuses (a
// compose network's driver and driver_opts, an external network's as Docker inspects it): the
// holds judge what Docker translates and forwards to a bridge it makes and names itself, so its
// ports would answer past them. Lint refuses what the release itself fixes; this sees the external
// networks. A host daemon's defaults for the networks a stack makes (default-network-opts) are its
// operator's. Nothing to judge where no hold applies (a Docker host not consented, or not as root).
func (in *Installer) heldNetworks(ctx context.Context, m *composeModel, env *EnvFile) error {
	get := func(k string) string { v, _ := env.Get(k); return v }
	svcs := in.heldServices(get)
	if len(svcs) == 0 || !in.gatewayApplies() {
		return nil
	}
	compose := in.R.Spec.Stack.Compose
	refuse := func(svc, what, fix string) error {
		where := "service " + svc
		if svc == FrontService {
			where = "the front (" + svc + ")"
		}
		return fmt.Errorf("%s %s: its ports held to interfaces are judged on what Docker translates and forwards to a bridge it makes itself, so they would answer past the hold; %s", where, what, fix)
	}
	external := map[string]string{} // by name: `docker network inspect` of it
	for _, svc := range svcs {
		if in.R.Spec.Services[svc].Interface != "" {
			return refuse(svc, "has an address of its own on "+in.R.Spec.Services[svc].Interface, "leave out its services."+svc+".interface, or the route's interfaces")
		}
		s, ok := m.Services[svc]
		if !ok {
			continue // a profile turned off
		}
		if s.NetworkMode != "" {
			return refuse(svc, "has network_mode "+s.NetworkMode, "in "+compose+", leave its network_mode out: it joins the project's networks")
		}
		for _, key := range slices.Sorted(maps.Keys(s.Networks)) {
			n := m.Networks[key]
			driver, opts := n.Driver, n.DriverOpts
			fix := "in " + compose + ", leave networks." + key + "'s driver and driver_opts to Docker (the neutral ones aside)"
			if n.External {
				name := cmp.Or(n.Name, key)
				out, seen := external[name]
				if !seen {
					raw, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"network", "inspect", "--format", "{{.Driver}}|{{json .Options}}", name}})
					if err != nil {
						return fmt.Errorf("service %s's external network %s (%s): %w", svc, key, compose, err)
					}
					out = strings.TrimSpace(string(raw))
					external[name] = out
				}
				d, o, _ := strings.Cut(out, "|")
				opts = nil
				if err := json.Unmarshal([]byte(o), &opts); err != nil {
					return fmt.Errorf("service %s's external network %s: `docker network inspect` gave %q, no driver and options: %w", svc, name, out, err)
				}
				driver = d
				fix = "make the external network " + name + " again with Docker's own bridge (`docker network create " + name + "`, no -d and no -o but the neutral ones)"
			}
			if why := spec.UnheldNetwork(driver, opts); why != "" {
				return refuse(svc, "is on network "+key+", which "+why, fix)
			}
		}
	}
	return nil
}

// unheldRoles are the roles of the ports held to interfaces (holds) that no NIC of this Docker host
// plays (no --interface ROLE=NIC), sorted: those ports answer no client through them (closed).
func (in *Installer) unheldRoles(get func(string) string) []string {
	var roles []string
	for _, h := range in.holds(get) {
		for _, r := range h.roles {
			if in.roleDevice(r, get) == "" && !slices.Contains(roles, r) {
				roles = append(roles, r)
			}
		}
	}
	slices.Sort(roles)
	return roles
}

// heldRules are the allow chain's rules on this Docker host (holdRules of holds), each role by its
// device here.
func (in *Installer) heldRules(chain string, get func(string) string) (map[string][]byte, error) {
	return holdRules(in.holds(get), chain, get, func(role string) string { return in.roleDevice(role, get) })
}

// allowSources gives allowChain the L4 routes' allowlists (allowRules), as applyChain does.
func (in *Installer) allowSources(ctx context.Context, get func(string) string, boot bool) error {
	chain, what := in.allowChain(), "the L4 routes' source allowlists"
	if err := os.Remove(in.allowHeld()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: %w", what, err)
	}
	err := in.allowBound(get)
	var rules map[string][]byte
	if err == nil {
		rules, err = in.heldRules(chain, get)
	}
	if err != nil {
		// Fail closed: the routes' ports, on their IPv4 bind addresses, drop every source until
		// the start that fixes it.
		// Every list empty and no bind address, which checkSources takes (it cannot fail): each
		// route's port drops every source on every address, whatever it is published on now; the
		// ports held to interfaces keep their holds.
		lists := map[string]bool{}
		for _, r := range in.allowed() {
			lists[r.AllowInput], lists[r.BindInput] = true, true
		}
		// An interfaced route keeps its bind address but an IPv6 one, which IPv4's rules cannot hold.
		for _, r := range in.interfaced() {
			if v6Bound(bindAddr(r, get)) {
				lists[r.BindInput] = true
			}
		}
		closed, _ := in.heldRules(chain, func(k string) string {
			if lists[k] {
				return ""
			}
			return get(k)
		})
		return errors.Join(err, in.applyChain(ctx, chain, in.allowRecord(), in.emptiedV6(closed), what, boot))
	}
	for _, r := range in.unheldRoles(get) {
		in.O.logf("warning: host.interfaces.%s: no NIC of this host plays it (`--interface %s=<NIC>` on a re-run names one): the ports held to it answer no client through it", r, r)
	}
	held6 := rules["ip6tables"] != nil
	mark := heldMark(rules) // of what the release holds, which allowHolds recomputes
	rules = in.emptiedV6(rules)
	// A boot runs before Docker, whose FORWARD jump is not there yet: the start after it says
	// whether the lists hold.
	if rules != nil && !boot {
		if err := in.dockerUserReached(ctx, what); err != nil {
			return err
		}
		if _, v6 := os.Stat(ipv6Proc); held6 && v6 == nil {
			if _, err := in.O.host().Run(ctx, Cmd{Args: []string{"ip6tables", "-w", "-C", "FORWARD", "-j", "DOCKER-USER"}}); err != nil {
				// IPv4's holds are applied all the same: the start fails, nothing left open that
				// IPv4 holds.
				v4 := in.emptiedV6(map[string][]byte{"iptables": rules["iptables"]})
				return errors.Join(fmt.Errorf("%s cannot hold for IPv6: ip6tables' FORWARD does not jump to DOCKER-USER (%v), so the Docker engine (its ip6tables off) passes the front's IPv6 clients through its userland proxy: turn its ip6tables on, or `%s tune --revert`", what, err, in.R.Ctl()),
					in.applyChain(ctx, chain, in.allowRecord(), v4, what, boot))
			}
		}
	}
	if err := in.applyChain(ctx, chain, in.allowRecord(), rules, what, boot); err != nil || rules == nil || boot {
		return err
	}
	in.allowApplied = mark
	return nil
}

// dockerUserReached refuses an engine that sends nothing through DOCKER-USER (its iptables off, its
// nftables backend): iptables' FORWARD does not jump to it, so no chain under it holds.
func (in *Installer) dockerUserReached(ctx context.Context, what string) error {
	if _, err := in.O.host().Run(ctx, Cmd{Args: []string{"iptables", "-w", "-C", "FORWARD", "-j", "DOCKER-USER"}}); err != nil {
		return fmt.Errorf("%s cannot hold: iptables' FORWARD does not jump to DOCKER-USER (%v), so the Docker engine (its iptables off, or its nftables backend) passes nothing through it: turn its iptables on, or `%s tune --revert`", what, err, in.R.Ctl())
	}
	return nil
}

// v6Bound reports whether a bind address is one IPv6 address: not an IPv4 one, nor the
// unspecified address, which is every address (bindAddr).
func v6Bound(a string) bool {
	ip, err := netip.ParseAddr(a)
	return err == nil && !ip.Unmap().Is4() && !ip.IsUnspecified()
}

// allowBound refuses an allowlisted route, or one held to its interfaces, bound to an IPv6
// address: a Docker host holds the list and the interfaces for IPv4 alone (allowV4Only).
func (in *Installer) allowBound(get func(string) string) error {
	for _, r := range in.allowed() {
		if a := bindAddr(r, get); v6Bound(a) {
			return fmt.Errorf("l4 route %s: %s=%s is an IPv6 address, and a Docker host holds %s for IPv4 alone: an IPv4 address in %s, or none", r.Name, r.BindInput, a, r.AllowInput, r.BindInput)
		}
	}
	for _, r := range in.interfaced() {
		if a := bindAddr(r, get); v6Bound(a) {
			return fmt.Errorf("l4 route %s: %s=%s is an IPv6 address, and a Docker host holds its interfaces for IPv4 alone: an IPv4 address in %s, or none", r.Name, r.BindInput, a, r.BindInput)
		}
	}
	return nil
}

// gatewayStart applies, at a Docker host's start, what is not the VM's own firewall's: the
// forwarding's chain, the L4 routes' allowlists' (each tried, their errors joined) and the tun
// roles' networkd files (the tables are loadTables', at every start
// that applies them). Not consented, the release's declarations are said not to be in force;
// consented but not run as root, they are said not to be applied (or, the release declaring none,
// not removed). A NIC role's MTU and policy routing are said to be the operator's.
func (in *Installer) gatewayStart(ctx context.Context, get func(string) string) error {
	if in.O.Firewall != "" {
		return nil
	}
	ctl := in.R.Ctl()
	if roles := in.nicSettings(); len(roles) > 0 {
		in.O.logf("warning: the MTU and policy routing of %s are this host's NICs', its operator's: `%s tune` gives networkd's settings for them", strings.Join(roles, ", "), ctl)
	}
	declared, consented := in.gatewayDeclared(), in.gatewayConsented()
	rec, err := in.readGateway()
	switch {
	case in.gatewayRecorded() && err != nil:
		in.O.logf("warning: %v: nothing of the stack's network declarations is applied (fix it, or remove it and `%s tune --apply` again)", err, ctl)
		return nil
	case in.gatewayRecorded() && !in.gatewayHere(rec):
		in.O.logf("warning: %s", in.gatewayElsewhere(rec))
		return nil
	case rec.Revoked:
		in.O.logf("warning: the consent to the stack's network declarations is withdrawn, but not all it applied is removed: `%s tune --revert` again removes the rest", ctl)
		return nil
	case !declared && !consented:
		return nil
	case !consented:
		in.O.logf("warning: %s are not in force on this host, whose firewall and routing are its operator's: `%s tune` lists them, `%s tune --apply` (as root) applies them, and each start then again", in.gatewayWhat(), ctl, ctl)
		return nil
	case in.O.euid() != 0 && declared:
		in.O.logf("warning: %s are applied by a start as root alone (`%s tune --apply` consented): this start applies nothing of them", in.gatewayWhat(), ctl)
		return nil
	case in.O.euid() != 0:
		if len(rec.Networkd) > 0 || fileExists(in.forwardRecord()) || fileExists(in.allowRecord()) || fileExists(in.path(TablesFile)) {
			in.O.logf("warning: what an earlier release applied of the stack's network declarations is removed by a start as root alone: this start removes nothing")
		}
		return nil
	}
	// The boot units as this release writes them: a consent an earlier release recorded gets the
	// tables' unit too.
	if _, err := in.writeUnits(ctx, false); err != nil {
		in.O.logf("warning: the boot units of %s are not written (%v): `%s tune --apply` writes them again", in.gatewayWhat(), err, ctl)
	}
	// Each is tried: one that fails never leaves the others unapplied.
	return errors.Join(in.forwardRoles(ctx, get), in.allowSources(ctx, get, false), in.gatewayRouting(ctx))
}

// gatewayWhat names the release's network declarations, for a warning.
func (in *Installer) gatewayWhat() string {
	var what []string
	if tables := in.R.Tables(); len(tables) > 0 {
		what = append(what, "the services' nftables tables ("+strings.Join(tables, ", ")+")")
	}
	if forwards(in.R.Spec.Host) {
		what = append(what, "the forwarding between host.interfaces")
	}
	if roles := in.gatewayRoles(); len(roles) > 0 {
		what = append(what, "the networkd files of "+strings.Join(roles, ", "))
	}
	if routes := in.allowRoutes(); len(routes) > 0 {
		what = append(what, "the source allowlists of the L4 routes "+strings.Join(routes, ", "))
	}
	if held := in.heldNames(); len(held) > 0 {
		what = append(what, "the interfaces of "+strings.Join(held, ", "))
	}
	if len(what) == 0 {
		return "the stack's network declarations"
	}
	return "the stack's network declarations, " + strings.Join(what, ", ")
}

// readGateway reads GatewayFile; an empty record when there is none. A record naming another path
// than the install's own networkd files (ownRecord) is refused: root removes what it lists.
func (in *Installer) readGateway() (gatewayRecord, error) {
	var r gatewayRecord
	raw, err := os.ReadFile(in.path(GatewayFile))
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return gatewayRecord{}, fmt.Errorf("%s: %w", in.path(GatewayFile), err)
	}
	if r.Digest != "" && !hexDigest.MatchString(r.Digest) {
		return gatewayRecord{}, fmt.Errorf("%s: digest %q is not 8 hex digits", in.path(GatewayFile), r.Digest)
	}
	for _, p := range slices.Sorted(maps.Keys(r.Networkd)) {
		role := r.Networkd[p].Role
		if r.Digest == "" || !spec.InterfaceName.MatchString(role) || p != filepath.Join(in.O.networkdDir(), tunFile(in.R.Spec.Name, r.Digest, role)) {
			return gatewayRecord{}, fmt.Errorf("%s records %s, which is not a networkd file of the install's: refused", in.path(GatewayFile), p)
		}
	}
	return r, nil
}

func (in *Installer) writeGateway(r gatewayRecord) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	// Readable by all, as nothing in it is secret: a start not as root reads the consent too.
	return writeAtomic(in.path(GatewayFile), bytes.NewReader(append(raw, '\n')), 0o644, nil)
}

// gatewayTunFile is the name of a tun role's networkd file, holding the install's digest, so two
// installs never share one.
func (in *Installer) gatewayTunFile(role string) string {
	return tunFile(in.R.Spec.Name, in.gatewayDigest(), role)
}

func tunFile(name, digest, role string) string {
	return "10-" + name + "-" + digest + "-" + role + ".network"
}

// hexDigest is a dirDigest.
var hexDigest = regexp.MustCompile(`^[0-9a-f]{8}$`)

// gatewayRouting writes each tun role's whole networkd file (kairos.InterfaceNetwork, the VM's
// own), which matches the device by name once its service opens it, and removes a recorded one
// whose role the release no longer declares. Every file is in the record, pending, before it is
// written; networkd reloads (which applies a changed file to its link), and the record then holds
// the files wanted, none pending: a start cut off in between is applied again by the next. A host
// where networkd does not run is warned and changed in nothing.
func (in *Installer) gatewayRouting(ctx context.Context) error {
	rec, err := in.readGateway()
	if err != nil {
		in.O.logf("warning: %v: the networkd files are left as they are (fix or remove it)", err)
		return nil
	}
	roles := in.gatewayRoles()
	digest := in.digestOf(rec)
	want, content := map[string]networkdFile{}, map[string]string{}
	for _, role := range roles {
		p := filepath.Join(in.O.networkdDir(), tunFile(in.R.Spec.Name, digest, role))
		want[p], content[p] = networkdFile{Role: role}, kairos.InterfaceNetwork(in.R.Spec.Name, role, in.R.Spec.Host.Interfaces[role])
	}
	var changed []string
	for _, p := range slices.Sorted(maps.Keys(content)) {
		if cur, err := os.ReadFile(p); err != nil || string(cur) != content[p] {
			changed = append(changed, p)
		}
	}
	// A pending entry differs from the one wanted: it is applied again.
	if len(changed) == 0 && maps.Equal(want, rec.Networkd) {
		return nil
	}
	host := in.O.host()
	if !networkdRuns(ctx, host) {
		if len(roles) > 0 {
			in.O.logf("warning: systemd-networkd does not run on this host: the networkd files of %s are not written or reloaded (`%s tune` gives their settings)", strings.Join(roles, ", "), in.R.Ctl())
		} else {
			in.O.logf("warning: systemd-networkd does not run on this host: the networkd files an earlier release wrote (%s) stay until a start as root finds it running", strings.Join(slices.Sorted(maps.Keys(rec.Networkd)), ", "))
		}
		return nil
	}
	held := maps.Clone(rec.Networkd)
	if held == nil {
		held = map[string]networkdFile{}
	}
	for _, p := range changed {
		held[p] = networkdFile{Role: want[p].Role, Pending: true}
	}
	if err := in.writeGateway(gatewayRecord{Digest: digest, Dir: rec.Dir, Networkd: held}); err != nil {
		return err
	}
	for _, p := range slices.Sorted(maps.Keys(held)) {
		if _, ok := want[p]; !ok {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	for _, p := range changed {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := writeAtomic(p, strings.NewReader(content[p]), 0o644, nil); err != nil {
			return err
		}
	}
	if _, err := host.Run(ctx, Cmd{Args: []string{"networkctl", "reload"}}); err != nil {
		return fmt.Errorf("networkctl reload: %w", err)
	}
	return in.writeGateway(gatewayRecord{Digest: digest, Dir: rec.Dir, Networkd: want})
}

// networkdRuns reports whether systemd-networkd runs on the host.
func networkdRuns(ctx context.Context, host Tool) bool {
	_, err := host.Run(ctx, Cmd{Args: []string{"systemctl", "is-active", "--quiet", "systemd-networkd"}})
	return err == nil
}

// gatewayUnit is the boot unit of a consented Docker install: the install's start at every boot,
// after Docker and the network, which loads its tables and forwarding again and recreates the
// services whose table it loaded (they feed its sets again).
func (in *Installer) gatewayUnit() (name, content string) {
	dir := in.gatewayDir()
	name = in.gatewayUnitName(in.gatewayDigest())
	ctl := filepath.Join(dir, in.R.Ctl())
	return name, fmt.Sprintf("[Unit]\nDescription=%s (%s): the stack's network declarations, applied by its start (tune --apply)\nAfter=docker.service network-online.target\nWants=docker.service network-online.target\n\n[Service]\nType=oneshot\nExecStart=%s --dir %s --no-input --no-provision\nTimeoutStartSec=30min\n\n[Install]\nWantedBy=multi-user.target\n",
		in.R.Spec.Name, systemdText(dir), systemdArg(ctl), systemdArg(dir))
}

// gatewayTablesUnit is the boot unit of a consented Docker install that loads the services' tables
// the last start loaded (tables-boot) before Docker restores their containers, once the network
// is up (a table may name a device by its index).
func (in *Installer) gatewayTablesUnit() (name, content string) {
	dir := in.gatewayDir()
	name = in.gatewayTablesUnitName(in.gatewayDigest())
	ctl := filepath.Join(dir, in.R.Ctl())
	return name, fmt.Sprintf("[Unit]\nDescription=%s (%s): the services' nftables tables and the L4 routes' source allowlists, before Docker restores their containers (tune --apply)\nAfter=network-online.target\nWants=network-online.target\nBefore=docker.service\n\n[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=%s %s --dir %s\n\n[Install]\nWantedBy=multi-user.target\n",
		in.R.Spec.Name, systemdText(dir), systemdArg(ctl), tablesBootCmd, systemdArg(dir))
}

// systemdArg is s as one argument of a unit's ExecStart: double-quoted, its backslashes and quotes
// escaped, its % (systemd's specifiers) and $ (its variables, expanded in quotes too) doubled, so
// a path with a space, a % or a $ stays the one argument it is.
func systemdArg(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(s)
	// A control character (a newline would end the line and start a directive) as a C escape,
	// which systemd reads in a quoted argument.
	// So is a byte outside UTF-8, kept as the byte it is.
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r < 0x20 || r == 0x7f || r == utf8.RuneError && n == 1 {
			fmt.Fprintf(&b, `\x%02x`, s[i])
		} else {
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return `"` + b.String() + `"`
}

// systemdText is s as a unit's free text (Description=): its % doubled, a control character a ?
// (no escape is read there, and a newline would end the line).
func systemdText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, strings.ReplaceAll(s, "%", "%%"))
}

// gatewayApply records the operator's consent, enables the boot unit, and runs the install's
// start, which applies the declarations (tune --apply on a Docker host).
func (in *Installer) gatewayApply(ctx context.Context) error {
	// Under the install's lock: a start running meanwhile would write the record it read. The
	// start this runs takes the lock again, waiting for one that took it meanwhile.
	unlock, err := lockFile(in.path(lifecycleLock))
	if errors.Is(err, errLocked) {
		return errors.New("a start, upgrade, rollback, provision or reset-db of this install runs: wait for it to finish, then tune --apply again")
	}
	if err != nil {
		return err
	}
	err = in.consent(ctx)
	unlock()
	if err != nil {
		return err
	}
	in.O.logf("applying %s: the install's start", in.gatewayWhat())
	// The consent changed the host already: whatever the start exits with, this run failed.
	return childFailed(in.ctl(), in.O.exec(ctx, in.ctl(), []string{"--dir", in.gatewayDir(), "--no-input", "--no-provision"}))
}

// consent enables the boot unit, then records the consent. A unit that fails leaves the record
// and the unit as they were (a unit this run wrote is removed again). A record of another
// directory is refused: its consent is that directory's.
func (in *Installer) consent(ctx context.Context) error {
	prev, err := in.readGateway()
	if err != nil {
		return fmt.Errorf("%w: fix or remove it, then tune --apply again", err)
	}
	if in.gatewayRecorded() && !in.gatewayHere(prev) {
		return errors.New(in.gatewayElsewhere(prev))
	}
	// A route bound to an IPv6 address holds no list here (allowBound): refused before consent.
	get, err := in.installedEnv()
	if err != nil {
		return fmt.Errorf("the install's record: %w: fix it, then tune --apply again", err)
	}
	if err := in.allowBound(get); err != nil {
		return fmt.Errorf("%w (a re-run sets it), then tune --apply again", err)
	}
	recorded := in.gatewayRecorded()
	rec := prev
	rec.Digest = in.digestOf(prev)
	rec.Revoked, rec.Dir = false, cmp.Or(prev.Dir, in.gatewayDir())
	undo, err := in.writeUnits(ctx, true)
	if err != nil {
		return err
	}
	if err := in.writeGateway(rec); err != nil {
		if recorded {
			return err // the record as it was, the units enabled for it: a revert reaches both
		}
		undo()
		return err
	}
	return nil
}

// writeUnits writes and enables the consented install's boot units (gatewayUnit,
// gatewayTablesUnit), each written only when its content differs, and enabled when one was written
// or enable says so. One that fails leaves every unit as it was (a unit this run wrote is removed
// again); undo puts them back so after a success.
func (in *Installer) writeUnits(ctx context.Context, enable bool) (undo func(), err error) {
	host := in.O.host()
	type was struct {
		name, path string
		old        []byte
		had        bool
	}
	var done []was
	undo = func() {
		for _, w := range done {
			if w.had {
				_ = writeAtomic(w.path, bytes.NewReader(w.old), 0o644, nil)
			} else {
				_, _ = host.Run(ctx, Cmd{Args: []string{"systemctl", "disable", w.name}})
				_ = os.Remove(w.path)
			}
		}
		_, _ = host.Run(ctx, Cmd{Args: []string{"systemctl", "daemon-reload"}})
	}
	fail := func(err error) (func(), error) {
		undo()
		return nil, err
	}
	var names []string
	reload := false
	for _, unit := range []func() (string, string){in.gatewayUnit, in.gatewayTablesUnit} {
		name, content := unit()
		path := filepath.Join(in.O.systemd(), name)
		old, readErr := os.ReadFile(path)
		w := was{name: name, path: path, old: old, had: readErr == nil}
		done = append(done, w)
		names = append(names, name)
		if w.had && string(old) == content {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fail(err)
		}
		if err := writeAtomic(path, strings.NewReader(content), 0o644, nil); err != nil {
			return fail(err)
		}
		reload = true
	}
	if !reload && !enable {
		return undo, nil
	}
	if reload {
		if _, err := host.Run(ctx, Cmd{Args: []string{"systemctl", "daemon-reload"}}); err != nil {
			return fail(err)
		}
	}
	if _, err := host.Run(ctx, Cmd{Args: append([]string{"systemctl", "enable"}, names...)}); err != nil {
		return fail(err)
	}
	return undo, nil
}

// lockedRevert is gatewayRevert under the install's lock (tune --revert).
func (in *Installer) lockedRevert(ctx context.Context) error {
	unlock, err := lockFile(in.path(lifecycleLock))
	if errors.Is(err, errLocked) {
		return errors.New("a start, upgrade, rollback, provision or reset-db of this install runs: wait for it to finish, then tune --revert again")
	}
	if err != nil {
		return err
	}
	defer unlock()
	if err := in.gatewayRevert(ctx); err != nil {
		return err
	}
	// The consent published them on IPv4 alone (allowV4Only); the next start publishes them on
	// every address, which a revert never starts itself (the stack may be stopped).
	if routes := in.allowRoutes(); len(routes) > 0 && fileExists(in.path(envName)) {
		in.O.logf("note: the L4 routes %s are open to every source now, and stay published on IPv4 alone until the next start, which publishes them on every address: `%s --dir %s --no-input --no-provision`", strings.Join(routes, ", "), in.ctl(), in.gatewayDir())
	}
	if get, err := in.installedEnv(); err == nil && fileExists(in.path(envName)) {
		if held := in.heldNamesIn(get); len(held) > 0 {
			in.O.logf("note: %s answer on every interface now, published as the consent published them (on IPv4 alone, but the front with the stack's IPv6 on) until the next start, which publishes them on every address: `%s --dir %s --no-input --no-provision`", strings.Join(held, ", "), in.ctl(), in.gatewayDir())
		}
	}
	return nil
}

// gatewayRevert removes what the declarations applied (the boot units, the services' tables, the
// forwarding's and the allowlists' chains, the networkd files) and the consent (tune --revert, uninstall on a Docker
// host; the caller holds the install's lock). The record is read first: one that does not read, or
// names another file, refuses before anything goes. Each step is tried; when one failed, the record
// stays, revoked (no start applies anything), and a revert again removes the rest. A service's
// table goes with the consent: the service runs on without it.
func (in *Installer) gatewayRevert(ctx context.Context) error {
	rec, err := in.readGateway()
	if err != nil {
		return fmt.Errorf("%w: fix or remove it, then `%s tune --revert` again", err, in.R.Ctl())
	}
	if !in.gatewayRevertsHere(rec) {
		return errors.New(in.gatewayElsewhere(rec))
	}
	host := in.O.host()
	var errs []error
	reload := false
	for _, name := range []string{in.gatewayUnitName(in.digestOf(rec)), in.gatewayTablesUnitName(in.digestOf(rec))} {
		if unit := filepath.Join(in.O.systemd(), name); fileExists(unit) {
			_, _ = host.Run(ctx, Cmd{Args: []string{"systemctl", "disable", name}})
			if err := os.Remove(unit); err != nil {
				errs = append(errs, err)
			} else {
				reload = true
			}
		}
	}
	if reload {
		if _, err := host.Run(ctx, Cmd{Args: []string{"systemctl", "daemon-reload"}}); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, in.syncTables(ctx, nil, nil), in.dropForward(ctx))
	if raw, err := os.ReadFile(in.path(TablesFile)); err == nil && len(strings.Fields(string(raw))) > 0 {
		errs = append(errs, fmt.Errorf("the nftables tables %s are still loaded", strings.Join(strings.Fields(string(raw)), ", ")))
	}
	for _, p := range slices.Sorted(maps.Keys(rec.Networkd)) {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	// networkd not running reads the files at its start: they are gone.
	if len(rec.Networkd) > 0 && networkdRuns(ctx, host) {
		if _, err := host.Run(ctx, Cmd{Args: []string{"networkctl", "reload"}}); err != nil {
			errs = append(errs, fmt.Errorf("networkctl reload: %w", err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		rec.Revoked = true
		if werr := in.writeGateway(rec); werr != nil {
			return errors.Join(err, werr)
		}
		return fmt.Errorf("the consent is withdrawn, but not all it applied is removed (`%s tune --revert` again removes the rest): %w", in.R.Ctl(), err)
	}
	if err := os.Remove(in.path(GatewayFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	in.O.logf("removed %s, and the consent to them", in.gatewayWhat())
	return nil
}

// dropForward removes the install's chains on a Docker host, the forwarding's and the
// allowlists' (dropChain: DOCKER-USER's jumps to each, its rules, the chain, its records), and the
// allowlists' held mark, each step tried.
func (in *Installer) dropForward(ctx context.Context) error {
	var errs []error
	if err := os.Remove(in.allowHeld()); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, c := range []struct{ chain, record string }{{in.forwardChain(), in.forwardRecord()}, {in.allowChain(), in.allowRecord()}} {
		errs = append(errs, in.dropChain(ctx, c.chain, c.record))
	}
	return errors.Join(errs...)
}

// dropChain removes chain's applied mark, then the chain, DOCKER-USER's jumps to it first, from
// each table that holds it (each tried), then its record once every step went.
func (in *Installer) dropChain(ctx context.Context, chain, record string) error {
	var errs []error
	// The applied mark first: a step failing below leaves no chain said to be applied.
	if err := os.Remove(appliedMark(record)); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	host := in.O.host()
	tables := []string{"iptables", "ip6tables"}
	if _, err := os.Stat(ipv6Proc); err != nil {
		tables = tables[:1]
	}
	for _, t := range tables {
		errs = append(errs, func() error {
			jump := []string{"DOCKER-USER", "-j", chain}
			for range 16 { // each jump inserted once a start; a bound on what deleting takes
				if _, err := host.Run(ctx, Cmd{Args: append([]string{t, "-w", "-C"}, jump...)}); err != nil {
					break
				}
				if _, err := host.Run(ctx, Cmd{Args: append([]string{t, "-w", "-D"}, jump...)}); err != nil {
					return fmt.Errorf("%s: DOCKER-USER's jump to %s: %w", t, chain, err)
				}
			}
			if _, err := host.Run(ctx, Cmd{Args: []string{t, "-w", "-n", "-L", chain}}); err != nil {
				return nil // no such chain
			}
			for _, op := range []string{"-F", "-X"} {
				if _, err := host.Run(ctx, Cmd{Args: []string{t, "-w", op, chain}}); err != nil {
					return fmt.Errorf("%s %s %s: %w", t, op, chain, err)
				}
			}
			return nil
		}())
	}
	// The record stays while a step failed: the next try finds the chain to remove.
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if err := os.Remove(record); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// gatewayStatus describes, for `status` on a Docker host, whether the declarations are applied:
// empty when the release declares none and no consent stands.
func (in *Installer) gatewayStatus(get func(string) string) string {
	recorded := in.gatewayRecorded()
	if in.O.Firewall != "" || !in.gatewayDeclared() && !recorded {
		return ""
	}
	ctl := in.R.Ctl()
	rec, err := in.readGateway()
	switch {
	case !recorded:
		return fmt.Sprintf("not in force: the host's firewall and routing are its operator's (`%s tune --apply` applies %s)", ctl, strings.TrimPrefix(in.gatewayWhat(), "the stack's network declarations, "))
	case err != nil:
		return fmt.Sprintf("not applied: %v (fix it, or remove it and `%s tune --apply` again)", err, ctl)
	case !in.gatewayHere(rec):
		return "not applied: " + in.gatewayElsewhere(rec)
	case rec.Revoked:
		return fmt.Sprintf("revoked, but not all it applied is removed: `%s tune --revert` again removes the rest", ctl)
	}
	var applied, notYet []string
	if loaded := in.loadedTables(context.Background()); len(loaded) > 0 {
		applied = append(applied, "tables "+strings.Join(loaded, ", "))
	}
	switch rules, _ := in.forwardTables(get); {
	case in.forwardApplied(get):
		applied = append(applied, "forwarding")
	case rules == nil && fileExists(in.forwardRecord()):
		notYet = append(notYet, "forwarding left (the next start as root removes it)")
	case rules != nil:
		notYet = append(notYet, "forwarding not applied (no start as root applied it since this release or these devices, or its error says why)")
	}
	if _, unmapped := in.forwardTables(get); len(unmapped) > 0 {
		notYet = append(notYet, "no forwarding through "+strings.Join(unmapped, ", ")+" (no NIC named: --interface)")
	}
	if unheld := in.unheldRoles(get); len(unheld) > 0 {
		notYet = append(notYet, "no client through "+strings.Join(unheld, ", ")+" to the ports held to it (no NIC named: --interface)")
	}
	switch routes, held := strings.Join(in.allowRoutes(), ", "), strings.Join(in.heldNamesIn(get), ", "); {
	case (routes != "" || held != "") && in.allowHolds(get):
		if routes != "" {
			applied = append(applied, "source allowlists of "+routes)
		}
		if held != "" {
			applied = append(applied, "interfaces of "+held)
		}
	case routes != "" || held != "":
		if routes != "" {
			notYet = append(notYet, "source allowlists of "+routes+" not held (no start as root applied them since this release or these values, or its error says why)")
		}
		if held != "" {
			notYet = append(notYet, "interfaces of "+held+" not held (no start as root applied them since this release or these values, or its error says why)")
		}
	case fileExists(in.allowRecord()):
		notYet = append(notYet, "published ports' holds left (the next start as root removes them)")
	}
	n, pending := 0, 0
	for _, f := range rec.Networkd {
		if f.Pending {
			pending++
		} else {
			n++
		}
	}
	if n > 0 {
		applied = append(applied, fmt.Sprintf("%d networkd file(s)", n))
	}
	if pending > 0 {
		notYet = append(notYet, fmt.Sprintf("%d networkd file(s) pending (written, not yet seen loaded by networkd: the next start as root reloads it)", pending))
	}
	if len(applied) == 0 && len(notYet) > 0 {
		return "consented (tune --apply), nothing in force: " + strings.Join(notYet, "; ")
	}
	applied = append(applied, notYet...)
	switch {
	case len(applied) == 0 && !in.gatewayDeclared():
		return "consented (tune --apply): the release declares none (`" + ctl + " tune --revert` withdraws the consent)"
	case len(applied) == 0:
		return "consented (tune --apply): nothing applied yet, the next start as root applies them"
	}
	return "applied at each start (tune --apply): " + strings.Join(applied, "; ")
}

// loadedTables are the services' tables the install recorded loaded (TablesFile); as root, those
// the kernel holds alone (a boot drops them until the next start loads them).
func (in *Installer) loadedTables(ctx context.Context) []string {
	raw, err := os.ReadFile(in.path(TablesFile))
	if err != nil {
		return nil
	}
	names := strings.Fields(string(raw))
	if in.O.euid() != 0 {
		return names
	}
	return slices.DeleteFunc(names, func(n string) bool {
		_, err := in.O.host().Run(ctx, Cmd{Args: []string{"nft", "list", "table", "inet", n}})
		return err != nil
	})
}
