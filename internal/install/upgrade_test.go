package install

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/command"
	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/spec"
)

// installed is an install of examples/minimal 1.0.0, provisioned, whose database dumps as "DUMP"
// and is at migration 5.
func installed(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.docker.dump = "DUMP"
	e.schemaAt("5", nil, true)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	return e
}

// schemaAt makes the schema command answer: migration v, these barriers, and whether an image
// reads a newer schema.
func (e *env) schemaAt(v string, barriers []string, readsNewer bool) {
	e.docker.schema = func(_ string, req map[string]any) (string, error) {
		var out any
		switch req["op"] {
		case "schema.current":
			out = map[string]any{"version": v}
		case "schema.barrier_after":
			out = map[string]any{"barriers": append([]string{}, barriers...)}
		case "schema.reads_newer":
			out = map[string]any{"reads_newer": readsNewer}
		}
		b, _ := json.Marshal(out)
		return string(b), nil
	}
}

// to switches e to the installer of version, whose spec mod changes.
func (e *env) to(version string, mod func(*spec.Spec)) {
	e.r, e.self = testInstallerWith(e.t, version, mod)
}

func (e *env) upgrade(mod func(*Options)) error {
	return e.installer(mod).Upgrade(context.Background())
}

func (e *env) rollback(mod func(*Options)) error {
	return e.installer(mod).Rollback(context.Background())
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestUpgradeKeepsTheRecordedValuesAndAddsTheNew(t *testing.T) {
	t.Parallel()
	e := installed(t)
	before := e.readEnv()
	v1 := read(t, filepath.Join(e.dir, "notes-stow"))
	e.to("2.0.0", func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "SMTP_HOST", Default: "smtp.${ADMIN_EMAIL}"}, spec.Input{Key: "WORKERS", Advanced: true})
		s.Secrets = append(s.Secrets, spec.Secret{Key: "NEW_KEY", Gen: "hex16"}, spec.Secret{Key: "TOKEN_KEY", Gen: "hex16", Rotation: true})
		s.SafeDefaults["NEW_FLAG"] = "on"
	})
	e.docker.calls = nil
	if err := e.upgrade(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	after := e.readEnv()
	for _, k := range before.Keys() {
		if k == "STOWAGE_PROJECT_VERSION" {
			continue
		}
		if get(after, k) != get(before, k) {
			t.Errorf("%s changed: %q → %q", k, get(before, k), get(after, k))
		}
	}
	if get(after, "STOWAGE_PROJECT_VERSION") != "2.0.0" || get(after, "SMTP_HOST") != "smtp.admin@example.org" ||
		len(get(after, "NEW_KEY")) != 32 || get(after, "NEW_FLAG") != "on" {
		t.Fatalf(".env after the upgrade: %v", after.Keys())
	}
	// An added advanced input nobody gave stays unrecorded; an added rotated secret gets its
	// empty next and previous values.
	if _, ok := after.Get("WORKERS"); ok {
		t.Error("the upgrade recorded an advanced input nobody gave")
	}
	for _, k := range []string{"TOKEN_KEY_NEXT", "TOKEN_KEY_PREVIOUS"} {
		if v, ok := after.Get(k); !ok || v != "" {
			t.Errorf("%s = %q, %v after the upgrade adding its secret", k, v, ok)
		}
	}
	if read(t, filepath.Join(e.dir, "notes-stow")) != read(t, e.self) {
		t.Error("the install does not keep the new installer")
	}

	// The release it replaced is kept, with the database's state before the upgrade.
	prev := filepath.Join(e.dir, PrevDir)
	if read(t, filepath.Join(prev, "notes-stow")) != v1 || !strings.Contains(read(t, filepath.Join(prev, ".env")), "STOWAGE_PROJECT_VERSION='1.0.0'") {
		t.Fatal("the previous release is not kept")
	}
	var st state
	_ = json.Unmarshal([]byte(read(t, filepath.Join(prev, prevState))), &st)
	if st.ProjectVersion != "1.0.0" || st.Revision != 1 || st.Schema != "5" || filepath.Dir(st.Snapshot) != filepath.Join(e.dir, SnapshotsDir) {
		t.Fatalf("state: %+v", st)
	}
	f, err := os.Open(st.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	if dump, _ := io.ReadAll(zr); string(dump) != "DUMP" {
		t.Errorf("snapshot: %q", dump)
	}
	for p, want := range map[string]os.FileMode{st.Snapshot: 0o600, filepath.Join(e.dir, SnapshotsDir): 0o700 | os.ModeDir, prev: 0o700 | os.ModeDir, filepath.Join(prev, ".env"): 0o600} {
		if fi, _ := os.Stat(p); fi.Mode() != want {
			t.Errorf("%s: %v, want %v", p, fi.Mode(), want)
		}
	}
	// Dumped before anything changed, started, then provisioned.
	var order []string
	for _, c := range e.docker.calls {
		a := strings.Join(c.Args, " ")
		for _, step := range []string{"pg_dump", "up --detach", ProvisionService} {
			if strings.Contains(a, step) && strings.HasPrefix(a, "compose") {
				order = append(order, step)
			}
		}
	}
	if strings.Join(order, ",") != "pg_dump,up --detach,"+ProvisionService {
		t.Errorf("order: %v", order)
	}
}

// A release that gives a route a path moves its recorded URL there, the address and port kept.
func TestAnUpgradeGivesARouteItsPath(t *testing.T) {
	t.Parallel()
	e := installed(t)
	was := get(e.readEnv(), "STOWAGE_URL_API")
	e.to("2.0.0", func(s *spec.Spec) {
		for i := range s.Front.Routes {
			if s.Front.Routes[i].Name == "api" {
				s.Front.Routes[i].Path = "/v2"
			}
		}
	})
	if err := e.upgrade(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if got := get(e.readEnv(), "STOWAGE_URL_API"); was == "" || got != was+"/v2" {
		t.Errorf("STOWAGE_URL_API %q → %q", was, got)
	}
}

func TestUpgradeAndRollbackForgetAReRunsChanges(t *testing.T) {
	t.Parallel()
	e := installed(t)
	e.docker.upErr = errors.New("unhealthy")
	if err := e.install(func(o *Options) { o.Set = map[string]string{"ADMIN_EMAIL": "x@example.org"} }); err == nil {
		t.Fatal("the change came up")
	}
	e.docker.upErr = nil
	e.to("2.0.0", nil)
	if err := e.upgrade(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	for _, f := range []string{".env.prev", ".stowage-changing"} {
		if _, err := os.Stat(filepath.Join(e.dir, f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after the upgrade: %v", f, err)
		}
	}
	_ = os.WriteFile(filepath.Join(e.dir, ".env.prev"), []byte("A='1'\n"), 0o600)
	if err := e.rollback(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if _, err := os.Stat(filepath.Join(e.dir, ".env.prev")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".env.prev after the rollback: %v", err)
	}
}

func TestUpgradeAsksOnlyWhatTheReleaseAdds(t *testing.T) {
	t.Parallel()
	e := installed(t)
	e.to("2.0.0", func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "SMTP_HOST", Required: true})
	})
	err := e.upgrade(nil)
	if err == nil || !strings.Contains(err.Error(), "SMTP_HOST") || strings.Contains(err.Error(), "ADMIN_EMAIL") {
		t.Fatalf("a required added input: %v", err)
	}
	for set, want := range map[string]string{"ADMIN_EMAIL": "recorded at the install", "SMTP_PORT": "no input this release adds is named SMTP_PORT (it adds: SMTP_HOST)"} {
		if err := e.upgrade(func(o *Options) { o.Set = map[string]string{set: "x"} }); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("--set %s: %v", set, err)
		}
	}
	if get(e.readEnv(), "STOWAGE_PROJECT_VERSION") != "1.0.0" {
		t.Fatal("a refused upgrade changed .env")
	}
	if err := e.upgrade(func(o *Options) { o.Set = map[string]string{"SMTP_HOST": "mail.example.org"} }); err != nil {
		t.Fatal(err)
	}
	if get(e.readEnv(), "SMTP_HOST") != "mail.example.org" {
		t.Fatal("the added input is not recorded")
	}
}

func TestUpgradeRefusesBeforeChangingAnything(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		version string
		mod     func(*spec.Spec)
		fake    func(*fakeDocker)
		want    string
	}{
		"older": {"0.9.0", nil, nil, "after this installer's 0.9.0"},
		"new route": {"2.0.0", func(s *spec.Spec) {
			s.Front.Routes = append(s.Front.Routes, spec.Route{Name: "docs", DomainInput: "DOCS_DOMAIN", Upstream: "web:9000"})
		}, nil, "adds the route docs, which has no loopback port: give it one with --ports docs=PORT"},
		"tls mode":          {"2.0.0", func(s *spec.Spec) { s.Front.TLSModes = []string{"acme"} }, nil, "does not offer the TLS mode"},
		"pull fails":        {"2.0.0", nil, func(f *fakeDocker) { f.loaded = map[string]string{}; f.pullErr = errors.New("denied") }, "denied"},
		"dump fails":        {"2.0.0", nil, func(f *fakeDocker) { f.dumpErr = errors.New("exit status 1") }, "the database could not be dumped, so nothing was changed"},
		"other project":     {"2.0.0", func(*spec.Spec) {}, nil, `an install of "notes", not "atlas"`},
		"renamed installer": {"2.0.0", func(s *spec.Spec) { s.ShortName = "nt" }, nil, "install this release afresh"},
		"bad hook":          {"2.0.0", func(s *spec.Spec) { s.Hooks.Provision.Config["image"] = "docs" }, nil, "hooks.provision"},
		"offline":           {"2.0.0", nil, nil, "--offline: this install pulls its images by digest"},
		"l4 on the front's port": {"2.0.0", func(s *spec.Spec) {
			s.L4.Routes = []spec.L4Route{{Name: "data", Protocol: "tcp", Port: 8000, Upstream: "api:7000"}}
		}, nil, "the front's api port 8000 is the L4 route data's"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := installed(t)
			e.to(c.version, c.mod)
			if name == "other project" {
				e.r.Manifest.Name = "atlas"
			}
			if c.fake != nil {
				c.fake(e.docker)
			}
			before := read(t, filepath.Join(e.dir, ".env"))
			if err := e.upgrade(func(o *Options) { o.Offline = name == "offline" }); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v, want %q", err, c.want)
			}
			if read(t, filepath.Join(e.dir, ".env")) != before {
				t.Error(".env changed")
			}
			if _, err := os.Stat(filepath.Join(e.dir, PrevDir)); !errors.Is(err, os.ErrNotExist) {
				t.Error("a previous release was kept")
			}
			if entries, _ := os.ReadDir(filepath.Join(e.dir, SnapshotsDir)); len(entries) != 0 {
				t.Errorf("snapshots left: %v", entries)
			}
		})
	}
}

