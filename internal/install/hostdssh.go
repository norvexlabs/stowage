package install

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"
)

// sshOwner keys SSH's revert in the daemon's armed reverts (netReverts), beside the NICs' (keyed
// by role, which never holds a slash).
const sshOwner = "/ssh"

// SSHAllowFile is the record `ssh-allow set` keeps in the VM's install dir dir (/usr/local, kept
// across boots): SSH's sources and the change awaiting confirmation. While it is there, every boot
// writes its sources (ssh-boot) over the cloud-config's (`vm create --ssh-allow`), which are then the
// first boot's alone. The unit that runs ssh-boot names this path: kairos/files' NAME-ssh.service.
func SSHAllowFile(dir string) string { return filepath.Join(dir, "ssh-allow.yaml") }

// parseSSHSources is flag's sources, each a CIDR with its prefix (192.0.2.0/24, 2001:db8::/32),
// masked: `vm create --ssh-allow`'s rule and `ssh-allow set`'s.
func parseSSHSources(flag string, vs []string) ([]netip.Prefix, error) {
	var ps []netip.Prefix
	for _, v := range vs {
		p, err := netip.ParsePrefix(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not a source with its prefix (192.0.2.0/24, 2001:db8::/32)", flag, v)
		}
		if p.Addr().Is4In6() {
			// An IPv4 network written in IPv6 (::ffff:a.b.c.d/N): an ip6 rule would never match an
			// IPv4 client.
			if p.Bits() < 96 {
				return nil, fmt.Errorf("%s: %q spans beyond IPv4's mapped addresses: write the IPv4 network (192.0.2.0/24)", flag, v)
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		ps = append(ps, p.Masked())
	}
	return ps, nil
}

// sshRules is SSH's rule file (SSHRules) accepting ps alone, headed by a comment naming what wrote it.
func sshRules(head string, ps []netip.Prefix) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", head)
	for _, p := range ps {
		family := "ip"
		if p.Addr().Is6() {
			family = "ip6"
		}
		fmt.Fprintf(&b, "%s saddr %s tcp dport 22 accept\n", family, p.Masked())
	}
	return b.String()
}

// readSSHRules are the sources SSH's rule file text accepts, as sshRules writes them, every
// source for the image's own rule (`tcp dport 22 accept`); false when a line is neither.
func readSSHRules(text string) ([]string, bool) {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		switch {
		case slices.Equal(f, []string{"tcp", "dport", "22", "accept"}):
			out = append(out, "0.0.0.0/0", "::/0")
		case len(f) == 7 && (f[0] == "ip" || f[0] == "ip6") && f[1] == "saddr" && slices.Equal(f[3:], []string{"tcp", "dport", "22", "accept"}):
			p, err := netip.ParsePrefix(f[2])
			if err != nil || (f[0] == "ip6") != p.Addr().Is6() {
				return nil, false
			}
			out = append(out, p.Masked().String())
		default:
			return nil, false
		}
	}
	return out, true
}

// SSHRequest is `ssh-allow set`'s: the sources SSH accepts from now on; RevertSecs as
// NetRequest's (nil: defaultRevertSecs, 0: no window).
type SSHRequest struct {
	Sources    []string `json:"sources"`
	RevertSecs *int     `json:"revert_secs,omitempty"`
}

// SSHAllow is the daemon's answer on SSH's sources: Sources what the rule file accepts (none when
// it does not read as SSH's sources, Rules then saying why), From what set them ("set": `ssh-allow
// set`; "cloud-config": the seed's, as at the first boot; "image": every source), Pending a change
// awaiting confirmation.
type SSHAllow struct {
	Message string      `json:"message,omitempty"`
	Sources []string    `json:"sources"`
	From    string      `json:"from"`
	Rules   string      `json:"rules,omitempty"`
	Pending *SSHPending `json:"pending,omitempty"`
}

