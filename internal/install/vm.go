package install

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/chown"
	"github.com/norvexlabs/stowage/internal/front"
	"github.com/norvexlabs/stowage/internal/lint"
	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/spec"
)

// The Kairos VM shape. The OS bundle carries <short>-stow with the release as the bundle carries it
// (release.Manifest.EncodeBundled) and preloads the release's images. On the VM that <short>-stow
// installs the release from the preloaded images, brings the install up at every boot — upgrading
// it after an A/B upgrade brought a newer OS, going back when the VM booted the release the last
// upgrade replaced — and runs the host daemon the VM verbs talk to.

// ChannelVar records the channel a VM install follows for its updates.
const ChannelVar = "STOWAGE_CHANNEL"

// VMDir is the install directory on the VM: on the persistent partition, which an A/B upgrade
// keeps.
func VMDir(name string) string { return "/usr/local/" + name }

// BundledCtl is where the OS bundle carries the installer of the project whose short name is short.
func BundledCtl(short string) string { return "/usr/sbin/" + providers.Ctl(short) }

// RunDir is the host daemon's runtime directory, and Socket its socket in it.
func RunDir(name string) string { return "/run/" + name }

// Socket is the host daemon's socket.
func Socket(name string) string { return filepath.Join(RunDir(name), "host.sock") }

// The host daemon's app socket, which the services shapes.kairos.host_api names reach through
// their mount of AppDir at HostAppMount, given the token HostTokenVar records.
const (
	HostTokenVar  = "STOWAGE_HOST_TOKEN"
	HostSocketVar = "STOWAGE_HOST_SOCKET"
	HostAppMount  = "/run/stowage-host"
	appSocketName = "app.sock"
)

// AppDir is the directory of the host daemon's app socket, which the services named mount.
func AppDir(name string) string { return filepath.Join(RunDir(name), "app") }

// AppSocket is the host daemon's app socket.
func AppSocket(name string) string { return filepath.Join(AppDir(name), appSocketName) }

// hostAPI is the services given the app socket, and the host directory they mount.
type hostAPI struct {
	services []string
	dir      string
}

// hostAPI is what the stack of a VM install gives the app socket; nothing elsewhere.
func (in *Installer) hostAPI() hostAPI {
	k := in.R.Spec.Shapes.Kairos
	if !in.O.Preloaded || k == nil || len(k.HostAPI) == 0 {
		return hostAPI{}
	}
	return hostAPI{services: k.HostAPI, dir: AppDir(in.R.Spec.Name)}
}

// secrets are the secrets an install generates: the spec's, and on a VM whose stack reaches the
// host daemon, the app socket's token.
func (in *Installer) secrets() []spec.Secret {
	out := slices.Clone(in.R.Spec.Secrets)
	if len(in.hostAPI().services) > 0 {
		out = append(out, spec.Secret{Key: HostTokenVar, Gen: "hex32"})
	}
	return out
}

// SettingsFile is where a VM's cloud-config writes the settings of its first install.
func SettingsFile(name string) string { return "/etc/" + name + "/install.yaml" }

// InstallNetworkFile is where a VM's cloud-config writes, at every boot, the network `vm create`
// gave it (a NetConfig), for the host daemon to answer. The override's state is NetworkFile.
func InstallNetworkFile(name string) string { return "/etc/" + name + "/install-network.yaml" }

// Settings are a VM's first install as its cloud-config gives them: what `install` takes from its
// flags, but no secret — a secret input is given over SSH, by `sudo <short>-stow install`.
type Settings struct {
	Channel   string            `yaml:"channel,omitempty"`
	TLS       string            `yaml:"tls,omitempty"`
	HTTPPort  int               `yaml:"http_port,omitempty"`
	HTTPSPort int               `yaml:"https_port,omitempty"`
	Inputs    map[string]string `yaml:"inputs,omitempty"`
	// Registry is the prefix the install reads its updates from, a mirror (--registry); RegistryCA
	// is the file on the VM holding the CA its certificate chains to (VMRegistryCA).
	Registry   string `yaml:"registry,omitempty"`
	RegistryCA string `yaml:"registry_ca,omitempty"`
	// AwaitSecrets: the first install waits for its secrets (`vm create --secrets`), which
	// `sudo <short>-stow install --secrets` gives over SSH.
	AwaitSecrets bool `yaml:"await_secrets,omitempty"`
	// IPv6 turns IPv6 on for the stack's network (`vm create --ipv6`, as `install --ipv6`).
	IPv6 bool `yaml:"ipv6,omitempty"`
	// Release is the release the first install runs when the VM boots another release's disk (a
	// release that published none boots its channel's disk set): the first boot writes its OS
	// bundle into the passive slot and reboots into it, then installs. Absent, the OS's release.
	Release *OSRelease `yaml:"release,omitempty"`
}

