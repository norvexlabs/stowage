package publish

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"

	"github.com/norvexlabs/stowage/internal/registry"
)

// With registry.pull none, a repository an install cannot read anonymously stops ship: before
// anything is built when it exists, after the push that created it otherwise (before the bundle,
// or before a channel points at the release), and a run once it is public carries on.
func TestShipStopsAtARepositoryAnInstallCannotRead(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.reg = registry.New(registry.Fixed(&authn.Basic{Username: "pub", Password: "s3cret"}))
	e.anon = registry.New(authn.NewMultiKeychain())
	e.private = []string{"/acme/notes/notes-web/"}
	img := e.host + "/acme/notes/notes-web"

	// A new image repository, private once pushed: refused before the bundle and its disks.
	_, err := e.ship("stable", 1)
	if err == nil || !strings.Contains(err.Error(), "registry.pull is none") || !strings.HasSuffix(err.Error(), "and "+img+" refuse an anonymous read: make each public (a registry that creates a repository private, as ghcr does, needs it made public once, in its package settings), or give registry.pull an account; then ship again") {
		t.Fatalf("%v", err)
	}
	eq(t, "bundle built", slices.ContainsFunc(e.builder.built, func(r BuildRequest) bool { return r.Image == "os" }), false)
	eq(t, "images built", len(e.builder.built) > 0, true)
	eq(t, "disks", len(e.disks.made), 0)

	// Made public: the same inputs carry on.
	e.private = []string{"/nothing/"}
	rel := e.mustShip("stable", 1)
	eq(t, "stable", e.digest("notes-release", "stable"), rel.Digest.String())

	// Private again, it exists: the next release stops before it builds anything.
	e.private = []string{"/acme/notes/notes-web/"}
	e.bump("1.1.0")
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), img+" refuse an anonymous read") || !strings.HasSuffix(err.Error(), "; then ship again") {
		t.Fatalf("%v", err)
	}
	eq(t, "built", len(e.builder.built), 0)

	// A repository the release's last writes create (the chart of a channel never shipped): the
	// release is published, and no channel points at it.
	e.private = []string{"/acme/notes/charts/notes-beta/"}
	if _, err := e.ship("beta", 1); err == nil || !strings.Contains(err.Error(), "charts/notes-beta refuse an anonymous read") ||
		!strings.Contains(err.Error(), "release beta-1.1.0 is published, no channel points at it yet") {
		t.Fatalf("%v", err)
	}
	eq(t, "beta before", e.digest("notes-release", "beta"), "")
	e.private = []string{"/nothing/"}
	beta := e.mustShip("beta", 1)
	eq(t, "beta", e.digest("notes-release", "beta"), beta.Digest.String())
}

// A registry may answer a refused anonymous read as an unknown repository; and the release
// manifest's own repository, created by the run's last writes, is checked before a channel points
// at it.
func TestShipStopsAtARepositoryThatAnswersUnknown(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.reg = registry.New(registry.Fixed(&authn.Basic{Username: "pub", Password: "s3cret"}))
	e.anon = registry.New(authn.NewMultiKeychain())
	e.private, e.privateAs404 = []string{"/acme/notes/notes-release/"}, true
	if _, err := e.ship("stable", 1); err == nil || !strings.Contains(err.Error(), "/acme/notes/notes-release refuse an anonymous read") ||
		!strings.Contains(err.Error(), "release 1.0.0 is published, no channel points at it yet") {
		t.Fatalf("%v", err)
	}
	eq(t, "stable", e.digest("notes-release", "stable"), "")

	// It exists now: the next run stops before it builds.
	e.bump("1.1.0")
	if _, err := e.ship("stable", 1); err == nil || !strings.HasSuffix(err.Error(), "notes-release refuse an anonymous read: make each public (a registry that creates a repository private, as ghcr does, needs it made public once, in its package settings), or give registry.pull an account; then ship again") {
		t.Fatalf("%v", err)
	}
	eq(t, "built", len(e.builder.built), 0)
}
