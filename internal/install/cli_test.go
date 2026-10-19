package install

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/norvexlabs/stowage/internal/spec"
)

func TestATypedDirectoryExpandsHomeOnly(t *testing.T) {
	home, _ := os.UserHomeDir()
	for in, want := range map[string]string{"~/apps/x": filepath.Join(home, "apps/x"), "~": home, "./x": "./x", "a/~/b": "a/~/b", "~bob/x": "~bob/x"} {
		if got := expandHome(in); got != want {
			t.Errorf("%s → %s, want %s", in, got, want)
		}
	}
}

func TestIPv6IsOnOffOrLeftOut(t *testing.T) {
	t.Parallel()
	for args, want := range map[string]string{"": "<nil>", "--ipv6": "true", "--ipv6=true": "true", "--ipv6=false": "false"} {
		fs := pflag.NewFlagSet("x", pflag.ContinueOnError)
		var v *bool
		ipv6Flag(fs, &v, "")
		if err := fs.Parse(strings.Fields(args)); err != nil {
			t.Fatal(err)
		}
		got := "<nil>"
		if v != nil {
			got = strconv.FormatBool(*v)
		}
		if got != want {
			t.Errorf("%q: %s, want %s", args, got, want)
		}
	}
}

func TestUpgradeVersionRefusesOffline(t *testing.T) {
	t.Parallel()
	r, self := testInstaller(t, "1.0.0")
	cmd := Command(r, self, io.Discard, io.Discard)
	cmd.SetArgs([]string{"upgrade", "--version", "2.0.0", "--offline", "--dir", t.TempDir()})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--version fetches the release from the registry") {
		t.Fatalf("%v", err)
	}
}

func TestK8sCommandOnlyWithTheKubernetesShape(t *testing.T) {
	t.Parallel()
	r, self := testInstallerWith(t, "1.0.0", func(s *spec.Spec) { s.Shapes.Kubernetes = nil })
	if c, _, err := Command(r, self, io.Discard, io.Discard).Find([]string{"k8s"}); err == nil && c.Name() == "k8s" {
		t.Error("k8s without the kubernetes shape")
	}
	r, self = testInstaller(t, "1.0.0")
	if c, _, err := Command(r, self, io.Discard, io.Discard).Find([]string{"k8s", "install"}); err != nil || c.Name() != "install" {
		t.Errorf("%v %v", c, err)
	}
}

func TestVMCreateOnlyWithTheKairosShape(t *testing.T) {
	t.Parallel()
	r, self := testInstallerWith(t, "1.0.0", func(s *spec.Spec) { s.Shapes.Kairos = nil })
	if c, _, err := Command(r, self, io.Discard, io.Discard).Find([]string{"vm"}); err == nil && c.Name() == "vm" {
		t.Error("vm without the kairos shape")
	}
	r, self = testInstaller(t, "1.0.0")
	c, _, err := Command(r, self, io.Discard, io.Discard).Find([]string{"vm", "create"})
	if err != nil || c.Name() != "create" {
		t.Fatalf("%v %v", c, err)
	}
	for _, f := range []string{"platform", "out", "hostname", "user", "ssh-key", "network", "address", "gateway", "dns", "tls", "domains", "http-port", "https-port", "set", "answers", "no-input", "accessible"} {
		if c.Flags().Lookup(f) == nil {
			t.Errorf("no --%s", f)
		}
	}
}

func TestDownloadProgressReportsEachTenth(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	p := downloadProgress(&b)
	for _, done := range []int64{0, 1 << 20, 5 << 20, 9<<20 + 1, 10 << 20, 10 << 20} {
		p(done, 10<<20)
	}
	p(1, 0)
	if want := "  0% of 10 MiB\n  10% of 10 MiB\n  50% of 10 MiB\n  90% of 10 MiB\n  100% of 10 MiB\n"; b.String() != want {
		t.Errorf("got %q", b.String())
	}
}

func TestK8sSubcommandsKeepTheirOwnDefaults(t *testing.T) {
	t.Parallel()
	e := newKubeEnv(t, spec.Provider{Name: "none"})
	asker := &fakePrompter{answers: map[string]string{"ADMIN_EMAIL": "asked@example.org"}}
	run := func(args ...string) string {
		var out bytes.Buffer
		cmd := newCommand(e.release, "notes-stow", &out, io.Discard, K8sOptions{Helm: kubeTool{e.f, "helm"}, Kubectl: kubeTool{e.f, "kubectl"}},
			func(bool) Prompter { return asker })
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%q: %v", args, err)
		}
		return out.String()
	}
	if out := run("k8s", "install", "--dry-run", "--set", "ADMIN_EMAIL=a@example.org", "--no-input"); !strings.Contains(out, " --timeout 10m0s ") || !strings.Contains(out, "--namespace notes") {
		t.Errorf("install:\n%s", out)
	}
	// On a terminal, an input nothing gave is asked.
	if out := run("k8s", "install", "--dry-run"); !strings.Contains(out, "ADMIN_EMAIL=asked@example.org") {
		t.Errorf("install on a terminal:\n%s", out)
	}
	e.runs("0.9.0", 1)
	e.stored(map[string]string{"ADMIN_EMAIL": "a@example.org", "SESSION_SECRET": "s", "POSTGRES_PASSWORD": "p"}, ours)
	if out := run("k8s", "upgrade", "--dry-run", "--namespace", "other", "--no-input"); !strings.Contains(out, " --timeout 6m0s ") || !strings.Contains(out, "--namespace other") {
		t.Errorf("upgrade:\n%s", out)
	}
	e.f.history = []helmRevision{{Revision: 1, Status: "superseded", Chart: "notes-0.1.0"}, {Revision: 2, Status: "deployed", Chart: "notes-0.2.0"}}
	if run("k8s", "rollback", "--dry-run"); !slices.Contains(e.f.ran("helm rollback")[0], "6m0s") {
		t.Errorf("rollback %q", e.f.ran("helm rollback"))
	}
}
