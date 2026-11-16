package install

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// roleDaemon is a host daemon of a release whose primary NIC is uplink, its secondaries edge and
// lan (vpn0 a tun device), on a VM where edge alone is named, addressed 192.0.2.5/24 without an
// override.
func roleDaemon(t *testing.T) (*fakeLink, *Hostd) {
	link := newLink(t)
	link.run = t.TempDir()
	link.nics = map[string]string{"edge": "192.0.2.5/24"}
	link.nicAddr = map[string]string{"edge": "192.0.2.5/24"}
	h := netDaemon(t, link)
	h.R = attachRelease(t)
	h.RunNetworkd = link.run
	h.SysNet, h.UdevData = t.TempDir(), t.TempDir()
	return link, h
}

func nicAddr(l *fakeLink, nic string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.nicAddr[nic]
}

func roleState(t *testing.T, h *Hostd, role string) netState {
	t.Helper()
	st, err := h.netFiles().forRole(role).read()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// A secondary NIC's override is its own: rendered under /run on its link alone, awaiting
// confirmation like the primary's, the one change that may await it.
func TestASecondaryNICsOverride(t *testing.T) {
	t.Parallel()
	link, h := roleDaemon(t)
	api := h.Handler()
	var a NetAnswer
	set := NetRequest{Role: "edge", NetConfig: NetConfig{Mode: "static", Address: "192.0.2.10/24"}, RevertSecs: intp(maxRevertSecs)}
	if code := call(t, api, http.MethodPost, "/v1/network", set, fresh(&a)); code != http.StatusOK {
		t.Fatalf("%d %+v", code, a)
	}
	if a.Pending == nil || a.Pending.Role != "edge" || a.Pending.Previous != nil || !strings.Contains(a.Message, "network confirm") {
		t.Fatalf("the change does not await confirmation: %+v", a)
	}
	unit, err := os.ReadFile(filepath.Join(link.run, "09-notes-edge-override.network"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[Match]\nName=edge\n", "Address=192.0.2.10/24\n", "LinkLocalAddressing=no\n", "RequiredForOnline=no\n", "network set --role edge"} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("no %q in:\n%s", want, unit)
		}
	}
	if strings.Contains(string(unit), "Gateway") || fileExists(filepath.Join(link.dir, "01-notes-override.network")) {
		t.Errorf("the primary's network is touched:\n%s", unit)
	}
	if got := nicAddr(link, "edge"); got != "192.0.2.10/24" {
		t.Errorf("edge runs %q", got)
	}
	if addr, gw, _ := link.live(); addr != leaseAddr || gw != leaseGW {
		t.Errorf("the primary runs %s via %s", addr, gw)
	}
	if !slices.ContainsFunc(link.calls(), func(c []string) bool { return slices.Equal(c, []string{"networkctl", "reconfigure", "edge"}) }) {
		t.Errorf("edge was not reconfigured: %v", link.calls())
	}
	var n Network
	call(t, api, http.MethodGet, "/v1/network", nil, &n)
	want := []RoleNetwork{
		{Role: "edge", MAC: "02:00:00:00:00:04", Addresses: []string{"192.0.2.10/24"}, Override: &NetConfig{Mode: "static", Address: "192.0.2.10/24"}},
		{Role: "lan", Addresses: []string{}},
	}
	if !reflect.DeepEqual(n.Roles, want) || n.Source != "dhcp" || n.Pending == nil || n.Pending.Role != "edge" {
		t.Fatalf("%+v", n)
	}
	banner, _ := os.ReadFile(filepath.Join(h.Issue, "06-notes-network.issue"))
	if !strings.HasPrefix(string(banner), "network: the VM's cloud-config") || !strings.Contains(string(banner), "edge: 192.0.2.10/24; set with `notes-stow network set --role edge`\n") || !strings.Contains(string(banner), "not confirmed") {
		t.Errorf("banner:\n%s", banner)
	}
	// One change awaits confirmation at a time, across the NICs.
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{NetConfig: NetConfig{Mode: "dhcp"}}, &errOf{}); code != http.StatusConflict {
		t.Errorf("a primary change while edge's awaits confirmation: %d", code)
	}
	if code := call(t, api, http.MethodPost, "/v1/network/confirm", nil, fresh(&a)); code != http.StatusOK || a.Pending != nil || a.Override == nil || a.Override.Address != "192.0.2.10/24" {
		t.Fatalf("%d %+v", code, a)
	}
	if st := roleState(t, h, "edge"); st.Pending != nil || st.Config == nil || st.Config.Address != "192.0.2.10/24" {
		t.Fatalf("recorded %+v", st)
	}
	if fileExists(NetworkFile(h.O.Dir)) {
		t.Error("the primary's override is recorded")
	}
	// A change undone comes back to the confirmed one, on edge alone.
	none := NetRequest{Role: "edge", NetConfig: NetConfig{Mode: "none"}, RevertSecs: intp(maxRevertSecs)}
	if code := call(t, api, http.MethodPost, "/v1/network", none, fresh(&a)); code != http.StatusOK || a.Pending == nil {
		t.Fatalf("%d %+v", code, a)
	}
	if got := nicAddr(link, "edge"); got != "" {
		t.Errorf("edge runs %q with none", got)
	}
	if code := call(t, api, http.MethodPost, "/v1/network/revert", nil, fresh(&a)); code != http.StatusOK || nicAddr(link, "edge") != "192.0.2.10/24" {
		t.Fatalf("%d %+v: edge runs %q", code, a, nicAddr(link, "edge"))
	}
	h.netMu.Lock()
	st := roleState(t, h, "edge")
	h.netMu.Unlock()
	if st.Pending != nil || st.Config == nil || st.Config.Address != "192.0.2.10/24" {
		t.Errorf("after the revert: %+v", st)
	}
	// Reset drops it on the same window; revert undoes the reset now.
	if code := call(t, api, http.MethodPost, "/v1/network/reset", NetResetRequest{Role: "edge", RevertSecs: intp(maxRevertSecs)}, fresh(&a)); code != http.StatusOK || a.Pending == nil || a.Pending.Role != "edge" {
		t.Fatalf("%d %+v", code, a)
	}
	if got := nicAddr(link, "edge"); got != "192.0.2.5/24" || fileExists(filepath.Join(link.run, "09-notes-edge-override.network")) {
		t.Errorf("after the reset edge runs %q", got)
	}
	if code := call(t, api, http.MethodPost, "/v1/network/revert", nil, fresh(&a)); code != http.StatusOK || a.Override == nil || a.Override.Address != "192.0.2.10/24" {
		t.Fatalf("%d %+v", code, a)
	}
	if got := nicAddr(link, "edge"); got != "192.0.2.10/24" {
		t.Errorf("after the revert edge runs %q", got)
	}
	// DHCP, without a window, then dropped for good.
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{Role: "edge", NetConfig: NetConfig{Mode: "dhcp"}, RevertSecs: intp(0)}, fresh(&a)); code != http.StatusOK || a.Pending != nil || a.Message != "applied, without a revert window" {
		t.Fatalf("%d %+v", code, a)
	}
	if got := nicAddr(link, "edge"); got != nicLease {
		t.Errorf("edge runs %q with dhcp", got)
	}
	if code := call(t, api, http.MethodPost, "/v1/network/reset", NetResetRequest{Role: "edge", RevertSecs: intp(0)}, fresh(&a)); code != http.StatusOK || a.Pending != nil {
		t.Fatalf("%d %+v", code, a)
	}
	if fileExists(RoleNetworkFile(h.O.Dir, "edge")) || nicAddr(link, "edge") != "192.0.2.5/24" {
		t.Error("the reset left edge's override")
	}
}