// SSHPending is a change awaiting confirmation: Previous the sources its revert brings back.
type SSHPending struct {
	Previous []string  `json:"previous"`
	Deadline time.Time `json:"deadline"`
	SecsLeft int       `json:"secs_left"`
	// Ended: the change is being undone (its window ended, a revert was asked, or it could not be
	// applied: EndedBy), or its undo failed and is tried again: a confirmation comes too late, and
	// tries the undo again.
	Ended   bool   `json:"ended,omitempty"`
	EndedBy string `json:"ended_by,omitempty"`
}

// sshState is the record (SSHAllowFile): Sources set by `ssh-allow set`; Pending the change
// awaiting confirmation, whose revert writes PreviousRules back and records Previous (none: the
// record goes, and the cloud-config's stage applies again at the next boot).
type sshState struct {
	Sources []string    `yaml:"sources"`
	Pending *sshPending `yaml:"pending,omitempty"`
}

type sshPending struct {
	ID            string    `yaml:"id"`
	Deadline      time.Time `yaml:"deadline"`
	Previous      []string  `yaml:"previous,omitempty"`
	PreviousRules string    `yaml:"previous_rules"`
	// PreviousAbsent: there was no rule file before the change; its revert removes the one it wrote.
	PreviousAbsent bool `yaml:"previous_absent,omitempty"`
}

// sshPaths are the record's and the rule file's paths.
func (h *Hostd) sshPaths() (record, rules string) {
	return SSHAllowFile(h.O.Dir), cmp.Or(h.SSHRules, SSHRules)
}

// sshRead strictly decodes the record; an absent one is the zero state. Every source it holds is
// one parseSSHSources takes.
func (h *Hostd) sshRead() (sshState, bool, error) {
	record, _ := h.sshPaths()
	var st sshState
	raw, err := os.ReadFile(record)
	if errors.Is(err, os.ErrNotExist) {
		return st, false, nil
	}
	if err != nil {
		return st, false, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&st); err != nil && !errors.Is(err, io.EOF) {
		return st, false, fmt.Errorf("%s: %w (remove it: the cloud-config's sources apply at the next boot)", record, err)
	}
	for _, l := range []*[]string{&st.Sources, sshPrevious(st.Pending)} {
		ps, err := parseSSHSources(record, *l)
		if err != nil {
			return st, false, err
		}
		// Masked, as the rule file holds them: a record edited by hand compares as written.
		for i, p := range ps {
			(*l)[i] = p.String()
		}
	}
	if len(st.Sources) == 0 {
		return st, false, fmt.Errorf("%s: no sources (remove it: the cloud-config's sources apply at the next boot)", record)
	}
	return st, true, nil
}

// sshPrevious is p's previous sources to read in place; an empty list without a pending change.
func sshPrevious(p *sshPending) *[]string {
	if p == nil {
		return &[]string{}
	}
	return &p.Previous
}

