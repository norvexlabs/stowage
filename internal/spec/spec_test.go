package spec

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const base = "apiVersion: stowage/v1\nname: x\n"

func TestParseFormatVersion(t *testing.T) {
	if _, err := Parse([]byte(base)); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse([]byte("name: x\n")); err == nil || !strings.Contains(err.Error(), "apiVersion is missing") {
		t.Errorf("missing apiVersion: %v", err)
	}
	if _, err := Parse([]byte("apiVersion: stowage/v2\n")); err == nil || !strings.Contains(err.Error(), "upgrade stowage") {
		t.Errorf("future apiVersion: %v", err)
	}
	if _, err := Parse([]byte(base + "bogus: 1\n")); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("unknown key: %v", err)
	}
	if _, err := Parse([]byte("apiVersion: [\n")); err == nil {
		t.Error("invalid YAML accepted")
	}
}

func TestProviderInlineAndDecode(t *testing.T) {
	s, err := Parse([]byte(base + "health: { provider: http-status, port: 80, path: /r }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Health.Name != "http-status" || s.Health.Config["port"] != 80 {
		t.Fatalf("Health = %+v", s.Health)
	}
	var ok struct {
		Port int    `yaml:"port"`
		Path string `yaml:"path"`
	}
	if err := s.Health.Decode(&ok); err != nil || ok.Port != 80 || ok.Path != "/r" {
		t.Errorf("Decode = %+v, %v", ok, err)
	}
	var narrow struct {
		Port int `yaml:"port"`
	}
	if err := s.Health.Decode(&narrow); err == nil || !strings.Contains(err.Error(), "path") {
		t.Errorf("Decode accepted an unknown key: %v", err)
	}
	if !(Provider{}).IsZero() || s.Health.IsZero() {
		t.Error("IsZero")
	}
	if (Provider{}).Or("d").Name != "d" || s.Health.Or("d").Name != "http-status" {
		t.Error("Or")
	}
}

func TestRuntimeViewDropsPublisherFields(t *testing.T) {
	s, err := Parse([]byte(base + `
version: { source: { provider: file } }
changelog: { provider: keepachangelog }
channels: { stable: { tag: "{v}" } }
registry:
  prefix: r.example/x
  pull: { provider: none }
  push: { provider: token, user: p, token_env: T }
signing: { provider: cosign-key, key: env://SECRET, public_keys: [a.pub] }
lint: { accept: lint-accept.yaml }
`))
	if err != nil {
		t.Fatal(err)
	}
	v := s.RuntimeView()
	if !v.Version.Source.IsZero() || !v.Changelog.IsZero() || v.Channels != nil || !v.Registry.Push.IsZero() || v.Lint != nil {
		t.Errorf("publisher fields kept: %+v", v)
	}
	if v.Registry.Prefix != "r.example/x" || v.Registry.Pull.Name != "none" {
		t.Error("runtime registry fields dropped")
	}
	if _, ok := v.Signing.Config["key"]; ok {
		t.Error("the signing key reached the runtime view")
	}
	if !reflect.DeepEqual(v.Signing.Config["public_keys"], []any{"a.pub"}) {
		t.Errorf("public keys = %v", v.Signing.Config["public_keys"])
	}
	// The original is untouched.
	if s.Signing.Config["key"] != "env://SECRET" || s.Channels == nil || s.Lint == nil || s.Lint.Accept != "lint-accept.yaml" {
		t.Error("RuntimeView mutated the spec")
	}
}

// The kubernetes shape's answers to lint are the publisher's: the runtime view drops them, the spec
// keeps them, and the view encodes as it does without them.
func TestTheKubernetesLintAnswersAreThePublishers(t *testing.T) {
	t.Parallel()
	with, err := Parse([]byte(base + "shapes:\n  kubernetes:\n    chart: c\n    unserved: [a]\n    guarded: [b]\n"))
	if err != nil {
		t.Fatal(err)
	}
	without, err := Parse([]byte(base + "shapes:\n  kubernetes:\n    chart: c\n"))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(with.RuntimeView())
	b, _ := json.Marshal(without.RuntimeView())
	if string(a) != string(b) || len(with.Shapes.Kubernetes.Unserved) != 1 || len(with.Shapes.Kubernetes.Guarded) != 1 {
		t.Errorf("runtime view %s, want %s; spec %+v", a, b, with.Shapes.Kubernetes)
	}
}

// The disks policy is the publisher's: no view an artifact carries keeps it, and the spec does.
func TestTheDisksPolicyIsThePublishers(t *testing.T) {
	t.Parallel()
	s, err := Parse([]byte(base + "shapes:\n  kairos:\n    disks: always\n"))
	if err != nil {
		t.Fatal(err)
	}
	if v := s.RuntimeView(); v.Shapes.Kairos.Disks != "" || v.Shapes.Kairos.AlwaysDisks() {
		t.Errorf("the runtime view keeps the policy: %+v", v.Shapes.Kairos)
	}
	if v := s.BundleView(); v.Shapes.Kairos.Disks != "" {
		t.Errorf("the bundle view keeps the policy: %+v", v.Shapes.Kairos)
	}
	// Left out of the view's JSON, which inputs digests hash: a release's inputs are as before it.
	if b, err := json.Marshal(s.RuntimeView()); err != nil || strings.Contains(string(b), `"Disks"`) {
		t.Errorf("the runtime view's JSON names the policy: %v %s", err, b)
	}
	if !s.Shapes.Kairos.AlwaysDisks() {
		t.Error("a view dropped the spec's own policy")
	}
	var none *KairosShape
	if none.AlwaysDisks() || (&KairosShape{Disks: DisksOptIn}).AlwaysDisks() {
		t.Error("AlwaysDisks without disks: always")
	}
}

func TestCheckEnvValueLength(t *testing.T) {
	if err := CheckEnvValue(strings.Repeat("v", MaxEnvValue)); err != nil {
		t.Fatalf("at the bound: %v", err)
	}
	if err := CheckEnvValue(strings.Repeat("v", MaxEnvValue+1)); err == nil {
		t.Fatal("past the bound passed")
	}
}

func TestTheHealthGateDefaultsWhenUnset(t *testing.T) {
	t.Parallel()
	var none *KairosShape
	if g := none.Gate(); g != DefaultHealthGate || g.DeadlineS != 600 || g.PollS != 30 {
		t.Errorf("no shape: %+v", g)
	}
	if g := (&KairosShape{}).Gate(); g != DefaultHealthGate {
		t.Errorf("unset: %+v", g)
	}
	set := HealthGate{DeadlineS: 60, DeadlineAfterResetS: 90, PollS: 5}
	if g := (&KairosShape{HealthGate: &set}).Gate(); g != set {
		t.Errorf("set: %+v", g)
	}
}

func TestUnheldNetwork(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		driver string
		opts   map[string]string
		want   string
	}{
		{"", nil, ""},
		{"bridge", map[string]string{"com.docker.network.driver.mtu": "1400"}, ""},
		{"macvlan", nil, "is of the macvlan driver"},
		{"bridge", map[string]string{"com.docker.network.bridge.name": "br-lan"}, "sets com.docker.network.bridge.name br-lan"},
		{"", map[string]string{"com.docker.network.bridge.trusted_host_interfaces": "eth1"}, "sets com.docker.network.bridge.trusted_host_interfaces eth1"},
	} {
		if got := UnheldNetwork(c.driver, c.opts); got != c.want {
			t.Errorf("%s %v: %q, want %q", c.driver, c.opts, got, c.want)
		}
	}
}