// docsRoute is a release adding the route docs: on its own domain input, and in loopback mode on
// port.
func docsRoute(port int) func(*spec.Spec) {
	return func(s *spec.Spec) {
		s.Front.Routes = append(s.Front.Routes, spec.Route{Name: "docs", DomainInput: "DOCS_DOMAIN", Upstream: "web:9000"})
		s.Front.LoopbackPorts["docs"] = port
	}
}

// An upgrade in loopback mode serves a route the release adds on its own port: the release's, or
// one --ports gives it, refused when another route has it or the host does; the routes the install
// publishes keep theirs.
func TestUpgradeAddsALoopbackRoute(t *testing.T) {
	t.Parallel()
	e := installed(t)
	before := e.readEnv()
	for name, c := range map[string]struct {
		port  int
		ports map[string]int
		want  string
	}{
		"another route's":  {8080, nil, "notes 2.0.0 adds the route docs on port 8080, which app has: choose another with --ports docs=PORT"},
		"--ports over one": {9000, map[string]int{"docs": 8000}, "notes 2.0.0 adds the route docs on port 8000, which api has"},
		"a recorded route": {9000, map[string]int{"app": 9001}, "--ports: the route app is recorded at the install, and an upgrade keeps its port"},
		"no route":         {9000, map[string]int{"nope": 9001}, `--ports: "nope" is no route (app, api, docs)`},
		"not a port":       {9000, map[string]int{"docs": 70000}, "--ports: docs=70000 is not a TCP port"},
		"busy":             {9000, nil, "port(s) 9000 (docs) already in use on 127.0.0.1: choose others with --ports"},
	} {
		e.busy[9000] = name == "busy"
		e.to("2.0.0", docsRoute(c.port))
		if err := e.upgrade(func(o *Options) { o.Ports = c.ports }); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	e.busy[9000] = false
	e.to("2.0.0", func(s *spec.Spec) {
		docsRoute(9000)(s)
		s.L4.Routes = []spec.L4Route{{Name: "data", Protocol: "tcp", Port: 9000, Upstream: "api:7000"}}
	})
	if err := e.upgrade(nil); err == nil || !strings.Contains(err.Error(), "the front's docs port 9000 is the L4 route data's: choose another with --ports") {
		t.Errorf("an L4 route's port: %v", err)
	}
	if get(e.readEnv(), "STOWAGE_PROJECT_VERSION") != "1.0.0" {
		t.Fatal("a refused upgrade changed .env")
	}
	// The port free: the route is served there, and recorded as a first install records it.
	e.to("2.0.0", docsRoute(9000))
	if err := e.upgrade(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	env := e.readEnv()
	for k, want := range map[string]string{"STOWAGE_PORT_DOCS": "9000", "STOWAGE_URL_DOCS": "http://localhost:9000", "STOWAGE_URL_APP": get(before, "STOWAGE_URL_APP"), "STOWAGE_PORT_APP": "8080"} {
		if got := get(env, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if fc := read(t, filepath.Join(e.dir, FrontCompose)); !strings.Contains(fc, "${STOWAGE_LOOPBACK_ADDRESS}:${STOWAGE_PORT_DOCS}:") {
		t.Errorf("the front does not publish docs:\n%s", fc)
	}
	// --ports gives a route the release adds another port.
	e = installed(t)
	e.to("2.0.0", docsRoute(9000))
	if err := e.upgrade(func(o *Options) { o.Ports = map[string]int{"docs": 9001} }); err != nil {
		t.Fatal(err)
	}
	if got := get(e.readEnv(), "STOWAGE_URL_DOCS"); got != "http://localhost:9001" {
		t.Errorf("STOWAGE_URL_DOCS = %q", got)
	}
}

// upgrade takes --ports, read as a first install reads it.
func TestUpgradeTakesPorts(t *testing.T) {
	t.Parallel()
	r, self := testInstaller(t, "1.0.0")
	cmd := Command(r, self, io.Discard, io.Discard)
	cmd.SetArgs([]string{"upgrade", "--ports", "docs=x", "--dir", t.TempDir()})
	if err := cmd.Execute(); err == nil || err.Error() != `--ports: "docs=x" is not route=port` {
		t.Fatalf("%v", err)
	}
}

// tlsInstalled is installed in the TLS mode internal, app and api on their domains.
func tlsInstalled(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.docker.dump = "DUMP"
	e.schemaAt("5", nil, true)
	e.vars["APP_DOMAIN"], e.vars["API_DOMAIN"] = "app.example.org", "api.example.org"
	if err := e.install(func(o *Options) { o.TLS = "internal" }); err != nil {
		t.Fatal(err)
	}
	return e
}

// An upgrade in a TLS mode serves a route the release adds on a domain the install records with
// nothing asked; one on a domain input the release adds takes it as an added input — given, or
// refused naming it — and a route the install publishes keeps a recorded domain.
func TestUpgradeAddsATLSRoute(t *testing.T) {
	t.Parallel()
	e := tlsInstalled(t)
	ca := func(s *spec.Spec) {
		s.Front.Routes = append(s.Front.Routes, spec.Route{Name: "ca", DomainInput: "APP_DOMAIN", Path: "/ca", Upstream: "web:8080"})
	}
	e.to("2.0.0", ca)
	if err := e.upgrade(nil); err != nil {
		t.Fatalf("a path route on a recorded domain: %v\n%s", err, e.log)
	}
	if got := get(e.readEnv(), "STOWAGE_URL_CA"); got != "https://app.example.org/ca" {
		t.Errorf("STOWAGE_URL_CA = %q", got)
	}
	if cf := read(t, filepath.Join(e.dir, Caddyfile)); !strings.Contains(cf, "path /ca /ca/*") {
		t.Errorf("the front does not serve /ca:\n%s", cf)
	}
	// A route on a domain input the release adds.
	docs := func(s *spec.Spec) { ca(s); docsRoute(9000)(s) }
	e.to("3.0.0", docs)
	for _, c := range []struct{ set, want string }{
		{"", "missing required input(s), set by --set KEY=VALUE, the environment or --answers: DOCS_DOMAIN (Domain for docs)"},
		{"app.example.org", "notes 3.0.0: app and docs both serve the domain app.example.org"},
		{"docs.example.org", ""},
	} {
		var given map[string]string
		if c.set != "" {
			given = map[string]string{"DOCS_DOMAIN": c.set}
		}
		err := e.upgrade(func(o *Options) { o.Set = given })
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Fatalf("DOCS_DOMAIN=%q: %v, want %q", c.set, err, c.want)
		}
	}
	env := e.readEnv()
	for k, want := range map[string]string{"DOCS_DOMAIN": "docs.example.org", "STOWAGE_URL_DOCS": "https://docs.example.org", "STOWAGE_URL_APP": "https://app.example.org"} {
		if got := get(env, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	// The domain input of a route the release adds that this shape never asks: refused
	// naming it.
	e.to("4.0.0", func(s *spec.Spec) {
		docs(s)
		s.Inputs = append(s.Inputs, spec.Input{Key: "WIKI_HOST", Prompt: "Wiki host", Shapes: []string{"kairos"}})
		s.Front.Routes = append(s.Front.Routes, spec.Route{Name: "wiki", DomainInput: "WIKI_HOST", Upstream: "web:8080"})
	})
	if err := e.upgrade(nil); err == nil || !strings.Contains(err.Error(), "notes 4.0.0 adds the route wiki, whose domain WIKI_HOST nothing gives: --set WIKI_HOST=<domain>") {
		t.Errorf("an empty domain: %v", err)
	}
	// A route the install publishes on a domain input it does not record is not given a domain.
	e.to("4.0.0", func(s *spec.Spec) { docs(s); s.Front.Routes[0].DomainInput = "WEB_DOMAIN" })
	if err := e.upgrade(func(o *Options) { o.Set = map[string]string{"WEB_DOMAIN": "web.example.org"} }); err == nil || !strings.Contains(err.Error(), "takes the domain of app from WEB_DOMAIN, which this install does not record") {
		t.Errorf("a published route's new domain input: %v", err)
	}
	// --ports is loopback mode's.
	e.to("4.0.0", docs)
	if err := e.upgrade(func(o *Options) { o.Ports = map[string]int{"docs": 9001} }); err == nil || !strings.Contains(err.Error(), "--ports is for loopback mode: this install's front serves internal") {
		t.Errorf("--ports in a TLS mode: %v", err)
	}
}

// An upgrade adding an L4 route checks its port is free on the host; one the running release
// already publishes is the install's own.
func TestUpgradeChecksTheL4PortsItAdds(t *testing.T) {
	t.Parallel()
	route := func(s *spec.Spec) {
		s.L4.Routes = []spec.L4Route{{Name: "data", Protocol: "tcp", Port: 7443, Upstream: "api:7000"}}
	}
	e := installed(t)
	e.busy[7443] = true
	e.to("2.0.0", route)
	if err := e.upgrade(nil); err == nil || !strings.Contains(err.Error(), "port(s) 7443 (data) already in use on this host") {
		t.Fatalf("err = %v", err)
	}
	e.busy[7443] = false
	if err := e.upgrade(nil); err != nil {
		t.Fatal(err)
	}
	// The stack's own: published by the running release, through a route or a service's ports:.
	e.busy[7443] = true
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if strings.HasSuffix(a, " ps --format json") {
			return []byte(`{"Service":"api","Publishers":[{"URL":"0.0.0.0","TargetPort":7000,"PublishedPort":7443,"Protocol":"tcp"}]}` + "\n" + `{"Service":"web","Publishers":[]}` + "\n"), true, nil
		}
		return nil, false, nil
	}
	e.to("3.0.0", route)
	if err := e.upgrade(nil); err != nil {
		t.Fatal(err)
	}
	// The front's recorded port: the refusal says how to move it.
	e.to("4.0.0", func(s *spec.Spec) {
		s.L4.Routes = []spec.L4Route{{Name: "data", Protocol: "tcp", Port: 8000, Upstream: "api:7000"}}
	})
	if err := e.upgrade(nil); err == nil || !strings.Contains(err.Error(), "the front's api port 8000 is the L4 route data's: choose another with --ports: give it to a re-run of the installer the install runs") {
		t.Fatalf("err = %v", err)
	}
}

// A route bound to one address holds its port there alone: the port the running release publishes
// on that address is the install's own, one on another address is not.
func TestUpgradeKnowsABoundL4PortItHolds(t *testing.T) {
	t.Parallel()
	// The running release publishes 7443 on 192.0.2.7 (published), where alone the port is held.
	bound := func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "DATA_BIND", Prompt: "Data bind address", Advanced: true})
		s.L4.Routes = []spec.L4Route{{Name: "data", Protocol: "tcp", Port: 7443, Upstream: "api:7000", BindInput: "DATA_BIND"}}
	}
	unbound := func(s *spec.Spec) {
		s.L4.Routes = []spec.L4Route{{Name: "data", Protocol: "tcp", Port: 7443, Upstream: "api:7000"}}
	}
	for _, c := range []struct {
		name, published, recorded, set string
		release                        func(*spec.Spec)
		own                            bool
		held                           []string // where the port is held: the stack's address, another process's
		want                           string   // the refusal; empty, the port in use
	}{
		{"recorded on its address", "192.0.2.7", "192.0.2.7", "", bound, true, []string{"192.0.2.7"}, ""},
		{"recorded on another address", "192.0.2.8", "192.0.2.7", "", bound, false, []string{"192.0.2.7", "192.0.2.8"}, ""},
		{"given to the upgrade on its address", "192.0.2.7", "", "192.0.2.7", bound, true, []string{"192.0.2.7", "192.0.2.8"}, ""},
		{"given to the upgrade on another address", "192.0.2.7", "", "192.0.2.8", bound, false, []string{"192.0.2.7", "192.0.2.8"}, ""},
		// Every address: the one the stack holds the port on is not probed, the others are.
		{"unbound by the release", "192.0.2.7", "192.0.2.7", "", unbound, true, []string{"192.0.2.7"}, ""},
		{"unbound by the release, held elsewhere", "192.0.2.7", "192.0.2.7", "", unbound, false, []string{"192.0.2.7", "192.0.2.8"}, ""},
		// Bound by the release, on every address now: the address is checked though the port is held.
		{"bound by the release elsewhere than the host", "0.0.0.0", "", "192.0.2.99", bound, false, nil, "192.0.2.99 (DATA_BIND, the L4 route data's address) is not an address of this host"},
		// The VM: the route's service on the host's network holds the port itself, on every address.
		{"on the host's network", "host", "", "", unbound, true, []string{"192.0.2.7", "192.0.2.8"}, ""},
		{"another service on the host's network", "host:web", "", "", unbound, false, []string{"192.0.2.7"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := installed(t)
			if c.recorded != "" {
				env := e.readEnv()
				env.Set("DATA_BIND", c.recorded)
				if err := env.WriteFile(filepath.Join(e.dir, ".env")); err != nil {
					t.Fatal(err)
				}
			}
			e.docker.answer = func(a string) ([]byte, bool, error) {
				if strings.HasSuffix(a, " ps --format json") {
					if svc, host := strings.CutPrefix(c.published, "host"); host {
						return []byte(`{"Service":"` + cmp.Or(strings.TrimPrefix(svc, ":"), "api") + `","Networks":"host","Publishers":[]}`), true, nil
					}
					return []byte(`{"Service":"api","Publishers":[{"URL":"` + c.published + `","TargetPort":7000,"PublishedPort":7443,"Protocol":"tcp"}]}`), true, nil
				}
				return nil, false, nil
			}
			e.to("2.0.0", c.release)
			err := e.upgrade(func(o *Options) {
				o.PortFree = func(h string, p int) bool { return p != 7443 || !slices.Contains(c.held, h) }
				if c.set != "" {
					o.Set = map[string]string{"DATA_BIND": c.set}
				}
			})
			if c.own && err != nil || !c.own && (err == nil || !strings.Contains(err.Error(), cmp.Or(c.want, "port(s) 7443 (data) already in use"))) {
				t.Errorf("%v", err)
			}
		})
	}
}

// The ports compose ps lists as published, from an array or a line per container.
func TestPublishedPorts(t *testing.T) {
	t.Parallel()
	for out, want := range map[string][]string{
		// A port on one address is keyed by it: it holds the port there alone.
		`[{"Publishers":[{"URL":"0.0.0.0","PublishedPort":7443,"Protocol":"udp"},{"URL":"0.0.0.0","PublishedPort":0,"Protocol":"tcp"},{"URL":"127.0.0.1","PublishedPort":5432,"Protocol":"tcp"}]}]`: {"udp/7443", "tcp/127.0.0.1/5432"},
		`{"Publishers":[{"URL":"::","PublishedPort":7443,"Protocol":"udp"}]}` + "\n\n" + `{"Publishers":null}`:                                                                                      {"udp/7443"},
		`{"Publishers":[{"URL":"::ffff:192.0.2.7","PublishedPort":17007,"Protocol":"tcp"},{"URL":"","PublishedPort":80,"Protocol":"tcp"}]}`:                                                         {"tcp/192.0.2.7/17007"},
		// A service on the host's network publishes nothing: it holds its ports itself.
		`{"Service":"echo","Networks":"host","Publishers":[]}` + "\n" + `{"Service":"api","Networks":"notes_default","Publishers":[]}`: {"host:echo"},
	} {
		got, err := publishedPorts([]byte(out))
		if err != nil || len(got) != len(want) || slices.ContainsFunc(want, func(k string) bool { return !got[k] }) {
			t.Errorf("%s: %v %v, want %v", out, got, err, want)
		}
	}
	if _, err := publishedPorts([]byte("{")); err == nil {
		t.Error("no error on a broken answer")
	}
}

func TestUpgradeToTheSameReleaseChangesNothing(t *testing.T) {
	t.Parallel()
	e := installed(t)
	e.docker.calls = nil
	if err := e.upgrade(nil); err != nil || !strings.Contains(e.out.String(), "already runs notes 1.0.0") || len(e.docker.ran("compose")) != 0 {
		t.Fatalf("%v %q %v", err, e.out, e.docker.calls)
	}
	// A later revision of the same version is an upgrade.
	e.r, e.self = testInstallerAt(t, "1.0.0", 2, nil)
	if err := e.upgrade(nil); err != nil || get(e.readEnv(), "STOWAGE_REVISION") != "2" {
		t.Fatalf("revision 2: %v", err)
	}
	e.r, e.self = testInstallerAt(t, "1.0.0", 1, nil)
	if err := e.upgrade(nil); err == nil || !strings.Contains(err.Error(), "after this installer's") {
		t.Fatalf("back to revision 1: %v", err)
	}
}

func TestUpgradeDryRunChangesNothing(t *testing.T) {
	t.Parallel()
	e := installed(t)
	e.to("2.0.0", func(s *spec.Spec) { s.Secrets = append(s.Secrets, spec.Secret{Key: "NEW_KEY", Gen: "hex16"}) })
	before := read(t, filepath.Join(e.dir, ".env"))
	e.docker.calls = nil
	if err := e.upgrade(func(o *Options) { o.DryRun = true }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "NEW_KEY=<redacted>") || !strings.Contains(e.out.String(), "STOWAGE_PROJECT_VERSION=2.0.0") || strings.Contains(e.out.String(), "ADMIN_EMAIL") {
		t.Fatalf("dry run: %s", e.out)
	}
	if read(t, filepath.Join(e.dir, ".env")) != before || len(e.docker.ran("compose --project-directory")) != 0 || len(e.docker.ran("pull")) != 0 {
		t.Fatalf("a dry run changed something: %v", e.docker.calls)
	}
	if _, err := os.Stat(filepath.Join(e.dir, SnapshotsDir)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a dry run dumped the database")
	}
}

func TestUpgradeWithoutProvisioning(t *testing.T) {
	t.Parallel()
	e := installed(t)
	e.to("2.0.0", nil)
	e.docker.calls = nil
	if err := e.upgrade(func(o *Options) { o.NoProvision = true }); err != nil {
		t.Fatal(err)
	}
	if len(e.hookCalls()) != 0 || !strings.Contains(e.log.String(), "provision --dir") {
		t.Fatalf("provisioned: %v\n%s", e.docker.calls, e.log)
	}
}

func TestAnUpgradeWhoseStackCameUpFinishesTheFirstInstall(t *testing.T) {
	t.Parallel()
	e := installed(t)
	marker := filepath.Join(e.dir, installingFile)
	if err := os.WriteFile(marker, []byte("failed: x\n"), 0o600); err != nil { // a first install that failed
		t.Fatal(err)
	}
	e.to("2.0.0", nil)
	e.docker.upErr = errors.New("unhealthy")
	if err := e.upgrade(nil); err == nil {
		t.Fatal("the upgrade passed")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("an upgrade whose stack did not come up finished the first install")
	}
	e.docker.upErr = nil
	e.to("2.1.0", nil)
	if err := e.upgrade(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the upgrade that came up and provisioned left the first install's marker")
	}
}

func TestAFailedUpgradeNamesTheRollback(t *testing.T) {
	t.Parallel()
	e := installed(t)
	e.to("2.0.0", nil)
	e.docker.upErr = errors.New("unhealthy")
	err := e.upgrade(nil)
	if err == nil || !strings.Contains(err.Error(), "notes 1.0.0 is kept: `"+filepath.Join(e.dir, "notes-stow")+" rollback --dir") {
		t.Fatalf("%v", err)
	}
}

// upgraded is installed(t) upgraded to 2.0.0, which adds a secret, and changes its api image and
// its compose file.
func upgraded(t *testing.T) *env {
	t.Helper()
	e := installed(t)
	e.to("2.0.0", func(s *spec.Spec) { s.Secrets = append(s.Secrets, spec.Secret{Key: "NEW_KEY", Gen: "hex16"}) })
	e.r.Manifest.Artifacts.Images["api"] = "sha256:" + strings.Repeat("9", 64)
	e.r.Compose = append(e.r.Compose, "# 2.0.0\n"...)
	if err := e.upgrade(nil); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestRollbackGoesBackAndKeepsWhatTheUpgradeAdded(t *testing.T) {
	t.Parallel()
	e := upgraded(t)
	v1, _ := ReadEnvFile(filepath.Join(e.dir, PrevDir, ".env"))
	compose1 := read(t, filepath.Join(e.dir, PrevDir, "compose.yaml"))
	v2 := e.readEnv()
	v2.Set("ADMIN_EMAIL", "changed@example.org") // a value changed since the upgrade is kept
	if err := v2.WriteFile(filepath.Join(e.dir, ".env")); err != nil {
		t.Fatal(err)
	}
	ctl2 := read(t, filepath.Join(e.dir, "notes-stow"))
	if get(v1, "STOWAGE_IMAGE_API") == get(v2, "STOWAGE_IMAGE_API") || compose1 == read(t, filepath.Join(e.dir, "compose.yaml")) {
		t.Fatal("the releases do not differ")
	}
	e.schemaAt("5", nil, false) // the database did not move: nothing is asked of the previous release
	e.docker.calls = nil
	if err := e.rollback(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	back := e.readEnv()
	if get(back, "STOWAGE_PROJECT_VERSION") != "1.0.0" || get(back, "STOWAGE_IMAGE_API") != get(v1, "STOWAGE_IMAGE_API") ||
		get(back, "NEW_KEY") != get(v2, "NEW_KEY") || get(back, "ADMIN_EMAIL") != "changed@example.org" {
		t.Fatalf(".env after the rollback: %v", back.Keys())
	}
	if read(t, filepath.Join(e.dir, "compose.yaml")) != compose1 {
		t.Error("the previous compose file is not back")
	}
	if _, err := os.Stat(filepath.Join(e.dir, PrevDir)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the previous release is still kept")
	}
	failed := filepath.Join(e.dir, FailedDir)
	if read(t, filepath.Join(failed, "notes-stow")) != ctl2 || !strings.Contains(read(t, filepath.Join(failed, ".env")), "STOWAGE_PROJECT_VERSION='2.0.0'") {
		t.Error("the release left is not kept")
	}
	if !strings.Contains(e.out.String(), "notes 1.0.0 is running") || len(e.docker.ran("compose")) == 0 {
		t.Fatalf("not started: %s", e.out)
	}
	for _, c := range e.docker.calls {
		if strings.Contains(strings.Join(c.Args, " "), "reads_newer") {
			t.Error("the previous release was asked")
		}
	}
	// One level: there is nothing more to go back to.
	e.r, e.self = testInstaller(t, "1.0.0")
	if err := e.rollback(nil); err == nil || !strings.Contains(err.Error(), "no previous release") {
		t.Fatalf("a second rollback: %v", err)
	}
}

func TestRollbackIsRunByTheInstalledRelease(t *testing.T) {
	t.Parallel()
	e := upgraded(t)
	e.r, e.self = testInstaller(t, "1.0.0")
	if err := e.rollback(nil); err == nil || !strings.Contains(err.Error(), filepath.Join(e.dir, "notes-stow")+" rollback") {
		t.Fatalf("%v", err)
	}
	e = installed(t)
	if err := e.rollback(nil); err == nil || !strings.Contains(err.Error(), "no previous release") {
		t.Fatalf("never upgraded: %v", err)
	}
}

func TestRollbackRefusedOnlyOnProof(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		now        string
		barriers   []string
		readsNewer bool
		unknown    bool
		want       string // "" = goes back
	}{
		"not moved":            {"5", []string{"7 (drop x)"}, false, false, ""},
		"barrier":              {"7", []string{"7 (drop x)"}, true, false, "the database carries 7 (drop x)"},
		"predates":             {"6", nil, false, false, "the database is at 6, and 1.0.0 predates reading a newer schema"},
		"reads newer":          {"6", nil, true, false, ""},
		"leading zeros":        {"0005", []string{"x"}, false, false, ""},
		"unknown is not proof": {"", nil, false, true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := upgraded(t)
			e.schemaAt(c.now, c.barriers, c.readsNewer)
			if c.unknown {
				e.docker.schema = func(string, map[string]any) (string, error) { return "", exitCode(2) }
			}
			err := e.rollback(nil)
			if c.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if c.unknown && !strings.Contains(e.log.String(), "going back unchecked") {
					t.Errorf("not reported: %s", e.log)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), filepath.Join(e.dir, SnapshotsDir)) {
				t.Fatalf("%v, want %q and the snapshot", err, c.want)
			}
			if get(e.readEnv(), "STOWAGE_PROJECT_VERSION") != "2.0.0" {
				t.Error("a refused rollback changed .env")
			}
		})
	}
}

