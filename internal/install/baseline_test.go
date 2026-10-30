package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/spec"
)

// atBaseline gives a release the `sql` provider declaring schema baseline b (0: none, which is 1).
func atBaseline(b int) func(*spec.Spec) {
	return func(s *spec.Spec) {
		s.Database.SchemaCompat = spec.Provider{Name: "sql", Config: map[string]any{
			"current": "SELECT max(version) FROM migrations", "baseline": b}}
	}
}

// baselineInstall is an install of 1.0.0 at schema baseline from, whose database is at migration 5.
func baselineInstall(t *testing.T, from int) *env {
	t.Helper()
	e := newEnv(t)
	e.r, e.self = testInstallerWith(t, "1.0.0", atBaseline(from))
	e.docker.dump = "DUMP"
	e.docker.psql = func(string) (string, error) { return "5", nil }
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAnUpgradeAcrossASchemaBaselineIsRefused(t *testing.T) {
	t.Parallel()
	for name, from := range map[string]int{"declared": 1, "none, which is 1": 0} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := baselineInstall(t, from)
			before := read(t, filepath.Join(e.dir, ".env"))
			e.to("2.0.0", atBaseline(2))
			e.docker.calls = nil
			err := e.upgrade(nil)
			if err == nil || !strings.Contains(err.Error(), "starts schema baseline 2") || !strings.Contains(err.Error(), "at baseline 1") ||
				!strings.Contains(err.Error(), "only with its data wiped: `"+e.self+" reset-db --new-baseline --dir "+e.dir+"` wipes it and installs 2.0.0") {
				t.Fatalf("%v", err)
			}
			if after := read(t, filepath.Join(e.dir, ".env")); after != before {
				t.Errorf("a refused upgrade changed .env")
			}
			if n := len(e.docker.ran("exec -T postgres pg_dump")); n != 0 {
				t.Errorf("dumped before refusing")
			}
		})
	}
	// Each side of the check: the same baseline upgrades, however it is spelled.
	for name, c := range map[string]struct{ from, to int }{"same": {2, 2}, "none before": {0, 1}, "none after": {1, 0}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := baselineInstall(t, c.from)
			e.to("2.0.0", atBaseline(c.to))
			if err := e.upgrade(nil); err != nil {
				t.Fatalf("%v\n%s", err, e.log)
			}
		})
	}
}

func TestARollbackAcrossASchemaBaselineIsRefusedBeforeAnyPull(t *testing.T) {
	t.Parallel()
	e := baselineInstall(t, 1)
	e.to("2.0.0", atBaseline(1))
	if err := e.upgrade(nil); err != nil {
		t.Fatal(err)
	}
	// The installer of the release the install now runs declares baseline 2.
	e.to("2.0.0", atBaseline(2))
	e.docker.calls = nil
	var refused rollbackRefused
	err := e.rollback(func(o *Options) { o.DryRun = true })
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "schema baseline 2, and 1.0.0 starts baseline 1") ||
		!strings.Contains(err.Error(), "takes a fresh install of 1.0.0") {
		t.Fatalf("%v", err)
	}
	for _, v := range e.verbs() {
		if strings.HasPrefix(v, "pull") || strings.HasPrefix(v, "image ") || strings.Contains(v, " pull") {
			t.Errorf("pulled before refusing: %q", e.verbs())
			break
		}
	}
	// The VM's health gate asks the same rollback, dry, and keeps the boot on its refusal.
	if why := (&Gate{In: e.installer(nil)}).refusedRollback(context.Background()); !strings.Contains(why, "schema baseline 2") {
		t.Errorf("the gate's dry rollback: %q", why)
	}
	// Positive control: within one baseline the same rollback goes back.
	e.to("2.0.0", atBaseline(1))
	if err := e.rollback(nil); err != nil {
		t.Fatalf("%v", err)
	}
}

