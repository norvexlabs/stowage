package install

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"

	"github.com/norvexlabs/stowage/internal/front"
	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
)

// Options is one run of the installer.
type Options struct {
	Dir     string         // the install directory, holding compose.yaml and .env
	Ports   map[string]int // --ports: a route's loopback port
	Address string         // the loopback address ports bind to; first install only
	IPv6    *bool          // --ipv6: IPv6 on the stack's network; nil keeps the recorded choice
	// TLS is the front's TLS mode (--tls); empty decides it from the domains given, or asks.
	TLS     string
	Domains map[string]string // --domains: a route's domain, sugar for its domain input
	// Cert and Key are a byo certificate chain and its key, copied into the install.
	Cert, Key string
	// CA is the CA a byo certificate chains to (--ca), PEM, copied into the install for
	// front.publish_ca.
	CA string
	// HTTPPort and HTTPSPort are the host ports a TLS front publishes; 0 is 80 and 443.
	HTTPPort, HTTPSPort int
	Registry            string // --registry: the prefix images are pulled from (a mirror); first install only
	// RegistryCA is a PEM file of the CA the registry's certificate chains to (--registry-ca), a
	// mirror's own: the installer's reads trust it, and an install records it (RegistryCAFile).
	RegistryCA string
	Offline    bool // images are already loaded: no login, no pull
	// Preloaded: the images are the ones an OS bundle preloads, named by digest
	// (release.PreloadTag); an install on the Kairos VM shape. It implies Offline.
	Preloaded bool
	// BootIDFile is where the kernel publishes this boot's id, which a VM install's journal
	// entries follow the boot.started of; empty is /proc/sys/kernel/random/boot_id. Cos is where
	// Kairos marks the system it booted (<mode>_mode, upgrade_failure); empty is /run/cos. The
	// host daemon and the health gate read both from their Options too: they are set in one place.
	BootIDFile, Cos string
	// Channel is the channel a VM install follows for its updates, recorded at the first install.
	Channel string
	// Settings is the settings file a VM's first install takes its values from (SettingsFile when
	// empty).
	Settings string
	// LockWait is how long a run waits for the install's lifecycle lock another run holds; 0
	// refuses at once. The VM's boot waits, so an operator's short run never fails it.
	LockWait    time.Duration
	DryRun      bool // check and print what would be written, write nothing
	NoProvision bool // start the stack without running the provision hook
	Yes         bool // reset-db: wipe without asking
	// Self is the running installer, kept in the install as <short>-stow; empty keeps none.
	Self string
	// SelfExe is the running executable itself (/proc/self/exe), copied in Self's place: the file
	// at Self's path may have been replaced since the run started. Empty copies Self.
	SelfExe string
	Set     map[string]string
	Answers map[string]string
	// Secrets is a decrypted SecretsFile (--secrets) a first install takes its secrets from.
	// NewSecrets: a VM's first install generates its secrets though its settings wait for
	// exported ones (--new-secrets).
	Secrets    []byte
	NewSecrets bool
	// Prompt asks for an input nothing else gave; nil takes its default.
	Prompt Prompter

	Getenv  func(string) string
	Environ []string
	Out     io.Writer // results: URLs, the dry-run file
	Log     io.Writer // progress, docker output
	Docker  Tool
	// Tofu and SSH are OpenTofu and ssh, run by `vm create --create opentofu` and `vm destroy`.
	Tofu Tool
	SSH  Tool
	Rand io.Reader
	// Exec runs an installer as a child (upgrade --version); nil runs it on this terminal.
	Exec func(ctx context.Context, exe string, args []string) error
	// K8s is a run of `k8s`.
	K8s K8sOptions
	// PortFree replaces the listening-port probe in tests.
	PortFree func(host string, port int) bool
	// Firewall is the directory the VM's firewall includes runtime rules from, where every start
	// opens the front's ports (openPorts); empty opens none. Host runs host programs (nft): Args[0]
	// is the program.
	Firewall string
	Host     Tool
	// SourceAddr is the address this machine reaches dst from; nil asks the system's routes.
	// LookupIP resolves a host name; nil is the system's resolver.
	SourceAddr func(dst netip.Addr) (netip.Addr, error)
	LookupIP   func(host string) ([]netip.Addr, error)
}

func (o *Options) logf(format string, a ...any) { fmt.Fprintf(o.Log, format+"\n", a...) }

// cos is where Kairos marks the system it booted.
func (o *Options) cos() string { return cmp.Or(o.Cos, "/run/cos") }

// bootID is this boot's id, as the kernel publishes it; empty when it cannot be read.
func (o *Options) bootID() string { return readBootID(cmp.Or(o.BootIDFile, kernelBootID)) }

// Installer installs and manages one release on Docker.
type Installer struct {
	R *Release
	O Options
	// removeDir: this run created the install directory before writing the install, so a failure
	// before .env removes it again.
	removeDir bool
	byo       *byoCert          // --tls byo: the operator's certificate, checked
	newCert   *byoCert          // a re-run's byo certificate, written before the stack comes up again
	answered  map[string]string // the form's answers, kept when it goes back to the TLS mode
	// recorded are the values an upgrade keeps, which an added input's default may name.
	recorded map[string]string
	// again: this run is a re-run of an install.
	again bool
	// upgraded: Upgrade moved the install to this release (not a no-op, not a dry run).
	upgraded bool
	// preloadErr: the VM's preload unit failed when this run waited for it (awaitPreload).
	preloadErr error
	// goOn names the inputs review's choice to go on; empty is the install's.
	goOn string
	// remote: the install is decided from afar, for a VM (`vm create`): neither loopback nor byo
	// is offered.
	remote bool
	// restored is the SecretsFile O.Secrets gives (useSecrets); nil when none.
	restored *SecretsFile
	// givenCA: the registry's CA is --registry-ca alone, never one recorded in a directory — the
	// commands that keep no install (k8s, mirror, vm create).
	givenCA bool
	// wroteDockerCA is the Docker daemon's CA file this run wrote, which a first install that
	// fails removes again.
	wroteDockerCA string
	// madeDockerCADirs are the directories this run made for it, deepest first.
	madeDockerCADirs []string
}

// input is one value a first install records, and its check beyond the .env rule.
type input struct {
	spec.Input
	check  func(string) error
	domain bool // a route's domain: on a terminal, never taken from the environment unseen
}

const envName = ".env"

// envPrev is the .env the stack last came up with before a re-run changed it; changingFile marks
// a change whose stack has not come up yet; installingFile marks a first install whose stack has
// not come up and been provisioned yet (written before .env, removed when a run finishes it).
const (
	envPrev        = ".env.prev"
	changingFile   = ".stowage-changing"
	installingFile = ".stowage-installing"
	// restoreAwaitedFile marks an install that --secrets gave another install's provision values:
	// they belong to that install's data, so the hook waits for it to be loaded (provisionUp).
	restoreAwaitedFile = ".stowage-restore-awaited"
)

func (in *Installer) path(name string) string { return filepath.Join(in.O.Dir, name) }

