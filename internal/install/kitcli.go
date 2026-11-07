package install

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/norvexlabs/stowage/internal/spec"
)

// kitVerbs are how a shape runs a handoff's steps, and flags what its commands take beside their own.
type kitVerbs struct {
	create func(cmd *cobra.Command, req KitRequest) (*IssuedKit, error)
	check  func(cmd *cobra.Command, handoff, instance string) (int, error)
	flags  func(c *cobra.Command)
}

// kitCommand is `kit`, for a project that declares a handoff; nil for one that does not.
func kitCommand(r *Release, stdout, stderr io.Writer, terminal func(bool) Prompter, verbs kitVerbs) *cobra.Command {
	if len(r.Spec.Handoffs) == 0 {
		return nil
	}
	root := &cobra.Command{Use: "kit", Short: "Issue a handoff kit, sealed with a code, or check one issued", Args: cobra.NoArgs}
	noAnswer := func(err error) error {
		if err == nil {
			return nil
		}
		fmt.Fprintln(stderr, "error:", err)
		return ExitCode(KitUnknown)
	}
	var instance, expires, out, answers string
	var sets []string
	var noInput, accessible bool
	create := &cobra.Command{
		Use:   "create HANDOFF",
		Short: "Run a handoff's step, and write what it gives to a kit another install takes with --kit",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			h := r.Spec.Handoff(args[0])
			if h == nil {
				return fmt.Errorf("%s declares no handoff %q", r.Spec.Name, args[0])
			}
			after, err := parseKitExpiry(expires)
			if err != nil {
				return err
			}
			var prompt Prompter
			if !noInput {
				prompt = terminal(accessible)
			}
			req := KitRequest{Handoff: h.Name, Instance: instance, Expires: after}
			if req.Answers, err = kitAnswers(cmd.Context(), h, sets, answers, stderr, prompt); err != nil {
				return err
			}
			if req.Instance == "" {
				if req.Instance, err = kitInstance(cmd.Context(), h, prompt); err != nil {
					return err
				}
			}
			if err := checkKitInstance(req.Instance); err != nil {
				return err
			}
			file := expandHome(out)
			if file == "" {
				file = h.Name + "-" + req.Instance + ".kit"
			}
			// Before the run, which may issue what cannot be issued twice: the file is there to write.
			f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return fmt.Errorf("the kit's file: %w (a kit never replaces a file: -o names another)", err)
			}
			done := false
			defer func() {
				_ = f.Close()
				if !done {
					_ = os.Remove(file)
				}
			}()
			issued, err := verbs.create(cmd, req)
			if err != nil {
				return err
			}
			if _, err := f.WriteString(issued.Kit); err != nil {
				return fmt.Errorf("%s: %w; what the handoff issued is lost: the project's script decides whether it can be issued again", file, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("%s: %w; what the handoff issued is lost: the project's script decides whether it can be issued again", file, err)
			}
			done = true
			_, _ = io.WriteString(stderr, issued.Log)
			if _, err := fmt.Fprintf(stdout, "kit   %s\ncode  %s\ngive them apart: `--kit %s` on an install of %s takes it, asking the code, which is shown only here\n", file, issued.Code, file, h.For); err != nil {
				return fmt.Errorf("the code could not be shown, so %s cannot be opened: %w; the project's script decides whether it can be issued again", file, err)
			}
			return nil
		},
	}
	cf := create.Flags()
	cf.StringVar(&instance, "name", "", "the instance this kit is for, which the handoff's run receives (default: asked when the handoff has an instance prompt, else the handoff's name)")
	cf.StringVar(&expires, "expires", "", "how long the kit is good for, as 7d or 12h (default 7d)")
	cf.StringVarP(&out, "output", "o", "", "the file to write (0600, never over a file; default HANDOFF-INSTANCE.kit)")
	cf.StringArrayVar(&sets, "set", nil, "what the handoff asks, KEY=VALUE (repeatable; never a secret answer: --answers gives one)")
	cf.StringVar(&answers, "answers", "", "a YAML file of what the handoff asks, KEY: value")
	cf.BoolVar(&noInput, "no-input", false, "never ask")
	cf.BoolVar(&accessible, "accessible", false, "ask with plain line prompts (screen readers); also when ACCESSIBLE is set or TERM=dumb")
	verbs.flags(create)
	check := &cobra.Command{
		Use:   "check HANDOFF INSTANCE",
		Short: "Run a handoff's check for an instance: exit 0 proven, 2 unknown or no answer, 1 not proven",
		// A check that never ran is no answer, as below: a script reads 1 as not proven.
		Args: func(cmd *cobra.Command, args []string) error {
			return noAnswer(cobra.ExactArgs(2)(cmd, args))
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			code, err := verbs.check(cmd, args[0], args[1])
			// No answer is unknown, never negative: a script must not read it as not proven.
			if err != nil {
				fmt.Fprintln(stderr, "error:", err)
				fmt.Fprintf(stdout, "%s %s: unknown\n", args[0], args[1])
				return ExitCode(KitUnknown)
			}
			if code < 0 {
				fmt.Fprintln(stderr, "error: the check was stopped, or could not start: its Job's log says why")
				code = KitUnknown // it said nothing
			}
			switch code {
			case KitProven:
				fmt.Fprintf(stdout, "%s %s: proven\n", args[0], args[1])
				return nil
			case KitUnknown:
				fmt.Fprintf(stdout, "%s %s: unknown\n", args[0], args[1])
				return ExitCode(KitUnknown)
			default:
				fmt.Fprintf(stdout, "%s %s: not proven (its check, or starting it, exited %d)\n", args[0], args[1], code)
				return ExitCode(1)
			}
		},
	}
	check.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return noAnswer(err) })
	verbs.flags(check)
	root.AddCommand(create, check)
	return root
}