// sshWrite records st, root's alone; a state without sources removes the record.
func (h *Hostd) sshWrite(st sshState) error {
	record, _ := h.sshPaths()
	if len(st.Sources) == 0 {
		if err := os.Remove(record); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	raw, err := yaml.Marshal(st)
	if err != nil {
		return err
	}
	head := fmt.Sprintf("# %s's SSH sources, which `%s ssh-allow set` changes: the cloud-config's apply while this file is absent.\n", h.R.Spec.Name, h.R.Ctl())
	return writePrivate(record, append([]byte(head), raw...))
}

// sshApply writes SSH's rule file and reloads the firewall, whose ruleset flushes its own table
// alone: Docker's rules stay.
//
// A load that fails leaves the ruleset loaded before it (nft -f is atomic): the rule file before it
// is written back, so the file always holds what the firewall loaded.
func (h *Hostd) sshApply(ctx context.Context, text string) error {
	_, rules := h.sshPaths()
	unlock, err := h.sshFirewallLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	old, rerr := os.ReadFile(rules)
	absent := errors.Is(rerr, os.ErrNotExist)
	if rerr != nil && !absent {
		return rerr
	}
	if err := writeAtomic(rules, strings.NewReader(text), 0o644, nil); err != nil {
		return err
	}
	if _, err := h.Host.Run(ctx, Cmd{Args: []string{"nft", "-f", FirewallConf}}); err != nil {
		var back error
		if absent {
			back = os.Remove(rules)
		} else {
			back = writeAtomic(rules, bytes.NewReader(old), 0o644, nil)
		}
		if back != nil {
			return fmt.Errorf("the firewall with SSH's sources: %w; writing the rule file before it back failed too: %v", err, back)
		}
		return fmt.Errorf("the firewall with SSH's sources: %w (the rule file before it is back)", err)
	}
	return nil
}

// sshPut puts the rule file before a change back: text, or no file when there was none (absent),
// and loads the firewall again.
func (h *Hostd) sshPut(ctx context.Context, text string, absent bool) error {
	if !absent {
		return h.sshApply(ctx, text)
	}
	_, rules := h.sshPaths()
	unlock, err := h.sshFirewallLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	old, rerr := os.ReadFile(rules)
	if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return rerr
	}
	if err := os.Remove(rules); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := h.Host.Run(ctx, Cmd{Args: []string{"nft", "-f", FirewallConf}}); err != nil {
		// The firewall kept its ruleset: the file it was loaded from goes back.
		if rerr == nil {
			if back := writeAtomic(rules, bytes.NewReader(old), 0o644, nil); back != nil {
				return fmt.Errorf("the firewall without SSH's rule file: %w; writing the rule file back failed too: %v", err, back)
			}
		}
		return fmt.Errorf("the firewall without SSH's rule file: %w (the rule file is back)", err)
	}
	return nil
}

// sshFirewallLock is the firewall's lock (firewallLock) in O.Firewall, which every process on the
// VM sets (PortRules); without it, beside the rule file.
//
// Its wait is bounded (revertRetrySecs): taken under netMu, a holder that hangs would stall every
// network and SSH request and every revert; a wait that ends fails the change, which an undo then
// tries again.
func (h *Hostd) sshFirewallLock(ctx context.Context) (func(), error) {
	_, rules := h.sshPaths()
	bounded, cancel := context.WithTimeout(ctx, revertRetrySecs*h.unit())
	defer cancel()
	return firewallLock(bounded, cmp.Or(h.O.Firewall, filepath.Dir(rules)))
}

// sshSetRules is the rule file `ssh-allow set` writes for sources.
//
// A source that does not parse is an error, never a file without it: a file of no rule shuts SSH
// out for every source.
func (h *Hostd) sshSetRules(sources []string) (string, error) {
	ps, err := parseSSHSources("SSH's sources", sources)
	if err == nil && len(ps) == 0 {
		err = errors.New("SSH's sources: none")
	}
	if err != nil {
		return "", err
	}
	return sshRules(fmt.Sprintf("SSH, from the sources `%s ssh-allow set` gave.", h.R.Ctl()), ps), nil
}

// sshApplySources writes and loads the rule file of sources (sshSetRules, sshApply).
func (h *Hostd) sshApplySources(ctx context.Context, sources []string) error {
	text, err := h.sshSetRules(sources)
	if err != nil {
		return err
	}
	return h.sshApply(ctx, text)
}

// sshRevert undoes the change id (any, when empty) awaiting confirmation: the rule file before it
// back, then its record; a change no longer awaiting is left.
func (h *Hostd) sshRevert(id string) func(context.Context) error {
	return func(ctx context.Context) error {
		st, _, err := h.sshRead()
		if err != nil {
			return err
		}
		if st.Pending == nil || id != "" && st.Pending.ID != id {
			return errNothingToUndo
		}
		// Sources recorded before it: written from them (the file read at the change may be one an
		// earlier change that failed left); none: the file before it, or no file.
		put := func() error { return h.sshPut(ctx, st.Pending.PreviousRules, st.Pending.PreviousAbsent) }
		if len(st.Pending.Previous) > 0 {
			put = func() error { return h.sshApplySources(ctx, st.Pending.Previous) }
		}
		if err := put(); err != nil {
			return err
		}
		return h.sshWrite(sshState{Sources: st.Pending.Previous})
	}
}

