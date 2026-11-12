package install

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/norvexlabs/stowage/internal/kit"
	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/spec"
)

const (
	// KitService and KitCompose are the one-shot container a handoff's run or check is on Docker and
	// the VM, and the file naming it, removed after each run.
	KitService = "stowage-kit"
	KitCompose = ".stowage-kit.yaml"

	// DefaultKitExpiry is how long a kit is good for when --expires does not say.
	DefaultKitExpiry = 7 * 24 * time.Hour

	// The results of a handoff's check: its exit status; any other is "not proven".
	KitProven  = 0
	KitUnknown = 2
)

// kitInstanceName is what a kit's instance may be: it names a file and reaches a script's environment.
var kitInstanceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// checkKitInstance refuses what no script should get, nor a file be named after.
func checkKitInstance(instance string) error {
	if !kitInstanceName.MatchString(instance) {
		return fmt.Errorf("the instance %q: letters, digits, '.', '_' and '-', starting with a letter or digit, at most 63", instance)
	}
	return nil
}

// KitRequest is what a kit is issued from.
type KitRequest struct {
	Handoff  string            `json:"handoff"`
	Instance string            `json:"instance"`
	Answers  map[string]string `json:"answers,omitempty"`
	Expires  time.Duration     `json:"expires,omitempty"`
}

// IssuedKit is a kit sealed, its code and what the run said.
type IssuedKit struct {
	Kit  string `json:"kit"`
	Code string `json:"code"`
	Log  string `json:"log,omitempty"`
}

// handoff finds a handoff, naming those there are.
func (in *Installer) handoff(name string) (*spec.Handoff, error) {
	s := in.R.Spec
	if h := s.Handoff(name); h != nil {
		return h, nil
	}
	var names []string
	for _, h := range s.Handoffs {
		names = append(names, h.Name)
	}
	return nil, fmt.Errorf("%s declares no handoff %q (handoffs: %s)", s.Name, name, cmp.Or(strings.Join(names, ", "), "none"))
}

// checkKitRequest refuses what no run should start with: an instance no script should get, an answer
// missing, one the handoff does not ask, or an expiry already past.
func (in *Installer) checkKitRequest(req *KitRequest) (*spec.Handoff, error) {
	h, err := in.handoff(req.Handoff)
	if err != nil {
		return nil, err
	}
	if err := checkKitInstance(req.Instance); err != nil {
		return nil, err
	}
	if req.Expires < 0 {
		return nil, errors.New("--expires: a kit expires after now")
	}
	req.Expires = cmp.Or(req.Expires, DefaultKitExpiry)
	for _, key := range sortedKeys(req.Answers) {
		if !h.Asks(key) {
			return nil, fmt.Errorf("the handoff %s does not ask %s", h.Name, key)
		}
	}
	for _, a := range h.Ask {
		v := req.Answers[a.Key]
		if v == "" {
			return nil, fmt.Errorf("the handoff %s asks %s (%s): --set or --answers gives it, or a terminal asks it", h.Name, a.Key, a.Prompt)
		}
		if err := spec.CheckEnvValue(v); err != nil {
			return nil, fmt.Errorf("%s: %w", a.Key, err)
		}
	}
	return h, nil
}

// kitVars is what a run or check gets beyond the keys it lists: which kit it is, and what was asked.
func kitVars(h *spec.Handoff, instance string, answers map[string]string) map[string]string {
	vars := map[string]string{"STOWAGE_KIT_NAME": h.Name, "STOWAGE_KIT_INSTANCE": instance, "STOWAGE_KIT_FOR": h.For}
	for k, v := range answers {
		vars[k] = v
	}
	return vars
}

