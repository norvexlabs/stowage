package install

import (
	"context"
	"fmt"
	"net/netip"
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

// PortVar records a route's loopback port in .env.
func PortVar(route string) string {
	return "STOWAGE_PORT_" + strings.TrimPrefix(spec.URLVar(route), "STOWAGE_URL_")
}

// loopbackCaddyfile serves each route on its own plain-HTTP port, as routeBody says; get reads a
// recorded value. A route asking for a client certificate refuses everyone: plain HTTP carries
// none.
func loopbackCaddyfile(f spec.Front, get func(string) string, logins map[string]login) string {
	var b strings.Builder
	b.WriteString("# Written by the installer from the project's front routes; rewritten on every run.\n")
	b.WriteString("{\n\tadmin off\n\tauto_https off\n\tpersist_config off\n}\n")
	for i, r := range f.Routes {
		fmt.Fprintf(&b, "\n# %s\nhttp://:%d {\n", r.Name, frontPort(i))
		if r.ClientAuth {
			b.WriteString("\trespond 403\n")
		} else {
			routeBody(&b, r, get, logins)
		}
		b.WriteString("}\n")
	}
	return b.String()
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
		fmt.Fprintf(b, "\treverse_proxy %s\n", r.Upstream)
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
	fmt.Fprintf(b, "\t\treverse_proxy %s\n\t}\n", r.Upstream)
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
	for _, r := range in.R.Spec.Front.Routes {
		if r.AllowInput == "" {
			continue
		}
		for _, s := range splitSources(get(r.AllowInput)) {
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
				in.O.logf("warning: route %s: %s=%s holds %s, %s every IPv6 client passes while the stack's IPv6 is off and this host has a global IPv6 address: install --ipv6, or list narrower sources", r.Name, r.AllowInput, get(r.AllowInput), what, s)
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
	byoCA     = "/etc/caddy/" + TLSDir + "/ca.pem"
	caMount   = "/stowage-ca"
)

// frontCommand runs Caddy; with publish_ca, after copying ca (a path in the container, as soon as
// it exists: the local CA's root is made as Caddy starts) into the published volume as ca.crt, or
// removing a ca.crt there when ca is empty.
func frontCommand(f spec.Front, ca string) []string {
	caddy := []string{"caddy", "run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"}
	if f.PublishCA == nil {
		return caddy
	}
	run := "exec " + strings.Join(caddy, " ")
	if ca == "" {
		return []string{"sh", "-c", "rm -f " + caMount + "/ca.crt; " + run}
	}
	// cp keeps the source's mode (Caddy writes its root 0600): the copy is made readable to every
	// service's user.
	cp := fmt.Sprintf("(until [ -s %[1]s ]; do sleep 1; done; cp %[1]s %[2]s/.ca.crt && chmod 0644 %[2]s/.ca.crt && mv %[2]s/.ca.crt %[2]s/ca.crt) & ", ca, caMount)
	return []string{"sh", "-c", cp + run}
}

// frontCompose adds the front service to the project's stack, unless it has none: in loopback mode each route on its
// port of the loopback address, behind TLS only the HTTP and HTTPS ports (80 and 443 unless
// moved), on every address. The host ports come from .env. The L4 routes' ports are published
// in every mode, to their services: each on every address, or on the one its bind input holds (get
// reads a recorded value).
// digest labels the front with its configuration (the Caddyfile, a byo certificate), so a change
// recreates it. ca is the CA it publishes (frontCommand). clientCA mounts the install's client CA.
// ipv6 turns IPv6 on for the default network.
func frontCompose(f spec.Front, l4 []spec.L4Route, get func(string) string, mode string, host hostAPI, digest, ca string, clientCA, ipv6, vm bool) ([]byte, error) {
	var ports []string
	volumes := []string{"./" + Caddyfile + ":/etc/caddy/Caddyfile:ro", "stowage-front-data:/data"}
	if mode == Loopback {
		for i, r := range f.Routes {
			ports = append(ports, fmt.Sprintf("${STOWAGE_LOOPBACK_ADDRESS}:${%s}:%d", PortVar(r.Name), frontPort(i)))
		}
	} else {
		ports = []string{"${" + HTTPPortVar + "}:${" + HTTPPortVar + "}", "${" + HTTPSPortVar + "}:${" + HTTPSPortVar + "}"}
	}
	if mode == "byo" {
		volumes = append(volumes, "./"+TLSDir+":/etc/caddy/"+TLSDir+":ro")
	}
	if clientCA {
		volumes = append(volumes, "./"+ClientCADir+":"+clientCADir+":ro")
	}
	if f.PublishCA != nil {
		volumes = append(volumes, f.PublishCA.Volume+":"+caMount)
	}
	service := map[string]any{
		"image":   "${" + FrontImageVar + "}",
		"command": frontCommand(f, ca),
		"volumes": volumes,
		"ports":   ports,
		"restart": "unless-stopped",
		"labels":  map[string]string{FrontConfigLabel: digest},
	}
	if mode == "dns-cloudflare" {
		service["environment"] = map[string]string{"CLOUDFLARE_API_TOKEN": "${" + CloudflareTokenVar + "}"}
	}
	// Each route's domain names the front on the stack's network too: a service reaching a route
	// as its clients do (an issuer's URL) reaches the front, not the address the name has outside.
	if mode != Loopback {
		var aliases []string
		for _, r := range f.Routes {
			if d := get(r.DomainInput); d != "" && !slices.Contains(aliases, d) {
				aliases = append(aliases, d)
			}
		}
		if len(aliases) > 0 {
			slices.Sort(aliases)
			service["networks"] = map[string]any{"default": map[string]any{"aliases": aliases}}
		}
	}
	services := map[string]any{FrontService: service}
	volumesDoc := map[string]any{"stowage-front-data": map[string]any{}}
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
	// compose appends these to the service's own ports. The VM publishes none: the route's
	// service runs on the host's network (hostNet).
	if vm {
		l4 = nil
	}
	for _, r := range l4 {
		name, target, _ := strings.Cut(r.Upstream, ":")
		svc, _ := services[name].(map[string]any)
		if svc == nil {
			svc = map[string]any{}
			services[name] = svc
		}
		ports, _ := svc["ports"].([]any)
		port := map[string]any{"target": target, "published": strconv.Itoa(r.Port), "protocol": r.Protocol}
		if a := bindAddr(r, get); a != "" {
			port["host_ip"] = a
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
