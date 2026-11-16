package install

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/kairos"
	"github.com/norvexlabs/stowage/internal/spec"
)

// A NIC attached to an installed VM (`network attach`) is recorded in the install
// directory (interfacesFile) and rendered as a cloud-config of the OEM partition, which every boot
// runs as it runs the seed's: its .link file in the initramfs stage, before udev names the NICs,
// and its addressing in the boot stage. The seed's own files are left as written: a NIC the seed
// named by its original name alone (the only one it knew) has its .link file replaced by one
// matching its MAC by the VM's first boot (pinScript), so the new NIC never takes its name.

// interfacesFile is the record of the NICs attached to the VM, in its install directory.
const interfacesFile = "interfaces.yaml"

// interfacesLock is the lock `network attach` holds while it reads and writes interfacesFile and
// the attached NICs' cloud-config, in the install directory: two at once each keep the other's
// NIC.
const interfacesLock = "interfaces.lock"

// oemDir, sysNetDir and udevDataDir are the OEM partition every boot reads cloud-configs from, the
// kernel's NICs and udev's records of them; tests move them.
var (
	oemDir      = "/oem"
	sysNetDir   = "/sys/class/net"
	udevDataDir = "/run/udev/data"
)

// attachedNIC is a NIC of interfacesFile: its role, its MAC, its own driver when the attach found
// the NIC there (nicDriver; empty when it was not), and its addressing (Mode, Address as a
// secondary's).
type attachedNIC struct {
	Role    string `yaml:"role"`
	MAC     string `yaml:"mac"`
	Driver  string `yaml:"driver,omitempty"`
	Mode    string `yaml:"mode,omitempty"`
	Address string `yaml:"address,omitempty"`
}

// attachedMatch is the [Match] of n's .link file: its MAC and the Driver= value of its driver
// recorded (macMatch, the boot's pin's rule): a driver of vfDrivers (a physical Mellanox NIC on
// metal) matched itself, any other or none recorded (the NIC not there at the attach) leaving
// vfDrivers out — so an accelerated networking's virtual function sharing that MAC is never named
// n's role. A physical NIC of one of vfDrivers attached before it is there is not named until a
// later `network attach` with the NIC there records its driver.
func attachedMatch(n attachedNIC) string { return macMatch(n.MAC, n.Driver) }

// attachedLink is the .link file naming an attached NIC: 09- sorts before the seed's 10- ones, and
// udev takes the first file that matches.
func attachedLink(name, role string) string { return "09-" + name + "-" + role + ".link" }

// attachedOEM is the cloud-config of the attached NICs on the OEM partition: after the seed's
// (95_userdata), which every boot runs first.
func attachedOEM(name string) string { return filepath.Join(oemDir, attachedOEMName(name)) }

// attachedOEMName is attachedOEM's name, which the boot's pin reads the attached NICs' MACs from.
func attachedOEMName(name string) string { return "96_" + name + "-interfaces.yaml" }

// NetAttach is `network attach`'s request: a secondary NIC the release names, by its MAC, and its
// addressing (Mode, default none; Address with static).
type NetAttach struct {
	Role, MAC, Mode, Address string
}

