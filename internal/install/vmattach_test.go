package install

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/spec"
)

// attachDirs moves the networkd, OEM and kernel NIC directories into a test's own: the VM's seed
// named uplink by its original name, and the NIC named so has the MAC 02:00:00:00:00:aa.
func attachDirs(t *testing.T) (networkd, oem string) {
	networkd, oem, sys := t.TempDir(), t.TempDir(), t.TempDir()
	saved := [3]string{networkdDir, oemDir, sysNetDir}
	networkdDir, oemDir, sysNetDir = networkd, oem, sys
	t.Cleanup(func() { networkdDir, oemDir, sysNetDir = saved[0], saved[1], saved[2] })
	if err := os.WriteFile(filepath.Join(networkd, "10-notes-uplink.link"), []byte(interfaceLink("notes", MachineInterface{Role: "uplink"})), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sys, "uplink"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sys, "uplink", "address"), []byte("02:00:00:00:00:aa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return networkd, oem
}

func attachRelease(t *testing.T) *Release {
	r, _ := testInstallerWith(t, "1.0.0", func(s *spec.Spec) {
		s.Host = &spec.Host{Interfaces: map[string]spec.HostInterface{"uplink": {Primary: true}, "edge": {}, "lan": {}, "vpn0": {Kind: spec.InterfaceTUN}}}
	})
	return r
}

// A NIC attached to an installed VM is named by its MAC in the initramfs stage of a cloud-config
// on the OEM partition, and addressed at boot; the seed's NIC named by its original name alone is
// named by its MAC too, ahead of the seed's file, once.
func TestANICAttachedIsNamedFromTheNextBoot(t *testing.T) {
	networkd, oem := attachDirs(t)
	r, dir := attachRelease(t), t.TempDir()
	var out strings.Builder
	if err := AttachNIC(r, dir, NetAttach{Role: "edge", MAC: "02:00:00:00:00:01", Mode: IfaceStatic, Address: "192.0.2.10/24"}, &out); err != nil {
		t.Fatal(err)
	}
	cc, err := os.ReadFile(filepath.Join(oem, "96_notes-interfaces.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#cloud-config\n", "initramfs:", "/etc/systemd/network/09-notes-edge.link", "MACAddress=02:00:00:00:00:01", "Name=edge",
		"/etc/systemd/network/09-notes-uplink.link", "MACAddress=02:00:00:00:00:aa", "/etc/systemd/network/10-notes-edge.network", "Address=192.0.2.10/24",
		"networkctl reload", notLive} {
		if !strings.Contains(string(cc), want) {
			t.Errorf("no %q in:\n%s", want, cc)
		}
	}
	if strings.Contains(string(cc), "10-notes-uplink.network") {
		t.Errorf("the pinned primary is addressed:\n%s", cc)
	}
	if !strings.Contains(out.String(), "uplink: its NIC (02:00:00:00:00:aa) is named by its MAC") || !strings.Contains(out.String(), "edge: the NIC 02:00:00:00:00:01 is named and addressed (static 192.0.2.10/24)") {
		t.Errorf("out %q", out.String())
	}
	// The next boot writes the link: the role is named, which the install's warning reads.
	if roleNamed("notes", "edge") {
		t.Error("named before the boot")
	}
	if err := os.WriteFile(filepath.Join(networkd, attachedLink("notes", "edge")), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if roleNamed("notes", "edge") {
		t.Error("named with no NIC there")
	}
	if err := os.MkdirAll(filepath.Join(sysNetDir, "edge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !roleNamed("notes", "edge") {
		t.Error("an attached NIC's role is not named")
	}

	// Again for the role: its addressing replaced, the pin kept once; another role beside it.
	out.Reset()
	if err := AttachNIC(r, dir, NetAttach{Role: "edge", MAC: "02:00:00:00:00:02", Mode: IfaceDHCP}, &out); err != nil {
		t.Fatal(err)
	}
	if err := AttachNIC(r, dir, NetAttach{Role: "lan", MAC: "02:00:00:00:00:03"}, &out); err != nil {
		t.Fatal(err)
	}
	cc, _ = os.ReadFile(filepath.Join(oem, "96_notes-interfaces.yaml"))
	if strings.Count(string(cc), "09-notes-uplink.link") != 1 || strings.Contains(string(cc), "02:00:00:00:00:01") || !strings.Contains(string(cc), "DHCP=ipv4") ||
		!strings.Contains(string(cc), "10-notes-lan.network") || !strings.Contains(string(cc), "ConfigureWithoutCarrier=yes") {
		t.Errorf("after a second attach:\n%s", cc)
	}
	if strings.Contains(out.String(), "uplink:") {
		t.Errorf("the pin said again: %q", out.String())
	}
	nics, err := readAttached(dir)
	if err != nil || len(nics) != 3 || nics[0].Role != "edge" || nics[1].Role != "lan" || !nics[2].Pin {
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
	if err := os.WriteFile(filepath.Join(dir, interfacesFile), []byte("- role: lan\n  mac: 02:00:00:00:00:03\n  colour: red\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AttachNIC(r, dir, NetAttach{Role: "lan", MAC: "02:00:00:00:00:03"}, &out); err == nil || !strings.Contains(err.Error(), "colour") {
		t.Errorf("an unknown field: %v", err)
	}
}

// A seed's NIC named by its original name and no NIC named so now: nothing to pin it by, refused
// before anything is written.
func TestANICAttachedRefusesWithoutThePrimarysMAC(t *testing.T) {
	_, oem := attachDirs(t)
	if err := os.RemoveAll(filepath.Join(sysNetDir, "uplink")); err != nil {
		t.Fatal(err)
	}
	err := AttachNIC(attachRelease(t), t.TempDir(), NetAttach{Role: "edge", MAC: "02:00:00:00:00:01"}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "no NIC is named so now") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(oem, "96_notes-interfaces.yaml")); err == nil {
		t.Error("written")
	}
}

// vm attach on a VM a release with one NIC made: the VM records the NIC over SSH first, the
// module's state gains it after the primary's entry, unchanged, the apply power-cycles the VM,
// and the VM is checked to name it. Run again, it goes on with the MAC the state holds.
func TestVMAttach(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	v.r.Spec.Host = &spec.Host{Interfaces: map[string]spec.HostInterface{"uplink": {Primary: true}}}
	r := newTofuRun(t)
	if err := v.create(tofuCreate(), r.mod(nil)); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	dir := filepath.Join(r.state, "notes", "vms", "notes-1")
	readNICs := func() []moduleNIC {
		var vars struct{ Nics []moduleNIC }
		b, _ := os.ReadFile(filepath.Join(dir, varsFile))
		if err := json.Unmarshal(b, &vars); err != nil {
			t.Fatal(err)
		}
		return vars.Nics
	}
	before := readNICs()
	v.r.Spec.Host.Interfaces["edge"] = spec.HostInterface{}
	in := v.installer(r.mod(nil))
	ctx := context.Background()
	ok := func() ([]byte, error) { return []byte("edge: recorded\n"), nil }
	boot := func(id string) func() ([]byte, error) {
		return func() ([]byte, error) { return []byte("0000000" + id + "-0000-0000-0000-000000000000\n"), nil }
	}
	// One system answering each call in turn, with the host key vm create recorded for its address.
	vm := func(steps ...func() ([]byte, error)) {
		if err := os.WriteFile(filepath.Join(dir, "known_hosts"), []byte("notes-1.lan key\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		n := 0
		r.ssh.calls, r.ssh.answers = nil, []func() ([]byte, error){func() ([]byte, error) { n++; return steps[min(n, len(steps))-1]() }}
	}
	vm(boot("1"), ok, ok)
	r.tofu.calls, r.tofu.plan, r.tofu.show = nil, 2, nicPlan
	a := &VMAttach{Host: "notes-1", Interface: "edge=static,address=192.0.2.10/24", User: "ops", SSHKey: v.key, Address: "notes-1.lan", Ctl: "/opt/notes-stow", Wait: 2 * time.Second}
	if err := in.AttachVM(ctx, a); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	nics := readNICs()
	if len(nics) != 2 || nics[0] != before[0] || nics[1].Role != "edge" || nics[1].Attach != "br0" || !macAddr.MatchString(nics[1].MAC) {
		t.Fatalf("nics %+v (before %+v)", nics, before)
	}
	mac := nics[1].MAC
	if c := r.ssh.calls; len(c) != 3 || !slices.Equal(c[1][len(c[1])-12:], []string{"sudo", "notes-stow", "network", "attach", "--role", "edge", "--mac", mac, "--mode", "static", "--address", "192.0.2.10/24"}) ||
		!slices.Contains(c[1], "ops@notes-1.lan") || !slices.Contains(c[1], "StrictHostKeyChecking=yes") || !slices.Contains(c[1], "HostKeyAlias=notes-1.lan") ||
		c[2][len(c[2])-1] != `test "$(cat /proc/sys/kernel/random/boot_id)" != 00000001-0000-0000-0000-000000000000 && test -e /sys/class/net/edge` {
		t.Fatalf("ssh ran %q", c)
	}
	if got := r.tofu.calls; len(got) != 5 || got[0][0] != "version" || got[1][1] != "init" || got[2][1] != "plan" || got[3][1] != "show" ||
		!slices.Contains(got[2], "-var") || !slices.ContainsFunc(got[2], func(a string) bool { return strings.HasPrefix(a, `nics=[{"role":"uplink"`) && strings.Contains(a, mac) }) ||
		!slices.Equal(got[4][1:], []string{"apply", "-input=false", "-no-color", attachPlanFile}) {
		t.Fatalf("tofu ran %q", got)
	}
	if !strings.Contains(v.pubEnv.out.String(), "edge: recorded\nAttached edge's NIC "+mac+" to notes-1.") {
		t.Errorf("out %q", v.pubEnv.out.String())
	}

	// Again: the MAC the state holds, no second entry, and no power cycle: the VM reboots for its
	// addressing. Another MAC or bridge given is refused.
	vm(boot("1"), ok, ok, ok)
	r.tofu.plan, r.tofu.calls = 0, nil
	if err := in.AttachVM(ctx, a); err != nil {
		t.Fatal(err)
	}
	if n := readNICs(); len(n) != 2 || len(r.ssh.calls) != 4 || n[1].MAC != mac || !slices.Contains(r.ssh.calls[1], mac) || !slices.Equal(r.ssh.calls[2][len(r.ssh.calls[2])-3:], []string{"sudo", "notes-stow", "reboot"}) ||
		slices.ContainsFunc(r.tofu.calls, func(c []string) bool { return len(c) > 1 && c[1] == "apply" }) {
		t.Errorf("resumed: %+v %q", n, r.ssh.calls)
	}
	b := *a
	b.Interface = "edge=dhcp,mac=02:00:00:00:00:77"
	if err := in.AttachVM(ctx, &b); err == nil || !strings.Contains(err.Error(), "with the MAC "+mac+" already") {
		t.Errorf("another MAC: %v", err)
	}
	b.Interface = "edge=dhcp,attach=br1"
	if err := in.AttachVM(ctx, &b); err == nil || !strings.Contains(err.Error(), "to br0 already") {
		t.Errorf("another bridge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, attachPlanFile)); err == nil {
		t.Error("the plan file is left")
	}
	// A plan that changes more than the NICs, adds them without a power cycle, or does not plan:
	// nothing changed, on the VM (only its boot id read) or in the state.
	noApply := func() bool {
		_, left := os.Stat(filepath.Join(dir, attachPlanFile))
		return !slices.ContainsFunc(r.tofu.calls, func(c []string) bool { return len(c) > 1 && c[1] == "apply" }) && len(r.ssh.calls) == 1 && left != nil
	}
	vb0, _ := os.ReadFile(filepath.Join(dir, varsFile))
	unchanged := func() bool { vb, _ := os.ReadFile(filepath.Join(dir, varsFile)); return string(vb) == string(vb0) }
	vm(boot("1"), ok, ok)
	r.tofu.calls, r.tofu.plan, r.tofu.show = nil, 2, strings.Replace(nicPlan, `"address": "truenas_zvol.disk", "change": {"actions": ["no-op"]}`, `"address": "truenas_zvol.disk", "change": {"actions": ["update"]}`, 1)
	if err := in.AttachVM(ctx, a); err == nil || !strings.Contains(err.Error(), "changes more than its NIC: truenas_zvol.disk (update)") || !strings.Contains(err.Error(), "nothing changed") || !strings.Contains(err.Error(), "STOWAGE_CTL=/opt/notes-stow tofu -chdir="+dir+" plan -var 'nics=[") || strings.Contains(err.Error(), " apply") || !noApply() || !unchanged() {
		t.Errorf("drift: %v", err)
	}
	vm(boot("1"), ok, ok)
	r.tofu.calls, r.tofu.show = nil, strings.Replace(nicPlan, `"address": "truenas_zvol.disk", "change": {"actions": ["no-op"]}`, `"address": "truenas_dataset.staging", "change": {"actions": ["create"]}`, 1)
	if err := in.AttachVM(ctx, a); err == nil || !strings.Contains(err.Error(), "truenas_dataset.staging (create)") || !noApply() || !unchanged() {
		t.Errorf("another resource created: %v", err)
	}
	vm(boot("1"), ok, ok)
	r.tofu.calls, r.tofu.plan, r.tofu.show = nil, 1, nicPlan
	if err := in.AttachVM(ctx, a); err == nil || !strings.Contains(err.Error(), "exit status 1") || !noApply() || !unchanged() {
		t.Errorf("a plan that fails: %v", err)
	}
	vm(boot("1"), ok, ok)
	r.tofu.calls, r.tofu.plan, r.tofu.show = nil, 2, strings.Replace(nicPlan, `"address": "terraform_data.power", "change": {"actions": ["delete", "create"]}`, `"address": "terraform_data.power", "change": {"actions": ["no-op"]}`, 1)
	if err := in.AttachVM(ctx, a); err == nil || !strings.Contains(err.Error(), "does not add the NIC with the VM's power cycle") || !noApply() || !unchanged() {
		t.Errorf("no power cycle: %v", err)
	}
	// Another NIC device created than the one attached: refused.
	vm(boot("1"), ok, ok)
	r.tofu.calls, r.tofu.show = nil, strings.Replace(nicPlan, `"address": "truenas_vm_device.nic[\"0\"]", "change": {"actions": ["no-op"]}`, `"address": "truenas_vm_device.nic[\"0\"]", "change": {"actions": ["create"]}`, 1)
	if err := in.AttachVM(ctx, a); err == nil || !strings.Contains(err.Error(), `truenas_vm_device.nic["0"] (create)`) || !noApply() || !unchanged() {
		t.Errorf("another NIC created: %v", err)
	}
	applied := func() bool {
		return slices.ContainsFunc(r.tofu.calls, func(c []string) bool { return len(c) > 1 && c[1] == "apply" })
	}
	rebooted := func() bool {
		return slices.ContainsFunc(r.ssh.calls, func(c []string) bool { return slices.Equal(c[len(c)-3:], []string{"sudo", "notes-stow", "reboot"}) })
	}
	// An earlier apply left half done (the power step alone to create or replace): refused, with
	// the way to finish it by hand.
	for _, acts := range []string{`["create"]`, `["delete", "create"]`} {
		vm(boot("1"), ok, ok)
		r.tofu.calls, r.tofu.show = nil, `{"resource_changes": [{"address": "terraform_data.power", "change": {"actions": `+acts+`}}]}`
		if err := in.AttachVM(ctx, a); err == nil || !strings.Contains(err.Error(), "does not add the NIC with the VM's power cycle") ||
			!strings.Contains(err.Error(), "power the VM off on TrueNAS, then `STOWAGE_CTL=/opt/notes-stow tofu -chdir="+dir+" apply`") ||
			!strings.Contains(err.Error(), "`/opt/notes-stow vm attach notes-1 --interface edge=static,address=192.0.2.10/24 --host notes-1.lan --user ops --ssh-key "+v.key+" --wait 2s` again checks it") || !noApply() || !unchanged() {
			t.Errorf("power alone %s: %v", acts, err)
		}
	}
	// A plan with changes and no resource changed (outputs): nothing applied, the VM rebooted.
	vm(boot("1"), ok, ok, ok)
	r.tofu.calls, r.tofu.show = nil, `{"resource_changes": [{"address": "truenas_zvol.disk", "change": {"actions": ["no-op"]}}]}`
	if err := in.AttachVM(ctx, a); err != nil || applied() || !rebooted() {
		t.Errorf("outputs alone: %v", err)
	}
	// A vm attach running on the VM already: refused.
	unlock, err := lockFile(stateLock(dir))
	if err != nil {
		t.Fatal(err)
	}
	vm(boot("1"))
	if err := in.AttachVM(ctx, a); err == nil || !strings.Contains(err.Error(), "another vm attach or vm destroy runs on it") || len(r.ssh.calls) != 0 {
		t.Errorf("locked: %v", err)
	}
	if err := in.DestroyVM(ctx, &VMDestroy{Host: "notes-1", Yes: true, Ctl: "/opt/notes-stow"}); err == nil || !strings.Contains(err.Error(), "another vm attach or vm destroy runs on it") {
		t.Errorf("vm destroy, locked: %v", err)
	}
	unlock()
	// An apply that fails says the NIC is recorded and a re-run goes on.
	vm(boot("1"), ok, ok)
	r.tofu.show, r.tofu.fail = nicPlan, errors.New("the start step failed")
	if err := in.AttachVM(ctx, a); err == nil || !strings.Contains(err.Error(), "the start step failed; the NIC is recorded on the VM and in "+dir+"'s variables: fix the cause, power the VM off on TrueNAS") || !strings.Contains(err.Error(), "` again checks it") {
		t.Errorf("an apply that fails: %v", err)
	}
	r.tofu.fail, r.tofu.plan = nil, 0
	// The reboot refused is said.
	vm(boot("1"), ok, refused)
	if err := in.AttachVM(ctx, a); err == nil || !strings.Contains(err.Error(), "notes-stow reboot") {
		t.Errorf("a reboot refused: %v", err)
	}
	// A boot id that does not read: refused before the VM is changed.
	vm(refused)
	if err := in.AttachVM(ctx, a); err == nil || !strings.Contains(err.Error(), "boot id does not read") || len(r.ssh.calls) != 1 {
		t.Errorf("no boot id: %v, ssh %q", err, r.ssh.calls)
	}
	for _, bad := range []struct{ user, addr, want string }{{"-oProxyCommand=x", "", "--user -oProxyCommand=x"}, {"", "-oProxyCommand=x", `--host "-oProxyCommand=x"`}, {"", "a b", `--host "a b"`}} {
		c := *a
		c.User, c.Address = cmp.Or(bad.user, c.User), cmp.Or(bad.addr, c.Address)
		r.ssh.calls = nil
		if err := in.AttachVM(ctx, &c); err == nil || !strings.Contains(err.Error(), bad.want) || len(r.ssh.calls) != 0 {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	r.tofu.plan, r.tofu.show = 2, nicPlan
	// Another address than the one vm create recorded the host key for: refused by ssh.
	vm(ok)
	b.Interface, b.Address = "edge=dhcp", "192.0.2.99"
	if err := in.AttachVM(ctx, &b); err == nil || !strings.Contains(err.Error(), "Host key verification failed") {
		t.Errorf("another address: %v", err)
	}

	// The VM boots and names no NIC edge.
	vm(boot("1"), ok, refused)
	slow := *a
	slow.Wait = 20 * time.Millisecond
	if err := in.AttachVM(ctx, &slow); err == nil || !strings.Contains(err.Error(), "has not booted again naming a NIC edge after 20ms") {
		t.Errorf("not named: %v", err)
	}
	// The VM refuses the record: the state is left as it was.
	vm(boot("1"), refused)
	b.Interface, b.Address = "lan=dhcp", a.Address
	v.r.Spec.Host.Interfaces["lan"] = spec.HostInterface{}
	r.tofu.show = strings.Replace(nicPlan, `nic[\"1\"]`, `nic[\"2\"]`, 1)
	if err := in.AttachVM(ctx, &b); err == nil || !strings.Contains(err.Error(), "network attach") || len(readNICs()) != 2 {
		t.Errorf("refused on the VM: %v, %+v", err, readNICs())
	}

	for _, c := range []struct{ host, iface, want string }{
		{"notes-2", "edge=dhcp", "holds no state"},
		{"notes-1", "uplink=mac=02:00:00:00:00:09", "the primary NIC is the VM's own"},
		{"notes-1", "wan=dhcp", "not one of notes's NICs"},
		{"notes-1", "lan=dhcp,mac=" + mac, "the MAC " + mac + " is edge's"},
	} {
		b := *a
		b.Host, b.Interface = c.host, c.iface
		if err := in.AttachVM(ctx, &b); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %s: %v", c.host, c.iface, err)
		}
	}
	// No host key known: refused before anything is run.
	if err := os.Remove(filepath.Join(dir, "known_hosts")); err != nil {
		t.Fatal(err)
	}
	r.ssh.calls = nil
	if err := in.AttachVM(ctx, a); err == nil || !strings.Contains(err.Error(), "host key is not known") || len(r.ssh.calls) != 0 {
		t.Errorf("no known host: %v", err)
	}
	// No TrueNAS API key: refused before the VM is changed.
	delete(r.vars, "TRUENAS_API_KEY")
	vm(ok)
	if err := in.AttachVM(ctx, a); err == nil || !strings.Contains(err.Error(), "TRUENAS_API_KEY") || len(r.ssh.calls) != 0 {
		t.Errorf("no API key: %v, ssh %q", err, r.ssh.calls)
	}
	r.vars["TRUENAS_API_KEY"] = "the-api-key"
	// A state without the VM's name, which the module derives its MAC from: refused.
	var raw map[string]any
	vb, _ := os.ReadFile(filepath.Join(dir, varsFile))
	if err := json.Unmarshal(vb, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "name")
	nb, _ := json.Marshal(raw)
	if err := os.WriteFile(filepath.Join(dir, varsFile), nb, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := in.AttachVM(ctx, a); err == nil || !strings.Contains(err.Error(), "no VM name") {
		t.Errorf("no name: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, varsFile), vb, 0o644); err != nil {
		t.Fatal(err)
	}
	// A module from before NICs by role: refused.
	vt := filepath.Join(dir, "variables.tf")
	tf, _ := os.ReadFile(vt)
	if err := os.WriteFile(vt, []byte(strings.Replace(string(tf), `variable "nics"`, `variable "nics_old"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := in.AttachVM(ctx, a); err == nil || !strings.Contains(err.Error(), "has one NIC") || len(r.ssh.calls) != 0 {
		t.Errorf("an old module: %v", err)
	}
	// Another platform's module: refused.
	if err := os.WriteFile(filepath.Join(dir, platformFile), []byte("libvirt builtin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := in.AttachVM(ctx, a); err == nil || !strings.Contains(err.Error(), "TrueNAS's built-in module") {
		t.Errorf("libvirt: %v", err)
	}
}

// vm attach on a VM a release naming no NIC made: the state's nics are empty, and the module's
// one NIC goes first unchanged, its MAC the one the module derives, which no NIC added may take.
func TestVMAttachToAVMOfOneUnnamedNIC(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	r := newTofuRun(t)
	if err := v.create(tofuCreate(), r.mod(nil)); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	dir := filepath.Join(r.state, "notes", "vms", "notes-1")
	v.r.Spec.Host = &spec.Host{Interfaces: map[string]spec.HostInterface{"uplink": {Primary: true}, "edge": {}}}
	in := v.installer(r.mod(nil))
	if err := os.WriteFile(filepath.Join(dir, "known_hosts"), []byte("notes-1.lan key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := 0
	r.tofu.plan, r.tofu.show = 2, nicPlan
	steps := []func() ([]byte, error){func() ([]byte, error) { return []byte("00000001-0000-0000-0000-000000000000"), nil }, func() ([]byte, error) { return nil, nil }}
	r.ssh.calls, r.ssh.answers = nil, []func() ([]byte, error){func() ([]byte, error) { n++; return steps[min(n, len(steps))-1]() }}
	a := &VMAttach{Host: "notes-1", Interface: "edge=dhcp,mac=" + truenasMAC("notes1"), User: "ops", SSHKey: v.key, Address: "notes-1.lan", Ctl: "/opt/notes-stow"}
	if err := in.AttachVM(context.Background(), a); err == nil || !strings.Contains(err.Error(), "is the primary NIC's") {
		t.Fatalf("the module's own MAC: %v", err)
	}
	a.Interface = "edge=dhcp"
	// A plan that does not add the new role's NIC (no changes, or none to a resource): refused
	// before the VM or the state changes.
	vb0, _ := os.ReadFile(filepath.Join(dir, varsFile))
	for _, c := range []struct {
		plan int
		show string
	}{{0, ""}, {2, `{"resource_changes": [{"address": "truenas_zvol.disk", "change": {"actions": ["no-op"]}}]}`}} {
		r.ssh.calls, n = nil, 0
		r.tofu.plan, r.tofu.show = c.plan, c.show
		err := in.AttachVM(context.Background(), a)
		vb, _ := os.ReadFile(filepath.Join(dir, varsFile))
		if err == nil || !strings.Contains(err.Error(), "does not add the NIC edge") || len(r.ssh.calls) != 1 || string(vb) != string(vb0) {
			t.Fatalf("no NIC planned (%d): %v, ssh %q", c.plan, err, r.ssh.calls)
		}
	}
	r.tofu.plan, r.tofu.show, n = 2, nicPlan, 0
	a.Interface = "edge=dhcp"
	if err := in.AttachVM(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	var vars struct{ Nics []moduleNIC }
	b, _ := os.ReadFile(filepath.Join(dir, varsFile))
	if err := json.Unmarshal(b, &vars); err != nil {
		t.Fatal(err)
	}
	if len(vars.Nics) != 2 || vars.Nics[0] != (moduleNIC{}) || vars.Nics[1].Role != "edge" || vars.Nics[1].Attach != "" {
		t.Errorf("nics %+v", vars.Nics)
	}
	// Again, naming the bridge the module put it on: the same bridge.
	n = 0
	a.Interface = "edge=dhcp,attach=br0"
	if err := in.AttachVM(context.Background(), a); err != nil {
		t.Errorf("the module's bridge named: %v", err)
	}
}

// truenasMAC is the module's local.mac.
func TestTrueNASMACIsTheModules(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("tofu/truenas/main.tf")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `sha256("stowage-vm/${var.name}")`) || !strings.Contains(string(b), `format("02:%s:%s:%s:%s:%s"`) {
		t.Error("the module derives its MAC otherwise")
	}
	// sha256("stowage-vm/notes1") as the module formats its first five bytes.
	if got := truenasMAC("notes1"); got != "02:22:fa:db:69:28" {
		t.Errorf("mac %q", got)
	}
}

// nicPlan is `tofu show -json` of a plan adding a NIC device and power-cycling the VM.
const nicPlan = `{"resource_changes": [
	{"address": "truenas_zvol.disk", "change": {"actions": ["no-op"]}},
	{"address": "truenas_vm_device.nic[\"0\"]", "change": {"actions": ["no-op"]}},
	{"address": "truenas_vm_device.nic[\"1\"]", "change": {"actions": ["create"]}},
	{"address": "terraform_data.power", "change": {"actions": ["delete", "create"]}}
]}`
