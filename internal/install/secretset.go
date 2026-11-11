package install

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/norvexlabs/stowage/internal/spec"
)

// maxSetSecret bounds a value `secrets set` records: a password or a token, which the host
// daemon's request body carries whole.
const maxSetSecret = 4096

// settable is the spec's secret key, refused unless it has settable: true (the error names those
// that have it).
func (in *Installer) settable(key string) (spec.Secret, error) {
	var names []string
	for _, s := range in.R.Spec.Secrets {
		if s.Settable {
			if s.Key == key {
				return s, nil
			}
			names = append(names, s.Key)
		}
	}
	return spec.Secret{}, fmt.Errorf("%s is no secret with settable: true (settable: %s)", key, cmp.Or(strings.Join(names, ", "), "none"))
}

// checkSetSecret refuses a value `secrets set` cannot record.
func checkSetSecret(v string) error {
	switch {
	case v == "":
		return errors.New("the value is empty: give one on standard input")
	case len(v) > maxSetSecret:
		return fmt.Errorf("the value is over %d bytes", maxSetSecret)
	}
	return spec.CheckEnvValue(v)
}

// checkSetValue refuses a value the settable secret key cannot take: checkSetSecret's, and, for
// the password of a front route's login, one bcrypt cannot take (the route would refuse everyone).
func (in *Installer) checkSetValue(key, v string) error {
	if err := checkSetSecret(v); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	for _, r := range in.R.Spec.Front.Routes {
		if r.BasicAuth != nil && r.BasicAuth.Secret == key {
			if err := checkLoginPassword(v); err != nil {
				return fmt.Errorf("%s, route %s's login password, %w", key, r.Name, err)
			}
		}
	}
	return nil
}

// readSetSecret reads `secrets set`'s value from r: its text, one trailing line break dropped.
func readSetSecret(r io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxSetSecret+3))
	if err != nil {
		return "", fmt.Errorf("standard input: %w", err)
	}
	v := string(raw)
	if t, ok := strings.CutSuffix(v, "\n"); ok {
		v = strings.TrimSuffix(t, "\r")
	}
	return v, checkSetSecret(v)
}

// SetSecret records value as the settable secret key of the Docker install in O.Dir, then brings
// the stack up again with it: the services reading it are recreated.
func (in *Installer) SetSecret(ctx context.Context, key, value string) error {
	if _, err := in.settable(key); err != nil {
		return err
	}
	if err := in.checkSetValue(key, value); err != nil {
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
	if old, _ := env.Get(key); old == value {
		in.O.logf("%s holds that value already: bringing the stack up again", key)
	} else {
		env.Set(key, value)
		in.fileVars(env) // a secret delivered as a file: the new value's path, recorded with it
		if err := env.WriteFile(in.path(envName)); err != nil {
			return err
		}
		in.O.logf("recorded the new %s: the services reading it are recreated; `secrets export` carries it from now on", key)
	}
	if err := in.fetchImages(ctx, env); err != nil {
		return err
	}
	if err := in.writeStack(env); err != nil {
		return err
	}
	return in.start(ctx, env)
}

// ShowSecret returns the value the Docker install in O.Dir records for the settable secret key:
// only when it runs this installer's release, whose spec says which secrets are settable.
func (in *Installer) ShowSecret(ctx context.Context, key string) (string, error) {
	if _, err := in.settable(key); err != nil {
		return "", err
	}
	env, err := in.open()
	if err != nil {
		return "", err
	}
	if err := in.sameRelease(ctx, env); err != nil {
		return "", err
	}
	return recordedSecret(env.Get, key, in.path(envName))
}

// K8sShowSecret returns the value the release's Secret records for the settable secret key.
func (in *Installer) K8sShowSecret(ctx context.Context, key string) (string, error) {
	if _, err := in.settable(key); err != nil {
		return "", err
	}
	if err := in.checkTarget(); err != nil {
		return "", err
	}
	k := in.O.K8s
	if _, err := in.checkHelm(ctx); err != nil {
		return "", err
	}
	rel, err := in.release(ctx)
	if err != nil {
		return "", err
	}
	if rel == nil {
		return "", in.noRelease()
	}
	// The release's spec says which secrets are settable: this installer's only when it carries it.
	cur, err := in.installedRelease(ctx)
	if err != nil {
		return "", err
	}
	if r, m := cur.Release, in.R.Manifest; r.Version != m.ProjectVersion || r.Revision != m.Revision {
		return "", refuse(fmt.Sprintf("the installer of %s", r.Version), "release %s in %s runs %s %s (revision %d), and this installer carries %s (revision %d): show with the installer of %s",
			k.Release, k.Namespace, m.Name, r.Version, r.Revision, m.ProjectVersion, m.Revision, r.Version)
	}
	rec, err := in.k8sRecorded(ctx)
	if err != nil {
		return "", err
	}
	if rec == nil {
		return "", in.noRelease()
	}
	where := k.envSecret()
	if in.R.Spec.AsFile(key) {
		where = k.secretFilesSecret()
	}
	return recordedSecret(func(k string) (string, bool) { v, ok := rec[k]; return v, ok }, key, "the Secret "+where+" in "+k.Namespace)
}

// recordedSecret is key's value as get reads it from where (named in the error when it holds none).
func recordedSecret(get func(string) (string, bool), key, where string) (string, error) {
	v, ok := get(key)
	if !ok || v == "" {
		return "", fmt.Errorf("%s holds no %s", where, key)
	}
	return v, nil
}
