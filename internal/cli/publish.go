package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/spf13/cobra"

	"github.com/norvexlabs/stowage/internal/kairos"
	"github.com/norvexlabs/stowage/internal/lint"
	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/publish"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/spec"
)

// lintGate refuses to publish a project with lint errors; warnings are printed and pass.
func (o *options) lintGate() error {
	if err := o.requires(); err != nil {
		return err
	}
	res, err := lint.Run(o.root, o.specFile(), o.now())
	if err != nil {
		return err
	}
	if res.Errors() == 0 {
		return nil
	}
	for _, f := range res.Findings {
		fmt.Fprintln(o.err, f)
	}
	return fmt.Errorf("lint: %d error(s): fix them before publishing", res.Errors())
}

// access is the credential a command needs.
type access int

const (
	readAccess  access = iota // registry.pull: what operators hold
	writeAccess               // registry.push: the publisher's
	listAccess                // registry.pull, reading manifests only: no signature is checked
)

// publisher opens a Publisher on the project's registry. Read-only commands use the pull account,
// so a check never needs the publisher's secret.
func (o *options) publisher(p *project.Project, a access) (*publish.Publisher, error) {
	cred, key := p.Spec.Registry.Push, "registry.push"
	if a != writeAccess {
		cred, key = p.Spec.Registry.Pull, "registry.pull"
	}
	kc, err := registry.Keychain(cred, o.getenv)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	builder, disks, workDir := o.builder, o.disks, ""
	kairosShape := p.Spec.Shapes.Kairos != nil
	var up publish.CloudUploader
	var cloudVars map[string]map[string]string
	var cloudErr error
	if a == writeAccess && (builder == nil || kairosShape) {
		// Builds land in the user's cache, on disk: an OS bundle and its disks are gigabytes.
		cache, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		if builder == nil {
			builder = &publish.DockerBuilder{WorkDir: filepath.Join(cache, "stowage", "build"), Stdout: o.err, Stderr: o.err}
		}
		if kairosShape {
			workDir = filepath.Join(cache, "stowage", "os")
			if disks == nil {
				disks = &kairos.DiskBuilder{WorkDir: filepath.Join(cache, "stowage", "disks"), Stdout: o.err, Stderr: o.err}
			}
			// Only ship and promote register images: the refusal is theirs, not yank's or prune's.
			up, cloudVars, cloudErr = o.clouds(p, cache)
		}
	}
	// A large blob's push logs its progress where the run logs.
	reg := registry.New(kc, append(slices.Clip(o.registryOpts), registry.WithLog(o.err))...)
	var signer publish.Signer
	if a != listAccess {
		if signer, err = o.signer(p, kc, a, reg); err != nil {
			return nil, err
		}
	}
	var pull *payload.Account
	var del publish.Deleter
	if a == writeAccess {
		if pull, err = o.pullAccount(p); err != nil {
			return nil, err
		}
		if del, err = o.deleter(p); err != nil {
			return nil, err
		}
	}
	return &publish.Publisher{
		Project:        p,
		Signer:         signer,
		Registry:       reg,
		Upstream:       registry.New(authn.DefaultKeychain, o.registryOpts...),
		Anonymous:      registry.New(authn.NewMultiKeychain(), o.registryOpts...),
		Builder:        builder,
		Disks:          disks,
		WorkDir:        workDir,
		Cloud:          up,
		CloudVars:      cloudVars,
		CloudErr:       cloudErr,
		Deleter:        del,
		StowageVersion: o.buildVersion,
		Installer:      o.installer,
		Pull:           pull,
		Now:            o.now,
		Log:            o.err,
	}, nil
}

// DisksEnv set to 1 or true publishes the disks as --disks does: for a hook ship, from the
// commit's environment.
const DisksEnv = "STOWAGE_SHIP_DISKS"

func (o *options) envDisks() bool {
	v := strings.ToLower(strings.TrimSpace(o.getenv(DisksEnv)))
	return v == "1" || v == "true"
}

// deleter is how the publisher deletes a manifest by digest (registry.delete): nil, the
// registry's distribution API, for oci.
func (o *options) deleter(p *project.Project) (publish.Deleter, error) {
	d := p.Spec.Registry.Delete
	if d.Or("oci").Name != "github" {
		return nil, nil
	}
	c, err := providers.GitHubDelete(d, p.Spec.Registry.Push)
	if err != nil {
		return nil, fmt.Errorf("registry.delete: %w", err)
	}
	token := o.getenv(c.TokenEnv)
	if token == "" {
		return nil, fmt.Errorf("registry.delete: %s is not set: the github provider deletes with it", c.TokenEnv)
	}
	return &registry.GitHubPackages{API: c.API, Token: token}, nil
}

