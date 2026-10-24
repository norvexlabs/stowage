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
	kv := []string{"Dockerfile", dockerfile}
	for _, n := range names {
		raw, _ := files.ReadFile(n)
		kv = append(kv, n, fmt.Sprintf("%o:%s", mode(n), raw))
	}
	return sum(kv...)
}

// DiskRecipe is a digest of what the disk images are made with: the pinned auroraboot, the
// qemu-img image's recipe and the OVA descriptor.
func DiskRecipe() string {
	return sum("auroraboot", Auroraboot, "flags", strings.Join(auroraFlags, " "), "disktools", disktools, "ovf", ovf,
		"qcow2", qcow2Cmd, "ova", ovaScript, "vhd", vhdScript, "gce", gceScript, "ova-bytes", fmt.Sprint(OVADiskBytes), "ustar", fmt.Sprint(ustarLimit))
}

func sum(kv ...string) string {
	h := sha256.New()
	for _, s := range kv {
		fmt.Fprintf(h, "%d:%s", len(s), s)
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}
