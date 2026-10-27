package install

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/stowage/internal/lint"
	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/spec"
)

// The provision hook runs as a service Stowage adds to the stack, from its own compose file.
const (
	ProvisionService = "stowage-provision"
	ProvisionCompose = "stowage-provision.yaml"
	// ProvisionEnv records what the hook returned, KEY='value' as in .env; the next run receives
	// it, so a value the hook minted comes back to it.
	ProvisionEnv = "provision.env"
	// lifecycleLock is held for a whole provision, upgrade, rollback or reset-db: one at a time per install.
	lifecycleLock = ".stowage.lock"
)

var errLocked = errors.New("locked")

// Bounds on one run of the hook.
var (
	provisionTimeout  = 30 * time.Minute
	maxProvisionOut   = 1 << 20  // bytes of stdout
	maxProvisionLine  = 64 << 10 // a recorded line stays within maxEnvLine
	maxProvisionKeys  = 256
	maxProvisionTotal = 256 << 10 // bytes provision.env may hold, across runs
	maxBadLinesListed = 5
)

// hook returns the release's provision hook, nil when it has none.
func (in *Installer) hook() (*providers.ProvisionHook, error) {
	h, err := providers.DecodeProvision(in.R.Spec.Short(), in.R.Spec.Hooks.Provision, in.R.Spec.Images)
	if err != nil {
		return nil, fmt.Errorf("hooks.provision: %w", err)
	}
	return h, nil
}

