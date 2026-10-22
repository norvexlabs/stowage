package install

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/chown"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// labels are the content labels contentCompose gives each service of m.
func labels(t *testing.T, m string, environ ...string) map[string]string {
	t.Helper()
	var model composeModel
	if err := json.Unmarshal([]byte(m), &model); err != nil {
		t.Fatal(err)
	}
	raw, err := contentCompose(&model, environ, []byte("key"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]struct {
			Labels map[string]string `yaml:"labels"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	out := map[string]string{}
	for name, s := range doc.Services {
		out[name] = s.Labels[ContentLabel]
	}
	return out
}

func stack(content, file string) string {
	return `{"services": {
		"a": {"configs": [{"source": "c", "target": "/c"}]},
		"b": {"secrets": [{"source": "s", "target": "/run/secrets/s"}]},
		"f": {"configs": [{"source": "file", "target": "/f"}]},
		"n": {}},
	"configs": {"c": {"content": "` + content + `"}, "file": {"file": "` + file + `"}},
	"secrets": {"s": {"environment": "SEC"}}}`
}

func TestContentLabelsFollowWhatComposeRenders(t *testing.T) {
	t.Parallel()
	first := labels(t, stack("one", "/x"), "SEC=x")
	if len(first) != 2 || first["a"] == "" || first["b"] == "" || first["a"] == first["b"] {
		t.Fatalf("labels: %v (a config's content and an environment secret, never a file or nothing)", first)
	}
	if again := labels(t, stack("one", "/y"), "SEC=x"); again["a"] != first["a"] || again["b"] != first["b"] {
		t.Errorf("the same rendered content changed a label: %v → %v", first, again)
	}
	if c := labels(t, stack("two", "/x"), "SEC=x"); c["a"] == first["a"] || c["b"] != first["b"] {
		t.Errorf("a config's content changed: %v → %v", first, c)
	}
	// The environment's last value is Compose's.
	if s := labels(t, stack("one", "/x"), "SEC=x", "SEC=y"); s["b"] == first["b"] || s["a"] != first["a"] {
		t.Errorf("a secret's value changed: %v → %v", first, s)
	}
	// The label is keyed: the same content under another install's key labels differently.
	var model composeModel
	if err := json.Unmarshal([]byte(stack("one", "/x")), &model); err != nil {
		t.Fatal(err)
	}
	other, err := contentCompose(&model, []string{"SEC=x"}, []byte("another key"), nil)
	if err != nil || strings.Contains(string(other), first["a"]) || strings.Contains(string(other), first["b"]) {
		t.Errorf("another key gave the same labels: %s, %v", other, err)
	}
	none, err := contentCompose(&composeModel{}, nil, []byte("key"), nil)
	if err != nil || string(none) != contentPlaceholder {
		t.Errorf("no rendered file: %q, %v", none, err)
	}
}

// A data volume with a uid is given it once the containers are made, before they start; a
// volume without one, or that no service mounts, is left alone.
func TestDataVolumesAreGivenTheirUID(t *testing.T) {
	e := newEnv(t)
	e.docker.model = `{"services": {
		"postgres": {"volumes": [{"type": "volume", "source": "postgres", "target": "/var/lib/postgresql/data"}]},
		"web": {"volumes": [{"type": "volume", "source": "uploads", "target": "/data/uploads"}, {"type": "bind", "source": "/srv", "target": "/srv"}]}},
	"volumes": {"postgres": {"name": "p_postgres"}, "uploads": {"name": "p_uploads"}, "cache": {"name": "p_cache"}}}`
	var script, image string
	e.docker.owner = func(c Cmd) ([]byte, error) {
		script, image = c.Args[len(c.Args)-1], c.Args[len(c.Args)-3]
		if !strings.Contains(strings.Join(c.Args, " "), " --volume p_postgres:/v/0 --volume p_uploads:/v/1 ") {
			t.Errorf("volumes: %v", c.Args)
		}
		return []byte("p_uploads\n"), nil
	}
	if err := e.install(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	var order []string
	for _, c := range e.docker.calls {
		a := strings.Join(c.Args, " ")
		switch {
		case strings.HasSuffix(a, " up --no-start --remove-orphans"):
			order = append(order, "create")
		case strings.HasPrefix(a, "run --rm --pull never --network none --user 0 "):
			order = append(order, "own")
		case strings.Contains(a, " up --detach --wait "):
			order = append(order, "start")
		}
	}
	if strings.Join(order, " ") != "create own start" {
		t.Fatalf("order: %v", order)
	}
	for _, want := range []string{`if [ "$(stat -c %u /v/0)" != 70 ]; then chown -hR 70 /v/0; echo p_postgres; fi`, `!= 1000 ]; then chown -hR 1000 /v/1;`} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	if want := e.installer(nil).imageRef(get(e.readEnv(), "STOWAGE_REGISTRY"), chown.Key); image != want {
		t.Errorf("the chown runs in %s, not the volume helper %s", image, want)
	}
	if !strings.Contains(e.log.String(), "volume p_uploads given to its uid") || strings.Contains(e.log.String(), "volume p_postgres") {
		t.Errorf("log:\n%s", e.log)
	}

	// A volume with a uid that no service mounts (its profile off) is left alone.
	e = newEnv(t)
	e.docker.model = `{"services": {"postgres": {"volumes": [{"type": "volume", "source": "postgres", "target": "/d"}]}}, "volumes": {"postgres": {"name": "p_postgres"}, "uploads": {"name": "p_uploads"}}}`
	var args string
	e.docker.owner = func(c Cmd) ([]byte, error) { args = strings.Join(c.Args, " "); return nil, nil }
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(args, "p_postgres:/v/0") || strings.Contains(args, "p_uploads") {
		t.Errorf("only the mounted volume is given its uid: %s", args)
	}

	// A volume the engine holds keeps its owner, its data's; an external one is never the stack's.
	e = newEnv(t)
	e.docker.model = `{"services": {
		"postgres": {"volumes": [{"type": "volume", "source": "postgres", "target": "/d"}]},
		"web": {"volumes": [{"type": "volume", "source": "uploads", "target": "/u"}]}},
	"volumes": {"postgres": {"name": "p_postgres"}, "uploads": {"name": "shared", "external": true}}}`
	e.docker.volumes = "other\np_postgres\n"
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if n := len(e.docker.ran("run --rm --pull never --network none --user 0 ")); n != 0 {
		t.Errorf("%d chowns of volumes that exist or are external", n)
	}

	// Without a uid, nothing is made first nor given.
	e = newEnv(t)
	e.docker.model = `{"services": {"web": {"volumes": [{"type": "volume", "source": "cache", "target": "/c"}]}}, "volumes": {"cache": {"name": "p_cache"}}}`
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if n := len(e.docker.ran("run --rm --pull never --network none --user 0 ")); n != 0 || len(e.docker.ran("compose --project-directory "+e.dir+" -f "+filepath.Join(e.dir, "compose.yaml")+" -f "+filepath.Join(e.dir, FrontCompose)+" -f "+filepath.Join(e.dir, ContentCompose)+" up --no-start")) != 0 {
		t.Errorf("a stack without a volume of a uid: %d chowns, calls %v", n, e.docker.calls)
	}
}

// The labels of what Compose renders are written before the stack starts, and kept by a rewrite of
// the stack's files.
func TestTheContentLabelsAreWrittenBeforeTheStackStarts(t *testing.T) {
	e := newEnv(t)
	e.docker.model = `{"services": {"web": {"configs": [{"source": "c", "target": "/c"}]}}, "configs": {"c": {"content": "x"}}}`
	var at string
	e.docker.onUp = func() error {
		raw, _ := os.ReadFile(filepath.Join(e.dir, ContentCompose))
		at = string(raw)
		return nil
	}
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(at, ContentLabel+": hmac-sha256:") {
		t.Fatalf("%s as the stack started:\n%s", ContentCompose, at)
	}
	if err := e.installer(nil).writeStack(e.readEnv()); err != nil {
		t.Fatal(err)
	}
	// The same compose file keeps its labels; another starts without them, as they may name
	// services it no longer has.
	if raw, _ := os.ReadFile(filepath.Join(e.dir, ContentCompose)); string(raw) != at {
		t.Errorf("rewriting the same compose file changed the labels:\n%s", raw)
	}
	other := e.installer(nil)
	other.R.Compose = append(slices.Clone(other.R.Compose), "\n# another release\n"...)
	if err := other.writeStack(e.readEnv()); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(e.dir, ContentCompose)); string(raw) != contentPlaceholder {
		t.Errorf("another compose file kept the labels:\n%s", raw)
	}
	// Rendered where none was written yet, it is the placeholder every compose command can read.
	dir := t.TempDir()
	if err := e.installer(nil).renderStack(e.readEnv(), dir); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, ContentCompose)); string(raw) != contentPlaceholder {
		t.Errorf("a new stack's %s: %q", ContentCompose, raw)
	}
}

// An install a Stowage without content labels made has no such file: compose is not told of it
// until a start writes it. The key of the labels is the install's own, private and kept.
func TestTheContentFileAndKeyOfAnInstall(t *testing.T) {
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	in := e.installer(nil)
	if !strings.Contains(strings.Join(in.composeBase(), " "), ContentCompose) {
		t.Fatal("compose is not told of the written labels")
	}
	if err := os.Remove(filepath.Join(e.dir, ContentCompose)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(in.composeBase(), " "), ContentCompose) {
		t.Fatal("compose is told of labels the install does not have")
	}
	if err := e.install(nil); err != nil {
		t.Fatalf("a re-run of an install without labels: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, ContentCompose)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(e.dir, ContentKey))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%s: %v %v", ContentKey, fi, err)
	}
	k1, _ := in.contentKey()
	k2, _ := in.contentKey()
	if len(k1) != 32 || string(k1) != string(k2) {
		t.Errorf("the key is not kept: %x, %x", k1, k2)
	}
}

// The stack files kept for a rollback take their labels with them; where none were kept (a
// release an earlier Stowage kept), the install's go, never naming services the compose file
// restored may lack.
func TestRestoredStackTakesItsLabels(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	in := e.installer(nil)
	if err := os.MkdirAll(e.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	kept := t.TempDir()
	for _, f := range stackFiles {
		testutil.Write(t, kept, f, "kept "+f)
		testutil.Write(t, e.dir, f, "current "+f)
	}
	if err := in.restoreStack(kept, false); err != nil {
		t.Fatal(err)
	}
	for _, f := range stackFiles {
		if raw, _ := os.ReadFile(filepath.Join(e.dir, f)); string(raw) != "kept "+f {
			t.Errorf("%s: %q", f, raw)
		}
	}
	testutil.Write(t, e.dir, ContentCompose, "current labels")
	if err := os.Remove(filepath.Join(kept, ContentCompose)); err != nil {
		t.Fatal(err)
	}
	if err := in.restoreStack(kept, false); err != nil {
		t.Fatalf("kept without labels: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, ContentCompose)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the install's labels outlived their compose file: %v", err)
	}
	if err := os.Remove(filepath.Join(kept, Caddyfile)); err != nil {
		t.Fatal(err)
	}
	if err := in.restoreStack(kept, false); err == nil {
		t.Error("a kept stack without its Caddyfile was taken")
	}
	if err := in.restoreStack(kept, true); err != nil {
		t.Errorf("missingOK: %v", err)
	}
}

// A start cut off between making a volume and giving it its uid gives it on the next start, though
// the volume then exists.
func TestAVolumeAwaitingItsUIDGetsItNextStart(t *testing.T) {
	e := newEnv(t)
	e.docker.model = `{"services": {"web": {"volumes": [{"type": "volume", "source": "uploads", "target": "/u"}]}}, "volumes": {"uploads": {"name": "p_uploads"}}}`
	e.docker.owner = func(Cmd) ([]byte, error) { return nil, errors.New("cut off") }
	if err := e.install(nil); err == nil {
		t.Fatal("the install went on without the volume's uid")
	}
	if raw, _ := os.ReadFile(filepath.Join(e.dir, VolumesPending)); string(raw) != "p_uploads\n" {
		t.Fatalf("%s: %q", VolumesPending, raw)
	}
	e.docker.volumes = "p_uploads\n" // made by the first start
	var ran bool
	e.docker.owner = func(c Cmd) ([]byte, error) {
		ran = strings.Contains(strings.Join(c.Args, " "), "p_uploads:/v/0")
		return nil, nil
	}
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("the volume awaiting its uid was not given it")
	}
	if _, err := os.Stat(filepath.Join(e.dir, VolumesPending)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s outlived the chown: %v", VolumesPending, err)
	}
}

// A release an earlier Stowage published carries no volume helper: its volumes keep the owner
// Docker gave them, as they did, and the start goes on.
func TestWithoutAVolumeHelperTheVolumesKeepTheirOwner(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	in := e.installer(nil)
	given, err := in.ownVolumes(context.Background(), NewEnvFile(), []string{"p_uploads"}, []int{1000})
	if err != nil || given {
		t.Fatalf("given %v, %v", given, err)
	}
	if !strings.Contains(e.log.String(), "carries no volume helper: p_uploads keep the owner Docker gave them until a release with one starts") || len(e.docker.calls) != 0 {
		t.Errorf("log %q, calls %v", e.log, e.docker.calls)
	}
}

// A start of a release without the helper leaves its new volumes awaiting their uid, which a
// release with one gives them.
func TestWithoutAVolumeHelperTheVolumesAwaitTheirUID(t *testing.T) {
	e := newEnv(t)
	e.r, e.self = testInstallerOf(t, "1.0.0", 1, nil, func(m *release.Manifest) {
		delete(m.Artifacts.Images, chown.Key)
		delete(m.Artifacts.Configs, chown.Key)
		delete(m.Artifacts.Manifests, chown.Key)
	})
	e.docker.model = `{"services": {"web": {"volumes": [{"type": "volume", "source": "uploads", "target": "/u"}]}}, "volumes": {"uploads": {"name": "p_uploads"}}}`
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(e.dir, VolumesPending)); string(raw) != "p_uploads\n" {
		t.Errorf("%s: %q", VolumesPending, raw)
	}
}

// noUID is examples/minimal with no data volume of a uid: its releases carry no volume helper.
func noUID(s *spec.Spec) {
	for k, v := range s.Data.Volumes {
		v.UID = nil
		s.Data.Volumes[k] = v
	}
}

// A rollback to a release without the volume helper records none: the newer release's reference
// does not outlive it.
func TestARollbackDropsAHelperItsReleaseLacks(t *testing.T) {
	e := newEnv(t)
	e.r, e.self = testInstallerWith(t, "1.0.0", noUID)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.readEnv().Get(ChownImageVar); ok {
		t.Fatal("a release without the helper records it")
	}
	e.to("2.0.0", nil)
	if err := e.upgrade(nil); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, e.log)
	}
	if _, ok := e.readEnv().Get(ChownImageVar); !ok {
		t.Fatal("the release with the helper does not record it")
	}
	if err := e.rollback(nil); err != nil {
		t.Fatalf("rollback: %v\n%s", err, e.log)
	}
	if v, ok := e.readEnv().Get(ChownImageVar); ok {
		t.Errorf("the rollback kept the newer release's helper: %s", v)
	}
}

// A volume awaiting its uid waits while no service mounts it, and is given it when one mounts it
// again; one a service mounts while no uid is owed to it is struck off, its data its own.
func TestAPendingVolumeWaitsUntilMountedAgain(t *testing.T) {
	e := newEnv(t)
	mounted := `{"services": {"web": {"volumes": [{"type": "volume", "source": "uploads", "target": "/u"}]}}, "volumes": {"uploads": {"name": "p_uploads"}}}`
	e.docker.model = mounted
	e.docker.owner = func(Cmd) ([]byte, error) { return nil, errors.New("cut off") }
	if err := e.install(nil); err == nil {
		t.Fatal("the install went on without the volume's uid")
	}
	pending := func() string { raw, _ := os.ReadFile(filepath.Join(e.dir, VolumesPending)); return string(raw) }
	e.docker.model = `{"services": {"web": {}}, "volumes": {"uploads": {"name": "p_uploads"}}}` // its service off
	e.docker.volumes = "p_uploads\n"
	e.docker.owner = nil
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if pending() != "p_uploads\n" {
		t.Fatalf("%s while no service mounts it: %q", VolumesPending, pending())
	}
	e.docker.model, e.docker.calls = mounted, nil
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if n := len(e.docker.ran("run --rm --pull never --network none --user 0 ")); n != 1 || pending() != "" {
		t.Errorf("mounted again: %d chowns, %s %q", n, VolumesPending, pending())
	}

	// Mounted where no uid is owed (a volume the spec gives none), it is struck off untouched.
	if err := os.WriteFile(filepath.Join(e.dir, VolumesPending), []byte("p_cache\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.docker.model = `{"services": {"web": {"volumes": [{"type": "volume", "source": "cache", "target": "/c"}]}}, "volumes": {"cache": {"name": "p_cache"}}}`
	e.docker.volumes, e.docker.calls = "p_cache\n", nil
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if n := len(e.docker.ran("run --rm --pull never --network none --user 0 ")); n != 0 || pending() != "" {
		t.Errorf("a volume owed nothing: %d chowns, %s %q", n, VolumesPending, pending())
	}
}

// frontless is examples/minimal without its front, served by an L4 route alone.
func frontless(s *spec.Spec) {
	s.Front = spec.Front{}
	s.L4.Routes = []spec.L4Route{{Name: "web", Protocol: "tcp", Port: 9000, Upstream: "web:8080"}}
}

// A project without a front installs with nothing asked or probed for one: no TLS mode, no front
// port, no route URL; its L4 route is published; the flags only a front takes are refused.
func TestAProjectWithoutAFront(t *testing.T) {
	e := newEnv(t)
	e.r, e.self = testInstallerWith(t, "1.0.0", frontless)
	e.busy[80], e.busy[443] = true, true // a front's ports, which nothing needs
	if err := e.install(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	f := e.readEnv()
	for _, k := range f.Keys() {
		if k == "STOWAGE_TLS_MODE" || k == HTTPPortVar || k == HTTPSPortVar || strings.HasPrefix(k, "STOWAGE_URL_") || strings.HasPrefix(k, "STOWAGE_PORT_") {
			t.Errorf("recorded %s", k)
		}
	}
	// No front image is recorded nor pulled; the images are still pulled by digest.
	if _, ok := f.Get(FrontImageVar); ok || !pinnedImages(f) || !pinnedImages(NewEnvFile()) {
		t.Errorf("front image recorded %v, pinned %v", ok, pinnedImages(f))
	}
	for _, c := range e.docker.ran("pull") {
		if strings.Contains(strings.Join(c.Args, " "), "notes-front") {
			t.Errorf("pulled the front: %v", c.Args)
		}
	}
	if get(f, "STOWAGE_LOOPBACK_ADDRESS") != "127.0.0.1" || get(f, "SECURE_COOKIES") != "true" {
		t.Errorf("loopback address %q, SECURE_COOKIES %q", get(f, "STOWAGE_LOOPBACK_ADDRESS"), get(f, "SECURE_COOKIES"))
	}
	fc, _ := os.ReadFile(filepath.Join(e.dir, FrontCompose))
	if strings.Contains(string(fc), FrontService) || strings.Contains(string(fc), "stowage-front-data") || !strings.Contains(string(fc), `published: "9000"`) {
		t.Errorf("%s:\n%s", FrontCompose, fc)
	}
	if cf, _ := os.ReadFile(filepath.Join(e.dir, Caddyfile)); !strings.Contains(string(cf), "notes has no front") {
		t.Errorf("%s:\n%s", Caddyfile, cf)
	}
	if !strings.HasPrefix(e.out.String(), "notes 1.0.0 is running\n") {
		t.Errorf("out: %q", e.out)
	}

	// A re-run changes an input; a flag only a front takes is refused, naming it.
	if err := e.install(func(o *Options) { o.Set = map[string]string{"ADMIN_EMAIL": "ops@example.org"} }); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	for name, mod := range map[string]func(*Options){
		"--tls":       func(o *Options) { o.TLS = "acme" },
		"--domains":   func(o *Options) { o.Domains = map[string]string{"web": "a.example.com"} },
		"--http-port": func(o *Options) { o.HTTPPort = 8080 },
		"--ports":     func(o *Options) { o.Ports = map[string]int{"web": 8080} },
	} {
		if err := e.install(mod); err == nil || !strings.Contains(err.Error(), "notes has no front: "+name+" set how a front serves") {
			t.Errorf("re-run with %s: %v", name, err)
		}
		o := e.installer(mod)
		o.O.Dir = filepath.Join(t.TempDir(), "other")
		if err := o.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "notes has no front: "+name) {
			t.Errorf("first install with %s: %v", name, err)
		}
	}

	// vm create refuses a front's mode before it says what a VM's front would need.
	if _, _, err := e.installer(func(o *Options) { o.TLS = Loopback }).vmSettings(context.Background()); err == nil || !strings.Contains(err.Error(), "notes has no front: --tls") {
		t.Errorf("vm create --tls none: %v", err)
	}

	// An upgrade keeps the install's front, or its lack of one.
	withFront, _ := testInstaller(t, "1.0.1")
	if err := (&Installer{R: withFront}).sameFront(f, ""); err == nil || !strings.Contains(err.Error(), "serves through a front, and this install has none") {
		t.Errorf("to a release with a front: %v", err)
	}
	served := NewEnvFile()
	served.Set("STOWAGE_TLS_MODE", "acme")
	if err := (&Installer{R: e.r}).sameFront(served, "acme"); err == nil || !strings.Contains(err.Error(), "has no front, and this install serves through one") {
		t.Errorf("from an install with a front: %v", err)
	}
	if err := (&Installer{R: e.r}).sameFront(f, ""); err != nil {
		t.Errorf("between releases without a front: %v", err)
	}
}
