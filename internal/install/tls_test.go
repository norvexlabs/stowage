package install

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/spec"
)

func tlsDomains(o *Options) {
	o.Domains = map[string]string{"app": "notes.example.com", "api": "api.example.com"}
}

func (e *env) file(name string) string {
	e.t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.dir, name))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(raw)
}

func TestDomainsServeHTTPSFromLetsEncryptWithTheSafeDefaults(t *testing.T) {
	e := newEnv(t)
	probed := map[string]int{}
	if err := e.install(func(o *Options) {
		tlsDomains(o)
		o.Set = map[string]string{ACMEEmailVar: "ops@example.com"}
		o.PortFree = func(h string, p int) bool { probed[fmt.Sprintf("%s:%d", h, p)]++; return true }
	}); err != nil {
		t.Fatal(err)
	}
	f := e.readEnv()
	for k, want := range map[string]string{"STOWAGE_TLS_MODE": "acme", "STOWAGE_URL_APP": "https://notes.example.com", "STOWAGE_URL_API": "https://api.example.com",
		"APP_DOMAIN": "notes.example.com", "API_DOMAIN": "api.example.com", ACMEEmailVar: "ops@example.com", HTTPPortVar: "80", HTTPSPortVar: "443",
		"SECURE_COOKIES": "true", "STOWAGE_LOOPBACK_ADDRESS": "127.0.0.1"} {
		if got := get(f, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	for _, k := range []string{"STOWAGE_PORT_APP"} {
		if _, ok := f.Get(k); ok {
			t.Errorf("%s recorded behind TLS", k)
		}
	}
	// Every address of the host, before and after the inputs.
	for _, h := range append(hostAddrs(), "127.0.0.1") {
		for _, p := range []int{80, 443} {
			if probed[fmt.Sprintf("%s:%d", h, p)] < 2 {
				t.Errorf("%s:%d probed %d times: %v", h, p, probed[fmt.Sprintf("%s:%d", h, p)], probed)
			}
		}
	}

	cf := e.file(Caddyfile)
	if strings.Contains(cf, "redir") {
		t.Errorf("a redirect of its own on port 443:\n%s", cf)
	}
	for _, want := range []string{"\thttp_port 80\n\thttps_port 443\n\tservers {\n\t\tprotocols h1 h2\n\t}\n\tcert_issuer acme\n\temail ops@example.com\n", "notes.example.com {\n\t" + hsts + "\n\treverse_proxy web:8080\n}", "api.example.com {\n\t" + hsts + "\n"} {
		if !strings.Contains(cf, want) {
			t.Errorf("Caddyfile lacks %q:\n%s", want, cf)
		}
	}
	if fc := e.file(FrontCompose); !strings.Contains(fc, "${STOWAGE_HTTP_PORT}:${STOWAGE_HTTP_PORT}") || !strings.Contains(fc, "${STOWAGE_HTTPS_PORT}:${STOWAGE_HTTPS_PORT}") || strings.Contains(fc, "LOOPBACK") {
		t.Errorf("front compose:\n%s", fc)
	}
	if !strings.Contains(e.out.String(), "https://notes.example.com") {
		t.Errorf("portal: %s", e.out)
	}
}

// dns-cloudflare proves the domains through Cloudflare's DNS: the token is required, recorded as
// a secret, and given to the front, not to the stack's services or the hook.
func TestCloudflareDNSGivesTheTokenToTheFrontNotTheStack(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.r.Spec.Front.TLSModes = append(e.r.Spec.Front.TLSModes, "dns-cloudflare")
	cf := func(o *Options) { tlsDomains(o); o.TLS = "dns-cloudflare" }
	if err := e.install(cf); err == nil || !strings.Contains(err.Error(), CloudflareTokenVar) {
		t.Fatalf("no token: %v", err)
	}
	e.nothingWritten()
	for _, bad := range []string{strings.Repeat("a", 34), strings.Repeat("a", 51), strings.Repeat("a", 34) + " ", strings.Repeat("a", 39) + ".", "cfut_" + strings.Repeat("a", 257), "cfxt_" + strings.Repeat("a", 60)} {
		if err := e.install(func(o *Options) { cf(o); o.Set = map[string]string{CloudflareTokenVar: bad} }); err == nil || !strings.Contains(err.Error(), "not a Cloudflare API token") {
			t.Fatalf("token %q: %v", bad, err)
		}
		e.nothingWritten()
	}
	// A user or account token longer than a classic one (50) passes on its prefix, up to 256 more.
	for _, good := range []string{strings.Repeat("a", 35), strings.Repeat("-", 50), "cfat_" + strings.Repeat("a", 60), "cfut_" + strings.Repeat("a", 256)} {
		if err := checkCloudflareToken(good); err != nil {
			t.Errorf("token %q: %v", good, err)
		}
	}
	token := "cfut_" + strings.Repeat("T0k", 12)
	if err := e.install(func(o *Options) {
		cf(o)
		o.Set = map[string]string{CloudflareTokenVar: token, ACMEEmailVar: "ops@example.com"}
	}); err != nil {
		t.Fatal(err)
	}
	f := e.readEnv()
	for k, want := range map[string]string{"STOWAGE_TLS_MODE": "dns-cloudflare", CloudflareTokenVar: token, "STOWAGE_URL_APP": "https://notes.example.com"} {
		if got := get(f, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	caddy := e.file(Caddyfile)
	for _, want := range []string{"\tcert_issuer acme {\n\t\tdns cloudflare {env.CLOUDFLARE_API_TOKEN}\n\t}\n\temail ops@example.com\n", "notes.example.com {\n\t" + hsts + "\n"} {
		if !strings.Contains(caddy, want) {
			t.Errorf("Caddyfile lacks %q:\n%s", want, caddy)
		}
	}
	if strings.Contains(caddy, token) {
		t.Error("the token is written into the Caddyfile")
	}
	if fc := e.file(FrontCompose); !strings.Contains(fc, "CLOUDFLARE_API_TOKEN: ${"+CloudflareTokenVar+"}") {
		t.Errorf("front compose:\n%s", fc)
	}
	in := &Installer{R: e.r}
	if keys := in.recordedEnv(f); slices.Contains(keys, CloudflareTokenVar) {
		t.Errorf("a command provider receives the token: %v", keys)
	}

	// The DNS challenge needs no port: a moved HTTPS port is the one the URLs name.
	e = newEnv(t)
	e.r.Spec.Front.TLSModes = append(e.r.Spec.Front.TLSModes, "dns-cloudflare")
	if err := e.install(func(o *Options) {
		cf(o)
		o.Set = map[string]string{CloudflareTokenVar: token}
		o.HTTPPort, o.HTTPSPort = 8080, 8443
	}); err != nil {
		t.Fatal(err)
	}
	if u := get(e.readEnv(), "STOWAGE_URL_APP"); u != "https://notes.example.com:8443" {
		t.Errorf("URL %q", u)
	}
	if caddy := e.file(Caddyfile); !strings.Contains(caddy, "redir https://notes.example.com:8443{uri} 308") {
		t.Errorf("Caddyfile:\n%s", caddy)
	}
}

func TestTheModeFollowsWhatWasGiven(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*Options)
		want string // the recorded mode, or the error
	}{
		{"nothing given", nil, "none"},
		{"a domain given by --set", func(o *Options) {
			o.Set = map[string]string{"APP_DOMAIN": "a.example.com", "API_DOMAIN": "b.example.com"}
		}, "acme"},
		{"a domain in the answers file", func(o *Options) {
			o.Answers = map[string]string{"APP_DOMAIN": "a.example.com", "API_DOMAIN": "b.example.com"}
		}, "acme"},
		{"internal", func(o *Options) { tlsDomains(o); o.TLS = "internal" }, "internal"},
		{"a domain only in the environment", func(o *Options) {
			o.Getenv = func(k string) string {
				return map[string]string{"ADMIN_EMAIL": "a@x", "APP_DOMAIN": "a.example.com", "API_DOMAIN": "b.example.com"}[k]
			}
		}, "none"},
		{"--cert alone", func(o *Options) { tlsDomains(o); o.Cert, o.Key = "c", "k" }, "--cert and --key: open c"},
		{"loopback with a domain", func(o *Options) { tlsDomains(o); o.TLS = Loopback }, "loopback mode serves no domain"},
		{"a mode the project does not offer", func(o *Options) { tlsDomains(o); o.TLS = "dns-cloudflare" }, "offers acme, byo, internal, none"},
		{"no such route", func(o *Options) { o.Domains = map[string]string{"web": "a.example.com"} }, `"web" is no route`},
		{"--domains and --set disagree", func(o *Options) {
			tlsDomains(o)
			o.Set = map[string]string{"APP_DOMAIN": "other.example.com"}
		}, "disagree"},
		{"a domain without TLS", func(o *Options) { o.TLS = Loopback; o.Set = map[string]string{"APP_DOMAIN": "a.example.com"} }, "loopback mode serves no domain"},
		{"a domain missing", func(o *Options) { o.TLS = "acme"; o.Set = map[string]string{"APP_DOMAIN": "a.example.com"} }, "API_DOMAIN"},
		{"--ports behind TLS", func(o *Options) { tlsDomains(o); o.Ports = map[string]int{"app": 9000} }, "--ports and --address are for loopback mode"},
		{"--https-port in loopback mode", func(o *Options) { o.HTTPSPort = 8443 }, "loopback mode takes --ports"},
		{"one port for both", func(o *Options) { tlsDomains(o); o.HTTPPort, o.HTTPSPort = 8443, 8443 }, "both 8443"},
		{"a port out of range", func(o *Options) { tlsDomains(o); o.HTTPSPort = 70000 }, "not a TCP port"},
		{"--cert outside byo", func(o *Options) { tlsDomains(o); o.TLS, o.Cert, o.Key = "acme", "c", "k" }, "--cert and --key are for --tls byo"},
		{"a given value .env cannot hold", func(o *Options) { o.Set = map[string]string{"ADMIN_EMAIL": "a\nb"} }, "ADMIN_EMAIL"},
		{"byo without a certificate", func(o *Options) { tlsDomains(o); o.TLS = "byo" }, "needs --cert FILE and --key FILE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			err := e.install(c.mod)
			if c.want == "none" || c.want == "acme" || c.want == "internal" {
				if err != nil {
					t.Fatal(err)
				}
				if got := get(e.readEnv(), "STOWAGE_TLS_MODE"); got != c.want {
					t.Fatalf("mode %q, want %q", got, c.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			e.nothingWritten()
		})
	}
}

func TestADomainOrAContactTheFrontCannotUseIsRefused(t *testing.T) {
	for _, bad := range []map[string]string{
		{"APP_DOMAIN": "Notes.example.com"}, {"APP_DOMAIN": "notes"}, {"APP_DOMAIN": "10.0.0.1"},
		{"APP_DOMAIN": "-a.example.com"}, {"APP_DOMAIN": "a b.example.com"}, {"APP_DOMAIN": "a.example.com {"},
		{"APP_DOMAIN": "api.example.com"}, // the api route's own
		{ACMEEmailVar: "ops"}, {ACMEEmailVar: "ops@example.com }"}, {ACMEEmailVar: "ops @example.com"},
	} {
		e := newEnv(t)
		err := e.install(func(o *Options) {
			o.TLS = "acme"
			o.Set = map[string]string{"APP_DOMAIN": "notes.example.com", "API_DOMAIN": "api.example.com"}
			for k, v := range bad {
				o.Set[k] = v
			}
		})
		if err == nil {
			t.Errorf("%v accepted", bad)
			continue
		}
		if len(e.docker.calls) != 0 && !strings.Contains(err.Error(), "both have the domain") {
			t.Errorf("%v refused after the host was checked: %v", bad, err)
		}
		e.nothingWritten()
	}
	// No contact is a contact left out, and the issuer is still Let's Encrypt alone.
	e := newEnv(t)
	if err := e.install(tlsDomains); err != nil {
		t.Fatal(err)
	}
	if cf := e.file(Caddyfile); strings.Contains(cf, "email") || !strings.Contains(cf, "cert_issuer acme") {
		t.Errorf("Caddyfile:\n%s", cf)
	}
}

func TestTheLocalCASendsNoHSTSAndOtherPortsReachTheURLs(t *testing.T) {
	e := newEnv(t)
	if err := e.install(func(o *Options) { tlsDomains(o); o.TLS = "internal"; o.HTTPPort, o.HTTPSPort = 8080, 8443 }); err != nil {
		t.Fatal(err)
	}
	cf := e.file(Caddyfile)
	if strings.Contains(cf, "Strict-Transport-Security") || !strings.Contains(cf, "\thttps_port 8443\n") || !strings.Contains(cf, "\tlocal_certs\n") || strings.Contains(cf, "acme") {
		t.Errorf("Caddyfile:\n%s", cf)
	}
	if !strings.Contains(cf, "http://notes.example.com {\n\tredir https://notes.example.com:8443{uri} 308\n}") {
		t.Errorf("no redirect to the moved port:\n%s", cf)
	}
	f := e.readEnv()
	if get(f, "STOWAGE_URL_APP") != "https://notes.example.com:8443" || get(f, HTTPPortVar) != "8080" {
		t.Errorf("URL %q, http port %q", get(f, "STOWAGE_URL_APP"), get(f, HTTPPortVar))
	}
	// A busy port names the flags that move it.
	e = newEnv(t)
	e.busy[443] = true
	if err := e.install(tlsDomains); err == nil || !strings.Contains(err.Error(), "443 (https)") || !strings.Contains(err.Error(), "--https-port") {
		t.Fatalf("err = %v", err)
	}
	e.nothingWritten()
}

// writeCert writes a self-signed certificate for names, valid until notAfter, and its key.
func writeCert(t *testing.T, notAfter time.Time, names ...string) (cert, key string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: notAfter.Add(-48 * time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(k)
	dir := t.TempDir()
	cert, key = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	_ = os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	return cert, key
}

func TestOwnCertificateIsCheckedAndCopiedIntoTheInstall(t *testing.T) {
	later := time.Now().Add(90 * 24 * time.Hour)
	cert, key := writeCert(t, later, "*.example.com")
	e := newEnv(t)
	byo := func(o *Options) { tlsDomains(o); o.TLS, o.Cert, o.Key = "byo", cert, key }
	if err := e.install(byo); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cert.pem", "key.pem"} {
		st, err := os.Stat(filepath.Join(e.dir, TLSDir, name))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", name, st, err)
		}
	}
	if cf := e.file(Caddyfile); !strings.Contains(cf, "notes.example.com {\n\ttls /etc/caddy/tls/cert.pem /etc/caddy/tls/key.pem\n\t"+hsts) {
		t.Errorf("Caddyfile:\n%s", cf)
	}
	if fc := e.file(FrontCompose); !strings.Contains(fc, "./tls:/etc/caddy/tls:ro") {
		t.Errorf("front compose:\n%s", fc)
	}
	// A re-run needs the copy, and changes nothing about TLS.
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	// A re-run changes a domain the certificate covers, and the front's ports.
	for name, c := range map[string]struct {
		mod  func(*Options)
		want string
	}{
		"uncovered":  {func(o *Options) { o.Domains = map[string]string{"app": "notes.example.org"} }, "APP_DOMAIN: the certificate does not cover notes.example.org"},
		"same":       {func(o *Options) { o.Domains = map[string]string{"app": "api.example.com"} }, "both have the domain api.example.com"},
		"--ports":    {func(o *Options) { o.Ports = map[string]int{"app": 9000} }, "--ports is for loopback mode"},
		"same ports": {func(o *Options) { o.HTTPPort = 443 }, "--http-port and --https-port are both 443"},
	} {
		if err := e.install(c.mod); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	e.busy[8080] = true
	if err := e.install(func(o *Options) { o.HTTPPort = 8080 }); err == nil || !strings.Contains(err.Error(), "8080 (http) already in use on this host: choose others with --http-port and --https-port") {
		t.Fatalf("a busy HTTP port: %v", err)
	}
	if err := e.install(func(o *Options) {
		o.Domains = map[string]string{"app": "app.example.com"}
		o.HTTPSPort = 8443
	}); err != nil {
		t.Fatal(err)
	}
	if f := e.readEnv(); get(f, "STOWAGE_URL_APP") != "https://app.example.com:8443" || get(f, "STOWAGE_URL_API") != "https://api.example.com:8443" || get(f, HTTPSPortVar) != "8443" || get(f, HTTPPortVar) != "80" {
		t.Fatalf("after the re-run: %s %s", get(f, "STOWAGE_URL_APP"), get(f, "STOWAGE_URL_API"))
	}
	if cf := e.file(Caddyfile); !strings.Contains(cf, "https_port 8443") || !strings.Contains(cf, "app.example.com {") {
		t.Errorf("Caddyfile after the re-run:\n%s", cf)
	}
	// An expired certificate never stops a re-run given no domain: the VM's boot is one.
	installed := filepath.Join(e.dir, TLSDir, "cert.pem")
	kept, _ := os.ReadFile(installed)
	old, _ := writeCert(t, time.Now().Add(-time.Hour), "*.example.com")
	raw, _ := os.ReadFile(old)
	_ = os.WriteFile(installed, raw, 0o600)
	if err := e.install(nil); err != nil {
		t.Fatalf("a re-run with an expired certificate: %v", err)
	}
	if err := e.install(func(o *Options) { o.Domains = map[string]string{"app": "app.example.com"} }); err != nil {
		t.Fatalf("a re-run giving the recorded domain, with an expired certificate: %v", err)
	}
	if err := e.install(func(o *Options) { o.Domains = map[string]string{"app": "notes.example.com"} }); err == nil || !strings.Contains(err.Error(), "the install's certificate: the certificate expired") {
		t.Fatalf("a domain checked against an expired certificate: %v", err)
	}
	_ = os.WriteFile(installed, kept, 0o600)
	_ = os.Remove(filepath.Join(e.dir, TLSDir, "key.pem"))
	if err := e.install(nil); err == nil || !strings.Contains(err.Error(), "certificate is missing") {
		t.Fatalf("a re-run without the key: %v", err)
	}

	other, otherKey := writeCert(t, later, "notes.example.com")
	_, strangerKey := writeCert(t, later, "*.example.com")
	expired, expiredKey := writeCert(t, time.Now().Add(-time.Hour), "*.example.com")
	for name, c := range map[string]struct{ cert, key, want string }{
		"a domain it does not cover": {other, otherKey, "does not cover api.example.com"},
		"another certificate's key":  {cert, strangerKey, "private key does not match"},
		"an expired certificate":     {expired, expiredKey, "expired"},
		"a missing file":             {cert, key + ".gone", "--key"},
	} {
		e := newEnv(t)
		if err := e.install(func(o *Options) { tlsDomains(o); o.TLS, o.Cert, o.Key = "byo", c.cert, c.key }); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
		e.nothingWritten()
	}
}

func TestAProjectServedOnlyOverHTTPSAsksItsDomains(t *testing.T) {
	e := newEnv(t)
	e.r.Spec.Front.TLSModes = []string{"acme"}
	if err := e.install(nil); err == nil || !strings.Contains(err.Error(), "HTTPS only: give --domains") {
		t.Fatalf("no terminal: %v", err)
	}
	p := &fakePrompter{answers: map[string]string{"APP_DOMAIN": "notes.example.com", "API_DOMAIN": "api.example.com"}}
	if err := e.install(func(o *Options) { o.Prompt = p }); err != nil {
		t.Fatal(err)
	}
	if len(p.forms) != 1 || p.forms[0].Questions[0].Key != "APP_DOMAIN" || p.forms[0].Back != "" || get(e.readEnv(), "STOWAGE_TLS_MODE") != "acme" {
		t.Fatalf("forms %+v", p.forms)
	}
}

func TestTheFormAsksTheModeAndCanGoBackToIt(t *testing.T) {
	e := newEnv(t)
	delete(e.vars, "ADMIN_EMAIL")
	backs := 0
	p := &fakePrompter{answers: map[string]string{"--tls": Loopback, "ADMIN_EMAIL": "a@x"}}
	p.onAsk = func() {
		f := p.forms[len(p.forms)-1]
		switch {
		case f.Questions[0].Key == "--tls" && backs == 1:
			// Back at the mode: the choice made before is where it starts.
			if f.Answers["--tls"] != Loopback {
				t.Errorf("the mode question starts from %q", f.Answers["--tls"])
			}
			p.answers = map[string]string{"--tls": "internal", "APP_DOMAIN": "notes.example.com", "API_DOMAIN": "api.example.com"}
			p.err = nil
		case f.Review && backs == 0:
			if f.Back == "" {
				t.Error("the review cannot go back to the mode")
			}
			backs++
			p.err = ErrBack
		default:
			p.err = nil
		}
	}
	if err := e.install(func(o *Options) { o.Prompt = p }); err != nil {
		t.Fatal(err)
	}
	var asked []string
	for _, f := range p.forms {
		var keys []string
		for _, q := range f.Questions {
			keys = append(keys, q.Key)
		}
		asked = append(asked, strings.Join(keys, ","))
	}
	if got := strings.Join(asked, " | "); got != "--tls | ADMIN_EMAIL | --tls | APP_DOMAIN,API_DOMAIN,ADMIN_EMAIL" {
		t.Fatalf("asked %s", got)
	}
	if last := p.forms[3]; last.Answers["ADMIN_EMAIL"] != "a@x" || last.Questions[0].Check("Notes.example.com") == nil {
		t.Errorf("the second form lost the first one's answers, or does not check a domain: %+v", last.Answers)
	}
	if f := e.readEnv(); get(f, "STOWAGE_TLS_MODE") != "internal" || get(f, "ADMIN_EMAIL") != "a@x" {
		t.Errorf("recorded %s, %s", get(f, "STOWAGE_TLS_MODE"), get(f, "ADMIN_EMAIL"))
	}
}

func TestAnAccessibleChoiceIsNumberedAndEnterKeepsIt(t *testing.T) {
	f := Form{
		Questions: []Question{{Title: "How is it served", Key: "--tls", Choices: []Choice{{"none", "Loopback"}, {"acme", "Let's Encrypt"}}}},
		Defaults:  func(map[string]string) map[string]string { return map[string]string{"--tls": "none"} },
	}
	ctx := context.Background()
	a, out, err := askLines(ctx, f, "\n")
	if err != nil || a["--tls"] != "none" || !strings.Contains(out, "1. Loopback\n2. Let's Encrypt\nChoose 1-2 [1]: ") {
		t.Fatalf("%v %v\n%s", a, err, out)
	}
	if a, _, err = askLines(ctx, f, "3\n2\n"); err != nil || a["--tls"] != "acme" {
		t.Fatalf("%v %v", a, err)
	}
	f.Answers = map[string]string{"--tls": "acme"}
	if a, out, err = askLines(ctx, f, "\n"); err != nil || a["--tls"] != "acme" || !strings.Contains(out, "[2]") {
		t.Fatalf("%v %v\n%s", a, err, out)
	}
	if _, _, err = askLines(ctx, f, ""); !errors.Is(err, ErrAborted) {
		t.Fatalf("end of input: %v", err)
	}
}

func TestMovedPortsBehindLetsEncryptStayPublicOn443(t *testing.T) {
	e := newEnv(t)
	if err := e.install(func(o *Options) { tlsDomains(o); o.HTTPPort, o.HTTPSPort = 8080, 8443 }); err != nil {
		t.Fatal(err)
	}
	if u := get(e.readEnv(), "STOWAGE_URL_APP"); u != "https://notes.example.com" {
		t.Errorf("URL %q", u)
	}
	if cf := e.file(Caddyfile); strings.Contains(cf, "redir") || !strings.Contains(cf, "\thttps_port 8443\n") {
		t.Errorf("Caddyfile:\n%s", cf)
	}
	if !strings.Contains(e.log.String(), "forward them to 8080 and 8443") {
		t.Errorf("log: %s", e.log)
	}
	// A project with no HTTPS mode says so.
	e = newEnv(t)
	e.r.Spec.Front.TLSModes = []string{"none"}
	if err := e.install(tlsDomains); err == nil || !strings.Contains(err.Error(), "over no HTTPS mode") {
		t.Fatalf("err = %v", err)
	}
}

func TestAGivenDomainIsCheckedBeforeTheForm(t *testing.T) {
	e := newEnv(t)
	delete(e.vars, "ADMIN_EMAIL")
	p := &fakePrompter{answers: map[string]string{"ADMIN_EMAIL": "a@x"}}
	err := e.install(func(o *Options) {
		o.Prompt = p
		o.Domains = map[string]string{"app": "Notes.example.com", "api": "api.example.com"}
	})
	if err == nil || !strings.Contains(err.Error(), "APP_DOMAIN") || len(p.forms) != 0 {
		t.Fatalf("%v, %d forms", err, len(p.forms))
	}
	// So is a given value .env cannot hold, on an input with no check of its own.
	e.r.Spec.Inputs = append(e.r.Spec.Inputs, inputWithDefault("NOTE", ""))
	err = e.install(func(o *Options) { o.Prompt = p; o.TLS = Loopback; o.Set = map[string]string{"NOTE": "a\nb"} })
	if err == nil || !strings.Contains(err.Error(), "NOTE") || len(p.forms) != 0 {
		t.Fatalf("%v, %d forms", err, len(p.forms))
	}
}

func TestGoingBackFromOwnCertificateForgetsIt(t *testing.T) {
	cert, key := writeCert(t, time.Now().Add(24*time.Hour), "*.example.com")
	e := newEnv(t)
	modes := []string{"byo", "acme"}
	p := &fakePrompter{}
	p.onAsk = func() {
		f := p.forms[len(p.forms)-1]
		p.err = nil
		switch {
		case f.Questions[0].Key == "--tls":
			p.answers = map[string]string{"--tls": modes[0]}
			modes = modes[1:]
		case f.Questions[0].Key == "--cert":
			p.answers = map[string]string{"--cert": cert, "--key": key}
		case f.Review && len(modes) == 1:
			p.err = ErrBack
		default:
			p.answers = map[string]string{"APP_DOMAIN": "notes.example.com", "API_DOMAIN": "api.example.com"}
		}
	}
	if err := e.install(func(o *Options) { o.Prompt = p }); err != nil {
		t.Fatal(err)
	}
	if get(e.readEnv(), "STOWAGE_TLS_MODE") != "acme" {
		t.Fatal("not acme")
	}
	if _, err := os.Stat(filepath.Join(e.dir, TLSDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the certificate given before going back was copied: %v", err)
	}
}

func TestADirectoryHoldingATLSFolderIsRefused(t *testing.T) {
	cert, key := writeCert(t, time.Now().Add(24*time.Hour), "*.example.com")
	e := newEnv(t)
	_ = os.MkdirAll(filepath.Join(e.dir, TLSDir), 0o755)
	_ = os.WriteFile(filepath.Join(e.dir, TLSDir, "key.pem"), []byte("the operator's"), 0o600)
	if err := e.install(func(o *Options) { tlsDomains(o); o.Cert, o.Key = cert, key }); err == nil || !strings.Contains(err.Error(), "already holds a tls/") {
		t.Fatalf("err = %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(e.dir, TLSDir, "key.pem")); string(raw) != "the operator's" {
		t.Errorf("the operator's file was replaced: %q", raw)
	}
}

func TestTheCertificateFormChecksEachFileAndOpensAgainOnAMismatch(t *testing.T) {
	later := time.Now().Add(24 * time.Hour)
	cert, key := writeCert(t, later, "*.example.com")
	_, stranger := writeCert(t, later, "*.example.com")
	expired, _ := writeCert(t, time.Now().Add(-time.Hour), "*.example.com")
	e := newEnv(t)
	p := &fakePrompter{answers: map[string]string{"--cert": cert, "--key": stranger}}
	p.onAsk = func() {
		if len(p.forms) == 2 {
			p.answers = map[string]string{"--key": key} // the second form keeps --cert
		}
	}
	in := e.installer(func(o *Options) { o.Prompt = p })
	c, err := in.loadCert(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.forms) != 2 || !strings.Contains(p.forms[1].Note, "not a certificate and its key") || p.forms[1].Answers["--cert"] != cert || p.forms[0].Note != "" {
		t.Fatalf("forms %+v", p.forms)
	}
	if raw, _ := os.ReadFile(key); string(c.key) != string(raw) {
		t.Error("the pair is not the final answers'")
	}
	qs := p.forms[0].Questions
	for _, c := range []struct {
		q       int
		v, want string
	}{
		{0, expired, "expired"}, {0, key, "no PEM certificate"}, {0, cert + ".gone", "no such file"}, {0, cert, ""},
		{1, cert, "no PEM private key"}, {1, key + ".gone", "no such file"}, {1, stranger, ""},
	} {
		err := qs[c.q].Check(c.v)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s %s: %v, want %q", qs[c.q].Key, filepath.Base(c.v), err, c.want)
		}
	}
}

func TestAnExportedDomainIsAskedOnATerminalAndCountsOnlyWithoutLoopback(t *testing.T) {
	exported := func(o *Options) {
		o.Getenv = func(k string) string {
			return map[string]string{"ADMIN_EMAIL": "a@x", "APP_DOMAIN": "staging.example.com", "API_DOMAIN": "api.example.com"}[k]
		}
	}
	// On a terminal, once TLS is chosen, the exported domain is a question whose default it is.
	e := newEnv(t)
	p := &fakePrompter{answers: map[string]string{"--tls": "internal"}}
	if err := e.install(func(o *Options) { exported(o); o.Prompt = p }); err != nil {
		t.Fatal(err)
	}
	if len(p.forms) != 2 || p.forms[1].Questions[0].Key != "APP_DOMAIN" || p.defaults["APP_DOMAIN"] != "staging.example.com" {
		t.Fatalf("forms %+v, defaults %v", p.forms, p.defaults)
	}
	if get(e.readEnv(), "APP_DOMAIN") != "staging.example.com" {
		t.Error("the default is not what was recorded")
	}
	// Without a terminal, an HTTPS-only project takes it.
	e = newEnv(t)
	e.r.Spec.Front.TLSModes = []string{"acme"}
	if err := e.install(exported); err != nil || get(e.readEnv(), "STOWAGE_URL_APP") != "https://staging.example.com" {
		t.Fatalf("%v", err)
	}
}

func TestAFailedCertificateCopyLeavesNothing(t *testing.T) {
	cert, key := writeCert(t, time.Now().Add(24*time.Hour), "*.example.com")
	byo := func(o *Options) { tlsDomains(o); o.Cert, o.Key = cert, key }
	t.Cleanup(func() { writeFile = os.WriteFile })
	writeFile = func(name string, data []byte, perm os.FileMode) error {
		if filepath.Base(name) == "key.pem" {
			return errors.New("disk full")
		}
		return os.WriteFile(name, data, perm)
	}
	// A directory this run created is removed whole.
	e := newEnv(t)
	if err := e.install(byo); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
	e.nothingWritten()
	// One that was there keeps everything but this run's copy, so a later run is not refused.
	e = newEnv(t)
	_ = os.MkdirAll(e.dir, 0o755)
	_ = os.WriteFile(filepath.Join(e.dir, "notes.txt"), []byte("mine"), 0o600)
	if err := e.install(byo); err == nil {
		t.Fatal("no error")
	}
	if _, err := os.Stat(filepath.Join(e.dir, TLSDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tls/ left: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "notes.txt")); err != nil {
		t.Errorf("the operator's file: %v", err)
	}
	writeFile = os.WriteFile
	if err := e.install(byo); err != nil {
		t.Fatalf("a later run: %v", err)
	}
	if err := e.installer(nil).tlsFree("acme"); err != nil {
		t.Fatalf("refused outside byo: %v", err)
	}
}

func TestOnATerminalAnExportedDomainNeverPicksTheMode(t *testing.T) {
	e := newEnv(t)
	e.r.Spec.Front.TLSModes = []string{"acme", "internal"}
	p := &fakePrompter{answers: map[string]string{"--tls": "internal"}}
	if err := e.install(func(o *Options) {
		o.Prompt = p
		o.Getenv = func(k string) string {
			return map[string]string{"ADMIN_EMAIL": "a@x", "APP_DOMAIN": "a.example.com", "API_DOMAIN": "b.example.com"}[k]
		}
	}); err != nil {
		t.Fatal(err)
	}
	if p.forms[0].Questions[0].Key != "--tls" || get(e.readEnv(), "STOWAGE_TLS_MODE") != "internal" {
		t.Fatalf("forms %+v", p.forms)
	}
}

func TestATLSFolderMadeDuringTheRunIsKept(t *testing.T) {
	cert, key := writeCert(t, time.Now().Add(24*time.Hour), "*.example.com")
	e := newEnv(t)
	_ = os.MkdirAll(e.dir, 0o755) // the operator's directory
	e.docker.inspected = func() { // the operator puts a certificate there as the install takes the lock
		e.docker.inspected = nil
		_ = os.MkdirAll(filepath.Join(e.dir, TLSDir), 0o700)
		_ = os.WriteFile(filepath.Join(e.dir, TLSDir, "key.pem"), []byte("the operator's"), 0o600)
	}
	if err := e.install(func(o *Options) { tlsDomains(o); o.Cert, o.Key = cert, key }); err == nil {
		t.Fatal("installed")
	}
	if raw, _ := os.ReadFile(filepath.Join(e.dir, TLSDir, "key.pem")); string(raw) != "the operator's" {
		t.Errorf("the operator's file was removed: %q", raw)
	}
}

// writeCAChain writes a CA and a certificate for names it signs: the CA's file, the chain's and the
// key's.
func writeCAChain(t *testing.T, names ...string) (ca, cert, key string) {
	t.Helper()
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour)}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &k.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(k)
	ca, cert, key = filepath.Join(dir, "ca.pem"), filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	_ = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600)
	_ = os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	return ca, cert, key
}

// frontLabel is the front's configuration digest in the front compose file.
func (e *env) frontLabel() string {
	e.t.Helper()
	for _, l := range strings.Split(e.file(FrontCompose), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), FrontConfigLabel+": "); ok {
			return v
		}
	}
	e.t.Fatalf("no %s label:\n%s", FrontConfigLabel, e.file(FrontCompose))
	return ""
}