// errNothingToUndo is a revert that found no change of its own awaiting confirmation: undo logs it
// as such, never as a change undone.
var errNothingToUndo = errors.New("no change of its own awaits confirmation any more: nothing undone")

// sshResume, as the daemon starts (under netMu): a change still awaiting confirmation is undone,
// and a rule file that differs from the record is written from it again.
func (h *Hostd) sshResume() {
	if h.Docker {
		return
	}
	st, set, err := h.sshRead()
	switch {
	case err != nil:
		h.O.logf("SSH's sources: %v", err)
		return
	case st.Pending != nil:
		_ = h.undo(sshOwner, 1, restartedUnconfirmed, h.sshRevert(st.Pending.ID))
		return
	case !set:
		return
	}
	_, rules := h.sshPaths()
	want, err := h.sshSetRules(st.Sources)
	if err != nil {
		h.O.logf("SSH's record: %v", err)
		return
	}
	if got, _ := os.ReadFile(rules); string(got) != want {
		if err := h.sshApply(h.ctx, want); err != nil {
			h.O.logf("SSH's rule file differs from its record, and applying the record again failed: %v", err)
			return
		}
		h.O.logf("SSH's rule file differed from its record: the recorded sources are applied again")
	}
}

// sshAnswer is the answer on SSH's sources as they stand.
func (h *Hostd) sshAnswer(msg string) (SSHAllow, error) {
	st, set, err := h.sshRead()
	if err != nil {
		return SSHAllow{}, err
	}
	a := SSHAllow{Message: msg}
	_, rules := h.sshPaths()
	raw, err := os.ReadFile(rules)
	absent := errors.Is(err, os.ErrNotExist)
	if err != nil && !absent {
		return a, err
	}
	sources, ok := readSSHRules(string(raw))
	switch {
	case absent && !set:
		a.From, a.Sources, a.Rules = "image", []string{}, fmt.Sprintf("%s is absent: no rule lets SSH in", rules)
	case ok && len(sources) == 0 && !set:
		a.From, a.Sources, a.Rules = "image", []string{}, fmt.Sprintf("%s holds no rule: no source lets SSH in", rules)
	case set && (!ok || !slices.Equal(sources, st.Sources)):
		// What the firewall holds, not the record: a load that failed, or a boot's unit that did not run.
		a.From, a.Sources = "set", sources
		if ok && sources == nil {
			a.Sources = []string{} // read, and holding no rule: no source
		}
		fix := fmt.Sprintf("`%s ssh-allow set %s` writes them again", h.R.Ctl(), strings.Join(st.Sources, ","))
		if st.Pending != nil {
			// set is refused while a change awaits confirmation: revert ends it first.
			fix = fmt.Sprintf("`%s ssh-allow revert` undoes the change awaiting confirmation", h.R.Ctl())
		}
		a.Rules = fmt.Sprintf("%s does not hold the recorded sources (%s): %s", rules, strings.Join(st.Sources, ","), fix)
	case set:
		a.From, a.Sources = "set", st.Sources
	case strings.Contains(string(raw), "`"+h.R.Ctl()+" ssh-allow set`"):
		// The record is gone, the file not rewritten yet: the cloud-config's come back at the next boot.
		a.From, a.Sources = "set", sources
		a.Rules = fmt.Sprintf("%s is gone: the cloud-config's sources (or the image's rule) come back at the next boot; `%s ssh-allow set` records sources again", SSHAllowFile(h.O.Dir), h.R.Ctl())
	case !ok:
		// The cloud-config and the image write SSH's sources alone: a file edited by hand.
		a.From, a.Rules = "unknown", fmt.Sprintf("%s holds lines other than SSH's sources", rules)
	case strings.Contains(string(raw), "saddr"):
		a.From, a.Sources = "cloud-config", sources
	default:
		a.From, a.Sources = "image", sources
	}
	if p := st.Pending; p != nil {
		a.Pending = &SSHPending{Previous: p.Previous, Deadline: p.Deadline, SecsLeft: max(int(time.Until(p.Deadline)/h.unit()), 0)}
		if a.Pending.Previous == nil {
			// Read from the rule file before it: none when it held no rule, unknown (nil) when it
			// held other lines.
			if prev, ok := readSSHRules(p.PreviousRules); ok {
				a.Pending.Previous = append([]string{}, prev...)
			}
		}
		// Ended exactly when a confirmation is refused (confirmSSHAllow): its undo's timer fired
		// (under way, tried again, or given up), or none is armed past the deadline.
		if r := h.netReverts[sshOwner]; r != nil && r.fired.Load() || r == nil && time.Now().After(p.Deadline) {
			a.Pending.Ended = true
			a.Pending.EndedBy = r.endedBy()
		}
	}
	return a, nil
}

