package install

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Down reports whether a service's container is down: exited with a code other than 0, or in a
// state no running container has (restarting, dead, being removed, created and never started). A
// container exited 0 is a one-shot that finished, or a long-running service that stopped of
// itself: Compose tells them apart by nothing, so it is not down. One the operator stopped is
// down too when its process exits non-zero (Why names the signal): Docker records no stop's
// author.
func (s Service) Down() bool {
	switch s.State {
	case "exited":
		return s.ExitCode != 0
	case "restarting", "dead", "created", "removing":
		return true
	}
	return false
}

// Why is a down service's state in words: its exit code, whether its memory cgroup killed it, and
// the memory limit its container ran with.
func (s Service) Why() string {
	var b strings.Builder
	switch s.State {
	case "exited":
		fmt.Fprintf(&b, "exited %d%s", s.ExitCode, signalOf(s.ExitCode))
	case "restarting":
		fmt.Fprintf(&b, "restarting (exited %d%s)", s.ExitCode, signalOf(s.ExitCode))
	case "created":
		b.WriteString("created, never started")
	default:
		b.WriteString(s.State)
	}
	if s.OOMKilled {
		b.WriteString(", OOM-killed")
	}
	if s.MemLimit > 0 {
		fmt.Fprintf(&b, " (memory limit %d MiB)", s.MemLimit>>20)
	}
	return b.String()
}

// signalOf names the signal an exit code of 128+N tells (N from 1 to 64): a stop's SIGTERM (143),
// a timeout's or the kernel's SIGKILL (137), the usual others by name, the rest by number; "" for
// an exit of the process's own.
func signalOf(code int) string {
	n := code - 128
	if n < 1 || n > 64 {
		return ""
	}
	if name, ok := signalNames[n]; ok {
		return " (" + name + ")"
	}
	return fmt.Sprintf(" (signal %d)", n)
}

// signalNames are Linux's numbers of the signals a container is usually stopped or killed with.
var signalNames = map[int]string{1: "SIGHUP", 2: "SIGINT", 3: "SIGQUIT", 4: "SIGILL", 6: "SIGABRT", 7: "SIGBUS", 8: "SIGFPE", 9: "SIGKILL", 10: "SIGUSR1", 11: "SIGSEGV", 12: "SIGUSR2", 13: "SIGPIPE", 14: "SIGALRM", 15: "SIGTERM", 5: "SIGTRAP"}

// downOnes are the containers of list that are down.
func downOnes(list []Service) []Service {
	var down []Service
	for _, s := range list {
		if s.Down() {
			down = append(down, s)
		}
	}
	return down
}

// serviceFacts adds to each service what `docker inspect` tells of its container beyond `compose
// ps`: whether its memory cgroup killed it, its memory limit, and how many times its restart
// policy restarted it. A container inspect does not answer for keeps what compose ps said: the
// facts are a help to the operator, never a condition.
func (in *Installer) serviceFacts(ctx context.Context, list []Service) {
	var ids []string
	for _, s := range list {
		if s.ID != "" {
			ids = append(ids, s.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	out, err := in.O.Docker.Run(ctx, Cmd{Args: append([]string{"inspect", "--type", "container", "--format", "{{.Id}} {{.State.OOMKilled}} {{.HostConfig.Memory}} {{.RestartCount}}"}, ids...)})
	if err != nil && len(out) == 0 {
		return
	}
	type facts struct {
		oom      bool
		mem      int64
		restarts int
	}
	byID := map[string]facts{}
	var whole []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 4 {
			continue
		}
		mem, err1 := strconv.ParseInt(f[2], 10, 64)
		n, err2 := strconv.Atoi(f[3])
		if err1 != nil || err2 != nil {
			continue
		}
		byID[f[0]] = facts{f[1] == "true", mem, n}
		whole = append(whole, f[0])
	}
	for i, s := range list {
		if s.ID == "" {
			continue
		}
		// compose ps may print an ID shortened: inspect's is whole. The exact one first, else the
		// one whole ID it begins.
		f, ok := byID[s.ID]
		if !ok {
			var match []string
			for _, id := range whole {
				if sameContainer(id, s.ID) {
					match = append(match, id)
				}
			}
			if len(match) != 1 {
				continue
			}
			f = byID[match[0]]
		}
		list[i].OOMKilled, list[i].MemLimit, list[i].Restarts = f.oom, f.mem, f.restarts
	}
}

// sameContainer reports whether two IDs name one container: equal, or one the other shortened.
func sameContainer(a, b string) bool {
	return a != "" && b != "" && (strings.HasPrefix(a, b) || strings.HasPrefix(b, a))
}

// stackServices is `compose ps --all` of the install env runs, each service with its facts.
func (in *Installer) stackServices(ctx context.Context, env *EnvFile) ([]Service, error) {
	out, err := in.Compose(ctx, env, nil, "ps", "--all", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("compose ps: %w", err)
	}
	list, err := parseServices(out)
	if err != nil {
		return nil, err
	}
	in.serviceFacts(ctx, list)
	return list, nil
}

// printDown prints a line for each service down (downOnes), and why.
func printDown(w io.Writer, list []Service) {
	for _, s := range downOnes(list) {
		fmt.Fprintf(w, "down: %s %s\n", s.Service, s.Why())
	}
}
