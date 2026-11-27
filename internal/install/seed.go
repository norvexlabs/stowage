package install

import (
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // checkTimezone reads zones on a host without its own database

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/kairos"
	"github.com/norvexlabs/stowage/internal/lint"
)

// Machine is a VM's own settings, which its cloud-config gives: its name, its administrator and
// its network.
type Machine struct {
	Hostname string
	User     string
	SSHKey   string // one authorized_keys line
	// Net is its network, valid (validNet): written as networkd's file at every boot, DHCP
	// included, as IPv6 is off unless Net turns it on, and as data (InstallNetworkFile) for the
	// host daemon to answer.
	Net NetConfig
	// SSHAllow are the sources SSH accepts; none is every source. publicSSH: they are this
	// machine's public address alone, as a server on the internet saw it (a cloud's default).
	SSHAllow  []netip.Prefix
	publicSSH bool
	// Timezone is an IANA zone name; empty keeps the OS's, UTC.
	Timezone string
	// Hosts are host records, "IP NAME" lines (parseHosts), which every boot writes into the VM's
	// /etc/hosts and, as data (VMHostsFile), its installs give the stack's containers.
	Hosts []string
	// HostKey is a one-time SSH host key (an OpenSSH private key) the VM serves until its first
	// install, which `vm create --secrets` pins to send the secrets; nil for none.
	HostKey []byte
	// Interfaces are the release's host.interfaces NICs as this machine has them, sorted by role,
	// the primary's addressing Net; none when the release declares none (one NIC, matched by name).
	Interfaces []MachineInterface
	// Install is the disk the installer ISO installs the OS on (`vm create --platform metal`): a
	// device path, or auto (Kairos's: the largest disk); empty for a disk image the machine boots.
	Install string
}

// MachineInterface is one of the release's host.interfaces NICs on a machine: the MAC its role is
// named from, and a secondary NIC's addressing.
type MachineInterface struct {
	Role string
	// MAC is the NIC's; empty, the machine's only NIC the release declares, named from whichever
	// NIC the machine has (an accelerated networking's virtual function left out) and pinned to
	// that NIC's MAC on the first boot (pinScript).
	MAC string
	// Attach is the platform's network the NIC attaches to (TrueNAS's bridge, libvirt's network, …);
	// empty, the primary's.
	Attach string
	// Primary: its addressing is the machine's Net. Else Mode is dhcp (an address, never the
	// default route nor the DNS servers), static (Address, with its prefix) or none (the link up,
	// no address: the stack's own rules use it).
	Primary       bool
	Mode, Address string
}

// The modes of a secondary interface.
const (
	IfaceDHCP   = "dhcp"
	IfaceStatic = "static"
	IfaceNone   = "none"
)

// vfDrivers are the drivers of an accelerated networking's virtual function, as udev records a
// network device's (ID_NET_DRIVER: the network device's own, as ethtool names it, which a .link
// file's Driver= matches — mlx4_en, not its PCI device's mlx4_core): beside the NIC it has the
// NIC's MAC and takes no name of its own. The original-name match leaves them out (vfMatch), as
// does a .link file by MAC unless its NIC's own driver is one of them (driverValue); the VM's NICs
// leave out a NIC another enslaves (deviceNICs), so a physical NIC of one of these drivers is
// named and counted.
var vfDrivers = []string{"mlx4_en", "mlx5_core", "mana"}

// vfDriver reports whether driver is one of vfDrivers.
func vfDriver(driver string) bool { return slices.Contains(vfDrivers, driver) }

// driverValue is the value of the Driver= line of a .link file naming by its MAC the NIC whose
// own driver is driver ("" when not known: a NIC attached before it is there): the one rule of
// the boot's pin (driverValueScript) and `network attach` (macMatch). A driver of vfDrivers (a
// physical NIC of one, on metal: leaving them out would never name it) is matched itself; any
// other, or none known, leaves vfDrivers out — a change of the NIC's model keeping its MAC
// (virtio_net to e1000) still matches, and a virtual function sharing that MAC never does.
func driverValue(driver string) string {
	if vfDriver(driver) {
		return driver
	}
	return "!" + strings.Join(vfDrivers, " ")
}

// vfMatch is the Driver= line leaving vfDrivers out: the original-name match's, and a match by
// MAC's whose NIC's driver is not one of them, or not known.
func vfMatch() string { return "Driver=" + driverValue("") }

// macMatch is the [Match] of a .link file naming by mac the NIC whose own driver is driver
// (driverValue).
func macMatch(mac, driver string) string {
	return "MACAddress=" + mac + "\nDriver=" + driverValue(driver)
}

// driverMatches reports whether a NIC whose driver is driver ("" for none udev records) matches
// a .link file's Driver= value, as udev matches it: a list after "!" matches every driver but
// those, none included; else that driver alone.
func driverMatches(value, driver string) bool {
	if not, ok := strings.CutPrefix(value, "!"); ok {
		return !slices.Contains(strings.Fields(not), driver)
	}
	return driver == value
}

// driverValueScript is driverValue for the boot's shell: it turns v, a NIC's driver as
// nicDriverScript reads it (any text, or empty), into its Driver= value.
func driverValueScript() string {
	return `case "$v" in ` + strings.Join(vfDrivers, "|") + `) ;; *) v='` + driverValue("") + `';; esac`
}

// driverValues is a shell case pattern of every value driverValue gives.
func driverValues() string {
	return `'` + driverValue("") + `'|` + strings.Join(vfDrivers, "|")
}

// interfaceLink is the networkd .link file naming i's NIC by its role: by MAC, or the only NIC's
// original name (vfDrivers left out), which the first boot pins to its MAC and driver (pinScript).
func interfaceLink(name string, i MachineInterface) string {
	match := "MACAddress=" + i.MAC
	if i.MAC == "" {
		match = "OriginalName=en* eth*\n" + vfMatch()
	}
	return linkFile(fmt.Sprintf("%s's interface %s (host.interfaces), named at every boot", name, i.Role), match, i.Role)
}

// linkFile is a .link file naming the NIC match finds role; comment heads it.
func linkFile(comment, match, role string) string {
	return fmt.Sprintf("# %s.\n[Match]\n%s\n\n[Link]\nName=%s\n", comment, match, role)
}

// seedLink is how the VM names a NIC role its seed names: by MAC (the seed's own, or the one the
// first boot pinned: Driver set), or by its original name alone (Original: not pinned). A link
// naming neither (empty, cut short, or not the seed's) is the zero seedLink.
type seedLink struct {
	MAC string
	// Driver is the Driver= value of a link by MAC: the boot's pin writes one (pinnedLink), the
	// seed's own link by MAC never does.
	Driver   string
	Original bool
}

// pinned reports whether the boot's pin names the role: by the MAC and the driver it pinned.
func (l seedLink) pinned() bool { return l.MAC != "" && l.Driver != "" }

// seedLinks are the NIC roles the seed of project name names, read from networkd's directory: the
// seed's .link files (kairos.InterfaceLink), which the first boot's pin rewrites to match by the
// MAC (pinnedLink). The one reader of those files.
func seedLinks(networkd, name string) (map[string]seedLink, error) {
	paths, err := filepath.Glob(filepath.Join(networkd, kairos.InterfaceLink(name, "*")))
	if err != nil {
		return nil, err
	}
	prefix, suffix, _ := strings.Cut(kairos.InterfaceLink(name, "\x00"), "\x00")
	links := map[string]seedLink{}
	for _, path := range paths {
		role := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), prefix), suffix)
		link, err := readLink(path)
		if err != nil {
			return nil, err
		}
		links[role] = link
	}
	return links, nil
}

