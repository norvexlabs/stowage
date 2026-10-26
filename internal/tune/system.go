package tune

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
)

// System is the machine tune reads and changes.
type System struct {
	Root string // "/" on the machine; a directory in tests
	Run  func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Look finds a program a udev rule may run as root: an absolute path the system owns.
	Look func(file string) (string, error)
	Euid func() int
	Ctl  string // the command that writes the files, named in their headers
	// Docker is the host's engine; nil leaves its daemon file out.
	Docker *Docker
	// Owned: the host is Stowage's own (a Kairos VM), whose Docker file rotates the containers'
	// logs on every profile.
	Owned bool
	// Profile is the rules' profile; "" is the one the last apply recorded, else Default.
	Profile Profile
	// Modules are the kernel modules the project declares, by name, which Inspect judges.
	Modules []Module
	// Packages are the distribution packages the project declares, by name, which Inspect judges.
	Packages []Package
	// Firmware are the firmware files the project declares, which Inspect judges.
	Firmware []Firmware
	// Interfaces are the network interfaces the project declares, by role, which Inspect judges.
	Interfaces []Interface
	// ServiceCores are the services' whole cores, which the NICs' steering stays off (isolated).
	ServiceCores []CoreRequest
	// KernelArgs are the kernel arguments the project declares, which Inspect judges against the
	// running kernel's command line.
	KernelArgs []KernelArg
	// Settings are the kernel settings the project declares (host.sysctls), which the host's
	// settings (Host.Sysctls) take after the profile's.
	Settings []Sysctl
	// Power is the project's posture (host.power): Performance holds the CPUs at their full speed;
	// "" or Platform leaves them as the host has them.
	Power string
}

// Host reads the machine (ReadHost) with the system's profile.
func (s System) Host() (Host, error) {
	h, err := ReadHost(s.Root)
	if err != nil {
		return h, err
	}
	h.Profile = s.Profile
	h.Settings = s.Settings
	h.Isolated = s.isolated(h)
	if h.Profile == "" {
		h.Profile = Default
		if st, err := s.readState(); err == nil && st.Profile != "" {
			h.Profile = st.Profile
		}
	}
	return h, nil
}

// Docker is what tune needs of the host's engine.
type Docker struct {
	Rootless bool // its daemon file is the user's, its network a user-mode one
	Desktop  bool // Docker Desktop: the containers run in its own VM, which this host's settings miss
	// UsernsRemap: the containers run in a user namespace of their own.
	UsernsRemap bool
}

func (s System) path(rel string) string { return filepath.Join(s.Root, rel) }

func (s System) read(rel string) (string, bool) {
	raw, err := os.ReadFile(s.path(rel))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(raw)), true
}

func (s System) exists(rel string) bool {
	_, err := os.Stat(s.path(rel))
	return err == nil
}

// ipv6Off reports whether key is IPv6's and the kernel runs without IPv6 (ipv6.disable=1): its
// settings are not there to set.
func (s System) ipv6Off(key string) bool {
	return strings.HasPrefix(key, "net.ipv6.") && !s.exists("proc/sys/net/ipv6")
}

