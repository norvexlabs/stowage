package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// A file blob's upload, in Unit: tests shorten the unit.
var (
	// uploadUnit scales every wait below.
	uploadUnit = time.Second
	// chunkMin and chunkMax bound a chunk; a registry that needs larger ones says so
	// (OCI-Chunk-Min-Length). Small enough that a chunk is acknowledged within uploadStall down
	// to about 4 KB/s.
	chunkMin int64 = 1 << 20
	chunkMax int64 = 64 << 20
)

const (
	// chunkTime is about how long a chunk takes, at the rate the last one went.
	chunkTime = 60
	// uploadStall is how long an upload goes on with no byte acknowledged, and a request with no
	// byte sent, before it fails.
	uploadStall = 5 * 60
	// retryWait is the first wait before a retry, doubled on each failure in a row up to retryMax.
	retryWait = 2
	retryMax  = 60
	// abortWait bounds the deletion of an upload given up.
	abortWait = 10
	// progressEvery is how often a push logs its progress.
	progressEvery = 30
)

// maxSessions bounds the upload sessions of one blob.
const maxSessions = 4

// errSessions is a registry that keeps losing an upload.
var errSessions = errors.New("the registry lost the upload")

// errChunkRefused is a registry refusing a chunk at the start of an upload: it takes a blob in one
// stream only.
var errChunkRefused = errors.New("the registry refuses chunked uploads")

// pushFile pushes a file blob in chunks (the OCI distribution spec's chunked upload): after a
// failure the upload resumes from the last byte the registry acknowledged, and it fails, naming
// the blob, when no byte is acknowledged for uploadStall. A registry that refuses chunks gets the
// blob in one stream.
func (c *Client) pushFile(ctx context.Context, repo name.Repository, b Blob) error {
	auth, err := authn.Resolve(ctx, c.keychain, repo)
	if err != nil {
		return err
	}
	rt, err := transport.NewWithContext(ctx, repo.Registry, auth, c.rt, []string{repo.Scope(transport.PushScope)})
	if err != nil {
		return err
	}
	f, err := os.Open(b.path)
	if err != nil {
		return err
	}
	defer f.Close()
	u := &upload{
		hc:    &http.Client{Transport: rt},
		repo:  repo,
		b:     b,
		f:     f,
		log:   c.log,
		label: cmpLabel(b),
		chunk: chunkMin,
		least: chunkMin,
	}
	err = u.run(ctx)
	if err != nil {
		u.abort(ctx)
	}
	if errors.Is(err, errChunkRefused) || errors.Is(err, errSessions) {
		u.logf("push %s: %v; pushing it in one stream", u.label, err)
		return remote.WriteLayer(repo, b.layer(), c.opts(ctx)...)
	}
	return err
}

// cmpLabel names a blob in messages: its title, with its digest.
func cmpLabel(b Blob) string {
	if t := b.Annotations[TitleAnnotation]; t != "" {
		return t + " (" + b.digest.String() + ")"
	}
	return b.digest.String()
}

type upload struct {
	hc       *http.Client
	repo     name.Repository
	b        Blob
	f        *os.File
	log      io.Writer
	label    string
	loc      *url.URL  // the upload session
	off      int64     // the bytes the registry acknowledged
	sessions int       // the sessions begun
	checked  bool      // the registry was asked whether it holds the blob
	ever     bool      // the registry acknowledged a chunk once
	patched  bool      // a chunk was sent in this session
	from     time.Time // the session began: the rate is measured from then
	lostAt   int64     // the bytes the last session lost held
	chunk    int64
	least    int64        // the smallest chunk but the last
	acked    time.Time    // when the registry last acknowledged a byte (or the upload began)
	sent     atomic.Int64 // when a request's body was last read, in Unix nanoseconds
}

func (u *upload) logf(format string, a ...any) {
	if u.log != nil {
		fmt.Fprintf(u.log, format+"\n", a...)
	}
}

func (u *upload) url(path string) *url.URL {
	return &url.URL{Scheme: "https", Host: u.repo.RegistryStr(), Path: "/v2/" + u.repo.RepositoryStr() + path}
}

