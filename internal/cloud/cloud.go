// Package cloud registers a release's disks as images in the publisher's cloud accounts: each
// cloud's built-in OpenTofu module uploads the disk and makes an image of it, with the
// publisher's credentials from the environment.
package cloud

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

//go:embed tofu/*/*.tf tofu/*/.terraform.lock.hcl tofu/*/*/*.tf tofu/*/*/.terraform.lock.hcl
var modules embed.FS

// Setting is a variable of a cloud's module read from the publisher's environment.
type Setting struct {
	Var, Env string
	// Default is the value when Env is unset; a setting without one is required.
	Default string
}

// Settings are each cloud's settings. A cloud is uploaded to when one of its required STOWAGE_
// settings is set; its required settings must then all be.
var Settings = map[string][]Setting{
	"aws": {
		{Var: "bucket", Env: "STOWAGE_AWS_BUCKET"},
		{Var: "region", Env: "AWS_REGION"},
		{Var: "vmimport_role", Env: "STOWAGE_AWS_VMIMPORT_ROLE", Default: "vmimport"},
	},
	"azure": {
		{Var: "resource_group", Env: "STOWAGE_AZURE_RESOURCE_GROUP"},
		{Var: "storage_account", Env: "STOWAGE_AZURE_STORAGE_ACCOUNT"},
		{Var: "container", Env: "STOWAGE_AZURE_CONTAINER"},
		{Var: "location", Env: "STOWAGE_AZURE_LOCATION"},
	},
	"gcp": {
		{Var: "project", Env: "STOWAGE_GCP_PROJECT"},
		{Var: "bucket", Env: "STOWAGE_GCP_BUCKET"},
	},
}

// Vars are the module variables of cloud read through getenv: nil when none of its required
// STOWAGE_ settings is set (nothing is uploaded to it), an error naming the required settings
// left unset when only some are.
func Vars(cloud string, getenv func(string) string) (map[string]string, error) {
	ss, ok := Settings[cloud]
	if !ok {
		return nil, fmt.Errorf("unknown cloud %q", cloud)
	}
	on := slices.ContainsFunc(ss, func(s Setting) bool {
		return s.Default == "" && strings.HasPrefix(s.Env, "STOWAGE_") && getenv(s.Env) != ""
	})
	if !on {
		return nil, nil
	}
	vars := map[string]string{}
	var set, missing []string
	for _, s := range ss {
		v := getenv(s.Env)
		if v != "" && strings.HasPrefix(s.Env, "STOWAGE_") {
			set = append(set, s.Env)
		}
		if v == "" {
			v = s.Default
		}
		if v == "" {
			missing = append(missing, s.Env)
		}
		vars[s.Var] = v
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("cloud %s: %s set, so its image is uploaded: set %s too", cloud, strings.Join(set, ", "), strings.Join(missing, ", "))
	}
	return vars, nil
}

// Required are the variables cloud's required settings are read from.
func Required(cloud string) []string {
	var out []string
	for _, s := range Settings[cloud] {
		if s.Default == "" {
			out = append(out, s.Env)
		}
	}
	return out
}

// Image is a disk to register: its image's name on the cloud (letters, digits and '-', starting
// with a letter: every cloud's rule), a description, and how to get the disk's file — written
// into dir, its path and SHA-256 (hex) returned — fetched only when the image is not registered.
type Image struct {
	Cloud, Name, Description string
	Vars                     map[string]string
	Fetch                    func(ctx context.Context, dir string) (file, sha256 string, err error)
}

// imageName is the names every cloud accepts for an image.
var imageName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

// Runner runs tofu with args in env, its output streamed to log, and returns its stdout when log
// is nil.
type Runner func(ctx context.Context, args, env []string, log io.Writer) ([]byte, error)

// Uploader applies a cloud's module in a working directory of its own per image and account, under
// WorkDir (on disk: a disk is gigabytes), which keeps the image's id once registered. An image is
// registered once: an image of its name already in the account, made by another machine or before
// the directory was lost, is taken as it is.
type Uploader struct {
	WorkDir string
	// Env is tofu's environment: the clouds' credentials, never the registry's; nil is this
	// process's.
	Env []string
	Log io.Writer
	// Tofu runs tofu (`vm create`'s runner, which stops a cancelled run gracefully).
	Tofu Runner
}

// idFile holds, in an image's working directory, its id on the cloud once registered.
const idFile = "id"

// dir is the working directory of img: by cloud, by account (the settings' digest, so other settings
// never reuse an id), then by name.
func (u *Uploader) dir(img Image) (string, error) {
	if _, ok := Settings[img.Cloud]; !ok {
		return "", fmt.Errorf("unknown cloud %q", img.Cloud)
	}
	if !imageName.MatchString(img.Name) {
		return "", fmt.Errorf("image name %q: lower-case letters, digits and '-', starting with a letter, at most 63", img.Name)
	}
	b, err := json.Marshal(img.Vars) // keys sorted
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return filepath.Join(u.WorkDir, img.Cloud, hex.EncodeToString(sum[:6]), img.Name), nil
}

