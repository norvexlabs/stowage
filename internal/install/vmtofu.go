package install

import (
	"bytes"
	"cmp"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/norvexlabs/stowage/internal/truenas"
)

// tofuModules are the OpenTofu modules `vm create` applies, one directory per platform that has
// one.
//
//go:embed tofu/truenas/*.tf
var tofuModules embed.FS

// The ways `vm create` makes a VM.
const (
	CreateTofu   = "opentofu"
	CreateManual = "manual"
)

// CtlVar names, in OpenTofu's environment, the <short>-stow its module's steps run.
const CtlVar = "STOWAGE_CTL"

// sshPoll is how often a new VM is asked whether it has installed.
var sshPoll = 10 * time.Second

// TofuVM is a VM OpenTofu creates on TrueNAS: where on the host, and its size.
type TofuVM struct {
	Name      string // the VM's name on TrueNAS: letters and digits
	Pool      string
	Dataset   string // under the pool: where its zvol and staging dataset go
	Bridge    string // the interface or bridge its NIC attaches to
	CPUs      int
	MemoryMiB int
	DiskGiB   int
}

// tofuVars are the module's variables (variables.tf): what the state records, never a credential.
type tofuVars struct {
	Insecure    bool   `json:"insecure_skip_verify"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Pool        string `json:"pool"`
	Dataset     string `json:"parent_dataset"`
	CPUs        int    `json:"cpus"`
	MemoryMiB   int    `json:"memory_mib"`
	DiskGiB     int    `json:"disk_gib"`
	Bridge      string `json:"nic_attach"`
	Image       string `json:"image"`
	ImageSHA256 string `json:"image_sha256"`
	Seed        string `json:"seed"`
	SeedSHA256  string `json:"seed_sha256"`
}

// xdgDir is the XDG base directory env names, else home's def; a relative value is ignored, as
// the XDG spec says (OpenTofu runs in another directory than this run's).
func (o *Options) xdgDir(env, def, what string) (string, error) {
	if base := o.getenv(env); filepath.IsAbs(base) {
		return base, nil
	}
	if home := o.getenv("HOME"); filepath.IsAbs(home) {
		return filepath.Join(home, def), nil
	}
	return "", fmt.Errorf("neither %s nor HOME is an absolute path: where %s goes", env, what)
}

// vmsDir holds the VMs OpenTofu created, one directory each (stateDir):
// $XDG_STATE_HOME/<name>/vms (~/.local/state by default).
func (o *Options) vmsDir(name string) (string, error) {
	base, err := o.xdgDir("XDG_STATE_HOME", filepath.Join(".local", "state"), "a VM's OpenTofu state")
	if err != nil {
		return "", err
	}
	return filepath.Join(base, name, "vms"), nil
}

// stateDir is where a VM OpenTofu created keeps its module, variables and state.
func (o *Options) stateDir(name, host string) (string, error) {
	vms, err := o.vmsDir(name)
	if err != nil {
		return "", err
	}
	// ssh reads its known hosts file's path with escapes and % tokens: none may be in it.
	if strings.ContainsAny(vms, `%\"`) {
		return "", fmt.Errorf("%s: a VM's state directory cannot hold %%, \\ or \" (set XDG_STATE_HOME elsewhere)", vms)
	}
	return filepath.Join(vms, host), nil
}

// diskCache is where `vm create` keeps the disks it fetched for OpenTofu:
// $XDG_CACHE_HOME/<name>/disks (~/.cache by default). Fetched again only when gone.
func (o *Options) diskCache(name string) (string, error) {
	base, err := o.xdgDir("XDG_CACHE_HOME", ".cache", "the disk image")
	if err != nil {
		return "", err
	}
	return filepath.Join(base, name, "disks"), nil
}

// varsFile names a VM's variables in its state directory: there once `vm create` applied, or
// tried to — what `vm destroy` needs, and what a second create of the host refuses.
const varsFile = "terraform.tfvars.json"

