package registry

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// proxiedHeaders are the response headers a read through ReadProxy passes on.
var proxiedHeaders = []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Docker-Content-Digest", "ETag"}

// ReadProxy serves one image of the registry read-only, with the client's account, on a loopback
// port, for a tool that must not hold the account (a third-party image): it reads source
// (<addr>/<repo>@<digest>) with an account of the proxy's own — a random password, new for each
// proxy, which the tool is given in a file, never on a command line — so another user of the
// machine cannot read through it. Only the image's manifest (by its digest), the repository's
// blobs and the /v2/ ping pass, by GET or HEAD. The client's account never leaves the process; a
// registry that issues tokens scopes it to pulling the repository, and against a Basic-auth
// registry the proxy's own filter is what limits it. stop closes it.
func (c *Client) ReadProxy(ctx context.Context, d name.Digest) (source string, account authn.AuthConfig, stop func(), err error) {
	repo := d.Context()
	auth, err := c.keychain.Resolve(repo)
	if err != nil {
		return "", authn.AuthConfig{}, nil, fmt.Errorf("%s: %w", repo, err)
	}
	rt, err := transport.NewWithContext(ctx, repo.Registry, auth, c.rt, []string{repo.Scope(transport.PullScope)})
	if err != nil {
		return "", authn.AuthConfig{}, nil, fmt.Errorf("%s: %w", repo, err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", authn.AuthConfig{}, nil, err
	}
	account = authn.AuthConfig{Username: proxyUser, Password: hex.EncodeToString(secret)}
	upstream := &url.URL{Scheme: repo.Scheme(), Host: repo.RegistryStr()}
	h := readProxy(upstream, repo.RepositoryStr(), d.DigestStr(), account.Password, &http.Client{Transport: rt})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", authn.AuthConfig{}, nil, err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(l) }()
	return l.Addr().String() + "/" + repo.RepositoryStr() + "@" + d.DigestStr(), account, func() { _ = srv.Close() }, nil
}

// proxyUser is the user name of a ReadProxy's own account.
const proxyUser = "stowage-proxy"

// readProxy forwards, for a request with the proxy's account (password), the reads of the
// manifest digest and the blobs of the repository repo to the registry upstream, with client.
func readProxy(upstream *url.URL, repo, digest, password string, client *http.Client) http.Handler {
	prefix := "/v2/" + repo + "/"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		u, pw, ok := r.BasicAuth()
		authorized := ok && u == proxyUser && subtle.ConstantTimeCompare([]byte(pw), []byte(password)) == 1
		switch {
		case !authorized:
			w.Header().Set("WWW-Authenticate", `Basic realm="stowage"`)
			http.Error(w, "the proxy's account is required", http.StatusUnauthorized)
			return
		case r.Method != http.MethodGet && r.Method != http.MethodHead:
			http.Error(w, "read only", http.StatusMethodNotAllowed)
			return
		case p == "/v2/" || p == "/v2":
			w.WriteHeader(http.StatusOK)
			return
		// The image's manifest and its repository's blobs, by a path that is already clean and
		// unescaped: nothing else is reached through "..", "//" or an escaped "/", "?" or "#".
		case r.URL.RawPath != "" || path.Clean(p) != p || strings.ContainsAny(p, "?#") ||
			p != prefix+"manifests/"+digest && !strings.HasPrefix(p, prefix+"blobs/sha256:"):
			http.NotFound(w, r)
			return
		}
		target := *upstream
		target.Path = p
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		for _, k := range []string{"Accept", "Range", "If-None-Match"} {
			for _, v := range r.Header.Values(k) {
				req.Header.Add(k, v)
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for _, k := range proxiedHeaders {
			for _, v := range resp.Header.Values(k) {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
}
