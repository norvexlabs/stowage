package install

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/testutil"
)

var updateDescribe = flag.Bool("update-describe", false, "rewrite testdata/describe-full.json")

// describeInputFields maps each field of spec.Input to the DescribedInput key that carries it; a
// field describe leaves out on purpose would map to "" with its reason in a comment. A field added
// to spec.Input fails TestEveryInputFieldIsDecidedInDescribe until it is mapped here.
var describeInputFields = map[string]string{
	"Key":      "key",
	"Prompt":   "prompt",
	"Default":  "default",
	"Required": "required",
	"Optional": "optional",
	"Secret":   "secret",
	"When":     "when",
	"Shapes":   "shapes",
	"Advanced": "advanced",
	"AsFile":   "as_file",
	"Profile":  "profile",
	"Type":     "type",
}

func TestEveryInputFieldIsDecidedInDescribe(t *testing.T) {
	t.Parallel()
	in := reflect.TypeFor[spec.Input]()
	for f := range in.Fields() {
		if _, ok := describeInputFields[f.Name]; !ok {
			t.Errorf("spec.Input.%s: describe neither carries it nor leaves it out on purpose: map it in describeInputFields (and DescribedInput)", f.Name)
		}
	}
	var keys []string
	for f := range reflect.TypeFor[DescribedInput]().Fields() {
		keys = append(keys, strings.Split(f.Tag.Get("json"), ",")[0])
	}
	for field, key := range describeInputFields {
		if _, ok := in.FieldByName(field); !ok {
			t.Errorf("describeInputFields names spec.Input.%s, which is gone", field)
		}
		if key != "" && !slices.Contains(keys, key) {
			t.Errorf("spec.Input.%s maps to %q, which DescribedInput has not", field, key)
		}
	}
}

// describeHandoffFields maps each field of spec.Handoff and spec.HandoffAsk to the JSON key describe
// gives it, or "" for one it leaves out on purpose (the run's container is the installer's own).
var describeHandoffFields = map[reflect.Type]map[string]string{
	reflect.TypeFor[spec.Handoff](): {"Name": "name", "For": "for", "Instance": "instance", "Ask": "ask",
		"Run": "", "Gives": "gives", "Check": "check"},
	reflect.TypeFor[spec.HandoffAsk](): {"Key": "key", "Prompt": "prompt", "Secret": "secret"},
}

