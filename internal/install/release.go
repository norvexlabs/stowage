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
	"maps"
	"os/exec"
	"path"
	"slices"
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
	// Nftables are the services' nftables tables of one file (services.<name>.nftables), by service.
	Nftables map[string][]byte
	// NftByInput are the tables an input picks, by service, then by the input's value.
	NftByInput map[string]map[string][]byte
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
	r := &Release{Manifest: m, Spec: &s, Compose: files[payload.Compose], Keys: payloadKeys(files),
		Nftables: map[string][]byte{}, NftByInput: map[string]map[string][]byte{}}
	for n, b := range files {
		if t, ok := strings.CutPrefix(n, payload.NftablesDir); ok {
			t, nft := strings.CutSuffix(t, ".nft")
			name, value, picked := strings.Cut(t, "/")
			svc, declared := s.Services[name]
			switch {
			case !nft || !declared:
			case !picked && svc.Nftables.File != "":
				r.Nftables[name] = b
				continue
			case picked && svc.Nftables.Files[value] != "" && spec.NftChoice.MatchString(value):
				if r.NftByInput[name] == nil {
					r.NftByInput[name] = map[string][]byte{}
				}
				r.NftByInput[name][value] = b
				continue
			}
			return nil, fmt.Errorf("payload: %s is no table a service declares (services.<name>.nftables)", n)
		}
	}
	// The VM loads each table as root: a table of the project's name would be its own firewall's,
	// and a body beyond its table (another table, the ruleset flushed) would reach past it.
	for _, name := range sortedKeys(s.Services) {
		t := s.Services[name].Nftables
		if !t.Set() {
			continue
		}
		if (t.File != "") == (t.Input != "") || (t.Input != "") != (len(t.Files) > 0) {
			return nil, fmt.Errorf("payload: services.%s.nftables: a file, or an input and its files, not both nor either alone", name)
		}
		if in, ok := s.Input(t.Input); t.Input != "" && (!ok || in.Advanced || in.Secret || in.When != "" || in.Profile != "" ||
			len(in.Shapes) > 0 && (s.Shapes.Docker != nil && !slices.Contains(in.Shapes, "docker") || s.Shapes.Kairos != nil && !slices.Contains(in.Shapes, "kairos")) ||
			in.Default == "" && !in.Required) {
			return nil, fmt.Errorf("payload: services.%s.nftables.input: %s is no input asked on every install, in the clear", name, t.Input)
		}
		if name == s.Name || !spec.NftTableName.MatchString(name) || spec.NftKeyword(name) {
			return nil, fmt.Errorf("payload: services.%s.nftables: no table of the project's name (the VM's own firewall's), nor of a name nft does not take", name)
		}
		bodies := map[string][]byte{}
		if t.File != "" {
			body, ok := r.Nftables[name]
			if !ok {
				return nil, fmt.Errorf("payload: services.%s.nftables has no %s%s.nft", name, payload.NftablesDir, name)
			}
			bodies[""] = body
		}
		for _, v := range t.Values() {
			body, ok := r.NftByInput[name][v]
			if !ok {
				return nil, fmt.Errorf("payload: services.%s.nftables.files.%s has no %s%s/%s.nft", name, v, payload.NftablesDir, name, v)
			}
			bodies[v] = body
		}
		var sets []string
		for i, v := range sortedKeys(bodies) {
			got := spec.NftSets(string(bodies[v]))
			slices.Sort(got)
			if i == 0 {
				sets = got
			} else if !slices.Equal(got, sets) {
				return nil, fmt.Errorf("payload: services.%s.nftables: its files declare different sets", name)
			}
			if err := lint.CheckNftTable(string(bodies[v]), name); err != nil {
				at := "services." + name + ".nftables"
				if v != "" {
					at += ".files." + v
				}
				return nil, fmt.Errorf("payload: %s: %w", at, err)
			}
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
	// schema is its schema baseline (0: no database), read apart from Release (payloadBaseline):
	// told even when Release is nil; schemaErr is why it is not.
	schema    int
	schemaErr error
	// reset and compose are its data.reset and compose file, read as the baseline is: what a
	// crossing wipes of it (wipeOf) when Release is nil.
	reset    []string
	resetErr error
	compose  []byte
	outputs  string                 // its provision hook's outputs file, read as reset is
	volumes  map[string]spec.Volume // its data.volumes' keep_on_reset, read as reset is
}

// keptOf reads a kept release from its installer's payload files: Release nil, and unread why,
// when this Stowage refuses its payload.
func keptOf(files payload.Files) (*keptRelease, error) {
	m, err := payloadRelease(files)
	if err != nil {
		return nil, err
	}
	k := &keptRelease{Manifest: m, Keys: payloadKeys(files)}
	k.schema, k.schemaErr = payloadBaseline(files[payload.Spec])
	k.reset, k.volumes, k.outputs, k.resetErr = payloadReset(files[payload.Spec])
	k.compose = files[payload.Compose]
	k.Release, k.unread = Load(files)
	return k, nil
}

// payloadBaseline is the schema baseline a release's spec.json records, 0 when it declares no
// database. Only that is read of it, tolerating every other field: a spec of another Stowage,
// which Load refuses whole, still tells it. It is where every Stowage since 0.4.1 writes it
// (Database.SchemaCompat, the spec's Go field names); a test pins the path.
func payloadBaseline(raw []byte) (int, error) {
	var s struct {
		Database *struct{ SchemaCompat spec.Provider }
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("payload %s: %w", payload.Spec, err)
	}
	if s.Database == nil {
		return 0, nil
	}
	return providers.SchemaBaseline(s.Database.SchemaCompat)
}

// payloadReset is the data.reset a release's spec.json records, and its provision hook's outputs
// file, read as payloadBaseline reads the baseline (Data.Reset, Hooks.Provision.Config, the spec's
// Go field names since 0.4.1), every other field ignored.
func payloadReset(raw []byte) (reset []string, volumes map[string]spec.Volume, outputs string, err error) {
	var s struct {
		Data struct {
			Reset   []string
			Volumes map[string]json.RawMessage
		}
		Hooks struct {
			Provision *struct{ Config map[string]any }
		}
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, nil, "", fmt.Errorf("payload %s: %w", payload.Spec, err)
	}
	// Each volume read alone: one of another shape keeps nothing, and fails nothing else.
	for v, raw := range s.Data.Volumes {
		var d struct{ KeepOnReset bool }
		if json.Unmarshal(raw, &d) == nil && d.KeepOnReset {
			if volumes == nil {
				volumes = map[string]spec.Volume{}
			}
			volumes[v] = spec.Volume{KeepOnReset: true}
		}
	}
	if p := s.Hooks.Provision; p != nil {
		switch v := p.Config["outputs"].(type) {
		case nil:
		case string:
			outputs = v
		default:
			return nil, nil, "", fmt.Errorf("payload %s: hooks.provision outputs %v is not a file name", payload.Spec, v)
		}
	}
	return s.Data.Reset, volumes, outputs, nil
}

// payloadHostAPI is the shapes.kairos.host_api a release's spec.json records (Shapes.Kairos.HostAPI,
// the spec's Go field names), read as payloadBaseline reads the baseline: nil when it is unread.
func payloadHostAPI(raw []byte) []string {
	var s struct {
		Shapes struct {
			Kairos *struct{ HostAPI []string }
		}
	}
	if json.Unmarshal(raw, &s) != nil || s.Shapes.Kairos == nil {
		return nil
	}
	return s.Shapes.Kairos.HostAPI
}

// baseline is the kept release's schema baseline, 0 when it has no database.
func (k *keptRelease) baseline() (int, error) { return k.schema, k.schemaErr }

func (k *keptRelease) projectVersion() string { return k.Manifest.ProjectVersion }

// readKept reads the installer at exe the install keeps, refusing it when its manifest is not the
// release expect wants (nil: any), before anything else is read of it. A payload this Stowage
// refuses is another Stowage's when that installer reads it itself (readsItself), and an error
// otherwise: not as its Stowage wrote it, or not told.
func (in *Installer) readKept(ctx context.Context, exe string, expect func(*release.Manifest) error) (*keptRelease, error) {
	files, err := payload.Read(exe)
	if errors.Is(err, payload.ErrNone) {
		return nil, notCarried{err}
	}
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
	k, err := keptOf(files)
	if err != nil {
		return nil, err
	}
	if k.Release != nil {
		return k, nil
	}
	err = k.unread
	switch ierr := in.O.readsItself(ctx, exe); {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(ierr, errProbeTimeout):
		return nil, fmt.Errorf("%w, and whether that installer reads it is not known (%v)", err, ierr)
	case ierr != nil && answered(ierr):
		return nil, notCarried{fmt.Errorf("%w, nor does that installer read it (%v)", err, ierr)}
	case ierr != nil:
		// The probe did not run (exec refused, no interpreter, no memory): no answer of it.
		return nil, fmt.Errorf("%w, and whether that installer reads it is not known (%v)", err, ierr)
	}
	return k, nil
}

// answered reports whether a probe's error is the installer's own answer: it ran, and exited 1,
// the status an installer exits with on its own error (cmd/stowage-install). Any other — a start
// that failed, a Go runtime's fatal 2, a loader's 127, a signal — tells nothing of its payload.
func answered(err error) bool {
	var ee *exec.ExitError
	var code ExitCode
	return errors.As(err, &ee) && ee.ExitCode() == 1 || errors.As(err, &code) && code == 1
}

// notCarried is readKept's answer that the installer carries no release: a file with no payload,
// or an installer whose probe answered that it does not read its own (answered). A manifest this
// Stowage cannot decode is no answer: a failure.
type notCarried struct{ error }

func (e notCarried) Unwrap() error { return e.error }

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

// Tables are the services' table names, sorted: of one file, or picked by an input (the release's
// decoding holds them to the services' nftables).
func (r *Release) Tables() []string {
	out := sortedKeys(r.Nftables)
	for name := range r.NftByInput {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// hasTable reports whether the service name has a table.
func (r *Release) hasTable(name string) bool {
	_, one := r.Nftables[name]
	return one || len(r.NftByInput[name]) > 0
}

// tableSets are the named sets the service's table declares: those of any of its files, as each
// file of a table an input picks declares the same (lint).
func (r *Release) tableSets(name string) []string {
	if b, ok := r.Nftables[name]; ok {
		return spec.NftSets(string(b))
	}
	for _, v := range sortedKeys(r.NftByInput[name]) {
		return spec.NftSets(string(r.NftByInput[name][v]))
	}
	return nil
}

// TablesNow are the tables a start loads, by service: each of one file, and each an input picks
// by the value get reads. A value that names none of its files is an error naming the input and
// the values it takes.
func (r *Release) TablesNow(get func(string) string) (map[string][]byte, error) {
	out := maps.Clone(r.Nftables)
	var errs []error
	for _, name := range sortedKeys(r.NftByInput) {
		t := r.Spec.Services[name].Nftables
		v := get(t.Input)
		body, ok := r.NftByInput[name][v]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: %w", t.Input, pickError(name, t, v)))
			continue
		}
		out[name] = body
	}
	return out, errors.Join(errs...)
}

// pickError is the refusal of value v of the input picking the service's table t; its callers
// name the input.
func pickError(name string, t spec.NftTable, v string) error {
	return fmt.Errorf("%q picks no table of %s (services.%s.nftables.files: %s)", v, name, name, strings.Join(t.Values(), ", "))
}
