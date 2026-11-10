package install

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/norvexlabs/stowage/internal/spec"
)

// MachineFormat is the version of the installer's machine interface, the JSON programs read
// (`describe --json`, `status --json`, the verbs' results). It moves only on a breaking change: a field removed, renamed or given
// another meaning. A field added keeps it.
const MachineFormat = 1

// firstInstallOnly are the settings an install records at its first run, by the names `describe`
// gives them, and how a run's options give each: a re-run given one is refused (rerun).
var firstInstallOnly = []firstInstallSetting{
	{"address", func(o *Options) bool { return o.Address != "" }},
	{"registry", func(o *Options) bool { return o.Registry != "" }},
	{"registry_ca", func(o *Options) bool { return o.RegistryCA != "" }},
	{"channel", func(o *Options) bool { return o.Channel != "" }},
}

// firstInstallSetting is a setting only a first install takes: its name, and whether a run's
// options give it.
type firstInstallSetting struct {
	Name  string
	given func(*Options) bool
}

// flag is the installer's flag giving the setting: its name, '_' read as '-'.
func (s firstInstallSetting) flag() string { return "--" + strings.ReplaceAll(s.Name, "_", "-") }

// firstInstallOnlyRefusal is a re-run's refusal of the settings it was given (at least one), each
// named by its flag.
func firstInstallOnlyRefusal(settings []firstInstallSetting) error {
	flags := make([]string, len(settings))
	for i, s := range settings {
		flags[i] = s.flag()
	}
	var named, verb string
	switch n := len(flags); n {
	case 1:
		named, verb = flags[0], "is"
	default:
		named, verb = strings.Join(flags[:n-1], ", ")+" and "+flags[n-1], "are"
	}
	return fmt.Errorf("%s %s recorded at the first install: this directory already holds one (a re-run changes the inputs with --set, --answers and --domains, the front's ports, and its TLS mode and certificate)", named, verb)
}

// Description is `describe --json`: what the release this installer carries offers, read with no
// install. It comes from the payload's manifest and spec alone.
type Description struct {
	Format int    `json:"format"`
	Name   string `json:"name"`
	// ShortName is the <short> of the installer <short>-stow, which an install keeps as <dir>/<short>-stow.
	ShortName        string           `json:"short_name"`
	Release          Version          `json:"release"`
	Channel          string           `json:"channel"`
	Shapes           []string         `json:"shapes"`
	Inputs           []DescribedInput `json:"inputs"`
	FirstInstallOnly []string         `json:"first_install_only"`
}

// DescribedInput is one of the spec's inputs, in the spec's order. Default is left out for a
// secret input, whatever the spec holds; an empty Shapes is every shape.
type DescribedInput struct {
	Key      string   `json:"key"`
	Type     string   `json:"type"`
	Secret   bool     `json:"secret"`
	Required bool     `json:"required"`
	Optional bool     `json:"optional"`
	Shapes   []string `json:"shapes"`
	When     string   `json:"when"`
	Advanced bool     `json:"advanced"`
	Profile  string   `json:"profile"`
	Prompt   string   `json:"prompt"`
	Default  *string  `json:"default,omitempty"`
}

// Describe is what r offers.
func Describe(r *Release) Description {
	d := Description{
		Format:    MachineFormat,
		Name:      r.Manifest.Name,
		ShortName: r.Spec.Short(),
		Release:   Version{Version: r.Manifest.ProjectVersion, Revision: r.Manifest.Revision},
		Channel:   r.Manifest.Channel,
		Shapes:    append([]string{}, r.Spec.Shapes.Names()...),
		Inputs:    []DescribedInput{},
	}
	for _, i := range r.Spec.Inputs {
		d.Inputs = append(d.Inputs, describeInput(i))
	}
	for _, s := range firstInstallOnly {
		d.FirstInstallOnly = append(d.FirstInstallOnly, s.Name)
	}
	return d
}

func describeInput(i spec.Input) DescribedInput {
	t := i.Type
	if t == "" {
		t = "text"
	}
	di := DescribedInput{Key: i.Key, Type: t, Secret: i.Secret, Required: i.Required, Optional: i.Optional,
		Shapes: append([]string{}, i.Shapes...), When: i.When, Advanced: i.Advanced, Profile: i.Profile, Prompt: i.Prompt}
	if !i.Secret {
		def := i.Default
		di.Default = &def
	}
	return di
}

// describeCommand is `describe`: Describe as JSON with --json, else a short summary for people.
func describeCommand(r *Release, stdout io.Writer) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "describe",
		Short: "What the release this installer carries offers: its shapes, its inputs, the settings only a first install takes",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			d := Describe(r)
			if asJSON {
				return printJSON(stdout, d)
			}
			return d.print(stdout)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print it as JSON, the machine interface")
	return c
}

func (d Description) print(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s, channel %s\n", d.Name, d.Release, d.Channel)
	fmt.Fprintf(&b, "shapes    %s\n", strings.Join(d.Shapes, ", "))
	for _, i := range d.Inputs {
		var tags []string
		if i.Type != "text" {
			tags = append(tags, i.Type)
		}
		for _, t := range []struct {
			on   bool
			name string
		}{{i.Secret, "secret"}, {i.Required, "required"}, {i.Optional, "optional"}, {i.Advanced, "advanced"}} {
			if t.on {
				tags = append(tags, t.name)
			}
		}
		if len(i.Shapes) > 0 {
			tags = append(tags, "shapes "+strings.Join(i.Shapes, ","))
		}
		if i.When != "" {
			tags = append(tags, "when "+i.When)
		}
		if i.Profile != "" {
			tags = append(tags, "profile "+i.Profile)
		}
		if i.Default != nil && *i.Default != "" {
			tags = append(tags, fmt.Sprintf("default %q", *i.Default))
		}
		fmt.Fprintf(&b, "input     %s", i.Key)
		if len(tags) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(tags, ", "))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "first install only: %s\n", strings.Join(d.FirstInstallOnly, ", "))
	_, err := io.WriteString(w, b.String())
	return err
}