// parseKitExpiry reads --expires: a number of days, as 7d, or what time.ParseDuration reads. Empty is
// the default, 0.
func parseKitExpiry(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	var d time.Duration
	var err error
	if days, ok := strings.CutSuffix(s, "d"); ok {
		var n int
		if n, err = strconv.Atoi(days); err == nil {
			if n > 36500 {
				return 0, fmt.Errorf("--expires %q: at most 36500d", s)
			}
			d = time.Duration(n) * 24 * time.Hour
		}
	} else {
		d, err = time.ParseDuration(s)
	}
	if err != nil {
		return 0, fmt.Errorf("--expires %q: a number of days, as 7d, or a duration, as 12h", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("--expires %q: a kit expires after now", s)
	}
	return d, nil
}

// kitInstance is the instance when --name gave none: asked when the handoff has a prompt for it, else
// the handoff's name.
func kitInstance(ctx context.Context, h *spec.Handoff, prompt Prompter) (string, error) {
	if h.Instance == "" {
		return h.Name, nil
	}
	if prompt == nil {
		return "", fmt.Errorf("the handoff %s asks %q: --name gives it", h.Name, h.Instance)
	}
	const key = "--name"
	a, err := prompt.Ask(ctx, Form{
		Questions: []Question{{Title: h.Instance, Key: key, Required: true}},
		Defaults:  func(map[string]string) map[string]string { return nil },
	})
	if err != nil {
		return "", err
	}
	return a[key], nil
}

// kitAnswers is what the handoff asks: --set, then --answers, then the terminal. A secret answer is
// never taken from a command line.
func kitAnswers(ctx context.Context, h *spec.Handoff, sets []string, answers string, stderr io.Writer, prompt Prompter) (map[string]string, error) {
	o := Options{Log: stderr}
	var err error
	if o.Set, err = parseSets(sets); err != nil {
		return nil, err
	}
	if answers != "" {
		if err := o.readAnswersFile(answers, false); err != nil {
			return nil, err
		}
	}
	got := map[string]string{}
	var ask []Question
	for _, a := range h.Ask {
		v, set := o.Set[a.Key]
		switch {
		case set && a.Secret:
			return nil, fmt.Errorf("%s is a secret answer, which a command line never carries: --answers gives it, or a terminal asks it", a.Key)
		case set:
			got[a.Key] = v
		case o.Answers[a.Key] != "":
			got[a.Key] = o.Answers[a.Key]
		default:
			ask = append(ask, Question{Title: a.Prompt, Key: a.Key, Required: true, Secret: a.Secret, Check: spec.CheckEnvValue})
		}
	}
	for _, m := range []map[string]string{o.Set, o.Answers} {
		for _, key := range sortedKeys(m) {
			if _, ok := got[key]; !ok && !h.Asks(key) {
				return nil, fmt.Errorf("the handoff %s does not ask %s", h.Name, key)
			}
		}
	}
	if len(ask) == 0 {
		return got, nil
	}
	if prompt == nil {
		var keys []string
		for _, q := range ask {
			keys = append(keys, q.Key)
		}
		return nil, fmt.Errorf("the handoff %s asks %s: --set or --answers gives it, or a terminal asks it", h.Name, strings.Join(keys, ", "))
	}
	a, err := prompt.Ask(ctx, Form{Questions: ask, Defaults: func(map[string]string) map[string]string { return nil }})
	if err != nil {
		return nil, err
	}
	for _, q := range ask {
		got[q.Key] = a[q.Key]
	}
	return got, nil
}

// vmKitVerbs run a handoff through the host daemon, on its operators' socket alone.
func vmKitVerbs(c *hostClient, stderr io.Writer) kitVerbs {
	return kitVerbs{
		flags: func(*cobra.Command) {},
		create: func(cmd *cobra.Command, req KitRequest) (*IssuedKit, error) {
			var out IssuedKit
			if err := c.do(cmd.Context(), http.MethodPost, "/v1/kits", req, &out); err != nil {
				return nil, err
			}
			return &out, nil
		},
		check: func(cmd *cobra.Command, handoff, instance string) (int, error) {
			var out KitCheckResult
			if err := c.do(cmd.Context(), http.MethodPost, "/v1/kits/check", KitCheckRequest{Handoff: handoff, Instance: instance}, &out); err != nil {
				return 0, err
			}
			_, _ = io.WriteString(stderr, out.Log)
			return out.Exit, nil
		},
	}
}

// dockerKitVerbs run a handoff in the install at the --dir flag names, as the provision hook runs.
func dockerKitVerbs(in *Installer, prepare func() error, dirFlag func(*cobra.Command)) kitVerbs {
	return kitVerbs{
		flags: dirFlag,
		create: func(cmd *cobra.Command, req KitRequest) (*IssuedKit, error) {
			if err := prepare(); err != nil {
				return nil, err
			}
			return in.IssueKit(cmd.Context(), req, time.Now())
		},
		check: func(cmd *cobra.Command, handoff, instance string) (int, error) {
			if err := prepare(); err != nil {
				return 0, err
			}
			return in.CheckKit(cmd.Context(), handoff, instance)
		},
	}
}

// k8sKitVerbs run a handoff as a Job of the release.
func k8sKitVerbs(in *Installer, o *Options, run func(cmd *cobra.Command, f func(context.Context) error) error) kitVerbs {
	return kitVerbs{
		flags: func(c *cobra.Command) {
			c.Flags().StringVar(&o.RegistryCA, "registry-ca", "", "a PEM file of the CA the registry's certificate chains to, a mirror's own: the installer's reads trust it")
		},
		create: func(cmd *cobra.Command, req KitRequest) (*IssuedKit, error) {
			var issued *IssuedKit
			err := run(cmd, func(ctx context.Context) (err error) {
				issued, err = in.K8sIssueKit(ctx, req, time.Now())
				return err
			})
			return issued, err
		},
		check: func(cmd *cobra.Command, handoff, instance string) (int, error) {
			var code int
			err := run(cmd, func(ctx context.Context) (err error) {
				code, err = in.K8sCheckKit(ctx, handoff, instance)
				return err
			})
			return code, err
		},
	}
}
