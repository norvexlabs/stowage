package publish

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/norvexlabs/stowage/internal/helm"
	"github.com/norvexlabs/stowage/internal/providers"
	"github.com/norvexlabs/stowage/internal/registry"
	"github.com/norvexlabs/stowage/internal/release"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/version"
)

// hasChart reports whether the project publishes a chart: it has the kubernetes shape.
func (p *Publisher) hasChart() bool { return p.Project.Spec.Shapes.Kubernetes != nil }

func (p *Publisher) chartRepo(channel string) string {
	return p.Project.Spec.Registry.Prefix + "/" + release.ChartRepository(p.Project.Spec.Name, channel)
}

// chartManifest is the release's chart as helm pushes one: the project's chart packaged at
// version 0.<release_seq>.0, appVersion the project version, with the release's images by digest
// (and the trusted public keys) in its values. Like the installer it embeds the release's outputs
// and carries no inputs digest: ship makes it again from the manifest and compares digests.
func (p *Publisher) chartManifest(m *release.Manifest) (registry.Manifest, error) {
	s := p.Project.Spec
	dir, err := spec.ProjectPath(p.Project.Root, s.Shapes.Kubernetes.Chart)
	if err != nil {
		return registry.Manifest{}, fmt.Errorf("shapes.kubernetes.chart: %w", err)
	}
	c, err := helm.Load(dir)
	if err != nil {
		return registry.Manifest{}, fmt.Errorf("shapes.kubernetes.chart: %w", err)
	}
	v := helm.Values{
		Registry: s.Registry.Prefix,
		Release:  helm.Release{Name: m.Name, Version: m.ProjectVersion, Revision: m.Revision, Channel: m.Channel, Tag: m.Tag},
		Images:   map[string]helm.Image{},
	}
	for k, d := range m.Artifacts.Images {
		v.Images[k] = helm.Image{Repository: p.imageName(k), Digest: d}
	}
	if v.L4, err = l4Values(s.L4.Routes); err != nil {
		return registry.Manifest{}, err
	}
	if s.Signs() {
		ck, err := providers.CosignKey(s.Signing)
		if err != nil {
			return registry.Manifest{}, fmt.Errorf("signing: %w", err)
		}
		for _, k := range ck.PublicKeys {
			pem, err := p.projectFile(k)
			if err != nil {
				return registry.Manifest{}, fmt.Errorf("signing.public_keys: %w", err)
			}
			v.PublicKeys = append(v.PublicKeys, string(pem))
		}
	}
	cv := version.ChartVersion(m.ReleaseSeq)
	archive, config, err := c.Package(cv, m.ProjectVersion, v)
	if err != nil {
		return registry.Manifest{}, fmt.Errorf("shapes.kubernetes.chart: %w", err)
	}
	return registry.Manifest{
		Config: registry.Blob{MediaType: helm.ConfigMediaType, Data: config},
		Layers: []registry.Blob{{MediaType: helm.ContentMediaType, Data: archive}},
		Annotations: map[string]string{
			"org.opencontainers.image.title":   c.Name,
			"org.opencontainers.image.version": cv,
		},
	}, nil
}

// l4Values are the L4 routes as the chart's values give them: the protocol as a Service names it,
// the upstream split into its service and port.
func l4Values(routes []spec.L4Route) ([]helm.L4Route, error) {
	var out []helm.L4Route
	for _, r := range routes {
		svc, port, _ := strings.Cut(r.Upstream, ":")
		target, err := strconv.Atoi(port)
		if err != nil {
			return nil, fmt.Errorf("l4.routes %s: upstream %q: %w", r.Name, r.Upstream, err)
		}
		out = append(out, helm.L4Route{Name: r.Name, Protocol: strings.ToUpper(r.Protocol), Port: r.Port, Service: svc, TargetPort: target})
	}
	return out, nil
}

