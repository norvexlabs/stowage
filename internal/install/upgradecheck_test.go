package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
)

// releaseOf publishes version, revision 1, on stable with an OS bundle, its spec changed by mod.
func (p *pubEnv) releaseOf(version string, mod func(*spec.Spec)) {
	t := p.t
	t.Helper()
	tag := version + "-1"
	r, exe := testInstallerKeys(t, version, 1, func(s *spec.Spec) { p.spec(s); mod(s) }, func(m *release.Manifest) {
		m.Tag, m.Artifacts.OS = tag, "sha256:"+strings.Repeat("0", 64)
	}, p.keys)
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	m := *r.Manifest
	m.Artifacts.Installer = p.pushInstaller("notes-stow", raw)
	p.putRelease(tag, &m)
	p.retag(tag, "stable")
}

// The daemon refuses an upgrade the boot into the release would refuse before it writes its OS:
// the release's installer is fetched, its spec read, and the upgrade's checks run given no input.
func TestUpgradeRefusesWhatTheBootWouldBeforeWritingTheOS(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		mod  func(*spec.Spec)
		want string
	}{
		"a route on another's port": {docsRoute(8080), "notes 2.0.0 adds the route docs on port 8080, which app has"},
		"an input nothing gives": {func(s *spec.Spec) {
			s.Inputs = append(s.Inputs, spec.Input{Key: "SMTP_HOST", Prompt: "Mail host", Required: true})
		}, "missing required input(s), set by --set KEY=VALUE, the environment or --answers: SMTP_HOST (Mail host)"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := vmPublished(t, spec.Provider{Name: "none"}, nil)
			p.releaseOf("2.0.0", c.mod)
			host := &fakeHost{}
			hh := daemon(p.env, host).Handler()
			var e errOf
			if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, &e); code != 409 || !strings.Contains(e.Error, c.want) ||
				!strings.HasSuffix(e.Error, "the boot into 2.0.0-1 would refuse this upgrade (it is given no input), so its OS is not written") {
				t.Fatalf("%d %s", code, e.Error)
			}
			if calls := fmt.Sprint(host.calls()); strings.Contains(calls, "kairos-agent") {
				t.Errorf("the OS was written: %v", calls)
			}
			if left, _ := filepath.Glob(filepath.Join(p.env.dir, NextDir+"-*")); len(left) > 0 {
				t.Errorf("the fetched installer is left: %v", left)
			}
		})
	}
}

// A release the boot would install is written: the route it adds on a port of its own.
func TestUpgradeWritesTheOSOfARouteTheBootAdds(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.releaseOf("2.0.0", docsRoute(9000))
	host := &fakeHost{}
	hh := daemon(p.env, host).Handler()
	no := false
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", Reboot: &no}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	if j := waitJob(t, hh); j.State != "done" || !strings.Contains(fmt.Sprint(host.calls()), "kairos-agent upgrade") {
		t.Fatalf("%+v %v", j, host.calls())
	}
	if left, _ := filepath.Glob(filepath.Join(p.env.dir, NextDir+"-*")); len(left) > 0 {
		t.Errorf("the fetched installer is left: %v", left)
	}
}

// An upgrade across a schema baseline is refused before its OS is written, naming the verb that
// crosses it: the boot into it would refuse it, and the health gate go back.
func TestUpgradeAcrossABaselineIsRefusedBeforeWritingTheOS(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.releaseOf("2.0.0", atBaseline(2))
	host := &fakeHost{}
	hh := daemon(p.env, host).Handler()
	var e errOf
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, &e); code != 409 ||
		!strings.Contains(e.Error, "notes 2.0.0 starts schema baseline 2, and the install runs notes 1.0.0, at baseline 1: its data cannot carry over, so its OS is not written; `notes-stow reset-db --new-baseline --version 2.0.0` wipes the data and installs it") {
		t.Fatalf("%d %s", code, e.Error)
	}
	if calls := fmt.Sprint(host.calls()); strings.Contains(calls, "kairos-agent") {
		t.Errorf("the OS was written: %v", calls)
	}
}

