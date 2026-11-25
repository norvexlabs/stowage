package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
)

// releaseOf publishes version, revision 1, on stable with an OS bundle, its spec changed by mod
// and its manifest by each of mms.
func (p *pubEnv) releaseOf(version string, mod func(*spec.Spec), mms ...func(*release.Manifest)) {
	t := p.t
	t.Helper()
	tag := version + "-1"
	r, exe := testInstallerKeys(t, version, 1, func(s *spec.Spec) { p.spec(s); mod(s) }, func(m *release.Manifest) {
		m.Tag, m.Artifacts.OS = tag, "sha256:"+strings.Repeat("0", 64)
		for _, mm := range mms {
			mm(m)
		}
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
		fix  string // the VM's: none where the fix would be a flag the VM's upgrade does not take
	}{
		"a route on another's port": {docsRoute(8080), "notes 2.0.0 adds the route docs on port 8080, which app has", ""},
		"a recorded input it declares as a file": {func(s *spec.Spec) {
			i := slices.IndexFunc(s.Inputs, func(i spec.Input) bool { return i.Key == "ADMIN_EMAIL" })
			s.Inputs[i].Type = spec.InputFile
		}, "ADMIN_EMAIL: notes 2.0.0 declares it as a file", "stay on the installed release: the project declares a changed input under a new key"},
		"an input nothing gives": {func(s *spec.Spec) {
			s.Inputs = append(s.Inputs, spec.Input{Key: "SMTP_HOST", Prompt: "Mail host", Required: true})
		}, "missing required input(s) the release adds, which the upgrade did not give: SMTP_HOST (Mail host)", vmGivesAdded(onStable)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := vmPublished(t, spec.Provider{Name: "none"}, nil)
			p.releaseOf("2.0.0", c.mod)
			host := &fakeHost{}
			hh := daemon(p.env, host).Handler()
			var e errOf
			// The boot's refusal is the upgrade's, with the fix it was raised with on the VM.
			if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, &e); code != 409 || !strings.Contains(e.Error, c.want) ||
				!strings.HasSuffix(e.Error, "the boot into 2.0.0-1 would refuse this upgrade (it is given no input), so its OS is not written") ||
				!e.Refused || e.Fix != c.fix {
				t.Fatalf("%d %s (fix %q)", code, e.Error, e.Fix)
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

// A release another Stowage published is judged by the boot into it, with that Stowage's rules:
// the daemon's own rules would refuse its route, and do not; the dry run's answer and the job's
// log say the boot judges. Its schema baseline is still checked here (TestUpgradeAcrossABaseline…).
func TestUpgradeOfAnotherStowagesReleaseLeavesItsValuesToTheBoot(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.releaseOf("2.0.0", docsRoute(8080), func(m *release.Manifest) { m.StowageVersion = "9.9.9" })
	host := &fakeHost{}
	hh := daemon(p.env, host).Handler()
	const note = "2.0.0-1 was published by Stowage 9.9.9, and the host daemon is Stowage " + testStowage + "'s: the boot into it judges whether it takes the install's values, with its own rules, and on a refusal falls back to the release the install runs"
	// The daemon's own rules, which would refuse the route, are said beside it.
	const rules = "; the host daemon's rules would refuse it: notes 2.0.0 adds the route docs on port 8080, which app has"
	var plan UpgradePlan
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", DryRun: true}, &plan); code != 200 || !strings.HasPrefix(plan.Unchecked, note+rules) {
		t.Fatalf("%d %+v", code, plan)
	}
	no := false
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", Reboot: &no}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	if j := waitJob(t, hh); j.State != "done" || !slices.Contains(j.Log, plan.Unchecked) || !strings.Contains(fmt.Sprint(host.calls()), "kairos-agent upgrade") {
		t.Fatalf("%+v %v", j, host.calls())
	}
}

// A release the boot would install is written: the route it adds on a port of its own.
func TestUpgradeWritesTheOSOfARouteTheBootAdds(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.releaseOf("2.0.0", docsRoute(9000))
	host := &fakeHost{}
	hh := daemon(p.env, host).Handler()
	// updates tells the upgrade downloads the release's installer to check it.
	var u Update
	if code := call(t, hh, "GET", "/v1/updates?channel=stable", nil, &u); code != 200 || !u.ChecksInstaller {
		t.Fatalf("%d %+v", code, u)
	}
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

// vmGivesAdded is the fix of a VM upgrade missing an input the release adds: the upgrade given
// args (upgradeArgs).
func vmGivesAdded(args string) string {
	return "`sudo notes-stow upgrade" + args + " --set KEY=VALUE` (or --set-file KEY=PATH, --answers FILE) gives each to the boot into the release"
}

// onStable is the arguments of an upgrade to 2.0.0 asked on stable.
const onStable = " --channel stable --version 2.0.0"

// The inputs a release adds are given to the upgrade: judged as the boot takes them, then staged
// root-only for that boot, never logged; a dry run stages nothing.
func TestUpgradeStagesTheInputsTheReleaseAddsForItsBoot(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.releaseOf("2.0.0", func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "SMTP_HOST", Prompt: "Mail host", Required: true},
			spec.Input{Key: "SMTP_PASSWORD", Prompt: "Mail password", Secret: true, Required: true})
	})
	host := &fakeHost{}
	hh := daemon(p.env, host).Handler()
	stagedAt := stagedInputsFile(p.env.dir)
	const secret = "s3cret-mail-password"
	for name, c := range map[string]struct {
		inputs    map[string]string
		want, fix string
	}{
		"one missing":      {map[string]string{"SMTP_HOST": "mail"}, "which the upgrade did not give: SMTP_PASSWORD (Mail password)", vmGivesAdded(onStable)},
		"a recorded input": {map[string]string{"SMTP_HOST": "mail", "SMTP_PASSWORD": secret, "ADMIN_EMAIL": "x@example.org"}, "ADMIN_EMAIL: recorded at the install, and an upgrade keeps it", ""},
		"an unknown input": {map[string]string{"SMTP_HOST": "mail", "SMTP_PASSWORD": secret, "NOPE": "x"}, "no input this release adds is named NOPE (it adds: SMTP_HOST, SMTP_PASSWORD)", "leave out NOPE"},
	} {
		var e errOf
		if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", DryRun: true, Inputs: c.inputs}, &e); code != 409 || !e.Refused ||
			!strings.Contains(e.Error, c.want) || !strings.Contains(e.Error, "(it is given only the inputs the upgrade gives)") || (c.fix != "" && e.Fix != c.fix) {
			t.Errorf("%s: %d %s (fix %q)", name, code, e.Error, e.Fix)
		}
	}
	given := map[string]string{"SMTP_HOST": "mail", "SMTP_PASSWORD": secret}
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", DryRun: true, Inputs: given}, nil); code != 200 {
		t.Fatalf("dry run: %d", code)
	}
	if _, err := os.Stat(stagedAt); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a dry run staged the inputs: %v", err)
	}
	no := false
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", Reboot: &no, Inputs: given}, nil); code != 202 {
		t.Fatalf("%d", code)
	}
	j := waitJob(t, hh)
	if j.State != "done" || strings.Contains(strings.Join(j.Log, "\n"), secret) {
		t.Fatalf("%+v", j)
	}
	fi, err := os.Stat(stagedAt)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("staged: %v %v", fi, err)
	}
	var st staged
	if raw, err := os.ReadFile(stagedAt); err != nil || json.Unmarshal(raw, &st) != nil || st.Release != "2.0.0 1" || !maps.Equal(st.Inputs, given) {
		t.Fatalf("staged %+v: %v", st, err)
	}
}

