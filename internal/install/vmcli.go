package install

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/norvexlabs/stowage/internal/lint"
	"github.com/norvexlabs/stowage/internal/spec"
)

// The verbs `<short>-stow` takes on a VM, where the OS bundle carries it.
const (
	bootCmd         = "boot"
	bootRollbackCmd = "boot-rollback"
	hostdCmd        = "hostd"
	healthGateCmd   = "health-gate"
)

// bootLockWait is how long the boot waits for an operator's run (a service action, a provision)
// to release the install.
const bootLockWait = 10 * time.Minute

// vmCommand is the command line of the <short>-stow an OS bundle carries: the first install and the
// boot run as root on the VM; every other verb asks the host daemon, which root and the admin
// group reach.
// times are the client's clocks (tests shorten them; zero is the real ones).
func vmCommand(r *Release, self string, stdout, stderr io.Writer, terminal func(accessible bool) Prompter, times clientTimes) *cobra.Command {
	name := r.Spec.Name
	o := Options{Out: stdout, Log: stderr, Docker: ExecTool{Bin: "docker"}, Self: self, Dir: VMDir(name), Offline: true, Preloaded: true,
		Firewall: PortRules, Host: HostTool{}, Tune: vmTune(r.Ctl()), TunedAtBoot: true}
	if _, err := os.Stat("/proc/self/exe"); err == nil {
		o.SelfExe = "/proc/self/exe"
	}
	in := &Installer{R: r}
	var ports, sets, domains []string
	var answers, secretsFile string
	var noInput, accessible, asJSON bool
	var group string
	socket := Socket(name)
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
	c := &hostClient{http: socketClient(&socket), out: stdout, clientTimes: times, ctl: r.Ctl()}
	root := &cobra.Command{
		Use:           c.ctl,
		Short:         fmt.Sprintf("Manage %s on this VM (its OS carries %s, revision %d)", name, r.Manifest.ProjectVersion, r.Manifest.Revision),
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&socket, "socket", socket, "the host daemon's socket")
	_ = root.PersistentFlags().MarkHidden("socket")
	dirFlag := func(cmd *cobra.Command) {
		cmd.Flags().StringVar(&o.Dir, "dir", o.Dir, "the install directory")
		_ = cmd.Flags().MarkHidden("dir")
	}
	settingsFlag := func(cmd *cobra.Command) {
		cmd.Flags().StringVar(&o.Settings, "settings", SettingsFile(name), "the settings file a first install takes its values from")
		_ = cmd.Flags().MarkHidden("settings")
	}

	install := &cobra.Command{
		Use:   "install",
		Short: fmt.Sprintf("Install %s %s on this VM from the images its OS carries (as root)", name, r.Manifest.ProjectVersion),
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := prepare(); err != nil {
				return err
			}
			if ch := in.O.Channel; ch != "" && !lint.ChannelName(ch) {
				return fmt.Errorf("--channel %q is not a channel name", ch)
			}
			if secretsFile != "" {
				if in.O.NewSecrets {
					return errors.New("--secrets gives the secrets, --new-secrets generates them: give one")
				}
				var err error
				if in.O.Secrets, err = readSecretsArg(secretsFile, cmd.InOrStdin()); err != nil {
					return err
				}
			}
			if err := in.vmInstallReady(cmd.Context(), os.Geteuid(), dirWritable); err != nil {
				return err
			}
			if err := in.withSettings(); err != nil {
				return err
			}
			if !noInput {
				in.O.Prompt = terminal(accessible)
			}
			in.O.Cert, in.O.Key, in.O.CA, in.O.ClientCA = expandHome(in.O.Cert), expandHome(in.O.Key), expandHome(in.O.CA), expandHome(in.O.ClientCA)
			if err := in.Install(cmd.Context()); err != nil {
				return err
			}
			in.retune(cmd.Context(), vmTune(r.Ctl()), stderr)
			in.Finished(cmd.Context(), onVM)
			return nil
		},
	}
	f := install.Flags()
	f.StringSliceVar(&ports, "ports", nil, "a route's port, route=port (loopback mode; on an installed VM, changes it)")
	f.StringVar(&o.Address, "address", "", "loopback address the ports bind to, default 127.0.0.1 (or ::1)")
	ipv6Flag(f, &o.IPv6, "IPv6 on the stack's network, so the front's IPv6 clients keep their address; --ipv6=false turns it off (default: the cloud-config's, `vm create --ipv6`; on an installed VM, changes it)")
	tuneProfileFlag(f, &o.TuneProfile, "the VM's tuning profile over the release's tune.profile ("+r.Spec.TuneProfile()+"): default or network; release follows the release's again (default: the cloud-config's, `vm create --tune-profile`; on an installed VM, changes it: a higher profile applies at once, a lower one at the next reboot)")
	f.StringVar(&o.TLS, "tls", "", tlsHelp(in))
	f.StringSliceVar(&domains, "domains", nil, "a route's domain, route=host: HTTPS (on an installed VM, changes it)")
	f.StringVar(&o.Cert, "cert", "", "--tls byo: the certificate chain, PEM, copied into the install")
	f.StringVar(&o.Key, "key", "", "--tls byo: the certificate's private key, PEM, copied into the install")
	if r.Spec.Front.PublishCA != nil {
		f.StringVar(&o.CA, "ca", "", "--tls byo: the CA the certificate chains to, PEM, published to the stack (none: no CA)")
	}
	if len(clientAuthRoutes(r.Spec.Front)) > 0 {
		f.StringVar(&o.ClientCA, "client-ca", "", "the CA, PEM, the client certificates of the routes with client_auth chain to, copied into the install (on an installed VM, replaces it; none removes it)")
	}
	f.IntVar(&o.HTTPPort, "http-port", 0, "the host port a TLS front serves HTTP on (default the project's front.http_port, else 80; on an installed VM, changes it)")
	f.IntVar(&o.HTTPSPort, "https-port", 0, "the host port a TLS front serves HTTPS on (default the project's front.https_port, else 443; on an installed VM, changes it)")
	f.StringVar(&o.Channel, "channel", "", "the channel `updates` and `upgrade` follow")
	hideFrontFlags(f, r.Spec, frontFlagNames)
	f.BoolVar(&o.DryRun, "dry-run", false, "check, print what would be written (secrets redacted), write nothing")
	f.StringArrayVar(&sets, "set", nil, "an input, KEY=VALUE (repeatable; on an installed VM, changes the recorded one)")
	f.StringVar(&answers, "answers", "", "a YAML file of inputs, KEY: value (on an installed VM, changes the recorded ones)")
	f.BoolVar(&noInput, "no-input", false, "never ask: an input nothing gave takes its default")
	f.BoolVar(&o.NoProvision, "no-provision", false, "start the stack without running the provision hook (`provision` runs it later)")
	f.StringVar(&secretsFile, "secrets", "", "a first install's secrets: the file `secrets export` wrote, decrypted (- for standard input)")
	f.BoolVar(&o.NewSecrets, "new-secrets", false, "generate the secrets though `vm create --secrets` made the install wait for exported ones")
	f.BoolVar(&accessible, "accessible", false, "ask with plain line prompts (screen readers); also when ACCESSIBLE is set or TERM=dumb")
	dirFlag(install)
	settingsFlag(install)

	boot := &cobra.Command{
		Use:    bootCmd,
		Short:  "Bring the install up with this OS's release: upgrade it to that release, or go back to it (run at boot)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := prepare(); err != nil {
				return err
			}
			in.O.LockWait = bootLockWait
			return in.Boot(cmd.Context())
		},
	}
	dirFlag(boot)
	settingsFlag(boot)
	back := &cobra.Command{
		Use:    bootRollbackCmd,
		Short:  "Go back to the release the last upgrade replaced, which the VM booted (run at boot)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := prepare(); err != nil {
				return err
			}
			in.O.LockWait = bootLockWait
			return in.BootRollback(cmd.Context())
		},
	}
	dirFlag(back)
	hostd := &cobra.Command{
		Use:    hostdCmd,
		Short:  "Serve the host daemon on its socket (a systemd service)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := prepare(); err != nil {
				return err
			}
			h := &Hostd{R: r, O: in.O, Host: HostTool{}, Group: group}
			return h.Serve(cmd.Context(), socket)
		},
	}
	dirFlag(hostd)
	hostd.Flags().StringVar(&group, "group", AdminGroup, "the group the socket opens to besides root (empty: root's alone)")
	_ = hostd.Flags().MarkHidden("group")
	gate := &cobra.Command{
		Use:    healthGateCmd,
		Short:  "Judge this boot: mark an upgraded boot good once the install is healthy, or reboot into the passive system (run at boot)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := prepare(); err != nil {
				return err
			}
			return (&Gate{In: in, Host: HostTool{}}).Run(cmd.Context())
		},
	}
	dirFlag(gate)
	netBoot := netBootCommand(name, c.ctl, &o.Dir, stderr)
	dirFlag(netBoot)
	root.AddCommand(install, boot, back, hostd, gate, netBoot, tuneBootCommand(r, stderr), tuneCommand(in, prepare, terminal, true))

	jsonFlag := func(cmd *cobra.Command) {
		cmd.Flags().BoolVar(&asJSON, "json", false, "print the daemon's JSON answer")
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "The release this OS carries, the install's, the disk and the services",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return c.status(cmd.Context(), asJSON)
		},
	}
	jsonFlag(status)
	ps := &cobra.Command{
		Use:   "ps",
		Short: "The stack's services and their state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return c.services(cmd.Context(), asJSON)
		},
	}
	jsonFlag(ps)
	var tail int
	logs := &cobra.Command{
		Use:   "logs SERVICE",
		Short: "A service's last log lines",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var out struct{ Logs string }
			if err := c.do(cmd.Context(), http.MethodGet, "/v1/services/"+url.PathEscape(args[0])+"/logs?tail="+strconv.Itoa(tail), nil, &out); err != nil {
				return err
			}
			_, err := io.WriteString(stdout, out.Logs)
			return err
		},
	}
	logs.Flags().IntVar(&tail, "tail", 200, "how many lines (1 to 5000)")
	health := &cobra.Command{
		Use:   "health",
		Short: "Whether the install is ready, as the health gate judges it: every service up (healthy where it has a healthcheck), and the health provider's answer",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var h Health
			if err := c.do(cmd.Context(), http.MethodGet, "/v1/health", nil, &h); err != nil {
				return err
			}
			if asJSON {
				if err := printJSON(stdout, h); err != nil {
					return err
				}
			} else if h.Ready {
				fmt.Fprintln(stdout, "ready")
			}
			if !h.Ready {
				return fmt.Errorf("not ready: %s", h.Reason)
			}
			return nil
		},
	}
	jsonFlag(health)
	var after int64
	var followEvents bool
	events := &cobra.Command{
		Use:   "events",
		Short: "What the host daemon and the health gate did, from the event journal: jobs, reboots, boots and their verdicts, the first install",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if after < 0 {
				return fmt.Errorf("--after %d is not an event number", after)
			}
			return c.events(cmd.Context(), stderr, after, followEvents, asJSON)
		},
	}
	events.Flags().Int64Var(&after, "after", 0, "the entries after this number (status --json's events is the last one)")
	events.Flags().BoolVarP(&followEvents, "follow", "f", false, "then wait for new entries, until interrupted")
	events.Flags().BoolVar(&asJSON, "json", false, "print each entry as the daemon's JSON, one per line")
	root.AddCommand(status, health, ps, logs, events)
	for _, a := range []string{"restart", "stop", "start"} {
		root.AddCommand(&cobra.Command{
			Use:   a + " SERVICE",
			Short: strings.ToUpper(a[:1]) + a[1:] + " one service of the stack",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := c.do(cmd.Context(), http.MethodPost, "/v1/services/"+url.PathEscape(args[0])+"/"+a, nil, nil); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "%s: %s done\n", args[0], a)
				return nil
			},
		})
	}
	var provSets []string
	prov := &cobra.Command{
		Use:   "provision",
		Short: "Run the provision hook again on the running stack (idempotent)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			set, err := parseSets(provSets)
			if err != nil {
				return err
			}
			var out struct{ Output string }
			if err := c.do(cmd.Context(), http.MethodPost, "/v1/provision", ProvisionRequest{Set: set}, &out); err != nil {
				return err
			}
			_, err = io.WriteString(stdout, out.Output)
			return err
		},
	}
	prov.Flags().StringArrayVar(&provSets, "set", nil, "correct an input only the hook reads, or set a value it returned or declares it returns (empty forgets it), KEY=VALUE (repeatable)")
	var channel, version string
	var yes, noReboot, skipBackup bool
	updates := &cobra.Command{
		Use:   "updates",
		Short: "The release an upgrade would install: the channel's latest, or --version's",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			u, err := c.updates(cmd.Context(), channel, version)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(stdout, u)
			}
			printUpdate(stdout, u)
			return nil
		},
	}
	updates.Flags().StringVar(&channel, "channel", "", "the channel to look on (default: the install's; before the first install, its settings')")
	updates.Flags().StringVar(&version, "version", "", "this version's latest revision, not the channel's latest release")
	jsonFlag(updates)
	upgrade := &cobra.Command{
		Use:   "upgrade",
		Short: "Write a newer release's OS to the passive slot and reboot into it; the boot upgrades the install (database dumped first)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			u, err := c.updates(ctx, channel, version)
			if err != nil {
				return err
			}
			printUpdate(stdout, u)
			if !u.Available {
				return nil
			}
			if !yes {
				p := terminal(accessible)
				if p == nil {
					return errors.New("no terminal to confirm on: --yes upgrades without asking")
				}
				a, err := p.Ask(ctx, Form{Questions: []Question{{
					Title: fmt.Sprintf("Upgrade to %s and reboot?", u.Target), Key: "--yes", Required: true,
					Choices: []Choice{{"yes", "Upgrade"}, {"no", "Not now"}},
				}}})
				if err != nil {
					return err
				}
				if a["--yes"] != "yes" {
					return ErrAborted
				}
			}
			reboot := !noReboot
			req := UpgradeRequest{Channel: channel, Version: version, Confirm: &u.Target, Reboot: &reboot, SkipBackup: skipBackup}
			var started JobStarted
			if err := c.do(ctx, http.MethodPost, "/v1/upgrade", req, &started); err != nil {
				return err
			}
			return c.follow(ctx, stderr, started)
		},
	}
	uf := upgrade.Flags()
	uf.StringVar(&channel, "channel", "", "the channel to upgrade on (default: the install's; before the first install, its settings')")
	uf.StringVar(&version, "version", "", "this version's latest revision, not the channel's latest release")
	uf.BoolVarP(&yes, "yes", "y", false, "upgrade without asking")
	uf.BoolVar(&noReboot, "no-reboot", false, "write the new OS but do not reboot (the next boot runs it)")
	uf.BoolVar(&skipBackup, "skip-backup", false, "no backup first, though one is configured")
	uf.BoolVar(&accessible, "accessible", false, "confirm with a plain line prompt")
	var drain int
	power := func(use, short, path string) *cobra.Command {
		cmd := &cobra.Command{
			Use:   use,
			Short: short,
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				if err := c.do(cmd.Context(), http.MethodPost, path, PowerRequest{DrainSecs: &drain}, nil); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "%s in %d s\n", use, drain)
				return nil
			},
		}
		cmd.Flags().IntVar(&drain, "drain-secs", 30, "seconds before it (0 to 600)")
		return cmd
	}
	rollback := &cobra.Command{
		Use:   "rollback",
		Short: "Go back to the release the last upgrade replaced: reboot into the passive system, which the VM keeps running (refused when the database moved past what that release reads)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			var p RollbackPlan
			if err := c.do(ctx, http.MethodGet, "/v1/rollback", nil, &p); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "the passive system carries %s %s; the install runs %s\n", name, p.To, p.From)
			if !yes {
				t := terminal(accessible)
				if t == nil {
					return errors.New("no terminal to confirm on: --yes goes back without asking")
				}
				a, err := t.Ask(ctx, Form{Questions: []Question{{
					Title: fmt.Sprintf("Go back to %s and reboot?", p.To), Key: "--yes", Required: true,
					Choices: []Choice{{"yes", "Go back"}, {"no", "Not now"}},
				}}})
				if err != nil {
					return err
				}
				if a["--yes"] != "yes" {
					return ErrAborted
				}
			}
			var started JobStarted
			if err := c.do(ctx, http.MethodPost, "/v1/rollback", RollbackRequest{Confirm: &p.To}, &started); err != nil {
				return err
			}
			return c.follow(ctx, stderr, started)
		},
	}
	rollback.Flags().BoolVarP(&yes, "yes", "y", false, "go back without asking")
	rollback.Flags().BoolVar(&accessible, "accessible", false, "confirm with a plain line prompt")
	var noProvision, newBaseline bool
	var resetChannel, resetVersion string
	reset := &cobra.Command{
		Use:   "reset-db",
		Short: "Wipe the data (the volumes data.reset names) and start again as first installed, without a reboot; the service stops meanwhile, nothing is dumped. With --new-baseline, a later release at another schema baseline is installed instead: its OS is written and booted, and that boot wipes the data",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			// The daemon's refusals come before the question.
			switch {
			case !newBaseline && (resetChannel != "" || resetVersion != ""):
				return errors.New("--channel and --version name the release --new-baseline installs: give --new-baseline, or neither")
			case newBaseline && noProvision:
				return errors.New("--no-provision is not given to the boot that crosses the baseline: `provision` runs the hook afterwards")
			}
			path := "/v1/reset-db"
			if newBaseline {
				q := url.Values{"new_baseline": {"true"}}
				if resetChannel != "" {
					q.Set("channel", resetChannel)
				}
				if resetVersion != "" {
					q.Set("version", resetVersion)
				}
				path += "?" + q.Encode()
			}
			var p ResetPlan
			if err := c.do(ctx, http.MethodGet, path, nil, &p); err != nil {
				return err
			}
			var t Prompter
			if !yes {
				t = terminal(accessible)
			}
			if err := confirmReset(ctx, t, yes, name+" "+p.Target.String(), p.Plan); err != nil {
				return err
			}
			var started JobStarted
			if err := c.do(ctx, http.MethodPost, "/v1/reset-db", ResetRequest{Confirm: &p.Target, Token: p.Token, NoProvision: noProvision,
				NewBaseline: newBaseline, Channel: resetChannel, Version: resetVersion}, &started); err != nil {
				return err
			}
			return c.follow(ctx, stderr, started)
		},
	}
	reset.Flags().BoolVarP(&yes, "yes", "y", false, "wipe without asking")
	reset.Flags().BoolVar(&noProvision, "no-provision", false, "start the stack without running the provision hook")
	reset.Flags().BoolVar(&accessible, "accessible", false, "confirm with a plain line prompt")
	reset.Flags().BoolVar(&newBaseline, "new-baseline", false, "install a later release at another schema baseline instead: its OS is written and booted, and that boot wipes the data")
	reset.Flags().StringVar(&resetChannel, "channel", "", "with --new-baseline: the channel to find the release on (default: the install's)")
	reset.Flags().StringVar(&resetVersion, "version", "", "with --new-baseline: this version's latest revision, not the channel's latest release")
	var rotate func(cmd *cobra.Command, key, pass string) error
	if slices.ContainsFunc(r.Spec.Secrets, func(s spec.Secret) bool { return s.Rotation }) {
		rotate = func(cmd *cobra.Command, key, pass string) error {
			var started JobStarted
			if err := c.do(cmd.Context(), http.MethodPost, "/v1/secrets/rotate", SecretRotateRequest{Key: key, Pass: pass}, &started); err != nil {
				return err
			}
			return c.follow(cmd.Context(), stderr, started)
		}
	}
	secrets := secretsCommand(name, stdout, func(*cobra.Command) {}, rotate, func(cmd *cobra.Command, recipients []string) ([]byte, error) {
		var out SecretsExport
		err := c.do(cmd.Context(), http.MethodPost, "/v1/secrets/export", SecretsExportRequest{Recipients: recipients}, &out)
		return []byte(out.File), err
	})
	restore := restoreCommand(c, name, "sudo "+c.ctl+" install", stderr, terminal)
	root.AddCommand(prov, updates, upgrade, rollback, reset, secrets, restore, networkCommand(c, name, stdout), backupCommand(c, name, stdout, stderr),
		power("reboot", "Reboot the VM", "/v1/reboot"),
		power("shutdown", "Power the VM off", "/v1/shutdown"))
	return root
}

