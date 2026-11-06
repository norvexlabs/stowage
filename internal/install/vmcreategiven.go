package install

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age/armor"
	"go.yaml.in/yaml/v3"
)

// Given is what `vm create` gives a VM's first install over the pinned SSH channel, which its
// cloud-config never carries, and what `install --given` takes: secret inputs, by their keys, in
// the form an answers file gives them (a file input as stowage-file:<base64>), and the front's TLS
// material as PEM. A certificate decides the byo mode.
type Given struct {
	Project string            `yaml:"project"`
	Inputs  map[string]string `yaml:"inputs,omitempty"`
	TLS     *GivenTLS         `yaml:"tls,omitempty"`
}

// GivenTLS is the front's TLS material, PEM.
type GivenTLS struct {
	Cert     string `yaml:"cert,omitempty"`
	Key      string `yaml:"key,omitempty"`
	CA       string `yaml:"ca,omitempty"`
	ClientCA string `yaml:"client_ca,omitempty"`
}

// marshalGiven is g as parseGiven reads it.
func marshalGiven(g *Given) ([]byte, error) { return yaml.Marshal(g) }

// parseGiven strictly decodes a Given file of project; one too large, still encrypted, of another
// project, holding nothing or naming half a certificate is refused.
func parseGiven(raw []byte, project string) (*Given, error) {
	if len(raw) > maxSecretsFile {
		return nil, fmt.Errorf("--given: more than %d bytes", maxSecretsFile)
	}
	t := bytes.TrimSpace(raw)
	if bytes.HasPrefix(t, []byte(armor.Header)) || bytes.HasPrefix(t, []byte("age-encryption.org/")) {
		return nil, errors.New("--given: the file is encrypted: this flag takes it in the clear, over the pinned SSH channel")
	}
	var g Given
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&g); err != nil {
		return nil, fmt.Errorf("--given: %w", err)
	}
	if g.Project != project {
		return nil, fmt.Errorf("--given: the file holds %q's material, not %s's", g.Project, project)
	}
	if len(g.Inputs) == 0 && (g.TLS == nil || *g.TLS == GivenTLS{}) {
		return nil, errors.New("--given: the file holds neither an input nor TLS material")
	}
	if t := g.TLS; t != nil {
		if (t.Cert == "") != (t.Key == "") {
			return nil, errors.New("--given: tls: cert and key are given together")
		}
		if t.CA != "" && t.Cert == "" {
			return nil, errors.New("--given: tls: ca is the CA of its cert, which the file does not hold")
		}
	}
	return &g, nil
}

// The names the TLS checks read a Given's PEM by, in place of the files --cert, --key, --ca and
// --client-ca name, so one set of rules judges both. Each reads as the source in an error.
const (
	givenCert     = "the given file's cert"
	givenKey      = "the given file's key"
	givenCA       = "the given file's ca"
	givenClientCA = "the given file's client_ca"
)

// readPEM reads what a TLS option names: a Given's PEM, else the file.
func (o *Options) readPEM(path string) ([]byte, error) {
	if t := o.givenTLS; t != nil {
		switch path {
		case givenCert:
			return []byte(t.Cert), nil
		case givenKey:
			return []byte(t.Key), nil
		case givenCA:
			return []byte(t.CA), nil
		case givenClientCA:
			return []byte(t.ClientCA), nil
		}
	}
	return os.ReadFile(path)
}

