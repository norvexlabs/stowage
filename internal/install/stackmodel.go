package install

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/norvexlabs/stowage/internal/tune"
	"go.yaml.in/yaml/v3"
)

// ContentCompose is the stack file of what the installer adds to each service (contentCompose):
// the label of what Compose renders into it, the network settings its namespace keeps of its own,
// its open files and its memory, and the host's MTU on the stack's networks. It is written before
// each `up` and kept between runs.
const ContentCompose = "stowage-content.yaml"

// ContentKey is the install's key of the content labels, private to it: a label is a keyed digest,
// which tells nothing of the secrets it covers.
const ContentKey = ".stowage-content.key"

// ContentLabel labels a service with the digest of the configs and secrets Compose renders into
// its container.
const ContentLabel = "io.stowage.content"

// contentPlaceholder is ContentCompose before the stack's first `up`: every compose command
// names the file.
const contentPlaceholder = "# Written by the installer: what it adds to each service at each start.\n{}\n"

// composeModel is what the installer reads of `docker compose config`: the stack as Compose runs
// it, its variables interpolated and the services of profiles that are off left out, without
// ContentCompose.
type composeModel struct {
	Services map[string]struct {
		NetworkMode string            `json:"network_mode"`
		Sysctls     map[string]string `json:"sysctls"`
		Ulimits     map[string]any    `json:"ulimits"`
		serviceMemory
		Configs []struct {
			Source string `json:"source"`
		} `json:"configs"`
		Secrets []struct {
			Source string `json:"source"`
		} `json:"secrets"`
		Volumes []struct {
			Type   string `json:"type"`
			Source string `json:"source"`
		} `json:"volumes"`
	} `json:"services"`
	Configs  map[string]renderedFile   `json:"configs"`
	Secrets  map[string]renderedFile   `json:"secrets"`
	Networks map[string]composeNetwork `json:"networks"`
	Volumes  map[string]struct {
		Name     string `json:"name"`
		External bool   `json:"external"`
	} `json:"volumes"`
}

// composeNetwork is a network of the compose file, as `docker compose config` renders it.
type composeNetwork struct {
	External   bool              `json:"external"`
	Driver     string            `json:"driver"`
	DriverOpts map[string]string `json:"driver_opts"`
}

// mtuOpt is the bridge driver's option holding a network's MTU.
const mtuOpt = "com.docker.network.driver.mtu"

// takesMTU reports whether the installer gives n the host's MTU: a bridge network the stack makes
// itself, whose compose file sets no MTU of its own.
func (n composeNetwork) takesMTU() bool {
	return !n.External && (n.Driver == "" || n.Driver == "bridge") && n.DriverOpts[mtuOpt] == ""
}

// serviceMemory is what a service sets of its memory and its number of containers, as `docker
// compose config` renders it (0: unset).
type serviceMemory struct {
	MemLimit       composeBytes `json:"mem_limit"`
	MemswapLimit   composeBytes `json:"memswap_limit"`
	MemReservation composeBytes `json:"mem_reservation"`
	Scale          int          `json:"scale"`
	Deploy         struct {
		Replicas  int `json:"replicas"`
		Resources struct {
			Limits struct {
				Memory composeBytes `json:"memory"`
			} `json:"limits"`
			Reservations struct {
				Memory composeBytes `json:"memory"`
			} `json:"reservations"`
		} `json:"resources"`
	} `json:"deploy"`
}

// composeBytes is a size as `docker compose config` renders it: a string of bytes, or a number.
type composeBytes int64

func (b *composeBytes) UnmarshalJSON(raw []byte) error {
	s := strings.Trim(string(raw), `"`)
	if s == "" || s == "null" {
		*b = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("a size of %s: %w", raw, err)
	}
	*b = composeBytes(n)
	return nil
}

// renderedFile is a config or a secret: inline content, or the value of an environment variable
// (the file of one is a bind mount, which a change reaches as it is).
type renderedFile struct {
	Content     string `json:"content"`
	Environment string `json:"environment"`
}

// model reads the stack as Compose runs it with environ, without what ContentCompose adds: a
// setting the model names is the project's.
func (in *Installer) model(ctx context.Context, environ []string) (*composeModel, error) {
	out, err := in.O.Docker.Run(ctx, Cmd{Args: append(in.composeFiles(false), "config", "--format", "json"), Env: environ})
	if err != nil {
		return nil, fmt.Errorf("docker compose config: %w", err)
	}
	var m composeModel
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, fmt.Errorf("docker compose config: %w", err)
	}
	return &m, nil
}

