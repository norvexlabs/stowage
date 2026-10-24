package install

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// readSSHString reads one of the SSH wire format's strings off b.
func readSSHString(t *testing.T, b *[]byte) []byte {
	t.Helper()
	if len(*b) < 4 {
		t.Fatalf("short: %d bytes", len(*b))
	}
	n := binary.BigEndian.Uint32(*b)
	if uint32(len(*b)-4) < n {
		t.Fatalf("a string of %d bytes in %d", n, len(*b)-4)
	}
	s := (*b)[4 : 4+n]
	*b = (*b)[4+n:]
	return s
}

func TestTheSeedHostKeyIsAnOpenSSHKey(t *testing.T) {
	t.Parallel()
	k, err := newSeedHostKey("notes-1")
	if err != nil {
		t.Fatal(err)
	}
	blk, rest := pem.Decode(k.Private)
	if blk == nil || blk.Type != "OPENSSH PRIVATE KEY" || len(rest) != 0 {
		t.Fatalf("PEM: %q", k.Private)
	}
	b, ok := bytes.CutPrefix(blk.Bytes, []byte("openssh-key-v1\x00"))
	if !ok {
		t.Fatal("no magic")
	}
	for _, want := range []string{"none", "none", ""} {
		if got := string(readSSHString(t, &b)); got != want {
			t.Errorf("cipher, KDF, options: %q, want %q", got, want)
		}
	}
	if n := binary.BigEndian.Uint32(b); n != 1 {
		t.Fatalf("%d keys", n)
	}
	b = b[4:]
	blob := readSSHString(t, &b)
	if k.Public != "ssh-ed25519 "+base64.StdEncoding.EncodeToString(blob) {
		t.Errorf("public %q", k.Public)
	}
	sum := sha256.Sum256(blob)
	if k.Fingerprint != "SHA256:"+base64.RawStdEncoding.EncodeToString(sum[:]) {
		t.Errorf("fingerprint %q", k.Fingerprint)
	}
	secret := readSSHString(t, &b)
	if len(b) != 0 || len(secret)%8 != 0 || !bytes.Equal(secret[:4], secret[4:8]) {
		t.Fatalf("private section: %d bytes, %d after", len(secret), len(b))
	}
	secret = secret[8:]
	pubBlob := blob
	readSSHString(t, &pubBlob)
	pub := readSSHString(t, &pubBlob)
	if typ := string(readSSHString(t, &secret)); typ != "ssh-ed25519" {
		t.Fatalf("type %q", typ)
	}
	if !bytes.Equal(readSSHString(t, &secret), pub) {
		t.Error("the private section's public key is not the key's")
	}
	priv := ed25519.PrivateKey(readSSHString(t, &secret))
	if len(priv) != ed25519.PrivateKeySize || !ed25519.Verify(ed25519.PublicKey(pub), []byte("m"), ed25519.Sign(priv, []byte("m"))) {
		t.Error("the private key does not sign for the public one")
	}
	if c := string(readSSHString(t, &secret)); c != "notes-1" {
		t.Errorf("comment %q", c)
	}
	for i, p := range secret {
		if p != byte(i+1) {
			t.Errorf("padding %v", secret)
		}
	}
	if o, _ := newSeedHostKey("notes-1"); o.Public == k.Public {
		t.Error("two keys alike")
	}
}

func TestTheCloudConfigServesTheSeedHostKeyUntilTheInstall(t *testing.T) {
	t.Parallel()
	m := Machine{Hostname: "notes-1", User: "ops", SSHKey: "ssh-ed25519 AAAA ops", Net: NetConfig{Mode: "dhcp"}}
	stages := func(m Machine) (string, map[string]stage) {
		out, err := UserData("notes", "notes-stow", m, Settings{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var cc cloudConfig
		if err := yaml.Unmarshal(out, &cc); err != nil {
			t.Fatal(err)
		}
		byName := map[string]stage{}
		for _, s := range cc.Stages["boot"] {
			byName[s.Name] = s
		}
		return string(out), byName
	}
	out, st := stages(m)
	if !strings.Contains(out, "It carries no secret.\n") {
		t.Errorf("header:\n%s", out)
	}
	for n := range st {
		if strings.Contains(n, "host key") {
			t.Errorf("a stage %q without a host key", n)
		}
	}
	m.HostKey = []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nk\n-----END OPENSSH PRIVATE KEY-----\n")
	out, st = stages(m)
	if !strings.Contains(out, "It carries no secret but a one-time SSH host key, served until the install.\n") {
		t.Errorf("header:\n%s", out)
	}
	const before, after = "[ ! -e /usr/local/notes/.env ] && [ ! -e /oem/notes-seed-host-key.retired ]", "[ -e /usr/local/notes/.env ] && [ ! -e /oem/notes-seed-host-key.retired ]"
	files := st["notes: the one-time SSH host key, until the install"]
	if files.If != before || len(files.Files) != 2 ||
		files.Files[0] != (cloudFile{Path: "/run/notes-seed-host-key", Permissions: 0o600, Content: string(m.HostKey)}) ||
		files.Files[1].Path != "/etc/ssh/sshd_config.d/01-notes-seed-host-key.conf" ||
		!strings.Contains(files.Files[1].Content, "\nHostKey /run/notes-seed-host-key\nHostKey /etc/ssh/ssh_host_ed25519_key\n") {
		t.Errorf("the key's step: %+v", files)
	}
	if r := st["notes: sshd with the one-time host key"]; r.If != before || len(r.Commands) != 1 || r.Commands[0] != sshdReload {
		t.Errorf("the reload: %+v", r)
	}
	if r := st["notes: the one-time SSH host key retired after the install"]; r.If != after || len(r.Commands) != 1 ||
		r.Commands[0] != "rm -f /etc/ssh/sshd_config.d/01-notes-seed-host-key.conf /run/notes-seed-host-key && touch /oem/notes-seed-host-key.retired && "+sshdReload {
		t.Errorf("the retirement: %+v", r)
	}
}
