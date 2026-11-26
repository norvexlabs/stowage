package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// sshNft is the VM's nft as `ssh-allow` runs it: each `nft -f` records SSH's rule file as it then
// stands, the first fails of them failing.
type sshNft struct {
	mu     sync.Mutex
	rules  string
	loaded []string
	fails  int
	// unlocked counts the loads made without the firewall's lock, beside the rule file.
	unlocked int
	// afterLoad, when set, runs after each load that succeeds.
	afterLoad func()
}

func (n *sshNft) Run(_ context.Context, c Cmd) ([]byte, error) {
	if !slices.Equal(c.Args, []string{"nft", "-f", FirewallConf}) {
		return nil, errors.New("unexpected " + strings.Join(c.Args, " "))
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if !firewallHeld(filepath.Dir(n.rules)) {
		n.unlocked++
	}
	if n.fails > 0 {
		n.fails--
		return nil, errors.New("nft: syntax error")
	}
	raw, err := os.ReadFile(n.rules)
	if errors.Is(err, os.ErrNotExist) {
		raw = []byte("<absent>") // the include's glob matches nothing
	} else if err != nil {
		return nil, err
	}
	n.loaded = append(n.loaded, string(raw))
	if n.afterLoad != nil {
		n.afterLoad()
	}
	return nil, nil
}

func (n *sshNft) last() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.loaded) == 0 {
		return ""
	}
	return n.loaded[len(n.loaded)-1]
}

const seedRules = "# SSH, from the sources `notes-stow vm create --ssh-allow` gave.\nip saddr 192.0.2.0/24 tcp dport 22 accept\n"

// sshDaemon is a host daemon whose SSH rule file holds rules (as the cloud-config or the image
// wrote it), its nft nft.
func sshDaemon(t *testing.T, rules string) (*Hostd, *sshNft) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "ssh.nft")
	if err := os.WriteFile(file, []byte(rules), 0o644); err != nil {
		t.Fatal(err)
	}
	nft := &sshNft{rules: file}
	t.Cleanup(func() {
		// Under its lock: a served daemon's start (sshResume) may still load through it.
		nft.mu.Lock()
		defer nft.mu.Unlock()
		if nft.unlocked > 0 {
			t.Errorf("%d loads without the firewall's lock", nft.unlocked)
		}
	})
	h := &Hostd{R: testRelease(t, "1.0.0"), O: Options{Dir: t.TempDir(), Log: io.Discard}, Host: nft, Unit: time.Millisecond, SSHRules: file, ctx: context.Background()}
	return h, nft
}

// freshSSH empties a before an answer is decoded into it.
func freshSSH(a *SSHAllow) *SSHAllow {
	*a = SSHAllow{}
	return a
}

func sshRulesNow(t *testing.T, h *Hostd) string {
	t.Helper()
	_, file := h.sshPaths()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestSSHAllowSetThenConfirm(t *testing.T) {
	t.Parallel()
	h, nft := sshDaemon(t, seedRules)
	timers := manualTimers(h)
	var a SSHAllow
	if code := call(t, h.Handler(), http.MethodGet, "/v1/ssh-allow", nil, freshSSH(&a)); code != http.StatusOK || a.From != "cloud-config" || !slices.Equal(a.Sources, []string{"192.0.2.0/24"}) {
		t.Fatalf("%d %+v", code, a)
	}
	// Sources as given: unmasked, repeated, spaced.
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.7/24", " 2001:db8::1/64", "198.51.100.0/24"}, RevertSecs: intp(minRevertSecs)}, freshSSH(&a)); code != http.StatusOK {
		t.Fatalf("%d %+v", code, a)
	}
	want := "# SSH, from the sources `notes-stow ssh-allow set` gave.\nip saddr 198.51.100.0/24 tcp dport 22 accept\nip6 saddr 2001:db8::/64 tcp dport 22 accept\n"
	if got := nft.last(); got != want {
		t.Fatalf("loaded %q", got)
	}
	if a.From != "set" || a.Pending == nil || !slices.Equal(a.Pending.Previous, []string{"192.0.2.0/24"}) || !strings.Contains(a.Message, "ssh-allow confirm") {
		t.Fatalf("%+v", a)
	}
	// A second change waits for the first's confirmation.
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"203.0.113.0/24"}}, nil); code != http.StatusConflict {
		t.Fatalf("a change over a pending one: %d", code)
	}
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow/confirm", nil, freshSSH(&a)); code != http.StatusOK || a.Pending != nil || a.Message != "confirmed" {
		t.Fatalf("%d %+v", code, a)
	}
	if st, set, err := h.sshRead(); err != nil || !set || st.Pending != nil || !slices.Equal(st.Sources, []string{"198.51.100.0/24", "2001:db8::/64"}) {
		t.Fatalf("%+v %v %v", st, set, err)
	}
	// Confirmed: the window's timer is disarmed, so nothing fires.
	timers.mu.Lock()
	for _, at := range timers.armed {
		if !at.stopped.Load() {
			t.Error("a timer still armed after the confirmation")
		}
	}
	timers.mu.Unlock()
	if sshRulesNow(t, h) != want {
		t.Error("the rule file changed after the confirmation")
	}
}

func TestSSHAllowWindowEndsBackToTheSeeds(t *testing.T) {
	t.Parallel()
	h, nft := sshDaemon(t, seedRules)
	timers := manualTimers(h)
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}, RevertSecs: intp(minRevertSecs)}, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	<-timers.fire(t)
	if got := nft.last(); got != seedRules {
		t.Fatalf("loaded %q", got)
	}
	// The record goes: the cloud-config's stage writes the seed's sources at the next boot again.
	if _, err := os.Stat(SSHAllowFile(h.O.Dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the record: %v", err)
	}
	var a SSHAllow
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow/confirm", nil, freshSSH(&a)); code != http.StatusConflict {
		t.Fatalf("a confirmation after the window: %d", code)
	}
}

func TestSSHAllowConfirmTooLateUndoes(t *testing.T) {
	t.Parallel()
	h, _ := sshDaemon(t, seedRules)
	manualTimers(h)
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}, RevertSecs: intp(minRevertSecs)}, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	// The window ended, its timer waiting for the lock the confirmation holds.
	h.netReverts[sshOwner].fired.Store(true)
	var e map[string]any
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow/confirm", nil, &e); code != http.StatusConflict || !strings.Contains(e["error"].(string), "the sources before it are back") {
		t.Fatalf("%d %v", code, e)
	}
	if sshRulesNow(t, h) != seedRules {
		t.Error("not undone")
	}
}