// kitContainer runs step once in a confined container, as the provision hook runs, with vars set and
// each file input or secret delivered as a file its env lists mounted as the hook's, its stdout to
// stdout; its failure, the container gone. runCtx is the run's; stopped says a Write stopped it.
func (in *Installer) kitContainer(ctx, runCtx context.Context, env *EnvFile, step *spec.HandoffStep, vars map[string]string, stdout io.Writer, stopped func() bool) error {
	if err := in.leftovers(ctx, env); err != nil {
		return err
	}
	name := containerName(env, KitService)
	h := &providers.ProvisionHook{Image: step.Image, Cmd: step.Cmd, Env: &step.Env}
	keys := in.hookEnv(env, h)
	files, err := in.writeHookFiles(env, h)
	if err != nil {
		return err
	}
	for _, k := range sortedKeys(vars) {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	base, cleanup, err := in.pullEnviron(env)
	if err != nil {
		return err
	}
	defer cleanup()
	ref, _ := env.Get(ImageVar(step.Image))
	user, err := in.hookUser(runCtx, ref, env.Environ(base)) // within the step's run, its stop included
	if err != nil {
		return err
	}
	svc := confined(KitService, "${"+ImageVar(step.Image)+"}", user, step.Cmd, keys)
	mountFiles(svc, files, in.fileVarOf)
	if hosts := in.targetHosts(); len(hosts) > 0 {
		svc["extra_hosts"] = hosts
	}
	file, err := serviceFile("a handoff's step", KitService, svc)
	if err != nil {
		return err
	}
	if err := os.WriteFile(in.path(KitCompose), file, 0o644); err != nil {
		return err
	}
	defer func() { _ = os.Remove(in.path(KitCompose)) }()
	environ := env.Environ(base)
	for _, k := range sortedKeys(vars) {
		environ = append(environ, k+"="+vars[k])
	}
	_, runErr := in.O.Docker.Run(runCtx, Cmd{
		Args:   append(in.composeBase(KitCompose), "run", "--rm", "--no-deps", "-T", "--name", name, KitService),
		Env:    environ,
		Stream: in.O.Log,
		Stdout: stdout,
	})
	runErr = hookRanAs(runErr, user)
	if runCtx.Err() != nil || stopped() {
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if _, err := in.O.Docker.Run(rmCtx, Cmd{Args: []string{"rm", "--force", name}}); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("the container may still be running: `docker rm --force %s` (%w)", name, err))
		}
	}
	return runErr
}

// IssueKit runs the handoff's step on this install's running stack and seals what it gave.
func (in *Installer) IssueKit(ctx context.Context, req KitRequest, now time.Time) (*IssuedKit, error) {
	h, err := in.checkKitRequest(&req)
	if err != nil {
		return nil, err
	}
	env, unlock, err := in.openLocked(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := in.sameRelease(ctx, env); err != nil {
		return nil, err
	}
	if err := in.checkDocker(ctx); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()
	outs := NewEnvFile()
	w := &outputWriter{outs: outs, reserved: map[string]bool{}, save: func() error { return nil }, stop: cancel}
	in.O.logf("issuing the kit %s for %s, instance %s", h.Name, h.For, req.Instance)
	runErr := in.kitContainer(ctx, runCtx, env, &h.Run, kitVars(h, req.Instance, req.Answers), w, func() bool { return w.stopped })
	if err := errors.Join(runErr, w.close(runErr == nil)); err != nil {
		return nil, fmt.Errorf("the handoff %s's run did not finish, and no kit is written: %w\nwhat it issued meanwhile is the project's to account for", h.Name, err)
	}
	gave := map[string]string{}
	for _, k := range outs.Keys() {
		gave[k], _ = outs.Get(k)
	}
	return in.sealKit(h, &req, gave, now)
}

// sealKit seals what the run gave: every key its gives lists, and nothing else, a KIT_TLS_ key as PEM
// in the stowage-file form.
func (in *Installer) sealKit(h *spec.Handoff, req *KitRequest, gave map[string]string, now time.Time) (*IssuedKit, error) {
	var other, missing []string
	for k := range gave {
		if !slices.Contains(h.Gives, k) {
			other = append(other, k)
		}
	}
	inputs := map[string]string{}
	tls := &kit.TLS{}
	fields := map[string]*string{spec.KitTLSCert: &tls.Cert, spec.KitTLSKey: &tls.Key, spec.KitTLSCA: &tls.CA, spec.KitTLSClientCA: &tls.ClientCA}
	for _, k := range h.Gives {
		v := gave[k]
		if v == "" {
			missing = append(missing, k)
			continue
		}
		field, isTLS := fields[k]
		if !isTLS {
			inputs[k] = v
			continue
		}
		pem, err := decodeFileInput(v)
		if err != nil {
			return nil, fmt.Errorf("the handoff %s's run gave %s: %w", h.Name, k, err)
		}
		*field = string(pem)
	}
	sort.Strings(other)
	sort.Strings(missing)
	if len(other) > 0 {
		return nil, fmt.Errorf("the handoff %s's run gave %s, which its gives does not list: no kit is written", h.Name, strings.Join(other, ", "))
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the handoff %s's run did not give %s: no kit is written", h.Name, strings.Join(missing, ", "))
	}
	k := &kit.Kit{For: h.For, Name: h.Name, Instance: req.Instance, Issued: now.UTC(), Expires: now.UTC().Add(req.Expires), Inputs: inputs}
	if *tls != (kit.TLS{}) {
		k.TLS = tls
	}
	if err := (&Given{Project: k.For, Inputs: k.Inputs, TLS: (*GivenTLS)(k.TLS)}).check("the handoff " + h.Name); err != nil {
		return nil, err
	}
	code, err := kit.NewCode()
	if err != nil {
		return nil, err
	}
	raw, err := kit.Seal(k, code, cmp.Or(in.O.kitWork, kit.WorkFactor))
	if err != nil {
		return nil, fmt.Errorf("the handoff %s: %w", h.Name, err)
	}
	return &IssuedKit{Kit: string(raw), Code: code}, nil
}

// CheckKit runs the handoff's check for an instance: its exit status, 0 proven, 2 unknown. A check that
// does not start is not proven, as nothing proved it; its stdout is not read.
func (in *Installer) CheckKit(ctx context.Context, handoff, instance string) (int, error) {
	h, err := in.handoffCheck(handoff, instance)
	if err != nil {
		return 0, err
	}
	env, unlock, err := in.openLocked(ctx)
	if err != nil {
		return 0, err
	}
	defer unlock()
	if err := in.sameRelease(ctx, env); err != nil {
		return 0, err
	}
	if err := in.checkDocker(ctx); err != nil {
		return 0, err
	}
	runCtx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()
	err = in.kitContainer(ctx, runCtx, env, h.Check, kitVars(h, instance, nil), io.Discard, func() bool { return false })
	if err == nil {
		return KitProven, nil
	}
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) && runCtx.Err() == nil {
		return ec.ExitCode(), nil
	}
	return 0, err
}

