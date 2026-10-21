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
	sizes   []int64 // each chunk's length
	minimum string  // the OCI-Chunk-Min-Length a session is opened with
}

func (f *faulty) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	return f, c, &out, strings.TrimPrefix(srv.URL, "http://") + "/acme/disk", b
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
		// A refusal after a chunk was sent may be a session lost: a new session tells, its first
		// chunk refused; then the stream's own.
		if f.posts != 3 {
			t.Errorf("%d sessions", f.posts)
		}
		// Neither session it opened is left behind.
		if f.deletes != 2 {
			t.Errorf("%d sessions deleted", f.deletes)
		}
	})

	t.Run("a refusal after chunks went up is a session lost, not a refusal of chunks", func(t *testing.T) {
		f, c, out, repo, b := uploadFixture(t)
		f.patch = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 3 {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return true
			}
			return false
		}
		if err := pushed(t, c, repo, b); err != nil {
			t.Fatal(err)
		}
		if f.posts != 2 || strings.Contains(out.String(), "one stream") {
			t.Fatalf("%d sessions, log:\n%s", f.posts, out)
		}
	})

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
