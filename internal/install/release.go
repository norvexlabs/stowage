// Package install is the Docker shape of the installer binary (`<short>-stow`): it installs the
// release its payload carries with Docker Compose, and manages that install afterwards.
package install

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"strings"
	"time"

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
	// Nftables are the services' nftables tables (services.<name>.nftables), by service.
	Nftables map[string][]byte
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
	m, err := payloadRelease(files)
	if err != nil {
		return nil, err
	}
	var s spec.Spec
	dec := json.NewDecoder(bytes.NewReader(files[payload.Spec]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("payload %s: %w (an installer of another Stowage?)", payload.Spec, err)
	}
	if s.Name != m.Name {
		return nil, fmt.Errorf("payload: the spec is %q's, the release %q's", s.Name, m.Name)
	}
	if s.Short() != m.ShortName {
		return nil, fmt.Errorf("payload: the spec's short name is %q, the release's %q", s.Short(), m.ShortName)
	}
	r := &Release{Manifest: m, Spec: &s, Compose: files[payload.Compose], Keys: payloadKeys(files), Nftables: map[string][]byte{}}
	for n, b := range files {
		if t, ok := strings.CutPrefix(n, payload.NftablesDir); ok {
			name, nft := strings.CutSuffix(t, ".nft")
			if svc, declared := s.Services[name]; !nft || !declared || svc.Nftables == "" {
				return nil, fmt.Errorf("payload: %s is no table a service declares (services.<name>.nftables)", n)
			}
			r.Nftables[name] = b
		}
	}
	// The VM loads each table as root: a table of the project's name would be its own firewall's,
	// and a body beyond its table (another table, the ruleset flushed) would reach past it.
	for _, name := range sortedKeys(s.Services) {
		svc := s.Services[name]
		if svc.Nftables == "" {
			continue
		}
		body, ok := r.Nftables[name]
		switch {
		case name == s.Name || !spec.NftTableName.MatchString(name) || spec.NftKeyword(name):
			return nil, fmt.Errorf("payload: services.%s.nftables: no table of the project's name (the VM's own firewall's), nor of a name nft does not take", name)
		case !ok:
			return nil, fmt.Errorf("payload: services.%s.nftables has no %s%s.nft", name, payload.NftablesDir, name)
		}
		if err := lint.CheckNftTable(string(body), name); err != nil {
			return nil, fmt.Errorf("payload: services.%s.nftables: %w", name, err)
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

// payloadRelease is the release manifest an installer's payload carries, read alone: what an
// installer of another Stowage's release can be checked by, as its spec may hold fields this one
// does not know.
func payloadRelease(files payload.Files) (*release.Manifest, error) {
	published, pok := files[payload.Release]
	bundled, bok := files[payload.Bundled]
	switch {
	case pok && bok:
		return nil, fmt.Errorf("the installer's payload has both %s and %s", payload.Release, payload.Bundled)
	case pok:
		return release.Decode(published)
	case bok:
		return release.DecodeBundled(bundled)
	}
	return nil, fmt.Errorf("the installer's payload has no %s", payload.Release)
}

// payloadKeys are the trusted public keys an installer's payload carries, by file name.
func payloadKeys(files payload.Files) map[string][]byte {
	keys := map[string][]byte{}
	for n, b := range files {
		if strings.HasPrefix(n, payload.KeysDir) {
			keys[path.Base(n)] = b
		}
	}
	return keys
}

// keptRelease is the release an installer the install keeps carries (its own, or PrevDir's), read
// as every Stowage writes one: its release manifest and its trusted keys. Release is nil when this
// Stowage refuses its payload and that installer reads it (another Stowage's format: a key
// moved, a check that differs), which this one never reads otherwise, so what only its spec
// tells is that installer's own run's.
type keptRelease struct {
	Manifest *release.Manifest
	Keys     map[string][]byte
	Release  *Release
	unread   error // why Release is nil
}

// readKept reads the installer at exe the install keeps, refusing it when its manifest is not the
// release expect wants (nil: any), before anything else is read of it. A payload this Stowage
// refuses is another Stowage's when that installer reads it itself (readsItself), and an error
// otherwise: not as its Stowage wrote it, or not told.
func (in *Installer) readKept(ctx context.Context, exe string, expect func(*release.Manifest) error) (*keptRelease, error) {
	files, err := payload.Read(exe)
	if err != nil {
		return nil, err
	}
	m, err := payloadRelease(files)
	if err != nil {
		return nil, err
	}
	if expect != nil {
		if err := expect(m); err != nil {
			return nil, err
		}
	}
	k := &keptRelease{Manifest: m, Keys: payloadKeys(files)}
	if k.Release, err = Load(files); err == nil {
		return k, nil
	}
	switch ierr := in.O.readsItself(ctx, exe); {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(ierr, errProbeTimeout):
		return nil, fmt.Errorf("%w, and whether that installer reads it is not known (%v)", err, ierr)
	case ierr != nil:
		return nil, fmt.Errorf("%w, nor does that installer read it (%v)", err, ierr)
	}
	k.unread = err
	return k, nil
}

// errProbeTimeout is readsItself's installer that did not answer within readsItselfWithin.
var errProbeTimeout = errors.New("it did not answer")

// readsItselfWithin bounds readsItself: reading a payload and printing a help takes moments.
var readsItselfWithin = 30 * time.Second

// readsItself runs the installer at exe as far as reading its own payload, which every installer
// does before anything else (its help printed, nothing run): nil when it reads it. A run stopped
// meanwhile is its own error, never a verdict on the payload.
func (o *Options) readsItself(ctx context.Context, exe string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if o.Exec != nil {
		return o.Exec(ctx, exe, []string{"--help"})
	}
	pctx, cancel := context.WithTimeout(ctx, readsItselfWithin)
	defer cancel()
	stderr := &capped{limit: 4 << 10}
	cmd := exec.CommandContext(pctx, exe, "--help")
	cmd.Stdout, cmd.Stderr = io.Discard, stderr
	cmd.WaitDelay = time.Second // a child left holding its output does not hold the bound
	err := cmd.Run()
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case pctx.Err() != nil:
		return fmt.Errorf("%w within %s", errProbeTimeout, readsItselfWithin)
	case err != nil:
		if msg := strings.TrimSpace(stderr.buf.String()); msg != "" {
			if stderr.over {
				msg += " …"
			}
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// foreign says why the kept release's spec is not read.
func (k *keptRelease) foreign() string {
	return fmt.Sprintf("%s %s (revision %d), built by Stowage %s, carries a spec this one does not read (its own installer does): %v",
		k.Manifest.Name, k.Manifest.ProjectVersion, k.Manifest.Revision, cmp.Or(k.Manifest.StowageVersion, "of no recorded version"), k.unread)
}

// checkFront refuses to install a release of a project with a front that records no front image.
// Only the release being
// installed needs one: a kept or previous release is read without it.
func (r *Release) checkFront() error {
	if _, ok := r.Manifest.Artifacts.Images[front.Key]; !ok && r.Spec.HasFront() {
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