// Install installs the release in O.Dir, or — when the directory already holds an install of the
// same release — finishes its setup (brings the stack up again). Nothing is written before every
// check passed.
func (in *Installer) Install(ctx context.Context) error {
	o := &in.O
	if o.Dir == "" {
		return errors.New("an install directory is required")
	}
	if err := in.R.checkFront(); err != nil {
		return err
	}
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return err
	}
	o.Dir = dir
	if _, err := os.Stat(in.path(envName)); err == nil {
		if o.Secrets != nil {
			return fmt.Errorf("%s holds an install, which keeps its secrets: --secrets is for a first install (a machine made again)", dir)
		}
		return in.rerun(ctx)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if o.Secrets != nil {
		f, err := in.readSecrets(o.Secrets)
		if err != nil {
			return err
		}
		in.useSecrets(f)
	}

	if err := in.applyDomains(); err != nil {
		return err
	}
	if _, err := in.hook(); err != nil {
		return err
	}
	ca, err := in.registryCA()
	if err != nil {
		return err
	}
	// Everything is checked and decided first; nothing is pulled or written before. The host is
	// checked before anything is asked, so a refusal never comes after the operator answered a
	// form. With nothing to ask, every given input is checked before the host. Going back from the
	// form to the TLS mode checks the mode's ports again.
	if o.Prompt == nil {
		mode, _, err := in.mode(ctx, "")
		if err != nil {
			return err
		}
		if err := in.knownInputs(mode); err != nil {
			return err
		}
		if _, err := in.inputs(ctx, in.inputList(mode), false); err != nil {
			return err
		}
	}
	if err := in.dirFree(dir); err != nil {
		return err
	}
	if err := in.checkDocker(ctx); err != nil {
		return err
	}
	var env *EnvFile
	var secrets map[string]bool
	var pub publish
	var mode string
	for {
		m, asked, err := in.mode(ctx, mode)
		if err != nil {
			return err
		}
		mode = m
		if err := in.knownInputs(mode); err != nil {
			return err
		}
		if pub, err = in.preflight(ctx, mode); err != nil {
			return err
		}
		env, secrets, err = in.decide(ctx, dir, mode, pub, asked)
		if errors.Is(err, ErrBack) {
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	// Again after the form, which may have stayed open for a while.
	if err := in.dirFree(dir); err != nil {
		return err
	}
	if err := in.tlsFree(mode); err != nil {
		return err
	}
	if err := in.portsFree(pub); err != nil {
		return err
	}
	if o.DryRun {
		for _, k := range env.Keys() {
			v, _ := env.Get(k)
			if secrets[k] && v != "" {
				v = "<redacted>"
			}
			fmt.Fprintf(o.Out, "%s=%s\n", k, v)
		}
		return nil
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		in.removeDir = true
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Everything from here runs under the lock: another run may have installed here meanwhile,
	// and its files are its own.
	unlock, err := in.lockRun(ctx, env)
	if err != nil {
		return err
	}
	defer unlock()
	if err := in.dirFree(dir); err != nil {
		return err
	}
	// Until .env is written no install owns what this run wrote: a failure removes it.
	certWritten, caWritten := false, false
	unwritten := func(err error) error {
		if in.wroteDockerCA != "" {
			_ = os.Remove(in.wroteDockerCA)
		}
		for _, d := range in.madeDockerCADirs {
			_ = os.Remove(d) // only when empty
		}
		switch {
		case in.removeDir:
			_ = os.RemoveAll(dir)
		default:
			if certWritten {
				_ = os.RemoveAll(in.path(TLSDir))
			}
			if caWritten {
				_ = os.Remove(in.path(RegistryCAFile))
			}
			_ = os.Remove(in.path(installingFile)) // written by this run, under its lock
		}
		return err
	}
	if err := in.fetchImages(ctx, env); err != nil {
		return unwritten(err)
	}
	if err := in.writeCert(); err != nil { // it removes a tls/ it created; one it did not is not this run's
		return unwritten(err)
	}
	certWritten = in.byo != nil
	if o.RegistryCA != "" {
		// A file of that name there before is not this run's to remove (it may be the CA given).
		_, err := os.Lstat(in.path(RegistryCAFile))
		caWritten = errors.Is(err, os.ErrNotExist)
		if err := writeAtomic(in.path(RegistryCAFile), bytes.NewReader(ca), 0o644, nil); err != nil {
			return unwritten(err)
		}
	}
	// .env first: once it exists the directory is this install's, and a re-run finishes it. The
	// marker before it: an install .env tells is not done until the stack came up, provisioned.
	if err := os.WriteFile(in.path(installingFile), nil, 0o600); err != nil {
		return unwritten(err)
	}
	if err := env.WriteFile(in.path(envName)); err != nil {
		return unwritten(err)
	}
	o.logf("wrote %s", in.path(envName))
	if err := in.writeRestoredProvision(); err != nil {
		in.markInstall(err)
		return err
	}
	if err := in.writeStack(env); err != nil {
		in.markInstall(err)
		return err
	}
	in.keepSelf()
	if err := in.start(ctx, env); err != nil {
		in.markInstall(err)
		return err
	}
	return nil
}

// publish is where the front listens on the host: in loopback mode each route's port on a loopback
// address, behind TLS ports "http" and "https" on every address.
type publish struct {
	addr  string
	ports map[string]int
	all   bool // probed on every address of the host, not only addr
}

// preflight checks what the mode needs — its flags, a byo certificate, every port free — and
// returns where the front listens.
func (in *Installer) preflight(ctx context.Context, mode string) (publish, error) {
	o := &in.O
	if mode == Loopback {
		if o.HTTPPort != 0 || o.HTTPSPort != 0 {
			return publish{}, errors.New("--http-port and --https-port are for TLS modes: loopback mode takes --ports")
		}
	} else if len(o.Ports) > 0 || o.Address != "" {
		return publish{}, errors.New("--ports and --address are for loopback mode: a TLS front listens on --http-port and --https-port")
	}
	in.byo = nil
	if err := in.tlsFree(mode); err != nil {
		return publish{}, err
	}
	if mode == "byo" {
		c, err := in.loadCert(ctx)
		if err != nil {
			return publish{}, err
		}
		in.byo = c
	}
	pub, err := in.publish(mode)
	if err != nil {
		return publish{}, err
	}
	return pub, in.portsFree(pub)
}

func (in *Installer) publish(mode string) (publish, error) {
	if mode != Loopback {
		return tlsPublish(cmp.Or(in.O.HTTPPort, 80), cmp.Or(in.O.HTTPSPort, 443))
	}
	addr, err := loopbackAddr(in.O.Address)
	if err != nil {
		return publish{}, err
	}
	ports, err := in.ports(in.R.Spec.Front.LoopbackPorts, in.O.Ports)
	return publish{addr: addr, ports: ports}, err
}

// tlsPublish is where a TLS front listens: Docker publishes on every address, so the ports are
// probed on each of this host's.
func tlsPublish(http, https int) (publish, error) {
	p := publish{addr: "127.0.0.1", all: true, ports: map[string]int{"http": http, "https": https}}
	for _, k := range []string{"http", "https"} {
		if v := p.ports[k]; v < 1 || v > 65535 {
			return publish{}, fmt.Errorf("--%s-port %d is not a TCP port", k, v)
		}
	}
	if http == https {
		return publish{}, fmt.Errorf("--http-port and --https-port are both %d", http)
	}
	return p, nil
}

// dirFree refuses a directory another run or project has taken: an install's .env, or a
// compose.yaml with none.
func (in *Installer) dirFree(dir string) error {
	if _, err := os.Stat(in.path(envName)); err == nil {
		return fmt.Errorf("%s now holds an install: run again to finish it", dir)
	}
	if _, err := os.Stat(in.path("compose.yaml")); err == nil {
		return fmt.Errorf("%s already holds a compose.yaml and no install of %s: choose another --dir", dir, in.R.Spec.Name)
	}
	return nil
}

func (in *Installer) portsFree(p publish) error {
	var busy []string
	hosts := []string{p.addr}
	if p.addr == "127.0.0.1" {
		hosts = append(hosts, "::1") // localhost's other address, the one an IPv6 host tries first
	}
	if p.all {
		hosts = append(hosts, hostAddrs()...)
	}
	for _, k := range sortedKeys(p.ports) {
		if slices.ContainsFunc(hosts, func(h string) bool { return !in.O.portFree(h, p.ports[k]) }) {
			busy = append(busy, fmt.Sprintf("%d (%s)", p.ports[k], k))
		}
	}
	if len(busy) == 0 {
		return nil
	}
	if p.all { // a TLS front
		return fmt.Errorf("port(s) %s already in use on this host: choose others with --http-port and --https-port", strings.Join(busy, ", "))
	}
	return fmt.Errorf("port(s) %s already in use on %s: choose others with --ports", strings.Join(busy, ", "), p.addr)
}

// decide computes every value a first install records, and checks each can be recorded. It returns
// the file and which keys hold secrets.
func (in *Installer) decide(ctx context.Context, dir, mode string, pub publish, back bool) (*EnvFile, map[string]bool, error) {
	o, s := &in.O, in.R.Spec
	values, err := in.inputs(ctx, in.inputList(mode), back)
	if err != nil {
		return nil, nil, err
	}
	tls := mode != Loopback
	domains, err := in.routeDomains(mode, func(k string) string { return values[k] })
	if err != nil {
		return nil, nil, err
	}
	prefix := s.Registry.Prefix
	if o.Registry != "" {
		prefix = strings.TrimSuffix(o.Registry, "/")
	}
	env := NewEnvFile()
	env.Set("COMPOSE_PROJECT_NAME", projectName(s.Name, dir))
	env.Set("STOWAGE_NAME", s.Name)
	env.Set("STOWAGE_SHORT_NAME", s.Short())
	env.Set("STOWAGE_PROJECT_VERSION", in.R.Manifest.ProjectVersion)
	env.Set("STOWAGE_REVISION", strconv.Itoa(in.R.Manifest.Revision))
	env.Set("STOWAGE_REGISTRY", prefix)
	env.Set("STOWAGE_TLS_MODE", mode)
	env.Set("STOWAGE_LOOPBACK_ADDRESS", pub.addr) // a TLS front publishes on every address; this one stays local
	if o.Channel != "" {
		env.Set(ChannelVar, o.Channel)
	}
	in.frontVars(env, mode, pub, domains)
	in.acmeForward(nil, env)
	env.Set(FrontImageVar, in.frontImage(prefix))
	for _, k := range s.ImageNames() {
		env.Set(ImageVar(k), in.imageRef(prefix, k))
	}
	// Every safe default is recorded, so a shell export never relaxes one.
	for _, k := range sortedKeys(s.SafeDefaults) {
		v := s.SafeDefaults[k]
		if relaxed, ok := s.Front.LoopbackEnv[k]; ok && !tls {
			v = relaxed
		}
		env.Set(k, v)
	}
	secrets := map[string]bool{}
	for _, i := range in.inputList(mode) {
		if v, ok := values[i.Key]; ok {
			env.Set(i.Key, v)
			secrets[i.Key] = i.Secret
		}
	}
	for _, sec := range in.secrets() {
		v := in.restoredSecret(sec.Key)
		if v == "" {
			if v, err = generate(o.rand(), sec.Gen); err != nil {
				return nil, nil, fmt.Errorf("secret %s: %w", sec.Key, err)
			}
		}
		env.Set(sec.Key, v)
		secrets[sec.Key] = true
	}
	// A rotation under way when the secrets were exported goes on from where it was.
	rotationVars(in.secrets(), func(k string) bool { _, ok := env.Get(k); return ok }, func(k, _ string) { env.Set(k, in.restoredSecret(k)); secrets[k] = true })
	return env, secrets, env.Check()
}

// rerun finishes the setup of an existing install of this release. What it was given — --set,
// --answers, --domains, the front's ports — changes what the install recorded first (changes):
// the changes are logged, the ports the install does not hold probed, then the stack is brought
// up again with them.
func (in *Installer) rerun(ctx context.Context) (err error) {
	o := &in.O
	if o.Address != "" || o.Registry != "" || o.RegistryCA != "" || o.Channel != "" {
		return errors.New("--address, --registry, --registry-ca and --channel are recorded at the first install: this directory already holds one (a re-run changes the inputs with --set, --answers and --domains, the front's ports, and its TLS mode and certificate)")
	}
	env, unlock, err := in.openLocked(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := in.sameRelease(env); err != nil {
		return err
	}
	in.again = true
	next, secrets, probe, err := in.changes(env)
	if err != nil {
		return err
	}
	if next != nil {
		if err := in.portsFree(probe); err != nil {
			return err
		}
	}
	if err := in.checkDocker(ctx); err != nil {
		return err
	}
	was, _ := env.Get("STOWAGE_TLS_MODE")
	mode := was
	if next != nil {
		mode, _ = next.Get("STOWAGE_TLS_MODE")
	}
	if mode == "byo" && in.newCert == nil {
		for _, f := range []string{"cert.pem", "key.pem"} {
			if _, err := os.Stat(filepath.Join(o.Dir, TLSDir, f)); err != nil {
				return fmt.Errorf("the install's certificate is missing: %w", err)
			}
		}
	}
	if next != nil {
		in.logChanges(env, next, secrets)
		in.acmeForward(env, next)
	}
	if o.DryRun {
		if next != nil {
			o.logf("%s holds this release: a run would record these changes and bring the stack up again", o.Dir)
		} else {
			o.logf("%s holds this release: a run would bring the stack up again", o.Dir)
		}
		return nil
	}
	if unfinished, _, _ := in.firstInstall(); unfinished {
		// From here this run brings the stack up: it finishes the first install, or records why
		// not (a refusal or a dry run above leaves the marker as it was).
		in.markInstall(nil)
		defer func() {
			if err != nil {
				in.markInstall(err)
			}
		}()
	}
	if err := in.fetchImages(ctx, env); err != nil {
		return err
	}
	if err := in.writeNewCert(); err != nil {
		return err
	}
	if next == nil {
		if err := in.writeStack(env); err != nil {
			return err
		}
		in.keepSelf()
		if err := in.cameUp(in.start(ctx, env)); err != nil {
			return err
		}
		in.leftByo(env)
		return nil
	}
	// .env.prev is the last .env that came up: a change that did not leaves it as it is, so a
	// secret input's old value — logged redacted — is never lost to a second try.
	if _, err := os.Stat(in.path(changingFile)); errors.Is(err, os.ErrNotExist) {
		if err := env.WriteFile(in.path(envPrev)); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := os.WriteFile(in.path(changingFile), nil, 0o600); err != nil {
		return err
	}
	if err := next.WriteFile(in.path(envName)); err != nil {
		return err
	}
	o.logf("recorded the changes in %s; %s holds the values the stack last came up with", in.path(envName), in.path(envPrev))
	recorded := func(err error) error {
		if err == nil {
			return nil
		}
		return fmt.Errorf("%w\nthe changes are recorded; %s holds the values the stack last came up with: a re-run giving them goes back", err, in.path(envPrev))
	}
	if err := in.writeStack(next); err != nil {
		return recorded(err)
	}
	in.keepSelf()
	if err := in.cameUp(in.start(ctx, next)); err != nil {
		return recorded(err)
	}
	in.leftByo(next)
	return nil
}

// leftByo removes, once a re-run's stack came up outside byo, the certificate and key of the byo
// mode it left: the .env it replaced (.env.prev, kept while a change has not come up) recorded
// byo, so tls/ is the install's own.
func (in *Installer) leftByo(env *EnvFile) {
	if mode, _ := env.Get("STOWAGE_TLS_MODE"); mode == "byo" {
		return
	}
	prev, err := ReadEnvFile(in.path(envPrev))
	if err != nil {
		return
	}
	if mode, _ := prev.Get("STOWAGE_TLS_MODE"); mode != "byo" {
		return
	}
	if _, err := os.Stat(in.path(TLSDir)); err != nil {
		return
	}
	if err := os.RemoveAll(in.path(TLSDir)); err != nil {
		in.O.logf("%v: remove %s", err, in.path(TLSDir))
		return
	}
	in.O.logf("removed %s, which the front no longer reads", in.path(TLSDir))
}

// cameUp ends a re-run whose stack came up (err nil): the values it recorded are the ones
// .env.prev is taken from at the next change. A marker it cannot remove is only reported: the
// stack is up.
func (in *Installer) cameUp(err error) error {
	if err == nil {
		if rmErr := os.Remove(in.path(changingFile)); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			in.O.logf("%v: the next change keeps %s as it is", rmErr, in.path(envPrev))
		}
	}
	return err
}

// The first install's marker: empty while a run is under way (or after a reboot or a kill cut
// it: a re-run, or the VM's boot, finishes it); installFailed and why once a run failed, then
// stackUp when the stack had come up (the provision hook failed: `provision` finishes it).
const (
	installFailed = "failed: "
	stackUp       = "stack: up"
)

// provisionFailed is a provision hook's failure on a stack that came up.
type provisionFailed struct{ error }

func (e provisionFailed) Unwrap() error { return e.error }

// firstInstall reports whether the first install has not finished and, when the last run of it
// failed, why; up tells that its stack had come up.
func (in *Installer) firstInstall() (unfinished bool, failed string, up bool) {
	raw, err := os.ReadFile(in.path(installingFile))
	if err != nil {
		return false, "", false
	}
	first, rest, _ := strings.Cut(string(raw), "\n")
	return true, strings.TrimPrefix(first, installFailed), strings.TrimSpace(rest) == stackUp
}

// maxReason is the most bytes of an error a marker records.
const maxReason = 300

// reason is the first line of err, at most maxReason bytes, cut at a character's boundary.
func reason(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	if len(line) <= maxReason {
		return line
	}
	cut := maxReason
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut]
}

// markInstall records the first install's state in its marker: under way (err nil, or a run
// cancelled: a reboot or a stop), or failed and why. A marker it cannot write is only reported.
func (in *Installer) markInstall(err error) {
	body := ""
	if err != nil && !errors.Is(err, context.Canceled) {
		body = installFailed + reason(err) + "\n"
		if errors.As(err, new(provisionFailed)) {
			body += stackUp + "\n"
		}
	}
	if werr := writeAtomic(in.path(installingFile), strings.NewReader(body), 0o600, nil); werr != nil {
		in.O.logf("%v: status cannot tell how the first install went", werr)
	}
	if body != "" {
		in.installEvent(evInstallFailed, reason(err))
	}
}

// installEvent journals the end of a VM's first install run: finished, or failed and why.
func (in *Installer) installEvent(kind, why string) {
	if !in.O.Preloaded {
		return
	}
	m := in.R.Manifest
	rel := Version{m.ProjectVersion, m.Revision}
	j := &journal{dir: in.O.Dir, boot: bootRecord(&in.O, rel)}
	if _, err := j.append(Event{Kind: kind, Release: &rel, Reason: why}); err != nil {
		in.O.logf("the event journal: %v", err)
	}
}

// restoreAwaited reports whether the install waits for its data before its provision hook runs.
func (in *Installer) restoreAwaited() bool { return fileExists(in.path(restoreAwaitedFile)) }

// restoreDone removes the restore-awaited marker: the hook ran on the data its values belong to,
// or on the data the operator chose. A marker it cannot remove is only reported.
func (in *Installer) restoreDone() {
	if err := os.Remove(in.path(restoreAwaitedFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		in.O.logf("%v: status still reports a restore awaited", err)
	}
}

// installDone removes the first install's marker: its stack came up and the provision hook ran
// (or there is none, or --no-provision skipped it). A marker it cannot remove is only reported.
func (in *Installer) installDone() {
	err := os.Remove(in.path(installingFile))
	switch {
	case err == nil:
		in.installEvent(evInstallFinished, "")
	case !errors.Is(err, os.ErrNotExist):
		in.O.logf("%v: status reports the first install as not finished", err)
	}
}

// forgetChanges removes .env.prev and the change marker: an upgrade or a rollback switched the
// release, and values of another release are no way back.
func (in *Installer) forgetChanges() {
	for _, f := range []string{envPrev, changingFile} {
		if err := os.Remove(in.path(f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			in.O.logf("%v", err)
		}
	}
}

// copySelf keeps the running installer in the install as <short>-stow, the command that manages it.
func (in *Installer) copySelf() error {
	if in.O.Self == "" {
		return nil
	}
	dst := in.path(in.R.Ctl())
	if a, err := os.Stat(in.O.Self); err == nil {
		if b, err := os.Stat(dst); err == nil && os.SameFile(a, b) {
			return nil
		}
	}
	f, err := os.Open(cmp.Or(in.O.SelfExe, in.O.Self))
	if err != nil {
		return err
	}
	defer f.Close()
	// The copy must carry this run's release: it is what manages the install from now on.
	return writeAtomic(dst, f, 0o755, func(tmp string) error {
		files, err := payload.Read(tmp)
		if err != nil {
			return err
		}
		r, err := Load(files)
		if err != nil {
			return err
		}
		if m, want := r.Manifest, in.R.Manifest; m.Name != want.Name || m.ProjectVersion != want.ProjectVersion || m.Revision != want.Revision {
			return fmt.Errorf("%s carries %s %s (revision %d), not %s %s (revision %d)", in.O.Self, m.Name, m.ProjectVersion, m.Revision, want.Name, want.ProjectVersion, want.Revision)
		}
		return nil
	})
}

// keepSelf is copySelf for an install or a re-run: a failure is reported, not fatal — the
// stack starts, and an upgrade refuses until an installer is kept.
func (in *Installer) keepSelf() {
	if err := in.copySelf(); err != nil {
		in.O.logf("the installer could not be kept in the install (%v): an upgrade needs it, `%s --dir %s` keeps it", err, in.O.Self, in.O.Dir)
	}
}

// start brings the stack up, then runs the provision hook — idempotent, so a re-run runs it
// again — unless --no-provision. Its caller holds the lifecycle lock.
func (in *Installer) start(ctx context.Context, env *EnvFile) error {
	if err := in.up(ctx, env); err != nil {
		return err
	}
	h, err := in.hook()
	if err != nil {
		return err
	}
	if err := in.provisionUp(ctx, env, h); err != nil {
		return err
	}
	in.installDone()
	return nil
}

// provisionUp runs the provision hook h (nil: none) on the stack just brought up, unless
// --no-provision.
func (in *Installer) provisionUp(ctx context.Context, env *EnvFile, h *providers.ProvisionHook) error {
	if h == nil {
		return nil
	}
	if in.O.NoProvision {
		in.O.logf("not provisioned (--no-provision): `%s provision --dir %s` does it", in.ctl(), in.O.Dir)
		return nil
	}
	if in.restoreAwaited() {
		if in.O.Preloaded {
			in.O.logf("not provisioned: --secrets gave the provision hook's values from another install, which belong to its data: `sudo %s restore OBJECT` loads a backup of it and provisions", in.R.Ctl())
		} else {
			in.O.logf("not provisioned: --secrets gave the provision hook's values from another install, which belong to its data: load a backup of it, then `%s provision --dir %s` provisions it", in.ctl(), in.O.Dir)
		}
		return nil
	}
	if err := in.runHook(ctx, env, h); err != nil {
		return provisionFailed{err}
	}
	return nil
}

// sameRelease refuses an install of another release: changing release is an upgrade.
func (in *Installer) sameRelease(env *EnvFile) error {
	m := in.R.Manifest
	name, v, rev, err := sameProject(in.O.Dir, env, m)
	if err != nil {
		return err
	}
	if v != m.ProjectVersion || rev != strconv.Itoa(m.Revision) {
		if in.newer(v, rev) != nil {
			if _, err := in.keptInstaller(name, m.ProjectVersion, strconv.Itoa(m.Revision)); err == nil {
				return fmt.Errorf("%s runs %s %s (revision %s), and keeps this installer, of %s (revision %d): the upgrade stopped before keeping its own; run the installer of %s with --dir %s", in.O.Dir, name, v, rev, m.ProjectVersion, m.Revision, v, in.O.Dir)
			}
			return fmt.Errorf("%s runs %s %s (revision %s), after this installer's %s (revision %d): run the installer it keeps, %s", in.O.Dir, name, v, rev, m.ProjectVersion, m.Revision, in.ctl())
		}
		return fmt.Errorf("%s runs %s %s (revision %s); this installer carries %s (revision %d): `%s upgrade --dir %s` moves it to this release", in.O.Dir, name, v, rev, m.ProjectVersion, m.Revision, cmp.Or(in.O.Self, in.R.Ctl()), in.O.Dir)
	}
	return nil
}

// writeStack writes the release's compose file and the front, which come from the payload and the
// recorded values and are rewritten on every run.
func (in *Installer) writeStack(env *EnvFile) error {
	return in.renderStack(env, in.O.Dir)
}

// renderStack writes the release's stack files for env into dir.
func (in *Installer) renderStack(env *EnvFile, dir string) error {
	f := in.R.Spec.Front
	mode, _ := env.Get("STOWAGE_TLS_MODE")
	get := func(k string) string { v, _ := env.Get(k); return v }
	cf := loopbackCaddyfile(f, get)
	if mode != Loopback {
		domains := map[string]string{}
		for _, r := range f.Routes {
			domains[r.Name], _ = env.Get(r.DomainInput)
		}
		email, _ := env.Get(ACMEEmailVar)
		httpPort, _ := env.Get(HTTPPortVar)
		httpsPort, _ := env.Get(HTTPSPortVar)
		cf = tlsCaddyfile(f, mode, domains, email, httpPort, httpsPort, get)
	}
	// The front's configuration: the Caddyfile, and in byo the files of tls/ it reads.
	h := sha256.New()
	fmt.Fprintf(h, "Caddyfile %d\n%s", len(cf), cf)
	ca := ""
	switch mode {
	case "internal":
		ca = localRoot
	case "byo":
		for _, name := range []string{"cert.pem", "key.pem", "ca.pem"} {
			raw, err := os.ReadFile(in.path(filepath.Join(TLSDir, name)))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%s %d\n%s", name, len(raw), raw)
			if name == "ca.pem" {
				ca = byoCA
			}
		}
	}
	fc, err := frontCompose(f, mode, in.hostAPI(), "sha256:"+hex.EncodeToString(h.Sum(nil)), ca, get(IPv6Var) == "true")
	if err != nil {
		return err
	}
	data := map[string][]byte{"compose.yaml": in.R.Compose, FrontCompose: fc, Caddyfile: []byte(cf)}
	for _, name := range stackFiles {
		if err := os.WriteFile(filepath.Join(dir, name), data[name], 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Compose runs `docker compose` on the install, with the recorded values shadowing the shell's.
func (in *Installer) Compose(ctx context.Context, env *EnvFile, stream io.Writer, args ...string) ([]byte, error) {
	return in.O.Docker.Run(ctx, Cmd{Args: append(in.composeBase(), args...), Env: env.Environ(in.O.environ()), Stream: stream})
}

// composeBase is `docker compose` on the install's stack, plus the extra compose files.
func (in *Installer) composeBase(extra ...string) []string {
	base := []string{"compose", "--project-directory", in.O.Dir, "-f", in.path("compose.yaml"), "-f", in.path(FrontCompose)}
	for _, f := range extra {
		base = append(base, "-f", in.path(f))
	}
	return base
}

// up brings the stack up; once it came up, tls.prev/ (certDone) is no fallback any more. When it
// fails with one of the release's images gone from the engine since it was pulled (removed
// meanwhile: Compose then refuses to create its container), the images are pulled again and the
// stack started once more.
func (in *Installer) up(ctx context.Context, env *EnvFile) error {
	if err := in.openPorts(ctx, env); err != nil {
		return err
	}
	in.O.logf("starting %s", in.R.Spec.Name)
	base, cleanup, err := in.pullEnviron(env)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := in.ipv6Network(ctx, env); err != nil {
		return err
	}
	run := func() error {
		_, err := in.O.Docker.Run(ctx, Cmd{Args: append(in.composeBase(), "up", "--detach", "--wait", "--remove-orphans"), Env: env.Environ(base), Stream: in.O.Log})
		return err
	}
	if err = run(); err != nil {
		if gone := in.goneImages(ctx, env); len(gone) > 0 {
			in.O.logf("%s left the engine as the stack started: pulling again", strings.Join(gone, ", "))
			if err = in.fetchImages(ctx, env); err == nil {
				err = run()
			}
		}
	}
	if err != nil {
		return fmt.Errorf("the stack did not become ready: %w (see: docker compose --project-directory %s logs)", err, in.O.Dir)
	}
	in.certDone()
	url, _ := env.Get(spec.URLVar(in.R.Spec.Front.Portal))
	fmt.Fprintf(in.O.Out, "%s %s is running: %s\n", in.R.Spec.Name, in.R.Manifest.ProjectVersion, url)
	return nil
}

// ipv6Network stops the stack when IPv6 was turned on or off (--ipv6) since its default network
// was made, as its label records: a Compose that does not make a changed network again would
// keep it, so `up` makes it again. Volumes are kept.
func (in *Installer) ipv6Network(ctx context.Context, env *EnvFile) error {
	get := func(k string) string { v, _ := env.Get(k); return v }
	want := get(IPv6Var) == "true"
	out, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"network", "ls", "--filter", "label=com.docker.compose.project=" + get("COMPOSE_PROJECT_NAME"), "--filter", "label=com.docker.compose.network=default", "--format", "{{.ID}} {{.Label \"" + IPv6Label + "\"}}"}})
	if err != nil {
		return fmt.Errorf("listing the stack's network: %w", err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	id, made, _ := strings.Cut(line, " ")
	// No label is IPv6 off: a network the project or the daemon gave IPv6 by itself keeps it.
	if id == "" || (strings.TrimSpace(made) == "true") == want {
		return nil
	}
	in.O.logf("IPv6 turned %s: the stack stops, and its network is made again", map[bool]string{true: "on", false: "off"}[want])
	// Orphans too: a container left on the network would keep it.
	if _, err := in.Compose(ctx, env, in.O.Log, "down", "--remove-orphans"); err != nil {
		return fmt.Errorf("stopping the stack to make its network again: %w", err)
	}
	return nil
}

// FirewallConf is the VM's firewall ruleset, which flushes its own table only.
const FirewallConf = "/etc/nftables.conf"

// openPorts opens a TLS front's host ports in the VM's firewall (O.Firewall): a rule file of the
// install's own, then the ruleset loaded again when the file changed. Loopback mode publishes
// nothing outside the VM: the file goes. Forwarded traffic never meets the input chain, but what
// Docker's userland proxy takes on the host does: an IPv6 client of a bridge without IPv6, a
// connection from the host to its own address.
func (in *Installer) openPorts(ctx context.Context, env *EnvFile) error {
	if in.O.Firewall == "" {
		return nil
	}
	file := filepath.Join(in.O.Firewall, in.R.Spec.Name+"-front.nft")
	var rule []byte
	if mode, _ := env.Get("STOWAGE_TLS_MODE"); mode != Loopback {
		var ports []int
		for _, k := range []string{HTTPPortVar, HTTPSPortVar} {
			v, _ := env.Get(k)
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 65535 {
				return fmt.Errorf("%s %q is not a TCP port", k, v)
			}
			ports = append(ports, n)
		}
		rule = fmt.Appendf(nil, "# %s's front, which every start of the install writes.\ntcp dport { %d, %d } accept\n", in.R.Spec.Name, ports[0], ports[1])
	}
	cur, _ := os.ReadFile(file)
	if bytes.Equal(cur, rule) {
		return nil
	}
	var err error
	if rule == nil {
		err = os.Remove(file)
	} else if err = os.MkdirAll(in.O.Firewall, 0o755); err == nil {
		err = writeAtomic(file, bytes.NewReader(rule), 0o644, nil)
	}
	if err != nil {
		return fmt.Errorf("the firewall's rule for the front: %w", err)
	}
	if _, err := in.O.Host.Run(ctx, Cmd{Args: []string{"nft", "-f", FirewallConf}}); err != nil {
		// No file a failed load left: the next start finds a change, and loads again.
		_ = os.Remove(file)
		return fmt.Errorf("loading the firewall with the front's ports: %w", err)
	}
	return nil
}

// tunables prints each advanced input of the install's shape: its recorded value (a secret's
// redacted), or (not set) when none is: the compose file's default, or off for one the front
// reads.
func (in *Installer) tunables(env *EnvFile, tls bool) {
	for _, i := range in.R.Spec.Inputs {
		if !i.Advanced || !applies(i, in.shape(), tls) {
			continue
		}
		v, ok := env.Get(i.Key)
		switch {
		case !ok:
			v = "(not set)"
		case i.Secret:
			v = redact(v)
		}
		fmt.Fprintf(in.O.Out, "%-9s %s=%s\n", "tunable", i.Key, v)
	}
}

// Status prints what the install runs and its services.
func (in *Installer) Status(ctx context.Context) error {
	dir, err := filepath.Abs(in.O.Dir)
	if err != nil {
		return err
	}
	in.O.Dir = dir
	env, err := ReadEnvFile(in.path(envName))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s holds no install", dir)
	}
	if err != nil {
		return err
	}
	get := func(k string) string { v, _ := env.Get(k); return v }
	fmt.Fprintf(in.O.Out, "%s %s (revision %s)\nregistry  %s\n", get("STOWAGE_NAME"), get("STOWAGE_PROJECT_VERSION"), get("STOWAGE_REVISION"), get("STOWAGE_REGISTRY"))
	switch unfinished, failed, _ := in.firstInstall(); {
	case failed != "":
		fmt.Fprintf(in.O.Out, "not finished: the first install failed (%s); a re-run of the installer with --dir %s finishes it\n", failed, dir)
	case unfinished:
		fmt.Fprintf(in.O.Out, "not finished: the first install runs, or was stopped; a re-run of the installer with --dir %s finishes it\n", dir)
	case in.restoreAwaited():
		fmt.Fprintf(in.O.Out, "not provisioned: a backup of the install --secrets came from is awaited; once loaded, `%s provision --dir %s` provisions it\n", in.ctl(), dir)
	}
	for _, r := range in.R.Spec.Front.Routes {
		fmt.Fprintf(in.O.Out, "%-9s %s\n", r.Name, get(spec.URLVar(r.Name)))
	}
	if url := in.dockerEndpoint(env); url != "" {
		fmt.Fprintf(in.O.Out, "%-9s %s\n", "endpoint", url)
	}
	in.tunables(env, get("STOWAGE_TLS_MODE") != Loopback)
	out, err := in.Compose(ctx, env, nil, "ps", "--all")
	if err != nil {
		return err
	}
	_, err = in.O.Out.Write(out)
	return err
}

// ports returns each route's loopback port: base's, with given (--ports) over it.
func (in *Installer) ports(base, given map[string]int) (map[string]int, error) {
	f := in.R.Spec.Front
	ports := maps.Clone(base)
	if ports == nil {
		ports = map[string]int{}
	}
	for k, v := range given {
		if !slices.ContainsFunc(f.Routes, func(r spec.Route) bool { return r.Name == k }) {
			return nil, fmt.Errorf("--ports: %q is no route (%s)", k, strings.Join(f.RouteNames(), ", "))
		}
		if v < 1 || v > 65535 {
			return nil, fmt.Errorf("--ports: %s=%d is not a TCP port", k, v)
		}
		ports[k] = v
	}
	seen := map[int]string{}
	for _, r := range f.Routes {
		p, ok := ports[r.Name]
		if !ok {
			return nil, fmt.Errorf("route %s has no port", r.Name)
		}
		if other, dup := seen[p]; dup {
			return nil, fmt.Errorf("--ports: %s and %s both use %d", other, r.Name, p)
		}
		seen[p] = r.Name
	}
	return ports, nil
}

// knownInputs refuses a --set or answers key that names no input of this run: a typo is an
// error, never a silently unused value.
func (in *Installer) knownInputs(mode string) error {
	used := map[string]bool{}
	for _, i := range in.inputList(mode) {
		used[i.Key] = true
	}
	var unknown []string
	for _, m := range []map[string]string{in.O.Set, in.O.Answers} {
		for k := range m {
			if !used[k] && !slices.Contains(unknown, k) {
				unknown = append(unknown, k)
			}
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		hint := ""
		if mode == Loopback && slices.ContainsFunc(unknown, func(k string) bool {
			return slices.ContainsFunc(in.R.Spec.Front.Routes, func(r spec.Route) bool { return r.DomainInput == k })
		}) {
			hint = "; a route's domain is one with --tls or --domains"
			if in.again {
				hint = "; this install is in loopback mode, which serves no domain"
			}
		}
		return fmt.Errorf("no input of this install is named %s (inputs: %s%s)", strings.Join(unknown, ", "), strings.Join(sortedKeys(used), ", "), hint)
	}
	return nil
}

// inputList is every input a first install in this mode records: the front's, then the project's
// that apply to this install's shape in this mode. A project input holding a route's domain is
// checked as one, and one the front's configuration reads as its value (frontCheck).
func (in *Installer) inputList(mode string) []input {
	tls := mode != Loopback
	out := in.frontInputs(mode)
	for _, i := range in.R.Spec.Inputs {
		if !applies(i, in.shape(), tls) {
			continue
		}
		check := frontCheck(in.R.Spec.Front, i.Key)
		domain := tls && slices.ContainsFunc(in.R.Spec.Front.Routes, func(r spec.Route) bool { return r.DomainInput == i.Key })
		if domain {
			check = in.domainCheck
		}
		out = append(out, input{i, check, domain})
	}
	return out
}

// shape is the deploy shape this install is: kairos on the VM, else docker.
func (in *Installer) shape() string {
	if in.O.Preloaded {
		return "kairos"
	}
	return "docker"
}

// applies reports whether an input is asked by an install of shape (docker or kairos) in this TLS
// mode.
func applies(i spec.Input, shape string, tls bool) bool {
	if len(i.Shapes) > 0 && !slices.Contains(i.Shapes, shape) {
		return false
	}
	return !(i.When == "tls" && !tls) && !(i.When == "loopback" && tls)
}

var refInDefault = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// inputs resolves each input of list: --set, then the environment, then the answers
// file, then — when a prompter is set — the operator's answer to one form over every input left,
// then the default (which may name an earlier input as ${KEY}). Every missing required input, then
// every value its check refuses, is reported at once. back lets the form's review go back to the
// TLS mode (ErrBack).
func (in *Installer) inputs(ctx context.Context, list []input, back bool) (map[string]string, error) {
	o := &in.O
	given := map[string]string{}
	var qs []Question
	for n, i := range list {
		v, ok := o.Set[i.Key]
		// An advanced input is never taken from the environment: it would be recorded unseen.
		if !ok && !i.Advanced && in.inputEnv(i.Key) != "" {
			v, ok = in.inputEnv(i.Key), true
			// On a terminal a route's domain from the environment is asked, as the default: what
			// the stack is published on is never taken from an exported variable unseen.
			if o.Prompt != nil && i.domain {
				list[n].Default, ok = v, false
				i = list[n]
			}
		}
		if !ok {
			v, ok = o.Answers[i.Key]
		}
		if ok {
			given[i.Key] = v
		} else if !i.Advanced {
			qs = append(qs, Question{Title: cmp.Or(i.Prompt, i.Key), Key: i.Key, Required: i.Required, Secret: i.Secret, Check: envAnd(i.check)})
		}
	}
	// A given value its check refuses is reported before the form, which would discard the answers.
	var bad []string
	for _, i := range list {
		if v, ok := given[i.Key]; ok {
			if err := i.checkValue(v); err != nil {
				bad = append(bad, fmt.Sprintf("%s: %v", i.Key, err))
			}
		}
	}
	if len(bad) > 0 {
		return nil, errors.New(strings.Join(bad, "; "))
	}
	defaults := func(answers map[string]string) map[string]string {
		values, _ := in.resolve(list, given, answers)
		return values
	}
	var answers map[string]string
	if o.Prompt != nil && len(qs) > 0 {
		f := Form{Questions: qs, Defaults: defaults, Review: true, Answers: in.answered, Go: in.goOn}
		if back {
			f.Back = "how it is served"
		}
		var err error
		answers, err = o.Prompt.Ask(ctx, f)
		if errors.Is(err, ErrBack) {
			in.answered = answers
		}
		if err != nil {
			return nil, err
		}
	}
	values, missing := in.resolve(list, given, answers)
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required input(s), set by --set KEY=VALUE, the environment or --answers: %s", strings.Join(missing, "; "))
	}
	for _, i := range list {
		if i.check == nil {
			continue
		}
		if err := i.check(values[i.Key]); err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", i.Key, err))
		}
	}
	if len(bad) > 0 {
		return nil, errors.New(strings.Join(bad, "; "))
	}
	return values, nil
}

// checkValue checks a value given for i: its own check, then the .env rule.
func (i input) checkValue(v string) error {
	if i.check == nil {
		return spec.CheckEnvValue(v)
	}
	return envAnd(i.check)(v)
}

// envAnd is check, then the .env rule.
func envAnd(check func(string) error) func(string) error {
	if check == nil {
		return nil
	}
	return func(v string) error {
		if err := check(v); err != nil {
			return err
		}
		return spec.CheckEnvValue(v)
	}
}

// resolve takes, in input order, each given value, else a non-empty answer, else the default
// expanded over the values before it; an advanced input only when given. The form shows what it returns for an unanswered field, so
// the default it displays is the one recorded.
func (in *Installer) resolve(list []input, given, answers map[string]string) (map[string]string, []string) {
	values := map[string]string{}
	for k, v := range in.recorded {
		values[k] = v
	}
	var missing []string
	for _, i := range list {
		v, ok := given[i.Key]
		if !ok && i.Advanced {
			continue // recorded only when given: the compose file's default otherwise
		}
		if !ok {
			v = answers[i.Key]
		}
		if !ok && v == "" {
			v = refInDefault.ReplaceAllStringFunc(i.Default, func(m string) string {
				return values[refInDefault.FindStringSubmatch(m)[1]]
			})
		}
		if v == "" && i.Required {
			missing = append(missing, fmt.Sprintf("%s (%s)", i.Key, i.Prompt))
			continue
		}
		values[i.Key] = v
	}
	return values, missing
}

// imageRef is where an image is taken from: pinned by digest; offline, by the release's tag,
// because `docker load` on the classic image store keeps tags but not registry digests — an
// offline install runs the images the operator loaded. A preloaded image keeps the name the OS
// bundle loaded it under, the project's own prefix, whatever registry (a mirror) the install
// reads its updates from.
func (in *Installer) imageRef(prefix, key string) string {
	if in.O.Preloaded {
		return fmt.Sprintf("%s/%s-%s:%s", in.R.Spec.Registry.Prefix, in.R.Spec.Name, key, release.PreloadTag(in.R.Manifest.Artifacts.Images[key]))
	}
	if in.O.Offline {
		return fmt.Sprintf("%s/%s-%s:%s", prefix, in.R.Spec.Name, key, in.R.Manifest.Tag)
	}
	return in.R.ImageRef(prefix, key)
}

// frontImage is where the release's front is taken from: published with its images.
func (in *Installer) frontImage(prefix string) string { return in.imageRef(prefix, front.Key) }

// checkDocker requires the Docker engine (≥ MinDockerIPv6 with --ipv6) and Compose ≥ MinCompose.
func (in *Installer) checkDocker(ctx context.Context) error {
	server, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"version", "--format", "{{.Server.Version}}"}})
	if err != nil {
		return fmt.Errorf("the Docker engine is not reachable: %w", err)
	}
	if on := in.O.IPv6; on != nil && *on && !in.O.DryRun {
		if v, ok := parseVersion(string(server)); !ok || older(v, MinDockerIPv6) {
			return fmt.Errorf("--ipv6: the Docker engine %s is too old: %d.%d or later gives a network IPv6 (a unique local subnet of its own) and forwards its published ports over IPv6", strings.TrimSpace(string(server)), MinDockerIPv6[0], MinDockerIPv6[1])
		}
		// The engine must make an IPv6 network before anything is written for one: a host
		// without IPv6, or without ip6tables, cannot.
		probe := "stowage-ipv6-check-" + strings.ToLower(rand.Text()[:8])
		if _, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"network", "create", "--ipv6", "--label", "io.stowage.probe=true", probe}}); err != nil {
			return fmt.Errorf("--ipv6: the Docker engine cannot make an IPv6 network here: %w", err)
		}
		if _, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"network", "rm", probe}}); err != nil {
			in.O.logf("removing the network %s: %v", probe, err)
		}
	}
	out, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"compose", "version", "--short"}})
	if err != nil {
		return fmt.Errorf("the Docker Compose plugin is required (docker compose, not docker-compose): %w", err)
	}
	v, ok := parseVersion(string(out))
	if !ok || older(v, MinCompose) {
		return fmt.Errorf("the Docker Compose plugin %s is too old: %d.%d or later is required", strings.TrimSpace(string(out)), MinCompose[0], MinCompose[1])
	}
	return nil
}