func TestABaselineIsNotCheckedWithoutADatabaseOrAReadableSpec(t *testing.T) {
	t.Parallel()
	var log strings.Builder
	o := &Options{Log: &log}
	with := func(mod func(*spec.Spec)) *Installer {
		r, _ := testInstallerWith(t, "1.0.0", mod)
		return &Installer{R: r}
	}
	two := with(atBaseline(2))
	without := *two.R.Spec
	without.Database = nil
	noDB := &Installer{R: &Release{Spec: &without, Manifest: two.R.Manifest}}
	if _, _, ok := baselines(o, noDB, two); ok {
		t.Error("a release without a database was checked")
	}
	if _, _, ok := baselines(o, two, noDB); ok {
		t.Error("a release without a database was checked")
	}
	unreadable := *two.R.Spec
	db := *two.R.Spec.Database
	db.SchemaCompat = spec.Provider{Name: "sql", Config: map[string]any{"current": "x", "baseline": "two"}}
	unreadable.Database = &db
	bad := &Installer{R: &Release{Spec: &unreadable, Manifest: two.R.Manifest}}
	if _, _, ok := baselines(o, bad, two); ok || !strings.Contains(log.String(), "not checked") {
		t.Errorf("an unreadable baseline was checked: %s", log.String())
	}
	if a, b, ok := baselines(o, two, with(atBaseline(1))); !ok || a != 2 || b != 1 {
		t.Errorf("baselines = %d, %d, %v", a, b, ok)
	}
}

