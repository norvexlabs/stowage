package install

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/front"
	"github.com/norvexlabs/stowage/internal/kairos"
	"github.com/norvexlabs/stowage/internal/spec"
)

// The front is confined as every service Stowage adds: its own user, no capability, no privilege
// gain, a read-only root and /tmp alone writable; it binds 80 and 443 as its network namespace lets
// any user, and mounts none of the install's byo files.
func TestTheFrontIsConfined(t *testing.T) {
	t.Parallel()
	f := l4TLSFront()
	f.PublishCA = &spec.PublishCA{Volume: "ca"}
	raw, err := frontCompose(f, nil, func(string) string { return "" }, "byo", hostAPI{}, "sha256:x", byoCA, false, false, false, nil, nil, FrontUID)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]struct {
			User        string            `yaml:"user"`
			CapDrop     []string          `yaml:"cap_drop"`
			CapAdd      []string          `yaml:"cap_add"`
			SecurityOpt []string          `yaml:"security_opt"`
			ReadOnly    bool              `yaml:"read_only"`
			Tmpfs       []string          `yaml:"tmpfs"`
			Sysctls     map[string]string `yaml:"sysctls"`
			Volumes     []string          `yaml:"volumes"`
			Privileged  bool              `yaml:"privileged"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	s := doc.Services[FrontService]
	if s.User != fmt.Sprintf("%d:%d", FrontUID, FrontUID) || strings.Join(s.CapDrop, ",") != "ALL" || len(s.CapAdd) != 0 || s.Privileged ||
		strings.Join(s.SecurityOpt, ",") != "no-new-privileges:true" || !s.ReadOnly || strings.Join(s.Tmpfs, ",") != "/tmp,/config" ||
		s.Sysctls["net.ipv4.ip_unprivileged_port_start"] != "0" || len(s.Sysctls) != 1 {
		t.Errorf("the front: %+v", s)
	}
	if strings.Join(s.Volumes, " ") != FrontDataVolume+":/data ca:"+caMount {
		t.Errorf("the front's mounts: %v", s.Volumes)
	}
	// Its writes: its volume, the published CA's, /tmp (the adapted configuration).
	if !strings.HasPrefix(frontJSON, "/tmp/") || !strings.HasPrefix(frontTLS, "/data/") || !strings.HasPrefix(localRoot, "/data/") {
		t.Errorf("a path the front writes is on its read-only root: %s %s %s", frontJSON, frontTLS, localRoot)
	}
	// The image's directories are the front's user's, so a volume made on them is.
	if want := fmt.Sprintf("chown -R %d:%d /data /config", FrontUID, FrontUID); !strings.Contains(string(front.Dockerfile), want) {
		t.Errorf("the front's image lacks %q", want)
	}
}

// tarNames lists the entries of a tar and their modes.
func tarNames(t *testing.T, raw []byte) map[string]os.FileMode {
	t.Helper()
	out := map[string]os.FileMode{}
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = os.FileMode(h.Mode)
	}
}

// Each start readies the front's volumes for its user: as that user with no capability, off any
// network, given the Caddyfile and a byo certificate on its standard input; root only for what
// that run says it cannot do, the front stopped first for its own volume; a failure fails the
// start, a refusal naming the project's fix and the operator's.
func TestEachStartReadiesTheFrontsVolumes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	docker := &fakeDocker{}
	in := &Installer{R: testRelease(t, "1.0.0"), O: Options{Dir: t.TempDir(), Docker: docker}}
	in.R.Spec.Front.PublishCA = &spec.PublishCA{Volume: "ca"}
	env := NewEnvFile()
	env.Set(FrontImageVar, "reg/notes-front@sha256:1")
	env.Set("COMPOSE_PROJECT_NAME", "p")
	env.Set("STOWAGE_TLS_MODE", "byo")
	m := &composeModel{}
	m.Volumes = map[string]struct {
		Name     string `json:"name"`
		External bool   `json:"external"`
	}{"ca": {Name: "site-ca"}}
	if err := os.WriteFile(in.path(Caddyfile), []byte("{\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stops := 0
	stop := func() error { stops++; return nil }
	var users, roots []Cmd
	userSays := []string{""}
	docker.frontUser = func(c Cmd) ([]byte, error) {
		users = append(users, c)
		say := userSays[0]
		if len(userSays) > 1 {
			userSays = userSays[1:]
		}
		return []byte(say), nil
	}
	rootSays := ""
	docker.frontPrep = func(c Cmd) ([]byte, error) { roots = append(roots, c); return []byte(rootSays), nil }
	reset := func(says ...string) { users, roots, stops, userSays = nil, nil, 0, says }
	// A byo install whose copy is gone is refused before anything runs.
	if err := in.prepareFront(ctx, env, m, stop); err == nil || !strings.Contains(err.Error(), "the byo certificate's copy is gone") || len(users) != 0 {
		t.Fatalf("no copy: %v", err)
	}
	if err := os.MkdirAll(in.path(TLSDir), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"cert.pem", "key.pem"} {
		if err := os.WriteFile(filepath.Join(in.path(TLSDir), f), []byte(f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A key gone is the copy gone too.
	_ = os.Remove(filepath.Join(in.path(TLSDir), "key.pem"))
	if err := in.prepareFront(ctx, env, m, stop); err == nil || !strings.Contains(err.Error(), "the byo certificate's copy is gone") || !strings.Contains(err.Error(), "key.pem") {
		t.Fatalf("no key: %v", err)
	}
	_ = os.WriteFile(filepath.Join(in.path(TLSDir), "key.pem"), []byte("key.pem"), 0o600)
	// All the user's already: one run as the front's user, no capability, the files on its input.
	reset("")
	if err := in.prepareFront(ctx, env, m, stop); err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || len(roots) != 0 || stops != 0 {
		t.Fatalf("%d user runs, %d root runs, %d stops", len(users), len(roots), stops)
	}
	got := strings.Join(users[0].Args, " ")
	if want := frontUserPrefix + "65532:65532 --cap-drop ALL --security-opt no-new-privileges:true --entrypoint sh --volume p_" + FrontDataVolume + ":/data --volume site-ca:" + caMount + " reg/notes-front@sha256:1 -euc "; !strings.HasPrefix(got, want) || strings.Contains(got, "--cap-add") {
		t.Errorf("the user's run: %s, want %q", got, want)
	}
	stdin, _ := io.ReadAll(users[0].Stdin)
	if names := tarNames(t, stdin); names["Caddyfile"] != 0o600 || names["tls/"] != 0o700 || names["tls/cert.pem"] != 0o600 || names["tls/key.pem"] != 0o600 || len(names) != 4 {
		t.Errorf("the files given: %v", names)
	}
	// The CA's directory root's (as Docker makes a volume): the user's run writes nothing, the
	// front stops, root gives it, the user's run then takes the files in.
	reset(frontPrepCA+" 0\n", "")
	if err := in.prepareFront(ctx, env, m, stop); err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || len(roots) != 1 || stops != 1 {
		t.Fatalf("CA: %d user runs, %d root runs, %d stops", len(users), len(roots), stops)
	}
	root := strings.Join(roots[0].Args, " ")
	if want := frontPrepPrefix + "--cap-add FOWNER --cap-add DAC_OVERRIDE --cap-add DAC_READ_SEARCH --security-opt no-new-privileges:true --entrypoint sh --volume p_" + FrontDataVolume + ":/data --volume site-ca:" + caMount + " reg/notes-front@sha256:1 -euc "; !strings.HasPrefix(root, want) ||
		!strings.Contains(root, "if false; then chown -hR") || !strings.Contains(root, "if true; then rm -f /stowage-ca/.ca.crt; chown 65532:65532 /stowage-ca; fi") {
		t.Errorf("the root run: %s", root)
	}
	// Another user's: refused before anything is stopped or changed, naming the operator's way
	// out and the project's.
	reset(frontPrepCA + " 2000\n")
	err := in.prepareFront(ctx, env, m, stop)
	var r Refused
	if !errors.As(err, &r) || len(roots) != 0 || stops != 0 ||
		r.Reason != "front.publish_ca.volume: the volume ca's directory belongs to uid 2000, not the front's (65532), which writes its CA there as its own user" ||
		!strings.Contains(r.Fix, "docker run --rm --user 0 --entrypoint chown -v site-ca:/v reg/notes-front@sha256:1 65532:65532 /v") || !strings.Contains(r.Fix, "data.volumes.ca.uid: 2000, the uid holding it") {
		t.Errorf("another user's: %v %+v, %d root runs, %d stops", err, r, len(roots), stops)
	}
	// The project's way is offered only where it declares no uid for the volume and the holder is
	// no front's own uid.
	for _, c := range []struct {
		holder  string
		declare bool
		offered bool
	}{{"2000", false, true}, {"2000", true, false}} {
		if c.declare {
			v := 3000
			in.R.Spec.Data.Volumes = map[string]spec.Volume{"ca": {UID: &v}}
		}
		reset(frontPrepCA + " " + c.holder + "\n")
		err := in.prepareFront(ctx, env, m, stop)
		var r Refused
		if !errors.As(err, &r) || strings.Contains(r.Fix, "the project declares") != c.offered {
			t.Errorf("%+v: %v %+v", c, err, r)
		}
		in.R.Spec.Data.Volumes = nil
	}
	// The CA's directory still another's after root: the start fails rather than go on without the
	// files.
	for _, again := range []string{"0", "2000"} {
		reset(frontPrepCA+" 0\n", frontPrepCA+" "+again+"\n")
		if err := in.prepareFront(ctx, env, m, stop); err == nil || !strings.Contains(err.Error(), "giving the front its volumes: the published CA's volume site-ca is still uid "+again+"'s") {
			t.Errorf("still the CA's (%s): %v", again, err)
		}
	}
	// A file of another user's in its volume: the front stops, root gives the volume, then the
	// files; one still there fails.
	reset(frontPrepForeign+"\n", "")
	if err := in.prepareFront(ctx, env, m, stop); err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || len(roots) != 1 || stops != 1 || !strings.Contains(strings.Join(roots[0].Args, " "), "if true; then chown -hR 65532:65532 /data; fi") {
		t.Errorf("foreign: %d user runs, %d root runs, %d stops", len(users), len(roots), stops)
	}
	reset(frontPrepForeign + "\n")
	if err := in.prepareFront(ctx, env, m, stop); err == nil || !strings.Contains(err.Error(), "a file of another user's is still in p_"+FrontDataVolume) {
		t.Errorf("still foreign: %v", err)
	}
	// A front running as another user stops after the first run, and the volume is judged again.
	docker.answer = func(a string) ([]byte, bool, error) {
		switch {
		case strings.HasPrefix(a, "ps -q --filter label=com.docker.compose.project=p --filter label=com.docker.compose.service="+FrontService):
			return []byte("abc\n"), true, nil
		case a == "container inspect --format {{.Config.User}} abc":
			return []byte("\n"), true, nil
		}
		return nil, false, nil
	}
	reset("")
	if err := in.prepareFront(ctx, env, m, stop); err != nil || stops != 1 || len(roots) != 0 || len(users) != 2 {
		t.Errorf("a front of root: %v, %d stops, %d user runs", err, stops, len(users))
	}
	reset("", frontPrepForeign+"\n", "")
	if err := in.prepareFront(ctx, env, m, stop); err != nil || stops != 1 || len(roots) != 1 || len(users) != 3 {
		t.Errorf("a front of root that wrote: %v, %d stops, %d root runs, %d user runs", err, stops, len(roots), len(users))
	}
	reset(frontPrepCA+" 0\n", frontPrepForeign+"\n", "")
	if err := in.prepareFront(ctx, env, m, stop); err != nil || len(users) != 3 || len(roots) != 1 || !strings.Contains(strings.Join(roots[0].Args, " "), "if true; then chown -hR") {
		t.Errorf("a front of root, the CA's to give: %v, %d user runs, %d root runs", err, len(users), len(roots))
	}
	// One gone between the listing and its inspection: stopped all the same.
	docker.answer = func(a string) ([]byte, bool, error) {
		switch {
		case strings.HasPrefix(a, "ps -q --filter label=com.docker.compose.project=p --filter label=com.docker.compose.service="+FrontService):
			return []byte("abc\n"), true, nil
		case strings.HasPrefix(a, "container inspect "):
			return nil, true, errors.New("no such container")
		}
		return nil, false, nil
	}
	reset("")
	if err := in.prepareFront(ctx, env, m, stop); err != nil || stops != 1 {
		t.Errorf("a front gone: %v, %d stops", err, stops)
	}
	docker.answer = nil
	docker.frontPrep = func(Cmd) ([]byte, error) { return nil, errors.New("no such image") }
	reset(frontPrepForeign + "\n")
	if err := in.prepareFront(ctx, env, m, stop); err == nil || !strings.Contains(err.Error(), "giving the front its volumes: no such image") {
		t.Errorf("a failure: %v", err)
	}
	docker.frontUser = func(Cmd) ([]byte, error) { return nil, errors.New("no such image") }
	if err := in.prepareFront(ctx, env, m, stop); err == nil || !strings.Contains(err.Error(), "giving the front its files: no such image") {
		t.Errorf("the user's failure: %v", err)
	}
	// The published CA's volume of a uid the release gives: the front runs as that user.
	uid := 1000
	in.R.Spec.Data.Volumes = map[string]spec.Volume{"ca": {UID: &uid}}
	docker.frontUser = func(c Cmd) ([]byte, error) { users = append(users, c); return nil, nil }
	docker.frontPrep = func(c Cmd) ([]byte, error) { roots = append(roots, c); return nil, nil }
	reset()
	if err := in.prepareFront(ctx, env, m, stop); err != nil || !strings.Contains(strings.Join(users[0].Args, " "), " --user 1000:1000 ") {
		t.Errorf("a volume of uid 1000: %v", err)
	}
	// No byo certificate: none given.
	in.R.Spec.Data.Volumes = nil
	env.Set("STOWAGE_TLS_MODE", "internal")
	reset()
	if err := in.prepareFront(ctx, env, m, stop); err != nil {
		t.Fatal(err)
	}
	stdin, _ = io.ReadAll(users[0].Stdin)
	if names := tarNames(t, stdin); len(names) != 1 || names["Caddyfile"] != 0o600 {
		t.Errorf("internal: %v", names)
	}
	// Without a front, nothing runs.
	in.R.Spec.Front = spec.Front{}
	reset()
	if err := in.prepareFront(ctx, env, m, stop); err != nil || len(users)+len(roots) != 0 {
		t.Errorf("no front: %v %d", err, len(users)+len(roots))
	}
}

// The scripts themselves, run by a shell on directories of this test's. As the front's user (this
// test's own, or another uid): the CA's directory of another uid said with that uid, a file of
// another user's said before anything is written; else the files given are taken in, a link left
// where they go replaced, never written through, a copy no longer given removed. As root (chown
// records, as a test runs as no root): the volume's owner and the CA's directory changed as
// decided, the front's stale temporary copy removed with the latter.
func TestTheFrontPrepScriptsJudgeWhatTheyFind(t *testing.T) {
	t.Parallel()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	paths := func(script, dir string) string {
		// The container's paths, longest first, through marks no path holds.
		for i, from := range []string{frontCaddyfile, frontTLS, caMount, "/data"} {
			script = strings.ReplaceAll(script, from, fmt.Sprintf("@%d@", i))
		}
		for i, to := range []string{"data/stowage-Caddyfile", "data/stowage-tls", "ca", "data"} {
			script = strings.ReplaceAll(script, fmt.Sprintf("@%d@", i), filepath.Join(dir, to))
		}
		return script
	}
	me := os.Getuid()
	files := func(byo bool) []byte {
		in := &Installer{O: Options{Dir: t.TempDir()}}
		_ = os.WriteFile(in.path(Caddyfile), []byte("{\n}\n"), 0o600)
		_ = os.MkdirAll(in.path(TLSDir), 0o700)
		for _, f := range []string{"cert.pem", "key.pem"} {
			_ = os.WriteFile(filepath.Join(in.path(TLSDir), f), []byte(f), 0o600)
		}
		raw, err := in.frontFiles(byo)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	user := func(uid int, byo bool, plant ...string) (dir, out string, err error) {
		dir = t.TempDir()
		for _, d := range []string{"data/stowage-tls", "ca"} {
			if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		_ = os.WriteFile(filepath.Join(dir, "data", "stowage-tls", "old.pem"), []byte("x"), 0o600)
		_ = os.WriteFile(filepath.Join(dir, "ca", "project.txt"), []byte("the project's"), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "ca", ".ca.crt"), nil, 0o644) // an interrupted front's copy
		for _, at := range plant {
			_ = os.RemoveAll(filepath.Join(dir, at))
			if err := os.Symlink(filepath.Join(dir, "ca", "project.txt"), filepath.Join(dir, at)); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command(sh, "-euc", paths(frontUserShell(uid), dir))
		cmd.Stdin = bytes.NewReader(files(byo))
		raw, err := cmd.CombinedOutput()
		return dir, string(raw), err
	}
	// Another uid: the test's own files are another user's, said before anything is written.
	dir, out, err := user(me+1, true)
	if err != nil || out != fmt.Sprintf("%s %d\n%s\n", frontPrepCA, me, frontPrepForeign) {
		t.Errorf("another uid: %v %q", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "stowage-Caddyfile")); err == nil {
		t.Error("another uid's run wrote")
	}
	// The user's own: taken in, the old copy gone, nothing said.
	dir, out, err = user(me, true)
	if err != nil || out != "" {
		t.Fatalf("the user's own: %v %q", err, out)
	}
	for f, mode := range map[string]os.FileMode{"data/stowage-Caddyfile": 0o600, "data/stowage-tls/cert.pem": 0o600, "data/stowage-tls/key.pem": 0o600, "data/stowage-tls": 0o700 | os.ModeDir} {
		if st, err := os.Lstat(filepath.Join(dir, f)); err != nil || st.Mode() != mode {
			t.Errorf("%s: %v %v", f, st, err)
		}
	}
	// The front's own user's temporary copy (one a running front may be making) is left.
	if _, err := os.Stat(filepath.Join(dir, "ca", ".ca.crt")); err != nil {
		t.Errorf("the front's own copy removed: %v", err)
	}
	for _, f := range []string{"data/stowage-tls/old.pem", "data/.stowage-in"} {
		if _, err := os.Lstat(filepath.Join(dir, f)); err == nil {
			t.Errorf("%s left", f)
		}
	}
	dir, _, _ = user(me, false)
	if _, err := os.Lstat(filepath.Join(dir, "data", "stowage-tls")); err == nil {
		t.Error("the byo copy left in another mode")
	}
	// A link where the files go is replaced, never written through.
	for _, at := range []string{"data/stowage-Caddyfile", "data/stowage-tls"} {
		dir, out, err := user(me, true, at)
		if err != nil {
			t.Fatalf("a link at %s: %v %s", at, err, out)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "ca", "project.txt")); string(b) != "the project's" {
			t.Errorf("a link at %s led the copy to the project's file: %q", at, b)
		}
		if st, err := os.Lstat(filepath.Join(dir, at)); err != nil || st.Mode()&os.ModeSymlink != 0 {
			t.Errorf("the link at %s stays: %v %v", at, st, err)
		}
	}

	// As root: what prepareFront decided, no more.
	for _, c := range []struct{ data, ca bool }{{true, false}, {false, true}, {true, true}} {
		dir := t.TempDir()
		bin := filepath.Join(dir, "bin")
		for _, d := range []string{"bin", "data", "ca"} {
			if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(bin, "chown"), []byte("#!/bin/sh\necho chown \"$@\" >>"+filepath.Join(dir, "log")+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "ca", ".ca.crt"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(sh, "-euc", paths(frontRootShell(FrontUID, c.data, c.ca), dir))
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%+v: %v %s", c, err, out)
		}
		log, _ := os.ReadFile(filepath.Join(dir, "log"))
		_, staleErr := os.Stat(filepath.Join(dir, "ca", ".ca.crt"))
		if strings.Contains(string(log), "chown -hR 65532:65532 "+filepath.Join(dir, "data")+"\n") != c.data ||
			strings.Contains(string(log), "chown 65532:65532 "+filepath.Join(dir, "ca")+"\n") != c.ca || (staleErr != nil) != c.ca {
			t.Errorf("%+v: %s (stale copy: %v)", c, log, staleErr)
		}
	}
}

// A start makes the stack's containers first only while a volume of the front's is not there yet:
// a re-run whose volumes the engine holds readies them as they are, and recreates nothing early.
func TestTheFrontsVolumesHeldNeedNoEarlyCreate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	creates := func() int {
		n := 0
		for _, c := range e.docker.calls {
			if strings.HasSuffix(strings.Join(c.Args, " "), " up --no-start --remove-orphans") {
				n++
			}
		}
		return n
	}
	if creates() != 1 || len(e.docker.ran(frontUserPrefix)) != 1 {
		t.Fatalf("a first install: %d creates, %d preparations", creates(), len(e.docker.ran(frontUserPrefix)))
	}
	e.docker.calls = nil
	e.docker.volumes = get(e.readEnv(), "COMPOSE_PROJECT_NAME") + "_" + FrontDataVolume + "\n"
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if creates() != 0 || len(e.docker.ran(frontUserPrefix)) != 1 {
		t.Errorf("a re-run: %d creates, %d preparations", creates(), len(e.docker.ran(frontUserPrefix)))
	}
}

// The stack files are written of their mode whatever the umask or the mode an earlier write left:
// the Caddyfile 0600 (the logins' hashes; the front takes it on its input), the others 0644, and
// the client CA's directory the front mounts 0755, as it reads it as a user of its own.
func TestTheFrontsFilesAreReadableByItsUser(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(e.dir, Caddyfile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.dir, ClientCADir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(e.dir, ClientCADir), 0o700); err != nil {
		t.Fatal(err)
	}
	in := e.installer(nil)
	if err := in.writeClientCA([]byte("x"), "internal"); err != nil {
		t.Fatal(err)
	}
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{Caddyfile: 0o600, ClientCADir: 0o755, "compose.yaml": 0o644} {
		if st, err := os.Stat(filepath.Join(e.dir, name)); err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %o", name, st, err, want)
		}
	}
}

// The OS bundle's ruleset, with a role's forwarding as a start writes it, loads in nft (checked,
// in a network namespace of this test's own): skipped where nft or an unprivileged namespace is
// not to be had.
func TestTheBundlesFirewallLoadsInNft(t *testing.T) {
	t.Parallel()
	nft, err := exec.LookPath("nft")
	if err != nil {
		t.Skip("no nft")
	}
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		t.Skip("no unshare")
	}
	if out, err := exec.Command(unshare, "-rn", nft, "list", "ruleset").CombinedOutput(); err != nil {
		t.Skipf("no unprivileged network namespace: %v %s", err, out)
	}
	dir := t.TempDir()
	if err := kairos.WriteContext(dir, kairos.Params{Name: "notes", Ctl: "notes-stow"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "files", FirewallConf))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	h := &spec.Host{Interfaces: map[string]spec.HostInterface{"uplink": {Primary: true}, "vpn0": {Kind: spec.InterfaceTUN, Device: "ovl-0-0", Forward: []string{"uplink"}}}}
	fwd := append(bridgeNft([]string{"appnet0"}), forwardNft(h, func(r string) string { return h.Interfaces[r].DeviceName(r) })...)
	if err := os.MkdirAll(filepath.Join(root, "run", "forward"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "run", "forward", "notes-forward.nft"), fwd, 0o644); err != nil {
		t.Fatal(err)
	}
	conf := strings.NewReplacer(PortRules, filepath.Join(root, "run"), "/etc/nftables.d", filepath.Join(root, "etc"), VMDir("notes"), filepath.Join(root, "usr")).Replace(string(raw))
	file := filepath.Join(root, "nftables.conf")
	if err := os.WriteFile(file, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(unshare, "-rn", "sh", "-c", nft+" -f "+file+" && "+nft+" list chain inet notes forward").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, conf)
	}
	for _, want := range []string{`iifname "ovl-0-0" oifname "uplink" accept`, `iifname "br-*" accept`, `oifname "appnet0" accept`, "drop"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the loaded chain lacks %q:\n%s", want, out)
		}
	}
}

// A bridge the stack's own network names (com.docker.network.bridge.name) is left to Docker's
// chains by the VM's forward chain, as docker0 and br-* are; another network's (external, not a
// bridge, already matched) adds nothing.
func TestTheStacksNamedBridgesAreLeftToDocker(t *testing.T) {
	t.Parallel()
	m := &composeModel{Networks: map[string]composeNetwork{
		"lan":   {DriverOpts: map[string]string{bridgeNameOpt: "appnet0"}},
		"lan2":  {Driver: "bridge", DriverOpts: map[string]string{bridgeNameOpt: "appnet0"}},
		"dflt":  {DriverOpts: map[string]string{bridgeNameOpt: "br-own"}},
		"ext":   {External: true, DriverOpts: map[string]string{bridgeNameOpt: "ext0"}},
		"ipvl":  {Driver: "ipvlan", DriverOpts: map[string]string{bridgeNameOpt: "ipv0"}},
		"plain": {},
	}}
	got := stackBridges(m)
	if strings.Join(got, ",") != "appnet0" {
		t.Fatalf("bridges %v", got)
	}
	ctx := context.Background()
	in := &Installer{R: testRelease(t, "1.0.0"), O: Options{Dir: t.TempDir(), Firewall: filepath.Join(t.TempDir(), "nftables.d"), Host: &nftHost{}, Log: io.Discard}}
	env := NewEnvFile()
	env.Set("STOWAGE_TLS_MODE", Loopback)
	if err := in.openPorts(ctx, env, hostNet{bridges: got}); err != nil {
		t.Fatal(err)
	}
	// On the persistent partition: the boot's ruleset has them before the install's start.
	b, _ := os.ReadFile(filepath.Join(in.O.Dir, ForwardRules, "notes-bridges.nft"))
	if !strings.Contains(string(b), "\niifname \"appnet0\" accept\noifname \"appnet0\" accept\n") {
		t.Errorf("the forward chain's rules:\n%s", b)
	}
}

// The external networks the stack joins give their bridges' names as the engine holds them: a
// bridge's named otherwise than docker0 or br-*, nothing for another driver.
func TestTheExternalNetworksBridgesAreAskedOfTheEngine(t *testing.T) {
	t.Parallel()
	docker := &fakeDocker{}
	calls := 0
	docker.answer = func(a string) ([]byte, bool, error) {
		if !strings.HasPrefix(a, "network inspect --format ") {
			return nil, false, nil
		}
		calls++
		known := map[string]string{"shared": "bridge lan0", "site_default": "bridge br-1234", "mesh": "overlay mesh0"}
		var out []string
		for _, n := range strings.Fields(a)[7:] { // past the format's four fields
			line, ok := known[n]
			if !ok {
				return nil, true, errors.New("no such network: " + n)
			}
			out = append(out, line)
		}
		return []byte(strings.Join(out, "\n") + "\n"), true, nil
	}
	in := &Installer{R: testRelease(t, "1.0.0"), O: Options{Dir: t.TempDir(), Docker: docker}}
	m := &composeModel{Networks: map[string]composeNetwork{
		"shared": {External: true}, "dflt": {External: true, Name: "site_default"}, "mesh": {External: true}, "own": {DriverOpts: map[string]string{bridgeNameOpt: "own0"}},
	}}
	got, err := in.externalBridges(context.Background(), m)
	if err != nil || strings.Join(got, ",") != "lan0" || calls != 1 {
		t.Errorf("%v %v, %d inspections", got, err, calls)
	}
	// The VM's forward chain takes both, its own networks' and the external ones'; a Docker host none.
	if got, _ := in.forwardBridges(context.Background(), m); len(got) != 0 {
		t.Errorf("a Docker host: %v", got)
	}
	in.O.Firewall = t.TempDir()
	if got, err := in.forwardBridges(context.Background(), m); err != nil || strings.Join(got, ",") != "lan0,own0" {
		t.Errorf("the VM: %v %v", got, err)
	}
	// A bridge named as one of the host's own devices is refused: its accept would forward through it.
	in.R.Spec.Host = &spec.Host{Interfaces: map[string]spec.HostInterface{"uplink": {Primary: true}, "vpn0": {Kind: spec.InterfaceTUN, Device: "ovl-0-0"}}}
	for _, b := range []string{"uplink", "ovl-0-0", "lo"} {
		m.Networks["own"] = composeNetwork{DriverOpts: map[string]string{bridgeNameOpt: b}}
		if _, err := in.forwardBridges(context.Background(), m); err == nil || !strings.Contains(err.Error(), "names its bridge "+b) {
			t.Errorf("a bridge named %s: %v", b, err)
		}
	}
	m.Networks["own"] = composeNetwork{DriverOpts: map[string]string{bridgeNameOpt: "own0"}}
	m.Networks["gone"] = composeNetwork{External: true}
	if _, err := in.externalBridges(context.Background(), m); err == nil || !strings.Contains(err.Error(), "the external networks site_default, gone, mesh, shared: no such network: gone") {
		t.Errorf("an unknown network: %v", err)
	}
}

// A front stopped for its volume stays stopped when the start then fails, as a failed start
// leaves its stack (its error naming the fix); the step runs before the stack's containers are
// made when its volumes are there.
func TestAFrontStoppedForAFailedPrepStaysStopped(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	e.docker.volumes = get(e.readEnv(), "COMPOSE_PROJECT_NAME") + "_" + FrontDataVolume + "\n"
	e.docker.frontUser = func(Cmd) ([]byte, error) { return []byte(frontPrepForeign + "\n"), nil }
	e.docker.frontPrep = func(Cmd) ([]byte, error) { return nil, errors.New("no such image") }
	e.docker.calls = nil
	if err := e.install(nil); err == nil || !strings.Contains(err.Error(), "giving the front its volumes: no such image") {
		t.Fatalf("%v", err)
	}
	var order []string
	for _, c := range e.docker.calls {
		switch a := strings.Join(c.Args, " "); {
		case strings.HasSuffix(a, " stop "+FrontService):
			order = append(order, "stop")
		case strings.HasSuffix(a, " start "+FrontService):
			order = append(order, "start")
		case strings.HasSuffix(a, " up --no-start --remove-orphans"):
			order = append(order, "create")
		case strings.HasPrefix(a, frontPrepPrefix):
			order = append(order, "root")
		}
	}
	if strings.Join(order, " ") != "stop root" {
		t.Errorf("order %v", order)
	}
}

// A stack restored from what an upgrade kept (a rollback) keeps the Caddyfile 0600, whatever mode
// the kept copy has.
func TestARestoredCaddyfileStaysTheInstallersAlone(t *testing.T) {
	t.Parallel()
	in := &Installer{R: testRelease(t, "1.0.0"), O: Options{Dir: t.TempDir()}}
	kept := t.TempDir()
	for _, f := range stackFiles {
		if err := os.WriteFile(filepath.Join(kept, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := in.restoreStack(kept, false); err != nil {
		t.Fatal(err)
	}
	for _, f := range stackFiles {
		if st, err := os.Stat(in.path(f)); err != nil || st.Mode().Perm() != stackFileMode(f) {
			t.Errorf("%s: %v %v", f, st, err)
		}
	}
	if stackFileMode(Caddyfile) != 0o600 {
		t.Error("the Caddyfile is not the installer's alone")
	}
}

// The user's run writes nothing when the published CA's directory is another uid's: it says so
// and stops, so a refusal leaves the volume as it was.
func TestTheFrontUserRunWritesNothingBeforeTheCAIsJudged(t *testing.T) {
	t.Parallel()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	for _, d := range []string{"bin", "data", "ca"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// stat says the CA's directory is uid 2000's; find, as the test's own files are the user's.
	if err := os.WriteFile(filepath.Join(dir, "bin", "stat"), []byte("#!/bin/sh\necho 2000\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := frontUserShell(os.Getuid())
	for i, from := range []string{frontCaddyfile, frontTLS, caMount, "/data"} {
		script = strings.ReplaceAll(script, from, fmt.Sprintf("@%d@", i))
	}
	for i, to := range []string{"data/stowage-Caddyfile", "data/stowage-tls", "ca", "data"} {
		script = strings.ReplaceAll(script, fmt.Sprintf("@%d@", i), filepath.Join(dir, to))
	}
	cmd := exec.Command(sh, "-euc", script)
	cmd.Env = []string{"PATH=" + filepath.Join(dir, "bin") + ":/usr/bin:/bin"}
	cmd.Stdin = strings.NewReader("not read")
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != frontPrepCA+" 2000\n" {
		t.Fatalf("%v %q", err, out)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "data")); len(entries) != 0 {
		t.Errorf("the run wrote: %v", entries)
	}
}

// A volume awaiting its uid (VolumesPending) is given it before the front's step, which then
// judges what that left: never the front's step first, whose change of the CA's directory would
// keep the volume helper from giving the volume's files.
func TestAVolumeAwaitingItsUIDIsGivenBeforeTheFrontsStep(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.to("1.0.0", func(s *spec.Spec) { s.Front.PublishCA = &spec.PublishCA{Volume: "uploads"} })
	e.docker.model = `{"services": {"web": {"volumes": [{"type": "volume", "source": "uploads", "target": "/data/uploads"}]}},
	"volumes": {"uploads": {"name": "p_uploads"}, "stowage-front-data": {"name": "p_front"}}}`
	if err := e.install(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	e.docker.volumes = "p_front\np_uploads\n"
	if err := os.WriteFile(filepath.Join(e.dir, VolumesPending), []byte("p_uploads\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.docker.calls = nil
	if err := e.install(nil); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	var order []string
	for _, c := range e.docker.calls {
		switch a := strings.Join(c.Args, " "); {
		case strings.HasSuffix(a, " up --no-start --remove-orphans"):
			order = append(order, "create")
		case strings.HasPrefix(a, frontUserPrefix):
			order = append(order, "front")
		case strings.HasPrefix(a, "run --rm --pull never --network none --user 0 "):
			order = append(order, "own")
		}
	}
	if strings.Join(order, " ") != "create own front" {
		t.Errorf("order %v", order)
	}
}

// Which of the front's volumes the engine lacks, each asked alone: one it says is not there is
// missing, another failure of the engine fails the start.
func TestTheFrontsMissingVolumesAreAskedOfTheEngine(t *testing.T) {
	t.Parallel()
	docker := &fakeDocker{volumes: "p_" + FrontDataVolume + "\n"}
	in := &Installer{R: testRelease(t, "1.0.0"), O: Options{Dir: t.TempDir(), Docker: docker}}
	in.R.Spec.Front.PublishCA = &spec.PublishCA{Volume: "ca"}
	env := NewEnvFile()
	env.Set("COMPOSE_PROJECT_NAME", "p")
	m := &composeModel{}
	if data, ca, err := in.frontVolumesMissing(context.Background(), env, m); err != nil || data || !ca {
		t.Errorf("the CA's missing: %v %v %v", data, ca, err)
	}
	docker.volumes += "p_ca\n"
	if data, ca, err := in.frontVolumesMissing(context.Background(), env, m); err != nil || data || ca {
		t.Errorf("both there: %v %v %v", data, ca, err)
	}
	docker.answer = func(a string) ([]byte, bool, error) {
		if strings.HasPrefix(a, "volume inspect ") {
			return nil, true, errors.New("Cannot connect to the Docker daemon")
		}
		return nil, false, nil
	}
	if _, _, err := in.frontVolumesMissing(context.Background(), env, m); err == nil || !strings.Contains(err.Error(), "the front's volume p_"+FrontDataVolume) {
		t.Errorf("the engine failing: %v", err)
	}
}
