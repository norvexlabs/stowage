package install

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// backupCommand is `backup` and its verbs, which ask the host daemon.
func backupCommand(c *hostClient, name string, stdout, stderr io.Writer) *cobra.Command {
	ctl := c.ctl
	var asJSON bool
	jsonFlag := func(cmd *cobra.Command) {
		cmd.Flags().BoolVar(&asJSON, "json", false, "print the daemon's JSON answer")
	}
	root := &cobra.Command{
		Use:   "backup",
		Short: "Back the database up, encrypted, to an rclone destination: on a schedule, before an upgrade, or now",
		Args:  cobra.NoArgs,
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "The backup settings, the next scheduled backup and the last one",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var st BackupStatus
			if err := c.do(cmd.Context(), http.MethodGet, "/v1/backup/status", nil, &st); err != nil {
				return err
			}
			if asJSON {
				return printJSON(stdout, st)
			}
			printBackupSettings(stdout, ctl, st.Settings)
			if st.Settings.Configured {
				fmt.Fprintf(stdout, "next         %s\n", orNone(st.Next))
			}
			switch l := st.Last; {
			case st.Running:
				fmt.Fprintln(stdout, "last         a backup is running")
			case l == nil:
				fmt.Fprintln(stdout, "last         none")
			case l.Error != "":
				fmt.Fprintf(stdout, "last         FAILED %s: %s\n", l.Finished.Format(time.RFC3339), l.Error)
			default:
				fmt.Fprintf(stdout, "last         %s, %s (%s, %s)\n", l.Finished.Format(time.RFC3339), l.Object, size(uint64(l.Bytes)), l.Finished.Sub(l.Started).Round(time.Second))
				if l.PruneError != "" {
					fmt.Fprintf(stdout, "             the older backups were not pruned: %s\n", l.PruneError)
				}
			}
			return nil
		},
	}
	jsonFlag(status)

	var req BackupConfigRequest
	var dest, rcloneFile, schedule, profile string
	var recipients []string
	var retention int
	var remove bool
	config := &cobra.Command{
		Use:   "config",
		Short: "Show the backup settings, or change them (the flags given; the others keep their value)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			f := cmd.Flags()
			var s BackupSettings
			changed := false
			for _, n := range []string{"destination", "rclone-config", "recipient", "retention-days", "schedule", "profile"} {
				changed = changed || f.Changed(n)
			}
			switch {
			case remove && changed:
				return fmt.Errorf("--remove drops the settings: no other flag with it")
			case remove:
				if err := c.do(ctx, http.MethodDelete, "/v1/backup/config", nil, &s); err != nil {
					return err
				}
				fmt.Fprintln(stdout, "the backup settings are removed: no backup runs")
				return nil
			case !changed:
				if err := c.do(ctx, http.MethodGet, "/v1/backup/config", nil, &s); err != nil {
					return err
				}
			default:
				body := req
				if f.Changed("destination") {
					body.Destination = &dest
				}
				if f.Changed("rclone-config") {
					raw, err := readFileOrStdin(rcloneFile, cmd.InOrStdin())
					if err != nil {
						return fmt.Errorf("--rclone-config: %w", err)
					}
					if len(raw) > maxRcloneConfig {
						return fmt.Errorf("--rclone-config: more than %d bytes", maxRcloneConfig)
					}
					conf := string(raw)
					body.RcloneConfig = &conf
				}
				if f.Changed("recipient") {
					body.Recipients = recipients
				}
				if f.Changed("retention-days") {
					body.RetentionDays = &retention
				}
				if f.Changed("schedule") {
					body.Schedule = &schedule
				}
				if f.Changed("profile") {
					body.Profile = &profile
				}
				if err := c.do(ctx, http.MethodPost, "/v1/backup/config", body, &s); err != nil {
					return err
				}
			}
			if asJSON {
				return printJSON(stdout, s)
			}
			printBackupSettings(stdout, ctl, s)
			return nil
		},
	}
	cf := config.Flags()
	cf.StringVar(&dest, "destination", "", "where backups go: a remote of the rclone config and a path, remote:path")
	cf.StringVar(&rcloneFile, "rclone-config", "", "the rclone config defining the destination's remote and credentials: a file, or - for standard input")
	cf.StringArrayVar(&recipients, "recipient", nil, "an age public key (age1…) backups are encrypted to (repeatable; replaces the list)")
	cf.IntVar(&retention, "retention-days", defaultRetentionDays, fmt.Sprintf("delete a profile's backups older than this after each backup; 0 keeps them all (at most %d)", maxRetentionDays))
	cf.StringVar(&schedule, "schedule", defaultBackupSchedule, "when the scheduled backup runs: a systemd calendar event")
	cf.StringVar(&profile, "profile", "", "the profile a scheduled backup takes (default: the release's default profile)")
	cf.BoolVar(&remove, "remove", false, "drop the settings: no backup runs")
	jsonFlag(config)

	var run BackupRequest
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Back the database up now, and wait for it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			var started struct {
				JobStarted
				Skipped string
			}
			if err := c.do(ctx, http.MethodPost, "/v1/backup", run, &started); err != nil {
				return err
			}
			if started.Skipped != "" {
				fmt.Fprintln(stdout, started.Skipped+": nothing to do")
				return nil
			}
			return c.follow(ctx, stderr, started.JobStarted)
		},
	}
	runCmd.Flags().StringVar(&run.Profile, "profile", "", "the profile to take (default: the settings')")
	runCmd.Flags().BoolVar(&run.IfConfigured, "if-configured", false, "without backup settings, do nothing rather than fail (the timer's run)")

	list := &cobra.Command{
		Use:   "list",
		Short: "The backups of " + name + " in the destination",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var l []BackupObject
			if err := c.do(cmd.Context(), http.MethodGet, "/v1/backup/list", nil, &l); err != nil {
				return err
			}
			if asJSON {
				return printJSON(stdout, l)
			}
			if len(l) == 0 {
				fmt.Fprintln(stdout, "no backup in the destination")
				return nil
			}
			tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSIZE\tTIME")
			for _, o := range l {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", o.Name, size(uint64(o.Size)), o.Time.UTC().Format(time.RFC3339))
			}
			return tw.Flush()
		},
	}
	jsonFlag(list)

	var vDest, vRclone string
	var vRecipients []string
	verify := &cobra.Command{
		Use:   "verify",
		Short: "Check the settings reach the destination: write a probe file there; the flags given try those settings over the saved ones, saving nothing",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f := cmd.Flags()
			var body any // none: the saved settings
			if f.Changed("destination") || f.Changed("rclone-config") || f.Changed("recipient") {
				try := &BackupConfigRequest{Recipients: vRecipients}
				body = try
				if f.Changed("destination") {
					try.Destination = &vDest
				}
				if f.Changed("rclone-config") {
					raw, err := readFileOrStdin(vRclone, cmd.InOrStdin())
					if err != nil {
						return fmt.Errorf("--rclone-config: %w", err)
					}
					if len(raw) > maxRcloneConfig {
						return fmt.Errorf("--rclone-config: more than %d bytes", maxRcloneConfig)
					}
					conf := string(raw)
					try.RcloneConfig = &conf
				}
			}
			var started JobStarted
			if err := c.do(cmd.Context(), http.MethodPost, "/v1/backup/verify", body, &started); err != nil {
				return err
			}
			j, err := c.followJob(cmd.Context(), stderr, started)
			if err != nil {
				return err
			}
			fmt.Fprintln(stdout, j.Result)
			return nil
		},
	}
	vf := verify.Flags()
	vf.StringVar(&vDest, "destination", "", "try this destination, remote:path")
	vf.StringVar(&vRclone, "rclone-config", "", "try this rclone config: a file, or - for standard input")
	vf.StringArrayVar(&vRecipients, "recipient", nil, "try these age public keys (repeatable)")
	root.AddCommand(status, config, runCmd, list, verify)
	return root
}

