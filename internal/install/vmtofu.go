package install

import (
	"cmp"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/norvexlabs/stowage/internal/truenas"
)

// tofuModules are the OpenTofu modules `vm create` applies, one directory per platform that has
// one.
//
//go:embed tofu/*/*.tf tofu/*/.terraform.lock.hcl
var tofuModules embed.FS

// The ways `vm create` makes a VM.
const (
	CreateTofu   = "opentofu"
	CreateManual = "manual"
)

// CtlVar names, in OpenTofu's environment, the <short>-stow its module's steps run.
const CtlVar = "STOWAGE_CTL"

// sshPoll is how often a new VM is asked whether it has installed.
var sshPoll = 10 * time.Second

// TofuVM is a VM OpenTofu creates: its name and size, where it goes on TrueNAS, and the other
// platforms' variables.
type TofuVM struct {
	Name      string // the VM's name on its platform: letters and digits on TrueNAS
	Pool      string // TrueNAS
	Dataset   string // TrueNAS, under the pool: where its zvol and staging dataset go
	Bridge    string // TrueNAS: the interface or bridge its NIC attaches to
	CPUs      int
	MemoryMiB int
	DiskGiB   int
	// Module is the operator's module directory, applied instead of the platform's built-in one.
	Module string
	// Vars are the module's own variables (--tofu-var), beside the contract's.
	Vars map[string]string
}

// tofuVars are the module's variables (variables.tf): what the state records, never a credential.
type tofuVars struct {
	Insecure    bool   `json:"insecure_skip_verify"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Pool        string `json:"pool"`
	Dataset     string `json:"parent_dataset"`
	CPUs        int    `json:"cpus"`
	MemoryMiB   int    `json:"memory_mib"`
	DiskGiB     int    `json:"disk_gib"`
	Bridge      string `json:"nic_attach"`
	Image       string `json:"image"`
	ImageSHA256 string `json:"image_sha256"`
	Seed        string `json:"seed"`
	SeedSHA256  string `json:"seed_sha256"`
}

// xdgDir is the XDG base directory env names, else home's def; a relative value is ignored, as
// the XDG spec says (OpenTofu runs in another directory than this run's).
func (o *Options) xdgDir(env, def, what string) (string, error) {
	if base := o.getenv(env); filepath.IsAbs(base) {
		return base, nil
	}
	if home := o.getenv("HOME"); filepath.IsAbs(home) {
		return filepath.Join(home, def), nil
	}
	return "", fmt.Errorf("neither %s nor HOME is an absolute path: where %s goes", env, what)
}

// vmsDir holds the VMs OpenTofu created, one directory each (stateDir):
// $XDG_STATE_HOME/<name>/vms (~/.local/state by default).
func (o *Options) vmsDir(name string) (string, error) {
	base, err := o.xdgDir("XDG_STATE_HOME", filepath.Join(".local", "state"), "a VM's OpenTofu state")
	if err != nil {
		return "", err
	}
	return filepath.Join(base, name, "vms"), nil
}

// stateDir is where a VM OpenTofu created keeps its module, variables and state.
func (o *Options) stateDir(name, host string) (string, error) {
	vms, err := o.vmsDir(name)
	if err != nil {
		return "", err
	}
	// ssh reads its known hosts file's path with escapes and % tokens: none may be in it.
	if strings.ContainsAny(vms, `%\"`) {
		return "", fmt.Errorf("%s: a VM's state directory cannot hold %%, \\ or \" (set XDG_STATE_HOME elsewhere)", vms)
	}
	return filepath.Join(vms, host), nil
}

// diskCache is where `vm create` keeps the disks it fetched for OpenTofu:
// $XDG_CACHE_HOME/<name>/disks (~/.cache by default). Fetched again only when gone.
func (o *Options) diskCache(name string) (string, error) {
	base, err := o.xdgDir("XDG_CACHE_HOME", ".cache", "the disk image")
	if err != nil {
		return "", err
	}
	return filepath.Join(base, name, "disks"), nil
}

