package install

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/spec"
)

// FrontImageVar records the front's image reference in .env; a project without a front records
// none.
const FrontImageVar = "STOWAGE_FRONT_IMAGE"

// ChownImageVar records the volume helper's image reference in .env, for a release that carries
// one.
const ChownImageVar = "STOWAGE_CHOWN_IMAGE"

// FrontService is the compose service Stowage adds; projects may not use the name.
const FrontService = "stowage-front"

// Files the installer writes next to the project's compose file.
const (
	FrontCompose = "stowage-front.yaml"
	Caddyfile    = "stowage-front.Caddyfile"
)

// FrontConfigLabel carries the digest of the front's configuration.
const FrontConfigLabel = "io.stowage.front-config"

// HTTPPortVar and HTTPSPortVar record the host ports a TLS front publishes.
const (
	HTTPPortVar  = "STOWAGE_HTTP_PORT"
	HTTPSPortVar = "STOWAGE_HTTPS_PORT"
	// IPv6Var records --ipv6: "true" turns IPv6 on for the stack's default network.
	IPv6Var = "STOWAGE_IPV6"
	// IPv6Label is the default network's label recording the --ipv6 choice it was made with.
	IPv6Label = "io.stowage.ipv6"
)

// frontPort is the port the front listens on, inside its container, for route i in loopback
// mode. The host side comes from .env, so changing a port never changes these files.
func frontPort(i int) int { return 10000 + i }

// frontL4Port is the port the front's container listens on for the L4 route of index i among the
// spec's, when it terminates its TLS: a range of its own, apart from 80, 443 and the loopback
// routes' (frontPort), whatever host port publishes it.
func frontL4Port(i int) int { return 20000 + i }

// PortVar records a route's loopback port in .env.
func PortVar(route string) string {
	return "STOWAGE_PORT_" + strings.TrimPrefix(spec.URLVar(route), "STOWAGE_URL_")
}

// loopbackCaddyfile serves each route on its own plain-HTTP port, as routeBody says; get reads a
// recorded value. A route asking for a client certificate refuses everyone: plain HTTP carries
// none.
func loopbackCaddyfile(f spec.Front, l4 []spec.L4Route, get func(string) string, logins map[string]login) string {
	var b strings.Builder
	b.WriteString("# Written by the installer from the project's front routes; rewritten on every run.\n")
	b.WriteString("{\n\tadmin off\n\tauto_https off\n\tpersist_config off\n")
	layer4(&b, l4, get, func(spec.L4Route) string { return "" }, false, false)
	b.WriteString("}\n")
	for i, r := range f.Routes {
		fmt.Fprintf(&b, "\n# %s\nhttp://:%d {\n", r.Name, frontPort(i))
		serveRoutes(&b, []indexed{{i, r}}, func(b *strings.Builder, r spec.Route) {
			if r.ClientAuth {
				b.WriteString("\trespond 403\n")
			} else {
				routeBody(b, r, get, logins)
			}
		})
		b.WriteString("}\n")
	}
	return b.String()
}

// indexed is a route and its index among the front's routes.
type indexed struct {
	i int
	r spec.Route
}

// serveRoutes writes the routes one site serves, each as body writes it: a route without a path
// alone as it is; else each route with a path by its prefix (the path itself and what is under
// it), the longest first, then the one without (every other path), else a 404.
func serveRoutes(b *strings.Builder, routes []indexed, body func(*strings.Builder, spec.Route)) {
	if len(routes) == 1 && routes[0].r.Path == "" {
		body(b, routes[0].r)
		return
	}
	sorted := slices.Clone(routes)
	slices.SortStableFunc(sorted, func(x, y indexed) int { return len(y.r.Path) - len(x.r.Path) })
	catchAll := false
	for _, x := range sorted {
		var inner strings.Builder
		body(&inner, x.r)
		if x.r.Path == "" {
			catchAll = true
			b.WriteString("\thandle {\n")
		} else {
			fmt.Fprintf(b, "\t@stowage_path_%d path %s %s/*\n\thandle @stowage_path_%d {\n", x.i, x.r.Path, x.r.Path, x.i)
		}
		for l := range strings.Lines(inner.String()) {
			b.WriteString("\t" + l)
		}
		b.WriteString("\t}\n")
	}
	if !catchAll {
		b.WriteString("\thandle {\n\t\trespond 404\n\t}\n")
	}
}

// ClientCAFile is the install's client CA (--client-ca), PEM: the CAs a client certificate of a
// route with client_auth chains to. Its directory is what the front mounts (clientCADir), so a
// configuration naming a file since removed never has Docker make a directory in its place;
// clientCAPath is where the front reads it.
const (
	ClientCADir  = "stowage-front-client-ca"
	ClientCAFile = ClientCADir + "/ca.pem"
	clientCADir  = "/etc/caddy/client-ca"
	clientCAPath = clientCADir + "/ca.pem"
)

