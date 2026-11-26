package install

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/norvexlabs/stowage/internal/providers"
)

// setTLSMode records mode as the install's TLS mode.
func setTLSMode(t *testing.T, dir, mode string) {
	t.Helper()
	env, err := ReadEnvFile(filepath.Join(dir, envName))
	if err != nil {
		t.Fatal(err)
	}
	env.Set("STOWAGE_TLS_MODE", mode)
	if err := env.WriteFile(filepath.Join(dir, envName)); err != nil {
		t.Fatal(err)
	}
}

// frontBackupVM is backupVM configured, in the TLS mode mode, its front's volume holding state:
// its front's volume, and the object of the one backup taken.
func frontBackupVM(t *testing.T, mode, state string) (*env, *Hostd, *fakeRclone, *age.X25519Identity, string, string) {
	t.Helper()
	e, d, f, key := backupVM(t)
	configure(t, d, validConfig(key))
	e.docker.answer = func(a string) ([]byte, bool, error) {
		return []byte("api\nweb\npostgres\n"), strings.HasSuffix(a, "config --services"), nil
	}
	setTLSMode(t, e.dir, mode)
	vol := stackVolumeOf(t, d, FrontDataVolume)
	f.volumes, f.staged = map[string][]byte{vol: []byte(state)}, map[string][]byte{}
	h := d.Handler()
	call(t, h, "POST", "/v1/backup", BackupRequest{}, nil)
	j := waitJob(t, h)
	if j.State != "done" {
		t.Fatalf("%+v", j)
	}
	return e, d, f, key, vol, j.Target
}

// A backup of an install in an ACME mode takes the front's ACME state beside the volumes, before
// its last object, its digest recorded; in another mode it takes none.
func TestABackupTakesTheFrontsCertificatesInAnACMEMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"acme", "dns-cloudflare"} {
		_, d, f, key, _, obj := frontBackupVM(t, mode, "the front's ACME state")
		at := "store:backups/notes/" + frontObject(obj)
		if at != "store:backups/notes/"+strings.TrimSuffix(obj, ".pgdump.age")+".stowage-front-data.tar.age" {
			t.Fatalf("%s: named %s", mode, at)
		}
		r, err := age.Decrypt(bytes.NewReader(f.objects[at]), key)
		if err != nil {
			t.Fatalf("%s: %s: %v", mode, at, err)
		}
		if plain, _ := io.ReadAll(r); string(plain) != "the front's ACME state" {
			t.Errorf("%s: archived %q", mode, plain)
		}
		rec, err := readDigests(d.O.Dir)
		if sum := sha256.Sum256(f.objects[at]); err != nil || rec[at].SHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("%s: the front's archive's digest is not recorded: %v %+v", mode, err, rec)
		}
		// Uploaded before the dump, which names a whole backup.
		var order []string
		for _, c := range f.calls() {
			if c[0] == "rcat" {
				order = append(order, c[len(c)-1])
			}
		}
		if i, j := slices.Index(order, at), slices.Index(order, "store:backups/notes/"+obj); i < 0 || j < i {
			t.Errorf("%s: uploaded %v", mode, order)
		}
		// Its pruning goes with its backup's: the archives' filter holds it.
		if !matches(ownArchives("notes", "full"), frontObject(obj)) {
			t.Errorf("the archives' filter misses %s", frontObject(obj))
		}
	}
	for _, mode := range []string{"internal", "byo"} {
		_, _, f, _, _, obj := frontBackupVM(t, mode, "the front's local CA")
		if _, ok := f.objects["store:backups/notes/"+frontObject(obj)]; ok {
			t.Errorf("%s: the front's state was taken", mode)
		}
	}
}

