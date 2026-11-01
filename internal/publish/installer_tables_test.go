package publish

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// The installer carries each service's table: one file as nftables/<service>.nft, a table an
// input picks as nftables/<service>/<value>.nft, each its file's bytes (the layout the installer's
// release decoding reads).
func TestTheInstallerCarriesTheServicesTables(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "full")
	p, err := project.Open(root, "")
	if err != nil {
		t.Fatal(err)
	}
	files, err := (&Publisher{Project: p}).payloadFiles("release.json", []byte("{}"), *p.Spec)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for n := range files {
		if strings.HasPrefix(n, payload.NftablesDir) {
			got = append(got, n)
		}
	}
	slices.Sort(got)
	want := map[string]string{"nftables/ingest/enforce.nft": "deploy/ingest-enforce.nft", "nftables/ingest/observe.nft": "deploy/ingest.nft", "nftables/vpn.nft": "deploy/vpn.nft"}
	if !slices.Equal(got, []string{"nftables/ingest/enforce.nft", "nftables/ingest/observe.nft", "nftables/vpn.nft"}) {
		t.Fatalf("carried %v", got)
	}
	for n, f := range want {
		if b, _ := os.ReadFile(filepath.Join(root, f)); string(files[n]) != string(b) {
			t.Errorf("%s is not %s", n, f)
		}
	}
}
