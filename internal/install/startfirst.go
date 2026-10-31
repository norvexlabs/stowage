package install

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/norvexlabs/stowage/internal/spec"
)

// hashLabel is the label Compose gives a container: the digest of its service's configuration,
// which a start compares to recreate the container when it changed.
const hashLabel = "com.docker.compose.config-hash"

// NetworksFile records, in the install's directory, the digest of the stack's networks as the
// last start that came up ran them: a start that changes them replaces its start-first services
// stop-first, as Compose makes a changed network again under every container on it.
const NetworksFile = ".stowage-networks"

// UpdateVolume is the volume a start-first service's update_socket directory mounts: one per
// service, both containers of an overlap mount it.
func UpdateVolume(service string) string { return "stowage-update-" + service }

// startFirst replaces each start-first service (services.<name>.update) this start would
// recreate before the stack's start does: its new container starts beside the old one, which
// stops once the new is healthy. One the start changes is replaced by the stack's start,
// stop-first, when it runs with a fixed name or address, a port published from a network of its
// own (an L4 route's on Docker), in another's network namespace, with another service in its
// namespaces or mounting its volumes (joiner), or a volume that awaits its uid (fresh: the start
// gives it its uid once the containers are made), and when the stack's networks differ from those
// of the last start that came up (prevNetworks, which takeNetworks read).
func (in *Installer) startFirst(ctx context.Context, environ []string, m *composeModel, fresh []string) error {
	var names []string
	for _, name := range sortedKeys(in.R.Spec.Services) {
		if _, ok := m.Services[name]; ok && in.R.Spec.Services[name].Update == spec.UpdateStartFirst {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		in.networks = "" // not recorded: a later start finds none
		return nil
	}
	raw, err := in.O.Docker.Run(ctx, Cmd{Args: append(in.composeBase(), "config", "--format", "json"), Env: environ})
	if err != nil {
		return fmt.Errorf("reading the stack's configuration: %w", err)
	}
	var cfg struct {
		Name     string                `json:"name"`
		Services map[string]runService `json:"services"`
		Networks json.RawMessage       `json:"networks"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("reading the stack's configuration: %w", err)
	}
	// Over a canonical encoding (keys sorted), so Compose's layout of its output is no change.
	var nets any
	if len(cfg.Networks) > 0 {
		if err := json.Unmarshal(cfg.Networks, &nets); err != nil {
			return fmt.Errorf("reading the stack's networks: %w", err)
		}
	}
	canon, err := json.Marshal(nets)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(canon)
	in.networks = hex.EncodeToString(sum[:])
	networks := ""
	switch {
	case in.prevNetworksErr != nil:
		networks = fmt.Sprintf("the record of the stack's networks cannot be read (%v)", in.prevNetworksErr)
	case in.prevNetworks == "":
		networks = "no start that came up recorded the stack's networks"
	case in.prevNetworks != in.networks:
		networks = "this start changes the stack's networks"
	}
	for _, name := range dependencyOrder(m, names) {
		svc := cfg.Services[name]
		why, join := "", joiner(cfg.Services, name, cfg.Name)
		switch {
		case networks != "":
			why = networks
		case svc.ContainerName != "":
			why = "its container has a fixed name"
		case svc.MacAddress != "" || fixedNet(svc.Networks):
			why = "its container has a fixed address"
		case strings.HasPrefix(svc.NetworkMode, "service:") || strings.HasPrefix(svc.NetworkMode, "container:"):
			why = "it runs in another's network namespace"
		case join != "":
			why = join + " joins its namespaces, mounts its volumes or links to it"
		case svc.NetworkMode != "host" && len(svc.Ports) > 0:
			why = "its ports are published from its own network"
		default:
			for _, v := range m.Services[name].Volumes {
				if v.Type == "volume" && slices.Contains(fresh, m.Volumes[v.Source].Name) {
					why = "a volume it mounts awaits its uid"
				}
			}
		}
		if err := in.overlap(ctx, environ, m, name, why); err != nil {
			return err
		}
	}
	return nil
}

// runService is what startFirst reads of a service as the start runs it.
type runService struct {
	ContainerName string                   `json:"container_name"`
	MacAddress    string                   `json:"mac_address"`
	NetworkMode   string                   `json:"network_mode"`
	Pid           string                   `json:"pid"`
	Ipc           string                   `json:"ipc"`
	VolumesFrom   []string                 `json:"volumes_from"`
	Links         []string                 `json:"links"`
	ExternalLinks []string                 `json:"external_links"`
	Ports         []any                    `json:"ports"`
	Networks      map[string]*fixedAddress `json:"networks"`
}

// joiner is a service that joins service's namespaces (network_mode, pid or ipc
// service:<service>, or container: its container's name in project, <project>-<service>-<n> or
// its compatibility name with _) or
// mounts its volumes (volumes_from: <service>, service:<service> or its container, with :ro or :rw)
// or links to it (links: <service>[:alias], or external_links: its container[:alias]), "" when
// none: the overlap would remove them with its old container. A container of the engine
// that joins it is joinedContainer's to tell.
func joiner(services map[string]runService, service, project string) string {
	own := func(v string) bool {
		if v == "service:"+service {
			return true
		}
		if project == "" {
			return false
		}
		for _, sep := range []string{"-", "_"} { // Compose's name, and its compatibility one
			if n, ok := strings.CutPrefix(v, "container:"+project+sep+service+sep); ok && n != "" && strings.Trim(n, "0123456789") == "" {
				return true
			}
		}
		return false
	}
	for _, n := range sortedKeys(services) {
		if n == service {
			continue
		}
		o := services[n]
		if own(o.NetworkMode) || own(o.Pid) || own(o.Ipc) {
			return n
		}
		for _, v := range o.VolumesFrom {
			if v = strings.TrimSuffix(strings.TrimSuffix(v, ":ro"), ":rw"); v == service || own(v) {
				return n
			}
		}
		for _, v := range o.Links {
			if v == service || strings.HasPrefix(v, service+":") {
				return n
			}
		}
		for _, v := range o.ExternalLinks {
			if target, _, _ := strings.Cut(strings.TrimPrefix(v, "/"), ":"); own("container:" + target) {
				return n
			}
		}
	}
	return ""
}

// joinedContainer is a container of the engine (running or not) that joins old's namespaces,
// mounts its volumes or links to it, as the engine records them (container:<ID>, its name, or a
// prefix of its ID no container is named), "" when none: the overlap would remove them with old. A
// container gone between the listing and its reading is none; any other that does not read fails
// the start.
func (in *Installer) joinedContainer(ctx context.Context, old container) (string, error) {
	out, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"ps", "--all", "--quiet", "--no-trunc"}})
	if err != nil {
		return "", fmt.Errorf("listing the engine's containers: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return "", nil
	}
	const format = "{{.Id}}|{{.Name}}|{{.HostConfig.NetworkMode}}|{{.HostConfig.PidMode}}|{{.HostConfig.IpcMode}}|{{range .HostConfig.VolumesFrom}}{{.}},{{end}}|{{range .HostConfig.Links}}{{.}},{{end}}{{range .NetworkSettings.Networks}}{{range .Links}}{{.}},{{end}}{{end}}"
	inspect := func(ids ...string) ([]byte, error) {
		return in.O.Docker.Run(ctx, Cmd{Args: append([]string{"inspect", "--type", "container", "--format", format}, ids...)})
	}
	if out, err = inspect(ids...); err != nil {
		// One at a time: a container removed meanwhile is none, any other error the start's.
		out = nil
		for _, id := range ids {
			one, err := inspect(id)
			if err != nil {
				if gone.MatchString(err.Error()) {
					continue
				}
				return "", fmt.Errorf("reading the engine's container %s: %w", id, err)
			}
			out = append(out, one...)
		}
	}
	var rows [][]string
	names := map[string]bool{}
	for line := range strings.Lines(string(out)) {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) != 7 {
			return "", fmt.Errorf("reading the engine's containers: %q", line)
		}
		rows = append(rows, f)
		names[strings.TrimPrefix(f[1], "/")] = true
	}
	name := strings.TrimPrefix(old.Name, "/")
	// As Docker resolves a reference: its ID, then a name, then a prefix of an ID.
	of := func(v string) bool {
		v = strings.TrimPrefix(strings.TrimSuffix(strings.TrimSuffix(v, ":ro"), ":rw"), "container:")
		return v != "" && (v == old.ID || v == name || !names[v] && strings.HasPrefix(old.ID, v))
	}
	for _, f := range rows {
		if f[0] == old.ID {
			continue
		}
		if slices.ContainsFunc(f[2:5], func(v string) bool { return strings.HasPrefix(v, "container:") && of(v) }) ||
			slices.ContainsFunc(list(f[5]), of) ||
			slices.ContainsFunc(list(f[6]), func(l string) bool { target, _, _ := strings.Cut(l, ":"); return of(strings.TrimPrefix(target, "/")) }) {
			return strings.TrimPrefix(f[1], "/"), nil
		}
	}
	return "", nil
}

// fixedNet reports whether a service fixes its address on any of its networks.
func fixedNet(nets map[string]*fixedAddress) bool {
	for _, a := range nets {
		if a.fixed() {
			return true
		}
	}
	return false
}

// fixedAddress is what a service fixes of its container on a network, which two containers cannot
// hold at once.
type fixedAddress struct {
	IPv4 string `json:"ipv4_address"`
	IPv6 string `json:"ipv6_address"`
	MAC  string `json:"mac_address"`
}

func (a *fixedAddress) fixed() bool { return a != nil && (a.IPv4 != "" || a.IPv6 != "" || a.MAC != "") }

// gone is the engine's answer on a container it no longer holds.
var gone = regexp.MustCompile(`(?i)no such (object|container)`)

// list is a list an inspect template renders as {{range}}{{.}},{{end}}.
func list(v string) []string { return strings.FieldsFunc(v, func(r rune) bool { return r == ',' }) }

// anonymous is the name the engine gives a volume no one named: a container's own, which only a
// recreate hands on to its next container.
var anonymous = regexp.MustCompile(`^[0-9a-f]{64}$`)

// container is what an overlap reads of a service's container.
type container struct {
	ID, Name, State, Exit, Health, Hash, Image string
	Volumes                                    []string
}

// serving reports whether c serves as its service runs it: running, healthy when it has a
// healthcheck; a one-shot (done: its dependents wait for it to complete) once it exited 0.
func (c container) serving(done bool) bool {
	if done {
		return c.State == "exited" && c.Exit == "0"
	}
	return c.State == "running" && (c.Health == "" || c.Health == "healthy")
}

// containerFormat renders a container as containers reads it. A container without a healthcheck
// has no State.Health: read through index, which renders nothing for a key the inspected object
// lacks, where a field reference fails the template.
const containerFormat = "{{.Id}}|{{.Name}}|{{.State.Status}}|{{.State.ExitCode}}|{{with index .State \"Health\"}}{{.Status}}{{end}}|{{index .Config.Labels \"" + hashLabel + "\"}}|{{.Image}}|{{range .Mounts}}{{if eq .Type \"volume\"}}{{.Name}},{{end}}{{end}}"

// containers reads service's containers (`ps --all`) and what each runs.
func (in *Installer) containers(ctx context.Context, environ []string, service string) ([]container, error) {
	out, err := in.O.Docker.Run(ctx, Cmd{Args: append(in.composeBase(), "ps", "--all", "--quiet", service), Env: environ})
	if err != nil {
		return nil, fmt.Errorf("listing its containers: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil, nil
	}
	return in.inspectContainers(ctx, ids...)
}

// inspectContainers reads the containers ids (containerFormat).
func (in *Installer) inspectContainers(ctx context.Context, ids ...string) ([]container, error) {
	out, err := in.O.Docker.Run(ctx, Cmd{Args: append([]string{"inspect", "--type", "container", "--format", containerFormat}, ids...)})
	if err != nil {
		return nil, fmt.Errorf("reading its containers: %w", err)
	}
	var cs []container
	for line := range strings.Lines(string(out)) {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) != 8 {
			return nil, fmt.Errorf("reading its containers: %q", line)
		}
		cs = append(cs, container{ID: f[0], Name: f[1], State: f[2], Exit: f[3], Health: f[4], Hash: f[5], Image: f[6], Volumes: list(f[7])})
	}
	return cs, nil
}

// unchanged reports whether each of cs runs service as this start runs it: its configuration's
// digest and its image. None is changed: the start makes them.
func (in *Installer) unchanged(ctx context.Context, environ []string, m *composeModel, service string, cs []container) (bool, error) {
	if len(cs) == 0 {
		return false, nil
	}
	out, err := in.O.Docker.Run(ctx, Cmd{Args: append(in.composeBase(), "config", "--hash", service), Env: environ})
	if err != nil {
		return false, fmt.Errorf("its configuration's digest: %w", err)
	}
	_, want, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	if out, err = in.O.Docker.Run(ctx, Cmd{Args: []string{"image", "inspect", "--format", "{{.Id}}", m.Services[service].Image}}); err != nil {
		return false, fmt.Errorf("its image %s: %w", m.Services[service].Image, err)
	}
	image := strings.TrimSpace(string(out))
	for _, c := range cs {
		if want == "" || c.Hash != want || c.Image != image {
			return false, nil
		}
	}
	return true, nil
}

// dependencies are the services service depends on, theirs included, each true when every service
// of them that depends on it waits for it to complete (condition: service_completed_successfully):
// a one-shot, done once it exited 0.
func dependencies(m *composeModel, service string) map[string]bool {
	deps := map[string]bool{}
	var walk func(string)
	walk = func(s string) {
		for d, on := range m.Services[s].DependsOn {
			if _, ok := m.Services[d]; !ok || d == service {
				continue
			}
			cond, _ := on.(map[string]any)
			done := cond["condition"] == "service_completed_successfully"
			if prev, was := deps[d]; was {
				deps[d] = prev && done
				continue
			}
			deps[d] = done
			walk(d)
		}
	}
	walk(service)
	return deps
}

// dependencyOrder is names with each after the ones it depends on (dependencies), else in name
// order: a start-first service is overlapped once the start-first services it depends on are.
func dependencyOrder(m *composeModel, names []string) []string {
	var out []string
	done := map[string]bool{}
	var visit func(string)
	visit = func(n string) {
		if done[n] {
			return
		}
		done[n] = true
		for _, d := range sortedKeys(dependencies(m, n)) {
			if slices.Contains(names, d) {
				visit(d)
			}
		}
		out = append(out, n)
	}
	for _, n := range names {
		visit(n)
	}
	return out
}

// overlap replaces service's one container when it differs from what this start runs (its
// configuration's digest, or its image): the new container is started beside it (`up --wait
// --no-deps`: running, healthy when it has a healthcheck), then the old one stops, its
// stop_grace_period the drain, and is removed. A new container that does not become healthy is
// removed and the old one serves on: the start fails. The stack's start replaces it stop-first
// when the overlap cannot keep it serving or would start it against what this start changes:
// several containers, one not running or not healthy, an anonymous volume (which the new container
// would not get), a dependency the start makes, changes or finds not serving, or why startFirst
// gives.
func (in *Installer) overlap(ctx context.Context, environ []string, m *composeModel, service, why string) error {
	stopFirst := func(why string) error {
		in.O.logf("services.%s.update: %s, so this start replaces it stop-first", service, why)
		return nil
	}
	cs, err := in.containers(ctx, environ, service)
	if err != nil {
		return fmt.Errorf("services.%s: %w", service, err)
	}
	if len(cs) == 0 {
		return nil // none: the stack's start makes it
	}
	same, err := in.unchanged(ctx, environ, m, service, cs)
	if err != nil {
		return fmt.Errorf("services.%s: %w", service, err)
	}
	switch {
	case same:
		return nil // the stack's start leaves it as it is
	case why != "":
		return stopFirst(why)
	case len(cs) > 1:
		return stopFirst(fmt.Sprintf("it has %d containers", len(cs)))
	case cs[0].State != "running":
		return stopFirst("its container is not running")
	case !cs[0].serving(false):
		return stopFirst("its container is not healthy")
	}
	for _, v := range cs[0].Volumes {
		if anonymous.MatchString(v) {
			return stopFirst("it has an anonymous volume, which only a recreate keeps")
		}
	}
	deps := dependencies(m, service)
	for _, d := range sortedKeys(deps) {
		dcs, err := in.containers(ctx, environ, d)
		if err != nil {
			return fmt.Errorf("services.%s: %w", d, err)
		}
		ok, err := in.unchanged(ctx, environ, m, d, dcs)
		if err != nil {
			return fmt.Errorf("services.%s: %w", d, err)
		}
		for _, c := range dcs {
			ok = ok && c.serving(deps[d])
		}
		if !ok {
			return stopFirst("this start makes or changes its dependency " + d + ", or finds it not serving")
		}
	}
	if j, err := in.joinedContainer(ctx, cs[0]); err != nil {
		return fmt.Errorf("services.%s: %w", service, err)
	} else if j != "" {
		return stopFirst("the container " + j + " joins its container's namespaces, mounts its volumes or links to it")
	}
	old := cs[0].ID
	if signalled, err := in.startBeside(ctx, environ, m.Name, service, old); err != nil {
		var before beforeUp
		switch {
		case errors.Is(err, errNoHandoff):
			return stopFirst("its old container does not mount the update_socket volume (no socket to hand over)")
		case errors.As(err, &before):
			// Nothing changed yet: the old container serves on, the start fails (a cancelled one
			// as cancelled).
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("services.%s: before its new container starts, which it does not: %w", service, before.err)
		}
		declined := errors.Is(err, errDeclined)
		serves := "which serves on"
		if signalled {
			serves = "which was sent " + in.R.Spec.Services[service].UpdateSignal + " (update_signal) and serves on as far as the app keeps serving after it"
		}
		failed := fmt.Errorf("services.%s: its new container did not become healthy beside the old one, %s: %w", service, serves, err)
		if declined {
			failed = fmt.Errorf("services.%s: its new container declined the overlap (exit status %d, update_decline_status), and the old one serves on", service, *in.R.Spec.Services[service].UpdateDeclineStatus)
		}
		now, lerr := in.containers(ctx, environ, service)
		if lerr != nil {
			return fmt.Errorf("%w (removing the new one: %v)", failed, lerr)
		}
		for _, c := range now {
			if c.ID != old {
				if _, rerr := in.O.Docker.Run(ctx, Cmd{Args: []string{"rm", "--force", c.ID}}); rerr != nil {
					if declined {
						return fmt.Errorf("%w: removing the new container %s: %v", failed, c.ID, rerr)
					}
					in.O.logf("warning: services.%s: the failed new container %s stays: %v", service, c.ID, rerr)
				}
			}
		}
		if declined {
			return stopFirst(fmt.Sprintf("its new container declined the overlap (exit status %d, update_decline_status)", *in.R.Spec.Services[service].UpdateDeclineStatus))
		}
		return failed
	}
	if _, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"stop", old}}); err != nil {
		return fmt.Errorf("services.%s: stopping its old container: %w", service, err)
	}
	if _, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"rm", old}}); err != nil {
		return fmt.Errorf("services.%s: removing its old container: %w", service, err)
	}
	in.O.logf("services.%s: the new container is healthy, the old one stopped", service)
	return nil
}

