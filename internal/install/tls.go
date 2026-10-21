package install

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/norvexlabs/stowage/internal/spec"
)

// Loopback is the TLS mode "none": plain HTTP, each route on its own port of a loopback address.
const Loopback = "none"

// ACMEEmailVar records the contact Let's Encrypt registers the front's account with.
const ACMEEmailVar = "STOWAGE_ACME_EMAIL"

// CloudflareTokenVar records the Cloudflare API token the front proves each domain with in the
// dns-cloudflare mode. The front's container is given it as CLOUDFLARE_API_TOKEN; the project's
// services and the provision hook are not (a stack that reads .env itself sees it).
const CloudflareTokenVar = "STOWAGE_CLOUDFLARE_API_TOKEN"

// TLSDir holds a byo certificate and key in the install directory.
const TLSDir = "tls"

var modeLabels = map[string]string{
	Loopback:         "Only on this machine: plain HTTP on a loopback address",
	"acme":           "HTTPS on your domains, certificates from Let's Encrypt",
	"dns-cloudflare": "HTTPS on your domains, Let's Encrypt through Cloudflare DNS",
	"byo":            "HTTPS on your domains, with your own certificate",
	"internal":       "HTTPS on your domains, certificates from a local CA browsers do not trust",
}

// domainKeys maps each route to the input holding its domain.
func (in *Installer) domainKeys() map[string]string {
	out := map[string]string{}
	for _, r := range in.R.Spec.Front.Routes {
		out[r.Name] = r.DomainInput
	}
	return out
}

// applyDomains turns --domains route=host into the routes' domain inputs.
func (in *Installer) applyDomains() error {
	o := &in.O
	if !in.R.Spec.HasFront() {
		return in.frontFlags()
	}
	keys := in.domainKeys()
	for route, host := range o.Domains {
		k, ok := keys[route]
		if !ok {
			return fmt.Errorf("--domains: %q is no route (%s)", route, strings.Join(in.R.Spec.Front.RouteNames(), ", "))
		}
		if v, set := o.Set[k]; set && v != host {
			return fmt.Errorf("--domains %s=%s and --set %s=%s disagree", route, host, k, v)
		}
		if o.Set == nil {
			o.Set = map[string]string{}
		}
		o.Set[k] = host
	}
	return nil
}