func (u *Uploader) run(ctx context.Context, dir string, log io.Writer, args ...string) ([]byte, error) {
	if u.Tofu == nil {
		return nil, errors.New("no tofu runner")
	}
	env := u.Env
	if env == nil {
		env = os.Environ()
	}
	// Each module carries its lock file (the providers' checksums): OpenTofu takes a provider from
	// the plugin cache, or downloads it, only when it matches. Its configuration is Stowage's own,
	// empty file: the publisher's ~/.tofurc could turn that check off or replace a provider.
	env = append(slices.Clip(env), "TF_IN_AUTOMATION=1", "TF_INPUT=0", "TF_PLUGIN_CACHE_DIR="+filepath.Join(u.WorkDir, "plugins"), "TF_CLI_CONFIG_FILE="+filepath.Join(u.WorkDir, cliConfig))
	return u.Tofu(ctx, append([]string{"-chdir=" + dir}, args...), env, log)
}

// cliConfig names OpenTofu's configuration file, empty, in WorkDir.
const cliConfig = "tofurc"

// apply initialises the module in dir (the providers its lock file names, written again with the
// module, whatever a stopped run had locked) and applies it.
func (u *Uploader) apply(ctx context.Context, dir string) error {
	if err := os.MkdirAll(filepath.Join(u.WorkDir, "plugins"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(u.WorkDir, cliConfig), nil, 0o600); err != nil {
		return err
	}
	if _, err := u.run(ctx, dir, u.Log, "init", "-input=false", "-no-color"); err != nil {
		return err
	}
	_, err := u.run(ctx, dir, u.Log, "apply", "-auto-approve", "-input=false", "-no-color")
	return err
}

// output is the module's output name in dir.
func (u *Uploader) output(ctx context.Context, dir, name string) (string, error) {
	out, err := u.run(ctx, dir, nil, "output", "-raw", name)
	return strings.TrimSpace(string(out)), err
}

// removeUpload destroys the disk's upload in dir/upload, if any, and the disk a stopped run left
// here: the image does not need them once made, and the disk may carry the registry's pull
// account. Destroying reads no file: one stands in for the disk, removed since.
func (u *Uploader) removeUpload(ctx context.Context, img Image, dir string) error {
	up := filepath.Join(dir, uploadDir)
	if _, err := os.Stat(filepath.Join(up, "terraform.tfstate")); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(up, "terraform.tfvars.json"))
	if err != nil {
		return err
	}
	var vars map[string]string
	if err := json.Unmarshal(b, &vars); err != nil {
		return err
	}
	switch _, err := os.Stat(vars["file"]); {
	case err == nil:
		defer os.Remove(vars["file"])
	case errors.Is(err, os.ErrNotExist):
		stand := filepath.Join(up, "disk.removed")
		if err := os.WriteFile(stand, nil, 0o600); err != nil {
			return err
		}
		defer os.Remove(stand)
		if err := writeModule(up, path.Join("tofu", img.Cloud, uploadDir), img, map[string]string{"file": stand, "sha256": vars["sha256"]}); err != nil {
			return err
		}
	}
	if _, err := u.run(ctx, up, u.Log, "init", "-input=false", "-no-color"); err != nil {
		return fmt.Errorf("cloud %s: remove the upload of %s: %w", img.Cloud, img.Name, err)
	}
	if _, err := u.run(ctx, up, u.Log, "destroy", "-auto-approve", "-input=false", "-no-color"); err != nil {
		return fmt.Errorf("cloud %s: remove the upload of %s: %w", img.Cloud, img.Name, err)
	}
	return nil
}

// uploadDir names, in an image's working directory, the upload module's.
const uploadDir = "upload"

// record removes the upload, then keeps id as the image's in dir.
func (u *Uploader) record(ctx context.Context, img Image, dir, id string) error {
	if err := u.removeUpload(ctx, img, dir); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, idFile), []byte(id+"\n"), 0o600)
}

