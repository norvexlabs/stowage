package install

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // checkTimezone reads zones on a host without its own database

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	"go.yaml.in/yaml/v3"
)

// Machine is a VM's own settings, which its cloud-config gives: its name, its administrator and
// its network.
type Machine struct {
	Hostname string
	User     string
	SSHKey   string // one authorized_keys line
	// Net is its network, valid (validNet): written as networkd's file at every boot, DHCP
	// included, as IPv6 is off unless Net turns it on, and as data (InstallNetworkFile) for the
	// host daemon to answer.
	Net NetConfig
	// SSHAllow are the sources SSH accepts; none is every source. publicSSH: they are this
	// machine's public address alone, as a server on the internet saw it (a cloud's default).
	SSHAllow  []netip.Prefix
	publicSSH bool
	// Timezone is an IANA zone name; empty keeps the OS's, UTC.
	Timezone string
	// HostKey is a one-time SSH host key (an OpenSSH private key) the VM serves until its first
	// install, which `vm create --secrets` pins to send the secrets; nil for none.
	HostKey []byte
}

// staticAddr is the VM's static IPv4 address, without its prefix; empty with DHCP.
func (m Machine) staticAddr() string {
	if m.Net.Mode != "static" {
		return ""
	}
	p, _ := netip.ParsePrefix(m.Net.Address)
	return p.Addr().String()
}

// staticAddr6 is the VM's static IPv6 address, without its prefix; empty without one.
func (m Machine) staticAddr6() string {
	if m.Net.Address6 == "" {
		return ""
	}
	p, _ := netip.ParsePrefix(m.Net.Address6)
	return p.Addr().String()
}

// connectIssue is the console's connect line: \4 is the VM's IPv4 address, \6 its IPv6 one, shown
// when IPv6 is on.
func connectIssue(m Machine, ctl string) string {
	s := fmt.Sprintf("connect: ssh %s@\\4, then `%s status`\n", m.User, ctl)
	if m.Net.IPv6 {
		s += fmt.Sprintf("         or ssh %s@\\6\n", m.User)
	}
	return s + "\n"
}

// The cloud-config Kairos reads (yip's stages): written at every boot, as /etc is not kept.
type cloudConfig struct {
	Stages map[string][]stage `yaml:"stages"`
}

type stage struct {
	Name     string               `yaml:"name"`
	If       string               `yaml:"if,omitempty"` // a shell test: the step runs when it holds
	Hostname string               `yaml:"hostname,omitempty"`
	Users    map[string]cloudUser `yaml:"users,omitempty"`
	Files    []cloudFile          `yaml:"files,omitempty"`
	Commands []string             `yaml:"commands,omitempty"`
}

type cloudUser struct {
	Groups            []string `yaml:"groups"`
	Homedir           string   `yaml:"homedir"`
	Shell             string   `yaml:"shell"`
	Passwd            string   `yaml:"passwd"`
	SSHAuthorizedKeys []string `yaml:"ssh_authorized_keys"`
}

type cloudFile struct {
	Path        string `yaml:"path"`
	Permissions uint32 `yaml:"permissions"`
	Content     string `yaml:"content"`
}

