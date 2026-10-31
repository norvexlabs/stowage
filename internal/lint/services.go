package lint

import (
	"fmt"
	"net/netip"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/norvexlabs/stowage/internal/spec"
)

// MaxServiceCPUs is the most whole cores a service is given (services.<name>.cpus).
const MaxServiceCPUs = 64

// serviceHost checks what a service asks of the host beyond its memory: its whole cores (an IRQ
// posture needs them), its nftables table (a file of the project holding its table alone), its
// address of its own on a NIC the release names, from an input, and how a start replaces it.
func (l *linter) serviceHost(at, name string, svc spec.Service) {
	l.serviceUpdate(at, name, svc)
	if svc.CPUs != 0 && (svc.CPUs < 1 || svc.CPUs > MaxServiceCPUs) {
		l.errf(at+".cpus", "%d: whole cores, 1 to %d", svc.CPUs, MaxServiceCPUs)
	}
	switch svc.IRQ {
	case "":
	case spec.IRQOff, spec.IRQOn:
		if svc.CPUs == 0 {
			l.errf(at+".irq", "is about the service's cores: give it cpus")
		}
	default:
		l.errf(at+".irq", "%q: %s (the default: the NICs' interrupts off its cores) or %s", svc.IRQ, spec.IRQOff, spec.IRQOn)
	}
	if f := svc.Nftables; f != "" {
		l.nftTable(at+".nftables", name, f)
	}
	l.serviceNetwork(at, name, svc)
	l.frontHosts(at, name, svc)
	switch {
	case (svc.Interface == "") != (svc.AddressInput == ""):
		l.errf(at, "interface and address_input go together: the NIC its address is on, and the input giving it")
	case svc.Interface != "":
		var nic bool
		if l.s.Host != nil {
			i, ok := l.s.Host.Interfaces[svc.Interface]
			nic = ok && (i.Kind == "" || i.Kind == spec.InterfaceNIC)
		}
		if !nic {
			l.errf(at+".interface", "%q is not a NIC of host.interfaces", svc.Interface)
		}
		in, ok := l.s.Input(svc.AddressInput)
		switch {
		case !ok:
			l.errf(at+".address_input", "%s is not an input", svc.AddressInput)
		case in.Secret:
			l.errf(at+".address_input", "%s is secret: an address is written into the compose file", svc.AddressInput)
		case in.Default != "" && !strings.Contains(in.Default, "${"):
			if _, err := netip.ParseAddr(in.Default); err != nil {
				l.errf(at+".address_input", "%s's default %q is not an address", svc.AddressInput, in.Default)
			}
		}
		if l.compose != nil {
			if s, ok := l.compose.Services[name]; ok && s.NetworkMode != "" {
				l.errf(at+".interface", "%s runs with network_mode %s: its address of its own is a network beside the stack's", name, s.NetworkMode)
			}
		}
	}
}

// frontHosts checks front_hosts: loopback, host-gateway or none, for a service the install runs
// on the host's network (host_network, or with a kairos shape an L4 route's upstream).
func (l *linter) frontHosts(at, name string, svc spec.Service) {
	switch svc.FrontHosts {
	case "":
		return
	case spec.FrontHostsLoopback, spec.FrontHostsGateway, spec.FrontHostsNone:
	default:
		l.errf(at+".front_hosts", "%q: %s (the default), %s or %s", svc.FrontHosts, spec.FrontHostsLoopback, spec.FrontHostsGateway, spec.FrontHostsNone)
		return
	}
	if !svc.HostNetwork && (l.s.Shapes.Kairos == nil || l.l4Route(name) < 0) {
		l.errf(at+".front_hosts", "is where a service on the host's network reaches the front's domains: %s is not on it (host_network, or an L4 route's upstream on the VM)", name)
	}
}

