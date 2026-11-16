package install

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/spf13/cobra"
)

// A command that fails returns what it printed with its error: `docker inspect` of containers one
// of which went prints the others'.
func TestExecToolReturnsTheOutputOfAFailure(t *testing.T) {
	t.Parallel()
	out, err := ExecTool{Bin: "sh"}.Run(context.Background(), Cmd{Args: []string{"-c", "echo found; echo gone >&2; exit 1"}})
	if string(out) != "found\n" || err == nil || !strings.Contains(err.Error(), "gone") {
		t.Errorf("%q %v", out, err)
	}
}

// toolFunc is a Tool of a function.
type toolFunc func(context.Context, Cmd) ([]byte, error)

func (f toolFunc) Run(ctx context.Context, c Cmd) ([]byte, error) { return f(ctx, c) }

// A named run cut off (its context ended) has its container removed by name under a context of
// its own; one gone already is no error, a removal that fails is said with its command. A run
// that ended of itself (exit 2 included) is left: --rm took its container.
func TestARunCutOffRemovesItsContainer(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		cut   bool
		rmErr error
		want  string
	}{
		"ended of itself": {false, nil, ""},
		"removed":         {true, nil, ""},
		"gone":            {true, errors.New("docker rm: exit status 1: Error response from daemon: No such container: c1"), ""},
		"kept":            {true, errors.New("docker rm: daemon busy"), "the container may still be running: `docker rm --force c1`"},
	} {
		var ran [][]string
		tool := toolFunc(func(ctx context.Context, cmd Cmd) ([]byte, error) {
			ran = append(ran, cmd.Args)
			if cmd.Args[0] == "rm" {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, c.rmErr
			}
			return nil, exitCode(2)
		})
		in := &Installer{O: Options{Docker: tool}}
		ctx, cancel := context.WithCancel(context.Background())
		if c.cut {
			cancel() // the run's context ended: the removal's is its own
		}
		_, err := in.runNamed(ctx, "c1", Cmd{Args: []string{"run", "--name", "c1", "img"}})
		cancel()
		removed := len(ran) == 2 && strings.Join(ran[1], " ") == "rm --force c1"
		if !errors.Is(err, exitCode(2)) || removed != c.cut || len(ran) > 2 {
			t.Errorf("%s: %v, ran %v", name, err, ran)
		}
		if got := fmt.Sprint(err); (c.want == "") == strings.Contains(got, "may still be running") || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestExecToolInterruptsWithGrace(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	out := &lockedBuf{}
	done := make(chan error)
	go func() {
		_, err := ExecTool{Bin: "sh", Grace: 10 * time.Second}.Run(ctx, Cmd{
			Args: []string{"-c", `trap 'echo state written; exit 3' INT; echo ready; while :; do sleep 0.01; done`}, Stdout: out})
		done <- err
	}()
	deadline := time.After(5 * time.Second)
	for !strings.Contains(out.String(), "ready") {
		select {
		case err := <-done:
			t.Fatalf("ended before it was interrupted: %v %q", err, out.String())
		case <-deadline:
			t.Fatal("never started")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
	if !strings.Contains(out.String(), "state written") {
		t.Errorf("not interrupted: %q", out.String())
	}
}

// lockedBuf is a buffer one goroutine writes while another reads it.
type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
func TestExecToolKillsTheGroupAfterTheGrace(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		// The background sleep ignores the interrupt, as a non-interactive shell's jobs do, and the
		// shell waits for it: only the group's kill ends it.
		_, err := ExecTool{Bin: "sh", Grace: 50 * time.Millisecond}.Run(ctx, Cmd{Args: []string{"-c", `sleep 30 & echo $! >` + pidFile + `; wait`}})
		done <- err
	}()
	deadline := time.After(5 * time.Second)
	for {
		if b, _ := os.ReadFile(pidFile); len(b) > 0 && b[len(b)-1] == '\n' {
			break
		}
		select {
		case <-deadline:
			t.Fatal("never started")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run outlived its grace")
	}
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	for range 500 {
		if p, err := os.FindProcess(pid); err != nil || p.Signal(syscall.Signal(0)) != nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Errorf("the tool's child %d outlived the grace", pid)
}

func TestSSHArgsWithoutTheKeyFile(t *testing.T) {
	t.Parallel()
	a := pinnedSSHArgs("known", "ops", "192.0.2.10", filepath.Join(t.TempDir(), "gone.pub"), "true")
	if slices.Contains(a, "-i") || slices.Contains(a, "IdentitiesOnly=yes") {
		t.Errorf("ssh %q names a key file that does not exist", a)
	}
	if !slices.Equal(a[len(a)-3:], []string{"ops@192.0.2.10", "--", "true"}) {
		t.Errorf("ssh %q", a)
	}
}

// The private key next to the public one is offered alone; with the public key alone, the agent's
// key of it is, through the public file.
func TestSSHArgsOfferTheGivenKeyAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pub := filepath.Join(dir, "ops.pub")
	if err := os.WriteFile(pub, []byte("ssh-ed25519 AAAA ops\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if a := pinnedSSHArgs("known", "ops", "192.0.2.10", pub, "true"); !slices.Contains(a, pub) || !slices.Contains(a, "IdentitiesOnly=yes") {
		t.Errorf("the public key alone: ssh %q", a)
	}
	priv := filepath.Join(dir, "ops")
	if err := os.WriteFile(priv, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if a := pinnedSSHArgs("known", "ops", "192.0.2.10", pub, "true"); !slices.Contains(a, priv) || slices.Contains(a, pub) || !slices.Contains(a, "IdentitiesOnly=yes") {
		t.Errorf("both keys: ssh %q", a)
	}
}

// Every verb that reaches a VM over SSH from this machine takes the VM's host key with
// --known-hosts-line: ssh is reached through link alone, which refuses a run without it, and the
// verbs that take the flag are these, each saying it is required.
func TestEverySSHVerbTakesTheVMsHostKey(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var users []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "SSH" {
					users = append(users, f+":"+fn.Name.Name)
				}
				return true
			})
		}
	}
	if slices.Sort(users); !slices.Equal(slices.Compact(users), []string{"vmkit.go:link"}) {
		t.Errorf("ssh is reached outside link: %v", users)
	}
	e := newEnv(t)
	e.r.Spec.Handoffs = []spec.Handoff{testHandoff()}
	root := newCommand(e.r, e.self, io.Discard, io.Discard, K8sOptions{}, func(bool) Prompter { return nil })
	vm, _, err := root.Find([]string{"vm"})
	if err != nil {
		t.Fatal(err)
	}
	var verbs []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if f := c.Flags().Lookup("known-hosts-line"); f != nil {
			verbs = append(verbs, c.CommandPath())
			if !strings.HasPrefix(f.Usage, "required: ") {
				t.Errorf("%s: --known-hosts-line %q", c.CommandPath(), f.Usage)
			}
		}
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk(vm)
	slices.Sort(verbs)
	if want := []string{"notes-stow vm host-key", "notes-stow vm kit check", "notes-stow vm kit create", "notes-stow vm kit take"}; !slices.Equal(verbs, want) {
		t.Errorf("verbs %v, want %v", verbs, want)
	}
}
