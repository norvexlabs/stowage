package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// VMHostKey is `vm host-key --json`: the VM's own SSH host key, read through the key the run
// pinned (the one-time key of its seed, or the VM's own), once the one-time key is retired.
type VMHostKey struct {
	Format int `json:"format"` // MachineFormat
	// PublicKey is the VM's own key ("ssh-ed25519 <base64>"), Fingerprint its SHA256 as ssh prints
	// it; KnownHostsLine the key as a known_hosts line, under the host name the run checked the VM
	// by (the --known-hosts-line's, else the address; on the VM itself, its host name), absent
	// when that name cannot sit in one (Note says why).
	PublicKey      string `json:"public_key"`
	KnownHostsLine string `json:"known_hosts_line,omitempty"`
	Fingerprint    string `json:"fingerprint"`
	// Retired: the one-time key is no longer served (its files gone, sshd reloaded); false, with
	// Reason, when sshd's reload did not take, the run then failing.
	Retired bool   `json:"retired"`
	Reason  string `json:"reason,omitempty"`
	Note    string `json:"note,omitempty"`
}

// errNotReloaded is host-key's failure when the one-time key's files are gone but sshd did not
// take the reload: it may serve that key until it does.
var errNotReloaded = errors.New("the seed's one-time host key's files are gone, but sshd did not reload: it may serve that key until it does; `sudo systemctl reload ssh` (or reboot the VM), then run host-key again")

// HostKey reads the VM r names's own ed25519 host key over SSH, checked against the key r gives
// (--known-hosts-line: a seed's line, or the VM's own), else the one vm create recorded, and
// retires the one-time key: from then on the VM serves its own alone. With vm create's record,
// the record becomes the VM's own key. Run again, it changes nothing more, through the VM's own
// key. A VM answering from its recovery or live system is refused, the one-time key kept. Its
// reach's errors, and a VM that answers no SSH, are refusals: nothing changed.
func (in *Installer) HostKey(ctx context.Context, r VMReach) (*VMHostKey, error) {
	l, err := in.link(r)
	if err != nil {
		return nil, refusal("", err)
	}
	out, err := l.run(ctx, in.retireSeedKeyCommand(true, kairosCos)...)
	if err != nil {
		var ec exitCoder
		if errors.As(err, &ec) && ec.ExitCode() == 255 {
			return nil, refuse("check that the VM is up at "+l.addr+" and takes your key; a VM that rebooted since its install serves its own key alone: check it on the VM's console (`ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub`) and give it with --known-hosts-line",
				"%s: ssh did not reach the VM through the key it was given (%v)", r.Host, err)
		}
		return nil, err
	}
	k, err := hostKeyFrom(out, l.alias, r.Host+": ", "")
	if err != nil {
		return nil, err
	}
	if r.KnownHostsLine == "" && k.KnownHostsLine != "" { // vm create's record (link read it): the VM's own key from now on
		if err := writeAtomic(l.known, strings.NewReader(k.KnownHostsLine+"\n"), 0o600, nil); err != nil {
			return nil, err
		}
	}
	if !k.Retired {
		return k, errNotReloaded
	}
	return k, nil
}

// hostKeyFrom is the VMHostKey of what retireSeedKeyCommand printed, the key under alias (none
// when alias is "", note saying why); a recovery or live boot's answer is refused (where prefixes
// the reason), the one-time key kept. Retired is false when the steps said sshd did not reload.
func hostKeyFrom(out []byte, alias, where, note string) (*VMHostKey, error) {
	blob, fp, err := ownHostKey(out)
	if errors.Is(err, errNotActive) {
		return nil, refuse("run it again once the VM rebooted into its active system", "%s%v", where, err)
	}
	if err != nil {
		return nil, err
	}
	k := &VMHostKey{Format: MachineFormat, PublicKey: "ssh-ed25519 " + blob, Fingerprint: fp, Retired: true, Note: note}
	if alias != "" {
		k.KnownHostsLine = alias + " ssh-ed25519 " + blob
	}
	if strings.Contains(string(out), sshdNotReloaded) {
		k.Retired, k.Reason = false, "sshd did not reload: it may serve the one-time key until it does"
	}
	return k, nil
}