// overlapPoll is how often startBeside reads the new container's health.
var overlapPoll = 500 * time.Millisecond

// readyWait is how long startBeside waits for the new container to write its update_ready file.
var readyWait = 10 * time.Minute

// startBeside starts service's new container beside its old one and returns once the new one
// serves: running, healthy when it has a healthcheck. With update_signal, the new container is
// started without waiting, the old one sent the signal once the new one runs (signalled) and, with
// update_ready, once it wrote that file in update_socket: one that differs from what the directory
// held before it started, read through the old container (within readyWait, else an error, the old
// one never signalled), and the new one's health read until it is healthy, or unhealthy or no
// longer running (an error). A new container that ends with update_decline_status before the
// signal is errDeclined. Before the new container is made: with update_socket and update_signal,
// an old container that does not mount the update volume is errNoHandoff, and a read of where it
// does, or of its ready file, that fails is a beforeUp: the caller replaces the service stop-first
// on the one, and fails the start on the other, nothing to remove either way.
func (in *Installer) startBeside(ctx context.Context, environ []string, project, service, old string) (signalled bool, err error) {
	svc := in.R.Spec.Services[service]
	signal := svc.UpdateSignal
	up := append(in.composeBase(), "up", "--detach")
	if signal == "" {
		up = append(up, "--wait")
	}
	up = append(up, "--no-deps", "--no-recreate", "--scale", service+"=2", service)
	// The ready file as the shared volume holds it before the new container starts (an earlier
	// overlap's, or none), read where the old container mounts the volume (an earlier release's
	// update_socket may be another directory): the new container's is one that differs.
	file := path.Join(svc.UpdateSocket, svc.UpdateReady)
	var before readyStamp
	var had bool
	if svc.UpdateSocket != "" && signal != "" {
		// The signal asks the old container to hand its sockets over in the volume: one that does
		// not mount it has none to hand.
		dir, mounted, err := in.updateMount(ctx, old, project+"_"+UpdateVolume(service))
		if err != nil {
			return false, beforeUp{fmt.Errorf("reading where its old container mounts its update_socket volume: %w", err)}
		}
		if !mounted {
			return false, errNoHandoff
		}
		if svc.UpdateReady != "" {
			at := path.Join(dir, svc.UpdateReady)
			if before, had, err = in.readyFile(ctx, old, at); err != nil {
				return false, beforeUp{fmt.Errorf("reading %s (update_ready) through its old container: %w", at, err)}
			}
		}
	}
	in.O.logf("services.%s: start-first: the new container starts beside the old one", service)
	if _, err := in.O.Docker.Run(ctx, Cmd{Args: up, Env: environ, Stream: in.O.Log}); err != nil || signal == "" {
		if err != nil && in.declinedBeside(ctx, environ, service, old) {
			return false, errDeclined
		}
		return false, err
	}
	cs, err := in.containers(ctx, environ, service)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(cs, func(c container) bool { return c.ID != old })
	if i < 0 {
		return false, errors.New("no new container beside the old one")
	}
	c := cs[i]
	ready := svc.UpdateReady == ""
	var unread error
	deadline := time.Now().Add(readyWait)
	for {
		switch {
		case c.State != "running" && c.State != "created":
			if !signalled && declines(svc, c) {
				return false, errDeclined
			}
			return signalled, fmt.Errorf("its new container %s is %s (exit code %s)", c.Name, c.State, c.Exit)
		case c.Health == "unhealthy":
			return signalled, fmt.Errorf("its new container %s is unhealthy", c.Name)
		case c.State == "running" && !signalled:
			if !ready {
				now, there, err := in.readyFile(ctx, c.ID, file)
				unread, ready = err, there && (!had || now != before)
				if !ready {
					if time.Now().After(deadline) {
						msg := fmt.Sprintf("its new container %s did not write %s (update_ready) within %s: the directory must be writable by the container's user (a volume made before keeps its owner)", c.Name, file, readyWait)
						if unread != nil {
							msg += fmt.Sprintf(" (%v)", unread)
						}
						return false, errors.New(msg)
					}
					break // read again after a poll
				}
				in.O.logf("services.%s: the new container wrote %s (update_ready)", service, file)
			}
			if _, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"kill", "--signal", signal, old}}); err != nil {
				return false, fmt.Errorf("sending %s to its old container: %w", signal, err)
			}
			in.O.logf("services.%s: the new container runs: %s sent to the old one (update_signal)", service, signal)
			signalled = true
			continue // judged again as read: one without a healthcheck serves at once
		case c.serving(false):
			return signalled, nil
		}
		select {
		case <-ctx.Done():
			return signalled, ctx.Err()
		case <-time.After(overlapPoll):
		}
		now, err := in.inspectContainers(ctx, c.ID)
		if err != nil || len(now) != 1 {
			return signalled, cmp.Or(err, fmt.Errorf("its new container %s is gone", c.Name))
		}
		c = now[0]
	}
}

