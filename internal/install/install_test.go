package install

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/front"
	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// fakeDocker answers the docker CLI the installer runs and records every call.
type fakeDocker struct {
	// mu guards calls: a backup's dump and upload run at once.
	mu      sync.Mutex
	calls   []Cmd
	loaded  map[string]string // image reference → the ID `image inspect` reports
	compose string            // `compose version --short`
	pullErr error
	// pullConfig is the Docker config.json the last pull ran with, and its contexts link.
	pullConfig, pullContexts string
	// hookOut is what the provision hook prints on stdout, then fails with hookErr.
	hookOut string
	hookErr error
	hookEnv []string // the provision.env lines the last hook run was given
	// hookSaved is provision.env once the hook printed its output, before it exited.
	hookSaved string
	hookStop  func() // stops the run as the installer would (its context), after the output
	leftover  string // the service whose container is there before the run
	rmErr     error
	// dump is what pg_dump writes, then fails with dumpErr.
	dump    string
	dumpErr error
	psql    func(q string) (string, error)
	// restored is what pg_restore read, which then fails with restoreErr.
	restored   string
	restoreErr error
	// schema answers the schema-compat command run in image.
	schema func(image string, req map[string]any) (string, error)
	// health answers the health command.
	health func(req map[string]any) (string, error)
	labels map[string]string // image reference → its labels, JSON
	upErr  error             // `compose up` fails
	onUp   func() error      // when set, runs at each `compose up` and gives its error
	// schemaFiles are the schema command's compose files, one per run.
	schemaFiles []string
	checked     func() // runs when the installer checks Compose's version
	inspected   func() // runs when the installer looks for a leftover container
	// answer, when it answers (ok), answers a call before anything else.
	answer func(args string) (out []byte, ok bool, err error)
	// runner answers `docker run` of the backup runner.
	runner func(c Cmd) ([]byte, error)
}

// exitCode is a command's exit status, as the docker CLI reports a container's.
type exitCode int

func (e exitCode) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitCode) ExitCode() int { return int(e) }

func (f *fakeDocker) Run(_ context.Context, c Cmd) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	a := strings.Join(c.Args, " ")
	if f.runner != nil && strings.HasPrefix(a, "run --rm --pull never ") {
		return f.runner(c)
	}
	if f.answer != nil {
		if out, ok, err := f.answer(a); ok {
			return out, err
		}
	}
	switch {
	case strings.HasPrefix(a, "version"):
		return []byte("27.3.1\n"), nil
	case a == "compose version --short":
		if f.checked != nil {
			f.checked()
		}
		return []byte(f.compose + "\n"), nil
	case strings.HasPrefix(a, "container inspect"):
		if f.inspected != nil {
			f.inspected()
		}
		if f.leftover != "" && strings.HasSuffix(a, "-"+f.leftover) {
			return []byte("running\n"), nil
		}
		return nil, errors.New("no such container")
	case strings.HasPrefix(a, "rm --force"):
		return nil, f.rmErr
	case strings.HasPrefix(a, "compose") && strings.Contains(a, " up --detach "):
		if f.onUp != nil {
			return nil, f.onUp()
		}
		return nil, f.upErr
	case strings.HasPrefix(a, "image inspect --format {{json .Config.Labels}}"):
		if l, ok := f.labels[c.Args[len(c.Args)-1]]; ok {
			return []byte(l + "\n"), nil
		}
		return nil, errors.New("no such image")
	case strings.Contains(a, " exec -T postgres pg_dump "):
		if _, err := io.WriteString(c.Stdout, f.dump); err != nil {
			// The CLI reports its exit, not the write that failed.
			return nil, errors.New("signal: broken pipe")
		}
		return nil, f.dumpErr
	case strings.Contains(a, " exec -T postgres sh -c ") && strings.Contains(a, "pg_restore"):
		b, err := io.ReadAll(c.Stdin)
		f.mu.Lock()
		f.restored = string(b)
		f.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, f.restoreErr
	case strings.Contains(a, " exec -T postgres psql "):
		if f.psql == nil {
			return nil, errors.New("psql: no database")
		}
		out, err := f.psql(c.Args[len(c.Args)-1])
		return []byte(out + "\n"), err
	case strings.Contains(a, " run --rm --no-deps -T --name ") && strings.HasSuffix(a, " "+HealthService):
		if f.health == nil {
			return nil, exitCode(1)
		}
		var req map[string]any
		body, _ := io.ReadAll(c.Stdin)
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		out, err := f.health(req)
		_, _ = io.WriteString(c.Stdout, out)
		return nil, err
	case strings.Contains(a, " run --rm --no-deps -T --name ") && strings.HasSuffix(a, " "+SchemaService):
		if f.schema == nil {
			return nil, exitCode(1)
		}
		raw, err := os.ReadFile(filepath.Join(c.Args[2], SchemaCompose))
		if err != nil {
			return nil, err
		}
		f.schemaFiles = append(f.schemaFiles, string(raw))
		image := regexp.MustCompile(`image: (\S+)`).FindStringSubmatch(string(raw))[1]
		var req map[string]any
		body, _ := io.ReadAll(c.Stdin)
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		out, err := f.schema(image, req)
		_, _ = io.WriteString(c.Stdout, out)
		return nil, err
	case strings.HasPrefix(a, "image inspect"):
		if id, ok := f.loaded[c.Args[len(c.Args)-1]]; ok {
			return []byte(id + "\n"), nil
		}
		return nil, errors.New("no such image")
	case strings.HasPrefix(a, "pull"):
		if f.pullErr != nil {
			return nil, f.pullErr
		}
		if d := dockerConfig(c.Env); d != "" {
			raw, _ := os.ReadFile(filepath.Join(d, "config.json"))
			f.pullConfig = string(raw)
			f.pullContexts, _ = os.Readlink(filepath.Join(d, "contexts"))
		}
		f.loaded[c.Args[len(c.Args)-1]] = "sha256:pulled"
		return nil, nil
	case strings.Contains(a, " run --rm --no-deps -T --name ") && strings.HasSuffix(a, " "+ProvisionService):
		raw, err := os.ReadFile(filepath.Join(c.Args[2], ProvisionEnv))
		if err != nil {
			return nil, err
		}
		f.hookEnv = strings.Fields(string(raw))
		if _, err := io.WriteString(c.Stdout, f.hookOut); err != nil {
			return nil, err
		}
		if f.hookStop != nil {
			f.hookStop()
			return nil, errors.New("signal: killed")
		}
		saved, _ := os.ReadFile(filepath.Join(c.Args[2], ProvisionEnv))
		f.hookSaved = string(saved)
		return nil, f.hookErr
	}
	return nil, nil
}

