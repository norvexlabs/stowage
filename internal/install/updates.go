package install

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/norvexlabs/stowage/internal/lint"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/sigverify"
)

// channelTarget is the release an upgrade on channel installs, read from the registry c reads at
// prefix: with v empty the channel's pointer, else v's latest revision not yanked (findRelease);
// either one signed by a key trust holds (nil: none is checked, the project does not sign). A
// yanked pointer is refused, and so is a pointer its tag no longer holds, or a release of another
// project or channel. Every shape's `updates` and the VM's `upgrade` read it here.
func channelTarget(ctx context.Context, c *registry.Client, prefix, project, channel, v string, trust *sigverify.Verifier, logf func(string, ...any)) (*release.Manifest, error) {
	if !lint.ChannelName(channel) {
		return nil, refuse("give --channel a channel's name", "%q is not a channel name", channel)
	}
	var m *release.Manifest
	var err error
	if v != "" {
		m, err = findRelease(ctx, c, prefix, project, channel, v, trust, logf)
	} else {
		repo := prefix + "/" + project + "-release"
		var d v1.Hash
		m, d, err = readRelease(ctx, c, repo, channel)
		if err == nil && m == nil {
			err = fmt.Errorf("the channel %s has no release in %s", channel, prefix)
		}
		if err == nil {
			var r name.Repository
			if r, err = c.Repo(repo); err == nil {
				err = checkSigned(ctx, c, r, d, m, trust)
			}
		}
		// A pointer left on a publication its tag no longer holds (yanked or rebuilt since; the
		// old one stays validly signed) is no pointer, as the publisher reads it.
		if err == nil && m.Yanked != nil {
			err = refuse("choose another release with --version: the channel's is yanked", "the channel %s's release %s is yanked: %s", channel, m.Tag, m.Yanked.Reason)
		}
		if err == nil {
			var td v1.Hash
			var tm *release.Manifest
			if tm, td, err = readRelease(ctx, c, repo, m.Tag); err == nil && (tm == nil || td != d) {
				err = fmt.Errorf("the channel %s's pointer names %s, which its tag no longer holds: the channel is being changed, try again later", channel, m.Tag)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if m.Name != project || m.Channel != channel {
		return nil, fmt.Errorf("the release found is %s's on %s, not %s's on %s", m.Name, m.Channel, project, channel)
	}
	return m, nil
}

// upgradeTarget is the release `upgrade --version` installs from channel on Docker and
// Kubernetes: version v's latest revision, v empty the version of the channel's pointer
// (channelTarget), whose latest revision may be a later one than the pointer's.
func upgradeTarget(ctx context.Context, c *registry.Client, prefix, project, channel, v string, trust *sigverify.Verifier, logf func(string, ...any)) (*release.Manifest, error) {
	m, err := channelTarget(ctx, c, prefix, project, channel, v, trust, logf)
	if err != nil || v != "" {
		return m, err
	}
	return channelTarget(ctx, c, prefix, project, channel, m.ProjectVersion, trust, logf)
}

// laterRelease reports whether m comes after version cur, revision rev, by the scheme m records;
// a `command` scheme, which no installer runs to read a release, orders by revision within one
// version only, and calls any other version later.
func laterRelease(m *release.Manifest, cur string, rev int) (bool, error) {
	c, err := compareTo(m, cur) // a `command` scheme: -1 for any other version
	if err != nil {
		return false, err
	}
	if c == 0 {
		return m.Revision > rev, nil
	}
	return c < 0, nil
}

// installUpdate is the Update of an install on Docker or Kubernetes running version cur, revision
// rev and following the channel follows, toward m found on channel (channelTarget: never
// yanked): offered when m comes after it, is on the channel the install follows, which `upgrade
// --version` reads alone, and has an installer, which it fetches. Follows names the install's
// channel when m comes after it on another.
func installUpdate(m *release.Manifest, channel, follows, cur string, rev int) (Update, error) {
	newer, err := laterRelease(m, cur, rev)
	if err != nil {
		return Update{}, err
	}
	u := Update{
		Channel: channel, Current: Version{cur, rev}, Target: Version{m.ProjectVersion, m.Revision},
		Available: newer && channel == follows && m.Artifacts.Installer != "", Changelog: m.Changelog.Project,
	}
	if channel != follows && newer {
		u.Follows = follows
	}
	return u, nil
}

// Updates is the release `upgrade --version` would move the install in O.Dir to: the channel's
// latest (channel empty: the install's release's), or version v's latest revision, read from the
// registry the install pulls from (a mirror, with its recorded CA and the Docker client's login
// there) and signed by a key the install trusts, as `upgrade --version` reads it.
func (in *Installer) Updates(ctx context.Context, channel, v string) (Update, error) {
	env, err := in.open()
	if err != nil {
		return Update{}, err
	}
	project, cur, rev, err := in.sameProject(env, in.R.Manifest)
	if err != nil {
		return Update{}, err
	}
	r, err := strconv.Atoi(rev)
	if err != nil {
		return Update{}, fmt.Errorf("the install's revision %q is not a number", rev)
	}
	if !pinnedImages(env) {
		return Update{}, refusal("run the new release's installer, upgrade --dir "+in.O.Dir, errors.New("updates: this install was made with --offline and never pulls: the new release's installer, its images loaded, upgrades it (`upgrade --dir "+in.O.Dir+"`)"))
	}
	trust, kept, err := in.installTrust(ctx, project, cur, rev, "")
	if r, ok := Refusal(err); ok && errors.As(err, new(keysUnknown)) {
		// The kept installer knows its keys: its own `updates` is what to run, never its
		// `upgrade --version`, which would upgrade; one from before `updates` checks a version
		// with a dry run.
		return Update{}, refusal(fmt.Sprintf("run %s updates with the same arguments (%s); an installer without `updates` checks a version with `%s upgrade --version <version> --dry-run --dir %s`", in.ctl(), keysWhy, in.ctl(), in.O.Dir), r)
	}
	if err != nil {
		return Update{}, err
	}
	follows := kept.Manifest.Channel
	channel = cmp.Or(channel, follows)
	prefix, _ := env.Get("STOWAGE_REGISTRY")
	c, err := in.registryClient(prefix)
	if err != nil {
		return Update{}, err
	}
	m, err := upgradeTarget(ctx, c, prefix, project, channel, v, trust, in.O.logf)
	if err != nil {
		return Update{}, err
	}
	return installUpdate(m, channel, follows, cur, r)
}

// K8sUpdates is Updates of the helm release O.K8s names: read from the registry its values record
// (--image-registry in its place), trusting the keys `k8s upgrade --version` trusts (k8sTrust).
func (in *Installer) K8sUpdates(ctx context.Context, channel, v string) (Update, error) {
	in.givenCA = true
	if err := in.checkTarget(); err != nil {
		return Update{}, err
	}
	if _, err := in.checkHelm(ctx); err != nil {
		return Update{}, err
	}
	rel, err := in.release(ctx)
	if err != nil {
		return Update{}, err
	}
	if rel == nil {
		return Update{}, in.noRelease()
	}
	if err := in.pending(rel); err != nil {
		return Update{}, err
	}
	cur, err := in.installedRelease(ctx)
	if err != nil {
		return Update{}, err
	}
	trust, err := in.k8sTrust(cur)
	if err != nil {
		return Update{}, err
	}
	prefix := cmp.Or(in.O.K8s.Registry, cur.Registry)
	c, err := in.registryClient(prefix)
	if err != nil {
		return Update{}, err
	}
	r := cur.Release
	channel = cmp.Or(channel, r.Channel)
	m, err := upgradeTarget(ctx, c, prefix, r.Name, channel, v, trust, in.O.logf)
	if err != nil {
		return Update{}, err
	}
	return installUpdate(m, channel, r.Channel, r.Version, r.Revision)
}

// k8sTrust is the keys a helm release's upgrade trusts: the installer run's, narrowed to those
// the release's values list when they list any — an installer of an earlier release does not
// bring back a key a later one dropped, and values (which an operator's --values can set) never
// add a key. An installer with none of them is refused for a release whose values list some.
func (in *Installer) k8sTrust(cur *deployed) (*sigverify.Verifier, error) {
	keys := in.R.Keys
	if listed := listedKeys(cur.PublicKeys); len(listed) > 0 {
		keys = map[string][]byte{}
		for n, k := range in.R.Keys {
			if listed[strings.TrimSpace(string(k))] {
				keys[n] = k
			}
		}
		if len(keys) == 0 {
			k := in.O.K8s
			return nil, refuse("run the installer of the release it runs, or a later one", "release %s in %s trusts signing keys this %s does not carry: run the installer of the release it runs, or a later one", k.Release, k.Namespace, in.R.Ctl())
		}
	}
	return trusted("this installer", keys)
}

// updatesCommand is `updates` on Docker (dirFlag sets its --dir) and on Kubernetes (flags adds
// its registry's): find answers the Update and what runs, which the VM's rendering names, with
// the shape's upgrade verb.
func updatesCommand(upgrade string, dirFlag func(*cobra.Command), flags func(*pflag.FlagSet), find func(cmd *cobra.Command, channel, version string) (Update, string, error), stdout io.Writer) *cobra.Command {
	var channel, version string
	var asJSON bool
	c := &cobra.Command{
		Use:   "updates",
		Short: "The release the channel offers after the one the install runs (its latest, or --version's), which `" + upgrade + " --version` moves to",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			u, runs, err := find(cmd, channel, version)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(stdout, u)
			}
			printUpdateOf(stdout, u, runs, upgrade)
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&channel, "channel", "", "the channel to look on (default: the install's, the one `"+upgrade+" --version` reads)")
	f.StringVar(&version, "version", "", "this version's latest revision, not the channel's latest release")
	f.BoolVar(&asJSON, "json", false, "print the answer as one JSON object, for a program")
	if dirFlag != nil {
		dirFlag(c)
	}
	if flags != nil {
		flags(f)
	}
	return c
}