func TestAReRunChangesTheTLSModeAndTheCertificate(t *testing.T) {
	later := time.Now().Add(90 * 24 * time.Hour)
	cert, key := writeCert(t, later, "*.example.com")
	e := newEnv(t)
	e.r.Spec.Front.TLSModes = append(e.r.Spec.Front.TLSModes, "dns-cloudflare")
	if err := e.install(func(o *Options) { tlsDomains(o); o.TLS, o.Cert, o.Key = "byo", cert, key }); err != nil {
		t.Fatal(err)
	}
	label := e.frontLabel()
	// A new certificate: copied over the old one, the front recreated (its label changes).
	cert2, key2 := writeCert(t, later, "*.example.com")
	if err := e.install(func(o *Options) { o.Cert, o.Key = cert2, key2 }); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(cert2)
	if got, _ := os.ReadFile(filepath.Join(e.dir, TLSDir, "cert.pem")); !bytes.Equal(got, want) {
		t.Fatal("the new certificate was not copied")
	}
	if e.frontLabel() == label {
		t.Fatal("the front's label did not change with its certificate")
	}
	// Refused: a certificate not covering a domain, one half of a pair, a move to loopback,
	// --cert outside byo.
	narrow, narrowKey := writeCert(t, later, "notes.example.com")
	for name, c := range map[string]struct {
		mod  func(*Options)
		want string
	}{
		"uncovered": {func(o *Options) { o.Cert, o.Key = narrow, narrowKey }, "the certificate does not cover api.example.com (api)"},
		"half":      {func(o *Options) { o.Cert = cert }, "--tls byo needs --cert FILE and --key FILE"},
		"loopback":  {func(o *Options) { o.TLS = Loopback }, "moving between loopback mode and a TLS mode is a new install"},
		"cert":      {func(o *Options) { o.TLS, o.Cert, o.Key = "internal", cert, key }, "--cert and --key are for --tls byo, not internal"},
		"token":     {func(o *Options) { o.TLS = "dns-cloudflare" }, CloudflareTokenVar + " is required"},
	} {
		if err := e.install(c.mod); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(e.dir, TLSDir, "cert.pem")); !bytes.Equal(got, want) {
		t.Fatal("a refused re-run changed the certificate")
	}
	// To dns-cloudflare with its token; the token rotates with --set; to internal, the token goes.
	token := "cfut_" + strings.Repeat("a", 40)
	if err := e.install(func(o *Options) {
		o.TLS, o.Set = "dns-cloudflare", map[string]string{CloudflareTokenVar: token}
	}); err != nil {
		t.Fatal(err)
	}
	if f := e.readEnv(); get(f, "STOWAGE_TLS_MODE") != "dns-cloudflare" || get(f, CloudflareTokenVar) != token {
		t.Fatalf("after the move to dns-cloudflare: %s", e.file(".env"))
	}
	if _, err := os.Stat(filepath.Join(e.dir, TLSDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tls/ stayed after the move from byo: %v", err)
	}
	if cf := e.file(Caddyfile); !strings.Contains(cf, "dns cloudflare") || strings.Contains(cf, "tls /etc/caddy") {
		t.Fatalf("Caddyfile:\n%s", cf)
	}
	token2 := "cfut_" + strings.Repeat("b", 40)
	if err := e.install(func(o *Options) { o.Set = map[string]string{CloudflareTokenVar: token2} }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.log.String(), "changing "+CloudflareTokenVar+": <redacted> → <redacted>") || get(e.readEnv(), CloudflareTokenVar) != token2 {
		t.Fatalf("the token did not rotate:\n%s", e.log)
	}
	if err := e.install(func(o *Options) { o.TLS = "internal" }); err != nil {
		t.Fatal(err)
	}
	if f := e.readEnv(); get(f, "STOWAGE_TLS_MODE") != "internal" || strings.Contains(e.file(".env"), CloudflareTokenVar) {
		t.Fatalf("after the move to internal:\n%s", e.file(".env"))
	}
	if !strings.Contains(e.log.String(), "removing "+CloudflareTokenVar) {
		t.Fatalf("the token's removal is not logged:\n%s", e.log)
	}
	// Back to byo, with a CA the certificate chains to.
	e.r.Spec.Front.PublishCA = &spec.PublishCA{Volume: "ca"}
	ca, caCert, caKey := writeCAChain(t, "notes.example.com", "api.example.com")
	if err := e.install(func(o *Options) { o.TLS, o.Cert, o.Key, o.CA = "byo", caCert, caKey, cert }); err == nil || !strings.Contains(err.Error(), "--ca: the certificate does not chain to it") {
		t.Fatalf("a CA the certificate does not chain to: %v", err)
	}
	if err := e.install(func(o *Options) { o.TLS, o.Cert, o.Key, o.CA = "byo", caCert, caKey, ca }); err != nil {
		t.Fatal(err)
	}
	wantCA, _ := os.ReadFile(ca)
	if got, _ := os.ReadFile(filepath.Join(e.dir, TLSDir, "ca.pem")); !bytes.Equal(got, wantCA) {
		t.Fatal("the CA was not copied")
	}
	if fc := e.file(FrontCompose); !strings.Contains(fc, "cp /etc/caddy/tls/ca.pem /stowage-ca/.ca.crt") || !strings.Contains(fc, "- ca:/stowage-ca") {
		t.Fatalf("front compose does not publish the CA:\n%s", fc)
	}
}

func TestARouteRefusesOtherSourcesThenStripsThenAsksTheAuthService(t *testing.T) {
	t.Parallel()
	r := spec.Route{Name: "api", Upstream: "api:9090", StripHeaders: []string{"X-Auth-Request-*"}, AllowInput: "SOURCES",
		ForwardAuth: &spec.ForwardAuth{AddressInput: "AUTH", URIInput: "AUTH_URI", CopyHeaders: []string{"X-Auth-Request-User", "X-Auth-Request-Email"}}}
	body := func(vals map[string]string) string {
		var b strings.Builder
		routeBody(&b, r, func(k string) string { return vals[k] })
		return b.String()
	}
	if got, want := body(map[string]string{"SOURCES": "10.0.0.0/8, 192.0.2.7", "AUTH": "http://auth:4180", "AUTH_URI": "/check?x=1"}), `	@stowage_denied not remote_ip 10.0.0.0/8 192.0.2.7
	route {
		respond @stowage_denied 403
		request_header -X-Auth-Request-*
		request_header -X-Auth-Request-User
		request_header -X-Auth-Request-Email
		forward_auth http://auth:4180 {
			uri /check?x=1
			copy_headers X-Auth-Request-User X-Auth-Request-Email
		}
		reverse_proxy api:9090
	}
`; got != want {
		t.Errorf("every directive:\n%s\nwant\n%s", got, want)
	}
	// An empty allowlist refuses everyone; an empty address leaves forward auth off, the headers it
	// would copy still stripped.
	strips := "request_header -X-Auth-Request-*\n\t\trequest_header -X-Auth-Request-User\n\t\trequest_header -X-Auth-Request-Email\n"
	if got, want := body(nil), "\troute {\n\t\trespond 403\n\t\t"+strips+"\t\treverse_proxy api:9090\n\t}\n"; got != want {
		t.Errorf("empty inputs:\n%s\nwant\n%s", got, want)
	}
	r.AllowInput, r.ForwardAuth.URIInput = "", ""
	// Caddy requires a URI: / without one.
	if got, want := body(map[string]string{"AUTH": "auth:4180", "AUTH_URI": "/ignored"}), "\troute {\n\t\t"+strips+"\t\tforward_auth auth:4180 {\n\t\t\turi /\n\t\t\tcopy_headers X-Auth-Request-User X-Auth-Request-Email\n\t\t}\n\t\treverse_proxy api:9090\n\t}\n"; got != want {
		t.Errorf("no allowlist, no URI input:\n%s\nwant\n%s", got, want)
	}
	if got, want := body(nil), "\trequest_header -X-Auth-Request-*\n\trequest_header -X-Auth-Request-User\n\trequest_header -X-Auth-Request-Email\n\treverse_proxy api:9090\n"; got != want {
		t.Errorf("forward auth off:\n%s\nwant\n%s", got, want)
	}
}

func TestTheValuesTheFrontReadsAreChecked(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		check func(string) error
		ok    []string
		bad   []string
	}{
		{checkSources, []string{"", "10.0.0.0/8", "192.0.2.7, 2001:db8::/32 ::1", "0.0.0.0/0,::/0"}, []string{"10.0.0.0/33", "example.com", "10.0.0.1}", "1.2.3", "fe80::1%eth0"}},
		{checkAuthAddress, []string{"", "auth:4180", "http://auth", "https://auth.example.com:443", "a", "[fd00::1]:9091", "http://[::1]:9091", "https://[2001:db8::7]"},
			[]string{"auth:4180 {", "ftp://auth", "auth:", "-auth", "auth/path", "auth:123456", "[fd00::1", "[192.0.2.1]:9091", "[fe80::1%eth0]:9091", "[::1]:", "[::1]:9091/x", "[::1]x", "fd00::1:9091", "[]:9091", "http://https://[fd00::1]:9091", "[::1]:99999", "[::1]:0", "[::ffff:192.0.2.1]:9091"}},
		{checkAuthURI, []string{"", "/", "/oauth2/auth?rd=x&y=%20"}, []string{"oauth2", "/a b", "/a{b}", `/a"`}},
	} {
		for _, v := range c.ok {
			if err := c.check(v); err != nil {
				t.Errorf("%q refused: %v", v, err)
			}
		}
		for _, v := range c.bad {
			if c.check(v) == nil {
				t.Errorf("%q accepted", v)
			}
		}
	}
	e := newEnv(t)
	e.r.Spec.Front.Routes[0].AllowInput = "SOURCES"
	e.r.Spec.Inputs = append(e.r.Spec.Inputs, spec.Input{Key: "SOURCES", Prompt: "Sources"})
	if err := e.install(func(o *Options) { o.Set = map[string]string{"SOURCES": "10.0.0.0/8 }"} }); err == nil || !strings.Contains(err.Error(), `SOURCES: "}" is not an IP address or a CIDR`) {
		t.Fatalf("a source spliced into the Caddyfile: %v", err)
	}
}

