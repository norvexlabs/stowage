// Package sbom makes a release's software bills of materials and scans them: syft writes an
// image's SPDX document, which ship attests with the release key, and grype matches a document
// against its vulnerability database. Both run as pinned images through the Docker CLI.
package sbom

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Syft and Grype are the pinned images that make and scan the documents. Grype is also a release's
// image Key, so a mirror carries it to an install that audits without reaching the internet: its
// linux/amd64 image alone (a release runs on that platform), a Docker v2 manifest.
const (
	Syft  = "docker.io/anchore/syft:v1.55.0@sha256:4cc92b64ca74ffca4c94d4c8fcc01f217731375300be14d0b6eeb2e82eaa110a"
	Grype = "docker.io/anchore/grype:v0.121.0@sha256:d39f3ac8a9f55477aadacd051cc4199a9d2dda30600ef00fdf5c406c9f40f0b7"
	Key   = "audit"
)

// PredicateType is the in-toto predicate type of an SPDX document, and CosignType the name
// `cosign attest --type` and `cosign verify-attestation --type` give it.
const (
	PredicateType = "https://spdx.dev/Document"
	CosignType    = "spdxjson"
)

// Platform is the one a release's images run on, which syft reads of an index.
const Platform = "linux/amd64"

// MaxDocument caps a document read or written: an OS's runs to a few MiB.
const MaxDocument = 64 << 20

// MaxReport caps grype's answer for one document.
const MaxReport = 64 << 20

// Docker runs the Docker CLI with args, its standard output to stdout.
type Docker func(ctx context.Context, stdout io.Writer, args ...string) error

// DockerCLI runs the docker CLI; its error carries the end of what it wrote to stderr.
func DockerCLI(ctx context.Context, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = stdout
	var errOut tail
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		if last := lastLines(errOut.String(), 5); last != "" {
			return fmt.Errorf("%w: %s", err, last)
		}
		return err
	}
	return nil
}

// lastLines are the last n lines of s that hold something, joined by " / ".
func lastLines(s string, n int) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep[max(len(keep)-n, 0):], " / ")
}

// tail keeps the last 16 KiB written to it: a tool's stderr ends with its error.
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if over := len(t.b) - 16<<10; over > 0 {
		t.b = append(t.b[:0:0], t.b[over:]...)
	}
	return len(p), nil
}

func (t *tail) String() string { return string(t.b) }

// Source is where syft reads an image: a reference by digest, over plain HTTP for a loopback
// registry, with the Docker config in Config (a directory; "" for none).
type Source struct {
	Ref      string
	Insecure bool
	Config   string
}

// Generate is the SPDX document of the image src names, as syft writes it.
func Generate(ctx context.Context, docker Docker, src Source) ([]byte, error) {
	args := []string{"run", "--rm", "--network", "host", "-e", "SYFT_CHECK_FOR_APP_UPDATE=false"}
	if src.Config != "" {
		m, err := bind(src.Config, "/config", true)
		if err != nil {
			return nil, err
		}
		args = append(args, "-e", "DOCKER_CONFIG=/config", "--mount", m)
	}
	if src.Insecure {
		args = append(args, "-e", "SYFT_REGISTRY_INSECURE_USE_HTTP=true")
	}
	// The platform a release runs on: an index's other platforms are none of the install's.
	args = append(args, Syft, "registry:"+src.Ref, "--platform", Platform, "-o", "spdx-json", "-q")
	out := &capped{max: MaxDocument}
	if err := docker(ctx, out, args...); err != nil {
		return nil, fmt.Errorf("syft %s: %w", src.Ref, err)
	}
	if out.over {
		return nil, fmt.Errorf("syft %s: a document of more than %d bytes", src.Ref, MaxDocument)
	}
	if err := Check(out.Bytes()); err != nil {
		return nil, fmt.Errorf("syft %s: %w", src.Ref, err)
	}
	return out.Bytes(), nil
}

