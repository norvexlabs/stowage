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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/spec"
)

// The provision hook on Kubernetes: a Job the installer creates once the helm release is settled,
// waits for and deletes (k8shookpod.go runs inside it).
const (
	hookMount  = "/stowage"      // the pod's shared volume, in each container
	prevMount  = "/stowage-prev" // the outputs Secret, in the record container
	tokenMount = "/var/run/stowage"
	// hookUID runs the installer's own three containers (copy, check, record); the hook's runs as
	// its image says.
	hookUID = 65532
)

// Bounds on the Job beyond the hook's own provisionTimeout, and how the installer watches it.
var (
	k8sHookGrace = 5 * time.Minute // the Job's deadline past the hook's
	k8sHookPoll  = 2 * time.Second
	k8sHookStuck = 2 * time.Minute // a pod that cannot start (its image, its config) is given up after
	// k8sHookUnschedulable bounds a pod that no node takes: long enough for an autoscaler's node.
	k8sHookUnschedulable = 10 * time.Minute
)

// unschedulable starts why a pod no node takes is given up.
const unschedulable = "its pod cannot be scheduled: "

// hookJob names the provision Job: a Job's name is a pod label, at most 63 characters.
func (k K8sOptions) hookJob() string { return k.Release + "-stowage" }

// hookName names what the provision hook keeps between runs: the Secret of what it returned, the
// ServiceAccount (and its Role and RoleBinding) that may patch that Secret alone, and the
// ConfigMap the chart may render to give the hook more environment.
func (k K8sOptions) hookName() string { return k.Release + "-stowage-provision" }

// hookImageOf is the release's installer image, by the digest the release manifest in the registry
// at prefix records under the release's tag: the installer runs from its own image in the Job. The
// manifest must be this installer's release (its version and revision) and signed by a key it
// carries.
func (in *Installer) hookImageOf(ctx context.Context, prefix string) (string, error) {
	m := in.R.Manifest
	c, err := in.registryClient(prefix)
	if err != nil {
		return "", err
	}
	repo, err := c.Repo(prefix + "/" + m.Name + "-release")
	if err != nil {
		return "", err
	}
	got, h, err := readRelease(ctx, c, repo.String(), m.Tag)
	if err != nil {
		return "", err
	}
	switch {
	case got == nil:
		return "", fmt.Errorf("%s holds no release manifest %s: the provision hook runs in the release's installer image (a mirror holds the %s-release and %s-installer repositories too)", repo, m.Tag, m.Name, m.Name)
	case got.Name != m.Name || got.ProjectVersion != m.ProjectVersion || got.Revision != m.Revision:
		return "", fmt.Errorf("%s:%s is %s %s (revision %d), not this installer's %s (revision %d): run the installer of the release it holds", repo, m.Tag, got.Name, got.ProjectVersion, got.Revision, m.ProjectVersion, m.Revision)
	case got.Artifacts.Installer == "":
		return "", fmt.Errorf("%s:%s records no installer image", repo, m.Tag)
	}
	trust, err := trusted("this installer", in.R.Keys)
	if err != nil {
		return "", err
	}
	if err := checkSigned(ctx, c, repo, h, got, trust); err != nil {
		return "", err
	}
	return prefix + "/" + m.Name + "-installer@" + got.Artifacts.Installer, nil
}

// k8sHookPlan is what a run of the hook on the release needs, read before anything changes.
type k8sHookPlan struct {
	hook   *providers.ProvisionHook
	prefix string // the registry the release pulls from
	ctl    string // the installer image
	pull   string // the release's pull Secret; empty: none
}

// k8sHookPlanFor reads what a run of the hook needs on the release just deployed from prefix: nil
// when the release has no hook or --no-provision says not to run it.
func (in *Installer) k8sHookPlanFor(ctx context.Context, prefix string) (*k8sHookPlan, error) {
	h, err := in.hook()
	if err != nil || h == nil {
		return nil, err
	}
	if in.O.NoProvision {
		return nil, nil
	}
	ref, err := in.hookImageOf(ctx, prefix)
	if err != nil {
		return nil, err
	}
	if err := in.checkHookGrants(ctx); err != nil {
		return nil, err
	}
	return &k8sHookPlan{hook: h, prefix: prefix, ctl: ref}, nil
}

