package install

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/truenas"
)

// Platform is where a VM of the Kairos shape runs: the disk formats it boots, preferred first, and
// how its cloud-config reaches it — a NoCloud seed ISO it attaches as a CD-ROM, or the user-data
// field of the cloud's own launch.
type Platform struct {
	Key, Label string
	Formats    []string
	ISO        bool
	// Tofu: `vm create` can create the VM with OpenTofu (its module in tofu/<Key>).
	Tofu bool
	// Steps are what the operator does with the files, in order; %[1]s is the disk file, %[2]s the
	// seed (the ISO or the user-data file).
	Steps []string
}

// Platforms are the platforms `vm create` knows, in the order it offers them.
var Platforms = []Platform{
	{Key: "truenas", Label: "TrueNAS", Formats: []string{"qcow2", "raw"}, ISO: true, Tofu: true, Steps: []string{
		"Copy %[1]s and %[2]s to the TrueNAS host.",
		"Create a zvol at least as large as the disk you want, and write the disk into it: qemu-img convert -O raw %[1]s /dev/zvol/<pool>/<zvol>.",
		"Create a VM with UEFI boot, that zvol as its disk (VirtIO) and %[2]s as its CD-ROM, then start it.",
	}},
	{Key: "proxmox", Label: "Proxmox VE", Formats: []string{"qcow2", "raw"}, ISO: true, Steps: []string{
		"Copy %[1]s to the Proxmox host, and %[2]s to an ISO storage (e.g. /var/lib/vz/template/iso/).",
		"Create a VM with OVMF (UEFI) firmware and no disk, then import the disk: qm disk import <vmid> %[1]s <storage>; attach it and grow it to the size you want.",
		"Attach %[2]s as the VM's CD-ROM, make the disk the first boot device, then start it.",
	}},
	{Key: "libvirt", Label: "libvirt / KVM (virt-install, virt-manager)", Formats: []string{"qcow2", "raw"}, ISO: true, Steps: []string{
		"Grow the disk to the size you want: qemu-img resize %[1]s 40G.",
		"virt-install --name <name> --memory 4096 --vcpus 2 --boot uefi --osinfo linux2022 --import --disk %[1]s --disk %[2]s,device=cdrom --network default --noautoconsole",
	}},
	{Key: "vmware", Label: "VMware vSphere, ESXi or Workstation", Formats: []string{"ova"}, ISO: true, Steps: []string{
		"Deploy %[1]s (Deploy OVF Template, or File > Open), and grow its disk to the size you want.",
		"Upload %[2]s to a datastore and attach it as the VM's CD-ROM (connected at power on), then power the VM on.",
	}},
	{Key: "hyperv", Label: "Hyper-V", Formats: []string{"vhd"}, ISO: true, Steps: []string{
		"Convert the disk for a generation 2 VM and grow it: Convert-VHD %[1]s <name>.vhdx; Resize-VHD <name>.vhdx -SizeBytes 40GB.",
		"Create a generation 2 VM on <name>.vhdx, with Secure Boot's template \"Microsoft UEFI Certificate Authority\", and %[2]s in its DVD drive, then start it.",
	}},
	{Key: "azure", Label: "Microsoft Azure", Formats: []string{"vhd"}, Steps: []string{
		"Upload %[1]s as a page blob, and create a managed image from it (az image create --source <blob URL> --os-type Linux --hyper-v-generation V2).",
		"Create the VM from that image with %[2]s as its custom data (az vm create --custom-data %[2]s).",
	}},
	{Key: "gcp", Label: "Google Cloud", Formats: []string{"gce"}, Steps: []string{
		"Upload %[1]s to a bucket, and create an image from it (gcloud compute images create --source-uri gs://<bucket>/<file> --guest-os-features UEFI_COMPATIBLE).",
		"Create the instance from that image with %[2]s as its user-data (gcloud compute instances create --metadata-from-file user-data=%[2]s).",
	}},
	{Key: "aws", Label: "Amazon EC2", Formats: []string{"raw"}, Steps: []string{
		"Upload %[1]s to S3, import it as a snapshot (aws ec2 import-snapshot), and register an image from it (aws ec2 register-image --boot-mode uefi).",
		"Launch the instance from that image with %[2]s as its user data (aws ec2 run-instances --user-data file://%[2]s).",
	}},
}

