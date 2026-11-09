package install

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/kairos"
	"github.com/norvexlabs/stowage/internal/spec"
)

// A NIC attached to an installed VM (`vm attach`, `network attach`) is recorded in the install
// directory (interfacesFile) and rendered as a cloud-config of the OEM partition, which every boot
// runs as it runs the seed's: its .link file in the initramfs stage, before udev names the NICs,
// and its addressing in the boot stage. The seed's own files are left as written: a NIC the seed
// named by its original name alone (the only one it knew) is named by its MAC too, ahead of the
// seed's file, so the new NIC never takes its name.

// interfacesFile is the record of the NICs attached to the VM, in its install directory.
const interfacesFile = "interfaces.yaml"

// oemDir and sysNetDir are the OEM partition every boot reads cloud-configs from and the kernel's
// NICs; tests move them.
var (
	oemDir    = "/oem"
	sysNetDir = "/sys/class/net"
)

// attachedNIC is a NIC of interfacesFile: one attached (Mode, Address as a secondary's), or a
// NIC of the seed's named by its MAC from then on (Pin).
type attachedNIC struct {
	Role    string `yaml:"role"`
	MAC     string `yaml:"mac"`
	Pin     bool   `yaml:"pin,omitempty"`
	Mode    string `yaml:"mode,omitempty"`
	Address string `yaml:"address,omitempty"`
}

// attachedLink is the .link file naming an attached or pinned NIC: 09- sorts before the seed's
// 10- ones, and udev takes the first file that matches.
func attachedLink(name, role string) string { return "09-" + name + "-" + role + ".link" }

// attachedOEM is the cloud-config of the attached NICs on the OEM partition: after the seed's
// (95_userdata), which every boot runs first.
func attachedOEM(name string) string { return filepath.Join(oemDir, "96_"+name+"-interfaces.yaml") }

// NetAttach is `network attach`'s request: a secondary NIC the release names, by its MAC, and its
// addressing (Mode, default none; Address with static).
type NetAttach struct {
	Role, MAC, Mode, Address string
}

// AttachNIC records a NIC for a role of r's host.interfaces the VM's cloud-config does not name,
// and writes the cloud-config that names and addresses it from the next boot. Given again for
// the same role, it replaces the role's MAC and addressing.
func AttachNIC(r *Release, dir string, a NetAttach, out io.Writer) error {
	name := r.Spec.Name
	var roles []string
	primary := ""
	if h := r.Spec.Host; h != nil {
		for _, k := range slices.Sorted(maps.Keys(h.Interfaces)) {
			if i := h.Interfaces[k]; i.Kind == "" || i.Kind == spec.InterfaceNIC {
				roles = append(roles, k)
				if i.Primary {
					primary = k
				}
			}
		}
	}
	switch {
	case !slices.Contains(roles, a.Role):
		if len(roles) == 0 {
			return fmt.Errorf("--role %s: %s declares no NIC (host.interfaces)", a.Role, name)
		}
		return fmt.Errorf("--role %s: not one of %s's NICs (%s)", a.Role, name, strings.Join(roles, ", "))
	case a.Role == primary:
		return fmt.Errorf("--role %s: the primary NIC is the VM's own: `network set` addresses it", a.Role)
	case seedNamed(name, a.Role):
		return fmt.Errorf("--role %s: the VM's cloud-config names its NIC already", a.Role)
	}
	mac := strings.ToLower(a.MAC)
	if !macAddr.MatchString(mac) || !unicastMAC(mac) {
		return fmt.Errorf("--mac %q is not a unicast MAC address (02:00:00:00:00:01)", a.MAC)
	}
	nic := attachedNIC{Role: a.Role, MAC: mac, Mode: cmp.Or(a.Mode, IfaceNone)}
	switch nic.Mode {
	case IfaceDHCP, IfaceNone:
		if a.Address != "" {
			return fmt.Errorf("--address is the static mode's")
		}
	case IfaceStatic:
		pfx, err := netip.ParsePrefix(a.Address)
		if err != nil || !pfx.Addr().Is4() || pfx.Bits() < 1 {
			return fmt.Errorf("--address %q: the static mode's IPv4 address with its prefix (192.0.2.10/24)", a.Address)
		}
		nic.Address = pfx.String()
	default:
		return fmt.Errorf("--mode %s: dhcp, static or none", a.Mode)
	}
	nics, err := readAttached(dir)
	if err != nil {
		return err
	}
	nics = slices.DeleteFunc(nics, func(n attachedNIC) bool { return n.Role == a.Role && !n.Pin })
	pins, err := seedPins(name, nics)
	if err != nil {
		return err
	}
	nics = append(nics, pins...)
	for _, n := range nics {
		if n.MAC == mac {
			return fmt.Errorf("--mac %s is %s's NIC's", mac, n.Role)
		}
	}
	if other, err := macTaken(name, a.Role, mac, roles); err != nil || other != "" {
		if err != nil {
			return err
		}
		return fmt.Errorf("--mac %s is %s's NIC's", mac, other)
	}
	nics = append(nics, nic)
	slices.SortFunc(nics, func(x, y attachedNIC) int { return strings.Compare(x.Role, y.Role) })
	rec, err := yaml.Marshal(nics)
	if err != nil {
		return err
	}
	cc, err := attachedConfig(name, r.Ctl(), nics)
	if err != nil {
		return err
	}
	// The cloud-config first: a record without it would say attached what no boot names.
	if err := writeAtomic(attachedOEM(name), bytes.NewReader(cc), 0o644, nil); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, interfacesFile), bytes.NewReader(rec), 0o644, nil); err != nil {
		return err
	}
	for _, p := range pins {
		fmt.Fprintf(out, "%s: its NIC (%s) is named by its MAC from the next boot, as another NIC joins it\n", p.Role, p.MAC)
	}
	how := nic.Mode
	if nic.Address != "" {
		how += " " + nic.Address
	}
	fmt.Fprintf(out, "%s: the NIC %s is named and addressed (%s) from the next boot: power the VM off and on once its platform attached the NIC (`vm attach` does both)\n", a.Role, mac, how)
	return nil
}

