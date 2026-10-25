package publish

import (
	"context"
	"errors"

	"github.com/norvexlabs/stowage/internal/sbom"
)

// Audit checks a release as Verify does, then reads the SBOM each of its images, its installer and
// its OS bundle carries, attested by a trusted key, and scans it with s: an artifact without one
// is in the report as not audited.
func (p *Publisher) Audit(ctx context.Context, channel, v string, revision int, s sbom.Scanner) (*sbom.Report, error) {
	if !p.signed() {
		return nil, errors.New("the project signs no release (signing): its releases carry no SBOM to audit")
	}
	r, err := p.Verify(ctx, channel, v, revision)
	if err != nil {
		return nil, err
	}
	targets, err := sbom.Targets(p.Project.Spec.Registry.Prefix, p.Project.Spec.Name, r.Manifest.Artifacts)
	if err != nil {
		return nil, err
	}
	rep, err := sbom.Audit(ctx, p.Signer.Attestation, s, targets, p.logf)
	if err != nil {
		return nil, err
	}
	m := r.Manifest
	rep.Project, rep.Version, rep.Revision, rep.Channel, rep.Tag = m.Name, m.ProjectVersion, m.Revision, m.Channel, m.Tag
	return &rep, nil
}

// ExportSBOMs checks a release as Verify does, then writes the SBOM each of its images, its
// installer and its OS bundle carries, attested by a trusted key, into dir with its index.
func (p *Publisher) ExportSBOMs(ctx context.Context, channel, v string, revision int, dir string) (sbom.Index, error) {
	if !p.signed() {
		return sbom.Index{}, errors.New("the project signs no release (signing): its releases carry no SBOM")
	}
	r, err := p.Verify(ctx, channel, v, revision)
	if err != nil {
		return sbom.Index{}, err
	}
	targets, err := sbom.Targets(p.Project.Spec.Registry.Prefix, p.Project.Spec.Name, r.Manifest.Artifacts)
	if err != nil {
		return sbom.Index{}, err
	}
	m := r.Manifest
	return sbom.Export(ctx, p.Signer.Attestation, targets, dir, sbom.Index{Project: m.Name, Version: m.ProjectVersion, Revision: m.Revision, Channel: m.Channel, Tag: m.Tag})
}
