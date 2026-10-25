package kairos

import (
	"archive/tar"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/norvexlabs/stowage/internal/spec"
)

var params = Params{Name: "notes", Ctl: "notes-stow", Base: "ubuntu:26.04@sha256:" + strings.Repeat("a", 64),
	KairosInit: "quay.io/kairos/kairos-init:v4.4.0-rc2@sha256:" + strings.Repeat("b", 64), OSVersion: "1.2.0-r2"}

func TestDockerfileRendersTheParams(t *testing.T) {
	t.Parallel()
	df, err := Dockerfile(params)
	if err != nil {
		t.Fatal(err)
	}
	s := string(df)
	for _, want := range []string{"FROM " + params.KairosInit + " AS kairos-init", "FROM " + params.Base + "\n",
		"-s install -v 0.0.0\n", `-s init --skip-step cisHardening -v "1.2.0-r2"`, "nftables open-vm-tools qemu-guest-agent tzdata", "apt-cache show tzdata-legacy", "/usr/share/notes-preload/", "/etc/notes/os-version",
		"notes-preload.service", "chmod 0440 /etc/sudoers.d/10-notes", "notes-health.service", "notes-network.service", "notes-tune.service", "notes-tune-online.service", "ethtool",
		"mv /usr/share/notes/08_grub.yaml \"$f\"", "sha256sum -c -", "shapes.kairos.kairos_init: its", "grep -qx 'AllowTcpForwarding no' /etc/ssh/sshd_config.d/05-kairos-hardening.conf",
		"mv /etc/ssh/sshd_config.d/05-kairos-hardening.conf /usr/lib/notes/sshd_config.d/",
		"printf 'Include /usr/lib/notes/sshd_config.d/*.conf\\n' > /etc/ssh/sshd_config.d/00-notes-hardening.conf", "grep -qx 'allowtcpforwarding no' \"$k/effective\"",
		"! test -e /etc/modprobe.d/cis-blocklist.conf", "umask[[:space:]]+0*27([^0-7]|$)' /etc/profile /etc/profile.d", "UMASK[[:space:]]+0*27", "sed -i '/^        net\\.core\\.rmem_max: 7500000$/d'"} {
		if !strings.Contains(s, want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
	if strings.Contains(s, "{{") {
		t.Error("Dockerfile has an unrendered action")
	}
	// The release's version first appears after the slow layers: kairos-init's install stage and
	// Docker's packages stay cached from one release to the next.
	if v, d := strings.Index(s, params.OSVersion), strings.Index(s, "docker-compose-plugin"); d < 0 || v < d {
		t.Errorf("the OS version (at %d) comes before Docker's packages (at %d)", v, d)
	}
}

func TestWriteContextNamesEveryFileAfterTheProject(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := WriteContext(dir, params); err != nil {
		t.Fatal(err)
	}
	var got []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		got = append(got, filepath.ToSlash(rel))
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(rel, placeholder) || strings.Contains(string(raw), "{{") {
			t.Errorf("%s is not rendered", rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		exec := strings.HasPrefix(rel, filepath.Join("files", "usr", "sbin"))
		if exec != (info.Mode().Perm() == 0o755) {
			t.Errorf("%s: mode %v", rel, info.Mode())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"files/usr/sbin/notes-preload", "files/etc/systemd/system/notes-preload.service",
		"files/etc/systemd/system/notes-expand.service", "files/system/oem/01_notes_expand.yaml", "files/etc/sudoers.d/10-notes"} {
		if !slices.Contains(got, want) {
			t.Errorf("context lacks %s (has %v)", want, got)
		}
	}
	unit, err := os.ReadFile(filepath.Join(dir, "files/etc/systemd/system/notes-expand.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "run-stage notes.expand") {
		t.Errorf("expand unit = %s", unit)
	}
	gate, err := os.ReadFile(filepath.Join(dir, "files/etc/systemd/system/notes-health.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gate), "ExecStart=/usr/sbin/notes-stow health-gate") || !strings.Contains(string(gate), "After=notes.service") {
		t.Errorf("health unit = %s", gate)
	}
	// The network override is rendered before networkd starts.
	nw, err := os.ReadFile(filepath.Join(dir, "files/etc/systemd/system/notes-network.service"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ExecStart=/usr/sbin/notes-stow network-boot\n", "Before=network-pre.target systemd-networkd.service\n", "DefaultDependencies=no\n"} {
		if !strings.Contains(string(nw), want) {
			t.Errorf("network unit lacks %q:\n%s", want, nw)
		}
	}
	// The tuning runs before the network and Docker, after the kernel's own settings.
	tu, err := os.ReadFile(filepath.Join(dir, "files/etc/systemd/system/notes-tune.service"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ExecStart=/usr/sbin/notes-stow tune-boot\n", "Before=network-pre.target systemd-networkd.service docker.service\n", "systemd-sysctl.service", "DefaultDependencies=no\n"} {
		if !strings.Contains(string(tu), want) {
			t.Errorf("tune unit lacks %q:\n%s", want, tu)
		}
	}
	// Again once the network is up (its MTU), before Docker reads its file.
	tuo, err := os.ReadFile(filepath.Join(dir, "files/etc/systemd/system/notes-tune-online.service"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ExecStart=/usr/sbin/notes-stow tune-boot\n", "After=network-online.target notes-tune.service\n", "Before=docker.service\n"} {
		if !strings.Contains(string(tuo), want) {
			t.Errorf("tune-online unit lacks %q:\n%s", want, tuo)
		}
	}
	// Azure's virtual function stays networkd's to leave alone, on Hyper-V alone.
	vf, err := os.ReadFile(filepath.Join(dir, "files/etc/systemd/network/00-notes-sriov.network"))
	if err != nil || !strings.Contains(string(vf), "Virtualization=microsoft\nDriver=mlx4_en mlx4_core mlx5_core mana\n") || !strings.Contains(string(vf), "Unmanaged=yes") {
		t.Errorf("the VF's network file: %v\n%s", err, vf)
	}
	// The scheduled backup: the host daemon runs it, when one is configured.
	for unit, wants := range map[string][]string{
		"notes-backup.service": {"ExecStart=/usr/sbin/notes-stow backup run --if-configured\n", "Type=oneshot\n"},
		"notes-backup.timer":   {"OnCalendar=*-*-* 03:00:00 UTC\n", "Persistent=true\n", "RandomizedDelaySec=15min\n"},
	} {
		raw, err := os.ReadFile(filepath.Join(dir, "files/etc/systemd/system", unit))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range wants {
			if !strings.Contains(string(raw), want) {
				t.Errorf("%s lacks %q:\n%s", unit, want, raw)
			}
		}
	}
	// The host daemon starts the timer: nothing enables it.
	if df, _ := Dockerfile(params); strings.Contains(string(df), "notes-backup") {
		t.Error("the bundle enables the backup timer")
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "files/etc/systemd/system/notes-backup.timer")); strings.Contains(string(raw), "[Install]") {
		t.Error("the backup timer has an [Install] section")
	}
	// Kairos's boot assessment with every step but the one that marks each boot good at once.
	grub, err := os.ReadFile(filepath.Join(dir, "files/usr/share/notes/08_grub.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Create upgrade failure sentinel", "enable_boot_assessment=yes", `set default="fallback"`,
		"save_env -f \"(${boot_assessment_blk})${boot_assessment}\" boot_assessment_tentative", "notes-stow health-gate", `if [ -z "${selected_entry}" -o "${selected_entry}" = "cos" ]; then`,
		"grub_boot_assessment\"\n        owner: 0\n        group: 0\n        permissions: 0600", "/tmp/mnt/STATE/boot_assessment set boot_assessment_tentative=\n"} {
		if !strings.Contains(string(grub), want) {
			t.Errorf("08_grub.yaml lacks %q", want)
		}
	}
	if strings.Contains(string(grub), "Remove GRUB sentinels") || strings.Contains(string(grub), "cos-state/boot_assessment set") {
		t.Error("08_grub.yaml still marks every boot good")
	}
	if st, err := os.Stat(filepath.Join(dir, PreloadDir)); err != nil || !st.IsDir() {
		t.Errorf("no preload directory: %v", err)
	}
}

func TestRecipesFollowTheirSources(t *testing.T) {
	b, d := BundleRecipe(), DiskRecipe()
	for _, c := range []struct {
		name string
		v    *string
		os   bool
	}{{"Dockerfile", &dockerfile, true}, {"disktools", &disktools, false}, {"ovf", &ovf, false}} {
		saved := *c.v
		*c.v += "\n# changed"
		nb, nd := BundleRecipe(), DiskRecipe()
		*c.v = saved
		if (nb != b) != c.os || (nd != d) == c.os {
			t.Errorf("%s: bundle changed %v, disks changed %v", c.name, nb != b, nd != d)
		}
	}
}

func TestDockerConfigCarriesTheAccount(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		auth authn.AuthConfig
		want map[string]string
	}{
		{authn.AuthConfig{Username: "pub", Password: "s3cret"}, map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte("pub:s3cret"))}},
		{authn.AuthConfig{Auth: "abc"}, map[string]string{"auth": "abc"}},
		{authn.AuthConfig{IdentityToken: "tok", RegistryToken: "reg"}, map[string]string{"identitytoken": "tok", "registrytoken": "reg"}},
		{authn.AuthConfig{}, nil},
	} {
		p := filepath.Join(t.TempDir(), "config.json")
		if err := writeDockerConfig(p, "registry.example.org", c.auth); err != nil {
			t.Fatal(err)
		}
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
			t.Errorf("mode %v", st.Mode())
		}
		raw, _ := os.ReadFile(p)
		var got struct {
			Auths map[string]map[string]string `json:"auths"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if !maps.Equal(got.Auths["registry.example.org"], c.want) || (c.want == nil && len(got.Auths) != 0) {
			t.Errorf("%+v: config = %s", c.auth, raw)
		}
	}
}

func TestOVAIsTheDescriptorThenTheDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	vmdk := filepath.Join(dir, "notes-1.0.0-disk1.vmdk")
	if err := os.WriteFile(vmdk, []byte("disk bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	ova := filepath.Join(dir, "notes-1.0.0.ova")
	if err := writeOVA(ova, "notes-1.0.0", vmdk, OVADiskBytes); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(ova)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var names []string
	var desc, disk []byte
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		b, _ := io.ReadAll(tr)
		if len(names) == 1 {
			desc = b
		} else {
			disk = b
		}
	}
	if !slices.Equal(names, []string{"notes-1.0.0.ovf", "notes-1.0.0-disk1.vmdk"}) {
		t.Fatalf("entries = %v", names)
	}
	for _, want := range []string{`ovf:href="notes-1.0.0-disk1.vmdk"`, `ovf:size="10"`, `ovf:capacity="68719476736"`, "<Name>notes-1.0.0</Name>", `vmw:value="efi"`} {
		if !strings.Contains(string(desc), want) {
			t.Errorf("descriptor lacks %s", want)
		}
	}
	if string(disk) != "disk bytes" {
		t.Errorf("disk = %q", disk)
	}
	if err := writeOVA(ova, "notes-1.0.0", vmdk, OVADiskBytes); err == nil {
		t.Error("an existing OVA was overwritten")
	}
}

// fakeDocker is a docker CLI that plays auroraboot (writing the disk its flag names into the
// state volume, or nothing when NO_DISK names the flag) and runs the disk scripts with a fake
// qemu-img that writes its last argument. Every call is logged.
const fakeDocker = `#!/bin/sh
echo "$*" >> "$FAKE_LOG"
vol() { echo "$1" | sed -n "s|.*--volume \([^ ]*\):/$2[ :].*|\1|p"; }
case "$*" in
*"--entrypoint chown"*|buildx*) exit 0 ;;
*auroraboot*)
	state=$(vol "$*" state)
	for a; do last=$a; done
	case "$last" in "$NO_DISK=true") exit 0 ;; esac
	case "$last" in
	disk.efi=true) echo "$*" | grep -o 'disk.state_size=[0-9]*' >> "$FAKE_LOG.state"
		raw=$FAKE_RAW; [ -z "$FAKE_RAW2" ] || [ ! -e "$state/../.second" ] || raw=$FAKE_RAW2; touch "$state/../.second"
		cp "$raw" "$state/kairos-x.raw"; [ -z "$RAW_SIZE" ] || truncate -s "$RAW_SIZE" "$state/kairos-x.raw" ;;
	esac ;;
*stowage-disktools*)
	work=$(vol "$*" work)
	for a; do last=$a; done
	cd "$work" && PATH="$FAKE_BIN:$PATH" sh -c "$last" ;;
*) echo "unexpected docker $*" >&2; exit 1 ;;
esac
`

// fakeQemu writes its last argument; a vpc one from a raw disk of FAKE_VHD_SIZE bytes is a fixed
// VHD of that size (its footer FAKE_VHD_FOOTER), unless BAD_VHD is set.
const fakeQemu = `#!/bin/sh
for a; do prev=$last; last=$a; done
case "$*:$BAD_VHD:$(stat -c %s "$prev" 2>/dev/null)" in
*vpc*::"$FAKE_VHD_SIZE")
	truncate -s "$FAKE_VHD_SIZE" "$last"
	cat "$FAKE_VHD_FOOTER" >> "$last" ;;
*) echo converted > "$last" ;;
esac
`

// vhdFooter is a fixed VHD's footer for a disk of size bytes, as checkVHD reads one.
func vhdFooter(size int64) []byte {
	f := make([]byte, 512)
	copy(f, "conectix")
	binary.BigEndian.PutUint64(f[40:], uint64(size))
	binary.BigEndian.PutUint64(f[48:], uint64(size))
	binary.BigEndian.PutUint32(f[60:], 2)
	var sum uint32
	for i, c := range f {
		if i < 64 || i >= 68 {
			sum += uint32(c)
		}
	}
	binary.BigEndian.PutUint32(f[64:], ^sum)
	return f
}

func fakeBuilder(t *testing.T) (*DiskBuilder, string) {
	bin := t.TempDir()
	for name, body := range map[string]string{"docker": fakeDocker, "qemu-img": fakeQemu, "sgdisk": "#!/bin/sh\n"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(t.TempDir(), "log")
	t.Setenv("FAKE_LOG", log)
	// A raw disk with a recovery partition of 200 MiB (sparse), and the VHD footer of its size,
	// which the disk scripts' fake qemu-img writes.
	raw := filepath.Join(t.TempDir(), "gpt.raw")
	writeGPT(t, raw, 200)
	st, err := os.Stat(raw)
	if err != nil {
		t.Fatal(err)
	}
	// The VHD script grows the raw disk to a whole MiB first.
	vhd := (st.Size() + 1<<20 - 1) &^ (1<<20 - 1)
	footer := filepath.Join(t.TempDir(), "footer")
	if err := os.WriteFile(footer, vhdFooter(vhd), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_RAW", raw)
	t.Setenv("FAKE_VHD_SIZE", strconv.FormatInt(vhd, 10))
	t.Setenv("FAKE_VHD_FOOTER", footer)
	t.Setenv("FAKE_BIN", bin)
	saved := OnDisk
	OnDisk = func(string) error { return nil }
	t.Cleanup(func() { OnDisk = saved })
	return &DiskBuilder{Docker: filepath.Join(bin, "docker"), WorkDir: t.TempDir(), Stderr: io.Discard}, log
}

func TestDiskBuilderMakesEachFormat(t *testing.T) {
	for _, c := range []struct {
		formats []string
		runs    []string // auroraboot flags, in order
	}{
		{[]string{"qcow2"}, []string{"disk.efi"}},
		{[]string{"raw", "qcow2", "ova", "vhd", "gce"}, []string{"disk.efi"}},
		{[]string{"vhd"}, []string{"disk.efi"}},
		{[]string{"gce"}, []string{"disk.efi"}},
	} {
		t.Run(strings.Join(c.formats, ","), func(t *testing.T) {
			b, log := fakeBuilder(t)
			r := DiskRequest{Source: "127.0.0.1:5000/notes-os@sha256:" + strings.Repeat("c", 64), Registry: "127.0.0.1:5000", RootfsMiB: 3500,
				Auth: authn.AuthConfig{Username: "u", Password: "p"}, Formats: c.formats, Base: "notes-1.0.0"}
			made, cleanup, err := b.Build(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			out := made.Files
			if !slices.Equal(slices.Sorted(maps.Keys(out)), slices.Sorted(slices.Values(c.formats))) {
				t.Errorf("files = %v", out)
			}
			for f, p := range out {
				if filepath.Base(p) != "notes-1.0.0."+Ext[f] {
					t.Errorf("%s: %s", f, p)
				}
				if _, err := os.Stat(p); err != nil {
					t.Errorf("%s: %v", f, err)
				}
			}
			dir := filepath.Dir(out[c.formats[0]])
			if left, _ := os.ReadDir(dir); len(left) != len(c.formats) {
				t.Errorf("the output holds more than the disks: %v", left)
			}
			calls, _ := os.ReadFile(log)
			var runs []string
			for _, l := range strings.Split(string(calls), "\n") {
				if strings.Contains(l, Auroraboot+" --set") {
					if !strings.Contains(l, "container_image=oci://"+r.Source) || !strings.Contains(l, "--privileged") || !strings.Contains(l, fmt.Sprintf("--set disk.state_size=%d ", StateMiB(3500))) {
						t.Errorf("auroraboot call: %s", l)
					}
					runs = append(runs, strings.TrimSuffix(l[strings.LastIndex(l, " ")+1:], "=true"))
				}
			}
			if !slices.Equal(runs, c.runs) {
				t.Errorf("auroraboot runs = %v, want %v", runs, c.runs)
			}
			cleanup()
			if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("cleanup left %s", dir)
			}
		})
	}
}

// auroraboot can exit 0 without writing its disk: that is a failure, and nothing is left behind.
func TestDiskBuilderRefusesAMissingDisk(t *testing.T) {
	b, _ := fakeBuilder(t)
	t.Setenv("NO_DISK", "disk.efi")
	_, cleanup, err := b.Build(context.Background(), DiskRequest{Source: "r/notes-os@sha256:" + strings.Repeat("c", 64), Registry: "r", RootfsMiB: 3500,
		Formats: []string{"qcow2", "vhd"}, Base: "notes-1.0.0"})
	if err == nil || !strings.Contains(err.Error(), "auroraboot disk.efi: expected one *.raw file in its output, found 0") {
		t.Fatalf("Build = %v", err)
	}
	cleanup()
	if left, _ := os.ReadDir(b.WorkDir); len(left) != 0 {
		t.Errorf("left %v", left)
	}
}

// A VHD that Azure would not import fails the build, and nothing is left behind.
func TestDiskBuilderRefusesABadVHD(t *testing.T) {
	b, _ := fakeBuilder(t)
	t.Setenv("BAD_VHD", "1")
	_, cleanup, err := b.Build(context.Background(), DiskRequest{Source: "r/notes-os@sha256:" + strings.Repeat("c", 64), Registry: "r", RootfsMiB: 3500,
		Formats: []string{"vhd"}, Base: "notes-1.0.0"})
	if err == nil || !strings.Contains(err.Error(), "notes-1.0.0.vhd: 10 bytes, no VHD footer") {
		t.Fatalf("Build = %v", err)
	}
	cleanup()
	if left, _ := os.ReadDir(b.WorkDir); len(left) != 0 {
		t.Errorf("left %v", left)
	}
}

func TestDiskBuilderRefusesBadRequests(t *testing.T) {
	b, _ := fakeBuilder(t)
	for _, c := range []struct {
		r    DiskRequest
		want string
	}{
		{DiskRequest{Formats: []string{"vmdk"}, Base: "notes-1.0.0"}, `unknown disk format "vmdk"`},
		{DiskRequest{Formats: []string{"raw"}, Base: "notes 1.0.0; rm -rf /"}, `disk name "notes 1.0.0; rm -rf /"`},
		{DiskRequest{Formats: []string{"raw"}, Base: "-rf"}, `disk name "-rf"`},
	} {
		if _, _, err := b.Build(context.Background(), c.r); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v", c.r, err)
		}
	}
	OnDisk = refuseRAM
	if _, err := os.Stat("/dev/shm"); err == nil {
		b.WorkDir = "/dev/shm"
		if _, _, err := b.Build(context.Background(), DiskRequest{Formats: []string{"raw"}, Base: "n"}); err == nil || !errors.Is(err, errRAM) {
			t.Errorf("a work directory in memory: %v", err)
		}
	}
}

// The OVA's disk declares OVADiskBytes, or Kairos's partitions (the raw disk and the state) and the
// app's least space beyond them when that is more.
func TestOVANeverShrinksTheDisk(t *testing.T) {
	b, _ := fakeBuilder(t)
	t.Setenv("RAW_SIZE", fmt.Sprint(int64(OVADiskBytes)+(1<<30)))
	made, cleanup, err := b.Build(context.Background(), DiskRequest{Source: "r/notes-os@sha256:" + strings.Repeat("c", 64), Registry: "r", RootfsMiB: 3500,
		Formats: []string{"ova"}, Base: "notes-1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	raw, _ := os.ReadFile(made.Files["ova"])
	files := map[string]string{}
	tr := tar.NewReader(strings.NewReader(string(raw)))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		c, _ := io.ReadAll(tr)
		files[h.Name] = string(c)
	}
	// The raw disk of 65 GiB, Kairos's partitions (it and the state) and the app's least beyond.
	capacity := (int64(OVADiskBytes+1<<30)>>20 + StateMiB(3500) + ovaAppMiB) << 20
	if want := fmt.Sprintf(`ovf:capacity="%d"`, capacity); !strings.Contains(files["notes-1.0.0.ovf"], want) {
		t.Errorf("descriptor lacks %s:\n%s", want, files["notes-1.0.0.ovf"])
	}
}

// A disk too large for USTAR is written with GNU's size extension.
func TestOVAOverUSTARsLimit(t *testing.T) {
	saved := ustarMax
	ustarMax = 4
	t.Cleanup(func() { ustarMax = saved })
	dir := t.TempDir()
	vmdk := filepath.Join(dir, "n-disk1.vmdk")
	if err := os.WriteFile(vmdk, []byte("disk bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeOVA(filepath.Join(dir, "n.ova"), "n", vmdk, OVADiskBytes); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(filepath.Join(dir, "n.ova"))
	defer f.Close()
	tr := tar.NewReader(f)
	formats := []tar.Format{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		formats = append(formats, h.Format)
	}
	if len(formats) != 2 || formats[0]&tar.FormatUSTAR == 0 || formats[1]&tar.FormatGNU == 0 {
		t.Errorf("formats = %v", formats)
	}
}

// Every format has a file extension.
func TestEveryFormatHasAnExtension(t *testing.T) {
	t.Parallel()
	for _, f := range spec.DiskFormats {
		if Ext[f] == "" {
			t.Errorf("format %s has no extension", f)
		}
	}
	if len(Ext) != len(spec.DiskFormats) {
		t.Errorf("Ext names formats spec does not: %v", Ext)
	}
}

func TestDockerHubConfigKey(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := writeDockerConfig(p, "index.docker.io", authn.AuthConfig{Username: "u", Password: "p"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), `"https://index.docker.io/v1/"`) {
		t.Errorf("config = %s", raw)
	}
}

func TestDiskRecipeCoversAuroraboot(t *testing.T) {
	d := DiskRecipe()
	saved := auroraFlags
	auroraFlags = append([]string{"another=true"}, saved...)
	t.Cleanup(func() { auroraFlags = saved })
	if DiskRecipe() == d {
		t.Error("auroraboot's flags are not in the disk recipe")
	}
}

// An empty field renders an empty path (missingkey=error sees only a missing key): refused.
func TestParamsWithoutTheInstallerAreRefused(t *testing.T) {
	p := params
	p.Ctl = ""
	if _, err := Dockerfile(p); err == nil {
		t.Fatal("Dockerfile rendered without the installer's name")
	}
	if err := WriteContext(t.TempDir(), p); err == nil {
		t.Fatal("WriteContext wrote without the installer's name")
	}
}

// The state partition holds three system images and a quarter more; first estimated from the
// bundle, an image half again its unpacked size.
func TestTheStateIsSizedFromTheImages(t *testing.T) {
	t.Parallel()
	// Images of 3700 MiB: 3 * 3700 * 5/4 + 100.
	if got := stateFor(3700); got != 13975 {
		t.Errorf("state %d", got)
	}
	// A bundle of 3500 MiB: an image estimated at 3500 * 3/2 + 200.
	if got := StateMiB(3500); got != stateFor(5450) {
		t.Errorf("first state %d", got)
	}
	if stateFor(3701) <= stateFor(3700) {
		t.Error("not monotonic")
	}
}

// The project's modules reach modprobe's and modules-load's files in the image's /usr, and the
// build checks them; without any, the build has no such step.
func TestTheProjectsModulesAreInTheBundle(t *testing.T) {
	t.Parallel()
	p := params
	p.Modules = map[string]spec.KernelModule{
		"irdma":   {Block: true, Reason: "the port's queues"},
		"tcp_bbr": {Load: true, Reason: "the service's congestion control"},
		"ice":     {Load: true, Options: map[string]string{"b": "1", "a": "x,y"}, Reason: "the NIC's settings"},
	}
	dir := t.TempDir()
	if err := WriteContext(dir, p); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		raw, err := os.ReadFile(filepath.Join(dir, "files", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	if got, want := read("usr/lib/modprobe.d/50-notes.conf"), "# notes's kernel modules (shapes.kairos.modules): those blocked, and their options. In the image's\n# /usr, not /etc, which Kairos keeps across upgrades and rollbacks.\n# the NIC's settings\noptions ice a=x,y b=1\n# the port's queues\nblacklist irdma\ninstall irdma /bin/false\n"; got != want {
		t.Errorf("modprobe.d:\n%s\nwant\n%s", got, want)
	}
	if got, want := read("usr/lib/modules-load.d/50-notes.conf"), "# notes's kernel modules (shapes.kairos.modules) loaded at boot.\n# the NIC's settings\nice\n# the service's congestion control\ntcp_bbr\n"; got != want {
		t.Errorf("modules-load.d:\n%s\nwant\n%s", got, want)
	}
	df, err := Dockerfile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(df)
	for _, want := range []string{
		"        modinfo -k \"$v\" ice >/dev/null || {", "grep -qx a || {", "grep -qx b || {",
		"[ \"$(modinfo -k \"$v\" -F filename irdma 2>/dev/null)\" != \"(builtin)\" ]", "[ \"$(modinfo -k \"$v\" -F filename ice 2>/dev/null)\" != \"(builtin)\" ]",
		"grep -qxF 'install irdma /bin/false'", "grep -qxF 'options ice a=x,y b=1'", "cat-config modules-load.d | grep -qx 'tcp_bbr'",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
	if strings.Contains(s, "modinfo -k \"$v\" irdma >/dev/null") || !strings.Contains(s, "-F filename tcp_bbr 2>/dev/null)\" != \"(builtin)\"") {
		t.Error("a blocked module need not exist; a loaded one may not be built in")
	}
	if plain, _ := Dockerfile(params); strings.Contains(string(plain), "shapes.kairos.modules") {
		t.Error("a bundle without modules checks them")
	}
	for _, bad := range []map[string]spec.KernelModule{
		{"a b": {Load: true, Reason: "r"}},
		{"ice": {Load: true, Reason: "two\nlines"}},
		{"ice": {Options: map[string]string{"a": "x y"}, Reason: "r"}},
		{"ice": {Options: map[string]string{"a'": "x"}, Reason: "r"}},
	} {
		p.Modules = bad
		if _, err := Dockerfile(p); err == nil {
			t.Errorf("rendered %v", bad)
		}
	}
}

// The bundle's build and its 08_grub.yaml name the release lint holds the project to.
func TestTheBundleNamesItsKairosInitRelease(t *testing.T) {
	t.Parallel()
	grub, err := files.ReadFile("files/usr/share/NAME/08_grub.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dockerfile, "use kairos-init "+KairosInitRelease+"\"") || !strings.Contains(string(grub), "(kairos-init "+KairosInitRelease+")") {
		t.Errorf("the Dockerfile or 08_grub.yaml names another release than %s", KairosInitRelease)
	}
}

// writeGPT writes a raw disk holding a GPT with a recovery partition of mib, as auroraboot writes
// one, which sizes the state partition; the file spans its partitions (sparse).
func writeGPT(t *testing.T, path string, mib int64) {
	t.Helper()
	writeGPTSectors(t, path, 2048, 64<<11, mib<<11, (2048+(128+mib)<<11+34)*512) // and the backup GPT
}

// patchGPT writes b at off in the raw disk at path, in place (the file stays sparse), and sets
// the GPT's CRC32s to what it then holds; rawPatch leaves them.
func patchGPT(t *testing.T, path string, off int64, b []byte) {
	t.Helper()
	rawPatch(t, path, off, b)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	hdr := make([]byte, 512)
	if _, err := f.ReadAt(hdr, 512); err != nil {
		t.Fatal(err)
	}
	// The entries' CRC when the header names entries the fixture can read.
	lba, n, size := binary.LittleEndian.Uint64(hdr[72:]), binary.LittleEndian.Uint32(hdr[80:]), binary.LittleEndian.Uint32(hdr[84:])
	if lba < 1<<20 && n <= 1<<12 && size <= 1<<12 {
		entries := make([]byte, n*size)
		if _, err := f.ReadAt(entries, int64(lba)*512); err == nil {
			binary.LittleEndian.PutUint32(hdr[88:], crc32.ChecksumIEEE(entries))
		}
	}
	clear(hdr[16:20])
	hsize := min(binary.LittleEndian.Uint32(hdr[12:]), 512)
	binary.LittleEndian.PutUint32(hdr[16:], crc32.ChecksumIEEE(hdr[:hsize]))
	if _, err := f.WriteAt(hdr, 512); err != nil {
		t.Fatal(err)
	}
}

// rawPatch writes b at off in the raw disk at path, in place.
func rawPatch(t *testing.T, path string, off int64, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(b, off); err != nil {
		t.Fatal(err)
	}
}

// writeGPTSectors writes a raw disk of size bytes, its GPT's EFI and OEM partitions of boot sectors
// each, from LBA start, and its recovery partition of recovery sectors after them.
func writeGPTSectors(t *testing.T, path string, start, boot, recovery, size int64) {
	t.Helper()
	b := make([]byte, 512*3)
	copy(b[512:], "EFI PART")
	binary.LittleEndian.PutUint32(b[512+12:], 92)                  // the header's size
	binary.LittleEndian.PutUint64(b[512+40:], 34)                  // the first usable LBA
	binary.LittleEndian.PutUint64(b[512+48:], uint64(size/512-34)) // the last, before the backup GPT
	binary.LittleEndian.PutUint64(b[512+72:], 2)                   // the entries at LBA 2
	binary.LittleEndian.PutUint32(b[512+80:], 4)                   // four of them
	binary.LittleEndian.PutUint32(b[512+84:], 128)                 // of 128 bytes
	put := func(i int, first, sectors int64, name string) {
		e := b[1024+i*128:]
		e[0] = 1 // a type: not an empty entry
		binary.LittleEndian.PutUint64(e[32:], uint64(first))
		binary.LittleEndian.PutUint64(e[40:], uint64(first+sectors-1))
		for j, c := range name {
			e[56+2*j] = byte(c)
		}
	}
	put(0, start, boot, "efi")
	put(1, start+boot, boot, "oem")
	put(2, start+2*boot, recovery, "recovery")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
	patchGPT(t, path, 512, []byte("EFI PART")) // the CRC32s
}

// The recovery partition auroraboot made sizes the state one: a first estimate too small makes the
// raw disk again, once, with the state its images call for; the disks record what Kairos takes.
func TestTheStateFollowsTheRecoveryImage(t *testing.T) {
	b, log := fakeBuilder(t)
	big := filepath.Join(t.TempDir(), "big.raw")
	writeGPT(t, big, 20000) // images of (20000 - 150) / 2: more than the first estimate
	t.Setenv("FAKE_RAW", big)
	d, cleanup, err := b.Build(context.Background(), DiskRequest{Source: "r/notes-os@sha256:" + strings.Repeat("c", 64), Registry: "r", RootfsMiB: 3500, Formats: []string{"raw"}, Base: "notes-1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	got, _ := os.ReadFile(log + ".state")
	// Images of (20000 - 150) / 2 = 9925 MiB: 3 * 9925 * 5/4, rounded up, and 100.
	want := fmt.Sprintf("disk.state_size=%d\ndisk.state_size=%d\n", StateMiB(3500), 37319)
	rawMiB := (int64(2048+(128+20000)<<11+34)*512 + 1<<20 - 1) >> 20
	if string(got) != want {
		t.Errorf("auroraboot runs:\n%s\nwant\n%s", got, want)
	}
	if d.OSMiB != rawMiB+37319 { // the raw disk, which spans its partitions, and the state
		t.Errorf("os %d MiB", d.OSMiB)
	}
	// A first estimate large enough makes it once.
	small := filepath.Join(t.TempDir(), "small.raw")
	writeGPT(t, small, 1000)
	t.Setenv("FAKE_RAW", small)
	_ = os.Remove(log + ".state")
	d, cleanup2, err := b.Build(context.Background(), DiskRequest{Source: "r/notes-os@sha256:" + strings.Repeat("c", 64), Registry: "r", RootfsMiB: 3500, Formats: []string{"raw"}, Base: "notes-1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup2()
	if got, _ := os.ReadFile(log + ".state"); string(got) != fmt.Sprintf("disk.state_size=%d\n", StateMiB(3500)) || d.OSMiB != 1+128+1000+1+StateMiB(3500) { // the raw disk, its backup GPT a MiB more rounded up
		t.Errorf("one run: %s, os %d", got, d.OSMiB)
	}
}

// A second run whose recovery partition grew again is refused: no third run.
func TestTheStateIsCheckedAgainAfterItsRebuild(t *testing.T) {
	b, _ := fakeBuilder(t)
	first, second := filepath.Join(t.TempDir(), "1.raw"), filepath.Join(t.TempDir(), "2.raw")
	writeGPT(t, first, 20000)
	writeGPT(t, second, 30000)
	t.Setenv("FAKE_RAW", first)
	t.Setenv("FAKE_RAW2", second)
	_, cleanup, err := b.Build(context.Background(), DiskRequest{Source: "r/notes-os@sha256:" + strings.Repeat("c", 64), Registry: "r", RootfsMiB: 3500, Formats: []string{"raw"}, Base: "notes-1.0.0"})
	cleanup()
	if err == nil || !strings.Contains(err.Error(), "made the recovery partition larger again (30000 MiB)") {
		t.Errorf("%v", err)
	}
}

func TestRawLayoutRefusesWhatIsNoGPT(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "x.raw")
	if err := os.WriteFile(p, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rawLayout(p); err == nil || !strings.Contains(err.Error(), "no GPT") {
		t.Errorf("%v", err)
	}
	refused := func(what, want string, patch func()) {
		t.Helper()
		writeGPT(t, p, 1000)
		if patch != nil {
			patch()
		}
		if _, _, err := rawLayout(p); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", what, err)
		}
	}
	u64 := func(v uint64) []byte { b := make([]byte, 8); binary.LittleEndian.PutUint64(b, v); return b }
	// The entries' LBA: 2 is the GPT's; 1 and past the disk are not.
	writeGPT(t, p, 1000)
	if _, _, err := rawLayout(p); err != nil {
		t.Errorf("valid: %v", err)
	}
	refused("entries at LBA 1", "entries at LBA 1", func() { patchGPT(t, p, 512+72, u64(1)) })
	refused("entries past the disk", "not within", func() { patchGPT(t, p, 512+72, u64(1<<55)) })
	refused("entries wrapping past 2^64", "not within", func() { patchGPT(t, p, 512+72, u64(^uint64(0))) })
	refused("usable LBAs wrapping past 2^64", "usable LBAs", func() { patchGPT(t, p, 512+48, u64(^uint64(0)-1)) })
	refused("a header's CRC32 not its content's", "header's CRC32", func() { rawPatch(t, p, 512+16, []byte{1}) })
	refused("an entry changed under its CRC32", "entries' CRC32", func() { rawPatch(t, p, 1024+2*128+33, []byte{1}) })
	refused("a header of 91 bytes", "a GPT header of 91 bytes", func() { patchGPT(t, p, 512+12, []byte{91}) })
	refused("a header of 513 bytes", "a GPT header of 513 bytes", func() { patchGPT(t, p, 512+12, []byte{1, 2}) })
	writeGPT(t, p, 1000)
	patchGPT(t, p, 512+12, []byte{0, 2}) // 512 bytes: the CRC32 over all of them
	if rec, _, err := rawLayout(p); err != nil || rec != 1000 {
		t.Errorf("a header of 512 bytes: %d %v", rec, err)
	}
	// A GPT naming no recovery partition: another is never taken for it.
	refused("unnamed", "no partition named recovery", func() { patchGPT(t, p, 1024+2*128+56, make([]byte, 16)) })
	// The recovery partition within the usable LBAs: at the last one, then one past it, then over the GPT.
	last := uint64(2048 + 1128<<11)
	writeGPT(t, p, 1000)
	patchGPT(t, p, 1024+2*128+40, u64(last))
	if _, _, err := rawLayout(p); err != nil {
		t.Errorf("at the last usable LBA: %v", err)
	}
	refused("past the usable LBAs", "is not within its usable LBAs", func() { patchGPT(t, p, 1024+2*128+40, u64(last+1)) })
	refused("over the GPT", "is not within its usable LBAs", func() { patchGPT(t, p, 1024+2*128+32, u64(2)) })
	// The usable LBAs: after the primary entries (LBA 2, one sector), before the backup ones and
	// header (the disk's last two sectors here); each just within, then just past.
	sectors := uint64(2048 + 1128<<11 + 34)
	for _, c := range []struct {
		off   int64
		v     uint64
		valid bool
	}{
		{512 + 40, 3, true}, {512 + 40, 2, false},
		{512 + 48, sectors - 3, true}, {512 + 48, sectors - 2, false},
	} {
		writeGPT(t, p, 1000)
		patchGPT(t, p, c.off, u64(c.v))
		if _, _, err := rawLayout(p); (err == nil) != c.valid {
			t.Errorf("header byte %d = %d: %v", c.off, c.v, err)
		}
	}
	// The entries' count and size, each bound on both sides.
	u32 := func(v uint32) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, v); return b }
	for _, c := range []struct {
		off   int64
		v     uint32
		valid bool
	}{
		{512 + 80, 1, true}, {512 + 80, 0, false}, {512 + 80, 1024, true}, {512 + 80, 1025, false},
		{512 + 84, 128, true}, {512 + 84, 127, false}, {512 + 84, 4096, true}, {512 + 84, 4097, false},
	} {
		writeGPT(t, p, 1000)
		patchGPT(t, p, c.off, u32(c.v))
		_, _, err := rawLayout(p)
		if gotValid := err == nil || !strings.Contains(err.Error(), "entries of"); gotValid != c.valid {
			t.Errorf("header byte %d = %d: %v", c.off, c.v, err)
		}
	}
	// An entry of a zero type GUID is unused; a name past ASCII is no "recovery".
	refused("a zero type GUID", "no partition named recovery", func() { patchGPT(t, p, 1024+2*128, make([]byte, 16)) })
	refused("a name past ASCII", "no partition named recovery", func() { patchGPT(t, p, 1024+2*128+56+1, []byte{1}) })
	// Two named recovery.
	refused("two", "two partitions named recovery", func() {
		b := make([]byte, 128)
		f, _ := os.Open(p)
		_, _ = f.ReadAt(b, 1024+2*128)
		f.Close()
		patchGPT(t, p, 1024+3*128, b)
	})
	// One too small to hold an image: 150 MiB and one, then 150.
	for mib, ok := range map[int64]bool{recoveryExtraMiB + 1: true, recoveryExtraMiB: false} {
		writeGPT(t, p, mib)
		if _, _, err := rawLayout(p); (err == nil) != ok {
			t.Errorf("a recovery partition of %d MiB: %v", mib, err)
		}
	}
}

// The recovery partition is the one named so, though another be larger.
func TestRawLayoutTakesTheRecoveryByName(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "x.raw")
	writeGPTSectors(t, p, 2048, 64<<11, 200<<11, 2000<<20) // a recovery partition of 200 MiB
	// A larger partition beside it (1000 MiB), named otherwise.
	e := make([]byte, 128)
	e[0] = 1
	binary.LittleEndian.PutUint64(e[32:], 400<<11)
	binary.LittleEndian.PutUint64(e[40:], 1400<<11-1)
	copy(e[56:], "s\x00t\x00a\x00t\x00e\x00")
	patchGPT(t, p, 1024+3*128, e)
	if rec, _, err := rawLayout(p); err != nil || rec != 200 {
		t.Errorf("recovery %d MiB, %v", rec, err)
	}
}
