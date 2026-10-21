// Package spec parses stowage.yaml, the one file a project writes to declare what Stowage packages.
package spec

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/backup"
	"github.com/norvexlabs/stowage/internal/front"
)

// APIVersion is the only spec format this build reads.
const APIVersion = "stowage/v1"

// Spec mirrors stowage.yaml. Unknown keys are refused, so a typo is an error rather than a silent
// default.
type Spec struct {
	APIVersion string `yaml:"apiVersion"`
	Name       string `yaml:"name"`
	// ShortName names the installer, <ShortName>-stow; empty is Name.
	ShortName    string             `yaml:"short_name"`
	Stowage      StowageReq         `yaml:"stowage"`
	Version      VersionSpec        `yaml:"version"`
	Changelog    Provider           `yaml:"changelog"`
	Channels     map[string]Channel `yaml:"channels"`
	Registry     Registry           `yaml:"registry"`
	Signing      *Provider          `yaml:"signing"`
	Images       map[string]Image   `yaml:"images"`
	Stack        Stack              `yaml:"stack"`
	Front        Front              `yaml:"front"`
	L4           L4                 `yaml:"l4"`
	Health       Provider           `yaml:"health"`
	Database     *Database          `yaml:"database"`
	Data         Data               `yaml:"data"`
	Secrets      []Secret           `yaml:"secrets"`
	SafeDefaults map[string]string  `yaml:"safe_defaults"`
	Inputs       []Input            `yaml:"inputs"`
	Hooks        Hooks              `yaml:"hooks"`
	Backup       *Provider          `yaml:"backup"`
	Shapes       Shapes             `yaml:"shapes"`
	// Extensions are the apps, published on the same registry prefix, whose endpoint an input
	// of this project takes.
	Extensions []Extension `yaml:"extensions"`
	// Endpoint names the route that is this project's endpoint when another project uses it as
	// an extension: its installer prints the route's URL after an install.
	Endpoint string `yaml:"endpoint"`
}

// Extension is an app another project's install can point this one at: its installer is
// <prefix>/<Name>-installer, Input the optional input holding its endpoint (empty: off), KeyInput
// an optional secret input holding its credential.
type Extension struct {
	Name     string `yaml:"name"`
	Input    string `yaml:"input"`
	Prompt   string `yaml:"prompt"`
	KeyInput string `yaml:"key_input"`
}

type StowageReq struct {
	Requires string `yaml:"requires"`
}

type VersionSpec struct {
	Source Provider `yaml:"source"`
	Scheme Provider `yaml:"scheme"`
}

type Channel struct {
	Tag     string `yaml:"tag"`
	Mutable bool   `yaml:"mutable"`
	Latest  bool   `yaml:"latest"`
}

type Registry struct {
	Prefix string   `yaml:"prefix"`
	Pull   Provider `yaml:"pull"`
	Push   Provider `yaml:"push"`
	// Delete is how the publisher deletes a manifest (an older disk set, a pruned release's):
	// oci, the distribution API (the default), or github, GitHub's packages API.
	Delete Provider `yaml:"delete"`
}

// Image is either built (Context + Dockerfile) or a pinned upstream reference (Ref).
type Image struct {
	Context    string            `yaml:"context"`
	Dockerfile string            `yaml:"dockerfile"`
	Target     string            `yaml:"target"`
	Args       map[string]string `yaml:"args"`
	Inputs     []string          `yaml:"inputs"`
	Ref        string            `yaml:"ref"`
}

// ReservedImageKeys are the image keys whose repository <prefix>/<name>-<key> Stowage publishes
// its own artifacts to (release manifests, sequence records, OS bundle, disks, installer, front,
// the VM's backup runner).
var ReservedImageKeys = []string{"release", "release-seq", "os", "disk", "installer", front.Key, backup.Key}

// Built reports whether Stowage builds this image (as opposed to pulling a pinned reference).
func (i Image) Built() bool { return i.Ref == "" }

type Stack struct {
	Compose string `yaml:"compose"`
}

type Front struct {
	TLSModes      []string       `yaml:"tls_modes"`
	Routes        []Route        `yaml:"routes"`
	Portal        string         `yaml:"portal"`
	LoopbackPorts map[string]int `yaml:"loopback_ports"`
	// LoopbackEnv relaxes safe defaults in loopback mode (plain HTTP on this machine), e.g. a
	// secure-cookie flag; every key is a safe_defaults key.
	LoopbackEnv map[string]string `yaml:"loopback_env"`
	// PublishCA copies the CA the front's certificates chain to into a project volume.
	PublishCA *PublishCA `yaml:"publish_ca"`
	// HTTPPort and HTTPSPort are the host ports a TLS front publishes unless the install gives
	// others; 0 is 80 and 443.
	HTTPPort  int `yaml:"http_port"`
	HTTPSPort int `yaml:"https_port"`
}

