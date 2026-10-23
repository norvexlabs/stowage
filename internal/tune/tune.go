// Package tune computes a host's network tuning from its RAM and its CPUs, reports how the host
// differs from it, and writes and applies the files that make it last. One set of rules serves
// every shape: a Docker host's operator applies them with `tune --apply`, and a container gets the
// ones its network namespace keeps of its own.
//
// Every memory ceiling is a share of RAM, so a small VM and a large one are each tuned in
// proportion, and never below the kernel's own default; a ceiling is memory the kernel may use
// under load, never memory it holds. A Kairos VM, which runs the stack alone, takes the Network
// profile at each boot.
package tune

import (
	"bufio"
	"bytes"
	"fmt"
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
}

// Profile is how far the rules lean on the network. Both keep every value at the kernel's default
// or above.
type Profile string

const (
	// Shared is a host whose other processes the admin runs: the zero value.
	Shared Profile = "shared"
	// Network is a host that runs the stack alone, a Kairos VM: larger ceilings, NIC rings and
	// flow steering.
	Network Profile = "network"
)

// ParseProfile reads a profile's name; "" is Shared.
func ParseProfile(s string) (Profile, error) {
	switch Profile(s) {
	case "", Shared:
		return Shared, nil
	case Network:
		return Network, nil
	}
	return "", fmt.Errorf("profile %q: shared or network", s)
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

// SocketBuffer is the largest buffer one socket may get: RAM/256 rounded down to a power of two,
// from 8 MiB (QUIC stacks ask for 7) to 64 MiB; on the network profile RAM/128, from 16 to 128 MiB
// (10 Gbit/s over 100 ms). The containers are given it, so it moves only when RAM doubles; the
// socket memory budgets (tcp_mem, udp_mem) bound all sockets together.
func (h Host) SocketBuffer() uint64 {
	b, top, share := uint64(8*mib), uint64(64*mib), uint64(256)
	if h.network() {
		b, top, share = 16*mib, 128*mib, 128
	}
	return clamp(floorPow2(h.MemBytes/share), b, top)
}

// floorPow2 is the largest power of two at most n; 0 for 0.
func floorPow2(n uint64) uint64 {
	if n == 0 {
		return 0
	}
	return 1 << (bits.Len64(n) - 1)
}

// Backlog is the packets each CPU may queue in a burst: 1 % of RAM across the CPUs at ~2 KiB a
// packet, from 1000 (the kernel's default) to 32768; on the network profile 2 %, up to 65536.
func (h Host) Backlog() uint64 {
	if h.network() {
		return clamp(h.MemBytes*2/100/(uint64(h.CPUs)*2*kib), 1000, 65536)
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

// TCPMem is the budget of every TCP socket together: the kernel's (RAM/16 pages) × 1.5, × 2 on the
// network profile.
func (h Host) TCPMem() string {
	if h.network() {
		return socketMem(h.pages()/16, 2, 1)
	}
	return socketMem(h.pages()/16, 3, 2)
}

// UDPMem is the budget of every UDP socket together (QUIC, tunnels): the kernel's (RAM/8 pages) ×
// 1.5.
func (h Host) UDPMem() string { return socketMem(h.pages()/8, 3, 2) }

// FlowEntries sizes the table that steers a flow's packets to the CPU its socket reads on: RAM /
// 128 KiB, a power of two from 4096 to 65536 (at most 512 KiB).
func (h Host) FlowEntries() uint64 {
	return floorPow2(clamp(h.MemBytes/(128*kib), 4096, 65536))
}

// TunQueueLen is a TUN or TAP device's transmit queue: 1 % of RAM at ~2 KiB a packet, from 500 to
// 10000; on the network profile 2 %, from 1000.
func (h Host) TunQueueLen() uint64 {
	if h.network() {
		return clamp(h.MemBytes*2/100/(2*kib), 1000, 10000)
	}
	return clamp(h.MemBytes/100/(2*kib), 500, 10000)
}

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

// RPSMask is the CPU mask of every online CPU, as rps_cpus reads it: 32-bit hex groups, comma
// separated.
func (h Host) RPSMask() string {
	ids := h.Online
	if len(ids) == 0 {
		for i := range max(h.CPUs, 1) {
			ids = append(ids, i)
		}
	}
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
	// higher) or AtMost; a vector is judged field by field.
	Rule Rule
}

// Rule is how a current value is judged against Want.
type Rule int

const (
	Exact Rule = iota
	AtLeast
	AtMost
	NotStrict // rp_filter: any value but 1 (strict)
)

// Satisfied reports whether cur, as /proc/sys reads it, needs no change.
func (s Sysctl) Satisfied(cur string) bool {
	have, want := strings.Fields(cur), strings.Fields(s.Want)
	if s.Rule == NotStrict {
		return len(have) == 1 && have[0] != "1"
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
		switch {
		case s.Rule == AtLeast && h < w, s.Rule == AtMost && h > w, s.Rule == Exact && h != w:
			return false
		}
	}
	return true
}

// Value is what an apply writes over cur: Want, but a ceiling the host holds higher (lower, for
// AtMost) is kept, field by field, and a filter that is not strict stays as it is.
func (s Sysctl) Value(cur string) string {
	have, want := strings.Fields(cur), strings.Fields(s.Want)
	switch {
	case s.Rule == NotStrict && s.Satisfied(cur):
		return have[0]
	case s.Rule != AtLeast && s.Rule != AtMost, len(have) != len(want):
		return s.Want
	}
	out := make([]string, len(want))
	for i := range want {
		h, errH := strconv.ParseInt(have[i], 10, 64)
		w, errW := strconv.ParseInt(want[i], 10, 64)
		switch {
		case errH != nil || errW != nil:
			return s.Want
		case s.Rule == AtLeast:
			out[i] = strconv.FormatInt(max(h, w), 10)
		default:
			out[i] = strconv.FormatInt(min(h, w), 10)
		}
	}
	return strings.Join(out, " ")
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

// Sysctls are the host's settings, in the order they are written.
func (h Host) Sysctls() []Sysctl {
	buf := h.SocketBuffer()
	share, tcp, backlog, conn := "RAM/256", "1.5", "1 %", "2 %"
	if h.network() {
		share, tcp, backlog, conn = "RAM/128", "2", "2 %", "4 %"
	}
	out := []Sysctl{
		{"net.core.rmem_max", nums(buf), "the largest receive buffer a socket may ask for (QUIC asks 7 MiB): " + share + ", a power of two", AtLeast},
		{"net.core.wmem_max", nums(buf), "the largest send buffer a socket may ask for: " + share + ", a power of two", AtLeast},
		{"net.ipv4.tcp_rmem", nums(4096, 131072, buf), "how far TCP's receive buffer may grow on a fast, distant link", AtLeast},
		{"net.ipv4.tcp_wmem", nums(4096, 16384, buf), "how far TCP's send buffer may grow", AtLeast},
		{"net.ipv4.tcp_mem", h.TCPMem(), "all TCP sockets' memory, in pages: the kernel's default × " + tcp, AtLeast},
		{"net.ipv4.udp_mem", h.UDPMem(), "all UDP sockets' memory (QUIC, tunnels), in pages: the kernel's default × 1.5", AtLeast},
		{"net.core.netdev_max_backlog", nums(h.Backlog()), "the packets a CPU may queue in a burst: " + backlog + " of RAM across the CPUs", AtLeast},
		{"net.netfilter.nf_conntrack_max", nums(h.ConntrackMax()), "the connections the NAT may track: " + conn + " of RAM, at least the kernel's default", AtLeast},
		{"net.netfilter.nf_conntrack_buckets", nums(h.ConntrackBuckets()), "the connection table's hash size", AtLeast},
		{"vm.min_free_kbytes", nums(h.MinFreeKB()), "memory kept free for a burst's allocations: 0.5 % of RAM, at most 128 MiB, at least the kernel's default", AtLeast},
		{"net.core.rps_sock_flow_entries", nums(h.FlowEntries()), "the flows steered to the CPU their socket reads on", AtLeast},
		{"net.core.default_qdisc", "fq", "pacing: what a QUIC or BBR sender asks of the queue", Exact},
		{"net.ipv4.tcp_congestion_control", "bbr", "throughput on a lossy or distant link", Exact},
		{"net.ipv4.tcp_notsent_lowat", "131072", "less unsent data held in a socket: lower latency, less memory", AtMost},
		{"net.ipv4.tcp_slow_start_after_idle", "0", "a kept-alive connection resumes at its speed", Exact},
		{"net.ipv4.tcp_mtu_probing", "1", "a path that drops ICMP still finds its MTU", AtLeast},
		{"net.core.somaxconn", "8192", "the connections a listener may queue", AtLeast},
		{"net.ipv4.tcp_max_syn_backlog", "8192", "the handshakes a listener may queue", AtLeast},
		{"net.ipv4.conf.all.rp_filter", "2", "loose reverse-path filtering: a tunnel's replies may come back another way", NotStrict},
		{"net.ipv4.conf.default.rp_filter", "2", "the same for interfaces made later", NotStrict},
	}
	if h.network() {
		out = append(out,
			Sysctl{"net.core.netdev_budget", "600", "the packets a CPU takes from the NICs in one pass: fewer drops in a flood", AtLeast},
			Sysctl{"net.core.netdev_budget_usecs", "8000", "the time that pass may take", AtLeast})
	}
	return out
}

// containerKeys are the settings a container's network namespace keeps of its own, starting from
// the kernel's defaults rather than the host's.
var containerKeys = []string{
	"net.core.somaxconn",
	"net.ipv4.tcp_max_syn_backlog",
	"net.ipv4.tcp_mtu_probing",
	"net.ipv4.tcp_notsent_lowat",
	"net.ipv4.tcp_rmem",
	"net.ipv4.tcp_slow_start_after_idle",
	"net.ipv4.tcp_wmem",
}

// Container is the settings each container is given, by key.
func (h Host) Container() map[string]string {
	out := map[string]string{}
	for _, s := range h.Sysctls() {
		if slices.Contains(containerKeys, s.Key) {
			out[s.Key] = s.Want
		}
	}
	return out
}

// Modules are the kernel modules loaded at boot: the TUN device, the connection tracker whose
// settings are written, BBR and the fq queue.
var Modules = []string{"tun", "nf_conntrack", "tcp_bbr", "sch_fq"}

// NofileLimit is the open files a container may hold, soft and hard.
const NofileLimit = 1048576
