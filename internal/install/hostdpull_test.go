package install

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
)

// keepLocal is settings keeping n backups on the host, encrypted to key.
func keepLocal(key *age.X25519Identity, n int) BackupConfigRequest {
	return BackupConfigRequest{KeepLocal: ptr(n), Recipients: []string{key.Recipient().String()}}
}

// localVM is backupVM whose restores find the stack's services.
func localVM(t *testing.T) (*env, *Hostd, *fakeRclone, *age.X25519Identity) {
	t.Helper()
	e, d, f, key := backupVM(t)
	e.docker.answer = func(a string) ([]byte, bool, error) {
		return []byte("api\nweb\npostgres\n"), strings.HasSuffix(a, "config --services"), nil
	}
	return e, d, f, key
}

// The settings put backups in a destination or on the host, never both: the one given drops the
// other, and verify has no destination to probe on the host.
func TestKeepLocalReplacesTheDestination(t *testing.T) {
	t.Parallel()
	_, d, _, key := backupVM(t)
	h := d.Handler()
	configure(t, d, validConfig(key))
	s := configure(t, d, BackupConfigRequest{KeepLocal: ptr(3)})
	if !s.Configured || s.KeepLocal != 3 || s.Destination != "" || s.RcloneConfigSet {
		t.Errorf("keep_local: %+v", s)
	}
	var e409 errOf
	if code := call(t, h, "POST", "/v1/backup/verify", nil, &e409); code != 409 || !strings.Contains(e409.Error, "no destination to verify") {
		t.Errorf("verify: %d %s", code, e409.Error)
	}
	if s := configure(t, d, BackupConfigRequest{Destination: ptr("store:backups/notes"), RcloneConfig: ptr(rcloneConf)}); s.KeepLocal != 0 || s.Destination != "store:backups/notes" {
		t.Errorf("a destination: %+v", s)
	}
	for _, c := range []struct {
		req  BackupConfigRequest
		want string
	}{
		{BackupConfigRequest{KeepLocal: ptr(maxKeepLocal + 1)}, "keep_local 101: 1 to 100"},
		{BackupConfigRequest{KeepLocal: ptr(-1)}, "keep_local -1: 1 to 100"},
		{BackupConfigRequest{KeepLocal: ptr(2), RcloneConfig: ptr(rcloneConf)}, "drop the destination and its rclone config, or keep_local"},
	} {
		var e errOf
		if code := call(t, h, "POST", "/v1/backup/config", c.req, &e); code != 400 || !e.Refused || !strings.Contains(e.Error, c.want) {
			t.Errorf("%+v: %d %+v", c.req, code, e)
		}
	}
	// Both sides of the bound.
	if s := configure(t, d, BackupConfigRequest{KeepLocal: ptr(maxKeepLocal)}); s.KeepLocal != maxKeepLocal {
		t.Errorf("%+v", s)
	}
	if s := configure(t, d, BackupConfigRequest{KeepLocal: ptr(1)}); s.KeepLocal != 1 {
		t.Errorf("%+v", s)
	}
	// keep_local 0 on the host leaves the backups nowhere.
	var e errOf
	if code := call(t, h, "POST", "/v1/backup/config", BackupConfigRequest{KeepLocal: ptr(0)}, &e); code != 400 || !strings.Contains(e.Error, "a destination is required") {
		t.Errorf("%d %+v", code, e)
	}
}

