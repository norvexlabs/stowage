package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/norvexlabs/stowage/internal/spec"
)

// ctlSplit runs the VM's <short>-stow of e with args against the daemon on sock, its standard
// output and standard error apart.
func ctlSplit(e *env, sock string, args ...string) (stdout, stderr string, err error) {
	var out, errb bytes.Buffer
	cmd := vmCommand(e.r, e.self, &out, &errb, func(bool) Prompter { return nil }, clientTimes{unit: time.Millisecond})
	cmd.SetArgs(append(args, "--socket", sock))
	err = cmd.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

// strictJSON decodes raw into v, refusing a field v does not name and anything after it.
func strictJSON(t *testing.T, raw string, v any) {
	t.Helper()
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	if d.More() {
		t.Fatalf("more than one object:\n%s", raw)
	}
}

// boot-rollback's refusal (exit 3, nothing changed by it) is the boot's status, and the boot
// claims no change; any other exit is the boot's failure.
func TestTheBootPassesBootRollbacksRefusalOn(t *testing.T) {
	t.Parallel()
	for code, refused := range map[ExitCode]bool{3: true, 1: false} {
		e, _, _ := upgradedVM(t)
		e.r, e.self = bundledInstaller(t, "1.0.0", 1, nil) // the OS fell back to the release replaced
		in := e.installer(vm)
		var ran []string
		in.O.Exec = func(_ context.Context, _ string, args []string) error {
			ran = append(ran, args[0])
			return code
		}
		err := in.Boot(context.Background())
		if !slices.Equal(ran, []string{bootRollbackCmd}) {
			t.Fatalf("%d: ran %v", code, ran)
		}
		var got ExitCode
		switch {
		case refused && (!errors.As(err, &got) || got != 3 || errors.As(err, new(changedErr))):
			t.Errorf("%d: %#v", code, err)
		case !refused && (err == nil || errors.As(err, new(ExitCode)) || !strings.Contains(err.Error(), "exited with status 1")):
			t.Errorf("%d: %v", code, err)
		}
	}
}

// The VM's `status --json` carries the machine interface's names: the format, the release
// `rollback` goes back to, and the keys of the inputs recorded — never a value, an input's or a
// secret's.
func TestVMStatusJSON(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	e.r, e.self = bundledInstaller(t, "1.0.0", 1, func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "ACCESS_TOKEN", Secret: true})
	})
	if err := e.install(func(o *Options) { vm(o); o.Set = map[string]string{"ACCESS_TOKEN": "tok-not-to-be-printed"} }); err != nil {
		t.Fatalf("%v\n%s", err, e.log)
	}
	sock := served(t, daemon(e, &fakeHost{}))
	out, _, err := ctlSplit(e, sock, "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	strictJSON(t, out, &st)
	if !strings.HasPrefix(out, "{\n  \"format\": 1,\n") || st.Installed == nil || *st.Installed != (Version{"1.0.0", 1}) ||
		st.Previous != nil || !strings.Contains(out, `"previous": null`) || st.Inputs == nil || !slices.Equal(*st.Inputs, []string{"ACCESS_TOKEN", "ADMIN_EMAIL"}) {
		t.Errorf("%+v\n%s", st, out)
	}
	env := e.readEnv()
	for _, k := range []string{"ADMIN_EMAIL", "ACCESS_TOKEN", "SESSION_SECRET", "POSTGRES_PASSWORD"} {
		if v := get(env, k); v == "" || strings.Contains(out, v) {
			t.Errorf("%s=%q is printed, or not recorded:\n%s", k, v, out)
		}
	}

	// Upgraded: the release replaced is the previous one, by version alone (the OS carries it).
	e, d, _ := upgradedVM(t)
	out, _, err = ctlSplit(e, served(t, d), "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	st = Status{}
	strictJSON(t, out, &st)
	if st.Previous == nil || *st.Previous != (StatusRelease{Version: "1.0.0", Revision: 1}) || st.Inputs == nil || !slices.Equal(*st.Inputs, []string{"ADMIN_EMAIL"}) {
		t.Errorf("%+v\n%s", st, out)
	}

	// Before the first install: no inputs to tell.
	e = vmEnv(t, "1.0.0")
	out, _, err = ctlSplit(e, served(t, daemon(e, &fakeHost{})), "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	st = Status{}
	strictJSON(t, out, &st)
	if st.Installed != nil || st.Previous != nil || st.Inputs != nil || strings.Contains(out, `"inputs"`) {
		t.Errorf("%+v\n%s", st, out)
	}
}

// The OS's <short>-stow describes the release it carries, as the Docker installer does.
func TestVMDescribe(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	out, _, err := ctlSplit(e, "unused", "describe", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := printJSON(&b, Describe(e.r)); err != nil {
		t.Fatal(err)
	}
	if out != b.String() {
		t.Errorf("%s\nwant\n%s", out, b.String())
	}
}

// result is one Result as --json prints it, decoded strictly.
func result(t *testing.T, out string) Result {
	t.Helper()
	var r Result
	strictJSON(t, out, &r)
	if r.Format != MachineFormat {
		t.Errorf("format %d", r.Format)
	}
	return r
}

// upgrade and rollback under --json print one Result alone on standard output: done, the release
// installed after the run as the daemon tells it; nothing, with the refusal and its fix when a
// rule said no before anything changed (client side, or the daemon's); and none at all on a
// failure — a refusal answered once the job started included, which is a failure.
func TestVMVerbsJSONResults(t *testing.T) {
	t.Parallel()
	avail := Update{Channel: "stable", Current: Version{"1.0.0", 1}, Target: Version{"2.0.0", 1}, Available: true}
	refused := func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 409, map[string]string{"error": "it is yanked", "fix": "choose another release"})
	}
	started := func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 202, JobStarted{Job: "upgrade", Target: "2.0.0-1", Started: fakeStart, Events: ptr(int64(0))})
	}
	jobEnds := func(state string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			reply(w, 200, JobStatus{Started: ptr(fakeStart), Kind: "upgrade", Target: "2.0.0-1", State: state, Error: "it broke", Log: []string{"a line"}})
		}
	}
	for name, c := range map[string]struct {
		args     []string
		routes   map[string]http.HandlerFunc
		result   string // "" : no Result, a failure
		fix      string
		exit3    bool
		released bool
	}{
		"upgrade done": {[]string{"upgrade", "--yes"}, map[string]http.HandlerFunc{
			"GET /v1/updates": func(w http.ResponseWriter, _ *http.Request) { reply(w, 200, avail) }, "POST /v1/upgrade": started, "GET /v1/job": jobEnds("done"),
		}, ResultDone, "", false, true},
		"upgrade with nothing to do": {[]string{"upgrade", "--yes"}, map[string]http.HandlerFunc{
			"GET /v1/updates": func(w http.ResponseWriter, _ *http.Request) {
				reply(w, 200, Update{Channel: "stable", Current: Version{"1.0.0", 1}, Target: Version{"1.0.0", 1}})
			},
		}, ResultNothing, "", false, true},
		"upgrade --version refused here": {[]string{"upgrade", "--yes", "--version", "0.9.0"}, map[string]http.HandlerFunc{
			"GET /v1/updates": func(w http.ResponseWriter, _ *http.Request) {
				reply(w, 200, Update{Channel: "stable", Current: Version{"1.0.0", 1}, Target: Version{"0.9.0", 1}})
			},
		}, ResultNothing, "choose a release published with an OS bundle after the one this VM runs", true, true},
		"upgrade without a terminal or --yes": {[]string{"upgrade"}, map[string]http.HandlerFunc{
			"GET /v1/updates": func(w http.ResponseWriter, _ *http.Request) { reply(w, 200, avail) },
		}, ResultNothing, "give --yes, which upgrades without asking", true, true},
		"upgrade refused by the daemon": {[]string{"upgrade", "--yes"}, map[string]http.HandlerFunc{
			"GET /v1/updates": func(w http.ResponseWriter, _ *http.Request) { reply(w, 200, avail) }, "POST /v1/upgrade": refused,
		}, ResultNothing, "choose another release", true, true},
		"upgrade refused once its job started": {[]string{"upgrade", "--yes"}, map[string]http.HandlerFunc{
			"GET /v1/updates": func(w http.ResponseWriter, _ *http.Request) { reply(w, 200, avail) }, "POST /v1/upgrade": started, "GET /v1/job": refused,
		}, "", "", false, false},
		"upgrade failed": {[]string{"upgrade", "--yes"}, map[string]http.HandlerFunc{
			"GET /v1/updates": func(w http.ResponseWriter, _ *http.Request) { reply(w, 200, avail) }, "POST /v1/upgrade": started, "GET /v1/job": jobEnds("failed"),
		}, "", "", false, false},
		"rollback done": {[]string{"rollback", "--yes"}, map[string]http.HandlerFunc{
			"GET /v1/rollback": func(w http.ResponseWriter, _ *http.Request) {
				reply(w, 200, RollbackPlan{From: Version{"2.0.0", 1}, To: Version{"1.0.0", 1}})
			},
			"POST /v1/rollback": started, "GET /v1/job": jobEnds("done"),
		}, ResultDone, "", false, true},
		"rollback refused by the daemon": {[]string{"rollback", "--yes"}, map[string]http.HandlerFunc{"GET /v1/rollback": refused}, ResultNothing, "choose another release", true, true},
		"rollback refused once its job started": {[]string{"rollback", "--yes"}, map[string]http.HandlerFunc{
			"GET /v1/rollback": func(w http.ResponseWriter, _ *http.Request) {
				reply(w, 200, RollbackPlan{From: Version{"2.0.0", 1}, To: Version{"1.0.0", 1}})
			},
			"POST /v1/rollback": started, "GET /v1/job": refused,
		}, "", "", false, false},
		"rollback without a terminal or --yes": {[]string{"rollback"}, map[string]http.HandlerFunc{
			"GET /v1/rollback": func(w http.ResponseWriter, _ *http.Request) {
				reply(w, 200, RollbackPlan{From: Version{"2.0.0", 1}, To: Version{"1.0.0", 1}})
			},
		}, ResultNothing, "give --yes, which goes back without asking", true, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := vmEnv(t, "1.0.0")
			mux := http.NewServeMux()
			for p, h := range c.routes {
				mux.HandleFunc(p, h)
			}
			mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, _ *http.Request) {
				reply(w, 200, Status{Name: "notes", Installed: &Version{"1.0.0", 1}, Job: "idle"})
			})
			out, log, err := ctlSplit(e, servedHandler(t, mux), append(c.args, "--json")...)
			if _, ok := Refusal(err); ok != c.exit3 || (c.result == ResultDone || c.result == ResultNothing && c.fix == "") && err != nil {
				t.Errorf("err %v (refused %t)\n%s", err, ok, log)
			}
			if c.result == "" {
				if out != "" || err == nil {
					t.Errorf("a failure printed %q (err %v)", out, err)
				}
				return
			}
			r := result(t, out)
			if r.Result != c.result || (r.Release == nil || *r.Release != (Version{"1.0.0", 1})) != !c.released {
				t.Errorf("%+v\n%s", r, out)
			}
			switch {
			case c.fix == "" && r.Refused != nil, c.fix != "" && (r.Refused == nil || r.Refused.Fix != c.fix || r.Refused.Reason != err.Error()):
				t.Errorf("refused %+v, want the fix %q", r.Refused, c.fix)
			}
			// The run's text is the log's, never standard output's.
			if strings.Contains(out, "2.0.0 (revision 1) on stable") || strings.Contains(out, "the passive system carries") {
				t.Errorf("text on standard output:\n%s", out)
			}
		})
	}
}

