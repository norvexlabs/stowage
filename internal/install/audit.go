package install

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/spf13/cobra"

	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/sbom"
	"github.com/norvexlabs/stowage/internal/sigverify"
)

// AuditOptions pick the release an audit or an SBOM export reads: the one this installer carries,
// or Version's (its latest revision not yanked, or Revision) on Channel ("" is the install's, else
// this installer's). DB is a grype database archive to scan with ("" fetches one).
type AuditOptions struct {
	Version  string
	Channel  string
	Revision int
	DB       string
}

// auditCache is the directory, in the install's, that keeps the scanner's database from one audit
// to the next.
const auditCache = ".stowage-grype"

// auditSource is what an audit or an export reads: the release, the registry it is read from
// (prefix, with its client), the keys its SBOMs must be attested by, and, for an install made
// offline (it pulls no image), the prefix its images were loaded under.
type auditSource struct {
	m       *release.Manifest
	prefix  string
	loaded  string
	c       *registry.Client
	trust   *sigverify.Verifier
	offline bool
}

// auditRelease is the release an audit or an export reads. In a directory holding this project's
// install: the release it runs (or --version's), from the registry it pulls from, with the keys it
// trusts (as for its upgrades); elsewhere (an operator's machine): this installer's release (or
// --version's), from --registry or this installer's registry, with this installer's keys.
func (in *Installer) auditRelease(ctx context.Context, o AuditOptions) (*auditSource, error) {
	src := &auditSource{m: in.R.Manifest, prefix: strings.TrimSuffix(in.R.Spec.Registry.Prefix, "/")}
	env, envErr := in.open()
	if envErr != nil && !notInstalled(in) {
		// An install there whose record does not read: its keys are not known, so none is assumed.
		return nil, fmt.Errorf("%s: %w", in.O.Dir, envErr)
	}
	channel := in.R.Manifest.Channel
	if envErr == nil {
		pname, cur, rev, err := sameProject(in.O.Dir, env, in.R.Manifest)
		if err != nil {
			return nil, err
		}
		if p, _ := env.Get("STOWAGE_REGISTRY"); p != "" {
			src.prefix = strings.TrimSuffix(p, "/")
			if _, err := registry.Repository(src.prefix); err != nil {
				return nil, fmt.Errorf("%s: STOWAGE_REGISTRY %q: %w", in.path(envName), p, err)
			}
		}
		// The install's keys decide, as for its upgrades: an older installer never brings back a
		// key a later release dropped; and the release audited is the one it runs.
		var kept *Release
		if src.trust, kept, err = in.installTrust(pname, cur, rev); err != nil {
			return nil, err
		}
		src.m, channel = kept.Manifest, kept.Manifest.Channel
		if ch, _ := env.Get(ChannelVar); ch != "" {
			channel = ch
		}
	}
	src.loaded = src.prefix
	if in.O.Registry != "" {
		src.prefix = strings.TrimSuffix(in.O.Registry, "/")
		if _, err := registry.Repository(src.prefix); err != nil {
			return nil, fmt.Errorf("--registry %q: %w", in.O.Registry, err)
		}
	}
	src.offline = in.auditOffline(env, envErr)
	var err error
	if src.trust == nil {
		if src.trust, err = trusted("this installer", in.R.Keys); err != nil {
			return nil, err
		}
	}
	if src.trust == nil {
		return nil, fmt.Errorf("%s does not sign its releases: they carry no SBOM", in.R.Spec.Name)
	}
	if src.c, err = in.registryClient(src.prefix); err != nil {
		return nil, err
	}
	if o.Version == "" {
		return src, nil
	}
	channel = cmp.Or(o.Channel, channel)
	if channel == "" {
		return nil, errors.New("--version: --channel is required: this installer's release names no channel")
	}
	if src.m, err = findRevision(ctx, src.c, src.prefix, in.R.Spec.Name, channel, o.Version, o.Revision, src.trust, in.O.logf); err != nil {
		return nil, err
	}
	return src, nil
}

