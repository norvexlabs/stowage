package backup

import (
	"regexp"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/dockerfile"
)

var pinned = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

// The recipe builds rclone of RcloneVersion with Go and runs it on Alpine, both bases pinned by
// digest, its version checked.
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
	}
	if !strings.HasPrefix(froms[0].Image, "golang:") || !strings.HasPrefix(froms[1].Image, "alpine:") {
		t.Errorf("FROMs = %v: rclone built with Go, run on Alpine", froms)
	}
	for _, want := range []string{
		"go get github.com/rclone/rclone@v" + RcloneVersion + " ",
		"-X github.com/rclone/rclone/fs.Version=v" + RcloneVersion + "'",
		"grep -q '^rclone v" + regexp.QuoteMeta(RcloneVersion) + "$'",
	} {
		if !strings.Contains(string(Dockerfile), want) {
			t.Errorf("the recipe lacks %q", want)
		}
	}
}