// varsFile names a VM's variables in its state directory: there once `vm create` applied, or
// tried to — what `vm destroy` needs, and what a second create of the host refuses.
const varsFile = "terraform.tfvars.json"

// claimedBy is the host of another VM OpenTofu created on the same platform, with a built-in
// module or not as onBuiltin says, whose name is vmName, ignoring case (a TrueNAS VM's zvol and
// staging dataset are named in lowercase): "" when none.
func (o *Options) claimedBy(name, host, vmName, platform string, onBuiltin bool) (string, error) {
	vms, err := o.vmsDir(name)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(vms)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == host {
			continue
		}
		b, err := os.ReadFile(filepath.Join(vms, e.Name(), varsFile))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		// Names are each platform's own: another platform's VM never claims one.
		if key, builtin, err := readPlatform(filepath.Join(vms, e.Name())); err != nil {
			return "", err
		} else if key != platform || builtin != onBuiltin {
			continue
		}
		// The name alone: a module's other variables are of any type.
		var v struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return "", fmt.Errorf("%s: %w", filepath.Join(vms, e.Name(), varsFile), err)
		}
		if strings.EqualFold(v.Name, vmName) {
			return e.Name(), nil
		}
	}
	return "", nil
}

// creation asks how the VM is made, on a platform OpenTofu can create it on.
func (in *Installer) creation(ctx context.Context, p Platform, given, module string) (string, error) {
	if given != "" && given != CreateTofu && given != CreateManual {
		return "", fmt.Errorf("--create %s: %s or %s", given, CreateTofu, CreateManual)
	}
	if module != "" {
		if given == CreateManual {
			return "", fmt.Errorf("--module is for --create %s", CreateTofu)
		}
		return CreateTofu, nil
	}
	if !p.Tofu {
		if given == CreateTofu {
			return "", fmt.Errorf("--create %s: OpenTofu has no built-in module for %s: give your own with --module <dir>, or create it by hand", CreateTofu, p.Label)
		}
		return CreateManual, nil
	}
	if given == "" && in.O.Prompt != nil {
		tofu := "OpenTofu creates it on " + p.Label + " (`tofu` on this machine)"
		if p.Preview {
			tofu += ", with a module not run on " + p.Label + " yet (a preview)"
		}
		a, err := in.O.Prompt.Ask(ctx, Form{
			Questions: []Question{{Title: "How is the VM created", Key: "--create", Choices: []Choice{
				{CreateTofu, tofu},
				{CreateManual, "By hand: write its disk and seed, and print the steps"},
			}}},
			Defaults: func(map[string]string) map[string]string {
				if p.Preview {
					return map[string]string{"--create": CreateManual}
				}
				return map[string]string{"--create": CreateTofu}
			},
		})
		if err != nil {
			return "", err
		}
		given = a["--create"]
	}
	return cmp.Or(given, CreateManual), nil
}

// tofuReady checks what OpenTofu needs before anything is asked: `tofu`, and the TrueNAS API's
// address and key in the environment, which the provider and the module's steps read.
func (in *Installer) tofuReady(ctx context.Context, truenasAPI bool) error {
	o := &in.O
	if o.Tofu == nil {
		return errors.New("OpenTofu is not available to this run")
	}
	if _, err := o.Tofu.Run(ctx, Cmd{Args: []string{"version"}}); err != nil {
		return fmt.Errorf("--create %s runs OpenTofu's `tofu` (https://opentofu.org/docs/intro/install/): %w", CreateTofu, err)
	}
	if !truenasAPI {
		return nil
	}
	for _, k := range []string{truenas.URLVar, truenas.KeyVar} {
		if o.getenv(k) == "" {
			return fmt.Errorf("--create %s: %s is not set (the TrueNAS API's %s, read from the environment only)", CreateTofu, k,
				map[string]string{truenas.URLVar: "address, https://<host>[:<port>]", truenas.KeyVar: "key"}[k])
		}
	}
	_, err := truenas.ParseURL(o.getenv(truenas.URLVar))
	return err
}