// seedNamed reports whether the VM's cloud-config names a NIC role (its .link file).
func seedNamed(name, role string) bool {
	_, err := os.Stat(filepath.Join(networkdDir, kairos.InterfaceLink(name, role)))
	return err == nil
}

// macTaken is the role another NIC with mac plays: one the seed names by it, or a NIC there now
// named as another of roles. A NIC with it named otherwise (one a boot did not name yet) is free.
func macTaken(name, role, mac string, roles []string) (string, error) {
	links, err := filepath.Glob(filepath.Join(networkdDir, kairos.InterfaceLink(name, "*")))
	if err != nil {
		return "", err
	}
	for _, l := range links {
		raw, err := os.ReadFile(l)
		if err != nil {
			return "", err
		}
		if other := linkRole(name, l); other != role && bytes.Contains(bytes.ToLower(raw), []byte("\nmacaddress="+mac+"\n")) {
			return other, nil
		}
	}
	for _, r := range roles {
		addr, err := os.ReadFile(filepath.Join(sysNetDir, r, "address"))
		if err == nil && r != role && strings.TrimSpace(string(addr)) == mac {
			return r, nil
		}
	}
	return "", nil
}

// linkRole is the role of the seed's .link file at path.
func linkRole(name, path string) string {
	return strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "10-"+name+"-"), ".link")
}

// readAttached reads interfacesFile; none is no NIC attached.
func readAttached(dir string) ([]attachedNIC, error) {
	path := filepath.Join(dir, interfacesFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var nics []attachedNIC
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&nics); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return nics, nil
}

// seedPins are the NICs the seed named by their original name alone (one NIC the release it was
// made by named), not pinned yet: each by the MAC of the NIC its role names now.
func seedPins(name string, have []attachedNIC) ([]attachedNIC, error) {
	links, err := filepath.Glob(filepath.Join(networkdDir, kairos.InterfaceLink(name, "*")))
	if err != nil {
		return nil, err
	}
	var pins []attachedNIC
	for _, l := range links {
		raw, err := os.ReadFile(l)
		if err != nil {
			return nil, err
		}
		role := linkRole(name, l)
		if !bytes.Contains(raw, []byte("\nOriginalName=")) || slices.ContainsFunc(have, func(n attachedNIC) bool { return n.Role == role }) {
			continue
		}
		addr, err := os.ReadFile(filepath.Join(sysNetDir, role, "address"))
		if err != nil {
			return nil, fmt.Errorf("the VM's cloud-config names a NIC %s by its original name, and no NIC is named so now to name it by its MAC: %w", role, err)
		}
		mac := strings.TrimSpace(string(addr))
		if !macAddr.MatchString(mac) {
			return nil, fmt.Errorf("the NIC %s has the address %q, not a MAC", role, mac)
		}
		pins = append(pins, attachedNIC{Role: role, MAC: mac, Pin: true})
	}
	return pins, nil
}