// claimedBy is the host of another VM OpenTofu created whose TrueNAS name is vmName, ignoring case
// (its zvol and staging dataset are named in lowercase): "" when none.
func (o *Options) claimedBy(name, host, vmName string) (string, error) {
	vms, err := o.vmsDir(name)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(vms)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == host {
			continue
		}
		b, err := os.ReadFile(filepath.Join(vms, e.Name(), varsFile))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		var v tofuVars
		if err := json.Unmarshal(b, &v); err != nil {
			return "", fmt.Errorf("%s: %w", filepath.Join(vms, e.Name(), varsFile), err)
		}
		if strings.EqualFold(v.Name, vmName) {
			return e.Name(), nil
		}
	}
	return "", nil
}

// creation asks how the VM is made, on a platform OpenTofu can create it on.
func (in *Installer) creation(ctx context.Context, p Platform, given string) (string, error) {
	if given != "" && given != CreateTofu && given != CreateManual {
		return "", fmt.Errorf("--create %s: %s or %s", given, CreateTofu, CreateManual)
	}
	if !p.Tofu {
		if given == CreateTofu {
			return "", fmt.Errorf("--create %s: OpenTofu creates VMs on truenas only; %s is by hand", CreateTofu, p.Label)
		}
		return CreateManual, nil
	}
	if given == "" && in.O.Prompt != nil {
		a, err := in.O.Prompt.Ask(ctx, Form{
			Questions: []Question{{Title: "How is the VM created", Key: "--create", Choices: []Choice{
				{CreateTofu, "OpenTofu creates it on " + p.Label + " (`tofu` on this machine)"},
				{CreateManual, "By hand: write its disk and seed, and print the steps"},
			}}},
			Defaults: func(map[string]string) map[string]string { return map[string]string{"--create": CreateTofu} },
		})
		if err != nil {
			return "", err
		}
		given = a["--create"]
	}
	return cmp.Or(given, CreateManual), nil
}

// tofuReady checks what OpenTofu needs before anything is asked: `tofu`, and the TrueNAS API's
// address and key in the environment, which the provider and the module's steps read.
func (in *Installer) tofuReady(ctx context.Context) error {
	o := &in.O
	if o.Tofu == nil {
		return errors.New("OpenTofu is not available to this run")
	}
	if _, err := o.Tofu.Run(ctx, Cmd{Args: []string{"version"}}); err != nil {
		return fmt.Errorf("--create %s runs OpenTofu's `tofu` (https://opentofu.org/docs/intro/install/): %w", CreateTofu, err)
	}
	for _, k := range []string{truenas.URLVar, truenas.KeyVar} {
		if o.getenv(k) == "" {
			return fmt.Errorf("--create %s: %s is not set (the TrueNAS API's %s, read from the environment only)", CreateTofu, k,
				map[string]string{truenas.URLVar: "address, https://<host>[:<port>]", truenas.KeyVar: "key"}[k])
		}
	}
	_, err := truenas.ParseURL(o.getenv(truenas.URLVar))
	return err
}

var vmName = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)