// contentCompose labels each service with an HMAC, under key, of the configs and secrets Compose
// renders into it (inline content, an environment variable's value from environ): Compose copies
// them into the container as it creates it, and recreates none when only they change, so the
// label makes their change one of the service's. It gives each service with a network namespace
// of its own the sysctls of ctr it does not set itself, each service ctr's open files unless it
// sets its own, each service mem shares its memory limit, and each bridge network the stack makes
// ctr's MTU.
func contentCompose(m *composeModel, environ []string, key []byte, ctr tune.Container, mem memoryPlan) ([]byte, error) {
	lookup := environLookup(environ)
	services := map[string]any{}
	for _, name := range sortedKeys(m.Services) {
		svc := m.Services[name]
		h := hmac.New(sha256.New, key)
		n := 0
		add := func(kind string, files map[string]renderedFile, source string) {
			f, ok := files[source]
			if !ok || f.Content == "" && f.Environment == "" {
				return
			}
			v := f.Content
			if f.Environment != "" {
				v = lookup(f.Environment)
			}
			fmt.Fprintf(h, "%s %s %d\n%s", kind, source, len(v), v)
			n++
		}
		for _, c := range svc.Configs {
			add("config", m.Configs, c.Source)
		}
		for _, s := range svc.Secrets {
			add("secret", m.Secrets, s.Source)
		}
		extra := map[string]any{}
		if n > 0 {
			extra["labels"] = map[string]string{ContentLabel: "hmac-sha256:" + hex.EncodeToString(h.Sum(nil))}
		}
		// The host's namespace, or another service's, is no service's own to set.
		if svc.NetworkMode == "" || svc.NetworkMode == "bridge" {
			own := map[string]string{}
			for k, v := range ctr.Sysctls {
				if _, set := svc.Sysctls[k]; !set {
					own[k] = v
				}
			}
			if len(own) > 0 {
				extra["sysctls"] = own
			}
		}
		if _, set := svc.Ulimits["nofile"]; !set && ctr.Nofile > 0 {
			extra["ulimits"] = map[string]any{"nofile": map[string]uint64{"soft": min(ctr.Nofile, tune.NofileSoft), "hard": ctr.Nofile}}
		}
		if share, ok := mem.Shares[name]; ok {
			mem.give(extra, share, svc.serviceMemory)
		}
		if len(extra) > 0 {
			services[name] = extra
		}
	}
	networks := map[string]any{}
	if mtu := networkMTUOpt(ctr.MTU); mtu != "" {
		for _, name := range sortedKeys(m.Networks) {
			if m.Networks[name].takesMTU() {
				networks[name] = map[string]any{"driver_opts": map[string]string{mtuOpt: mtu}}
			}
		}
	}
	if len(services) == 0 && len(networks) == 0 {
		return []byte(contentPlaceholder), nil
	}
	doc := map[string]any{}
	if len(services) > 0 {
		doc["services"] = services
	}
	if len(networks) > 0 {
		doc["networks"] = networks
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return append([]byte("# Written by the installer: what it adds to each service at each start.\n"), out...), nil
}

// contentKey is the install's key of the content labels, made once (0600, never logged): losing it
// only makes the next start recreate the services that carry a label.
func (in *Installer) contentKey() ([]byte, error) {
	path := in.path(ContentKey)
	raw, err := os.ReadFile(path)
	if err == nil && len(raw) == 64 {
		if key, err := hex.DecodeString(string(raw)); err == nil {
			return key, nil
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := writeAtomic(path, strings.NewReader(hex.EncodeToString(key)), 0o600, nil); err != nil {
		return nil, err
	}
	return key, nil
}

// environLookup reads a variable of environ, its last value; "" when absent.
func environLookup(environ []string) func(string) string {
	return func(k string) string {
		for _, kv := range slices.Backward(environ) {
			if name, v, ok := strings.Cut(kv, "="); ok && name == k {
				return v
			}
		}
		return ""
	}
}

// networkMTUOpt is the driver option a network of the host's MTU (0: unknown) takes, 1500
// included, so the daemon's own default-network-opts never stands in for it; "" when the MTU
// does not read.
func networkMTUOpt(mtu int) string {
	if mtu <= 0 {
		return ""
	}
	return strconv.Itoa(mtu)
}

// madeMTU is a network's MTU as its driver option says: none is Docker's 1500.
func madeMTU(opt string) string { return cmp.Or(strings.TrimSpace(opt), "1500") }

// previousMTU is the MTU the last start gave the stack's networks (ContentCompose's); 0 when it
// gave none.
func (in *Installer) previousMTU() int {
	raw, err := os.ReadFile(in.path(ContentCompose))
	if err != nil {
		return 0
	}
	var doc struct {
		Networks map[string]struct {
			DriverOpts map[string]string `yaml:"driver_opts"`
		} `yaml:"networks"`
	}
	if yaml.Unmarshal(raw, &doc) != nil {
		return 0
	}
	for _, name := range sortedKeys(doc.Networks) {
		if n, err := strconv.Atoi(doc.Networks[name].DriverOpts[mtuOpt]); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// networkMTU stops the stack when a network the installer gives the host's MTU (mtu) was made
// with another, as its driver option says: a network's MTU is set when it is made, so `up` makes
// it again. Volumes are kept. An MTU that does not read (no default route this moment) judges
// nothing.
func (in *Installer) networkMTU(ctx context.Context, environ []string, m *composeModel, mtu int) error {
	if mtu <= 0 {
		return nil
	}
	want := networkMTUOpt(mtu)
	out, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"network", "ls", "--quiet", "--filter", "label=com.docker.compose.project=" + environLookup(environ)("COMPOSE_PROJECT_NAME")}})
	if err != nil {
		return fmt.Errorf("listing the stack's networks: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil
	}
	out, err = in.O.Docker.Run(ctx, Cmd{Args: append([]string{"network", "inspect", "--format", "{{index .Labels \"com.docker.compose.network\"}} {{index .Options \"" + mtuOpt + "\"}}"}, ids...)})
	if err != nil {
		return fmt.Errorf("reading the stack's networks: %w", err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		name, made, _ := strings.Cut(line, " ")
		n, ok := m.Networks[name]
		if name == "" || !ok || !n.takesMTU() || madeMTU(made) == want {
			continue
		}
		in.O.logf("the host's MTU is %d, network %s was made with %s: the stack stops, and its networks are made again", mtu, name, madeMTU(made))
		if _, err := in.O.Docker.Run(ctx, Cmd{Args: append(in.composeBase(), "down", "--remove-orphans"), Env: environ, Stream: in.O.Log}); err != nil {
			return fmt.Errorf("stopping the stack to make its networks again: %w", err)
		}
		return nil
	}
	return nil
}

// writeContent writes ContentCompose for the stack as Compose runs it with environ, by the
// install's record env (never the shell's), and makes the stack's networks again when the host's
// MTU changed.
func (in *Installer) writeContent(ctx context.Context, m *composeModel, env *EnvFile, environ []string) error {
	key, err := in.contentKey()
	if err != nil {
		return fmt.Errorf("%s: %w", ContentKey, err)
	}
	ctr, mem, err := in.containerSizing(ctx, func(k string) string { v, _ := env.Get(k); return v })
	if err != nil {
		return err
	}
	if ctr.MTUUnread {
		// No default route this moment: the networks keep the MTU the last start gave them,
		// rather than be made again without it.
		ctr.MTU = in.previousMTU()
	}
	raw, err := contentCompose(m, environ, key, ctr, mem)
	if err != nil {
		return err
	}
	if err := writeAtomic(in.path(ContentCompose), bytes.NewReader(raw), 0o644, nil); err != nil {
		return err
	}
	return in.networkMTU(ctx, environ, m, ctr.MTU)
}

// VolumesPending lists, one per line, the volumes a start made that still await their uid: written
// before the start makes them, struck off once they have it, so a start cut off in between gives
// them their uid when a later start mounts them again.
const VolumesPending = ".stowage-volumes-pending"

// newVolumes are the data volumes with a uid that a service of m mounts and that the engine does
// not hold yet (the ones this start makes) or that VolumesPending lists: their Docker names and
// uids, in the order of their keys. keep is what VolumesPending goes on listing besides them: a
// volume that awaits its uid and no service mounts now. A listed volume the engine no longer
// holds, or that a service mounts while no uid is owed to it, is struck off. Any other volume
// that exists keeps its owner (its data's), and an external one is never the stack's.
func (in *Installer) newVolumes(ctx context.Context, m *composeModel) (names []string, uids []int, keep []string, err error) {
	mounted := map[string]bool{}
	for _, svc := range m.Services {
		for _, v := range svc.Volumes {
			if v.Type == "volume" {
				mounted[v.Source] = true
			}
		}
	}
	vols := in.R.Spec.Data.Volumes
	owed := map[string]string{} // Docker name → key
	for _, k := range sortedKeys(vols) {
		if vols[k].UID != nil && mounted[k] && !m.Volumes[k].External && m.Volumes[k].Name != "" {
			owed[m.Volumes[k].Name] = k
		}
	}
	raw, err := os.ReadFile(in.path(VolumesPending))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil, err
	}
	var pending []string
	for line := range strings.Lines(string(raw)) {
		if v := strings.TrimSpace(line); v != "" {
			pending = append(pending, v)
		}
	}
	if len(owed) == 0 && len(pending) == 0 {
		return nil, nil, nil, nil
	}
	out, err := in.O.Docker.Run(ctx, Cmd{Args: []string{"volume", "ls", "--format", "{{.Name}}"}})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listing the engine's volumes: %w", err)
	}
	held := map[string]bool{}
	for line := range strings.Lines(string(out)) {
		held[strings.TrimSpace(line)] = true
	}
	mountedName := map[string]bool{}
	for k, v := range m.Volumes {
		if mounted[k] {
			mountedName[v.Name] = true
		}
	}
	isPending := map[string]bool{}
	for _, name := range pending {
		isPending[name] = true
		if held[name] && !mountedName[name] {
			keep = append(keep, name)
		}
	}
	for _, k := range sortedKeys(vols) {
		name := m.Volumes[k].Name
		if owed[name] == k && (!held[name] || isPending[name]) {
			names = append(names, name)
			uids = append(uids, *vols[k].UID)
		}
	}
	return names, uids, keep, nil
}

// writePending records the volumes that await their uid, or removes the record when none does.
func (in *Installer) writePending(names []string) error {
	if len(names) == 0 {
		if err := os.Remove(in.path(VolumesPending)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return writeAtomic(in.path(VolumesPending), strings.NewReader(strings.Join(names, "\n")+"\n"), 0o600, nil)
}

// ownVolumes gives each volume to its uid when its root belongs to another user: Docker makes a
// new volume owned by the image's directory it mounts on, root when the image has none. It runs
// once the stack's containers are made, so a new volume already holds what the image put in it, as
// root in the release's volume helper (ChownImageVar); nothing reaches the network. A symbolic link in
// a volume is changed itself, never what it points to. given is false when the release carries no
// helper: the volumes then keep the owner Docker gave them, and await their uid.
func (in *Installer) ownVolumes(ctx context.Context, env *EnvFile, names []string, uids []int) (given bool, err error) {
	if len(names) == 0 {
		return true, nil
	}
	image, ok := env.Get(ChownImageVar)
	if !ok {
		in.O.logf("%s %s carries no volume helper: %s keep the owner Docker gave them until a release with one starts", in.R.Spec.Name, in.R.Manifest.ProjectVersion, strings.Join(names, ", "))
		return false, nil
	}
	args := []string{"run", "--rm", "--pull", "never", "--network", "none", "--user", "0", "--entrypoint", "sh"}
	var script strings.Builder
	script.WriteString("set -e\n")
	for i, name := range names {
		dir := "/v/" + strconv.Itoa(i)
		args = append(args, "--volume", name+":"+dir)
		fmt.Fprintf(&script, "if [ \"$(stat -c %%u %[1]s)\" != %[2]d ]; then chown -hR %[2]d %[1]s; echo %[3]s; fi\n", dir, uids[i], name)
	}
	args = append(args, image, "-c", script.String())
	out, err := in.O.Docker.Run(ctx, Cmd{Args: args, Env: in.O.environ()})
	if err != nil {
		return false, fmt.Errorf("giving the data volumes their uid (%s): %w", strings.Join(names, ", "), err)
	}
	for line := range strings.Lines(string(out)) {
		if v := strings.TrimSpace(line); v != "" {
			in.O.logf("volume %s given to its uid", v)
		}
	}
	return true, nil
}