var (
	vmName = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
	// dnsVMName is a VM name every other platform takes (Google Cloud's is the narrowest).
	dnsVMName = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
)

// tofuVM asks what the flags did not give of the VM: its name and size, and where it goes — on
// TrueNAS its pool, dataset and bridge, elsewhere the module's own variables. It returns the
// module's files and the variables they declare, its contract checked.
func (in *Installer) tofuVM(ctx context.Context, p Platform, t *TofuVM, host string) (map[string][]byte, []moduleVar, error) {
	files, err := moduleFiles(p.Key, t.Module)
	if err != nil {
		return nil, nil, err
	}
	declared, err := moduleVars(files)
	if err != nil {
		return nil, nil, err
	}
	if err := checkContract(declared, t.Vars); err != nil {
		return nil, nil, err
	}
	onTrueNAS := t.builtinTrueNAS(p)
	if !onTrueNAS && (t.Pool != "" || t.Dataset != "" || t.Bridge != "") {
		return nil, nil, errors.New("--pool, --dataset and --bridge are for TrueNAS's built-in module: give a module's own variables with --tofu-var NAME=VALUE")
	}
	for _, k := range sortedKeys(t.Vars) {
		if onTrueNAS && slices.Contains(truenasVars, k) {
			return nil, nil, fmt.Errorf("--tofu-var %s: TrueNAS's built-in module takes it from its own flag or environment (--pool, --dataset, --bridge, %s)", k, truenas.InsecureVar)
		}
	}
	sized := map[string]bool{"--cpus": declares(declared, "cpus"), "--memory": declares(declared, "memory_mib")}
	for _, f := range []struct {
		flag string
		n    int
	}{{"--cpus", t.CPUs}, {"--memory", t.MemoryMiB}} {
		if f.n != 0 && !sized[f.flag] {
			return nil, nil, fmt.Errorf("%s: the module sizes the VM by a variable of its own (an instance type, a machine type): give it with --tofu-var", f.flag)
		}
	}
	size := in.vmSize()
	num := func(least, floor int) func(string) error {
		return func(v string) error {
			n, err := strconv.Atoi(v)
			if err != nil || n < least {
				if least > floor {
					return fmt.Errorf("%q: a whole number, at least %d, the least %s runs on", v, least, in.R.Spec.Name)
				}
				return fmt.Errorf("%q: a whole number, at least %d", v, least)
			}
			return nil
		}
	}
	nonEmpty := func(v string) error {
		if strings.TrimSpace(v) != v || v == "" {
			return fmt.Errorf("%q: a name without spaces around it", v)
		}
		return nil
	}
	dataset := func(v string) error {
		if err := nonEmpty(v); err != nil {
			return err
		}
		if strings.HasPrefix(v, "/") || strings.HasSuffix(v, "/") || strings.Contains(v, "//") {
			return fmt.Errorf("%q: a dataset path under the pool, e.g. vms", v)
		}
		return nil
	}
	checkName := func(v string) error {
		if !vmName.MatchString(v) {
			return fmt.Errorf("%q: letters and digits only, as TrueNAS names a VM", v)
		}
		return nil
	}
	defName := strings.Map(func(r rune) rune {
		if r < '0' || (r > '9' && r < 'A') || (r > 'Z' && r < 'a') || r > 'z' {
			return -1
		}
		return r
	}, host)
	if !onTrueNAS {
		checkName = func(v string) error {
			if !dnsVMName.MatchString(v) {
				return fmt.Errorf("%q: lower-case letters, digits and '-', starting with a letter, at most 63 (a name every platform takes)", v)
			}
			return nil
		}
		defName = strings.ToLower(host)
	}
	itoa := func(n int) string {
		if n == 0 {
			return ""
		}
		return strconv.Itoa(n)
	}
	qs := []Question{{Title: "The VM's name on " + p.Label, Key: "--vm-name", Check: checkName}}
	if onTrueNAS {
		qs = append(qs,
			Question{Title: "The pool its disk goes on", Key: "--pool", Required: true, Check: nonEmpty},
			Question{Title: "The dataset, under the pool, its disk goes in", Key: "--dataset", Required: true, Check: dataset},
			Question{Title: "The bridge or interface its network attaches to", Key: "--bridge", Required: true, Check: nonEmpty})
	}
	if sized["--cpus"] {
		qs = append(qs, Question{Title: "Its CPUs", Key: "--cpus", Check: num(size.least.cpus, 1)})
	}
	if sized["--memory"] {
		qs = append(qs, Question{Title: "Its memory, in MiB", Key: "--memory", Check: num(size.least.mib, minVMMiB)})
	}
	qs = append(qs, Question{Title: "Its disk, in GiB", Key: "--disk", Check: num(size.least.gib, minVMGiB)})
	given := map[string]string{"--vm-name": t.Name, "--pool": t.Pool, "--dataset": t.Dataset, "--bridge": t.Bridge,
		"--cpus": itoa(t.CPUs), "--memory": itoa(t.MemoryMiB), "--disk": itoa(t.DiskGiB)}
	own := platformVars(declared, onTrueNAS)
	for _, v := range own {
		title := cmp.Or(v.Description, v.Name)
		if v.HasDefault {
			title += " (empty: the module's default)"
		}
		qs = append(qs, Question{Title: title, Key: v.Name, Required: !v.HasDefault})
		given[v.Name] = t.Vars[v.Name]
	}
	defaults := map[string]string{"--vm-name": defName, "--disk": itoa(size.def.gib)}
	if sized["--cpus"] {
		defaults["--cpus"] = itoa(size.def.cpus)
	}
	if sized["--memory"] {
		defaults["--memory"] = itoa(size.def.mib)
	}
	a, err := in.askGiven(ctx, qs, given, defaults)
	if err != nil {
		return nil, nil, err
	}
	required := []string{"--vm-name"}
	if onTrueNAS {
		required = append(required, "--pool", "--dataset", "--bridge")
	}
	for _, k := range required {
		if a[k] == "" {
			return nil, nil, fmt.Errorf("%s is required with --create %s", k, CreateTofu)
		}
	}
	t.Name, t.Pool, t.Dataset, t.Bridge = a["--vm-name"], a["--pool"], a["--dataset"], a["--bridge"]
	t.CPUs, _ = strconv.Atoi(a["--cpus"])
	t.MemoryMiB, _ = strconv.Atoi(a["--memory"])
	t.DiskGiB, _ = strconv.Atoi(a["--disk"])
	size.warn(in.O.logf, in.R.Spec.Name, *t, sized["--cpus"], sized["--memory"])
	for _, v := range own {
		switch val := a[v.Name]; {
		case val != "":
			if t.Vars == nil {
				t.Vars = map[string]string{}
			}
			t.Vars[v.Name] = val
		case !v.HasDefault:
			return nil, nil, fmt.Errorf("--tofu-var %s=<value> is required by the module: %s", v.Name, cmp.Or(v.Description, "it has no default"))
		}
	}
	// A built-in module's firewall is a bool OpenTofu would refuse only once the apply started;
	// its spellings (OpenTofu's: 1 and 0 too) are made true or false, which moduleFirewall reads.
	if fw, ok := t.Vars["firewall"]; ok && t.Module == "" && declares(declared, "firewall") {
		on, known := map[string]string{"true": "true", "1": "true", "false": "false", "0": "false"}[fw]
		if !known {
			return nil, nil, fmt.Errorf("--tofu-var firewall=%s: true or false", fw)
		}
		t.Vars["firewall"] = on
	}
	return files, declared, nil
}

