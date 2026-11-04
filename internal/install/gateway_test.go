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
	stdin  []string // what each iptables-restore read
	// jumps are DOCKER-USER's jumps by chain, chain the chains made (a table's alone: one
	// iptables -C finds the jump the other's -I made).
	jumps map[string]int
	chain map[string]bool
	// noNetworkd: systemd-networkd does not run; reloadErr fails `networkctl reload`; xErr fails
	// deleting a chain.
	noNetworkd bool
	// noUser: iptables has no DOCKER-USER until one is made (-N), as before Docker starts;
	// noForward: Docker's FORWARD does not jump to it (its iptables off).
	noUser, noForward                     bool
	reloadErr, xErr, deleteErr, enableErr error
	restoreErr                            error // fails iptables-restore
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
		if h.restoreErr != nil {
			return nil, h.restoreErr
		}
		if c.Stdin != nil {
			b, _ := io.ReadAll(c.Stdin)
			h.stdin = append(h.stdin, string(b))
			for _, l := range strings.Split(string(b), "\n") {
				if n, ok := strings.CutPrefix(l, ":"); ok {
					h.chain[strings.Fields(n)[0]] = true
				}
			}
		}
	case a[0] == "iptables" && slices.Contains(a, "FORWARD"):
		if h.noForward {
			return nil, errors.New("Bad rule: no such jump")
		}
	case a[0] == "iptables" && h.noUser && slices.Contains(a, "-N"):
		h.noUser = false
	case a[0] == "iptables" && h.noUser && (slices.Contains(a, "-I") || slices.Contains(a, "-C") || slices.Contains(a, "-L") && a[len(a)-1] == "DOCKER-USER"):
		return nil, errors.New("No chain/target/match by that name")
	case strings.HasPrefix(a[0], "ip") && slices.Contains(a, "-C"):
		if h.jumps[a[len(a)-1]] == 0 {
			return nil, errors.New("Bad rule")
		}
	case strings.HasPrefix(a[0], "ip") && slices.Contains(a, "-I"):
		h.jumps[a[len(a)-1]]++
	case strings.HasPrefix(a[0], "ip") && slices.Contains(a, "-D"):
		h.jumps[a[len(a)-1]]--
	case strings.HasPrefix(a[0], "ip") && slices.Contains(a, "-L"):
		if !h.chain[a[len(a)-1]] {
			return nil, errors.New("No chain")
		}
	case strings.HasPrefix(a[0], "ip") && slices.Contains(a, "-X"):
		if h.xErr != nil {
			return nil, h.xErr
		}
		delete(h.chain, a[len(a)-1])
	}
	return nil, nil
}