// running reports whether a process of that name runs (proc/<pid>/comm).
func (s System) running(name string) bool {
	entries, err := os.ReadDir(s.path("proc"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if comm, ok := s.read(filepath.Join("proc", e.Name(), "comm")); ok && comm == name {
			return true
		}
	}
	return false
}

// steerCPUs are the online CPUs the NICs' interrupts and receive steering go to: all but the
// Isolated ones, every one when nothing else is left.
func (h Host) steerCPUs() []int {
	ids := slices.DeleteFunc(slices.Clone(h.cpuIDs()), func(id int) bool { return slices.Contains(h.Isolated, id) })
	if len(ids) == 0 {
		return h.cpuIDs()
	}
	return ids
}

// cpuIDs are the online CPUs' ids.
func (h Host) cpuIDs() []int {
	if len(h.Online) > 0 {
		return h.Online
	}
	ids := make([]int, max(h.CPUs, 1))
	for i := range ids {
		ids[i] = i
	}
	return ids
}

// irqs are a NIC's interrupts, as its PCI device lists them (msi_irqs: a virtio NIC's device is
// the PCI device's child, and only its parent's are taken), in order.
func (s System) irqs(n nic) []int {
	dev, err := filepath.EvalSymlinks(s.path(filepath.Join("sys/class/net", n.name, "device")))
	if err != nil {
		return nil
	}
	dirs := []string{dev}
	if strings.HasPrefix(filepath.Base(dev), "virtio") {
		dirs = append(dirs, filepath.Dir(dev))
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(filepath.Join(dir, "msi_irqs"))
		if err != nil {
			continue
		}
		var out []int
		for _, e := range entries {
			if irq, err := strconv.Atoi(e.Name()); err == nil {
				out = append(out, irq)
			}
		}
		slices.Sort(out)
		return out
	}
	return nil
}

// irqsStacked reports whether two or more interrupts all land on the same CPUs (their effective
// affinity, else the one asked for): a NIC whose queues are all served by one CPU.
func (s System) irqsStacked(irqs []int) bool {
	if len(irqs) < 2 {
		return false
	}
	first := ""
	for i, irq := range irqs {
		dir := filepath.Join("proc/irq", strconv.Itoa(irq))
		cpus, ok := s.read(filepath.Join(dir, "effective_affinity_list"))
		if !ok || cpus == "" {
			if cpus, ok = s.read(filepath.Join(dir, "smp_affinity_list")); !ok {
				return false
			}
		}
		if i == 0 {
			first = cpus
		} else if cpus != first {
			return false
		}
	}
	return true
}

// cpuGovernor is one CPU's frequency governor: its file, its value, whether it offers performance.
type cpuGovernor struct {
	file   string
	have   string
	offers bool
}

// governors are the governors of the CPUs whose frequency the kernel drives; none on most VMs,
// where the hypervisor does.
func (s System) governors() []cpuGovernor {
	cpus, err := filepath.Glob(s.path("sys/devices/system/cpu/cpu[0-9]*/cpufreq"))
	if err != nil {
		return nil
	}
	var out []cpuGovernor
	for _, dir := range cpus {
		rel, err := filepath.Rel(s.Root, dir)
		if err != nil {
			continue
		}
		have, ok := s.read(filepath.Join(rel, "scaling_governor"))
		if !ok {
			continue
		}
		avail, _ := s.read(filepath.Join(rel, "scaling_available_governors"))
		out = append(out, cpuGovernor{filepath.Join(rel, "scaling_governor"), have, slices.Contains(strings.Fields(avail), "performance")})
	}
	return out
}

// State is an item's verdict.
type State string

const (
	OK      State = "ok"
	Differs State = "differs"
	Advice  State = "advice" // outside the OS (the hypervisor, the cloud): never applied, never drift
	NA      State = "n/a"
)

// Item is one line of the report.
type Item struct {
	State State
	Kind  string // sysctl, module, package, firmware, interface, kernel, cpu, nic, tun, docker, nftables, state
	Name  string
	Have  string
	Want  string
}

// nic is a network interface of the host's namespace.
type nic struct {
	name     string
	physical bool // backed by a device, not a bridge, veth or tunnel
	driver   string
	queues   int // receive queues
	tun      bool
}

func (s System) nics() []nic {
	entries, err := os.ReadDir(s.path("sys/class/net"))
	if err != nil {
		return nil
	}
	var out []nic
	for _, e := range entries {
		n := nic{name: e.Name()}
		if n.name == "lo" {
			continue
		}
		dir := filepath.Join("sys/class/net", n.name)
		n.physical = s.exists(filepath.Join(dir, "device"))
		n.tun = s.exists(filepath.Join(dir, "tun_flags"))
		if link, err := os.Readlink(s.path(filepath.Join(dir, "device/driver"))); err == nil {
			n.driver = filepath.Base(link)
		}
		if q, err := os.ReadDir(s.path(filepath.Join(dir, "queues"))); err == nil {
			for _, e := range q {
				if strings.HasPrefix(e.Name(), "rx-") {
					n.queues++
				}
			}
		}
		out = append(out, n)
	}
	return out
}

// DefaultRoute is the interface of the default route, IPv4's else IPv6's; "" when there is none.
func (s System) DefaultRoute() string {
	if raw, ok := s.read("proc/net/route"); ok {
		sc := bufio.NewScanner(strings.NewReader(raw))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) >= 8 && f[1] == "00000000" && f[7] == "00000000" {
				return f[0]
			}
		}
	}
	if raw, ok := s.read("proc/net/ipv6_route"); ok {
		sc := bufio.NewScanner(strings.NewReader(raw))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) == 10 && f[0] == strings.Repeat("0", 32) && f[1] == "00" && f[9] != "lo" {
				return f[9]
			}
		}
	}
	return ""
}

