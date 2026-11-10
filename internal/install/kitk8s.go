package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"bytes"

	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/spec"
)

// kitExitKey is where a check's Job records its exit status, in the kit's Secret.
const kitExitKey = "KIT_EXIT"

// kitGrants are what a handoff's run needs beyond the hook's: deleting what it used, its Secret holding
// what was issued above all, so it refuses to start rather than leave values in the cluster, and
// reading what it would delete, to leave another tool's.
var kitGrants = [][2]string{
	{"delete", "secrets"}, {"delete", "serviceaccounts"}, {"delete", "roles.rbac.authorization.k8s.io"}, {"delete", "rolebindings.rbac.authorization.k8s.io"},
	{"get", "serviceaccounts"}, {"get", "roles.rbac.authorization.k8s.io"}, {"get", "rolebindings.rbac.authorization.k8s.io"},
}

// maxK8sName bounds a Job's name, which reaches its pods' labels.
const maxK8sName = 63

// k8sKitInstaller is this installer aimed at a handoff's Job and Secret, read as the provision hook's
// are, and the plan to run step: the release must run this installer's release.
func (in *Installer) k8sKitInstaller(ctx context.Context, h *spec.Handoff, step *spec.HandoffStep) (*Installer, *k8sHookPlan, error) {
	in.givenCA = true
	k, m := in.O.K8s, in.R.Manifest
	kin := *in
	kin.O.K8s.kit = h.Name
	if name := kin.O.K8s.hookJob(); len(name) > maxK8sName {
		return nil, nil, fmt.Errorf("the Job %s is more than %d characters: the release's name and the handoff's come to too long a name", name, maxK8sName)
	}
	if err := in.checkTarget(); err != nil {
		return nil, nil, err
	}
	if _, err := in.checkHelm(ctx); err != nil {
		return nil, nil, err
	}
	if _, err := k.Kubectl.Run(ctx, Cmd{Args: []string{"version", "--client"}}); err != nil {
		return nil, nil, fmt.Errorf("kubectl is required: %w", err)
	}
	rel, err := in.release(ctx)
	if err != nil {
		return nil, nil, err
	}
	if rel == nil {
		return nil, nil, in.noRelease()
	}
	if err := in.pending(rel); err != nil {
		return nil, nil, err
	}
	cur, err := in.installedRelease(ctx)
	if err != nil {
		return nil, nil, err
	}
	if r := cur.Release; r.Version != m.ProjectVersion || r.Revision != m.Revision {
		return nil, nil, fmt.Errorf("release %s in %s runs %s %s (revision %d), and this installer carries %s (revision %d): issue it with the installer of %s",
			k.Release, k.Namespace, m.Name, r.Version, r.Revision, m.ProjectVersion, m.Revision, r.Version)
	}
	ref, err := in.hookImageOf(ctx, cur.Registry, kitUse)
	if err != nil {
		return nil, nil, err
	}
	if err := in.checkGrants(ctx, append(slices.Clone(hookGrants), kitGrants...), "a handoff runs as a Job of its own, whose Secret is deleted after it", ""); err != nil {
		return nil, nil, err
	}
	p := &k8sHookPlan{hook: &providers.ProvisionHook{Image: step.Image, Cmd: step.Cmd, Env: &step.Env}, prefix: cur.Registry, ctl: ref}
	if err := in.k8sReadPull(ctx, p); err != nil {
		return nil, nil, err
	}
	return &kin, p, nil
}

