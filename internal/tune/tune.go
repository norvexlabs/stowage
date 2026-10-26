// Package tune computes a host's network tuning from its RAM and its CPUs, reports how the host
// differs from it, and writes and applies the files that make it last. A profile's rules serve
// every shape: a Docker host's operator applies them with `tune --apply`, a Kairos VM at each boot,
// and a container gets the ones its network namespace keeps of its own.
//
// Every memory ceiling is a share of RAM, so a small VM and a large one are each tuned in
// proportion, and never below the kernel's own default; a ceiling is memory the kernel may use
// under load, never memory it holds.
package tune

import (
	"bufio"
	"bytes"
	"fmt"
	"maps"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

const (
	kib = 1 << 10
	mib = 1 << 20
)

// Host is what the rules are computed from.
type Host struct {
	MemBytes uint64 // MemTotal, rounded down to MemStep
	CPUs     int
	Online   []int // the online CPUs' ids; empty is 0 to CPUs-1
	PageSize int
	Profile  Profile
	// Reserved are the ports the stack listens on that the network profile's range of local
	// ports holds: the kernel never gives them to an outgoing connection.
	Reserved []int
	// Settings are the project's own kernel settings (host.sysctls), written after the profile's:
	// none is a key a profile sets.
	Settings []Sysctl
	// Isolated are the CPUs of the services' whole cores that the NICs' interrupts and receive
	// steering stay off (System.isolated).
	Isolated []int
}

// Profile is how far the rules lean on the network. Both keep every value at the kernel's default
// or above; each is a list of rules (defaultRules, networkRules).
type Profile string

const (
	// Default makes Docker's own path as fast as the host's network: a container keeps the host's
	// settings, the stack's networks its MTU, and the host the connection table and the queue
	// that path goes through. The zero value is Default.
	Default Profile = "default"
	// Network is a host that runs the stack alone and serves a heavy network load: Default's
	// rules, then larger ceilings, BBR and fq, the ports and neighbours of many connections, NIC
	// rings, interrupts and flow steering, the CPUs at their full speed.
	Network Profile = "network"
	// legacyShared is the profile a Stowage before profiles recorded: Default now.
	legacyShared Profile = "shared"
)

// Profiles are the profiles' names, the lower first.
var Profiles = []Profile{Default, Network}

// ParseProfile reads a profile's name.
func ParseProfile(s string) (Profile, error) {
	if p := Profile(s); slices.Contains(Profiles, p) {
		return p, nil
	}
	return "", fmt.Errorf("profile %q: default or network", s)
}

// Higher is the profile that leans harder of a and b; "" is Default.
func Higher(a, b Profile) Profile {
	if a == Network || b == Network {
		return Network
	}
	return Default
}

func (h Host) network() bool { return h.Profile == Network }

func (h Host) pages() uint64 { return h.MemBytes / uint64(max(h.PageSize, 1)) }

// MemStep is the RAM's granularity: MemTotal moves by a few KiB from one kernel or boot to the
// next, which must not make another host of it.
const MemStep = 64 * mib

// ReadHost reads the machine's RAM (proc/meminfo under root) and its online CPUs (the CPUs this
// process may run on when the kernel does not list them).
func ReadHost(root string) (Host, error) {
	raw, err := os.ReadFile(filepath.Join(root, "proc/meminfo"))
	if err != nil {
		return Host{}, err
	}
	var mem uint64
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 3 && f[0] == "MemTotal:" && f[2] == "kB" {
			kb, err := strconv.ParseUint(f[1], 10, 64)
			if err != nil || kb == 0 {
				return Host{}, fmt.Errorf("proc/meminfo: MemTotal %q", f[1])
			}
			mem = max(kb*kib/MemStep, 1) * MemStep
			break
		}
	}
	if mem == 0 {
		return Host{}, fmt.Errorf("proc/meminfo: no MemTotal")
	}
	h := Host{MemBytes: mem, CPUs: max(runtime.NumCPU(), 1), PageSize: os.Getpagesize()}
	if raw, err := os.ReadFile(filepath.Join(root, "sys/devices/system/cpu/online")); err == nil {
		if ids, ok := cpuList(strings.TrimSpace(string(raw))); ok {
			h.Online, h.CPUs = ids, len(ids)
		}
	}
	return h, nil
}

