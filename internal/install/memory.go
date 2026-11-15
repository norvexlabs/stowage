package install

import (
	"fmt"
	"strconv"

	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/tune"
)

// The variables a memory share (services.<name>.memory) gives each service it sizes, in its container's environment: a
// service sizes its own caches from them.
const (
	MemLimitVar = "STOWAGE_MEM_LIMIT_MB" // the container's memory limit, in MiB
	HostMemVar  = "STOWAGE_HOST_MEM_MB"  // the host's RAM, in MiB
	HostCPUsVar = "STOWAGE_HOST_CPUS"    // the host's online CPUs
)

// MinMemLimit is the least limit a share gives.
const MinMemLimit = int64(spec.MinMemLimit)

// hostPlan is what a host gives the services: their memory shares (none without one) and their
// whole cores, by service.
type hostPlan struct {
	Shares map[string]spec.Share
	Host   tune.Host
	// CPUSets are each service's cores (services.<name>.cpus), as Compose's cpuset reads them.
	CPUSets map[string]string
	// IPVlans are each service's address of its own on a NIC (services.<name>.interface).
	IPVlans map[string]ipvlan
	// Tables are each service's table's last load's token (TableLabel, tableLoads), on the VM: a
	// load changes it, so the start that loads a table recreates its service.
	Tables map[string]string
	// Sockets are each start-first service's update_socket directory, which UpdateVolume mounts.
	Sockets map[string]string
}

// memoryPlan is the release's memory shares on host h, read at each start, so a host given more
// RAM gives the services more. None on Docker Desktop: its containers run in a VM of its own,
// whose RAM is not this host's.
func (in *Installer) memoryPlan(sys tune.System, h tune.Host) hostPlan {
	shares := in.R.Spec.MemoryShares()
	if len(shares) == 0 || sys.Docker != nil && sys.Docker.Desktop {
		return hostPlan{}
	}
	return hostPlan{Shares: shares, Host: h}
}

// limit is a container's limit from the share, split among n containers (spec.Share.Limit), on
// this host's RAM, never below floor: the limit the compose file sets itself, or the memory it
// reserves (Docker refuses a limit under the reservation).
func (p hostPlan) limit(share spec.Share, n int, floor int64) int64 {
	return int64(share.Limit(p.Host.MemBytes, n, spec.Bytes(floor)))
}

// give adds to a service's content entry the memory limit share gives each of its containers, on
// the key its compose file sets the limit with (mem_limit when none, both when both: Compose
// refuses them distinct), its swap limit moved by as much, and the variables it sizes itself
// from. The share is the service's: its containers (scale, deploy.replicas) split it.
func (p hostPlan) give(extra map[string]any, share spec.Share, svc serviceMemory) {
	memLimit, deploy := int64(svc.MemLimit), int64(svc.Deploy.Resources.Limits.Memory)
	own := max(memLimit, deploy)
	reserved := max(int64(svc.MemReservation), int64(svc.Deploy.Resources.Reservations.Memory))
	limit := p.limit(share, max(svc.Scale, svc.Deploy.Replicas), max(own, reserved))
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

// floorsOver is the warning a start logs when the least the shares give the services of m (each
// share's min, MinMemLimit or what the compose file gives the container itself, as lint sums them
// for the least VM) passes the part of this host's RAM the shares take (spec.MaxMemoryShares %):
// "" when it does not, or when the host's RAM is unknown.
func (p hostPlan) floorsOver(m *composeModel) string {
	if p.Host.MemBytes == 0 || len(p.Shares) == 0 {
		return ""
	}
	var total spec.Bytes
	largest, most := "", spec.Bytes(0)
	for _, name := range sortedKeys(p.Shares) {
		svc, ok := m.Services[name]
		if !ok {
			continue
		}
		own := max(int64(svc.MemLimit), int64(svc.Deploy.Resources.Limits.Memory), int64(svc.MemReservation), int64(svc.Deploy.Resources.Reservations.Memory))
		n := max(svc.Scale, svc.Deploy.Replicas, 1)
		floor := spec.Share{Min: p.Shares[name].Min}.Limit(0, n, spec.Bytes(own)) * spec.Bytes(n)
		total += floor
		if floor > most {
			largest, most = name, floor
		}
	}
	budget := spec.Bytes(p.Host.MemBytes / 100 * spec.MaxMemoryShares)
	if total <= budget {
		return ""
	}
	return fmt.Sprintf("the memory shares' floors total %d MiB, over the %d MiB this host's RAM leaves them (%d %%): services may be OOM-killed; give the host more RAM or lower services.%s.memory.min",
		total.MiB(), budget.MiB(), spec.MaxMemoryShares, largest)
}