// k8sProvisioned runs the plan's hook on the release that now runs (nil: none), after a deploy: a
// failure leaves the release as it runs and names `k8s provision`.
func (in *Installer) k8sProvisioned(ctx context.Context, p *k8sHookPlan) error {
	if p == nil {
		if h, _ := in.hook(); h != nil && in.O.NoProvision {
			in.O.logf("not provisioned (--no-provision): `%s` runs the provision hook", in.k8sCtl("provision"))
		}
		return nil
	}
	if err := in.k8sReadPull(ctx, p); err != nil {
		return in.hookFailed(err, outputKeys(nil, p.hook, nil))
	}
	return in.k8sRunHook(ctx, p, nil) // a deploy's --set are inputs, never the hook's values
}

// k8sReadPull reads the pull Secret the release's values name.
func (in *Installer) k8sReadPull(ctx context.Context, p *k8sHookPlan) error {
	out, err := in.helm(ctx, Cmd{Args: []string{"get", "values", in.O.K8s.Release, "--all", "--output", "json"}})
	if err != nil {
		return err
	}
	var v struct {
		Stowage struct {
			PullSecret any `json:"pullSecret"`
		} `json:"stowage"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return fmt.Errorf("helm get values: %w", err)
	}
	p.pull, _ = v.Stowage.PullSecret.(string)
	return nil
}

// K8sProvision is `k8s provision`: the provision hook again on the release, run by the installer of
// the release it runs. --set sets a value the hook returned before or declares it returns (empty
// forgets it), first.
func (in *Installer) K8sProvision(ctx context.Context) error {
	in.givenCA = true
	k, m := in.O.K8s, in.R.Manifest
	if err := in.checkTarget(); err != nil {
		return err
	}
	h, err := in.hook()
	if err != nil {
		return err
	}
	if h == nil {
		return fmt.Errorf("%s has no provision hook", m.Name)
	}
	if _, err := in.checkHelm(ctx); err != nil {
		return err
	}
	if _, err := k.Kubectl.Run(ctx, Cmd{Args: []string{"version", "--client"}}); err != nil {
		return fmt.Errorf("kubectl is required: %w", err)
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
	cur, err := in.installedRelease(ctx)
	if err != nil {
		return err
	}
	if r := cur.Release; r.Version != m.ProjectVersion || r.Revision != m.Revision {
		return fmt.Errorf("release %s in %s runs %s %s (revision %d), and this installer carries %s (revision %d): provision it with the installer of %s",
			k.Release, k.Namespace, m.Name, r.Version, r.Revision, m.ProjectVersion, m.Revision, r.Version)
	}
	in.O.NoProvision = false // the verb runs the hook
	p, err := in.k8sHookPlanFor(ctx, cur.Registry)
	if err != nil {
		return err
	}
	if err := in.k8sReadPull(ctx, p); err != nil {
		return err
	}
	return in.k8sRunHook(ctx, p, in.O.Set)
}

// hookFailed is a failed run of the hook on the release, which runs on: it names the command that
// runs it again, and the --set that sets a value it returned or declares it returns (keys).
func (in *Installer) hookFailed(err error, keys []string) error {
	k := in.O.K8s
	return provisionFailed{fmt.Errorf("provisioning did not finish: %w\nrelease %s in %s runs %s %s; what the hook returned is in the Secret %s: `%s` runs it again%s",
		err, k.Release, k.Namespace, in.R.Manifest.Name, in.R.Manifest.ProjectVersion, k.hookName(), in.k8sCtl("provision"), setHint(keys))}
}

// k8sRunHook runs the hook as a Job on the release and records what it returns in the outputs
// Secret: its stdout reaches no log. set sets values the hook returned before or declares it
// returns (empty: forgets one), first.
func (in *Installer) k8sRunHook(ctx context.Context, p *k8sHookPlan, set map[string]string) error {
	o, k := &in.O, in.O.K8s
	env, err := in.readSecret(ctx, k.envSecret())
	if err != nil {
		return err
	}
	if env == nil {
		return fmt.Errorf("release %s in %s has no Secret %s: `%s` writes it", k.Release, k.Namespace, k.envSecret(), in.k8sCtl("install"))
	}
	outs, err := in.readSecret(ctx, k.hookName())
	if err != nil {
		return err
	}
	if outs == nil {
		outs = map[string]string{} // no outputs Secret yet: applyHookObjects creates it
	}
	// The keys the hook may not return: the release's Secret's and the safe defaults (what Docker
	// records in .env), and the chart's ConfigMap's (an output of that name would shadow the
	// chart's value from then on).
	reserved := map[string]bool{}
	for key := range env {
		reserved[key] = true
	}
	for key := range in.R.Spec.SafeDefaults {
		reserved[key] = true
	}
	cm, err := in.configMapKeys(ctx, k.hookName())
	if err != nil {
		return err
	}
	chart := map[string]bool{}
	for _, key := range cm {
		reserved[key], chart[key] = true, true
	}
	// What changes in the outputs before the run: a key now reserved is dropped, as is one that is
	// no variable name, and --set sets (empty: forgets) a value the hook returned or declares it
	// returns.
	change := map[string]*string{}
	for _, key := range sortedKeys(outs) {
		why := ""
		switch {
		case !providers.EnvName.MatchString(key):
			why = "not a variable name"
		case reserved[key]:
			why = "now a value of the release (its Secret, a safe default) or of the ConfigMap " + k.hookName()
		}
		if why != "" {
			o.logf("the Secret %s: dropping %s, %s", k.hookName(), key, why)
			change[key] = nil
			delete(outs, key)
		}
	}
	h := p.hook
	settable := outputKeys(sortedKeys(outs), h, reserved)
	var bad []string
	for _, key := range sortedKeys(set) {
		v := set[key]
		if !slices.Contains(settable, key) {
			bad = append(bad, fmt.Sprintf("%s is no value the provision hook returned or declares it returns (%s); on Kubernetes --set sets only those", key, cmp.Or(strings.Join(settable, ", "), "none")))
			continue
		}
		if err := spec.CheckEnvValue(v); err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", key, err))
			continue
		}
		if v == "" {
			change[key] = nil
			delete(outs, key)
		} else {
			change[key] = &v
			outs[key] = v
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("--set: %s", strings.Join(bad, "; "))
	}
	returned := sortedKeys(outs)
	fail := func(err error) error { return in.hookFailed(err, outputKeys(returned, h, reserved)) }
	if err := in.applyHookObjects(ctx); err != nil {
		return fail(err)
	}
	if len(change) > 0 {
		if err := in.patchOutputs(ctx, change); err != nil {
			return fail(err)
		}
		var given []string
		for _, key := range sortedKeys(set) {
			if _, ok := change[key]; ok {
				given = append(given, key)
			}
		}
		if len(given) > 0 {
			o.logf("recorded %s in the Secret %s", strings.Join(given, ", "), k.hookName())
		}
	}
	if err := in.clearHookJob(ctx); err != nil {
		return fail(err)
	}
	job, err := json.Marshal(in.hookJobObject(p, env, outs, chart, sortedKeys(reserved)))
	if err != nil {
		return err
	}
	o.logf("provisioning %s: the Job %s runs the hook", in.R.Spec.Name, k.hookJob())
	if _, err := in.kubectl(ctx, Cmd{Args: []string{"create", "--filename", "-"}, Stdin: bytes.NewReader(job)}); err != nil {
		return fail(err)
	}
	runErr := in.awaitHookJob(ctx)
	// Removed whatever happened, an interrupted run included: its pod stops the hook with it.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second) // kubectl's own --timeout 60s, and more
	if err := in.deleteHookJob(dctx); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("the Job %s could not be deleted: `kubectl delete job %s --namespace %s` (%w)", k.hookJob(), k.hookJob(), k.Namespace, err))
	}
	cancel()
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	if after, err := in.readSecret(rctx, k.hookName()); err == nil && after != nil {
		returned = sortedKeys(after)
	}
	cancel()
	if runErr != nil {
		return fail(runErr)
	}
	if len(returned) > 0 {
		fmt.Fprintf(o.Out, "provisioned: %s (in the Secret %s)\n", strings.Join(returned, ", "), k.hookName())
	} else {
		fmt.Fprintln(o.Out, "provisioned")
	}
	return nil
}

// configMapKeys are the keys of the ConfigMap name in the release's namespace, none when there is
// none.
func (in *Installer) configMapKeys(ctx context.Context, name string) ([]string, error) {
	out, err := in.kubectl(ctx, Cmd{Args: []string{"get", "configmap", name, "--ignore-not-found", "--output", "json"}})
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		return nil, err
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(out, &cm); err != nil {
		return nil, fmt.Errorf("configmap %s: %w", name, err)
	}
	return sortedKeys(cm.Data), nil
}

// hookGrants are what the hook's run does in the release's namespace, as `kubectl auth can-i` asks
// them: verb and resource. Keep in sync with what k8sRunHook and the Job's polling run.
var hookGrants = [][2]string{
	{"create", "jobs.batch"}, {"delete", "jobs.batch"}, {"get", "jobs.batch"}, {"list", "jobs.batch"}, {"watch", "jobs.batch"},
	{"get", "pods"}, {"list", "pods"}, {"watch", "pods"}, {"get", "pods --subresource=log"},
	{"create", "serviceaccounts"}, {"patch", "serviceaccounts"}, {"create", "roles.rbac.authorization.k8s.io"},
	{"patch", "roles.rbac.authorization.k8s.io"}, {"create", "rolebindings.rbac.authorization.k8s.io"},
	{"patch", "rolebindings.rbac.authorization.k8s.io"}, {"get", "secrets"}, {"create", "secrets"}, {"patch", "secrets"},
	{"get", "configmaps"},
}

// checkHookGrants refuses, before anything changes, a run whose account may not do what the hook's
// run does in the namespace. The questions are asked at once.
func (in *Installer) checkHookGrants(ctx context.Context) error {
	k := in.O.K8s
	no := make([]bool, len(hookGrants))
	errs := make([]error, len(hookGrants))
	ask := func(i int) {
		// `kubectl auth can-i` prints yes (exit 0) or no (exit 1) on stdout; anything else, an
		// exit 1 without that answer included, is that it could not ask.
		var out bytes.Buffer
		args := append([]string{"auth", "can-i"}, strings.Fields(hookGrants[i][0]+" "+hookGrants[i][1])...)
		_, err := in.kubectl(ctx, Cmd{Args: args, Stdout: &out})
		switch answer := strings.TrimSpace(out.String()); {
		case err == nil && answer == "yes":
		case err != nil && (answer == "no" || strings.HasPrefix(answer, "no ")): // "no - <reason>" with a reason
			no[i] = true
		case err == nil:
			errs[i] = fmt.Errorf("kubectl %s: answered %q", strings.Join(args, " "), answer)
		default:
			errs[i] = fmt.Errorf("kubectl %s: %w", strings.Join(args, " "), err)
		}
	}
	// The first question alone: a kubeconfig's credential plugin (a browser sign-in, a cloud
	// token) runs once, and the others reuse what it cached; then the rest at once.
	ask(0)
	if errs[0] == nil {
		var wg sync.WaitGroup
		for i := 1; i < len(hookGrants); i++ {
			wg.Go(func() { ask(i) })
		}
		wg.Wait()
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	var missing []string
	for i, g := range hookGrants {
		if no[i] {
			missing = append(missing, g[0]+" "+g[1])
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the provision hook runs as a Job of its own, and this account may not, in %s: %s (`--no-provision` deploys without it)", k.Namespace, strings.Join(missing, ", "))
	}
	return nil
}

// deleteHookJob deletes the provision Job and waits for its pod to go.
func (in *Installer) deleteHookJob(ctx context.Context) error {
	_, err := in.kubectl(ctx, Cmd{Args: []string{"delete", "job", in.O.K8s.hookJob(), "--ignore-not-found", "--cascade=foreground", "--wait", "--timeout", "60s"}})
	return err
}

// applyHookObjects applies, server-side, the outputs Secret (its labels: its data is the hook's
// and patched), the ServiceAccount the Job's record step runs as, and the Role and RoleBinding
// that let it patch that one Secret and nothing else.
func (in *Installer) applyHookObjects(ctx context.Context) error {
	k := in.O.K8s
	name := k.hookName()
	meta := map[string]any{"name": name, "namespace": k.Namespace, "labels": in.secretLabels()}
	items := []any{
		map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": meta},
		map[string]any{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": meta, "automountServiceAccountToken": false},
		map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": meta, "rules": []any{
			map[string]any{"apiGroups": []string{""}, "resources": []string{"secrets"}, "resourceNames": []string{name}, "verbs": []string{"patch"}},
		}},
		map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": meta,
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": name},
			"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": name, "namespace": k.Namespace}},
		},
	}
	list, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
	if err != nil {
		return err
	}
	_, err = in.kubectl(ctx, Cmd{Args: []string{"apply", "--server-side", "--field-manager", managedBy, "--filename", "-"}, Stdin: bytes.NewReader(list)})
	return err
}

// patchOutputs merges change into the outputs Secret's data (nil removes a key), from a private
// file: the values never reach a command line.
func (in *Installer) patchOutputs(ctx context.Context, change map[string]*string) error {
	data := map[string]any{}
	for key, v := range change {
		if v == nil {
			data[key] = nil
		} else {
			data[key] = base64.StdEncoding.EncodeToString([]byte(*v))
		}
	}
	raw, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "stowage-patch-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "patch.json")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		return err
	}
	_, err = in.kubectl(ctx, Cmd{Args: []string{"patch", "secret", in.O.K8s.hookName(), "--type", "merge", "--patch-file", file}})
	return err
}

// k8sJob is the part of a Job or a pod the installer reads.
type k8sJob struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Status struct {
		Active     int `json:"active"`
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
		Phase                 string            `json:"phase"`
		InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
		ContainerStatuses     []containerStatus `json:"containerStatuses"`
	} `json:"status"`
}

type containerStatus struct {
	Name  string `json:"name"`
	State struct {
		Waiting *struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"waiting"`
		Terminated *struct {
			ExitCode int    `json:"exitCode"`
			Reason   string `json:"reason"`
			Message  string `json:"message"`
		} `json:"terminated"`
	} `json:"state"`
}