// cpuList reads a kernel CPU list, as "0-3,8,10-11".
func cpuList(s string) ([]int, bool) {
	var ids []int
	for part := range strings.SplitSeq(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil || a < 0 || a > 4095 {
			return nil, false
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a || b > 4095 {
				return nil, false
			}
		}
		for i := a; i <= b; i++ {
			ids = append(ids, i)
		}
	}
	return ids, len(ids) > 0
}

func clamp(v, lo, hi uint64) uint64 { return min(max(v, lo), hi) }

// SocketBuffer is the largest buffer one socket may get on the network profile: RAM/128 rounded
// down to a power of two, from 16 MiB (QUIC stacks ask for 7) to 128 MiB (10 Gbit/s over 100 ms).
// The containers are given it, so it moves only when RAM doubles; the socket memory budgets
// (tcp_mem, udp_mem) bound all sockets together.
func (h Host) SocketBuffer() uint64 { return clamp(floorPow2(h.MemBytes/128), 16*mib, 128*mib) }

// floorPow2 is the largest power of two at most n; 0 for 0.
func floorPow2(n uint64) uint64 {
	if n == 0 {
		return 0
	}
	return 1 << (bits.Len64(n) - 1)
}

// Backlog is the packets each CPU may queue in a burst: 1 % of RAM across the CPUs at ~2 KiB a
// packet, from 1000 (the kernel's default) to 32768; on the network profile 2 %, from 65536 to
// 262144.
func (h Host) Backlog() uint64 {
	if h.network() {
		return clamp(h.MemBytes*2/100/(uint64(h.CPUs)*2*kib), 65536, 262144)
	}
	return clamp(h.MemBytes/100/(uint64(h.CPUs)*2*kib), 1000, 32768)
}

// kernelConntrack is the kernel's own size of the connection table, its buckets and its entries
// alike: 262144 above 4 GiB, 65536 above 1 GiB, else RAM/16384 buckets of 8 B, at least 1024.
func (h Host) kernelConntrack() uint64 {
	switch {
	case h.MemBytes > 4<<30:
		return 262144
	case h.MemBytes > 1<<30:
		return 65536
	}
	return max(h.MemBytes/16384/8, 1024)
}

// ConntrackMax is the connections the NAT may track: 2 % of RAM at ~320 B an entry (4 % on the
// network profile), at least the kernel's default.
func (h Host) ConntrackMax() uint64 {
	share := uint64(2)
	if h.network() {
		share = 4
	}
	return max(h.MemBytes*share/100/320, h.kernelConntrack())
}

// ConntrackBuckets is the connection table's hash size: a quarter of its entries, at least the
// kernel's default.
func (h Host) ConntrackBuckets() uint64 { return max(h.ConntrackMax()/4, h.kernelConntrack()) }

// MinFreeKB is the memory, in KiB, the kernel keeps free for the allocations a burst of packets
// makes: 0.5 % of RAM, at most 128 MiB, and never below the kernel's own default (√(16 × RAM in
// KiB), from 128 KiB to 256 MiB). It is the one rule that takes memory from the processes.
func (h Host) MinFreeKB() uint64 {
	kernel := clamp(uint64(math.Sqrt(float64(h.MemBytes/kib*16))), 128, 256*kib)
	return max(min(h.MemBytes/kib/200, 128*kib), kernel)
}

// socketMem is a socket memory budget, in pages, as the kernel sizes it from limit (its low mark
// three quarters of it, its high mark twice the low), scaled by num/den.
func socketMem(limit, num, den uint64) string {
	limit = max(limit, 128)
	low := limit / 4 * 3
	return nums(low*num/den, limit*num/den, low*2*num/den)
}

// TCPMem is the budget of every TCP socket together: the kernel's (RAM/16 pages) × 2.
func (h Host) TCPMem() string { return socketMem(h.pages()/16, 2, 1) }

// UDPMem is the budget of every UDP socket together (QUIC, tunnels): the kernel's (RAM/8 pages) ×
// 1.5.
func (h Host) UDPMem() string { return socketMem(h.pages()/8, 3, 2) }