func TestEveryHandoffFieldIsDecidedInDescribe(t *testing.T) {
	t.Parallel()
	described := map[reflect.Type]reflect.Type{
		reflect.TypeFor[spec.Handoff]():    reflect.TypeFor[DescribedHandoff](),
		reflect.TypeFor[spec.HandoffAsk](): reflect.TypeFor[DescribedHandoffAsk](),
	}
	for typ, fields := range describeHandoffFields {
		var keys []string
		for f := range described[typ].Fields() {
			keys = append(keys, f.Tag.Get("json"))
		}
		for f := range typ.Fields() {
			key, ok := fields[f.Name]
			switch {
			case !ok:
				t.Errorf("%s.%s: describe neither carries it nor leaves it out on purpose: map it in describeHandoffFields", typ, f.Name)
			case key != "" && !slices.Contains(keys, key):
				t.Errorf("%s.%s maps to %q, which %s has not", typ, f.Name, key, described[typ])
			}
		}
		// The other way: a key describe prints that no field of the spec gives.
		for _, key := range keys {
			if !slices.Contains(slices.Collect(maps.Values(fields)), key) {
				t.Errorf("%s prints %q, which no field of %s maps to", described[typ], key, typ)
			}
		}
	}
	// Each field set, then each left at its zero, as a handoff without check or secret answers is.
	h := spec.Handoff{Name: "n", For: "f", Instance: "i", Ask: []spec.HandoffAsk{{Key: "K", Prompt: "p", Secret: true}},
		Gives: []string{"G"}, Check: &spec.HandoffStep{}}
	bare := spec.Handoff{Name: "b", For: "f", Ask: []spec.HandoffAsk{{Key: "Q", Prompt: "q"}}, Gives: []string{"H"}}
	got := Describe(&Release{Spec: &spec.Spec{Handoffs: []spec.Handoff{h, bare}}, Manifest: &release.Manifest{}}).Handoffs
	want := []DescribedHandoff{{Name: "n", For: "f", Instance: "i", Ask: []DescribedHandoffAsk{{Key: "K", Prompt: "p", Secret: true}}, Gives: []string{"G"}, Check: true},
		{Name: "b", For: "f", Ask: []DescribedHandoffAsk{{Key: "Q", Prompt: "q"}}, Gives: []string{"H"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Describe's handoffs = %+v, want %+v", got, want)
	}
}

// examples/full, which uses every input key, described as its installer would.
func TestDescribeFull(t *testing.T) {
	t.Parallel()
	p, err := project.Open(testutil.CopyExample(t, "full"), "")
	if err != nil {
		t.Fatal(err)
	}
	view := p.Spec.RuntimeView()
	r := &Release{Spec: &view, Manifest: &release.Manifest{Name: view.Name, ProjectVersion: "2.4.0", Revision: 3, Channel: "stable"}}
	var got bytes.Buffer
	if err := printJSON(&got, Describe(r)); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join("testdata", "describe-full.json")
	if *updateDescribe {
		if err := os.WriteFile(file, got.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Errorf("describe --json of examples/full:\n%s\nwant (testdata/describe-full.json; -update-describe rewrites it):\n%s", got.Bytes(), want)
	}
}

// describe reads the payload alone: it runs with no install, and a secret input's default never
// reaches its output, as JSON or as text.
func TestDescribeNeedsNoInstallAndHidesASecretsDefault(t *testing.T) {
	t.Parallel()
	const secretDefault = "do-not-print-me"
	r, self := testInstallerWith(t, "1.0.0", func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "API_TOKEN", Prompt: "API token", Secret: true, Default: secretDefault}, spec.Input{Key: "PLAIN", Default: "shown"})
	})
	var out bytes.Buffer
	cmd := Command(r, self, &out, io.Discard)
	cmd.SetArgs([]string{"describe", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), secretDefault) {
		t.Errorf("a secret's default printed:\n%s", out.String())
	}
	var d map[string]any
	dec := json.NewDecoder(&out)
	if err := dec.Decode(&d); err != nil {
		t.Fatal(err)
	}
	if d["format"] != float64(MachineFormat) || d["name"] != "notes" || d["short_name"] != r.Spec.Short() || r.Ctl() != "notes-stow" || d["channel"] != "stable" {
		t.Errorf("%v", d)
	}
	if rel, _ := d["release"].(map[string]any); rel["version"] != "1.0.0" || rel["revision"] != float64(1) {
		t.Errorf("release %v", d["release"])
	}
	inputs, _ := d["inputs"].([]any)
	byKey := map[string]map[string]any{}
	for _, i := range inputs {
		m := i.(map[string]any)
		byKey[m["key"].(string)] = m
	}
	if _, ok := byKey["API_TOKEN"]["default"]; ok || byKey["API_TOKEN"]["secret"] != true {
		t.Errorf("secret input %v", byKey["API_TOKEN"])
	}
	if byKey["PLAIN"]["default"] != "shown" || byKey["PLAIN"]["type"] != "text" {
		t.Errorf("plain input %v", byKey["PLAIN"])
	}

	out.Reset()
	cmd = Command(r, self, &out, io.Discard)
	cmd.SetArgs([]string{"describe"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Contains(text, secretDefault) || !strings.Contains(text, "notes 1.0.0 (revision 1), channel stable") || !strings.Contains(text, `input     PLAIN (default "shown")`) ||
		!strings.Contains(text, "first install only: address, registry, registry_ca, channel") {
		t.Errorf("text:\n%s", text)
	}
}

// Every setting describe names as first-install-only is refused on a re-run, by the same table.
func TestFirstInstallOnlyIsWhatARerunRefuses(t *testing.T) {
	t.Parallel()
	given := map[string]func(*Options){
		"address":     func(o *Options) { o.Address = "127.0.0.9" },
		"registry":    func(o *Options) { o.Registry = "mirror.example.org/notes" },
		"registry_ca": func(o *Options) { o.RegistryCA = "ca.pem" },
		"channel":     func(o *Options) { o.Channel = "beta" },
	}
	var names []string
	for _, s := range firstInstallOnly {
		names = append(names, s.Name)
		set, ok := given[s.Name]
		if !ok {
			t.Errorf("%s: no option sets it here", s.Name)
			continue
		}
		var o Options
		set(&o)
		for _, other := range firstInstallOnly {
			if other.given(&o) != (other.Name == s.Name) {
				t.Errorf("%s set: %s given = %v", s.Name, other.Name, other.given(&o))
			}
		}
	}
	if len(names) != len(given) {
		t.Errorf("first install only %v, want %d settings", names, len(given))
	}
}

// The inputs a TLS mode adds are described as the form asks for them: each mode's, in the form's
// order, on the shapes the front serves; none without a front.
func TestDescribeTLSInputs(t *testing.T) {
	t.Parallel()
	r, _ := testInstallerWith(t, "1.0.0", func(s *spec.Spec) { s.Front.TLSModes = append(s.Front.TLSModes, "dns-cloudflare") })
	d := Describe(r)
	for _, mode := range []string{"internal", "acme", "dns-cloudflare"} {
		var described, asked []string
		for _, i := range d.TLSInputs {
			if slices.Contains(i.TLSModes, mode) {
				described = append(described, i.Key)
			}
		}
		for _, i := range (&Installer{R: r}).frontInputs(mode) {
			if !i.domain {
				asked = append(asked, i.Key)
			}
		}
		if !slices.Equal(described, asked) {
			t.Errorf("%s: described %v, the form asks %v", mode, described, asked)
		}
	}
	var b bytes.Buffer
	if err := d.print(&b); err != nil {
		t.Fatal(err)
	}
	if want := "input     " + CloudflareTokenVar + " (secret, required, shapes docker,kairos, tls dns-cloudflare)\n"; !strings.Contains(b.String(), want) {
		t.Errorf("no %q in:\n%s", want, b.String())
	}
	// A mode the release does not offer asks nothing: without dns-cloudflare, no token.
	r.Spec.Front.TLSModes = slices.DeleteFunc(r.Spec.Front.TLSModes, func(m string) bool { return m == "dns-cloudflare" })
	if got := Describe(r).TLSInputs; len(got) != 1 || got[0].Key != ACMEEmailVar || !slices.Equal(got[0].TLSModes, []string{"acme"}) {
		t.Errorf("without dns-cloudflare: %+v", got)
	}
	r.Spec.Front.TLSModes = []string{"none", "internal"}
	if got := Describe(r).TLSInputs; got == nil || len(got) != 0 {
		t.Errorf("internal alone: %+v", got)
	}
	// Kubernetes alone: the chart's ingress owns the certificate.
	r.Spec.Front.TLSModes = []string{"none", "acme", "dns-cloudflare"}
	k8sOnly := *r.Spec
	k8sOnly.Shapes = spec.Shapes{Kubernetes: &spec.K8sShape{}}
	if got := describeTLSInputs(&k8sOnly); got == nil || len(got) != 0 {
		t.Errorf("kubernetes alone: %+v", got)
	}
	r.Spec.Front = spec.Front{}
	if got := Describe(r).TLSInputs; got == nil || len(got) != 0 {
		t.Errorf("without a front: %#v", got)
	}
}