// reset-db --new-baseline on the VM writes the later release's OS and the mark its boot crosses
// the baseline by; an upgrade written after it removes the mark.
func TestTheDaemonResetsToANewBaselineByItsOS(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.releaseOf("2.0.0", atBaseline(2))
	host := &fakeHost{}
	d := daemon(p.env, host)
	hh := d.Handler()
	var e errOf
	if code := call(t, hh, "GET", "/v1/reset-db?channel=stable", nil, &e); code != 400 || !strings.Contains(e.Error, "names the release new_baseline installs") {
		t.Fatalf("a channel without new_baseline: %d %s", code, e.Error)
	}
	// The token of a plain reset's plan crosses nothing: it shows another plan.
	var plain ResetPlan
	if code := call(t, hh, "GET", "/v1/reset-db", nil, &plain); code != 200 {
		t.Fatalf("plain plan: %d", code)
	}
	if code := call(t, hh, "POST", "/v1/reset-db", ResetRequest{Confirm: &Version{"2.0.0", 1}, Token: plain.Token, NewBaseline: true, Channel: "stable"}, &e); code != 409 || !strings.Contains(e.Error, "showed another plan") {
		t.Fatalf("a plain plan's token: %d %s", code, e.Error)
	}
	if strings.Contains(fmt.Sprint(host.calls()), "kairos-agent") {
		t.Fatal("the OS was written")
	}
	var plan ResetPlan
	if code := call(t, hh, "GET", "/v1/reset-db?new_baseline=true&channel=stable", nil, &plan); code != 200 || plan.Target != (Version{"2.0.0", 1}) ||
		!strings.Contains(plan.Plan, "write 2.0.0-1's OS and reboot into it; that boot stops notes, deletes its volumes postgres, uploads, forgets") ||
		!strings.Contains(plan.Plan, "at schema baseline 2 (from 1)") {
		t.Fatalf("plan: %d %+v %s", code, plan, e.Error)
	}
	if code := call(t, hh, "POST", "/v1/reset-db", ResetRequest{Confirm: &plan.Target, Token: plan.Token, NewBaseline: true, NoProvision: true}, &e); code != 400 || !strings.Contains(e.Error, "no_provision is not given to the boot") {
		t.Fatalf("no_provision: %d %s", code, e.Error)
	}
	// The release the OS carries is not what a new baseline confirms.
	if code := call(t, hh, "POST", "/v1/reset-db", ResetRequest{Confirm: &Version{"1.0.0", 1}, Token: plan.Token, NewBaseline: true, Channel: "stable"}, &e); code != 409 || !strings.Contains(e.Error, "which was confirmed") {
		t.Fatalf("confirmed the OS's release: %d %s", code, e.Error)
	}
	no := false
	fetched := func() int {
		p.mu.Lock()
		defer p.mu.Unlock()
		n := 0
		for _, path := range p.paths {
			if strings.Contains(path, "notes-installer/blobs/") {
				n++
			}
		}
		return n
	}
	planned := fetched()
	if planned == 0 {
		t.Fatal("the plan read no installer")
	}
	if code := call(t, hh, "POST", "/v1/reset-db", ResetRequest{Confirm: &plan.Target, Token: plan.Token, NewBaseline: true, Channel: "stable"}, &e); code != 202 {
		t.Fatalf("%d %s", code, e.Error)
	}
	// The plan's check is taken as it is: the target's installer is not fetched again.
	if n := fetched(); n != planned {
		t.Errorf("the POST fetched the installer again: %d reads, %d by the plan", n, planned)
	}
	j := waitJob(t, hh)
	if !strings.Contains(fmt.Sprint(host.calls()), "kairos-agent upgrade") {
		t.Fatalf("%+v %v", j, host.calls())
	}
	mark := newBaselineFile(p.env.dir)
	if raw, err := os.ReadFile(mark); err != nil || strings.TrimSpace(string(raw)) != "2.0.0-1" {
		t.Fatalf("mark %q %v (job %+v)", raw, err, j)
	}
	if n := len(p.env.docker.ran("volume rm")); n != 0 {
		t.Errorf("wiped before the boot into the new release")
	}
	// Within one baseline the flag is refused: an upgrade moves it.
	q := vmPublished(t, spec.Provider{Name: "none"}, nil)
	q.releaseOf("2.0.0", atBaseline(1))
	if code := call(t, daemon(q.env, &fakeHost{}).Handler(), "GET", "/v1/reset-db?new_baseline=true&channel=stable", nil, &e); code != 409 || !strings.Contains(e.Error, "--new-baseline only crosses a baseline") {
		t.Fatalf("same baseline: %d %s", code, e.Error)
	}
	// An upgrade written instead takes the mark away: it crosses no baseline.
	q.releaseOf("2.1.0", atBaseline(1))
	qm := newBaselineFile(q.env.dir)
	if err := os.WriteFile(qm, []byte("2.0.0-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	qh := daemon(q.env, &fakeHost{}).Handler()
	if code := call(t, qh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", Reboot: &no}, &e); code != 202 {
		t.Fatalf("upgrade: %d %s", code, e.Error)
	}
	if j := waitJob(t, qh); j.State != "done" {
		t.Fatalf("%+v", j)
	}
	if _, err := os.Stat(qm); !os.IsNotExist(err) {
		t.Errorf("the upgrade kept the crossing's mark: %v", err)
	}
}

// On the passive system a failed boot went back to, an upgrade to the release it runs writes that
// release's OS to the active slot again; on the active system it is no upgrade.
func TestUpgradeRewritesTheActiveSlotFromThePassiveSystem(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"active", "passive"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			p := vmPublished(t, spec.Provider{Name: "none"}, nil)
			p.releaseOf("1.0.0", func(*spec.Spec) {})
			host := &fakeHost{}
			d := daemon(p.env, host)
			boots(t, d, mode, nil)
			hh := d.Handler()
			var u Update
			if code := call(t, hh, "GET", "/v1/updates?channel=stable", nil, &u); code != 200 || u.Available != (mode == "passive") || u.Rewrite != (mode == "passive") {
				t.Fatalf("%d %+v", code, u)
			}
			no := false
			var e errOf
			code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", Reboot: &no}, &e)
			if mode == "active" {
				if code != 409 || !strings.Contains(e.Error, "does not come after") {
					t.Fatalf("%d %s", code, e.Error)
				}
				return
			}
			if code != 202 {
				t.Fatalf("%d %s", code, e.Error)
			}
			if j := waitJob(t, hh); j.State != "done" || !strings.Contains(fmt.Sprint(host.calls()), "kairos-agent upgrade") {
				t.Fatalf("%+v %v", j, host.calls())
			}
		})
	}
}

