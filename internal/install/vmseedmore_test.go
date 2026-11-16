package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
)

// seedOf is vm seed's run on v, as the CLI gives it, with mod and so.
func seedOf(t *testing.T, v *vmCreate, mod func(*Options), so SeedOptions) (*VMSeed, error) {
	t.Helper()
	in := v.installer(func(o *Options) {
		o.PublicAddr = func(context.Context) (netip.Addr, error) { return netip.MustParseAddr("198.51.100.40"), nil }
		o.TLS, o.Set = "internal", map[string]string{"ADMIN_EMAIL": "admin@example.org", "APP_DOMAIN": "app.example.org", "API_DOMAIN": "api.example.org"}
		if mod != nil {
			mod(o)
		}
	})
	return in.SeedVM(context.Background(), &VMCreate{Platform: "truenas", Hostname: "notes-1", User: "ops", SSHKey: v.key}, so)
}

// The seed carries the size a program creating the VM refuses under and makes by default, and
// what a platform's firewall in front of the VM lets through, as vm seed's settings give them.
func TestVMSeedSizeAndFirewall(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	v.sized(leastNotes, recNotes)
	s, err := seedOf(t, v, nil, SeedOptions{})
	if err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	if want := (SeedSize{CPUs: SeedRange{2, 4}, MemoryMiB: SeedRange{2048, 8192}, AppGiB: SeedRange{20, 60}}); s.Size != want {
		t.Errorf("size %+v, want %+v", s.Size, want)
	}
	in := v.installer(nil)
	if want := fmt.Sprint(in.openedPorts(&Settings{TLS: "internal"})); fmt.Sprint(s.Firewall.OpenPorts) != want || len(s.Firewall.OpenPorts) == 0 ||
		!slices.Equal(s.Firewall.SSHSources, everySource) {
		t.Errorf("firewall %+v, want ports %s", s.Firewall, want)
	}
	raw, _ := json.Marshal(s)
	for _, key := range []string{`"size":{"cpus":{"min":2,"recommended":4},"memory_mib":{"min":2048,"recommended":8192},"app_gib":{"min":20,"recommended":60}}`, `"firewall":{"ssh_sources":["0.0.0.0/0","::/0"],"open_ports":[{"protocol":"tcp","port":`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("no %s in %s", key, raw)
		}
	}
	// No size stated: the floor, and the default; SSH from --ssh-allow's sources alone.
	v = newVMCreate(t, "qcow2")
	in = v.installer(func(o *Options) {
		o.TLS, o.Set = "internal", map[string]string{"ADMIN_EMAIL": "admin@example.org", "APP_DOMAIN": "app.example.org", "API_DOMAIN": "api.example.org"}
	})
	s, err = in.SeedVM(context.Background(), &VMCreate{Platform: "truenas", Hostname: "notes-1", User: "ops", SSHKey: v.key, SSHAllow: []string{"192.0.2.0/24", "2001:db8::/32"}}, SeedOptions{})
	if err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	if want := (SeedSize{CPUs: SeedRange{1, 2}, MemoryMiB: SeedRange{1024, 4096}, AppGiB: SeedRange{4, 20}}); s.Size != want || s.AppGiB != 20 {
		t.Errorf("size %+v, want %+v", s.Size, want)
	}
	if !slices.Equal(s.Firewall.SSHSources, []string{"192.0.2.0/24", "2001:db8::/32"}) {
		t.Errorf("ssh sources %v", s.Firewall.SSHSources)
	}
}