// stalled is the error once no byte was acknowledged for uploadStall. Bytes sent are no
// progress: a link that drops everything still takes a socket buffer's worth on each attempt.
func (u *upload) stalled(err error) error {
	if time.Since(u.acked) < uploadStall*uploadUnit {
		return nil
	}
	return fmt.Errorf("push %s: no byte acknowledged for %s, at %d of %d bytes: %w", u.label, uploadStall*uploadUnit, u.off, u.b.size, err)
}

func (u *upload) run(ctx context.Context) error {
	u.acked, u.from = time.Now(), time.Now()
	start, logged := time.Now(), time.Now()
	wait := retryWait * uploadUnit
	for {
		var err error
		switch {
		case !u.checked:
			var ok bool
			if ok, err = u.exists(ctx); err == nil {
				if ok {
					return nil
				}
				u.checked = true
			}
		case u.loc == nil:
			err = u.begin(ctx)
		case u.off < u.b.size:
			err = u.patch(ctx)
		default:
			if err = u.commit(ctx); err == nil {
				if d := time.Since(start); d >= progressEvery*uploadUnit {
					u.logf("push %s: %s in %s", u.label, size(u.b.size), d.Round(time.Second))
				}
				return nil
			}
		}
		if err == nil {
			wait = retryWait * uploadUnit
			if time.Since(logged) >= progressEvery*uploadUnit && u.off < u.b.size {
				logged = time.Now()
				rate := float64(u.off) / time.Since(u.from).Seconds()
				u.logf("push %s: %s of %s (%d %%), %s/s", u.label, size(u.off), size(u.b.size), u.off*100/u.b.size, size(int64(rate)))
			}
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errChunkRefused) || errors.Is(err, errSessions) {
			return err
		}
		if final(err) {
			return fmt.Errorf("push %s: %w", u.label, err)
		}
		if s := u.stalled(err); s != nil {
			return s
		}
		u.logf("push %s: %v; again in %s, from the last byte acknowledged", u.label, err, wait.Round(time.Second))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(2*wait, retryMax*uploadUnit)
		u.chunk = max(u.chunk/2, u.least)
		if u.loc != nil {
			u.resync(ctx)
		}
	}
}

// do sends req with a watchdog: the request is cancelled once nothing was acknowledged for
// uploadStall and its body was not read for as long.
func (u *upload) do(ctx context.Context, req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	u.sent.Store(time.Now().UnixNano())
	stall, acked := uploadStall*uploadUnit, u.acked
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(stall / 8)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if time.Since(time.Unix(0, u.sent.Load())) >= stall && time.Since(acked) >= stall {
					cancel()
					return
				}
			}
		}
	}()
	resp, err := u.hc.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	// The body is read here, under the watchdog.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

// final reports whether err is a refusal no retry changes: the credentials, the repository, a
// registry that takes no push, the blob's digest or size, or any other request refused (400) but
// an upload session lost. A session lost (404, 416, 400 BLOB_UPLOAD_*), a request timed out (408),
// a chunk too large (413), a rate limit (429) and a server error are retried.
func final(err error) bool {
	var te *transport.Error
	if !errors.As(err, &te) {
		return false
	}
	if refusal(te) {
		return true
	}
	switch te.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusMethodNotAllowed:
		return true
	case http.StatusBadRequest:
		return !sessionLost(te)
	}
	return false
}

// refusal reports whether the registry names a refusal of the push itself, whatever the request.
func refusal(te *transport.Error) bool {
	for _, d := range te.Errors {
		switch d.Code {
		case transport.DigestInvalidErrorCode, transport.SizeInvalidErrorCode, transport.NameUnknownErrorCode,
			transport.NameInvalidErrorCode, transport.DeniedErrorCode, transport.UnsupportedErrorCode:
			return true
		}
	}
	return false
}