func TestSSHAllowRevertBackToTheEarlierSet(t *testing.T) {
	t.Parallel()
	h, nft := sshDaemon(t, "# SSH, open to all.\ntcp dport 22 accept\n")
	manualTimers(h)
	var a SSHAllow
	if code := call(t, h.Handler(), http.MethodGet, "/v1/ssh-allow", nil, freshSSH(&a)); code != http.StatusOK || a.From != "image" || !slices.Equal(a.Sources, []string{"0.0.0.0/0", "::/0"}) {
		t.Fatalf("%d %+v", code, a)
	}
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}}, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow/confirm", nil, freshSSH(&a)); code != http.StatusOK || a.Pending != nil {
		t.Fatalf("%d %+v", code, a)
	}
	first := sshRulesNow(t, h)
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"203.0.113.0/24"}}, freshSSH(&a)); code != http.StatusOK || a.Pending == nil || !slices.Equal(a.Pending.Previous, []string{"198.51.100.0/24"}) {
		t.Fatalf("%d %+v", code, a)
	}
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow/revert", nil, freshSSH(&a)); code != http.StatusOK || a.Message != "reverted" || a.From != "set" || !slices.Equal(a.Sources, []string{"198.51.100.0/24"}) {
		t.Fatalf("%d %+v", code, a)
	}
	if nft.last() != first {
		t.Errorf("loaded %q", nft.last())
	}
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow/revert", nil, nil); code != http.StatusConflict {
		t.Errorf("a revert with nothing pending: %d", code)
	}
}

func TestSSHAllowRefusals(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		req  SSHRequest
		want string
	}{
		{"none", SSHRequest{}, "none given"},
		{"no prefix", SSHRequest{Sources: []string{"198.51.100.0/24", "198.51.100.7"}}, `"198.51.100.7" is not a source with its prefix`},
		{"window", SSHRequest{Sources: []string{"198.51.100.0/24"}, RevertSecs: intp(minRevertSecs - 1)}, "revert_secs"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, nft := sshDaemon(t, seedRules)
			var e map[string]any
			if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", c.req, &e); code != http.StatusBadRequest || !strings.Contains(e["error"].(string), c.want) || e["refused"] != true {
				t.Fatalf("%d %v", code, e)
			}
			if len(nft.loaded) != 0 || sshRulesNow(t, h) != seedRules {
				t.Error("a refusal changed the firewall")
			}
		})
	}
	t.Run("docker", func(t *testing.T) {
		t.Parallel()
		h, _ := sshDaemon(t, seedRules)
		h.Docker = true
		for _, p := range []string{"/v1/ssh-allow", "/v1/ssh-allow/confirm", "/v1/ssh-allow/revert"} {
			if code := call(t, h.Handler(), http.MethodPost, p, SSHRequest{Sources: []string{"198.51.100.0/24"}}, nil); code != http.StatusNotFound {
				t.Errorf("%s on Docker: %d", p, code)
			}
		}
		if code := call(t, h.Handler(), http.MethodGet, "/v1/ssh-allow", nil, nil); code != http.StatusNotFound {
			t.Errorf("GET on Docker: %d", code)
		}
	})
	t.Run("app", func(t *testing.T) {
		t.Parallel()
		h, _ := sshDaemon(t, seedRules)
		app := h.routes(true)
		if code := call(t, app, http.MethodGet, "/v1/ssh-allow", nil, nil); code != http.StatusOK {
			t.Errorf("the app reads the sources: %d", code)
		}
		for _, p := range []string{"/v1/ssh-allow", "/v1/ssh-allow/confirm", "/v1/ssh-allow/revert"} {
			if code := call(t, app, http.MethodPost, p, SSHRequest{Sources: []string{"0.0.0.0/0"}}, nil); code != http.StatusForbidden {
				t.Errorf("the app's %s: %d", p, code)
			}
		}
	})
	t.Run("reboot", func(t *testing.T) {
		t.Parallel()
		h, _ := sshDaemon(t, seedRules)
		h.rebooting = true
		if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}}, nil); code != http.StatusConflict {
			t.Errorf("during a reboot: %d", code)
		}
	})
}

func TestSSHAllowFailedApplyRestores(t *testing.T) {
	t.Parallel()
	h, nft := sshDaemon(t, seedRules)
	manualTimers(h)
	nft.fails = 1
	var e map[string]any
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}}, &e); code != http.StatusInternalServerError || !strings.Contains(e["error"].(string), "the sources before it are back") {
		t.Fatalf("%d %v", code, e)
	}
	// The firewall kept its ruleset (nft -f is atomic): the rule file before it is written back,
	// and nothing loads again.
	if len(nft.loaded) != 0 || sshRulesNow(t, h) != seedRules {
		t.Errorf("loaded %q, rules %q", nft.loaded, sshRulesNow(t, h))
	}
	if _, err := os.Stat(SSHAllowFile(h.O.Dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the record: %v", err)
	}
	if h.netReverts[sshOwner] != nil {
		t.Error("a window still armed")
	}
}

func TestSSHAllowDaemonStart(t *testing.T) {
	t.Parallel()
	t.Run("pending undone", func(t *testing.T) {
		t.Parallel()
		h, _ := sshDaemon(t, seedRules)
		manualTimers(h)
		if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}}, nil); code != http.StatusOK {
			t.Fatal(code)
		}
		h.netReverts = nil // the daemon restarted
		h.netMu.Lock()
		h.sshResume()
		h.netMu.Unlock()
		if sshRulesNow(t, h) != seedRules {
			t.Error("not undone")
		}
	})
	t.Run("record applied again", func(t *testing.T) {
		t.Parallel()
		h, nft := sshDaemon(t, seedRules)
		// Recorded, but the daemon stopped before the rule file was written.
		if err := h.sshWrite(sshState{Sources: []string{"198.51.100.0/24"}}); err != nil {
			t.Fatal(err)
		}
		h.sshResume()
		want := rulesOf(t, h, []string{"198.51.100.0/24"})
		if sshRulesNow(t, h) != want || nft.last() != want {
			t.Fatalf("rules %q", sshRulesNow(t, h))
		}
		// Written already, it is left alone: no reload.
		n := len(nft.loaded)
		h.sshResume()
		if len(nft.loaded) != n {
			t.Error("reloaded")
		}
	})
	t.Run("record that does not read", func(t *testing.T) {
		t.Parallel()
		h, nft := sshDaemon(t, seedRules)
		var log bytes.Buffer
		h.O.Log = &log
		if err := os.WriteFile(SSHAllowFile(h.O.Dir), []byte("sources: [198.51.100.7]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		h.sshResume()
		if len(nft.loaded) != 0 || !strings.Contains(log.String(), "is not a source with its prefix") {
			t.Fatalf("%v %s", nft.loaded, log.String())
		}
		var e map[string]any
		if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}}, &e); code == http.StatusOK || !strings.Contains(e["error"].(string), "is not a source") {
			t.Fatalf("%d %v", code, e)
		}
	})
}

