package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
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
		Short: "Back the data up (the database's dump and the volumes the backup lists, or those volumes alone), encrypted, to an rclone destination or kept on the host: on a schedule, before an upgrade, or now; or stream one out (run --output, export)",
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
	var retention, keepLocal int
	var remove, lock, unlock bool
	config := &cobra.Command{
		Use:   "config",
		Short: "Show the backup settings, or change them (the flags given; the others keep their value)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			f := cmd.Flags()
			var s BackupSettings
			changed := false
			if lock && unlock {
				return fmt.Errorf("--lock or --unlock, not both")
			}
			for _, n := range []string{"destination", "rclone-config", "recipient", "retention-days", "schedule", "profile", "keep-local", "lock", "unlock"} {
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
				if f.Changed("keep-local") {
					body.KeepLocal = &keepLocal
				}
				if lock || unlock {
					body.Lock = &lock
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
	cf.IntVar(&keepLocal, "keep-local", 0, fmt.Sprintf("keep the backups on the host rather than send them to a destination: the N latest of the profile (1 to %d; drops the destination and its rclone config; a --destination given drops it)", maxKeepLocal))
	cf.BoolVar(&remove, "remove", false, "drop the settings: no backup runs")
	cf.BoolVar(&lock, "lock", false, "the app socket may no longer change the settings (the operator still does)")
	cf.BoolVar(&unlock, "unlock", false, "the app socket may change the settings again")
	jsonFlag(config)

	var run BackupRequest
	var output string
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Back the data up now, and wait for it; with --output, stream it out (to FILE, or - for standard output) rather than store it, with no destination needed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			ctx := cmd.Context()
			f := cmd.Flags()
			if output == "" && (f.Changed("recipient") || asJSON) {
				return Refused{Reason: "--recipient and --json go with --output"}
			}
			req := run
			req.Output = output != ""
			// The file is made before the backup is taken: one that cannot be written fails now,
			// not once the backup ran.
			var part *os.File
			if output != "" && output != "-" {
				if part, err = createPartial(output); err != nil {
					return err
				}
				// This run's own: removed unless renamed whole.
				defer func() {
					part.Close()
					if err != nil {
						os.Remove(part.Name())
					}
				}()
			}
			var started struct {
				JobStarted
				Skipped string
			}
			if err = c.do(ctx, http.MethodPost, "/v1/backup", req, &started); err != nil {
				return err
			}
			if started.Skipped != "" {
				fmt.Fprintln(stdout, started.Skipped+": nothing to do")
				return nil
			}
			if !req.Output {
				return c.follow(ctx, stderr, started.JobStarted)
			}
			// The job's log goes to standard error: standard output may be the archive's alone.
			j, err := c.followJob(ctx, stderr, started.JobStarted)
			if err != nil {
				return err
			}
			return streamedOutErr(c.export(ctx, j.Result, true, output, part, stdout, stderr, asJSON), c.ctl, j.Result)
		},
	}
	rf := runCmd.Flags()
	rf.StringVar(&run.Profile, "profile", "", "the profile to take (default: the settings', else the release's default)")
	rf.BoolVar(&run.IfConfigured, "if-configured", false, "without backup settings, do nothing rather than fail (the timer's run)")
	rf.StringVar(&output, "output", "", "stream the backup out as one archive, to this file or - for standard output, rather than to the destination or the host's directory; no settings needed, and none kept")
	rf.StringArrayVar(&run.Recipients, "recipient", nil, "with --output: an age public key (age1…) to encrypt the backup to (repeatable; default: the settings' recipients)")
	rf.BoolVar(&asJSON, "json", false, "with --output: print, on standard error, one JSON line naming the backup: its object, the archive's sha256 and bytes, and its release")

	var exportTo string
	export := &cobra.Command{
		Use:   "export OBJECT",
		Short: "Stream a backup kept on the host (`backup list` marks them local), or the one `backup run --output` left there, out as one archive, to a file or standard output, which `restore --input` takes back",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			var part *os.File
			if exportTo != "-" {
				if part, err = createPartial(exportTo); err != nil {
					return err
				}
				defer func() {
					part.Close()
					if err != nil {
						os.Remove(part.Name())
					}
				}()
			}
			return c.export(cmd.Context(), args[0], false, exportTo, part, stdout, stderr, asJSON)
		},
	}
	export.Flags().StringVarP(&exportTo, "output", "o", "", "the file to write the archive to, or - for standard output")
	_ = export.MarkFlagRequired("output")
	export.Flags().BoolVar(&asJSON, "json", false, "print, on standard error, one JSON line naming the backup: its object, the archive's sha256 and bytes, and its release")

	list := &cobra.Command{
		Use:   "list",
		Short: "The backups of " + name + " in the destination and those kept on the host (marked local)",
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
				fmt.Fprintln(stdout, "no backup in the destination or on the host")
				return nil
			}
			tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSIZE\tTIME")
			for _, o := range l {
				fmt.Fprintf(tw, "%s\t%s\t%s", o.Name, size(uint64(o.Size)), o.Time.UTC().Format(time.RFC3339))
				var notes []string
				if o.Local {
					notes = append(notes, "local: kept on the host (backup export)")
				}
				if o.DroppedProfile {
					notes = append(notes, "a profile the release dropped: never pruned")
				}
				if len(notes) > 0 {
					fmt.Fprint(tw, "\t"+strings.Join(notes, "; "))
				}
				fmt.Fprintln(tw)
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
	root.AddCommand(status, config, runCmd, list, verify, export)
	return root
}