// readLink reads a .link file's match: the MAC it names its NIC by (lower case) and that match's
// Driver= value, or whether it names it by its original name.
func readLink(path string) (seedLink, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return seedLink{}, err
	}
	var l seedLink
	var driver string
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if m, ok := strings.CutPrefix(line, "MACAddress="); ok {
			l.MAC = strings.ToLower(m)
		} else if d, ok := strings.CutPrefix(line, "Driver="); ok {
			driver = d
		} else if strings.HasPrefix(line, "OriginalName=") {
			l.Original = true
		}
	}
	if l.MAC != "" {
		l.Original, l.Driver = false, driver
	}
	return l, nil
}

// deviceNIC is a NIC of the VM: its kernel name and its MAC (lower case).
type deviceNIC struct{ Name, MAC string }

// deviceNICs are the VM's NICs in sys (the kernel's NIC directory, sysNetDir): the network
// devices with a device (no bridge, veth or tunnel) that no other device enslaves (no master: an
// accelerated networking's virtual function is its NIC's, with that NIC's MAC). deviceNICsScript
// is the same rule for the boot's shell. A NIC gone between the listing and the read of its MAC
// is left out, as gone.
func deviceNICs(sys string) ([]deviceNIC, error) {
	devs, err := filepath.Glob(filepath.Join(sys, "*", "device"))
	if err != nil {
		return nil, err
	}
	var nics []deviceNIC
	for _, d := range devs {
		dir := filepath.Dir(d)
		if _, err := os.Lstat(filepath.Join(dir, "master")); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		addr, err := os.ReadFile(filepath.Join(dir, "address"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		nics = append(nics, deviceNIC{Name: filepath.Base(dir), MAC: strings.ToLower(strings.TrimSpace(string(addr)))})
	}
	return nics, nil
}

// vmNICs is what a hint asks of the VM's NICs (nicsThere).
type vmNICs struct {
	// drivers are the drivers of the VM's NICs that have a MAC: "" for a NIC udev records none
	// of, none when no NIC has the MAC.
	drivers func(mac string) ([]string, error)
	// holder is the MAC of the network device named name on the VM (any device: a NIC, a bridge,
	// a tunnel), held false when none is.
	holder func(name string) (mac string, held bool, err error)
}

// nicsThere answers, for a MAC, the drivers of the VM's NICs (deviceNICs of sys) that have it, as
// nicDriver reads them from udev's records (in udev), and, for a name, the device of sys that
// holds it. The NICs and their drivers are read once, at the first question, whatever the number
// of questions; one that does not read is the error of every answer, one gone since the listing
// left out.
func nicsThere(sys, udev string) vmNICs {
	type nic struct{ mac, driver string }
	var nics []nic
	var err error
	read := false
	drivers := func(mac string) ([]string, error) {
		if !read {
			read = true
			var devs []deviceNIC
			if devs, err = deviceNICs(sys); err == nil {
				for _, n := range devs {
					var d string
					if d, err = nicDriver(sys, udev, n.Name); errors.Is(err, os.ErrNotExist) {
						// Gone since deviceNICs listed it.
						err = nil
						continue
					} else if err != nil {
						break
					}
					nics = append(nics, nic{n.MAC, d})
				}
			}
		}
		if err != nil {
			return nil, err
		}
		var drivers []string
		for _, n := range nics {
			if n.mac == mac {
				drivers = append(drivers, n.driver)
			}
		}
		return drivers, nil
	}
	holder := func(name string) (string, bool, error) {
		addr, err := os.ReadFile(filepath.Join(sys, name, "address"))
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return strings.ToLower(strings.TrimSpace(string(addr))), true, nil
	}
	return vmNICs{drivers, holder}
}

// ownNIC is, of the VM's NICs (devs, in sys and udev) with mac, the one a .link file by mac is
// meant for, and its driver (nicDriver): the only one, else the only one whose driver is not one
// of vfDrivers (an accelerated networking's virtual function beside its NIC, of the NIC's MAC,
// before the NIC enslaves it); "" when none has the MAC. Several left, which one is not known: a
// refusal naming them.
func ownNIC(sys, udev string, devs []deviceNIC, mac string) (dev, driver string, err error) {
	var names, drivers, own []string
	for _, n := range devs {
		if n.MAC != mac {
			continue
		}
		d, err := nicDriver(sys, udev, n.Name)
		if errors.Is(err, os.ErrNotExist) {
			// Gone since deviceNICs listed it.
			continue
		}
		if err != nil {
			return "", "", err
		}
		names, drivers = append(names, n.Name), append(drivers, d)
		if !vfDriver(d) {
			own = append(own, n.Name)
		}
	}
	switch {
	case len(names) == 0:
		return "", "", nil
	case len(names) == 1:
		return names[0], drivers[0], nil
	case len(own) == 1:
		i := slices.Index(names, own[0])
		return names[i], drivers[i], nil
	}
	var each []string
	for i, n := range names {
		each = append(each, fmt.Sprintf("%s (%s)", n, cmp.Or(drivers[i], "no driver udev records")))
	}
	return "", "", refuse("give each NIC its own MAC on the VM's platform, then run this again",
		"--mac %s: the VM's NICs %s all have it, and no one alone is told apart from a virtual function's (%s): which one is the NIC is not known", mac, strings.Join(each, ", "), strings.Join(vfDrivers, ", "))
}

// deviceNICsScript prints the names of the VM's NICs, one a line, by deviceNICs's rule.
func deviceNICsScript() string {
	return `for d in /sys/class/net/*; do [ -e "$d/device" ] || continue; ` +
		`{ [ -e "$d/master" ] || [ -L "$d/master" ]; } && continue; basename "$d"; done`
}

// driverName matches a driver's name a .link file's Driver= takes.
var driverName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// nicDriver is the driver of the VM's NIC dev (in sys, sysNetDir) a .link file's Driver= matches:
// udev's ID_NET_DRIVER in its record of the NIC's ifindex (in udev, udevDataDir) — the network
// device's own, so an accelerated networking's virtual function sharing the NIC's MAC never has
// it. nicDriverScript is the same rule for the boot's shell, which the pin reads it by. A NIC udev
// records no driver of (no record yet), or one no .link file can name, has none (""); an ifindex
// or a record that does not read is an error.
func nicDriver(sys, udev, dev string) (string, error) {
	idx, err := os.ReadFile(filepath.Join(sys, dev, "ifindex"))
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(udev, "n"+strings.TrimSpace(string(idx))))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if d, ok := strings.CutPrefix(line, "E:ID_NET_DRIVER="); ok {
			if !driverName.MatchString(d) {
				return "", nil
			}
			return d, nil
		}
	}
	return "", nil
}

// nicDriverScript is nicDriver's rule for the boot's shell: the driver of the NIC named $1, empty
// with none recorded. A name driverName does not match is no driver of vfDrivers, so the pin
// takes it as nicDriver's "" (driverValueScript, vf).
func nicDriverScript() string {
	return `$(sed -n 's/^E:ID_NET_DRIVER=//p' "/run/udev/data/n$(cat "/sys/class/net/$1/ifindex" 2>/dev/null)" 2>/dev/null | head -n1)`
}