// mode decides how the front serves: --tls; else byo for --cert or --key; else HTTPS (acme when
// offered) when a domain was given by --domains, --set or the answers file — or, with no terminal
// and no loopback mode offered, by the environment: nothing is published on an exported variable
// alone; else the operator's choice when there is a terminal; else loopback.
// asked reports that the operator chose, so the form may go back to that choice.
func (in *Installer) mode(ctx context.Context, prev string) (mode string, asked bool, err error) {
	if !in.R.Spec.HasFront() {
		return "", false, in.frontFlags()
	}
	o, offered := &in.O, in.R.Spec.Front.TLSModes
	if in.remote {
		offered = remoteModes(offered)
		if len(offered) == 0 {
			return "", false, fmt.Errorf("%s offers no TLS mode a VM can be created with: %s (loopback is not reached from outside the VM, and byo's key never goes in its cloud-config)", in.R.Spec.Name, strings.Join(in.R.Spec.Front.TLSModes, ", "))
		}
	}
	if len(offered) == 0 {
		return "", false, fmt.Errorf("%s offers no TLS mode", in.R.Spec.Name)
	}
	domainGiven := false
	for _, k := range in.domainKeys() {
		_, set := o.Set[k]
		_, answered := o.Answers[k]
		// The environment counts only where loopback is not offered: a variable exported for
		// something else must not publish the stack.
		env := in.inputEnv(k) != "" && o.Prompt == nil && !slices.Contains(offered, Loopback)
		domainGiven = domainGiven || set || answered || env
	}
	switch {
	case o.TLS != "":
		mode = o.TLS
	case o.Cert != "" || o.Key != "":
		mode = "byo"
	case domainGiven:
		if mode = domainMode(offered); mode == "" {
			return "", false, fmt.Errorf("a domain was given, but this installer serves %s over no HTTPS mode (it offers %s)", in.R.Spec.Name, strings.Join(offered, ", "))
		}
	case o.Prompt != nil && len(offered) > 1:
		var choices []Choice
		for _, m := range offered {
			choices = append(choices, Choice{m, modeLabels[m]})
		}
		def := cmpFirst(offered, Loopback, func(string) bool { return true })
		a, err := o.Prompt.Ask(ctx, Form{
			Questions: []Question{{Title: "How is it served", Key: "--tls", Choices: choices}},
			Defaults:  func(map[string]string) map[string]string { return map[string]string{"--tls": def} },
			Answers:   map[string]string{"--tls": prev},
		})
		if err != nil {
			return "", false, err
		}
		mode, asked = a["--tls"], true
		if mode == "" {
			mode = def
		}
	case slices.Contains(offered, Loopback):
		mode = Loopback
	case o.Prompt != nil && len(offered) == 1:
		mode = offered[0] // its domains are asked
	default:
		return "", false, fmt.Errorf("%s is served over HTTPS only: give --domains (%s)", in.R.Spec.Name, strings.Join(in.R.Spec.Front.RouteNames(), ", "))
	}
	if !slices.Contains(offered, mode) {
		return "", false, fmt.Errorf("--tls %s: %s offers %s", mode, in.R.Spec.Name, strings.Join(offered, ", "))
	}
	if mode == Loopback && domainGiven {
		return "", false, errors.New("a domain was given: loopback mode serves no domain")
	}
	if (o.Cert != "" || o.Key != "") && mode != "byo" {
		return "", false, fmt.Errorf("--cert and --key are for --tls byo, not %s", mode)
	}
	if err := in.caFlag(mode); err != nil {
		return "", false, err
	}
	return mode, asked, nil
}

// caFlag refuses --ca outside byo, and for a project that publishes no CA.
func (in *Installer) caFlag(mode string) error {
	switch {
	case in.O.CA == "":
		return nil
	case in.R.Spec.Front.PublishCA == nil:
		return fmt.Errorf("--ca: %s publishes no CA to its services", in.R.Spec.Name)
	case mode != "byo":
		return fmt.Errorf("--ca is for --tls byo, not %s", mode)
	}
	return nil
}

// frontFlags refuses, for a project without a front, the flags only a front takes: its TLS mode,
// certificate, domains, ports and loopback address.
func (in *Installer) frontFlags() error {
	o := &in.O
	var given []string
	for _, f := range []struct {
		flag string
		set  bool
	}{
		{"--tls", o.TLS != ""}, {"--cert", o.Cert != ""}, {"--key", o.Key != ""}, {"--ca", o.CA != ""},
		{"--domains", len(o.Domains) > 0}, {"--http-port", o.HTTPPort != 0}, {"--https-port", o.HTTPSPort != 0},
		{"--ports", len(o.Ports) > 0}, {"--address", o.Address != ""},
	} {
		if f.set {
			given = append(given, f.flag)
		}
	}
	if len(given) > 0 {
		return fmt.Errorf("%s has no front: %s set how a front serves, and its L4 routes are its only way in", in.R.Spec.Name, strings.Join(given, ", "))
	}
	return nil
}

// remoteModes are the modes of offered a VM created from afar can take: it is reached over its
// network (no loopback), and its cloud-config carries no private key (no byo).
func remoteModes(offered []string) []string {
	return slices.DeleteFunc(slices.Clone(offered), func(m string) bool { return m == Loopback || m == "byo" })
}

// domainMode is the mode a domain given without --tls picks: acme when offered, else the first
// HTTPS mode offered; "" when there is none.
func domainMode(offered []string) string {
	return cmpFirst(offered, "acme", func(m string) bool { return m != Loopback })
}

// cmpFirst returns want when offered holds it, else the first offered mode ok accepts.
func cmpFirst(offered []string, want string, ok func(string) bool) string {
	if slices.Contains(offered, want) {
		return want
	}
	for _, m := range offered {
		if ok(m) {
			return m
		}
	}
	return ""
}