// OSRelease is a release by its version and revision, its tag, and its OS bundle's digest.
type OSRelease struct {
	Version  string `yaml:"version"`
	Revision int    `yaml:"revision"`
	Tag      string `yaml:"tag"`
	OS       string `yaml:"os"`
}

// osSwitchFile records, on the persistent partition beside the install directory, the release
// whose OS bundle the first boot wrote: a boot finding it, still on another OS, stops instead of
// writing it again.
func osSwitchFile(dir string) string { return filepath.Clean(dir) + ".os-switch" }

// switchOS writes the OS bundle of the release the settings name into the passive slot and
// reboots into it; the boot after it makes the first install from that OS. A second boot still
// on another OS stops.
func (in *Installer) switchOS(ctx context.Context, st *Settings) error {
	o, m, t := &in.O, in.R.Manifest, st.Release
	if t.OS == "" || !strings.HasPrefix(t.OS, "sha256:") {
		return fmt.Errorf("%s: release.os %q is not a digest", cmp.Or(o.Settings, SettingsFile(in.R.Spec.Name)), t.OS)
	}
	mark := osSwitchFile(o.Dir)
	if raw, err := os.ReadFile(mark); err == nil && strings.TrimSpace(string(raw)) == t.Tag {
		return fmt.Errorf("this OS carries %s %s (revision %d) after %s's OS bundle was written and booted: the first install of %s %s stops (remove %s to write it again)",
			in.R.Spec.Name, m.ProjectVersion, m.Revision, t.Tag, in.R.Spec.Name, t.Version, mark)
	}
	if o.Host == nil {
		return errors.New("no host tool to write the OS bundle with")
	}
	prefix := cmp.Or(st.Registry, in.R.Spec.Registry.Prefix)
	src := fmt.Sprintf("%s/%s-os@%s", prefix, in.R.Spec.Name, t.OS)
	o.logf("this OS carries %s %s (revision %d); the first install runs %s (revision %d): writing its OS bundle %s, then rebooting into it",
		in.R.Spec.Name, m.ProjectVersion, m.Revision, t.Version, t.Revision, src)
	if err := writeAtomic(mark, strings.NewReader(t.Tag+"\n"), 0o600, nil); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if err := writeOSBundle(ctx, in, o.Host, os.TempDir(), prefix, src, o.Log); err != nil {
		// Written again by the next boot.
		_ = os.Remove(mark)
		return err
	}
	o.logf("%s's OS is written: rebooting into it", t.Tag)
	_, err := o.Host.Run(ctx, Cmd{Args: []string{"systemctl", "reboot"}})
	return err
}

// ReadSettings strictly decodes the settings file at path; nil when there is none.
func ReadSettings(path string) (*Settings, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st Settings
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&st); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &st, nil
}

// applySettings takes the settings file's values where the run was given none: the flags, then
// --answers, win. A byo certificate or a secret input in it is refused: the cloud-config carries
// none.
func (in *Installer) applySettings(path string, st *Settings) error {
	o := &in.O
	if st.Channel != "" && !lint.ChannelName(st.Channel) {
		return fmt.Errorf("%s: channel %q is not a channel name", path, st.Channel)
	}
	if st.AwaitSecrets && o.Secrets == nil && !o.NewSecrets {
		return fmt.Errorf("%s: the first install waits for its secrets (`vm create --secrets`): `sudo %s install --secrets FILE` gives them; without them it would generate new ones the backups do not match (--new-secrets does, the file lost)", path, in.R.Ctl())
	}
	if st.TLS == "byo" {
		return fmt.Errorf("%s: tls byo copies a certificate's private key: give it over SSH, with `sudo %s install --tls byo`", path, in.R.Ctl())
	}
	// Every mode's secrets: the mode is decided later, from the domains too.
	secret := map[string]bool{CloudflareTokenVar: true}
	for _, i := range in.R.Spec.Inputs {
		secret[i.Key] = secret[i.Key] || i.Secret
	}
	for _, k := range sortedKeys(st.Inputs) {
		if secret[k] {
			return fmt.Errorf("%s: %s is a secret input: give it over SSH, with `sudo %s install`", path, k, in.R.Ctl())
		}
	}
	o.Channel = cmp.Or(o.Channel, st.Channel)
	o.Registry, o.RegistryCA = cmp.Or(o.Registry, st.Registry), cmp.Or(o.RegistryCA, st.RegistryCA)
	// A certificate given decides the mode (byo), as without the file; so does a domain given
	// over the file's loopback mode. A domain over its HTTPS mode keeps that mode.
	domain := len(o.Domains) > 0
	for _, k := range in.domainKeys() {
		_, set := o.Set[k]
		_, answered := o.Answers[k]
		domain = domain || set || answered
	}
	if o.Cert == "" && o.Key == "" && !(domain && st.TLS == Loopback) {
		o.TLS = cmp.Or(o.TLS, st.TLS)
	}
	o.HTTPPort = cmp.Or(o.HTTPPort, st.HTTPPort)
	o.HTTPSPort = cmp.Or(o.HTTPSPort, st.HTTPSPort)
	if o.IPv6 == nil && st.IPv6 {
		o.IPv6 = &st.IPv6
	}
	if len(st.Inputs) > 0 {
		answers := maps.Clone(st.Inputs)
		// Loopback chosen over the file serves no domain: the file's domains are not taken.
		if o.TLS == Loopback {
			for _, k := range in.domainKeys() {
				delete(answers, k)
			}
		}
		maps.Copy(answers, o.Answers)
		o.Answers = answers
	}
	return nil
}