// Inputs for a release whose boot does not read what an upgrade stages (published by a Stowage
// before 0.43.0, other than the daemon's) are refused, nothing staged; from 0.43.0 they are taken.
func TestUpgradeInputsNeedABootThatTakesThem(t *testing.T) {
	t.Parallel()
	for stowage, refused := range map[string]bool{"0.42.9": true, "": true, "0.43.0": false} {
		p := vmPublished(t, spec.Provider{Name: "none"}, nil)
		p.releaseOf("2.0.0", func(s *spec.Spec) {
			s.Inputs = append(s.Inputs, spec.Input{Key: "SMTP_HOST", Prompt: "Mail host", Required: true})
		}, func(m *release.Manifest) { m.StowageVersion = stowage })
		hh := daemon(p.env, &fakeHost{}).Handler()
		// updates says so first, for a program to tell before it gives any.
		var u Update
		if code := call(t, hh, "GET", "/v1/updates?channel=stable", nil, &u); code != 200 || u.TakesInputs == refused {
			t.Errorf("published by Stowage %q: updates %d %+v", stowage, code, u)
		}
		// Without the input, the daemon's own rules name the way that works: the upgrade's flag where
		// that release takes it, none where it does not.
		if stowage != "" {
			var plan UpgradePlan
			call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", DryRun: true}, &plan)
			if strings.Contains(plan.Unchecked, "upgrade --channel stable --version 2.0.0 --set") == refused ||
				strings.Contains(plan.Unchecked, "whose boot takes no input the upgrade gives: it is reached only by a release that requires none") != refused {
				t.Errorf("published by Stowage %q, no input: %q", stowage, plan.Unchecked)
			}
		}
		var e errOf
		code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", DryRun: true, Inputs: map[string]string{"SMTP_HOST": "mail"}}, &e)
		if got := code == 409 && e.Refused && strings.Contains(e.Error, "whose boot takes no input an upgrade gives: nothing is staged"); got != refused || (!refused && code != 200) {
			t.Errorf("published by Stowage %q: %d %s", stowage, code, e.Error)
		}
	}
}