// errDeclined is startBeside's when the new container exits with its service's
// update_decline_status before the old one was signalled.
var errDeclined = errors.New("the new container declined the overlap")

// declines tells whether c, a new container of an overlap of svc, ended declining it: exited
// with update_decline_status, or restarting after it (its restart policy's; the engine keeps the
// exit code until it runs again).
func declines(svc spec.Service, c container) bool {
	return svc.UpdateDeclineStatus != nil && (c.State == "exited" || c.State == "restarting") && c.Exit == strconv.Itoa(*svc.UpdateDeclineStatus)
}

// declinedBeside tells whether the service's new container beside old (a failed up --wait's)
// ended declining the overlap; one that cannot be read did not.
func (in *Installer) declinedBeside(ctx context.Context, environ []string, service, old string) bool {
	svc := in.R.Spec.Services[service]
	if svc.UpdateDeclineStatus == nil {
		return false
	}
	cs, err := in.containers(ctx, environ, service)
	if err != nil {
		return false
	}
	for _, c := range cs {
		if c.ID != old && declines(svc, c) {
			return true
		}
	}
	return false
}

// errNoHandoff is startBeside's when, with update_signal, the old container does not mount the
// service's update_socket volume: it has no socket to hand over, nor a ready file to tell an
// earlier one by.
var errNoHandoff = errors.New("its old container does not mount the update_socket volume")

