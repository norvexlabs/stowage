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

	"github.com/norvexlabs/stowage/internal/providers"
)

// SecretsFile is an install's secrets as `secrets export` writes them, encrypted to the operator's
// age keys, and as `install --secrets` and `vm create --secrets` take them back, decrypted: a
// machine lost makes its install again with them (a disaster recovery). Secrets holds what the
// install generated (a rotated secret's next and previous values included) and the secret inputs
// it was given; Provision what the provision hook returned; FrontCA the front's local CA, for an
// install in the internal mode whose front made one. Export marks a file `secrets export` wrote:
// only such a file makes an install made again (made_again, its data awaited); a file without it
// is secrets a deployment seeds into a new install, which takes no provision values.
type SecretsFile struct {
	Project   string            `yaml:"project"`
	Export    bool              `yaml:"export,omitempty"`
	Secrets   map[string]string `yaml:"secrets"`
	Provision map[string]string `yaml:"provision,omitempty"`
	FrontCA   *FrontCA          `yaml:"front_ca,omitempty"`
}

// newExport is the SecretsFile `secrets export` starts from, on every shape: marked an export.
func newExport(project string) *SecretsFile {
	return &SecretsFile{Project: project, Export: true, Secrets: map[string]string{}}
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
		if i.Key == k && i.Secret && (!in.kube || i.OnKubernetes()) {
			return true
		}
	}
	// The Cloudflare token is the Docker front's: a Kubernetes release's ingress takes none.
	return k == CloudflareTokenVar && !in.kube
}

// exportSecrets is the install's SecretsFile: its recorded secrets that are not empty, the
// provision hook's record and the front's local CA.
func (in *Installer) exportSecrets(ctx context.Context, env *EnvFile) (*SecretsFile, error) {
	f := newExport(in.R.Spec.Name)
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
	// The secrets go out whatever the front's state: a recovery needs them most.
	if f.FrontCA, err = in.frontCA(ctx, env); err != nil {
		in.O.logf("warning: %v: the export carries no front CA, and a machine made again from it issues from a new root", err)
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
		return nil, refusal("", err) // the keys given: nothing changed
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
	env, err := in.open()
	if err != nil {
		return nil, err
	}
	if err := in.sameRelease(context.Background(), env); err != nil {
		return nil, err
	}
	f, err := in.exportSecrets(context.Background(), env)
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
	if !f.Export {
		var keys []string
		if len(f.Provision) > 0 {
			keys = append(keys, "provision")
		}
		if f.FrontCA != nil {
			keys = append(keys, "front_ca")
		}
		if len(keys) > 0 {
			named := strings.Join(keys, " and ")
			return nil, fmt.Errorf("--secrets: %s: only a secrets export (export: true) gives %s, with its data, never a new install's secrets file: for an export a release built with Stowage 0.48.0 or earlier wrote, which lacks the mark, add the line `export: true`; for a new install's file, remove %s", named, named, named)
		}
	}
	if ca := f.FrontCA; ca != nil {
		if _, err := parseFrontCA([]byte(ca.Cert), []byte(ca.Key)); err != nil {
			return nil, fmt.Errorf("--secrets: front_ca: %w", err)
		}
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

// madeAgainRecords writes the records of a first install: made_again when it is given a secrets
// export (Export: a seeded file makes a plain install), and restore_awaited when that install awaits its data — its release runs backups (a
// restore loads them), or its hook is given the file's provision values: the machine made again
// is not the old one until a backup of it is loaded (restore), or the operator says it is
// (provision, reset-db). One an earlier attempt left (its .env never written) is removed when
// this install makes none. Run before .env, so no install exists without its records. It says
// which it wrote.
func (in *Installer) madeAgainRecords() (written []string, err error) {
	again := in.restored != nil && in.restored.Export
	if in.restored != nil && !again {
		in.O.logf("--secrets: a new install's secrets (no export: true): a plain install, not made again. If the file is an export a release built with Stowage 0.48.0 or earlier wrote, this machine is not made again: give it no backup settings, add the line `export: true` to the file, and make the machine again from it (`uninstall` this install on Docker, or create the VM again; `--secrets` is refused on an install that exists)")
	}
	var h *providers.ProvisionHook
	if again {
		if h, err = in.hook(); err != nil {
			return nil, fmt.Errorf("--secrets: %w", err)
		}
	}
	awaits := again && (providers.RunsBackups(in.R.Spec) || len(in.restored.Provision) > 0 && h != nil)
	for _, r := range []struct {
		file string
		on   bool
	}{{madeAgainFile, again}, {restoreAwaitedFile, awaits}} {
		if !r.on {
			if err := os.Remove(in.path(r.file)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return written, fmt.Errorf("a record an earlier attempt left: %w", err)
			}
			continue
		}
		if err := os.WriteFile(in.path(r.file), nil, 0o600); err != nil {
			return written, fmt.Errorf("--secrets: %w", err)
		}
		written = append(written, r.file)
	}
	// The hook's values the file gives belong with the wait: written with it, before .env.
	switch wrote, err := in.writeRestoredProvision(h); {
	case err != nil:
		return written, err
	case wrote:
		written = append(written, ProvisionEnv)
	}
	if awaits && h == nil { // a hook's provisionUp says it otherwise
		in.O.logf("an install made again by --secrets awaits its data: %s", in.awaitedHint())
	}
	return written, nil
}

// writeRestoredProvision records the provision hook h's values --secrets gives, as the hook's own
// run would: its next run is given them; the install awaits its data (madeAgainRecords). It says
// whether it wrote them.
func (in *Installer) writeRestoredProvision(h *providers.ProvisionHook) (bool, error) {
	if in.restored == nil || len(in.restored.Provision) == 0 {
		return false, nil
	}
	if h == nil {
		in.O.logf("--secrets: %s %s runs no provision hook: its values in the file are left out", in.R.Spec.Name, in.R.Manifest.ProjectVersion)
		return false, nil
	}
	// The values are what the hook returned on another install's data: run on this empty database
	// it would be refused them, or mint new ones the restore then leaves matching nothing. Every
	// run — this one, a re-run after a failure or --no-provision, the boot's — waits until the data
	// is loaded (provisionUp).
	p := NewEnvFile()
	for _, k := range sortedKeys(in.restored.Provision) {
		p.Set(k, in.restored.Provision[k])
	}
	if err := p.Check(); err != nil {
		return false, fmt.Errorf("--secrets: provision: %w", err)
	}
	if err := p.WriteFile(in.path(ProvisionEnv)); err != nil {
		return false, err
	}
	return true, nil
}
