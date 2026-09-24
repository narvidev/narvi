package compat

import "testing"

// TestStep157 pins the review round 6 (Step 157) finding: the structural
// invariants that only ever ran inside a genuine two-sided diff --
// unionShapeOf (via diffUnion), fc-required-orphan (diffProperties), and
// the boolean-$ref-target rejection (resolver.resolveDef) -- never ran on
// a subtree that was ADDED rather than CHANGED. Three shapes, matching
// the PR body's own examples: a brand-new file, a new $defs entry in an
// already-governed file, and a new property on an already-governed def.
// Before validateStructure (structuralcheck.go) existed, all three were
// admitted silently -- the first PR to discover the bad shape was
// whichever LATER PR happened to touch it from a genuinely two-sided
// diff, which then failed closed on content it did not introduce. Each
// case here must now fail closed in the SAME PR that introduces the bad
// shape.

// TestStep157_NewFileIllegalUnionShapeFailsClosed: a brand-new file whose
// only content is an anyOf of two plain, undiscriminated inline scalars
// -- neither a $ref to a pure object def nor the bare {"type":"null"}
// literal, so it matches neither of the two whitelisted union shapes
// (defdiff.go's unionShapeOf). Before this fix, compare.go's own
// `!inBase && inHead` branch called nothing but walkSchema, which
// enforces the keyword allowlist but never routes the new file's content
// through diffUnion at all.
func TestStep157_NewFileIllegalUnionShapeFailsClosed(t *testing.T) {
	existingFile := "rest/v1/dtos.schema.json"
	existingDoc := minimalFile("https://narvi.dev/rest/v1/dtos.schema.json", defsOf("Widget", schemaObj("type", "string")))
	schemaFile := "rest/v2/dtos.schema.json"
	headDoc := schemaObj(
		"$schema", "https://json-schema.org/draft/2020-12/schema",
		"$id", "https://narvi.dev/rest/v2/dtos.schema.json",
		"title", "T",
		"$defs", defsOf("Value", schemaObj(
			"type", "object",
			"properties", schemaObj("v", schemaObj(
				"anyOf", []any{schemaObj("type", "string"), schemaObj("type", "integer")},
			)),
		)),
	)
	report, err := Compare(Input{
		BaseManifestRaw:  manifestJSON(surfaceRow(existingFile, DirectiveBySuffix, StatusCurrent)),
		HeadManifestRaw:  manifestJSON(surfaceRow(existingFile, DirectiveBySuffix, StatusCurrent), surfaceRow(schemaFile, string(DirC2P), StatusCurrent)),
		BaseVersion:      "1.0.0",
		HeadVersion:      "2.0.0",
		BaseChangelogRaw: []byte("## [1.0.0]\n"),
		HeadChangelogRaw: changelogFor("2.0.0", schemaFile),
		BaseRoutes:       []byte(""),
		HeadRoutes:       []byte(""),
		BaseSchemaFiles:  map[string][]byte{existingFile: mustMarshal(existingDoc)},
		HeadSchemaFiles:  map[string][]byte{existingFile: mustMarshal(existingDoc), schemaFile: mustMarshal(headDoc)},
	})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !containsFinding(report.Findings, "fc-oneof-unpairable", SeverityFailClosed) {
		t.Fatalf("an illegal anyOf shape in a brand-new file must fail closed in the PR that introduces it, got: %+v", report.Findings)
	}
	if !report.HasBreaking() {
		t.Fatalf("report must be breaking, got: %+v", report.Findings)
	}
}

// TestStep157_NewDefsEntryRequiredOrphanFailsClosed: an existing,
// already-governed file gains a brand-new $defs entry whose `required`
// names a property ("x") that has no matching `properties` entry at all.
// Before this fix, DiffSurface's own row-32 "$defs entry added" branch
// (rootdiff.go) recorded the addition and moved on -- it never called
// DiffDef on the new entry's own content, so fc-required-orphan never ran
// on it.
func TestStep157_NewDefsEntryRequiredOrphanFailsClosed(t *testing.T) {
	schemaFile := "rest/v1/dtos.schema.json"
	id := "https://narvi.dev/" + schemaFile
	baseDoc := minimalFile(id, defsOf("Widget", schemaObj("type", "string")))
	headDoc := minimalFile(id, map[string]any{
		"Widget": schemaObj("type", "string"),
		"Bad":    schemaObj("type", "object", "required", []any{"x"}), // no "properties" at all
	})

	report, err := Compare(Input{
		BaseManifestRaw:  manifestJSON(surfaceRow(schemaFile, DirectiveBySuffix, StatusCurrent)),
		HeadManifestRaw:  manifestJSON(surfaceRow(schemaFile, DirectiveBySuffix, StatusCurrent)),
		BaseVersion:      "1.0.0",
		HeadVersion:      "1.1.0",
		BaseChangelogRaw: []byte("## [1.0.0]\n"),
		HeadChangelogRaw: changelogFor("1.1.0", schemaFile),
		BaseRoutes:       []byte(""),
		HeadRoutes:       []byte(""),
		BaseSchemaFiles:  map[string][]byte{schemaFile: mustMarshal(baseDoc)},
		HeadSchemaFiles:  map[string][]byte{schemaFile: mustMarshal(headDoc)},
	})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !containsFinding(report.Findings, "fc-required-orphan", SeverityFailClosed) {
		t.Fatalf("a new $defs entry with a required-but-undeclared property must fail closed in the PR that introduces it, got: %+v", report.Findings)
	}
	if !report.HasBreaking() {
		t.Fatalf("report must be breaking, got: %+v", report.Findings)
	}
}

