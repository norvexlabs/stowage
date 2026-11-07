// Package kit is the handoff kit: what one install issues to another, sealed with a code.
//
// A kit is age, armored, to a scrypt recipient and nothing else. The code is its authentication as
// well as its secrecy: age has no sender authentication, so a kit to a public key could be forged by
// anyone, and a kit sets what an install trusts. age refuses a scrypt file holding any other
// recipient, so a file made without the code does not open with it.
package kit

import (
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"
	"go.yaml.in/yaml/v3"
)

const (
	// Format names this format; a kit of another is refused.
	Format = "stowage-kit/v1"
	// MaxContent bounds a kit's decrypted content.
	MaxContent = 1 << 20
	// MaxSize bounds a kit's file, armored, which holds MaxContent.
	MaxSize = 2 << 20
	// WorkFactor is scrypt's, as log2(N): what Seal is called with, and the most Open pays before it
	// knows the code, so a file cannot make an install allocate more than this costs (256 MiB).
	WorkFactor = 18
)

// Kit is what a kit holds. Inputs are by the target's input names, a file input as
// stowage-file:<base64>; TLS is the target's front's material, PEM.
type Kit struct {
	Format   string            `yaml:"format"`
	For      string            `yaml:"for"`
	Name     string            `yaml:"name,omitempty"`
	Instance string            `yaml:"instance,omitempty"`
	Issued   time.Time         `yaml:"issued,omitempty"`
	Expires  time.Time         `yaml:"expires"`
	Inputs   map[string]string `yaml:"inputs,omitempty"`
	TLS      *TLS              `yaml:"tls,omitempty"`
}

// TLS is a front's material, PEM.
type TLS struct {
	Cert     string `yaml:"cert,omitempty"`
	Key      string `yaml:"key,omitempty"`
	CA       string `yaml:"ca,omitempty"`
	ClientCA string `yaml:"client_ca,omitempty"`
}

// Seal writes k, sealed with code at workFactor (WorkFactor, a test's lower one aside).
func Seal(k *Kit, code string, workFactor int) ([]byte, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, errors.New("no code to seal it with")
	}
	c := *k
	c.Format = Format
	plain, err := yaml.Marshal(&c)
	if err != nil {
		return nil, err
	}
	if len(plain) > MaxContent {
		return nil, fmt.Errorf("it holds %d bytes, and a kit holds %d at most", len(plain), MaxContent)
	}
	if _, err := Parse(plain); err != nil {
		return nil, err
	}
	r, err := age.NewScryptRecipient(code)
	if err != nil {
		return nil, err
	}
	r.SetWorkFactor(workFactor)
	var out bytes.Buffer
	a := armor.NewWriter(&out)
	w, err := age.Encrypt(a, r)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if err := a.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// NewCode makes a code of 160 random bits: a kit can be attacked offline, so its code is never one a
// person chose. It reads as groups of four, for a person to give over a call.
func NewCode() (string, error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	enc := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
	var groups []string
	for len(enc) > 0 {
		n := min(4, len(enc))
		groups = append(groups, enc[:n])
		enc = enc[n:]
	}
	return strings.Join(groups, "-"), nil
}

// Sniff refuses what cannot be a kit, before anyone is asked its code.
func Sniff(raw []byte) error {
	if len(raw) > MaxSize {
		return fmt.Errorf("more than %d bytes", MaxSize)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte(armor.Header)) {
		return errors.New("not a kit: a kit is age, armored")
	}
	return nil
}

// Open decrypts and strictly decodes a kit. It says nothing of who it is for or until when: Check.
func Open(raw []byte, code string) (*Kit, error) {
	if err := Sniff(raw); err != nil {
		return nil, err
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, errors.New("no code to open it with")
	}
	id, err := age.NewScryptIdentity(code)
	if err != nil {
		return nil, err
	}
	id.SetMaxWorkFactor(WorkFactor)
	r, err := age.Decrypt(armor.NewReader(bytes.NewReader(raw)), id)
	if err != nil {
		var none *age.NoIdentityMatchError
		if errors.As(err, &none) {
			return nil, errors.New("this code does not open it: a wrong code, a changed file, or a file not sealed with a code")
		}
		return nil, fmt.Errorf("it does not open: %w", err)
	}
	plain, err := io.ReadAll(io.LimitReader(r, MaxContent+1))
	if err != nil {
		return nil, fmt.Errorf("it does not open, the file changed or cut short: %w", err)
	}
	if len(plain) > MaxContent {
		return nil, fmt.Errorf("it holds more than %d bytes", MaxContent)
	}
	return Parse(plain)
}

// Parse strictly decodes a kit's content: one document of this format, naming its project and when
// it expires. The format is read first, so a newer one is refused as that, not by a field.
func Parse(plain []byte) (*Kit, error) {
	var head struct {
		Format string `yaml:"format"`
	}
	if err := yaml.Unmarshal(plain, &head); err != nil {
		return nil, fmt.Errorf("its content: %w", err)
	}
	if head.Format != Format {
		return nil, fmt.Errorf("its format is %q, and this installer reads %s", head.Format, Format)
	}
	var k Kit
	dec := yaml.NewDecoder(bytes.NewReader(plain))
	dec.KnownFields(true)
	if err := dec.Decode(&k); err != nil {
		return nil, fmt.Errorf("its content: %w", err)
	}
	var more yaml.Node
	if err := dec.Decode(&more); !errors.Is(err, io.EOF) {
		return nil, errors.New("its content: more than one document")
	}
	if k.For == "" {
		return nil, errors.New("its content names no project (for)")
	}
	if k.Expires.IsZero() {
		return nil, errors.New("its content names no expiry (expires)")
	}
	return &k, nil
}

// Check refuses a kit for another project, or at or past its expiry. The expiry is judged by the
// clock of the machine opening it, and is no revocation.
func (k *Kit) Check(project string, now time.Time) error {
	if k.For != project {
		return fmt.Errorf("this kit is for %q, not %s", k.For, project)
	}
	if !now.Before(k.Expires) {
		return fmt.Errorf("this kit expired at %s (by this machine's clock, %s): ask its issuer for a new one", k.Expires.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}
	return nil
}