// takeGiven makes raw this install's given material. A flag and an answers file win, as over the
// settings file: a certificate named by a flag leaves out the file's certificate, key and CA, as
// does a mode other than byo, and --ca or --client-ca leaves out its own; each is said.
func (in *Installer) takeGiven(raw []byte) error {
	o := &in.O
	g, err := parseGiven(raw, in.R.Spec.Name)
	if err != nil {
		return err
	}
	// The reset to come wipes an install made here, and the operator's step then removes the only
	// copy of what it took.
	if bootMode(o.cos()) == "recovery" {
		return errors.New("--given: this is Kairos's recovery system, whose reset wipes an install: run it again once the first boot rebooted into the active system")
	}
	o.Given = g
	if len(g.Inputs) > 0 {
		answers := maps.Clone(g.Inputs)
		maps.Copy(answers, o.Answers)
		o.Answers = answers
	}
	t := g.TLS
	if t == nil {
		return nil
	}
	byFlag := o.Cert != "" || o.Key != "" || (o.TLS != "" && o.TLS != "byo")
	var left []string
	if t.Cert != "" {
		if byFlag {
			left = append(left, "cert", "key")
		} else {
			o.Cert, o.Key = givenCert, givenKey
		}
	}
	if t.CA != "" {
		if byFlag || o.CA != "" {
			left = append(left, "ca")
		} else {
			o.CA = givenCA
		}
	}
	if t.ClientCA != "" {
		if o.ClientCA != "" {
			left = append(left, "client_ca")
		} else {
			o.ClientCA = givenClientCA
		}
	}
	o.givenTLS = t
	if len(left) > 0 {
		o.logf("--given: tls: %s left out, as the flags given decide", strings.Join(left, ", "))
	}
	return nil
}

// awaits is what the settings of a VM waiting for g say, for the mode its install takes.
func (g *Given) awaits() string {
	if g.TLS != nil && g.TLS.Cert != "" {
		return "byo"
	}
	return "given"
}

// names lists what g holds, never a value: for the operator's review.
func (g *Given) names() string {
	var out []string
	if g.TLS != nil && g.TLS.Cert != "" {
		out = append(out, "the certificate and its key")
	}
	if g.TLS != nil && g.TLS.CA != "" {
		out = append(out, "its CA")
	}
	if g.TLS != nil && g.TLS.ClientCA != "" {
		out = append(out, "the client CA")
	}
	return strings.Join(append(out, sortedKeys(g.Inputs)...), ", ")
}

// givenMode is the mode the review shows: byo when g holds a certificate, else the settings'.
func (g *Given) givenMode(mode string) string {
	if g != nil && g.TLS != nil && g.TLS.Cert != "" {
		return "byo"
	}
	return mode
}

// givenRetired says what the pinned step cannot do: a boot after an install began retires the one-time
// host key, and ssh then refuses it, the file staying.
const givenRetired = "If the install began and the VM rebooted since, the one-time key is retired and ssh refuses it: check the VM's own key on its console (`ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub`), run the same command without its HostKeyAlias, StrictHostKeyChecking and UserKnownHostsFile options, and remove the file."

// writeGiven writes g where its owner alone reads it, with the known_hosts file naming the one-time
// host key beside it, for givenSteps.
func writeGiven(path, pin string, g *Given, hostKey *seedHostKey) error {
	raw, err := marshalGiven(g)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := writeAtomic(pin, strings.NewReader(seedHostAlias+" "+hostKey.Public+"\n"), 0o600, nil); err != nil {
		return err
	}
	return writeAtomic(path, bytes.NewReader(raw), 0o600, nil)
}

// givenSteps tells how to give a VM's install the file written by writeGiven: piped over SSH to the
// pinned one-time host key, ssh refusing any other, then removed. Run again, it finishes an install
// a failure stopped, and it removes the file only once the install took it. An operator runs it, so
// a key's passphrase may be asked: BatchMode is left out of the arguments every other call shares.
func givenSteps(user, addr, ctl, file, pin, pubKey string) string {
	words := []string{"ssh"}
	args := pinnedSSHArgs(pin, user, addr, pubKey, "sudo", ctl, "install", "--given", "-", "--no-input")
	for n := 0; n < len(args); n++ {
		if args[n] == "-o" && args[n+1] == "BatchMode=yes" {
			n++
			continue
		}
		words = append(words, shellQuote(args[n]))
	}
	return fmt.Sprintf("%s < %s && rm %s %s", strings.Join(words, " "), shellQuote(file), shellQuote(file), shellQuote(pin))
}