// noSSHOnDocker refuses the SSH routes on a Docker install, whose host's SSH is not Stowage's.
func (h *Hostd) noSSHOnDocker() error {
	if h.Docker {
		return refuseAs(http.StatusNotFound, "", "SSH's sources are the Kairos VM's: a Docker install's host keeps its own firewall")
	}
	return nil
}

func (h *Hostd) sshAllow(w http.ResponseWriter, r *http.Request) {
	if err := h.noSSHOnDocker(); err != nil {
		replyErr(w, err)
		return
	}
	h.netMu.Lock()
	defer h.netMu.Unlock()
	a, err := h.sshAnswer("")
	if err != nil {
		replyErr(w, err)
		return
	}
	reply(w, http.StatusOK, a)
}

func (h *Hostd) setSSHAllow(w http.ResponseWriter, r *http.Request) {
	if err := h.noSSHOnDocker(); err != nil {
		replyErr(w, err)
		return
	}
	var req SSHRequest
	if err := decodeBody(r, &req); err != nil {
		replyErr(w, err)
		return
	}
	ps, err := parseSSHSources("sources", req.Sources)
	if err == nil && len(ps) == 0 {
		err = errors.New("sources: none given: SSH would accept no source, and no one could log in (0.0.0.0/0,::/0 is every source)")
	}
	if err != nil {
		replyErr(w, refuseRequest(err))
		return
	}
	secs := defaultRevertSecs
	if req.RevertSecs != nil {
		secs = *req.RevertSecs
	}
	if secs < minRevertSecs || secs > maxRevertSecs {
		// Every change of who may log in is confirmed over a new connection: none is applied for good
		// at once.
		err = fmt.Errorf("revert_secs %d: every change of SSH's sources has a window, %d to %d s", secs, minRevertSecs, maxRevertSecs)
	}
	if err != nil {
		replyErr(w, refuseRequest(err))
		return
	}
	next := make([]string, 0, len(ps))
	for _, p := range ps {
		if s := p.String(); !slices.Contains(next, s) {
			next = append(next, s)
		}
	}
	h.netMu.Lock()
	defer h.netMu.Unlock()
	if err := h.noReboot(); err != nil {
		replyErr(w, err)
		return
	}
	cur, set, err := h.sshRead()
	if err != nil {
		replyErr(w, err)
		return
	}
	if cur.Pending != nil {
		replyErr(w, refuseAs(http.StatusConflict, fmt.Sprintf("`%s ssh-allow confirm` keeps it, `%s ssh-allow revert` undoes it", h.R.Ctl(), h.R.Ctl()), "a change of SSH's sources awaits confirmation"))
		return
	}
	// A change that could not be applied whose restore is tried again (or gave up): its restore runs
	// first, so the rule file this change starts from is the one in force; one that fails refuses.
	if a := h.netReverts[sshOwner]; a != nil && a.undo != nil {
		h.netDisarm(sshOwner)
		// h.undo logs it, counts nothing to undo as done, and arms a retry when it fails.
		if err := h.undo(sshOwner, 1, a.why, a.undo); err != nil {
			replyErr(w, refuseAs(http.StatusConflict, "wait for it, or fix what fails, then set again", "an earlier change that could not be applied is still being restored: %v", err))
			return
		}
	}
	_, rules := h.sshPaths()
	before, err := os.ReadFile(rules)
	beforeAbsent := errors.Is(err, os.ErrNotExist)
	if err != nil && !beforeAbsent {
		replyErr(w, err)
		return
	}
	var previous []string
	if set {
		previous = cur.Sources
	}
	st := sshState{Sources: next, Pending: &sshPending{ID: rand.Text(), Deadline: time.Now().Add(time.Duration(secs) * h.unit()), Previous: previous, PreviousRules: string(before), PreviousAbsent: beforeAbsent}}
	// Recorded first: a daemon stopped before the rule file is written undoes or re-applies it at
	// its start (sshResume).
	if err := h.sshWrite(st); err != nil {
		replyErr(w, err)
		return
	}
	// The daemon's context: a client cut by the change itself does not make it fail.
	if err := h.sshApplySources(h.ctx, next); err != nil {
		// sshApply wrote the file before it back, and the firewall kept its ruleset: the file is put
		// back only when that failed, then the record goes back. Until the file is back, the record
		// keeps the change, pending: a restart or a boot undoes it, never applies it.
		restore := func(ctx context.Context) error {
			if got, err := os.ReadFile(rules); !(beforeAbsent && errors.Is(err, os.ErrNotExist) || err == nil && !beforeAbsent && bytes.Equal(got, before)) {
				if err := h.sshPut(ctx, string(before), beforeAbsent); err != nil {
					return err
				}
			}
			return h.sshWrite(sshState{Sources: previous})
		}
		if uerr := h.undo(sshOwner, 1, notApplied, restore); uerr != nil {
			replyErr(w, fmt.Errorf("%w; restoring the sources before it failed too, and is tried again every %d s: %v", err, revertRetrySecs, uerr))
			return
		}
		replyErr(w, fmt.Errorf("%w: the sources before it are back", err))
		return
	}
	// The window counts from the load: the firewall's lock may have been waited for.
	st.Pending.Deadline = time.Now().Add(time.Duration(secs) * h.unit())
	if err := h.sshWrite(st); err != nil {
		h.O.logf("the change's deadline, counted from its load, is not recorded (the one before stands): %v", err)
	}
	h.armRevert(sshOwner, st.Pending.Deadline, 1, notConfirmed, h.sshRevert(st.Pending.ID))
	msg := fmt.Sprintf("applied: `%s ssh-allow confirm`, run over a new SSH connection (not a shared one: `ssh -S none`), keeps it within %d s, else the sources before it come back", h.R.Ctl(), secs)
	a, err := h.sshAnswer(msg)
	if err != nil {
		// Applied, its window armed: the answer says so, whatever could not be read after.
		h.O.logf("SSH's sources, after the change: %v", err)
		a = SSHAllow{Message: msg, From: "set", Sources: next}
	}
	reply(w, http.StatusOK, a)
}