// VMCreate is a run of `vm create`: what its flags gave. An empty field is asked, or defaulted.
type VMCreate struct {
	Platform string
	Out      string // the directory the disk and the seed are written to
	Hostname string
	User     string
	SSHKey   string // the public key's file
	Network  string // dhcp or static
	Address  string // static: the IPv4 address with its prefix
	Gateway  string
	IPv6     bool   // IPv6 on the VM's link (and its install's front); off, none
	Address6 string // with IPv6, either mode: a static IPv6 address with its prefix
	Gateway6 string
	DNS      []string
	SSHAllow []string // CIDR: the sources SSH accepts; none is every source
	Timezone string
	// Secrets is a decrypted SecretsFile the first install takes (a machine made again): given over
	// SSH, never in the cloud-config.
	Secrets  []byte
	Progress func(done, total int64) // the disk's download; nil reports nothing

	Create string // CreateTofu or CreateManual; empty is asked where OpenTofu can create the VM
	TofuVM
	Host string        // OpenTofu: the name or address the VM answers at (default: its static address, else Hostname)
	Wait time.Duration // OpenTofu: how long to wait for the VM to answer and install
	Ctl  string        // this <short>-stow's path, which OpenTofu's steps run
}

// CreateVM writes what a new VM of this release needs: its disk image in the format its platform
// boots, fetched by digest with the pull account, and its cloud-config (UserData) — on a seed ISO
// for a hypervisor, as the user-data file for a cloud — then prints the platform's steps. The
// release's own <short>-stow installs on the VM's first boot with the settings asked here; a secret
// input is never asked, and is given over SSH.
func (in *Installer) CreateVM(ctx context.Context, c *VMCreate) error {
	o, m, name := &in.O, in.R.Manifest, in.R.Spec.Name
	in.givenCA = true
	if in.R.Spec.Shapes.Kairos == nil || len(m.Artifacts.Disks) == 0 {
		return fmt.Errorf("%s %s publishes no disk image (shapes.kairos.disk_formats)", name, m.ProjectVersion)
	}
	if len(remoteModes(in.R.Spec.Front.TLSModes)) == 0 {
		return fmt.Errorf("%s offers no TLS mode a VM can be created with: %s (loopback is not reached from outside the VM, and byo's key never goes in its cloud-config)", name, strings.Join(in.R.Spec.Front.TLSModes, ", "))
	}
	if c.Secrets != nil {
		f, err := in.readSecrets(c.Secrets)
		if err != nil {
			return err
		}
		in.useSecrets(f)
	}
	p, format, err := in.platform(ctx, c.Platform)
	if err != nil {
		return err
	}
	how, err := in.creation(ctx, p, c.Create)
	if err != nil {
		return err
	}
	tofu := how == CreateTofu
	if tofu {
		if err := in.tofuReady(ctx); err != nil {
			return err
		}
		if c.Host != "" {
			// An IPv6 address is taken bare, as ssh takes it.
			if h, ok := strings.CutPrefix(c.Host, "["); ok && strings.HasSuffix(h, "]") {
				c.Host = strings.TrimSuffix(h, "]")
			}
			if _, err := netip.ParseAddr(c.Host); err != nil {
				if err := checkHostname(c.Host); err != nil {
					return fmt.Errorf("--host: %w", err)
				}
			}
		}
	} else if c.Host != "" || c.Wait != 0 || c.TofuVM != (TofuVM{}) {
		return fmt.Errorf("--host, --wait, --vm-name, --pool, --dataset, --bridge, --cpus, --memory and --disk are for --create %s", CreateTofu)
	}
	mach, err := in.machine(ctx, c)
	if err != nil {
		return err
	}
	st, waiting, err := in.vmSettings(ctx)
	if err != nil {
		return err
	}
	st.AwaitSecrets = c.Secrets != nil
	st.IPv6 = c.IPv6 // the install's network too: the front's IPv6 clients keep their address
	// A mirror: the disk comes from it, and the VM's updates.
	var ca []byte
	if o.Registry != "" {
		st.Registry = strings.TrimSuffix(o.Registry, "/")
		if _, err := registry.Repository(st.Registry); err != nil {
			return fmt.Errorf("--registry: %w", err)
		}
	}
	if o.RegistryCA != "" {
		if ca, err = registry.ReadCA(o.RegistryCA); err != nil {
			return fmt.Errorf("--registry-ca: %w", err)
		}
		st.RegistryCA = VMRegistryCA(name)
	}
	out := expandHome(cmp.Or(c.Out, "."))
	seed := filepath.Join(out, mach.Hostname+"-user-data.yaml")
	if p.ISO {
		seed = filepath.Join(out, mach.Hostname+"-seed.iso")
	}
	var state, addr string
	if tofu {
		if err := in.tofuVM(ctx, &c.TofuVM, mach.Hostname); err != nil {
			return err
		}
		if state, err = o.stateDir(name, mach.Hostname); err != nil {
			return err
		}
		// A VM of this host, or another of the same TrueNAS name, was applied (or tried to be):
		// never applied over — its zvol is its data.
		if _, err := os.Lstat(filepath.Join(state, varsFile)); err == nil {
			return fmt.Errorf("%s exists: a VM %s was created before (`%s vm destroy %s` removes it), or give this one another --hostname", state, mach.Hostname, in.R.Ctl(), mach.Hostname)
		}
		switch other, err := o.claimedBy(name, mach.Hostname, c.Name); {
		case err != nil:
			return err
		case other != "":
			return fmt.Errorf("the VM %s, of host %s, is named %s on TrueNAS already: give this one another --vm-name", other, other, c.Name)
		}
		if out, err = o.diskCache(name); err != nil {
			return err
		}
		// One directory per disk: another release's disk of the same file name never meets it.
		d := strings.TrimPrefix(m.Artifacts.Disks[format], "sha256:")
		if len(d) != 64 {
			return fmt.Errorf("the release's %s disk digest %q is not sha256", format, m.Artifacts.Disks[format])
		}
		out = filepath.Join(out, d[:16])
		seed = filepath.Join(state, "seed.iso")
		addr = cmp.Or(c.Host, mach.staticAddr(), mach.Hostname)
		in.sshAllows(addr, mach.SSHAllow)
		if a6 := mach.staticAddr6(); c.Host == "" && a6 != "" {
			in.sshAllows(a6, mach.SSHAllow) // ssh to the VM's name may take it
		}
	} else if _, err := os.Lstat(seed); err == nil { // a seed may already be attached to a VM: never replaced
		return fmt.Errorf("%s exists: move it away, or give the VM another --hostname or --out", seed)
	}
	for _, a := range []string{mach.staticAddr(), mach.staticAddr6()} {
		if !tofu && a != "" {
			in.sshAllows(a, mach.SSHAllow)
		}
	}
	if err := in.confirmVM(ctx, p, format, mach, st, seed, c, addr); err != nil {
		return err
	}
	userData, err := UserData(name, in.R.Ctl(), mach, *st, ca)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if tofu {
		if err := os.MkdirAll(state, 0o700); err != nil {
			return err
		}
	}
	prefix := cmp.Or(st.Registry, in.R.Spec.Registry.Prefix)
	cl, err := in.registryClient(prefix)
	if err != nil {
		return err
	}
	ref, err := cl.Digest(prefix+"/"+name+"-disk", m.Artifacts.Disks[format])
	if err != nil {
		return err
	}
	o.logf("fetching %s's %s disk (%s)", name, format, registry.Short(ref.DigestStr()))
	fallback := fmt.Sprintf("%s-%s.%s", name, m.Tag, format)
	disk, layer, err := cl.Download(ctx, ref, release.DiskArtifactType, release.DiskMediaType(format), out, fallback, c.Progress)
	if err != nil {
		return err
	}
	if p.ISO {
		err = WriteSeedISO(seed, userData, MetaData(mach))
	} else {
		err = writeAtomic(seed, bytes.NewReader(userData), 0o644, nil)
	}
	if err != nil {
		return err
	}
	ctl := in.R.Ctl()
	if tofu {
		if err := in.applyVM(ctx, state, c.Ctl, &c.TofuVM, disk, layer.Hex, seed); err != nil {
			return fmt.Errorf("%w\n(the state is in %s: `%s vm create` again with the same --hostname is refused; `%s vm destroy %s` removes what was made)", err, state, ctl, ctl, mach.Hostname)
		}
		if err := in.awaitVM(ctx, state, mach.User, addr, c.SSHKey, cmp.Or(c.Wait, 20*time.Minute), len(waiting) == 0 && !st.AwaitSecrets); err != nil {
			return err
		}
		if st.AwaitSecrets && len(waiting) == 0 {
			o.logf("installing over SSH with the secrets given")
			// After the boot unit: it runs after the release's images are loaded, which the install
			// takes (`systemctl start` waits for it, or returns at once once it ran).
			args := sshArgs(filepath.Join(state, "known_hosts"), mach.User, addr, c.SSHKey,
				"sudo", "systemctl", "start", name+".service", "&&", "sudo", ctl, "install", "--secrets", "-", "--no-input")
			if _, err := o.SSH.Run(ctx, Cmd{Args: args, Stdin: bytes.NewReader(c.Secrets), Stream: o.Log}); err != nil {
				return fmt.Errorf("the install over SSH with the secrets: %w; the VM is kept: `ssh %s@%s %s status` tells where it stopped — `sudo %s install` finishes an install begun, and %s; `%s vm destroy %s` removes it",
					err, mach.User, addr, ctl, ctl, secretsSteps(mach.User, addr, ctl), ctl, mach.Hostname)
			}
		}
		switch {
		case st.AwaitSecrets && len(waiting) > 0:
			fmt.Fprintf(o.Out, "The VM %s answers. Its install waits for its secrets and %s, which the cloud-config never carries: %s\n", mach.Hostname, strings.Join(waiting, ", "), secretsSteps(mach.User, addr, ctl))
		case len(waiting) > 0:
			fmt.Fprintf(o.Out, "The VM %s answers. Its install waits for %s, which the cloud-config never carries: ssh %s@%s sudo %s install\n", mach.Hostname, strings.Join(waiting, ", "), mach.User, addr, ctl)
		default:
			fmt.Fprintf(o.Out, "The VM %s runs %s %s. Then: ssh %s@%s %s status\n", mach.Hostname, name, m.ProjectVersion, mach.User, addr, ctl)
		}
		fmt.Fprintf(o.Out, "Its OpenTofu state is in %s; `%s vm destroy %s` removes it.\n", state, ctl, mach.Hostname)
		return nil
	}
	fmt.Fprintf(o.Out, "Wrote %s and %s.\n\nOn %s:\n", disk, seed, p.Label)
	for n, s := range p.Steps {
		fmt.Fprintf(o.Out, "  %d. %s\n", n+1, fmt.Sprintf(s, disk, seed))
	}
	if st.AwaitSecrets {
		fmt.Fprintf(o.Out, "\nIts first install waits for its secrets (the file `secrets export` wrote, decrypted), which the cloud-config never carries: %s\n", secretsSteps(mach.User, "<address>", ctl))
		if len(waiting) > 0 {
			fmt.Fprintf(o.Out, "It asks for %s, which the file does not hold.\n", strings.Join(waiting, ", "))
		}
		return nil
	}
	fmt.Fprintf(o.Out, "\nAt its first boot the VM installs %s %s. Then: ssh %s@<address> %s status\n", name, m.ProjectVersion, mach.User, ctl)
	if len(waiting) > 0 {
		fmt.Fprintf(o.Out, "The install waits for %s, which the cloud-config never carries: ssh %s@<address> sudo %s install\n", strings.Join(waiting, ", "), mach.User, ctl)
	}
	return nil
}