// secondarySections is a secondary NIC's addressing: never the default route's, the DNS servers'
// nor the host name's, and the boot waits for none of them.
func secondarySections(i MachineInterface) string {
	var b strings.Builder
	b.WriteString("[Network]\n")
	switch i.Mode {
	case IfaceDHCP:
		b.WriteString("DHCP=ipv4\n")
	case IfaceStatic:
		fmt.Fprintf(&b, "Address=%s\n", i.Address)
	default:
		b.WriteString("ConfigureWithoutCarrier=yes\n")
	}
	b.WriteString("LinkLocalAddressing=no\nIPv6AcceptRA=no\n")
	if i.Mode == IfaceDHCP {
		// The host's name is the primary's: a DHCP server registering names maps it there alone.
		b.WriteString("\n[DHCPv4]\nUseGateway=no\nUseRoutes=no\nUseDNS=no\nSendHostname=no\n")
	}
	b.WriteString("\n[Link]\nRequiredForOnline=no\n")
	return b.String()
}

// unnamedNetwork is the networkd file of the NICs no role names (kairos.UnnamedUnit), on a VM that
// names its NICs by role: each carries no address — no DHCP, no IPv6 (no router advertisement,
// no link-local address), so no route — and a NIC added to the machine (or a port a cable
// reaches by mistake) carries none of the network beside the roles' NICs until `network attach`
// names it. Kairos's DHCP default would configure it, and a second NIC on the primary's LAN would
// take its routes. The link stays up, and one the kernel enslaved to another (a failover's standby
// or virtual function, at boot or plugged later, a port of a bond made outside networkd) keeps its
// master (KeepMaster: networkd ignores a Bond= of a file sorting after it),
// which carries it. It matches physical NICs alone (Kind=!*, as dracut's default network); an
// accelerated networking's virtual function on Azure is the bundle's 00-<name>-sriov.network's,
// which sorts first and leaves it unmanaged.
func unnamedNetwork(name, ctl string) string {
	return fmt.Sprintf("# %s: a NIC no role names carries no address: `%s network` lists it, `%s network attach` names it.\n", name, ctl, ctl) +
		"[Match]\n" + everyNIC + "\nKind=!*\n\n[Link]\nRequiredForOnline=no\n\n[Network]\nKeepMaster=yes\nDHCP=no\nLinkLocalAddressing=no\nIPv6AcceptRA=no\n"
}

// unnamedIssue is the console banner's line of the NICs left unaddressed, which unnamedStages writes with
// their file.
func unnamedIssue(name string) string { return "/etc/issue.d/07-" + name + "-unnamed.issue" }

// unnamedWaitSecs bounds the boot's wait for udev and for the primary's NIC before unnamedStages
// decide.
const unnamedWaitSecs = 30

// unnamedStages leave the NICs no role names unaddressed (unnamedNetwork, and the banner's line), after
// the renames and the pin, before the network's reload — only while the primary role's NIC is
// named: a primary its rename missed (its NIC replaced, a MAC no longer matching) keeps its
// kernel's name, and unaddressed with the rest it would leave the machine unreachable, recovery
// system included. Then the files go, logged, and Kairos's DHCP default configures every NIC, as
// on a machine naming none.
func unnamedStages(name, ctl, primary string) []stage {
	dev := "/sys/class/net/" + primary
	unit, issue := "/etc/systemd/network/"+kairos.UnnamedUnit(name), unnamedIssue(name)
	// Both are written beside their place under names networkd and agetty do not read, then moved
	// into it, or removed, by one decision.
	return []stage{{
		Name: name + ": the NICs no role names left unaddressed while " + primary + " is named",
		Files: []cloudFile{
			{Path: unit + ".staged", Permissions: 0o644, Content: unnamedNetwork(name, ctl)},
			{Path: issue + ".staged", Permissions: 0o644, Content: fmt.Sprintf("NICs no role names carry no address: `%s network` lists any by its MAC\n", ctl)},
		},
		// A NIC probed late (firmware a driver loads) is named once udev has handled it: the
		// decision waits for that, and for the primary up to unnamedWaitSecs, so a slow primary
		// does not hand every NIC to DHCP for the boot; only a primary never named waits it out.
		// The console's prompt drawn already shows the banner as it was: agetty reads it again.
		Commands: []string{fmt.Sprintf(`udevadm settle --timeout=%[1]d || true; i=0; while [ ! -e %[2]s ] && [ $i -lt %[1]d ]; do sleep 1; i=$((i+1)); done; `+
			`if [ -e %[2]s ]; then mv -f %[3]s.staged %[3]s || echo '%[5]s: %[3]s did not move into place: the DHCP default of Kairos configures the NICs no role names' >&2; mv -f %[4]s.staged %[4]s || true; `+
			`else rm -f %[3]s %[3]s.staged %[4]s %[4]s.staged; echo '%[5]s: no NIC is named %[6]s: none is left unaddressed, and DHCP configures each' >&2; fi; `+
			`agetty --reload 2>/dev/null || true`, unnamedWaitSecs, dev, unit, issue, name, primary)},
	}}
}

// nicFuncsScript defines the boot's shell functions over the VM's NICs, $c holding their names
// (deviceNICsScript): mac and drv print a NIC's MAC and driver (nicDriverScript); vf passes for a
// NIC of a virtual function's driver (vfDrivers); twin for a virtual function beside its NIC —
// vf, with the MAC of a NIC of $c that is not (one not enslaved yet), which is the NIC. The one
// rule of the pin (pinScript) and the seed's rename by MAC (renameScript).
func nicFuncsScript() string {
	return strings.NewReplacer("{NICDRV}", nicDriverScript(), "{VF}", strings.Join(vfDrivers, "|")).Replace(
		`mac() { cat "/sys/class/net/$1/address" 2>/dev/null; }; drv() { printf '%s' "{NICDRV}"; }; ` +
			`vf() { case "$(drv "$1")" in {VF}) return 0;; esac; return 1; }; ` +
			`twin() { vf "$1" || return 1; for e in $c; do [ "$e" != "$1" ] && [ "$(mac "$e")" = "$(mac "$1")" ] && ! vf "$e" && return 0; done; return 1; }; `)
}

// renameScript names i's NIC its role now, when the boot named it before its .link file was
// written (the first boot): renameNIC. A NIC named by its MAC is the VM's NIC of that MAC
// (deviceNICsScript) that is not a virtual function beside it (nicFuncsScript's twin); of several,
// none is renamed, said. One named by its original name alone is the NIC of the default route,
// and only while the role is neither pinned (pinnedCheck: the pin, pinScript, names it from then
// on) nor a NIC's name already: a NIC added later may get the first default route, and is never
// taken for the role.
func renameScript(name string, i MachineInterface) (string, error) {
	rename := renameNIC(name, `"$i"`, i.Role)
	if i.MAC != "" {
		return strings.NewReplacer("{NICS}", deviceNICsScript(), "{FUNCS}", nicFuncsScript(), "{MAC}", i.MAC, "{NAME}", name, "{ROLE}", i.Role, "{RENAME}", rename).Replace(
			`c=$({NICS}); {FUNCS}i=; for d in $c; do [ "$(mac "$d")" = {MAC} ] && ! twin "$d" && i="$i $d"; done; set -- $i; ` +
				`if [ $# -gt 1 ]; then echo "{NAME}: the interface {ROLE}: the NICs $* all have its MAC ({MAC}): which one is its NIC is not known, so none is renamed" >&2; false; ` +
				`else i=$1; [ -z "$i" ] || {RENAME}; fi`), nil
	}
	pinned, err := pinnedCheck(name, i.Role)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`if %s || [ -e /sys/class/net/%s ]; then :; else i=$(ip -o route show default | awk '{for (n = 1; n < NF; n++) if ($n == "dev") {print $(n+1); exit}}'); [ -z "$i" ] || %s; fi`,
		pinned, i.Role, rename), nil
}

