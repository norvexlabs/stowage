// Package lint checks a project's stowage.yaml and the files it names, so a spec defect is found
// at ship time rather than on an operator's host.
package lint

import (
	"cmp"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/changelog"
	"github.com/norvexlabs/stowage/internal/digest"
	"github.com/norvexlabs/stowage/internal/dockerfile"
	"github.com/norvexlabs/stowage/internal/helm"
	"github.com/norvexlabs/stowage/internal/kairos"
	"github.com/norvexlabs/stowage/internal/lock"
	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/tune"
	"github.com/norvexlabs/stowage/internal/version"
)

// Severity of a finding. Any Error fails `stowage lint` and blocks `stowage ship`.
type Severity string

const (
	Error   Severity = "error"
	Warning Severity = "warning"
)

// Finding is one lint result. Where is a spec key path or a file:line.
type Finding struct {
	Severity Severity `json:"severity"`
	Where    string   `json:"where"`
	Message  string   `json:"message"`
}

func (f Finding) String() string { return fmt.Sprintf("%s: %s: %s", f.Severity, f.Where, f.Message) }

// Result is a lint run's findings plus what it resolved on the way.
type Result struct {
	Findings []Finding `json:"findings"`
	Version  string    `json:"version,omitempty"`
}

// Errors counts the error findings.
func (r *Result) Errors() int {
	n := 0
	for _, f := range r.Findings {
		if f.Severity == Error {
			n++
		}
	}
	return n
}

type linter struct {
	root     string
	specPath string
	s        *spec.Spec
	now      time.Time
	findings []Finding
	compose  *Compose
	scheme   version.Scheme
	version  string
	// releasable: the version validated and its tag can be released; schemeFailed: the scheme
	// could not answer (as opposed to refusing the version).
	releasable   bool
	schemeFailed bool
}

func (l *linter) errf(where, format string, args ...any) {
	l.findings = append(l.findings, Finding{Error, where, fmt.Sprintf(format, args...)})
}

func (l *linter) warnf(where, format string, args ...any) {
	l.findings = append(l.findings, Finding{Warning, where, fmt.Sprintf(format, args...)})
}

var (
	imageKeyRe  = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	imageNameRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	pinnedRe    = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)
	prefixRe    = regexp.MustCompile(`^(?:[a-z0-9.-]+|\[[0-9a-f:]+\])(?::[0-9]+)?(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
	upstreamRe  = regexp.MustCompile(`^([a-z0-9][a-z0-9_.-]*):([0-9]{1,5})$`)
	imageVarRe  = regexp.MustCompile(`^\$\{([A-Z0-9_]+)\}$`)
	// routePathRe is a route's path prefix: clean, absolute, no trailing '/', no wildcard or
	// placeholder the front would read.
	routePathRe = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)+$`)
)

// Run lints the spec at specPath with paths relative to root. now feeds the scheme's commit guard.
func Run(root, specPath string, now time.Time) (*Result, error) {
	s, err := spec.Load(specPath)
	if err != nil {
		return nil, err
	}
	// Providers resolve paths against the root, and a command provider runs from it.
	if root, err = filepath.Abs(root); err != nil {
		return nil, err
	}
	l := &linter{root: root, specPath: specPath, s: s, now: now}
	l.name()
	l.versioning()
	l.channels()
	l.registry()
	l.images()
	l.stack()
	l.memory()
	l.vmSize()
	l.tune()
	l.host()
	l.front()
	l.l4()
	l.healthAndDatabase()
	l.data()
	l.secretsAndInputs()
	l.hooks()
	l.shapes()
	l.lockFile(filepath.Join(filepath.Dir(specPath), lock.FileName))
	sort.SliceStable(l.findings, func(i, j int) bool {
		return l.findings[i].Severity == Error && l.findings[j].Severity != Error
	})
	return &Result{Findings: l.findings, Version: l.version}, nil
}

func (l *linter) exists(where, rel string, dir bool) bool {
	if rel == "" {
		l.errf(where, "is required")
		return false
	}
	if err := spec.InsideRoot(rel); err != nil {
		l.errf(where, "%s %v", rel, err)
		return false
	}
	fi, err := os.Stat(spec.Path(l.root, rel))
	if err != nil {
		l.errf(where, "%s: %v", rel, err)
		return false
	}
	if fi.IsDir() != dir {
		kind := "a file"
		if dir {
			kind = "a directory"
		}
		l.errf(where, "%s is not %s", rel, kind)
		return false
	}
	return true
}

// processVars are variables the docker CLI itself needs from its environment; an install's
// recorded values take their place in that environment, so a project may not declare them.
var processVars = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "SHELL": true, "TMPDIR": true, "TERM": true, "LANG": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
	"SSH_AUTH_SOCK": true, "XDG_CONFIG_HOME": true, "XDG_RUNTIME_DIR": true, "XDG_CACHE_HOME": true,
	"TMP": true, "TEMP": true, "LC_ALL": true, "LANGUAGE": true, "TZ": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "GODEBUG": true, "GOMAXPROCS": true,
	"LD_PRELOAD": true, "LD_LIBRARY_PATH": true,
}

// recordable checks a value the spec gives an install can be written to its .env as is.
func (l *linter) recordable(where, v string) {
	if err := spec.CheckEnvValue(v); err != nil {
		l.errf(where, "%v", err)
	}
}

func (l *linter) name() {
	if !spec.NamePattern.MatchString(l.s.Name) {
		l.errf("name", "%q must be 2–40 lowercase letters, digits or '-', starting with a letter", l.s.Name)
	}
	if l.s.ShortName != "" && !spec.NamePattern.MatchString(l.s.ShortName) {
		l.errf("short_name", "%q must be 2–40 lowercase letters, digits or '-', starting with a letter", l.s.ShortName)
	}
	if short := l.s.Short(); short == "stowage" || strings.HasPrefix(short, "stowage-") {
		key := "short_name"
		if l.s.ShortName == "" {
			key = "name"
		}
		l.errf(key, "%q gives the installer %s, a name the install directory keeps for Stowage's own files (stowage-*): set another short_name", short, providers.Ctl(short))
	}
	if l.s.Stowage.Requires == "" {
		l.warnf("stowage.requires", "no accepted Stowage range declared")
	} else if _, err := version.ParseSemVerRange(l.s.Stowage.Requires); err != nil {
		l.errf("stowage.requires", "%v", err)
	}
}

func (l *linter) versioning() {
	sch, err := version.NewScheme(l.root, l.s.Version.Scheme)
	if err != nil {
		l.errf("version.scheme", "%v", err)
	}
	src, err := version.NewSource(l.root, l.s.Version.Source)
	if err != nil {
		l.errf("version.source", "%v", err)
	}
	cl, err := changelog.New(l.root, l.s.Changelog)
	if err != nil {
		l.errf("changelog", "%v", err)
	}
	l.scheme = sch
	if src == nil || sch == nil {
		return
	}
	v, err := src.Get()
	if err != nil {
		l.errf("version.source", "%v", err)
		return
	}
	if err := sch.Validate(v); err != nil {
		l.errf("version.scheme", "%v", err)
		l.schemeFailed = !version.IsInvalid(err)
		return
	}
	l.version = v
	if tag, err := sch.Tag(v); err != nil {
		l.errf("version.scheme", "%v", err)
	} else if err := version.CheckVersionTag(tag); err != nil {
		l.errf("version.scheme", "%v", err)
	} else {
		l.releasable = true
	}
	if err := sch.Guard(v, l.now); err != nil {
		l.warnf("version.scheme", "%v", err)
	}
	if cl == nil {
		return
	}
	if _, ok, err := cl.Section(v); err != nil {
		l.errf("changelog", "%v", err)
	} else if !ok {
		l.errf("changelog", "no section for the current version %s: operators would see an empty changelog", v)
	}
}

func (l *linter) channels() {
	if len(l.s.Channels) == 0 {
		l.errf("channels", "at least one channel is required")
		return
	}
	latest := 0
	for _, name := range l.s.ChannelNames() {
		c := l.s.Channels[name]
		where := "channels." + name
		if !ChannelName(name) {
			l.errf(where, "channel name must be lowercase letters, digits, '.', '_' or '-', at most 128 characters")
		}
		if err := version.CheckTemplate(c.Tag); err != nil {
			l.errf(where+".tag", "%v", err)
		}
		if c.Latest {
			latest++
			if c.Mutable {
				l.warnf(where, "latest moves with a mutable channel: the installer :latest would follow rebuilt releases")
			}
		}
	}
	if latest > 1 {
		l.errf("channels", "only one channel may move :latest")
	}
	if latest == 0 {
		l.warnf("channels", "no channel sets latest: the bootstrap one-liner (installer :latest) never moves")
	}
	if l.scheme == nil || l.schemeFailed {
		return // versioning() reported the scheme; asking it again would report it twice
	}
	var chs []version.Channel
	for _, name := range l.s.ChannelNames() {
		chs = append(chs, version.Channel{Name: name, Template: l.s.Channels[name].Tag})
	}
	conflicts, err := version.TemplateConflicts(chs, l.scheme, version.ReservedTags)
	if err != nil {
		// The scheme could not answer; the version-level check would ask it the same questions.
		l.errf("channels", "cannot check the channels' tags: %v", err)
		return
	}
	// A version that cannot be released is reported by versioning(); nothing to check here.
	if l.releasable {
		more, err := version.VersionConflicts(chs, l.scheme, l.version)
		if err != nil {
			l.errf("channels", "cannot check the channels' tags: %v", err)
		}
		conflicts = append(conflicts, more...)
	}
	for _, c := range conflicts {
		l.errf("channels", "%s: every channel writes into the same repositories", c)
	}
}

func (l *linter) registry() {
	r := l.s.Registry
	if r.Prefix == "" {
		l.errf("registry.prefix", "is required: the registry is the single deployment point")
	} else if strings.Contains(r.Prefix, "://") || !prefixRe.MatchString(r.Prefix) {
		l.errf("registry.prefix", "%q must be <host>[:port][/path] with no scheme, tag or trailing slash", r.Prefix)
	}
	if err := providers.RegistryAuth(r.Pull, false); err != nil {
		l.errf("registry.pull", "%v", err)
	}
	if err := providers.RegistryAuth(r.Push, true); err != nil {
		l.errf("registry.push", "%v", err)
	}
	if err := providers.RegistryDelete(r.Delete, r.Push); err != nil {
		l.errf("registry.delete", "%v", err)
	}
	l.pullAccount()
	if err := providers.Signing(l.s.Signing); err != nil {
		l.errf("signing", "%v", err)
	} else if l.s.Signs() {
		c, _ := providers.CosignKey(l.s.Signing)
		for i, k := range c.PublicKeys {
			l.publicKey(fmt.Sprintf("signing.public_keys[%d]", i), k)
		}
	}
}

