package install

import (
	"archive/tar"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/norvexlabs/stowage/internal/providers"
)

// A backup's objects live in a store: the destination of the settings (rclone, which the backup
// runner reaches), or a directory of the install's own on the host's persistent disk — the backups
// kept there (`keep_local`), the one `backup run --output` streams out, the archive `restore
// --input` was given. Every object is age-encrypted as it is written, and recorded by key in
// BackupDigestsFile once its backup is whole: a restore loads only what matches its record.
type backupStore interface {
	// where names the store in messages: the destination, or the directory.
	where() string
	// shown names an object in messages; key is its record's name in BackupDigestsFile.
	shown(obj string) string
	key(obj string) string
	// put writes obj with what write writes (the encrypted bytes), and answers their size and
	// SHA-256, as the store then holds them; nothing is left under obj when it fails.
	put(ctx context.Context, obj string, write func(io.Writer) error, logf func(string, ...any)) (int64, string, error)
	// discard removes obj, logging what it could not.
	discard(ctx context.Context, obj string, logf func(string, ...any))
	// size is obj's size; errObjectAbsent when the store holds none.
	size(ctx context.Context, obj string) (int64, error)
	// fetch gives obj's bytes as a file read from its start, their SHA-256, and what removes what
	// it made; copies says whether it copies the object into stage first (a destination's) rather
	// than read it where it is.
	fetch(ctx context.Context, stage, obj string) (*os.File, string, func(), error)
	copies() bool
	// list lists the project's backups (their last object), the profile a later release dropped
	// marked.
	list(ctx context.Context) ([]BackupObject, error)
	// prune deletes the profile's backups the settings no longer keep, and every volume archive
	// whose backup is gone.
	prune(ctx context.Context, profile string) error
}

// errObjectAbsent is an object the store does not hold.
var errObjectAbsent = errors.New("not found")

// localPrefix is the key of a backup's object held on the host: one name whichever of the
// install's directories holds it, so a backup streamed out and given back is the one recorded.
const localPrefix = "local:"

// The install's directories of backups held on the host, root's alone: LocalBackupsDir keeps
// those of `keep_local` settings; its outputDir the one `backup run --output` streams out (one at
// a time, emptied as the next starts and as the daemon starts); restoreInput, in the install's
// directory, the archive `restore --input` was given (removed as the restore ends, and as the
// next one or the daemon starts).
func LocalBackupsDir(dir string) string { return filepath.Join(dir, "backups") }

const (
	outputDir    = ".output"
	restoreInput = ".stowage-restore-input"
	// partialSuffix marks an object still being written: renamed to its name once whole.
	partialSuffix = ".partial"
	// maxKeepLocal bounds keep_local.
	maxKeepLocal = 100
)

// outputBackups is the directory of the backups `backup run --output` streams out, each in a
// directory of its own (outputOf), so one taken while another streams out leaves it be.
func outputBackups(dir string) string { return filepath.Join(LocalBackupsDir(dir), outputDir) }

// outputOf is the directory of the backup obj taken to be streamed out.
func outputOf(dir, obj string) string {
	base, _ := backupBase(obj)
	return filepath.Join(outputBackups(dir), base)
}

// exportLock is the lock file, in the host's directory of backups, every export holds shared while
// it streams; a new backup streamed out sweeping the earlier ones, and keep_local's pruning, take
// it exclusive without waiting, so neither cuts an export short nor holds a job for one.
func exportLock(dir string) string { return filepath.Join(dir, ".export.lock") }

// rcloneStore is the settings' destination, reached by the backup runner with the rclone config
// in conf.
type rcloneStore struct {
	h         *Hostd
	in        *Installer
	conf      string
	dest      string
	retention int // days; 0 keeps every backup
}

func (s rcloneStore) where() string           { return s.dest }
func (s rcloneStore) shown(obj string) string { return within(s.dest, obj) }
func (s rcloneStore) key(obj string) string   { return within(s.dest, obj) }
func (s rcloneStore) copies() bool            { return true }