// secretsSteps tells how to give a VM's install its secrets file: copied over, its mode kept
// 0600, then an install on a terminal, which can ask what the file does not hold.
func secretsSteps(user, addr, ctl string) string {
	dst := addr // scp reads an IPv6 address's colons as the path's, unless bracketed
	if a, err := netip.ParseAddr(addr); err == nil && a.Is6() {
		dst = "[" + addr + "]"
	}
	return fmt.Sprintf("chmod 600 FILE; scp -p FILE %[1]s@%[4]s:secrets.yaml, then ssh -t %[1]s@%[2]s 'sudo %[3]s install --secrets secrets.yaml; rm secrets.yaml'", user, addr, ctl, dst)
}

// sshAllows warns when the SSH sources leave out the address this machine reaches addr from:
// `vm create` waits for the VM, and the secrets steps run, over SSH. A warning, not a refusal:
// the VM may see this machine at another address (NAT, a VPN). A name that does not resolve yet
// (one the VM's DHCP lease registers) is not checked.
func (in *Installer) sshAllows(addr string, allow []netip.Prefix) {
	if len(allow) == 0 {
		return
	}
	dsts := []netip.Addr{}
	if a, err := netip.ParseAddr(addr); err == nil {
		dsts = append(dsts, a)
	} else {
		ips, lerr := in.O.lookupIP(addr)
		if lerr != nil || len(ips) == 0 {
			in.O.logf("--ssh-allow: %s does not resolve yet, so whether the sources let this machine in is not checked", addr)
			return
		}
		dsts = ips
	}
	// ssh may take any address the name resolves to (an IPv6 one first): each is checked, each
	// source said once.
	var said []netip.Addr
	for _, dst := range dsts {
		src, err := in.O.sourceAddr(dst.Unmap())
		if err != nil {
			continue
		}
		src = src.Unmap().WithZone("") // a prefix never contains a zoned address
		if slices.Contains(said, src) {
			continue
		}
		said = append(said, src)
		if !slices.ContainsFunc(allow, func(p netip.Prefix) bool { return p.Contains(src) }) {
			in.O.logf("--ssh-allow leaves out %s, the address this machine reaches %s (%s) from: unless the VM sees it at another address (NAT, a VPN), SSH from here will be refused", src, addr, dst.Unmap())
		}
	}
}