// tofu runs OpenTofu in dir, its output on the log, with this run's environment (the TrueNAS
// API's address and key among it) and CtlVar naming ctl.
func (in *Installer) tofu(ctx context.Context, dir, ctl string, args ...string) error {
	env := append(in.O.environ(), CtlVar+"="+ctl, "TF_IN_AUTOMATION=1", "TF_INPUT=0")
	_, err := in.O.Tofu.Run(ctx, Cmd{Args: append([]string{"-chdir=" + dir}, args...), Env: env, Stream: in.O.Log})
	return err
}

// modulePort is a port the VM's firewall opens to some source, for a module's own firewall.
type modulePort struct {
	Protocol string `json:"protocol"` // tcp or udp
	Port     int    `json:"port"`
}

// openedPorts are the ports the VM's firewall opens besides SSH: the front's, and each L4
// route's, whose allowlist the VM's firewall alone applies.
func (in *Installer) openedPorts(st *Settings) []modulePort {
	ports := []modulePort{}
	if in.R.Spec.HasFront() {
		ports = append(ports, modulePort{"tcp", cmp.Or(st.HTTPPort, in.R.Spec.Front.HTTPPort, 80)}, modulePort{"tcp", cmp.Or(st.HTTPSPort, in.R.Spec.Front.HTTPSPort, 443)})
	}
	for _, r := range in.R.Spec.L4.Routes {
		ports = append(ports, modulePort{r.Protocol, r.Port})
	}
	return ports
}