// pullAccount is the project's pull account as the installer bakes it in: the token provider's
// user, with its password from the publisher's environment; nil when registry.pull is not a token
// or the password is not set (ship refuses then, naming the variable).
func (o *options) pullAccount(p *project.Project) (*payload.Account, error) {
	if p.Spec.Registry.Pull.Name != "token" {
		return nil, nil
	}
	c, err := providers.Token(p.Spec.Registry.Pull)
	if err != nil {
		return nil, fmt.Errorf("registry.pull: %w", err)
	}
	pw := o.getenv(c.TokenEnv)
	if pw == "" {
		return nil, nil
	}
	return &payload.Account{User: c.User, Password: pw}, nil
}

// keyURI matches the key forms cosign reads other than a file: env://, KMS and Vault URIs, pkcs11:.
var keyURI = regexp.MustCompile(`^[a-z][a-z0-9+.-]*:`)

// signer returns the project's signer, nil when it does not sign; it reads signatures with reg,
// the publisher's client. A read-only command gets a verify-only signer that reaches the
// registry with the pull account.
func (o *options) signer(p *project.Project, kc authn.Keychain, a access, reg *registry.Client) (publish.Signer, error) {
	if !p.Spec.Signs() {
		return nil, nil
	}
	c, err := providers.CosignKey(p.Spec.Signing)
	if err != nil {
		return nil, fmt.Errorf("signing: %w", err)
	}
	repo, err := name.NewRepository(p.Spec.Registry.Prefix, name.StrictValidation)
	if err != nil {
		return nil, err
	}
	auth, err := kc.Resolve(repo.Registry)
	if err != nil {
		return nil, err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	s := publish.CosignSigner{Cosign: o.getenv("STOWAGE_COSIGN"), Registry: repo.Registry, Auth: auth, WorkDir: filepath.Join(cache, "stowage", "sign"),
		Reader: reg}
	for _, k := range c.PublicKeys {
		path, err := spec.ProjectPath(p.Root, k)
		if err != nil {
			return nil, fmt.Errorf("signing.public_keys: %w", err)
		}
		s.PublicKeys = append(s.PublicKeys, path)
	}
	if a == writeAccess {
		s.Key = c.Key
		if !keyURI.MatchString(c.Key) && !filepath.IsAbs(c.Key) {
			base := p.Root
			if o.keyRoot != "" {
				base = o.keyRoot
			}
			s.Key = filepath.Join(base, c.Key)
		}
	}
	signer, err := publish.NewCosignSigner(s)
	if err != nil || a != writeAccess || o.test.Key == "" {
		return signer, err
	}
	// A test release is signed with the run's key alone: the key given must be the one its public
	// key names, so no form of the project's key (env://, a copy re-encrypted) signs it.
	if err := o.test.sameKey(context.Background(), signer, s.PublicKeys); err != nil {
		return nil, err
	}
	return signer, nil
}

// openPublisher opens the project, its publisher and the channel (the default one when empty).
func (o *options) openPublisher(channel *string, a access) (*project.Project, *publish.Publisher, error) {
	p, err := o.open()
	if err != nil {
		return nil, nil, err
	}
	if *channel == "" {
		*channel = defaultChannel(p.Spec.Channels, p.Spec.ChannelNames())
	}
	if _, ok := p.Spec.Channels[*channel]; !ok {
		return nil, nil, fmt.Errorf("channel %q is not declared (%s)", *channel, strings.Join(p.Spec.ChannelNames(), ", "))
	}
	pub, err := o.publisher(p, a)
	return p, pub, err
}

func (o *options) printRelease(r *publish.Ref) error {
	if o.json {
		return o.emitJSON(map[string]any{"digest": r.Digest.String(), "manifest": r.Manifest})
	}
	m := r.Manifest
	fmt.Fprintf(o.out, "%s %s revision %d · channel %s · tag %s · seq %d\n%s\n", m.Name, m.ProjectVersion, m.Revision, m.Channel, m.Tag, m.ReleaseSeq, r.Digest)
	return nil
}

// ship publishes the tree's release on channel (the default one when empty). want, when set, is
// the version the caller expects the tree to be at: another one is refused.
func (o *options) ship(ctx context.Context, channel string, revision int, want string, disks bool) error {
	if err := o.lintGate(); err != nil {
		return err
	}
	p, v, err := o.releaseVersion()
	if err != nil {
		return err
	}
	if want != "" {
		if c, err := p.Scheme.Compare(v, want); err != nil {
			return err
		} else if c != 0 {
			return fmt.Errorf("the tree is at %s now, not %s: not shipped", v, want)
		}
	}
	if channel == "" {
		channel = defaultChannel(p.Spec.Channels, p.Spec.ChannelNames())
	}
	pub, err := o.publisher(p, writeAccess)
	if err != nil {
		return err
	}
	// lint warns of a version out of its month; a ship of it is refused unless the channel holds
	// it already (a ship resumed, or a later revision of a version out).
	if gerr := p.Scheme.Guard(v, o.now()); gerr != nil {
		out, err := o.released(ctx, pub, p, channel, v)
		if err != nil {
			return fmt.Errorf("version.scheme month_guard: %w, and whether %s holds it is not known: %v; not shipped", gerr, channel, err)
		}
		if !out {
			return fmt.Errorf("version.scheme month_guard: %w; not shipped", gerr)
		}
	}
	r, err := pub.Ship(ctx, publish.ShipOptions{Version: v, Channel: channel, Revision: revision, Disks: disks})
	return o.published(r, err)
}

// published prints a release a ship or a promote published. A cleanup that failed after it is
// said apart, with exit status 3: the release is out, and is not shipped again.
func (o *options) published(r *publish.Ref, err error) error {
	var ce *publish.CleanupError
	if errors.As(err, &ce) && r != nil {
		if perr := o.printRelease(r); perr != nil {
			return perr
		}
		fmt.Fprintf(o.err, "stowage: %v\n", ce)
		return ExitError{Code: 3}
	}
	if err != nil {
		return err
	}
	return o.printRelease(r)
}

func shipCmd(o *options) *cobra.Command {
	var channel string
	var revision int
	var disks bool
	c := &cobra.Command{
		Use:   "ship",
		Short: "Build what changed, push the release and move the channel pointer",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.ship(cmd.Context(), channel, revision, "", disks || o.envDisks())
		},
	}
	c.Flags().BoolVar(&disks, "disks", false, "also publish the kairos shape's disks (and register cloud_publish's images); the channel's older disk sets are removed")
	c.Flags().StringVar(&channel, "channel", "", "channel (default: the one that moves latest, else the first)")
	c.Flags().IntVar(&revision, "revision", 1, "package revision")
	c.Flags().BoolVar(&o.json, "json", false, "machine-readable output")
	return c
}

