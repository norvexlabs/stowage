// Package graph computes the build graph of a release and each node's inputs digest.
//
//	images ──► os-bundle ──► disks
//	   │           │
//	   └──► chart  └──► installer ──► release
//
// A node's inputs digest covers everything that can change what the node produces. A node whose
// pushed artifact already carries the same digest is skipped (an image is re-tagged, not rebuilt).
// The project version enters a digest only where the artifact embeds it, so a version bump alone
// re-tags every image that does not read it.
package graph

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/norvexlabs/stowage/internal/backup"
	"github.com/norvexlabs/stowage/internal/chown"
	"github.com/norvexlabs/stowage/internal/digest"
	"github.com/norvexlabs/stowage/internal/dockerfile"
	"github.com/norvexlabs/stowage/internal/front"
	"github.com/norvexlabs/stowage/internal/helm"
	"github.com/norvexlabs/stowage/internal/kairos"
	"github.com/norvexlabs/stowage/internal/project"
	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/sbom"
	"github.com/norvexlabs/stowage/internal/spec"
)

// Platform is the platform every built image targets. It is one of an image's inputs, so an image
// built for another platform is never kept or re-tagged in its place.
const Platform = "linux/amd64"

// platform is what imageNode digests; tests change it to prove the platform is an input.
var platform = Platform

// VersionArg is the build argument through which an image may read the project version. An image
// whose Dockerfile declares it receives it, and the version becomes one of its inputs.
const VersionArg = "STOWAGE_PROJECT_VERSION"

// Kinds of node.
const (
	KindImage     = "image"
	KindChart     = "chart"
	KindOSBundle  = "os-bundle"
	KindDisks     = "disks"
	KindInstaller = "installer"
	KindRelease   = "release"
)

// FrontID is the node of the front Stowage builds for every release from its own recipe
// (front.Dockerfile), published as the image front.Key.
const FrontID = KindImage + "/" + front.Key

// BackupID is the node of the VM's backup runner Stowage builds from its own recipe
// (backup.Dockerfile) for a release whose VM runs backups (providers.RunsBackups), published as the image
// backup.Key.
const BackupID = KindImage + "/" + backup.Key

// AuditID is the node of the vulnerability scanner a signing release carries (sbom.Grype),
// published as the image sbom.Key.
const AuditID = KindImage + "/" + sbom.Key

// ChownID is the node of the helper that gives data volumes their uid, which Stowage builds from
// its own recipe (chown.Dockerfile) for a release whose installs give one (providers.OwnsVolumes),
// published as the image chown.Key.
const ChownID = KindImage + "/" + chown.Key

// Node is one artifact of a release.
type Node struct {
	ID     string   `json:"id"`
	Kind   string   `json:"kind"`
	Deps   []string `json:"deps,omitempty"`
	Inputs string   `json:"inputs"`
	// Pinned is set for an upstream image taken by reference: nothing is built.
	Pinned string `json:"pinned,omitempty"`
	// Converts is set for an upstream image, pinned by digest, published with OCI media types:
	// its digest is the conversion's, found again by its inputs as a built image's is.
	Converts string `json:"converts,omitempty"`
	// ReadsVersion is set for an image whose Dockerfile declares VersionArg.
	ReadsVersion bool `json:"reads_version,omitempty"`
	// Recipe is the Dockerfile of an image Stowage supplies (the front): what its inputs digest
	// hashes and what is built, with an empty context.
	Recipe []byte `json:"-"`
}

// Graph is a release's nodes in dependency order.
type Graph struct {
	ProjectVersion string `json:"project_version"`
	Revision       int    `json:"revision"`
	Channel        string `json:"channel"`
	Nodes          []Node `json:"nodes"`
}