// renameNIC is the boot's shell naming the NIC dev (a shell word) role now: down, renamed, up; a
// NIC named so already is left. A name another NIC holds is never taken (the NIC is not taken
// down), and a rename that fails brings the NIC back up: both said, and a failure. The seed's
// rename (renameScript) and the pin's (pinScript).
func renameNIC(name, dev, role string) string {
	return fmt.Sprintf(`{ [ %[1]s = %[2]s ] || if [ -e /sys/class/net/%[2]s ]; then echo "%[3]s: the interface %[2]s: another NIC holds its name: "%[1]s" keeps its own" >&2; false; `+
		`else ip link set dev %[1]s down && { ip link set dev %[1]s name %[2]s || { echo "%[3]s: the interface %[2]s: "%[1]s" is not renamed so: it is brought back up" >&2; ip link set dev %[1]s up; false; }; } && ip link set dev %[2]s up; fi; }`, dev, role, name)
}

// macMark and driverMark stand for the MAC and the Driver= value (driverValue) in pinnedLink and
// the pin's cloud-config, which the boot pinning the NIC replaces.
const (
	macMark    = "@MAC@"
	driverMark = "@DRIVER@"
)

// macGlob matches a MAC as the kernel writes it, as a shell pattern and a basic regular expression.
var macGlob = "[0-9a-f][0-9a-f]" + strings.Repeat(":[0-9a-f][0-9a-f]", 5)

// pinnedLink is the seed's .link file of role (pinnedLinkFile) as the boot pins it: the role's
// NIC matched by its MAC (macMark) and the Driver= value of its own driver (driverMark,
// driverValue of udev's ID_NET_DRIVER of it, read when pinned), as `network attach` matches
// (macMatch): an accelerated networking's virtual function, of the NIC's MAC, is never matched.
func pinnedLink(name, role string) string {
	return linkFile(fmt.Sprintf("%s's interface %s (host.interfaces), pinned to its NIC's MAC by the VM's boot", name, role),
		"MACAddress="+macMark+"\nDriver="+driverMark, role)
}

// pinnedLinkFile is the seed's .link file of role, which the pin replaces: the role has that one
// .link file, matching by MAC.
func pinnedLinkFile(name, role string) string {
	return "/etc/systemd/network/" + kairos.InterfaceLink(name, role)
}

// pinnedOEM is the cloud-config on the OEM partition, which a reset keeps, holding role's
// pinnedLink: every boot after the one that wrote it writes the .link file in its initramfs stage,
// before udev names the NICs, over the seed's. 96_ sorts after the seed's 95_userdata, whose
// stage writing the file comes first (yip walks /oem's files in lexical order, each file's steps
// of a stage after the previous file's) — and that stage skips once pinned (pinnedCheck), so no
// order between the two files decides the result.
func pinnedOEM(name, role string) string { return "/oem/96_" + name + "-" + role + "-pinned.yaml" }

// pinnedOEMContent is pinnedOEM's cloud-config, its MAC macMark and its driver driverMark.
func pinnedOEMContent(name, role string) (string, error) {
	cc := cloudConfig{Stages: map[string][]stage{"initramfs": {{
		Name:  name + ": the interface " + role + " pinned to its NIC's MAC",
		If:    notLive,
		Files: []cloudFile{{Path: pinnedLinkFile(name, role), Permissions: 0o644, Content: pinnedLink(name, role)}},
	}}}}
	out, err := yaml.Marshal(cc)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("#cloud-config\n# %s's NIC %s, pinned to its MAC by the VM's boot.\n%s", name, role, out), nil
}

// pinnedCheck is a shell test passing when role's pin is done: pinnedOEM holds the pin's
// cloud-config of a MAC and a Driver= value (driverValues), whole — an empty or truncated file is
// not a pin. It sets m and v to that MAC and value, which the pin reads (pinScript): the one
// reader of the pin.
func pinnedCheck(name, role string) (string, error) {
	oem, err := pinnedOEMContent(name, role)
	if err != nil {
		return "", err
	}
	return strings.NewReplacer("{OEM}", pinnedOEM(name, role), "{MAC}", macGlob, "{VALUES}", driverValues(), "{CONTENT}", shellQuote(oem), "{MM}", macMark, "{DM}", driverMark).Replace(
		`{ m=$(sed -n 's/^ *MACAddress=\({MAC}\)$/\1/p' {OEM} 2>/dev/null | head -n1); ` +
			`v=$(sed -n 's/^ *Driver=//p' {OEM} 2>/dev/null | head -n1); case "$v" in {VALUES}) ;; *) v=;; esac; ` +
			`[ -n "$m" ] && [ -n "$v" ] && [ "$(printf '%s' {CONTENT} | sed "s/{MM}/$m/;s/{DM}/$v/")" = "$(cat {OEM})" ]; }`), nil
}

