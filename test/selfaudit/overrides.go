package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/norvexlabs/stowage/internal/backup"
	"github.com/norvexlabs/stowage/internal/chown"
	"github.com/norvexlabs/stowage/internal/front"
	"github.com/norvexlabs/stowage/internal/sbom"
)

// recipe is an image Stowage supplies, named by the file it embeds. Upstream is the project whose
// release it builds with modules of its own choosing, and module that release's Go module; both
// are empty when it builds none.
type recipe struct {
	key      string
	path     string
	file     []byte
	upstream string
	module   string
}

var recipes = []recipe{
	{front.Key, "internal/front/Dockerfile", front.Dockerfile, "Caddy", "github.com/caddyserver/caddy/v2"},
	{backup.Key, "internal/backup/Dockerfile", backup.Dockerfile, "rclone", "github.com/rclone/rclone"},
	{chown.Key, "internal/chown/Dockerfile", chown.Dockerfile, "", ""},
	{sbom.Key, "internal/sbom/Dockerfile", sbom.Dockerfile, "grype", "github.com/anchore/grype"},
}

// override is a module a recipe forces over the version its upstream's release ships, and the
// form that forces it (what to drop when the release catches up).
type override struct{ module, version, form string }

var (
	replaceFlag = regexp.MustCompile(`--replace\s+([^=\s]+)=(\S+)`)
	goGet       = regexp.MustCompile(`\bgo get\s+([^&;|]+)`)
	xcaddyBuild = regexp.MustCompile(`\bxcaddy build\s+(\S+)`)
)

// instructions is the recipe with its comments gone and its continuation lines joined.
func instructions(file []byte) string {
	var kept []string
	for _, line := range strings.Split(string(file), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			kept = append(kept, line)
		}
	}
	return strings.ReplaceAll(strings.Join(kept, "\n"), "\\\n", " ")
}

// overrides reads them from a recipe, so the list cannot fall behind it: each --replace, named by
// the module it replaces, and each module of a `go get` after the first, the program built.
func overrides(file []byte) []override {
	text := instructions(file)
	var out []override
	for _, m := range replaceFlag.FindAllStringSubmatch(text, -1) {
		_, version, ok := strings.Cut(m[2], "@")
		if !ok {
			version = m[2]
		}
		out = append(out, override{m[1], version, "--replace"})
	}
	for _, m := range goGet.FindAllStringSubmatch(text, -1) {
		program := true
		for _, f := range strings.Fields(m[1]) {
			if strings.HasPrefix(f, "-") {
				continue
			}
			if !program {
				module, version, _ := strings.Cut(f, "@")
				out = append(out, override{module, version, "go get"})
			}
			program = false
		}
	}
	return out
}

// release is the version of its upstream the recipe builds: the program's of its `go get` when
// that program is in the upstream's module, else the version `xcaddy build` names.
func release(r recipe) (string, error) {
	text := instructions(r.file)
	version := ""
	for _, m := range goGet.FindAllStringSubmatch(text, -1) {
		for _, f := range strings.Fields(m[1]) {
			if strings.HasPrefix(f, "-") {
				continue
			}
			if path, v, _ := strings.Cut(f, "@"); path == r.module || strings.HasPrefix(path, r.module+"/") {
				version = v
			}
			break
		}
	}
	if version == "" {
		if m := xcaddyBuild.FindStringSubmatch(text); m != nil {
			version = m[1]
		}
	}
	if version == "" {
		return "", fmt.Errorf("%s: no release of %s (%s) is named by a `go get` or an `xcaddy build`", r.path, r.upstream, r.module)
	}
	if !semver.IsValid(version) {
		return "", fmt.Errorf("%s: %s's release %q is not a module version", r.path, r.upstream, version)
	}
	return version, nil
}

// fetchMod returns a module's go.mod at a version.
type fetchMod func(ctx context.Context, module, version string) ([]byte, error)

// proxyMod reads it from the Go module proxy, which serves it for every tagged release.
func proxyMod(ctx context.Context, mod, version string) ([]byte, error) {
	url, err := modURL(mod, version)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// modURL is where the proxy serves a module's go.mod: its path and version case-encoded.
func modURL(mod, version string) (string, error) {
	path, err := module.EscapePath(mod)
	if err != nil {
		return "", err
	}
	v, err := module.EscapeVersion(version)
	if err != nil {
		return "", err
	}
	return "https://proxy.golang.org/" + path + "/@v/" + v + ".mod", nil
}

// requirement is the version a go.mod requires of a module, "" when it requires none.
func requirement(gomod []byte, mod string) (string, error) {
	f, err := modfile.ParseLax("go.mod", gomod, nil)
	if err != nil {
		return "", err
	}
	for _, r := range f.Require {
		if r.Mod.Path == mod {
			return r.Mod.Version, nil
		}
	}
	return "", nil
}

// checkOverrides prints every override live or unjudged and returns one error per stale one: an
// override whose upstream's release already requires the module at its version or past it holds
// nothing up, and is dropped. A module the release does not require is left to judge by hand.
func checkOverrides(ctx context.Context, w io.Writer, rs []recipe, fetch fetchMod) error {
	var errs []error
	some := false
	for _, r := range rs {
		forced := overrides(r.file)
		if len(forced) == 0 {
			continue
		}
		tag, err := release(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		gomod, err := fetch(ctx, r.module, tag)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %s %s's go.mod: %w", r.path, r.upstream, tag, err))
			continue
		}
		for _, o := range forced {
			if !semver.IsValid(o.version) {
				errs = append(errs, fmt.Errorf("%s: the %s of %s forces %q, not a module version", r.path, o.form, o.module, o.version))
				continue
			}
			req, err := requirement(gomod, o.module)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %s %s's go.mod: %w", r.path, r.upstream, tag, err))
				break
			}
			switch {
			case req == "":
				fmt.Fprintf(w, "override: %s forces %s %s over what %s's release ships (not in %s %s's go.mod: judge it by hand)\n", r.key, o.module, o.version, r.upstream, r.upstream, tag)
				some = true
			case semver.Compare(req, o.version) >= 0:
				errs = append(errs, fmt.Errorf("override: %s forces %s %s, and %s %s already requires %s: the override is stale, drop the %s of %s from %s", r.key, o.module, o.version, r.upstream, tag, req, o.form, o.module, r.path))
			default:
				fmt.Fprintf(w, "override: %s forces %s %s over the %s %s %s requires\n", r.key, o.module, o.version, req, r.upstream, tag)
				some = true
			}
		}
	}
	if some {
		fmt.Fprintln(w, "reminder: each override above is a stopgap, not the upstream's fix. Check whether that upstream's latest release now carries the module at that version or later; if it does, move the recipe to that release and drop the override.")
	}
	return errors.Join(errs...)
}