func TestReadSSHRules(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		text string
		want []string
		ok   bool
	}{
		{"# open\ntcp dport 22 accept\n", []string{"0.0.0.0/0", "::/0"}, true},
		{seedRules, []string{"192.0.2.0/24"}, true},
		{"\nip6 saddr 2001:db8::/32 tcp dport 22 accept\n", []string{"2001:db8::/32"}, true},
		{"", nil, true},
		{"ip saddr 2001:db8::/32 tcp dport 22 accept\n", nil, false},
		{"ip saddr 192.0.2.0/24 tcp dport 2222 accept\n", nil, false},
		{"ip saddr { 192.0.2.0/24 } tcp dport 22 accept\n", nil, false},
	} {
		got, ok := readSSHRules(c.text)
		if ok != c.ok || !slices.Equal(got, c.want) {
			t.Errorf("%q: %v %v", c.text, got, ok)
		}
	}
}

func TestSSHSourceWarning(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		conn    string
		sources []string
		want    string
	}{
		{"198.51.100.7 51000 192.0.2.10 22", []string{"198.51.100.0/24"}, ""},
		{"::ffff:198.51.100.7 51000 192.0.2.10 22", []string{"198.51.100.0/24"}, ""},
		{"", []string{"198.51.100.0/24"}, ""},
		{"fe80::5%enp1s0 51000 fe80::1%enp1s0 22", []string{"fe80::/10"}, ""},
		{"198.51.100.7 51000 192.0.2.10 22", []string{"203.0.113.0/24"}, "this session comes from 198.51.100.7, which none of the new sources holds: no new SSH connection from there will be let in; confirm it from a source they hold, or let the window undo it (`notes-stow ssh-allow revert` now)\n"},
	} {
		var b bytes.Buffer
		sshSourceWarning(&b, c.conn, c.sources, "notes-stow")
		if c.want == "" && b.Len() != 0 || !strings.Contains(b.String(), c.want) {
			t.Errorf("%q %v: %q", c.conn, c.sources, b.String())
		}
	}
}

func TestSSHAllowOverTheSocket(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	sh, nft := sshDaemon(t, seedRules)
	h := daemon(e, &fakeHost{})
	h.Host, h.SSHRules = nft, sh.SSHRules
	sock := served(t, h)
	out, err := ctl(e, sock, "ssh-allow")
	if err != nil || !strings.Contains(out, "sources    192.0.2.0/24\n") || !strings.Contains(out, "set by     the cloud-config (vm create --ssh-allow)\n") {
		t.Fatalf("%v\n%s", err, out)
	}
	// Repeated and comma-separated sources alike.
	out, err = ctl(e, sock, "ssh-allow", "set", "198.51.100.0/24,2001:db8::/32", "203.0.113.0/24", "--revert-secs", "3600")
	if err != nil || !strings.Contains(out, "keeps it within 3600 s") || !strings.Contains(out, "sources    198.51.100.0/24, 2001:db8::/32, 203.0.113.0/24\n") || !strings.Contains(out, "else back to 192.0.2.0/24\n") {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err = ctl(e, sock, "ssh-allow", "confirm", "--json"); err != nil || !strings.Contains(out, `"message": "confirmed"`) || !strings.Contains(out, `"from": "set"`) || strings.Contains(out, "pending") {
		t.Fatalf("%v %s", err, out)
	}
	// Every change has a window: 0 is refused.
	if _, err = ctl(e, sock, "ssh-allow", "set", "198.51.100.0/24", "--revert-secs", "0"); err == nil || !strings.Contains(err.Error(), "every change of SSH's sources has a window") {
		t.Errorf("%v", err)
	}
	if _, err = ctl(e, sock, "ssh-allow", "set"); err == nil {
		t.Error("set with no source")
	}
	if _, err = ctl(e, sock, "ssh-allow", "set", "198.51.100.7"); err == nil || !strings.Contains(err.Error(), `"198.51.100.7" is not a source with its prefix`) {
		t.Errorf("%v", err)
	}
}

func TestTheDaemonStartingUndoesAPendingSSHChange(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	sh, nft := sshDaemon(t, seedRules)
	h := daemon(e, &fakeHost{})
	h.Host, h.SSHRules = nft, sh.SSHRules
	// A change awaiting confirmation when the daemon stopped (a crash, a reboot inside its window).
	set := rulesOf(t, h, []string{"198.51.100.0/24"})
	if err := os.WriteFile(h.SSHRules, []byte(set), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.sshWrite(sshState{Sources: []string{"198.51.100.0/24"}, Pending: &sshPending{ID: "x", Deadline: time.Now().Add(time.Hour), PreviousRules: seedRules}}); err != nil {
		t.Fatal(err)
	}
	served(t, h)
	eventually(t, "the change undone at the daemon's start", func() bool {
		raw, _ := os.ReadFile(h.SSHRules)
		return string(raw) == seedRules
	})
}

// The revert at the window's end failed once, its retry armed: a confirmation meanwhile comes too
// late, and undoes the change.
func TestSSHAllowConfirmAfterAFailedRevertIsTooLate(t *testing.T) {
	t.Parallel()
	h, nft := sshDaemon(t, seedRules)
	timers := manualTimers(h)
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}, RevertSecs: intp(minRevertSecs)}, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	nft.mu.Lock()
	nft.fails = 1
	nft.mu.Unlock()
	<-timers.fire(t)
	var e map[string]any
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow/confirm", nil, &e); code != http.StatusConflict || !strings.Contains(e["error"].(string), "the sources before it are back") {
		t.Fatalf("%d %v", code, e)
	}
	if sshRulesNow(t, h) != seedRules {
		t.Error("not undone")
	}
}

func TestSSHAllowWithoutARuleFile(t *testing.T) {
	t.Parallel()
	h, _ := sshDaemon(t, "")
	if err := os.Remove(h.SSHRules); err != nil {
		t.Fatal(err)
	}
	var a SSHAllow
	if code := call(t, h.Handler(), http.MethodGet, "/v1/ssh-allow", nil, &a); code != http.StatusOK || a.Sources == nil || len(a.Sources) != 0 || !strings.Contains(a.Rules, "is absent: no rule lets SSH in") {
		t.Fatalf("%d %+v", code, a)
	}
	var b bytes.Buffer
	printSSHAllow(&b, a)
	if !strings.Contains(b.String(), "sources    none: ") {
		t.Errorf("%s", b.String())
	}
}