func TestCAIsForByoAndAProjectThatPublishesOne(t *testing.T) {
	t.Parallel()
	ca, cert, key := writeCAChain(t, "notes.example.com", "api.example.com")
	e := newEnv(t)
	if err := e.install(func(o *Options) { tlsDomains(o); o.TLS, o.Cert, o.Key, o.CA = "byo", cert, key, ca }); err == nil || !strings.Contains(err.Error(), "--ca: notes publishes no CA to its services") {
		t.Fatalf("--ca without publish_ca: %v", err)
	}
	e.r.Spec.Front.PublishCA = &spec.PublishCA{Volume: "ca"}
	if err := e.install(func(o *Options) { tlsDomains(o); o.TLS, o.CA = "internal", ca }); err == nil || !strings.Contains(err.Error(), "--ca is for --tls byo, not internal") {
		t.Fatalf("--ca with internal: %v", err)
	}
	if err := e.install(func(o *Options) { tlsDomains(o); o.TLS, o.Cert, o.Key, o.CA = "byo", cert, key, ca }); err != nil {
		t.Fatal(err)
	}
	if fc := e.file(FrontCompose); !strings.Contains(fc, "cp /etc/caddy/tls/ca.pem /stowage-ca/.ca.crt && chmod 0644 /stowage-ca/.ca.crt && mv") {
		t.Fatalf("front compose:\n%s", fc)
	}
}