// restoreCommand is `restore OBJECT`, asking the host daemon c reaches; install names the command
// that installs.
func restoreCommand(c *hostClient, name, install string, stderr io.Writer, terminal func(accessible bool) Prompter) *cobra.Command {
	var yes, accessible bool
	var identity string
	restore := &cobra.Command{
		Use:   "restore OBJECT",
		Short: "Load a backup (`backup list` names them) of the release the install runs into its database; the age identity that decrypts it is read from standard input or --identity, and kept nowhere; the services but the database stop meanwhile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			var st Status
			if err := c.do(ctx, http.MethodGet, "/v1/status", nil, &st); err != nil {
				return err
			}
			if st.Installed == nil {
				return fmt.Errorf("%s is not installed yet: `%s` installs it", name, install)
			}
			key, err := readFileOrStdin(identity, cmd.InOrStdin())
			if err != nil {
				return fmt.Errorf("--identity: %w", err)
			}
			if !yes {
				t := terminal(accessible)
				if t == nil {
					return errors.New("no terminal to confirm on: --yes restores without asking")
				}
				a, err := t.Ask(ctx, Form{Questions: []Question{{
					Title: fmt.Sprintf("Replace %s %s's data with %s?", name, st.Installed, args[0]), Key: "--yes", Required: true,
					Choices: []Choice{{"yes", "Restore"}, {"no", "Not now"}},
				}}})
				if err != nil {
					return err
				}
				if a["--yes"] != "yes" {
					return ErrAborted
				}
			}
			var started JobStarted
			if err := c.do(ctx, http.MethodPost, "/v1/restore", RestoreRequest{Object: args[0], Identity: string(key), Confirm: st.Installed}, &started); err != nil {
				return err
			}
			return c.follow(ctx, stderr, started)
		},
	}
	restore.Flags().StringVar(&identity, "identity", "-", "the age identity file that decrypts the backup; - for standard input")
	restore.Flags().BoolVarP(&yes, "yes", "y", false, "restore without asking")
	restore.Flags().BoolVar(&accessible, "accessible", false, "confirm with a plain line prompt")
	return restore
}

