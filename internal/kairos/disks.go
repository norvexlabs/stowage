package kairos

import (
	"archive/tar"
	"cmp"
	"compress/gzip"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/norvexlabs/stowage/internal/spec"
)

// Auroraboot is the image that makes the raw disk from the bundle; the other formats are made
// from that disk.
const Auroraboot = "quay.io/kairos/auroraboot:v0.28.0@sha256:9a3ae40ca2f4e5856055502ad72ff3b6343758b78a97968387ee3af0233892c2"

//go:embed disktools.Dockerfile
var disktools string

//go:embed template.ovf
var ovf string

var safeBase = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// Ext is each format's file extension.
var Ext = map[string]string{"raw": "raw", "qcow2": "qcow2", "ova": "ova", "vhd": "vhd", "gce": "gce.tar.gz", "iso": "iso"}

// The commands that make the disks. They are part of DiskRecipe: a change to one changes the
// disks it makes.
const (
	// qcow2Cmd converts the raw disk %[1]s.raw, compressed.
	qcow2Cmd = "qemu-img convert -f raw -O qcow2 -c %[1]s.raw %[1]s.qcow2"
	// ovaScript grows a copy of the raw disk %[1]s.raw to %[2]d bytes and converts it to the
	// streamOptimized VMDK %[3]s. A grown disk's backup GPT is moved to its new end (sgdisk -e), as
	// in the scripts below.
	ovaScript = "set -e; cp --sparse=always %[1]s.raw ova.raw; truncate -s %[2]d ova.raw; sgdisk -e ova.raw >/dev/null; " +
		"qemu-img convert -f raw -O vmdk -o subformat=streamOptimized ova.raw %[3]s; rm ova.raw"
	// vhdScript converts a copy of the raw disk %[1]s.raw, grown to a whole number of MiB, to the
	// fixed VHD Azure imports, its virtual size the disk's exactly.
	vhdScript = "set -e; cp --sparse=always %[1]s.raw vhd.raw; truncate -s %%1M vhd.raw; sgdisk -e vhd.raw >/dev/null; " +
		"qemu-img convert -f raw -O vpc -o subformat=fixed,force_size=on vhd.raw %[1]s.vhd; rm vhd.raw"
	// gceScript puts a copy of the raw disk %[1]s.raw, grown to a whole number of GiB, as disk.raw
	// in the sparse, oldgnu tar.gz Google Cloud imports.
	gceScript = "set -e; mkdir gce; cp --sparse=always %[1]s.raw gce/disk.raw; truncate -s %%1G gce/disk.raw; sgdisk -e gce/disk.raw >/dev/null; " +
		"tar --format=oldgnu --sparse --numeric-owner --owner=0 --group=0 -C gce -czf %[1]s.gce.tar.gz disk.raw; rm -r gce"
)

// isoConfig is the cloud-config the installer ISO embeds, merged with the machine's own (its
// cidata seed, which gives the install's device): the state partition of %d MiB the raw disk is
// laid out with (Kairos's installer sizes it to the images it writes, leaving a later release no
// room to grow), and the screen's console last, so that /dev/console, the boot's messages and the
// recovery shell are on a server's screen rather than its serial port.
const isoConfig = "#cloud-config\ninstall:\n  partitions:\n    state:\n      size: %d\n  grub_options:\n    extra_cmdline: console=tty1\n"

// auroraFlags are auroraboot's settings besides its state directory, source and disk.
var auroraFlags = []string{"disable_http_server=true", "disable_netboot=true", "no_default_cloud_config=true"}

// ustarLimit is the size from which a file does not fit USTAR, and ustarMax the one in force;
// tests lower it.
const ustarLimit = 8 << 30

var ustarMax int64 = ustarLimit

// gceUnit is the size Google Cloud's disk.raw is a whole number of (gceScript's %1G); tests lower it.
var gceUnit int64 = 1 << 30

