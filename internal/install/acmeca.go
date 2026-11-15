package install

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/spf13/pflag"

	"github.com/norvexlabs/stowage/internal/spec"
)

// ACMECAVar records the ACME directory the front's issuer uses instead of Let's Encrypt's
// (--acme-ca), in the acme and dns-cloudflare modes alone: a move to another mode removes it.
const ACMECAVar = "STOWAGE_ACME_CA"

// ACMERootFile is the install's copy of the root the ACME directory's TLS chains to
// (--acme-ca-root), PEM. Its directory is what the front mounts (acmeRootMount), as the client
// CA's, so a configuration naming a file since removed never has Docker make a directory in its
// place; acmeRootPath is where the front reads it.
const (
	ACMERootDir   = "stowage-front-acme-ca"
	ACMERootFile  = ACMERootDir + "/root.pem"
	acmeRootMount = "/etc/caddy/acme-ca"
	acmeRootPath  = acmeRootMount + "/root.pem"
)

// acmeCAFlags defines --acme-ca and --acme-ca-root on f, for a project whose front offers acme
// or dns-cloudflare; rerun names the run that keeps or changes them.
func acmeCAFlags(f *pflag.FlagSet, s *spec.Spec, o *Options, rerun string) {
	if !slices.ContainsFunc(s.Front.TLSModes, acmeMode) {
		return
	}
	f.StringVar(&o.ACMECA, "acme-ca", "", "--tls acme or dns-cloudflare: the ACME directory, an https URL, the front's certificates come from instead of Let's Encrypt (recorded: "+rerun+" keeps it; none removes it)")
	f.StringVar(&o.ACMECARoot, "acme-ca-root", "", "with --acme-ca: the root, PEM, its directory's TLS chains to, copied into the install ("+rerun+" keeps it, or replaces it; none removes it, as does a new --acme-ca without it)")
}

// acmeMode reports whether the front obtains its certificates over ACME in mode.
func acmeMode(mode string) bool { return mode == "acme" || mode == "dns-cloudflare" }

// acmeIssuer is what the front's ACME issuer is given: the contact (ACMEEmailVar), the directory
// (ACMECAVar; empty for Let's Encrypt's) and whether the install holds that directory's root
// (ACMERootFile).
type acmeIssuer struct {
	Email, CA string
	Root      bool
}

// checkACMEDirectory refuses an ACME directory that is no https URL with a host, or that holds
// what the Caddyfile would read otherwise than as one word: a space, a quote, a brace (a
// placeholder), a backslash or a #.
func checkACMEDirectory(v string) error {
	if strings.ContainsFunc(v, func(r rune) bool { return r <= ' ' || r == 0x7f || strings.ContainsRune("\"'{}\\#`", r) }) {
		return errors.New("holds a space, a quote, a brace, a backslash or a #: give the directory's URL as its CA publishes it")
	}
	u, err := url.Parse(v)
	switch {
	case err != nil:
		return err
	case u.Scheme != "https" || u.Host == "" || u.Hostname() == "":
		return errors.New("is not an https URL: an ACME directory's, https://host[:port]/path")
	case u.User != nil:
		return errors.New("names a user: an ACME directory takes none")
	}
	return nil
}

// readACMECA checks --acme-ca and --acme-ca-root before anything is decided, and returns the root
// to copy into the install (nil when none is given, or none removes it): an https URL, and CA
// certificates alone. --acme-ca none, which removes the root with the CA, takes no root.
func (in *Installer) readACMECA() ([]byte, error) {
	o := &in.O
	if v := o.ACMECA; v != "" && v != noCA {
		if err := checkACMEDirectory(v); err != nil {
			return nil, fmt.Errorf("--acme-ca %s: %w", v, err)
		}
	}
	if o.ACMECARoot == "" || o.ACMECARoot == noCA {
		return nil, nil
	}
	if o.ACMECA == noCA {
		return nil, refuse("leave out --acme-ca-root", "--acme-ca none removes the ACME CA and its root: leave out --acme-ca-root")
	}
	raw, err := o.readPEM(o.ACMECARoot)
	if err != nil {
		return nil, fmt.Errorf("--acme-ca-root: %w", err)
	}
	certs, err := clientCAs(raw)
	if err != nil {
		return nil, fmt.Errorf("--acme-ca-root %s: %w", o.ACMECARoot, err)
	}
	return certs, nil
}

// acmeVar records in env the ACME CA of a run in mode: --acme-ca's URL; none, or a mode that is
// not acme or dns-cloudflare, removes it; else it stays as recorded. --acme-ca or --acme-ca-root
// given in another mode is refused, naming it, as is a root given while no CA is given or
// recorded.
func (in *Installer) acmeVar(env *EnvFile, mode string) error {
	o := &in.O
	given := func(v string) bool { return v != "" && v != noCA }
	if !acmeMode(mode) {
		for _, f := range []struct{ flag, v string }{{"--acme-ca", o.ACMECA}, {"--acme-ca-root", o.ACMECARoot}} {
			if given(f.v) {
				return refuse("leave out "+f.flag+", or give --tls acme", "%s is for --tls acme or dns-cloudflare, not %s", f.flag, mode)
			}
		}
		env.Delete(ACMECAVar)
		return nil
	}
	switch {
	case o.ACMECA == noCA:
		env.Delete(ACMECAVar)
	case o.ACMECA != "":
		env.Set(ACMECAVar, o.ACMECA)
	}
	if v, _ := env.Get(ACMECAVar); v == "" && given(o.ACMECARoot) {
		return refuse("give --acme-ca URL with it", "--acme-ca-root is the root of --acme-ca's directory, and this install uses Let's Encrypt's: give --acme-ca URL with it")
	}
	return nil
}

// writeACMERoot keeps the install's ACME root (ACMERootFile) with the CA env records, was being
// the CA recorded before this run: root, when given, is written; otherwise the install's goes
// when env records no CA, when --acme-ca-root none, or when the CA is another than was (a root
// belongs to the CA it was given with).
func (in *Installer) writeACMERoot(root []byte, was string, env *EnvFile) error {
	now, _ := env.Get(ACMECAVar)
	switch {
	case now != "" && root != nil:
		// 0755 whatever the umask: the front reads it as a user of its own.
		if err := os.MkdirAll(in.path(ACMERootDir), 0o755); err != nil {
			return err
		}
		if err := os.Chmod(in.path(ACMERootDir), 0o755); err != nil {
			return err
		}
		if err := writeAtomic(in.path(ACMERootFile), bytes.NewReader(root), 0o644, nil); err != nil {
			return err
		}
		in.O.logf("copied the ACME CA's root into %s", in.path(ACMERootFile))
	case now == "" || in.O.ACMECARoot == noCA || now != was:
		if _, err := os.Lstat(in.path(ACMERootDir)); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err := os.RemoveAll(in.path(ACMERootDir)); err != nil {
			return err
		}
		in.O.logf("removed the ACME CA's root %s", in.path(ACMERootFile))
	}
	return nil
}

// heldACMERoot is the install's ACME root as the front reads it, nil when the install holds
// none or records no ACME CA in mode.
func (in *Installer) heldACMERoot(mode, ca string) ([]byte, error) {
	if !acmeMode(mode) || ca == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(in.path(ACMERootFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the ACME CA's root: %w", err)
	}
	return raw, nil
}