// MTU is the default route's interface's MTU; 0 when unknown.
func (s System) MTU() int {
	dev := s.DefaultRoute()
	if dev == "" {
		return 0
	}
	v, _ := s.read(filepath.Join("sys/class/net", dev, "mtu"))
	n, _ := strconv.Atoi(v)
	return n
}

// ethtoolFeatures reads `ethtool -k dev`: each feature's on or off.
func (s System) ethtoolFeatures(ctx context.Context, eth, dev string) (map[string]string, error) {
	out, err := s.Run(ctx, eth, "-k", dev)
	if err != nil {
		return nil, err
	}
	f := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), ":")
		if ok {
			f[k] = strings.TrimSpace(v)
		}
	}
	return f, nil
}

// ring is a NIC's receive and transmit ring: its driver's maximum and its current size.
type ring struct{ maxRX, maxTX, rx, tx uint64 }

// ethtoolRings reads `ethtool -g dev`.
func (s System) ethtoolRings(ctx context.Context, eth, dev string) (ring, error) {
	out, err := s.Run(ctx, eth, "-g", dev)
	if err != nil {
		return ring{}, err
	}
	var r ring
	var rx, tx *uint64
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "Pre-set maximums"):
			rx, tx = &r.maxRX, &r.maxTX
		case strings.HasPrefix(line, "Current hardware settings"):
			rx, tx = &r.rx, &r.tx
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || rx == nil {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		if err != nil {
			continue
		}
		switch k {
		case "RX":
			*rx = n
		case "TX":
			*tx = n
		}
	}
	return r, nil
}

// ringWant is the ring size an apply gives dev; 0 leaves it. It never shrinks a ring.
func (h Host) ringWant(n nic, have, driverMax uint64) uint64 {
	if w := h.RingEntries(n.queues, driverMax); w > have {
		return w
	}
	return 0
}

// emulated are the drivers of NICs a hypervisor emulates, the slowest it offers.
var emulated = []string{"e1000", "e1000e", "8139cp", "8139too", "pcnet32", "ne2k-pci", "tulip"}

// advice judges a physical NIC on what the hypervisor or the cloud decides.
func (s System) advice(n nic, all []nic, cpus int) string {
	vendor, _ := s.read("sys/class/dmi/id/sys_vendor")
	switch {
	case slices.Contains(emulated, n.driver):
		return "an emulated NIC: give the VM a paravirtual one (virtio, vmxnet3)"
	case n.driver == "virtio_net" && strings.HasPrefix(vendor, "Google"):
		return "gVNIC is Compute Engine's faster NIC: the instance's nic_type GVNIC, on an image that declares it"
	case n.driver == "virtio_net" && n.queues < cpus:
		return fmt.Sprintf("%d receive queue(s) for %d vCPUs: one queue per vCPU (multiqueue virtio) is the hypervisor's setting", n.queues, cpus)
	case n.driver == "hv_netvsc" && !slices.ContainsFunc(all, func(o nic) bool {
		return o.physical && slices.Contains([]string{"mlx4_core", "mlx5_core", "mana"}, o.driver)
	}):
		return "no accelerated networking: the VM's NIC setting on Azure"
	case n.driver == "vif" || n.driver == "xen_netfront":
		return "a Xen NIC: an instance type with ENA"
	}
	return ""
}

// rule renders how a value is judged.
func (r Rule) render(want string) string {
	switch r {
	case AtLeast:
		return "≥ " + want
	case AtMost:
		return "≤ " + want
	case NotStrict:
		return "not 1 (" + want + ")"
	}
	return want
}

func verdict(ok bool) State {
	if ok {
		return OK
	}
	return Differs
}

