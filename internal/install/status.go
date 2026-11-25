package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
)

// InstallerVar records, in a Docker install's .env, the installer image of the release it runs by
// the digest the release manifest records: absent when the install did not read it (--offline, a
// registry that holds no release manifest). It is a release's value: an upgrade records the new
// release's, a rollback takes back the previous one's.
const InstallerVar = "STOWAGE_INSTALLER"

// InstallerOfVar is the release InstallerVar was recorded for (installerOf): the image is told only
// for that release, so one an installer that does not record it carried over names nothing.
const InstallerOfVar = "STOWAGE_INSTALLER_RELEASE"

// installerOf is a release's identity as the installer's record names it: version/revision,
// compared for equality alone.
func installerOf(version string, revision int) string { return version + "/" + strconv.Itoa(revision) }

// installerFor is ref when of names the release version at revision; "" otherwise.
func installerFor(ref, of, version string, revision int) string {
	if ref == "" || of != installerOf(version, revision) {
		return ""
	}
	return ref
}

// k8sInstallerKey is the value under the chart's stowage.release recording the same on Kubernetes,
// in the release's values: `helm rollback` takes it back with the revision.
const k8sInstallerKey = "installer"

// k8sInstallerOfKey is the release k8sInstallerKey was recorded for (installerOf), beside it.
const k8sInstallerOfKey = "installerRelease"

// StatusRelease is a release as `status --json` names it on Docker and Kubernetes: the VM's
// Version, and its installer image by digest when the install recorded it.
type StatusRelease struct {
	Version   string `json:"version"`
	Revision  int    `json:"revision"`
	Installer string `json:"installer,omitempty"`
}

// InstallStatus is what `status --json` prints on every shape, under the VM's Status names.
type InstallStatus struct {
	Format int    `json:"format"` // MachineFormat
	Name   string `json:"name"`
	// Installed is the release that runs; nil when none does (a Kubernetes release uninstalled with
	// --keep-history, which Previous then names).
	Installed *StatusRelease `json:"installed"`
	Channel   string         `json:"channel,omitempty"`
	// Installing: the first install has not finished; InstallFailed: why its last run failed.
	Installing    bool   `json:"installing,omitempty"`
	InstallFailed string `json:"install_failed,omitempty"`
	// Previous is the release `rollback` (no --revision) goes back to; nil when there is none. It is
	// never the revision Installed is read from; on Kubernetes it may be the same version and
	// revision of the project run with other values (an upgrade that changed only values).
	Previous *StatusRelease `json:"previous"`
	// Inputs are the keys of the inputs the install records, sorted ([] when none): never a value,
	// so the output can be logged. Nil (left out) when this installer cannot tell them: it carries
	// another release than the one installed, whose spec it does not have.
	Inputs *[]string `json:"inputs,omitempty"`
	// Registry is the prefix the install pulls its images from, as recorded: on Docker the one its
	// first install chose (--registry, else the release's), on Kubernetes the release's values'.
	// Empty (left out) when none is recorded.
	Registry string `json:"registry,omitempty"`
	// Values: with --values, what the install records that a configuration gives again
	// (recordedValues); left out without it, or when this installer cannot tell what is secret.
	Values *RecordedValues `json:"values,omitempty"`
}

// RecordedValues is what `status --json --values` adds: the recorded values a configuration
// gives again, never a secret's.
type RecordedValues struct {
	// Inputs are the recorded inputs, by key, that are not secret: the installed release's and
	// the recorded TLS mode's; a file input as sha256:<hex> of its content. {} when none.
	Inputs map[string]string `json:"inputs"`
	// TLS is the front's mode (none for loopback); Domains each route's domain in a TLS mode;
	// Ports each route's loopback port. Left out without a front, as on Kubernetes.
	TLS     string            `json:"tls,omitempty"`
	Domains map[string]string `json:"domains,omitempty"`
	Ports   map[string]int    `json:"ports,omitempty"`
	// HTTPPort and HTTPSPort are the host ports a TLS mode's front publishes (--http-port,
	// --https-port); left out in loopback mode.
	HTTPPort  int `json:"http_port,omitempty"`
	HTTPSPort int `json:"https_port,omitempty"`
	// ACMECA is the ACME directory the front's certificates come from instead of Let's Encrypt's
	// (--acme-ca), in the acme and dns-cloudflare modes; left out when none is recorded.
	ACMECA string `json:"acme_ca,omitempty"`
	// ACMECARoot is sha256:<hex> of the root the install holds for ACMECA (--acme-ca-root), as
	// a file input shows; left out when it holds none. ACMECARootError is why the root held
	// cannot be read (ACMECARoot then left out); left out when it can.
	ACMECARoot      string `json:"acme_ca_root,omitempty"`
	ACMECARootError string `json:"acme_ca_root_error,omitempty"`
	// PullLogin is whether the install holds a pull login (--registry-login-file,
	// `registry-login`; on Kubernetes the pull Secret's), never the login: false too when the
	// login held cannot be read, which PullLoginError says (on Docker and the VM as status's
	// registry_login_error; on Kubernetes kubectl's read of the pull Secret failing, or its login
	// not parsing); left out when it can.
	PullLogin      bool   `json:"pull_login"`
	PullLoginError string `json:"pull_login_error,omitempty"`
}

