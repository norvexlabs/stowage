//go:build dockersmoke

// A real Docker run of the installer: a throwaway registry, a one-service stack, the front, and a
// request through it. Run with `make smoke-install` (needs Docker).
package install

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/norvexlabs/stowage/internal/front"
	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/project"
	publisher "github.com/norvexlabs/stowage/internal/publish"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
)

const (
	smokeRegistry = "ghcr.io/project-zot/zot-linux-amd64:v2.1.22@sha256:46f688dc26315a35a247e1784368829e66a67bf91de94c7d8d1645044fac1d5b"
	smokeImage    = "busybox:1.37@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
)

func sh(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestSmokeInstallServesThroughTheFront(t *testing.T) {
	work := t.TempDir()
	regPort := freePort(t)
	_ = os.MkdirAll(filepath.Join(work, "zot"), 0o755)
	_ = os.WriteFile(filepath.Join(work, "zot/config.json"), []byte(`{"storage":{"rootDirectory":"/etc/zot/data"},"http":{"address":"0.0.0.0","port":"5000"},"log":{"level":"warn"}}`), 0o644)
	cname := fmt.Sprintf("stowage-smoke-zot-%d", os.Getpid())
	sh(t, "docker", "run", "-d", "--name", cname, "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "-p", fmt.Sprintf("127.0.0.1:%d:5000", regPort), "-v", filepath.Join(work, "zot")+":/etc/zot", smokeRegistry, "serve", "/etc/zot/config.json")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", cname).Run() })
	reg := fmt.Sprintf("127.0.0.1:%d", regPort)
	for i := 0; i < 50; i++ {
		if r, err := http.Get("http://" + reg + "/v2/"); err == nil {
			r.Body.Close()
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	sh(t, "docker", "pull", "-q", smokeImage)
	repo := reg + "/smoke/notes-web"
	sh(t, "docker", "tag", smokeImage, repo+":1")
	sh(t, "docker", "push", "-q", repo+":1")
	ref, _ := name.ParseReference(repo + ":1")
	desc, err := remote.Head(ref)
	if err != nil {
		t.Fatal(err)
	}
	// The front, built from its recipe by ship's builder, published next to the project's images.
	frontRepo := reg + "/smoke/notes-" + front.Key
	b := &publisher.DockerBuilder{WorkDir: filepath.Join(work, "build"), Stderr: os.Stderr}
	fimg, cleanup, err := b.Build(context.Background(), &project.Project{Spec: &spec.Spec{Name: "notes"}},
		publisher.BuildRequest{Image: front.Key, Inputs: fmt.Sprintf("sha256:smoke%d", os.Getpid()), Recipe: front.Dockerfile})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatal(err)
	}
	fref, _ := name.ParseReference(frontRepo + ":1")
	if err := remote.Write(fref, fimg); err != nil {
		t.Fatal(err)
	}
	fdesc, err := remote.Head(fref)
	if err != nil {
		t.Fatal(err)
	}

	appPort := freePort(t)
	s := &spec.Spec{Name: "notes",
		Registry: spec.Registry{Prefix: reg + "/smoke", Pull: spec.Provider{Name: "none"}},
		Images:   map[string]spec.Image{"web": {Ref: smokeImage}},
		Front: spec.Front{Portal: "app", TLSModes: []string{"byo", "internal", "none"}, LoopbackPorts: map[string]int{"app": appPort},
			Routes:    []spec.Route{{Name: "app", DomainInput: "APP_DOMAIN", Upstream: "web:8080", StripHeaders: []string{"X-Auth-Request-*", "X-Tenant"}}},
			PublishCA: &spec.PublishCA{Volume: "ca"}},
		// The hook mints a token once (it comes back on the next run), reads a recorded value,
		// reaches the stack on its network and reports its capabilities.
		Hooks: spec.Hooks{Provision: &spec.Provider{Name: "container", Config: map[string]any{
			"image": "web", "outputs": "credentials.txt", "env": []any{}, // it reads only what it returned and the URL
			"cmd": []any{"sh", "-c", `echo provisioning >&2
echo "TOKEN=${TOKEN:-$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')}"
echo "SEEN_URL=$STOWAGE_URL_APP"
echo "PAGE=$(wget -qO- http://web:8080/)"
awk '/^CapEff/{print "CAPS="$2}' /proc/self/status`},
		}}},
		// The "database" is busybox with a pg_dump; the schema command reads the migration number
		// and the rollback barriers the web service publishes.
		Database: &spec.Database{Engine: "postgres", Service: "postgres", Name: "notes", Roles: spec.Roles{Migrate: "notes", App: "notes"},
			SchemaCompat: spec.Provider{Name: "command", Config: map[string]any{"image": "web", "cmd": []any{"sh", "-c", `req=$(cat)
case "$req" in
*schema.current*) echo "{\"version\": \"$(wget -qO- http://web:8080/schema)\"}" ;;
*schema.barrier_after*) echo "{\"barriers\": [$(wget -qO- http://web:8080/barriers)]}" ;;
*schema.reads_newer*) echo '{"reads_newer": true}' ;;
*) exit 2 ;;
esac`}}}},
	}
	// release v is at migration v; barriers is the JSON list of rollback barriers it carries.
	build := func(v int, barriers string) (*Release, string) {
		version := fmt.Sprintf("%d.0.0", v)
		m := &release.Manifest{SchemaVersion: release.SchemaVersion, Name: "notes", ShortName: "notes", ProjectVersion: version, Revision: 1, ReleaseSeq: v, Channel: "stable", Tag: version,
			Date: time.Now(), Inputs: "sha256:in", Scheme: release.Scheme{Provider: "semver"},
			Artifacts: release.Artifacts{ // pulled by digest: the IDs are not read
				Images:    map[string]string{"web": desc.Digest.String(), front.Key: fdesc.Digest.String()},
				Manifests: map[string]string{"web": desc.Digest.String(), front.Key: fdesc.Digest.String()},
				Configs:   map[string]string{"web": desc.Digest.String(), front.Key: fdesc.Digest.String()}}}
		compose := fmt.Sprintf(`services:
  web:
    image: ${STOWAGE_IMAGE_WEB}
    command: ["sh", "-c", "mkdir -p /www && echo \"url=$$PUBLIC_URL v=%d\" > /www/index.html && echo %d > /www/schema && echo '%s' > /www/barriers && exec httpd -f -p 8080 -h /www"]
    environment:
      PUBLIC_URL: ${STOWAGE_URL_APP}
    volumes:
      - ca:/ca:ro
  postgres:
    image: ${STOWAGE_IMAGE_WEB}
    command: ["sh", "-c", "mkdir -p /usr/local/bin && printf '#!/bin/sh\\necho \"-- dump of $$3\"\\n' > /usr/local/bin/pg_dump && chmod +x /usr/local/bin/pg_dump && exec sleep 86400"]
volumes:
  ca: {}
`, v, v, strings.ReplaceAll(barriers, `"`, `\"`))
		r := &Release{Manifest: m, Spec: s, Compose: []byte(compose)}
		man, err := m.Encode()
		if err != nil {
			t.Fatal(err)
		}
		specJSON, _ := json.Marshal(s)
		var buf bytes.Buffer
		if err := payload.Write(&buf, strings.NewReader("exe"), payload.Files{payload.Release: man, payload.Spec: specJSON, payload.Compose: []byte(compose)}); err != nil {
			t.Fatal(err)
		}
		exe := filepath.Join(work, "notes-stow-"+version)
		if err := os.WriteFile(exe, buf.Bytes(), 0o755); err != nil {
			t.Fatal(err)
		}
		return r, exe
	}
	r, self := build(1, "")
	smokeCloudflareCaddyfile(t, work, frontRepo+"@"+fdesc.Digest.String(), s.Front)

	dir := filepath.Join(work, "stowage-smoke-notes")
	var out, log bytes.Buffer
	in := &Installer{R: r, O: Options{Dir: dir, Out: &out, Log: &log, Docker: ExecTool{Bin: "docker"}, Self: self}}
	t.Cleanup(func() {
		env, err := ReadEnvFile(filepath.Join(dir, ".env"))
		if err == nil {
			_, _ = in.Compose(context.Background(), env, io.Discard, "down", "--volumes")
		}
	})
	if err := in.Install(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", appPort))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	want := fmt.Sprintf("url=http://localhost:%d v=1", appPort)
	if resp.StatusCode != 200 || strings.TrimSpace(string(body)) != want {
		t.Fatalf("GET through the front: %d %q, want %q\n%s", resp.StatusCode, body, want, log.String())
	}

	outs, err := ReadEnvFile(filepath.Join(dir, ProvisionEnv))
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	token := get(outs, "TOKEN")
	if len(token) != 16 || get(outs, "SEEN_URL") != fmt.Sprintf("http://localhost:%d", appPort) ||
		get(outs, "PAGE") != "url="+get(outs, "SEEN_URL")+" v=1" || get(outs, "CAPS") != "0000000000000000" {
		t.Fatalf("hook outputs: %v %q %q %q %q\n%s", outs.Keys(), token, get(outs, "SEEN_URL"), get(outs, "PAGE"), get(outs, "CAPS"), log.String())
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "credentials.txt")); err != nil || !strings.Contains(string(raw), "TOKEN="+token+"\n") {
		t.Fatalf("credentials.txt: %q %v", raw, err)
	}

	// A re-run brings the same stack up again, with nothing re-decided, and provisions again:
	// the hook gets its token back.
	if err := in.Install(context.Background()); err != nil {
		t.Fatalf("re-run: %v\n%s", err, log.String())
	}
	if outs, _ := ReadEnvFile(filepath.Join(dir, ProvisionEnv)); get(outs, "TOKEN") != token {
		t.Fatalf("the re-run minted another token: %q, was %q", get(outs, "TOKEN"), token)
	}
	// A re-run given another port moves the front there, and the URL the stack reads with it; the
	// next moves it back.
	for _, port := range []int{freePort(t), appPort} {
		in.O.Ports = map[string]int{"app": port}
		if err := in.Install(context.Background()); err != nil {
			t.Fatalf("re-run --ports app=%d: %v\n%s", port, err, log.String())
		}
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err != nil {
			t.Fatalf("after --ports app=%d: %v\n%s", port, err, log.String())
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if want := fmt.Sprintf("url=http://localhost:%d v=1", port); strings.TrimSpace(string(body)) != want {
			t.Fatalf("after --ports app=%d: %q, want %q", port, body, want)
		}
	}
	in.O.Ports = nil
	// Another install, in loopback mode on the IPv6 loopback address: compose takes the bare
	// address in the port, the stack its bracketed URL.
	dir6 := filepath.Join(work, "stowage-smoke-notes-v6")
	port6 := freePort(t)
	// With --ipv6: the stack's network has IPv6, and the front an IPv6 address on it.
	ipv6 := true
	in6 := &Installer{R: r, O: Options{Dir: dir6, Out: io.Discard, Log: &log, Docker: ExecTool{Bin: "docker"}, Self: self, Address: "::1", Ports: map[string]int{"app": port6}, IPv6: &ipv6}}
	t.Cleanup(func() {
		if env, err := ReadEnvFile(filepath.Join(dir6, ".env")); err == nil {
			_, _ = in6.Compose(context.Background(), env, io.Discard, "down", "--volumes")
		}
	})
	if err := in6.Install(context.Background()); err != nil {
		t.Fatalf("install on ::1: %v\n%s", err, log.String())
	}
	if resp, err := http.Get(fmt.Sprintf("http://[::1]:%d/", port6)); err != nil {
		t.Fatalf("GET on ::1: %v", err)
	} else {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if want := fmt.Sprintf("url=http://[::1]:%d v=1", port6); strings.TrimSpace(string(body)) != want {
			t.Fatalf("GET on ::1: %q, want %q", body, want)
		}
	}
	env6, _ := ReadEnvFile(filepath.Join(dir6, ".env"))
	project6, _ := env6.Get("COMPOSE_PROJECT_NAME")
	net6, err := exec.Command("docker", "network", "inspect", "--format", "{{.EnableIPv6}}", project6+"_default").Output()
	if err != nil || strings.TrimSpace(string(net6)) != "true" {
		t.Fatalf("the stack's network: IPv6 %q %v", net6, err)
	}
	front6, err := exec.Command("docker", "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.GlobalIPv6Address}}{{end}}", project6+"-"+FrontService+"-1").Output()
	if err != nil || !strings.Contains(string(front6), ":") {
		t.Fatalf("the front's IPv6 address: %q %v", front6, err)
	}
	if _, err := in6.Compose(context.Background(), env6, io.Discard, "down", "--volumes"); err != nil {
		t.Fatalf("down on ::1: %v", err)
	}
	out.Reset()
	if err := in.Status(context.Background()); err != nil || !strings.Contains(out.String(), "stowage-front") {
		t.Fatalf("status: %v\n%s", err, out.String())
	}

	// Upgrade to 2 (migration 2), go back to 1: the database moved, no barrier, and 1 reads newer.
	page := func() string {
		t.Helper()
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", appPort))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return strings.TrimSpace(string(b))
	}
	// step runs the installer of version, which carries barriers, then the stack serves serves.
	step := func(what string, version int, barriers string, run func(*Installer) error, serves int) error {
		t.Helper()
		r, self := build(version, barriers)
		in := &Installer{R: r, O: Options{Dir: dir, Out: &out, Log: &log, Docker: ExecTool{Bin: "docker"}, Self: self}}
		err := run(in)
		if want := fmt.Sprintf("v=%d", serves); !strings.HasSuffix(page(), want) {
			t.Fatalf("%s (%v): serves %q, want %s\n%s", what, err, page(), want, log.String())
		}
		return err
	}
	upgrade := func(in *Installer) error { return in.Upgrade(context.Background()) }
	rollback := func(in *Installer) error { return in.Rollback(context.Background()) }
	if err := step("upgrade to 2", 2, "", upgrade, 2); err != nil {
		t.Fatalf("upgrade to 2: %v\n%s", err, log.String())
	}
	snaps, _ := filepath.Glob(filepath.Join(dir, SnapshotsDir, "*.sql.gz"))
	if len(snaps) != 1 {
		t.Fatalf("snapshots: %v", snaps)
	}
	if f, err := os.Open(snaps[0]); err != nil {
		t.Fatal(err)
	} else if zr, err := gzip.NewReader(f); err != nil {
		t.Fatal(err)
	} else if dump, _ := io.ReadAll(zr); strings.TrimSpace(string(dump)) != "-- dump of notes" {
		t.Fatalf("snapshot: %q", dump)
	}
	if outs, _ := ReadEnvFile(filepath.Join(dir, ProvisionEnv)); get(outs, "TOKEN") != token || !strings.HasSuffix(get(outs, "PAGE"), "v=2") {
		t.Fatalf("not provisioned again after the upgrade: %v", outs.Keys())
	}
	if err := step("rollback to 1", 2, "", rollback, 1); err != nil {
		t.Fatalf("rollback to 1: %v\n%s", err, log.String())
	}
	// Upgrade to 3, whose migration 3 is a rollback barrier: going back is refused.
	if err := step("upgrade to 3", 3, `"3 (drop the old table)"`, upgrade, 3); err != nil {
		t.Fatalf("upgrade to 3: %v\n%s", err, log.String())
	}
	err = step("rollback from 3", 3, `"3 (drop the old table)"`, rollback, 3)
	if err == nil || !strings.Contains(err.Error(), "the database carries 3 (drop the old table)") || !strings.Contains(err.Error(), filepath.Join(dir, SnapshotsDir)) {
		t.Fatalf("rollback over a barrier: %v\n%s", err, log.String())
	}

	// Behind TLS, on other ports than 80 and 443: the local CA, then the operator's certificate and
	// its CA. Each one's CA reaches the stack (publish_ca), and serves: what the front presents
	// chains to it. Then the byo install moves to internal, and back.
	ca, cert, key := writeCAChain(t, "notes.smoke.test")
	// served is the certificate the front presents on httpsPort once it chains to the CA the stack
	// reads (the web service's /ca/ca.crt), and that CA.
	served := func(in *Installer, httpsPort int) (*x509.Certificate, []byte) {
		t.Helper()
		env, err := ReadEnvFile(filepath.Join(in.O.Dir, ".env"))
		if err != nil {
			t.Fatal(err)
		}
		c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // checked below against the published CA
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", httpsPort))
			}}}
		var last error
		for i := 0; i < 100; i++ {
			time.Sleep(200 * time.Millisecond)
			// As a user other than root: the services read it.
			pub, err := in.Compose(context.Background(), env, nil, "exec", "-T", "-u", "65534", "web", "cat", "/ca/ca.crt")
			if err != nil {
				last = fmt.Errorf("the stack has no CA: %w", err)
				continue
			}
			resp, err := c.Get(fmt.Sprintf("https://notes.smoke.test:%d/", httpsPort))
			if err != nil {
				last = err
				continue
			}
			resp.Body.Close()
			roots := x509.NewCertPool()
			roots.AppendCertsFromPEM(pub)
			inter := x509.NewCertPool()
			for _, c := range resp.TLS.PeerCertificates[1:] {
				inter.AddCert(c)
			}
			leaf := resp.TLS.PeerCertificates[0]
			if _, last = leaf.Verify(x509.VerifyOptions{DNSName: "notes.smoke.test", Roots: roots, Intermediates: inter}); last == nil {
				return leaf, pub
			}
		}
		t.Fatalf("%s: what the front serves never chained to the published CA: %v\n%s", in.O.Dir, last, log.String())
		return nil, nil
	}
	for _, mode := range []string{"internal", "byo"} {
		dir := filepath.Join(work, "stowage-smoke-notes-"+mode)
		httpPort, httpsPort := freePort(t), freePort(t)
		out.Reset()
		in := &Installer{R: r, O: Options{Dir: dir, Out: &out, Log: &log, Docker: ExecTool{Bin: "docker"}, TLS: mode,
			Domains: map[string]string{"app": "notes.smoke.test"}, HTTPPort: httpPort, HTTPSPort: httpsPort}}
		if mode == "byo" {
			in.O.Cert, in.O.Key, in.O.CA = cert, key, ca
		}
		t.Cleanup(func() {
			if env, err := ReadEnvFile(filepath.Join(dir, ".env")); err == nil {
				_, _ = in.Compose(context.Background(), env, io.Discard, "down", "--volumes")
			}
		})
		if err := in.Install(context.Background()); err != nil {
			t.Fatalf("%s: %v\n%s", mode, err, log.String())
		}
		// The domain resolves to this host's published port; the certificate is not one a client trusts.
		c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // a test CA, checked below by name
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", httpsPort))
			}}}
		var resp *http.Response
		for i := 0; i < 50; i++ { // the local CA issues on the first handshake
			if resp, err = c.Get(fmt.Sprintf("https://notes.smoke.test:%d/", httpsPort)); err == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("%s: %v\n%s", mode, err, log.String())
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		want := fmt.Sprintf("url=https://notes.smoke.test:%d v=1", httpsPort)
		if resp.StatusCode != 200 || strings.TrimSpace(string(body)) != want {
			t.Fatalf("%s: GET through the front: %d %q, want %q", mode, resp.StatusCode, body, want)
		}
		if err := resp.TLS.PeerCertificates[0].VerifyHostname("notes.smoke.test"); err != nil {
			t.Errorf("%s: %v", mode, err)
		}
		// Plain HTTP is sent to the HTTPS port the host publishes.
		plain := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", httpPort))
			}}}
		if r, err := plain.Get(fmt.Sprintf("http://notes.smoke.test:%d/x", httpPort)); err != nil {
			t.Errorf("%s: plain HTTP: %v", mode, err)
		} else if r.Body.Close(); r.Header.Get("Location") != fmt.Sprintf("https://notes.smoke.test:%d/x", httpsPort) {
			t.Errorf("%s: plain HTTP: %d to %q", mode, r.StatusCode, r.Header.Get("Location"))
		}
		hsts := resp.Header.Get("Strict-Transport-Security")
		if (mode == "internal") != (hsts == "") {
			t.Errorf("%s: Strict-Transport-Security %q", mode, hsts)
		}
		if raw, _ := os.ReadFile(cert); mode == "byo" && !bytes.Contains(raw, pemBody(resp.TLS.PeerCertificates[0].Raw)) {
			t.Errorf("byo served another certificate than the operator's")
		}
		_, pub := served(in, httpsPort)
		if want, _ := os.ReadFile(ca); mode == "byo" && !bytes.Equal(pub, want) {
			t.Errorf("byo published %q, not the --ca given", pub)
		}
		if mode != "byo" {
			continue
		}
		// The byo install moves to the local CA: its certificate and key go, the local CA's root
		// is published; then back to the operator's certificate.
		in.O.TLS, in.O.Cert, in.O.Key, in.O.CA, in.O.Domains, in.O.HTTPPort, in.O.HTTPSPort = "internal", "", "", "", nil, 0, 0
		if err := in.Install(context.Background()); err != nil {
			t.Fatalf("byo to internal: %v\n%s", err, log.String())
		}
		leaf, pub := served(in, httpsPort)
		raw, _ := os.ReadFile(cert)
		if want, _ := os.ReadFile(ca); bytes.Contains(raw, pemBody(leaf.Raw)) || bytes.Equal(pub, want) {
			t.Errorf("after the move to internal the front still serves the operator's certificate, or the stack reads its CA")
		}
		if _, err := os.Stat(filepath.Join(dir, TLSDir)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("tls/ stayed after the move to internal: %v", err)
		}
		in.O.TLS, in.O.Cert, in.O.Key, in.O.CA = "byo", cert, key, ca
		if err := in.Install(context.Background()); err != nil {
			t.Fatalf("internal to byo: %v\n%s", err, log.String())
		}
		leaf, pub = served(in, httpsPort)
		if !bytes.Contains(raw, pemBody(leaf.Raw)) {
			t.Errorf("back in byo the front serves another certificate than the operator's")
		}
		if want, _ := os.ReadFile(ca); !bytes.Equal(pub, want) {
			t.Errorf("back in byo the stack reads %q, not the --ca given", pub)
		}
	}
	smokeIPv6Client(t, r, work, &log)
}

