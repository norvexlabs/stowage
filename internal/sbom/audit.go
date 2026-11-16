package sbom

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"regexp"
	"slices"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/norvexlabs/stowage/internal/registry"
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
	// Disk is a disk of the OS bundle, which carries the bundle's SBOM.
	Disk bool
	// Platform is an image's platform (os/arch) in its artifact's index (the installer's), whose
	// own SBOM it carries; "" for the artifact itself.
	Platform string
	// Err, set, is why the target could not be named (OtherPlatforms): it is reported not audited
	// with it, never read.
	Err string
}

// OtherPlatforms stands for the installer index's images but the target's when the index could
// not be read: one target not audited in their place.
const OtherPlatforms = "other platforms"

// platformName is a platform as an index names it, os/arch[/variant]: it names an exported file.
var platformName = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+(/[a-z0-9]+)?$`)

// Entry is the image of an index for one platform (os/arch).
type Entry struct {
	Platform string
	Digest   name.Digest
}

// Entries lists the images of the index d names; none when d names an image.
type Entries func(ctx context.Context, d name.Digest) ([]Entry, error)

// WithEntries is targets with, after the installer, each image of its index but the one for
// Platform, whose SBOM the index carries: a target of the installer's name for its platform. An
// installer that is one image (made before indexes) adds none.
func WithEntries(ctx context.Context, targets []Target, entries Entries) []Target {
	var out []Target
	for _, t := range targets {
		out = append(out, t)
		if t.Name != "installer" || t.Platform != "" {
			continue
		}
		es, err := entries(ctx, t.Digest)
		if err == nil {
			for _, e := range es {
				if !platformName.MatchString(e.Platform) {
					err = fmt.Errorf("%q is not a platform (os/arch)", e.Platform)
					break
				}
			}
		}
		// An index that cannot be read leaves its images not audited, in one target, beside the
		// rest of the release: never the whole audit stopped.
		if err != nil {
			out = append(out, Target{Name: t.Name, Digest: t.Digest, Optional: t.Optional, Platform: OtherPlatforms,
				Err: fmt.Sprintf("the installer's images (one per platform): %v", err)})
			continue
		}
		for _, e := range es {
			if e.Platform != Platform {
				out = append(out, Target{Name: t.Name, Digest: e.Digest, Optional: t.Optional, Platform: e.Platform})
			}
		}
	}
	return out
}

// IndexReader reads an index's images (registry.Client).
type IndexReader interface {
	Platforms(ctx context.Context, ref name.Reference) ([]registry.PlatformEntry, bool, error)
}

// RegistryEntries are the Entries r reads.
func RegistryEntries(r IndexReader) Entries {
	return func(ctx context.Context, d name.Digest) ([]Entry, error) {
		es, _, err := r.Platforms(ctx, d)
		if err != nil {
			return nil, err
		}
		var out []Entry
		for _, e := range es {
			out = append(out, Entry{Platform: e.Platform.String(), Digest: d.Context().Digest(e.Digest.String())})
		}
		return out, nil
	}
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
		out = append(out, Target{Name: n, Digest: r.Digest(d), Optional: repo == "os" || repo == "disk", Disk: repo == "disk"})
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
		sub := Subject{Name: t.Name, Platform: t.Platform, Owner: Owner(t.Name), Digest: t.Digest.DigestStr(), Findings: []Finding{}}
		if t.Err != "" {
			sub.Error = t.Err
			r.Subjects = append(r.Subjects, sub)
			continue
		}
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
	// The cache is the scanner's alone from the database's import to the last scan.
	unlock, err := lockCache(ctx, s.Cache, logf)
	if err != nil {
		return r, err
	}
	defer unlock()
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
				logf("scanning %s (%s)", label(t.Name, t.Platform), t.Digest.DigestStr())
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