// socketClient is an HTTP client that dials the unix socket *path.
func socketClient(path *string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", *path)
		},
	}}
}

// hostClient asks the host daemon.
type hostClient struct {
	http *http.Client
	out  io.Writer
	// ctl is the installer's name, which messages give the operator.
	ctl string
	clientTimes
}

// clientTimes are the host client's clocks. unit is its second, which its waits count (tests
// shorten it); 0 is a second. margin is how long a follow's request may take past the wait it asks
// for, whatever the unit; 0 is requestMargin. Only a test of a daemon that never answers
// shortens it: a short one would fail a request a loaded machine answers late.
type clientTimes struct{ unit, margin time.Duration }

// second is the client's second.
func (c *hostClient) second() time.Duration { return cmp.Or(c.unit, time.Second) }

// patience is how long a follow's request may take past its wait.
func (c *hostClient) patience() time.Duration { return cmp.Or(c.margin, requestMargin) }

// unreachable is a request that got no answer from the daemon: it is not running, or it stopped
// before answering.
type unreachable struct{ err error }

func (u unreachable) Error() string { return u.err.Error() }
func (u unreachable) Unwrap() error { return u.err }

// do sends body (JSON, when not nil) and decodes the answer into out (when not nil).
func (c *hostClient) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://hostd"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("the host daemon's socket: permission denied (are you root or in the %s group?)", AdminGroup)
		}
		return unreachable{fmt.Errorf("the host daemon is not reachable (systemctl status of its service tells why): %w", err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return unreachable{fmt.Errorf("the host daemon stopped before it answered: %w", err)}
	}
	if resp.StatusCode >= 400 {
		var e struct{ Error string }
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("the host daemon answered %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("the host daemon's answer: %w", err)
	}
	return nil
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (c *hostClient) status(ctx context.Context, asJSON bool) error {
	var st Status
	if err := c.do(ctx, http.MethodGet, "/v1/status", nil, &st); err != nil {
		return err
	}
	if asJSON {
		return printJSON(c.out, st)
	}
	fmt.Fprintf(c.out, "os         %s %s\n", st.Name, st.OS)
	if st.Installed == nil {
		fmt.Fprintf(c.out, "installed  no (`sudo %s install` installs it)\n", c.ctl)
	} else {
		fmt.Fprintf(c.out, "installed  %s %s\n", st.Name, st.Installed)
		switch {
		case st.VolumesSplit:
			fmt.Fprintf(c.out, "           not running: a restore left the volumes half restored; `sudo %s restore OBJECT` again finishes it\n", c.ctl)
		case st.InstallFailed != "":
			fmt.Fprintf(c.out, "           not finished: the first install failed: %s (`journalctl -u %s` tells more); `sudo %s install` runs it again\n", st.InstallFailed, st.Name, c.ctl)
		case st.Installing:
			fmt.Fprintln(c.out, "           not finished: the stack is coming up, or the provision hook runs")
		case st.RestoreAwaited:
			fmt.Fprintf(c.out, "           not provisioned: a backup of the install --secrets came from is awaited (`sudo %s restore OBJECT` loads it and provisions)\n", c.ctl)
		}
	}
	if st.Channel != "" {
		fmt.Fprintf(c.out, "channel    %s\n", st.Channel)
	}
	if st.Tune != "" {
		fmt.Fprintf(c.out, "tune       %s\n", st.Tune)
	}
	if st.Boot != "" {
		boot := st.Boot
		if st.Fallback {
			boot += " (the fallback: the last upgrade failed its health gate, or `rollback` chose it)"
		}
		fmt.Fprintf(c.out, "boot       %s\n", boot)
	}
	if fb := st.FailedBoot; fb != nil {
		fmt.Fprintf(c.out, "failed     %s %s at %s: %s\n           diagnostics: %s\n", st.Name, fb.Release, fb.At.Format(time.RFC3339), fb.Reason, fb.Dir)
		if fb.Kept != "" {
			fmt.Fprintf(c.out, "           kept on this system, as going back is refused: %s\n", strings.ReplaceAll(fb.Kept, "\n", "\n           "))
		}
	}
	if d := st.Disk; d != nil && d.Total > 0 {
		fmt.Fprintf(c.out, "disk       %s: %s free of %s\n", d.Path, size(d.Available), size(d.Total))
	}
	if p := st.Pressure; p != nil {
		if p.Level == "unknown" {
			fmt.Fprintf(c.out, "pressure   unknown: the disk cannot be read (soft %d%%, hard %d%%)\n", p.SoftPct, p.HardPct)
		} else {
			fmt.Fprintf(c.out, "pressure   %s: %.1f%% used (soft %d%%, hard %d%%)\n", p.Level, p.UsedPct, p.SoftPct, p.HardPct)
		}
		if l := p.Last; l != nil {
			fmt.Fprintf(c.out, "           last POST at %s (%s, %.1f%%): %s\n", l.At.Format(time.RFC3339), l.Level, l.UsedPct, l.Result)
		}
	}
	if ct := st.Cert; ct != nil {
		fmt.Fprintf(c.out, "cert       %s, valid until %s (%d days)\n", ct.Subject, ct.NotAfter.Format(time.DateOnly), ct.DaysLeft)
		if ct.Warning != "" {
			fmt.Fprintf(c.out, "           warning: %s\n", ct.Warning)
		}
	}
	if st.RebootPending {
		fmt.Fprintln(c.out, "reboot     scheduled")
	}
	if st.Job != "idle" {
		fmt.Fprintf(c.out, "job        %s\n", st.Job)
	}
	if st.Installed == nil {
		return nil
	}
	return c.services(ctx, false)
}

func size(b uint64) string {
	const g = 1 << 30
	return fmt.Sprintf("%.1f GiB", float64(b)/g)
}

func (c *hostClient) services(ctx context.Context, asJSON bool) error {
	var list []Service
	if err := c.do(ctx, http.MethodGet, "/v1/services", nil, &list); err != nil {
		return err
	}
	if asJSON {
		return printJSON(c.out, list)
	}
	for _, s := range list {
		state := s.State
		if s.Health != "" {
			state += " (" + s.Health + ")"
		}
		fmt.Fprintf(c.out, "%-20s %-22s %s\n", s.Service, state, s.Image)
	}
	return nil
}

func (c *hostClient) updates(ctx context.Context, channel, version string) (Update, error) {
	q := url.Values{}
	if channel != "" {
		q.Set("channel", channel)
	}
	if version != "" {
		q.Set("version", version)
	}
	path := "/v1/updates"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var u Update
	err := c.do(ctx, http.MethodGet, path, nil, &u)
	return u, err
}

func printUpdate(w io.Writer, u Update) {
	switch {
	case u.Passive && u.Installed != nil && *u.Installed != u.Current && !u.Available:
		defer fmt.Fprintf(w, "the install runs %s, which this OS (%s) does not carry: `upgrade --version %s` writes its OS\n", *u.Installed, u.Current, u.Installed.Version)
	case u.Passive && !u.Rewrite && !u.Available:
		defer fmt.Fprintf(w, "the VM runs the passive system (a boot that failed went back to it): `upgrade --version %s` writes %s's latest revision (%s again when it is that one) to the active slot\n", u.Current.Version, u.Current.Version, u.Current)
	}
	if u.LeftByRollback {
		fmt.Fprintf(w, "no upgrade on %s: the install went back from %s, and the boot does not upgrade to it again\n", u.Channel, u.Target)
		return
	}
	if !u.Available {
		fmt.Fprintf(w, "no upgrade on %s: this OS runs %s, the release found is %s\n", u.Channel, u.Current, u.Target)
		return
	}
	if u.Rewrite {
		fmt.Fprintf(w, "%s on %s: the VM runs it from the passive system (a boot that failed went back to it); an upgrade writes it to the active slot again\n", u.Target, u.Channel)
		return
	}
	fmt.Fprintf(w, "%s on %s: this OS runs %s\n", u.Target, u.Channel, u.Current)
	if u.Changelog != "" {
		fmt.Fprintf(w, "\n%s\n", strings.TrimSpace(u.Changelog))
	}
}

// follow prints the log of the job started until it ends; a failed job is an error, and so is
// a job lost, another job in its place, and an interrupted follow. A job done that scheduled a
// reboot or power-off ends the follow there (rebootNote): the follow runs on the VM, which that
// reboot stops, so its outcome is the journal's, read after it.
func (c *hostClient) follow(ctx context.Context, w io.Writer, started JobStarted) error {
	_, err := c.followJob(ctx, w, started)
	return err
}

// jobEvents is what the event journal, and GET job, told of a job, the journal read from after
// on.
type jobEvents struct {
	started JobStarted
	after   int64
	// since is the job's log lines printed; unlogged tells unlogged said so.
	since    int
	unlogged bool
	// blind: the journal does not hold the job's start (its 202 gave no number), so the follow
	// reads the job alone, and never the journal.
	blind bool
	// job is the job's end as GET job answered it (done or failed); nil until read.
	job *JobStatus
	// ended is the job's job.ended, lost its job.lost.
	ended, lost *Event
}

// read tells the job's end or loss was read: from then on it is the follow's answer, whatever a
// later request meets.
func (f *jobEvents) read() bool { return f.final() != nil || f.lost != nil }

// final is the job's end: GET job's, else the journal's job.ended (whose log is not read); nil
// when neither was read.
func (f *jobEvents) final() *JobStatus {
	if f.job != nil {
		return f.job
	}
	e := f.ended
	if e == nil {
		return nil
	}
	return &JobStatus{Started: &f.started.Started, Kind: e.JobKind, Target: e.Target, State: e.State, Error: e.Error, Result: e.Result, Reboot: e.Reboot, Log: []string{}}
}

const (
	// requestMargin is how long a follow's request may take past the wait it asks for, in real
	// time (never the client's shortened second): a daemon that takes the request and never
	// answers is then one that does not answer.
	requestMargin = 120 * time.Second
)

// interrupted is the error of a follow its context ended (Ctrl-C): what the job did is not
// known here, never taken as done.
func interrupted(ctx context.Context, what string) error {
	return fmt.Errorf("interrupted before %s: `status` and `events` tell how it went: %w", what, ctx.Err())
}

// stopped answers a follow its context ended: from the job's end or loss when it was read
// (known), whatever stopped the follow; anything else is interrupted.
func (c *hostClient) stopped(ctx context.Context, w io.Writer, f *jobEvents, what string) (JobStatus, error) {
	if f.read() {
		return c.known(w, f)
	}
	return JobStatus{}, interrupted(ctx, what)
}

// known answers a follow from the job's end (endedAs), else its loss (f.read holds).
func (c *hostClient) known(w io.Writer, f *jobEvents) (JobStatus, error) {
	if f.final() != nil {
		return c.endedAs(w, f)
	}
	return JobStatus{}, fmt.Errorf("%s %s lost: %s", f.started.Job, f.started.Target, f.lost.Reason)
}

// rebootSeen is how a follow read the end of a job that scheduled a reboot: from GET job
// (seenLive), or from the journal (seenJournal).
type rebootSeen int

const (
	seenLive rebootSeen = iota
	seenJournal
)

// doneAs answers a job done: with the reboot or power-off it scheduled, it says so and where
// that reboot's outcome is read (rebootNote). The follow asks nothing more.
func (c *hostClient) doneAs(w io.Writer, j JobStatus, seen rebootSeen) (JobStatus, error) {
	if j.Reboot != nil {
		fmt.Fprintln(w, c.rebootNote(j, seen))
	}
	return j, nil
}

// rebootNotes is, per action a job schedules, what the VM does (counted down by rebootNote) and
// what follows it.
var rebootNotes = map[string]struct{ verb, then string }{
	"reboot":   {"reboots", " to run it — after it"},
	"poweroff": {"powers off", ", and its next start runs it — after that start"},
}

// rebootNote is the line a follow ends with at the reboot or power-off its job scheduled: the
// follow runs on the VM, so it never follows that reboot; status and events tell its outcome
// after it. It counts the drain down only from GET job's answer (seenLive) and a drain that is
// not 0: an end read from the journal was written a part of the drain ago. It never says the
// reboot is under way: a daemon that stops answering tells nothing of it.
func (c *hostClient) rebootNote(j JobStatus, seen rebootSeen) string {
	n, ok := rebootNotes[j.Reboot.Action]
	if !ok {
		n = rebootNotes["reboot"]
	}
	if seen == seenLive && j.Reboot.DrainSecs > 0 {
		n.verb += fmt.Sprintf(" in %d s", j.Reboot.DrainSecs)
	}
	ctl := c.ctl
	return fmt.Sprintf("%s done: the VM %s%s, `%s status` and `%s events` tell how it went", j.Kind, n.verb, n.then, ctl, ctl)
}

// logged prints j's log lines (from f.since on) and moves f.since past them.
func (c *hostClient) logged(w io.Writer, f *jobEvents, j JobStatus) {
	for _, l := range j.Log {
		fmt.Fprintf(w, "  %s\n", l)
	}
	f.since = max(f.since, j.LogTotal)
}

// unlogged says, of a job answered from its journaled end (never read done or failed from GET
// job), that its last log lines were not read — whatever kept them unread.
func (c *hostClient) unlogged(w io.Writer, f *jobEvents) {
	if f.job == nil && f.ended != nil && !f.unlogged {
		f.unlogged = true
		fmt.Fprintf(w, "the %s's last log lines were not read: its end is the journal's (`%s events`)\n",
			f.started.Job, c.ctl)
	}
}

// within is do, given wait of the client's seconds (the wait the request asks the daemon for) and
// the client's patience to answer.
func (c *hostClient) within(ctx context.Context, wait int, method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(wait)*c.second()+c.patience())
	defer cancel()
	return c.do(ctx, method, path, body, out)
}

// await reads the journal's entries after f.after, waiting up to wait seconds for one, and
// the client's patience more for the answer. A context that ended is its error, whatever the
// request's.
func (c *hostClient) await(ctx context.Context, f *jobEvents, wait int) error {
	var a EventsAnswer
	if err := c.within(ctx, wait, http.MethodGet, fmt.Sprintf("/v1/events?after=%d&wait=%d", f.after, wait), nil, &a); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if a.Truncated {
		f.after = nextAfter(f.after, a)
	}
	job := func(t *time.Time) bool { return t != nil && t.Equal(f.started.Started) }
	for _, e := range a.Events {
		f.after = e.Seq
		switch {
		case e.Kind == evJobEnded && job(e.Job):
			f.ended = &e
		case e.Kind == evJobLost && job(e.Job):
			f.lost = &e
		}
	}
	return nil
}

// readOn reads the journal on from f.after while each read moves it, until the job's end or loss
// is read: a journal longer than one answer is read to its end. A read that fails stops it.
func (c *hostClient) readOn(ctx context.Context, f *jobEvents) {
	for !f.blind && f.lost == nil && f.ended == nil {
		before := f.after
		if c.await(ctx, f, 0) != nil || f.after == before {
			return
		}
	}
}

// followJob is follow, answering the job as it ended. The job is told by its start, which the
// 202 that started it carries. Between two reads of its log, the journal's long poll wakes the
// follow at the job's end, whose entry tells the end when reading the job again then fails in any
// way (the daemon stopping for the reboot the job scheduled), or when another job took its place;
// a job.lost read likewise answers the loss. A 202 without
// the journal's number is followed by the job's log alone, which the follow says. A context that
// ends is never the job's end (stopped). Every request is bounded (its wait and the client's patience).
func (c *hostClient) followJob(ctx context.Context, w io.Writer, started JobStarted) (JobStatus, error) {
	f := &jobEvents{started: started}
	if started.Events == nil {
		f.blind = true
		fmt.Fprintf(w, "the event journal does not hold this %s's start (%s): it is followed by its log alone\n",
			started.Job, cmp.Or(started.EventsError, "no number given"))
	} else {
		f.after = *started.Events
	}
	for {
		var j JobStatus
		if err := c.within(ctx, 0, http.MethodGet, "/v1/job?since="+strconv.Itoa(f.since), nil, &j); err != nil {
			if ctx.Err() != nil {
				return c.stopped(ctx, w, f, started.Job+" ended")
			}
			if !f.read() {
				return j, err
			}
			// The job's end or loss was journaled: it is the answer, whatever this request met —
			// the daemon stopping for the reboot the job scheduled, or failing as it stops.
			return c.known(w, f)
		}
		if j.Started == nil || !j.Started.Equal(started.Started) {
			return c.notThisJob(ctx, w, j, f)
		}
		c.logged(w, f, j)
		switch j.State {
		case "done":
			f.job = &j
			return c.doneAs(w, j, seenLive)
		case "failed":
			f.job = &j
			return j, fmt.Errorf("%s failed: %s", j.Kind, j.Error)
		case "running":
		default:
			return j, fmt.Errorf("the job is %s", j.State)
		}
		err := errors.New("no journal")
		if !f.blind {
			err = c.await(ctx, f, 2)
		}
		if err != nil {
			if ctx.Err() != nil {
				return c.stopped(ctx, w, f, started.Job+" ended")
			}
			select { // read the job again, which tells why
			case <-ctx.Done():
				return c.stopped(ctx, w, f, started.Job+" ended")
			case <-time.After(2 * c.second()):
			}
		}
	}
}

// endedAs is the job's answer from its end (f.final), saying its last log lines were not read
// when it is the journal's (unlogged): failed, its error; done, doneAs. A follow that read the
// end from GET job answered there, so an end reaching endedAs is the journal's.
func (c *hostClient) endedAs(w io.Writer, f *jobEvents) (JobStatus, error) {
	c.unlogged(w, f)
	j := *f.final()
	if j.State == "failed" {
		return j, fmt.Errorf("%s failed: %s", j.Kind, j.Error)
	}
	return c.doneAs(w, j, seenJournal)
}

// notThisJob answers a follow whose job the daemon no longer holds (j is what it holds): from
// the job's end when the journal holds it (the next job took the slot since), else an error —
// lost (the journal's job.lost names it: the daemon restarted), or another job in its place, or
// none.
func (c *hostClient) notThisJob(ctx context.Context, w io.Writer, j JobStatus, f *jobEvents) (JobStatus, error) {
	c.readOn(ctx, f)
	this := f.started.Job + " " + f.started.Target
	if f.read() {
		return c.known(w, f)
	}
	switch {
	case ctx.Err() != nil:
		return c.stopped(ctx, w, f, f.started.Job+" ended")
	case j.Started == nil:
		return j, fmt.Errorf("the daemon holds no job now, not this %s: `status` and `events` tell how it went", this)
	}
	return j, fmt.Errorf("the daemon's job is now %s %s, not this %s: `status` and `events` tell how it went", j.Kind, j.Target, this)
}
