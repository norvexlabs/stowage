package install

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/spec"
)

// Command is the installer's command line: with no subcommand it installs (or finishes an
// install), as the bootstrap one-liner runs it. self is the running installer, which an install
// keeps.
func Command(r *Release, self string, stdout, stderr io.Writer) *cobra.Command {
	return newCommand(r, self, stdout, stderr, K8sOptions{Helm: ExecTool{Bin: "helm"}, Kubectl: ExecTool{Bin: "kubectl"}}, Terminal)
}

// newCommand is Command running helm and kubectl as tools gives them, asking with terminal.
func newCommand(r *Release, self string, stdout, stderr io.Writer, tools K8sOptions, terminal func(accessible bool) Prompter) *cobra.Command {
	if r.Manifest.Bundled() {
		return vmCommand(r, self, stdout, stderr, terminal, 0)
	}
	o := Options{Out: stdout, Log: stderr, Docker: ExecTool{Bin: "docker"}, Tofu: ExecTool{Bin: "tofu", Grace: 5 * time.Minute}, SSH: ExecTool{Bin: "ssh"}, Self: self, K8s: tools}
	if _, err := os.Stat("/proc/self/exe"); err == nil {
		o.SelfExe = "/proc/self/exe" // the running file, even when its path was replaced since
	}
	var ports, sets, domains []string
	var answers, to, secretsFile string
	var noInput, accessible bool
	name := r.Spec.Name
	in := &Installer{R: r}
	prepare := func() error {
		var err error
		if o.Ports, err = parsePorts(ports); err != nil {
			return err
		}
		if o.Set, err = parseSets(sets); err != nil {
			return err
		}
		if o.Domains, err = parseDomains(domains); err != nil {
			return err
		}
		if answers != "" {
			if o.Answers, err = readAnswers(answers); err != nil {
				return err
			}
		}
		in.O = o
		return nil
	}
	root := &cobra.Command{
		Use:           r.Ctl(),
		Short:         fmt.Sprintf("Install and manage %s %s with Docker", name, r.Manifest.ProjectVersion),
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := prepare(); err != nil {
				return err
			}
			if !noInput {
				in.O.Prompt = terminal(accessible)
			}
			in.O.Cert, in.O.Key, in.O.CA = expandHome(in.O.Cert), expandHome(in.O.Key), expandHome(in.O.CA)
			if secretsFile != "" {
				var err error
				if in.O.Secrets, err = readSecretsArg(secretsFile, cmd.InOrStdin()); err != nil {
					return err
				}
			}
			if in.O.Prompt != nil && !cmd.Flags().Changed("dir") {
				def := in.O.Dir
				a, err := in.O.Prompt.Ask(cmd.Context(), Form{
					Questions: []Question{{Title: "Install into", Key: "--dir", Required: true, Check: checkDir}},
					Defaults:  func(map[string]string) map[string]string { return map[string]string{"--dir": def} },
				})
				if err != nil {
					return err
				}
				in.O.Dir = expandHome(cmp.Or(a["--dir"], def))
			}
			if err := in.Install(cmd.Context()); err != nil {
				return err
			}
			in.Finished(cmd.Context(), onDocker)
			return nil
		},
	}
	dirFlag := func(c *cobra.Command) { c.Flags().StringVar(&o.Dir, "dir", "./"+name, "install directory") }
	dirFlag(root)
	rf := root.Flags()
	rf.StringSliceVar(&ports, "ports", nil, "a route's port, route=port (loopback mode; a re-run changes it)")
	rf.StringVar(&o.Address, "address", "", "loopback address the ports bind to, 127.x.x.x or ::1, default 127.0.0.1 (first install only)")
	rf.StringVar(&o.TLS, "tls", "", tlsHelp(in))
	rf.StringSliceVar(&domains, "domains", nil, "a route's domain, route=host: HTTPS (a re-run changes it)")
	rf.StringVar(&o.Cert, "cert", "", "--tls byo: the certificate chain, PEM, copied into the install (a re-run replaces it)")
	rf.StringVar(&o.Key, "key", "", "--tls byo: the certificate's private key, PEM, copied into the install")
	if r.Spec.Front.PublishCA != nil {
		rf.StringVar(&o.CA, "ca", "", "--tls byo: the CA the certificate chains to, PEM, published to the stack")
	}
	rf.IntVar(&o.HTTPPort, "http-port", 0, "the host port a TLS front serves HTTP on (default the project's front.http_port, else 80; a re-run changes it)")
	rf.IntVar(&o.HTTPSPort, "https-port", 0, "the host port a TLS front serves HTTPS on (default the project's front.https_port, else 443; a re-run changes it)")
	rf.StringVar(&o.Registry, "registry", "", "pull the images from this registry prefix, a mirror (first install only)")
	rf.StringVar(&o.RegistryCA, "registry-ca", "", "a PEM file of the CA the registry's certificate chains to, a mirror's own: kept in the install, given to the Docker daemon (first install only)")
	ipv6Flag(rf, &o.IPv6, "IPv6 on the stack's network, so the front's IPv6 clients keep their address (Docker 27 or later); --ipv6=false turns it off (default off; on a re-run, the recorded choice)")
	rf.BoolVar(&o.Offline, "offline", false, "the images are already loaded: never log in or pull")
	rf.BoolVar(&o.DryRun, "dry-run", false, "check, print what would be written (secrets redacted), write nothing")
	rf.StringVar(&secretsFile, "secrets", "", "a first install's secrets: the file `secrets export` wrote, decrypted (- for standard input)")
	rf.StringArrayVar(&sets, "set", nil, "an input, KEY=VALUE (repeatable; a re-run changes the recorded one)")
	rf.StringVar(&answers, "answers", "", "a YAML file of inputs, KEY: value (a re-run changes the recorded ones)")
	rf.BoolVar(&noInput, "no-input", false, "never ask: an input nothing gave takes its default")
	rf.BoolVar(&o.NoProvision, "no-provision", false, "start the stack without running the provision hook (`provision` runs it later)")
	rf.BoolVar(&accessible, "accessible", false, "ask with plain line prompts (screen readers); also when ACCESSIBLE is set or TERM=dumb")
	status := &cobra.Command{
		Use:   "status",
		Short: "What the install runs, its URLs and its services",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := prepare(); err != nil {
				return err
			}
			return in.Status(cmd.Context())
		},
	}
	dirFlag(status)
	root.AddCommand(status)
	prov := &cobra.Command{
		Use:   "provision",
		Short: "Run the provision hook again on the running stack (idempotent; continues an unfinished one)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := prepare(); err != nil {
				return err
			}
			return in.Provision(cmd.Context())
		},
	}
	prov.Flags().StringArrayVar(&sets, "set", nil, "correct an input only the hook reads, or set a value it returned or declares it returns (empty forgets it), KEY=VALUE (repeatable)")
	dirFlag(prov)
	root.AddCommand(prov)
	up := &cobra.Command{
		Use:   "upgrade",
		Short: fmt.Sprintf("Move an install of an earlier %s release to %s (dumps the database first; `rollback` goes back)", name, r.Manifest.ProjectVersion),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := prepare(); err != nil {
				return err
			}
			if to != "" {
				if in.O.Offline {
					return errors.New("--version fetches the release from the registry: --offline upgrades with the new release's own installer")
				}
				return in.UpgradeTo(cmd.Context(), to, passed(cmd, "version"))
			}
			if !noInput {
				in.O.Prompt = terminal(accessible)
			}
			if err := in.Upgrade(cmd.Context()); err != nil {
				return err
			}
			if in.upgraded {
				in.Finished(cmd.Context(), onDocker)
			}
			return nil
		},
	}
	uf := up.Flags()
	uf.StringVar(&to, "version", "", "fetch the installer of this version (its latest revision, on the install's channel) from the install's registry, and upgrade with it")
	uf.StringArrayVar(&sets, "set", nil, "an input this release adds, KEY=VALUE (repeatable)")
	uf.StringVar(&answers, "answers", "", "a YAML file of the inputs this release adds, KEY: value")
	uf.BoolVar(&noInput, "no-input", false, "never ask: an added input nothing gave takes its default")
	uf.BoolVar(&accessible, "accessible", false, "ask with plain line prompts")
	uf.BoolVar(&o.Offline, "offline", false, "the images are already loaded: never log in or pull")
	uf.BoolVar(&o.DryRun, "dry-run", false, "check, print the values that would change (secrets redacted), change nothing")
	uf.BoolVar(&o.NoProvision, "no-provision", false, "start the release without running the provision hook")
	dirFlag(up)
	root.AddCommand(up)
	back := &cobra.Command{
		Use:   "rollback",
		Short: "Go back to the release the last upgrade replaced; refused when the database moved past what it reads",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := prepare(); err != nil {
				return err
			}
			return in.Rollback(cmd.Context())
		},
	}
	back.Flags().BoolVar(&o.Offline, "offline", false, "the images are already loaded: never log in or pull")
	back.Flags().BoolVar(&o.DryRun, "dry-run", false, "check (pulling the previous images the check reads), change nothing")
	dirFlag(back)
	root.AddCommand(back)
	reset := &cobra.Command{
		Use:   "reset-db",
		Short: "Wipe the data (the volumes data.reset names) and start again as first installed; the service stops meanwhile, nothing is dumped",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := prepare(); err != nil {
				return err
			}
			if !noInput {
				in.O.Prompt = terminal(accessible)
			}
			return in.Reset(cmd.Context())
		},
	}
	rsf := reset.Flags()
	rsf.BoolVarP(&o.Yes, "yes", "y", false, "wipe without asking")
	rsf.BoolVar(&noInput, "no-input", false, "never ask: without --yes, refuse")
	rsf.BoolVar(&accessible, "accessible", false, "confirm with a plain line prompt")
	rsf.BoolVar(&o.DryRun, "dry-run", false, "print what would be wiped, change nothing")
	rsf.BoolVar(&o.NoProvision, "no-provision", false, "start the stack without running the provision hook")
	dirFlag(reset)
	root.AddCommand(reset)
	var rotate func(cmd *cobra.Command, key, pass string) error
	if slices.ContainsFunc(r.Spec.Secrets, func(s spec.Secret) bool { return s.Rotation }) {
		rotate = func(cmd *cobra.Command, key, pass string) error {
			if err := prepare(); err != nil {
				return err
			}
			return in.RotateSecret(cmd.Context(), key, pass)
		}
	}
	root.AddCommand(secretsCommand(name, stdout, func(c *cobra.Command) { dirFlag(c) }, rotate, func(_ *cobra.Command, recipients []string) ([]byte, error) {
		if err := prepare(); err != nil {
			return nil, err
		}
		return in.ExportSecrets(recipients)
	}))
	if r.Spec.Shapes.Kubernetes != nil {
		root.AddCommand(k8sCommand(in, &o, prepare, terminal, &sets, &answers, &noInput, &accessible))
	}
	if r.Spec.Shapes.Kairos != nil {
		root.AddCommand(vmCreateCommand(in, &o, prepare, terminal, &sets, &domains, &answers, &noInput, &accessible))
	}
	root.AddCommand(mirrorCommand(in, &o, prepare))
	root.AddCommand(dockerHostCommands(in, prepare, dirFlag, stdout, stderr, terminal)...)
	if r.Spec.Shapes.Kubernetes != nil && r.Spec.Hooks.Provision != nil {
		root.AddCommand(k8sHookPodCommand(stderr))
	}
	root.AddCommand(&cobra.Command{
		Use:   payload.ScriptCommand,
		Short: "Print the bootstrap script: this installer inside a shell script, as its image prints it",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			f, err := os.Open(self)
			if err != nil {
				return err
			}
			defer f.Close()
			return payload.Script(stdout, name, r.Ctl(), f)
		},
	})
	return root
}