// njumps counts DOCKER-USER's jumps, to every chain.
func (h *gwHost) njumps() int {
	n := 0
	for _, j := range h.jumps {
		n += j
	}
	return n
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
	host := &gwHost{tables: map[string]bool{}, jumps: map[string]int{}, chain: map[string]bool{}}
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
	if err := in.loadTables(ctx, none); err != nil {
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
	if err := in.loadTables(ctx, none); err != nil {
		t.Fatal(err)
	}
	if err := in.gatewayStart(ctx, none); err != nil {
		t.Fatal(err)
	}
	if len(host.ran) != 0 || !strings.Contains(strings.Join(*log, ""), "applied by a start as root alone") {
		t.Errorf("not root: ran %v, log %q", host.ran, *log)
	}
	in.O.Euid = func() int { return 0 }
	if err := in.loadTables(ctx, none); err != nil {
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
	if err := in.loadTables(ctx, none); err != nil || in.gatewayStart(ctx, none) != nil || host.did("nft") || !strings.Contains(strings.Join(*log, ""), "nothing of the stack's network declarations is applied (fix it, or remove it and `notes-stow tune --apply` again)") {
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
		if err := in.loadTables(ctx, none); err != nil {
			return err
		}
		return in.gatewayStart(ctx, none)
	}
	if err := in.gatewayApply(ctx); err != nil {
		t.Fatal(err)
	}
	name, content := in.gatewayUnit()
	if raw, err := os.ReadFile(filepath.Join(in.O.Systemd, name)); err != nil || string(raw) != content || !strings.Contains(content, "ExecStart=\""+filepath.Join(in.O.Dir, "notes-stow")+"\" --dir \""+in.O.Dir+"\" --no-input --no-provision\n") || !strings.Contains(content, "WantedBy=multi-user.target") {
		t.Errorf("unit: %q %v", raw, err)
	}
	tname, tcontent := in.gatewayTablesUnit()
	if raw, err := os.ReadFile(filepath.Join(in.O.Systemd, tname)); err != nil || string(raw) != tcontent || tname != "notes-gateway-"+in.gatewayDigest()+"-tables.service" ||
		!strings.Contains(tcontent, "ExecStart=\""+filepath.Join(in.O.Dir, "notes-stow")+"\" tables-boot --dir \""+in.O.Dir+"\"\n") || !strings.Contains(tcontent, "Before=docker.service\n") || !strings.Contains(tcontent, "After=network-online.target\n") {
		t.Errorf("tables unit %s: %q %v", tname, raw, err)
	}
	if !host.did("systemctl enable "+name+" "+tname) || !slices.Equal(started, []string{filepath.Join(in.O.Dir, "notes-stow"), "--dir", in.O.Dir, "--no-input", "--no-provision"}) {
		t.Errorf("ran %v, started %v", host.ran, started)
	}
	if !in.gatewayConsented() || !host.tables["vpn"] || host.njumps() != 1 || !fileExists(in.tunFile("vpn0")) {
		t.Fatalf("consented %v, tables %v, jumps %v, ran %v", in.gatewayConsented(), host.tables, host.jumps, host.ran)
	}
	// A revert with networkd stopped, and a chain that does not go: the rest goes, the consent is
	// revoked, and a revert again removes the chain and the record.
	host.noNetworkd, host.xErr = true, errors.New("in use")
	if err := in.gatewayRevert(ctx); err == nil || !strings.Contains(err.Error(), "the consent is withdrawn, but not all it applied is removed (`notes-stow tune --revert` again removes the rest)") || strings.Contains(err.Error(), "networkctl") {
		t.Fatalf("revert: %v", err)
	}
	if in.gatewayConsented() || !in.gatewayRecorded() || host.tables["vpn"] || host.njumps() != 0 || fileExists(filepath.Join(in.O.Systemd, name)) || fileExists(filepath.Join(in.O.Systemd, tname)) || fileExists(in.tunFile("vpn0")) {
		t.Errorf("left: consented %v, tables %v, jumps %d", in.gatewayConsented(), host.tables, host.njumps())
	}
	if st := in.gatewayStatus(none); !strings.HasPrefix(st, "revoked, but not all it applied is removed") {
		t.Errorf("status %q", st)
	}
	if !host.did("systemctl disable "+name) || !host.did("systemctl disable "+tname) {
		t.Errorf("ran %v", host.ran)
	}
	// Revoked: a start as root applies nothing, and says what is left.
	host.ran = nil
	var log []string
	in.O.Log = writerFunc(func(p []byte) { log = append(log, string(p)) })
	if err := in.loadTables(ctx, none); err != nil || in.gatewayStart(ctx, none) != nil || host.did("nft") || host.did("iptables-restore") || !strings.Contains(strings.Join(log, ""), "the consent to the stack's network declarations is withdrawn, but not all it applied is removed") {
		t.Errorf("a revoked consent applied: %v %v %q", err, host.ran, log)
	}
	host.xErr = nil
	if err := in.gatewayRevert(ctx); err != nil || len(host.chain) > 0 || in.gatewayRecorded() {
		t.Errorf("again: %v, chain %v, recorded %v", err, host.chain, in.gatewayRecorded())
	}
	// A table nft does not delete fails the revert.
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
		t.Fatal(err)
	}
	if err := in.loadTables(ctx, none); err != nil {
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
	if err := in.gatewayApply(ctx); err == nil || in.gatewayRecorded() || fileExists(filepath.Join(in.O.Systemd, name)) || fileExists(filepath.Join(in.O.Systemd, tname)) {
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
	if fileExists(in.tunFile("vpn0")) || !host.did("networkctl reload") || !host.did("iptables -w -X STOWAGE-FWD-"+dirDigest(in.O.Dir)) || fileExists(in.forwardRecord()) {
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
	if _, content := rel.gatewayUnit(); !strings.Contains(content, "ExecStart=\""+filepath.Join(cwd, "relative-dir", "notes-stow")+"\" --dir \""+filepath.Join(cwd, "relative-dir")+"\" ") {
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
	if err := in.loadTables(ctx, none); err != nil {
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
	if st := in.gatewayStatus(none); !strings.HasPrefix(st, "consented (tune --apply), nothing in force: ") || !strings.Contains(st, "no forwarding through uplink (no NIC named: --interface)") {
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
		if err := in.loadTables(ctx, none); err != nil {
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

// At boot, before Docker, the tables the last start loaded are loaded again from their files, each
// with a load token of its own (the boot's start recreates its service, which feeds its sets); a
// Docker host's without the consent loads none; one that does not load is said, the others load.
func TestTheBootLoadsTheTablesBeforeDocker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, host, log := gateway(t, 0)
	in.R.Nftables["probe"] = []byte("table inet probe {\n}\n")
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
		t.Fatal(err)
	}
	if err := in.loadTables(ctx, none); err != nil {
		t.Fatal(err)
	}
	before := in.tableLoads()
	host.tables, host.ran = map[string]bool{}, nil // a reboot: the kernel keeps no table
	// The consent's record gone: the tables recorded load none.
	rec, err := os.ReadFile(in.path(GatewayFile))
	if err != nil || os.Remove(in.path(GatewayFile)) != nil {
		t.Fatal(err)
	}
	if err := in.bootTables(ctx); err != nil || host.did("nft") {
		t.Fatalf("no consent: %v, ran %v", err, host.ran)
	}
	if err := os.WriteFile(in.path(GatewayFile), rec, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := in.bootTables(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"probe", "vpn"} {
		if !host.tables[name] || !host.did("nft -f "+filepath.Join(in.O.Dir, TablesDir, name+".nft")) {
			t.Errorf("%s not loaded: %v", name, host.ran)
		}
		if after := in.tableLoads(); after[name] == before[name] || after[name] == "" {
			t.Errorf("%s's token %q, before %q", name, after[name], before[name])
		}
	}
	if !strings.Contains(strings.Join(*log, ""), "loaded the nftables table inet vpn, before the containers start") {
		t.Errorf("log %q", *log)
	}
	// The boot's start finds them loaded: it loads none again.
	host.ran = nil
	if err := in.loadTables(ctx, none); err != nil || host.did("nft -f") {
		t.Errorf("the start loaded again: %v %v", err, host.ran)
	}
	// One whose file nft refuses: the other loads, the error names it.
	if err := os.WriteFile(filepath.Join(in.O.Dir, TablesDir, "probe.nft"), []byte("refused\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	host.tables = map[string]bool{}
	in.O.Host = refusing{host, filepath.Join(in.O.Dir, TablesDir, "probe.nft")}
	if err := in.bootTables(ctx); err == nil || !strings.Contains(err.Error(), "services.probe.nftables: its table does not load at boot (the install's start loads it again)") || !host.tables["vpn"] {
		t.Errorf("a table refused: %v, tables %v", err, host.tables)
	}
	// The VM's firewall is Stowage's: no consent to look for.
	in.O.Host, in.O.Firewall = host, t.TempDir()
	if err := os.Remove(in.path(GatewayFile)); err != nil {
		t.Fatal(err)
	}
	host.tables = map[string]bool{}
	if err := in.bootTables(ctx); err != nil || !host.tables["vpn"] {
		t.Errorf("the VM: %v, tables %v", err, host.tables)
	}
}

// refusing is a host whose nft refuses to load one file.
type refusing struct {
	*gwHost
	file string
}

func (r refusing) Run(ctx context.Context, c Cmd) ([]byte, error) {
	if len(c.Args) == 3 && c.Args[0] == "nft" && c.Args[1] == "-f" && c.Args[2] == r.file {
		return nil, errors.New("syntax error")
	}
	return r.gwHost.Run(ctx, c)
}

// A consent an earlier release recorded, with the start's unit alone, gets the tables' unit at its
// next start as root; a start that finds both as they are writes and enables nothing.
func TestAStartWritesTheBootUnitsOfAnEarlierConsent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, host, _ := gateway(t, 0)
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
		t.Fatal(err)
	}
	name, content := in.gatewayUnit()
	if err := os.WriteFile(filepath.Join(in.O.Systemd, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := in.gatewayStart(ctx, none); err != nil {
		t.Fatal(err)
	}
	tname, tcontent := in.gatewayTablesUnit()
	if raw, err := os.ReadFile(filepath.Join(in.O.Systemd, tname)); err != nil || string(raw) != tcontent || !host.did("systemctl daemon-reload") || !host.did("systemctl enable "+name+" "+tname) {
		t.Errorf("the tables' unit: %q %v, ran %v", raw, err, host.ran)
	}
	host.ran = nil
	if err := in.gatewayStart(ctx, none); err != nil || host.did("systemctl") {
		t.Errorf("again: %v, ran %v", err, host.ran)
	}
}

// A unit's argument stays one argument, whatever the path: quoted, its quotes, backslashes and
// systemd's % escaped.
func TestSystemdArg(t *testing.T) {
	t.Parallel()
	if got := systemdText("/srv/a%b\nX=1"); got != "/srv/a%%b?X=1" {
		t.Errorf("text %q", got)
	}
	for in, want := range map[string]string{
		"/srv/caf\xe9":                 `"/srv/caf\xe9"`,
		"/srv/café":                    `"/srv/café"`,
		"/srv/notes":                   `"/srv/notes"`,
		"/srv/my stack":                `"/srv/my stack"`,
		"/opt/app$1":                   `"/opt/app$$1"`,
		`/srv/a"b\c%d`:                 `"/srv/a\"b\\c%%d"`,
		"/srv/x\nExecStartPre=/bin/sh": `"/srv/x\x0aExecStartPre=/bin/sh"`,
	} {
		if got := systemdArg(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}

// On a consented Docker host, a set fed through the VM's app socket (its service, or a feeder of
// its table, given the socket) and filled by no input is warned: nothing feeds it there; a set an
// input fills, and a table no socket holder feeds, are not.
func TestADockerHostWarnsOfASetOnlyTheAppSocketFeeds(t *testing.T) {
	t.Parallel()
	in, _, log := gateway(t, 0)
	in.R.Nftables["vpn"] = []byte("table inet vpn {\n\tset peers {\n\t\ttype ipv4_addr\n\t}\n\tset allowed {\n\t\ttype ipv4_addr\n\t}\n}\n")
	in.R.Nftables["probe"] = []byte("table inet probe {\n\tset seen {\n\t\ttype ipv4_addr\n\t}\n}\n")
	in.R.Spec.Services = map[string]spec.Service{"vpn": {Sets: map[string]string{"allowed": "ALLOWED"}}, "api": {Feeds: []string{"vpn"}}, "probe": {}}
	in.R.Spec.Shapes.Kairos = &spec.KairosShape{HostAPI: []string{"api"}}
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
		t.Fatal(err)
	}
	// A start prepares twice after a pull: warned once.
	for range 2 {
		in.warnSocketFed()
	}
	got := strings.Join(*log, "")
	if strings.Count(got, "set peers") != 1 || !strings.Contains(got, "warning: the set peers of the table inet vpn is fed through the VM's app socket, which a Docker host does not serve: it stays as the table declares it here unless its own rules fill it (services.vpn.sets fills a set from an input)") ||
		strings.Contains(got, "set allowed") || strings.Contains(got, "inet probe") {
		t.Errorf("log %q", got)
	}
}

// A consented Docker host enforces the L4 routes' allowlists, IPv4's alone, in a chain of the
// install's that DOCKER-USER jumps to, replaced at each start: each route's published port from its
// sources, the rest dropped; a list that does not read, or an engine that sends nothing through
// DOCKER-USER, fails the start with every source dropped; a revert removes the chain.
func TestAConsentedDockerHostEnforcesTheL4Allowlists(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, host, log := gateway(t, 0)
	in.R.Nftables, in.R.Spec.Host = nil, nil
	in.R.Spec.L4.Routes = []spec.L4Route{
		{Name: "db", Protocol: "tcp", Port: 5432, Upstream: "postgres:5432", AllowInput: "DB_SOURCES"},
		{Name: "feed", Protocol: "udp", Port: 17007, Upstream: "api:17007"},
	}
	values := map[string]string{"DB_SOURCES": "10.0.0.0/8, 192.0.2.1 fd00::/8"}
	get := func(k string) string { return values[k] }
	if err := in.gatewayStart(ctx, get); err != nil || len(host.ran) != 0 || !strings.Contains(strings.Join(*log, ""), "the source allowlists of the L4 routes db are not in force on this host") {
		t.Fatalf("not consented: %v, ran %v, log %q", err, host.ran, *log)
	}
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
		t.Fatal(err)
	}
	if err := in.gatewayStart(ctx, get); err != nil {
		t.Fatal(err)
	}
	if in.allowHolds(get) {
		t.Error("held before the stack is up")
	}
	if err := in.markAllowHeld(); err != nil {
		t.Fatal(err)
	}
	chain := "STOWAGE-ALW-" + dirDigest(in.O.Dir)
	match := "-A " + chain + " -p tcp -m conntrack --ctstate DNAT --ctdir ORIGINAL --ctorigdstport 5432"
	rules := func(lines ...string) string {
		return "*filter\n:" + chain + " - [0:0]\n" + strings.Join(lines, "\n") + "\nCOMMIT\n"
	}
	v4 := rules(match+" -s 10.0.0.0/8 -j RETURN", match+" -s 192.0.2.1/32 -j RETURN", match+" -j DROP")
	if !slices.Equal(host.stdin, []string{v4}) || !host.did("iptables -w -I DOCKER-USER -j "+chain) || host.did("ip6tables -w -I DOCKER-USER -j "+chain) || !host.did("iptables -w -C FORWARD -j DOCKER-USER") {
		t.Errorf("rules %q, ran %v", host.stdin, host.ran)
	}
	if st := in.gatewayStatus(get); st != "applied at each start (tune --apply): source allowlists of db" {
		t.Errorf("status %q", st)
	}
	// Values changed by no start as root (one not as root applies nothing): no longer held.
	if changed := func(k string) string { return map[string]string{"DB_SOURCES": "192.0.2.0/24"}[k] }; in.allowHolds(changed) || !strings.Contains(in.gatewayStatus(changed), "source allowlists of db not held") {
		t.Errorf("held for other values: %q", in.gatewayStatus(changed))
	}
	// Replaced at each start: a chain flushed meanwhile holds again.
	host.stdin = nil
	if err := in.gatewayStart(ctx, get); err != nil || !slices.Equal(host.stdin, []string{v4}) {
		t.Errorf("again: %v, rules %q", err, host.stdin)
	}
	values["DB_SOURCES"], host.stdin = "", nil
	if err := in.gatewayStart(ctx, get); err != nil || !slices.Equal(host.stdin, []string{rules(match + " -j DROP")}) {
		t.Errorf("an empty list refuses every source: %v, rules %q", err, host.stdin)
	}
	// A recorded list that does not read: every source dropped, the start failed, no record.
	values["DB_SOURCES"], host.stdin = "10.0.0.0/8 -j ACCEPT", nil
	if err := in.gatewayStart(ctx, get); err == nil || !strings.Contains(err.Error(), "l4 route db: DB_SOURCES") || !slices.Equal(host.stdin, []string{rules(match + " -j DROP")}) || fileExists(in.allowHeld()) || !strings.Contains(in.gatewayStatus(get), "source allowlists of db not held") {
		t.Errorf("a list that does not read: %v, rules %q", err, host.stdin)
	}
	// An engine that sends nothing through DOCKER-USER holds no list: refused.
	values["DB_SOURCES"], host.noForward = "10.0.0.0/8", true
	if err := in.gatewayStart(ctx, get); err == nil || !strings.Contains(err.Error(), "iptables' FORWARD does not jump to DOCKER-USER") || fileExists(in.allowHeld()) {
		t.Errorf("no FORWARD jump: %v", err)
	}
	host.noForward = false
	// A restore that fails keeps the record, not the applied mark: a release without lists then
	// removes the chain.
	host.restoreErr = errors.New("xtables lock")
	if err := in.gatewayStart(ctx, get); err == nil || !fileExists(in.allowRecord()) || fileExists(appliedMark(in.allowRecord())) {
		t.Errorf("a failed restore: %v", err)
	}
	host.restoreErr, host.ran = nil, nil
	in.R.Spec.L4.Routes = in.R.Spec.L4.Routes[1:]
	if err := in.gatewayStart(ctx, get); err != nil || !host.did("iptables -w -X "+chain) || fileExists(in.allowRecord()) {
		t.Errorf("a release without lists: %v, ran %v", err, host.ran)
	}
	in.R.Spec.L4.Routes = []spec.L4Route{{Name: "db", Protocol: "tcp", Port: 5432, Upstream: "postgres:5432", AllowInput: "DB_SOURCES"}}
	if err := in.gatewayStart(ctx, get); err != nil {
		t.Fatal(err)
	}
	if err := in.dropForward(ctx); err != nil || !host.did("iptables -w -X "+chain) || fileExists(in.allowRecord()) {
		t.Errorf("revert: %v, ran %v", err, host.ran)
	}
}

// At boot, before Docker restores the containers, the tables' unit applies the allowlists from
// the install's recorded values, into a DOCKER-USER it makes when Docker has not yet: no route is
// open a moment past its list. Without the consent, nothing; a record that does not read drops
// every source.
func TestTheBootAppliesTheL4AllowlistsBeforeDocker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, host, log := gateway(t, 0)
	in.R.Nftables, in.R.Spec.Host = nil, nil
	in.R.Spec.L4.Routes = []spec.L4Route{{Name: "db", Protocol: "tcp", Port: 5432, Upstream: "postgres:5432", AllowInput: "DB_SOURCES", BindInput: "DB_BIND"}}
	env := NewEnvFile()
	env.Set("DB_SOURCES", "10.0.0.0/8")
	env.Set("DB_BIND", "192.0.2.7")
	if err := env.WriteFile(in.path(envName)); err != nil {
		t.Fatal(err)
	}
	if err := in.bootTables(ctx); err != nil || len(host.ran) != 0 {
		t.Fatalf("not consented: %v, ran %v", err, host.ran)
	}
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
		t.Fatal(err)
	}
	host.noUser, host.noForward = true, true // before Docker: no DOCKER-USER, no FORWARD jump yet
	if err := in.bootTables(ctx); err != nil {
		t.Fatal(err)
	}
	chain := "STOWAGE-ALW-" + dirDigest(in.O.Dir)
	if !host.did("iptables -w -N DOCKER-USER") || !host.did("iptables -w -I DOCKER-USER -j "+chain) || len(host.stdin) != 1 || !strings.Contains(host.stdin[0], "--ctorigdst 192.0.2.7 -s 10.0.0.0/8 -j RETURN") || !fileExists(in.allowRecord()) || fileExists(in.allowHeld()) || host.did("iptables -w -C FORWARD") {
		t.Errorf("ran %v, rules %q", host.ran, host.stdin)
	}
	if !strings.Contains(strings.Join(*log, ""), "applied the L4 routes' source allowlists, before the containers start") {
		t.Errorf("log %q", *log)
	}
	if _, content := in.gatewayTablesUnit(); !strings.Contains(content, "Before=docker.service") {
		t.Errorf("unit %s", content)
	}
	// A record that does not read: every source of the port dropped, on its bind address too
	// when the record gives none, said.
	if err := os.Remove(in.path(envName)); err != nil {
		t.Fatal(err)
	}
	host.stdin = nil
	if err := in.bootTables(ctx); err == nil || !strings.Contains(err.Error(), "the L4 routes' source allowlists at boot") || len(host.stdin) != 1 || strings.Contains(host.stdin[0], "RETURN") || fileExists(in.allowHeld()) {
		t.Errorf("unread: %v, rules %q", err, host.stdin)
	}
}

// A route bound to one address matches that address alone (another install's port on another
// address is not its own), and keeps it when its list does not read; one bound to an IPv6
// address is refused: a Docker host holds the list for IPv4 alone.
func TestABoundAllowlistMatchesItsAddress(t *testing.T) {
	t.Parallel()
	routes := []spec.L4Route{{Name: "db", Protocol: "udp", Port: 5353, Upstream: "dns:53", AllowInput: "SRC", BindInput: "BIND"}}
	vals := map[string]string{"SRC": "10.0.0.0/8, fd00::/8", "BIND": "192.0.2.5"}
	get := func(k string) string { return vals[k] }
	rules, err := allowRules(routes, "C", get)
	if err != nil || len(rules) != 1 || !strings.Contains(string(rules["iptables"]), "--ctorigdstport 5353 --ctorigdst 192.0.2.5 -s 10.0.0.0/8 -j RETURN\n") || strings.Contains(string(rules["iptables"]), "fd00") {
		t.Errorf("%v %q", err, rules)
	}
	in, host, _ := gateway(t, 0)
	in.R.Spec.L4.Routes = routes
	vals["SRC"] = "x"
	if err := in.allowSources(context.Background(), get, false); err == nil || len(host.stdin) != 1 || !strings.Contains(host.stdin[0], "--ctorigdstport 5353 -j DROP\n") {
		t.Errorf("closed on every address: %v %q", err, host.stdin)
	}
	vals["SRC"], vals["BIND"] = "10.0.0.0/8", "2001:db8::5"
	host.stdin = nil
	if err := in.allowSources(context.Background(), get, false); err == nil || !strings.Contains(err.Error(), "l4 route db: BIND=2001:db8::5 is an IPv6 address, and a Docker host holds SRC for IPv4 alone") ||
		len(host.stdin) != 1 || strings.Contains(host.stdin[0], "2001:db8") || !strings.Contains(host.stdin[0], "--ctorigdstport 5353 -j DROP\n") {
		t.Errorf("IPv6 bind, closed on every address: %v %q", err, host.stdin)
	}
	// A route to a service on the host's network is no list's: Docker translates no port to it.
	in.R.Spec.Services = map[string]spec.Service{"dns": {HostNetwork: true}}
	if got := in.allowRoutes(); got != nil {
		t.Errorf("a host-network upstream's: %v", got)
	}
}

// A consented Docker host publishes an allowlisted route on IPv4 alone, whatever the stack's
// IPv6: Docker's userland proxy may carry its IPv6 clients past DOCKER-USER. Not consented, or a
// bind address given, as before.
func TestAnAllowlistedRouteIsIPv4s(t *testing.T) {
	t.Parallel()
	in, _, _ := gateway(t, 0)
	routes := []spec.L4Route{{Name: "db", Protocol: "tcp", Port: 5432, Upstream: "postgres:5432", AllowInput: "DB_SOURCES"}, {Name: "feed", Protocol: "udp", Port: 17007, Upstream: "api:17007"}}
	in.R.Spec.L4.Routes = routes
	if got := in.allowV4Only(); got != nil {
		t.Errorf("not consented: %v", got)
	}
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
		t.Fatal(err)
	}
	if got := in.allowV4Only(); !slices.Equal(got, []string{"db"}) {
		t.Errorf("consented: %v", got)
	}
	fc, err := frontCompose(in.R.Spec.Front, routes, func(string) string { return "" }, Loopback, hostAPI{}, "sha256:x", "", false, true, nil, []string{"db"})
	if err != nil || !strings.Contains(string(fc), "host_ip: 0.0.0.0\n") || strings.Count(string(fc), "host_ip") != 1 {
		t.Errorf("%v\n%s", err, fc)
	}
}

// A revert of a consent whose allowlisted routes it published on IPv4 alone says the next start
// publishes them on every address; it never starts the stack itself.
func TestARevertNamesTheStartThatPublishesEverywhere(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, allow := range []string{"DB_SOURCES", ""} {
		in, _, log := gateway(t, 0)
		in.R.Nftables, in.R.Spec.Host = nil, nil
		in.R.Spec.L4.Routes = []spec.L4Route{{Name: "db", Protocol: "tcp", Port: 5432, Upstream: "postgres:5432", AllowInput: allow}}
		if err := NewEnvFile().WriteFile(in.path(envName)); err != nil {
			t.Fatal(err)
		}
		if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
			t.Fatal(err)
		}
		in.O.Exec = func(context.Context, string, []string) error { t.Error("a revert started the stack"); return nil }
		err := in.lockedRevert(ctx)
		noted := strings.Contains(strings.Join(*log, ""), "note: the L4 routes db are open to every source now, and stay published on IPv4 alone until the next start, which publishes them on every address: `"+in.ctl()+" --dir "+in.gatewayDir()+" --no-input --no-provision`")
		if err != nil || in.gatewayRecorded() || (allow != "") != noted {
			t.Errorf("allow %q: %v, log %q", allow, err, *log)
		}
	}
}

// status says the forwarding is applied only while its applied mark is the rules the release and
// the devices give now, and not applied while a chain is recorded without it.
func TestStatusSaysWhetherTheForwardingIsApplied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, _, _ := gateway(t, 0)
	in.R.Nftables = nil
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
		t.Fatal(err)
	}
	if err := in.gatewayStart(ctx, none); err != nil {
		t.Fatal(err)
	}
	if st := in.gatewayStatus(none); !strings.Contains(st, "forwarding;") || strings.Contains(st, "not applied") {
		t.Errorf("applied: %q", st)
	}
	in.O.Interfaces = map[string]string{"uplink": "eth1"} // another NIC named, no start as root since
	if st := in.gatewayStatus(none); !strings.Contains(st, "forwarding not applied (no start as root applied it since this release or these devices") {
		t.Errorf("another NIC: %q", st)
	}
}

// A consented Docker host refuses an IPv6 bind address for an allowlisted route as it is given,
// and refuses to consent while one is recorded: the list holds for IPv4 alone.
func TestAnIPv6BindOfAnAllowlistedRouteIsRefusedEarly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, _, _ := gateway(t, 0)
	in.R.Nftables, in.R.Spec.Host = nil, nil
	in.R.Spec.Inputs = append(in.R.Spec.Inputs, spec.Input{Key: "SRC"}, spec.Input{Key: "BIND"})
	in.R.Spec.L4.Routes = []spec.L4Route{{Name: "db", Protocol: "tcp", Port: 5432, Upstream: "postgres:5432", AllowInput: "SRC", BindInput: "BIND"}}
	env := NewEnvFile()
	env.Set("BIND", "2001:db8::5")
	if err := env.WriteFile(in.path(envName)); err != nil {
		t.Fatal(err)
	}
	if err := in.consent(ctx); err == nil || !strings.Contains(err.Error(), "BIND=2001:db8::5 is an IPv6 address") || in.gatewayRecorded() {
		t.Errorf("consent: %v", err)
	}
	// A record that does not read could hide one: refused too.
	raw, _ := os.ReadFile(in.path(envName))
	if err := os.WriteFile(in.path(envName), []byte("not a record\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := in.consent(ctx); err == nil || !strings.Contains(err.Error(), "the install's record") || in.gatewayRecorded() {
		t.Errorf("consent on a record that does not read: %v", err)
	}
	if err := os.WriteFile(in.path(envName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	check := func() func(string) error {
		for _, i := range in.inputList(Loopback) {
			if i.Key == "BIND" {
				return i.check
			}
		}
		return nil
	}
	if c := check(); c != nil && c("2001:db8::5") != nil {
		t.Error("refused before the consent")
	}
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
		t.Fatal(err)
	}
	if err := check()("2001:db8::5"); err == nil || !strings.Contains(err.Error(), "holds the route's allowlist for IPv4 alone") {
		t.Errorf("given after the consent: %v", err)
	}
	for _, v := range []string{"192.0.2.5", "::"} { // :: is every address
		if err := check()(v); err != nil {
			t.Errorf("%s: %v", v, err)
		}
	}
}

// DOCKER-USER is made at boot alone, before Docker: a start after it finding none is an engine
// that keeps none, whose chain nothing would reach: an error, nothing said to be applied.
func TestAStartWithoutDockerUserFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, host, _ := gateway(t, 0)
	in.R.Nftables = nil
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
		t.Fatal(err)
	}
	host.noUser = true
	if err := in.gatewayStart(ctx, none); err == nil || !strings.Contains(err.Error(), "the forwarding between the host's interfaces (iptables's DOCKER-USER)") || host.did("iptables -w -N") || in.forwardApplied(none) {
		t.Errorf("%v, ran %v", err, host.ran)
	}
}

// An engine that sends nothing through DOCKER-USER (its FORWARD does not jump to it, as with a
// DOCKER-USER a boot made) holds neither chain: each says so, naming the fix, and nothing is marked
// applied; with nothing in force, status says so.
func TestAnEngineWithoutDockerUserHoldsNoChain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, host, _ := gateway(t, 0)
	in.R.Nftables = nil
	in.R.Spec.Host.Interfaces = map[string]spec.HostInterface{"uplink": {Primary: true}, "lan": {Forward: []string{"uplink"}}}
	in.O.Interfaces = map[string]string{"uplink": "eth0", "lan": "eth1"}
	in.R.Spec.L4.Routes = []spec.L4Route{{Name: "db", Protocol: "tcp", Port: 5432, Upstream: "postgres:5432", AllowInput: "SRC"}}
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
		t.Fatal(err)
	}
	host.noForward = true
	err := in.gatewayStart(ctx, none)
	for _, want := range []string{"the forwarding between the host's interfaces cannot hold: iptables' FORWARD does not jump to DOCKER-USER (Bad rule: no such jump)", "the L4 routes' source allowlists cannot hold", "`notes-stow tune --revert`"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("no %q in %v", want, err)
		}
	}
	if in.forwardApplied(none) || in.allowHolds(none) || len(host.stdin) != 0 {
		t.Errorf("applied: rules %q", host.stdin)
	}
	if st := in.gatewayStatus(none); !strings.HasPrefix(st, "consented (tune --apply), nothing in force: forwarding not applied") || !strings.Contains(st, "source allowlists of db not held") {
		t.Errorf("status %q", st)
	}
}