// beforeUp is an error of startBeside's before the new container is made: the old one serves on,
// and there is nothing to remove.
type beforeUp struct{ err error }

func (b beforeUp) Error() string { return b.err.Error() }
func (b beforeUp) Unwrap() error { return b.err }

// updateMount is where the container id mounts the volume of that name (the service's update
// volume as Compose names it, <project>_<UpdateVolume>); mounted false when it does not.
func (in *Installer) updateMount(ctx context.Context, id, volume string) (dir string, mounted bool, err error) {
	out, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"inspect", "--type", "container", "--format", "{{json .Mounts}}", id}})
	if err != nil {
		return "", false, err
	}
	var mounts []struct {
		Type, Name, Destination string
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &mounts); err != nil {
		return "", false, fmt.Errorf("its mounts: %w", err)
	}
	for _, mt := range mounts {
		if mt.Type == "volume" && mt.Name == volume {
			return mt.Destination, true, nil
		}
	}
	return "", false, nil
}

// readyStamp is what tells one ready file from another: its time as the engine's archive keeps it
// (whole seconds), its size and the digest of its content.
type readyStamp struct {
	mod  time.Time
	size int64
	sum  [sha256.Size]byte
}

// readyFile reads file in the container id (`docker cp`, which needs nothing in the image): its
// stamp, there false when the engine answers that the container holds none.
func (in *Installer) readyFile(ctx context.Context, id, file string) (st readyStamp, there bool, err error) {
	out, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"cp", id + ":" + file, "-"}})
	if err != nil {
		// The engine's answers for a path the container lacks, never a connection's or the CLI's.
		if e := strings.ToLower(err.Error()); strings.Contains(e, "could not find the file") || strings.Contains(e, "no such container:path") {
			return readyStamp{}, false, nil
		}
		return readyStamp{}, false, err
	}
	r := tar.NewReader(bytes.NewReader(out))
	h, err := r.Next()
	if err != nil {
		return readyStamp{}, false, fmt.Errorf("reading %s: %w", file, err)
	}
	d := sha256.New()
	if _, err := io.Copy(d, r); err != nil {
		return readyStamp{}, false, fmt.Errorf("reading %s: %w", file, err)
	}
	st = readyStamp{mod: h.ModTime.UTC(), size: h.Size}
	copy(st.sum[:], d.Sum(nil))
	return st, true, nil
}