// The partitions a disk's first boot lays out besides the persistent one: auroraboot's recovery
// partition (its image and a transition one during a recovery upgrade), sized by auroraboot from
// the bundle as it measures it (by walking its files, which no tar sum reproduces), and the state
// partition this build sizes itself — auroraboot's default, three times the recovery partition,
// leaves a large image's persistent partition nothing: the active, passive and upgrade-transition
// systems, each an image the size of the recovery one, plus a quarter for later releases to grow.
// A release that outgrows it cannot upgrade a VM made before it.
const (
	imageExtraMiB    = 200 // an image of the system: the bundle and this
	stateGrowth      = 4   // the state partition holds three images and a quarter more: 3 * 5/4
	stateExtraMiB    = 100
	recoveryExtraMiB = 150 // auroraboot's recovery partition: two images and this
	estimateNum      = 3   // the first estimate of an image: the bundle's unpacked size * 3/2
	estimateDen      = 2
)

// sizingVersion is the partitions' sizing, for the disks' recipe: raised by a change to how a disk
// is sized that its terms below do not show (a formula, a rounding), so the disks are made again.
const sizingVersion = 1

// sizing is the partitions' sizing, for the disks' recipe: a change to a term, the version, or a
// formula's values makes new disks.
var sizing = fmt.Sprint(sizingVersion, imageExtraMiB, stateGrowth, stateExtraMiB, recoveryExtraMiB, estimateNum, estimateDen, ovaAppMiB, int64(OVADiskBytes),
	stateFor(1), stateFor(10000), StateMiB(1), StateMiB(10000), imageOf(1000), imageOf(10001), OVACapacityMiB(1), OVACapacityMiB(1<<20))

// imageOf is the system image a recovery partition of recoveryMiB holds two of, rounded up.
func imageOf(recoveryMiB int64) int64 { return (recoveryMiB - recoveryExtraMiB + 1) / 2 }

// stateFor is the state partition holding three system images of imageMiB, and a quarter more.
func stateFor(imageMiB int64) int64 {
	return (3*imageMiB*(stateGrowth+1)+stateGrowth-1)/stateGrowth + stateExtraMiB
}

// StateMiB is the state partition first given for a bundle of rootfsMiB unpacked: its image
// estimated larger than the bundle (auroraboot's measure of it runs larger than the tar's), the
// disk made again once with the size its recovery image calls for if that is larger.
func StateMiB(rootfsMiB int64) int64 {
	return stateFor(rootfsMiB*estimateNum/estimateDen + imageExtraMiB)
}

// ovaAppMiB is the least the app gets of an OVA's disk, beyond Kairos's partitions: the capacity
// grows past OVADiskBytes for a bundle that would leave less.
const ovaAppMiB = 8 << 10

// OVACapacityMiB is the capacity an OVA's disk declares, Kairos taking osMiB of it: OVADiskBytes,
// or Kairos's partitions and ovaAppMiB beyond them when that is more. No platform shrinks it.
func OVACapacityMiB(osMiB int64) int64 { return max(int64(OVADiskBytes)>>20, osMiB+ovaAppMiB) }

// OVADiskBytes is the capacity of the OVA's disk: VMware and VirtualBox do not grow an imported
// disk, so it is declared large and sparse; the persistent partition grows into it at first boot.
const OVADiskBytes = 64 << 30

// DiskRequest is the disks of one bundle.
type DiskRequest struct {
	// Source is the bundle in the registry, by digest; Registry is its host, and Auth the account
	// auroraboot pulls it with.
	Source, Registry string
	Auth             authn.AuthConfig
	Formats          []string
	// Base names the files: <Base>.<ext>.
	Base string
	// RootfsMiB is the bundle's unpacked size, which sizes the state partition (StateMiB).
	RootfsMiB int64
}

// Disks are the disks a build made: each format's file, and the MiB Kairos takes of a disk a VM
// boots one on besides the persistent partition (the raw disk's partitions and the state one).
type Disks struct {
	Files map[string]string
	OSMiB int64
}

// recoveryName is the recovery partition's name in a Kairos disk's GPT.
const recoveryName = "recovery"