// moduleFirewall warns when a built-in module's network firewall leaves out what the VM's opens:
// with `firewall` off, the network's own rules must let SSH and the opened ports reach it; with
// it on, SSH's rule takes IPv4 sources alone, so sources all IPv6 make none.
func (in *Installer) moduleFirewall(p Platform, t *TofuVM, mach Machine, st *Settings, declared []moduleVar) {
	if t.Module != "" || !declares(declared, "firewall") {
		return
	}
	if t.Vars["firewall"] == "true" {
		if len(mach.SSHAllow) > 0 && !slices.ContainsFunc(mach.SSHAllow, func(a netip.Prefix) bool { return a.Addr().Is4() }) {
			in.O.logf("warning: SSH's sources name no IPv4 address, and %s's SSH rule takes IPv4 alone: none is made, so the network's own rules must let tcp/22 reach the VM (--ssh-allow with an IPv4 source makes it)", p.Label)
		}
		return
	}
	open := []string{"tcp/22"}
	for _, o := range in.openedPorts(st) {
		open = append(open, fmt.Sprintf("%s/%d", o.Protocol, o.Port))
	}
	in.O.logf("warning: no firewall rule is made on %s: the network's own rules must let %s reach the VM (its own firewall then narrows them); --tofu-var firewall=true makes them, if the account may create firewall rules", p.Label, strings.Join(open, ", "))
}

