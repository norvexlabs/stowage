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
// so a rolling restart never refuses the other age's tokens.
const (
	RotateStage  = "stage"
	RotateSwap   = "swap"
	RotateFinish = "finish"
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

// rotated returns the secret key names, which must have rotation: true.
func (in *Installer) rotated(key string) (spec.Secret, error) {
	var names []string
	for _, s := range in.R.Spec.Secrets {
		if s.Rotation {
			if s.Key == key {
				return s, nil
			}
			names = append(names, s.Key)
		}
	}
	return spec.Secret{}, fmt.Errorf("%s is no secret with rotation: true (rotated: %s)", key, cmp.Or(strings.Join(names, ", "), "none"))
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
func (in *Installer) rotatePass(sec spec.Secret, pass string, get func(string) string, set func(k, v string)) (string, error) {
	what, err := in.rotateValues(sec, pass, get, set)
	if err == nil {
		acceptedVars([]spec.Secret{sec}, get, set)
	}
	return what, err
}

// rotateValues is rotatePass on KEY, KEY_NEXT and KEY_PREVIOUS.
func (in *Installer) rotateValues(sec spec.Secret, pass string, get func(string) string, set func(k, v string)) (string, error) {
	k, next, prev := sec.Key, NextVar(sec.Key), PreviousVar(sec.Key)
	ctl := in.R.Ctl() + " secrets rotate " + k
	// A pass whose state is already there (a run that recorded it, then failed to restart) is run
	// again: the stack is brought up with it.
	switch pass {
	case RotateStage:
		switch {
		case get(next) != "":
			return fmt.Sprintf("a new %s is staged already: bringing it up again; `%s` then puts it in use", k, ctl), nil
		case get(prev) != "":
			return "", fmt.Errorf("the last rotation of %s is not finished: `%s --finish` ends it", k, ctl)
		}
		v, err := generate(in.O.rand(), sec.Gen)
		if err != nil {
			return "", fmt.Errorf("secret %s: %w", k, err)
		}
		set(next, v)
		return fmt.Sprintf("staged a new %s as %s; once every service runs with it, `%s` puts it in use", k, next, ctl), nil
	case RotateSwap:
		switch {
		case get(next) == "" && get(prev) != "":
			return fmt.Sprintf("%s is in use already, the old one kept as %s: bringing it up again; `%s --finish` then drops it", k, prev, ctl), nil
		case get(next) == "":
			return "", fmt.Errorf("no new %s is staged: `%s --stage` first", k, ctl)
		}
		set(prev, get(k))
		set(k, get(next))
		set(next, "")
		return fmt.Sprintf("%s is the new value, the old one kept as %s; once nothing uses it, `%s --finish` drops it", k, prev, ctl), nil
	case RotateFinish:
		switch {
		case get(next) != "":
			return "", fmt.Errorf("a new %s is staged, not in use: `%s` puts it in use first", k, ctl)
		case get(prev) == "":
			return fmt.Sprintf("%s keeps no previous value: bringing the stack up again", k), nil
		}
		set(prev, "")
		return fmt.Sprintf("dropped the previous %s: the rotation is done", k), nil
	}
	return "", fmt.Errorf("unknown pass %q", pass)
}

// RotateSecret runs one pass of a rotated secret's rotation on the Docker install in O.Dir, then
// brings the stack up again with it.
func (in *Installer) RotateSecret(ctx context.Context, key, pass string) error {
	sec, err := in.rotated(key)
	if err != nil {
		return err
	}
	env, unlock, err := in.openLocked(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := in.sameRelease(env); err != nil {
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
	what, err := in.rotatePass(sec, pass, get, env.Set)
	if err != nil {
		return err
	}
	if err := env.WriteFile(in.path(envName)); err != nil {
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
	sec, err := in.rotated(key)
	if err != nil {
		return err
	}
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
		return fmt.Errorf("no release %s in %s", k.Release, k.Namespace)
	}
	if err := in.pending(rel); err != nil {
		return err
	}
	cur, err := in.installedRelease(ctx)
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
	what, err := in.rotatePass(sec, pass, func(k string) string { return rec[k] }, func(k, v string) { rec[k] = v })
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
	for _, w := range workloads {
		in.O.logf("kubectl: restarting %s", w)
		if _, err := in.kubectl(ctx, Cmd{Args: []string{"rollout", "restart", w}}); err != nil {
			return err
		}
		if _, err := in.kubectl(ctx, Cmd{Args: []string{"rollout", "status", w, "--timeout", k.Timeout.String()}}); err != nil {
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