// publicKey checks a file holds a PEM public key cosign can verify with.
func (l *linter) publicKey(where, rel string) {
	if !l.exists(where, rel, false) {
		return
	}
	data, err := os.ReadFile(spec.Path(l.root, rel))
	if err != nil {
		l.errf(where, "%s: %v", rel, err)
		return
	}
	b, _ := pem.Decode(data)
	if b == nil || b.Type != "PUBLIC KEY" {
		l.errf(where, "%s is not a PEM public key (\"PUBLIC KEY\" block, as cosign generate-key-pair writes)", rel)
		return
	}
	if _, err := x509.ParsePKIXPublicKey(b.Bytes); err != nil {
		l.errf(where, "%s: %v", rel, err)
	}
}

func (l *linter) images() {
	if len(l.s.Images) == 0 {
		l.errf("images", "at least one image is required")
	}
	for _, name := range l.s.ImageNames() {
		img := l.s.Images[name]
		where := "images." + name
		if !imageNameRe.MatchString(name) {
			// '-' is the only separator, so each key has its own STOWAGE_IMAGE_<KEY> variable.
			l.errf(where, "image key must be lowercase letters and digits separated by single '-'")
		}
		if slices.Contains(spec.ReservedImageKeys, name) {
			l.errf(where, "image key %q is reserved: %s-%s is where Stowage publishes its own artifacts", name, l.s.Name, name)
		}
		if !img.Built() {
			if img.Context != "" || img.Dockerfile != "" || img.Target != "" || len(img.Args) > 0 || len(img.AddHosts) > 0 || img.Network != "" || len(img.Inputs) > 0 {
				l.errf(where, "ref and build fields are exclusive")
			}
			if !pinnedRe.MatchString(img.Ref) {
				l.errf(where+".ref", "%q must be pinned by digest (<ref>@sha256:<64 hex>)", img.Ref)
			}
			continue
		}
		if l.exists(where+".context", img.Context, true) && l.exists(where+".dockerfile", img.Dockerfile, false) {
			if ig, err := digest.DockerIgnore(spec.Path(l.root, img.Context), spec.Path(l.root, img.Dockerfile)); err != nil {
				l.errf(where, "%v", err)
			} else if g, err := digest.GitInContext(ig); err != nil {
				l.errf(where+".context", "%v", err)
			} else if g != "" {
				rel, _ := filepath.Rel(l.root, g)
				l.errf(where+".context", "%s: %v", rel, digest.ErrGitInContext)
			}
		}
		if l.exists(where+".dockerfile", img.Dockerfile, false) {
			stages := l.baseImages(where+".dockerfile", img.Dockerfile)
			if img.Target != "" && stages != nil && !stages[strings.ToLower(img.Target)] {
				l.errf(where+".target", "%q is not a stage of %s", img.Target, img.Dockerfile)
			}
		}
		for i, in := range img.Inputs {
			l.exists(fmt.Sprintf("%s.inputs[%d]", where, i), in, isDirHint(spec.Path(l.root, in)))
		}
		for _, h := range slices.Sorted(maps.Keys(img.AddHosts)) {
			if !hostName(h) {
				l.errf(where+".add_hosts", "%q is not a host name (lowercase, like registry.example.test)", h)
			}
			if len(img.AddHosts[h]) == 0 {
				l.errf(where+".add_hosts."+h, "is empty: the addresses the name resolves to, like [192.0.2.7]")
			}
			seen := map[netip.Addr]bool{}
			for i, v := range img.AddHosts[h] {
				a, err := netip.ParseAddr(v)
				switch {
				case err != nil || a.Zone() != "":
					l.errf(fmt.Sprintf("%s.add_hosts.%s[%d]", where, h, i), "%q is not an IP address without a zone (like 192.0.2.7 or 2001:db8::7)", v)
				case seen[a]:
					l.errf(fmt.Sprintf("%s.add_hosts.%s[%d]", where, h, i), "%s is given twice", v)
				}
				seen[a] = true
			}
		}
		if img.Network != "" && !slices.Contains(spec.BuildNetworks, img.Network) {
			l.errf(where+".network", "%q is not a build network: %s", img.Network, strings.Join(spec.BuildNetworks, ", "))
		}
	}
}

// baseImages requires every stage of a built image to start from an image pinned by digest (or
// from an earlier stage, or scratch). The Dockerfile's bytes are part of the image's inputs
// digest, so a pinned base is how a base-image update reaches the digest — and so the release.
// It returns the Dockerfile's stage names (nil when unreadable).
func (l *linter) baseImages(where, rel string) map[string]bool {
	src, err := os.ReadFile(spec.Path(l.root, rel))
	if err != nil {
		l.errf(where, "%v", err)
		return nil
	}
	parsed, err := dockerfile.Parse(src)
	if err != nil {
		l.errf(where, "%s: %v", rel, err)
		return nil
	}
	if parsed.Frontend != "" {
		l.warnf(where, "%s uses the frontend %s: its base images and stages cannot be checked", rel, parsed.Frontend)
		return nil
	}
	froms := parsed.Froms()
	stages := map[string]bool{}
	for _, f := range froms {
		at := fmt.Sprintf("%s:%d", rel, f.Line)
		switch {
		case stages[strings.ToLower(f.Image)], f.Image == "scratch", pinnedRe.MatchString(f.Image):
		case strings.Contains(f.Image, "$"):
			l.warnf(at, "FROM %s is set by a build argument: its pinning cannot be checked", f.Image)
		default:
			l.errf(at, "FROM %s must be pinned by digest (<image>:<tag>@sha256:<64 hex>): an unpinned base changes without changing the image's inputs digest, so a rebuild would never happen", f.Image)
		}
		if f.Stage != "" {
			stages[f.Stage] = true
		}
	}
	return stages
}

