package install

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/release"
)

// upgradeCheck runs, against the install env records, what an upgrade to this installer's release
// decides before it changes anything, as the VM's boot runs it: given no input, asking nothing.
// The release must be this project's, its front one the install keeps (upgradeFits), and every
// input it adds must take a value without being given one, so each route it adds has its domain
// (upgradeRoutes), and its services' whole cores fit this host (coresUpgrade). It reads and writes
// nothing but env.
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
	if err := in.upgradeRoutes(env, next, mode, pub); err != nil {
		return err
	}
	return in.coresUpgrade(ctx)
}

// checkUpgrade refuses, before the OS of release m is written, an upgrade the boot into it would
// refuse (upgradeCheck), so a refusal never costs a reboot there and one back. The release's
// installer is read as targetRelease reads it. An upgrade across a schema baseline is refused
// (reset-db --new-baseline crosses it); of a payload this Stowage does not read (a newer
// Stowage's) only that is checked: the boot decides the rest.
func (h *Hostd) checkUpgrade(ctx context.Context, in *Installer, env *EnvFile, m *release.Manifest) error {
	// An install that runs m already (a crossing whose boot fell back) moves no data: its OS is
	// written again.
	name, v, rev := runs(env)
	if v == m.ProjectVersion && rev == strconv.Itoa(m.Revision) {
		return nil
	}
	// The data is at the baseline of the release the install runs: the OS's, or — after a
	// crossing whose boot fell back — the later one its own installer carries.
	var running baselined = in
	if this := in.R.Manifest; v != this.ProjectVersion || rev != strconv.Itoa(this.Revision) {
		kept, err := in.keptInstaller(ctx, name, v, rev)
		if err != nil {
			return fail(http.StatusConflict, "%v", err)
		}
		running = kept
	}
	k, err := h.targetRelease(ctx, in, env, m)
	if err != nil {
		return err
	}
	if from, to, ok := baselines(&in.O, running, k); ok && from != to {
		way := fmt.Sprintf("`%s reset-db --new-baseline --version %s` wipes the data and installs it", in.R.Ctl(), m.ProjectVersion)
		if running != baselined(in) { // reset-db resets the OS's release: the install's comes back first
			way = fmt.Sprintf("`%s upgrade --version %s` writes the OS of the release the install runs, then `%s reset-db --new-baseline --version %s` wipes the data and installs it", in.R.Ctl(), v, in.R.Ctl(), m.ProjectVersion)
		}
		return fail(http.StatusConflict, "%s %s starts schema baseline %d, and the install runs %s %s, at baseline %d: its data cannot carry over, so its OS is not written; %s",
			m.Name, m.ProjectVersion, to, m.Name, v, from, way)
	}
	if k.Release == nil {
		h.O.logf("the installer of %s carries a spec this Stowage does not read (%v): the boot into it checks the upgrade", m.Tag, k.unread)
		return nil
	}
	t := &Installer{R: k.Release, O: in.O}
	if err := t.upgradeCheck(ctx, env); err != nil {
		return fail(http.StatusConflict, "%v\nthe boot into %s would refuse this upgrade (it is given no input), so its OS is not written", err, m.Tag)
	}
	return nil
}

// targetRelease is release m as its installer carries it, fetched as `upgrade --version` fetches
// it — by the digest of m, whose signature target checked, and carrying m — and read from its
// payload, never run: Release nil when this Stowage does not read its spec, whose schema baseline
// it still tells.
func (h *Hostd) targetRelease(ctx context.Context, in *Installer, env *EnvFile, m *release.Manifest) (*keptRelease, error) {
	prefix, _ := env.Get("STOWAGE_REGISTRY")
	c, err := in.registryClient(prefix)
	if err != nil {
		return nil, err
	}
	exe, err := in.fetchInstaller(ctx, c, prefix, m)
	if err != nil {
		return nil, fail(http.StatusBadGateway, "the installer of %s: %v", m.Tag, err)
	}
	defer os.RemoveAll(filepath.Dir(exe))
	files, err := payload.Read(exe)
	if err != nil {
		return nil, fail(http.StatusBadGateway, "the installer of %s: %v", m.Tag, err)
	}
	k, err := keptOf(files)
	if err != nil {
		return nil, fail(http.StatusBadGateway, "the installer of %s: %v", m.Tag, err)
	}
	return k, nil
}