// Inspect reports how the host differs from what h's rules want.
func (s System) Inspect(ctx context.Context, h Host) []Item {
	if s.Docker != nil && s.Docker.Desktop {
		return []Item{{State: NA, Kind: "docker", Name: "Docker Desktop", Have: "its own VM", Want: "the VM's settings: this host's do not reach the containers"}}
	}
	var items []Item
	// A value an earlier apply wrote is judged against what the host held before it: a host that
	// shrank is not kept at a larger host's ceilings.
	st, err := s.readState()
	if err != nil {
		items = append(items, Item{Differs, "state", StateFile, "unreadable: " + err.Error(), "what --apply records, which --revert reads"})
		st = &state{}
	}
	for _, c := range h.Sysctls() {
		have, ok := s.read(c.Path())
		if !ok && s.ipv6Off(c.Key) {
			items = append(items, Item{NA, "sysctl", c.Key, "IPv6 is off in this kernel", ""})
			continue
		}
		have = strings.Join(strings.Fields(have), " ")
		base, _ := st.base(c, have, ok)
		if !ok {
			have = "absent"
		}
		items = append(items, Item{verdict(ok && c.Satisfied(have) && have == c.Value(base)), "sysctl", c.Key, have, c.Rule.render(c.Value(base))})
	}
	tun := s.exists("sys/module/tun") || s.exists("dev/net/tun")
	items = append(items, Item{verdict(tun), "module", "tun", map[bool]string{true: "present", false: "absent"}[tun], "present"})
	all := s.nics()
	eth, lookErr := s.Look("ethtool")
	for _, n := range all {
		if !n.physical {
			continue
		}
		// What --apply cannot change is advice or n/a, never drift.
		const gro = "rx-udp-gro-forwarding"
		if lookErr != nil {
			items = append(items, Item{Advice, "nic", n.name + " " + gro, "ethtool is not installed: install it, then --apply turns the offload on", ""})
		} else if f, err := s.ethtoolFeatures(ctx, eth, n.name); err != nil {
			items = append(items, Item{Advice, "nic", n.name + " " + gro, "ethtool -k: " + err.Error(), ""})
		} else if v, ok := f[gro]; !ok || strings.Contains(v, "[fixed]") {
			items = append(items, Item{NA, "nic", n.name + " " + gro, cmp.Or(v, "absent") + ": the kernel or the driver decides it", ""})
		} else {
			items = append(items, Item{verdict(v == "on"), "nic", n.name + " " + gro, v, "on"})
		}
		if h.network() && lookErr == nil {
			items = append(items, s.inspectRing(ctx, h, eth, n))
		}
		if q := h.QueueFlows(); q > 0 {
			for i := range n.queues {
				flows, _ := s.read(filepath.Join("sys/class/net", n.name, "queues", fmt.Sprintf("rx-%d", i), "rps_flow_cnt"))
				items = append(items, Item{verdict(atLeast(flows, q)), "nic", fmt.Sprintf("%s rx-%d rps_flow_cnt", n.name, i), flows, AtLeast.render(nums(q))})
			}
		}
		if h.network() {
			items = append(items, s.inspectSteering(h, n)...)
		}
		if a := s.advice(n, all, h.CPUs); a != "" {
			items = append(items, Item{Advice, "nic", n.name + " " + cmp.Or(n.driver, "driver"), a, ""})
		}
	}
	items = append(items, s.inspectPower()...)
	items = append(items, s.inspectKernelArgs(h)...)
	for _, n := range all {
		if !n.tun || !h.network() {
			continue
		}
		dir := filepath.Join("sys/class/net", n.name)
		qlen, _ := s.read(filepath.Join(dir, "tx_queue_len"))
		mask, _ := s.read(filepath.Join(dir, "queues/rx-0/rps_cpus"))
		flows, _ := s.read(filepath.Join(dir, "queues/rx-0/rps_flow_cnt"))
		items = append(items,
			Item{verdict(atLeast(qlen, h.TunQueueLen())), "tun", n.name + " tx_queue_len", qlen, AtLeast.render(nums(h.TunQueueLen()))},
			Item{verdict(sameMask(mask, h.RPSMask())), "tun", n.name + " rps_cpus", mask, h.RPSMask()},
			Item{verdict(atLeast(flows, h.FlowEntries())), "tun", n.name + " rps_flow_cnt", flows, AtLeast.render(nums(h.FlowEntries()))})
	}
	items = append(items, s.inspectModules()...)
	items = append(items, s.inspectPackages()...)
	items = append(items, s.inspectFirmware()...)
	items = append(items, s.inspectInterfaces(ctx)...)
	if s.Docker == nil {
		return items
	}
	if s.Docker.Rootless {
		return append(items, Item{NA, "docker", "daemon.json", "rootless Docker", "its own daemon file and user-mode network"})
	}
	daemon := s.daemonFile()
	raw, err := os.ReadFile(s.path(daemon))
	if err != nil && !os.IsNotExist(err) {
		return append(items, Item{Differs, "docker", daemon, "unreadable: " + err.Error(), ""})
	}
	args := s.dockerdArgs()
	for _, d := range s.daemonWants(h) {
		have, ok, err := daemonValue(raw, d.path)
		why := leave(d, raw, args)
		switch {
		case d.flag != "" && args[d.flag] != "":
			items = append(items, Item{NA, "docker", strings.Join(d.path, "."), "dockerd runs with " + d.flag + ", which the file may not repeat", ""})
		case err != nil:
			items = append(items, Item{Differs, "docker", strings.Join(d.path, "."), "unreadable: " + err.Error(), d.show})
		case !(ok && d.ok(have)) && why != "":
			items = append(items, Item{NA, "docker", strings.Join(d.path, "."), why, ""})
		default:
			items = append(items, Item{verdict(ok && d.ok(have)), "docker", strings.Join(d.path, "."), cmp.Or(string(have), "absent"), d.show})
		}
	}
	return items
}