// L4 is the stack's layer-4 routes: host ports published to a service as they are, TCP or UDP,
// beside the front and never through it.
type L4 struct {
	Routes []L4Route `yaml:"routes"`
}

// L4Route publishes the host port Port, over Protocol, to Upstream (service:port).
// AddressInput names the input holding the host name or address clients dial, which the stack
// reads as the route's endpoint (L4Var).
type L4Route struct {
	Name         string `yaml:"name"`
	Protocol     string `yaml:"protocol"`
	Port         int    `yaml:"port"`
	Upstream     string `yaml:"upstream"`
	AddressInput string `yaml:"address_input"`
}

// L4Var is the variable carrying an L4 route's endpoint, <address>:<port>.
func L4Var(route string) string {
	return "STOWAGE_L4_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(route))
}

// PublishCA names the project volume the front copies its CA into, as ca.crt: the local CA's
// root in the internal mode, the one given with --ca in byo; none in the other modes.
type PublishCA struct {
	Volume string `yaml:"volume"`
}

// CheckEnvValue refuses a value an install's .env cannot hold literally: it is written
// single-quoted, which compose reads without expansion, so a quote, a line break, a NUL or a
// trailing backslash (read as escaping the closing quote) would change it.
func CheckEnvValue(v string) error {
	if len(v) > MaxEnvValue {
		return fmt.Errorf("a value is at most %d bytes", MaxEnvValue)
	}
	if strings.ContainsAny(v, "'\n\r\x00") {
		return errors.New("a value cannot hold a single quote or a line break")
	}
	if strings.HasSuffix(v, "\\") {
		return errors.New("a value cannot end with a backslash")
	}
	return nil
}

// MaxEnvValue bounds a recorded value: a process's environment takes no longer one (Linux's
// MAX_ARG_STRLEN is 128 KiB for the whole KEY=value string).
const MaxEnvValue = 64 << 10

// URLVar is the variable carrying a route's public URL: https://<domain> behind TLS,
// http://<address>:<port> in loopback mode.
func URLVar(route string) string {
	return "STOWAGE_URL_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(route))
}

type Route struct {
	Name         string   `yaml:"name"`
	DomainInput  string   `yaml:"domain_input"`
	Upstream     string   `yaml:"upstream"`
	StripHeaders []string `yaml:"strip_headers"`
	// ForwardAuth asks an authentication service before proxying; off while its address is empty.
	ForwardAuth *ForwardAuth `yaml:"forward_auth"`
	// AllowInput names the input holding the source addresses (CIDRs) the route serves; the
	// front refuses any other, and everyone while it is empty.
	AllowInput string `yaml:"allow_input"`
}

// ForwardAuth is a route's authentication service: the inputs holding its address and the URI it
// is asked at, and the headers of its answer passed on to the upstream.
type ForwardAuth struct {
	AddressInput string   `yaml:"address_input"`
	URIInput     string   `yaml:"uri_input"`
	CopyHeaders  []string `yaml:"copy_headers"`
}

// DB is the project's database, the zero Database when it declares none (HasDatabase).
func (s *Spec) DB() Database {
	if s.Database == nil {
		return Database{}
	}
	return *s.Database
}

// Input is the input named key, and whether there is one.
func (s *Spec) Input(key string) (Input, bool) {
	if i := slices.IndexFunc(s.Inputs, func(i Input) bool { return i.Key == key }); i >= 0 {
		return s.Inputs[i], true
	}
	return Input{}, false
}

// RouteNames are the names of the front's routes, in their order.
func (f Front) RouteNames() []string {
	var n []string
	for _, r := range f.Routes {
		n = append(n, r.Name)
	}
	return n
}

// HasDatabase reports whether the project declares a database.
func (s *Spec) HasDatabase() bool { return s.Database != nil }

type Database struct {
	Engine       string   `yaml:"engine"`
	Service      string   `yaml:"service"`
	Name         string   `yaml:"name"`
	Roles        Roles    `yaml:"roles"`
	SchemaCompat Provider `yaml:"schema_compat"`
}

type Roles struct {
	Migrate string `yaml:"migrate"`
	App     string `yaml:"app"`
}

type Data struct {
	Volumes map[string]Volume `yaml:"volumes"`
	Reset   []string          `yaml:"reset"`
}