// isDirHint lets exists() check an input path whose kind the spec does not state.
func isDirHint(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// imageVars maps each STOWAGE_IMAGE_<KEY> variable to its images key.
func (l *linter) imageVars() map[string]string {
	m := map[string]string{}
	for _, k := range l.s.ImageNames() {
		m[ImageVar(k)] = k
	}
	return m
}

func (l *linter) stack() {
	where := "stack.compose"
	if !l.exists(where, l.s.Stack.Compose, false) {
		return
	}
	c, err := ParseCompose(spec.Path(l.root, l.s.Stack.Compose))
	if err != nil {
		l.errf(where, "%v", err)
		return
	}
	l.compose = c
	file := l.s.Stack.Compose
	if _, ok := c.TopLevel["name"]; ok {
		l.errf(file, "top-level name: is set by Stowage per install (the project name), remove it")
	}
	if len(c.Services) == 0 {
		l.errf(file, "declares no service")
	}
	for _, section := range []string{"volumes", "networks"} {
		if m, ok := c.TopLevel[section].(map[string]any); ok {
			for _, n := range sortedKeys(m) {
				if strings.HasPrefix(n, "stowage-") {
					l.errf(file+": "+section+"."+n, "names starting stowage- are Stowage's own (the front's data volume is stowage-front-data)")
				}
			}
		}
	}
	inherited := c.inherit()
	for _, name := range sortedKeys(inherited) {
		l.errf(file+": services."+name+".extends", "%v", inherited[name])
	}
	imageVars := l.imageVars()
	used := map[string]bool{}
	for _, name := range sortedKeys(c.Services) {
		svc := c.Services[name]
		w := file + ": services." + name
		if strings.HasPrefix(name, "stowage-") {
			l.errf(w, "names starting stowage- are Stowage's own (it adds the front as stowage-front)")
		}
		if PublishesBeyondLoopback(svc.Ports) {
			switch {
			case l.s.Shapes.Kairos != nil:
				l.errf(w+".ports", "the VM's firewall opens only what Stowage publishes: declare the port in l4.routes")
			case l.s.Shapes.Docker != nil:
				l.warnf(w+".ports", "published past Stowage, which neither checks nor lists it: declare it in l4.routes")
			}
		}
		if svc.NetworkMode == "host" {
			switch {
			case l.s.Shapes.Kairos != nil:
				l.errf(w+".network_mode", "host: the VM's firewall opens only what Stowage publishes, and a port on the host's network is none: publish it with l4.routes")
			case l.s.Shapes.Docker != nil:
				l.warnf(w+".network_mode", "host: the service listens on the host past Stowage, which neither checks nor lists its ports: publish them with l4.routes")
			}
		}
		// Not refused: a stack may need one (a VPN's NET_ADMIN). Each makes a compromise of the
		// service one of the host.
		switch b, known := composeBool(svc.Privileged); {
		case b:
			l.warnf(w+".privileged", "true: the container holds every power of the host's root, its devices included; a compromise of the service is the host's")
		case !known:
			l.warnf(w+".privileged", "%v, which only a run knows or Compose does not read as a boolean: if true, the container holds every power of the host's root, its devices included; a compromise of the service is the host's", svc.Privileged)
		}
		if caps := joinCaps(svc.CapAdd); caps != "" {
			l.warnf(w+".cap_add", "%s: powers of the host's root beyond Docker's defaults; give the fewest the service needs", caps)
		}
		if svc.Pid == "host" {
			l.warnf(w+".pid", "host: the service sees and signals every process of the host")
		}
		// A one-shot is made again when a re-run changes it; Compose restarts a service waiting for
		// it only with restart: true.
		if deps, ok := svc.DependsOn.(map[string]any); ok {
			for _, d := range sortedKeys(deps) {
				on, _ := deps[d].(map[string]any)
				if on["condition"] != "service_completed_successfully" {
					continue
				}
				switch b, known := composeBool(on["restart"]); {
				case !known:
					l.warnf(w+".depends_on."+d, "restart: %v, which only a run knows or Compose does not read as a boolean: unless true, when a re-run makes the one-shot %s again, %s keeps what it read of its last run (a file it renders, read once)", on["restart"], d, name)
				case !b:
					l.warnf(w+".depends_on."+d, "waits for the one-shot %s without restart: true: when a re-run makes %s again, %s keeps what it read of its last run (a file it renders, read once)", d, d, name)
				}
			}
		}
		if MountsDockerSocket(svc.Volumes) {
			l.warnf(w+".volumes", "the Docker socket is mounted, or a directory holding it: the service drives the host's engine, so holds the host")
		}
		for _, p := range svc.Profiles {
			if !slices.Contains(l.s.Profiles(), p) {
				l.errf(w+".profiles", "no input turns %s on: name it in an input's profile:, or remove it (no install ever starts this service)", p)
			}
		}
		if svc.Build != nil {
			l.errf(w, "build: is forbidden — a host without the image would build instead of pull; images are built by stowage ship")
		}
		m := imageVarRe.FindStringSubmatch(svc.Image)
		if m == nil || imageVars[m[1]] == "" {
			l.errf(w+".image", "%q must be ${STOWAGE_IMAGE_<KEY>} for a key of images: the registry is the single deployment point", svc.Image)
			continue
		}
		used[imageVars[m[1]]] = true
	}
	for _, k := range l.s.ImageNames() {
		if used[k] || l.hookUsesImage(k) {
			continue
		}
		l.warnf("images."+k, "used by no compose service and no hook")
	}
}

// memory checks services: each a service the compose file declares (when it reads), asking
// something; its memory share from 1 %, the shares together at most spec.MaxMemoryShares, each
// maximum one the service can be given; the rest serviceHost.
func (l *linter) memory() {
	if len(l.s.Services) == 0 {
		if l.leftEmpty("services") {
			l.errf("services", "names no service: give one what it asks of the host, or remove services")
		}
		return
	}
	total, shares := 0, false
	for _, name := range sortedKeys(l.s.Services) {
		svc, at := l.s.Services[name], "services."+name
		l.service(at, name)
		if reflect.ValueOf(svc).IsZero() {
			l.errf(at, "says nothing: memory, cpus, nftables, transparent, host_network, interface or update")
		}
		l.serviceHost(at, name, svc)
		share := svc.Memory
		if share == nil {
			continue
		}
		shares = true
		if share.Max != 0 {
			l.shareMax(at+".memory", name, *share)
		}
		if pct := share.Percent; pct < 1 || pct > spec.MaxMemoryShares {
			l.errf(at+".memory", "%d: a percent of the host's RAM, from 1 to %d", pct, spec.MaxMemoryShares)
			continue
		}
		total += share.Percent
	}
	l.containerJoins()
	if total > spec.MaxMemoryShares {
		l.errf("services", "the memory shares total %d %%: at most %d, the rest is the OS's, the engine's and Stowage's", total, spec.MaxMemoryShares)
	}
	if shares && l.s.Shapes.Docker == nil && l.s.Shapes.Kairos == nil {
		l.warnf("services", "only the docker and kairos shapes size services from the host: the chart gives its pods their resources")
	}
}

// shareMax checks a share's maximum: at least spec.MinMemLimit and, split among the service's
// containers, at least what its compose file gives each of them itself (a share never gives less),
// and a warning when every host the shapes allow has RAM enough that the percent is always over
// it: only the VM's least size bounds a host, of which its kernel shows at least vmRAMShown.
func (l *linter) shareMax(at, name string, share spec.Share) {
	at += ".max"
	floor, n := spec.Bytes(0), 1
	if l.compose != nil {
		floor, n = l.compose.Services[name].MemFloor()
	}
	// A maximum is split among the service's containers: each part is checked.
	part, each := share.Max/spec.Bytes(n), name
	if n > 1 {
		each = fmt.Sprintf("each of %s's %d containers", name, n)
	}
	switch {
	case part < spec.MinMemLimit:
		l.errf(at, "%s: %s for %s, under %s, the least limit a share gives", share.Max, part, each, spec.MinMemLimit)
		return
	case part < floor:
		l.errf(at, "%s: %s for %s, under the %s limit or reservation %s gives it itself, which a share never goes below", share.Max, part, each, floor, l.s.Stack.Compose)
		return
	}
	k := l.s.Shapes.Kairos
	if l.s.Shapes.Docker != nil || k == nil || k.Size == nil || k.Size.Min == nil || k.Size.Min.Memory == 0 || share.Percent < 1 || share.Percent > spec.MaxMemoryShares {
		return
	}
	// Divided first: a size of petabytes times a percent would overflow.
	shown := k.Size.Min.Memory / 100 * vmRAMShown
	if least := shown / 100 * spec.Bytes(share.Percent); least >= share.Max {
		l.warnf(at, "%s: %d %% of the least VM's %s (%s of it shown by its kernel) is already %s, so the limit is always the maximum: give %s a %s limit in %s instead", share.Max, share.Percent, k.Size.Min.Memory, shown&^(1<<20-1), least&^(1<<20-1), each, part&^(1<<20-1), l.s.Stack.Compose)
	}
}

// vmRAMShown is the percent of a VM's memory its kernel shows, at least: the firmware and the
// kernel keep the rest.
const vmRAMShown = 90

// vmSize checks shapes.kairos.size: sizes vm create can make, the least at most the recommended.
func (l *linter) vmSize() {
	k := l.s.Shapes.Kairos
	if k == nil {
		return
	}
	z := k.Size
	if z == nil && !l.leftEmpty("shapes", "kairos", "size") {
		return
	}
	where := "shapes.kairos.size"
	if z == nil || z.Min == nil && z.Recommended == nil {
		l.errf(where, "states no size: give min, recommended or both, or remove size")
		return
	}
	check := func(at string, m *spec.Machine) bool {
		ok := true
		if m.CPUs == 0 && m.Memory == 0 && m.Disk == 0 {
			l.errf(at, "states nothing: give cpus, memory or disk")
			return false
		}
		if m.Memory != 0 && (m.Memory < spec.MinVMMemory || m.Memory%(1<<20) != 0) {
			l.errf(at+".memory", "%s: whole MiB, at least %s", m.Memory, spec.MinVMMemory)
			ok = false
		}
		if m.Disk != 0 && (m.Disk < spec.MinVMDisk || m.Disk%(1<<30) != 0) {
			l.errf(at+".disk", "%s: whole GiB, at least %s", m.Disk, spec.MinVMDisk)
			ok = false
		}
		return ok
	}
	for _, f := range []struct {
		key string
		m   *spec.Machine
	}{{"min", z.Min}, {"recommended", z.Recommended}} {
		if f.m == nil && l.leftEmpty("shapes", "kairos", "size", f.key) {
			l.errf(where+"."+f.key, "states nothing: give cpus, memory or disk, or remove %s", f.key)
		}
	}
	minOK := z.Min != nil && check(where+".min", z.Min)
	recOK := z.Recommended != nil && check(where+".recommended", z.Recommended)
	if !minOK || !recOK {
		return
	}
	lo, hi, at := z.Min, z.Recommended, where+".recommended."
	if lo.CPUs != 0 && hi.CPUs != 0 && lo.CPUs > hi.CPUs {
		l.errf(at+"cpus", "%d: under the minimum %d", hi.CPUs, lo.CPUs)
	}
	if lo.Memory != 0 && hi.Memory != 0 && lo.Memory > hi.Memory {
		l.errf(at+"memory", "%s: under the minimum %s", hi.Memory, lo.Memory)
	}
	if lo.Disk != 0 && hi.Disk != 0 && lo.Disk > hi.Disk {
		l.errf(at+"disk", "%s: under the minimum %s", hi.Disk, lo.Disk)
	}
}

// tune checks tune: a profile tune knows.
func (l *linter) tune() {
	t := l.s.Tune
	if t == nil && !l.leftEmpty("tune") {
		return
	}
	if t == nil || t.Profile == "" {
		l.errf("tune.profile", "missing: default or network, or remove tune")
		return
	}
	if _, err := tune.ParseProfile(t.Profile); err != nil {
		l.errf("tune.profile", "%q: default or network", t.Profile)
		return
	}
	if l.s.Shapes.Docker == nil && l.s.Shapes.Kairos == nil {
		l.warnf("tune.profile", "only the docker and kairos shapes tune their hosts: the nodes are the cluster's, and `k8s install --unsafe-sysctls` gives the pods the network profile's settings")
	}
}

// leftEmpty: the spec writes the key at path with nothing under it, which decodes as no key at
// all.
func (l *linter) leftEmpty(path ...string) bool {
	raw, err := os.ReadFile(l.specPath)
	if err != nil {
		return false
	}
	var doc yaml.Node
	if yaml.Unmarshal(raw, &doc) != nil || len(doc.Content) == 0 {
		return false
	}
	n := doc.Content[0]
	for _, k := range path {
		if n.Kind != yaml.MappingNode {
			return false
		}
		var next *yaml.Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == k {
				next = n.Content[i+1]
			}
		}
		if next == nil {
			return false
		}
		n = next
	}
	return true
}

func (l *linter) hookUsesImage(key string) bool {
	compat := l.s.DB().SchemaCompat
	for _, p := range []*spec.Provider{l.s.Hooks.Provision, l.s.Backup, &l.s.Health, &compat} {
		if p != nil && p.Config["image"] == key {
			return true
		}
	}
	return false
}

func (l *linter) service(where, name string) {
	if l.compose == nil || name == "" {
		return
	}
	if _, ok := l.compose.Services[name]; !ok {
		l.errf(where, "service %q is not in %s", name, l.s.Stack.Compose)
	}
}

