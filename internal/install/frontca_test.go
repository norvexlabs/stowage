package install

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// testRoot is a root CA's (or, with ca false, a leaf's) PEM certificate and key.
func testRoot(t *testing.T, ca bool) (cert, key []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Local Root"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour),
		IsCA: ca, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})
}

func TestAFrontCAIsARootAndItsKey(t *testing.T) {
	t.Parallel()
	cert, key := testRoot(t, true)
	other, otherKey := testRoot(t, true)
	leaf, leafKey := testRoot(t, false)
	if ca, err := parseFrontCA(cert, key); err != nil || ca.Cert != string(cert) || ca.Key != string(key) {
		t.Fatalf("%v %+v", err, ca)
	}
	for what, c := range map[string]struct {
		cert, key []byte
		want      string
	}{
		"not PEM":       {[]byte("x"), key, "not PEM"},
		"another's key": {cert, otherKey, "does not go with the certificate"},
		"not a CA":      {leaf, leafKey, "not a CA's"},
		"a key as cert": {key, key, "not PEM"},
	} {
		if _, err := parseFrontCA(c.cert, c.key); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", what, err)
		}
	}
	if c, k, err := splitPEM(append(append([]byte{}, cert...), key...)); err != nil || !bytes.Equal(c, cert) || !bytes.Equal(k, key) {
		t.Errorf("split: %v", err)
	}
	for what, out := range map[string][]byte{"the key alone": key, "two certificates": append(append(append([]byte{}, cert...), other...), key...), "nothing": []byte("x\n")} {
		if _, _, err := splitPEM(out); err == nil {
			t.Errorf("%s: split", what)
		}
	}
}

// The scripts the front's image runs, run here by sh on a directory standing in for its volume.
func TestTheFrontCAScriptsWriteAndReadTheRoot(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	data := t.TempDir()
	at := func(script string) string {
		return strings.NewReplacer(localCADir, data+"/caddy/pki/authorities/local", localLeavesDir, data+"/caddy/certificates/local").Replace(script)
	}
	run := func(script string, stdin []byte) ([]byte, error) {
		c := exec.Command("sh", "-c", at(script))
		c.Stdin = bytes.NewReader(stdin)
		return c.Output()
	}
	if out, err := run(frontCAReadShell, nil); err != nil || len(out) != 0 {
		t.Fatalf("no CA yet: %v %q", err, out)
	}
	dir := filepath.Join(data, "caddy/pki/authorities/local")
	leaves := filepath.Join(data, "caddy/certificates/local/app.example.org")
	for _, d := range []string{dir, leaves} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"root.crt", "root.key", "intermediate.crt", "intermediate.key"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("an earlier root's\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cert, key := testRoot(t, true)
	if out, err := run(frontCAWriteShell, append(append([]byte{}, cert...), key...)); err != nil {
		t.Fatalf("write: %v %s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "root.crt")); !bytes.Equal(got, cert) {
		t.Errorf("root.crt:\n%s", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "root.key")); !bytes.Equal(got, key) {
		t.Errorf("root.key:\n%s", got)
	}
	if fi, err := os.Stat(filepath.Join(dir, "root.key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("root.key: %v %v", err, fi)
	}
	for _, p := range []string{filepath.Join(dir, "intermediate.crt"), filepath.Join(dir, "intermediate.key"), filepath.Join(data, "caddy/certificates/local"), filepath.Join(dir, ".restore")} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s stayed: %v", p, err)
		}
	}
	out, err := run(frontCAReadShell, nil)
	if c, k, serr := splitPEM(out); err != nil || serr != nil || !bytes.Equal(c, cert) || !bytes.Equal(k, key) {
		t.Fatalf("read back: %v %v\n%s", err, serr, out)
	}
	// Half a root is refused, the one there kept.
	if _, err := run(frontCAWriteShell, cert); err == nil {
		t.Error("a certificate without its key was written")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "root.key")); !bytes.Equal(got, key) {
		t.Error("a refused write replaced the key")
	}
}