// Node returns the node with the given id.
func (g *Graph) Node(id string) (Node, bool) {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

// Release identifies what is built: a project version at a package revision, for a channel, and
// what the installer carries beyond the tree.
type Release struct {
	Version  string
	Revision int
	Channel  string
	// Installer is the installer executable the release's installer image is made of: the one
	// this Stowage publishes.
	Installer []byte
	// Pull is the pull account baked into the installer ("" when the project has none).
	Pull string
	// Scanner is the vulnerability scanner a signing release carries, pinned by digest: sbom.Grype
	// when empty.
	Scanner string
}

// Build computes the graph of p for a release. The installer image is the installer executable
// with the release appended: the executable, the baked pull account and what the carried
// manifest records beyond the tree (changelog, scheme) are its inputs too; the release's outputs
// it also carries (image digests, number, date) are not, so ship never keeps an installer on
// its inputs digest. The revision and the channel enter every artifact that embeds them — the
// chart (published in the channel's own repository), the OS bundle and the installer (which
// follow the channel for updates) and the release manifest — and never an image.
// The release sequence a chart version carries is fixed when the release is first published and
// never changes for it, so it is not an input.
func Build(p *project.Project, r Release) (*Graph, error) {
	if r.Revision < 1 {
		return nil, fmt.Errorf("revision %d: revisions start at 1", r.Revision)
	}
	if _, ok := p.Spec.Channels[r.Channel]; !ok {
		return nil, fmt.Errorf("channel %q is not declared", r.Channel)
	}
	s := p.Spec
	projectVersion := r.Version
	rev := fmt.Sprint(r.Revision)
	g := &Graph{ProjectVersion: projectVersion, Revision: r.Revision, Channel: r.Channel}
	specJSON, err := json.Marshal(s.RuntimeView())
	if err != nil {
		return nil, err
	}
	lockField := "none" // the pinned stowage version, which fixes every runtime piece
	if p.Lock != nil {
		lockField = p.Lock.Stowage
	}

	// The public keys installs trust (signing.public_keys): every artifact that carries them —
	// chart values, OS bundle, installer — hashes their contents.
	var trusted []string
	if s.Signs() {
		c, err := providers.CosignKey(s.Signing)
		if err != nil {
			return nil, fmt.Errorf("signing: %w", err)
		}
		trusted = c.PublicKeys
	}
	installed := append([]string{s.Stack.Compose}, trusted...)
	// The services' nftables tables, which the installer carries.
	for _, name := range slices.Sorted(maps.Keys(s.Services)) {
		if f := s.Services[name].Nftables; f != "" {
			installed = append(installed, f)
		}
	}

	var imageIDs []string
	imageDigests := digest.New("image-set")
	for _, name := range s.ImageNames() {
		n, err := imageNode(p, name, projectVersion)
		if err != nil {
			return nil, err
		}
		g.Nodes = append(g.Nodes, n)
		imageIDs = append(imageIDs, n.ID)
		imageDigests.Field(n.ID, n.Inputs)
	}
	var stowageImages []Node
	if s.HasFront() {
		stowageImages = append(stowageImages, frontNode())
	}
	if providers.RunsBackups(s) {
		stowageImages = append(stowageImages, backupNode())
	}
	if providers.OwnsVolumes(s) {
		stowageImages = append(stowageImages, chownNode())
	}
	if s.Signs() {
		stowageImages = append(stowageImages, auditNode(cmp.Or(r.Scanner, sbom.Grype)))
	}
	for _, n := range stowageImages {
		g.Nodes = append(g.Nodes, n)
		imageIDs = append(imageIDs, n.ID)
		imageDigests.Field(n.ID, n.Inputs)
	}
	images := imageDigests.Sum()
	all := digest.New("release")
	all.Field("version", projectVersion)
	all.Field("revision", rev)
	all.Field("channel", r.Channel)
	all.Field("images", images)

	if k := s.Shapes.Kubernetes; k != nil {
		// The packaged chart carries values derived from the spec and the Stowage the lock pins.
		b := digest.New(KindChart)
		b.Field("version", projectVersion)
		b.Field("revision", rev)
		b.Field("channel", r.Channel)
		b.Field("spec", string(specJSON))
		b.Field("lock", lockField)
		b.Field("images", images)
		// The chart's files as they are packaged: what its .helmignore excludes is not an input.
		dir, err := spec.ProjectPath(p.Root, k.Chart)
		if err != nil {
			return nil, fmt.Errorf("shapes.kubernetes.chart: %w", err)
		}
		c, err := helm.Load(dir)
		if err != nil {
			return nil, fmt.Errorf("shapes.kubernetes.chart: %w", err)
		}
		for _, f := range c.Files {
			sum := sha256.Sum256(f.Data)
			b.Field("chart-file", f.Name+"\x00"+hex.EncodeToString(sum[:]))
		}
		b.Field("chart-files", fmt.Sprint(len(c.Files)))
		if err := b.Tree(p.Root, trusted, nil); err != nil {
			return nil, fmt.Errorf("chart: %w", err)
		}
		n := Node{ID: KindChart, Kind: KindChart, Deps: imageIDs, Inputs: b.Sum()}
		g.Nodes = append(g.Nodes, n)
		all.Field(n.ID, n.Inputs)
	}

	// The installer and the OS bundle's <short>-stow carry the release manifest: what it records
	// beyond the tree's files.
	text, _, err := p.Changelog.Section(projectVersion)
	if err != nil {
		return nil, fmt.Errorf("changelog: %w", err)
	}
	scheme, err := json.Marshal(release.SchemeOf(s.Version.Scheme))
	if err != nil {
		return nil, err
	}
	executable := fmt.Sprintf("sha256:%x", sha256.Sum256(r.Installer))
	pull := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(r.Pull)))

	installerDeps := append([]string(nil), imageIDs...)
	installer := digest.New(KindInstaller)
	if k := s.Shapes.Kairos; k != nil {
		// The bundle embeds no channel, so a promotion re-tags it: the OS version is the
		// release's version and revision, and the images are preloaded under channel-free tags.
		b := digest.New(KindOSBundle)
		b.Field("version", projectVersion)
		b.Field("revision", rev)
		b.Field("name", s.Name)
		b.Field("base", k.Base)
		b.Field("kairos_init", k.KairosInit)
		b.Field("recipe", bundleRecipe())
		b.Field("images", images) // its <short>-stow names them all, the scanner's included
		// Its <short>-stow: the installer executable with the release as the bundle carries it.
		bundleView, err := json.Marshal(s.BundleView())
		if err != nil {
			return nil, err
		}
		b.Field("spec", string(bundleView))
		b.Field("lock", lockField)
		b.Field("changelog", text)
		b.Field("scheme", string(scheme))
		b.Field("executable", executable)
		b.Field("pull", pull)
		if err := b.Tree(p.Root, installed, nil); err != nil {
			return nil, fmt.Errorf("os bundle: %w", err)
		}
		osNode := Node{ID: KindOSBundle, Kind: KindOSBundle, Deps: imageIDs, Inputs: b.Sum()}
		d := digest.New(KindDisks)
		d.Field("os", osNode.Inputs)
		// In the order they are made, not the spec's: reordering the list changes no disk.
		d.Field("formats", strings.Join(k.Formats(), ","))
		d.Field("recipe", diskRecipe())
		d.Field("layer", diskLayer)
		disks := Node{ID: KindDisks, Kind: KindDisks, Deps: []string{osNode.ID}, Inputs: d.Sum()}
		g.Nodes = append(g.Nodes, osNode, disks)
		all.Field(osNode.ID, osNode.Inputs)
		all.Field(disks.ID, disks.Inputs)
		installerDeps = append(installerDeps, osNode.ID)
		installer.Field("os", osNode.Inputs)
	}

	installer.Field("version", projectVersion)
	installer.Field("revision", rev)
	installer.Field("channel", r.Channel)
	installer.Field("spec", string(specJSON))
	installer.Field("lock", lockField)
	installer.Field("images", images)
	installer.Field("changelog", text)
	installer.Field("scheme", string(scheme))
	installer.Field("executable", executable)
	installer.Field("pull", pull)
	if err := installer.Tree(p.Root, installed, nil); err != nil {
		return nil, fmt.Errorf("installer: %w", err)
	}
	inst := Node{ID: KindInstaller, Kind: KindInstaller, Deps: installerDeps, Inputs: installer.Sum()}
	g.Nodes = append(g.Nodes, inst)
	all.Field(inst.ID, inst.Inputs)

	all.Field("changelog", text)
	all.Field("lock", lockField)
	var deps []string
	for _, n := range g.Nodes {
		deps = append(deps, n.ID)
	}
	sort.Strings(deps)
	g.Nodes = append(g.Nodes, Node{ID: KindRelease, Kind: KindRelease, Deps: deps, Inputs: all.Sum()})
	return g, nil
}

