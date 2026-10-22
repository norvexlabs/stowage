// Package tune computes a host's network tuning from its RAM and its CPUs, reports how the host
// differs from it, and writes and applies the files that make it last. One set of rules serves
// every shape: a Docker host's operator applies them with `tune --apply`, and a container gets the
// ones its network namespace keeps of its own.
//
// Every memory ceiling is a share of RAM, so a small VM and a large one are each tuned in
// proportion; a ceiling is memory the kernel may use under load, never memory it holds.
package tune

import (
	"bufio"
	"bytes"
	"fmt"
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
}

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
// from 8 MiB (QUIC stacks ask for 7) to 64 MiB. The containers are given it, so it moves only when
// RAM doubles; the socket memory budgets (tcp_mem, udp_mem) bound all sockets together.
func (h Host) SocketBuffer() uint64 {
	b := uint64(8 * mib)
	for b*2 <= h.MemBytes/256 && b < 64*mib {
		b *= 2
	}
	return b
}

// Backlog is the packets each CPU may queue in a burst: 1 % of RAM across the CPUs at ~2 KiB a
// packet.
func (h Host) Backlog() uint64 { return clamp(h.MemBytes/100/(uint64(h.CPUs)*2*kib), 1000, 32768) }

// ConntrackMax is the connections the NAT may track: 2 % of RAM at ~320 B an entry.
func (h Host) ConntrackMax() uint64 { return h.MemBytes * 2 / 100 / 320 }

// MinFreeKB is the memory, in KiB, the kernel keeps free for the allocations a burst of packets
// makes: 0.5 % of RAM, at most 128 MiB. A smaller host keeps the kernel's own, larger, default.
func (h Host) MinFreeKB() uint64 { return min(h.MemBytes/kib/200, 128*kib) }

// FlowEntries sizes the table that steers a flow's packets to the CPU its socket reads on: RAM /
// 128 KiB, a power of two from 4096 to 65536 (at most 512 KiB).
func (h Host) FlowEntries() uint64 {
	n := clamp(h.MemBytes/(128*kib), 4096, 65536)
	p := uint64(4096)
	for p*2 <= n {
		p *= 2
	}
	return p
}

// TunQueueLen is a TUN or TAP device's transmit queue: 1 % of RAM at ~2 KiB a packet, from 500 to
// 10000.
func (h Host) TunQueueLen() uint64 { return clamp(h.MemBytes/100/(2*kib), 500, 10000) }

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
	pages := h.MemBytes / uint64(max(h.PageSize, 1))
	pct := func(p uint64) uint64 { return pages * p / 100 }
	conn := h.ConntrackMax()
	return []Sysctl{
		{"net.core.rmem_max", nums(buf), "the largest receive buffer a socket may ask for (QUIC asks 7 MiB): RAM/256, a power of two", AtLeast},
		{"net.core.wmem_max", nums(buf), "the largest send buffer a socket may ask for: RAM/256, a power of two", AtLeast},
		{"net.ipv4.tcp_rmem", nums(4096, 131072, buf), "how far TCP's receive buffer may grow on a fast, distant link", AtLeast},
		{"net.ipv4.tcp_wmem", nums(4096, 16384, buf), "how far TCP's send buffer may grow", AtLeast},
		{"net.ipv4.tcp_mem", nums(pct(4), pct(6), pct(8)), "all TCP sockets' memory, in pages: 4, 6 and 8 % of RAM", Exact},
		{"net.ipv4.udp_mem", nums(pct(4), pct(6), pct(8)), "all UDP sockets' memory, in pages: 4, 6 and 8 % of RAM", Exact},
		{"net.core.netdev_max_backlog", nums(h.Backlog()), "the packets a CPU may queue in a burst: 1 % of RAM across the CPUs", AtLeast},
		{"net.netfilter.nf_conntrack_max", nums(conn), "the connections the NAT may track: 2 % of RAM", AtLeast},
		{"net.netfilter.nf_conntrack_buckets", nums(conn / 4), "the connection table's hash size", AtLeast},
		{"vm.min_free_kbytes", nums(h.MinFreeKB()), "memory kept free for a burst's allocations: 0.5 % of RAM, at most 128 MiB", AtLeast},
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