// Check refuses what is not an SPDX JSON document.
func Check(doc []byte) error {
	var d struct {
		SPDXVersion string `json:"spdxVersion"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return fmt.Errorf("not an SPDX JSON document: %w", err)
	}
	if !strings.HasPrefix(d.SPDXVersion, "SPDX-") {
		return errors.New("not an SPDX JSON document: no spdxVersion")
	}
	return nil
}

// bind is a --mount of the host path src at dst: unlike -v, it takes a path holding a colon (grype's
// archives are named by their time); a comma, its own separator, is refused.
func bind(src, dst string, ro bool) (string, error) {
	if strings.Contains(src, ",") {
		return "", fmt.Errorf("%s: a path with a comma cannot be mounted", src)
	}
	m := "type=bind,src=" + src + ",dst=" + dst
	if ro {
		m += ",readonly"
	}
	return m, nil
}

// mount is a bind mount, read-only or not.
type mount struct {
	src, dst string
	ro       bool
}

// capped is a buffer that keeps at most max bytes and says when it was given more.
type capped struct {
	bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); len(p) > room {
		c.over = true
		c.Buffer.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return c.Buffer.Write(p)
}

// Scanner runs grype on documents with its database in Cache, a directory of the caller's,
// which keeps it from one run to the next. User is the uid:gid grype runs as ("" for the
// image's).
type Scanner struct {
	Docker Docker
	Image  string // grype's image: Grype, or a release's copy of it
	Cache  string
	User   string
	// DB is a database archive to import first (grype's listing has them), for a host that does
	// not reach grype's servers: the scan then never fetches one.
	DB string
}

// run runs grype with everything it writes under the cache, which the image's user may write
// (its /tmp it may not).
func (s Scanner) run(ctx context.Context, stdout io.Writer, mounts []mount, args ...string) error {
	if err := os.MkdirAll(filepath.Join(s.Cache, "tmp"), 0o700); err != nil {
		return err
	}
	a := []string{"run", "--rm", "-e", "HOME=/db", "-e", "TMPDIR=/db/tmp", "-e", "GRYPE_DB_CACHE_DIR=/db/cache",
		"-e", "GRYPE_CHECK_FOR_APP_UPDATE=false"}
	if s.User != "" {
		a = append(a, "--user", s.User)
	}
	// The database is made ready once, by Import: a scan never checks for a newer one. One given
	// (--db) is the operator's choice, whatever its age.
	a = append(a, "-e", "GRYPE_DB_AUTO_UPDATE=false")
	if s.DB != "" {
		a = append(a, "-e", "GRYPE_DB_VALIDATE_AGE=false")
	}
	for _, m := range append([]mount{{s.Cache, "/db", false}}, mounts...) {
		b, err := bind(m.src, m.dst, m.ro)
		if err != nil {
			return err
		}
		a = append(a, "--mount", b)
	}
	return s.Docker(ctx, stdout, append(append(a, cmp.Or(s.Image, Grype)), args...)...)
}

// Import makes the database ready, once before the scans: s.DB loaded into the cache, else the
// cache's brought up to date from grype's servers.
func (s Scanner) Import(ctx context.Context) error {
	if s.DB == "" {
		if err := s.run(ctx, io.Discard, nil, "db", "update"); err != nil {
			return fmt.Errorf("grype db update (`--db FILE` scans offline): %w", err)
		}
		return nil
	}
	abs, err := filepath.Abs(s.DB)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err != nil {
		return fmt.Errorf("--db: %w", err)
	}
	if err := s.run(ctx, io.Discard, []mount{{abs, "/in/db", true}}, "db", "import", "/in/db"); err != nil {
		return fmt.Errorf("grype db import %s: %w", s.DB, err)
	}
	return nil
}

// Scan matches doc against the database: its findings, and when the database was built.
func (s Scanner) Scan(ctx context.Context, doc []byte) ([]Finding, string, error) {
	if err := os.MkdirAll(filepath.Join(s.Cache, "tmp"), 0o700); err != nil {
		return nil, "", err
	}
	dir, err := os.MkdirTemp(filepath.Join(s.Cache, "tmp"), "sbom-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "sbom.json"), doc, 0o644); err != nil {
		return nil, "", err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return nil, "", err
	}
	out := &capped{max: MaxReport}
	if err := s.run(ctx, out, []mount{{dir, "/in", true}}, "sbom:/in/sbom.json", "-o", "json", "-q"); err != nil {
		return nil, "", fmt.Errorf("grype: %w", err)
	}
	if out.over {
		return nil, "", fmt.Errorf("grype: an answer of more than %d bytes", MaxReport)
	}
	return ParseGrype(out.Bytes())
}

// ParseGrype reads grype's JSON answer: its findings, sorted, and when its database was built.
func ParseGrype(raw []byte) ([]Finding, string, error) {
	var g struct {
		Matches []struct {
			Vulnerability struct {
				ID       string `json:"id"`
				Severity string `json:"severity"`
				Fix      struct {
					Versions []string `json:"versions"`
				} `json:"fix"`
			} `json:"vulnerability"`
			Artifact struct {
				Name    string `json:"name"`
				Version string `json:"version"`
				Type    string `json:"type"`
			} `json:"artifact"`
		} `json:"matches"`
		Descriptor struct {
			DB struct {
				Status struct {
					Built string `json:"built"`
				} `json:"status"`
			} `json:"db"`
		} `json:"descriptor"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, "", fmt.Errorf("grype's answer: %w", err)
	}
	out := make([]Finding, 0, len(g.Matches))
	for _, m := range g.Matches {
		out = append(out, Finding{ID: m.Vulnerability.ID, Severity: ParseSeverity(m.Vulnerability.Severity),
			Package: m.Artifact.Name, Version: m.Artifact.Version, Type: m.Artifact.Type, Fixed: m.Vulnerability.Fix.Versions})
	}
	sortFindings(out)
	return slices.CompactFunc(out, func(a, b Finding) bool {
		return a.ID == b.ID && a.Package == b.Package && a.Version == b.Version && a.Type == b.Type
	}), g.Descriptor.DB.Status.Built, nil
}