// Registered is the id of img on its cloud, "" when it is not registered: the id kept once
// registered; else, after a run stopped once its image was made, the module's output (its upload
// then removed); else the image of its name the account holds, looked up by the cloud's probe
// module.
func (u *Uploader) Registered(ctx context.Context, img Image) (string, error) {
	dir, err := u.dir(img)
	if err != nil {
		return "", err
	}
	switch b, err := os.ReadFile(filepath.Join(dir, idFile)); {
	case err == nil:
		return strings.TrimSpace(string(b)), nil
	case !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	if _, err := os.Stat(filepath.Join(dir, "terraform.tfstate")); err == nil {
		// A run stopped part way: an id once its image was made, else the account is looked at,
		// and the next upload goes on.
		if id, err := u.output(ctx, dir, "id"); err == nil && id != "" {
			return id, u.record(ctx, img, dir, id)
		}
	}
	probe := filepath.Join(dir, "probe")
	if err := os.MkdirAll(probe, 0o700); err != nil {
		return "", err
	}
	if err := writeModule(probe, path.Join("tofu", img.Cloud, "probe"), img, map[string]string{"description": img.Description}); err != nil {
		return "", err
	}
	if err := u.apply(ctx, probe); err != nil {
		return "", fmt.Errorf("cloud %s: look for image %s: %w", img.Cloud, img.Name, err)
	}
	id, err := u.output(ctx, probe, "id")
	if err != nil {
		return "", fmt.Errorf("cloud %s: look for image %s: %w", img.Cloud, img.Name, err)
	}
	if id != "" {
		return id, u.record(ctx, img, dir, id)
	}
	// An image of that name not ready (failed, or still being made), or another disk's, holds the
	// name.
	unready, err := u.output(ctx, probe, "unready")
	if err != nil {
		return "", fmt.Errorf("cloud %s: look for image %s: %w", img.Cloud, img.Name, err)
	}
	if unready != "" {
		return "", fmt.Errorf("cloud %s: image %s exists and is not this disk's ready image (%s): wait for it, or delete it, then ship again", img.Cloud, img.Name, unready)
	}
	return "", nil
}

// Upload registers img on its cloud and returns its id: the disk fetched into the image's working
// directory and uploaded (the upload module, in upload/), the image made of the upload (the image
// module), then the upload and the disk removed. A run stopped part way leaves OpenTofu's states,
// so the next one goes on from them.
func (u *Uploader) Upload(ctx context.Context, img Image) (string, error) {
	dir, err := u.dir(img)
	if err != nil {
		return "", err
	}
	up := filepath.Join(dir, uploadDir)
	if err := os.MkdirAll(up, 0o700); err != nil {
		return "", err
	}
	file, sum, err := img.Fetch(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("cloud %s: %w", img.Cloud, err)
	}
	defer os.Remove(file)
	if err := writeModule(up, path.Join("tofu", img.Cloud, uploadDir), img, map[string]string{"file": file, "sha256": sum}); err != nil {
		return "", fmt.Errorf("cloud %s: %w", img.Cloud, err)
	}
	id, err := u.image(ctx, img, dir, up)
	if err != nil {
		// An upload is never left behind: the next run uploads again.
		if rerr := u.removeUpload(context.WithoutCancel(ctx), img, dir); rerr != nil {
			return "", fmt.Errorf("%w (and %w: delete it by hand)", err, rerr)
		}
		return "", err
	}
	return id, u.record(ctx, img, dir, id)
}

// image uploads the disk with the upload module in up and makes the image of it with the image
// module in dir: its id.
func (u *Uploader) image(ctx context.Context, img Image, dir, up string) (string, error) {
	if err := u.apply(ctx, up); err != nil {
		return "", fmt.Errorf("cloud %s: upload: %w", img.Cloud, err)
	}
	where, err := u.output(ctx, up, "upload")
	if err != nil || where == "" {
		return "", fmt.Errorf("cloud %s: the upload module gave no upload (%v)", img.Cloud, err)
	}
	if err := writeModule(dir, path.Join("tofu", img.Cloud), img, map[string]string{"description": img.Description, "upload": where}); err != nil {
		return "", fmt.Errorf("cloud %s: %w", img.Cloud, err)
	}
	if err := u.apply(ctx, dir); err != nil {
		return "", fmt.Errorf("cloud %s: %w", img.Cloud, err)
	}
	id, err := u.output(ctx, dir, "id")
	if err != nil {
		return "", fmt.Errorf("cloud %s: %w", img.Cloud, err)
	}
	if id == "" {
		return "", fmt.Errorf("cloud %s: the module gave no image id", img.Cloud)
	}
	return id, nil
}

// writeModule writes the module at root (its files, not its directories) into dir, and its
// variables: the image's name, its cloud's settings and more.
func writeModule(dir, root string, img Image, more map[string]string) error {
	entries, err := fs.ReadDir(modules, root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := fs.ReadFile(modules, path.Join(root, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
			return err
		}
	}
	vars := map[string]string{"name": img.Name}
	for _, s := range Settings[img.Cloud] {
		vars[s.Var] = img.Vars[s.Var]
	}
	maps.Copy(vars, more)
	b, err := json.MarshalIndent(vars, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "terraform.tfvars.json"), append(b, '\n'), 0o600)
}