func TestRollbackAsksThePreviousImage(t *testing.T) {
	t.Parallel()
	e := upgraded(t)
	prev, _ := ReadEnvFile(filepath.Join(e.dir, PrevDir, ".env"))
	cur := e.readEnv()
	e.docker.schema = func(image string, req map[string]any) (string, error) {
		switch req["op"] {
		case "schema.current", "schema.barrier_after":
			if image != get(cur, "STOWAGE_IMAGE_API") {
				t.Errorf("%s ran in %s", req["op"], image)
			}
			if req["op"] == "schema.current" {
				return `{"version": 6}`, nil
			}
			return `{"barriers": []}`, nil
		}
		if image != get(prev, "STOWAGE_IMAGE_API") || req["version"] != float64(6) {
			t.Errorf("reads_newer ran in %s with %v", image, req)
		}
		return `{"reads_newer": true}`, nil
	}
	if err := e.rollback(nil); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackWithTheSQLProvider(t *testing.T) {
	t.Parallel()
	sql := func(s *spec.Spec) {
		s.Database.SchemaCompat = spec.Provider{Name: "sql", Config: map[string]any{
			"current":           "SELECT max(version) FROM migrations",
			"barrier_after":     "SELECT count(*) > 0 FROM migrations WHERE version > $1 AND barrier AND x <> '$10'",
			"reads_newer_label": "com.example.reads-newer", "reads_newer_image": "api"}}
	}
	for name, c := range map[string]struct {
		barrier, labels, want string
	}{
		"barrier":     {"t", `{"com.example.reads-newer":"true"}`, "a rollback barrier after migration 5"},
		"no label":    {"f", `{"other":"true"}`, "predates reading a newer schema"},
		"label":       {"f", `{"com.example.reads-newer":"true"}`, ""},
		"label false": {"f", `{"com.example.reads-newer":"false"}`, "predates"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.r, e.self = testInstallerWith(t, "1.0.0", sql)
			e.docker.dump = "DUMP"
			at := "5"
			var queries []string
			e.docker.psql = func(q string) (string, error) {
				queries = append(queries, q)
				if strings.HasPrefix(q, "SELECT max") {
					return at, nil
				}
				return c.barrier, nil
			}
			if err := e.install(nil); err != nil {
				t.Fatal(err)
			}
			e.to("2.0.0", sql)
			if err := e.upgrade(nil); err != nil {
				t.Fatal(err)
			}
			at = "6"
			prev, _ := ReadEnvFile(filepath.Join(e.dir, PrevDir, ".env"))
			e.docker.labels = map[string]string{get(prev, "STOWAGE_IMAGE_API"): c.labels}
			err := e.rollback(nil)
			if c.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v, want %q", err, c.want)
			}
			if !strings.Contains(strings.Join(queries, "\n"), "version > 5 AND barrier AND x <> '$10'") {
				t.Errorf("queries: %q", queries)
			}
		})
	}
}