// smokeIPv6Client installs the stack behind TLS with an allowlist on its route, and calls the front
// from this host's own IPv6 address: with --ipv6 the front sees that address, so an allowlist
// holding it serves and one without it refuses (403); without --ipv6 the front sees the bridge's
// IPv4 gateway, which the same allowlist refuses. A host with no global IPv6 address skips it.
func smokeIPv6Client(t *testing.T, r *Release, work string, log *bytes.Buffer) {
	t.Helper()
	h6 := hostIPv6()
	if !h6.IsValid() {
		t.Log("this host has no global IPv6 address: the IPv6 client is not checked")
		return
	}
	s := *r.Spec
	s.Front.Routes = slices.Clone(s.Front.Routes)
	s.Front.Routes[0].AllowInput = "ALLOW"
	s.Inputs = append(slices.Clone(s.Inputs), spec.Input{Key: "ALLOW", Optional: true})
	r6 := *r
	r6.Spec = &s
	dir := filepath.Join(work, "stowage-smoke-notes-ipv6-client")
	httpPort, httpsPort := freePort(t), freePort(t)
	in := &Installer{R: &r6, O: Options{Dir: dir, Out: io.Discard, Log: log, Docker: ExecTool{Bin: "docker"}, TLS: "internal",
		Domains: map[string]string{"app": "notes.smoke.test"}, HTTPPort: httpPort, HTTPSPort: httpsPort}}
	t.Cleanup(func() {
		if env, err := ReadEnvFile(filepath.Join(dir, ".env")); err == nil {
			_, _ = in.Compose(context.Background(), env, io.Discard, "down", "--volumes")
		}
	})
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // the local CA: what is checked is the status
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp6", net.JoinHostPort(h6.String(), strconv.Itoa(httpsPort)))
		}}}
	self := netip.PrefixFrom(h6, 128).String()
	on, off := true, false
	for _, c6 := range []struct {
		what  string
		ipv6  *bool
		allow string
		want  int
	}{
		{"--ipv6, the allowlist holds this host", &on, self, 200},
		{"--ipv6, the allowlist holds another network", &on, "2001:db8::/32", 403},
		{"--ipv6=false, the allowlist holds this host", &off, self, 403},
	} {
		in.O.IPv6, in.O.Set = c6.ipv6, map[string]string{"ALLOW": c6.allow}
		if err := in.Install(context.Background()); err != nil {
			t.Fatalf("%s: %v\n%s", c6.what, err, log.String())
		}
		in.O.Domains, in.O.HTTPPort, in.O.HTTPSPort = nil, 0, 0 // a re-run keeps what the install recorded
		var resp *http.Response
		var err error
		// The local CA issues on the first handshake; the upstream may still be starting (5xx).
		for i := 0; i < 50; i++ {
			if resp, err = c.Get(fmt.Sprintf("https://notes.smoke.test:%d/", httpsPort)); err == nil {
				if resp.Body.Close(); resp.StatusCode < 500 {
					break
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("%s: GET from %s: %v", c6.what, h6, err)
		}
		if resp.StatusCode != c6.want {
			t.Fatalf("%s: GET from %s: %d, want %d", c6.what, h6, resp.StatusCode, c6.want)
		}
	}
}

// hostIPv6 is the global IPv6 address this host sends from (a UDP socket connected to a global
// address: the kernel picks the source, a usable one, and nothing is sent); the zero Addr when it
// has none.
func hostIPv6() netip.Addr {
	c, err := net.Dial("udp6", "[2001:4860:4860::8888]:53")
	if err != nil {
		return netip.Addr{}
	}
	defer c.Close()
	a := c.LocalAddr().(*net.UDPAddr).AddrPort().Addr()
	if !a.IsGlobalUnicast() || a.Is4In6() {
		return netip.Addr{}
	}
	return a
}

// The dns-cloudflare Caddyfile is one the published front loads: its Cloudflare plugin is there and
// takes the token from the environment, in the user-token form longer than a classic token. Issuing would need a real zone.
func smokeCloudflareCaddyfile(t *testing.T, work, frontRef string, f spec.Front) {
	t.Helper()
	dir := filepath.Join(work, "dns-cloudflare")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The route asks an authentication service, serves an allowlist only and requires a client
	// certificate the client CA issued: Caddy takes all three, the CA loaded where the front reads it.
	// Another asks for a login, beside its own allowlist.
	f.Routes = slices.Clone(f.Routes)
	f.Routes[0].ForwardAuth = &spec.ForwardAuth{AddressInput: "AUTH", URIInput: "AUTH_URI", CopyHeaders: []string{"X-Auth-Request-User"}}
	f.Routes[0].AllowInput = "ALLOW"
	f.Routes[0].ClientAuth = true
	f.Routes[1].BasicAuth = &spec.BasicAuth{User: "ops", Secret: "OPS_PASSWORD"}
	f.Routes[1].AllowInput = "ALLOW"
	vals := map[string]string{"AUTH": "http://auth:4180", "AUTH_URI": "/oauth2/auth?rd=x", "ALLOW": "10.0.0.0/8, 192.0.2.7", "OPS_PASSWORD": "secret"}
	logins, err := (&Installer{O: Options{Dir: work}}).logins(f, func(k string) string { return vals[k] }, work)
	if err != nil {
		t.Fatal(err)
	}
	cf := tlsCaddyfile(f, "dns-cloudflare", map[string]string{"app": "notes.smoke.test", "api": "api.smoke.test"}, "ops@example.com", "80", "443", true, func(k string) string { return vals[k] }, logins)
	if !strings.Contains(cf, "client_auth") || !strings.Contains(cf, "basic_auth {\n\t\t\tops $2a$") {
		t.Fatalf("no client authentication or login in:\n%s", cf)
	}
	ca, _, _ := writeCAChain(t, "client")
	if err := os.Chmod(ca, 0o644); err != nil { // the front reads it as its own user
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Caddyfile"), []byte(cf), 0o644); err != nil {
		t.Fatal(err)
	}
	out := sh(t, "docker", "run", "--rm", "-e", "CLOUDFLARE_API_TOKEN=cfut_"+strings.Repeat("x", 60), "-v", dir+":/c:ro", "-v", ca+":"+clientCAPath+":ro", frontRef,
		"caddy", "validate", "--config", "/c/Caddyfile", "--adapter", "caddyfile")
	if !strings.Contains(out, "Valid configuration") {
		t.Fatalf("the dns-cloudflare Caddyfile:\n%s\n%s", cf, out)
	}
}

// pemBody is the base64 text a PEM file carries for der.
func pemBody(der []byte) []byte {
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return bytes.Join(bytes.Split(b, []byte("\n"))[1:3], []byte("\n"))
}
