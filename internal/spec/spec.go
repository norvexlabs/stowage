// Package spec parses stowage.yaml, the one file a project writes to declare what Stowage packages.
package spec

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/backup"
	"github.com/norvexlabs/stowage/internal/chown"
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
	ShortName string             `yaml:"short_name"`
	Stowage   StowageReq         `yaml:"stowage"`
	Version   VersionSpec        `yaml:"version"`
	Changelog Provider           `yaml:"changelog"`
	Channels  map[string]Channel `yaml:"channels"`
	Registry  Registry           `yaml:"registry"`
	Signing   *Provider          `yaml:"signing"`
	Images    map[string]Image   `yaml:"images"`
	Stack     Stack              `yaml:"stack"`
	// Services are what the project asks for its compose services beyond their compose file, by
	// service.
	Services     map[string]Service `yaml:"services"`
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
	// Host is what the stack asks of the machine it runs on: kernel modules and packages.
	Host *Host `yaml:"host"`
	// Tune is how the hosts of the docker and kairos shapes are tuned for the stack's network.
	Tune *Tune `yaml:"tune"`
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
// the VM's backup runner, the volume helper).
var ReservedImageKeys = []string{"release", "release-seq", "os", "disk", "installer", front.Key, backup.Key, chown.Key, "audit" /* sbom.Key */}

// Built reports whether Stowage builds this image (as opposed to pulling a pinned reference).
func (i Image) Built() bool { return i.Ref == "" }

type Stack struct {
	Compose string `yaml:"compose"`
}

// Service is what the project asks for one compose service beyond its compose file
// (services.<name>): what Stowage gives it from the host it runs on.
type Service struct {
	// Memory is its share of the host's RAM (Docker and the VM): the percent of it its memory
	// limit is, capped at the share's maximum, never below the limit its compose file sets.
	Memory *Share `yaml:"memory"`
	// CPUs are the whole cores it runs on alone, which the install picks from the host's.
	CPUs int `yaml:"cpus"`
	// IRQ is where the host's NICs' interrupts and receive steering go: off its cores (off, the
	// default) or on them too (on).
	IRQ string `yaml:"irq"`
	// Nftables is its nftables table, a file of the project holding one `table inet <service>`.
	Nftables string `yaml:"nftables"`
	// Interface is the host.interfaces NIC it has an address of its own on, an ipvlan network
	// beside the stack's bridge, the address from AddressInput.
	Interface    string `yaml:"interface"`
	AddressInput string `yaml:"address_input"`
	// Update is how a start replaces its container: stop-first (the default), or start-first, the
	// new container healthy beside the old before the old one stops.
	Update string `yaml:"update"`
	// UpdateSocket is a directory, in the container, both containers of a start-first overlap
	// mount: where the old one hands its listening sockets to the new.
	UpdateSocket string `yaml:"update_socket"`
}

// The orders of services.<name>.update.
const (
	UpdateStopFirst  = "stop-first"
	UpdateStartFirst = "start-first"
)

// The postures of services.<name>.irq.
const (
	IRQOff = "off"
	IRQOn  = "on"
)

// MemoryShares are the services' shares of the host's RAM, by service: none for one without.
func (s *Spec) MemoryShares() map[string]Share {
	out := map[string]Share{}
	for name, svc := range s.Services {
		if svc.Memory != nil {
			out[name] = *svc.Memory
		}
	}
	return out
}

// Tune names the tuning profile the stack's hosts take: "default" (Docker's path as fast as the
// host's network) or "network" (a host serving a heavy network load). Absent is "default".
type Tune struct {
	Profile string `yaml:"profile"`
}

// TuneProfile is the release's tuning profile's name: tune.profile, "default" when absent.
func (s *Spec) TuneProfile() string {
	if s.Tune == nil || s.Tune.Profile == "" {
		return "default"
	}
	return s.Tune.Profile
}

// MaxMemoryShares is the total of the shares: the rest of the RAM is the OS's, the engine's and
// Stowage's own.
const MaxMemoryShares = 90