func TestSSHBoot(t *testing.T) {
	t.Parallel()
	t.Run("no record", func(t *testing.T) {
		t.Parallel()
		h, nft := sshDaemon(t, seedRules)
		if err := h.sshBoot(context.Background()); err != nil || len(nft.loaded) != 0 || sshRulesNow(t, h) != seedRules {
			t.Fatalf("%v %v", err, nft.loaded)
		}
	})
	t.Run("recorded sources over the seed's", func(t *testing.T) {
		t.Parallel()
		h, nft := sshDaemon(t, seedRules)
		if err := h.sshWrite(sshState{Sources: []string{"198.51.100.0/24"}}); err != nil {
			t.Fatal(err)
		}
		want := rulesOf(t, h, []string{"198.51.100.0/24"})
		if err := h.sshBoot(context.Background()); err != nil || sshRulesNow(t, h) != want || nft.last() != want {
			t.Fatalf("%v %q", err, sshRulesNow(t, h))
		}
	})
	t.Run("pending from no record: this boot's rule file stands", func(t *testing.T) {
		t.Parallel()
		h, nft := sshDaemon(t, seedRules)
		if err := h.sshWrite(sshState{Sources: []string{"198.51.100.0/24"}, Pending: &sshPending{ID: "x", Deadline: time.Now().Add(time.Hour), PreviousRules: "the last boot's\n", PreviousAbsent: true}}); err != nil {
			t.Fatal(err)
		}
		if err := h.sshBoot(context.Background()); err != nil || sshRulesNow(t, h) != seedRules || len(nft.loaded) != 0 {
			t.Fatalf("%v %q %v", err, sshRulesNow(t, h), nft.loaded)
		}
		if _, err := os.Stat(SSHAllowFile(h.O.Dir)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the record: %v", err)
		}
	})
	t.Run("pending undone", func(t *testing.T) {
		t.Parallel()
		h, _ := sshDaemon(t, seedRules)
		if err := h.sshWrite(sshState{Sources: []string{"198.51.100.0/24"}, Pending: &sshPending{ID: "x", Deadline: time.Now().Add(time.Hour), Previous: []string{"203.0.113.0/24"}, PreviousRules: "the last boot's\n"}}); err != nil {
			t.Fatal(err)
		}
		if err := h.sshBoot(context.Background()); err != nil || sshRulesNow(t, h) != rulesOf(t, h, []string{"203.0.113.0/24"}) {
			t.Fatalf("%v %q", err, sshRulesNow(t, h))
		}
		if st, set, err := h.sshRead(); err != nil || !set || st.Pending != nil || !slices.Equal(st.Sources, []string{"203.0.113.0/24"}) {
			t.Fatalf("%+v %v %v", st, set, err)
		}
	})
	t.Run("record that does not read", func(t *testing.T) {
		t.Parallel()
		h, nft := sshDaemon(t, seedRules)
		if err := os.WriteFile(SSHAllowFile(h.O.Dir), []byte("sources: []\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := h.sshBoot(context.Background()); err == nil || len(nft.loaded) != 0 || sshRulesNow(t, h) != seedRules {
			t.Fatalf("%v %v", err, nft.loaded)
		}
	})
	// The boot unit's condition names the record's path.
	if got := SSHAllowFile(VMDir("notes")); got != "/usr/local/notes/ssh-allow.yaml" {
		t.Errorf("record %s", got)
	}
}

func TestSSHResumeLogsAReapply(t *testing.T) {
	t.Parallel()
	h, _ := sshDaemon(t, seedRules)
	var log bytes.Buffer
	h.O.Log = &log
	if err := h.sshWrite(sshState{Sources: []string{"198.51.100.0/24"}}); err != nil {
		t.Fatal(err)
	}
	h.sshResume()
	if !strings.Contains(log.String(), "the recorded sources are applied again") || strings.Contains(log.String(), "undone") {
		t.Errorf("%s", log.String())
	}
}

// The revert gave up: a confirmation later still comes too late, and tries the undo again.
func TestSSHAllowConfirmAfterTheRevertGaveUp(t *testing.T) {
	t.Parallel()
	h, nft := sshDaemon(t, seedRules)
	timers := manualTimers(h)
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}, RevertSecs: intp(minRevertSecs)}, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	nft.mu.Lock()
	nft.fails = revertAttempts
	nft.mu.Unlock()
	for range revertAttempts {
		<-timers.fire(t)
	}
	var e map[string]any
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow/confirm", nil, &e); code != http.StatusConflict || !strings.Contains(e["error"].(string), "the sources before it are back") {
		t.Fatalf("%d %v", code, e)
	}
	if sshRulesNow(t, h) != seedRules {
		t.Error("not undone")
	}
}

func TestSSHAllowShowsWhatTheFirewallHolds(t *testing.T) {
	t.Parallel()
	t.Run("an empty rule file", func(t *testing.T) {
		t.Parallel()
		h, _ := sshDaemon(t, "")
		var a SSHAllow
		if code := call(t, h.Handler(), http.MethodGet, "/v1/ssh-allow", nil, &a); code != http.StatusOK || a.Sources == nil || len(a.Sources) != 0 || !strings.Contains(a.Rules, "holds no rule") {
			t.Fatalf("%d %+v", code, a)
		}
	})
	t.Run("a rule file other than the record", func(t *testing.T) {
		t.Parallel()
		h, _ := sshDaemon(t, seedRules)
		if err := h.sshWrite(sshState{Sources: []string{"198.51.100.0/24"}}); err != nil {
			t.Fatal(err)
		}
		var a SSHAllow
		if code := call(t, h.Handler(), http.MethodGet, "/v1/ssh-allow", nil, &a); code != http.StatusOK || a.From != "set" || !slices.Equal(a.Sources, []string{"192.0.2.0/24"}) ||
			!strings.Contains(a.Rules, "does not hold the recorded sources (198.51.100.0/24): `notes-stow ssh-allow set 198.51.100.0/24` writes them again") {
			t.Fatalf("%d %+v", code, a)
		}
	})
	t.Run("a change from no source", func(t *testing.T) {
		t.Parallel()
		h, _ := sshDaemon(t, "")
		manualTimers(h)
		var a SSHAllow
		if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}}, freshSSH(&a)); code != http.StatusOK || a.Pending == nil || a.Pending.Previous == nil || len(a.Pending.Previous) != 0 {
			t.Fatalf("%d %+v", code, a)
		}
		var b bytes.Buffer
		printSSHAllow(&b, a)
		if !strings.Contains(b.String(), "else back to no source\n") {
			t.Errorf("%s", b.String())
		}
	})
}

