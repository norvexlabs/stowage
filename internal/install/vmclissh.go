package install

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"

	"github.com/spf13/cobra"
)

// sshAllowCommand is `ssh-allow` and its verbs, which ask the host daemon: the sources SSH
// accepts, changed after the first boot with a window to confirm the change in from a new
// connection.
func sshAllowCommand(c *hostClient, stdout, stderr io.Writer) *cobra.Command {
	ctl := c.ctl
	var asJSON bool
	answer := func(ctx context.Context, method, path string, body any) error {
		var a SSHAllow
		if err := c.do(ctx, method, path, body, &a); err != nil {
			return err
		}
		if asJSON {
			return printJSON(stdout, a)
		}
		printSSHAllow(stdout, a)
		return nil
	}
	show := &cobra.Command{
		Use:   "ssh-allow",
		Short: "The sources SSH accepts, what set them, and a change awaiting confirmation",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return answer(cmd.Context(), http.MethodGet, "/v1/ssh-allow", nil)
		},
	}
	show.PersistentFlags().BoolVar(&asJSON, "json", false, "print the daemon's JSON answer")

	var revert int
	set := &cobra.Command{
		Use:   "set CIDR[,CIDR...]...",
		Short: "Set the sources SSH accepts, over the cloud-config's (`vm create --ssh-allow`), as root through the host daemon: the change comes undone unless `ssh-allow confirm`, run over a new SSH connection (`ssh -S none`), keeps it in time",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var sources []string
			for _, a := range args {
				sources = append(sources, strings.Split(a, ",")...)
			}
			req := SSHRequest{Sources: sources}
			if cmd.Flags().Changed("revert-secs") {
				req.RevertSecs = &revert
			}
			if err := answer(cmd.Context(), http.MethodPost, "/v1/ssh-allow", req); err != nil {
				return err
			}
			// Once the change is applied: a refused one shuts no one out.
			sshSourceWarning(stderr, c.getenv("SSH_CONNECTION"), sources, ctl)
			return nil
		},
	}
	set.Flags().IntVar(&revert, "revert-secs", defaultRevertSecs, fmt.Sprintf("seconds to confirm the change in before it is undone (%d to %d: every change of SSH's sources has a window)", minRevertSecs, maxRevertSecs))
	confirm := &cobra.Command{
		Use:   "confirm",
		Short: "Keep the change of SSH's sources awaiting confirmation: run it over a new SSH connection, not a shared one (`ssh -S none`), which proves the new sources let you in",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return answer(cmd.Context(), http.MethodPost, "/v1/ssh-allow/confirm", nil)
		},
	}
	revertCmd := &cobra.Command{
		Use:   "revert",
		Short: "Undo the change of SSH's sources awaiting confirmation now",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return answer(cmd.Context(), http.MethodPost, "/v1/ssh-allow/revert", nil)
		},
	}
	show.AddCommand(set, confirm, revertCmd)
	return show
}

// sshSourceWarning warns, on w, when the SSH session the command runs in (conn, SSH_CONNECTION:
// "client port server port") comes from an address none of sources holds: the session itself stays
// (the firewall accepts an established connection), but no new one from there is let in, so the
// change can be confirmed only from another source. Sources that do not parse are the daemon's to
// refuse.
func sshSourceWarning(w io.Writer, conn string, sources []string, ctl string) {
	f := strings.Fields(conn)
	if len(f) < 1 {
		return
	}
	a, err := netip.ParseAddr(f[0])
	if err != nil {
		return
	}
	// A link-local client carries its zone (fe80::5%eth0), which no prefix contains.
	a = a.Unmap().WithZone("")
	ps, err := parseSSHSources("", sources)
	if err != nil {
		return
	}
	for _, p := range ps {
		if p.Contains(a) {
			return
		}
	}
	fmt.Fprintf(w, "warning: this session comes from %s, which none of the new sources holds: no new SSH connection from there will be let in; confirm it from a source they hold, or let the window undo it (`%s ssh-allow revert` now)\n", a, ctl)
}

func printSSHAllow(w io.Writer, a SSHAllow) {
	if a.Message != "" {
		fmt.Fprintln(w, a.Message)
	}
	from := map[string]string{
		"set":          "ssh-allow set",
		"cloud-config": "the cloud-config (vm create --ssh-allow)",
		"image":        "the image",
		"unknown":      "unknown (a rule file edited by hand)",
	}[a.From]
	switch {
	case a.Sources == nil:
		fmt.Fprintf(w, "sources    unknown: %s\n", a.Rules)
	case len(a.Sources) == 0:
		fmt.Fprintf(w, "sources    none: %s\n", a.Rules)
	default:
		fmt.Fprintf(w, "sources    %s\n", strings.Join(a.Sources, ", "))
		if a.Rules != "" {
			fmt.Fprintf(w, "warning    %s\n", a.Rules)
		}
	}
	fmt.Fprintf(w, "set by     %s\n", from)
	if p := a.Pending; p != nil {
		back := strings.Join(p.Previous, ", ")
		switch {
		case p.Previous == nil:
			back = "the rule file before it (lines other than SSH's sources)"
		case len(p.Previous) == 0:
			back = "no source"
		}
		if p.Ended {
			fmt.Fprintf(w, "pending    %s: the change is being undone (or its undo is tried again); a confirmation comes too late (back to %s)\n", p.EndedBy, back)
		} else {
			fmt.Fprintf(w, "pending    awaits confirmation for %d s more, else back to %s\n", p.SecsLeft, back)
		}
	}
}