// sessionLost reports whether a refusal is the registry no longer holding the upload where the
// client left it.
func sessionLost(te *transport.Error) bool {
	switch te.StatusCode {
	case http.StatusNotFound, http.StatusRequestedRangeNotSatisfiable:
		return true
	}
	for _, d := range te.Errors {
		if d.Code == transport.BlobUploadInvalidErrorCode || d.Code == transport.BlobUploadUnknownErrorCode {
			return true
		}
	}
	return false
}

// lost drops the session, deleted (it may still hold what was sent): the blob is looked for
// again (a commit whose answer was lost may have landed), then a new session starts from the
// first byte.
func (u *upload) lost(ctx context.Context) {
	u.abort(ctx)
	u.loc, u.checked, u.lostAt = nil, false, u.off
}

func (u *upload) exists(ctx context.Context) (bool, error) {
	req, err := http.NewRequest(http.MethodHead, u.url("/blobs/"+u.b.digest.String()).String(), nil)
	if err != nil {
		return false, err
	}
	resp, err := u.do(ctx, req)
	if err != nil {
		return false, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, transport.CheckError(resp, http.StatusOK)
}

// begin opens an upload session, from the first byte. A registry that loses the upload more
// than maxSessions times fails it: every byte is sent again, so acknowledged bytes are no bound.
func (u *upload) begin(ctx context.Context) error {
	if u.sessions == maxSessions {
		return fmt.Errorf("%w %d times", errSessions, maxSessions)
	}
	req, err := http.NewRequest(http.MethodPost, u.url("/blobs/uploads/").String(), nil)
	if err != nil {
		return err
	}
	resp, err := u.do(ctx, req)
	if err != nil {
		return err
	}
	if err := transport.CheckError(resp, http.StatusAccepted); err != nil {
		return err
	}
	loc, err := location(req.URL, resp)
	if err != nil {
		return err
	}
	u.loc, u.off, u.patched = loc, 0, false
	u.from = time.Now()
	u.sessions++
	if u.sessions > 1 {
		u.logf("push %s: the registry lost the upload at %s; a new one, from the first byte", u.label, size(u.lostAt))
	}
	if n, err := strconv.ParseInt(resp.Header.Get("OCI-Chunk-Min-Length"), 10, 64); err == nil && n > u.least {
		u.least = n
		u.chunk = max(u.chunk, n)
	}
	return nil
}

// location is the upload session a response names, resolved against the request's URL.
func location(base *url.URL, resp *http.Response) (*url.URL, error) {
	l := resp.Header.Get("Location")
	if l == "" {
		return nil, errors.New("the registry names no upload location")
	}
	return base.Parse(l)
}

// acknowledged is the bytes a response's Range header (0-<last byte>) says the registry holds, or
// -1 when it has none. "0-0" is one byte, or nothing when before (the bytes held or sent so far)
// is 0, as registries answer it for an empty upload.
func acknowledged(resp *http.Response, before int64) int64 {
	r := strings.TrimPrefix(resp.Header.Get("Range"), "bytes=")
	first, last, ok := strings.Cut(r, "-")
	if !ok || first != "0" {
		return -1
	}
	n, err := strconv.ParseInt(last, 10, 64)
	if err != nil || n < 0 || n == math.MaxInt64 {
		return -1
	}
	if n == 0 && before == 0 {
		return 0
	}
	return n + 1
}

// patch sends the next chunk. The first chunk of a session, before any was acknowledged, refused
// as a request the registry does not take (400, 405, 416, 501) is the registry refusing chunks:
// it cannot be a session lost, nothing was sent to it yet.
func (u *upload) patch(ctx context.Context) error {
	fresh := !u.ever && !u.patched
	u.patched = true
	n := min(u.chunk, u.b.size-u.off)
	off := u.off
	body := func() io.Reader { return &touch{io.NewSectionReader(u.f, off, n), &u.sent} }
	req, err := http.NewRequest(http.MethodPatch, u.loc.String(), body())
	if err != nil {
		return err
	}
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(body()), nil }
	req.ContentLength = n
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Range", fmt.Sprintf("%d-%d", off, off+n-1))
	t0 := time.Now()
	resp, err := u.do(ctx, req)
	if err != nil {
		return err
	}
	if err := transport.CheckError(resp, http.StatusAccepted, http.StatusNoContent); err != nil {
		te := (*transport.Error)(nil)
		switch resp.StatusCode {
		case http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusRequestedRangeNotSatisfiable, http.StatusNotImplemented:
			if fresh && !(errors.As(err, &te) && refusal(te)) {
				return fmt.Errorf("%w: %w", errChunkRefused, err)
			}
		}
		if errors.As(err, &te) && sessionLost(te) {
			u.lost(ctx)
		}
		return err
	}
	if loc, err := location(u.loc, resp); err == nil {
		u.loc = loc
	}
	got := acknowledged(resp, off+n)
	if got < 0 {
		got = off + n
	}
	if got <= off || got > off+n {
		u.lost(ctx)
		return fmt.Errorf("the registry acknowledged %d bytes after a chunk of %d-%d", got, off, off+n-1)
	}
	u.advance(got)
	u.ever = true
	if d := time.Since(t0).Seconds(); d > 0 {
		rate := float64(got-off) / d
		u.chunk = max(min(int64(rate*chunkTime*uploadUnit.Seconds()), chunkMax), u.least)
	}
	return nil
}

