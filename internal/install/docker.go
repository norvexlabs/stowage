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
// a signal —, as the container then runs on. A run that ended of itself (exit 2, "no answer",
// included) is left as it is: `--rm` took its container. The removal takes a context of its own,
// bounded (removeWait), as the run's may be what ended; a container gone already is no error, and
// a removal that fails is joined to the run's error with the command that removes it.
func (in *Installer) runNamed(ctx context.Context, name string, c Cmd) ([]byte, error) {
	out, err := in.O.Docker.Run(ctx, c)
	var exit *exec.ExitError
	killed := errors.As(err, &exit) && exit.ProcessState != nil && !exit.ProcessState.Exited()
	if err == nil || ctx.Err() == nil && !killed {
		return out, err
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeWait)
	defer cancel()
	if _, rerr := in.O.Docker.Run(rctx, Cmd{Args: []string{"rm", "--force", name}}); rerr != nil && !strings.Contains(rerr.Error(), "No such container") {
		err = errors.Join(err, fmt.Errorf("the container may still be running: `docker rm --force %s` (%w)", name, rerr))
	}
	return out, err
}

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