// hookEnv lists the recorded keys the provision hook receives: the values Stowage provides, each
// route's URL, and the keys its env declares. Nothing else of .env reaches it: not a secret it
// did not ask for, nor an image, a port or compose's own settings.
func (in *Installer) hookEnv(env *EnvFile, h *providers.ProvisionHook) []string {
	keys := in.providedEnv(env)
	for _, k := range h.Keys() {
		if _, ok := env.Get(k); ok && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

// providedEnv lists the recorded values Stowage provides, each route's URL and each L4 route's
// endpoint.
func (in *Installer) providedEnv(env *EnvFile) []string {
	var keys []string
	add := func(k string) {
		if _, ok := env.Get(k); ok && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	for _, k := range sortedKeys(lint.ProvidedVars) {
		add(k)
	}
	for _, r := range in.R.Spec.Front.Routes {
		add(spec.URLVar(r.Name))
	}
	for _, r := range in.R.Spec.L4.Routes {
		add(spec.L4Var(r.Name))
	}
	return keys
}

// recordedEnv lists the recorded keys a `command` provider's container receives: the values
// Stowage provides, each route's URL and domain, the inputs, the secrets and the safe defaults.
// Images, ports and compose's own settings stay out.
func (in *Installer) recordedEnv(env *EnvFile) []string {
	s := in.R.Spec
	keys := in.providedEnv(env)
	add := func(k string) {
		if _, ok := env.Get(k); ok && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	for _, i := range s.Inputs {
		add(i.Key)
	}
	for _, r := range s.Front.Routes {
		add(r.DomainInput)
	}
	for _, sec := range s.Secrets {
		add(sec.Key)
	}
	for _, k := range sortedKeys(s.SafeDefaults) {
		add(k)
	}
	return keys
}

// confined is a service Stowage runs on demand in the stack (behind its own profile): cmd as the
// container's whole command line, no capability, no privilege gain, no file of the host, and the
// recorded values by name (compose takes them from the environment it runs with).
func confined(service, image string, cmd, keys []string) map[string]any {
	c := make([]string, len(cmd))
	for i, a := range cmd {
		c[i] = strings.ReplaceAll(a, "$", "$$") // compose interpolates $; the spec's cmd is literal
	}
	return map[string]any{
		"image":        image,
		"entrypoint":   c,
		"profiles":     []string{service},
		"cap_drop":     []string{"ALL"},
		"security_opt": []string{"no-new-privileges:true"},
		"environment":  keys,
		"restart":      "no",
	}
}

// serviceFile is a compose file holding one service.
func serviceFile(what, service string, svc map[string]any) ([]byte, error) {
	out, err := yaml.Marshal(map[string]any{"services": map[string]any{service: svc}})
	if err != nil {
		return nil, err
	}
	return append([]byte("# Written by the installer: "+what+", run on demand.\n"), out...), nil
}

// provisionCompose is the hook's service, confined, which also reads the hook's earlier outputs
// from ProvisionEnv, with hosts (targetHosts) as its extra hosts.
func provisionCompose(h *providers.ProvisionHook, keys, hosts []string) ([]byte, error) {
	svc := confined(ProvisionService, "${"+ImageVar(h.Image)+"}", h.Cmd, keys)
	svc["env_file"] = []string{"./" + ProvisionEnv}
	if len(hosts) > 0 {
		svc["extra_hosts"] = hosts
	}
	return serviceFile("the provision hook", ProvisionService, svc)
}

// containerName is the container of a service Stowage runs on demand: one name per install, so a
// container a stopped run left behind is found.
func containerName(env *EnvFile, service string) string {
	project, _ := env.Get("COMPOSE_PROJECT_NAME")
	return project + "-" + service
}

// lockRun takes the install's lifecycle lock, held until the returned func: one provision,
// upgrade or rollback at a time, and nothing it reads or writes changes under it. A hook or
// schema container an earlier run left behind is refused too: it holds the fixed name a run needs.
func (in *Installer) lockRun(ctx context.Context, env *EnvFile) (func(), error) {
	unlock, err := lockFile(in.path(lifecycleLock))
	for deadline := time.Now().Add(in.O.LockWait); errors.Is(err, errLocked) && time.Now().Before(deadline); {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		unlock, err = lockFile(in.path(lifecycleLock))
	}
	if errors.Is(err, errLocked) {
		return nil, errors.New("a provision, upgrade, rollback or reset-db is already running for this install: wait for it to finish")
	}
	if err != nil {
		return nil, err
	}
	if err := in.leftovers(ctx, env); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

// leftovers refuses a run while a provision or schema container an earlier run left is there:
// its fixed name would fail the run's own.
func (in *Installer) leftovers(ctx context.Context, env *EnvFile) error {
	for _, svc := range []string{ProvisionService, SchemaService} {
		name := containerName(env, svc)
		if _, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"container", "inspect", "--format", "{{.State.Status}}", name}}); err == nil {
			return fmt.Errorf("the container %s of an earlier run is still there: once it is not running anything you need, `docker rm --force %s`, then run again", name, name)
		}
	}
	return nil
}

// runHook runs the hook once, under the lock lockRun took.
func (in *Installer) runHook(ctx context.Context, env *EnvFile, h *providers.ProvisionHook) error {
	name := containerName(env, ProvisionService)
	keys := in.hookEnv(env, h)
	reserved := recordedKeys(env)
	outs := NewEnvFile()
	if _, err := os.Stat(in.path(ProvisionEnv)); err == nil {
		prev, err := ReadEnvFile(in.path(ProvisionEnv))
		if err != nil {
			return err
		}
		for _, k := range prev.Keys() {
			if reserved[k] {
				in.O.logf("%s: dropping %s, now a recorded value of the install", in.path(ProvisionEnv), k)
				continue
			}
			if slices.Contains(in.dropped.returns, k) {
				in.O.logf("the provision hook of %s %s no longer returns %s: dropped", in.R.Spec.Name, in.R.Manifest.ProjectVersion, k)
				continue
			}
			v, _ := prev.Get(k)
			outs.Set(k, v)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Written before the run: compose requires the file, and it holds what the last run returned.
	if err := outs.WriteFile(in.path(ProvisionEnv)); err != nil {
		return err
	}
	pc, err := provisionCompose(h, keys, in.targetHosts())
	if err != nil {
		return err
	}
	if err := os.WriteFile(in.path(ProvisionCompose), pc, 0o644); err != nil {
		return err
	}
	in.O.logf("provisioning %s", in.R.Spec.Name)
	runCtx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()
	w := &outputWriter{outs: outs, reserved: reserved, save: func() error { return outs.WriteFile(in.path(ProvisionEnv)) }, stop: cancel}
	base, cleanup, err := in.pullEnviron(env)
	if err != nil {
		return err
	}
	defer cleanup()
	_, runErr := in.O.Docker.Run(runCtx, Cmd{
		Args:   append(in.composeBase(ProvisionCompose), "run", "--rm", "--no-deps", "-T", "--name", name, ProvisionService),
		Env:    env.Environ(base),
		Stream: in.O.Log,
		Stdout: w,
	})
	if runCtx.Err() != nil || w.stopped {
		// The installer stopped the run: only the docker CLI died, the container is removed here.
		rmCtx, rmCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		if _, err := in.O.Docker.Run(rmCtx, Cmd{Args: []string{"rm", "--force", name}}); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("the hook's container may still be running: `docker rm --force %s` (%w)", name, err))
		}
		rmCancel()
	}
	err = errors.Join(runErr, w.close(runErr == nil))
	if h.Outputs != "" {
		err = errors.Join(err, writeOutputs(in.path(h.Outputs), outs))
	}
	if err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("%w (stopped after %s)", err, provisionTimeout)
		}
		return fmt.Errorf("provisioning did not finish: %w\nwhat it returned is recorded in %s; the stack is running: `%s provision --dir %s` continues it%s",
			err, in.path(ProvisionEnv), in.ctl(), in.O.Dir, setHint(outputKeys(outs.Keys(), h, reserved)))
	}
	where := in.path(ProvisionEnv)
	if h.Outputs != "" {
		where = in.path(h.Outputs)
	}
	if ks := outs.Keys(); len(ks) > 0 {
		fmt.Fprintf(in.O.Out, "provisioned: %s (in %s)\n", strings.Join(ks, ", "), where)
	} else {
		fmt.Fprintln(in.O.Out, "provisioned")
	}
	return nil
}