// clientAuth is a TLS route's client authentication, inside its tls directive: a certificate
// chaining to the client CA is required, and verified.
const clientAuth = "\t\tclient_auth {\n\t\t\tmode require_and_verify\n\t\t\ttrust_pool file " + clientCAPath + "\n\t\t}\n"

// routeBody is a route's handling, in this order: a source the allowlist does not hold is refused
// (403; everyone while the allowlist is empty), the client logs in (basic_auth, its login from
// logins; everyone refused while the route has none), the stripped headers — and the headers
// forward_auth copies, so a client never forges one, even while it is off, and the login's
// Authorization — are removed whatever the client sent, the authentication service is asked at
// its URI (/ when none is given; its answer's listed headers passed on; off while its address is
// empty), then the upstream serves.
func routeBody(b *strings.Builder, r spec.Route, get func(string) string, logins map[string]login) {
	auth, uri := "", "/"
	strip := r.StripHeaders
	if a := r.ForwardAuth; a != nil {
		auth = get(a.AddressInput)
		if a.URIInput != "" && get(a.URIInput) != "" {
			uri = get(a.URIInput)
		}
		strip = append(slices.Clone(strip), a.CopyHeaders...)
	}
	if r.BasicAuth != nil {
		strip = append(slices.Clone(strip), "Authorization")
	}
	if auth == "" && r.AllowInput == "" && r.BasicAuth == nil {
		for _, h := range strip {
			fmt.Fprintf(b, "\trequest_header -%s\n", h)
		}
		reverseProxy(b, "\t", r.Upstream)
		return
	}
	sources := splitSources(get(r.AllowInput))
	if len(sources) > 0 {
		fmt.Fprintf(b, "\t@stowage_denied not remote_ip %s\n", strings.Join(sources, " "))
	}
	b.WriteString("\troute {\n")
	switch {
	case len(sources) > 0:
		b.WriteString("\t\trespond @stowage_denied 403\n")
	case r.AllowInput != "":
		b.WriteString("\t\trespond 403\n")
	}
	if r.BasicAuth != nil {
		if l := logins[r.Name]; l.Hash != "" {
			fmt.Fprintf(b, "\t\tbasic_auth {\n\t\t\t%s %s\n\t\t}\n", l.User, l.Hash)
		} else {
			b.WriteString("\t\trespond 403\n")
		}
	}
	for _, h := range strip {
		fmt.Fprintf(b, "\t\trequest_header -%s\n", h)
	}
	if auth != "" {
		fmt.Fprintf(b, "\t\tforward_auth %s {\n\t\t\turi %s\n", auth, uri)
		if hs := r.ForwardAuth.CopyHeaders; len(hs) > 0 {
			fmt.Fprintf(b, "\t\t\tcopy_headers %s\n", strings.Join(hs, " "))
		}
		b.WriteString("\t\t}\n")
	}
	reverseProxy(b, "\t\t", r.Upstream)
	b.WriteString("\t}\n")
}

// proxyRetry is how long the front tries an upstream it cannot reach again, resolving its name at
// each try: a container that stops (a start-first overlap's old one, a restart) costs a request
// a wait, never an error, while the name answers for a container that serves.
const proxyRetry = "5s"

// reverseProxy writes the route's reverse_proxy to upstream, indented by indent.
func reverseProxy(b *strings.Builder, indent, upstream string) {
	fmt.Fprintf(b, "%sreverse_proxy %s {\n%s\tlb_try_duration %s\n%s}\n", indent, upstream, indent, proxyRetry, indent)
}

// splitSources is an allowlist's addresses and CIDRs, separated by commas or spaces.
func splitSources(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
}

// dockerPools are Docker's default address pools, which a stack's network is cut from.
var dockerPools = []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.168.0.0/16")}

// warnIPv6Allowlists warns of a route's allowlist that every IPv6 client passes: with IPv6 off on
// the stack's network, Docker's userland proxy hands the front an IPv6 client as the bridge's
// IPv4 gateway, gw, so a list holding it admits them all on a host IPv6 reaches (a global
// address). A gateway not read (gw invalid) is judged by Docker's default pools: a list overlapping
// one may hold it.
func (in *Installer) warnIPv6Allowlists(get func(string) string, gw netip.Addr) {
	if get(IPv6Var) == "true" || !slices.ContainsFunc(in.O.hostAddrs(), func(h string) bool {
		a, err := netip.ParseAddr(h)
		return err == nil && a.Is6() && !a.Is4In6() && a.IsGlobalUnicast() && !a.IsPrivate()
	}) {
		return
	}
	holds := func(p netip.Prefix) bool { return slices.ContainsFunc(dockerPools, p.Overlaps) }
	what := "an address of Docker's default pools, as which"
	if gw.IsValid() {
		holds = func(p netip.Prefix) bool { return p.Contains(gw) }
		what = "the stack network's gateway " + gw.String() + ", as which"
	}
	type listed struct{ name, input string }
	var lists []listed
	for _, r := range in.R.Spec.Front.Routes {
		lists = append(lists, listed{r.Name, r.AllowInput})
	}
	// An L4 route the front terminates TLS on takes no allowlist in loopback mode.
	for _, r := range in.R.Spec.L4.Routes {
		if r.Terminated() && get("STOWAGE_TLS_MODE") != Loopback {
			lists = append(lists, listed{r.Name, r.AllowInput})
		}
	}
	for _, r := range lists {
		if r.input == "" {
			continue
		}
		for _, s := range splitSources(get(r.input)) {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				a, aerr := netip.ParseAddr(s)
				if aerr != nil {
					continue
				}
				p = netip.PrefixFrom(a, a.BitLen())
			}
			if p.Addr().Is4In6() {
				p = netip.PrefixFrom(p.Addr().Unmap(), max(p.Bits()-96, 0))
			}
			if holds(p) {
				in.O.logf("warning: route %s: %s=%s: %s holds %s every IPv6 client passes while the stack's IPv6 is off and this host has a global IPv6 address: install --ipv6, or list narrower sources", r.name, r.input, get(r.input), s, what)
				break
			}
		}
	}
}