// k8sCommand is `k8s`: the release on a Kubernetes cluster, with helm and kubectl. Every
// subcommand works in the current directory, where `upgrade --version` fetches an installer.
func k8sCommand(in *Installer, o *Options, prepare func() error, terminal func(bool) Prompter, sets *[]string, answers *string, noInput, accessible *bool) *cobra.Command {
	name, v := in.R.Spec.Name, in.R.Manifest.ProjectVersion
	var to string
	// One per subcommand: a flag writes its default into its variable when it is defined.
	var installTimeout, upTimeout, backTimeout time.Duration
	// run prepares the options, then asks on the terminal when the subcommand asks and the operator
	// did not say --no-input.
	run := func(cmd *cobra.Command, timeout time.Duration, asks bool, f func(context.Context) error) error {
		if err := prepare(); err != nil {
			return err
		}
		in.O.Dir, in.O.K8s.Timeout = ".", timeout
		if asks && !*noInput {
			in.O.Prompt = terminal(*accessible)
		}
		return f(cmd.Context())
	}
	k := &cobra.Command{
		Use:   "k8s",
		Short: fmt.Sprintf("Install and manage %s %s on Kubernetes, with helm and kubectl", name, v),
		Args:  cobra.NoArgs,
	}
	pf := k.PersistentFlags()
	pf.StringVar(&o.K8s.Namespace, "namespace", name, "the release's namespace")
	pf.StringVar(&o.K8s.Release, "release", name, "the helm release's name")
	pf.StringVar(&o.K8s.Context, "kube-context", "", "the kubeconfig context (default: the current one)")
	deployFlags := func(c *cobra.Command, timeout *time.Duration, def time.Duration, what string) {
		f := c.Flags()
		f.StringArrayVar(&o.K8s.Values, "values", nil, "a chart values file, given to helm after the installer's (repeatable)")
		f.StringVar(&o.K8s.Registry, "image-registry", "", "pull the chart and the images from this registry prefix, a mirror")
		f.StringVar(&o.RegistryCA, "registry-ca", "", "a PEM file of the CA the registry's certificate chains to, a mirror's own: the installer's reads trust it (the cluster's nodes must trust it already)")
		f.StringArrayVar(sets, "set", nil, what)
		f.StringVar(answers, "answers", "", "a YAML file of inputs, KEY: value")
		f.BoolVar(noInput, "no-input", false, "never ask: an input nothing gave takes its default")
		f.BoolVar(accessible, "accessible", false, "ask with plain line prompts (screen readers); also when ACCESSIBLE is set or TERM=dumb")
		f.BoolVar(&o.DryRun, "dry-run", false, "check, print what would be applied (secrets redacted), change nothing")
		f.DurationVar(timeout, "timeout", def, "how long helm waits for the release to be ready before it rolls it back")
		f.BoolVar(&o.NoProvision, "no-provision", false, "deploy without running the provision hook (`k8s provision` runs it later)")
	}
	install := &cobra.Command{
		Use:   "install",
		Short: fmt.Sprintf("Install %s %s as a new helm release: its Secrets, then the chart (rolled back when not ready)", name, v),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, installTimeout, true, in.K8sInstall)
		},
	}
	deployFlags(install, &installTimeout, 10*time.Minute, "an input, KEY=VALUE (repeatable)")
	up := &cobra.Command{
		Use:   "upgrade",
		Short: fmt.Sprintf("Move the release to %s, or apply it again with new values; rolled back by helm when not ready", v),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if to != "" {
				return run(cmd, upTimeout, false, func(ctx context.Context) error { return in.K8sUpgradeTo(ctx, to, passed(cmd, "version")) })
			}
			return run(cmd, upTimeout, true, in.K8sUpgrade)
		},
	}
	deployFlags(up, &upTimeout, 6*time.Minute, "an input this release adds, KEY=VALUE (repeatable)")
	up.Flags().StringVar(&to, "version", "", "fetch the installer of this version (its latest revision, on the release's channel) from the release's registry, and upgrade with it")
	back := &cobra.Command{
		Use:   "rollback",
		Short: "Roll the helm release back to its previous revision, or --revision N",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, backTimeout, false, in.K8sRollback)
		},
	}
	back.Flags().IntVar(&o.K8s.Revision, "revision", 0, "the helm revision to go back to (helm history lists them)")
	back.Flags().DurationVar(&backTimeout, "timeout", 6*time.Minute, "how long helm waits for the rolled-back release to be ready")
	back.Flags().BoolVar(&o.DryRun, "dry-run", false, "check, change nothing")
	status := &cobra.Command{
		Use:   "status",
		Short: "What the helm release runs, then helm's status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, 0, false, in.K8sStatus)
		},
	}
	var resetTimeout time.Duration
	reset := &cobra.Command{
		Use:   "reset-db",
		Short: "Uninstall the release, delete its PersistentVolumeClaims and install it again with the same values, as first installed; the release is down meanwhile, nothing is dumped",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, resetTimeout, true, in.K8sReset)
		},
	}
	rf := reset.Flags()
	rf.BoolVarP(&o.Yes, "yes", "y", false, "wipe without asking")
	rf.BoolVar(noInput, "no-input", false, "never ask: without --yes, refuse")
	rf.BoolVar(accessible, "accessible", false, "confirm with a plain line prompt")
	rf.BoolVar(&o.DryRun, "dry-run", false, "print what would be deleted, change nothing")
	rf.StringVar(&o.RegistryCA, "registry-ca", "", "a PEM file of the CA the release's registry's certificate chains to, a mirror's own")
	rf.DurationVar(&resetTimeout, "timeout", 10*time.Minute, "how long helm and kubectl wait at each step")
	rf.BoolVar(&o.NoProvision, "no-provision", false, "install again without running the provision hook")
	rf.BoolVar(&o.K8s.NewBaseline, "new-baseline", false, "the release runs another schema baseline: wipe it and install this installer's release instead")
	var uninstallTimeout time.Duration
	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall the release and what the installer made beside it (its Secrets, the provision hook's Secret, account and Job); its PersistentVolumeClaims only with --data or when answered",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, uninstallTimeout, true, in.K8sUninstall)
		},
	}
	uf := uninstall.Flags()
	uf.BoolVar(&o.K8s.Data, "data", false, "delete the release's PersistentVolumeClaims too: its data")
	uf.BoolVarP(&o.Yes, "yes", "y", false, "uninstall without asking (the data is kept unless --data)")
	uf.BoolVar(noInput, "no-input", false, "never ask: without --yes, refuse")
	uf.BoolVar(accessible, "accessible", false, "confirm with a plain line prompt")
	uf.BoolVar(&o.DryRun, "dry-run", false, "print what would be deleted, change nothing")
	uf.DurationVar(&uninstallTimeout, "timeout", 10*time.Minute, "how long helm and kubectl wait at each step")
	k.AddCommand(install, up, back, status, reset, uninstall)
	if in.R.Spec.Hooks.Provision != nil {
		prov := &cobra.Command{
			Use:   "provision",
			Short: "Run the provision hook again on the release, as a Job (idempotent; continues an unfinished one)",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return run(cmd, 0, false, in.K8sProvision)
			},
		}
		pf := prov.Flags()
		pf.StringArrayVar(sets, "set", nil, "set a value the hook returned before or declares it returns (empty forgets it), KEY=VALUE (repeatable)")
		pf.StringVar(&o.RegistryCA, "registry-ca", "", "a PEM file of the CA the release's registry's certificate chains to, a mirror's own")
		k.AddCommand(prov)
	}
	if slices.ContainsFunc(in.R.Spec.Secrets, func(s spec.Secret) bool { return s.Rotation }) {
		var rotateTimeout time.Duration
		k.AddCommand(secretsCommand(in.R.Spec.Name, o.Out, func(c *cobra.Command) {
			c.Flags().DurationVar(&rotateTimeout, "timeout", 6*time.Minute, "how long kubectl waits for each restarted workload to be ready")
		}, func(cmd *cobra.Command, key, pass string) error {
			return run(cmd, rotateTimeout, false, func(ctx context.Context) error { return in.K8sRotateSecret(ctx, key, pass) })
		}, nil))
	}
	return k
}