// On the passive system, updates names the rewrite of the running release, whatever the channel
// offers (most often the release that failed, which is not offered again).
func TestUpdatesNameTheRewriteOnThePassiveSystem(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	printUpdate(&b, Update{Channel: "stable", Current: Version{"1.0.0", 1}, Target: Version{"1.0.1", 1}, LeftByRollback: true, Passive: true})
	if out := b.String(); !strings.Contains(out, "the install went back from 1.0.1") ||
		!strings.Contains(out, "`upgrade --version 1.0.0` writes 1.0.0's latest revision (1.0.0 (revision 1) again when it is that one) to the active slot") {
		t.Errorf("%s", out)
	}
	// After a crossing whose boot fell back, the install runs the later release: its OS is named.
	b.Reset()
	printUpdate(&b, Update{Channel: "stable", Current: Version{"1.0.0", 1}, Target: Version{"2.0.0", 1}, LeftByRollback: true, Passive: true, Installed: &Version{"2.0.0", 1}})
	if out := b.String(); !strings.Contains(out, "the install runs 2.0.0 (revision 1), which this OS (1.0.0 (revision 1)) does not carry: `upgrade --version 2.0.0` writes its OS") || strings.Contains(out, "--version 1.0.0") {
		t.Errorf("%s", out)
	}
	b.Reset()
	printUpdate(&b, Update{Channel: "stable", Current: Version{"1.0.0", 1}, Target: Version{"1.0.0", 1}, Available: true, Rewrite: true, Passive: true})
	if out := b.String(); strings.Count(out, "active slot again") != 1 {
		t.Errorf("%s", out)
	}
}

// What the boot's crossing would wipe is told before the OS is written: a data.reset the target's
// compose file does not declare is refused by the plan.
func TestTheVMsNewBaselineRefusesAWipeItCannotTell(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.releaseOf("2.0.0", func(s *spec.Spec) {
		atBaseline(2)(s)
		s.Data.Reset = append(s.Data.Reset, "nowhere")
	})
	var e errOf
	if code := call(t, daemon(p.env, &fakeHost{}).Handler(), "GET", "/v1/reset-db?new_baseline=true&channel=stable", nil, &e); code != 409 ||
		!strings.Contains(e.Error, `"nowhere" is no volume of the compose file`) {
		t.Fatalf("%d %s", code, e.Error)
	}
}

