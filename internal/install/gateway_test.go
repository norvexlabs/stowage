package install

import (
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/spec"
)

// gwHost is a Docker host's programs: nft holds the tables loaded, iptables the jumps to chains.
type gwHost struct {
	ran    [][]string
	tables map[string]bool
	jumps  int
	chain  bool
	// noNetworkd: systemd-networkd does not run; reloadErr fails `networkctl reload`; xErr fails
	// deleting a chain.
	noNetworkd                            bool
	reloadErr, xErr, deleteErr, enableErr error
}

func (h *gwHost) Run(_ context.Context, c Cmd) ([]byte, error) {
	h.ran = append(h.ran, c.Args)
	a := c.Args
	switch {
	case a[0] == "systemctl" && a[1] == "enable":
		return nil, h.enableErr
	case a[0] == "systemctl" && a[1] == "is-active":
		if h.noNetworkd {
			return nil, errors.New("inactive")
		}
	case a[0] == "networkctl" && a[1] == "reload":
		if h.noNetworkd {
			return nil, errors.New("Failed to connect to bus")
		}
		return nil, h.reloadErr
	case a[0] == "nft" && a[1] == "-f":
		raw, _ := os.ReadFile(a[2])
		for _, l := range strings.Split(string(raw), "\n") {
			if n, ok := strings.CutPrefix(l, "delete table inet "); ok {
				h.tables[n] = true
			}
		}
	case a[0] == "nft" && a[1] == "list":
		if !h.tables[a[len(a)-1]] {
			return nil, errors.New("No such file or directory")
		}
	case a[0] == "nft" && a[1] == "delete":
		if h.deleteErr != nil {
			return nil, h.deleteErr
		}
		delete(h.tables, a[len(a)-1])
	case strings.HasSuffix(a[0], "-restore"):
		h.chain = true
	case strings.HasPrefix(a[0], "ip") && slices.Contains(a, "-C"):
		if h.jumps == 0 {
			return nil, errors.New("Bad rule")
		}
	case strings.HasPrefix(a[0], "ip") && slices.Contains(a, "-I"):
		h.jumps++
	case strings.HasPrefix(a[0], "ip") && slices.Contains(a, "-D"):
		h.jumps--
	case strings.HasPrefix(a[0], "ip") && slices.Contains(a, "-L"):
		if !h.chain {
			return nil, errors.New("No chain")
		}
	case strings.HasPrefix(a[0], "ip") && slices.Contains(a, "-X"):
		if h.xErr != nil {
			return nil, h.xErr
		}
		h.chain = false
	}
	return nil, nil
}

func (h *gwHost) did(prefix string) bool {
	return slices.ContainsFunc(h.ran, func(a []string) bool { return strings.HasPrefix(strings.Join(a, " "), prefix) })
}

// gateway is a Docker install declaring a service's table, a tun role forwarded out of a NIC role
// with a route, and a NIC role with a policy rule (the operator's).
func gateway(t *testing.T, euid int) (*Installer, *gwHost, *[]string) {
	t.Helper()
	r := testRelease(t, "1.0.0")
	r.Nftables = map[string][]byte{"vpn": []byte("table inet vpn {\n}\n")}
	r.Spec.Host = &spec.Host{Interfaces: map[string]spec.HostInterface{
		"uplink": {Primary: true},
		"edge":   {RoutingPolicyRules: []map[string]string{{"From": "10.8.0.0/24", "Table": "100"}}},
		"vpn0":   {Kind: spec.InterfaceTUN, Forward: []string{"uplink"}, Routes: []map[string]string{{"Destination": "0.0.0.0/0", "Table": "100"}}},
	}}
	host := &gwHost{tables: map[string]bool{}}
	var log []string
	in := &Installer{R: r, O: Options{
		Dir: t.TempDir(), Host: host, Euid: func() int { return euid },
		NetworkdDir: t.TempDir(), GatewayRun: t.TempDir(), Systemd: t.TempDir(),
		Interfaces: map[string]string{"uplink": "eth0"},
	}}
	in.O.Log = writerFunc(func(p []byte) { log = append(log, string(p)) })
	return in, host, &log
}