// UserData is the VM's cloud-config: its hostname (set before the network starts); its administrator, in the admin group (the host
// daemon's socket and sudo without a password), who logs in with the SSH key alone; its network,
// DHCP included, as networkd's file and as a record the host daemon answers (InstallNetworkFile),
// both at every boot; and the first install's settings (SettingsFile), which the boot
// installs from. With ca, the CA of the registry the settings name (a mirror's) goes to
// VMRegistryCA and into the VM's trust store, at every boot, before the install reads it. ctl is
// the installer its comments and console banner name. It carries no secret, but for
// m.HostKey: written, while the install does not exist and the key was never retired, to a file under /run (gone at the next
// boot) that a drop-in of sshd names first, then the system's own keys (seedHostKeyFiles).
func UserData(name, ctl string, m Machine, st Settings, ca []byte) ([]byte, error) {
	settings, err := yaml.Marshal(st)
	if err != nil {
		return nil, err
	}
	boot := []stage{{
		Name: name + ": the administrator and the first install",
		Users: map[string]cloudUser{m.User: {
			Groups:            []string{AdminGroup},
			Homedir:           "/home/" + m.User,
			Shell:             "/bin/bash",
			Passwd:            "!", // no password: the SSH key alone logs in
			SSHAuthorizedKeys: []string{m.SSHKey},
		}},
		Files: []cloudFile{
			{Path: SettingsFile(name), Permissions: 0o600, Content: string(settings)},
			// The console's login screen tells how to reach the VM; \4 is its address.
			{Path: "/etc/issue.d/07-" + name + "-connect.issue", Permissions: 0o644, Content: connectIssue(m, ctl)},
		},
	}, {
		// A step of its own, after the one that creates the user (yip runs a step's commands
		// before its users): yip only sets the key and password of a user the base already has
		// (ubuntu's own), so the group is given here, either way.
		Name:     name + ": the administrator in the " + AdminGroup + " group",
		Commands: []string{"usermod -a -G " + AdminGroup + " " + m.User},
	}}
	if ca != nil {
		boot = append(boot, stage{
			Name:  name + ": the registry's CA",
			Files: []cloudFile{{Path: VMRegistryCA(name), Permissions: 0o644, Content: string(ca)}},
		}, stage{
			// A step of its own, after the one that writes the file.
			Name:     name + ": the registry's CA in the trust store",
			Commands: []string{"update-ca-certificates"},
		})
	}
	if len(m.SSHAllow) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "# SSH, from the sources `%s vm create --ssh-allow` gave.\n", ctl)
		for _, p := range m.SSHAllow {
			family := "ip"
			if p.Addr().Is6() {
				family = "ip6"
			}
			fmt.Fprintf(&b, "%s saddr %s tcp dport 22 accept\n", family, p.Masked())
		}
		boot = append(boot, stage{
			Name:  name + ": SSH's sources",
			Files: []cloudFile{{Path: SSHRules, Permissions: 0o644, Content: b.String()}},
		}, stage{
			// A step of its own, after the one that writes the file. The ruleset flushes its own
			// table only: Docker's rules stay (a restart of nftables.service would flush them all).
			Name:     name + ": the firewall with SSH's sources",
			Commands: []string{"nft -f /etc/nftables.conf"},
		})
	}
	if m.Timezone != "" {
		boot = append(boot, stage{
			Name: name + ": the time zone",
			// The zone is checked against this host's database: one the VM's lacks stays UTC, said.
			Commands: []string{fmt.Sprintf("if [ -f /usr/share/zoneinfo/%[1]s ]; then ln -sf /usr/share/zoneinfo/%[1]s /etc/localtime && echo %[1]s > /etc/timezone; else echo 'no time zone %[1]s on this OS: it stays UTC' >&2; fi", m.Timezone)},
		})
	}
	netYAML, err := yaml.Marshal(m.Net)
	if err != nil {
		return nil, err
	}
	boot = append(boot, stage{
		Name: name + ": the network",
		Files: []cloudFile{
			// 05- sorts before Kairos's 20-dhcp.network, and after the override `network set`
			// makes: networkd takes the first file that matches.
			{Path: "/etc/systemd/network/" + staticUnit(name), Permissions: 0o644, Content: "[Match]\nName=en* eth*\n\n" + networkSections(m.Net)},
			// The same network as data, for the host daemon to answer what the install set.
			{Path: InstallNetworkFile(name), Permissions: 0o644, Content: string(netYAML)},
		},
		Commands: []string{"networkctl reload"},
	})
	if m.HostKey != nil {
		key, dropIn, retired := seedHostKeyFiles(name)
		before := "[ ! -e " + VMDir(name) + "/" + envName + " ] && [ ! -e " + retired + " ]"
		boot = append(boot, stage{
			Name: name + ": the one-time SSH host key, until the install",
			If:   before,
			Files: []cloudFile{
				{Path: key, Permissions: 0o600, Content: string(m.HostKey)},
				{Path: dropIn, Permissions: 0o644, Content: "# The one-time host key `" + ctl + " vm create` pins, served until the install.\nHostKey " + key + "\nHostKey /etc/ssh/ssh_host_ed25519_key\nHostKey /etc/ssh/ssh_host_ecdsa_key\nHostKey /etc/ssh/ssh_host_rsa_key\n"},
			},
		}, stage{
			// A step of its own, after the one that writes the files; an sshd not started yet reads them.
			Name:     name + ": sshd with the one-time host key",
			If:       before,
			Commands: []string{sshdReload},
		}, stage{
			Name:     name + ": the one-time SSH host key retired after the install",
			If:       "[ -e " + VMDir(name) + "/" + envName + " ] && [ ! -e " + retired + " ]",
			Commands: []string{"rm -f " + dropIn + " " + key + " && touch " + retired + " && " + sshdReload},
		})
	}
	// The host name is set in the initramfs stage, before networkd starts: its DHCP request then
	// carries it, and a DHCP server registering names registers this one.
	initramfs := []stage{{Name: name + ": the host name", Hostname: m.Hostname}}
	out, err := yaml.Marshal(cloudConfig{Stages: map[string][]stage{"initramfs": initramfs, "boot": boot}})
	if err != nil {
		return nil, err
	}
	carries := "It carries no secret."
	if m.HostKey != nil {
		carries = "It carries no secret but a one-time SSH host key, served until the install."
	}
	head := fmt.Sprintf("#cloud-config\n# %s's VM %s, written by `%s vm create`. %s\n", name, m.Hostname, ctl, carries)
	return append([]byte(head), out...), nil
}

