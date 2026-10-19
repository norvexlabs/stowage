package install

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

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
				!strings.Contains(err.Error(), "only by a fresh install") {
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