// serviceNetwork checks a service on the host's network (host_network): one container, no network
// of the stack's, no address of its own, no port published (the host's network publishes none);
// and the roles it intercepts on (transparent): each one of host.interfaces, once, for a service
// on the host's network with its own table, which steers the traffic there. Only the VM's
// firewall is Stowage's: without a kairos shape, warned.
func (l *linter) serviceNetwork(at, name string, svc spec.Service) {
	if svc.HostNetwork {
		if svc.Interface != "" {
			l.errf(at+".interface", "%s runs on the host's network (host_network), which holds no network beside it", name)
		}
		if l.compose != nil {
			if s, ok := l.compose.Services[name]; ok {
				if s.NetworkMode != "" {
					l.errf(at+".host_network", "%s sets network_mode %s in %s: host_network gives it the host's", name, s.NetworkMode, l.s.Stack.Compose)
				}
				if !emptyList(s.Ports) {
					l.errf(l.s.Stack.Compose+": services."+name+".ports", "%s runs on the host's network (services.%s.host_network), where it listens on its ports itself: publish none", name, name)
				}
				l.hostNetService(name, name+" runs on the host's network (services."+name+".host_network)")
			}
		}
		if l.s.Shapes.Docker == nil && l.s.Shapes.Kairos == nil {
			l.warnf(at+".host_network", "only the docker and kairos shapes run a service on the host's network: the chart's pod sets hostNetwork itself")
		}
	}
	if svc.Transparent == nil {
		return
	}
	at += ".transparent"
	l.roleList(at, svc.Transparent, "the host.interfaces roles its table intercepts traffic on, like [vpn0]", nil)
	if svc.Nftables == "" {
		l.errf(at, "%s's own table steers the traffic to its sockets: give it nftables", name)
	}
	if !svc.HostNetwork {
		l.errf(at, "a transparent socket is on the host's network, where the traffic is steered: set host_network: true")
	}
	if l.s.Shapes.Kairos == nil {
		l.warnf(at, "only the VM's firewall is Stowage's (a kairos shape): a Docker host's operator accepts the intercepted traffic in its input chain (tune advises it)")
	}
}

