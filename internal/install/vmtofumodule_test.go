package install

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/spec"
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
		// Its lock file holds the providers' checksums, of the provider and release versions.tf pins.
		pins := regexp.MustCompile(`(?m)^\s+source\s+=\s+"([^"]+)"\n\s+version\s+=\s+"([^"]+)"`).FindAllStringSubmatch(string(files["versions.tf"]), -1)
		lock := string(files[".terraform.lock.hcl"])
		if len(pins) == 0 || !strings.Contains(lock, "h1:") {
			t.Errorf("%s: no pin (%d) or no checksums in its lock file", p.Key, len(pins))
		}
		for _, pin := range pins {
			if !strings.Contains(lock, "provider \"registry.opentofu.org/"+pin[1]+"\" {\n  version     = \""+pin[2]+"\"") {
				t.Errorf("%s: its lock file is not of %s %s", p.Key, pin[1], pin[2])
			}
		}
		// An image module declares the image's variables and others of the VM module alone (given
		// its values), outputs image_id, which the VM module takes, and pins the same providers.
		img, err := imageFiles(p.Key)
		if err != nil {
			t.Fatalf("%s: %v", p.Key, err)
		}
		if clouds := []string{"aws", "azure", "gcp", "scaleway"}; (img != nil) != slices.Contains(clouds, p.Key) {
			t.Errorf("%s: an image module %v", p.Key, img != nil)
		}
		if img != nil {
			ivars, err := moduleVars(img)
			if err != nil {
				t.Fatalf("%s image: %v", p.Key, err)
			}
			for _, n := range imageVars {
				if !declares(ivars, n) {
					t.Errorf("%s image: no variable %s", p.Key, n)
				}
			}
			for _, v := range ivars {
				if !slices.Contains(imageVars, v.Name) && !declares(platformVars(vars, false), v.Name) {
					t.Errorf("%s image: variable %s is none of the VM module's own", p.Key, v.Name)
				}
			}
			if !strings.Contains(string(img["outputs.tf"]), `output "image_id" {`) || !declares(vars, "image_id") {
				t.Errorf("%s: the image module's image_id is not the VM module's", p.Key)
			}
			ipins := regexp.MustCompile(`(?m)^\s+source\s+=\s+"([^"]+)"\n\s+version\s+=\s+"([^"]+)"`).FindAllStringSubmatch(string(img["versions.tf"]), -1)
			if string(img[".terraform.lock.hcl"]) != lock || fmt.Sprint(ipins) != fmt.Sprint(pins) {
				t.Errorf("%s image: its providers or lock are not the VM module's", p.Key)
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
	if n != 8 {
		t.Errorf("%d built-in modules", n)
	}
}

// Scaleway's module gives the cloud-config as the cloud-init user data (the key the OS bundle's
// datasource reads), makes a security group dropping what it does not accept unless one is given,
// and never replaces the instance for a later image or cloud-config.
func TestTheScalewayModule(t *testing.T) {
	t.Parallel()
	files, err := moduleFiles("scaleway", "")
	if err != nil {
		t.Fatal(err)
	}
	main := string(files["main.tf"])
	for _, want := range []string{"cloud-init = file(var.seed)", `inbound_default_policy  = "drop"`, `count                   = var.security_group_id == "" ? 1 : 0`,
		"ignore_changes = [image, user_data]", `volume_type = "sbs_volume"`, "size_in_gb  = ceil(var.disk_gib * 1.073741824)", "image             = var.image_id"} {
		if !strings.Contains(main, want) {
			t.Errorf("main.tf lacks %q", want)
		}
	}
}

// Each cloud's module takes an address reserved beforehand, making none of its own then, and
// gives it as its address.
func TestTheCloudModulesTakeAReservedAddress(t *testing.T) {
	t.Parallel()
	for key, want := range map[string][]string{
		"scaleway": {`count = var.ip_id == "" ? 1 : 0`, `ip_id             = local.ip_id`, "data.scaleway_instance_ip.given[0].address", "value       = local.address", `variable "ip_id"`},
		"aws":      {`count         = var.eip_allocation_id == "" ? 0 : 1`, "allocation_id = var.eip_allocation_id", "data.aws_eip.given[0].public_ip", `variable "eip_allocation_id"`},
		"azure":    {`count               = var.public_ip_name == "" ? 1 : 0`, "public_ip_address_id          = local.public_ip_id", "data.azurerm_public_ip.given[0].ip_address", "value       = local.address", `variable "public_ip_name"`},
		"gcp":      {`nat_ip = var.nat_ip == "" ? null : var.nat_ip`, `variable "nat_ip"`},
	} {
		files, err := moduleFiles(key, "")
		if err != nil {
			t.Fatal(err)
		}
		all := string(files["main.tf"]) + string(files["outputs.tf"]) + string(files["variables.tf"])
		for _, w := range want {
			if !strings.Contains(all, w) {
				t.Errorf("%s lacks %q", key, w)
			}
		}
	}
}

// A cloud's disk is made an image by its image module, applied first in a state the VMs of that
// disk in one place share: a second VM finds it there, another bucket gets its own, the VM's
// module boots the image_id it gave, and `vm destroy` leaves it.
func TestTheCloudImageIsSharedAndKeptByDestroy(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	r := newTofuRun(t)
	delete(r.vars, "TRUENAS_URL")
	r.tofu.outputs = `{"id": {"value": "1"}, "address": {"value": "192.0.2.7"}}`
	images := filepath.Join(r.state, "notes", "images")
	create := func(host, bucket string) string {
		t.Helper()
		r.tofu.calls = nil
		c := VMCreate{Platform: "scaleway", Create: CreateTofu, Hostname: host, User: "ops", Ctl: "/opt/notes-stow", Wait: 5 * time.Second,
			Vars: map[string]string{"zone": "fr-par-1", "bucket": bucket}}
		if err := v.create(c, r.mod(nil)); err != nil {
			t.Fatalf("%v\n%s", err, v.log)
		}
		var dirs []string
		for _, c := range r.tofu.calls {
			if dir, ok := strings.CutPrefix(c[0], "-chdir="); ok && len(c) > 1 && c[1] == "apply" {
				dirs = append(dirs, dir)
			}
		}
		if len(dirs) != 2 || filepath.Dir(dirs[0]) != images || dirs[1] != filepath.Join(r.state, "notes", "vms", host) {
			t.Fatalf("applied %q: want the image's state, then the VM's", dirs)
		}
		ivars := readVars(t, dirs[0])
		if ivars["name"] != "notes" || ivars["zone"] != "fr-par-1" || ivars["bucket"] != bucket || ivars["image_sha256"] == nil || ivars["ssh_sources"] != nil ||
			"scaleway-"+fmt.Sprint(ivars["state_key"]) != filepath.Base(dirs[0]) {
			t.Errorf("the image module's variables: %v", ivars)
		}
		if got := readVars(t, dirs[1])["image_id"]; got != "image-"+filepath.Base(dirs[0]) {
			t.Errorf("the VM's image_id %v", got)
		}
		return dirs[0]
	}
	first := create("notes-1", "b")
	if !strings.HasPrefix(filepath.Base(first), "scaleway-") {
		t.Errorf("the image's state %s", first)
	}
	if again := create("notes-2", "b"); again != first {
		t.Errorf("a second VM of the disk applied %s, not the shared %s", again, first)
	}
	if other := create("notes-3", "c"); other == first {
		t.Errorf("another bucket shares %s", first)
	}
	r.tofu.calls = nil
	in := v.installer(r.mod(nil))
	if err := in.DestroyVM(context.Background(), &VMDestroy{Host: "notes-1", Yes: true, Ctl: "/opt/notes-stow"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.tofu.calls {
		if strings.HasPrefix(c[0], "-chdir="+images) {
			t.Errorf("vm destroy ran %q on the image", c)
		}
	}
	if _, err := os.Stat(filepath.Join(first, varsFile)); err != nil {
		t.Errorf("the image's state after vm destroy: %v", err)
	}
	if !strings.Contains(v.out2(), "The disk's image stays, for the next VM of it: each state in "+images+" holds one") {
		t.Errorf("out:\n%s", v.out2())
	}
}

// libvirt's module states ACPI (libvirt refuses a UEFI domain without it on x86_64) and gives
// each disk by its volume's path: AppArmor's virt-aa-helper resolves a file disk and its backing
// chain into the domain's profile, never a pool volume.
func TestTheLibvirtModuleBootsUnderUEFIAndAppArmor(t *testing.T) {
	t.Parallel()
	files, err := moduleFiles("libvirt", "")
	if err != nil {
		t.Fatal(err)
	}
	main := string(files["main.tf"])
	if !regexp.MustCompile(`(?s)features = \{\s*acpi = true\s*apic = \{\}\s*\}`).MatchString(main) || !strings.Contains(main, `firmware     = "efi"`) {
		t.Error("the UEFI domain does not state ACPI and the APIC")
	}
	if strings.Contains(main, "volume = { pool") {
		t.Error("a disk is a pool volume, which AppArmor's virt-aa-helper does not resolve")
	}
	for _, v := range []string{"disk", "seed"} {
		if !strings.Contains(main, "file = { file = libvirt_volume."+v+".path }") {
			t.Errorf("the %s is not given by its path", v)
		}
	}
	// The firmware's features in libvirt's order, by name: the provider compares them by position
	// with what libvirt returns, and fails the apply on another order.
	names := regexp.MustCompile(`\{ name = "([^"]+)", enabled = "no" \}`).FindAllStringSubmatch(main, -1)
	var got []string
	for _, n := range names {
		got = append(got, n[1])
	}
	if !slices.Equal(got, []string{"enrolled-keys", "secure-boot"}) {
		t.Errorf("the firmware's features are %q, not in libvirt's order", got)
	}
	// The seed volume's format is the one libvirt probes on an ISO 9660 image, or a plan drifts.
	if !regexp.MustCompile(`(?s)resource "libvirt_volume" "seed" \{.*?format = \{ type = "iso" \}`).MatchString(main) {
		t.Error("the seed volume is not of format iso")
	}
	// KVM by default; QEMU's emulation, on a host without it, with the CPU it emulates.
	vars := string(files["variables.tf"])
	if !strings.Contains(main, "type        = var.domain_type") || !strings.Contains(main, `mode = var.domain_type == "kvm" ? "host-passthrough" : "maximum"`) ||
		!regexp.MustCompile(`(?s)variable "domain_type" \{.*?default     = "kvm".*?contains\(\["kvm", "qemu"\], var.domain_type\)`).MatchString(vars) {
		t.Error("the domain's type is not domain_type, kvm by default")
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
	if vars["name"] != "notes-1" || vars["node"] != "pve1" || vars["cpus"] != 2.0 || vars["image_format"] != "qcow2" || vars["seed"] != filepath.Join(dir, "seed.iso") || vars["pool"] != nil {
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
	for _, s := range []string{"OpenTofu creates the VM notes-1 on Proxmox VE: 2 CPUs, 4096 MiB, a 43 GiB disk (the OS 23 GiB, the app 20 GiB)", "    node = pve1",
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

func TestTheGCPModuleIsGivenSSHsSourcesAndTheOpenedPorts(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "gce")
	v.r.Spec.L4.Routes = append(v.r.Spec.L4.Routes, spec.L4Route{Name: "dns", Protocol: "udp", Port: 5353, AllowInput: "DNS_SOURCES"})
	v.r.Spec.Inputs = append(v.r.Spec.Inputs, spec.Input{Key: "DNS_SOURCES", Default: "192.0.2.0/24"})
	// A secret list is not given to vm create: its port opens to every source, and no fixed rule is told.
	v.r.Spec.L4.Routes = append(v.r.Spec.L4.Routes, spec.L4Route{Name: "hidden", Protocol: "tcp", Port: 5454, AllowInput: "HIDDEN_SOURCES"})
	v.r.Spec.Inputs = append(v.r.Spec.Inputs, spec.Input{Key: "HIDDEN_SOURCES", Secret: true})
	r := newTofuRun(t)
	delete(r.vars, "TRUENAS_URL")
	r.tofu.outputs = `{"id": {"value": "1"}, "address": {"value": "192.0.2.7"}}`
	c := VMCreate{Platform: "gcp", Create: CreateTofu, Hostname: "notes-1", User: "ops", Ctl: "/opt/notes-stow", Wait: 5 * time.Second,
		Vars: map[string]string{"project": "p", "zone": "z", "bucket": "b"}}
	if err := v.create(c, r.mod(nil)); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	vars := readVars(t, filepath.Join(r.state, "notes", "vms", "notes-1"))
	if got := fmt.Sprint(vars["ssh_sources"]); got != "[198.51.100.40/32]" {
		t.Errorf("ssh_sources %s", got)
	}
	if !strings.Contains(v.log.String(), "l4 route dns: the module's rule for udp/5353 takes the sources DNS_SOURCES holds now; a later change of DNS_SOURCES on the VM does not reach it") {
		t.Errorf("not told the rule is fixed:\n%s", v.log)
	}
	if strings.Contains(v.log.String(), "l4 route hidden:") {
		t.Errorf("a secret list told as the rule's:\n%s", v.log)
	}
	// The public address is no route's source behind a NAT: not compared with it.
	if strings.Contains(v.log.String(), "--ssh-allow leaves out") {
		t.Errorf("warned:\n%s", v.log)
	}
	if got := fmt.Sprint(vars["open_ports"]); got != "[map[port:80 protocol:tcp sources:[0.0.0.0/0 ::/0]] map[port:443 protocol:tcp sources:[0.0.0.0/0 ::/0]] map[port:5353 protocol:udp sources:[192.0.2.0/24]] map[port:5454 protocol:tcp sources:[0.0.0.0/0 ::/0]]]" {
		t.Errorf("open_ports %s", got)
	}
	// No firewall rule unless asked: the network's own rules are the operator's to open.
	if !strings.Contains(v.log.String(), "warning: no firewall rule is made on Google Cloud: the network's own rules must let tcp/22, tcp/80, tcp/443, udp/5353, tcp/5454 reach the VM") {
		t.Errorf("no warning:\n%s", v.log)
	}
	// Every source when told so, the front's ports as the install publishes them, the rules asked.
	v = newVMCreate(t, "gce")
	r = newTofuRun(t)
	delete(r.vars, "TRUENAS_URL")
	r.tofu.outputs = `{"id": {"value": "1"}, "address": {"value": "192.0.2.7"}}`
	c.SSHAllow = []string{"0.0.0.0/0", "::/0"}
	c.Vars["firewall"] = "true"
	if err := v.create(c, r.mod(func(o *Options) { o.HTTPPort, o.HTTPSPort = 8080, 8443 })); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	vars = readVars(t, filepath.Join(r.state, "notes", "vms", "notes-1"))
	if got := fmt.Sprint(vars["ssh_sources"], vars["open_ports"]); got != "[0.0.0.0/0 ::/0] [map[port:8080 protocol:tcp sources:[0.0.0.0/0 ::/0]] map[port:8443 protocol:tcp sources:[0.0.0.0/0 ::/0]]]" {
		t.Errorf("variables %s", got)
	}
	if vars["firewall"] != "true" || strings.Contains(v.log.String(), "no firewall rule") {
		t.Errorf("firewall %v:\n%s", vars["firewall"], v.log)
	}
}

func TestTheGCPModulesFirewallIsOptIn(t *testing.T) {
	t.Parallel()
	const off = "warning: no firewall rule is made on Google Cloud"
	for _, tc := range []struct {
		name, platform, firewall, warn string
		allow                          []string
	}{
		{"off when told so", "gcp", "false", off, nil},
		{"off, in OpenTofu's other spelling", "gcp", "0", off, nil},
		{"on, in OpenTofu's other spelling", "gcp", "1", "", nil},
		{"on, with IPv4 sources", "gcp", "true", "", []string{"192.0.2.0/24", "2001:db8::/32"}},
		{"on, with IPv6 sources alone", "gcp", "true", "warning: SSH's sources name no IPv4 address, and Google Cloud's SSH rule takes IPv4 alone", []string{"2001:db8::/32"}},
		{"a module without it", "aws", "", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := newVMCreate(t, "gce", "raw")
			r := newTofuRun(t)
			delete(r.vars, "TRUENAS_URL")
			r.tofu.outputs = `{"id": {"value": "1"}, "address": {"value": "192.0.2.7"}}`
			c := VMCreate{Platform: tc.platform, Create: CreateTofu, Hostname: "notes-1", User: "ops", Ctl: "/opt/notes-stow", Wait: 5 * time.Second,
				Vars: map[string]string{"project": "p", "zone": "z", "bucket": "b"}, SSHAllow: tc.allow}
			if tc.platform == "aws" {
				c.Vars = map[string]string{"region": "r", "bucket": "b", "subnet_id": "s", "instance_type": "t3.medium"}
			}
			if tc.firewall != "" {
				c.Vars["firewall"] = tc.firewall
			}
			if err := v.create(c, r.mod(nil)); err != nil {
				t.Fatalf("%v\n%s", err, v.log)
			}
			if got := v.log.String(); tc.warn == "" && (strings.Contains(got, "warning: no firewall rule") || strings.Contains(got, "name no IPv4 address")) || tc.warn != "" && !strings.Contains(got, tc.warn) ||
				tc.warn != "" && strings.Count(got, "warning: no firewall rule")+strings.Count(got, "warning: SSH's sources name no IPv4") != 1 {
				t.Errorf("want %q:\n%s", tc.warn, got)
			}
		})
	}
	// A value OpenTofu would refuse mid-apply is refused before.
	v := newVMCreate(t, "gce")
	r := newTofuRun(t)
	delete(r.vars, "TRUENAS_URL")
	c := VMCreate{Platform: "gcp", Create: CreateTofu, Hostname: "notes-1", User: "ops", Ctl: "/opt/notes-stow", Wait: 5 * time.Second,
		Vars: map[string]string{"project": "p", "zone": "z", "bucket": "b", "firewall": "yes"}}
	if err := v.create(c, r.mod(nil)); err == nil || err.Error() != "--tofu-var firewall=yes: true or false" {
		t.Fatalf("err = %v", err)
	}
	if len(r.tofu.calls) != 1 {
		t.Errorf("OpenTofu ran: %v", r.tofu.calls)
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
	write("variables.tf", contract+"variable \"admin_user\" {\n}\n\nvariable \"ssh_public_key\" {\n}\n\nvariable \"subnet\" {\n  description = \"The subnet.\"\n}\n\nvariable \"firewall\" {\n  default = \"\"\n}\n")
	if _, _, _, err := create(nil); err == nil || !strings.Contains(err.Error(), "--tofu-var subnet=<value> is required by the module: The subnet.") {
		t.Fatalf("%v", err)
	}
	// The operator's own `firewall` is neither checked nor warned of: only a built-in module's is a bool.
	_, v, r, err := create(map[string]string{"subnet": "subnet-1", "firewall": "sg-1"})
	if err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	dir := filepath.Join(r.state, "notes", "vms", "notes-1")
	vars := readVars(t, dir)
	if vars["subnet"] != "subnet-1" || vars["firewall"] != "sg-1" || strings.Contains(v.log.String(), "firewall rule") || vars["admin_user"] != "ops" || !strings.HasPrefix(vars["ssh_public_key"].(string), "ssh-ed25519 ") ||
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
	if _, ok := vars["cpus"]; ok || vars["image_format"] != nil || vars["region"] != "eu-west-3" || vars["seed"] == nil {
		t.Errorf("variables %v", vars)
	}
	if !strings.Contains(v.log.String(), "OpenTofu creates the VM notes-1 on Amazon EC2: a 43 GiB disk (the OS 23 GiB, the app 20 GiB)\n") {
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