// readSecretsArg reads --secrets' file, or standard input for "-".
func readSecretsArg(path string, stdin io.Reader) ([]byte, error) {
	r := stdin
	if path != "-" {
		f, err := os.Open(expandHome(path))
		if err != nil {
			return nil, fmt.Errorf("--secrets: %w", err)
		}
		defer f.Close()
		r = f
	}
	return io.ReadAll(io.LimitReader(r, maxSecretsFile+1))
}

// secretsCommand is `secrets`: `rotate KEY [--stage | --finish]`, one pass of a rotated secret's
// rotation run by rotate (none without it), and `export`, the secrets export returns written
// where --output says (none without it); flags adds the shape's own flags to each.
func secretsCommand(name string, stdout io.Writer, flags func(*cobra.Command), rotate func(cmd *cobra.Command, key, pass string) error, export func(cmd *cobra.Command, recipients []string) ([]byte, error)) *cobra.Command {
	s := &cobra.Command{Use: "secrets", Short: "The install's secrets: rotate one, or export them all for a disaster recovery", Args: cobra.NoArgs}
	if rotate != nil {
		var stage, finish bool
		r := &cobra.Command{
			Use:   "rotate KEY",
			Short: "Replace a generated secret in three passes: --stage records a new value as KEY_NEXT, then a run without a flag makes it KEY and keeps the old one as KEY_PREVIOUS, then --finish drops KEY_PREVIOUS; the stack restarts at each pass",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				pass := RotateSwap
				switch {
				case stage && finish:
					return errors.New("--stage and --finish are two passes: give one")
				case stage:
					pass = RotateStage
				case finish:
					pass = RotateFinish
				}
				return rotate(cmd, args[0], pass)
			},
		}
		r.Flags().BoolVar(&stage, "stage", false, "the first pass: record a new value as KEY_NEXT, accepted but not used")
		r.Flags().BoolVar(&finish, "finish", false, "the last pass: drop KEY_PREVIOUS")
		flags(r)
		s.AddCommand(r)
	}
	if export != nil {
		var recipients []string
		var output string
		e := &cobra.Command{
			Use:   "export",
			Short: "Write the install's secrets (generated, given, the provision hook's) encrypted to age public keys: `install --secrets` takes them back, decrypted, on a machine made again",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				if len(recipients) == 0 {
					return errors.New("--recipient is required: an age public key (age1…) the file is encrypted to; its private key stays off this machine")
				}
				raw, err := export(cmd, recipients)
				if err != nil {
					return err
				}
				if output == "-" {
					_, err = stdout.Write(raw)
					return err
				}
				f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					return fmt.Errorf("--output: %w", err)
				}
				if _, err := f.Write(raw); err != nil {
					f.Close()
					return err
				}
				if err := f.Close(); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "wrote %s, encrypted to %d key(s): keep it off this machine\n", output, len(recipients))
				return nil
			},
		}
		e.Flags().StringArrayVar(&recipients, "recipient", nil, "an age public key (age1…) the file is encrypted to (repeatable)")
		e.Flags().StringVarP(&output, "output", "o", name+"-secrets.age", "the file written, never over another; - for standard output")
		flags(e)
		s.AddCommand(e)
	}
	return s
}