// attachedConfig is the OEM partition's cloud-config of nics: their .link files before udev
// names the NICs, the attached ones' addressing (a secondary's) at boot.
func attachedConfig(name, ctl string, nics []attachedNIC) ([]byte, error) {
	var links, networks []cloudFile
	for _, n := range nics {
		i := MachineInterface{Role: n.Role, MAC: n.MAC, Mode: n.Mode, Address: n.Address}
		links = append(links, cloudFile{Path: "/etc/systemd/network/" + attachedLink(name, n.Role), Permissions: 0o644, Content: interfaceLink(name, i)})
		if !n.Pin {
			networks = append(networks, cloudFile{Path: "/etc/systemd/network/" + kairos.InterfaceUnit(name, n.Role), Permissions: 0o644,
				Content: "[Match]\nName=" + n.Role + "\n\n" + secondarySections(i)})
		}
	}
	cc := cloudConfig{Stages: map[string][]stage{
		"initramfs": {{Name: name + ": the attached interfaces' names", If: notLive, Files: links}},
		"boot":      {{Name: name + ": the attached interfaces' network", If: notLive, Files: networks, Commands: []string{"networkctl reload"}}},
	}}
	out, err := yaml.Marshal(cc)
	if err != nil {
		return nil, err
	}
	head := fmt.Sprintf("#cloud-config\n# %s's NICs attached to this VM, written by `%s network attach` from %s.\n", name, ctl, interfacesFile)
	return append([]byte(head), out...), nil
}

// VMAttach is `vm attach`'s request: a NIC for a role of the release, on a VM `vm create
// --create tofu` made on TrueNAS.
type VMAttach struct {
	Host string
	// Interface is ROLE=SPEC, as `vm create --interface` takes it.
	Interface string
	// User, SSHKey and Address reach the VM over SSH: vm create's defaults, the host name.
	User, SSHKey, Address string
	Wait                  time.Duration
	Ctl                   string
}

// bootIDForm is a boot id as the kernel gives it, which the probe after the power cycle embeds.
var bootIDForm = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// truenasMAC is the MAC TrueNAS's built-in module derives for a VM's NIC given none: its
// local.mac, a locally administered one from the VM's name.
func truenasMAC(name string) string {
	h := sha256.Sum256([]byte("stowage-vm/" + name))
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", h[0], h[1], h[2], h[3], h[4])
}

// attachWait is how long vm attach waits for the VM to answer again after its power cycle.
var attachWait = 10 * time.Minute

