package install

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/norvexlabs/stowage/internal/lint"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
)

// VMSeed is `vm seed --json`: a new VM's first-boot material, for a tool that creates the VM with
// its own means. It is what `vm create` writes beside the VM an operator makes, made by the same
// steps.
type VMSeed struct {
	Format   int    `json:"format"` // MachineFormat
	Platform string `json:"platform"`
	// UserData is the cloud-config for Platform: no secret input is in it (those go after the
	// boot, `install --given -` over SSH), but the one-time host key's private key is.
	UserData string `json:"user_data"`
	// MetaData is the NoCloud meta-data for a hypervisor that builds its own seed ISO beside
	// user_data: the instance-id and local-hostname, both the VM's host name.
	MetaData string `json:"meta_data"`
	// KnownHostsLine is the one-time host key as a known_hosts line, under the name the `vm`
	// verbs' --known-hosts-line take it by; Fingerprint is its SHA256, as ssh prints it.
	KnownHostsLine string `json:"known_hosts_line"`
	Fingerprint    string `json:"fingerprint"`
	// HostKeyPrivate is the one-time host key, an OpenSSH private key: the VM serves it until its
	// install and the reboot after it; the caller keeps it secret until then.
	HostKeyPrivate string `json:"host_key_private"`
	// Disk is, per platform the registry holds a disk for, the disk image that platform boots.
	Disk map[string]SeedDisk `json:"disk"`
	// AppGiB is the app's space on the VM's disk the release recommends (else 20 GiB, at least its
	// minimum): each disk's disk_gib is it plus what Kairos takes of that disk.
	AppGiB int `json:"app_gib"`
	// DiskFile is --platform's disk written as a file (--disk-dir); nil without --disk-dir.
	DiskFile *SeedDiskFile `json:"disk_file,omitempty"`
	// AwaitSecrets: the first install waits for a decrypted secrets export, given over SSH once
	// the VM answers (`install --secrets -`), as `vm create --secrets` makes it (--await-secrets).
	// AwaitTLS: it waits for the certificate, its key and CA, and the secret inputs, given over
	// SSH (`install --given -`), its front serving them (--tls byo).
	AwaitSecrets bool `json:"await_secrets,omitempty"`
	AwaitTLS     bool `json:"await_tls,omitempty"`
	// AwaitGiven are the keys of the secret inputs the --answers file gives, sorted (never a
	// value): the first install waits for them, given over SSH once the VM answers (`install
	// --given -`, a file `project: <name>` and `inputs: {KEY: value}`), with the certificate when
	// AwaitTLS. Empty: omitted.
	AwaitGiven []string `json:"await_given,omitempty"`
	// Waiting are the keys of the required secret inputs nothing gives, sorted (never a value):
	// the VM's first install waits for them, given over SSH once the VM answers. Empty: omitted.
	Waiting []string `json:"waiting,omitempty"`
	// RegistryLogin is the host whose login --registry-login-file checked (the release's own
	// registry's or the mirror's): the VM's reads of that host take it, given over SSH once the VM
	// answers (`sudo <short>-stow registry-login -`); never the login itself. Empty: omitted.
	RegistryLogin string `json:"registry_login,omitempty"`
	// Interfaces are the NICs the VM is made with, the release's host.interfaces as --interface
	// gives them, sorted by role: what the program creating the VM gives each NIC; [] when the
	// release declares none (never omitted: a consumer tells an installer that names none).
	Interfaces []SeedInterface `json:"interfaces"`
	// Size is the VM's size the release takes (shapes.kairos.size): what a program creating the VM
	// refuses under (min) and makes by default (recommended).
	Size SeedSize `json:"size"`
	// Firewall is what a platform's own firewall in front of the VM lets through: SSH from
	// SSHSources, and each port the VM's own firewall opens, from its sources.
	Firewall SeedFirewall `json:"firewall"`
}

// SeedSize is a VM's CPUs, memory and app's space on its disk (to which each disk's disk_gib adds
// what Kairos takes), each at least the floor every VM needs (1 CPU, 1024 MiB, 4 GiB) raised by the
// release's size.min, and by default its size.recommended (else 2 CPUs, 4096 MiB, 20 GiB) raised
// to that least.
type SeedSize struct {
	CPUs      SeedRange `json:"cpus"`
	MemoryMiB SeedRange `json:"memory_mib"`
	AppGiB    SeedRange `json:"app_gib"`
}

// SeedRange is a size's least and its default.
type SeedRange struct {
	Min         int `json:"min"`
	Recommended int `json:"recommended"`
}