// finished is the Job's terminal condition (Complete or Failed), empty while it runs.
func (j k8sJob) finished() (string, string) {
	for _, c := range j.Status.Conditions {
		if (c.Type == "Complete" || c.Type == "Failed") && c.Status == "True" {
			return c.Type, strings.TrimSpace(c.Reason + ": " + c.Message)
		}
	}
	return "", ""
}

// clearHookJob removes a provision Job an earlier run left (an interrupted installer), unless it
// still runs: another run of the hook may be under way.
func (in *Installer) clearHookJob(ctx context.Context) error {
	k := in.O.K8s
	out, err := in.kubectl(ctx, Cmd{Args: []string{"get", "job", k.hookJob(), "--ignore-not-found", "--output", "json"}})
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil
	}
	var j k8sJob
	if err := json.Unmarshal(out, &j); err != nil {
		return fmt.Errorf("job %s: %w", k.hookJob(), err)
	}
	if j.Metadata.Labels["app.kubernetes.io/managed-by"] != managedBy || j.Metadata.Labels["app.kubernetes.io/instance"] != k.Release {
		return fmt.Errorf("job %s in %s was not created by %s for release %s: it is not deleted, and the provision hook needs its name", k.hookJob(), k.Namespace, in.R.Ctl(), k.Release)
	}
	if done, _ := j.finished(); done == "" {
		return fmt.Errorf("the provision Job %s in %s still runs (another run of the hook?): wait for it, or `kubectl delete job %s --namespace %s`, then run again", k.hookJob(), k.Namespace, k.hookJob(), k.Namespace)
	}
	in.O.logf("deleting the provision Job %s an earlier run left", k.hookJob())
	return in.deleteHookJob(ctx)
}

