package sbom

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/norvexlabs/stowage/internal/release"
)

// Target is an artifact of a release whose SBOM an audit reads: an image by its key, the
// installer, the OS bundle.
type Target struct {
	Name   string
	Digest name.Digest
	// Optional is an artifact a mirror may leave out: the OS bundle and the disks, which an
	// export carries only for the shapes and the disk formats it names.
	Optional bool
}

// Targets are the artifacts of a release (a) that carry an SBOM, under the registry prefix the
// reader pulls from (a mirror's included): every image by key, then the installer, the OS bundle
// and each of its disks (disk-<format>).
func Targets(prefix, project string, a release.Artifacts) ([]Target, error) {
	var out []Target
	add := func(n, repo, d string) error {
		r, err := name.NewRepository(prefix + "/" + project + "-" + repo)
		if err != nil {
			return err
		}
		out = append(out, Target{Name: n, Digest: r.Digest(d), Optional: repo == "os" || repo == "disk"})
		return nil
	}
	keys := make([]string, 0, len(a.Images))
	for k := range a.Images {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if err := add(k, k, a.Images[k]); err != nil {
			return nil, err
		}
	}
	if a.Installer != "" {
		if err := add("installer", "installer", a.Installer); err != nil {
			return nil, err
		}
	}
	if a.OS != "" {
		if err := add("os", "os", a.OS); err != nil {
			return nil, err
		}
	}
	// Each disk carries its bundle's SBOM.
	for _, f := range slices.Sorted(maps.Keys(a.Disks)) {
		if err := add("disk-"+f, "disk", a.Disks[f]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Attested reads a target's SBOM attested by a trusted key.
type Attested func(ctx context.Context, d name.Digest) ([]byte, error)

// Audit reads each target's attested SBOM and scans it: a target without one, or whose scan
// failed, is in the report with its error, never left out. The database is made ready once, and
// only when a target has an SBOM to scan.
func Audit(ctx context.Context, read Attested, s Scanner, targets []Target, logf func(string, ...any)) (Report, error) {
	var r Report
	docs := make([][]byte, len(targets))
	ready := false
	for i, t := range targets {
		sub := Subject{Name: t.Name, Digest: t.Digest.DigestStr(), Findings: []Finding{}}
		doc, err := read(ctx, t.Digest)
		if err == nil {
			err = Check(doc)
		}
		if err != nil {
			sub.Error = err.Error()
		} else {
			docs[i], ready = doc, true
		}
		r.Subjects = append(r.Subjects, sub)
	}
	if !ready {
		return r, nil
	}
	// A database that could not be made ready leaves each SBOM unscanned, named, beside the
	// targets that have none.
	if err := s.Import(ctx); err != nil {
		for i := range targets {
			if docs[i] != nil {
				r.Subjects[i].Error = fmt.Sprintf("the scan: %v", err)
			}
		}
		return r, nil
	}
	// A document is scanned once: each disk carries its bundle's.
	type scanned struct {
		f     []Finding
		built string
		err   error
	}
	seen := map[[32]byte]scanned{}
	for i, t := range targets {
		if docs[i] == nil {
			continue
		}
		key := sha256.Sum256(docs[i])
		sc, ok := seen[key]
		if !ok {
			if logf != nil {
				logf("scanning %s (%s)", t.Name, t.Digest.DigestStr())
			}
			sc.f, sc.built, sc.err = s.Scan(ctx, docs[i])
			seen[key] = sc
		}
		f, built, err := sc.f, sc.built, sc.err
		if err != nil {
			r.Subjects[i].Error = fmt.Sprintf("the scan: %v", err)
		}
		r.Subjects[i].Findings = append(r.Subjects[i].Findings, f...)
		r.DBBuilt = max(r.DBBuilt, built)
	}
	return r, nil
}
