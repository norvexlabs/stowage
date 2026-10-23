package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/norvexlabs/stowage/internal/gitcmd"
	"github.com/norvexlabs/stowage/internal/hook"
)

func hookCmd(o *options) *cobra.Command {
	c := &cobra.Command{
		Use:   "hook",
		Short: "Ship a release from git's post-commit hook when a commit moves the version",
		Args:  cobra.NoArgs,
	}
	// A hook ship is the project's release: its background run takes no test flag.
	c.PersistentPreRunE = func(*cobra.Command, []string) error {
		if o.test.on() {
			return errors.New("hook: a commit ships the project's release, never a test one: the --test-… flags and --shapes are for ship")
		}
		return nil
	}
	c.AddCommand(postCommitCmd(o), hookInstallCmd(o))
	return c
}

func postCommitCmd(o *options) *cobra.Command {
	var channel, ship, commit string
	var disks bool
	c := &cobra.Command{
		Use:   "post-commit",
		Short: "When HEAD moved the version, start its ship in the background (always exits 0)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if ship != "" || commit != "" {
				if ship == "" || commit == "" {
					return errors.New("--ship and --commit go together")
				}
				return o.hookShip(cmd.Context(), channel, ship, commit, disks)
			}
			if err := o.postCommit(channel, disks || o.envDisks()); err != nil {
				fmt.Fprintf(o.err, "stowage hook: %v\n", err)
			}
			return nil
		},
	}
	c.Flags().StringVar(&channel, "channel", "", "channel (default: the one that moves latest, else the first)")
	c.Flags().StringVar(&ship, "ship", "", "ship this version now, under the hook's lock (what the background run does)")
	c.Flags().StringVar(&commit, "commit", "", "with --ship: the commit the project must be exactly at")
	c.Flags().BoolVar(&disks, "disks", false, "the ship publishes the disks (as ship --disks)")
	return c
}

// postCommit starts the release's ship in a session of its own when HEAD moved the version; it
// returns at once, so the commit never waits for its release.
func (o *options) postCommit(channel string, disks bool) error {
	p, err := o.open()
	if err != nil {
		return err
	}
	if channel == "" {
		channel = defaultChannel(p.Spec.Channels, p.Spec.ChannelNames())
	}
	if _, ok := p.Spec.Channels[channel]; !ok {
		return fmt.Errorf("channel %q is not declared (%s)", channel, strings.Join(p.Spec.ChannelNames(), ", "))
	}
	r, err := hook.Open(p.Root)
	if err != nil {
		return err
	}
	if _, err := o.specInRepo(r, "HEAD"); err != nil {
		return err
	}
	d, err := hook.Detect(p, r)
	if err != nil {
		return err
	}
	if d.Version == "" {
		fmt.Fprintf(o.out, "stowage hook: %s\n", d.Reason)
		return nil
	}
	// Said at the commit, rather than by the background run's log.
	if err := p.Scheme.Guard(d.Version, o.now()); err != nil {
		return fmt.Errorf("version.scheme month_guard: %w; not shipped", err)
	}
	tag, err := p.Scheme.Tag(d.Version)
	if err != nil {
		return err
	}
	exe, err := o.executable()
	if err != nil {
		return err
	}
	args := []string{"-C", p.Root}
	if o.specPath != "" {
		spec, err := filepath.Abs(o.specPath)
		if err != nil {
			return err
		}
		args = append(args, "-f", spec)
	}
	args = append(args, "hook", "post-commit", "--channel", channel, "--ship", d.Version, "--commit", d.Commit)
	if disks {
		args = append(args, "--disks")
	}
	logPath := r.LogPath(p.Spec.Name, tag)
	if err := o.launch(exe, args, logPath); err != nil {
		return fmt.Errorf("start the ship of %s: %w", d.Version, err)
	}
	with := ""
	if disks {
		with = ", with its disks,"
	}
	fmt.Fprintf(o.out, "stowage hook: release %s%s: ship to channel %s started in the background\n  follow: tail -f %s\n", d.Version, with, channel, logPath)
	return nil
}

