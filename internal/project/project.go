// Package project opens a Stowage project: its spec, its lock and the providers that interpret its
// version and changelog.
package project

import (
	"fmt"
	"path/filepath"

	"github.com/norvexlabs/stowage/internal/changelog"
	"github.com/norvexlabs/stowage/internal/lock"
	"github.com/norvexlabs/stowage/internal/spec"
	"github.com/norvexlabs/stowage/internal/version"
)

// SpecFile is the spec's default name at the project root.
const SpecFile = "stowage.yaml"

// Project is an opened project. Every path in the spec is relative to Root.
type Project struct {
	Root      string
	SpecPath  string
	Spec      *spec.Spec
	Lock      *lock.Lock
	Scheme    version.Scheme
	Source    version.Source
	Changelog changelog.Provider
}

// Open loads the spec (specPath defaults to <root>/stowage.yaml), the lock beside it and the
// version, scheme and changelog providers.
func Open(root, specPath string) (*Project, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if specPath == "" {
		specPath = filepath.Join(abs, SpecFile)
	}
	s, err := spec.Load(specPath)
	if err != nil {
		return nil, err
	}
	p := &Project{Root: abs, SpecPath: specPath, Spec: s}
	if p.Lock, err = lock.Load(filepath.Join(filepath.Dir(specPath), lock.FileName)); err != nil {
		return nil, err
	}
	if p.Scheme, err = version.NewScheme(abs, s.Version.Scheme); err != nil {
		return nil, err
	}
	if p.Source, err = version.NewSource(abs, s.Version.Source); err != nil {
		return nil, err
	}
	if p.Changelog, err = changelog.New(abs, s.Changelog); err != nil {
		return nil, err
	}
	return p, nil
}

// Version returns the project's current version, validated by its scheme.
func (p *Project) Version() (string, error) {
	v, err := p.Source.Get()
	if err != nil {
		return "", fmt.Errorf("version source: %w", err)
	}
	if err := p.Scheme.Validate(v); err != nil {
		return "", fmt.Errorf("version scheme: %w", err)
	}
	return v, nil
}

// Path resolves a spec-relative path.
func (p *Project) Path(rel string) string { return spec.Path(p.Root, rel) }

// CheckTags returns the tag conflicts of the project's channels for version v, or the error of a
// scheme that could not answer. A release whose tags conflict must not be planned or published.
func (p *Project) CheckTags(v string) ([]string, error) {
	tag, err := p.Scheme.Tag(v)
	if err != nil {
		return []string{err.Error()}, nil
	}
	if err := version.CheckVersionTag(tag); err != nil {
		return []string{err.Error()}, nil
	}
	var chs []version.Channel
	for _, name := range p.Spec.ChannelNames() {
		if err := version.CheckTemplate(p.Spec.Channels[name].Tag); err != nil {
			return []string{fmt.Sprintf("channel %s: %v", name, err)}, nil
		}
		chs = append(chs, version.Channel{Name: name, Template: p.Spec.Channels[name].Tag})
	}
	conflicts, err := version.TemplateConflicts(chs, p.Scheme, version.ReservedTags)
	if err != nil {
		return nil, err
	}
	more, err := version.VersionConflicts(chs, p.Scheme, v)
	if err != nil {
		return nil, err
	}
	return append(conflicts, more...), nil
}
