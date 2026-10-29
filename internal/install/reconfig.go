package install

import (
	"cmp"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/norvexlabs/stowage/internal/spec"
)

// changes is env with what a re-run was given applied: --set and --answers (and --domains) over
// the recorded inputs, --ports in loopback mode or --http-port and --https-port in a TLS mode over
// the recorded ports, and what derives from them — each route's URL —
// computed again. The environment never changes a recorded value. It returns the new file, which
// of its keys hold secrets, and where the front listens with only the ports the install does not
// hold already; next is nil when nothing changes. Secrets the install generated are kept.
func (in *Installer) changes(env *EnvFile) (next *EnvFile, secrets map[string]bool, probe publish, err error) {
	o, s := &in.O, in.R.Spec
	in.byo = nil // a domain is checked against the certificate this run decides on, below
	was, _ := env.Get("STOWAGE_TLS_MODE")
	mode, err := in.rerunMode(was)
	if err != nil {
		return nil, nil, publish{}, err
	}
	if mode == Loopback && len(o.Domains) > 0 {
		return nil, nil, publish{}, errors.New("--domains: this install is in loopback mode, which serves no domain (a TLS mode is chosen by a new install)")
	}
	if err := in.applyDomains(); err != nil {
		return nil, nil, publish{}, err
	}
	if err := in.knownInputs(mode); err != nil {
		return nil, nil, publish{}, err
	}
	// A domain is checked only when it changes: a value as recorded is no change.
	var domainGiven []string
	for _, r := range s.Front.Routes {
		v, ok := o.Set[r.DomainInput]
		if !ok {
			v, ok = o.Answers[r.DomainInput]
		}
		if was, _ := env.Get(r.DomainInput); ok && v != was && !slices.Contains(domainGiven, r.DomainInput) {
			domainGiven = append(domainGiven, r.DomainInput)
		}
	}
	switch {
	case len(domainGiven) > 0 && mode == Loopback:
		return nil, nil, publish{}, fmt.Errorf("%s: this install is in loopback mode, which serves no domain (a TLS mode is chosen by a new install)", strings.Join(domainGiven, ", "))
	case in.newCert != nil:
		// Every route's domain is checked against it below.
	case len(domainGiven) > 0 && mode == "byo":
		// A domain the re-run gives must be one the install's certificate covers.
		_, leaf, err := readLeaf(filepath.Join(o.Dir, TLSDir, "cert.pem"))
		if err != nil {
			return nil, nil, publish{}, fmt.Errorf("the install's certificate: %w", err)
		}
		in.byo = &byoCert{leaf: leaf}
		defer func() { in.byo = nil }()
	}
	next = NewEnvFile()
	for _, k := range env.Keys() {
		v, _ := env.Get(k)
		next.Set(k, v)
	}
	secrets = map[string]bool{}
	var bad []string
	for _, i := range in.inputList(mode) {
		secrets[i.Key] = i.Secret
		v, ok := o.Set[i.Key]
		if !ok {
			v, ok = o.Answers[i.Key]
		}
		if !ok {
			continue
		}
		if was, _ := env.Get(i.Key); v == was {
			continue
		}
		if v == "" && i.Advanced {
			next.Delete(i.Key) // back to the compose file's default
			continue
		}
		switch err := i.checkValue(v); {
		case v == "" && i.Required:
			bad = append(bad, fmt.Sprintf("%s is required", i.Key))
		case err != nil:
			bad = append(bad, fmt.Sprintf("%s: %v", i.Key, err))
		default:
			next.Set(i.Key, v)
		}
	}
	// A new mode: the inputs it requires are given, the front's inputs it does not read go, and a
	// new certificate covers every domain.
	used := map[string]bool{}
	for _, i := range in.inputList(mode) {
		used[i.Key] = true
		if v, _ := next.Get(i.Key); v == "" && i.Required {
			bad = append(bad, fmt.Sprintf("%s is required (--set %s=…)", i.Key, i.Key))
		}
	}
	if mode != was {
		next.Set("STOWAGE_TLS_MODE", mode)
		for _, i := range in.frontInputs(was) {
			if !used[i.Key] {
				next.Delete(i.Key)
			}
		}
	}
	if in.newCert != nil {
		for _, r := range s.Front.Routes {
			if d, _ := next.Get(r.DomainInput); in.newCert.leaf.VerifyHostname(d) != nil {
				bad = append(bad, fmt.Sprintf("the certificate does not cover %s (%s)", d, r.Name))
			}
		}
	}
	if len(bad) > 0 {
		return nil, nil, publish{}, errors.New(strings.Join(bad, "; "))
	}
	for _, sec := range in.secrets() {
		secrets[sec.Key] = true
	}

	// Where the front listens: the recorded ports, with those given over them.
	held := map[int]bool{}
	recordedPort := func(k string) int {
		v, _ := env.Get(k)
		p, _ := strconv.Atoi(v)
		held[p] = true
		return p
	}
	var pub publish
	switch {
	case !s.HasFront():
		// No front: no port of its own (frontFlags refused the flags that would set one).
	case mode == Loopback:
		if o.HTTPPort != 0 || o.HTTPSPort != 0 {
			return nil, nil, publish{}, errors.New("--http-port and --https-port are for TLS modes: this install is in loopback mode, which takes --ports")
		}
		ports := map[string]int{}
		for _, r := range s.Front.Routes {
			ports[r.Name] = recordedPort(PortVar(r.Name))
		}
		addr, _ := env.Get("STOWAGE_LOOPBACK_ADDRESS")
		p, err := in.ports(ports, o.Ports)
		if err != nil {
			return nil, nil, publish{}, err
		}
		pub = publish{addr: addr, ports: p}
	default:
		if len(o.Ports) > 0 {
			return nil, nil, publish{}, fmt.Errorf("--ports is for loopback mode: this install's front serves %s, on --http-port and --https-port", mode)
		}
		if pub, err = tlsPublish(cmp.Or(o.HTTPPort, recordedPort(HTTPPortVar)), cmp.Or(o.HTTPSPort, recordedPort(HTTPSPortVar))); err != nil {
			return nil, nil, publish{}, err
		}
	}
	if err := in.l4Clash(pub); err != nil {
		return nil, nil, publish{}, err
	}
	domains, err := in.routeDomains(mode, func(k string) string { v, _ := next.Get(k); return v })
	if err != nil {
		return nil, nil, publish{}, err
	}
	in.frontVars(next, mode, pub, domains)
	in.tuneVar(next)
	in.profilesVar(next)
	in.l4Vars(next)
	if err := next.Check(); err != nil {
		return nil, nil, publish{}, err
	}

	same := len(next.Keys()) == len(env.Keys())
	for _, k := range next.Keys() {
		a, _ := env.Get(k)
		if b, _ := next.Get(k); a != b {
			same = false
		}
	}
	if same {
		return nil, nil, publish{}, nil
	}
	probe = publish{addr: pub.addr, all: pub.all, ports: map[string]int{}}
	for k, p := range pub.ports {
		if !held[p] {
			probe.ports[k] = p
		}
	}
	return next, secrets, probe, nil
}

