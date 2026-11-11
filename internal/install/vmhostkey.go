package install

import (
	"context"
	"errors"
	"strings"
)

// VMHostKey is `vm host-key --json`: the VM's own SSH host key, read through the key the run
// pinned (the one-time key of its seed, or the VM's own), once the one-time key is retired.
type VMHostKey struct {
	Format int `json:"format"` // MachineFormat
	// KnownHostsLine is the VM's own key as a known_hosts line, under the host name the run checked
	// the VM by (the --known-hosts-line's, else the address); Fingerprint its SHA256, as ssh prints it.
	KnownHostsLine string `json:"known_hosts_line"`
	Fingerprint    string `json:"fingerprint"`
}

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
	out, err := l.run(ctx, in.retireSeedKeyCommand()...)
	if err != nil {
		var ec exitCoder
		if errors.As(err, &ec) && ec.ExitCode() == 255 {
			return nil, refuse("check that the VM is up at "+l.addr+" and takes your key; a VM that rebooted since its install serves its own key alone: check it on the VM's console (`ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub`) and give it with --known-hosts-line",
				"%s: ssh did not reach the VM through the key it was given (%v)", r.Host, err)
		}
		return nil, err
	}
	blob, fp, err := ownHostKey(out)
	if errors.Is(err, errNotActive) {
		return nil, refuse("run it again once the VM rebooted into its active system", "%s: %v", r.Host, err)
	}
	if err != nil {
		return nil, err
	}
	line := l.alias + " ssh-ed25519 " + blob
	if r.KnownHostsLine == "" { // vm create's record (link read it): the VM's own key from now on
		if err := writeAtomic(l.known, strings.NewReader(line+"\n"), 0o600, nil); err != nil {
			return nil, err
		}
	}
	return &VMHostKey{Format: MachineFormat, KnownHostsLine: line, Fingerprint: fp}, nil
}