// pushChart puts the chart in its channel's repository by digest and signs it, and returns where
// it will be. No tag moves: tagChart moves the version's tag once the release manifest naming it
// is written, so a publication that fails before leaves no chart version behind.
func (p *Publisher) pushChart(ctx context.Context, cm registry.Manifest, m *release.Manifest) (*release.Chart, error) {
	_, h, err := cm.Body()
	if err != nil {
		return nil, err
	}
	repo, err := p.Registry.Repo(p.chartRepo(m.Channel))
	if err != nil {
		return nil, err
	}
	d := repo.Digest(h.String())
	if have, err := p.Registry.Head(ctx, d); err != nil {
		return nil, err
	} else if have == nil {
		p.logf("chart: push %s", registry.Short(h.String()))
		if _, err := p.Registry.PutManifest(ctx, repo, cm, nil); err != nil {
			return nil, err
		}
	}
	if err := p.ensureSigned(ctx, d); err != nil {
		return nil, fmt.Errorf("chart: %w", err)
	}
	return &release.Chart{Repository: release.ChartRepository(m.Name, m.Channel), Version: version.ChartVersion(m.ReleaseSeq), Digest: h.String()}, nil
}

// tagChart moves the chart version's tag to its digest, unless it is there already.
func (p *Publisher) tagChart(ctx context.Context, m *release.Manifest) error {
	c := m.Artifacts.Chart
	repo := p.chartRepo(m.Channel)
	d, err := p.Registry.Digest(repo, c.Digest)
	if err != nil {
		return err
	}
	t, err := p.Registry.Tag(repo, c.Version)
	if err != nil {
		return err
	}
	if have, err := p.Registry.Head(ctx, t); err != nil {
		return err
	} else if have != nil && have.Digest.String() == c.Digest {
		return nil
	}
	p.logf("chart: tag %s → %s:%s", registry.Short(c.Digest), c.Repository, c.Version)
	return p.Registry.Retag(ctx, d, t)
}

// chartVersion matches the tags a chart version takes (version.ChartVersion): the only tags the
// sweep may delete. Any other tag of the repository (a signature's, an operator's) is left.
var chartVersion = regexp.MustCompile(`^0\.[1-9][0-9]*\.0$`)

// sweepCharts deletes the chart versions of a mutable channel that no release manifest of the
// channel names, nor its pointer: those of builds a rebuild replaced (a new build of a mutable tag
// takes a new number) and of runs stopped between a rewrite and a sweep. A chart version is tagged
// only after the manifest naming it is written, and the tags are listed before the manifests are
// read, so a listed version whose manifest exists is always seen named. A manifest that cannot be
// read stops the sweep: its chart cannot be told unnamed. An immutable channel, which never
// rebuilds, is never swept. A failure is a warning: the next ship or prune sweeps again.
func (p *Publisher) sweepCharts(ctx context.Context, channel string) {
	if !p.hasChart() || !p.Project.Spec.Channels[channel].Mutable {
		return
	}
	warn := func(err error) { p.logf("warning: chart versions of %s not swept: %v", p.chartRepo(channel), err) }
	repo, err := p.Registry.Repo(p.chartRepo(channel))
	if err != nil {
		warn(err)
		return
	}
	tags, err := p.Registry.Tags(ctx, repo)
	if err != nil {
		warn(err)
		return
	}
	var versions []string
	for _, t := range tags {
		if chartVersion.MatchString(t) {
			versions = append(versions, t)
		}
	}
	if len(versions) == 0 {
		return
	}
	rs, err := p.scanReleases(ctx, channel, true)
	if err != nil {
		warn(err)
		return
	}
	if ptr, err := p.pointer(ctx, channel); err != nil {
		warn(err)
		return
	} else if ptr != nil {
		rs = append(rs, ptr)
	}
	named := map[string]bool{}
	for _, r := range rs {
		if c := r.Manifest.Artifacts.Chart; c != nil {
			named[c.Version] = true
		}
	}
	for _, v := range versions {
		if named[v] {
			continue
		}
		t, err := p.Registry.Tag(repo.Name(), v)
		if err == nil {
			err = p.Registry.DeleteTag(ctx, t)
		}
		if err != nil {
			p.logf("warning: %s:%s names no release and is left (the next ship or prune retries): %v", repo.Name(), v, err)
			continue
		}
		p.logf("chart: %s:%s names no release: removed", repo.Name(), v)
	}
}