// Kept on the host: each object 0600 in a 0700 directory of the install's, recorded, listed as
// local, the profile's latest kept, an orphan archive pruned; restored with no destination.
func TestABackupKeptOnTheHost(t *testing.T) {
	t.Parallel()
	e, d, f, key := localVM(t)
	h := d.Handler()
	configure(t, d, keepLocal(key, 2))
	vol := stackVolumeOf(t, d, "uploads")
	f.volumes, f.staged = map[string][]byte{vol: []byte("the uploads' archive")}, map[string][]byte{}
	dir := LocalBackupsDir(d.O.Dir)
	// Two older backups and an orphan archive, as earlier runs left them; a newer archive of a
	// backup still running stays.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Both within the two hours an orphan archive is kept: the one pruned takes its archive along.
	at := func(ago time.Duration) string { return time.Now().UTC().Add(-ago).Format("20060102T150405Z") }
	old := []string{"notes-full-" + at(20*time.Minute) + "-1.0.0-r1", "notes-full-" + at(10*time.Minute) + "-1.0.0-r1"}
	for _, n := range []string{old[0] + ".pgdump.age", old[0] + ".uploads.tar.age", old[1] + ".pgdump.age", old[1] + ".uploads.tar.age", "notes-full-20191231T000000Z-1.0.0-r1.uploads.tar.age", "notes-full-20991231T000000Z-1.0.0-r1.uploads.tar.age", "notes-full-20200103T000000Z-1.0.0-r1.pgdump.age.partial"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var started JobStarted
	if code := call(t, h, "POST", "/v1/backup", nil, &started); code != 202 {
		t.Fatalf("%d", code)
	}
	if j := waitJob(t, h); j.State != "done" || !strings.Contains(strings.Join(j.Log, "\n"), "backed up: "+filepath.Join(dir, started.Target)) {
		t.Fatalf("%+v", j)
	}
	obj := started.Target
	left, _ := os.ReadDir(dir)
	var names []string
	for _, de := range left {
		if !strings.HasPrefix(de.Name(), ".") { // the export lock
			names = append(names, de.Name())
		}
	}
	want := []string{old[1] + ".pgdump.age", old[1] + ".uploads.tar.age", strings.TrimSuffix(obj, ".pgdump.age") + ".uploads.tar.age", obj, "notes-full-20991231T000000Z-1.0.0-r1.uploads.tar.age"}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("kept %v, not %v", names, want)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("%s: %v %v", dir, fi.Mode(), err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, obj))
	if fi, _ := os.Stat(filepath.Join(dir, obj)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	if r, err := age.Decrypt(bytes.NewReader(raw), key); err != nil {
		t.Fatal(err)
	} else if plain, _ := io.ReadAll(r); string(plain) != "the database's dump" {
		t.Errorf("%q", plain)
	}
	rec, err := readDigests(d.O.Dir)
	if sum := sha256.Sum256(raw); err != nil || rec[localPrefix+obj].SHA256 != hex.EncodeToString(sum[:]) || len(rec) != 2 {
		t.Errorf("%v %+v", err, rec)
	}
	if len(f.calls()) != 0 {
		t.Errorf("rclone ran %v", f.calls())
	}
	var list []BackupObject
	if code := call(t, h, "GET", "/v1/backup/list", nil, &list); code != 200 || len(list) != 2 || list[1].Name != obj || !list[1].Local || !list[0].Local || list[1].Size != int64(len(raw)) {
		t.Errorf("%d %+v", code, list)
	}
	// Restored from the host: no destination.
	n := len(e.docker.ran("compose"))
	call(t, h, "POST", "/v1/restore", RestoreRequest{Object: obj, Identity: key.String(), Confirm: &Version{"1.0.0", 1}}, nil)
	if j := waitJob(t, h); j.State != "done" || !swapped(e, n) || len(f.staged) != 0 || string(f.volumes[vol]) != "the uploads' archive" {
		t.Errorf("%+v", j)
	}
	// A backup the host does not keep, with no destination to find it in, is refused before the job.
	var e409 errOf
	const gone = "notes-full-20200101T000001Z-1.0.0-r1.pgdump.age"
	if code := call(t, h, "POST", "/v1/restore", RestoreRequest{Object: gone, Identity: key.String(), Confirm: &Version{"1.0.0", 1}}, &e409); code != 404 || !e409.Refused || !strings.Contains(e409.Error, gone+" is not among the backups kept on the host") {
		t.Errorf("%d %s", code, e409.Error)
	}
	// Without settings, it names them.
	call(t, h, "DELETE", "/v1/backup/config", nil, nil)
	if code := call(t, h, "POST", "/v1/restore", RestoreRequest{Object: gone, Identity: key.String(), Confirm: &Version{"1.0.0", 1}}, &e409); code != 409 || !strings.Contains(e409.Error, "no backup is configured") {
		t.Errorf("%d %s", code, e409.Error)
	}
}

// writeTar writes a tar of the named contents, in order.
func writeTar(t *testing.T, files ...[2]string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: f[0], Size: int64(len(f[1])), Mode: 0o600}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(f[1]))
	}
	tw.Close()
	return b.Bytes()
}

// restoreInputBody is POST /v1/restore/input's body: req on a line, then the archive.
func restoreInputBody(t *testing.T, req RestoreRequest, archive []byte) io.Reader {
	t.Helper()
	line, _ := json.Marshal(req)
	return io.MultiReader(bytes.NewReader(append(line, '\n')), bytes.NewReader(archive))
}

func postInput(t *testing.T, h http.Handler, body io.Reader, out any) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://hostd/v1/restore/input", body)
	resp, err := handlerTransport{h}.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