func TestAnOfflineInstallUpgradesFromLoadedImages(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, v := range []string{"1.0.0", "2.0.0"} {
		for _, k := range []string{"api", "web", "postgres", "front", "chown"} {
			e.docker.loaded["ghcr.io/acme/notes/notes-"+k+":"+v] = config
		}
	}
	if err := e.install(func(o *Options) { o.Offline = true }); err != nil {
		t.Fatal(err)
	}
	e.to("2.0.0", nil)
	if err := e.upgrade(nil); err != nil {
		t.Fatal(err)
	}
	if got := get(e.readEnv(), "STOWAGE_IMAGE_API"); got != "ghcr.io/acme/notes/notes-api:2.0.0" || len(e.docker.ran("pull")) != 0 {
		t.Fatalf("%s, %d pulls", got, len(e.docker.ran("pull")))
	}
}

func TestAStoppedCheckChangesNothing(t *testing.T) {
	t.Parallel()
	for _, verb := range []string{"upgrade", "rollback"} {
		t.Run(verb, func(t *testing.T) {
			t.Parallel()
			var e *env
			if verb == "rollback" {
				e = upgraded(t)
				e.schemaAt("7", nil, true)
			} else {
				e = installed(t)
				e.to("2.0.0", nil)
			}
			ctx, cancel := context.WithCancel(context.Background())
			e.docker.schema = func(string, map[string]any) (string, error) {
				cancel() // Ctrl-C while the schema is read
				return "", errors.New("signal: killed")
			}
			before := read(t, filepath.Join(e.dir, ".env"))
			in := e.installer(nil)
			run := in.Upgrade
			if verb == "rollback" {
				run = in.Rollback
			}
			if err := run(ctx); err == nil || !strings.Contains(err.Error(), "stopped before anything changed") {
				t.Fatalf("%v", err)
			}
			if read(t, filepath.Join(e.dir, ".env")) != before {
				t.Error(".env changed")
			}
			if strings.Contains(e.log.String(), "will not be checked") {
				t.Errorf("a stopped upgrade warned about a rollback: %s", e.log)
			}
			_, err := os.Stat(filepath.Join(e.dir, PrevDir))
			if kept := err == nil; kept != (verb == "rollback") {
				t.Errorf("previous release kept: %v", kept)
			}
			if snaps, _ := os.ReadDir(filepath.Join(e.dir, SnapshotsDir)); len(snaps) != map[string]int{"upgrade": 0, "rollback": 1}[verb] {
				t.Errorf("snapshots: %v", snaps)
			}
		})
	}
}