// serviceUpdate checks a start-first service: one container, no fixed name, no address of its
// own and no port published from a network of its own (two containers hold neither at once), and
// a healthcheck its compose file sets (the overlap waits for it; an image's own is warned about).
// update_socket is a directory of the container, update_signal one of spec.UpdateSignals, each
// for a start-first service alone.
func (l *linter) serviceUpdate(at, name string, svc spec.Service) {
	if g := svc.UpdateSignal; g != "" && !slices.Contains(spec.UpdateSignals, g) {
		l.errf(at+".update_signal", "%q: one of %s", g, strings.Join(spec.UpdateSignals, ", "))
	}
	switch svc.Update {
	case "", spec.UpdateStopFirst:
		if svc.UpdateSocket != "" {
			l.errf(at+".update_socket", "is the directory a start-first overlap hands its sockets over in: set update: %s", spec.UpdateStartFirst)
		}
		if svc.UpdateSignal != "" {
			l.errf(at+".update_signal", "is what a start-first overlap sends the old container: set update: %s", spec.UpdateStartFirst)
		}
		if svc.UpdateReady != "" {
			l.errf(at+".update_ready", "is the file a start-first overlap's new container writes once ready: set update: %s", spec.UpdateStartFirst)
		}
		if svc.UpdateDeclineStatus != nil {
			l.errf(at+".update_decline_status", "is how a start-first overlap's new container declines it: set update: %s", spec.UpdateStartFirst)
		}
		return
	case spec.UpdateStartFirst:
	default:
		l.errf(at+".update", "%q: %s (the default) or %s", svc.Update, spec.UpdateStopFirst, spec.UpdateStartFirst)
		return
	}
	if d := svc.UpdateSocket; d != "" && (!path.IsAbs(d) || path.Clean(d) != d || d == "/") {
		l.errf(at+".update_socket", "%q: a directory of the container, an absolute clean path other than /", d)
	}
	l.updateReady(at, svc)
	if svc.Interface != "" {
		l.errf(at+".update", "%s has an address of its own (interface), which two containers cannot hold at once", name)
	}
	if l.compose == nil {
		return
	}
	s, ok := l.compose.Services[name]
	if !ok {
		return
	}
	route := l.l4Route(name)
	switch {
	case (s.NetworkMode == "host" || svc.HostNetwork) && svc.UpdateSocket == "":
		l.warnf(at+".update", "%s runs on the host's network, where both containers of an overlap listen on its ports: the app sets SO_REUSEPORT, or hands its sockets over (update_socket)", name)
	case route >= 0 && l.s.Shapes.Kairos != nil && svc.UpdateSocket == "":
		l.warnf(at+".update", "%s is l4.routes[%d]'s upstream: on the VM it runs on the host's network, where both containers of an overlap listen on the route's port: the app sets SO_REUSEPORT, or hands its socket over (update_socket)", name, route)
	}
	if route >= 0 && l.s.Shapes.Docker != nil && s.NetworkMode != "host" && !svc.HostNetwork && !hasVar(s.NetworkMode) {
		l.warnf(at+".update", "%s is l4.routes[%d]'s upstream: on Docker its port is published from its own network, so a start there replaces it stop-first", name, route)
	}
	if s.ContainerName != "" {
		l.errf(at+".update", "%s sets container_name %s, which two containers cannot share", name, s.ContainerName)
	}
	if _, n := s.MemFloor(); n > 1 {
		l.errf(at+".update", "%s runs %d containers: a start-first overlap replaces one", name, n)
	}
	for _, v := range []any{s.Scale, s.Deploy.Replicas} {
		if q, ok := v.(string); ok {
			if _, err := strconv.Atoi(q); err != nil {
				l.errf(at+".update", "%s's number of containers is %q, which only a run knows: a start-first overlap replaces one", name, q)
			}
		}
	}
	if a := staticAddress(s); a != "" {
		l.errf(at+".update", "%s sets %s, which two containers cannot hold at once", name, a)
	}
	if strings.HasPrefix(s.NetworkMode, "service:") || strings.HasPrefix(s.NetworkMode, "container:") {
		l.errf(at+".update", "%s runs in another's network namespace (network_mode %s), where two containers cannot listen on one port", name, s.NetworkMode)
	}
	if s.NetworkMode != "host" && !svc.HostNetwork && !hasVar(s.NetworkMode) && !emptyList(s.Ports) {
		l.errf(at+".update", "%s publishes ports from its own network, which two containers cannot publish at once: run it on the host's network (network_mode: host, the port shared by SO_REUSEPORT or update_socket), or reach it through the front", name)
	}
	for _, other := range sortedKeys(l.compose.Services) {
		o := l.compose.Services[other]
		for _, c := range []struct{ key, v string }{{"network_mode", o.NetworkMode}, {"pid", o.Pid}, {"ipc", o.Ipc}} {
			if c.v == "service:"+name {
				l.errf(at+".update", "%s joins %s's namespace (%s: %s), which the overlap removes with its old container", other, name, c.key, c.v)
			}
		}
		for _, v := range stringList(o.VolumesFrom) {
			if base := strings.TrimSuffix(strings.TrimSuffix(v, ":ro"), ":rw"); base == name || base == "service:"+name {
				l.errf(at+".update", "%s mounts %s's volumes (volumes_from: %s), which the overlap leaves with its old container", other, name, v)
			}
		}
		for _, v := range stringList(o.Links) {
			if v == name || strings.HasPrefix(v, name+":") {
				l.errf(at+".update", "%s links to %s (links: %s), a link to its old container, which the overlap removes: reach it by its name", other, name, v)
			}
		}
	}
	h := s.Healthcheck
	broken, none, off, offKnown := "", false, false, true
	if h != nil {
		broken, none = healthBroken(h.Test)
		off, offKnown = composeBool(h.Disable)
	}
	switch {
	case h == nil:
		l.warnf(at+".update", "%s sets no healthcheck: the overlap waits for the image's, and without one only for the new container to run", name)
	case off || none:
		l.errf(at+".update", "%s disables its healthcheck, which the overlap waits for", name)
	case broken != "":
		l.errf(at+".update", "%s's healthcheck test %s: it never tells when the new container serves", name, broken)
	case healthVar(h.Test):
		l.warnf(at+".update", "%s's healthcheck test comes from a variable: lint cannot tell it runs a probe, which the overlap waits for", name)
	case emptyList(h.Test):
		l.warnf(at+".update", "%s's healthcheck sets no test: the overlap waits for the image's, and without one only for the new container to run", name)
	}
	if h != nil && !offKnown {
		l.warnf(at+".update", "%s's healthcheck disable is %v, which only a run knows or Compose does not read as a boolean: the overlap waits for its healthcheck", name, h.Disable)
	}
}

