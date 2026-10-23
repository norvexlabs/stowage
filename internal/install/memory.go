package install

import (
	"strconv"

	"github.com/norvexlabs/stowage/internal/tune"
)

// The variables stack.memory gives each service it sizes, in its container's environment: a
// service sizes its own caches from them.
const (
	MemLimitVar = "STOWAGE_MEM_LIMIT_MB" // the container's memory limit, in MiB
	HostMemVar  = "STOWAGE_HOST_MEM_MB"  // the host's RAM, in MiB
	HostCPUsVar = "STOWAGE_HOST_CPUS"    // the host's online CPUs
)

// MinMemLimit is the least limit a share gives: a share of a small host must still start its
// service.
const MinMemLimit = 64 << 20

// memoryPlan is stack.memory on a host: no share without one.
type memoryPlan struct {
	Shares map[string]int
	Host   tune.Host
}

// memoryPlan is the release's stack.memory on host h, read at each start, so a host given more
// RAM gives the services more. None on Docker Desktop: its containers run in a VM of its own,
// whose RAM is not this host's.
func (in *Installer) memoryPlan(sys tune.System, h tune.Host) memoryPlan {
	m := in.R.Spec.Stack.Memory
	if m == nil || len(m.Shares) == 0 || sys.Docker != nil && sys.Docker.Desktop {
		return memoryPlan{}
	}
	return memoryPlan{Shares: m.Shares, Host: h}
}

// limit is a container's part of pct of the host's RAM, split among n containers, in whole MiB,
// from MinMemLimit, and never below floor: the limit the compose file sets itself, or the memory
// it reserves (Docker refuses a limit under the reservation).
func (p memoryPlan) limit(pct, n int, floor int64) int64 {
	share := int64(p.Host.MemBytes*uint64(pct)/100/uint64(max(n, 1))) &^ (1<<20 - 1)
	return max(share, MinMemLimit, floor)
}

// give adds to a service's content entry the memory limit pct gives each of its containers, on
// the key its compose file sets the limit with (mem_limit when none, both when both: Compose
// refuses them distinct), its swap limit moved by as much, and the variables it sizes itself
// from. The share is the service's: its containers (scale, deploy.replicas) split it.
func (p memoryPlan) give(extra map[string]any, pct int, svc serviceMemory) {
	memLimit, deploy := int64(svc.MemLimit), int64(svc.Deploy.Resources.Limits.Memory)
	own := max(memLimit, deploy)
	reserved := max(int64(svc.MemReservation), int64(svc.Deploy.Resources.Reservations.Memory))
	limit := p.limit(pct, max(svc.Scale, svc.Deploy.Replicas), max(own, reserved))
	if limit > own {
		if deploy > 0 {
			extra["deploy"] = map[string]any{"resources": map[string]any{"limits": map[string]any{"memory": limit}}}
		}
		if memLimit > 0 || deploy == 0 {
			extra["mem_limit"] = limit
		}
		if swap := int64(svc.MemswapLimit); swap > 0 {
			extra["memswap_limit"] = swap + limit - own
		}
	}
	extra["environment"] = map[string]string{
		MemLimitVar: strconv.FormatInt(limit>>20, 10),
		HostMemVar:  strconv.FormatUint(p.Host.MemBytes>>20, 10),
		HostCPUsVar: strconv.Itoa(p.Host.CPUs),
	}
}