// frontNode is the front: its inputs are the recipe and the platform; it reads no project file.
func frontNode() Node {
	recipe := front.Dockerfile
	b := digest.New(KindImage)
	b.Field("name", front.Key)
	b.Field("platform", platform)
	b.Field("recipe", string(recipe))
	return Node{ID: FrontID, Kind: KindImage, Inputs: b.Sum(), Recipe: recipe}
}

// backupNode is the VM's backup runner: like the front, its inputs are the recipe and the
// platform.
func backupNode() Node {
	recipe := backup.Dockerfile
	b := digest.New(KindImage)
	b.Field("name", backup.Key)
	b.Field("platform", platform)
	b.Field("recipe", string(recipe))
	return Node{ID: BackupID, Kind: KindImage, Inputs: b.Sum(), Recipe: recipe}
}

// auditNode is the scanner (grype), pinned at ref: a signing release publishes it converted to
// OCI media types (a registry need not take Docker's), so an install audits the SBOMs it carries
// with its own release's scanner, wherever it pulls from.
func auditNode(ref string) Node {
	b := digest.New(KindImage)
	b.Field("name", sbom.Key)
	b.Field("ref", ref)
	b.Field("media-types", "oci")
	return Node{ID: AuditID, Kind: KindImage, Inputs: b.Sum(), Converts: ref}
}

