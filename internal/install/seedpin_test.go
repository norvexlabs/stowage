package install

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/testutil"
)

// fakeNIC is a kernel network device of a test: its MAC, its driver as udev records it ("-": a
// device udev records no driver of; none: no device, a bridge or a veth), and the device that
// enslaves it (master).
type fakeNIC struct{ mac, driver, master string }

// fakeSys writes nics into a kernel NIC directory (sys/class/net) and udev's records (udev) of a
// test's own.
func fakeSys(t *testing.T, sys, udev string, nics map[string]fakeNIC) {
	t.Helper()
	idx := 1
	for _, dev := range slices.Sorted(maps.Keys(nics)) {
		nic := nics[dev]
		d := filepath.Join(sys, dev)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		idx++
		for file, content := range map[string]string{"address": nic.mac + "\n", "ifindex": fmt.Sprintf("%d\n", idx)} {
			if err := os.WriteFile(filepath.Join(d, file), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if nic.master != "" {
			if err := os.Symlink("../"+nic.master, filepath.Join(d, "master")); err != nil {
				t.Fatal(err)
			}
		}
		if nic.driver == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Join(d, "device"), 0o755); err != nil {
			t.Fatal(err)
		}
		if nic.driver == "-" {
			continue
		}
		if err := os.WriteFile(filepath.Join(udev, fmt.Sprintf("n%d", idx)), []byte("I:12\nE:ID_NET_DRIVER="+nic.driver+"\nE:ID_NET_NAME_MAC=x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// bootPaths moves a boot's shell script's paths into a test's own directories.
func bootPaths(t *testing.T, script, oem, networkd, sys, udev string) string {
	t.Helper()
	for from, to := range map[string]string{"/sys/class/net/": sys + "/", "/oem/": oem + "/", "/etc/systemd/network/": networkd + "/", "/run/udev/data/": udev + "/"} {
		script = strings.ReplaceAll(script, from, to)
	}
	return script
}

// runSh runs a shell script, returning its standard error and whether it exited 0.
func runSh(t *testing.T, script string) (string, bool) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	if _, exited := err.(*exec.ExitError); err != nil && !exited {
		t.Fatal(err)
	}
	return stderr.String(), err == nil
}

// pinRun runs the boot's pin of uplink on a VM whose kernel NICs are nics, the boot's paths moved
// into the test's own and ip a fake recording its calls; it returns the script's standard error
// and ip's calls, one a line.
func pinRun(t *testing.T, oem, networkd string, nics map[string]fakeNIC) (stderr, ip string) {
	t.Helper()
	sys, udev, bin := t.TempDir(), t.TempDir(), t.TempDir()
	fakeSys(t, sys, udev, nics)
	calls := filepath.Join(bin, "calls")
	if err := testutil.WriteExecutable(filepath.Join(bin, "ip"), []byte("#!/bin/sh\necho \"$*\" >> "+calls+"\n")); err != nil {
		t.Fatal(err)
	}
	script, err := pinScript("notes", MachineInterface{Role: "uplink", Primary: true})
	if err != nil {
		t.Fatal(err)
	}
	// The boot's own paths, before the test moves them: the seed's .link file, rewritten.
	for _, want := range []string{"f=/oem/96_notes-uplink-pinned.yaml; ", "> /etc/systemd/network/10-notes-uplink.link && ", "path: /etc/systemd/network/10-notes-uplink.link", "/sys/class/net/", "/run/udev/data/"} {
		if !strings.Contains(script, want) {
			t.Fatalf("no %q in %s", want, script)
		}
	}
	stderr, ok := runSh(t, "PATH="+bin+":$PATH; "+bootPaths(t, script, oem, networkd, sys, udev))
	if !ok {
		t.Fatalf("the pin failed: %s", stderr)
	}
	raw, err := os.ReadFile(calls)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return stderr, string(raw)
}

// pinOf is the pin's cloud-config of a NIC of mac and a Driver= value, as the boot writes it, its
// .link file's path moved into networkd as the test's boot moves it.
func pinOf(t *testing.T, networkd, mac, value string) string {
	t.Helper()
	oem, err := pinnedOEMContent("notes", "uplink")
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReplacer(macMark, mac, driverMark, value, "/etc/systemd/network/", networkd+"/").Replace(oem)
}

// attach writes the attached NICs' cloud-config on oem, as `network attach` writes it, of edge's
// NIC of mac.
func attach(t *testing.T, oem, mac string) {
	t.Helper()
	cc, err := attachedConfig("notes", "notes-stow", []attachedNIC{{Role: "edge", MAC: mac, Driver: "virtio_net", Mode: IfaceNone}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oem, attachedOEMName("notes")), cc, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The VM's boot pins the NIC the seed names by its original name alone to its MAC and the
// Driver= value of its driver (macMatch's rule): in a cloud-config of the OEM partition, whose
// initramfs stage writes the seed's .link file at every later boot, and in that file now. Only
// with exactly one free NIC there (a NIC another enslaves or of no device, a virtual function
// beside its NIC of its MAC, and a NIC an attached role's link names left out): named as the
// role, unpinned; or any, the pinned NIC gone, then named the role now. Never again while the
// pinned NIC is there (its model changed included), but again over a pin that does not read
// whole.
func TestTheBootPinsTheOnlyNIC(t *testing.T) {
	t.Parallel()
	const mac = "02:00:00:00:00:aa"
	notVF := driverValue("")
	type pinCase struct {
		name     string
		nics     map[string]fakeNIC
		attached string // a MAC the attached NICs' cloud-config names, edge's
		value    string // the pin's Driver= value; none, not pinned
	}
	cases := []pinCase{
		{"one NIC", map[string]fakeNIC{"uplink": {mac, "virtio_net", ""}}, "", notVF},
		// A physical NIC of a virtual function's driver, on metal, is matched by its own driver.
		{"a physical mlx5 NIC alone", map[string]fakeNIC{"uplink": {mac, "mlx5_core", ""}}, "", "mlx5_core"},
		{"a second NIC", map[string]fakeNIC{"uplink": {mac, "virtio_net", ""}, "eth1": {"02:00:00:00:00:01", "virtio_net", ""}}, "", ""},
		// Another role's NIC, attached, is not one the pin counts.
		{"a second NIC another role names", map[string]fakeNIC{"uplink": {mac, "virtio_net", ""}, "eth1": {"02:00:00:00:00:01", "virtio_net", ""}}, "02:00:00:00:00:01", notVF},
		// Azure's accelerated networking before hv_netvsc enslaves its virtual function: the pair
		// of one MAC is one NIC, hv_netvsc's.
		{"a virtual function of its MAC not enslaved yet", map[string]fakeNIC{"uplink": {mac, "hv_netvsc", ""}, "enP1s1": {mac, "mlx4_en", ""}}, "", notVF},
		{"two NICs of one MAC, neither a virtual function's", map[string]fakeNIC{"uplink": {mac, "hv_netvsc", ""}, "eth1": {mac, "virtio_net", ""}}, "", ""},
		{"the NIC not renamed", map[string]fakeNIC{"eth0": {mac, "virtio_net", ""}}, "", ""},
		{"no NIC", nil, "", ""},
		// No driver known: the virtual functions' left out, as `network attach` before its NIC.
		{"no driver udev records", map[string]fakeNIC{"uplink": {mac, "-", ""}}, "", notVF},
	}
	for _, d := range vfDrivers {
		// Azure's accelerated networking: hv_netvsc's NIC and its virtual function of the same MAC,
		// enslaved to it; hv_netvsc's alone is pinned, the virtual functions' drivers left out.
		cases = append(cases, pinCase{"an Azure pair and a bridge, the virtual function of " + d,
			map[string]fakeNIC{"uplink": {mac, "hv_netvsc", ""}, "docker0": {"02:42:00:00:00:01", "", ""}, "enP1s1": {mac, d, "uplink"}}, "", notVF})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			oem, networkd := t.TempDir(), t.TempDir()
			if c.attached != "" {
				attach(t, oem, c.attached)
			}
			stderr, ip := pinRun(t, oem, networkd, c.nics)
			oemFile := filepath.Join(oem, filepath.Base(pinnedOEM("notes", "uplink")))
			linkFile := filepath.Join(networkd, "10-notes-uplink.link")
			raw, err := os.ReadFile(oemFile)
			if ip != "" {
				t.Errorf("renamed: %s", ip)
			}
			if c.value == "" {
				if err == nil || !strings.Contains(stderr, "notes: the interface uplink is not pinned to its NIC's MAC: ") {
					t.Errorf("pinned: %v %q", err, stderr)
				}
				if _, err := os.Stat(linkFile); err == nil {
					t.Error("the .link file written")
				}
				return
			}
			if err != nil {
				t.Fatalf("%v: %s", err, stderr)
			}
			if string(raw) != pinOf(t, networkd, mac, c.value) || !strings.HasPrefix(string(raw), "#cloud-config\n") {
				t.Errorf("the pin:\n%s", raw)
			}
			var cc cloudConfig
			if err := yaml.Unmarshal(raw, &cc); err != nil {
				t.Fatal(err)
			}
			st := cc.Stages["initramfs"]
			link := strings.NewReplacer(macMark, mac, driverMark, c.value).Replace(pinnedLink("notes", "uplink"))
			if len(cc.Stages) != 1 || len(st) != 1 || st[0].If != notLive || len(st[0].Files) != 1 ||
				st[0].Files[0].Path != linkFile || st[0].Files[0].Content != link {
				t.Errorf("the cloud-config:\n%s", raw)
			}
			// The role's one .link file matches by the MAC and the Driver= value `network attach`
			// writes for that driver (macMatch).
			driver := c.nics["uplink"].driver
			for _, want := range []string{"[Match]\n" + macMatch(mac, strings.Trim(driver, "-")) + "\n\n", "[Link]\nName=uplink\n"} {
				if !strings.Contains(link, want) {
					t.Errorf("no %q in:\n%s", want, link)
				}
			}
			if strings.Contains(link, "OriginalName") {
				t.Errorf("the pin still matches by the original name:\n%s", link)
			}
			if now, err := os.ReadFile(linkFile); err != nil || string(now) != link {
				t.Errorf("the .link file now: %v\n%s", err, now)
			}
			if links, err := seedLinks(networkd, "notes"); err != nil || len(links) != 1 || links["uplink"] != (seedLink{MAC: mac, Driver: c.value}) || !links["uplink"].pinned() {
				t.Errorf("seedLinks: %+v %v", links, err)
			}
			// A later boot, the pinned NIC there beside another: the pin is left, nothing said.
			later := maps.Clone(c.nics)
			later["eth1"] = fakeNIC{"02:00:00:00:00:01", "virtio_net", ""}
			if stderr, ip := pinRun(t, oem, networkd, later); stderr != "" || ip != "" {
				t.Errorf("the pinned NIC there: %q %q", stderr, ip)
			}
			if again, _ := os.ReadFile(oemFile); string(again) != string(raw) {
				t.Errorf("pinned again:\n%s", again)
			}
			// The NIC's model changed, its MAC kept (virtio_net to e1000): still the pinned NIC,
			// unless its own driver is a virtual function's, which the pin names.
			if c.value == notVF {
				if stderr, ip := pinRun(t, oem, networkd, map[string]fakeNIC{"uplink": {mac, "e1000", ""}}); stderr != "" || ip != "" {
					t.Errorf("the NIC's model changed: %q %q", stderr, ip)
				}
			}
			// The pin's MAC on a NIC whose driver its Driver= value does not match (a virtual
			// function's, the pin leaving them out; any other, the pin naming one): not the pinned
			// NIC, so that NIC, the only one, is pinned in its place by its own driver.
			drv, value := "mlx5_core", "mlx5_core"
			if c.value != notVF {
				drv, value = "e1000", notVF
			}
			stderr, ip = pinRun(t, oem, networkd, map[string]fakeNIC{"uplink": {mac, drv, ""}})
			if again, _ := os.ReadFile(oemFile); string(again) != pinOf(t, networkd, mac, value) || ip != "" ||
				stderr != "notes: the interface uplink's pinned NIC ("+mac+", Driver="+c.value+") is not here: the only NIC here no other role names, uplink, is pinned in its place\n" {
				t.Errorf("a NIC of the pin's MAC and driver %s: %q %q\n%s", drv, stderr, ip, again)
			}
			if err := os.WriteFile(oemFile, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			// A pin cut short, empty, or of a Driver= value the pin never writes (the NIC's driver
			// itself, not one of a virtual function) is no pin: the next boot writes it again, whole,
			// as a first pin, saying nothing.
			for _, cut := range []string{string(raw[:len(raw)-12]), "", pinOf(t, networkd, mac, c.nics["uplink"].driver)} {
				if cut == string(raw) {
					continue
				}
				if err := os.WriteFile(oemFile, []byte(cut), 0o644); err != nil {
					t.Fatal(err)
				}
				stderr, _ := pinRun(t, oem, networkd, c.nics)
				if again, _ := os.ReadFile(oemFile); string(again) != string(raw) || stderr != "" {
					t.Errorf("a pin of %d bytes not written again: %q\n%s", len(cut), stderr, again)
				}
			}
			// The pinned NIC gone, and no NIC, two others, or only another role's NIC there: the
			// pin is left as it is, said.
			pinned := "notes: the interface uplink's pinned NIC (" + mac + ", Driver=" + c.value + ") is not here"
			for _, nics := range []map[string]fakeNIC{nil, {"eth0": {"02:00:00:00:00:01", "virtio_net", ""}, "eth1": {"02:00:00:00:00:02", "virtio_net", ""}}, {"edge": {"02:00:00:00:00:0e", "virtio_net", ""}}} {
				attach(t, oem, "02:00:00:00:00:0e")
				stderr, ip := pinRun(t, oem, networkd, nics)
				if again, _ := os.ReadFile(oemFile); string(again) != string(raw) || ip != "" ||
					!strings.HasPrefix(stderr, pinned+", and the NICs here no other role names are ") || !strings.HasSuffix(stderr, ", not one: its pin is left as it is\n") {
					t.Errorf("%v: %q %q\n%s", slices.Sorted(maps.Keys(nics)), stderr, ip, again)
				}
			}
			// The pinned NIC replaced by another, the only one no other role names: that one is
			// pinned in its place, and named the role now — the pin's .link file, of the NIC gone,
			// left it its kernel name.
			const other = "02:00:00:00:00:0f"
			for _, n := range []struct{ dev, ip string }{
				{"eth0", "link set dev eth0 down\nlink set dev eth0 name uplink\nlink set dev uplink up\n"},
				{"uplink", ""}, // named so already this boot
			} {
				if err := os.WriteFile(oemFile, raw, 0o644); err != nil {
					t.Fatal(err)
				}
				stderr, ip := pinRun(t, oem, networkd, map[string]fakeNIC{n.dev: {other, "e1000", ""}, "edge": {"02:00:00:00:00:0e", "virtio_net", ""}})
				again, _ := os.ReadFile(oemFile)
				now, _ := os.ReadFile(linkFile)
				if string(again) != pinOf(t, networkd, other, notVF) || string(now) != strings.NewReplacer(macMark, other, driverMark, notVF).Replace(pinnedLink("notes", "uplink")) || ip != n.ip ||
					stderr != pinned+": the only NIC here no other role names, "+n.dev+", is pinned in its place\n" {
					t.Errorf("%s replacing it: %q %q\n%s\n%s", n.dev, stderr, ip, again, now)
				}
			}
		})
	}
}

// Pinned, a NIC matching the pin that is not named the role (another NIC held the name when udev
// named it) is named it now, by the boot's one rename: never over a name another NIC holds. The
// pin is left as it is.
func TestThePinNamesItsMatchingNICTheRole(t *testing.T) {
	t.Parallel()
	const mac = "02:00:00:00:00:aa"
	for _, c := range []struct {
		name       string
		nics       map[string]fakeNIC
		stderr, ip string
	}{
		{"the name free", map[string]fakeNIC{"eth0": {mac, "virtio_net", ""}}, "", "link set dev eth0 down\nlink set dev eth0 name uplink\nlink set dev uplink up\n"},
		{"the name held", map[string]fakeNIC{"eth0": {mac, "virtio_net", ""}, "uplink": {"02:00:00:00:00:01", "virtio_net", ""}},
			"notes: the interface uplink: another NIC holds its name: eth0 keeps its own\n", ""},
		{"named so", map[string]fakeNIC{"uplink": {mac, "virtio_net", ""}}, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			oem, networkd := t.TempDir(), t.TempDir()
			pin := pinOf(t, networkd, mac, driverValue(""))
			oemFile := filepath.Join(oem, filepath.Base(pinnedOEM("notes", "uplink")))
			if err := os.WriteFile(oemFile, []byte(pin), 0o644); err != nil {
				t.Fatal(err)
			}
			stderr, ip := pinRun(t, oem, networkd, c.nics)
			if again, _ := os.ReadFile(oemFile); stderr != c.stderr || ip != c.ip || string(again) != pin {
				t.Errorf("%q %q\n%s", stderr, ip, again)
			}
		})
	}
}

// Once pinned, the seed's own stage writing the role's .link file by its original name skips:
// the pin's cloud-config alone writes it, whichever of the two yip runs first. A pin that does not
// read whole leaves the seed's stage to run.
func TestTheSeedLeavesThePinnedLinkToThePin(t *testing.T) {
	t.Parallel()
	m := Machine{Hostname: "notes", User: "admin", SSHKey: "ssh-ed25519 AAAA", Interfaces: []MachineInterface{{Role: "uplink", Primary: true}}}
	ud, err := UserData("notes", "notes-stow", m, Settings{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var cc cloudConfig
	if err := yaml.Unmarshal(ud, &cc); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(cc.Stages["initramfs"], func(s stage) bool { return s.Name == "notes: the interfaces' names" })
	if i < 0 {
		t.Fatalf("no stage naming the interfaces:\n%s", ud)
	}
	names := cc.Stages["initramfs"][i]
	if len(names.Files) != 1 || names.Files[0].Path != "/etc/systemd/network/10-notes-uplink.link" || !strings.Contains(names.Files[0].Content, "OriginalName=en* eth*\nDriver=!mlx4_en mlx5_core mana\n") {
		t.Errorf("the stage: %+v", names)
	}
	oem, networkd, sys, udev := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	cond := bootPaths(t, strings.Replace(names.If, notLive+" && ", "", 1), oem, networkd, sys, udev)
	if !strings.HasPrefix(names.If, notLive+" && ! { ") {
		t.Errorf("the stage's condition: %s", names.If)
	}
	if _, runs := runSh(t, cond); !runs {
		t.Error("the seed's stage skips before any pin")
	}
	pinRun(t, oem, networkd, map[string]fakeNIC{"uplink": {"02:00:00:00:00:aa", "virtio_net", ""}})
	if _, runs := runSh(t, cond); runs {
		t.Error("the seed's stage runs once pinned")
	}
	pin := filepath.Join(oem, filepath.Base(pinnedOEM("notes", "uplink")))
	raw, err := os.ReadFile(pin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pin, raw[:len(raw)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, runs := runSh(t, cond); !runs {
		t.Error("the seed's stage skips with a pin cut short")
	}
	// The pin's file runs after the seed's (yip walks /oem in lexical order, 95_userdata's files
	// first), and is not the attached NICs' file.
	if base := filepath.Base(pinnedOEM("notes", "uplink")); base <= "95_userdata" || base == filepath.Base(attachedOEM("notes")) {
		t.Errorf("the pin's file %s", base)
	}
}

// The VM's NICs leave out a NIC another enslaves (an accelerated networking's virtual function),
// in Go and in the boot's shell alike, whatever its driver: a physical NIC of a virtual function's
// driver is one of them. The original-name .link file alone leaves those drivers out; one by MAC
// matches the NIC of that MAC whatever its driver.
func TestTheVMsNICsLeaveOutTheEnslaved(t *testing.T) {
	t.Parallel()
	sys, udev := t.TempDir(), t.TempDir()
	nics := map[string]fakeNIC{"uplink": {"02:00:00:00:00:aa", "hv_netvsc", ""}, "docker0": {"02:42:00:00:00:01", "", ""}, "enp2s0": {"02:00:00:00:00:0b", "mlx5_core", ""}}
	for n, d := range vfDrivers {
		nics[fmt.Sprintf("enP%ds1", n)] = fakeNIC{"02:00:00:00:00:aa", d, "uplink"}
	}
	fakeSys(t, sys, udev, nics)
	if got := strings.Fields(strings.TrimPrefix(vfMatch(), "Driver=!")); !slices.Equal(got, vfDrivers) {
		t.Errorf("the original-name match leaves out %v", got)
	}
	if l := interfaceLink("notes", MachineInterface{Role: "uplink"}); !strings.Contains(l, "[Match]\nOriginalName=en* eth*\n"+vfMatch()+"\n\n") {
		t.Errorf("the original-name match:\n%s", l)
	}
	if l := interfaceLink("notes", MachineInterface{Role: "edge", MAC: "02:00:00:00:00:0b"}); !strings.Contains(l, "[Match]\nMACAddress=02:00:00:00:00:0b\n\n") || strings.Contains(l, "Driver=") {
		t.Errorf("the match by MAC:\n%s", l)
	}
	want := []deviceNIC{{Name: "enp2s0", MAC: "02:00:00:00:00:0b"}, {Name: "uplink", MAC: "02:00:00:00:00:aa"}}
	if got, err := deviceNICs(sys); err != nil || !slices.Equal(got, want) {
		t.Errorf("deviceNICs: %+v %v", got, err)
	}
	out, err := exec.Command("sh", "-c", bootPaths(t, deviceNICsScript(), "", "", sys, udev)).Output()
	if err != nil || string(out) != "enp2s0\nuplink\n" {
		t.Errorf("the boot's NICs: %q %v", out, err)
	}
	// Read once, at the first question: the NIC gone since, it answers as it read them; a new
	// reading sees it gone.
	there := nicsThere(sys, udev)
	if d, err := there.drivers("02:00:00:00:00:0b"); !slices.Equal(d, []string{"mlx5_core"}) || err != nil {
		t.Errorf("there: %v %v", d, err)
	}
	// The enslaved virtual functions of uplink's MAC are not the VM's NICs.
	if d, err := there.drivers("02:00:00:00:00:aa"); !slices.Equal(d, []string{"hv_netvsc"}) || err != nil {
		t.Errorf("uplink's: %v %v", d, err)
	}
	if err := os.RemoveAll(filepath.Join(sys, "enp2s0")); err != nil {
		t.Fatal(err)
	}
	if d, err := there.drivers("02:00:00:00:00:0b"); len(d) != 1 || err != nil {
		t.Errorf("read again: %v %v", d, err)
	}
	if d, err := nicsThere(sys, udev).drivers("02:00:00:00:00:0b"); d != nil || err != nil {
		t.Errorf("a new reading: %v %v", d, err)
	}
	// A NIC gone between the listing and the read of its MAC or its ifindex is left out, as gone.
	if err := os.MkdirAll(filepath.Join(sys, "eth9", "device"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := deviceNICs(sys); err != nil || !slices.Equal(got, want[1:]) {
		t.Errorf("deviceNICs, a NIC gone: %+v %v", got, err)
	}
	if err := os.Remove(filepath.Join(sys, "uplink", "ifindex")); err != nil {
		t.Fatal(err)
	}
	if d, err := nicsThere(sys, udev).drivers("02:00:00:00:00:aa"); d != nil || err != nil {
		t.Errorf("nicsThere, a NIC gone: %v %v", d, err)
	}
	// A NIC whose MAC does not read is an error, never a NIC left out.
	if err := os.Remove(filepath.Join(sys, "uplink", "address")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(sys, "uplink", "address"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := deviceNICs(sys); err == nil {
		t.Error("deviceNICs with a MAC unread")
	}
	if d, err := nicsThere(sys, udev).drivers("02:00:00:00:00:aa"); d != nil || err == nil {
		t.Errorf("nicsThere with a MAC unread: %v %v", d, err)
	}
}

// The driver `network attach` records and the one the boot's pin reads are one rule: udev's
// ID_NET_DRIVER of the NIC's ifindex, none when udev records none or one no .link file can name.
func TestANICsDriverReadsAlikeInGoAndAtBoot(t *testing.T) {
	t.Parallel()
	sys, udev := t.TempDir(), t.TempDir()
	fakeSys(t, sys, udev, map[string]fakeNIC{"eth0": {"02:00:00:00:00:01", "hv_netvsc", ""}, "eth1": {"02:00:00:00:00:02", "-", ""},
		"eth2": {"02:00:00:00:00:03", "bad/name", ""}, "enP1s1": {"02:00:00:00:00:01", "mlx5_core", "eth0"}})
	for dev, want := range map[string]string{"eth0": "hv_netvsc", "eth1": "", "eth2": "", "enP1s1": "mlx5_core"} {
		if got, err := nicDriver(sys, udev, dev); err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", dev, got, err, want)
		}
		script := bootPaths(t, `v=`+nicDriverScript()+`; case "$v" in ''|*[!A-Za-z0-9_-]*) v=;; esac; printf %s "$v"`, "", "", sys, udev)
		if out, err := exec.Command("sh", "-c", script, "sh", dev).Output(); err != nil || string(out) != want {
			t.Errorf("%s at boot: %q %v, want %q", dev, out, err, want)
		}
	}
	if _, err := nicDriver(sys, udev, "eth9"); err == nil {
		t.Error("a NIC's ifindex unread")
	}
}

// The Driver= value of a .link file by MAC is one rule in Go (`network attach`) and in the boot's
// shell (the pin): a driver of vfDrivers itself, any other or none leaving them out; and a NIC
// matches it as udev matches it.
func TestTheDriverValueIsOneRule(t *testing.T) {
	t.Parallel()
	notVF := "!mlx4_en mlx5_core mana"
	for _, d := range []string{"virtio_net", "e1000", "hv_netvsc", "mlx4_en", "mlx5_core", "mana", "", "bad/name", "mlx5_core_x", "mlx4_core"} {
		want := notVF
		if vfDriver(d) {
			want = d
		}
		if got := driverValue(d); got != want {
			t.Errorf("%q: %q, want %q", d, got, want)
		}
		if got := macMatch("02:00:00:00:00:01", d); got != "MACAddress=02:00:00:00:00:01\nDriver="+want {
			t.Errorf("%q: %q", d, got)
		}
		out, err := exec.Command("sh", "-c", `v=$1; `+driverValueScript()+`; printf %s "$v"`, "sh", d).Output()
		if err != nil || string(out) != want {
			t.Errorf("%q at boot: %q %v, want %q", d, out, err, want)
		}
		// The value matches its own driver; leaving the virtual functions out, every other
		// driver but theirs, none included.
		if !driverMatches(want, d) {
			t.Errorf("%q does not match its own value %q", d, want)
		}
	}
	for _, c := range []struct {
		value, driver string
		match         bool
	}{
		{notVF, "virtio_net", true}, {notVF, "", true}, {notVF, "mlx5_core", false}, {notVF, "mana", false},
		{"mlx5_core", "mlx5_core", true}, {"mlx5_core", "mlx4_en", false}, {"mlx5_core", "", false}, {"mlx5_core", "virtio_net", false},
	} {
		if got := driverMatches(c.value, c.driver); got != c.match {
			t.Errorf("%q, %q: %v", c.value, c.driver, got)
		}
	}
}

// seedLinks reads the seed's .link files: a role named by its original name, or by a MAC (its
// own, or the one its first boot pinned in that file), the reader macTaken takes the MACs of.
func TestSeedLinks(t *testing.T) {
	t.Parallel()
	networkd := t.TempDir()
	write := func(file, content string) {
		if err := os.WriteFile(filepath.Join(networkd, file), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("10-notes-uplink.link", interfaceLink("notes", MachineInterface{Role: "uplink"}))
	write("10-notes-edge.link", interfaceLink("notes", MachineInterface{Role: "edge", MAC: "02:00:00:00:00:0b"}))
	// An attached role (09- alone) is not the seed's; another project's file is not this one's.
	write("09-notes-lan.link", "[Match]\nMACAddress=02:00:00:00:00:03\n")
	write("10-other-wan.link", "[Match]\nMACAddress=02:00:00:00:00:04\n")
	links, err := seedLinks(networkd, "notes")
	if err != nil || len(links) != 2 || links["uplink"] != (seedLink{Original: true}) || links["edge"] != (seedLink{MAC: "02:00:00:00:00:0b"}) {
		t.Errorf("%+v %v", links, err)
	}
	write("10-notes-uplink.link", strings.NewReplacer(macMark, "02:00:00:00:00:AA", driverMark, "virtio_net").Replace(pinnedLink("notes", "uplink")))
	if links, err := seedLinks(networkd, "notes"); err != nil || links["uplink"] != (seedLink{MAC: "02:00:00:00:00:aa", Driver: "virtio_net"}) || !links["uplink"].pinned() || links["edge"].pinned() {
		t.Errorf("pinned: %+v %v", links, err)
	}
	// A link naming neither a MAC nor an original name (empty, or a pin cut short) names no NIC.
	for _, cut := range []string{"", "[Match]\nDriver=virtio_net\n"} {
		write("10-notes-uplink.link", cut)
		if links, err := seedLinks(networkd, "notes"); err != nil || links["uplink"] != (seedLink{}) {
			t.Errorf("%q: %+v %v", cut, links, err)
		}
	}
}

// The seed names a NIC by its original name only as the machine's only NIC: one pin at most.
func TestASeedOfSeveralNICsNamesEachByItsMAC(t *testing.T) {
	t.Parallel()
	m := Machine{Hostname: "notes", User: "admin", SSHKey: "ssh-ed25519 AAAA", Interfaces: []MachineInterface{{Role: "uplink", Primary: true}, {Role: "edge", MAC: "02:00:00:00:00:0b"}}}
	if _, err := UserData("notes", "notes-stow", m, Settings{}, nil); err == nil || !strings.Contains(err.Error(), "interface uplink: no MAC, and the machine has 2 NICs") {
		t.Errorf("%v", err)
	}
}

// The boot's rename never takes a NIC down for a name another NIC holds, and brings back up a NIC
// whose rename fails; the seed's rename of the NIC of the default route is left once the role is
// pinned or a NIC's name: a NIC added later, which may get the first default route, keeps its own.
func TestTheBootsRenameLeavesANameHeld(t *testing.T) {
	t.Parallel()
	const mac = "02:00:00:00:00:0a"
	// run runs script on a VM of nics, uplink pinned to mac when pinned, and ip a fake recording
	// its calls, whose default route is on eth1 and whose rename fails with failName.
	run := func(t *testing.T, script string, nics map[string]fakeNIC, pinned, failName bool) (stderr string, ok bool, ip string) {
		t.Helper()
		sys, udev, oem, networkd, bin := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
		fakeSys(t, sys, udev, nics)
		if pinned {
			if err := os.WriteFile(filepath.Join(oem, filepath.Base(pinnedOEM("notes", "uplink"))), []byte(pinOf(t, networkd, mac, driverValue(""))), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		calls := filepath.Join(bin, "calls")
		fake := "#!/bin/sh\necho \"$*\" >> " + calls + "\ncase \"$*\" in *'route show default'*) echo 'default via 10.0.0.1 dev eth1 proto dhcp';; esac\n"
		if failName {
			fake += "case \"$*\" in *' name '*) exit 1;; esac\n"
		}
		if err := testutil.WriteExecutable(filepath.Join(bin, "ip"), []byte(fake)); err != nil {
			t.Fatal(err)
		}
		stderr, ok = runSh(t, "PATH="+bin+":$PATH; "+bootPaths(t, script, oem, networkd, sys, udev))
		raw, err := os.ReadFile(calls)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return stderr, ok, string(raw)
	}
	eth1 := fakeNIC{"02:00:00:00:00:01", "virtio_net", ""}
	rename := "i=eth1; " + renameNIC("notes", `"$i"`, "uplink")
	t.Run("a name held", func(t *testing.T) {
		t.Parallel()
		stderr, ok, ip := run(t, rename, map[string]fakeNIC{"uplink": {mac, "virtio_net", ""}, "eth1": eth1}, false, false)
		if ok || ip != "" || stderr != "notes: the interface uplink: another NIC holds its name: eth1 keeps its own\n" {
			t.Errorf("%v %q %q", ok, ip, stderr)
		}
	})
	t.Run("a rename that fails", func(t *testing.T) {
		t.Parallel()
		stderr, ok, ip := run(t, rename, map[string]fakeNIC{"eth1": eth1}, false, true)
		if ok || ip != "link set dev eth1 down\nlink set dev eth1 name uplink\nlink set dev eth1 up\n" || stderr != "notes: the interface uplink: eth1 is not renamed so: it is brought back up\n" {
			t.Errorf("%v %q %q", ok, ip, stderr)
		}
	})
	t.Run("a rename", func(t *testing.T) {
		t.Parallel()
		stderr, ok, ip := run(t, rename, map[string]fakeNIC{"eth1": eth1}, false, false)
		if !ok || ip != "link set dev eth1 down\nlink set dev eth1 name uplink\nlink set dev uplink up\n" || stderr != "" {
			t.Errorf("%v %q %q", ok, ip, stderr)
		}
	})
	seed, err := renameScript("notes", MachineInterface{Role: "uplink", Primary: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		nics map[string]fakeNIC
		pin  bool
		ip   string
	}{
		// The pinned NIC gone and a NIC added, of the default route: the pin's, never this.
		{"pinned", map[string]fakeNIC{"eth1": eth1}, true, ""},
		{"named", map[string]fakeNIC{"uplink": {mac, "virtio_net", ""}, "eth1": eth1}, false, ""},
		{"neither, the first boot", map[string]fakeNIC{"eth1": eth1}, false, "-o route show default\nlink set dev eth1 down\nlink set dev eth1 name uplink\nlink set dev uplink up\n"},
	} {
		t.Run("the default route's NIC, "+c.name, func(t *testing.T) {
			t.Parallel()
			stderr, ok, ip := run(t, seed, c.nics, c.pin, false)
			if !ok || ip != c.ip || stderr != "" {
				t.Errorf("%v %q %q", ok, ip, stderr)
			}
		})
	}
	// The NIC of the seed's MAC, its role's name held by another NIC: left up as it is.
	byMAC, err := renameScript("notes", MachineInterface{Role: "uplink", MAC: "02:00:00:00:00:01"})
	if err != nil {
		t.Fatal(err)
	}
	if stderr, ok, ip := run(t, byMAC, map[string]fakeNIC{"uplink": {mac, "virtio_net", ""}, "eth1": eth1}, false, false); ok || ip != "" || !strings.Contains(stderr, "another NIC holds its name: eth1 keeps its own") {
		t.Errorf("by its MAC, the name held: %v %q %q", ok, ip, stderr)
	}
	// The NIC of the seed's MAC is a NIC of a device, never a virtual function beside it (one not
	// enslaved yet, or enslaved) nor a bridge of that MAC, as the pin counts them; of several,
	// none is renamed, said.
	renamed := "link set dev eth1 down\nlink set dev eth1 name uplink\nlink set dev uplink up\n"
	for _, c := range []struct {
		name       string
		nics       map[string]fakeNIC
		ok         bool
		ip, stderr string
	}{
		{"a virtual function not enslaved yet", map[string]fakeNIC{"eth1": {eth1.mac, "hv_netvsc", ""}, "enP1s1": {eth1.mac, "mlx5_core", ""}}, true, renamed, ""},
		{"a virtual function enslaved", map[string]fakeNIC{"eth1": {eth1.mac, "hv_netvsc", ""}, "enP1s1": {eth1.mac, "mlx5_core", "eth1"}}, true, renamed, ""},
		{"a bridge", map[string]fakeNIC{"br0": {eth1.mac, "", ""}, "eth1": eth1}, true, renamed, ""},
		{"a physical NIC of a virtual function's driver", map[string]fakeNIC{"eth1": {eth1.mac, "mlx5_core", ""}}, true, renamed, ""},
		{"two NICs", map[string]fakeNIC{"eth1": eth1, "eth2": eth1}, false, "", "notes: the interface uplink: the NICs eth1 eth2 all have its MAC (02:00:00:00:00:01): which one is its NIC is not known, so none is renamed\n"},
		{"none", map[string]fakeNIC{"eth2": {"02:00:00:00:00:02", "virtio_net", ""}}, true, "", ""},
	} {
		t.Run("by its MAC, "+c.name, func(t *testing.T) {
			t.Parallel()
			if stderr, ok, ip := run(t, byMAC, c.nics, false, false); ok != c.ok || ip != c.ip || stderr != c.stderr {
				t.Errorf("%v %q %q", ok, ip, stderr)
			}
		})
	}
}