// SeedFirewall is the VM's reachable ports, for a platform's firewall: SSHSources are the CIDRs
// --ssh-allow gave (every source without it), OpenPorts the front's ports and each L4 route's
// (routeSources), never empty sources.
type SeedFirewall struct {
	SSHSources []string   `json:"ssh_sources"`
	OpenPorts  []SeedPort `json:"open_ports"`
}

// SeedInterface is one NIC a seed's VM is made with: its role, the MAC its cloud-config names it
// by (given, else generated on a hypervisor; empty, so omitted, for a VM of one NIC, named from
// the NIC it has and pinned to its MAC on its first boot, and on a cloud, which gives its NICs
// their MACs), the network it attaches to (empty: the primary's) and whether it is the primary.
type SeedInterface struct {
	Role    string `json:"role"`
	MAC     string `json:"mac,omitempty"`
	Attach  string `json:"attach,omitempty"`
	Primary bool   `json:"primary,omitempty"`
}

// SeedDisk is a disk image: its format, its reference by digest, what Kairos takes of a VM's disk
// of it (os_gib), the VM's disk size with the app's space (disk_gib), and on a cloud the name of
// the image `ship --disks` registers from it there (cloud_image; left out on another platform, a
// cloud the release does not publish to, or another release's disks, whose ship's cloud_publish
// this release does not tell).
type SeedDisk struct {
	Format     string `json:"format"`
	Image      string `json:"image"`
	OSGiB      int    `json:"os_gib"`
	DiskGiB    int    `json:"disk_gib"`
	CloudImage string `json:"cloud_image,omitempty"`
}

// SeedDiskFile is a disk written as a file: its path, format, the SHA-256 (hex) of its bytes
// (decompressed: what a hypervisor imports), and the VM's disk size.
type SeedDiskFile struct {
	Path    string `json:"path"`
	Format  string `json:"format"`
	SHA256  string `json:"sha256"`
	DiskGiB int    `json:"disk_gib"`
}

// SeedOptions are vm seed's own: the channel the VM follows (empty: the release's), whether its
// first install waits for a secrets export, and the directory --platform's disk is written into
// (empty: none).
type SeedOptions struct {
	Channel      string
	AwaitSecrets bool
	DiskDir      string
	// RegistryLogin is --registry-login-file's login file, checked for the VM's registry (createLogin);
	// nil: none. The seed names its host alone.
	RegistryLogin []byte
}

// seedRefusal refuses a seed given the secret inputs or TLS material names: it carries neither.
func seedRefusal(names string) error {
	return refuse("leave the secret inputs and TLS material out of the seed, and give them once the VM answers, over SSH (install --given -)",
		"%s: a seed carries no secret input and no TLS material; they go after the boot, over SSH", names)
}

// checkChannel refuses a channel ch (empty: the release's) other than the release's own that the
// registry at prefix holds no release of this project on: the VM's updates would find none. A
// release on ch that this installer cannot read (another Stowage's) is one.
func (in *Installer) checkChannel(ctx context.Context, cl *registry.Client, prefix, ch string) error {
	name, own := in.R.Spec.Name, in.R.Manifest.Channel
	if ch == "" || ch == own {
		return nil
	}
	ptr, _, err := readRelease(ctx, cl, prefix+"/"+name+"-release", ch)
	switch {
	case errors.As(err, new(foreignManifest)):
		return nil
	case err != nil:
		return fmt.Errorf("--channel %s: %w", ch, err)
	case ptr == nil || ptr.Name != name || ptr.Channel != ch:
		// An OS bundle's installer is on no channel: the VM's settings name the one it follows.
		hint, on := "give --channel a channel the publisher ships "+name+" on, or leave it out", ""
		if own != "" {
			hint, on = hint+" to follow "+own, " (this release is on "+own+")"
		}
		return refuse(hint, "--channel %s: the registry %s holds no release of %s on %s%s", ch, prefix, name, ch, on)
	}
	return nil
}

// checkInstallChannel refuses a first install's --channel ch (empty: none given) that the registry
// the install reads (the settings', else the project's) holds no release of the project on: it is
// the channel `updates` follows, checked as `vm seed --channel` is, whatever images the OS carries.
// An installed VM's re-run records no channel, and is not checked.
func (in *Installer) checkInstallChannel(ctx context.Context, ch string) error {
	if ch == "" || fileExists(in.path(envName)) {
		return nil
	}
	prefix := strings.TrimSuffix(cmp.Or(in.O.Registry, in.R.Spec.Registry.Prefix), "/")
	cl, err := in.registryClient(prefix)
	if err != nil {
		return err
	}
	return in.checkChannel(ctx, cl, prefix, ch)
}