func none(string) string { return "" }

type writerFunc func([]byte)

func (w writerFunc) Write(p []byte) (int, error) { w(p); return len(p), nil }

func (in *Installer) tunFile(role string) string {
	return filepath.Join(in.O.NetworkdDir, in.gatewayTunFile(role))
}

func TestADockerHostAppliesNothingOfTheGatewayWithoutConsent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, host, log := gateway(t, 0)
	if err := in.loadTables(ctx); err != nil {
		t.Fatal(err)
	}
	if err := in.gatewayStart(ctx, none); err != nil {
		t.Fatal(err)
	}
	if len(host.ran) != 0 {
		t.Errorf("ran %v", host.ran)
	}
	l := strings.Join(*log, "")
	for _, want := range []string{
		"the stack's network declarations, the services' nftables tables (vpn), the forwarding between host.interfaces, the networkd files of vpn0 are not in force on this host",
		"the MTU and policy routing of edge are this host's NICs', its operator's",
	} {
		if !strings.Contains(l, want) {
			t.Errorf("no %q in %q", want, l)
		}
	}
	if st := in.gatewayStatus(none); !strings.HasPrefix(st, "not in force") {
		t.Errorf("status %q", st)
	}
	// Nothing declared: nothing said.
	in.R.Nftables, in.R.Spec.Host = nil, nil
	*log = nil
	if err := in.gatewayStart(ctx, none); err != nil || len(*log) != 0 || in.gatewayStatus(none) != "" {
		t.Errorf("%v %q %q", err, *log, in.gatewayStatus(none))
	}
}

func TestAConsentedStartAppliesTheGatewayAsRootAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, host, log := gateway(t, 1000)
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
		t.Fatal(err)
	}
	if err := in.loadTables(ctx); err != nil {
		t.Fatal(err)
	}
	if err := in.gatewayStart(ctx, none); err != nil {
		t.Fatal(err)
	}
	if len(host.ran) != 0 || !strings.Contains(strings.Join(*log, ""), "applied by a start as root alone") {
		t.Errorf("not root: ran %v, log %q", host.ran, *log)
	}
	in.O.Euid = func() int { return 0 }
	if err := in.loadTables(ctx); err != nil {
		t.Fatal(err)
	}
	if err := in.gatewayStart(ctx, none); err != nil {
		t.Fatal(err)
	}
	chain := "STOWAGE-FWD-" + dirDigest(in.O.Dir)
	for _, want := range []string{"nft -f " + filepath.Join(in.O.Dir, TablesDir, "vpn.nft.next"), "iptables -w -I DOCKER-USER -j " + chain, "networkctl reload"} {
		if !host.did(want) {
			t.Errorf("did not run %q: %v", want, host.ran)
		}
	}
	tun := in.tunFile("vpn0")
	if raw, err := os.ReadFile(tun); err != nil || !strings.Contains(string(raw), "Name=vpn0") || !strings.Contains(string(raw), "[Route]\nDestination=0.0.0.0/0\nTable=100\n") {
		t.Errorf("tun file: %q %v", raw, err)
	}
	if left, _ := filepath.Glob(filepath.Join(in.O.NetworkdDir, "*")); len(left) != 1 {
		t.Errorf("a NIC's settings are written: %v", left)
	}
	if rec, _ := in.readGateway(); !maps.Equal(rec.Networkd, map[string]networkdFile{tun: {Role: "vpn0"}}) {
		t.Errorf("record %v", rec.Networkd)
	}
	if st := in.gatewayStatus(none); st != "applied at each start (tune --apply): tables vpn; forwarding; 1 networkd file(s)" {
		t.Errorf("status %q", st)
	}
	// Again: nothing changed, networkd is not reloaded.
	host.ran = nil
	if err := in.gatewayStart(ctx, none); err != nil || host.did("networkctl reload") {
		t.Errorf("%v: ran %v", err, host.ran)
	}
	// A file changed whose reload fails stays pending, and the next start reloads it.
	vpn0 := in.R.Spec.Host.Interfaces["vpn0"]
	vpn0.MTU = 1380
	in.R.Spec.Host.Interfaces["vpn0"] = vpn0
	host.reloadErr = errors.New("busy")
	if err := in.gatewayStart(ctx, none); err == nil {
		t.Fatal("a reload that fails")
	}
	if st := in.gatewayStatus(none); !strings.Contains(st, "forwarding; 1 networkd file(s) pending") {
		t.Errorf("status %q", st)
	}
	host.reloadErr, host.ran = nil, nil
	if err := in.gatewayStart(ctx, none); err != nil || !host.did("networkctl reload") {
		t.Errorf("pending: %v: ran %v", err, host.ran)
	}
	if rec, _ := in.readGateway(); rec.Networkd[tun].Pending {
		t.Errorf("still pending: %v", rec.Networkd)
	}
	// A release that drops the tun role: its file goes, networkd's directory stays.
	delete(in.R.Spec.Host.Interfaces, "vpn0")
	host.ran = nil
	if err := in.gatewayStart(ctx, none); err != nil || fileExists(tun) || !host.did("networkctl reload") {
		t.Errorf("the dropped role's file: %v, ran %v", err, host.ran)
	}
	if !fileExists(in.O.NetworkdDir) {
		t.Error("networkd's directory removed")
	}
}