// inspectSteering judges how a NIC's received packets spread over the CPUs: its interrupts, and
// RPS on a NIC with fewer queues than CPUs.
func (s System) inspectSteering(h Host, n nic) []Item {
	var items []Item
	switch irqs := s.irqs(n); {
	case len(irqs) < 2 || len(h.cpuIDs()) < 2:
	case s.running("irqbalance") && len(h.Isolated) > 0 && s.irqbalanceBans(h.Isolated):
		items = append(items, Item{OK, "nic", n.name + " interrupts", "irqbalance spreads them, off the services' cores", "IRQBALANCE_BANNED_CPULIST=" + cpuRanges(h.Isolated)})
	case s.running("irqbalance") && len(h.Isolated) > 0:
		items = append(items, Item{Advice, "nic", n.name + " interrupts", "irqbalance spreads them, the services' cores included:", "IRQBALANCE_BANNED_CPULIST=" + cpuRanges(h.Isolated) + " in /" + s.irqbalanceFile() + ", then restart irqbalance"})
	case s.running("irqbalance"):
		items = append(items, Item{NA, "nic", n.name + " interrupts", "irqbalance spreads them", ""})
	case s.irqsStacked(irqs):
		// Advice, never drift: a driver whose interrupts the kernel manages refuses the move.
		items = append(items, Item{Advice, "nic", n.name + " interrupts", fmt.Sprintf("%d, all on the same CPUs: --apply (the VM's boot) spreads them, unless the kernel manages them", len(irqs)), ""})
	default:
		items = append(items, Item{OK, "nic", n.name + " interrupts", fmt.Sprintf("%d, spread", len(irqs)), "spread over the CPUs"})
	}
	if h.steers(n) {
		for i := range n.queues {
			mask, _ := s.read(filepath.Join("sys/class/net", n.name, "queues", fmt.Sprintf("rx-%d", i), "rps_cpus"))
			items = append(items, Item{verdict(sameMask(mask, h.RPSMask())), "nic", fmt.Sprintf("%s rx-%d rps_cpus", n.name, i), mask, h.RPSMask()})
		}
	}
	return items
}

// inspectGovernor judges the CPUs' frequency governors.
func (s System) inspectGovernor() Item {
	gs := s.governors()
	if len(gs) == 0 {
		return Item{NA, "cpu", "governor", "no cpufreq: the hypervisor or the firmware drives the CPUs' frequency", ""}
	}
	var have []string
	ok := true
	for _, g := range gs {
		if !slices.Contains(have, g.have) {
			have = append(have, g.have)
		}
		ok = ok && (g.have == "performance" || !g.offers)
	}
	return Item{verdict(ok), "cpu", "governor", strings.Join(have, ","), "performance"}
}

