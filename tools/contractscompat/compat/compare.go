package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Input bundles every already-read byte slice Compare needs. All I/O
// (reading contracts/manifest.json, VERSION, CHANGELOG.md, the schema
// files, and controlplane/testdata/routes.golden, plus the git-archive
// extraction of the merge-base copy) is the CLI's job (tools/
// contractscompat/main.go) -- this package stays a pure function of its
// inputs, which is what makes compat_test.go's corpus fixtures possible
// without a single os/exec or filesystem call.
type Input struct {
	BaseManifestRaw  []byte
	HeadManifestRaw  []byte
	BaseVersion      string
	HeadVersion      string
	BaseChangelogRaw []byte
	HeadChangelogRaw []byte
	BaseRoutes       []byte
	HeadRoutes       []byte
	// BaseSchemaFiles/HeadSchemaFiles are keyed by the contracts-relative
	// path (e.g. "rest/v1/dtos.schema.json"), matching manifest.json's own
	// "path" field exactly.
	BaseSchemaFiles map[string][]byte
	HeadSchemaFiles map[string][]byte
	// Genesis is true when the CLI's own caller (main.go's loadInput) had
	// no real base manifest.json to read at all -- the PR that FIRST adds
	// contracts governance, and (per CI's BASE = merge-base) any PR whose
	// merge-base predates that PR, has no prior state to compare against.
	// BaseManifestRaw is still a parseable Manifest in that case (main.go
	// substitutes HEAD's own copy, so the SURFACE SET lines up and
	// DiffSurfaceSet/validateManifestMatchesFiles have something
	// structurally sound to work with), but E8 (round 3): its RELAXATIONS
	// must never be trusted -- an openEnums pointer or a `retired` status
	// that only exists because it is literally a copy of HEAD's own
	// manifest is not a relaxation any earlier, reviewed PR actually
	// established, which is the entire premise both relaxations rely on
	// (COMPATIBILITY.md's "Relaxations" section, and this file's own C1
	// comment on openEnums below). Compare neutralizes both the moment
	// this is true, regardless of what BaseManifestRaw's own content says.
	Genesis bool
}