// An archive is restored only when it is exactly one whole backup of the release the install
// runs: anything else is refused before the job, the archive's directory removed.
func TestARestoresArchiveIsOneWholeBackup(t *testing.T) {
	t.Parallel()
	_, d, _, key := localVM(t)
	h := d.Handler()
	const obj = "notes-full-20261010T030000Z-1.0.0-r1.pgdump.age"
	vol := strings.TrimSuffix(obj, ".pgdump.age") + ".uploads.tar.age"
	v := &Version{"1.0.0", 1}
	req := RestoreRequest{Identity: key.String(), Confirm: v}
	dump := strings.Repeat("d", 1000) // two blocks: a cut inside it leaves it short
	whole := writeTar(t, [2]string{vol, "v"}, [2]string{obj, dump})
	for name, c := range map[string]struct {
		body io.Reader
		code int
		want string
	}{
		"empty":            {restoreInputBody(t, req, nil), 400, "it is empty"},
		"cut before dump":  {restoreInputBody(t, req, writeTar(t, [2]string{vol, "v"})), 400, "it ends before " + obj + ": it is cut short"},
		"cut in the dump":  {restoreInputBody(t, req, whole[:len(whole)-1024-1024+500]), 400, obj + " is cut short"},
		"dump first":       {restoreInputBody(t, req, writeTar(t, [2]string{obj, "d"}, [2]string{vol, "v"})), 400, "not the archive of the volume uploads"},
		"another object":   {restoreInputBody(t, req, writeTar(t, [2]string{vol, "v"}, [2]string{"notes-full-20261010T030000Z-1.0.0-r1.other.tar.age", "x"})), 400, "where " + obj + " comes"},
		"after its end":    {restoreInputBody(t, req, writeTar(t, [2]string{vol, "v"}, [2]string{obj, "d"}, [2]string{"x", "y"})), 400, `it holds "x" after ` + obj},
		"data after":       {restoreInputBody(t, req, append(slices.Clone(whole), []byte("junk")...)), 400, "data follows its end"},
		"another release":  {restoreInputBody(t, req, writeTar(t, [2]string{strings.Replace(vol, "1.0.0", "0.9.0", 1), "v"}, [2]string{strings.Replace(obj, "1.0.0", "0.9.0", 1), "d"})), 409, "a backup restores onto the release it was taken from"},
		"named object":     {restoreInputBody(t, RestoreRequest{Object: obj, Identity: key.String(), Confirm: v}, whole), 400, "object is the archive's"},
		"no line":          {strings.NewReader(strings.Repeat("x", maxRestoreLine+1)), 400, "on one line"},
		"not confirmed":    {restoreInputBody(t, RestoreRequest{Identity: key.String()}, whole), 400, "confirm the release it runs"},
		"not an identity":  {restoreInputBody(t, RestoreRequest{Identity: "x", Confirm: v}, whole), 400, "not an age identity"},
		"a link, not file": {restoreInputBody(t, req, linkTar(t, vol)), 400, vol + " is not a file"},
	} {
		var e errOf
		if code := postInput(t, h, c.body, &e); code != c.code || !strings.Contains(e.Error, c.want) {
			t.Errorf("%s: %d %s", name, code, e.Error)
		}
		if _, err := os.Stat(filepath.Join(d.O.Dir, restoreInput)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the archive's directory is left: %v", name, err)
		}
	}
	// Padding of zeros after the end is a record's, not data.
	var started JobStarted
	if code := postInput(t, h, restoreInputBody(t, req, append(slices.Clone(whole), make([]byte, 4096)...)), &started); code != 202 || started.Target != obj {
		t.Fatalf("%d %+v", code, started)
	}
	// Its objects are not this install's (none recorded): refused in the job, nothing loaded.
	if j := waitJob(t, h); j.State != "failed" || !strings.Contains(j.Error, filepath.Join(d.O.Dir, restoreInput, obj)+": this install recorded no digest of it") {
		t.Errorf("%+v", j)
	}
	if _, err := os.Stat(filepath.Join(d.O.Dir, restoreInput)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the archive's directory is left: %v", err)
	}
}

// linkTar is an archive whose first member is a link named name.
func linkTar(t *testing.T, name string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: "/etc/shadow"})
	tw.Close()
	return b.Bytes()
}