// pinScript pins i's NIC, which the seed names by its original name alone, to its MAC
// (pinnedLink, its Driver= value driverValue's), when the VM has exactly one free NIC: a NIC of a
// device (deviceNICsScript) whose MAC no attached NIC's .link file names (attachedOEM's, read at
// boot: another role's NIC is never taken), and not a virtual function beside its NIC
// (nicFuncsScript's twin):
//   - not pinned yet, that NIC has the role's name (the seed's .link file named it);
//   - pinned, a NIC of the VM matching the pin's MAC and Driver= value is named the role now
//     (renameNIC), when it is not already: one whose name another NIC held keeps its own, said;
//   - pinned, but no NIC of the VM matches the pin's MAC and Driver= value as udev matches them
//     (the NIC was replaced), that NIC is pinned in its place, and named the role now
//     (renameNIC): the pin's .link file, written before udev named the NICs, did not match it.
//
// It rewrites the seed's .link file now, then writes the OEM partition's cloud-config, synced
// before and after its rename, which marks the pin done: every later boot leaves it while its NIC
// is there, and a NIC added later, in any order, never takes the role's name. A boot that cannot
// pin says why on its log (with no free NIC, or several, a pin whose NIC is gone is left as it
// is), and the next boot tries again; a pin that does not read whole (pinnedCheck, which reads
// the pin's MAC and value for this script) is written again.
func pinScript(name string, i MachineInterface) (string, error) {
	oem, err := pinnedOEMContent(name, i.Role)
	if err != nil {
		return "", err
	}
	pinned, err := pinnedCheck(name, i.Role)
	if err != nil {
		return "", err
	}
	return strings.NewReplacer("{OEM}", pinnedOEM(name, i.Role), "{ATTACHED}", "/oem/"+attachedOEMName(name), "{ROLE}", i.Role, "{NAME}", name, "{MAC}", macGlob,
		"{NICS}", deviceNICsScript(), "{PINNED}", pinned, "{LINK}", shellQuote(pinnedLink(name, i.Role)), "{LINKFILE}", pinnedLinkFile(name, i.Role),
		"{CONTENT}", shellQuote(oem), "{MM}", macMark, "{DM}", driverMark, "{FUNCS}", nicFuncsScript(),
		"{VALUE}", driverValueScript(), "{RENAME}", renameNIC(name, `"$n"`, i.Role), "{RENAMEW}", renameNIC(name, `"$w"`, i.Role)).Replace(
		`f={OEM}; {FUNCS}` +
			// matches passes for a NIC the pin's .link file, of MAC $2 and Driver= value $3, names.
			`matches() { [ "$(mac "$1")" = "$2" ] || return 1; case "$3" in '!'*) ! vf "$1";; *) [ "$(drv "$1")" = "$3" ];; esac; }; ` +
			`c=$({NICS}); ` +
			// a: the MACs the attached NICs' .link files name, another role's.
			`a=" $(sed -n 's/^ *MACAddress=\({MAC}\)$/\1/p' {ATTACHED} 2>/dev/null | tr '\n' ' ')"; ` +
			`fr=; for d in $c; do case "$a" in *" $(mac "$d") "*) continue;; esac; ` +
			`twin "$d" && continue; ` +
			`fr="$fr $d"; done; set -- $fr; l=$(echo $fr); l=${l:-none}; n=; ` +
			`if {PINNED}; then p="$m, Driver=$v"; ` +
			`w=; for d in $c; do matches "$d" "$m" "$v" && w=$d; done; ` +
			`if [ -n "$w" ]; then {RENAMEW}; ` +
			`elif [ $# -eq 1 ]; then n=$1; echo "{NAME}: the interface {ROLE}'s pinned NIC ($p) is not here: the only NIC here no other role names, $1, is pinned in its place" >&2; ` +
			`else echo "{NAME}: the interface {ROLE}'s pinned NIC ($p) is not here, and the NICs here no other role names are $l, not one: its pin is left as it is" >&2; fi; ` +
			`elif [ "$l" = {ROLE} ]; then n={ROLE}; ` +
			`else echo "{NAME}: the interface {ROLE} is not pinned to its NIC's MAC: the NICs here no other role names are $l, not one NIC named {ROLE}" >&2; fi; ` +
			`if [ -n "$n" ]; then m=$(mac "$n"); v=$(drv "$n"); {VALUE}; case "$m" in {MAC}) ;; *) m=;; esac; ` +
			`if [ -z "$m" ]; then echo "{NAME}: the interface {ROLE} is not pinned to its NIC's MAC: the NIC $n has no MAC" >&2; ` +
			`else printf '%s' {LINK} | sed "s/{MM}/$m/;s/{DM}/$v/" > {LINKFILE} && ` +
			`printf '%s' {CONTENT} | sed "s/{MM}/$m/;s/{DM}/$v/" > "$f.tmp" && sync "$f.tmp" && mv -f "$f.tmp" "$f" && sync "${f%/*}" && ` +
			`{RENAME}; fi; fi`), nil
}

// staticAddr is the VM's static IPv4 address, without its prefix; empty with DHCP.
func (m Machine) staticAddr() string {
	if m.Net.Mode != "static" {
		return ""
	}
	p, _ := netip.ParsePrefix(m.Net.Address)
	return p.Addr().String()
}

// staticAddr6 is the VM's static IPv6 address, without its prefix; empty without one.
func (m Machine) staticAddr6() string {
	if m.Net.Address6 == "" {
		return ""
	}
	p, _ := netip.ParsePrefix(m.Net.Address6)
	return p.Addr().String()
}

// connectIssue is the console's connect line: \4 is the VM's IPv4 address, \6 its IPv6 one, shown
// when IPv6 is on.
func connectIssue(m Machine, ctl string) string {
	s := fmt.Sprintf("connect: ssh %s@\\4, then `%s status`\n", m.User, ctl)
	if m.Net.IPv6 {
		s += fmt.Sprintf("         or ssh %s@\\6\n", m.User)
	}
	return s + "\n"
}

// The cloud-config Kairos reads (yip's stages): written at every boot, as /etc is not kept.
type cloudConfig struct {
	Install *cloudInstall      `yaml:"install,omitempty"`
	Stages  map[string][]stage `yaml:"stages"`
}

// cloudInstall is Kairos's automated install from its installer ISO, merged with the ISO's own
// (the partitions' sizes, the console). It powers the machine off once installed, so the
// installer's medium cannot boot again; the cloud-config it ran with is kept as the installed
// system's first-boot user-data (/oem/95_userdata, the seed's after-install-chroot stage), so no
// boot pulls the seed again and its stages run once a boot.
type cloudInstall struct {
	Auto     bool   `yaml:"auto"`
	Device   string `yaml:"device"`
	Poweroff bool   `yaml:"poweroff"`
}

// afterInstallMove moves the cloud-config the installer kept (one 9N_custom.yaml a source it read,
// 90 the first) into 95_userdata, the first as its userdata.yaml; the stage fails without one.
const afterInstallMove = `set -e; ls /oem/9[0-9]_custom.yaml >/dev/null; mkdir -p /oem/95_userdata; ` +
	`for f in /oem/9[0-9]_custom.yaml; do n=$(basename "$f"); [ "$n" = 90_custom.yaml ] && n=userdata.yaml; mv -f "$f" "/oem/95_userdata/$n"; done`

// notLive is the test of a stage that runs on the installed system only: the installer ISO boots
// the OS live, with the seed, to install it.
const notLive = "[ ! -e /run/cos/live_mode ]"

type stage struct {
	Name     string               `yaml:"name"`
	If       string               `yaml:"if,omitempty"` // a shell test: the step runs when it holds
	Hostname string               `yaml:"hostname,omitempty"`
	Users    map[string]cloudUser `yaml:"users,omitempty"`
	Files    []cloudFile          `yaml:"files,omitempty"`
	// Directories a step's files go in, made before them with their permissions: yip makes a
	// file's missing parent 0744, which only root can search.
	Directories []cloudDir `yaml:"directories,omitempty"`
	Commands    []string   `yaml:"commands,omitempty"`
}

type cloudDir struct {
	Path        string `yaml:"path"`
	Permissions uint32 `yaml:"permissions"`
}

type cloudUser struct {
	Groups            []string `yaml:"groups"`
	Homedir           string   `yaml:"homedir"`
	Shell             string   `yaml:"shell"`
	Passwd            string   `yaml:"passwd"`
	SSHAuthorizedKeys []string `yaml:"ssh_authorized_keys"`
}

type cloudFile struct {
	Path        string `yaml:"path"`
	Permissions uint32 `yaml:"permissions"`
	Content     string `yaml:"content"`
}

