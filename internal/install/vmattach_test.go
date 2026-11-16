package install

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/spec"
)

// attachDirs moves the networkd, OEM, kernel NIC and udev record directories into a test's own:
// the VM's seed named uplink by its original name, its first boot pinned it to its NIC's MAC and
// driver (the seed's .link file rewritten), and the NIC named so has that MAC, 02:00:00:00:00:aa.
func attachDirs(t *testing.T) (networkd, oem string) {
	networkd, oem, sys, udev := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	saved := [4]string{networkdDir, oemDir, sysNetDir, udevDataDir}
	networkdDir, oemDir, sysNetDir, udevDataDir = networkd, oem, sys, udev
	t.Cleanup(func() { networkdDir, oemDir, sysNetDir, udevDataDir = saved[0], saved[1], saved[2], saved[3] })
	pin := strings.NewReplacer(macMark, "02:00:00:00:00:aa", driverMark, "virtio_net").Replace(pinnedLink("notes", "uplink"))
	if err := os.WriteFile(filepath.Join(networkd, "10-notes-uplink.link"), []byte(pin), 0o644); err != nil {
		t.Fatal(err)
	}
	sysNIC(t, "uplink", "02:00:00:00:00:aa")
	return networkd, oem
}

// sysVF puts a virtual function named dev with mac in the test's kernel NIC directory, enslaved
// to uplink.
func sysVF(t *testing.T, dev, mac string) {
	t.Helper()
	sysNIC(t, dev, mac)
	if err := os.Symlink("../uplink", filepath.Join(sysNetDir, dev, "master")); err != nil {
		t.Fatal(err)
	}
}

// sysNIC puts a NIC of a device named dev with mac in the test's kernel NIC directory, its
// ifindex one of its own and no driver udev records.
func sysNIC(t *testing.T, dev, mac string) {
	t.Helper()
	ents, err := os.ReadDir(sysNetDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sysNetDir, dev, "device"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysNetDir, dev, "address"), []byte(mac+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysNetDir, dev, "ifindex"), fmt.Appendf(nil, "%d\n", len(ents)+2), 0o644); err != nil {
		t.Fatal(err)
	}
}