// A restore into an install made again puts the front's ACME state back before anything stops,
// the front's container removed first; one into a live install leaves its front's as they are;
// a backup without the archive restores, putting nothing back.
func TestAMachineMadeAgainGetsTheFrontsCertificatesBack(t *testing.T) {
	t.Parallel()
	e, d, f, key, vol, obj := frontBackupVM(t, "acme", "at the backup")
	h := d.Handler()
	req := RestoreRequest{Object: obj, Identity: key.String(), Confirm: &Version{"1.0.0", 1}}
	restore := func() JobStatus {
		t.Helper()
		call(t, h, "POST", "/v1/restore", req, nil)
		return waitJob(t, h)
	}
	// A live install: its front's own, newer, stay.
	f.volumes[vol] = []byte("the live front's")
	if j := restore(); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if string(f.volumes[vol]) != "the live front's" || len(f.frontPut) != 0 {
		t.Errorf("a live front's state replaced: %q %v", f.volumes[vol], f.frontPut)
	}
	// Made again, awaiting its data: put back, before the services stop.
	awaits := func() {
		if err := os.WriteFile(filepath.Join(e.dir, restoreAwaitedFile), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	awaits()
	f.volumes[vol] = nil
	n := len(e.docker.ran("compose"))
	j := restore()
	if j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if string(f.volumes[vol]) != "at the backup" || !slices.Equal(f.frontPut, []string{vol}) {
		t.Errorf("the front's state %q, put into %v", f.volumes[vol], f.frontPut)
	}
	log := strings.Join(j.Log, "\n")
	if put, stop := strings.Index(log, "putting the front's ACME account and certificates back"), strings.Index(log, "stopping notes"); put < 0 || stop < put {
		t.Errorf("log:\n%s", log)
	}
	var rm, up int = -1, -1
	for i, c := range e.docker.ran("compose")[n:] {
		a := strings.Join(c.Args, " ")
		switch {
		case strings.HasSuffix(a, " rm --stop --force "+FrontService) && rm < 0:
			rm = i
		case strings.Contains(a, " up --detach --wait --remove-orphans "):
			up = i
			if strings.Contains(a, "--scale") {
				t.Errorf("the restore that ended the wait brought the stack up without its front: %s", a)
			}
		}
	}
	if rm < 0 || up < rm {
		t.Errorf("the front's container removed at %d, the stack up at %d", rm, up)
	}
	// A backup taken without it (another mode, an older Stowage) restores, putting nothing back.
	awaits()
	delete(f.objects, "store:backups/notes/"+frontObject(obj))
	f.volumes[vol], f.frontPut = nil, nil
	if j := restore(); j.State != "done" || !strings.Contains(strings.Join(j.Log, "\n"), "holds no archive of the front's certificates") {
		t.Fatalf("%+v", j)
	}
	if len(f.frontPut) != 0 {
		t.Errorf("put back %v", f.frontPut)
	}
}

// The front's archive is checked as every object: one changed since the backup recorded it is
// refused before anything stops, and --unrecorded names what it vouches for in it.
func TestTheFrontsArchiveIsCheckedAsEveryObject(t *testing.T) {
	t.Parallel()
	e, d, f, key, _, obj := frontBackupVM(t, "acme", "at the backup")
	if err := os.WriteFile(filepath.Join(e.dir, restoreAwaitedFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var planted bytes.Buffer
	w, _ := age.Encrypt(&planted, key.Recipient())
	w.Write([]byte("planted"))
	w.Close()
	f.objects["store:backups/notes/"+frontObject(obj)] = planted.Bytes()
	h := d.Handler()
	n := len(e.docker.ran("compose"))
	call(t, h, "POST", "/v1/restore", RestoreRequest{Object: obj, Identity: key.String(), Confirm: &Version{"1.0.0", 1}}, nil)
	if j := waitJob(t, h); j.State != "failed" || !strings.Contains(j.Error, "is not the object this install's backup uploaded") || !strings.Contains(j.Error, "nothing restored") {
		t.Fatalf("%+v", j)
	}
	if calls := composeCalls(e, n); slices.Contains(calls, "stop") || len(f.frontPut) != 0 {
		t.Errorf("ran %v, put back %v", calls, f.frontPut)
	}
	pg, err := d.pgBackup()
	if err != nil {
		t.Fatal(err)
	}
	if got := restoreTrust(pg, obj, "notes")(frontObject(obj)); !strings.Contains(got, "into the front's volume") {
		t.Errorf("the front's archive vouched for as %q", got)
	}
}

// The front's scripts take Caddy's ACME account and certificates alone, and put back each
// directory the archive holds in place of the volume's, the rest of the volume kept; an archive of
// nothing puts nothing back. Run by the host's shell and tar on directories standing for /v.
func TestTheFrontsScripts(t *testing.T) {
	t.Parallel()
	run := func(script, v string, stdin []byte) []byte {
		t.Helper()
		cmd := exec.Command("sh", "-euc", strings.ReplaceAll(script, "/v", v))
		cmd.Stdin = bytes.NewReader(stdin)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			t.Fatalf("%v: %s", err, errb.String())
		}
		return out.Bytes()
	}
	write := func(root string, files ...string) {
		t.Helper()
		for _, f := range files {
			p := filepath.Join(root, f)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(f), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	held := func(root string) []string {
		t.Helper()
		var out []string
		filepath.WalkDir(root, func(p string, de os.DirEntry, err error) error {
			if err == nil && !de.IsDir() {
				rel, _ := filepath.Rel(root, p)
				out = append(out, rel)
			}
			return err
		})
		return out
	}
	src, dst := t.TempDir(), t.TempDir()
	write(src, "caddy/acme/acct.json", "caddy/certificates/le/a.crt", "caddy/locks/l", "caddy/ocsp/o", "caddy/pki/authorities/local/root.key", "stowage-Caddyfile")
	archive := run(frontArchiveScript, src, nil)
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(h.Name, "./caddy/acme") && !strings.HasPrefix(h.Name, "./caddy/certificates") {
			t.Errorf("the archive holds %s", h.Name)
		}
	}
	write(dst, "caddy/certificates/le/old.crt", "caddy/pki/authorities/local/root.key", "caddy/locks/mine", "stowage-Caddyfile")
	run(frontRestoreScript, dst, archive)
	if got, want := held(dst), []string{"caddy/acme/acct.json", "caddy/certificates/le/a.crt", "caddy/locks/mine", "caddy/pki/authorities/local/root.key", "stowage-Caddyfile"}; !slices.Equal(got, want) {
		t.Errorf("put back %v, want %v", got, want)
	}
	if _, err := os.Lstat(filepath.Join(dst, frontStage)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staging is left: %v", err)
	}
	// Nothing to take: an archive of nothing, which puts nothing back; caddy/ is made when absent.
	empty, fresh := t.TempDir(), t.TempDir()
	write(empty, "stowage-Caddyfile")
	none := run(frontArchiveScript, empty, nil)
	if !bytes.Equal(none, make([]byte, 1024)) {
		t.Errorf("archived %q", none)
	}
	write(fresh, "caddy/certificates/le/old.crt")
	run(frontRestoreScript, fresh, none)
	if got := held(fresh); !slices.Equal(got, []string{"caddy/certificates/le/old.crt"}) {
		t.Errorf("an archive of nothing left %v", got)
	}
	bare := t.TempDir()
	run(frontRestoreScript, bare, archive)
	if fi, err := os.Stat(filepath.Join(bare, "caddy")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("caddy/ made as %v %v", fi, err)
	}
	if got := held(bare); !slices.Equal(got, []string{"caddy/acme/acct.json", "caddy/certificates/le/a.crt"}) {
		t.Errorf("put into a volume without caddy/: %v", got)
	}
}

// While an install made again awaits its data, its front in an ACME mode waits too (scaled to
// 0); in another mode, or once the wait is over, the stack starts whole. Provision ending the wait
// starts the front.
func TestTheFrontWaitsForTheDataInAnACMEMode(t *testing.T) {
	t.Parallel()
	scaled := func(e *env, from int) (ups int, held bool) {
		for _, c := range e.docker.ran("compose")[from:] {
			if a := strings.Join(c.Args, " "); strings.Contains(a, " up --detach --wait --remove-orphans ") {
				ups++
				held = strings.HasSuffix(a, " --scale "+FrontService+"=0")
			}
		}
		return ups, held
	}
	for _, c := range []struct {
		mode   string
		awaits bool
		held   bool
	}{{"acme", true, true}, {"dns-cloudflare", true, true}, {"acme", false, false}, {"internal", true, false}} {
		e := newEnv(t)
		if err := e.install(func(o *Options) { tlsDomains(o); o.TLS = "internal" }); err != nil {
			t.Fatal(err)
		}
		setTLSMode(t, e.dir, c.mode)
		if c.awaits {
			if err := os.WriteFile(filepath.Join(e.dir, restoreAwaitedFile), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		n := len(e.docker.ran("compose"))
		var started bytes.Buffer
		if err := e.installer(func(o *Options) { o.Out = &started }).up(context.Background(), e.readEnv()); err != nil {
			t.Fatal(err)
		}
		// A held front's start names no URL it does not serve.
		if strings.Contains(started.String(), "the front waits for the restore") != c.held || c.held && strings.Contains(started.String(), "https://") {
			t.Errorf("%s, awaiting %v: started\n%s", c.mode, c.awaits, started.String())
		}
		if ups, held := scaled(e, n); ups != 1 || held != c.held {
			t.Errorf("%s, awaiting %v: %d starts, the front held %v", c.mode, c.awaits, ups, held)
		}
		// status says so, in its text and its JSON, the host daemon's too.
		var out bytes.Buffer
		if err := e.installer(func(o *Options) { o.Out = &out }).Status(context.Background()); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "the front waits for the restore") != c.held {
			t.Errorf("%s, awaiting %v: status\n%s", c.mode, c.awaits, out.String())
		}
		if st, _ := statusOf(t, e, nil); st.FrontWaits != c.held {
			t.Errorf("%s, awaiting %v: front_waits %v", c.mode, c.awaits, st.FrontWaits)
		}
		var vst Status
		if code := call(t, daemon(e, &fakeHost{}).Handler(), "GET", "/v1/status", nil, &vst); code != 200 || vst.FrontWaits != c.held {
			t.Errorf("%s, awaiting %v: the daemon's front_waits %v (%d)", c.mode, c.awaits, vst.FrontWaits, code)
		}
		if !c.held {
			continue
		}
		// provision ends the wait, and starts the front.
		n = len(e.docker.ran("compose"))
		if err := e.installer(nil).Provision(context.Background()); err != nil {
			t.Fatal(err)
		}
		if ups, held := scaled(e, n); ups != 1 || held || !strings.Contains(e.log.String(), "starting the front, which waited for the install's data") {
			t.Errorf("%s: provision ran %d starts, the front held %v", c.mode, ups, held)
		}
	}
}

// A backup's archive (backup export, backup run --output) carries the front's archive between the
// volumes' and its last object, and a restore of one given takes it there alone: with volumes or
// without, absent too.
func TestABackupsArchiveCarriesTheFrontsArchive(t *testing.T) {
	t.Parallel()
	_, d, f, key := localVM(t)
	configure(t, d, keepLocal(key, 3))
	setTLSMode(t, d.O.Dir, "acme")
	f.volumes, f.staged = map[string][]byte{stackVolumeOf(t, d, FrontDataVolume): []byte("the front's"), stackVolumeOf(t, d, "uploads"): []byte("uploads")}, map[string][]byte{}
	h := d.Handler()
	call(t, h, "POST", "/v1/backup", BackupRequest{}, nil)
	j := waitJob(t, h)
	if j.State != "done" {
		t.Fatalf("%+v", j)
	}
	obj := j.Target
	pg, err := d.pgBackup()
	if err != nil {
		t.Fatal(err)
	}
	target := Version{"1.0.0", 1}
	var archive bytes.Buffer
	if err := (localStore{name: "notes", dir: LocalBackupsDir(d.O.Dir)}).writeArchive(&archive, obj, pg); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "in")
	if got, err := d.importArchive(context.Background(), bytes.NewReader(archive.Bytes()), dir, pg, target); err != nil || got != obj {
		t.Fatalf("%s %v", got, err)
	}
	for _, o := range []string{volumeObject(obj, "uploads"), frontObject(obj), obj} {
		if _, err := os.Stat(filepath.Join(dir, o)); err != nil {
			t.Errorf("imported without %s: %v", o, err)
		}
	}
	const dump = "notes-full-20261010T030000Z-1.0.0-r1.pgdump.age"
	vol, front := volumeObject(dump, "uploads"), frontObject(dump)
	noVolumes := *pg
	noVolumes.Volumes = nil
	for name, c := range map[string]struct {
		pg    *providers.AgeRclone
		files [][2]string
		want  string
	}{
		"volumes, front":    {pg, [][2]string{{vol, "v"}, {front, "f"}, {dump, "d"}}, ""},
		"volumes, no front": {pg, [][2]string{{vol, "v"}, {dump, "d"}}, ""},
		"front alone":       {&noVolumes, [][2]string{{front, "f"}, {dump, "d"}}, ""},
		"dump alone":        {&noVolumes, [][2]string{{dump, "d"}}, ""},
		"front after dump":  {pg, [][2]string{{vol, "v"}, {dump, "d"}, {front, "f"}}, fmt.Sprintf("it holds %q after %s", front, dump)},
		"front before vol":  {pg, [][2]string{{front, "f"}, {vol, "v"}, {dump, "d"}}, "not the archive of the volume uploads"},
		"cut after front":   {pg, [][2]string{{vol, "v"}, {front, "f"}}, "it ends before " + dump + ": it is cut short"},
		"cut before front":  {pg, [][2]string{{vol, "v"}}, "it ends before " + dump + ": it is cut short"},
		"front alone, cut":  {&noVolumes, [][2]string{{front, "f"}}, "it ends before " + dump + ": it is cut short"},
		"another's front":   {pg, [][2]string{{vol, "v"}, {frontObject(strings.Replace(dump, "T03", "T04", 1)), "f"}, {dump, "d"}}, "where " + dump + " comes"},
	} {
		dir := filepath.Join(t.TempDir(), "in")
		got, err := d.importArchive(context.Background(), bytes.NewReader(writeTar(t, c.files...)), dir, c.pg, target)
		switch {
		case c.want == "" && (err != nil || got != dump):
			t.Errorf("%s: %s %v", name, got, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

// A front whose volume was never made (it never ran) is backed up as an archive of nothing, and
// a restore into it puts nothing back.
func TestAFrontVolumeNeverMadeHoldsNothing(t *testing.T) {
	t.Parallel()
	e, d, f, key, vol, obj := frontBackupVM(t, "acme", "unused")
	gone := func(a string) ([]byte, bool, error) {
		switch {
		case strings.HasSuffix(a, "config --services"):
			return []byte("api\nweb\npostgres\n"), true, nil
		case strings.HasPrefix(a, "volume inspect ") && strings.HasSuffix(a, " "+vol):
			return nil, true, errors.New("Error: No such volume: " + vol)
		}
		return nil, false, nil
	}
	e.docker.answer = gone
	h := d.Handler()
	call(t, h, "POST", "/v1/backup", BackupRequest{}, nil)
	j := waitJob(t, h)
	if j.State != "done" {
		t.Fatalf("%+v", j)
	}
	r, err := age.Decrypt(bytes.NewReader(f.objects["store:backups/notes/"+frontObject(j.Target)]), key)
	if err != nil {
		t.Fatal(err)
	}
	if plain, _ := io.ReadAll(r); !bytes.Equal(plain, make([]byte, 1024)) {
		t.Errorf("archived %q", plain)
	}
	if err := os.WriteFile(filepath.Join(e.dir, restoreAwaitedFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	call(t, h, "POST", "/v1/restore", RestoreRequest{Object: obj, Identity: key.String(), Confirm: &Version{"1.0.0", 1}}, nil)
	if j := waitJob(t, h); j.State != "done" || !strings.Contains(strings.Join(j.Log, "\n"), "the front's volume is not there") || len(f.frontPut) != 0 {
		t.Fatalf("%+v %v", j, f.frontPut)
	}
}

// A provision that ends the wait, whose start of the front then fails, names the re-run that starts
// it: the wait is over, so a later provision does not.
func TestAFailedStartOfTheWaitingFrontNamesTheRerun(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(func(o *Options) { tlsDomains(o); o.TLS = "internal" }); err != nil {
		t.Fatal(err)
	}
	setTLSMode(t, e.dir, "acme")
	if err := os.WriteFile(filepath.Join(e.dir, restoreAwaitedFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e.docker.upErr = errors.New("unhealthy")
	err := e.installer(nil).Provision(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the wait for the install's data ended, but starting the front, which waited for it, failed: ") || !strings.Contains(err.Error(), " --dir "+e.dir+"` starts the stack, the front with it") {
		t.Fatalf("%v", err)
	}
	if _, ok := Refusal(err); ok {
		t.Error("a failure after the hook ran is answered as a refusal")
	}
}

// provision cannot end the wait (its marker stays): it says so, and the front is not started.
func TestProvisionThatCannotEndTheWaitSaysSo(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(func(o *Options) { tlsDomains(o); o.TLS = "internal" }); err != nil {
		t.Fatal(err)
	}
	setTLSMode(t, e.dir, "acme")
	// A directory in the marker's place: os.Remove cannot remove it (not empty).
	marker := filepath.Join(e.dir, restoreAwaitedFile)
	if err := os.MkdirAll(filepath.Join(marker, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := e.installer(nil).Provision(context.Background())
	if err == nil || !strings.Contains(err.Error(), "could not be removed: the front, which waits for the install's data, stays stopped; remove it, then `") || !strings.Contains(err.Error(), " --dir "+e.dir+"` starts the stack, the front with it") {
		t.Fatalf("%v", err)
	}
	// No front held (internal mode): the marker that stays is logged, as before; provision passes.
	setTLSMode(t, e.dir, "internal")
	if err := e.installer(nil).Provision(context.Background()); err != nil {
		t.Fatalf("%v", err)
	}
}

// A front held while the install awaits its data has no container by design: it is no service down.
func TestAHeldFrontIsNotDown(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(func(o *Options) { tlsDomains(o); o.TLS = "internal" }); err != nil {
		t.Fatal(err)
	}
	setTLSMode(t, e.dir, "acme")
	declared := func(context.Context) ([]string, error) { return []string{"never-started", FrontService}, nil }
	missing := func() []string {
		list, err := e.installer(nil).stackList(context.Background(), e.readEnv(), declared)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range list {
			if s.State == stateMissing {
				out = append(out, s.Service)
			}
		}
		return out
	}
	if got := missing(); !slices.Contains(got, FrontService) || !slices.Contains(got, "never-started") {
		t.Fatalf("not waiting: missing %v", got)
	}
	if err := os.WriteFile(filepath.Join(e.dir, restoreAwaitedFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := missing(); slices.Contains(got, FrontService) || !slices.Contains(got, "never-started") {
		t.Fatalf("held: missing %v", got)
	}
	// Held, a front container started by hand that exited: not a service down either.
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if strings.HasSuffix(a, " ps --all --format json") {
			return []byte(`{"Service":"` + FrontService + `","State":"exited","Health":"","ExitCode":1}`), true, nil
		}
		return nil, false, nil
	}
	list, err := e.installer(nil).stackList(context.Background(), e.readEnv(), declared)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(list, func(s Service) bool { return s.Service == FrontService }) {
		t.Errorf("a held front's container listed: %+v", list)
	}
	// One that runs (started by hand) stays listed: the hold was bypassed.
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if strings.HasSuffix(a, " ps --all --format json") {
			return []byte(`{"Service":"` + FrontService + `","State":"running","Health":"","ExitCode":0}`), true, nil
		}
		return nil, false, nil
	}
	if list, err = e.installer(nil).stackList(context.Background(), e.readEnv(), declared); err != nil || !slices.ContainsFunc(list, func(s Service) bool { return s.Service == FrontService && s.State == "running" }) {
		t.Errorf("a running held front not listed: %+v %v", list, err)
	}
}

// The front's wait is said whatever line the install's state gives first (not finished here).
func TestStatusSaysTheFrontWaitsInEveryState(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(func(o *Options) { tlsDomains(o); o.TLS = "internal" }); err != nil {
		t.Fatal(err)
	}
	setTLSMode(t, e.dir, "acme")
	for _, f := range []string{restoreAwaitedFile, installingFile} {
		if err := os.WriteFile(filepath.Join(e.dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := e.installer(func(o *Options) { o.Out = &out }).Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not finished") || !strings.Contains(out.String(), "the front waits for the restore") {
		t.Errorf("%s", out.String())
	}
}
