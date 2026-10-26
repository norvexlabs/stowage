package install

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/norvexlabs/stowage/internal/spec"
)

// InterfaceVarPrefix starts the install's variables that name, on a host Stowage does not own,
// the NIC playing a host.interfaces role (STOWAGE_INTERFACE_<ROLE>, `install --interface`): on
// the VM each NIC is named by its role.
const InterfaceVarPrefix = "STOWAGE_INTERFACE_"

// InterfaceVar is the variable of role.
func InterfaceVar(role string) string { return InterfaceVarPrefix + strings.ToUpper(role) }

// ipvlan is a service's address of its own on a NIC (services.<name>.interface): an ipvlan
// network beside the stack's, on the NIC playing the role (its parent), in the subnet the NIC is
// in, the address from the service's input.
type ipvlan struct {
	Network, Parent string
	Subnet          netip.Prefix
	Address         netip.Addr
}

// ipvlanNetwork is the name of the stack's network on role's NIC.
func ipvlanNetwork(role string) string { return "stowage-" + role }

// ipvlans are the services' addresses of their own, by service, read at each start: the NIC's
// subnet from its IPv4 address now, the address from the install's input (empty, none: the
// service is on the stack's networks alone). A NIC no variable names
// on a Docker host, one with no IPv4 address, or an address outside its subnet fails the start,
// naming what to give.
func (in *Installer) ipvlans(ctx context.Context, get func(string) string) (map[string]ipvlan, error) {
	out := map[string]ipvlan{}
	for _, name := range sortedKeys(in.R.Spec.Services) {
		svc := in.R.Spec.Services[name]
		if svc.Interface == "" || strings.TrimSpace(get(svc.AddressInput)) == "" {
			continue // none given: the service is on the stack's networks alone
		}
		parent := svc.Interface
		if in.O.Firewall == "" {
			if parent = get(InterfaceVar(svc.Interface)); parent == "" {
				return nil, fmt.Errorf("services.%s.interface: which NIC of this host plays %s? `%s install --interface %s=<its NIC>` names it", name, svc.Interface, in.R.Ctl(), svc.Interface)
			}
		}
		raw, err := in.O.Host.Run(ctx, Cmd{Args: []string{"ip", "-4", "-o", "addr", "show", "dev", parent}})
		if err != nil {
			return nil, fmt.Errorf("services.%s.interface: the NIC %s: %w", name, parent, err)
		}
		var subnet netip.Prefix
		var own []netip.Addr
		for _, f := range strings.Fields(string(raw)) {
			if p, err := netip.ParsePrefix(f); err == nil && p.Addr().Is4() {
				own = append(own, p.Addr())
				if !subnet.IsValid() {
					subnet = p.Masked()
				}
			}
		}
		if !subnet.IsValid() {
			return nil, fmt.Errorf("services.%s.interface: the NIC %s has no IPv4 address now, whose subnet its address would be in", name, parent)
		}
		a, err := netip.ParseAddr(strings.TrimSpace(get(svc.AddressInput)))
		if err != nil || !subnet.Contains(a) {
			return nil, fmt.Errorf("services.%s.address_input: %s %q is not an address of %s, the subnet of %s", name, svc.AddressInput, get(svc.AddressInput), subnet, parent)
		}
		if slices.Contains(own, a) || a == subnet.Addr() || subnet.Bits() <= 30 && a == broadcast(subnet) {
			return nil, fmt.Errorf("services.%s.address_input: %s %s is %s's own address, or %s's network or broadcast address", name, svc.AddressInput, a, parent, subnet)
		}
		out[name] = ipvlan{Network: ipvlanNetwork(svc.Interface), Parent: parent, Subnet: subnet, Address: a}
	}
	return out, nil
}

// giveIPVlan joins a service to its ipvlan network at its address, beside the networks it joins.
func giveIPVlan(extra map[string]any, v ipvlan, joined map[string]any) {
	nets := map[string]any{}
	for n := range joined {
		nets[n] = map[string]any{}
	}
	if len(nets) == 0 {
		nets["default"] = map[string]any{} // a service joining no network named joins the default one
	}
	nets[v.Network] = map[string]any{"ipv4_address": v.Address.String()}
	extra["networks"] = nets
}

// ipvlanNetworks are the ipvlan networks the services' addresses are on, as Compose declares them.
func ipvlanNetworks(vs map[string]ipvlan) map[string]any {
	out := map[string]any{}
	for _, v := range vs {
		out[v.Network] = map[string]any{
			"driver":      "ipvlan",
			"driver_opts": map[string]string{"parent": v.Parent, "ipvlan_mode": "l2"},
			"ipam":        map[string]any{"config": []map[string]string{{"subnet": v.Subnet.String()}}},
		}
	}
	return out
}

// interfaceVars records --interface: each role's NIC on this host (InterfaceVar), a role of the
// release's NICs and a NIC this host has. The VM names its NICs by their roles: none is given there.
func (in *Installer) interfaceVars(env *EnvFile) error {
	for _, role := range sortedKeys(in.O.Interfaces) {
		nic := in.O.Interfaces[role]
		var i spec.HostInterface
		ok := in.R.Spec.Host != nil
		if ok {
			i, ok = in.R.Spec.Host.Interfaces[role]
		}
		switch {
		case in.O.Firewall != "":
			return fmt.Errorf("--interface: the VM names its NICs by their roles (vm create --interface)")
		case !ok || i.Kind == spec.InterfaceTUN:
			return fmt.Errorf("--interface %s: not one of %s's NICs (host.interfaces)", role, in.R.Spec.Name)
		case nic == "":
			env.Delete(InterfaceVar(role))
		case !ifName.MatchString(nic):
			return fmt.Errorf("--interface %s=%s: not an interface's name", role, nic)
		default:
			env.Set(InterfaceVar(role), nic)
		}
	}
	return nil
}

// ifName is an interface's name as the kernel takes one.
var ifName = regexp.MustCompile(`^[A-Za-z0-9_.:@-]{1,15}$`)

// interfacesValue is --interface, ROLE=NIC, repeatable.
type interfacesValue struct{ m *map[string]string }

func (v interfacesValue) String() string { return "" }
func (v interfacesValue) Type() string   { return "ROLE=NIC" }
func (v interfacesValue) Set(s string) error {
	role, nic, ok := strings.Cut(s, "=")
	if !ok || role == "" {
		return fmt.Errorf("%q: ROLE=NIC", s)
	}
	if *v.m == nil {
		*v.m = map[string]string{}
	}
	(*v.m)[role] = nic
	return nil
}

// broadcast is an IPv4 subnet's broadcast address.
func broadcast(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	for i := p.Bits(); i < 32; i++ {
		b[i/8] |= 1 << (7 - i%8)
	}
	return netip.AddrFrom4(b)
}
