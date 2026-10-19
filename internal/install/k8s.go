package install

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/helm"
	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/registry"
)

// K8sOptions is one run of `<short>-stow k8s`.
type K8sOptions struct {
	Namespace string // the release's namespace
	Release   string // the helm release's name
	Context   string // the kubeconfig context; empty is the current one
	// Values are the operator's chart values files, given to helm after the ones the installer
	// writes, so they win.
	Values []string
	// Registry is the prefix the chart and the images are pulled from (a mirror); empty is the
	// project's registry on an install, the one the release was installed from on an upgrade.
	Registry string
	Timeout  time.Duration
	Revision int // rollback: the helm revision to go back to; 0 is the one before the last upgrade
	// NewBaseline lets reset-db wipe a release that runs another schema baseline than this
	// installer's release, and install this one: the only way a release crosses a baseline.
	NewBaseline bool
	// Data lets uninstall delete the release's PersistentVolumeClaims without asking.
	Data    bool
	Helm    Tool
	Kubectl Tool
}

// MinHelm is the oldest helm the installer accepts: --reset-then-reuse-values.
var MinHelm = [3]int{3, 14, 0}

// maxChart bounds the chart archive read from the registry.
var maxChart int64 = 64 << 20

// managedBy labels the Secrets the installer writes.
const managedBy = "stowage"

// dnsLabel is a Kubernetes name of one DNS label (RFC 1123).
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// deployed is what a release of the chart records about itself in its values block.
type deployed struct {
	Registry string `json:"registry"`
	Release  struct {
		Name     string `json:"name"`
		Version  string `json:"version"`
		Revision int    `json:"revision"`
		Channel  string `json:"channel"`
		Tag      string `json:"tag"`
	} `json:"release"`
	// PublicKeys are the keys the release was installed with: an installer carrying none does
	// not upgrade it.
	// Raw: a value of another shape an operator set must not break every verb.
	PublicKeys json.RawMessage `json:"publicKeys"`
}

// listedKeys is the set of PEM keys (trimmed) a stowage.publicKeys value lists; a value of any
// other shape lists none.
func listedKeys(raw json.RawMessage) map[string]bool {
	var l []any
	if json.Unmarshal(raw, &l) != nil {
		return nil
	}
	out := map[string]bool{}
	for _, k := range l {
		s, ok := k.(string)
		if !ok {
			s = fmt.Sprint(k) // listed, matching no key
		}
		out[strings.TrimSpace(s)] = true
	}
	return out
}

// helmRelease is a line of `helm list`.
type helmRelease struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	AppVersion string `json:"app_version"`
}

// helmRevision is a line of `helm history`.
type helmRevision struct {
	Revision    int    `json:"revision"`
	Status      string `json:"status"`
	Chart       string `json:"chart"`
	Description string `json:"description"`
}

// rolledBackTo matches the description helm gives a revision a rollback made.
var rolledBackTo = regexp.MustCompile(`^Rollback to ([0-9]+)$`)

// previousRevision is the revision a rollback goes back to. After a revision that did not deploy (a
// failed upgrade or rollback helm left), the latest one that deployed: what ran before it. Otherwise
// one upgrade back: the latest revision that deployed before the one the release runs came from (a
// revision a rollback made comes from the one it copies) and that ran other values, so neither a
// failed revision nor a copy of what runs (a rollback's, or an upgrade that changed nothing) is
// chosen.
func (in *Installer) previousRevision(ctx context.Context) (int, error) {
	k := in.O.K8s
	out, err := in.helm(ctx, Cmd{Args: []string{"history", k.Release, "--max", "1000", "--output", "json"}})
	if err != nil {
		return 0, err
	}
	var hs []helmRevision
	if err := json.Unmarshal(out, &hs); err != nil {
		return 0, fmt.Errorf("helm history: %w", err)
	}
	if len(hs) == 0 {
		return 0, fmt.Errorf("helm history lists no revision of %s in %s", k.Release, k.Namespace)
	}
	slices.SortFunc(hs, func(a, b helmRevision) int { return cmp.Compare(b.Revision, a.Revision) }) // newest first
	latest := hs[0]
	if latest.Status == "uninstalled" { // `helm uninstall --keep-history`: what ran until then
		return latest.Revision, nil
	}
	if !deployedOnce(latest) {
		for _, h := range hs[1:] {
			if deployedOnce(h) {
				return h.Revision, nil
			}
		}
		return 0, fmt.Errorf("release %s in %s has no revision that deployed (`helm history %s --namespace %s` lists them)", k.Release, k.Namespace, k.Release, k.Namespace)
	}
	byRev := map[int]helmRevision{}
	for _, h := range hs {
		byRev[h.Revision] = h
	}
	// Each step goes to a lower revision, so the walk ends; a revision helm no longer keeps ends it.
	from := latest.Revision
	for {
		h, ok := byRev[from]
		m := rolledBackTo.FindStringSubmatch(h.Description)
		if !ok || m == nil {
			break
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n >= from {
			break
		}
		from = n
	}
	var running []byte
	for _, h := range hs {
		if h.Revision >= from || !deployedOnce(h) {
			continue
		}
		if h.Chart == latest.Chart {
			if running == nil {
				if running, err = in.revisionValues(ctx, latest.Revision); err != nil {
					return 0, err
				}
			}
			v, err := in.revisionValues(ctx, h.Revision)
			if err != nil {
				return 0, err
			}
			if bytes.Equal(v, running) {
				continue
			}
		}
		return h.Revision, nil
	}
	return 0, fmt.Errorf("release %s in %s has no revision that ran something else before revision %d, the one it runs comes from (`helm history %s --namespace %s` lists them; --revision N picks one)",
		k.Release, k.Namespace, from, k.Release, k.Namespace)
}

// deployedOnce: the revision deployed (superseded: deployed, then replaced; deployed: still runs,
// as after a failed upgrade helm did not roll back).
func deployedOnce(h helmRevision) bool { return h.Status == "superseded" || h.Status == "deployed" }

// revisionValues is every value a revision ran with, as canonical JSON.
func (in *Installer) revisionValues(ctx context.Context, rev int) ([]byte, error) {
	out, err := in.helm(ctx, Cmd{Args: []string{"get", "values", in.O.K8s.Release, "--revision", strconv.Itoa(rev), "--all", "--output", "json"}})
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(out))
	d.UseNumber() // numbers compared as written, not as float64
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, fmt.Errorf("helm get values --revision %d: %w", rev, err)
	}
	return json.Marshal(v) // map keys sorted, no spacing
}