func TestARenewalKeepsTheInstallsCAWhenItChainsToIt(t *testing.T) {
	t.Parallel()
	ca, cert, key := writeCAChain(t, "notes.example.com", "api.example.com")
	e := newEnv(t)
	e.r.Spec.Front.PublishCA = &spec.PublishCA{Volume: "ca"}
	if err := e.install(func(o *Options) { tlsDomains(o); o.TLS, o.Cert, o.Key, o.CA = "byo", cert, key, ca }); err != nil {
		t.Fatal(err)
	}
	// A certificate of another CA, without --ca: refused, nothing changed.
	other, otherKey := writeCert(t, time.Now().Add(24*time.Hour), "*.example.com")
	if err := e.install(func(o *Options) { o.Cert, o.Key = other, otherKey }); err == nil || !strings.Contains(err.Error(), "does not chain to the install's CA") {
		t.Fatalf("a certificate of another CA: %v", err)
	}
	// The same certificate again (a renewal under the CA), without --ca: the CA stays published,
	// the replaced files are gone once the stack came up.
	if err := e.install(func(o *Options) { o.Cert, o.Key = cert, key }); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(ca)
	if got, _ := os.ReadFile(filepath.Join(e.dir, TLSDir, "ca.pem")); !bytes.Equal(got, want) {
		t.Fatal("a renewal without --ca dropped the install's CA")
	}
	if _, err := os.Stat(filepath.Join(e.dir, tlsPrev)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tls.prev/ stayed after the stack came up: %v", err)
	}
	// A run whose stack fails keeps the replaced files beside the new ones, and a second failed
	// run keeps them still: tls.prev/ is the last that came up.
	e.docker.upErr = errors.New("unhealthy")
	for range 2 {
		if err := e.install(func(o *Options) { o.Cert, o.Key, o.CA = other, otherKey, other }); err == nil || !strings.Contains(err.Error(), "did not become ready") {
			t.Fatalf("the failed start was not reported: %v", err)
		}
		if got, _ := os.ReadFile(filepath.Join(e.dir, tlsPrev, "ca.pem")); !bytes.Equal(got, want) {
			t.Fatal("the replaced certificate was not kept after a failed start")
		}
	}
	// A plain re-run that comes up removes it; --ca none publishes no CA.
	e.docker.upErr = nil
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, tlsPrev)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tls.prev/ stayed after a re-run came up: %v", err)
	}
	if err := e.install(func(o *Options) { o.Cert, o.Key, o.CA = other, otherKey, noCA }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, TLSDir, "ca.pem")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("--ca none kept a CA: %v", err)
	}
}