// recordedValues reads, through get, the values recorded that s, the installed release's spec,
// declares: its inputs and the recorded TLS mode's that are not secret, and with front its mode,
// each route's domain and the host ports the front publishes in a TLS mode, each front route's
// port in loopback mode. Nil when s is: what is secret is not known.
func recordedValues(s *spec.Spec, get func(string) (string, bool), front bool) *RecordedValues {
	if s == nil {
		return nil
	}
	out := &RecordedValues{Inputs: map[string]string{}}
	inputs := slices.Clone(s.Inputs)
	if front {
		out.TLS, _ = get("STOWAGE_TLS_MODE")
		// The recorded mode's own: one an earlier mode left recorded is not this mode's.
		for _, t := range tlsModeInputs {
			if slices.Contains(t.modes, out.TLS) {
				inputs = append(inputs, t.Input)
			}
		}
	}
	for _, i := range inputs {
		v, ok := get(i.Key)
		if !ok || i.Secret {
			continue
		}
		if isFileInput(i, v) {
			// Never the content: a value recorded as a file shows as its digest, whatever the
			// input is declared now; one that does not decode is left out (not known).
			if v, ok = fileContentDigest(v); !ok {
				continue
			}
		}
		out.Inputs[i.Key] = v
	}
	if !front || out.TLS == "" {
		return out
	}
	// A port recorded; none, or one that is no port (recordedPortOf), is left out: not known.
	port := func(k string) int {
		v, _ := get(k)
		n, _ := recordedPortOf(v)
		return n
	}
	if out.TLS == Loopback {
		// --ports moves the front's routes alone: an L4 route's host port is the spec's.
		for _, r := range s.Front.Routes {
			if n := port(PortVar(r.Name)); n > 0 {
				if out.Ports == nil {
					out.Ports = map[string]int{}
				}
				out.Ports[r.Name] = n
			}
		}
		return out
	}
	if acmeMode(out.TLS) {
		out.ACMECA, _ = get(ACMECAVar)
	}
	// The ports the front publishes in this mode, as the install binds them.
	http, https := frontServed(s.Front, s.L4.Routes, out.TLS)
	if http {
		out.HTTPPort = port(HTTPPortVar)
	}
	if https {
		out.HTTPSPort = port(HTTPSPortVar)
	}
	for _, d := range s.RouteDomains() {
		// Never a secret's, never a file's content: a domain input the project declares secret,
		// or a value recorded as a file, is left out.
		i, declared := s.Input(d.Input)
		if declared && i.Secret {
			continue
		}
		if v, ok := get(d.Input); ok && v != "" && !isFileInput(i, v) {
			if out.Domains == nil {
				out.Domains = map[string]string{}
			}
			out.Domains[d.Route] = v
		}
	}
	return out
}

// heldValues adds to v what the install holds beside its recorded values: whether it holds a
// pull login (loginHost and loginProblem, heldLoginHost's, read once by the status), and the
// digest of its ACME root, or why each cannot be read. Nil v is left.
func (in *Installer) heldValues(v *RecordedValues, loginHost, loginProblem string) {
	if v == nil {
		return
	}
	v.PullLogin, v.PullLoginError = loginHost != "", loginProblem
	raw, err := in.heldACMERoot(v.TLS, v.ACMECA)
	switch {
	case err != nil:
		v.ACMECARootError = err.Error()
	case raw != nil:
		sum := sha256.Sum256(raw)
		v.ACMECARoot = "sha256:" + hex.EncodeToString(sum[:])
	}
}