// byoCert is the operator's certificate and key, checked as a pair, and the CA it chains to when
// one was given.
type byoCert struct {
	cert, key, ca []byte
	leaf          *x509.Certificate
}

// loadCert reads --cert and --key, asking for the paths nothing gave. The form checks each file as
// it is asked, and opens again, saying why, when the two are not a pair.
func (in *Installer) loadCert(ctx context.Context) (*byoCert, error) {
	o := &in.O
	if (o.Cert == "") != (o.Key == "") || (o.Cert == "" && o.Prompt == nil) {
		return nil, errors.New("--tls byo needs --cert FILE and --key FILE: the certificate chain and its private key, PEM")
	}
	if o.Cert != "" {
		c, err := readPair(o.Cert, o.Key)
		if err != nil {
			return nil, fmt.Errorf("--cert and --key: %w", err)
		}
		return c, in.checkChain(c)
	}
	// What the form asks stays this run's, never a flag: going back to the mode asks it again.
	f := Form{
		Questions: []Question{
			{Title: "Certificate chain (PEM file)", Key: "--cert", Required: true, Check: func(v string) error {
				_, _, err := readLeaf(expandHome(v))
				return err
			}},
			{Title: "Its private key (PEM file)", Key: "--key", Required: true, Check: func(v string) error {
				raw, err := os.ReadFile(expandHome(v))
				if err == nil && !strings.Contains(string(raw), "PRIVATE KEY-----") {
					err = fmt.Errorf("%s holds no PEM private key", v)
				}
				return err
			}},
		},
		Defaults: func(map[string]string) map[string]string { return nil },
	}
	for {
		a, err := o.Prompt.Ask(ctx, f)
		if err != nil {
			return nil, err
		}
		c, err := readPair(expandHome(a["--cert"]), expandHome(a["--key"]))
		if err == nil {
			err = in.checkChain(c)
		}
		if err == nil {
			return c, nil
		}
		f.Answers, f.Note = a, fmt.Sprintf("These two files are not a certificate and its key (%v): change either.", err)
	}
}

// checkChain checks a byo certificate's chain: to --ca when given, else to the host's trusted CAs.
func (in *Installer) checkChain(c *byoCert) error {
	if in.O.CA != "" && in.O.CA != noCA {
		return in.readCA(c)
	}
	if err := in.chainComplete(c); err != nil {
		return fmt.Errorf("--cert: %w", err)
	}
	return nil
}

// noCA is --ca's value that publishes no CA: a re-run moving a byo install to a certificate of a
// public CA.
const noCA = "none"

// readCA reads --ca, when given, into c: PEM certificates, one of which c's chain ends at.
func (in *Installer) readCA(c *byoCert) error {
	if in.O.CA == "" || in.O.CA == noCA {
		return nil
	}
	raw, err := os.ReadFile(in.O.CA)
	if err != nil {
		return fmt.Errorf("--ca: %w", err)
	}
	if err := chainsTo(c, raw); err != nil {
		return fmt.Errorf("--ca: %w", err)
	}
	c.ca = raw
	return nil
}

// chainsTo checks that c's chain ends at one of the PEM certificates ca holds.
func chainsTo(c *byoCert, ca []byte) error {
	roots, inter := x509.NewCertPool(), x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return errors.New("it holds no PEM certificate")
	}
	inter.AppendCertsFromPEM(c.cert)
	if _, err := c.leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return fmt.Errorf("the certificate does not chain to it: %w", err)
	}
	return nil
}

// readLeaf reads a PEM certificate chain and returns it and its first certificate, refused when
// expired.
func readLeaf(path string) ([]byte, *x509.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	b, _ := pem.Decode(raw)
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("%s holds no PEM certificate", path)
	}
	leaf, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return nil, nil, err
	}
	if time.Now().After(leaf.NotAfter) {
		return nil, nil, fmt.Errorf("the certificate expired on %s", leaf.NotAfter.Format(time.DateOnly))
	}
	return raw, leaf, nil
}