// mirrorCommand is `mirror`: a release carried, as one file, to a registry without internet.
func mirrorCommand(in *Installer, o *Options, prepare func() error) *cobra.Command {
	name := in.R.Spec.Name
	m := &cobra.Command{
		Use:   "mirror",
		Short: fmt.Sprintf("Carry %s's latest release to a registry without internet: export it to one file, import that file", name),
		Args:  cobra.NoArgs,
	}
	var channel, output, to, version string
	var older bool
	var revision int
	var disks, shapes []string
	export := &cobra.Command{
		Use:   "export",
		Short: "Write the channel's latest release (or --version's), every artifact and signature, into one OCI layout archive",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := prepare(); err != nil {
				return err
			}
			formats := disks
			if !cmd.Flags().Changed("disks") {
				formats = nil
			} else if len(disks) == 1 && disks[0] == "none" || len(disks) == 0 {
				formats = []string{}
			}
			if revision != 0 && version == "" {
				return errors.New("--revision goes with --version")
			}
			if revision < 0 {
				return fmt.Errorf("--revision %d: a revision is 1 or more", revision)
			}
			req := MirrorExportRequest{Channel: channel, File: output, Version: version, Revision: revision, Formats: formats}
			if cmd.Flags().Changed("shapes") {
				req.Shapes = append([]string{}, shapes...)
			}
			return in.MirrorExport(cmd.Context(), req)
		},
	}
	ef := export.Flags()
	ef.StringVar(&channel, "channel", "", "the channel whose latest release is exported (default: this release's, "+in.R.Manifest.Channel+")")
	ef.StringVarP(&output, "output", "o", "", "the archive written (default: "+name+"-<tag>.oci.tar here); an existing file is refused")
	ef.StringSliceVar(&disks, "disks", nil, "the VM disk formats exported (default: all; none for no disk)")
	ef.StringVar(&version, "version", "", "export this release of the channel (its latest revision) instead of its latest")
	ef.IntVar(&revision, "revision", 0, "with --version: that revision")
	ef.StringSliceVar(&shapes, "shapes", nil, "the shapes the site runs (default: every shape): without kairos no OS bundle and no disk, without kubernetes no chart")
	imp := &cobra.Command{
		Use:   "import FILE",
		Short: "Push an exported archive into a registry (a mirror), by digest, under the tags it was published with",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := prepare(); err != nil {
				return err
			}
			if to == "" {
				return errors.New("--to: the registry prefix to import into is required")
			}
			return in.MirrorImport(cmd.Context(), args[0], to, older)
		},
	}
	imp.Flags().StringVar(&to, "to", "", "the registry prefix the release goes under, e.g. mirror.example:5000/"+name+" (the login is the Docker client's: `docker login`)")
	imp.Flags().StringVar(&o.RegistryCA, "registry-ca", "", "a PEM file of the CA the registry's certificate chains to")
	imp.Flags().BoolVar(&older, "older", false, "import a release that is an earlier publication of its channel than the mirror's (the channel went back where it is published: a yank)")
	m.AddCommand(export, imp)
	return m
}

