package install

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// Every built-in module holds the contract: it declares the contract's variables, outputs id and
// address, and asks the operator nothing a flag of vm create gives.
func TestEveryBuiltInModuleHoldsTheContract(t *testing.T) {
	t.Parallel()
	n := 0
	for _, p := range Platforms {
		if !p.Tofu {
			if _, err := moduleFiles(p.Key, ""); err == nil {
				t.Errorf("%s has a module and no Tofu", p.Key)
			}
			continue
		}
		n++
		files, err := moduleFiles(p.Key, "")
		if err != nil {
			t.Fatalf("%s: %v", p.Key, err)
		}
		vars, err := moduleVars(files)
		if err != nil {
			t.Fatalf("%s: %v", p.Key, err)
		}
		if err := checkContract(vars, nil); err != nil {
			t.Errorf("%s: %v", p.Key, err)
		}
		for _, out := range []string{`output "id" {`, `output "address" {`} {
			if !strings.Contains(string(files["outputs.tf"]), out) {
				t.Errorf("%s: no %s", p.Key, out)
			}
		}
		for _, v := range platformVars(vars, p.Key == "truenas") {
			if v.Description == "" {
				t.Errorf("%s: variable %s has no description to ask it with", p.Key, v.Name)
			}
		}
		if p.Preview != (p.Key != "truenas") {
			t.Errorf("%s: preview %v", p.Key, p.Preview)
		}
		// Credentials come from the environment: no module declares one.
		for _, v := range vars {
			if regexp.MustCompile(`(?i)password|secret|token|api_key|access_key`).MatchString(v.Name) {
				t.Errorf("%s declares %s", p.Key, v.Name)
			}
		}
	}
	if n != 7 {
		t.Errorf("%d built-in modules", n)
	}
}