func (l *linter) front() {
	if !l.s.HasFront() {
		return // a stack served by its L4 routes alone, if any
	}
	f := l.s.Front
	if len(f.TLSModes) == 0 {
		l.errf("front.tls_modes", "at least one TLS mode is required")
	}
	for _, m := range f.TLSModes {
		if !slices.Contains(providers.TLSModes, m) {
			l.errf("front.tls_modes", "unknown mode %q (%s)", m, strings.Join(providers.TLSModes, ", "))
		}
	}
	if len(f.Routes) == 0 {
		l.errf("front.routes", "at least one route is required")
	}
	names := map[string]bool{}
	vars := map[string]string{}
	domains := map[string]string{} // domain_input and path → route
	shared := map[string]int{}     // domain_input → its routes
	for _, r := range f.Routes {
		shared[r.DomainInput]++
	}
	for i, r := range f.Routes {
		w := fmt.Sprintf("front.routes[%d]", i)
		if r.ClientAuth && shared[r.DomainInput] > 1 {
			l.errf(w+".client_auth", "%s is shared with another route, and a client certificate is asked for a whole domain (TLS): give the route a domain of its own", r.DomainInput)
		}
		if !imageKeyRe.MatchString(r.Name) {
			l.errf(w+".name", "%q must be lowercase letters, digits, '.', '_' or '-'", r.Name)
		}
		if names[r.Name] {
			l.errf(w+".name", "duplicate route %q", r.Name)
		}
		names[r.Name] = true
		if other, dup := vars[spec.URLVar(r.Name)]; dup && other != r.Name {
			l.errf(w+".name", "%q and %q both give %s: route names must differ by more than '-', '.' and '_'", other, r.Name, spec.URLVar(r.Name))
		}
		vars[spec.URLVar(r.Name)] = r.Name
		if !providers.EnvName.MatchString(r.DomainInput) {
			l.errf(w+".domain_input", "%q is not an environment variable name", r.DomainInput)
		}
		if seg := strings.Split(r.Path, "/"); r.Path != "" && (!routePathRe.MatchString(r.Path) || slices.Contains(seg, ".") || slices.Contains(seg, "..")) {
			l.errf(w+".path", "%q is not a path prefix: /, then segments of letters, digits, '.', '_', '~' or '-', no trailing / (like /idp or /api/v2)", r.Path)
		}
		// The front matches paths whatever their case.
		if other, dup := domains[r.DomainInput+" "+strings.ToLower(r.Path)]; dup {
			key, where := ".domain_input", "every path"
			if r.Path != "" {
				key, where = ".path", r.Path
			}
			l.errf(w+key, "%s serves %s of %s already: routes of one domain differ by their path", other, where, r.DomainInput)
		}
		domains[r.DomainInput+" "+strings.ToLower(r.Path)] = r.Name
		m := upstreamRe.FindStringSubmatch(r.Upstream)
		if m == nil {
			l.errf(w+".upstream", "%q must be <service>:<port>", r.Upstream)
		} else {
			l.service(w+".upstream", m[1])
		}
		for _, h := range r.StripHeaders {
			if strings.TrimSuffix(h, "*") == "" || strings.ContainsAny(h, " :") {
				l.errf(w+".strip_headers", "%q is not a header name (a trailing * matches a prefix)", h)
			}
		}
		if a := r.ForwardAuth; a != nil {
			if a.AddressInput == "" {
				l.errf(w+".forward_auth.address_input", "is required: the input holding the authentication service's address")
			} else {
				l.frontInput(w+".forward_auth.address_input", a.AddressInput)
			}
			if a.URIInput != "" {
				l.frontInput(w+".forward_auth.uri_input", a.URIInput)
			}
			for _, h := range a.CopyHeaders {
				if !headerRe.MatchString(h) {
					l.errf(w+".forward_auth.copy_headers", "%q is not a header name", h)
				}
			}
		}
		if r.AllowInput != "" {
			l.frontInput(w+".allow_input", r.AllowInput)
			if in, ok := l.s.Input(r.AllowInput); ok && in.Advanced {
				l.errf(w+".allow_input", "%s is advanced, never asked: the route would refuse everyone until it is set", r.AllowInput)
			}
		}
		if a := r.BasicAuth; a != nil {
			l.basicAuth(w+".basic_auth", r)
		}
		if r.ClientAuth {
			if !slices.ContainsFunc(f.TLSModes, func(m string) bool { return m != "none" }) {
				l.errf(w+".client_auth", "no TLS mode is offered: plain HTTP carries no client certificate, so the route would refuse everyone")
			}
		}
	}
	if p := f.PublishCA; p != nil {
		if !slices.Contains(f.TLSModes, "internal") && !slices.Contains(f.TLSModes, "byo") {
			l.errf("front.publish_ca", "no mode offered has a CA to publish (internal, or byo with --ca)")
		}
		if l.compose != nil {
			if vols, _ := l.compose.TopLevel["volumes"].(map[string]any); vols == nil || !hasKey(vols, p.Volume) {
				l.errf("front.publish_ca.volume", "%q is not a volume %s declares", p.Volume, l.s.Stack.Compose)
			}
		}
	}
	if !names[f.Portal] {
		l.errf("front.portal", "%q names no route: the installer ends by printing the portal URL", f.Portal)
	}
	ports := map[int]string{}
	for _, name := range sortedKeys(f.LoopbackPorts) {
		port := f.LoopbackPorts[name]
		if !names[name] {
			l.errf("front.loopback_ports."+name, "names no route")
		}
		if port < 1 || port > 65535 {
			l.errf("front.loopback_ports."+name, "%d is not a TCP port", port)
		}
		if other, dup := ports[port]; dup {
			l.errf("front.loopback_ports."+name, "%d is already %s's port", port, other)
		}
		ports[port] = name
	}
	// Loopback mode is the mode none: a project that does not offer it owes no loopback port, and
	// a loopback key would do nothing.
	if slices.Contains(f.TLSModes, "none") {
		for _, r := range f.Routes {
			if _, ok := f.LoopbackPorts[r.Name]; !ok {
				l.errf("front.loopback_ports", "route %s has no port: loopback mode serves every route on its own port", r.Name)
			}
		}
	} else {
		if len(f.LoopbackPorts) > 0 {
			l.errf("front.loopback_ports", "tls_modes does not offer none, the mode loopback serves: no install serves these ports")
		}
		if len(f.LoopbackEnv) > 0 {
			l.errf("front.loopback_env", "tls_modes does not offer none, the mode loopback serves: no install relaxes these defaults")
		}
	}
	for _, k := range sortedKeys(f.LoopbackEnv) {
		if _, ok := l.s.SafeDefaults[k]; !ok {
			l.errf("front.loopback_env."+k, "is not a safe_defaults key: loopback mode only relaxes a safe default")
		}
	}
}