// passed is the flags given to cmd, but for skip, as the arguments that give them again.
func passed(cmd *cobra.Command, skip string) []string {
	var args []string
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if f.Name == skip || f.Name == "dir" {
			return
		}
		if s, ok := f.Value.(pflag.SliceValue); ok {
			for _, v := range s.GetSlice() {
				args = append(args, "--"+f.Name+"="+v)
			}
			return
		}
		args = append(args, "--"+f.Name+"="+f.Value.String())
	})
	return args
}

// optionalBool is a boolean flag that may be left out: --ipv6, --ipv6=false, or nil.
type optionalBool struct{ v **bool }

func (b optionalBool) String() string {
	if b.v == nil || *b.v == nil {
		return ""
	}
	return strconv.FormatBool(**b.v)
}

func (b optionalBool) Set(s string) error {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	*b.v = &v
	return nil
}

func (optionalBool) Type() string     { return "bool" }
func (optionalBool) IsBoolFlag() bool { return true }

// ipv6Flag adds --ipv6 to f.
func ipv6Flag(f *pflag.FlagSet, v **bool, usage string) {
	f.VarPF(optionalBool{v}, "ipv6", "", usage).NoOptDefVal = "true"
}

// vmCreateCommand is `vm create`: the files a new VM of the release needs, on a hypervisor or a
// cloud (Installer.CreateVM).
func vmCreateCommand(in *Installer, o *Options, prepare func() error, terminal func(bool) Prompter, sets, domains *[]string, answers *string, noInput, accessible *bool) *cobra.Command {
	name, m := in.R.Spec.Name, in.R.Manifest
	var c VMCreate
	var secretsFile string
	var tofuVars []string
	vm := &cobra.Command{
		Use:   "vm",
		Short: fmt.Sprintf("Run %s on a VM of its own: its Kairos OS, from a disk image", name),
		Args:  cobra.NoArgs,
	}
	create := &cobra.Command{
		Use:   "create",
		Short: fmt.Sprintf("Write a new VM's disk image of %s %s and its cloud-config, then print the platform's steps", name, m.ProjectVersion),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			if err := prepare(); err != nil {
				return err
			}
			if !*noInput {
				in.O.Prompt = terminal(*accessible)
			}
			c.Progress = downloadProgress(in.O.Log)
			for _, kv := range tofuVars {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || k == "" {
					return fmt.Errorf("--tofu-var %q: NAME=VALUE", kv)
				}
				if slices.Contains(contractVars, k) || slices.Contains(optionalVars, k) {
					return fmt.Errorf("--tofu-var %s: vm create writes it (the module contract's), from its own flags", k)
				}
				if _, dup := c.Vars[k]; dup {
					return fmt.Errorf("--tofu-var %s is given twice", k)
				}
				if c.Vars == nil {
					c.Vars = map[string]string{}
				}
				c.Vars[k] = v
			}
			if c.Ctl, err = os.Executable(); err != nil {
				return err
			}
			if secretsFile != "" {
				if c.Secrets, err = readSecretsArg(secretsFile, cmd.InOrStdin()); err != nil {
					return err
				}
			}
			return in.CreateVM(cmd.Context(), &c)
		},
	}
	var keys []string
	for _, p := range Platforms {
		keys = append(keys, p.Key)
	}
	f := create.Flags()
	f.StringVar(&c.Platform, "platform", "", "where the VM runs: "+strings.Join(keys, ", ")+" (it decides the disk format and how the cloud-config reaches the VM)")
	f.StringVar(&c.Out, "out", ".", "the directory the disk image and the cloud-config are written to")
	f.StringVar(&c.Hostname, "hostname", "", "the VM's host name (default: "+name+")")
	f.StringVar(&c.User, "user", "", "its administrator, in the admin group, who logs in over SSH (default: your user name, else sysadmin)")
	f.StringVar(&c.SSHKey, "ssh-key", "", "the administrator's SSH public key file (default: ~/.ssh/id_ed25519.pub, id_ecdsa.pub or id_rsa.pub)")
	f.StringVar(&c.Network, "network", "", "dhcp (default) or static")
	f.StringVar(&c.Address, "address", "", "--network static: the address with its prefix, e.g. 192.0.2.10/24")
	f.StringVar(&c.Gateway, "gateway", "", "--network static: the gateway")
	f.BoolVar(&c.IPv6, "ipv6", false, "turn IPv6 on: the VM takes router advertisements (and DHCPv6 with dhcp), and its front IPv6 clients (default: no IPv6)")
	f.StringVar(&c.Address6, "address6", "", "with --ipv6: a static IPv6 address with its prefix, e.g. 2001:db8::10/64, either network")
	f.StringVar(&c.Gateway6, "gateway6", "", "with --address6: the IPv6 gateway, link-local or in its subnet (default: the router advertisements')")
	f.StringSliceVar(&c.DNS, "dns", nil, "--network static: a DNS server (repeatable, or comma-separated)")
	f.StringSliceVar(&c.SSHAllow, "ssh-allow", nil, "the sources SSH accepts, CIDR (repeatable, or comma-separated; default: every source)")
	f.StringVar(&c.Timezone, "timezone", "", "the VM's time zone, an IANA name such as Europe/Paris (default UTC)")
	f.StringVar(&secretsFile, "secrets", "", "the install's secrets, the file `secrets export` wrote, decrypted: the first install waits for them, given over SSH, never in the cloud-config")
	tlsVM := "how the VM's front serves: none a VM can take (" + name + " offers " + strings.Join(in.R.Spec.Front.TLSModes, ", ") + ")"
	if remote := remoteModes(in.R.Spec.Front.TLSModes); len(remote) > 0 {
		tlsVM = "how the VM's front serves: " + strings.Join(remote, ", ") + "; default " + domainMode(remote) + " with --domains, else asked on a terminal"
	}
	f.StringVar(&o.TLS, "tls", "", tlsVM)
	f.StringSliceVar(domains, "domains", nil, "a route's domain, route=host: HTTPS")
	f.IntVar(&o.HTTPPort, "http-port", 0, "the VM's port a TLS front serves HTTP on (default the project's front.http_port, else 80)")
	f.IntVar(&o.HTTPSPort, "https-port", 0, "the VM's port a TLS front serves HTTPS on (default the project's front.https_port, else 443)")
	f.StringArrayVar(sets, "set", nil, "an input, KEY=VALUE (repeatable; never a secret input)")
	f.StringVar(answers, "answers", "", "a YAML file of inputs, KEY: value (never a secret input)")
	f.BoolVar(noInput, "no-input", false, "never ask: what nothing gave takes its default")
	f.BoolVar(accessible, "accessible", false, "ask with plain line prompts (screen readers); also when ACCESSIBLE is set or TERM=dumb")
	f.StringVar(&o.Registry, "registry", "", "a registry prefix to read the disk and the VM's updates from, a mirror")
	f.StringVar(&o.RegistryCA, "registry-ca", "", "a PEM file of the CA the registry's certificate chains to, a mirror's own: trusted here and, through the cloud-config, by the VM")
	var tofuKeys []string
	for _, p := range Platforms {
		if p.Tofu {
			tofuKeys = append(tofuKeys, p.Key)
		}
	}
	f.StringVar(&c.Create, "create", "", CreateTofu+" (OpenTofu creates the VM: "+strings.Join(tofuKeys, ", ")+", or any platform with --module) or "+CreateManual+" (write its disk and seed, print the steps); asked where OpenTofu can, else "+CreateManual)
	f.StringVar(&c.Module, "module", "", "OpenTofu: your own module directory, applied instead of the platform's built-in one (it holds the module contract: README)")
	f.StringArrayVar(&tofuVars, "tofu-var", nil, "OpenTofu: a variable of the module, NAME=VALUE (repeatable): a platform's own, never a credential (those come from the environment)")
	f.StringVar(&c.Name, "vm-name", "", "OpenTofu: the VM's name on its platform: letters and digits on TrueNAS, else lower-case letters, digits and '-' (default: the host name's)")
	f.StringVar(&c.Pool, "pool", "", "OpenTofu on TrueNAS: the pool the VM's disk goes on")
	f.StringVar(&c.Dataset, "dataset", "", "OpenTofu on TrueNAS: the dataset, under the pool, the VM's disk goes in")
	f.StringVar(&c.Bridge, "bridge", "", "OpenTofu on TrueNAS: the bridge or interface the VM's network attaches to")
	f.IntVar(&c.CPUs, "cpus", 0, "OpenTofu: the VM's CPUs (default 2)")
	f.IntVar(&c.MemoryMiB, "memory", 0, "OpenTofu: the VM's memory in MiB (default 4096, at least 1024)")
	f.IntVar(&c.DiskGiB, "disk", 0, "OpenTofu: the VM's disk in GiB (default 40, at least 16)")
	f.StringVar(&c.Host, "host", "", "OpenTofu: the name or address the VM answers at once booted (default: --address's, else the host name)")
	f.DurationVar(&c.Wait, "wait", 0, "OpenTofu: how long to wait for the VM to answer over SSH and install (default 20m)")

	var d VMDestroy
	destroy := &cobra.Command{
		Use:   "destroy HOSTNAME",
		Short: "Destroy a VM `vm create --create " + CreateTofu + "` made — the VM, its disk and data, its uploads — and its OpenTofu state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if err := prepare(); err != nil {
				return err
			}
			if !*noInput {
				in.O.Prompt = terminal(*accessible)
			}
			d.Host = args[0]
			if d.Ctl, err = os.Executable(); err != nil {
				return err
			}
			return in.DestroyVM(cmd.Context(), &d)
		},
	}
	df := destroy.Flags()
	df.BoolVar(&d.Yes, "yes", false, "destroy without asking")
	df.BoolVar(noInput, "no-input", false, "never ask: without --yes, refuse")
	df.BoolVar(accessible, "accessible", false, "ask with plain line prompts (screen readers); also when ACCESSIBLE is set or TERM=dumb")
	vm.AddCommand(create, destroy, truenasCommand(os.Getenv))
	return vm
}