func TestSSHAllowShowsAWindowThatEnded(t *testing.T) {
	t.Parallel()
	h, nft := sshDaemon(t, seedRules)
	timers := manualTimers(h)
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}, RevertSecs: intp(minRevertSecs)}, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	nft.mu.Lock()
	nft.fails = 1
	nft.mu.Unlock()
	<-timers.fire(t)
	var a SSHAllow
	if code := call(t, h.Handler(), http.MethodGet, "/v1/ssh-allow", nil, &a); code != http.StatusOK || a.Pending == nil || !a.Pending.Ended {
		t.Fatalf("%d %+v", code, a)
	}
	var b bytes.Buffer
	printSSHAllow(&b, a)
	if !strings.Contains(b.String(), "pending    the change's window ended: the change is being undone (or its undo is tried again)") {
		t.Errorf("%s", b.String())
	}
}

func TestSSHAllowRecordReadMasked(t *testing.T) {
	t.Parallel()
	h, _ := sshDaemon(t, seedRules)
	if err := os.WriteFile(SSHAllowFile(h.O.Dir), []byte("sources: [10.0.0.1/8]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.sshResume()
	var a SSHAllow
	if code := call(t, h.Handler(), http.MethodGet, "/v1/ssh-allow", nil, &a); code != http.StatusOK || a.Rules != "" || !slices.Equal(a.Sources, []string{"10.0.0.0/8"}) {
		t.Fatalf("%d %+v", code, a)
	}
}

// A rule file other than the record while a change awaits confirmation: the hint is revert, as set
// is refused then.
func TestSSHAllowMismatchWhilePendingNamesRevert(t *testing.T) {
	t.Parallel()
	h, _ := sshDaemon(t, seedRules)
	if err := h.sshWrite(sshState{Sources: []string{"198.51.100.0/24"}, Pending: &sshPending{ID: "x", Deadline: time.Now().Add(time.Hour), PreviousRules: seedRules}}); err != nil {
		t.Fatal(err)
	}
	var a SSHAllow
	if code := call(t, h.Handler(), http.MethodGet, "/v1/ssh-allow", nil, &a); code != http.StatusOK || !strings.HasSuffix(a.Rules, "`notes-stow ssh-allow revert` undoes the change awaiting confirmation") {
		t.Fatalf("%d %+v", code, a)
	}
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow/revert", nil, nil); code != http.StatusOK || sshRulesNow(t, h) != seedRules {
		t.Fatalf("the hint's revert: %d", code)
	}
}

// A load that fails at boot: the rule file goes back to what the firewall still holds, and the
// answer shows it beside the record.
func TestSSHBootFailedLoadKeepsTheFileTrue(t *testing.T) {
	t.Parallel()
	h, nft := sshDaemon(t, seedRules)
	if err := h.sshWrite(sshState{Sources: []string{"198.51.100.0/24"}}); err != nil {
		t.Fatal(err)
	}
	nft.fails = 2 // the boot's load, then the daemon's start's
	if err := h.sshBoot(context.Background()); err == nil || !strings.Contains(err.Error(), "the rule file before it is back") || sshRulesNow(t, h) != seedRules {
		t.Fatalf("%v %q", err, sshRulesNow(t, h))
	}
	h.sshResume()
	if sshRulesNow(t, h) != seedRules {
		t.Fatalf("%q", sshRulesNow(t, h))
	}
	var a SSHAllow
	if code := call(t, h.Handler(), http.MethodGet, "/v1/ssh-allow", nil, &a); code != http.StatusOK || !slices.Equal(a.Sources, []string{"192.0.2.0/24"}) || !strings.Contains(a.Rules, "does not hold the recorded sources") {
		t.Fatalf("%d %+v", code, a)
	}
}

func TestSSHAllowRecordedButNoRule(t *testing.T) {
	t.Parallel()
	h, _ := sshDaemon(t, "")
	if err := h.sshWrite(sshState{Sources: []string{"198.51.100.0/24"}}); err != nil {
		t.Fatal(err)
	}
	var a SSHAllow
	if code := call(t, h.Handler(), http.MethodGet, "/v1/ssh-allow", nil, &a); code != http.StatusOK || a.Sources == nil || len(a.Sources) != 0 {
		t.Fatalf("%d %+v", code, a)
	}
}

func TestSSHAllowPreviousRulesOfOtherLines(t *testing.T) {
	t.Parallel()
	h, _ := sshDaemon(t, "tcp dport 22 accept\ntcp dport 2222 accept\n")
	manualTimers(h)
	var a SSHAllow
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}}, freshSSH(&a)); code != http.StatusOK || a.Pending == nil || a.Pending.Previous != nil {
		t.Fatalf("%d %+v", code, a)
	}
	var b bytes.Buffer
	printSSHAllow(&b, a)
	if !strings.Contains(b.String(), "else back to the rule file before it (lines other than SSH's sources)") {
		t.Errorf("%s", b.String())
	}
}

// A window's timer whose change is gone (its record removed by hand) undoes nothing, and says so.
func TestSSHAllowNothingToUndoIsNotLoggedUndone(t *testing.T) {
	t.Parallel()
	h, _ := sshDaemon(t, seedRules)
	var log bytes.Buffer
	h.O.Log = &log
	timers := manualTimers(h)
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}}, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	if err := os.Remove(SSHAllowFile(h.O.Dir)); err != nil {
		t.Fatal(err)
	}
	<-timers.fire(t)
	if !strings.Contains(log.String(), "nothing undone") || strings.Contains(log.String(), "undone: not confirmed") {
		t.Errorf("%s", log.String())
	}
	if h.netReverts[sshOwner] != nil {
		t.Error("retried")
	}
}

func TestSSHAllowPrintsTheMismatch(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	printSSHAllow(&b, SSHAllow{From: "set", Sources: []string{"192.0.2.0/24"}, Rules: "x does not hold the recorded sources"})
	if !strings.Contains(b.String(), "warning    x does not hold the recorded sources\n") {
		t.Errorf("%s", b.String())
	}
}