// rawLayout reads a raw disk's GPT: its recovery partition (the one named so: another is never
// taken for it), and the disk's own size, both in MiB rounded up.
func rawLayout(path string) (recoveryMiB, diskMiB int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	hdr := make([]byte, 512)
	if _, err := f.ReadAt(hdr, 512); err != nil {
		return 0, 0, fmt.Errorf("%s: its GPT header: %w", path, err)
	}
	if string(hdr[:8]) != "EFI PART" {
		return 0, 0, fmt.Errorf("%s: no GPT (no \"EFI PART\" at LBA 1)", path)
	}
	// The header's CRC32 is of its own size's bytes, the CRC field zeroed.
	hsize := binary.LittleEndian.Uint32(hdr[12:])
	if hsize < 92 || hsize > 512 {
		return 0, 0, fmt.Errorf("%s: a GPT header of %d bytes", path, hsize)
	}
	sum := binary.LittleEndian.Uint32(hdr[16:])
	clear(hdr[16:20])
	if crc32.ChecksumIEEE(hdr[:hsize]) != sum {
		return 0, 0, fmt.Errorf("%s: its GPT header's CRC32 is not its content's", path)
	}
	sectors := uint64(st.Size() / 512)
	lba, n, size := binary.LittleEndian.Uint64(hdr[72:]), int64(binary.LittleEndian.Uint32(hdr[80:])), int64(binary.LittleEndian.Uint32(hdr[84:]))
	if n <= 0 || n > 1024 || size < 128 || size > 4096 || size%8 != 0 {
		return 0, 0, fmt.Errorf("%s: a GPT of %d entries of %d bytes", path, n, size)
	}
	table := uint64((n*size + 511) / 512) // the entries' sectors, primary and backup alike
	if lba < 2 || lba >= sectors || table > sectors-lba {
		return 0, 0, fmt.Errorf("%s: its GPT's entries at LBA %d, not within its %d sectors", path, lba, sectors)
	}
	// The partitions lie between the header's first and last usable LBAs: after the primary
	// entries, before the backup ones and the backup header at the disk's end.
	usable0, usable1 := binary.LittleEndian.Uint64(hdr[40:]), binary.LittleEndian.Uint64(hdr[48:])
	if usable0 < lba+table || usable1 < usable0 || usable1 >= sectors || table+1 >= sectors-usable1 {
		return 0, 0, fmt.Errorf("%s: its GPT's usable LBAs %d to %d, not between its entries and the backup GPT of its %d sectors", path, usable0, usable1, sectors)
	}
	entries := make([]byte, n*size)
	if _, err := f.ReadAt(entries, int64(lba)*512); err != nil {
		return 0, 0, fmt.Errorf("%s: the GPT's entries: %w", path, err)
	}
	if crc32.ChecksumIEEE(entries) != binary.LittleEndian.Uint32(hdr[88:]) {
		return 0, 0, fmt.Errorf("%s: its GPT entries' CRC32 is not the header's", path)
	}
	var named int64
	for i := int64(0); i < n; i++ {
		e := entries[i*size:]
		first, last := binary.LittleEndian.Uint64(e[32:]), binary.LittleEndian.Uint64(e[40:])
		// An entry of a zero type GUID is unused, whatever else it holds.
		if allZero(e[:16]) || gptName(e[56:128]) != recoveryName {
			continue
		}
		if named != 0 {
			return 0, 0, fmt.Errorf("%s: two partitions named %s in its GPT", path, recoveryName)
		}
		// Within the usable LBAs: a partition over the GPT or past it is no layout to size from.
		if first < usable0 || last < first || last > usable1 {
			return 0, 0, fmt.Errorf("%s: its recovery partition, LBA %d to %d, is not within its usable LBAs %d to %d", path, first, last, usable0, usable1)
		}
		named = int64(last-first+1) * 512
	}
	mib := func(b int64) int64 { return (b + 1<<20 - 1) >> 20 }
	if named == 0 {
		return 0, 0, fmt.Errorf("%s: no partition named %s in its GPT", path, recoveryName)
	}
	if mib(named) <= recoveryExtraMiB {
		return 0, 0, fmt.Errorf("%s: a recovery partition of %d MiB holds no system image", path, mib(named))
	}
	return mib(named), mib(st.Size()), nil
}

