package install

import (
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/tune"
)

// A service given whole cores runs on them (cpuset), the count of its CPUs in its environment
// beside a memory share's variables; a host of too few cores fails the start, naming the services.
func TestServicesRunOnTheirWholeCores(t *testing.T) {
	t.Parallel()
	s := &spec.Spec{Services: map[string]spec.Service{"echo": {CPUs: 1, IRQ: spec.IRQOn}, "api": {Memory: &spec.Share{Percent: 10}}}}
	sets, err := cpuSets(s, [][]int{{0, 2}, {1, 3}})
	if err != nil || sets["echo"] != "1,3" || len(sets) != 1 {
		t.Fatalf("sets %v %v", sets, err)
	}
	if reqs := coreRequests(s); len(reqs) != 1 || reqs[0] != (tune.CoreRequest{Service: "echo", Cores: 1, IRQ: true}) {
		t.Errorf("requests %+v", reqs)
	}
	plan := hostPlan{CPUSets: sets}
	extra := map[string]any{"environment": map[string]string{MemLimitVar: "512"}}
	plan.giveCPUs(extra, "echo")
	env := extra["environment"].(map[string]string)
	if extra["cpuset"] != "1,3" || env[HostCoresVar] != "2" || env[MemLimitVar] != "512" {
		t.Errorf("extra %v", extra)
	}
	none := map[string]any{}
	plan.giveCPUs(none, "api")
	if len(none) != 0 {
		t.Errorf("a service without cores: %v", none)
	}
	if _, err := cpuSets(s, [][]int{{0, 1}}); err == nil || !strings.Contains(err.Error(), "echo ask 1 whole cores, and this host has 0 beside the one of CPU 0") {
		t.Errorf("one core: %v", err)
	}
}
