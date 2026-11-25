package install

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/version"
)

// upgradeCheck runs, against the install env records, what an upgrade to this installer's release
// decides before it changes anything, as the VM's boot runs it: given the inputs the upgrade
// stages for that boot (given, which may be empty) and nothing else, asking nothing. The release
// must be this project's, its front one the install keeps (upgradeFits); given must name only
// inputs it adds (onlyAdded), and every input it adds must take a value, so each route it adds has
// its domain (upgradeRoutes), and its services' whole cores fit this host (coresUpgrade). It reads
// and writes nothing but env.
func (in *Installer) upgradeCheck(ctx context.Context, env *EnvFile, given map[string]string) error {
	o := &in.O
	o.Set, o.Answers, o.Ports, o.Prompt = given, nil, nil, nil
	in.givenNothing = o.Preloaded
	o.Getenv = func(string) string { return "" } // the boot's environment gives no input either
	if err := in.R.checkFront(); err != nil {
		return err
	}
	if _, _, _, err := in.sameProject(env, in.R.Manifest); err != nil {
		return err
	}
	mode, _ := env.Get("STOWAGE_TLS_MODE")
	pub, err := in.upgradeFits(env, mode)
	if err != nil {
		return err
	}
	if err := in.recordedForms(env); err != nil {
		return err
	}
	added := in.addedInputs(env, mode)
	if err := in.onlyAdded(env, added); err != nil {
		return err
	}
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

// checksTarget reports whether an upgrade to m fetches m's installer and checks it against the
// install env records before the OS is written (checkUpgrade): not before the first install, not
// for a rewrite of the running release, and not when the install runs m already (a crossing whose
// boot fell back moves no data: its OS is written again). `updates` tells it, so `upgrade` says
// the download comes.
func checksTarget(first, rewrite bool, env *EnvFile, m *release.Manifest) bool {
	_, v, rev := runs(env)
	return !first && !rewrite && !isAt(m.ProjectVersion, m.Revision, v, rev)
}

// checkUpgrade refuses, before the OS of release m is written, an upgrade the boot into it would
// refuse, so a refusal never costs a reboot there and one back. The release's installer is read as
// targetRelease reads it. An upgrade across a schema baseline is always refused (reset-db
// --new-baseline crosses it); the install's values are judged as valuesCheck judges them, and what
// it leaves to the boot is answered (empty when nothing). channel is the one the upgrade was asked
// with ("" for the install's), which the crossing's hint names again; args the upgrade's
// (upgradeArgs), which a refusal's fix names; given the inputs the upgrade stages for the boot
// into m. Asked only when checksTarget.
func (h *Hostd) checkUpgrade(ctx context.Context, in *Installer, env *EnvFile, m *release.Manifest, channel, args string, given map[string]string) (string, error) {
	name, v, rev := runs(env)
	// The data is at the baseline of the release the install runs: the OS's, or — after a
	// crossing whose boot fell back — the later one its own installer carries.
	var running baselined = in
	if this := in.R.Manifest; !isAt(this.ProjectVersion, this.Revision, v, rev) {
		kept, err := in.keptInstaller(ctx, name, v, rev)
		if err != nil {
			return "", failAs(http.StatusConflict, err)
		}
		running = kept
	}
	k, err := h.targetRelease(ctx, in, env, m)
	if err != nil {
		return "", err
	}
	if from, to, ok := baselines(&in.O, running, k); ok && from != to {
		// reset-db's --version finds m, on the channel the upgrade was asked with: its --unordered is
		// decided by m against the release the install runs (needsUnordered from runsVersion, as
		// resetPlan orders it) — v, before and after an upgrade to v. upgrade's --version v names
		// that release, which the daemon never refuses as unordered. Both are the VM's commands, as
		// the operator types them over SSH (vmCtl).
		ch := ""
		if channel != "" {
			ch = " --channel " + channel
		}
		reset := fmt.Sprintf("`%s reset-db --new-baseline%s --version %s%s` wipes it and installs %s", in.vmCtl(), ch, m.ProjectVersion, unorderedFlag(needsUnordered(m, h.runsVersion(env))), m.ProjectVersion)
		way := reset
		if running != baselined(in) { // reset-db resets the OS's release: the install's comes back first
			way = fmt.Sprintf("`%s upgrade --version %s` writes the OS of the release the install runs, then %s", in.vmCtl(), v, reset)
		}
		return "", failAs(http.StatusConflict, baselineRefused("stay on this release, or cross to it with its data wiped (reset-db --new-baseline)", m, to, fmt.Sprintf("the install runs %s %s", m.Name, v), from, way+"; its OS is not written"))
	}
	return h.valuesCheck(ctx, in, env, m, k, "this upgrade", &vmGiving{run: "the upgrade", cmd: "upgrade" + args, since: stagedInputsSince}, given)
}

// valuesCheck judges the install's values against target release m, as its installer carries it
// (k), the way the boot into m takes them (upgradeCheck), given the inputs the run stages for that
// boot (given) — only when m was published by the Stowage that built this daemon, whose rules are
// then the boot's. Otherwise, or when this Stowage does not read m's spec, it refuses nothing and
// answers why, logged: the boot into m judges, with its own rules, and falls back to the release
// the install runs on a refusal; where it reads m's spec, what this Stowage's rules would refuse is
// answered with it, so a dry run names it. what names the run a refusal stops ("this upgrade",
// "it"); gives the command that gives the boot the inputs the release adds (the upgrade, the
// crossing), which a refusal of one names — or says why it gives none, when m's boot does not
// take what that command stages.
func (h *Hostd) valuesCheck(ctx context.Context, in *Installer, env *EnvFile, m *release.Manifest, k *keptRelease, what string, gives *vmGiving, given map[string]string) (string, error) {
	check := func() error {
		g := *gives
		if !stagedInputsBy(m.StowageVersion, h.Stowage, g.since) {
			g.none = fmt.Sprintf("%s was published by Stowage %s, whose boot takes no input %s gives: it is reached only by a release that requires none", m.Tag, cmp.Or(m.StowageVersion, "of no recorded version"), g.run)
		}
		c := &Installer{R: k.Release, O: in.O, vmGives: &g}
		return c.upgradeCheck(ctx, env, given)
	}
	note := ""
	switch {
	case k.Release == nil:
		note = fmt.Sprintf("the installer of %s carries a spec this Stowage does not read (%v)", m.Tag, k.unread)
	case !sameStowage(m.StowageVersion, h.Stowage):
		note = fmt.Sprintf("%s was published by Stowage %s, and the host daemon is Stowage %s's", m.Tag, cmp.Or(m.StowageVersion, "of no recorded version"), cmp.Or(h.Stowage, "of no recorded version"))
	}
	if note != "" {
		note += ": the boot into it judges whether it takes the install's values, with its own rules, and on a refusal falls back to the release the install runs"
		if k.Release != nil {
			if err := check(); err != nil {
				note += fmt.Sprintf("; the host daemon's rules would refuse it: %v", err)
			}
		}
		h.O.logf("%s", note)
		return note, nil
	}
	if err := check(); err != nil {
		// A refusal stays one, with the fix it was raised with on the VM (or none).
		gets := "it is given no input"
		if len(given) > 0 {
			gets = "it is given only the inputs " + gives.run + " gives"
		}
		return "", failAs(http.StatusConflict, fmt.Errorf("%w\nthe boot into %s would refuse %s (%s), so its OS is not written", err, m.Tag, what, gets))
	}
	return "", nil
}

// sameStowage reports whether two Stowage versions, as release manifests record them, are one: one
// that does not read as a version (none recorded) is never the same as another.
func sameStowage(a, b string) bool {
	c, err := version.SemVer{}.Compare(a, b)
	return err == nil && c == 0
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
	h.O.logf("fetching the installer of %s to check it against the install, before its OS is written", m.Tag)
	exe, files, err := in.fetchInstaller(ctx, c, prefix, m)
	if err != nil {
		return nil, fail(http.StatusBadGateway, "the installer of %s: %v", m.Tag, err)
	}
	defer os.RemoveAll(filepath.Dir(exe))
	k, err := keptOf(files)
	if err != nil {
		return nil, fail(http.StatusBadGateway, "the installer of %s: %v", m.Tag, err)
	}
	return k, nil
}

// baselineRefused is the refusal, fixed by fix, of an upgrade to release m, which starts schema
// baseline to, of an install at baseline from: runs says what runs that install ("the install runs
// notes 1.0.0", "release notes in ns runs notes 1.0.0"), way the commands that cross to m with the
// data wiped. Every shape refuses a crossing in these words.
func baselineRefused(fix string, m *release.Manifest, to int, runs string, from int, way string) error {
	return refuse(fix, "%s %s starts schema baseline %d, and %s, at baseline %d: its data cannot carry over, so it moves to %s only with its data wiped: %s",
		m.Name, m.ProjectVersion, to, runs, from, m.ProjectVersion, way)
}