// The VM's install under --json: a refusal prints its Result, a failure none.
func TestVMInstallJSON(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	dir := filepath.Join(t.TempDir(), "install")
	out, _, err := ctlSplit(e, "unused", "install", "--json", "--dir", dir, "--no-input", "--secrets", "s.yaml", "--given", "g.yaml")
	if r := result(t, out); !isRefused(err) || r.Result != ResultNothing || r.Refused == nil || r.Refused.Fix != "give --secrets or --given, not both" || r.Release != nil {
		t.Errorf("%v\n%s", err, out)
	}
	out, _, err = ctlSplit(e, "unused", "install", "--json", "--dir", dir, "--no-input", "--channel", "Bad Channel")
	if err == nil || isRefused(err) || out != "" {
		t.Errorf("%v\n%q", err, out)
	}
}

// The daemon answers a refusal with its fix, which the client takes as a refusal (exit 3); any
// other error carries none.
func TestTheDaemonAnswersARefusalWithItsFix(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	d := daemon(p.env, &fakeHost{})
	h := d.Handler()
	answer := func(method, path string, body any) (int, map[string]string) {
		t.Helper()
		var out map[string]string
		code := call(t, h, method, path, body, &out)
		return code, out
	}
	// Going back: from the passive system, with no passive system, with no previous release; and
	// a rollback check's refusal.
	boots(t, d, "passive", nil)
	if code, out := answer("GET", "/v1/rollback", nil); code != 409 || out["fix"] != "nothing to go back to: the VM runs the earlier system already" {
		t.Errorf("passive: %d %v", code, out)
	}
	_ = os.Remove(filepath.Join(d.O.Cos, "passive_mode"))
	boots(t, d, "active", nil)
	passive := filepath.Join(d.assessment().dir, passiveImage)
	if err := os.Remove(passive); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if code, out := answer("GET", "/v1/rollback", nil); code != 409 || out["fix"] != "nothing to go back to: an upgrade leaves the system it replaces" {
		t.Errorf("no passive: %d %v", code, out)
	}
	if err := os.MkdirAll(filepath.Dir(passive), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passive, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := answer("GET", "/v1/rollback", nil); code != 409 || out["fix"] != "nothing to go back to: an upgrade keeps the release it replaces" {
		t.Errorf("no previous: %d %v", code, out)
	}
	w := httptest.NewRecorder()
	replyErr(w, failAs(409, rollbackRefused("the database moved on")))
	var out map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 409 || out["error"] != "the database moved on" || !strings.HasPrefix(out["fix"], "stay on the installed release") {
		t.Errorf("%d %v", w.Code, out)
	}
	// An error that is no refusal: no fix.
	for _, err := range []error{fail(409, "a job (upgrade) is running"), failAs(409, errors.New("plain"))} {
		w := httptest.NewRecorder()
		replyErr(w, err)
		if strings.Contains(w.Body.String(), "fix") {
			t.Errorf("%v: %s", err, w.Body)
		}
	}
	// The client: a fix makes the answer a Refused; none, a plain error.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /refused", func(w http.ResponseWriter, _ *http.Request) { replyErr(w, refuseAs(409, "do this", "no")) })
	mux.HandleFunc("GET /failed", func(w http.ResponseWriter, _ *http.Request) { replyErr(w, fail(409, "no")) })
	sock := servedHandler(t, mux)
	c := &hostClient{http: socketClient(&sock)}
	if r, ok := Refusal(c.do(context.Background(), "GET", "/refused", nil, nil)); !ok || r.Reason != "no" || r.Fix != "do this" {
		t.Errorf("%+v %t", r, ok)
	}
	if err := c.do(context.Background(), "GET", "/failed", nil, nil); err == nil || isRefused(err) {
		t.Errorf("%v", err)
	}
}

// vm seed --json is the seed vm create's manual path writes, by the same steps: the cloud-config
// with its one-time host key, that key's known_hosts line, fingerprint and private key, and the
// disk each platform boots, by digest. It never asks, and carries no secret input.
func TestVMSeed(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2", "raw")
	v.r.Spec.Inputs = append(v.r.Spec.Inputs, spec.Input{Key: "ACCESS_TOKEN", Secret: true})
	v.vars["ACCESS_TOKEN"] = "tok-not-to-be-printed"
	asked := &askCounter{}
	seed := func(mod func(*Options), channel string) (*VMSeed, error) {
		in := v.installer(func(o *Options) {
			o.PublicAddr = func(context.Context) (netip.Addr, error) { return netip.MustParseAddr("198.51.100.40"), nil }
			o.TLS, o.Set = "internal", map[string]string{"ADMIN_EMAIL": "admin@example.org", "APP_DOMAIN": "app.example.org", "API_DOMAIN": "api.example.org"}
			o.Prompt = asked // never asked
			if mod != nil {
				mod(o)
			}
		})
		return in.SeedVM(context.Background(), &VMCreate{Platform: "truenas", Hostname: "notes-1", User: "ops", SSHKey: v.key}, channel)
	}
	s, err := seed(nil, "beta")
	if err != nil || asked.n != 0 {
		t.Fatalf("%v (asked %d times)\n%s", err, asked.n, v.log)
	}
	raw, _ := json.Marshal(s)
	if strings.Contains(string(raw), "tok-not-to-be-printed") {
		t.Errorf("a secret input in the seed:\n%s", raw)
	}
	st, cc := cloudSettings(t, s.UserData)
	if s.Format != 1 || s.Platform != "truenas" || st.Channel != "beta" || st.AwaitSecrets || st.Inputs["ADMIN_EMAIL"] != "admin@example.org" || st.Inputs["ACCESS_TOKEN"] != "" {
		t.Errorf("%+v\n%+v", s, st)
	}
	// The key the cloud-config carries is the one the line and the fingerprint name.
	var carried bool
	for _, stg := range cc.Stages {
		for _, x := range stg {
			for _, f := range x.Files {
				carried = carried || f.Content == s.HostKeyPrivate
			}
		}
	}
	priv, err := ssh.ParsePrivateKey([]byte(s.HostKeyPrivate))
	if err != nil || !carried {
		t.Fatalf("%v; carried %t", err, carried)
	}
	_, hosts, pub, _, _, err := ssh.ParseKnownHosts([]byte(s.KnownHostsLine))
	if err != nil || !slices.Equal(hosts, []string{seedHostAlias}) || !bytes.Equal(pub.Marshal(), priv.PublicKey().Marshal()) || s.Fingerprint != ssh.FingerprintSHA256(pub) {
		t.Errorf("%v %v %q", err, hosts, s.Fingerprint)
	}
	// Every platform the registry holds a disk for, by digest: qcow2 where it boots it, raw on EC2.
	for p, f := range map[string]string{"truenas": "qcow2", "proxmox": "qcow2", "libvirt": "qcow2", "scaleway": "qcow2", "aws": "raw"} {
		if d := s.Disk[p]; d.Format != f || d.Image != v.prefix+"/notes-disk@"+v.r.Manifest.Artifacts.Disks[f] {
			t.Errorf("%s: %+v", p, d)
		}
	}
	if _, ok := s.Disk["vmware"]; ok || len(s.Disk) != 5 {
		t.Errorf("%v", s.Disk)
	}

	// A secret input given, by the answers or a command line: refused, and never printed.
	_, err = seed(func(o *Options) { o.Answers = map[string]string{"ACCESS_TOKEN": "tok-not-to-be-printed"} }, "")
	if r, ok := Refusal(err); !ok || !strings.Contains(r.Reason, "ACCESS_TOKEN") || strings.Contains(r.Reason, "tok-not") || !strings.Contains(r.Fix, "install --given -") {
		t.Errorf("%v", err)
	}
	if _, err := seed(func(o *Options) { o.Set["ACCESS_TOKEN"] = "tok-not-to-be-printed" }, ""); err == nil || strings.Contains(err.Error(), "tok-not") {
		t.Errorf("%v", err)
	}
	// A bad channel, and no platform: refused before the registry is read.
	if _, err := seed(nil, "Bad Channel"); err == nil || !strings.Contains(err.Error(), "is not a channel name") {
		t.Errorf("%v", err)
	}
	in := v.installer(nil)
	if _, err := in.SeedVM(context.Background(), &VMCreate{Hostname: "notes-1", SSHKey: v.key}, ""); err == nil || !strings.Contains(err.Error(), "--platform is required") {
		t.Errorf("%v", err)
	}
}

// vm seed's command prints the seed alone, as JSON, and takes --json only.
func TestVMSeedCommand(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	var out, errb bytes.Buffer
	o := v.installer(nil).O
	o.Out, o.Log = &out, &errb
	in := &Installer{R: v.r}
	var sets, setFiles, domains []string
	var answers string
	var noInput, accessible, takesTLS bool
	prepare := func() error {
		var err error
		if o.Set, err = parseSets(sets); err != nil {
			return err
		}
		if o.Domains, err = parseDomains(domains); err != nil {
			return err
		}
		in.O = o
		return nil
	}
	c := vmCreateCommand(in, &o, prepare, func(bool) Prompter { return nil }, &sets, &setFiles, &domains, &answers, &noInput, &accessible, &takesTLS)
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	c.SetArgs([]string{"seed", "--platform", "truenas", "--ssh-key", v.key})
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "json") {
		t.Errorf("without --json: %v", err)
	}
	c.SetArgs([]string{"seed", "--json", "--platform", "truenas", "--ssh-key", v.key, "--user", "ops", "--tls", "internal",
		"--set", "ADMIN_EMAIL=admin@example.org", "--domains", "app=app.example.org,api=api.example.org"})
	if err := c.Execute(); err != nil {
		t.Fatalf("%v\n%s", err, errb.String())
	}
	var s VMSeed
	strictJSON(t, out.String(), &s)
	if s.Platform != "truenas" || s.Disk["truenas"].Format != "qcow2" {
		t.Errorf("%+v", s)
	}
}