// A key written with nothing under it (null, {} or []) is refused with its line; the same key with
// content, or left out, decodes.
func TestParseRefusesEmptyKeys(t *testing.T) {
	t.Parallel()
	cases := []struct{ empty, full, want string }{
		{"services:\n", "services: { api: { memory: 10 } }\n", "stowage.yaml:3: services is written with nothing under it: name a service, or remove services"},
		{"services: {}\n", "", "stowage.yaml:3: services is written with nothing under it"},
		{"tune: ~\n", "tune: { profile: network }\n", "stowage.yaml:3: tune is written with nothing under it: give its profile, or remove tune"},
		{"shapes:\n  kubernetes:\n    chart: c\n    unserved:\n", "shapes:\n  kubernetes:\n    chart: c\n    unserved: [a]\n", "stowage.yaml:6: shapes.kubernetes.unserved is written with nothing under it: list the front routes the chart does not serve, or remove unserved"},
		{"shapes:\n  kubernetes:\n    chart: c\n    guarded: ~\n", "shapes:\n  kubernetes:\n    chart: c\n    guarded: [a]\n", "stowage.yaml:6: shapes.kubernetes.guarded is written with nothing under it: list the front routes whose guards the chart's ingress or the operator's enforces, or remove guarded"},
		{"shapes:\n  kubernetes:\n    chart: c\n    unserved: []\n", "shapes:\n  kubernetes:\n    chart: c\n    unserved: [a]\n", "stowage.yaml:6: shapes.kubernetes.unserved is written with nothing under it"},
		{"shapes:\n  kairos:\n    size:\n", "shapes:\n  kairos:\n    size: { min: { cpus: 2 } }\n", "stowage.yaml:5: shapes.kairos.size is written with nothing under it: give min, recommended or both, or remove size"},
		{"shapes:\n  kairos:\n    size: { min: {}, recommended: { cpus: 2 } }\n", "", "stowage.yaml:5: shapes.kairos.size.min is written with nothing under it: give cpus, memory or disk, or remove min"},
		{"shapes:\n  kairos:\n    size: { min: { cpus: 1 }, recommended: }\n", "", "stowage.yaml:5: shapes.kairos.size.recommended is written with nothing under it"},
	}
	for _, c := range cases {
		if _, err := Parse([]byte(base + c.empty)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: want %q, got %v", c.empty, c.want, err)
		}
		if c.full == "" {
			continue
		}
		if _, err := Parse([]byte(base + c.full)); err != nil {
			t.Errorf("%q: %v", c.full, err)
		}
	}
	if _, err := Parse([]byte(base + "shapes:\n  kairos: {}\n")); err != nil {
		t.Errorf("a kairos shape without size: %v", err)
	}
}