// withSettings applies the settings file to a first install run by the operator; an install that
// exists already keeps what it recorded.
func (in *Installer) withSettings() error {
	if _, err := os.Stat(in.path(envName)); err == nil {
		return nil
	}
	path := cmp.Or(in.O.Settings, SettingsFile(in.R.Spec.Name))
	st, err := ReadSettings(path)
	if err != nil || st == nil {
		return err
	}
	return in.applySettings(path, st)
}

// awaitPreload waits until the preload unit has loaded the release's images, which an install on
// the VM runs: an operator's `install` can come before the boot has loaded them (`vm create` names
// it as soon as the VM answers over SSH). `systemctl start` of the oneshot returns at once once it
// ran, waits for it while it runs, and runs it again when it failed. A host with no such unit (or
// no systemd) has no boot loading them, and nothing is waited for. The boot's own install never
// comes here: its unit is ordered after the preload unit.
//
// A preload that fails does not refuse by itself: it is logged and kept (preloadErr), and the
// install's check of the images (fetchImages) decides — every image loaded goes on, a missing one
// is refused naming the unit and its journal.
func (in *Installer) awaitPreload(ctx context.Context) error {
	unit := preloadUnit(in.R.Spec.Name)
	st, err := unitState(ctx, in.O.Host, unit)
	switch {
	case errors.Is(err, exec.ErrNotFound), err == nil && st == noBootUnit:
		return nil
	case err != nil && !in.systemdBooted(ctx):
		return nil // systemctl, but systemd is not the init system (a container): no boot loads them
	case err != nil:
		return err
	}
	if st == "failed" && in.allPreloaded(ctx) {
		in.O.logf("%s failed, but every image of the release is loaded: it is not run again (`journalctl -u %s` tells why it failed)", unit, unit)
		return nil
	}
	in.O.logf("waiting for %s (loading the release's images)…", unit)
	if _, err := in.O.Host.Run(ctx, Cmd{Args: []string{"systemctl", "start", unit}}); err != nil {
		in.preloadErr = err
		in.O.logf("%s did not finish (%v): the install goes on if every image is loaded (`journalctl -u %s` tells why it failed)", unit, err, unit)
	}
	return nil
}

func preloadUnit(name string) string { return name + "-preload.service" }

// systemdBooted reports whether systemd runs the host (as sd_booted tells: /run/systemd/system is
// a directory).
func (in *Installer) systemdBooted(ctx context.Context) bool {
	_, err := in.O.Host.Run(ctx, Cmd{Args: []string{"test", "-d", "/run/systemd/system"}})
	return err == nil
}

// allPreloaded reports whether the engine holds every image of the release the OS preloads.
func (in *Installer) allPreloaded(ctx context.Context) bool {
	keys := in.R.Spec.ImageNames()
	for _, k := range []string{front.Key, chown.Key} { // the release's own images a start runs
		if in.R.Manifest.Artifacts.Images[k] != "" {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		if _, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"image", "inspect", "--format", "{{.Id}}", in.imageRef("", k)}}); err != nil {
			return false
		}
	}
	return true
}

