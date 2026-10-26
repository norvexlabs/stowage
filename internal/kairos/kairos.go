// Package kairos is the recipe of the Kairos VM shape's OS bundle — a Dockerfile and the files it
// adds, rendered for one project — and the builder of the bundle's disk images.
package kairos

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"unicode"

	"github.com/norvexlabs/stowage/internal/spec"
)

//go:embed Dockerfile.tmpl
var dockerfile string

//go:embed all:files
var files embed.FS

// Params is what the recipe is rendered with.
type Params struct {
	Name string // the project's name: it names the paths and units the bundle adds
	Ctl  string // the installer's name: the bundle carries it as /usr/sbin/<Ctl>
	// Base is the distribution converted into Kairos, and KairosInit the kairos-init image that
	// converts it; both pinned by digest.
	Base, KairosInit string
	// OSVersion is the version the OS image carries: the release's version, without its channel.
	OSVersion string
	// Modules are host.modules: the bundle's modprobe and modules-load files, and the build's
	// checks of them.
	Modules map[string]spec.KernelModule
	// Packages are host.packages: installed after kairos-init's, a purged one included.
	Packages map[string]spec.HostPackage
	// KernelArgs are host.kernel_args: the image's boot script adds them to the command line.
	KernelArgs map[string]spec.HostValue
}

// Purged are the packages kairos-init installs that the bundle removes, before its init stage: no
// part of the bundle uses them, and each one is a service, a listener, a report to a vendor or a
// tool for storage the appliance does without. PurgedAfterInit go after it, as its init stage
// enables them unconditionally. The build fails when one is still there after the purges; one
// host.packages declares, and one a declared package depends on, is installed again after the
// init stage (so its initrd lacks it).
var (
	Purged = []string{
		"haveged", "lldpd", "mdadm", "nbd-client", "nfs-common", "open-iscsi", "os-prober", "packagekit",
		"rpcbind", "snmpd", "ubuntu-advantage-tools", "xauth", "xclip", "xdg-user-dirs", "zfsutils-linux",
	}
	PurgedAfterInit = []string{"fail2ban"}
)

// ownedPackages are the prefixes of the packages the bundle's own boot depends on: the kernel, the
// boot loader, the initrd's builder, the init system, the container runtime and Kairos. A project
// declaring one would change what boots or runs the stack.
var ownedPackages = []string{"cloud-init", "containerd", "docker", "dracut", "grub", "immucore", "kairos", "linux-generic", "linux-image", "linux-modules", "shim", "systemd", "udev"}

// OwnedPackage reports whether name is a package of the bundle's own boot, which host.packages
// may not name, and the prefix it matched.
func OwnedPackage(name string) (string, bool) { return owned(ownedPackages, name) }

// owned reports the entry of list that name is, or starts with followed by - or ., if any.
func owned(list []string, name string) (string, bool) {
	for _, p := range list {
		if name == p || strings.HasPrefix(name, p+"-") || strings.HasPrefix(name, p+".") {
			return p, true
		}
	}
	return "", false
}

// ownedArgs are the kernel arguments of Kairos's own boot: the root, its image and how it is
// mounted, the boot loader's (BOOT_IMAGE, initrd), the initrd's (rd.*, rdinit, and dracut's network:
// ip, ifname, bond, vlan, …), Kairos's (cos.*, kairos.*, and the configuration keys its agent reads
// from the command line: config_url, install, stages, strict, debug, …), the consoles, the init
// system (init, systemd.*, udev.*), the security modules, the interfaces' names the network
// configuration follows, and what a panic does. The build also refuses one the boot script sets.
var ownedArgs = []string{
	"1", "2", "3", "4", "5", "BOOT_IMAGE", "S", "apparmor", "biosdevname", "bond", "bootdev", "bridge", "bundles",
	"cloud-init-paths", "config_url", "console", "cos", "debug", "eject-cd", "emergency", "enforcing", "env",
	"fail_on_bundles_errors", "fstab", "grub_options", "ifname", "immucore", "init", "initrd", "install", "ip",
	"k3s", "kairos", "lsm", "luks", "mount.usr", "mount.usrflags", "nameserver", "net.ifnames", "net.naming-scheme",
	"noresume", "options", "p2p", "panic", "rd", "rdinit", "rescue", "reset", "resume", "ro", "root", "roothash",
	"rootdelay", "rootflags", "rootfstype", "rootwait", "rw", "s", "security", "selinux", "single", "stages",
	"strict", "systemd", "team", "udev", "upgrade", "users", "usrhash", "vlan",
}

