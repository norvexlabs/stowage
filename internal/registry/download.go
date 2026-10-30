package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// TitleAnnotation names the file an artifact's layer is written to.
const TitleAnnotation = "org.opencontainers.image.title"

// Download writes the one layer of the artifact at d — of artifactType, its layer of type layer,
// or layer+ZstdSuffix, then decompressed as it is written and its title's ".zst" dropped — into
// dir, under the layer's title (a plain file name), else fallback, and returns its path and the
// file's digest (what the file's bytes hash to: a compressed layer's UncompressedAnnotation, checked). The
// manifest is checked against d's digest (by go-containerregistry's read), and the layer's bytes
// against its digest and size as they are read; the file reaches its name only once read whole and
// checked. A layer read cut short is resumed where it stopped (see blobReader). A file of that name already
// holding the layer is kept, not fetched again; any other is refused, never replaced. The file is
// 0644: what it holds is published, and a program running as another user (a hypervisor) reads
// it. progress, when set, is called as bytes arrive.
func (c *Client) Download(ctx context.Context, d name.Digest, artifactType, layer types.MediaType, dir, fallback string, progress func(done, total int64)) (string, v1.Hash, error) {
	desc, err := c.Get(ctx, d)
	if err != nil {
		return "", v1.Hash{}, err
	}
	if desc == nil {
		return "", v1.Hash{}, fmt.Errorf("%s does not exist", d)
	}
	var m struct {
		ArtifactType types.MediaType `json:"artifactType"`
		Layers       []v1.Descriptor `json:"layers"`
	}
	if err := json.Unmarshal(desc.Manifest, &m); err != nil {
		return "", v1.Hash{}, fmt.Errorf("%s: manifest: %w", d, err)
	}
	if m.ArtifactType != artifactType || len(m.Layers) != 1 || (m.Layers[0].MediaType != layer && m.Layers[0].MediaType != layer+ZstdSuffix) {
		return "", v1.Hash{}, fmt.Errorf("%s is not a %s artifact with one %s layer", d, artifactType, layer)
	}
	l := m.Layers[0]
	compressed := l.MediaType == layer+ZstdSuffix
	want, size, title := l.Digest, l.Size, l.Annotations[TitleAnnotation]
	if compressed {
		if want, err = uncompressedDigest(l); err != nil {
			return "", v1.Hash{}, fmt.Errorf("%s: %w", d, err)
		}
		size, title = -1, strings.TrimSuffix(title, ".zst")
	}
	file := fallback
	if plainName(title) {
		file = title
	}
	path := filepath.Join(dir, file)
	switch ok, err := holds(path, want, size); {
	case err != nil:
		return "", v1.Hash{}, err
	case ok:
		return path, want, nil
	}
	rc, err := c.openBlob(ctx, d.Context(), l)
	if err != nil {
		return "", v1.Hash{}, fmt.Errorf("%s: %w", d, err)
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(dir, "."+file+".part-*")
	if err != nil {
		return "", v1.Hash{}, err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	var r io.Reader = rc
	if progress != nil {
		r = &counter{r: r, total: l.Size, progress: progress}
	}
	err = write(tmp, r, compressed, want)
	if err == nil {
		err = tmp.Chmod(0o644)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", v1.Hash{}, fmt.Errorf("%s: %w", d, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", v1.Hash{}, err
	}
	return path, want, nil
}

// write copies the layer r into w: as it is, or decompressed, its bytes then checked against want.
// The layer is read to its end either way, where its own digest is checked (by blobReader).
func write(w io.Writer, r io.Reader, compressed bool, want v1.Hash) error {
	if !compressed {
		_, err := io.Copy(w, r)
		return err
	}
	dec, err := unzstd(r)
	if err != nil {
		return err
	}
	defer dec.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), dec); err != nil {
		return fmt.Errorf("decompress: %w", err)
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != want.String() {
		return fmt.Errorf("decompressed, the layer is %s, not %s", got, want)
	}
	return nil
}

// plainName reports whether t is a file name with no directory in it.
func plainName(t string) bool {
	return t != "" && t != "." && t != ".." && !strings.ContainsAny(t, `/\`) && filepath.Base(t) == t
}

// holds reports whether the file at path is the file of digest want (and size, unless it is
// negative): false when there is none, an error when it is something else.
func holds(path string, want v1.Hash, size int64) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !st.Mode().IsRegular() {
		return false, fmt.Errorf("%s exists and is not a file", path)
	}
	h := sha256.New()
	if size < 0 || st.Size() == size {
		if _, err := io.Copy(h, f); err != nil {
			return false, err
		}
		if "sha256:"+hex.EncodeToString(h.Sum(nil)) == want.String() {
			return true, nil
		}
	}
	return false, fmt.Errorf("%s exists and is not %s: move it away", path, want)
}

// counter reports the bytes read through it.
type counter struct {
	r        io.Reader
	done     int64
	total    int64
	progress func(done, total int64)
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.done += int64(n)
	if n > 0 {
		c.progress(c.done, c.total)
	}
	return n, err
}

// A layer's download, in downloadUnit: tests shorten the unit.
var downloadUnit = time.Second

const (
	// downloadTries is how many attempts in a row may fail with no new byte before a download
	// fails.
	downloadTries = 5
	// downloadWait is the first wait before a resumed read, doubled on each failure in a row up
	// to downloadWaitMax.
	downloadWait    = 2
	downloadWaitMax = 30
	// downloadStall is how long a stream open goes with no byte before it is dropped and resumed:
	// a connection that stays up and sends nothing never ends a read by itself.
	downloadStall = 120
)

// errStalled is a stream dropped after downloadStall with no byte.
var errStalled = fmt.Errorf("no byte for %d s", downloadStall)

// openBlob opens the layer l of repo for reading, through a blobReader.
func (c *Client) openBlob(ctx context.Context, repo name.Repository, l v1.Descriptor) (*blobReader, error) {
	if l.Digest.Algorithm != "sha256" || l.Size < 0 {
		return nil, fmt.Errorf("layer %s of %d bytes: not a sha256 digest and a size", l.Digest, l.Size)
	}
	auth, err := authn.Resolve(ctx, c.keychain, repo)
	if err != nil {
		return nil, err
	}
	rt, err := transport.NewWithContext(ctx, repo.Registry, auth, c.rt, []string{repo.Scope(transport.PullScope)})
	if err != nil {
		return nil, err
	}
	label := l.Digest.String()
	if t := l.Annotations[TitleAnnotation]; t != "" {
		label = t + " (" + label + ")"
	}
	return &blobReader{
		ctx:   ctx,
		hc:    &http.Client{Transport: rt},
		url:   &url.URL{Scheme: repo.Registry.Scheme(), Host: repo.RegistryStr(), Path: "/v2/" + repo.RepositoryStr() + "/blobs/" + l.Digest.String()},
		want:  l.Digest,
		size:  l.Size,
		h:     sha256.New(),
		log:   c.log,
		label: label,
		wait:  downloadWait * downloadUnit,
	}, nil
}

// blobReader reads a blob of known digest and size from the registry. A read cut short (an error,
// or the stream ending early) is resumed at the byte reached, by a ranged GET (Range: bytes=N-):
// a 206 answer goes on from there, a 200 (a server ignoring the range) is read from its start
// with the first N bytes skipped. After downloadTries attempts in a row with no new byte, a
// refusal no retry changes (any 4xx but 408 and 429) or the context's end, the read fails. It
// returns io.EOF only once size bytes are read and they hash to the digest; it never reads past
// size.
type blobReader struct {
	ctx  context.Context
	hc   *http.Client
	url  *url.URL
	want v1.Hash
	size int64
	off  int64
	h    hash.Hash
	body io.ReadCloser
	// idle drops the stream open (cancel) once it brought no byte for downloadStall; stalled
	// tells it did.
	idle    *time.Timer
	cancel  context.CancelFunc
	stalled atomic.Bool
	fails   int   // attempts in a row with no new byte
	skipped int64 // the furthest a skip of a range ignored went
	got     bool  // the stream open brought a byte
	wait    time.Duration
	log     io.Writer
	label   string
}

func (b *blobReader) Read(p []byte) (int, error) {
	for {
		if b.off == b.size {
			if got := "sha256:" + hex.EncodeToString(b.h.Sum(nil)); got != b.want.String() {
				return 0, fmt.Errorf("blob %s: its %d bytes are %s", b.label, b.size, got)
			}
			return 0, io.EOF
		}
		if len(p) == 0 {
			return 0, nil
		}
		var err error
		if b.body == nil {
			err = b.open()
		}
		if err == nil {
			var n int
			n, err = b.watched(b.body, p[:min(int64(len(p)), b.size-b.off)])
			if n > 0 {
				b.h.Write(p[:n])
				b.off += int64(n)
				b.fails, b.got, b.wait = 0, true, downloadWait*downloadUnit
				if err != nil {
					b.closeBody()
				}
				return n, nil
			}
			if err == nil {
				continue
			}
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			b.closeBody()
		}
		if b.stalled.Swap(false) && b.ctx.Err() == nil {
			err = errStalled
		}
		if err := b.failed(err); err != nil {
			return 0, err
		}
	}
}

// failed counts a failed attempt and waits before the next one, or returns the error that ends
// the download.
func (b *blobReader) failed(err error) error {
	if b.ctx.Err() != nil {
		return b.ctx.Err()
	}
	var te *transport.Error
	if errors.As(err, &te) && te.StatusCode < 500 && te.StatusCode != http.StatusRequestTimeout && te.StatusCode != http.StatusTooManyRequests {
		return fmt.Errorf("blob %s: %w", b.label, err)
	}
	if b.got {
		b.got = false // the stream that failed brought bytes: not an attempt with none
	} else if b.fails++; b.fails >= downloadTries {
		return fmt.Errorf("blob %s: %d attempts in a row with no byte read, at %d of %d bytes: %w", b.label, b.fails, b.off, b.size, err)
	}
	if b.log != nil {
		fmt.Fprintf(b.log, "download %s: %v; resuming at %d of %d bytes\n", b.label, err, b.off, b.size)
	}
	t := time.NewTimer(b.wait)
	defer t.Stop()
	select {
	case <-b.ctx.Done():
		return b.ctx.Err()
	case <-t.C:
	}
	b.wait = min(2*b.wait, downloadWaitMax*downloadUnit)
	return nil
}

// open GETs the blob from the byte reached.
// watched reads r under the idle watchdog: armed while the read waits, so the time a caller takes
// between reads never counts as a silent stream.
func (b *blobReader) watched(r io.Reader, p []byte) (int, error) {
	b.idle.Reset(downloadStall * downloadUnit)
	defer b.idle.Stop()
	return r.Read(p)
}

func (b *blobReader) open() error {
	ctx, cancel := context.WithCancel(b.ctx)
	b.cancel = cancel
	b.stalled.Store(false) // a watchdog of an earlier stream that fired late says nothing of this one
	b.idle = time.AfterFunc(downloadStall*downloadUnit, func() { b.stalled.Store(true); cancel() })
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url.String(), nil)
	if err != nil {
		b.closeBody()
		return err
	}
	if b.off > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(b.off, 10)+"-")
	}
	resp, err := b.hc.Do(req)
	b.idle.Stop()
	if err != nil {
		b.closeBody()
		return err
	}
	switch {
	case b.off > 0 && resp.StatusCode == http.StatusPartialContent:
		if cr := resp.Header.Get("Content-Range"); !strings.HasPrefix(cr, "bytes "+strconv.FormatInt(b.off, 10)+"-") {
			resp.Body.Close()
			b.closeBody()
			return fmt.Errorf("asked for bytes %d-, the registry sent %q", b.off, cr)
		}
	case b.off > 0 && resp.StatusCode == http.StatusOK:
		// Skipped under the watchdog read by read: a long skip that keeps bringing bytes goes on.
		n, err := io.CopyN(io.Discard, readerFunc(func(p []byte) (int, error) { return b.watched(resp.Body, p) }), b.off)
		// A skip that got further than any before is progress, though it brought no new byte.
		if n > b.skipped {
			b.skipped, b.got = n, true
		}
		if err != nil {
			resp.Body.Close()
			b.closeBody()
			return fmt.Errorf("skipping the %d bytes already read: %w", b.off, err)
		}
	default:
		if err := transport.CheckError(resp, http.StatusOK); err != nil {
			resp.Body.Close()
			b.closeBody()
			return err
		}
	}
	b.body = resp.Body
	return nil
}

// closeBody drops the stream open, or the one being opened, with its watchdog.
func (b *blobReader) closeBody() {
	if b.body != nil {
		b.body.Close()
		b.body = nil
	}
	if b.idle != nil {
		b.idle.Stop()
		b.cancel()
		b.idle, b.cancel = nil, nil
	}
}

// Close closes the stream open, if any.
func (b *blobReader) Close() error {
	b.closeBody()
	return nil
}

// readerFunc is a function as an io.Reader.
type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