func printBackupSettings(w io.Writer, ctl string, s BackupSettings) {
	if !s.Configured {
		fmt.Fprintf(w, "no backup is configured: `%s backup config --destination remote:path --rclone-config FILE --recipient age1…` sets one (or --keep-local N for none)\n", ctl)
		fmt.Fprintf(w, "profiles     %s (default %s)\n", strings.Join(s.Profiles, ", "), s.Profile)
		return
	}
	rc := "not set"
	switch {
	case s.RcloneConfigSet && s.RcloneConfigFrom == rcloneUnknown:
		rc = fmt.Sprintf("set (not shown), not known to be the operator's: not run until saved over SSH, `%s backup config --rclone-config FILE`", ctl)
	case s.RcloneConfigSet:
		rc = "set (not shown)"
	}
	keep := fmt.Sprintf("%d days", s.RetentionDays)
	if s.RetentionDays == 0 {
		keep = "every backup"
	}
	if s.KeepLocal > 0 {
		fmt.Fprintf(w, "kept         on the host: the %d latest of the profile (`%s backup export` streams one out)\n", s.KeepLocal, ctl)
	} else {
		fmt.Fprintf(w, "destination  %s\n", s.Destination)
		fmt.Fprintf(w, "rclone       %s\n", rc)
	}
	fmt.Fprintf(w, "recipients   %s\n", strings.Join(s.Recipients, ", "))
	if s.KeepLocal == 0 {
		fmt.Fprintf(w, "retention    %s\n", keep)
	}
	fmt.Fprintf(w, "schedule     %s\n", s.Schedule)
	fmt.Fprintf(w, "profile      %s (of %s)\n", s.Profile, strings.Join(s.Profiles, ", "))
	if s.Locked {
		fmt.Fprintln(w, "locked       the app socket may not change these (--unlock)")
	}
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

// ExportedBackup is the JSON line `backup run --output --json` and `backup export --json` print on
// standard error: the backup, and the archive streamed out (its SHA-256 and size, as written).
type ExportedBackup struct {
	Object  string  `json:"object"`
	SHA256  string  `json:"sha256"`
	Bytes   int64   `json:"bytes"`
	Release Version `json:"release"`
}

// createPartial makes path.partial (0600) for an archive streamed out, refusing one that exists:
// another run writes it, or one cut off left it.
func createPartial(path string) (*os.File, error) {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		return nil, Refused{Reason: fmt.Sprintf("--output: %s is a directory: name a file", path)}
	}
	f, err := os.OpenFile(path+partialSuffix, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil, Refused{Reason: fmt.Sprintf("--output: %s exists: another run writes it, or one cut off left it; remove it, or write elsewhere", path+partialSuffix)}
	}
	if err != nil {
		return nil, fmt.Errorf("--output: %w", err)
	}
	return f, nil
}

// streamedOutErr is what `backup run --output` says of its stream's error err: cut short, the
// backup is still on the host, and export streams it again; refused (gone), it is not.
func streamedOutErr(err error, ctl, obj string) error {
	if cut := (streamCut{}); errors.As(err, &cut) {
		return fmt.Errorf("%w; the backup stays on the host until the next backup run --output, or the host daemon's restart: `%s backup export %s -o FILE` streams it out again", err, ctl, obj)
	}
	return err
}

// streamCut is an archive not written whole (its stream stopped before its end, or its file could
// not be written): the backup is still on the host.
type streamCut struct{ error }

func (s streamCut) Unwrap() error { return s.error }

// export streams the backup obj held on the host (output: the one just taken to be streamed
// out) to part, made by createPartial and renamed to path once whole, or to stdout for "-"; then
// names it on stderr. No file half written is taken for an archive.
func (c *hostClient) export(ctx context.Context, obj string, output bool, path string, part *os.File, stdout, stderr io.Writer, asJSON bool) error {
	q := url.Values{"object": {obj}}
	if output {
		q.Set("output", "1")
	}
	resp, err := c.send(ctx, http.MethodGet, "/v1/backup/export?"+q.Encode(), "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sum := sha256.New()
	w := stdout
	if part != nil {
		w = part
	}
	n, err := io.Copy(io.MultiWriter(w, sum), resp.Body)
	if err != nil {
		return streamCut{fmt.Errorf("streaming %s out stopped after %d bytes: %w", obj, n, err)}
	}
	if part != nil {
		err = part.Sync()
		if err == nil {
			err = os.Rename(part.Name(), path)
		}
		if err != nil {
			// Not written: the backup is still on the host, as for a stream cut short.
			return streamCut{fmt.Errorf("writing %s: %w", path, err)}
		}
	}
	e := ExportedBackup{Object: obj, SHA256: hex.EncodeToString(sum.Sum(nil)), Bytes: n}
	e.Release, _ = backupRelease(obj)
	if asJSON {
		return json.NewEncoder(stderr).Encode(e)
	}
	where := path
	if path == "-" {
		where = "standard output"
	}
	fmt.Fprintf(stderr, "%s: %s streamed to %s (%d bytes, sha256 %s)\n", c.ctl, obj, where, n, e.SHA256)
	return nil
}

// backupRelease is the release a backup's name says it was taken from: its name ends in
// -<version>-r<revision> and its suffix.
func backupRelease(obj string) (Version, bool) {
	base, ok := backupBase(obj)
	if !ok {
		return Version{}, false
	}
	i := strings.LastIndex(base, "-r")
	if i < 0 {
		return Version{}, false
	}
	rev, err := strconv.Atoi(base[i+2:])
	if err != nil {
		return Version{}, false
	}
	// <project>-<profile>-<time>-<version>: the version follows the time, which has no '-'.
	head := base[:i]
	t := ownBackupTime.FindStringIndex(head)
	if t == nil {
		return Version{}, false
	}
	return Version{Version: head[t[1]:], Revision: rev}, true
}

// ownBackupTime is a backup's time in its name, with the dashes around it.
var ownBackupTime = regexp.MustCompile(`-\d{8}T\d{6}Z-`)