// image is one image the stack runs, and the IDs a loaded copy of the release's image may have.
type image struct {
	ref string
	// ids: the image digest and the linux/amd64 manifest digest (containerd store), the
	// linux/amd64 config digest (classic store).
	ids []string
}

// pinned reports whether the reference names its content by digest; otherwise it is a tag the
// operator loaded (an offline install), whose content is checked against ids.
func (im image) pinned() bool { return strings.Contains(im.ref, "@sha256:") }

// stackImages lists the images the recorded file names.
func (in *Installer) stackImages(env *EnvFile) []image {
	a := in.R.Manifest.Artifacts
	ref, _ := env.Get(FrontImageVar)
	out := []image{{ref, []string{a.Images[front.Key], a.Manifests[front.Key], a.Configs[front.Key]}}}
	for _, k := range in.R.Spec.ImageNames() {
		ref, _ := env.Get(ImageVar(k))
		out = append(out, image{ref, []string{a.Images[k], a.Manifests[k], a.Configs[k]}})
	}
	return out
}

// goneImages are the release's images pinned by digest that the engine does not hold (an
// offline install's, recorded by tag, are never pulled: none).
func (in *Installer) goneImages(ctx context.Context, env *EnvFile) []string {
	var gone []string
	for _, im := range in.stackImages(env) {
		if !im.pinned() {
			continue
		}
		if _, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"image", "inspect", "--format", "{{.Id}}", im.ref}}); err != nil {
			gone = append(gone, im.ref)
		}
	}
	return gone
}

