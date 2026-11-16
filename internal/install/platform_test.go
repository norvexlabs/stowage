package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/norvexlabs/stowage/internal/installerbin"
	"github.com/norvexlabs/stowage/internal/payload"
)

// On a platform other than the target's (an arm64 Docker host that pulled the installer image's
// arm64 entry), every verb on an install of this host refuses, naming the platform; an operator's
// machine's verbs run.
func TestAnotherPlatformRunsTheOperatorsVerbsAlone(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cmd := func(platform string, out io.Writer) *cobra.Command {
		return newCommandOn(platform, e.r, e.self, out, io.Discard, K8sOptions{}, func(bool) Prompter { return nil })
	}
	for _, args := range [][]string{{"--dir", e.dir, "--no-input"}, {"upgrade", "--dir", e.dir}, {"status", "--dir", e.dir}, {"uninstall", "--dir", e.dir}, {"secrets", "export"}} {
		err := run(cmd("linux/arm64", io.Discard), args)
		var r Refused
		if !errors.As(err, &r) || !strings.Contains(err.Error(), "this installer is the linux/arm64 build: notes installs on linux/amd64 hosts alone") ||
			!strings.Contains(r.Fix, "docker run --rm --platform linux/amd64 "+e.r.Spec.Registry.Prefix+"/notes-installer:"+e.r.Manifest.Tag+" > notes.sh") {
			t.Errorf("%v on linux/arm64: %v (fix %q)", args, err, r.Fix)
		}
	}
	// A verb under --json says it in its Result.
	var out bytes.Buffer
	if err := run(cmd("darwin/arm64", &out), []string{"upgrade", "--dir", e.dir, "--json"}); err == nil {
		t.Fatal("upgrade ran on darwin/arm64")
	}
	var res Result
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.Refused == nil || !strings.Contains(res.Refused.Reason, "the darwin/arm64 build") {
		t.Errorf("the --json Result: %s %v", out.String(), err)
	}
	// The bootstrap script and describe run anywhere.
	out.Reset()
	if err := run(cmd("darwin/arm64", &out), []string{payload.ScriptCommand}); err != nil || !strings.HasPrefix(out.String(), "#!/bin/sh\n") {
		t.Errorf("script on darwin/arm64: %v", err)
	}
	if err := run(cmd("darwin/arm64", io.Discard), []string{"describe", "--json"}); err != nil {
		t.Errorf("describe on darwin/arm64: %v", err)
	}
	// Exactly the verbs outside operatorVerbs are marked, on another platform; none on the target.
	var marked, runnable []string
	walkCommands(cmd("linux/arm64", io.Discard), func(root, c *cobra.Command) {
		if c.Runnable() {
			runnable = append(runnable, c.CommandPath())
			if _, ok := c.Annotations[targetOnlyAnnotation]; ok {
				marked = append(marked, c.CommandPath())
			}
			if want := c == root || !slices.Contains(operatorVerbs, verbOf(root, c)); want != slices.Contains(marked, c.CommandPath()) {
				t.Errorf("%s: target only %v", c.CommandPath(), !want)
			}
		}
	})
	for _, v := range []string{"notes-stow", "notes-stow upgrade", "notes-stow tune", "notes-stow registry-login"} {
		if !slices.Contains(marked, v) {
			t.Errorf("%s is not refused on linux/arm64: %v", v, marked)
		}
	}
	for _, v := range []string{"notes-stow vm seed", "notes-stow k8s install", "notes-stow mirror import", "notes-stow describe", "notes-stow script"} {
		if !slices.Contains(runnable, v) || slices.Contains(marked, v) {
			t.Errorf("%s is not an operator's verb on linux/arm64", v)
		}
	}
	walkCommands(cmd(installerbin.Target, io.Discard), func(_, c *cobra.Command) {
		if _, ok := c.Annotations[targetOnlyAnnotation]; ok {
			t.Errorf("%s is refused on the target's platform", c.CommandPath())
		}
	})
}

func run(c *cobra.Command, args []string) error {
	c.SetArgs(args)
	c.SetErr(io.Discard)
	return c.Execute()
}

// walkCommands calls f on every command of root, root included.
func walkCommands(root *cobra.Command, f func(root, c *cobra.Command)) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		f(root, c)
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}