// heldPackages match the installed packages the boot and the stack run on (the kernel, the init
// system, the initrd's builder and what it carries, the boot loader, the container runtime): the
// project's packages are installed with them held, so the initrd the init stage built still
// matches the root it mounts.
const heldPackages = `^(linux-|systemd|libsystemd|udev|libudev|dracut|grub|shim|cryptsetup|libcryptsetup|lvm2|dmsetup|libdevmapper|multipath-tools|kmod|libkmod|docker-|containerd|openssh-|nftables|libnftables)`

// Held is heldPackages, the pattern the build's hold matches installed packages with.
func (Params) Held() string { return heldPackages }

// OwnedKernelArg reports whether name is a kernel argument of Kairos's own boot, which
// host.kernel_args may not name, and the name or prefix it matched: compared with _ read as -, as
// systemd and udev read their arguments; any name holding the fallback boot's marker.
func OwnedKernelArg(name string) (string, bool) {
	n := strings.ReplaceAll(name, "_", "-")
	if strings.Contains(n, FallbackMarker) {
		return "upgrade_failure", true
	}
	for _, a := range ownedArgs {
		if _, ok := owned([]string{strings.ReplaceAll(a, "_", "-")}, n); ok {
			return a, true
		}
	}
	return "", false
}

// FallbackMarker is what Kairos's boot finds on the command line of a boot that fell back to the
// passive system (upgrade_failure, with _ read as -): no argument of the project's may hold it.
const FallbackMarker = "upgrade-failure"

// MarksFallback reports whether a kernel argument's value holds the fallback boot's marker.
func MarksFallback(value string) bool {
	return strings.Contains(strings.ReplaceAll(value, "_", "-"), FallbackMarker)
}

// PurgeBeforeInit and PurgeAfterInit are the packages of Purged and PurgedAfterInit the build
// removes, a declared one included: it is installed again after the init stage.
func (Params) PurgeBeforeInit() []string { return Purged }
func (Params) PurgeAfterInit() []string  { return PurgedAfterInit }

// own are packages the bundle's boot and stack run on, which the build's last step finds
// installed: no step after theirs removes them.
var own = []string{"containerd.io", "docker-ce", "docker-compose-plugin", "dracut", "linux-image-generic", "nftables", "openssh-server", "systemd", "systemd-sysv"}

// Own are the packages the build's last step finds installed, sorted.
func (Params) Own() []string { return own }

// Absent are the purged packages the build fails on finding after its purges, sorted.
func (p Params) Absent() []string {
	out := append(p.undeclared(Purged), p.undeclared(PurgedAfterInit)...)
	sort.Strings(out)
	return out
}

func (p Params) undeclared(names []string) []string {
	var out []string
	for _, n := range names {
		if _, ok := p.Packages[n]; !ok {
			out = append(out, n)
		}
	}
	return out
}

// placeholder in a file's path is replaced by the project's name.
const placeholder = "NAME"

// KairosInitRelease is the kairos-init release whose 08_grub.yaml the bundle's boot assessment is
// derived from: the bundle's build refuses another (the Dockerfile names it too).
const KairosInitRelease = "v4.4.0-rc2"

// PreloadDir is the directory of the context the image archives go to, and CtlDir the one
// <short>-stow goes to.
const (
	PreloadDir = "preload"
	CtlDir     = "ctl"
)

// Dockerfile renders the bundle's Dockerfile.
func Dockerfile(p Params) ([]byte, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	return render("Dockerfile", dockerfile, p)
}