// rerunMode is the TLS mode a re-run moves the install to from was: --tls, else byo for --cert and
// --key, else was. A move between loopback and a TLS mode is a new install. A new byo
// certificate (--cert and --key, or --ca alone with the install's own) is checked and kept in
// newCert.
func (in *Installer) rerunMode(was string) (string, error) {
	o := &in.O
	if !in.R.Spec.HasFront() {
		return "", in.frontFlags()
	}
	mode := o.TLS
	if mode == "" && (o.Cert != "" || o.Key != "") {
		mode = "byo"
	}
	mode = cmp.Or(mode, was)
	switch {
	case mode != was && !slices.Contains(in.R.Spec.Front.TLSModes, mode):
		return "", fmt.Errorf("--tls %s: %s offers %s", mode, in.R.Spec.Name, strings.Join(in.R.Spec.Front.TLSModes, ", "))
	case mode != was && (mode == Loopback || was == Loopback):
		return "", fmt.Errorf("--tls %s: this install serves %s; moving between loopback mode and a TLS mode is a new install", mode, was)
	case (o.Cert != "" || o.Key != "") && mode != "byo":
		return "", fmt.Errorf("--cert and --key are for --tls byo, not %s", mode)
	}
	if err := in.caFlag(mode); err != nil {
		return "", err
	}
	in.newCert = nil
	switch {
	case mode != "byo":
	case o.Cert != "" || o.Key != "" || was != "byo":
		if o.Cert == "" || o.Key == "" {
			return "", errors.New("--tls byo needs --cert FILE and --key FILE: the certificate chain and its private key, PEM")
		}
		c, err := readPair(o.Cert, o.Key)
		if err != nil {
			return "", fmt.Errorf("--cert and --key: %w", err)
		}
		if err := in.readCA(c); err != nil {
			return "", err
		}
		if err := in.keepCA(c, was); err != nil {
			return "", err
		}
		in.newCert = c
	case o.CA != "":
		dir := filepath.Join(o.Dir, TLSDir)
		c, err := readPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
		if err != nil {
			return "", fmt.Errorf("the install's certificate: %w", err)
		}
		if err := in.readCA(c); err != nil {
			return "", err
		}
		in.newCert = c
	}
	return mode, nil
}