// tofuVM asks what the flags did not give of where on TrueNAS the VM goes and its size.
func (in *Installer) tofuVM(ctx context.Context, t *TofuVM, host string) error {
	num := func(min int) func(string) error {
		return func(v string) error {
			if n, err := strconv.Atoi(v); err != nil || n < min {
				return fmt.Errorf("%q: a whole number, at least %d", v, min)
			}
			return nil
		}
	}
	nonEmpty := func(v string) error {
		if strings.TrimSpace(v) != v || v == "" {
			return fmt.Errorf("%q: a name without spaces around it", v)
		}
		return nil
	}
	dataset := func(v string) error {
		if err := nonEmpty(v); err != nil {
			return err
		}
		if strings.HasPrefix(v, "/") || strings.HasSuffix(v, "/") || strings.Contains(v, "//") {
			return fmt.Errorf("%q: a dataset path under the pool, e.g. vms", v)
		}
		return nil
	}
	checkName := func(v string) error {
		if !vmName.MatchString(v) {
			return fmt.Errorf("%q: letters and digits only, as TrueNAS names a VM", v)
		}
		return nil
	}
	itoa := func(n int) string {
		if n == 0 {
			return ""
		}
		return strconv.Itoa(n)
	}
	qs := []Question{
		{Title: "The VM's name on TrueNAS", Key: "--vm-name", Check: checkName},
		{Title: "The pool its disk goes on", Key: "--pool", Required: true, Check: nonEmpty},
		{Title: "The dataset, under the pool, its disk goes in", Key: "--dataset", Required: true, Check: dataset},
		{Title: "The bridge or interface its network attaches to", Key: "--bridge", Required: true, Check: nonEmpty},
		{Title: "Its CPUs", Key: "--cpus", Check: num(1)},
		{Title: "Its memory, in MiB", Key: "--memory", Check: num(1024)},
		{Title: "Its disk, in GiB", Key: "--disk", Check: num(16)},
	}
	given := map[string]string{"--vm-name": t.Name, "--pool": t.Pool, "--dataset": t.Dataset, "--bridge": t.Bridge,
		"--cpus": itoa(t.CPUs), "--memory": itoa(t.MemoryMiB), "--disk": itoa(t.DiskGiB)}
	defaults := map[string]string{"--vm-name": strings.Map(func(r rune) rune {
		if r < '0' || (r > '9' && r < 'A') || (r > 'Z' && r < 'a') || r > 'z' {
			return -1
		}
		return r
	}, host), "--cpus": "2", "--memory": "4096", "--disk": "40"}
	a, err := in.askGiven(ctx, qs, given, defaults)
	if err != nil {
		return err
	}
	for _, k := range []string{"--vm-name", "--pool", "--dataset", "--bridge"} {
		if a[k] == "" {
			return fmt.Errorf("%s is required with --create %s", k, CreateTofu)
		}
	}
	t.Name, t.Pool, t.Dataset, t.Bridge = a["--vm-name"], a["--pool"], a["--dataset"], a["--bridge"]
	t.CPUs, _ = strconv.Atoi(a["--cpus"])
	t.MemoryMiB, _ = strconv.Atoi(a["--memory"])
	t.DiskGiB, _ = strconv.Atoi(a["--disk"])
	return nil
}

// writeModule writes the platform's module and its variables into dir.
func writeModule(dir, platform string, v tofuVars) error {
	root := path.Join("tofu", platform)
	entries, err := fs.ReadDir(tofuModules, root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		b, err := fs.ReadFile(tofuModules, path.Join(root, e.Name()))
		if err != nil {
			return err
		}
		if err := writeAtomic(filepath.Join(dir, e.Name()), bytes.NewReader(b), 0o644, nil); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, varsFile), bytes.NewReader(append(b, '\n')), 0o644, nil)
}

// tofu runs OpenTofu in dir, its output on the log, with this run's environment (the TrueNAS
// API's address and key among it) and CtlVar naming ctl.
func (in *Installer) tofu(ctx context.Context, dir, ctl string, args ...string) error {
	env := append(in.O.environ(), CtlVar+"="+ctl, "TF_IN_AUTOMATION=1", "TF_INPUT=0")
	_, err := in.O.Tofu.Run(ctx, Cmd{Args: append([]string{"-chdir=" + dir}, args...), Env: env, Stream: in.O.Log})
	return err
}

// applyVM writes the module into dir and applies it: the VM is created, its disk written from
// image, its seed attached, and it is started.
func (in *Installer) applyVM(ctx context.Context, dir, ctl string, t *TofuVM, image, imageSHA, seed string) error {
	seedSHA, err := truenas.SHA256File(seed)
	if err != nil {
		return err
	}
	m := in.R.Manifest
	v := tofuVars{
		Insecure: truenas.Insecure(in.O.getenv(truenas.InsecureVar)),
		Name:     t.Name, Description: fmt.Sprintf("%s %s (revision %d), created by %s vm create", in.R.Spec.Name, m.ProjectVersion, m.Revision, in.R.Ctl()),
		Pool: t.Pool, Dataset: t.Dataset, CPUs: t.CPUs, MemoryMiB: t.MemoryMiB, DiskGiB: t.DiskGiB, Bridge: t.Bridge,
		Image: image, ImageSHA256: imageSHA, Seed: seed, SeedSHA256: seedSHA,
	}
	if err := writeModule(dir, "truenas", v); err != nil {
		return err
	}
	if err := in.tofu(ctx, dir, ctl, "init", "-input=false", "-no-color"); err != nil {
		return err
	}
	return in.tofu(ctx, dir, ctl, "apply", "-input=false", "-auto-approve", "-no-color")
}