// chownNode is the helper that gives data volumes their uid: like the front, its inputs are the
// recipe and the platform.
func chownNode() Node {
	recipe := chown.Dockerfile
	b := digest.New(KindImage)
	b.Field("name", chown.Key)
	b.Field("platform", platform)
	b.Field("recipe", string(recipe))
	return Node{ID: ChownID, Kind: KindImage, Inputs: b.Sum(), Recipe: recipe}
}

func imageNode(p *project.Project, name, projectVersion string) (Node, error) {
	img := p.Spec.Images[name]
	id := KindImage + "/" + name
	b := digest.New(KindImage)
	b.Field("name", name)
	if !img.Built() {
		b.Field("ref", img.Ref)
		return Node{ID: id, Kind: KindImage, Inputs: b.Sum(), Pinned: img.Ref}, nil
	}
	dfPath, err := spec.ProjectPath(p.Root, img.Dockerfile)
	if err != nil {
		return Node{}, fmt.Errorf("image %s dockerfile: %w", name, err)
	}
	ctxPath, err := spec.ProjectPath(p.Root, img.Context)
	if err != nil {
		return Node{}, fmt.Errorf("image %s context: %w", name, err)
	}
	df, err := os.ReadFile(dfPath)
	if err != nil {
		return Node{}, fmt.Errorf("image %s: %w", name, err)
	}
	n := Node{ID: id, Kind: KindImage}
	b.Field("platform", platform)
	b.Field("dockerfile", img.Dockerfile)
	b.Field("dockerfile-content", string(df))
	b.Field("context", img.Context)
	b.Field("target", img.Target)
	argKeys := make([]string, 0, len(img.Args))
	for k := range img.Args {
		argKeys = append(argKeys, k)
	}
	sort.Strings(argKeys)
	for _, k := range argKeys {
		b.Field("arg", k+"="+img.Args[k])
	}
	// Each address in its order: a step's resolver returns them so.
	for _, h := range slices.Sorted(maps.Keys(img.AddHosts)) {
		for _, a := range img.AddHosts[h] {
			b.Field("add-host", h+"="+a)
		}
	}
	// Entered only when set (default is BuildKit's own network): a build naming neither digests as
	// one that has no such keys.
	if img.Network != "" && img.Network != "default" {
		b.Field("network", img.Network)
	}
	parsed, err := dockerfile.Parse(df)
	if err != nil {
		return Node{}, fmt.Errorf("image %s: %s: %w", name, img.Dockerfile, err)
	}
	reads, err := parsed.MayDeclareArg(VersionArg)
	if err != nil {
		return Node{}, fmt.Errorf("image %s: %s: %w", name, img.Dockerfile, err)
	}
	if reads {
		n.ReadsVersion = true
		b.Field("version", projectVersion)
	}
	ig, err := digest.DockerIgnore(ctxPath, dfPath)
	if err != nil {
		return Node{}, fmt.Errorf("image %s: %w", name, err)
	}
	inputs := img.Inputs
	if len(inputs) == 0 {
		inputs = []string{img.Context}
	}
	if err := b.Tree(p.Root, inputs, ig); err != nil {
		return Node{}, fmt.Errorf("image %s: %w", name, err)
	}
	n.Inputs = b.Sum()
	return n, nil
}

// bundleRecipe and diskRecipe are the Kairos recipes' digests; tests change them to prove they
// are inputs.
var bundleRecipe, diskRecipe = kairos.BundleRecipe, kairos.DiskRecipe

// diskLayer is how a disk's file is pushed, as its layer: compressed with zstd, the media type
// suffix registry.ZstdFileBlob gives it. Another way makes every disk again; tests change it.
var diskLayer = registry.ZstdSuffix

// OSKey is the key the OS bundle is kept on: its node's inputs and the images it preloads, by
// digest. An image is kept on its inputs, so one rebuilt with the same inputs is another image;
// keying the bundle on the digests keeps it from preloading the one it replaced.
func OSKey(inputs string, images map[string]string) string {
	d := digest.New(KindOSBundle)
	d.Field("inputs", inputs)
	for _, k := range slices.Sorted(maps.Keys(images)) {
		d.Field("image", k+"\x00"+images[k])
	}
	return d.Sum()
}

// DiskKey is the key a disk is kept on: the bundle it is made of, by digest, its format, what it
// is made with, and how it is pushed.
func DiskKey(bundle, format string) string {
	d := digest.New(KindDisks)
	d.Field("os", bundle)
	d.Field("format", format)
	d.Field("recipe", diskRecipe())
	d.Field("layer", diskLayer)
	return d.Sum()
}