func (s rcloneStore) put(ctx context.Context, obj string, write func(io.Writer) error, logf func(string, ...any)) (int64, string, error) {
	h, in, dest := s.h, s.in, s.shown(obj)
	pr, pw := io.Pipe()
	// The producer's own error does not say whether the upload stopped first (the CLI reports
	// its exit, not the broken pipe): its writes do.
	watched := &pipeWatch{w: pw}
	produced := make(chan error, 1)
	go func() {
		err := write(watched)
		pw.CloseWithError(err)
		produced <- err
	}()
	// What rclone read is what it uploaded: its SHA-256 is the object's, which the destination holds.
	sum := sha256.New()
	sent := &counter{r: io.TeeReader(pr, sum)}
	_, upErr := h.rcloneNamed(ctx, in, s.conf, h.runnerName(), sent, "rcat", dest)
	// An upload that stopped early leaves the producer nothing to write to.
	pr.CloseWithError(errUploadEnded)
	prodErr := <-produced
	ctxErr := ctx.Err() // before the cleanup, which may outlast the deadline
	// A failed upload leaves nothing under its name: rclone may have taken the stream's end for
	// the file's, or a backend kept what it was sent.
	if prodErr != nil || upErr != nil {
		s.discard(ctx, obj, logf)
		if err := cutShort(ctxErr); err != nil {
			return 0, "", err
		}
		if prodErr != nil && !watched.ended.Load() {
			// The producer failed on its own: the upload's error is its echo.
			return 0, "", prodErr
		}
		// The upload ended first (rclone may exit 0 before reading to the end).
		return 0, "", fmt.Errorf("the upload: %w", cmp.Or(upErr, errUploadEnded))
	}
	n := sent.n.Load()
	size, err := s.size(ctx, obj)
	switch {
	case errors.Is(err, errObjectAbsent):
		s.discard(ctx, obj, logf)
		return n, "", fmt.Errorf("the destination holds no %s, not the %d bytes sent", dest, n)
	case err != nil:
		s.discard(ctx, obj, logf)
		return n, "", fmt.Errorf("the uploaded backup could not be checked: %w", err)
	case size != n:
		s.discard(ctx, obj, logf)
		return n, "", fmt.Errorf("the destination holds %d bytes of %s, not the %d bytes sent", size, dest, n)
	}
	return n, hex.EncodeToString(sum.Sum(nil)), nil
}

// cutShort is what a backup whose context ended says: the hour's limit, or the daemon stopping;
// nil while it runs.
func cutShort(ctxErr error) error {
	switch {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		return fmt.Errorf("the backup did not finish within %s", backupTimeout)
	case ctxErr != nil:
		return errors.New("the backup was stopped: the host daemon is stopping")
	}
	return nil
}

func (s rcloneStore) discard(ctx context.Context, obj string, logf func(string, ...any)) {
	s.h.discard(ctx, s.in, s.conf, s.shown(obj), logf)
}

func (s rcloneStore) size(ctx context.Context, obj string) (int64, error) {
	out, err := s.h.rclone(ctx, s.in, s.conf, nil, "lsjson", "--files-only", "--no-mimetype", s.shown(obj))
	if err != nil {
		return 0, err
	}
	var found []struct{ Size int64 }
	if err := json.Unmarshal(out, &found); err != nil {
		return 0, fmt.Errorf("the destination's list: %w", err)
	}
	if len(found) != 1 {
		return 0, errObjectAbsent
	}
	return found[0].Size, nil
}