func (f *fakeDocker) ran(prefix string) []Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Cmd
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c.Args, " "), prefix) {
			out = append(out, c)
		}
	}
	return out
}

const (
	digest   = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	config   = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	manifest = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
)

// testRelease builds the payload of examples/minimal at version and loads it back.
func testRelease(t *testing.T, version string) *Release {
	t.Helper()
	r, _ := testInstaller(t, version)
	return r
}

// testInstaller builds an installer of examples/minimal at version: its release, and the file
// carrying it.
func testInstaller(t *testing.T, version string) (*Release, string) {
	t.Helper()
	return testInstallerWith(t, version, nil)
}

// testInstallerWith is testInstaller with the spec mod changes, in the payload too.
func testInstallerWith(t *testing.T, version string, mod func(*spec.Spec)) (*Release, string) {
	t.Helper()
	return testInstallerAt(t, version, 1, mod)
}

// testInstallerAt is testInstallerWith at a revision.
func testInstallerAt(t *testing.T, version string, revision int, mod func(*spec.Spec)) (*Release, string) {
	t.Helper()
	return testInstallerOf(t, version, revision, mod, nil)
}

// testInstallerOf is testInstallerAt with the release manifest mm changes.
func testInstallerOf(t *testing.T, version string, revision int, mod func(*spec.Spec), mm func(*release.Manifest)) (*Release, string) {
	t.Helper()
	return testInstallerKeys(t, version, revision, mod, mm, nil)
}

// testInstallerKeys is testInstallerOf with extra payload files (trusted keys).
func testInstallerKeys(t *testing.T, version string, revision int, mod func(*spec.Spec), mm func(*release.Manifest), extra payload.Files) (*Release, string) {
	t.Helper()
	root := testutil.CopyExample(t, "minimal")
	p, err := project.Open(root, "")
	if err != nil {
		t.Fatal(err)
	}
	view := p.Spec.RuntimeView()
	if mod != nil {
		mod(&view)
	}
	specJSON, _ := json.Marshal(view)
	images, configs, manifests := map[string]string{}, map[string]string{}, map[string]string{}
	for _, k := range append(p.Spec.ImageNames(), front.Key) {
		images[k], configs[k], manifests[k] = digest, config, manifest
	}
	m := &release.Manifest{SchemaVersion: release.SchemaVersion, Name: "notes", ShortName: view.Short(), ProjectVersion: version, Revision: revision, ReleaseSeq: 1, Channel: "stable", Tag: version,
		Date: time.Now(), Inputs: "sha256:in", Scheme: release.Scheme{Provider: "semver"}, Artifacts: release.Artifacts{Images: images, Configs: configs, Manifests: manifests}}
	if mm != nil {
		mm(m)
	}
	man, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	compose, _ := os.ReadFile(filepath.Join(root, "deploy/compose.yaml"))
	var buf bytes.Buffer
	files := payload.Files{payload.Release: man, payload.Spec: specJSON, payload.Compose: compose}
	maps.Copy(files, extra)
	if err := payload.Write(&buf, strings.NewReader("exe"), files); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "notes-stow")
	_ = os.WriteFile(exe, buf.Bytes(), 0o755)
	files, err = payload.Read(exe)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Load(files)
	if err != nil {
		t.Fatal(err)
	}
	return r, exe
}

type env struct {
	t      *testing.T
	r      *Release
	self   string // the installer carrying r
	docker *fakeDocker
	dir    string
	cos    string // where Kairos's marks are read (Options.Cos)
	out    *bytes.Buffer
	log    *bytes.Buffer
	vars   map[string]string
	busy   map[int]bool
}

func newEnv(t *testing.T) *env {
	r, self := testInstaller(t, "1.0.0")
	return &env{t: t, r: r, self: self, docker: &fakeDocker{loaded: map[string]string{}, compose: "2.29.1"},
		dir: filepath.Join(t.TempDir(), "notes"), cos: t.TempDir(), out: &bytes.Buffer{}, log: &bytes.Buffer{},
		vars: map[string]string{"ADMIN_EMAIL": "admin@example.org"}, busy: map[int]bool{}}
}

func (e *env) installer(mod func(*Options)) *Installer {
	o := Options{Dir: e.dir, Cos: e.cos, Out: e.out, Log: e.log, Docker: e.docker, Self: e.self,
		Getenv:   func(k string) string { return e.vars[k] },
		Environ:  []string{"PATH=/bin", "SESSION_SECRET=from-the-shell", "COMPOSE_PROJECT_NAME=other"},
		PortFree: func(_ string, p int) bool { return !e.busy[p] }}
	if mod != nil {
		mod(&o)
	}
	return &Installer{R: e.r, O: o}
}

func (e *env) install(mod func(*Options)) error {
	return e.installer(mod).Install(context.Background())
}

func (e *env) readEnv() *EnvFile {
	e.t.Helper()
	f, err := ReadEnvFile(filepath.Join(e.dir, ".env"))
	if err != nil {
		e.t.Fatal(err)
	}
	return f
}

func (e *env) nothingWritten() {
	e.t.Helper()
	if _, err := os.Stat(e.dir); !errors.Is(err, os.ErrNotExist) {
		e.t.Fatalf("%s was written", e.dir)
	}
}

func get(f *EnvFile, k string) string { v, _ := f.Get(k); return v }

