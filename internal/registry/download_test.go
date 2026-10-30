package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
)

// blobFunc answers the n-th GET (from 1) of the layer: the blob it holds, the offset a Range
// header asked for (-1: none).
type blobFunc func(w http.ResponseWriter, n int, blob []byte, off int64)

// blobHost serves a registry whose layer GETs are answered by f, and records each GET's Range.
func blobHost(t *testing.T, f blobFunc) (host string, ranges func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	host = serve(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/blobs/") {
				h.ServeHTTP(w, r)
				return
			}
			inner := r.Clone(r.Context())
			inner.Header.Del("Range")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, inner)
			mu.Lock()
			seen = append(seen, r.Header.Get("Range"))
			n := len(seen)
			mu.Unlock()
			off := int64(-1)
			if rg := r.Header.Get("Range"); rg != "" {
				v, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"), 10, 64)
				if err != nil {
					t.Errorf("range %q", rg)
				}
				off = v
			}
			f(w, n, rec.Body.Bytes(), off)
		})
	})
	return host, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
}

// whole answers as a registry honouring Range does.
func whole(w http.ResponseWriter, blob []byte, off int64) {
	if off <= 0 {
		w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
		_, _ = w.Write(blob)
		return
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, len(blob)-1, len(blob)))
	w.Header().Set("Content-Length", strconv.Itoa(len(blob)-int(off)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(blob[off:])
}

// cut announces the whole blob but sends only its first upto bytes: the connection then drops.
func cut(w http.ResponseWriter, blob []byte, upto int) {
	w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
	_, _ = w.Write(blob[:upto])
}

// resumeDisk has no period: a resumed read from the wrong byte cannot rebuild it.
var resumeDisk = func() []byte {
	var b []byte
	for h := sha256.Sum256(nil); len(b) < 11000; h = sha256.Sum256(h[:]) {
		b = append(b, h[:]...)
	}
	return b
}()

func TestDownloadResumesACutRead(t *testing.T) {
	unit := downloadUnit
	downloadUnit = time.Millisecond
	t.Cleanup(func() { downloadUnit = unit })
	half := len(resumeDisk) / 2

	t.Run("a cut once goes on from the byte reached", func(t *testing.T) {
		t.Parallel()
		host, ranges := blobHost(t, func(w http.ResponseWriter, n int, blob []byte, off int64) {
			if n == 1 {
				cut(w, blob, half)
				return
			}
			whole(w, blob, off)
		})
		var log bytes.Buffer
		c := New(authn.DefaultKeychain, WithLog(&log))
		d := pushDisk(t, c, host, "atlas.qcow2", resumeDisk)
		dir := t.TempDir()
		var last int64
		p, _, err := c.Download(context.Background(), d, kind, kind, dir, "f", func(done, total int64) {
			if done < last || total != int64(len(resumeDisk)) {
				t.Errorf("progress %d of %d after %d", done, total, last)
			}
			last = done
		})
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(p); !bytes.Equal(got, resumeDisk) || last != int64(len(resumeDisk)) {
			t.Errorf("wrote %d bytes, progress %d", len(got), last)
		}
		if r := ranges(); len(r) != 2 || r[0] != "" || r[1] != fmt.Sprintf("bytes=%d-", half) {
			t.Errorf("ranges %q", r)
		}
		if want := "download atlas.qcow2 (sha256:"; !strings.Contains(log.String(), want) || !strings.Contains(log.String(), fmt.Sprintf("resuming at %d of %d bytes", half, len(resumeDisk))) {
			t.Errorf("log %q", log.String())
		}
	})

	t.Run("a stream that stops sending is dropped and resumed", func(t *testing.T) {
		t.Parallel()
		hold := make(chan struct{})
		host, ranges := blobHost(t, func(w http.ResponseWriter, n int, blob []byte, off int64) {
			if n == 1 {
				w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
				_, _ = w.Write(blob[:half])
				w.(http.Flusher).Flush()
				<-hold // the connection stays up, silent
				return
			}
			whole(w, blob, off)
		})
		t.Cleanup(func() { close(hold) })
		var log bytes.Buffer
		c := New(authn.DefaultKeychain, WithLog(&log))
		d := pushDisk(t, c, host, "atlas.qcow2", resumeDisk)
		p, _, err := c.Download(context.Background(), d, kind, kind, t.TempDir(), "f", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(p); !bytes.Equal(got, resumeDisk) {
			t.Errorf("wrote %d bytes", len(got))
		}
		if r := ranges(); len(r) != 2 || r[1] != fmt.Sprintf("bytes=%d-", half) {
			t.Errorf("ranges %q", r)
		}
		if !strings.Contains(log.String(), "no byte for 120 s; resuming at") {
			t.Errorf("log %q", log.String())
		}
	})

	t.Run("a slow skip that keeps bringing bytes is not a stall", func(t *testing.T) {
		t.Parallel()
		host, _ := blobHost(t, func(w http.ResponseWriter, n int, blob []byte, off int64) {
			if n == 1 {
				cut(w, blob, half)
				return
			}
			// Range ignored, the whole blob sent in steps: the skip alone outlasts downloadStall.
			w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
			step := len(blob) / 10
			for i := 0; i < len(blob); i += step {
				_, _ = w.Write(blob[i:min(i+step, len(blob))])
				w.(http.Flusher).Flush()
				time.Sleep(downloadStall * downloadUnit / 4)
			}
		})
		c := New(authn.DefaultKeychain)
		d := pushDisk(t, c, host, "atlas.qcow2", resumeDisk)
		p, _, err := c.Download(context.Background(), d, kind, kind, t.TempDir(), "f", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(p); !bytes.Equal(got, resumeDisk) {
			t.Errorf("wrote %d bytes", len(got))
		}
	})

	t.Run("a skip cut short that got further each time is progress", func(t *testing.T) {
		t.Parallel()
		host, _ := blobHost(t, func(w http.ResponseWriter, n int, blob []byte, off int64) {
			switch {
			case n == 1:
				cut(w, blob, half)
			case n <= 2+downloadTries: // range ignored, each answer cut further into the skip
				cut(w, blob, half*(n-1)/(2+downloadTries))
			default:
				whole(w, blob, -1)
			}
		})
		c := New(authn.DefaultKeychain)
		d := pushDisk(t, c, host, "atlas.qcow2", resumeDisk)
		p, _, err := c.Download(context.Background(), d, kind, kind, t.TempDir(), "f", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(p); !bytes.Equal(got, resumeDisk) {
			t.Errorf("wrote %d bytes", len(got))
		}
	})

	t.Run("a server ignoring the range has the bytes read skipped", func(t *testing.T) {
		t.Parallel()
		host, ranges := blobHost(t, func(w http.ResponseWriter, n int, blob []byte, off int64) {
			if n == 1 {
				cut(w, blob, half)
				return
			}
			whole(w, blob, 0)
		})
		c := New(authn.DefaultKeychain)
		d := pushDisk(t, c, host, "atlas.qcow2", resumeDisk)
		p, _, err := c.Download(context.Background(), d, kind, kind, t.TempDir(), "f", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(p); !bytes.Equal(got, resumeDisk) || len(ranges()) != 2 {
			t.Errorf("wrote %d bytes, %d reads", len(got), len(ranges()))
		}
	})

	t.Run("a partial answer from another byte is not taken", func(t *testing.T) {
		t.Parallel()
		host, _ := blobHost(t, func(w http.ResponseWriter, n int, blob []byte, off int64) {
			if n == 1 {
				cut(w, blob, half)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(blob)-1, len(blob)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(blob)
		})
		c := New(authn.DefaultKeychain)
		d := pushDisk(t, c, host, "atlas.qcow2", resumeDisk)
		if _, _, err := c.Download(context.Background(), d, kind, kind, t.TempDir(), "f", nil); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("asked for bytes %d-", half)) {
			t.Errorf("err %v", err)
		}
	})

	t.Run("no new byte in a row fails after the bound, naming the blob", func(t *testing.T) {
		t.Parallel()
		host, ranges := blobHost(t, func(w http.ResponseWriter, n int, blob []byte, off int64) {
			switch {
			case n == 1:
				cut(w, blob, half)
			case n%2 == 0:
				cut(w, blob, 0)
			default:
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		})
		c := New(authn.DefaultKeychain)
		d := pushDisk(t, c, host, "atlas.qcow2", resumeDisk)
		dir := t.TempDir()
		_, _, err := c.Download(context.Background(), d, kind, kind, dir, "f", nil)
		if err == nil || !strings.Contains(err.Error(), "atlas.qcow2 (sha256:") || !strings.Contains(err.Error(), fmt.Sprintf("%d attempts in a row with no byte read, at %d of", downloadTries, half)) {
			t.Fatalf("err %v", err)
		}
		if n := len(ranges()); n != 1+downloadTries {
			t.Errorf("%d reads", n)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("left %v", entries)
		}
	})

	for _, code := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprintf("a %d is not retried", code), func(t *testing.T) {
			t.Parallel()
			host, ranges := blobHost(t, func(w http.ResponseWriter, n int, blob []byte, off int64) {
				if n == 1 && code == http.StatusNotFound {
					cut(w, blob, half) // refused on the resumed read
					return
				}
				w.WriteHeader(code)
			})
			c := New(authn.DefaultKeychain)
			d := pushDisk(t, c, host, "atlas.qcow2", resumeDisk)
			want := 1
			if code == http.StatusNotFound {
				want = 2
			}
			if _, _, err := c.Download(context.Background(), d, kind, kind, t.TempDir(), "f", nil); err == nil || !strings.Contains(err.Error(), strconv.Itoa(code)) || len(ranges()) != want {
				t.Errorf("err %v after %d reads", err, len(ranges()))
			}
		})
	}

	t.Run("a corrupted byte is refused, resumed or not", func(t *testing.T) {
		t.Parallel()
		for _, resume := range []bool{false, true} {
			host, _ := blobHost(t, func(w http.ResponseWriter, n int, blob []byte, off int64) {
				bad := bytes.Clone(blob)
				bad[len(bad)-3] ^= 1
				if resume && n == 1 {
					cut(w, bad, half)
					return
				}
				whole(w, bad, off)
			})
			c := New(authn.DefaultKeychain)
			d := pushDisk(t, c, host, "atlas.qcow2", resumeDisk)
			dir := t.TempDir()
			if _, _, err := c.Download(context.Background(), d, kind, kind, dir, "f", nil); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("its %d bytes are sha256:", len(resumeDisk))) {
				t.Errorf("resume %v: err %v", resume, err)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("resume %v: left %v", resume, entries)
			}
		}
	})

	t.Run("a cancelled context stops at once", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		host, ranges := blobHost(t, func(w http.ResponseWriter, n int, blob []byte, off int64) {
			cancel()
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		c := New(authn.DefaultKeychain)
		d := pushDisk(t, c, host, "atlas.qcow2", resumeDisk)
		if _, _, err := c.Download(ctx, d, kind, kind, t.TempDir(), "f", nil); !errors.Is(err, context.Canceled) || len(ranges()) != 1 {
			t.Errorf("err %v after %d reads", err, len(ranges()))
		}
	})
}