func verifyCmd(o *options) *cobra.Command {
	var channel string
	var revision int
	c := &cobra.Command{
		Use:   "verify [<version>]",
		Short: "Check a release (or the channel pointer's) is complete and digest-exact on the registry",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, pub, err := o.openPublisher(&channel, readAccess)
			if err != nil {
				return err
			}
			v := ""
			if len(args) == 1 {
				v = args[0]
			}
			r, err := pub.Verify(cmd.Context(), channel, v, revision)
			if err != nil {
				return err
			}
			if err := o.printRelease(r); err != nil {
				return err
			}
			if !o.json {
				fmt.Fprintln(o.out, "ok")
			}
			return nil
		},
	}
	c.Flags().StringVar(&channel, "channel", "", "channel (default: the one that moves latest, else the first)")
	c.Flags().IntVar(&revision, "revision", 1, "package revision (with <version>)")
	c.Flags().BoolVar(&o.json, "json", false, "machine-readable output")
	return c
}

func promoteCmd(o *options) *cobra.Command {
	var from, to string
	var revision int
	c := &cobra.Command{
		Use:   "promote <version> --from <channel> --to <channel>",
		Short: "Publish a release on another channel by re-tagging it; nothing is rebuilt",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.lintGate(); err != nil {
				return err
			}
			p, v, err := o.releaseVersion()
			if err != nil {
				return err
			}
			if err := p.Scheme.Validate(args[0]); err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			if c, err := p.Scheme.Compare(v, args[0]); err != nil {
				return err
			} else if c != 0 {
				return fmt.Errorf("the tree is at %s, not %s: check out the release's commit to promote it", v, args[0])
			}
			for _, ch := range []string{from, to} {
				if _, ok := p.Spec.Channels[ch]; !ok {
					return fmt.Errorf("channel %q is not declared (%s)", ch, strings.Join(p.Spec.ChannelNames(), ", "))
				}
			}
			pub, err := o.publisher(p, writeAccess)
			if err != nil {
				return err
			}
			r, err := pub.Promote(cmd.Context(), v, from, to, revision)
			return o.published(r, err)
		},
	}
	c.Flags().StringVar(&from, "from", "", "source channel (required)")
	c.Flags().StringVar(&to, "to", "", "target channel (required)")
	c.Flags().IntVar(&revision, "revision", 1, "package revision")
	c.Flags().BoolVar(&o.json, "json", false, "machine-readable output")
	_ = c.MarkFlagRequired("from")
	_ = c.MarkFlagRequired("to")
	return c
}

