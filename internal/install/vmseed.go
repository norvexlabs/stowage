package install

import (
	"context"
	"fmt"
	"slices"

	"github.com/norvexlabs/stowage/internal/lint"
)

// VMSeed is `vm seed --json`: a new VM's first-boot material, for a tool that creates the VM with
// its own means. It is what `vm create --create manual` writes, made by the same steps.
type VMSeed struct {
	Format   int    `json:"format"` // MachineFormat
	Platform string `json:"platform"`
	// UserData is the cloud-config for Platform: no secret input is in it (those go after the
	// boot, `install --given -` over SSH), but the one-time host key's private key is.
	UserData string `json:"user_data"`
	// KnownHostsLine is the one-time host key as a known_hosts line, under the name the `vm`
	// verbs' --known-hosts-line take it by; Fingerprint is its SHA256, as ssh prints it.
	KnownHostsLine string `json:"known_hosts_line"`
	Fingerprint    string `json:"fingerprint"`
	// HostKeyPrivate is the one-time host key, an OpenSSH private key: the VM serves it until its
	// install and the reboot after it; the caller keeps it secret until then.
	HostKeyPrivate string `json:"host_key_private"`
	// Disk is, per platform the registry holds a disk for, the disk image that platform boots.
	Disk map[string]SeedDisk `json:"disk"`
	// Waiting are the keys of the required secret inputs nothing gives, sorted (never a value):
	// the VM's first install waits for them, given over SSH once the VM answers. Empty: omitted.
	Waiting []string `json:"waiting,omitempty"`
}

// SeedDisk is a disk image: its format, and its reference by digest.
type SeedDisk struct {
	Format string `json:"format"`
	Image  string `json:"image"`
}

// seedSecretFix is the fix of a seed given a secret input or TLS material.
const seedSecretFix = "leave the secret inputs and TLS material out of the seed, and give them once the VM answers, over SSH (install --given -)"

// SeedVM is the first-boot material of a new VM of this release on c's platform, the
// first install following channel (empty: the release's). It never asks (the run's prompter is
// not read), and refuses a secret input or TLS material: a seed carries neither.
func (in *Installer) SeedVM(ctx context.Context, c *VMCreate, channel string) (*VMSeed, error) {
	in.O.Prompt = nil
	if channel != "" && !lint.ChannelName(channel) {
		return nil, fmt.Errorf("--channel %q is not a channel name", channel)
	}
	if err := in.seedable(); err != nil {
		return nil, err
	}
	// A secret input on the command line: the seed's own refusal, before the registry is read
	// (vm create's, run by seedSettings, would advise the answers file, which a seed refuses too).
	for _, i := range in.R.Spec.Inputs {
		if _, set := in.O.Set[i.Key]; set && i.Secret {
			return nil, refuse(seedSecretFix, "%s: a seed carries no secret input and no TLS material; they go after the boot, over SSH", i.Key)
		}
	}
	// A --platform missing, unknown or whose disk the registry lacks is refused once the registry
	// is read (platform), naming the platforms it offers: a fix that holds.
	prefix, cl, err := in.seedRegistry(ctx)
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
		return nil, refuse(seedSecretFix,
			"%s: a seed carries no secret input and no TLS material; they go after the boot, over SSH", given.names())
	}
	if channel != "" {
		st.Channel = channel
	}
	userData, key, err := in.seedUserData(p, &mach, st, ca, true)
	if err != nil {
		return nil, err
	}
	s := &VMSeed{Format: MachineFormat, Platform: p.Key, UserData: string(userData), KnownHostsLine: seedHostAlias + " " + key.Public,
		Fingerprint: key.Fingerprint, HostKeyPrivate: string(key.Private), Disk: map[string]SeedDisk{}, Waiting: slices.Sorted(slices.Values(waiting))}
	for _, pl := range Platforms {
		i := slices.IndexFunc(pl.Formats, func(f string) bool { _, ok := in.boot.osMiB[f]; return ok })
		if i < 0 {
			continue
		}
		ref, err := in.seedDisk(cl, prefix, pl.Formats[i])
		if err != nil {
			return nil, err
		}
		s.Disk[pl.Key] = SeedDisk{Format: pl.Formats[i], Image: ref.String()}
	}
	return s, nil
}