// MinMemLimit is the least limit a share gives, and the least maximum it takes: a share of a small
// host must still start its service.
const MinMemLimit Bytes = 64 << 20

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
	// Interface is the host.interfaces role the front serves on, which the VM's firewall opens
	// its ports on alone; empty, every interface.
	Interface string `yaml:"interface"`
}

// L4 is the stack's layer-4 routes: host ports published to a service as they are, TCP or UDP,
// beside the front and never through it.
type L4 struct {
	Routes []L4Route `yaml:"routes"`
}

// L4Route publishes the host port Port, over Protocol, to Upstream (service:port).
// AddressInput names the input holding the host name or address clients dial, which the stack
// reads as the route's endpoint (L4Var). AllowInput names the input holding the source addresses
// (CIDRs) the VM's firewall lets reach the port: empty refuses every one. BindInput names the
// advanced input holding the host address the port is published on: empty is every address.
type L4Route struct {
	Name         string `yaml:"name"`
	Protocol     string `yaml:"protocol"`
	Port         int    `yaml:"port"`
	Upstream     string `yaml:"upstream"`
	AddressInput string `yaml:"address_input"`
	AllowInput   string `yaml:"allow_input"`
	BindInput    string `yaml:"bind_input"`
	// Interface is the host.interfaces role the route listens on, which the VM's firewall opens
	// its port on alone; empty, every interface.
	Interface string `yaml:"interface"`
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
	// ClientAuth makes the front require a client certificate the install's client CA issued
	// (--client-ca) on the route: without one given, and in loopback mode, it refuses everyone.
	ClientAuth bool `yaml:"client_auth"`
	// BasicAuth makes the front ask for a user and its password before proxying.
	BasicAuth *BasicAuth `yaml:"basic_auth"`
}

// BasicAuth is a route's login: the user's name, and the key of the spec secret or secret input
// holding its password. The front refuses everyone while that value is empty.
type BasicAuth struct {
	User   string `yaml:"user"`
	Secret string `yaml:"secret"`
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

// Declared reports whether the front section has anything in it: an absent or empty one is no
// front.
func (f Front) Declared() bool { return !reflect.ValueOf(f).IsZero() }

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
	// UID is the user a volume is given when its root belongs to another (Docker and the VM);
	// nil leaves it as Docker makes it.
	UID         *int `yaml:"uid"`
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
var DiskFormats = []string{"raw", "qcow2", "ova", "vhd", "gce", "iso"}

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
	// Size is the VM's least and recommended size, which `vm create` sizes a VM from.
	Size *VMSize `yaml:"size"`
}

// Host is what the stack asks of the machine it runs on, whatever the shape: written where Stowage
// owns the host (the kairos shape's OS bundle and its boot); elsewhere `tune` reports its modules,
// packages and kernel arguments as advice, and its settings and power posture as its profile's,
// which `tune --apply` writes when the operator runs it.
type Host struct {
	// Modules are the kernel modules the host loads, blocks or gives options, by name.
	Modules map[string]KernelModule `yaml:"modules"`
	// Packages are the distribution's packages the host has beyond the OS bundle's own, by name.
	Packages map[string]HostPackage `yaml:"packages"`
	// Firmware are the files the host's drivers load, by their path under /lib/firmware.
	Firmware map[string]HostPackage `yaml:"firmware"`
	// Interfaces are the host's network interfaces by role, the role their name: the NICs on each
	// side of the stack and the devices its services open.
	Interfaces map[string]HostInterface `yaml:"interfaces"`
	// KernelArgs are the kernel's command-line parameters the host boots with, by name.
	KernelArgs map[string]HostValue `yaml:"kernel_args"`
	// Sysctls are kernel settings beyond those a tune profile sets, by key.
	Sysctls map[string]HostValue `yaml:"sysctls"`
	// Power is the CPUs' posture: performance (their full speed) or platform (the host's own, the
	// default).
	Power string `yaml:"power"`
}