// applyVM writes the module into dir with its variables, and the platform it was made for, then
// applies it: the VM is created, its disk written from image, its seed attached, and it is
// started. It returns the module's outputs: the VM's id, and its address when the platform tells.
func (in *Installer) applyVM(ctx context.Context, dir, ctl string, p Platform, mach Machine, st *Settings, t *TofuVM, files map[string][]byte, declared []moduleVar, image, format, imageSHA, seed string) (id, addr string, err error) {
	seedSHA, err := truenas.SHA256File(seed)
	if err != nil {
		return "", "", err
	}
	m := in.R.Manifest
	vars := map[string]any{
		"name": t.Name, "description": fmt.Sprintf("%s %s (revision %d), created by %s vm create", in.R.Spec.Name, m.ProjectVersion, m.Revision, in.R.Ctl()),
		"disk_gib": t.DiskGiB, "image": image, "image_sha256": imageSHA, "seed": seed, "seed_sha256": seedSHA,
	}
	if t.builtinTrueNAS(p) {
		vars["insecure_skip_verify"] = truenas.Insecure(in.O.getenv(truenas.InsecureVar))
		vars["pool"], vars["parent_dataset"], vars["nic_attach"] = t.Pool, t.Dataset, t.Bridge
	}
	for _, v := range declared {
		switch v.Name {
		case "cpus":
			vars[v.Name] = t.CPUs
		case "memory_mib":
			vars[v.Name] = t.MemoryMiB
		case "image_format":
			vars[v.Name] = format
		case "dhcp":
			vars[v.Name] = mach.staticAddr() == ""
		case "admin_user":
			vars[v.Name] = mach.User
		case "ssh_public_key":
			vars[v.Name] = mach.SSHKey
		case "ssh_sources":
			src := []string{"0.0.0.0/0", "::/0"}
			if len(mach.SSHAllow) > 0 {
				src = nil
				for _, a := range mach.SSHAllow {
					src = append(src, a.String())
				}
			}
			vars[v.Name] = src
		case "open_ports":
			vars[v.Name] = in.openedPorts(st)
		}
	}
	for k, v := range t.Vars {
		vars[k] = v
	}
	in.moduleFirewall(p, t, mach, st, declared)
	// The platform first: a state with variables is one `vm destroy` must know the platform of.
	how := "module"
	if t.Module == "" {
		how = "builtin"
	}
	if err := writeAtomic(filepath.Join(dir, platformFile), strings.NewReader(p.Key+" "+how+"\n"), 0o644, nil); err != nil {
		return "", "", err
	}
	if err := writeModule(dir, files, vars); err != nil {
		return "", "", err
	}
	if err := in.tofu(ctx, dir, ctl, "init", "-input=false", "-no-color"); err != nil {
		return "", "", err
	}
	if err := in.tofu(ctx, dir, ctl, "apply", "-input=false", "-auto-approve", "-no-color"); err != nil {
		return "", "", err
	}
	return in.tofuOutputs(ctx, dir, ctl)
}

// sshArgs are ssh's arguments to run command on the VM at addr as user: in batch mode (no
// prompt), with the private key next to the public one given when there is one, else the agent's
// key of that public one alone (ssh's other keys would spend the VM's few authentication tries),
// and the host key taken on first contact into the file known, apart from the operator's known
// hosts.
func sshArgs(known, user, addr, pubKey string, command ...string) []string {
	return sshArgsChecking("accept-new", known, user, addr, pubKey, command...)
}

// sshArgsChecking are sshArgs with ssh's StrictHostKeyChecking set to checking.
func sshArgsChecking(checking, known, user, addr, pubKey string, command ...string) []string {
	// Quoted: ssh splits an option's value at white space, into several files.
	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=" + checking,
		"-o", `UserKnownHostsFile="` + known + `"`}
	pub := expandHome(pubKey)
	if priv, ok := strings.CutSuffix(pub, ".pub"); ok {
		if _, err := os.Stat(priv); err == nil {
			args = append(args, "-i", priv, "-o", "IdentitiesOnly=yes")
		} else if _, err := os.Stat(pub); err == nil {
			args = append(args, "-i", pub, "-o", "IdentitiesOnly=yes")
		}
	}
	return append(append(args, user+"@"+addr, "--"), command...)
}

// seedHostAlias is the name a pinned known_hosts file gives the VM's one-time host key.
const seedHostAlias = "vm-seed-host-key"

// pinnedSSHArgs are sshArgs, the VM's host key checked against the file pin's alone (its line
// names seedHostAlias): ssh refuses any other, never adding one.
func pinnedSSHArgs(pin, user, addr, pubKey string, command ...string) []string {
	return append([]string{"-o", "HostKeyAlias=" + seedHostAlias}, sshArgsChecking("yes", pin, user, addr, pubKey, command...)...)
}

