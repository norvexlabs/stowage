// Package cli wires the stowage commands. It holds no logic of its own beyond argument handling
// and output formatting, so every command is testable in-process through New.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/norvexlabs/stowage/internal/changelog"
	"github.com/norvexlabs/stowage/internal/graph"
	"github.com/norvexlabs/stowage/internal/hook"
	"github.com/norvexlabs/stowage/internal/installerbin"
	"github.com/norvexlabs/stowage/internal/lint"
	"github.com/norvexlabs/stowage/internal/lock"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/publish"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/version"
)

// ExitError carries a process exit code for a failure already reported on stderr.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("exit %d", e.Code) }

type options struct {
	buildVersion string
	getenv       func(string) string
	// registryOpts, builder and disks replace the registry transport, the image builder and the
	// disk builder in tests.
	registryOpts []registry.Option
	builder      publish.Builder
	disks        publish.DiskMaker
	cloud        publish.CloudUploader
	// installer is the installer executable releases are published with.
	installer []byte
	// executable is this stowage's path, and launch starts it detached; the post-commit hook
	// uses them, and tests replace them.
	executable func() (string, error)
	launch     func(exe string, args []string, logPath string) error
	// keyRoot, when set, is the directory a relative signing key is read from instead of the
	// project root: a hook ship publishes from a checkout, which has no untracked key file.
	keyRoot  string
	root     string
	specPath string
	json     bool
	now      func() time.Time
	out, err io.Writer
}

func (o *options) specFile() string {
	if o.specPath != "" {
		return o.specPath
	}
	return filepath.Join(o.root, project.SpecFile)
}

func (o *options) open() (*project.Project, error) {
	if err := o.requires(); err != nil {
		return nil, err
	}
	return project.Open(o.root, o.specFile())
}

// requires refuses a project whose stowage.requires does not accept this stowage, before its
// spec is decoded. A build that is not a release (its version is not SemVer) is not checked, nor
// is a range that does not parse: lint reports it, and so ship and promote refuse it.
func (o *options) requires() error {
	raw, err := os.ReadFile(o.specFile())
	if err != nil {
		return nil // reading the spec reports it
	}
	req := spec.Requires(raw)
	if req == "" || (version.SemVer{}).Validate(o.buildVersion) != nil {
		return nil
	}
	file := filepath.Base(o.specFile())
	r, err := version.ParseSemVerRange(req)
	if err != nil {
		return nil
	}
	ok, err := r.Contains(o.buildVersion)
	if err != nil {
		return fmt.Errorf("%s: stowage.requires: stowage %s: %w", file, o.buildVersion, err)
	}
	if !ok {
		return fmt.Errorf("%s: stowage.requires accepts Stowage %s, and this is stowage %s: run a Stowage it accepts", file, r, o.buildVersion)
	}
	return nil
}

// releaseVersion opens the project and returns its version, refusing one whose tags would
// collide on the registry (the same check lint and ship run).
func (o *options) releaseVersion() (*project.Project, string, error) {
	p, err := o.open()
	if err != nil {
		return nil, "", err
	}
	v, err := p.Version()
	if err != nil {
		return nil, "", err
	}
	conflicts, err := p.CheckTags(v)
	if err != nil {
		return nil, "", fmt.Errorf("cannot check the channels' tags: %w", err)
	}
	if len(conflicts) > 0 {
		return nil, "", fmt.Errorf("version %s cannot be released:\n  %s", v, strings.Join(conflicts, "\n  "))
	}
	return p, v, nil
}