func (h *Hostd) confirmSSHAllow(w http.ResponseWriter, r *http.Request) {
	if err := h.noSSHOnDocker(); err != nil {
		replyErr(w, err)
		return
	}
	h.netMu.Lock()
	defer h.netMu.Unlock()
	st, _, err := h.sshRead()
	if err != nil {
		replyErr(w, err)
		return
	}
	if st.Pending == nil {
		replyErr(w, fail(http.StatusConflict, "no change of SSH's sources awaits confirmation: one not confirmed in its window, or before a restart or a reboot, is undone (`%s ssh-allow` shows the sources)", h.R.Ctl()))
		return
	}
	// Ended: its timer fired, or none is armed past the deadline (an undo the daemon's stop cut, which
	// its start does).
	if a := h.netReverts[sshOwner]; a != nil && a.fired.Load() || a == nil && time.Now().After(st.Pending.Deadline) {
		ended, why := a.endedBy(), notConfirmed
		if a != nil {
			why = a.why // a retry keeps what it undoes for
		}
		h.netDisarm(sshOwner)
		if err := h.undo(sshOwner, 1, why, h.sshRevert(st.Pending.ID)); err != nil {
			replyErr(w, fail(http.StatusConflict, "%s: undoing it failed, and is tried again every %d s: %v", ended, revertRetrySecs, err))
			return
		}
		replyErr(w, fail(http.StatusConflict, "%s: the sources before it are back", ended))
		return
	}
	// Recorded first: a confirmation that is not recorded leaves the window armed.
	st.Pending = nil
	if err := h.sshWrite(st); err != nil {
		replyErr(w, err)
		return
	}
	h.netDisarm(sshOwner)
	a, err := h.sshAnswer("confirmed")
	if err != nil {
		replyErr(w, err)
		return
	}
	reply(w, http.StatusOK, a)
}

