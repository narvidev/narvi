package compat

import (
	"bytes"
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

	openEnums := baseManifest.OpenEnumSet()

	var all []Finding
	all = append(all, DiffSurfaceSet(baseManifest, headManifest)...)
	all = append(all, DiffRoutes(in.BaseRoutes, in.HeadRoutes)...)

	var changedSurfaces []string
	anythingChanged := len(all) > 0 || !bytes.Equal(in.BaseManifestRaw, in.HeadManifestRaw)

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
		if !inBase || !inHead {
			// Whole-file add/remove is DiffSurfaceSet's job (rows 37/38);
			// there is nothing to structurally diff here.
			continue
		}

		directive, ok := surfaceDirective(headManifest, path)
		if !ok {
			directive, ok = surfaceDirective(baseManifest, path)
		}
		if !ok {
			return Report{}, fmt.Errorf("surface %s has no manifest row on either side", path)
		}

		findings, err := DiffSurface(directive, baseRaw, headRaw, openEnums)
		if err != nil {
			return Report{}, fmt.Errorf("diff %s: %w", path, err)
		}
		if len(findings) > 0 || !bytes.Equal(baseRaw, headRaw) {
			anythingChanged = true
			changedSurfaces = append(changedSurfaces, path)
		}
		for i := range findings {
			findings[i].Surface = path
		}
		all = append(all, findings...)
	}

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

	all = append(all, CheckVersionAndChangelog(VersionCheckInput{
		BaseVersion:      in.BaseVersion,
		HeadVersion:      in.HeadVersion,
		BaseChangelogTop: baseTop,
		HeadChangelog:    headChangelogSections,
		AnythingChanged:  anythingChanged,
		WorstFinding:     worst,
		ChangedSurfaces:  changedSurfaces,
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