// A role is one of the release's secondary NICs, named on the VM; its override sets a mode and an
// address alone.
func TestASecondaryNICsOverrideIsRefused(t *testing.T) {
	t.Parallel()
	link, h := roleDaemon(t)
	api := h.Handler()
	for _, c := range []struct {
		req  NetRequest
		code int
		want string
	}{
		{NetRequest{Role: "uplink", NetConfig: NetConfig{Mode: "dhcp"}}, http.StatusBadRequest, "is the primary NIC: `notes-stow network set` and `network reset` without --role"},
		{NetRequest{Role: "vpn0", NetConfig: NetConfig{Mode: "dhcp"}}, http.StatusBadRequest, "not one of notes's secondary NICs (edge, lan)"},
		{NetRequest{Role: "wan", NetConfig: NetConfig{Mode: "dhcp"}}, http.StatusBadRequest, "not one of notes's secondary NICs"},
		{NetRequest{Role: "edge", NetConfig: NetConfig{Mode: "static", Address: "192.0.2.10/24", Gateway: "192.0.2.1"}}, http.StatusBadRequest, "are the primary NIC's"},
		{NetRequest{Role: "edge", NetConfig: NetConfig{Mode: "dhcp", IPv6: true}}, http.StatusBadRequest, "are the primary NIC's"},
		{NetRequest{Role: "edge", NetConfig: NetConfig{Mode: "dhcp", DNS: []string{"9.9.9.9"}}}, http.StatusBadRequest, "are the primary NIC's"},
		{NetRequest{Role: "edge", NetConfig: NetConfig{Mode: "static"}}, http.StatusBadRequest, "the static mode needs an address"},
		{NetRequest{Role: "edge", NetConfig: NetConfig{Mode: "none", Address: "192.0.2.10/24"}}, http.StatusBadRequest, "an address is for the static mode"},
		{NetRequest{Role: "edge", NetConfig: NetConfig{Mode: "static", Address: "192.0.2.0/24"}}, http.StatusBadRequest, "network address"},
		{NetRequest{Role: "edge", NetConfig: NetConfig{Mode: "bridge"}}, http.StatusBadRequest, "dhcp, static or none"},
		{NetRequest{NetConfig: NetConfig{Mode: "none"}}, http.StatusBadRequest, "dhcp or static"},
	} {
		var e errOf
		if code := call(t, api, http.MethodPost, "/v1/network", c.req, &e); code != c.code || !strings.Contains(e.Error, c.want) {
			t.Errorf("%+v: %d %q, want %d %q", c.req, code, e.Error, c.code, c.want)
		}
	}
	// unnamed asks an override of lan, which no NIC is named as: every answer is a refusal (exit
	// 3), its fix what names one.
	unnamed := func(why, fix string) {
		t.Helper()
		var e errOf
		if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{Role: "lan", NetConfig: NetConfig{Mode: "dhcp"}}, &e); code != http.StatusConflict || !e.Refused ||
			e.Error != "role lan: no NIC is named so on this VM: "+why || e.Fix != fix+", then set its network" {
			t.Errorf("%d %+v\nwant %q\nfix  %q", code, e, why, fix)
		}
	}
	unnamed("none is recorded for it", "`sudo notes-stow network attach --role lan --mac <its MAC>` records one, named from the next boot: add that NIC on the VM's platform and restart the VM")
	var e errOf
	if code := call(t, api, http.MethodPost, "/v1/network/reset", NetResetRequest{Role: "uplink"}, &e); code != http.StatusBadRequest || !strings.Contains(e.Error, "primary NIC") {
		t.Errorf("reset of the primary as a role: %d %q", code, e.Error)
	}
	// No NIC named lan: nothing to apply on, but reset changes nothing there.
	if code := call(t, api, http.MethodPost, "/v1/network/reset", NetResetRequest{Role: "lan"}, &NetAnswer{}); code != http.StatusOK {
		t.Errorf("reset of lan: %d", code)
	}
	if slices.ContainsFunc(link.calls(), func(c []string) bool { return c[0] == "networkctl" }) {
		t.Errorf("a refused change reached networkd: %v", link.calls())
	}
	// lan recorded by `network attach`: its NIC not there yet, then there under the kernel's name.
	if err := os.WriteFile(filepath.Join(h.O.Dir, interfacesFile), []byte("- role: lan\n  mac: 02:00:00:00:00:05\n  mode: none\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, want := range []roleHint{
		{"its NIC (02:00:00:00:00:05) is recorded, and not on this VM yet", "add it on the VM's platform with that MAC and restart the VM"},
		{"its NIC (02:00:00:00:00:05) is recorded and there, and is named so from the next boot", "`sudo notes-stow reboot`"},
	} {
		unnamed(want.Why, want.Fix)
		// The kernel's NICs, as `network attach` and the install's warning read them.
		fakeSys(t, h.SysNet, h.UdevData, map[string]fakeNIC{"ens10": {"02:00:00:00:00:05", "virtio_net", ""}})
	}
	// The driver recorded is a virtual function's (the NIC there at the attach was a physical
	// Mellanox one) and the NIC's is not: its link does not match it, so the attach again does.
	if err := os.WriteFile(filepath.Join(h.O.Dir, interfacesFile), []byte("- role: lan\n  mac: 02:00:00:00:00:05\n  driver: mlx5_core\n  mode: dhcp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unnamed("its NIC (02:00:00:00:00:05) is recorded and there, but its link matches Driver=mlx5_core (the driver recorded: mlx5_core), not the NIC's (virtio_net)",
		"`sudo notes-stow network attach --role lan --mac 02:00:00:00:00:05 --mode dhcp`, run with the NIC there, records its driver, then `sudo notes-stow reboot` names it")
	// A record that does not decode, a 0.39.0 pin entry among them: its refusal, with its fix,
	// as the role's.
	for _, c := range []struct{ record, fix string }{
		{"- role: lan\n  mac: 02:00:00:00:00:05\n- role: uplink\n  mac: 02:00:00:00:00:aa\n  pin: true\n", "pin the VM's NIC by hand (\"A VM seeded before 0.40.0\" in docs/vm.md), remove the entries with `pin: true` from " + filepath.Join(h.O.Dir, interfacesFile) + ", then run this again"},
		{"- role: lan\n  colour: red\n", "fix " + filepath.Join(h.O.Dir, interfacesFile) + " (a list of role, mac, driver, mode and address), or remove it and run `network attach` again for each NIC it held"},
	} {
		if err := os.WriteFile(filepath.Join(h.O.Dir, interfacesFile), []byte(c.record), 0o644); err != nil {
			t.Fatal(err)
		}
		e = errOf{}
		if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{Role: "lan", NetConfig: NetConfig{Mode: "dhcp"}}, &e); code != http.StatusConflict || !e.Refused || e.Fix != c.fix ||
			!strings.HasPrefix(e.Error, "role lan: no NIC is named so on this VM: "+filepath.Join(h.O.Dir, interfacesFile)) {
			t.Errorf("%q: %d %+v", c.record, code, e)
		}
	}
	if err := os.Remove(filepath.Join(h.O.Dir, interfacesFile)); err != nil {
		t.Fatal(err)
	}
	// lan named by the seed's MAC, which no NIC has: the platform's NIC, never `network attach`,
	// which refuses a role the seed names.
	if err := os.WriteFile(filepath.Join(h.Networkd, "10-notes-lan.link"), []byte(interfaceLink("notes", MachineInterface{Role: "lan", MAC: "02:00:00:00:00:0c"})), 0o644); err != nil {
		t.Fatal(err)
	}
	unnamed("the VM's cloud-config names it by its MAC (02:00:00:00:00:0c)", "add the NIC of that MAC on the VM's platform (or fix the MAC of the one there) and restart the VM")
	// lan named by the seed's original name, never pinned: its NIC alone at a boot, never
	// `network attach`.
	if err := os.WriteFile(filepath.Join(h.Networkd, "10-notes-lan.link"), []byte(interfaceLink("notes", MachineInterface{Role: "lan"})), 0o644); err != nil {
		t.Fatal(err)
	}
	unnamed("the VM's cloud-config names it by its original name, as the VM's only NIC",
		"leave its NIC alone on the VM (remove the others on its platform) and restart the VM, which names it (and, for a VM seeded by Stowage 0.40.0 or later, pins it to that NIC's MAC, so NICs added after it keep their names)")
	// Another role's NIC recorded by `network attach`: kept, every other NIC removed.
	if err := os.WriteFile(filepath.Join(h.O.Dir, interfacesFile), []byte("- role: edge\n  mac: 02:00:00:00:00:0e\n  mode: none\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unnamed("the VM's cloud-config names it by its original name, as the VM's only NIC",
		"leave its NIC alone on the VM (remove the others on its platform but the NICs `network attach` recorded, which keep their roles: edge's 02:00:00:00:00:0e) and restart the VM, which names it (and, for a VM seeded by Stowage 0.40.0 or later, pins it to that NIC's MAC, so NICs added after it keep their names)")
	// lan named by the boot's pin (its .link file's MAC and Driver=): its NIC gone, or of a driver
	// the pin does not match, one free NIC left alone is pinned in its place; there and matched, a
	// reboot names it.
	pinFix := "leave one NIC on the VM that no other role names (remove the others on its platform but the NICs `network attach` recorded, which keep their roles: edge's 02:00:00:00:00:0e) and restart the VM, which pins the role to that NIC"
	if err := os.WriteFile(filepath.Join(h.Networkd, "10-notes-lan.link"), []byte(strings.NewReplacer(macMark, "02:00:00:00:00:0d", driverMark, driverValue("")).Replace(pinnedLink("notes", "lan"))), 0o644); err != nil {
		t.Fatal(err)
	}
	pinned := "the VM's boot pinned it to its NIC's MAC (02:00:00:00:00:0d, Driver=!mlx4_en mlx5_core mana)"
	unnamed(pinned+", and no NIC of that MAC is on this VM", pinFix)
	fakeSys(t, h.SysNet, h.UdevData, map[string]fakeNIC{"ens11": {"02:00:00:00:00:0d", "mlx5_core", ""}})
	unnamed(pinned+", and the NIC of that MAC here has the driver mlx5_core, which the pin does not match", pinFix)
	if err := os.RemoveAll(filepath.Join(h.SysNet, "ens11")); err != nil {
		t.Fatal(err)
	}
	fakeSys(t, h.SysNet, h.UdevData, map[string]fakeNIC{"ens12": {"02:00:00:00:00:0d", "virtio_net", ""}})
	unnamed(pinned+", and its NIC is here", "`sudo notes-stow reboot` names it")
	if err := os.Remove(filepath.Join(h.O.Dir, interfacesFile)); err != nil {
		t.Fatal(err)
	}
	// A .link file naming no NIC (empty, cut short, or foreign): the file to restore, never
	// `network attach`, which refuses a role the seed names.
	for _, cut := range []string{"", "# notes's interface lan.\n[Match]\n", "[Match]\nName=eth0\n\n[Link]\nName=lan\n"} {
		if err := os.WriteFile(filepath.Join(h.Networkd, "10-notes-lan.link"), []byte(cut), 0o644); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(h.Networkd, "10-notes-lan.link")
		unnamed("its .link file "+path+" names no NIC (neither a MACAddress= nor an OriginalName=)", "restore "+path+" as the VM's cloud-config writes it: `sudo notes-stow reboot` writes it again")
	}
	// A .link file that does not read: what names lan is not known, refused with what to restore.
	if err := os.Remove(filepath.Join(h.Networkd, "10-notes-lan.link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(h.Networkd, "10-notes-lan.link"), 0o755); err != nil {
		t.Fatal(err)
	}
	e = errOf{}
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{Role: "lan", NetConfig: NetConfig{Mode: "dhcp"}}, &e); code != http.StatusConflict || !e.Refused ||
		!strings.HasPrefix(e.Error, "role lan: no NIC is named so on this VM: what names one is not known (") || !strings.Contains(e.Error, "10-notes-lan.link") ||
		!strings.HasPrefix(e.Fix, "restore what does not read (") || !strings.HasSuffix(e.Fix, "), then set its network") || !strings.Contains(e.Fix, "10-notes-lan.link") {
		t.Errorf("a .link file unread: %d %+v", code, e)
	}
	if err := os.Remove(filepath.Join(h.Networkd, "10-notes-lan.link")); err != nil {
		t.Fatal(err)
	}
	// A release without a secondary NIC.
	_, plain := roleDaemon(t)
	plain.R = testRelease(t, "1.0.0")
	if code := call(t, plain.Handler(), http.MethodPost, "/v1/network", NetRequest{Role: "edge", NetConfig: NetConfig{Mode: "dhcp"}}, &e); code != http.StatusBadRequest || !strings.Contains(e.Error, "declares no secondary NIC") {
		t.Errorf("no secondary: %d %q", code, e.Error)
	}
}

// A record that does not read sets nothing and stops every change, confirm and revert, as it may
// hold the change awaiting confirmation: each names its `network reset`, which drops it whatever
// another NIC does, its revert timer kept; `network` answers the rest.
func TestARecordThatDoesNotReadStopsChangesUntilItsReset(t *testing.T) {
	t.Parallel()
	link, h := roleDaemon(t)
	timers := manualTimers(h)
	api := h.Handler()
	var a NetAnswer
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{NetConfig: NetConfig{Mode: "static", Address: "10.0.0.60/24", Gateway: "10.0.0.1"}, RevertSecs: intp(maxRevertSecs)}, fresh(&a)); code != http.StatusOK || a.Pending == nil {
		t.Fatalf("%d %+v", code, a)
	}
	if err := os.WriteFile(RoleNetworkFile(h.O.Dir, "edge"), []byte("version: 2\nconfig: {mode: static, address: 192.0.2.10/24, gateway: 192.0.2.1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var e errOf
	for _, c := range []struct {
		path string
		body any
	}{{"/v1/network", NetRequest{Role: "lan", NetConfig: NetConfig{Mode: "none"}}}, {"/v1/network", NetRequest{Role: "edge", NetConfig: NetConfig{Mode: "none"}}}, {"/v1/network/confirm", nil}, {"/v1/network/revert", nil}} {
		if code := call(t, api, http.MethodPost, c.path, c.body, &e); code == http.StatusOK || !strings.Contains(e.Error, "`notes-stow network reset --role edge` drops it") {
			t.Errorf("%s %+v: %d %q", c.path, c.body, code, e.Error)
		}
	}
	var n Network
	if code := call(t, api, http.MethodGet, "/v1/network", nil, &n); code != http.StatusOK || n.Roles[0].Override != nil || !strings.Contains(n.Roles[0].Error, "network reset --role edge") || n.Pending == nil || n.Pending.Role != "" {
		t.Errorf("network: %d %+v", code, n)
	}
	if code := call(t, api, http.MethodPost, "/v1/network/reset", NetResetRequest{Role: "edge"}, fresh(&a)); code != http.StatusOK || !strings.Contains(a.Message, "dropped, the settings the cloud-config or `network attach` gave edge apply") {
		t.Fatalf("%d %+v", code, a)
	}
	if fileExists(RoleNetworkFile(h.O.Dir, "edge")) || nicAddr(link, "edge") != "192.0.2.5/24" {
		t.Error("the record is left")
	}
	h.netMu.Lock()
	armed := h.netReverts[""] != nil
	h.netMu.Unlock()
	if !armed {
		t.Error("the drop disarmed the primary's window")
	}
	if code := call(t, api, http.MethodPost, "/v1/network/confirm", nil, fresh(&a)); code != http.StatusOK || a.Message != "confirmed" {
		t.Fatalf("confirm once dropped: %d %+v", code, a)
	}
	// The other way: the primary's record dropped beside edge's window, which still ends.
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{Role: "edge", NetConfig: NetConfig{Mode: "none"}, RevertSecs: intp(minRevertSecs)}, fresh(&a)); code != http.StatusOK || a.Pending == nil {
		t.Fatalf("%d %+v", code, a)
	}
	if err := os.WriteFile(NetworkFile(h.O.Dir), []byte("bad: key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := call(t, api, http.MethodPost, "/v1/network/confirm", nil, &e); code == http.StatusOK || !strings.Contains(e.Error, "`notes-stow network reset` drops it") {
		t.Errorf("confirm: %d %q", code, e.Error)
	}
	if code := call(t, api, http.MethodPost, "/v1/network/reset", NetResetRequest{}, fresh(&a)); code != http.StatusOK || !strings.Contains(a.Message, "dropped") {
		t.Fatalf("%d %+v", code, a)
	}
	<-timers.fire(t) // edge's window ends
	eventually(t, "edge's change undone at its window's end", func() bool { return nicAddr(link, "edge") == "192.0.2.5/24" })
}

// A revert armed for one NIC, retries included, is its own: another NIC's change leaves it armed;
// the same NIC's change replaces it.
func TestARevertArmedIsItsNICsAlone(t *testing.T) {
	t.Parallel()
	link, h := roleDaemon(t)
	api := h.Handler()
	h.netMu.Lock()
	h.armRevert("edge", time.Now().Add(time.Hour), 2, "a test", func(context.Context) error { return nil })
	h.netMu.Unlock()
	var e errOf
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{NetConfig: NetConfig{Mode: "dhcp"}, RevertSecs: intp(0)}, &e); code != http.StatusOK {
		t.Errorf("a primary change: %d %q", code, e.Error)
	}
	if code := call(t, api, http.MethodPost, "/v1/network/reset", NetResetRequest{RevertSecs: intp(0)}, &e); code != http.StatusOK {
		t.Errorf("a primary reset: %d %q", code, e.Error)
	}
	h.netMu.Lock()
	armed := h.netReverts["edge"] != nil
	h.netMu.Unlock()
	if !armed {
		t.Error("the primary's change disarmed edge's revert")
	}
	var a NetAnswer
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{Role: "edge", NetConfig: NetConfig{Mode: "none"}, RevertSecs: intp(0)}, fresh(&a)); code != http.StatusOK {
		t.Fatalf("edge's change: %d %+v", code, a)
	}
	h.netMu.Lock()
	armed = h.netReverts["edge"] != nil
	h.netMu.Unlock()
	if armed || nicAddr(link, "edge") != "" {
		t.Errorf("edge's change left the revert armed (%t) or did not apply", armed)
	}
	// A pending change on another NIC is named.
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{Role: "edge", NetConfig: NetConfig{Mode: "dhcp"}, RevertSecs: intp(maxRevertSecs)}, fresh(&a)); code != http.StatusOK || a.Pending == nil {
		t.Fatalf("%d %+v", code, a)
	}
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{NetConfig: NetConfig{Mode: "dhcp"}}, &e); code != http.StatusConflict || !strings.Contains(e.Error, "edge's network change awaits confirmation") {
		t.Errorf("%d %q", code, e.Error)
	}
}

