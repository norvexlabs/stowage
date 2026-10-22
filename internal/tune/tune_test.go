package tune

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func host(memMiB uint64, cpus int) Host {
	return Host{MemBytes: memMiB * mib, CPUs: cpus, PageSize: 4096}
}

func TestRulesScaleWithRAM(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		mem                             uint64
		cpus                            int
		buf, backlog, conn, free, flows uint64
		tunq                            uint64
	}{
		// RAM/256 as a power of two: 8 MiB to 4 GiB, 64 MiB from 16 GiB.
		{mem: 1024, cpus: 1, buf: 8 * mib, backlog: 5242, conn: 67108, free: 5242, flows: 8192, tunq: 5242},
		{mem: 1792, cpus: 2, buf: 8 * mib, backlog: 4587, conn: 117440, free: 9175, flows: 8192, tunq: 9175},
		{mem: 2048, cpus: 2, buf: 8 * mib, backlog: 5242, conn: 134217, free: 10485, flows: 16384, tunq: 10000},
		{mem: 16384, cpus: 4, buf: 64 * mib, backlog: 20971, conn: 1073741, free: 83886, flows: 65536, tunq: 10000},
		{mem: 32768, cpus: 4, buf: 64 * mib, backlog: 32768, conn: 2147483, free: 131072, flows: 65536, tunq: 10000},
		// The floors: a backlog of 1000, a TUN queue of 500, 4096 flows.
		{mem: 256, cpus: 64, buf: 8 * mib, backlog: 1000, conn: 16777, free: 1310, flows: 4096, tunq: 1310},
		{mem: 64, cpus: 1, buf: 8 * mib, backlog: 1000, conn: 4194, free: 327, flows: 4096, tunq: 500},
	} {
		h := host(c.mem, c.cpus)
		got := []uint64{h.SocketBuffer(), h.Backlog(), h.ConntrackMax(), h.MinFreeKB(), h.FlowEntries(), h.TunQueueLen()}
		want := []uint64{c.buf, c.backlog, c.conn, c.free, c.flows, c.tunq}
		if !slices.Equal(got, want) {
			t.Errorf("%d MiB, %d CPUs: buf, backlog, conntrack, min_free, flows, tun queue = %v, want %v", c.mem, c.cpus, got, want)
		}
	}
}

func TestSocketBufferMovesOnlyWhenRAMDoubles(t *testing.T) {
	t.Parallel()
	for memMiB, want := range map[uint64]uint64{64: 8, 2048: 8, 4095: 8, 4096: 16, 8191: 16, 8192: 32, 16384: 64, 1 << 20: 64} {
		if got := host(memMiB, 1).SocketBuffer(); got != want*mib {
			t.Errorf("%d MiB: %d MiB, want %d", memMiB, got/mib, want)
		}
	}
}

func TestBudgetsArePercentagesOfRAM(t *testing.T) {
	t.Parallel()
	h := host(8192, 2) // 2097152 pages
	for _, c := range h.Sysctls() {
		if c.Key == "net.ipv4.tcp_mem" || c.Key == "net.ipv4.udp_mem" {
			if c.Want != "83886 125829 167772" || c.Rule != Exact {
				t.Errorf("%s = %q (%v), want 4/6/8 %% of 2097152 pages, exact", c.Key, c.Want, c.Rule)
			}
		}
	}
}

func TestSatisfied(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		s   Sysctl
		cur string
		ok  bool
	}{
		{Sysctl{Want: "100", Rule: AtLeast}, "100", true},
		{Sysctl{Want: "100", Rule: AtLeast}, "99", false},
		{Sysctl{Want: "100", Rule: AtLeast}, "101", true},
		{Sysctl{Want: "4096 131072 8388608", Rule: AtLeast}, "4096\t131072\t8388608", true},
		{Sysctl{Want: "4096 131072 8388608", Rule: AtLeast}, "4096 131072 6291456", false},
		{Sysctl{Want: "4096 131072 8388608", Rule: AtLeast}, "4096 131072", false},
		{Sysctl{Want: "131072", Rule: AtMost}, "131072", true},
		{Sysctl{Want: "131072", Rule: AtMost}, "131073", false},
		{Sysctl{Want: "131072", Rule: AtMost}, "4294967295", false},
		{Sysctl{Want: "0", Rule: Exact}, "1", false},
		{Sysctl{Want: "fq", Rule: Exact}, "fq", true},
		{Sysctl{Want: "fq", Rule: Exact}, "fq_codel", false},
		{Sysctl{Want: "2", Rule: NotStrict}, "0", true},
		{Sysctl{Want: "2", Rule: NotStrict}, "2", true},
		{Sysctl{Want: "2", Rule: NotStrict}, "1", false},
	} {
		if got := c.s.Satisfied(c.cur); got != c.ok {
			t.Errorf("%v %q against %q = %v, want %v", c.s.Rule, c.s.Want, c.cur, got, c.ok)
		}
	}
}

