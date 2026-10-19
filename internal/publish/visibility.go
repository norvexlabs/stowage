package publish

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// anonymousReads reports whether every install reads the registry anonymously (registry.pull is
// none): then what a release writes must answer an anonymous read.
func (p *Publisher) anonymousReads() bool {
	return p.Project.Spec.Registry.Pull.Name == "none" && p.Anonymous != nil
}

// privateRepos are the repositories among repos that hold tags the publisher reads and that refuse
// an anonymous read, sorted. A repository not written yet holds none: its first push creates it,
// and the checks after the pushes see it.
func (p *Publisher) privateRepos(ctx context.Context, repos []string) ([]string, error) {
	var private []string
	for _, r := range repos {
		repo, err := p.Registry.Repo(r)
		if err != nil {
			return nil, err
		}
		tags, err := p.Registry.Tags(ctx, repo)
		if err != nil {
			return nil, err
		}
		if len(tags) == 0 {
			continue
		}
		// A registry may answer a refused read as an unknown repository: no tags is a refusal.
		if anon, err := p.Anonymous.Tags(ctx, repo); err != nil || len(anon) == 0 {
			private = append(private, r)
		}
	}
	slices.Sort(private)
	return slices.Compact(private), nil
}

// unreadable are the repositories of refs (what the release wrote, by digest) that refuse an
// anonymous read of it, sorted.
func (p *Publisher) unreadable(ctx context.Context, refs []name.Reference) []string {
	var private []string
	for _, ref := range refs {
		if d, err := p.Anonymous.Head(ctx, ref); err != nil || d == nil {
			private = append(private, ref.Context().String())
		}
	}
	slices.Sort(private)
	return slices.Compact(private)
}

// privateError names the repositories an install could not read, what makes them readable, and
// what a run after it does.
func (p *Publisher) privateError(repos []string, then string) error {
	return fmt.Errorf("registry.pull is none, so every install reads %s anonymously, and %s refuse an anonymous read: make each public (a registry that creates a repository private, as ghcr does, needs it made public once, in its package settings), or give registry.pull an account; %s",
		p.Project.Spec.Registry.Prefix, strings.Join(repos, ", "), then)
}

// releaseRefs are what a release wrote, by digest, in the repositories it writes.
func (p *Publisher) releaseRefs(rel *Ref) ([]name.Reference, error) {
	m := rel.Manifest
	type at struct{ repo, digest string }
	var all []at
	for _, img := range sortedKeys(m.Artifacts.Images) {
		all = append(all, at{p.imageRepo(img), m.Artifacts.Images[img]})
	}
	if m.Artifacts.OS != "" {
		all = append(all, at{p.osRepo(), m.Artifacts.OS})
	}
	for _, f := range sortedKeys(m.Artifacts.Disks) {
		all = append(all, at{p.diskRepo(), m.Artifacts.Disks[f]})
	}
	if c := m.Artifacts.Chart; c != nil {
		all = append(all, at{p.chartRepo(m.Channel), c.Digest})
	}
	if m.Artifacts.Installer != "" {
		all = append(all, at{p.installerRepo(), m.Artifacts.Installer})
	}
	all = append(all, at{p.releaseRepo(), rel.Digest.String()})
	var refs []name.Reference
	for _, a := range all {
		d, err := p.Registry.Digest(a.repo, a.digest)
		if err != nil {
			return nil, err
		}
		refs = append(refs, d)
	}
	return refs, nil
}

// imageRefs are the images planned, by digest.
func (p *Publisher) imageRefs(plans []*imagePlan) ([]name.Reference, error) {
	var refs []name.Reference
	for _, pl := range plans {
		d, err := p.Registry.Digest(p.imageRepo(pl.image), pl.digest)
		if err != nil {
			return nil, err
		}
		refs = append(refs, d)
	}
	return refs, nil
}

// shipRepos are the repositories a release of the channel writes.
func (p *Publisher) shipRepos(plans []*imagePlan, channel string) []string {
	var repos []string
	for _, pl := range plans {
		repos = append(repos, p.imageRepo(pl.image))
	}
	if p.hasOS() {
		repos = append(repos, p.osRepo())
		if len(p.Project.Spec.Shapes.Kairos.Formats()) > 0 {
			repos = append(repos, p.diskRepo())
		}
	}
	if p.hasChart() {
		repos = append(repos, p.chartRepo(channel))
	}
	return append(repos, p.installerRepo(), p.releaseRepo())
}