// inspectRing judges a NIC's rings against what the network profile gives it.
func (s System) inspectRing(ctx context.Context, h Host, eth string, n nic) Item {
	name := n.name + " rings"
	r, err := s.ethtoolRings(ctx, eth, n.name)
	if err != nil {
		return Item{NA, "nic", name, "ethtool -g: " + err.Error(), ""}
	}
	rx, tx := h.RingEntries(n.queues, r.maxRX), h.RingEntries(n.queues, r.maxTX)
	if rx == 0 && tx == 0 {
		return Item{NA, "nic", name, fmt.Sprintf("rx %d tx %d: the driver offers no larger ring", r.rx, r.tx), ""}
	}
	have, want := fmt.Sprintf("rx %d tx %d", r.rx, r.tx), fmt.Sprintf("rx ≥ %d tx ≥ %d", rx, tx)
	if r.rx >= rx && r.tx >= tx {
		return Item{OK, "nic", name, have, want}
	}
	// A resize resets the link: the Stowage VM grows a ring at its boot, before its network; on a
	// running host it is the admin's to do. Some drivers also refuse a ring they list: never drift.
	return Item{Advice, "nic", name, fmt.Sprintf("%s: a resize resets the link, so it grows at a boot before the network, or by `ethtool -G %s rx %d tx %d` while the link is down", have, n.name, max(rx, r.rx), max(tx, r.tx)), want}
}

// leave is why d is the admin's to keep; "" when it is not.
func leave(d daemonWant, raw []byte, args map[string]string) string {
	if d.leave == nil {
		return ""
	}
	return d.leave(raw, args)
}

func atLeast(cur string, want uint64) bool {
	n, err := strconv.ParseUint(strings.TrimSpace(cur), 10, 64)
	return err == nil && n >= want
}

// sameMask compares two rps_cpus masks as numbers: the kernel pads its groups to the CPU count.
func sameMask(a, b string) bool {
	norm := func(m string) string {
		return strings.TrimLeft(strings.ReplaceAll(strings.TrimSpace(m), ",", ""), "0")
	}
	return a != "" && norm(a) == norm(b)
}

// Drift is how many items differ.
func Drift(items []Item) int {
	n := 0
	for _, it := range items {
		if it.State == Differs {
			n++
		}
	}
	return n
}

// Print writes the report.
func Print(w io.Writer, items []Item) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, it := range items {
		if it.State == Advice || it.State == NA {
			fmt.Fprintf(tw, "%s\t%s %s\t%s\n", it.State, it.Kind, it.Name, strings.TrimSpace(it.Have+" "+it.Want))
			continue
		}
		fmt.Fprintf(tw, "%s\t%s %s\t%s → %s\n", it.State, it.Kind, it.Name, it.Have, it.Want)
	}
	return tw.Flush()
}

// daemonValue is the JSON at path in the daemon file raw (canonical: keys sorted), and whether
// it is there.
func daemonValue(raw []byte, path []string) (json.RawMessage, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false, nil
	}
	var doc any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&doc); err != nil {
		return nil, false, err
	}
	for _, k := range path {
		m, ok := doc.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		if doc, ok = m[k]; !ok {
			return nil, false, nil
		}
	}
	out, err := json.Marshal(doc)
	return out, true, err
}

// Container is what each container is given so that it runs as it would on the host.
type Container struct {
	// Sysctls are its network namespace's own settings, by key.
	Sysctls map[string]string
	// MTU is the host's default route's MTU, its networks' (1500 included); 0 when it does not
	// read or the engine's networks take none (rootless, Docker Desktop).
	MTU int
	// MTUUnread: the engine's networks take the host's MTU, but it did not read this moment (no
	// default route): they keep the one they have.
	MTUUnread bool
	// Nofile is the open files it may hold (its hard limit); 0 leaves the engine's. Its soft
	// limit is NofileSoft at most.
	Nofile uint64
}

// NofileSoft is the most open files a container's soft limit is given: a program that needs more
// raises it to the hard limit itself, and one that walks every descriptor up to it stays fast.
const NofileSoft = 65536

