package install

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/norvexlabs/stowage/internal/providers"
)

// Backups pulled over SSH: a tool reaching the host pulls each backup into a store of its own, and
// gives one back to restore, while the host holds no credential to any storage.
//
//	GET  /v1/backup/export?object=O     the backup O kept on the host, as one tar stream
//	GET  /v1/backup/export?object=O&output=1
//	                                    the backup `POST /v1/backup {"output": true}` took, as
//	                                    one tar stream; it stays until the next one's job sweeps it,
//	                                    or the daemon starts
//	POST /v1/restore/input              a RestoreRequest's JSON on one line, then a backup's
//	                                    tar stream: the restore of that backup (a job)
//
// The stream (writeArchive) is a tar of the backup's objects as a store holds them, encrypted,
// the volumes' archives first and the dump (or the manifest) last; any corrupted or truncated
// object fails its age decryption, and an archive cut before its last object is refused.

// exportBackup streams a backup held on the host as one tar. A failure once the stream started
// aborts the answer: the client reads it cut short, never as a whole archive.
func (h *Hostd) exportBackup(w http.ResponseWriter, r *http.Request) {
	pg, err := h.pgBackup()
	if err != nil {
		replyErr(w, err)
		return
	}
	q := r.URL.Query()
	obj, output := q.Get("object"), q.Get("output") == "1"
	name := h.R.Spec.Name
	if !matchesOwnBackup(name, obj) {
		replyErr(w, refuseAs(http.StatusBadRequest, "", "%q is not a backup of %s (`backup list` shows them)", obj, name))
		return
	}
	// Read under the shared lock: a new backup streamed out does not sweep it, nor a pruning
	// prune it, meanwhile.
	streamed := localStore{name: name, dir: outputOf(h.O.Dir, obj)}
	if _, err := os.Stat(LocalBackupsDir(h.O.Dir)); err == nil {
		unlock, err := flockWait(r.Context(), exportLock(LocalBackupsDir(h.O.Dir)), false)
		if err != nil {
			replyErr(w, err)
			return
		}
		defer unlock()
	}
	// output: the one just taken; otherwise one kept, else one streamed out that is still there
	// (its stream failed), which stays.
	st := streamed
	if !output {
		st = localStore{name: name, dir: LocalBackupsDir(h.O.Dir)}
	}
	_, err = st.localObjects(obj, pg)
	if !output && errors.Is(err, errObjectAbsent) {
		st = streamed
		_, err = st.localObjects(obj, pg)
	}
	if errors.Is(err, errObjectAbsent) || errors.Is(err, os.ErrNotExist) {
		if output {
			replyErr(w, refuseAs(http.StatusNotFound, "", "the backup %s taken to be streamed out is gone (another was taken since, or the host daemon restarted): back up again", obj))
		} else {
			replyErr(w, refuseAs(http.StatusNotFound, "", "%s is not among the backups kept on the host (`backup list` marks them local), nor one streamed out still there", obj))
		}
		return
	} else if err != nil {
		replyErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		h.O.logf("streaming the backup %s out: %v", obj, err)
		panic(http.ErrAbortHandler)
	}
	// The backup stays on the host until the next one streamed out, or the daemon's start, sweeps
	// it: whether the client wrote it whole, only the client knows.
	// A client that reads nothing for exportStall ends it: the lock it holds is never held for good.
	err = st.writeArchive(deadlineWriter{w, rc}, obj, pg)
	if err == nil {
		err = rc.Flush()
	}
	if err != nil {
		h.O.logf("streaming the backup %s out: %v", obj, err)
		panic(http.ErrAbortHandler)
	}
}

// exportStall is how long an export waits for its client to take a write.
var exportStall = 10 * time.Minute

// deadlineWriter writes to w, each write bounded by exportStall where the connection takes a
// deadline (a socket's; the in-process transport's does not).
type deadlineWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (d deadlineWriter) Write(p []byte) (int, error) {
	if err := d.rc.SetWriteDeadline(time.Now().Add(exportStall)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return 0, err
	}
	return d.w.Write(p)
}

// maxRestoreLine bounds the request line before a restore's archive.
const maxRestoreLine = 64 << 10

