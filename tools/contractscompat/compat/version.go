package compat

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// SemVer is a parsed "x.y.z" version, the shape contracts/VERSION and
// contracts/package.json's own "version" field both use.
type SemVer struct {
	Major, Minor, Patch int
}

var semverRE = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)

// ParseSemVer parses a strict "MAJOR.MINOR.PATCH" string (no pre-release
// or build metadata -- neither contracts/VERSION nor package.json's
// "version" field ever carries one).
func ParseSemVer(s string) (SemVer, error) {
	s = strings.TrimSpace(s)
	m := semverRE.FindStringSubmatch(s)
	if m == nil {
		return SemVer{}, fmt.Errorf("%q is not a plain MAJOR.MINOR.PATCH version", s)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	return SemVer{major, minor, patch}, nil
}

func (v SemVer) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Less reports whether v sorts strictly before o.
func (v SemVer) Less(o SemVer) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

// BumpClass classifies what kind of bump `to` represents relative to v
// (v assumed to be the base version, to the head version, and to already
// known to be strictly greater than v): SeverityMajor if the major
// component increased, else SeverityMinor if minor increased, else
// SeverityPatch.
func (v SemVer) BumpClass(to SemVer) Severity {
	switch {
	case to.Major > v.Major:
		return SeverityMajor
	case to.Minor > v.Minor:
		return SeverityMinor
	default:
		return SeverityPatch
	}
}

// RequiredBump maps a report's worst finding severity to the minimum
// semver bump class §6.3 design spec §4's CI rule requires: any MAJOR (or
// FAIL-CLOSED, which never gets this far since the run aborts first, but
// mapped defensively anyway) needs at least a major bump, any MINOR needs
// at least a minor bump, and PATCH-only changes need at least a patch
// bump.
func RequiredBump(worst Severity) Severity {
	switch worst {
	case SeverityMajor, SeverityFailClosed:
		return SeverityMajor
	case SeverityMinor:
		return SeverityMinor
	default:
		return SeverityPatch
	}
}

// changelogHeadingRE matches a Keep-a-Changelog "## [x.y.z]" section
// heading; changelogSubsectionRE matches this repo's own "### <surface
// path>" subsection heading directly beneath one.
var changelogHeadingRE = regexp.MustCompile(`^##\s+\[(\d+\.\d+\.\d+)\]`)
var changelogSubsectionRE = regexp.MustCompile(`^###\s+(\S+)`)

// ChangelogSection is one "## [x.y.z]" section of CHANGELOG.md, parsed by
// ParseChangelog into an ordered list (newest first, matching the file's
// own required order) plus, per version, the set of "### <path>"
// subsection names beneath it.
type ChangelogSection struct {
	Version    string
	Subsurface map[string]bool
}

// ParseChangelog parses CHANGELOG.md's own top-level version headings.
func ParseChangelog(data []byte) []ChangelogSection {
	var sections []ChangelogSection
	var cur *ChangelogSection
	for _, line := range strings.Split(string(data), "\n") {
		if m := changelogHeadingRE.FindStringSubmatch(line); m != nil {
			sections = append(sections, ChangelogSection{Version: m[1], Subsurface: map[string]bool{}})
			cur = &sections[len(sections)-1]
			continue
		}
		if cur == nil {
			continue
		}
		if m := changelogSubsectionRE.FindStringSubmatch(line); m != nil {
			cur.Subsurface[m[1]] = true
		}
	}
	return sections
}

// VersionCheckInput bundles everything CheckVersionAndChangelog needs to
// enforce §6.3 design spec §4's CI rule: "if any schema file, manifest.json
// or routes.golden differs from base, then VERSION must be strictly
// greater, the bump class >= the highest finding class, CHANGELOG's first
// heading == new VERSION, and that section has a ### subsection for every
// changed surface. If nothing changed, VERSION and the CHANGELOG top
// heading must be unchanged."
type VersionCheckInput struct {
	BaseVersion      string
	HeadVersion      string
	BaseChangelogTop string // "" if base CHANGELOG has no heading yet
	HeadChangelog    []ChangelogSection
	AnythingChanged  bool
	WorstFinding     Severity
	ChangedSurfaces  []string // contracts-relative paths with any diff (schema content, manifest row, or routes.golden)
}

// CheckVersionAndChangelog returns Findings (always Severity Major -- this
// is a hard release-discipline gate, not a graded compatibility class) for
// every violation of the rule quoted on VersionCheckInput.
func CheckVersionAndChangelog(in VersionCheckInput) []Finding {
	var findings []Finding
	fail := func(msg string) {
		findings = append(findings, Finding{RuleID: "version-changelog", Severity: SeverityMajor, Pointer: "contracts/VERSION", Message: msg})
	}

	if !in.AnythingChanged {
		if in.HeadVersion != in.BaseVersion {
			fail(fmt.Sprintf("no contracts content changed, but VERSION changed (%s -> %s)", in.BaseVersion, in.HeadVersion))
		}
		headTop := ""
		if len(in.HeadChangelog) > 0 {
			headTop = in.HeadChangelog[0].Version
		}
		if headTop != in.BaseChangelogTop {
			fail(fmt.Sprintf("no contracts content changed, but CHANGELOG's top heading changed (%s -> %s)", in.BaseChangelogTop, headTop))
		}
		return findings
	}

	base, err := ParseSemVer(in.BaseVersion)
	if err != nil {
		fail(fmt.Sprintf("base contracts/VERSION invalid: %v", err))
		return findings
	}
	head, err := ParseSemVer(in.HeadVersion)
	if err != nil {
		fail(fmt.Sprintf("head contracts/VERSION invalid: %v", err))
		return findings
	}
	if !base.Less(head) {
		fail(fmt.Sprintf("contracts changed but VERSION was not bumped strictly upward (%s -> %s)", base, head))
		return findings
	}

	required := RequiredBump(in.WorstFinding)
	actual := base.BumpClass(head)
	if actual < required {
		fail(fmt.Sprintf("VERSION bump (%s -> %s, a %s bump) is smaller than the highest finding class (%s) requires", base, head, actual, required))
	}

	if len(in.HeadChangelog) == 0 || in.HeadChangelog[0].Version != head.String() {
		fail(fmt.Sprintf("CHANGELOG.md's first \"## [x.y.z]\" heading must equal the new VERSION (%s)", head))
		return findings
	}

	top := in.HeadChangelog[0]
	for _, surface := range in.ChangedSurfaces {
		if !top.Subsurface[surface] {
			fail(fmt.Sprintf("CHANGELOG.md's %s section is missing a \"### %s\" subsection", top.Version, surface))
		}
	}
	return findings
}
