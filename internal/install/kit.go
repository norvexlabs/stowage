package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/norvexlabs/stowage/internal/kit"
)

// KitCodeVar is the variable holding a kit's code when no terminal can ask it.
const KitCodeVar = "STOWAGE_KIT_CODE"

// maxKitCodeFile bounds --kit-code-file's read: a code is short.
const maxKitCodeFile = 4096

// givenFlags are --given, --kit and --kit-code-file: what another install gives this run.
type givenFlags struct {
	given, kit, codeFile string
}

// any reports whether one of them was given.
func (f *givenFlags) any() bool { return f.given != "" || f.kit != "" || f.codeFile != "" }

// name is the flag the run gives its material by.
func (f *givenFlags) name() string {
	if f.given != "" {
		return "--given"
	}
	return "--kit"
}

// defineGiven defines --given.
func (f *givenFlags) defineGiven(fs *pflag.FlagSet, what string) {
	fs.Var(pathFlag{&f.given}, "given", what)
}

// defineKit defines --kit and --kit-code-file.
func (f *givenFlags) defineKit(fs *pflag.FlagSet, what string) {
	fs.Var(pathFlag{&f.kit}, "kit", what)
	fs.Var(pathFlag{&f.codeFile}, "kit-code-file", "the file holding --kit's code (default: "+KitCodeVar+", else asked on a terminal)")
}

// pathFlag is a flag naming a file, refusing an empty value: a script's unset variable must not turn
// a run that gives material into one that gives none.
type pathFlag struct{ v *string }

func (p pathFlag) String() string {
	if p.v == nil {
		return ""
	}
	return *p.v
}

func (p pathFlag) Set(s string) error {
	if s == "" {
		return errors.New("empty: it names a file")
	}
	*p.v = s
	return nil
}

func (pathFlag) Type() string { return "string" }

// take reads what the flags name and hands it to use, with the flag that gave it; nothing when no
// flag was given. Standard input is read once, by one flag; prompt asks a kit's code when nothing
// gives it, and may be nil.
func (f *givenFlags) take(ctx context.Context, in *Installer, stdin io.Reader, prompt Prompter, now time.Time, use func(g *Given, flag string) error) error {
	if f.given != "" && (f.kit != "" || f.codeFile != "") {
		return errors.New("--given takes a file in the clear, --kit a sealed one: give one")
	}
	switch {
	case f.given != "":
		raw, err := readStdinFlag("--given", f.given, stdin)
		if err != nil {
			return err
		}
		g, err := parseGiven(raw, in.R.Spec.Name)
		if err != nil {
			return err
		}
		return use(g, "--given")
	case f.kit != "":
		g, err := in.openKit(ctx, f.kit, f.codeFile, stdin, prompt, now)
		if err != nil {
			return err
		}
		return use(g, "--kit")
	case f.codeFile != "":
		return errors.New("--kit-code-file is the code of a kit --kit names, and none is")
	}
	return nil
}

// openKit reads the kit at path, opens it and checks it is this project's and not expired: what it
// holds, as a Given.
func (in *Installer) openKit(ctx context.Context, path, codeFile string, stdin io.Reader, prompt Prompter, now time.Time) (*Given, error) {
	raw, err := readFlagFile("--kit", path, stdin, kit.MaxSize)
	if err != nil {
		return nil, err
	}
	if err := kit.Sniff(raw); err != nil {
		return nil, fmt.Errorf("--kit: %w", err)
	}
	code, err := in.kitCode(ctx, codeFile, prompt)
	if err != nil {
		return nil, err
	}
	k, err := kit.Open(raw, code)
	if err != nil {
		return nil, fmt.Errorf("--kit: %w", err)
	}
	if err := k.Check(in.R.Spec.Name, now); err != nil {
		return nil, fmt.Errorf("--kit: %w", err)
	}
	g := &Given{Project: k.For, Inputs: k.Inputs, TLS: (*GivenTLS)(k.TLS)}
	if err := g.check("--kit"); err != nil {
		return nil, err
	}
	in.O.logf("--kit: %q, instance %q, until %s", k.Name, k.Instance, k.Expires.UTC().Format(time.RFC3339))
	return g, nil
}

// kitCode is a kit's code: --kit-code-file's, else the variable's, else asked on the terminal. The
// variable leaves this process's environment here, so no program the run starts inherits it.
func (in *Installer) kitCode(ctx context.Context, codeFile string, prompt Prompter) (string, error) {
	fromEnv := strings.TrimSpace(in.O.getenv(KitCodeVar))
	if in.O.Getenv == nil {
		if err := os.Unsetenv(KitCodeVar); err != nil {
			return "", err
		}
	}
	if codeFile != "" {
		f, err := os.Open(expandHome(codeFile))
		if err != nil {
			return "", fmt.Errorf("--kit-code-file: %w", err)
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, maxKitCodeFile+1))
		if err != nil {
			return "", fmt.Errorf("--kit-code-file: %w", err)
		}
		if len(raw) > maxKitCodeFile {
			return "", fmt.Errorf("--kit-code-file: more than %d bytes", maxKitCodeFile)
		}
		code := strings.TrimSpace(string(raw))
		if code == "" {
			return "", fmt.Errorf("--kit-code-file %s is empty", codeFile)
		}
		return code, nil
	}
	if fromEnv != "" {
		return fromEnv, nil
	}
	if prompt == nil {
		return "", fmt.Errorf("--kit: nothing gives its code, and nothing can ask it (standard input is no terminal, or --no-input): give --kit-code-file FILE or %s", KitCodeVar)
	}
	const key = "--kit-code"
	a, err := prompt.Ask(ctx, Form{
		Questions: []Question{{Title: "The kit's code", Key: key, Required: true, Secret: true}},
		Defaults:  func(map[string]string) map[string]string { return nil },
	})
	if err != nil {
		return "", err
	}
	return a[key], nil
}

// refusesKit makes c refuse --given, --kit and --kit-code-file, naming the command that takes them.
func refusesKit(c *cobra.Command, fix string) {
	f := c.Flags()
	names := []string{"given", "kit", "kit-code-file"}
	for _, name := range names {
		f.String(name, "", "")
		_ = f.MarkHidden(name)
	}
	c.PreRunE = func(cmd *cobra.Command, _ []string) error {
		for _, name := range names {
			if cmd.Flags().Changed(name) {
				return refuse(fix, "--%s: an upgrade takes none of it: %s", name, fix)
			}
		}
		return nil
	}
}
