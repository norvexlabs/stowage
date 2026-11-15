package registry

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// faulty is an in-process registry whose upload requests a test may break: it answers an upload
// session's status (GET) from the ranges the registry acknowledged.
type faulty struct {
	next http.Handler
	mu   sync.Mutex
	held map[string]int64 // session → bytes acknowledged
	// patch, when set, sees each chunk (its index from 0) before the registry: true takes it over.
	patch   func(n int, w http.ResponseWriter, r *http.Request) bool
	put     func(w http.ResponseWriter, r *http.Request) bool
	status  func(w http.ResponseWriter, r *http.Request) bool
	head    func(w http.ResponseWriter, r *http.Request) bool
	post    func(w http.ResponseWriter, r *http.Request) bool
	posts   int
	chunks  int
	deletes int
	sizes   []int64      // each chunk's length
	minimum string       // the OCI-Chunk-Min-Length a session is opened with
	pings   atomic.Int32 // GET /v2/: one per transport made
}

func (f *faulty) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v2/" {
		f.pings.Add(1)
	}
	upload := strings.Contains(r.URL.Path, "/blobs/uploads/")
	id := path.Base(r.URL.Path)
	f.mu.Lock()
	switch {
	case upload && r.Method == http.MethodPost:
		f.posts++
		if f.minimum != "" {
			w.Header().Set("OCI-Chunk-Min-Length", f.minimum)
		}
		if f.post != nil {
			f.mu.Unlock()
			if f.post(w, r) {
				return
			}
			f.next.ServeHTTP(w, r)
			return
		}
	case upload && r.Method == http.MethodDelete:
		f.deletes++
	case !upload && r.Method == http.MethodHead && strings.Contains(r.URL.Path, "/blobs/"):
		f.mu.Unlock()
		if f.head != nil && f.head(w, r) {
			return
		}
		f.next.ServeHTTP(w, r)
		return
	case upload && r.Method == http.MethodPatch && r.Header.Get("Content-Range") != "":
		n := f.chunks
		f.chunks++
		f.sizes = append(f.sizes, r.ContentLength)
		f.mu.Unlock()
		if f.patch != nil && f.patch(n, w, r) {
			return
		}
		f.next.ServeHTTP(w, r)
		if got := acknowledged(&http.Response{Header: w.Header()}, 1); got >= 0 {
			f.mu.Lock()
			f.held[id] = got
			f.mu.Unlock()
		}
		return
	case upload && r.Method == http.MethodGet:
		held := f.held[id]
		f.mu.Unlock()
		if f.status != nil && f.status(w, r) {
			return
		}
		w.Header().Set("Location", r.URL.Path)
		w.Header().Set("Range", fmt.Sprintf("0-%d", max(held-1, 0)))
		w.WriteHeader(http.StatusNoContent)
		return
	case upload && r.Method == http.MethodPut:
		f.mu.Unlock()
		if f.put != nil && f.put(w, r) {
			return
		}
		f.next.ServeHTTP(w, r)
		return
	}
	f.mu.Unlock()
	f.next.ServeHTTP(w, r)
}

// drop reads half a chunk, then breaks the connection.
func drop(w http.ResponseWriter, r *http.Request) {
	_, _ = io.CopyN(io.Discard, r.Body, r.ContentLength/2)
	panic(http.ErrAbortHandler)
}

// uploadFixture is a client of a faulty registry, the log it writes and a file blob of 20 KiB.
func uploadFixture(t *testing.T) (*faulty, *Client, *bytes.Buffer, string, Blob) {
	t.Helper()
	return uploadFixtureOf(t, 20<<10)
}

// uploadFixtureOf is uploadFixture with a file blob of n bytes.
func uploadFixtureOf(t *testing.T, n int) (*faulty, *Client, *bytes.Buffer, string, Blob) {
	t.Helper()
	f := &faulty{next: ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))), held: map[string]int64{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	var out bytes.Buffer
	c := New(authn.NewMultiKeychain(), WithLog(&out))
	return f, c, &out, strings.TrimPrefix(srv.URL, "http://") + "/acme/disk", fileBlob(t, n)
}