// UserData is the VM's cloud-config: its hostname (set before the network starts); its administrator, in the admin group (the host
// daemon's socket and sudo without a password), who logs in with the SSH key alone; its network,
// DHCP included, as networkd's file and as a record the host daemon answers (InstallNetworkFile),
// both at every boot; and the first install's settings (SettingsFile), which the boot
// installs from. With ca, the CA of the registry the settings name (a mirror's) goes to
// VMRegistryCA and into the VM's trust store, at every boot, before the install reads it. ctl is
// the installer its comments and console banner name. It carries no secret, but for
// m.HostKey: written, while the install does not exist and the key was never retired, to a file under /run (gone at the next
// boot) that a drop-in of sshd names first, then the system's own keys (seedHostKeyFiles).
func UserData(name, ctl string, m Machine, st Settings, ca []byte) ([]byte, error) {
	settings, err := yaml.Marshal(st)
	if err != nil {
		return nil, err
	}
	boot := []stage{{
		Name: name + ": the administrator and the first install",
		Users: map[string]cloudUser{m.User: {
			Groups:            []string{AdminGroup},
			Homedir:           "/home/" + m.User,
			Shell:             "/bin/bash",
			Passwd:            "!", // no password: the SSH key alone logs in
			SSHAuthorizedKeys: []string{m.SSHKey},
		}},
		Files: []cloudFile{
			{Path: SettingsFile(name), Permissions: 0o600, Content: string(settings)},
			// The console's login screen tells how to reach the VM; \4 is its address.
			{Path: "/etc/issue.d/07-" + name + "-connect.issue", Permissions: 0o644, Content: connectIssue(m, ctl)},
		},
	}, {
		// A step of its own, after the one that creates the user (yip runs a step's commands
		// before its users): yip only sets the key and password of a user the base already has
		// (ubuntu's own), so the group is given here, either way.
		Name:     name + ": the administrator in the " + AdminGroup + " group",
		Commands: []string{"usermod -a -G " + AdminGroup + " " + m.User},
	}}
	if ca != nil {
		boot = append(boot, stage{
			Name:  name + ": the registry's CA",
			Files: []cloudFile{{Path: VMRegistryCA(name), Permissions: 0o644, Content: string(ca)}},
		}, stage{
			// A step of its own, after the one that writes the file.
			Name:     name + ": the registry's CA in the trust store",
			Commands: []string{"update-ca-certificates"},
		})
	}
	if len(m.SSHAllow) > 0 {
		// At every boot (/etc/nftables.d is not kept); once `ssh-allow set` recorded sources of its
		// own (SSHAllowFile), the boot unit that runs `ssh-boot` after this stage writes them over.
		boot = append(boot, stage{
			Name:  name + ": SSH's sources",
			Files: []cloudFile{{Path: SSHRules, Permissions: 0o644, Content: sshRules(fmt.Sprintf("SSH, from the sources `%s vm create --ssh-allow` gave.", ctl), m.SSHAllow)}},
		}, stage{
			// A step of its own, after the one that writes the file. The ruleset flushes its own
			// table only: Docker's rules stay (a restart of nftables.service would flush them all).
			Name:     name + ": the firewall with SSH's sources",
			Commands: []string{"nft -f " + FirewallConf},
		})
	}
	if m.Timezone != "" {
		boot = append(boot, stage{
			Name: name + ": the time zone",
			// The zone is checked against this host's database: one the VM's lacks stays UTC, said.
			Commands: []string{fmt.Sprintf("if [ -f /usr/share/zoneinfo/%[1]s ]; then ln -sf /usr/share/zoneinfo/%[1]s /etc/localtime && echo %[1]s > /etc/timezone; else echo 'no time zone %[1]s on this OS: it stays UTC' >&2; fi", m.Timezone)},
		})
	}
	if len(m.Hosts) > 0 {
		mark := " # " + name + ": vm create --add-host"
		boot = append(boot, stage{
			Name:  name + ": the host records",
			Files: []cloudFile{{Path: VMHostsFile(name), Permissions: 0o644, Content: strings.Join(m.Hosts, "\n") + "\n"}},
		}, stage{
			// A step of its own, after the one that writes the file: the records replace those an
			// earlier boot added, marked.
			Name:     name + ": the host records in /etc/hosts",
			Commands: []string{fmt.Sprintf("sed -i '/%s$/d' /etc/hosts && sed 's/$/%s/' %s >> /etc/hosts", mark, mark, VMHostsFile(name))},
		})
	}
	netYAML, err := yaml.Marshal(m.Net)
	if err != nil {
		return nil, err
	}
	match := everyNIC
	var links, secondaries []cloudFile
	var renames []string
	// pin is the boot's pin of the NIC the seed names by its original name alone, and pinned the
	// test that it is done: one at most, as a NIC goes without a MAC only as the machine's only
	// one (machineInterfaces), which a seed of several NICs, each by its MAC, never has.
	var pin, pinned string
	var primary string // the primary's role, when the machine names its NICs by role
	for _, i := range m.Interfaces {
		links = append(links, cloudFile{Path: "/etc/systemd/network/" + kairos.InterfaceLink(name, i.Role), Permissions: 0o644, Content: interfaceLink(name, i)})
		rename, err := renameScript(name, i)
		if err != nil {
			return nil, err
		}
		renames = append(renames, rename)
		if i.MAC == "" {
			if len(m.Interfaces) > 1 {
				return nil, fmt.Errorf("interface %s: no MAC, and the machine has %d NICs: only the only one goes without", i.Role, len(m.Interfaces))
			}
			if pin, err = pinScript(name, i); err != nil {
				return nil, err
			}
			if pinned, err = pinnedCheck(name, i.Role); err != nil {
				return nil, err
			}
		}
		if i.Primary {
			match, primary = "Name="+i.Role, i.Role
			continue
		}
		secondaries = append(secondaries, cloudFile{Path: "/etc/systemd/network/" + kairos.InterfaceUnit(name, i.Role), Permissions: 0o644,
			Content: "[Match]\nName=" + i.Role + "\n\n" + secondarySections(i)})
	}
	if len(m.Interfaces) > 0 {
		boot = append(boot, stage{
			// A step of its own, before the network's: its files match the NICs by their roles.
			Name:     name + ": the interfaces named by their roles",
			Commands: renames,
		})
	}
	if pin != "" {
		boot = append(boot, stage{
			// After the renames: the NIC pinned has the role's name, or takes it (one replacing the
			// NIC pinned, which the pin's .link file did not match).
			Name:     name + ": the interface named by its original name pinned to its MAC",
			Commands: []string{pin},
		})
	}
	if primary != "" {
		boot = append(boot, unnamedStages(name, ctl, primary)...)
	}
	boot = append(boot, stage{
		Name: name + ": the network",
		Files: append([]cloudFile{
			// 05- sorts before Kairos's 20-dhcp.network, and after the override `network set`
			// makes: networkd takes the first file that matches.
			{Path: "/etc/systemd/network/" + staticUnit(name), Permissions: 0o644, Content: "[Match]\n" + match + "\n\n" + networkSections(m.Net)},
			// The same network as data, for the host daemon to answer what the install set.
			{Path: InstallNetworkFile(name), Permissions: 0o644, Content: string(netYAML)},
		}, secondaries...),
		Commands: []string{"networkctl reload"},
	})
	if st.AwaitGiven != "" {
		boot[0].Files = append(boot[0].Files, cloudFile{Path: AwaitGivenFile(SettingsFile(name)), Permissions: 0o600, Content: st.AwaitGiven + "\n"})
	}
	// The host name again on the recovery system, first in the boot stage: Kairos's recovery
	// cloud-config names it cos-recovery in that stage, before this one. The recovery system keeps
	// the VM's name, which this stage's network step then sends with its DHCP request, so a DHCP
	// server registering names maps it to the recovery system as to the VM.
	boot = append([]stage{{Name: name + ": the host name, the recovery system's too", If: "[ -f /run/cos/recovery_mode ]", Hostname: m.Hostname}}, boot...)
	if m.HostKey != nil {
		key, dropIn, retired := seedHostKeyFiles(name)
		before := "[ ! -e " + VMDir(name) + "/" + envName + " ] && [ ! -e " + retired + " ]"
		boot = append(boot, stage{
			Name: name + ": the one-time SSH host key, until the install",
			If:   before,
			Files: []cloudFile{
				{Path: key, Permissions: 0o600, Content: string(m.HostKey)},
				{Path: dropIn, Permissions: 0o644, Content: "# The one-time host key `" + ctl + " vm create` pins, served until the install.\nHostKey " + key + "\nHostKey /etc/ssh/ssh_host_ed25519_key\nHostKey /etc/ssh/ssh_host_ecdsa_key\nHostKey /etc/ssh/ssh_host_rsa_key\n"},
			},
		}, stage{
			// A step of its own, after the one that writes the files; an sshd not started yet reads them.
			Name:     name + ": sshd with the one-time host key",
			If:       before,
			Commands: []string{sshdReload},
		}, stage{
			Name:     name + ": the one-time SSH host key retired after the install",
			If:       "[ -e " + VMDir(name) + "/" + envName + " ] && [ ! -e " + retired + " ]",
			Commands: []string{retireSteps(name)},
		})
	}
	// The host name is set in the initramfs stage, before networkd starts: its DHCP request then
	// carries it, and a DHCP server registering names registers this one.
	// With it, networkd's DHCP client identity: a DUID of the NIC's MAC, not of the machine-id,
	// for DHCPv4 and DHCPv6, so the VM keeps its leases on the recovery system, whose machine-id
	// is its own; Kairos's DHCP file, which configures the boot's first exchange (before the boot
	// stage writes the seed's network), takes it too rather than the bare MAC, so the booted
	// system holds one lease (the initrd's own DHCP, before any stage, keeps its identity). networkd reads both when it starts, after this stage, as its own user: their
	// directories, which the image does not ship, are made searchable by all.
	initramfs := []stage{{Name: name + ": the host name", Hostname: m.Hostname,
		Directories: []cloudDir{{Path: path.Dir(DUIDConf(name)), Permissions: 0o755},
			{Path: path.Dir(KairosDHCPDropIn(name, "20-dhcp")), Permissions: 0o755},
			{Path: path.Dir(KairosDHCPDropIn(name, "20-dhcp-legacy")), Permissions: 0o755}},
		Files: []cloudFile{{Path: DUIDConf(name), Permissions: 0o644, Content: duidConf},
			{Path: KairosDHCPDropIn(name, "20-dhcp"), Permissions: 0o644, Content: kairosDHCPDUID},
			{Path: KairosDHCPDropIn(name, "20-dhcp-legacy"), Permissions: 0o644, Content: kairosDHCPDUID}}}}
	if len(links) > 0 {
		// Before the booted system's udev names the NICs: each by its role from then on.
		names := stage{Name: name + ": the interfaces' names", Files: links}
		if pinned != "" {
			// Once pinned, the pin's own cloud-config (pinnedOEM) writes the one .link file, by MAC.
			names.If = "! " + pinned
		}
		initramfs = append(initramfs, names)
	}
	cc := cloudConfig{Stages: map[string][]stage{"initramfs": initramfs, "boot": boot}}
	for _, ss := range cc.Stages {
		for i := range ss {
			ss[i].If = strings.TrimSuffix(notLive+" && "+ss[i].If, " && ")
		}
	}
	if m.Install != "" {
		cc.Install = &cloudInstall{Auto: true, Device: m.Install, Poweroff: true}
		// The installer keeps the cloud-config it ran with as the installed system's 90_custom.yaml;
		// as 95_userdata (the directory a datasource fills, its userdata.yaml) it is the first
		// boot's cloud-config kept, as on a VM: no boot pulls the seed again, its medium left in or
		// not, nor runs its stages twice. The chroot's stage sees the installed system's OEM
		// partition as /oem (the installer's own mounts it under /run/cos/oem).
		cc.Stages["after-install-chroot"] = []stage{{
			Name:     name + ": the cloud-config kept as the user-data the first boot pulled",
			Commands: []string{afterInstallMove},
		}}
	}
	out, err := yaml.Marshal(cc)
	if err != nil {
		return nil, err
	}
	carries := "It carries no secret."
	if m.HostKey != nil {
		carries = "It carries no secret but a one-time SSH host key, served until the install."
	}
	head := fmt.Sprintf("#cloud-config\n# %s's VM %s, written by `%s vm create`. %s\n", name, m.Hostname, ctl, carries)
	return append([]byte(head), out...), nil
}