// A backup streamed out with no settings: encrypted to the recipients given, named on standard
// error (one JSON line with --json), written whole to its file; given back, it restores as one of
// this install's, and on an install without its record only --unrecorded.
func TestABackupStreamedOutAndGivenBack(t *testing.T) {
	t.Parallel()
	e, d, f, key := localVM(t)
	vol := stackVolumeOf(t, d, "uploads")
	f.volumes, f.staged = map[string][]byte{vol: []byte("at the backup")}, map[string][]byte{}
	sock := served(t, d)
	tmp := t.TempDir()
	id := filepath.Join(tmp, "key.txt")
	if err := os.WriteFile(id, []byte(key.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(stdin io.Reader, args ...string) (string, string, error) {
		var out, errb bytes.Buffer
		cmd := vmCommand(e.r, e.self, &out, &errb, func(bool) Prompter { return nil }, clientTimes{unit: time.Millisecond})
		cmd.SetArgs(append(args, "--socket", sock))
		if stdin != nil {
			cmd.SetIn(stdin)
		}
		err := cmd.ExecuteContext(context.Background())
		return out.String(), errb.String(), err
	}
	// No recipient, none in settings: refused.
	if _, errs, err := run(nil, "backup", "run", "--output", "-"); !errors.As(err, new(Refused)) || !strings.Contains(err.Error(), "no recipient to encrypt the backup to") {
		t.Errorf("%v %s", err, errs)
	}
	if _, _, err := run(nil, "backup", "run", "--recipient", key.Recipient().String()); !errors.As(err, new(Refused)) || !strings.Contains(err.Error(), "go with --output") {
		t.Errorf("%v", err)
	}
	// A file that cannot be written fails before the backup is taken.
	if _, _, err := run(nil, "backup", "run", "--output", filepath.Join(tmp, "none", "b.tar"), "--recipient", key.Recipient().String()); err == nil || !strings.Contains(err.Error(), "--output: ") {
		t.Errorf("%v", err)
	}
	var j JobStatus
	if call(t, d.Handler(), "GET", "/v1/job", nil, &j); j.Kind != "" {
		t.Errorf("a backup ran for a file that cannot be written: %+v", j)
	}
	file := filepath.Join(tmp, "b.tar")
	out, errs, err := run(nil, "backup", "run", "--output", file, "--recipient", key.Recipient().String(), "--json")
	if err != nil || out != "" {
		t.Fatalf("%v %q %s", err, out, errs)
	}
	lines := strings.Split(strings.TrimSpace(errs), "\n")
	var named ExportedBackup
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &named); err != nil {
		t.Fatalf("%v: %s", err, errs)
	}
	archive, err := os.ReadFile(file)
	sum := sha256.Sum256(archive)
	if err != nil || named.SHA256 != hex.EncodeToString(sum[:]) || named.Bytes != int64(len(archive)) || named.Release != (Version{"1.0.0", 1}) || !matchesOwnBackup("notes", named.Object) {
		t.Fatalf("%v %+v", err, named)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Errorf("%v", fi.Mode())
	}
	if _, err := os.Stat(file + partialSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%v", err)
	}
	// The host keeps it until the next one streamed out sweeps it; the archive holds the
	// backup's objects, the dump last.
	if _, err := os.Stat(filepath.Join(outputOf(d.O.Dir, named.Object), named.Object)); err != nil {
		t.Errorf("not kept until swept: %v", err)
	}
	tr := tar.NewReader(bytes.NewReader(archive))
	var members []string
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		members = append(members, hdr.Name)
	}
	if want := []string{volumeObject(named.Object, "uploads"), named.Object}; !slices.Equal(members, want) {
		t.Errorf("members %v", members)
	}
	// Still there, export streams it again: the same bytes.
	if again, _, err := run(nil, "backup", "export", named.Object, "-o", "-"); err != nil || again != string(archive) {
		t.Errorf("exported again: %v, %d bytes", err, len(again))
	}
	// Given back on standard input: recorded, so restored as it is.
	f.volumes[vol] = []byte("since")
	if _, _, err := run(bytes.NewReader(archive), "restore", "--input", "-", "--identity", "-", "--yes"); err == nil || !strings.Contains(err.Error(), "--identity FILE") {
		t.Errorf("%v", err)
	}
	if _, _, err := run(bytes.NewReader(archive), "restore", "--input", "-", "--identity", id); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("%v", err)
	}
	if _, _, err := run(bytes.NewReader(archive), "restore", named.Object, "--input", "-", "--identity", id, "--yes"); !errors.As(err, new(Refused)) || !strings.Contains(err.Error(), "one of them") {
		t.Errorf("%v", err)
	}
	if _, _, err := run(nil, "restore", "--input", "", "--identity", id, "--yes"); !errors.As(err, new(Refused)) || !strings.Contains(err.Error(), "names no file") {
		t.Errorf("%v", err)
	}
	n := len(e.docker.ran("compose"))
	if _, errs, err := run(bytes.NewReader(archive), "restore", "--input", "-", "--identity", id, "--yes"); err != nil || !swapped(e, n) || string(f.volumes[vol]) != "at the backup" {
		t.Fatalf("%v %s", err, errs)
	}
	// An install without its record (another machine's): --unrecorded alone loads it.
	if err := os.Remove(BackupDigestsFile(d.O.Dir)); err != nil {
		t.Fatal(err)
	}
	n = len(e.docker.ran("compose"))
	if _, _, err := run(nil, "restore", "--input", file, "--identity", id, "--yes"); err == nil || !strings.Contains(err.Error(), "this install recorded no digest of it") || swapped(e, n) {
		t.Errorf("%v", err)
	}
	if _, errs, err := run(nil, "restore", "--input", file, "--identity", id, "--yes", "--unrecorded"); err != nil || !swapped(e, n) {
		t.Errorf("%v %s", err, errs)
	}
	// A truncated archive restores nothing.
	n = len(e.docker.ran("compose"))
	if _, _, err := run(bytes.NewReader(archive[:len(archive)/2]), "restore", "--input", "-", "--identity", id, "--yes", "--unrecorded"); !errors.As(err, new(Refused)) || swapped(e, n) {
		t.Errorf("%v", err)
	}
}

// A backup kept on the host streams out as the same bytes each time, to standard output alone.
func TestABackupKeptExportsTheSameBytes(t *testing.T) {
	t.Parallel()
	e, d, _, key := localVM(t)
	configure(t, d, keepLocal(key, 1))
	h := d.Handler()
	var started JobStarted
	call(t, h, "POST", "/v1/backup", nil, &started)
	if j := waitJob(t, h); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	sock := served(t, d)
	export := func() (string, string) {
		var out, errb bytes.Buffer
		cmd := vmCommand(e.r, e.self, &out, &errb, func(bool) Prompter { return nil }, clientTimes{unit: time.Millisecond})
		cmd.SetArgs([]string{"backup", "export", started.Target, "-o", "-", "--socket", sock})
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("%v %s", err, errb.String())
		}
		return out.String(), errb.String()
	}
	a, errs := export()
	b, _ := export()
	sum := sha256.Sum256([]byte(a))
	if a != b || len(a) == 0 || !strings.Contains(errs, started.Target+" streamed to standard output") || !strings.Contains(errs, hex.EncodeToString(sum[:])) {
		t.Errorf("%d %d bytes; %s", len(a), len(b), errs)
	}
	// Each object timed as its backup's name says, whenever it is exported.
	tr := tar.NewReader(strings.NewReader(a))
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if at, ok := backupTime("notes", hdr.Name); !ok || !hdr.ModTime.Equal(at) || hdr.Mode != 0o600 || hdr.Uid != 0 {
			t.Errorf("%s: %v %o %d", hdr.Name, hdr.ModTime, hdr.Mode, hdr.Uid)
		}
	}
	// It is still kept.
	if _, err := os.Stat(filepath.Join(LocalBackupsDir(d.O.Dir), started.Target)); err != nil {
		t.Error(err)
	}
}

