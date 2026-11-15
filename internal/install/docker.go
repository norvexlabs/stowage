package install

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Cmd is one run of a CLI: docker, helm or kubectl.
type Cmd struct {
	Args  []string
	Env   []string // nil: the installer's own environment
	Stdin io.Reader
	// Stream, when set, receives the command's output as it runs (pulls, `up --wait`); otherwise
	// stdout is returned.
	Stream io.Writer
	// Stdout, when set, receives stdout as it runs, apart from Stream; a write error stops the run.
	Stdout io.Writer
	// Cancel, when set, runs when the run's context ends, before the CLI is killed: its stdin is
	// still open then, and the CLI is killed once Cancel returns.
	Cancel func()
}

// Tool runs a CLI.
type Tool interface {
	Run(ctx context.Context, c Cmd) ([]byte, error)
}

// ExecTool runs the binary Bin, a path or a name looked up on PATH.
type ExecTool struct {
	Bin string
	// Grace, when set, runs the tool in a session of its own and stops a cancelled run with one
	// interrupt, killing it and what it started once Grace has passed (graceful): a tool that
	// records state as it goes (OpenTofu) writes it before it exits.
	Grace time.Duration
}

// Run implements Tool.
func (d ExecTool) Run(ctx context.Context, c Cmd) ([]byte, error) {
	bin := d.Bin
	cmd := exec.CommandContext(ctx, bin, c.Args...)
	cmd.Env, cmd.Stdin = c.Env, c.Stdin
	// The pipes are closed pipeWait after the cancel (Cancel's own time aside: exec times the
	// delay from its return).
	cmd.WaitDelay = pipeWait
	if d.Grace > 0 {
		defer graceful(cmd, d.Grace)()
		cmd.WaitDelay = d.Grace + pipeWait // its group is killed at Grace
	}
	if c.Cancel != nil {
		// c's Cancel first, its stdin still open, then the tool's own: graceful's interrupt of
		// its group and timed kill, else the kill.
		then := cmd.Cancel
		if then == nil {
			then = func() error { return cmd.Process.Kill() }
		}
		cmd.Cancel = func() error {
			c.Cancel()
			return then()
		}
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if c.Stream != nil {
		cmd.Stdout, cmd.Stderr = c.Stream, c.Stream
	}
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	}
	// A failing command's output is returned with its error: a command over several objects
	// (`docker inspect` of containers one of which went) prints what it found.
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			return out.Bytes(), fmt.Errorf("%s %s: %w", filepath.Base(bin), strings.Join(head(c.Args, 2), " "), err)
		}
		return out.Bytes(), fmt.Errorf("%s %s: %w: %s", filepath.Base(bin), strings.Join(head(c.Args, 2), " "), err, msg)
	}
	return out.Bytes(), nil
}

// runNamed runs c, a `docker run` or `compose run` of a container named name, and removes that
// container by its name when the run was cut — its context ended, or the docker CLI was killed by
// a signal —, as the container then runs on (runNamedGrace without a grace).
func (in *Installer) runNamed(ctx context.Context, name string, c Cmd) ([]byte, error) {
	return in.runNamedGrace(ctx, name, c, 0)
}

// runNamedGrace is runNamed, the container cut off stopped first when grace is not zero: SIGTERM,
// then grace to end before Docker kills it (`docker stop --time`), sent while the run's stdin is
// still open — when its context ends, before the CLI is killed (Cmd.Cancel), or when its stdin
// fails, before the CLI sees its end — so a program reading it (rclone's upload) is stopped rather
// than reading an end of input and completing what it was sent. The stop runs once, within grace
// and removeWait; then the container is removed. A run that ended of itself (exit 2, "no answer",
// included) is left as it is: `--rm` took its container. The stop and the removal take contexts
// of their own, bounded, as the run's may be what ended; a container gone already, or whose
// removal is under way (`--rm`'s, after the stop), is no error; a stop that fails is joined to the
// run's error and the removal still runs, and a removal that fails is joined with the command that
// removes it. A run cut off returns within runnerCutWait(grace).
func (in *Installer) runNamedGrace(ctx context.Context, name string, c Cmd, grace time.Duration) ([]byte, error) {
	var stopErr error
	var once sync.Once
	var stopped atomic.Bool // a stop was attempted: its stdin's failure, or the cut
	stop := func() {
		once.Do(func() {
			stopped.Store(true)
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace+removeWait)
			defer cancel()
			if _, err := in.O.Docker.Run(sctx, Cmd{Args: []string{"stop", "--time", stopSeconds(grace), name}}); !containerGone(err) {
				stopErr = fmt.Errorf("the container %s did not stop: %w", name, err)
			}
		})
	}
	if grace > 0 {
		c.Cancel = stop
		if c.Stdin != nil {
			c.Stdin = &stopOnFail{r: c.Stdin, stop: stop}
		}
	}
	out, err := in.O.Docker.Run(ctx, c)
	var exit *exec.ExitError
	killed := errors.As(err, &exit) && exit.ProcessState != nil && !exit.ProcessState.Exited()
	if (err == nil || ctx.Err() == nil && !killed) && !stopped.Load() {
		return out, err
	}
	if grace > 0 {
		stop()
		err = errors.Join(err, stopErr)
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeWait)
	defer cancel()
	if _, rerr := in.O.Docker.Run(rctx, Cmd{Args: []string{"rm", "--force", name}}); !containerGone(rerr) {
		err = errors.Join(err, fmt.Errorf("the container may still be running: `docker rm --force %s` (%w)", name, rerr))
	}
	return out, err
}