func TestValueKeepsWhatTheHostHoldsBeyond(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		s        Sysctl
		cur, out string
	}{
		{Sysctl{Want: "100", Rule: AtLeast}, "99", "100"},
		{Sysctl{Want: "100", Rule: AtLeast}, "101", "101"},
		{Sysctl{Want: "4096 131072 8388608", Rule: AtLeast}, "4096 87380 33554432", "4096 131072 33554432"},
		{Sysctl{Want: "131072", Rule: AtMost}, "4294967295", "131072"},
		{Sysctl{Want: "131072", Rule: AtMost}, "16384", "16384"},
		{Sysctl{Want: "83886 125829 167772", Rule: Exact}, "1 2 3", "83886 125829 167772"},
		{Sysctl{Want: "2", Rule: NotStrict}, "0", "0"},
		{Sysctl{Want: "2", Rule: NotStrict}, "1", "2"},
		{Sysctl{Want: "100", Rule: AtLeast}, "1 2", "100"},
		{Sysctl{Want: "fq", Rule: Exact}, "fq_codel", "fq"},
	} {
		if got := c.s.Value(c.cur); got != c.out {
			t.Errorf("%v %q over %q = %q, want %q", c.s.Rule, c.s.Want, c.cur, got, c.out)
		}
	}
}