type Volume struct {
	UID         int  `yaml:"uid"`
	KeepOnReset bool `yaml:"keep_on_reset"`
}

type Secret struct {
	Key string `yaml:"key"`
	Gen string `yaml:"gen"`
	// Rotation lets `secrets rotate` replace the value in three passes, through KEY_NEXT and
	// KEY_PREVIOUS.
	Rotation bool `yaml:"rotation"`
}

type Input struct {
	Key      string   `yaml:"key"`
	Prompt   string   `yaml:"prompt"`
	Default  string   `yaml:"default"`
	Required bool     `yaml:"required"`
	Optional bool     `yaml:"optional"`
	Secret   bool     `yaml:"secret"`
	When     string   `yaml:"when"`
	Shapes   []string `yaml:"shapes"`
	// Advanced makes the input a tunable: never asked, recorded only when given, the compose
	// file's default otherwise.
	Advanced bool `yaml:"advanced"`
	// Profile names the compose profile the input turns on while it holds a value: the services
	// of that profile run only then.
	Profile string `yaml:"profile"`
}

// Profiles are the compose profiles the inputs turn on, sorted, each once.
func (s *Spec) Profiles() []string {
	var p []string
	for _, i := range s.Inputs {
		if i.Profile != "" && !slices.Contains(p, i.Profile) {
			p = append(p, i.Profile)
		}
	}
	slices.Sort(p)
	return p
}

// OnKubernetes reports whether the input is recorded on the kubernetes shape: its shapes allow it,
// and it is asked in no front mode (a front is the Docker shape's).
func (i Input) OnKubernetes() bool {
	return (len(i.Shapes) == 0 || slices.Contains(i.Shapes, "kubernetes")) && i.When == ""
}

type Hooks struct {
	Provision   *Provider    `yaml:"provision"`
	Maintenance *Maintenance `yaml:"maintenance"`
}

type Maintenance struct {
	DiskPressure *DiskPressure `yaml:"disk_pressure"`
}

type DiskPressure struct {
	SoftPct    int    `yaml:"soft_pct"`
	HardPct    int    `yaml:"hard_pct"`
	Post       string `yaml:"post"`
	AuthSecret string `yaml:"auth_secret"`
}

type Shapes struct {
	Docker     *DockerShape `yaml:"docker"`
	Kubernetes *K8sShape    `yaml:"kubernetes"`
	Kairos     *KairosShape `yaml:"kairos"`
}

// Names are the shapes the project has: docker, kubernetes, kairos, in that order.
func (s Shapes) Names() []string {
	var out []string
	if s.Docker != nil {
		out = append(out, "docker")
	}
	if s.Kubernetes != nil {
		out = append(out, "kubernetes")
	}
	if s.Kairos != nil {
		out = append(out, "kairos")
	}
	return out
}

type DockerShape struct{}

type K8sShape struct {
	Chart         string   `yaml:"chart"`
	PostgresModes []string `yaml:"postgres_modes"`
	// Schema lets `k8s upgrade` record the database's schema and `k8s rollback` check it: the
	// sql schema-compat queries run with psql in a pod of Image, with the environment of the
	// container of the workload running database.schema_compat's reads_newer_image, connecting
	// to the URL the variable URLEnv holds there.
	Schema *K8sSchema `yaml:"schema"`
}

// K8sSchema is shapes.kubernetes.schema.
type K8sSchema struct {
	URLEnv string `yaml:"url_env"`
	Image  string `yaml:"image"`
}

// DiskFormats are the kairos shape's disk image formats, in the order they are made.
var DiskFormats = []string{"raw", "qcow2", "ova", "vhd", "gce"}

// CloudFormat is the disk format each cloud of cloud_publish imports: AWS a raw disk, Azure a
// fixed VHD, Google Cloud a disk.raw in a tar.gz.
var CloudFormat = map[string]string{"aws": "raw", "azure": "vhd", "gcp": "gce"}

type KairosShape struct {
	Base         string      `yaml:"base"`
	KairosInit   string      `yaml:"kairos_init"`
	DiskFormats  []string    `yaml:"disk_formats"`
	CloudPublish []string    `yaml:"cloud_publish"`
	HealthGate   *HealthGate `yaml:"health_gate"`
	// HostAPI are the compose services given the host daemon's app socket and its token on the VM.
	HostAPI []string `yaml:"host_api"`
}

// Formats are the shape's disk formats, those cloud_publish's clouds import included, in the
// order they are made (DiskFormats'), whatever the order the spec lists them in.
func (k *KairosShape) Formats() []string {
	var out []string
	for _, f := range DiskFormats {
		if slices.Contains(k.DiskFormats, f) || slices.ContainsFunc(k.CloudPublish, func(c string) bool { return CloudFormat[c] == f }) {
			out = append(out, f)
		}
	}
	return out
}