// HostInterface is one of host.interfaces: a NIC the host has (kind nic, the default), named by its
// role from its MAC address where the install knows it, or a device a service of the stack opens
// (kind tun), which only the routing and the firewall name; its MTU, and its policy routing in
// networkd's own terms, each entry a [RoutingPolicyRule] or [Route] section's settings.
type HostInterface struct {
	Kind string `yaml:"kind"`
	// Primary is the interface of the default route, which the install's own addressing (vm
	// create's --network, network set) is.
	Primary            bool                `yaml:"primary"`
	MTU                int                 `yaml:"mtu"`
	RoutingPolicyRules []map[string]string `yaml:"routing_policy_rules"`
	Routes             []map[string]string `yaml:"routes"`
	// Ethtool is a NIC's settings in ethtool's own terms, over the tune profile's.
	Ethtool *Ethtool `yaml:"ethtool"`
	Reason  string   `yaml:"reason"`
}

// Ethtool is one of host.interfaces' NIC settings: its channels (combined queues: max or a
// count), its rings (rx, tx: max or a count), its features by name (on or off, each set where the
// driver lets it be changed), and its Energy-Efficient Ethernet (on or off).
type Ethtool struct {
	Channels *struct {
		Combined string `yaml:"combined"`
	} `yaml:"channels"`
	Rings *struct {
		RX string `yaml:"rx"`
		TX string `yaml:"tx"`
	} `yaml:"rings"`
	Features map[string]string `yaml:"features"`
	EEE      string            `yaml:"eee"`
}

// NftTableName is a service's nftables table's name, the service's: a letter, then letters, digits
// and _ - . (what both a compose service's name and nft's identifier take, nft's / left out: the
// installer names a file after it).
var NftTableName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]*$`)

// nftKeywords are words nft reads as its own where a table's name goes: a service of one has no
// table of its own.
var nftKeywords = []string{"accept", "add", "arp", "bridge", "chain", "comment", "continue", "counter", "create", "ct",
	"define", "delete", "describe", "destroy", "dnat", "drop", "dup", "element", "elements", "ether", "exthdr", "fib",
	"filter", "flags", "flow", "flowtable", "flush", "fwd", "goto", "hash", "hook", "icmp", "icmpv6", "include",
	"inet", "insert", "ip", "ip6", "jhash", "jump", "limit", "list", "log", "map", "masquerade", "meta", "monitor",
	"nat", "netdev", "notrack", "numgen", "osf", "policy", "priority", "queue", "quota", "redefine", "redirect",
	"reject", "rename", "replace", "reset", "return", "route", "rt", "rule", "ruleset", "secmark", "set", "size",
	"snat", "socket", "symhash", "synproxy", "table", "tcp", "timeout", "tproxy", "type", "udp", "undefine", "vlan",
	"ah", "cgroup", "comp", "dccp", "esp", "export", "geneve", "get", "gre", "handle", "igmp", "iif", "iifname",
	"import", "index", "last", "mark", "oif", "oifname", "pkttype", "position", "sctp", "skgid", "skuid", "th",
	"udplite", "vmap", "vxlan"}

// NftKeyword reports whether name is one nft reads as its own (nftKeywords): its keywords are
// lower-case, and nft's identifiers case-sensitive.
func NftKeyword(name string) bool { return slices.Contains(nftKeywords, name) }

// EthtoolFeature is a feature's name as ethtool -k lists it.
var EthtoolFeature = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// The kinds of host.interfaces.
const (
	InterfaceNIC = "nic"
	InterfaceTUN = "tun"
)

// InterfaceName is a role's name, the interface's: a kernel's interface name of lower-case letters
// and digits; ReservedInterfacePrefixes are the names the kernel, udev and Docker give, which a
// role never takes.
var (
	InterfaceName             = regexp.MustCompile(`^[a-z][a-z0-9]{0,14}$`)
	ReservedInterfacePrefixes = []string{"lo", "en", "eth", "wl", "ww", "ib", "docker", "br", "veth", "virbr", "tun", "tap", "sit", "gre", "ip6", "dummy", "bond"}
	// RoutingPolicyRuleKeys and RouteKeys are the networkd settings a routing entry may give.
	RoutingPolicyRuleKeys = []string{"DestinationPort", "Family", "FirewallMark", "From", "IPProtocol", "IncomingInterface", "Invert", "OutgoingInterface", "Priority", "SourcePort", "SuppressPrefixLength", "Table", "To", "Type"}
	RouteKeys             = []string{"Destination", "Gateway", "GatewayOnLink", "MTUBytes", "Metric", "PreferredSource", "Protocol", "Scope", "Source", "Table", "Type"}
	// RoutingValue is what a routing setting's value may hold: an address, a prefix, a mark and
	// its mask, a port range, a word.
	RoutingValue = regexp.MustCompile(`^[A-Za-z0-9_.:/,+-]{1,64}$`)
)

// HostPackage is one of host.packages or host.firmware: why the stack needs it.
type HostPackage struct {
	Reason string `yaml:"reason"`
}

// HostValue is one of host.kernel_args or host.sysctls: its value (a kernel argument's may be
// empty: a flag), and why.
type HostValue struct {
	Value  string `yaml:"value"`
	Reason string `yaml:"reason"`
}

// KernelModule is one of host.modules: what the host does with the module, and why.
type KernelModule struct {
	Load  bool `yaml:"load"`
	Block bool `yaml:"block"`
	// Options are the module's parameters, by name.
	Options map[string]string `yaml:"options"`
	Reason  string            `yaml:"reason"`
}

// ModuleName, ModuleOption and ModuleValue are what a kernel module's name, a parameter's name
// and its value may hold: they are written into modprobe's files and the bundle's build.
var (
	ModuleName   = regexp.MustCompile(`^[a-z0-9_]{1,56}$`)
	ModuleOption = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
	ModuleValue  = regexp.MustCompile(`^[A-Za-z0-9_.,:/+-]{1,256}$`)
)

// PackageName is what a Debian package's name may hold (Debian policy 5.6.1): it is written into
// the bundle's build.
var PackageName = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{1,63}$`)