// The upgrade's body may carry a file input's content: up to maxSecretsFile and 64 KiB, and a
// byte over it is a 413.
func TestTheUpgradesBodyIsBounded(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	hh := daemon(p.env, &fakeHost{}).Handler()
	const limit = maxSecretsFile + 64<<10
	body := func(n int) string {
		head, tail := `{"dry_run":true,"inputs":{"K":"`, `"}}`
		return head + strings.Repeat("x", n-len(head)-len(tail)) + tail
	}
	// The client refuses inputs past what one request carries, before it asks.
	// Measured as sent: JSON escapes "<" to 6 bytes.
	if _, err := upgradeInputs([]string{"K=" + strings.Repeat("<", maxSecretsFile/6+1)}, nil, ""); err == nil || !strings.Contains(err.Error(), "over the 1048576 an upgrade or a reset-db carries") {
		t.Errorf("inputs past the cap: %v", err)
	}
	if _, err := upgradeInputs([]string{"K=" + strings.Repeat("x", maxSecretsFile-len(`{"K":""}`))}, nil, ""); err != nil {
		t.Errorf("inputs at the cap: %v", err)
	}
	for n, over := range map[int]bool{limit: false, limit + 1: true} {
		w := httptest.NewRecorder()
		hh.ServeHTTP(w, httptest.NewRequest("POST", "/v1/upgrade", strings.NewReader(body(n))))
		if (w.Code == http.StatusRequestEntityTooLarge) != over {
			t.Errorf("a body of %d bytes: %d %s", n, w.Code, w.Body.String())
		}
	}
}

// An upgrade across a schema baseline is refused before its OS is written, naming the verb that
// crosses it, on the channel the upgrade was asked with: the boot into it would refuse it, and the
// health gate go back.
func TestUpgradeAcrossABaselineIsRefusedBeforeWritingTheOS(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.releaseOf("2.0.0", atBaseline(2))
	env := p.env.readEnv()
	env.Set(ChannelVar, "stable") // the channel an upgrade given none takes
	if err := env.WriteFile(filepath.Join(p.env.dir, envName)); err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{}
	hh := daemon(p.env, host).Handler()
	for channel, way := range map[string]string{
		"stable": "`sudo notes-stow reset-db --new-baseline --channel stable --version 2.0.0` wipes it and installs 2.0.0; its OS is not written",
		"":       "`sudo notes-stow reset-db --new-baseline --version 2.0.0` wipes it and installs 2.0.0; its OS is not written",
	} {
		var e errOf
		if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: channel}, &e); code != 409 ||
			!strings.Contains(e.Error, "notes 2.0.0 starts schema baseline 2, and the install runs notes 1.0.0, at baseline 1: its data cannot carry over, so it moves to 2.0.0 only with its data wiped: "+way) ||
			e.Fix != "stay on this release, or cross to it with its data wiped (reset-db --new-baseline)" {
			t.Fatalf("channel %q: %d %s (fix %q)", channel, code, e.Error, e.Fix)
		}
	}
	if calls := fmt.Sprint(host.calls()); strings.Contains(calls, "kairos-agent") {
		t.Errorf("the OS was written: %v", calls)
	}
}

