package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// xdgDir is the XDG base directory env names, else home's def; a relative value is ignored, as
// the XDG spec says.
func (o *Options) xdgDir(env, def, what string) (string, error) {
	if base := o.getenv(env); filepath.IsAbs(base) {
		return base, nil
	}
	if home := o.getenv("HOME"); filepath.IsAbs(home) {
		return filepath.Join(home, def), nil
	}
	return "", fmt.Errorf("neither %s nor HOME is an absolute path: where %s goes", env, what)
}

// sshPathOK refuses a directory ssh would misread in a known hosts file's path, with its escapes and
// % tokens.
func sshPathOK(dir string) error {
	if strings.ContainsAny(dir, `%\"`) {
		return fmt.Errorf("%s: a path ssh reads cannot hold %%, \\ or \"", dir)
	}
	return nil
}

// sshArgsChecking are ssh's arguments to run command on the VM at addr as user: in batch mode (no
// prompt), with the private key next to the public one given when there is one, else the agent's
// key of that public one alone (ssh's other keys would spend the VM's few authentication tries),
// and StrictHostKeyChecking set to checking. The VM's key is the file known's alone: the
// operator's global known hosts are never read, and a key ssh records is written in the clear
// (HashKnownHosts=no), which the operator can read and check.
//
// alias is the name the VM's key has in known (HostKeyAlias): every link names it, so ssh never
// looks the key up under its own idea of the host (an ssh_config HostName or Port).
func sshArgsChecking(checking, alias, known, user, addr, pubKey string, command ...string) []string {
	// Quoted: ssh splits an option's value at white space, into several files.
	args := []string{"-o", "HostKeyAlias=" + alias, "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=" + checking,
		"-o", `UserKnownHostsFile="` + known + `"`, "-o", "GlobalKnownHostsFile=/dev/null", "-o", "HashKnownHosts=no"}
	pub := expandHome(pubKey)
	if priv, ok := strings.CutSuffix(pub, ".pub"); ok {
		if _, err := os.Stat(priv); err == nil {
			args = append(args, "-i", priv, "-o", "IdentitiesOnly=yes")
		} else if _, err := os.Stat(pub); err == nil {
			args = append(args, "-i", pub, "-o", "IdentitiesOnly=yes")
		}
	}
	return append(append(args, user+"@"+addr, "--"), command...)
}

// seedHostAlias is the name a pinned known_hosts file gives the VM's one-time host key.
const seedHostAlias = "vm-seed-host-key"

// pinnedSSHArgs are ssh's arguments with the VM's host key checked against the file pin's alone
// (its line names seedHostAlias): ssh refuses any other, never adding one.
func pinnedSSHArgs(pin, user, addr, pubKey string, command ...string) []string {
	return sshArgsChecking("yes", seedHostAlias, pin, user, addr, pubKey, command...)
}