// FlowEntries sizes the table that steers a flow's packets to the CPU its socket reads on: RAM /
// 128 KiB, a power of two from 4096 to 65536 (at most 512 KiB).
func (h Host) FlowEntries() uint64 {
	return floorPow2(clamp(h.MemBytes/(128*kib), 4096, 65536))
}

// TunQueueLen is a TUN or TAP device's transmit queue on the network profile: 2 % of RAM at ~2 KiB
// a packet, from 1000 to 10000.
func (h Host) TunQueueLen() uint64 { return clamp(h.MemBytes*2/100/(2*kib), 1000, 10000) }

// RingEntries is the receive and transmit ring a NIC of queues queues is given on the network
// profile: the driver's maximum, within 1 % of RAM at ~2 KiB a slot, a power of two; 0 leaves the
// NIC's own.
func (h Host) RingEntries(queues int, driverMax uint64) uint64 {
	if !h.network() || driverMax == 0 {
		return 0
	}
	budget := h.MemBytes / 100 / (2 * kib * 2 * uint64(max(queues, 1)))
	p := floorPow2(min(driverMax, budget))
	if p < 64 {
		return 0
	}
	return p
}

// QueueFlows is each receive queue's share of the steered flows: the table over the CPUs (a
// multiqueue NIC has a queue per CPU), a power of two, as the udev rule and an apply both write
// it; 0 off the network profile, where a NIC's queues are not steered.
func (h Host) QueueFlows() uint64 {
	if !h.network() {
		return 0
	}
	return max(floorPow2(h.FlowEntries()/uint64(max(h.CPUs, 1))), 1)
}

// RPSMask is the CPU mask of every online CPU the steering may use (steerCPUs), as rps_cpus reads
// it: 32-bit hex groups, comma separated.
func (h Host) RPSMask() string {
	ids := h.steerCPUs()
	groups := make([]uint32, slices.Max(ids)/32+1)
	for _, id := range ids {
		groups[id/32] |= 1 << (id % 32)
	}
	out := make([]string, len(groups))
	for i, g := range groups {
		out[len(groups)-1-i] = fmt.Sprintf("%08x", g)
	}
	return strings.Join(out, ",")
}

// Sysctl is one kernel setting and the values that satisfy it.
type Sysctl struct {
	Key  string
	Want string // the value written
	Why  string
	// Rule is how a value already set is judged: Exact, AtLeast (a ceiling a host may hold
	// higher), AtMost, Range or Ports; a vector is judged field by field.
	Rule Rule
}

// Rule is how a current value is judged against Want.
type Rule int

const (
	Exact Rule = iota
	AtLeast
	AtMost
	NotStrict // rp_filter: any value but 1 (strict)
	// Range is a range of two fields that holds Want's: its low at most Want's, its high at
	// least.
	Range
	// Ports is a list of ports and ranges (ip_local_reserved_ports) that holds every port of
	// Want's.
	Ports
)

// Satisfied reports whether cur, as /proc/sys reads it, needs no change.
func (s Sysctl) Satisfied(cur string) bool {
	have, want := strings.Fields(cur), strings.Fields(s.Want)
	switch s.Rule {
	case NotStrict:
		return len(have) == 1 && have[0] != "1"
	case Ports:
		held, ok := portSet(cur)
		wanted, _ := portSet(s.Want)
		return ok && !slices.ContainsFunc(slices.Collect(maps.Keys(wanted)), func(p int) bool { return !held[p] })
	}
	if len(have) != len(want) {
		return false
	}
	for i := range want {
		h, errH := strconv.ParseInt(have[i], 10, 64)
		w, errW := strconv.ParseInt(want[i], 10, 64)
		if errH != nil || errW != nil {
			if have[i] != want[i] {
				return false
			}
			continue
		}
		rule := s.Rule
		if rule == Range {
			rule = map[bool]Rule{true: AtMost, false: AtLeast}[i == 0]
		}
		switch {
		case rule == AtLeast && h < w, rule == AtMost && h > w, rule == Exact && h != w:
			return false
		}
	}
	return true
}