// AttachNIC records a NIC for a role of r's host.interfaces the VM's cloud-config does not name,
// and writes the cloud-config that names and addresses it from the next boot: by its MAC and the
// Driver= value of its driver, recorded when the NIC is there (attachedMatch). Given again for the same role, it replaces the role's
// MAC and addressing, and records the driver of a NIC there now (the same MAC keeps the one
// recorded while its NIC is not there).
func AttachNIC(r *Release, dir string, a NetAttach, out io.Writer) error {
	name := r.Spec.Name
	var roles []string
	primary := ""
	if h := r.Spec.Host; h != nil {
		for _, k := range slices.Sorted(maps.Keys(h.Interfaces)) {
			if i := h.Interfaces[k]; i.Kind == "" || i.Kind == spec.InterfaceNIC {
				roles = append(roles, k)
				if i.Primary {
					primary = k
				}
			}
		}
	}
	switch {
	case !slices.Contains(roles, a.Role):
		if len(roles) == 0 {
			return fmt.Errorf("--role %s: %s declares no NIC (host.interfaces)", a.Role, name)
		}
		return fmt.Errorf("--role %s: not one of %s's NICs (%s)", a.Role, name, strings.Join(roles, ", "))
	case a.Role == primary:
		return fmt.Errorf("--role %s: the primary NIC is the VM's own: `network set` addresses it", a.Role)
	}
	if named, err := seedNamed(name, a.Role); err != nil {
		return refuse(fmt.Sprintf("restore it as the VM's cloud-config writes it (`sudo %s reboot` writes it again), then run this again", r.Ctl()), "--role %s: %w", a.Role, err)
	} else if named {
		return fmt.Errorf("--role %s: the VM's cloud-config names its NIC already", a.Role)
	}
	// A role the seed names by its NIC's original name, not pinned to its MAC (a VM seeded before
	// 0.40.0, or one whose first boot did not pin it): a NIC added may take its name.
	links, err := seedLinks(networkdDir, name)
	if err != nil {
		return err
	}
	for _, role := range slices.Sorted(maps.Keys(links)) {
		if l := links[role]; l.Original && !l.pinned() {
			return refuse("pin it first: by hand, as \"A VM seeded before 0.40.0\" in docs/vm.md says, or, on a VM seeded by Stowage 0.40.0 or later, leave its NIC the VM's only one and restart the VM, whose boot pins it to that NIC's MAC; then run this again",
				"--role %s: the VM's cloud-config names %s by its NIC's original name, not pinned to its MAC: a NIC added may take its name", a.Role, role)
		}
	}
	mac := strings.ToLower(a.MAC)
	if !macAddr.MatchString(mac) || !unicastMAC(mac) {
		return fmt.Errorf("--mac %q is not a unicast MAC address (02:00:00:00:00:01)", a.MAC)
	}
	nic := attachedNIC{Role: a.Role, MAC: mac, Mode: cmp.Or(a.Mode, IfaceNone)}
	switch nic.Mode {
	case IfaceDHCP, IfaceNone:
		if a.Address != "" {
			return fmt.Errorf("--address is the static mode's")
		}
	case IfaceStatic:
		pfx, err := netip.ParsePrefix(a.Address)
		if err != nil || !pfx.Addr().Is4() || pfx.Bits() < 1 {
			return fmt.Errorf("--address %q: the static mode's IPv4 address with its prefix (192.0.2.10/24)", a.Address)
		}
		nic.Address = pfx.String()
	default:
		return fmt.Errorf("--mode %s: dhcp, static or none", a.Mode)
	}
	// One attach at a time: each reads the record, then writes it whole.
	unlock, err := flockWait(context.Background(), filepath.Join(dir, interfacesLock), true)
	if err != nil {
		return err
	}
	defer unlock()
	nics, err := readAttached(dir)
	if err != nil {
		return err
	}
	var before attachedNIC
	if i := slices.IndexFunc(nics, func(n attachedNIC) bool { return n.Role == a.Role }); i >= 0 {
		before = nics[i]
	}
	nics = slices.DeleteFunc(nics, func(n attachedNIC) bool { return n.Role == a.Role })
	for _, n := range nics {
		if n.MAC == mac {
			return fmt.Errorf("--mac %s is %s's NIC's", mac, n.Role)
		}
	}
	// The VM's NICs, read before anything changes: unread, the attach is refused.
	devs, err := deviceNICs(sysNetDir)
	if err != nil {
		return err
	}
	if other, err := macTaken(name, a.Role, mac, roles, devs); err != nil || other != "" {
		if err != nil {
			return err
		}
		return fmt.Errorf("--mac %s is %s's NIC's", mac, other)
	}
	// The NIC there (ownNIC: a virtual function of its MAC beside it left out), its driver is
	// recorded; not there (or no driver recorded), the one a previous attach of the same MAC
	// recorded, if any.
	dev, driver, err := ownNIC(sysNetDir, udevDataDir, devs, mac)
	if err != nil {
		return err
	}
	there := dev != ""
	nic.Driver = driver
	if nic.Driver == "" && before.MAC == mac {
		nic.Driver = before.Driver
	}
	nics = append(nics, nic)
	slices.SortFunc(nics, func(x, y attachedNIC) int { return strings.Compare(x.Role, y.Role) })
	rec, err := yaml.Marshal(nics)
	if err != nil {
		return err
	}
	cc, err := attachedConfig(name, r.Ctl(), nics)
	if err != nil {
		return err
	}
	// The cloud-config first: a record without it would say attached what no boot names.
	if err := writeAtomic(attachedOEM(name), bytes.NewReader(cc), 0o644, nil); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, interfacesFile), bytes.NewReader(rec), 0o644, nil); err != nil {
		return err
	}
	how := nic.Mode
	if nic.Address != "" {
		how += " " + nic.Address
	}
	reboot := fmt.Sprintf("`sudo %s reboot`", r.Ctl())
	next := "add the NIC on the VM's platform, then restart the VM: the boot names it"
	if there {
		next = "the NIC is there: " + reboot + " names it"
	}
	if nic.Driver == "" {
		next += "; its driver unread (no NIC there, or none udev records), its link leaves out a virtual function's drivers (" +
			strings.Join(vfDrivers, ", ") + "): a NIC of one of them is named once this is run again with the NIC there"
	}
	fmt.Fprintf(out, "%s: the NIC %s is named and addressed (%s) from the next boot: %s\n", a.Role, mac, how, next)
	return nil
}