// awaitVM waits, up to wait, until `<short>-stow status --json` on the VM at addr answers from a
// system that is not Kairos's recovery and — unless the install waits for a secret input —
// reports the install. A first boot runs the recovery system, then resets and reboots into the
// active one, each with its own SSH host key: every attempt takes the key into a file of its own,
// and the VM's known_hosts in dir is the one of the attempt that answered. With pin, a known_hosts
// file of the host key the VM serves (pinnedSSHArgs), every attempt takes that key alone.
func (in *Installer) awaitVM(ctx context.Context, dir, pin, user, addr, pubKey string, wait time.Duration, installs bool) error {
	o, ctl := &in.O, in.R.Ctl()
	if o.SSH == nil {
		return errors.New("ssh is not available to this run")
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	o.logf("waiting for %s@%s to answer over SSH%s (up to %s)", user, addr, map[bool]string{true: ", then to install"}[installs], wait)
	var last error
	known, attempt := filepath.Join(dir, "known_hosts"), filepath.Join(dir, "known_hosts.attempt")
	for {
		if err := os.Remove(attempt); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		args := sshArgs(attempt, user, addr, pubKey, ctl, "status", "--json")
		if pin != "" {
			args = pinnedSSHArgs(pin, user, addr, pubKey, ctl, "status", "--json")
		}
		out, err := o.SSH.Run(ctx, Cmd{Args: args})
		if err == nil {
			var st Status
			if err = json.Unmarshal(out, &st); err == nil {
				switch {
				case st.Boot == "recovery":
					err = errors.New("the recovery system answers: the first boot is not done")
				case installs && st.InstallFailed != "":
					if rerr := os.Rename(attempt, known); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
						return rerr
					}
					return fmt.Errorf("%s@%s: the first install failed: %s (`ssh %s@%s sudo journalctl -u %s` tells more); `ssh %s@%s sudo %s install` runs it again; the VM is kept: `%s vm destroy %s` removes it",
						user, addr, st.InstallFailed, user, addr, in.R.Spec.Name, user, addr, ctl, ctl, filepath.Base(dir))
				case installs && st.Installed == nil:
					err = errors.New("not installed yet")
				case installs && st.Installing:
					err = errors.New("the first install has not finished: its stack is coming up, or the provision hook runs")
				default:
					if err := os.Rename(attempt, known); err != nil && !errors.Is(err, os.ErrNotExist) {
						return err
					}
					return nil
				}
			}
		}
		if ctx.Err() == nil || last == nil { // an attempt cut by the deadline says nothing new
			last = err
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("%s@%s: not ready after %s (last: %v); the VM is kept: look at its console on TrueNAS, or `%s vm destroy %s`", user, addr, wait, last, ctl, filepath.Base(dir))
			}
			return fmt.Errorf("%w; the VM is kept: `%s vm destroy %s` removes it", ctx.Err(), ctl, filepath.Base(dir))
		case <-time.After(sshPoll):
		}
	}
}

// VMDestroy is a run of `vm destroy`.
type VMDestroy struct {
	Host string
	Yes  bool // destroy without asking
	Ctl  string
}

