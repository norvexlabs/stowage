package backup

import (
	"regexp"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/dockerfile"
)

var pinned = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

// The recipe is the pinned rclone, its base pinned by digest, its version checked.
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
	if fr := froms[0].Image; !pinned.MatchString(fr) || !strings.HasPrefix(fr, "rclone/rclone:"+RcloneVersion+"@") {
		t.Errorf("FROM %s is not rclone %s pinned by digest", fr, RcloneVersion)
	}
	if want := "grep -q '^rclone v" + regexp.QuoteMeta(RcloneVersion) + "$'"; !strings.Contains(string(Dockerfile), want) {
		t.Errorf("the recipe lacks %q", want)
	}
}