// attested reads a target's SBOM, attested by a key of trust.
func attested(c *registry.Client, trust *sigverify.Verifier) sbom.Attested {
	return func(ctx context.Context, d name.Digest) ([]byte, error) {
		return trust.Attestation(ctx, c, d, sbom.PredicateType)
	}
}

// Audit reads the SBOM each artifact of the release carries, attested by a key it trusts, and scans
// it on the local engine with the release's own scanner, read from the registry it reads.
func (in *Installer) Audit(ctx context.Context, o AuditOptions) (*sbom.Report, error) {
	// --offline pulls no image: another release's scanner never reaches the engine, and nothing
	// is read for it.
	if o.Version != "" && in.O.Offline {
		return nil, errors.New("--version: --offline pulls no image: audit the release the install runs, without --version")
	}
	src, err := in.auditRelease(ctx, o)
	if err != nil {
		return nil, err
	}
	// An install made offline pulls no image: another release's scanner never reaches it.
	if o.Version != "" && src.offline {
		return nil, errors.New("--version: this install was made offline and never pulls: audit the release it runs, without --version")
	}
	m := src.m
	if m.Artifacts.Images[sbom.Key] == "" {
		return nil, fmt.Errorf("%s %s (revision %d) carries no scanner: a release shipped before audit existed has no SBOM", m.Name, m.ProjectVersion, m.Revision)
	}
	targets, err := sbom.Targets(src.prefix, m.Name, m.Artifacts)
	if err != nil {
		return nil, err
	}
	image, err := in.scannerImage(ctx, src)
	if err != nil {
		return nil, err
	}
	cache := filepath.Join(in.O.Dir, auditCache)
	if notInstalled(in) {
		dir, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		cache = filepath.Join(dir, "stowage", "grype")
	}
	s := sbom.Scanner{Docker: in.dockerRun, Image: image, Cache: cache, User: fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), DB: o.DB}
	rep, err := sbom.Audit(ctx, attested(src.c, src.trust), s, targets, in.O.logf)
	if err != nil {
		return nil, err
	}
	rep.Project, rep.Version, rep.Revision, rep.Channel, rep.Tag = m.Name, m.ProjectVersion, m.Revision, m.Channel, m.Tag
	return &rep, nil
}

// ExportSBOMs writes the SBOM each artifact of the release carries, attested by a key it trusts,
// into dir, with its index.
func (in *Installer) ExportSBOMs(ctx context.Context, o AuditOptions, dir string) (sbom.Index, error) {
	src, err := in.auditRelease(ctx, o)
	if err != nil {
		return sbom.Index{}, err
	}
	m := src.m
	targets, err := sbom.Targets(src.prefix, m.Name, m.Artifacts)
	if err != nil {
		return sbom.Index{}, err
	}
	return sbom.Export(ctx, attested(src.c, src.trust), targets, dir, sbom.Index{Project: m.Name, Version: m.ProjectVersion, Revision: m.Revision, Channel: m.Channel, Tag: m.Tag})
}

// auditOffline: the audit pulls no image. An install made offline (--offline, or its images
// recorded by tag) has its images loaded. The SBOMs are read from the registry either way.
func (in *Installer) auditOffline(env *EnvFile, envErr error) bool {
	return in.O.Offline || envErr == nil && !pinnedImages(env)
}

// dockerRun runs the Docker CLI as the scanner does.
func (in *Installer) dockerRun(ctx context.Context, stdout io.Writer, args ...string) error {
	_, err := in.O.Docker.Run(ctx, Cmd{Args: args, Stdout: stdout})
	return err
}

// errLoadEnded closes the scanner's pipe once `docker load` returned: the writer's error is then
// docker's to explain.
var errLoadEnded = errors.New("docker load ended")