// envSecret names the Secret holding the release's secrets and inputs, as KEY: value.
func (k K8sOptions) envSecret() string { return k.Release + "-stowage" }

// pullSecret names the Secret holding the pull account, as a Docker config.
func (k K8sOptions) pullSecret() string { return k.Release + "-stowage-pull" }

// k8sCtl is the command line of a k8s verb on this release.
func (in *Installer) k8sCtl(verb string) string {
	return in.R.Ctl() + " k8s " + verb + k8sTarget(in.O.K8s)
}

// k8sTarget is the flags naming the release, as an operator gives them again.
func k8sTarget(k K8sOptions) string {
	s := " --namespace " + k.Namespace + " --release " + k.Release
	if k.Context != "" {
		s += " --kube-context " + shellQuote(k.Context)
	}
	return s
}

// checkTarget refuses a namespace or a release name Kubernetes would refuse, and names that leave
// no room for the Secrets named after the release.
func (in *Installer) checkTarget() error {
	k := in.O.K8s
	if len(k.Namespace) > 63 || !dnsLabel.MatchString(k.Namespace) {
		return fmt.Errorf("--namespace %q: a namespace is a DNS label (lower-case letters, digits, '-'; at most 63)", k.Namespace)
	}
	if len(k.Release) > 53 || !dnsLabel.MatchString(k.Release) {
		return fmt.Errorf("--release %q: a release name is a DNS label of at most 53 characters (lower-case letters, digits, '-')", k.Release)
	}
	return nil
}

func (in *Installer) helm(ctx context.Context, c Cmd) ([]byte, error) {
	k := in.O.K8s
	c.Args = append(c.Args, "--namespace", k.Namespace)
	if k.Context != "" {
		c.Args = append(c.Args, "--kube-context", k.Context)
	}
	return k.Helm.Run(ctx, c)
}

func (in *Installer) kubectl(ctx context.Context, c Cmd) ([]byte, error) {
	k := in.O.K8s
	c.Args = append(c.Args, "--namespace", k.Namespace)
	if k.Context != "" {
		c.Args = append(c.Args, "--context", k.Context)
	}
	return k.Kubectl.Run(ctx, c)
}

// checkHelm requires helm ≥ MinHelm and returns its major version.
func (in *Installer) checkHelm(ctx context.Context) (int, error) {
	out, err := in.O.K8s.Helm.Run(ctx, Cmd{Args: []string{"version", "--template", "{{.Version}}"}})
	if err != nil {
		return 0, fmt.Errorf("helm is required: %w", err)
	}
	v, ok := parseVersion(string(out))
	if !ok {
		return 0, fmt.Errorf("helm version %q could not be read", strings.TrimSpace(string(out)))
	}
	if older(v, MinHelm) {
		return 0, fmt.Errorf("helm %d.%d.%d is older than %d.%d.%d, the oldest this installer runs", v[0], v[1], v[2], MinHelm[0], MinHelm[1], MinHelm[2])
	}
	return v[0], nil
}