type HealthGate struct {
	DeadlineS           int `yaml:"deadline_s"`
	DeadlineAfterResetS int `yaml:"deadline_after_reset_s"`
	PollS               int `yaml:"poll_s"`
}

// DefaultHealthGate is the health gate of a spec that sets none.
var DefaultHealthGate = HealthGate{DeadlineS: 600, DeadlineAfterResetS: 1800, PollS: 30}

// Gate is the shape's health gate: health_gate, or DefaultHealthGate.
func (k *KairosShape) Gate() HealthGate {
	if k == nil || k.HealthGate == nil {
		return DefaultHealthGate
	}
	return *k.HealthGate
}

// Load reads and strictly decodes a spec file. It checks the format version and nothing else;
// semantic checks are the linter's.
func Load(path string) (*Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse strictly decodes spec bytes.
func Parse(raw []byte) (*Spec, error) {
	var probe struct {
		APIVersion string `yaml:"apiVersion"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("stowage.yaml: %w", err)
	}
	if probe.APIVersion != APIVersion {
		if probe.APIVersion == "" {
			return nil, errors.New("stowage.yaml: apiVersion is missing (expected " + APIVersion + ")")
		}
		return nil, fmt.Errorf("stowage.yaml: apiVersion %q is not supported by this stowage (it reads %s) — upgrade stowage", probe.APIVersion, APIVersion)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("stowage.yaml: %w", err)
	}
	return &s, nil
}

// InsideRoot refuses a spec path that is absolute or climbs out of the project root.
func InsideRoot(p string) error {
	if p == "" {
		return errors.New("empty path")
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return errors.New("must be relative to the project root")
	}
	c := filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
	if c == ".." || strings.HasPrefix(c, "../") {
		return errors.New("leaves the project root")
	}
	return nil
}

// ProjectPath resolves a spec path that must name something inside the project.
func ProjectPath(root, rel string) (string, error) {
	if err := InsideRoot(rel); err != nil {
		return "", fmt.Errorf("%q %w", rel, err)
	}
	return Path(root, rel), nil
}

// Path joins a spec-relative path to the project root. It does not check the path: callers that
// read or hash what it names use ProjectPath, or a path InsideRoot already accepted.
func Path(root, rel string) string {
	return filepath.Join(root, filepath.FromSlash(rel))
}

// ImageNames returns the image keys in a stable order.
func (s *Spec) ImageNames() []string {
	return sortedKeys(s.Images)
}

// ChannelNames returns the channel names in a stable order.
func (s *Spec) ChannelNames() []string {
	return sortedKeys(s.Channels)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// NamePattern is the form of a project's name and short_name: 2–40 lowercase letters, digits or
// '-', starting with a letter. Both name files and repositories, so neither may name a path.
var NamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,38}[a-z0-9]$`)

// Short is the name the installer is named after: short_name, else name.
func (s *Spec) Short() string { return cmp.Or(s.ShortName, s.Name) }

// Signs reports whether the project signs its releases: a signing provider other than none.
func (s *Spec) Signs() bool { return s.Signing != nil && s.Signing.Name != "none" }

// RuntimeView is the part of the spec that reaches an install: the spec without the fields only
// the publisher reads (version and changelog providers, channels, push credentials, the signing
// key, the delete provider). Artifacts embed this view, so their inputs digests hash it and nothing else: fixing push
// credentials or a channel template must not change what a release contains.
func (s *Spec) RuntimeView() Spec {
	v := *s
	v.Version = VersionSpec{}
	v.Changelog = Provider{}
	v.Channels = nil
	v.Registry.Push = Provider{}
	v.Registry.Delete = Provider{}
	if s.Signing != nil {
		pub := Provider{Name: s.Signing.Name}
		if keys, ok := s.Signing.Config["public_keys"]; ok {
			pub.Config = map[string]any{"public_keys": keys}
		}
		v.Signing = &pub
	}
	return v
}

// BundleView is the runtime view the Kairos OS bundle carries: without the disk formats and cloud
// targets, which are made from the bundle and never read on it, so adding a format builds only
// its disk.
func (s *Spec) BundleView() Spec {
	v := s.RuntimeView()
	if k := s.Shapes.Kairos; k != nil {
		c := *k
		c.DiskFormats, c.CloudPublish = nil, nil
		v.Shapes.Kairos = &c
	}
	return v
}
