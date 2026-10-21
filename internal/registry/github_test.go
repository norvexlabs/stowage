package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
)

// A manifest is deleted as the package version named by its digest, under a user when no
// organisation has the package; the token goes as a bearer and never into an error.
func TestGitHubPackagesDeletesTheVersionOfADigest(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/orgs/"):
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/users/acme/packages/container/notes%2Fdisk",
			r.Method == http.MethodGet && r.URL.RawPath == "/users/acme/packages/container/notes%2Fdisk":
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.EscapedPath(), "/versions"):
			if r.URL.Query().Get("page") != "1" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":7,"name":"sha256:` + strings.Repeat("a", 64) + `"},{"id":9,"name":"sha256:` + strings.Repeat("b", 64) + `"}]`))
		case r.Method == http.MethodDelete:
			mu.Lock()
			deleted = append(deleted, r.URL.EscapedPath())
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, r.Method+" "+r.URL.EscapedPath(), http.StatusTeapot)
		}
	}))
	defer srv.Close()
	g := &GitHubPackages{API: srv.URL, Token: "s3cret", HTTP: srv.Client()}
	d, err := name.NewDigest("ghcr.io/acme/notes/disk@sha256:" + strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.DeleteManifest(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "/users/acme/packages/container/notes%2Fdisk/versions/9" {
		t.Fatalf("deleted %v", deleted)
	}
	// A digest no version holds deletes nothing.
	none, _ := name.NewDigest("ghcr.io/acme/notes/disk@sha256:" + strings.Repeat("c", 64))
	if err := g.DeleteManifest(context.Background(), none); err != nil || len(deleted) != 1 {
		t.Fatalf("%v %v", err, deleted)
	}
	// A refused token is an error naming the scope, never the token.
	g.Token = "wrong"
	err = g.DeleteManifest(context.Background(), d)
	if err == nil || !strings.Contains(err.Error(), "delete:packages") || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("got %v", err)
	}
}
