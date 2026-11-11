package install

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/helm"
)

// foreignSecret is the refusal of a Secret another tool wrote under a name this installer uses.
type foreignSecret struct{ msg string }

func (e *foreignSecret) Error() string { return e.msg }

// replaceable reports whether an upgrade's refused flags name an input --given could replace: one
// recorded and asked on Kubernetes that the upgrade does not already take.
func replaceable(rec map[string]string, all, list []input, given ...map[string]string) bool {
	has := func(l []input, key string) bool {
		return slices.ContainsFunc(l, func(i input) bool { return i.Key == key })
	}
	for _, m := range given {
		for key := range m {
			if _, ok := rec[key]; ok && has(all, key) && !has(list, key) {
				return true
			}
		}
	}
	return false
}

// givenInputs is what --given gives; nil when nothing is.
func (o *Options) givenInputs() map[string]string {
	if o.Given == nil {
		return nil
	}
	return o.Given.Inputs
}

// useK8sGiven makes g, which flag gave, this run's given material on Kubernetes. Its inputs join the answers, a flag
// and an answers file winning; an input of another shape is left out and said, one the project does
// not declare refused. Its tls is left out and said: the chart's ingress owns TLS on this shape. A g
// left with nothing is refused.
func (in *Installer) useK8sGiven(g *Given, flag string) error {
	o := &in.O
	answers := map[string]string{}
	var unknown, left []string
	for _, key := range sortedKeys(g.Inputs) {
		switch i, ok := in.R.Spec.Input(key); {
		case !ok:
			unknown = append(unknown, key)
		case !i.OnKubernetes():
			left = append(left, key)
			delete(g.Inputs, key)
		default:
			answers[key] = g.Inputs[key]
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("%s: %s %s declares no input %s", flag, in.R.Spec.Name, in.R.Manifest.ProjectVersion, strings.Join(unknown, ", "))
	}
	if len(left) > 0 {
		o.logf("%s: %s left out: not an input on Kubernetes", flag, strings.Join(left, ", "))
	}
	if g.TLS != nil {
		o.logf("%s: tls left out: on Kubernetes the chart's ingress owns the certificate", flag)
		g.TLS = nil
	}
	if err := g.leftNothing(flag); err != nil {
		return err
	}
	maps.Copy(answers, o.Answers)
	o.Given, o.Answers = g, answers
	return nil
}

// k8sFiles is the data of the files Secret: each file input's content under its key, empty while the
// input is, so a mount holds a file either way. A key no input declares now stays, with what it holds,
// until the Secret is deleted: the release a rollback or a failed upgrade leaves running mounts it.
// A later release declaring it asks it, as any input it adds; what that empties is said. Nil, writing no Secret, for a release declaring none where
// no earlier one wrote it. A Secret of the name another tool wrote is refused when one is to be written.
func (in *Installer) k8sFiles(ctx context.Context, env map[string]string) (map[string][]byte, error) {
	var declared []input
	for _, i := range in.k8sInputs() {
		if i.File() {
			declared = append(declared, i)
		}
	}
	had, err := in.readSecret(ctx, in.O.K8s.filesSecret())
	var foreign *foreignSecret
	if len(declared) == 0 && errors.As(err, &foreign) {
		return nil, nil // never written nor read: the name is free for another tool's
	}
	if err != nil {
		return nil, err
	}
	if had == nil && len(declared) == 0 {
		return nil, nil
	}
	files := map[string][]byte{}
	for key, held := range had {
		content, err := decodeFileInput(env[key])
		if err != nil || len(content) == 0 {
			content = []byte(held)
		}
		files[key] = append([]byte{}, content...)
	}
	for _, i := range declared {
		content, err := decodeFileInput(env[i.Key])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", i.Key, err)
		}
		if len(content) == 0 && had[i.Key] != "" {
			in.O.logf("input %s: nothing recorded, so its file in the Secret %s is emptied", i.Key, in.O.K8s.filesSecret())
		}
		files[i.Key] = append([]byte{}, content...)
	}
	return files, nil
}

// k8sRestartList is what an upgrade restarts the readers of: each input --given gave, changed or not,
// as a failed upgrade may have left the Secret ahead of the pods, and each recorded input this run
// changed (a required file input recorded empty).
func (in *Installer) k8sRestartList(changed []string) []string {
	keys := sortedKeys(in.O.givenInputs())
	for _, key := range changed {
		_, was := in.recorded[key]
		if _, isInput := in.R.Spec.Input(key); was && isInput && !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// k8sRestartReaders restarts each workload of the release reading one of keys: its pods keep what
// they started with, and helm rolls only what its templates change.
func (in *Installer) k8sRestartReaders(ctx context.Context, keys []string) error {
	k := in.O.K8s
	if len(keys) == 0 {
		return nil
	}
	manifest, err := in.helm(ctx, Cmd{Args: []string{"get", "manifest", k.Release}})
	if err != nil {
		return err
	}
	workloads, err := releaseWorkloads(manifest, k.Namespace, k.envSecret(), keys)
	if err != nil {
		return err
	}
	for _, f := range []struct {
		secret string
		of     func(string) bool
	}{
		{k.filesSecret(), func(key string) bool { i, ok := in.R.Spec.Input(key); return ok && i.File() }},
		{k.secretFilesSecret(), in.R.Spec.AsFile},
	} {
		if slices.ContainsFunc(keys, f.of) {
			mounting, err := releaseWorkloads(manifest, k.Namespace, f.secret, keys)
			if err != nil {
				return err
			}
			workloads = append(workloads, mounting...)
			slices.Sort(workloads)
			workloads = slices.Compact(workloads)
		}
	}
	// The context named in the hint a failure prints, read first: the current one may change meanwhile.
	kctx := k.Context
	if kctx == "" && len(workloads) > 0 {
		out, err := k.Kubectl.Run(ctx, Cmd{Args: []string{"config", "current-context"}})
		if err != nil {
			return fmt.Errorf("kubectl config current-context: %w", err)
		}
		kctx = strings.TrimSpace(string(out))
	}
	if err := in.restartWorkloads(ctx, workloads); err != nil {
		cmd := fmt.Sprintf("kubectl rollout restart %s --namespace %s --context %s", strings.Join(workloads, " "), k.Namespace, shellQuote(kctx))
		return fmt.Errorf("%w\nonce it can start, `%s` restarts what reads them", err, cmd)
	}
	return nil
}

// withFilesValue sets the files Secret's name, the secret files' one's and the keys delivered as
// files (asFile) in the values a release ran with.
func withFilesValue(values []byte, name, secretFiles string, asFile []string) ([]byte, error) {
	var v map[string]any
	if err := yaml.Unmarshal(values, &v); err != nil {
		return nil, fmt.Errorf("the release's values: %w", err)
	}
	if v == nil {
		v = map[string]any{}
	}
	block, _ := v[helm.ValuesKey].(map[string]any)
	if block == nil {
		block = map[string]any{}
	}
	block["files"] = name
	block["secretFiles"] = secretFiles
	block["asFile"] = append([]string{}, asFile...)
	v[helm.ValuesKey] = block
	return yaml.Marshal(v)
}