func TestAReRunKeepsAModeTheReleaseNoLongerOffers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.install(func(o *Options) { tlsDomains(o); o.TLS = "internal" }); err != nil {
		t.Fatal(err)
	}
	e.r.Spec.Front.TLSModes = []string{"acme"}
	if err := e.install(nil); err != nil {
		t.Fatalf("a re-run of a mode no longer offered: %v", err)
	}
	if err := e.install(func(o *Options) { o.TLS = "byo" }); err == nil || !strings.Contains(err.Error(), "--tls byo: notes offers acme") {
		t.Fatalf("a move to a mode not offered: %v", err)
	}
}

func TestAMoveFromByoWhoseFirstTryFailedRemovesTheCertificateOnceUp(t *testing.T) {
	t.Parallel()
	cert, key := writeCert(t, time.Now().Add(24*time.Hour), "*.example.com")
	e := newEnv(t)
	if err := e.install(func(o *Options) { tlsDomains(o); o.TLS, o.Cert, o.Key = "byo", cert, key }); err != nil {
		t.Fatal(err)
	}
	e.docker.upErr = errors.New("unhealthy")
	if err := e.install(func(o *Options) { o.TLS = "internal" }); err == nil {
		t.Fatal("the failed start was not reported")
	}
	if _, err := os.Stat(filepath.Join(e.dir, TLSDir, "key.pem")); err != nil {
		t.Fatalf("the key went before the stack came up without it: %v", err)
	}
	e.docker.upErr = nil
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, TLSDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tls/ stayed once the stack came up in internal: %v", err)
	}
}