// scannerImage is the image ID the release's scanner runs by: the one loaded by tag when the
// install was made offline under the install's own prefix (it never pulls, and audits the release
// it runs alone), else the registry's by digest, read with the registry client (its pull account,
// the CA it trusts) and loaded into the engine under a local name: the engine's own trust is never
// changed for an audit. Whichever it is, it runs only when its image is the release's: its layers'
// diff IDs and how it starts are those of the config the signed release names (an engine's image
// ID is no config digest once it stores images in containerd); and it runs by the ID checked, so
// a tag moved meanwhile runs nothing else.
func (in *Installer) scannerImage(ctx context.Context, src *auditSource) (string, error) {
	m := src.m
	h, err := v1.NewHash(m.Artifacts.Images[sbom.Key])
	if err != nil {
		return "", fmt.Errorf("the scanner's digest: %w", err)
	}
	ref, err := src.c.Digest(src.prefix+"/"+m.Name+"-"+sbom.Key, h.String())
	if err != nil {
		return "", err
	}
	img, err := src.c.Image(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("the scanner: %w", err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return "", fmt.Errorf("the scanner: %w", err)
	}
	// What runs: the layers (an engine checks each against its diff ID), and how the image starts.
	// The engine's config is read whole: a field the image leaves out is absent there, which a
	// template naming it refuses.
	type startConfig struct {
		Entrypoint []string
		Cmd        []string
		Env        []string
		WorkingDir string
		User       string
	}
	type started struct {
		ID     string
		Layers []string
		Config startConfig
	}
	c := cf.Config
	want := started{Config: startConfig{c.Entrypoint, c.Cmd, c.Env, c.WorkingDir, c.User}}
	for _, d := range cf.RootFS.DiffIDs {
		want.Layers = append(want.Layers, d.String())
	}
	// is reads the image local names: its ID when it is the release's scanner, "" when the engine
	// holds none of the name.
	is := func(local string) (string, error) {
		out, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"image", "inspect", "--format",
			`{"ID":{{json .Id}},"Layers":{{json .RootFS.Layers}},"Config":{{json .Config}}}`, local}})
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "no such image") {
				return "", nil
			}
			return "", fmt.Errorf("the scanner %s: %w", local, err)
		}
		var have started
		if err := json.Unmarshal(out, &have); err != nil || have.ID == "" || !slices.Equal(have.Layers, want.Layers) {
			return "", fmt.Errorf("%s is not the release's scanner: its layers are not %s's", local, ref)
		}
		h, w := have.Config, want.Config
		if !slices.Equal(h.Entrypoint, w.Entrypoint) || !slices.Equal(h.Cmd, w.Cmd) || !slices.Equal(h.Env, w.Env) || h.WorkingDir != w.WorkingDir || h.User != w.User {
			return "", fmt.Errorf("%s is not the release's scanner: its layers or how it starts are not %s's", local, ref)
		}
		return have.ID, nil
	}
	if src.offline {
		local := fmt.Sprintf("%s/%s-%s:%s", src.loaded, m.Name, sbom.Key, m.Tag)
		id, err := is(local)
		if err != nil || id != "" {
			return id, err
		}
		return "", fmt.Errorf("the install was made offline: the scanner %s is not loaded (docker load it, from the release's mirror export)", local)
	}
	local := scannerLocal(src.prefix, m.Name, h.Hex)
	// One tagged so by another image (another tool, a load that went wrong) is loaded over.
	if id, err := is(local); err == nil && id != "" {
		return id, nil
	}
	tag, err := name.NewTag(local)
	if err != nil {
		return "", err
	}
	in.O.logf("pulling %s as %s", ref, local)
	pr, pw := io.Pipe()
	wrote := make(chan error, 1)
	go func() {
		err := tarball.Write(tag, img, pw)
		pw.CloseWithError(err)
		wrote <- err
	}()
	_, err = in.O.Docker.Run(ctx, Cmd{Args: []string{"load", "--quiet"}, Stdin: pr})
	pr.CloseWithError(errLoadEnded)
	// The reading's own error first (docker's would be an unexpected end of its input), unless it
	// is only that docker stopped reading.
	if werr := <-wrote; werr != nil && !errors.Is(werr, errLoadEnded) {
		return "", fmt.Errorf("the scanner: %w", werr)
	}
	if err != nil {
		return "", fmt.Errorf("the scanner: docker load: %w", err)
	}
	id, err := is(local)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("the scanner: docker load left no %s", local)
	}
	return id, nil
}

