package version

import (
	"fmt"
	"regexp"
	"strings"
)

// revisionLike is any ending that reads as a revision suffix: a version whose tag ends so is
// refused, whatever the digits, so no version tag can be mistaken for one.
var revisionLike = regexp.MustCompile(`-r[0-9]+$`)

// revisionSuffix is exactly what ChannelTag appends: -r<N> for N >= 2, without a leading zero.
var revisionSuffix = regexp.MustCompile(`-r(?:[2-9]|[1-9][0-9]+)$`)

// ChannelTag renders a channel's tag template for a release. The version's tag (the version
// itself, Scheme.Tag) replaces {v}; a revision above 1 is appended to it as -r<N>, so `v{v}-dev`
// gives `v1.2.0-r2-dev`.
//
// The mapping (version, revision) → tag is injective because a version whose own tag already ends
// like a revision suffix is refused: otherwise 1.2.0 at revision 2 and the pre-release 1.2.0-r2
// would share a tag.
func ChannelTag(template, schemeTag string, revision int) (string, error) {
	if strings.Count(template, "{v}") != 1 {
		return "", fmt.Errorf("channel tag template %q must contain {v} exactly once", template)
	}
	if revision < 1 {
		return "", fmt.Errorf("revision %d: revisions start at 1", revision)
	}
	if err := CheckVersionTag(schemeTag); err != nil {
		return "", err
	}
	v := schemeTag
	if revision > 1 {
		v = fmt.Sprintf("%s-r%d", v, revision)
	}
	t := strings.Replace(template, "{v}", v, 1)
	if !ValidTag(t) {
		return "", fmt.Errorf("channel tag %q is not a legal OCI tag", t)
	}
	return t, nil
}

// ChartVersion is the Helm chart version of a release: 0.<releaseSeq>.0, where releaseSeq numbers
// the channel's releases in publication order. Each channel publishes its own chart repository, so
// the sequence alone orders and identifies its charts — revisions included, which SemVer could not
// order (build metadata has no precedence) — and the project version rides as appVersion.
func ChartVersion(releaseSeq int) string { return fmt.Sprintf("0.%d.0", releaseSeq) }

// CheckVersionTag refuses a version tag that ends like a revision suffix: 1.2.0-r2 at revision 1
// and 1.2.0 at revision 2 would share a tag.
func CheckVersionTag(tag string) error {
	if revisionLike.MatchString(tag) {
		return fmt.Errorf("version tag %q ends like a revision suffix (-r<N>), which would make two releases share a tag", tag)
	}
	return nil
}

// CheckTemplate validates a channel tag template on its own: `{v}` exactly once, and only
// characters a tag may hold around it.
func CheckTemplate(template string) error {
	if strings.Count(template, "{v}") != 1 {
		return fmt.Errorf("channel tag template %q must contain {v} exactly once", template)
	}
	if t := strings.Replace(template, "{v}", "0", 1); !ValidTag(t) {
		return fmt.Errorf("channel tag template %q renders tags that are not legal OCI tags", template)
	}
	return nil
}

// Channel is what conflict checking needs from a channel: its name and tag template.
type Channel struct {
	Name     string
	Template string
}

// ReservedTags are tags Stowage moves itself, which no release may write: the floating `latest`.
// The disk repository's `latest-<format>` needs no entry of its own — a release's disk tags are
// `<tag>-<format>`, which spell it only when <tag> is `latest`. Channel pointer tags (the channel
// names) are reserved as well and added by TemplateConflicts.
var ReservedTags = []string{"latest"}

// TemplateConflicts reports what the channels' tag templates allow for ANY version: two channels
// sharing a template, or a template that spells a reserved tag or a channel pointer tag for some
// version the scheme accepts. Channels whose template CheckTemplate refuses are left to it. The
// error is set when the scheme could not answer a question the check needs.
func TemplateConflicts(channels []Channel, s Scheme, reserved []string) ([]string, error) {
	channels = validTemplates(channels)
	var out []string
	for i, a := range channels {
		for j, b := range channels {
			if i < j && a.Template == b.Template {
				out = append(out, fmt.Sprintf("channels %s and %s share the tag template %q", a.Name, b.Name, a.Template))
			}
		}
	}
	type reservedTag struct{ tag, what string }
	var tags []reservedTag
	for _, n := range channels {
		tags = append(tags, reservedTag{n.Name, "the pointer tag of channel " + n.Name})
	}
	for _, r := range reserved {
		tags = append(tags, reservedTag{r, "the reserved tag " + r})
	}
	for _, r := range tags {
		for _, c := range channels {
			other, ok, err := versionUnder(c.Template, r.tag, s)
			if err != nil {
				return out, err
			}
			if ok {
				out = append(out, fmt.Sprintf("channel %s would write %s for version %s", c.Name, r.what, other))
			}
		}
	}
	return out, nil
}

// VersionConflicts reports where a tag that version v writes on one channel, at revision 1 or
// above, is also a tag another channel writes for some version the scheme accepts. Every channel
// publishes into the same repositories, so such a tag would be overwritten or refused.
// The version must be one that can be released (Scheme.Tag and CheckVersionTag accept it); that
// is the caller's check, reported once against the version rather than once per channel.
func VersionConflicts(channels []Channel, s Scheme, v string) ([]string, error) {
	channels = validTemplates(channels)
	vt, err := s.Tag(v)
	if err != nil {
		return nil, err
	}
	if err := CheckVersionTag(vt); err != nil {
		return nil, err
	}
	var out []string
	for _, a := range channels {
		for _, rev := range []int{1, 2} {
			tag, err := ChannelTag(a.Template, vt, rev)
			if err != nil {
				out = append(out, fmt.Sprintf("channel %s: %v", a.Name, err))
				break
			}
			for _, b := range channels {
				if b.Name == a.Name || a.Template == b.Template {
					continue
				}
				other, ok, err := versionUnder(b.Template, tag, s)
				if err != nil {
					return out, err
				}
				if ok {
					out = append(out, fmt.Sprintf("channel %s tag %q is also channel %s's tag for version %s", a.Name, tag, b.Name, other))
				}
			}
		}
	}
	return out, nil
}

func validTemplates(channels []Channel) []Channel {
	var out []Channel
	for _, c := range channels {
		if CheckTemplate(c.Template) == nil {
			out = append(out, c)
		}
	}
	return out
}

// versionUnder returns a version the scheme accepts whose tag under template (at any revision)
// is tag. An error means the scheme could not answer.
func versionUnder(template, tag string, s Scheme) (string, bool, error) {
	pre, suf, _ := strings.Cut(template, "{v}")
	if len(tag) < len(pre)+len(suf) || !strings.HasPrefix(tag, pre) || !strings.HasSuffix(tag, suf) {
		return "", false, nil
	}
	middle := tag[len(pre) : len(tag)-len(suf)]
	// The tag is a version at revision 1, or a version followed by exactly the suffix ChannelTag
	// appends for revision 2 and above. Either way the version itself never ends like a revision.
	candidates := []string{middle}
	if m := revisionSuffix.FindStringIndex(middle); m != nil {
		candidates = append(candidates, middle[:m[0]])
	}
	for _, c := range candidates {
		if c == "" || !ValidTag(c) || revisionLike.MatchString(c) {
			continue
		}
		err := s.Validate(c)
		if IsInvalid(err) {
			continue
		}
		if err != nil {
			return "", false, fmt.Errorf("checking whether %q is a version: %w", c, err)
		}
		return c, true, nil
	}
	return "", false, nil
}