// gptName is a GPT entry's name: UTF-16LE, NUL-padded; a name with a code unit past ASCII is ""
// (a Kairos label is ASCII, and no other name stands for one).
func gptName(b []byte) string {
	var s []byte
	for i := 0; i+1 < len(b); i += 2 {
		if b[i] == 0 && b[i+1] == 0 {
			break
		}
		if b[i+1] != 0 || b[i] >= 0x80 {
			return ""
		}
		s = append(s, b[i])
	}
	return string(s)
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// DiskBuilder makes disk images with Docker: auroraboot (privileged, on the host's network so a
// loopback registry is reachable) for the raw, VHD and GCE disks, and qemu-img in a container for
// qcow2 and the OVA's disk. Its WorkDir must be on disk: a disk is gigabytes, and auroraboot needs
// about 10 GB more while it works.
type DiskBuilder struct {
	Docker  string // the docker CLI; "docker" when empty
	WorkDir string
	Stdout  io.Writer
	Stderr  io.Writer
}

func (b *DiskBuilder) docker(ctx context.Context, args ...string) error {
	d := b.Docker
	if d == "" {
		d = "docker"
	}
	cmd := exec.CommandContext(ctx, d, args...)
	cmd.Stdout, cmd.Stderr = b.Stdout, b.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w", strings.Join(args[:min(2, len(args))], " "), err)
	}
	return nil
}

// Build makes r's disks and returns each format's file. The cleanup function removes them, and is
// always safe to call.
func (b *DiskBuilder) Build(ctx context.Context, r DiskRequest) (Disks, func(), error) {
	if err := os.MkdirAll(b.WorkDir, 0o700); err != nil {
		return Disks{}, nil, err
	}
	if err := OnDisk(b.WorkDir); err != nil {
		return Disks{}, nil, err
	}
	dir, err := os.MkdirTemp(b.WorkDir, "disks-")
	if err != nil {
		return Disks{}, nil, err
	}
	// auroraboot and qemu-img write as root: their files are handed back before they are removed.
	cleanup := func() {
		if os.RemoveAll(dir) != nil {
			_ = b.chown(context.WithoutCancel(ctx), dir)
			_ = os.RemoveAll(dir)
		}
	}
	files, osMiB, err := b.build(ctx, dir, r)
	if err != nil {
		cleanup()
		return Disks{}, func() {}, err
	}
	return Disks{Files: files, OSMiB: osMiB}, cleanup, nil
}