// SeedVM is the first-boot material of a new VM of this release on c's platform, as so asks. It
// never asks (the run's prompter is not read), and refuses TLS material and a secret input on the
// command line: a seed carries neither. Its first install waits over SSH, as vm create's does
// (awaitOverSSH), for the secret inputs --answers holds, the certificate with --tls byo, a secrets
// export with --await-secrets.
func (in *Installer) SeedVM(ctx context.Context, c *VMCreate, so SeedOptions) (*VMSeed, error) {
	in.O.Prompt, in.seeding = nil, true
	channel := so.Channel
	if channel != "" && !lint.ChannelName(channel) {
		return nil, refuse("give --channel a channel name (lower-case letters, digits and -)", "--channel %q is not a channel name", channel)
	}
	// A certificate given to a seed is refused before anything reads it: the seed carries none.
	if o := &in.O; o.Cert != "" || o.Key != "" || (o.CA != "" && o.CA != noCA) || (o.ClientCA != "" && o.ClientCA != noCA) {
		return nil, seedRefusal("the certificate, its key, its CA or a client CA")
	}
	byo := in.O.TLS == "byo"
	if so.DiskDir != "" {
		if st, err := os.Stat(so.DiskDir); err != nil || !st.IsDir() {
			return nil, refuse("give --disk-dir a directory that exists", "--disk-dir %s is not a directory", so.DiskDir)
		}
		abs, err := filepath.Abs(so.DiskDir)
		if err != nil {
			return nil, err
		}
		so.DiskDir = abs // disk_file's path, read from any directory
	}
	if err := in.seedable(); err != nil {
		return nil, err
	}
	// A --platform missing, unknown or whose disk the registry lacks is refused once the registry
	// is read (platform), naming the platforms it offers: a fix that holds.
	prefix, cl, err := in.seedRegistry(ctx, &c.Platform)
	if err != nil {
		return nil, err
	}
	if err := in.checkChannel(ctx, cl, prefix, channel); err != nil {
		return nil, err
	}
	login, err := in.createLogin(so.RegistryLogin, prefix)
	if err != nil {
		return nil, err
	}
	p, _, err := in.platform(ctx, c.Platform)
	if err != nil {
		return nil, err
	}
	secrets := ""
	if so.AwaitSecrets {
		secrets = "--await-secrets"
	}
	// The first install waits for what goes over SSH, pinned to the seed's one-time host key, as
	// vm create's does: the seed carries none of it.
	mach, st, waiting, given, ca, err := in.seedSettings(ctx, c, p, prefix, secrets)
	if err != nil {
		return nil, err
	}
	if channel != "" {
		st.Channel = channel
	}
	userData, key, err := in.seedUserData(p, &mach, st, ca, true)
	if err != nil {
		return nil, err
	}
	app := in.vmSize().def.gib
	s := &VMSeed{Format: MachineFormat, Platform: p.Key, UserData: string(userData), MetaData: string(MetaData(mach)), KnownHostsLine: seedHostAlias + " " + key.Public,
		Fingerprint: key.Fingerprint, HostKeyPrivate: string(key.Private), Disk: map[string]SeedDisk{}, AppGiB: app, Waiting: slices.Sorted(slices.Values(waiting)),
		AwaitSecrets: so.AwaitSecrets, AwaitTLS: byo}
	if given != nil {
		s.AwaitGiven = slices.Sorted(maps.Keys(given.Inputs))
	}
	if login != nil {
		s.RegistryLogin = login.Host
	}
	z := in.vmSize()
	s.Size = SeedSize{CPUs: SeedRange{z.least.cpus, z.def.cpus}, MemoryMiB: SeedRange{z.least.mib, z.def.mib}, AppGiB: SeedRange{z.least.gib, z.def.gib}}
	s.Firewall = SeedFirewall{SSHSources: sshSources(mach), OpenPorts: in.openedPorts(st)}
	s.Interfaces = []SeedInterface{}
	for _, i := range mach.Interfaces {
		s.Interfaces = append(s.Interfaces, SeedInterface{Role: i.Role, MAC: i.MAC, Attach: i.Attach, Primary: i.Primary})
	}
	var platformRef name.Digest // --platform's disk, which --disk-dir writes
	for _, pl := range Platforms {
		i := slices.IndexFunc(pl.Formats, func(f string) bool { _, ok := in.boot.osMiB[f]; return ok })
		if i < 0 {
			continue
		}
		format := pl.Formats[i]
		ref, err := in.seedDisk(cl, prefix, format)
		if err != nil {
			return nil, err
		}
		if pl.Key == p.Key {
			platformRef = ref
		}
		d := SeedDisk{Format: format, Image: ref.String(), OSGiB: in.boot.osGiB(format), DiskGiB: in.boot.diskGiB(format, app)}
		// The release's own disks alone: another release's (the channel's set) were published, and
		// perhaps registered, by its ship, whose cloud_publish this spec does not tell.
		if in.boot.release == in.R.Manifest.Tag && slices.Contains(in.R.Spec.Shapes.Kairos.CloudPublish, pl.Key) {
			d.CloudImage = release.CloudImageName(in.R.Spec.Name, ref.DigestStr())
		}
		s.Disk[pl.Key] = d
	}
	if so.DiskDir != "" {
		// --platform's disk is in s.Disk: platform refused one the registry holds no disk for.
		d, ref := s.Disk[p.Key], platformRef
		in.O.logf("fetching %s's %s disk (%s) into %s", in.R.Spec.Name, d.Format, registry.Short(ref.DigestStr()), so.DiskDir)
		file, h, err := cl.Download(ctx, ref, release.DiskArtifactType, release.DiskMediaType(d.Format), so.DiskDir, fmt.Sprintf("%s-%s.%s", in.R.Spec.Name, in.boot.release, d.Format), nil)
		if err != nil {
			return nil, err
		}
		s.DiskFile = &SeedDiskFile{Path: file, Format: d.Format, SHA256: h.Hex, DiskGiB: d.DiskGiB}
	}
	return s, nil
}

