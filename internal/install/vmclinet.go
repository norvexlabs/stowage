package install

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

// netBootCmd renders the network override at boot, before networkd starts.
const netBootCmd = "network-boot"

// netBootCommand is the hidden verb the boot runs as root: NetBoot over the install in *dir.
func netBootCommand(name, ctl string, dir *string, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    netBootCmd,
		Short:  "Render the network override for networkd, a change never confirmed undone first (run at boot)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return NetBoot(name, ctl, *dir, stderr)
		},
	}
	return cmd
}

// networkCommand is `network` and its verbs, which ask the host daemon.
func networkCommand(c *hostClient, name string, stdout io.Writer) *cobra.Command {
	ctl := c.ctl
	var asJSON bool
	show := &cobra.Command{
		Use:   "network",
		Short: "The VM's address, gateway and DNS servers, what sets them, and a change awaiting confirmation",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var n Network
			if err := c.do(cmd.Context(), http.MethodGet, "/v1/network", nil, &n); err != nil {
				return err
			}
			if asJSON {
				return printJSON(stdout, n)
			}
			printNetwork(stdout, ctl, n)
			return nil
		},
	}
	show.Flags().BoolVar(&asJSON, "json", false, "print the daemon's JSON answer")

	var req NetRequest
	var revert int
	var noRevert bool
	// window is the revert_secs a change sends: the default unless --revert-secs or --no-revert.
	window := func(cmd *cobra.Command) (*int, error) {
		if noRevert {
			if cmd.Flags().Changed("revert-secs") {
				return nil, fmt.Errorf("--revert-secs and --no-revert: one or the other")
			}
			zero := 0
			return &zero, nil
		}
		if cmd.Flags().Changed("revert-secs") {
			return &revert, nil
		}
		return nil, nil
	}
	windowFlags := func(cmd *cobra.Command) {
		cmd.Flags().IntVar(&revert, "revert-secs", defaultRevertSecs, fmt.Sprintf("seconds to confirm an addressing change in before it is undone (%d to %d)", minRevertSecs, maxRevertSecs))
		cmd.Flags().BoolVar(&noRevert, "no-revert", false, "apply an addressing change for good, with no window to confirm it in")
	}
	answer := func(ctx context.Context, path string, body any) error {
		var a NetAnswer
		if err := c.do(ctx, http.MethodPost, path, body, &a); err != nil {
			return err
		}
		fmt.Fprintln(stdout, a.Message)
		return nil
	}
	set := &cobra.Command{
		Use:   "set",
		Short: "Set the VM's network, over the cloud-config's: an addressing change comes undone unless `network confirm` keeps it in time",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, err := window(cmd)
			if err != nil {
				return err
			}
			body := req
			body.RevertSecs = w
			return answer(cmd.Context(), "/v1/network", body)
		},
	}
	sf := set.Flags()
	sf.StringVar(&req.Mode, "mode", "", "dhcp or static")
	sf.StringVar(&req.Address, "address", "", "static: the address with its prefix (192.0.2.10/24)")
	sf.StringVar(&req.Gateway, "gateway", "", "static: the default gateway, in the address's subnet")
	sf.BoolVar(&req.IPv6, "ipv6", false, "turn IPv6 on: router advertisements (and DHCPv6 with dhcp), and --address6 (default: no IPv6 on the link)")
	sf.StringVar(&req.Address6, "address6", "", "with --ipv6: a static IPv6 address with its prefix (2001:db8::10/64), beside the advertised ones")
	sf.StringVar(&req.Gateway6, "gateway6", "", "with --address6: the IPv6 default gateway, link-local or in its subnet (default: the router advertisements')")
	sf.StringSliceVar(&req.DNS, "dns", nil, fmt.Sprintf("DNS servers, comma-separated (at most %d); with dhcp, in place of the lease's", maxDNS))
	_ = set.MarkFlagRequired("mode")
	windowFlags(set)
	confirm := &cobra.Command{
		Use:   "confirm",
		Short: "Keep the network change awaiting confirmation",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return answer(cmd.Context(), "/v1/network/confirm", nil)
		},
	}
	revertCmd := &cobra.Command{
		Use:   "revert",
		Short: "Undo the network change awaiting confirmation now",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return answer(cmd.Context(), "/v1/network/revert", nil)
		},
	}
	reset := &cobra.Command{
		Use:   "reset",
		Short: "Drop the override: back to the cloud-config's network, which `network confirm` keeps in time, like any addressing change",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, err := window(cmd)
			if err != nil {
				return err
			}
			return answer(cmd.Context(), "/v1/network/reset", NetResetRequest{RevertSecs: w})
		},
	}
	windowFlags(reset)
	show.AddCommand(set, confirm, revertCmd, reset)
	return show
}

func printNetwork(w io.Writer, ctl string, n Network) {
	l := n.Live
	fmt.Fprintf(w, "interface  %s %s\n", l.Interface, l.MAC)
	fmt.Fprintf(w, "address    %s\n", strings.Join(l.Addresses, ", "))
	gws := l.Gateways6
	if l.Gateway != "" {
		gws = append([]string{l.Gateway}, gws...)
	}
	fmt.Fprintf(w, "gateway    %s\n", strings.Join(gws, ", "))
	fmt.Fprintf(w, "dns        %s\n", strings.Join(l.DNS, ", "))
	switch n.Source {
	case "override":
		fmt.Fprintf(w, "set by     `%s network set`: %s\n", ctl, describeNet(n.Override))
	case "install":
		if n.Install != nil {
			fmt.Fprintf(w, "set by     the VM's cloud-config, as `vm create` gave it: %s\n", describeNet(n.Install))
		} else {
			fmt.Fprintln(w, "set by     the VM's cloud-config: the network `vm create` gave")
		}
	default:
		fmt.Fprintln(w, "set by     DHCP")
	}
	if p := n.Pending; p != nil {
		fmt.Fprintf(w, "pending    not confirmed: in %d s, %s comes back unless `%s network confirm` keeps it\n", p.SecsLeft, describeNet(p.Previous), ctl)
	}
}

// describeNet names a network setting; nil is the cloud-config's.
func describeNet(c *NetConfig) string {
	if c == nil {
		return "the cloud-config's network"
	}
	s := "DHCP"
	if c.Mode == "static" {
		s = c.Address
		if c.Gateway != "" {
			s += " via " + c.Gateway
		}
	}
	switch {
	case c.Address6 != "":
		s += ", " + c.Address6
		if c.Gateway6 != "" {
			s += " via " + c.Gateway6
		}
	case c.IPv6:
		s += ", automatic IPv6"
	default:
		s += ", no IPv6"
	}
	if len(c.DNS) > 0 {
		s += ", DNS " + strings.Join(c.DNS, ", ")
	}
	return s
}