func (b *DiskBuilder) build(ctx context.Context, dir string, r DiskRequest) (map[string]string, int64, error) {
	// Base lands in qemu-img's shell command line.
	if !safeBase.MatchString(r.Base) {
		return nil, 0, fmt.Errorf("disk name %q: only letters, digits, '.', '_' and '-'", r.Base)
	}
	for _, f := range r.Formats {
		if !slices.Contains(spec.DiskFormats, f) {
			return nil, 0, fmt.Errorf("unknown disk format %q", f)
		}
	}
	if r.RootfsMiB <= 0 {
		return nil, 0, errors.New("disks: the bundle's size is unknown")
	}
	want := func(f string) bool { return slices.Contains(r.Formats, f) }
	config := filepath.Join(dir, "docker-config.json")
	if err := writeDockerConfig(config, r.Registry, r.Auth); err != nil {
		return nil, 0, err
	}
	outDir := filepath.Join(dir, "out")
	if err := os.Mkdir(outDir, 0o700); err != nil {
		return nil, 0, err
	}
	out := map[string]string{}
	file := func(f string) string { return filepath.Join(outDir, r.Base+"."+Ext[f]) }
	// Every format is made from the raw disk.
	state := StateMiB(r.RootfsMiB)
	if err := b.auroraboot(ctx, dir, config, r.Source, state, "disk.efi", ".raw", file("raw")); err != nil {
		return nil, 0, err
	}
	// The recovery partition auroraboot made says the size of a system image: a state partition
	// under three of them, and their quarter, is made again once with that size.
	recovery, rawMiB, err := rawLayout(file("raw"))
	if err != nil {
		return nil, 0, err
	}
	if need := stateFor(imageOf(recovery)); need > state {
		fmt.Fprintf(cmp.Or[io.Writer](b.Stderr, io.Discard), "disks: the state partition of %d MiB holds no three system images of this bundle: made again with %d MiB\n", state, need)
		state = need
		if err := os.Remove(file("raw")); err != nil {
			return nil, 0, err
		}
		if err := b.auroraboot(ctx, dir, config, r.Source, state, "disk.efi", ".raw", file("raw")); err != nil {
			return nil, 0, err
		}
		// The second run's recovery partition, measured again, must hold to it: no third run.
		if recovery, rawMiB, err = rawLayout(file("raw")); err != nil {
			return nil, 0, err
		}
		if need := stateFor(imageOf(recovery)); need > state {
			return nil, 0, fmt.Errorf("disks: auroraboot made the recovery partition larger again (%d MiB): the state partition of %d MiB holds no three system images of it", recovery, state)
		}
	}
	osMiB := rawMiB + state
	out["raw"] = file("raw")
	if slices.ContainsFunc(r.Formats, func(f string) bool { return f != "raw" && f != "iso" }) {
		if err := b.disktools(ctx, dir); err != nil {
			return nil, 0, err
		}
	}
	if want("qcow2") {
		if err := b.qemu(ctx, outDir, fmt.Sprintf(qcow2Cmd, r.Base)); err != nil {
			return nil, 0, err
		}
		out["qcow2"] = file("qcow2")
	}
	if want("ova") {
		vmdk := r.Base + "-disk1.vmdk"
		// The disk grows to its capacity, never shrinks: Kairos's partitions and the app's least
		// space fit in it, however large the bundle; never under the raw disk, which osMiB holds.
		capacity := OVACapacityMiB(osMiB) << 20
		script := fmt.Sprintf(ovaScript, r.Base, capacity, vmdk)
		if err := b.qemu(ctx, outDir, script); err != nil {
			return nil, 0, err
		}
		if err := writeOVA(file("ova"), r.Base, filepath.Join(outDir, vmdk), capacity); err != nil {
			return nil, 0, err
		}
		if err := os.Remove(filepath.Join(outDir, vmdk)); err != nil {
			return nil, 0, err
		}
		out["ova"] = file("ova")
	}
	if want("vhd") {
		if err := b.qemu(ctx, outDir, fmt.Sprintf(vhdScript, r.Base)); err != nil {
			return nil, 0, err
		}
		if err := checkVHD(file("vhd")); err != nil {
			return nil, 0, err
		}
		out["vhd"] = file("vhd")
	}
	if want("gce") {
		if err := b.qemu(ctx, outDir, fmt.Sprintf(gceScript, r.Base)); err != nil {
			return nil, 0, err
		}
		if err := checkGCE(file("gce")); err != nil {
			return nil, 0, err
		}
		out["gce"] = file("gce")
	}
	// The installer lays out the raw disk's partitions on the machine it installs.
	if want("iso") {
		if err := b.iso(ctx, dir, config, r.Source, state, r.Base, file("iso")); err != nil {
			return nil, 0, err
		}
		out["iso"] = file("iso")
	}
	if !want("raw") {
		if err := os.Remove(out["raw"]); err != nil {
			return nil, 0, err
		}
		delete(out, "raw")
	}
	return out, osMiB, nil
}

// auroraboot makes one disk of source with flag set and moves the file it wrote, the one ending in
// suffix, to dst. auroraboot can exit 0 without writing its disk, so the file is looked for.
func (b *DiskBuilder) auroraboot(ctx context.Context, dir, config, source string, stateMiB int64, flag, suffix, dst string) error {
	state, err := os.MkdirTemp(dir, "state-")
	if err != nil {
		return err
	}
	args := []string{"run", "--rm", "--privileged", "--network", "host",
		"--volume", config + ":/root/.docker/config.json:ro",
		"--volume", "/dev:/dev",
		"--volume", state + ":/state",
		Auroraboot}
	for _, f := range auroraFlags {
		args = append(args, "--set", f)
	}
	args = append(args, "--set", "state_dir=/state", "--set", "container_image=oci://"+source, "--set", fmt.Sprintf("disk.state_size=%d", stateMiB),
		"--set", flag+"=true")
	err = b.docker(ctx, args...)
	if err != nil {
		return fmt.Errorf("auroraboot %s: %w", flag, err)
	}
	if err := b.chown(ctx, state); err != nil {
		return err
	}
	entries, err := os.ReadDir(state)
	if err != nil {
		return err
	}
	var found []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), suffix) {
			found = append(found, e.Name())
		}
	}
	if len(found) != 1 {
		return fmt.Errorf("auroraboot %s: expected one *%s file in its output, found %d", flag, suffix, len(found))
	}
	if err := os.Rename(filepath.Join(state, found[0]), dst); err != nil {
		return err
	}
	if err := os.Chmod(dst, 0o644); err != nil { // auroraboot leaves it world-writable
		return err
	}
	return os.RemoveAll(state)
}