// sshArgs are ssh's arguments to run command on the VM at addr as user: in batch mode (no
// prompt), with the private key next to the public one given when there is one, and the host key
// taken on first contact into the file known, apart from the operator's known hosts.
func sshArgs(known, user, addr, pubKey string, command ...string) []string {
	// Quoted: ssh splits an option's value at white space, into several files.
	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=accept-new",
		"-o", `UserKnownHostsFile="` + known + `"`}
	if priv, ok := strings.CutSuffix(expandHome(pubKey), ".pub"); ok {
		if _, err := os.Stat(priv); err == nil {
			args = append(args, "-i", priv, "-o", "IdentitiesOnly=yes")
		}
	}
	return append(append(args, user+"@"+addr, "--"), command...)
}

// awaitVM waits, up to wait, until `<short>-stow status --json` on the VM at addr answers from a
// system that is not Kairos's recovery and — unless the install waits for a secret input —
// reports the install. A first boot runs the recovery system, then resets and reboots into the
// active one, each with its own SSH host key: every attempt takes the key into a file of its own,
// and the VM's known_hosts in dir is the one of the attempt that answered.
func (in *Installer) awaitVM(ctx context.Context, dir, user, addr, pubKey string, wait time.Duration, installs bool) error {
	o, ctl := &in.O, in.R.Ctl()
	if o.SSH == nil {
		return errors.New("ssh is not available to this run")
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	o.logf("waiting for %s@%s to answer over SSH%s (up to %s)", user, addr, map[bool]string{true: ", then to install"}[installs], wait)
	var last error
	known, attempt := filepath.Join(dir, "known_hosts"), filepath.Join(dir, "known_hosts.attempt")
	for {
		if err := os.Remove(attempt); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		out, err := o.SSH.Run(ctx, Cmd{Args: sshArgs(attempt, user, addr, pubKey, ctl, "status", "--json")})
		if err == nil {
			var st Status
			if err = json.Unmarshal(out, &st); err == nil {
				switch {
				case st.Boot == "recovery":
					err = errors.New("the recovery system answers: the first boot is not done")
				case installs && st.InstallFailed != "":
					if rerr := os.Rename(attempt, known); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
						return rerr
					}
					return fmt.Errorf("%s@%s: the first install failed: %s (`ssh %s@%s sudo journalctl -u %s` tells more); `ssh %s@%s sudo %s install` runs it again; the VM is kept: `%s vm destroy %s` removes it",
						user, addr, st.InstallFailed, user, addr, in.R.Spec.Name, user, addr, ctl, ctl, filepath.Base(dir))
				case installs && st.Installed == nil:
					err = errors.New("not installed yet")
				case installs && st.Installing:
					err = errors.New("the first install has not finished: its stack is coming up, or the provision hook runs")
				default:
					if err := os.Rename(attempt, known); err != nil && !errors.Is(err, os.ErrNotExist) {
						return err
					}
					return nil
				}
			}
		}
		if ctx.Err() == nil || last == nil { // an attempt cut by the deadline says nothing new
			last = err
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("%s@%s: not ready after %s (last: %v); the VM is kept: look at its console on TrueNAS, or `%s vm destroy %s`", user, addr, wait, last, ctl, filepath.Base(dir))
			}
			return fmt.Errorf("%w; the VM is kept: `%s vm destroy %s` removes it", ctx.Err(), ctl, filepath.Base(dir))
		case <-time.After(sshPoll):
		}
	}
}

// VMDestroy is a run of `vm destroy`.
type VMDestroy struct {
	Host string
	Yes  bool // destroy without asking
	Ctl  string
}

