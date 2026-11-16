package install

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/spec"
)

const testACMEDir = "https://acme.example.test:14000/dir"

// The ACME directory and its root are global options in both ACME modes, after the issuer they
// fill in; Let's Encrypt's stays the issuer's only without them, and the other modes name none.
func TestTheACMECAInTheCaddyfile(t *testing.T) {
	t.Parallel()
	routes := spec.Front{TLSModes: []string{"acme", "dns-cloudflare", "byo", "internal"}, Routes: []spec.Route{{Name: "app", Upstream: "web:8080", DomainInput: "APP_DOMAIN"}}}
	l4Only := spec.Front{TLSModes: []string{"acme"}}
	l4 := []spec.L4Route{{Name: "db", Protocol: "tcp", Port: 5432, Upstream: "db:5432", TLS: &spec.L4TLS{DomainInput: "DB_DOMAIN"}}}
	domains := map[string]string{"app": "a.example.test", "db": "db.example.test"}
	none := func(string) string { return "" }
	ca := "\tacme_ca " + testACMEDir + "\n"
	root := "\tacme_ca_root /etc/caddy/acme-ca/root.pem\n"
	for _, c := range []struct {
		name   string
		f      spec.Front
		l4     []spec.L4Route
		mode   string
		acme   acmeIssuer
		issuer string // what precedes the CA's options
		want   string // the CA's options, "" for none
	}{
		{"acme, CA and root", routes, nil, "acme", acmeIssuer{Email: "ops@example.test", CA: testACMEDir, Root: true}, "\tcert_issuer acme\n\temail ops@example.test\n", ca + root},
		{"acme, CA alone", routes, nil, "acme", acmeIssuer{CA: testACMEDir}, "\tcert_issuer acme\n", ca},
		{"acme, Let's Encrypt", routes, nil, "acme", acmeIssuer{Root: true}, "\tcert_issuer acme\n", ""},
		{"acme, L4 routes alone", l4Only, l4, "acme", acmeIssuer{CA: testACMEDir, Root: true}, "\tcert_issuer acme {\n\t\tdisable_tlsalpn_challenge\n\t}\n", ca + root},
		{"dns-cloudflare", routes, nil, "dns-cloudflare", acmeIssuer{CA: testACMEDir, Root: true}, "\tcert_issuer acme {\n\t\tdns cloudflare {env.CLOUDFLARE_API_TOKEN}\n\t}\n", ca + root},
		{"byo", routes, nil, "byo", acmeIssuer{CA: testACMEDir, Root: true}, "", ""},
		{"internal", routes, nil, "internal", acmeIssuer{CA: testACMEDir, Root: true}, "", ""},
	} {
		cf := tlsCaddyfile(c.f, c.l4, c.mode, domains, c.acme, "443", false, none, nil)
		global, _, _ := strings.Cut(cf, "\n}\n")
		global += "\n"
		switch {
		case c.want == "" && strings.Contains(global, "acme_ca"):
			t.Errorf("%s: a CA is named:\n%s", c.name, global)
		case c.want != "" && !strings.Contains(global, c.issuer+c.want):
			t.Errorf("%s: want %q after %q:\n%s", c.name, c.want, c.issuer, global)
		case c.want == ca && strings.Contains(global, "acme_ca_root"):
			t.Errorf("%s: a root is named:\n%s", c.name, global)
		}
	}
}

