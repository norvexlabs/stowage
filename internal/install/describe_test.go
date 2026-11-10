package install

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
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