func TestRollbackAsksThePreviousReleasesOwnCommand(t *testing.T) {
	t.Parallel()
	e := installed(t)
	e.to("2.0.0", func(s *spec.Spec) { s.Database.SchemaCompat.Config["cmd"] = []any{"notes-api", "compat"} })
	if err := e.upgrade(nil); err != nil {
		t.Fatal(err)
	}
	e.schemaAt("6", nil, true)
	e.docker.schemaFiles = nil
	if err := e.rollback(nil); err != nil {
		t.Fatal(err)
	}
	if len(e.docker.schemaFiles) != 3 || !strings.Contains(e.docker.schemaFiles[0], "- compat") || !strings.Contains(e.docker.schemaFiles[2], "- schema-compat") {
		t.Fatalf("%q", e.docker.schemaFiles)
	}
}

func TestRollbackDryRunChecksAndChangesNothing(t *testing.T) {
	t.Parallel()
	e := upgraded(t)
	e.schemaAt("6", nil, false)
	before := read(t, filepath.Join(e.dir, ".env"))
	if err := e.rollback(func(o *Options) { o.DryRun = true }); err == nil || !strings.Contains(err.Error(), "predates") {
		t.Fatalf("a dry run refuses what a run refuses: %v", err)
	}
	e.schemaAt("6", nil, true)
	if err := e.rollback(func(o *Options) { o.DryRun = true }); err != nil || !strings.Contains(e.log.String(), "a run would go back to notes 1.0.0") {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if read(t, filepath.Join(e.dir, ".env")) != before {
		t.Error(".env changed")
	}
	if _, err := os.Stat(filepath.Join(e.dir, PrevDir)); err != nil {
		t.Error("the previous release is gone")
	}
}

func TestAnUnreadableRecordedSchemaGoesBackUnchecked(t *testing.T) {
	t.Parallel()
	e := upgraded(t)
	path := filepath.Join(e.dir, PrevDir, prevState)
	if err := os.WriteFile(path, []byte(`{"project_version": "1.0.0", "revision": 1, "schema": "5 OR true"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e.schemaAt("7", []string{"7"}, false)
	e.docker.schemaFiles = nil
	if err := e.rollback(nil); err != nil || !strings.Contains(e.log.String(), `"5 OR true", is not a migration number`) || len(e.docker.schemaFiles) != 0 {
		t.Fatalf("%v %d\n%s", err, len(e.docker.schemaFiles), e.log)
	}
}

func TestOneLifecycleRunAtATime(t *testing.T) {
	t.Parallel()
	e := upgraded(t)
	unlock, err := lockFile(filepath.Join(e.dir, lifecycleLock))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.rollback(nil); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("rollback: %v", err)
	}
	e.to("3.0.0", nil)
	if err := e.upgrade(nil); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("upgrade: %v", err)
	}
	unlock()
	// A schema command's container a killed run left holds the name the next check needs.
	e.docker.leftover = SchemaService
	if err := e.upgrade(nil); err == nil || !strings.Contains(err.Error(), "-"+SchemaService+" of an earlier run is still there") {
		t.Fatalf("leftover: %v", err)
	}
}

func TestARunDecidesFromWhatItLocked(t *testing.T) {
	t.Parallel()
	e := installed(t)
	e.to("2.0.0", nil)
	e.docker.inspected = func() { // another run finishes as this one takes the lock
		e.docker.inspected = nil
		f := e.readEnv()
		f.Set("ADMIN_EMAIL", "other@example.org")
		_ = f.WriteFile(filepath.Join(e.dir, ".env"))
	}
	if err := e.upgrade(nil); err != nil {
		t.Fatal(err)
	}
	if got := get(e.readEnv(), "ADMIN_EMAIL"); got != "other@example.org" {
		t.Fatalf("the upgrade wrote back what it read before the lock: %s", got)
	}
}

func TestAnUpgradeNeedsTheInstallerKept(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.self = ""
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	e.to("2.0.0", nil)
	if err := e.upgrade(nil); err == nil || !strings.Contains(err.Error(), "notes-stow is not the installer of notes 1.0.0 (revision 1), so a rollback could not go back") {
		t.Fatalf("none kept: %v", err)
	}
	// The new installer saved over the kept one.
	if err := os.WriteFile(filepath.Join(e.dir, "notes-stow"), []byte(read(t, e.self)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.upgrade(nil); err == nil || !strings.Contains(err.Error(), "it carries notes 2.0.0 (revision 1)") {
		t.Fatalf("the new one kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, PrevDir)); !errors.Is(err, os.ErrNotExist) {
		t.Error("an upgrade went ahead")
	}
}

func TestEveryLifecycleRunReadsTheInstallUnderTheLock(t *testing.T) {
	t.Parallel()
	for _, verb := range []string{"provision", "re-run"} {
		t.Run(verb, func(t *testing.T) {
			t.Parallel()
			e := installed(t)
			e.docker.inspected = func() { // an upgrade to 2.0.0 finishes as this run takes the lock
				e.docker.inspected = nil
				f := e.readEnv()
				f.Set("STOWAGE_PROJECT_VERSION", "2.0.0")
				_ = f.WriteFile(filepath.Join(e.dir, ".env"))
			}
			in := e.installer(nil)
			run := in.Provision
			if verb == "re-run" {
				run = in.Install
			}
			if err := run(context.Background()); err == nil || !strings.Contains(err.Error(), "runs notes 2.0.0") {
				t.Fatalf("%v", err)
			}
			if get(e.readEnv(), "STOWAGE_PROJECT_VERSION") != "2.0.0" {
				t.Error("the run wrote back what it read before the lock")
			}
		})
	}
}

func TestRollbackRefusesAMixedPreviousRelease(t *testing.T) {
	t.Parallel()
	e := upgraded(t)
	path := filepath.Join(e.dir, PrevDir, prevState)
	if err := os.WriteFile(path, []byte(`{"project_version": "0.9.0", "revision": 1, "schema": "5"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.rollback(nil); err == nil || !strings.Contains(err.Error(), "does not hold one release") {
		t.Fatalf("%v", err)
	}
}

func TestRollbackWhenThePreviousReleaseClaimsNothing(t *testing.T) {
	t.Parallel()
	none := func(s *spec.Spec) { s.Database.SchemaCompat = spec.Provider{Name: "none"} }
	e := newEnv(t)
	e.r, e.self = testInstallerWith(t, "1.0.0", none)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	e.to("2.0.0", nil)
	e.schemaAt("5", nil, true)
	if err := e.upgrade(nil); err != nil {
		t.Fatal(err)
	}
	e.schemaAt("6", nil, true)
	if err := e.rollback(nil); err != nil || !strings.Contains(e.log.String(), "going back unchecked: 1.0.0 declares no schema compatibility") {
		t.Fatalf("%v\n%s", err, e.log)
	}
	// Without a check, a dry run pulls nothing.
	e = newEnv(t)
	e.r, e.self = testInstallerWith(t, "1.0.0", none)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	e.to("2.0.0", none)
	e.r.Manifest.Artifacts.Images["api"] = "sha256:" + strings.Repeat("9", 64)
	if err := e.upgrade(nil); err != nil {
		t.Fatal(err)
	}
	e.docker.loaded, e.docker.calls = map[string]string{}, nil
	if err := e.rollback(func(o *Options) { o.DryRun = true }); err != nil || len(e.docker.ran("pull")) != 0 {
		t.Fatalf("%v: %d pulls", err, len(e.docker.ran("pull")))
	}
	// Nor when only the installed release checks: the previous one makes no claim to read.
	e = newEnv(t)
	e.r, e.self = testInstallerWith(t, "1.0.0", none)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	e.to("2.0.0", nil)
	e.r.Manifest.Artifacts.Images["api"] = "sha256:" + strings.Repeat("9", 64)
	e.schemaAt("5", nil, true)
	if err := e.upgrade(nil); err != nil {
		t.Fatal(err)
	}
	e.docker.loaded, e.docker.calls = map[string]string{}, nil
	e.schemaAt("6", nil, true)
	if err := e.rollback(func(o *Options) { o.DryRun = true }); err != nil || len(e.docker.ran("pull")) != 0 {
		t.Fatalf("%v: %d pulls", err, len(e.docker.ran("pull")))
	}
}

func TestLaterSchema(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		a, b string
		want bool
	}{{"6", "5", true}, {"5", "5", false}, {"4", "5", false}, {"10", "9", true}, {"9", "10", false}, {"20240102000000", "20240101000000", true}} {
		if laterSchema(c.a, c.b) != c.want {
			t.Errorf("%s after %s: %v", c.a, c.b, !c.want)
		}
	}
}

func FuzzLaterSchema(f *testing.F) {
	f.Add("6", "5")
	f.Add("10", "7")
	f.Fuzz(func(t *testing.T, a, b string) {
		if !schemaVersion.MatchString(a) || !schemaVersion.MatchString(b) {
			return
		}
		x, _ := new(big.Int).SetString(a, 10)
		y, _ := new(big.Int).SetString(b, 10)
		if laterSchema(a, b) != (x.Cmp(y) > 0) {
			t.Fatalf("%s after %s: %v", a, b, laterSchema(a, b))
		}
	})
}

func TestSchemaAnswersAreBounded(t *testing.T) {
	t.Parallel()
	e := installed(t)
	e.docker.schema = func(string, map[string]any) (string, error) {
		return `{"version": "` + strings.Repeat("1", maxSchemaAnswer) + `"}`, nil
	}
	if _, err := (schema{in: e.installer(nil), stack: e.readEnv()}).current(context.Background(), e.readEnv()); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("%v", err)
	}
	e.docker.schema = func(string, map[string]any) (string, error) { return `{"version": "0006"}`, nil }
	if v, err := (schema{in: e.installer(nil), stack: e.readEnv()}).current(context.Background(), e.readEnv()); v != "6" || err != nil {
		t.Fatalf("leading zeros: %q %v", v, err)
	}
	for _, v := range []string{`"5; DROP"`, "1234567890123456", "-1", `"0x10"`} {
		e.docker.schema = func(string, map[string]any) (string, error) { return `{"version": ` + v + `}`, nil }
		if _, err := (schema{in: e.installer(nil), stack: e.readEnv()}).current(context.Background(), e.readEnv()); err == nil || !strings.Contains(err.Error(), "not a migration number") {
			t.Errorf("%s: %v", v, err)
		}
	}
	e.docker.schema = func(string, map[string]any) (string, error) { return `{"version": 123456789012345}`, nil }
	if _, err := (schema{in: e.installer(nil), stack: e.readEnv()}).current(context.Background(), e.readEnv()); err != nil {
		t.Errorf("15 digits: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, SchemaCompose)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the schema command's compose file is left in the install")
	}
	e.docker.schema = func(string, map[string]any) (string, error) { return `{"version": "5; DROP"}`, nil }
	if _, err := (schema{in: e.installer(nil), stack: e.readEnv()}).current(context.Background(), e.readEnv()); err == nil || !strings.Contains(err.Error(), "not a migration number") {
		t.Fatalf("%v", err)
	}
	e.docker.schema = func(string, map[string]any) (string, error) { return "", exitCode(2) }
	if _, err := (schema{in: e.installer(nil), stack: e.readEnv()}).current(context.Background(), e.readEnv()); !errors.Is(err, command.ErrNoAnswer) {
		t.Fatalf("%v", err)
	}
}

func TestTwoFirstInstallsIntoOneDirectory(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.docker.inspected = func() { // the other install finishes as this one takes the lock
		e.docker.inspected = nil
		_ = os.WriteFile(filepath.Join(e.dir, ".env"), []byte("STOWAGE_NAME='notes'\n"), 0o600)
	}
	if err := e.install(nil); err == nil || !strings.Contains(err.Error(), "now holds an install") {
		t.Fatalf("%v", err)
	}
	if read(t, filepath.Join(e.dir, ".env")) != "STOWAGE_NAME='notes'\n" {
		t.Fatal("the other install's .env was overwritten")
	}
}

func TestTheKeptReleaseIsItsOwn(t *testing.T) {
	t.Parallel()
	e := installed(t)
	compose1 := read(t, filepath.Join(e.dir, "compose.yaml"))
	// An earlier upgrade stopped after writing its stack files.
	if err := os.WriteFile(filepath.Join(e.dir, "compose.yaml"), []byte("# another release's\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.to("2.0.0", nil)
	if err := e.upgrade(nil); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(e.dir, PrevDir, "compose.yaml")) != compose1 {
		t.Fatal("the kept compose file is not the kept release's")
	}
}

func TestUpgradeWriteOrder(t *testing.T) {
	t.Parallel()
	// The stack files fail: the install still runs the release it ran.
	e := installed(t)
	e.to("2.0.0", nil)
	front := filepath.Join(e.dir, FrontCompose)
	_ = os.Remove(front)
	_ = os.Mkdir(front, 0o755)
	if err := e.upgrade(nil); err == nil || get(e.readEnv(), "STOWAGE_PROJECT_VERSION") != "1.0.0" {
		t.Fatalf("%v: .env switched before the stack files", err)
	}
	// The installer cannot be kept: .env has switched, and the error says how to finish.
	e = installed(t)
	ctl1 := read(t, filepath.Join(e.dir, "notes-stow"))
	e.to("2.0.0", nil)
	e.self = filepath.Join(t.TempDir(), "gone")
	err := e.upgrade(nil)
	if err == nil || !strings.Contains(err.Error(), "the install runs 2.0.0 now: `"+e.self+" --dir") {
		t.Fatalf("%v", err)
	}
	if get(e.readEnv(), "STOWAGE_PROJECT_VERSION") != "2.0.0" || read(t, filepath.Join(e.dir, "notes-stow")) != ctl1 {
		t.Fatal("the installer was replaced before .env switched")
	}
}

func TestAnUpgradeKnowsItsInstaller(t *testing.T) {
	t.Parallel()
	e := installed(t)
	e.to("2.0.0", nil)
	e.self = ""
	if err := e.upgrade(nil); err == nil || !strings.Contains(err.Error(), "its path is not known") {
		t.Fatalf("%v", err)
	}
	if get(e.readEnv(), "STOWAGE_PROJECT_VERSION") != "1.0.0" {
		t.Fatal("the install changed")
	}
}

func TestAStoppedRollbackCanRunAgain(t *testing.T) {
	t.Parallel()
	e := upgraded(t)
	e.schemaAt("5", nil, true)
	ctl := filepath.Join(e.dir, "notes-stow")
	saved := read(t, ctl)
	_ = os.Remove(ctl)
	_ = os.Mkdir(ctl, 0o755) // the installer cannot be read: the rollback stops
	if err := e.rollback(nil); err == nil {
		t.Fatal("went back")
	}
	if get(e.readEnv(), "STOWAGE_PROJECT_VERSION") != "2.0.0" {
		t.Fatal(".env switched")
	}
	_ = os.Remove(ctl)
	_ = os.WriteFile(ctl, []byte(saved), 0o755)
	if err := e.rollback(nil); err != nil {
		t.Fatalf("again: %v", err)
	}
}

func TestAnOlderInstallerNamesTheKeptOne(t *testing.T) {
	t.Parallel()
	e := upgraded(t)
	e.r, e.self = testInstaller(t, "1.0.0")
	if err := e.install(nil); err == nil || !strings.Contains(err.Error(), "after this installer's 1.0.0 (revision 1): run the installer it keeps, "+filepath.Join(e.dir, "notes-stow")) {
		t.Fatalf("%v", err)
	}
}

func TestAnInstallStartsWhenItsInstallerCannotBeKept(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.self = filepath.Join(t.TempDir(), "gone")
	if err := e.install(nil); err != nil || !strings.Contains(e.log.String(), "the installer could not be kept") {
		t.Fatalf("%v\n%s", err, e.log)
	}
	e.log.Reset()
	if err := e.install(nil); err != nil || !strings.Contains(e.log.String(), "the installer could not be kept") {
		t.Fatalf("re-run: %v\n%s", err, e.log)
	}
}

// stoppedRollback is upgraded(t) where a rollback stopped right after .env switched back.
func stoppedRollback(t *testing.T) *env {
	t.Helper()
	e := upgraded(t)
	prev := filepath.Join(e.dir, PrevDir)
	for _, f := range append(slices.Clone(stackFiles), ".env") {
		if err := copyFile(filepath.Join(prev, f), filepath.Join(e.dir, f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func TestAStoppedRollbackIsFinished(t *testing.T) {
	t.Parallel()
	e := stoppedRollback(t)
	// The kept installer (2.0.0) no longer matches; an upgrade names the way out.
	if err := e.upgrade(nil); err == nil || !strings.Contains(err.Error(), "an upgrade from or a rollback to notes 1.0.0 stopped before it finished") {
		t.Fatalf("upgrade: %v", err)
	}
	ctl1 := read(t, filepath.Join(e.dir, PrevDir, "notes-stow"))
	e.docker.calls = nil
	if err := e.rollback(nil); err != nil || !strings.Contains(e.log.String(), "finishing the upgrade from or the rollback to notes 1.0.0") {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if read(t, filepath.Join(e.dir, "notes-stow")) != ctl1 || len(e.docker.ran("compose --project-directory")) == 0 {
		t.Fatal("not finished")
	}
	if _, err := os.Stat(filepath.Join(e.dir, PrevDir)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the previous release is still kept")
	}
}

func TestARollbackStoppedAtTheEndKeepsWhatFailed(t *testing.T) {
	t.Parallel()
	e := upgraded(t)
	e.schemaAt("5", nil, true)
	if err := e.rollback(nil); err != nil {
		t.Fatal(err)
	}
	failed := read(t, filepath.Join(e.dir, FailedDir, ".env"))
	// As if it stopped before removing what it kept: .env, the installer and the kept release are
	// all 1.0.0.
	e.r, e.self = testInstaller(t, "1.0.0")
	if err := e.installer(nil).keepPrev(e.readEnv(), state{ProjectVersion: "1.0.0", Revision: 1, Schema: "5"}, &keptRelease{Manifest: e.r.Manifest, Release: e.r}); err != nil {
		t.Fatal(err)
	}
	if err := e.rollback(nil); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(e.dir, FailedDir, ".env")) != failed {
		t.Fatal("the release the rollback left was overwritten")
	}
}

func TestAnUpgradeStoppedBeforeKeepingItsInstaller(t *testing.T) {
	t.Parallel()
	e := installed(t)
	e.to("2.0.0", nil)
	e.self = filepath.Join(t.TempDir(), "gone")
	if err := e.upgrade(nil); err == nil {
		t.Fatal("upgraded")
	}
	e.r, e.self = testInstaller(t, "1.0.0") // the kept one
	if err := e.install(nil); err == nil || !strings.Contains(err.Error(), "the upgrade stopped before keeping its own; run the installer of 2.0.0") {
		t.Fatalf("%v", err)
	}
}

func TestTheKeptInstallerCarriesTheRelease(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, e.self = testInstaller(t, "2.0.0") // the path now holds another release's installer
	if err := e.install(nil); err != nil || !strings.Contains(e.log.String(), "carries notes 2.0.0 (revision 1), not notes 1.0.0") {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "notes-stow")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("another release's installer was kept")
	}
}

func TestUpgradeRefusesAnInstallVersionTheSchemeDoesNotRead(t *testing.T) {
	t.Parallel()
	e := installed(t)
	env := e.readEnv()
	env.Set("STOWAGE_PROJECT_VERSION", "banana")
	if err := env.WriteFile(filepath.Join(e.dir, ".env")); err != nil {
		t.Fatal(err)
	}
	e.to("2.0.0", nil)
	err := e.upgrade(nil)
	if err == nil || !strings.Contains(err.Error(), `the install's version "banana"`) {
		t.Fatalf("%v", err)
	}
}

// An upgrade that stopped after writing the next release's stack files but before .env switched
// is finished as a stopped rollback: the kept release's stack files come back.
func TestFinishingAStoppedUpgradeRestoresTheStackFiles(t *testing.T) {
	t.Parallel()
	e := installed(t)
	in := e.installer(nil)
	env, err := in.open()
	if err != nil {
		t.Fatal(err)
	}
	if err := in.keepPrev(env, state{ProjectVersion: "1.0.0", Revision: 1}, &keptRelease{Manifest: e.r.Manifest, Release: e.r}); err != nil {
		t.Fatal(err)
	}
	want := read(t, filepath.Join(e.dir, "compose.yaml"))
	if err := os.WriteFile(filepath.Join(e.dir, "compose.yaml"), []byte(want+"# the next release\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.rollback(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if read(t, filepath.Join(e.dir, "compose.yaml")) != want {
		t.Error("the next release's compose file is left")
	}
	if _, err := os.Stat(filepath.Join(e.dir, PrevDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("PrevDir is left: %v", err)
	}
}

// An upgrade to a release that no longer declares an input, or a key its provision hook returned,
// drops the recorded value, saying so, and asks nothing; a rollback takes both back.
func TestUpgradeDropsWhatTheReleaseRemoved(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.schemaAt("5", nil, true)
	e.to("1.0.0", func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "OLD_KNOB", Prompt: "old"}, spec.Input{Key: "OLD_ADV", Prompt: "adv", Advanced: true})
		s.Hooks.Provision.Config["returns"] = []any{"OLD_KEY", "KEPT_KEY"}
	})
	e.docker.hookOut = "OLD_KEY=a\nKEPT_KEY=b\n"
	if err := e.install(func(o *Options) { o.Set = map[string]string{"OLD_KNOB": "x", "OLD_ADV": "y"} }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	e.to("2.0.0", func(s *spec.Spec) { s.Hooks.Provision.Config["returns"] = []any{"KEPT_KEY"} })
	e.docker.hookOut = ""
	if err := e.upgrade(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	after := e.readEnv()
	for _, k := range []string{"OLD_KNOB", "OLD_ADV"} {
		if _, ok := after.Get(k); ok {
			t.Errorf("%s is still recorded", k)
		}
		if !strings.Contains(e.log.String(), "input "+k+" is not in notes 2.0.0: dropped") {
			t.Errorf("no line for %s:\n%s", k, e.log)
		}
	}
	rec := read(t, filepath.Join(e.dir, ProvisionEnv))
	if strings.Contains(rec, "OLD_KEY") || !strings.Contains(rec, "KEPT_KEY='b'") || strings.Contains(strings.Join(e.docker.hookEnv, " "), "OLD_KEY") {
		t.Errorf("provision.env %q, hook given %v", rec, e.docker.hookEnv)
	}
	if !strings.Contains(e.log.String(), "no longer returns OLD_KEY: dropped") {
		t.Errorf("no line for OLD_KEY:\n%s", e.log)
	}
	// Back on 1.0.0, the values it reads come back.
	if err := e.rollback(nil); err != nil {
		t.Fatalf("rollback: %v\n%s", err, e.log)
	}
	if get(e.readEnv(), "OLD_KNOB") != "x" || !strings.Contains(read(t, filepath.Join(e.dir, ProvisionEnv)), "OLD_KEY='a'") {
		t.Errorf("rollback did not take them back: %v", e.readEnv().Keys())
	}
}

// An input the release now records as a secret is no removed input: its value stays. One now a
// safe default is: the safe value replaces the operator's.
func TestAnInputMovedToASecretIsKept(t *testing.T) {
	t.Parallel()
	from := &Release{Spec: &spec.Spec{Inputs: []spec.Input{{Key: "DB_PASSWORD"}, {Key: "MODE"}, {Key: "GONE"}}}}
	to := &Release{Spec: &spec.Spec{Secrets: []spec.Secret{{Key: "DB_PASSWORD", Gen: "hex16"}}, SafeDefaults: map[string]string{"MODE": "safe"}}}
	if r := removedBy(from, to); strings.Join(r.inputs, ",") != "GONE,MODE" {
		t.Errorf("removed %v", r.inputs)
	}
}

// An install a restore left half restored is not upgraded: a restore again finishes it, onto the
// release it runs only.
func TestUpgradeRefusesVolumesHalfRestored(t *testing.T) {
	t.Parallel()
	e := installed(t)
	before, _ := os.ReadFile(filepath.Join(e.dir, envName))
	if err := os.WriteFile(filepath.Join(e.dir, volumesSplitFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e.to("2.0.0", nil)
	if err := e.upgrade(nil); err == nil || !strings.Contains(err.Error(), "half restored") {
		t.Fatalf("err = %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(e.dir, envName)); !bytes.Equal(before, after) {
		t.Error("the refused upgrade changed .env")
	}
}

// anotherStowages makes the install's kept installer one another Stowage built: its manifest
// records Stowage 0.0.9, and its spec a field this one does not know.
func anotherStowages(t *testing.T, e *env) []byte {
	t.Helper()
	return anotherStowagesAt(t, filepath.Join(e.dir, "notes-stow"))
}

// anotherStowagesAt makes the installer at ctl one another Stowage built, as anotherStowages.
func anotherStowagesAt(t *testing.T, ctl string) []byte {
	t.Helper()
	files, err := payload.Read(ctl)
	if err != nil {
		t.Fatal(err)
	}
	m, err := payloadRelease(files)
	if err != nil {
		t.Fatal(err)
	}
	m.StowageVersion = "0.0.9"
	if files[payload.Release], err = m.Encode(); err != nil {
		t.Fatal(err)
	}
	files[payload.Spec] = append([]byte(`{"Retired":1,`), files[payload.Spec][1:]...)
	var buf bytes.Buffer
	if err := payload.Write(&buf, strings.NewReader("exe"), files); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ctl, buf.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// An install of a release another Stowage built upgrades without reading its spec, and goes back
// by its own installer: a re-run of it starts it. What only its spec tells is not checked, but
// the barriers the current release's database records still refuse.
func TestAReleaseOfAnotherStowageUpgradesAndGoesBack(t *testing.T) {
	t.Parallel()
	e := installed(t)
	old := anotherStowages(t, e)
	e.to("2.0.0", nil)
	if err := e.upgrade(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	const foreign = `notes 1.0.0 (revision 1), built by Stowage 0.0.9, carries a spec this one does not read (its own installer does): payload spec.json: json: unknown field "Retired" (an installer of another Stowage?)`
	for _, want := range []string{"the inputs and provision hook returns notes 2.0.0 removes are not known (" + foreign + "): kept"} {
		if !strings.Contains(e.log.String(), want) {
			t.Errorf("not logged: %s\n%s", want, e.log)
		}
	}
	if !bytes.Equal([]byte(read(t, filepath.Join(e.dir, PrevDir, "notes-stow"))), old) {
		t.Error("the upgrade did not keep the older installer")
	}
	var ran [][]string
	var exe string
	start := func(o *Options) {
		o.Exec = func(_ context.Context, x string, args []string) error {
			if args[0] == "--help" { // reading its own payload: the kept installer, in PrevDir by then
				if x != filepath.Join(e.dir, PrevDir, "notes-stow") {
					return errors.New("probed " + x)
				}
				return nil
			}
			exe, ran = x, append(ran, args)
			return nil
		}
	}
	e.schemaAt("7", []string{"7 (drop x)"}, true)
	if err := e.rollback(start); err == nil || !strings.Contains(err.Error(), "the database carries 7 (drop x)") || len(ran) != 0 {
		t.Fatalf("a barrier: %v, ran %v", err, ran)
	}
	e.log.Reset()
	e.schemaAt("6", nil, true)
	if err := e.rollback(start); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if want := [][]string{{"--dir", e.dir, "--no-input", "--no-provision"}}; !slices.EqualFunc(ran, want, slices.Equal) || exe != filepath.Join(e.dir, "notes-stow") {
		t.Errorf("ran %s %v, want %v", exe, ran, want)
	}
	if !bytes.Equal([]byte(read(t, exe)), old) || get(e.readEnv(), "STOWAGE_PROJECT_VERSION") != "1.0.0" {
		t.Error("the install does not record 1.0.0 with its own installer")
	}
	for _, want := range []string{foreign + ": what only its spec tells is not checked",
		"starting notes 1.0.0 (revision 1) with its own installer: " + exe + " --dir " + e.dir + " --no-input --no-provision",
		"going back unchecked: whether 1.0.0 reads the newer schema 6 is not known: another Stowage built it"} {
		if !strings.Contains(e.log.String(), want) {
			t.Errorf("not logged: %s\n%s", want, e.log)
		}
	}
}

// A kept installer whose payload this Stowage refuses is another Stowage's when it reads that
// payload itself, and goes back unchecked; one that does not read it either (another project's
// spec, one cut short) refuses the rollback before anything changes.
func TestARollbackRefusesAPreviousInstallerItCannotTrust(t *testing.T) {
	t.Parallel()
	other := func(b []byte) []byte { return bytes.Replace(b, []byte(`"Name":"notes"`), []byte(`"Name":"other"`), 1) }
	for name, c := range map[string]struct {
		spec   func([]byte) []byte
		itself error  // what the kept installer says, reading its own payload
		refuse string // "" = goes back
	}{
		"another project":   {other, errors.New("exit status 1"), `payload: the spec is "other"'s, the release "notes"'s, nor does that installer read it (exit status 1)`},
		"cut short":         {func(b []byte) []byte { return b[:len(b)/2] }, errors.New("exit status 1"), "payload spec.json: unexpected EOF"},
		"another stowage's": {other, nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := upgraded(t)
			ctl := filepath.Join(e.dir, PrevDir, "notes-stow")
			files, err := payload.Read(ctl)
			if err != nil {
				t.Fatal(err)
			}
			m, err := payloadRelease(files)
			if err != nil {
				t.Fatal(err)
			}
			m.StowageVersion = "0.0.9"
			if files[payload.Release], err = m.Encode(); err != nil {
				t.Fatal(err)
			}
			files[payload.Spec] = c.spec(files[payload.Spec])
			var buf bytes.Buffer
			if err := payload.Write(&buf, strings.NewReader("exe"), files); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(ctl, buf.Bytes(), 0o755); err != nil {
				t.Fatal(err)
			}
			err = e.rollback(func(o *Options) {
				o.Exec = func(_ context.Context, exe string, args []string) error {
					if args[0] == "--help" {
						if exe != ctl {
							return errors.New("probed " + exe)
						}
						return c.itself
					}
					return nil
				}
			})
			if c.refuse == "" {
				if err != nil || !strings.Contains(e.log.String(), "built by Stowage 0.0.9, carries a spec this one does not read (its own installer does)") {
					t.Fatalf("%v\n%s", err, e.log)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.refuse) {
				t.Fatalf("%v\n%s", err, e.log)
			}
			if get(e.readEnv(), "STOWAGE_PROJECT_VERSION") != "2.0.0" {
				t.Error("the refused rollback changed .env")
			}
			if _, err := os.Stat(filepath.Join(e.dir, PrevDir)); err != nil {
				t.Errorf("the refused rollback removed what it keeps: %v", err)
			}
		})
	}
}

// An install's kept installer carrying no key falls back to this installer's keys only when this
// Stowage reads its payload: another Stowage's may keep its keys where this one does not look.
func TestTheTrustOfAnotherStowagesReleaseIsNotGuessed(t *testing.T) {
	t.Parallel()
	e := installed(t)
	if _, _, err := e.installer(nil).installTrust(context.Background(), "notes", "1.0.0", "1"); err != nil {
		t.Fatalf("this Stowage's release without keys: %v", err)
	}
	anotherStowages(t, e)
	if _, _, err := e.installer(nil).installTrust(context.Background(), "notes", "1.0.0", "1"); err == nil || !strings.Contains(err.Error(), "the keys the install trusts cannot be told") {
		t.Fatalf("another Stowage's release without keys: %v", err)
	}
}

// A kept installer reads its own payload when its --help exits 0; one that exits otherwise says
// why (its stderr, capped); one that does not answer within the bound, or a run stopped
// meanwhile, is no verdict on the payload.
func TestAKeptInstallerReadsItself(t *testing.T) {
	readsItselfWithin = 200 * time.Millisecond
	t.Cleanup(func() { readsItselfWithin = 30 * time.Second })
	dir := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	o := &Options{}
	ctx := context.Background()
	if err := o.readsItself(ctx, script("reads", `[ "$1" = --help ] && echo usage`)); err != nil {
		t.Errorf("one that reads it: %v", err)
	}
	if err := o.readsItself(ctx, script("refuses", "echo 'error: payload spec.json: bad' >&2; exit 1")); err == nil || err.Error() != "exit status 1: error: payload spec.json: bad" {
		t.Errorf("one that refuses it: %v", err)
	}
	if err := o.readsItself(ctx, script("silent", "exit 3")); err == nil || err.Error() != "exit status 3" {
		t.Errorf("one that refuses it silently: %v", err)
	}
	if err := o.readsItself(ctx, script("loud", "head -c 9000 /dev/zero | tr '\\0' x >&2; exit 1")); err == nil || !strings.HasSuffix(err.Error(), " …") || len(err.Error()) > 4200 {
		t.Errorf("one that says too much: %d bytes", len(fmt.Sprint(err)))
	}
	slow := script("slow", "exec sleep 5")
	if err := o.readsItself(ctx, slow); !errors.Is(err, errProbeTimeout) {
		t.Errorf("one that does not answer: %v", err)
	}
	stop, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := o.readsItself(stop, slow); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a run stopped meanwhile: %v", err)
	}
	done, cancelDone := context.WithCancel(ctx)
	cancelDone()
	if err := o.readsItself(done, slow); !errors.Is(err, context.Canceled) {
		t.Errorf("a run already stopped: %v", err)
	}
}

// In byo mode a route the release adds is served on a domain the install's certificate covers
// alone.
func TestUpgradeAddsAByoRouteTheCertificateCovers(t *testing.T) {
	t.Parallel()
	cert, key := writeCert(t, time.Now().Add(90*24*time.Hour), "*.example.com")
	e := newEnv(t)
	e.docker.dump = "DUMP"
	e.schemaAt("5", nil, true)
	if err := e.install(func(o *Options) { tlsDomains(o); o.TLS, o.Cert, o.Key = "byo", cert, key }); err != nil {
		t.Fatal(err)
	}
	e.to("2.0.0", docsRoute(9000))
	if err := e.upgrade(func(o *Options) { o.Set = map[string]string{"DOCS_DOMAIN": "docs.example.org"} }); err == nil || !strings.Contains(err.Error(), "notes 2.0.0 adds the route docs: the install's certificate does not cover docs.example.org (DOCS_DOMAIN)") {
		t.Fatalf("a domain the certificate does not cover: %v", err)
	}
	if err := e.upgrade(func(o *Options) { o.Set = map[string]string{"DOCS_DOMAIN": "docs.example.com"} }); err != nil {
		t.Fatalf("a covered domain: %v\n%s", err, e.log)
	}
	// A release adding no route never reads the certificate (expired, or gone).
	if err := os.Remove(filepath.Join(e.dir, TLSDir, "cert.pem")); err != nil {
		t.Fatal(err)
	}
	e.to("3.0.0", docsRoute(9000))
	if err := e.upgrade(nil); err != nil {
		t.Fatalf("no route added: %v", err)
	}
}

// An upgrade refuses, before anything changes, a release whose services ask more whole cores
// than the host has beside CPU 0's; one that fits, a host it cannot read (no tune) and Docker
// Desktop (no cores) are the start's to judge.
func TestAnUpgradeRefusesCoresTheHostLacks(t *testing.T) {
	t.Parallel()
	check := func(cpus int, info string, withTune bool) error {
		root := tuneRoot(t)
		if err := os.MkdirAll(filepath.Join(root, "sys/devices/system/cpu"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "sys/devices/system/cpu/online"), []byte("0-2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		r := testRelease(t, "2.0.0")
		r.Spec.Services = map[string]spec.Service{"echo": {CPUs: cpus}}
		d := &fakeDocker{}
		dockerInfo(d, info)
		in := &Installer{R: r, O: Options{Docker: d, Log: io.Discard}}
		if withTune {
			in.O.Tune = fakeTune(root, 0)
		}
		err := in.coresUpgrade(context.Background())
		if in.tuned != nil {
			t.Errorf("the check left its reading of the host for the upgrade's start")
		}
		return err
	}
	const engine = `27.3.1|Ubuntu 24.04 LTS|["name=seccomp,profile=builtin"]`
	if err := check(2, engine, true); err != nil {
		t.Errorf("2 cores of 3, CPU 0's kept: %v", err)
	}
	err := check(3, engine, true)
	if err == nil || !strings.Contains(err.Error(), "2.0.0 cannot run here, and nothing was changed: services.*.cpus: echo ask 3 whole cores, and this host has 2 beside the one of CPU 0") {
		t.Errorf("3 cores of 3: %v", err)
	}
	if err := check(3, engine, false); err != nil {
		t.Errorf("no host to read: %v", err)
	}
	if err := check(3, `27.3.1|Docker Desktop|[]`, true); err != nil {
		t.Errorf("Docker Desktop: %v", err)
	}
	// An engine not answering refuses, nothing changed: the upgrade is tried again.
	if err := check(2, "", true); err == nil || !strings.Contains(err.Error(), "reading this host's cores for the services' cpus: the Docker engine is not reachable") {
		t.Errorf("an engine not answering: %v", err)
	}
	// Docker Desktop stopped, which its CLI's context tells: no cores to check.
	root := tuneRoot(t)
	d := &fakeDocker{answer: func(a string) ([]byte, bool, error) {
		switch {
		case strings.HasPrefix(a, "info --format"):
			return nil, true, errors.New("cannot connect")
		case a == "context show":
			return []byte("desktop-linux\n"), true, nil
		}
		return nil, false, nil
	}}
	r := testRelease(t, "2.0.0")
	r.Spec.Services = map[string]spec.Service{"echo": {CPUs: 64}}
	if err := (&Installer{R: r, O: Options{Docker: d, Log: io.Discard, Tune: fakeTune(root, 0)}}).coresUpgrade(context.Background()); err != nil {
		t.Errorf("Docker Desktop stopped: %v", err)
	}
	if err := check(3, "garbled", true); err == nil || !strings.Contains(err.Error(), "reading this host's cores for the services' cpus: docker info") {
		t.Errorf("an engine answering garbled: %v", err)
	}
}
