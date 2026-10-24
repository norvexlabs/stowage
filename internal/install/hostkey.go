package install

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
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

// retireSeedHostKey, once the install over SSH took the secrets, reads the VM's own ed25519 host
// key through the pinned key, which vouches for it, into known_hosts in dir, then retires the
// one-time key (its drop-in and file removed, its retirement marked, sshd reloaded) and prints the VM's key's
// fingerprint. A failure is said, not returned: the install is done, and the first boot after it
// retires the key (an install that rebooted the VM already has, and the pinned key no longer answers).
func (in *Installer) retireSeedHostKey(ctx context.Context, dir, pin string, m Machine, addr, pubKey string) {
	o, name := &in.O, in.R.Spec.Name
	key, dropIn, retired := seedHostKeyFiles(name)
	out, err := o.SSH.Run(ctx, Cmd{Args: pinnedSSHArgs(pin, m.User, addr, pubKey,
		"cat", "/etc/ssh/ssh_host_ed25519_key.pub", "&&", "sudo", "rm", "-f", dropIn, key, "&&", "sudo", "touch", retired, "&&", "sudo", "sh", "-c", "'"+sshdReload+"'")})
	var own string
	if err == nil {
		f := strings.Fields(string(out))
		if len(f) < 2 || f[0] != "ssh-ed25519" {
			err = fmt.Errorf("no ed25519 host key in %q", out)
		} else if own, err = fingerprint(f[1]); err == nil {
			err = writeAtomic(filepath.Join(dir, "known_hosts"), strings.NewReader(addr+" "+f[0]+" "+f[1]+"\n"), 0o600, nil)
		}
	}
	if err != nil {
		o.logf("warning: the VM's own SSH host key was not read through the one-time key (%v): known_hosts does not hold it, check it on the console (`ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub`); the first boot after the install retires the one-time key, if no boot did yet", err)
		return
	}
	if err := os.Remove(pin); err != nil {
		o.logf("warning: %v", err)
	}
	fmt.Fprintf(o.Out, "The VM's own SSH host key, read through the one-time key and now served alone: %s (ED25519); known_hosts: %s\n", own, filepath.Join(dir, "known_hosts"))
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