// updateReady checks a start-first service's update_ready, a file name of its update_socket
// directory with update_signal (the overlap holds the signal until the file is written), and its
// update_decline_status, an exit status from 1 to spec.MaxDeclineStatus, warned with update_signal
// and no update_ready (the old container is signalled as soon as the new one runs, and a decline
// after that fails the start).
func (l *linter) updateReady(at string, svc spec.Service) {
	if f := svc.UpdateReady; f != "" {
		switch {
		case strings.Contains(f, "/") || f == "." || f == "..":
			l.errf(at+".update_ready", "%q: a file name of update_socket's directory, without a /", f)
		case svc.UpdateSocket == "":
			l.errf(at+".update_ready", "is a file of update_socket's directory: set update_socket")
		case svc.UpdateSignal == "":
			l.errf(at+".update_ready", "holds update_signal until the new container writes it: set update_signal")
		}
	}
	if svc.UpdateDeclineStatus != nil {
		switch n := *svc.UpdateDeclineStatus; {
		case n < 1 || n > spec.MaxDeclineStatus:
			l.errf(at+".update_decline_status", "%d: an exit status from 1 to %d (those above are the engine's, the shell's and the signals')", n, spec.MaxDeclineStatus)
		case svc.UpdateSignal != "" && svc.UpdateReady == "":
			l.warnf(at+".update_decline_status", "the overlap sends update_signal as soon as the new container runs, and a decline after it fails the start: set update_ready, which the new container writes once it will not decline")
		}
	}
}

// staticAddress is the key of an address a service fixes for its container ("" when none): its
// mac_address, or a network's ipv4_address, ipv6_address or mac_address.
func staticAddress(s Service) string {
	if s.MacAddress != "" {
		return "mac_address"
	}
	nets, _ := s.Networks.(map[string]any)
	for _, n := range sortedKeys(nets) {
		cfg, _ := nets[n].(map[string]any)
		for _, k := range []string{"ipv4_address", "ipv6_address", "mac_address"} {
			if v, ok := cfg[k]; ok && v != nil && v != "" {
				return fmt.Sprintf("networks.%s.%s", n, k)
			}
		}
	}
	return ""
}

// hasVar reports whether v holds an interpolation only a run resolves.
func hasVar(v string) bool {
	refs, _ := Interpolations(v) // the compose file's load already refused one that does not parse
	return len(refs) > 0
}

// healthVar reports whether a healthcheck's test holds an interpolation (Interpolations: a $$
// escape is none), which only a run resolves.
func healthVar(test any) bool {
	var parts []any
	switch t := test.(type) {
	case string:
		parts = []any{t}
	case []any:
		parts = t
	}
	for _, a := range parts {
		if q, ok := a.(string); ok && hasVar(q) {
			return true
		}
	}
	return false
}