// A host where networkd does not run is warned and changed in nothing; a record naming another
// file than the install's is refused, never followed.
func TestTheGatewaysNetworkdFilesAreTheInstallsAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, host, log := gateway(t, 0)
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
		t.Fatal(err)
	}
	host.noNetworkd = true
	if err := in.gatewayRouting(ctx); err != nil || !strings.Contains(strings.Join(*log, ""), "systemd-networkd does not run on this host: the networkd files of vpn0 are not written") {
		t.Fatalf("%v %q", err, *log)
	}
	if left, _ := filepath.Glob(filepath.Join(in.O.NetworkdDir, "*")); len(left) != 0 || host.did("networkctl") {
		t.Errorf("wrote %v, ran %v", left, host.ran)
	}
	host.noNetworkd = false
	victim := filepath.Join(in.O.NetworkdDir, "10-eth0.network")
	if err := os.WriteFile(victim, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		p, role string
		ok      bool
	}{
		{in.tunFile("vpn0"), "vpn0", true},
		{victim, "vpn0", false},
		{in.tunFile("vpn1"), "vpn0", false},
		{filepath.Join(in.O.NetworkdDir, "10-notes-vpn0.network"), "vpn0", false},
		{in.O.NetworkdDir + "/x/../" + in.gatewayTunFile("vpn0"), "vpn0", false},
		{in.tunFile("Bad"), "Bad", false},
	} {
		if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir(), Networkd: map[string]networkdFile{c.p: {Role: c.role}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := in.readGateway(); (err == nil) != c.ok {
			t.Errorf("%s (%s): %v", c.p, c.role, err)
		}
	}
	if err := in.writeGateway(gatewayRecord{Digest: "../../x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := in.readGateway(); err == nil || !strings.Contains(err.Error(), "is not 8 hex digits") {
		t.Errorf("a digest of another shape: %v", err)
	}
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir(), Networkd: map[string]networkdFile{victim: {Role: "vpn0"}}}); err != nil {
		t.Fatal(err)
	}
	// A start warns and leaves the files; a revert refuses before anything goes.
	*log = nil
	if err := in.gatewayRouting(ctx); err != nil || !strings.Contains(strings.Join(*log, ""), "which is not a networkd file of the install's: refused: the networkd files are left as they are (fix or remove it)") || !fileExists(victim) {
		t.Errorf("start: %v %q", err, *log)
	}
	if err := in.gatewayRevert(ctx); err == nil || !strings.Contains(err.Error(), "which is not a networkd file of the install's: refused: fix or remove it") || !fileExists(victim) || !in.gatewayRecorded() {
		t.Errorf("revert: %v", err)
	}
	// Nothing is applied from it: a start as root says so, and loads no table.
	*log, host.ran = nil, nil
	if err := in.loadTables(ctx); err != nil || in.gatewayStart(ctx, none) != nil || host.did("nft") || !strings.Contains(strings.Join(*log, ""), "nothing of the stack's network declarations is applied (fix it, or remove it and `notes-stow tune --apply` again)") {
		t.Errorf("start: %v %q %v", err, *log, host.ran)
	}
	if st := in.gatewayStatus(none); !strings.HasPrefix(st, "not applied: ") {
		t.Errorf("status %q", st)
	}
}

