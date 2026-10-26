package tune

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Cores are host h's online CPUs by physical core, a core's SMT threads together, ordered by
// their first CPU: read from sysfs (core_id within physical_package_id); a CPU whose topology does
// not read is a core of its own.
func (s System) Cores(h Host) [][]int {
	type key struct{ pkg, core string }
	byKey := map[key][]int{}
	var order []key
	for _, cpu := range h.cpuIDs() {
		dir := "sys/devices/system/cpu/cpu" + strconv.Itoa(cpu) + "/topology/"
		pkg, ok1 := s.read(dir + "physical_package_id")
		core, ok2 := s.read(dir + "core_id")
		k := key{pkg, core}
		if !ok1 || !ok2 {
			k = key{"cpu", strconv.Itoa(cpu)}
		}
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], cpu)
	}
	out := make([][]int, 0, len(order))
	for _, k := range order {
		c := byKey[k]
		slices.Sort(c)
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b []int) int { return a[0] - b[0] })
	return out
}

// CoreRequest is a service's whole cores (services.<name>.cpus): their count, and whether the
// NICs' interrupts and receive steering may go on them (IRQ: on).
type CoreRequest struct {
	Service string
	Cores   int
	IRQ     bool
}

// Allocate gives each request its cores of cores (Cores'), a core's threads together: from the
// last core down, the requests in their services' order, never the core of CPU 0 (the host's:
// its timers, the interrupts by default, the stack's other services). The same host gives the
// same sets at every start and every boot. Fewer cores than asked, beside CPU 0's, is an error.
func Allocate(reqs []CoreRequest, cores [][]int) (map[string][]int, error) {
	reqs = slices.Clone(reqs)
	slices.SortFunc(reqs, func(a, b CoreRequest) int { return strings.Compare(a.Service, b.Service) })
	asked := 0
	for _, r := range reqs {
		asked += r.Cores
	}
	if asked == 0 {
		return nil, nil
	}
	free := slices.DeleteFunc(slices.Clone(cores), func(c []int) bool { return slices.Contains(c, 0) })
	if asked > len(free) {
		var names []string
		for _, r := range reqs {
			names = append(names, r.Service)
		}
		return nil, fmt.Errorf("services.*.cpus: %s ask %d whole cores, and this host has %d beside the one of CPU 0, which stays the host's", strings.Join(names, ", "), asked, len(free))
	}
	out := map[string][]int{}
	next := len(free)
	for _, r := range reqs {
		var ids []int
		for _, c := range free[next-r.Cores : next] {
			ids = append(ids, c...)
		}
		next -= r.Cores
		slices.Sort(ids)
		out[r.Service] = ids
	}
	return out, nil
}

// isolated are the CPUs the NICs' interrupts and receive steering stay off: the cores the
// services were given that keep them off (IRQ off, the default). None when the requests do not
// fit: the start refuses them, and the steering is as without them.
func (s System) isolated(h Host) []int {
	sets, err := Allocate(s.ServiceCores, s.Cores(h))
	if err != nil {
		return nil
	}
	var out []int
	for _, r := range s.ServiceCores {
		if !r.IRQ {
			out = append(out, sets[r.Service]...)
		}
	}
	slices.Sort(out)
	return out
}