// healthBroken is why a healthcheck's list or string test can never tell when a container serves
// ("" when it can), and none when it is ["NONE"], Compose's other way to disable it: a list not of CMD, CMD-SHELL or NONE (Docker runs no probe for it), a blank
// command (`sh -c ""` passes at once, an empty CMD never passes), a blank string (the same as
// CMD-SHELL "").
func healthBroken(test any) (why string, none bool) {
	switch t := test.(type) {
	case string:
		switch strings.TrimSpace(t) {
		case "":
			return "runs no command", false
		case "NONE":
			return `is the string NONE, the shell command "NONE", which never passes`, false
		}
	case []any:
		if len(t) == 0 || healthVar(t[:1]) {
			return "", false // a head only a run resolves: healthVar's
		}
		switch t[0] {
		case "CMD", "CMD-SHELL":
			if len(t) < 2 {
				return "runs no command", false
			}
			q, ok := t[1].(string)
			if !ok {
				return fmt.Sprintf("has a command (after %s) that is not a string", t[0]), false
			}
			if strings.TrimSpace(q) == "" {
				return "runs no command", false
			}
		case "NONE":
			return "", true
		default:
			return fmt.Sprintf("starts with %v, not CMD, CMD-SHELL or NONE, so Docker runs no probe", t[0]), false
		}
	}
	return "", false
}

// containerJoins warns, once a service is start-first, of each key of a service that joins a
// container by its name or ID (network_mode, pid, ipc or volumes_from container:…, or
// external_links), or one only a run resolves (links included): if that container is a
// start-first service's, the install replaces that service stop-first. A start-first service's
// own network_mode container:… is serviceUpdate's error; one from a variable is warned here.
func (l *linter) containerJoins() {
	first := func(n string) bool { return l.s.Services[n].Update == spec.UpdateStartFirst }
	some := false
	for n := range l.s.Services {
		some = some || first(n)
	}
	if l.compose == nil || !some {
		return
	}
	for _, name := range sortedKeys(l.compose.Services) {
		o := l.compose.Services[name]
		at := l.s.Stack.Compose + ": services." + name
		for _, k := range []struct {
			key string
			vs  []string
		}{{"network_mode", []string{o.NetworkMode}}, {"pid", []string{o.Pid}}, {"ipc", []string{o.Ipc}},
			{"volumes_from", stringList(o.VolumesFrom)}, {"links", stringList(o.Links)}, {"external_links", stringList(o.ExtLinks)}} {
			if k.key == "network_mode" && first(name) && (strings.HasPrefix(o.NetworkMode, "container:") || strings.HasPrefix(o.NetworkMode, "service:")) {
				continue // serviceUpdate's error
			}
		values:
			for _, v := range k.vs {
				switch {
				case hasVar(v) && k.key == "network_mode" && first(name):
					l.warnf(at+"."+k.key, "%s", l.ownNetworkVar(name, v))
				case hasVar(v) && k.key == "links":
					l.warnf(at+"."+k.key, "%s, which only a run resolves: if it names a start-first service, the install replaces that service stop-first", v)
				case hasVar(v):
					l.warnf(at+"."+k.key, "%s, which only a run resolves: if it names a start-first service or its container, the install replaces that service stop-first", v)
				case k.key == "external_links":
					l.warnf(at+"."+k.key, "links to a container by its name (%s): if that container is a start-first service's, the install replaces that service stop-first (reach the service by its name instead)", v)
				case k.key != "links" && strings.HasPrefix(v, "container:"):
					l.warnf(at+"."+k.key, "joins a container by its name or ID (%s): if that container is a start-first service's, the install replaces that service stop-first (join it as service:<name>)", v)
				default:
					continue
				}
				break values // once a key
			}
		}
	}
}

// composeLists refuses a compose service's volumes_from, links or external_links that is not a
// list of strings, which Compose refuses: the checks reading them would leave the entry out. Each
// is the service's own, before extends joins its base's.
func (l *linter) composeLists() {
	if l.compose == nil {
		return
	}
	for _, name := range sortedKeys(l.compose.Services) {
		o := l.compose.Services[name]
		for _, k := range []struct {
			key string
			v   any
		}{{"volumes_from", o.OwnVolumesFrom}, {"links", o.OwnLinks}, {"external_links", o.OwnExtLinks}} {
			if k.v == nil {
				continue
			}
			at := l.s.Stack.Compose + ": services." + name + "." + k.key
			items, ok := k.v.([]any)
			if !ok {
				l.errf(at, "is not a list: Compose takes a list of strings")
				continue
			}
			for i, item := range items {
				if _, ok := item.(string); !ok {
					l.errf(fmt.Sprintf("%s[%d]", at, i), "%v is not a string, which Compose refuses", item)
				}
			}
		}
	}
}