func TestTuneApplyConsentsAndRevertRemovesTheGateway(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, host, _ := gateway(t, 0)
	var started []string
	in.O.Exec = func(_ context.Context, exe string, args []string) error {
		started = append([]string{exe}, args...)
		// The start applies them, as a start does.
		if err := in.loadTables(ctx); err != nil {
			return err
		}
		return in.gatewayStart(ctx, none)
	}
	if err := in.gatewayApply(ctx); err != nil {
		t.Fatal(err)
	}
	name, content := in.gatewayUnit()
	if raw, err := os.ReadFile(filepath.Join(in.O.Systemd, name)); err != nil || string(raw) != content || !strings.Contains(content, "ExecStart="+filepath.Join(in.O.Dir, "notes-stow")+" --dir "+in.O.Dir+" --no-input --no-provision\n") || !strings.Contains(content, "WantedBy=multi-user.target") {
		t.Errorf("unit: %q %v", raw, err)
	}
	if !host.did("systemctl enable "+name) || !slices.Equal(started, []string{filepath.Join(in.O.Dir, "notes-stow"), "--dir", in.O.Dir, "--no-input", "--no-provision"}) {
		t.Errorf("ran %v, started %v", host.ran, started)
	}
	if !in.gatewayConsented() || !host.tables["vpn"] || host.jumps != 1 || !fileExists(in.tunFile("vpn0")) {
		t.Fatalf("consented %v, tables %v, jumps %d", in.gatewayConsented(), host.tables, host.jumps)
	}
	// A revert with networkd stopped, and a chain that does not go: the rest goes, the consent is
	// revoked, and a revert again removes the chain and the record.
	host.noNetworkd, host.xErr = true, errors.New("in use")
	if err := in.gatewayRevert(ctx); err == nil || !strings.Contains(err.Error(), "the consent is withdrawn, but not all it applied is removed (`notes-stow tune --revert` again removes the rest)") || strings.Contains(err.Error(), "networkctl") {
		t.Fatalf("revert: %v", err)
	}
	if in.gatewayConsented() || !in.gatewayRecorded() || host.tables["vpn"] || host.jumps != 0 || fileExists(filepath.Join(in.O.Systemd, name)) || fileExists(in.tunFile("vpn0")) {
		t.Errorf("left: consented %v, tables %v, jumps %d", in.gatewayConsented(), host.tables, host.jumps)
	}
	if st := in.gatewayStatus(none); !strings.HasPrefix(st, "revoked, but not all it applied is removed") {
		t.Errorf("status %q", st)
	}
	if !host.did("systemctl disable " + name) {
		t.Errorf("ran %v", host.ran)
	}
	// Revoked: a start as root applies nothing, and says what is left.
	host.ran = nil
	var log []string
	in.O.Log = writerFunc(func(p []byte) { log = append(log, string(p)) })
	if err := in.loadTables(ctx); err != nil || in.gatewayStart(ctx, none) != nil || host.did("nft") || host.did("iptables-restore") || !strings.Contains(strings.Join(log, ""), "the consent to the stack's network declarations is withdrawn, but not all it applied is removed") {
		t.Errorf("a revoked consent applied: %v %v %q", err, host.ran, log)
	}
	host.xErr = nil
	if err := in.gatewayRevert(ctx); err != nil || host.chain || in.gatewayRecorded() {
		t.Errorf("again: %v, chain %v, recorded %v", err, host.chain, in.gatewayRecorded())
	}
	// A table nft does not delete fails the revert.
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
		t.Fatal(err)
	}
	if err := in.loadTables(ctx); err != nil {
		t.Fatal(err)
	}
	host.deleteErr = errors.New("busy")
	if err := in.gatewayRevert(ctx); err == nil || !strings.Contains(err.Error(), "the nftables tables vpn are still loaded") {
		t.Errorf("a table left: %v", err)
	}
	// A unit systemd does not enable leaves the record as it was (here revoked, a table left), and
	// a unit it had: a revert still reaches them.
	host.enableErr = errors.New("no systemd")
	before, _ := os.ReadFile(in.path(GatewayFile))
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(in.path(GatewayFile), before, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := in.gatewayApply(ctx); err == nil || !in.gatewayRecorded() || in.gatewayConsented() {
		t.Errorf("a unit not enabled, a record kept: %v", err)
	}
	// None recorded and no unit before: neither after.
	if err := os.Remove(in.path(GatewayFile)); err != nil {
		t.Fatal(err)
	}
	if err := in.gatewayApply(ctx); err == nil || in.gatewayRecorded() || fileExists(filepath.Join(in.O.Systemd, name)) {
		t.Errorf("a unit not enabled: %v, recorded %v", err, in.gatewayRecorded())
	}
	host.enableErr = nil
	// The install's lock held (a start runs): apply refuses.
	unlock, err := lockFile(in.path(lifecycleLock))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := in.gatewayApply(ctx); err == nil || !strings.Contains(err.Error(), "runs: wait for it to finish, then tune --apply again") {
		t.Errorf("apply under a start: %v", err)
	}
}

