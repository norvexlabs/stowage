package spec

import (
	"bytes"
	"fmt"

	"go.yaml.in/yaml/v3"
)

// Provider is the one shape every project-specific concern takes: `{ provider: <name>, …config }`.
// The config keys belong to the named provider, which decodes them strictly with Decode.
type Provider struct {
	Name   string         `yaml:"provider"`
	Config map[string]any `yaml:",inline"`
}

// IsZero reports whether the concern was left out of the spec, so its default provider applies.
func (p Provider) IsZero() bool { return p.Name == "" && len(p.Config) == 0 }

// Decode decodes the provider's config into out, refusing keys out does not declare.
func (p Provider) Decode(out any) error {
	if len(p.Config) == 0 {
		return nil
	}
	raw, err := yaml.Marshal(p.Config)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("provider %q: %w", p.Name, err)
	}
	return nil
}

// Or returns p, or a provider named def when p was left out.
func (p Provider) Or(def string) Provider {
	if p.Name == "" {
		p.Name = def
	}
	return p
}