// After a crossing whose boot fell back, the install runs the newer release on the older OS: an
// upgrade to that release only writes its OS again, which no baseline check refuses.
func TestAnUpgradeToTheReleaseTheInstallRunsIsNoCrossing(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.releaseOf("2.0.0", atBaseline(2))
	env := p.env.readEnv()
	env.Set("STOWAGE_PROJECT_VERSION", "2.0.0")
	if err := env.WriteFile(filepath.Join(p.env.dir, envName)); err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{}
	hh := daemon(p.env, host).Handler()
	no := false
	var e errOf
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", Reboot: &no}, &e); code != 202 {
		t.Fatalf("%d %s", code, e.Error)
	}
	if j := waitJob(t, hh); j.State != "done" || !strings.Contains(fmt.Sprint(host.calls()), "kairos-agent upgrade") {
		t.Fatalf("%+v", j)
	}
}

// On the passive system with an install of a later release (a crossing whose boot fell back),
// writing the passive system's own release again would not run the install: no rewrite.
func TestNoRewriteOfAReleaseTheInstallDoesNotRun(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.releaseOf("1.0.0", func(*spec.Spec) {})
	env := p.env.readEnv()
	env.Set("STOWAGE_PROJECT_VERSION", "2.0.0")
	if err := env.WriteFile(filepath.Join(p.env.dir, envName)); err != nil {
		t.Fatal(err)
	}
	d := daemon(p.env, &fakeHost{})
	boots(t, d, "passive", nil)
	var u Update
	if code := call(t, d.Handler(), "GET", "/v1/updates?channel=stable", nil, &u); code != 200 || u.Rewrite || u.Available || u.Installed == nil || *u.Installed != (Version{"2.0.0", 1}) {
		t.Fatalf("%d %+v", code, u)
	}
}

// After a crossing whose boot fell back, the data is at the install's release's baseline, not the
// OS's: an upgrade to a later release at that baseline is no crossing.
func TestAnUpgradeFromACrossedInstallUsesItsBaseline(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	_, crossed := testInstallerWith(t, "2.0.0", func(s *spec.Spec) { p.spec(s); atBaseline(2)(s) })
	if err := copyFile(crossed, filepath.Join(p.env.dir, "notes-stow"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := p.env.readEnv()
	env.Set("STOWAGE_PROJECT_VERSION", "2.0.0")
	if err := env.WriteFile(filepath.Join(p.env.dir, envName)); err != nil {
		t.Fatal(err)
	}
	p.releaseOf("2.1.0", atBaseline(2))
	host := &fakeHost{}
	hh := daemon(p.env, host).Handler()
	no := false
	var e errOf
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", Reboot: &no}, &e); code != 202 {
		t.Fatalf("%d %s", code, e.Error)
	}
	if j := waitJob(t, hh); j.State != "done" {
		t.Fatalf("%+v", j)
	}
}

// A release whose services ask more whole cores than the VM has beside CPU 0's is refused before
// its OS is written: its boot would fail the start, and the health gate go back.
func TestUpgradeRefusesCoresTheVMLacksBeforeWritingTheOS(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.releaseOf("2.0.0", func(s *spec.Spec) { s.Services = map[string]spec.Service{"api": {CPUs: 2}} })
	root := tuneRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "sys/devices/system/cpu"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sys/devices/system/cpu/online"), []byte("0-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := p.env.docker.answer
	p.env.docker.answer = func(a string) ([]byte, bool, error) {
		if strings.HasPrefix(a, "info --format") {
			return []byte(`27.3.1|Kairos|["name=seccomp,profile=builtin"]` + "\n"), true, nil
		}
		if prev != nil {
			return prev(a)
		}
		return nil, false, nil
	}
	host := &fakeHost{}
	d := daemon(p.env, host)
	d.O.Tune = fakeTune(root, 0)
	var e errOf
	if code := call(t, d.Handler(), "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, &e); code != 409 ||
		!strings.Contains(e.Error, "notes 2.0.0 cannot run here, and nothing was changed: services.*.cpus: api ask 2 whole cores, and this host has 1 beside the one of CPU 0") ||
		!strings.HasSuffix(e.Error, "so its OS is not written") {
		t.Fatalf("%d %s", code, e.Error)
	}
	if calls := fmt.Sprint(host.calls()); strings.Contains(calls, "kairos-agent") {
		t.Errorf("the OS was written: %v", calls)
	}
}