// stopSeconds is grace in `docker stop --time`'s whole seconds, rounded up: at least 1.
func stopSeconds(grace time.Duration) string {
	return strconv.Itoa(int(max((grace+time.Second-1)/time.Second, 1)))
}

// containerGone reports whether a stop's or a removal's error leaves the container gone: none, no
// such container, or its removal under way (`--rm`'s, as Docker answers 409).
func containerGone(err error) bool {
	return err == nil || strings.Contains(err.Error(), "No such container") || strings.Contains(err.Error(), "is already in progress")
}

// stopOnFail is a run's stdin that stops the run (stop) before it reports a failure of its own
// reader: the run sees no end of input before its stop.
type stopOnFail struct {
	r    io.Reader
	stop func()
}

func (s *stopOnFail) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		s.stop()
	}
	return n, err
}

// runnerCutWait bounds what a run cut off takes, past its cut, in runNamedGrace: its stop when it
// has a grace (grace and removeWait, then that run's own pipes, pipeWait), its pipes' close
// (pipeWait), then its removal (removeWait, then that run's pipes).
func runnerCutWait(grace time.Duration) time.Duration {
	wait := pipeWait + removeWait + pipeWait
	if grace > 0 {
		wait += grace + removeWait + pipeWait
	}
	return wait
}

// pipeWait is how long a cancelled run's pipes stay open once it is killed (exec's WaitDelay).
const pipeWait = 10 * time.Second

// removeWait bounds the removal of a named container whose run failed (runNamed).
const removeWait = 30 * time.Second

func head(s []string, n int) []string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

// MinCompose is the oldest Docker Compose the installer accepts: `up --wait` and the
// `include`/profile behaviour it relies on.
var MinCompose = [3]int{2, 20, 0}

// MinDockerIPv6 is the oldest Docker engine --ipv6 accepts: a network's IPv6 subnet allocated
// without one given (a unique local /64), and ip6tables on by default.
var MinDockerIPv6 = [3]int{27, 0, 0}

// parseVersion reads "2.29.1", "v2.29.1" or "2.29.1-desktop.1".
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	s, _, _ = strings.Cut(s, "-")
	s, _, _ = strings.Cut(s, "+")
	parts := strings.Split(s, ".")
	var v [3]int
	if len(parts) < 2 {
		return v, false
	}
	for i := 0; i < 3 && i < len(parts); i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

func older(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// hostAddrs lists this host's own addresses.
func hostAddrs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !(n.IP.To4() == nil && n.IP.IsLinkLocalUnicast()) { // IPv6 link-local needs a zone
			out = append(out, n.IP.String())
		}
	}
	return out
}

// portHolders are the local addresses holding protocol's port (tcp: listening; udp: bound) in the
// kernel's socket tables under dir (/proc/net's tcp, tcp6, udp, udp6), sorted, each once; none
// when they cannot be read.
func portHolders(dir, protocol string, port int) []string {
	var out []string
	for _, file := range []string{protocol, protocol + "6"} {
		raw, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			continue
		}
		lines := strings.Split(string(raw), "\n")
		for _, line := range lines[min(1, len(lines)):] {
			f := strings.Fields(line)
			if len(f) < 4 || (protocol == "tcp" && f[3] != "0A") {
				continue
			}
			hexAddr, hexPort, ok := strings.Cut(f[1], ":")
			if p, err := strconv.ParseUint(hexPort, 16, 16); !ok || err != nil || int(p) != port {
				continue
			}
			if a, ok := procAddr(hexAddr); ok {
				out = append(out, a.String())
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// procAddr decodes /proc/net's address: 8 or 32 hex digits, each the value of a 32-bit word of
// the address as the host's byte order reads it.
func procAddr(h string) (netip.Addr, bool) {
	b, err := hex.DecodeString(h)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return netip.Addr{}, false
	}
	for i := 0; i < len(b); i += 4 {
		binary.NativeEndian.PutUint32(b[i:], binary.BigEndian.Uint32(b[i:]))
	}
	a, _ := netip.AddrFromSlice(b)
	return a.Unmap(), true
}

// udpPortFree reports whether no socket of the host holds the UDP port on host: a bind to it
// fails only then. A port this process may not bind (below the unprivileged ports, as a user)
// is not known to be held, and counts as free.
func udpPortFree(host string, port int) bool {
	c, err := net.ListenPacket("udp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return !errors.Is(err, syscall.EADDRINUSE)
	}
	c.Close()
	return true
}

// portFree reports whether nothing listens on host:port.
func portFree(host string, port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 500*time.Millisecond)
	if err != nil {
		return true
	}
	c.Close()
	return false
}