// Each ARP setting's range, on both sides of its bounds; another name passes.
func TestCheckARP(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, value string
		ok          bool
	}{
		{"arp_announce", "0", true}, {"arp_announce", "2", true}, {"arp_announce", "3", false}, {"arp_announce", "-1", false},
		{"arp_ignore", "3", true}, {"arp_ignore", "4", false}, {"arp_ignore", "7", false}, {"arp_ignore", "8", true}, {"arp_ignore", "9", false},
		{"arp_ignore", "one", false}, {"arp_filter", "9", true},
	} {
		if err := CheckARP(c.name, c.value); (err == nil) != c.ok {
			t.Errorf("%s %s: %v", c.name, c.value, err)
		}
	}
	if err := CheckARP("arp_ignore", "4"); err == nil || err.Error() != "arp_ignore is 0, 1, 2, 3 or 8" {
		t.Errorf("message: %v", err)
	}
}

// An alias or a merge key leaving a key empty is refused as the key written empty: decoding reads
// them; one bringing content is not.
func TestParseRefusesEmptyKeysThroughAliases(t *testing.T) {
	t.Parallel()
	for doc, want := range map[string]string{
		"safe_defaults: &e {}\nservices: *e\n":                                     "stowage.yaml:4: services is written with nothing under it",
		"safe_defaults: &e {}\nshapes:\n  kairos:\n    size: *e\n":                 "stowage.yaml:6: shapes.kairos.size is written with nothing under it",
		"safe_defaults: &e {}\nservices: { <<: *e }\n":                             "stowage.yaml:4: services is written with nothing under it",
		"safe_defaults: &e {}\nservices: { <<: [*e, *e] }\n":                       "stowage.yaml:4: services is written with nothing under it",
		"safe_defaults: &n\ntune: *n\n":                                            "stowage.yaml:4: tune is written with nothing under it",
		"shapes:\n  kairos:\n    size: { min: &m { cpus: 2 }, recommended: *m }\n": "",
		"safe_defaults: &e {}\nservices: { <<: *e, api: { memory: 10 } }\n":        "",
	} {
		_, err := Parse([]byte(base + doc))
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%q: want %q, got %v", doc, want, err)
		}
	}
}
