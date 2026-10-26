package tune

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// KernelArg is one of host.kernel_args: a parameter of the kernel's command line, its value (""
// for a flag), and why.
type KernelArg struct {
	Key, Value, Reason string
}

// String is the argument as the command line holds it.
func (a KernelArg) String() string {
	if a.Value == "" {
		return a.Key
	}
	return a.Key + "=" + a.Value
}

// The postures of host.power: Performance holds the CPUs at their full speed; Platform (the
// default) leaves the CPUs' frequency to the platform, as the host has it.
const (
	Performance = "performance"
	Platform    = "platform"
)

// powerDaemons each re-assert a power profile of their own over another writer's.
var powerDaemons = []string{"power-profiles-daemon", "tuned"}

// cmdline is the running kernel's command line, each parameter's last value ("" for a flag); false
// when it does not read.
func (s System) cmdline() (map[string]string, bool) {
	raw, ok := s.read("proc/cmdline")
	if !ok {
		return nil, false
	}
	out := map[string]string{}
	for f := range strings.FieldsSeq(raw) {
		k, v, _ := strings.Cut(f, "=")
		out[k] = v
	}
	return out, true
}

// inspectKernelArgs judges each kernel argument the project declares against the running
// kernel's command line. On a host Stowage owns (the VM, whose image's boot loader gives them) one
// missing is drift; on another, advice: its boot loader is its operator's. On the network profile
// an IOMMU that translates every device's DMA is advice too, unless the project declares how it
// wants it.
func (s System) inspectKernelArgs(h Host) []Item {
	cmd, ok := s.cmdline()
	var items []Item
	for _, a := range s.KernelArgs {
		why := " (" + a.Reason + ")"
		v, given := cmd[a.Key]
		have := "absent"
		if given {
			have = KernelArg{a.Key, v, ""}.String()
		}
		switch {
		case !ok:
			items = append(items, Item{NA, "kernel", a.String(), "/proc/cmdline does not read", ""})
		case given && v == a.Value:
			items = append(items, Item{OK, "kernel", a.String(), have, a.String() + why})
		case s.Owned:
			items = append(items, Item{Differs, "kernel", a.String(), have, a.String() + why})
		default:
			items = append(items, Item{Advice, "kernel", a.String(), fmt.Sprintf("%s, the project asks for %s%s:", have, a.String(), why), "the boot loader's command line (GRUB_CMDLINE_LINUX in /etc/default/grub, then update-grub), from the next boot"})
		}
	}
	if !h.network() || !ok || !s.iommuTranslates() {
		return items
	}
	pt := KernelArg{Key: "iommu", Value: "pt"}
	if runtime.GOARCH == "arm64" {
		pt = KernelArg{Key: "iommu.passthrough", Value: "1"}
	}
	if _, given := cmd[pt.Key]; given || slices.ContainsFunc(s.KernelArgs, func(a KernelArg) bool { return a.Key == pt.Key }) {
		return items
	}
	return append(items, Item{Advice, "kernel", pt.String(), "the IOMMU translates every device's DMA, several percent of CPU under a throughput load:", "host.kernel_args " + pt.String() + " passes it through (a device the host owns is then not confined)"})
}

// iommuTranslates reports whether an IOMMU group translates its devices' DMA (type DMA or DMA-FQ)
// rather than passing it through (identity).
func (s System) iommuTranslates() bool {
	groups, _ := filepath.Glob(s.path("sys/kernel/iommu_groups/*/type"))
	for _, g := range groups {
		raw, err := os.ReadFile(g)
		if err == nil && strings.HasPrefix(strings.TrimSpace(string(raw)), "DMA") {
			return true
		}
	}
	return false
}

// epps are the energy-performance preferences of the CPUs whose driver exposes one, as governors
// gives them: each one's file, its value, whether it offers performance.
func (s System) epps() []cpuGovernor {
	var out []cpuGovernor
	for _, g := range s.governors() {
		dir := filepath.Dir(g.file)
		have, ok := s.read(filepath.Join(dir, "energy_performance_preference"))
		if !ok {
			continue
		}
		avail, _ := s.read(filepath.Join(dir, "energy_performance_available_preferences"))
		out = append(out, cpuGovernor{filepath.Join(dir, "energy_performance_preference"), have, slices.Contains(strings.Fields(avail), "performance")})
	}
	return out
}

// minPerf is intel_pstate's floor, a percent of the CPUs' top performance, which performance holds
// at 100.
const minPerf = "sys/devices/system/cpu/intel_pstate/min_perf_pct"

// inspectPower judges host.power's performance posture: every CPU on the performance governor,
// its preference performance where exposed, intel_pstate's floor at its ceiling, and no daemon
// that re-asserts a profile of its own. Under platform, nothing.
func (s System) inspectPower() []Item {
	if s.Power != Performance {
		return nil
	}
	items := []Item{s.inspectGovernor()}
	if es := s.epps(); len(es) > 0 {
		var have []string
		ok := true
		for _, e := range es {
			if !slices.Contains(have, e.have) {
				have = append(have, e.have)
			}
			ok = ok && (e.have == Performance || !e.offers)
		}
		items = append(items, Item{verdict(ok), "cpu", "energy_performance_preference", strings.Join(have, ","), Performance})
	}
	if v, ok := s.read(minPerf); ok {
		items = append(items, Item{verdict(v == "100"), "cpu", "intel_pstate min_perf_pct", v, "100"})
	}
	for _, d := range powerDaemons {
		if s.running(d) {
			items = append(items, Item{Advice, "cpu", d, "runs, and re-asserts its own power profile:", "systemctl disable --now " + d})
		}
	}
	return items
}

// power holds the CPUs at their full speed under host.power performance: the governor, then the
// preference (which intel_pstate refuses to change under the performance governor, which sets it)
// and its floor.
func (s System) power() []string {
	if s.Power != Performance {
		return nil
	}
	notes := s.governor()
	for _, e := range s.epps() {
		if e.have == Performance || !e.offers {
			continue
		}
		if v, _ := s.read(e.file); v == Performance {
			continue
		}
		if err := os.WriteFile(s.path(e.file), []byte(Performance+"\n"), 0o644); err != nil {
			notes = append(notes, fmt.Sprintf("%s: %v", e.file, err))
		}
	}
	if v, ok := s.read(minPerf); ok && v != "100" {
		if err := os.WriteFile(s.path(minPerf), []byte("100\n"), 0o644); err != nil {
			notes = append(notes, fmt.Sprintf("%s: %v", minPerf, err))
		}
	}
	for _, d := range powerDaemons {
		if s.running(d) {
			notes = append(notes, fmt.Sprintf("%s runs and re-asserts its own power profile: `systemctl disable --now %s`", d, d))
		}
	}
	return notes
}