// DestroyVM destroys a VM `vm create` made with OpenTofu — the VM, its disk and its uploads on
// TrueNAS — then removes its state directory. The disk image fetched stays in the cache.
func (in *Installer) DestroyVM(ctx context.Context, d *VMDestroy) error {
	o, name := &in.O, in.R.Spec.Name
	if err := checkHostname(d.Host); err != nil {
		return err
	}
	dir, err := o.stateDir(name, d.Host)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, varsFile)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no VM %s made by `%s vm create --create %s`: %s holds no state", d.Host, in.R.Ctl(), CreateTofu, dir)
		}
		return err
	}
	if err := in.tofuReady(ctx); err != nil {
		return err
	}
	if !d.Yes {
		if o.Prompt == nil {
			return fmt.Errorf("destroying %s deletes its disk: give --yes", d.Host)
		}
		a, err := o.Prompt.Ask(ctx, Form{
			Questions: []Question{{Title: fmt.Sprintf("Destroy the VM %s: its disk, its data and its uploads on TrueNAS", d.Host), Key: "confirm",
				Choices: []Choice{{"no", "No, keep it"}, {"yes", "Yes, destroy it"}}}},
			Defaults: func(map[string]string) map[string]string { return map[string]string{"confirm": "no"} },
		})
		if err != nil {
			return err
		}
		if a["confirm"] != "yes" {
			return ErrAborted
		}
	}
	if err := in.tofu(ctx, dir, d.Ctl, "init", "-input=false", "-no-color"); err != nil {
		return err
	}
	if err := in.tofu(ctx, dir, d.Ctl, "destroy", "-input=false", "-auto-approve", "-no-color"); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "Destroyed %s.\n", d.Host)
	return nil
}

// truenasCommand is `vm truenas`, hidden: the steps of the TrueNAS module OpenTofu runs, each
// reading its arguments and the API's address and key from the environment.
func truenasCommand(getenv func(string) string) *cobra.Command {
	t := &cobra.Command{
		Use:    "truenas",
		Short:  "The steps OpenTofu's TrueNAS module runs (TRUENAS_URL, TRUENAS_API_KEY, TRUENAS_INSECURE_SKIP_VERIFY)",
		Hidden: true,
		Args:   cobra.NoArgs,
	}
	need := func(keys ...string) error {
		for _, k := range keys {
			if getenv(k) == "" {
				return fmt.Errorf("%s is not set", k)
			}
		}
		return nil
	}
	vmID := func() (int64, error) {
		id, err := strconv.ParseInt(getenv("VM_ID"), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("VM_ID %q: %w", getenv("VM_ID"), err)
		}
		return id, nil
	}
	step := func(use, short string, keys []string, f func(context.Context, *truenas.Client) error) *cobra.Command {
		return &cobra.Command{
			Use: use, Short: short, Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				if err := need(keys...); err != nil {
					return err
				}
				c, err := truenas.FromEnv(cmd.Context(), getenv)
				if err != nil {
					return err
				}
				defer c.Close()
				return f(cmd.Context(), c)
			},
		}
	}
	t.AddCommand(
		step("import-image", "Upload IMAGE (checked against IMAGE_SHA256) into STAGING_DIR and write it into the zvol ZVOL",
			[]string{"IMAGE", "IMAGE_SHA256", "STAGING_DIR", "ZVOL"}, func(ctx context.Context, c *truenas.Client) error {
				return c.ImportImage(ctx, getenv("IMAGE"), getenv("IMAGE_SHA256"), getenv("STAGING_DIR"), getenv("ZVOL"))
			}),
		step("seed", "Upload the seed ISO SEED to SEED_PATH", []string{"SEED", "SEED_PATH"}, func(ctx context.Context, c *truenas.Client) error {
			return c.Upload(ctx, getenv("SEED"), getenv("SEED_PATH"))
		}),
		step("start", "Start the VM VM_ID unless it runs", []string{"VM_ID"}, func(ctx context.Context, c *truenas.Client) error {
			id, err := vmID()
			if err != nil {
				return err
			}
			return c.Start(ctx, id)
		}),
		step("stop", "Power the VM VM_ID off when it runs", []string{"VM_ID"}, func(ctx context.Context, c *truenas.Client) error {
			id, err := vmID()
			if err != nil {
				return err
			}
			return c.Stop(ctx, id)
		}),
		step("drop-snapshots", "Delete every snapshot of DATASET", []string{"DATASET"}, func(ctx context.Context, c *truenas.Client) error {
			return c.DropSnapshots(ctx, getenv("DATASET"))
		}),
	)
	return t
}