// fetchImages makes every image the recorded file names present. One pinned by digest that is
// present is the right one, and only the missing ones are pulled. One recorded by tag (an offline
// install) is never pulled — a tag can move — and must be the release's own image: its ID is one
// the release manifest records. Which applies is decided by what was recorded at the first
// install, not by this run's flags.
func (in *Installer) fetchImages(ctx context.Context, env *EnvFile) error {
	var missing, unloaded, wrong []string
	for _, im := range in.stackImages(env) {
		out, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"image", "inspect", "--format", "{{.Id}}", im.ref}})
		id := strings.TrimSpace(string(out))
		switch {
		case err != nil && im.pinned():
			missing = append(missing, im.ref)
		case err != nil:
			unloaded = append(unloaded, im.ref)
		case !im.pinned() && !slices.Contains(im.ids, id):
			wrong = append(wrong, fmt.Sprintf("%s is %s, not the release's image", im.ref, id))
		}
	}
	if len(wrong) > 0 {
		return fmt.Errorf("a loaded image is not the one this release ships:\n  %s", strings.Join(wrong, "\n  "))
	}
	if len(unloaded) > 0 && in.O.Preloaded {
		unit := preloadUnit(in.R.Spec.Name)
		failed := ""
		if in.preloadErr != nil {
			failed = fmt.Sprintf(" — it failed (%v) —", in.preloadErr)
		}
		return fmt.Errorf("these images are not loaded, which %s loads from the OS%s (`journalctl -u %s` tells why it did not):\n  %s", unit, failed, unit, strings.Join(unloaded, "\n  "))
	}
	if len(unloaded) > 0 {
		return fmt.Errorf("these images are not loaded (an offline install: docker load them first):\n  %s", strings.Join(unloaded, "\n  "))
	}
	if len(missing) == 0 {
		return nil
	}
	if in.O.Offline {
		return fmt.Errorf("--offline: these images are not loaded (docker load them first):\n  %s", strings.Join(missing, "\n  "))
	}
	prefix, _ := env.Get("STOWAGE_REGISTRY")
	if err := in.dockerCA(prefix); err != nil {
		return err
	}
	cfg, cleanup, err := in.pullConfig(prefix)
	if err != nil {
		return err
	}
	defer cleanup()
	for _, r := range missing {
		in.O.logf("pulling %s", r)
		if _, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"pull", "--quiet", r}, Env: cfg, Stream: in.O.Log}); err != nil {
			return fmt.Errorf("%w\nnothing was written; for an install without registry access, load the images and use --offline", err)
		}
	}
	return nil
}

