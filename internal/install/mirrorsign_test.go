package install

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"

	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// countingMirror is a mirror registry's prefix, and the count of the requests that write to it.
func countingMirror(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	writes := &atomic.Int64{}
	reg := ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writes.Add(1)
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://") + "/site/notes", writes
}

// trusting is v's installer trusting the keys of signers alone.
func trusting(v *vmCreate, signers ...*testutil.KeySigner) *Installer {
	in := v.installer(nil)
	r := *in.R
	r.Keys = map[string][]byte{}
	for i, s := range signers {
		r.Keys[string(rune('a'+i))+".pub"] = s.PEM
	}
	in.R = &r
	return in
}

// An import checks, before it pushes anything, that the archive's release and each yank it carries
// are signed by a key the importing installer trusts, reading the signatures from the archive:
// unsigned, signed by another key, or signed over another digest, it is refused with nothing
// written to the mirror; signed by a trusted key, it is imported.
func TestMirrorImportChecksTheArchivesSignatures(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	first := withDisks(t, v)
	ctx := context.Background()
	a, b, foreign := testutil.NewKeySigner(t), testutil.NewKeySigner(t), testutil.NewKeySigner(t)
	export := func(in *Installer) string {
		t.Helper()
		f := filepath.Join(t.TempDir(), "notes.oci.tar")
		if err := in.MirrorExport(ctx, MirrorExportRequest{File: f}); err != nil {
			t.Fatalf("%v\n%s", err, v.log)
		}
		return f
	}
	refused := func(what string, in *Installer, file string, want ...string) {
		t.Helper()
		to, writes := countingMirror(t)
		err := in.MirrorImport(ctx, file, to, false)
		r, ok := Refusal(err)
		if !ok {
			t.Fatalf("%s: not refused: %v", what, err)
		}
		if !strings.HasPrefix(r.Fix, "export the release again from its registry") || !strings.Contains(r.Fix, "notes-stow of a release whose keys sign it") {
			t.Errorf("%s: fix %q", what, r.Fix)
		}
		for _, w := range append(want, "nothing imported") {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: %v, want %q", what, err, w)
			}
		}
		if n := writes.Load(); n != 0 {
			t.Errorf("%s: %d writes to the mirror", what, n)
		}
	}
	c := registry.New(authn.NewMultiKeychain())
	ref, _ := c.Tag(v.prefix+"/notes-release", first.Tag)
	d, err := c.Get(ctx, ref)
	if err != nil || d == nil {
		t.Fatal(err)
	}
	rel := ref.Context().Digest(d.Digest.String())

	refused("unsigned", trusting(v, a), export(v.installer(nil)), "notes 1.0.0 (revision 1, "+first.Tag+") is not signed by a key this install trusts", "no signature")
	// A signature by the trusted key over another digest, as a referrer of the release's.
	a.SignAs(t, rel, ref.Context().Digest(first.Artifacts.Installer))
	refused("signed over another digest", trusting(v, a), export(v.installer(nil)), "1 signature(s), none by a trusted key")
	a.Sign(t, rel)
	signed := export(v.installer(nil))
	refused("by another key", trusting(v, foreign), signed, "none by a trusted key")
	to, writes := countingMirror(t)
	if err := trusting(v, a).MirrorImport(ctx, signed, to, false); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	if writes.Load() == 0 {
		t.Error("nothing written to the mirror")
	}

	// A yank carried is checked too: the release signed by b as well, its later yanked publication
	// by a alone.
	later := *first
	later.ProjectVersion, later.Tag, later.ReleaseSeq = "1.1.0", "1.1.0-1", first.ReleaseSeq+1
	later.Artifacts.Disks = nil
	later.Artifacts.BootDisks = &release.BootDisks{Release: first.Tag, Stowage: "0.6.0", Disks: first.Artifacts.Disks}
	later.Yanked = &release.Yank{Reason: "broken", Date: later.Date}
	v.putRelease(later.Tag, &later)
	v.signRelease(later.Tag, a)
	b.Sign(t, rel)
	withYank := export(trusting(v, a))
	if got := archived(t, withYank); !strings.Contains(strings.Join(got, " "), "notes-release:"+later.Tag) {
		t.Fatalf("the yank is not carried: %v", got)
	}
	refused("a yank by another key", trusting(v, b), withYank, "notes 1.1.0 (revision 1, "+later.Tag+") is not signed", "1 signature(s), none by a trusted key")
	if err := trusting(v, a).MirrorImport(ctx, withYank, to, false); err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
}
