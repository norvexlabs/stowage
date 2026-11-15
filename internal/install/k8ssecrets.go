package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/spec"
)

// restoreAwaitedLabel marks the provision hook's outputs Secret a `k8s install --secrets` wrote
// the values of another install into: they belong to that install's data, so no deploy runs the
// hook until `k8s provision` (the operator, the backup loaded) does, which removes the label. Set
// by a merge patch, it is no field the installer's server-side applies own, which keep it.
const restoreAwaitedLabel = "stowage.dev/restore-awaited"

// K8sExportSecrets is the release's secrets as `secrets export` writes them on Docker (one
// format, SecretsFile): the spec's secrets and the secret inputs on Kubernetes its Secrets hold,
// and what the provision hook returned, encrypted to recipients. The pull Secret, the operator's
// credential, is never in it; the front's CA neither (the chart's ingress owns the certificate).
func (in *Installer) K8sExportSecrets(ctx context.Context, recipients []string) ([]byte, error) {
	in.kube = true
	// The keys first: a refusal reads nothing from the cluster.
	if _, err := parseRecipients(recipients, maxRecipients); err != nil {
		return nil, err
	}
	rec, err := in.k8sRecordedHere(ctx, "export")
	if err != nil {
		return nil, err
	}
	f := &SecretsFile{Project: in.R.Spec.Name, Secrets: map[string]string{}}
	for _, k := range sortedKeys(rec) {
		if v := rec[k]; v != "" && in.secretKey(k) {
			f.Secrets[k] = v
		}
	}
	outs, err := in.readSecret(ctx, in.O.K8s.provisionName())
	if err != nil {
		return nil, err
	}
	for _, k := range sortedKeys(outs) {
		if f.Provision == nil {
			f.Provision = map[string]string{}
		}
		f.Provision[k] = outs[k]
	}
	in.O.logf("exporting %d secret(s) of release %s in %s and %d value(s) the provision hook returned", len(f.Secrets), in.O.K8s.Release, in.O.K8s.Namespace, len(f.Provision))
	return encryptSecrets(f, in.R.Ctl()+" k8s", recipients)
}

// k8sUseSecrets reads --secrets for a first install on Kubernetes, as Docker's install does
// (readSecrets, useSecrets); a front CA in the file is left out, said.
func (in *Installer) k8sUseSecrets() error {
	if in.O.Secrets == nil {
		return nil
	}
	f, err := in.readSecrets(in.O.Secrets)
	if err != nil {
		return err
	}
	if f.FrontCA != nil {
		in.O.logf("--secrets: front_ca left out: on Kubernetes the chart's ingress owns the certificate")
	}
	in.useSecrets(f)
	return nil
}

// k8sRestoredOutputs is the merge of the provision hook's outputs Secret --secrets makes: each
// value the hook returned on the install the file came from, any other key the Secret holds
// removed; nil when the file gives none, or the release runs no hook (said). Read before anything
// changes: a value no variable can hold, or another tool's Secret of that name, refuses the run.
func (in *Installer) k8sRestoredOutputs(ctx context.Context) (map[string]*string, error) {
	if in.restored == nil || len(in.restored.Provision) == 0 {
		return nil, nil
	}
	h, err := in.hook()
	if err != nil {
		return nil, err
	}
	if h == nil {
		in.O.logf("--secrets: %s %s runs no provision hook: its values in the file are left out", in.R.Spec.Name, in.R.Manifest.ProjectVersion)
		return nil, nil
	}
	var bad []string
	for _, k := range sortedKeys(in.restored.Provision) {
		if !providers.EnvName.MatchString(k) {
			bad = append(bad, fmt.Sprintf("%s: not a variable name", k))
		} else if err := spec.CheckEnvValue(in.restored.Provision[k]); err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", k, err))
		}
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("--secrets: provision: %s", strings.Join(bad, "; "))
	}
	held, err := in.readSecret(ctx, in.O.K8s.provisionName())
	if err != nil {
		return nil, err
	}
	change := map[string]*string{}
	for k := range held {
		change[k] = nil
	}
	for k, v := range in.restored.Provision {
		change[k] = &v
	}
	return change, nil
}

// k8sWriteRestoredOutputs writes change (k8sRestoredOutputs) into the provision hook's outputs
// Secret, created when missing, with restoreAwaitedLabel in the same patch: no failure leaves the
// values without the wait.
func (in *Installer) k8sWriteRestoredOutputs(ctx context.Context, change map[string]*string) error {
	if change == nil {
		return nil
	}
	k := in.O.K8s
	list, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{in.secretObject(k.provisionName(), "Opaque", nil)}})
	if err != nil {
		return err
	}
	if _, err := in.kubectl(ctx, Cmd{Args: []string{"apply", "--server-side", "--field-manager", managedBy, "--filename", "-"}, Stdin: bytes.NewReader(list)}); err != nil {
		return err
	}
	if err := in.patchSecret(ctx, k.provisionName(), map[string]any{"metadata": map[string]any{"labels": map[string]any{restoreAwaitedLabel: "true"}}, "data": patchData(change)}); err != nil {
		return err
	}
	in.O.logf("--secrets: the provision hook's values %s are in the Secret %s; they belong to the data of the install the file came from, so the hook waits for its backup: load it into the release's database, then `%s` runs the hook",
		strings.Join(sortedKeys(in.restored.Provision), ", "), k.provisionName(), in.k8sCtl("provision"))
	return nil
}

// k8sRestoreAwaited reports whether the outputs Secret carries restoreAwaitedLabel: a deploy then
// leaves the hook unrun.
func (in *Installer) k8sRestoreAwaited(ctx context.Context) (bool, error) {
	_, labels, err := in.readSecretLabeled(ctx, in.O.K8s.provisionName())
	if err != nil {
		return false, err
	}
	return labels[restoreAwaitedLabel] != "", nil
}

// k8sRestoreDone removes restoreAwaitedLabel once `k8s provision` ran the hook: the data is what
// the operator chose. A label it cannot remove is only reported.
func (in *Installer) k8sRestoreDone(ctx context.Context) {
	if err := in.patchSecret(ctx, in.O.K8s.provisionName(), map[string]any{"metadata": map[string]any{"labels": map[string]any{restoreAwaitedLabel: nil}}}); err != nil {
		in.O.logf("the label %s could not be removed from the Secret %s (%v): a deploy leaves the provision hook unrun until it is (`kubectl label secret %s %s- %s`)",
			restoreAwaitedLabel, in.O.K8s.provisionName(), err, in.O.K8s.provisionName(), restoreAwaitedLabel, in.O.K8s.kubectlWhere())
	}
}