// seedHostKeyFiles are, on the VM, the one-time host key's file, the sshd drop-in naming it, and
// the mark of its retirement on the OEM partition, which a reset keeps and every system mounts:
// no boot serves the key past it, not one whose install is gone, nor the recovery system's.
func seedHostKeyFiles(name string) (key, dropIn, retired string) {
	return "/run/" + name + "-seed-host-key", "/etc/ssh/sshd_config.d/01-" + name + "-seed-host-key.conf", "/oem/" + name + "-seed-host-key.retired"
}

// sshdReload makes a running sshd read its configuration again (Debian's unit is ssh.service,
// others' sshd.service); one not running reads it when it starts.
const sshdReload = "for u in ssh.service sshd.service; do systemctl try-reload-or-restart $u 2>/dev/null; done; true"

// SSHRules is the firewall's SSH rule file on the VM: the bundle's opens SSH to every source, and
// a cloud-config with Machine.SSHAllow replaces it.
const SSHRules = "/etc/nftables.d/ssh.nft"

// zoneName is an IANA time zone name, as /usr/share/zoneinfo names its files.
var zoneName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+)*$`)

// checkTimezone takes an IANA time zone name the Go time zone database knows.
func checkTimezone(v string) error {
	// Local is Go's own name for the host's zone; the others are the database's files of no zone,
	// and its leap-second and duplicate trees.
	if !zoneName.MatchString(v) || len(v) > 64 || slices.Contains([]string{"Local", "localtime", "posixrules", "Factory"}, v) ||
		strings.HasPrefix(v, "right/") || strings.HasPrefix(v, "posix/") {
		return fmt.Errorf("%q is not a time zone name (Europe/Paris, UTC)", v)
	}
	if _, err := time.LoadLocation(v); err != nil {
		return fmt.Errorf("%q is not a time zone: %w", v, err)
	}
	return nil
}

// MetaData is the NoCloud meta-data of the VM: its instance, named after its host.
func MetaData(m Machine) []byte {
	return fmt.Appendf(nil, "instance-id: %s\nlocal-hostname: %s\n", m.Hostname, m.Hostname)
}

// SeedVolume is the label a NoCloud seed's volume carries, which Kairos's cdrom datasource looks
// for.
const SeedVolume = "cidata"

// WriteSeedISO writes a NoCloud seed at path, of mode (0644: a hypervisor running as its own user
// reads it, when it carries no secret): an ISO 9660 image (Rock Ridge and Joliet names)
// labelled SeedVolume, holding user-data and meta-data, which a hypervisor attaches as a CD-ROM.
// The file is the sectors the volume declares, then seedPad sectors of zeros: VMware Workstation's CD-ROM reads no image that ends short of a whole sector.
func WriteSeedISO(path string, userData, metaData []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	work, err := os.MkdirTemp(dir, ".seed-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	tmp := filepath.Join(work, "seed.iso")
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil { // whatever the umask
		f.Close()
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp, path)
		}
	}()
	staging := filepath.Join(work, "files")
	if err := os.Mkdir(staging, 0o700); err != nil {
		return err
	}
	fs, err := iso9660.Create(file.New(f, false), 0, 0, 2048, staging)
	if err != nil {
		return err
	}
	for _, e := range []struct {
		name string
		data []byte
	}{{"user-data", userData}, {"meta-data", metaData}} {
		w, err := fs.OpenFile("/"+e.name, os.O_CREATE|os.O_RDWR)
		if err != nil {
			return err
		}
		if _, err := w.Write(e.data); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
	}
	// Padded with spaces, as ISO 9660 pads it, which blkid trims: the label reads SeedVolume.
	if err := fs.Finalize(iso9660.FinalizeOptions{RockRidge: true, Joliet: true, VolumeIdentifier: fmt.Sprintf("%-32s", SeedVolume)}); err != nil {
		return err
	}
	// go-diskfs may end the file inside the last sector its volume declares (VMware Workstation's
	// CD-ROM then cannot read the image at all: the guest cannot read its first sector), or write
	// a sector of zeros past it. The file is cut to the declared volume, then seedPad sectors of
	// zeros, as Linux reads ahead past a CD's last sector.
	st, err := f.Stat()
	if err != nil {
		return err
	}
	pvd := make([]byte, 4)
	if _, err := f.ReadAt(pvd, seedVolumeSize); err != nil {
		return fmt.Errorf("reading the seed's volume size: %w", err)
	}
	vol := int64(binary.LittleEndian.Uint32(pvd)) * seedSector
	if vol <= seedVolumeSize || vol < st.Size()-seedSector || vol > st.Size()+seedSector {
		return fmt.Errorf("the seed's volume declares %d bytes, its file holds %d", vol, st.Size())
	}
	return f.Truncate(vol + seedPad*seedSector)
}

// seedSector is a CD's sector, seedPad the sectors of zeros after the seed's volume, and
// seedVolumeSize the offset of the volume's size in sectors (little-endian) in its primary
// volume descriptor (sector 16).
const (
	seedSector     = 2048
	seedPad        = 150
	seedVolumeSize = 16*seedSector + 80
)
