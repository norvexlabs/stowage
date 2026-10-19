// Package install is the Docker shape of the installer binary (`<short>-stow`): it installs the
// release its payload carries with Docker Compose, and manages that install afterwards.
package install

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/norvexlabs/stowage/internal/front"
	"github.com/norvexlabs/stowage/internal/lint"
	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
)

// Release is the release an installer carries.
type Release struct {
	Manifest *release.Manifest
	Spec     *spec.Spec // the runtime view
	Compose  []byte
	Keys     map[string][]byte // trusted public keys by file name
	// Pull is the project's pull account, baked in at ship time; nil when the project has none.
	Pull *payload.Account
}

// Ctl is the name of the release's installer, <short_name>-stow.
func (r *Release) Ctl() string { return providers.Ctl(r.Spec.Short()) }

// Load reads a release from an installer's payload.
func Load(files payload.Files) (*Release, error) {
	for _, f := range []string{payload.Spec, payload.Compose} {
		if _, ok := files[f]; !ok {
			return nil, fmt.Errorf("the installer's payload has no %s", f)
		}
	}
	published, pok := files[payload.Release]
	bundled, bok := files[payload.Bundled]
	var m *release.Manifest
	var err error
	switch {
	case pok && bok:
		return nil, fmt.Errorf("the installer's payload has both %s and %s", payload.Release, payload.Bundled)
	case pok:
		m, err = release.Decode(published)
	case bok:
		m, err = release.DecodeBundled(bundled)
	default:
		return nil, fmt.Errorf("the installer's payload has no %s", payload.Release)
	}
	if err != nil {
		return nil, err
	}
	var s spec.Spec
	dec := json.NewDecoder(bytes.NewReader(files[payload.Spec]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("payload %s: %w (an installer from a newer Stowage?)", payload.Spec, err)
	}
	if s.Name != m.Name {
		return nil, fmt.Errorf("payload: the spec is %q's, the release %q's", s.Name, m.Name)
	}
	if s.Short() != m.ShortName {
		return nil, fmt.Errorf("payload: the spec's short name is %q, the release's %q", s.Short(), m.ShortName)
	}
	r := &Release{Manifest: m, Spec: &s, Compose: files[payload.Compose], Keys: map[string][]byte{}}
	for n, b := range files {
		if strings.HasPrefix(n, payload.KeysDir) {
			r.Keys[path.Base(n)] = b
		}
	}
	if raw, ok := files[payload.Pull]; ok {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r.Pull); err != nil {
			return nil, fmt.Errorf("payload %s: %w", payload.Pull, err)
		}
		if err := dec.Decode(&json.RawMessage{}); err != io.EOF {
			return nil, fmt.Errorf("payload %s: trailing data", payload.Pull)
		}
		if r.Pull == nil || r.Pull.User == "" || r.Pull.Password == "" {
			return nil, fmt.Errorf("payload %s: the pull account needs a user and a password", payload.Pull)
		}
	}
	for k := range s.Images {
		if _, ok := m.Artifacts.Images[k]; !ok {
			return nil, fmt.Errorf("payload: image %s is not in the release", k)
		}
	}
	return r, nil
}

// checkFront refuses to install a release that records no front image. Only the release being
// installed needs one: a kept or previous release is read without it.
func (r *Release) checkFront() error {
	if _, ok := r.Manifest.Artifacts.Images[front.Key]; !ok {
		return fmt.Errorf("%s %s has no front image (published by a Stowage without one): this installer cannot install it", r.Manifest.Name, r.Manifest.ProjectVersion)
	}
	return nil
}

// ImageRef is where an image of the release is pulled from, by digest.
func (r *Release) ImageRef(registryPrefix, key string) string {
	return fmt.Sprintf("%s/%s-%s@%s", registryPrefix, r.Spec.Name, key, r.Manifest.Artifacts.Images[key])
}

// ImageVar is the compose variable that names an image.
func ImageVar(key string) string { return lint.ImageVar(key) }
