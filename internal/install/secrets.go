package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"
	"go.yaml.in/yaml/v3"
)

// SecretsFile is an install's secrets as `secrets export` writes them, encrypted to the operator's
// age keys, and as `install --secrets` and `vm create --secrets` take them back, decrypted: a
// machine lost makes its install again with them (a disaster recovery). Secrets holds what the
// install generated (a rotated secret's next and previous values included) and the secret inputs
// it was given; Provision what the provision hook returned.
type SecretsFile struct {
	Project   string            `yaml:"project"`
	Secrets   map[string]string `yaml:"secrets"`
	Provision map[string]string `yaml:"provision,omitempty"`
}

// maxSecretsFile bounds a secrets file read.
const maxSecretsFile = 1 << 20

// secretKey reports whether the install records k as a secret: one it generates, a rotated
// secret's next or previous value, a secret input, or the Cloudflare token.
func (in *Installer) secretKey(k string) bool {
	for _, s := range in.secrets() {
		if k == s.Key || s.Rotation && (k == NextVar(s.Key) || k == PreviousVar(s.Key) || k == AcceptedVar(s.Key)) {
			return true
		}
	}
	for _, i := range in.R.Spec.Inputs {
		if i.Key == k && i.Secret {
			return true
		}
	}
	return k == CloudflareTokenVar
}

// exportSecrets is the install's SecretsFile: its recorded secrets that are not empty, and the
// provision hook's record.
func (in *Installer) exportSecrets(env *EnvFile) (*SecretsFile, error) {
	f := &SecretsFile{Project: in.R.Spec.Name, Secrets: map[string]string{}}
	for _, k := range env.Keys() {
		if v, _ := env.Get(k); v != "" && in.secretKey(k) {
			f.Secrets[k] = v
		}
	}
	p, err := ReadEnvFile(in.path(ProvisionEnv))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		for _, k := range p.Keys() {
			if f.Provision == nil {
				f.Provision = map[string]string{}
			}
			f.Provision[k], _ = p.Get(k)
		}
	}
	return f, nil
}

// parseRecipients reads age public keys, one each, that can encrypt one file together.
func parseRecipients(list []string, max int) ([]age.Recipient, error) {
	if len(list) == 0 || len(list) > max {
		return nil, fmt.Errorf("recipients: 1 to %d age public keys (age1…)", max)
	}
	var recips []age.Recipient
	for _, s := range list {
		if strings.ContainsAny(s, "\r\n#") {
			return nil, fmt.Errorf("recipient %q: one age public key (age1…)", s)
		}
		r, err := age.ParseRecipients(strings.NewReader(s))
		if err != nil || len(r) != 1 {
			return nil, fmt.Errorf("recipient %q is not an age public key (age1…): an identity's (age-keygen -y prints it)", s)
		}
		recips = append(recips, r[0])
	}
	// Some recipient types do not mix: a header for them all is what a file needs.
	if _, err := age.Encrypt(io.Discard, recips...); err != nil {
		return nil, fmt.Errorf("recipients: %w", err)
	}
	return recips, nil
}

