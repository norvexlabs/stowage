package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/lock"
	"github.com/norvexlabs/stowage/internal/testutil"
)

func execute(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := New("test", &out, &errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String() + errOut.String(), err
}

func TestLintExitCodes(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	if out, err := execute(t, "-C", root, "lint"); err != nil {
		t.Fatalf("clean project failed: %v\n%s", err, out)
	}
	testutil.Edit(t, root, "deploy/compose.yaml", "${LOG_LEVEL:-info}", "${LOG_LEVEL}")
	out, err := execute(t, "-C", root, "lint")
	var exit ExitError
	if !errors.As(err, &exit) || exit.Code != 1 {
		t.Fatalf("err = %v, want exit 1", err)
	}
	if !strings.Contains(out, "LOG_LEVEL") || !strings.Contains(out, "1 error(s)") {
		t.Errorf("output:\n%s", out)
	}
}

func TestLintJSON(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	out, err := execute(t, "-C", root, "lint", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Version  string
		Findings []struct{ Severity string }
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Version != "1.0.0" {
		t.Fatalf("json = %q, %v", out, err)
	}
}

func TestProjectVersionWithChannel(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	out, err := execute(t, "-C", root, "project-version", "--channel", "beta", "--revision", "2", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	if v["tag"] != "beta-1.0.0-r2" || v["project_version"] != "1.0.0" {
		t.Errorf("got %v", v)
	}
	if _, err := execute(t, "-C", root, "project-version", "--channel", "nightly"); err == nil {
		t.Error("undeclared channel accepted")
	}
}

func TestChangelogRange(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	testutil.Write(t, root, "VERSION", "1.2.0\n")
	testutil.Write(t, root, "changes/1.1.0.md", "- eleven\n")
	testutil.Write(t, root, "changes/1.2.0.md", "- twelve\n")
	out, err := execute(t, "-C", root, "changelog", "--from", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(out, "## 1.2.0") > strings.Index(out, "## 1.1.0") || strings.Contains(out, "## 1.0.0") {
		t.Errorf("output:\n%s", out)
	}
	if _, err := execute(t, "-C", root, "changelog", "--from", "not-a-version"); err == nil {
		t.Error("invalid --from accepted")
	}
}

func TestPlan(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	out, err := execute(t, "-C", root, "plan")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"notes 1.0.0 (revision 1, channel stable)", "image/api", "reads STOWAGE_PROJECT_VERSION", "release"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output lacks %q:\n%s", want, out)
		}
	}
}

func TestMonthGuardWarnsNotFails(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	testutil.Edit(t, root, "stowage.yaml", "scheme: { provider: semver }", "scheme: { provider: calver, format: YYYY.M.MICRO, month_guard: utc }")
	testutil.Write(t, root, "VERSION", "2026.9.1\n")
	testutil.Write(t, root, "changes/2026.9.1.md", "- x\n")
	// 2026.9 is in the past whenever this runs: the guard fires, as a warning.
	out, err := execute(t, "-C", root, "lint")
	if err != nil {
		t.Fatalf("stale month must warn, not fail: %v\n%s", err, out)
	}
	if !strings.Contains(out, "warning: version.scheme") {
		t.Errorf("month guard did not warn:\n%s", out)
	}
}

// The commands that name a release refuse one whose tags would collide, like lint.
func TestCollidingTagsAreRefused(t *testing.T) {
	root := testutil.CopyExample(t, "minimal")
	testutil.Edit(t, root, "stowage.yaml", `beta:   { tag: "beta-{v}", mutable: true }`, `beta:   { tag: "{v}-beta", mutable: true }`)
	for _, args := range [][]string{{"project-version", "--channel", "stable"}, {"plan"}} {
		out, err := execute(t, append([]string{"-C", root}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "cannot be released") {
			t.Errorf("%v: err = %v\n%s", args, err, out)
		}
	}
}

func lockAs(t *testing.T, buildVersion string, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := New(buildVersion, &out, &errOut)
	cmd.SetArgs(append([]string{"lock"}, args...))
	err := cmd.Execute()
	return out.String() + errOut.String(), err
}

func TestLock(t *testing.T) {
	t.Parallel()
	root := testutil.CopyExample(t, "minimal")
	path := filepath.Join(root, lock.FileName)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	read := func() string {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	if _, err := lockAs(t, "dev", "-C", root); err == nil || !strings.Contains(err.Error(), "not a release build") {
		t.Fatalf("a dev build: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a dev build wrote the lock: %v", err)
	}
	if _, err := lockAs(t, "1.2.0", "-C", t.TempDir()); err == nil || !strings.Contains(err.Error(), "next to the spec") {
		t.Fatalf("no spec: %v", err)
	}

	if out, err := lockAs(t, "1.2.0", "-C", root); err != nil || !strings.Contains(out, "stowage.lock pins stowage 1.2.0") {
		t.Fatalf("first lock: %v\n%s", err, out)
	}
	l, err := lock.Load(path)
	if err != nil || l.Stowage != "1.2.0" || strings.Contains(read(), "runtime") {
		t.Fatalf("written lock: %+v, %v\n%s", l, err, read())
	}
	if out, err := lockAs(t, "1.2.0", "-C", root); err != nil || !strings.Contains(out, "already pins stowage 1.2.0") {
		t.Fatalf("again: %v\n%s", err, out)
	}

	// Another stowage without --update changes nothing.
	before := read()
	if _, err := lockAs(t, "1.3.0", "-C", root); err == nil || !strings.Contains(err.Error(), "pins stowage 1.2.0, this is 1.3.0") || !strings.Contains(err.Error(), "--update") {
		t.Fatalf("another stowage: %v", err)
	}
	if read() != before {
		t.Fatal("a refused lock rewrote the file")
	}
	if out, err := lockAs(t, "1.3.0", "-C", root, "--update"); err != nil || !strings.Contains(out, "stowage 1.2.0 -> 1.3.0") {
		t.Fatalf("--update: %v\n%s", err, out)
	}
	if l, err := lock.Load(path); err != nil || l.Stowage != "1.3.0" {
		t.Fatalf("updated lock: %+v, %v", l, err)
	}

	if out, err := lockAs(t, "1.1.0", "-C", root, "--update"); err != nil || !strings.Contains(out, "stowage 1.3.0 -> 1.1.0, an older stowage") {
		t.Fatalf("--update to an older stowage: %v\n%s", err, out)
	}

	testutil.Write(t, root, lock.FileName, "stowage: 1.2.0\nextra: 1\n")
	if _, err := lockAs(t, "1.3.0", "-C", root, "--update"); err == nil || !strings.Contains(err.Error(), "extra") || !strings.Contains(err.Error(), "remove it and run stowage lock") {
		t.Fatalf("a broken lock: %v", err)
	}
	if read() != "stowage: 1.2.0\nextra: 1\n" {
		t.Fatal("a broken lock was overwritten")
	}
	// A lock that cannot be read is not one to remove: its pins may be valid.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := lockAs(t, "1.3.0", "-C", root); err == nil || strings.Contains(err.Error(), "remove it") {
		t.Fatalf("an unreadable lock: %v", err)
	}
}