// A release another Stowage built, whose spec this one refuses whole (a field it dropped), still
// tells its schema baseline: an upgrade from it across one is refused, and one within it upgrades.
func TestAnotherStowagesReleaseTellsItsBaseline(t *testing.T) {
	t.Parallel()
	e := baselineInstall(t, 1)
	anotherStowagesAt(t, filepath.Join(e.dir, "notes-stow"))
	e.to("2.0.0", atBaseline(2))
	if err := e.upgrade(nil); err == nil || !strings.Contains(err.Error(), "starts schema baseline 2") || !strings.Contains(err.Error(), "at baseline 1") {
		t.Fatalf("%v\n%s", err, e.log)
	}
	e.to("2.0.0", atBaseline(1))
	if err := e.upgrade(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	// Going back to it: PrevDir's installer is the other Stowage's, read for its baseline alone.
	e.to("2.0.0", atBaseline(2))
	var refused rollbackRefused
	if err := e.rollback(func(o *Options) { o.DryRun = true }); !errors.As(err, &refused) || !strings.Contains(err.Error(), "schema baseline 2, and 1.0.0 starts baseline 1") {
		t.Fatalf("%v\n%s", err, e.log)
	}
}

func TestPayloadBaseline(t *testing.T) {
	t.Parallel()
	r, _ := testInstallerWith(t, "1.0.0", atBaseline(3))
	raw, err := json.Marshal(r.Spec)
	if err != nil {
		t.Fatal(err)
	}
	// The path every Stowage writes, from the spec's own marshalling: a rename of it fails here.
	if b, err := payloadBaseline(raw); err != nil || b != 3 {
		t.Errorf("baseline %d, %v", b, err)
	}
	if b, err := payloadBaseline(append([]byte(`{"Memory":1,`), raw[1:]...)); err != nil || b != 3 {
		t.Errorf("with a field this Stowage does not know: %d, %v", b, err)
	}
	if b, err := payloadBaseline([]byte(`{"Name":"notes"}`)); err != nil || b != 0 {
		t.Errorf("no database: %d, %v", b, err)
	}
	for _, bad := range []string{`{"Database":{"SchemaCompat":{"Name":"sql","Config":{"baseline":"two"}}}}`, `[`} {
		if _, err := payloadBaseline([]byte(bad)); err == nil {
			t.Errorf("%s read", bad)
		}
	}
}

// crossable is an install of 1.5.0 at schema baseline 1, upgraded from 1.0.0 (kept in PrevDir), its
// volumes listed; this installer carries 2.0.0 at baseline 2.
func crossable(t *testing.T) *env {
	t.Helper()
	e := baselineInstall(t, 1)
	e.to("1.5.0", atBaseline(1))
	if err := e.upgrade(nil); err != nil {
		t.Fatal(err)
	}
	e.docker.hookOut = "ADMIN_PASSWORD=second\n"
	e.docker.answer = volumes(get(e.readEnv(), "COMPOSE_PROJECT_NAME"), nil)
	e.to("2.0.0", atBaseline(2))
	e.docker.calls = nil
	return e
}

func TestResetNewBaselineWipesAndUpgradesOnDocker(t *testing.T) {
	t.Parallel()
	e := crossable(t)
	if err := e.reset(func(o *Options) { o.Yes = true }); err == nil || !strings.Contains(err.Error(), "runs notes 1.5.0 (revision 1); this installer carries 2.0.0 (revision 1): `") {
		t.Fatalf("without the flag: %v", err)
	}
	if n := len(e.docker.ran("volume rm")); n != 0 {
		t.Fatalf("wiped without the flag")
	}
	if err := e.reset(func(o *Options) { o.NewBaseline, o.DryRun = true, true }); err != nil ||
		!strings.Contains(e.out.String(), "then install notes 2.0.0, at the new schema baseline, with the same values") {
		t.Fatalf("dry run: %v %s", err, e.out)
	}
	if n := len(e.docker.ran("volume rm")); n != 0 || get(e.readEnv(), "STOWAGE_PROJECT_VERSION") != "1.5.0" {
		t.Fatalf("the dry run changed the install")
	}
	if err := e.reset(func(o *Options) { o.NewBaseline, o.Yes = true, true }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if v := get(e.readEnv(), "STOWAGE_PROJECT_VERSION"); v != "2.0.0" {
		t.Errorf("runs %s", v)
	}
	project := get(e.readEnv(), "COMPOSE_PROJECT_NAME")
	v := e.verbs()
	at := func(prefix string) int {
		return slices.IndexFunc(v, func(s string) bool { return strings.HasPrefix(s, prefix) })
	}
	down, rm, up := at("compose down --remove-orphans"), at("volume rm "+project+"_postgres"), at("compose up --detach")
	if down < 0 || rm < down || up < rm {
		t.Fatalf("down %d, rm %d, up %d: %q", down, rm, up, v)
	}
	if n := len(e.docker.ran("exec -T postgres pg_dump")); n != 0 {
		t.Errorf("dumped data it wipes")
	}
	if _, err := os.Stat(filepath.Join(e.dir, PrevDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a release is kept to go back to across the baseline: %v", err)
	}
	if len(e.docker.hookEnv) != 0 {
		t.Errorf("the hook was given %v", e.docker.hookEnv)
	}
	if outs, _ := ReadEnvFile(filepath.Join(e.dir, ProvisionEnv)); get(outs, "ADMIN_PASSWORD") != "second" {
		t.Errorf("provision.env: %v", outs.Keys())
	}
}

func TestResetNewBaselineRefusalsOnDocker(t *testing.T) {
	t.Parallel()
	flag := func(o *Options) { o.NewBaseline, o.Yes = true, true }
	unchanged := func(t *testing.T, e *env, what string) {
		t.Helper()
		if n := len(e.docker.ran("volume rm")); n != 0 || get(e.readEnv(), "STOWAGE_PROJECT_VERSION") != "1.5.0" {
			t.Errorf("%s: the install changed", what)
		}
	}
	e := crossable(t)
	e.to("2.0.0", atBaseline(1))
	if err := e.reset(flag); err == nil || !strings.Contains(err.Error(), "--new-baseline only crosses a baseline; `") {
		t.Errorf("same baseline: %v", err)
	}
	unchanged(t, e, "same baseline")
	e = crossable(t)
	e.to("1.5.0", atBaseline(2))
	if err := e.reset(flag); err == nil || !strings.Contains(err.Error(), "already runs notes 1.5.0 (revision 1): --new-baseline installs another release") {
		t.Errorf("same release: %v", err)
	}
	// A baseline the install's installer does not tell: never wiped on a guess.
	e = crossable(t)
	ctl := filepath.Join(e.dir, "notes-stow")
	files, err := payload.Read(ctl)
	if err != nil {
		t.Fatal(err)
	}
	files[payload.Spec] = bytes.Replace(files[payload.Spec], []byte(`"baseline":1`), []byte(`"baseline":"one"`), 1)
	var buf bytes.Buffer
	if err := payload.Write(&buf, strings.NewReader("exe"), files); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ctl, buf.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.reset(func(o *Options) {
		flag(o)
		o.Exec = func(context.Context, string, []string) error { return nil } // it reads itself
	}); err == nil || !strings.Contains(err.Error(), "cannot be told (the log says why): --new-baseline never wipes on a guess") {
		t.Errorf("unknown baseline: %v\n%s", err, e.log)
	}
	unchanged(t, e, "unknown baseline")
	// The install moved after the confirmation: the upgrade wipes only the release confirmed.
	e = crossable(t)
	in := e.installer(flag)
	in.crossing = &crossing{"1.0.0", "1", nil}
	if err := in.Upgrade(context.Background()); err == nil || !strings.Contains(err.Error(), "which was confirmed: nothing was wiped") {
		t.Errorf("moved: %v", err)
	}
	unchanged(t, e, "moved")
}

// A crossing wipes what the release the install runs names in data.reset, its stack's, beside
// the new release's: a volume the new one no longer lists is deleted all the same, whether this
// Stowage reads the old spec or only its data.reset.
func TestResetNewBaselineWipesTheOldReleasesVolumes(t *testing.T) {
	t.Parallel()
	for name, foreign := range map[string]bool{"read": false, "another Stowage's": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := crossable(t)
			if foreign {
				anotherStowagesAt(t, filepath.Join(e.dir, "notes-stow"))
			}
			e.to("2.0.0", func(s *spec.Spec) {
				atBaseline(2)(s)
				s.Data.Reset = []string{"uploads"}
			})
			if err := e.reset(func(o *Options) {
				o.NewBaseline, o.Yes = true, true
				o.Exec = func(context.Context, string, []string) error { return nil } // it reads itself
			}); err != nil {
				t.Fatalf("%v\n%s", err, e.log)
			}
			project := get(e.readEnv(), "COMPOSE_PROJECT_NAME")
			for _, v := range []string{"postgres", "uploads"} {
				if n := len(e.docker.ran("volume rm " + project + "_" + v)); n != 1 {
					t.Errorf("%s removed %d times", v, n)
				}
			}
		})
	}
}

// The crossing's question names every volume it deletes, the old release's included; one the new
// release keeps on a reset is kept, whatever the old one said; a new release naming none in
// data.reset still crosses, wiping the old one's.
func TestResetNewBaselineSaysWhatItWipes(t *testing.T) {
	t.Parallel()
	for name, reset := range map[string][]string{"kept by the new release": {"postgres"}, "none in the new release": nil} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := crossable(t)
			e.to("2.0.0", func(s *spec.Spec) {
				atBaseline(2)(s)
				s.Data.Reset = reset
				if reset != nil {
					s.Data.Volumes = map[string]spec.Volume{"uploads": {KeepOnReset: true}}
				}
			})
			if err := e.reset(func(o *Options) { o.NewBaseline, o.DryRun = true, true }); err != nil {
				t.Fatalf("%v\n%s", err, e.log)
			}
			want := "delete its volumes postgres, uploads, forget"
			if reset != nil {
				want = "delete its volumes postgres (keeping uploads), forget"
			}
			if out := e.out.String(); !strings.Contains(out, want) {
				t.Errorf("%s", out)
			}
		})
	}
}