func (u *upload) advance(to int64) {
	if to > u.off {
		u.acked = time.Now()
	}
	u.off = to
}

// resync reads the session's progress after a failure. A registry that cannot tell keeps the
// offset last acknowledged; one that lost the session (404, or 400 BLOB_UPLOAD_*, as one that
// cancels an upload resumed at another offset answers) gets a new one.
func (u *upload) resync(ctx context.Context) {
	req, err := http.NewRequest(http.MethodGet, u.loc.String(), nil)
	if err != nil {
		return
	}
	resp, err := u.do(ctx, req)
	if err != nil {
		return
	}
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK, http.StatusAccepted:
		if got := acknowledged(resp, u.off); got >= 0 && got <= u.b.size {
			if loc, err := location(u.loc, resp); err == nil {
				u.loc = loc
			}
			u.advance(got)
		}
	default:
		if te := (*transport.Error)(nil); errors.As(transport.CheckError(resp), &te) && sessionLost(te) {
			u.lost(ctx)
		}
	}
}

// commit closes the session with the blob's digest. A commit whose answer was lost may have
// landed: the blob is looked for before the next attempt.
func (u *upload) commit(ctx context.Context) error {
	q := u.loc.Query()
	q.Set("digest", u.b.digest.String())
	l := *u.loc
	l.RawQuery = q.Encode()
	req, err := http.NewRequest(http.MethodPut, l.String(), nil)
	if err != nil {
		return err
	}
	req.ContentLength = 0
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := u.do(ctx, req)
	if err == nil {
		if err = transport.CheckError(resp, http.StatusCreated, http.StatusNoContent); err == nil {
			return nil
		}
		if te := (*transport.Error)(nil); errors.As(err, &te) && sessionLost(te) {
			u.lost(ctx) // run looks for the blob before a new session
			return err
		}
	}
	if ok, herr := u.exists(ctx); herr == nil && ok {
		return nil
	}
	return err
}

// abort deletes the upload session, when one is open: a push that failed, or goes on in one
// stream, leaves none behind. A session that holds every byte is left: its commit may be under
// way. It is not cancelled with ctx, but bounded to abortWait.
func (u *upload) abort(ctx context.Context) {
	if u.loc == nil || u.off >= u.b.size {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortWait*uploadUnit)
	defer cancel()
	if req, err := http.NewRequest(http.MethodDelete, u.loc.String(), nil); err == nil {
		_, _ = u.do(ctx, req)
	}
}

// touch records when its reader is read.
type touch struct {
	r    io.Reader
	when *atomic.Int64
}

func (t *touch) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.when.Store(time.Now().UnixNano())
	}
	return n, err
}

// size renders a byte count.
func size(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	d, e := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		d *= unit
		e++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(d), "KMGTPE"[e])
}
