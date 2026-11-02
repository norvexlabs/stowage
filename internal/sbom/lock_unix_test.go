//go:build unix

package sbom

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
)

// An audit waits for the cache another audit holds, saying so, and gives up when its context
// ends; the holder released, the next one takes it.
func TestAuditWaitsForTheCache(t *testing.T) {
	t.Parallel()
	cache := t.TempDir()
	unlock, err := lockCache(context.Background(), cache, nil)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := name.NewDigest("reg.example/acme/notes-api@sha256:" + strings.Repeat("a", 64))
	read := func(context.Context, name.Digest) ([]byte, error) { return []byte(`{"spdxVersion":"SPDX-2.3"}`), nil }
	f := &fakeDocker{out: map[string]string{"sbom:": grypeOut}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var said []string
	logf := func(format string, a ...any) { said = append(said, fmt.Sprintf(format, a...)) }
	_, err = Audit(ctx, read, Scanner{Docker: f.run, Cache: cache}, []Target{{Name: "api", Digest: d}}, logf)
	if err == nil || !strings.Contains(err.Error(), "waiting for the scanner's cache") || len(f.runs) != 0 {
		t.Fatalf("%v, %d runs", err, len(f.runs))
	}
	if len(said) != 1 || !strings.Contains(said[0], "another audit or ship holds it") {
		t.Errorf("said %q", said)
	}
	unlock()
	if _, err := Audit(context.Background(), read, Scanner{Docker: f.run, Cache: cache}, []Target{{Name: "api", Digest: d}}, nil); err != nil {
		t.Fatalf("after the release: %v", err)
	}
}

// A second audit waits while the cache is held, saying so once however long it waits, and scans
// once the holder releases it. Not parallel: it shortens lockPoll.
func TestAuditTakesTheCacheOnceReleased(t *testing.T) {
	saved := lockPoll
	lockPoll = time.Millisecond
	t.Cleanup(func() { lockPoll = saved })
	cache := t.TempDir()
	unlock, err := lockCache(context.Background(), cache, nil)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := name.NewDigest("reg.example/acme/notes-api@sha256:" + strings.Repeat("a", 64))
	read := func(context.Context, name.Digest) ([]byte, error) { return []byte(`{"spdxVersion":"SPDX-2.3"}`), nil }
	f := &fakeDocker{out: map[string]string{"sbom:": grypeOut}}
	waiting := make(chan struct{})
	var once sync.Once
	var said []string
	logf := func(format string, a ...any) {
		said = append(said, fmt.Sprintf(format, a...))
		if strings.HasPrefix(format, "waiting") {
			once.Do(func() { close(waiting) })
		}
	}
	done := make(chan error)
	go func() {
		_, err := Audit(context.Background(), read, Scanner{Docker: f.run, Cache: cache}, []Target{{Name: "api", Digest: d}}, logf)
		done <- err
	}()
	<-waiting
	// Several polls go by before the release: the wait is said once.
	for range 5 {
		if ok, err := tryLock(cache); ok || !errors.Is(err, context.Canceled) {
			t.Fatalf("the cache while held: free %v, %v", ok, err)
		}
		time.Sleep(5 * lockPoll)
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range said {
		if strings.HasPrefix(s, "waiting") {
			n++
		}
	}
	if n != 1 || len(f.runs) == 0 {
		t.Errorf("said %q, %d runs", said, len(f.runs))
	}
}

// tryLock reports whether the cache's lock is free, releasing it at once.
func tryLock(dir string) (bool, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	u, err := lockCache(ctx, dir, nil)
	if err != nil {
		return false, err
	}
	u()
	return true, nil
}