// Under a `command` scheme the crossing's hint names --unordered with the --version it names:
// reset-db orders that version as an upgrade does (unorderedTo).
func TestUpgradeAcrossABaselineHintsUnorderedUnderACommandScheme(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.releaseOf("2.0.0", atBaseline(2), commandScheme)
	hh := daemon(p.env, &fakeHost{}).Handler()
	var e errOf
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", Version: "2.0.0", Unordered: true}, &e); code != 409 ||
		!strings.Contains(e.Error, "`sudo notes-stow reset-db --new-baseline --channel stable --version 2.0.0 --unordered` wipes it and installs 2.0.0") {
		t.Fatalf("%d %s (fix %q)", code, e.Error, e.Fix)
	}
}

// Under a `command` scheme the upgrade's fix for an input the release adds names --unordered, as
// its --version needs it, though the upgrade was the channel's.
func TestTheAddedInputFixIsUnorderedUnderACommandScheme(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.releaseOf("2.0.0", func(s *spec.Spec) {
		s.Inputs = append(s.Inputs, spec.Input{Key: "SMTP_HOST", Prompt: "Mail host", Required: true})
	}, commandScheme)
	var e errOf
	if code := call(t, daemon(p.env, &fakeHost{}).Handler(), "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", DryRun: true}, &e); code != 409 ||
		e.Fix != vmGivesAdded(onStable+" --unordered") {
		t.Fatalf("%d %s (fix %q)", code, e.Error, e.Fix)
	}
}