// downloadProgress reports a download to w at every tenth of it.
func downloadProgress(w io.Writer) func(done, total int64) {
	last := int64(-1)
	return func(done, total int64) {
		if total <= 0 {
			return
		}
		if tenth := done * 10 / total; tenth != last {
			last = tenth
			fmt.Fprintf(w, "  %d%% of %d MiB\n", tenth*10, total>>20)
		}
	}
}

// checkDir refuses a directory name no file system takes; anything else is the file system's call.
func checkDir(v string) error {
	if strings.ContainsAny(v, "\x00\n\r") {
		return errors.New("a directory name cannot hold a line break or a NUL")
	}
	return nil
}

// expandHome expands a leading ~/ in a typed directory, as the shell does for --dir.
func expandHome(dir string) string {
	rest, ok := strings.CutPrefix(dir, "~/")
	if dir == "~" {
		rest, ok = "", true
	}
	if !ok {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return dir
	}
	return filepath.Join(home, rest)
}

func parsePorts(items []string) (map[string]int, error) {
	if len(items) == 0 {
		return nil, nil
	}
	out := map[string]int{}
	for _, it := range items {
		k, v, ok := strings.Cut(it, "=")
		n, err := strconv.Atoi(v)
		if !ok || err != nil {
			return nil, fmt.Errorf("--ports: %q is not route=port", it)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("--ports: %s given twice", k)
		}
		out[k] = n
	}
	return out, nil
}