// pullEnviron is the environment `compose up` and the provision hook's run take: the one
// fetchImages pulls with, so an image gone since it ran (pruned meanwhile) is pulled again with
// the pull account, not anonymously. An offline install pulls nothing.
func (in *Installer) pullEnviron(env *EnvFile) ([]string, func(), error) {
	if in.O.Offline {
		return in.O.environ(), func() {}, nil
	}
	prefix, _ := env.Get("STOWAGE_REGISTRY")
	cfg, cleanup, err := in.pullConfig(prefix)
	if err != nil {
		return nil, nil, err
	}
	if cfg == nil {
		return in.O.environ(), cleanup, nil
	}
	return cfg, cleanup, nil
}

// pullConfig returns the environment the pulls run with. When the project has a pull account — its
// password from the environment, else the one baked into the installer — that is a private Docker
// config holding the account — written by the
// installer, never by `docker login`, so nothing reaches the operator's own config or credential
// store, and removed afterwards — which keeps the operator's current context (its contexts are
// linked in), so the pulls reach the daemon `compose up` uses. Otherwise it is nil: the
// installer's own environment and the Docker client's own login.
func (in *Installer) pullConfig(prefix string) ([]string, func(), error) {
	none := func() {}
	user, token, err := in.pullAccount(prefix)
	if err != nil || token == "" {
		return nil, none, err
	}
	repo, err := name.NewRepository(prefix)
	if err != nil {
		return nil, none, err
	}
	own := in.O.getenv("DOCKER_CONFIG")
	if own == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, none, err
		}
		own = filepath.Join(home, ".docker")
	}
	extra := map[string]any{}
	if raw, err := os.ReadFile(filepath.Join(own, "config.json")); err == nil {
		var mine struct {
			CurrentContext string `json:"currentContext"`
		}
		if json.Unmarshal(raw, &mine) == nil && mine.CurrentContext != "" {
			extra["currentContext"] = mine.CurrentContext
		}
	}
	// Inside the install directory, not the system temporary one: a confined docker CLI (the
	// Ubuntu snap) has a private /tmp, and would pull without the account. A directory this run
	// creates is removed again if the install stops here.
	created := false
	if _, err := os.Stat(in.O.Dir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(in.O.Dir, 0o755); err != nil {
			return nil, none, err
		}
		created = true
	}
	dir, err := os.MkdirTemp(in.O.Dir, ".stowage-pull-")
	if err != nil {
		return nil, none, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if created {
		in.removeDir = true
	}
	if err := registry.WriteDockerConfig(dir, repo.Registry, &authn.AuthConfig{Username: user, Password: token}, extra); err != nil {
		cleanup()
		return nil, none, err
	}
	if _, err := os.Stat(filepath.Join(own, "contexts")); err == nil {
		if err := os.Symlink(filepath.Join(own, "contexts"), filepath.Join(dir, "contexts")); err != nil {
			cleanup()
			return nil, none, err
		}
	}
	var env []string
	for _, kv := range in.O.environ() {
		if !strings.HasPrefix(kv, "DOCKER_CONFIG=") {
			env = append(env, kv)
		}
	}
	return append(env, "DOCKER_CONFIG="+dir), cleanup, nil
}