// What a backup or a restore cut off left on the host goes as the daemon starts.
func TestTheDaemonSweepsWhatACutOffRunLeft(t *testing.T) {
	t.Parallel()
	_, d, _, _ := backupVM(t)
	partial := filepath.Join(LocalBackupsDir(d.O.Dir), "notes-full-20261010T030000Z-1.0.0-r1.pgdump.age"+partialSuffix)
	kept := filepath.Join(LocalBackupsDir(d.O.Dir), "notes-full-20261010T030000Z-1.0.0-r1.pgdump.age")
	output := filepath.Join(outputBackups(d.O.Dir), "x")
	input := filepath.Join(d.O.Dir, restoreInput, "x")
	for _, p := range []string{partial, kept, output, input} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	served(t, d)
	for p, stays := range map[string]bool{partial: false, kept: true, output: false, input: false} {
		if _, err := os.Stat(p); (err == nil) != stays {
			t.Errorf("%s: %v", p, err)
		}
	}
}

// The in-process transport holds an answer whole unless its handler flushes it; once flushed it
// streams, and a handler that then aborts leaves it cut short, never whole.
func TestTheInProcessTransportStreams(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	tr := handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/held":
			w.WriteHeader(http.StatusTeapot)
			io.WriteString(w, "whole")
		case "/stream":
			io.WriteString(w, "first ")
			w.(http.Flusher).Flush()
			<-release // the answer is read before the handler ends
			io.WriteString(w, "then")
		case "/abort":
			io.WriteString(w, "part")
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		case "/abort-held":
			panic(http.ErrAbortHandler)
		}
	})}
	get := func(path string) (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodGet, "http://hostd"+path, nil)
		return tr.RoundTrip(req)
	}
	resp, err := get("/held")
	if err != nil || resp.StatusCode != http.StatusTeapot || resp.ContentLength != 5 {
		t.Fatalf("%v %+v", err, resp)
	}
	resp, err = get("/stream")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %+v", err, resp)
	}
	head := make([]byte, 6)
	if _, err := io.ReadFull(resp.Body, head); err != nil || string(head) != "first " {
		t.Fatalf("%q %v", head, err)
	}
	close(release)
	if rest, err := io.ReadAll(resp.Body); err != nil || string(rest) != "then" {
		t.Errorf("%q %v", rest, err)
	}
	resp, err = get("/abort")
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(resp.Body); !errors.Is(err, io.ErrUnexpectedEOF) || string(b) != "part" {
		t.Errorf("%q %v", b, err)
	}
	if _, err := get("/abort-held"); err == nil {
		t.Error("an answer aborted before it was sent reads as one")
	}
	// A request's body reaches the handler as it is sent, not read whole first.
	got := make(chan string, 1)
	pr, pw := io.Pipe()
	tr2 := handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 3)
		io.ReadFull(r.Body, b)
		got <- string(b)
		io.Copy(io.Discard, r.Body)
	})}
	req, _ := http.NewRequest(http.MethodPost, "http://hostd/in", pr)
	done := make(chan error, 1)
	go func() { _, err := tr2.RoundTrip(req); done <- err }()
	pw.Write([]byte("abc"))
	select {
	case s := <-got:
		if s != "abc" {
			t.Errorf("%q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the body was held until its end")
	}
	pw.Close()
	if err := <-done; err != nil {
		t.Error(err)
	}
}