// --known-hosts-line gives the VM's host key in place of vm create's record: ssh checks that key
// alone, strictly, under the line's host name; a line that is not one host's key is refused
// before any connection.
func TestKnownHostsLine(t *testing.T) {
	t.Parallel()
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEAIlrmYko7n75rw1in9PSsqvQQwoVBxv2WjCA2q6yuS"
	state := t.TempDir()
	in := &Installer{R: kitRelease(), O: Options{Log: io.Discard, SSH: &kitSSH{}, Getenv: func(k string) string {
		if k == "XDG_STATE_HOME" {
			return state
		}
		return ""
	}}}
	// No vm create state for this VM: the line alone is its key.
	l, err := in.link(VMReach{Host: "notes-1", User: "ops", Address: "192.0.2.10", KnownHostsLine: "VM-Seed-Host-Key " + key + " a comment\n"})
	if err != nil {
		t.Fatal(err)
	}
	args := l.args("true")
	if !slices.Contains(args, "StrictHostKeyChecking=yes") || !slices.Contains(args, "HostKeyAlias=vm-seed-host-key") || !slices.Contains(args, `UserKnownHostsFile="`+l.known+`"`) ||
		slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, "accept-new") }) || !slices.Contains(args, "ops@192.0.2.10") {
		t.Errorf("%q", args)
	}
	if raw, err := os.ReadFile(l.known); err != nil || string(raw) != "vm-seed-host-key "+key+"\n" || filepath.Dir(l.known) != filepath.Join(state, "notes", "known-hosts") {
		t.Errorf("%s %q: %v", l.known, raw, err)
	}
	if fi, err := os.Stat(l.known); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("%v %v", fi, err)
	}
	// The same line, the same file.
	if l2, err := in.link(VMReach{Host: "notes-1", KnownHostsLine: "vm-seed-host-key " + key}); err != nil || l2.known != l.known {
		t.Errorf("%v %s", err, l2.known)
	}
	for line, want := range map[string]string{
		"": "the VM's host key is not known",
		"vm-seed-host-key " + key + "\nother " + key: "one line",
		"@cert-authority * " + key:                   "not a @cert-authority line",
		"@revoked vm " + key:                         "not a @revoked line",
		"* " + key:                                   "not one host name",
		"a,b " + key:                                 "not one host name",
		"|1|c2FsdA==|aGFzaA== " + key:                "not one host name",
		"[vm]:2222 " + key:                           "not one host name",
		"vm ssh-ed25519 not-base64":                  "not a known_hosts line",
		"# a comment":                                "not a known_hosts line",
	} {
		if _, err := in.link(VMReach{Host: "notes-1", KnownHostsLine: line}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", line, err, want)
		}
	}
}
