package tune

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// The files tune writes, under the system's root. They are named for Stowage, not for one stack:
// the rules are the same for every stack, so two stacks on one host write the same values.
const (
	SysctlFile  = "etc/sysctl.d/60-stowage-net.conf"
	ModulesFile = "etc/modules-load.d/60-stowage-net.conf"
	UdevFile    = "etc/udev/rules.d/60-stowage-net.rules"
	DaemonFile  = "etc/docker/daemon.json"
	// StateFile records what the first apply replaced, for `--revert`.
	StateFile = "etc/stowage/tune.json"
)

// daemonWant is one key tune sets in the Docker daemon's file.
type daemonWant struct {
	path  []string
	value any
	show  string
	ok    func(have json.RawMessage) bool
	// flag is dockerd's flag for the key: a daemon given both refuses to start.
	flag string
}

func (s System) daemonWants() []daemonWant {
	w := []daemonWant{{
		path:  []string{"default-ulimits", "nofile"},
		value: map[string]any{"Name": "nofile", "Soft": NofileLimit, "Hard": NofileLimit},
		show:  fmt.Sprintf("Soft and Hard ≥ %d", NofileLimit),
		flag:  "--default-ulimit",
		ok: func(have json.RawMessage) bool {
			var u struct{ Soft, Hard int64 }
			return json.Unmarshal(have, &u) == nil && u.Soft >= NofileLimit && u.Hard >= NofileLimit
		},
	}}
	if mtu := s.MTU(); s.Docker.NetworkOpts && mtu > 0 && mtu != 1500 {
		v := strconv.Itoa(mtu)
		w = append(w, daemonWant{
			path:  []string{"default-network-opts", "bridge", "com.docker.network.driver.mtu"},
			value: v,
			show:  v + " (the host's MTU, on the networks made from now on)",
			flag:  "--default-network-opt",
			ok:    func(have json.RawMessage) bool { return string(have) == strconv.Quote(v) },
		})
	}
	return w
}

// File is one file a plan writes.
type File struct {
	Path    string // under the root
	Content []byte
	Old     []byte // nil: absent
}

// Changed reports whether writing the file changes it.
func (f File) Changed() bool { return f.Old == nil || !bytes.Equal(f.Old, f.Content) }

// Plan is what an apply writes and sets.
type Plan struct {
	Host   Host
	Files  []File
	Daemon *File // nil: the daemon file is left alone
	// DaemonKeys are the daemon file's keys the plan changes, as key: old → new.
	DaemonKeys []string
	// Notes are what the plan cannot do.
	Notes   []string
	records []daemonRecord
	// values are the settings it writes, in Sysctls' order: Want, or a ceiling the host holds
	// beyond it.
	values [][2]string
}

// state is StateFile: what the first apply found, so a revert puts it back.
type state struct {
	Sysctls map[string]string `json:"sysctls"`
	// Wrote is what the last apply wrote: a value that still holds it is judged against Sysctls,
	// one changed since is the host's own.
	Wrote  map[string]string `json:"wrote"`
	Daemon []daemonRecord    `json:"daemon"`
}

// base is what c is judged against: the value the host held before the first apply while the
// last apply's value holds, else the current one; ok is false when there is neither.
func (st *state) base(c Sysctl, cur string, haveCur bool) (string, bool) {
	rec, recorded := st.Sysctls[c.Key]
	switch {
	case recorded && (!haveCur || st.Wrote[c.Key] == cur):
		return rec, true
	case haveCur:
		return cur, true
	}
	return "", false
}

// daemonRecord is a daemon file key tune set: its file (under the root), its value before (absent:
// none) and the value set.
type daemonRecord struct {
	File   string          `json:"file"`
	Path   []string        `json:"path"`
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after"`
}

func (s System) header(h Host) string {
	return fmt.Sprintf("# Written by `%s tune --apply` for a host of %d MiB and %d CPUs: `%s tune --revert` removes it.\n", s.Ctl, h.MemBytes/mib, h.CPUs, s.Ctl)
}

