package install

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/spec"
)

// resetVolumes is what a reset wipes (data.reset) and what it keeps (the data volumes marked
// keep_on_reset), each sorted.
func (in *Installer) resetVolumes() (wipe, keep []string) {
	d := in.R.Spec.Data
	wipe = slices.Sorted(slices.Values(d.Reset))
	for _, v := range sortedKeys(d.Volumes) {
		if d.Volumes[v].KeepOnReset {
			keep = append(keep, v)
		}
	}
	return wipe, keep
}

// resetPlan is what reset-db does to a Docker or VM install, as it asks to confirm it.
func (in *Installer) resetPlan() string {
	wipe, keep := in.resetVolumes()
	what := "stop it, delete its volumes " + strings.Join(wipe, ", ")
	if len(keep) > 0 {
		what += " (keeping " + strings.Join(keep, ", ") + ")"
	}
	return what + ", forget what its provision hook returned, then start it as first installed"
}

// resetForm is the confirmation every shape's reset-db asks: default no.
func resetForm(name, what string) Form {
	return Form{
		Questions: []Question{{Title: "Reset " + name + ": " + what, Key: "confirm",
			Choices: []Choice{{"no", "No, keep the data"}, {"yes", "Yes, wipe it"}}}},
		Defaults: func(map[string]string) map[string]string { return map[string]string{"confirm": "no"} },
	}
}

// confirmReset asks before a reset, unless --yes; without a terminal it refuses.
func confirmReset(ctx context.Context, p Prompter, yes bool, name, what string) error {
	if yes {
		return nil
	}
	if p == nil {
		return errors.New("reset-db asks before it wipes the data, and there is no terminal to answer: give --yes")
	}
	a, err := p.Ask(ctx, resetForm(name, what))
	if err != nil {
		return err
	}
	if a["confirm"] != "yes" {
		return ErrAborted
	}
	return nil
}

// composeVolumes checks that each volume is one the release's compose file declares and Compose
// creates (not external): a reset that deleted nothing would leave the data. It returns each one's
// `name:`, empty when the compose file gives none.
func (in *Installer) composeVolumes(names []string) (map[string]string, error) {
	var doc struct {
		Volumes map[string]struct {
			Name     string `yaml:"name"`
			External any    `yaml:"external"`
		} `yaml:"volumes"`
	}
	if err := yaml.Unmarshal(in.R.Compose, &doc); err != nil {
		return nil, fmt.Errorf("the release's compose file: %w", err)
	}
	named := map[string]string{}
	for _, v := range names {
		decl, ok := doc.Volumes[v]
		switch {
		case !ok:
			return nil, fmt.Errorf("data.reset: %q is no volume of the compose file: reset-db would wipe nothing of it", v)
		case decl.External != nil && decl.External != false:
			return nil, fmt.Errorf("data.reset: %q is an external volume, which Compose does not own: reset-db does not delete it", v)
		}
		if strings.Contains(decl.Name, "$") {
			return nil, fmt.Errorf("data.reset: %q is named %q, which Compose interpolates: reset-db finds a volume by its labels or a literal name", v, decl.Name)
		}
		named[v] = decl.Name
	}
	return named, nil
}

// adopted reports whether the volume vol exists and belongs to project or to no Compose project
// (one Compose adopted carries no labels), and the stack mounted it. A volume of another project,
// or one without labels the stack never mounted, is refused, never deleted.
func (in *Installer) adopted(ctx context.Context, vol, project string, mounted bool) (bool, error) {
	out, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"volume", "inspect", "--format", `{{index .Labels "com.docker.compose.project"}}`, vol}})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such volume") {
			return false, nil
		}
		return false, err
	}
	if owner := strings.TrimSpace(string(out)); owner != "" && owner != "<no value>" && owner != project {
		return false, fmt.Errorf("it belongs to the Compose project %s, not %s: not deleted", owner, project)
	}
	if !mounted {
		return false, errors.New("no container of the stack mounts it, and it carries no Compose labels: not deleted (`docker volume rm` it, then reset-db again)")
	}
	return true, nil
}

// hasDatabase refuses a verb that only exists for the database when the release declares none.
func (in *Installer) hasDatabase(what string) error {
	if in.R.Spec.HasDatabase() {
		return nil
	}
	return fmt.Errorf("%s %s has no database: %s", in.R.Spec.Name, in.R.Manifest.ProjectVersion, what)
}