// vmInstallReady is what the VM's `install` checks and waits for before anything else: that the
// install directory (or the nearest directory above it that exists) is writable (by root on the
// VM; euid tells which the refusal names), then the preload unit; a dry run checks the directory
// too, waits for nothing and says what a run would.
func (in *Installer) vmInstallReady(ctx context.Context, euid int, writable func(dir string) bool) error {
	dir := in.O.Dir
	for {
		if _, err := os.Stat(dir); err == nil || filepath.Dir(dir) == dir {
			break
		}
		dir = filepath.Dir(dir)
	}
	switch {
	case writable(dir):
	case euid == 0:
		return fmt.Errorf("%s is not writable, by root either (a read-only filesystem?): the install cannot be written there", dir)
	default:
		return fmt.Errorf("%s is not writable by this user: the VM's install runs as root, `sudo %s install`", dir, in.R.Ctl())
	}
	if in.O.DryRun {
		unit := preloadUnit(in.R.Spec.Name)
		if st, err := unitState(ctx, in.O.Host, unit); err == nil && st != noBootUnit && (st != "failed" || !in.allPreloaded(ctx)) {
			in.O.logf("%s is writable; a run would first wait for %s to load the release's images", dir, unit)
		} else {
			in.O.logf("%s is writable; a run would wait for no preload unit", dir)
		}
		return nil
	}
	return in.awaitPreload(ctx)
}

// dirWritable reports whether a file can be created in dir (created and removed at once).
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".stowage-probe-*")
	if err != nil {
		return false
	}
	f.Close()
	return os.Remove(f.Name()) == nil
}

// waiting lists the secret inputs a first install needs that nothing gives: the boot cannot ask
// them, so the install waits for `sudo <short>-stow install`. A key that names no input is refused
// first.
func (in *Installer) waiting(ctx context.Context) ([]string, error) {
	o := &in.O
	mode, _, err := in.mode(ctx, "")
	if err != nil {
		return nil, err
	}
	if err := in.knownInputs(mode); err != nil {
		return nil, err
	}
	var out []string
	for _, i := range in.inputList(mode) {
		_, set := o.Set[i.Key]
		_, answered := o.Answers[i.Key]
		if i.Secret && i.Required && i.Default == "" && !set && !answered && in.inputEnv(i.Key) == "" {
			out = append(out, i.Key)
		}
	}
	return out, nil
}

// firstBoot makes the VM's first install from the settings file at path, when its cloud-config
// wrote one; without it, or while a secret input is missing, the VM waits for `sudo <short>-stow
// install`.
func (in *Installer) firstBoot(ctx context.Context, path string) error {
	o, ctl := &in.O, in.R.Ctl()
	st, err := ReadSettings(path)
	if err != nil {
		return err
	}
	if st == nil {
		o.logf("%s holds no install yet, and no cloud-config gave %s: `sudo %s install` makes one", o.Dir, path, ctl)
		return nil
	}
	if t := st.Release; t != nil && (t.Version != in.R.Manifest.ProjectVersion || t.Revision != in.R.Manifest.Revision) {
		return in.switchOS(ctx, st)
	}
	if st.AwaitSecrets {
		o.logf("the first install waits for its secrets, which the cloud-config never carries: `sudo %s install --secrets FILE` gives them", ctl)
		return nil
	}
	if err := in.applySettings(path, st); err != nil {
		return err
	}
	missing, err := in.waiting(ctx)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		o.logf("the first install needs %s, which the cloud-config never carries: `sudo %s install` asks for it", strings.Join(missing, ", "), ctl)
		return nil
	}
	o.logf("installing %s %s with the settings in %s", in.R.Spec.Name, in.R.Manifest.ProjectVersion, path)
	return in.Install(ctx)
}