// iso makes the installer ISO of the bundle source with auroraboot's build-iso, embedding
// isoConfig for a state partition of stateMiB, as dst. It needs no device: no loop device is made.
func (b *DiskBuilder) iso(ctx context.Context, dir, config, source string, stateMiB int64, base, dst string) error {
	state, err := os.MkdirTemp(dir, "iso-")
	if err != nil {
		return err
	}
	cc := filepath.Join(dir, "iso-config.yaml")
	if err := os.WriteFile(cc, fmt.Appendf(nil, isoConfig, stateMiB), 0o644); err != nil {
		return err
	}
	err = b.docker(ctx, "run", "--rm", "--network", "host",
		"--volume", config+":/root/.docker/config.json:ro",
		"--volume", cc+":/iso-config.yaml:ro",
		"--volume", state+":/state",
		Auroraboot, "build-iso", "--output", "/state", "--override-name", base, "--cloud-config", "/iso-config.yaml", "oci://"+source)
	if err != nil {
		return fmt.Errorf("auroraboot build-iso: %w", err)
	}
	if err := b.chown(ctx, state); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(state, base+".iso"), dst); err != nil {
		return fmt.Errorf("auroraboot build-iso: %w", err)
	}
	if err := os.Chmod(dst, 0o644); err != nil {
		return err
	}
	return os.RemoveAll(state)
}

// chown gives dir back to the user running stowage, through a container: what auroraboot wrote
// belongs to root.
func (b *DiskBuilder) chown(ctx context.Context, dir string) error {
	return b.docker(ctx, "run", "--rm", "--entrypoint", "chown", "--volume", dir+":/state",
		Auroraboot, "-R", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "/state")
}

// disktoolsTag names the qemu-img image built from disktools: its recipe's digest.
func disktoolsTag() string {
	return "stowage-disktools:" + strings.TrimPrefix(sum(disktools), "sha256:")[:16]
}

func (b *DiskBuilder) disktools(ctx context.Context, dir string) error {
	ctxDir := filepath.Join(dir, "disktools")
	if err := os.Mkdir(ctxDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte(disktools), 0o600); err != nil {
		return err
	}
	return b.docker(ctx, "buildx", "build", "--load", "--platform", "linux/amd64", "--tag", disktoolsTag(), ctxDir)
}

// qemu runs script with qemu-img in dir, as the user running stowage.
func (b *DiskBuilder) qemu(ctx context.Context, dir, script string) error {
	return b.docker(ctx, "run", "--rm", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--volume", dir+":/work", "--workdir", "/work", disktoolsTag(), "sh", "-c", script)
}

// checkVHD refuses a file Azure does not import: a VHD whose footer is not a valid fixed disk's,
// or whose size is not a whole number of MiB.
func checkVHD(path string) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return err
	}
	name := filepath.Base(path)
	var foot [512]byte
	if st.Size() < int64(len(foot)) {
		return fmt.Errorf("%s: %d bytes, no VHD footer", name, st.Size())
	}
	if _, err := fh.ReadAt(foot[:], st.Size()-int64(len(foot))); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	// The checksum is the one's complement of the footer's bytes' sum, its own field left out.
	var sum uint32
	for i, c := range foot {
		if i < 64 || i >= 68 {
			sum += uint32(c)
		}
	}
	size := binary.BigEndian.Uint64(foot[48:56])
	switch {
	case string(foot[:8]) != "conectix":
		return fmt.Errorf("%s: no VHD footer", name)
	case binary.BigEndian.Uint32(foot[64:68]) != ^sum:
		return fmt.Errorf("%s: the VHD footer's checksum is wrong", name)
	case binary.BigEndian.Uint32(foot[60:64]) != 2:
		return fmt.Errorf("%s: disk type %d, not fixed (2)", name, binary.BigEndian.Uint32(foot[60:64]))
	case size == 0 || size%(1<<20) != 0:
		return fmt.Errorf("%s: virtual size %d is not a whole number of MiB", name, size)
	case uint64(st.Size()) != size+512:
		return fmt.Errorf("%s: %d bytes for a fixed disk of %d", name, st.Size(), size)
	}
	return nil
}