// keepCA gives a new certificate of a byo install given no --ca the install's CA, which it must
// chain to: a renewal under the same CA keeps it published.
func (in *Installer) keepCA(c *byoCert, was string) error {
	if c.ca != nil || was != "byo" || in.O.CA == noCA {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(in.O.Dir, TLSDir, "ca.pem"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if chainsTo(c, raw) != nil {
		return fmt.Errorf("the certificate does not chain to the install's CA (%s/ca.pem): give its own with --ca FILE, or --ca none to publish none", TLSDir)
	}
	c.ca = raw
	return nil
}

// tlsPrev keeps the byo files a re-run replaced until its stack came up.
const tlsPrev = TLSDir + ".prev"

// writeNewCert puts a re-run's new byo certificate in tls/: written whole into a new directory,
// which takes tls/'s place by a rename; the replaced one is kept as tls.prev/ until the stack
// came up (removed by certDone), so a failed run leaves the operator both.
func (in *Installer) writeNewCert() error {
	if in.newCert == nil {
		return nil
	}
	dir, next, prev := in.path(TLSDir), in.path(TLSDir+".new"), in.path(tlsPrev)
	if err := os.RemoveAll(next); err != nil {
		return err
	}
	if err := os.Mkdir(next, 0o700); err != nil {
		return err
	}
	if err := writeCertFiles(next, in.newCert); err != nil {
		_ = os.RemoveAll(next)
		return err
	}
	// tls.prev/ already there holds the last certificate that came up (an earlier run failed):
	// it is kept, and the failed one goes.
	_, err := os.Stat(prev)
	keptPrev := err == nil
	if _, err := os.Stat(dir); err == nil {
		if keptPrev {
			err = os.RemoveAll(dir)
		} else {
			err = os.Rename(dir, prev)
		}
		if err != nil {
			return err
		}
	}
	if err := os.Rename(next, dir); err != nil {
		return err
	}
	in.O.logf("copied the certificate into %s", dir)
	return nil
}

// certDone removes tls.prev/ once the stack came up (any run: re-run, rotation, reset-db,
// upgrade), with whatever certificate tls/ holds.
func (in *Installer) certDone() {
	_ = os.RemoveAll(in.path(tlsPrev))
}

// logChanges logs each value next changes from env, a secret's redacted, and each it removes.
func (in *Installer) logChanges(env, next *EnvFile, secrets map[string]bool) {
	for _, k := range env.Keys() {
		if _, ok := next.Get(k); !ok {
			in.O.logf("removing %s", k)
		}
	}
	for _, k := range next.Keys() {
		a, _ := env.Get(k)
		b, _ := next.Get(k)
		if a == b {
			continue
		}
		if secrets[k] {
			a, b = redact(a), redact(b)
		}
		in.O.logf("changing %s: %s → %s", k, cmp.Or(a, `""`), cmp.Or(b, `""`))
	}
}

func redact(v string) string {
	if v == "" {
		return v
	}
	return "<redacted>"
}

// routeDomains is each route's domain in a TLS mode, as get reads its input. Routes on one
// domain are told apart by their paths: two serving the same path of it are refused, and so is a
// route asking for a client certificate on a domain another route has, as a certificate is asked
// for a whole domain. Loopback mode serves none.
func (in *Installer) routeDomains(mode string, get func(string) string) (map[string]string, error) {
	domains := map[string]string{}
	if mode == Loopback {
		return domains, nil
	}
	routes := in.R.Spec.Front.Routes
	seen := map[string]string{} // domain and path → route
	for _, r := range routes {
		d := get(r.DomainInput)
		// The front matches paths whatever their case.
		if other, dup := seen[d+" "+strings.ToLower(r.Path)]; dup {
			what := "the domain " + d
			if r.Path != "" {
				what = d + r.Path
			}
			return nil, fmt.Errorf("%s and %s both serve %s: routes of one domain differ by their path", other, r.Name, what)
		}
		seen[d+" "+strings.ToLower(r.Path)], domains[r.Name] = r.Name, d
	}
	for _, r := range routes {
		if i := slices.IndexFunc(routes, func(o spec.Route) bool { return o.Name != r.Name && domains[o.Name] == domains[r.Name] }); r.ClientAuth && i >= 0 {
			return nil, fmt.Errorf("%s asks for a client certificate on %s, which %s has too: a certificate is asked for a whole domain", r.Name, domains[r.Name], routes[i].Name)
		}
	}
	return domains, nil
}

// frontVars records where the front listens and each route's URL: in loopback mode each route's
// port on pub's address; in a TLS mode the host's HTTP and HTTPS ports and each route's domain;
// and --ipv6 when given.
func (in *Installer) frontVars(env *EnvFile, mode string, pub publish, domains map[string]string) {
	if in.O.IPv6 != nil {
		env.Set(IPv6Var, strconv.FormatBool(*in.O.IPv6))
	} else if _, ok := env.Get(IPv6Var); !ok {
		env.Set(IPv6Var, "false") // always recorded: the compose file may read it
	}
	if !in.R.Spec.HasFront() {
		return
	}
	if mode != Loopback {
		env.Set(HTTPPortVar, strconv.Itoa(pub.ports["http"]))
		env.Set(HTTPSPortVar, strconv.Itoa(pub.ports["https"]))
	}
	for _, r := range in.R.Spec.Front.Routes {
		routeVars(env, mode, r, pub, domains[r.Name])
	}
}

// routeVars records route r's URL as the front serves it, and in loopback mode its port: in
// loopback mode its port on pub's address; in a TLS mode its domain on the public HTTPS port.
func routeVars(env *EnvFile, mode string, r spec.Route, pub publish, domain string) {
	if mode == Loopback {
		env.Set(PortVar(r.Name), strconv.Itoa(pub.ports[r.Name]))
		env.Set(spec.URLVar(r.Name), "http://"+urlHost(pub.addr)+":"+strconv.Itoa(pub.ports[r.Name])+r.Path)
		return
	}
	port := ""
	if p := publicHTTPSPort(mode, strconv.Itoa(pub.ports["https"])); p != "443" {
		port = ":" + p
	}
	env.Set(spec.URLVar(r.Name), "https://"+domain+port+r.Path)
}

// routePaths gives each route's recorded URL the release's domain and path, as the front serves
// them: in a TLS mode its domain input's recorded value and the public HTTPS port; in loopback
// mode the address and port the install chose, with the release's path.
func (in *Installer) routePaths(env *EnvFile, mode string) error {
	get := func(k string) string { v, _ := env.Get(k); return v }
	port := ""
	if p := publicHTTPSPort(mode, get(HTTPSPortVar)); p != "443" {
		port = ":" + p
	}
	for _, r := range in.R.Spec.Front.Routes {
		k := spec.URLVar(r.Name)
		v, ok := env.Get(k)
		if !ok || v == "" {
			continue
		}
		if mode != Loopback {
			env.Set(k, "https://"+get(r.DomainInput)+port+r.Path)
			continue
		}
		u, err := url.Parse(v)
		if err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
		u.Path, u.RawPath = r.Path, ""
		env.Set(k, u.String())
	}
	return nil
}

// acmeForward reminds, when the front's ports change from was's (nil on a first install), that
// Let's Encrypt's HTTP challenge reaches ports 80 and 443.
func (in *Installer) acmeForward(was, env *EnvFile) {
	get := func(f *EnvFile, k string) string { v, _ := f.Get(k); return v }
	http, https := get(env, HTTPPortVar), get(env, HTTPSPortVar)
	if get(env, "STOWAGE_TLS_MODE") != "acme" || (http == "80" && https == "443") {
		return
	}
	if was != nil && get(was, HTTPPortVar) == http && get(was, HTTPSPortVar) == https {
		return
	}
	in.O.logf("Let's Encrypt reaches each domain on ports 80 and 443: forward them to %s and %s here; IPv6 forwards no port, so a domain whose AAAA record names this host is reached on 80 and 443 themselves", http, https)
}