// pullAccount is the project's pull account on its own registry: the password from the
// environment, else the one baked into the installer. The token is empty for public images, a
// mirror (the operator logs in to it) or a password nothing gives.
func (in *Installer) pullAccount(prefix string) (user, token string, err error) {
	p := in.R.Spec.Registry.Pull
	if p.Name != "token" || prefix != in.R.Spec.Registry.Prefix {
		return "", "", nil
	}
	c, err := providers.Token(p)
	if err != nil {
		return "", "", fmt.Errorf("registry.pull: %w", err)
	}
	user, token = c.User, in.O.getenv(c.TokenEnv)
	if token == "" && in.R.Pull != nil {
		user, token = in.R.Pull.User, in.R.Pull.Password
	}
	if token == "" {
		in.O.logf("%s is not set: reading the registry with the Docker client's own login", c.TokenEnv)
	}
	return user, token, nil
}

// projectName names an install's compose project after its directory: two installs in two
// directories with the same name stay two projects.
func projectName(name, dir string) string {
	base := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 'a' - 'A'
		}
		return '-'
	}, filepath.Base(dir))
	base = strings.Trim(base, "-")
	if len(base) > 32 {
		base = strings.TrimRight(base[:32], "-")
	}
	return fmt.Sprintf("%s-%s-%08x", name, base, crc32.ChecksumIEEE([]byte(dir)))
}