func (o *options) emitJSON(v any) error {
	enc := json.NewEncoder(o.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// New builds the root command. stdout carries results, stderr diagnostics.
func New(buildVersion string, stdout, stderr io.Writer) *cobra.Command {
	return newRoot(&options{buildVersion: buildVersion, getenv: os.Getenv, now: time.Now, out: stdout, err: stderr, installer: installerbin.Executable,
		executable: os.Executable, launch: hook.Launch})
}

func newRoot(o *options) *cobra.Command {
	root := &cobra.Command{
		Use:           "stowage",
		Short:         "Package a container stack and publish its releases to one registry",
		Version:       o.buildVersion,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(o.out)
	root.SetErr(o.err)
	root.PersistentFlags().StringVarP(&o.root, "dir", "C", ".", "project root (spec paths are relative to it)")
	root.PersistentFlags().StringVarP(&o.specPath, "file", "f", "", "spec file (default <dir>/stowage.yaml)")
	root.AddCommand(lintCmd(o), versionCmd(o), changelogCmd(o), planCmd(o), lockCmd(o), hookCmd(o),
		shipCmd(o), verifyCmd(o), promoteCmd(o), yankCmd(o), pruneCmd(o), releasesCmd(o))
	return root
}

func lintCmd(o *options) *cobra.Command {
	c := &cobra.Command{
		Use:   "lint",
		Short: "Validate stowage.yaml and the files it names",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if err := o.requires(); err != nil {
				return err
			}
			res, err := lint.Run(o.root, o.specFile(), o.now())
			if err != nil {
				return err
			}
			if o.json {
				if err := o.emitJSON(res); err != nil {
					return err
				}
			} else {
				for _, f := range res.Findings {
					fmt.Fprintln(o.out, f)
				}
				fmt.Fprintf(o.out, "%d error(s), %d warning(s)\n", res.Errors(), len(res.Findings)-res.Errors())
			}
			if res.Errors() > 0 {
				return ExitError{Code: 1}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&o.json, "json", false, "machine-readable output")
	return c
}

func versionCmd(o *options) *cobra.Command {
	var channel string
	var revision int
	c := &cobra.Command{
		Use:   "project-version",
		Short: "Print the project version and the tag it ships under",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, v, err := o.releaseVersion()
			if err != nil {
				return err
			}
			out := map[string]any{"name": p.Spec.Name, "project_version": v, "revision": revision}
			if channel != "" {
				ch, ok := p.Spec.Channels[channel]
				if !ok {
					return fmt.Errorf("channel %q is not declared (%s)", channel, strings.Join(p.Spec.ChannelNames(), ", "))
				}
				st, err := p.Scheme.Tag(v)
				if err != nil {
					return err
				}
				tag, err := version.ChannelTag(ch.Tag, st, revision)
				if err != nil {
					return err
				}
				out["channel"], out["tag"] = channel, tag
			}
			if o.json {
				return o.emitJSON(out)
			}
			fmt.Fprintln(o.out, v)
			if t, ok := out["tag"]; ok {
				fmt.Fprintf(o.out, "tag %s (channel %s, revision %d)\n", t, channel, revision)
			}
			return nil
		},
	}
	c.Flags().StringVar(&channel, "channel", "", "also render the channel's tag")
	c.Flags().IntVar(&revision, "revision", 1, "package revision")
	c.Flags().BoolVar(&o.json, "json", false, "machine-readable output")
	return c
}

func changelogCmd(o *options) *cobra.Command {
	var from, to string
	c := &cobra.Command{
		Use:   "changelog",
		Short: "Print the changelog sections after --from up to --to (default: the current version)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := o.open()
			if err != nil {
				return err
			}
			if to == "" {
				if to, err = p.Version(); err != nil {
					return err
				}
			}
			for _, v := range []string{from, to} {
				if v == "" {
					continue
				}
				if err := p.Scheme.Validate(v); err != nil {
					return err
				}
			}
			if from == "" {
				// Without --from, the one section of --to.
				text, ok, err := p.Changelog.Section(to)
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("the changelog has no section for %s", to)
				}
				return o.printEntries([]changelog.Entry{{Version: to, Text: text}})
			}
			entries, err := changelog.Between(p.Changelog, p.Scheme, from, to)
			if err != nil {
				return err
			}
			return o.printEntries(entries)
		},
	}
	c.Flags().StringVar(&from, "from", "", "running version (exclusive)")
	c.Flags().StringVar(&to, "to", "", "target version (inclusive)")
	c.Flags().BoolVar(&o.json, "json", false, "machine-readable output")
	return c
}

func (o *options) printEntries(entries []changelog.Entry) error {
	if o.json {
		return o.emitJSON(entries)
	}
	for i, e := range entries {
		if i > 0 {
			fmt.Fprintln(o.out)
		}
		fmt.Fprintf(o.out, "## %s\n\n%s\n", e.Version, e.Text)
	}
	return nil
}

func lockCmd(o *options) *cobra.Command {
	var update bool
	c := &cobra.Command{
		Use:   "lock",
		Short: "Pin this stowage's version in stowage.lock (ship and promote refuse another stowage)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if err := (version.SemVer{}).Validate(o.buildVersion); err != nil {
				return fmt.Errorf("this stowage is not a release build (version %q): pin a release build (make build)", o.buildVersion)
			}
			specPath := o.specFile()
			if _, err := os.Stat(specPath); err != nil {
				return fmt.Errorf("%s is written next to the spec: %w", lock.FileName, err)
			}
			if err := o.requires(); err != nil {
				return err
			}
			path := filepath.Join(filepath.Dir(specPath), lock.FileName)
			l, err := lock.Load(path)
			var ioErr *fs.PathError
			switch {
			case errors.As(err, &ioErr):
				return err
			case err != nil: // the file does not decode or validate
				return fmt.Errorf("%w (fix the file by hand, or remove it and run stowage lock)", err)
			}
			msg := fmt.Sprintf("%s pins stowage %s\n", lock.FileName, o.buildVersion)
			if l == nil {
				l = &lock.Lock{}
			} else {
				order, err := l.Compare(o.buildVersion)
				if err != nil {
					return err
				}
				switch {
				case order == 0:
					fmt.Fprintf(o.out, "%s already pins stowage %s\n", lock.FileName, l.Stowage)
					return nil
				case !update:
					return fmt.Errorf("%s pins stowage %s, this is %s: stowage lock --update moves the pin (the installer and the front change with it)",
						lock.FileName, l.Stowage, o.buildVersion)
				case order < 0:
					msg = fmt.Sprintf("%s: stowage %s -> %s, an older stowage\n", lock.FileName, l.Stowage, o.buildVersion)
				default:
					msg = fmt.Sprintf("%s: stowage %s -> %s\n", lock.FileName, l.Stowage, o.buildVersion)
				}
			}
			l.Stowage = o.buildVersion
			if err := lock.Write(path, l); err != nil {
				return err
			}
			fmt.Fprint(o.out, msg)
			return nil
		},
	}
	c.Flags().BoolVar(&update, "update", false, "move an existing pin to this stowage")
	return c
}