// The seed carries what a consumer otherwise reads from the registry or makes itself: the NoCloud
// meta-data, each disk's sizes, the cloud image ship registered from it, and --platform's disk as
// a file.
func TestVMSeedMachineMaterial(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2", "raw")
	v.r.Spec.Shapes.Kairos.CloudPublish = []string{"aws", "gcp"}
	dir := t.TempDir()
	s, err := seedOf(t, v, nil, SeedOptions{DiskDir: dir})
	if err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	if s.MetaData != "instance-id: notes-1\nlocal-hostname: notes-1\n" {
		t.Errorf("meta_data %q", s.MetaData)
	}
	osGiB := int((23000 + diskSlackMiB + 1023) / 1024)
	if s.AppGiB != 20 {
		t.Errorf("app_gib %d", s.AppGiB)
	}
	for p, d := range s.Disk {
		if d.OSGiB != osGiB || d.DiskGiB != s.AppGiB+osGiB {
			t.Errorf("%s: %+v", p, d)
		}
	}
	// aws publishes raw, which the registry holds; gcp's gce disk it lacks; truenas is no cloud.
	raw := v.r.Manifest.Artifacts.Disks["raw"]
	if got := s.Disk["aws"].CloudImage; got != "notes-"+strings.TrimPrefix(raw, "sha256:")[:12] || got == "" {
		t.Errorf("aws %q", got)
	}
	if _, ok := s.Disk["gcp"]; ok || s.Disk["truenas"].CloudImage != "" || s.Disk["scaleway"].CloudImage != "" {
		t.Errorf("only a published cloud names an image: %+v", s.Disk)
	}
	f := s.DiskFile
	if f == nil || filepath.Dir(f.Path) != dir || !filepath.IsAbs(f.Path) || f.Format != "qcow2" || f.DiskGiB != s.Disk["truenas"].DiskGiB {
		t.Fatalf("%+v", f)
	}
	data, err := os.ReadFile(f.Path)
	sum := sha256.Sum256(data)
	if err != nil || string(data) != "a qcow2 disk" || f.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("%v %q %s", err, data, f.SHA256)
	}
	// Without --disk-dir no file; a --disk-dir that is no directory is refused before the registry.
	if s, err := seedOf(t, v, nil, SeedOptions{}); err != nil || s.DiskFile != nil {
		t.Errorf("%v %+v", err, s)
	}
	if _, err := seedOf(t, v, nil, SeedOptions{DiskDir: filepath.Join(dir, "missing")}); !isRefused(err) || !strings.Contains(err.Error(), "is not a directory") {
		t.Errorf("%v", err)
	}
}

// The seed names the NICs it makes the VM with: each one's role, the MAC its cloud-config names it
// by (generated on a hypervisor for several; none for one, unless given), its network and the primary; none for a
// release declaring none.
func TestVMSeedNamesItsNICs(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	if s, err := seedOf(t, v, nil, SeedOptions{}); err != nil {
		t.Fatal(err)
	} else if b, _ := json.Marshal(s); !strings.Contains(string(b), `"interfaces":[]`) {
		t.Fatalf("no NIC declared: %s", b)
	}
	v.r.Spec.Host = &spec.Host{Interfaces: map[string]spec.HostInterface{"uplink": {Primary: true}, "edge": {}}}
	in := v.installer(func(o *Options) {
		o.TLS, o.Set = "internal", map[string]string{"ADMIN_EMAIL": "admin@example.org", "APP_DOMAIN": "app.example.org", "API_DOMAIN": "api.example.org"}
	})
	s, err := in.SeedVM(context.Background(), &VMCreate{Platform: "truenas", Hostname: "notes-1", User: "ops", SSHKey: v.key,
		Interfaces: []string{"edge=dhcp,mac=02:00:00:00:00:0b,attach=br1"}}, SeedOptions{})
	if err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	if len(s.Interfaces) != 2 {
		t.Fatalf("%+v", s.Interfaces)
	}
	edge, uplink := s.Interfaces[0], s.Interfaces[1]
	// The generated MAC: locally administered (second bit of the first octet set) and unicast.
	if edge != (SeedInterface{Role: "edge", MAC: "02:00:00:00:00:0b", Attach: "br1"}) || uplink.Role != "uplink" || !uplink.Primary || uplink.Attach != "" ||
		!macAddr.MatchString(uplink.MAC) || !unicastMAC(uplink.MAC) || uplink.MAC[1]&2 == 0 {
		t.Fatalf("%+v", s.Interfaces)
	}
	for _, i := range s.Interfaces {
		if i.MAC == "" || !strings.Contains(s.UserData, i.MAC) {
			t.Errorf("the cloud-config names no NIC by %s's MAC %q", i.Role, i.MAC)
		}
	}
	// One NIC on a hypervisor: named from the NIC the VM has and pinned to its MAC by its first
	// boot, so no MAC unless one is given; the JSON leaves it out.
	v.r.Spec.Host = &spec.Host{Interfaces: map[string]spec.HostInterface{"uplink": {Primary: true}, "edge": {Optional: true}}}
	for given, want := range map[string]string{
		"":                             `"interfaces":[{"role":"uplink","primary":true}]`,
		"uplink=mac=02:00:00:00:00:0c": `"interfaces":[{"role":"uplink","mac":"02:00:00:00:00:0c","primary":true}]`,
	} {
		c := &VMCreate{Platform: "truenas", Hostname: "notes-1", User: "ops", SSHKey: v.key}
		if given != "" {
			c.Interfaces = []string{given}
		}
		s, err := in.SeedVM(context.Background(), c, SeedOptions{})
		if err != nil {
			t.Fatalf("one NIC, %q: %v", given, err)
		}
		if b, _ := json.Marshal(s); !strings.Contains(string(b), want) {
			t.Errorf("one NIC, %q: the JSON's interfaces: %s", given, b)
		}
		if pinned := strings.Contains(s.UserData, "OriginalName=") && strings.Contains(s.UserData, pinnedOEM("notes", "uplink")); pinned != (given == "") {
			t.Errorf("one NIC, %q: named by its original name and pinned %v:\n%s", given, pinned, s.UserData)
		}
	}
}