// AttachVM attaches a NIC for a role of the release to a VM vm create made with OpenTofu on
// TrueNAS: the VM records it (`network attach` over SSH), the module's state gains it (its MAC
// generated, unless given), the apply powers the VM off and on, and the VM is checked to name it.
// Run again for a role attached already, it goes on with the MAC the state holds.
func (in *Installer) AttachVM(ctx context.Context, a *VMAttach) error {
	o, ctl := &in.O, in.R.Ctl()
	if err := checkHostname(a.Host); err != nil {
		return err
	}
	dir, err := o.stateDir(in.R.Spec.Name, a.Host)
	if err != nil {
		return err
	}
	// The plan file and the variables are this run's: no other vm attach or vm destroy runs.
	unlock, raw, err := openState(dir, a.Host, ctl)
	if err != nil {
		return err
	}
	defer unlock()
	key, builtin, err := readPlatform(dir)
	if err != nil {
		return err
	}
	pi := slices.IndexFunc(Platforms, func(p Platform) bool { return p.Key == key })
	if !builtinTrueNAS(key, builtin) || pi < 0 {
		return fmt.Errorf("vm attach adds a NIC through TrueNAS's built-in module, and %s was made with another (%s): attach the NIC on its platform, run `sudo %s network attach --role ROLE --mac MAC` on the VM, then power it off and on", a.Host, key, ctl)
	}
	// The module as vm create left it in the state: one without `nics` would add no NIC.
	files, err := moduleFiles(key, dir)
	if err != nil {
		return err
	}
	declared, err := moduleVars(files)
	if err != nil {
		return err
	}
	if !declares(declared, "nics") {
		return fmt.Errorf("%s: the VM's module (%s) has one NIC, from a Stowage before NICs by role: attach the NIC on TrueNAS, run `sudo %s network attach --role ROLE --mac MAC` on the VM, then power it off and on", a.Host, dir, ctl)
	}
	if err := in.tofuReady(ctx, true); err != nil {
		return err
	}
	role, _, _ := strings.Cut(a.Interface, "=")
	ifs, err := machineInterfaces(in.R.Spec, []string{a.Interface}, Platforms[pi])
	if err != nil {
		return err
	}
	i := ifs[slices.IndexFunc(ifs, func(i MachineInterface) bool { return i.Role == role })]
	if i.Primary {
		return fmt.Errorf("--interface %s: the primary NIC is the VM's own", a.Interface)
	}
	var vars map[string]json.RawMessage
	if err := json.Unmarshal(raw, &vars); err != nil {
		return fmt.Errorf("%s: %w", filepath.Join(dir, varsFile), err)
	}
	var nics []moduleNIC
	if v, ok := vars["nics"]; ok {
		if err := json.Unmarshal(v, &nics); err != nil {
			return fmt.Errorf("%s: nics: %w", filepath.Join(dir, varsFile), err)
		}
	}
	if len(nics) == 0 {
		nics = []moduleNIC{{}} // the module's one NIC, its MAC its own: index 0 is applied unchanged
	}
	var vmName, nicAttach string
	if err := json.Unmarshal(vars["name"], &vmName); err != nil || vmName == "" {
		return fmt.Errorf("%s: no VM name", filepath.Join(dir, varsFile))
	}
	_ = json.Unmarshal(vars["nic_attach"], &nicAttach)
	bridge := func(b string) string { return cmp.Or(b, nicAttach) } // "": the module's nic_attach
	given := func(k string) bool {
		_, rest, _ := strings.Cut(a.Interface, "=")
		return slices.ContainsFunc(strings.Split(rest, ","), func(f string) bool { return strings.HasPrefix(f, k+"=") })
	}
	attach := cmp.Or(i.Attach, nics[0].Attach)
	j := slices.IndexFunc(nics, func(n moduleNIC) bool { return n.Role == role })
	fresh := j < 0
	switch {
	case j >= 0 && given("mac") && nics[j].MAC != i.MAC:
		return fmt.Errorf("--interface %s: the VM's state attaches %s with the MAC %s already", a.Interface, role, nics[j].MAC)
	case j >= 0 && given("attach") && bridge(nics[j].Attach) != bridge(attach):
		return fmt.Errorf("--interface %s: the VM's state attaches %s to %s already", a.Interface, role, bridge(nics[j].Attach))
	case j >= 0:
		i.MAC = nics[j].MAC
	default:
		for _, n := range nics {
			// The module's own MAC where the state gives none: the one it derives from the VM's name.
			if cmp.Or(n.MAC, truenasMAC(vmName)) == i.MAC {
				return fmt.Errorf("--interface %s: the MAC %s is %s's", a.Interface, i.MAC, cmp.Or(n.Role, "the primary NIC"))
			}
		}
		nics = append(nics, moduleNIC{Role: role, MAC: i.MAC, Attach: attach})
		j = len(nics) - 1
	}
	// The host key vm create recorded for the address it waited at: the VM answers with it, or
	// ssh refuses, an address it recorded none for included.
	l, err := in.link(VMReach{Host: a.Host, User: a.User, SSHKey: a.SSHKey, Address: a.Address})
	if err != nil {
		return err
	}
	user, addr, ssh := l.user, l.addr, l.run
	// The boot the VM runs now, read before anything changes: the probe waits for another.
	b, err := ssh(ctx, "cat", "/proc/sys/kernel/random/boot_id")
	before := strings.TrimSpace(string(b))
	if err != nil || !bootIDForm.MatchString(before) {
		if err == nil {
			err = fmt.Errorf("it printed %q", before)
		}
		return fmt.Errorf("%s@%s: its boot id does not read (--host: the address vm create waited at): %w", user, addr, err)
	}
	nicsJSON, err := json.Marshal(nics)
	if err != nil {
		return err
	}
	// The plan first, with the NICs given on the command line over the state's variables, saved
	// and read: one that adds the NIC and powers the VM off and on (TrueNAS applies a device at a
	// start) is applied as planned; one changing no resource, a NIC the state held already,
	// reboots the VM for the addressing recorded, through the host daemon (its drain, its answer,
	// then the reboot); anything else (drift, an earlier attach left half done) is refused before
	// the VM or the state changes. The plan file goes whatever happens.
	defer os.Remove(filepath.Join(dir, attachPlanFile))
	if err := in.tofu(ctx, dir, a.Ctl, "init", "-input=false", "-no-color"); err != nil {
		return err
	}
	keyFlag := a.SSHKey
	if keyFlag != "" {
		if abs, err := filepath.Abs(expandHome(keyFlag)); err == nil {
			keyFlag = abs
		}
	}
	waitFlag := ""
	if a.Wait != 0 {
		waitFlag = a.Wait.String()
	}
	again := shellQuote(a.Ctl) + " vm attach " + a.Host + " --interface " + shellQuote(a.Interface)
	for _, f := range []struct{ flag, v string }{{"--host", a.Address}, {"--user", a.User}, {"--ssh-key", keyFlag}, {"--wait", waitFlag}} {
		if f.v != "" {
			again += " " + f.flag + " " + shellQuote(f.v)
		}
	}
	finish := fmt.Sprintf("power the VM off on TrueNAS, then `%s=%s tofu -chdir=%s apply` (with the TrueNAS API's variables) finishes it, and `%s` again checks it", CtlVar, shellQuote(a.Ctl), shellQuote(dir), again)
	cycles, err := in.attachPlan(ctx, dir, a.Ctl, nicsJSON, fmt.Sprintf("truenas_vm_device.nic[%q]", strconv.Itoa(j)), finish)
	if err != nil {
		return err
	}
	if fresh && !cycles {
		return fmt.Errorf("the VM's OpenTofu plan does not add the NIC %s (the module takes no NIC at %d); nothing changed", role, j)
	}
	cmd := []string{"sudo", ctl, "network", "attach", "--role", role, "--mac", i.MAC, "--mode", i.Mode}
	if i.Address != "" {
		cmd = append(cmd, "--address", i.Address)
	}
	o.logf("recording %s's NIC %s on %s@%s", role, i.MAC, user, addr)
	said, err := ssh(ctx, cmd...)
	if err != nil {
		return fmt.Errorf("%s@%s: %s network attach: %w", user, addr, ctl, err)
	}
	if _, err := o.Out.Write(said); err != nil {
		return err
	}
	vars["nics"] = nicsJSON
	b, err = json.MarshalIndent(vars, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, varsFile), bytes.NewReader(append(b, '\n')), 0o644, nil); err != nil {
		return err
	}
	if cycles {
		if err := in.tofu(ctx, dir, a.Ctl, "apply", "-input=false", "-no-color", attachPlanFile); err != nil {
			return fmt.Errorf("%w; the NIC is recorded on the VM and in %s's variables: fix the cause, %s", err, dir, finish)
		}
	} else {
		o.logf("rebooting %s@%s: the NIC was attached already, its addressing applies at a boot", user, addr)
		if _, err := ssh(ctx, "sudo", ctl, "reboot"); err != nil {
			return fmt.Errorf("%s@%s: %s reboot: %w", user, addr, ctl, err)
		}
	}
	// The VM answers again, from a new boot, with the host key it had, and names the NIC by its role.
	wait := cmp.Or(a.Wait, attachWait)
	o.logf("waiting for %s@%s to boot again and name its NIC %s (up to %s)", user, addr, role, wait)
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	probe := fmt.Sprintf(`test "$(cat /proc/sys/kernel/random/boot_id)" != %s && test -e /sys/class/net/%s`, before, role)
	for {
		_, err := ssh(wctx, probe)
		if err == nil {
			break
		}
		select {
		case <-wctx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%s@%s has not booted again naming a NIC %s after %s, with the NIC %s attached (last: %v): `ssh %s@%s sudo journalctl -b -u systemd-udevd -u systemd-networkd` tells more", user, addr, role, wait, i.MAC, err, user, addr)
		case <-time.After(sshPoll):
		}
	}
	fmt.Fprintf(o.Out, "Attached %s's NIC %s to %s.\n", role, i.MAC, a.Host)
	return nil
}