// Value is what an apply writes over cur: Want, but a ceiling the host holds higher (lower, for
// AtMost) is kept, field by field, a range the host holds wider is kept, a filter that is not
// strict stays as it is, and a list of ports keeps the host's and adds Want's.
func (s Sysctl) Value(cur string) string {
	have, want := strings.Fields(cur), strings.Fields(s.Want)
	switch {
	case s.Rule == NotStrict && s.Satisfied(cur):
		return have[0]
	case s.Rule == Ports:
		return portUnion(cur, s.Want)
	case s.Rule != AtLeast && s.Rule != AtMost && s.Rule != Range, len(have) != len(want):
		return s.Want
	}
	out := make([]string, len(want))
	for i := range want {
		h, errH := strconv.ParseInt(have[i], 10, 64)
		w, errW := strconv.ParseInt(want[i], 10, 64)
		switch {
		case errH != nil || errW != nil:
			return s.Want
		case s.Rule == AtLeast, s.Rule == Range && i > 0:
			out[i] = strconv.FormatInt(max(h, w), 10)
		default:
			out[i] = strconv.FormatInt(min(h, w), 10)
		}
	}
	return strings.Join(out, " ")
}

// portSet reads a kernel list of ports and port ranges, as "8080,9000-9100"; "" is none. ok is
// false for a list that does not read.
func portSet(s string) (map[int]bool, bool) {
	out := map[int]bool{}
	s = strings.TrimSpace(s)
	if s == "" {
		return out, true
	}
	for part := range strings.SplitSeq(s, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		a, err := strconv.Atoi(lo)
		if err != nil || a < 1 || a > 65535 {
			return nil, false
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a || b > 65535 {
				return nil, false
			}
		}
		for p := a; p <= b; p++ {
			out[p] = true
		}
	}
	return out, true
}

// portUnion is the list cur, with each port of want it does not hold added; want alone when cur
// does not read.
func portUnion(cur, want string) string {
	held, ok := portSet(cur)
	cur = strings.TrimSpace(cur)
	if !ok || cur == "" {
		return want
	}
	wanted, _ := portSet(want)
	missing := []string{}
	for _, p := range slices.Sorted(maps.Keys(wanted)) {
		if !held[p] {
			missing = append(missing, strconv.Itoa(p))
		}
	}
	return strings.Join(append([]string{cur}, missing...), ",")
}

// Path is the setting's file under proc/sys.
func (s Sysctl) Path() string { return filepath.Join("proc/sys", strings.ReplaceAll(s.Key, ".", "/")) }

func nums(v ...uint64) string {
	f := make([]string, len(v))
	for i, n := range v {
		f[i] = strconv.FormatUint(n, 10)
	}
	return strings.Join(f, " ")
}

// rule is one setting of a profile: its key, how a value the host holds is judged, why it is
// set, and its value on h ("" leaves it out).
type rule struct {
	key   string
	judge Rule
	why   string
	want  func(h Host) string
}

func fixed(v string) func(Host) string { return func(Host) string { return v } }

// defaultRules are the Default profile's: what Docker's path from the host's network to a
// container goes through — the NAT's connection table, the queue each packet a bridge forwards
// waits in.
var defaultRules = []rule{
	{"net.core.netdev_max_backlog", AtLeast, "the packets a CPU may queue in a burst, a bridge's to its containers included: 1 % of RAM across the CPUs", func(h Host) string { return nums(h.Backlog()) }},
	{"net.netfilter.nf_conntrack_max", AtLeast, "the connections Docker's NAT may track: 2 % of RAM, at least the kernel's default", func(h Host) string { return nums(h.ConntrackMax()) }},
	{"net.netfilter.nf_conntrack_buckets", AtLeast, "the connection table's hash size", func(h Host) string { return nums(h.ConntrackBuckets()) }},
}

// LocalPorts is the network profile's range of local ports: the ports outgoing connections take,
// whose width caps the connections open to one destination.
const LocalPorts = "10000 65535"

