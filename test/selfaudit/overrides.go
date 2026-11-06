package main

import (
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/norvexlabs/stowage/internal/backup"
	"github.com/norvexlabs/stowage/internal/chown"
	"github.com/norvexlabs/stowage/internal/front"
	"github.com/norvexlabs/stowage/internal/sbom"
)

// recipe is an image Stowage supplies. Upstream is the project whose release it builds with modules
// of its own choosing, empty when it builds none.
type recipe struct {
	key      string
	file     []byte
	upstream string
}

var recipes = []recipe{
	{front.Key, front.Dockerfile, "Caddy"},
	{backup.Key, backup.Dockerfile, "rclone"},
	{chown.Key, chown.Dockerfile, ""},
	{sbom.Key, sbom.Dockerfile, "grype"},
}

// override is a module a recipe forces over the version its upstream's release ships.
type override struct{ module, version string }

var (
	replaceFlag = regexp.MustCompile(`--replace\s+([^=\s]+)=(\S+)`)
	goGet       = regexp.MustCompile(`\bgo get\s+([^&;|]+)`)
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
		out = append(out, override{m[1], version})
	}
	for _, m := range goGet.FindAllStringSubmatch(text, -1) {
		program := true
		for _, f := range strings.Fields(m[1]) {
			if strings.HasPrefix(f, "-") {
				continue
			}
			if !program {
				module, version, _ := strings.Cut(f, "@")
				out = append(out, override{module, version})
			}
			program = false
		}
	}
	return out
}

// remind prints every override and what is owed for it. It decides nothing: an override's
// finding is gone from the scan, so only this line says the fix is still not upstream's.
func remind(w io.Writer, rs []recipe) {
	some := false
	for _, r := range rs {
		for _, o := range overrides(r.file) {
			fmt.Fprintf(w, "override: %s forces %s %s over what %s's release ships\n", r.key, o.module, o.version, r.upstream)
			some = true
		}
	}
	if some {
		fmt.Fprintln(w, "reminder: each override above is a stopgap, not the upstream's fix. Check whether that upstream's latest release now carries the module at that version or later; if it does, move the recipe to that release and drop the override.")
	}
}