// encryptSecrets is f as YAML, encrypted to recipients, ASCII-armored; ctl is the installer its
// header names.
func encryptSecrets(f *SecretsFile, ctl string, recipients []string) ([]byte, error) {
	recips, err := parseRecipients(recipients, maxRecipients)
	if err != nil {
		return nil, err
	}
	raw, err := yaml.Marshal(f)
	if err != nil {
		return nil, err
	}
	head := fmt.Sprintf("# %s's secrets, written by `%s secrets export`. `install --secrets` takes this file back, decrypted.\n", f.Project, ctl)
	var b bytes.Buffer
	aw := armor.NewWriter(&b)
	w, err := age.Encrypt(aw, recips...)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(w, head); err != nil {
		return nil, err
	}
	if _, err := w.Write(raw); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if err := aw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ExportSecrets is the Docker install's SecretsFile in O.Dir, encrypted to recipients.
func (in *Installer) ExportSecrets(recipients []string) ([]byte, error) {
	if notInstalled(in) {
		return nil, fmt.Errorf("%s holds no install", in.O.Dir)
	}
	env, err := in.open()
	if err != nil {
		return nil, err
	}
	if err := in.sameRelease(context.Background(), env); err != nil {
		return nil, err
	}
	f, err := in.exportSecrets(env)
	if err != nil {
		return nil, err
	}
	return encryptSecrets(f, in.R.Ctl(), recipients)
}

// readSecrets strictly decodes a decrypted SecretsFile; one still encrypted, or another
// project's, is refused.
func (in *Installer) readSecrets(raw []byte) (*SecretsFile, error) {
	if len(raw) > maxSecretsFile {
		return nil, fmt.Errorf("--secrets: more than %d bytes", maxSecretsFile)
	}
	t := bytes.TrimSpace(raw)
	if bytes.HasPrefix(t, []byte(armor.Header)) || bytes.HasPrefix(t, []byte("age-encryption.org/")) {
		return nil, errors.New("--secrets: the file is still encrypted: `age --decrypt -i KEY FILE` gives what this flag takes")
	}
	var f SecretsFile
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("--secrets: %w", err)
	}
	if f.Project != in.R.Spec.Name {
		return nil, fmt.Errorf("--secrets: the file holds %q's secrets, not %s's", f.Project, in.R.Spec.Name)
	}
	return &f, nil
}

// useSecrets makes f the secrets this first install takes: the generated ones it holds are not
// generated again, its secret inputs are given (over the environment, under the flags), and the
// provision hook is given what it returned before. A key of f that is no secret of this release is
// left out, and a generated secret f lacks is generated: both are told.
func (in *Installer) useSecrets(f *SecretsFile) {
	in.restored = f
	for _, k := range sortedKeys(f.Secrets) {
		if !in.secretKey(k) {
			in.O.logf("--secrets: %s is no secret of %s %s: left out", k, in.R.Spec.Name, in.R.Manifest.ProjectVersion)
		}
	}
	for _, s := range in.secrets() {
		if f.Secrets[s.Key] == "" {
			in.O.logf("--secrets: the file has no %s: the install generates a new one", s.Key)
		}
	}
}

// restoredSecret is the value --secrets gives a secret, empty when none.
func (in *Installer) restoredSecret(k string) string {
	if in.restored == nil || !in.secretKey(k) {
		return ""
	}
	return in.restored.Secrets[k]
}

// writeRestoredProvision records the provision hook's values --secrets gives, as the hook's own
// run would: its next run is given them.
func (in *Installer) writeRestoredProvision() error {
	if in.restored == nil || len(in.restored.Provision) == 0 {
		return nil
	}
	h, err := in.hook()
	if err != nil {
		return err
	}
	if h == nil {
		in.O.logf("--secrets: %s %s runs no provision hook: its values in the file are left out", in.R.Spec.Name, in.R.Manifest.ProjectVersion)
		return nil
	}
	// The values are what the hook returned on another install's data: run on this empty database
	// it would be refused them, or mint new ones the restore then leaves matching nothing. Every
	// run — this one, a re-run after a failure or --no-provision, the boot's — waits until the data
	// is loaded (provisionUp). Written before the values, before the stack starts: no failure on
	// the way leaves the values without the wait.
	if err := os.WriteFile(in.path(restoreAwaitedFile), nil, 0o600); err != nil {
		return fmt.Errorf("--secrets: %w", err)
	}
	p := NewEnvFile()
	for _, k := range sortedKeys(in.restored.Provision) {
		p.Set(k, in.restored.Provision[k])
	}
	if err := p.Check(); err != nil {
		return fmt.Errorf("--secrets: provision: %w", err)
	}
	return p.WriteFile(in.path(ProvisionEnv))
}