// Reset is `reset-db`: the install back to its first setup. The stack stops, its data.reset
// volumes are deleted (keep_on_reset ones and the front's stay), what the provision hook returned
// is forgotten, then the stack starts — migrating an empty database — and the hook runs as on a
// first install, from the values .env records. Nothing is dumped first; the service is down
// meanwhile. Every check runs before the stack stops.
func (in *Installer) Reset(ctx context.Context) error {
	o, name := &in.O, in.R.Spec.Name
	if err := in.hasDatabase("reset-db has nothing to reset"); err != nil {
		return err
	}
	wipe, _ := in.resetVolumes()
	if _, err := in.composeVolumes(wipe); err != nil {
		return err
	}
	if _, err := in.hook(); err != nil {
		return err
	}
	// A crossing also wipes what the release the install runs names: checked there.
	if o.NewBaseline {
		return in.resetBaseline(ctx)
	}
	if len(wipe) == 0 {
		return fmt.Errorf("%s names no volume in data.reset: reset-db has nothing to wipe", name)
	}
	// Checked before the confirmation, which holds no lock, and again under the lock.
	check := func(env *EnvFile) error {
		if err := in.sameRelease(ctx, env); err != nil {
			return err
		}
		_, err := in.composeProject(env)
		return err
	}
	env, err := in.open()
	if err != nil {
		return err
	}
	if err := check(env); err != nil {
		return err
	}
	what := in.resetPlan()
	if o.DryRun {
		fmt.Fprintf(o.Out, "a run would %s\n", what)
		return nil
	}
	if err := confirmReset(ctx, o.Prompt, o.Yes, name, what); err != nil {
		return err
	}
	env, unlock, err := in.openLocked(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := check(env); err != nil {
		return err
	}
	if err := in.volumesSplit(); err != nil {
		return err
	}
	if err := in.checkDocker(ctx); err != nil {
		return err
	}
	if err := in.wipeData(ctx, env, nil); err != nil {
		return err
	}
	return in.start(ctx, env)
}

// wipeSet is what wipeData deletes: the data.reset volumes, sorted, each with the name its compose
// file gives it (empty: the one Compose gives), and the files that record a provisioning. With old
// (a crossing), old's are added to this release's — its stack is the one stopped, whose volumes
// carry the data, and its names win.
func (in *Installer) wipeSet(old *Installer) (wipe, keep []string, named map[string]string, forget []string, err error) {
	named = map[string]string{}
	forget = []string{ProvisionEnv, restoreAwaitedFile}
	var kept []string
	_, ours := in.resetVolumes() // what this release keeps on a reset, never wiped
	for _, src := range []*Installer{old, in} {
		if src == nil {
			continue
		}
		keys, k := src.resetVolumes()
		kept = append(kept, k...)
		keys = slices.DeleteFunc(slices.Clone(keys), func(v string) bool { return slices.Contains(ours, v) })
		names, err := src.composeVolumes(keys)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		for _, k := range keys {
			if _, ok := named[k]; !ok {
				named[k] = names[k]
				wipe = append(wipe, k)
			}
		}
		h, err := src.hook()
		if err != nil {
			return nil, nil, nil, nil, err
		}
		out := src.outputs // another Stowage's release, read for that alone (wipeOf)
		if h != nil {
			out = h.Outputs
		}
		if out != "" && !slices.Contains(forget, out) {
			forget = append(forget, out)
		}
	}
	slices.Sort(wipe)
	for _, v := range kept {
		if !slices.Contains(wipe, v) && !slices.Contains(keep, v) {
			keep = append(keep, v)
		}
	}
	slices.Sort(keep)
	return wipe, keep, named, forget, nil
}

// wipeData stops the stack env runs and deletes the volumes data.reset names, with what the
// provision hook returned and the restored values a --secrets install waited to provision: the
// database is provisioned afresh at the next start; with old, a crossing's, its volumes too
// (wipeSet). A volume found by its name alone is deleted only when the stack used it.
func (in *Installer) wipeData(ctx context.Context, env *EnvFile, old *Installer) error {
	o, name := &in.O, in.R.Spec.Name
	wipe, _, named, forget, err := in.wipeSet(old)
	if err != nil {
		return err
	}
	project, err := in.composeProject(env)
	if err != nil {
		return err
	}
	// The volumes the stack's containers mount, read before `down` removes them.
	mounts, err := o.Docker.Run(ctx, Cmd{Args: []string{"ps", "--all", "--filter", "label=com.docker.compose.project=" + project, "--format", "{{.Mounts}}"}})
	if err != nil {
		return fmt.Errorf("listing the stack's containers: %w", err)
	}
	mounted := map[string]bool{}
	for _, m := range strings.FieldsFunc(string(mounts), func(r rune) bool { return r == ',' || r == '\n' }) {
		mounted[strings.TrimSpace(m)] = true
	}
	o.logf("stopping %s", name)
	if _, err := in.Compose(ctx, env, o.Log, "down", "--remove-orphans", "--timeout", "30"); err != nil {
		return fmt.Errorf("stopping the stack: %w", err)
	}
	// One listing of the project's volumes, each with its compose key; a volume never created
	// (its service never ran) has nothing to delete.
	out, err := o.Docker.Run(ctx, Cmd{Args: []string{"volume", "ls", "--filter", "label=com.docker.compose.project=" + project,
		"--format", `{{.Name}} {{.Label "com.docker.compose.volume"}}`}})
	if err != nil {
		return fmt.Errorf("listing the volumes: %w (the stack is stopped: reset-db again finishes the reset)", err)
	}
	byKey := map[string][]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if vol, key, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			byKey[key] = append(byKey[key], vol)
		}
	}
	for _, v := range wipe {
		if len(byKey[v]) == 0 {
			// A volume Compose did not create (made by hand, then adopted) carries no labels: it
			// is found by the name Compose gives it.
			vol := cmp.Or(named[v], project+"_"+v)
			adopted, err := in.adopted(ctx, vol, project, mounted[vol])
			if err != nil {
				return fmt.Errorf("volume %s: %w (the stack is stopped: reset-db again finishes the reset)", vol, err)
			}
			if adopted {
				byKey[v] = []string{vol}
			} else {
				o.logf("the volume %s was never created: nothing to delete", v)
			}
		}
		for _, vol := range byKey[v] {
			o.logf("deleting the volume %s", vol)
			if _, err := o.Docker.Run(ctx, Cmd{Args: []string{"volume", "rm", vol}}); err != nil {
				return fmt.Errorf("volume %s: %w (the stack is stopped: reset-db again finishes the reset)", vol, err)
			}
		}
	}
	for _, f := range forget {
		if err := os.Remove(in.path(f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// composeProject is the Compose project env records, whose volumes reset-db finds by its label.
func (in *Installer) composeProject(env *EnvFile) (string, error) {
	project, _ := env.Get("COMPOSE_PROJECT_NAME")
	if project == "" {
		return "", fmt.Errorf("%s records no COMPOSE_PROJECT_NAME: its volumes cannot be found", in.path(envName))
	}
	return project, nil
}

// crossing is the release a reset-db --new-baseline confirmed wiping: the upgrade it runs wipes
// the install's data only while the install still runs it, across a baseline it still crosses.
type crossing struct {
	version, revision string
	// old is that release as far as its wipe goes (wipeOf): the stack the wipe stops is its.
	old *Installer
}

// wipeOf is the release the install runs, kept, as far as a crossing's wipe goes: its data.reset
// and compose file — its whole Release when this Stowage reads it, else those two alone — refused
// when they do not tell what to delete, before anything changes.
func (in *Installer) wipeOf(kept *keptRelease) (*Installer, error) {
	m := kept.Manifest
	old := &Installer{R: kept.Release, O: in.O}
	if kept.Release == nil {
		if kept.resetErr != nil {
			return nil, fmt.Errorf("what %s %s wipes cannot be told (%v): --new-baseline never wipes on a guess", m.Name, m.ProjectVersion, kept.resetErr)
		}
		old.R = &Release{Manifest: m, Spec: &spec.Spec{Name: m.Name, Data: spec.Data{Reset: kept.reset, Volumes: kept.volumes}}, Compose: kept.compose}
		if kept.outputs != "" {
			if err := providers.OutputsFile(m.ShortName, kept.outputs); err != nil {
				return nil, fmt.Errorf("%s %s's hooks.provision: %w: --new-baseline cannot tell what to forget of it", m.Name, m.ProjectVersion, err)
			}
			old.outputs = kept.outputs
		}
	}
	wipe, _ := old.resetVolumes()
	if _, err := old.composeVolumes(wipe); err != nil {
		return nil, fmt.Errorf("%s %s's %w: --new-baseline cannot tell what to wipe of it", m.Name, m.ProjectVersion, err)
	}
	return old, nil
}

// resetBaseline is reset-db --new-baseline: an install of another release, at another schema
// baseline than this installer's, is wiped and upgraded to this release with its recorded values
// (Upgrade with in.crossing). Every check runs before anything changes; nothing is dumped, and
// no previous release is kept: going back across a baseline is refused anyway.
func (in *Installer) resetBaseline(ctx context.Context) error {
	o, m := &in.O, in.R.Manifest
	env, err := in.open()
	if err != nil {
		return err
	}
	name, cur, rev, err := sameProject(o.Dir, env, m)
	if err != nil {
		return err
	}
	if cur == m.ProjectVersion && rev == strconv.Itoa(m.Revision) {
		return fmt.Errorf("%s already runs %s %s (revision %s): --new-baseline installs another release, which this installer does not carry; reset-db without it wipes this one",
			o.Dir, name, cur, rev)
	}
	if err := in.newer(cur, rev); err != nil {
		return err
	}
	// Every refusal of the upgrade comes before the question, as the crossing would meet them
	// after it — the baselines and what the release the install runs wipes among them; the
	// upgrade runs them again under the lock.
	in.crossing = &crossing{version: cur, revision: rev}
	if _, _, _, _, err := in.upgradeRefusals(ctx, env, name, cur, rev); err != nil {
		return err
	}
	wipe, keep, _, _, err := in.wipeSet(in.crossing.old) // not empty: upgradeRefusals refuses that
	if err != nil {
		return err
	}
	what := "stop it, delete its volumes " + strings.Join(wipe, ", ")
	if len(keep) > 0 {
		what += " (keeping " + strings.Join(keep, ", ") + ")"
	}
	what += fmt.Sprintf(", forget what its provision hook returned, then install %s %s, at the new schema baseline, with the same values", name, m.ProjectVersion)
	if o.DryRun {
		fmt.Fprintf(o.Out, "a run would %s\n", what)
		return nil
	}
	if err := confirmReset(ctx, o.Prompt, o.Yes, name, what); err != nil {
		return err
	}
	return in.Upgrade(ctx) // which reads what the old release wipes again, under its lock
}

// crosses refuses unless the install's release, kept, and this installer's start different schema
// baselines, both told: --new-baseline never wipes on a guess, and within a baseline an upgrade
// moves the install.
func (in *Installer) crosses(kept *keptRelease, name, cur string) error {
	from, to, ok := baselines(&in.O, kept, in)
	switch {
	case !ok:
		return fmt.Errorf("the schema baseline of %s %s, which the install runs, cannot be told (the log says why): --new-baseline never wipes on a guess", name, cur)
	case from == to:
		return fmt.Errorf("%s runs %s %s, at the schema baseline of %s %s: --new-baseline only crosses a baseline; `%s upgrade --dir %s` moves it",
			in.O.Dir, name, cur, name, in.R.Manifest.ProjectVersion, cmp.Or(in.O.Self, in.ctl()), in.O.Dir)
	}
	return nil
}

// K8sReset is `k8s reset-db`: the helm release back to its first setup. The release is
// uninstalled, the PersistentVolumeClaims it made or its StatefulSets claimed are deleted, and the
// same chart is installed again with the values it was given. The Secrets the installer wrote stay,
// so the secrets and inputs are the ones first installed. Nothing is dumped first, a database
// outside the release is not touched, and the release is down meanwhile.
func (in *Installer) K8sReset(ctx context.Context) error {
	in.givenCA = true
	o, k, m := &in.O, in.O.K8s, in.R.Manifest
	if err := in.checkTarget(); err != nil {
		return err
	}
	if err := in.hasDatabase("reset-db has nothing to reset"); err != nil {
		return err
	}
	if len(in.R.Spec.Data.Reset) == 0 {
		return fmt.Errorf("%s names no volume in data.reset: reset-db has nothing to wipe", m.Name)
	}
	if m.Artifacts.Chart == nil {
		return fmt.Errorf("%s %s has no chart (published without the kubernetes shape)", m.Name, m.ProjectVersion)
	}
	major, err := in.checkHelm(ctx)
	if err != nil {
		return err
	}
	if _, err := k.Kubectl.Run(ctx, Cmd{Args: []string{"version", "--client"}}); err != nil {
		return fmt.Errorf("kubectl is required: %w", err)
	}
	rel, err := in.release(ctx)
	if err != nil {
		return err
	}
	if rel == nil {
		return fmt.Errorf("no release %s in %s", k.Release, k.Namespace)
	}
	if err := in.pending(rel); err != nil {
		return err
	}
	cur, err := in.installedRelease(ctx)
	if err != nil {
		return err
	}
	// A reset installs the release it wipes again, with its installer; only across a schema
	// baseline, asked for by --new-baseline, does it install this installer's release instead.
	other := cur.Release.Version != m.ProjectVersion || cur.Release.Revision != m.Revision
	if other || in.O.NewBaseline {
		r := cur.Release
		if !other {
			return fmt.Errorf("release %s in %s already runs %s %s (revision %d): --new-baseline installs another release, which this installer does not carry; reset-db without it wipes this one",
				k.Release, k.Namespace, m.Name, r.Version, r.Revision)
		}
		running, runErr := in.k8sRunning(ctx, cur, cmp.Or(k.Registry, cur.Registry))
		from, to, ok := in.k8sBaselines(running, runErr, cur)
		crosses := ok && from != to
		switch {
		case !in.O.NewBaseline && crosses:
			return fmt.Errorf("release %s in %s runs %s %s (revision %d), at schema baseline %d, and this installer carries %s (revision %d), at baseline %d: `%s --new-baseline` wipes it and installs %s; the installer of %s resets it as it is",
				k.Release, k.Namespace, m.Name, r.Version, r.Revision, from, m.ProjectVersion, m.Revision, to, in.k8sCtl("reset-db"), m.ProjectVersion, r.Version)
		case !in.O.NewBaseline:
			return fmt.Errorf("release %s in %s runs %s %s (revision %d), and this installer carries %s (revision %d): reset it with the installer of %s",
				k.Release, k.Namespace, m.Name, r.Version, r.Revision, m.ProjectVersion, m.Revision, r.Version)
		case !ok:
			return fmt.Errorf("the schema baseline of %s %s, which release %s in %s runs, cannot be told (the log says why): --new-baseline never wipes on a guess",
				m.Name, r.Version, k.Release, k.Namespace)
		case !crosses:
			return fmt.Errorf("release %s in %s runs %s %s (revision %d), at the schema baseline of %s %s: --new-baseline only crosses a baseline; `%s` moves it",
				k.Release, k.Namespace, m.Name, r.Version, r.Revision, m.Name, m.ProjectVersion, in.k8sCtl("upgrade"))
		}
	}
	// Across a baseline the new release's Secrets are written before the wipe: the secrets and
	// inputs it adds (asked as an upgrade asks), and the pull account it carries.
	var d deploy
	if other {
		if d.env, d.secret, d.changed, err = in.k8sEnv(ctx, true); err != nil {
			return err
		}
		user, token, err := in.pullAccount(cur.Registry)
		if err != nil {
			return err
		}
		if token != "" {
			host, _, _ := strings.Cut(cur.Registry, "/")
			if d.pull, err = dockerAuths(host, user, token); err != nil {
				return err
			}
			if _, err := in.readSecret(ctx, k.pullSecret()); err != nil {
				return err
			}
		}
	}
	values, err := in.helm(ctx, Cmd{Args: []string{"get", "values", k.Release, "--output", "yaml"}})
	if err != nil {
		return err
	}
	manifest, err := in.helm(ctx, Cmd{Args: []string{"get", "manifest", k.Release}})
	if err != nil {
		return err
	}
	listed, err := in.kubectl(ctx, Cmd{Args: []string{"get", "persistentvolumeclaims", "--output", "json"}})
	if err != nil {
		return err
	}
	claims, keptClaims, _, err := releaseClaims(manifest, listed, k.Namespace)
	if err != nil {
		return err
	}
	// What the hook returned answers for data the reset wipes: forgotten, as on Docker. A Secret of
	// its name another tool wrote is refused here, before anything changes.
	hook, err := in.hook()
	if err != nil {
		return err
	}
	if hook != nil {
		if _, err := in.readSecret(ctx, k.hookName()); err != nil {
			return err
		}
	}
	plan, err := in.k8sHookPlanFor(ctx, cur.Registry)
	if err != nil {
		return err
	}
	// The context the run acts on, named in the hint a failure prints: the current one may change.
	kctx := k.Context
	if kctx == "" {
		out, err := k.Kubectl.Run(ctx, Cmd{Args: []string{"config", "current-context"}})
		if err != nil {
			return fmt.Errorf("kubectl config current-context: %w", err)
		}
		kctx = strings.TrimSpace(string(out))
	}
	what := fmt.Sprintf("uninstall release %s in %s", k.Release, k.Namespace)
	if len(claims) > 0 {
		what += ", delete its PersistentVolumeClaims " + strings.Join(claims, ", ")
	}
	if len(keptClaims) > 0 {
		what += " (keeping " + strings.Join(keptClaims, ", ") + ", which its chart keeps)"
	}
	if hook != nil {
		what += fmt.Sprintf(", forget what the provision hook returned (the Secret %s)", k.hookName())
	}
	if other {
		if len(d.changed) > 0 {
			what = fmt.Sprintf("write the secrets and inputs %s %s adds (%s), ", m.Name, m.ProjectVersion, strings.Join(d.changed, ", ")) + what
		}
		what += fmt.Sprintf(", then install %s %s, at the new schema baseline, with the same values", m.Name, m.ProjectVersion)
	} else {
		what += fmt.Sprintf(", then install %s %s again with the same values", m.Name, m.ProjectVersion)
	}
	if o.DryRun {
		fmt.Fprintf(o.Out, "a run would %s\n", what)
		return nil
	}
	if err := confirmReset(ctx, o.Prompt, o.Yes, m.Name, what); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "stowage-chart-")
	if err != nil {
		return err
	}
	keepDir := false // the values' only copy when they could not be kept elsewhere
	defer func() {
		if !keepDir {
			os.RemoveAll(dir)
		}
	}()
	chart := filepath.Join(dir, "chart.tgz")
	if err := in.fetchChart(ctx, cur.Registry, chart); err != nil {
		return err
	}
	valuesFile := filepath.Join(dir, "values.yaml")
	if err := os.WriteFile(valuesFile, values, 0o600); err != nil {
		return err
	}
	timeout := k.Timeout.String()
	// From the uninstall on, the release may be gone (helm forgets it even when its wait fails): a
	// failure keeps its values in a new file where the operator finds them.
	// The install that brings it back reads the chart where the release was installed from.
	target := k
	target.Context = kctx
	again := in.R.Ctl() + " k8s install" + k8sTarget(target) + " --timeout " + timeout
	if cur.Registry != in.R.Spec.Registry.Prefix {
		again += " --image-registry " + cur.Registry
	}
	if o.RegistryCA != "" {
		again += " --registry-ca " + o.RegistryCA
	}
	kept := func(err error, left []string) error {
		file := valuesFile
		f, werr := os.CreateTemp(o.Dir, k.Release+"-values-*.yaml")
		if werr == nil {
			_, werr = f.Write(values)
			werr = errors.Join(werr, f.Close())
			if werr != nil {
				os.Remove(f.Name())
			} else {
				file = f.Name()
			}
		}
		if werr != nil {
			keepDir = true
			o.logf("the values could not be kept in %s (%v): they stay in %s", o.Dir, werr, valuesFile)
		}
		msg := fmt.Sprintf("%v\nthe release may be uninstalled; its Secrets are kept, and its values are in %s", err, file)
		if len(left) > 0 {
			msg += fmt.Sprintf("; the claims %s may still hold the data: `kubectl delete persistentvolumeclaims %s --namespace %s --context %s` first, then",
				strings.Join(left, ", "), strings.Join(left, " "), k.Namespace, shellQuote(kctx))
		} else {
			msg += ":"
		}
		return fmt.Errorf("%s `%s --values %s` installs it again", msg, again, file)
	}
	if other {
		if err := in.applySecrets(ctx, d); err != nil {
			return err
		}
	}
	o.logf("helm: uninstalling %s in %s", k.Release, k.Namespace)
	if _, err := in.helm(ctx, Cmd{Args: []string{"uninstall", k.Release, "--wait", "--timeout", timeout}, Stream: o.Log}); err != nil {
		return kept(err, claims)
	}
	if len(claims) > 0 {
		o.logf("kubectl: deleting the PersistentVolumeClaims %s", strings.Join(claims, ", "))
		args := append([]string{"delete", "persistentvolumeclaims"}, claims...)
		if _, err := in.kubectl(ctx, Cmd{Args: append(args, "--ignore-not-found", "--wait", "--timeout", timeout), Stream: o.Log}); err != nil {
			return kept(err, claims)
		}
	}
	if hook != nil {
		if _, err := in.kubectl(ctx, Cmd{Args: []string{"delete", "secret", k.hookName(), "--ignore-not-found"}}); err != nil {
			return kept(err, nil)
		}
	}
	args := helmDeploy(major, k, chart, valuesFile)
	o.logf("helm: installing %s %s (chart %s) in %s again", m.Name, m.ProjectVersion, m.Artifacts.Chart.Version, k.Namespace)
	if _, err := in.helm(ctx, Cmd{Args: args, Stream: o.Log}); err != nil {
		return kept(err, nil)
	}
	fmt.Fprintf(o.Out, "release %s in %s runs %s %s (revision %d), as first installed\n", k.Release, k.Namespace, m.Name, m.ProjectVersion, m.Revision)
	return in.k8sProvisioned(ctx, plan)
}

// releaseClaims is, among the PersistentVolumeClaims of namespace listed (`kubectl get --output
// json`), the ones a release's manifest makes there, and the ones its StatefulSets there claim
// (<template>-<set>-<ordinal>, carrying the set's selector labels, which Kubernetes gives them),
// sorted; and, apart, the ones the manifest makes with `helm.sh/resource-policy: keep`, which the
// chart keeps through an uninstall and a reset keeps too; and, among claims, the manifest's own,
// which `helm uninstall` deletes with the release (a StatefulSet's claims outlive it).
func releaseClaims(manifest, listed []byte, namespace string) (claims, kept, byHelm []string, err error) {
	type meta struct {
		Name        string            `yaml:"name"`
		Namespace   string            `yaml:"namespace"`
		Annotations map[string]string `yaml:"annotations"`
	}
	type set struct {
		prefix string
		labels map[string]string
	}
	made, keep := map[string]bool{}, map[string]bool{}
	var claimed []set
	d := yaml.NewDecoder(bytes.NewReader(manifest))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata meta   `yaml:"metadata"`
			Spec     struct {
				Selector struct {
					MatchLabels map[string]string `yaml:"matchLabels"`
				} `yaml:"selector"`
				VolumeClaimTemplates []struct {
					Metadata meta `yaml:"metadata"`
				} `yaml:"volumeClaimTemplates"`
			} `yaml:"spec"`
		}
		err := d.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("the release's manifest: %w", err)
		}
		if doc.Metadata.Namespace != "" && doc.Metadata.Namespace != namespace {
			continue
		}
		switch doc.Kind {
		case "PersistentVolumeClaim":
			if doc.Metadata.Annotations["helm.sh/resource-policy"] == "keep" {
				keep[doc.Metadata.Name] = true
			} else {
				made[doc.Metadata.Name] = true
			}
		case "StatefulSet":
			if len(doc.Spec.Selector.MatchLabels) == 0 {
				continue // its claims cannot be told from another set's
			}
			for _, t := range doc.Spec.VolumeClaimTemplates {
				claimed = append(claimed, set{t.Metadata.Name + "-" + doc.Metadata.Name + "-", doc.Spec.Selector.MatchLabels})
			}
		}
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(listed, &list); err != nil {
		return nil, nil, nil, fmt.Errorf("kubectl get persistentvolumeclaims: %w", err)
	}
	for _, it := range list.Items {
		n, labels := it.Metadata.Name, it.Metadata.Labels
		ofSet := slices.ContainsFunc(claimed, func(c set) bool {
			if !ordinal(n, c.prefix) {
				return false
			}
			for k, v := range c.labels {
				if labels[k] != v {
					return false
				}
			}
			return true
		})
		switch {
		case keep[n]:
			kept = append(kept, n)
		case made[n] || ofSet:
			claims = append(claims, n)
			if made[n] {
				byHelm = append(byHelm, n)
			}
		}
	}
	slices.Sort(claims)
	slices.Sort(kept)
	slices.Sort(byHelm)
	return claims, kept, byHelm, nil
}

// ordinal reports whether name is prefix followed by a StatefulSet ordinal.
func ordinal(name, prefix string) bool {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok || rest == "" {
		return false
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// shellQuote quotes s for a POSIX shell, unless it needs none.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:@+,") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
