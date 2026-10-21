package install

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// certWarnDays is how long before a byo certificate's end `status` warns.
const certWarnDays = 30

// CertStatus is a byo install's certificate as `status` gives it.
type CertStatus struct {
	Subject  string    `json:"subject"`
	NotAfter time.Time `json:"not_after"`
	// DaysLeft is the whole days left before NotAfter, negative once it passed.
	DaysLeft int `json:"days_left"`
	// Warning is set from certWarnDays before NotAfter, and once it passed.
	Warning string `json:"warning,omitempty"`
}

// certStatus reads the byo certificate the install in dir copied (TLSDir/cert.pem); nil when it
// holds none.
func certStatus(dir string, now time.Time, renew string) (*CertStatus, error) {
	raw, err := os.ReadFile(filepath.Join(dir, TLSDir, "cert.pem"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b, _ := pem.Decode(raw)
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s holds no PEM certificate", filepath.Join(dir, TLSDir, "cert.pem"))
	}
	leaf, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return nil, err
	}
	subject := leaf.Subject.CommonName
	if len(leaf.DNSNames) > 0 {
		subject = strings.Join(leaf.DNSNames, ", ")
	}
	st := &CertStatus{Subject: subject, NotAfter: leaf.NotAfter.UTC(), DaysLeft: int(math.Floor(leaf.NotAfter.Sub(now).Hours() / 24))}
	switch {
	case !now.Before(leaf.NotAfter):
		st.Warning = fmt.Sprintf("the certificate expired on %s: every route refuses its clients; %s", st.NotAfter.Format(time.DateOnly), renew)
	case st.DaysLeft < certWarnDays:
		st.Warning = fmt.Sprintf("the certificate expires in %d day(s), on %s: %s", st.DaysLeft, st.NotAfter.Format(time.DateOnly), renew)
	}
	return st, nil
}

// printCert prints a byo install's certificate line, and its warning.
func (in *Installer) printCert(st *CertStatus) {
	if st == nil {
		return
	}
	fmt.Fprintf(in.O.Out, "%-9s %s, valid until %s (%d days)\n", "cert", st.Subject, st.NotAfter.Format(time.DateOnly), st.DaysLeft)
	if st.Warning != "" {
		fmt.Fprintf(in.O.Out, "warning: %s\n", st.Warning)
	}
}

// renewHint is how a byo install's certificate is renewed: a re-run given the new pair.
func (in *Installer) renewHint() string {
	return fmt.Sprintf("renew it: a re-run of the installer with --dir %s --cert NEW --key NEW", in.O.Dir)
}

// chainComplete checks, for a byo certificate given without --ca, that its chain verifies to the
// host's trusted CAs: a leaf without its intermediate works in browsers, which fetch it, and
// fails every client that does not (curl, Go, Python). A chain ending at a self-signed CA no host
// trusts (a private CA's, or a self-signed certificate) is said, and kept (--ca publishes a CA). Without the host's CAs nothing is
// checked.
func (in *Installer) chainComplete(c *byoCert) error {
	read := in.O.Roots
	if read == nil {
		read = x509.SystemCertPool
	}
	roots, err := read()
	if err != nil || roots == nil {
		in.O.logf("the certificate chain is not checked: this host's trusted CAs could not be read (%v)", err)
		return nil
	}
	var certs []*x509.Certificate
	for rest := c.cert; ; {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			break
		}
		if b.Type == "CERTIFICATE" {
			if crt, err := x509.ParseCertificate(b.Bytes); err == nil {
				certs = append(certs, crt)
			}
		}
	}
	inter := x509.NewCertPool()
	for _, crt := range certs[1:] {
		inter.AddCert(crt)
	}
	_, verr := c.leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	if verr == nil {
		return nil
	}
	last := certs[len(certs)-1]
	// Self-signed: issued by its own subject, under its own key (a CA's flags not required).
	if bytes.Equal(last.RawIssuer, last.RawSubject) && last.CheckSignature(last.SignatureAlgorithm, last.RawTBSCertificate, last.Signature) == nil {
		in.O.logf("the certificate chains to %q, a CA this host does not trust: clients must trust it (--ca publishes it to them)", last.Subject.CommonName)
		return nil
	}
	return fmt.Errorf("the certificate does not verify to a CA this host trusts (%v): the file must hold the whole chain, the certificate then each intermediate (its issuer is %q); a private CA's root is given as --ca", verr, last.Issuer.CommonName)
}

// certEvery is how often, in Hostd.Unit, the host daemon reads a byo install's certificate.
const certEvery = 24 * 3600

// renewHint is how a VM's byo certificate is renewed.
func (h *Hostd) renewHint() string {
	return fmt.Sprintf("renew it: `sudo %s install --tls byo --cert NEW --key NEW`", h.R.Ctl())
}

// certWatch journals a cert.expiring event (its warning as the reason) when the daemon starts and
// once a day after, while a byo install's certificate is within certWarnDays of its end or past
// it.
func (h *Hostd) certWatch(ctx context.Context) {
	for {
		in, _ := h.run()
		if env, err := in.open(); err == nil {
			if mode, _ := env.Get("STOWAGE_TLS_MODE"); mode == "byo" {
				if st, err := certStatus(in.O.Dir, time.Now(), h.renewHint()); err == nil && st != nil && st.Warning != "" {
					if _, err := h.event(Event{Kind: evCertExpiring, Reason: st.Warning}); err != nil {
						h.O.logf("the certificate's warning could not be journaled: %v", err)
					}
				}
			}
		}
		t := time.NewTimer(time.Duration(certEvery) * h.unit())
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}