// loopbackAddr returns the loopback address ports bind to (compose's port syntax takes an IP,
// an IPv6 one bare): 127.0.0.1 by default and for "localhost", 127.x.x.x or ::1. Any other address
// would serve sign-in over plain HTTP to the network.
func loopbackAddr(a string) (string, error) {
	if a == "" || a == "localhost" {
		return "127.0.0.1", nil
	}
	ip, err := netip.ParseAddr(a)
	if err != nil || ip.Zone() != "" || !ip.Unmap().IsLoopback() {
		return "", fmt.Errorf("address %s is not a loopback address (127.x.x.x, ::1): without TLS, the stack would serve sign-in over plain HTTP to the network", a)
	}
	return ip.Unmap().String(), nil
}

func urlHost(a string) string {
	switch ip, _ := netip.ParseAddr(a); {
	case a == "127.0.0.1":
		return "localhost"
	case ip.Is6():
		return "[" + a + "]"
	}
	return a
}

const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// generate makes a secret with one of providers.SecretGenerators.
func generate(r io.Reader, gen string) (string, error) {
	bytesOf := func(n int) ([]byte, error) {
		b := make([]byte, n)
		_, err := io.ReadFull(r, b)
		return b, err
	}
	pass := func(n int) (string, error) {
		var s strings.Builder
		for range n {
			i, err := rand.Int(r, big.NewInt(int64(len(alnum))))
			if err != nil {
				return "", err
			}
			s.WriteByte(alnum[i.Int64()])
		}
		return s.String(), nil
	}
	switch gen {
	case "hex32", "hex16":
		b, err := bytesOf(map[string]int{"hex32": 32, "hex16": 16}[gen])
		return hex.EncodeToString(b), err
	case "b64-32":
		b, err := bytesOf(32)
		return base64.RawURLEncoding.EncodeToString(b), err
	case "pass18":
		return pass(18)
	case "pass32":
		return pass(32)
	}
	return "", fmt.Errorf("unknown generator %q", gen)
}