// networkRules are the Network profile's, over Default's: a key in both takes the network's value,
// in Default's place.
var networkRules = []rule{
	{"net.core.rmem_max", AtLeast, "the largest receive buffer a socket may ask for (QUIC asks 7 MiB): RAM/128, a power of two", func(h Host) string { return nums(h.SocketBuffer()) }},
	{"net.core.wmem_max", AtLeast, "the largest send buffer a socket may ask for: RAM/128, a power of two", func(h Host) string { return nums(h.SocketBuffer()) }},
	{"net.ipv4.tcp_rmem", AtLeast, "how far TCP's receive buffer may grow on a fast, distant link", func(h Host) string { return nums(4096, 131072, h.SocketBuffer()) }},
	{"net.ipv4.tcp_wmem", AtLeast, "how far TCP's send buffer may grow", func(h Host) string { return nums(4096, 16384, h.SocketBuffer()) }},
	{"net.ipv4.tcp_mem", AtLeast, "all TCP sockets' memory, in pages: the kernel's default × 2", Host.TCPMem},
	{"net.ipv4.udp_mem", AtLeast, "all UDP sockets' memory (QUIC, tunnels), in pages: the kernel's default × 1.5", Host.UDPMem},
	{"net.core.netdev_max_backlog", AtLeast, "the packets a CPU may queue in a burst: 2 % of RAM across the CPUs, at least 65536", func(h Host) string { return nums(h.Backlog()) }},
	{"net.netfilter.nf_conntrack_max", AtLeast, "the connections the NAT may track: 4 % of RAM, at least the kernel's default", func(h Host) string { return nums(h.ConntrackMax()) }},
	{"vm.min_free_kbytes", AtLeast, "memory kept free for a burst's allocations: 0.5 % of RAM, at most 128 MiB, at least the kernel's default", func(h Host) string { return nums(h.MinFreeKB()) }},
	{"net.core.rps_sock_flow_entries", AtLeast, "the flows steered to the CPU their socket reads on", func(h Host) string { return nums(h.FlowEntries()) }},
	{"net.core.default_qdisc", Exact, "pacing: what a QUIC or BBR sender asks of the queue", fixed("fq")},
	{"net.ipv4.tcp_congestion_control", Exact, "throughput on a lossy or distant link", fixed("bbr")},
	{"net.ipv4.tcp_notsent_lowat", AtMost, "less unsent data held in a socket: lower latency, less memory", fixed("131072")},
	{"net.ipv4.tcp_slow_start_after_idle", Exact, "a kept-alive connection resumes at its speed", fixed("0")},
	{"net.ipv4.tcp_mtu_probing", AtLeast, "a path that drops ICMP still finds its MTU", fixed("1")},
	{"net.core.somaxconn", AtLeast, "the connections a listener may queue", fixed("8192")},
	{"net.ipv4.tcp_max_syn_backlog", AtLeast, "the handshakes a listener may queue", fixed("8192")},
	{"net.ipv4.tcp_tw_reuse", Exact, "an outgoing connection may reuse a port in TIME_WAIT: a stack dialling one destination fast does not run out of ports", fixed("1")},
	{"net.ipv4.ip_local_port_range", Range, "the ports outgoing connections take: the range's width caps the connections open to one destination", fixed(LocalPorts)},
	{"net.ipv4.ip_local_reserved_ports", Ports, "the stack's own ports within that range, never given to an outgoing connection", func(h Host) string { return h.reserved() }},
	{"net.ipv4.neigh.default.gc_thresh1", AtLeast, "the neighbour entries kept without garbage collection, for a network of many clients", fixed("8192")},
	{"net.ipv4.neigh.default.gc_thresh2", AtLeast, "the neighbour entries before collection turns aggressive", fixed("32768")},
	{"net.ipv4.neigh.default.gc_thresh3", AtLeast, "the neighbour table's cap: past it a client's address does not resolve", fixed("65536")},
	{"net.ipv6.neigh.default.gc_thresh1", AtLeast, "the same for IPv6", fixed("8192")},
	{"net.ipv6.neigh.default.gc_thresh2", AtLeast, "the same for IPv6", fixed("32768")},
	{"net.ipv6.neigh.default.gc_thresh3", AtLeast, "the same for IPv6", fixed("65536")},
	{"net.ipv4.conf.all.rp_filter", NotStrict, "loose reverse-path filtering: a tunnel's replies may come back another way", fixed("2")},
	{"net.ipv4.conf.default.rp_filter", NotStrict, "the same for interfaces made later", fixed("2")},
	{"net.core.netdev_budget", AtLeast, "the packets a CPU takes from the NICs in one pass: fewer drops in a flood", fixed("600")},
	{"net.core.netdev_budget_usecs", AtLeast, "the time that pass may take", fixed("8000")},
}

