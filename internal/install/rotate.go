package install

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/spec"
)

// The passes of a secret's rotation: stage records a new value as KEY_NEXT, which the services
// accept but do not use; swap makes it KEY and keeps the old one as KEY_PREVIOUS, still accepted;
// finish drops KEY_PREVIOUS. Between two passes every service runs with both values it may meet,
// so a rolling restart never refuses the other age's tokens. replace is a secret's one pass
// without rotation: true — a new KEY at once, the services reading it recreated, nothing of the
// old value kept.
const (
	RotateStage   = "stage"
	RotateSwap    = "swap"
	RotateFinish  = "finish"
	RotateReplace = "replace"
)

// NextVar and PreviousVar are a rotated secret's staged and previous values: empty outside a
// rotation, recorded beside it from the first install.
func NextVar(key string) string     { return key + "_NEXT" }
func PreviousVar(key string) string { return key + "_PREVIOUS" }

// AcceptedVar is the other value a rotated secret's services accept beside KEY: KEY_NEXT while
// one is staged, else KEY_PREVIOUS, empty outside a rotation (the two are never both set). A
// service that accepts one more value reads it alone.
func AcceptedVar(key string) string { return key + "_ACCEPTED" }

// acceptedVars records each rotated secret's AcceptedVar from the next and previous values get
// reads.
func acceptedVars(secrets []spec.Secret, get func(string) string, set func(k, v string)) {
	for _, s := range secrets {
		if s.Rotation {
			set(AcceptedVar(s.Key), cmp.Or(get(NextVar(s.Key)), get(PreviousVar(s.Key))))
		}
	}
}

// The ways `secrets rotate` changes a secret, as `describe --json` names them: RotatePasses its three
// passes (rotation: true), RotateReplace its one pass (replace: true), RotateNone none.
const (
	RotatePasses = "passes"
	RotateNone   = "none"
)

// rotateWay is how `secrets rotate` changes sec on shape (onDocker, onK8s, onVM), from the spec
// alone: RotatePasses for rotation: true, RotateReplace for replace: true but on Kubernetes (`k8s
// secrets rotate` takes the three passes alone), RotateNone otherwise. Every reader of the way
// asks it (describe, the verbs, the daemon), so they agree; lint holds rotation: true and
// replace: true off one secret.
func rotateWay(sec spec.Secret, shape string) string {
	switch {
	case sec.Rotation:
		return RotatePasses
	case sec.Replace && shape != onK8s:
		return RotateReplace
	}
	return RotateNone
}

// settableOn is whether `secrets set` records a value the operator chooses for sec on shape:
// Kubernetes has no `secrets set`.
func settableOn(sec spec.Secret, shape string) bool { return sec.Settable && shape != onK8s }

// secretsCmd is the `secrets` verb args as its operator types it on shape: Docker's naming the
// install's directory, the VM's run as root (over the daemon), Kubernetes's under `k8s`.
func (in *Installer) secretsCmd(shape, args string) string {
	switch shape {
	case onK8s:
		return in.k8sCtl("secrets " + args) // the release it acts on named
	case onVM:
		return in.vmCtl() + " secrets " + args
	}
	return shellQuote(in.ctl()) + " secrets " + args + " --dir " + shellQuote(in.O.Dir)
}

// rotation is the spec's secret key for pass on shape: one whose way (rotateWay) is RotatePasses
// takes its three passes, RotateReplace that pass alone, RotateNone none. Each no is a refusal:
// nothing changed.
func (in *Installer) rotation(shape, key, pass string) (spec.Secret, error) {
	i := slices.IndexFunc(in.R.Spec.Secrets, func(s spec.Secret) bool { return s.Key == key })
	if i < 0 {
		var names []string
		for _, s := range in.R.Spec.Secrets {
			names = append(names, s.Key)
		}
		return spec.Secret{}, refuse("", "%s is no secret of %s (secrets: %s)", key, in.R.Spec.Name, cmp.Or(strings.Join(names, ", "), "none"))
	}
	sec, stage, one := in.R.Spec.Secrets[i], in.secretsCmd(shape, "rotate "+key+" --stage"), in.secretsCmd(shape, "rotate "+key)
	switch way := rotateWay(sec, shape); {
	case way == RotatePasses && pass == RotateReplace:
		return spec.Secret{}, refuse(stage, "%s has rotation: true: it is replaced in three passes, `%s` first", key, stage)
	case way == RotateReplace && pass != RotateReplace:
		return spec.Secret{}, refuse(one, "%s has replace: true: `%s` replaces it in one pass, without --stage or --finish", key, one)
	case way != RotateNone:
		return sec, nil
	case sec.Replace:
		return spec.Secret{}, refuse("", "%s has replace: true, which `secrets rotate` replaces in one pass on Docker and the VM: `k8s secrets rotate` takes a secret with rotation: true alone", key)
	}
	var ways []string
	for _, s := range in.R.Spec.Secrets {
		if rotateWay(s, shape) != RotateNone {
			ways = append(ways, s.Key)
		}
	}
	return spec.Secret{}, refuse("", "%s has neither rotation: true nor replace: true: its release lets `secrets rotate` change it in no way (rotated or replaced: %s)", key, cmp.Or(strings.Join(ways, ", "), "none"))
}