func yankCmd(o *options) *cobra.Command {
	var channel, reason string
	var revision int
	c := &cobra.Command{
		Use:   "yank <version> --reason <text>",
		Short: "Mark a release so consumers refuse to upgrade to it; nothing is deleted",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(reason) == "" {
				return errors.New("--reason is required")
			}
			_, pub, err := o.openPublisher(&channel, writeAccess)
			if err != nil {
				return err
			}
			return pub.Yank(cmd.Context(), channel, args[0], revision, reason)
		},
	}
	c.Flags().StringVar(&channel, "channel", "", "channel (default: the one that moves latest, else the first)")
	c.Flags().StringVar(&reason, "reason", "", "why (shown to operators)")
	c.Flags().IntVar(&revision, "revision", 1, "package revision")
	return c
}

func pruneCmd(o *options) *cobra.Command {
	var channel string
	c := &cobra.Command{
		Use:   "prune --channel <mutable channel> [<version>]",
		Short: "Delete a mutable channel's releases: one version, or all but the pointer's",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if channel == "" {
				return errors.New("--channel is required")
			}
			_, pub, err := o.openPublisher(&channel, writeAccess)
			if err != nil {
				return err
			}
			v := ""
			if len(args) == 1 {
				v = args[0]
			}
			pruned, err := pub.Prune(cmd.Context(), channel, v)
			for _, t := range pruned {
				fmt.Fprintln(o.out, t)
			}
			return err
		},
	}
	c.Flags().StringVar(&channel, "channel", "", "mutable channel (required)")
	return c
}

func releasesCmd(o *options) *cobra.Command {
	var channel string
	c := &cobra.Command{
		Use:   "releases",
		Short: "List the published releases from their release manifests",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := o.open()
			if err != nil {
				return err
			}
			if channel != "" {
				if _, ok := p.Spec.Channels[channel]; !ok {
					return fmt.Errorf("channel %q is not declared (%s)", channel, strings.Join(p.Spec.ChannelNames(), ", "))
				}
			}
			pub, err := o.publisher(p, listAccess)
			if err != nil {
				return err
			}
			rs, err := pub.Releases(cmd.Context(), channel)
			if err != nil {
				return err
			}
			if o.json {
				out := []map[string]any{}
				for _, r := range rs {
					out = append(out, map[string]any{"digest": r.Digest.String(), "manifest": r.Manifest})
				}
				return o.emitJSON(out)
			}
			tw := tabwriter.NewWriter(o.out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "CHANNEL\tVERSION\tREV\tSEQ\tTAG\tDATE\tYANKED")
			for _, r := range rs {
				m := r.Manifest
				y := ""
				if m.Yanked != nil {
					y = m.Yanked.Reason
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\t%s\n", m.Channel, m.ProjectVersion, m.Revision, m.ReleaseSeq, m.Tag, m.Date.Format("2006-01-02 15:04"), y)
			}
			return tw.Flush()
		},
	}
	c.Flags().StringVar(&channel, "channel", "", "only this channel")
	c.Flags().BoolVar(&o.json, "json", false, "machine-readable output")
	return c
}

// released reports whether channel holds a release of version v, any revision.
func (o *options) released(ctx context.Context, pub *publish.Publisher, p *project.Project, channel, v string) (bool, error) {
	// Releases' order does not matter here: a release it could not order is still listed.
	refs, err := pub.Releases(ctx, channel)
	for _, r := range refs {
		if c, cerr := p.Scheme.Compare(r.Manifest.ProjectVersion, v); cerr == nil && c == 0 {
			return true, nil
		}
	}
	return false, err
}