// reserved is Reserved within LocalPorts, as ip_local_reserved_ports reads it; "" when none is.
func (h Host) reserved() string {
	lo, _, _ := strings.Cut(LocalPorts, " ")
	low, _ := strconv.Atoi(lo)
	var out []string
	for _, p := range slices.Compact(slices.Sorted(slices.Values(h.Reserved))) {
		if p >= low && p <= 65535 {
			out = append(out, strconv.Itoa(p))
		}
	}
	return strings.Join(out, ",")
}

// rules are h's profile's rules: Default's, and the network's over them.
func (h Host) rules() []rule {
	if !h.network() {
		return defaultRules
	}
	out := slices.Clone(defaultRules)
	for _, r := range networkRules {
		if i := slices.IndexFunc(out, func(o rule) bool { return o.key == r.key }); i >= 0 {
			out[i] = r
		} else {
			out = append(out, r)
		}
	}
	return out
}

// Sysctls are the host's settings, in the order they are written.
func (h Host) Sysctls() []Sysctl {
	var out []Sysctl
	for _, r := range h.rules() {
		if v := r.want(h); v != "" {
			out = append(out, Sysctl{r.key, v, r.why, r.judge})
		}
	}
	for _, c := range h.Settings {
		if !slices.ContainsFunc(out, func(o Sysctl) bool { return o.Key == c.Key }) {
			out = append(out, c)
		}
	}
	return out
}

// ProfileKeys are the settings a profile sets on some host, sorted: the project's own
// (host.sysctls) are other keys.
func ProfileKeys() []string {
	var out []string
	for _, p := range Profiles {
		h := Host{Profile: p}
		for _, r := range h.rules() {
			if !slices.Contains(out, r.key) {
				out = append(out, r.key)
			}
		}
	}
	slices.Sort(out)
	return out
}

// containerKeys are the settings a container's network namespace keeps of its own, most starting
// at the kernel's defaults rather than the host's: every profile gives a container the host's
// value, so a service runs as it would on the host, and the network profile its own value where
// the host holds less.
var containerKeys = []string{
	"net.core.somaxconn",
	"net.ipv4.ip_local_port_range",
	"net.ipv4.tcp_max_syn_backlog",
	"net.ipv4.tcp_mtu_probing",
	"net.ipv4.tcp_notsent_lowat",
	"net.ipv4.tcp_rmem",
	"net.ipv4.tcp_slow_start_after_idle",
	"net.ipv4.tcp_tw_reuse",
	"net.ipv4.tcp_wmem",
}

// podKeys are the container settings a Kubernetes pod is given: those its namespace starts at a
// fixed default for. It copies the node's TCP buffer sizes, which the node's own tuning sets, and
// sizes its SYN queue from the node's memory, which the installer does not read.
var podKeys = []string{
	"net.core.somaxconn",
	"net.ipv4.tcp_mtu_probing",
	"net.ipv4.tcp_notsent_lowat",
	"net.ipv4.tcp_slow_start_after_idle",
}

// Pod is the settings each pod is given, sorted by key: the network profile's, as the kubelet's
// unsafe sysctls, the same on every node, as no node's size enters them.
func Pod() []Sysctl {
	h := Host{MemBytes: MemStep, CPUs: 1, PageSize: 4096, Profile: Network}
	var out []Sysctl
	for _, s := range h.Sysctls() {
		if slices.Contains(podKeys, s.Key) {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b Sysctl) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// Modules are the kernel modules h's profile loads at boot: the TUN device and the connection
// tracker whose settings are written; on the network profile BBR and the fq queue too.
func (h Host) Modules() []string {
	if h.network() {
		return []string{"tun", "nf_conntrack", "tcp_bbr", "sch_fq"}
	}
	return []string{"tun", "nf_conntrack"}
}

// NofileLimit is the most open files a container is given, soft and hard: fs.nr_open's default.
const NofileLimit = 1048576