// platform is the platform the VM runs on, and the disk format it takes: the first of the
// platform's formats the release publishes. Only the platforms the release has a disk for are
// offered.
func (in *Installer) platform(ctx context.Context, key string) (Platform, string, error) {
	disks := in.R.Manifest.Artifacts.Disks
	var offered []Platform
	for _, p := range Platforms {
		if slices.ContainsFunc(p.Formats, func(f string) bool { return disks[f] != "" }) {
			offered = append(offered, p)
		}
	}
	keys := func() string {
		var ks []string
		for _, p := range offered {
			ks = append(ks, p.Key)
		}
		return strings.Join(ks, ", ")
	}
	if len(offered) == 0 {
		return Platform{}, "", fmt.Errorf("no platform `vm create` knows boots the disk formats %s %s publishes", in.R.Spec.Name, in.R.Manifest.ProjectVersion)
	}
	if key == "" && in.O.Prompt != nil {
		choices := make([]Choice, len(offered))
		for n, p := range offered {
			choices[n] = Choice{p.Key, p.Label}
		}
		a, err := in.O.Prompt.Ask(ctx, Form{
			Questions: []Question{{Title: "Where does the VM run", Key: "--platform", Choices: choices}},
			Defaults:  func(map[string]string) map[string]string { return map[string]string{"--platform": offered[0].Key} },
		})
		if err != nil {
			return Platform{}, "", err
		}
		key = cmp.Or(a["--platform"], offered[0].Key)
	}
	if key == "" {
		return Platform{}, "", fmt.Errorf("--platform is required (%s)", keys())
	}
	i := slices.IndexFunc(offered, func(p Platform) bool { return p.Key == key })
	if i < 0 {
		return Platform{}, "", fmt.Errorf("--platform %s: %s %s offers %s", key, in.R.Spec.Name, in.R.Manifest.ProjectVersion, keys())
	}
	p := offered[i]
	f := p.Formats[slices.IndexFunc(p.Formats, func(f string) bool { return disks[f] != "" })]
	return p, f, nil
}

