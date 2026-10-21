package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
)

// GitHubPackages deletes manifests of a GitHub container registry through GitHub's packages API:
// the registry itself deletes nothing through the distribution API. A manifest is a package
// version, named by its digest; deleting it removes its tags.
type GitHubPackages struct {
	// API is GitHub's REST API root (https://api.github.com, or an Enterprise server's
	// https://<host>/api/v3).
	API   string
	Token string
	HTTP  *http.Client
}

// maxGitHubAnswer bounds one answer of the packages API read.
const maxGitHubAnswer = 8 << 20

// githubVersion is a package version as the API lists it.
type githubVersion struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// DeleteManifest deletes the package version holding d; nothing when there is none. The
// repository's path under its host is <owner>/<package>, the package being the rest of the path.
func (g *GitHubPackages) DeleteManifest(ctx context.Context, d name.Digest) error {
	owner, pkg, ok := strings.Cut(d.Context().RepositoryStr(), "/")
	if !ok || owner == "" || pkg == "" {
		return fmt.Errorf("delete %s: the repository is not <owner>/<package>", d)
	}
	base, err := g.packageURL(ctx, owner, pkg)
	if err != nil {
		return fmt.Errorf("delete %s: %w", d, err)
	}
	if base == "" {
		return nil // no such package: nothing to delete
	}
	for page := 1; ; page++ {
		var vs []githubVersion
		found, err := g.call(ctx, http.MethodGet, fmt.Sprintf("%s/versions?per_page=100&page=%d", base, page), &vs)
		if err != nil {
			return fmt.Errorf("delete %s: %w", d, err)
		}
		if !found || len(vs) == 0 {
			return nil
		}
		for _, v := range vs {
			if v.Name == d.DigestStr() {
				if _, err := g.call(ctx, http.MethodDelete, fmt.Sprintf("%s/versions/%d", base, v.ID), nil); err != nil {
					return fmt.Errorf("delete %s: %w", d, err)
				}
				return nil
			}
		}
		if len(vs) < 100 {
			return nil
		}
	}
}

// packageURL is the package's API path, of an organisation or else of a user; "" when neither
// has it.
func (g *GitHubPackages) packageURL(ctx context.Context, owner, pkg string) (string, error) {
	root := strings.TrimSuffix(g.API, "/")
	p := url.PathEscape(pkg)
	for _, kind := range []string{"orgs", "users"} {
		u := fmt.Sprintf("%s/%s/%s/packages/container/%s", root, kind, url.PathEscape(owner), p)
		found, err := g.call(ctx, http.MethodGet, u, nil)
		if err != nil {
			return "", err
		}
		if found {
			return u, nil
		}
	}
	return "", nil
}

// call sends one request; it reports false on a 404, and decodes a 2xx answer into out when set.
// The token is never in an error.
func (g *GitHubPackages) call(ctx context.Context, method, u string, out any) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+g.Token)
	hc := g.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return false, fmt.Errorf("%s %s: %w", method, u, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGitHubAnswer))
	if err != nil {
		return false, fmt.Errorf("%s %s: %w", method, u, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		var e struct{ Message string }
		_ = json.Unmarshal(body, &e)
		msg := e.Message
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		hint := ""
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
			hint = " (the token needs delete:packages)"
		}
		return false, fmt.Errorf("%s %s: %d %s%s", method, u, resp.StatusCode, msg, hint)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return false, fmt.Errorf("%s %s: %w", method, u, err)
		}
	}
	return true, nil
}