// sysDriver records driver as udev's ID_NET_DRIVER of the test's NIC dev.
func sysDriver(t *testing.T, dev, driver string) {
	t.Helper()
	idx, err := os.ReadFile(filepath.Join(sysNetDir, dev, "ifindex"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(udevDataDir, "n"+strings.TrimSpace(string(idx))), []byte("I:12\nE:ID_NET_DRIVER="+driver+"\nE:ID_NET_NAME_MAC=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// attachedLinkOf is the .link file of role in the attached NICs' cloud-config on oem.
func attachedLinkOf(t *testing.T, oem, role string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(oem, "96_notes-interfaces.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cc cloudConfig
	if err := yaml.Unmarshal(raw, &cc); err != nil {
		t.Fatal(err)
	}
	for _, st := range cc.Stages["initramfs"] {
		for _, f := range st.Files {
			if f.Path == "/etc/systemd/network/"+attachedLink("notes", role) {
				return f.Content
			}
		}
	}
	t.Fatalf("no link of %s in:\n%s", role, raw)
	return ""
}

func attachRelease(t *testing.T) *Release {
	r, _ := testInstallerWith(t, "1.0.0", func(s *spec.Spec) {
		s.Host = &spec.Host{Interfaces: map[string]spec.HostInterface{"uplink": {Primary: true}, "edge": {}, "lan": {}, "vpn0": {Kind: spec.InterfaceTUN}}}
	})
	return r
}

// A NIC attached to an installed VM is named by its MAC in the initramfs stage of a cloud-config
// on the OEM partition, and addressed at boot.
func TestANICAttachedIsNamedFromTheNextBoot(t *testing.T) {
	networkd, oem := attachDirs(t)
	r, dir := attachRelease(t), t.TempDir()
	// The NIC is there already, under the kernel's name: the platform attached it and the VM
	// restarted to take it, before the VM names it.
	sysNIC(t, "eth1", "02:00:00:00:00:01")
	sysDriver(t, "eth1", "virtio_net")
	// A bridge, a veth or a tunnel has no device: not a NIC the advice looks for.
	if err := os.MkdirAll(filepath.Join(sysNetDir, "docker0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysNetDir, "docker0", "address"), []byte("02:42:00:00:00:01\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := AttachNIC(r, dir, NetAttach{Role: "edge", MAC: "02:00:00:00:00:01", Mode: IfaceStatic, Address: "192.0.2.10/24"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "edge: the NIC 02:00:00:00:00:01 is named and addressed (static 192.0.2.10/24) from the next boot: the NIC is there: `sudo notes-stow reboot` names it\n") {
		t.Errorf("out %q", out.String())
	}
	cc, err := os.ReadFile(filepath.Join(oem, "96_notes-interfaces.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#cloud-config\n", "initramfs:", "/etc/systemd/network/09-notes-edge.link", "MACAddress=02:00:00:00:00:01", "Name=edge",
		"/etc/systemd/network/10-notes-edge.network", "Address=192.0.2.10/24", "networkctl reload", notLive} {
		if !strings.Contains(string(cc), want) {
			t.Errorf("no %q in:\n%s", want, cc)
		}
	}
	// The NIC there, its driver is recorded; not a virtual function's, its link leaves those out,
	// so a change of the NIC's model keeping its MAC still matches.
	if l := attachedLinkOf(t, oem, "edge"); !strings.Contains(l, "[Match]\nMACAddress=02:00:00:00:00:01\nDriver=!mlx4_en mlx5_core mana\n\n[Link]\nName=edge\n") {
		t.Errorf("the link of a NIC there:\n%s", l)
	}
	if nics, err := readAttached(dir); err != nil || len(nics) != 1 || nics[0].Driver != "virtio_net" {
		t.Errorf("record %+v, %v", nics, err)
	}
	// The seed's NIC is its first boot's to pin: the attach leaves it.
	if strings.Contains(string(cc), "uplink") || strings.Count(out.String(), "\n") != 1 {
		t.Errorf("the attach names the primary:\n%s\n%s", cc, out.String())
	}
	// The next boot writes the link: the role is named, which the install's warning reads.
	if roleNamed("edge") {
		t.Error("named before the boot")
	}
	if err := os.WriteFile(filepath.Join(networkd, attachedLink("notes", "edge")), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if roleNamed("edge") {
		t.Error("named with no NIC there")
	}
	sysNIC(t, "edge", "02:00:00:00:00:01")
	if !roleNamed("edge") {
		t.Error("an attached NIC's role is not named")
	}

	// Again for the role: its addressing replaced; another role beside it.
	out.Reset()
	if err := AttachNIC(r, dir, NetAttach{Role: "edge", MAC: "02:00:00:00:00:02", Mode: IfaceDHCP}, &out); err != nil {
		t.Fatal(err)
	}
	if err := AttachNIC(r, dir, NetAttach{Role: "lan", MAC: "02:00:00:00:00:03"}, &out); err != nil {
		t.Fatal(err)
	}
	cc, _ = os.ReadFile(filepath.Join(oem, "96_notes-interfaces.yaml"))
	if strings.Contains(string(cc), "uplink") || strings.Contains(string(cc), "02:00:00:00:00:01") || !strings.Contains(string(cc), "DHCP=ipv4") ||
		!strings.Contains(string(cc), "10-notes-lan.network") || !strings.Contains(string(cc), "ConfigureWithoutCarrier=yes") {
		t.Errorf("after a second attach:\n%s", cc)
	}
	nics, err := readAttached(dir)
	if err != nil || len(nics) != 2 || nics[0].Role != "edge" || nics[1].Role != "lan" {
		t.Errorf("record %+v, %v", nics, err)
	}

	// The static mode's prefix, both sides of its least.
	if err := AttachNIC(r, dir, NetAttach{Role: "lan", MAC: "02:00:00:00:00:03", Mode: IfaceStatic, Address: "10.0.0.1/1"}, &out); err != nil {
		t.Errorf("a /1: %v", err)
	}
	// A MAC the seed names another NIC by, or a NIC there now named as another role, has.
	if err := os.WriteFile(filepath.Join(networkd, "10-notes-wan.link"), []byte("[Match]\nMACAddress=02:00:00:00:00:66\n\n[Link]\nName=wan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysNetDir, "edge", "address"), []byte("02:00:00:00:00:55\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		a    NetAttach
		want string
	}{
		{NetAttach{Role: "lan", MAC: "02:00:00:00:00:03", Mode: IfaceStatic, Address: "10.0.0.1/0"}, "IPv4 address"},
		{NetAttach{Role: "lan", MAC: "02:00:00:00:00:66"}, "is wan's NIC's"},
		{NetAttach{Role: "lan", MAC: "02:00:00:00:00:55"}, "is edge's NIC's"},
		{NetAttach{Role: "uplink", MAC: "02:00:00:00:00:09"}, "the primary NIC is the VM's own"},
		{NetAttach{Role: "vpn0", MAC: "02:00:00:00:00:09"}, "not one of notes's NICs (edge, lan, uplink)"},
		{NetAttach{Role: "edge", MAC: "03:00:00:00:00:09"}, "not a unicast MAC"},
		{NetAttach{Role: "edge", MAC: "02:00:00:00:00:aa"}, "is uplink's NIC's"},
		{NetAttach{Role: "edge", MAC: "02:00:00:00:00:03"}, "is lan's NIC's"},
		{NetAttach{Role: "edge", MAC: "02:00:00:00:00:09", Mode: IfaceStatic}, "--address \"\": the static mode's"},
		{NetAttach{Role: "edge", MAC: "02:00:00:00:00:09", Mode: IfaceStatic, Address: "2001:db8::1/64"}, "IPv4 address"},
		{NetAttach{Role: "edge", MAC: "02:00:00:00:00:09", Mode: IfaceDHCP, Address: "192.0.2.1/24"}, "the static mode's"},
		{NetAttach{Role: "edge", MAC: "02:00:00:00:00:09", Mode: "bridge"}, "dhcp, static or none"},
	} {
		if err := AttachNIC(r, dir, c.a, &out); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v", c.a, err)
		}
	}
	// A role the seed names is the seed's.
	if err := os.WriteFile(filepath.Join(networkd, "10-notes-edge.link"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AttachNIC(r, dir, NetAttach{Role: "edge", MAC: "02:00:00:00:00:09"}, &out); err == nil || !strings.Contains(err.Error(), "names its NIC already") {
		t.Errorf("a seed's role: %v", err)
	}
	// Whether the seed names the role does not read: refused, with what to restore, never taken
	// for a role the seed does not name.
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	networkdDir = notDir
	before, _ := os.ReadFile(filepath.Join(dir, interfacesFile))
	if err := AttachNIC(r, dir, NetAttach{Role: "lan", MAC: "02:00:00:00:00:03"}, &out); err == nil ||
		err.Error() != "--role lan: the VM's cloud-config's .link file of lan does not read (stat "+filepath.Join(notDir, "10-notes-lan.link")+": not a directory)" ||
		refusedFix(err) != "restore it as the VM's cloud-config writes it (`sudo notes-stow reboot` writes it again), then run this again" {
		t.Errorf("the seed's .link unread: %v (%q)", err, refusedFix(err))
	}
	if after, _ := os.ReadFile(filepath.Join(dir, interfacesFile)); string(after) != string(before) {
		t.Errorf("the record changed:\n%s", after)
	}
	networkdDir = networkd
	if err := os.WriteFile(filepath.Join(dir, interfacesFile), []byte("- role: lan\n  mac: 02:00:00:00:00:03\n  colour: red\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AttachNIC(r, dir, NetAttach{Role: "lan", MAC: "02:00:00:00:00:03"}, &out); err == nil || !strings.Contains(err.Error(), "colour") ||
		refusedFix(err) != "fix "+filepath.Join(dir, interfacesFile)+" (a list of role, mac, driver, mode and address), or remove it and run `network attach` again for each NIC it held" {
		t.Errorf("an unknown field: %v (%q)", err, refusedFix(err))
	}
	// A record of Stowage 0.39.0 pinning the seed's NIC: refused, naming its fix.
	if err := os.WriteFile(filepath.Join(dir, interfacesFile), []byte("- role: lan\n  mac: 02:00:00:00:00:03\n- role: uplink\n  mac: 02:00:00:00:00:aa\n  pin: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AttachNIC(r, dir, NetAttach{Role: "lan", MAC: "02:00:00:00:00:03"}, &out); err == nil || err.Error() != filepath.Join(dir, interfacesFile)+" has a pin entry from Stowage 0.39.0 or earlier: remove its entries with `pin: true` (the first boot pins the NIC itself now), then run this again; this VM's cloud-config, from before 0.40.0, does not: pin its NIC by hand first, as \"A VM seeded before 0.40.0\" in docs/vm.md says" ||
		refusedFix(err) != "pin the VM's NIC by hand (\"A VM seeded before 0.40.0\" in docs/vm.md), remove the entries with `pin: true` from "+filepath.Join(dir, interfacesFile)+", then run this again" {
		t.Errorf("a 0.39.0 pin: %v (%q)", err, refusedFix(err))
	}
}

// The seed's NIC is pinned by the VM's first boot, not by the attach: whichever NICs are there,
// in whatever order the platform added them, the attach names the new one alone, and its advice
// follows what the kernel's NICs say — unread, it says so and gives both ways.
func TestANICAttachedWhateverNICsAreThere(t *testing.T) {
	unread := "; its driver unread (no NIC there, or none udev records), its link leaves out a virtual function's drivers (mlx4_en, mlx5_core, mana): a NIC of one of them is named once this is run again with the NIC there"
	for _, c := range []struct {
		name string
		nics map[string]string
		want string
	}{
		{"not there yet", nil, "add the NIC on the VM's platform, then restart the VM: the boot names it" + unread + "\n"},
		{"there, and a third NIC", map[string]string{"eth1": "02:00:00:00:00:01", "eth2": "02:00:00:00:00:77"}, "the NIC is there: `sudo notes-stow reboot` names it\n"},
		{"there under the role's original name", map[string]string{"ens4": "02:00:00:00:00:01"}, "the NIC is there: `sudo notes-stow reboot` names it\n"},
		{"a virtual function of its MAC alone", map[string]string{"enP1s1": "02:00:00:00:00:01"}, "add the NIC on the VM's platform, then restart the VM: the boot names it" + unread + "\n"},
		{"there, no driver udev records", map[string]string{"eth9": "02:00:00:00:00:01"}, "the NIC is there: `sudo notes-stow reboot` names it" + unread + "\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, oem := attachDirs(t)
			for dev, mac := range c.nics {
				if strings.HasPrefix(dev, "enP") {
					sysVF(t, dev, mac)
					sysDriver(t, dev, "mlx5_core")
					continue
				}
				sysNIC(t, dev, mac)
				if dev != "eth9" {
					sysDriver(t, dev, "virtio_net")
				}
			}
			var out strings.Builder
			if err := AttachNIC(attachRelease(t), t.TempDir(), NetAttach{Role: "edge", MAC: "02:00:00:00:00:01"}, &out); err != nil {
				t.Fatal(err)
			}
			if want := "edge: the NIC 02:00:00:00:00:01 is named and addressed (none) from the next boot: " + c.want; out.String() != want {
				t.Errorf("out %q\nwant %q", out.String(), want)
			}
			if cc, _ := os.ReadFile(filepath.Join(oem, "96_notes-interfaces.yaml")); strings.Contains(string(cc), "uplink") {
				t.Errorf("the primary named by the attach:\n%s", cc)
			}
		})
	}
	// The kernel's NICs unread: refused before anything changes, never one guessed.
	_, oem := attachDirs(t)
	if err := os.Remove(filepath.Join(sysNetDir, "uplink", "address")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(sysNetDir, "uplink", "address"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := AttachNIC(attachRelease(t), dir, NetAttach{Role: "edge", MAC: "02:00:00:00:00:01"}, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "uplink/address") {
		t.Errorf("the NICs unread: %v", err)
	}
	for _, f := range []string{filepath.Join(oem, "96_notes-interfaces.yaml"), filepath.Join(dir, interfacesFile)} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s written", f)
		}
	}
}

// A role the seed names by its NIC's original name, never pinned (a VM seeded before 0.40.0, or
// one whose first boot did not pin it): no NIC is attached beside it, which may take its name.
func TestNoNICIsAttachedBesideAnUnpinnedOne(t *testing.T) {
	networkd, oem := attachDirs(t)
	if err := os.WriteFile(filepath.Join(networkd, "10-notes-uplink.link"), []byte(interfaceLink("notes", MachineInterface{Role: "uplink"})), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	err := AttachNIC(attachRelease(t), dir, NetAttach{Role: "edge", MAC: "02:00:00:00:00:01"}, &strings.Builder{})
	if err == nil || err.Error() != "--role edge: the VM's cloud-config names uplink by its NIC's original name, not pinned to its MAC: a NIC added may take its name" ||
		refusedFix(err) != "pin it first: by hand, as \"A VM seeded before 0.40.0\" in docs/vm.md says, or, on a VM seeded by Stowage 0.40.0 or later, leave its NIC the VM's only one and restart the VM, whose boot pins it to that NIC's MAC; then run this again" {
		t.Errorf("%v (fix %q)", err, refusedFix(err))
	}
	for _, f := range []string{filepath.Join(oem, "96_notes-interfaces.yaml"), filepath.Join(dir, interfacesFile)} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s written", f)
		}
	}
}

// An attached NIC's link matches its MAC and the Driver= value of its driver (macMatch): the
// virtual functions' drivers left out, unless the NIC's own is one of them (a physical Mellanox
// NIC, matched by it), so a virtual function sharing that MAC is never named its role. The driver
// is recorded in interfaces.yaml once a run finds the NIC there, a virtual function of its MAC
// beside it left out.
func TestAnAttachedNICsLinkMatchesItsDriver(t *testing.T) {
	_, oem := attachDirs(t)
	r, dir := attachRelease(t), t.TempDir()
	edge := NetAttach{Role: "edge", MAC: "02:00:00:00:00:01", Mode: IfaceDHCP}
	record := func() attachedNIC {
		t.Helper()
		nics, err := readAttached(dir)
		if err != nil || len(nics) != 1 {
			t.Fatalf("record %+v, %v", nics, err)
		}
		return nics[0]
	}
	// Attached first: no NIC there, no driver.
	if err := AttachNIC(r, dir, edge, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if l := attachedLinkOf(t, oem, "edge"); !strings.Contains(l, "[Match]\nMACAddress=02:00:00:00:00:01\nDriver=!mlx4_en mlx5_core mana\n\n") {
		t.Errorf("the link of a NIC not there:\n%s", l)
	}
	if n := record(); n.Driver != "" {
		t.Errorf("a driver recorded with no NIC there: %+v", n)
	}
	// The NIC there now, under the kernel's name, with its virtual function of the same MAC
	// beside it, not enslaved yet (Azure's accelerated networking as it comes up): run again, the
	// NIC's driver is recorded, never the virtual function's, and its link leaves those out.
	sysNIC(t, "eth1", "02:00:00:00:00:01")
	sysDriver(t, "eth1", "hv_netvsc")
	sysNIC(t, "enP1s1", "02:00:00:00:00:01")
	sysDriver(t, "enP1s1", "mlx5_core")
	var out strings.Builder
	if err := AttachNIC(r, dir, edge, &out); err != nil {
		t.Fatal(err)
	}
	if l := attachedLinkOf(t, oem, "edge"); !strings.Contains(l, "[Match]\nMACAddress=02:00:00:00:00:01\nDriver=!mlx4_en mlx5_core mana\n\n") {
		t.Errorf("the link once the NIC is there:\n%s", l)
	}
	if strings.Contains(out.String(), "driver unread") || !strings.Contains(out.String(), "the NIC is there") {
		t.Errorf("out %q", out.String())
	}
	// The record's round trip: written with its driver, read back with it, rendered from it.
	raw, err := os.ReadFile(filepath.Join(dir, interfacesFile))
	if err != nil || !strings.Contains(string(raw), "driver: hv_netvsc\n") {
		t.Errorf("record %q, %v", raw, err)
	}
	if n := record(); n != (attachedNIC{Role: "edge", MAC: "02:00:00:00:00:01", Driver: "hv_netvsc", Mode: IfaceDHCP}) {
		t.Errorf("record %+v", n)
	}
	// The virtual function enslaved now: the same.
	if err := os.Symlink("../eth1", filepath.Join(sysNetDir, "enP1s1", "master")); err != nil {
		t.Fatal(err)
	}
	if err := AttachNIC(r, dir, edge, &strings.Builder{}); err != nil || record().Driver != "hv_netvsc" {
		t.Errorf("enslaved: %v %+v", err, record())
	}
	if err := os.Remove(filepath.Join(sysNetDir, "enP1s1", "master")); err != nil {
		t.Fatal(err)
	}
	// Two NICs of that MAC, neither a virtual function's: which one is not known, refused naming
	// them, nothing written.
	sysDriver(t, "enP1s1", "virtio_net")
	before, _ := os.ReadFile(filepath.Join(dir, interfacesFile))
	err = AttachNIC(r, dir, NetAttach{Role: "edge", MAC: "02:00:00:00:00:01"}, &strings.Builder{})
	if err == nil || err.Error() != "--mac 02:00:00:00:00:01: the VM's NICs enP1s1 (virtio_net), eth1 (hv_netvsc) all have it, and no one alone is told apart from a virtual function's (mlx4_en, mlx5_core, mana): which one is the NIC is not known" ||
		refusedFix(err) != "give each NIC its own MAC on the VM's platform, then run this again" {
		t.Errorf("two NICs of one MAC: %v (%q)", err, refusedFix(err))
	}
	if after, _ := os.ReadFile(filepath.Join(dir, interfacesFile)); string(after) != string(before) {
		t.Errorf("written: %s", after)
	}
	if err := os.RemoveAll(filepath.Join(sysNetDir, "enP1s1")); err != nil {
		t.Fatal(err)
	}
	// Run again while the NIC is gone: the same MAC keeps the driver recorded; another drops it.
	if err := os.RemoveAll(filepath.Join(sysNetDir, "eth1")); err != nil {
		t.Fatal(err)
	}
	if err := AttachNIC(r, dir, NetAttach{Role: "edge", MAC: "02:00:00:00:00:01"}, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if n := record(); n.Driver != "hv_netvsc" || n.Mode != IfaceNone {
		t.Errorf("the same MAC, its NIC gone: %+v", n)
	}
	if err := AttachNIC(r, dir, NetAttach{Role: "edge", MAC: "02:00:00:00:00:02"}, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if n := record(); n.Driver != "" || !strings.Contains(attachedLinkOf(t, oem, "edge"), "MACAddress=02:00:00:00:00:02\nDriver=!mlx4_en mlx5_core mana\n") {
		t.Errorf("another MAC, its NIC not there: %+v", n)
	}
	// A physical Mellanox NIC there (metal): matched by its own driver, one of the virtual
	// functions'.
	sysNIC(t, "enp3s0", "02:00:00:00:00:03")
	sysDriver(t, "enp3s0", "mlx5_core")
	if err := AttachNIC(r, dir, NetAttach{Role: "lan", MAC: "02:00:00:00:00:03"}, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if l := attachedLinkOf(t, oem, "lan"); !strings.Contains(l, "[Match]\nMACAddress=02:00:00:00:00:03\nDriver=mlx5_core\n\n") {
		t.Errorf("a physical Mellanox NIC:\n%s", l)
	}
	// The NIC gone between the listing and the read of its ifindex: no NIC there, never an error.
	if err := os.Remove(filepath.Join(sysNetDir, "enp3s0", "ifindex")); err != nil {
		t.Fatal(err)
	}
	var gone strings.Builder
	if err := AttachNIC(r, dir, NetAttach{Role: "lan", MAC: "02:00:00:00:00:03"}, &gone); err != nil || !strings.Contains(gone.String(), "add the NIC on the VM's platform") {
		t.Errorf("the NIC gone: %v %q", err, gone.String())
	}
	// The NIC's ifindex unread: refused, never a link guessed.
	if err := os.Mkdir(filepath.Join(sysNetDir, "enp3s0", "ifindex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := AttachNIC(r, dir, NetAttach{Role: "lan", MAC: "02:00:00:00:00:03"}, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "ifindex") {
		t.Errorf("the ifindex unread: %v", err)
	}
}

// The hint for a role recorded by `network attach` whose NIC is there tells a reboot only when the
// link as written matches that NIC (its MAC and the Driver= value of the driver recorded, as udev
// matches it); else it names the `network attach` that records the NIC's driver, its addressing
// kept, and that run, followed literally, makes the reboot name it.
func TestAnAttachedRolesHintFollowsItsLink(t *testing.T) {
	attachDirs(t)
	r, dir := attachRelease(t), t.TempDir()
	hint := func() string {
		t.Helper()
		h, err := unnamedRoleHint(networkdDir, "notes", "notes-stow", dir, "lan", nicsThere(sysNetDir, udevDataDir))
		if err != nil {
			t.Fatal(err)
		}
		return h.String()
	}
	reboot := "its NIC (02:00:00:00:00:03) is recorded and there, and is named so from the next boot: `sudo notes-stow reboot`"
	// Attached with a physical Mellanox NIC there: its link names mlx5_core.
	sysNIC(t, "enp3s0", "02:00:00:00:00:03")
	sysDriver(t, "enp3s0", "mlx5_core")
	lan := NetAttach{Role: "lan", MAC: "02:00:00:00:00:03", Mode: IfaceStatic, Address: "192.0.2.20/24"}
	if err := AttachNIC(r, dir, lan, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if got := hint(); got != reboot {
		t.Errorf("matched: %q", got)
	}
	// The NIC's model changed on the platform, its MAC kept: the link names mlx5_core still.
	sysDriver(t, "enp3s0", "virtio_net")
	want := "its NIC (02:00:00:00:00:03) is recorded and there, but its link matches Driver=mlx5_core (the driver recorded: mlx5_core), not the NIC's (virtio_net): " +
		"`sudo notes-stow network attach --role lan --mac 02:00:00:00:00:03 --mode static --address 192.0.2.20/24`, run with the NIC there, records its driver, then `sudo notes-stow reboot` names it"
	if got := hint(); got != want {
		t.Errorf("not matched:\n%q\nwant %q", got, want)
	}
	// Followed literally: the record's driver and its link follow the NIC, the addressing kept.
	if err := AttachNIC(r, dir, lan, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if got := hint(); got != reboot || !strings.Contains(attachedLinkOf(t, oemDir, "lan"), "Driver=!mlx4_en mlx5_core mana\n") {
		t.Errorf("after the attach again: %q", got)
	}
	if nics, _ := readAttached(dir); len(nics) != 1 || nics[0].Address != "192.0.2.20/24" || nics[0].Driver != "virtio_net" {
		t.Errorf("record %+v", nics)
	}
	// Attached before a physical Mellanox NIC was there: its link leaves mlx5_core out, so the NIC
	// there now is not named until the attach runs again.
	if err := os.Remove(filepath.Join(dir, interfacesFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(sysNetDir, "enp3s0")); err != nil {
		t.Fatal(err)
	}
	if err := AttachNIC(r, dir, NetAttach{Role: "lan", MAC: "02:00:00:00:00:03"}, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	sysNIC(t, "enp3s0", "02:00:00:00:00:03")
	sysDriver(t, "enp3s0", "mlx5_core")
	want = "its NIC (02:00:00:00:00:03) is recorded and there, but its link matches Driver=!mlx4_en mlx5_core mana (the driver recorded: none), not the NIC's (mlx5_core): " +
		"`sudo notes-stow network attach --role lan --mac 02:00:00:00:00:03 --mode none`, run with the NIC there, records its driver, then `sudo notes-stow reboot` names it"
	if got := hint(); got != want {
		t.Errorf("attached first:\n%q\nwant %q", got, want)
	}
	if err := AttachNIC(r, dir, NetAttach{Role: "lan", MAC: "02:00:00:00:00:03", Mode: IfaceNone}, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if got := hint(); got != reboot || !strings.Contains(attachedLinkOf(t, oemDir, "lan"), "Driver=mlx5_core\n") {
		t.Errorf("after the attach again: %q", got)
	}
}

// Two `network attach` runs at once, for two roles, each keep the other's NIC: one at a time
// reads the record and writes it whole.
func TestTwoAttachesAtOnceKeepBothNICs(t *testing.T) {
	_, oem := attachDirs(t)
	r := attachRelease(t)
	for range 20 {
		dir := t.TempDir()
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, a := range []NetAttach{{Role: "edge", MAC: "02:00:00:00:00:01"}, {Role: "lan", MAC: "02:00:00:00:00:03"}} {
			wg.Go(func() { errs <- AttachNIC(r, dir, a, io.Discard) })
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if nics, err := readAttached(dir); err != nil || len(nics) != 2 {
			t.Fatalf("record %+v, %v", nics, err)
		}
		if cc, _ := os.ReadFile(filepath.Join(oem, attachedOEMName("notes"))); !strings.Contains(string(cc), "02:00:00:00:00:01") || !strings.Contains(string(cc), "02:00:00:00:00:03") {
			t.Fatalf("the cloud-config:\n%s", cc)
		}
	}
}