// rotationVars records, for each rotated secret, an empty next and previous value where none is.
func rotationVars(secrets []spec.Secret, has func(string) bool, set func(k, v string)) {
	for _, s := range secrets {
		if !s.Rotation {
			continue
		}
		for _, k := range []string{NextVar(s.Key), PreviousVar(s.Key)} {
			if !has(k) {
				set(k, "")
			}
		}
	}
}

// rotatePass applies one pass of sec's rotation to the values get reads and set writes, its
// AcceptedVar with them, and says what it did. A pass out of order is refused.
func (in *Installer) rotatePass(shape string, sec spec.Secret, pass string, get func(string) string, set func(k, v string)) (string, error) {
	what, err := in.rotateValues(shape, sec, pass, get, set)
	if err == nil {
		acceptedVars([]spec.Secret{sec}, get, set)
	}
	return what, err
}

// rotateValues is rotatePass on KEY, KEY_NEXT and KEY_PREVIOUS.
func (in *Installer) rotateValues(shape string, sec spec.Secret, pass string, get func(string) string, set func(k, v string)) (string, error) {
	k, next, prev := sec.Key, NextVar(sec.Key), PreviousVar(sec.Key)
	// The next pass's command, as its operator types it: a pass out of order is refused (nothing
	// recorded yet), fixed by the pass that comes first.
	run := func(flag string) string { return in.secretsCmd(shape, strings.TrimSpace("rotate "+k+" "+flag)) }
	// A pass whose state is already there (a run that recorded it, then failed to restart) is run
	// again: the stack is brought up with it.
	switch pass {
	case RotateStage:
		switch {
		case get(next) != "":
			return fmt.Sprintf("a new %s is staged already: bringing it up again; `%s` then puts it in use", k, run("")), nil
		case get(prev) != "":
			return "", refuse(run("--finish"), "the last rotation of %s is not finished: `%s` ends it", k, run("--finish"))
		}
		v, err := generate(in.O.rand(), sec.Gen)
		if err != nil {
			return "", fmt.Errorf("secret %s: %w", k, err)
		}
		set(next, v)
		return fmt.Sprintf("staged a new %s as %s; once every service runs with it, `%s` puts it in use", k, next, run("")), nil
	case RotateSwap:
		switch {
		case get(next) == "" && get(prev) != "":
			return fmt.Sprintf("%s is in use already, the old one kept as %s: bringing it up again; `%s` then drops it", k, prev, run("--finish")), nil
		case get(next) == "":
			return "", refuse(run("--stage"), "no new %s is staged: `%s` first", k, run("--stage"))
		}
		set(prev, get(k))
		set(k, get(next))
		set(next, "")
		return fmt.Sprintf("%s is the new value, the old one kept as %s; once nothing uses it, `%s` drops it", k, prev, run("--finish")), nil
	case RotateFinish:
		switch {
		case get(next) != "":
			return "", refuse(run(""), "a new %s is staged, not in use: `%s` puts it in use first", k, run(""))
		case get(prev) == "":
			return fmt.Sprintf("%s keeps no previous value: bringing the stack up again", k), nil
		}
		set(prev, "")
		return fmt.Sprintf("dropped the previous %s: the rotation is done", k), nil
	case RotateReplace:
		v, err := generate(in.O.rand(), sec.Gen)
		if err != nil {
			return "", fmt.Errorf("secret %s: %w", k, err)
		}
		set(k, v)
		return fmt.Sprintf("recorded a new %s: the services reading it are recreated; `secrets export` carries it from now on", k), nil
	}
	return "", fmt.Errorf("unknown pass %q", pass)
}

