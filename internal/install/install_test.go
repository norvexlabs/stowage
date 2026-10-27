package install

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/chown"
	"github.com/norvexlabs/stowage/internal/front"
	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// fakeDocker answers the docker CLI the installer runs and records every call.
type fakeDocker struct {
	// mu guards calls: a backup's dump and upload run at once.
	mu     sync.Mutex
	calls  []Cmd
	loaded map[string]string // image reference → the ID `image inspect` reports
	// loadedBytes is what `docker load` read; it loads loadAs, which `image inspect` then answers
	// with loadInspect.
	loadedBytes         int64
	loadAs, loadInspect string
	loadErr             error
	compose             string // `compose version --short`
	pullErr             error
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
	upOut  string            // what `compose up` prints
	onUp   func() error      // when set, runs at each `compose up` and gives its error
	// schemaFiles are the schema command's compose files, one per run.
	schemaFiles []string
	checked     func() // runs when the installer checks Compose's version
	inspected   func() // runs when the installer looks for a leftover container
	// answer, when it answers (ok), answers a call before anything else.
	answer func(args string) (out []byte, ok bool, err error)
	// runner answers `docker run` of the backup runner.
	runner func(c Cmd) ([]byte, error)
	// model is what `compose config --format json` prints; empty, an empty stack.
	model string
	// owner answers the run that gives the data volumes their uid.
	owner func(c Cmd) ([]byte, error)
	// volumes are the names `volume ls` prints, one per line.
	volumes string
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
	case a == "volume ls --format {{.Name}}":
		return []byte(f.volumes), nil
	case strings.HasPrefix(a, "compose") && strings.HasSuffix(a, " config --format json"):
		return []byte(cmp.Or(f.model, "{}")), nil
	case strings.HasPrefix(a, "run --rm --pull never --network none --user 0 --entrypoint sh "):
		if f.owner == nil {
			return nil, nil
		}
		return f.owner(c)
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
		if c.Stream != nil {
			_, _ = io.WriteString(c.Stream, f.upOut)
		}
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
	case a == "load --quiet":
		if f.loadErr != nil {
			return nil, f.loadErr // as a daemon that refuses it, before reading it all
		}
		n, err := io.Copy(io.Discard, c.Stdin)
		f.loadedBytes += n
		if f.loadAs != "" {
			f.loaded[f.loadAs] = f.loadInspect
		}
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
	keys := append(p.Spec.ImageNames(), front.Key)
	if providers.OwnsVolumes(&view) { // as ship publishes it
		keys = append(keys, chown.Key)
	}
	for _, k := range keys {
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
	// udpBusy are the UDP ports a socket of the host holds.
	udpBusy map[int]bool
}

func newEnv(t *testing.T) *env {
	r, self := testInstaller(t, "1.0.0")
	return &env{t: t, r: r, self: self, docker: &fakeDocker{loaded: map[string]string{}, compose: "2.29.1"},
		dir: filepath.Join(t.TempDir(), "notes"), cos: t.TempDir(), out: &bytes.Buffer{}, log: &bytes.Buffer{},
		vars: map[string]string{"ADMIN_EMAIL": "admin@example.org"}, busy: map[int]bool{}}
}

func (e *env) installer(mod func(*Options)) *Installer {
	o := Options{Dir: e.dir, Cos: e.cos, Out: e.out, Log: e.log, Docker: e.docker, Self: e.self,
		Getenv:      func(k string) string { return e.vars[k] },
		Environ:     []string{"PATH=/bin", "SESSION_SECRET=from-the-shell", "COMPOSE_PROJECT_NAME=other"},
		PortFree:    func(_ string, p int) bool { return !e.busy[p] },
		HostAddrs:   func() []string { return []string{"127.0.0.1", "::1", "192.0.2.7", "192.0.2.8"} },
		UDPPortFree: func(_ string, p int) bool { return !e.udpBusy[p] },
		Exec:        inProcess(e, mod, nil)}
	if mod != nil {
		mod(&o)
	}
	return &Installer{R: e.r, O: o}
}

// inProcess runs the installers a run starts as the host would, in this process: each loads its
// own release from its file and runs its verb — boot, boot-rollback, or a re-run of the install
// (--dir and its flags) — with the options mod gives, running the next the same way; ran, when not
// nil, records each one's arguments.
func inProcess(e *env, mod func(*Options), ran *[][]string) func(context.Context, string, []string) error {
	var run func(context.Context, string, []string) error
	run = func(ctx context.Context, exe string, args []string) error {
		if ran != nil {
			*ran = append(*ran, args)
		}
		files, err := payload.Read(exe)
		if err != nil {
			return err
		}
		if args[0] == "--help" { // its own payload, as its Stowage reads it: anotherStowages's knows Retired
			files[payload.Spec] = bytes.Replace(files[payload.Spec], []byte(`{"Retired":1,`), []byte(`{`), 1)
		}
		r, err := Load(files)
		if err != nil {
			return err
		}
		if args[0] == "--help" {
			return nil
		}
		in := e.installer(mod)
		in.R, in.O.Self, in.O.Exec = r, exe, run
		switch args[0] {
		case bootCmd:
			return in.Boot(ctx)
		case bootRollbackCmd:
			return in.BootRollback(ctx)
		case "--dir":
			if r.Manifest.Bundled() {
				return errors.New("a VM's installer has no re-run: unknown flag --dir")
			}
			for i := 0; i < len(args); i++ {
				switch args[i] {
				case "--dir":
					i++
					in.O.Dir = args[i]
				case "--no-input":
				case "--no-provision":
					in.O.NoProvision = true
				case "--offline":
					in.O.Offline = true
				default:
					return errors.New("no flag " + args[i])
				}
			}
			return in.Install(ctx)
		}
		return errors.New("no verb " + args[0])
	}
	return run
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
	for _, n := range []string{"compose.yaml", FrontCompose, Caddyfile, ContentCompose} {
		if _, err := os.Stat(filepath.Join(e.dir, n)); err != nil {
			t.Error(err)
		}
	}
	if n := len(e.docker.ran("pull")); n != 5 {
		t.Errorf("%d pulls, want 5 (3 images, the front and the volume helper)", n)
	}
	up := e.docker.ran("compose")
	if len(up) != 4 || !strings.HasSuffix(strings.Join(up[1].Args, " "), "config --format json") || !strings.Contains(strings.Join(up[2].Args, " "), "up --detach --wait") || !strings.HasSuffix(strings.Join(up[3].Args, " "), "run --rm --no-deps -T --name "+get(e.readEnv(), "COMPOSE_PROJECT_NAME")+"-"+ProvisionService+" "+ProvisionService) {
		t.Fatalf("compose calls: %v", up)
	}
	// The recorded values win over the shell's.
	envs := strings.Join(up[2].Env, "\n")
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
	e.docker.loaded["ghcr.io/acme/notes/notes-chown:1.0.0"] = manifest
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
	// The peers' addresses are no front's, on both sides of their /24.
	for a, ok := range map[string]bool{"127.0.3.255": true, "127.0.4.0": false, "127.0.4.37": false, "::ffff:127.0.4.255": false, "127.0.5.0": true} {
		if _, err := loopbackAddr(a); (err == nil) != ok || err != nil && !strings.Contains(err.Error(), "is in 127.0.4.0/24, the addresses the services on the host") {
			t.Errorf("%s: %v", a, err)
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

// An input turns its compose profile on while it holds a value; once it is emptied, the profile's
// services are removed, as `up` would leave them running. A shell's COMPOSE_PROFILES starts none.
func TestAnInputTurnsItsProfileOn(t *testing.T) {
	e := newEnv(t)
	e.r.Spec.Inputs = append(e.r.Spec.Inputs, spec.Input{Key: "RELAY_SOURCE", Prompt: "Relay source", Optional: true, Profile: "relay"})
	e.r.Compose = []byte(strings.Replace(string(e.r.Compose), "services:\n", "services:\n  relay:\n    image: ${STOWAGE_IMAGE_API}\n    profiles: [relay]\n", 1))
	removed := func() bool {
		return slices.ContainsFunc(e.docker.calls, func(c Cmd) bool {
			return strings.HasSuffix(strings.Join(c.Args, " "), " --profile relay rm --stop --force relay")
		})
	}
	for _, c := range []struct {
		set     map[string]string
		on      string
		removes bool
	}{
		{nil, "", true}, // never asked a value: off
		{map[string]string{"RELAY_SOURCE": "sensors"}, "relay", false},
		{nil, "relay", false}, // kept
		{map[string]string{"RELAY_SOURCE": ""}, "", true},
	} {
		e.docker.calls = nil
		if err := e.install(func(o *Options) {
			o.Set, o.Environ = c.set, append(o.Environ, "COMPOSE_PROFILES=relay")
		}); err != nil {
			t.Fatal(err)
		}
		env := e.readEnv()
		got, recorded := env.Get(ProfilesVar)
		up := slices.IndexFunc(e.docker.calls, func(c Cmd) bool { return slices.Contains(c.Args, "up") })
		if !recorded || got != c.on || removed() != c.removes || up < 0 || !slices.Contains(e.docker.calls[up].Env, ProfilesVar+"="+c.on) {
			t.Errorf("--set %v: recorded %q (%v), removed %v", c.set, got, recorded, removed())
		}
	}
	// A release whose inputs turn no profile on records none.
	e = newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.readEnv().Get(ProfilesVar); ok {
		t.Error("recorded without a profile input")
	}
}

// An upgrade to a release whose inputs turn no profile on drops the recorded profiles.
func TestAnUpgradeDropsTheProfiles(t *testing.T) {
	t.Parallel()
	in := &Installer{R: testRelease(t, "2.0.0")}
	env := NewEnvFile()
	env.Set("STOWAGE_REGISTRY", "ghcr.io/acme/notes")
	env.Set(ProfilesVar, "relay")
	next, _, err := in.upgradeEnv(env, Loopback, nil, nil)
	if _, ok := next.Get(ProfilesVar); err != nil || ok {
		t.Errorf("recorded after the upgrade: %v", err)
	}
}

// An L4 route publishes its host port to its service in every mode, and hands the stack its
// endpoint; the front's ports never take one of its TCP ports, and its port must be free at the
// first install. The project's front ports are the defaults an install moves.
func TestL4Routes(t *testing.T) {
	newL4 := func() *env {
		e := newEnv(t)
		s := e.r.Spec
		s.Inputs = append(s.Inputs, spec.Input{Key: "DATA_ADDRESS", Prompt: "Data address", Optional: true},
			spec.Input{Key: "DATA_SOURCES", Prompt: "Data sources", Optional: true})
		s.L4.Routes = []spec.L4Route{
			{Name: "data", Protocol: "tcp", Port: 7443, Upstream: "api:7000", AddressInput: "DATA_ADDRESS"},
			{Name: "data-udp", Protocol: "udp", Port: 7443, Upstream: "api:7000", AllowInput: "DATA_SOURCES"},
		}
		s.Front.HTTPSPort = 8443
		e.vars["APP_DOMAIN"], e.vars["API_DOMAIN"] = "app.example.org", "api.example.org"
		return e
	}
	tls := func(o *Options) { o.TLS = "internal" }
	e := newL4()
	e.vars["DATA_ADDRESS"] = "fd00::7"
	if err := e.install(tls); err != nil {
		t.Fatal(err)
	}
	env := e.readEnv()
	if got := get(env, "STOWAGE_L4_DATA"); got != "[fd00::7]:7443" {
		t.Errorf("STOWAGE_L4_DATA = %q", got)
	}
	if _, ok := env.Get("STOWAGE_L4_DATA_UDP"); ok {
		t.Error("an endpoint recorded for a route without an address input")
	}
	if got := get(env, HTTPSPortVar); got != "8443" {
		t.Errorf("the project's HTTPS port: %q", got)
	}
	fc, _ := os.ReadFile(filepath.Join(e.dir, FrontCompose))
	for _, want := range []string{"  api:\n        ports:\n            - protocol: tcp\n              published: \"7443\"\n              target: \"7000\"\n            - protocol: udp\n              published: \"7443\"\n              target: \"7000\"\n"} {
		if !strings.Contains(string(fc), want) {
			t.Errorf("no %q in:\n%s", want, fc)
		}
	}
	if err := e.install(func(o *Options) { o.Set = map[string]string{"DATA_ADDRESS": "data.example.org:7443"} }); err == nil || !strings.Contains(err.Error(), "is not a host name or an IP address") {
		t.Errorf("an address with a port: %v", err)
	}
	if err := e.install(func(o *Options) { o.Set = map[string]string{"DATA_SOURCES": "192.0.2.0/24; tcp dport 22 accept"} }); err == nil || !strings.Contains(err.Error(), "is not an IP address or a CIDR") {
		t.Errorf("an allowlist of rules: %v", err)
	}
	// A re-run that empties the address empties the endpoint; one that moves the front onto the
	// route's TCP port is refused.
	if err := e.install(func(o *Options) { o.Set = map[string]string{"DATA_ADDRESS": ""} }); err != nil || get(e.readEnv(), "STOWAGE_L4_DATA") != "" {
		t.Fatalf("%v %q", err, get(e.readEnv(), "STOWAGE_L4_DATA"))
	}
	if err := e.install(func(o *Options) { o.HTTPSPort = 7443 }); err == nil || !strings.Contains(err.Error(), "the front's https port 7443 is the L4 route data's: choose another with --https-port") {
		t.Errorf("err = %v", err)
	}
	var out bytes.Buffer
	if err := e.installer(func(o *Options) { o.Out = &out }).Status(context.Background()); err != nil || !strings.Contains(out.String(), "\ndata      tcp/7443 · on every address, open to whoever reaches this host\ndata-udp  udp/7443 · on every address, open to whoever reaches this host\n") {
		t.Errorf("%v\n%s", err, out.String())
	}
	// The first install: an option over the project's port, and the route's port free.
	e = newL4()
	if err := e.install(func(o *Options) { tls(o); o.HTTPPort = 7443 }); err == nil || !strings.Contains(err.Error(), "--http-port") {
		t.Errorf("err = %v", err)
	}
	e.busy[7443] = true
	if err := e.install(tls); err == nil || !strings.Contains(err.Error(), "port(s) 7443 (data) already in use on this host: the project's L4 routes publish them") {
		t.Errorf("err = %v", err)
	}
	// A UDP route's port, which another socket holds.
	e = newL4()
	e.udpBusy = map[int]bool{7443: true}
	if err := e.install(tls); err == nil || !strings.Contains(err.Error(), "port(s) 7443 (data-udp) already in use on this host") {
		t.Errorf("err = %v", err)
	}
	// Held on another address than the host's own (a local resolver's): the wildcard is probed.
	e = newL4()
	wildcard := func(o *Options) {
		tls(o)
		o.UDPPortFree = func(h string, p int) bool { return h != "0.0.0.0" || p != 7443 }
	}
	if err := e.install(wildcard); err == nil || !strings.Contains(err.Error(), "port(s) 7443 (data-udp) already in use on this host") {
		t.Errorf("err = %v", err)
	}
	// Loopback mode publishes them too; a route's loopback port is the front's.
	e = newL4()
	if err := e.install(func(o *Options) { o.TLS, o.Ports = Loopback, map[string]int{"app": 7443} }); err == nil || !strings.Contains(err.Error(), "the front's app port 7443 is the L4 route data's: choose another with --ports") {
		t.Errorf("err = %v", err)
	}
	if err := e.install(func(o *Options) { o.TLS = Loopback }); err != nil {
		t.Fatal(err)
	}
	if fc, _ := os.ReadFile(filepath.Join(e.dir, FrontCompose)); !strings.Contains(string(fc), `published: "7443"`) {
		t.Errorf("loopback mode:\n%s", fc)
	}
	// The provision hook is handed the endpoint, like the routes' URLs.
	if keys := e.installer(nil).providedEnv(e.readEnv()); !slices.Contains(keys, "STOWAGE_L4_DATA") {
		t.Errorf("provided: %v", keys)
	}
}

func TestAnL4RoutesExposureIsSaid(t *testing.T) {
	t.Parallel()
	bound := spec.L4Route{Name: "data", Protocol: "tcp", Port: 7443, BindInput: "DATA_BIND"}
	for _, c := range []struct {
		r        spec.L4Route
		bind, fw string
		want     string
	}{
		{bound, "192.0.2.7", "", "on 192.0.2.7 alone"},
		{bound, "", "", "on every address, open to whoever reaches this host (DATA_BIND gives it one address)"},
		{bound, "0.0.0.0", "", "on every address, open to whoever reaches this host (DATA_BIND gives it one address)"},
		{spec.L4Route{Name: "data", Protocol: "udp", Port: 7443}, "", "", "on every address, open to whoever reaches this host"},
		{bound, "", "/run/notes/firewall", ""}, // the VM's firewall applies the allowlist
		{spec.L4Route{Name: "data", Protocol: "tcp", Port: 7443, Upstream: "proxy:7443"}, "", "", "on the addresses proxy binds on the host's network, behind this host's own firewall"},
	} {
		in := &Installer{R: &Release{Spec: &spec.Spec{Services: map[string]spec.Service{"proxy": {HostNetwork: true}}}}, O: Options{Firewall: c.fw}}
		if got := in.l4Exposure(c.r, func(k string) string { return map[string]string{"DATA_BIND": c.bind}[k] }); got != c.want {
			t.Errorf("%+v: %q", c, got)
		}
	}
}

// An L4 route bound to one host address is published there alone, and its port probed there alone:
// two installs holding one port on two addresses of a host.
func TestAnL4RouteBoundToOneAddress(t *testing.T) {
	newL4 := func() *env {
		e := newEnv(t)
		s := e.r.Spec
		s.Inputs = append(s.Inputs, spec.Input{Key: "DATA_BIND", Prompt: "Data bind address", Advanced: true})
		s.L4.Routes = []spec.L4Route{
			{Name: "data", Protocol: "tcp", Port: 7443, Upstream: "api:7000", BindInput: "DATA_BIND"},
			{Name: "data-udp", Protocol: "udp", Port: 7443, Upstream: "api:7000", BindInput: "DATA_BIND"},
		}
		return e
	}
	// Another install holds the port on every address but 192.0.2.7.
	var tcp, udp []string
	held := func(o *Options) {
		probe := func(on *[]string) func(string, int) bool {
			return func(h string, p int) bool {
				if p != 7443 {
					return true
				}
				*on = append(*on, h)
				return h == "192.0.2.7" || h == "0.0.0.0" // a dial of the wildcard reaches the loopback alone
			}
		}
		o.PortFree, o.UDPPortFree = probe(&tcp), probe(&udp)
	}
	bind := func(a string) func(*Options) {
		return func(o *Options) { held(o); o.Set = map[string]string{"DATA_BIND": a} }
	}
	e := newL4()
	if err := e.install(held); err == nil || !strings.Contains(err.Error(), "port(s) 7443 (data), 7443 (data-udp) already in use on this host") {
		t.Fatalf("on every address: %v", err)
	}
	tcp, udp = nil, nil
	if err := e.install(bind("192.0.2.8")); err == nil || !strings.Contains(err.Error(), "port(s) 7443 (data), 7443 (data-udp) already in use") {
		t.Fatalf("on an address held: %v", err)
	}
	// The unspecified address is every address: probed on each.
	if err := e.install(bind("0.0.0.0")); err == nil || !strings.Contains(err.Error(), "port(s) 7443 (data), 7443 (data-udp) already in use") {
		t.Fatalf("on 0.0.0.0: %v", err)
	}
	if err := e.install(bind("data.example.org")); err == nil || !strings.Contains(err.Error(), "is not an IP address") {
		t.Fatalf("a host name: %v", err)
	}
	if err := e.install(bind("192.0.2.99")); err == nil || !strings.Contains(err.Error(), "192.0.2.99 (DATA_BIND, the L4 route data's address) is not an address of this host") {
		t.Fatalf("an address of another host: %v", err)
	}
	e.nothingWritten()
	tcp, udp = nil, nil
	// An IPv4-mapped address is that IPv4 address.
	if err := e.install(bind("::ffff:192.0.2.7")); err != nil {
		t.Fatal(err)
	}
	if len(tcp) == 0 || len(udp) == 0 || slices.ContainsFunc(append(tcp, udp...), func(h string) bool { return h != "192.0.2.7" }) {
		t.Errorf("probed %v (tcp), %v (udp)", tcp, udp)
	}
	fc, _ := os.ReadFile(filepath.Join(e.dir, FrontCompose))
	if strings.Count(string(fc), "host_ip: 192.0.2.7\n") != 2 {
		t.Errorf("not published on 192.0.2.7 alone:\n%s", fc)
	}
	if err := e.install(func(o *Options) { o.Set = map[string]string{"DATA_BIND": "192.0.2.99"} }); err == nil || !strings.Contains(err.Error(), "is not an address of this host") {
		t.Fatalf("a re-run onto an address of another host: %v", err)
	}
	// A re-run emptying it publishes on every address again.
	if err := e.install(func(o *Options) { o.Set = map[string]string{"DATA_BIND": ""} }); err != nil {
		t.Fatal(err)
	}
	if fc, _ := os.ReadFile(filepath.Join(e.dir, FrontCompose)); strings.Contains(string(fc), "host_ip") {
		t.Errorf("still bound:\n%s", fc)
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
	if len(e.docker.ran("pull")) != 0 || len(e.docker.ran("compose")) != 4 {
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
	if len(e.docker.ran("compose")) != 4 || !strings.Contains(e.out.String(), "http://localhost:18080") {
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
	cf := loopbackCaddyfile(e.r.Spec.Front, func(string) string { return "" }, nil)
	for _, want := range []string{"http://:10000 {\n\treverse_proxy web:8080", "http://:10001 {\n\trequest_header -X-Auth-Request-*\n\trequest_header -X-Tenant\n\treverse_proxy api:8000", "admin off"} {
		if !strings.Contains(cf, want) {
			t.Errorf("Caddyfile lacks %q:\n%s", want, cf)
		}
	}
	fc, err := frontCompose(e.r.Spec.Front, nil, nil, Loopback, hostAPI{}, "sha256:x", "", false, false, nil)
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
	tags := map[string]string{"ghcr.io/acme/notes/notes-front:1.0.0": config, "ghcr.io/acme/notes/notes-chown:1.0.0": config, "ghcr.io/acme/notes/notes-api:1.0.0": config,
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

// Compose's --wait fails once a one-shot service no other one waits for exits, whatever its code:
// the install then judges the stack by its services' state.
func TestAStackUpWaitRefusedIsJudgedByItsServices(t *testing.T) {
	unit := settleUnit
	settleUnit = 0
	t.Cleanup(func() { settleUnit = unit })
	row := func(svc, state, health string, code int) string {
		return `{"Service":"` + svc + `","State":"` + state + `","Health":"` + health + `","ExitCode":` + strconv.Itoa(code) + `}`
	}
	exited := " Container notes-api-1 Healthy\ncontainer notes-check-1 exited (0)\n"
	for _, c := range []struct {
		name string
		// polls answers each `compose ps`, the last one again once they run out.
		polls []string
		want  string // empty: the install succeeds
		// out is what up printed; empty, its wait refusing a container that exited.
		out string
	}{
		{"a one-shot exited 0", []string{row("api", "running", "", 0) + "\n" + row("check", "exited", "", 0)}, "", ""},
		{"a healthcheck still starting is waited for", []string{row("api", "running", "starting", 0), row("api", "running", "healthy", 0)}, "", ""},
		{"a one-shot exited 1", []string{row("api", "running", "", 0) + "\n" + row("check", "exited", "", 1)}, "check exited with code 1", ""},
		{"an unhealthy service", []string{row("api", "running", "unhealthy", 0)}, "api is unhealthy", ""},
		{"a container never started", []string{row("api", "created", "", 0)}, "api is created", ""},
		{"a restarting service", []string{row("api", "restarting", "", 0)}, "api is restarting", ""},
		{"no service", []string{""}, "no service of the stack runs", ""},
		{"healthy at the last poll", append(slices.Repeat([]string{row("api", "running", "starting", 0)}, settlePolls), row("api", "running", "healthy", 0)), "", ""},
		{"still starting after the last poll", append(slices.Repeat([]string{row("api", "running", "starting", 0)}, settlePolls+1), row("api", "running", "healthy", 0)), "api still starting", ""},
		// Up refused before its wait: the containers may be the last configuration's, and are
		// never judged.
		{"a refusal before the wait", []string{row("api", "running", "", 0)}, "-", " Network notes_default Error\nfailed to create network notes_default: exists\n"},
		{"an exit without its code", []string{row("api", "running", "", 0)}, "-", "container notes-check-1 exited (0.5)\n"},
		{"an exit named before the last line", []string{row("api", "running", "", 0)}, "-", "container notes-check-1 exited (0)\nError response from daemon: no such network\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.docker.upErr = errors.New("up refused")
			e.docker.upOut = cmp.Or(c.out, exited)
			n := 0
			e.docker.answer = func(a string) ([]byte, bool, error) {
				if !strings.HasSuffix(a, " ps --all --format json") {
					return nil, false, nil
				}
				out := c.polls[min(n, len(c.polls)-1)]
				n++
				return []byte(out), true, nil
			}
			err := e.install(nil)
			switch c.want {
			case "":
				if err != nil {
					t.Fatal(err)
				}
				if !slices.ContainsFunc(e.docker.ran("compose"), func(c Cmd) bool { return strings.Contains(strings.Join(c.Args, " "), " run --rm ") }) {
					t.Fatal("the provision hook did not run")
				}
			case "-":
				if err == nil || !strings.HasPrefix(strings.TrimPrefix(err.Error(), "the stack did not become ready: "), "up refused (") || n != 0 {
					t.Fatalf("got %v after %d polls, want compose's error alone", err, n)
				}
			default:
				if err == nil || !strings.Contains(err.Error(), "up refused: "+c.want) {
					t.Fatalf("got %v, want %q after compose's error", err, c.want)
				}
			}
		})
	}
	if settlePolls < 2 {
		t.Fatal("the bound's cases need two polls at least")
	}
}

// Compose's refusal is read through its colours, and only its output's last 4 KiB are kept; a
// judgement that cannot read the services says why, after compose's error.
func TestUpWaitsRefusalIsReadAtTheEndOfItsOutput(t *testing.T) {
	t.Parallel()
	var b tailBuffer
	_, _ = b.Write([]byte(strings.Repeat("x", 5000) + "\n"))
	_, _ = b.Write([]byte("\x1b[31mcontainer notes-check-1 exited (0)\x1b[0m\n"))
	if len(b.Bytes()) != 4<<10 || !waitExited(b.Bytes()) {
		t.Errorf("%d bytes kept, exit read %v", len(b.Bytes()), waitExited(b.Bytes()))
	}
	for _, out := range []string{"container notes-check-1 exited (0)\nhint: see the logs\n", "Error: container notes-check-1 exited (0)\n", ""} {
		if waitExited([]byte(out)) {
			t.Errorf("%q read as the wait's refusal", out)
		}
	}
	e := newEnv(t)
	e.docker.upErr, e.docker.upOut = errors.New("up refused"), "container notes-check-1 exited (0)\n"
	e.docker.answer = func(a string) ([]byte, bool, error) {
		return nil, strings.HasSuffix(a, " ps --all --format json"), errors.New("daemon gone")
	}
	if err := e.install(nil); err == nil || !strings.Contains(err.Error(), "up refused: judging its services: daemon gone") {
		t.Errorf("err = %v", err)
	}
}

// Where the stack holds an L4 route's port already: on that address or on every one; a wildcard
// is never taken as held.
func TestHeldOn(t *testing.T) {
	t.Parallel()
	tcp := spec.L4Route{Name: "data", Protocol: "tcp", Port: 7443}
	udp := spec.L4Route{Name: "dns", Protocol: "udp", Port: 53}
	for _, c := range []struct {
		own  string
		r    spec.L4Route
		host string
		held bool
	}{
		{"tcp/7443", tcp, "192.0.2.8", true},
		{"tcp/192.0.2.7/7443", tcp, "192.0.2.7", true},
		{"tcp/192.0.2.7/7443", tcp, "::ffff:192.0.2.7", true},
		{"tcp/192.0.2.7/7443", tcp, "192.0.2.8", false},
		{"udp/7443", tcp, "192.0.2.8", false},
		{"udp/192.0.2.7/53", udp, "0.0.0.0", false},
		{"udp/53", udp, "::", false},
	} {
		if got := heldOn(map[string]bool{c.own: true}, c.r, c.host); got != c.held {
			t.Errorf("%s holds %s on %s: %v, want %v", c.own, c.r.Name, c.host, got, c.held)
		}
	}
}

// A re-run moving an L4 route's bind address probes the port there, unless the stack holds it
// there already; one keeping it probes nothing. Unknown host addresses refuse no address.
func TestAReRunMovingAnL4RouteProbesItsNewAddress(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.r.Spec.Inputs = append(e.r.Spec.Inputs, spec.Input{Key: "DATA_BIND", Prompt: "Data bind address", Advanced: true})
	e.r.Spec.L4.Routes = []spec.L4Route{{Name: "data", Protocol: "tcp", Port: 7443, Upstream: "api:7000", BindInput: "DATA_BIND"}}
	published := "0.0.0.0"
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if strings.HasSuffix(a, " ps --format json") {
			return []byte(`{"Service":"api","Publishers":[{"URL":"` + published + `","TargetPort":7000,"PublishedPort":7443,"Protocol":"tcp"}]}`), true, nil
		}
		return nil, false, nil
	}
	var probed []string
	// The stack's socket answers wherever it listens, and another install holds 192.0.2.8.
	held := func(o *Options) {
		o.PortFree = func(h string, p int) bool {
			if p == 7443 {
				probed = append(probed, h)
			}
			return p != 7443 || h != "192.0.2.8" && (published == "0.0.0.0" || h != published)
		}
	}
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	set := func(v string) func(*Options) {
		return func(o *Options) { held(o); o.Set = map[string]string{"DATA_BIND": v} }
	}
	// From every address, which the stack holds, to one of them.
	if err := e.install(set("192.0.2.7")); err != nil {
		t.Fatalf("onto an address the stack holds: %v", err)
	}
	published, probed = "192.0.2.7", nil
	if err := e.install(func(o *Options) { held(o); o.Set = map[string]string{"ADMIN_EMAIL": "x@example.org"} }); err != nil || len(probed) != 0 {
		t.Fatalf("a re-run keeping the address: %v, probed %v", err, probed)
	}
	if err := e.install(set("192.0.2.8")); err == nil || !strings.Contains(err.Error(), "port(s) 7443 (data) already in use") {
		t.Fatalf("onto an address another install holds: %v", err)
	}
	// The route's service on the host's network (the VM's) holds the port wherever it listens: a
	// move is not refused by its own socket.
	published = "host"
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if strings.HasSuffix(a, " ps --format json") {
			return []byte(`{"Service":"api","Networks":"host","Publishers":[]}`), true, nil
		}
		return nil, false, nil
	}
	if err := e.install(func(o *Options) {
		set("0.0.0.0")(o)
		o.PortFree = func(h string, p int) bool { return p != 7443 }
	}); err != nil {
		t.Errorf("the service on the host's network moving: %v", err)
	}
	if err := e.install(func(o *Options) {
		set("192.0.2.99")(o)
		o.HostAddrs = func() []string { return nil }
		o.PortFree = func(string, int) bool { return true }
	}); err != nil {
		t.Errorf("with the host's addresses unknown: %v", err)
	}
}

// The UDP probe: a port a socket holds is busy, and free again once it closes.
func TestUDPPortFree(t *testing.T) {
	t.Parallel()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	if udpPortFree("127.0.0.1", port) {
		t.Error("a held port is free")
	}
	c.Close()
	if !udpPortFree("127.0.0.1", port) {
		t.Error("a released port is busy")
	}
}
