package install

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
)

// sshKey writes an ed25519 public key file and returns its path and line.
func sshKey(t *testing.T) (string, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	str := func(b []byte) []byte { return append(binary.BigEndian.AppendUint32(nil, uint32(len(b))), b...) }
	blob := append(str([]byte("ssh-ed25519")), str(pub)...)
	line := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " ops@laptop"
	path := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, line
}

// vmCreate is a published release of examples/minimal whose disks of formats are in the registry,
// as `vm create` reads them; its run writes into out.
type vmCreate struct {
	*pubEnv
	out, key, keyLine string
	disks             map[string][]byte
}

func newVMCreate(t *testing.T, formats ...string) *vmCreate {
	t.Helper()
	return newVMCreatePulling(t, spec.Provider{Name: "none"}, formats...)
}

// newVMCreatePulling is newVMCreate with the pull account pull, none baked in.
func newVMCreatePulling(t *testing.T, pull spec.Provider, formats ...string) *vmCreate {
	t.Helper()
	p := published(t, pull, nil)
	v := &vmCreate{pubEnv: p, out: filepath.Join(t.TempDir(), "vm"), disks: map[string][]byte{}}
	v.key, v.keyLine = sshKey(t)
	c := registry.New(nil)
	repo, err := c.Repo(p.prefix + "/notes-disk")
	if err != nil {
		t.Fatal(err)
	}
	disks := map[string]string{}
	for _, f := range formats {
		data := []byte("a " + f + " disk")
		b := registry.Blob{MediaType: release.DiskMediaType(f), Data: data, Annotations: map[string]string{registry.TitleAnnotation: "notes-1.0.0." + f}}
		h, err := c.PutManifest(context.Background(), repo, registry.Manifest{ArtifactType: release.DiskArtifactType,
			Config: registry.Blob{MediaType: registry.EmptyMediaType, Data: []byte("{}")}, Layers: []registry.Blob{b},
			Annotations: map[string]string{release.DiskOSAnnotation: "23000"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		disks[f], v.disks[f] = h.String(), data
	}
	v.r.Manifest.Artifacts.Disks = disks
	v.r.Manifest.Artifacts.OS = "sha256:" + strings.Repeat("0", 64)
	return v
}

// create runs `vm create` with c (its key and out filled in when empty) and mod's options; with
// no prompter, the domains and the input the example requires are given unless mod gave them.
func (v *vmCreate) create(c VMCreate, mod func(*Options)) error {
	c.Out = cmpOr(c.Out, v.out)
	c.SSHKey = cmpOr(c.SSHKey, v.key)
	in := v.installer(func(o *Options) {
		// This machine's public address, which a cloud's VM takes SSH from: never asked of the network.
		o.PublicAddr = func(context.Context) (netip.Addr, error) { return netip.MustParseAddr("198.51.100.40"), nil }
		if mod != nil {
			mod(o)
		}
		if o.Prompt != nil {
			return
		}
		if o.Set == nil {
			o.Set = map[string]string{}
		}
		for k, val := range map[string]string{"ADMIN_EMAIL": "admin@example.org", "APP_DOMAIN": "app.example.org", "API_DOMAIN": "api.example.org"} {
			if _, ok := o.Set[k]; !ok {
				o.Set[k] = val
			}
		}
	})
	return in.CreateVM(context.Background(), &c)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// readSeed reads the user-data and meta-data of the seed ISO at path, and its volume label.
func readSeed(t *testing.T, path string) (userData, metaData, label string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) < seedVolumeSize+4 {
		t.Fatalf("seed ISO of %d bytes: %v", len(raw), err)
	}
	// The declared volume whole (whole sectors), then 150 sectors of zeros, or VMware's CD-ROM
	// cannot read it.
	size, vol := int64(len(raw)), int64(binary.LittleEndian.Uint32(raw[seedVolumeSize:]))*2048
	if vol%2048 != 0 || size != vol+150*2048 || strings.Trim(string(raw[vol:]), "\x00") != "" {
		t.Errorf("seed ISO of %d bytes, a volume of %d: want the volume then 150 sectors of zeros", size, vol)
	}
	fs, err := iso9660.Read(file.New(f, true), size, 0, seedSector)
	if err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		r, err := fs.OpenFile("/"+name, os.O_RDONLY)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, _ := io.ReadAll(r)
		return string(b)
	}
	return read("user-data"), read("meta-data"), fs.Label()
}

// cloudSettings decodes the settings file a user-data writes.
func cloudSettings(t *testing.T, userData string) (Settings, cloudConfig) {
	t.Helper()
	var cc cloudConfig
	if err := yaml.Unmarshal([]byte(userData), &cc); err != nil {
		t.Fatal(err)
	}
	var st Settings
	for _, s := range cc.Stages["boot"] {
		for _, f := range s.Files {
			if f.Path == "/etc/notes/install.yaml" {
				dec := yaml.NewDecoder(strings.NewReader(f.Content))
				dec.KnownFields(true)
				if err := dec.Decode(&st); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return st, cc
}

func TestVMCreateWritesTheDiskTheSeedAndTheSteps(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2", "raw")
	err := v.create(VMCreate{Platform: "truenas", Hostname: "notes-1", User: "ops"}, func(o *Options) {
		o.TLS, o.Set = "internal", map[string]string{"APP_DOMAIN": "app.example.org", "API_DOMAIN": "api.example.org"}
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	disk, _ := os.ReadFile(filepath.Join(v.out, "notes-1.0.0.qcow2"))
	if !bytes.Equal(disk, v.disks["qcow2"]) {
		t.Errorf("disk %q", disk)
	}
	if fi, err := os.Stat(filepath.Join(v.out, "notes-1-seed.iso")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("seed: %v %v", fi, err)
	}
	ud, md, label := readSeed(t, filepath.Join(v.out, "notes-1-seed.iso"))
	if label != "cidata"+strings.Repeat(" ", 26) || md != "instance-id: notes-1\nlocal-hostname: notes-1\n" {
		t.Errorf("label %q, meta-data %q", label, md)
	}
	if !strings.HasPrefix(ud, "#cloud-config\n") {
		t.Errorf("user-data:\n%s", ud)
	}
	st, cc := cloudSettings(t, ud)
	want := Settings{Channel: "stable", TLS: "internal", Inputs: map[string]string{"ADMIN_EMAIL": "admin@example.org", "APP_DOMAIN": "app.example.org", "API_DOMAIN": "api.example.org"}}
	if st.Channel != want.Channel || st.TLS != want.TLS || !mapsEqual(st.Inputs, want.Inputs) {
		t.Errorf("settings %+v, want %+v", st, want)
	}
	boot := cc.Stages["boot"]
	u := boot[0].Users["ops"]
	if ini := cc.Stages["initramfs"]; len(ini) != 1 || ini[0].Hostname != "notes-1" || boot[0].Hostname != "" {
		t.Errorf("the host name is not set in the initramfs stage alone: %+v", cc.Stages)
	}
	if !slices.Equal(u.Groups, []string{"admin"}) || u.Passwd != "!" || !slices.Equal(u.SSHAuthorizedKeys, []string{v.keyLine}) {
		t.Errorf("boot stage %+v", boot[0])
	}
	// IPv6 off: the console's connect line names the IPv4 address alone.
	if c := boot[0].Files[1]; c.Path != "/etc/issue.d/07-notes-connect.issue" || c.Content != "connect: ssh ops@\\4, then `notes-stow status`\n\n" {
		t.Errorf("connect line %+v", c)
	}
	// DHCP, IPv6 off: the network file says so, as networkd takes router advertisements by default.
	if len(boot) != 3 || boot[2].Files[0].Content != "[Match]\nName=en* eth*\n\n[Network]\nDHCP=ipv4\nLinkLocalAddressing=no\nIPv6AcceptRA=no\n" {
		t.Errorf("DHCP's network: %+v", boot[2:])
	}
	// After the step that creates the user, in a step of its own.
	if len(boot[0].Commands) > 0 || !slices.Equal(boot[1].Commands, []string{"usermod -a -G admin ops"}) {
		t.Errorf("the administrator is not put in the admin group after the user is made: %+v", boot)
	}
	out := v.out
	for _, s := range []string{"On TrueNAS:", "1. Copy " + filepath.Join(out, "notes-1.0.0.qcow2"), filepath.Join(out, "notes-1-seed.iso") + " as its CD-ROM", "ssh ops@<address> notes-stow status"} {
		if !strings.Contains(v.out2(), s) {
			t.Errorf("no %q in:\n%s", s, v.out2())
		}
	}
}

// Bare metal: the installer ISO and a seed whose install block names the disk, which the machine
// is powered off after; the installer's copy of the cloud-config kept as the first boot's, and
// every other stage skipped by the installer's live system. The disk must be named, and only
// there.
func TestVMCreateOnMetalWritesTheInstallerAndItsDisk(t *testing.T) {
	t.Parallel()
	set := func(o *Options) {
		o.TLS, o.Set = "internal", map[string]string{"APP_DOMAIN": "app.example.org", "API_DOMAIN": "api.example.org"}
	}
	v := newVMCreate(t, "iso")
	if err := v.create(VMCreate{Platform: "metal", Hostname: "notes-1", User: "ops", Device: "/dev/disk/by-id/nvme-Samsung_SSD_990_S7"}, set); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	if disk, _ := os.ReadFile(filepath.Join(v.out, "notes-1.0.0.iso")); !bytes.Equal(disk, v.disks["iso"]) {
		t.Errorf("installer %q", disk)
	}
	ud, _, _ := readSeed(t, filepath.Join(v.out, "notes-1-seed.iso"))
	_, cc := cloudSettings(t, ud)
	if cc.Install == nil || *cc.Install != (cloudInstall{Auto: true, Device: "/dev/disk/by-id/nvme-Samsung_SSD_990_S7", Poweroff: true}) {
		t.Errorf("install block %+v", cc.Install)
	}
	if ai := cc.Stages["after-install-chroot"]; len(ai) != 1 || ai[0].If != "" || !slices.Equal(ai[0].Commands, []string{afterInstallMove}) {
		t.Errorf("after-install %+v", ai)
	}
	for _, at := range []string{"initramfs", "boot"} {
		for _, st := range cc.Stages[at] {
			if !strings.HasPrefix(st.If, notLive) {
				t.Errorf("%s step %q runs in the installer: if %q", at, st.Name, st.If)
			}
		}
	}
	for _, s := range []string{"On Bare metal (amd64):", "1. Write " + filepath.Join(v.out, "notes-1.0.0.iso"), "the installer wipes the --device disk", "Take both sticks out"} {
		if !strings.Contains(v.out2(), s) {
			t.Errorf("no %q in:\n%s", s, v.out2())
		}
	}
	if !strings.Contains(v.log.String(), "the installer wipes /dev/disk/by-id/nvme-Samsung_SSD_990_S7") && !strings.Contains(v.out2(), "the installer wipes /dev/disk/by-id/nvme-Samsung_SSD_990_S7") {
		t.Errorf("the confirmation names no disk:\n%s\n%s", v.log, v.out2())
	}
	for _, c := range []struct {
		vm   VMCreate
		want string
	}{
		{VMCreate{Platform: "metal", Hostname: "notes-2", User: "ops"}, "--device is required"},
		{VMCreate{Platform: "metal", Hostname: "notes-3", User: "ops", Device: "/dev/disk/../../etc/passwd"}, "--device \"/dev/disk/../../etc/passwd\""},
		{VMCreate{Platform: "metal", Hostname: "notes-4", User: "ops", Device: "sda"}, "--device \"sda\""},
		{VMCreate{Platform: "truenas", Hostname: "notes-5", User: "ops", Device: "/dev/sda"}, "--device is for a platform whose installer"},
	} {
		if err := newVMCreate(t, "iso", "qcow2").create(c.vm, set); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.vm, err, c.want)
		}
	}
	if err := checkDevice("auto"); err != nil {
		t.Error(err)
	}
	// A disk image boots: no install block, no after-install step.
	raw, err := UserData("notes", "notes-stow", Machine{Hostname: "notes-1", User: "ops", SSHKey: v.keyLine}, Settings{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ud2 := string(raw); strings.Contains(ud2, "install:") || strings.Contains(ud2, "after-install") {
		t.Errorf("a VM's seed installs:\n%s", ud2)
	}
}

func (v *vmCreate) out2() string { return v.env.out.String() }

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, x := range a {
		if y, ok := b[k]; !ok || x != y {
			return false
		}
	}
	return true
}

func TestVMCreateKeepsADiskItFetchedAndNeverReplacesAnotherFile(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	c := VMCreate{Platform: "libvirt"}
	if err := v.create(c, nil); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	// The seed, which a VM may have attached, is never replaced.
	if err := v.create(c, nil); err == nil || !strings.Contains(err.Error(), "notes-seed.iso exists") {
		t.Fatalf("seed replaced: %v", err)
	}
	_ = os.Remove(filepath.Join(v.out, "notes-seed.iso"))
	if err := v.create(c, nil); err != nil {
		t.Fatalf("again: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(v.out, "notes-1.0.0.qcow2")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("disk: %v %v", fi, err)
	}
	blobs := 0
	for _, p := range v.paths {
		if strings.HasPrefix(p, "GET ") && strings.Contains(p, "/notes-disk/blobs/") {
			blobs++
		}
	}
	if blobs != 1 {
		t.Errorf("the disk was fetched %d times", blobs)
	}
	_ = os.Remove(filepath.Join(v.out, "notes-seed.iso"))
	_ = os.WriteFile(filepath.Join(v.out, "notes-1.0.0.qcow2"), []byte("mine"), 0o644)
	if err := v.create(c, nil); err == nil || !strings.Contains(err.Error(), "move it away") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(v.out, "notes-1.0.0.qcow2")); string(b) != "mine" {
		t.Error("the operator's file was replaced")
	}
}

func TestVMCreateOnACloudWritesTheUserDataFile(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "raw")
	if err := v.create(VMCreate{Platform: "aws", Hostname: "edge"}, nil); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	ud, err := os.ReadFile(filepath.Join(v.out, "edge-user-data.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := cloudSettings(t, string(ud)); st.TLS != "acme" {
		t.Errorf("settings %+v", st)
	}
	if fi, _ := os.Stat(filepath.Join(v.out, "edge-user-data.yaml")); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(v.out, "notes-1.0.0.raw")); err != nil {
		t.Error(err)
	}
}

func TestACloudsVMTakesSSHFromThisMachinesPublicAddress(t *testing.T) {
	t.Parallel()
	rules := func(t *testing.T, c VMCreate, mod func(*Options)) (string, error) {
		v := newVMCreate(t, "raw")
		if err := v.create(c, mod); err != nil {
			return "", err
		}
		ud, err := os.ReadFile(filepath.Join(v.out, c.Hostname+"-user-data.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		_, cc := cloudSettings(t, string(ud))
		for _, s := range cc.Stages["boot"] {
			for _, f := range s.Files {
				if f.Path == SSHRules {
					return f.Content, nil
				}
			}
		}
		return "", nil
	}
	got, err := rules(t, VMCreate{Platform: "aws", Hostname: "edge"}, nil)
	if err != nil || !strings.Contains(got, "\nip saddr 198.51.100.40/32 tcp dport 22 accept\n") || strings.Count(got, "accept") != 1 {
		t.Errorf("rules %q, %v", got, err)
	}
	unknown := func(o *Options) {
		o.PublicAddr = func(context.Context) (netip.Addr, error) { return netip.Addr{}, errors.New("no route") }
	}
	if _, err := rules(t, VMCreate{Platform: "aws", Hostname: "edge"}, unknown); err == nil || !strings.Contains(err.Error(), "--ssh-allow: a cloud's VM answers on a public address") || !strings.Contains(err.Error(), "no route") {
		t.Errorf("err = %v", err)
	}
	// The sources given are SSH's, the address never asked; every source when told so.
	if got, err := rules(t, VMCreate{Platform: "aws", Hostname: "edge", SSHAllow: []string{"0.0.0.0/0", "::/0"}}, unknown); err != nil || got != "# SSH, from the sources `notes-stow vm create --ssh-allow` gave.\nip saddr 0.0.0.0/0 tcp dport 22 accept\nip6 saddr ::/0 tcp dport 22 accept\n" {
		t.Errorf("rules %q, %v", got, err)
	}
	// A hypervisor's VM is on the operator's network: every source, the address never asked.
	v := newVMCreate(t, "qcow2")
	if err := v.create(VMCreate{Platform: "proxmox"}, unknown); err != nil || !strings.Contains(v.log.String(), "SSH accepts every source") {
		t.Errorf("%v\n%s", err, v.log)
	}
}

func TestVMCreateWritesAStaticNetwork(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	c := VMCreate{Platform: "proxmox", IPv6: true, Network: "static", Address: "192.0.2.10/24", Gateway: "192.0.2.1", DNS: []string{"192.0.2.53", "2001:db8::53"}, TuneProfile: "network"}
	if err := v.create(c, nil); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	ud, _, _ := readSeed(t, filepath.Join(v.out, "notes-seed.iso"))
	st, cc := cloudSettings(t, ud)
	if st.TuneProfile != "network" || !st.IPv6 {
		t.Errorf("the install's settings: %+v", st)
	}
	boot := cc.Stages["boot"]
	if len(boot) != 3 || boot[2].Files[0].Path != "/etc/systemd/network/05-notes-static.network" || !slices.Equal(boot[2].Commands, []string{"networkctl reload"}) {
		t.Fatalf("boot %+v", boot)
	}
	// The console's login screen tells how to connect: \4 is the IPv4 address, \6 the IPv6 one.
	if i := slices.IndexFunc(boot[0].Files, func(f cloudFile) bool { return f.Path == "/etc/issue.d/07-notes-connect.issue" }); i < 0 ||
		!regexp.MustCompile("^connect: ssh ([a-z]+)@\\\\4, then `notes-stow status`\n {9}or ssh [a-z]+@\\\\6\n\n$").MatchString(boot[0].Files[i].Content) {
		t.Errorf("no connect line in %+v", boot[0].Files)
	}
	if !strings.Contains(v.log.String(), "network: 192.0.2.10/24 via 192.0.2.1, automatic IPv6, DNS 192.0.2.53, 2001:db8::53\n") {
		t.Errorf("summary:\n%s", v.log)
	}
	// The DNS servers given replace the advertised ones too.
	want := "[Match]\nName=en* eth*\n\n[Network]\nAddress=192.0.2.10/24\nGateway=192.0.2.1\nDNS=192.0.2.53\nDNS=2001:db8::53\n\n[DHCPv4]\nUseDNS=no\n\n[DHCPv6]\nUseDNS=no\n\n[IPv6AcceptRA]\nUseDNS=no\n"
	if got := boot[2].Files[0].Content; got != want {
		t.Errorf("network file:\n%s", got)
	}
	// The same network as data, for the host daemon to answer.
	var rec NetConfig
	if f := boot[2].Files; len(f) != 2 || f[1].Path != "/etc/notes/install-network.yaml" || yaml.Unmarshal([]byte(f[1].Content), &rec) != nil ||
		!reflect.DeepEqual(rec, NetConfig{Mode: "static", Address: "192.0.2.10/24", Gateway: "192.0.2.1", IPv6: true, DNS: []string{"192.0.2.53", "2001:db8::53"}}) {
		t.Errorf("the install's network record: %+v", boot[2].Files)
	}
	// An IPv6 address beside a static IPv4 one, then beside DHCP.
	for _, c := range []struct {
		create      VMCreate
		unit, shown string
	}{
		{VMCreate{Platform: "proxmox", IPv6: true, Network: "static", Address: "192.0.2.10/24", Address6: "2001:db8::10/64", Gateway6: "fe80::1"},
			"[Network]\nAddress=192.0.2.10/24\nAddress=2001:db8::10/64\nGateway=fe80::1\n", "network: 192.0.2.10/24, 2001:db8::10/64 via fe80::1\n"},
		{VMCreate{Platform: "proxmox", IPv6: true, Address6: "2001:DB8::10/64"}, "[Network]\nDHCP=yes\nAddress=2001:db8::10/64\n\n[DHCPv6]\nWithoutRA=solicit\n", "network: DHCP, 2001:db8::10/64\n"},
		{VMCreate{Platform: "proxmox", IPv6: true}, "[Network]\nDHCP=yes\n\n[DHCPv6]\nWithoutRA=solicit\n", "network: DHCP, automatic IPv6\n"},
	} {
		v.log.Reset()
		v.out = t.TempDir()
		if err := v.create(c.create, nil); err != nil {
			t.Fatalf("%+v: %v\n%s", c.create, err, v.log)
		}
		ud, _, _ := readSeed(t, filepath.Join(v.out, "notes-seed.iso"))
		st, cc := cloudSettings(t, ud)
		if !st.IPv6 {
			t.Errorf("%+v: the install's settings leave IPv6 off", c.create)
		}
		boot := cc.Stages["boot"]
		if got := boot[len(boot)-1].Files[0]; got.Path != "/etc/systemd/network/05-notes-static.network" || got.Content != "[Match]\nName=en* eth*\n\n"+c.unit {
			t.Errorf("%+v: %+v", c.create, got)
		}
		// DHCP is recorded too: the record is what tells it from a static install.
		var rec NetConfig
		if f := boot[len(boot)-1].Files; len(f) != 2 || yaml.Unmarshal([]byte(f[1].Content), &rec) != nil || rec.Mode != cmp.Or(c.create.Network, "dhcp") || !rec.IPv6 {
			t.Errorf("%+v: the install's network record: %+v", c.create, f)
		}
		if !strings.Contains(v.log.String(), c.shown) {
			t.Errorf("summary:\n%s", v.log)
		}
	}
	// An address alone: no gateway or DNS shown, nor written.
	v.log.Reset()
	v.out = filepath.Join(t.TempDir(), "alone")
	if err := v.create(VMCreate{Platform: "proxmox", Network: "static", Address: "192.0.2.10/24"}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.log.String(), "network: 192.0.2.10/24, no IPv6\n") {
		t.Errorf("summary:\n%s", v.log)
	}
}

func TestTheSecretsStepsBracketAnIPv6Address(t *testing.T) {
	t.Parallel()
	for addr, want := range map[string]string{
		"2001:db8::7":    "scp -p FILE ops@[2001:db8::7]:secrets.yaml, then ssh -t ops@2001:db8::7 'sudo",
		"192.0.2.7":      "scp -p FILE ops@192.0.2.7:secrets.yaml, then ssh -t ops@192.0.2.7 'sudo",
		"vm.lan":         "scp -p FILE ops@vm.lan:secrets.yaml, then ssh -t ops@vm.lan 'sudo",
		"<address>":      "scp -p FILE ops@<address>:secrets.yaml",
		"::ffff:1.2.3.4": "ops@[::ffff:1.2.3.4]:secrets.yaml",
	} {
		if got := secretsSteps("ops", addr, "notes-stow"); !strings.Contains(got, want) {
			t.Errorf("%s: %s", addr, got)
		}
	}
}

func TestVMCreateWritesTheSSHSourcesAndTheTimeZone(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	c := VMCreate{Platform: "proxmox", SSHAllow: []string{"192.0.2.9/24", " 2001:db8::1/64"}, Timezone: "Europe/Paris"}
	if err := v.create(c, nil); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	ud, _, _ := readSeed(t, filepath.Join(v.out, "notes-seed.iso"))
	_, cc := cloudSettings(t, ud)
	boot := cc.Stages["boot"]
	if len(boot) != 6 {
		t.Fatalf("boot %+v", boot)
	}
	// The sources, masked, each by its family; then the ruleset loaded again, never restarted.
	want := "# SSH, from the sources `notes-stow vm create --ssh-allow` gave.\nip saddr 192.0.2.0/24 tcp dport 22 accept\nip6 saddr 2001:db8::/64 tcp dport 22 accept\n"
	if f := boot[2].Files; len(f) != 1 || f[0].Path != SSHRules || f[0].Content != want {
		t.Errorf("ssh rules %+v", f)
	}
	if !slices.Equal(boot[3].Commands, []string{"nft -f /etc/nftables.conf"}) {
		t.Errorf("reload %+v", boot[3])
	}
	if !slices.Equal(boot[4].Commands, []string{"if [ -f /usr/share/zoneinfo/Europe/Paris ]; then ln -sf /usr/share/zoneinfo/Europe/Paris /etc/localtime && echo Europe/Paris > /etc/timezone; else echo 'no time zone Europe/Paris on this OS: it stays UTC' >&2; fi"}) {
		t.Errorf("time zone %+v", boot[4])
	}
	if !strings.Contains(v.log.String(), "SSH from [192.0.2.0/24 2001:db8::/64]; time zone Europe/Paris") || strings.Contains(v.log.String(), "SSH accepts every source") {
		t.Errorf("summary:\n%s", v.log)
	}
	// Neither given: the bundle's open SSH rule and its UTC are kept, and the run warns.
	v.log.Reset()
	v.out = filepath.Join(t.TempDir(), "open")
	if err := v.create(VMCreate{Platform: "proxmox"}, nil); err != nil {
		t.Fatal(err)
	}
	ud, _, _ = readSeed(t, filepath.Join(v.out, "notes-seed.iso"))
	if _, cc = cloudSettings(t, ud); len(cc.Stages["boot"]) != 3 {
		t.Errorf("boot %+v", cc.Stages["boot"])
	}
	if !strings.Contains(v.log.String(), "SSH accepts every source: --ssh-allow CIDR") || !strings.Contains(v.log.String(), "SSH from every source; time zone UTC") {
		t.Errorf("summary:\n%s", v.log)
	}
}

// The plan names the L4 ports the VM opens, and from which sources, and the host devices the
// stack maps, which the VM must have.
func TestVMCreatePlanNamesTheL4PortsAndTheDevices(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	v.r.Spec.L4.Routes = []spec.L4Route{
		{Name: "db", Protocol: "tcp", Port: 15432, Upstream: "postgres:5432"},
		{Name: "feed", Protocol: "udp", Port: 17007, Upstream: "api:17007", AllowInput: "FEED_SOURCES"},
	}
	v.r.Compose = bytes.Replace(v.r.Compose, []byte("  web:\n"), []byte("  web:\n    devices: [\"/dev/ttyUSB0:/dev/ttyS0\"]\n"), 1)
	if err := v.create(VMCreate{Platform: "libvirt"}, nil); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	for _, want := range []string{"  opens tcp/15432 (L4 route db) to every source\n", "  opens udp/17007 (L4 route feed) to the sources of FEED_SOURCES\n",
		"  the service web maps the host's /dev/ttyUSB0: the VM must have them\n"} {
		if !strings.Contains(v.log.String(), want) {
			t.Errorf("no %q in:\n%s", want, v.log)
		}
	}
}

func TestVMCreateWithSecretsWaitsForThemOverSSH(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	if err := v.create(VMCreate{Platform: "proxmox", Secrets: []byte("project: notes\nsecrets:\n  SESSION_SECRET: kept\n")}, nil); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	ud, _, _ := readSeed(t, filepath.Join(v.out, "notes-seed.iso"))
	if st, _ := cloudSettings(t, ud); !st.AwaitSecrets || strings.Contains(ud, "kept") {
		t.Errorf("seed:\n%s", ud)
	}
	if !strings.Contains(v.out2(), "Its first install waits for its secrets") ||
		!strings.Contains(v.out2(), "@<address>:secrets.yaml, then ssh -t ") || !strings.Contains(v.out2(), "@<address> 'sudo notes-stow install --secrets secrets.yaml; rm secrets.yaml'") {
		t.Errorf("out:\n%s", v.out2())
	}
}

func TestVMCreateRefusals(t *testing.T) {
	t.Parallel()
	secret := func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "API_KEY", Prompt: "API key", Secret: true, Required: true})
	}
	for _, c := range []struct {
		name    string
		formats []string
		mod     func(*spec.Spec)
		create  VMCreate
		opts    func(*Options)
		want    string
	}{
		{"no disk", nil, nil, VMCreate{Platform: "truenas"}, nil, "has no disk in"},
		{"no platform", []string{"qcow2"}, nil, VMCreate{}, nil, "--platform is required (truenas, proxmox, libvirt)"},
		{"platform without its format", []string{"qcow2"}, nil, VMCreate{Platform: "aws"}, nil, "--platform aws: notes 1.0.0 offers truenas, proxmox, libvirt"},
		{"unknown platform", []string{"ova"}, nil, VMCreate{Platform: "virtualbox"}, nil, "offers vmware"},
		{"bad host name", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", Hostname: "Notes_1"}, nil, "--hostname: \"Notes_1\" is not a host name"},
		{"root", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", User: "root"}, nil, "root is the OS's own"},
		{"bad key", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", SSHKey: "/etc/hostname"}, nil, "not an SSH public key"},
		{"address with DHCP", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", Address: "192.0.2.10/24"}, nil, "are for --network static"},
		{"static without address", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", Network: "static"}, nil, "--address is required"},
		{"address without prefix", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", Network: "static", Address: "192.0.2.10"}, nil, "with its prefix"},
		{"IPv6 address as IPv4's", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", Network: "static", Address: "2001:db8::10/64"}, nil, `the VM's network: address "2001:db8::10" is not an IPv4 address`},
		{"network address", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", Network: "static", Address: "192.0.2.0/24"}, nil, "network address of 192.0.2.0/24"},
		{"IPv6 gateway alone", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", IPv6: true, Gateway6: "fe80::1"}, nil, "gateway6 is for a static IPv6 address"},
		{"IPv4 as IPv6's", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", IPv6: true, Network: "static", Address: "192.0.2.10/24", Address6: "192.0.2.11/24"}, nil, "address6 \"192.0.2.11/24\" is not an IPv6 address"},
		{"IPv6 gateway outside", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", IPv6: true, Address6: "2001:db8::10/64", Gateway6: "2001:db9::1"}, nil, "gateway6 2001:db9::1 is outside 2001:db8::/64"},
		{"IPv6 address without --ipv6", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", Network: "static", Address: "192.0.2.10/24", Address6: "2001:db8::10/64"}, nil, "--address6 and --gateway6 are for --ipv6"},
		{"IPv6 DNS without --ipv6", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", Network: "static", Address: "192.0.2.10/24", DNS: []string{"2001:db8::53"}}, nil, "DNS server 2001:db8::53 is IPv6, which is off"},
		{"bad network", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", Network: "bridge"}, nil, "dhcp or static"},
		{"unknown tune profile", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", TuneProfile: "shared"}, nil, `--tune-profile: profile "shared": default or network`},
		{"ssh source without prefix", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", SSHAllow: []string{"192.0.2.0/24", "192.0.2.7"}}, nil, `--ssh-allow: "192.0.2.7" is not a source with its prefix`},
		{"time zone not a name", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", Timezone: "../../etc/passwd"}, nil, "--timezone: \"../../etc/passwd\" is not a time zone name"},
		{"unknown time zone", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", Timezone: "Mars/Olympus"}, nil, "--timezone: \"Mars/Olympus\" is not a time zone:"},
		{"Go's local zone", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", Timezone: "Local"}, nil, "--timezone: \"Local\" is not a time zone name"},
		{"the database's own file", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", Timezone: "localtime"}, nil, "--timezone: \"localtime\" is not a time zone name"},
		{"a leap-second zone", []string{"qcow2"}, nil, VMCreate{Platform: "truenas", Timezone: "right/Europe/Paris"}, nil, "--timezone: \"right/Europe/Paris\" is not a time zone name"},
		{"byo", []string{"qcow2"}, nil, VMCreate{Platform: "truenas"}, func(o *Options) { o.TLS = "byo" }, "never carries"},
		{"loopback", []string{"qcow2"}, nil, VMCreate{Platform: "truenas"}, func(o *Options) { o.TLS = "none" }, "nothing outside it reaches"},
		{"no mode for a VM", []string{"qcow2"}, func(s *spec.Spec) { s.Front.TLSModes = []string{"none", "byo"} }, VMCreate{Platform: "virtualbox", Hostname: "Bad_Name"}, nil, "offers no TLS mode a VM can be created with: none, byo"},
		{"secret input", []string{"qcow2"}, secret, VMCreate{Platform: "truenas"}, func(o *Options) { o.Set = map[string]string{"API_KEY": "k"} }, "API_KEY is a secret input"},
		{"unknown input", []string{"qcow2"}, nil, VMCreate{Platform: "truenas"}, func(o *Options) { o.Set = map[string]string{"NOPE": "k"} }, "no input of this install is named NOPE"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			v := newVMCreate(t, c.formats...)
			if c.mod != nil {
				c.mod(v.r.Spec)
			}
			if c.formats == nil {
				v.r.Manifest.Artifacts.Disks = nil
			}
			if err := v.create(c.create, c.opts); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if _, err := os.Stat(v.out); err == nil {
				t.Error("a refused run wrote", v.out)
			}
		})
	}
}

func TestVMCreateReadsNoInputFromTheEnvironment(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	if v.vars["ADMIN_EMAIL"] == "" {
		t.Fatal("the environment gives no ADMIN_EMAIL: nothing is shown")
	}
	in := v.installer(func(o *Options) { o.Domains = map[string]string{"app": "app.example.org", "api": "api.example.org"} })
	err := in.CreateVM(context.Background(), &VMCreate{Platform: "truenas", Out: v.out, SSHKey: v.key})
	if err == nil || !strings.Contains(err.Error(), "ADMIN_EMAIL") {
		t.Fatalf("err = %v", err)
	}
}

func TestVMCreateTakesNoDomainFromTheEnvironment(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	v.vars["APP_DOMAIN"], v.vars["API_DOMAIN"] = "app.example.org", "api.example.org"
	in := v.installer(func(o *Options) { o.Set = map[string]string{"ADMIN_EMAIL": "a@example.org"} })
	err := in.CreateVM(context.Background(), &VMCreate{Platform: "truenas", Out: v.out, SSHKey: v.key})
	if err == nil || !strings.Contains(err.Error(), "give --domains") {
		t.Fatalf("err = %v", err)
	}
}

func TestVMCreateReadsTheDiskWithThePullTokenFromTheEnvironment(t *testing.T) {
	t.Parallel()
	pull := spec.Provider{Name: "token", Config: map[string]any{"user": "reader", "token_env": "NOTES_PULL_TOKEN"}}
	v := newVMCreatePulling(t, pull, "qcow2")
	v.vars["NOTES_PULL_TOKEN"] = "from-env"
	v.auth = nil
	if err := v.create(VMCreate{Platform: "truenas"}, nil); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("reader:from-env"))
	if !slices.Contains(v.auth, want) {
		t.Errorf("auth %q, want %q", v.auth, want)
	}
}

func TestVMCreateListsTheSecretInputsGivenOverSSH(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	v.r.Spec.Inputs = append(v.r.Spec.Inputs, spec.Input{Key: "API_KEY", Prompt: "API key", Secret: true, Required: true})
	if err := v.create(VMCreate{Platform: "truenas", User: "ops"}, nil); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	if !strings.Contains(v.out2(), "The install waits for API_KEY") || !strings.Contains(v.out2(), "ssh ops@<address> sudo notes-stow install") {
		t.Errorf("out:\n%s", v.out2())
	}
	ud, _, _ := readSeed(t, filepath.Join(v.out, "notes-seed.iso"))
	if strings.Contains(ud, "API_KEY") {
		t.Errorf("the user-data names the secret:\n%s", ud)
	}
}

func TestVMCreateAsksWhatNoFlagGave(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "ova")
	delete(v.vars, "ADMIN_EMAIL")
	p := &fakePrompter{answers: map[string]string{"--platform": "vmware", "--hostname": "asked", "--user": "", "--ssh-key": v.key,
		"--network": "dhcp", "--tls": "internal", "APP_DOMAIN": "app.example.org", "API_DOMAIN": "api.example.org", "ADMIN_EMAIL": "asked@example.org", "confirm": "yes"}}
	if err := v.create(VMCreate{SSHKey: "-"}, func(o *Options) { o.Prompt = p }); err == nil {
		t.Fatal("a key file \"-\" was taken")
	}
	// An answer the form let through is checked again.
	p.answers["--hostname"] = "Not_A_Host"
	if err := v.create(VMCreate{}, func(o *Options) { o.Prompt = p }); err == nil || !strings.Contains(err.Error(), "is not a host name") {
		t.Fatalf("err = %v", err)
	}
	p.answers["--hostname"] = "asked"
	p.forms = nil
	in := v.installer(func(o *Options) {
		o.Prompt = p
		o.Getenv = func(k string) string { return map[string]string{"USER": "dev"}[k] }
	})
	if err := in.CreateVM(context.Background(), &VMCreate{Out: v.out}); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	var asked []string
	for _, f := range p.forms {
		for _, q := range f.Questions {
			asked = append(asked, q.Key)
		}
	}
	// Where the built-in module is a preview, how is asked, and by hand is the default.
	if !slices.Equal(asked, []string{"--platform", "--create", "--hostname", "--user", "--ssh-key", "--network", "--tls", "APP_DOMAIN", "API_DOMAIN", "ADMIN_EMAIL", "confirm"}) {
		t.Errorf("asked %v", asked)
	}
	if d := p.forms[1].Defaults(nil)["--create"]; d != CreateManual {
		t.Errorf("--create defaults to %q on a preview", d)
	}
	var modes []string
	for _, c := range p.forms[3].Questions[0].Choices {
		modes = append(modes, c.Value)
	}
	if !slices.Equal(modes, []string{"acme", "internal"}) {
		t.Errorf("modes offered %v: a VM is offered neither loopback nor byo", modes)
	}
	if f := p.forms[4]; !f.Review || f.Go != "Go on with these answers" {
		t.Errorf("the inputs form %+v", f)
	}
	ud, _, _ := readSeed(t, filepath.Join(v.out, "asked-seed.iso"))
	st, cc := cloudSettings(t, ud)
	if _, ok := cc.Stages["boot"][0].Users["dev"]; !ok || st.Inputs["ADMIN_EMAIL"] != "asked@example.org" {
		t.Errorf("users %v, settings %+v", cc.Stages["boot"][0].Users, st)
	}
	// Quitting at the confirmation writes nothing.
	q := &fakePrompter{answers: map[string]string{"--platform": "vmware", "--network": "dhcp", "--tls": "internal", "APP_DOMAIN": "a.example.org", "API_DOMAIN": "b.example.org", "ADMIN_EMAIL": "a@example.org", "confirm": "no"}}
	v.out = filepath.Join(t.TempDir(), "quit")
	if err := v.create(VMCreate{}, func(o *Options) { o.Prompt = q }); err != ErrAborted {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(v.out); err == nil {
		t.Error("quitting wrote", v.out)
	}
}

func TestAuthorizedKeyTakesOneKeyOfItsOwnType(t *testing.T) {
	t.Parallel()
	_, line := sshKey(t)
	f := strings.Fields(line)
	dir := t.TempDir()
	for name, c := range map[string]struct {
		content string
		ok      bool
	}{
		"key":           {line + "\n", true},
		"no comment":    {f[0] + " " + f[1], true},
		"two keys":      {line + "\n" + line, false},
		"type mismatch": {"ssh-rsa " + f[1], false},
		"unknown type":  {"ssh-dss " + f[1], false},
		"not base64":    {"ssh-ed25519 !!!", false},
		"short blob":    {"ssh-ed25519 AAAA", false},
		"type only":     {"ssh-ed25519", false},
		"private key":   {"-----BEGIN OPENSSH PRIVATE KEY-----", false},
	} {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		_ = os.WriteFile(p, []byte(c.content), 0o600)
		if _, err := authorizedKey(p); (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestHostAndUserNamesAtTheirBounds(t *testing.T) {
	t.Parallel()
	label := strings.Repeat("a", 63)
	for v, ok := range map[string]bool{"notes": true, "notes-1.lan": true, label: true, label + "a": false, "-notes": false, "notes-": false,
		"Notes": false, "no_tes": false, "": false, "a..b": false, strings.Repeat(label+".", 4)[:253]: true, strings.Repeat(label+".", 4)[:254]: false} {
		if err := checkHostname(v); (err == nil) != ok {
			t.Errorf("host %q: %v", v, err)
		}
	}
	user := "a" + strings.Repeat("b", 31)
	for v, ok := range map[string]bool{"ops": true, "ubuntu": true, "_ops": false, "systemd-x": false, "www-data": false, user: true, user + "c": false, "1ops": false, "Ops": false, "root": false, "admin": false, "kairos": false} {
		if err := checkUser(v); (err == nil) != ok {
			t.Errorf("user %q: %v", v, err)
		}
	}
}

// bootVia makes v's release one that published no disk: it records set as the disk set it boots,
// and the channel's pointer is release 0.9.0, published with v's disks by stowage by.
func bootVia(t *testing.T, v *vmCreate, set *release.BootDisks, by string) {
	t.Helper()
	ptr := *v.r.Manifest
	ptr.ProjectVersion, ptr.Tag, ptr.StowageVersion, ptr.ReleaseSeq = "0.9.0", "0.9.0", by, 1
	ptr.Artifacts.Images = maps.Clone(ptr.Artifacts.Images)
	v.putRelease(ptr.Channel, &ptr)
	v.r.Manifest.Artifacts.Disks, v.r.Manifest.Artifacts.BootDisks = nil, set
}

// A release that published no disk boots its channel's disk set: the one it records, else, gone,
// the pointer's; its first boot takes the release's OS bundle.
func TestVMCreateBootsTheChannelsDiskSet(t *testing.T) {
	t.Parallel()
	gone := &release.BootDisks{Release: "0.8.0", Stowage: "0.6.0", Disks: map[string]string{"qcow2": "sha256:" + strings.Repeat("e", 64)}}
	for _, c := range []struct {
		name string
		set  *release.BootDisks
	}{{"recorded", nil}, {"pointer's", gone}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			v := newVMCreate(t, "qcow2")
			set := c.set
			if set == nil {
				set = &release.BootDisks{Release: "0.9.0", Stowage: "0.6.0", Disks: maps.Clone(v.r.Manifest.Artifacts.Disks)}
			}
			bootVia(t, v, set, "0.6.0")
			err := v.create(VMCreate{Platform: "truenas", Hostname: "notes-1", User: "ops"}, func(o *Options) { o.TLS = "internal" })
			if err != nil {
				t.Fatalf("%v\n%s", err, v.log)
			}
			disk, _ := os.ReadFile(filepath.Join(v.out, "notes-1.0.0.qcow2")) // named by the disk's own title
			if !bytes.Equal(disk, v.disks["qcow2"]) {
				t.Fatalf("disk %q", disk)
			}
			ud, _, _ := readSeed(t, filepath.Join(v.out, "notes-1-seed.iso"))
			st, _ := cloudSettings(t, ud)
			m := v.r.Manifest
			if r := st.Release; r == nil || *r != (OSRelease{m.ProjectVersion, m.Revision, m.Tag, m.Artifacts.OS}) {
				t.Fatalf("release %+v", r)
			}
		})
	}
}

// A release's own disk: the first boot installs from its OS, no release named.
func TestVMCreateOfItsOwnDiskNamesNoRelease(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	if err := v.create(VMCreate{Platform: "truenas", Hostname: "notes-1", User: "ops"}, func(o *Options) { o.TLS = "internal" }); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	ud, _, _ := readSeed(t, filepath.Join(v.out, "notes-1-seed.iso"))
	if st, _ := cloudSettings(t, ud); st.Release != nil {
		t.Fatalf("release %+v", st.Release)
	}
}

// A disk set an older stowage published, whose first boot cannot take another release, is
// refused; so is a release whose channel holds no disk.
func TestVMCreateRefusesADiskSetThatCannotSwitch(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	bootVia(t, v, &release.BootDisks{Release: "0.9.0", Stowage: "0.5.0", Disks: maps.Clone(v.r.Manifest.Artifacts.Disks)}, "0.5.0")
	err := v.create(VMCreate{Platform: "truenas", Hostname: "notes-1", User: "ops"}, func(o *Options) { o.TLS = "internal" })
	if err == nil || !strings.Contains(err.Error(), `published by stowage "0.5.0", whose first boot cannot take another release's OS`) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(v.out); err == nil {
		t.Error("a refused run wrote", v.out)
	}
}

// What the VM's first install would refuse of the front is refused before any file is written.
func TestVMCreateRefusesTheFrontsPortsAndDomainsFirst(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	err := v.create(VMCreate{Platform: "truenas", Hostname: "notes-1", User: "ops"}, func(o *Options) { o.TLS = "internal"; o.HTTPPort, o.HTTPSPort = 8080, 80 })
	if err == nil || !strings.Contains(err.Error(), "--https-port 80") {
		t.Fatalf("https on 80: %v", err)
	}
	err = v.create(VMCreate{Platform: "truenas", Hostname: "notes-1", User: "ops"}, func(o *Options) {
		o.TLS, o.Set = "internal", map[string]string{"APP_DOMAIN": "x.example.com", "API_DOMAIN": "x.example.com"}
	})
	if err == nil || !strings.Contains(err.Error(), "app and api both serve the domain x.example.com") {
		t.Fatalf("one domain for two routes: %v", err)
	}
	if err := v.create(VMCreate{Platform: "truenas", Hostname: "notes-1", User: "ops"}, func(o *Options) { o.TLS, o.HTTPPort = "internal", 22 }); err == nil || !strings.Contains(err.Error(), "--http-port 22 is the VM's SSH") {
		t.Fatalf("the SSH port: %v", err)
	}
	v.r.Spec.L4.Routes = []spec.L4Route{{Name: "data", Protocol: "tcp", Port: 7443, Upstream: "api:7443"}}
	err = v.create(VMCreate{Platform: "truenas", Hostname: "notes-1", User: "ops"}, func(o *Options) { o.TLS, o.HTTPSPort = "internal", 7443 })
	if err == nil || !strings.Contains(err.Error(), "7443 is the L4 route data's") {
		t.Fatalf("an L4 route's port: %v", err)
	}
	if _, err := os.Stat(v.out); err == nil {
		t.Error("a refused run wrote", v.out)
	}
}

// With neither the release's disks nor the pointer's there, the channel's disk set record
// gives the set.
func TestVMCreateBootsTheChannelsDiskSetRecord(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	disks := maps.Clone(v.r.Manifest.Artifacts.Disks)
	gone := &release.BootDisks{Release: "0.8.0", Stowage: "0.6.0", Disks: map[string]string{"qcow2": "sha256:" + strings.Repeat("e", 64)}}
	bootVia(t, v, gone, "0.6.0")
	// The pointer boots the same gone set.
	ptr := *v.r.Manifest
	ptr.Artifacts.Images = maps.Clone(ptr.Artifacts.Images)
	v.putRelease(ptr.Channel, &ptr)
	data, _ := json.Marshal(release.DiskSet{Channel: ptr.Channel, Release: "0.9.0", Stowage: "0.6.0", Disks: disks})
	c := registry.New(nil)
	tag, _ := c.Tag(v.prefix+"/notes-disk", release.DiskSetTag(ptr.Channel))
	if _, err := c.PutArtifact(context.Background(), tag, registry.Artifact{Type: release.DiskSetMediaType, Data: data}); err != nil {
		t.Fatal(err)
	}
	if err := v.create(VMCreate{Platform: "truenas", Hostname: "notes-1", User: "ops"}, func(o *Options) { o.TLS = "internal" }); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	if !strings.Contains(v.log.String(), "disk of 0.9.0") {
		t.Errorf("log %s", v.log)
	}
}

// A user-data ending on a sector boundary: go-diskfs then writes a sector of zeros past its
// volume, which the seed's padding replaces.
func TestSeedISOOfWholeSectors(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 2048, 4096, 2047, 2049} {
		path := filepath.Join(t.TempDir(), "seed.iso")
		ud := strings.Repeat("x", n)
		if err := WriteSeedISO(path, []byte(ud), []byte("instance-id: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, _, label := readSeed(t, path); got != ud || strings.TrimSpace(label) != SeedVolume {
			t.Errorf("user-data of %d bytes read back as %d, label %q", n, len(got), label)
		}
	}
}

// A disk set that does not say what Kairos takes of a disk boots no VM: vm create cannot size it.
func TestADiskWithoutItsOSSizeIsRefused(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	c := registry.New(nil)
	repo, err := c.Repo(v.prefix + "/notes-disk")
	if err != nil {
		t.Fatal(err)
	}
	b := registry.Blob{MediaType: release.DiskMediaType("qcow2"), Data: []byte("an older disk"), Annotations: map[string]string{registry.TitleAnnotation: "notes-1.0.0.qcow2"}}
	h, err := c.PutManifest(context.Background(), repo, registry.Manifest{ArtifactType: release.DiskArtifactType,
		Config: registry.Blob{MediaType: registry.EmptyMediaType, Data: []byte("{}")}, Layers: []registry.Blob{b}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v.r.Manifest.Artifacts.Disks = map[string]string{"qcow2": h.String()}
	if err := v.create(VMCreate{Platform: "proxmox"}, nil); err == nil || !strings.Contains(err.Error(), "records no dev.stowage.disk.os-mib: published by an earlier Stowage") {
		t.Errorf("%v", err)
	}
	// One disk of the set without it: a platform that takes the other format boots it, one that
	// takes only that format is told why.
	w := newVMCreate(t, "raw")
	wrepo, err := c.Repo(w.prefix + "/notes-disk")
	if err != nil {
		t.Fatal(err)
	}
	if h, err = c.PutManifest(context.Background(), wrepo, registry.Manifest{ArtifactType: release.DiskArtifactType,
		Config: registry.Blob{MediaType: registry.EmptyMediaType, Data: []byte("{}")}, Layers: []registry.Blob{b}}, nil); err != nil {
		t.Fatal(err)
	}
	w.r.Manifest.Artifacts.Disks["ova"] = h.String()
	if err := w.create(VMCreate{Platform: "aws"}, nil); err != nil {
		t.Fatalf("the raw disk of a set whose ova records no size: %v", err)
	}
	if err := w.create(VMCreate{Platform: "vmware", Out: t.TempDir()}, nil); err == nil || !strings.Contains(err.Error(), "--platform vmware: the ova disk") || !strings.Contains(err.Error(), "published by an earlier Stowage") {
		t.Errorf("a platform whose format records no size: %v", err)
	}
	if h, err = c.PutManifest(context.Background(), wrepo, registry.Manifest{ArtifactType: release.DiskArtifactType,
		Config: registry.Blob{MediaType: registry.EmptyMediaType, Data: []byte("{}")}, Layers: []registry.Blob{b},
		Annotations: map[string]string{release.DiskOSAnnotation: "0"}}, nil); err != nil {
		t.Fatal(err)
	}
	w.r.Manifest.Artifacts.Disks["ova"] = h.String()
	if err := w.create(VMCreate{Platform: "vmware", Out: t.TempDir()}, nil); err == nil || !strings.Contains(err.Error(), `records dev.stowage.disk.os-mib "0"`) || !strings.Contains(err.Error(), "not as Stowage publishes it") {
		t.Errorf("a platform whose format records 0: %v", err)
	}
}

// A mirror exported with some of a release's disk formats boots the platforms that take one of
// them; a platform that takes only another is told which format the registry lacks.
func TestVMCreateBootsTheFormatsAMirrorCarries(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	v.r.Manifest.Artifacts.Disks["raw"] = "sha256:" + strings.Repeat("e", 64)
	if err := v.create(VMCreate{Platform: "aws", Out: t.TempDir()}, nil); err == nil || !strings.Contains(err.Error(), "--platform aws: the raw disk sha256:eeeeeeeeeeee is not in "+v.prefix) {
		t.Errorf("a platform whose format is not there: %v", err)
	}
	if err := v.create(VMCreate{Platform: "truenas", Hostname: "notes-1", User: "ops"}, func(o *Options) { o.TLS = "internal" }); err != nil {
		t.Fatalf("the format there: %v\n%s", err, v.log)
	}
}

func TestOSMiBIsBounded(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]bool{"1": true, "0": false, "-1": false, "16777216": true, "16777217": false, "1e3": false, "": false, "9223372036854775808": false} {
		if _, ok := osMiBOf(s); ok != want {
			t.Errorf("%q: %v", s, ok)
		}
	}
}

// The release's NICs on a machine: a secondary's addressing (none by default), MACs generated on
// a hypervisor for more than one NIC, required on metal, none on a cloud of one NIC; refusals
// name the flag.
func TestMachineInterfacesFromTheFlags(t *testing.T) {
	t.Parallel()
	s := &spec.Spec{Name: "atlas", Host: &spec.Host{Interfaces: map[string]spec.HostInterface{
		"uplink": {Primary: true}, "edge": {}, "vpn0": {Kind: spec.InterfaceTUN},
	}}}
	plat := func(key string) Platform {
		for _, p := range Platforms {
			if p.Key == key {
				return p
			}
		}
		t.Fatalf("no platform %s", key)
		return Platform{}
	}
	got, err := machineInterfaces(s, []string{"edge=static,address=192.0.2.9/24,attach=br1"}, plat("truenas"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Role != "edge" || got[0].Attach != "br1" || got[0].Mode != IfaceStatic || got[0].Address != "192.0.2.9/24" ||
		got[1].Role != "uplink" || !got[1].Primary || got[1].Mode != IfaceNone {
		t.Errorf("truenas: %+v", got)
	}
	for _, i := range got {
		if !macAddr.MatchString(i.MAC) || !strings.HasPrefix(i.MAC, "02:") || !unicastMAC(i.MAC) {
			t.Errorf("generated MAC %s", i.MAC)
		}
	}
	if got[0].MAC == got[1].MAC {
		t.Errorf("two NICs of one MAC %s", got[0].MAC)
	}
	// Generated afresh on each run: two VMs of one host name never share one.
	again, _ := machineInterfaces(s, nil, plat("truenas"))
	if again[0].MAC == got[0].MAC {
		t.Errorf("the same MAC on two runs: %s", got[0].MAC)
	}
	for mac, uni := range map[string]bool{"02:00:00:00:00:01": true, "0c:00:00:00:00:01": true, "1a:00:00:00:00:01": true, "0b:00:00:00:00:01": false, "0f:00:00:00:00:01": false, "01:00:5e:00:00:01": false, "00:00:00:00:00:00": false} {
		if unicastMAC(mac) != uni {
			t.Errorf("unicastMAC(%s) = %v", mac, !uni)
		}
	}
	got, err = machineInterfaces(s, []string{"uplink=mac=02:00:00:00:00:0A", "edge=dhcp,mac=02:00:00:00:00:0b"}, plat("metal"))
	if err != nil || got[0].MAC != "02:00:00:00:00:0b" || got[0].Mode != IfaceDHCP || got[1].MAC != "02:00:00:00:00:0a" {
		t.Errorf("metal: %+v %v", got, err)
	}
	for _, c := range []struct {
		given []string
		plat  string
		want  string
	}{
		{nil, "metal", "--interface edge=mac=…: a server names each NIC"},
		{[]string{"edge=mac=0b:00:00:00:00:01"}, "truenas", "is not a unicast MAC address"},
		{[]string{"uplink=mac=02:00:00:00:00:01", "edge=mac=02:00:00:00:00:01"}, "metal", "have the MAC 02:00:00:00:00:01"},
		{nil, "gcp", "names its VMs' NICs itself, one here: atlas declares 2"},
		{[]string{"edge=mac=02:00:00:00:00:0b"}, "gcp", "names its VMs' NICs itself"},
		{[]string{"vpn0=dhcp"}, "truenas", `"vpn0" is not one of atlas's NICs (edge, uplink)`},
		{[]string{"uplink=dhcp"}, "truenas", "the primary NIC's addressing is --network's"},
		{[]string{"edge=static"}, "truenas", "the static mode's address is required"},
		{[]string{"edge=dhcp,address=192.0.2.9/24"}, "truenas", "address= is the static mode's"},
		{[]string{"edge=static,address=2001:db8::1/64"}, "truenas", "is not an IPv4 address with its prefix"},
		{[]string{"edge=mac=03:00:00:00:00:01"}, "truenas", "is not a unicast MAC address"},
		{[]string{"edge=attach=br1"}, "metal", `"attach=br1": dhcp, static or none`},
		{[]string{"edge=dhcp", "edge=none"}, "truenas", "given twice"},
		{[]string{"edge=bogus"}, "truenas", `"bogus": dhcp, static or none`},
	} {
		if _, err := machineInterfaces(s, c.given, plat(c.plat)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v on %s: %v, want %q", c.given, c.plat, err, c.want)
		}
	}
	one := &spec.Spec{Name: "atlas", Host: &spec.Host{Interfaces: map[string]spec.HostInterface{"uplink": {Primary: true}}}}
	if got, err := machineInterfaces(one, nil, plat("gcp")); err != nil || len(got) != 1 || got[0].MAC != "" {
		t.Errorf("a cloud's one NIC: %+v %v", got, err)
	}
	if _, err := machineInterfaces(one, []string{"uplink=mac=02:00:00:00:00:0a"}, plat("gcp")); err == nil || !strings.Contains(err.Error(), "gives its VMs' NICs their MACs") {
		t.Errorf("a cloud's MAC: %v", err)
	}
	if _, err := machineInterfaces(one, nil, plat("metal")); err == nil || !strings.Contains(err.Error(), "a server names each NIC") {
		t.Errorf("a server's one NIC without its MAC: %v", err)
	}
	// A hypervisor's machine of one NIC: named from the NIC it has, whatever MAC its module gives.
	if got, err := machineInterfaces(one, nil, plat("truenas")); err != nil || len(got) != 1 || got[0].MAC != "" {
		t.Errorf("a hypervisor's one NIC: %+v %v", got, err)
	}
	if _, err := machineInterfaces(&spec.Spec{Name: "notes"}, []string{"edge=dhcp"}, plat("truenas")); err == nil || !strings.Contains(err.Error(), "declares no NIC") {
		t.Errorf("no interfaces: %v", err)
	}
}

// The seed names each NIC by its role before the booted system's udev does (by MAC, or the only
// NIC's original name), renames one named before, and addresses each: the primary as the machine's
// network, a secondary never the default route's, the DNS servers' nor the host name's.
func TestTheSeedNamesAndAddressesTheNICs(t *testing.T) {
	t.Parallel()
	m := Machine{Hostname: "h1", User: "ops", SSHKey: "ssh-ed25519 AAAA k", Net: NetConfig{Mode: "dhcp"}, Interfaces: []MachineInterface{
		{Role: "edge", MAC: "02:00:00:00:00:0b", Mode: IfaceDHCP},
		{Role: "uplink", MAC: "02:00:00:00:00:0a", Primary: true},
	}}
	raw, err := UserData("atlas", "at-stow", m, Settings{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var cc cloudConfig
	if err := yaml.Unmarshal(raw, &cc); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	var cmds []string
	for _, at := range []string{"initramfs", "boot"} {
		for _, st := range cc.Stages[at] {
			for _, f := range st.Files {
				files[at+":"+f.Path] = f.Content
			}
			cmds = append(cmds, st.Commands...)
		}
	}
	for path, want := range map[string]string{
		"initramfs:/etc/systemd/network/10-atlas-uplink.link": "[Match]\nMACAddress=02:00:00:00:00:0a\n\n[Link]\nName=uplink\n",
		"initramfs:/etc/systemd/network/10-atlas-edge.link":   "[Match]\nMACAddress=02:00:00:00:00:0b\n\n[Link]\nName=edge\n",
		"boot:/etc/systemd/network/05-atlas-static.network":   "[Match]\nName=uplink\n\n[Network]\nDHCP=ipv4\n",
		"boot:/etc/systemd/network/10-atlas-edge.network":     "[Match]\nName=edge\n\n[Network]\nDHCP=ipv4\nLinkLocalAddressing=no\nIPv6AcceptRA=no\n\n[DHCPv4]\nUseGateway=no\nUseRoutes=no\nUseDNS=no\nSendHostname=no\n\n[Link]\nRequiredForOnline=no\n",
	} {
		if !strings.Contains(files[path], want) {
			t.Errorf("%s:\n%s\nwant it to hold\n%s", path, files[path], want)
		}
	}
	if !slices.ContainsFunc(cmds, func(c string) bool {
		return strings.Contains(c, `= 02:00:00:00:00:0b ] && basename`) && strings.Contains(c, `ip link set dev "$i" name edge`)
	}) {
		t.Errorf("no rename of edge in %q", cmds)
	}
	one := m
	one.Interfaces = []MachineInterface{{Role: "uplink", Primary: true}}
	raw, _ = UserData("atlas", "at-stow", one, Settings{}, nil)
	if !strings.Contains(string(raw), "OriginalName=en* eth*") || !strings.Contains(string(raw), "Driver=!mlx4_core mlx5_core mana") || !strings.Contains(string(raw), "ip -o route show default") {
		t.Errorf("one NIC without its MAC:\n%s", raw)
	}
	none := m
	none.Interfaces = nil
	raw, _ = UserData("atlas", "at-stow", none, Settings{}, nil)
	if strings.Contains(string(raw), ".link") || !strings.Contains(string(raw), "Name=en* eth*") {
		t.Errorf("no interfaces:\n%s", raw)
	}
}
