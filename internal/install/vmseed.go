package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/norvexlabs/stowage/internal/lint"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
)

// VMSeed is `vm seed --json`: a new VM's first-boot material, for a tool that creates the VM with
// its own means. It is what `vm create --create manual` writes, made by the same steps.
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
	// Waiting are the keys of the required secret inputs nothing gives, sorted (never a value):
	// the VM's first install waits for them, given over SSH once the VM answers. Empty: omitted.
	Waiting []string `json:"waiting,omitempty"`
	// RegistryLogin is the host of the mirror whose login --registry-login-file checked: the VM's
	// reads of the mirror take it, given over SSH once the VM answers (`sudo <short>-stow
	// registry-login -`); never the login itself. Empty: omitted.
	RegistryLogin string `json:"registry_login,omitempty"`
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
	// RegistryLogin is --registry-login-file's login file, checked for the mirror (createLogin);
	// nil: none. The seed names its host alone.
	RegistryLogin []byte
}

// seedRefusal refuses a seed given the secret inputs or TLS material names: it carries neither.
func seedRefusal(names string) error {
	return refuse("leave the secret inputs and TLS material out of the seed, and give them once the VM answers, over SSH (install --given -)",
		"%s: a seed carries no secret input and no TLS material; they go after the boot, over SSH", names)
}

// SeedVM is the first-boot material of a new VM of this release on c's platform, as so asks. It
// never asks (the run's prompter is not read), and refuses a secret input or TLS material: a seed
// carries neither; with --tls byo or --await-secrets its first install waits for them over SSH.
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
	if byo && so.AwaitSecrets {
		return nil, refuse("leave out --await-secrets, or --tls byo", "--tls byo and --await-secrets: the first install waits for one of them over SSH, the certificate (install --given -) or a secrets export (install --secrets -)")
	}
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
	prefix, cl, err := in.seedRegistry(ctx)
	if err != nil {
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
	mach, st, waiting, given, ca, err := in.seedSettings(ctx, c, p, prefix)
	if err != nil {
		return nil, err
	}
	if given != nil {
		return nil, seedRefusal(given.names())
	}
	if channel != "" {
		st.Channel = channel
	}
	// The first install waits for what goes over SSH, pinned to the seed's one-time host key.
	switch {
	case byo:
		st.AwaitSecrets, st.AwaitGiven = true, "byo"
	case so.AwaitSecrets:
		st.AwaitSecrets = true
	}
	userData, key, err := in.seedUserData(p, &mach, st, ca, true)
	if err != nil {
		return nil, err
	}
	app := in.vmSize().def.gib
	s := &VMSeed{Format: MachineFormat, Platform: p.Key, UserData: string(userData), MetaData: string(MetaData(mach)), KnownHostsLine: seedHostAlias + " " + key.Public,
		Fingerprint: key.Fingerprint, HostKeyPrivate: string(key.Private), Disk: map[string]SeedDisk{}, AppGiB: app, Waiting: slices.Sorted(slices.Values(waiting)),
		AwaitSecrets: so.AwaitSecrets, AwaitTLS: byo}
	if login != nil {
		s.RegistryLogin = login.Host
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