// WriteContext writes the files the Dockerfile copies into dir, an empty build context, and
// creates its PreloadDir and CtlDir.
func WriteContext(dir string, p Params) error {
	if err := p.check(); err != nil {
		return err
	}
	err := fs.WalkDir(files, "files", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := files.ReadFile(name)
		if err != nil {
			return err
		}
		out, err := render(name, string(raw), p)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(strings.ReplaceAll(name, placeholder, p.Name)))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, out, mode(name))
	})
	if err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(dir, PreloadDir), 0o755); err != nil {
		return err
	}
	return os.Mkdir(filepath.Join(dir, CtlDir), 0o755)
}

// mode is a file's mode in the image: the scripts under usr/sbin run.
func mode(name string) os.FileMode {
	if path.Dir(name) == "files/usr/sbin" {
		return 0o755
	}
	return 0o644
}

// check refuses Params a template would render into empty paths: missingkey=error does not see
// an empty field.
func (p Params) check() error {
	if p.Name == "" || p.Ctl == "" {
		return fmt.Errorf("kairos: the project's name and its installer's are required (%q, %q)", p.Name, p.Ctl)
	}
	// What lint checks, again: the modules and packages are written into the build's shell and
	// modprobe's files.
	for name, m := range p.Modules {
		if !spec.ModuleName.MatchString(name) || strings.ContainsFunc(m.Reason, unicode.IsControl) {
			return fmt.Errorf("host.modules.%s: not a module's name, or a reason of more than one line (`stowage lint`)", name)
		}
		for o, v := range m.Options {
			if !spec.ModuleOption.MatchString(o) || !spec.ModuleValue.MatchString(v) {
				return fmt.Errorf("host.modules.%s.options: %q=%q (`stowage lint`)", name, o, v)
			}
		}
	}
	for name := range p.Packages {
		if _, owned := OwnedPackage(name); owned || !spec.PackageName.MatchString(name) {
			return fmt.Errorf("host.packages.%s: not a package's name, or one of the bundle's own (`stowage lint`)", name)
		}
	}
	for name, a := range p.KernelArgs {
		if _, owned := OwnedKernelArg(name); owned || MarksFallback(a.Value) || !spec.KernelArgName.MatchString(name) || a.Value != "" && !spec.KernelArgValue.MatchString(a.Value) {
			return fmt.Errorf("host.kernel_args.%s: not a kernel argument's name or value, or one of Kairos's own (`stowage lint`)", name)
		}
	}
	return nil
}

func render(name, text string, data any) ([]byte, error) {
	t, err := template.New(name).Option("missingkey=error").Parse(text)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return nil, fmt.Errorf("render %s: %w", name, err)
	}
	return b.Bytes(), nil
}

// BundleRecipe is a digest of what the bundle is built from before rendering: its Dockerfile and
// files. A change to either changes the bundle ship builds.
func BundleRecipe() string {
	var names []string
	_ = fs.WalkDir(files, "files", func(name string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, name)
		}
		return err
	})
	sort.Strings(names)
	// The bundle records its unpacked size (publish's RootfsAnnotation), which its disks need: a
	// bundle built before it did is rebuilt.
	kv := []string{"Dockerfile", dockerfile, "records", "rootfs-mib"}
	for _, n := range names {
		raw, _ := files.ReadFile(n)
		kv = append(kv, n, fmt.Sprintf("%o:%s", mode(n), raw))
	}
	return sum(kv...)
}

// DiskRecipe is a digest of what the disk images are made with: the pinned auroraboot, the
// qemu-img image's recipe and the OVA descriptor.
func DiskRecipe() string {
	return sum("auroraboot", Auroraboot, "flags", strings.Join(auroraFlags, " "), "sizing", sizing, "disktools", disktools, "ovf", ovf,
		"qcow2", qcow2Cmd, "ova", ovaScript, "vhd", vhdScript, "gce", gceScript, "ustar", fmt.Sprint(ustarLimit))
}

func sum(kv ...string) string {
	h := sha256.New()
	for _, s := range kv {
		fmt.Fprintf(h, "%d:%s", len(s), s)
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}
