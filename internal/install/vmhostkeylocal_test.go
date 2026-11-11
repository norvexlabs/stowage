package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hostKeyHost answers the command host-key runs on the VM with out (or err), recording it.
type hostKeyHost struct {
	out []byte
	err error
	ran []string
}

func (h *hostKeyHost) Run(_ context.Context, c Cmd) ([]byte, error) {
	run := strings.Join(c.Args, " ")
	if c.Stdin != nil {
		b, _ := io.ReadAll(c.Stdin)
		run += " <<< " + string(b)
	}
	h.ran = append(h.ran, run)
	return h.out, h.err
}

const testHostKeyBlob = "AAAAC3NzaC1lZDI1NTE5AAAAIHxW7ZbkC7Ocl3rG3Vh3cZQmOl2nQxvYgMAkSPqfkHaZ"

// localHostKeyInstaller is an installed VM's installer whose host answers host's.
func localHostKeyInstaller(t *testing.T, host *hostKeyHost) *Installer {
	t.Helper()
	r, _ := testInstallerWith(t, "1.0.0", nil)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, envName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return &Installer{R: r, O: Options{Dir: dir, Cos: t.TempDir(), Host: host, Euid: func() int { return 0 }, Hostname: func() (string, error) { return "notes-1", nil }}}
}

// On the VM itself, host-key runs vm host-key's own steps locally, as root without sudo: the VM's
// key under its host name, the one-time key retired; refused on the recovery or a live boot
// (nothing retired), not as root and before the first install; a failure of the steps, or no host
// name, fails naming what.
func TestHostKeyOnTheVM(t *testing.T) {
	t.Parallel()
	host := &hostKeyHost{out: []byte("ssh-ed25519 " + testHostKeyBlob + " root@notes\n")}
	in := localHostKeyInstaller(t, host)
	in.O.Hostname = func() (string, error) { return "Notes-1.", nil } // as ssh matches it
	k, err := in.LocalHostKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := fingerprint(testHostKeyBlob)
	if k.Format != MachineFormat || k.KnownHostsLine != "notes-1 ssh-ed25519 "+testHostKeyBlob || k.Fingerprint != want {
		t.Errorf("%+v", k)
	}
	if len(host.ran) != 1 || host.ran[0] != "sh -s <<< "+strings.Join(in.retireSeedKeyCommand(false, in.O.cos()), " ")+"\n" || strings.Contains(host.ran[0], "sudo") {
		t.Errorf("ran %q", host.ran)
	}
	if over := strings.Join(in.retireSeedKeyCommand(true, "/run/cos"), " "); strings.Count(over, "sudo ") != 3 || !strings.Contains(over, retireSteps("notes", true)) {
		t.Errorf("over SSH: %s", over)
	}
	// The steps' own check of the boot (it changed since): refused, nothing retired.
	host.out = []byte(recoveryMark + "\n")
	if _, err := in.LocalHostKey(context.Background()); !isRefused(err) || !strings.Contains(err.Error(), "recovery or live system") {
		t.Errorf("recovery: %v", err)
	}
	host.out, host.err = nil, errors.New("touch: /oem/notes-seed-host-key.retired: Read-only file system")
	if _, err := in.LocalHostKey(context.Background()); err == nil || isRefused(err) || !strings.Contains(err.Error(), "the steps retiring the seed's one-time host key") || !strings.Contains(err.Error(), "Read-only") {
		t.Errorf("the steps failing: %v", err)
	}
	host.err = nil
	for _, h := range []func() (string, error){
		func() (string, error) { return "", nil },
		func() (string, error) { return "localhost", nil },
		func() (string, error) { return "localhost.localdomain", nil },
		func() (string, error) { return "", errors.New("uname failed") },
		func() (string, error) { return "(none)", nil },
		func() (string, error) { return "*", nil },
		func() (string, error) { return "LOCALHOST.", nil },
	} {
		in.O.Hostname = h
		if _, err := in.LocalHostKey(context.Background()); err == nil || !strings.Contains(err.Error(), "the VM's host name, which its known_hosts line holds") || !isRefused(err) && !strings.Contains(err.Error(), "uname") {
			t.Errorf("host name: %v", err)
		}
	}
	// Refused before anything runs: not as root; not installed yet.
	for name, mod := range map[string]func(*Installer){
		"run the same command with sudo": func(in *Installer) { in.O.Euid = func() int { return 1000 } },
		"is not installed yet":           func(in *Installer) { _ = os.Remove(filepath.Join(in.O.Dir, envName)) },
		"first install is not done": func(in *Installer) {
			_ = os.WriteFile(filepath.Join(in.O.Dir, installingFile), []byte("2026-11-11T00:00:00Z\n"), 0o600)
		},
		// A recovery or live boot has no install to look at: refused as such, before that check.
		"Kairos's recovery system or a live boot": func(in *Installer) {
			_ = os.Remove(filepath.Join(in.O.Dir, envName))
			_ = os.WriteFile(filepath.Join(in.O.Cos, "recovery_mode"), nil, 0o600)
		},
		"recovery system or a live boot, whose host keys": func(in *Installer) {
			_ = os.WriteFile(filepath.Join(in.O.Cos, "live_mode"), nil, 0o600)
		},
	} {
		host := &hostKeyHost{}
		in := localHostKeyInstaller(t, host)
		mod(in)
		if _, err := in.LocalHostKey(context.Background()); !isRefused(err) || !strings.Contains(err.Error()+fixOf(err), name) || len(host.ran) != 0 {
			t.Errorf("%s: %v, ran %q", name, err, host.ran)
		}
	}
}