// Compare runs the full pipeline (§6.3 design spec §3) and returns every
// classified/fail-closed Finding across every surface, routes.golden, and
// the VERSION/CHANGELOG discipline. A non-nil error means the run could
// not even start classifying (guards 1-4: an empty or manifest/on-disk
// mismatched surface set, or a parse/compile failure) -- the caller
// should treat that as distinct from "the diff has findings" and print it
// plainly rather than as one more Finding line.
func Compare(in Input) (Report, error) {
	baseManifest, err := ParseManifest(in.BaseManifestRaw)
	if err != nil {
		return Report{}, fmt.Errorf("base manifest: %w", err)
	}
	headManifest, err := ParseManifest(in.HeadManifestRaw)
	if err != nil {
		return Report{}, fmt.Errorf("head manifest: %w", err)
	}

	// E8 (round 3): in genesis mode, BaseManifestRaw's own content is a
	// synthesized stand-in for "no prior governance," never a real
	// merge-base -- strip BOTH relaxations from the parsed copy before
	// anything below can read them, rather than trusting whatever HEAD
	// itself happens to say. openEnums empty means every enum in this
	// diff is scored as CLOSED (row 11 MAJOR on P2C); no `retired`
	// status means DiffSurfaceSet's row 37 exemption never fires, so a
	// file disappearing still requires the MAJOR bump row 37 demands.
	if in.Genesis {
		baseManifest.OpenEnums = nil
		for i := range baseManifest.Surfaces {
			if baseManifest.Surfaces[i].Status == StatusRetired {
				baseManifest.Surfaces[i].Status = StatusCurrent
			}
		}
	}

	if err := validateManifestMatchesFiles("base", baseManifest, in.BaseSchemaFiles); err != nil {
		return Report{}, err
	}
	if err := validateManifestMatchesFiles("head", headManifest, in.HeadSchemaFiles); err != nil {
		return Report{}, err
	}

	for path, data := range in.BaseSchemaFiles {
		if err := CompileCheck(data); err != nil {
			return Report{}, fmt.Errorf("base %s does not compile: %w", path, err)
		}
	}
	for path, data := range in.HeadSchemaFiles {
		if err := CompileCheck(data); err != nil {
			return Report{}, fmt.Errorf("head %s does not compile: %w", path, err)
		}
	}

	// C1: openEnums is a relaxation read from the MERGE-BASE manifest
	// only. Per-surface DIRECTION is the same kind of relaxation-adjacent
	// decision and is resolved the same way below, surface by surface --
	// never defaulted to HEAD's own declared value for a surface the base
	// already governs.
	openEnums := baseManifest.OpenEnumSet()

	var all []Finding
	all = append(all, DiffSurfaceSet(baseManifest, headManifest)...)
	all = append(all, DiffRoutes(in.BaseRoutes, in.HeadRoutes)...)

	changedSurfaces := map[string]bool{}
	forceMajorBump := false
	for _, f := range all {
		if f.Surface != "" {
			changedSurfaces[f.Surface] = true
		}
		if f.RuleID == "37" || f.RuleID == "38" {
			// §4: a surface gaining a new vN sibling, or losing a
			// retired one, always requires a MAJOR version bump -- that
			// is the versioned-sibling/retirement DISCIPLINE, a separate
			// concern from row 37/38's own graded compatibility severity
			// (MAJOR/MINOR respectively, which describes whether the
			// file's mere presence change breaks an existing consumer;
			// it usually does not).
			forceMajorBump = true
		}
	}

	// D6/D12: a surface whose BASE row already says "retired" produces NO
	// row-37 finding at all when its file disappears (DiffSurfaceSet's own
	// `if b.Status == StatusRetired { continue }` -- that is the point of
	// the three-PR retirement procedure's own PR C, COMPATIBILITY.md
	// "Retiring an old version"). But the loop above only ever forces a
	// MAJOR bump and marks the surface changed FROM a row-37/38 Finding,
	// so exactly the one deletion this policy sanctions escaped both the
	// MAJOR-bump requirement and the CHANGELOG "### <path>" subsection
	// requirement COMPATIBILITY.md's versioning section says removal
	// "always requires at least a MAJOR bump" for. Determine "a surface
	// disappeared" directly from the two manifests instead of from
	// whichever Finding happened to fire, so the retired case is covered
	// on the exact same terms as the non-retired one (which already gets
	// this from row 37's own MAJOR severity, redundantly with this check).
	baseByPath := map[string]ManifestSurface{}
	for _, s := range baseManifest.Surfaces {
		baseByPath[s.Path] = s
	}
	headByPath := map[string]ManifestSurface{}
	for _, s := range headManifest.Surfaces {
		headByPath[s.Path] = s
	}
	for p := range baseByPath {
		if _, stillPresent := headByPath[p]; !stillPresent {
			changedSurfaces[p] = true
			forceMajorBump = true
		}
	}
	for p := range headByPath {
		if _, existedBefore := baseByPath[p]; !existedBefore {
			changedSurfaces[p] = true
			forceMajorBump = true
		}
	}

	// C1: a surface present in BOTH manifests must keep the same
	// direction -- a manifest-only flip (rule 44) is MAJOR even when the
	// schema content is byte-identical, and is never something a PR can
	// launder by ALSO changing head's own manifest row in the same diff
	// (Compare always reads the comparison direction from base below, so
	// a flip only ever shows up here, as its own dedicated finding).
	// Any manifest row change at all (direction OR status) also marks the
	// surface "changed" for the VERSION/CHANGELOG discipline (C18), even
	// when nothing else about it moved.
	for _, hs := range headManifest.Surfaces {
		bs, ok := baseManifest.SurfaceByPath(hs.Path)
		if !ok {
			continue
		}
		if bs.Direction != hs.Direction {
			all = append(all, Finding{
				RuleID:   "44",
				Severity: SeverityMajor,
				Surface:  hs.Path,
				Pointer:  "#",
				Message:  fmt.Sprintf("manifest direction changed: %s -> %s", bs.Direction, hs.Direction),
			})
			changedSurfaces[hs.Path] = true
		}
		if bs.Status != hs.Status {
			changedSurfaces[hs.Path] = true
		}
	}

	paths := map[string]bool{}
	for p := range in.BaseSchemaFiles {
		paths[p] = true
	}
	for p := range in.HeadSchemaFiles {
		paths[p] = true
	}
	sortedPaths := make([]string, 0, len(paths))
	for p := range paths {
		sortedPaths = append(sortedPaths, p)
	}
	sort.Strings(sortedPaths)

	for _, path := range sortedPaths {
		baseRaw, inBase := in.BaseSchemaFiles[path]
		headRaw, inHead := in.HeadSchemaFiles[path]

		if inBase && !inHead {
			// DiffSurfaceSet's row 37 already covers a whole-file
			// removal; there is nothing left to structurally diff.
			continue
		}

		if !inBase && inHead {
			// C11: a brand-new file was, until now, admitted with its
			// content entirely unexamined -- not walked against the
			// keyword allowlist, and its manifest direction never
			// validated -- so the FIRST later edit to it was the one
			// that discovered a pre-existing disallowed keyword or a bad
			// manifest row, as a fail-closed surprise blocking an
			// unrelated change. Walk it now, at the point it is
			// introduced, instead.
			var headRoot map[string]any
			if err := json.Unmarshal(headRaw, &headRoot); err != nil {
				return Report{}, fmt.Errorf("parse head %s: %w", path, err)
			}
			if err := walkSchema(headRoot, "#", true); err != nil {
				if fc, ok := err.(*FailClosedError); ok {
					fc.Finding.Surface = path
					all = append(all, fc.Finding)
					changedSurfaces[path] = true
					continue
				}
				return Report{}, err
			}
			directive, ok := surfaceDirective(headManifest, path)
			if !ok {
				return Report{}, fmt.Errorf("new surface %s has no manifest row", path)
			}
			if _, err := rootDirection(directive); err != nil {
				return Report{}, fmt.Errorf("new surface %s: %w", path, err)
			}
			changedSurfaces[path] = true
			continue
		}

		// C1: direction comes from the MERGE-BASE manifest row whenever
		// one exists for this path (it always does here, since `path` is
		// present in both BaseSchemaFiles and HeadSchemaFiles, and guard
		// 1/2 above already required the manifest and on-disk sets to
		// match on each side). The head fallback only matters for a
		// surface genuinely new to both sides at once, which cannot
		// happen in this branch.
		directive, ok := surfaceDirective(baseManifest, path)
		if !ok {
			directive, ok = surfaceDirective(headManifest, path)
		}
		if !ok {
			return Report{}, fmt.Errorf("surface %s has no manifest row on either side", path)
		}

		findings, err := DiffSurface(directive, baseRaw, headRaw, openEnums)
		if err != nil {
			return Report{}, fmt.Errorf("diff %s: %w", path, err)
		}
		if len(findings) > 0 || !bytes.Equal(baseRaw, headRaw) {
			changedSurfaces[path] = true
		}
		for i := range findings {
			findings[i].Surface = path
		}
		all = append(all, findings...)
	}

	anythingChanged := len(all) > 0 || !bytes.Equal(in.BaseManifestRaw, in.HeadManifestRaw) || len(changedSurfaces) > 0

	worst := SeverityPatch
	for _, f := range all {
		worst = maxSeverity(worst, f.Severity)
	}

	baseChangelogSections := ParseChangelog(in.BaseChangelogRaw)
	baseTop := ""
	if len(baseChangelogSections) > 0 {
		baseTop = baseChangelogSections[0].Version
	}
	headChangelogSections := ParseChangelog(in.HeadChangelogRaw)

	sortedChanged := make([]string, 0, len(changedSurfaces))
	for p := range changedSurfaces {
		sortedChanged = append(sortedChanged, p)
	}
	sort.Strings(sortedChanged)

	all = append(all, CheckVersionAndChangelog(VersionCheckInput{
		BaseVersion:      in.BaseVersion,
		HeadVersion:      in.HeadVersion,
		BaseChangelogTop: baseTop,
		HeadChangelog:    headChangelogSections,
		AnythingChanged:  anythingChanged,
		WorstFinding:     worst,
		ChangedSurfaces:  sortedChanged,
		ForceMajorBump:   forceMajorBump,
	})...)

	return Report{Findings: all}, nil
}