// frontGateway is the IPv4 gateway of the stack's default network, the front's, once it exists;
// invalid when it cannot be read.
func (in *Installer) frontGateway(ctx context.Context, environ []string) netip.Addr {
	out, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"network", "ls", "--quiet", "--filter", "label=com.docker.compose.project=" + environLookup(environ)("COMPOSE_PROJECT_NAME"), "--filter", "label=com.docker.compose.network=default"}})
	ids := strings.Fields(string(out))
	if err != nil || len(ids) != 1 {
		return netip.Addr{}
	}
	out, err = in.O.Docker.Run(ctx, Cmd{Args: []string{"network", "inspect", "--format", "{{range .IPAM.Config}}{{.Gateway}} {{end}}", ids[0]}})
	if err != nil {
		return netip.Addr{}
	}
	for _, f := range strings.Fields(string(out)) {
		if a, err := netip.ParseAddr(f); err == nil && a.Is4() {
			return a
		}
	}
	return netip.Addr{}
}

// checkSources accepts an allowlist of IP addresses and CIDRs (empty: refuse everyone).
func checkSources(v string) error {
	for _, s := range splitSources(v) {
		if _, err := netip.ParsePrefix(s); err == nil {
			continue
		}
		if a, err := netip.ParseAddr(s); err != nil || a.Zone() != "" {
			return fmt.Errorf("%q is not an IP address or a CIDR", s)
		}
	}
	return nil
}