func TestCPUList(t *testing.T) {
	t.Parallel()
	for in, want := range map[string][]int{"0": {0}, "0-3": {0, 1, 2, 3}, "0-1,8,10-11": {0, 1, 8, 10, 11}} {
		if got, ok := cpuList(in); !ok || !slices.Equal(got, want) {
			t.Errorf("%q = %v %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "a", "3-1", "0-", "-1", "0-99999", "1,,2"} {
		if _, ok := cpuList(bad); ok {
			t.Errorf("%q reads", bad)
		}
	}
	if m := (Host{CPUs: 2, Online: []int{0, 33}}).RPSMask(); m != "00000002,00000001" {
		t.Errorf("CPUs 0 and 33: %s", m)
	}
}

func TestRPSMask(t *testing.T) {
	t.Parallel()
	for cpus, want := range map[int]string{
		1: "00000001", 4: "0000000f", 32: "ffffffff", 33: "00000001,ffffffff", 64: "ffffffff,ffffffff",
	} {
		if got := host(1024, cpus).RPSMask(); got != want {
			t.Errorf("%d CPUs: %s, want %s", cpus, got, want)
		}
	}
	if !sameMask("00000000,0000000f", "0000000f") || sameMask("", "0000000f") || sameMask("00000007", "0000000f") {
		t.Error("sameMask compares the masks' values")
	}
}

func TestContainerKeepsTheNamespacedSettings(t *testing.T) {
	t.Parallel()
	c := host(4096, 2).Container()
	if len(c) != len(containerKeys) {
		t.Fatalf("%d settings, want %d: %v", len(c), len(containerKeys), c)
	}
	if c["net.ipv4.tcp_rmem"] != "4096 131072 16777216" || c["net.core.somaxconn"] != "8192" {
		t.Errorf("container settings %v", c)
	}
	for k := range c {
		if strings.HasPrefix(k, "vm.") || strings.Contains(k, "netfilter") || strings.Contains(k, "rmem_max") {
			t.Errorf("%s is the host's, not a namespace's", k)
		}
	}
}

func TestReadHost(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, "proc/meminfo", "MemTotal:        8040000 kB\nMemFree:  100 kB\n")
	h, err := ReadHost(root)
	if err != nil || h.MemBytes != 122*MemStep || h.CPUs < 1 || h.PageSize < 1 {
		t.Fatalf("ReadHost = %+v, %v", h, err)
	}
	// A few KiB less is the same host; a step less is another.
	write(t, root, "proc/meminfo", "MemTotal: 7995392 kB\n")
	if h2, _ := ReadHost(root); h2.MemBytes != h.MemBytes {
		t.Errorf("7995392 kB = %d, want %d", h2.MemBytes, h.MemBytes)
	}
	write(t, root, "proc/meminfo", "MemTotal: 7995391 kB\n")
	if h2, _ := ReadHost(root); h2.MemBytes != 121*MemStep {
		t.Errorf("7995391 kB = %d, want 121 steps", h2.MemBytes)
	}
	write(t, root, "proc/meminfo", "MemTotal: 1000 kB\n")
	if h2, _ := ReadHost(root); h2.MemBytes != MemStep {
		t.Errorf("a tiny host = %d, want one step", h2.MemBytes)
	}
	write(t, root, "sys/devices/system/cpu/online", "4-7\n")
	if h2, _ := ReadHost(root); h2.CPUs != 4 || h2.RPSMask() != "000000f0" {
		t.Errorf("CPUs 4-7: %d CPUs, mask %s", h2.CPUs, h2.RPSMask())
	}
	write(t, root, "sys/devices/system/cpu/online", "x\n")
	if h2, _ := ReadHost(root); h2.Online != nil || h2.CPUs < 1 {
		t.Errorf("a list that does not read: %+v", h2)
	}
	write(t, root, "proc/meminfo", "MemFree:  100 kB\n")
	if _, err := ReadHost(root); err == nil {
		t.Error("a meminfo without MemTotal read")
	}
	write(t, root, "proc/meminfo", "MemTotal: 0 kB\n")
	if _, err := ReadHost(root); err == nil {
		t.Error("a MemTotal of 0 read")
	}
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// fakeHost is a root with a virtio NIC eth0 of mtu, one receive queue, a TUN device tun0, every
// setting at an untuned value, and the commands it ran.
func fakeHost(t *testing.T, mtu string) (System, *[]string) {
	t.Helper()
	root := t.TempDir()
	for _, c := range host(4096, 2).Sysctls() {
		write(t, root, c.Path(), "1\n")
	}
	write(t, root, Sysctl{Key: "net.ipv4.tcp_rmem"}.Path(), "4096\t131072\t6291456\n")
	write(t, root, Sysctl{Key: "net.core.default_qdisc"}.Path(), "fq_codel\n")
	write(t, root, "sys/class/net/eth0/mtu", mtu+"\n")
	write(t, root, "sys/class/net/eth0/queues/rx-0/rps_cpus", "00000000\n")
	if err := os.MkdirAll(filepath.Join(root, "drivers/virtio_net"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sys/class/net/eth0/device"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "drivers/virtio_net"), filepath.Join(root, "sys/class/net/eth0/device/driver")); err != nil {
		t.Fatal(err)
	}
	write(t, root, "sys/class/net/docker0/mtu", "1500\n")
	write(t, root, "sys/class/net/tun0/tun_flags", "0x1001\n")
	write(t, root, "sys/class/net/tun0/tx_queue_len", "500\n")
	write(t, root, "sys/class/net/tun0/queues/rx-0/rps_cpus", "00000000\n")
	write(t, root, "sys/class/net/tun0/queues/rx-0/rps_flow_cnt", "0\n")
	write(t, root, "proc/sys/kernel/osrelease", "6.8.0-45-generic\n")
	write(t, root, "proc/net/route", "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\nbr-x\t0011AC0A\t00000000\t0001\t0\t0\t0\t00FFFFFF\neth0\t00000000\t0100A8C0\t0003\t0\t0\t100\t00000000\n")
	var ran []string
	s := System{
		Root: root, Ctl: "acme-stow",
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			ran = append(ran, strings.Join(append([]string{name}, args...), " "))
			if name == "/usr/sbin/ethtool" && args[0] == "-k" {
				return []byte("Features for eth0:\nrx-gro-list: off\nrx-udp-gro-forwarding: off\n"), nil
			}
			return nil, nil
		},
		Look:   func(f string) (string, error) { return "/usr/sbin/" + f, nil },
		Euid:   func() int { return 0 },
		Docker: &Docker{NetworkOpts: true},
	}
	return s, &ran
}

func find(items []Item, kind, name string) *Item {
	for i := range items {
		if items[i].Kind == kind && items[i].Name == name {
			return &items[i]
		}
	}
	return nil
}

func TestInspect(t *testing.T) {
	t.Parallel()
	s, _ := fakeHost(t, "1460")
	h := host(4096, 2)
	items := s.Inspect(context.Background(), h)
	for _, c := range []struct {
		kind, name string
		state      State
		have       string
	}{
		{"sysctl", "net.ipv4.tcp_rmem", Differs, "4096 131072 6291456"},
		{"sysctl", "net.core.default_qdisc", Differs, "fq_codel"},
		{"sysctl", "net.ipv4.tcp_mtu_probing", OK, "1"},
		{"sysctl", "net.ipv4.conf.all.rp_filter", Differs, "1"},
		{"module", "tun", Differs, "absent"},
		{"nic", "eth0 rx-udp-gro-forwarding", Differs, "off"},
		{"tun", "tun0 tx_queue_len", Differs, "500"},
		{"tun", "tun0 rps_cpus", Differs, "00000000"},
		{"docker", "default-ulimits.nofile", Differs, "absent"},
		{"docker", "default-network-opts.bridge.com.docker.network.driver.mtu", Differs, "absent"},
	} {
		it := find(items, c.kind, c.name)
		if it == nil || it.State != c.state || it.Have != c.have {
			t.Errorf("%s %s = %+v, want %s with %q", c.kind, c.name, it, c.state, c.have)
		}
	}
	if a := find(items, "nic", "eth0 virtio_net"); a == nil || a.State != Advice || !strings.Contains(a.Have, "1 receive queue(s) for 2 vCPUs") {
		t.Errorf("the queue advice %+v", a)
	}
	if find(items, "nic", "docker0 rx-udp-gro-forwarding") != nil {
		t.Error("a bridge's offload judged: only a NIC backed by a device is")
	}
	if Drift(items) == 0 {
		t.Error("an untuned host reports no drift")
	}
	var b strings.Builder
	if err := Print(&b, items); err != nil || !strings.Contains(b.String(), "differs  sysctl net.ipv4.tcp_rmem") {
		t.Errorf("Print:\n%s", b.String())
	}
}

func TestInspectWithoutEthtoolOrADockerToTune(t *testing.T) {
	t.Parallel()
	s, _ := fakeHost(t, "1500")
	s.Look = func(string) (string, error) { return "", errors.New("not found") }
	s.Docker.Rootless = true
	items := s.Inspect(context.Background(), host(4096, 2))
	// Nothing --apply could change: advice, no drift.
	if it := find(items, "nic", "eth0 rx-udp-gro-forwarding"); it == nil || it.State != Advice || !strings.Contains(it.Have, "ethtool is not installed") {
		t.Errorf("without ethtool: %+v", it)
	}
	if it := find(items, "docker", "daemon.json"); it == nil || it.State != NA {
		t.Errorf("rootless Docker: %+v", it)
	}
	s.Docker = &Docker{Desktop: true}
	if items := s.Inspect(context.Background(), host(4096, 2)); len(items) != 1 || items[0].State != NA {
		t.Errorf("Docker Desktop: %+v", items)
	}
	s.Docker = &Docker{NetworkOpts: true}
	if find(s.Inspect(context.Background(), host(4096, 2)), "docker", "default-network-opts.bridge.com.docker.network.driver.mtu") != nil {
		t.Error("an MTU of 1500, Docker's own, is set")
	}
}

func TestInspectLeavesWhatTheDriverFixesOut(t *testing.T) {
	t.Parallel()
	s, _ := fakeHost(t, "1500")
	for v, want := range map[string]State{"off [fixed]": NA, "on": OK, "off": Differs, "": NA} {
		s.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
			if v == "" {
				return []byte("Features for eth0:\nrx-gro-list: off\n"), nil
			}
			return []byte("Features for eth0:\nrx-udp-gro-forwarding: " + v + "\n"), nil
		}
		if it := find(s.Inspect(context.Background(), host(1024, 1)), "nic", "eth0 rx-udp-gro-forwarding"); it == nil || it.State != want {
			t.Errorf("%q: %+v, want %s", v, it, want)
		}
	}
}

func TestDockerdFlagsKeepTheirKeysOutOfTheFile(t *testing.T) {
	t.Parallel()
	s, _ := fakeHost(t, "9001")
	write(t, s.Root, "proc/812/comm", "dockerd\n")
	write(t, s.Root, "proc/812/status", "Name:\tdockerd\nUid:\t0\t0\t0\t0\n")
	write(t, s.Root, "proc/812/cmdline", "/usr/bin/dockerd\x00--default-ulimit=nofile=32768:65536\x00-H\x00fd://\x00")
	write(t, s.Root, "proc/900/comm", "containerd\n")
	p, err := s.Plan(host(1024, 1))
	if err != nil {
		t.Fatal(err)
	}
	if p.Daemon == nil || len(p.DaemonKeys) != 1 || !strings.HasPrefix(p.DaemonKeys[0], "default-network-opts.") {
		t.Errorf("daemon keys %v", p.DaemonKeys)
	}
	if !slices.ContainsFunc(p.Notes, func(n string) bool { return strings.Contains(n, "dockerd runs with --default-ulimit") }) {
		t.Errorf("notes %v", p.Notes)
	}
	if it := find(s.Inspect(context.Background(), host(1024, 1)), "docker", "default-ulimits.nofile"); it == nil || it.State != NA {
		t.Errorf("report %+v", it)
	}
}

func TestContainerSettingsOnlyWhereANamespaceTakesThem(t *testing.T) {
	t.Parallel()
	s, _ := fakeHost(t, "1500")
	h := host(1024, 1)
	if len(s.Container(h)) != len(containerKeys) {
		t.Fatalf("a 6.8 kernel and a plain engine: %v", s.Container(h))
	}
	for name, d := range map[string]*Docker{"rootless": {Rootless: true}, "userns-remap": {UsernsRemap: true}, "Desktop": {Desktop: true}, "unknown engine": nil} {
		s.Docker = d
		if c := s.Container(h); c != nil {
			t.Errorf("%s: %v", name, c)
		}
	}
	s.Docker = &Docker{}
	for rel, n := range map[string]int{"4.14.330": 0, "4.15.0-213-generic": len(containerKeys), "5.4": len(containerKeys), "3.10.0": 0, "x": 0,
		"6.10-rc1": len(containerKeys), "5.4-1-pve": len(containerKeys), "4.9-rc15": 0} {
		write(t, s.Root, "proc/sys/kernel/osrelease", rel+"\n")
		if got := len(s.Container(h)); got != n {
			t.Errorf("kernel %s: %d settings, want %d", rel, got, n)
		}
	}
}

func TestAShrunkHostIsNotKeptAtALargerHostsCeilings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := fakeHost(t, "1500")
	big, small := host(32768, 4), host(2048, 4)
	p, err := s.Plan(big)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx, p, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	minFree := Sysctl{Key: "vm.min_free_kbytes"}.Path()
	if got := read(t, s.Root, minFree); got != "131072\n" {
		t.Fatalf("32 GiB: min_free_kbytes %q", got)
	}
	if it := find(s.Inspect(ctx, small), "sysctl", "vm.min_free_kbytes"); it == nil || it.State != Differs || it.Want != "≥ 10485" {
		t.Errorf("the 2 GiB report: %+v", it)
	}
	if p, err = s.Plan(small); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx, p, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, s.Root, minFree); got != "10485\n" {
		t.Errorf("2 GiB: min_free_kbytes %q, want 0.5 %% of it", got)
	}
}

