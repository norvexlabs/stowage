package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/norvexlabs/stowage/internal/payload"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/testutil"
)

// fakeHost records the host programs the daemon runs.
type fakeHost struct {
	mu     sync.Mutex
	ran    [][]string
	config string // the Docker config kairos-agent ran with
	out    string // what kairos-agent prints
	err    error  // kairos-agent fails
	// gate, when set, holds kairos-agent until it is closed.
	gate chan struct{}
	// boot is the ActiveState of <name>.service, and judge of <name>-health.service; empty is
	// active.
	boot, judge string
	// editErr fails grub2-editenv; remountErr fails a remount, roErr a remount read-only;
	// rebootErr fails `systemctl reboot`; editAfterReboot, once it failed, fails grub2-editenv.
	editErr, remountErr, roErr, rebootErr, editAfterReboot error
	// rebootHold, when set, holds `systemctl reboot` (recorded) until it is closed: a reboot
	// whose shutdown stops the daemon before the command returns.
	rebootHold chan struct{}
	// calendarErr fails systemd-analyze calendar; noTimer: systemd has no backup timer.
	calendarErr error
	noTimer     bool
}

func (f *fakeHost) Run(_ context.Context, c Cmd) ([]byte, error) {
	if len(c.Args) > 1 && c.Args[0] == "systemctl" && c.Args[1] == "show" {
		f.mu.Lock()
		defer f.mu.Unlock()
		var st string
		switch c.Args[len(c.Args)-1] {
		case "notes-backup.timer":
			switch {
			case slices.Contains(c.Args, "--value"):
				return []byte("Sun 2026-10-11 03:04:05 UTC\n"), nil
			case f.noTimer:
				return []byte("LoadState=not-found\nActiveState=inactive\n"), nil
			}
			return []byte("LoadState=loaded\nActiveState=active\n"), nil
		case "notes.service":
			st = f.boot
		case "notes-health.service":
			st = f.judge
		default:
			return nil, fmt.Errorf("asked about %v", c.Args)
		}
		switch st {
		case "":
			return []byte("LoadState=loaded\nActiveState=active\n"), nil
		case noBootUnit:
			return []byte("LoadState=not-found\nActiveState=inactive\n"), nil
		}
		return []byte("LoadState=loaded\nActiveState=" + st + "\n"), nil
	}
	if f.gate != nil && c.Args[0] == "kairos-agent" {
		<-f.gate
	}
	if f.rebootHold != nil && len(c.Args) > 1 && c.Args[0] == "systemctl" && c.Args[1] == "reboot" {
		f.mu.Lock()
		f.ran = append(f.ran, c.Args)
		f.mu.Unlock()
		<-f.rebootHold
		return nil, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, c.Args)
	switch c.Args[0] {
	case "mount":
		if f.roErr != nil && c.Args[2] == "remount,ro" {
			return nil, f.roErr
		}
		return nil, f.remountErr
	case "grub2-editenv":
		if f.editErr != nil {
			return nil, f.editErr
		}
		return nil, editEnv(c.Args[1], c.Args[3:])
	case "systemctl":
		if len(c.Args) > 1 && c.Args[1] == "reboot" {
			if f.rebootErr != nil && f.editAfterReboot != nil {
				f.editErr = f.editAfterReboot
			}
			return nil, f.rebootErr
		}
		return nil, nil
	case "journalctl":
		return []byte("the boot's journal\n"), nil
	case "systemd-analyze":
		return nil, f.calendarErr
	case "kairos-agent":
	default:
		return nil, nil
	}
	if d := dockerConfig(c.Env); d != "" {
		raw, _ := os.ReadFile(filepath.Join(d, "config.json"))
		f.config = string(raw)
	}
	_, _ = io.WriteString(c.Stream, f.out)
	return nil, f.err
}

// editEnv does `grub2-editenv file set kv...` on a plain file.
func editEnv(file string, kv []string) error {
	vars, err := assessment{dir: filepath.Dir(file)}.read()
	if err != nil {
		return err
	}
	for _, s := range kv {
		k, v, _ := strings.Cut(s, "=")
		vars[k] = v
	}
	var b strings.Builder
	b.WriteString("# GRUB Environment Block\n")
	for _, k := range sortedKeys(vars) {
		fmt.Fprintf(&b, "%s=%s\n", k, vars[k])
	}
	return os.WriteFile(file, []byte(b.String()), 0o644)
}

func (f *fakeHost) calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.ran...)
}

// daemon is a host daemon over e's install, carrying e's release.
func daemon(e *env, host *fakeHost) *Hostd {
	o := e.installer(vm).O
	return &Hostd{R: e.r, O: o, Host: host, Unit: time.Millisecond, Run: e.t.TempDir(), State: e.t.TempDir(), Networkd: e.t.TempDir(), Issue: e.t.TempDir(), InstallNet: filepath.Join(e.t.TempDir(), "install-network.yaml"), Systemd: e.t.TempDir()}
}

// call asks h and decodes its JSON answer into out; it returns the status.
func call(t *testing.T, h http.Handler, method, path string, body any, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, rd))
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, w.Body)
		}
	}
	return w.Code
}