// --await-secrets makes the first install wait for a secrets export; --tls byo for its certificate,
// given over SSH: the seed carries neither, and refuses both at once.
func TestVMSeedAwaits(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	s, err := seedOf(t, v, nil, SeedOptions{AwaitSecrets: true})
	if err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	st, cc := cloudSettings(t, s.UserData)
	if !s.AwaitSecrets || s.AwaitTLS || !st.AwaitSecrets || awaitedGiven(cc) != "" {
		t.Errorf("await secrets: %+v %+v %q", s, st, awaitedGiven(cc))
	}
	v.r.Spec.Front.TLSModes = append(slices.Clone(v.r.Spec.Front.TLSModes), "byo")
	byo := func(o *Options) { o.TLS = "byo" }
	if s, err = seedOf(t, v, byo, SeedOptions{}); err != nil {
		t.Fatalf("byo: %v\n%s", err, v.log)
	}
	st, cc = cloudSettings(t, s.UserData)
	if s.AwaitSecrets || !s.AwaitTLS || !st.AwaitSecrets || st.TLS != "" || awaitedGiven(cc) != "byo" {
		t.Errorf("byo: %+v %+v %q", s, st, awaitedGiven(cc))
	}
	// Refused by vm create's one rule, its fix followed literally: either flag left out passes.
	_, err = seedOf(t, v, byo, SeedOptions{AwaitSecrets: true})
	if r, ok := Refusal(err); !ok || !strings.HasPrefix(r.Reason, "--await-secrets and --tls byo: the first install waits for one of them over SSH") ||
		r.Fix != "leave out --tls byo (choose another --tls mode), or leave out --await-secrets" {
		t.Errorf("both: %v", err)
	}
	// byo and a secret input the answers give: one wait, for the certificate and the input.
	v.r.Spec.Inputs = append(v.r.Spec.Inputs, spec.Input{Key: "ACCESS_TOKEN", Secret: true})
	if s, err = seedOf(t, v, func(o *Options) { byo(o); o.Answers = map[string]string{"ACCESS_TOKEN": "tok"} }, SeedOptions{}); err != nil {
		t.Fatalf("byo and an answer: %v\n%s", err, v.log)
	}
	st, cc = cloudSettings(t, s.UserData)
	if !s.AwaitTLS || !slices.Equal(s.AwaitGiven, []string{"ACCESS_TOKEN"}) || !st.AwaitSecrets || awaitedGiven(cc) != "byo" {
		t.Errorf("byo and an answer: %+v %+v %q", s, st, awaitedGiven(cc))
	}
	v.r.Spec.Inputs = v.r.Spec.Inputs[:len(v.r.Spec.Inputs)-1]
	// A certificate given to the seed is refused as a seed refuses it, before any file is read.
	cert := func(o *Options) { o.TLS, o.Cert, o.Key = "byo", "missing-cert.pem", "missing-key.pem" }
	if _, err := seedOf(t, v, cert, SeedOptions{}); !isRefused(err) || !strings.Contains(err.Error(), "a seed carries no secret input and no TLS material") {
		t.Errorf("a certificate: %v", err)
	}
	// A release whose only VM mode is byo: the seed's fix is --tls byo, never --cert.
	v.r.Spec.Front.TLSModes = []string{"none", "byo"}
	if _, err := seedOf(t, v, func(o *Options) { o.TLS = "" }, SeedOptions{}); !isRefused(err) || !strings.HasPrefix(refusedFix(err), "give --tls byo") {
		t.Errorf("byo alone: %v", err)
	}
	// A bad channel: a refusal, with its fix.
	if _, err := seedOf(t, v, nil, SeedOptions{Channel: "Bad!"}); !isRefused(err) || refusedFix(err) == "" {
		t.Errorf("channel: %v", err)
	}
}