// release returns the helm release, in any status, nil when the namespace has none of that name.
// Each status is named: helm 3 lists deployed and failed releases by default, and helm 4 has no --all.
func (in *Installer) release(ctx context.Context) (*helmRelease, error) {
	k := in.O.K8s
	out, err := in.helm(ctx, Cmd{Args: []string{"list", "--filter", "^" + regexp.QuoteMeta(k.Release) + "$",
		"--deployed", "--failed", "--pending", "--superseded", "--uninstalled", "--uninstalling", "--output", "json"}})
	if err != nil {
		return nil, err
	}
	var rs []helmRelease
	if err := json.Unmarshal(out, &rs); err != nil {
		return nil, fmt.Errorf("helm list: %w", err)
	}
	for _, r := range rs {
		if r.Name == k.Release {
			return &r, nil
		}
	}
	return nil, nil
}

// installedRelease reads what the release runs from its values; an error when it was not installed
// from a chart of this project.
func (in *Installer) installedRelease(ctx context.Context) (*deployed, error) {
	k := in.O.K8s
	out, err := in.helm(ctx, Cmd{Args: []string{"get", "values", k.Release, "--all", "--output", "json"}})
	if err != nil {
		return nil, err
	}
	var v struct {
		Stowage *deployed `json:"stowage"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("helm get values: %w", err)
	}
	if v.Stowage == nil || v.Stowage.Release.Name == "" {
		return nil, fmt.Errorf("release %s in %s was not installed from a chart Stowage published", k.Release, k.Namespace)
	}
	if v.Stowage.Release.Name != in.R.Manifest.Name {
		return nil, fmt.Errorf("release %s in %s runs %q, not %q", k.Release, k.Namespace, v.Stowage.Release.Name, in.R.Manifest.Name)
	}
	return v.Stowage, nil
}

// k8sInputs is every input an install on Kubernetes records: the project's that apply to the
// kubernetes shape and to no front mode (a front is Docker's).
func (in *Installer) k8sInputs() []input {
	var out []input
	for _, i := range in.R.Spec.Inputs {
		if i.OnKubernetes() {
			out = append(out, input{Input: i})
		}
	}
	return out
}

// readSecret returns the data of a Secret the installer wrote; nil when there is none. A Secret of
// that name it did not write is refused: it is never written over.
func (in *Installer) readSecret(ctx context.Context, name string) (map[string]string, error) {
	out, err := in.kubectl(ctx, Cmd{Args: []string{"get", "secret", name, "--ignore-not-found", "--output", "json"}})
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	var s struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Data map[string][]byte `json:"data"`
	}
	if err := json.Unmarshal(out, &s); err != nil {
		return nil, fmt.Errorf("secret %s: %w", name, err)
	}
	want := in.secretLabels()
	// Who wrote it first: another tool's Secret is named as such, not as another release's.
	for _, key := range []string{"app.kubernetes.io/managed-by", "app.kubernetes.io/part-of", "app.kubernetes.io/instance"} {
		if got, want := s.Metadata.Labels[key], want[key]; got != want {
			return nil, fmt.Errorf("secret %s in %s was not written by %s for release %s (label %s is %q, not %q): it is not written over", name, in.O.K8s.Namespace, in.R.Ctl(), in.O.K8s.Release, key, got, want)
		}
	}
	data := map[string]string{}
	for k, v := range s.Data {
		data[k] = string(v)
	}
	return data, nil
}

// k8sObject is a Kubernetes object the installer applies.
type k8sObject struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   k8sMeta           `json:"metadata"`
	Type       string            `json:"type,omitempty"`
	Data       map[string][]byte `json:"data,omitempty"`
}

type k8sMeta struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

func (in *Installer) secretObject(name, typ string, data map[string][]byte) k8sObject {
	k := in.O.K8s
	return k8sObject{APIVersion: "v1", Kind: "Secret", Type: typ, Data: data, Metadata: k8sMeta{Name: name, Namespace: k.Namespace, Labels: in.secretLabels()}}
}

// secretLabels mark a Secret the installer writes, for this project's release: one of its names
// without all of them is never written over or read.
func (in *Installer) secretLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": managedBy,
		"app.kubernetes.io/part-of":    in.R.Spec.Name,
		"app.kubernetes.io/instance":   in.O.K8s.Release,
	}
}

// dockerAuths is a pull account as a kubernetes.io/dockerconfigjson Secret holds it.
func dockerAuths(host, user, token string) ([]byte, error) {
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + token))
	return json.Marshal(map[string]any{"auths": map[string]any{host: map[string]string{"username": user, "password": token, "auth": auth}}})
}

// deploy is a plan the installer applies: the Secrets, then the chart.
type deploy struct {
	namespace bool // the namespace is created
	env       map[string]string
	secret    map[string]bool // keys of env never printed
	changed   []string        // keys of env this run adds or changes
	pull      []byte          // the pull Secret's Docker config; nil writes none
	values    []byte          // the values file the installer gives helm
}

// K8sInstall installs the release the installer carries as a new helm release: its namespace
// and Secrets, then `helm upgrade --install` with the chart pulled by the digest the release
// records, rolled back by helm when it does not become ready. A Secret left by an install that
// stopped is kept, its values never asked or generated again.
func (in *Installer) K8sInstall(ctx context.Context) error {
	return in.k8sDeploy(ctx, false)
}

// K8sUpgrade moves the helm release to the release the installer carries: refused when the
// release runs a later one. The same release again re-applies it with the values given (how a
// release is reconfigured). An input the release adds is asked; every recorded value is kept.
func (in *Installer) K8sUpgrade(ctx context.Context) error {
	return in.k8sDeploy(ctx, true)
}

func (in *Installer) k8sDeploy(ctx context.Context, upgrade bool) error {
	in.givenCA = true
	o, k, m := &in.O, in.O.K8s, in.R.Manifest
	if err := in.checkTarget(); err != nil {
		return err
	}
	if m.Artifacts.Chart == nil {
		return fmt.Errorf("%s %s has no chart (published without the kubernetes shape): it is not installed on Kubernetes", m.Name, m.ProjectVersion)
	}
	for _, f := range k.Values {
		if _, err := os.Stat(f); err != nil {
			return fmt.Errorf("--values: %w", err)
		}
	}
	major, err := in.checkHelm(ctx)
	if err != nil {
		return err
	}
	if _, err := k.Kubectl.Run(ctx, Cmd{Args: []string{"version", "--client"}}); err != nil {
		return fmt.Errorf("kubectl is required: %w", err)
	}
	rel, err := in.release(ctx)
	if err != nil {
		return err
	}
	prefix := cmp.Or(k.Registry, in.R.Spec.Registry.Prefix)
	switch {
	case !upgrade && rel != nil:
		return fmt.Errorf("release %s in %s exists (%s, %s): `%s` moves it to %s", k.Release, k.Namespace, rel.Status, rel.AppVersion, in.k8sCtl("upgrade"), m.ProjectVersion)
	case upgrade && rel == nil:
		return fmt.Errorf("no release %s in %s: `%s` installs %s", k.Release, k.Namespace, in.k8sCtl("install"), m.ProjectVersion)
	case upgrade:
		if err := in.pending(rel); err != nil {
			return err
		}
		cur, err := in.installedRelease(ctx)
		if err != nil {
			return err
		}
		if err := in.k8sNewer(cur); err != nil {
			return err
		}
		prefix = cmp.Or(k.Registry, cur.Registry)
		// The same release applied again shares its baseline: nothing to fetch.
		if cur.Release.Version != m.ProjectVersion || cur.Release.Revision != m.Revision {
			if from, to, ok := in.k8sBaselines(ctx, cur, prefix); ok && from != to {
				return fmt.Errorf("%s %s starts schema baseline %d, and release %s in %s runs %s %s, at baseline %d: its data cannot carry over, so it moves to %s only by a fresh install: `%s --new-baseline` wipes it and installs %s",
					m.Name, m.ProjectVersion, to, k.Release, k.Namespace, cur.Release.Name, cur.Release.Version, from, m.ProjectVersion, in.k8sCtl("reset-db"), m.ProjectVersion)
			}
		}
	}
	var d deploy
	if !upgrade {
		// Reading a namespace takes a cluster-wide grant an operator limited to it may not have:
		// one that cannot be read is taken to exist, and the Secrets' apply says when it does not.
		ns, err := in.kubectl(ctx, Cmd{Args: []string{"get", "namespace", k.Namespace, "--ignore-not-found", "--output", "name"}})
		if err != nil {
			o.logf("namespace %s could not be read (%v): taken to exist, it is not created", k.Namespace, err)
		}
		d.namespace = err == nil && len(bytes.TrimSpace(ns)) == 0
	}
	if d.env, d.secret, d.changed, err = in.k8sEnv(ctx, upgrade); err != nil {
		return err
	}
	user, token, err := in.pullAccount(prefix)
	if err != nil {
		return err
	}
	if token == "" && !upgrade && in.R.Spec.Registry.Pull.Name == "token" && prefix == in.R.Spec.Registry.Prefix {
		c, err := providers.Token(in.R.Spec.Registry.Pull)
		if err != nil {
			return err
		}
		return fmt.Errorf("the cluster pulls the images with the pull account %s, whose password this installer does not carry: set %s", c.User, c.TokenEnv)
	}
	if token != "" {
		host, _, _ := strings.Cut(prefix, "/")
		if d.pull, err = dockerAuths(host, user, token); err != nil {
			return err
		}
		if _, err := in.readSecret(ctx, k.pullSecret()); err != nil {
			return err
		}
	}
	vals := map[string]any{"registry": prefix, "secret": k.envSecret()}
	switch {
	case d.pull != nil:
		vals["pullSecret"] = k.pullSecret()
	case !upgrade:
		vals["pullSecret"] = ""
		// An upgrade with no password keeps the pull Secret the release names (reused values).
	}
	if d.values, err = yaml.Marshal(map[string]any{helm.ValuesKey: vals}); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "stowage-chart-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	chart := filepath.Join(dir, "chart.tgz")
	if err := in.fetchChart(ctx, prefix, chart); err != nil {
		return err
	}
	valuesFile := filepath.Join(dir, "stowage-values.yaml")
	if err := os.WriteFile(valuesFile, d.values, 0o600); err != nil {
		return err
	}
	// The hook runs from the release's installer image: one the registry does not hold refuses
	// the run before anything changes.
	plan, err := in.k8sHookPlanFor(ctx, prefix)
	if err != nil {
		return err
	}
	args := helmDeploy(major, k, chart, valuesFile)
	if upgrade {
		// helm keeps the values given before, over the new chart's own: the operator's and the
		// installer's, never the release's images, which the chart's values carry.
		args = append(args, "--reset-then-reuse-values")
	}
	for _, f := range k.Values {
		args = append(args, "--values", f)
	}
	if o.DryRun {
		c := m.Artifacts.Chart
		return in.printDeploy(d, args, map[string]string{chart: "oci://" + prefix + "/" + c.Repository + "@" + c.Digest, valuesFile: "<the values above>"}, plan)
	}
	if err := in.applySecrets(ctx, d); err != nil {
		return err
	}
	o.logf("helm: %s %s %s (chart %s) in %s", map[bool]string{false: "installing", true: "upgrading to"}[upgrade], m.Name, m.ProjectVersion, m.Artifacts.Chart.Version, k.Namespace)
	if _, err := in.helm(ctx, Cmd{Args: args, Stream: o.Log}); err != nil {
		if upgrade {
			return fmt.Errorf("%w\nhelm rolled the release back to what it ran: `%s` shows it", err, in.k8sCtl("status"))
		}
		return fmt.Errorf("%w\nhelm removed the release; its Secret %s is kept, and a new `%s` reuses its values", err, k.envSecret(), in.k8sCtl("install"))
	}
	fmt.Fprintf(o.Out, "release %s in %s runs %s %s (revision %d)\n", k.Release, k.Namespace, m.Name, m.ProjectVersion, m.Revision)
	// The release runs: its endpoint and tips are printed whatever the hook does.
	err = in.k8sProvisioned(ctx, plan)
	in.k8sFinished(ctx, d.env, prefix)
	return err
}

// k8sFinished prints what a Kubernetes install or upgrade ends with: the project's endpoint
// inside the cluster, when it declares one, and a tip per extension whose input the release
// records empty.
func (in *Installer) k8sFinished(ctx context.Context, env map[string]string, prefix string) {
	if url := in.k8sEndpoint(); url != "" {
		fmt.Fprintf(in.O.Out, "endpoint  %s (the URL a project using %s as an extension is given)\n", url, in.R.Spec.Name)
	}
	in.extensionTips(ctx, onK8s, prefix, func(key string) (string, bool) {
		if i, ok := in.R.Spec.Input(key); !ok || !i.OnKubernetes() {
			return "", false
		}
		return env[key], true
	})
}

// helmDeploy is the helm command that installs or upgrades the release with chart, waiting for it
// and rolling it back when it is not ready: `--atomic` on helm 3, `--rollback-on-failure` on 4.
func helmDeploy(major int, k K8sOptions, chart, values string) []string {
	args := []string{"upgrade", "--install", k.Release, chart, "--wait", "--timeout", k.Timeout.String(), "--values", values}
	if major >= 4 {
		return append(args, "--rollback-on-failure")
	}
	return append(args, "--atomic")
}

// pending refuses a release helm is working on, or left half done.
func (in *Installer) pending(rel *helmRelease) error {
	if !strings.HasPrefix(rel.Status, "pending-") {
		return nil
	}
	k := in.O.K8s
	end := "helm rollback"
	if rel.Status == "pending-install" {
		end = "helm uninstall" // no earlier revision to go back to; the Secrets are kept
	}
	return fmt.Errorf("release %s in %s is %s: another helm operation runs on it, or one stopped before it finished (`%s %s --namespace %s` ends it)", k.Release, k.Namespace, rel.Status, end, k.Release, k.Namespace)
}

// k8sNewer refuses a release that comes before the one the helm release runs, by the scheme the
// release records; the same release passes (it is applied again).
func (in *Installer) k8sNewer(cur *deployed) error {
	m := in.R.Manifest
	if m.Scheme.Provider == "command" {
		if cur.Release.Version != m.ProjectVersion {
			in.O.logf("%s orders its versions with its own command, which an install does not run: not checking %s comes after %s", m.Name, m.ProjectVersion, cur.Release.Version)
		}
		return nil
	}
	c, err := compareTo(m, cur.Release.Version)
	if err != nil {
		return err
	}
	if cmp.Or(c, cmp.Compare(cur.Release.Revision, m.Revision)) > 0 {
		return fmt.Errorf("release %s in %s runs %s %s (revision %d), after this installer's %s (revision %d): `%s` goes back one upgrade",
			in.O.K8s.Release, in.O.K8s.Namespace, m.Name, cur.Release.Version, cur.Release.Revision, m.ProjectVersion, m.Revision, in.k8sCtl("rollback"))
	}
	return nil
}

// k8sEnv is the Secret of the release's secrets and inputs: what it holds, with every input it does
// not hold resolved and every secret it does not hold generated. A first install takes --set over
// a value an earlier run recorded; an upgrade sets only an input the release adds.
func (in *Installer) k8sEnv(ctx context.Context, upgrade bool) (env map[string]string, secret map[string]bool, changed []string, err error) {
	o, k := &in.O, in.O.K8s
	rec, err := in.readSecret(ctx, k.envSecret())
	if err != nil {
		return nil, nil, nil, err
	}
	recorded := NewEnvFile()
	for _, key := range sortedKeys(rec) {
		recorded.Set(key, rec[key])
	}
	all := in.k8sInputs()
	var list []input
	for _, i := range all {
		_, set := o.Set[i.Key]
		_, ans := o.Answers[i.Key]
		if _, ok := rec[i.Key]; !ok || (!upgrade && (set || ans)) {
			list = append(list, i)
		}
	}
	if upgrade {
		err = in.onlyAdded(recorded, list)
	} else {
		err = in.knownK8sInputs(all)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	in.recorded = rec
	values, err := in.inputs(ctx, list, false)
	if err != nil {
		return nil, nil, nil, err
	}
	secret = map[string]bool{}
	for _, i := range all {
		secret[i.Key] = i.Secret
	}
	for _, s := range in.R.Spec.Secrets {
		secret[s.Key] = true
		if _, ok := values[s.Key]; ok {
			continue
		}
		if values[s.Key], err = generate(o.rand(), s.Gen); err != nil {
			return nil, nil, nil, fmt.Errorf("secret %s: %w", s.Key, err)
		}
	}
	rotationVars(in.R.Spec.Secrets, func(k string) bool { _, ok := rec[k]; return ok }, func(k, v string) { values[k] = v; secret[k] = true })
	for _, key := range sortedKeys(values) {
		if old, ok := rec[key]; !ok || old != values[key] {
			changed = append(changed, key)
		}
	}
	return values, secret, changed, nil
}

// knownK8sInputs refuses a --set or answers key that is no input of the release on Kubernetes.
func (in *Installer) knownK8sInputs(list []input) error {
	var keys, unknown []string
	for _, i := range list {
		keys = append(keys, i.Key)
	}
	for _, m := range []map[string]string{in.O.Set, in.O.Answers} {
		for _, key := range sortedKeys(m) {
			if !slices.Contains(keys, key) && !slices.Contains(unknown, key) {
				unknown = append(unknown, key)
			}
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("no input of this release on Kubernetes is named %s (inputs: %s)", strings.Join(unknown, ", "), cmp.Or(strings.Join(keys, ", "), "none"))
	}
	return nil
}

// fetchChart pulls the release's chart by the digest its manifest records into file.
func (in *Installer) fetchChart(ctx context.Context, prefix, file string) error {
	c := in.R.Manifest.Artifacts.Chart
	cl, err := in.registryClient(prefix)
	if err != nil {
		return err
	}
	ref, err := cl.Digest(prefix+"/"+c.Repository, c.Digest)
	if err != nil {
		return err
	}
	in.O.logf("pulling the chart %s %s (%s)", c.Repository, c.Version, registry.Short(c.Digest))
	data, err := cl.GetLayer(ctx, ref, helm.ConfigMediaType, helm.ContentMediaType, maxChart)
	if err != nil {
		return fmt.Errorf("chart: %w", err)
	}
	if data == nil {
		return fmt.Errorf("chart %s is not in the registry (a mirror holds the charts/ repositories too)", ref)
	}
	return os.WriteFile(file, data, 0o600)
}

// applySecrets creates the namespace when it is missing, then applies the Secrets, server-side.
func (in *Installer) applySecrets(ctx context.Context, d deploy) error {
	k := in.O.K8s
	var items []any
	if d.namespace {
		items = append(items, k8sObject{APIVersion: "v1", Kind: "Namespace", Metadata: k8sMeta{Name: k.Namespace}})
	}
	data := map[string][]byte{}
	for key, v := range d.env {
		data[key] = []byte(v)
	}
	items = append(items, in.secretObject(k.envSecret(), "Opaque", data))
	if d.pull != nil {
		items = append(items, in.secretObject(k.pullSecret(), "kubernetes.io/dockerconfigjson", map[string][]byte{".dockerconfigjson": d.pull}))
	}
	list, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
	if err != nil {
		return err
	}
	in.O.logf("kubectl: applying the Secrets %s", strings.Join(secretNames(d, k), ", "))
	_, err = in.kubectl(ctx, Cmd{Args: []string{"apply", "--server-side", "--field-manager", managedBy, "--filename", "-"}, Stdin: bytes.NewReader(list)})
	return err
}

func secretNames(d deploy, k K8sOptions) []string {
	names := []string{k.envSecret()}
	if d.pull != nil {
		names = append(names, k.pullSecret())
	}
	return names
}

// printDeploy prints what a run would apply: the Secrets' changed values (secrets redacted), the
// installer's values and the helm command, its files (which the run removes) named by shown.
func (in *Installer) printDeploy(d deploy, args []string, shown map[string]string, plan *k8sHookPlan) error {
	k, w := in.O.K8s, in.O.Out
	if d.namespace {
		fmt.Fprintf(w, "namespace %s: created\n", k.Namespace)
	}
	fmt.Fprintf(w, "secret %s:\n", k.envSecret())
	for _, key := range d.changed {
		v := d.env[key]
		if d.secret[key] && v != "" {
			v = "<redacted>"
		}
		fmt.Fprintf(w, "  %s=%s\n", key, v)
	}
	if d.pull != nil {
		fmt.Fprintf(w, "secret %s: the pull account (redacted)\n", k.pullSecret())
	}
	fmt.Fprintf(w, "values:\n%s", indent(string(d.values)))
	a := []string{"helm"}
	for _, s := range args {
		a = append(a, cmp.Or(shown[s], s))
	}
	a = append(a, "--namespace", k.Namespace)
	if k.Context != "" {
		a = append(a, "--kube-context", k.Context)
	}
	fmt.Fprintf(w, "%s\n", strings.Join(a, " "))
	if plan != nil {
		fmt.Fprintf(w, "then the provision hook, as the Job %s (the installer image %s)\n", k.hookJob(), plan.ctl)
	}
	return nil
}

func indent(s string) string {
	lines := strings.SplitAfter(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = "  " + l
		}
	}
	return strings.Join(lines, "")
}

// K8sRollback rolls the helm release back to a revision: by default the one before the last
// upgrade (previousRevision).
func (in *Installer) K8sRollback(ctx context.Context) error {
	o, k := &in.O, in.O.K8s
	if err := in.checkTarget(); err != nil {
		return err
	}
	if k.Revision < 0 {
		return fmt.Errorf("--revision %d: a helm revision is 1 or more (`helm history %s --namespace %s` lists them)", k.Revision, k.Release, k.Namespace)
	}
	if _, err := in.checkHelm(ctx); err != nil {
		return err
	}
	rel, err := in.release(ctx)
	if err != nil {
		return err
	}
	if rel == nil {
		return fmt.Errorf("no release %s in %s", k.Release, k.Namespace)
	}
	if err := in.pending(rel); err != nil {
		return err
	}
	if _, err := in.installedRelease(ctx); err != nil {
		return err
	}
	to := k.Revision
	if to == 0 {
		if to, err = in.previousRevision(ctx); err != nil {
			return err
		}
	}
	args := []string{"rollback", k.Release, strconv.Itoa(to), "--wait", "--timeout", k.Timeout.String()}
	if o.DryRun {
		args = append(args, "--dry-run")
	}
	o.logf("helm: rolling %s in %s back to revision %d", k.Release, k.Namespace, to)
	if _, err := in.helm(ctx, Cmd{Args: args, Stream: o.Log}); err != nil {
		return err
	}
	if o.DryRun {
		return nil
	}
	cur, err := in.installedRelease(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "release %s in %s runs %s %s (revision %d)\n", k.Release, k.Namespace, cur.Release.Name, cur.Release.Version, cur.Release.Revision)
	return nil
}

// K8sStatus prints what the helm release runs, then helm's own status.
func (in *Installer) K8sStatus(ctx context.Context) error {
	o, k := &in.O, in.O.K8s
	if err := in.checkTarget(); err != nil {
		return err
	}
	rel, err := in.release(ctx)
	if err != nil {
		return err
	}
	if rel == nil {
		return fmt.Errorf("no release %s in %s", k.Release, k.Namespace)
	}
	cur, err := in.installedRelease(ctx)
	if err != nil {
		return err
	}
	r := cur.Release
	fmt.Fprintf(o.Out, "%s %s (revision %d, %s)\nregistry  %s\n", r.Name, r.Version, r.Revision, r.Channel, cur.Registry)
	if url := in.k8sEndpoint(); url != "" {
		fmt.Fprintf(o.Out, "endpoint  %s\n", url)
	}
	_, err = in.helm(ctx, Cmd{Args: []string{"status", k.Release}, Stdout: o.Out})
	return err
}

// K8sUpgradeTo moves the helm release to version v, on the channel its release came from: it
// fetches that release's installer from the registry the release was installed from (or the
// --image-registry mirror), checks it carries v, and runs its `k8s upgrade` with args. Among v's
// revisions the latest one not yanked is taken.
func (in *Installer) K8sUpgradeTo(ctx context.Context, v string, args []string) error {
	in.givenCA = true
	k := in.O.K8s
	if err := in.checkTarget(); err != nil {
		return err
	}
	if _, err := in.checkHelm(ctx); err != nil {
		return err
	}
	rel, err := in.release(ctx)
	if err != nil {
		return err
	}
	if rel == nil {
		return fmt.Errorf("no release %s in %s: `%s` installs %s", k.Release, k.Namespace, in.k8sCtl("install"), in.R.Manifest.ProjectVersion)
	}
	if err := in.pending(rel); err != nil {
		return err
	}
	cur, err := in.installedRelease(ctx)
	if err != nil {
		return err
	}
	prefix := cmp.Or(k.Registry, cur.Registry)
	c, err := in.registryClient(prefix)
	if err != nil {
		return err
	}
	name, r := cur.Release.Name, cur.Release
	// The keys of the installer run, narrowed to those the release's values list when they list
	// any: an installer of an earlier release does not bring back a key a later one dropped, and
	// values (which an operator's --values can set) never add a key. An installer with none does
	// not upgrade a release whose values list some.
	keys := in.R.Keys
	if listed := listedKeys(cur.PublicKeys); len(listed) > 0 {
		keys = map[string][]byte{}
		for n, k := range in.R.Keys {
			if listed[strings.TrimSpace(string(k))] {
				keys[n] = k
			}
		}
		if len(keys) == 0 {
			return fmt.Errorf("release %s in %s trusts signing keys this %s does not carry: run the installer of the release it runs, or a later one", k.Release, k.Namespace, in.R.Ctl())
		}
	}
	trust, err := trusted("this installer", keys)
	if err != nil {
		return err
	}
	m, err := findRelease(ctx, c, prefix, name, r.Channel, v, trust, in.O.logf)
	if err != nil {
		return err
	}
	order, err := compareTo(m, r.Version)
	if err != nil {
		return err
	}
	switch {
	case order > 0 || (order == 0 && r.Revision > m.Revision):
		return fmt.Errorf("release %s in %s runs %s %s (revision %d), after %s %s (revision %d): `%s` goes back one upgrade", k.Release, k.Namespace, name, r.Version, r.Revision, name, m.ProjectVersion, m.Revision, in.k8sCtl("rollback"))
	case order == 0 && r.Revision == m.Revision:
		fmt.Fprintf(in.O.Out, "release %s in %s already runs %s %s (revision %d)\n", k.Release, k.Namespace, name, r.Version, r.Revision)
		return nil
	}
	in.O.logf("fetching the installer of %s %s (revision %d, %s)", name, m.ProjectVersion, m.Revision, m.Tag)
	exe, err := in.fetchInstaller(ctx, c, prefix, m)
	if err != nil {
		return err
	}
	if err := in.O.exec(ctx, exe, append([]string{"k8s", "upgrade"}, args...)); err != nil {
		in.O.logf("the fetched installer is kept: %s", exe)
		return err
	}
	if err := os.RemoveAll(filepath.Dir(exe)); err != nil {
		in.O.logf("the fetched installer could not be removed: %v", err)
	}
	return nil
}

// k8sBaselines are the schema baselines of the release the helm release runs (from) and of this
// installer's (to). A release's baseline is in its spec, which travels in its installer: the
// installer of the release the helm release runs is fetched by the tag it was installed from,
// from prefix, signature-checked against this installer's keys, and read. ok is false when either
// has no database or a baseline cannot be told (said in the log): the check then does not refuse,
// as on Docker.
func (in *Installer) k8sBaselines(ctx context.Context, cur *deployed, prefix string) (int, int, bool) {
	if !in.R.Spec.HasDatabase() {
		return 0, 0, false
	}
	r := cur.Release
	unknown := func(err error) (int, int, bool) {
		in.O.logf("the schema baseline of %s %s is not known (%v): not checked", r.Name, r.Version, err)
		return 0, 0, false
	}
	if r.Tag == "" {
		return unknown(errors.New("the release records no tag"))
	}
	c, err := in.registryClient(prefix)
	if err != nil {
		return unknown(err)
	}
	repo, err := c.Repo(prefix + "/" + r.Name + "-release")
	if err != nil {
		return unknown(err)
	}
	m, h, err := readRelease(ctx, c, repo.String(), r.Tag)
	if err == nil && m == nil {
		err = fmt.Errorf("%s holds no release manifest", r.Tag)
	}
	if err != nil {
		return unknown(err)
	}
	trust, err := trusted("this installer", in.R.Keys)
	if err == nil {
		err = checkSigned(ctx, c, repo, h, m, trust)
	}
	if err != nil {
		return unknown(err)
	}
	// Fetched to the system's temporary directory: an operator's current one may be read-only.
	probe := &Installer{R: in.R, O: in.O}
	probe.O.Dir = os.TempDir()
	exe, err := probe.fetchInstaller(ctx, c, prefix, m)
	if err != nil {
		return unknown(err)
	}
	defer func() {
		if err := os.RemoveAll(filepath.Dir(exe)); err != nil {
			in.O.logf("the fetched installer could not be removed: %v", err)
		}
	}()
	files, err := payload.Read(exe)
	if err != nil {
		return unknown(err)
	}
	running, err := Load(files)
	if err != nil {
		return unknown(err)
	}
	return baselines(&in.O, &Installer{R: running, O: in.O}, in)
}