// restoreInput restores the backup of the archive its body carries after the request's line: the
// archive is read into restoreInput (refused unless it is exactly one backup of the release the
// install runs, whole), then restored from there as a backup kept on the host is, its digests
// checked against the install's records (`--unrecorded` for one another install took).
func (h *Hostd) restoreInput(w http.ResponseWriter, r *http.Request) {
	br := bufio.NewReaderSize(r.Body, maxRestoreLine)
	line, err := br.ReadSlice('\n')
	if err != nil {
		replyErr(w, refuseAs(http.StatusBadRequest, "", "the request: a RestoreRequest's JSON on one line of at most %d bytes, then the archive", maxRestoreLine))
		return
	}
	var req RestoreRequest
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || !trailingSpace(dec) {
		replyErr(w, refuseAs(http.StatusBadRequest, "", "the request's line is no restore request: %v", err))
		return
	}
	if req.Object != "" {
		replyErr(w, refuseAs(http.StatusBadRequest, "", "object is the archive's: give none"))
		return
	}
	in, pg, target, ids, err := h.restoreChecks(r, req)
	if err != nil {
		replyErr(w, err)
		return
	}
	if err := h.judged(r.Context()); err != nil {
		replyErr(w, err)
		return
	}
	// Refused before the archive is read: a job running would refuse it once read whole.
	h.mu.Lock()
	err = h.slotRefusal(nil)
	h.mu.Unlock()
	if err != nil {
		replyErr(w, err)
		return
	}
	if !h.inputMu.TryLock() {
		replyErr(w, refuseAs(http.StatusConflict, "", "another restore of an archive given is running: restore again once it is done"))
		return
	}
	dir := in.path(restoreInput)
	release := func() {
		os.RemoveAll(dir)
		h.inputMu.Unlock()
	}
	obj, err := h.importArchive(r.Context(), br, dir, pg, target)
	if err != nil {
		release()
		replyErr(w, err)
		return
	}
	req.Object = obj
	h.startRestore(w, in, pg, req, ids, dir, release)
}

// importArchive reads a backup's archive from r into dir, made afresh (root's alone): exactly the
// objects of one backup of name taken from the release target, in the order writeArchive gives
// them, each a file the disk has room for, and nothing after. It answers the backup's last
// object. Everything it refuses is a refusal: nothing was restored.
func (h *Hostd) importArchive(ctx context.Context, r io.Reader, dir string, pg *providers.AgeRclone, target Version) (string, error) {
	name := h.R.Spec.Name
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	refused := func(format string, a ...any) error {
		return refuseAs(http.StatusBadRequest, "", "the archive: "+format+": nothing restored", a...)
	}
	tr := tar.NewReader(r)
	first, err := tr.Next()
	switch {
	case errors.Is(err, io.EOF):
		return "", refused("it is empty")
	case err != nil:
		return "", refused("it does not read as a backup's archive (%v)", err)
	}
	// Its first object names the backup: the first volume's archive, else the front's (frontObject),
	// else the backup's last object.
	obj := first.Name
	if len(pg.Volumes) > 0 {
		base, ok := strings.CutSuffix(first.Name, "."+pg.Volumes[0]+".tar.age")
		if !ok {
			return "", refused("it begins with %q, not the archive of the volume %s that a backup of %s begins with", first.Name, pg.Volumes[0], name)
		}
		obj = base + lastSuffix(pg)
	} else if base, ok := strings.CutSuffix(first.Name, "."+FrontDataVolume+".tar.age"); ok {
		obj = base + lastSuffix(pg)
	}
	if err := ownBackup(name, obj, target, pg); err != nil {
		return "", err
	}
	// The volumes' archives, the front's when the backup took one (its install in an ACME mode),
	// then the last object.
	front := frontObject(obj)
	want := []string{}
	for _, v := range pg.Volumes {
		want = append(want, volumeObject(obj, v))
	}
	want = append(want, front, obj)
	hdr, read := first, true // read: hdr is the next member's, not yet taken
	for _, o := range want {
		if !read {
			hdr, err = tr.Next()
			switch {
			case errors.Is(err, io.EOF):
				if o == front {
					o = obj
				}
				return "", refused("it ends before %s: it is cut short", o)
			case err != nil:
				return "", refused("it does not read as a backup's archive (%v)", err)
			}
		}
		read = false
		if o == front && hdr.Name != front {
			read = true // none: hdr is the last object's
			continue
		}
		switch {
		case hdr.Name != o:
			return "", refused("it holds %q where %s comes", hdr.Name, o)
		case hdr.Typeflag != tar.TypeReg || hdr.Size < 0:
			return "", refused("%s is not a file", o)
		}
		// The disk is the stack's data's too: what is written leaves it its reserve.
		if err := roomFor(h.diskAt(dir), dir, uint64(hdr.Size)); err != nil {
			return "", refused("%s is %d bytes: %v", o, hdr.Size, err)
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := writeObject(filepath.Join(dir, o), tr); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return "", refused("%s is cut short", o)
			}
			return "", fmt.Errorf("the archive: %s: %w: nothing restored", o, err)
		}
	}
	if hdr, err := tr.Next(); err == nil {
		return "", refused("it holds %q after %s, the backup's last object", hdr.Name, obj)
	} else if !errors.Is(err, io.EOF) {
		return "", refused("it does not end as an archive (%v)", err)
	}
	// Past the archive's end, a record's padding of zeros alone.
	rest, err := io.ReadAll(io.LimitReader(r, maxRestoreLine+1))
	if err != nil {
		return "", err
	}
	if len(rest) > maxRestoreLine || bytes.ContainsFunc(rest, func(c rune) bool { return c != 0 }) {
		return "", refused("data follows its end")
	}
	return obj, nil
}

// writeObject writes r, a tar member (which ends short with io.ErrUnexpectedEOF), to path (0600,
// new), made durable.
func writeObject(path string, r io.Reader) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