// errOf is an error answer's message.
type errOf struct{ Error string }

func TestStatusBeforeAndAfterTheInstall(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	h := daemon(e, &fakeHost{}).Handler()
	var st Status
	if code := call(t, h, "GET", "/v1/status", nil, &st); code != 200 || st.Installed != nil || st.OS != (Version{"1.0.0", 1}) || st.Job != "idle" {
		t.Fatalf("%d %+v", code, st)
	}
	var e409 errOf
	if code := call(t, h, "GET", "/v1/services", nil, &e409); code != 409 || !strings.Contains(e409.Error, "not installed yet") {
		t.Errorf("services before the install: %d %s", code, e409.Error)
	}
	if err := e.install(func(o *Options) { vm(o); o.Channel = "beta" }); err != nil {
		t.Fatal(err)
	}
	if code := call(t, h, "GET", "/v1/status", nil, &st); code != 200 || st.Installed == nil || *st.Installed != (Version{"1.0.0", 1}) || st.Channel != "beta" || st.Installing {
		t.Fatalf("%d %+v", code, st)
	}
	// A first install not finished: under way (an empty marker), then failed (why in it).
	marker := filepath.Join(e.dir, installingFile)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	st = Status{}
	if code := call(t, h, "GET", "/v1/status", nil, &st); code != 200 || st.Installed == nil || !st.Installing || st.InstallFailed != "" {
		t.Fatalf("under way: %d %+v", code, st)
	}
	if err := os.WriteFile(marker, []byte("failed: the stack did not come up\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st = Status{}
	if code := call(t, h, "GET", "/v1/status", nil, &st); code != 200 || !st.Installing || st.InstallFailed != "the stack did not come up" {
		t.Fatalf("failed: %d %+v", code, st)
	}
	// The boot runs the install again: the failure recorded before is not the last word.
	hb := &fakeHost{boot: "activating"}
	st = Status{}
	if code := call(t, daemon(e, hb).Handler(), "GET", "/v1/status", nil, &st); code != 200 || !st.Installing || st.InstallFailed != "" {
		t.Fatalf("retried by the boot: %d %+v", code, st)
	}
}

func TestStatusTellsABootThatFailedBeforeWritingTheInstall(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	for boot, failed := range map[string]bool{"failed": true, "active": false, "activating": false} {
		var st Status
		if code := call(t, daemon(e, &fakeHost{boot: boot}).Handler(), "GET", "/v1/status", nil, &st); code != 200 || st.Installed != nil || (st.InstallFailed != "") != failed {
			t.Errorf("%s: %d %+v", boot, code, st)
		}
	}
}

func TestStatusNamesTheSystemKairosBooted(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	d := daemon(e, &fakeHost{})
	d.O.Cos = t.TempDir()
	h := d.Handler()
	var st Status
	if call(t, h, "GET", "/v1/status", nil, &st); st.Boot != "" {
		t.Errorf("boot %q with no mark", st.Boot)
	}
	for _, m := range []string{"recovery", "passive", "active"} {
		if err := os.WriteFile(filepath.Join(d.O.Cos, m+"_mode"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		st = Status{}
		if call(t, h, "GET", "/v1/status", nil, &st); st.Boot != m {
			t.Errorf("boot %q, want %s", st.Boot, m)
		}
		if err := os.Remove(filepath.Join(d.O.Cos, m+"_mode")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestServicesReadsEitherComposeFormat(t *testing.T) {
	t.Parallel()
	for name, out := range map[string]string{
		"lines": `{"Service":"api","State":"running","Health":"healthy","Image":"a"}` + "\n" + `{"Service":"web","State":"exited","Health":"","Image":"w","ExitCode":3}` + "\n",
		"array": `[{"Service":"api","State":"running","Health":"healthy","Image":"a"},{"Service":"web","State":"exited","Image":"w","ExitCode":3}]`,
	} {
		e := vmEnv(t, "1.0.0")
		if err := e.install(vm); err != nil {
			t.Fatal(err)
		}
		e.docker.answer = func(a string) ([]byte, bool, error) {
			return []byte(out), strings.HasSuffix(a, "ps --all --format json"), nil
		}
		var list []Service
		if code := call(t, daemon(e, &fakeHost{}).Handler(), "GET", "/v1/services", nil, &list); code != 200 {
			t.Fatalf("%s: %d", name, code)
		}
		want := []Service{{"api", "running", "healthy", "a", 0}, {"web", "exited", "", "w", 3}}
		if fmt.Sprint(list) != fmt.Sprint(want) {
			t.Errorf("%s: %+v", name, list)
		}
	}
}

// installedVM is a VM install of 1.0.0 whose stack has the services api, web and postgres.
func installedVM(t *testing.T) *env {
	t.Helper()
	e := vmEnv(t, "1.0.0")
	if err := e.install(vm); err != nil {
		t.Fatal(err)
	}
	e.docker.answer = func(a string) ([]byte, bool, error) {
		switch {
		case strings.HasSuffix(a, "config --services"):
			return []byte("api\nweb\npostgres\n"), true, nil
		case strings.Contains(a, " logs "):
			return []byte("a line\n"), true, nil
		}
		return nil, false, nil
	}
	return e
}

func TestLogsTakeAKnownServiceAndABoundedTail(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	h := daemon(e, &fakeHost{}).Handler()
	for path, want := range map[string]int{
		"/v1/services/api/logs":           200,
		"/v1/services/api/logs?tail=1":    200,
		"/v1/services/api/logs?tail=5000": 200,
		"/v1/services/api/logs?tail=0":    400,
		"/v1/services/api/logs?tail=5001": 400,
		"/v1/services/api/logs?tail=x":    400,
		"/v1/services/nope/logs":          404,
		"/v1/services/-x/logs":            400,
	} {
		if code := call(t, h, "GET", path, nil, nil); code != want {
			t.Errorf("%s: %d, want %d", path, code, want)
		}
	}
	var out struct{ Logs string }
	call(t, h, "GET", "/v1/services/web/logs?tail=7", nil, &out)
	if out.Logs != "a line\n" {
		t.Errorf("logs = %q", out.Logs)
	}
	got := e.docker.ran("compose")
	if last := strings.Join(got[len(got)-1].Args, " "); !strings.HasSuffix(last, "logs --no-color --tail 7 web") {
		t.Errorf("ran %s", last)
	}
}

func TestServiceActions(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	h := daemon(e, &fakeHost{}).Handler()
	for action, args := range map[string]string{"restart": "restart --timeout 30 api", "stop": "stop --timeout 30 api", "start": "start api"} {
		if code := call(t, h, "POST", "/v1/services/api/"+action, nil, nil); code != 200 {
			t.Errorf("%s: %d", action, code)
		}
		got := e.docker.ran("compose")
		if last := strings.Join(got[len(got)-1].Args, " "); !strings.HasSuffix(last, " "+args) {
			t.Errorf("%s ran %s", action, last)
		}
	}
	if code := call(t, h, "POST", "/v1/services/api/kill", nil, nil); code != 404 {
		t.Errorf("an unknown action: %d", code)
	}
	if code := call(t, h, "POST", "/v1/services/nope/restart", nil, nil); code != 404 {
		t.Errorf("an unknown service: %d", code)
	}
}

// vmPublished is a VM install of 1.0.0 whose registry is an in-process one, where 1.0.0 and the
// releases a test publishes are, each with an OS bundle.
func vmPublished(t *testing.T, pull spec.Provider, bake *payload.Account) *pubEnv {
	t.Helper()
	p := published(t, pull, bake)
	p.r, p.self = bundledInstaller(t, "1.0.0", 1, p.spec)
	p.r.Pull = bake
	return p
}

// release publishes version at revision on stable with an OS bundle, and moves stable's pointer
// to it: the tag's manifest, as a publisher re-tags it.
func (p *pubEnv) release(version string, revision int, mm func(*release.Manifest)) *release.Manifest {
	var out *release.Manifest
	p.publish(version, revision, func(m *release.Manifest) {
		m.Artifacts.OS = "sha256:" + strings.Repeat("0", 64)
		if mm != nil {
			mm(m)
		}
		out = m
	}, nil)
	p.retag(fmt.Sprintf("%s-%d", version, revision), "stable")
	return out
}

// retag points the release repository's tag to at the manifest from holds.
func (p *pubEnv) retag(from, to string) {
	t := p.t
	t.Helper()
	src, err := name.NewTag(p.prefix + "/notes-release:" + from)
	if err != nil {
		t.Fatal(err)
	}
	d, err := remote.Get(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Tag(src.Context().Tag(to), d); err != nil {
		t.Fatal(err)
	}
}

func TestUpdatesFindTheChannelsLatestOrAVersion(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.release("2.0.0", 1, func(m *release.Manifest) { m.Changelog.Project = "### Added\n- More.\n" })
	h := daemon(p.env, &fakeHost{}).Handler()
	var u Update
	if code := call(t, h, "GET", "/v1/updates?channel=stable", nil, &u); code != 200 {
		t.Fatalf("%d", code)
	}
	if !u.Available || u.Target != (Version{"2.0.0", 1}) || u.Current != (Version{"1.0.0", 1}) || !strings.Contains(u.Changelog, "More.") {
		t.Errorf("%+v", u)
	}
	if code := call(t, h, "GET", "/v1/updates?channel=stable&version=1.0.0", nil, &u); code != 200 || u.Available {
		t.Errorf("1.0.0: %d %+v", code, u)
	}
	var e errOf
	if code := call(t, h, "GET", "/v1/updates", nil, &e); code != 400 || !strings.Contains(e.Error, "follows no channel") {
		t.Errorf("no channel: %d %s", code, e.Error)
	}
	if code := call(t, h, "GET", "/v1/updates?channel=Bad!", nil, nil); code != 400 {
		t.Errorf("a bad channel: %d", code)
	}
	if code := call(t, h, "GET", "/v1/updates?channel=nightly", nil, nil); code != 502 {
		t.Errorf("a channel with no release: %d", code)
	}
}

// A VM whose release carries trusted keys finds only a release signed by one of them, by the
// channel's pointer or by version.
func TestUpdatesNeedASignatureByTheOSKeys(t *testing.T) {
	t.Parallel()
	k, other := testutil.NewKeySigner(t), testutil.NewKeySigner(t)
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.r.Keys = map[string][]byte{"release.pub": k.PEM}
	p.release("2.0.0", 1, nil)
	h := daemon(p.env, &fakeHost{}).Handler()
	for _, q := range []string{"channel=stable", "channel=stable&version=2.0.0"} {
		var e errOf
		if code := call(t, h, "GET", "/v1/updates?"+q, nil, &e); code != 502 || !strings.Contains(e.Error, "notes 2.0.0 (revision 1, 2.0.0-1) is not signed by a key this install trusts") {
			t.Errorf("%s, unsigned: %d %s", q, code, e.Error)
		}
	}
	p.signRelease("2.0.0-1", other)
	if code := call(t, h, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, nil); code != 502 {
		t.Errorf("an upgrade to a release signed by another key: %d", code)
	}
	p.signRelease("2.0.0-1", k) // the pointer is the same manifest
	for _, q := range []string{"channel=stable", "channel=stable&version=2.0.0"} {
		var u Update
		if code := call(t, h, "GET", "/v1/updates?"+q, nil, &u); code != 200 || !u.Available || u.Target != (Version{"2.0.0", 1}) {
			t.Errorf("%s, signed: %d %+v", q, code, u)
		}
	}
	// A pointer left on a publication its tag no longer holds (rebuilt since) is no pointer,
	// though validly signed.
	p.publish("2.0.0", 1, func(m *release.Manifest) { m.Artifacts.OS = "sha256:" + strings.Repeat("1", 64) }, nil)
	p.signRelease("2.0.0-1", k)
	var e errOf
	if code := call(t, h, "GET", "/v1/updates?channel=stable", nil, &e); code != 502 || !strings.Contains(e.Error, "which its tag no longer holds") {
		t.Errorf("a stale pointer: %d %s", code, e.Error)
	}
	// A pointer on a yanked release is none either.
	p.publish("2.0.0", 2, func(m *release.Manifest) { m.Artifacts.OS = "sha256:" + strings.Repeat("2", 64) }, &release.Yank{Reason: "broken", Date: time.Now()})
	p.signRelease("2.0.0-2", k)
	p.retag("2.0.0-2", "stable")
	if code := call(t, h, "GET", "/v1/updates?channel=stable", nil, &e); code != 502 || !strings.Contains(e.Error, "is yanked: broken") {
		t.Errorf("a yanked pointer: %d %s", code, e.Error)
	}
}

// waitJob waits for h's job to leave running.
func waitJob(t *testing.T, h http.Handler) JobStatus {
	t.Helper()
	for range 2000 {
		var j JobStatus
		call(t, h, "GET", "/v1/job", nil, &j)
		if j.State != "running" {
			return j
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the job did not end")
	return JobStatus{}
}

func TestUpgradeWritesTheOSByDigestWithThePullAccountThenReboots(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "token", Config: map[string]any{"user": "pull", "token_env": "NOTES_PULL_TOKEN"}}, &payload.Account{User: "pull", Password: "baked"})
	m := p.release("2.0.0", 1, nil)
	host := &fakeHost{out: "pulling\nwritten"}
	h := daemon(p.env, host)
	hh := h.Handler()
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	j := waitJob(t, hh)
	if j.State != "done" || j.Target != "2.0.0-1" {
		t.Fatalf("%+v", j)
	}
	if want := "oci:" + p.prefix + "/notes-os@" + m.Artifacts.OS; !strings.Contains(fmt.Sprint(host.calls()), "kairos-agent upgrade --source "+want) {
		t.Errorf("ran %v, want %s", host.calls(), want)
	}
	if !strings.Contains(host.config, `"auth"`) || !strings.Contains(host.config, strings.Split(p.prefix, "/")[0]) {
		t.Errorf("kairos-agent pulled with %s", host.config)
	}
	if log := strings.Join(j.Log, "\n"); !strings.Contains(log, "pulling\nwritten") || !strings.HasSuffix(log, "rebooting into 2.0.0-1 in 30 s: the boot upgrades the install") {
		t.Errorf("log %v", j.Log)
	}
	if j.Reboot == nil || *j.Reboot != (EventReboot{"reboot", 30}) {
		t.Errorf("the job's reboot: %+v", j.Reboot)
	}
	// The reboot comes after the drain, once; no other job starts meanwhile.
	for range 2000 {
		if strings.Contains(fmt.Sprint(host.calls()), "systemctl reboot") {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(fmt.Sprint(host.calls()), "systemctl reboot") {
		t.Fatalf("no reboot: %v", host.calls())
	}
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, nil); code != 409 {
		t.Errorf("an upgrade after the reboot was scheduled: %d", code)
	}
	var st Status
	call(t, hh, "GET", "/v1/status", nil, &st)
	if !st.RebootPending || st.Job != "done" {
		t.Errorf("%+v", st)
	}
}

func TestUpgradeWithoutRebootLeavesTheVMRunning(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.release("2.0.0", 1, nil)
	host := &fakeHost{}
	hh := daemon(p.env, host).Handler()
	no := false
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", Reboot: &no}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	if j := waitJob(t, hh); j.State != "done" || !strings.Contains(strings.Join(j.Log, "\n"), "next boot runs it") {
		t.Fatalf("%+v", j)
	}
	// Its end schedules nothing, and says so.
	if e := ofKind(held(t, p.env.dir), evJobEnded); len(e) != 1 || e[0].State != "done" || e[0].Reboot != nil {
		t.Errorf("job.ended %+v", e)
	}
	time.Sleep(50 * time.Millisecond)
	if strings.Contains(fmt.Sprint(host.calls()), "systemctl") {
		t.Errorf("rebooted: %v", host.calls())
	}
	if host.config != "" {
		t.Errorf("a public registry got a Docker config: %s", host.config)
	}
}

func TestUpgradeRefusals(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		publish func(p *pubEnv)
		body    UpgradeRequest
		want    string
	}{
		"not newer": {func(p *pubEnv) { p.release("0.9.0", 1, nil) }, UpgradeRequest{Channel: "stable"}, "does not come after"},
		"same":      {func(p *pubEnv) { p.release("1.0.0", 1, nil) }, UpgradeRequest{Channel: "stable", Version: "1.0.0"}, "does not come after"},
		"no os":     {func(p *pubEnv) { p.release("2.0.0", 1, func(m *release.Manifest) { m.Artifacts.OS = "" }) }, UpgradeRequest{Channel: "stable"}, "without an OS bundle"},
		"bad body":  {func(p *pubEnv) { p.release("2.0.0", 1, nil) }, UpgradeRequest{}, "follows no channel"},
		"yanked": {func(p *pubEnv) {
			m := p.release("2.0.0", 1, nil)
			m.Yanked = &release.Yank{Reason: "broken", Date: time.Now()}
			p.putRelease("stable", m)
		}, UpgradeRequest{Channel: "stable"}, "yanked: broken"},
		"new revison": {func(p *pubEnv) { p.release("1.0.0", 2, nil) }, UpgradeRequest{Channel: "stable"}, ""},
	} {
		p := vmPublished(t, spec.Provider{Name: "none"}, nil)
		c.publish(p)
		host := &fakeHost{}
		hh := daemon(p.env, host).Handler()
		var e errOf
		code := call(t, hh, "POST", "/v1/upgrade", c.body, &e)
		if c.want == "" {
			if code != 202 {
				t.Errorf("%s: %d %s", name, code, e.Error)
			}
			waitJob(t, hh)
			continue
		}
		if code < 400 || !strings.Contains(e.Error, c.want) {
			t.Errorf("%s: %d %q, want %q", name, code, e.Error, c.want)
		}
		if len(host.calls()) > 0 {
			t.Errorf("%s: ran %v", name, host.calls())
		}
	}
}

func TestOneJobAtATime(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.release("2.0.0", 1, nil)
	host := &fakeHost{gate: make(chan struct{}), err: errors.New("no space left")}
	d := daemon(p.env, host)
	hh := d.Handler()
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	var e errOf
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, &e); code != 409 || !strings.Contains(e.Error, "a job (upgrade) is running") {
		t.Errorf("a second upgrade: %d %s", code, e.Error)
	}
	if code := call(t, hh, "POST", "/v1/reboot", nil, &e); code != 409 {
		t.Errorf("a reboot during the upgrade: %d", code)
	}
	close(host.gate)
	j := waitJob(t, hh)
	if j.State != "failed" || !strings.Contains(j.Error, "no space left") {
		t.Errorf("%+v", j)
	}
	if strings.Contains(fmt.Sprint(host.calls()), "systemctl") {
		t.Errorf("a failed upgrade rebooted: %v", host.calls())
	}
	// A failed job frees the slot; the next upgrade waits for a reboot (kairos-agent may have
	// moved the slots), which empties the daemon's runtime directory.
	host.err = nil
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, nil); code != 409 {
		t.Errorf("after a failure: %d", code)
	}
	if err := os.Remove(filepath.Join(d.Run, osWritten)); err != nil {
		t.Fatal(err)
	}
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, nil); code != 202 {
		t.Errorf("after the reboot: %d", code)
	}
	waitJob(t, hh)
}

func TestJobLogFromALine(t *testing.T) {
	t.Parallel()
	h := &Hostd{}
	hh := h.Handler()
	var j JobStatus
	if call(t, hh, "GET", "/v1/job", nil, &j); j.State != "idle" || j.Log == nil || j.Started != nil {
		t.Errorf("%+v", j)
	}
	started := time.Date(2026, 10, 14, 9, 0, 0, 1, time.UTC)
	h.job = &job{Started: started, Kind: "upgrade", State: "running", Log: []string{"a", "b", "c"}}
	call(t, hh, "GET", "/v1/job?since=1", nil, &j)
	if strings.Join(j.Log, ",") != "b,c" || j.LogTotal != 3 || j.Started == nil || !j.Started.Equal(started) {
		t.Errorf("%+v", j)
	}
	call(t, hh, "GET", "/v1/job?since=9", nil, &j)
	if len(j.Log) != 0 || j.LogTotal != 3 {
		t.Errorf("%+v", j)
	}
	if code := call(t, hh, "GET", "/v1/job?since=-1", nil, nil); code != 400 {
		t.Errorf("since -1: %d", code)
	}
}

func TestPowerTakesABoundedDrainAndHappensOnce(t *testing.T) {
	t.Parallel()
	for drain, want := range map[int]int{-1: 400, 0: 202, 600: 202, 601: 400} {
		host := &fakeHost{}
		hh := (&Hostd{R: testRelease(t, "1.0.0"), O: Options{Dir: t.TempDir()}, Host: host, Unit: time.Millisecond}).Handler()
		if code := call(t, hh, "POST", "/v1/shutdown", PowerRequest{DrainSecs: &drain}, nil); code != want {
			t.Errorf("drain %d: %d, want %d", drain, code, want)
		}
		if want == 202 {
			if code := call(t, hh, "POST", "/v1/reboot", nil, nil); code != 409 {
				t.Errorf("a second: %d", code)
			}
		}
	}
	host := &fakeHost{}
	hh := (&Hostd{R: testRelease(t, "1.0.0"), O: Options{Dir: t.TempDir()}, Host: host, Unit: time.Millisecond}).Handler()
	zero := 0
	call(t, hh, "POST", "/v1/reboot", PowerRequest{DrainSecs: &zero}, nil)
	for range 2000 {
		if len(host.calls()) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if fmt.Sprint(host.calls()) != "[[systemctl reboot]]" {
		t.Errorf("ran %v", host.calls())
	}
	if code := call(t, hh, "POST", "/v1/reboot", map[string]any{"drain": 1}, nil); code != 400 {
		t.Errorf("an unknown field: %d", code)
	}
}

func TestNothingChangesWhileTheBootRuns(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.release("2.0.0", 1, nil)
	p.docker.answer = func(a string) ([]byte, bool, error) {
		return []byte("api\nweb\npostgres\n"), strings.HasSuffix(a, "config --services"), nil
	}
	host := &fakeHost{boot: "activating"}
	hh := daemon(p.env, host).Handler()
	for _, c := range []struct {
		path string
		body any
	}{
		{"/v1/upgrade", UpgradeRequest{Channel: "stable"}},
		{"/v1/services/api/restart", nil},
		{"/v1/provision", nil},
	} {
		var e errOf
		if code := call(t, hh, "POST", c.path, c.body, &e); code != 409 || !strings.Contains(e.Error, "still bringing the install up") {
			t.Errorf("%s: %d %s", c.path, code, e.Error)
		}
	}
	if len(host.calls()) > 0 {
		t.Errorf("ran %v", host.calls())
	}
	host.boot = "active"
	if code := call(t, hh, "POST", "/v1/services/api/restart", nil, nil); code != 200 {
		t.Errorf("after the boot: %d", code)
	}
	// A boot that hangs can still be power-cycled.
	host.boot = "activating"
	if code := call(t, hh, "POST", "/v1/reboot", nil, nil); code != 202 {
		t.Errorf("a reboot during the boot: %d", code)
	}
}

func TestUpgradeRefusesAReleaseOtherThanTheConfirmed(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.release("2.0.0", 2, nil)
	host := &fakeHost{}
	hh := daemon(p.env, host).Handler()
	var e errOf
	for _, other := range []Version{{"2.0.0", 1}, {"3.0.0", 2}} {
		if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", Confirm: &other}, &e); code != 409 || !strings.Contains(e.Error, "which was confirmed") {
			t.Fatalf("%v: %d %s", other, code, e.Error)
		}
	}
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", Confirm: &Version{"2.0.0", 2}}, nil); code != 202 {
		t.Fatalf("the confirmed revision: %d", code)
	}
	waitJob(t, hh)
}

func TestTheReleaseARollbackLeftIsNotOffered(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.release("2.0.0", 1, nil)
	// The install went back from 2.0.0.
	failed := filepath.Join(p.dir, FailedDir)
	if err := os.MkdirAll(failed, 0o700); err != nil {
		t.Fatal(err)
	}
	f := p.readEnv()
	f.Set("STOWAGE_PROJECT_VERSION", "2.0.0")
	if err := f.WriteFile(filepath.Join(failed, ".env")); err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{}
	hh := daemon(p.env, host).Handler()
	var u Update
	if call(t, hh, "GET", "/v1/updates?channel=stable", nil, &u); u.Available || !u.LeftByRollback {
		t.Errorf("%+v", u)
	}
	var e errOf
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, &e); code != 409 || !strings.Contains(e.Error, "went back from") {
		t.Errorf("%d %s", code, e.Error)
	}
	if len(host.calls()) > 0 {
		t.Errorf("ran %v", host.calls())
	}
}

func TestTheJobLogSplitsProgressLinesAndKeepsItsEnd(t *testing.T) {
	t.Parallel()
	h := &Hostd{}
	j := &job{Kind: "upgrade", State: "running"}
	h.job = j
	w := &jobWriter{h: h, j: j}
	_, _ = io.WriteString(w, "pull 10%\rpull 50%\r\npart")
	_, _ = io.WriteString(w, "ial\n")
	if strings.Join(j.Log, "|") != "pull 10%|pull 50%|partial" {
		t.Errorf("log %q", j.Log)
	}
	// A line is cut at maxLine, whether its end came or not.
	_, _ = io.WriteString(w, strings.Repeat("x", maxLine+1))
	if len(j.Log) != 4 || len(j.Log[3]) != maxLine {
		t.Errorf("unterminated: %d lines", len(j.Log))
	}
	_, _ = io.WriteString(w, "\n"+strings.Repeat("y", 2*maxLine)+"\n")
	if len(j.Log) != 7 || j.Log[4] != "x" || len(j.Log[5]) != maxLine || len(j.Log[6]) != maxLine {
		t.Errorf("terminated: %d lines", len(j.Log))
	}
	// Up to twice maxLog lines are kept; past that, the last maxLog.
	for i := len(j.Log); i < 2*maxLog; i++ {
		h.logf(j, "line %d", i)
	}
	if len(j.Log) != 2*maxLog || j.dropped != 0 {
		t.Fatalf("kept %d, dropped %d", len(j.Log), j.dropped)
	}
	h.logf(j, "last")
	if len(j.Log) != maxLog || j.dropped != maxLog+1 || j.Log[maxLog-1] != "last" {
		t.Fatalf("kept %d, dropped %d", len(j.Log), j.dropped)
	}
	var st JobStatus
	hh := h.Handler()
	call(t, hh, "GET", fmt.Sprintf("/v1/job?since=%d", 2*maxLog), nil, &st)
	if strings.Join(st.Log, "|") != "last" || st.LogTotal != 2*maxLog+1 {
		t.Errorf("%d lines, total %d", len(st.Log), st.LogTotal)
	}
	call(t, hh, "GET", "/v1/job?since=0", nil, &st)
	if len(st.Log) != maxLog || st.Log[0] != fmt.Sprintf("line %d", maxLog+1) {
		t.Errorf("from 0: %d lines from %q", len(st.Log), st.Log[0])
	}
}

func TestAServiceActionIsRefusedUnderTheLifecycleLock(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	unlock, err := lockFile(filepath.Join(e.dir, lifecycleLock))
	if err != nil {
		t.Fatal(err)
	}
	hh := daemon(e, &fakeHost{}).Handler()
	var er errOf
	if code := call(t, hh, "POST", "/v1/services/api/restart", nil, &er); code != 409 || !strings.Contains(er.Error, "is running") {
		t.Errorf("%d %s", code, er.Error)
	}
	// The install is read under the lock: nothing of it is looked at before.
	if code := call(t, hh, "POST", "/v1/services/nope/restart", nil, &er); code != 409 {
		t.Errorf("an unknown service under the lock: %d %s", code, er.Error)
	}
	// So are reboot and shutdown, but for the boot's own run.
	if code := call(t, hh, "POST", "/v1/reboot", nil, &er); code != 409 || !strings.Contains(er.Error, "is running") {
		t.Errorf("reboot: %d %s", code, er.Error)
	}
	unlock()
	if code := call(t, hh, "POST", "/v1/services/api/restart", nil, nil); code != 200 {
		t.Errorf("after: %d", code)
	}
}

func TestTheBootMustHaveRun(t *testing.T) {
	t.Parallel()
	e := installedVM(t)
	host := &fakeHost{}
	hh := daemon(e, host).Handler()
	for state, want := range map[string]int{"inactive": 409, "activating": 409, "deactivating": 409, "active": 200, "failed": 200, noBootUnit: 200} {
		host.mu.Lock()
		host.boot = state
		host.mu.Unlock()
		if code := call(t, hh, "POST", "/v1/services/api/restart", nil, nil); code != want {
			t.Errorf("%s: %d, want %d", state, code, want)
		}
	}
	// A hung boot holds the install's lock: it can still be power-cycled.
	unlock, err := lockFile(filepath.Join(e.dir, lifecycleLock))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	host.mu.Lock()
	host.boot = "activating"
	host.mu.Unlock()
	if code := call(t, hh, "POST", "/v1/shutdown", nil, nil); code != 202 {
		t.Errorf("a shutdown during the boot: %d", code)
	}
}

func TestTheJobLogKeepsBlankLinesAndOneEndPerCRLF(t *testing.T) {
	t.Parallel()
	h := &Hostd{}
	j := &job{Kind: "upgrade", State: "running"}
	w := &jobWriter{h: h, j: j}
	for _, chunk := range []string{"error:\n\ndetails\r", "\nnext\r\n", "a\rb\n"} {
		_, _ = io.WriteString(w, chunk)
	}
	if got := strings.Join(j.Log, "|"); got != "error:||details|next|a|b" {
		t.Errorf("log %q", got)
	}
}

// An upgrade whose reboot cannot be scheduled fails, and its log never announces the reboot.
func TestAnUpgradeAnnouncesItsRebootOnlyOnceScheduled(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.release("2.0.0", 1, nil)
	host := &fakeHost{gate: make(chan struct{})}
	d := daemon(p.env, host)
	hh := d.Handler()
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	// A reboot claimed while the OS is written: the upgrade cannot schedule its own.
	if err := d.claimReboot(false); err != nil {
		t.Fatal(err)
	}
	close(host.gate)
	j := waitJob(t, hh)
	if j.State != "failed" || !strings.Contains(j.Error, "a reboot is scheduled") || j.Reboot != nil {
		t.Errorf("%+v", j)
	}
	if log := strings.Join(j.Log, "\n"); strings.Contains(log, "rebooting into") {
		t.Errorf("announced a reboot not scheduled:\n%s", log)
	}
	if evs := ofKind(held(t, d.O.Dir), evRebootScheduled); len(evs) != 0 {
		t.Errorf("%+v", evs)
	}
}

// The daemon's socket appears at its path listening, with its owner and mode already given; closing
// it removes the path, unless another socket has taken it since.
func TestTheSocketAppearsListeningAndGuarded(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("", "sock") // short: a unix socket's path is bounded
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "host.sock")
	if err := os.WriteFile(path, nil, 0o600); err != nil { // a stale one is replaced
		t.Fatal(err)
	}
	l, err := listen(path, func(p string) error {
		if p == path {
			t.Error("perm given the socket's own path: it is reachable before its mode")
		}
		if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
			t.Error("the socket is at its path before its mode is given")
		}
		if c, err := net.Dial("unix", p); err != nil {
			t.Errorf("not listening once made: %v", err)
		} else {
			c.Close()
		}
		return os.Chmod(p, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("left beside it: %v", entries)
	}
	if l.Addr().String() != path {
		t.Errorf("serving on %s, not %s", l.Addr(), path)
	}
	if c, err := net.Dial("unix", path); err != nil {
		t.Fatal(err)
	} else {
		c.Close()
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("closed, the socket stays: %v", err)
	}
	// A daemon started since holds the path: the first one closing leaves it.
	first, err := listen(path, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	second, err := listen(path, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	first.Close()
	if c, err := net.Dial("unix", path); err != nil {
		t.Errorf("the newer socket was removed: %v", err)
	} else {
		c.Close()
	}
	// perm failing leaves nothing.
	if _, err := listen(filepath.Join(dir, "other.sock"), func(string) error { return errors.New("no group") }); err == nil {
		t.Fatal("perm's failure ignored")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("left after a failure: %v", entries)
	}
}

// Before the first install (none made, or one refused), updates and upgrade read the first
// install's settings: a newer release's OS replaces the one whose install was refused, and the boot
// into it makes the first install of that release.
func TestAnUpgradeBeforeTheFirstInstall(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.release("1.0.0", 2, nil)
	if err := os.RemoveAll(p.env.dir); err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{}
	h := daemon(p.env, host)
	h.O.Settings = filepath.Join(t.TempDir(), "install.yaml")
	hh := h.Handler()
	// No settings: the request names the channel.
	var e errOf
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{}, &e); code != 400 || !strings.Contains(e.Error, "follows no channel") {
		t.Fatalf("no channel: %d %s", code, e.Error)
	}
	if err := os.WriteFile(h.O.Settings, []byte("channel: stable\nregistry: "+p.prefix+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "beta"}, &e); code != 409 || !strings.Contains(e.Error, "the first install follows stable (its settings)") {
		t.Fatalf("another channel: %d %s", code, e.Error)
	}
	var u Update
	if code := call(t, hh, "GET", "/v1/updates", nil, &u); code != 200 || !u.Available || u.Target != (Version{"1.0.0", 2}) {
		t.Fatalf("updates: %d %+v", code, u)
	}
	// An operator's first install holding the lock, .env not written yet.
	if err := os.MkdirAll(p.env.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockFile(filepath.Join(p.env.dir, lifecycleLock))
	if err != nil {
		t.Fatal(err)
	}
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{}, &e); code != 409 || !strings.Contains(e.Error, "an install is running") {
		t.Fatalf("an install running: %d %s", code, e.Error)
	}
	unlock()
	no := false
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Reboot: &no}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	j := waitJob(t, hh)
	if log := strings.Join(j.Log, "\n"); j.State != "done" || !strings.Contains(log, "the next boot runs it and makes the first install") || strings.Contains(log, "backup") {
		t.Fatalf("%+v", j)
	}
	if !strings.Contains(fmt.Sprint(host.calls()), "kairos-agent upgrade --source oci:"+p.prefix+"/notes-os@") {
		t.Errorf("ran %v", host.calls())
	}
	if raw, err := os.ReadFile(osUpgradeFile(p.env.dir)); err != nil || string(raw) != "1.0.0-2\n" {
		t.Errorf("mark: %q %v", raw, err)
	}
	// A Docker host's daemon has no OS to write: still the install's alone.
	h.Docker = true
	if code := call(t, h.Handler(), "GET", "/v1/updates", nil, &e); code != 409 || !strings.Contains(e.Error, "not installed yet") {
		t.Errorf("Docker: %d %s", code, e.Error)
	}
}