// A change that never loads, and whose reloads keep failing: the record goes back to the sources
// before it at once, so no boot applies the refused ones.
func TestSSHAllowAFailedChangeIsNotRecorded(t *testing.T) {
	t.Parallel()
	h, nft := sshDaemon(t, seedRules)
	manualTimers(h)
	nft.fails = 100
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}}, nil); code != http.StatusInternalServerError {
		t.Fatal(code)
	}
	if _, err := os.Stat(SSHAllowFile(h.O.Dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused sources are recorded: %v", err)
	}
	if sshRulesNow(t, h) != seedRules {
		t.Error("the rule file is not the one before")
	}
}

// No rule file before the change: its revert removes the one it wrote.
func TestSSHAllowRevertRemovesARuleFileThatWasNot(t *testing.T) {
	t.Parallel()
	h, _ := sshDaemon(t, "")
	if err := os.Remove(h.SSHRules); err != nil {
		t.Fatal(err)
	}
	manualTimers(h)
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}}, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow/revert", nil, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	if _, err := os.Stat(h.SSHRules); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the rule file: %v", err)
	}
}

// The record removed, the rule file still the set's: said so, not taken for the cloud-config's.
func TestSSHAllowASetsFileWithoutItsRecord(t *testing.T) {
	t.Parallel()
	h, _ := sshDaemon(t, seedRules)
	if err := os.WriteFile(h.SSHRules, []byte(rulesOf(t, h, []string{"198.51.100.0/24"})), 0o644); err != nil {
		t.Fatal(err)
	}
	var a SSHAllow
	if code := call(t, h.Handler(), http.MethodGet, "/v1/ssh-allow", nil, &a); code != http.StatusOK || a.From != "set" || !slices.Equal(a.Sources, []string{"198.51.100.0/24"}) || !strings.Contains(a.Rules, "is gone: the cloud-config's sources (or the image's rule) come back at the next boot") {
		t.Fatalf("%d %+v", code, a)
	}
}

// A timer that fired but has not run its undo yet (waiting for the lock): shown ended, as a
// confirmation then is refused.
func TestSSHAllowAFiredTimerIsShownEnded(t *testing.T) {
	t.Parallel()
	h, _ := sshDaemon(t, seedRules)
	manualTimers(h)
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}}, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	h.netReverts[sshOwner].fired.Store(true)
	var a SSHAllow
	if code := call(t, h.Handler(), http.MethodGet, "/v1/ssh-allow", nil, &a); code != http.StatusOK || a.Pending == nil || !a.Pending.Ended || a.Pending.EndedBy != "the change's window ended" {
		t.Fatalf("%d %+v", code, a.Pending)
	}
}

// failWritesFrom makes every createTemp from the nth call on fail; undone at the test's end. It
// replaces a package variable: its tests are not parallel.
func failWritesFrom(t *testing.T, n int) {
	var calls int
	var mu sync.Mutex
	createTemp = func(dir, pattern string) (*os.File, error) {
		mu.Lock()
		calls++
		c := calls
		mu.Unlock()
		if c >= n {
			return nil, errors.New("disk full")
		}
		return os.CreateTemp(dir, pattern)
	}
	t.Cleanup(func() { createTemp = os.CreateTemp })
}

// firewallHeld is whether another holds the firewall's lock in dir (firewallLock).
func firewallHeld(dir string) bool {
	f, err := os.OpenFile(filepath.Join(dir, ".firewall.lock"), os.O_RDONLY, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// A window whose undo the daemon's stop cut: no timer armed, the deadline past: the confirmation
// comes too late, and undoes it.
func TestSSHAllowConfirmPastTheDeadlineWithNoTimer(t *testing.T) {
	t.Parallel()
	h, _ := sshDaemon(t, seedRules)
	if err := os.WriteFile(h.SSHRules, []byte(rulesOf(t, h, []string{"198.51.100.0/24"})), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.sshWrite(sshState{Sources: []string{"198.51.100.0/24"}, Pending: &sshPending{ID: "x", Deadline: time.Now().Add(-time.Second), PreviousRules: seedRules}}); err != nil {
		t.Fatal(err)
	}
	var e map[string]any
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow/confirm", nil, &e); code != http.StatusConflict || !strings.Contains(e["error"].(string), "the sources before it are back") {
		t.Fatalf("%d %v", code, e)
	}
	if sshRulesNow(t, h) != seedRules {
		t.Error("not undone")
	}
}

// A revert to no rule file whose load fails: the file it was loaded from goes back.
func TestSSHPutAbsentFailedLoadPutsTheFileBack(t *testing.T) {
	t.Parallel()
	h, nft := sshDaemon(t, seedRules)
	nft.fails = 1
	if err := h.sshPut(context.Background(), "", true); err == nil || !strings.Contains(err.Error(), "the rule file is back") || sshRulesNow(t, h) != seedRules {
		t.Fatalf("%v %q", err, sshRulesNow(t, h))
	}
}

func TestSSHAllowAHandEditedRuleFile(t *testing.T) {
	t.Parallel()
	h, _ := sshDaemon(t, "tcp dport 2222 accept\n")
	var a SSHAllow
	if code := call(t, h.Handler(), http.MethodGet, "/v1/ssh-allow", nil, &a); code != http.StatusOK || a.From != "unknown" || a.Sources != nil {
		t.Fatalf("%d %+v", code, a)
	}
}

// A change that could not be applied, its restore failing and tried again: the change still awaits
// (its record), a new one is refused, and a confirmation says what happened.
func TestSSHAllowAChangeThatCouldNotBeApplied(t *testing.T) {
	h, nft := sshDaemon(t, seedRules)
	manualTimers(h)
	// Sources recorded before the change, which its restore records again.
	if err := h.sshWrite(sshState{Sources: []string{"203.0.113.0/24"}}); err != nil {
		t.Fatal(err)
	}
	nft.fails = 1        // the change's load
	failWritesFrom(t, 3) // the rule file's write back, then the restore's record
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}}, nil); code != http.StatusInternalServerError {
		t.Fatal(code)
	}
	createTemp = os.CreateTemp
	var a SSHAllow
	if code := call(t, h.Handler(), http.MethodGet, "/v1/ssh-allow", nil, &a); code != http.StatusOK || a.Pending == nil || !a.Pending.Ended || a.Pending.EndedBy != "the change could not be applied" {
		t.Fatalf("a failed apply not shown as one: %d %+v", code, a.Pending)
	}
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"203.0.113.0/24"}}, nil); code != http.StatusConflict {
		t.Errorf("a change over one being restored: %d", code)
	}
	var e map[string]any
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow/confirm", nil, &e); code != http.StatusConflict || !strings.HasPrefix(e["error"].(string), "the change could not be applied: the sources before it are back") {
		t.Fatalf("%d %v", code, e)
	}
}

