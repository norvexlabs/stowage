package tune

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	// Profile is the rules' profile; "" is the one the last apply recorded, else Shared.
	Profile Profile
}

// Host reads the machine (ReadHost) with the system's profile.
func (s System) Host() (Host, error) {
	h, err := ReadHost(s.Root)
	if err != nil {
		return h, err
	}
	h.Profile = s.Profile
	if h.Profile == "" {
		h.Profile = Shared
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
	// NetworkOpts: the engine reads default-network-opts (Docker 24 or later).
	NetworkOpts bool
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
	Kind  string // sysctl, module, nic, tun, docker
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
		if a := s.advice(n, all, h.CPUs); a != "" {
			items = append(items, Item{Advice, "nic", n.name + " " + cmp.Or(n.driver, "driver"), a, ""})
		}
	}
	for _, n := range all {
		if !n.tun {
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

// Container is h's container settings that the host's kernel has, a container's namespace having
// the same keys from Linux 4.15 on: one it lacked would fail the container's start. A ceiling the
// host holds higher is kept, field by field, as a namespace copies the host's buffer sizes and a
// kernel may size them above ours. None on an
// older kernel, on an engine whose containers are in a user namespace of their own (rootless,
// userns-remap: their namespace hides keys) or in Docker Desktop's VM, or when the engine is not
// known.
func (s System) Container(h Host) map[string]string {
	if d := s.Docker; d == nil || d.Rootless || d.UsernsRemap || d.Desktop || !s.kernelAtLeast(4, 15) {
		return nil
	}
	out := h.Container()
	for _, c := range h.Sysctls() {
		if _, ok := out[c.Key]; !ok {
			continue
		}
		if cur, ok := s.read(c.Path()); !ok {
			delete(out, c.Key)
		} else {
			out[c.Key] = c.Value(strings.Join(strings.Fields(cur), " "))
		}
	}
	return out
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