// --channel names a channel the registry holds a release of the project on: a typo is refused
// before the seed is made, the release's own channel needs no pointer.
func TestVMSeedChecksTheChannel(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	own := v.r.Manifest.Channel
	if _, err := seedOf(t, v, nil, SeedOptions{Channel: "beta"}); !isRefused(err) || !strings.Contains(err.Error(), "--channel beta: the registry "+v.prefix+" holds no release of notes on beta (this release is on "+own+")") {
		t.Fatalf("a channel without a release: %v", err)
	}
	other := *v.r.Manifest
	other.Name = "other"
	other.Channel = "beta"
	v.putRelease("beta", &other)
	if _, err := seedOf(t, v, nil, SeedOptions{Channel: "beta"}); !isRefused(err) {
		t.Fatalf("another project's release on the channel: %v", err)
	}
	beta := *v.r.Manifest
	beta.Channel = "beta"
	v.putRelease("beta", &beta)
	for _, ch := range []string{"beta", own} {
		s, err := seedOf(t, v, nil, SeedOptions{Channel: ch})
		if err != nil {
			t.Fatalf("%s: %v\n%s", ch, err, v.log)
		}
		if st, _ := cloudSettings(t, s.UserData); st.Channel != ch {
			t.Errorf("%s: channel %q", ch, st.Channel)
		}
	}
}

// awaitedGiven is what the cloud-config's AwaitGivenFile holds; "" when it writes none.
func awaitedGiven(cc cloudConfig) string {
	for _, stg := range cc.Stages {
		for _, x := range stg {
			for _, f := range x.Files {
				if strings.HasSuffix(f.Path, ".given") {
					return strings.TrimSpace(f.Content)
				}
			}
		}
	}
	return ""
}

// vm seed's refusal is one object on standard output, as status --json's, and the run exits 3.
func TestVMSeedCommandRefusal(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	var out, errb bytes.Buffer
	o := v.installer(nil).O
	o.Out, o.Log = &out, &errb
	in := &Installer{R: v.r}
	var sets, setFiles, domains []string
	var answers string
	var noInput, accessible, takesTLS bool
	prepare := func() error {
		in.O = o
		return nil
	}
	c := vmCreateCommand(in, &o, prepare, func(bool) Prompter { return nil }, &sets, &setFiles, &domains, &answers, &noInput, &accessible, &takesTLS)
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	c.SetArgs([]string{"seed", "--json", "--platform", "truenas", "--ssh-key", v.key, "--disk-dir", filepath.Join(t.TempDir(), "missing")})
	err := c.Execute()
	if r := oneStatusRefused(t, out.String()); !isRefused(err) || !strings.Contains(r.Refused.Reason, "is not a directory") || r.Refused.Fix == "" {
		t.Errorf("%v %+v", err, r)
	}
}