// setHint names, after the command that runs the hook again, how its --set sets a value the hook
// returned or declares it returns (keys): a hook refusing a value that has stopped being true, or
// missing one it cannot mint, is unblocked by it.
func setHint(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	return fmt.Sprintf(", and with `--set KEY=VALUE` sets a value it returned or declares it returns (%s) first, `--set KEY=` forgets one", strings.Join(keys, ", "))
}

// recordedKeys are the keys the install records in env: none is ever a value of the hook's.
func recordedKeys(env *EnvFile) map[string]bool {
	keys := map[string]bool{}
	for _, k := range env.Keys() {
		keys[k] = true
	}
	return keys
}

// outputKeys are the keys `provision --set` may set as the hook's: what it returned and what its
// returns declares, sorted, less any key reserved, which the install records.
func outputKeys(returned []string, h *providers.ProvisionHook, reserved map[string]bool) []string {
	var keys, declared []string
	if h != nil {
		declared = h.Returns
	}
	for _, k := range slices.Concat(returned, declared) {
		if !reserved[k] && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// writeOutputs writes what the hook returned as plain KEY=VALUE lines, readable by the operator
// only.
func writeOutputs(path string, outs *EnvFile) error {
	var b bytes.Buffer
	for _, k := range outs.Keys() {
		v, _ := outs.Get(k)
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	return writePrivate(path, b.Bytes())
}

// writePrivate replaces path with data, mode 0600, atomically.
func writePrivate(path string, data []byte) error {
	return writeAtomic(path, bytes.NewReader(data), 0o600, nil)
}

// writeAtomic replaces path with what r holds and mode, atomically and durably: the file and the
// rename reach the disk before it returns. check, when set, vets the written copy first.
func writeAtomic(path string, r io.Reader, mode os.FileMode, check func(string) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".stowage-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, r)
	if err = errors.Join(err, tmp.Chmod(mode), tmp.Sync(), tmp.Close()); err != nil {
		return err
	}
	if check != nil {
		if err := check(tmp.Name()); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// outputWriter reads the hook's stdout as it arrives: each KEY=VALUE line is recorded, and saved
// when it changes a value, so a value the hook minted survives a run that stops later. A line that
// is not one, or longer than maxProvisionLine, is reported at the end; empty lines are skipped.
// Past maxProvisionOut bytes the run is stopped.
type outputWriter struct {
	outs     *EnvFile
	reserved map[string]bool
	save     func() error
	buf      []byte
	n        int
	line     int
	bad      []string
	nbad     int
	saveErr  error
	stop     func() // stops the run
	stopped  bool   // a Write refused: the run was stopped
	refused  error  // why: the docker CLI, killed, reports only its signal
	total    int    // bytes of the recorded keys and values
}

// refuse stops the run on a write past the bounds.
func (w *outputWriter) refuse(err error) (int, error) {
	w.stopped = true
	if w.refused == nil {
		w.refused = err
	}
	if w.stop != nil {
		w.stop()
	}
	return 0, err
}

func (w *outputWriter) Write(p []byte) (int, error) {
	w.n += len(p)
	if w.n > maxProvisionOut {
		return w.refuse(fmt.Errorf("the hook wrote more than %d bytes on stdout", maxProvisionOut))
	}
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.parse(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	// A CRLF line's '\r' is trimmed by the parse: a buffer one past the bound by it alone waits for its '\n'.
	if len(w.buf) > maxProvisionLine && !(len(w.buf) == maxProvisionLine+1 && w.buf[len(w.buf)-1] == '\r') {
		return w.refuse(fmt.Errorf("stdout line %d is longer than %d bytes", w.line+1, maxProvisionLine))
	}
	return len(p), nil
}

func (w *outputWriter) parse(line string) {
	w.line++
	line = strings.TrimSuffix(line, "\r")
	if strings.TrimSpace(line) == "" {
		return
	}
	k, v, ok := strings.Cut(line, "=")
	old, known := w.outs.Get(k)
	var why string
	switch {
	case len(line) > maxProvisionLine:
		why = fmt.Sprintf("longer than %d bytes", maxProvisionLine)
	case !ok || !providers.EnvName.MatchString(k):
		why = "not KEY=VALUE"
	case w.reserved[k] || strings.HasPrefix(k, "STOWAGE_") || strings.HasPrefix(k, "COMPOSE_"):
		why = k + " is a value the install records, not one a hook returns"
	default:
		if err := spec.CheckEnvValue(v); err != nil {
			why = fmt.Sprintf("%s: %v", k, err)
		} else if !known && len(w.outs.keys) >= maxProvisionKeys {
			why = fmt.Sprintf("more than %d outputs", maxProvisionKeys)
		} else if w.size()+w.grow(k, old, v, known) > maxProvisionTotal {
			why = fmt.Sprintf("the outputs would pass %d bytes", maxProvisionTotal)
		}
	}
	if why != "" {
		w.nbad++
		if len(w.bad) < maxBadLinesListed {
			w.bad = append(w.bad, fmt.Sprintf("stdout line %d: %s", w.line, why))
		}
		return
	}
	if known && old == v {
		return
	}
	w.total += w.grow(k, old, v, known)
	w.outs.Set(k, v)
	if err := w.save(); err != nil && w.saveErr == nil {
		w.saveErr = err
	}
}

// size is what the recorded outputs hold, keys and values. The outputs a run starts from are
// counted once, then each change.
func (w *outputWriter) size() int {
	if w.total == 0 {
		for _, k := range w.outs.keys {
			w.total += len(k) + len(w.outs.vals[k])
		}
	}
	return w.total
}

// grow is how much setting k to v changes size.
func (w *outputWriter) grow(k, old, v string, known bool) int {
	if known {
		return len(v) - len(old)
	}
	return len(k) + len(v)
}

// close reports what was refused. A last line without a line break is recorded only from a run
// that finished: a stopped one may have been cut mid-value.
func (w *outputWriter) close(finished bool) error {
	if len(w.buf) > 0 {
		if finished {
			w.parse(string(w.buf))
		} else if strings.TrimSpace(string(w.buf)) != "" {
			w.line++
			w.nbad++
			w.bad = append(w.bad, fmt.Sprintf("stdout line %d: not recorded, it has no line break and the run did not finish", w.line))
		}
	}
	w.buf = nil
	if w.saveErr != nil || w.refused != nil {
		return errors.Join(w.refused, w.saveErr)
	}
	if w.nbad == 0 {
		return nil
	}
	more := ""
	if w.nbad > len(w.bad) {
		more = fmt.Sprintf("; %d more", w.nbad-len(w.bad))
	}
	return fmt.Errorf("the hook's stdout carries KEY=VALUE lines only (progress goes to stderr): %s%s", strings.Join(w.bad, "; "), more)
}

// Provision runs the provision hook of an install of this release again, on its running stack.
// --set corrects an input only the hook reads, or sets a value the hook returned before or declares
// it returns, and records it first.
func (in *Installer) Provision(ctx context.Context) error {
	o := &in.O
	h, err := in.hook()
	if err != nil {
		return err
	}
	if h == nil {
		return fmt.Errorf("%s has no provision hook", in.R.Spec.Name)
	}
	env, unlock, err := in.openLocked(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := in.sameRelease(ctx, env); err != nil {
		return err
	}
	var inputs, outs map[string]string
	var returned *EnvFile
	if len(o.Set) > 0 {
		if returned, err = in.returned(); err != nil {
			return err
		}
		if inputs, outs, err = in.setHookValues(env, h, returned); err != nil {
			return err
		}
	}
	if err := in.volumesSplit(); err != nil {
		return err
	}
	if err := in.checkDocker(ctx); err != nil {
		return err
	}
	if len(inputs) > 0 {
		for _, k := range sortedKeys(inputs) {
			env.Set(k, inputs[k])
		}
		if err := env.WriteFile(in.path(envName)); err != nil {
			return err
		}
		o.logf("recorded %s in %s", strings.Join(sortedKeys(inputs), ", "), in.path(envName))
	}
	if len(outs) > 0 {
		setReturned(returned, outs)
		if err := returned.WriteFile(in.path(ProvisionEnv)); err != nil {
			return err
		}
		o.logf("recorded %s in %s", strings.Join(sortedKeys(outs), ", "), in.path(ProvisionEnv))
	}
	if err := in.provisionNow(ctx, env, h); err != nil {
		return err
	}
	// Run by the operator: the data is what they chose, the wait a --secrets install began is over.
	in.restoreDone()
	return nil
}

// returned is what the hook returned before, as ProvisionEnv records it; empty when it has not run.
func (in *Installer) returned() (*EnvFile, error) {
	f, err := ReadEnvFile(in.path(ProvisionEnv))
	if errors.Is(err, os.ErrNotExist) {
		return NewEnvFile(), nil
	}
	return f, err
}

// setReturned replaces the values the hook returned with set: an empty value forgets the key, so
// the hook runs without it (and mints it again, when it mints it).
func setReturned(returned *EnvFile, set map[string]string) {
	for _, k := range sortedKeys(set) {
		if set[k] == "" {
			returned.Delete(k)
		} else {
			returned.Set(k, set[k])
		}
	}
}

// provisionNow runs the provision hook h on the running stack, the data it is to provision loaded
// (`provision`, or a restore): a first install whose stack came up and whose hook failed is
// finished by it.
func (in *Installer) provisionNow(ctx context.Context, env *EnvFile, h *providers.ProvisionHook) error {
	if err := in.runHook(ctx, env, h); err != nil {
		return err
	}
	if _, _, up := in.firstInstall(); up {
		in.installDone()
	}
	return nil
}

var composeWord = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// passesEnvOn refuses a compose file through which the stack may read the install's .env: an
// env_file, a bind mount of the install directory, of a directory above it or of .env, a bind
// source compose would expand ($, ~), or a volume with driver options (which may bind anything).
func (in *Installer) passesEnvOn() error {
	var doc struct {
		Services map[string]struct {
			EnvFile any   `yaml:"env_file"`
			Volumes []any `yaml:"volumes"`
		} `yaml:"services"`
		Volumes map[string]struct {
			DriverOpts map[string]any `yaml:"driver_opts"`
		} `yaml:"volumes"`
	}
	if err := yaml.Unmarshal(in.R.Compose, &doc); err != nil {
		return fmt.Errorf("compose.yaml: %w", err)
	}
	refuse := func(why string) error {
		return fmt.Errorf("--set: %s may pass the install's .env to the stack: a re-run, `%s --dir %s --set KEY=VALUE`, changes an input and brings the stack up again", why, in.ctl(), in.O.Dir)
	}
	for _, name := range sortedKeys(doc.Volumes) {
		if len(doc.Volumes[name].DriverOpts) > 0 {
			return refuse("volume " + name + "'s driver_opts")
		}
	}
	for _, name := range sortedKeys(doc.Services) {
		svc := doc.Services[name]
		if svc.EnvFile != nil {
			return refuse("service " + name + "'s env_file")
		}
		for _, v := range svc.Volumes {
			var src string
			switch v := v.(type) {
			case string:
				src, _, _ = strings.Cut(v, ":")
			case map[string]any:
				src, _ = v["source"].(string)
			}
			if src != "" && bindsEnv(in.O.Dir, src) {
				return refuse(fmt.Sprintf("service %s's mount of %s", name, src))
			}
		}
	}
	return nil
}

// bindsEnv reports whether a mount source may reach dir/.env: dir itself, a directory above it,
// .env, or a path compose would expand. A named volume, read as a path inside dir, never does.
func bindsEnv(dir, src string) bool {
	if strings.ContainsAny(src, "$~") {
		return true
	}
	p := src
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	p = filepath.Clean(p)
	rel, err := filepath.Rel(p, filepath.Join(dir, envName))
	return err == nil && (rel == "." || !strings.HasPrefix(rel, ".."))
}

// setHookValues sorts --set: an input the install recorded that nothing but the hook reads (its
// env lists it), or a key the hook returned before (returned) or declares it returns. One the stack
// or the front reads is changed by a re-run, which brings the stack up again; any other key is
// refused.
func (in *Installer) setHookValues(env *EnvFile, h *providers.ProvisionHook, returned *EnvFile) (inputs, outs map[string]string, err error) {
	settable := in.hookInputs(env, h)
	outputs := outputKeys(returned.Keys(), h, recordedKeys(env)) // a key the install records is never the hook's
	inputs, outs = map[string]string{}, map[string]string{}
	var bad []string
	for _, k := range sortedKeys(in.O.Set) {
		v := in.O.Set[k]
		i, isInput := settable[k]
		isOutput := slices.Contains(outputs, k)
		switch {
		case !isInput && !isOutput:
			bad = append(bad, fmt.Sprintf("%s is neither an input only the provision hook reads (%s) nor a value it returned or declares it returns (%s); a re-run, `%s --dir %s --set %s=…`, changes an input the stack reads",
				k, cmp.Or(strings.Join(sortedKeys(settable), ", "), "none"), cmp.Or(strings.Join(outputs, ", "), "none"), in.ctl(), in.O.Dir, k))
			continue
		case isInput && v == "" && i.Required:
			bad = append(bad, fmt.Sprintf("%s is required", k))
			continue
		}
		if err := spec.CheckEnvValue(v); err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", k, err))
			continue
		}
		if isInput {
			inputs[k] = v
		} else {
			outs[k] = v
		}
	}
	if len(bad) > 0 {
		return nil, nil, fmt.Errorf("--set: %s", strings.Join(bad, "; "))
	}
	if len(inputs) > 0 {
		// Conservative: a stack that may read .env itself may read any of them.
		if err := in.passesEnvOn(); err != nil {
			return nil, nil, err
		}
	}
	return inputs, outs, nil
}

// hookInputs are the inputs the install recorded that only the provision hook reads: its env
// lists them, and the compose file names none of them anywhere — interpolated, passed by name, in
// a comment (conservative) — nor does the front.
func (in *Installer) hookInputs(env *EnvFile, h *providers.ProvisionHook) map[string]spec.Input {
	stack := map[string]bool{}
	for _, w := range composeWord.FindAll(in.R.Compose, -1) {
		stack[string(w)] = true
	}
	for _, r := range in.R.Spec.Front.Routes {
		stack[r.DomainInput] = true // the front's
	}
	settable := map[string]spec.Input{}
	for _, i := range in.R.Spec.Inputs {
		if _, recorded := env.Get(i.Key); recorded && !stack[i.Key] && slices.Contains(h.Keys(), i.Key) {
			settable[i.Key] = i
		}
	}
	return settable
}