// fixOf is a refusal's fix, "" for another error.
func fixOf(err error) string {
	r, _ := Refusal(err)
	return r.Fix
}

// host-key prints the line, or under --json the key or its refusal as one object.
func TestHostKeyOutput(t *testing.T) {
	t.Parallel()
	k := &VMHostKey{Format: MachineFormat, KnownHostsLine: "notes-1 ssh-ed25519 AAAA", Fingerprint: "SHA256:x"}
	var out bytes.Buffer
	if err := printHostKey(&out, k, nil, false); err != nil || out.String() != "notes-1 ssh-ed25519 AAAA\n" {
		t.Errorf("text: %q %v", out.String(), err)
	}
	out.Reset()
	var got VMHostKey
	if err := printHostKey(&out, k, nil, true); err != nil || json.Unmarshal(out.Bytes(), &got) != nil || got != *k {
		t.Errorf("json: %q %v", out.String(), err)
	}
	refused := refuse("run it again once the VM rebooted into its active system", "the VM answers from its recovery or live system")
	out.Reset()
	var sr StatusRefused
	if err := printHostKey(&out, nil, refused, true); !isRefused(err) || json.Unmarshal(out.Bytes(), &sr) != nil || sr.Refused.Fix != "run it again once the VM rebooted into its active system" {
		t.Errorf("json refusal: %q %v", out.String(), err)
	}
	out.Reset()
	if err := printHostKey(&out, nil, refused, false); !isRefused(err) || out.Len() != 0 {
		t.Errorf("text refusal: %q %v", out.String(), err)
	}
}

// The verb is the VM's: `<short>-stow host-key`, with --json.
func TestTheVMHasHostKey(t *testing.T) {
	t.Parallel()
	e := vmEnv(t, "1.0.0")
	cmd := vmCommand(e.r, e.self, nil, nil, func(bool) Prompter { return nil }, clientTimes{})
	c, _, err := cmd.Find([]string{"host-key"})
	if err != nil || c.Name() != "host-key" || c.Flags().Lookup("json") == nil {
		t.Fatalf("%v", err)
	}
}

// The script as host-key sends it, run by a shell against a directory standing in for the VM's
// paths (systemctl stubbed out: the host's own is never called): the VM's key printed and every
// step's status kept, so a step that fails (the mark not written) fails the script.
func TestTheRetirementScriptRunsAndFailsAsAList(t *testing.T) {
	t.Parallel()
	r, _ := testInstallerWith(t, "1.0.0", nil)
	in := &Installer{R: r}
	run := func(markDir string) ([]byte, error) {
		root := t.TempDir()
		key, dropIn, retired := seedHostKeyFiles("notes")
		script := strings.Join(in.retireSeedKeyCommand(false, filepath.Join(root, "cos")), " ")
		for from, to := range map[string]string{
			"/etc/ssh/ssh_host_ed25519_key.pub": filepath.Join(root, "own.pub"),
			dropIn:                              filepath.Join(root, "dropin.conf"),
			key:                                 filepath.Join(root, "seed-key"),
			retired:                             filepath.Join(root, markDir, "retired"),
			"systemctl":                         "true",
		} {
			script = strings.ReplaceAll(script, from, to)
		}
		if err := os.WriteFile(filepath.Join(root, "own.pub"), []byte("ssh-ed25519 "+testHostKeyBlob+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, "oem"), 0o755); err != nil {
			t.Fatal(err)
		}
		return exec.Command("sh", "-c", script).Output()
	}
	if out, err := run("oem"); err != nil || !strings.Contains(string(out), testHostKeyBlob) {
		t.Errorf("the steps: %q %v", out, err)
	}
	if _, err := run("no-such-dir"); err == nil {
		t.Error("a mark not written, yet the steps succeeded")
	}
}