// A start's marks say what holds now: a forwarding marked applied by an earlier start is no longer
// once the engine sends nothing through DOCKER-USER.
func TestAnEarlierForwardingMarkGoesWithTheEngine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, host, _ := gateway(t, 0)
	in.R.Nftables = nil
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
		t.Fatal(err)
	}
	if err := in.gatewayStart(ctx, none); err != nil || !in.forwardApplied(none) {
		t.Fatalf("%v, applied %v", err, in.forwardApplied(none))
	}
	host.noForward = true
	if err := in.gatewayStart(ctx, none); err == nil || in.forwardApplied(none) {
		t.Errorf("%v, applied %v", err, in.forwardApplied(none))
	}
}

// A consented start as root, whole: the route published on IPv4 alone in the front's file, its
// list marked held once the stack is up, and said so at the end.
func TestAConsentedStartPublishesAndHoldsTheList(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.r.Spec.Inputs = append(e.r.Spec.Inputs, spec.Input{Key: "SRC", Default: "10.0.0.0/8"})
	e.r.Spec.L4.Routes = []spec.L4Route{{Name: "data", Protocol: "tcp", Port: 7443, Upstream: "web:7443", AllowInput: "SRC"}}
	host := &gwHost{tables: map[string]bool{}, jumps: map[string]int{}, chain: map[string]bool{}}
	run := t.TempDir()
	opts := func(o *Options) {
		o.Host, o.GatewayRun, o.Euid, o.Systemd = host, run, func() int { return 0 }, t.TempDir()
	}
	if err := e.install(opts); err != nil {
		t.Fatal(err)
	}
	in := e.installer(opts)
	if fc, _ := os.ReadFile(in.path(FrontCompose)); strings.Contains(string(fc), "host_ip") {
		t.Errorf("not consented, on IPv4 alone:\n%s", fc)
	}
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir()}); err != nil {
		t.Fatal(err)
	}
	// A stack that does not come up holds no list: its old publication may still run.
	e.docker.upErr = errors.New("unhealthy")
	if err := e.install(opts); err == nil || fileExists(in.allowHeld()) || !strings.Contains(in.gatewayStatus(func(k string) string { return map[string]string{"SRC": "10.0.0.0/8"}[k] }), "source allowlists of data not held") {
		t.Errorf("up failed: %v, held %v", err, fileExists(in.allowHeld()))
	}
	e.docker.upErr = nil
	e.log.Reset()
	if err := e.install(opts); err != nil {
		t.Fatal(err)
	}
	if fc, _ := os.ReadFile(in.path(FrontCompose)); !strings.Contains(string(fc), "host_ip: 0.0.0.0") {
		t.Errorf("consented, not on IPv4 alone:\n%s", fc)
	}
	if !fileExists(in.allowHeld()) || !strings.Contains(e.log.String(), "L4 route data: tcp/7443 on IPv4's every address, from the IPv4 sources SRC holds") {
		t.Errorf("held %v, log:\n%s", fileExists(in.allowHeld()), e.log.String())
	}
}

// Networkd files not yet seen loaded are not in force: alone, status says nothing is.
func TestPendingNetworkdFilesAreNotInForce(t *testing.T) {
	t.Parallel()
	in, _, _ := gateway(t, 0)
	in.R.Nftables = nil
	in.R.Spec.Host.Interfaces = map[string]spec.HostInterface{"vpn0": {Kind: spec.InterfaceTUN}}
	file := in.tunFile("vpn0")
	if err := in.writeGateway(gatewayRecord{Digest: dirDigest(in.gatewayDir()), Dir: in.gatewayDir(), Networkd: map[string]networkdFile{file: {Role: "vpn0", Pending: true}}}); err != nil {
		t.Fatal(err)
	}
	if st := in.gatewayStatus(none); !strings.HasPrefix(st, "consented (tune --apply), nothing in force: 1 networkd file(s) pending") {
		t.Errorf("status %q", st)
	}
}