// FirmwarePath is a firmware file's path under /lib/firmware, as a driver asks for it: names of
// letters, digits and _ . + -, none starting with a dot, one / apart, at most 200 (no "..", no
// leading /). A file there compressed (.zst, .xz) is the same file.
var FirmwarePath = regexp.MustCompile(`^[A-Za-z0-9_+-][A-Za-z0-9_.+-]*(/[A-Za-z0-9_+-][A-Za-z0-9_.+-]*){0,15}$`)

// KernelArgName and KernelArgValue are what a kernel argument's name and value may hold, and
// SysctlKey and SysctlValue a kernel setting's: written into the boot loader's script, sysctl's
// file and /proc/sys, so no space but a sysctl value's single ones, no quote, no $ and no path.
var (
	KernelArgName  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)
	KernelArgValue = regexp.MustCompile(`^[A-Za-z0-9_.,:/+@-]{1,128}$`)
	SysctlKey      = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_-]+){1,8}$`)
	SysctlValue    = regexp.MustCompile(`^[A-Za-z0-9_.,:/+-]+( [A-Za-z0-9_.,:/+-]+){0,7}$`)
)

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

// Requires reads stowage.requires alone from spec bytes, leniently, so a Stowage the project does
// not accept can refuse before a key it does not know stops the strict decode. It is "" when the
// key is absent or the bytes do not decode (Parse reports that).
func Requires(raw []byte) string {
	var probe struct {
		Stowage struct {
			Requires string `yaml:"requires"`
		} `yaml:"stowage"`
	}
	_ = yaml.Unmarshal(raw, &probe)
	return probe.Stowage.Requires
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

// HasFront reports whether the project has a front (Front.Declared). Without one, nothing is
// served over HTTP and its L4 routes are the stack's only way in.
func (s *Spec) HasFront() bool { return s.Front.Declared() }

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

// BundleView is the runtime view the Kairos OS bundle carries: without the disk formats, the cloud
// targets and the VM's size, which are read where a VM is made and never on it, so adding a format
// builds only its disk.
func (s *Spec) BundleView() Spec {
	v := s.RuntimeView()
	if k := s.Shapes.Kairos; k != nil {
		c := *k
		c.DiskFormats, c.CloudPublish, c.Size = nil, nil, nil
		v.Shapes.Kairos = &c
	}
	return v
}