// Container is h's container settings on this host. Sysctls are containerKeys that the host's
// kernel has, a container's namespace having the same keys from Linux 4.15 on (one it lacked
// would fail the container's start): the host's value, or on the network profile the profile's
// where the host holds less, field by field. None on an older kernel or on an engine whose
// containers are in a user namespace of their own (rootless, userns-remap: their namespace hides
// keys). Nofile is fs.nr_open, at most NofileLimit and the hard limit dockerd and containerd run
// with (a container's limit above theirs fails its start where they may not raise it, as in an
// unprivileged LXC container); none when neither is seen, or under a user namespace, where the
// engine's own limit bounds it. MTU is not on rootless Docker, whose network is a user-mode one.
// Nothing in Docker Desktop's VM, or when the engine is not known.
func (s System) Container(h Host) Container {
	d := s.Docker
	if d == nil || d.Desktop || d.Rootless {
		return Container{}
	}
	c := Container{MTU: s.MTU()}
	c.MTUUnread = c.MTU == 0
	if d.UsernsRemap {
		return c
	}
	if v, ok := s.read("proc/sys/fs/nr_open"); ok {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			if engine, seen := s.engineNofile(); seen {
				c.Nofile = min(n, NofileLimit, engine)
			}
		}
	}
	if !s.kernelAtLeast(4, 15) {
		return c
	}
	want := map[string]Sysctl{}
	for _, r := range h.Sysctls() {
		want[r.Key] = r
	}
	c.Sysctls = map[string]string{}
	for _, k := range containerKeys {
		cur, ok := s.read(Sysctl{Key: k}.Path())
		if !ok {
			continue
		}
		cur = strings.Join(strings.Fields(cur), " ")
		if r, tuned := want[k]; tuned {
			cur = r.Value(cur)
		}
		c.Sysctls[k] = cur
	}
	return c
}

// engineNofile is the lowest hard limit of open files the host's own root dockerd and containerd
// run with (proc/<pid>/limits); an engine in a container of its own (kind, dind) is not the
// host's: its NSpid names more than one pid. False when neither is seen.
func (s System) engineNofile() (uint64, bool) {
	entries, err := os.ReadDir(s.path("proc"))
	if err != nil {
		return 0, false
	}
	low, seen := uint64(math.MaxUint64), false
	for _, e := range entries {
		comm, ok := s.read(filepath.Join("proc", e.Name(), "comm"))
		if !ok || comm != "dockerd" && comm != "containerd" {
			continue
		}
		if status, _ := s.read(filepath.Join("proc", e.Name(), "status")); !rootUID(status) || !hostPID(status) {
			continue
		}
		limits, _ := s.read(filepath.Join("proc", e.Name(), "limits"))
		for line := range strings.SplitSeq(limits, "\n") {
			f := strings.Fields(strings.TrimPrefix(line, "Max open files"))
			if !strings.HasPrefix(line, "Max open files") || len(f) < 2 {
				continue
			}
			hard := uint64(math.MaxUint64)
			if f[1] != "unlimited" {
				n, err := strconv.ParseUint(f[1], 10, 64)
				if err != nil {
					continue
				}
				hard = n
			}
			low, seen = min(low, hard), true
		}
	}
	return low, seen
}

// SystemLook finds file in the system's own program directories, never in PATH.
func SystemLook(file string) (string, error) {
	for _, dir := range []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"} {
		p := filepath.Join(dir, file)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: not installed", file)
}

// kernelAtLeast reports whether the running kernel (proc/sys/kernel/osrelease) is major.minor or
// later; false when it does not read.
func (s System) kernelAtLeast(major, minor int) bool {
	rel, ok := s.read("proc/sys/kernel/osrelease")
	if !ok {
		return false
	}
	lead := func(v string) string {
		if i := strings.IndexFunc(v, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
			return v[:i]
		}
		return v
	}
	maj, rest, _ := strings.Cut(rel, ".")
	a, errA := strconv.Atoi(maj)
	b, errB := strconv.Atoi(lead(rest))
	return errA == nil && errB == nil && (a > major || a == major && b >= minor)
}

// dockerdArgs are the flags the system's dockerd was started with (proc/<pid>/cmdline of the
// process named dockerd that runs as root, not a user's rootless one), each with its value ("set"
// for one without); none when it is not found.
func (s System) dockerdArgs() map[string]string {
	entries, err := os.ReadDir(s.path("proc"))
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if comm, ok := s.read(filepath.Join("proc", e.Name(), "comm")); !ok || comm != "dockerd" {
			continue
		}
		if status, _ := s.read(filepath.Join("proc", e.Name(), "status")); !rootUID(status) {
			continue
		}
		raw, err := os.ReadFile(s.path(filepath.Join("proc", e.Name(), "cmdline")))
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		out := map[string]string{}
		for i, a := range args {
			f, v, eq := strings.Cut(a, "=")
			if !strings.HasPrefix(f, "--") {
				continue
			}
			if !eq {
				v = "set"
				if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					v = args[i+1]
				}
			}
			out[f] = v
		}
		return out
	}
	return nil
}