// A refusal the daemon answers before it read the archive whole (its first object names another
// release) reaches the operator as the refusal, not as a body the daemon stopped reading.
func TestAnArchiveRefusedEarlyIsTheRefusal(t *testing.T) {
	t.Parallel()
	e, d, _, key := localVM(t)
	sock := served(t, d)
	tmp := t.TempDir()
	id := filepath.Join(tmp, "key.txt")
	if err := os.WriteFile(id, []byte(key.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const other = "notes-full-20261010T030000Z-0.9.0-r1"
	big := writeTar(t, [2]string{other + ".uploads.tar.age", strings.Repeat("x", 32<<20)}, [2]string{other + ".pgdump.age", "d"})
	file := filepath.Join(tmp, "other.tar")
	if err := os.WriteFile(file, big, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, stdin := range []bool{false, true} {
		var out, errb bytes.Buffer
		cmd := vmCommand(e.r, e.self, &out, &errb, func(bool) Prompter { return nil }, clientTimes{unit: time.Millisecond})
		args := []string{"restore", "--input", file, "--identity", id, "--yes", "--socket", sock}
		if stdin {
			args[2] = "-"
			cmd.SetIn(bytes.NewReader(big))
		}
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(context.Background())
		if !errors.As(err, new(Refused)) || !strings.Contains(err.Error(), "a backup restores onto the release it was taken from") {
			t.Errorf("stdin %v: %v", stdin, err)
		}
	}
}

// A stream the daemon cuts short fails the export and leaves no file, whole or partial; a partial
// file there (another run's, or one cut off) is refused and left as it is.
func TestAnExportCutShortLeavesNoFile(t *testing.T) {
	t.Parallel()
	c := &hostClient{ctl: "notes-stow", http: &http.Client{Transport: handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, strings.Repeat("x", 4096))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})}}}
	const obj = "notes-full-20261010T030000Z-1.0.0-r1.pgdump.age"
	export := func(path string) error {
		cmd := backupCommand(c, "notes", io.Discard, io.Discard)
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		cmd.SetArgs([]string{"export", obj, "-o", path})
		return cmd.ExecuteContext(context.Background())
	}
	path := filepath.Join(t.TempDir(), "b.tar")
	err := export(path)
	if !errors.As(err, new(streamCut)) || !strings.Contains(err.Error(), "stopped after 4096 bytes") {
		t.Errorf("%v", err)
	}
	for _, p := range []string{path, path + partialSuffix} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if err := os.WriteFile(path+partialSuffix, []byte("another's"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := export(path); !errors.As(err, new(Refused)) || !strings.Contains(err.Error(), "exists: another run writes it") {
		t.Errorf("%v", err)
	}
	if b, _ := os.ReadFile(path + partialSuffix); string(b) != "another's" {
		t.Errorf("%q", b)
	}
	// `backup run --output` names export as the way back only for a stream cut short.
	if err := streamedOutErr(streamCut{errors.New("cut")}, "notes-stow", obj); !strings.Contains(err.Error(), "until the next backup run --output, or the host daemon's restart: `notes-stow backup export "+obj+" -o FILE`") {
		t.Errorf("%v", err)
	}
	if err := streamedOutErr(Refused{Reason: "gone"}, "notes-stow", obj); strings.Contains(err.Error(), "backup export") || !errors.As(err, new(Refused)) {
		t.Errorf("%v", err)
	}
}

// A backup taken to be streamed out sweeps the one before it, unless that one is streaming out:
// then it fails at once rather than hold the job slot.
func TestAnOutputSweepsNotWhatStreamsOut(t *testing.T) {
	t.Parallel()
	_, d, _, key := backupVM(t)
	h := d.Handler()
	recip := []string{key.Recipient().String()}
	if err := os.MkdirAll(LocalBackupsDir(d.O.Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := flockWait(context.Background(), exportLock(LocalBackupsDir(d.O.Dir)), false)
	if err != nil {
		t.Fatal(err)
	}
	call(t, h, "POST", "/v1/backup", BackupRequest{Output: true, Recipients: recip}, nil)
	if j := waitJob(t, h); j.State != "failed" || !strings.Contains(j.Error, "is streaming out now") {
		t.Errorf("%+v", j)
	}
	unlock()
	// One an earlier run left, swept by the next.
	const earlier = "notes-full-20200101T000000Z-1.0.0-r1.pgdump.age"
	if err := os.MkdirAll(outputOf(d.O.Dir, earlier), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputOf(d.O.Dir, earlier), earlier), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var started JobStarted
	call(t, h, "POST", "/v1/backup", BackupRequest{Output: true, Recipients: recip}, &started)
	if j := waitJob(t, h); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if _, err := os.Stat(outputOf(d.O.Dir, earlier)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the earlier one is not swept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outputOf(d.O.Dir, started.Target), started.Target)); err != nil {
		t.Error(err)
	}
}

// A backup kept on the host stops as its writes reach the disk's reserve, read as it writes.
func TestAKeptBackupStopsAtTheReserve(t *testing.T) {
	every := diskWatchEvery
	t.Cleanup(func() { diskWatchEvery = every })
	diskWatchEvery = 4
	_, d, f, key := localVM(t)
	f.volumes = map[string][]byte{stackVolumeOf(t, d, "uploads"): []byte(strings.Repeat("u", 1000))}
	h := d.Handler()
	configure(t, d, keepLocal(key, 1))
	// Room before each object is written; none once one is part written: only a read as it
	// writes stops it.
	var low atomic.Bool
	d.Statfs = func(dir string) *Disk {
		parts, _ := filepath.Glob(filepath.Join(dir, "*"+partialSuffix))
		for _, p := range parts {
			if fi, err := os.Stat(p); err == nil && fi.Size() > 0 {
				low.Store(true)
				return &Disk{Path: dir, Total: 20000, Available: 10}
			}
		}
		return &Disk{Path: dir, Total: 20000, Available: 5000}
	}
	call(t, h, "POST", "/v1/backup", nil, nil)
	if j := waitJob(t, h); j.State != "failed" || !strings.Contains(j.Error, "kept free for the stack's data") || !low.Load() {
		t.Errorf("read low %v: %+v", low.Load(), j)
	}
	if left, _ := os.ReadDir(LocalBackupsDir(d.O.Dir)); len(left) != 0 {
		t.Errorf("left %v", left)
	}
}

// restore --input is refused before its archive is read while a job runs.
func TestARestoreOfAnArchiveWaitsForNoJob(t *testing.T) {
	t.Parallel()
	_, d, _, key := localVM(t)
	d.mu.Lock()
	d.job = &job{Kind: "backup", State: "running"}
	d.mu.Unlock()
	read := false
	body := io.MultiReader(restoreInputBody(t, RestoreRequest{Identity: key.String(), Confirm: &Version{"1.0.0", 1}}, nil), readerFunc(func([]byte) (int, error) { read = true; return 0, io.EOF }))
	var e errOf
	if code := postInput(t, d.Handler(), body, &e); code != 409 || read {
		t.Errorf("%d %+v, archive read %v", code, e, read)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// A backup streamed out is the operator's, asked for by hand, its recipients its own.
func TestBackupOutputIsTheOperators(t *testing.T) {
	t.Parallel()
	e, d, _, key := backupVM(t)
	recip := []string{key.Recipient().String()}
	var e403 errOf
	if code := call(t, asApp(t, e), "POST", "/v1/backup", BackupRequest{Output: true, Recipients: recip}, &e403); code != 403 || !strings.Contains(e403.Error, "the operator's") {
		t.Errorf("app: %d %s", code, e403.Error)
	}
	h := d.Handler()
	for _, req := range []BackupRequest{{Output: true, IfConfigured: true, Recipients: recip}, {Recipients: recip}} {
		var e errOf
		if code := call(t, h, "POST", "/v1/backup", req, &e); code != 400 || !e.Refused {
			t.Errorf("%+v: %d %+v", req, code, e)
		}
	}
	// keep_local drops the destination and its rclone config: the operator's alone.
	configure(t, d, validConfig(key))
	if code := call(t, asApp(t, e), "POST", "/v1/backup/config", BackupConfigRequest{KeepLocal: ptr(1)}, &e403); code != 403 || !strings.Contains(e403.Error, "keep_local is the operator's") {
		t.Errorf("app keep_local: %d %s", code, e403.Error)
	}
	// Given recipients, settings that do not read are left alone.
	if err := os.WriteFile(BackupFile(d.O.Dir), []byte("not: settings\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var started JobStarted
	if code := call(t, h, "POST", "/v1/backup", BackupRequest{Output: true, Recipients: recip}, &started); code != 202 {
		t.Fatalf("%d", code)
	}
	if j := waitJob(t, h); j.State != "done" || j.Result != started.Target {
		t.Errorf("%+v", j)
	}
	// It is not the settings' last backup, which status shows.
	if _, err := os.Stat(BackupLastFile(d.O.Dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("recorded as the last backup: %v", err)
	}
	// Not streamed out (the stream failed), it stays, and export streams it again, keeping it.
	var out bytes.Buffer
	c := &hostClient{ctl: "notes-stow", http: &http.Client{Transport: handlerTransport{h}}}
	if err := c.export(context.Background(), started.Target, false, "-", nil, &out, io.Discard, false); err != nil || out.Len() == 0 {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(filepath.Join(outputOf(d.O.Dir, started.Target), started.Target)); err != nil {
		t.Errorf("an export by name removed it: %v", err)
	}
	var e500 errOf
	if code := call(t, h, "POST", "/v1/backup", BackupRequest{Output: true}, &e500); code < 400 || !strings.Contains(e500.Error, "backup.yaml") {
		t.Errorf("settings that do not read, no recipient: %d %s", code, e500.Error)
	}
}

// What is written on the host leaves the disk the stack's data shares a twentieth free: an
// archive's object refused past it, a backup kept there stopped before it.
func TestWritesOnTheHostKeepTheDisksReserve(t *testing.T) {
	t.Parallel()
	_, d, _, key := localVM(t)
	h := d.Handler()
	const obj = "notes-full-20261010T030000Z-1.0.0-r1.pgdump.age"
	vol := strings.TrimSuffix(obj, ".pgdump.age") + ".uploads.tar.age"
	archive := writeTar(t, [2]string{vol, "v"}, [2]string{obj, strings.Repeat("d", 1000)})
	req := RestoreRequest{Identity: key.String(), Confirm: &Version{"1.0.0", 1}}
	var avail atomic.Uint64
	d.Statfs = func(dir string) *Disk { return &Disk{Path: dir, Total: 20000, Available: avail.Load()} }
	// The dump's 1000 bytes and the reserve's 1000: one byte short, then just enough.
	avail.Store(1999)
	var e errOf
	if code := postInput(t, h, restoreInputBody(t, req, archive), &e); code != 400 || !strings.Contains(e.Error, "kept free for the stack's data") {
		t.Errorf("%d %s", code, e.Error)
	}
	avail.Store(2000)
	var started JobStarted
	if code := postInput(t, h, restoreInputBody(t, req, archive), &started); code != 202 {
		t.Errorf("%d %+v", code, started)
	}
	waitJob(t, h)
	// Below the reserve, a backup kept on the host writes nothing.
	configure(t, d, keepLocal(key, 1))
	avail.Store(999)
	call(t, h, "POST", "/v1/backup", nil, nil)
	if j := waitJob(t, h); j.State != "failed" || !strings.Contains(j.Error, "kept free for the stack's data") {
		t.Errorf("%+v", j)
	}
	if left, _ := os.ReadDir(LocalBackupsDir(d.O.Dir)); len(left) != 0 {
		t.Errorf("left %v", left)
	}
	if roomFor(&Disk{Total: 20000, Available: 1000}, "/d", 0) != nil || roomFor(&Disk{Total: 20000, Available: 999}, "/d", 0) == nil || roomFor(nil, "/d", 1<<60) != nil {
		t.Error("the reserve's bound")
	}
}

// An export holds the lock while it streams: a backup streamed out meanwhile does not sweep it,
// and keep_local's pruning skips its turn; neither waits.
func TestAnExportIsNotSweptNorPruned(t *testing.T) {
	t.Parallel()
	_, d, _, key := backupVM(t)
	h := d.Handler()
	recip := []string{key.Recipient().String()}
	var first JobStarted
	call(t, h, "POST", "/v1/backup", BackupRequest{Output: true, Recipients: recip}, &first)
	if j := waitJob(t, h); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://hostd/v1/backup/export?output=1&object="+first.Target, nil)
	resp, err := handlerTransport{h}.RoundTrip(req) // streaming, its body not read yet
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %+v", err, resp)
	}
	call(t, h, "POST", "/v1/backup", BackupRequest{Output: true, Recipients: recip}, nil)
	if j := waitJob(t, h); j.State != "failed" || !strings.Contains(j.Error, "is streaming out now") {
		t.Errorf("%+v", j)
	}
	// Kept on the host: a backup is taken, its pruning left for the next.
	configure(t, d, keepLocal(key, 1))
	const older = "notes-full-20200101T000000Z-1.0.0-r1.pgdump.age"
	if err := os.WriteFile(filepath.Join(LocalBackupsDir(d.O.Dir), older), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	call(t, h, "POST", "/v1/backup", nil, nil)
	if j := waitJob(t, h); j.State != "done" {
		t.Errorf("%+v", j)
	}
	var st BackupStatus
	call(t, h, "GET", "/v1/backup/status", nil, &st)
	if st.Last == nil || !strings.Contains(st.Last.PruneError, "the next backup prunes them") {
		t.Errorf("%+v", st.Last)
	}
	if _, err := os.Stat(filepath.Join(LocalBackupsDir(d.O.Dir), older)); err != nil {
		t.Errorf("pruned during an export: %v", err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || len(b) == 0 {
		t.Errorf("the export: %v, %d bytes", err, len(b))
	}
	// Ended, the next backup prunes.
	call(t, h, "POST", "/v1/backup", nil, nil)
	waitJob(t, h)
	if _, err := os.Stat(filepath.Join(LocalBackupsDir(d.O.Dir), older)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("not pruned after the export: %v", err)
	}
}

// An export of what the host does not hold is refused, naming what to do.
func TestAnExportOfWhatIsGoneIsRefused(t *testing.T) {
	t.Parallel()
	_, d, _, _ := backupVM(t)
	h := d.Handler()
	const obj = "notes-full-20200101T000000Z-1.0.0-r1.pgdump.age"
	for q, want := range map[string]string{
		"output=1&object=" + obj: "taken to be streamed out is gone (another was taken since, or the host daemon restarted): back up again",
		"object=" + obj:          "is not among the backups kept on the host (`backup list` marks them local), nor one streamed out still there",
		"object=../x":            "is not a backup of notes",
	} {
		var e errOf
		if code := call(t, h, "GET", "/v1/backup/export?"+q, nil, &e); code/100 != 4 || !e.Refused || !strings.Contains(e.Error, want) {
			t.Errorf("%s: %d %+v", q, code, e)
		}
	}
	// A directory named as the file: refused before any backup.
	dir := t.TempDir()
	if _, err := createPartial(dir); !errors.As(err, new(Refused)) || !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("%v", err)
	}
}

// An export whose client reads nothing gives the lock up once a write waits exportStall: a backup
// streamed out after it is not held off for good. Not parallel: it shortens exportStall.
func TestAStalledExportEnds(t *testing.T) {
	stall := exportStall
	t.Cleanup(func() { exportStall = stall })
	exportStall = 200 * time.Millisecond
	e, d, _, key := backupVM(t)
	e.docker.dump = strings.Repeat("d", 8<<20) // past what the socket's buffers hold
	// Through the socket alone: the served daemon's own handler.
	sock := served(t, d)
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	hc := &hostClient{ctl: "notes-stow", http: c, clientTimes: clientTimes{unit: time.Millisecond}}
	backup := func() (JobStatus, error) {
		var started JobStarted
		if err := hc.do(context.Background(), http.MethodPost, "/v1/backup", BackupRequest{Output: true, Recipients: []string{key.Recipient().String()}}, &started); err != nil {
			return JobStatus{}, err
		}
		return hc.followJob(context.Background(), io.Discard, started)
	}
	first, err := backup()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get("http://hostd/v1/backup/export?output=1&object=" + first.Target)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %+v", err, resp)
	}
	defer resp.Body.Close() // never read
	// Given up once a write waited exportStall.
	freed := false
	for range 200 {
		if unlock, err := lockFile(exportLock(LocalBackupsDir(d.O.Dir))); err == nil {
			unlock()
			freed = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !freed {
		t.Fatal("a stalled export holds the lock")
	}
	if j, err := backup(); err != nil {
		t.Errorf("held off by a stalled export: %v %+v", err, j)
	}
}