func planCmd(o *options) *cobra.Command {
	var revision int
	var channel string
	c := &cobra.Command{
		Use:   "plan",
		Short: "Show the release's build graph and each node's inputs digest",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, v, err := o.releaseVersion()
			if err != nil {
				return err
			}
			if channel == "" {
				channel = defaultChannel(p.Spec.Channels, p.Spec.ChannelNames())
			}
			pull, err := o.pullAccount(p)
			if err != nil {
				return err
			}
			// What ship would refuse, plan reports: the installer's inputs would not be ship's.
			if len(o.installer) == 0 {
				fmt.Fprintln(o.err, "warning: this stowage carries no installer (a build without it): the installer's inputs are not a release build's")
			}
			pub := &publish.Publisher{Project: p, Installer: o.installer, Pull: pull, StowageVersion: o.buildVersion}
			if err := pub.Unpinned(); err != nil {
				fmt.Fprintf(o.err, "warning: %v\n", err)
			}
			if env, err := pub.PullUnset(); err != nil {
				return err
			} else if env != "" {
				fmt.Fprintf(o.err, "warning: %s is not set: the installer's inputs leave out the pull account ship would bake in\n", env)
			}
			g, err := pub.Graph(v, revision, channel)
			if err != nil {
				return err
			}
			if o.json {
				return o.emitJSON(g)
			}
			fmt.Fprintf(o.out, "%s %s (revision %d, channel %s)\n\n", p.Spec.Name, v, revision, channel)
			tw := tabwriter.NewWriter(o.out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NODE\tINPUTS\tDEPENDS ON\tNOTE")
			for _, n := range g.Nodes {
				note := ""
				switch {
				case n.Pinned != "":
					note = "pinned " + n.Pinned
				case n.ReadsVersion:
					note = "reads " + graph.VersionArg
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n.ID, registry.Short(n.Inputs), strings.Join(n.Deps, ","), note)
			}
			return tw.Flush()
		},
	}
	c.Flags().IntVar(&revision, "revision", 1, "package revision")
	c.Flags().StringVar(&channel, "channel", "", "channel (default: the one that moves latest, else the first)")
	c.Flags().BoolVar(&o.json, "json", false, "machine-readable output")
	return c
}

// defaultChannel is the channel that moves latest, else the first in name order.
func defaultChannel(channels map[string]spec.Channel, names []string) string {
	for _, n := range names {
		if channels[n].Latest {
			return n
		}
	}
	if len(names) == 0 {
		return ""
	}
	return names[0]
}
