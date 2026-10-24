package kairos

import (
	"archive/tar"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
	disk.efi=true) echo raw > "$state/kairos-x.raw"; [ -z "$RAW_SIZE" ] || truncate -s "$RAW_SIZE" "$state/kairos-x.raw" ;;
	esac ;;
*stowage-disktools*)
	work=$(vol "$*" work)
	for a; do last=$a; done
	cd "$work" && PATH="$FAKE_BIN:$PATH" sh -c "$last" ;;
*) echo "unexpected docker $*" >&2; exit 1 ;;
esac
`

// fakeQemu writes its last argument; a vpc one from a raw disk of 1 MiB, the size the script
// grows the fake raw disk to, is a fixed VHD of that size, unless BAD_VHD is set.
const fakeQemu = `#!/bin/sh
for a; do prev=$last; last=$a; done
case "$*:$BAD_VHD:$(stat -c %s "$prev" 2>/dev/null)" in
*vpc*::1048576)
	truncate -s 1M "$last"
	{ printf conectix; head -c 40 /dev/zero; printf '\000\000\000\000\000\020\000\000'
	  printf '\000\000\000\000\000\000\000\002\377\377\374\220'; head -c 444 /dev/zero; } >> "$last" ;;
*) echo converted > "$last" ;;
esac
`

func fakeBuilder(t *testing.T) (*DiskBuilder, string) {
	bin := t.TempDir()
	for name, body := range map[string]string{"docker": fakeDocker, "qemu-img": fakeQemu, "sgdisk": "#!/bin/sh\n"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(t.TempDir(), "log")
	t.Setenv("FAKE_LOG", log)
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
			r := DiskRequest{Source: "127.0.0.1:5000/notes-os@sha256:" + strings.Repeat("c", 64), Registry: "127.0.0.1:5000",
				Auth: authn.AuthConfig{Username: "u", Password: "p"}, Formats: c.formats, Base: "notes-1.0.0"}
			out, cleanup, err := b.Build(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
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
					if !strings.Contains(l, "container_image=oci://"+r.Source) || !strings.Contains(l, "--privileged") {
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
	_, cleanup, err := b.Build(context.Background(), DiskRequest{Source: "r/notes-os@sha256:" + strings.Repeat("c", 64), Registry: "r",
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
	_, cleanup, err := b.Build(context.Background(), DiskRequest{Source: "r/notes-os@sha256:" + strings.Repeat("c", 64), Registry: "r",
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

// The OVA's disk grows to OVADiskBytes, and a raw disk already larger keeps its size.
func TestOVANeverShrinksTheDisk(t *testing.T) {
	b, _ := fakeBuilder(t)
	t.Setenv("RAW_SIZE", fmt.Sprint(int64(OVADiskBytes)+(1<<30)))
	out, cleanup, err := b.Build(context.Background(), DiskRequest{Source: "r/notes-os@sha256:" + strings.Repeat("c", 64), Registry: "r",
		Formats: []string{"ova"}, Base: "notes-1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	raw, _ := os.ReadFile(out["ova"])
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
	if want := fmt.Sprintf(`ovf:capacity="%d"`, int64(OVADiskBytes)+(1<<30)); !strings.Contains(files["notes-1.0.0.ovf"], want) {
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