var userName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// checkHostname takes a host name of letters, digits and '-', each dot-separated label 1 to 63
// long: the VM's name on the network.
func checkHostname(v string) error {
	if len(v) > 253 {
		return errors.New("a host name is at most 253 characters")
	}
	for l := range strings.SplitSeq(v, ".") {
		if !hostLabel.MatchString(l) {
			return fmt.Errorf("%q is not a host name: lowercase letters, digits and '-', in labels of 1 to 63", v)
		}
	}
	return nil
}

// systemUsers are the accounts the OS owns (the base's and Kairos's): never the administrator, who
// would log in as a service.
var systemUsers = []string{"root", AdminGroup, "kairos", "daemon", "bin", "sys", "sync", "games", "man", "lp", "mail",
	"news", "uucp", "proxy", "www-data", "backup", "list", "irc", "gnats", "nobody", "messagebus", "sshd", "docker",
	"polkitd", "syslog", "uuidd", "tcpdump", "tss", "dnsmasq", "landscape", "fwupd-refresh", "usbmux", "lxd",
	"ntp", "chrony", "avahi", "rpc", "statd", "pollinate", "containerd", "postfix", "ftp", "geoclue", "cups", "sssd",
	"dhcpcd", "rtkit", "colord", "saned", "speech-dispatcher", "kernoops", "hplip", "whoopsie", "qemu", "libvirt-qemu",
	"adm", "halt", "shutdown", "operator", "nscd", "tftp", "dbus", "unbound", "rpcuser", "squid", "named", "apache", "nginx"}

// checkUser takes a Linux user name, never one the OS owns.
func checkUser(v string) error {
	if !userName.MatchString(v) {
		return fmt.Errorf("%q is not a user name: a lowercase letter or '_', then up to 31 lowercase letters, digits, '_' or '-'", v)
	}
	if slices.Contains(systemUsers, v) || strings.HasPrefix(v, "_") || strings.HasPrefix(v, "systemd-") {
		return fmt.Errorf("%s is the OS's own: choose another administrator", v)
	}
	return nil
}

// sshKeyTypes are the public key types an authorized key may have.
var sshKeyTypes = []string{"ssh-ed25519", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521", "ssh-rsa",
	"sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com"}

