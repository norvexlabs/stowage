package install

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// seedHostKey is a one-time SSH host key: the OpenSSH private key the VM's cloud-config carries,
// its public key as a known_hosts line's key ("ssh-ed25519 <base64>") and its SHA256 fingerprint
// as ssh prints it.
type seedHostKey struct {
	Private     []byte
	Public      string
	Fingerprint string
}

// newSeedHostKey generates an ed25519 host key, written in OpenSSH's private key format
// (unencrypted: sshd reads it unattended), comment its comment.
func newSeedHostKey(comment string) (*seedHostKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	blk, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return nil, err
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return &seedHostKey{
		Private:     pem.EncodeToMemory(blk),
		Public:      strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))),
		Fingerprint: ssh.FingerprintSHA256(sp),
	}, nil
}

// retireSeedKeyCommand is the command, run on the VM over SSH, that prints the VM's own ed25519
// host key, then retires the one-time key: its file and drop-in removed, its retirement marked on
// the OEM partition, sshd reloaded. Run again, it prints the key and changes nothing more.
// On Kairos's recovery system, or a live (installer) boot, it prints recoveryMark alone and
// changes nothing: their keys are not the active system's, and the mark would keep the active
// system from serving the one-time key. Its steps are retireSteps', under sudo or not; cos is
// the directory Kairos marks its boot's mode in (/run/cos).
func (in *Installer) retireSeedKeyCommand(sudo bool, cos string) []string {
	return []string{"if", "[", "-e", shellQuote(cos + "/recovery_mode"), "]", "||", "[", "-e", shellQuote(cos + "/live_mode"), "];", "then", "echo", recoveryMark + ";", "else",
		"cat", "/etc/ssh/ssh_host_ed25519_key.pub", "&&", retireStepsChecked(in.R.Spec.Name, sudo) + ";", "fi"}
}

// recoveryMark is what retireSeedKeyCommand prints on a recovery or live boot.
const recoveryMark = "stowage-not-the-active-system"

// errNotActive is ownHostKey's error for a VM answering from its recovery or live system.
var errNotActive = errors.New("the VM answers from its recovery or live system: its first boot is not done, and the one-time key is kept")

// ownHostKey reads the VM's own ed25519 host key from what retireSeedKeyCommand printed: its
// base64 blob and its SHA256 fingerprint.
func ownHostKey(out []byte) (blob, fp string, err error) {
	if strings.TrimSpace(string(out)) == recoveryMark {
		return "", "", errNotActive
	}
	f := strings.Fields(string(out))
	if len(f) < 2 || f[0] != "ssh-ed25519" {
		return "", "", fmt.Errorf("no ed25519 host key in %q", out)
	}
	fp, err = fingerprint(f[1])
	return f[1], fp, err
}

// fingerprint is the SHA256 fingerprint, as ssh prints it, of an ed25519 public key's base64 blob.
func fingerprint(blob64 string) (string, error) {
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte("ssh-ed25519 " + blob64))
	if err != nil {
		return "", err
	}
	return ssh.FingerprintSHA256(k), nil
}

// seedHostKeyNote tells the operator who sends the secrets the one-time host key to expect, and
// that the VM's own replaces it once the install is done and the VM rebooted.
func seedHostKeyNote(k *seedHostKey, addr string) string {
	if k == nil {
		return ""
	}
	return fmt.Sprintf("Until its install, the VM's SSH host key is a one-time key its cloud-config carries: ssh must show %s (ED25519), or do not send the secrets. After the install and a reboot it serves its own: `ssh-keygen -R %s` then.\n", k.Fingerprint, addr)
}
