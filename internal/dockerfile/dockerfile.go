// Package dockerfile reads the few facts Stowage needs from a Dockerfile: the build arguments it
// declares and the images its stages start FROM. It uses BuildKit's own parser and instruction
// layer, so continuations, the escape directive, comments, quoting, heredocs and the validation of
// each instruction are exactly the build's; an ARG name is expanded by BuildKit's shell lexer, as
// the build expands it.
package dockerfile

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
)

// File is a parsed Dockerfile.
type File struct {
	stages []instructions.Stage
	args   []instructions.ArgCommand
	escape rune
	// Frontend is the `# syntax=` image when the Dockerfile names one other than the standard
	// docker/dockerfile frontend; its grammar is not the built-in one, so Parse did not validate
	// the instructions, and MayDeclareArg answers "declares" (the image is rebuilt on every
	// version, never shipped stale).
	Frontend string
}

// standardFrontend is the frontend whose grammar the built-in parser implements.
const standardFrontend = "docker/dockerfile"

// Parse reads a Dockerfile; an error is one the build would report too.
func Parse(src []byte) (*File, error) {
	if syntax, _, _, ok := parser.DetectSyntax(src); ok {
		name, _, _ := strings.Cut(strings.TrimPrefix(syntax, "docker.io/"), ":")
		name, _, _ = strings.Cut(name, "@")
		if name != standardFrontend {
			return &File{Frontend: syntax}, nil
		}
	}
	res, err := parser.Parse(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("dockerfile: %w", err)
	}
	stages, metaArgs, err := instructions.Parse(res.AST, nil)
	if err != nil {
		return nil, fmt.Errorf("dockerfile: %w", err)
	}
	if len(stages) == 0 {
		return nil, fmt.Errorf("dockerfile: no stage (FROM): the build refuses it")
	}
	f := &File{stages: stages, args: metaArgs, escape: res.EscapeToken}
	for _, st := range stages {
		for _, c := range st.Commands {
			if a, ok := c.(*instructions.ArgCommand); ok {
				f.args = append(f.args, *a)
			}
		}
	}
	return f, nil
}

// noEnv is the empty environment ARG names are expanded against.
type noEnv struct{}

func (noEnv) Get(string) (string, bool) { return "", false }
func (noEnv) Keys() []string            { return nil }

// MayDeclareArg reports whether an ARG instruction declares name. A name is compared after shell
// expansion (`ARG "NAME"` declares NAME). A name built from a variable (`ARG PREFIX_${X}`) may
// resolve to anything at build time, so it counts as declaring name: the answer errs toward
// "declares", which only makes an image rebuild more often, never ship stale.
func (f *File) MayDeclareArg(name string) (bool, error) {
	if f.Frontend != "" {
		return true, nil
	}
	lex := shell.NewLex(f.escape)
	for _, a := range f.args {
		for _, kv := range a.Args {
			if strings.Contains(kv.Key, "$") {
				return true, nil
			}
			key, _, err := lex.ProcessWord(kv.Key, noEnv{})
			if err != nil {
				return false, fmt.Errorf("dockerfile: ARG %s: %w", kv.Key, err)
			}
			if key == name {
				return true, nil
			}
		}
	}
	return false, nil
}

// From is one stage's FROM.
type From struct {
	Image string // as written, possibly holding ${ARG}
	Stage string // the AS name, lower-cased; "" when unnamed
	Line  int
}

// Froms lists the stages' FROM lines in order.
func (f *File) Froms() []From {
	out := make([]From, 0, len(f.stages))
	for _, st := range f.stages {
		fr := From{Image: st.BaseName, Stage: strings.ToLower(st.Name)}
		if len(st.Location) > 0 {
			fr.Line = st.Location[0].Start.Line
		}
		out = append(out, fr)
	}
	return out
}