var authAddressRe = regexp.MustCompile(`^(https?://)?[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)

// checkAuthAddress accepts an authentication service's address, [http[s]://]host[:port], or none
// (forward auth off).
func checkAuthAddress(v string) error {
	if v == "" {
		return nil
	}
	// An IPv6 address is bracketed, as in a URL: [fd00::1]:9091.
	hostPort := schemeRe.ReplaceAllString(v, "")
	if strings.HasPrefix(hostPort, "[") {
		var a netip.Addr
		var port uint16 = 1
		if inner, ok := strings.CutSuffix(strings.TrimPrefix(hostPort, "["), "]"); ok {
			a, _ = netip.ParseAddr(inner)
		} else if ap, err := netip.ParseAddrPort(hostPort); err == nil {
			a, port = ap.Addr(), ap.Port()
		}
		if a.Is6() && !a.Is4In6() && a.Zone() == "" && port != 0 {
			return nil
		}
	} else if authAddressRe.MatchString(v) {
		return nil
	}
	return fmt.Errorf("%q is not an address, [http[s]://]host[:port] (an IPv6 one bracketed)", v)
}

var schemeRe = regexp.MustCompile(`^https?://`)

var authURIRe = regexp.MustCompile(`^/[A-Za-z0-9._~!$&()*+,;=:@%/?-]*$`)

// checkAuthURI accepts the path (and query) the authentication service is asked at, or none.
func checkAuthURI(v string) error {
	if v != "" && !authURIRe.MatchString(v) {
		return fmt.Errorf("%q is not a path, /…", v)
	}
	return nil
}

// frontCheck is the check of an input a route's front configuration reads, nil for any other.
func frontCheck(f spec.Front, key string) func(string) error {
	for _, r := range f.Routes {
		switch {
		case r.AllowInput == key:
			return checkSources
		case r.ForwardAuth != nil && r.ForwardAuth.AddressInput == key:
			return checkAuthAddress
		case r.ForwardAuth != nil && r.ForwardAuth.URIInput == key:
			return checkAuthURI
		}
	}
	return nil
}

// CA paths in the front's container: the local CA's root (Caddy's data directory), a byo CA, and
// the project volume publish_ca names.
const (
	localRoot = "/data/caddy/pki/authorities/local/root.crt"
	byoCA     = frontTLS + "/ca.pem"
	caMount   = "/stowage-ca"
)

// FrontUID is the user and group the front runs as (frontUser) unless the volume it publishes its
// CA in is given another: never root, with no capability (it binds 80 and 443 as its network
// namespace lets any user: frontSysctls). prepareFront gives it its volume and the published CA's.
const FrontUID = 65532

// frontUser is the uid (and gid) the front runs as: the one the release gives the volume it
// publishes its CA in (data.volumes.<volume>.uid, never 0: lint), whose services own it and which
// the front writes in; else FrontUID.
func (in *Installer) frontUser() int {
	if p := in.R.Spec.Front.PublishCA; p != nil {
		if uid := in.R.Spec.Data.Volumes[p.Volume].UID; uid != nil && *uid > 0 {
			return *uid
		}
	}
	return FrontUID
}

// frontCaddyfile is where the front reads its configuration: in its own volume, where prepareFront
// copies the install's Caddyfile (readable by the installer's user alone: it holds the logins'
// hashes) at each start.
const frontCaddyfile = "/data/stowage-Caddyfile"

// frontTLS is where the front reads a byo certificate (cert.pem, key.pem, ca.pem): in its own
// volume, where prepareFront copies them from the install's TLSDir, readable by the installer's
// user alone, at each start.
const frontTLS = "/data/stowage-tls"

// frontSysctls let the front's user bind the ports under 1024 in its own network namespace (the
// stack's network, never the host's), as Docker's engines do by default: no capability needed.
var frontSysctls = map[string]string{"net.ipv4.ip_unprivileged_port_start": "0"}

// FrontDataVolume is the front's own volume (Caddy's data directory: its certificates, its local
// CA), in the compose project.
const FrontDataVolume = "stowage-front-data"

// frontCommand runs Caddy; with publish_ca, after copying ca (a path in the container, as soon as
// it exists: the local CA's root is made as Caddy starts) into the published volume as ca.crt, or
// removing a ca.crt there when ca is empty. noALPN (a TLS front terminating an L4 route) runs it
// from the Caddyfile adapted to JSON, where the L4 routes' noALPNMark is the empty ALPN list.
func frontCommand(f spec.Front, ca string, noALPN bool) []string {
	caddy := []string{"caddy", "run", "--config", frontCaddyfile, "--adapter", "caddyfile"}
	if f.PublishCA == nil && !noALPN {
		return caddy
	}
	run := "exec " + strings.Join(caddy, " ")
	if noALPN {
		// A Caddyfile cannot write an empty ALPN list (an absent one is h2 and http/1.1): the
		// configuration is adapted to JSON, where noALPNMark becomes the empty list. A mark left
		// (adapt's output changed) stops the start: served, it would refuse every client's ALPN.
		run = fmt.Sprintf(`caddy adapt --config `+frontCaddyfile+` --adapter caddyfile >%[1]s && sed -i 's/"alpn":\["%[2]s"\]/"alpn":[]/g' %[1]s && if grep -q %[2]s %[1]s; then echo "%[2]s left in %[1]s: the front's ALPN cannot be cleared" >&2; exit 1; fi && exec caddy run --config %[1]s`, frontJSON, noALPNMark)
	}
	switch {
	case f.PublishCA == nil:
		return []string{"sh", "-c", run}
	case ca == "":
		return []string{"sh", "-c", "rm -f " + caMount + "/ca.crt; " + run}
	}
	// cp keeps the source's mode (Caddy writes its root 0600): the copy is made readable to every
	// service's user.
	cp := fmt.Sprintf("(until [ -s %[1]s ]; do sleep 1; done; cp %[1]s %[2]s/.ca.crt && chmod 0644 %[2]s/.ca.crt && mv %[2]s/.ca.crt %[2]s/ca.crt) & ", ca, caMount)
	return []string{"sh", "-c", cp + run}
}

// noALPNMark is the ALPN protocol the Caddyfile names for "none", which the front's start
// (frontCommand) replaces with the empty list; lint refuses it as a declared protocol.
const noALPNMark = spec.NoALPNMark

// frontJSON is where the front's start writes the configuration adapted from the Caddyfile.
const frontJSON = "/tmp/stowage-caddy.json"

// frontCompose adds the front service to the project's stack, unless it has none: in loopback mode each route on its
// port of the loopback address, behind TLS the HTTP and HTTPS ports something serves on alone
// (frontServed; 80 and 443 unless moved), on every address. The host ports come from .env. The L4 routes' ports are published
// in every mode, to their services: each on every address, or on the one its bind input holds (get
// reads a recorded value).
// digest labels the front with its configuration (the Caddyfile, a byo certificate), so a change
// recreates it. ca is the CA it publishes (frontCommand); noALPN, the Caddyfile names noALPNMark.
// clientCA mounts the install's client CA.
// ipv6 turns IPv6 on for the default network. onHost are the services on the host's network
// (hostServices), whose L4 routes' ports are their own. frontV4 publishes the front's ports on
// IPv4 alone (frontV4Only). uid is the front's user and group (frontUser).
func frontCompose(f spec.Front, l4 []spec.L4Route, get func(string) string, mode string, host hostAPI, digest, ca string, noALPN, clientCA, ipv6 bool, onHost, v4Only []string, frontV4 bool, uid int) ([]byte, error) {
	var ports []any
	volumes := []string{FrontDataVolume + ":/data"}
	if mode == Loopback {
		for i, r := range f.Routes {
			ports = append(ports, fmt.Sprintf("${STOWAGE_LOOPBACK_ADDRESS}:${%s}:%d", PortVar(r.Name), frontPort(i)))
		}
	} else {
		// The ports something serves on alone (frontServed), as the VM's firewall opens them; held
		// to its interfaces on a Docker host without the stack's IPv6, on IPv4's every address
		// alone, as an allowlisted route is (frontV4Only).
		ports = []any{}
		on := ""
		if frontV4 {
			on = "0.0.0.0:"
		}
		http, https := frontServed(f, l4, mode)
		if http {
			ports = append(ports, on+"${"+HTTPPortVar+"}:80")
		}
		if https {
			ports = append(ports, on+"${"+HTTPSPortVar+"}:443")
		}
	}
	if clientCA {
		volumes = append(volumes, "./"+ClientCADir+":"+clientCADir+":ro")
	}
	if f.PublishCA != nil {
		volumes = append(volumes, f.PublishCA.Volume+":"+caMount)
	}
	// Confined as every service Stowage adds: its own user, no capability, no privilege gain, a
	// read-only root (its writes go to its volume, the published CA's, /tmp, where its start
	// adapts the configuration, and /config, Caddy's configuration directory, which it never
	// persists to: persist_config off).
	service := map[string]any{
		"image":        "${" + FrontImageVar + "}",
		"command":      frontCommand(f, ca, noALPN),
		"volumes":      volumes,
		"ports":        ports,
		"restart":      "unless-stopped",
		"labels":       map[string]string{FrontConfigLabel: digest},
		"user":         fmt.Sprintf("%d:%d", uid, uid),
		"cap_drop":     []string{"ALL"},
		"security_opt": []string{"no-new-privileges:true"},
		"read_only":    true,
		"tmpfs":        []string{"/tmp", "/config"},
		"sysctls":      frontSysctls,
	}
	if mode == "dns-cloudflare" {
		service["environment"] = map[string]string{"CLOUDFLARE_API_TOKEN": "${" + CloudflareTokenVar + "}"}
	}
	// Each route's domain names the front on the stack's network too: a service reaching a route
	// as its clients do (an issuer's URL) reaches the front, not the address the name has outside,
	// on the port its URL names (443 in the container, or the site's other address).
	if aliases := routeAliases(f, mode, get); len(aliases) > 0 {
		service["networks"] = map[string]any{"default": map[string]any{"aliases": aliases}}
	}
	services := map[string]any{FrontService: service}
	volumesDoc := map[string]any{FrontDataVolume: map[string]any{}}
	if !f.Declared() {
		services, volumesDoc = map[string]any{}, nil // no front: the L4 ports and the host API alone
	}
	for _, svc := range host.services {
		services[svc] = map[string]any{
			"volumes": []string{host.dir + ":" + HostAppMount + ":ro"},
			"environment": map[string]string{
				HostSocketVar: HostAppMount + "/" + appSocketName,
				HostTokenVar:  "${" + HostTokenVar + "}",
			},
		}
	}
	// Each L4 route's host port, on every address or its bind address, to its upstream as it is:
	// compose appends these to the service's own ports. A route of v4Only is published on IPv4's
	// every address alone: Docker's userland proxy would carry its IPv6 clients past DOCKER-USER,
	// where its allowlist is (allowRules). None for a service on the host's network
	// (hostNet), which listens on the port itself: on the VM, every route's.
	for i, r := range l4 {
		name, target, _ := strings.Cut(r.Upstream, ":")
		if r.Terminated() {
			// The front's own port: it terminates TLS there and forwards the plaintext stream,
			// filtering the sources itself. In loopback mode, plaintext on the loopback address
			// alone: TLS is what is in front of the install's.
			port := map[string]any{"target": frontL4Port(i), "published": strconv.Itoa(r.Port), "protocol": "tcp"}
			switch a := bindAddr(r, get); {
			case mode == Loopback:
				port["host_ip"] = "${STOWAGE_LOOPBACK_ADDRESS}"
			case a != "":
				port["host_ip"] = a
			case slices.Contains(v4Only, r.Name):
				port["host_ip"] = "0.0.0.0"
			}
			front, _ := services[FrontService].(map[string]any)
			if front == nil {
				return nil, fmt.Errorf("l4.routes %s: the front terminates its TLS, and there is no front", r.Name)
			}
			front["ports"] = append(front["ports"].([]any), port)
			continue
		}
		if slices.Contains(onHost, name) {
			continue
		}
		svc, _ := services[name].(map[string]any)
		if svc == nil {
			svc = map[string]any{}
			services[name] = svc
		}
		ports, _ := svc["ports"].([]any)
		port := map[string]any{"target": target, "published": strconv.Itoa(r.Port), "protocol": r.Protocol}
		if a := bindAddr(r, get); a != "" {
			port["host_ip"] = a
		} else if slices.Contains(v4Only, r.Name) {
			port["host_ip"] = "0.0.0.0"
		}
		svc["ports"] = append(ports, port)
	}
	doc := map[string]any{"services": services}
	if volumesDoc != nil {
		doc["volumes"] = volumesDoc
	}
	// IPv6 on the default network, which the front is on: Docker forwards the published ports
	// over IPv6 to the front itself (its own unique local subnet, NAT), rather than through its
	// userland proxy, which shows every IPv6 client as the bridge's IPv4 gateway.
	// The label records the choice (ipv6Network): without it, IPv6 is off, and the project's own
	// definition of its default network (an external one included) is left alone.
	if ipv6 {
		doc["networks"] = map[string]any{"default": map[string]any{"enable_ipv6": true, "labels": map[string]string{IPv6Label: "true"}}}
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return append([]byte("# Written by the installer: the front Stowage adds to the stack.\n"), out...), nil
}

// hostDomains are the routes' domains a service on the host's network reaches the front by at the
// loopback: only where the front publishes the port the routes' URLs name (acme's URLs keep 443
// when its host port moves, which nothing on the host holds).
func hostDomains(f spec.Front, mode string, get func(string) string) []string {
	if publicHTTPSPort(mode, get(HTTPSPortVar)) != get(HTTPSPortVar) {
		return nil
	}
	return routeAliases(f, mode, get)
}

// routeAliases are the front's routes' domains in a TLS mode, sorted, each once: the names the
// stack's services reach the front by. None in loopback mode, which serves no domain.
func routeAliases(f spec.Front, mode string, get func(string) string) []string {
	if mode == Loopback {
		return nil
	}
	var aliases []string
	for _, r := range f.Routes {
		if d := get(r.DomainInput); d != "" && !slices.Contains(aliases, d) {
			aliases = append(aliases, d)
		}
	}
	slices.Sort(aliases)
	return aliases
}

// The lines frontUserShell prints for what the front's user cannot fix itself, which
// frontRootShell then fixes: frontPrepForeign, a file of another user's in the front's volume;
// frontPrepCA and the uid, the published CA's directory another uid holds.
const (
	frontPrepForeign = "stowage-front-foreign"
	frontPrepCA      = "stowage-front-ca"
)

// frontUserShell runs as the front's user (uid) with no capability: in its own volume (/data),
// whatever a link there leads to, it writes nothing the user could not. It says what it cannot do
// itself, before anything is written (a refusal then leaves the volume as it was): a file of
// another user's in /data (frontPrepForeign), the published CA's directory (/stowage-ca) of another
// uid (frontPrepCA and that uid). Otherwise it removes the front's temporary copy of the CA
// another user holds there (an interrupted earlier front's, run as root: one the front's own user
// holds, it overwrites, so a copy a running front is making is left alone), and takes the files the installer gives on its standard
// input (a tar: Caddyfile, and tls/ for a byo certificate) into a directory of its own, renames the
// Caddyfile to frontCaddyfile and tls/ to frontTLS (a copy no longer given removed), and removes
// what an interrupted run left.
func frontUserShell(uid int) string {
	return fmt.Sprintf(`ca=
if [ -d %[3]s ] && [ "$(stat -c %%u %[3]s)" != %[1]d ]; then ca=1; echo "%[5]s $(stat -c %%u %[3]s)"; fi
if [ -n "$(find /data ! -user %[1]d -print | head -n 1)" ]; then echo %[4]s; exit 0; fi
if [ -n "$ca" ]; then exit 0; fi
if [ -d %[3]s ]; then find %[3]s -maxdepth 1 -name .ca.crt -type f ! -user %[1]d -exec rm -f {} +; fi
cd /data
rm -rf .stowage-in
mkdir -m 0700 .stowage-in
tar -x -f - -C .stowage-in
mv -T -f .stowage-in/Caddyfile %[6]s
rm -rf %[2]s
if [ -d .stowage-in/tls ]; then mv -T .stowage-in/tls %[2]s; fi
rm -rf .stowage-in
`, uid, frontTLS, caMount, frontPrepForeign, frontPrepCA, frontCaddyfile)
}

// frontRootShell runs as root, the front stopped, for what frontUserShell could not do, as
// prepareFront decided it: with data, it gives the front's user (uid) its volume (chown -hR: no
// link followed); with ca, the published CA's directory alone (the project's files in it left as
// they are), first removing the front's temporary copy an interrupted earlier one left (rm
// removes a link, never follows it).
func frontRootShell(uid int, data, ca bool) string {
	return fmt.Sprintf(`if %[2]t; then chown -hR %[1]d:%[1]d /data; fi
if %[3]t; then rm -f %[4]s/.ca.crt; chown %[1]d:%[1]d %[4]s; fi
`, uid, data, ca, caMount)
}

// frontVolumes are the front's volume and the published CA's (empty without publish_ca) as the
// engine holds them (m names them, else compose's <project>_<key>).
func (in *Installer) frontVolumes(env *EnvFile, m *composeModel) (data, ca string, err error) {
	project, _ := env.Get("COMPOSE_PROJECT_NAME")
	volume := func(key string) string {
		if n := m.Volumes[key].Name; n != "" {
			return n
		}
		if p := cmp.Or(m.Name, project); p != "" {
			return p + "_" + key
		}
		return ""
	}
	if data = volume(FrontDataVolume); data == "" {
		return "", "", fmt.Errorf("the front's volume: the stack names no %s", FrontDataVolume)
	}
	if p := in.R.Spec.Front.PublishCA; p != nil {
		if ca = volume(p.Volume); ca == "" {
			return "", "", fmt.Errorf("front.publish_ca.volume: the stack names no volume %s", p.Volume)
		}
	}
	return data, ca, nil
}

// noSuchVolume reports whether err is the engine's saying a volume is not there.
func noSuchVolume(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no such volume")
}

// frontVolumesMissing reports which of the front's volumes (frontVolumes) the engine lacks: its
// own (data) and the published CA's (ca), as `docker volume inspect` of each tells; another failure
// of the engine is an error. The start then makes them, with the stack's containers, before
// prepareFront.
func (in *Installer) frontVolumesMissing(ctx context.Context, env *EnvFile, m *composeModel) (data, ca bool, err error) {
	if !in.R.Spec.HasFront() {
		return false, false, nil
	}
	dv, cv, err := in.frontVolumes(env, m)
	if err != nil {
		return false, false, err
	}
	gone := func(v string) (bool, error) {
		if v == "" {
			return false, nil
		}
		_, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"volume", "inspect", "--format", "{{.Name}}", v}})
		if noSuchVolume(err) {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("the front's volume %s: %w", v, err)
		}
		return false, nil
	}
	if data, err = gone(dv); err != nil {
		return false, false, err
	}
	ca, err = gone(cv)
	return data, ca, err
}