func (h *Hostd) revertSSHAllow(w http.ResponseWriter, r *http.Request) {
	if err := h.noSSHOnDocker(); err != nil {
		replyErr(w, err)
		return
	}
	h.netMu.Lock()
	defer h.netMu.Unlock()
	st, _, err := h.sshRead()
	if err != nil {
		replyErr(w, err)
		return
	}
	if st.Pending == nil {
		replyErr(w, fail(http.StatusConflict, "no change of SSH's sources awaits confirmation: `%s ssh-allow set` changes them", h.R.Ctl()))
		return
	}
	h.netDisarm(sshOwner)
	if err := h.undo(sshOwner, 1, revertAsked, h.sshRevert(st.Pending.ID)); err != nil {
		replyErr(w, fmt.Errorf("%w (tried again every %d s)", err, revertRetrySecs))
		return
	}
	a, err := h.sshAnswer("reverted")
	if err != nil {
		replyErr(w, err)
		return
	}
	reply(w, http.StatusOK, a)
}

// sshBootCmd writes SSH's recorded sources at boot, after the cloud-config's stage.
const sshBootCmd = "ssh-boot"

// sshBoot, run at boot as root after the cloud-config's stage (which writes the seed's sources, or
// leaves the image's rule, /etc/nftables.d being not kept): a change still awaiting confirmation
// when the VM stopped is undone, as the daemon's start would; else the recorded sources are written
// and loaded. A record that does not read fails the unit, the seed's sources (or the image's) left.
func (h *Hostd) sshBoot(ctx context.Context) error {
	st, set, err := h.sshRead()
	switch {
	case err != nil:
		return fmt.Errorf("SSH's sources: %w", err)
	case st.Pending != nil && len(st.Pending.Previous) == 0:
		// No sources were recorded before the change: the rule file this boot's cloud-config wrote,
		// or the image's, stands; the record goes.
		h.O.logf("the change of SSH's sources awaiting confirmation is undone: the VM restarted inside its window")
		return h.sshWrite(sshState{})
	case st.Pending != nil:
		// The sources recorded before it, written for this boot (the rule file of the boot it was
		// made in is not this one's).
		h.O.logf("the change of SSH's sources awaiting confirmation is undone: the VM restarted inside its window")
		if err := h.sshApplySources(ctx, st.Pending.Previous); err != nil {
			return err
		}
		return h.sshWrite(sshState{Sources: st.Pending.Previous})
	case !set:
		return nil
	}
	return h.sshApplySources(ctx, st.Sources)
}

// sshBootCommand is the hidden verb the boot unit runs: sshBoot over the install of r's.
func sshBootCommand(r *Release, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:    sshBootCmd,
		Short:  "Write SSH's sources `ssh-allow set` recorded, over the cloud-config's (run at boot)",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h := &Hostd{R: r, O: Options{Dir: VMDir(r.Spec.Name), Firewall: PortRules, Log: stderr}, Host: HostTool{}}
			return h.sshBoot(cmd.Context())
		},
	}
}