// releaseFlags pick the release, as AuditOptions; the registry's bind to o, which prepare copies.
func releaseFlags(c *cobra.Command, f *AuditOptions, o *Options) {
	c.Flags().StringVar(&f.Version, "version", "", "this version's latest revision (or --revision's), not the release this installer carries")
	c.Flags().StringVar(&f.Channel, "channel", "", "the channel to look on with --version (default: the install's, else this installer's)")
	c.Flags().IntVar(&f.Revision, "revision", 0, "this revision of --version")
	c.Flags().StringVar(&o.Registry, "registry", "", "the prefix to read from (a mirror); default: the install's, else this installer's")
	c.Flags().StringVar(&o.RegistryCA, "registry-ca", "", "the CA its certificate chains to, a PEM file")
}

// auditCommand is `audit`, run where the installer runs (a Docker host, an operator's machine): the
// scanner on the local engine.
func auditCommand(in *Installer, o *Options, prepare func() error, dirFlag func(*cobra.Command), stdout io.Writer) *cobra.Command {
	var f AuditOptions
	var failOn string
	var asJSON bool
	c := &cobra.Command{
		Use:   "audit",
		Short: "Check a release's SBOMs are attested by a trusted key and scan them for known vulnerabilities",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			min, on, err := sbom.FailOn(failOn)
			if err != nil {
				return err
			}
			if err := prepare(); err != nil {
				return err
			}
			rep, err := in.Audit(cmd.Context(), f)
			if err != nil {
				return err
			}
			if asJSON {
				err = rep.JSON(stdout)
			} else {
				err = rep.Print(stdout)
			}
			if err != nil {
				return err
			}
			return rep.Verdict(min, on)
		},
	}
	releaseFlags(c, &f, o)
	c.Flags().StringVar(&f.DB, "db", "", "a grype database archive to scan with (grype's listing has them), for a host that does not reach grype's servers")
	c.Flags().StringVar(&failOn, "fail-on", "none", "exit non-zero on a vulnerability at or above this severity: none, negligible, low, medium, high, critical")
	c.Flags().BoolVar(&o.Offline, "offline", false, "the images are loaded: take the scanner loaded by tag and pull no image (the SBOMs are still read from the registry)")
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	dirFlag(c)
	return c
}

// sbomCommand is `sbom export DIR`: the release's SBOMs, verified, as files for the operator's
// own tools.
func sbomCommand(in *Installer, o *Options, prepare func() error, dirFlag func(*cobra.Command), stdout io.Writer) *cobra.Command {
	root := &cobra.Command{Use: "sbom", Short: "The release's SBOMs (SPDX), attested by a trusted key"}
	var f AuditOptions
	export := &cobra.Command{
		Use:   "export DIR",
		Short: "Write each artifact's SBOM, verified, into DIR, with index.json naming them",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := prepare(); err != nil {
				return err
			}
			idx, err := in.ExportSBOMs(cmd.Context(), f, args[0])
			for _, a := range idx.Artifacts {
				if a.File != "" {
					fmt.Fprintf(stdout, "%-12s %s  %s\n", a.Name, a.Digest, filepath.Join(args[0], a.File))
				}
			}
			return err
		},
	}
	releaseFlags(export, &f, o)
	dirFlag(export)
	root.AddCommand(export)
	return root
}

// scannerLocal is the name a scanner read from the registry prefix is loaded as: the prefix's
// hash in it, so a scanner of the same digest read from another registry is another image, and
// removing one never takes the other.
func scannerLocal(prefix, project, hex string) string {
	p := sha256.Sum256([]byte(prefix))
	return fmt.Sprintf("stowage.local/%x/%s-%s:%s", p[:4], project, sbom.Key, hex[:16])
}