// ownNetworkVar is the warning for a start-first service whose network_mode only a run resolves,
// naming the cases that apply to it.
func (l *linter) ownNetworkVar(name, v string) string {
	msg := v + ", which only a run resolves: if it is service:… or container:…"
	route := l.l4Route(name) >= 0
	if s, ok := l.compose.Services[name]; ok && !emptyList(s.Ports) || route {
		msg += ", or a network of its own while it publishes ports"
		if route && l.s.Shapes.Docker != nil {
			msg += " (its L4 route's on Docker included)"
		}
	}
	msg += ", the install replaces " + name + " stop-first"
	if l.s.Services[name].UpdateSocket == "" {
		msg += ", and if it is host, its ports are the app's to share (SO_REUSEPORT or update_socket)"
	}
	return msg
}

// l4Route is the index of the first L4 route whose upstream is service, -1 when none.
func (l *linter) l4Route(service string) int {
	for i, r := range l.s.L4.Routes {
		if m := upstreamRe.FindStringSubmatch(r.Upstream); m != nil && m[1] == service {
			return i
		}
	}
	return -1
}

// hostNetworkServices are the services that run on the host's network, each sorted and once:
// always, those of services.<name>.host_network and a compose service of network_mode host; onVM,
// with a kairos shape, every other L4 route's upstream, which the VM alone runs there.
func (l *linter) hostNetworkServices() (always, onVM []string) {
	for name, svc := range l.s.Services {
		if svc.HostNetwork {
			always = append(always, name)
		}
	}
	if l.compose != nil {
		for name, svc := range l.compose.Services {
			if svc.NetworkMode == "host" {
				always = append(always, name)
			}
		}
	}
	slices.Sort(always)
	always = slices.Compact(always)
	if l.s.Shapes.Kairos != nil {
		for _, r := range l.s.L4.Routes {
			if m := upstreamRe.FindStringSubmatch(r.Upstream); m != nil && !slices.Contains(always, m[1]) {
				onVM = append(onVM, m[1])
			}
		}
	}
	slices.Sort(onVM)
	return always, slices.Compact(onVM)
}

// stringList is a compose list's strings; an entry of another kind is left out.
func stringList(v any) []string {
	items, _ := v.([]any)
	var out []string
	for _, f := range items {
		if q, ok := f.(string); ok && q != "" {
			out = append(out, q)
		}
	}
	return out
}

// emptyList reports whether a compose list (ports, volumes) is absent or empty.
func emptyList(v any) bool {
	l, ok := v.([]any)
	return v == nil || ok && len(l) == 0
}

// nftTable checks a service's nftables file: inside the project, holding one table, `table inet
// <service>`, and nothing beside it (no include, no flush of the ruleset or of another table): the
// host's firewall stays Stowage's. The OS bundle's build and the VM check its syntax with nft.
func (l *linter) nftTable(at, service, rel string) {
	path, err := spec.ProjectPath(l.root, rel)
	if err != nil {
		l.errf(at, "%v", err)
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		l.errf(at, "%v", err)
		return
	}
	if service == l.s.Name {
		l.errf(at, "the service %s has the project's name: its table inet %s would be the VM's own firewall's", service, service)
		return
	}
	if spec.NftKeyword(service) {
		l.errf(at, "the service's name %q is a word of nft's own: its table needs another name", service)
		return
	}
	if !spec.NftTableName.MatchString(service) {
		l.errf(at, "the service's name %q is no nftables table's: a letter, then letters, digits and _ - . /", service)
		return
	}
	if err := CheckNftTable(string(raw), service); err != nil {
		l.errf(at, "%s: %v", rel, err)
	} else if nftDropsInput(string(raw)) {
		l.warnf(at, "%s: a chain hooks the input with a drop policy: a base chain of any table judges every packet, so it drops what the VM's own firewall accepts (SSH, the stack's ports) unless it accepts them itself", rel)
	}
}