// authorizedKey reads the public key file at path: one line "<type> <base64 key> [comment]", the
// key's own type matching the line's.
func authorizedKey(path string) (string, error) {
	raw, err := os.ReadFile(expandHome(path))
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(raw))
	if strings.ContainsAny(line, "\r\n") {
		return "", fmt.Errorf("%s: one public key, on one line", path)
	}
	f := strings.Fields(line)
	if len(f) < 2 || !slices.Contains(sshKeyTypes, f[0]) {
		return "", fmt.Errorf("%s is not an SSH public key (%s, then the key)", path, strings.Join(sshKeyTypes, ", "))
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil || len(blob) < 4 {
		return "", fmt.Errorf("%s: the key is not base64", path)
	}
	n := binary.BigEndian.Uint32(blob)
	if uint64(n) > uint64(len(blob)-4) || string(blob[4:4+n]) != f[0] {
		return "", fmt.Errorf("%s: the key is not a %s key", path, f[0])
	}
	return line, nil
}

// defaultKey is the operator's first public key of the usual names in home; empty when none
// exists.
func defaultKey(home string) string {
	if home == "" {
		return ""
	}
	for _, k := range []string{"id_ed25519.pub", "id_ecdsa.pub", "id_rsa.pub"} {
		p := filepath.Join(home, ".ssh", k)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// defaultUser is the operator's own user name when the VM may take it, else "sysadmin".
func defaultUser(getenv func(string) string) string {
	if u := getenv("USER"); checkUser(u) == nil {
		return u
	}
	return "sysadmin"
}

// machine asks what the flags did not give of the VM's own settings, and checks them.
func (in *Installer) machine(ctx context.Context, c *VMCreate) (Machine, error) {
	o := &in.O
	given := map[string]string{"--hostname": c.Hostname, "--user": c.User, "--ssh-key": c.SSHKey, "--network": c.Network}
	defaults := map[string]string{"--hostname": in.R.Spec.Name, "--user": defaultUser(o.getenv), "--ssh-key": defaultKey(o.getenv("HOME")), "--network": "dhcp"}
	checkKey := func(v string) error { _, err := authorizedKey(v); return err }
	checkNet := func(v string) error {
		if v != "dhcp" && v != "static" {
			return fmt.Errorf("--network %s: dhcp or static", v)
		}
		return nil
	}
	qs := []Question{
		{Title: "The VM's host name", Key: "--hostname", Check: checkHostname},
		{Title: "Its administrator, who logs in over SSH", Key: "--user", Check: checkUser},
		{Title: "The administrator's SSH public key file", Key: "--ssh-key", Required: defaults["--ssh-key"] == "", Check: checkKey},
		{Title: "Its network", Key: "--network", Choices: []Choice{{"dhcp", "DHCP"}, {"static", "A static address"}}},
	}
	a, err := in.askGiven(ctx, qs, given, defaults)
	if err != nil {
		return Machine{}, err
	}
	m := Machine{Hostname: a["--hostname"], User: a["--user"]}
	if a["--ssh-key"] == "" {
		return Machine{}, errors.New("--ssh-key is required: the administrator's SSH public key file (no ~/.ssh/id_ed25519.pub, id_ecdsa.pub or id_rsa.pub)")
	}
	if m.SSHKey, err = authorizedKey(a["--ssh-key"]); err != nil {
		return Machine{}, err
	}
	c.SSHKey = a["--ssh-key"] // the key ssh logs in with, OpenTofu's VM waited for
	for _, v := range c.SSHAllow {
		p, err := netip.ParsePrefix(strings.TrimSpace(v))
		if err != nil {
			return Machine{}, fmt.Errorf("--ssh-allow: %q is not a source with its prefix (192.0.2.0/24, 2001:db8::/32)", v)
		}
		m.SSHAllow = append(m.SSHAllow, p.Masked())
	}
	if len(m.SSHAllow) == 0 {
		o.logf("SSH accepts every source: --ssh-allow CIDR,… limits it to the operators' addresses")
	}
	if c.Timezone != "" {
		if err := checkTimezone(c.Timezone); err != nil {
			return Machine{}, fmt.Errorf("--timezone: %w", err)
		}
		m.Timezone = c.Timezone
	}
	if err := checkNet(a["--network"]); err != nil {
		return Machine{}, err
	}
	// The IPv6 pair is checked with the rest of the addressing, by the rules `network set` follows.
	if !c.IPv6 && (c.Address6 != "" || c.Gateway6 != "") {
		return Machine{}, errors.New("--address6 and --gateway6 are for --ipv6")
	}
	setNet := func(n NetConfig) (Machine, error) {
		n.IPv6 = c.IPv6
		var err error
		if m.Net, err = validNet(n); err != nil {
			return Machine{}, fmt.Errorf("the VM's network: %w", err)
		}
		return m, nil
	}
	if a["--network"] == "dhcp" {
		if c.Address != "" || c.Gateway != "" || len(c.DNS) > 0 {
			return Machine{}, errors.New("--address, --gateway and --dns are for --network static")
		}
		return setNet(NetConfig{Mode: "dhcp", Address6: c.Address6, Gateway6: c.Gateway6})
	}
	checkPrefix := func(v string) error {
		if _, err := netip.ParsePrefix(v); err != nil {
			return fmt.Errorf("%q is not an address with its prefix (192.0.2.10/24)", v)
		}
		return nil
	}
	checkAddr := func(v string) error {
		if v == "" {
			return nil
		}
		if _, err := netip.ParseAddr(v); err != nil {
			return fmt.Errorf("%q is not an IP address", v)
		}
		return nil
	}
	checkAddrs := func(v string) error {
		for d := range strings.SplitSeq(v, ",") {
			if err := checkAddr(strings.TrimSpace(d)); err != nil {
				return err
			}
		}
		return nil
	}
	qs = []Question{
		{Title: "Its address, with the prefix (192.0.2.10/24)", Key: "--address", Required: true, Check: checkPrefix},
		{Title: "Its gateway", Key: "--gateway", Check: checkAddr},
	}
	if c.IPv6 {
		qs = append(qs, Question{Title: "Its IPv6 address, with the prefix (2001:db8::10/64): none takes the router advertisements'", Key: "--address6", Check: func(v string) error {
			_, _, err := validNet6(v, "")
			return err
		}}, Question{Title: "Its IPv6 gateway, with an IPv6 address: none takes the router advertisements'", Key: "--gateway6", Check: checkAddr})
	}
	qs = append(qs, Question{Title: "Its DNS servers, comma-separated", Key: "--dns", Check: checkAddrs})
	a, err = in.askGiven(ctx, qs, map[string]string{"--address": c.Address, "--gateway": c.Gateway, "--address6": c.Address6, "--gateway6": c.Gateway6, "--dns": strings.Join(c.DNS, ",")}, nil)
	if err != nil {
		return Machine{}, err
	}
	if a["--address"] == "" {
		return Machine{}, errors.New("--address is required with --network static")
	}
	var dns []string
	for d := range strings.SplitSeq(a["--dns"], ",") {
		if d = strings.TrimSpace(d); d != "" {
			dns = append(dns, d)
		}
	}
	return setNet(NetConfig{Mode: "static", Address: a["--address"], Gateway: a["--gateway"], Address6: a["--address6"], Gateway6: a["--gateway6"], DNS: dns})
}

// askGiven asks, in one form, each question the flags left empty; a run with no terminal takes
// the defaults. It returns every answer, given or asked, the default for one left empty. Each
// given value is checked before the form, and every value not empty after it.
func (in *Installer) askGiven(ctx context.Context, qs []Question, given, defaults map[string]string) (map[string]string, error) {
	out := map[string]string{}
	var ask []Question
	for _, q := range qs {
		if v := given[q.Key]; v != "" {
			if q.Check != nil {
				if err := q.Check(v); err != nil {
					return nil, fmt.Errorf("%s: %w", q.Key, err)
				}
			}
			out[q.Key] = v
			continue
		}
		ask = append(ask, q)
	}
	if in.O.Prompt != nil && len(ask) > 0 {
		a, err := in.O.Prompt.Ask(ctx, Form{Questions: ask, Defaults: func(map[string]string) map[string]string { return defaults }})
		if err != nil {
			return nil, err
		}
		for _, q := range ask {
			out[q.Key] = a[q.Key]
		}
	}
	for _, q := range ask {
		if out[q.Key] == "" {
			out[q.Key] = defaults[q.Key]
		}
		if v := out[q.Key]; v != "" && q.Check != nil {
			if err := q.Check(v); err != nil {
				return nil, fmt.Errorf("%s: %w", q.Key, err)
			}
		}
	}
	return out, nil
}

// vmSettings asks the product's questions as the VM's install asks them — the TLS mode, then the
// inputs of the Kairos shape in that mode — but no secret: a secret input given is refused, and
// one the install needs is returned, for the operator to give over SSH. The VM is served over
// HTTPS: loopback, which nothing outside the VM reaches, and byo, whose key the cloud-config never
// carries, are not offered. No input and no domain is read from the operator's environment (the
// pull token still is).
func (in *Installer) vmSettings(ctx context.Context) (*Settings, []string, error) {
	o := &in.O
	o.Preloaded = true // the VM's install: the inputs of the Kairos shape
	in.goOn, in.remote = "Go on with these answers", true
	switch o.TLS {
	case "byo":
		return nil, nil, fmt.Errorf("--tls byo copies a certificate's private key, which the cloud-config never carries: create the VM with another mode, or install it over SSH (`sudo %s install --tls byo`)", in.R.Ctl())
	case Loopback:
		return nil, nil, errors.New("--tls none serves on the VM's loopback address alone, which nothing outside it reaches: give the VM an HTTPS mode")
	}
	if err := in.applyDomains(); err != nil {
		return nil, nil, err
	}
	var mode string
	var values map[string]string
	var list []input
	for {
		m, asked, err := in.mode(ctx, mode)
		if err != nil {
			return nil, nil, err
		}
		mode = m
		if err := in.knownInputs(mode); err != nil {
			return nil, nil, err
		}
		list = nil
		for _, i := range in.inputList(mode) {
			_, set := o.Set[i.Key]
			_, answered := o.Answers[i.Key]
			if i.Secret && (set || answered) {
				return nil, nil, fmt.Errorf("%s is a secret input, which the cloud-config never carries: give it over SSH (`sudo %s install`)", i.Key, in.R.Ctl())
			}
			if !i.Secret {
				list = append(list, i)
			}
		}
		values, err = in.inputs(ctx, list, asked)
		if errors.Is(err, ErrBack) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		break
	}
	st := &Settings{Channel: in.R.Manifest.Channel, TLS: mode, HTTPPort: o.HTTPPort, HTTPSPort: o.HTTPSPort}
	for _, i := range list {
		if v := values[i.Key]; v != "" {
			if st.Inputs == nil {
				st.Inputs = map[string]string{}
			}
			st.Inputs[i.Key] = v
		}
	}
	var waiting []string
	for _, i := range in.inputList(mode) {
		if i.Secret && i.Required && i.Default == "" && in.restoredSecret(i.Key) == "" {
			waiting = append(waiting, i.Key)
		}
	}
	return st, waiting, nil
}

// confirmVM shows what will be written, and on a terminal asks to go on.
func (in *Installer) confirmVM(ctx context.Context, p Platform, format string, m Machine, st *Settings, seed string, c *VMCreate, addr string) error {
	o := &in.O
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s %s (revision %d) on %s: its %s disk, and %s\n", in.R.Spec.Name, in.R.Manifest.ProjectVersion, in.R.Manifest.Revision, p.Label, format, seed)
	tofu := addr != ""
	if tofu {
		t := c.TofuVM
		fmt.Fprintf(&b, "  OpenTofu creates the VM %s at %s: %d CPUs, %d MiB, a %d GiB disk on %s/%s, its network on %s\n", t.Name, o.getenv(truenas.URLVar), t.CPUs, t.MemoryMiB, t.DiskGiB, t.Pool, t.Dataset, t.Bridge)
		fmt.Fprintf(&b, "  then waits for it at %s\n", addr)
	}
	fmt.Fprintf(&b, "  host %s, administrator %s (%s key)\n", m.Hostname, m.User, strings.Fields(m.SSHKey)[0])
	fmt.Fprintf(&b, "  network: %s\n", describeNet(&m.Net))
	ssh := "every source"
	if len(m.SSHAllow) > 0 {
		ssh = fmt.Sprint(m.SSHAllow)
	}
	fmt.Fprintf(&b, "  SSH from %s; time zone %s\n", ssh, cmp.Or(m.Timezone, "UTC"))
	fmt.Fprintf(&b, "  served: %s; updates from the %s channel\n", st.TLS, st.Channel)
	if st.Registry != "" || st.RegistryCA != "" {
		fmt.Fprintf(&b, "  registry: %s", cmp.Or(st.Registry, in.R.Spec.Registry.Prefix))
		if st.RegistryCA != "" {
			fmt.Fprintf(&b, ", trusting the CA of %s", o.RegistryCA)
		}
		b.WriteString("\n")
	}
	for _, k := range sortedKeys(st.Inputs) {
		fmt.Fprintf(&b, "  %s=%s\n", k, st.Inputs[k])
	}
	if st.AwaitSecrets {
		b.WriteString("  secrets: --secrets', given over SSH once the VM answers (never in its cloud-config)\n")
	}
	o.logf("%s", strings.TrimRight(b.String(), "\n"))
	if o.Prompt == nil {
		return nil
	}
	a, err := o.Prompt.Ask(ctx, Form{
		Questions: []Question{{Title: map[bool]string{false: "Write the VM's disk and cloud-config", true: "Create the VM"}[tofu], Key: "confirm",
			Choices: []Choice{{"yes", map[bool]string{false: "Yes, write them", true: "Yes, create it"}[tofu]}, {"no", "No, quit writing nothing"}}}},
		Defaults: func(map[string]string) map[string]string { return map[string]string{"confirm": "yes"} },
	})
	if err != nil {
		return err
	}
	if cmp.Or(a["confirm"], "yes") != "yes" {
		return ErrAborted
	}
	return nil
}