// The boot renders each secondary's override under /run, a change never confirmed undone, the
// banner telling them after the primary's line.
func TestTheBootRendersTheSecondariesOverrides(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := newNetFiles("notes", "notes-stow", dir, t.TempDir(), t.TempDir(), t.TempDir(), []string{"edge", "lan"})
	edge, lan := f.forRole("edge"), f.forRole("lan")
	prev := &NetConfig{Mode: "static", Address: "192.0.2.10/24"}
	if err := edge.write(netState{Config: &NetConfig{Mode: "dhcp"}, Pending: &netPending{ID: "w", Deadline: time.Now().Add(time.Hour), Previous: prev}}); err != nil {
		t.Fatal(err)
	}
	if err := lan.write(netState{Config: &NetConfig{Mode: "none"}}); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	for _, g := range []netFiles{edge, lan, f} {
		if err := g.boot(&log); err != nil {
			t.Fatal(err)
		}
	}
	if u, _ := os.ReadFile(filepath.Join(f.run, "09-notes-edge-override.network")); !strings.Contains(string(u), "Address=192.0.2.10/24") || !strings.Contains(log.String(), "not confirmed before the reboot") {
		t.Errorf("edge:\n%s\nlog: %s", u, log.String())
	}
	if u, _ := os.ReadFile(filepath.Join(f.run, "09-notes-lan-override.network")); !strings.Contains(string(u), "ConfigureWithoutCarrier=yes") {
		t.Errorf("lan:\n%s", u)
	}
	if st, err := edge.read(); err != nil || st.Pending != nil || !reflect.DeepEqual(st.Config, prev) {
		t.Errorf("%+v %v", st, err)
	}
	if fileExists(f.overrideUnit()) || len(dirNames(t, f.etc)) != 0 {
		t.Errorf("the primary's directory holds %v", dirNames(t, f.etc))
	}
	want := "network: the VM's cloud-config (DHCP, or its static address)\nedge: 192.0.2.10/24; set with `notes-stow network set --role edge`\nlan: no address (the link up); set with `notes-stow network set --role lan`\n"
	if b, _ := os.ReadFile(f.banner()); string(b) != want {
		t.Errorf("banner:\n%s", b)
	}
	// None left: the file goes.
	if err := lan.write(netState{}); err != nil {
		t.Fatal(err)
	}
	if err := lan.boot(io.Discard); err != nil || fileExists(lan.overrideUnit()) {
		t.Errorf("%v: lan's file is left", err)
	}
	// A record that does not read sets nothing, naming the verb with its role.
	if err := os.WriteFile(edge.state, []byte("config: {mode: none, ipv6: true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := edge.boot(io.Discard); err == nil || !strings.Contains(err.Error(), "the network override of edge") || !strings.Contains(err.Error(), "network reset --role edge") || fileExists(edge.overrideUnit()) {
		t.Errorf("%v", err)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	es, _ := os.ReadDir(dir)
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// The daemon's start undoes a secondary's change not confirmed, and renders a record its /run file
// lacks.
func TestTheDaemonStartingUndoesASecondarysChange(t *testing.T) {
	t.Parallel()
	link, h := roleDaemon(t)
	edge := h.netFiles().forRole("edge")
	if err := edge.write(netState{Config: &NetConfig{Mode: "none"}, Pending: &netPending{ID: "w", Deadline: time.Now().Add(time.Hour), Previous: &NetConfig{Mode: "static", Address: "192.0.2.10/24"}}}); err != nil {
		t.Fatal(err)
	}
	h.NetResume()
	if st := roleState(t, h, "edge"); st.Pending != nil || st.Config == nil || st.Config.Address != "192.0.2.10/24" || nicAddr(link, "edge") != "192.0.2.10/24" {
		t.Errorf("%+v, edge runs %q", st, nicAddr(link, "edge"))
	}
	if err := os.Remove(edge.overrideUnit()); err != nil {
		t.Fatal(err)
	}
	h.NetResume()
	if !fileExists(edge.overrideUnit()) {
		t.Error("the record is not rendered again")
	}
}

func TestValidSecondaryNetworks(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ in, want NetConfig }{
		{NetConfig{Mode: "dhcp"}, NetConfig{Mode: "dhcp"}},
		{NetConfig{Mode: "none"}, NetConfig{Mode: "none"}},
		{NetConfig{Mode: "static", Address: "192.0.2.10/24"}, NetConfig{Mode: "static", Address: "192.0.2.10/24"}},
		{NetConfig{Mode: "static", Address: "192.0.2.10/32"}, NetConfig{Mode: "static", Address: "192.0.2.10/32"}},
	} {
		if got, err := validRoleNet(c.in); err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%+v: %+v %v", c.in, got, err)
		}
	}
	for _, bad := range []NetConfig{{Mode: "static", Address: "2001:db8::1/64"}, {Mode: "static", Address: "192.0.2.10"}, {Mode: "dhcp", Address6: "2001:db8::1/64"}, {Mode: "none", Gateway6: "fe80::1"}, {Mode: ""}} {
		if _, err := validRoleNet(bad); err == nil {
			t.Errorf("%+v is valid", bad)
		}
	}
}

func TestNetworkPrintsTheSecondaries(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	printNetwork(&b, "notes-stow", Network{Roles: []RoleNetwork{
		{Role: "edge", MAC: "02:00:00:00:00:04", Addresses: []string{"192.0.2.10/24"}, Override: &NetConfig{Mode: "static", Address: "192.0.2.10/24"}},
		{Role: "lan", Addresses: []string{}},
		{Role: "mgmt", MAC: "02:00:00:00:00:05", Addresses: []string{}},
		{Role: "wan", MAC: "02:00:00:00:00:06", Addresses: []string{}, Error: "network-wan.yaml: bad"},
	}, Pending: &NetPending{Role: "edge", SecsLeft: 30}})
	for _, want := range []string{
		"role       edge: 02:00:00:00:00:04 192.0.2.10/24; set by `notes-stow network set --role edge`: 192.0.2.10/24\n",
		"role       lan: no NIC is named so; set by the cloud-config or `network attach`\n",
		"role       mgmt: 02:00:00:00:00:05 no address; set by the cloud-config or `network attach`\n",
		"role       wan: 02:00:00:00:00:06 no address; set by the cloud-config or `network attach`, as its override's record does not read: network-wan.yaml: bad\n",
		"pending    not confirmed: in 30 s, edge's settings from the cloud-config or `network attach` comes back unless `notes-stow network confirm` keeps it\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %q in:\n%s", want, b.String())
		}
	}
}