// A consented install whose release no longer declares anything empties what earlier ones applied;
// a start not as root says what it leaves.
func TestAConsentedReleaseDeclaringNothingClearsTheGateway(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, host, log := gateway(t, 0)
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
		t.Fatal(err)
	}
	if err := in.gatewayStart(ctx, none); err != nil {
		t.Fatal(err)
	}
	in.R.Nftables, in.R.Spec.Host = nil, nil
	in.O.Euid = func() int { return 1000 }
	*log = nil
	if err := in.gatewayStart(ctx, none); err != nil || !strings.Contains(strings.Join(*log, ""), "is removed by a start as root alone: this start removes nothing") {
		t.Errorf("not root: %v %q", err, *log)
	}
	in.O.Euid = func() int { return 0 }
	host.ran = nil
	if err := in.gatewayStart(ctx, none); err != nil {
		t.Fatal(err)
	}
	if fileExists(in.tunFile("vpn0")) || !host.did("networkctl reload") || !host.did("iptables-restore -w --noflush") || fileExists(in.forwardRecord()) {
		t.Errorf("left: ran %v", host.ran)
	}
	if st := in.gatewayStatus(none); !strings.HasPrefix(st, "consented (tune --apply): the release declares none") {
		t.Errorf("status %q", st)
	}
	// Nothing left: a start not as root says nothing.
	in.O.Euid = func() int { return 1000 }
	*log = nil
	if err := in.gatewayStart(ctx, none); err != nil || len(*log) != 0 {
		t.Errorf("%v %q", err, *log)
	}
}