// hostPID reports whether a proc/<pid>/status reads as a process of the host's own pid namespace:
// its NSpid names one pid (a kernel without the line, before 4.1, is taken as the host's).
func hostPID(status string) bool {
	for line := range strings.SplitSeq(status, "\n") {
		if f := strings.Fields(line); len(f) >= 1 && f[0] == "NSpid:" {
			return len(f) == 2
		}
	}
	return true
}

// rootUID reports whether a proc/<pid>/status reads as a process whose real uid is root.
func rootUID(status string) bool {
	for line := range strings.SplitSeq(status, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "Uid:" {
			return f[1] == "0"
		}
	}
	return false
}

// daemonFile is the daemon's file under the root: the running dockerd's --config-file (a relative
// one from /, its working directory as a service), else DaemonFile.
func (s System) daemonFile() string {
	if f := s.dockerdArgs()["--config-file"]; f != "" && f != "set" {
		return strings.TrimPrefix(filepath.Join("/", f), "/")
	}
	return DaemonFile
}

// cpuRanges is a list of CPUs as the kernel's lists read it: ranges of consecutive ids, comma
// separated (0-3,8).
func cpuRanges(ids []int) string {
	var parts []string
	for i := 0; i < len(ids); {
		j := i
		for j+1 < len(ids) && ids[j+1] == ids[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, fmt.Sprintf("%d-%d", ids[i], ids[j]))
		} else {
			parts = append(parts, strconv.Itoa(ids[i]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// irqbalanceFile is irqbalance's environment file on this host: Debian's /etc/default, else the
// RHEL family's /etc/sysconfig (the one its unit reads), relative to the root.
func (s System) irqbalanceFile() string {
	if !s.exists("etc/default/irqbalance") && s.exists("etc/sysconfig/irqbalance") {
		return "etc/sysconfig/irqbalance"
	}
	return "etc/default/irqbalance"
}

// irqbalanceEnv is the running irqbalance's environment (proc/<pid>/environ), else what its
// environment file sets (systemd's EnvironmentFile: a line starting with # is a comment, the rest
// KEY=value, quoted or not, exported or not, the last one winning).
func (s System) irqbalanceEnv() map[string]string {
	vals := map[string]string{}
	entries, _ := os.ReadDir(s.path("proc"))
	for _, e := range entries {
		if comm, ok := s.read(filepath.Join("proc", e.Name(), "comm")); !ok || comm != "irqbalance" {
			continue
		}
		raw, err := os.ReadFile(s.path(filepath.Join("proc", e.Name(), "environ")))
		if err != nil {
			break
		}
		for _, kv := range strings.Split(string(raw), "\x00") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				vals[k] = v
			}
		}
		return vals
	}
	raw, _ := s.read(s.irqbalanceFile())
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "="); ok {
			vals[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return vals
}

// irqbalanceBans reports whether irqbalance keeps off every CPU of cpus, as irqbalance reads its
// environment: IRQBALANCE_BANNED_CPUS (a mask) first, else IRQBALANCE_BANNED_CPULIST (a CPU list),
// else the CPUs the kernel isolates (isolcpus) or runs without ticks (nohz_full), which it then
// bans itself.
func (s System) irqbalanceBans(cpus []int) bool {
	env := s.irqbalanceEnv()
	var banned []int
	switch {
	case env["IRQBALANCE_BANNED_CPUS"] != "":
		banned = maskCPUs(env["IRQBALANCE_BANNED_CPUS"])
	case env["IRQBALANCE_BANNED_CPULIST"] != "":
		banned, _ = cpuList(env["IRQBALANCE_BANNED_CPULIST"])
	default:
		for _, f := range []string{"sys/devices/system/cpu/isolated", "sys/devices/system/cpu/nohz_full"} {
			if v, ok := s.read(f); ok && v != "" && v != "(null)" {
				ids, _ := cpuList(v)
				banned = append(banned, ids...)
			}
		}
	}
	return !slices.ContainsFunc(cpus, func(c int) bool { return !slices.Contains(banned, c) })
}

// maskCPUs are the CPUs of a hexadecimal mask as the kernel writes one (32-bit groups, comma
// separated, the highest first); none for one that does not read.
func maskCPUs(mask string) []int {
	groups := strings.Split(mask, ",")
	var out []int
	for i := range groups {
		g, err := strconv.ParseUint(groups[len(groups)-1-i], 16, 32)
		if err != nil {
			return nil
		}
		for b := range 32 {
			if g&(1<<b) != 0 {
				out = append(out, i*32+b)
			}
		}
	}
	return out
}
