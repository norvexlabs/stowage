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
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the recipe lacks %q", want)
		}
	}
	if CaddyVersion == "2.11.6" || strings.Contains(strings.ReplaceAll(src, "Never Caddy 2.11.6", ""), "2.11.6") {
		t.Error("Caddy 2.11.6 cuts every response to a request with a body at 60 s")
	}
}