var headerRe = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]+$`)

// frontInput checks that key, which the front reads, is an input recorded on the Docker shape: a
// route's value is written into the front's configuration.
func (l *linter) frontInput(where, key string) {
	in, ok := l.s.Input(key)
	switch {
	case !ok:
		l.errf(where, "%s is not an input", key)
	case len(in.Shapes) > 0 && !slices.Contains(in.Shapes, "docker"):
		l.errf(where, "%s is not an input of the docker shape, which runs the front", key)
	case len(in.Shapes) > 0 && l.s.Shapes.Kairos != nil && !slices.Contains(in.Shapes, "kairos"):
		l.errf(where, "%s is not an input of the kairos shape, whose VM runs the front", key)
	case in.Required:
		l.errf(where, "%s is required: empty is how the front leaves it off (or refuses everyone)", key)
	case in.When != "":
		l.errf(where, "%s is asked only when %s: the front reads it in every mode", key, in.When)
	}
}

// l4 checks the L4 routes: a name, a protocol, a host port none other takes over that protocol,
// an upstream service of the compose file, and an address input. Over TCP a route's port is not
// one the front publishes by default — its HTTP and HTTPS ports in the TLS modes, a route's loopback
// port in loopback mode — and never 80 or 443 while acme is offered, whose challenges need them.
func (l *linter) l4() {
	f := l.s.Front
	for _, p := range []struct {
		key  string
		port int
	}{{"front.http_port", f.HTTPPort}, {"front.https_port", f.HTTPSPort}} {
		if p.port < 0 || p.port > 65535 {
			l.errf(p.key, "%d is not a TCP port", p.port)
		}
	}
	// The ports a TLS front publishes: nothing to a project offering loopback alone.
	tls := slices.ContainsFunc(f.TLSModes, func(m string) bool { return m != "none" })
	httpPort, httpsPort := cmp.Or(f.HTTPPort, 80), cmp.Or(f.HTTPSPort, 443)
	switch {
	case !tls:
	case httpPort == httpsPort:
		l.errf("front.https_port", "%d is the HTTP port too", httpsPort)
	case httpsPort == 80:
		l.errf("front.https_port", "80 is the front's plain HTTP port in its container: choose another")
	}
	taken := map[string]string{}
	if tls {
		taken[fmt.Sprintf("tcp/%d", httpPort)] = "the front's HTTP port"
		taken[fmt.Sprintf("tcp/%d", httpsPort)] = "the front's HTTPS port"
	}
	if slices.Contains(f.TLSModes, "acme") {
		taken["tcp/80"], taken["tcp/443"] = "acme's HTTP challenge port", "acme's TLS challenge port"
	}
	if slices.Contains(f.TLSModes, "none") {
		for _, name := range sortedKeys(f.LoopbackPorts) {
			taken[fmt.Sprintf("tcp/%d", f.LoopbackPorts[name])] = "route " + name + "'s loopback port"
		}
	}
	names := map[string]string{}
	for _, r := range f.Routes {
		names[r.Name] = "a front route's"
	}
	for i, r := range l.s.L4.Routes {
		w := fmt.Sprintf("l4.routes[%d]", i)
		if !l4Name.MatchString(r.Name) || len(r.Name) > 15 {
			l.errf(w+".name", "%q must be 1 to 15 lowercase letters, digits or '-', starting with a letter, no '-' at its end or beside another (a chart names a Service port after it)", r.Name)
		}
		if what, dup := names[r.Name]; dup {
			l.errf(w+".name", "%q is already %s name", r.Name, what)
		}
		names[r.Name] = "another L4 route's"
		if r.Protocol != "tcp" && r.Protocol != "udp" {
			l.errf(w+".protocol", "%q: expected tcp or udp", r.Protocol)
		}
		if r.Port < 1 || r.Port > 65535 {
			l.errf(w+".port", "%d is not a port", r.Port)
		}
		key := fmt.Sprintf("%s/%d", r.Protocol, r.Port)
		if other, dup := taken[key]; dup {
			l.errf(w+".port", "%s is %s", key, other)
		}
		taken[key] = "route " + r.Name + "'s port"
		if m := upstreamRe.FindStringSubmatch(r.Upstream); m == nil {
			l.errf(w+".upstream", "%q must be <service>:<port>", r.Upstream)
		} else if p, _ := strconv.Atoi(m[2]); p < 1 || p > 65535 {
			l.errf(w+".upstream", "%s is not a port", m[2])
		} else {
			l.service(w+".upstream", m[1])
			l.l4Host(w, r, m[1], p)
		}
		if k := r.AddressInput; k != "" {
			if in, ok := l.s.Input(k); !ok {
				l.errf(w+".address_input", "%s is not an input", k)
			} else if in.Secret {
				l.errf(w+".address_input", "%s is secret: the endpoint is handed to the stack and printed", k)
			} else if err := CheckL4Address(in.Default); err != nil && !strings.Contains(in.Default, "${") {
				l.errf(w+".address_input", "%s's default: %v", k, err)
			}
		}
		if k := r.AllowInput; k != "" {
			l.l4Allow(w+".allow_input", k)
		}
		if k := r.BindInput; k != "" {
			l.l4Bind(w+".bind_input", k)
		}
	}
}

// basicAuth checks a front route's login: a user name the Caddyfile holds as one token, and a
// password from a generated secret or a secret input, never another login beside it.
func (l *linter) basicAuth(where string, r spec.Route) {
	a := r.BasicAuth
	if !basicUserRe.MatchString(a.User) {
		l.errf(where+".user", "%q must be 1 to 64 letters, digits, '.', '_' or '-'", a.User)
	}
	if r.ForwardAuth != nil {
		l.errf(where, "and forward_auth both ask who the client is: keep one")
	}
	if i := slices.IndexFunc(l.s.Secrets, func(s spec.Secret) bool { return s.Key == a.Secret }); i >= 0 {
		if l.s.Secrets[i].Rotation {
			l.errf(where+".secret", "%s rotates, and the front holds one password: no rotation", a.Secret)
		}
		return
	}
	switch in, ok := l.s.Input(a.Secret); {
	case a.Secret == "":
		l.errf(where+".secret", "is required: the secret or secret input holding the password")
	case !ok:
		l.errf(where+".secret", "%s is neither a secret nor an input", a.Secret)
	case !in.Secret:
		l.errf(where+".secret", "%s is not secret: a password is never printed", a.Secret)
	}
}

// basicUserRe is a basic_auth user's name.
var basicUserRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// refTag is an image reference's tag: what follows the last ':' of its name, before its digest;
// "" without one.
func refTag(ref string) string {
	name, _, _ := strings.Cut(ref, "@")
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		return name[i+1:]
	}
	return ""
}

// l4Host checks an L4 route's service against how the VM runs it: on the host's network, with no
// Docker port publishing in between, so the service itself listens on the route's port, holds no
// network of the stack's, and runs as one container. A Docker host publishes it as it is.
func (l *linter) l4Host(where string, r spec.L4Route, name string, port int) {
	host := l.s.Services[name].HostNetwork
	if host && r.BindInput != "" && l.s.Shapes.Docker != nil {
		l.errf(where+".bind_input", "%s runs on the host's network (services.%s.host_network), where it listens on the addresses it binds itself: on Docker nothing holds it to %s's", name, name, r.BindInput)
	}
	if l.s.Shapes.Kairos == nil && !host || l.compose == nil {
		return
	}
	if port != r.Port {
		on := "on the VM the service runs"
		if host {
			on = "the service runs"
		}
		l.errf(where+".upstream", "%s: %s on the host's network, where nothing maps one port to another: it listens on the route's port, %d", r.Upstream, on, r.Port)
	}
	if !host {
		l.hostNetService(name, "the VM runs "+name+" on the host's network (the L4 route "+r.Name+")")
	}
}

// hostNetService checks a service that runs on the host's network (why says who runs it so)
// against it: no network of the stack's, one container.
func (l *linter) hostNetService(name, why string) {
	svc, ok := l.compose.Services[name]
	if !ok {
		return
	}
	at := l.s.Stack.Compose + ": services." + name
	switch {
	case svc.NetworksFrom != "":
		l.errf(l.s.Stack.Compose+": services."+svc.NetworksFrom+".networks", "%s extends %s, whose networks it takes: %s, which holds no other: leave them out of %s, or extend another", name, svc.NetworksFrom, why, svc.NetworksFrom)
	case svc.Networks != nil:
		l.errf(at+".networks", "%s, which holds no other: leave its networks out", why)
	}
	if _, n := svc.MemFloor(); n > 1 {
		l.errf(at, "%d containers: %s, where one container holds its ports", n, why)
	}
}

// l4Bind checks an L4 route's bind input: a tunable an operator sets on a host that serves
// several installs, so it is never asked (advanced) and is shown in the ports it publishes.
func (l *linter) l4Bind(where, key string) {
	in, ok := l.s.Input(key)
	switch {
	case !ok:
		l.errf(where, "%s is not an input", key)
	case !in.Advanced:
		l.errf(where, "%s must be advanced: the address is set only where a host serves several installs, never asked", key)
	case in.Secret:
		l.errf(where, "%s is secret: the ports the stack publishes show it", key)
	}
}

// l4Allow checks an L4 route's allowlist input: only the VM's firewall enforces it (a Docker
// host's is the operator's), so it is asked on the kairos shape alone, in every mode, and an
// empty one refuses every source; without a kairos shape nothing enforces it (a warning).
func (l *linter) l4Allow(where, key string) {
	in, ok := l.s.Input(key)
	switch {
	case !ok:
		l.errf(where, "%s is not an input", key)
	case l.s.Shapes.Kairos == nil:
		l.warnf(where, "only the VM's firewall enforces it, and the project has no kairos shape: the route is open to every source (a Docker host's firewall is the operator's)")
	case !slices.Equal(in.Shapes, []string{"kairos"}):
		l.errf(where, "%s must be asked on the kairos shape alone (shapes: [kairos]): only the VM's firewall enforces it, a Docker host's is the operator's", key)
	case in.Secret:
		l.errf(where, "%s is secret: the firewall's rules show it", key)
	case in.Advanced:
		l.errf(where, "%s is advanced, never asked: the route would refuse every source until it is set", key)
	case in.When != "":
		l.errf(where, "%s is asked only when %s: the firewall reads it in every mode", key, in.When)
	}
}

// l4Name is an L4 route's name: a Kubernetes Service port name (an IANA service name), so a chart
// can name its port after it.
var l4Name = regexp.MustCompile(`^[a-z](-?[a-z0-9])*$`)

func hasKey(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}

func (l *linter) healthAndDatabase() {
	svc, err := providers.Health(l.s.Health, l.s.Images)
	if err != nil {
		l.errf("health", "%v", err)
	}
	l.service("health.service", svc)
	if !l.s.HasDatabase() {
		return // a stateless project: what needs a database is refused where it is declared
	}
	d := l.s.DB()
	if d.Engine != "postgres" {
		l.errf("database.engine", "%q: only postgres is supported", d.Engine)
	}
	l.service("database.service", d.Service)
	if d.Service == "" {
		l.errf("database.service", "is required")
	}
	if d.Name == "" {
		l.errf("database.name", "is required")
	}
	if d.Roles.Migrate == "" || d.Roles.App == "" {
		l.errf("database.roles", "migrate and app are required (the same role twice is allowed)")
	}
	if err := providers.SchemaCompat(d.SchemaCompat, l.s.Images); err != nil {
		l.errf("database.schema_compat", "%v", err)
	}
}

// maxUID is the largest uid Linux gives a user: 2³²−1 is "no user".
const maxUID = 1<<32 - 2

func (l *linter) data() {
	for _, name := range sortedKeys(l.s.Data.Volumes) {
		if uid := l.s.Data.Volumes[name].UID; uid != nil && (*uid < 0 || *uid > maxUID) {
			l.errf("data.volumes."+name, "uid must be 0 to %d", maxUID)
		}
	}
	for _, r := range l.s.Data.Reset {
		v, ok := l.s.Data.Volumes[r]
		if !ok {
			l.errf("data.reset", "%q is not a data volume", r)
		} else if v.KeepOnReset {
			l.errf("data.reset", "%q is marked keep_on_reset", r)
		}
	}
	switch {
	case !l.s.HasDatabase():
		if len(l.s.Data.Reset) > 0 {
			l.errf("data.reset", "reset-db resets a database, and the project declares none")
		}
	case len(l.s.Data.Reset) == 0:
		l.warnf("data.reset", "empty: reset-db would wipe nothing")
	}
}

func (l *linter) secretsAndInputs() {
	keys := map[string]string{}
	claim := func(where, key, what string) {
		if !providers.EnvName.MatchString(key) {
			l.errf(where, "%q is not an environment variable name", key)
		}
		for _, p := range []string{"STOWAGE_", "COMPOSE_", "DOCKER_"} {
			if strings.HasPrefix(key, p) {
				l.errf(where, "%s: the %s prefix is reserved (Stowage and Docker read it)", key, p)
			}
		}
		if processVars[strings.ToUpper(key)] {
			l.errf(where, "%s: the Docker client reads it from its own environment; name the variable after the project", key)
		}
		if prev, ok := keys[key]; ok {
			l.errf(where, "%s is already declared as %s", key, prev)
		}
		keys[key] = what
	}
	for i, s := range l.s.Secrets {
		w := fmt.Sprintf("secrets[%d]", i)
		claim(w, s.Key, "a secret")
		if _, ok := providers.SecretGenerators[s.Gen]; !ok {
			l.errf(w+".gen", "unknown generator %q (%s)", s.Gen, strings.Join(sortedKeys(providers.SecretGenerators), ", "))
		}
		if s.Rotation {
			claim(w+".rotation", s.Key+"_NEXT", "a rotated secret's next value")
			claim(w+".rotation", s.Key+"_PREVIOUS", "a rotated secret's previous value")
			claim(w+".rotation", s.Key+"_ACCEPTED", "a rotated secret's other accepted value")
			l.rotated(w, s.Key)
		}
	}
	for _, k := range sortedKeys(l.s.SafeDefaults) {
		claim("safe_defaults."+k, k, "a safe default")
		l.recordable("safe_defaults."+k, l.s.SafeDefaults[k])
	}
	for _, k := range sortedKeys(l.s.Front.LoopbackEnv) {
		l.recordable("front.loopback_env."+k, l.s.Front.LoopbackEnv[k])
	}
	for i, in := range l.s.Inputs {
		w := fmt.Sprintf("inputs[%d]", i)
		claim(w, in.Key, "an input")
		if in.Prompt == "" {
			l.errf(w+".prompt", "is required: the installer asks it")
		}
		l.recordable(w+".default", in.Default)
		if in.Required && in.Optional {
			l.errf(w, "required and optional are exclusive")
		}
		if in.Secret && in.Default != "" {
			l.errf(w, "a secret input must not carry a default")
		}
		if in.Profile != "" {
			l.profileInput(w, in)
		}
		if in.Advanced && (in.Required || in.Default != "") {
			l.errf(w+".advanced", "an advanced input is never asked and takes the compose file's default: no required, no default")
		}
		switch in.When {
		case "":
		case "tls", "loopback":
			if !l.s.HasFront() {
				l.errf(w+".when", "%q: the project has no front, so no TLS mode: the input would never be asked", in.When)
			}
		default:
			l.errf(w+".when", "%q: expected tls or loopback", in.When)
		}
		for _, sh := range in.Shapes {
			if sh != "docker" && sh != "kubernetes" && sh != "kairos" {
				l.errf(w+".shapes", "unknown shape %q", sh)
			}
		}
	}
	for _, r := range l.s.Front.Routes {
		if _, ok := keys[r.DomainInput]; !ok {
			keys[r.DomainInput] = "a route domain"
		}
	}
	l.composeVars(keys)
	l.inputsConsumed()
	l.extensions()
}

// rotated checks that the compose file reads a rotated secret's next and previous values, or the
// one of them it holds (_ACCEPTED): a service that does not accept them drops the other age's
// tokens during a rotation.
func (l *linter) rotated(where, key string) {
	if l.compose == nil {
		return
	}
	reads := func(k string) bool {
		return slices.ContainsFunc(l.compose.Vars, func(v VarRef) bool { return v.Name == k })
	}
	if reads(key + "_ACCEPTED") {
		return
	}
	for _, k := range []string{key + "_NEXT", key + "_PREVIOUS"} {
		if !reads(k) {
			l.errf(where+".rotation", "%s reads no ${%s} (nor ${%s_ACCEPTED}, either of them): during a rotation its services accept both values", l.s.Stack.Compose, k, key)
		}
	}
}

func (l *linter) composeVars(keys map[string]string) {
	if l.compose == nil {
		return
	}
	file := l.s.Stack.Compose
	safeSeen := map[string]bool{}
	for _, v := range l.compose.Vars {
		w := fmt.Sprintf("%s:%d", file, v.Line)
		if want, ok := l.s.SafeDefaults[v.Name]; ok {
			safeSeen[v.Name] = true
			if v.Op != ":-" || v.Default != want {
				l.errf(w, "${%s} must be written ${%s:-%s}: the safe value is the default, only the installer relaxes it", v.Name, v.Name, want)
			}
			continue
		}
		if strings.HasPrefix(v.Name, "STOWAGE_IMAGE_") {
			if _, ok := l.imageVars()[v.Name]; !ok {
				l.errf(w, "%s names no image (the images keys give %s)", v.Name, strings.Join(sortedKeys(l.imageVars()), ", "))
			}
			continue
		}
		if strings.HasPrefix(v.Name, "STOWAGE_URL_") {
			if !slices.ContainsFunc(l.s.Front.Routes, func(r spec.Route) bool { return spec.URLVar(r.Name) == v.Name }) {
				l.errf(w, "%s names no front route", v.Name)
			}
			continue
		}
		if strings.HasPrefix(v.Name, "STOWAGE_L4_") {
			if !slices.ContainsFunc(l.s.L4.Routes, func(r spec.L4Route) bool { return spec.L4Var(r.Name) == v.Name && r.AddressInput != "" }) {
				l.errf(w, "%s names no L4 route with an address_input", v.Name)
			}
			continue
		}
		if v.Name == "STOWAGE_TLS_MODE" && !l.s.HasFront() {
			l.errf(w, "%s: the project has no front, so no TLS mode is recorded", v.Name)
			continue
		}
		if strings.HasPrefix(v.Name, "STOWAGE_") {
			if _, ok := ProvidedVars[v.Name]; !ok {
				l.errf(w, "%s is not a variable Stowage provides (%s, STOWAGE_IMAGE_<KEY>, STOWAGE_URL_<ROUTE>, STOWAGE_L4_<ROUTE>)", v.Name, strings.Join(sortedKeys(ProvidedVars), ", "))
			}
			continue
		}
		if v.NeedsValue() && slices.ContainsFunc(l.s.Inputs, func(i spec.Input) bool { return i.Key == v.Name && i.Advanced }) {
			l.errf(w, "${%s} is an advanced input, recorded only when given: write its default here, ${%s:-…}", v.Name, v.Name)
			continue
		}
		if _, ok := keys[v.Name]; ok || !v.NeedsValue() {
			continue
		}
		l.errf(w, "${%s} has no default and is no input, secret, safe default or route domain: compose would run with it empty or refuse to start", v.Name)
	}
	for _, k := range sortedKeys(l.s.SafeDefaults) {
		if !safeSeen[k] {
			l.errf("safe_defaults."+k, "not used in %s as ${%s:-%s}", file, k, l.s.SafeDefaults[k])
		}
	}
}

func (l *linter) inputsConsumed() {
	if l.compose == nil {
		return // without the compose file nothing can be said
	}
	read := l.readInputs()
	for i, in := range l.s.Inputs {
		if !read(in) {
			l.warnf(fmt.Sprintf("inputs[%d]", i), "%s is asked but read by no compose service and no hook", in.Key)
		}
	}
}

// profileInput checks an input that turns a compose profile on: a profile name compose accepts,
// carried by a service of the compose file, on a shape that runs it, and an input that may be
// left empty (a required one would always turn it on).
func (l *linter) profileInput(w string, in spec.Input) {
	if !composeProfile.MatchString(in.Profile) {
		l.errf(w+".profile", "%q is not a compose profile name ([a-zA-Z0-9][a-zA-Z0-9_.-]*)", in.Profile)
	}
	if in.Required {
		l.errf(w+".profile", "a required input always turns %s on: drop required, or the profile", in.Profile)
	}
	if len(in.Shapes) > 0 && !slices.Contains(in.Shapes, "docker") && !slices.Contains(in.Shapes, "kairos") {
		l.errf(w+".profile", "compose profiles run on the docker and kairos shapes, and this input is asked on neither")
	}
	if l.compose == nil {
		return
	}
	for _, svc := range l.compose.Services {
		if slices.Contains(svc.Profiles, in.Profile) {
			return
		}
	}
	l.errf(w+".profile", "no service of %s has the profile %s", l.s.Stack.Compose, in.Profile)
}

// composeProfile is a compose profile's name.
var composeProfile = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// readInputs reports whether something reads an input: a compose service, a route of the front
// or the provision hook's env; one asked on Kubernetes only is the chart's, taken as read.
func (l *linter) readInputs() func(spec.Input) bool {
	inCompose := map[string]bool{}
	for _, v := range l.compose.Vars {
		inCompose[v.Name] = true
	}
	// The provision hook reads the keys its env lists.
	if h, err := providers.DecodeProvision(l.s.Short(), l.s.Hooks.Provision, l.s.Images); err == nil {
		for _, k := range h.Keys() {
			inCompose[k] = true
		}
	}
	// The front reads its routes' inputs.
	for _, r := range l.s.Front.Routes {
		inCompose[r.DomainInput], inCompose[r.AllowInput] = true, true
		if a := r.ForwardAuth; a != nil {
			inCompose[a.AddressInput], inCompose[a.URIInput] = true, true
		}
		if a := r.BasicAuth; a != nil {
			inCompose[a.Secret] = true
		}
	}
	// The installer reads the L4 routes' inputs, and the services' addresses of their own.
	for _, r := range l.s.L4.Routes {
		inCompose[r.AddressInput], inCompose[r.AllowInput], inCompose[r.BindInput] = true, true, true
	}
	for _, svc := range l.s.Services {
		inCompose[svc.AddressInput] = true
	}
	return func(in spec.Input) bool {
		if in.Profile != "" {
			return true // the installer reads it: it turns the profile on
		}
		if len(in.Shapes) > 0 && !slices.Contains(in.Shapes, "docker") && !slices.Contains(in.Shapes, "kairos") {
			return true // asked on Kubernetes only, where the chart reads it
		}
		return inCompose[in.Key]
	}
}

// extensions checks each extension names an app and the inputs that configure it: an optional,
// non-secret input something reads for its endpoint, an optional secret one for its key; and
// that the project's own endpoint is one of its routes.
func (l *linter) extensions() {
	if e := l.s.Endpoint; e != "" && !slices.ContainsFunc(l.s.Front.Routes, func(r spec.Route) bool { return r.Name == e }) {
		l.errf("endpoint", "%q is not a route of front.routes (%s)", e, strings.Join(l.s.Front.RouteNames(), ", "))
	}
	input := l.s.Input
	read := func(spec.Input) bool { return true } // without the compose file nothing can be said
	if l.compose != nil {
		read = l.readInputs()
	}
	names, claimed := map[string]bool{}, map[string]string{}
	claim := func(where, key, ext string) {
		if prev, ok := claimed[key]; ok {
			l.errf(where, "%s is already the input of the extension %s", key, prev)
		}
		claimed[key] = ext
	}
	for i, e := range l.s.Extensions {
		w := fmt.Sprintf("extensions[%d]", i)
		switch {
		case !spec.NamePattern.MatchString(e.Name):
			l.errf(w+".name", "%q must be an app name: 2–40 lowercase letters, digits or '-', starting with a letter", e.Name)
		case e.Name == l.s.Name:
			l.errf(w+".name", "%q is this project: an extension is another app", e.Name)
		case names[e.Name]:
			l.errf(w+".name", "duplicate extension %q", e.Name)
		}
		names[e.Name] = true
		if e.Prompt == "" {
			l.errf(w+".prompt", "is required: the install's tip names the extension with it")
		}
		in, ok := input(e.Input)
		switch {
		case e.Input == "":
			l.errf(w+".input", "is required: the input holding the extension's endpoint")
		case !ok:
			l.errf(w+".input", "%q is not an input", e.Input)
		case !in.Optional || in.Secret:
			l.errf(w+".input", "%s must be an optional, non-secret input: its endpoint, empty while the extension is off", e.Input)
		case in.Advanced || in.When != "":
			l.errf(w+".input", "%s must be asked on every install (no advanced, no when): an install that does not record it empty shows no tip", e.Input)
		case in.Default != "":
			l.errf(w+".input", "%s must have no default: empty is how the extension is off", e.Input)
		case !read(in):
			l.errf(w+".input", "%s is read by no compose service and no hook: the endpoint would reach nothing", e.Input)
		}
		if e.Input != "" {
			claim(w+".input", e.Input, e.Name)
		}
		if e.KeyInput == "" {
			continue
		}
		if k, ok := input(e.KeyInput); !ok {
			l.errf(w+".key_input", "%q is not an input", e.KeyInput)
		} else if !k.Optional || !k.Secret {
			l.errf(w+".key_input", "%s must be an optional secret input: the extension's credential", e.KeyInput)
		} else if k.Advanced || k.When != "" || !slices.Equal(slices.Sorted(slices.Values(k.Shapes)), slices.Sorted(slices.Values(in.Shapes))) {
			l.errf(w+".key_input", "%s must be asked wherever %s is: the same shapes, no advanced, no when", e.KeyInput, e.Input)
		} else if !read(k) {
			l.errf(w+".key_input", "%s is read by no compose service and no hook: the credential would reach nothing", e.KeyInput)
		}
		claim(w+".key_input", e.KeyInput, e.Name)
	}
}

func (l *linter) hooks() {
	if h, err := providers.DecodeProvision(l.s.Short(), l.s.Hooks.Provision, l.s.Images); err != nil {
		l.errf("hooks.provision", "%v", err)
	} else if h != nil {
		l.hookEnv(h)
	}
	if m := l.s.Hooks.Maintenance; m != nil && m.DiskPressure != nil {
		d := m.DiskPressure
		w := "hooks.maintenance.disk_pressure"
		if l.s.Shapes.Kairos == nil && l.s.Shapes.Docker == nil {
			l.warnf(w, "the kairos shape's host daemon and a Docker install's timer watch the disk: with neither shape, nothing does")
		}
		if d.SoftPct < 1 || d.HardPct > 99 || d.SoftPct >= d.HardPct {
			l.errf(w, "need 1 <= soft_pct < hard_pct <= 99")
		}
		if d.AuthSecret != "" && !slices.ContainsFunc(l.s.Secrets, func(s spec.Secret) bool { return s.Key == d.AuthSecret }) {
			l.errf(w+".auth_secret", "%q is not a secret", d.AuthSecret)
		}
		host := strings.TrimPrefix(d.Post, "http://")
		if host == d.Post {
			l.errf(w+".post", "%q must be an http:// URL on the stack's internal network", d.Post)
		} else if m := upstreamRe.FindStringSubmatch(strings.SplitN(host, "/", 2)[0]); m == nil {
			l.errf(w+".post", "%q must name <service>:<port>", d.Post)
		} else {
			l.service(w+".post", m[1])
		}
	}
	if l.s.Backup != nil && !l.s.HasDatabase() {
		l.errf("backup", "a backup dumps the database, and the project declares none")
	} else if err := providers.Backup(l.s.Backup, l.s.Images); err != nil {
		l.errf("backup", "%v", err)
	} else if pg, err := providers.DecodeBackup(l.s.Backup); err == nil {
		l.backupVolumes(pg.Volumes)
	}
}

// backupVolumes checks the volumes a backup archives: each one the compose file declares and
// Compose creates, under a literal name (a backup finds it by its labels or that name), and none
// the database mounts (its files are the dump's, read live).
func (l *linter) backupVolumes(vols []string) {
	if l.compose == nil {
		return
	}
	decls, _ := l.compose.TopLevel["volumes"].(map[string]any)
	for i, v := range vols {
		w := fmt.Sprintf("backup.volumes[%d]", i)
		decl, ok := decls[v]
		if !ok {
			l.errf(w, "%q is not a volume %s declares", v, l.s.Stack.Compose)
			continue
		}
		d, _ := decl.(map[string]any)
		if ext, ok := d["external"]; ok && ext != false {
			l.errf(w, "%q is an external volume, which Compose does not own: a backup finds a volume by its labels", v)
		}
		if name, _ := d["name"].(string); strings.Contains(name, "$") {
			l.errf(w, "%q is named %q, which Compose interpolates: a backup finds a volume by its labels or a literal name", v, name)
		}
		if l.s.HasDatabase() {
			if svc, ok := l.compose.Services[l.s.DB().Service]; ok && mountsVolume(svc.Volumes, v) {
				l.errf(w, "%q is mounted by the database's service: the dump backs its data up, and an archive of its files read live would not restore", v)
			}
		}
	}
}

// hookEnv checks that each key the provision hook's env lists is one the install records (an
// input, a secret or a safe default), and that no key its returns lists is: the install's value
// would shadow it, and `provision --set` changes an input only through what records it.
func (l *linter) hookEnv(h *providers.ProvisionHook) {
	known := map[string]string{}
	for _, i := range l.s.Inputs {
		known[i.Key] = "an input"
	}
	for _, sec := range l.s.Secrets {
		known[sec.Key] = "a secret"
	}
	for k := range l.s.SafeDefaults {
		known[k] = "a safe default"
	}
	for i, k := range h.Keys() {
		if known[k] == "" {
			l.errf(fmt.Sprintf("hooks.provision.env[%d]", i), "%s is no input, secret or safe default (the URLs and what the hook returned before it receives without asking)", k)
		}
	}
	for _, r := range l.s.Front.Routes {
		if known[r.DomainInput] == "" {
			known[r.DomainInput] = "a route domain"
		}
	}
	for i, k := range h.Returns {
		if what := known[k]; what != "" {
			l.errf(fmt.Sprintf("hooks.provision.returns[%d]", i), "%s is %s, a value the install records, not one the hook returns", k, what)
		}
	}
}

// chart checks the chart packages as ship packages it, and that its templates read what ship and
// `<short>-stow k8s` give it: the release's images, the Secret of its secrets and inputs, the pull
// Secret, the pods' network settings.
func (l *linter) chart(rel string) {
	c, err := helm.Load(spec.Path(l.root, rel))
	if err != nil {
		l.errf("shapes.kubernetes.chart", "%s: %v", rel, err)
		return
	}
	reads := func(key string) bool {
		re := regexp.MustCompile(regexp.QuoteMeta(".Values."+helm.ValuesKey+"."+key) + `\b`)
		for _, f := range c.Files {
			if strings.HasPrefix(f.Name, "templates/") && re.Match(f.Data) {
				return true
			}
		}
		return false
	}
	if !reads("images") {
		l.warnf("shapes.kubernetes.chart", "%s: no template reads .Values.%s.images: the chart does not run the release's images by digest", rel, helm.ValuesKey)
	}
	k8sInput := slices.ContainsFunc(l.s.Inputs, spec.Input.OnKubernetes)
	if (len(l.s.Secrets) > 0 || k8sInput) && !reads("secret") {
		l.warnf("shapes.kubernetes.chart", "%s: no template reads .Values.%s.secret: the release's secrets and inputs do not reach the chart", rel, helm.ValuesKey)
	}
	if l.s.Registry.Pull.Name == "token" && !reads("pullSecret") {
		l.warnf("shapes.kubernetes.chart", "%s: no template reads .Values.%s.pullSecret: the cluster pulls the images without the pull account", rel, helm.ValuesKey)
	}
	if !reads("sysctls") {
		l.warnf("shapes.kubernetes.chart", "%s: no template reads .Values.%s.sysctls: the pods are not given the network settings `k8s install --unsafe-sysctls` sets", rel, helm.ValuesKey)
	}
	if len(l.s.L4.Routes) > 0 && !reads("l4") {
		l.warnf("shapes.kubernetes.chart", "%s: no template reads .Values.%s.l4: the L4 routes are not published on Kubernetes", rel, helm.ValuesKey)
	}
}

// k8sSchema checks shapes.kubernetes.schema: it needs the sql schema-compat provider, with the
// image it reads the label of, and names an image to run psql in. Without it, a project whose
// schema the Docker shape checks is not checked on Kubernetes: a warning.
func (l *linter) k8sSchema(k *spec.K8sShape) {
	const w = "shapes.kubernetes.schema"
	sql := l.s.HasDatabase() && l.s.DB().SchemaCompat.Name == "sql"
	sc := k.Schema
	if sc == nil {
		if sql {
			l.warnf(w, "is not set: `k8s rollback` is not checked against the database's schema, as Docker's and the VM's rollbacks are")
		}
		return
	}
	if !sql {
		l.errf(w, "needs a database whose schema_compat provider is sql")
		return
	}
	if !providers.EnvName.MatchString(sc.URLEnv) {
		l.errf(w+".url_env", "%q is not an environment variable name", sc.URLEnv)
	}
	if _, ok := l.s.Images[sc.Image]; !ok {
		l.errf(w+".image", "%q is not an images key (the image psql runs in)", sc.Image)
	}
	if c, err := providers.SQL(l.s.DB().SchemaCompat, l.s.Images); err == nil && c.ReadsNewerImage == "" {
		l.errf(w, "needs database.schema_compat.reads_newer_image: the workload running it gives the environment")
	}
}

func (l *linter) shapes() {
	sh := l.s.Shapes
	if sh.Docker == nil && sh.Kubernetes == nil && sh.Kairos == nil {
		l.errf("shapes", "at least one deploy shape is required")
	}
	if k := sh.Kubernetes; k != nil {
		if l.exists("shapes.kubernetes.chart", k.Chart, true) {
			l.chart(k.Chart)
		}
		if len(k.PostgresModes) > 0 && !l.s.HasDatabase() {
			l.errf("shapes.kubernetes.postgres_modes", "the database's modes, and the project declares no database")
		}
		for _, m := range k.PostgresModes {
			if m != "external" && m != "embedded" && m != "cnpg" {
				l.errf("shapes.kubernetes.postgres_modes", "unknown mode %q (external, embedded, cnpg)", m)
			}
		}
		l.k8sSchema(k)
	}
	if k := sh.Kairos; k != nil {
		w := "shapes.kairos"
		// Both are FROM lines of the bundle: pinned, so an update of either changes its inputs.
		for _, f := range []struct{ key, ref string }{{"base", k.Base}, {"kairos_init", k.KairosInit}} {
			switch {
			case f.ref == "":
				l.errf(w+"."+f.key, "is required")
			case !pinnedRe.MatchString(f.ref):
				l.errf(w+"."+f.key, "%q must be pinned by digest (<image>[:<tag>]@sha256:<64 hex>): an unpinned image changes without changing the OS bundle's inputs digest", f.ref)
			}
		}
		// The repository's last element, not the image: the build reads /etc/os-release and
		// refuses another distribution, so a mirror or a derived image of Ubuntu passes there.
		if name, _, _ := strings.Cut(k.Base, "@"); k.Base != "" {
			name = strings.TrimSuffix(name, ":"+refTag(k.Base))
			if repo := name[strings.LastIndex(name, "/")+1:]; repo != "ubuntu" {
				l.warnf(w+".base", "%s: the OS bundle's build takes Ubuntu alone (another distribution is refused, minutes into ship)", repo)
			}
		}
		// An equality of tags, not an ordering: the bundle's build refuses any other release.
		if tag := refTag(k.KairosInit); k.KairosInit != "" && tag != kairos.KairosInitRelease {
			l.warnf(w+".kairos_init", "%s: the OS bundle's build takes kairos-init %s alone, the release its boot assessment is derived from (a digest of another release is refused, minutes into ship)", cmp.Or(tag, "no tag"), kairos.KairosInitRelease)
		}
		for _, f := range k.DiskFormats {
			if !slices.Contains(spec.DiskFormats, f) {
				l.errf(w+".disk_formats", "unknown format %q (%s)", f, strings.Join(spec.DiskFormats, ", "))
			}
		}
		for _, c := range k.CloudPublish {
			if spec.CloudFormat[c] == "" {
				l.errf(w+".cloud_publish", "unknown cloud %q (%s)", c, strings.Join(slices.Sorted(maps.Keys(spec.CloudFormat)), ", "))
			}
		}
		if g := k.HealthGate; g != nil && (g.DeadlineS <= 0 || g.PollS <= 0 || g.PollS >= g.DeadlineS || g.DeadlineAfterResetS < g.DeadlineS) {
			l.errf(w+".health_gate", "need 0 < poll_s < deadline_s <= deadline_after_reset_s")
		}
		seen := map[string]bool{}
		for _, svc := range k.HostAPI {
			if seen[svc] {
				l.errf(w+".host_api", "service %q is named twice", svc)
			}
			seen[svc] = true
			l.service(w+".host_api", svc)
		}
		switch {
		case l.s.Backup == nil && !l.s.HasDatabase():
		case l.s.Backup == nil:
			l.warnf("backup", "the VM keeps its data locally and no backup is declared")
		case !providers.RunsBackups(l.s):
			l.warnf("backup", "the VM runs %s backups only: it schedules no %s backup", providers.VMBackup, l.s.Backup.Name)
		}
	}
}

// host checks host: its modules and packages are written into the OS bundle's build and printed by
// tune, and each entry says why.
func (l *linter) host() {
	h := l.s.Host
	if h == nil {
		return
	}
	l.kernelModules("host.modules", h.Modules)
	for _, name := range slices.Sorted(maps.Keys(h.Packages)) {
		at := "host.packages." + name
		if p, owned := kairos.OwnedPackage(name); owned {
			l.errf(at, "is the OS bundle's own (%s): the kernel, boot, init system, container runtime and Kairos are not the project's", p)
		} else if !spec.PackageName.MatchString(name) {
			l.errf(at, "not a package's name: lower-case letters, digits and + - ., starting with a letter or digit, 2 to 64")
		}
		l.reason(at+".reason", h.Packages[name].Reason)
	}
	for _, path := range slices.Sorted(maps.Keys(h.Firmware)) {
		at := "host.firmware." + path
		if len(path) > 200 || !spec.FirmwarePath.MatchString(path) {
			l.errf(at, "not a firmware file's path under /lib/firmware: names of letters, digits and _ . + -, none starting with a dot, one / apart, at most 200")
		}
		l.reason(at+".reason", h.Firmware[path].Reason)
	}
	for _, name := range slices.Sorted(maps.Keys(h.KernelArgs)) {
		a, at := h.KernelArgs[name], "host.kernel_args."+name
		switch k, owned := kairos.OwnedKernelArg(name); {
		case owned:
			l.errf(at, "is Kairos's own (%s): the root, the initrd, Kairos, the consoles and the init system are its boot's", k)
		case !spec.KernelArgName.MatchString(name):
			l.errf(at, "not a kernel argument's name: letters, digits and _ . -, at most 64")
		case a.Value != "" && !spec.KernelArgValue.MatchString(a.Value):
			l.errf(at+".value", "%q: letters, digits and _ . , : / + @ -, no space or quote, at most 128", a.Value)
		case kairos.MarksFallback(a.Value):
			l.errf(at+".value", "%q holds upgrade_failure, which marks a boot that fell back to the passive system", a.Value)
		}
		l.reason(at+".reason", a.Reason)
	}
	profile := tune.ProfileKeys()
	for _, key := range slices.Sorted(maps.Keys(h.Sysctls)) {
		c, at := h.Sysctls[key], "host.sysctls."+key
		switch {
		case !spec.SysctlKey.MatchString(key):
			l.errf(at, "not a kernel setting's key: dotted lower-case letters, digits, _ and -")
		case slices.Contains(profile, key):
			l.errf(at, "is a tune profile's: tune computes it from the host (`tune` reports it)")
		case !spec.SysctlValue.MatchString(c.Value):
			l.errf(at+".value", "%q: letters, digits and _ . , : / + -, fields of one space, at most 8", c.Value)
		case namespaced(key):
			l.warnf(at, "a namespace keeps its own: the host's value holds in the host's namespace (Docker's NAT and bridges among it), not in a container's own (the compose file's `sysctls` give a service its value)")
		}
		l.reason(at+".reason", c.Reason)
	}
	if p := h.Power; p != "" && p != tune.Performance && p != tune.Platform {
		l.errf("host.power", "%q: %s or %s (the default)", p, tune.Performance, tune.Platform)
	}
	l.interfaces(h.Interfaces)
}

// interfaces checks host.interfaces: each role is an interface's name, which networkd's files,
// the firewall's rules and tune's report carry; its routing is networkd's own settings, each
// written into a .network file; one NIC is the default route's when any NIC is declared (a tun
// device alone names none). A route or
// the front that names a role names a declared one.
func (l *linter) interfaces(ifs map[string]spec.HostInterface) {
	primaries, nics := 0, 0
	for _, role := range slices.Sorted(maps.Keys(ifs)) {
		i, at := ifs[role], "host.interfaces."+role
		switch {
		case !spec.InterfaceName.MatchString(role):
			l.errf(at, "not an interface's name: a lower-case letter, then lower-case letters and digits, at most 15")
		case slices.ContainsFunc(spec.ReservedInterfacePrefixes, func(p string) bool { return strings.HasPrefix(role, p) }):
			l.errf(at, "a name the kernel, udev or Docker gives (%s…): name the role, such as client, upstream or vpn0", strings.Join(spec.ReservedInterfacePrefixes, "…, "))
		}
		switch i.Kind {
		case "", spec.InterfaceNIC:
			nics++
			if i.Primary {
				primaries++
			}
		case spec.InterfaceTUN:
			if i.Primary {
				l.errf(at+".primary", "a tun device a service opens is not the default route's")
			}
		default:
			l.errf(at+".kind", "%q: %s (the default) or %s", i.Kind, spec.InterfaceNIC, spec.InterfaceTUN)
		}
		if i.MTU != 0 && (i.MTU < 1280 || i.MTU > 9216) {
			l.errf(at+".mtu", "%d: 1280 to 9216 (IPv6's least, a jumbo frame's most), or none for the network's", i.MTU)
		}
		if e := i.Ethtool; e != nil {
			l.ethtool(at+".ethtool", i.Kind == spec.InterfaceTUN, e)
		}
		l.routing(at+".routing_policy_rules", i.RoutingPolicyRules, spec.RoutingPolicyRuleKeys)
		l.routing(at+".routes", i.Routes, spec.RouteKeys)
		l.reason(at+".reason", i.Reason)
	}
	if nics > 0 && primaries != 1 {
		l.errf("host.interfaces", "%d NICs are primary: one is the default route's (primary: true)", primaries)
	}
	kairos := l.s.Shapes.Kairos != nil
	named := func(at, role string) {
		switch _, ok := ifs[role]; {
		case role == "":
		case !ok:
			l.errf(at, "%q is not one of host.interfaces", role)
		case !kairos:
			l.warnf(at, "only the VM's firewall opens a port on one interface alone (a kairos shape): a Docker host's firewall is its operator's")
		}
	}
	named("front.interface", l.s.Front.Interface)
	for i, r := range l.s.L4.Routes {
		named(fmt.Sprintf("l4.routes[%d].interface", i), r.Interface)
	}
	for _, role := range slices.Sorted(maps.Keys(ifs)) {
		fw, at := ifs[role].Forward, "host.interfaces."+role+".forward"
		if fw != nil && len(fw) == 0 {
			l.errf(at, "is empty: the roles the host forwards %s's traffic out of, like [uplink]", role)
		}
		for j, out := range fw {
			w := fmt.Sprintf("%s[%d]", at, j)
			switch _, ok := ifs[out]; {
			case !ok:
				l.errf(w, "%q is not one of host.interfaces", out)
			case out == role:
				l.errf(w, "is %s itself: the host forwards from one interface out of another", role)
			case slices.Index(fw, out) < j:
				l.errf(w, "%s is given twice", out)
			}
		}
		if len(fw) > 0 && !kairos {
			l.warnf(at, "only the VM's firewall is Stowage's (a kairos shape): a Docker host's operator accepts the forwarding in DOCKER-USER (tune advises it)")
		}
	}
}

// ethtool checks a NIC's ethtool settings: counts or max, each feature on or off by a name ethtool
// lists, EEE on or off; a tun device has none of them.
func (l *linter) ethtool(at string, tun bool, e *spec.Ethtool) {
	if tun {
		l.errf(at, "a tun device has no NIC's settings")
		return
	}
	count := func(w, v string, most int) {
		if v == "" || v == "max" {
			return
		}
		if n, err := strconv.Atoi(v); err != nil || n < 1 || n > most {
			l.errf(w, "%q: max, or a count from 1 to %d", v, most)
		}
	}
	if c := e.Channels; c != nil {
		if c.Combined == "" {
			l.errf(at+".channels", "says nothing: combined (max or a count)")
		}
		count(at+".channels.combined", c.Combined, 1024)
	}
	if r := e.Rings; r != nil {
		if r.RX == "" && r.TX == "" {
			l.errf(at+".rings", "says nothing: rx or tx (max or a count)")
		}
		count(at+".rings.rx", r.RX, 65536)
		count(at+".rings.tx", r.TX, 65536)
	}
	for _, f := range slices.Sorted(maps.Keys(e.Features)) {
		switch v := e.Features[f]; {
		case !spec.EthtoolFeature.MatchString(f):
			l.errf(at+".features", "%q is not a feature's name as ethtool -k lists it", f)
		case v != "on" && v != "off":
			l.errf(at+".features."+f, "%q: on or off", v)
		}
	}
	if v := e.EEE; v != "" && v != "on" && v != "off" {
		l.errf(at+".eee", "%q: on or off", v)
	}
}

// routing checks a list of networkd sections' settings: each key one networkd takes there, each
// value of the characters an address, a mark or a word holds.
func (l *linter) routing(at string, entries []map[string]string, keys []string) {
	for n, e := range entries {
		w := fmt.Sprintf("%s[%d]", at, n)
		if len(e) == 0 {
			l.errf(w, "is empty")
		}
		for _, k := range slices.Sorted(maps.Keys(e)) {
			switch {
			case !slices.Contains(keys, k):
				l.errf(w, "%q is not one of networkd's settings here (%s)", k, strings.Join(keys, ", "))
			case !spec.RoutingValue.MatchString(e[k]):
				l.errf(w+"."+k, "%q: letters, digits and _ . : / , + -, at most 64", e[k])
			}
		}
	}
}

// reason checks a host entry's reason: tune prints it beside what differs.
func (l *linter) reason(at, reason string) {
	switch r := strings.TrimSpace(reason); {
	case r == "":
		l.errf(at, "is required: tune prints it beside what differs")
	case len(r) > 200 || strings.ContainsFunc(r, unicode.IsControl):
		l.errf(at, "one line of at most 200 bytes")
	}
}

// namespaced reports whether a kernel setting is, mostly, one a network, IPC or UTS namespace keeps
// of its own: a container's starts at the kernel's default, whatever the host holds. The TCP and
// UDP memory limits are the host's, shared by every namespace.
func namespaced(key string) bool {
	if key == "net.ipv4.tcp_mem" || key == "net.ipv4.udp_mem" {
		return false
	}
	for _, p := range []string{"net.ipv4.", "net.ipv6.", "net.unix.", "net.netfilter.", "net.mptcp.", "net.sctp.", "kernel.shm", "kernel.msg", "kernel.sem", "fs.mqueue."} {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return key == "net.core.somaxconn" || key == "kernel.hostname" || key == "kernel.domainname"
}

// kernelModules checks host.modules: each name, option and value is written into modprobe's files
// and the OS bundle's build, and each entry says why.
func (l *linter) kernelModules(w string, mods map[string]spec.KernelModule) {
	for _, name := range slices.Sorted(maps.Keys(mods)) {
		m, at := mods[name], w+"."+name
		switch {
		case !spec.ModuleName.MatchString(name):
			l.errf(at, "not a kernel module's name: lower-case letters, digits and _ (modprobe reads - as _), at most 56")
		case m.Load && m.Block:
			l.errf(at, "is both loaded and blocked")
		case m.Block && len(m.Options) > 0:
			l.errf(at+".options", "a blocked module takes no options")
		case !m.Load && !m.Block && len(m.Options) == 0:
			l.errf(at, "says nothing: load, block or options")
		}
		l.reason(at+".reason", m.Reason)
		for _, o := range slices.Sorted(maps.Keys(m.Options)) {
			switch v := m.Options[o]; {
			case !spec.ModuleOption.MatchString(o):
				l.errf(at+".options", "%q is not a module parameter's name: letters, digits and _", o)
			case !spec.ModuleValue.MatchString(v):
				l.errf(at+".options."+o, "%q: letters, digits and _ . , : / + -, no space or quote, at most 256", v)
			}
		}
	}
}

// pullAccount refuses a pull account that can hold the publisher's secret: it is baked into
// every installer.
func (l *linter) pullAccount() {
	pull, push := l.s.Registry.Pull, l.s.Registry.Push
	if pull.Name == "token" && push.Name == "token" {
		pc, perr := providers.Token(pull)
		uc, uerr := providers.Token(push)
		if perr == nil && uerr == nil && pc.TokenEnv == uc.TokenEnv {
			l.errf("registry.pull", "token_env %s is registry.push's: the pull account is baked into every installer, so it must hold a read-only secret of its own", pc.TokenEnv)
		}
	}
	if pull.Name == "docker-config" {
		pc, err := providers.DockerConfig(pull)
		if err != nil {
			return
		}
		if push.Name == "docker-config" {
			if uc, err := providers.DockerConfig(push); err == nil && filepath.Clean(pc.Config) == filepath.Clean(uc.Config) {
				l.errf("registry.pull", "is registry.push's Docker config: the pull account is baked into every installer, so it must be a read-only account of its own")
			}
		}
	}
}

func (l *linter) lockFile(path string) {
	lk, err := lock.Load(path)
	if err != nil {
		l.errf(lock.FileName, "%s", strings.TrimPrefix(err.Error(), lock.FileName+": "))
		return
	}
	if lk == nil {
		l.errf(lock.FileName, "missing: run stowage lock to pin the Stowage this project publishes with (ship and promote refuse without it)")
		return
	}
	if r, err := version.ParseSemVerRange(l.s.Stowage.Requires); err == nil {
		if ok, _ := r.Contains(lk.Stowage); !ok {
			l.errf(lock.FileName, "pins stowage %s, which stowage.requires (%s) does not accept: pin a Stowage it accepts, or widen the range", lk.Stowage, r)
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// ChannelName reports whether name may name a channel: it is a tag (the channel's pointer).
func ChannelName(name string) bool { return len(name) <= 128 && imageKeyRe.MatchString(name) }

// RegistryPrefix reports whether p is written as registry.prefix must be: host[:port][/path], lower
// case, no scheme, no trailing slash.
func RegistryPrefix(p string) bool { return !strings.Contains(p, "://") && prefixRe.MatchString(p) }