func (o *Options) rand() io.Reader {
	if o.Rand != nil {
		return o.Rand
	}
	return rand.Reader
}

// inputEnv is an input's value in the environment; none for a VM created from afar, which the
// operator's environment says nothing about.
func (in *Installer) inputEnv(k string) string {
	if v := in.restoredSecret(k); v != "" {
		return v
	}
	if in.remote {
		return ""
	}
	return in.O.getenv(k)
}

func (o *Options) getenv(k string) string {
	if o.Getenv == nil {
		return os.Getenv(k)
	}
	return o.Getenv(k)
}

// sourceAddr is the address this machine reaches dst from: the route's, read by a UDP socket's
// connect, which sends nothing.
func (o *Options) sourceAddr(dst netip.Addr) (netip.Addr, error) {
	if o.SourceAddr != nil {
		return o.SourceAddr(dst)
	}
	c, err := net.Dial("udp", netip.AddrPortFrom(dst, 22).String())
	if err != nil {
		return netip.Addr{}, err
	}
	defer c.Close()
	ap, err := netip.ParseAddrPort(c.LocalAddr().String())
	return ap.Addr(), err
}

// lookupIP resolves host to its addresses.
func (o *Options) lookupIP(host string) ([]netip.Addr, error) {
	if o.LookupIP != nil {
		return o.LookupIP(host)
	}
	return net.DefaultResolver.LookupNetIP(context.Background(), "ip", host)
}

func (o *Options) environ() []string {
	if o.Environ == nil {
		return os.Environ()
	}
	return o.Environ
}

func (o *Options) portFree(host string, port int) bool {
	if o.PortFree != nil {
		return o.PortFree(host, port)
	}
	return portFree(host, port)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