// checkGCE refuses a file Google Cloud does not import: a tar.gz that does not hold a disk.raw of a
// whole number of GiB alone.
func checkGCE(path string) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	name := filepath.Base(path)
	gz, err := gzip.NewReader(fh)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if h.Name != "disk.raw" || h.Size == 0 || h.Size%gceUnit != 0 {
		return fmt.Errorf("%s: first entry %s of %d bytes, not a disk.raw of a whole number of GiB", name, h.Name, h.Size)
	}
	if h, err := tr.Next(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("%s: %s after disk.raw: Google Cloud imports an archive holding disk.raw alone", name, h.Name)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// writeDockerConfig writes the Docker config auroraboot pulls the bundle with, readable by the
// user alone.
func writeDockerConfig(path, registry string, a authn.AuthConfig) error {
	entry := map[string]string{}
	switch {
	case a.IdentityToken != "":
		entry["identitytoken"] = a.IdentityToken
	case a.Auth != "":
		entry["auth"] = a.Auth
	case a.Username != "" || a.Password != "":
		entry["auth"] = base64.StdEncoding.EncodeToString([]byte(a.Username + ":" + a.Password))
	}
	if a.RegistryToken != "" {
		entry["registrytoken"] = a.RegistryToken
	}
	auths := map[string]any{}
	if registry == "index.docker.io" {
		registry = "https://index.docker.io/v1/" // where Docker and go-containerregistry look Docker Hub up
	}
	if len(entry) > 0 {
		auths[registry] = entry
	}
	raw, err := json.Marshal(map[string]any{"auths": auths})
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// writeOVA writes the OVA: a tar of the descriptor, then the disk, as the format requires.
func writeOVA(path, name, vmdk string, capacity int64) (err error) {
	st, err := os.Stat(vmdk)
	if err != nil {
		return err
	}
	desc, err := render("template.ovf", ovf, struct {
		VMName, DiskFile           string
		DiskFileSize, DiskCapacity int64
	}{name, filepath.Base(vmdk), st.Size(), capacity})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	tw := tar.NewWriter(f)
	if err := tw.WriteHeader(&tar.Header{Name: name + ".ovf", Mode: 0o644, Size: int64(len(desc)), Format: tar.FormatUSTAR}); err != nil {
		return err
	}
	if _, err := tw.Write(desc); err != nil {
		return err
	}
	// USTAR, which the OVF specification names, holds at most 8 GiB a file; a larger disk takes
	// GNU's size extension, which VMware and VirtualBox read.
	format := tar.FormatUSTAR
	if st.Size() >= ustarMax {
		format = tar.FormatGNU
	}
	if err := tw.WriteHeader(&tar.Header{Name: filepath.Base(vmdk), Mode: 0o644, Size: st.Size(), Format: format}); err != nil {
		return err
	}
	src, err := os.Open(vmdk)
	if err != nil {
		return err
	}
	defer src.Close()
	if _, err := io.Copy(tw, src); err != nil {
		return err
	}
	return tw.Close()
}

// OnDisk refuses a work directory in memory (tmpfs, ramfs): what is written there is RAM. Tests,
// whose temporary directories may be in memory, replace it.
var OnDisk = refuseRAM

// errRAM is returned for a work directory in memory.
var errRAM = errors.New("is in memory (tmpfs or ramfs): disk images are gigabytes; set XDG_CACHE_HOME to a directory on disk")
