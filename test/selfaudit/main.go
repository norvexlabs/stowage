// Command self-audit is the vulnerability gate of Stowage's own release: it builds every image
// Stowage supplies to a release (the front, the backup runner, the volume helper, the scanner)
// from the recipes this tree embeds, makes the SBOM of each and of Stowage's binaries (stowage,
// stowage-install, which the OS bundle carries), scans them with the pinned scanner and refuses,
// exit 1, on a fixable finding at or above high that no allowance in force accepts. It runs where
// Docker does: `make self-audit`, and the release workflow before anything is published.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/norvexlabs/stowage/internal/backup"
	"github.com/norvexlabs/stowage/internal/chown"
	"github.com/norvexlabs/stowage/internal/front"
	"github.com/norvexlabs/stowage/internal/sbom"
)

func main() {
	bin := flag.String("bin", "bin", "the directory holding stowage and stowage-install")
	allow := flag.String("allow", "audit-allow.yaml", "the allowlist file (sbom.ParseAllowances)")
	db := flag.String("db", "", "a grype database archive to scan with (default: grype's servers)")
	cache := flag.String("cache", "", "the scanner's database directory (default: the user cache's stowage/grype)")
	flag.Parse()
	if err := run(context.Background(), *bin, *allow, *db, *cache); err != nil {
		fmt.Fprintln(os.Stderr, "self-audit:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, bin, allowFile, db, cache string) error {
	now := time.Now().UTC()
	doc, err := os.ReadFile(allowFile)
	if err != nil {
		return err
	}
	allow, err := sbom.ParseAllowances(doc, now)
	if err != nil {
		return fmt.Errorf("%s: %w", allowFile, err)
	}
	if cache == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		cache = filepath.Join(dir, "stowage", "grype")
	}
	// The images' archives are kept on disk beside the database, never in a RAM-backed TMPDIR.
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return err
	}
	work, err := os.MkdirTemp(filepath.Dir(cache), "self-audit-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	// Each subject: its name as a release names it, and what syft reads.
	type subject struct{ name, file, source string }
	var subjects []subject
	for _, r := range []struct {
		key    string
		recipe []byte
	}{{front.Key, front.Dockerfile}, {backup.Key, backup.Dockerfile}, {chown.Key, chown.Dockerfile}, {sbom.Key, sbom.Dockerfile}} {
		tag := "stowage-self-audit/" + r.key + ":latest"
		fmt.Fprintf(os.Stderr, "building %s\n", r.key)
		if err := docker(ctx, bytes.NewReader(r.recipe), nil, "buildx", "build", "--platform", sbom.Platform, "--load", "-t", tag, "-"); err != nil {
			return fmt.Errorf("build %s: %w", r.key, err)
		}
		archive := r.key + ".tar"
		if err := docker(ctx, nil, nil, "image", "save", "-o", filepath.Join(work, archive), tag); err != nil {
			return fmt.Errorf("save %s: %w", r.key, err)
		}
		subjects = append(subjects, subject{r.key, archive, "docker-archive:/in/" + archive})
	}
	for _, b := range []string{"stowage", "stowage-install"} {
		data, err := os.ReadFile(filepath.Join(bin, b))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(work, b), data, 0o644); err != nil {
			return err
		}
		subjects = append(subjects, subject{b, b, "file:/in/" + b})
	}
	if err := os.Chmod(work, 0o755); err != nil {
		return err
	}

	docs := map[string][]byte{}
	var targets []sbom.Target
	for _, s := range subjects {
		fmt.Fprintf(os.Stderr, "making the SBOM of %s\n", s.name)
		var out bytes.Buffer
		if err := docker(ctx, nil, &out, "run", "--rm", "-e", "SYFT_CHECK_FOR_APP_UPDATE=false",
			"--mount", "type=bind,src="+work+",dst=/in,readonly", sbom.Syft, s.source,
			"--select-catalogers", "-linux-kernel-cataloger", "-o", "spdx-json", "-q"); err != nil {
			return fmt.Errorf("syft %s: %w", s.name, err)
		}
		// A target is named by the document's own digest: nothing here is in a registry.
		sum := sha256.Sum256(out.Bytes())
		d, err := name.NewDigest(fmt.Sprintf("stowage.local/self-audit/%s@sha256:%x", s.name, sum))
		if err != nil {
			return err
		}
		docs[d.String()] = out.Bytes()
		targets = append(targets, sbom.Target{Name: s.name, Digest: d})
	}
	read := func(_ context.Context, d name.Digest) ([]byte, error) { return docs[d.String()], nil }
	s := sbom.Scanner{Docker: sbom.DockerCLI, Cache: cache, User: fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), DB: db}
	rep, err := sbom.Audit(ctx, read, s, targets, func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) })
	if err != nil {
		return err
	}
	// The binaries are Stowage's too.
	for i := range rep.Subjects {
		rep.Subjects[i].Owner = "stowage"
	}
	rep.Project, rep.Version = "stowage", "self-audit"
	if err := rep.Print(os.Stdout); err != nil {
		return err
	}
	p := sbom.Policy{FailOn: sbom.High, On: true, FixableOnly: true, Allow: allow}
	g := rep.Judge(p, now)
	for _, n := range g.Accepted() {
		fmt.Fprintln(os.Stderr, "accepted:", n)
	}
	for _, n := range g.Notes() {
		fmt.Fprintln(os.Stderr, "note:", n)
	}
	if err := g.Err(p); err != nil {
		return err
	}
	fmt.Printf("passes: no fixable finding at or above high (%d accepted by %s)\n", g.Allowed, allowFile)
	return nil
}

// docker runs the Docker CLI, stdin and stdout as given, its stderr passed through.
func docker(ctx context.Context, stdin *bytes.Reader, stdout *bytes.Buffer, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	if stdout != nil {
		cmd.Stdout = stdout
	} else {
		cmd.Stdout = os.Stderr
	}
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
