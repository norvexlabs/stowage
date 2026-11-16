package install

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/norvexlabs/stowage/internal/kairos"
	"github.com/norvexlabs/stowage/internal/lint"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/tune"
	"github.com/norvexlabs/stowage/internal/version"
)

// Platform is where a VM of the Kairos shape runs: the disk formats it boots, preferred first, and
// how its cloud-config reaches it — a NoCloud seed ISO it attaches as a CD-ROM, or the user-data
// field of the cloud's own launch.
type Platform struct {
	Key, Label string
	Formats    []string
	ISO        bool
	// Install: the disk is the installer ISO, which installs the OS on the machine's own disk
	// (--device), the seed's install block saying which.
	Install bool
	// Steps are what the operator does with the files, in order; %[1]s is the disk file, %[2]s the
	// seed (the ISO or the user-data file), %[3]d the disk's size in GiB (the OS's and the app's).
	Steps []string
	// UserDataMax is the most user data the cloud takes, in bytes (0: as much as a file input holds).
	UserDataMax int
	// Console is where the operator watches the VM's boot, as a deadline's message names it.
	Console string
}

// Platforms are the platforms `vm create` knows, in the order it offers them.
var Platforms = []Platform{
	{Key: "truenas", Console: "its display on TrueNAS", Label: "TrueNAS", Formats: []string{"qcow2", "raw"}, ISO: true, Steps: []string{
		"Copy %[1]s and %[2]s to the TrueNAS host.",
		"Create a zvol of %[3]d GiB, and write the disk into it: qemu-img convert -O raw %[1]s /dev/zvol/<pool>/<zvol>.",
		"Create a VM with UEFI boot, that zvol as its disk (VirtIO) and %[2]s as its CD-ROM, then start it.",
	}},
	{Key: "proxmox", Console: "its console in Proxmox VE", Label: "Proxmox VE", Formats: []string{"qcow2", "raw"}, ISO: true, Steps: []string{
		"Copy %[1]s to the Proxmox host, and %[2]s to an ISO storage (e.g. /var/lib/vz/template/iso/).",
		"Create a VM with OVMF (UEFI) firmware and no disk, then import the disk: qm disk import <vmid> %[1]s <storage>; attach it and grow it to %[3]d GiB.",
		"Attach %[2]s as the VM's CD-ROM, make the disk the first boot device, then start it.",
	}},
	{Key: "libvirt", Console: "`virsh console` on the libvirt host", Label: "libvirt / KVM (virt-install, virt-manager)", Formats: []string{"qcow2", "raw"}, ISO: true, Steps: []string{
		"Grow the disk: qemu-img resize %[1]s %[3]dG.",
		"virt-install --name <name> --memory 4096 --vcpus 2 --boot uefi --osinfo linux2022 --import --disk %[1]s --disk %[2]s,device=cdrom --network default --noautoconsole",
	}},
	{Key: "vmware", Console: "its console in vSphere or Workstation", Label: "VMware vSphere, ESXi or Workstation", Formats: []string{"ova"}, ISO: true, Steps: []string{
		"Deploy %[1]s (Deploy OVF Template, or File > Open), and grow its disk to %[3]d GiB.",
		"Upload %[2]s to a datastore and attach it as the VM's CD-ROM (connected at power on), then power the VM on.",
	}},
	{Key: "hyperv", Console: "its console in Hyper-V Manager", Label: "Hyper-V", Formats: []string{"vhd"}, ISO: true, Steps: []string{
		"Convert the disk for a generation 2 VM and grow it: Convert-VHD %[1]s <name>.vhdx; Resize-VHD <name>.vhdx -SizeBytes %[3]dGB.",
		"Create a generation 2 VM on <name>.vhdx, with Secure Boot's template \"Microsoft UEFI Certificate Authority\", and %[2]s in its DVD drive, then start it.",
	}},
	{Key: "metal", Console: "the machine's own screen", Label: "Bare metal (amd64)", Formats: []string{"iso"}, ISO: true, Install: true, Steps: []string{
		"Write %[1]s to a USB stick, and %[2]s to another: dd if=<file> of=/dev/<stick> bs=4M conv=fsync (or any image writer).",
		"Plug both into the server and boot it from the first, in UEFI mode with Secure Boot off: the installer wipes the --device disk (at least %[3]d GiB), installs the OS on it and powers the server off.",
		"Take both sticks out, then power the server on: its first boot installs the release.",
	}},
	{Key: "azure", Console: "its serial console in the Azure portal", Label: "Microsoft Azure", Formats: []string{"vhd"}, UserDataMax: 64 << 10, Steps: []string{
		"Upload %[1]s as a page blob, and create a managed image from it (az image create --source <blob URL> --os-type Linux --hyper-v-generation V2).",
		"Create the VM from that image with %[2]s as its custom data and a %[3]d GiB disk (az vm create --custom-data %[2]s --os-disk-size-gb %[3]d).",
	}},
	{Key: "gcp", Console: "its serial port output in the Google Cloud console", Label: "Google Cloud", Formats: []string{"gce"}, Steps: []string{
		"Upload %[1]s to a bucket, and create an image from it (gcloud compute images create --source-uri gs://<bucket>/<file> --guest-os-features UEFI_COMPATIBLE).",
		"Create the instance from that image with %[2]s as its user-data and a %[3]d GiB disk (gcloud compute instances create --metadata-from-file user-data=%[2]s --boot-disk-size %[3]dGB).",
	}},
	{Key: "aws", Console: "its serial console or system log in the EC2 console", Label: "Amazon EC2", Formats: []string{"raw"}, UserDataMax: 16 << 10, Steps: []string{
		"Upload %[1]s to S3, import it as a snapshot (aws ec2 import-snapshot), and register an image from it (aws ec2 register-image --boot-mode uefi).",
		"Launch the instance from that image with %[2]s as its user data and a %[3]d GiB root volume (aws ec2 run-instances --user-data file://%[2]s, the image's root device mapped to a volume of that size with --block-device-mappings).",
	}},
	{Key: "scaleway", Console: "the instance's Console tab in the Scaleway console", Label: "Scaleway", Formats: []string{"qcow2"}, Steps: []string{
		"Upload %[1]s to an Object Storage bucket in the instance's region, import it as a snapshot, and make an image of it (scw block snapshot import-from-object-storage bucket=<bucket> key=<key> name=<name>; scw instance image create snapshot-id=<snapshot id> arch=x86_64 name=<name>).",
		"Create the instance from that image with %[2]s as its cloud-init user data and an SBS root volume of %[3]d GiB, which Scaleway counts in GB: %[3]d × 1.073741824, rounded up (scw instance server create type=PLAY2-NANO image=<image id> root-volume=sbs:<GB>GB cloud-init=@%[2]s).",
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
	// Hosts are --add-host's NAME=IP: names the VM and its stack resolve to fixed addresses.
	Hosts    []string
	SSHAllow []string // CIDR: the sources SSH accepts; none is every source
	Timezone string
	// Interfaces are --interface's ROLE=SPEC: a host.interfaces NIC's addressing (dhcp, static with
	// address=, none), its mac= and the platform's network it attaches to (attach=).
	Interfaces []string
	// Device is the disk the installer ISO installs the OS on, wiped (metal): a path under /dev/,
	// or auto (Kairos's choice: the largest disk).
	Device string
	// TuneProfile is the operator's tuning profile, which the VM's boots apply and its first
	// install records; empty is the release's.
	TuneProfile string
	// Secrets is a decrypted SecretsFile the first install takes (a machine made again): given over
	// SSH, never in the cloud-config.
	Secrets []byte
	// RegistryLogin is a login file (--registry-login-file), a Docker config holding one auths
	// entry for the host of the release's own registry or of the mirror: given over SSH once the
	// VM answers (`registry-login -`), never in the cloud-config.
	RegistryLogin []byte
	Progress      func(done, total int64) // the disk's download; nil reports nothing
}

// CreateVM writes what a new VM of this release needs: its disk image in the format its platform
// boots, fetched by digest with the pull account, and its cloud-config (UserData) — on a seed ISO
// for a hypervisor, as the user-data file for a cloud — then prints the platform's steps. The
// release's own <short>-stow installs on the VM's first boot with the settings asked here; a secret
// input is never asked, and is given over SSH.
func (in *Installer) CreateVM(ctx context.Context, c *VMCreate) error {
	o, m, name := &in.O, in.R.Manifest, in.R.Spec.Name
	if err := in.seedable(); err != nil {
		return err
	}
	if c.Secrets != nil {
		f, err := in.readSecrets(c.Secrets)
		if err != nil {
			return err
		}
		in.useSecrets(f)
	}
	prefix, cl, err := in.seedRegistry(ctx, &c.Platform)
	if err != nil {
		return err
	}
	boot := in.boot
	login, err := in.createLogin(c.RegistryLogin, prefix)
	if err != nil {
		return err
	}
	p, format, err := in.platform(ctx, c.Platform)
	if err != nil {
		return err
	}
	secrets := ""
	if c.Secrets != nil {
		secrets = "--secrets"
	}
	mach, st, waiting, given, ca, err := in.seedSettings(ctx, c, p, prefix, secrets)
	if err != nil {
		return err
	}
	out := expandHome(cmp.Or(c.Out, "."))
	seed := filepath.Join(out, mach.Hostname+"-user-data.yaml")
	if p.ISO {
		seed = filepath.Join(out, mach.Hostname+"-seed.iso")
	}
	if _, err := os.Lstat(seed); err == nil { // a seed may already be attached to a VM: never replaced
		return fmt.Errorf("%s exists: move it away, or give the VM another --hostname or --out", seed)
	}
	// The given material waits in a file of the operator's, beside the seed, and ssh reads its
	// pin's path. Absolute: the step is run later, from anywhere.
	abs, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	givenFile := filepath.Join(abs, mach.Hostname+".given")
	if given != nil {
		if err := sshPathOK(abs); err != nil {
			return fmt.Errorf("--out: %w", err)
		}
	}
	for _, f := range []string{givenFile, givenFile + ".known_hosts"} {
		if _, err := os.Lstat(f); given != nil && err == nil {
			return fmt.Errorf("%s exists: move it away, or give the VM another --hostname or --out", f)
		}
	}
	givenPin := givenFile + ".known_hosts"
	for _, a := range []string{mach.staticAddr(), mach.staticAddr6()} {
		if a != "" {
			in.sshAllows(a, mach)
		}
	}
	// What goes over SSH goes to the host key the cloud-config carries, never to a key taken on
	// first sight. Made before the confirmation: a cloud-config past what the cloud takes is
	// refused before the operator is asked to go on.
	userData, hostKey, err := in.seedUserData(p, &mach, st, ca, st.AwaitSecrets || login != nil)
	if err != nil {
		return err
	}
	if err := in.confirmVM(ctx, p, format, mach, st, given, seed); err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	ref, err := in.seedDisk(cl, prefix, format)
	if err != nil {
		return err
	}
	if boot.release != m.Tag {
		why := "published none"
		if len(m.Artifacts.Disks) > 0 {
			why = "has none left in " + prefix + " (a later ship's disk set replaced them)"
		}
		o.logf("fetching %s's %s disk of %s (%s): %s %s %s. The VM boots %s's OS first; its first boot writes %s's OS bundle and reboots into it, and an install on the VM is refused until then",
			name, format, boot.release, registry.Short(ref.DigestStr()), name, m.ProjectVersion, why, boot.release, m.Tag)
	} else {
		o.logf("fetching %s's %s disk (%s)", name, format, registry.Short(ref.DigestStr()))
	}
	fallback := fmt.Sprintf("%s-%s.%s", name, boot.release, format)
	disk, _, err := cl.Download(ctx, ref, release.DiskArtifactType, release.DiskMediaType(format), out, fallback, c.Progress)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if hostKey != nil {
		mode = 0o600 // it holds the host key
	}
	if p.ISO {
		err = WriteSeedISO(seed, userData, MetaData(mach), mode)
	} else {
		err = writeAtomic(seed, bytes.NewReader(userData), mode, nil)
	}
	if err != nil {
		return err
	}
	ctl := in.R.Ctl()
	fmt.Fprintf(o.Out, "Wrote %s and %s.\n\nOn %s:\n", disk, seed, p.Label)
	if n := in.vmSize().note(name); n != "" {
		fmt.Fprintf(o.Out, "  Size the VM: %s.\n", n)
	}
	app := in.vmSize().def.gib
	fmt.Fprintf(o.Out, "  The disk: %s.\n", in.diskSplit(format, app))
	for n, s := range p.Steps {
		fmt.Fprintf(o.Out, "  %d. %s\n", n+1, fmt.Sprintf(s, disk, seed, in.boot.diskGiB(format, app)))
	}
	if len(mach.Interfaces) > 1 {
		fmt.Fprintf(o.Out, "  Its NICs, each with its MAC (the cloud-config names each by it):\n")
		for _, i := range mach.Interfaces {
			on := "the primary's network"
			switch {
			case i.Primary:
				on = "the network of its default route"
			case i.Attach != "":
				on = i.Attach
			}
			fmt.Fprintf(o.Out, "    %s  %s  on %s\n", i.Role, i.MAC, on)
		}
	}
	if login != nil {
		fmt.Fprintf(o.Out, "\nIts reads of %s take the login --registry-login-file gave, which the cloud-config never carries: once it answers, %s\n", login.Host, loginSteps(mach.User, "<address>", ctl))
		if given == nil && !st.AwaitSecrets {
			fmt.Fprint(o.Out, seedHostKeyNote(hostKey, "<address>"))
		}
	}
	if given != nil {
		if err := writeGiven(givenFile, givenPin, given, hostKey); err != nil {
			return err
		}
		fmt.Fprintf(o.Out, "\nIts first install waits for %s, which the cloud-config never carries. %s holds them, for you alone: once the first boot is done (a VM boots its recovery system, then reboots), run, with its address for <address>:\n  %s\n", given.names(), givenFile, givenSteps(mach.User, "<address>", ctl, givenFile, givenPin, c.SSHKey))
		fmt.Fprintf(o.Out, "%s\n", givenRetired)
		if st.Release != nil {
			fmt.Fprintf(o.Out, "Its disk carries %s: its first boot also writes %s's OS and reboots into it, and the step is for that OS (`%s status` over SSH names it).\n", boot.release, m.Tag, ctl)
		}
		fmt.Fprint(o.Out, seedHostKeyNote(hostKey, "<address>"))
		return nil
	}
	if st.AwaitSecrets {
		fmt.Fprintf(o.Out, "\nIts first install waits for its secrets (the file `secrets export` wrote, decrypted), which the cloud-config never carries: %s\n", secretsSteps(mach.User, "<address>", ctl))
		fmt.Fprint(o.Out, seedHostKeyNote(hostKey, "<address>"))
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

// createLogin checks vm create's --registry-login-file, raw (nil: none), for the VM reading prefix:
// its one entry must be for the host of the release's own registry or of the mirror, and the
// first boot must read without it — it reaches the VM over SSH only after that boot.
func (in *Installer) createLogin(raw []byte, prefix string) (*registryLogin, error) {
	if raw == nil {
		return nil, nil
	}
	if len(raw) > maxRegistryLogin {
		return nil, fmt.Errorf("--registry-login-file is over %d KiB: one entry is a few hundred bytes", maxRegistryLogin>>10)
	}
	l, err := parseRegistryLogin(raw)
	if err != nil {
		return nil, fmt.Errorf("--registry-login-file: %w", err)
	}
	own, mirror := in.R.Spec.Registry.Prefix, ""
	if in.O.Registry != "" && prefix != own {
		mirror = prefix
	}
	if err := loginReads(l.Host, own, mirror, "the VM"); err != nil {
		return nil, fmt.Errorf("--registry-login-file: %w", err)
	}
	m := in.R.Manifest
	if in.boot.release == m.Tag {
		return &l, nil
	}
	// The first boot of another release's disk set reads m's OS bundle from prefix, before the
	// login reaches the VM over SSH: with the account baked into the installer on the release's
	// own registry, with none on a mirror.
	switch {
	case mirror != "" && strings.EqualFold(l.Host, registryHost(mirror)):
		return nil, fmt.Errorf("--registry-login-file: the VM would boot %s's disk set, and its first boot reads %s's OS bundle from the mirror before the login reaches it over SSH: give the mirror %s's own disk set (`mirror export --disks`, then `mirror import`), or use a mirror that reads without a sign-in", in.boot.release, m.Tag, m.Tag)
	case mirror == "" && in.R.Spec.Registry.Pull.Name == "token" && (in.R.Pull == nil || in.R.Pull.Password == ""):
		return nil, fmt.Errorf("--registry-login-file: the VM would boot %s's disk set, and its first boot reads %s's OS bundle from %s before the login reaches it over SSH, with the pull account this installer does not carry: publish %s's own disk set, or use the installer of a release that carries its pull account", in.boot.release, m.Tag, prefix, m.Tag)
	}
	return &l, nil
}

// loginSteps tells how to give a VM its login to its registry by hand, from the login file FILE.
func loginSteps(user, addr, ctl string) string {
	return fmt.Sprintf("ssh %s@%s sudo %s registry-login - < FILE", user, addr, ctl)
}

// seedable refuses a release a VM's seed cannot be written for: one that publishes no OS bundle,
// or whose front offers no TLS mode a VM takes.
func (in *Installer) seedable() error {
	m := in.R.Manifest
	in.givenCA = true
	if in.R.Spec.Shapes.Kairos == nil || m.Artifacts.OS == "" {
		return fmt.Errorf("%s %s publishes no OS bundle (shapes.kairos)", in.R.Spec.Name, m.ProjectVersion)
	}
	if in.R.Spec.HasFront() && len(in.createModes()) == 0 {
		return in.noCreateMode()
	}
	return nil
}

// seedRegistry is the registry a VM's disk comes from, its prefix and its client: the mirror when
// one is given (--registry), as the VM's updates do. The disk set the VM boots is read from it
// into in.boot (bootDisks): one holding a format of the platform keyed *platform, when it is one;
// none given and a prompter there, the platform is asked first (askPlatform) and *platform set,
// the set then taken from the sets its walk read.
func (in *Installer) seedRegistry(ctx context.Context, platform *string) (string, *registry.Client, error) {
	prefix := in.R.Spec.Registry.Prefix
	if r := in.O.Registry; r != "" {
		prefix = strings.TrimSuffix(r, "/")
		if _, err := registry.Repository(prefix); err != nil {
			return "", nil, fmt.Errorf("--registry: %w", err)
		}
	}
	cl, err := in.registryClient(prefix)
	if err != nil {
		return "", nil, err
	}
	var walked *diskWalk
	if *platform == "" && in.O.Prompt != nil {
		if *platform, walked, err = in.askPlatform(ctx, cl, prefix); err != nil {
			return "", nil, err
		}
	}
	var formats []string
	if j := slices.IndexFunc(Platforms, func(p Platform) bool { return p.Key == *platform }); j >= 0 {
		formats = Platforms[j].Formats
	}
	// A walk an error stopped before a set holding the platform's format is walked again, as with
	// --platform: the source that stopped it decides.
	if walked != nil && (walked.stopped == nil || slices.ContainsFunc(walked.sets, func(d bootDisk) bool { return d.holdsOne(formats) })) {
		in.boot, err = walked.pick(formats)
	} else {
		in.boot, err = in.bootDisks(ctx, cl, prefix, formats)
	}
	if err != nil {
		return "", nil, err
	}
	return prefix, cl, nil
}

// askPlatform asks where the VM runs, offering every platform some disk set this release boots
// holds a format of, in Platforms' order, and returns the walk of the sets it read (walkDisks, one
// walk, until every platform's formats are held or no set is left), which serves the platform's
// lookup (pick). None offered asks nothing: the lookup of any set then says why. A source that
// fails to be read or checked ends the walk (w.stopped) without failing the question, which then
// offers the platforms of the sets read before it and notes why; with none read, it is the error.
func (in *Installer) askPlatform(ctx context.Context, cl *registry.Client, prefix string) (string, *diskWalk, error) {
	var all []string
	for _, p := range Platforms {
		all = append(all, p.Formats...)
	}
	var read *diskWalk
	w, err := in.walkDisks(ctx, cl, prefix, func(w *diskWalk) bool {
		read = w
		return !slices.ContainsFunc(all, func(f string) bool { return !w.holds(f) })
	})
	if err != nil {
		if read == nil { // no set read
			return "", nil, err
		}
		w = read
		w.stopped = err
	}
	var choices []Choice
	for _, p := range Platforms {
		if slices.ContainsFunc(p.Formats, w.holds) {
			choices = append(choices, Choice{p.Key, p.Label})
		}
	}
	if len(choices) == 0 {
		return "", w, nil
	}
	if w.stopped != nil {
		in.O.logf("note: the platforms offered are those of the disk sets read before one that could not be: %v", w.stopped)
	}
	a, err := in.O.Prompt.Ask(ctx, Form{
		Questions: []Question{{Title: "Where does the VM run", Key: "--platform", Choices: choices}},
		Defaults:  func(map[string]string) map[string]string { return map[string]string{"--platform": choices[0].Value} },
	})
	if err != nil {
		return "", nil, err
	}
	return cmp.Or(a["--platform"], choices[0].Value), w, nil
}

// seedSettings are what a VM's cloud-config gives on platform p, its disk from the registry at
// prefix: the machine (machine), the first install's settings (vmSettings; what the install
// awaits over SSH, awaitOverSSH: a secrets export when secrets names the flag asking one, the
// given material vmSettings returns, a seed's byo certificate), and the mirror's CA, which ca
// holds when --registry-ca names one. waiting are the secret inputs the install needs that
// nothing gives.
func (in *Installer) seedSettings(ctx context.Context, c *VMCreate, p Platform, prefix, secrets string) (mach Machine, st *Settings, waiting []string, given *Given, ca []byte, err error) {
	o, m := &in.O, in.R.Manifest
	if mach, err = in.machine(ctx, c, p); err != nil {
		return
	}
	if st, waiting, given, err = in.vmSettings(ctx); err != nil {
		return
	}
	if err = awaitOverSSH(st, secrets, in.seeding && o.TLS == "byo", given); err != nil {
		return
	}
	st.IPv6 = c.IPv6 // the install's network too: the front's IPv6 clients keep their address
	st.TuneProfile = c.TuneProfile
	// Another release's disk: the first boot takes this release's OS bundle before it installs.
	if in.boot.release != m.Tag {
		st.Release = &OSRelease{Version: m.ProjectVersion, Revision: m.Revision, Tag: m.Tag, OS: m.Artifacts.OS}
	}
	// A mirror: the disk comes from it, and the VM's updates.
	if o.Registry != "" {
		st.Registry = prefix
	}
	if o.RegistryCA != "" {
		if ca, err = registry.ReadCA(o.RegistryCA); err != nil {
			err = fmt.Errorf("--registry-ca: %w", err)
			return
		}
		st.RegistryCA = VMRegistryCA(in.R.Spec.Name)
	}
	return
}

// awaitOverSSH sets what a new VM's first install waits for over SSH, pinned to the one-time host
// key its cloud-config carries, the one rule of `vm create` and `vm seed`: a decrypted secrets
// export (`install --secrets -`) when secrets names the flag asking one (--secrets, --await-secrets;
// "": none); the secret inputs and TLS material (`install --given -`) when given holds any (the
// --answers file's secret inputs, a certificate) or byo (a seed's --tls byo, whose certificate
// goes after the boot), the install then taking byo when a certificate goes, else its own mode.
// A secrets export beside given material is refused: the install waits for one of them.
func awaitOverSSH(st *Settings, secrets string, byo bool, given *Given) error {
	if secrets != "" && (byo || given != nil) {
		var what, fix []string
		// A certificate goes in byo mode alone, which is what is left out: without it, its
		// certificate is refused as being for byo, naming the flags.
		cert := byo
		if given != nil {
			what = append(what, given.names())
			if len(given.Inputs) > 0 {
				fix = append(fix, "remove "+strings.Join(sortedKeys(given.Inputs), ", ")+" from the --answers file")
			}
			cert = cert || given.TLS != nil && given.TLS.Cert != ""
			if given.TLS != nil && given.TLS.ClientCA != "" {
				fix = append(fix, "leave out the client CA (--client-ca and the --answers file's tls client_ca)")
			}
		}
		if byo {
			what = append(what, "--tls byo")
		}
		if cert {
			fix = append(fix, "leave out --tls byo (choose another --tls mode)")
		}
		return refuse(strings.Join(fix, " and ")+", or leave out "+secrets,
			"%s and %s: the first install waits for one of them over SSH, a secrets export (install --secrets -) or what install --given - gives", secrets, strings.Join(what, ", "))
	}
	st.AwaitSecrets = secrets != "" || byo || given != nil
	switch {
	case byo:
		st.AwaitGiven = "byo"
	case given != nil:
		st.AwaitGiven = given.awaits()
	}
	return nil
}

// seedUserData is the VM's cloud-config (UserData) for platform p, checked to fit what p takes;
// withKey, it carries a one-time SSH host key generated here (mach.HostKey), which key is.
func (in *Installer) seedUserData(p Platform, mach *Machine, st *Settings, ca []byte, withKey bool) (userData []byte, key *seedHostKey, err error) {
	if withKey {
		if key, err = newSeedHostKey(mach.Hostname); err != nil {
			return nil, nil, err
		}
		mach.HostKey = key.Private
	}
	if userData, err = UserData(in.R.Spec.Name, in.R.Ctl(), *mach, *st, ca); err != nil {
		return nil, nil, err
	}
	if err := fileInputsFit(p, in.R.Spec, st, len(userData)); err != nil {
		return nil, nil, err
	}
	return userData, key, nil
}

// seedDisk is the disk of format the VM boots (in.boot), in the registry at prefix, by digest.
func (in *Installer) seedDisk(cl *registry.Client, prefix, format string) (name.Digest, error) {
	return cl.Digest(prefix+"/"+in.R.Spec.Name+"-disk", in.boot.disks[format])
}

// bootDisk is the disk set a VM boots: the release tag the disks are under, and each disk's digest
// by format.
type bootDisk struct {
	release string
	disks   map[string]string
	// osMiB is what Kairos takes of a VM's disk besides the app's space, by format: the formats
	// the registry holds, each recording its size.
	osMiB map[string]int64
	// missing is why each other format of the set boots nothing from this registry.
	missing map[string]error
}

// osGiB is what Kairos takes of a VM's disk of format, with room for the partition tables and
// their alignment: the app keeps every GiB it asked for.
func (b bootDisk) osGiB(format string) int {
	return int((b.osMiB[format] + diskSlackMiB + 1023) / 1024)
}

// diskSlackMiB is the room osGiB leaves for the GPT and the partitions' alignment.
const diskSlackMiB = 256

// maxOSMiB bounds what a disk may record Kairos takes of it: 16 TiB.
const maxOSMiB = 16 << 20

// osMiBOf is a disk's DiskOSAnnotation as MiB, 1 to maxOSMiB; false for anything else.
func osMiBOf(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n > 0 && n <= maxOSMiB
}

// diskGiB is the disk a VM of format is made with: the app's space and what Kairos takes; an
// OVA's never under the capacity it declares (kairos.OVACapacityMiB), which no platform shrinks.
func (b bootDisk) diskGiB(format string, app int) int {
	n := app + b.osGiB(format)
	if format == "ova" {
		n = max(n, int((kairos.OVACapacityMiB(b.osMiB[format])+1023)/1024))
	}
	return n
}

// switchingStowage is the first stowage whose OS's first boot takes another release's OS bundle
// (Settings.Release): a disk set an older one published cannot boot a VM of another release.
const switchingStowage = "0.6.0"

// bootDisks is the disk set a VM of this release boots, read in the registry at prefix: the
// release's own disks, or the set it records (a release that published none boots its channel's);
// when they are no longer there (a newer set replaced them), the set of the channel pointer's
// release, its manifest signed by a key this installer trusts, else the channel's disk set
// record, signed too. A set is taken when the registry holds one of formats (nil: of any format);
// when none does, the first set holding any format is, so that the platform names the format
// this registry lacks.
func (in *Installer) bootDisks(ctx context.Context, cl *registry.Client, prefix string, formats []string) (bootDisk, error) {
	w, err := in.walkDisks(ctx, cl, prefix, func(w *diskWalk) bool { return w.sets[len(w.sets)-1].holdsOne(formats) })
	if err != nil {
		return bootDisk{}, err
	}
	return w.pick(formats)
}

// diskWalk is the disk sets walkDisks read, in bootDisks' order, each one a VM of this release can
// boot and holding a format with its size.
type diskWalk struct {
	sets []bootDisk
	// own: sets[0] is the release's own set, which this release boots whatever formats it holds;
	// the walk ends there.
	own bool
	// end is why no further set was found, set when the walk read every source.
	end error
	// stopped is the error that ended the walk before every source was read (askPlatform's walk).
	stopped error
}

// holds reports whether a set of the walk holds format f.
func (w *diskWalk) holds(f string) bool {
	return slices.ContainsFunc(w.sets, func(d bootDisk) bool { _, ok := d.osMiB[f]; return ok })
}

// holdsOne reports whether the registry holds one of formats of d (nil: any).
func (d bootDisk) holdsOne(formats []string) bool {
	return formats == nil || slices.ContainsFunc(formats, func(f string) bool { _, ok := d.osMiB[f]; return ok })
}

// pick is bootDisks' set among those w read: the release's own set, else the first holding one of
// formats (nil: of any), else the first; when w read none, why.
func (w *diskWalk) pick(formats []string) (bootDisk, error) {
	if len(w.sets) == 0 {
		return bootDisk{}, w.end
	}
	if !w.own {
		if i := slices.IndexFunc(w.sets, func(d bootDisk) bool { return d.holdsOne(formats) }); i >= 0 {
			return w.sets[i], nil
		}
	}
	return w.sets[0], nil
}

// walkDisks reads, in the registry at prefix, the disk sets a VM of this release may boot, in
// bootDisks' order: the release's own (or the set it records), where the walk ends when it is
// there; else the channel pointer's release's, its manifest signed by a key this installer
// trusts; else the channel's disk set record, signed too. After each set it reads, enough(w)
// stops the walk. An error is a read or a check that failed; a walk that read every source sets
// w.end, a noBootDisk unless the channel's release could not be read.
func (in *Installer) walkDisks(ctx context.Context, cl *registry.Client, prefix string, enough func(*diskWalk) bool) (*diskWalk, error) {
	m, name := in.R.Manifest, in.R.Spec.Name
	w := &diskWalk{}
	// cannot is why a set found could not boot this release (an older stowage's, or one recording
	// no OS size), said when no other one can.
	var cannot error
	// present is the size Kairos takes of each disk of the set the registry holds, by format, and
	// why each other format is not there (a mirror exported with --disks carries some formats
	// alone) or records no size (published by an earlier Stowage).
	present := func(disks map[string]string) (map[string]int64, map[string]error, error) {
		sizes, missing := map[string]int64{}, map[string]error{}
		for _, f := range slices.Sorted(maps.Keys(disks)) {
			ref, err := cl.Digest(prefix+"/"+name+"-disk", disks[f])
			if err != nil {
				return nil, nil, err
			}
			ann, _, ok, err := cl.Annotations(ctx, ref)
			if err != nil {
				return nil, nil, fmt.Errorf("the %s disk: %w", f, err)
			}
			if !ok {
				missing[f] = fmt.Errorf("the %s disk %s is not in %s (a mirror carries the formats its `mirror export --disks` named)", f, registry.Short(ref.DigestStr()), prefix)
				continue
			}
			n, valid := osMiBOf(ann[release.DiskOSAnnotation])
			if !valid {
				what := fmt.Sprintf("records no %s: published by an earlier Stowage", release.DiskOSAnnotation)
				if s, set := ann[release.DiskOSAnnotation]; set {
					what = fmt.Sprintf("records %s %q, not a size in MiB (1 to %d): not as Stowage publishes it", release.DiskOSAnnotation, s, maxOSMiB)
				}
				missing[f] = fmt.Errorf("the %s disk %s %s, the publisher ships a new revision with its disks", f, registry.Short(ref.DigestStr()), what)
				cannot = cmp.Or(cannot, missing[f])
				continue
			}
			sizes[f] = n
		}
		return sizes, missing, nil
	}
	// usable reads the set src boots into w when the registry holds a format of it this release
	// can boot, and reports whether the walk ends there.
	usable := func(src *release.Manifest) (bool, error) {
		tag, disks := src.Artifacts.Boot(src.Tag)
		if len(disks) == 0 {
			return false, nil
		}
		sizes, missing, err := present(disks)
		if err != nil || len(sizes) == 0 {
			return false, err
		}
		if tag != m.Tag {
			by := src.BootStowage()
			if old, err := (version.SemVer{}).Compare(by, switchingStowage); err != nil || old < 0 {
				cannot = cmp.Or(cannot, fmt.Errorf("the disk set %s boots is %s's, published by stowage %q, whose first boot cannot take another release's OS: the publisher ships a release with its disks (stowage ship --disks)",
					src.Tag, tag, by))
				return false, nil
			}
		}
		w.sets = append(w.sets, bootDisk{tag, disks, sizes, missing})
		return enough(w), nil
	}
	// The release's own set, the registry holding some of it: it is the one this release boots,
	// never another release's, and a platform whose format it lacks refuses it naming that format.
	if _, err := usable(m); err != nil {
		return nil, err
	}
	if len(w.sets) > 0 {
		w.own = true
		return w, nil
	}
	trust, err := trusted("this installer", in.R.Keys)
	if err != nil {
		return nil, err
	}
	repo := prefix + "/" + name + "-release"
	ptr, h, err := readRelease(ctx, cl, repo, m.Channel)
	// unread is why the channel's release was not read: then "no disk" is no answer.
	var unread error
	if err != nil {
		// Unread (another stowage's), it boots nothing here: the record may.
		in.O.logf("the channel %s's release could not be read (%v): its disk set record is tried", m.Channel, err)
		ptr = nil
		if !errors.As(err, new(foreignManifest)) {
			unread = err
		}
	}
	if ptr != nil && ptr.Name == name && ptr.Channel == m.Channel {
		r, err := cl.Repo(repo)
		if err != nil {
			return nil, err
		}
		if err := checkSigned(ctx, cl, r, h, ptr, trust); err != nil {
			return nil, err
		}
		if done, err := usable(ptr); err != nil || done {
			return w, err
		}
	}
	// The channel's disk set record, which only a ship publishing disks writes: signed as a
	// release is when the installer trusts a key.
	setTag, err := cl.Tag(prefix+"/"+name+"-disk", release.DiskSetTag(m.Channel))
	if err != nil {
		return nil, err
	}
	a, h, err := cl.GetArtifact(ctx, setTag, release.DiskSetMediaType)
	if err != nil && !errors.Is(err, registry.ErrNotArtifact) {
		return nil, err
	}
	if a != nil {
		var set release.DiskSet
		if err := json.Unmarshal(a.Data, &set); err != nil {
			return nil, fmt.Errorf("%s: %w", setTag, err)
		}
		if err := set.Check(m.Channel); err != nil {
			return nil, fmt.Errorf("%s: %w", setTag, err)
		}
		if trust != nil {
			r, err := cl.Repo(prefix + "/" + name + "-disk")
			if err != nil {
				return nil, err
			}
			if err := trust.Verify(ctx, cl, r.Digest(h.String())); err != nil {
				return nil, fmt.Errorf("%s is not signed by a key this installer trusts: %w", setTag, err)
			}
		}
		rec := &release.Manifest{Tag: set.Release, StowageVersion: set.Stowage, Artifacts: release.Artifacts{Disks: set.Disks}}
		if done, err := usable(rec); err != nil || done {
			return w, err
		}
	}
	switch {
	case unread != nil:
		w.end = fmt.Errorf("%s %s has no disk in %s, and its channel %s's release could not be read: %w", name, m.ProjectVersion, prefix, m.Channel, unread)
	case cannot != nil:
		w.end = noBootDisk{cannot}
	default:
		w.end = noBootDisk{fmt.Errorf("%s %s has no disk in %s, nor does its channel %s: the publisher ships a release with its disks (stowage ship --disks)", name, m.ProjectVersion, prefix, m.Channel)}
	}
	return w, nil
}

// noBootDisk is bootDisks' answer that the registry holds no disk set a VM of the release boots,
// told apart from a registry that could not be read.
type noBootDisk struct{ error }

func (e noBootDisk) Unwrap() error { return e.error }

// firstInstallOverSSH is the command `vm create` runs on the VM over SSH, what flag (--secrets or
// --given) takes on its standard input. The install comes after the boot unit, which runs once the release's images are
// loaded (`systemctl start` waits for it, or returns at once once it ran). A start whose job the
// boot cancels (`Job for <name>.service canceled.`, the boot's own start of it still queued) is
// tried once more, after the boot's jobs are done (waited for 10 min at most); a unit that failed
// is not run again, and a second start that fails fails the install too.
func firstInstallOverSSH(name, ctl, flag string) []string {
	unit := name + ".service"
	return []string{"sudo", "systemctl", "start", unit, "||", "{", "sudo", "timeout", "600", "systemctl", "is-system-running", "--wait", ">/dev/null", ";",
		"!", "systemctl", "is-failed", "--quiet", unit, "&&", "sudo", "systemctl", "start", unit, ";", "}", "&&", "sudo", ctl, "install", flag, "-", "--no-input"}
}

// secretsSteps tells how to give a VM's install its secrets file: copied over, its mode kept
// 0600, then an install on a terminal, which can ask what the file does not hold.
func secretsSteps(user, addr, ctl string) string {
	// scp reads an IPv6 address's colons as the path's, unless bracketed.
	return fmt.Sprintf("chmod 600 FILE; scp -p FILE %[1]s@%[4]s:secrets.yaml, then ssh -t %[1]s@%[2]s 'sudo %[3]s install --secrets secrets.yaml; rm secrets.yaml'", user, addr, ctl, bracketIPv6(addr))
}

// sshAllows warns when the SSH sources leave out the address this machine reaches addr from:
// `vm create` waits for the VM, and the secrets steps run, over SSH. A warning, not a refusal:
// the VM may see this machine at another address (NAT, a VPN). A name that does not resolve yet
// (one the VM's DHCP lease registers) is not checked.
func (in *Installer) sshAllows(addr string, m Machine) {
	// The public address the sources hold is not this machine's own, behind a NAT: nothing to
	// compare with the route's source.
	allow := m.SSHAllow
	if len(allow) == 0 || m.publicSSH {
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
// platform's formats the registry holds. Only the platforms the registry has a disk for are
// offered (askPlatform asked one, when none was given); one whose formats the release publishes
// but the registry lacks is told which.
func (in *Installer) platform(ctx context.Context, key string) (Platform, string, error) {
	held := in.boot.osMiB
	var offered []Platform
	for _, p := range Platforms {
		if slices.ContainsFunc(p.Formats, func(f string) bool { _, ok := held[f]; return ok }) {
			offered = append(offered, p)
		}
	}
	if len(offered) == 0 {
		return Platform{}, "", fmt.Errorf("no platform `vm create` knows boots the disk formats %s %s publishes", in.R.Spec.Name, in.R.Manifest.ProjectVersion)
	}
	// A platform whose disk the release publishes but the registry lacks says which.
	if j := slices.IndexFunc(Platforms, func(p Platform) bool { return p.Key == key }); j >= 0 && !slices.ContainsFunc(offered, func(p Platform) bool { return p.Key == key }) {
		for _, f := range Platforms[j].Formats {
			if why := in.boot.missing[f]; why != nil {
				return Platform{}, "", refuse(platformFix(offered), "--platform %s: %w; this registry offers %s", key, why, platformKeys(offered))
			}
		}
	}
	if err := checkPlatform(key, offered, fmt.Sprintf("%s %s offers", in.R.Spec.Name, in.R.Manifest.ProjectVersion)); err != nil {
		return Platform{}, "", err
	}
	p := offered[slices.IndexFunc(offered, func(p Platform) bool { return p.Key == key })]
	f := p.Formats[slices.IndexFunc(p.Formats, func(f string) bool { _, ok := held[f]; return ok })]
	return p, f, nil
}

// checkPlatform refuses a --platform key that is empty, or none of among's (the platforms a run
// can make a VM for); offers names among in its message.
func checkPlatform(key string, among []Platform, offers string) error {
	switch {
	case key == "":
		return refuse(platformFix(among), "--platform is required (%s)", platformKeys(among))
	case !slices.ContainsFunc(among, func(p Platform) bool { return p.Key == key }):
		return refuse(platformFix(among), "--platform %s: %s %s", key, offers, platformKeys(among))
	}
	return nil
}

func platformKeys(ps []Platform) string {
	var ks []string
	for _, p := range ps {
		ks = append(ks, p.Key)
	}
	return strings.Join(ks, ", ")
}

func platformFix(ps []Platform) string { return "give --platform one of " + platformKeys(ps) }

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

// devicePath is a disk's path the installer takes: under /dev/, of the characters udev's names use.
var devicePath = regexp.MustCompile(`^/dev/[A-Za-z0-9][A-Za-z0-9_.:+@/=-]*$`)

// checkDevice checks --device: auto, or a path under /dev/ with no "..".
func checkDevice(v string) error {
	if v == "auto" || devicePath.MatchString(v) && !strings.Contains(v, "..") {
		return nil
	}
	return fmt.Errorf("--device %q: a disk's path under /dev/ (e.g. /dev/disk/by-id/nvme-…, /dev/sda), or auto", v)
}

// macAddr is a MAC address as networkd's and the platforms' files write it.
var macAddr = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)

// generatedMAC is a random, locally administered unicast MAC: two VMs of one host name on one
// network never share one. Each run of vm create or vm seed draws its own, written into its seed
// (and vm seed's interfaces, which the program creating the VM gives its NICs): the NICs take the
// MACs the run that wrote their seed gave.
func generatedMAC() (string, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4]), nil
}

// unicastMAC reports whether mac (as macAddr reads it) is a unicast address, its first octet's
// low bit clear, and not all zeros.
func unicastMAC(mac string) bool {
	first, err := strconv.ParseUint(mac[:2], 16, 8)
	return err == nil && first&1 == 0 && mac != "00:00:00:00:00:00"
}

// machineInterfaces are the release's host.interfaces NICs on the machine p creates, from
// --interface: each secondary's addressing (none by default: the link up, no address), each NIC's
// MAC and the network it attaches to. A VM of one such NIC needs no MAC: it is named from the NIC
// it has, and its first boot pins that name to the NIC's MAC (pinScript). Of more, each is named
// by its MAC: given, else generated on a hypervisor (which creates the NICs with them). A server's are its own, given for each NIC (it may have more ports). A
// cloud names its NICs' MACs itself: it takes one, and no MAC. An optional NIC is made only when
// --interface names it. Nil when the release declares none.
func machineInterfaces(s *spec.Spec, given []string, p Platform) ([]MachineInterface, error) {
	var roles []string
	primary := ""
	if s.Host != nil {
		for _, r := range slices.Sorted(maps.Keys(s.Host.Interfaces)) {
			if i := s.Host.Interfaces[r]; i.Kind == "" || i.Kind == spec.InterfaceNIC {
				roles = append(roles, r)
				if i.Primary {
					primary = r
				}
			}
		}
	}
	if len(roles) == 0 {
		if len(given) > 0 {
			return nil, fmt.Errorf("--interface: %s declares no NIC (host.interfaces)", s.Name)
		}
		return nil, nil
	}
	cloud := !p.ISO
	required := slices.DeleteFunc(slices.Clone(roles), func(r string) bool { return s.Host.Interfaces[r].Optional })
	if cloud && len(required) > 1 {
		return nil, fmt.Errorf("%s names its VMs' NICs itself, one here: %s requires %d (host.interfaces: %s): a hypervisor's or a server's VM takes it", p.Label, s.Name, len(required), strings.Join(required, ", "))
	}
	byRole := map[string]MachineInterface{}
	for _, r := range roles {
		byRole[r] = MachineInterface{Role: r, Primary: r == primary, Mode: IfaceNone}
	}
	set := map[string]bool{}
	for _, g := range given {
		role, rest, _ := strings.Cut(g, "=")
		i, ok := byRole[role]
		switch {
		case !ok:
			return nil, fmt.Errorf("--interface %s: %q is not one of %s's NICs (%s)", g, role, s.Name, strings.Join(roles, ", "))
		case set[role]:
			return nil, fmt.Errorf("--interface %s: given twice", role)
		}
		set[role] = true
		for n, f := range strings.Split(rest, ",") {
			k, val, kv := strings.Cut(f, "=")
			switch {
			case n == 0 && !kv && (f == IfaceDHCP || f == IfaceStatic || f == IfaceNone):
				if i.Primary {
					return nil, fmt.Errorf("--interface %s: the primary NIC's addressing is --network's", g)
				}
				i.Mode = f
			case k == "address" && !i.Primary:
				pfx, err := netip.ParsePrefix(val)
				if err != nil || !pfx.Addr().Is4() || pfx.Bits() < 1 {
					return nil, fmt.Errorf("--interface %s: address %q is not an IPv4 address with its prefix (192.0.2.10/24)", g, val)
				}
				i.Address = pfx.String()
			case k == "mac":
				if v := strings.ToLower(val); cloud {
					return nil, fmt.Errorf("--interface %s: mac=: %s gives its VMs' NICs their MACs", g, p.Label)
				} else if macAddr.MatchString(v) && unicastMAC(v) {
					i.MAC = v
				} else {
					return nil, fmt.Errorf("--interface %s: mac %q is not a unicast MAC address (02:00:00:00:00:01)", g, val)
				}
			case k == "attach" && p.ISO && !p.Install:
				if !hostNetName.MatchString(val) {
					return nil, fmt.Errorf("--interface %s: attach %q: the platform's network or bridge, letters, digits and _ . : / -", g, val)
				}
				i.Attach = val
			default:
				return nil, fmt.Errorf("--interface %s: %q: dhcp, static or none (not the primary's), then address=A/P (static), mac=, attach= (a hypervisor's network)", g, f)
			}
		}
		if (i.Mode == IfaceStatic) != (i.Address != "") {
			return nil, fmt.Errorf("--interface %s: address= is the static mode's, and the static mode's address is required", g)
		}
		byRole[role] = i
	}
	notAsked := func(r string) bool { return s.Host.Interfaces[r].Optional && !set[r] }
	var skipped []string
	for _, r := range roles {
		if notAsked(r) {
			skipped = append(skipped, r)
		}
	}
	roles = slices.DeleteFunc(roles, notAsked)
	if cloud && len(roles) > 1 {
		asked := slices.DeleteFunc(slices.Clone(roles), func(r string) bool { return slices.Contains(required, r) })
		return nil, fmt.Errorf("--interface %s: %s names its VMs' NICs itself, one here: leave the optional %s out", asked[0], p.Label, strings.Join(asked, ", "))
	}
	out := make([]MachineInterface, 0, len(roles))
	macs := map[string]string{}
	for _, r := range roles {
		i := byRole[r]
		switch {
		case i.MAC != "":
		case p.Install:
			also := ""
			if len(skipped) > 0 {
				also = fmt.Sprintf("; the optional %s too, when given: --interface %s=mac=…", strings.Join(skipped, ", "), skipped[0])
			}
			return nil, fmt.Errorf("--interface %s=mac=…: a server names each NIC the release names (%s) by its MAC, as it may have more ports than those%s", r, strings.Join(roles, ", "), also)
		case !cloud && len(roles) > 1:
			mac, err := generatedMAC()
			if err != nil {
				return nil, err
			}
			i.MAC = mac
		}
		if other, dup := macs[i.MAC]; dup && i.MAC != "" {
			return nil, fmt.Errorf("--interface: %s and %s have the MAC %s", other, r, i.MAC)
		}
		macs[i.MAC] = r
		out = append(out, i)
	}
	return out, nil
}

// hostNetName is a platform's network or bridge name, as --interface's attach= gives it.
var hostNetName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:/-]{0,63}$`)

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
func (in *Installer) machine(ctx context.Context, c *VMCreate, p Platform) (Machine, error) {
	o := &in.O
	given := map[string]string{"--hostname": c.Hostname, "--user": c.User, "--ssh-key": c.SSHKey, "--network": c.Network, "--device": c.Device}
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
	switch {
	case p.Install:
		qs = append(qs, Question{Title: "The disk the OS installs on, wiped (a /dev/disk/by-id/ path names it whatever the boot order; auto: the largest disk)", Key: "--device", Required: true, Check: checkDevice})
	case c.Device != "":
		return Machine{}, fmt.Errorf("--device is for a platform whose installer installs the OS on a disk (metal), not %s: its disk image boots", p.Key)
	}
	a, err := in.askGiven(ctx, qs, given, defaults)
	if err != nil {
		return Machine{}, err
	}
	m := Machine{Hostname: a["--hostname"], User: a["--user"], Install: a["--device"]}
	if m.Interfaces, err = machineInterfaces(in.R.Spec, c.Interfaces, p); err != nil {
		return Machine{}, err
	}
	if p.Install && m.Install == "" {
		return Machine{}, errors.New("--device is required: the disk the OS installs on, wiped")
	}
	if a["--ssh-key"] == "" {
		return Machine{}, errors.New("--ssh-key is required: the administrator's SSH public key file (no ~/.ssh/id_ed25519.pub, id_ecdsa.pub or id_rsa.pub)")
	}
	if m.SSHKey, err = authorizedKey(a["--ssh-key"]); err != nil {
		return Machine{}, err
	}
	c.SSHKey = a["--ssh-key"] // the key the steps' ssh logs in with
	for _, v := range c.SSHAllow {
		p, err := netip.ParsePrefix(strings.TrimSpace(v))
		if err != nil {
			return Machine{}, fmt.Errorf("--ssh-allow: %q is not a source with its prefix (192.0.2.0/24, 2001:db8::/32)", v)
		}
		m.SSHAllow = append(m.SSHAllow, p.Masked())
	}
	switch {
	case len(m.SSHAllow) == 0 && !p.ISO:
		// A cloud's VM has a public address: SSH from this machine's alone, unless told.
		a, err := o.publicAddr(ctx)
		if err != nil {
			return Machine{}, fmt.Errorf("--ssh-allow: a cloud's VM answers on a public address, and SSH accepts this machine's alone unless given the sources (--ssh-allow CIDR,…; 0.0.0.0/0,::/0 is every source), but its public address is unknown: %w", err)
		}
		m.SSHAllow, m.publicSSH = []netip.Prefix{netip.PrefixFrom(a, a.BitLen())}, true
		o.logf("SSH accepts this machine's public address alone, %s (%s answered; ssh over another address, IPv6 or another way out of this network, is refused): --ssh-allow CIDR,… gives the operators' addresses", a, publicAddrURL)
	case len(m.SSHAllow) == 0:
		o.logf("SSH accepts every source: --ssh-allow CIDR,… limits it to the operators' addresses")
	}
	if c.Timezone != "" {
		if err := checkTimezone(c.Timezone); err != nil {
			return Machine{}, fmt.Errorf("--timezone: %w", err)
		}
		m.Timezone = c.Timezone
	}
	if m.Hosts, err = parseHosts(c.Hosts, c.IPv6); err != nil {
		return Machine{}, err
	}
	if err := checkNet(a["--network"]); err != nil {
		return Machine{}, err
	}
	if c.TuneProfile != "" {
		if _, err := tune.ParseProfile(c.TuneProfile); err != nil {
			return Machine{}, fmt.Errorf("--tune-profile: %w", err)
		}
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
// inputs of the Kairos shape in that mode — but no secret: a secret input in the answers file, and
// a byo certificate, are checked here and returned as what goes over the pinned SSH channel, never
// in Settings; a secret input --set is refused, as a command line is no place for one, and one the
// install needs is returned, for the operator to give over SSH. The VM is served over HTTPS:
// loopback, which nothing outside the VM reaches, is not offered, and byo only with its certificate
// (createModes). No input and no domain is read from the operator's environment (the pull token
// still is).
func (in *Installer) vmSettings(ctx context.Context) (*Settings, []string, *Given, error) {
	o := &in.O
	o.Preloaded = true // the VM's install: the inputs of the Kairos shape
	in.goOn, in.remote = "Go on with these answers", true
	if !in.R.Spec.HasFront() {
		if err := in.frontFlags(); err != nil {
			return nil, nil, nil, err
		}
	}
	if o.TLS == Loopback {
		return nil, nil, nil, errors.New("--tls none serves on the VM's loopback address alone, which nothing outside it reaches: give the VM an HTTPS mode")
	}
	if err := in.applyDomains(); err != nil {
		return nil, nil, nil, err
	}
	// The front's ports, as the VM's install takes them (a TLS mode's, whichever it is), before
	// the form: its first boot would refuse them, and one the VM's SSH holds.
	if in.R.Spec.HasFront() && len(in.createModes()) > 0 {
		pub, err := in.publish("")
		if err == nil {
			err = in.l4Clash(pub)
		}
		for _, k := range sortedKeys(pub.ports) {
			if err == nil && pub.ports[k] == 22 {
				err = fmt.Errorf("--%s-port 22 is the VM's SSH: choose another", k)
			}
		}
		if err != nil {
			return nil, nil, nil, err
		}
	}
	var mode string
	var values map[string]string
	var list []input
	for {
		m, asked, err := in.mode(ctx, mode)
		if err != nil {
			return nil, nil, nil, err
		}
		mode = m
		in.byo = nil
		// A seed's byo install waits for its certificate over SSH: none is read here (SeedVM refuses
		// one given, as the seed carries none).
		if mode == "byo" && !in.seeding {
			if in.byo, err = in.loadCert(ctx); err != nil {
				return nil, nil, nil, err
			}
		}
		if err := in.knownInputs(mode); err != nil {
			return nil, nil, nil, err
		}
		list = nil
		for _, i := range in.inputList(mode) {
			if _, set := o.Set[i.Key]; i.Secret && set {
				if in.seeding {
					return nil, nil, nil, seedRefusal(i.Key)
				}
				// No fix: what clears it depends on the rest of the run (an --answers file, --secrets,
				// the install's other secrets), so the reason states the rule alone.
				return nil, nil, nil, refuse("", "%s is a secret input, which a command line (--set, --set-file) never carries", i.Key)
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
			return nil, nil, nil, err
		}
		break
	}
	// The VM's install would refuse them at its first boot: refused here, before a VM exists.
	if in.R.Spec.HasFront() {
		if _, err := in.routeDomains(mode, func(k string) string { return cmp.Or(values[k], o.Set[k]) }); err != nil {
			return nil, nil, nil, err
		}
	}
	given, err := in.givenOverSSH(mode)
	if err != nil {
		return nil, nil, nil, err
	}
	// A certificate decides the mode the VM's install takes; the settings never name byo.
	shown := mode
	if mode == "byo" {
		shown = ""
	}
	st := &Settings{Channel: in.R.Manifest.Channel, TLS: shown, HTTPPort: o.HTTPPort, HTTPSPort: o.HTTPSPort}
	// An input that took a default naming another input is left to the VM's install, which
	// derives it again and records it so (DerivedVar): given here, a re-run would keep it.
	for _, i := range list {
		if v := values[i.Key]; v != "" && !in.derived[i.Key] {
			if st.Inputs == nil {
				st.Inputs = map[string]string{}
			}
			st.Inputs[i.Key] = v
		}
	}
	var waiting []string
	for _, i := range in.inputList(mode) {
		if i.Secret && i.Required && i.Default == "" && in.restoredSecret(i.Key) == "" && (given == nil || given.Inputs[i.Key] == "") {
			waiting = append(waiting, i.Key)
		}
	}
	// The install over SSH runs with no terminal to ask them.
	if given != nil && len(waiting) > 0 {
		return nil, nil, nil, fmt.Errorf("%s go over SSH, and the install also needs %s, which nothing gives: add it to the --answers file", given.names(), strings.Join(waiting, ", "))
	}
	return st, waiting, given, nil
}

// givenOverSSH collects what the VM's install in mode takes over the pinned SSH channel, each value
// checked as the install would, before a VM exists: the answers file's secret inputs, the byo
// certificate loadCert read and the client CA. nil when there is none (awaitOverSSH refuses it
// beside --secrets).
func (in *Installer) givenOverSSH(mode string) (*Given, error) {
	o := &in.O
	g := &Given{Project: in.R.Spec.Name}
	var t GivenTLS
	if c := in.byo; c != nil {
		t.Cert, t.Key, t.CA = string(c.cert), string(c.key), string(c.ca)
	}
	client, err := in.readClientCA()
	if err != nil {
		return nil, err
	}
	t.ClientCA = string(client)
	if t != (GivenTLS{}) {
		g.TLS = &t
	}
	for _, i := range in.inputList(mode) {
		v := o.Answers[i.Key]
		if !i.Secret || v == "" {
			continue
		}
		if err := i.checkValue(v); err != nil {
			return nil, fmt.Errorf("%s: %w", i.Key, err)
		}
		if g.Inputs == nil {
			g.Inputs = map[string]string{}
		}
		g.Inputs[i.Key] = v
	}
	if g.TLS == nil && g.Inputs == nil {
		return nil, nil
	}
	raw, err := marshalGiven(g)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxSecretsFile {
		return nil, fmt.Errorf("%s come to %d bytes, and the install over SSH takes %d at most: give the rest over SSH once the VM answers", g.names(), len(raw), maxSecretsFile)
	}
	return g, nil
}

// diskSplit says the disk a VM of format gets for app GiB of the app's: "a 43 GiB disk (the OS 23
// GiB, the app 20 GiB)".
func (in *Installer) diskSplit(format string, app int) string {
	total, kairosGiB := in.boot.diskGiB(format, app), in.boot.osGiB(format)
	return fmt.Sprintf("a %d GiB disk (the OS %d GiB, the app %d GiB)", total, kairosGiB, total-kairosGiB)
}

// confirmVM shows what will be written, and on a terminal asks to go on.
func (in *Installer) confirmVM(ctx context.Context, p Platform, format string, m Machine, st *Settings, given *Given, seed string) error {
	o := &in.O
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s %s (revision %d) on %s: its %s disk, and %s\n", in.R.Spec.Name, in.R.Manifest.ProjectVersion, in.R.Manifest.Revision, p.Label, format, seed)
	fmt.Fprintf(&b, "  host %s, administrator %s (%s key)\n", m.Hostname, m.User, strings.Fields(m.SSHKey)[0])
	fmt.Fprintf(&b, "  network: %s\n", describeNet(&m.Net))
	if m.Install != "" {
		fmt.Fprintf(&b, "  the installer wipes %s, installs the OS on it and powers the machine off\n", m.Install)
	}
	for _, i := range m.Interfaces {
		how := "the network above"
		if !i.Primary {
			how = cmp.Or(i.Address, i.Mode)
		}
		fmt.Fprintf(&b, "  interface %s: %s, %s\n", i.Role, cmp.Or(i.MAC, "the machine's NIC"), how)
	}
	ssh := "every source"
	if len(m.SSHAllow) > 0 {
		ssh = fmt.Sprint(m.SSHAllow)
	}
	fmt.Fprintf(&b, "  SSH from %s; time zone %s\n", ssh, cmp.Or(m.Timezone, "UTC"))
	if len(m.Hosts) > 0 {
		fmt.Fprintf(&b, "  host records: %s\n", strings.Join(m.Hosts, ", "))
	}
	fmt.Fprintf(&b, "  served: %s; updates from the %s channel\n", cmp.Or(given.givenMode(st.TLS), "no front, its L4 routes alone"), st.Channel)
	if st.Registry != "" || st.RegistryCA != "" {
		fmt.Fprintf(&b, "  registry: %s", cmp.Or(st.Registry, in.R.Spec.Registry.Prefix))
		if st.RegistryCA != "" {
			fmt.Fprintf(&b, ", trusting the CA of %s", o.RegistryCA)
		}
		b.WriteString("\n")
	}
	for _, k := range sortedKeys(st.Inputs) {
		v := st.Inputs[k]
		if i, ok := in.R.Spec.Input(k); ok && i.File() {
			v = fileLogDigest(v)
		}
		fmt.Fprintf(&b, "  %s=%s\n", k, v)
	}
	if given != nil {
		fmt.Fprintf(&b, "  given over SSH once the VM answers, to a one-time host key its cloud-config carries (never these): %s\n", given.names())
	} else if st.AwaitSecrets {
		b.WriteString("  secrets: --secrets', given over SSH once the VM answers, to a one-time host key its cloud-config carries (never the secrets)\n")
	}
	for _, r := range in.R.Spec.L4.Routes {
		from := "every source"
		switch {
		case r.Terminated() && r.AllowInput != "":
			from += ", TLS by the front, which admits the sources of " + r.AllowInput
		case r.Terminated():
			from += ", TLS by the front"
		case r.AllowInput != "":
			from = "the sources of " + r.AllowInput
		}
		fmt.Fprintf(&b, "  opens %s/%d (L4 route %s) to %s\n", r.Protocol, r.Port, r.Name, from)
	}
	// The host devices the stack maps, which the VM must have: its platform or its host gives them.
	devices, err := lint.ServiceDevices(in.R.Compose)
	if err != nil {
		return err
	}
	for _, svc := range sortedKeys(devices) {
		fmt.Fprintf(&b, "  the service %s maps the host's %s: the VM must have them\n", svc, strings.Join(devices[svc], ", "))
	}
	o.logf("%s", strings.TrimRight(b.String(), "\n"))
	if o.Prompt == nil {
		return nil
	}
	a, err := o.Prompt.Ask(ctx, Form{
		Questions: []Question{{Title: "Write the VM's disk and cloud-config", Key: "confirm",
			Choices: []Choice{{"yes", "Yes, write them"}, {"no", "No, quit writing nothing"}}}},
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