// RotateSecret runs one pass of a secret's rotation (rotation) on the Docker install in O.Dir, then
// brings the stack up again with it.
func (in *Installer) RotateSecret(ctx context.Context, key, pass string) error {
	sec, err := in.rotation(in.fixOn(onDocker, onVM), key, pass)
	if err != nil {
		return err
	}
	env, unlock, err := in.openLocked(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := in.sameRelease(ctx, env); err != nil {
		return err
	}
	// Before anything is recorded: nothing would come up to use it.
	if err := in.volumesSplit(); err != nil {
		return err
	}
	if err := in.checkDocker(ctx); err != nil {
		return err
	}
	get := func(k string) string { v, _ := env.Get(k); return v }
	what, err := in.rotatePass(in.fixOn(onDocker, onVM), sec, pass, get, env.Set)
	if err != nil {
		return err
	}
	in.fileVars(env) // a secret delivered as a file: the new value's path, recorded with it
	if err := in.writeEnv(env); err != nil {
		return err
	}
	in.O.logf("%s", what)
	if err := in.fetchImages(ctx, env); err != nil {
		return err
	}
	if err := in.writeStack(env); err != nil {
		return err
	}
	return in.start(ctx, env)
}

// K8sRotateSecret runs one pass of a rotated secret's rotation on the helm release this installer
// carries: its Secret changes, then each of the release's workloads that reads the key restarts,
// one rollout after the other.
func (in *Installer) K8sRotateSecret(ctx context.Context, key, pass string) error {
	k := in.O.K8s
	sec, err := in.rotation(onK8s, key, pass)
	if err != nil {
		return err
	}
	if err := in.checkTarget(); err != nil {
		return err
	}
	if _, err := in.checkHelm(ctx); err != nil {
		return err
	}
	cur, err := in.k8sCurrent(ctx)
	if err != nil {
		return err
	}
	if r, m := cur.Release, in.R.Manifest; r.Version != m.ProjectVersion || r.Revision != m.Revision {
		return fmt.Errorf("release %s in %s runs %s %s (revision %d), and this installer carries %s (revision %d): rotate with the installer of %s",
			k.Release, k.Namespace, m.Name, r.Version, r.Revision, m.ProjectVersion, m.Revision, r.Version)
	}
	rec, err := in.readSecret(ctx, k.envSecret())
	if err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("release %s in %s has no Secret %s", k.Release, k.Namespace, k.envSecret())
	}
	what, err := in.rotatePass(onK8s, sec, pass, func(k string) string { return rec[k] }, func(k, v string) { rec[k] = v })
	if err != nil {
		return err
	}
	manifest, err := in.helm(ctx, Cmd{Args: []string{"get", "manifest", k.Release}})
	if err != nil {
		return err
	}
	workloads, err := releaseWorkloads(manifest, k.Namespace, k.envSecret(), []string{key, NextVar(key), PreviousVar(key), AcceptedVar(key)})
	if err != nil {
		return err
	}
	if err := in.applySecrets(ctx, deploy{env: rec}); err != nil {
		return err
	}
	in.O.logf("%s", what)
	return in.restartWorkloads(ctx, workloads)
}

// restartWorkloads restarts each workload, one at a time, waiting for it to be ready.
func (in *Installer) restartWorkloads(ctx context.Context, workloads []string) error {
	for _, w := range workloads {
		in.O.logf("kubectl: restarting %s", w)
		if _, err := in.kubectl(ctx, Cmd{Args: []string{"rollout", "restart", w}}); err != nil {
			return err
		}
		if _, err := in.kubectl(ctx, Cmd{Args: []string{"rollout", "status", w, "--timeout", in.O.K8s.Timeout.String()}}); err != nil {
			return fmt.Errorf("%s did not become ready: %w", w, err)
		}
	}
	return nil
}

// releaseWorkloads are the Deployments, StatefulSets and DaemonSets a release's manifest makes in
// namespace that read one of keys from the Secret secret — a `secretKeyRef` naming the key, or the
// whole Secret by `envFrom` or a volume — as kind/name, in the manifest's order.
func releaseWorkloads(manifest []byte, namespace, secret string, keys []string) ([]string, error) {
	var out []string
	d := yaml.NewDecoder(bytes.NewReader(manifest))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
			Spec any `yaml:"spec"`
		}
		err := d.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("the release's manifest: %w", err)
		}
		if doc.Metadata.Namespace != "" && doc.Metadata.Namespace != namespace {
			continue
		}
		if slices.Contains([]string{"Deployment", "StatefulSet", "DaemonSet"}, doc.Kind) && readsSecret(doc.Spec, secret, keys) {
			out = append(out, strings.ToLower(doc.Kind)+"/"+doc.Metadata.Name)
		}
	}
}

// readsSecret reports whether a workload's spec reads one of keys from secret: a secretKeyRef of
// that Secret and key, or the whole Secret (an envFrom secretRef, a secret volume).
func readsSecret(spec any, secret string, keys []string) bool {
	switch v := spec.(type) {
	case map[string]any:
		if ref, ok := v["secretKeyRef"].(map[string]any); ok && ref["name"] == secret {
			if k, _ := ref["key"].(string); slices.Contains(keys, k) {
				return true
			}
		}
		if ref, ok := v["secretRef"].(map[string]any); ok && ref["name"] == secret {
			return true
		}
		// A secret volume names it secretName; a projected volume's source, name.
		if vol, ok := v["secret"].(map[string]any); ok && (vol["secretName"] == secret || vol["name"] == secret) {
			return true
		}
		for _, x := range v {
			if readsSecret(x, secret, keys) {
				return true
			}
		}
	case []any:
		for _, x := range v {
			if readsSecret(x, secret, keys) {
				return true
			}
		}
	}
	return false
}