// nftTokens are an nftables file's tokens as nft lexes them where it matters here: a quoted
// string one token (its braces and # its own), a comment (# outside a string, to the line's end)
// none, a backslash before a newline a continuation, a brace one token, the rest words split at
// spaces, tabs, newlines, ; and braces. An unterminated string, and a character nft takes for junk
// (a carriage return, a form feed, a vertical tab), are errors.
func nftTokens(text string) ([]string, error) {
	var out []string
	word := strings.Builder{}
	flush := func() {
		if word.Len() > 0 {
			out = append(out, word.String())
			word.Reset()
		}
	}
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case c == '"':
			flush()
			j := strings.IndexByte(text[i+1:], '"')
			if j < 0 {
				return nil, fmt.Errorf("a string is not closed")
			}
			out = append(out, text[i:i+j+2])
			i += j + 1
		case c == '#':
			flush()
			if j := strings.IndexByte(text[i:], '\n'); j >= 0 {
				i += j
			} else {
				i = len(text)
			}
		case c == '\\' && i+1 < len(text) && text[i+1] == '\n':
			flush()
			i++
		case c == '\r' || c == '\f' || c == '\v':
			return nil, fmt.Errorf("holds a carriage return, a form feed or a vertical tab, which nft refuses (save it with Unix line endings)")
		case c == '{' || c == '}':
			flush()
			out = append(out, string(c))
		case c == ';' || c == ' ' || c == '\t' || c == '\n':
			flush()
		default:
			word.WriteByte(c)
		}
	}
	flush()
	return out, nil
}

// CheckNftTable checks that an nftables file is one `table inet <name> { … }` alone, as nft lexes
// it (strings and comments aside): nothing before or after it, its braces balanced, no include nor
// a word naming the ruleset.
func CheckNftTable(text, name string) error {
	toks, err := nftTokens(text)
	if err != nil {
		return err
	}
	head := fmt.Sprintf("table inet %s {", name)
	if len(toks) < 4 || toks[0] != "table" || toks[1] != "inet" || toks[2] != name || toks[3] != "{" {
		return fmt.Errorf("does not start with %q", head)
	}
	depth := 0
	for i, t := range toks {
		switch t {
		case "include", "ruleset":
			return fmt.Errorf("holds an include or names the ruleset: the file is its table alone")
		case "{":
			depth++
		case "}":
			if depth--; depth == 0 && i != len(toks)-1 {
				return fmt.Errorf("holds something after its table: the file is %q alone", head+" … }")
			}
		}
	}
	if depth != 0 {
		return fmt.Errorf("its braces do not balance")
	}
	return nil
}

// nftDropsInput reports whether a table's chain hooks the input, or a hook before it (prerouting,
// ingress), with a drop policy: a base chain of any table judges every packet, so it drops what
// the host's own firewall accepts (SSH, the stack's ports) unless the chain accepts them itself.
func nftDropsInput(text string) bool {
	toks, err := nftTokens(text)
	if err != nil {
		return false
	}
	depth, hook, drop := 0, false, false
	for i, t := range toks {
		switch {
		case t == "{":
			if depth++; depth == 2 {
				hook, drop = false, false
			}
		case t == "}":
			if depth == 2 && hook && drop {
				return true
			}
			depth--
		case depth == 2 && t == "hook" && i+1 < len(toks) && slices.Contains([]string{"input", "prerouting", "ingress"}, toks[i+1]):
			hook = true
		case depth == 2 && t == "policy" && i+1 < len(toks) && toks[i+1] == "drop":
			drop = true
		}
	}
	return false
}
