package install

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// peerAddress is the loopback address the VM publishes the nth (from 1) peer of the L4 routes'
// services on — a service one depends on, which it reaches by name there from the host's network:
// 127.0.4.n, one address each, so two peers listening on one port never clash.
func peerAddress(n int) string { return fmt.Sprintf("127.0.4.%d", n) }

// maxPeers bounds the peers, one address of peerNet each.
const maxPeers = 254

// peerNet holds the peers' addresses, which the front's loopback address never takes.
var peerNet = netip.MustParsePrefix("127.0.4.0/24")

// hostNet is how the VM runs the stack's L4 routes' services (targets): on the host's network,
// with no Docker networking between their clients and them. A target reaches each service it
// depends on by name at that peer's own address (peerAddress), where its ports are published, and another target
// at the loopback; every other service reaches a target by name at the host's gateway. Zero on
// Docker, which publishes the routes' ports from the stack's network.
type hostNet struct {
	targets map[string]bool
	// peers are the ports each peer is published on at its address (addrs); reach, the peers each
	// target maps by name; open, the targets' ports the stack's services reach from its bridges.
	peers map[string][]peerPort
	addrs map[string]string
	reach map[string][]string
	open  []peerPort
}

// bridged is the targets' ports the firewall accepts from the stack's bridges, sorted, each once.
func (hn hostNet) bridged() []peerPort {
	return sortPorts(hn.open)
}

// sortPorts sorts ports by port then protocol, each once.
func sortPorts(ports []peerPort) []peerPort {
	var out []peerPort
	for _, p := range ports {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b peerPort) int {
		if a.Port != b.Port {
			return a.Port - b.Port
		}
		return strings.Compare(a.Protocol, b.Protocol)
	})
	return out
}

// peerPort is a port a service listens on, over its protocol.
type peerPort struct {
	Port     int
	Protocol string
}

// give adds to extra, the service name's settings, what hn gives it: the host's network and the
// names it maps for a target, the ports at its address for a peer, and every target's name for a
// service of a network of its own (another's namespace or none maps nothing).
func (hn hostNet) give(extra map[string]any, name, mode string) {
	if len(hn.targets) == 0 {
		return
	}
	var hosts []string
	if hn.targets[name] {
		extra["network_mode"] = "host"
		for _, p := range hn.reach[name] {
			hosts = append(hosts, p+":"+hn.addrs[p])
		}
		for _, t := range sortedKeys(hn.targets) {
			if t != name {
				hosts = append(hosts, t+":127.0.0.1")
			}
		}
	} else if mode == "" || mode == "bridge" {
		for _, t := range sortedKeys(hn.targets) {
			hosts = append(hosts, t+":host-gateway")
		}
	}
	if len(hosts) > 0 {
		extra["extra_hosts"] = hosts
	}
	if ports := hn.peers[name]; len(ports) > 0 && !hn.targets[name] {
		var pub []map[string]any
		for _, p := range ports {
			pub = append(pub, map[string]any{"host_ip": hn.addrs[name], "target": p.Port, "published": strconv.Itoa(p.Port), "protocol": p.Protocol})
		}
		extra["ports"] = pub
	}
}