// A pending change with no timer armed and its deadline ahead (a state built here; the daemon arms
// one for every pending change): the confirmation keeps it — the other side of the deadline rule.
func TestSSHAllowConfirmBeforeTheDeadlineWithNoTimer(t *testing.T) {
	t.Parallel()
	h, _ := sshDaemon(t, seedRules)
	if err := os.WriteFile(h.SSHRules, []byte(rulesOf(t, h, []string{"198.51.100.0/24"})), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.sshWrite(sshState{Sources: []string{"198.51.100.0/24"}, Pending: &sshPending{ID: "x", Deadline: time.Now().Add(time.Hour), PreviousRules: seedRules}}); err != nil {
		t.Fatal(err)
	}
	var a SSHAllow
	if code := call(t, h.Handler(), http.MethodGet, "/v1/ssh-allow", nil, freshSSH(&a)); code != http.StatusOK || a.Pending == nil || a.Pending.Ended {
		t.Fatalf("%d %+v", code, a)
	}
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow/confirm", nil, freshSSH(&a)); code != http.StatusOK || a.Message != "confirmed" {
		t.Fatalf("%d %+v", code, a)
	}
}

// Past the deadline with no timer: shown ended, as the confirmation then judges it.
func TestSSHAllowShowsPastTheDeadlineEnded(t *testing.T) {
	t.Parallel()
	h, _ := sshDaemon(t, seedRules)
	if err := h.sshWrite(sshState{Sources: []string{"198.51.100.0/24"}, Pending: &sshPending{ID: "x", Deadline: time.Now().Add(-time.Second), PreviousRules: seedRules}}); err != nil {
		t.Fatal(err)
	}
	var a SSHAllow
	if code := call(t, h.Handler(), http.MethodGet, "/v1/ssh-allow", nil, &a); code != http.StatusOK || a.Pending == nil || !a.Pending.Ended {
		t.Fatalf("%d %+v", code, a)
	}
}

// A revert with sources recorded before the change writes them, not the file read at the change.
func TestSSHAllowRevertWritesThePreviousSources(t *testing.T) {
	t.Parallel()
	h, _ := sshDaemon(t, "a failed change's file\n")
	if err := h.sshWrite(sshState{Sources: []string{"198.51.100.0/24"}, Pending: &sshPending{ID: "x", Deadline: time.Now().Add(time.Hour), Previous: []string{"203.0.113.0/24"}, PreviousRules: "a failed change's file\n"}}); err != nil {
		t.Fatal(err)
	}
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow/revert", nil, nil); code != http.StatusOK || sshRulesNow(t, h) != rulesOf(t, h, []string{"203.0.113.0/24"}) {
		t.Fatalf("%d %q", code, sshRulesNow(t, h))
	}
}

// A revert asked whose load failed, tried again: the refusal names it, not a failed apply.
func TestSSHAllowConfirmAfterAFailedRevertNamesIt(t *testing.T) {
	t.Parallel()
	h, nft := sshDaemon(t, seedRules)
	manualTimers(h)
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}}, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	nft.mu.Lock()
	nft.fails = 1
	nft.mu.Unlock()
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow/revert", nil, nil); code == http.StatusOK {
		t.Fatal("a revert whose load failed")
	}
	var e map[string]any
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow/confirm", nil, &e); code != http.StatusConflict || !strings.HasPrefix(e["error"].(string), "a revert was asked: the sources before it are back") {
		t.Fatalf("%d %v", code, e)
	}
}

func TestSSHAllowAWindowOutOfRangeNamesTheRange(t *testing.T) {
	t.Parallel()
	h, _ := sshDaemon(t, seedRules)
	for _, secs := range []int{0, minRevertSecs - 1, maxRevertSecs + 1} {
		var e map[string]any
		if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}, RevertSecs: intp(secs)}, &e); code != http.StatusBadRequest ||
			e["error"] != fmt.Sprintf("revert_secs %d: every change of SSH's sources has a window, %d to %d s", secs, minRevertSecs, maxRevertSecs) {
			t.Errorf("%d: %d %v", secs, code, e)
		}
	}
	for _, secs := range []int{minRevertSecs, maxRevertSecs} {
		h, _ := sshDaemon(t, seedRules)
		manualTimers(h)
		if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}, RevertSecs: intp(secs)}, nil); code != http.StatusOK {
			t.Errorf("%d: %d", secs, code)
		}
	}
}

func TestEndedByADaemonRestart(t *testing.T) {
	t.Parallel()
	if got := (&netRevert{why: restartedUnconfirmed}).endedBy(); got != "the host daemon restarted inside the window" {
		t.Errorf("%q", got)
	}
	if got := (&netRevert{why: "something else"}).endedBy(); got != "the change is being undone (something else)" {
		t.Errorf("%q", got)
	}
}

// A change the daemon refuses prints no warning: it shuts no one out.
func TestSSHAllowWarnsOnlyOnceApplied(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	sh, nft := sshDaemon(t, seedRules)
	h := daemon(e, &fakeHost{})
	h.Host, h.SSHRules = nft, sh.SSHRules
	sock := served(t, h)
	times := clientTimes{unit: time.Millisecond, env: func(k string) string {
		if k == "SSH_CONNECTION" {
			return "203.0.113.5 51000 192.0.2.10 22"
		}
		return ""
	}}
	out, err := ctlTimes(context.Background(), e, sock, times, "ssh-allow", "set", "198.51.100.0/24", "--revert-secs", "5")
	if err == nil || strings.Contains(out, "this session comes from") {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err = ctlTimes(context.Background(), e, sock, times, "ssh-allow", "set", "198.51.100.0/24"); err != nil || !strings.Contains(out, "this session comes from 203.0.113.5") {
		t.Fatalf("%v\n%s", err, out)
	}
}

// The firewall's lock held by another (a hung load): the change fails within the bound, not stalls.
func TestSSHAllowTheFirewallsLockIsWaitedForBounded(t *testing.T) {
	t.Parallel()
	h, nft := sshDaemon(t, seedRules)
	manualTimers(h)
	unlock, err := firewallLock(context.Background(), filepath.Dir(h.SSHRules))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	done := make(chan int, 1)
	go func() {
		done <- call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}}, nil)
	}()
	select {
	case code := <-done:
		if code == http.StatusOK || len(nft.loaded) != 0 {
			t.Fatalf("%d %v", code, nft.loaded)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the change waits for the lock unbounded")
	}
}

