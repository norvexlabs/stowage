package registry

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestReadProxyReadsOneRepositoryWithTheAccount(t *testing.T) {
	t.Parallel()
	reg := ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0)))
	// The registry refuses every request without the account.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "pub" || p != "secret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	c := New(Fixed(&authn.Basic{Username: "pub", Password: "secret"}))
	ctx := context.Background()
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"acme/notes/os", "acme/notes/other"} {
		tag, err := c.Tag(host+"/"+repo, "v1")
		if err != nil {
			t.Fatal(err)
		}
		if err := c.WriteImage(ctx, tag, img); err != nil {
			t.Fatal(err)
		}
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	osRef, err := c.Digest(host+"/acme/notes/os", d.String())
	if err != nil {
		t.Fatal(err)
	}
	source, acct, stop, err := c.ReadProxy(ctx, osRef)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	addr, _, _ := strings.Cut(source, "/")
	if source != addr+"/acme/notes/os@"+d.String() || acct.Username == "" || len(acct.Password) != 64 {
		t.Fatalf("source %s, account %+v", source, acct)
	}
	ref, err := name.NewDigest(source, name.StrictValidation)
	if err != nil {
		t.Fatal(err)
	}
	// Without the proxy's account, nothing is read through it.
	anon := New(authn.NewMultiKeychain())
	if _, err := remote.Image(ref, anon.opts(ctx)...); err == nil {
		t.Error("read through the proxy without its account")
	}
	// With it, as the disk builder reads (its account in a Docker config file).
	tool := New(Fixed(&authn.Basic{Username: acct.Username, Password: acct.Password}))
	got, err := remote.Image(ref, tool.opts(ctx)...)
	if err != nil {
		t.Fatalf("read through the proxy: %v", err)
	}
	if gd, _ := got.Digest(); gd != d {
		t.Errorf("digest %s, want %s", gd, d)
	}
	if layers, _ := got.Layers(); len(layers) != 1 {
		t.Fatalf("%d layers", len(layers))
	} else if _, err := layers[0].Compressed(); err != nil {
		t.Errorf("blob: %v", err)
	}
	// The registry itself refuses that account: the proxy's own was needed.
	if _, err := remote.Image(osRef, tool.opts(ctx)...); err == nil {
		t.Error("the registry took the proxy's account: the test proves nothing")
	}

	sv := "/v2/acme/notes/os"
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/v2/", 200},
		{"HEAD", sv + "/manifests/" + d.String(), 200},
		{"GET", sv + "/manifests/v1", 404}, // by tag: another image the repository holds
		{"GET", sv + "/manifests/sha256:" + strings.Repeat("0", 64), 404},
		{"GET", sv + "/blobs/sha256:x%3Fdigest=sha256:y", 404},
		{"GET", sv + "/blobs/sha256:x%23y", 404},
		{"PUT", sv + "/manifests/v2", 405},
		{"POST", sv + "/blobs/uploads/", 405},
		{"DELETE", sv + "/manifests/" + d.String(), 405},
		{"GET", sv + "/blobs/uploads/x", 404},
		{"GET", "/v2/acme/notes/other/manifests/" + d.String(), 404},
		{"GET", sv + "/manifests/../../other/manifests/v1", 404},
		{"GET", sv + "/manifests/..%2F..%2Fother%2Fmanifests%2Fv1", 404},
		{"GET", sv + "/tags/list", 404},
		{"GET", "/v2/_catalog", 404},
	} {
		req, err := http.NewRequest(c.method, "http://"+addr+c.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
		for _, withAccount := range []bool{true, false} {
			if withAccount {
				req.SetBasicAuth(acct.Username, acct.Password)
			} else {
				req.SetBasicAuth(acct.Username, strings.Repeat("0", 64))
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if want := map[bool]int{true: c.want, false: 401}[withAccount]; resp.StatusCode != want {
				t.Errorf("%s %s (account %v): %d, want %d", c.method, c.path, withAccount, resp.StatusCode, want)
			}
		}
	}
}