// fileBlob is a file blob of n random bytes.
func fileBlob(t *testing.T, n int) Blob {
	t.Helper()
	file := filepath.Join(t.TempDir(), "disk.raw")
	data := make([]byte, n)
	_, _ = rand.Read(data)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := FileBlob("application/x-test", file)
	if err != nil {
		t.Fatal(err)
	}
	b.Annotations = map[string]string{TitleAnnotation: "disk.raw"}
	return b
}

// pushed pushes b alone and checks the registry holds it, whole.
func pushed(t *testing.T, c *Client, repo string, b Blob) error {
	t.Helper()
	r, err := c.Repo(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.pushFile(context.Background(), r, b); err != nil {
		return err
	}
	l, err := remote.Layer(r.Digest(b.digest.String()), c.opts(context.Background())...)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := l.Compressed()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	want, _ := os.ReadFile(b.path)
	if !bytes.Equal(got, want) {
		t.Fatalf("the registry holds %d bytes, not the file's %d", len(got), len(want))
	}
	return nil
}

// fixedChunks makes every chunk but the last chunkMax bytes, whatever the rate the upload measures
// (which sizes them otherwise, and a loaded host slows): a blob's last chunk is then the remainder
// alone. For the test's length; its subtests run one at a time.
func fixedChunks(t *testing.T) {
	lo := chunkMin
	chunkMin = chunkMax
	t.Cleanup(func() { chunkMin = lo })
}

func TestAFileBlobGoesUpInChunksAndResumes(t *testing.T) {
	unit, lo, hi := uploadUnit, chunkMin, chunkMax
	uploadUnit, chunkMin, chunkMax = time.Millisecond, 1<<10, 4<<10
	t.Cleanup(func() { uploadUnit, chunkMin, chunkMax = unit, lo, hi })

	t.Run("in chunks, resumed after a broken one", func(t *testing.T) {
		f, c, out, repo, b := uploadFixture(t)
		f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
			time.Sleep(6 * time.Millisecond) // progress lines, every 30 ms
			if n == 1 || n == 4 {
				drop(w, r)
			}
			return false
		}
		if err := pushed(t, c, repo, b); err != nil {
			t.Fatal(err)
		}
		// One session: the upload went on from the bytes acknowledged, never from the start.
		if f.posts != 1 || f.chunks < 7 {
			t.Fatalf("%d sessions, %d chunks", f.posts, f.chunks)
		}
		for _, want := range []string{"push disk.raw (sha256:", "again in", "from the last byte acknowledged", " %), "} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("the log misses %q:\n%s", want, out)
			}
		}
	})

	t.Run("already there", func(t *testing.T) {
		f, c, _, repo, b := uploadFixture(t)
		if err := pushed(t, c, repo, b); err != nil {
			t.Fatal(err)
		}
		f.posts = 0
		if err := pushed(t, c, repo, b); err != nil || f.posts != 0 {
			t.Fatalf("pushed again: %v, %d sessions", err, f.posts)
		}
	})

	t.Run("a session the registry lost starts again", func(t *testing.T) {
		f, c, out, repo, b := uploadFixture(t)
		f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 2 {
				drop(w, r)
			}
			return false
		}
		f.status = func(w http.ResponseWriter, r *http.Request) bool {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		if err := pushed(t, c, repo, b); err != nil {
			t.Fatal(err)
		}
		if f.posts != 2 || !strings.Contains(out.String(), "the registry lost the upload at ") || !strings.Contains(out.String(), "a new one, from the first byte") {
			t.Fatalf("%d sessions, want a second one; log:\n%s", f.posts, out)
		}
	})

	t.Run("a registry that keeps losing the upload gets one stream", func(t *testing.T) {
		f, c, out, repo, b := uploadFixture(t)
		f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n%2 == 1 {
				drop(w, r)
			}
			return false
		}
		f.status = func(w http.ResponseWriter, r *http.Request) bool {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		if err := pushed(t, c, repo, b); err != nil {
			t.Fatal(err)
		}
		// The sessions chunked, and the stream's own.
		if f.posts != maxSessions+1 || !strings.Contains(out.String(), "lost the upload 4 times; pushing it in one stream") {
			t.Fatalf("%d sessions, log:\n%s", f.posts, out)
		}
	})

	t.Run("a registry that cannot tell keeps the last acknowledged byte", func(t *testing.T) {
		f, c, _, repo, b := uploadFixture(t)
		f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 2 {
				drop(w, r)
			}
			return false
		}
		f.status = func(w http.ResponseWriter, r *http.Request) bool {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return true
		}
		if err := pushed(t, c, repo, b); err != nil {
			t.Fatal(err)
		}
		if f.posts != 1 {
			t.Fatalf("%d sessions", f.posts)
		}
	})

	t.Run("a registry refusing chunks gets one stream", func(t *testing.T) {
		f, c, out, repo, b := uploadFixture(t)
		f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 0 { // the first one lost on the way: the refusal comes after
				drop(w, r)
			}
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return true
		}
		if err := pushed(t, c, repo, b); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "refuses chunked uploads") || !strings.Contains(out.String(), "pushing it in one stream") {
			t.Errorf("log:\n%s", out)
		}
		// A refusal after a chunk was sent may be a session lost: the session's status tells it
		// holds the offset, the smallest chunk refused; then the stream's own.
		if f.posts != 2 {
			t.Errorf("%d sessions", f.posts)
		}
		// The session it opened is not left behind.
		if f.deletes != 1 {
			t.Errorf("%d sessions deleted", f.deletes)
		}
	})

	t.Run("the smallest chunk refused while the status cannot be read gets one stream", func(t *testing.T) {
		f, c, out, repo, b := uploadFixture(t)
		f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 0 {
				drop(w, r)
			}
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return true
		}
		f.status = func(w http.ResponseWriter, r *http.Request) bool {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return true
		}
		if err := pushed(t, c, repo, b); err != nil || f.posts != 2 || !strings.Contains(out.String(), "pushing it in one stream") {
			t.Fatalf("%v, %d sessions, log:\n%s", err, f.posts, out)
		}
	})

	t.Run("a refusal after chunks went up is a session lost, not a refusal of chunks", func(t *testing.T) {
		f, c, out, repo, b := uploadFixture(t)
		gone := false
		f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 3 {
				gone = true
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return true
			}
			return false
		}
		// The registry dropped the session with its 416: it no longer knows it.
		f.status = func(w http.ResponseWriter, r *http.Request) bool {
			if gone {
				w.WriteHeader(http.StatusNotFound)
				return true
			}
			return false
		}
		if err := pushed(t, c, repo, b); err != nil {
			t.Fatal(err)
		}
		if f.posts != 2 || strings.Contains(out.String(), "one stream") || strings.Contains(out.String(), "refuses a chunk") {
			t.Fatalf("%d sessions, log:\n%s", f.posts, out)
		}
	})

	// A registry that caps a request's body (ghcr.io: 416 over 4 MiB, others 413): the chunk
	// shrinks, the session goes on, and the client's later uploads to it stay under the cap.
	for _, status := range []int{http.StatusRequestedRangeNotSatisfiable, http.StatusRequestEntityTooLarge} {
		t.Run(fmt.Sprintf("a chunk refused with %d for its size shrinks", status), func(t *testing.T) {
			f, c, out, repo, b := uploadFixture(t)
			const limit = 2 << 10
			refused := 0
			f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
				if r.ContentLength > limit {
					refused++
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"errors":[{"code":"REQUESTED_RANGE_NOT_SATISFIABLE","message":"the request body is too large"}]}`))
					return true
				}
				return false
			}
			if err := pushed(t, c, repo, b); err != nil {
				t.Fatal(err)
			}
			if f.posts != 1 || refused != 1 || strings.Contains(out.String(), "one stream") || strings.Contains(out.String(), "again in") ||
				!strings.Contains(out.String(), "refuses a chunk of 4.0 KiB; chunks of at most 2.0 KiB from now") {
				t.Fatalf("%d sessions, %d refused, log:\n%s", f.posts, refused, out)
			}
			// The chunk at the cap went up: the boundary's other side.
			if f.sizes[len(f.sizes)-2] != limit {
				t.Fatalf("chunks %v", f.sizes)
			}
			// Another blob, same client: no chunk over the cap is sent.
			f.posts, refused = 0, 0
			if err := pushed(t, c, repo, fileBlob(t, 20<<10)); err != nil || refused != 0 || f.posts != 1 {
				t.Fatalf("%v, %d refused, %d sessions", err, refused, f.posts)
			}
		})

		// The smallest chunk refused for its size before any was acknowledged: no smaller one to
		// try, the registry gets one stream.
		t.Run(fmt.Sprintf("the smallest chunk refused with %d gets one stream", status), func(t *testing.T) {
			f, c, out, repo, b := uploadFixture(t)
			f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
				if r.ContentLength >= chunkMin {
					w.WriteHeader(status)
					return true
				}
				return false
			}
			if err := pushed(t, c, repo, b); err != nil {
				t.Fatal(err)
			}
			if f.posts != 2 || f.deletes != 1 || !strings.Contains(out.String(), "pushing it in one stream") {
				t.Fatalf("%d sessions, %d deleted, log:\n%s", f.posts, f.deletes, out)
			}
		})
	}

	// A 416 the session's status explains otherwise: not a chunk's size.
	t.Run("a 416 while the session holds another offset goes on from there", func(t *testing.T) {
		f, c, out, repo, b := uploadFixture(t)
		f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 2 { // the registry took the chunk, then answered 416
				f.next.ServeHTTP(httptest.NewRecorder(), r)
				f.mu.Lock()
				_, last, _ := strings.Cut(r.Header.Get("Content-Range"), "-")
				end, _ := strconv.ParseInt(last, 10, 64)
				f.held[path.Base(r.URL.Path)] = end + 1
				f.mu.Unlock()
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return true
			}
			return false
		}
		if err := pushed(t, c, repo, b); err != nil {
			t.Fatal(err)
		}
		// Further than the chunk's start: the upload goes on at once, its chunk kept.
		if f.posts != 1 || strings.Contains(out.String(), "refuses a chunk") || strings.Contains(out.String(), "again in") {
			t.Fatalf("%d sessions, log:\n%s", f.posts, out)
		}
	})

	t.Run("a 416 the session's status cannot explain is retried, not a ceiling", func(t *testing.T) {
		f, c, out, repo, b := uploadFixture(t)
		refused := false
		f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 2 {
				refused = true
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return true
			}
			return false
		}
		f.status = func(w http.ResponseWriter, r *http.Request) bool {
			if refused {
				refused = false
				w.WriteHeader(http.StatusServiceUnavailable)
				return true
			}
			return false
		}
		if err := pushed(t, c, repo, b); err != nil {
			t.Fatal(err)
		}
		if f.posts != 1 || strings.Contains(out.String(), "refuses a chunk") || !strings.Contains(out.String(), "again in") {
			t.Fatalf("%d sessions, log:\n%s", f.posts, out)
		}
	})

	t.Run("a 416 while the session claims bytes never sent is a session lost", func(t *testing.T) {
		f, c, out, repo, b := uploadFixture(t)
		claim := false
		f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 2 {
				claim = true
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return true
			}
			return false
		}
		f.status = func(w http.ResponseWriter, r *http.Request) bool {
			if claim {
				claim = false
				w.Header().Set("Range", fmt.Sprintf("0-%d", b.size-1))
				w.WriteHeader(http.StatusNoContent)
				return true
			}
			return false
		}
		if err := pushed(t, c, repo, b); err != nil || f.posts != 2 || strings.Contains(out.String(), "refuses a chunk") {
			t.Fatalf("%v, %d sessions, log:\n%s", err, f.posts, out)
		}
	})

	t.Run("a 416 while the session holds less goes on from there, not a ceiling", func(t *testing.T) {
		f, c, out, repo, b := uploadFixture(t)
		back := int64(-1)
		f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 2 {
				first, _, _ := strings.Cut(r.Header.Get("Content-Range"), "-")
				back, _ = strconv.ParseInt(first, 10, 64)
				back -= 512
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return true
			}
			return false
		}
		f.status = func(w http.ResponseWriter, r *http.Request) bool {
			if back > 0 {
				w.Header().Set("Range", fmt.Sprintf("0-%d", back-1))
				w.WriteHeader(http.StatusNoContent)
				back = 0
				return true
			}
			return false
		}
		if err := pushed(t, c, repo, b); err != nil || f.posts != 1 || strings.Contains(out.String(), "refuses a chunk") {
			t.Fatalf("%v, %d sessions, log:\n%s", err, f.posts, out)
		}
	})

	t.Run("a 416 to the last chunk whose status cannot be read is retried", func(t *testing.T) {
		f, c, out, repo, _ := uploadFixture(t)
		fixedChunks(t)
		b := fileBlob(t, int(4*chunkMin+512)) // four whole chunks, then 512 B
		refused, unread := 0, false
		f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if r.ContentLength < chunkMin && refused == 0 {
				refused, unread = 1, true
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return true
			}
			return false
		}
		f.status = func(w http.ResponseWriter, r *http.Request) bool {
			if unread {
				unread = false
				w.WriteHeader(http.StatusServiceUnavailable)
				return true
			}
			return false
		}
		if err := pushed(t, c, repo, b); err != nil || refused != 1 || f.posts != 1 || strings.Contains(out.String(), "one stream") {
			t.Fatalf("%v, %d refused, %d sessions, log:\n%s", err, refused, f.posts, out)
		}
	})

	t.Run("a ceiling another upload learnt bounds the next chunk", func(t *testing.T) {
		f, c, _, repo, b := uploadFixture(t)
		r, err := c.Repo(repo)
		if err != nil {
			t.Fatal(err)
		}
		// Learnt while this upload's chunk is past it, as by a disk going up beside it.
		ceiling := c.ceiling(r.RegistryStr())
		f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 1 {
				ceiling.Store(2 << 10)
			}
			return false
		}
		if err := pushed(t, c, repo, b); err != nil {
			t.Fatal(err)
		}
		for _, n := range f.sizes[2:] {
			if n > 2<<10 {
				t.Fatalf("chunks %v", f.sizes)
			}
		}
	})

	t.Run("a last chunk under the smallest refused for its size is retried", func(t *testing.T) {
		f, c, out, repo, _ := uploadFixture(t)
		fixedChunks(t)
		b := fileBlob(t, int(4*chunkMin+512)) // four whole chunks, then 512 B
		refused := 0
		f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if r.ContentLength < chunkMin && refused == 0 {
				refused++
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				return true
			}
			return false
		}
		if err := pushed(t, c, repo, b); err != nil || refused != 1 || f.posts != 1 || strings.Contains(out.String(), "one stream") || strings.Contains(out.String(), "refuses a chunk") {
			t.Fatalf("%v, %d refused, %d sessions, log:\n%s", err, refused, f.posts, out)
		}
	})

	// A size refusal that names a refusal of the push itself fails it at once.
	for _, status := range []int{http.StatusRequestEntityTooLarge, http.StatusRequestedRangeNotSatisfiable} {
		t.Run(fmt.Sprintf("a %d DENIED after chunks went up fails the push", status), func(t *testing.T) {
			f, c, out, repo, b := uploadFixture(t)
			f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
				if n == 2 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"errors":[{"code":"DENIED","message":"over quota"}]}`))
					return true
				}
				return false
			}
			err := pushed(t, c, repo, b)
			if err == nil || !strings.Contains(err.Error(), "over quota") || f.posts != 1 || strings.Contains(out.String(), "refuses a chunk") || strings.Contains(out.String(), "again in") {
				t.Fatalf("%v, %d sessions, log:\n%s", err, f.posts, out)
			}
		})
	}

	// A 400 mid-upload: the session lost when the registry says so, otherwise a refusal.
	for _, c := range []struct {
		name, code string
		lost       bool
	}{
		{"BLOB_UPLOAD_INVALID", "BLOB_UPLOAD_INVALID", true},
		{"BLOB_UPLOAD_UNKNOWN", "BLOB_UPLOAD_UNKNOWN", true},
		{"another", "MANIFEST_INVALID", false},
		{"DENIED", "DENIED", false},
	} {
		t.Run("a 400 "+c.name+" after chunks went up", func(t *testing.T) {
			f, c2, out, repo, b := uploadFixture(t)
			f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
				if n == 2 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = fmt.Fprintf(w, `{"errors":[{"code":%q,"message":"no"}]}`, c.code)
					return true
				}
				return false
			}
			err := pushed(t, c2, repo, b)
			if c.lost {
				if err != nil || f.posts != 2 || f.deletes != 1 {
					t.Fatalf("%v, %d sessions, %d deleted", err, f.posts, f.deletes)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.code) || f.posts != 1 || f.deletes != 1 || strings.Contains(out.String(), "again in") {
				t.Fatalf("%v, %d sessions, %d deleted; log:\n%s", err, f.posts, f.deletes, out)
			}
		})
	}

	// The first chunk of a session refused: a registry that takes no chunks, unless it names a
	// refusal of the push itself.
	for _, c := range []struct {
		status   int
		code     string
		fallback bool
	}{
		{http.StatusBadRequest, "BLOB_UPLOAD_INVALID", true},
		{http.StatusMethodNotAllowed, "", true},
		{http.StatusNotImplemented, "", true},
		{http.StatusBadRequest, "DENIED", false},
	} {
		t.Run(fmt.Sprintf("a first chunk refused with %d %s", c.status, c.code), func(t *testing.T) {
			f, c2, out, repo, b := uploadFixture(t)
			f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				if c.code != "" {
					_, _ = fmt.Fprintf(w, `{"errors":[{"code":%q,"message":"no"}]}`, c.code)
				}
				return true
			}
			err := pushed(t, c2, repo, b)
			if c.fallback != (err == nil && strings.Contains(out.String(), "pushing it in one stream")) {
				t.Fatalf("%v; log:\n%s", err, out)
			}
		})
	}

	t.Run("a registry that cannot read a session's progress keeps it", func(t *testing.T) {
		f, c, _, repo, b := uploadFixture(t)
		f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 2 {
				drop(w, r)
			}
			return false
		}
		f.status = func(w http.ResponseWriter, r *http.Request) bool {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":[{"code":"UNSUPPORTED","message":"no"}]}`))
			return true
		}
		if err := pushed(t, c, repo, b); err != nil || f.posts != 1 {
			t.Fatalf("%v, %d sessions", err, f.posts)
		}
	})

	t.Run("a 400 BLOB_UPLOAD_INVALID to a session's progress loses it", func(t *testing.T) {
		f, c, _, repo, b := uploadFixture(t)
		f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 2 {
				drop(w, r)
			}
			return false
		}
		f.status = func(w http.ResponseWriter, r *http.Request) bool {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":[{"code":"BLOB_UPLOAD_INVALID","message":"no"}]}`))
			return true
		}
		if err := pushed(t, c, repo, b); err != nil || f.posts != 2 {
			t.Fatalf("%v, %d sessions", err, f.posts)
		}
	})

	t.Run("the first look for the blob is retried", func(t *testing.T) {
		f, c, _, repo, b := uploadFixture(t)
		heads := 0
		f.head = func(w http.ResponseWriter, r *http.Request) bool {
			heads++
			if heads == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return true
			}
			return false
		}
		if err := pushed(t, c, repo, b); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a blob of one byte", func(t *testing.T) {
		f, c, _, repo, b := uploadFixtureOf(t, 1)
		if err := pushed(t, c, repo, b); err != nil || f.posts != 1 {
			t.Fatalf("%v, %d sessions", err, f.posts)
		}
	})

	t.Run("a registry's minimum chunk", func(t *testing.T) {
		f, c, _, repo, b := uploadFixture(t)
		f.minimum = "8192"
		if err := pushed(t, c, repo, b); err != nil {
			t.Fatal(err)
		}
		for i, n := range f.sizes[:len(f.sizes)-1] {
			if n < 8192 {
				t.Fatalf("chunk %d of %d bytes: %v", i, n, f.sizes)
			}
		}
	})

	t.Run("no byte acknowledged fails, naming the blob", func(t *testing.T) {
		f, c, _, repo, b := uploadFixture(t)
		// Before the server closes: it does not see the client go while the body is unread.
		stop := make(chan struct{})
		t.Cleanup(func() { close(stop) })
		f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 0 {
				return false
			}
			select { // a link that took the connection and carries nothing
			case <-r.Context().Done():
			case <-stop:
			}
			return true
		}
		t0 := time.Now()
		err := pushed(t, c, repo, b)
		if err == nil || !strings.Contains(err.Error(), "push disk.raw (sha256:") || !strings.Contains(err.Error(), "no byte acknowledged for 300ms") {
			t.Fatalf("err %v", err)
		}
		// Not before the bound: the first chunk was acknowledged, then nothing for 300 ms.
		if d := time.Since(t0); d < 300*time.Millisecond || d > 5*time.Second {
			t.Fatalf("failed after %s", d)
		}
	})

	t.Run("a digest refused fails at once", func(t *testing.T) {
		f, c, _, repo, b := uploadFixture(t)
		f.put = func(w http.ResponseWriter, r *http.Request) bool {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":[{"code":"DIGEST_INVALID","message":"no"}]}`))
			return true
		}
		t0 := time.Now()
		if err := pushed(t, c, repo, b); err == nil || !strings.Contains(err.Error(), "DIGEST_INVALID") || strings.Contains(err.Error(), "no byte acknowledged") {
			t.Fatalf("err %v", err)
		}
		if d := time.Since(t0); d > 200*time.Millisecond {
			t.Fatalf("failed after %s: retried", d)
		}
		// A session that holds every byte is left to the registry: its commit may be under way.
		if f.deletes != 0 {
			t.Fatalf("%d sessions deleted", f.deletes)
		}
	})

	t.Run("a repository the registry refuses fails at once", func(t *testing.T) {
		f, c, _, repo, b := uploadFixture(t)
		f.post = func(w http.ResponseWriter, r *http.Request) bool {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"code":"NAME_UNKNOWN","message":"no"}]}`))
			return true
		}
		t0 := time.Now()
		if err := pushed(t, c, repo, b); err == nil || !strings.Contains(err.Error(), "NAME_UNKNOWN") {
			t.Fatalf("err %v", err)
		}
		if d := time.Since(t0); d > 200*time.Millisecond {
			t.Fatalf("failed after %s: retried", d)
		}
	})

	t.Run("a commit whose answer was lost is looked for, not sent again", func(t *testing.T) {
		f, c, _, repo, b := uploadFixture(t)
		puts, heads := 0, 0
		f.put = func(w http.ResponseWriter, r *http.Request) bool {
			puts++
			f.next.ServeHTTP(httptest.NewRecorder(), r) // it lands; its answer does not
			panic(http.ErrAbortHandler)
		}
		f.head = func(w http.ResponseWriter, r *http.Request) bool {
			heads++
			if heads == 2 { // right after the commit: not there yet
				w.WriteHeader(http.StatusNotFound)
				return true
			}
			return false
		}
		f.status = func(w http.ResponseWriter, r *http.Request) bool {
			w.WriteHeader(http.StatusNotFound) // the session, committed, is gone
			return true
		}
		if err := pushed(t, c, repo, b); err != nil {
			t.Fatal(err)
		}
		if f.posts != 1 || puts != 1 {
			t.Fatalf("%d sessions, %d commits", f.posts, puts)
		}
	})
}

func TestAcknowledged(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		r      string
		before int64
		want   int64
	}{
		{"0-0", 0, 0}, // an empty session
		{"0-0", 1, 1},
		{"0-9", 0, 10},
		{"bytes=0-9", 5, 10},
		{"", 0, -1},
		{"1-9", 0, -1},
		{"0-x", 0, -1},
		{"0--1", 0, -1},
	} {
		h := http.Header{}
		h.Set("Range", c.r)
		if got := acknowledged(&http.Response{Header: h}, c.before); got != c.want {
			t.Errorf("%q after %d: %d, want %d", c.r, c.before, got, c.want)
		}
	}
}

// FuzzAcknowledged: whatever a registry answers, the bytes read are none, or within the range it
// names.
func FuzzAcknowledged(f *testing.F) {
	for _, r := range []string{"0-0", "0-9", "bytes=0-9", "", "1-9", "0-", "0--1", "0-9223372036854775807"} {
		f.Add(r, int64(0))
	}
	f.Fuzz(func(t *testing.T, r string, before int64) {
		h := http.Header{}
		h.Set("Range", r)
		got := acknowledged(&http.Response{Header: h}, before)
		if got == -1 {
			return
		}
		last, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimPrefix(h.Get("Range"), "bytes="), "0-"), 10, 64)
		if err != nil || got < 0 || (got != last+1 && !(last == 0 && got == 0)) {
			t.Fatalf("%q after %d: %d", r, before, got)
		}
	})
}

func TestSize(t *testing.T) {
	t.Parallel()
	for n, want := range map[int64]string{1023: "1023 B", 1024: "1.0 KiB", 3 << 29: "1.5 GiB"} {
		if got := size(n); got != want {
			t.Errorf("%d: %s, want %s", n, got, want)
		}
	}
}

// A commit is awaited for its own bound, scaled to the blob, beyond the client's response bound:
// a registry verifying a large blob for longer than that answers one commit, not a retry.
func TestACommitIsAwaitedForTheBlobsSize(t *testing.T) {
	unit, wait, rate := uploadUnit, responseWait, commitRate
	uploadUnit, responseWait, commitRate = time.Millisecond, 200*time.Millisecond, 40
	t.Cleanup(func() { uploadUnit, responseWait, commitRate = unit, wait, rate })
	f, c, out, repo, b := uploadFixture(t)
	// 20 KiB at 40 bytes a unit: the commit is awaited for 200 ms and 512 units.
	if got := commitWait(b.size); got != 712*time.Millisecond {
		t.Fatalf("commitWait: %s", got)
	}
	var commits atomic.Int32
	f.put = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("digest") != "" {
			commits.Add(1)
			time.Sleep(2 * responseWait) // past the client's response bound, within the commit's
		}
		return false
	}
	if err := pushed(t, c, repo, b); err != nil {
		t.Fatal(err)
	}
	if n := commits.Load(); n != 1 || strings.Contains(out.String(), "again in") {
		t.Fatalf("%d commits, log:\n%s", n, out)
	}
	// A registry refusing chunks gets the blob in one stream, its commit awaited as long.
	f, c, out, repo, b = uploadFixture(t)
	f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return true
	}
	commits.Store(0)
	f.put = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("digest") != "" {
			commits.Add(1)
			time.Sleep(2 * responseWait)
		}
		return false
	}
	if err := pushed(t, c, repo, b); err != nil || !strings.Contains(out.String(), "pushing it in one stream") {
		t.Fatalf("one stream: %v, log:\n%s", err, out)
	}
	if n := commits.Load(); n != 1 {
		t.Fatalf("one stream: %d commits", n)
	}
}

// A registry failing every commit fails the push once the commit's bound passed since the first
// commit, not at each attempt; a blob already there makes no commit transport.
func TestAFailingCommitStopsAtItsBound(t *testing.T) {
	unit, wait, rate := uploadUnit, responseWait, commitRate
	uploadUnit, responseWait, commitRate = time.Millisecond, 50*time.Millisecond, 1<<30
	t.Cleanup(func() { uploadUnit, responseWait, commitRate = unit, wait, rate })
	f, c, _, repo, b := uploadFixture(t)
	f.put = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("digest") != "" {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	}
	r, err := c.Repo(repo)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	err = c.pushFile(ctx, r, b)
	if err == nil || !strings.Contains(err.Error(), "not committed in 300ms since every byte was acknowledged") {
		t.Fatalf("a commit failing for good: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("stopped after %s", d)
	}
	// Lazily made: the commit's transport is one ping more than the upload's.
	f.put = nil
	if err := pushed(t, c, repo, b); err != nil {
		t.Fatal(err)
	}
	pings := f.pings.Load()
	if err := c.pushFile(context.Background(), r, b); err != nil {
		t.Fatal(err)
	}
	if n := f.pings.Load() - pings; n != 1 {
		t.Fatalf("a blob already there: %d transports made, want 1", n)
	}
}

// The waiting transport is made once per client, when a commit first needs it: a push the registry
// already holds makes none, and every later push reuses the first one's.
func TestTheWaitingTransportIsMadeOnce(t *testing.T) {
	t.Parallel()
	_, c, _, repo, b := uploadFixture(t)
	if err := pushed(t, c, repo, b); err != nil {
		t.Fatal(err)
	}
	first := c.awaited
	if first == nil {
		t.Fatal("a commit made no waiting transport")
	}
	if err := pushed(t, c, repo, fileBlob(t, 4<<10)); err != nil {
		t.Fatal(err)
	}
	if c.awaited != first {
		t.Fatal("a second push made another waiting transport")
	}
	_, fresh, _, _, _ := uploadFixture(t)
	r, err := fresh.Repo(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.pushFile(context.Background(), r, b); err != nil || fresh.awaited != nil {
		t.Fatalf("a blob already there: %v, waiting transport %v", err, fresh.awaited)
	}
}