func TestInstallWritesTheStackAndStartsIt(t *testing.T) {
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	fi, err := os.Stat(filepath.Join(e.dir, ".env"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf(".env: %v %v", err, fi.Mode())
	}
	f := e.readEnv()
	for k, want := range map[string]string{
		"STOWAGE_NAME": "notes", "STOWAGE_SHORT_NAME": "notes", "STOWAGE_PROJECT_VERSION": "1.0.0", "STOWAGE_REVISION": "1",
		"STOWAGE_REGISTRY": "ghcr.io/acme/notes", "STOWAGE_LOOPBACK_ADDRESS": "127.0.0.1",
		"STOWAGE_PORT_APP": "8080", "STOWAGE_URL_APP": "http://localhost:8080", "STOWAGE_URL_API": "http://localhost:8000",
		"STOWAGE_IMAGE_API": "ghcr.io/acme/notes/notes-api@" + digest,
		"SECURE_COOKIES":    "false", "ADMIN_EMAIL": "admin@example.org",
	} {
		if got := get(f, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(get(f, "SESSION_SECRET")) {
		t.Errorf("SESSION_SECRET = %q", get(f, "SESSION_SECRET"))
	}
	if !regexp.MustCompile(`^[A-Za-z0-9]{18}$`).MatchString(get(f, "POSTGRES_PASSWORD")) {
		t.Errorf("POSTGRES_PASSWORD = %q", get(f, "POSTGRES_PASSWORD"))
	}
	if !regexp.MustCompile(`^notes-notes-[0-9a-f]{8}$`).MatchString(get(f, "COMPOSE_PROJECT_NAME")) {
		t.Errorf("project = %q", get(f, "COMPOSE_PROJECT_NAME"))
	}
	for _, n := range []string{"compose.yaml", FrontCompose, Caddyfile} {
		if _, err := os.Stat(filepath.Join(e.dir, n)); err != nil {
			t.Error(err)
		}
	}
	if n := len(e.docker.ran("pull")); n != 4 {
		t.Errorf("%d pulls, want 4 (3 images + the front)", n)
	}
	up := e.docker.ran("compose")
	if len(up) != 3 || !strings.Contains(strings.Join(up[1].Args, " "), "up --detach --wait") || !strings.HasSuffix(strings.Join(up[2].Args, " "), "run --rm --no-deps -T --name "+get(e.readEnv(), "COMPOSE_PROJECT_NAME")+"-"+ProvisionService+" "+ProvisionService) {
		t.Fatalf("compose calls: %v", up)
	}
	// The recorded values win over the shell's.
	envs := strings.Join(up[1].Env, "\n")
	if strings.Contains(envs, "from-the-shell") || strings.Contains(envs, "COMPOSE_PROJECT_NAME=other") || !strings.Contains(envs, "PATH=/bin") {
		t.Fatalf("compose environment:\n%s", envs)
	}
	if !strings.Contains(e.out.String(), "notes 1.0.0 is running: http://localhost:8080") {
		t.Fatalf("output: %s", e.out)
	}
}

func TestNothingIsWrittenBeforeEveryCheckPassed(t *testing.T) {
	for name, tc := range map[string]struct {
		mod  func(*env, *Options)
		want string
	}{
		"required input":   {func(e *env, _ *Options) { delete(e.vars, "ADMIN_EMAIL") }, "ADMIN_EMAIL (Administrator email)"},
		"busy port":        {func(e *env, _ *Options) { e.busy[8000] = true }, "port(s) 8000 (api) already in use"},
		"network address":  {func(_ *env, o *Options) { o.Address = "0.0.0.0" }, "not a loopback address"},
		"ipv6 address":     {func(_ *env, o *Options) { o.Address = "2001:db8::1" }, "not a loopback address"},
		"ipv6 unspecified": {func(_ *env, o *Options) { o.Address = "::" }, "not a loopback address"},
		"ipv6 zone":        {func(_ *env, o *Options) { o.Address = "::1%lo" }, "not a loopback address"},
		"duplicate ports":  {func(_ *env, o *Options) { o.Ports = map[string]int{"api": 8080} }, "both use 8080"},
		"unknown route":    {func(_ *env, o *Options) { o.Ports = map[string]int{"admin": 9000} }, `"admin" is no route`},
		"old compose":      {func(e *env, _ *Options) { e.docker.compose = "2.19.9" }, "too old"},
		"offline, missing": {func(_ *env, o *Options) { o.Offline = true }, "ghcr.io/acme/notes/notes-web:1.0.0"},
		"pull fails":       {func(e *env, _ *Options) { e.docker.pullErr = errors.New("denied") }, "denied"},
		"unknown input":    {func(_ *env, o *Options) { o.Set = map[string]string{"ADMIN_EMAL": "x"} }, "no input of this install is named ADMIN_EMAL"},
		"quote in a value": {func(e *env, _ *Options) { e.vars["ADMIN_EMAIL"] = "o'brien@x" }, "ADMIN_EMAIL: a value cannot hold a single quote"},
		"foreign compose": {func(e *env, _ *Options) {
			_ = os.MkdirAll(e.dir, 0o755)
			_ = os.WriteFile(filepath.Join(e.dir, "compose.yaml"), nil, 0o644)
		}, "already holds a compose.yaml"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			err := e.install(func(o *Options) { tc.mod(e, o) })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v", err)
			}
			if name == "foreign compose" {
				if _, err := os.Stat(filepath.Join(e.dir, ".env")); err == nil {
					t.Fatal(".env written")
				}
				return
			}
			e.nothingWritten()
			if len(e.docker.ran("pull")) > 0 && name != "pull fails" {
				t.Fatal("pulled before every check passed")
			}
		})
	}
}

func TestOfflineRunsTheLoadedImagesByTagAndNeverPulls(t *testing.T) {
	e := newEnv(t)
	// docker load on the classic image store keeps tags, not registry digests: the ID is the config
	// digest there, the image digest on the containerd store.
	e.docker.loaded["ghcr.io/acme/notes/notes-front:1.0.0"] = manifest
	e.docker.loaded["ghcr.io/acme/notes/notes-api:1.0.0"] = config
	e.docker.loaded["ghcr.io/acme/notes/notes-web:1.0.0"] = digest
	e.docker.loaded["ghcr.io/acme/notes/notes-postgres:1.0.0"] = "sha256:3333"
	err := e.install(func(o *Options) { o.Offline = true })
	if err == nil || !strings.Contains(err.Error(), "notes-postgres:1.0.0 is sha256:3333, not the release's image") || strings.Contains(err.Error(), "notes-api") {
		t.Fatalf("a loaded image with the right tag and other content: %v", err)
	}
	e.nothingWritten()
	e.docker.loaded["ghcr.io/acme/notes/notes-postgres:1.0.0"] = manifest // a single-platform archive on the containerd store
	if err := e.install(func(o *Options) { o.Offline = true }); err != nil {
		t.Fatal(err)
	}
	if n := len(e.docker.ran("pull")) + len(e.docker.ran("login")); n != 0 {
		t.Fatalf("%d pulls or logins offline", n)
	}
	f := e.readEnv()
	if get(f, "STOWAGE_IMAGE_API") != "ghcr.io/acme/notes/notes-api:1.0.0" || get(f, FrontImageVar) != "ghcr.io/acme/notes/notes-front:1.0.0" {
		t.Fatalf("%s %s", get(f, "STOWAGE_IMAGE_API"), get(f, FrontImageVar))
	}
}

func TestPortsAddressAndMirror(t *testing.T) {
	e := newEnv(t)
	err := e.install(func(o *Options) {
		o.Ports = map[string]int{"app": 18080}
		o.Address = "127.0.0.9"
		o.Registry = "mirror.example.org/notes/"
	})
	if err != nil {
		t.Fatal(err)
	}
	f := e.readEnv()
	for k, want := range map[string]string{
		"STOWAGE_URL_APP": "http://127.0.0.9:18080", "STOWAGE_URL_API": "http://127.0.0.9:8000",
		"STOWAGE_LOOPBACK_ADDRESS": "127.0.0.9", "STOWAGE_REGISTRY": "mirror.example.org/notes",
		"STOWAGE_IMAGE_WEB": "mirror.example.org/notes/notes-web@" + digest,
		FrontImageVar:       "mirror.example.org/notes/notes-front@" + digest,
	} {
		if got := get(f, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestTheIPv6Loopback(t *testing.T) {
	e := newEnv(t)
	if err := e.install(func(o *Options) { o.Ports, o.Address = map[string]int{"app": 18080}, "::1" }); err != nil {
		t.Fatal(err)
	}
	f := e.readEnv()
	// Bare in the variable (compose's port syntax takes "::1:18080:80"), bracketed in a URL.
	for k, want := range map[string]string{"STOWAGE_LOOPBACK_ADDRESS": "::1", "STOWAGE_URL_APP": "http://[::1]:18080", "STOWAGE_URL_API": "http://[::1]:8000"} {
		if got := get(f, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if got, _ := loopbackAddr("::ffff:127.0.0.9"); got != "127.0.0.9" {
		t.Errorf("an IPv4-mapped loopback address kept as %q", got)
	}
}

func TestLocalhostsIPv6AddressIsProbedToo(t *testing.T) {
	// localhost resolves to ::1 first on an IPv6 host: a port held there is held.
	e := newEnv(t)
	held := func(o *Options) { o.PortFree = func(h string, p int) bool { return h != "::1" || p != 8000 } }
	if err := e.install(held); err == nil || !strings.Contains(err.Error(), "port(s) 8000 (api) already in use") {
		t.Fatalf("err = %v", err)
	}
	// Another loopback address is not localhost: only it is probed.
	e = newEnv(t)
	if err := e.install(func(o *Options) { held(o); o.Address = "127.0.0.9" }); err != nil {
		t.Fatal(err)
	}
}

func TestIPv6OnTheStacksNetwork(t *testing.T) {
	on, off := true, false
	// Off, recorded: the compose file may read it.
	e := newEnv(t)
	if err := e.install(nil); err != nil || get(e.readEnv(), IPv6Var) != "false" {
		t.Fatalf("%v %q", err, get(e.readEnv(), IPv6Var))
	}
	e = newEnv(t)
	stopped := func() bool {
		return slices.ContainsFunc(e.docker.calls, func(c Cmd) bool { return strings.HasSuffix(strings.Join(c.Args, " "), " down --remove-orphans") })
	}
	probed := func() bool {
		return slices.ContainsFunc(e.docker.calls, func(c Cmd) bool {
			return strings.HasPrefix(strings.Join(c.Args, " "), "network create --ipv6 --label io.stowage.probe=true stowage-ipv6-check-")
		})
	}
	// The network's label: the choice it was made with ("" before the label).
	made := ""
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if strings.HasPrefix(a, "network ls --filter label=com.docker.compose.project=") && strings.HasSuffix(a, `--format {{.ID}} {{.Label "io.stowage.ipv6"}}`) {
			return []byte("n1 " + made + "\n"), true, nil
		}
		return nil, false, nil
	}
	for _, c := range []struct {
		ipv6     *bool
		made     string
		recorded string
		network  bool // the front's compose file turns IPv6 on
		stops    bool // the network was made the other way: the stack stops first
	}{
		{nil, "", "false", false, false}, // a network made before the label: off
		{&on, "", "true", true, true},
		{nil, "true", "true", true, false}, // kept
		{&off, "true", "false", false, true},
		{nil, "false", "false", false, false},
	} {
		e.docker.calls, made = nil, c.made
		if err := e.install(func(o *Options) { o.IPv6 = c.ipv6 }); err != nil {
			t.Fatal(err)
		}
		fc, _ := os.ReadFile(filepath.Join(e.dir, FrontCompose))
		if got := get(e.readEnv(), IPv6Var); got != c.recorded || strings.Contains(string(fc), "enable_ipv6: true") != c.network ||
			strings.Contains(string(fc), "io.stowage.ipv6: \"true\"") != c.network || strings.Contains(string(fc), "networks:") != c.network || stopped() != c.stops || probed() != (c.ipv6 == &on) {
			t.Errorf("--ipv6 %v, network %q: recorded %q, compose file:\n%s\nstopped %v, probed %v", c.ipv6, c.made, got, fc, stopped(), probed())
		}
	}
	// A dry run makes no network.
	e.docker.calls = nil
	if err := e.install(func(o *Options) { o.IPv6, o.DryRun = &on, true }); err != nil || probed() {
		t.Errorf("dry run: %v, probed %v", err, probed())
	}
	// An engine that cannot make an IPv6 network: refused before anything is written.
	inner := e.docker.answer
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if strings.HasPrefix(a, "network create --ipv6 ") {
			return nil, true, errors.New("could not find an available, non-overlapping IPv6 address pool")
		}
		return inner(a)
	}
	e.docker.calls = nil
	if err := e.install(func(o *Options) { o.IPv6 = &on }); err == nil || !strings.Contains(err.Error(), "cannot make an IPv6 network here") || stopped() || get(e.readEnv(), IPv6Var) != "false" {
		t.Errorf("err = %v, stopped %v, recorded %q", err, stopped(), get(e.readEnv(), IPv6Var))
	}
	// An engine older than 27 does not give a network IPv6 by itself.
	e = newEnv(t)
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if a == "version --format {{.Server.Version}}" {
			return []byte("26.1.4\n"), true, nil
		}
		return nil, false, nil
	}
	if err := e.install(func(o *Options) { o.IPv6 = &on }); err == nil || !strings.Contains(err.Error(), "--ipv6: the Docker engine 26.1.4 is too old: 27.0 or later") {
		t.Fatalf("err = %v", err)
	}
	if err := e.install(nil); err != nil {
		t.Fatalf("without --ipv6, Docker 26: %v", err)
	}
}

func TestAnUpgradeRecordsIPv6Off(t *testing.T) {
	t.Parallel()
	in := &Installer{R: testRelease(t, "2.0.0")}
	for was, want := range map[string]string{"": "false", "true": "true", "false": "false"} {
		env := NewEnvFile()
		env.Set("STOWAGE_REGISTRY", "ghcr.io/acme/notes")
		if was != "" {
			env.Set(IPv6Var, was)
		}
		next, _, err := in.upgradeEnv(env, Loopback, nil, nil)
		if got, _ := next.Get(IPv6Var); err != nil || got != want {
			t.Errorf("%q: %q %v", was, got, err)
		}
	}
}

func TestAReRunFinishesSetupAndNeverRedecides(t *testing.T) {
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(e.dir, ".env"))
	for _, mod := range []func(*Options){
		func(o *Options) { o.Address = "127.0.0.9" },
		func(o *Options) { o.Registry = "mirror.example.org/notes" },
		func(o *Options) { o.Channel = "beta" },
	} {
		if err := e.install(mod); err == nil || !strings.Contains(err.Error(), "recorded at the first install") {
			t.Fatalf("a first-install choice on a re-run: %v", err)
		}
	}
	if err := e.install(func(o *Options) { o.TLS = "internal" }); err == nil || !strings.Contains(err.Error(), "moving between loopback mode and a TLS mode is a new install") {
		t.Fatalf("a loopback install moved to a TLS mode: %v", err)
	}
	e.docker.calls = nil
	e.vars["ADMIN_EMAIL"] = "someone@else.org" // the environment never changes a recorded value
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(e.dir, ".env"))
	if !bytes.Equal(before, after) {
		t.Fatal("a re-run rewrote .env")
	}
	if len(e.docker.ran("pull")) != 0 || len(e.docker.ran("compose")) != 3 {
		t.Fatalf("re-run calls: %v", e.docker.calls)
	}

	e.r = testRelease(t, "1.1.0")
	if err := e.install(nil); err == nil || !strings.Contains(err.Error(), e.self+" upgrade --dir") {
		t.Fatalf("another release: %v", err)
	}
}

// An install keeps its installer under the short name it was made with, so a release under
// another short name is refused rather than taking the install over; the same release re-runs.
func TestAnInstallOfAnotherShortNameIsRefused(t *testing.T) {
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if err := e.install(nil); err != nil {
		t.Fatalf("a re-run of the same release: %v", err)
	}
	envFile := filepath.Join(e.dir, ".env")
	raw, _ := os.ReadFile(envFile)
	renamed := regexp.MustCompile(`(?m)^STOWAGE_SHORT_NAME=.*$`).ReplaceAll(raw, []byte("STOWAGE_SHORT_NAME='nt'"))
	if bytes.Equal(raw, renamed) {
		t.Fatal(".env records no STOWAGE_SHORT_NAME")
	}
	if err := os.WriteFile(envFile, renamed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.install(nil); err == nil || !strings.Contains(err.Error(), "install this release afresh") {
		t.Fatalf("a re-run over an install of another short name: %v", err)
	}
}

func TestAReRunChangesWhatItIsGiven(t *testing.T) {
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(e.dir, ".env"))
	unchanged := func(what string) {
		t.Helper()
		if after, _ := os.ReadFile(filepath.Join(e.dir, ".env")); !bytes.Equal(before, after) {
			t.Fatalf("%s changed .env", what)
		}
	}
	for name, c := range map[string]struct {
		mod  func(*Options)
		want string
	}{
		"unknown":   {func(o *Options) { o.Set = map[string]string{"ADMIN_MAIL": "x@y.org"} }, "no input of this install is named ADMIN_MAIL"},
		"required":  {func(o *Options) { o.Answers = map[string]string{"ADMIN_EMAIL": ""} }, "ADMIN_EMAIL is required"},
		"bad value": {func(o *Options) { o.Set = map[string]string{"ADMIN_EMAIL": "a\nb"} }, "ADMIN_EMAIL:"},
		"no route":  {func(o *Options) { o.Ports = map[string]int{"web": 9000} }, `--ports: "web" is no route`},
		"same port": {func(o *Options) { o.Ports = map[string]int{"app": 8000} }, "--ports: app and api both use 8000"},
		"TLS ports": {func(o *Options) { o.HTTPSPort = 8443 }, "this install is in loopback mode"},
		"domains":   {tlsDomains, "this install is in loopback mode, which serves no domain"},
		"busy port": {func(o *Options) { o.Ports = map[string]int{"app": 18081} }, "port(s) 18081 (app) already in use"},
		"dry run":   {func(o *Options) { o.DryRun, o.Ports = true, map[string]int{"app": 18080} }, ""},
	} {
		e.busy[18081] = true
		e.log.Reset()
		err := e.install(c.mod)
		if c.want == "" && err == nil && strings.Contains(e.log.String(), "changing STOWAGE_PORT_APP: 8080 → 18080") && strings.Contains(e.log.String(), "a run would record these changes") {
			unchanged(name)
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) || c.want == "" {
			t.Fatalf("%s: %v\n%s", name, err, e.log)
		}
		unchanged(name)
	}

	// The install's own ports are its stack's: only a port it does not hold is probed.
	e.busy[8000], e.busy[8080] = true, true
	e.log.Reset()
	e.docker.calls = nil
	if err := e.install(func(o *Options) {
		o.Set = map[string]string{"ADMIN_EMAIL": "ops@example.org"}
		o.Ports = map[string]int{"app": 18080}
	}); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	f := e.readEnv()
	for k, want := range map[string]string{
		"ADMIN_EMAIL": "ops@example.org", "STOWAGE_PORT_APP": "18080", "STOWAGE_URL_APP": "http://localhost:18080",
		"STOWAGE_PORT_API": "8000", "STOWAGE_URL_API": "http://localhost:8000",
	} {
		if got := get(f, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if prev, _ := os.ReadFile(filepath.Join(e.dir, ".env.prev")); !bytes.Equal(prev, before) {
		t.Errorf(".env.prev is not the .env before the change:\n%s", prev)
	}
	if got, was := get(f, "SESSION_SECRET"), get(mustRead(t, before), "SESSION_SECRET"); got != was {
		t.Errorf("a re-run generated SESSION_SECRET again")
	}
	for _, want := range []string{"changing ADMIN_EMAIL: admin@example.org → ops@example.org", "changing STOWAGE_URL_APP: http://localhost:8080 → http://localhost:18080"} {
		if !strings.Contains(e.log.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, e.log)
		}
	}
	if len(e.docker.ran("compose")) != 3 || !strings.Contains(e.out.String(), "http://localhost:18080") {
		t.Fatalf("the stack was not brought up again: %v\n%s", e.docker.calls, e.out)
	}

	// Two routes swapping the ports the install holds probe neither.
	if err := e.install(func(o *Options) { o.Ports = map[string]int{"app": 8000, "api": 18080} }); err != nil {
		t.Fatalf("swap: %v", err)
	}

	// A change the stack does not come up with stays recorded; .env.prev keeps the values it last
	// came up with, through a second try too.
	cameUp, _ := os.ReadFile(filepath.Join(e.dir, ".env"))
	e.docker.upErr = errors.New("unhealthy")
	for _, email := range []string{"x@example.org", "y@example.org"} {
		if err := e.install(func(o *Options) { o.Set = map[string]string{"ADMIN_EMAIL": email} }); err == nil || !strings.Contains(err.Error(), filepath.Join(e.dir, ".env.prev")+" holds the values the stack last came up with") {
			t.Fatalf("a change the stack does not come up with: %v", err)
		}
		if get(e.readEnv(), "ADMIN_EMAIL") != email {
			t.Fatal("the change was not recorded")
		}
		if prev, _ := os.ReadFile(filepath.Join(e.dir, ".env.prev")); !bytes.Equal(prev, cameUp) {
			t.Fatalf(".env.prev after a failed change %s:\n%s", email, prev)
		}
	}
	if err := e.install(nil); err == nil || strings.Contains(err.Error(), "the changes are recorded") {
		t.Fatalf("a re-run with no change: %v", err)
	}
	// Once the stack comes up, the next change keeps the values it came up with.
	e.docker.upErr = nil
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	upNow, _ := os.ReadFile(filepath.Join(e.dir, ".env"))
	if err := e.install(func(o *Options) { o.Set = map[string]string{"ADMIN_EMAIL": "z@example.org"} }); err != nil {
		t.Fatal(err)
	}
	if prev, _ := os.ReadFile(filepath.Join(e.dir, ".env.prev")); !bytes.Equal(prev, upNow) {
		t.Fatalf(".env.prev after the stack came up:\n%s", prev)
	}
	if _, err := os.Stat(filepath.Join(e.dir, ".stowage-changing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the change marker is left: %v", err)
	}
	// A typo is no domain: no hint about one.
	if err := e.install(func(o *Options) { o.Set = map[string]string{"ADMIN_MAIL": "x"} }); err == nil || strings.Contains(err.Error(), "domain") {
		t.Fatalf("a typo: %v", err)
	}
}

func TestALoopbackReRunTakesNoDomain(t *testing.T) {
	e := newEnv(t)
	e.r.Spec.Inputs = append(e.r.Spec.Inputs, spec.Input{Key: "APP_DOMAIN"}) // the project's own input
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	for _, mod := range []func(*Options){
		func(o *Options) { o.Domains = map[string]string{"app": "app.example.com"} },
		func(o *Options) { o.Answers = map[string]string{"APP_DOMAIN": "app.example.com"} },
	} {
		if err := e.install(mod); err == nil || !strings.Contains(err.Error(), "this install is in loopback mode, which serves no domain") {
			t.Fatalf("a domain on a loopback re-run: %v", err)
		}
	}
}

func mustRead(t *testing.T, raw []byte) *EnvFile {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := ReadEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestChangesLogASecretRedacted(t *testing.T) {
	t.Parallel()
	var log bytes.Buffer
	in := &Installer{O: Options{Log: &log}}
	a, b := NewEnvFile(), NewEnvFile()
	a.Set("TOKEN", "old-secret")
	b.Set("TOKEN", "new-secret")
	a.Set("NAME", "")
	b.Set("NAME", "x")
	in.logChanges(a, b, map[string]bool{"TOKEN": true})
	if got := log.String(); got != "changing TOKEN: <redacted> → <redacted>\nchanging NAME: \"\" → x\n" {
		t.Fatalf("%q", got)
	}
}

func TestDryRunRedactsSecretsAndWritesNothing(t *testing.T) {
	e := newEnv(t)
	if err := e.install(func(o *Options) { o.DryRun = true }); err != nil {
		t.Fatal(err)
	}
	e.nothingWritten()
	out := e.out.String()
	if !strings.Contains(out, "SESSION_SECRET=<redacted>") || !strings.Contains(out, "ADMIN_EMAIL=admin@example.org") {
		t.Fatalf("dry run:\n%s", out)
	}
	if len(e.docker.ran("pull")) != 0 {
		t.Fatal("a dry run pulled")
	}
}

func TestInputPrecedenceAndDefaults(t *testing.T) {
	e := newEnv(t)
	e.r.Spec.Inputs = append(e.r.Spec.Inputs,
		inputWithDefault("OWNER_EMAIL", "${ADMIN_EMAIL}"),
		inputWithDefault("TEAM", "core"))
	err := e.install(func(o *Options) {
		o.Set = map[string]string{"TEAM": "from-set"}
		o.Answers = map[string]string{"TEAM": "from-answers", "ADMIN_EMAIL": "from-answers@x"}
	})
	if err != nil {
		t.Fatal(err)
	}
	f := e.readEnv()
	for k, want := range map[string]string{"ADMIN_EMAIL": "admin@example.org", "OWNER_EMAIL": "admin@example.org", "TEAM": "from-set"} {
		if got := get(f, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestThePullAccountGoesInAPrivateConfigNeverTheOperatorsLogin(t *testing.T) {
	e := newEnv(t)
	e.r.Spec.Registry.Pull = spec.Provider{Name: "token", Config: map[string]any{"user": "pull", "token_env": "NOTES_PULL_TOKEN"}}
	e.vars["NOTES_PULL_TOKEN"] = "s3cret"
	own := t.TempDir()
	_ = os.WriteFile(filepath.Join(own, "config.json"), []byte(`{"currentContext":"rootless","credsStore":"desktop"}`), 0o600)
	_ = os.MkdirAll(filepath.Join(own, "contexts"), 0o700)
	e.vars["DOCKER_CONFIG"] = own
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if n := len(e.docker.ran("login")); n != 0 {
		t.Fatalf("%d docker logins: a login writes to the operator's config or credential store", n)
	}
	want := base64.StdEncoding.EncodeToString([]byte("pull:s3cret"))
	if e.docker.pullConfig != `{"auths":{"ghcr.io":{"auth":"`+want+`"}},"currentContext":"rootless"}` {
		t.Fatalf("pull config: %s", e.docker.pullConfig)
	}
	if e.docker.pullContexts != filepath.Join(own, "contexts") {
		t.Fatalf("contexts link: %q", e.docker.pullContexts)
	}
	for _, c := range e.docker.calls {
		if strings.Contains(strings.Join(c.Args, " "), "s3cret") {
			t.Fatalf("the password on a command line: %v", c.Args)
		}
	}
	// Inside the install directory (a snap-confined docker CLI has a private /tmp), then removed.
	cfg := dockerConfig(e.docker.ran("pull")[0].Env)
	if !strings.HasPrefix(cfg, e.dir+string(filepath.Separator)) {
		t.Fatalf("pull config at %s, outside %s", cfg, e.dir)
	}
	if _, err := os.Stat(cfg); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s was left behind", cfg)
	}
	// `up` pulls an image gone since with the account too.
	ups := 0
	for _, c := range e.docker.calls {
		if slices.Contains(c.Args, "up") {
			ups++
			if up := dockerConfig(c.Env); !strings.HasPrefix(up, e.dir+string(filepath.Separator)) {
				t.Fatalf("up ran with DOCKER_CONFIG %q, not the account's", up)
			}
		}
	}
	if ups != 1 {
		t.Fatalf("%d ups", ups)
	}
	hooks := e.hookCalls()
	if len(hooks) != 1 || !strings.HasPrefix(dockerConfig(hooks[0].Env), e.dir+string(filepath.Separator)) {
		t.Fatalf("the hook did not run with the account's config: %d runs", len(hooks))
	}
	raw, _ := os.ReadFile(filepath.Join(own, "config.json"))
	if string(raw) != `{"currentContext":"rootless","credsStore":"desktop"}` {
		t.Fatalf("the operator's config changed: %s", raw)
	}
}

func TestProjectNamesFollowTheDirectory(t *testing.T) {
	a, b := projectName("notes", "/srv/a/Notes App"), projectName("notes", "/srv/b/Notes App")
	if a == b || !strings.HasPrefix(a, "notes-notes-app-") {
		t.Fatalf("%s %s", a, b)
	}
	if long := projectName("notes", "/x/"+strings.Repeat("y", 80)); len(long) > len("notes-")+32+9 {
		t.Fatalf("%s", long)
	}
}

func TestFrontStripsHeadersAndPublishesOnTheLoopback(t *testing.T) {
	e := newEnv(t)
	e.r.Spec.Front.Routes[1].StripHeaders = []string{"X-Auth-Request-*", "X-Tenant"}
	cf := loopbackCaddyfile(e.r.Spec.Front, func(string) string { return "" })
	for _, want := range []string{"http://:10000 {\n\treverse_proxy web:8080", "http://:10001 {\n\trequest_header -X-Auth-Request-*\n\trequest_header -X-Tenant\n\treverse_proxy api:8000", "admin off"} {
		if !strings.Contains(cf, want) {
			t.Errorf("Caddyfile lacks %q:\n%s", want, cf)
		}
	}
	fc, err := frontCompose(e.r.Spec.Front, Loopback, hostAPI{}, "sha256:x", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fc), "${STOWAGE_LOOPBACK_ADDRESS}:${STOWAGE_PORT_API}:10001") {
		t.Fatalf("front compose:\n%s", fc)
	}
}

func TestEnvFileRefusesALineBreak(t *testing.T) {
	f := NewEnvFile()
	f.Set("A", "x\ny")
	if err := f.WriteFile(filepath.Join(t.TempDir(), ".env")); err == nil {
		t.Fatal("a value with a newline was written")
	}
}

func inputWithDefault(key, def string) spec.Input {
	return spec.Input{Key: key, Prompt: key, Default: def}
}

func TestLocalhostBindsTheIPv4Loopback(t *testing.T) {
	e := newEnv(t)
	if err := e.install(func(o *Options) { o.Address = "localhost" }); err != nil {
		t.Fatal(err)
	}
	f := e.readEnv()
	if get(f, "STOWAGE_LOOPBACK_ADDRESS") != "127.0.0.1" || get(f, "STOWAGE_URL_APP") != "http://localhost:8080" {
		t.Fatalf("%s %s", get(f, "STOWAGE_LOOPBACK_ADDRESS"), get(f, "STOWAGE_URL_APP"))
	}
}

func TestSafeDefaultsAreRecorded(t *testing.T) {
	e := newEnv(t)
	e.r.Spec.SafeDefaults["TRUSTED_PROXY_HOPS"] = "1"
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	f := e.readEnv()
	for k, want := range map[string]string{"TRUSTED_PROXY_HOPS": "1", "SECURE_COOKIES": "false"} {
		if got := get(f, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if _, ok := f.Get("COMPOSE_PROFILES"); ok {
		t.Error("an install records COMPOSE_PROFILES: no input turns a compose profile on")
	}
}

func TestEnvFileKeepsDollarsAndHashes(t *testing.T) {
	f := NewEnvFile()
	f.Set("PASSWORD", "pa$$w0rd #1 ${HOME} \\n")
	p := filepath.Join(t.TempDir(), ".env")
	if err := f.WriteFile(p); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if string(raw) != "PASSWORD='pa$$w0rd #1 ${HOME} \\n'\n" {
		t.Fatalf("%q", raw)
	}
	back, err := ReadEnvFile(p)
	if err != nil || get(back, "PASSWORD") != "pa$$w0rd #1 ${HOME} \\n" {
		t.Fatalf("%v %q", err, get(back, "PASSWORD"))
	}
}

func dockerConfig(env []string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "DOCKER_CONFIG="); ok {
			return v
		}
	}
	return ""
}

func TestDockerHubCredentialsUseTheKeyTheCLIReads(t *testing.T) {
	for _, prefix := range []string{"acme/notes", "docker.io/acme", "index.docker.io/acme"} {
		e := newEnv(t)
		e.r.Spec.Registry.Prefix = prefix
		e.r.Spec.Registry.Pull = spec.Provider{Name: "token", Config: map[string]any{"user": "pull", "token_env": "T"}}
		e.vars["T"] = "x"
		e.vars["DOCKER_CONFIG"] = t.TempDir()
		if err := e.install(nil); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(e.docker.pullConfig, `"https://index.docker.io/v1/"`) {
			t.Fatalf("%s: %s", prefix, e.docker.pullConfig)
		}
	}
}

func TestAnOfflineInstallStaysOfflineOnARerun(t *testing.T) {
	e := newEnv(t)
	tags := map[string]string{"ghcr.io/acme/notes/notes-front:1.0.0": config, "ghcr.io/acme/notes/notes-api:1.0.0": config,
		"ghcr.io/acme/notes/notes-web:1.0.0": config, "ghcr.io/acme/notes/notes-postgres:1.0.0": config}
	for r, id := range tags {
		e.docker.loaded[r] = id
	}
	if err := e.install(func(o *Options) { o.Offline = true }); err != nil {
		t.Fatal(err)
	}
	// Re-run without --offline: the recorded tags are still checked, and never pulled.
	e.docker.loaded["ghcr.io/acme/notes/notes-web:1.0.0"] = "sha256:other"
	if err := e.install(nil); err == nil || !strings.Contains(err.Error(), "notes-web:1.0.0 is sha256:other") {
		t.Fatalf("a re-run trusted a changed tag: %v", err)
	}
	delete(e.docker.loaded, "ghcr.io/acme/notes/notes-web:1.0.0")
	if err := e.install(nil); err == nil || !strings.Contains(err.Error(), "not loaded") {
		t.Fatalf("a re-run with a missing tag: %v", err)
	}
	if n := len(e.docker.ran("pull")); n != 0 {
		t.Fatalf("%d pulls of a tag", n)
	}
}

func TestAnInterruptedFirstInstallIsFinishedByARerun(t *testing.T) {
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	// Interrupted after .env, before the stack files.
	for _, n := range []string{"compose.yaml", FrontCompose, Caddyfile} {
		_ = os.Remove(filepath.Join(e.dir, n))
	}
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "compose.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestEnvValuesComposeWouldMisread(t *testing.T) {
	for _, v := range []string{"o'brien", "a\nb", `ends with \`} {
		if err := spec.CheckEnvValue(v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
	for _, v := range []string{`a\b`, "pa$$ #1", ""} {
		if err := spec.CheckEnvValue(v); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
}

func TestAFailedPullWithTheAccountLeavesNothing(t *testing.T) {
	e := newEnv(t)
	e.r.Spec.Registry.Pull = spec.Provider{Name: "token", Config: map[string]any{"user": "pull", "token_env": "T"}}
	e.vars["T"] = "x"
	e.vars["DOCKER_CONFIG"] = t.TempDir()
	e.docker.pullErr = errors.New("unauthorized")
	if err := e.install(nil); err == nil {
		t.Fatal("installed")
	}
	e.nothingWritten()
}

func TestTheBakedPullAccountPullsWhenTheEnvironmentHasNone(t *testing.T) {
	e := newEnv(t)
	e.r.Spec.Registry.Pull = spec.Provider{Name: "token", Config: map[string]any{"user": "pull", "token_env": "NOTES_PULL_TOKEN"}}
	e.r.Pull = &payload.Account{User: "baked", Password: "b4ked"}
	e.vars["DOCKER_CONFIG"] = t.TempDir()
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if want := base64.StdEncoding.EncodeToString([]byte("baked:b4ked")); !strings.Contains(e.docker.pullConfig, want) {
		t.Fatalf("pull config: %s", e.docker.pullConfig)
	}
	// The operator's own password for the account wins over the baked one (a rotation).
	e2 := newEnv(t)
	e2.r.Spec.Registry.Pull = e.r.Spec.Registry.Pull
	e2.r.Pull = e.r.Pull
	e2.vars["DOCKER_CONFIG"] = t.TempDir()
	e2.vars["NOTES_PULL_TOKEN"] = "rotated"
	if err := e2.install(nil); err != nil {
		t.Fatal(err)
	}
	if want := base64.StdEncoding.EncodeToString([]byte("pull:rotated")); !strings.Contains(e2.docker.pullConfig, want) {
		t.Fatalf("pull config: %s", e2.docker.pullConfig)
	}
}

func TestAPayloadPullAccountIsReadStrictly(t *testing.T) {
	t.Parallel()
	_, exe := testInstaller(t, "1.0.0")
	files, err := payload.Read(exe)
	if err != nil {
		t.Fatal(err)
	}
	for raw, want := range map[string]string{
		`{"user":"pull","password":"s3cret"}`:             "",
		`{"user":"pull","password":"s3cret","token":"x"}`: "unknown field",
		`{"user":"pull"}`: "a user and a password",
		`null`:            "a user and a password",
		`{"user":"pull","password":"s3cret"}{"user":"evil"}`: "trailing data",
	} {
		files[payload.Pull] = []byte(raw)
		r, err := Load(files)
		switch {
		case want == "" && (err != nil || r.Pull == nil || r.Pull.Password != "s3cret"):
			t.Errorf("%s: %v", raw, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: %v, want %q", raw, err, want)
		}
	}
}

// The front is the release's: installing or upgrading to a release without one is refused before
// anything runs, while one is still read as a kept or previous release.
func TestAReleaseWithoutAFrontIsNotInstalled(t *testing.T) {
	t.Parallel()
	_, exe := testInstaller(t, "1.0.0")
	files, err := payload.Read(exe)
	if err != nil {
		t.Fatal(err)
	}
	m, err := release.Decode(files[payload.Release])
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []map[string]string{m.Artifacts.Images, m.Artifacts.Configs, m.Artifacts.Manifests} {
		delete(a, front.Key)
	}
	if files[payload.Release], err = m.Encode(); err != nil {
		t.Fatal(err)
	}
	r, err := Load(files)
	if err != nil {
		t.Fatalf("a kept release without a front is unreadable: %v", err)
	}
	e := newEnv(t)
	e.r = r
	if err := e.install(nil); err == nil || !strings.Contains(err.Error(), "has no front image") {
		t.Fatalf("install: %v", err)
	}
	e.nothingWritten()
	if err := e.upgrade(nil); err == nil || !strings.Contains(err.Error(), "has no front image") {
		t.Fatalf("upgrade: %v", err)
	}
}

func TestScriptPrintsTheInstallerItself(t *testing.T) {
	t.Parallel()
	r, exe := testInstaller(t, "1.0.0")
	var out bytes.Buffer
	c := Command(r, exe, &out, io.Discard)
	c.SetArgs([]string{payload.ScriptCommand})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	start := strings.Index(s, "<<'STOWAGE_INSTALLER_END'\n")
	end := strings.Index(s, "\nSTOWAGE_INSTALLER_END\n")
	if !strings.HasPrefix(s, "#!/bin/sh\n") || start < 0 || end < start {
		t.Fatalf("not a bootstrap script:\n%.300s", s)
	}
	got, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(s[start+len("<<'STOWAGE_INSTALLER_END'\n"):end], "\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(exe)
	if !bytes.Equal(got, want) {
		t.Fatal("the script does not carry this installer")
	}
}

func TestAnImageGoneAsTheStackStartsIsPulledAgain(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ups := 0
	e.docker.onUp = func() error {
		ups++
		if ups == 1 { // another process removed an image between the pull and the start
			for ref := range e.docker.loaded {
				if strings.Contains(ref, "notes-web@") {
					delete(e.docker.loaded, ref)
					return errors.New("No such image")
				}
			}
			t.Fatal("no web image was pulled")
		}
		return nil
	}
	if err := e.install(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if ups != 2 || !strings.Contains(e.log.String(), "left the engine as the stack started: pulling again") {
		t.Fatalf("%d ups:\n%s", ups, e.log)
	}
	// A failure with every image there is the stack's own: no second start.
	ups = 0
	e.docker.onUp = func() error { ups++; return errors.New("unhealthy") }
	if err := e.install(nil); err == nil || ups != 1 {
		t.Fatalf("%v, %d ups", err, ups)
	}
}