// SeedPort is a port the VM's firewall opens to some sources, for a platform's own firewall.
type SeedPort struct {
	Protocol string `json:"protocol"` // tcp or udp
	Port     int    `json:"port"`
	// Sources are the CIDRs the VM's own firewall lets reach the port, as the seed knows them
	// (routeSources): every source for the front's ports, and an L4 route's allowlist.
	Sources []string `json:"sources"`
}

// everySource is every IPv4 and IPv6 source.
var everySource = []string{"0.0.0.0/0", "::/0"}

// sshSources are the CIDRs SSH accepts (--ssh-allow), every source when none is given.
func sshSources(m Machine) []string {
	if len(m.SSHAllow) == 0 {
		return everySource
	}
	var src []string
	for _, a := range m.SSHAllow {
		src = append(src, a.String())
	}
	return src
}

// openedPorts are the ports the VM's firewall opens besides SSH, each with its sources: the
// front's, and each L4 route's but one the front holds to the loopback address (loopback mode).
func (in *Installer) openedPorts(st *Settings) []SeedPort {
	ports := []SeedPort{}
	// The front's that something serves on in the mode given (frontServed); with none given, the
	// mode the VM's install takes is not known here: both.
	http, https := in.R.Spec.HasFront(), in.R.Spec.HasFront()
	if st.TLS != "" {
		http, https = frontServed(in.R.Spec.Front, in.R.Spec.L4.Routes, st.TLS)
	}
	if http {
		ports = append(ports, SeedPort{"tcp", cmp.Or(st.HTTPPort, in.R.Spec.Front.HTTPPort, 80), everySource})
	}
	if https {
		ports = append(ports, SeedPort{"tcp", cmp.Or(st.HTTPSPort, in.R.Spec.Front.HTTPSPort, 443), everySource})
	}
	for _, r := range in.R.Spec.L4.Routes {
		// In loopback mode the front holds a route it terminates TLS on to the loopback address.
		if r.Terminated() && st.TLS == Loopback {
			continue
		}
		src, _ := in.routeSources(st, r)
		ports = append(ports, SeedPort{r.Protocol, r.Port, src})
	}
	return ports
}

// routeSources are the sources the VM's firewall lets reach an L4 route's port, as the seed
// knows them, and whether they are its allowlist's: every source for a route without an
// allowlist or one the front terminates TLS on (the front filters its own port), and for an
// allowlist whose value is not known here (a secret input, given over SSH; one the spec does not
// declare; one the install refuses); else its input's value in the settings, or its default, as
// the VM's firewall reads it (familySources: each a network's prefix, an IPv4-mapped address as
// IPv4, one another holds left out), sorted by family — none when the list is empty, which the
// VM's firewall refuses too. A platform's rules made from them are fixed when they are applied:
// a later change of the list on the VM does not reach them.
func (in *Installer) routeSources(st *Settings, r spec.L4Route) (sources []string, listed bool) {
	i := slices.IndexFunc(in.R.Spec.Inputs, func(i spec.Input) bool { return i.Key == r.AllowInput })
	v, given := st.Inputs[r.AllowInput]
	switch {
	case r.AllowInput == "" || r.Terminated() || i < 0:
		return everySource, false
	case !given && in.R.Spec.Inputs[i].Secret:
		return everySource, false
	case !given:
		v = in.R.Spec.Inputs[i].Default
	}
	// A list the install refuses is no list a platform takes: every source, left to it.
	if checkSources(v) != nil {
		return everySource, false
	}
	v4, v6 := familySources(v)
	slices.Sort(v4)
	slices.Sort(v6)
	return append(append([]string{}, v4...), v6...), true
}