// tlsHelp describes --tls for the modes this project offers.
func tlsHelp(in *Installer) string {
	offered := in.R.Spec.Front.TLSModes
	var defaults []string
	if m := domainMode(offered); m != "" {
		defaults = append(defaults, m+" with --domains")
	}
	if slices.Contains(offered, "byo") {
		defaults = append(defaults, "byo with --cert or --key")
	}
	if slices.Contains(offered, Loopback) {
		defaults = append(defaults, "else asked, or none")
	} else {
		defaults = append(defaults, "else asked")
	}
	return "how the front serves: " + strings.Join(modesHelp(offered), ", ") + "; default " + strings.Join(defaults, ", ") + " (a re-run moves between TLS modes)"
}

// modesHelp lists the TLS modes the project offers, none meaning loopback.
func modesHelp(offered []string) []string {
	var out []string
	for _, m := range offered {
		if m == Loopback {
			m += " (loopback)"
		}
		out = append(out, m)
	}
	return out
}

func parseDomains(items []string) (map[string]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, it := range items {
		k, v, ok := strings.Cut(it, "=")
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("--domains: %q is not route=host", it)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("--domains: %s given twice", k)
		}
		out[k] = v
	}
	return out, nil
}

func parseSets(items []string) (map[string]string, error) {
	out := map[string]string{}
	for _, it := range items {
		k, v, ok := strings.Cut(it, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--set: %q is not KEY=VALUE", it)
		}
		out[k] = v
	}
	return out, nil
}

func readAnswers(path string) (map[string]string, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("--answers %s: a map of KEY: value is expected: %w", path, err)
	}
	return m, nil
}