func printBackupSettings(w io.Writer, ctl string, s BackupSettings) {
	if !s.Configured {
		fmt.Fprintf(w, "no backup is configured: `%s backup config --destination remote:path --rclone-config FILE --recipient age1…` sets one\n", ctl)
		fmt.Fprintf(w, "profiles     %s (default %s)\n", strings.Join(s.Profiles, ", "), s.Profile)
		return
	}
	rc := "not set"
	if s.RcloneConfigSet {
		rc = "set (not shown)"
	}
	keep := fmt.Sprintf("%d days", s.RetentionDays)
	if s.RetentionDays == 0 {
		keep = "every backup"
	}
	fmt.Fprintf(w, "destination  %s\n", s.Destination)
	fmt.Fprintf(w, "rclone       %s\n", rc)
	fmt.Fprintf(w, "recipients   %s\n", strings.Join(s.Recipients, ", "))
	fmt.Fprintf(w, "retention    %s\n", keep)
	fmt.Fprintf(w, "schedule     %s\n", s.Schedule)
	fmt.Fprintf(w, "profile      %s (of %s)\n", s.Profile, strings.Join(s.Profiles, ", "))
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// readFileOrStdin reads the file path, or stdin when path is "-": at most maxRcloneConfig+1
// bytes.
func readFileOrStdin(path string, stdin io.Reader) ([]byte, error) {
	r := stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	return io.ReadAll(io.LimitReader(r, maxRcloneConfig+1))
}