// A change that could not be applied whose rule file cannot be put back keeps its record, pending:
// a new change is refused, and revert (or a restart, a boot) undoes it.
func TestSSHAllowAFailedChangeStaysPendingUntilUndone(t *testing.T) {
	h, nft := sshDaemon(t, seedRules)
	manualTimers(h)
	nft.fails = 1
	failWritesFrom(t, 3) // the rule file's write back, then the restore's write of it
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"203.0.113.0/24"}}, nil); code != http.StatusInternalServerError {
		t.Fatal(code)
	}
	if st, _, err := h.sshRead(); err != nil || st.Pending == nil {
		t.Fatalf("the failed change is not pending: %+v %v", st, err)
	}
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}}, nil); code != http.StatusConflict {
		t.Fatalf("a change over it: %d", code)
	}
	createTemp = os.CreateTemp
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow/revert", nil, nil); code != http.StatusOK || sshRulesNow(t, h) != seedRules {
		t.Fatalf("the revert: %d %q", code, sshRulesNow(t, h))
	}
}

// A revert that gave up, its record removed by hand since: a new change finds nothing to undo, and
// goes on.
func TestSSHAllowAGaveUpRevertWithItsRecordGone(t *testing.T) {
	t.Parallel()
	h, nft := sshDaemon(t, seedRules)
	timers := manualTimers(h)
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}}, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	nft.mu.Lock()
	nft.fails = revertAttempts
	nft.mu.Unlock()
	for range revertAttempts {
		<-timers.fire(t)
	}
	if err := os.Remove(SSHAllowFile(h.O.Dir)); err != nil {
		t.Fatal(err)
	}
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"203.0.113.0/24"}}, nil); code != http.StatusOK {
		t.Fatalf("%d", code)
	}
}

// A late confirmation whose undo fails again keeps what the undo was for.
func TestSSHAllowALateConfirmKeepsTheWhy(t *testing.T) {
	t.Parallel()
	h, nft := sshDaemon(t, seedRules)
	manualTimers(h)
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}}, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	nft.mu.Lock()
	nft.fails = 2
	nft.mu.Unlock()
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow/revert", nil, nil); code == http.StatusOK {
		t.Fatal("a revert whose load failed")
	}
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow/confirm", nil, nil); code != http.StatusConflict {
		t.Fatal(code)
	}
	var a SSHAllow
	if code := call(t, h.Handler(), http.MethodGet, "/v1/ssh-allow", nil, &a); code != http.StatusOK || a.Pending == nil || a.Pending.EndedBy != "a revert was asked" {
		t.Fatalf("%d %+v", code, a.Pending)
	}
}

// The window counts from the load: a wait for the firewall's lock does not shorten it.
func TestSSHAllowTheWindowCountsFromTheLoad(t *testing.T) {
	t.Parallel()
	h, _ := sshDaemon(t, seedRules)
	h.Unit = 40 * time.Millisecond // the lock's bound 600 ms (polled every 100 ms), the window 3.6 s
	manualTimers(h)
	unlock, err := firewallLock(context.Background(), filepath.Dir(h.SSHRules))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan int, 1)
	go func() {
		close(started)
		done <- call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}}, nil)
	}()
	<-started
	// The change waits for the lock: what it waits does not count in its window.
	time.Sleep(150 * time.Millisecond)
	unlocked := time.Now()
	unlock()
	if code := <-done; code != http.StatusOK {
		t.Fatal(code)
	}
	st, _, err := h.sshRead()
	if err != nil || st.Pending == nil || st.Pending.Deadline.Before(unlocked.Add(time.Duration(defaultRevertSecs)*h.Unit)) {
		t.Fatalf("deadline %v, the load after %v: %v", st.Pending, unlocked, err)
	}
}

// readSSHRules reads text an operator may edit: whatever it is, what it takes are masked prefixes,
// and what sshRules writes it reads back.
func FuzzReadSSHRules(f *testing.F) {
	for _, s := range []string{seedRules, "tcp dport 22 accept\n", "ip6 saddr 2001:db8::/32 tcp dport 22 accept\n", "", "# x\n\nip saddr 10.0.0.1/8 tcp dport 22 accept\n", "ip saddr { 1.2.3.4 } tcp dport 22 accept"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		got, ok := readSSHRules(text)
		if !ok {
			return
		}
		ps, err := parseSSHSources("", got)
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		for i, p := range ps {
			if p.String() != got[i] {
				t.Fatalf("%q: %s not masked", text, got[i])
			}
		}
		if back, ok := readSSHRules(sshRules("x", ps)); !ok || !slices.Equal(back, got) {
			t.Fatalf("%q: written back %v %v", text, back, ok)
		}
	})
}

// rulesOf is the rule file `ssh-allow set` writes for sources.
func rulesOf(t *testing.T, h *Hostd, sources []string) string {
	t.Helper()
	text, err := h.sshSetRules(sources)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestSSHSetRulesRefusesWhatDoesNotParse(t *testing.T) {
	t.Parallel()
	h, _ := sshDaemon(t, seedRules)
	for _, bad := range [][]string{nil, {"198.51.100.0/24", "nope"}} {
		if text, err := h.sshSetRules(bad); err == nil || text != "" {
			t.Errorf("%v: %q %v", bad, text, err)
		}
	}
}

func TestSSHSourcesIPv4MappedAreIPv4(t *testing.T) {
	t.Parallel()
	ps, err := parseSSHSources("x", []string{"::ffff:203.0.113.9/120"})
	if err != nil || len(ps) != 1 || ps[0].String() != "203.0.113.0/24" {
		t.Fatalf("%v %v", ps, err)
	}
	if _, err := parseSSHSources("x", []string{"::ffff:0.0.0.0/64"}); err == nil || !strings.Contains(err.Error(), "spans beyond IPv4's mapped addresses") {
		t.Fatalf("%v", err)
	}
}

// Applied, its window armed, and its answer cannot be read after: 200 all the same, saying so.
func TestSSHAllowAppliedAnswersEvenIfItsReadFails(t *testing.T) {
	t.Parallel()
	h, nft := sshDaemon(t, seedRules)
	manualTimers(h)
	// A directory in the rule file's place once loaded: reading it fails, as root too.
	nft.afterLoad = func() { _ = os.Remove(h.SSHRules); _ = os.Mkdir(h.SSHRules, 0o755) }
	var a SSHAllow
	if code := call(t, h.Handler(), http.MethodPost, "/v1/ssh-allow", SSHRequest{Sources: []string{"198.51.100.0/24"}}, &a); code != http.StatusOK || !strings.Contains(a.Message, "ssh-allow confirm") {
		t.Fatalf("%d %+v", code, a)
	}
}