// A crossing that would wipe nothing is refused before anything changes, whichever path asked it
// (the VM's boot comes straight to the upgrade): the new release would start on the old data.
func TestACrossingWithNothingToWipeIsRefused(t *testing.T) {
	t.Parallel()
	e := crossable(t)
	// The installer the install keeps, of 1.5.0, names no volume in data.reset either.
	_, none := testInstallerWith(t, "1.5.0", func(s *spec.Spec) { atBaseline(1)(s); s.Data.Reset = nil })
	if err := copyFile(none, filepath.Join(e.dir, "notes-stow"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.to("2.0.0", func(s *spec.Spec) {
		atBaseline(2)(s)
		s.Data.Reset = nil
	})
	in := e.installer(nil)
	in.crossing = &crossing{version: "1.5.0", revision: "1"}
	if err := in.Upgrade(context.Background()); err == nil || !strings.Contains(err.Error(), "--new-baseline has nothing to wipe") {
		t.Fatalf("%v", err)
	}
	if n := len(e.docker.ran("volume rm")) + len(e.docker.ran("compose down")); n != 0 || get(e.readEnv(), "STOWAGE_PROJECT_VERSION") != "1.5.0" {
		t.Errorf("the install changed")
	}
}

// The upgrade's own refusals come before the crossing's question: an upgrade that stopped is
// refused without asking, nothing wiped.
func TestResetNewBaselineRefusesBeforeItAsks(t *testing.T) {
	t.Parallel()
	e := crossable(t)
	if err := os.WriteFile(filepath.Join(e.dir, PrevDir, prevState), []byte(`{"project_version":"1.5.0","revision":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	asked := false
	p := &fakePrompter{answers: map[string]string{"confirm": "yes"}, onAsk: func() { asked = true }}
	err := e.reset(func(o *Options) { o.NewBaseline, o.Prompt = true, p })
	if err == nil || !strings.Contains(err.Error(), "stopped before it finished") || asked {
		t.Fatalf("asked %v: %v", asked, err)
	}
	if n := len(e.docker.ran("volume rm")); n != 0 {
		t.Errorf("wiped")
	}
}

// A crossing out of another Stowage's release forgets that release's provision outputs file too,
// read from its spec for that alone, though the new release names another.
func TestACrossingForgetsTheOldReleasesOutputsFile(t *testing.T) {
	t.Parallel()
	outputs := func(name string) func(*spec.Spec) {
		return func(s *spec.Spec) {
			s.Hooks.Provision.Config["outputs"] = name
		}
	}
	e := newEnv(t)
	e.r, e.self = testInstallerWith(t, "1.0.0", func(s *spec.Spec) { atBaseline(1)(s); outputs("admin.env")(s) })
	e.docker.psql = func(string) (string, error) { return "5", nil }
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "admin.env"), []byte("ADMIN_PASSWORD=old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	anotherStowagesAt(t, filepath.Join(e.dir, "notes-stow"))
	e.docker.answer = volumes(get(e.readEnv(), "COMPOSE_PROJECT_NAME"), nil)
	e.to("2.0.0", func(s *spec.Spec) { atBaseline(2)(s); outputs("creds.env")(s) })
	if err := e.reset(func(o *Options) {
		o.NewBaseline, o.Yes = true, true
		o.Exec = func(context.Context, string, []string) error { return nil } // it reads itself
	}); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "admin.env")); !os.IsNotExist(err) {
		t.Errorf("the old release's outputs file outlived the crossing: %v", err)
	}
}

// A crossing whose wipe fails leaves the install as it was, its previous release still kept to
// go back to.
func TestACrossingWhoseWipeFailsKeepsThePreviousRelease(t *testing.T) {
	t.Parallel()
	e := crossable(t)
	project := get(e.readEnv(), "COMPOSE_PROJECT_NAME")
	e.docker.answer = volumes(project, func(a string) ([]byte, bool, error) {
		if strings.HasPrefix(a, "volume rm ") {
			return nil, true, errors.New("volume in use")
		}
		return nil, false, nil
	})
	if err := e.reset(func(o *Options) { o.NewBaseline, o.Yes = true, true }); err == nil || !strings.Contains(err.Error(), "volume in use") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, PrevDir, prevState)); err != nil {
		t.Errorf("the previous release is gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, crossedFile)); !os.IsNotExist(err) {
		t.Errorf("a crossing that failed is recorded: %v", err)
	}
}
