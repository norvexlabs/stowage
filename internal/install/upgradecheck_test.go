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