// frontCAEnv is an install whose front's volume holds root (a PEM certificate and key), as the
// fake Docker answers its read; a restore's write succeeds.
func frontCAEnv(t *testing.T, root []byte) *env {
	t.Helper()
	e := newEnv(t)
	e.vars["APP_DOMAIN"], e.vars["API_DOMAIN"] = "app.example.org", "api.example.org"
	answer := e.docker.answer
	e.docker.answer = func(a string) ([]byte, bool, error) {
		switch {
		// The read as root with the read-search capability alone, the write as the front's user.
		case strings.Contains(a, " run --rm --no-deps -T --user 0 --cap-add DAC_READ_SEARCH --entrypoint sh "+FrontService+" -c "+frontCAReadShell):
			return root, true, nil
		case strings.Contains(a, " run --rm --no-deps -T --entrypoint sh "+FrontService+" -c "+frontCAWriteShell):
			return nil, true, nil
		}
		if answer != nil {
			return answer(a)
		}
		return nil, false, nil
	}
	return e
}

func TestTheFrontCAGoesThroughADisasterRecovery(t *testing.T) {
	t.Parallel()
	cert, key := testRoot(t, true)
	e := frontCAEnv(t, append(append([]byte{}, cert...), key...))
	if err := e.install(func(o *Options) { o.TLS = "internal" }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	f, err := e.installer(nil).exportSecrets(context.Background(), e.readEnv())
	if err != nil || f.FrontCA == nil || f.FrontCA.Cert != string(cert) || f.FrontCA.Key != string(key) {
		t.Fatalf("export: %v %+v", err, f.FrontCA)
	}
	plain, err := yaml.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	// The machine made again: the root goes into the front's volume before the first up.
	e.dir = filepath.Join(t.TempDir(), "notes")
	e.docker.calls = nil
	if err := e.install(func(o *Options) { o.TLS = "internal"; o.Secrets = plain }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	var rm, load, up = -1, -1, -1
	for i, c := range e.docker.calls {
		a := strings.Join(c.Args, " ")
		switch {
		case strings.HasSuffix(a, " rm --stop --force "+FrontService) && rm < 0:
			rm = i
		case strings.HasSuffix(a, frontCAWriteShell) && load < 0:
			load = i
			if raw, _ := readAllOf(c); string(raw) != string(cert)+string(key) {
				t.Errorf("the restore wrote:\n%s", raw)
			}
		case slices.Contains(c.Args, "--detach") && up < 0: // the front's first start
			up = i
		}
	}
	if rm < 0 || load < rm || up < load {
		t.Fatalf("rm %d, load %d, up %d: the root is not in the volume before the front first starts", rm, load, up)
	}
	if _, err := os.Stat(filepath.Join(e.dir, FrontCARestore)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the record stayed once loaded: %v", err)
	}
	if !strings.Contains(e.log.String(), "the front's local CA is the one --secrets gave") {
		t.Errorf("not said:\n%s", e.log)
	}
	// A start without a record loads nothing.
	e.docker.calls = nil
	if err := e.install(nil); err != nil {
		t.Fatalf("re-run: %v\n%s", err, e.log)
	}
	for _, c := range e.docker.calls {
		if strings.HasSuffix(strings.Join(c.Args, " "), frontCAWriteShell) {
			t.Fatal("a re-run loaded the root again")
		}
	}
}

// readAllOf is what a call was given on standard input.
func readAllOf(c Cmd) ([]byte, error) {
	if c.Stdin == nil {
		return nil, nil
	}
	var b bytes.Buffer
	_, err := b.ReadFrom(c.Stdin)
	return b.Bytes(), err
}

// A certificate and key without their last line break (a YAML block stripped of it) are
// recorded each on lines of their own, as the write splits them.
func TestARestoredFrontCAIsRecordedLineByLine(t *testing.T) {
	t.Parallel()
	cert, key := testRoot(t, true)
	e := frontCAEnv(t, nil)
	plain, _ := yaml.Marshal(SecretsFile{Project: "notes", Secrets: map[string]string{}, FrontCA: &FrontCA{Cert: strings.TrimSuffix(string(cert), "\n"), Key: strings.TrimSuffix(string(key), "\n")}})
	in := e.installer(func(o *Options) { o.Secrets = plain })
	f, err := in.readSecrets(plain)
	if err != nil {
		t.Fatal(err)
	}
	in.restored = f
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := in.writeRestoredFrontCA("internal"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(e.dir, FrontCARestore)); string(got) != string(cert)+string(key) {
		t.Errorf("recorded:\n%s", got)
	}
}

func TestAFrontCALoadThatFailsIsTriedAgain(t *testing.T) {
	t.Parallel()
	cert, key := testRoot(t, true)
	e := frontCAEnv(t, nil)
	plain, err := yaml.Marshal(SecretsFile{Project: "notes", Secrets: map[string]string{}, FrontCA: &FrontCA{Cert: string(cert), Key: string(key)}})
	if err != nil {
		t.Fatal(err)
	}
	answer := e.docker.answer
	failing := true
	e.docker.answer = func(a string) ([]byte, bool, error) {
		if failing && strings.HasSuffix(a, frontCAWriteShell) {
			return nil, true, errors.New("no space left")
		}
		return answer(a)
	}
	if err := e.install(func(o *Options) { o.TLS = "internal"; o.Secrets = plain }); err == nil || !strings.Contains(err.Error(), "the front's local CA, into its volume: no space left") {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if _, err := os.Stat(filepath.Join(e.dir, FrontCARestore)); err != nil {
		t.Fatalf("the record went with the load: %v", err)
	}
	failing = false
	if err := e.install(nil); err != nil {
		t.Fatalf("re-run: %v\n%s", err, e.log)
	}
	if _, err := os.Stat(filepath.Join(e.dir, FrontCARestore)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the re-run did not load it: %v", err)
	}
}

func TestAFrontCAOutsideTheInternalModeIsLeftOut(t *testing.T) {
	t.Parallel()
	cert, key := testRoot(t, true)
	e := frontCAEnv(t, append(append([]byte{}, cert...), key...))
	if err := e.install(nil); err != nil {
		t.Fatal(err)
	}
	f, err := e.installer(nil).exportSecrets(context.Background(), e.readEnv())
	if err != nil || f.FrontCA != nil {
		t.Fatalf("loopback mode exported %v %+v", err, f.FrontCA)
	}
	f.FrontCA = &FrontCA{Cert: string(cert), Key: string(key)}
	plain, _ := yaml.Marshal(f)
	e.dir = filepath.Join(t.TempDir(), "notes")
	if err := e.install(func(o *Options) { o.Secrets = plain }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	if _, err := os.Stat(filepath.Join(e.dir, FrontCARestore)); !errors.Is(err, os.ErrNotExist) || !strings.Contains(e.log.String(), "the front's local CA is left out: this install's TLS mode is none, not internal") {
		t.Errorf("%v\n%s", err, e.log)
	}
}

func TestAnExportOfAFrontWithoutACA(t *testing.T) {
	t.Parallel()
	for what, c := range map[string]struct {
		out  []byte
		want string
	}{
		"none yet": {nil, ""},
		"garbage":  {[]byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), "warning: the front's local CA (" + localCADir + ")"},
	} {
		e := frontCAEnv(t, c.out)
		if err := e.install(func(o *Options) { o.TLS = "internal" }); err != nil {
			t.Fatal(err)
		}
		f, err := e.installer(nil).exportSecrets(context.Background(), e.readEnv())
		// The secrets go out either way: a CA that cannot be read is a warning.
		said := cmp.Or(c.want, "the front has made no local CA yet")
		if err != nil || f.FrontCA != nil || f.Secrets["SESSION_SECRET"] == "" || !strings.Contains(e.log.String(), said) {
			t.Errorf("%s: %v %+v\n%s", what, err, f, e.log)
		}
	}
}

func TestASecretsFileWithABadFrontCAIsRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cert, _ := testRoot(t, true)
	_, other := testRoot(t, true)
	plain, _ := yaml.Marshal(SecretsFile{Project: "notes", Secrets: map[string]string{}, FrontCA: &FrontCA{Cert: string(cert), Key: string(other)}})
	if _, err := e.installer(nil).readSecrets(plain); err == nil || !strings.Contains(err.Error(), "--secrets: front_ca: the key does not go with the certificate") {
		t.Errorf("%v", err)
	}
}
