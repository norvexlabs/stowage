package install

import (
	"context"
	"net/http"
	"os"
	"path/filepath"

	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/release"
)

// upgradeCheck runs, against the install env records, what an upgrade to this installer's release
// decides before it changes anything, as the VM's boot runs it: given no input, asking nothing.
// The release must be this project's, its front one the install keeps (upgradeFits), and every
// input it adds must take a value without being given one, so each route it adds has its domain
// (upgradeRoutes). It reads and writes nothing but env.
func (in *Installer) upgradeCheck(ctx context.Context, env *EnvFile) error {
	o := &in.O
	o.Set, o.Answers, o.Ports, o.Prompt = nil, nil, nil, nil
	o.Getenv = func(string) string { return "" } // the boot's environment gives no input either
	if err := in.R.checkFront(); err != nil {
		return err
	}
	if _, _, _, err := sameProject(o.Dir, env, in.R.Manifest); err != nil {
		return err
	}
	mode, _ := env.Get("STOWAGE_TLS_MODE")
	pub, err := in.upgradeFits(env, mode)
	if err != nil {
		return err
	}
	added := in.addedInputs(env, mode)
	in.recorded = map[string]string{}
	for _, k := range env.Keys() {
		in.recorded[k], _ = env.Get(k)
	}
	values, err := in.inputs(ctx, added, false)
	if err != nil {
		return err
	}
	next, _, err := in.upgradeEnv(env, mode, added, values)
	if err != nil {
		return err
	}
	return in.upgradeRoutes(env, next, mode, pub)
}

// checkUpgrade refuses, before the OS of release m is written, an upgrade the boot into it would
// refuse (upgradeCheck), so a refusal never costs a reboot there and one back. The release's
// installer is fetched as `upgrade --version` fetches it — by the digest of m, whose signature
// target checked, and carrying m — and its spec read from its payload, never run. A payload this
// Stowage does not read (a newer Stowage's) is not checked: the boot decides.
func (h *Hostd) checkUpgrade(ctx context.Context, in *Installer, env *EnvFile, m *release.Manifest) error {
	prefix, _ := env.Get("STOWAGE_REGISTRY")
	c, err := in.registryClient(prefix)
	if err != nil {
		return err
	}
	exe, err := in.fetchInstaller(ctx, c, prefix, m)
	if err != nil {
		return fail(http.StatusBadGateway, "the installer of %s: %v", m.Tag, err)
	}
	defer os.RemoveAll(filepath.Dir(exe))
	files, err := payload.Read(exe)
	if err != nil {
		return fail(http.StatusBadGateway, "the installer of %s: %v", m.Tag, err)
	}
	r, err := Load(files)
	if err != nil {
		h.O.logf("the installer of %s carries a spec this Stowage does not read (%v): the boot into it checks the upgrade", m.Tag, err)
		return nil
	}
	t := &Installer{R: r, O: in.O}
	if err := t.upgradeCheck(ctx, env); err != nil {
		return fail(http.StatusConflict, "%v\nthe boot into %s would refuse this upgrade (it is given no input), so its OS is not written", err, m.Tag)
	}
	return nil
}