// takeNetworks reads the networks' record into the start (prevNetworks, prevNetworksErr when it
// cannot be read) and removes it, before the start changes anything: only a start that comes up
// writes it again (recordNetworks), or one that fails having moved no network puts it back
// (keepNetworks). One it cannot remove fails the start there.
func (in *Installer) takeNetworks() error {
	in.prevNetworks, in.prevNetworksErr = "", nil
	raw, err := os.ReadFile(in.path(NetworksFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		in.prevNetworksErr = err
	default:
		in.prevNetworks = strings.TrimSpace(string(raw))
	}
	if err := os.Remove(in.path(NetworksFile)); err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return fmt.Errorf("%s, the record of the stack's networks, cannot be removed (%v): remove it, or a start that fails would leave it for a later one to trust", in.path(NetworksFile), err)
	}
	return nil
}

// keepNetworks puts back the record takeNetworks read, when the start changed no network.
func (in *Installer) keepNetworks() {
	if in.prevNetworks == "" {
		return
	}
	if err := writeAtomic(in.path(NetworksFile), strings.NewReader(in.prevNetworks+"\n"), 0o644, nil); err != nil {
		in.O.logf("warning: the record of the stack's networks is gone (%v): the next start replaces its start-first services stop-first", err)
	}
}

// recordNetworks records the digest of the networks the start that came up ran (NetworksFile);
// takeNetworks removed the last one, so a write that fails leaves none.
func (in *Installer) recordNetworks() error {
	if in.networks == "" {
		return nil
	}
	return writeAtomic(in.path(NetworksFile), strings.NewReader(in.networks+"\n"), 0o644, nil)
}
