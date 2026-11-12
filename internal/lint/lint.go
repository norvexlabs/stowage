// Package lint checks a project's stowage.yaml and the files it names, so a spec defect is found
// at ship time rather than on an operator's host.
package lint

import (
	"cmp"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"maps"
	"math"
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
	"github.com/norvexlabs/stowage/internal/gitcmd"
	"github.com/norvexlabs/stowage/internal/helm"
	"github.com/norvexlabs/stowage/internal/kairos"
	"github.com/norvexlabs/stowage/internal/lock"
	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/sbom"
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
	l.audit()
	l.images()
	l.stack()
	l.memory()
	l.composeLists()
	l.vmSize()
	l.vmCores()
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

// audit checks the release gate: a signing project (only its releases carry SBOMs), a severity,
// and an allowlist file that parses.
func (l *linter) audit() {
	a := l.s.Audit
	if a == nil {
		return
	}
	if !l.s.Signs() {
		l.errf("audit", "a project that signs no release (signing) carries no SBOM to audit: sign its releases, or remove audit")
	}
	if min, on, err := sbom.FailOn(a.FailOn); err != nil || !on || min < sbom.Negligible {
		l.errf("audit.fail_on", "%q: one of negligible, low, medium, high, critical", a.FailOn)
	}
	if a.Allow == "" || !l.exists("audit.allow", a.Allow, false) {
		return
	}
	doc, err := os.ReadFile(spec.Path(l.root, a.Allow))
	var allow []sbom.Allowance
	if err == nil {
		allow, err = sbom.ParseAllowances(doc, l.now)
	}
	if err != nil {
		l.errf("audit.allow", "%s: %v", a.Allow, err)
		return
	}
	// The artifacts a release can carry: the project's images, the OS bundle with the kairos
	// shape, and what Stowage supplies. A disk's findings are its bundle's (os).
	known := slices.Sorted(maps.Keys(l.s.Images))
	if l.s.Shapes.Kairos != nil {
		known = append(known, "os")
	}
	known = append(known, sbom.StowageSubjects...)
	for _, e := range allow {
		switch {
		case e.Artifact != "*" && !slices.Contains(known, e.Artifact):
			l.errf("audit.allow", "%s: allowance %s: artifact %q is no artifact of the release: one of %s, or \"*\"", a.Allow, e, e.Artifact, strings.Join(known, ", "))
		case !e.Holds(l.now):
			l.warnf("audit.allow", "%s: allowance %s expired on %s: the finding counts again; renew it, fix the finding, or remove it", a.Allow, e, e.Expires)
		case !e.Holds(l.now.Add(sbom.ExpiryNotice)):
			l.warnf("audit.allow", "%s: allowance %s expires on %s, within %d days: renew it or fix the finding before then", a.Allow, e, e.Expires, int(sbom.ExpiryNotice.Hours()/24))
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
			if img.Context != "" || img.Dockerfile != "" || img.Target != "" || len(img.Args) > 0 || len(img.AddHosts) > 0 || img.Network != "" || len(img.Contexts) > 0 || len(img.Inputs) > 0 || len(img.Untracked) > 0 {
				l.errf(where, "ref and build fields are exclusive")
			}
			if !pinnedRe.MatchString(img.Ref) {
				l.errf(where+".ref", "%q must be pinned by digest (<ref>@sha256:<64 hex>)", img.Ref)
			}
			continue
		}
		ctxOK := l.exists(where+".context", img.Context, true)
		// The build context is resolved by BuildKit, a link at any of its components followed,
		// while the inputs digest follows none: the same refusal as a named context's.
		if ctxOK {
			if _, _, err := spec.ContextDir(l.root, img.Context); err != nil {
				l.errf(where+".context", "%v", err)
				ctxOK = false
			}
		}
		if ctxOK && l.exists(where+".dockerfile", img.Dockerfile, false) {
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
			stages := l.baseImages(where+".dockerfile", img.Dockerfile, img.Contexts)
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
		if ctxOK {
			l.untracked(where, img)
		}
		// A named context is a second build context: a directory of the project, sent less its
		// own .dockerignore, which must keep its .git out as the main context's must.
		for _, cn := range slices.Sorted(maps.Keys(img.Contexts)) {
			cw := where + ".contexts." + cn
			if !imageKeyRe.MatchString(cn) {
				l.errf(where+".contexts", "%q is not a build context name (lowercase letters and digits, separated by single '.', '_' or '-')", cn)
			}
			// These two collide with BuildKit's own names: `scratch` is the empty base, `context`
			// the main build context; which a step then reads depends on how it names it.
			if cn == "scratch" || cn == "context" {
				l.errf(where+".contexts", "%q collides with a name BuildKit gives its own (scratch: the empty base; context: the main build context): name the context otherwise", cn)
			}
			_, dir, err := spec.ContextDir(l.root, img.Contexts[cn])
			if err != nil {
				l.errf(cw, "%v", err)
				continue
			}
			if ig, err := digest.ContextIgnore(dir); err != nil {
				l.errf(cw, "%v", err)
			} else if g, err := digest.GitInContext(ig); err != nil {
				l.errf(cw, "%v", err)
			} else if g != "" {
				rel, _ := filepath.Rel(l.root, g)
				l.errf(cw, "%s: %v", rel, digest.ErrGitInContext)
			}
		}
	}
}

// untracked checks an image's untracked files (images.<key>.untracked): each a file of the
// project, through no symbolic link, that the build is sent — in its context or a named one, and
// not kept out by that one's ignore file — named once, and one git does not record (git's
// own file the commit carries; checked when the project is in a git working tree).
func (l *linter) untracked(where string, img spec.Image) {
	seen := map[string]bool{}
	for i, u := range img.Untracked {
		w := fmt.Sprintf("%s.untracked[%d].path", where, i)
		clean, abs, _, err := spec.UntrackedFile(l.root, u.Path)
		if err != nil {
			l.errf(w, "%v", err)
			continue
		}
		if seen[clean] {
			l.errf(w, "%s is named twice", clean)
			continue
		}
		seen[clean] = true
		// The context that sends it: the main one, else a named one holding it.
		var ig *digest.Ignore
		in := func(dir string) bool {
			rel, err := filepath.Rel(spec.Path(l.root, dir), abs)
			return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
		}
		switch {
		case in(img.Context):
			ig, err = digest.DockerIgnore(spec.Path(l.root, img.Context), spec.Path(l.root, img.Dockerfile))
		default:
			found, bad := false, false
			for _, cn := range slices.Sorted(maps.Keys(img.Contexts)) {
				if !in(img.Contexts[cn]) {
					continue
				}
				found = true
				_, dir, cerr := spec.ContextDir(l.root, img.Contexts[cn])
				if bad = cerr != nil; !bad { // a context that is not one is said of it
					ig, err = digest.ContextIgnore(dir)
				}
				break
			}
			if !found {
				l.errf(w, "%s is in neither the build's context (%s) nor a named one: the build is not sent it", clean, img.Context)
			}
			if !found || bad {
				continue
			}
		}
		if err != nil {
			l.errf(w, "%v", err)
			continue
		}
		if out, err := ig.Excluded(abs); err != nil {
			l.errf(w, "%v", err)
		} else if out {
			l.errf(w, "%s is kept out of the build by its context's .dockerignore: the build is not sent it", clean)
		}
		if tracked, err := gitTracks(l.root, clean); err == nil && tracked {
			l.errf(w, "git records %s: an untracked file is one the commit does not carry (git rm --cached it and ignore it), or drop it from untracked", clean)
		}
	}
}

// gitTracks reports whether git records the path rel of the working tree root is in; an error
// when root is not in a git working tree.
func gitTracks(root, rel string) (bool, error) {
	out, err := gitcmd.Run(root, "ls-files", "--", rel)
	return strings.TrimSpace(out) != "", err
}

// baseImages requires every stage of a built image to start from an image pinned by digest (or
// from an earlier stage, or scratch). The Dockerfile's bytes are part of the image's inputs
// digest, so a pinned base is how a base-image update reaches the digest — and so the release.
// A stage may also start from one of the image's named contexts (contexts), whose whole tree is an
// input already; a context named like a stage replaces that stage, warned.
// It returns the Dockerfile's stage names (nil when unreadable).
func (l *linter) baseImages(where, rel string, contexts map[string]string) map[string]bool {
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
		name, isName := dockerfile.ContextName(f.Image)
		_, named := contexts[name]
		// A stage a named context replaces is never built, so its base is never pulled.
		_, replaced := contexts[f.Stage]
		switch {
		case stages[strings.ToLower(f.Image)], f.Image == "scratch", pinnedRe.MatchString(f.Image), isName && named, replaced:
		case strings.Contains(f.Image, "$"):
			l.warnf(at, "FROM %s is set by a build argument: its pinning cannot be checked", f.Image)
		default:
			l.errf(at, "FROM %s must be pinned by digest (<image>:<tag>@sha256:<64 hex>): an unpinned base changes without changing the image's inputs digest, so a rebuild would never happen", f.Image)
		}
		if f.Stage != "" {
			stages[f.Stage] = true
		}
	}
	for _, cn := range slices.Sorted(maps.Keys(contexts)) {
		if stages[cn] {
			l.warnf(where, "%s: the named context %s replaces the stage of that name: every read of the stage reads the directory %s instead", rel, cn, contexts[cn])
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
	l.longRunning(c, file)
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
		if caps := capsBeyondDefaults(svc.CapAdd); caps != "" {
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
// minimum and maximum one the service can be given, and on the least VM each share's limit and
// the floors together (shareOnLeastVM); the rest serviceHost.
func (l *linter) memory() {
	if len(l.s.Services) == 0 {
		if l.leftEmpty("services") {
			l.errf("services", "names no service: give one what it asks of the host, or remove services")
		}
		return
	}
	total, shares, floors := 0, false, spec.Bytes(0)
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
		if share.Min != 0 {
			l.shareMin(at+".memory", name, *share)
		}
		if pct := share.Percent; pct < 1 || pct > spec.MaxMemoryShares {
			l.errf(at+".memory", "%d: a percent of the host's RAM, from 1 to %d", pct, spec.MaxMemoryShares)
			continue
		}
		total += share.Percent
		// Saturated, never wrapped: a size of petabytes stays over every VM.
		if f := l.shareOnLeastVM(at+".memory", name, *share); f > math.MaxInt64-floors {
			floors = math.MaxInt64
		} else {
			floors += f
		}
	}
	l.containerJoins()
	if total > spec.MaxMemoryShares {
		l.errf("services", "the memory shares total %d %%: at most %d, the rest is the OS's, the engine's and Stowage's", total, spec.MaxMemoryShares)
	}
	if least := l.leastVMRAM(); least > 0 && floors > least/100*spec.MaxMemoryShares {
		l.warnf("services", "the memory shares' floors (each the largest of its min, its compose limit or reservation and %s, for each container) total %s, over the %s the least VM (shapes.kairos.size.min.memory: %s, %s of it shown by its kernel) leaves the shares (%d %%): its services may be OOM-killed under load", spec.MinMemLimit, floors&^(1<<20-1), (least/100*spec.MaxMemoryShares)&^(1<<20-1), l.s.Shapes.Kairos.Size.Min.Memory, least&^(1<<20-1), spec.MaxMemoryShares)
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
	floor, n, each := l.shareContainers(name)
	// A maximum is split among the service's containers: each part is checked.
	part := share.Max / spec.Bytes(n)
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
	shown := l.leastVMRAM()
	// The percent's limit there, its compose floor included, never its min: the min is not the percent.
	if least := (spec.Share{Percent: share.Percent, Max: share.Max}).Limit(uint64(shown), n, floor); least >= part&^(1<<20-1) {
		l.warnf(at, "%s: %d %% of the least VM's %s (%s of it shown by its kernel) is already %s, so the limit is always the maximum: give %s a %s limit in %s instead", share.Max, share.Percent, k.Size.Min.Memory, shown&^(1<<20-1), least, each, part&^(1<<20-1), l.s.Stack.Compose)
	}
}

// shareMin checks a share's minimum: at least spec.MinMemLimit for each of the service's
// containers, and not over the share's maximum, which it would always win over; one the compose
// file's own limit or reservation always wins over is warned.
func (l *linter) shareMin(at, name string, share spec.Share) {
	at += ".min"
	floor, n, each := l.shareContainers(name)
	switch part := share.Min / spec.Bytes(n); {
	case part < spec.MinMemLimit:
		l.errf(at, "%s: %s for %s, under %s, the least limit a share gives", share.Min, part, each, spec.MinMemLimit)
	case share.Max != 0 && share.Min > share.Max:
		l.errf(at, "%s: over the share's max %s, which it would always win over: give a min up to the max", share.Min, share.Max)
	case (spec.Share{Min: share.Min}).Limit(0, n, 0) <= floor:
		l.warnf(at, "%s: %s for %s, at most the %s limit or reservation %s gives it itself, which always wins: the min never changes its limit", share.Min, part, each, floor, l.s.Stack.Compose)
	}
}

// leastVMRAM is the RAM the least VM's kernel shows (shapes.kairos.size.min.memory, of which it
// shows at least vmRAMShown), or 0 without one.
func (l *linter) leastVMRAM() spec.Bytes {
	k := l.s.Shapes.Kairos
	if k == nil || k.Size == nil || k.Size.Min == nil {
		return 0
	}
	// Divided first: a size of petabytes times a percent would overflow.
	return k.Size.Min.Memory / 100 * vmRAMShown
}

// lowShare is the limit under which a share without a min is warned on the least VM: a server and
// the command its healthcheck runs outgrow less.
const lowShare spec.Bytes = 128 << 20

// shareOnLeastVM computes the share's limit on the least VM, as the install does
// (spec.Share.Limit), and warns when it comes out under lowShare for a share that states no min
// (nor a max under lowShare, a limit the project chose); a least VM bounds every VM whatever other
// shapes the project has, so it is checked beside a docker shape too;
// it returns the service's floor (the largest of its min, its compose limit or reservation and
// spec.MinMemLimit, for each container), which the floors' total is checked with. Without a least
// VM, it checks nothing and returns 0.
func (l *linter) shareOnLeastVM(at, name string, share spec.Share) spec.Bytes {
	least := l.leastVMRAM()
	if least == 0 {
		return 0
	}
	floor, n, who := l.shareContainers(name)
	keepMax := ""
	if share.Max != 0 {
		keepMax = ", max: " + share.Max.String()
	}
	// A share stating no min, unless its own max caps it under lowShare: that limit is the project's.
	if limit := share.Limit(uint64(least), n, floor); share.Min == 0 && (share.Max == 0 || share.Max/spec.Bytes(n) >= lowShare) && limit < lowShare {
		l.warnf(at, "%d %% of the least VM's %s (%s of it shown by its kernel) is a %s limit for %s, under %s: state the least it needs as the share's min (memory: { percent: %d, min: <size>%s })", share.Percent, l.s.Shapes.Kairos.Size.Min.Memory, least&^(1<<20-1), limit, who, lowShare, share.Percent, keepMax)
	}
	each := spec.Share{Min: share.Min}.Limit(0, n, floor)
	if each > math.MaxInt64/spec.Bytes(n) {
		return math.MaxInt64
	}
	return each * spec.Bytes(n)
}

// shareContainers is what a share is split with: the largest memory the compose file gives each
// of the service's containers itself, their number, and the words naming them.
func (l *linter) shareContainers(name string) (floor spec.Bytes, n int, who string) {
	n, who = 1, name
	if l.compose != nil {
		floor, n = l.compose.Services[name].MemFloor()
	}
	if n > 1 {
		who = fmt.Sprintf("each of %s's %d containers", name, n)
	}
	return floor, n, who
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

// vmCores checks the whole cores the services ask (services.<name>.cpus) against the VM's size:
// more than shapes.kairos.size.min.cpus less the core of CPU 0 is an error (the least VM's start
// fails), more than recommended's a warning. A vCPU counts as a core: a VM whose CPUs are SMT
// threads has fewer.
func (l *linter) vmCores() {
	k := l.s.Shapes.Kairos
	if k == nil || k.Size == nil {
		return
	}
	asked := 0
	var names []string
	for _, name := range slices.Sorted(maps.Keys(l.s.Services)) {
		if n := l.s.Services[name].CPUs; n >= 1 && n <= MaxServiceCPUs {
			asked += n
			names = append(names, name)
		}
	}
	if asked == 0 {
		return
	}
	say := func(m *spec.Machine, key string, f func(at, format string, args ...any)) bool {
		if m == nil || m.CPUs < 1 || asked <= m.CPUs-1 {
			return false
		}
		f("shapes.kairos.size."+key+".cpus", "%d: services.*.cpus (%s) ask %d whole cores, and a VM of %d CPUs has %d beside the one of CPU 0, which stays the host's: its start fails", m.CPUs, strings.Join(names, ", "), asked, m.CPUs, m.CPUs-1)
		return true
	}
	if !say(k.Size.Min, "min", l.errf) {
		say(k.Size.Recommended, "recommended", l.warnf)
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
	for _, h := range l.s.Handoffs {
		if h.Run.Image == key || (h.Check != nil && h.Check.Image == key) {
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
	// A front may serve L4 routes alone: their TLS is its, and their domains its sites.
	l4TLS := slices.ContainsFunc(l.s.L4.Routes, spec.L4Route.Terminated)
	if len(f.Routes) == 0 && !l4TLS {
		l.errf("front.routes", "at least one route is required (or an l4 route with tls, the front terminating its TLS)")
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
			l.hostNetGuard(w+".upstream", r, m[1], m[2])
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
		// The front writes its CA there as the volume's uid when it has one: never root.
		if uid := l.s.Data.Volumes[p.Volume].UID; uid != nil && *uid == 0 {
			l.errf("front.publish_ca.volume", "%s's uid is 0 (data.volumes.%s.uid): the front writes its CA there as that volume's user, and never runs as root; give it its services' non-root uid, or none", p.Volume, p.Volume)
		}
	}
	switch {
	case f.Portal == "" && len(f.Routes) == 0 && l4TLS:
		// The install ends naming the L4 routes, as a project without a front does.
	case !names[f.Portal]:
		l.errf("front.portal", "%q names no route: the installer ends by printing the portal URL (a front without routes, serving l4 routes with tls alone, leaves it out)", f.Portal)
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

// alpnRe is an ALPN protocol ID as l4.routes[].tls.alpn takes it: letters, digits and . _ / + -, a
// word the Caddyfile reads as it is (RFC 7301 allows any bytes; braces, quotes, # and $ mean
// something there).
var alpnRe = regexp.MustCompile(`^[A-Za-z0-9._/+-]{1,255}$`)

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
			svc, _, _ := strings.Cut(r.Upstream, ":")
			l.l4Allow(w+".allow_input", k, svc, r.TLS != nil)
		}
		if k := r.BindInput; k != "" {
			l.l4Bind(w+".bind_input", k)
		}
		if r.TLS != nil {
			l.l4TLS(w+".tls", r)
		}
	}
}

// l4TLS checks an L4 route the front terminates TLS on: a TCP route, a front with a TLS mode to
// give it a certificate, and a domain input.
func (l *linter) l4TLS(where string, r spec.L4Route) {
	f := l.s.Front
	if r.Protocol == "udp" {
		l.errf(where, "the front terminates TLS over TCP alone: a UDP route passes through as it is (DTLS is the upstream's)")
	}
	seen := map[string]bool{}
	for i, p := range r.TLS.ALPN {
		w := fmt.Sprintf("%s.alpn[%d]", where, i)
		switch {
		case !alpnRe.MatchString(p):
			l.errf(w, "%q is not an ALPN protocol: 1 to 255 letters, digits, '.', '_', '/', '+' or '-' (like http/1.1 or postgresql)", p)
		case p == spec.NoALPNMark:
			l.errf(w, "%q is Stowage's own: leave alpn out for no protocol", p)
		case seen[p]:
			l.errf(w, "%q is listed twice", p)
		}
		seen[p] = true
	}
	if !f.Declared() || !slices.ContainsFunc(f.TLSModes, func(m string) bool { return m != "none" }) {
		l.errf(where, "the front terminates TLS with the certificate of its TLS mode, and %s offers none: give front.tls_modes a TLS mode, or leave tls out (the upstream's own TLS passes through)", l.s.Name)
	}
	if !providers.EnvName.MatchString(r.TLS.DomainInput) {
		l.errf(where+".domain_input", "%q is not an environment variable name", r.TLS.DomainInput)
	} else if in, ok := l.s.Input(r.TLS.DomainInput); ok {
		// Declared, it must be asked wherever the front serves the route, as an undeclared one
		// is: never empty, never left unasked.
		leftOut := slices.ContainsFunc([]string{"docker", "kairos"}, func(sh string) bool {
			return slices.Contains(l.s.Shapes.Names(), sh) && len(in.Shapes) > 0 && !slices.Contains(in.Shapes, sh)
		})
		switch {
		case in.Secret:
			l.errf(where+".domain_input", "%s is secret: a domain is in the certificate, which every client reads", r.TLS.DomainInput)
		case in.Optional || in.Advanced || in.When == "loopback" || leftOut:
			l.errf(where+".domain_input", "%s must be asked on every install the front terminates the route's TLS on: neither optional nor advanced, nor asked in loopback mode alone, nor left out of a docker or kairos shape — or leave it undeclared, and the install asks it as a route's domain", r.TLS.DomainInput)
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

// hostNetGuard warns of a front route whose guard (basic_auth, client_auth, forward_auth,
// allow_input) holds only through the front while its upstream runs on the host's network
// (host_network, a compose network_mode: host, or with a kairos shape an L4 route's upstream, on
// the VM): every container of the stack
// reaches that port directly, at the host's gateway, past the front.
func (l *linter) hostNetGuard(at string, r spec.Route, service, port string) {
	var guards []string
	if r.BasicAuth != nil {
		guards = append(guards, "basic_auth")
	}
	if r.ClientAuth {
		guards = append(guards, "client_auth")
	}
	if r.ForwardAuth != nil {
		guards = append(guards, "forward_auth")
	}
	if r.AllowInput != "" {
		guards = append(guards, "allow_input")
	}
	if len(guards) == 0 {
		return
	}
	always, onVM := l.hostNetworkServices()
	where := "the host's network"
	switch {
	case slices.Contains(always, service):
	case slices.Contains(onVM, service):
		where = "the VM's network (an L4 route's upstream)"
	default:
		return
	}
	l.warnf(at, "%s runs on %s, where every container of the stack reaches its port %s directly, at the host's gateway, past the route's %s: %s checks who calls it itself", service, where, port, strings.Join(guards, ", "), service)
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
	on := "on the VM the service runs"
	if host {
		on = "the service runs"
	}
	switch {
	case r.TLS != nil && port == r.Port:
		l.errf(where+".upstream", "%s: %s on the host's network, where the front holds the route's port %d to terminate TLS: the service listens on another, which the front forwards the plaintext to", r.Upstream, on, r.Port)
	case r.TLS == nil && port != r.Port:
		l.errf(where+".upstream", "%s: %s on the host's network, where nothing maps one port to another: it listens on the route's port, %d", r.Upstream, on, r.Port)
	}
	if r.TLS != nil && (r.AllowInput != "" || r.TLS.ClientAuth) {
		var guards []string
		if r.AllowInput != "" {
			guards = append(guards, "allow_input")
		}
		if r.TLS.ClientAuth {
			guards = append(guards, "tls.client_auth")
		}
		net := "the VM's network (an L4 route's upstream)"
		if host {
			net = "the host's network"
		}
		l.warnf(where+".upstream", "%s runs on %s, where every container of the stack reaches its port %d directly, at the host's gateway, past the route's %s: %s checks who calls it itself", name, net, port, strings.Join(guards, " and "), name)
	}
	if r.TLS != nil && host && l.s.Shapes.Docker != nil {
		l.warnf(where+".upstream", "%s runs on the host's network (services.%s.host_network), where on Docker its plaintext port %d is open to whoever reaches the host, past the route's TLS (the host's firewall is its operator's): have it listen on the address the front reaches it at alone", name, name, port)
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

// l4Allow checks an L4 route's allowlist input: the VM's firewall enforces it, and a Docker host's
// once its operator consents (tune --apply), so it is asked on those of the docker and kairos
// shapes the project has and on no other (a cluster's firewall is its own), in every mode, and an
// empty one refuses every source; with neither shape nothing enforces it (a warning), nor on
// Docker for an upstream on the host's network, which Docker translates no port to (a warning).
// On a route the front terminates TLS on (terminated), the front enforces it on those shapes.
func (l *linter) l4Allow(where, key, upstream string, terminated bool) {
	in, ok := l.s.Input(key)
	if !ok {
		l.errf(where, "%s is not an input", key)
		return
	}
	// The shapes whose firewall enforces it: the VM's, and a Docker host's once its operator
	// consented (tune --apply).
	var enforcing []string
	for _, sh := range l.s.Shapes.Names() {
		if sh == "docker" || sh == "kairos" {
			enforcing = append(enforcing, sh)
		}
	}
	asked := func(sh string) bool { return len(in.Shapes) == 0 || slices.Contains(in.Shapes, sh) }
	switch {
	case len(enforcing) == 0:
		l.warnf(where, "only the VM's firewall and a Docker host's (tune --apply) enforce it, and the project has neither a docker nor a kairos shape: the route is open to every source")
	case slices.ContainsFunc(enforcing, func(sh string) bool { return !asked(sh) }) || l.s.Shapes.Kubernetes != nil && asked("kubernetes"):
		l.errf(where, "%s must be asked on the %s shapes alone (shapes: [%s]): their firewalls enforce it, a cluster's is its own", key, strings.Join(enforcing, " and "), strings.Join(enforcing, ", "))
	case in.Secret:
		l.errf(where, "%s is secret: the firewall's rules show it", key)
	case in.Advanced:
		l.errf(where, "%s is advanced, never asked: the route would refuse every source until it is set", key)
	case in.When != "":
		l.errf(where, "%s is asked only when %s: the firewall reads it in every mode", key, in.When)
	}
	// The front filters a route it terminates TLS on itself, whatever network its upstream is on.
	if always, _ := l.hostNetworkServices(); !terminated && l.s.Shapes.Docker != nil && slices.Contains(always, upstream) {
		l.warnf(where, "on the docker shape %s runs on the host's network, whose input chain is the Docker host's operator's: the route is open to every source there", upstream)
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
			if s.Settable {
				l.errf(w+".settable", "%s rotates: `secrets rotate` changes it, in passes every service follows; keep one of rotation and settable", s.Key)
			}
		}
		if s.Settable {
			l.settable(w, s.Key)
		}
		if s.AsFile && s.Rotation {
			l.errf(w+".as_file", "%s rotates: its passes give the services %s_NEXT and %s_PREVIOUS to read beside it, as variables; keep one of rotation and as_file", s.Key, s.Key, s.Key)
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
		switch {
		case !in.AsFile:
		case !in.Secret:
			l.errf(w+".as_file", "%s is no secret input: as_file keeps a secret out of the services' environment (a file the operator gives is type: file)", in.Key)
		case in.File():
			l.errf(w+".as_file", "%s is a file input already: a service mounts ${%s}", in.Key, spec.FileVar(in.Key))
		}
		switch in.Type {
		case "":
		case spec.InputFile, spec.InputPEM:
			if in.Default != "" {
				l.errf(w+".type", "a file input is given as a file: no default")
			}
			if in.Advanced {
				l.errf(w+".type", "a file input is mounted by a service, so asked: not advanced")
			}
		default:
			l.errf(w+".type", "%q: expected %s or %s", in.Type, spec.InputFile, spec.InputPEM)
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
	for _, d := range l.s.RouteDomains() {
		if _, ok := keys[d.Input]; !ok {
			keys[d.Input] = "a route domain"
		}
	}
	l.composeVars(keys)
	l.inputsConsumed()
	l.extensions()
	l.handoffs()
	l.fileInputsOnlyMounted()
}

// fileInputsOnlyMounted refuses a file input read as text: what is recorded for it is encoded. A
// provision hook's env may name one: the hook gets its file, not that value.
func (l *linter) fileInputsOnlyMounted() {
	use := func(where, key string) {
		if i, ok := l.s.Input(key); ok && i.File() {
			l.errf(where, "%s is a file input, which a service mounts (${%s}): it holds no text this can read", key, spec.FileVar(key))
		}
	}
	for n, r := range l.s.Front.Routes {
		w := fmt.Sprintf("front.routes[%d]", n)
		use(w+".domain_input", r.DomainInput)
		use(w+".allow_input", r.AllowInput)
		if a := r.ForwardAuth; a != nil {
			use(w+".forward_auth.address_input", a.AddressInput)
			use(w+".forward_auth.uri_input", a.URIInput)
		}
		if a := r.BasicAuth; a != nil {
			use(w+".basic_auth.secret", a.Secret)
		}
	}
	for n, r := range l.s.L4.Routes {
		w := fmt.Sprintf("l4.routes[%d]", n)
		use(w+".address_input", r.AddressInput)
		use(w+".allow_input", r.AllowInput)
		use(w+".bind_input", r.BindInput)
		if r.TLS != nil {
			use(w+".tls.domain_input", r.TLS.DomainInput)
		}
	}
	for _, name := range sortedKeys(l.s.Services) {
		svc := l.s.Services[name]
		w := "services." + name
		use(w+".address_input", svc.AddressInput)
		for _, k := range svc.Sets {
			use(w+".sets", k)
		}
		use(w+".nftables.input", svc.Nftables.Input)
	}
	for n, e := range l.s.Extensions {
		w := fmt.Sprintf("extensions[%d]", n)
		use(w+".input", e.Input)
		use(w+".key_input", e.KeyInput)
	}
	for n, in := range l.s.Inputs {
		for _, other := range l.s.Inputs {
			if other.File() && strings.Contains(in.Default, "${"+other.Key+"}") {
				use(fmt.Sprintf("inputs[%d].default", n), other.Key)
			}
		}
	}
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

// settable refuses a settable secret the database's service reads in its environment: the
// database keeps the value it was first made with, so a new one in .env breaks its readers.
func (l *linter) settable(where, key string) {
	if l.compose == nil || !l.s.HasDatabase() {
		return
	}
	db := l.s.DB().Service
	if envReads(l.compose.Services[db].Environment, key) {
		l.errf(where+".settable", "%s is read by the database's service %s, which keeps the value it was first made with: a secret the data is made with is never settable", key, db)
	}
}

// envReads reports whether a compose environment (a map, or a list of KEY[=VALUE]) reads key:
// passes it through by name (a list entry KEY, a map key KEY without a value), or interpolates it
// in a value (${KEY}, ${KEY…} with a modifier, $KEY; compose's $$ is a literal dollar).
func envReads(env any, key string) bool {
	re := regexp.MustCompile(`\$(\{` + regexp.QuoteMeta(key) + `[}:?+-]|` + regexp.QuoteMeta(key) + `\b)`)
	reads := func(v string) bool { return re.MatchString(strings.ReplaceAll(v, "$$", "")) }
	switch e := env.(type) {
	case map[string]any:
		for k, v := range e {
			if k == key && v == nil || v != nil && reads(fmt.Sprint(v)) {
				return true
			}
		}
	case []any:
		for _, v := range e {
			s := fmt.Sprint(v)
			if name, val, _ := strings.Cut(s, "="); name == key && !strings.Contains(s, "=") || reads(val) {
				return true
			}
		}
	}
	return false
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
		if key, ok := strings.CutPrefix(v.Name, spec.FileVarPrefix); ok {
			if i, found := l.s.Input(key); !found || !i.File() {
				l.errf(w, "%s names no file input (inputs[].type: file or pem)", v.Name)
			}
			continue
		}
		if key, ok := strings.CutPrefix(v.Name, spec.SecretFileVarPrefix); ok {
			if !l.s.AsFile(key) {
				l.errf(w, "%s names no secret delivered as a file (secrets[].as_file, or a secret input's as_file)", v.Name)
			}
			continue
		}
		if v.Name == "STOWAGE_TLS_MODE" && !l.s.HasFront() {
			l.errf(w, "%s: the project has no front, so no TLS mode is recorded", v.Name)
			continue
		}
		if strings.HasPrefix(v.Name, "STOWAGE_") {
			if _, ok := ProvidedVars[v.Name]; !ok {
				l.errf(w, "%s is not a variable Stowage provides (%s, STOWAGE_IMAGE_<KEY>, STOWAGE_URL_<ROUTE>, STOWAGE_L4_<ROUTE>, STOWAGE_INPUT_FILE_<KEY>, STOWAGE_SECRET_FILE_<KEY>)", v.Name, strings.Join(sortedKeys(ProvidedVars), ", "))
			}
			continue
		}
		if i, ok := l.s.Input(v.Name); ok && i.File() {
			l.errf(w, "${%s} is a file input: what is recorded for it is no text to read; mount ${%s}", v.Name, spec.FileVar(v.Name))
			continue
		}
		if l.s.AsFile(v.Name) {
			l.errf(w, "${%s} is delivered as a file (as_file: true), never as a variable: mount ${%s} and read the file", v.Name, spec.SecretFileVar(v.Name))
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

// readInputs reports whether something reads an input: a compose service, a route of the front,
// the provision hook's env or a handoff step's; one asked on Kubernetes only is the chart's, taken
// as read.
func (l *linter) readInputs() func(spec.Input) bool {
	inCompose := map[string]bool{}
	for _, v := range l.compose.Vars {
		name := strings.TrimPrefix(v.Name, spec.FileVarPrefix)
		inCompose[strings.TrimPrefix(name, spec.SecretFileVarPrefix)] = true
	}
	// The provision hook reads the keys its env lists.
	if h, err := providers.DecodeProvision(l.s.Short(), l.s.Hooks.Provision, l.s.Images); err == nil {
		for _, k := range h.Keys() {
			inCompose[k] = true
		}
	}
	// A handoff's steps read the keys their env lists.
	for _, h := range l.s.Handoffs {
		for _, st := range []*spec.HandoffStep{&h.Run, h.Check} {
			if st != nil {
				for _, k := range st.Env {
					inCompose[k] = true
				}
			}
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
	// The installer reads the L4 routes' inputs, the services' addresses of their own, the sets'
	// inputs and those picking a table.
	for _, r := range l.s.L4.Routes {
		inCompose[r.AddressInput], inCompose[r.AllowInput], inCompose[r.BindInput] = true, true, true
		if r.TLS != nil {
			inCompose[r.TLS.DomainInput] = true
		}
	}
	for _, svc := range l.s.Services {
		inCompose[svc.AddressInput] = true
		for _, k := range svc.Sets {
			inCompose[k] = true
		}
		if k := svc.Nftables.Input; k != "" {
			inCompose[k] = true
		}
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
	switch {
	case l.s.Backup == nil:
	case !l.s.HasDatabase() && (l.s.Backup.Name == providers.VMBackup || l.s.Backup.Name == "command"):
		l.errf("backup", "a %s backup backs a database up, and the project declares none (%s backs up the volumes of a project without one)", l.s.Backup.Name, providers.VolumesBackup)
	case l.s.HasDatabase() && l.s.Backup.Name == providers.VolumesBackup:
		l.errf("backup", "%s leaves the database out, and the project declares one: %s backs it up with its volumes", providers.VolumesBackup, providers.VMBackup)
	}
	if err := providers.Backup(l.s.Backup, l.s.Images); err != nil {
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
	for _, d := range l.s.RouteDomains() {
		if known[d.Input] == "" {
			known[d.Input] = "a route domain"
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
	// A file input's key there is its recorded form: a chart reads it from the files Secret.
	k8sInput := slices.ContainsFunc(l.s.Inputs, func(i spec.Input) bool { return !i.File() && i.OnKubernetes() })
	if (len(l.s.Secrets) > 0 || k8sInput) && !reads("secret") {
		l.warnf("shapes.kubernetes.chart", "%s: no template reads .Values.%s.secret: the release's secrets and inputs do not reach the chart", rel, helm.ValuesKey)
	}
	k8sFile := slices.ContainsFunc(l.s.Inputs, func(i spec.Input) bool { return i.File() && i.OnKubernetes() })
	if k8sFile && !reads("files") {
		l.warnf("shapes.kubernetes.chart", "%s: no template reads .Values.%s.files: the file inputs on Kubernetes do not reach the chart", rel, helm.ValuesKey)
	}
	k8sAsFile := slices.ContainsFunc(l.s.Secrets, func(s spec.Secret) bool { return s.AsFile }) ||
		slices.ContainsFunc(l.s.Inputs, func(i spec.Input) bool { return i.Secret && i.AsFile && i.OnKubernetes() })
	if k8sAsFile && !reads("secretFiles") {
		l.warnf("shapes.kubernetes.chart", "%s: no template reads .Values.%s.secretFiles: the secrets delivered as files (as_file) do not reach the chart", rel, helm.ValuesKey)
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
		if k.Disks != "" && k.Disks != spec.DisksAlways && k.Disks != spec.DisksOptIn {
			l.errf(w+".disks", "unknown policy %q (%s, %s)", k.Disks, spec.DisksAlways, spec.DisksOptIn)
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
		case l.s.Backup == nil && !l.s.HasDatabase() && len(l.s.Data.Volumes) > 0:
			l.warnf("backup", "the VM keeps its data volumes locally and no backup is declared (%s archives them)", providers.VolumesBackup)
		case l.s.Backup == nil && !l.s.HasDatabase():
		case l.s.Backup == nil:
			l.warnf("backup", "the VM keeps its data locally and no backup is declared")
		case !providers.RunsBackups(l.s):
			l.warnf("backup", "the VM runs %s and %s backups only: it schedules no %s backup", providers.VMBackup, providers.VolumesBackup, l.s.Backup.Name)
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
	var kept []string
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
			kept = append(kept, key)
			l.arpSetting(at, h, key, c.Value)
		default:
			l.arpSetting(at, h, key, c.Value)
		}
		l.reason(at+".reason", c.Reason)
	}
	if len(kept) > 0 {
		// The host's network namespace alone is theirs: an IPC or UTS key stays a container's.
		also := ""
		if slices.ContainsFunc(kept, func(k string) bool { return strings.HasPrefix(k, "net.") }) {
			always, onVM := l.hostNetworkServices()
			var parts []string
			if len(always) > 0 {
				parts = append(parts, strings.Join(always, ", ")+" (on the host's network)")
			}
			if len(onVM) > 0 {
				parts = append(parts, strings.Join(onVM, ", ")+" (on the VM's)")
			}
			if len(parts) > 0 {
				also = ", " + strings.Join(parts, " and ") + " included, for its network settings"
			}
		}
		l.warnf("host.sysctls", "a namespace keeps its own %s: the host's value holds in the host's namespace (Docker's NAT and bridges among it%s), not in a container's own (the compose file's `sysctls` give a service its value)", strings.Join(kept, ", "), also)
	}
	if p := h.Power; p != "" && p != tune.Performance && p != tune.Platform {
		l.errf("host.power", "%q: %s or %s (the default)", p, tune.Performance, tune.Platform)
	}
	l.interfaces(h.Interfaces)
}

// arpSetting refuses, on a VM of more than one NIC role, a NIC's or default's ARP setting below
// all's (the project's own, else spec.ARPSettings'): the kernel takes the larger, so it would not
// hold.
func (l *linter) arpSetting(at string, h *spec.Host, key, value string) {
	if l.s.Shapes.Kairos == nil || h.NICRoles() < 2 {
		return
	}
	rest, ok := strings.CutPrefix(key, "net.ipv4.conf.")
	i := strings.LastIndexByte(rest, '.')
	if !ok || i <= 0 {
		return
	}
	dev, name := rest[:i], rest[i+1:]
	all, arp := spec.ARPSettings[name]
	if !arp || dev == "all" {
		return
	}
	if c, own := h.Sysctls[spec.ARPKey(name)]; own {
		v, err := strconv.Atoi(c.Value)
		if err != nil {
			return
		}
		all = v
	}
	if v, err := strconv.Atoi(value); err == nil && v < all {
		l.errf(at+".value", "%d is below the %s %d all holds on a VM of several NIC roles, and the kernel takes the larger: set %s to %d to choose it for every NIC", v, name, all, spec.ARPKey(name), v)
	}
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
				if i.Optional {
					l.errf(at+".optional", "the primary NIC, the default route's, is every machine's")
				}
			}
		case spec.InterfaceTUN:
			if i.Optional {
				l.errf(at+".optional", "a NIC's: a tun device is its service's, absent until it opens it")
			}
			if i.Primary {
				l.errf(at+".primary", "a tun device a service opens is not the default route's")
			}
			l.tunDevice(at+".device", role, i.Device, ifs)
		default:
			l.errf(at+".kind", "%q: %s (the default) or %s", i.Kind, spec.InterfaceNIC, spec.InterfaceTUN)
		}
		if i.Device != "" && i.Kind != spec.InterfaceTUN {
			l.errf(at+".device", "a tun role's: a NIC role's device is the one the VM names after the role, or the one --interface names on a Docker host")
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
	const consent = "only the VM's firewall is Stowage's (a kairos shape): a Docker host holds it in DOCKER-USER once its operator consents (tune --apply)"
	if f := l.s.Front.Interfaces; f != nil {
		l.roleList("front.interfaces", f, "leave it out to open the front on every interface", nil)
		if !kairos && len(f) > 0 {
			l.warnf("front.interfaces", consent)
		}
	}
	held := len(l.s.Front.Interfaces) > 0
	for i, r := range l.s.L4.Routes {
		if r.Interfaces == nil {
			continue
		}
		held = held || len(r.Interfaces) > 0
		at := fmt.Sprintf("l4.routes[%d].interfaces", i)
		l.roleList(at, r.Interfaces, "leave it out to open the route on every interface", nil)
		if !kairos && len(r.Interfaces) > 0 {
			l.warnf(at, consent)
		}
	}
	if held && l.compose != nil {
		l.heldServices()
	}
	for _, role := range slices.Sorted(maps.Keys(ifs)) {
		fw, at := ifs[role].Forward, "host.interfaces."+role+".forward"
		if fw != nil {
			l.roleList(at, fw, fmt.Sprintf("the roles the host forwards %s's traffic out of, like [uplink]", role), func(w, out string) bool {
				if out == role {
					l.errf(w, "is %s itself: the host forwards from one interface out of another", role)
				}
				return out == role
			})
		}
		if len(fw) > 0 && !kairos {
			l.warnf(at, "only the VM's firewall is Stowage's (a kairos shape): a Docker host applies it in DOCKER-USER once its operator consents (tune --apply)")
		}
	}
}

// heldServices refuses, for each service whose published ports are held to interfaces, what the
// holds would not judge (spec.UnheldNetwork, as the start refuses it): the front, with
// front.interfaces or an L4 route it terminates TLS on with interfaces, on the project's default
// network; an interfaced route's upstream Docker publishes (not on the host's network: on the VM
// every route's upstream is), with an address of its own on a NIC (services.<name>.interface), a
// network_mode, or on a compose network the predicate refuses. An external network is the start's
// to judge.
func (l *linter) heldServices() {
	held := map[string]string{} // service ("" the front): what holds it
	if len(l.s.Front.Interfaces) > 0 {
		held[""] = "front.interfaces"
	}
	docker := l.s.Shapes.Docker != nil
	for i, r := range l.s.L4.Routes {
		if len(r.Interfaces) == 0 {
			continue
		}
		at := fmt.Sprintf("l4.routes[%d].interfaces", i)
		name, _, _ := strings.Cut(r.Upstream, ":")
		switch {
		case r.Terminated():
			if _, ok := held[""]; !ok {
				held[""] = at
			}
		case docker && !l.s.Services[name].HostNetwork:
			held[name] = at
		}
	}
	nets, _ := l.compose.TopLevel["networks"].(map[string]any)
	told := map[string]bool{}
	network := func(at, who, key string) {
		n, _ := nets[key].(map[string]any)
		if ext, _ := n["external"].(bool); ext || told[key] {
			return
		}
		driver := ""
		if v, ok := n["driver"]; ok {
			driver = fmt.Sprint(v)
		}
		opts := map[string]string{}
		if o, ok := n["driver_opts"].(map[string]any); ok {
			for k, v := range o {
				opts[k] = fmt.Sprint(v)
			}
		}
		// A value from a variable is the start's to judge, as compose resolves it.
		if strings.Contains(driver, "$") || slices.ContainsFunc(slices.Collect(maps.Keys(opts)), func(k string) bool { return strings.Contains(k+opts[k], "$") }) {
			return
		}
		if why := spec.UnheldNetwork(driver, opts); why != "" {
			told[key] = true
			l.errf(l.s.Stack.Compose, "networks.%s %s, and %s is on it while %s holds its ports to interfaces: the holds judge what Docker translates and forwards to a bridge it makes itself, so they would answer past them; leave its driver and driver_opts to Docker (the neutral ones aside)", key, why, who, at)
		}
	}
	for _, svc := range slices.Sorted(maps.Keys(held)) {
		at := held[svc]
		if svc == "" {
			network(at, "the front", "default")
			continue
		}
		if i := l.s.Services[svc].Interface; i != "" {
			l.errf("services."+svc+".interface", "%s has an address of its own on %s, past what %s holds: leave one of them out", svc, i, at)
		}
		c, ok := l.compose.Services[svc]
		if !ok {
			continue
		}
		if c.NetworkMode != "" {
			l.errf(l.s.Stack.Compose, "services.%s.network_mode %s: %s holds its ports to interfaces, judged on what Docker translates to the project's networks: leave network_mode out", svc, c.NetworkMode, at)
			continue // on no network of the project's
		}
		var keys []string
		switch n := c.Networks.(type) {
		case []any:
			for _, k := range n {
				keys = append(keys, fmt.Sprint(k))
			}
		case map[string]any:
			keys = slices.Sorted(maps.Keys(n))
		case nil:
			keys = []string{"default"}
		}
		for _, k := range keys {
			network(at, svc, k)
		}
	}
}

// tunDevice checks a tun role's device (spec.ValidTunDevice), the name its service gives the
// device where the role's own cannot be it: one no other role is (the VM names its NICs by their
// roles) and no other tun role's device is.
func (l *linter) tunDevice(at, role, dev string, ifs map[string]spec.HostInterface) {
	if dev == "" {
		return
	}
	if !spec.ValidTunDevice(dev) {
		l.errf(at, "%q: a device's name of 1 to 15 letters, digits, _, . or -, starting with a letter or digit (the name its service gives it)", dev)
		return
	}
	// The host's own devices are never a service's tun: a tun or tap name is one (tun0, tap0).
	if p := slices.IndexFunc(spec.ReservedInterfacePrefixes, func(p string) bool { return p != "tun" && p != "tap" && strings.HasPrefix(dev, p) }); p >= 0 {
		l.errf(at, "%q is a name the kernel, udev or Docker gives (%s…): the name the service gives its tun device", dev, spec.ReservedInterfacePrefixes[p])
		return
	}
	for _, r := range slices.Sorted(maps.Keys(ifs)) {
		switch {
		case r == role:
		case r == dev:
			l.errf(at, "%q is the role %s's name, another interface's", dev, r)
		case ifs[r].Kind == spec.InterfaceTUN && ifs[r].Device == dev:
			l.errf(at, "%q is host.interfaces.%s.device too: one device plays one role", dev, r)
		}
	}
}

// roleList checks a list of host.interfaces roles at at: not empty (empty says what it lists),
// each role declared and given once. extra, when set, checks a declared role further: it reports
// its own error and returns true when it found one.
func (l *linter) roleList(at string, roles []string, empty string, extra func(w, role string) bool) {
	if len(roles) == 0 {
		l.errf(at, "is empty: %s", empty)
		return
	}
	for i, role := range roles {
		w := fmt.Sprintf("%s[%d]", at, i)
		var ok bool
		if l.s.Host != nil {
			_, ok = l.s.Host.Interfaces[role]
		}
		switch {
		case !ok:
			l.errf(w, "%q is not one of host.interfaces", role)
		case extra != nil && extra(w, role):
		case slices.Index(roles, role) < i:
			l.errf(w, "%s is given twice", role)
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

// longRunning checks what a long-running service tells the install, with a docker or kairos
// shape (the chart's pods have their own restart and probes): one with no restart policy is warned
// (a crash or an OOM kill leaves it down until the next start), and the services with no
// healthcheck are warned together (the install is done once they run, not once they are ready). A
// one-shot is none: a service another waits for with service_completed_successfully, or one that
// says so with restart: "no" (which the restart warning offers for a one-shot alone).
func (l *linter) longRunning(c *Compose, file string) {
	if l.s.Shapes.Docker == nil && l.s.Shapes.Kairos == nil {
		return
	}
	oneShot := map[string]bool{}
	for _, svc := range c.Services {
		deps, _ := svc.DependsOn.(map[string]any)
		for d, v := range deps {
			if on, _ := v.(map[string]any); on["condition"] == "service_completed_successfully" {
				oneShot[d] = true
			}
		}
	}
	var unhealthy []string
	for _, name := range sortedKeys(c.Services) {
		svc := c.Services[name]
		// deploy.restart_policy's condition none is restart: "no" written the other way.
		policy, _ := svc.Deploy.RestartPolicy.(map[string]any)
		if oneShot[name] || svc.Restart == "no" || policy != nil && policy["condition"] == "none" {
			continue
		}
		if svc.Restart == nil && svc.Deploy.RestartPolicy == nil {
			l.warnf(file+": services."+name, "no restart policy: a crash or an OOM kill leaves it down until the next start; give it restart: unless-stopped, or restart: \"no\" for a one-shot")
		}
		// None, or one disabled (disable: true, test: ["NONE"]); a block without a test tunes the
		// image's own, which lint does not read.
		if h := svc.Healthcheck; h == nil {
			unhealthy = append(unhealthy, name)
		} else if _, none := healthBroken(h.Test); none {
			unhealthy = append(unhealthy, name)
		} else if off, _ := composeBool(h.Disable); off {
			unhealthy = append(unhealthy, name)
		}
	}
	if len(unhealthy) > 0 {
		l.warnf(file, "%s set no healthcheck: the install (and vm create) is done once they run, not once they are ready, so give one that passes once the service serves, or has done the work it does after it starts (an image's own HEALTHCHECK counts, which lint does not read)", strings.Join(unhealthy, ", "))
	}
}