// prepareFront readies the front's volumes for its user before the stack's start runs it, the
// volumes made: frontUserShell, in the front's image as the front's user with no capability, off
// any network, its root read-only, given the Caddyfile and a byo certificate on its standard input;
// then, only for what it says it cannot do, frontRootShell as root with the capabilities to change
// owners and remove a file alone, the front stopped first (stop), and frontUserShell again. The
// published CA's directory is given to the front's user when root holds it; one another uid holds
// is refused by a user's run before it writes anything (the first, before the front is stopped, or
// the one judging again after a front of another user was stopped), naming the operator's fix and
// the project's. A front running as another user is stopped after that first run and the volume
// judged again (what it wrote meanwhile given back too); a front the step stopped stays stopped when
// the start fails.
// A byo install whose certificate's copy is gone is refused, naming the re-run that gives one.
// Nothing for a project without a front.
func (in *Installer) prepareFront(ctx context.Context, env *EnvFile, m *composeModel, stop func() error) error {
	if !in.R.Spec.HasFront() {
		return nil
	}
	image, _ := env.Get(FrontImageVar)
	if image == "" {
		return fmt.Errorf("the front's volume: no front image recorded (%s)", FrontImageVar)
	}
	data, ca, err := in.frontVolumes(env, m)
	if err != nil {
		return err
	}
	mode, _ := env.Get("STOWAGE_TLS_MODE")
	files, err := in.frontFiles(mode == "byo")
	if err != nil {
		return err
	}
	uid := in.frontUser()
	mounts := []string{"--volume", data + ":/data"}
	key := ""
	if p := in.R.Spec.Front.PublishCA; p != nil {
		key = p.Volume
		mounts = append(mounts, "--volume", ca+":"+caMount)
	}
	user := func(judge bool) (foreign bool, caUID int, err error) {
		args := append([]string{"run", "--rm", "-i", "--pull", "never", "--network", "none", "--read-only", "--user", fmt.Sprintf("%d:%d", uid, uid),
			"--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--entrypoint", "sh"}, mounts...)
		out, err := in.O.Docker.Run(ctx, Cmd{Args: append(args, image, "-euc", frontUserShell(uid)), Stdin: bytes.NewReader(files), Env: env.Environ(in.O.environ())})
		if err != nil {
			return false, -1, fmt.Errorf("giving the front its files: %w", err)
		}
		caUID = -1
		for line := range strings.Lines(string(out)) {
			f := strings.Fields(line)
			switch {
			case len(f) == 1 && f[0] == frontPrepForeign:
				foreign = true
			case len(f) == 2 && f[0] == frontPrepCA:
				if n, err := strconv.Atoi(f[1]); err == nil && n != uid {
					caUID = n
				}
			}
		}
		// The CA's directory another user's: refused, nothing changed. Root's, as Docker makes a
		// volume, is given to the front.
		if judge && caUID > 0 {
			fix := fmt.Sprintf("give that directory to the front's user, if the volume's services write nothing there: `docker run --rm --user 0 --entrypoint chown -v %s:/v %s %d:%d /v`", ca, image, uid, uid)
			// The project's way, where it declares no uid for the volume (the front then runs as
			// FrontUID, which never holds it here): declare the holder's, and the front runs as it.
			if in.R.Spec.Data.Volumes[key].UID == nil {
				fix += fmt.Sprintf("; or the project declares data.volumes.%s.uid: %d, the uid holding it, which the front then runs as", key, caUID)
			}
			return false, -1, refuse(fix,
				"front.publish_ca.volume: the volume %s's directory belongs to uid %d, not the front's (%d), which writes its CA there as its own user", key, caUID, uid)
		}
		return foreign, caUID, nil
	}
	foreign, caUID, err := user(true)
	if err != nil {
		return err
	}
	// A front running as another user (an older release's ran as root) writes files the front's
	// user may not: stopped, and the volume judged again.
	other, err := in.frontRunsAsAnother(ctx, env, m, uid)
	if err != nil {
		return err
	}
	if !foreign && caUID < 0 && !other {
		return nil
	}
	if err := stop(); err != nil {
		return err
	}
	if other { // what it wrote until it stopped is judged too
		if foreign, caUID, err = user(true); err != nil {
			return err
		}
		if !foreign && caUID < 0 {
			return nil
		}
	}
	args := append([]string{"run", "--rm", "--pull", "never", "--network", "none", "--read-only", "--user", "0", "--cap-drop", "ALL",
		"--cap-add", "CHOWN", "--cap-add", "FOWNER", "--cap-add", "DAC_OVERRIDE", "--cap-add", "DAC_READ_SEARCH", "--security-opt", "no-new-privileges:true", "--entrypoint", "sh"}, mounts...)
	if _, err := in.O.Docker.Run(ctx, Cmd{Args: append(args, image, "-euc", frontRootShell(uid, foreign, caUID >= 0)), Env: env.Environ(in.O.environ())}); err != nil {
		return fmt.Errorf("giving the front its volumes: %w", err)
	}
	// Root's run judged: what is still there fails the start as it is, never as the project's.
	if foreign, caUID, err = user(false); err != nil {
		return err
	}
	switch {
	case foreign:
		return fmt.Errorf("giving the front its volumes: a file of another user's is still in %s", data)
	case caUID >= 0:
		return fmt.Errorf("giving the front its volumes: the published CA's volume %s is still uid %d's", ca, caUID)
	}
	return nil
}

