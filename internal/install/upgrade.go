package install

import (
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
)

const (
	// PrevDir keeps the release an upgrade replaced — its stack files, .env, installer and
	// prevState — for `rollback`. One level: the next upgrade replaces it.
	PrevDir = ".stowage-prev"
	// FailedDir keeps the release a rollback left, for a look at what failed.
	FailedDir = ".stowage-failed"
	// SnapshotsDir holds the database dumps taken before each upgrade.
	SnapshotsDir = "snapshots"
	prevState    = "state.json"
)

// stackFiles are the files an install's release writes (writeStack): the project's compose file
// and the front.
var stackFiles = []string{"compose.yaml", FrontCompose, Caddyfile}

// state is what an upgrade records about the release it replaced.
type state struct {
	ProjectVersion string `json:"project_version"`
	Revision       int    `json:"revision"`
	// Schema is the database's migration number before the upgrade; empty when it was not read.
	Schema   string `json:"schema,omitempty"`
	Snapshot string `json:"snapshot,omitempty"` // the dump taken before the upgrade
}

// open reads the install in O.Dir.
func (in *Installer) open() (*EnvFile, error) {
	dir, err := filepath.Abs(in.O.Dir)
	if err != nil {
		return nil, err
	}
	in.O.Dir = dir
	env, err := ReadEnvFile(in.path(envName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s holds no install", dir)
	}
	return env, err
}

// ctl is the installer the install keeps.
func (in *Installer) ctl() string { return in.path(in.R.Ctl()) }

// sameProject refuses an install that is not one of the release m: another project's, or this project's
// under another installer name. The install keeps its installer as <short_name>-stow, and finds
// it again by the name of the release it runs, so short_name never changes over an install: a
// release that renames the installer is installed afresh.
// It returns which release the install runs (runs).
func sameProject(dir string, env *EnvFile, m *release.Manifest) (name, version, revision string, err error) {
	name, version, revision = runs(env)
	if name != m.Name {
		return "", "", "", fmt.Errorf("%s holds an install of %q, not %q", dir, name, m.Name)
	}
	if short, _ := env.Get("STOWAGE_SHORT_NAME"); short != m.ShortName {
		return "", "", "", fmt.Errorf("%s holds an install of %s kept by %s, not %s (short_name %q, this release's %q): run the installer it keeps, or install this release afresh", dir, name, providers.Ctl(short), providers.Ctl(m.ShortName), short, m.ShortName)
	}
	return name, version, revision, nil
}

// runs reads which release an install runs.
func runs(env *EnvFile) (name, version, revision string) {
	name, _ = env.Get("STOWAGE_NAME")
	version, _ = env.Get("STOWAGE_PROJECT_VERSION")
	revision, _ = env.Get("STOWAGE_REVISION")
	return name, version, revision
}

// openLocked takes the install's lifecycle lock, then reads the install: everything a run decides
// from is read under the lock it holds until the returned func.
func (in *Installer) openLocked(ctx context.Context) (*EnvFile, func(), error) {
	env, err := in.open()
	if err != nil {
		return nil, nil, err
	}
	unlock, err := in.lockRun(ctx, env)
	if err != nil {
		return nil, nil, err
	}
	if env, err = in.open(); err != nil {
		unlock()
		return nil, nil, err
	}
	return env, unlock, nil
}

// pinnedImages reports whether the install pulls its images by digest; one made with --offline
// runs loaded images by tag, and keeps doing so.
func pinnedImages(env *EnvFile) bool {
	front, _ := env.Get(FrontImageVar)
	return strings.Contains(front, "@sha256:")
}

// Upgrade moves an install of an earlier release of the project to the one this installer
// carries. Every check runs, and every image is present, before anything changes; then the
// database is dumped, the running release is kept in PrevDir for `rollback`, and this one is
// written and started. Recorded values are kept; what the release adds — inputs, secrets, safe
// defaults — is decided as a first install decides it.
func (in *Installer) Upgrade(ctx context.Context) error {
	o := &in.O
	if err := in.R.checkFront(); err != nil {
		return err
	}
	env, unlock, err := in.openLocked(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	m := in.R.Manifest
	name, cur, rev, err := sameProject(o.Dir, env, m)
	if err != nil {
		return err
	}
	if cur == m.ProjectVersion && rev == strconv.Itoa(m.Revision) {
		fmt.Fprintf(o.Out, "%s already runs %s %s (revision %s): `%s --dir %s` brings it up again\n", o.Dir, name, cur, rev, cmp.Or(o.Self, in.ctl()), o.Dir)
		if pinnedImages(env) {
			fmt.Fprintf(o.Out, "`%s upgrade --version V --dir %s` moves it to version V\n", in.ctl(), o.Dir)
		}
		return nil
	}
	if err := in.newer(cur, rev); err != nil {
		return err
	}
	mode, _ := env.Get("STOWAGE_TLS_MODE")
	if err := in.sameFront(env, mode); err != nil {
		return err
	}
	if _, err := in.hook(); err != nil {
		return err
	}
	if in.schemaOn() {
		if err := providers.SchemaCompat(in.R.Spec.DB().SchemaCompat, in.R.Spec.Images); err != nil {
			return fmt.Errorf("database.schema_compat: %w", err)
		}
	}
	if o.Offline && pinnedImages(env) {
		return errors.New("--offline: this install pulls its images by digest; an install made with --offline upgrades from loaded images")
	}
	o.Offline = !pinnedImages(env)
	var added []input
	for _, i := range in.inputList(mode) {
		if _, ok := env.Get(i.Key); !ok {
			added = append(added, i)
		}
	}
	if err := in.onlyAdded(env, added); err != nil {
		return err
	}
	if raw, err := os.ReadFile(filepath.Join(in.path(PrevDir), prevState)); err == nil {
		var st state
		if json.Unmarshal(raw, &st) == nil && st.ProjectVersion == cur && strconv.Itoa(st.Revision) == rev {
			return fmt.Errorf("an upgrade from or a rollback to %s %s stopped before it finished: `%s rollback --dir %s` finishes it, back on %s %s", name, cur, in.ctl(), o.Dir, name, cur)
		}
	}
	kept, err := in.keptInstaller(name, cur, rev)
	if err != nil {
		return err
	}
	if from, to, ok := baselines(o, &Installer{R: kept, O: in.O}, in); ok && from != to {
		return fmt.Errorf("%s %s starts schema baseline %d, and the install runs %s %s, at baseline %d: its data cannot carry over, so it moves to %s only by a fresh install",
			m.Name, m.ProjectVersion, to, name, cur, from, m.ProjectVersion)
	}
	if o.Self == "" {
		return errors.New("an upgrade keeps the installer it runs as the install's: its path is not known")
	}
	if err := in.checkDocker(ctx); err != nil {
		return err
	}
	if err := in.l4Upgrade(ctx, env, mode); err != nil {
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
	in.dropped = removedBy(kept, in.R)
	for _, k := range in.dropped.inputs {
		if _, ok := env.Get(k); ok {
			o.logf("input %s is not in %s %s: dropped", k, name, m.ProjectVersion)
		}
	}
	next, secrets, err := in.upgradeEnv(env, mode, added, values)
	if err != nil {
		return err
	}
	if o.DryRun {
		for _, k := range next.Keys() {
			v, _ := next.Get(k)
			if old, ok := env.Get(k); ok && old == v {
				continue
			}
			if secrets[k] && v != "" {
				v = "<redacted>"
			}
			fmt.Fprintf(o.Out, "%s=%s\n", k, v)
		}
		if in.R.Spec.HasDatabase() {
			o.logf("a run would dump the database to %s, keep %s %s in %s, then start %s", in.path(SnapshotsDir), name, cur, in.path(PrevDir), m.ProjectVersion)
		} else {
			o.logf("a run would keep %s %s in %s, then start %s", name, cur, in.path(PrevDir), m.ProjectVersion)
		}
		return nil
	}
	if err := in.fetchImages(ctx, next); err != nil {
		return err
	}
	st := state{ProjectVersion: cur}
	st.Revision, _ = strconv.Atoi(rev)
	if in.schemaOn() {
		st.Schema, err = (schema{in: in, stack: env}).current(ctx, next)
		if ctx.Err() == nil && err != nil {
			o.logf("the schema version could not be read (%v): a rollback will not be checked against it", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("stopped before anything changed: %w", err)
	}
	if st.Snapshot, err = in.snapshot(ctx, env); err != nil {
		return err
	}
	if err := (&Installer{R: kept, O: in.O}).keepPrev(env, st); err != nil {
		return err
	}
	// .env is the switch: before it the install still runs the release it ran (a re-run of its
	// installer rewrites the stack files), after it this release.
	if err := in.writeStack(next); err != nil {
		return err
	}
	if err := next.WriteFile(in.path(envName)); err != nil {
		return err
	}
	in.forgetChanges()
	if err := in.copySelf(); err != nil {
		return fmt.Errorf("the installer could not be kept (%w)\nthe install runs %s now: `%s --dir %s` keeps it and starts it", err, m.ProjectVersion, o.Self, o.Dir)
	}
	if err := in.up(ctx, next); err != nil {
		return fmt.Errorf("%w\n%s %s is kept: `%s rollback --dir %s` goes back to it", err, name, cur, in.ctl(), o.Dir)
	}
	h, err := in.hook()
	if err != nil {
		return err
	}
	if err := in.provisionUp(ctx, next, h); err != nil {
		return err
	}
	in.installDone() // a first install that stopped is finished by an upgrade whose stack came up
	in.upgraded = true
	return nil
}

// keptInstaller returns the release the install's kept installer carries, and refuses one that is
// not the release it runs: kept with it in PrevDir, that installer is what a rollback goes back
// with.
func (in *Installer) keptInstaller(name, version, revision string) (*Release, error) {
	files, err := payload.Read(in.ctl())
	if err == nil {
		var r *Release
		if r, err = Load(files); err == nil {
			m := r.Manifest
			if m.Name == name && m.ProjectVersion == version && strconv.Itoa(m.Revision) == revision {
				return r, nil
			}
			err = fmt.Errorf("it carries %s %s (revision %d)", m.Name, m.ProjectVersion, m.Revision)
		}
	}
	return nil, fmt.Errorf("%s is not the installer of %s %s (revision %s), so a rollback could not go back: re-run that release's installer in %s first (%w)",
		in.ctl(), name, version, revision, in.O.Dir, err)
}

// newer refuses to upgrade to a release that is not after the one the install runs, ordered by
// the scheme the release records. A `command` scheme is the project's own program, which an
// install does not run: the order is then not checked.
func (in *Installer) newer(cur, rev string) error {
	m := in.R.Manifest
	if m.Scheme.Provider == "command" {
		in.O.logf("%s orders its versions with its own command, which an install does not run: not checking %s comes after %s", m.Name, m.ProjectVersion, cur)
		return nil
	}
	r, err := strconv.Atoi(rev)
	if err != nil {
		return fmt.Errorf("the install's revision %q is not a number", rev)
	}
	c, err := compareTo(m, cur)
	if err != nil {
		return err
	}
	if cmp.Or(c, cmp.Compare(r, m.Revision)) > 0 {
		return fmt.Errorf("%s runs %s %s (revision %d), after this installer's %s (revision %d): `%s rollback --dir %s` goes back one upgrade",
			in.O.Dir, m.Name, cur, r, m.ProjectVersion, m.Revision, in.ctl(), in.O.Dir)
	}
	return nil
}

// sameFront refuses a release whose front the install cannot keep: a TLS mode it no longer offers,
// a route the install does not publish, a route's domain under an input the install does not
// record.
func (in *Installer) sameFront(env *EnvFile, mode string) error {
	m := in.R.Manifest
	if !slices.Contains(in.R.Spec.Front.TLSModes, mode) {
		return fmt.Errorf("%s %s does not offer the TLS mode this install uses (%s)", m.Name, m.ProjectVersion, mode)
	}
	for _, r := range in.R.Spec.Front.Routes {
		if _, ok := env.Get(spec.URLVar(r.Name)); !ok {
			return fmt.Errorf("%s %s adds the route %s, which this install does not publish: a new route needs a new install", m.Name, m.ProjectVersion, r.Name)
		}
		if _, ok := env.Get(r.DomainInput); mode != Loopback && !ok {
			return fmt.Errorf("%s %s takes the domain of %s from %s, which this install does not record", m.Name, m.ProjectVersion, r.Name, r.DomainInput)
		}
	}
	return nil
}

// onlyAdded refuses a --set or answers key that is not an input the release adds: an upgrade keeps
// every recorded value.
func (in *Installer) onlyAdded(env *EnvFile, added []input) error {
	var recorded, unknown []string
	for _, m := range []map[string]string{in.O.Set, in.O.Answers} {
		for _, k := range sortedKeys(m) {
			_, rec := env.Get(k)
			switch {
			case slices.ContainsFunc(added, func(i input) bool { return i.Key == k }):
			case rec && !slices.Contains(recorded, k):
				recorded = append(recorded, k)
			case !rec && !slices.Contains(unknown, k):
				unknown = append(unknown, k)
			}
		}
	}
	if len(recorded) > 0 {
		return fmt.Errorf("%s: recorded at the install, and an upgrade keeps it (only an input the release adds is set)", strings.Join(recorded, ", "))
	}
	if len(unknown) > 0 {
		var keys []string
		for _, i := range added {
			keys = append(keys, i.Key)
		}
		return fmt.Errorf("no input this release adds is named %s (it adds: %s)", strings.Join(unknown, ", "), cmp.Or(strings.Join(keys, ", "), "none"))
	}
	return nil
}

// upgradeEnv is env with what the release adds and its own values: version, revision and images.
// It returns which keys hold secrets.
func (in *Installer) upgradeEnv(env *EnvFile, mode string, added []input, values map[string]string) (*EnvFile, map[string]bool, error) {
	s := in.R.Spec
	next := NewEnvFile()
	for _, k := range env.Keys() {
		if slices.Contains(in.dropped.inputs, k) {
			continue
		}
		v, _ := env.Get(k)
		next.Set(k, v)
	}
	secrets := map[string]bool{}
	for _, i := range added {
		if v, ok := values[i.Key]; ok || !i.Advanced { // an advanced input only when given
			next.Set(i.Key, v)
		}
		secrets[i.Key] = i.Secret
	}
	for _, sec := range in.secrets() {
		if _, ok := next.Get(sec.Key); ok {
			continue
		}
		v, err := generate(in.O.rand(), sec.Gen)
		if err != nil {
			return nil, nil, fmt.Errorf("secret %s: %w", sec.Key, err)
		}
		next.Set(sec.Key, v)
		secrets[sec.Key] = true
	}
	rotationVars(in.secrets(), func(k string) bool { _, ok := next.Get(k); return ok }, func(k, v string) { next.Set(k, v); secrets[k] = true })
	for _, k := range sortedKeys(s.SafeDefaults) {
		if _, ok := next.Get(k); ok {
			continue
		}
		v := s.SafeDefaults[k]
		if relaxed, ok := s.Front.LoopbackEnv[k]; ok && mode == Loopback {
			v = relaxed
		}
		next.Set(k, v)
	}
	next.Set("STOWAGE_PROJECT_VERSION", in.R.Manifest.ProjectVersion)
	next.Set("STOWAGE_REVISION", strconv.Itoa(in.R.Manifest.Revision))
	if _, ok := next.Get(IPv6Var); !ok {
		next.Set(IPv6Var, "false") // an install made before --ipv6
	}
	prefix, _ := env.Get("STOWAGE_REGISTRY")
	next.Set(FrontImageVar, in.frontImage(prefix))
	for _, k := range s.ImageNames() {
		next.Set(ImageVar(k), in.imageRef(prefix, k))
	}
	in.profilesVar(next)
	in.l4Vars(next)
	return next, secrets, next.Check()
}

// snapshot dumps the database of the running stack into SnapshotsDir, readable by the operator
// only, and returns the file; "" when the release has no database. A dump that fails leaves no
// file.
func (in *Installer) snapshot(ctx context.Context, env *EnvFile) (string, error) {
	d := in.R.Spec.DB()
	if d.Service == "" {
		return "", nil
	}
	dir := in.path(SnapshotsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	final := filepath.Join(dir, time.Now().UTC().Format("2006-01-02T15-04-05Z")+".sql.gz")
	in.O.logf("dumping the database to %s", final)
	f, err := os.CreateTemp(dir, ".partial-")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	gz := gzip.NewWriter(f)
	_, err = in.O.Docker.Run(ctx, Cmd{
		Args:   append(in.composeBase(), "exec", "-T", d.Service, "pg_dump", "-U", d.Roles.Migrate, d.Name),
		Env:    env.Environ(in.O.environ()),
		Stdout: gz,
	})
	err = errors.Join(err, gz.Close(), f.Sync(), f.Close())
	if err == nil {
		err = os.Rename(f.Name(), final)
	}
	if err != nil {
		return "", fmt.Errorf("the database could not be dumped, so nothing was changed: %w", err)
	}
	return final, nil
}

// keepPrev keeps the running release — in.R, from the kept installer — in PrevDir, replacing the
// one kept before. Its stack files are rendered from that release, not copied from the install,
// whose files an interrupted upgrade may have left at another release.
func (in *Installer) keepPrev(env *EnvFile, st state) error {
	tmp := in.path(PrevDir + ".new")
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return err
	}
	if err := in.renderStack(env, tmp); err != nil {
		return err
	}
	if err := copyFile(in.ctl(), filepath.Join(tmp, in.R.Ctl()), 0o755); err != nil {
		return err
	}
	if err := env.WriteFile(filepath.Join(tmp, envName)); err != nil {
		return err
	}
	// What the hook returned, which a rollback takes back what the upgrade dropped from.
	if err := copyFile(in.path(ProvisionEnv), filepath.Join(tmp, ProvisionEnv), 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, prevState), append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.RemoveAll(in.path(PrevDir)); err != nil {
		return err
	}
	return os.Rename(tmp, in.path(PrevDir))
}

// copyFile replaces dst with a copy of src and mode, atomically.
func copyFile(src, dst string, mode os.FileMode) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	return writeAtomic(dst, f, mode, nil)
}

// restoreReturned takes back, from what the hook returned when the previous release ran (kept),
// the keys the hook's record cur lacks: what an upgrade dropped. Nothing is kept: nothing to do.
func restoreReturned(kept, cur string) error {
	prev, err := ReadEnvFile(kept)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	now, err := ReadEnvFile(cur)
	if errors.Is(err, os.ErrNotExist) {
		now, err = NewEnvFile(), nil
	}
	if err != nil {
		return err
	}
	changed := false
	for _, k := range prev.Keys() {
		if _, ok := now.Get(k); !ok {
			v, _ := prev.Get(k)
			now.Set(k, v)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return now.WriteFile(cur)
}

// releaseKeys are the recorded values a release sets: a rollback takes them back from the
// previous release and keeps every other value, so a secret an upgrade added is the same when
// the release that uses it comes back.
func releaseKey(k string) bool {
	switch k {
	case "STOWAGE_PROJECT_VERSION", "STOWAGE_REVISION", FrontImageVar:
		return true
	}
	return strings.HasPrefix(k, "STOWAGE_IMAGE_")
}

// Rollback goes back to the release the last upgrade replaced. It is run by the installer of the
// release the install runs, and refuses — before anything changes — when the database moved past
// what the previous release reads: a rollback barrier since, or a previous release that does not
// read a newer schema. A check that cannot run does not refuse.
func (in *Installer) Rollback(ctx context.Context) error {
	o := &in.O
	env, unlock, err := in.openLocked(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	m := in.R.Manifest
	name, cur, rev, err := sameProject(o.Dir, env, m)
	if err != nil {
		return err
	}
	prev := in.path(PrevDir)
	raw, err := os.ReadFile(filepath.Join(prev, prevState))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s has no previous release to go back to (an upgrade keeps one)", o.Dir)
	}
	if err != nil {
		return err
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("%s: %w", filepath.Join(prev, prevState), err)
	}
	prevEnv, err := ReadEnvFile(filepath.Join(prev, envName))
	if err != nil {
		return err
	}
	files, err := payload.Read(filepath.Join(prev, in.R.Ctl()))
	if err != nil {
		return fmt.Errorf("the previous release's installer: %w", err)
	}
	pr, err := Load(files)
	if err != nil {
		return fmt.Errorf("the previous release's installer: %w", err)
	}
	pname, pversion, prevRev := runs(prevEnv)
	if pm := pr.Manifest; pm.Name != name || pm.ProjectVersion != st.ProjectVersion || pm.Revision != st.Revision ||
		pname != name || pversion != st.ProjectVersion || prevRev != strconv.Itoa(st.Revision) {
		return fmt.Errorf("%s does not hold one release: its installer carries %s %s (revision %d), its record says %s (revision %d), its .env %s %s (revision %s)",
			prev, pm.Name, pm.ProjectVersion, pm.Revision, st.ProjectVersion, st.Revision, pname, pversion, prevRev)
	}
	if cur == st.ProjectVersion && rev == strconv.Itoa(st.Revision) {
		// A rollback that stopped after .env switched, or an upgrade that stopped before it (its
		// stack files may already be the next release's): the stack files and the installer
		// PrevDir keeps, then the start.
		if o.DryRun {
			o.logf("a run would finish the upgrade from or the rollback to %s %s that stopped", name, cur)
			return nil
		}
		o.logf("finishing the upgrade from or the rollback to %s %s that stopped, back on it", name, cur)
		for _, f := range stackFiles {
			if err := copyFile(filepath.Join(prev, f), in.path(f), 0o644); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := copyFile(filepath.Join(prev, in.R.Ctl()), in.ctl(), 0o755); err != nil {
			return err
		}
		if err := os.RemoveAll(prev); err != nil {
			return err
		}
		return (&Installer{R: pr, O: in.O}).up(ctx, env)
	}
	if cur != m.ProjectVersion || rev != strconv.Itoa(m.Revision) {
		return fmt.Errorf("%s runs %s %s (revision %s), and this installer carries %s (revision %d): a rollback is run by the installer the install keeps, `%s rollback --dir %s`",
			o.Dir, name, cur, rev, m.ProjectVersion, m.Revision, in.ctl(), o.Dir)
	}
	o.Offline = o.Offline || !pinnedImages(prevEnv)
	back := &Installer{R: pr, O: in.O}
	rolled := NewEnvFile()
	for _, k := range env.Keys() {
		v, _ := env.Get(k)
		if pv, ok := prevEnv.Get(k); ok && releaseKey(k) {
			v = pv
		}
		rolled.Set(k, v)
	}
	for _, k := range prevEnv.Keys() {
		if _, ok := rolled.Get(k); !ok {
			v, _ := prevEnv.Get(k)
			rolled.Set(k, v)
		}
	}
	if err := in.baselineCheck(back, st); err != nil {
		return err
	}
	if err := in.checkDocker(ctx); err != nil {
		return err
	}
	// A dry run pulls them only when the check may read them.
	if !o.DryRun || in.schemaOn() && back.schemaOn() {
		if err := back.fetchImages(ctx, rolled); err != nil {
			return err
		}
	}
	if err := in.rollbackCheck(ctx, back, env, prevEnv, st); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("stopped before anything changed: %w", err)
	}
	if o.DryRun {
		o.logf("a run would go back to %s %s (revision %d)", pr.Manifest.Name, st.ProjectVersion, st.Revision)
		return nil
	}
	failed := in.path(FailedDir)
	if err := os.RemoveAll(failed); err != nil {
		return err
	}
	if err := os.Mkdir(failed, 0o700); err != nil {
		return err
	}
	for _, f := range append(slices.Clone(stackFiles), envName, in.R.Ctl()) {
		mode := os.FileMode(0o644)
		switch f {
		case envName:
			mode = 0o600
		case in.R.Ctl():
			mode = 0o755
		}
		if err := copyFile(in.path(f), filepath.Join(failed, f), mode); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	// As an upgrade: stack files, then .env (the switch), then the installer; PrevDir stays whole
	// until the end, so an interrupted rollback can run again.
	for _, f := range stackFiles {
		if err := copyFile(filepath.Join(prev, f), in.path(f), 0o644); err != nil {
			return err
		}
	}
	if err := rolled.WriteFile(in.path(envName)); err != nil {
		return err
	}
	if err := restoreReturned(filepath.Join(prev, ProvisionEnv), in.path(ProvisionEnv)); err != nil {
		return err
	}
	in.forgetChanges()
	if err := copyFile(filepath.Join(prev, in.R.Ctl()), in.ctl(), 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(prev); err != nil {
		return err
	}
	if err := back.up(ctx, rolled); err != nil {
		return fmt.Errorf("%w\nthe release left is kept in %s", err, failed)
	}
	return nil
}

// rollbackRefused is the rollback check's refusal: the database moved past what the previous
// release reads, so going back would leave the install unable to start.
type rollbackRefused string

func (e rollbackRefused) Error() string { return string(e) }

// rollbackCheck refuses to go back when the database moved past what the previous release reads.
// The installed release's provider reads the database; whether the previous release reads a newer
// schema is the previous release's own provider's answer (back).
func (in *Installer) rollbackCheck(ctx context.Context, back *Installer, env, prevEnv *EnvFile, st state) error {
	if !in.schemaOn() {
		return nil
	}
	unchecked := func(why string, a ...any) error {
		in.O.logf("going back unchecked: "+why, a...)
		return nil
	}
	if st.Schema == "" {
		return unchecked("the schema version before the upgrade was not recorded")
	}
	if !schemaVersion.MatchString(st.Schema) {
		return unchecked("the schema version recorded before the upgrade, %q, is not a migration number", st.Schema)
	}
	s := schema{in: in, stack: env}
	now, err := s.current(ctx, env)
	if err != nil {
		return unchecked("the schema version could not be read (%v)", err)
	}
	if !laterSchema(now, st.Schema) {
		return nil
	}
	refuse := func(why string) error {
		var b strings.Builder
		fmt.Fprintf(&b, "refusing: %s; going back would leave the install unable to start.", why)
		if st.Snapshot != "" {
			fmt.Fprintf(&b, "\nRestore from the snapshot taken before the upgrade instead: %s", st.Snapshot)
		}
		return rollbackRefused(b.String())
	}
	barriers, err := s.barriers(ctx, st.Schema)
	if err != nil {
		in.O.logf("the rollback barriers could not be read (%v)", err)
	} else if barriers != "" {
		return refuse(fmt.Sprintf("the database carries %s, which removed something %s may still use", barriers, st.ProjectVersion))
	}
	if !back.schemaOn() {
		return unchecked("%s declares no schema compatibility", st.ProjectVersion)
	}
	reads, err := (schema{in: back, stack: env}).readsNewer(ctx, prevEnv, now)
	if err != nil {
		return unchecked("whether %s reads the newer schema %s is not known (%v)", st.ProjectVersion, now, err)
	}
	if !reads {
		return refuse(fmt.Sprintf("the database is at %s, and %s predates reading a newer schema", now, st.ProjectVersion))
	}
	in.O.logf("the database is at %s; %s reads past the newer migrations", now, st.ProjectVersion)
	return nil
}

// baseline is the schema baseline the release declares (providers.SchemaBaseline); 0 for a release
// without a database, which has no schema to share.
func (in *Installer) baseline() (int, error) {
	if !in.R.Spec.HasDatabase() {
		return 0, nil
	}
	return providers.SchemaBaseline(in.R.Spec.DB().SchemaCompat)
}

// baselines are the schema baselines of the releases from (the install's) and to; ok is false
// when either has no database or its baseline does not read (said in the log): the check then
// does not refuse.
func baselines(o *Options, from, to *Installer) (int, int, bool) {
	a, err := from.baseline()
	if err != nil {
		o.logf("the schema baseline of %s is not known (%v): not checked", from.R.Manifest.ProjectVersion, err)
		return 0, 0, false
	}
	b, err := to.baseline()
	if err != nil {
		o.logf("the schema baseline of %s is not known (%v): not checked", to.R.Manifest.ProjectVersion, err)
		return 0, 0, false
	}
	return a, b, a != 0 && b != 0
}

// baselineCheck refuses to go back to the release back when it starts another schema baseline
// than the installed one: decided from the two specs alone, before Docker is asked anything.
func (in *Installer) baselineCheck(back *Installer, st state) error {
	from, to, ok := baselines(&in.O, in, back)
	if !ok || from == to {
		return nil
	}
	return rollbackRefused(fmt.Sprintf("refusing: the database is at schema baseline %d, and %s starts baseline %d, which shares no schema with it; going back would leave the install unable to start, and takes a fresh install of %s.",
		from, st.ProjectVersion, to, st.ProjectVersion))
}
