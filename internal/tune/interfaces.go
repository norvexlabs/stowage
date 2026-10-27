package tune

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Interface is one of the project's host.interfaces: its role (the interface's name on a host
// Stowage owns), its kind (nic or tun), its MTU and policy routing in networkd's terms, and why.
type Interface struct {
	Role, Kind         string
	MTU                int
	RoutingPolicyRules []map[string]string
	Routes             []map[string]string
	// NIC is a NIC's ethtool settings; nil for none.
	NIC *NICSettings
	// Forward are the roles the host forwards its traffic out of; Transparent, whether a service
	// intercepts traffic on it (a transparent socket).
	Forward     []string
	Transparent bool
	Reason      string
}

// Firewall are the rules a host's firewall gives the interface's traffic for the project, where
// Stowage does not own the host: its traffic out of each Forward role accepted in chain (Docker's
// DOCKER-USER, ahead of its FORWARD policy, DROP), and the replies back, for IPv4 and IPv6; and,
// when Transparent, what the kernel hands a transparent socket accepted by an input chain that
// drops.
func (i Interface) Firewall(chain string) []string {
	var out []string
	for _, o := range i.Forward {
		for _, t := range []string{"iptables", "ip6tables"} {
			out = append(out, fmt.Sprintf("%s -I %s -i %s -o %s -j ACCEPT", t, chain, i.Role, o),
				fmt.Sprintf("%s -I %s -i %s -o %s -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT", t, chain, o, i.Role))
		}
	}
	if i.Transparent {
		out = append(out, fmt.Sprintf("nft: iifname %q meta l4proto { tcp, udp } socket transparent 1 accept, in an input chain that drops", i.Role))
	}
	return out
}

// Networkd is the interface's settings as a networkd .network file's sections, for the operator
// of a host Stowage does not own to give the NIC that plays its role.
func (i Interface) Networkd() string {
	var b strings.Builder
	if i.MTU != 0 {
		fmt.Fprintf(&b, "[Link] MTUBytes=%d; ", i.MTU)
	}
	for _, sec := range []struct {
		name    string
		entries []map[string]string
	}{{"RoutingPolicyRule", i.RoutingPolicyRules}, {"Route", i.Routes}} {
		for _, e := range sec.entries {
			var kv []string
			for _, k := range slices.Sorted(maps.Keys(e)) {
				kv = append(kv, k+"="+e[k])
			}
			fmt.Fprintf(&b, "[%s] %s; ", sec.name, strings.Join(kv, " "))
		}
	}
	return strings.TrimSuffix(b.String(), "; ")
}

// inspectInterfaces judges each interface the project declares. On a host Stowage owns, its NICs
// are named by their roles: one missing, or of another MTU, is drift; a tun device is its service's
// and absent until it opens it. On another host the NICs are its operator's: advice, with the
// networkd settings that give one its role's.
func (s System) inspectInterfaces(ctx context.Context) []Item {
	var items []Item
	eth, err := s.Look("ethtool")
	if err != nil {
		eth = ""
	}
	for _, i := range s.Interfaces {
		why := " (" + i.Reason + ")"
		set := i.Networkd()
		if !s.Owned {
			want := "a NIC of this host plays it: networkd's .network file of that NIC"
			if set != "" {
				want += " with " + set
			}
			if i.NIC != nil {
				want += "; " + strings.Join(i.NIC.commands("<its NIC>"), "; ")
			}
			if i.Kind == "tun" {
				want = "its service opens it (the host's network, /dev/net/tun)"
				if set != "" {
					want += "; networkd's .network file matching it by name with " + set
				}
			}
			items = append(items, Item{Advice, "interface", i.Role, "the project names an interface " + i.Role + why + ":", want})
			if fw := i.Firewall("DOCKER-USER"); len(fw) > 0 {
				items = append(items, Item{Advice, "interface", i.Role, "the project forwards or intercepts traffic on " + i.Role + ", which this host's firewall drops:", strings.Join(fw, "; ")})
			}
			continue
		}
		mtu, ok := s.read("sys/class/net/" + i.Role + "/mtu")
		switch {
		case !ok && i.Kind == "tun":
			items = append(items, Item{NA, "interface", i.Role, "absent: its service opens it", "present" + why})
		case !ok:
			items = append(items, Item{Differs, "interface", i.Role, "absent", "present" + why})
		case i.MTU != 0 && mtu != strconv.Itoa(i.MTU):
			items = append(items, Item{Differs, "interface", i.Role, "MTU " + mtu, fmt.Sprintf("MTU %d%s", i.MTU, why)})
		default:
			items = append(items, Item{OK, "interface", i.Role, "present, MTU " + mtu, "present" + why})
		}
		if ok && eth != "" {
			items = append(items, s.inspectNIC(ctx, eth, i)...)
		}
	}
	return items
}
