package compat

// DiffSurfaceSet implements rows 37-38: a schema file (a manifest.json
// row) that disappears between base and head is MAJOR unless the BASE
// manifest already marked it "retired" (the two-PR relaxation discipline:
// PR A sets deprecated, PR B sets retired, PR C deletes -- only PR C's
// diff, comparing against a base where the row already says retired,
// is exempt). A file that appears new is MINOR. baseManifest/headManifest
// are already-parsed Manifest values; base's retirement status is what
// governs (never head's -- see Manifest's own doc comment for why).
func DiffSurfaceSet(baseManifest, headManifest Manifest) []Finding {
	baseByPath := map[string]ManifestSurface{}
	for _, s := range baseManifest.Surfaces {
		baseByPath[s.Path] = s
	}
	headByPath := map[string]ManifestSurface{}
	for _, s := range headManifest.Surfaces {
		headByPath[s.Path] = s
	}

	paths := map[string]bool{}
	for p := range baseByPath {
		paths[p] = true
	}
	for p := range headByPath {
		paths[p] = true
	}

	var findings []Finding
	for p := range paths {
		b, inBase := baseByPath[p]
		_, inHead := headByPath[p]
		switch {
		case inBase && !inHead:
			if b.Status == StatusRetired {
				continue
			}
			findings = append(findings, Finding{RuleID: "37", Severity: SeverityMajor, Surface: p, Pointer: "#", Message: "schema file removed/renamed without being retired in the base manifest first"})
		case !inBase && inHead:
			findings = append(findings, Finding{RuleID: "38", Severity: SeverityMinor, Surface: p, Pointer: "#", Message: "schema file added under a new manifest row"})
		}
	}
	return findings
}