// k8sKitRun runs one Job of a handoff's step, and hands back what the Secret holds, the objects left for
// k8sKitDone once the kit is sealed. Its Secret is deleted only once what it holds is in a kit: any other
// end keeps it, as it may be the only copy of what was issued, and it stops the next run, for the
// operator to read and delete. A check's holds only an exit status, so a check keeps nothing. While the
// Job exists it stands for a run under way: Kubernetes has no lock here.
func (in *Installer) k8sKitRun(ctx context.Context, p *k8sHookPlan, answers map[string]string) (map[string]string, error) {
	k := in.O.K8s
	env, err := in.readSecret(ctx, k.envSecret())
	if err != nil {
		return nil, err
	}
	if env == nil {
		return nil, fmt.Errorf("release %s in %s has no Secret %s: `%s` writes it", k.Release, k.Namespace, k.envSecret(), in.k8sCtl("install"))
	}
	cm, err := in.configMapKeys(ctx, k.provisionName())
	if err != nil {
		return nil, err
	}
	chart := map[string]bool{}
	for _, key := range cm {
		chart[key] = true
	}
	exists, ours, err := in.ownedState(ctx, k8sOwned{"job", k.hookJob()})
	if err != nil {
		return nil, err
	}
	if exists && ours {
		return nil, fmt.Errorf("the Job %s is there, of a run under way or one interrupted: once none runs, `kubectl get secret %s %s --output json` shows what it gave, `kubectl delete job/%s secret/%s --ignore-not-found %s` clears the way, then run again", k.hookJob(), k.hookName(), k.kubectlWhere(), k.hookJob(), k.hookName(), k.kubectlWhere())
	}
	if exists {
		return nil, fmt.Errorf("job %s in %s was not made by %s for release %s: it is not deleted, and this handoff needs its name", k.hookJob(), k.Namespace, in.R.Ctl(), k.Release)
	}
	// Never another tool's object of the name, which would be deleted here.
	for _, kind := range []string{"secret", "rolebinding", "role", "serviceaccount"} {
		exists, ours, err := in.ownedState(ctx, k8sOwned{kind, k.hookName()})
		if err != nil {
			return nil, err
		}
		if exists && !ours {
			return nil, fmt.Errorf("%s %s in %s was not made by %s for release %s: it is not deleted, and this handoff needs its name", kind, k.hookName(), k.Namespace, in.R.Ctl(), k.Release)
		}
	}
	// What an earlier run left may be the only copy of what it issued: the operator's to read and
	// delete, never this run's. A check's exit status is worth nothing.
	held, err := in.readSecret(ctx, k.hookName())
	if err != nil {
		return nil, err
	}
	if kitHolds(held) {
		return nil, fmt.Errorf("the Secret %s holds what an earlier run gave, perhaps the only copy of what it issued: `kubectl get secret %s %s --output json` shows it, `kubectl delete secret %s %s` clears the way, then run again", k.hookName(), k.hookName(), k.kubectlWhere(), k.hookName(), k.kubectlWhere())
	}
	if err := in.deleteKitObjects(ctx); err != nil {
		return nil, err
	}
	// Before a Job, the objects hold no more than the answers. They are the other run's when it holds it.
	created, overlap := false, false
	defer func() {
		if !created && !overlap {
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
			defer cancel()
			if err := in.deleteKitObjects(dctx); err != nil {
				in.O.logf("the Secret %s holds the handoff's answers and could not be deleted: `kubectl delete secret %s %s` (%v)", k.hookName(), k.hookName(), k.kubectlWhere(), err)
			}
		}
	}()
	if err := in.applyHookObjects(ctx); err != nil {
		return nil, err
	}
	change := map[string]*string{}
	outs := map[string]string{}
	for key, v := range answers {
		change[key] = &v
		outs[key] = v
	}
	if len(change) > 0 {
		if err := in.patchOutputs(ctx, change); err != nil {
			return nil, err
		}
	}
	job, err := json.Marshal(in.hookJobObject(p, env, outs, chart, nil))
	if err != nil {
		return nil, err
	}
	if _, err := in.kubectl(ctx, Cmd{Args: []string{"create", "--filename", "-"}, Stdin: bytes.NewReader(job)}); err != nil {
		overlap = strings.Contains(err.Error(), "AlreadyExists")
		if !overlap {
			// An answer lost may hide a Job the server took: its name was free, so it is this run's.
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
			defer cancel()
			if derr := in.deleteHookJob(dctx); derr != nil {
				return nil, fmt.Errorf("%w\nthe Job %s may exist, and its step be running: `kubectl delete job %s %s` (%v)", err, k.hookJob(), k.hookJob(), k.kubectlWhere(), derr)
			}
		}
		return nil, err
	}
	created = true
	runErr := in.awaitHookJob(ctx)
	var got map[string]string
	if runErr == nil {
		got, runErr = in.readKitSecret(ctx)
	}
	if runErr == nil {
		for key := range answers {
			delete(got, key)
		}
		return got, nil
	}
	if p.exitOnly {
		in.k8sKitDone(ctx)
		return nil, runErr
	}
	// The step is stopped, what it recorded kept. The Job gone, the Secret still stops the next run.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()
	if err := in.deleteHookJob(dctx); err != nil {
		return nil, fmt.Errorf("%w\n%s\nthe Job %s could not be deleted, and its step may still be running: `kubectl delete job %s %s` (%v)", runErr, in.kitKept(), k.hookJob(), k.hookJob(), k.kubectlWhere(), err)
	}
	return nil, fmt.Errorf("%w\n%s", runErr, in.kitKept())
}

// kitKept says where what a run may have issued is, and how to go on.
func (in *Installer) kitKept() string {
	k := in.O.K8s
	return fmt.Sprintf("the Secret %s is kept, as it may hold what the step issued: `kubectl get secret %s %s --output json` shows it, and `kubectl delete secret %s %s` clears the way for the next run", k.hookName(), k.hookName(), k.kubectlWhere(), k.hookName(), k.kubectlWhere())
}