// DockerStatus is `status --json` on Docker: the stack's services too, as `ps` reads them, or why
// they could not be read.
type DockerStatus struct {
	InstallStatus
	FirstInstall
	// RestoreAwaited: made again by --secrets, the install awaits its data (madeAgainRecords): no
	// hook runs, no backup is taken, until restore, provision or reset-db;
	// VolumesSplit: a restore left the volumes half restored (or one runs). Docker's as the VM's.
	RestoreAwaited bool `json:"restore_awaited,omitempty"`
	VolumesSplit   bool `json:"volumes_split,omitempty"`
	// MadeAgain: the install was made by --secrets, from another install's export.
	MadeAgain     bool      `json:"made_again,omitempty"`
	Services      []Service `json:"services"`
	ServicesError string    `json:"services_error,omitempty"`
	// RegistryLogin is the host the install holds a login for (`registry-login`,
	// --registry-login-file), the release's own registry's or its mirror's: never the login itself.
	// Empty when it holds none; RegistryLoginError when the file held cannot be read (the reads of
	// its host fail on it).
	RegistryLogin      string `json:"registry_login,omitempty"`
	RegistryLoginError string `json:"registry_login_error,omitempty"`
}

// HelmStatus is the helm release's latest revision and its status, as `helm list` gives it.
type HelmStatus struct {
	Revision int    `json:"revision"`
	Status   string `json:"status"`
}

// K8sReleaseStatus is `k8s status --json`: helm's view of the release too.
type K8sReleaseStatus struct {
	InstallStatus
	Helm HelmStatus `json:"helm"`
}

// readInstallerImage is the installer image of this installer's release by digest, as the
// release manifest in the registry at prefix records it (releaseInstallerImage, or the seam).
func (in *Installer) readInstallerImage(ctx context.Context, prefix string) (string, error) {
	if in.O.InstallerImage != nil {
		return in.O.InstallerImage(ctx, prefix)
	}
	return in.releaseInstallerImage(ctx, prefix)
}

// recordedImage is the installer image a read answered, as a deploy records it: nothing when the
// read failed (unrecorded says why).
func recordedImage(ref string, err error) string {
	if err != nil {
		return ""
	}
	return ref
}

// unrecorded says why the installer image is not recorded, when its read failed.
func (in *Installer) unrecorded(err error) {
	if err != nil {
		in.O.logf("the release's installer image is not recorded (status --json leaves it out): %v", err)
	}
}

// recordInstaller records in env the installer image of the release it installs (InstallerVar),
// read from the registry env pulls from; an install that never pulls (a VM's included, whose OS
// carries its release) records none.
func (in *Installer) recordInstaller(ctx context.Context, env *EnvFile) {
	env.Delete(InstallerVar)
	env.Delete(InstallerOfVar)
	prefix, _ := env.Get("STOWAGE_REGISTRY")
	if in.O.Offline || in.O.Preloaded || prefix == "" || !pinnedImages(env) {
		return
	}
	ref, err := in.readInstallerImage(ctx, prefix)
	in.unrecorded(err)
	if ref = recordedImage(ref, err); ref != "" {
		env.Set(InstallerVar, ref)
		env.Set(InstallerOfVar, installerOf(in.R.Manifest.ProjectVersion, in.R.Manifest.Revision))
	}
}

// InputsVar records, in a Docker install's .env (and the VM's), the keys of the inputs it records,
// comma-separated and sorted: the inputs of each release that wrote it, so a key only a newer
// release declares stays listed after a rollback, which keeps .env's keys. Not a value compose or
// a hook reads.
const InputsVar = "STOWAGE_INPUTS"

// recordInputKeys sets InputsVar in env, called by each run that writes .env with inputs added or
// dropped (install, re-run, upgrade, rollback, provision; a secret's rotation or set changes no
// key): the keys env records that are inputs of this release or
// that a prior record (the ones env was made from; none for a first install) lists.
func (in *Installer) recordInputKeys(env *EnvFile, priors ...*EnvFile) {
	keys := map[string]bool{}
	for _, prior := range priors {
		for _, k := range listedInputs(prior) {
			keys[k] = true
		}
	}
	for _, i := range in.R.Spec.Inputs {
		keys[i.Key] = true
	}
	var out []string
	for _, k := range sortedKeys(keys) {
		if _, ok := env.Get(k); ok {
			out = append(out, k)
		}
	}
	env.Set(InputsVar, strings.Join(out, ","))
}