// LocalHostKey is `<short>-stow host-key` on the VM itself: what HostKey runs over SSH
// (retireSeedKeyCommand), run here as root, so no SSH config of the operator's is needed; the
// VM's own key under its host name (O.Hostname, else os.Hostname). Its refusals: HostKey's (a
// recovery or live boot keeps the one-time key), a run not as root (which could not retire it),
// and a VM not installed yet, whose first install may still wait for what is given over SSH
// pinned to the one-time key (the install retires it itself).
func (in *Installer) LocalHostKey(ctx context.Context) (*VMHostKey, error) {
	// First: a recovery or live boot has no install of its own to look at.
	if cos := in.O.cos(); fileExists(filepath.Join(cos, "recovery_mode")) || fileExists(filepath.Join(cos, "live_mode")) {
		return nil, refuse("run it again once the VM rebooted into its active system", "this is Kairos's recovery system or a live boot, whose host keys are not the active system's: the one-time key is kept")
	}
	if in.O.euid() != 0 {
		return nil, refuse("run the same command with sudo", "host-key retires the seed's one-time host key, which root alone can")
	}
	// Until the first install is done (its .env is written before its stack is up), vm create
	// may still reach the VM pinned to the one-time key.
	if unfinished, _, _ := in.firstInstall(); unfinished && !notInstalled(in) {
		return nil, refuse("wait for the first install to finish, which retires the one-time key itself; then run it again", "%s's first install is not done: it may still wait for what vm create gives over SSH, pinned to the seed's one-time key", in.R.Spec.Name)
	}
	if notInstalled(in) {
		return nil, absent("wait for the first install, which retires the one-time key itself; then run it again", "%s is not installed yet: a first install may still wait for secrets or a certificate given over SSH, pinned to the seed's one-time key", in.R.Spec.Name)
	}
	hostname := in.O.Hostname
	if hostname == nil {
		hostname = os.Hostname
	}
	raw, err := hostname()
	if err != nil {
		return nil, fmt.Errorf("the VM's host name, which its known_hosts line holds: %w", err)
	}
	// As ssh matches it: lower-cased, without a root dot. A name no known_hosts line can hold
	// leaves the line out (the key and its retirement are no name's), the note saying why.
	host := strings.ToLower(strings.TrimSuffix(raw, "."))
	switch {
	case host == "" || host == "localhost" || strings.HasPrefix(host, "localhost."):
		err = fmt.Errorf("%q names no machine", raw)
	default:
		err = checkHostname(host) // known_hosts takes no pattern: no '*', ',' or '(none)'
	}
	note := ""
	if err != nil {
		note = fmt.Sprintf("no known_hosts line: the VM's host name cannot sit in one (%v); write one under the name or address ssh reaches the VM by, or give the VM a host name of its own (hostnamectl set-hostname NAME)", err)
		host = ""
	}
	// The script on standard input: an error then names the step's own message, not the script.
	out, err := in.O.host().Run(ctx, Cmd{Args: []string{"sh", "-s"}, Stdin: strings.NewReader(strings.Join(in.retireSeedKeyCommand(false, in.O.cos()), " ") + "\n")})
	if err != nil {
		return nil, fmt.Errorf("the steps retiring the seed's one-time host key (its file and sshd drop-in removed, the mark on /oem, sshd reloaded) failed: %w; once the cause is fixed, run it again: the steps are run again whole", err)
	}
	k, err := hostKeyFrom(out, host, "", note)
	if err == nil && !k.Retired {
		err = errNotReloaded
	}
	return k, err
}

// printHostKey is `host-key`'s output: k as one JSON object (asJSON) or its known_hosts line (its
// public key, and its note on errOut, when it has none); a refusal under asJSON as one object too
// (exit 3 all the same). A key read whose retirement did not take (sshd not reloaded) is printed
// all the same, its failure returned (a non-zero exit).
func printHostKey(out, errOut io.Writer, k *VMHostKey, err error, asJSON bool) error {
	switch {
	case k == nil && err != nil && asJSON:
		return printStatusRefused(out, err)
	case k == nil && err != nil:
		return err
	case asJSON:
		return errors.Join(printJSON(out, k), err)
	}
	line := k.KnownHostsLine
	if line == "" {
		line = k.PublicKey
	}
	if _, werr := fmt.Fprintln(out, line); werr != nil {
		return werr
	}
	if k.Note != "" && errOut != nil {
		fmt.Fprintln(errOut, "note: "+k.Note)
	}
	return err
}
