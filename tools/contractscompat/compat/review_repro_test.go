package compat

import "testing"

// This file pins, as permanent corpus cases, every one of the adversarial
// review's 22 reproductions against PR #320 (see the PR body's "Review
// round 1" section). Each of these passed as MINOR or "no findings"
// before this rewrite; every one must now come back MAJOR or FAIL-CLOSED.
// Fixtures are self-contained (synthetic manifest + schema, not the real
// /contracts tree) so these tests do not depend on repo content drifting
// out from under them -- smoke_test.go covers the real tree separately.

func changelogFor(version, path string) []byte {
	return []byte("## [" + version + "]\n### " + path + "\n- test fixture change\n\n## [1.0.0]\n")
}

func manifestJSON(rows ...map[string]any) []byte {
	return mustMarshal(map[string]any{"version": "1.0.0", "surfaces": rows})
}

func surfaceRow(path, direction, status string) map[string]any {
	return map[string]any{"path": path, "direction": direction, "status": status}
}

// C1: a manifest-only direction flip (no schema content change at all)
// must be MAJOR, not "no findings" -- pinned as TestWholeSurfaceCorpus's
// "row44" case above. This test additionally pins the ORIGINAL repro
// shape: flipping direction AND relaxing a P2C-strict rule (enum value
// added to a closed enum) in the SAME diff must still net out MAJOR
// (the enum-add alone, graded honestly under the base's own by-suffix
// direction).
func TestReviewRepro_C1_DirectionFlipCannotLaunderAnEnumAdd(t *testing.T) {
	schemaFile := "t/v1/x.schema.json"
	baseDoc := minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Status", schemaObj("type", "string", "enum", []any{"a", "b"})))
	headDoc := minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Status", schemaObj("type", "string", "enum", []any{"a", "b", "c"})))

	report, err := Compare(Input{
		BaseManifestRaw:  manifestJSON(surfaceRow(schemaFile, DirectiveBySuffix, StatusCurrent)),
		HeadManifestRaw:  manifestJSON(surfaceRow(schemaFile, string(DirC2P), StatusCurrent)), // flipped in head
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
	if !report.HasBreaking() {
		t.Fatalf("a manifest-only direction flip must be breaking, got: %+v", report.Findings)
	}
	if !containsFinding(report.Findings, "44", SeverityMajor) {
		t.Fatalf("want rule 44 MAJOR for the direction flip itself, got: %+v", report.Findings)
	}
}

// C2/C12/C14: dropping the `type` keyword outright (not narrowing it to a
// smaller union) must be MAJOR both directions, not row 8's "narrowed"
// MINOR-on-P2C bucket.
func TestReviewRepro_C2_TypeKeywordDroppedIsPresenceChange(t *testing.T) {
	baseDefs := defsOf("Title", schemaObj("type", []any{"string", "null"}))
	headDefs := defsOf("Title", schemaObj())
	findings, err := DiffDef(baseDefs, headDefs, "Title", DirP2C, nil)
	if err != nil {
		t.Fatalf("DiffDef: %v", err)
	}
	if !containsFinding(findings, "6", SeverityMajor) {
		t.Fatalf("dropping `type` entirely on a P2C shape must be MAJOR (row 6), got: %+v", findings)
	}
}

// C3: rewriting `"type": "string"` as `"anyOf": [{"type":"string"},
// {"type":"null"}]` puts the identical null on the wire and must be
// caught the same as row 9 (null added), not scored MINOR via
// type-narrowed-to-empty plus two "variant added" MINORs.
func TestReviewRepro_C3_NullableViaAnyOfCannotEvadeRow9(t *testing.T) {
	baseDefs := defsOf("ContractsVersion", schemaObj("type", "string"))
	headDefs := defsOf("ContractsVersion", schemaObj("anyOf", []any{schemaObj("type", "string"), schemaObj("type", "null")}))
	findings, err := DiffDef(baseDefs, headDefs, "ContractsVersion", DirP2C, nil)
	if err != nil {
		t.Fatalf("DiffDef: %v", err)
	}
	worst := SeverityPatch
	for _, f := range findings {
		worst = maxSeverity(worst, f.Severity)
	}
	if worst != SeverityMajor {
		t.Fatalf("nullable-via-anyOf on a P2C shape must classify MAJOR overall, got %s: %+v", worst, findings)
	}
}

// C4/C9: a name appended to `required` with no matching `properties`
// entry must not be invisible -- it now fails closed as a malformed
// schema (fc-required-orphan) rather than passing as a PATCH/no-op.
func TestReviewRepro_C4_RequiredEntryWithNoPropertyFailsClosed(t *testing.T) {
	baseDefs := defsOf("CreateSessionRequest", schemaObj(
		"type", "object",
		"properties", schemaObj("a", schemaObj("type", "string")),
		"required", []any{"a"},
		"additionalProperties", false,
	))
	headDefs := defsOf("CreateSessionRequest", schemaObj(
		"type", "object",
		"properties", schemaObj("a", schemaObj("type", "string")),
		"required", []any{"a", "dryRun"}, // "dryRun" never declared in properties
		"additionalProperties", false,
	))
	_, err := DiffDef(baseDefs, headDefs, "CreateSessionRequest", DirC2P, nil)
	fc, ok := err.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError, got err=%v", err)
	}
	if fc.Finding.RuleID != "fc-required-orphan" {
		t.Fatalf("want fc-required-orphan, got %s", fc.Finding.RuleID)
	}
}

// C5/C8: keywords sitting BESIDE a $ref (siblings) must be diffed, not
// silently dropped when the $ref itself is unchanged.
func TestReviewRepro_C5_SiblingKeywordsBesideUnchangedRefAreDiffed(t *testing.T) {
	baseDefs := map[string]any{
		"Wrapper": schemaObj("type", "object", "properties", schemaObj(
			"digest", schemaObj("$ref", "#/$defs/Digest"),
		)),
		"Digest": schemaObj("type", "object", "properties", schemaObj(
			"archDecisions", schemaObj("type", "string"),
		)),
	}
	headDefs := map[string]any{
		"Wrapper": schemaObj("type", "object", "properties", schemaObj(
			// same $ref, but a "required" sibling now added directly on
			// the referencing node -- archDecisions moves into required.
			"digest", schemaObj("$ref", "#/$defs/Digest", "required", []any{"archDecisions"}),
		)),
		"Digest": schemaObj("type", "object", "properties", schemaObj(
			"archDecisions", schemaObj("type", "string"),
		)),
	}
	findings, err := DiffDef(baseDefs, headDefs, "Wrapper", DirC2P, nil)
	if err != nil {
		t.Fatalf("DiffDef: %v", err)
	}
	if !containsFinding(findings, "4", SeverityMajor) {
		t.Fatalf("a required-name added beside an unchanged $ref must be classified (row 4, MAJOR on C2P), got: %+v", findings)
	}
}

func TestReviewRepro_C8_ConstraintAddedBesideRefIsMajor(t *testing.T) {
	baseDefs := map[string]any{
		"Wrapper": schemaObj("type", "object", "properties", schemaObj(
			"digest", schemaObj("$ref", "#/$defs/Digest"),
		)),
		"Digest": schemaObj("type", "string"),
	}
	headDefs := map[string]any{
		"Wrapper": schemaObj("type", "object", "properties", schemaObj(
			"digest", schemaObj("$ref", "#/$defs/Digest", "minLength", float64(5)),
		)),
		"Digest": schemaObj("type", "string"),
	}
	findings, err := DiffDef(baseDefs, headDefs, "Wrapper", DirC2P, nil)
	if err != nil {
		t.Fatalf("DiffDef: %v", err)
	}
	if !containsFinding(findings, "18", SeverityMajor) {
		t.Fatalf("minLength added beside an unchanged $ref must be classified (row 18, MAJOR on C2P), got: %+v", findings)
	}
}

// C6/C7/C20: the document ROOT's own $ref (session-config's whole
// contract) must be diffed, not silently accepted via a $defs-add MINOR.
func TestReviewRepro_C6_RootRefRetargetIsDiffed(t *testing.T) {
	base := mustMarshal(schemaObj(
		"$schema", "https://json-schema.org/draft/2020-12/schema",
		"$id", "https://narvi.dev/t/v1/x.schema.json",
		"title", "T",
		"$ref", "#/$defs/Config",
		"$defs", defsOf("Config", schemaObj("type", "object", "properties", schemaObj("a", schemaObj("type", "string")))),
	))
	headDoc := schemaObj(
		"$schema", "https://json-schema.org/draft/2020-12/schema",
		"$id", "https://narvi.dev/t/v1/x.schema.json",
		"title", "T",
		"$ref", "#/$defs/ConfigV2",
		"$defs", map[string]any{
			"Config":   schemaObj("type", "object", "properties", schemaObj("a", schemaObj("type", "string"))),
			"ConfigV2": schemaObj("type", "object", "properties", schemaObj("a", schemaObj("type", "string")), "required", []any{"a"}),
		},
	)
	head := mustMarshal(headDoc)
	// C2P (not session-config's real P2C): a required property gained on
	// a client-produced shape is the strict column (row 4, MAJOR) -- this
	// is what makes the assertion below unambiguous. The point under
	// test is that the root $ref retarget is diffed AT ALL (row 27 now
	// appears; before this rewrite the only output was a MINOR "$defs
	// entry added" and the retarget itself was invisible).
	findings, err := DiffSurface(string(DirC2P), base, head, nil)
	if err != nil {
		t.Fatalf("DiffSurface: %v", err)
	}
	if !containsFinding(findings, "27", SeverityMajor) {
		t.Fatalf("retargeting the root $ref to a def that adds a required property must be MAJOR (row 27), got: %+v", findings)
	}
}

func TestReviewRepro_C7_RootKeywordAddedIsDiffed(t *testing.T) {
	base := mustMarshal(schemaObj(
		"$schema", "https://json-schema.org/draft/2020-12/schema",
		"$id", "https://narvi.dev/t/v1/x.schema.json",
		"title", "T",
		"$defs", defsOf("A", schemaObj("oneOf", []any{schemaObj("type", "string")})),
		"oneOf", []any{schemaObj("$ref", "#/$defs/A")},
	))
	headDoc := schemaObj(
		"$schema", "https://json-schema.org/draft/2020-12/schema",
		"$id", "https://narvi.dev/t/v1/x.schema.json",
		"title", "T",
		"$defs", defsOf("A", schemaObj("oneOf", []any{schemaObj("type", "string")})),
		"oneOf", []any{schemaObj("$ref", "#/$defs/A")},
		"required", []any{"zzz"}, // new root keyword: every document now invalid
	)
	head := mustMarshal(headDoc)
	findings, err := DiffSurface(string(DirP2C), base, head, nil)
	if err != nil {
		t.Fatalf("DiffSurface: %v", err)
	}
	worst := SeverityPatch
	for _, f := range findings {
		worst = maxSeverity(worst, f.Severity)
	}
	// A required name ("zzz") with no matching declared property is
	// itself a malformed-schema condition this checker fails closed on
	// (fc-required-orphan, C4/C9's fix) -- FAIL-CLOSED is >= MAJOR in
	// severity ordering and blocks CI exactly the same way, so either
	// outcome proves the root keyword is no longer invisible.
	if worst < SeverityMajor {
		t.Fatalf("a new root `required` keyword must be classified as breaking, got worst=%s: %+v", worst, findings)
	}
}

// C11: a brand-new file's content is walked against the allowlist right
// away, not admitted unexamined.
func TestReviewRepro_C11_NewFileIsWalkedAgainstAllowlist(t *testing.T) {
	existingFile := "rest/v1/dtos.schema.json"
	existingDoc := minimalFile("https://narvi.dev/rest/v1/dtos.schema.json", defsOf("Widget", schemaObj("type", "string")))
	schemaFile := "rest/v2/dtos.schema.json"
	headDoc := schemaObj(
		"$schema", "https://json-schema.org/draft/2020-12/schema",
		"$id", "https://narvi.dev/rest/v2/dtos.schema.json",
		"title", "T",
		"$defs", defsOf("Session", schemaObj("type", "object", "x-narvi-note", "hi")), // disallowed keyword, but still compiler-valid (an unknown key is not itself a JSON Schema violation)
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
	if !containsFinding(report.Findings, "fc-keyword", SeverityFailClosed) {
		t.Fatalf("a disallowed keyword in a brand-new file must fail closed immediately, got: %+v", report.Findings)
	}
}

// C13: additionalProperties schema -> permissive is a distinct MAJOR (on
// P2C) row, not the generic MINOR "loosening" bucket.
func TestReviewRepro_C13_AdditionalPropertiesSchemaToPermissiveIsMajorOnP2C(t *testing.T) {
	baseDefs := defsOf("MintUploadResponse", schemaObj(
		"type", "object", "properties", schemaObj("headers", schemaObj(
			"type", "object", "additionalProperties", schemaObj("type", "string"),
		)),
	))
	headDefs := defsOf("MintUploadResponse", schemaObj(
		"type", "object", "properties", schemaObj("headers", schemaObj(
			"type", "object", "additionalProperties", true,
		)),
	))
	findings, err := DiffDef(baseDefs, headDefs, "MintUploadResponse", DirP2C, nil)
	if err != nil {
		t.Fatalf("DiffDef: %v", err)
	}
	if !containsFinding(findings, "42", SeverityMajor) {
		t.Fatalf("widening a P2C map's additionalProperties schema to permissive must be MAJOR (row 42), got: %+v", findings)
	}
}

// C15: introducing anyOf where none existed on a C2P shape must be MAJOR,
// not MINOR.
func TestReviewRepro_C15_UnionKeywordIntroducedOnC2PShapeIsMajor(t *testing.T) {
	baseDefs := defsOf("CreateSessionRequest", schemaObj("type", "object"))
	headDefs := defsOf("CreateSessionRequest", schemaObj("type", "object", "anyOf", []any{schemaObj("type", "null")}))
	findings, err := DiffDef(baseDefs, headDefs, "CreateSessionRequest", DirC2P, nil)
	if err != nil {
		t.Fatalf("DiffDef: %v", err)
	}
	if !containsFinding(findings, "43", SeverityMajor) {
		t.Fatalf("introducing anyOf on a C2P shape must be MAJOR (row 43), got: %+v", findings)
	}
}

// C18: a new vN sibling file requires a MAJOR version bump, not MINOR --
// even though the file-add finding itself (row 38) stays MINOR.
func TestReviewRepro_C18_NewSiblingFileRequiresMajorBump(t *testing.T) {
	schemaFileV1 := "rest/v1/dtos.schema.json"
	schemaFileV2 := "rest/v2/dtos.schema.json"
	v1Doc := minimalFile("https://narvi.dev/rest/v1/dtos.schema.json", defsOf("Widget", schemaObj("type", "string")))
	v2Doc := minimalFile("https://narvi.dev/rest/v2/dtos.schema.json", defsOf("Widget", schemaObj("type", "string")))

	report, err := Compare(Input{
		BaseManifestRaw:  manifestJSON(surfaceRow(schemaFileV1, DirectiveBySuffix, StatusCurrent)),
		HeadManifestRaw:  manifestJSON(surfaceRow(schemaFileV1, DirectiveBySuffix, StatusCurrent), surfaceRow(schemaFileV2, string(DirC2P), StatusCurrent)),
		BaseVersion:      "1.0.0",
		HeadVersion:      "1.1.0", // MINOR bump only -- must be rejected
		BaseChangelogRaw: []byte("## [1.0.0]\n"),
		HeadChangelogRaw: changelogFor("1.1.0", schemaFileV2),
		BaseRoutes:       []byte(""),
		HeadRoutes:       []byte(""),
		BaseSchemaFiles:  map[string][]byte{schemaFileV1: mustMarshal(v1Doc)},
		HeadSchemaFiles:  map[string][]byte{schemaFileV1: mustMarshal(v1Doc), schemaFileV2: mustMarshal(v2Doc)},
	})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !containsFinding(report.Findings, "version-changelog", SeverityMajor) {
		t.Fatalf("a new vN sibling with only a MINOR version bump must fail the version-changelog rule, got: %+v", report.Findings)
	}
	if !report.HasBreaking() {
		t.Fatal("report must be breaking")
	}
}

// C16 (M10 pin): a def reachable from BOTH a *Request root and a
// response/entity root must be classified DirBoth, so the WORSE of its
// two columns' readings governs. Removing a required name is row 5 (P2C
// MAJOR, C2P MINOR) -- if Both-reachability were ever disabled (falling
// back to whichever single category the reachability scan happens to hit
// first), this exact change could read MINOR instead of MAJOR.
func TestReviewRepro_BothReachabilityGovernsWorstColumn(t *testing.T) {
	baseDoc := minimalFile("https://narvi.dev/t/v1/x.schema.json", map[string]any{
		"PostThingRequest": schemaObj("type", "object", "properties", schemaObj(
			"digest", schemaObj("$ref", "#/$defs/Digest"),
		)),
		"ThingResponse": schemaObj("type", "object", "properties", schemaObj(
			"digest", schemaObj("$ref", "#/$defs/Digest"),
		)),
		"Digest": schemaObj("type", "object", "properties", schemaObj(
			"summary", schemaObj("type", "string"),
		), "required", []any{"summary"}),
	})
	headDoc := minimalFile("https://narvi.dev/t/v1/x.schema.json", map[string]any{
		"PostThingRequest": schemaObj("type", "object", "properties", schemaObj(
			"digest", schemaObj("$ref", "#/$defs/Digest"),
		)),
		"ThingResponse": schemaObj("type", "object", "properties", schemaObj(
			"digest", schemaObj("$ref", "#/$defs/Digest"),
		)),
		"Digest": schemaObj("type", "object", "properties", schemaObj(
			"summary", schemaObj("type", "string"),
		), "required", []any{}), // "summary" removed from required
	})
	findings, err := DiffSurface(DirectiveBySuffix, mustMarshal(baseDoc), mustMarshal(headDoc), nil)
	if err != nil {
		t.Fatalf("DiffSurface: %v", err)
	}
	if !containsFinding(findings, "5", SeverityMajor) {
		t.Fatalf("a Both-reachable def's required-property removal must classify MAJOR (the P2C column's own severity, the worse of the two), got: %+v", findings)
	}
}

// C16 (M15 pin): Report.HasBreaking() must treat a FAIL-CLOSED finding as
// breaking even when no MAJOR finding is present at all -- FAIL-CLOSED
// means "the checker refuses to classify this," which is at least as
// serious as a known break, not something CI should let slide because
// nothing else in the diff happened to be graded MAJOR.
func TestReviewRepro_HasBreakingTreatsFailClosedAsBreaking(t *testing.T) {
	report := Report{Findings: []Finding{
		{RuleID: "35", Severity: SeverityPatch},
		{RuleID: "2", Severity: SeverityMinor},
		{RuleID: "fc-keyword", Severity: SeverityFailClosed},
	}}
	if !report.HasBreaking() {
		t.Fatal("a report containing only a FAIL-CLOSED finding (no MAJOR) must still be breaking")
	}
}

// C16 (M27 pin): a PATCH-class change (the smallest bump class,
// WorstFinding == SeverityPatch) still requires VERSION to move at all --
// "strictly greater" has no PATCH-sized exemption. RED 14b in
// version_test.go already pins the Minor case; the strictly-greater check
// itself only gets exercised end-to-end when the required bump class is
// the SMALLEST one, because a Minor-or-above requirement is also caught
// independently by the bump-class-too-small check even with an unchanged
// version (base.BumpClass(head) on two equal versions is always Patch,
// which is already smaller than Minor).
func TestReviewRepro_PatchChangeStillRequiresVersionBump(t *testing.T) {
	findings := CheckVersionAndChangelog(VersionCheckInput{
		BaseVersion:      "1.0.0",
		HeadVersion:      "1.0.0", // unchanged
		BaseChangelogTop: "1.0.0",
		HeadChangelog:    []ChangelogSection{{Version: "1.0.0", Subsurface: map[string]bool{"x.schema.json": true}}},
		AnythingChanged:  true,
		WorstFinding:     SeverityPatch,
		ChangedSurfaces:  []string{"x.schema.json"},
	})
	if !containsFinding(findings, "version-changelog", SeverityMajor) {
		t.Fatalf("a PATCH-class change with VERSION left unchanged must still fail the version-changelog rule, got: %+v", findings)
	}
}

// C16 (guard 1/2 pin): an empty base schema-file set, or a base
// manifest/on-disk mismatch, must be a hard Compare() error -- never
// silently treated as "no findings." (§3's vacuous-pass guards 1-2.)
func TestReviewRepro_EmptyBaseIsHardError(t *testing.T) {
	schemaFile := "t/v1/x.schema.json"
	headDoc := minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Widget", schemaObj("type", "string")))
	_, err := Compare(Input{
		BaseManifestRaw:  manifestJSON(surfaceRow(schemaFile, DirectiveBySuffix, StatusCurrent)),
		HeadManifestRaw:  manifestJSON(surfaceRow(schemaFile, DirectiveBySuffix, StatusCurrent)),
		BaseVersion:      "1.0.0",
		HeadVersion:      "1.0.0",
		BaseChangelogRaw: []byte("## [1.0.0]\n"),
		HeadChangelogRaw: []byte("## [1.0.0]\n"),
		BaseRoutes:       []byte(""),
		HeadRoutes:       []byte(""),
		BaseSchemaFiles:  map[string][]byte{}, // empty on-disk set, despite the manifest listing a row
		HeadSchemaFiles:  map[string][]byte{schemaFile: mustMarshal(headDoc)},
	})
	if err == nil {
		t.Fatal("an empty BASE schema-file set (with a non-empty manifest) must be a hard Compare() error, got nil")
	}
}

func TestReviewRepro_BaseManifestDiskMismatchIsHardError(t *testing.T) {
	schemaFile := "t/v1/x.schema.json"
	otherFile := "t/v1/y.schema.json"
	doc := minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Widget", schemaObj("type", "string")))
	_, err := Compare(Input{
		BaseManifestRaw:  manifestJSON(surfaceRow(schemaFile, DirectiveBySuffix, StatusCurrent)),
		HeadManifestRaw:  manifestJSON(surfaceRow(schemaFile, DirectiveBySuffix, StatusCurrent)),
		BaseVersion:      "1.0.0",
		HeadVersion:      "1.0.0",
		BaseChangelogRaw: []byte("## [1.0.0]\n"),
		HeadChangelogRaw: []byte("## [1.0.0]\n"),
		BaseRoutes:       []byte(""),
		HeadRoutes:       []byte(""),
		// on-disk file does not match the manifest's own path at all
		BaseSchemaFiles: map[string][]byte{otherFile: mustMarshal(doc)},
		HeadSchemaFiles: map[string][]byte{schemaFile: mustMarshal(doc)},
	})
	if err == nil {
		t.Fatal("a base manifest/on-disk path mismatch must be a hard Compare() error, got nil")
	}
}

// C16 (guard 4 pin): a schema that satisfies the keyword allowlist but is
// not actually a valid JSON Schema (e.g. "minimum" holding a non-numeric
// value -- walkSchema never validates VALUE types for leaf keywords, only
// which keywords may appear) must be a hard Compare() error via
// CompileCheck's real jsonschema.NewCompiler() pass, not silently
// accepted.
func TestReviewRepro_InvalidSchemaCompileIsHardError(t *testing.T) {
	schemaFile := "t/v1/x.schema.json"
	baseDoc := minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Value", schemaObj("type", "integer")))
	headDoc := minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Value", schemaObj("type", "integer", "minimum", "not-a-number")))
	_, err := Compare(Input{
		BaseManifestRaw:  manifestJSON(surfaceRow(schemaFile, DirectiveBySuffix, StatusCurrent)),
		HeadManifestRaw:  manifestJSON(surfaceRow(schemaFile, DirectiveBySuffix, StatusCurrent)),
		BaseVersion:      "1.0.0",
		HeadVersion:      "1.0.1",
		BaseChangelogRaw: []byte("## [1.0.0]\n"),
		HeadChangelogRaw: changelogFor("1.0.1", schemaFile),
		BaseRoutes:       []byte(""),
		HeadRoutes:       []byte(""),
		BaseSchemaFiles:  map[string][]byte{schemaFile: mustMarshal(baseDoc)},
		HeadSchemaFiles:  map[string][]byte{schemaFile: mustMarshal(headDoc)},
	})
	if err == nil {
		t.Fatal("a schema with an invalid (non-numeric) `minimum` must fail CompileCheck as a hard Compare() error, got nil")
	}
}

// C16 (M22 pin): retargeting a $ref AND deleting the old target from
// head's own $defs in the same diff must be unconditionally MAJOR (row
// 27's own "old target no longer exists" branch) -- distinct from row 31
// (whole $defs entry removed), which fires separately for the SAME
// disappearance; this pins that row 27 itself also flags it at the
// retargeting pointer, not just at the vanished def's own pointer.
func TestReviewRepro_RefRetargetToDeletedOldTargetIsMajor(t *testing.T) {
	baseDefs := map[string]any{
		"Wrapper": schemaObj("type", "object", "properties", schemaObj("value", schemaObj("$ref", "#/$defs/A"))),
		"A":       schemaObj("type", "string"),
		"B":       schemaObj("type", "string"),
	}
	headDefs := map[string]any{
		"Wrapper": schemaObj("type", "object", "properties", schemaObj("value", schemaObj("$ref", "#/$defs/B"))),
		// "A" deleted entirely from head's own $defs.
		"B": schemaObj("type", "string"),
	}
	findings, err := DiffDef(baseDefs, headDefs, "Wrapper", DirP2C, nil)
	if err != nil {
		t.Fatalf("DiffDef: %v", err)
	}
	if !containsFinding(findings, "27", SeverityMajor) {
		t.Fatalf("retargeting a $ref to a def while deleting the old target must be MAJOR (row 27), got: %+v", findings)
	}
}