func TestTheDaemonsConfigFileIsTheOneItReads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := fakeHost(t, "1500")
	snap := "var/snap/docker/current/config/daemon.json"
	write(t, s.Root, "proc/77/comm", "dockerd\n")
	write(t, s.Root, "proc/77/status", "Uid:\t0\t0\t0\t0\n")
	// A user's rootless engine, listed first, is not the system's.
	write(t, s.Root, "proc/1200/comm", "dockerd\n")
	write(t, s.Root, "proc/1200/status", "Uid:\t1000\t1000\t1000\t1000\n")
	write(t, s.Root, "proc/1200/cmdline", "dockerd\x00--config-file=/home/u/.config/docker/daemon.json\x00")
	write(t, s.Root, "proc/77/cmdline", "dockerd\x00--group\x00docker\x00--config-file\x00/"+snap+"\x00")
	write(t, s.Root, snap, `{"log-level": "error"}`)
	p, err := s.Plan(host(1024, 1))
	if err != nil || p.Daemon == nil || p.Daemon.Path != snap {
		t.Fatalf("plan: %v %+v", err, p)
	}
	if _, err := s.Apply(ctx, p, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, DaemonFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("/etc/docker/daemon.json written, which the engine does not read")
	}
	if it := find(s.Inspect(ctx, host(1024, 1)), "docker", "default-ulimits.nofile"); it == nil || it.State != OK {
		t.Errorf("report %+v", it)
	}
	// The revert finds the file the apply wrote, even with the engine gone.
	if err := os.RemoveAll(filepath.Join(s.Root, "proc/77")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Revert(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := read(t, s.Root, snap); strings.Contains(got, "nofile") || !strings.Contains(got, "log-level") {
		t.Errorf("after the revert:\n%s", got)
	}
}

func TestRecordsFollowTheModulesAndTheOperator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := fakeHost(t, "1500")
	conntrack := Sysctl{Key: "net.netfilter.nf_conntrack_max"}.Path()
	if err := os.Remove(filepath.Join(s.Root, conntrack)); err != nil {
		t.Fatal(err)
	}
	run := s.Run
	s.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "modprobe" && args[0] == "nf_conntrack" {
			write(t, s.Root, conntrack, "65536\n")
		}
		return run(ctx, name, args...)
	}
	h := host(1024, 1)
	p, err := s.Plan(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx, p, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if st, err := s.readState(); err != nil || st.Sysctls["net.netfilter.nf_conntrack_max"] != "65536" {
		t.Errorf("the module's own value not recorded: %v %v", err, st)
	}
	// The operator raises a ceiling and turns the filter off: the host's own, kept.
	somaxconn, rp := Sysctl{Key: "net.core.somaxconn"}.Path(), Sysctl{Key: "net.ipv4.conf.all.rp_filter"}.Path()
	write(t, s.Root, somaxconn, "65535\n")
	write(t, s.Root, rp, "0\n")
	items := s.Inspect(ctx, h)
	for _, k := range []string{"net.core.somaxconn", "net.ipv4.conf.all.rp_filter"} {
		if it := find(items, "sysctl", k); it == nil || it.State != OK {
			t.Errorf("%s after the operator's change: %+v", k, it)
		}
	}
	if p, err = s.Plan(h); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx, p, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if read(t, s.Root, somaxconn) != "65535\n" || read(t, s.Root, rp) != "0\n" {
		t.Errorf("an apply undid the operator's values: somaxconn %q, rp_filter %q", read(t, s.Root, somaxconn), read(t, s.Root, rp))
	}
	write(t, s.Root, StateFile, "{")
	if it := find(s.Inspect(ctx, h), "state", StateFile); it == nil || it.State != Differs {
		t.Errorf("a broken record reported as %+v", it)
	}
}

