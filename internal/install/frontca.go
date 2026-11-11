package install

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
)

// FrontCA is the front's local CA (`--tls internal`) as a SecretsFile carries it: its root's
// certificate and private key, PEM. A machine made again with them issues from the same root, so
// every client that trusts it (a browser, a service publish_ca gives it to) still does.
type FrontCA struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

// The local CA's directory in the front's data volume (Caddy's), and where a restored root waits
// in the install's directory until the front's volume holds it.
const (
	localCADir       = "/data/caddy/pki/authorities/local"
	localLeavesDir   = "/data/caddy/certificates/local"
	FrontCARestore   = ".stowage-front-ca"
	frontCAReadShell = `d=` + localCADir + `; [ -e "$d/root.crt" ] || exit 0; cat "$d/root.crt" "$d/root.key"`
	// The restore writes the root, then drops the intermediate and the certificates an earlier
	// root issued, which Caddy makes again from the root it finds.
	frontCAWriteShell = `set -e; umask 077; d=` + localCADir + `; mkdir -p "$d"; cat >"$d/.restore"; ` +
		`sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' "$d/.restore" >"$d/.root.crt"; ` +
		`sed -n '/-----BEGIN [A-Z ]*PRIVATE KEY-----/,/-----END [A-Z ]*PRIVATE KEY-----/p' "$d/.restore" >"$d/.root.key"; ` +
		`rm -f "$d/.restore"; [ -s "$d/.root.crt" ] && [ -s "$d/.root.key" ]; ` +
		`rm -f "$d/intermediate.crt" "$d/intermediate.key"; rm -rf ` + localLeavesDir + `; ` +
		`mv "$d/.root.key" "$d/root.key"; mv "$d/.root.crt" "$d/root.crt"`
)

// parseFrontCA reads a root's PEM certificate and key: a CA certificate, and the key that goes
// with it.
func parseFrontCA(cert, key []byte) (*FrontCA, error) {
	b, _ := pem.Decode(cert)
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, errors.New("the certificate is not PEM")
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the certificate: %w", err)
	}
	if !c.IsCA {
		return nil, errors.New("the certificate is not a CA's")
	}
	if _, err := tls.X509KeyPair(cert, key); err != nil {
		return nil, fmt.Errorf("the key does not go with the certificate: %w", err)
	}
	return &FrontCA{Cert: string(cert), Key: string(key)}, nil
}

// splitPEM is the certificate and the private key of out, which holds a root's two PEM blocks.
func splitPEM(out []byte) (cert, key []byte, err error) {
	for rest := out; ; {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		switch {
		case b.Type == "CERTIFICATE" && cert == nil:
			cert = pem.EncodeToMemory(b)
		case strings.HasSuffix(b.Type, "PRIVATE KEY") && key == nil:
			key = pem.EncodeToMemory(b)
		default:
			return nil, nil, fmt.Errorf("an unexpected PEM block %s", b.Type)
		}
	}
	if cert == nil || key == nil {
		return nil, nil, errors.New("no certificate and key")
	}
	return cert, key, nil
}

// frontCA reads the front's local CA from its data volume, in a container of the front's image
// (the stack running or not): nil when the install is not in the internal mode, or when its front
// never made one.
func (in *Installer) frontCA(ctx context.Context, env *EnvFile) (*FrontCA, error) {
	if mode, _ := env.Get("STOWAGE_TLS_MODE"); mode != "internal" || !in.R.Spec.Front.Declared() {
		return nil, nil
	}
	// As root reading alone, whoever holds the volume's files yet (a start not yet past
	// prepareFront): a file it cannot read is never taken for no CA.
	out, err := in.O.Docker.Run(ctx, Cmd{Args: append(in.composeBase(), "run", "--rm", "--no-deps", "-T", "--user", "0", "--cap-add", "DAC_READ_SEARCH",
		"--entrypoint", "sh", FrontService, "-c", frontCAReadShell),
		Env: env.Environ(in.O.environ())})
	if err != nil {
		return nil, fmt.Errorf("the front's local CA: %w", err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		in.O.logf("the front has made no local CA yet: the export carries none")
		return nil, nil
	}
	cert, key, err := splitPEM(out)
	if err == nil {
		var ca *FrontCA
		if ca, err = parseFrontCA(cert, key); err == nil {
			return ca, nil
		}
	}
	return nil, fmt.Errorf("the front's local CA (%s): %w", localCADir, err)
}

// writeRestoredFrontCA records the front's local CA --secrets gives, for an install in the
// internal mode: the stack's next start puts it in the front's volume (loadFrontCA). In another
// mode it is left out, and said.
func (in *Installer) writeRestoredFrontCA(mode string) error {
	if in.restored == nil || in.restored.FrontCA == nil {
		return nil
	}
	switch {
	case !in.R.Spec.Front.Declared():
		in.O.logf("--secrets: the front's local CA is left out: %s %s has no front", in.R.Spec.Name, in.R.Manifest.ProjectVersion)
		return nil
	case mode != "internal":
		in.O.logf("--secrets: the front's local CA is left out: this install's TLS mode is %s, not internal", mode)
		return nil
	}
	// Each block on lines of its own: the write splits them by their lines.
	ca := in.restored.FrontCA
	return writePrivate(in.path(FrontCARestore), []byte(strings.TrimSpace(ca.Cert)+"\n"+strings.TrimSpace(ca.Key)+"\n"))
}

// loadFrontCA puts the local CA writeRestoredFrontCA recorded in the front's data volume, before
// the front starts with it: a front container already there is removed (it holds another root),
// the root written, and what an earlier root issued dropped. The record goes once the volume
// holds it; until then each start tries again.
func (in *Installer) loadFrontCA(ctx context.Context, env *EnvFile) error {
	raw, err := os.ReadFile(in.path(FrontCARestore))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := in.Compose(ctx, env, in.O.Log, "rm", "--stop", "--force", FrontService); err != nil {
		return fmt.Errorf("the front's local CA: %w", err)
	}
	// As the front's user, in its volume once the start's prepareFront gave it to that user: no
	// link there leads the write elsewhere.
	if _, err := in.O.Docker.Run(ctx, Cmd{Args: append(in.composeBase(), "run", "--rm", "--no-deps", "-T", "--entrypoint", "sh", FrontService, "-c", frontCAWriteShell),
		Env: env.Environ(in.O.environ()), Stdin: bytes.NewReader(raw)}); err != nil {
		return fmt.Errorf("the front's local CA, into its volume: %w", err)
	}
	in.O.logf("the front's local CA is the one --secrets gave")
	return os.Remove(in.path(FrontCARestore))
}