// What --acme-ca and --acme-ca-root take, and what they refuse before anything is written.
func TestTheACMECAFlagsRefuse(t *testing.T) {
	t.Parallel()
	ca, leaf, key := writeCAChain(t, "notes.example.com")
	notPEM := filepath.Join(t.TempDir(), "root.txt")
	if err := os.WriteFile(notPEM, []byte("no certificate here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	acme := func(more func(*Options)) func(*Options) {
		return func(o *Options) {
			tlsDomains(o)
			o.TLS = "acme"
			more(o)
		}
	}
	for _, c := range []struct {
		name string
		mod  func(*Options)
		want string
	}{
		{"another mode", func(o *Options) { tlsDomains(o); o.TLS, o.ACMECA = "internal", testACMEDir }, "--acme-ca is for --tls acme or dns-cloudflare, not internal"},
		{"a root in another mode", func(o *Options) { tlsDomains(o); o.TLS, o.ACMECARoot = "internal", ca }, "--acme-ca-root is for --tls acme or dns-cloudflare, not internal"},
		{"loopback", func(o *Options) { o.TLS, o.ACMECA = Loopback, testACMEDir }, "--acme-ca is for --tls acme or dns-cloudflare, not none"},
		{"plain HTTP", acme(func(o *Options) { o.ACMECA = "http://acme.example.test/dir" }), "is not an https URL"},
		{"no URL", acme(func(o *Options) { o.ACMECA = "acme.example.test/dir" }), "is not an https URL"},
		{"no host", acme(func(o *Options) { o.ACMECA = "https:///dir" }), "is not an https URL"},
		{"a user", acme(func(o *Options) { o.ACMECA = "https://u:p@acme.example.test/dir" }), "names a user"},
		{"a placeholder", acme(func(o *Options) { o.ACMECA = "https://acme.example.test/{env.TOKEN}" }), "holds a space, a quote, a brace"},
		{"a second word", acme(func(o *Options) { o.ACMECA = "https://acme.example.test/dir\n\tadmin on" }), "holds a space, a quote, a brace"},
		{"a root alone", acme(func(o *Options) { o.ACMECARoot = ca }), "--acme-ca-root is the root of --acme-ca's directory, and this install uses Let's Encrypt's"},
		{"a root with none", acme(func(o *Options) { o.ACMECA, o.ACMECARoot = noCA, ca }), "--acme-ca none removes the ACME CA and its root"},
		{"an unreadable root", acme(func(o *Options) { o.ACMECA, o.ACMECARoot = testACMEDir, filepath.Join(t.TempDir(), "gone.pem") }), "no such file"},
		{"no PEM", acme(func(o *Options) { o.ACMECA, o.ACMECARoot = testACMEDir, notPEM }), "holds no PEM certificate"},
		{"a key", acme(func(o *Options) { o.ACMECA, o.ACMECARoot = testACMEDir, key }), "holds a EC PRIVATE KEY"},
		{"a leaf", acme(func(o *Options) { o.ACMECA, o.ACMECARoot = testACMEDir, leaf }), "is not a CA certificate"},
	} {
		e := newEnv(t)
		if err := e.install(c.mod); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
		e.nothingWritten()
	}
	// A dry run reads them and writes nothing.
	e := newEnv(t)
	if err := e.install(acme(func(o *Options) { o.ACMECA, o.ACMECARoot, o.DryRun = testACMEDir, ca, true })); err != nil {
		t.Fatal(err)
	}
	e.nothingWritten()
	// A project without a front takes neither.
	e = newEnv(t)
	e.r.Spec.Front = spec.Front{}
	if err := e.install(func(o *Options) { o.ACMECA = testACMEDir }); err == nil || !strings.Contains(err.Error(), "has no front: --acme-ca") {
		t.Errorf("no front: %v", err)
	}
}

// The ACME CA is recorded, its root copied and mounted, both kept by a re-run; a root replaced,
// a new CA without its root, none and a move to another mode each change what the front reads,
// which recreates it.
func TestTheACMECAIsRecordedAndKept(t *testing.T) {
	t.Parallel()
	root, _, _ := writeCAChain(t, "notes.example.com")
	other, _, _ := writeCAChain(t, "notes.example.com")
	rootPEM, _ := os.ReadFile(root)
	otherPEM, _ := os.ReadFile(other)
	e := newEnv(t)
	global := func() string { g, _, _ := strings.Cut(e.file(Caddyfile), "\n}\n"); return g + "\n" }
	mounted := func() bool { return strings.Contains(e.file(FrontCompose), "./"+ACMERootDir+":/etc/caddy/acme-ca:ro") }
	held := func() string { raw, _ := os.ReadFile(filepath.Join(e.dir, ACMERootFile)); return string(raw) }
	recorded := func() (string, bool) { return e.readEnv().Get(ACMECAVar) }
	check := func(step, url, rootWant string) {
		t.Helper()
		g := global()
		v, ok := recorded()
		switch {
		case url == "" && (ok || strings.Contains(g, "acme_ca")):
			t.Errorf("%s: a CA is left: %q\n%s", step, v, g)
		case url != "" && (v != url || !strings.Contains(g, "\tacme_ca "+url+"\n")):
			t.Errorf("%s: CA %q, want %q\n%s", step, v, url, g)
		case held() != rootWant:
			t.Errorf("%s: the install holds the root %q, want %q", step, held(), rootWant)
		case (rootWant != "") != mounted() || (rootWant != "") != strings.Contains(g, "\tacme_ca_root /etc/caddy/acme-ca/root.pem\n"):
			t.Errorf("%s: the root's mount %v, want %v\n%s", step, mounted(), rootWant != "", g)
		}
	}
	if err := e.install(func(o *Options) { tlsDomains(o); o.TLS, o.ACMECA, o.ACMECARoot = "acme", testACMEDir, root }); err != nil {
		t.Fatal(err)
	}
	check("first install", testACMEDir, string(rootPEM))
	if st, err := os.Stat(filepath.Join(e.dir, ACMERootDir)); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("the root's directory: %v %v", st, err)
	}
	label := e.frontLabel()
	// A re-run given neither keeps both.
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	check("a re-run", testACMEDir, string(rootPEM))
	if e.frontLabel() != label {
		t.Error("a re-run changing nothing recreates the front")
	}
	// Another root alone replaces it: the front's configuration digest follows it.
	if err := e.install(func(o *Options) { o.ACMECARoot = other }); err != nil {
		t.Fatal(err)
	}
	check("another root", testACMEDir, string(otherPEM))
	if e.frontLabel() == label {
		t.Error("the front's configuration digest does not follow the ACME CA's root")
	}
	// Another CA without its root removes the root, which belonged to the CA before.
	const next = "https://acme.example.test/other/dir"
	if err := e.install(func(o *Options) { o.ACMECA = next }); err != nil {
		t.Fatal(err)
	}
	check("another CA", next, "")
	if !strings.Contains(e.log.String(), "changing "+ACMECAVar+": "+testACMEDir+" → "+next) {
		t.Errorf("the change is not logged:\n%s", e.log)
	}
	// The same CA again with a root takes it; none for the root removes it alone.
	if err := e.install(func(o *Options) { o.ACMECA, o.ACMECARoot = next, root }); err != nil {
		t.Fatal(err)
	}
	check("the CA's root", next, string(rootPEM))
	if err := e.install(func(o *Options) { o.ACMECARoot = noCA }); err != nil {
		t.Fatal(err)
	}
	check("--acme-ca-root none", next, "")
	// none for the CA goes back to Let's Encrypt, its root removed.
	if err := e.install(func(o *Options) { o.ACMECA, o.ACMECARoot = testACMEDir, root }); err != nil {
		t.Fatal(err)
	}
	if err := e.install(func(o *Options) { o.ACMECA = noCA }); err != nil {
		t.Fatal(err)
	}
	check("--acme-ca none", "", "")
	// A move to dns-cloudflare keeps them; a move to another mode removes both, and a re-run
	// there refuses them.
	if err := e.install(func(o *Options) { o.ACMECA, o.ACMECARoot = testACMEDir, root }); err != nil {
		t.Fatal(err)
	}
	if v := recordedValues(e.r.Spec, e.readEnv().Get, true); v == nil || v.ACMECA != testACMEDir {
		t.Errorf("status --json --values: %+v", v)
	}
	// status --json --values: the root's digest, as a file input's; whether a pull login is held,
	// never the login.
	sum := sha256.Sum256(rootPEM)
	values := func(o *Options) { o.StatusValues = true }
	if st, out := statusOf(t, e, values); st.Values == nil || st.Values.ACMECARoot != "sha256:"+hex.EncodeToString(sum[:]) || st.Values.PullLogin || !strings.Contains(out, `"pull_login": false`) {
		t.Errorf("status --json --values: %+v\n%s", st.Values, out)
	}
	if err := os.WriteFile(registryLoginFile(e.dir), loginFile("registry.example.org", "ops", loginPassword), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, out := statusOf(t, e, values); st.Values == nil || !st.Values.PullLogin || st.Values.PullLoginError != "" || strings.Contains(out, loginPassword) {
		t.Errorf("a login held: %+v\n%s", st.Values, out)
	}
	// A login held that does not read, and a root held that does not: each said, never "none".
	if err := os.WriteFile(registryLoginFile(e.dir), []byte("{"+loginPassword), 0o600); err != nil {
		t.Fatal(err)
	}
	rootFile := filepath.Join(e.dir, ACMERootFile)
	if err := os.Rename(rootFile, rootFile+".away"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(rootFile, 0o700); err != nil {
		t.Fatal(err)
	}
	if st, out := statusOf(t, e, values); st.Values == nil || st.Values.PullLogin || st.Values.PullLoginError == "" || st.Values.PullLoginError != st.RegistryLoginError ||
		st.Values.ACMECARoot != "" || !strings.HasPrefix(st.Values.ACMECARootError, "the ACME CA's root: ") || strings.Contains(out, loginPassword) {
		t.Errorf("unreadable: %+v\n%s", st.Values, out)
	}
	if err := os.Remove(rootFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(rootFile+".away", rootFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(registryLoginFile(e.dir)); err != nil {
		t.Fatal(err)
	}
	e.r.Spec.Front.TLSModes = append(e.r.Spec.Front.TLSModes, "dns-cloudflare")
	if err := e.install(func(o *Options) {
		o.TLS, o.Set = "dns-cloudflare", map[string]string{CloudflareTokenVar: strings.Repeat("a", 40)}
	}); err != nil {
		t.Fatal(err)
	}
	check("dns-cloudflare", testACMEDir, string(rootPEM))
	if err := e.install(func(o *Options) { o.TLS = "internal" }); err != nil {
		t.Fatal(err)
	}
	check("internal", "", "")
	if _, err := os.Stat(filepath.Join(e.dir, ACMERootDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the root's directory is left: %v", err)
	}
	if v := recordedValues(e.r.Spec, e.readEnv().Get, true); v == nil || v.ACMECA != "" {
		t.Errorf("status --json --values in internal: %+v", v)
	}
	before := e.file(".env")
	if err := e.install(func(o *Options) { o.ACMECA = testACMEDir }); err == nil || !strings.Contains(err.Error(), "--acme-ca is for --tls acme or dns-cloudflare, not internal") {
		t.Errorf("a re-run in internal: %v", err)
	}
	if e.file(".env") != before {
		t.Error("a refused re-run changed .env")
	}
}
