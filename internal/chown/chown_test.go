package chown

import (
	"regexp"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/dockerfile"
)

var pinned = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

// The recipe is the pinned busybox, its base pinned by digest, its version checked.
func TestRecipe(t *testing.T) {
	t.Parallel()
	f, err := dockerfile.Parse(Dockerfile)
	if err != nil {
		t.Fatal(err)
	}
	froms := f.Froms()
	if len(froms) != 1 {
		t.Fatalf("FROMs = %v", froms)
	}
	if fr := froms[0].Image; !pinned.MatchString(fr) || !strings.HasPrefix(fr, "busybox:"+BusyboxVersion+"@") {
		t.Errorf("FROM %s is not busybox %s pinned by digest", fr, BusyboxVersion)
	}
	if want := "grep -q '^BusyBox v" + regexp.QuoteMeta(BusyboxVersion) + " '"; !strings.Contains(string(Dockerfile), want) {
		t.Errorf("the recipe lacks %q", want)
	}
}
