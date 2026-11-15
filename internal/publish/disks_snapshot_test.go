package publish

import (
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// A snapshot finds a repository however its reference spells it: a manifest deleted through
// docker.io/acme/notes drops the tags read through acme/notes.
func TestATagSnapshotKeysRepositoriesCanonically(t *testing.T) {
	t.Parallel()
	h, _ := v1.NewHash("sha256:" + strings.Repeat("a", 64))
	other, _ := v1.NewHash("sha256:" + strings.Repeat("b", 64))
	read, err := name.NewRepository("acme/notes")
	if err != nil {
		t.Fatal(err)
	}
	s := tagSnapshot{snapKey(read): {"1.0.0": h, "1.1.0": other}}
	d, err := name.NewDigest("docker.io/acme/notes@" + h.String())
	if err != nil {
		t.Fatal(err)
	}
	s.dropDigest(d)
	tag, _ := name.NewTag("index.docker.io/acme/notes:1.1.0")
	s.drop(tag)
	if m, _ := s.tagsOf(read); len(m) != 0 {
		t.Fatalf("left %v", m)
	}
}