// Plan computes the files for h.
func (s System) Plan(h Host) (*Plan, error) {
	if s.Docker != nil && s.Docker.Desktop {
		return nil, fmt.Errorf("Docker Desktop runs the containers in its own VM: this host's settings do not reach them")
	}
	st, err := s.readState()
	if err != nil {
		return nil, err
	}
	p := &Plan{Host: h}
	var b strings.Builder
	b.WriteString(s.header(h))
	b.WriteString("# A memory ceiling is a share of RAM, which the kernel uses under load only.\n")
	for _, c := range h.Sysctls() {
		// Judged against what the host held before the first apply, when one recorded it.
		v := c.Want
		cur, haveCur := s.read(c.Path())
		if base, ok := st.base(c, strings.Join(strings.Fields(cur), " "), haveCur); ok {
			v = c.Value(base)
		}
		p.values = append(p.values, [2]string{c.Key, v})
		fmt.Fprintf(&b, "\n# %s\n%s = %s\n", c.Why, c.Key, v)
	}
	files := map[string]string{
		SysctlFile:  b.String(),
		ModulesFile: s.header(h) + strings.Join(Modules, "\n") + "\n",
	}
	b.Reset()
	b.WriteString(s.header(h))
	if eth, err := s.Look("ethtool"); err != nil || !filepath.IsAbs(eth) {
		p.Notes = append(p.Notes, "ethtool is not installed: the NICs' UDP receive offload for forwarded traffic is left as it is")
	} else {
		b.WriteString("# A NIC keeps forwarded UDP (QUIC, tunnels, traffic to containers) batched.\n")
		fmt.Fprintf(&b, "ACTION==\"add\", SUBSYSTEM==\"net\", DEVPATH!=\"/devices/virtual/*\", RUN+=\"%s -K $name rx-udp-gro-forwarding on rx-gro-list off\"\n", eth)
	}
	b.WriteString("# A TUN or TAP device made on the host: a longer queue, its packets spread over every CPU.\n")
	fmt.Fprintf(&b, "ACTION==\"add\", SUBSYSTEM==\"net\", TEST==\"tun_flags\", ATTR{tx_queue_len}=\"%d\", ATTR{queues/rx-0/rps_cpus}=\"%s\", ATTR{queues/rx-0/rps_flow_cnt}=\"%d\"\n",
		h.TunQueueLen(), h.RPSMask(), h.FlowEntries())
	files[UdevFile] = b.String()
	for _, name := range []string{SysctlFile, ModulesFile, UdevFile} {
		f := File{Path: name, Content: []byte(files[name])}
		if old, err := os.ReadFile(s.path(name)); err == nil {
			f.Old = old
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		p.Files = append(p.Files, f)
	}
	if s.Docker == nil || s.Docker.Rootless || s.Docker.Desktop {
		return p, nil
	}
	return p, s.planDaemon(p)
}

// planDaemon merges the daemon keys into the daemon file, keeping every other key.
func (s System) planDaemon(p *Plan) error {
	daemon := s.daemonFile()
	old, err := os.ReadFile(s.path(daemon))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	doc := map[string]any{}
	if len(bytes.TrimSpace(old)) > 0 {
		d := json.NewDecoder(bytes.NewReader(old))
		d.UseNumber()
		if err := d.Decode(&doc); err != nil {
			return fmt.Errorf("%s: %w", s.path(daemon), err)
		}
	}
	args := s.dockerdArgs()
	for _, w := range s.daemonWants() {
		have, ok, _ := daemonValue(old, w.path)
		if ok && w.ok(have) {
			continue
		}
		if args[w.flag] != "" {
			p.Notes = append(p.Notes, fmt.Sprintf("%s is left out: dockerd runs with %s, and refuses to start when its file repeats one", strings.Join(w.path, "."), w.flag))
			continue
		}
		after, err := json.Marshal(w.value)
		if err != nil {
			return err
		}
		if err := setPath(doc, w.path, w.value); err != nil {
			return fmt.Errorf("%s: %w", s.path(daemon), err)
		}
		p.DaemonKeys = append(p.DaemonKeys, fmt.Sprintf("%s: %s → %s", strings.Join(w.path, "."), cmp.Or(string(have), "absent"), after))
		r := daemonRecord{File: daemon, Path: w.path, After: after}
		if ok {
			r.Before = have
		}
		p.records = append(p.records, r)
	}
	if len(p.records) == 0 {
		return nil
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	p.Daemon = &File{Path: daemon, Content: append(out, '\n'), Old: old}
	return nil
}

// setPath sets doc's value at path, making the objects on the way.
func setPath(doc map[string]any, path []string, v any) error {
	for i, k := range path[:len(path)-1] {
		next, ok := doc[k]
		if !ok {
			next = map[string]any{}
			doc[k] = next
		}
		m, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("%s is not an object", strings.Join(path[:i+1], "."))
		}
		doc = m
	}
	doc[path[len(path)-1]] = v
	return nil
}

// deletePath removes doc's value at path, and the objects it leaves empty.
func deletePath(doc map[string]any, path []string) {
	if len(path) == 1 {
		delete(doc, path[0])
		return
	}
	m, ok := doc[path[0]].(map[string]any)
	if !ok {
		return
	}
	deletePath(m, path[1:])
	if len(m) == 0 {
		delete(doc, path[0])
	}
}

// Describe writes what the plan changes.
func (p *Plan) Describe(w io.Writer, root string) {
	for _, f := range p.Files {
		if !f.Changed() {
			fmt.Fprintf(w, "%s: unchanged\n", filepath.Join(root, f.Path))
			continue
		}
		fmt.Fprintf(w, "%s:\n", filepath.Join(root, f.Path))
		for l := range strings.SplitSeq(strings.TrimRight(string(f.Content), "\n"), "\n") {
			fmt.Fprintf(w, "  %s\n", l)
		}
	}
	if p.Daemon != nil {
		fmt.Fprintf(w, "%s (its other keys kept):\n", filepath.Join(root, p.Daemon.Path))
		for _, k := range p.DaemonKeys {
			fmt.Fprintf(w, "  %s\n", k)
		}
	}
	for _, n := range p.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
}

// Changes reports whether the plan writes anything.
func (p *Plan) Changes() bool {
	return p.Daemon != nil || slices.ContainsFunc(p.Files, File.Changed)
}

// writeFile writes path under the root atomically, its directory made when missing.
func (s System) writeFile(rel string, content []byte, mode os.FileMode) error {
	path := s.path(rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp := path + ".stowage-tmp"
	if err := os.WriteFile(tmp, content, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	if err := os.Rename(tmp, path); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	return nil
}

func (s System) readState() (*state, error) {
	raw, err := os.ReadFile(s.path(StateFile))
	if errors.Is(err, os.ErrNotExist) {
		return &state{Sysctls: map[string]string{}, Wrote: map[string]string{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var st state
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&st); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path(StateFile), err)
	}
	if st.Sysctls == nil {
		st.Sysctls = map[string]string{}
	}
	if st.Wrote == nil {
		st.Wrote = map[string]string{}
	}
	return &st, nil
}

// CheckRoot refuses an apply or a revert not run as root, before anything is asked.
func (s System) CheckRoot() error { return s.root() }

func (s System) root() error {
	if s.Euid() != 0 {
		return fmt.Errorf("tune writes %s: run it as root", filepath.Join(s.Root, "etc"))
	}
	return nil
}

// ApplyOptions are how an apply runs.
type ApplyOptions struct {
	// Boot: the apply runs before Docker starts, which then reads its file as written.
	Boot bool
	// RestartDocker restarts the engine when its file changed, which restarts the containers.
	RestartDocker bool
}

// Apply writes the plan's files and applies them to the running host. It returns what did not
// apply, which a reboot may.
func (s System) Apply(ctx context.Context, p *Plan, o ApplyOptions) ([]string, error) {
	if err := s.root(); err != nil {
		return nil, err
	}
	st, err := s.readState()
	if err != nil {
		return nil, err
	}
	// The modules first: the settings they bring are then there to record.
	notes := s.loadModules(ctx)
	// What the first apply finds is what a revert puts back: a later apply records only keys the
	// earlier ones did not set.
	for _, c := range p.Host.Sysctls() {
		if _, ok := st.Sysctls[c.Key]; !ok {
			if v, ok := s.read(c.Path()); ok {
				st.Sysctls[c.Key] = strings.Join(strings.Fields(v), " ")
			}
		}
	}
	for _, kv := range p.values {
		st.Wrote[kv[0]] = kv[1]
	}
	for _, r := range p.records {
		i := slices.IndexFunc(st.Daemon, func(o daemonRecord) bool { return o.File == r.File && slices.Equal(o.Path, r.Path) })
		if i < 0 {
			st.Daemon = append(st.Daemon, r)
		} else {
			st.Daemon[i].After = r.After
		}
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := s.writeFile(StateFile, append(raw, '\n'), 0o644); err != nil {
		return nil, err
	}
	for _, f := range p.Files {
		if f.Changed() {
			if err := s.writeFile(f.Path, f.Content, 0o644); err != nil {
				return nil, err
			}
		}
	}
	if p.Daemon != nil {
		if err := s.writeFile(p.Daemon.Path, p.Daemon.Content, 0o644); err != nil {
			return nil, err
		}
	}
	notes = append(notes, s.live(ctx, p)...)
	switch {
	case p.Daemon == nil || o.Boot:
	case o.RestartDocker:
		if _, err := s.Run(ctx, "systemctl", "restart", "docker"); err != nil {
			notes = append(notes, fmt.Sprintf("restarting Docker: %v", err))
		}
	default:
		notes = append(notes, "Docker reads its file when it restarts (`systemctl restart docker`, which restarts the containers): until then it runs as before")
	}
	return notes, nil
}

// loadModules loads Modules into the running kernel.
func (s System) loadModules(ctx context.Context) []string {
	var notes []string
	for _, m := range Modules {
		if _, err := s.Run(ctx, "modprobe", m); err != nil {
			notes = append(notes, fmt.Sprintf("modprobe %s: %v", m, err))
		}
	}
	return notes
}

// live applies the plan to the running host, its modules loaded: the settings, the NICs and the
// TUN devices already there.
func (s System) live(ctx context.Context, p *Plan) []string {
	h := p.Host
	var notes []string
	if q, _ := s.read(Sysctl{Key: "net.core.default_qdisc"}.Path()); q != "fq" {
		notes = append(notes, "an interface already up takes the fq queue when it next comes up")
	}
	for _, kv := range p.values {
		if err := os.WriteFile(s.path(Sysctl{Key: kv[0]}.Path()), []byte(kv[1]+"\n"), 0o644); err != nil {
			notes = append(notes, fmt.Sprintf("%s: %v", kv[0], err))
		}
	}
	all := s.nics()
	if eth, err := s.Look("ethtool"); err == nil {
		for _, n := range all {
			if n.physical {
				if _, err := s.Run(ctx, eth, "-K", n.name, "rx-udp-gro-forwarding", "on", "rx-gro-list", "off"); err != nil {
					notes = append(notes, fmt.Sprintf("ethtool -K %s: %v", n.name, err))
				}
			}
		}
	}
	for _, n := range all {
		if !n.tun {
			continue
		}
		dir := filepath.Join("sys/class/net", n.name)
		for _, a := range [][2]string{
			{"tx_queue_len", nums(h.TunQueueLen())},
			{"queues/rx-0/rps_cpus", h.RPSMask()},
			{"queues/rx-0/rps_flow_cnt", nums(h.FlowEntries())},
		} {
			if err := os.WriteFile(s.path(filepath.Join(dir, a[0])), []byte(a[1]+"\n"), 0o644); err != nil {
				notes = append(notes, fmt.Sprintf("%s %s: %v", n.name, a[0], err))
			}
		}
	}
	return notes
}

// Revert removes the files, puts back the daemon keys still holding what tune set and the
// settings the first apply found. It returns what it left.
func (s System) Revert(ctx context.Context, restartDocker bool) ([]string, error) {
	if err := s.root(); err != nil {
		return nil, err
	}
	st, err := s.readState()
	if err != nil {
		return nil, err
	}
	var notes []string
	for _, f := range []string{SysctlFile, ModulesFile, UdevFile} {
		if err := os.Remove(s.path(f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if len(st.Daemon) > 0 {
		changed := false
		byFile := map[string][]daemonRecord{}
		for _, r := range st.Daemon {
			f := cmp.Or(r.File, DaemonFile)
			byFile[f] = append(byFile[f], r)
		}
		for _, f := range slices.Sorted(maps.Keys(byFile)) {
			c, err := s.revertDaemon(f, byFile[f], &notes)
			if err != nil {
				return nil, err
			}
			changed = changed || c
		}
		switch {
		case !changed:
		case restartDocker:
			if _, err := s.Run(ctx, "systemctl", "restart", "docker"); err != nil {
				notes = append(notes, fmt.Sprintf("restarting Docker: %v", err))
			}
		default:
			notes = append(notes, "Docker reads its file when it restarts (`systemctl restart docker`, which restarts the containers): until then it runs as tuned")
		}
	}
	for _, k := range slices.Sorted(maps.Keys(st.Sysctls)) {
		c := Sysctl{Key: k}
		if err := os.WriteFile(s.path(c.Path()), []byte(st.Sysctls[k]+"\n"), 0o644); err != nil {
			notes = append(notes, fmt.Sprintf("%s: %v", k, err))
		}
	}
	notes = append(notes, "the NICs' offload and the TUN devices' queues stay as they are until the next boot")
	if err := os.Remove(s.path(StateFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return notes, nil
}

// revertDaemon puts each recorded key back, unless it changed since tune set it.
func (s System) revertDaemon(daemon string, records []daemonRecord, notes *[]string) (bool, error) {
	old, err := os.ReadFile(s.path(daemon))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	doc := map[string]any{}
	d := json.NewDecoder(bytes.NewReader(old))
	d.UseNumber()
	if err := d.Decode(&doc); err != nil {
		return false, fmt.Errorf("%s: %w", s.path(daemon), err)
	}
	changed := false
	for _, r := range records {
		have, ok, _ := daemonValue(old, r.Path)
		if !ok || !sameJSON(have, r.After) {
			*notes = append(*notes, fmt.Sprintf("%s: left as it is, it changed since tune set it", strings.Join(r.Path, ".")))
			continue
		}
		if r.Before == nil {
			deletePath(doc, r.Path)
		} else {
			var v any
			if err := json.Unmarshal(r.Before, &v); err != nil {
				return false, err
			}
			if err := setPath(doc, r.Path, v); err != nil {
				return false, err
			}
		}
		changed = true
	}
	if !changed {
		return false, nil
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return false, err
	}
	return true, s.writeFile(daemon, append(out, '\n'), 0o644)
}

// sameJSON compares two JSON values whatever their spacing (the state file indents them).
func sameJSON(a, b json.RawMessage) bool {
	var ca, cb bytes.Buffer
	return json.Compact(&ca, a) == nil && json.Compact(&cb, b) == nil && bytes.Equal(ca.Bytes(), cb.Bytes())
}