// The daemon's start applies a record its networkd file lacks, and tries again while it fails,
// on that NIC's own timer.
func TestTheDaemonStartingRetriesARecordItCouldNotApply(t *testing.T) {
	t.Parallel()
	link, h := roleDaemon(t)
	timers := manualTimers(h)
	link.reloadFails = 1
	if err := h.netFiles().forRole("edge").write(netState{Config: &NetConfig{Mode: "none"}}); err != nil {
		t.Fatal(err)
	}
	h.NetResume()
	h.netMu.Lock()
	armed := h.netReverts["edge"] != nil
	h.netMu.Unlock()
	if !armed || nicAddr(link, "edge") != "192.0.2.5/24" {
		t.Fatalf("armed %t, edge runs %q", armed, nicAddr(link, "edge"))
	}
	<-timers.fire(t)
	if nicAddr(link, "edge") != "" {
		t.Errorf("edge runs %q", nicAddr(link, "edge"))
	}
}

// Two changes awaiting confirmation at the daemon's start (a record repaired by hand) are each
// undone, each on its NIC's own timer while it fails: one's retry never disarms the other's.
func TestTheDaemonStartingUndoesEachPendingChangeOnItsOwnTimer(t *testing.T) {
	t.Parallel()
	link, h := roleDaemon(t)
	timers := manualTimers(h)
	api := h.Handler()
	var a NetAnswer
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{Role: "edge", NetConfig: NetConfig{Mode: "none"}, RevertSecs: intp(maxRevertSecs)}, fresh(&a)); code != http.StatusOK || a.Pending == nil {
		t.Fatalf("%d %+v", code, a)
	}
	edge := roleState(t, h, "edge")
	if code := call(t, api, http.MethodPost, "/v1/network/confirm", nil, fresh(&a)); code != http.StatusOK {
		t.Fatalf("%d %+v", code, a)
	}
	if code := call(t, api, http.MethodPost, "/v1/network", NetRequest{NetConfig: NetConfig{Mode: "static", Address: "10.0.0.60/24", Gateway: "10.0.0.1"}, RevertSecs: intp(maxRevertSecs)}, fresh(&a)); code != http.StatusOK || a.Pending == nil {
		t.Fatalf("%d %+v", code, a)
	}
	// edge's window put back by hand beside the primary's.
	if err := h.netFiles().forRole("edge").write(edge); err != nil {
		t.Fatal(err)
	}
	h.netMu.Lock()
	h.netDisarm("")
	h.netMu.Unlock()
	link.mu.Lock()
	link.reloadFails = 2
	link.mu.Unlock()
	h.NetResume()
	h.netMu.Lock()
	armed := len(h.netReverts)
	h.netMu.Unlock()
	if armed != 2 {
		t.Fatalf("%d reverts armed", armed)
	}
	<-timers.fire(t)
	if addr, _, _ := link.live(); addr != leaseAddr || nicAddr(link, "edge") != "192.0.2.5/24" {
		t.Errorf("the primary runs %s, edge %q", addr, nicAddr(link, "edge"))
	}
	if roleState(t, h, "").Pending != nil || roleState(t, h, "edge").Pending != nil {
		t.Error("a change still awaits confirmation")
	}
}

