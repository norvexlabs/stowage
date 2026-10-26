package install

import (
	"encoding/json"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/tune"
)

func TestContentComposeGivesTheMemoryShares(t *testing.T) {
	t.Parallel()
	var model composeModel
	// As `docker compose config --format json` renders sizes: strings of bytes.
	if err := json.Unmarshal([]byte(`{"services": {
		"db": {"mem_limit": "536870912", "memswap_limit": "1073741824"},
		"api": {"deploy": {"resources": {"limits": {"memory": "268435456"}}}},
		"both": {"mem_limit": "268435456", "deploy": {"resources": {"limits": {"memory": "268435456"}}}},
		"bare": {},
		"large": {"mem_limit": 4294967296},
		"small": {},
		"reserved": {"mem_reservation": "1073741824"},
		"scaled": {"scale": 2},
		"replicated": {"deploy": {"replicas": 4, "resources": {"reservations": {"memory": "134217728"}}}},
		"capped": {},
		"other": {}}}`), &model); err != nil {
		t.Fatal(err)
	}
	mem := hostPlan{
		Shares: map[string]spec.Share{"db": {Percent: 50}, "api": {Percent: 25}, "both": {Percent: 25}, "bare": {Percent: 10}, "large": {Percent: 10},
			"small": {Percent: 1}, "reserved": {Percent: 10}, "scaled": {Percent: 10}, "replicated": {Percent: 10}, "gone": {Percent: 5},
			"capped": {Percent: 50, Max: 1 << 30}},
		Host: tune.Host{MemBytes: 4 << 30, CPUs: 3},
	}
	raw, err := contentCompose(&model, nil, []byte("key"), tune.Container{}, mem, hostNet{})
	if err != nil {
		t.Fatal(err)
	}
	type svc struct {
		MemLimit     int64             `yaml:"mem_limit"`
		MemswapLimit int64             `yaml:"memswap_limit"`
		Environment  map[string]string `yaml:"environment"`
		Deploy       struct {
			Resources struct {
				Limits struct {
					Memory int64 `yaml:"memory"`
				} `yaml:"limits"`
			} `yaml:"resources"`
		} `yaml:"deploy"`
	}
	var doc struct {
		Services map[string]svc `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	got := doc.Services
	const mib = 1 << 20
	// 50 % of 4 GiB, its swap moved by as much as its limit.
	if s := got["db"]; s.MemLimit != 2048*mib || s.MemswapLimit != 2560*mib || s.Deploy.Resources.Limits.Memory != 0 {
		t.Errorf("db: %+v", s)
	}
	if e := got["db"].Environment; e[MemLimitVar] != "2048" || e[HostMemVar] != "4096" || e[HostCPUsVar] != "3" {
		t.Errorf("db's environment: %v", e)
	}
	// Set where the compose file sets it: Compose refuses mem_limit beside a distinct deploy limit.
	if s := got["api"]; s.MemLimit != 0 || s.Deploy.Resources.Limits.Memory != 1024*mib {
		t.Errorf("api: %+v", s)
	}
	if s := got["both"]; s.MemLimit != 1024*mib || s.Deploy.Resources.Limits.Memory != 1024*mib {
		t.Errorf("both: %+v", s)
	}
	// 10 % of 4 GiB, whole MiB.
	if s := got["bare"]; s.MemLimit != 409*mib || s.Environment[MemLimitVar] != "409" {
		t.Errorf("bare: %+v", s)
	}
	// Never below the compose file's own limit: it keeps it, and says so.
	if s := got["large"]; s.MemLimit != 0 || s.Environment[MemLimitVar] != "4096" {
		t.Errorf("large: %+v", s)
	}
	if s := got["small"]; s.MemLimit != MinMemLimit || s.Environment[MemLimitVar] != "64" {
		t.Errorf("small, 1 %% of 4 GiB under the floor: %+v", s)
	}
	// Never under its reservation, which Docker refuses a limit below.
	if s := got["reserved"]; s.MemLimit != 1024*mib {
		t.Errorf("reserved: %+v", s)
	}
	// The service's share, split among its containers.
	if s := got["scaled"]; s.MemLimit != 204*mib {
		t.Errorf("scaled: %+v", s)
	}
	if s := got["replicated"]; s.MemLimit != 128*mib || s.Environment[MemLimitVar] != "128" {
		t.Errorf("replicated, 102 MiB each under its 128 MiB reservation: %+v", s)
	}
	// 50 % of 4 GiB capped at 1 GiB: the cap is what it is given and told.
	if s := got["capped"]; s.MemLimit != 1024*mib || s.Environment[MemLimitVar] != "1024" {
		t.Errorf("capped: %+v", s)
	}
	for _, name := range []string{"other", "gone"} {
		if _, ok := got[name]; ok {
			t.Errorf("%s given %+v", name, got[name])
		}
	}
}

func TestMemoryShareBounds(t *testing.T) {
	t.Parallel()
	const mib = 1 << 20
	p := hostPlan{Host: tune.Host{MemBytes: 8 << 30}}
	for _, c := range []struct {
		pct       int
		own, want int64
	}{
		{1, 0, 81 * mib},        // 1 % of 8 GiB: above the floor
		{1, 81 * mib, 81 * mib}, // the compose file's own limit equal: kept
		{1, 82 * mib, 82 * mib}, // just above: its own
		{25, 0, 2048 * mib},     // a quarter
		{90, 0, 7372 * mib},     // the most a share is
		{0, 0, MinMemLimit},     // nothing: the floor
		{0, MinMemLimit + 1, MinMemLimit + 1},
	} {
		if got := p.limit(spec.Share{Percent: c.pct}, 1, c.own); got != c.want {
			t.Errorf("%d %% with own %d: %d MiB, want %d MiB", c.pct, c.own, got/mib, c.want/mib)
		}
	}
	small := hostPlan{Host: tune.Host{MemBytes: 6400 * mib}}
	if got := small.limit(spec.Share{Percent: 1}, 1, 0); got != MinMemLimit {
		t.Errorf("1 %% of 6400 MiB, at the floor: %d MiB", got/mib)
	}
	under := hostPlan{Host: tune.Host{MemBytes: 6336 * mib}}
	if got := under.limit(spec.Share{Percent: 1}, 1, 0); got != MinMemLimit {
		t.Errorf("1 %% of 6336 MiB, under the floor: %d MiB", got/mib)
	}
	if got := p.limit(spec.Share{Percent: 25}, 2, 0); got != 1024*mib {
		t.Errorf("25 %% of 8 GiB split in two: %d MiB", got/mib)
	}
	if got := p.limit(spec.Share{Percent: 25}, 0, 0); got != 2048*mib {
		t.Errorf("25 %% of 8 GiB, no replica count: %d MiB", got/mib)
	}
	// The maximum, on both sides of the share it caps, split with the share, never under the floors.
	for _, c := range []struct {
		max       spec.Bytes
		n         int
		own, want int64
	}{
		{2049 * mib, 1, 0, 2048 * mib},              // over the share: the share
		{2048 * mib, 1, 0, 2048 * mib},              // the share itself
		{2047 * mib, 1, 0, 2047 * mib},              // under it: the maximum
		{2047 * mib, 2, 0, 1023 * mib},              // split: half of each, in whole MiB
		{100 * mib, 1, 512 * mib, 512 * mib},        // the compose file's own limit is a floor
		{spec.MinMemLimit - mib, 1, 0, MinMemLimit}, // under the least limit: the least
	} {
		if got := p.limit(spec.Share{Percent: 25, Max: c.max}, c.n, c.own); got != c.want {
			t.Errorf("25 %% of 8 GiB, max %s, %d containers, own %d: %d MiB, want %d MiB", c.max, c.n, c.own, got/mib, c.want/mib)
		}
	}
	over := hostPlan{Host: tune.Host{MemBytes: 6528 * mib}}
	if got := over.limit(spec.Share{Percent: 1}, 1, 0); got != 65*mib {
		t.Errorf("1 %% of 6528 MiB, above the floor: %d MiB", got/mib)
	}
}

// Docker Desktop's containers run in its own VM: the host's RAM sizes none of them.
func TestNoMemorySharesOnDockerDesktop(t *testing.T) {
	t.Parallel()
	in := &Installer{R: &Release{Spec: &spec.Spec{Services: map[string]spec.Service{"db": {Memory: &spec.Share{Percent: 50}}}}}}
	h := tune.Host{MemBytes: 8 << 30}
	if p := in.memoryPlan(tune.System{Docker: &tune.Docker{}}, h); p.Shares["db"].Percent != 50 {
		t.Errorf("on an engine: %+v", p)
	}
	if p := in.memoryPlan(tune.System{Docker: &tune.Docker{Desktop: true}}, h); len(p.Shares) != 0 {
		t.Errorf("on Docker Desktop: %+v", p)
	}
}