func TestEachDaemonFileIsReverted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := fakeHost(t, "1500")
	write(t, s.Root, DaemonFile, "{}")
	p, err := s.Plan(host(1024, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx, p, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	// The engine moves to a file of its own, relative to its working directory.
	write(t, s.Root, "proc/5/comm", "dockerd\n")
	write(t, s.Root, "proc/5/status", "Uid:\t0\t0\t0\t0\n")
	write(t, s.Root, "proc/5/cmdline", "dockerd\x00--config-file=etc/docker/alt.json\x00")
	write(t, s.Root, "etc/docker/alt.json", "{}")
	if p, err = s.Plan(host(1024, 1)); err != nil || p.Daemon == nil || p.Daemon.Path != "etc/docker/alt.json" {
		t.Fatalf("plan: %v %+v", err, p)
	}
	if _, err := s.Apply(ctx, p, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Revert(ctx, false); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{DaemonFile, "etc/docker/alt.json"} {
		if got := read(t, s.Root, f); strings.Contains(got, "nofile") {
			t.Errorf("%s after the revert:\n%s", f, got)
		}
	}
}

func TestPlanRefusesDockerDesktop(t *testing.T) {
	t.Parallel()
	s, _ := fakeHost(t, "1500")
	s.Docker = &Docker{Desktop: true}
	if _, err := s.Plan(host(1024, 1)); err == nil {
		t.Error("a plan for Docker Desktop's host")
	}
}

func TestApplyThenRevert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, ran := fakeHost(t, "9001")
	h := host(4096, 2)
	write(t, s.Root, DaemonFile, `{"log-driver": "journald", "default-ulimits": {"core": {"Name": "core", "Soft": 0, "Hard": 0}}}`)
	p, err := s.Plan(h)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Changes() || len(p.DaemonKeys) != 2 {
		t.Fatalf("plan: changes %v, daemon keys %v", p.Changes(), p.DaemonKeys)
	}
	var d strings.Builder
	p.Describe(&d, s.Root)
	if !strings.Contains(d.String(), "default-ulimits.nofile: absent →") {
		t.Errorf("Describe:\n%s", d.String())
	}
	notes, err := s.Apply(ctx, p, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, "systemctl restart docker") }) {
		t.Errorf("no word of the restart Docker needs: %v", notes)
	}
	if slices.Contains(*ran, "systemctl restart docker") {
		t.Error("Docker restarted without --restart-docker")
	}
	if !slices.Contains(*ran, "/usr/sbin/ethtool -K eth0 rx-udp-gro-forwarding on rx-gro-list off") || !slices.Contains(*ran, "modprobe tun") {
		t.Errorf("ran %v", *ran)
	}
	if !strings.Contains(read(t, s.Root, SysctlFile), "\nnet.core.rmem_max = 16777216\n") {
		t.Errorf("sysctl file:\n%s", read(t, s.Root, SysctlFile))
	}
	if !strings.Contains(read(t, s.Root, UdevFile), `RUN+="/usr/sbin/ethtool -K $name rx-udp-gro-forwarding on rx-gro-list off"`) {
		t.Errorf("udev file:\n%s", read(t, s.Root, UdevFile))
	}
	daemon := read(t, s.Root, DaemonFile)
	for _, want := range []string{`"log-driver": "journald"`, `"core": {`, `"Soft": 1048576`, `"com.docker.network.driver.mtu": "9001"`} {
		if !strings.Contains(daemon, want) {
			t.Errorf("daemon.json lacks %s:\n%s", want, daemon)
		}
	}
	if got := read(t, s.Root, Sysctl{Key: "net.core.somaxconn"}.Path()); got != "8192\n" {
		t.Errorf("somaxconn live = %q", got)
	}
	if got := read(t, s.Root, "sys/class/net/tun0/tx_queue_len"); got != "10000\n" {
		t.Errorf("tun0 queue live = %q", got)
	}
	// What the fake cannot change still differs: the tun module, the offload its ethtool reads.
	if items := s.Inspect(ctx, h); Drift(items) != 2 {
		var b strings.Builder
		_ = Print(&b, items)
		t.Errorf("after an apply, %d items differ:\n%s", Drift(items), b.String())
	}
	// A second plan changes nothing; the operator then changes the MTU key by hand.
	p, err = s.Plan(h)
	if err != nil || p.Changes() {
		t.Fatalf("plan after apply: %v, changes %v", err, p != nil && p.Changes())
	}
	// A second apply keeps what the first one found for the revert.
	if _, err := s.Apply(ctx, p, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	write(t, s.Root, DaemonFile, strings.Replace(read(t, s.Root, DaemonFile), `"9001"`, `"8000"`, 1))
	notes, err = s.Revert(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{SysctlFile, ModulesFile, UdevFile, StateFile} {
		if _, err := os.Stat(filepath.Join(s.Root, f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left by the revert", f)
		}
	}
	daemon = read(t, s.Root, DaemonFile)
	if strings.Contains(daemon, "nofile") || !strings.Contains(daemon, `"8000"`) || !strings.Contains(daemon, `"core"`) || !strings.Contains(daemon, "journald") {
		t.Errorf("daemon.json after the revert:\n%s", daemon)
	}
	if !slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, "driver.mtu: left as it is") }) {
		t.Errorf("the hand-changed key's note missing: %v", notes)
	}
	if !slices.Contains(*ran, "systemctl restart docker") {
		t.Error("--restart-docker did not restart it")
	}
	if got := read(t, s.Root, Sysctl{Key: "net.core.somaxconn"}.Path()); got != "1\n" {
		t.Errorf("somaxconn after the revert = %q, want the value the apply found", got)
	}
	if got := read(t, s.Root, Sysctl{Key: "net.ipv4.tcp_rmem"}.Path()); got != "4096 131072 6291456\n" {
		t.Errorf("tcp_rmem after the revert = %q", got)
	}
}