// frontRunsAsAnother reports whether the stack's running front runs as another user than uid (an
// older release's ran as root): it writes files the front's user may not, so it is stopped first.
func (in *Installer) frontRunsAsAnother(ctx context.Context, env *EnvFile, m *composeModel, uid int) (bool, error) {
	project, _ := env.Get("COMPOSE_PROJECT_NAME")
	project = cmp.Or(m.Name, project)
	out, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"ps", "-q", "--filter", "label=com.docker.compose.project=" + project, "--filter", "label=com.docker.compose.service=" + FrontService}})
	if err != nil {
		return false, fmt.Errorf("the front's container: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return false, nil
	}
	users, err := in.O.Docker.Run(ctx, Cmd{Args: append([]string{"container", "inspect", "--format", "{{.Config.User}}"}, ids...)})
	if err != nil {
		return true, nil // one gone since, or unread: stopped all the same, the safe side
	}
	want := fmt.Sprintf("%d:%d", uid, uid)
	lines := strings.Split(strings.TrimRight(string(users), "\n"), "\n")
	return len(lines) != len(ids) || slices.ContainsFunc(lines, func(u string) bool { return strings.TrimSpace(u) != want }), nil
}

// frontFiles is the tar frontUserShell takes on its standard input: the install's Caddyfile, and
// with byo its certificate's files (tls/), readable by the front's user alone. A byo install
// whose copy is gone is refused, naming the re-run that gives one.
func (in *Installer) frontFiles(byo bool) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(name string, raw []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(raw)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(raw)
		return err
	}
	cf, err := os.ReadFile(in.path(Caddyfile))
	if err != nil {
		return nil, fmt.Errorf("the front's configuration: %w", err)
	}
	if err := add("Caddyfile", cf); err != nil {
		return nil, err
	}
	if byo {
		for _, f := range []string{"cert.pem", "key.pem"} {
			if _, err := os.Stat(filepath.Join(in.path(TLSDir), f)); err != nil {
				return nil, refuse(in.flagFix("--cert FILE --key FILE"), "the byo certificate's copy is gone (%v): a re-run with --cert FILE --key FILE gives the front one", err)
			}
		}
		if err := tw.WriteHeader(&tar.Header{Name: "tls/", Mode: 0o700, Typeflag: tar.TypeDir}); err != nil {
			return nil, err
		}
		for _, f := range []string{"cert.pem", "key.pem", "ca.pem"} {
			raw, err := os.ReadFile(filepath.Join(in.path(TLSDir), f))
			if errors.Is(err, os.ErrNotExist) && f == "ca.pem" {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("the byo certificate: %w", err)
			}
			if err := add("tls/"+f, raw); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