// vm host-key reads the VM's own key through the key --known-hosts-line gives, retires the one-time
// key, and prints the VM's key under the line's host name; it writes no record. A link ssh cannot
// make names the console.
func TestVMHostKey(t *testing.T) {
	t.Parallel()
	const own = "AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dhl7VTCvK"
	const seedKey = "AAAAC3NzaC1lZDI1NTE5AAAAIEAIlrmYko7n75rw1in9PSsqvQQwoVBxv2WjCA2q6yuS"
	seedLine := seedHostAlias + " ssh-ed25519 " + seedKey
	reach := VMReach{Host: "notes-1", User: "ops", Address: "192.0.2.10", KnownHostsLine: seedLine}
	ssh := &kitSSH{stdout: "ssh-ed25519 " + own + " root@notes-1\n"}
	in, _, state := vmKitRun(t, ssh, nil)
	k, err := in.HostKey(context.Background(), reach)
	if err != nil {
		t.Fatal(err)
	}
	// The VM's key under the line's host name: the next run's --known-hosts-line.
	fp, _ := fingerprint(own)
	if k.Format != MachineFormat || k.KnownHostsLine != seedHostAlias+" ssh-ed25519 "+own || k.Fingerprint != fp || !k.Retired {
		t.Errorf("%+v", k)
	}
	call := strings.Join(ssh.calls[len(ssh.calls)-1], " ")
	for _, want := range []string{"cat /etc/ssh/ssh_host_ed25519_key.pub", "sudo rm -f /etc/ssh/sshd_config.d/01-notes-seed-host-key.conf /run/notes-seed-host-key", "sudo touch /oem/notes-seed-host-key.retired", "StrictHostKeyChecking=yes", "HostKeyAlias=" + seedHostAlias, "ops@192.0.2.10"} {
		if !strings.Contains(call, want) {
			t.Errorf("no %q in %s", want, call)
		}
	}
	// The line's own file keeps the line, and nothing else is written.
	files, _ := filepath.Glob(filepath.Join(state, "notes", "*", "*"))
	for _, f := range files {
		if b, _ := os.ReadFile(f); filepath.Base(filepath.Dir(f)) != "known-hosts" || string(b) != seedLine+"\n" {
			t.Errorf("%s holds %q", f, b)
		}
	}
	if len(files) != 1 {
		t.Errorf("files %v", files)
	}
	// Without the line: refused, nothing run.
	ssh = &kitSSH{stdout: "ssh-ed25519 " + own + "\n"}
	in, _, _ = vmKitRun(t, ssh, nil)
	if _, err := in.HostKey(context.Background(), VMReach{Host: "notes-1", User: "ops"}); !isRefused(err) || !strings.Contains(err.Error(), "the VM's host key is not known") || len(ssh.calls) != 0 {
		t.Errorf("no key: %v %v", err, ssh.calls)
	}
	// ssh refused (255): the console's check named; an answer that is no ed25519 key fails.
	in, _, _ = vmKitRun(t, &kitSSH{err: exitErr(255)}, nil)
	if _, err := in.HostKey(context.Background(), reach); !isRefused(err) || !strings.Contains(refusedFix(err), "check it on the VM's console") {
		t.Errorf("%v (fix %q)", err, refusedFix(err))
	}
	// The recovery or live system answers: refused, the one-time key kept.
	in, _, _ = vmKitRun(t, &kitSSH{stdout: recoveryMark + "\n"}, nil)
	if _, err := in.HostKey(context.Background(), reach); !isRefused(err) || !strings.Contains(err.Error(), "recovery or live system") {
		t.Errorf("recovery: %v", err)
	}
	in, _, _ = vmKitRun(t, &kitSSH{stdout: "nothing\n"}, nil)
	if _, err := in.HostKey(context.Background(), reach); err == nil || !strings.Contains(err.Error(), "no ed25519 host key") {
		t.Errorf("%v", err)
	}
	// sshd did not reload: the VM's key read, not retired, the run failing naming the fix; the
	// steps checked its reload.
	ssh = &kitSSH{stdout: "ssh-ed25519 " + own + " root@notes-1\n" + sshdNotReloaded + "\n"}
	in, _, _ = vmKitRun(t, ssh, nil)
	k, err = in.HostKey(context.Background(), reach)
	if !errors.Is(err, errNotReloaded) || k == nil || k.Retired || k.Reason == "" || k.KnownHostsLine != seedHostAlias+" ssh-ed25519 "+own {
		t.Errorf("not reloaded: %+v %v", k, err)
	}
	if call := strings.Join(ssh.calls[len(ssh.calls)-1], " "); !strings.Contains(call, "|| echo "+sshdNotReloaded) || !strings.Contains(call, "systemctl is-active -q ssh.socket") {
		t.Errorf("the reload is not checked: %s", call)
	}
}

// exitErr is an error carrying exit status code, as ssh's ExitError does.
type exitErr int

func (e exitErr) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitErr) ExitCode() int { return int(e) }

