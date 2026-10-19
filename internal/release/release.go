// Package release is the release manifest: the one document every consumer reads to learn what a
// release is, where its artifacts are and whether it may be installed. It is published as an OCI
// artifact of type MediaType under the release's tag and under its channel's pointer tag.
package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/version"
)

var sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// diskFormat is a disk format name: it ends a tag (DiskTag) and a media type.
var diskFormat = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,15}$`)

// MediaType is the manifest's artifact type.
const MediaType = "application/vnd.stowage.release.v1+json"

// SchemaVersion is the only manifest format this build reads and writes.
const SchemaVersion = 2

// Manifest describes one published release.
type Manifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Name          string `json:"name"`
	// ShortName names the release's installer, <short_name>-stow.
	ShortName      string `json:"short_name"`
	ProjectVersion string `json:"project_version"`
	Revision       int    `json:"revision"`
	// ReleaseSeq numbers the channel's publications in order; a number is never handed out twice,
	// and a mutable tag rebuilt with new content gets a new one.
	ReleaseSeq int    `json:"release_seq"`
	Channel    string `json:"channel"`
	// Tag is the tag every artifact of the release carries.
	Tag  string    `json:"tag"`
	Date time.Time `json:"date"`
	// Inputs is the release node's inputs digest: equal inputs, equal release.
	Inputs         string    `json:"inputs"`
	Scheme         Scheme    `json:"scheme"`
	StowageVersion string    `json:"stowage_version"`
	Artifacts      Artifacts `json:"artifacts"`
	Changelog      Changelog `json:"changelog"`
	Yanked         *Yank     `json:"yanked"`
}

// Scheme is the version scheme the project declared, so consumers order releases without the
// project's spec.
type Scheme struct {
	Provider string         `json:"provider"`
	Config   map[string]any `json:"config,omitempty"`
}

// SchemeOf records a spec provider.
func SchemeOf(p spec.Provider) Scheme { return Scheme{Provider: p.Name, Config: p.Config} }

// Artifacts holds the digest of every published artifact. Images are keyed by the spec's images
// key; each lives at <prefix>/<name>-<key>.
type Artifacts struct {
	Images map[string]string `json:"images"`
	// Configs is each image's linux/amd64 config digest: the ID the classic Docker image store
	// gives an image loaded from an archive, which keeps no registry digest. An offline install
	// checks a loaded image against it (or against Images, the ID on the containerd store).
	Configs map[string]string `json:"configs"`
	// Manifests is each image's linux/amd64 manifest digest (the image digest itself when the
	// image is not an index): the ID the containerd store gives a single-platform archive.
	Manifests map[string]string `json:"manifests"`
	// Installer is the installer image's digest, at <prefix>/<name>-installer. The installer
	// carries this manifest without this field, which names the installer itself.
	Installer string `json:"installer,omitempty"`
	// Chart is the release's Helm chart, for a project with the kubernetes shape.
	Chart *Chart `json:"chart,omitempty"`
	// OS is the Kairos OS bundle's digest, at <prefix>/<name>-os, for a project with the kairos
	// shape; Disks is each of its disk images' digest by format, at <prefix>/<name>-disk under
	// DiskTag.
	OS    string            `json:"os,omitempty"`
	Disks map[string]string `json:"disks,omitempty"`
}

// DiskArtifactType is a disk image's artifact type; its one layer is the disk file compressed with
// zstd, of media type DiskMediaType(format) and registry.ZstdSuffix, titled with the file's name
// and ".zst", annotated with the file's own digest (registry.ZstdFileBlob); a release before 0.5.0
// has the file as it is, of media type DiskMediaType(format), titled with its name.
const DiskArtifactType = "application/vnd.stowage.disk.v1"

// DiskMediaType is the media type of a disk file of format.
func DiskMediaType(format string) types.MediaType {
	return types.MediaType("application/vnd.stowage.disk." + format)
}

// DiskTag is the tag of a release's disk image of format.
func DiskTag(tag, format string) string { return tag + "-" + format }

// Chart locates a release's Helm chart: version ChartRepository's tag Version holds Digest.
type Chart struct {
	// Repository is under the registry prefix: charts/<name>-<channel>.
	Repository string `json:"repository"`
	Version    string `json:"version"`
	Digest     string `json:"digest"`
}

// ChartRepository is the chart repository of a project's channel, under its registry prefix.
func ChartRepository(name, channel string) string { return "charts/" + name + "-" + channel }

// Changelog is what operators read before upgrading.
type Changelog struct {
	Project  string `json:"project"`
	Platform string `json:"platform,omitempty"`
}

// Yank marks a release consumers must not upgrade to.
type Yank struct {
	Reason string    `json:"reason"`
	Date   time.Time `json:"date"`
}

// Encode serialises m after checking it.
func (m *Manifest) Encode() ([]byte, error) {
	if err := m.check(); err != nil {
		return nil, err
	}
	return json.MarshalIndent(m, "", "  ")
}

// Decode parses a manifest strictly: an unknown field or another schema version is refused, so a
// consumer never acts on a document it only half understands.
func Decode(raw []byte) (*Manifest, error) { return decode(raw, (*Manifest).check) }

// EncodeBundled serialises m as the OS bundle carries it, after checking it is one: a release
// without its publication — no channel, tag, number, date, yank or artifact beyond the images —
// since the bundle is built before the release is published and serves every channel it is
// promoted to.
func (m *Manifest) EncodeBundled() ([]byte, error) {
	if err := m.checkBundled(); err != nil {
		return nil, err
	}
	return json.MarshalIndent(m, "", "  ")
}

// DecodeBundled parses a manifest an OS bundle carries, as strictly as Decode.
func DecodeBundled(raw []byte) (*Manifest, error) { return decode(raw, (*Manifest).checkBundled) }

// Bundled reports whether m is the release an OS bundle carries rather than a published one.
func (m *Manifest) Bundled() bool { return m.Channel == "" && m.Tag == "" }

func decode(raw []byte, check func(*Manifest) error) (*Manifest, error) {
	var probe struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("release manifest: %w", err)
	}
	if probe.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("release manifest schemaVersion %d is not supported (this stowage reads %d); upgrade stowage", probe.SchemaVersion, SchemaVersion)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("release manifest: %w", err)
	}
	if err := dec.Decode(&json.RawMessage{}); err != io.EOF {
		return nil, errors.New("release manifest: trailing data")
	}
	if err := check(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) checkBundled() error {
	a := m.Artifacts
	switch {
	case !m.Bundled(), m.ReleaseSeq != 0, !m.Date.IsZero(), m.Yanked != nil:
		return errors.New("release manifest: an OS bundle's carries no channel, tag, release_seq, date or yank")
	case a.Installer != "", a.Chart != nil, a.OS != "", len(a.Disks) > 0:
		return errors.New("release manifest: an OS bundle's records no installer, chart, os or disks")
	}
	// Everything else is checked as a published manifest's.
	c := *m
	c.Channel, c.Tag, c.ReleaseSeq = "-", "-", 1
	return c.check()
}

// PreloadTag is the tag an image is preloaded under in the OS bundle: its digest, so the bundle
// serves every channel and an install finds the release's images by digest alone.
func PreloadTag(digest string) string { return strings.Replace(digest, ":", "-", 1) }

func (m *Manifest) check() error {
	switch {
	case m.SchemaVersion != SchemaVersion:
		return fmt.Errorf("release manifest: schemaVersion %d", m.SchemaVersion)
	case m.Name == "", m.ShortName == "", m.ProjectVersion == "", m.Channel == "", m.Tag == "", m.Inputs == "":
		return errors.New("release manifest: name, short_name, project_version, channel, tag and inputs are required")
	case !spec.NamePattern.MatchString(m.Name), !spec.NamePattern.MatchString(m.ShortName):
		return fmt.Errorf("release manifest: name %q or short_name %q is not a project name", m.Name, m.ShortName)
	case m.Revision < 1:
		return fmt.Errorf("release manifest: revision %d", m.Revision)
	case m.ReleaseSeq < 1:
		return fmt.Errorf("release manifest: release_seq %d", m.ReleaseSeq)
	case m.Scheme.Provider == "":
		return errors.New("release manifest: scheme.provider is required")
	}
	for k := range m.Artifacts.Images {
		if m.Artifacts.Configs[k] == "" || m.Artifacts.Manifests[k] == "" {
			return fmt.Errorf("release manifest: image %s has no linux/amd64 manifest or config digest", k)
		}
	}
	if i := m.Artifacts.Installer; i != "" && !sha256Digest.MatchString(i) {
		return fmt.Errorf("release manifest: installer %q is not a sha256 digest", i)
	}
	if o := m.Artifacts.OS; o != "" && !sha256Digest.MatchString(o) {
		return fmt.Errorf("release manifest: os %q is not a sha256 digest", o)
	}
	if len(m.Artifacts.Disks) > 0 && m.Artifacts.OS == "" {
		return errors.New("release manifest: disks without an os bundle")
	}
	// A format this build does not know is kept: a later stowage may publish more, and a consumer
	// uses the formats it knows.
	for f, d := range m.Artifacts.Disks {
		if !diskFormat.MatchString(f) {
			return fmt.Errorf("release manifest: disk format %q is not lowercase letters, digits, '.', '_' or '-'", f)
		}
		if !sha256Digest.MatchString(d) {
			return fmt.Errorf("release manifest: disk %s %q is not a sha256 digest", f, d)
		}
	}
	if c := m.Artifacts.Chart; c != nil {
		switch {
		case c.Repository != ChartRepository(m.Name, m.Channel):
			return fmt.Errorf("release manifest: chart repository %q is not %s", c.Repository, ChartRepository(m.Name, m.Channel))
		case c.Version != version.ChartVersion(m.ReleaseSeq):
			return fmt.Errorf("release manifest: chart version %q is not %s, from release_seq", c.Version, version.ChartVersion(m.ReleaseSeq))
		case !sha256Digest.MatchString(c.Digest):
			return fmt.Errorf("release manifest: chart digest %q is not a sha256 digest", c.Digest)
		}
	}
	if len(m.Artifacts.Configs) != len(m.Artifacts.Images) || len(m.Artifacts.Manifests) != len(m.Artifacts.Images) {
		return errors.New("release manifest: a manifest or config digest names no image")
	}
	return nil
}