// awaitHookJob waits for the provision Job to end, then prints its two steps' logs (the hook's
// stderr, and the keys the record step wrote): nil when it completed.
func (in *Installer) awaitHookJob(ctx context.Context) error {
	k := in.O.K8s
	deadline := time.Now().Add(provisionTimeout + k8sHookGrace + time.Minute)
	var stuckSince time.Time
	var stuckKind string
	for {
		// A read that fails (the API server unreachable a moment) is tried again: stopping here
		// would delete a Job whose hook still runs. One that keeps failing is given up as a pod
		// that cannot start is.
		pod, j, why, err := in.hookJobState(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			why = err.Error()
		}
		if err == nil && pod != nil && (pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed") {
			in.hookLogs(ctx, pod)
			if pod.Status.Phase == "Succeeded" {
				return nil
			}
			return podFailure(pod)
		}
		if err == nil {
			if done, msg := j.finished(); done == "Failed" {
				if pod != nil {
					in.hookLogs(ctx, pod)
				}
				return fmt.Errorf("the Job %s failed (%s)", k.hookJob(), msg)
			}
		}
		// An unschedulable pod waits longer: a cluster autoscaler may be adding its node. Each kind
		// of trouble (a failed read, no node, the rest) is timed from when it began.
		limit, kind := k8sHookStuck, "pod"
		switch {
		case err != nil:
			kind = "read"
		case strings.HasPrefix(why, unschedulable):
			limit, kind = k8sHookUnschedulable, "unschedulable"
		}
		if kind != stuckKind {
			stuckSince, stuckKind = time.Time{}, kind
		}
		switch {
		case why == "":
			stuckSince = time.Time{}
		case stuckSince.IsZero():
			stuckSince = time.Now()
		case time.Since(stuckSince) > limit:
			if pod != nil {
				in.hookLogs(ctx, pod) // what the steps that ran said, before the Job goes
			}
			return fmt.Errorf("the Job %s: %s", k.hookJob(), why)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the Job %s did not end within %s", k.hookJob(), provisionTimeout+k8sHookGrace)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(k8sHookPoll):
		}
	}
}