// The VM keeps its one chain and its record under its firewall's directory.
func TestTheVMsForwardingKeepsItsChain(t *testing.T) {
	t.Parallel()
	in, _, _ := gateway(t, 0)
	in.O.Firewall = "/run/notes/nftables.d"
	if in.forwardChain() != ForwardChain || in.forwardRecord() != "/run/notes/nftables.d/notes-forward.rules" || !in.gatewayApplies() {
		t.Errorf("%s %s", in.forwardChain(), in.forwardRecord())
	}
	if in.gatewayConsented() || in.gatewayStatus(none) != "" {
		t.Error("the VM has no consent to give")
	}
}

// A record is its directory's: reached through a link it is the same; at another directory (the
// install moved or copied) it applies nothing, and --apply and --revert there refuse, naming it. A
// directory named relative is taken absolute.
func TestTheGatewaysRecordIsItsDirectorys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, host, log := gateway(t, 0)
	in.O.Exec = func(context.Context, string, []string) error { return nil }
	if err := in.gatewayApply(ctx); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(in.path(GatewayFile)); err != nil || st.Mode().Perm() != 0o644 {
		t.Errorf("the record's mode: %v %v", st, err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(in.O.Dir, link); err != nil {
		t.Fatal(err)
	}
	linked := *in
	linked.O.Dir = link
	if !linked.gatewayConsented() {
		t.Error("the directory through a link is not its own")
	}
	cp := filepath.Join(t.TempDir(), "copy")
	if err := os.MkdirAll(cp, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(in.path(GatewayFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cp, GatewayFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	copied := *in
	copied.O.Dir = cp
	*log, host.ran = nil, nil
	if err := copied.gatewayStart(ctx, none); err != nil || copied.gatewayConsented() || !strings.Contains(strings.Join(*log, ""), "is "+in.O.Dir+"'s (the directory moved or copied): nothing of the stack's network declarations is applied here") || host.did("iptables") {
		t.Errorf("copy: %v %q %v", err, *log, host.ran)
	}
	for name, run := range map[string]func() error{"revert": func() error { return copied.gatewayRevert(ctx) }, "apply": func() error { return copied.gatewayApply(ctx) }} {
		if err := run(); err == nil || !strings.Contains(err.Error(), "tune --revert --dir "+in.O.Dir+"` removes what it applied") || !in.gatewayConsented() {
			t.Errorf("%s elsewhere: %v", name, err)
		}
	}
	if st := copied.gatewayStatus(none); !strings.HasPrefix(st, "not applied: this install's") {
		t.Errorf("status %q", st)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel := *in
	rel.O.Dir = "relative-dir"
	if _, content := rel.gatewayUnit(); !strings.Contains(content, "ExecStart="+filepath.Join(cwd, "relative-dir", "notes-stow")+" --dir "+filepath.Join(cwd, "relative-dir")+" ") {
		t.Errorf("unit %q", content)
	}
}

// As root, the tables loaded are the kernel's: one a boot dropped is not.
func TestTheTablesLoadedAreTheKernelsAsRoot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, host, _ := gateway(t, 0)
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
		t.Fatal(err)
	}
	if err := in.loadTables(ctx); err != nil {
		t.Fatal(err)
	}
	if got := in.loadedTables(ctx); !slices.Equal(got, []string{"vpn"}) {
		t.Errorf("loaded %v", got)
	}
	host.tables = map[string]bool{} // a reboot
	if got := in.loadedTables(ctx); len(got) != 0 {
		t.Errorf("after a reboot %v", got)
	}
	in.O.Euid = func() int { return 1000 }
	if got := in.loadedTables(ctx); !slices.Equal(got, []string{"vpn"}) {
		t.Errorf("not root: the record's %v", got)
	}
}

// On a Docker host a NIC role is its NIC's (--interface, recorded): the forwarding names it; with
// none named, the pair is not applied, said, and status says so. A tun role is its own name.
func TestTheForwardingNamesARolesNIC(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, host, log := gateway(t, 0)
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
		t.Fatal(err)
	}
	var rules string
	in.O.Host = hostFunc(func(c Cmd) ([]byte, error) {
		if strings.HasSuffix(c.Args[0], "-restore") && c.Stdin != nil {
			raw, _ := io.ReadAll(c.Stdin)
			rules += string(raw)
		}
		return host.Run(ctx, c)
	})
	in.O.Interfaces = nil
	recorded := func(k string) string { return map[string]string{InterfaceVar("uplink"): "enp1s0"}[k] }
	if err := in.forwardRoles(ctx, recorded); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rules, "-i vpn0 -o enp1s0 -j ACCEPT") || !strings.Contains(rules, "-i enp1s0 -o vpn0 -m conntrack") || strings.Contains(rules, "uplink") {
		t.Errorf("rules %q", rules)
	}
	rules, *log = "", nil
	if err := os.Remove(in.forwardRecord()); err != nil {
		t.Fatal(err)
	}
	if err := in.forwardRoles(ctx, none); err != nil || strings.Contains(rules, "-A ") || !strings.Contains(strings.Join(*log, ""), "host.interfaces.uplink: no NIC of this host plays it (`--interface uplink=<NIC>` on a re-run names one): the forwarding through it is not applied") {
		t.Errorf("unmapped: %v %q %q", err, rules, *log)
	}
	if st := in.gatewayStatus(none); !strings.Contains(st, "no forwarding through uplink (no NIC named: --interface)") {
		t.Errorf("status %q", st)
	}
	// The VM names its NICs by their roles.
	in.O.Firewall = t.TempDir()
	if r, unmapped := forwardRules(in.R.Spec.Host, ForwardChain, func(r string) string { return in.roleDevice(r, none) }); !strings.Contains(string(r), "-i vpn0 -o uplink") || len(unmapped) != 0 {
		t.Errorf("VM: %q %v", r, unmapped)
	}
}

type hostFunc func(Cmd) ([]byte, error)

func (f hostFunc) Run(_ context.Context, c Cmd) ([]byte, error) { return f(c) }

// A recorded NIC name that is not an interface's (a wildcard, a space) names no device.
func TestARecordedNICIsCheckedAsAFlagIs(t *testing.T) {
	t.Parallel()
	in, _, _ := gateway(t, 0)
	in.O.Interfaces = nil
	for _, v := range []string{"+", "eth+", "eth 0", "eth0;x"} {
		if d := in.roleDevice("uplink", func(string) string { return v }); d != "" {
			t.Errorf("%q: %q", v, d)
		}
	}
	if d := in.roleDevice("uplink", func(string) string { return "enp1s0" }); d != "enp1s0" {
		t.Errorf("enp1s0: %q", d)
	}
}

// A directory gone (the install moved): a revert here removes what its record applied.
func TestAMovedInstallRevertsWhereItIs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, host, _ := gateway(t, 0)
	in.O.Exec = func(context.Context, string, []string) error {
		if err := in.loadTables(ctx); err != nil {
			return err
		}
		return in.gatewayStart(ctx, none)
	}
	if err := in.gatewayApply(ctx); err != nil {
		t.Fatal(err)
	}
	name, _ := in.gatewayUnit()
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(in.O.Dir, moved); err != nil {
		t.Fatal(err)
	}
	in.O.Dir = moved
	if in.gatewayConsented() || !strings.Contains(in.gatewayStatus(none), "which is gone (the directory moved)") {
		t.Errorf("moved: %v %q", in.gatewayConsented(), in.gatewayStatus(none))
	}
	if err := in.gatewayRevert(ctx); err != nil || in.gatewayRecorded() || host.tables["vpn"] || fileExists(filepath.Join(in.O.Systemd, name)) {
		t.Errorf("revert: %v, tables %v", err, host.tables)
	}
}