// On the VM, status gives a byo install's certificate, and the daemon journals its warning.
func TestTheVMSaysItsCertificateExpires(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	cert, key := writeCert(t, time.Now().Add(10*24*time.Hour), "*.example.com")
	if err := e.install(func(o *Options) { vm(o); tlsDomains(o); o.TLS, o.Cert, o.Key = "byo", cert, key }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	d := daemon(e, &fakeHost{})
	var st Status
	if call(t, d.Handler(), "GET", "/v1/status", nil, &st); st.Cert == nil || st.Cert.DaysLeft != 9 || !strings.Contains(st.Cert.Warning, "expires in 9 day(s)") {
		t.Fatalf("status cert %+v", st.Cert)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var got []Event
	d.onEvent = func(evs []Event) { mu.Lock(); got = append(got, evs...); mu.Unlock(); cancel() }
	d.certWatch(ctx)
	if len(got) != 1 || got[0].Kind != evCertExpiring || !strings.Contains(got[0].Reason, "sudo notes-stow install --tls byo") {
		t.Fatalf("events %+v", got)
	}
}

// issued writes a CA, an intermediate it signs and a leaf for name the intermediate signs; it
// returns the CA's pool, the leaf alone, the leaf with its intermediate, and the leaf's key.
func issued(t *testing.T, name string) (*x509.CertPool, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	mk := func(cn string, ca bool, parent *x509.Certificate, pk *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour),
			NotAfter: time.Now().Add(90 * 24 * time.Hour), IsCA: ca, BasicConstraintsValid: true}
		if ca {
			tmpl.KeyUsage = x509.KeyUsageCertSign
		} else {
			tmpl.DNSNames = []string{name}
		}
		if parent == nil {
			parent, pk = tmpl, k
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &k.PublicKey, pk)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := x509.ParseCertificate(der)
		return c, k, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	root, rk, _ := mk("Test Root", true, nil, nil)
	inter, ik, interPEM := mk("Test Intermediate", true, root, rk)
	_, lk, leafPEM := mk(name, false, inter, ik)
	kder, _ := x509.MarshalECPrivateKey(lk)
	key := filepath.Join(dir, "key.pem")
	_ = os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	alone, full := filepath.Join(dir, "leaf.pem"), filepath.Join(dir, "full.pem")
	_ = os.WriteFile(alone, leafPEM, 0o600)
	_ = os.WriteFile(full, append(leafPEM, interPEM...), 0o600)
	pool := x509.NewCertPool()
	pool.AddCert(root)
	return pool, alone, full, key
}

// Without --ca, a leaf given without its intermediate is refused; the whole chain is taken.
func TestAByoChainWithoutItsIntermediateIsRefused(t *testing.T) {
	t.Parallel()
	pool, alone, full, key := issued(t, "*.example.com")
	roots := func(o *Options) { o.Roots = func() (*x509.CertPool, error) { return pool, nil } }
	e := newEnv(t)
	err := e.install(func(o *Options) { tlsDomains(o); roots(o); o.TLS, o.Cert, o.Key = "byo", alone, key })
	if err == nil || !strings.Contains(err.Error(), "the file must hold the whole chain") || !strings.Contains(err.Error(), `its issuer is "Test Intermediate"`) {
		t.Fatalf("leaf alone: %v", err)
	}
	e = newEnv(t)
	if err := e.install(func(o *Options) { tlsDomains(o); roots(o); o.TLS, o.Cert, o.Key = "byo", full, key }); err != nil {
		t.Fatalf("the whole chain: %v", err)
	}
}

// status names the certificate's end and warns from 30 days before it, and once it passed.
func TestCertStatusWarnsBeforeTheEnd(t *testing.T) {
	t.Parallel()
	now := time.Now()
	for _, c := range []struct {
		left time.Duration
		warn string
	}{
		{31 * 24 * time.Hour, ""},
		{29*24*time.Hour + time.Hour, "expires in 29 day(s)"},
		{-time.Hour, "expired on"},
	} {
		dir := t.TempDir()
		cert, _ := writeCert(t, now.Add(c.left), "app.example.org")
		raw, _ := os.ReadFile(cert)
		_ = os.MkdirAll(filepath.Join(dir, TLSDir), 0o700)
		_ = os.WriteFile(filepath.Join(dir, TLSDir, "cert.pem"), raw, 0o600)
		st, err := certStatus(dir, now, "renew it")
		if err != nil || st == nil || st.Subject != "app.example.org" {
			t.Fatalf("%v: %+v %v", c.left, st, err)
		}
		if (c.warn == "") != (st.Warning == "") || !strings.Contains(st.Warning, c.warn) {
			t.Errorf("%v: warning %q, want %q", c.left, st.Warning, c.warn)
		}
	}
	if st, err := certStatus(t.TempDir(), now, ""); st != nil || err != nil {
		t.Errorf("no certificate: %+v %v", st, err)
	}
}
