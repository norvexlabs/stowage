package install

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/spf13/cobra"
)

// With IPv6 on, the VM's primary link takes its IPv6 default route from a router advertisement.
// networkd stops sending router solicitations at the first advertisement of any router, one with
// a router lifetime of 0 (a router that is not a default one) included: one solicited answer of the
// default router lost then leaves the link without a default route until that router's next
// unsolicited advertisement, which may be half an hour away. Stowage solicits again, with backoff,
// while the primary link has no IPv6 default route: the boot waits a bounded time for one before
// Docker starts the services (route6Boot), and the host daemon keeps soliciting past it, warning
// in `status` and the event journal while none comes (route6Watch). Each solicitation reaches
// networkd's own listener through the router's answer, so the route is networkd's as ever.

const (
	// route6Bound is how long the daemon finds no IPv6 default route, in Unit, before it warns,
	// and the boot's wait for one, in seconds (route6BootCommand).
	route6Bound = 60
	// route6MaxBackoff caps the boot's wait between two solicitations, in Unit; route6WatchBackoff
	// the daemon's (RFC 7559's MAX_RTR_SOLICITATION_INTERVAL: a link without an IPv6 router is
	// solicited hourly at most); route6Every is the daemon's check while the route is there, or
	// IPv6 is off.
	route6MaxBackoff   = 60
	route6WatchBackoff = 3600
	route6Every        = 30
)

// route6BootCmd waits at boot for the IPv6 default route, before Docker starts.
const route6BootCmd = "route6-boot"

// primaryIPv6 reports whether the primary link runs IPv6: its override's setting, else the
// network `vm create` gave (an override that does not read is not applied: the boot renders the
// cloud-config's).
func (h *Hostd) primaryIPv6() bool {
	f := h.netFiles()
	if st, err := f.read(); err == nil && st.Config != nil {
		return st.Config.IPv6
	}
	c, _ := f.readInstall()
	return c != nil && c.IPv6
}

// route6 reads the primary link — the IPv4 default route's (a secondary NIC's aside), else the one
// netLive picks — and
// whether it has an IPv6 default route; an error when `ip` does not answer, which tells nothing of
// the route.
func (h *Hostd) route6(ctx context.Context) (iface string, ok bool, err error) {
	out, err := h.Host.Run(ctx, Cmd{Args: []string{"ip", "-j", "route", "show", "default"}})
	if err != nil {
		return "", false, fmt.Errorf("ip route: %w", err)
	}
	var rs []ipRoute
	if err := json.Unmarshal(out, &rs); err != nil {
		return "", false, fmt.Errorf("ip route: %w", err)
	}
	// A secondary NIC (named by its role) may hold a default route of its own lease: not the
	// primary's.
	roles, _ := secondaryNICs(h.R.Spec)
	for _, r := range rs {
		if r.Dev != "" && (r.Dst == "default" || r.Dst == "") && !slices.Contains(roles, r.Dev) {
			iface = r.Dev
			break
		}
	}
	if iface == "" {
		live, err := h.netLive(ctx)
		if err != nil {
			return "", false, err
		}
		if iface = live.Interface; iface == "" {
			return "", false, nil
		}
	}
	out, err = h.Host.Run(ctx, Cmd{Args: []string{"ip", "-6", "-j", "route", "show", "default"}})
	if err != nil {
		return iface, false, fmt.Errorf("ip -6 route: %w", err)
	}
	var rs6 []ipRoute
	if err := json.Unmarshal(out, &rs6); err != nil {
		return iface, false, fmt.Errorf("ip -6 route: %w", err)
	}
	for _, r := range rs6 {
		for _, n := range append([]ipHop{r.ipHop}, r.Nexthops...) {
			if n.Gateway != "" && n.Dev == iface {
				return iface, true, nil
			}
		}
	}
	return iface, false, nil
}

// solicit sends a router solicitation on iface (Solicit in tests).
func (h *Hostd) solicit(iface string) error {
	if h.Solicit != nil {
		return h.Solicit(iface)
	}
	return sendRouterSolicitation(iface)
}

// route6Boot waits, up to bound, for the primary link's IPv6 default route when IPv6 is on,
// soliciting the routers with backoff (1 Unit, doubling to route6MaxBackoff) until one answers.
// Past the bound it warns and returns: the boot goes on, and the daemon keeps soliciting. A read
// of the routes that fails, or a solicitation, is logged once by its error.
func (h *Hostd) route6Boot(ctx context.Context, bound time.Duration, log io.Writer) {
	if !h.primaryIPv6() {
		return
	}
	deadline := time.Now().Add(bound)
	wait, sent, logged := h.unit(), 0, map[string]bool{}
	var readErr error // the last read of the routes, when it failed
	once := func(format string, a ...any) {
		if msg := fmt.Sprintf(format, a...); !logged[msg] {
			logged[msg] = true
			fmt.Fprintln(log, msg)
		}
	}
	for {
		iface, ok, err := h.route6(ctx)
		readErr = err
		switch {
		case err != nil:
			once("the routes do not read: %v", err)
		case ok:
			if sent > 0 {
				fmt.Fprintf(log, "the IPv6 default route on %s came after %d router solicitations\n", iface, sent)
			}
			return
		case iface != "":
			if err := h.solicit(iface); err != nil {
				once("a router solicitation on %s failed: %v", iface, err)
			} else {
				sent++
			}
		}
		left := time.Until(deadline)
		if left <= 0 && readErr != nil && sent == 0 {
			fmt.Fprintf(log, "warning: the routes did not read for %s (%v): whether the IPv6 default route is there is not known, and no router was solicited; the services start, and the host daemon keeps checking\n", bound, readErr)
			return
		}
		if left <= 0 {
			fmt.Fprintf(log, "warning: no IPv6 default route on %s after %s: the services start without one; the host daemon keeps soliciting the routers (`%s status` warns while none comes)\n", cmpIface(iface), bound, h.R.Ctl())
			return
		}
		t := time.NewTimer(min(wait, left))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		wait = min(2*wait, route6MaxBackoff*h.unit())
	}
}