func (s rcloneStore) fetch(ctx context.Context, stage, obj string) (*os.File, string, func(), error) {
	f, err := os.CreateTemp(stage, "object-*")
	if err != nil {
		return nil, "", nil, err
	}
	done := func() { f.Close(); os.Remove(f.Name()) }
	hash := sha256.New()
	if _, err := s.h.rcloneTo(ctx, s.in, s.conf, s.h.runnerName(), nil, io.MultiWriter(f, hash), "cat", s.shown(obj)); err != nil {
		done()
		return nil, "", nil, fmt.Errorf("downloading %s: %w", s.shown(obj), err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		done()
		return nil, "", nil, err
	}
	return f, hex.EncodeToString(hash.Sum(nil)), done, nil
}

func (s rcloneStore) list(ctx context.Context) ([]BackupObject, error) {
	h := s.h
	out, err := h.rclone(ctx, s.in, s.conf, nil, "lsjson", "--files-only", "--no-mimetype", "--max-depth", "1", "--include", ownBackups(h.R.Spec.Name, anyProfile), s.dest)
	if err != nil {
		return nil, fmt.Errorf("the destination: %w", err)
	}
	var found []struct {
		Name    string
		Size    int64
		ModTime time.Time
	}
	if err := json.Unmarshal(out, &found); err != nil {
		return nil, fmt.Errorf("the destination's list: %w", err)
	}
	var list []BackupObject
	for _, f := range found {
		list = append(list, BackupObject{Name: f.Name, Size: f.Size, Time: f.ModTime})
	}
	return list, nil
}

func (s rcloneStore) prune(ctx context.Context, profile string) error {
	if s.retention != 0 {
		if _, err := s.h.rclone(ctx, s.in, s.conf, nil, "delete", "--min-age", fmt.Sprintf("%dd", s.retention), "--max-depth", "1", "--include", ownBackups(s.h.R.Spec.Name, profile), s.dest); err != nil {
			return err
		}
	}
	// Orphan archives go whatever the retention and the volumes now listed: a failed backup whose
	// cleanup failed left them, or retention took a dump an earlier release archived volumes with.
	return s.h.pruneArchives(ctx, s.in, s.conf, s.dest, profile)
}

// localStore is a directory of the install's on the host: dir holds the objects, 0600 in a 0700
// directory, each written under its name with partialSuffix and renamed once whole. keep is how
// many of a profile's backups prune keeps (0: prune deletes none).
type localStore struct {
	name string // the project's
	dir  string
	keep int
	// disk reads the space of a directory's filesystem (Hostd.diskAt); nil: put does not watch it.
	disk func(string) *Disk
}

// diskReserve is the space a write on the host leaves free on the filesystem the stack's data
// shares: a twentieth of it.
func diskReserve(d *Disk) uint64 { return d.Total / 20 }

// diskWatchEvery is how many bytes put writes between two reads of the disk's space (a variable:
// tests shorten it).
var diskWatchEvery int64 = 64 << 20

// roomFor refuses size bytes more on dir's filesystem when they would leave it less than
// diskReserve free; nil when its space cannot be read.
func roomFor(d *Disk, dir string, size uint64) error {
	if d == nil || size+diskReserve(d) <= d.Available {
		return nil
	}
	return fmt.Errorf("the disk of %s has %d bytes available, and %d more would leave it less than the %d kept free for the stack's data (free space or grow the disk)", dir, d.Available, size, diskReserve(d))
}

func (s localStore) where() string           { return s.dir }
func (s localStore) shown(obj string) string { return filepath.Join(s.dir, obj) }
func (s localStore) key(obj string) string   { return localPrefix + obj }
func (s localStore) copies() bool            { return false }

func (s localStore) put(ctx context.Context, obj string, write func(io.Writer) error, logf func(string, ...any)) (int64, string, error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return 0, "", err
	}
	if err := os.Chmod(s.dir, 0o700); err != nil {
		return 0, "", err
	}
	part := s.shown(obj) + partialSuffix
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, "", err
	}
	sum := sha256.New()
	cw := &countWriter{w: io.MultiWriter(f, sum)}
	if s.disk != nil {
		// The disk is the stack's data's too: the write stops before it fills it.
		cw.check = func() error { return roomFor(s.disk(s.dir), s.dir, 0) }
		if err := cw.check(); err != nil {
			f.Close()
			s.discard(ctx, obj, logf)
			return 0, "", err
		}
	}
	err = write(cw)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(part, s.shown(obj))
	}
	if err == nil {
		err = syncDir(s.dir)
	}
	if err != nil {
		s.discard(ctx, obj, logf)
		if cerr := cutShort(ctx.Err()); cerr != nil {
			return 0, "", cerr
		}
		return 0, "", err
	}
	return cw.n, hex.EncodeToString(sum.Sum(nil)), nil
}