// seedNamed reports whether the VM's cloud-config names a NIC role (its .link file there); a
// file whose presence does not read is an error, never one absent.
func seedNamed(name, role string) (bool, error) {
	path := filepath.Join(networkdDir, kairos.InterfaceLink(name, role))
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	}
	return false, fmt.Errorf("the VM's cloud-config's .link file of %s does not read (%w)", role, err)
}

// macTaken is the role another NIC with mac plays: one the seed names by it (its own MAC, or the
// one its first boot pinned), or one of the VM's NICs (nics, deviceNICs) named as another of roles
// now. A NIC with it named otherwise (one a boot did not name yet) is free.
func macTaken(name, role, mac string, roles []string, nics []deviceNIC) (string, error) {
	links, err := seedLinks(networkdDir, name)
	if err != nil {
		return "", err
	}
	for _, other := range slices.Sorted(maps.Keys(links)) {
		if other != role && links[other].MAC == mac {
			return other, nil
		}
	}
	for _, n := range nics {
		if n.Name != role && n.MAC == mac && slices.Contains(roles, n.Name) {
			return n.Name, nil
		}
	}
	return "", nil
}

// readAttached reads interfacesFile; none is no NIC attached. A record that does not decode is a
// refusal naming its fix: one of Stowage 0.39.0 or earlier pinning the seed's NIC (`pin: true`, a
// field no longer known) its own.
func readAttached(dir string) ([]attachedNIC, error) {
	path := filepath.Join(dir, interfacesFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var nics []attachedNIC
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&nics); err != nil && !errors.Is(err, io.EOF) {
		var loose []map[string]any
		if yaml.Unmarshal(raw, &loose) == nil && slices.ContainsFunc(loose, func(n map[string]any) bool { _, ok := n["pin"]; return ok }) {
			return nil, refuse(fmt.Sprintf("pin the VM's NIC by hand (\"A VM seeded before 0.40.0\" in docs/vm.md), remove the entries with `pin: true` from %s, then run this again", path),
				"%s has a pin entry from Stowage 0.39.0 or earlier: remove its entries with `pin: true` (the first boot pins the NIC itself now), then run this again; this VM's cloud-config, from before 0.40.0, does not: pin its NIC by hand first, as \"A VM seeded before 0.40.0\" in docs/vm.md says", path)
		}
		return nil, refusal(fmt.Sprintf("fix %s (a list of role, mac, driver, mode and address), or remove it and run `network attach` again for each NIC it held", path), fmt.Errorf("%s: %w", path, err))
	}
	return nics, nil
}

// attachedConfig is the OEM partition's cloud-config of nics: their .link files before udev
// names the NICs, their addressing (a secondary's) at boot.
func attachedConfig(name, ctl string, nics []attachedNIC) ([]byte, error) {
	var links, networks []cloudFile
	for _, n := range nics {
		i := MachineInterface{Role: n.Role, MAC: n.MAC, Mode: n.Mode, Address: n.Address}
		links = append(links, cloudFile{Path: "/etc/systemd/network/" + attachedLink(name, n.Role), Permissions: 0o644,
			Content: linkFile(fmt.Sprintf("%s's interface %s (`network attach`), named at every boot", name, n.Role), attachedMatch(n), n.Role)})
		networks = append(networks, cloudFile{Path: "/etc/systemd/network/" + kairos.InterfaceUnit(name, n.Role), Permissions: 0o644,
			Content: "[Match]\nName=" + n.Role + "\n\n" + secondarySections(i)})
	}
	cc := cloudConfig{Stages: map[string][]stage{
		"initramfs": {{Name: name + ": the attached interfaces' names", If: notLive, Files: links}},
		"boot":      {{Name: name + ": the attached interfaces' network", If: notLive, Files: networks, Commands: []string{"networkctl reload"}}},
	}}
	out, err := yaml.Marshal(cc)
	if err != nil {
		return nil, err
	}
	head := fmt.Sprintf("#cloud-config\n# %s's NICs attached to this VM, written by `%s network attach` from %s.\n", name, ctl, interfacesFile)
	return append([]byte(head), out...), nil
}
