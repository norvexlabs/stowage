package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	if _, err := seedOf(t, v, byo, SeedOptions{AwaitSecrets: true}); !isRefused(err) || !strings.Contains(err.Error(), "--tls byo and --await-secrets") {
		t.Errorf("both: %v", err)
	}
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

// vm host-key reads the VM's own key through the pinned key, retires the one-time key, and prints
// the VM's key; with vm create's record it replaces the record. A link ssh cannot make names the
// console.
func TestVMHostKey(t *testing.T) {
	t.Parallel()
	const own = "AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dhl7VTCvK"
	ssh := &kitSSH{stdout: "ssh-ed25519 " + own + " root@notes-1\n"}
	in, _, dir := vmKitRun(t, ssh, nil)
	k, err := in.HostKey(context.Background(), VMReach{Host: "notes-1", User: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	fp, _ := fingerprint(own)
	if k.Format != MachineFormat || k.KnownHostsLine != "notes-1 ssh-ed25519 "+own || k.Fingerprint != fp {
		t.Errorf("%+v", k)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "known_hosts")); string(got) != "notes-1 ssh-ed25519 "+own+"\n" {
		t.Errorf("record %q", got)
	}
	call := strings.Join(ssh.calls[len(ssh.calls)-1], " ")
	for _, want := range []string{"cat /etc/ssh/ssh_host_ed25519_key.pub", "sudo rm -f /etc/ssh/sshd_config.d/01-notes-seed-host-key.conf /run/notes-seed-host-key", "sudo touch /oem/notes-seed-host-key.retired", "StrictHostKeyChecking=yes"} {
		if !strings.Contains(call, want) {
			t.Errorf("no %q in %s", want, call)
		}
	}
	// Given the seed's line: the VM's key under its host name, and no record written.
	in, _, dir = vmKitRun(t, ssh, nil)
	if err := os.Remove(filepath.Join(dir, "known_hosts")); err != nil {
		t.Fatal(err)
	}
	const seedKey = "AAAAC3NzaC1lZDI1NTE5AAAAIEAIlrmYko7n75rw1in9PSsqvQQwoVBxv2WjCA2q6yuS"
	k, err = in.HostKey(context.Background(), VMReach{Host: "notes-1", User: "ops", Address: "192.0.2.10", KnownHostsLine: seedHostAlias + " ssh-ed25519 " + seedKey})
	if err != nil || k.KnownHostsLine != seedHostAlias+" ssh-ed25519 "+own {
		t.Errorf("%v %+v", err, k)
	}
	if _, err := os.Stat(filepath.Join(dir, "known_hosts")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a record written: %v", err)
	}
	// The line's own file keeps the line: the next run giving it checks the same key.
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(filepath.Dir(dir)), "known-hosts", "*"))
	for _, f := range files {
		if b, _ := os.ReadFile(f); string(b) != seedHostAlias+" ssh-ed25519 "+seedKey+"\n" {
			t.Errorf("%s holds %q", f, b)
		}
	}
	if len(files) != 1 {
		t.Errorf("line files %v", files)
	}
	// ssh refused (255): the console's check named; an answer that is no ed25519 key fails.
	in, _, _ = vmKitRun(t, &kitSSH{err: exitErr(255)}, nil)
	if _, err := in.HostKey(context.Background(), VMReach{Host: "notes-1", User: "ops"}); !isRefused(err) || !strings.Contains(refusedFix(err), "check it on the VM's console") {
		t.Errorf("%v (fix %q)", err, refusedFix(err))
	}
	// The recovery or live system answers: refused, the one-time key kept (nothing written).
	in, _, dir = vmKitRun(t, &kitSSH{stdout: recoveryMark + "\n"}, nil)
	before, _ := os.ReadFile(filepath.Join(dir, "known_hosts"))
	if _, err := in.HostKey(context.Background(), VMReach{Host: "notes-1", User: "ops"}); !isRefused(err) || !strings.Contains(err.Error(), "recovery or live system") {
		t.Errorf("recovery: %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "known_hosts")); !bytes.Equal(before, after) {
		t.Errorf("the record changed: %q", after)
	}
	// No record (vm create's pin of a seed is not one: a create may still wait on it): refused,
	// naming the record, nothing run.
	ssh = &kitSSH{stdout: "ssh-ed25519 " + own + "\n"}
	in, _, dir = vmKitRun(t, ssh, nil)
	_ = os.Remove(filepath.Join(dir, "known_hosts"))
	_ = os.WriteFile(filepath.Join(dir, "known_hosts.seed"), []byte(seedHostAlias+" ssh-ed25519 "+seedKey+"\n"), 0o600)
	if _, err := in.HostKey(context.Background(), VMReach{Host: "notes-1", User: "ops"}); !isRefused(err) || !strings.Contains(err.Error(), "the VM's host key is not known") || len(ssh.calls) != 0 {
		t.Errorf("no key: %v %v", err, ssh.calls)
	}
	in, _, _ = vmKitRun(t, &kitSSH{stdout: "nothing\n"}, nil)
	if _, err := in.HostKey(context.Background(), VMReach{Host: "notes-1", User: "ops"}); err == nil || !strings.Contains(err.Error(), "no ed25519 host key") {
		t.Errorf("%v", err)
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