func TestApplyKeepsAHigherCeilingAndRefusesWithoutRoot(t *testing.T) {
	t.Parallel()
	s, _ := fakeHost(t, "1500")
	write(t, s.Root, Sysctl{Key: "vm.min_free_kbytes"}.Path(), "67584\n")
	p, err := s.Plan(host(512, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(context.Background(), p, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, s.Root, Sysctl{Key: "vm.min_free_kbytes"}.Path()); got != "67584\n" {
		t.Errorf("min_free_kbytes lowered to %q", got)
	}
	if !strings.Contains(read(t, s.Root, SysctlFile), "\nvm.min_free_kbytes = 67584\n") {
		t.Error("the file lowers min_free_kbytes")
	}
	write(t, s.Root, DaemonFile, `{"default-ulimits": {"nofile": {"Name": "nofile", "Soft": 2097152, "Hard": 2097152}}}`)
	p, err = s.Plan(host(4096, 2))
	if err != nil || p.Daemon != nil {
		t.Fatalf("a higher open-file limit is changed: %v %+v", err, p.Daemon)
	}
	if err := os.Remove(filepath.Join(s.Root, SysctlFile)); err != nil {
		t.Fatal(err)
	}
	s.Euid = func() int { return 1000 }
	if _, err := s.Apply(context.Background(), p, ApplyOptions{}); err == nil || !strings.Contains(err.Error(), "run it as root") {
		t.Errorf("an apply as a user: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, SysctlFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a refused apply wrote")
	}
	write(t, s.Root, DaemonFile, `{"default-ulimits": [1]`)
	if _, err := s.Plan(host(4096, 2)); err == nil {
		t.Error("a daemon file that does not parse is merged")
	}
	write(t, s.Root, DaemonFile, `{"default-ulimits": "x"}`)
	if _, err := s.Plan(host(4096, 2)); err == nil || !strings.Contains(err.Error(), "default-ulimits is not an object") {
		t.Errorf("a daemon key of the wrong type: %v", err)
	}
}

func TestSystemLookIgnoresPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if p, err := SystemLook("sh"); err != nil || !filepath.IsAbs(p) || !strings.HasPrefix(p, "/") {
		t.Errorf("sh = %q, %v", p, err)
	}
	if _, err := SystemLook("stowage-no-such-program"); err == nil {
		t.Error("a missing program found")
	}
}

func TestPlanLeavesARelativeEthtoolOut(t *testing.T) {
	t.Parallel()
	s, _ := fakeHost(t, "1500")
	s.Look = func(string) (string, error) { return "ethtool", nil }
	p, err := s.Plan(host(1024, 1))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(p.Files[2].Content), "ethtool") || len(p.Notes) != 1 {
		t.Errorf("udev file with a relative ethtool:\n%s", p.Files[2].Content)
	}
}

func TestBootApplyNeedsNoRestart(t *testing.T) {
	t.Parallel()
	s, ran := fakeHost(t, "1500")
	p, err := s.Plan(host(2048, 1))
	if err != nil {
		t.Fatal(err)
	}
	notes, err := s.Apply(context.Background(), p, ApplyOptions{Boot: true, RestartDocker: true})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(*ran, "systemctl restart docker") || slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, "restart") }) {
		t.Errorf("a boot apply restarts Docker: %v %v", *ran, notes)
	}
}