func TestModuleVarsReadsTheDeclarations(t *testing.T) {
	t.Parallel()
	vars, err := moduleVars(map[string][]byte{
		"variables.tf": []byte(`variable "zone" {
  description = "The \"zone\"."
  type        = string
}

variable "size" {
  type    = string
  default = "s"
}

resource "x" "y" {
  variable = "not one"
}

variable "flat" {}
variable "flat_default" { default = "x" }
variable "flat_typed" { type = string }
variable "flat_quoted" { description = "no default = pick one" }
variable "flat_commented" { type = string } # a comment
variable "flat_object" { type = object({ default = string }) }
variable "object" {
  type = object({
    default = string
  })
}

variable "url" {
  description = "A # is not a comment, nor // in a string" // a comment
  default     = "https://example.org/#x"
}
`),
		"crlf.tf":      []byte("variable \"crlf\" {\r\n  default = 1\r\n}\r\n"),
		"more.tf.json": []byte(`{"variable": {"region": {"description": "R"}, "tier": {"default": null}, "count": {"default": 2}}}`),
		"README.md":    []byte(`variable "readme" {`),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []moduleVar{{"count", "", true}, {"crlf", "", true}, {"flat", "", false}, {"flat_commented", "", false}, {"flat_default", "", true},
		{"flat_object", "", false}, {"flat_quoted", "", false}, {"flat_typed", "", false}, {"object", "", false},
		{"region", "R", false}, {"size", "", true}, {"tier", "", false}, {"url", "A # is not a comment, nor // in a string", true}, {"zone", `The "zone".`, false}}
	if !slices.Equal(vars, want) {
		t.Errorf("%+v\nwant %+v", vars, want)
	}
	if _, err := moduleVars(map[string][]byte{"a.tf": []byte("variable \"x\" {\n  type = string\n")}); err == nil || !strings.Contains(err.Error(), `variable "x" is not closed`) {
		t.Errorf("an unclosed block: %v", err)
	}
	if _, err := moduleVars(map[string][]byte{"a.tf.json": []byte(`{"variable": [`)}); err == nil {
		t.Error("broken JSON passed")
	}
}

func FuzzModuleVars(f *testing.F) {
	f.Add([]byte("variable \"a\" {\n  description = \"x\"\n  default = 1\n}\n"), []byte(`{"variable":{"b":{}}}`))
	f.Add([]byte("variable \"a\" {\n"), []byte(`{}`))
	f.Fuzz(func(t *testing.T, tf, js []byte) {
		vars, err := moduleVars(map[string][]byte{"m.tf": tf, "m.tf.json": js})
		if err != nil {
			return
		}
		if !slices.IsSortedFunc(vars, func(a, b moduleVar) int { return strings.Compare(a.Name, b.Name) }) {
			t.Errorf("not sorted: %+v", vars)
		}
		for i := 1; i < len(vars); i++ {
			if vars[i].Name == vars[i-1].Name {
				t.Errorf("%s twice", vars[i].Name)
			}
		}
	})
}

// proxmoxCreate is a VM on Proxmox VE with its built-in module, a preview.
func proxmoxCreate() VMCreate {
	return VMCreate{Platform: "proxmox", Create: CreateTofu, Hostname: "notes-1", User: "ops", Ctl: "/opt/notes-stow",
		TofuVM: TofuVM{Vars: map[string]string{"node": "pve1"}}, Wait: 5 * time.Second}
}

func readVars(t *testing.T, dir string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, varsFile))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVMCreateWithABuiltInPreviewModule(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	r := newTofuRun(t)
	delete(r.vars, "TRUENAS_URL") // TrueNAS's API is not this module's
	r.tofu.outputs = `{"id": {"value": 1234567890123456789}, "address": {"value": "192.0.2.7"}}`
	if err := v.create(proxmoxCreate(), r.mod(nil)); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	dir := filepath.Join(r.state, "notes", "vms", "notes-1")
	vars := readVars(t, dir)
	if vars["name"] != "notes-1" || vars["node"] != "pve1" || vars["cpus"] != 2.0 || vars["seed"] != filepath.Join(dir, "seed.iso") || vars["pool"] != nil {
		t.Errorf("variables %v", vars)
	}
	for _, k := range contractVars {
		if _, ok := vars[k]; !ok {
			t.Errorf("no %s", k)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, platformFile)); string(b) != "proxmox builtin\n" {
		t.Errorf("platform %q", b)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "main.tf")); !strings.Contains(string(got), "proxmox_virtual_environment_vm") {
		t.Error("not the proxmox module")
	}
	// The address the module returned is the one waited for and printed.
	if a := r.ssh.calls[0]; !slices.Contains(a, "ops@192.0.2.7") {
		t.Errorf("ssh %q", a)
	}
	for _, s := range []string{"OpenTofu's Proxmox VE module is a preview", "--create manual", "ssh ops@192.0.2.7 notes-stow status"} {
		if !strings.Contains(v.out2(), s) {
			t.Errorf("no %q in:\n%s", s, v.out2())
		}
	}
	for _, s := range []string{"OpenTofu creates the VM notes-1 on Proxmox VE: 2 CPUs, 4096 MiB, a 40 GiB disk", "    node = pve1",
		"then waits for it at the address its module returns, else notes-1", "OpenTofu created the VM notes-1 (1234567890123456789)"} {
		if !strings.Contains(v.log.String(), s) {
			t.Errorf("no %q in the log:\n%s", s, v.log)
		}
	}

	// Its destroy needs no TrueNAS API, and names the platform.
	p := &fakePrompter{answers: map[string]string{"confirm": "yes"}}
	in := v.installer(r.mod(func(o *Options) { o.Prompt = p }))
	if err := in.DestroyVM(context.Background(), &VMDestroy{Host: "notes-1", Ctl: "/opt/notes-stow"}); err != nil {
		t.Fatal(err)
	}
	if title := p.forms[0].Questions[0].Title; !strings.Contains(title, "on Proxmox VE") {
		t.Errorf("asked %q", title)
	}
}

func TestVMCreateWithABuiltInModuleRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		c       func(*VMCreate)
		outputs string
		want    string
	}{
		{"a required variable missing", func(c *VMCreate) { c.Vars = nil }, "", "--tofu-var node=<value> is required by the module: The Proxmox VE node"},
		{"an unknown variable", func(c *VMCreate) { c.Vars["zone"] = "z" }, "", "--tofu-var zone: the module declares no variable zone"},
		{"a TrueNAS name", func(c *VMCreate) { c.Name = "Notes1" }, "", "lower-case letters, digits and '-'"},
		{"no id", nil, `{"address": {"value": "192.0.2.7"}}`, "the module has no output id"},
		{"an empty id", nil, `{"id": {"value": " "}}`, "the module's output id is empty"},
		{"a list for an id", nil, `{"id": {"value": ["i-1"]}}`, "the module's output id is []interface {}"},
		{"no module of its own", func(c *VMCreate) { c.Platform, c.Vars = "hyperv", nil }, "", "OpenTofu has no built-in module for Hyper-V: give your own with --module"},
		{"a size the cloud's module does not take", func(c *VMCreate) {
			c.Platform, c.CPUs, c.Vars = "aws", 8, map[string]string{"region": "r", "bucket": "b", "subnet_id": "s"}
		}, "", "--cpus: the module sizes the VM by a variable of its own"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := newVMCreate(t, "qcow2", "vhd", "raw")
			r := newTofuRun(t)
			r.tofu.outputs = tc.outputs
			c := proxmoxCreate()
			if tc.c != nil {
				tc.c(&c)
			}
			if err := v.create(c, r.mod(nil)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestVMCreateWithTheOperatorsModule(t *testing.T) {
	t.Parallel()
	mod := t.TempDir()
	contract := ""
	for _, n := range contractVars {
		contract += "variable \"" + n + "\" {\n}\n\n"
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(mod, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.tf", `resource "terraform_data" "vm" {}`+"\n")
	write("outputs.tf", `output "id" { value = "x" }`+"\n")
	write(".terraform.lock.hcl", "# lock\n")
	write("notes.txt", "not the module's\n")
	if err := os.Mkdir(filepath.Join(mod, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("sub/x.tf", "not read\n")
	shared := filepath.Join(t.TempDir(), "shared.tf")
	if err := os.WriteFile(shared, []byte("locals {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, filepath.Join(mod, "linked.tf")); err != nil {
		t.Fatal(err)
	}
	create := func(vars map[string]string) (VMCreate, *vmCreate, *tofuRun, error) {
		v := newVMCreate(t, "raw")
		r := newTofuRun(t)
		delete(r.vars, "TRUENAS_API_KEY")
		c := VMCreate{Platform: "aws", Hostname: "notes-1", User: "ops", Ctl: "/opt/notes-stow", Host: "notes-1.example.org",
			TofuVM: TofuVM{Module: mod, Vars: vars}, Wait: 5 * time.Second}
		return c, v, r, v.create(c, r.mod(nil))
	}

	// A module without the contract's variables is refused before anything is fetched.
	write("variables.tf", "variable \"name\" {\n}\n")
	if _, _, r, err := create(nil); err == nil || !strings.Contains(err.Error(), "the module does not declare description, disk_gib") || len(r.tofu.calls) != 1 {
		t.Fatalf("%v", err)
	}
	write("variables.tf", contract+"variable \"admin_user\" {\n}\n\nvariable \"ssh_public_key\" {\n}\n\nvariable \"subnet\" {\n  description = \"The subnet.\"\n}\n")
	if _, _, _, err := create(nil); err == nil || !strings.Contains(err.Error(), "--tofu-var subnet=<value> is required by the module: The subnet.") {
		t.Fatalf("%v", err)
	}
	_, v, r, err := create(map[string]string{"subnet": "subnet-1"})
	if err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	dir := filepath.Join(r.state, "notes", "vms", "notes-1")
	vars := readVars(t, dir)
	if vars["subnet"] != "subnet-1" || vars["admin_user"] != "ops" || !strings.HasPrefix(vars["ssh_public_key"].(string), "ssh-ed25519 ") ||
		vars["seed"] != filepath.Join(dir, "user-data.yaml") {
		t.Errorf("variables %v", vars)
	}
	if b, err := os.Lstat(filepath.Join(dir, "linked.tf")); err != nil || !b.Mode().IsRegular() {
		t.Errorf("the linked file is not copied as a file: %v", err)
	}
	for _, f := range []string{"main.tf", "variables.tf", "outputs.tf", ".terraform.lock.hcl"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s not copied: %v", f, err)
		}
	}
	for _, f := range []string{"notes.txt", "sub", "x.tf"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s copied", f)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, platformFile)); string(b) != "aws module\n" {
		t.Errorf("platform %q", b)
	}
	// Another VM after it: its variables, of any type, do not stop the name check.
	b, _ := os.ReadFile(filepath.Join(dir, varsFile))
	if err := os.WriteFile(filepath.Join(dir, varsFile), []byte(strings.Replace(string(b), `"subnet": "subnet-1"`, `"subnet": "subnet-1", "cpus": "two"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	c2 := VMCreate{Platform: "aws", Hostname: "notes-2", User: "ops", Ctl: "/opt/notes-stow", Host: "notes-2.example.org",
		TofuVM: TofuVM{Module: mod, Vars: map[string]string{"subnet": "subnet-1"}}, Wait: 5 * time.Second}
	if err := v.create(c2, r.mod(nil)); err != nil {
		t.Errorf("a second VM: %v", err)
	}
	if strings.Contains(v.out2(), "preview") {
		t.Errorf("the operator's module is called a preview:\n%s", v.out2())
	}
	if !strings.Contains(v.log.String(), "OpenTofu creates the VM notes-1 on Amazon EC2 with the module "+mod) {
		t.Errorf("review:\n%s", v.log)
	}
}

func TestVMCreateWithOpenTofuRefusesTrueNASVariablesAsTofuVars(t *testing.T) {
	t.Parallel()
	for _, k := range truenasVars {
		v := newVMCreate(t, "qcow2")
		r := newTofuRun(t)
		c := tofuCreate()
		c.Vars = map[string]string{k: "x"}
		if err := v.create(c, r.mod(nil)); err == nil || !strings.Contains(err.Error(), "--tofu-var "+k+": TrueNAS's built-in module takes it from its own flag") {
			t.Errorf("%s: %v", k, err)
		}
	}
}

// A cloud's module sizes the VM by its instance type: --cpus and --memory are neither asked nor
// written, and the review states no size it does not make.
func TestVMCreateOnACloudAsksNoCPUsOrMemory(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "raw")
	r := newTofuRun(t)
	p := &fakePrompter{answers: map[string]string{"region": "eu-west-3", "bucket": "b", "subnet_id": "subnet-1", "confirm": "yes"}}
	c := VMCreate{Platform: "aws", Create: CreateTofu, Hostname: "notes-1", User: "ops", Ctl: "/opt/notes-stow", Host: "notes-1.example.org", Wait: 5 * time.Second}
	if err := v.create(c, r.mod(func(o *Options) { o.Prompt = p })); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	for _, f := range p.forms {
		for _, q := range f.Questions {
			if q.Key == "--cpus" || q.Key == "--memory" {
				t.Errorf("asked %s", q.Key)
			}
		}
	}
	vars := readVars(t, filepath.Join(r.state, "notes", "vms", "notes-1"))
	if _, ok := vars["cpus"]; ok || vars["region"] != "eu-west-3" || vars["seed"] == nil {
		t.Errorf("variables %v", vars)
	}
	if !strings.Contains(v.log.String(), "OpenTofu creates the VM notes-1 on Amazon EC2: a 40 GiB disk\n") {
		t.Errorf("review:\n%s", v.log)
	}
}

// libvirt's module waits for a DHCP lease only when the VM takes one.
func TestVMCreateTellsTheModuleWhetherTheAddressComesByDHCP(t *testing.T) {
	t.Parallel()
	for _, static := range []bool{false, true} {
		v := newVMCreate(t, "qcow2")
		r := newTofuRun(t)
		c := VMCreate{Platform: "libvirt", Create: CreateTofu, Hostname: "notes-1", User: "ops", Ctl: "/opt/notes-stow", Wait: 5 * time.Second}
		if static {
			c.Network, c.Address, c.Gateway, c.DNS = "static", "192.0.2.10/24", "192.0.2.1", []string{"192.0.2.1"}
		}
		if err := v.create(c, r.mod(nil)); err != nil {
			t.Fatalf("%v\n%s", err, v.log)
		}
		if got := readVars(t, filepath.Join(r.state, "notes", "vms", "notes-1"))["dhcp"]; got != !static {
			t.Errorf("static %v: dhcp %v", static, got)
		}
	}
}

// A VM name is each platform's own: one of another platform does not claim it.
func TestVMCreateWithOpenTofuNamesArePerPlatform(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	r := newTofuRun(t)
	c := tofuCreate()
	if err := v.create(c, r.mod(nil)); err != nil {
		t.Fatal(err)
	}
	p := proxmoxCreate()
	p.Hostname, p.Name = "notes-2", "notes1" // the TrueNAS VM's name
	if err := v.create(p, r.mod(nil)); err != nil {
		t.Fatalf("another platform's name: %v", err)
	}
	p.Hostname = "notes-3"
	if err := v.create(p, r.mod(nil)); err == nil || !strings.Contains(err.Error(), "the VM notes-2, of host notes-2, is named notes1 on its platform already") {
		t.Fatalf("the same platform's name: %v", err)
	}
}