// DUIDConf is the networkd.conf drop-in a VM's seed writes: its DHCP clients' DUID.
func DUIDConf(name string) string { return "/etc/systemd/networkd.conf.d/10-" + name + "-duid.conf" }

// duidConf makes every DHCP client of the VM name itself by a DUID of its NIC's MAC
// (systemd.networkd.conf: [DHCPv4] and [DHCPv6] DUIDType=link-layer).
const duidConf = "[DHCPv4]\nDUIDType=link-layer\n\n[DHCPv6]\nDUIDType=link-layer\n"

// KairosDHCPDropIn is the drop-in a VM's seed writes for one of Kairos's DHCP files (unit:
// 20-dhcp, for en* names, or 20-dhcp-legacy, for eth*), whose ClientIdentifier=mac it replaces
// with the DUID's.
func KairosDHCPDropIn(name, unit string) string {
	return "/etc/systemd/network/" + unit + ".network.d/10-" + name + "-duid.conf"
}

const kairosDHCPDUID = "[DHCPv4]\nClientIdentifier=duid\n"

// seedHostKeyFiles are, on the VM, the one-time host key's file, the sshd drop-in naming it, and
// the mark of its retirement on the OEM partition, which a reset keeps and every system mounts:
// no boot serves the key past it, not one whose install is gone, nor the recovery system's.
func seedHostKeyFiles(name string) (key, dropIn, retired string) {
	return "/run/" + name + "-seed-host-key", "/etc/ssh/sshd_config.d/01-" + name + "-seed-host-key.conf", "/oem/" + name + "-seed-host-key.retired"
}

// retireSteps are the boot's shell steps retiring the seed's one-time host key of the project
// name, as root: its file and sshd drop-in removed, its retirement marked on the OEM partition,
// sshd reloaded if it runs (one not started yet reads its configuration as it starts: the reload's
// status is not judged).
func retireSteps(name string) string {
	return retireFiles(name, false) + " && " + sshdReload
}

// retireFiles are the retirement's file steps (retireSteps', retireStepsChecked's), each under sudo
// (over SSH, as the VM's user) or as it is (as root).
func retireFiles(name string, sudo bool) string {
	key, dropIn, retired := seedHostKeyFiles(name)
	s := ""
	if sudo {
		s = "sudo "
	}
	return s + "rm -f " + dropIn + " " + key + " && " + s + "touch " + retired
}

// retireStepsChecked are host-key's (on the VM, and vm host-key and vm create over SSH, under sudo):
// retireSteps' file steps, failing the list when they fail, and sshd's reload checked
// (sshdReloadChecked), printing sshdNotReloaded when it did not take, the list then exiting 0, as
// the key is retired on disk and the reload alone is left.
func retireStepsChecked(name string, sudo bool) string {
	reload := sshdReloadChecked
	if sudo {
		reload = "sudo sh -c '" + sshdReloadChecked + "'"
	}
	return retireFiles(name, sudo) + " && { " + reload + " || echo " + sshdNotReloaded + "; }"
}