// hookShip is the background run: under the repository's hook lock (waiting for another hook
// ship to end), it ships version v from a checkout of commit, so the release is that commit
// whatever the working tree holds meanwhile.
func (o *options) hookShip(ctx context.Context, channel, v, commit string, disks bool) (err error) {
	fmt.Fprintf(o.out, "%s stowage hook: ship %s (commit %s) to channel %s\n", o.now().UTC().Format(time.RFC3339), v, commit, channel)
	root, err := filepath.Abs(o.root)
	if err != nil {
		return err
	}
	r, err := hook.Open(root)
	if err != nil {
		return err
	}
	rel, err := relTo(r.Top, root)
	if err != nil {
		return err
	}
	id, err := r.Commit(commit)
	if err != nil {
		return err
	}
	spec, err := o.specInRepo(r, id)
	if err != nil {
		return err
	}
	// A checkout has no submodule content unless it is fetched again, which would also activate
	// the submodules in the repository's own configuration.
	if subs, err := r.Submodules(id, rel); err != nil {
		return err
	} else if len(subs) > 0 {
		return fmt.Errorf("%s is not shipped: the project holds submodules (%s), which a hook ship does not check out; publish with stowage ship", v, strings.Join(subs, ", "))
	}
	unlock, err := hook.Lock(r.LockPath(), func() {
		fmt.Fprintln(o.out, "stowage hook: waiting for the hook ship running in this repository")
	})
	if err != nil {
		return err
	}
	defer unlock()
	top, remove, err := r.Checkout(id)
	if err != nil {
		return fmt.Errorf("check out %s: %w", id, err)
	}
	defer func() {
		if rerr := remove(); rerr != nil && err == nil {
			err = fmt.Errorf("remove the checkout of %s: %w", id, rerr)
		}
	}()
	return o.inCheckout(top, rel, spec, root).ship(ctx, channel, 1, v, disks)
}

// inCheckout is o run on the project at rel (and the spec at spec, when set) in the checkout at
// top, reading a relative signing key from root, the project's own directory.
func (o *options) inCheckout(top, rel, spec, root string) *options {
	at := *o
	at.root = filepath.Join(top, rel)
	if spec != "" {
		at.specPath = filepath.Join(top, spec)
	}
	at.keyRoot = root
	return &at
}

func hookInstallCmd(o *options) *cobra.Command {
	var channel string
	var disks bool
	c := &cobra.Command{
		Use:   "install",
		Short: "Write git's post-commit hook so it runs stowage hook post-commit for this project",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := o.open()
			if err != nil {
				return err
			}
			if channel != "" {
				if _, ok := p.Spec.Channels[channel]; !ok {
					return fmt.Errorf("channel %q is not declared (%s)", channel, strings.Join(p.Spec.ChannelNames(), ", "))
				}
			}
			r, err := hook.Open(p.Root)
			if err != nil {
				return err
			}
			exe, err := o.executable()
			if err != nil {
				return err
			}
			// Git runs a hook from the working tree's top level: the project is named from there,
			// so the hook keeps working wherever the clone moves. Git resolves symbolic links in
			// the top level, so the project's path is resolved too.
			rel, err := relTo(r.Top, p.Root)
			if err != nil {
				return err
			}
			spec, err := o.specInRepo(r, "HEAD")
			if err != nil {
				return err
			}
			argv := []string{exe, "-C", rel}
			if spec != "" {
				argv = append(argv, "-f", spec)
			}
			argv = append(argv, "hook", "post-commit")
			if channel != "" {
				argv = append(argv, "--channel", channel)
			}
			if disks {
				argv = append(argv, "--disks")
			}
			replaced, err := hook.Install(r.HookFile, argv)
			if err != nil {
				return err
			}
			verb := "wrote"
			if replaced {
				verb = "updated"
			}
			fmt.Fprintf(o.out, "%s %s\n", verb, r.HookFile)
			return nil
		},
	}
	c.Flags().StringVar(&channel, "channel", "", "channel the hook ships to (default: the one that moves latest, else the first)")
	c.Flags().BoolVar(&disks, "disks", false, "every release the hook ships publishes its disks (without it, a commit made with STOWAGE_SHIP_DISKS=1 does)")
	return c
}

// relTo is path relative to top, symbolic links resolved on both sides.
func relTo(top, path string) (string, error) {
	t, err := filepath.EvalSymlinks(top)
	if err != nil {
		return "", err
	}
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Rel(t, p)
}

// specInRepo is the -f spec's path relative to the repository's top level, "" without -f. A spec
// outside the repository, or one commit rev does not record, is refused: a hook ship runs in a
// checkout of the commit, which does not have it.
func (o *options) specInRepo(r hook.Repo, rev string) (string, error) {
	if o.specPath == "" {
		return "", nil
	}
	abs, err := filepath.Abs(o.specPath)
	if err != nil {
		return "", err
	}
	rel, err := relTo(r.Top, abs)
	if err != nil {
		return "", err
	}
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("the spec %s is outside the repository: a hook ship runs in a checkout of the commit", abs)
	}
	if ok, err := gitcmd.Verify(r.Top, rev+":"+filepath.ToSlash(rel)); err != nil {
		return "", err
	} else if !ok {
		return "", fmt.Errorf("the spec %s is not committed (at %s): a hook ship runs in a checkout of the commit", abs, rev)
	}
	return rel, nil
}