// listedInputs are the keys InputsVar lists in env; none when it is absent (an install made
// before it was recorded) or empty.
func listedInputs(env *EnvFile) []string {
	v, _ := env.Get(InputsVar)
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

// DerivedVar records, in a Docker install's .env (and the VM's), the inputs whose recorded value
// is their default naming another input (${KEY}), never given (--set, the environment, --answers or
// an answer on the form), comma-separated and sorted: a re-run that changes the input such a
// default names expands it again (rederive). Not a value compose or a hook reads.
const DerivedVar = "STOWAGE_DERIVED"

// derivedInputs are the keys DerivedVar lists in env.
func derivedInputs(env *EnvFile) map[string]bool {
	out := map[string]bool{}
	if v, _ := env.Get(DerivedVar); v != "" {
		for _, k := range strings.Split(v, ",") {
			out[k] = true
		}
	}
	return out
}

// recordDerived sets DerivedVar in env to the keys of derived env records; none, no variable.
func recordDerived(env *EnvFile, derived map[string]bool) {
	var keys []string
	for _, k := range sortedKeys(derived) {
		if _, ok := env.Get(k); ok && derived[k] {
			keys = append(keys, k)
		}
	}
	env.Delete(DerivedVar)
	if len(keys) > 0 {
		env.Set(DerivedVar, strings.Join(keys, ","))
	}
}

// installInputs are the keys of the inputs env records: those s (the installed release's spec;
// nil when no installer of it is read) declares, and those InputsVar lists — every release's that
// wrote it —, sorted. Nil (unknown) when neither tells: no spec, and no list recorded.
func installInputs(env *EnvFile, s *spec.Spec) *[]string {
	_, listed := env.Get(InputsVar)
	if s == nil && !listed {
		return nil
	}
	out := []string{}
	if s != nil {
		out = *recordedInputs(s, func(k string) bool { _, ok := env.Get(k); return ok })
	}
	for _, k := range listedInputs(env) {
		if _, ok := env.Get(k); ok && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return &out
}

// recordedInputs is the keys of s's inputs recorded holds, sorted; [] when none (never null).
func recordedInputs(s *spec.Spec, recorded func(string) bool) *[]string {
	out := []string{}
	for _, i := range s.Inputs {
		if recorded(i.Key) && !slices.Contains(out, i.Key) {
			out = append(out, i.Key)
		}
	}
	slices.Sort(out)
	return &out
}

// isRelease reports whether m is the release name version at revision.
func isRelease(m *release.Manifest, name, version string, revision int) bool {
	return m.Name == name && isAt(m.ProjectVersion, m.Revision, version, strconv.Itoa(revision))
}

// isAt reports whether the release of version v at revision r is version at revision, the
// revision as an install records it (its decimal text): the one test of which release a record,
// a manifest or a kept state names.
func isAt(v string, r int, version, revision string) bool {
	return v == version && strconv.Itoa(r) == revision
}

// carriedRelease is the release name version at revision as an installer carrying it tells it — this
// one, else the one the install keeps — with its spec (nil when this Stowage does not read the
// kept one's); nil when neither carries it. No kept installer is said by the fields left out; one
// that does not read is logged.
func (in *Installer) carriedRelease(name, version string, revision int) (*release.Manifest, *spec.Spec) {
	m, s, err := in.readCarried(name, version, revision)
	if err != nil {
		in.O.logf("%v", err)
	}
	return m, s
}

// readCarried is carriedRelease, its error the kept installer's that did not read (a later read
// may not fail): none when it is absent, or carries another release. One that carries the release
// in a spec this Stowage does not read is logged, its manifest returned alone.
func (in *Installer) readCarried(name, version string, revision int) (*release.Manifest, *spec.Spec, error) {
	if isRelease(in.R.Manifest, name, version, revision) {
		return in.R.Manifest, in.R.Spec, nil
	}
	files, err := payload.Read(in.ctl())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	var k *keptRelease
	if err == nil {
		k, err = keptOf(files)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("the installer the install keeps, %s: %w", in.ctl(), err)
	}
	if !isRelease(k.Manifest, name, version, revision) {
		return nil, nil, nil
	}
	if k.Release == nil {
		in.O.logf("the installer the install keeps, %s: %v", in.ctl(), k.unread)
		return k.Manifest, nil, nil
	}
	return k.Manifest, k.Release.Spec, nil
}

// StatusJSON prints the Docker install's status as one JSON object (DockerStatus).
func (in *Installer) StatusJSON(ctx context.Context) error {
	env, err := in.open()
	if err != nil {
		return err
	}
	name, v, rev := runs(env)
	n, err := strconv.Atoi(rev)
	if err != nil {
		return fmt.Errorf("%s: STOWAGE_REVISION %q is not a number", in.path(envName), rev)
	}
	ref, _ := env.Get(InstallerVar)
	of, _ := env.Get(InstallerOfVar)
	st := DockerStatus{InstallStatus: InstallStatus{Format: MachineFormat, Name: name,
		Installed: &StatusRelease{Version: v, Revision: n, Installer: installerFor(ref, of, v, n)}}}
	st.Installing, st.InstallFailed, _ = in.firstInstall()
	st.RestoreAwaited = in.restoreAwaited()
	st.MadeAgain = fileExists(in.path(madeAgainFile))
	// As the VM's: not while a restore runs (it holds the mark from its database's swap to its last
	// volume's), which holds the install's hostLock — a backup refuses to start on split volumes.
	st.VolumesSplit = in.volumesSplit() != nil && !hostBusy(in.O.Dir)
	// The channel the install follows (--channel at the first install, as the VM's), else the
	// installed release's; its inputs from an installer that carries that release.
	st.Channel, _ = env.Get(ChannelVar)
	if st.Registry, st.FirstInstall, err = readFirstInstall(env, in.O.Dir); err != nil {
		return err
	}
	m, s := in.carriedRelease(name, v, n)
	if m != nil && st.Channel == "" {
		st.Channel = m.Channel
	}
	st.Inputs = installInputs(env, s)
	st.RegistryLogin, st.RegistryLoginError = in.heldLoginHost()
	if in.O.StatusValues {
		st.Values = recordedValues(s, env.Get, s != nil && s.HasFront())
		in.heldValues(st.Values, st.RegistryLogin, st.RegistryLoginError)
	}
	if st.Previous, err = in.keptPrevious(v, n); err != nil {
		return err
	}
	if st.Services, err = in.stackServices(ctx, env, nil); err != nil {
		st.ServicesError = err.Error()
	}
	return printJSON(in.O.Out, st)
}

// keptPrevious is the release `rollback` goes back to, the one PrevDir keeps; nil when it keeps
// none, or keeps the release installed (version at revision: an upgrade or a rollback that
// stopped, which `rollback` finishes on it). Its installer is the one its .env records for it.
func (in *Installer) keptPrevious(version string, revision int) (*StatusRelease, error) {
	prev := in.path(PrevDir)
	st, err := in.readPrevState()
	if st == nil || err != nil {
		return nil, err
	}
	if isAt(st.ProjectVersion, st.Revision, version, strconv.Itoa(revision)) {
		return nil, nil
	}
	r := &StatusRelease{Version: st.ProjectVersion, Revision: st.Revision}
	env, err := ReadEnvFile(filepath.Join(prev, envName))
	if err != nil {
		in.O.logf("the previous release's installer image is not known: %v", err)
		return r, nil
	}
	ref, _ := env.Get(InstallerVar)
	of, _ := env.Get(InstallerOfVar)
	r.Installer = installerFor(ref, of, st.ProjectVersion, st.Revision)
	return r, nil
}

// K8sStatusJSON prints the helm release's status as one JSON object (K8sReleaseStatus).
func (in *Installer) K8sStatusJSON(ctx context.Context) error {
	st, cur, err := in.k8sReleaseStatus(ctx, true)
	if err != nil {
		return err
	}
	r := cur.Release
	// The inputs, as the spec of the release installed tells them: this installer's only when it
	// carries that release.
	if st.Installed != nil && isRelease(in.R.Manifest, r.Name, r.Version, r.Revision) {
		data, err := in.k8sRecorded(ctx)
		// Another tool's Secret of the secret files' name says nothing of what is recorded.
		if foreign := (*foreignSecret)(nil); errors.As(err, &foreign) {
			data, err = in.readSecret(ctx, in.O.K8s.envSecret())
		}
		if err != nil {
			return err
		}
		st.Inputs = recordedInputs(in.R.Spec, func(key string) bool { _, ok := data[key]; return ok })
		if in.O.StatusValues {
			st.Values = recordedValues(in.R.Spec, func(key string) (string, bool) { v, ok := data[key]; return v, ok }, false)
			if st.Values != nil {
				held, broken, err := in.pullLoginHeld(ctx)
				st.Values.PullLogin = err == nil && held != nil
				switch {
				case err != nil:
					st.Values.PullLoginError = fmt.Sprintf("the pull Secret %s cannot be read: %v", in.O.K8s.pullSecret(), err)
				case broken != "":
					st.Values.PullLoginError = fmt.Sprintf("the pull Secret %s holds a login that does not parse: %s", in.O.K8s.pullSecret(), broken)
				}
			}
		}
	}
	return printJSON(in.O.Out, st)
}

// k8sReleaseStatus is what both forms of `k8s status` print: helm's status, the release installed
// (nil after an uninstall that kept its history) and, with previous or when none is installed, the
// one `rollback` brings back; with the latest revision's values it reads them from.
func (in *Installer) k8sReleaseStatus(ctx context.Context, previous bool) (K8sReleaseStatus, *deployed, error) {
	rel, err := in.k8sStatusRelease(ctx)
	if err != nil {
		return K8sReleaseStatus{}, nil, err
	}
	h, err := in.helmHistory(ctx)
	if err != nil {
		return K8sReleaseStatus{}, nil, err
	}
	// What `helm get values` gives is the latest revision's: read once, with the history's.
	var cur *deployed
	if n := h.latest(); n == 0 {
		cur, err = in.installedRelease(ctx)
	} else if raw, rerr := h.values(ctx, n); rerr != nil {
		err = rerr
	} else {
		cur, err = in.deployedIn(raw, fmt.Sprintf("helm get values --revision %d", n))
	}
	if err != nil {
		return K8sReleaseStatus{}, nil, err
	}
	r := cur.Release
	st := K8sReleaseStatus{InstallStatus: InstallStatus{Format: MachineFormat, Name: r.Name}, Helm: HelmStatus{Revision: h.latest(), Status: rel.Status}}
	// After `helm uninstall --keep-history` nothing runs: the latest revision is what `rollback`
	// brings back (previous), and nothing is installed.
	if len(h.revs) == 0 || h.revs[0].Status != "uninstalled" {
		st.Channel, st.Installing, st.Registry = r.Channel, rel.Status == "pending-install", cur.Registry
		st.Installed = &StatusRelease{Version: r.Version, Revision: r.Revision, Installer: installerFor(r.Installer, r.InstallerRelease, r.Version, r.Revision)}
	}
	if !previous && st.Installed != nil {
		return st, cur, nil
	}
	// The revision `rollback` goes back to: never the latest, but after an uninstall (when nothing
	// is installed); none before an upgrade.
	switch n, err := h.previous(ctx); {
	case errors.As(err, new(noPrevious)):
	case err != nil:
		return K8sReleaseStatus{}, nil, err
	default:
		if st.Previous, err = h.release(ctx, n); err != nil {
			return K8sReleaseStatus{}, nil, err
		}
	}
	return st, cur, nil
}

// release is the release revision n records in its values; nil when they record none of this
// project's.
func (h *releaseHistory) release(ctx context.Context, n int) (*StatusRelease, error) {
	raw, err := h.values(ctx, n)
	if err != nil {
		return nil, err
	}
	var v struct {
		Stowage *deployed `json:"stowage"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("helm get values --revision %d: %w", n, err)
	}
	if v.Stowage == nil || v.Stowage.Release.Name != h.in.R.Manifest.Name {
		return nil, nil
	}
	r := v.Stowage.Release
	return &StatusRelease{Version: r.Version, Revision: r.Revision, Installer: installerFor(r.Installer, r.InstallerRelease, r.Version, r.Revision)}, nil
}

// fileDigest is sha256:<hex> of the file at path; empty when there is none.
func fileDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// FirstInstall is what an install's first run recorded beside its registry, as status --json gives
// it on Docker and the VM: the <short> of the installer it keeps (<dir>/<short>-stow), the loopback
// address (--address, else 127.0.0.1), and sha256:<hex> of the registry CA it keeps (--registry-ca,
// as given; left out when it keeps none).
type FirstInstall struct {
	ShortName  string `json:"short_name,omitempty"`
	Address    string `json:"address,omitempty"`
	RegistryCA string `json:"registry_ca,omitempty"`
}

// readFirstInstall is the registry and the FirstInstall env records, the CA in dir hashed at each
// read (a small file: never a digest of one replaced since); err when that CA cannot be read.
func readFirstInstall(env *EnvFile, dir string) (registry string, f FirstInstall, err error) {
	registry, _ = env.Get("STOWAGE_REGISTRY")
	f.ShortName, _ = env.Get("STOWAGE_SHORT_NAME")
	f.Address, _ = env.Get("STOWAGE_LOOPBACK_ADDRESS")
	f.RegistryCA, err = fileDigest(filepath.Join(dir, RegistryCAFile))
	return registry, f, err
}