// sshdReloadChecked reloads the running sshd (ssh.service or sshd.service) and fails when none
// took it: a running unit's reload failing, or none running and no ssh.socket to start one (a
// socket-started sshd reads its configuration at the next connection).
const sshdReloadChecked = `s=; for u in ssh.service sshd.service; do if systemctl is-active -q $u; then if systemctl reload-or-restart $u; then s=ok; else s=failed; break; fi; fi; done; [ "$s" = ok ] || { [ -z "$s" ] && systemctl is-active -q ssh.socket; }`

// sshdNotReloaded is the line retireStepsChecked prints when sshd's reload did not take.
const sshdNotReloaded = "stowage-sshd-not-reloaded"

// sshdReload makes a running sshd read its configuration again (Debian's unit is ssh.service,
// others' sshd.service); one not running reads it when it starts. Grouped: in `a && b &&
// sshdReload` the list's status stays a's or b's failure, and the reload's own is always 0.
const sshdReload = "{ for u in ssh.service sshd.service; do systemctl try-reload-or-restart $u 2>/dev/null; done; true; }"

// SSHRules is the firewall's SSH rule file on the VM: the bundle's opens SSH to every source; a
// cloud-config with Machine.SSHAllow replaces it at every boot (/etc/nftables.d is not kept); once
// `ssh-allow set` recorded sources of its own (SSHAllowFile), `ssh-boot` writes them over at every
// boot after the cloud-config's stage.
const SSHRules = "/etc/nftables.d/ssh.nft"

// zoneName is an IANA time zone name, as /usr/share/zoneinfo names its files.
var zoneName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+)*$`)

// checkTimezone takes an IANA time zone name the Go time zone database knows.
func checkTimezone(v string) error {
	// Local is Go's own name for the host's zone; the others are the database's files of no zone,
	// and its leap-second and duplicate trees.
	if !zoneName.MatchString(v) || len(v) > 64 || slices.Contains([]string{"Local", "localtime", "posixrules", "Factory"}, v) ||
		strings.HasPrefix(v, "right/") || strings.HasPrefix(v, "posix/") {
		return fmt.Errorf("%q is not a time zone name (Europe/Paris, UTC)", v)
	}
	if _, err := time.LoadLocation(v); err != nil {
		return fmt.Errorf("%q is not a time zone: %w", v, err)
	}
	return nil
}

// maxHosts bounds vm create's host records.
const maxHosts = 64

// parseHosts reads vm create's --add-host NAME=IP records into /etc/hosts lines ("IP NAME"), sorted:
// NAME a lower-case host name whose last label is not all digits (no IP address), IP one without
// a zone, IPv6 only with ipv6 (the VM has none else); the same record twice is refused.
func parseHosts(flags []string, ipv6 bool) ([]string, error) {
	if len(flags) > maxHosts {
		return nil, fmt.Errorf("--add-host: %d records, at most %d", len(flags), maxHosts)
	}
	var out []string
	for _, f := range flags {
		name, ip, ok := strings.Cut(f, "=")
		if !ok {
			return nil, fmt.Errorf("--add-host %q: NAME=IP, as peer.example.org=192.0.2.10", f)
		}
		if _, err := netip.ParseAddr(name); err == nil || name == "" || lint.CheckL4Address(name) != nil {
			return nil, fmt.Errorf("--add-host %q: %q is not a host name (lower-case labels of letters, digits and -, the last not all digits)", f, name)
		}
		a, err := netip.ParseAddr(ip)
		switch {
		case err != nil || a.Zone() != "":
			return nil, fmt.Errorf("--add-host %q: %q is not an IP address (without a zone)", f, ip)
		case a.Is6() && !a.Is4In6() && !ipv6:
			return nil, fmt.Errorf("--add-host %q: an IPv6 address, and the VM has no IPv6 without --ipv6", f)
		}
		line := a.Unmap().String() + " " + name
		if slices.Contains(out, line) {
			return nil, fmt.Errorf("--add-host %q: given twice", f)
		}
		out = append(out, line)
	}
	slices.Sort(out)
	return out, nil
}

// MetaData is the NoCloud meta-data of the VM: its instance, named after its host.
func MetaData(m Machine) []byte {
	return fmt.Appendf(nil, "instance-id: %s\nlocal-hostname: %s\n", m.Hostname, m.Hostname)
}

// SeedVolume is the label a NoCloud seed's volume carries, which Kairos's cdrom datasource looks
// for.
const SeedVolume = "cidata"

// WriteSeedISO writes a NoCloud seed at path, of mode (0644: a hypervisor running as its own user
// reads it, when it carries no secret): an ISO 9660 image (Rock Ridge and Joliet names)
// labelled SeedVolume, holding user-data and meta-data, which a hypervisor attaches as a CD-ROM.
// The file is the sectors the volume declares, then seedPad sectors of zeros: VMware Workstation's CD-ROM reads no image that ends short of a whole sector.
func WriteSeedISO(path string, userData, metaData []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	work, err := os.MkdirTemp(dir, ".seed-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	tmp := filepath.Join(work, "seed.iso")
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil { // whatever the umask
		f.Close()
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp, path)
		}
	}()
	staging := filepath.Join(work, "files")
	if err := os.Mkdir(staging, 0o700); err != nil {
		return err
	}
	fs, err := iso9660.Create(file.New(f, false), 0, 0, 2048, staging)
	if err != nil {
		return err
	}
	for _, e := range []struct {
		name string
		data []byte
	}{{"user-data", userData}, {"meta-data", metaData}} {
		w, err := fs.OpenFile("/"+e.name, os.O_CREATE|os.O_RDWR)
		if err != nil {
			return err
		}
		if _, err := w.Write(e.data); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
	}
	// Padded with spaces, as ISO 9660 pads it, which blkid trims: the label reads SeedVolume.
	if err := fs.Finalize(iso9660.FinalizeOptions{RockRidge: true, Joliet: true, VolumeIdentifier: fmt.Sprintf("%-32s", SeedVolume)}); err != nil {
		return err
	}
	// go-diskfs may end the file inside the last sector its volume declares (VMware Workstation's
	// CD-ROM then cannot read the image at all: the guest cannot read its first sector), or write
	// a sector of zeros past it. The file is cut to the declared volume, then seedPad sectors of
	// zeros, as Linux reads ahead past a CD's last sector.
	st, err := f.Stat()
	if err != nil {
		return err
	}
	pvd := make([]byte, 4)
	if _, err := f.ReadAt(pvd, seedVolumeSize); err != nil {
		return fmt.Errorf("reading the seed's volume size: %w", err)
	}
	vol := int64(binary.LittleEndian.Uint32(pvd)) * seedSector
	if vol <= seedVolumeSize || vol < st.Size()-seedSector || vol > st.Size()+seedSector {
		return fmt.Errorf("the seed's volume declares %d bytes, its file holds %d", vol, st.Size())
	}
	return f.Truncate(vol + seedPad*seedSector)
}

// seedSector is a CD's sector, seedPad the sectors of zeros after the seed's volume, and
// seedVolumeSize the offset of the volume's size in sectors (little-endian) in its primary
// volume descriptor (sector 16).
const (
	seedSector     = 2048
	seedPad        = 150
	seedVolumeSize = 16*seedSector + 80
)