// hookJobState reads the provision Job and its pod (nil before it has one), and why the pod cannot
// start, when it cannot.
func (in *Installer) hookJobState(ctx context.Context) (pod *k8sJob, j k8sJob, why string, err error) {
	k := in.O.K8s
	{
		out, err := in.kubectl(ctx, Cmd{Args: []string{"get", "pods", "--selector", "job-name=" + k.hookJob(), "--output", "json"}})
		if err != nil {
			return nil, j, "", err
		}
		var pods struct{ Items []k8sJob }
		if err := json.Unmarshal(out, &pods); err != nil {
			return nil, j, "", fmt.Errorf("the provision Job's pods: %w", err)
		}
		if len(pods.Items) > 0 {
			pod = &pods.Items[0]
		}
		jout, err := in.kubectl(ctx, Cmd{Args: []string{"get", "job", k.hookJob(), "--output", "json"}})
		if err != nil {
			return pod, j, "", err
		}
		if err := json.Unmarshal(jout, &j); err != nil {
			return pod, j, "", fmt.Errorf("job %s: %w", k.hookJob(), err)
		}
	}
	if pod == nil {
		return nil, j, "no pod was created (`kubectl describe job " + k.hookJob() + " --namespace " + k.Namespace + "` says why)", nil
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == "PodScheduled" && c.Status == "False" && c.Reason == "Unschedulable" {
			why = unschedulable + c.Message
		}
	}
	for _, c := range append(slices.Clone(pod.Status.InitContainerStatuses), pod.Status.ContainerStatuses...) {
		if w := c.State.Waiting; w != nil && slices.Contains([]string{"ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError"}, w.Reason) {
			why = fmt.Sprintf("its container %s cannot start: %s: %s", c.Name, w.Reason, w.Message)
		}
		// The hook's step exits 0 once it has recorded how the hook ended; any other end (killed
		// for its memory, a failed start) leaves the record step waiting for that record.
		if t := c.State.Terminated; c.Name == "provision" && t != nil && t.ExitCode != 0 {
			why = fmt.Sprintf("its hook step ended without recording how the hook ended: %s (exit %d) %s", t.Reason, t.ExitCode, t.Message)
		}
		// The record step ending while the hook runs on: nothing the hook prints is recorded.
		if t := c.State.Terminated; c.Name == "record" && t != nil && t.ExitCode != 0 {
			why = fmt.Sprintf("its record step ended while the hook ran: %s (exit %d) %s", t.Reason, t.ExitCode, strings.TrimSpace(t.Message))
		}
	}
	return pod, j, why, nil
}

