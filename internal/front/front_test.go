package front

import (
	"regexp"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/dockerfile"
)

var pinned = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

// The recipe builds the pinned Caddy, from bases pinned by digest, and never 2.11.6.
func TestRecipe(t *testing.T) {
	t.Parallel()
	f, err := dockerfile.Parse(Dockerfile)
	if err != nil {
		t.Fatal(err)
	}
	froms := f.Froms()
	if len(froms) != 2 {
		t.Fatalf("FROMs = %v", froms)
	}
	for _, fr := range froms {
		if !pinned.MatchString(fr.Image) {
			t.Errorf("FROM %s is not pinned by digest", fr.Image)
		}
		if !strings.HasPrefix(fr.Image, "caddy:"+CaddyVersion+"-") {
			t.Errorf("FROM %s is not Caddy %s", fr.Image, CaddyVersion)
		}
	}
	src := string(Dockerfile)
	for _, want := range []string{
		"xcaddy build v" + CaddyVersion + " ",
		"github.com/caddy-dns/cloudflare@v",
		"grep -q '^v" + regexp.QuoteMeta(CaddyVersion) + " '",
		"dns.providers.cloudflare",
		// The layer4 app, by a tagged release, which an L4 route's TLS termination needs.
		"github.com/mholt/caddy-l4@v0.",
		"grep -qx 'layer4.handlers.tls'",
		"grep -qx 'layer4.matchers.remote_ip'",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the recipe lacks %q", want)
		}
	}
	if CaddyVersion == "2.11.6" || strings.Contains(strings.ReplaceAll(src, "Never Caddy 2.11.6", ""), "2.11.6") {
		t.Error("Caddy 2.11.6 cuts every response to a request with a body at 60 s")
	}
}

// The front's binary carries no file capability: the base image's is taken off and checked absent
// before the capability tools go, as a front with every capability dropped could not exec it.
func TestTheBinaryCarriesNoFileCapability(t *testing.T) {
	t.Parallel()
	s := string(Dockerfile)
	strip := strings.Index(s, "setcap -r /usr/bin/caddy")
	// getcap's own failure (missing) fails the build too, and the check gates the rest (&&).
	check := strings.Index(s, `caps="$(getcap /usr/bin/caddy)" && [ -z "$caps" ] && \`)
	copied := strings.Index(s, "COPY --from=builder /usr/bin/caddy /usr/bin/caddy")
	gone := strings.Index(s, "apk del --no-cache curl libcap libcap-utils")
	if copied < 0 || strip < copied || check < strip || gone < check {
		t.Errorf("the binary's capability is not taken off after its copy and checked before libcap goes:\n%s", s)
	}
}
