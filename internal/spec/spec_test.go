package spec

import (
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
`))
	if err != nil {
		t.Fatal(err)
	}
	v := s.RuntimeView()
	if !v.Version.Source.IsZero() || !v.Changelog.IsZero() || v.Channels != nil || !v.Registry.Push.IsZero() {
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
	if s.Signing.Config["key"] != "env://SECRET" || s.Channels == nil {
		t.Error("RuntimeView mutated the spec")
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