func surfaceDirective(m Manifest, path string) (string, bool) {
	s, ok := m.SurfaceByPath(path)
	if !ok {
		return "", false
	}
	return s.Direction, true
}

// validateManifestMatchesFiles implements vacuous-pass guards 1-2: the
// manifest's own surface path set must exactly equal the set of schema
// files actually present on disk for that side, and neither may be
// empty -- an empty base directory (e.g. a botched git-archive extraction)
// must be a hard error, never silently "no findings".
func validateManifestMatchesFiles(side string, m Manifest, files map[string][]byte) error {
	if len(files) == 0 {
		return fmt.Errorf("%s: no schema files found on disk -- refusing to treat an empty directory as \"no findings\"", side)
	}
	if len(m.Surfaces) == 0 {
		return fmt.Errorf("%s: manifest.json lists no surfaces", side)
	}
	manifestPaths := map[string]bool{}
	for _, s := range m.Surfaces {
		manifestPaths[s.Path] = true
	}
	for p := range files {
		if !manifestPaths[p] {
			return fmt.Errorf("%s: schema file %s is on disk but has no manifest.json row", side, p)
		}
	}
	for p := range manifestPaths {
		if _, ok := files[p]; !ok {
			return fmt.Errorf("%s: manifest.json lists %s but it is not on disk", side, p)
		}
	}
	return nil
}