// A drop disarms a revert of its own NIC's record, which would bring the override back; one that
// fails leaves the record, so a re-run drops it.
func TestADropDisarmsItsOwnRevertAndIsRetried(t *testing.T) {
	t.Parallel()
	link, h := roleDaemon(t)
	api := h.Handler()
	h.netMu.Lock()
	h.armRevert("edge", time.Now().Add(time.Hour), 2, "a test", func(context.Context) error { return nil })
	h.netMu.Unlock()
	if err := os.WriteFile(RoleNetworkFile(h.O.Dir, "edge"), []byte("bad: key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link.mu.Lock()
	link.reloadFails = 1
	link.mu.Unlock()
	var e errOf
	if code := call(t, api, http.MethodPost, "/v1/network/reset", NetResetRequest{Role: "edge"}, &e); code == http.StatusOK || !fileExists(RoleNetworkFile(h.O.Dir, "edge")) {
		t.Fatalf("a drop that fails: %d %q", code, e.Error)
	}
	var a NetAnswer
	if code := call(t, api, http.MethodPost, "/v1/network/reset", NetResetRequest{Role: "edge"}, &a); code != http.StatusOK || !strings.Contains(a.Message, "dropped") || fileExists(RoleNetworkFile(h.O.Dir, "edge")) {
		t.Fatalf("the re-run: %d %+v", code, a)
	}
	h.netMu.Lock()
	armed := h.netReverts["edge"] != nil
	h.netMu.Unlock()
	if armed {
		t.Error("edge's revert is still armed")
	}
}

// A role recorded by `network attach` whose NIC's presence does not read: an error, never a hint
// guessed.
func TestAnUnnamedRolesHintNeverGuesses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, interfacesFile), []byte("- role: lan\n  mac: 02:00:00:00:00:05\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unread := errors.New("unread")
	if hint, err := unnamedRoleHint(t.TempDir(), "notes", "notes-stow", dir, "lan", vmNICs{drivers: func(string) ([]string, error) { return nil, unread }}); !errors.Is(err, unread) || hint != (roleHint{}) {
		t.Errorf("%q %v", hint, err)
	}
	networkd := t.TempDir()
	if err := os.Mkdir(filepath.Join(networkd, "10-notes-lan.link"), 0o755); err != nil {
		t.Fatal(err)
	}
	if hint, err := unnamedRoleHint(networkd, "notes", "notes-stow", dir, "lan", vmNICs{drivers: func(string) ([]string, error) { return []string{"virtio_net"}, nil }}); err == nil || hint != (roleHint{}) {
		t.Errorf("the seed's .link unread: %q %v", hint, err)
	}
}

// A role whose NIC is here and matched by its .link file but not named: a reboot names it only
// while no other device holds its name; one of several NICs of the seed's MAC is named by none.
func TestAnUnnamedRolesHintSaysWhyItsNICIsNotNamed(t *testing.T) {
	t.Parallel()
	const mac = "02:00:00:00:00:05"
	networkd, dir := t.TempDir(), t.TempDir()
	byMAC := interfaceLink("notes", MachineInterface{Role: "lan", MAC: mac})
	pinned := strings.NewReplacer(macMark, mac, driverMark, "virtio_net").Replace(pinnedLink("notes", "lan"))
	reboot := "`sudo notes-stow reboot` names it"
	held := "remove or rename that device (or remove the record that names it so: its .link or .netdev file), then `sudo notes-stow reboot` names it"
	for _, c := range []struct {
		name, link string
		nics       int
		holder     string
		why, fix   string
	}{
		{"by MAC, none", byMAC, 0, "", "the VM's cloud-config names it by its MAC (" + mac + ")", "add the NIC of that MAC on the VM's platform (or fix the MAC of the one there) and restart the VM"},
		{"by MAC, here", byMAC, 1, "", "the VM's cloud-config names it by its MAC (" + mac + "), and its NIC is here", reboot},
		{"by MAC, several", byMAC, 2, "", "the VM's cloud-config names it by its MAC (" + mac + "), and 2 NICs of this VM have that MAC, so the boot names none of them",
			"remove the other NICs with that MAC on the VM's platform (or give each a MAC of its own) and restart the VM"},
		{"by MAC, the name held", byMAC, 1, "02:00:00:00:00:0b", "the VM's cloud-config names it by its MAC (" + mac + "), and its NIC is here, but another device holds the name lan (MAC 02:00:00:00:00:0b)", held},
		{"pinned, here", pinned, 1, "", "the VM's boot pinned it to its NIC's MAC (" + mac + ", Driver=virtio_net), and its NIC is here", reboot},
		{"pinned, the name held by a device of no MAC", pinned, 1, "-", "the VM's boot pinned it to its NIC's MAC (" + mac + ", Driver=virtio_net), and its NIC is here, but another device holds the name lan (no MAC)", held},
	} {
		if err := os.WriteFile(filepath.Join(networkd, "10-notes-lan.link"), []byte(c.link), 0o644); err != nil {
			t.Fatal(err)
		}
		there := vmNICs{
			drivers: func(m string) ([]string, error) {
				if m != mac {
					return nil, nil
				}
				return slices.Repeat([]string{"virtio_net"}, c.nics), nil
			},
			holder: func(name string) (string, bool, error) {
				if name != "lan" || c.holder == "" {
					return "", false, nil
				}
				return strings.TrimPrefix(c.holder, "-"), true, nil
			},
		}
		if hint, err := unnamedRoleHint(networkd, "notes", "notes-stow", dir, "lan", there); err != nil || hint != (roleHint{c.why, c.fix}) {
			t.Errorf("%s: %v\n%q\nwant %q", c.name, err, hint, roleHint{c.why, c.fix})
		}
	}
	// Who holds the name does not read: an error, never a reboot promised.
	unread := errors.New("unread")
	there := vmNICs{
		drivers: func(string) ([]string, error) { return []string{"virtio_net"}, nil },
		holder:  func(string) (string, bool, error) { return "", false, unread },
	}
	if hint, err := unnamedRoleHint(networkd, "notes", "notes-stow", dir, "lan", there); !errors.Is(err, unread) || hint != (roleHint{}) {
		t.Errorf("the holder unread: %q %v", hint, err)
	}
	// nicsThere's holder: any device of the name, its MAC.
	sys, udev := t.TempDir(), t.TempDir()
	fakeSys(t, sys, udev, map[string]fakeNIC{"lan": {"02:00:00:00:00:0B", "", ""}})
	h := nicsThere(sys, udev).holder
	if m, ok, err := h("lan"); m != "02:00:00:00:00:0b" || !ok || err != nil {
		t.Errorf("held: %q %v %v", m, ok, err)
	}
	if m, ok, err := h("edge"); m != "" || ok || err != nil {
		t.Errorf("free: %q %v %v", m, ok, err)
	}
}
