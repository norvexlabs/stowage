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
	"sync"
	"time"

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
	// crossedFile records the releases a reset-db --new-baseline crossed from and to ("<from
	// version> <revision> <to version> <revision>"): the install keeps no release to go back to;
	// the next upgrade removes it.
	crossedFile = ".stowage-crossed"
)

// stackFiles are the files an install's release writes (writeStack): the project's compose file,
// the front, and the content labels of that compose file's services.
var stackFiles = []string{"compose.yaml", FrontCompose, Caddyfile, ContentCompose}

// restoreStack copies the stack files dir keeps over the install's; missingOK skips one dir lacks.
// The content labels go with the compose file they were written for: where dir keeps none, the
// install's are removed, never left naming services the restored compose file may not have.
func (in *Installer) restoreStack(dir string, missingOK bool) error {
	for _, f := range stackFiles {
		err := copyFile(filepath.Join(dir, f), in.path(f), 0o644)
		switch {
		case err == nil:
		case errors.Is(err, os.ErrNotExist) && f == ContentCompose:
			if err := os.Remove(in.path(f)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		case errors.Is(err, os.ErrNotExist) && missingOK:
		default:
			return err
		}
	}
	return nil
}

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
// runs loaded images by tag, and keeps doing so. Every reference recorded is one or the other;
// the first found says which, and with none recorded the install pulls.
func pinnedImages(env *EnvFile) bool {
	for _, k := range env.Keys() {
		if k == FrontImageVar || k == ChownImageVar || strings.HasPrefix(k, "STOWAGE_IMAGE_") {
			v, _ := env.Get(k)
			return strings.Contains(v, "@sha256:")
		}
	}
	return true
}

// Upgrade moves an install of an earlier release of the project to the one this installer
// carries. Every check runs, and every image is present, before anything changes; then the
// database is dumped, the running release is kept in PrevDir for `rollback`, and this one is
// written and started. Recorded values are kept; what the release adds — inputs, secrets, safe
// defaults — is decided as a first install decides it.
//
// Across a schema baseline it refuses, unless in.crossing (reset-db --new-baseline) names the
// release the install runs: the data is then wiped (wipeData) where the database would be dumped,
// and no release is kept to go back to.
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
	if c := in.crossing; c != nil && (c.version != cur || c.revision != rev) {
		return fmt.Errorf("%s runs %s %s (revision %s) now, not %s (revision %s), which was confirmed: nothing was wiped; reset-db --new-baseline again", o.Dir, name, cur, rev, c.version, c.revision)
	}
	if err := in.newer(cur, rev); err != nil {
		return err
	}
	mode, pub, added, kept, err := in.upgradeRefusals(ctx, env, name, cur, rev)
	if err != nil {
		return err
	}
	o.Offline = !pinnedImages(env)
	in.recorded = map[string]string{}
	for _, k := range env.Keys() {
		in.recorded[k], _ = env.Get(k)
	}
	values, err := in.inputs(ctx, added, false)
	if err != nil {
		return err
	}
	if kept.Release != nil {
		in.dropped = removedBy(kept.Release, in.R)
	} else {
		o.logf("the inputs and provision hook returns %s %s removes are not known (%s): kept", name, m.ProjectVersion, kept.foreign())
	}
	for _, k := range in.dropped.inputs {
		if _, ok := env.Get(k); ok {
			o.logf("input %s is not in %s %s: dropped", k, name, m.ProjectVersion)
		}
	}
	next, secrets, err := in.upgradeEnv(env, mode, added, values)
	if err != nil {
		return err
	}
	if err := in.upgradeRoutes(env, next, mode, pub); err != nil {
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
	if in.schemaOn() && in.crossing == nil {
		st.Schema, err = (schema{in: in, stack: env}).current(ctx, next)
		if ctx.Err() == nil && err != nil {
			o.logf("the schema version could not be read (%v): a rollback will not be checked against it", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("stopped before anything changed: %w", err)
	}
	if in.crossing != nil {
		// No release is kept to go back to, nor one left by a rollback: across a baseline neither
		// reads the data.
		if err := in.wipeData(ctx, env, in.crossing.old); err != nil {
			return err
		}
		for _, d := range []string{PrevDir, FailedDir} {
			if err := os.RemoveAll(in.path(d)); err != nil {
				return err
			}
		}
	} else {
		if st.Snapshot, err = in.snapshot(ctx, env); err != nil {
			return err
		}
		if err := in.keepPrev(env, st, kept); err != nil {
			return err
		}
	}
	// .env is the switch: before it the install still runs the release it ran (a re-run of its
	// installer rewrites the stack files), after it this release.
	if err := in.writeStack(next); err != nil {
		return err
	}
	// A crossing's releases are recorded before the switch, the record dropped by another
	// upgrade after it: a record naming releases the install does not run means nothing.
	if in.crossing != nil {
		if err := writeAtomic(in.path(crossedFile), strings.NewReader(fmt.Sprintf("%s %s %s %d\n", cur, rev, m.ProjectVersion, m.Revision)), 0o600, nil); err != nil {
			return err
		}
	}
	if err := next.WriteFile(in.path(envName)); err != nil {
		return err
	}
	if in.crossing == nil {
		if err := os.Remove(in.path(crossedFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			o.logf("warning: removing %s: %v", in.path(crossedFile), err)
		}
	}
	in.forgetChanges()
	if err := in.copySelf(); err != nil {
		return fmt.Errorf("the installer could not be kept (%w)\nthe install runs %s now: `%s --dir %s` keeps it and starts it", err, m.ProjectVersion, o.Self, o.Dir)
	}
	if err := in.up(ctx, next); err != nil {
		if in.crossing != nil {
			return fmt.Errorf("%w\nthe install runs %s %s, its data wiped: `%s --dir %s` starts it again", err, name, m.ProjectVersion, in.ctl(), o.Dir)
		}
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

// upgradeRefusals runs what an upgrade of the install env records (name cur at revision rev) to
// this release refuses before it asks or changes anything: its front, ports, hook and schema
// compat (upgradeFits, returning the TLS mode and the ports), the inputs it adds (returned), an
// upgrade or rollback that stopped, the release the install runs (its kept installer, returned)
// and the schema baseline — crossed only with in.crossing, whose old release it reads (wipeOf)
// each time, under whatever lock its caller holds —, Docker and the L4 routes' ports. It writes
// nothing but in.crossing.old.
func (in *Installer) upgradeRefusals(ctx context.Context, env *EnvFile, name, cur, rev string) (mode string, pub publish, added []input, kept *keptRelease, err error) {
	o, m := &in.O, in.R.Manifest
	mode, _ = env.Get("STOWAGE_TLS_MODE")
	pub, err = in.upgradeFits(env, mode)
	if err != nil {
		return "", publish{}, nil, nil, err
	}
	if o.Offline && pinnedImages(env) {
		return "", publish{}, nil, nil, errors.New("--offline: this install pulls its images by digest; an install made with --offline upgrades from loaded images")
	}
	added = in.addedInputs(env, mode)
	if err := in.onlyAdded(env, added); err != nil {
		return "", publish{}, nil, nil, err
	}
	if raw, err := os.ReadFile(filepath.Join(in.path(PrevDir), prevState)); err == nil {
		var st state
		if json.Unmarshal(raw, &st) == nil && st.ProjectVersion == cur && strconv.Itoa(st.Revision) == rev {
			return "", publish{}, nil, nil, fmt.Errorf("an upgrade from or a rollback to %s %s stopped before it finished: `%s rollback --dir %s` finishes it, back on %s %s", name, cur, in.ctl(), o.Dir, name, cur)
		}
	}
	kept, err = in.keptInstaller(ctx, name, cur, rev)
	if err != nil {
		return "", publish{}, nil, nil, err
	}
	if in.crossing != nil {
		if err := in.crosses(kept, name, cur); err != nil {
			return "", publish{}, nil, nil, err
		}
		if in.crossing.old, err = in.wipeOf(kept); err != nil {
			return "", publish{}, nil, nil, err
		}
		if wipe, _, _, _, err := in.wipeSet(in.crossing.old); err != nil || len(wipe) == 0 {
			return "", publish{}, nil, nil, cmp.Or(err, fmt.Errorf("neither %s %s nor %s %s names a volume in data.reset: --new-baseline has nothing to wipe, and the new release would start on the old baseline's data", name, cur, name, m.ProjectVersion))
		}
	} else if from, to, ok := baselines(o, kept, in); ok && from != to {
		return "", publish{}, nil, nil, fmt.Errorf("%s %s starts schema baseline %d, and the install runs %s %s, at baseline %d: its data cannot carry over, so it moves to %s only with its data wiped: `%s reset-db --new-baseline --dir %s` wipes it and installs %s",
			m.Name, m.ProjectVersion, to, name, cur, from, m.ProjectVersion, cmp.Or(o.Self, in.ctl()), o.Dir, m.ProjectVersion)
	}
	if o.Self == "" {
		return "", publish{}, nil, nil, errors.New("an upgrade keeps the installer it runs as the install's: its path is not known")
	}
	if err := in.volumesSplit(); err != nil {
		return "", publish{}, nil, nil, err
	}
	if err := in.checkDocker(ctx); err != nil {
		return "", publish{}, nil, nil, err
	}
	if err := in.l4Upgrade(ctx, env, mode); err != nil {
		return "", publish{}, nil, nil, err
	}
	return mode, pub, added, kept, nil
}

// upgradeFits refuses a release the install cannot move to as it stands, before anything is asked:
// its front (sameFront) and where it listens (upgradePorts, returned), its provision hook, its
// database.schema_compat.
func (in *Installer) upgradeFits(env *EnvFile, mode string) (publish, error) {
	if err := in.sameFront(env, mode); err != nil {
		return publish{}, err
	}
	pub, err := in.upgradePorts(env, mode)
	if err != nil {
		return publish{}, err
	}
	if _, err := in.hook(); err != nil {
		return publish{}, err
	}
	if in.schemaOn() {
		if err := providers.SchemaCompat(in.R.Spec.DB().SchemaCompat, in.R.Spec.Images); err != nil {
			return publish{}, fmt.Errorf("database.schema_compat: %w", err)
		}
	}
	return pub, nil
}

// addedInputs are the inputs of the release in this mode the install does not record.
func (in *Installer) addedInputs(env *EnvFile, mode string) []input {
	var added []input
	for _, i := range in.inputList(mode) {
		if _, ok := env.Get(i.Key); !ok {
			added = append(added, i)
		}
	}
	return added
}

// keptInstaller returns the release the install's kept installer carries (readKept), and refuses
// one that is not the release it runs: kept with it in PrevDir, that installer is what a rollback
// goes back with.
func (in *Installer) keptInstaller(ctx context.Context, name, version, revision string) (*keptRelease, error) {
	k, err := in.readKept(ctx, in.ctl(), func(m *release.Manifest) error {
		if m.Name == name && m.ProjectVersion == version && strconv.Itoa(m.Revision) == revision {
			return nil
		}
		return fmt.Errorf("it carries %s %s (revision %d)", m.Name, m.ProjectVersion, m.Revision)
	})
	if err == nil {
		return k, nil
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

// sameFront refuses a release whose front the install cannot keep: a front where the install has
// none or none where it has one, a TLS mode it no longer offers, a route's domain under an input
// the install records empty, or one it does not record for a route the install publishes, routes
// the recorded domains make clash (routeDomains). A route the release adds is decided as a first
// install decides it: on its domain input's recorded value, or on the value given to that input
// the release adds (upgradeRoutes); in loopback mode on its own port (upgradePorts).
func (in *Installer) sameFront(env *EnvFile, mode string) error {
	m := in.R.Manifest
	_, served := env.Get("STOWAGE_TLS_MODE") // recorded by an install with a front
	switch {
	case !in.R.Spec.HasFront() && served:
		return fmt.Errorf("%s %s has no front, and this install serves through one: a release without a front needs a new install", m.Name, m.ProjectVersion)
	case !in.R.Spec.HasFront():
		return nil
	case !served:
		return fmt.Errorf("%s %s serves through a front, and this install has none: a front needs a new install", m.Name, m.ProjectVersion)
	}
	if !slices.Contains(in.R.Spec.Front.TLSModes, mode) {
		return fmt.Errorf("%s %s does not offer the TLS mode this install uses (%s)", m.Name, m.ProjectVersion, mode)
	}
	if mode == Loopback {
		return nil
	}
	unrecorded := false
	for _, r := range in.R.Spec.Front.Routes {
		v, recorded := env.Get(r.DomainInput)
		_, published := env.Get(spec.URLVar(r.Name))
		switch {
		case !recorded && !published:
			unrecorded = true // an input the release adds: given with the others
		case v == "":
			return fmt.Errorf("%s %s takes the domain of %s from %s, which this install does not record (or records empty): give it to a re-run of the installer the install runs, `%s --dir %s --set %s=<domain>`, then upgrade",
				m.Name, m.ProjectVersion, r.Name, r.DomainInput, in.ctl(), in.O.Dir, r.DomainInput)
		}
	}
	if unrecorded {
		return nil // the routes' domains are checked once given (upgradeRoutes)
	}
	_, err := in.routeDomains(mode, func(k string) string { v, _ := env.Get(k); return v })
	if err != nil {
		return fmt.Errorf("%s %s: %w; give the routes their domains with a re-run of the installer the install runs, `%s --dir %s`, then upgrade", m.Name, m.ProjectVersion, err, in.ctl(), in.O.Dir)
	}
	return nil
}

// upgradePorts is where the upgraded install's front listens: in a TLS mode the recorded HTTP and
// HTTPS ports; in loopback mode the recorded address, each route the install publishes on its
// recorded port, and each route the release adds on its port from --ports, else the release's
// front.loopback_ports — one no other route has, no L4 route publishes, and free on the host.
// --ports names only a route the release adds.
func (in *Installer) upgradePorts(env *EnvFile, mode string) (publish, error) {
	s, o, m := in.R.Spec, &in.O, in.R.Manifest
	get := func(k string) string { v, _ := env.Get(k); return v }
	port := func(k string) int { p, _ := strconv.Atoi(get(k)); return p }
	switch {
	case !s.HasFront() && len(o.Ports) > 0:
		return publish{}, fmt.Errorf("--ports: %s %s has no front", m.Name, m.ProjectVersion)
	case !s.HasFront():
		return publish{}, nil
	case mode != Loopback && len(o.Ports) > 0:
		return publish{}, fmt.Errorf("--ports is for loopback mode: this install's front serves %s, on its recorded ports", mode)
	case mode != Loopback:
		return publish{addr: "127.0.0.1", all: true, ports: map[string]int{"http": port(HTTPPortVar), "https": port(HTTPSPortVar)}}, nil
	}
	pub := publish{addr: get("STOWAGE_LOOPBACK_ADDRESS"), ports: map[string]int{}}
	added := publish{addr: pub.addr, ports: map[string]int{}}
	for _, r := range s.Front.Routes {
		if _, ok := env.Get(spec.URLVar(r.Name)); ok {
			if _, given := o.Ports[r.Name]; given {
				return publish{}, fmt.Errorf("--ports: the route %s is recorded at the install, and an upgrade keeps its port (only a route the release adds is given one)", r.Name)
			}
			pub.ports[r.Name] = port(PortVar(r.Name))
			continue
		}
		p, ok := o.Ports[r.Name]
		if !ok {
			p, ok = s.Front.LoopbackPorts[r.Name]
		}
		if !ok {
			return publish{}, fmt.Errorf("%s %s adds the route %s, which has no loopback port: give it one with --ports %s=PORT", m.Name, m.ProjectVersion, r.Name, r.Name)
		}
		if p < 1 || p > 65535 {
			return publish{}, fmt.Errorf("--ports: %s=%d is not a TCP port", r.Name, p)
		}
		pub.ports[r.Name], added.ports[r.Name] = p, p
	}
	for _, k := range sortedKeys(o.Ports) {
		if _, ok := pub.ports[k]; !ok {
			return publish{}, fmt.Errorf("--ports: %q is no route (%s)", k, strings.Join(s.Front.RouteNames(), ", "))
		}
	}
	for _, k := range sortedKeys(added.ports) {
		for _, other := range sortedKeys(pub.ports) {
			if other != k && pub.ports[other] == added.ports[k] {
				return publish{}, fmt.Errorf("%s %s adds the route %s on port %d, which %s has: choose another with --ports %s=PORT", m.Name, m.ProjectVersion, k, added.ports[k], other, k)
			}
		}
	}
	if err := in.l4Clash(added); err != nil {
		return publish{}, err
	}
	return pub, in.portsFree(added)
}

// upgradeRoutes records in next the URL of each route the release adds (and its port in loopback
// mode) as a first install records it, where pub has the front listen; the routes the install
// publishes keep theirs (routePaths). In a TLS mode every route's domain is its domain input's
// value — recorded, or given to an input the release adds — and the routes must not clash on
// them (routeDomains).
func (in *Installer) upgradeRoutes(env, next *EnvFile, mode string, pub publish) error {
	s, m := in.R.Spec, in.R.Manifest
	if !s.HasFront() {
		return nil
	}
	get := func(k string) string { v, _ := next.Get(k); return v }
	if mode != Loopback {
		for _, r := range s.Front.Routes {
			if get(r.DomainInput) == "" {
				return fmt.Errorf("%s %s adds the route %s, whose domain %s nothing gives: --set %s=<domain>", m.Name, m.ProjectVersion, r.Name, r.DomainInput, r.DomainInput)
			}
		}
	}
	// In byo mode the install's certificate must cover the domain of a route the release adds, as
	// a re-run's.
	adds := slices.ContainsFunc(s.Front.Routes, func(r spec.Route) bool { _, ok := env.Get(spec.URLVar(r.Name)); return !ok })
	if mode == "byo" && adds {
		_, leaf, err := readLeaf(filepath.Join(in.O.Dir, TLSDir, "cert.pem"))
		if err != nil {
			return fmt.Errorf("the install's certificate: %w", err)
		}
		for _, r := range s.Front.Routes {
			if _, ok := env.Get(spec.URLVar(r.Name)); !ok && leaf.VerifyHostname(get(r.DomainInput)) != nil {
				return fmt.Errorf("%s %s adds the route %s: the install's certificate does not cover %s (%s)", m.Name, m.ProjectVersion, r.Name, get(r.DomainInput), r.DomainInput)
			}
		}
	}
	domains, err := in.routeDomains(mode, get)
	if err != nil {
		return fmt.Errorf("%s %s: %w", m.Name, m.ProjectVersion, err)
	}
	if err := in.routePaths(next, mode); err != nil {
		return err
	}
	for _, r := range s.Front.Routes {
		if _, ok := env.Get(spec.URLVar(r.Name)); !ok {
			routeVars(next, mode, r, pub, domains[r.Name])
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
	acceptedVars(in.secrets(), func(k string) string { v, _ := next.Get(k); return v }, func(k, v string) { next.Set(k, v); secrets[k] = true })
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
	in.stowageImageVars(next, prefix)
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

// keepPrev keeps the running release — kept, the install's kept installer — in PrevDir, replacing
// the one kept before. Its stack files are rendered from that release, not copied from the
// install, whose files an interrupted upgrade may have left at another release; another
// Stowage's release is copied as the install holds it, as its installer, which a rollback starts
// it with, writes them again.
func (in *Installer) keepPrev(env *EnvFile, st state, kept *keptRelease) error {
	tmp := in.path(PrevDir + ".new")
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return err
	}
	if kept.Release != nil {
		if err := (&Installer{R: kept.Release, O: in.O}).renderStack(env, tmp); err != nil {
			return err
		}
	} else {
		// Another Stowage's stack files are its own to render: those its last start wrote.
		for _, f := range stackFiles {
			if err := copyFile(in.path(f), filepath.Join(tmp, f), 0o644); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
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
	case "STOWAGE_PROJECT_VERSION", "STOWAGE_REVISION", FrontImageVar, ChownImageVar:
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
	env, locked, err := in.openLocked(ctx)
	if err != nil {
		return err
	}
	// The previous release's installer starts it, taking the lock itself.
	unlock := sync.OnceFunc(locked)
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
	pname, pversion, prevRev := runs(prevEnv)
	var mixed error // PrevDir holding more than one release, told before its installer is run
	pk, err := in.readKept(ctx, filepath.Join(prev, in.R.Ctl()), func(pm *release.Manifest) error {
		if pm.Name != name || pm.ProjectVersion != st.ProjectVersion || pm.Revision != st.Revision ||
			pname != name || pversion != st.ProjectVersion || prevRev != strconv.Itoa(st.Revision) {
			mixed = fmt.Errorf("%s does not hold one release: its installer carries %s %s (revision %d), its record says %s (revision %d), its .env %s %s (revision %s)",
				prev, pm.Name, pm.ProjectVersion, pm.Revision, st.ProjectVersion, st.Revision, pname, pversion, prevRev)
		}
		return mixed
	})
	switch {
	case mixed != nil:
		return mixed
	case err != nil:
		return fmt.Errorf("the previous release's installer: %w", err)
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
		if err := in.restoreStack(prev, true); err != nil {
			return err
		}
		if err := copyFile(filepath.Join(prev, in.R.Ctl()), in.ctl(), 0o755); err != nil {
			return err
		}
		if err := os.RemoveAll(prev); err != nil {
			return err
		}
		unlock()
		return in.startKept(ctx, pk.Manifest, o.Offline || !pinnedImages(env))
	}
	if cur != m.ProjectVersion || rev != strconv.Itoa(m.Revision) {
		return fmt.Errorf("%s runs %s %s (revision %s), and this installer carries %s (revision %d): a rollback is run by the installer the install keeps, `%s rollback --dir %s`",
			o.Dir, name, cur, rev, m.ProjectVersion, m.Revision, in.ctl(), o.Dir)
	}
	o.Offline = o.Offline || !pinnedImages(prevEnv)
	// back is the previous release as this Stowage reads it: nil when another built it, whose
	// spec only its own installer reads.
	var back *Installer
	if pk.Release != nil {
		back = &Installer{R: pk.Release, O: in.O}
	} else {
		o.logf("%s: what only its spec tells is not checked", pk.foreign())
	}
	rolled := NewEnvFile()
	// A value the release sets is the previous release's, or none where it records none (an image
	// only the newer release has).
	for _, k := range env.Keys() {
		v, _ := env.Get(k)
		if releaseKey(k) {
			pv, ok := prevEnv.Get(k)
			if !ok {
				continue
			}
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
	if err := in.baselineCheck(pk, st); err != nil {
		return err
	}
	if err := in.volumesSplit(); err != nil {
		return err
	}
	if err := in.checkDocker(ctx); err != nil {
		return err
	}
	// A dry run pulls them only when the check may read them.
	if back != nil && (!o.DryRun || in.schemaOn() && back.schemaOn()) {
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
		o.logf("a run would go back to %s %s (revision %d)", pk.Manifest.Name, st.ProjectVersion, st.Revision)
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
	if err := in.restoreStack(prev, back == nil); err != nil {
		return err
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
	unlock()
	if err := in.startKept(ctx, pk.Manifest, o.Offline); err != nil {
		return fmt.Errorf("%w\nthe install records %s %s; the release left is kept in %s", err, pk.Manifest.Name, st.ProjectVersion, failed)
	}
	return nil
}

// startKept starts the release the install now records, m, with the installer it keeps: a release
// is started by its own Stowage's code, whichever Stowage built the one this installer carries.
// On Docker that is a re-run of it (without the provision hook); a VM's installer (an OS
// bundle's) runs its boot, which brings up the release the install records. The install is
// unlocked: the child locks it, and says how to start it again if it cannot.
func (in *Installer) startKept(ctx context.Context, m *release.Manifest, offline bool) error {
	args := []string{"--dir", in.O.Dir, "--no-input", "--no-provision"}
	if m.Bundled() {
		args = []string{bootCmd, "--dir", in.O.Dir}
	} else if offline {
		args = append(args, "--offline")
	}
	in.O.logf("starting %s %s (revision %d) with its own installer: %s %s", m.Name, m.ProjectVersion, m.Revision, in.ctl(), strings.Join(args, " "))
	return in.O.exec(ctx, in.ctl(), args)
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
	if back == nil {
		return unchecked("whether %s reads the newer schema %s is not known: another Stowage built it", st.ProjectVersion, now)
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

func (in *Installer) projectVersion() string { return in.R.Manifest.ProjectVersion }

// baselined is a release whose schema baseline is told: an installer's own, or a kept release's,
// read from its payload whichever Stowage built it.
type baselined interface {
	baseline() (int, error)
	projectVersion() string
}

// baselines are the schema baselines of the releases from (the install's) and to; ok is false
// when either has no database or its baseline does not read (said in the log): the check then
// does not refuse.
func baselines(o *Options, from, to baselined) (int, int, bool) {
	a, err := from.baseline()
	if err != nil {
		o.logf("the schema baseline of %s is not known (%v): not checked", from.projectVersion(), err)
		return 0, 0, false
	}
	b, err := to.baseline()
	if err != nil {
		o.logf("the schema baseline of %s is not known (%v): not checked", to.projectVersion(), err)
		return 0, 0, false
	}
	return a, b, a != 0 && b != 0
}

// baselineCheck refuses to go back to the release back when it starts another schema baseline
// than the installed one: decided from the two specs alone, before Docker is asked anything, back's
// read from its payload whichever Stowage built it.
func (in *Installer) baselineCheck(back *keptRelease, st state) error {
	from, to, ok := baselines(&in.O, in, back)
	if !ok || from == to {
		return nil
	}
	return rollbackRefused(fmt.Sprintf("refusing: the database is at schema baseline %d, and %s starts baseline %d, which shares no schema with it; going back would leave the install unable to start, and takes a fresh install of %s.",
		from, st.ProjectVersion, to, st.ProjectVersion))
}