// DestroyVM destroys a VM `vm create` made with OpenTofu — the VM, its disk and its uploads on
// TrueNAS — then removes its state directory. The disk image fetched stays in the cache.
func (in *Installer) DestroyVM(ctx context.Context, d *VMDestroy) error {
	o, name := &in.O, in.R.Spec.Name
	if err := checkHostname(d.Host); err != nil {
		return err
	}
	dir, err := o.stateDir(name, d.Host)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, varsFile)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no VM %s made by `%s vm create --create %s`: %s holds no state", d.Host, in.R.Ctl(), CreateTofu, dir)
		}
		return err
	}
	key, builtin, err := readPlatform(dir)
	if err != nil {
		return err
	}
	if err := in.tofuReady(ctx, builtinTrueNAS(key, builtin)); err != nil {
		return err
	}
	label := key
	if i := slices.IndexFunc(Platforms, func(p Platform) bool { return p.Key == key }); i >= 0 {
		label = Platforms[i].Label
	}
	if !d.Yes {
		if o.Prompt == nil {
			return fmt.Errorf("destroying %s deletes its disk: give --yes", d.Host)
		}
		a, err := o.Prompt.Ask(ctx, Form{
			Questions: []Question{{Title: fmt.Sprintf("Destroy the VM %s: its disk, its data and its uploads on %s", d.Host, label), Key: "confirm",
				Choices: []Choice{{"no", "No, keep it"}, {"yes", "Yes, destroy it"}}}},
			Defaults: func(map[string]string) map[string]string { return map[string]string{"confirm": "no"} },
		})
		if err != nil {
			return err
		}
		if a["confirm"] != "yes" {
			return ErrAborted
		}
	}
	if err := in.tofu(ctx, dir, d.Ctl, "init", "-input=false", "-no-color"); err != nil {
		return err
	}
	if err := in.tofu(ctx, dir, d.Ctl, "destroy", "-input=false", "-auto-approve", "-no-color"); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "Destroyed %s.\n", d.Host)
	return nil
}

// truenasCommand is `vm truenas`, hidden: the steps of the TrueNAS module OpenTofu runs, each
// reading its arguments and the API's address and key from the environment.
func truenasCommand(getenv func(string) string) *cobra.Command {
	t := &cobra.Command{
		Use:    "truenas",
		Short:  "The steps OpenTofu's TrueNAS module runs (TRUENAS_URL, TRUENAS_API_KEY, TRUENAS_INSECURE_SKIP_VERIFY)",
		Hidden: true,
		Args:   cobra.NoArgs,
	}
	need := func(keys ...string) error {
		for _, k := range keys {
			if getenv(k) == "" {
				return fmt.Errorf("%s is not set", k)
			}
		}
		return nil
	}
	vmID := func() (int64, error) {
		id, err := strconv.ParseInt(getenv("VM_ID"), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("VM_ID %q: %w", getenv("VM_ID"), err)
		}
		return id, nil
	}
	step := func(use, short string, keys []string, f func(context.Context, *truenas.Client) error) *cobra.Command {
		return &cobra.Command{
			Use: use, Short: short, Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				if err := need(keys...); err != nil {
					return err
				}
				c, err := truenas.FromEnv(cmd.Context(), getenv)
				if err != nil {
					return err
				}
				defer c.Close()
				return f(cmd.Context(), c)
			},
		}
	}
	t.AddCommand(
		step("import-image", "Upload IMAGE (checked against IMAGE_SHA256) into STAGING_DIR and write it into the zvol ZVOL",
			[]string{"IMAGE", "IMAGE_SHA256", "STAGING_DIR", "ZVOL"}, func(ctx context.Context, c *truenas.Client) error {
				return c.ImportImage(ctx, getenv("IMAGE"), getenv("IMAGE_SHA256"), getenv("STAGING_DIR"), getenv("ZVOL"))
			}),
		step("seed", "Upload the seed ISO SEED to SEED_PATH", []string{"SEED", "SEED_PATH"}, func(ctx context.Context, c *truenas.Client) error {
			return c.Upload(ctx, getenv("SEED"), getenv("SEED_PATH"))
		}),
		step("start", "Start the VM VM_ID unless it runs", []string{"VM_ID"}, func(ctx context.Context, c *truenas.Client) error {
			id, err := vmID()
			if err != nil {
				return err
			}
			return c.Start(ctx, id)
		}),
		step("stop", "Power the VM VM_ID off when it runs", []string{"VM_ID"}, func(ctx context.Context, c *truenas.Client) error {
			id, err := vmID()
			if err != nil {
				return err
			}
			return c.Stop(ctx, id)
		}),
		step("drop-snapshots", "Delete every snapshot of DATASET", []string{"DATASET"}, func(ctx context.Context, c *truenas.Client) error {
			return c.DropSnapshots(ctx, getenv("DATASET"))
		}),
	)
	return t
}
