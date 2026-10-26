package install

import (
	"strconv"
	"strings"

	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/tune"
)

// HostCoresVar is the variable a service given whole cores (services.<name>.cpus) finds the count
// of its CPUs in (its cores' threads), to size its own workers from.
const HostCoresVar = "STOWAGE_CPUS"

// coreRequests are the release's services.<name>.cpus, as tune allocates them.
func coreRequests(s *spec.Spec) []tune.CoreRequest {
	var out []tune.CoreRequest
	for _, name := range sortedKeys(s.Services) {
		if svc := s.Services[name]; svc.CPUs > 0 {
			out = append(out, tune.CoreRequest{Service: name, Cores: svc.CPUs, IRQ: svc.IRQ == spec.IRQOn})
		}
	}
	return out
}

// cpuSets are the services' whole cores on a host of cores (tune.Allocate), each a set as
// Compose's cpuset reads it.
func cpuSets(s *spec.Spec, cores [][]int) (map[string]string, error) {
	sets, err := tune.Allocate(coreRequests(s), cores)
	if err != nil || len(sets) == 0 {
		return nil, err
	}
	out := map[string]string{}
	for name, ids := range sets {
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = strconv.Itoa(id)
		}
		out[name] = strings.Join(parts, ",")
	}
	return out, nil
}

// giveCPUs adds to a service's content entry its cpuset, and the count of its CPUs to its
// environment (beside a memory share's variables).
func (p hostPlan) giveCPUs(extra map[string]any, name string) {
	set, ok := p.CPUSets[name]
	if !ok {
		return
	}
	extra["cpuset"] = set
	env, _ := extra["environment"].(map[string]string)
	if env == nil {
		env = map[string]string{}
	}
	env[HostCoresVar] = strconv.Itoa(len(strings.Split(set, ",")))
	extra["environment"] = env
}