// attachPlanFile is the plan vm attach saves and applies, in the state directory.
const attachPlanFile = "attach.tfplan"

// attachPlan plans the state's module, with nics over its variable, into attachPlanFile, and
// reports whether the plan powers the VM off and on: the NIC device at created added and the
// power step replaced, every other resource unchanged. No resource changed is false (a NIC the
// state holds, applied). Anything else (drift, an earlier apply left half done) is an error
// naming it, and finish, the way to complete or correct it by hand.
func (in *Installer) attachPlan(ctx context.Context, dir, ctl string, nics []byte, created, finish string) (bool, error) {
	err := in.tofu(ctx, dir, ctl, "plan", "-detailed-exitcode", "-input=false", "-no-color", "-out="+attachPlanFile, "-var", "nics="+string(nics))
	var code interface{ ExitCode() int }
	switch {
	case err == nil:
		return false, nil
	case !errors.As(err, &code) || code.ExitCode() != 2:
		return false, err
	}
	env := append(in.O.environ(), CtlVar+"="+ctl, "TF_IN_AUTOMATION=1", "TF_INPUT=0")
	out, err := in.O.Tofu.Run(ctx, Cmd{Args: []string{"-chdir=" + dir, "show", "-json", "-no-color", attachPlanFile}, Env: env})
	if err != nil {
		return false, err
	}
	var plan struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Change  struct {
				Actions []string `json:"actions"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal(out, &plan); err != nil {
		return false, fmt.Errorf("tofu show -json: its output does not read: %w", err)
	}
	var nic, power, started bool
	var other []string
	for _, c := range plan.ResourceChanges {
		acts := strings.Join(c.Change.Actions, ",")
		switch {
		case acts == "no-op" || acts == "read":
		case acts == "create" && c.Address == created:
			nic = true
		case c.Address == "terraform_data.power" && (acts == "delete,create" || acts == "create,delete"):
			power = true
		case c.Address == "terraform_data.power" && acts == "create":
			started = true
		default:
			other = append(other, c.Address+" ("+acts+")")
		}
	}
	switch {
	case len(other) > 0:
		// Drift is the operator's to look at: an apply would carry it out.
		return false, fmt.Errorf("the VM's OpenTofu plan changes more than its NIC: %s; nothing changed (`%s=%s tofu -chdir=%s plan -var %s`, with the TrueNAS API's variables, shows it)", strings.Join(other, ", "), CtlVar, shellQuote(ctl), shellQuote(dir), shellQuote("nics="+string(nics)))
	case started || nic != power:
		return false, fmt.Errorf("the VM's OpenTofu plan does not add the NIC with the VM's power cycle, which TrueNAS applies it at (an earlier vm attach left half done); nothing changed: %s", finish)
	}
	return nic, nil
}

// openState takes the lock of a VM's state directory, which vm attach and vm destroy hold for
// their whole run (another one running is refused), and reads its variables under it: a VM with
// none, destroyed meanwhile included, is refused.
func openState(dir, host, ctl string) (unlock func(), vars []byte, err error) {
	none := fmt.Errorf("no VM %s made by `%s vm create --create %s`: %s holds no state", host, ctl, CreateTofu, dir)
	if _, err := os.Stat(filepath.Join(dir, varsFile)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, none
		}
		return nil, nil, err
	}
	unlock, err = lockFile(stateLock(dir))
	if errors.Is(err, errLocked) {
		return nil, nil, fmt.Errorf("%s: another vm attach or vm destroy runs on it", host)
	}
	if err != nil {
		return nil, nil, err
	}
	vars, err = os.ReadFile(filepath.Join(dir, varsFile))
	if err != nil {
		unlock()
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, none
		}
		return nil, nil, err
	}
	return unlock, vars, nil
}

// stateLock is the lock file of a VM's state directory: beside it, which vm destroy removes,
// named as no host is (a host name starts with no dot). It is never removed: a run holding a lock
// on a file unlinked meanwhile would exclude no other.
func stateLock(dir string) string {
	return filepath.Join(filepath.Dir(dir), "."+filepath.Base(dir)+".lock")
}