// targetHosts are the extra hosts a service Stowage runs on demand on the stack's network (the
// provision hook, a command provider) reaches the VM's L4 routes' services by: each at the host's
// gateway (one a profile turned off too: its name then reaches nothing). None on Docker.
func (in *Installer) targetHosts() []string {
	if in.O.Firewall == "" {
		return nil
	}
	var hosts []string
	for _, r := range in.R.Spec.L4.Routes {
		name, _, _ := strings.Cut(r.Upstream, ":")
		if h := name + ":host-gateway"; !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	slices.Sort(hosts)
	return hosts
}

// hostNetwork is the VM's hostNet for the stack m (zero on Docker): the services the L4 routes
// target that m runs (a profile that is off leaves one out), and each one's peers, the services
// of m it depends on with a network namespace of their own, with the ports each exposes (its
// `expose:`, then its image's EXPOSE) but those taken, which the stack or the host holds on every
// address (a peer's can then not be published on its own). A peer exposing none, and one in
// another's namespace, are logged: the target cannot reach it by name.
func (in *Installer) hostNetwork(ctx context.Context, m *composeModel, taken map[peerPort]bool) (hostNet, error) {
	hn := hostNet{targets: map[string]bool{}, peers: map[string][]peerPort{}, addrs: map[string]string{}, reach: map[string][]string{}}
	if in.O.Firewall == "" {
		return hostNet{}, nil
	}
	for _, r := range in.R.Spec.L4.Routes {
		name, _, _ := strings.Cut(r.Upstream, ":")
		if _, ok := m.Services[name]; ok {
			hn.targets[name] = true
			hn.open = append(hn.open, peerPort{r.Port, r.Protocol})
		}
	}
	// The front's routes to a target, and the ports a target exposes, are reached from the stack.
	for _, r := range in.R.Spec.Front.Routes {
		name, port, _ := strings.Cut(r.Upstream, ":")
		if n, err := strconv.Atoi(port); err == nil && hn.targets[name] {
			hn.open = append(hn.open, peerPort{n, "tcp"})
		}
	}
	for _, t := range sortedKeys(hn.targets) {
		ports, err := in.exposed(ctx, m, t)
		if err != nil {
			return hostNet{}, err
		}
		hn.open = append(hn.open, ports...)
	}
	// A port a target listens on, on every address of the host, is no peer's to be published on.
	held := maps.Clone(taken)
	if held == nil {
		held = map[peerPort]bool{}
	}
	for _, q := range hn.open {
		held[q] = true
	}
	var peers []string
	for _, t := range sortedKeys(hn.targets) {
		for _, p := range sortedKeys(m.Services[t].DependsOn) {
			svc, ok := m.Services[p]
			if !ok || hn.targets[p] {
				continue
			}
			if mode := svc.NetworkMode; mode != "" && mode != "bridge" {
				in.O.logf("%s (an L4 route's, on the host's network) depends on %s, whose network is %s: it cannot reach %s by name", t, p, mode, p)
				continue
			}
			if _, done := hn.peers[p]; !done {
				ports, err := in.exposed(ctx, m, p)
				if err != nil {
					return hostNet{}, err
				}
				var free []peerPort
				for _, q := range ports {
					if held[q] {
						in.O.logf("%s's %s/%d is not published for %s: the host holds it on every address", p, q.Protocol, q.Port, t)
						continue
					}
					free = append(free, q)
				}
				hn.peers[p] = free
				if len(free) > 0 {
					peers = append(peers, p)
				}
			}
			if len(hn.peers[p]) == 0 {
				in.O.logf("%s (an L4 route's, on the host's network) depends on %s, which exposes no port it can be published on: it cannot reach %s by name (give %s's ports in its expose:)", t, p, p, p)
				continue
			}
			hn.reach[t] = append(hn.reach[t], p)
		}
	}
	if len(peers) > maxPeers {
		return hostNet{}, fmt.Errorf("the L4 routes' services depend on more than %d services, each given an address of 127.0.4.0/24", maxPeers)
	}
	// Each peer's address follows from its name, the next free one on a clash: a peer added or
	// removed moves another only when their names clash.
	used := map[int]bool{}
	for _, p := range sortedKeys(hn.peers) {
		if !slices.Contains(peers, p) {
			continue
		}
		h := fnv.New32a()
		h.Write([]byte(p))
		n := int(h.Sum32()%maxPeers) + 1
		for used[n] {
			n = n%maxPeers + 1
		}
		used[n] = true
		hn.addrs[p] = peerAddress(n)
	}
	if len(hn.targets) == 0 {
		return hostNet{}, nil
	}
	return hn, nil
}

// exposed is the ports service exposes: its `expose:` entries (a port or a range, over tcp unless
// it names udp; another fails), then its image's EXPOSE (one this cannot take, or an image that
// cannot be read, logged and left out), sorted, each once.
func (in *Installer) exposed(ctx context.Context, m *composeModel, service string) ([]peerPort, error) {
	svc := m.Services[service]
	var ports []peerPort
	for _, s := range svc.Expose {
		p, err := exposedPorts(s)
		if err != nil {
			return nil, fmt.Errorf("services.%s.expose: %w", service, err)
		}
		ports = append(ports, p...)
	}
	if svc.Image != "" {
		// An image gone from the engine is pulled again by the start (up), which this must not
		// fail before.
		var exp map[string]any
		out, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"image", "inspect", "--format", "{{json .Config.ExposedPorts}}", svc.Image}})
		if err == nil {
			err = json.Unmarshal([]byte(strings.TrimSpace(string(out))), &exp)
		}
		if err != nil {
			in.O.logf("the ports %s's image exposes are not read (%v): this start leaves them out", service, err)
		}
		for _, s := range sortedKeys(exp) {
			p, err := exposedPorts(s)
			if err != nil {
				in.O.logf("%s's image exposes %v: left out", service, err)
				continue
			}
			ports = append(ports, p...)
		}
	}
	return sortPorts(ports), nil
}

// maxExposedRange bounds the ports one `expose:` range publishes.
const maxExposedRange = 64

// exposedPorts reads one `expose:` entry or EXPOSE key: `5432`, `5432/udp` or `8000-8010/tcp`.
func exposedPorts(s string) ([]peerPort, error) {
	rng, proto, _ := strings.Cut(s, "/")
	proto = strings.ToLower(proto)
	if proto == "" {
		proto = "tcp"
	}
	if proto != "tcp" && proto != "udp" {
		return nil, fmt.Errorf("%q: tcp or udp", s)
	}
	lo, hi, isRange := strings.Cut(rng, "-")
	a, err := strconv.Atoi(lo)
	b := a
	if err == nil && isRange {
		b, err = strconv.Atoi(hi)
	}
	if err != nil || a < 1 || b > 65535 || b < a {
		return nil, fmt.Errorf("%q is not a port or a range of ports", s)
	}
	if b-a >= maxExposedRange {
		return nil, fmt.Errorf("%q: at most %d ports in a range", s, maxExposedRange)
	}
	var out []peerPort
	for p := a; p <= b; p++ {
		out = append(out, peerPort{p, proto})
	}
	return out, nil
}