// A client CA given to a seed is refused as its certificate is, before the file is read.
func TestVMSeedRefusesAClientCA(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	if _, err := seedOf(t, v, func(o *Options) { o.ClientCA = "missing-ca.pem" }, SeedOptions{}); !isRefused(err) || !strings.Contains(err.Error(), "a client CA: a seed carries no") {
		t.Errorf("%v", err)
	}
}

// "none" for the client CA carries nothing: a seed takes it (as "none" for the CA, which this
// release, publishing no CA, refuses for its own reason).
func TestVMSeedTakesNoCA(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	v.r.Spec.Front.Routes[0].ClientAuth = true
	if _, err := seedOf(t, v, func(o *Options) { o.ClientCA = noCA }, SeedOptions{}); err != nil {
		t.Errorf("%v", err)
	}
}

// A release booting another release's disks (the channel's set, which its ship published) names
// no cloud image: whether that ship registered one is not this spec's to tell.
func TestVMSeedNamesNoCloudImageOfAnotherReleasesDisks(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2", "raw")
	v.r.Spec.Shapes.Kairos.CloudPublish = []string{"aws"}
	m := v.r.Manifest
	m.Artifacts.BootDisks = &release.BootDisks{Release: "1.0.0-r0", Stowage: "0.34.0", Disks: m.Artifacts.Disks}
	m.Artifacts.Disks = nil
	s, err := seedOf(t, v, nil, SeedOptions{})
	if err != nil {
		t.Fatalf("%v\n%s", err, v.log)
	}
	if d, ok := s.Disk["aws"]; !ok || d.CloudImage != "" {
		t.Errorf("%+v %v", d, ok)
	}
}

// A CA an answers file gives is refused as a seed refuses TLS material; "none" is taken.
func TestVMSeedCA(t *testing.T) {
	t.Parallel()
	v := newVMCreate(t, "qcow2")
	v.r.Spec.Front.PublishCA = &spec.PublishCA{Volume: "ca"}
	if _, err := seedOf(t, v, func(o *Options) { o.CA = "missing-ca.pem" }, SeedOptions{}); !isRefused(err) || !strings.Contains(err.Error(), "a seed carries no") {
		t.Errorf("a CA: %v", err)
	}
	if _, err := seedOf(t, v, func(o *Options) { o.CA = noCA }, SeedOptions{}); err != nil && strings.Contains(err.Error(), "a seed carries no") {
		t.Errorf("none: %v", err)
	}
}

// awaitOverSSH's refusal names each thing that waits beside the secrets export and a fix for each.
func TestAwaitOverSSHRefusal(t *testing.T) {
	t.Parallel()
	given := &Given{Project: "notes", TLS: &GivenTLS{Cert: "c", Key: "k"}, Inputs: map[string]string{"B": "2", "A": "1"}}
	err := awaitOverSSH(&Settings{}, "--secrets", true, given)
	r, ok := Refusal(err)
	if !ok || r.Reason != "--secrets and the certificate and its key, A, B, --tls byo: the first install waits for one of them over SSH, a secrets export (install --secrets -) or what install --given - gives" ||
		r.Fix != "remove A, B from the --answers file and leave out --tls byo (choose another --tls mode), or leave out --secrets" {
		t.Errorf("%v (fix %q)", err, r.Fix)
	}
	// vm create's certificate (byo by --tls or the --answers file's tls section), a client CA beside
	// it: the mode is left out, and the client CA, which any mode takes.
	err = awaitOverSSH(&Settings{}, "--secrets", false, &Given{Project: "notes", TLS: &GivenTLS{Cert: "c", Key: "k"}})
	if r, _ := Refusal(err); r.Fix != "leave out --tls byo (choose another --tls mode), or leave out --secrets" {
		t.Errorf("a certificate: %v (fix %q)", err, r.Fix)
	}
	err = awaitOverSSH(&Settings{}, "--secrets", false, &Given{Project: "notes", TLS: &GivenTLS{ClientCA: "c"}})
	if r, _ := Refusal(err); r.Fix != "leave out the client CA (--client-ca and the --answers file's tls client_ca), or leave out --secrets" {
		t.Errorf("a client CA: %v (fix %q)", err, r.Fix)
	}
	st := &Settings{}
	if err := awaitOverSSH(st, "", false, given); err != nil || !st.AwaitSecrets || st.AwaitGiven == "" {
		t.Errorf("given alone: %v %+v", err, st)
	}
}