// countWriter counts what it writes through, and runs check (when set) every diskWatchEvery
// bytes, failing the write on its error.
type countWriter struct {
	w       io.Writer
	n, last int64
	check   func() error
}

func (c *countWriter) Write(p []byte) (int, error) {
	if c.check != nil && c.n-c.last >= diskWatchEvery {
		c.last = c.n
		if err := c.check(); err != nil {
			return 0, err
		}
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// syncDir makes a rename in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s localStore) discard(_ context.Context, obj string, logf func(string, ...any)) {
	for _, p := range []string{s.shown(obj), s.shown(obj) + partialSuffix} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			logf("%s may be left incomplete: removing it failed (%v)", p, err)
		}
	}
}

func (s localStore) size(_ context.Context, obj string) (int64, error) {
	fi, err := os.Lstat(s.shown(obj))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 0, errObjectAbsent
	case err != nil:
		return 0, err
	case !fi.Mode().IsRegular():
		return 0, fmt.Errorf("%s is not a file", s.shown(obj))
	}
	return fi.Size(), nil
}

func (s localStore) fetch(_ context.Context, _ string, obj string) (*os.File, string, func(), error) {
	f, err := openRegular(s.shown(obj))
	if err != nil {
		return nil, "", nil, err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		f.Close()
		return nil, "", nil, fmt.Errorf("reading %s: %w", s.shown(obj), err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, "", nil, err
	}
	return f, hex.EncodeToString(hash.Sum(nil)), func() { f.Close() }, nil
}

// openRegular opens path for reading, refusing anything but a regular file (a link included).
func openRegular(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	// The file opened is the one checked: not one a rename put there since.
	fi, err := f.Stat()
	if err == nil && !os.SameFile(before, fi) {
		err = fmt.Errorf("%s changed as it was opened", path)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// localEntry is one of the project's objects in a local store.
type localEntry struct {
	name string
	size int64
	at   time.Time // the backup's time, as its name gives it
}

// entries lists the project's objects in the directory (none when it does not exist): the
// backups' last objects (last), and their volumes' archives (archives).
func (s localStore) entries() (last, archives []localEntry, err error) {
	des, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, de := range des {
		n := de.Name()
		if !de.Type().IsRegular() || !strings.HasPrefix(n, s.name+"-") {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue // gone meanwhile
		}
		at, ok := backupTime(s.name, n)
		if !ok {
			continue
		}
		e := localEntry{n, fi.Size(), at}
		switch {
		case matchesOwnBackup(s.name, n):
			last = append(last, e)
		case strings.HasSuffix(n, ".tar.age"):
			archives = append(archives, e)
		}
	}
	return last, archives, nil
}

// backupTime is the UTC time a backup's object names (its profile's, then its time's field).
func backupTime(name, obj string) (time.Time, bool) {
	rest, ok := strings.CutPrefix(obj, name+"-")
	if !ok {
		return time.Time{}, false
	}
	_, rest, ok = strings.Cut(rest, "-")
	if !ok || len(rest) < 16 {
		return time.Time{}, false
	}
	at, err := time.Parse("20060102T150405Z", rest[:16])
	return at, err == nil
}

func (s localStore) list(context.Context) ([]BackupObject, error) {
	last, _, err := s.entries()
	if err != nil {
		return nil, err
	}
	var list []BackupObject
	for _, e := range last {
		list = append(list, BackupObject{Name: e.name, Size: e.size, Time: e.at, Local: true})
	}
	return list, nil
}

func (s localStore) prune(_ context.Context, profile string) error {
	unlock, err := lockFile(exportLock(s.dir))
	if errors.Is(err, errLocked) {
		return errors.New("a backup kept on the host is streaming out (backup export): the next backup prunes them")
	}
	if err != nil {
		return err
	}
	defer unlock()
	last, archives, err := s.entries()
	if err != nil {
		return err
	}
	var errs []error
	remove := func(n string) {
		if err := os.Remove(filepath.Join(s.dir, n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	var mine []localEntry
	for _, e := range last {
		if backupProfile(s.name, e.name) == profile {
			mine = append(mine, e)
		}
	}
	// The newest first, by time then name.
	sort.Slice(mine, func(i, j int) bool {
		if !mine[i].at.Equal(mine[j].at) {
			return mine[i].at.After(mine[j].at)
		}
		return mine[i].name > mine[j].name
	})
	present := map[string]bool{}
	for _, e := range last {
		present[e.name] = true
	}
	for i, e := range mine {
		if s.keep > 0 && i >= s.keep {
			// Its volumes' archives with it: they are its alone.
			base, _ := backupBase(e.name)
			for _, a := range archives {
				if strings.HasPrefix(a.name, base+".") {
					remove(a.name)
				}
			}
			remove(e.name)
			delete(present, e.name)
		}
	}
	var bases []string
	for n := range present {
		if base, ok := backupBase(n); ok {
			bases = append(bases, base+".")
		}
	}
	// An archive whose backup is gone goes, once old enough not to be a backup's still running.
	for _, a := range archives {
		if slices.ContainsFunc(bases, func(b string) bool { return strings.HasPrefix(a.name, b) }) {
			continue
		}
		if backupProfile(s.name, a.name) == profile && time.Since(a.at) >= 2*backupTimeout {
			remove(a.name)
		}
	}
	return errors.Join(errs...)
}

// sweepPartial removes what a backup cut off left half written in dir.
func sweepPartial(dir string) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, de := range des {
		if strings.HasSuffix(de.Name(), partialSuffix) {
			os.Remove(filepath.Join(dir, de.Name()))
		}
	}
}

// localObjects is the objects of the backup obj in s, as a backup writes them: the volumes'
// archives (those of the release's volumes in their order, then any other the store holds, by
// name), then obj itself.
func (s localStore) localObjects(obj string, pg *providers.AgeRclone) ([]localEntry, error) {
	base, ok := backupBase(obj)
	if !ok {
		return nil, fmt.Errorf("%s is no backup's last object", obj)
	}
	_, archives, err := s.entries()
	if err != nil {
		return nil, err
	}
	var mine []localEntry
	for _, a := range archives {
		if strings.HasPrefix(a.name, base+".") {
			mine = append(mine, a)
		}
	}
	order := func(n string) int {
		for i, v := range pg.Volumes {
			if n == volumeObject(obj, v) {
				return i
			}
		}
		return len(pg.Volumes)
	}
	sort.SliceStable(mine, func(i, j int) bool {
		if oi, oj := order(mine[i].name), order(mine[j].name); oi != oj {
			return oi < oj
		}
		return mine[i].name < mine[j].name
	})
	size, err := s.size(context.Background(), obj)
	if err != nil {
		return nil, err
	}
	at, _ := backupTime(s.name, obj)
	return append(mine, localEntry{obj, size, at}), nil
}

// writeArchive writes the backup obj of s to w as one tar (ustar where the names fit): its
// objects, as stored, in localObjects' order, each 0600, owned by 0:0, timed as the backup's
// name says — the same backup always gives the same bytes.
func (s localStore) writeArchive(w io.Writer, obj string, pg *providers.AgeRclone) error {
	objs, err := s.localObjects(obj, pg)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	for _, o := range objs {
		f, err := openRegular(s.shown(o.name))
		if err != nil {
			return err
		}
		fi, err := f.Stat()
		if err == nil {
			err = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: o.name, Size: fi.Size(), Mode: 0o600, ModTime: o.at})
		}
		if err == nil {
			var n int64
			n, err = io.Copy(tw, f)
			if err == nil && n != fi.Size() {
				err = fmt.Errorf("%s changed as it was read", s.shown(o.name))
			}
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

// ageEncrypt is write's writer for obj's bytes, produced by produce and encrypted to recips.
func ageEncrypt(recips []age.Recipient, produce func(io.Writer) error) func(io.Writer) error {
	return func(w io.Writer) error {
		enc, err := age.Encrypt(w, recips...)
		if err != nil {
			return err
		}
		if err := produce(enc); err != nil {
			return err
		}
		return enc.Close()
	}
}