// readPair reads a certificate chain and its private key, checked as a pair.
func readPair(certPath, keyPath string) (*byoCert, error) {
	cert, leaf, err := readLeaf(certPath)
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	if _, err := tls.X509KeyPair(cert, key); err != nil {
		return nil, err
	}
	return &byoCert{cert: cert, key: key, leaf: leaf}, nil
}

// writeCert copies a byo certificate and key into a tls/ directory this run creates, readable by
// the owner only (the front runs as root); a failure removes it again.
func (in *Installer) writeCert() (err error) {
	if in.byo == nil {
		return nil
	}
	dir := in.path(TLSDir)
	if err := os.Mkdir(dir, 0o700); err != nil { // tlsFree refused one already there
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	if err := writeCertFiles(dir, in.byo); err != nil {
		return err
	}
	in.O.logf("copied the certificate into %s", dir)
	return nil
}

// writeCertFiles writes c into dir, readable by the owner only: cert.pem, key.pem, and ca.pem when
// c has a CA (removed when it has none).
func writeCertFiles(dir string, c *byoCert) error {
	for name, data := range map[string][]byte{"cert.pem": c.cert, "key.pem": c.key, "ca.pem": c.ca} {
		p := filepath.Join(dir, name)
		if data == nil {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if err := writeFile(p, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// writeFile writes a file; tests replace it to fail a write.
var writeFile = os.WriteFile

// tlsFree refuses, for a byo install, a directory that already holds a tls/: the install copies
// the certificate there and owns it.
func (in *Installer) tlsFree(mode string) error {
	if mode != "byo" {
		return nil
	}
	if _, err := os.Lstat(in.path(TLSDir)); err == nil {
		return fmt.Errorf("%s already holds a %s/, where the install copies its certificate: move it (keep --cert and --key elsewhere) or choose another --dir", in.O.Dir, TLSDir)
	}
	return nil
}

var hostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// checkDomain accepts a lowercase DNS name of two labels or more whose last label is not all
// digits (no IP address): the front routes by it and it is spliced into the Caddyfile.
func checkDomain(v string) error {
	labels := strings.Split(v, ".")
	if len(v) > 253 || len(labels) < 2 {
		return fmt.Errorf("%q is not a domain name (lowercase, like app.example.com)", v)
	}
	for _, l := range labels {
		if !hostLabel.MatchString(l) {
			return fmt.Errorf("%q is not a domain name (lowercase, like app.example.com)", v)
		}
	}
	if _, err := strconv.Atoi(labels[len(labels)-1]); err == nil {
		return fmt.Errorf("%q is an address, not a domain name", v)
	}
	return nil
}

var emailRe = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$`)

// checkEmail accepts no address, or one the Caddyfile carries as is.
func checkEmail(v string) error {
	if v != "" && !emailRe.MatchString(v) {
		return fmt.Errorf("%q is not an email address", v)
	}
	return nil
}

// domainCheck checks a route's domain, and that a byo certificate covers it.
func (in *Installer) domainCheck(v string) error {
	if err := checkDomain(v); err != nil {
		return err
	}
	if in.byo != nil {
		if err := in.byo.leaf.VerifyHostname(v); err != nil {
			return fmt.Errorf("the certificate does not cover %s", v)
		}
	}
	return nil
}

// The token forms the front's Cloudflare plugin accepts: a classic API token, or a user (cfut_) or
// account (cfat_) one. The plugin refuses any other, and its error prints the token.
var cloudflareTokens = []*regexp.Regexp{
	regexp.MustCompile(`^[A-Za-z0-9_-]{35,50}$`),
	regexp.MustCompile(`^cf(ut|at)_[A-Za-z0-9_-]{32,256}$`),
}

// checkCloudflareToken accepts a token the front's plugin accepts.
func checkCloudflareToken(v string) error {
	for _, re := range cloudflareTokens {
		if re.MatchString(v) {
			return nil
		}
	}
	return errors.New("this is not a Cloudflare API token (35 to 50 letters, digits, - or _; or cfut_ / cfat_ and at least 32 more)")
}

// frontInputs are the inputs a TLS mode adds before the project's: each route's domain the
// project does not declare as an input itself, then the ACME contact, then the Cloudflare token.
func (in *Installer) frontInputs(mode string) []input {
	if mode == Loopback {
		return nil
	}
	var out []input
	for _, r := range in.R.Spec.Front.Routes {
		if _, ok := in.R.Spec.Input(r.DomainInput); !ok {
			out = append(out, input{spec.Input{Key: r.DomainInput, Prompt: "Domain for " + r.Name, Required: true}, in.domainCheck, true})
		}
	}
	if mode == "acme" || mode == "dns-cloudflare" {
		out = append(out, input{spec.Input{Key: ACMEEmailVar, Prompt: "Email Let's Encrypt writes to before a certificate expires (optional)"}, checkEmail, false})
	}
	if mode == "dns-cloudflare" {
		out = append(out, input{spec.Input{Key: CloudflareTokenVar, Prompt: "Cloudflare API token (Zone DNS Edit on each domain's zone)", Required: true, Secret: true}, checkCloudflareToken, false})
	}
	return out
}

// publicHTTPSPort is the HTTPS port clients reach: with acme always 443, which its challenge
// needs public (moved host ports sit behind a forward); otherwise — dns-cloudflare's DNS challenge
// needs no port — the host port.
func publicHTTPSPort(mode, hostPort string) string {
	if mode == "acme" {
		return "443"
	}
	return hostPort
}

const hsts = `header Strict-Transport-Security "max-age=31536000; includeSubDomains"`

// tlsCaddyfile serves each route on its domain over HTTPS, with one certificate source and no
// fallback: a failed issuance shows, rather than an untrusted certificate. dns-cloudflare proves
// each domain to Let's Encrypt through Cloudflare's DNS, with the token from the front's
// environment. HSTS is sent except
// with the local CA, which browsers do not trust. The front listens on the host's port numbers,
// so its ACME challenge and its redirects to HTTPS use them.
func tlsCaddyfile(f spec.Front, mode string, domains map[string]string, email string, httpPort, httpsPort string, get func(string) string) string {
	public := publicHTTPSPort(mode, httpsPort)
	var b strings.Builder
	b.WriteString("# Written by the installer from the project's front routes; rewritten on every run.\n")
	// HTTP/1.1 and HTTP/2 only: HTTP/3 would need UDP published, probed and forwarded too.
	fmt.Fprintf(&b, "{\n\tadmin off\n\tpersist_config off\n\thttp_port %s\n\thttps_port %s\n\tservers {\n\t\tprotocols h1 h2\n\t}\n", httpPort, httpsPort)
	switch mode {
	case "acme", "dns-cloudflare":
		if mode == "acme" {
			b.WriteString("\tcert_issuer acme\n")
		} else {
			b.WriteString("\tcert_issuer acme {\n\t\tdns cloudflare {env.CLOUDFLARE_API_TOKEN}\n\t}\n")
		}
		if email != "" {
			fmt.Fprintf(&b, "\temail %s\n", email)
		}
	case "internal":
		b.WriteString("\tlocal_certs\n")
	}
	b.WriteString("}\n")
	for _, r := range f.Routes {
		fmt.Fprintf(&b, "\n# %s\n%s {\n", r.Name, domains[r.Name])
		if mode == "byo" {
			fmt.Fprintf(&b, "\ttls /etc/caddy/%s/cert.pem /etc/caddy/%s/key.pem\n", TLSDir, TLSDir)
		}
		if mode != "internal" {
			fmt.Fprintf(&b, "\t%s\n", hsts)
		}
		routeBody(&b, r, get)
		b.WriteString("}\n")
		// Caddy's own redirect leaves out the port it listens on: a public port other than 443 is
		// named here.
		if public != "443" {
			fmt.Fprintf(&b, "http://%s {\n\tredir https://%s:%s{uri} 308\n}\n", domains[r.Name], domains[r.Name], public)
		}
	}
	return b.String()
}