// cmpIface names the primary link in a message, or says none was found.
func cmpIface(iface string) string {
	if iface == "" {
		return "the primary link (none found)"
	}
	return iface
}

// route6Watch keeps the primary link's IPv6 default route soliciting while IPv6 is on: while none
// is there, a router solicitation with backoff (1 Unit, doubling to route6WatchBackoff), the
// route checked at each one and every route6Every between them; past
// route6Bound without one, a warning in `status` (route6Warning) and one network.no_ipv6_route
// event for the stretch; once one is there, the warning clears and a check every route6Every.
// IPv6 turned off ends a stretch, its warning cleared; a read of the routes that fails is logged
// once by its error and neither starts nor ends one.
func (h *Hostd) route6Watch(ctx context.Context) {
	// since: the stretch's start; nextRS: when the next solicitation is due, its backoff wait.
	var since, nextRS time.Time
	wait, warned, logged := h.unit(), false, map[string]bool{}
	once := func(format string, a ...any) {
		if msg := fmt.Sprintf(format, a...); !logged[msg] {
			if len(logged) >= 32 { // errors that keep changing: logged again after 32
				clear(logged)
			}
			logged[msg] = true
			h.O.logf("%s", msg)
		}
	}
	end := func() {
		since, nextRS, wait, warned, logged = time.Time{}, time.Time{}, h.unit(), false, map[string]bool{}
		h.setRoute6Warning("")
	}
	for {
		next := route6Every * h.unit()
		if !h.primaryIPv6() {
			if warned {
				h.O.logf("IPv6 is off on the primary link: its default route is no longer watched")
			}
			if !since.IsZero() || warned {
				end()
			}
		} else if iface, ok, err := h.route6(ctx); err != nil {
			once("the IPv6 default route does not read: %v", err)
		} else if ok {
			if warned {
				h.O.logf("the IPv6 default route on %s is back", iface)
			}
			end()
		} else {
			if since.IsZero() {
				since = time.Now()
			}
			if iface != "" && !time.Now().Before(nextRS) {
				if err := h.solicit(iface); err != nil {
					once("a router solicitation on %s failed: %v", iface, err)
				}
				nextRS = time.Now().Add(wait)
				wait = min(2*wait, route6WatchBackoff*h.unit())
			}
			if !warned && time.Since(since) >= route6Bound*h.unit() {
				warned = true
				msg := fmt.Sprintf("no IPv6 default route on %s since %s: the services' IPv6 traffic beyond the link has no route; router solicitations are sent again until a router answers (`ip -6 route show default` shows the route)", cmpIface(iface), since.UTC().Format(time.RFC3339))
				h.setRoute6Warning(msg)
				h.O.logf("warning: %s", msg)
				if _, err := h.event(Event{Kind: evNoRoute6, Reason: msg}); err != nil {
					h.O.logf("the missing IPv6 default route could not be journaled: %v", err)
				}
			}
			// Checked every route6Every at most, so a route that comes back clears the warning
			// then, whatever the solicitations' backoff has grown to.
			next = route6Next(next, nextRS, time.Now(), h.unit())
		}
		t := time.NewTimer(next)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// route6Next is the wait before the watch's next check while the route is missing: the check's
// own period every, or less when the next solicitation (due at nextRS; zero: none planned) comes
// sooner — never less than unit.
func route6Next(every time.Duration, nextRS, now time.Time, unit time.Duration) time.Duration {
	if nextRS.IsZero() {
		return every
	}
	return max(min(every, nextRS.Sub(now)), unit)
}

// setRoute6Warning sets the warning `status` gives of the missing IPv6 default route ("": none).
func (h *Hostd) setRoute6Warning(msg string) {
	h.route6Mu.Lock()
	defer h.route6Mu.Unlock()
	h.route6Warn = msg
}

// route6Warning is the warning setRoute6Warning set.
func (h *Hostd) route6Warning() string {
	h.route6Mu.Lock()
	defer h.route6Mu.Unlock()
	return h.route6Warn
}

// route6BootCommand is the hidden verb a boot unit runs as root after the network is online and
// before Docker starts: route6Boot over the install of r's.
func route6BootCommand(r *Release, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:    route6BootCmd,
		Short:  "Wait, bounded, for the IPv6 default route, soliciting the routers, before Docker starts (run at boot)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h := &Hostd{R: r, O: Options{Dir: VMDir(r.Spec.Name), Log: stderr}, Host: HostTool{}}
			h.route6Boot(cmd.Context(), route6Bound*time.Second, stderr)
			return nil
		},
	}
}