// hookLogs copies the logs of the pod's hook and record steps to the installer's log: the hook's
// stderr (its stdout went to the pod's volume) and the keys recorded.
func (in *Installer) hookLogs(ctx context.Context, pod *k8sJob) {
	started := map[string]bool{}
	for _, c := range append(slices.Clone(pod.Status.InitContainerStatuses), pod.Status.ContainerStatuses...) {
		started[c.Name] = c.State.Terminated != nil || c.State.Waiting == nil
	}
	for _, c := range []string{"check", "provision", "record"} {
		if !started[c] {
			continue // a step that never ran has no log
		}
		out, err := in.kubectl(ctx, Cmd{Args: []string{"logs", pod.Metadata.Name, "--container", c}})
		if err != nil {
			in.O.logf("the %s step's log could not be read: %v", c, err)
			continue
		}
		if len(out) > 0 {
			fmt.Fprintf(in.O.Log, "%s", out)
			if !bytes.HasSuffix(out, []byte("\n")) {
				fmt.Fprintln(in.O.Log)
			}
		}
	}
}

// podFailure says why the provision pod failed: the record step's message (the hook's end and
// what was refused), else the first step that failed.
func podFailure(pod *k8sJob) error {
	// The record and check steps say why in their termination message (keys, never values).
	for _, c := range append(slices.Clone(pod.Status.InitContainerStatuses), pod.Status.ContainerStatuses...) {
		if t := c.State.Terminated; (c.Name == "record" || c.Name == "check") && t != nil && t.ExitCode != 0 {
			if msg := strings.TrimSpace(t.Message); msg != "" {
				return errors.New(msg)
			}
			return fmt.Errorf("its %s step exited %d", c.Name, t.ExitCode)
		}
	}
	for _, c := range append(slices.Clone(pod.Status.InitContainerStatuses), pod.Status.ContainerStatuses...) {
		if t := c.State.Terminated; t != nil && t.ExitCode != 0 {
			return fmt.Errorf("its %s step exited %d (%s)", c.Name, t.ExitCode, t.Reason)
		}
	}
	return errors.New("its pod failed")
}