// handoffCheck refuses a check no run should start: a handoff without one, or an instance no script
// should get.
func (in *Installer) handoffCheck(handoff, instance string) (*spec.Handoff, error) {
	h, err := in.handoff(handoff)
	if err != nil {
		return nil, err
	}
	if h.Check == nil {
		return nil, fmt.Errorf("the handoff %s declares no check", h.Name)
	}
	if err := checkKitInstance(instance); err != nil {
		return nil, err
	}
	return h, nil
}

// kitTips says, after an install, what this project can issue. Each command works when followed as
// printed: an instance the handoff asks is asked on a terminal.
func (in *Installer) kitTips(shape string) {
	for _, h := range in.R.Spec.Handoffs {
		var cmd string
		switch shape {
		case onK8s:
			cmd = in.k8sCtl("kit create " + h.Name)
		case onVM:
			cmd = "sudo " + in.R.Ctl() + " kit create " + h.Name
		default:
			cmd = fmt.Sprintf("%s kit create %s --dir %s", in.ctl(), h.Name, in.O.Dir)
		}
		fmt.Fprintf(in.O.Out, "tip: `%s` issues a kit for %s: its install takes it with --kit FILE\n", cmd, h.For)
	}
}

// loginTips names, at the end of an install or an upgrade, the settable secrets the install holds
// (the logins an operator signs in with) and the verb that shows one on a terminal.
func (in *Installer) loginTips(shape string) {
	var keys []string
	for _, s := range in.R.Spec.Secrets {
		if s.Settable {
			keys = append(keys, s.Key)
		}
	}
	if len(keys) == 0 {
		return
	}
	var cmd string
	switch shape {
	case onK8s:
		cmd = in.k8sCtl("secrets show KEY")
	case onVM:
		cmd = "sudo " + in.R.Ctl() + " secrets show KEY"
	default:
		cmd = fmt.Sprintf("%s secrets show KEY --dir %s", in.ctl(), in.O.Dir)
	}
	fmt.Fprintf(in.O.Out, "logins    %s: `%s` shows one, on a terminal\n", strings.Join(keys, ", "), cmd)
}