// k8sKitDone removes a run's objects once nothing in them is needed, the Secret before the Job, which
// stands for a run until it goes.
func (in *Installer) k8sKitDone(ctx context.Context) {
	k := in.O.K8s
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()
	if err := in.deleteKitObjects(dctx); err != nil {
		in.O.logf("the Secret %s holds what the handoff gave and could not be deleted: `kubectl delete secret %s %s` (%v)", k.hookName(), k.hookName(), k.kubectlWhere(), err)
	}
	if err := in.deleteHookJob(dctx); err != nil {
		in.O.logf("the Job %s could not be deleted, and blocks the next run: `kubectl delete job %s %s` (%v)", k.hookJob(), k.hookJob(), k.kubectlWhere(), err)
	}
}

// kitReadTries is how many times the Secret a finished Job leaves is read: a read that fails a moment
// must not cost what the step issued.
const kitReadTries = 30

// readKitSecret reads the Secret a finished Job leaves, trying again while the API server is away.
func (in *Installer) readKitSecret(ctx context.Context) (map[string]string, error) {
	var got map[string]string
	var err error
	for range kitReadTries {
		if got, err = in.readSecret(ctx, in.O.K8s.hookName()); err == nil || ctx.Err() != nil {
			return got, err
		}
		var foreign *foreignSecret
		if errors.As(err, &foreign) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(k8sHookPoll):
		}
	}
	return nil, err
}

// deleteKitObjects deletes what a handoff's Job used, and the Job's Secret first of all.
func (in *Installer) deleteKitObjects(ctx context.Context) error {
	name := in.O.K8s.hookName()
	_, err := in.kubectl(ctx, Cmd{Args: []string{"delete", "secret,rolebinding,role,serviceaccount", name, "--ignore-not-found"}})
	return err
}

// K8sIssueKit runs the handoff's step as a Job and seals what it gave. The values stay out of the pod's
// log, as the provision hook's: its record step writes them to a Secret, read here, then deleted.
func (in *Installer) K8sIssueKit(ctx context.Context, req KitRequest, now time.Time) (*IssuedKit, error) {
	h, err := in.checkKitRequest(&req)
	if err != nil {
		return nil, err
	}
	kin, p, err := in.k8sKitInstaller(ctx, h, &h.Run)
	if err != nil {
		return nil, err
	}
	p.vars = kitVars(h, req.Instance, nil)
	in.O.logf("issuing the kit %s for %s, instance %s: the Job %s runs it", h.Name, h.For, req.Instance, kin.O.K8s.hookJob())
	gave, err := kin.k8sKitRun(ctx, p, req.Answers)
	if err != nil {
		return nil, fmt.Errorf("the handoff %s's run did not finish, and no kit is written: %w", h.Name, err)
	}
	issued, err := in.sealKit(h, &req, gave, now)
	if err != nil {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
		defer cancel()
		if derr := kin.deleteHookJob(dctx); derr != nil {
			in.O.logf("the Job %s could not be deleted, and blocks the next run: `kubectl delete job %s %s` (%v)", kin.O.K8s.hookJob(), kin.O.K8s.hookJob(), kin.O.K8s.kubectlWhere(), derr)
		}
		return nil, fmt.Errorf("%w\n%s", err, kin.kitKept())
	}
	kin.k8sKitDone(ctx)
	return issued, nil
}

// K8sCheckKit runs the handoff's check as a Job: its exit status, which its record step records as a
// value, so a check that says "not proven" ends a Job that succeeded.
func (in *Installer) K8sCheckKit(ctx context.Context, handoff, instance string) (int, error) {
	h, err := in.handoffCheck(handoff, instance)
	if err != nil {
		return 0, err
	}
	kin, p, err := in.k8sKitInstaller(ctx, h, h.Check)
	if err != nil {
		return 0, err
	}
	p.vars, p.exitOnly = kitVars(h, instance, nil), true
	got, err := kin.k8sKitRun(ctx, p, nil)
	if err != nil {
		return 0, err
	}
	kin.k8sKitDone(ctx)
	v, ok := got[kitExitKey]
	if !ok {
		return 0, errors.New("the check's Job recorded no exit status")
	}
	code, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("the check's Job recorded %q as its exit status", v)
	}
	return code, nil
}

// kitHolds tells a Secret holding what a run gave from one holding nothing, or a check's exit status,
// which is worth nothing.
func kitHolds(held map[string]string) bool {
	return slices.ContainsFunc(sortedKeys(held), func(key string) bool { return key != kitExitKey })
}

// k8sKitLeftovers names the handoffs' Secrets a run killed before it could delete them. Each holds
// what was issued, until the operator deletes it, or `k8s uninstall` does. A Secret that does not
// read does not hide those after it.
func (in *Installer) k8sKitLeftovers(ctx context.Context) ([]string, error) {
	var left []string
	var errs []error
	for _, h := range in.R.Spec.Handoffs {
		k := in.O.K8s
		k.kit = h.Name
		got, err := in.readSecret(ctx, k.hookName())
		var foreign *foreignSecret
		if errors.As(err, &foreign) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if kitHolds(got) {
			left = append(left, k.hookName())
		}
	}
	return left, errors.Join(errs...)
}