// reset-db --new-baseline --version under a `command` scheme, which the daemon cannot run, takes
// another version than the OS's only with unordered, in the plan and in the job; unordered needs
// a version; the channel's release, the OS's own version and another scheme's are not refused so.
func TestTheDaemonsNewBaselineVersionIsUnorderedUnderACommandScheme(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.releaseOf("2.0.0", atBaseline(2), commandScheme)
	host := &fakeHost{}
	hh := daemon(p.env, host).Handler()
	var e errOf
	const fix = "run the same command again with --unordered added (only if you checked 2.0.0 comes after 1.0.0), or a plain reset-db --new-baseline (the channel's release)"
	if code := call(t, hh, "GET", "/v1/reset-db?new_baseline=true&channel=stable&version=2.0.0", nil, &e); code != 409 ||
		!strings.Contains(e.Error, "own command, which runs only where the project publishes") || e.Fix != fix {
		t.Fatalf("ordered: %d %s (fix %q)", code, e.Error, e.Fix)
	}
	// The app, which may not send unordered, is told the operator's command over SSH.
	e = errOf{}
	if code := call(t, asApp(t, p.env), "GET", "/v1/reset-db?new_baseline=true&channel=stable&version=2.0.0", nil, &e); code != 409 ||
		!strings.Contains(e.Error, "own command, which runs only where the project publishes") ||
		e.Fix != "run `sudo notes-stow reset-db --new-baseline --channel stable --version 2.0.0 --unordered` over SSH (only if you checked 2.0.0 comes after 1.0.0), or a plain reset-db --new-baseline (the channel's release)" {
		t.Fatalf("the app's: %d %s (fix %q)", code, e.Error, e.Fix)
	}
	e = errOf{}
	if code := call(t, hh, "GET", "/v1/reset-db?new_baseline=true&channel=stable&unordered=true", nil, &e); code != 400 || !strings.Contains(e.Error, "--unordered is for --version") {
		t.Fatalf("unordered alone: %d %s", code, e.Error)
	}
	// The OS's own version is not another: what refuses it is that it comes after nothing.
	e = errOf{}
	if code := call(t, hh, "GET", "/v1/reset-db?new_baseline=true&channel=stable&version=1.0.0", nil, &e); code != 409 || !strings.Contains(e.Error, "does not come after") {
		t.Fatalf("the OS's version: %d %s", code, e.Error)
	}
	var plain ResetPlan
	if code := call(t, hh, "GET", "/v1/reset-db?new_baseline=true&channel=stable", nil, &plain); code != 200 || plain.Target != (Version{"2.0.0", 1}) {
		t.Fatalf("the channel's: %d %+v", code, plain)
	}
	var plan ResetPlan
	if code := call(t, hh, "GET", "/v1/reset-db?new_baseline=true&channel=stable&version=2.0.0&unordered=true", nil, &plan); code != 200 || plan.Target != (Version{"2.0.0", 1}) {
		t.Fatalf("unordered: %d %+v", code, plan)
	}
	// The job checks the order again: the POST without unordered is refused, its token kept.
	e = errOf{}
	req := ResetRequest{Confirm: &plan.Target, Token: plan.Token, NewBaseline: true, Channel: "stable", Version: "2.0.0"}
	if code := call(t, hh, "POST", "/v1/reset-db", req, &e); code != 409 || e.Fix != fix {
		t.Fatalf("POST ordered: %d %s (fix %q)", code, e.Error, e.Fix)
	}
	req.Unordered = true
	if code := call(t, hh, "POST", "/v1/reset-db", req, &e); code != 202 {
		t.Fatalf("POST unordered: %d %s", code, e.Error)
	}
	if j := waitJob(t, hh); j.State != "done" || !strings.Contains(fmt.Sprint(host.calls()), "kairos-agent upgrade") {
		t.Fatalf("%+v %v", j, host.calls())
	}
	// Another scheme orders the version itself.
	q := vmPublished(t, spec.Provider{Name: "none"}, nil)
	q.releaseOf("2.0.0", atBaseline(2))
	if code := call(t, daemon(q.env, &fakeHost{}).Handler(), "GET", "/v1/reset-db?new_baseline=true&channel=stable&version=2.0.0", nil, &plan); code != 200 || plan.Target != (Version{"2.0.0", 1}) {
		t.Fatalf("semver: %d %+v", code, plan)
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
	// What an earlier upgrade staged was for the OS the crossing's replaces: replaced by what the
	// crossing gives its boot, the channel it was asked on alone.
	if err := os.WriteFile(stagedInputsFile(p.env.dir), []byte(`{"release":"1.5.0 1","inputs":{"K":"v"}}`), 0o600); err != nil {
		t.Fatal(err)
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
	if raw, err := os.ReadFile(mark); err != nil || strings.TrimSpace(string(raw)) != "2.0.0 1" {
		t.Fatalf("mark %q %v (job %+v)", raw, err, j)
	}
	if raw, err := os.ReadFile(stagedInputsFile(p.env.dir)); err != nil || string(raw) != `{"release":"2.0.0 1","channel":"stable"}` {
		t.Errorf("an upgrade's staged inputs outlive the crossing: %s %v", raw, err)
	}
	if n := len(p.env.docker.ran("volume rm")); n != 0 {
		t.Errorf("wiped before the boot into the new release")
	}
	// Within one baseline the flag is refused: an upgrade moves it.
	q := vmPublished(t, spec.Provider{Name: "none"}, nil)
	q.releaseOf("2.0.0", atBaseline(1))
	if code := call(t, daemon(q.env, &fakeHost{}).Handler(), "GET", "/v1/reset-db?new_baseline=true&channel=stable", nil, &e); code != 409 || !strings.Contains(e.Error, "--new-baseline only crosses a baseline; `sudo notes-stow upgrade` moves it") {
		t.Fatalf("same baseline: %d %s", code, e.Error)
	}
	// An upgrade written instead takes the mark away: it crosses no baseline.
	q.releaseOf("2.1.0", atBaseline(1))
	qm := newBaselineFile(q.env.dir)
	if err := os.WriteFile(qm, []byte("2.0.0 1\n"), 0o600); err != nil {
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

// The app may look at a reset-db plan, never confirm one: its plan carries no token, and the
// operator's plan shown meanwhile is still confirmed.
func TestTheAppsResetPlanLeavesTheOperatorsToken(t *testing.T) {
	t.Parallel()
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.releaseOf("2.0.0", atBaseline(2))
	const token = "app-token"
	f := p.env.readEnv()
	f.Set(HostTokenVar, token)
	if err := f.WriteFile(filepath.Join(p.env.dir, envName)); err != nil {
		t.Fatal(err)
	}
	d := daemon(p.env, &fakeHost{})
	op, appH := d.Handler(), d.AppHandler()
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
		appH.ServeHTTP(w, r)
	})
	var plan, seen ResetPlan
	if code := call(t, op, "POST", "/v1/reset-db/plan", ResetPlanRequest{NewBaseline: true, Channel: "stable"}, &plan); code != 200 || plan.Token == "" {
		t.Fatalf("the operator's plan: %d %+v", code, plan)
	}
	if code := call(t, app, "GET", "/v1/reset-db?new_baseline=true&channel=stable", nil, &seen); code != 200 || seen.Token != "" || seen.Target != plan.Target {
		t.Fatalf("the app's plan: %d %+v", code, seen)
	}
	var e errOf
	if code := call(t, op, "POST", "/v1/reset-db", ResetRequest{Confirm: &plan.Target, Token: plan.Token, NewBaseline: true, Channel: "stable"}, &e); code != 202 {
		t.Fatalf("the operator's confirmation: %d %s", code, e.Error)
	}
	waitJob(t, op)
}

// A crossing to a release adding a required input is given it: its plan, asked with the inputs by
// POST (no URL carries them), judges them as the boot takes them, its refusal naming the reset-db
// that gives them; the POST must give the plan's inputs again, and stages them root-only for the
// boot, never logged. Inputs without a crossing, and for a release whose crossing's boot reads none
// (published by a Stowage before 0.45.0), are refused.
func TestACrossingStagesTheInputsTheReleaseAdds(t *testing.T) {
	t.Parallel()
	const secret = "s3cret-mail-password"
	adds := func(s *spec.Spec) {
		atBaseline(2)(s)
		s.Inputs = append(s.Inputs, spec.Input{Key: "SMTP_PASSWORD", Prompt: "Mail password", Secret: true, Required: true})
	}
	p := vmPublished(t, spec.Provider{Name: "none"}, nil)
	p.releaseOf("2.0.0", adds)
	host := &fakeHost{}
	hh := daemon(p.env, host).Handler()
	var e errOf
	const fix = "`sudo notes-stow reset-db --new-baseline --channel stable --version 2.0.0 --set KEY=VALUE` (or --set-file KEY=PATH, --answers FILE) gives each to the boot into the release"
	if code := call(t, hh, "GET", "/v1/reset-db?new_baseline=true&channel=stable", nil, &e); code != 409 || !e.Refused ||
		!strings.Contains(e.Error, "missing required input(s) the release adds, which reset-db --new-baseline did not give: SMTP_PASSWORD (Mail password)") || e.Fix != fix ||
		!strings.Contains(e.Error, "(it is given no input)") {
		t.Fatalf("no input: %d %s (fix %q)", code, e.Error, e.Fix)
	}
	if code := call(t, hh, "POST", "/v1/reset-db/plan", ResetPlanRequest{Inputs: map[string]string{"SMTP_PASSWORD": secret}}, &e); code != 400 || !strings.Contains(e.Error, "inputs are what the release new_baseline installs adds") {
		t.Fatalf("inputs without a crossing: %d %s", code, e.Error)
	}
	given := map[string]string{"SMTP_PASSWORD": secret}
	var plan ResetPlan
	if code := call(t, hh, "POST", "/v1/reset-db/plan", ResetPlanRequest{NewBaseline: true, Channel: "stable", Inputs: given}, &plan); code != 200 || plan.Target != (Version{"2.0.0", 1}) {
		t.Fatalf("plan with the input: %d %+v", code, plan)
	}
	// The POST gives the plan's inputs again: other ones, or none, are another plan.
	for _, other := range []map[string]string{nil, {"SMTP_PASSWORD": "another"}} {
		if code := call(t, hh, "POST", "/v1/reset-db", ResetRequest{Confirm: &plan.Target, Token: plan.Token, NewBaseline: true, Channel: "stable", Inputs: other}, &e); code != 409 || !strings.Contains(e.Error, "showed another plan") {
			t.Fatalf("inputs %v: %d %s", other, code, e.Error)
		}
	}
	if code := call(t, hh, "POST", "/v1/reset-db", ResetRequest{Confirm: &plan.Target, Token: plan.Token, NewBaseline: true, Channel: "stable", Inputs: given}, &e); code != 202 {
		t.Fatalf("%d %s", code, e.Error)
	}
	j := waitJob(t, hh)
	if j.State != "done" || strings.Contains(strings.Join(j.Log, "\n"), secret) {
		t.Fatalf("%+v", j)
	}
	at := stagedInputsFile(p.env.dir)
	var st staged
	fi, err := os.Stat(at)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("staged: %v %v", fi, err)
	}
	if raw, err := os.ReadFile(at); err != nil || json.Unmarshal(raw, &st) != nil || st.Release != "2.0.0 1" || st.Channel != "stable" || !maps.Equal(st.Inputs, given) {
		t.Fatalf("staged %+v: %v", st, err)
	}
	for stowage, refused := range map[string]bool{"0.44.0": true, "": true, crossedInputsSince: false} {
		p := vmPublished(t, spec.Provider{Name: "none"}, nil)
		p.releaseOf("2.0.0", adds, func(m *release.Manifest) { m.StowageVersion = stowage })
		var e errOf
		code := call(t, daemon(p.env, &fakeHost{}).Handler(), "POST", "/v1/reset-db/plan", ResetPlanRequest{NewBaseline: true, Channel: "stable", Inputs: given}, &e)
		if got := code == 409 && e.Refused && strings.Contains(e.Error, "whose crossing's boot takes no input a reset-db gives: nothing is staged"); got != refused || (!refused && code != 200) {
			t.Errorf("published by Stowage %q: %d %s", stowage, code, e.Error)
		}
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
	// It adds no input: a re-run changes one.
	if code := call(t, hh, "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable", Reboot: &no, Inputs: map[string]string{"SMTP_HOST": "mail"}}, &e); code != 409 || !e.Refused ||
		!strings.Contains(e.Error, "adds no input to the install: the install runs it already") {
		t.Fatalf("inputs: %d %+v", code, e)
	}
	e = errOf{}
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

// Across a baseline from an install a crossing left on an older OS, the hint names the upgrade back
// to the install's release, then the crossing. Under a `command` scheme the crossing names
// --unordered and the upgrade, to the release the install runs, does not (it is never refused so);
// under another scheme neither does.
func TestACrossingFromACrossedInstallHintsUnorderedUnderACommandScheme(t *testing.T) {
	t.Parallel()
	for _, command := range []bool{false, true} {
		p := vmPublished(t, spec.Provider{Name: "none"}, nil)
		_, crossed := testInstallerWith(t, "2.0.0", func(s *spec.Spec) { p.spec(s); atBaseline(2)(s) })
		if err := copyFile(crossed, filepath.Join(p.env.dir, "notes-stow"), 0o755); err != nil {
			t.Fatal(err)
		}
		env := p.env.readEnv()
		env.Set("STOWAGE_PROJECT_VERSION", "2.0.0")
		env.Set(ChannelVar, "stable")
		if err := env.WriteFile(filepath.Join(p.env.dir, envName)); err != nil {
			t.Fatal(err)
		}
		var scheme []func(*release.Manifest)
		if command {
			scheme = append(scheme, commandScheme)
		}
		p.releaseOf("2.0.0", atBaseline(2), scheme...)
		p.releaseOf("3.0.0", atBaseline(3), scheme...)
		want := map[bool]string{
			true:  ": `sudo notes-stow upgrade --version 2.0.0` writes the OS of the release the install runs, then `sudo notes-stow reset-db --new-baseline --channel stable --version 3.0.0 --unordered` wipes it",
			false: ": `sudo notes-stow upgrade --version 2.0.0` writes the OS of the release the install runs, then `sudo notes-stow reset-db --new-baseline --channel stable --version 3.0.0` wipes it",
		}[command]
		var e errOf
		if code := call(t, daemon(p.env, &fakeHost{}).Handler(), "POST", "/v1/upgrade", UpgradeRequest{Channel: "stable"}, &e); code != 409 || !strings.Contains(e.Error, want) {
			t.Errorf("command scheme %v: %d %s", command, code, e.Error)
		}
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