// Boot brings the VM's install up with the release this OS carries. With no install yet it makes
// the first one from the settings file the cloud-config wrote (firstBoot).
//
// An upgrade or a rollback a reboot stopped is finished first, by the install's own installer
// (see finishStopped). Then an install of this release comes up as a re-run of the install does.
// An install of the release the last upgrade replaced means the VM booted that release again (the
// A/B fallback): the install's own installer — the newer release's, which knows the newer schema —
// goes back, refusing as `rollback` does. An install that went back from this release stays as it
// is, brought up by its own installer (no upgrade loop). Any other install is upgraded to this
// release, after its own installer brought its stack up so the upgrade can dump the database.
// The install's own installer is run only when it carries the release the install runs, so a run
// never takes the same branch again.
func (in *Installer) Boot(ctx context.Context) error {
	o := &in.O
	if _, err := os.Stat(filepath.Join(o.Dir, envName)); errors.Is(err, os.ErrNotExist) {
		return in.firstBoot(ctx, cmp.Or(o.Settings, SettingsFile(in.R.Spec.Name)))
	}
	env, err := in.open()
	if err != nil {
		return err
	}
	m := in.R.Manifest
	name, cur, rev, err := sameProject(o.Dir, env, m)
	if err != nil {
		return err
	}
	if in.prevIs(cur, rev) {
		if env, err = in.finishStopped(ctx); err != nil {
			return err
		}
		_, cur, rev = runs(env)
	}
	if cur == m.ProjectVersion && rev == strconv.Itoa(m.Revision) {
		return in.Install(ctx)
	}
	if _, err := in.keptInstaller(name, cur, rev); err != nil {
		return err
	}
	switch {
	case in.prevIs(m.ProjectVersion, strconv.Itoa(m.Revision)):
		o.logf("this OS carries %s %s (revision %d), which the install's last upgrade replaced: going back to it", name, m.ProjectVersion, m.Revision)
		return o.exec(ctx, in.ctl(), []string{bootRollbackCmd, "--dir", o.Dir})
	case in.leftByRollback(m.ProjectVersion, m.Revision):
		o.logf("the install went back from %s %s (revision %d), which this OS carries: it stays on %s (revision %s); upgrade to a later release, or remove %s to try this one again",
			name, m.ProjectVersion, m.Revision, cur, rev, in.path(FailedDir))
		return o.exec(ctx, in.ctl(), []string{bootCmd, "--dir", o.Dir})
	}
	o.logf("the install runs %s %s (revision %s); this OS carries %s (revision %d): upgrading", name, cur, rev, m.ProjectVersion, m.Revision)
	if err := o.exec(ctx, in.ctl(), []string{bootCmd, "--dir", o.Dir}); err != nil {
		return fmt.Errorf("bringing %s %s up before the upgrade: %w", name, cur, err)
	}
	return in.Upgrade(ctx)
}

// finishStopped finishes an upgrade or a rollback a reboot stopped, which PrevDir recording the
// release the install runs gives away: an upgrade that kept the running release but did not
// switch .env yet, or a rollback that switched .env but did not finish. In both the install's own
// installer — the older release's after the first, the newer's after the second — finishes it as
// `rollback` finishes a stopped one: the kept installer back, PrevDir removed, the stack up.
func (in *Installer) finishStopped(ctx context.Context) (*EnvFile, error) {
	o := &in.O
	o.logf("finishing the upgrade or rollback a reboot stopped")
	if err := o.exec(ctx, in.ctl(), []string{bootRollbackCmd, "--dir", o.Dir}); err != nil {
		return nil, fmt.Errorf("finishing the upgrade or rollback a reboot stopped: %w", err)
	}
	env, err := in.open()
	if err != nil {
		return nil, err
	}
	_, cur, rev := runs(env)
	if in.prevIs(cur, rev) {
		return nil, fmt.Errorf("%s still records the release the install runs after %s finished", in.path(PrevDir), in.ctl())
	}
	return env, nil
}

// BootRollback is the install's own installer going back at boot (boot-rollback): the database
// is brought up first, so the rollback check reads the schema the newer release left. A database
// that does not come up refuses: going back unchecked could leave a release on a schema it cannot
// read. Finishing a stopped run checks nothing, and brings nothing up first.
func (in *Installer) BootRollback(ctx context.Context) error {
	env, err := in.open()
	if err != nil {
		return err
	}
	if err := in.checkDocker(ctx); err != nil {
		return err
	}
	if _, cur, rev := runs(env); in.prevIs(cur, rev) {
		return in.Rollback(ctx)
	}
	if svc := in.R.Spec.DB().Service; svc != "" {
		if _, err := in.Compose(ctx, env, in.O.Log, "up", "--detach", "--wait", svc); err != nil {
			return fmt.Errorf("the database did not come up, so going back cannot be checked: %w", err)
		}
	}
	return in.Rollback(ctx)
}

// leftByRollback reports whether the release the install's last rollback left, kept in FailedDir,
// is version at revision: booting it again must not upgrade to it again.
func (in *Installer) leftByRollback(version string, revision int) bool {
	env, err := ReadEnvFile(filepath.Join(in.path(FailedDir), envName))
	if err != nil {
		return false
	}
	_, v, rev := runs(env)
	return v == version && rev == strconv.Itoa(revision)
}

// prevIs reports whether the release kept in PrevDir is version at revision.
func (in *Installer) prevIs(version, revision string) bool {
	raw, err := os.ReadFile(filepath.Join(in.path(PrevDir), prevState))
	if err != nil {
		return false
	}
	var st state
	return json.Unmarshal(raw, &st) == nil && st.ProjectVersion == version && strconv.Itoa(st.Revision) == revision
}