// hookJobObject is the provision Job: the hook's container gets the keys its env declares that the
// release's Secret holds, what it returned before (the outputs Secret, key by key), the values
// Stowage provides and the chart's optional ConfigMap; it holds no ServiceAccount token.
func (in *Installer) hookJobObject(p *k8sHookPlan, env, outs map[string]string, chart map[string]bool, reserved []string) map[string]any {
	k, s, m := in.O.K8s, in.R.Spec, in.R.Manifest
	ctl := "/" + providers.Ctl(s.Short())
	name := k.hookName()
	labels := in.secretLabels()
	labels["app.kubernetes.io/component"] = "stowage-provision"
	var vars []any
	for _, kv := range [][2]string{{"STOWAGE_NAME", s.Name}, {"STOWAGE_PROJECT_VERSION", m.ProjectVersion}, {"STOWAGE_REVISION", strconv.Itoa(m.Revision)}} {
		vars = append(vars, map[string]any{"name": kv[0], "value": kv[1]})
	}
	ref := func(secret, key string) map[string]any {
		return map[string]any{"name": key, "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": secret, "key": key}}}
	}
	for _, key := range p.hook.Keys() {
		if _, ok := env[key]; ok {
			vars = append(vars, ref(k.envSecret(), key))
		} else if v, ok := s.SafeDefaults[key]; ok && !chart[key] {
			// A safe default is no secret, and the release's Secret does not hold it; the chart's
			// ConfigMap, which the hook gets too, sets it for the cluster when it names it.
			vars = append(vars, map[string]any{"name": key, "value": v})
		}
	}
	for _, key := range sortedKeys(outs) {
		vars = append(vars, ref(name, key))
	}
	ours := map[string]any{"runAsNonRoot": true, "runAsUser": hookUID, "runAsGroup": hookUID, "allowPrivilegeEscalation": false,
		"readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}}
	shared := map[string]any{"name": "stowage", "mountPath": hookMount}
	run := append([]string{hookMount + ctl, k8sHookCommand, "run", "--dir", hookMount, "--timeout", provisionTimeout.String(), "--"}, p.hook.Cmd...)
	pod := map[string]any{
		"restartPolicy":                "Never",
		"serviceAccountName":           name,
		"automountServiceAccountToken": false,
		"enableServiceLinks":           false,
		"securityContext":              map[string]any{"seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"volumes": []any{
			map[string]any{"name": "stowage", "emptyDir": map[string]any{}},
			map[string]any{"name": "prev", "secret": map[string]any{"secretName": name, "optional": true}},
			map[string]any{"name": "token", "projected": map[string]any{"defaultMode": 0o444, "sources": []any{
				map[string]any{"serviceAccountToken": map[string]any{"path": "token", "expirationSeconds": 3600}},
				map[string]any{"configMap": map[string]any{"name": "kube-root-ca.crt", "items": []any{map[string]any{"key": "ca.crt", "path": "ca.crt"}}}},
			}}},
		},
		"initContainers": []any{
			map[string]any{"name": "copy", "image": p.ctl, "command": []string{ctl, k8sHookCommand, "copy", hookMount + ctl},
				"securityContext": ours, "volumeMounts": []any{shared}},
			// Before the hook makes anything, the record step's way to the Secret is proven (its
			// grant, the network to the API server): an empty patch.
			map[string]any{"name": "check", "image": p.ctl,
				"command":         []string{ctl, k8sHookCommand, "record", "--check", "--token", tokenMount, "--namespace", k.Namespace, "--secret", name},
				"securityContext": ours,
				"volumeMounts":    []any{map[string]any{"name": "token", "mountPath": tokenMount, "readOnly": true}}},
		},
		"containers": []any{
			map[string]any{"name": "provision", "image": in.R.ImageRef(p.prefix, p.hook.Image), "command": run, "env": vars,
				"envFrom": []any{map[string]any{"configMapRef": map[string]any{"name": name, "optional": true}}},
				// Non-root, as the restricted Pod Security standard requires: the image's own user.
				"securityContext": map[string]any{"runAsNonRoot": true, "allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}},
				"volumeMounts":    []any{shared}},
			map[string]any{"name": "record", "image": p.ctl,
				"command": []string{ctl, k8sHookCommand, "record", "--dir", hookMount, "--prev", prevMount, "--token", tokenMount,
					"--namespace", k.Namespace, "--secret", name, "--reserved", strings.Join(reserved, ",")},
				"securityContext": ours,
				"volumeMounts": []any{
					map[string]any{"name": "stowage", "mountPath": hookMount, "readOnly": true},
					map[string]any{"name": "prev", "mountPath": prevMount, "readOnly": true},
					map[string]any{"name": "token", "mountPath": tokenMount, "readOnly": true},
				}},
		},
	}
	if p.pull != "" {
		pod["imagePullSecrets"] = []any{map[string]any{"name": p.pull}}
	}
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata":   map[string]any{"name": k.hookJob(), "namespace": k.Namespace, "labels": labels},
		"spec": map[string]any{
			"backoffLimit":            0,
			"activeDeadlineSeconds":   int((provisionTimeout + k8sHookGrace) / time.Second),
			"ttlSecondsAfterFinished": 3600,
			"template":                map[string]any{"metadata": map[string]any{"labels": labels}, "spec": pod},
		},
	}
}