// TestStep157_NewPropertyBooleanRefTargetFailsClosed: an existing,
// already-governed def gains a brand-new property that $refs a brand-new
// $defs entry which is itself the boolean schema literal `true` -- legal
// on its own (an unreferenced bare-boolean def), but not "supported
// behind a $ref" (ref.go's resolver.resolveDef). Before this fix,
// diffProperties' own rows-2/3 "property added" branch (defdiff.go)
// recorded the addition and moved on -- it never recursed diffNode into
// the new property, so resolveDef's boolean-target rejection never ran on
// it.
func TestStep157_NewPropertyBooleanRefTargetFailsClosed(t *testing.T) {
	schemaFile := "rest/v1/dtos.schema.json"
	id := "https://narvi.dev/" + schemaFile
	baseDoc := minimalFile(id, defsOf("Widget", schemaObj(
		"type", "object",
		"properties", schemaObj("a", schemaObj("type", "string")),
	)))
	headDoc := minimalFile(id, map[string]any{
		"Widget": schemaObj(
			"type", "object",
			"properties", schemaObj(
				"a", schemaObj("type", "string"),
				"x", schemaObj("$ref", "#/$defs/Any"), // new property, refs a boolean def
			),
		),
		"Any": true, // new $defs entry: the boolean schema literal
	})

	report, err := Compare(Input{
		BaseManifestRaw:  manifestJSON(surfaceRow(schemaFile, DirectiveBySuffix, StatusCurrent)),
		HeadManifestRaw:  manifestJSON(surfaceRow(schemaFile, DirectiveBySuffix, StatusCurrent)),
		BaseVersion:      "1.0.0",
		HeadVersion:      "1.1.0",
		BaseChangelogRaw: []byte("## [1.0.0]\n"),
		HeadChangelogRaw: changelogFor("1.1.0", schemaFile),
		BaseRoutes:       []byte(""),
		HeadRoutes:       []byte(""),
		BaseSchemaFiles:  map[string][]byte{schemaFile: mustMarshal(baseDoc)},
		HeadSchemaFiles:  map[string][]byte{schemaFile: mustMarshal(headDoc)},
	})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !containsFinding(report.Findings, "fc-ref", SeverityFailClosed) {
		t.Fatalf("a new property $ref'ing a boolean $defs entry must fail closed in the PR that introduces it, got: %+v", report.Findings)
	}
	if !report.HasBreaking() {
		t.Fatalf("report must be breaking, got: %+v", report.Findings)
	}
}

// TestStep157_RealContractsSelfValidateClean is the positive control:
// validateStructure itself, run directly against every real schema file
// at HEAD, must report zero findings -- the actual contracts are
// well-formed, so this fix must not turn a clean tree into a false
// positive. (TestSmokeRealContractsAgainstItself already exercises this
// indirectly through Compare(); this pins validateStructure's own
// contribution in isolation, one file at a time, so a future regression
// in this specific function is easy to bisect to.)
func TestStep157_RealContractsSelfValidateClean(t *testing.T) {
	in := loadRealInput(t)
	m, err := ParseManifest(in.HeadManifestRaw)
	if err != nil {
		t.Fatalf("parse real manifest.json: %v", err)
	}
	for path, raw := range in.HeadSchemaFiles {
		s, ok := m.SurfaceByPath(path)
		if !ok {
			t.Fatalf("real manifest.json has no row for %s", path)
		}
		findings, err := validateStructure(s.Direction, raw, m.OpenEnumSetForSurface(path))
		if err != nil {
			t.Fatalf("validateStructure(%s): %v", path, err)
		}
		if len(findings) != 0 {
			t.Fatalf("validateStructure(%s) against the real, already-clean contracts tree found %d findings, want 0: %+v", path, len(findings), findings)
		}
	}
}
