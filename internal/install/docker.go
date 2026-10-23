package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	cmd.WaitDelay = 10 * time.Second
	if d.Grace > 0 {
		defer graceful(cmd, d.Grace)()
		cmd.WaitDelay = d.Grace + 10*time.Second // its group is killed at Grace
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if c.Stream != nil {
		cmd.Stdout, cmd.Stderr = c.Stream, c.Stream
	}
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			return nil, fmt.Errorf("%s %s: %w", filepath.Base(bin), strings.Join(head(c.Args, 2), " "), err)
		}
		return nil, fmt.Errorf("%s %s: %w: %s", filepath.Base(bin), strings.Join(head(c.Args, 2), " "), err, msg)
	}
	return out.Bytes(), nil
}

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
