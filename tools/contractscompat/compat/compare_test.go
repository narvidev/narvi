package compat

import "testing"

func minimalManifestJSON(t *testing.T, openEnums []string) []byte {
	t.Helper()
	doc := map[string]any{
		"version": "1.0.0",
		"surfaces": []any{
			map[string]any{"path": "t/v1/x.schema.json", "direction": DirectiveBySuffix, "status": StatusCurrent},
		},
	}
	if openEnums != nil {
		doc["openEnums"] = openEnums
	}
	return mustMarshalFile(t, doc)
}

// TestOpenEnumsReadFromMergeBaseOnly is the exit criterion's RED #16: "add
// a pointer to openEnums AND the enum value in the same PR" must still be
// MAJOR, because Compare derives openEnums from the BASE manifest only
// (compare.go: `baseManifest.OpenEnumSetForSurface(path)`) -- head opening
// the enum in the very same diff that adds a value to it must not help.
func TestOpenEnumsReadFromMergeBaseOnly(t *testing.T) {
	schemaFile := "t/v1/x.schema.json"
	baseSchema := mustMarshalFile(t, minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Status", schemaObj("type", "string", "enum", []any{"a", "b"}))))
	headSchema := mustMarshalFile(t, minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Status", schemaObj("type", "string", "enum", []any{"a", "b", "c"}))))

	in := Input{
		BaseManifestRaw:  minimalManifestJSON(t, nil),                                          // base does NOT open Status
		HeadManifestRaw:  minimalManifestJSON(t, []string{"t/v1/x.schema.json#/$defs/Status"}), // head opens it in the SAME PR
		BaseVersion:      "1.0.0",
		HeadVersion:      "1.1.0",
		BaseChangelogRaw: []byte("## [1.0.0]\n"),
		HeadChangelogRaw: []byte("## [1.1.0]\n### " + schemaFile + "\n- Changed: Status enum, opened openEnums\n\n## [1.0.0]\n"),
		BaseRoutes:       []byte(""),
		HeadRoutes:       []byte(""),
		BaseSchemaFiles:  map[string][]byte{schemaFile: baseSchema},
		HeadSchemaFiles:  map[string][]byte{schemaFile: headSchema},
	}

	report, err := Compare(in)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !containsFinding(report.Findings, "11", SeverityMajor) {
		t.Fatalf("adding an enum value while opening it in the SAME PR must still be MAJOR (merge-base-only relaxation), got: %+v", report.Findings)
	}
	if !report.HasBreaking() {
		t.Fatal("report must be breaking")
	}
}

// TestOpenEnumsAlreadyEstablishedInBase is the corresponding GREEN case:
// once a PRIOR PR has already landed the openEnums entry (so it is
// present in the MERGE-BASE manifest), a later PR adding a value to that
// enum is MINOR.
func TestOpenEnumsAlreadyEstablishedInBase(t *testing.T) {
	schemaFile := "t/v1/x.schema.json"
	baseSchema := mustMarshalFile(t, minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Status", schemaObj("type", "string", "enum", []any{"a", "b"}))))
	headSchema := mustMarshalFile(t, minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Status", schemaObj("type", "string", "enum", []any{"a", "b", "c"}))))

	in := Input{
		BaseManifestRaw:  minimalManifestJSON(t, []string{"t/v1/x.schema.json#/$defs/Status"}), // already open in base
		HeadManifestRaw:  minimalManifestJSON(t, []string{"t/v1/x.schema.json#/$defs/Status"}),
		BaseVersion:      "1.0.0",
		HeadVersion:      "1.1.0",
		BaseChangelogRaw: []byte("## [1.0.0]\n"),
		HeadChangelogRaw: []byte("## [1.1.0]\n### " + schemaFile + "\n- Changed: Status enum\n\n## [1.0.0]\n"),
		BaseRoutes:       []byte(""),
		HeadRoutes:       []byte(""),
		BaseSchemaFiles:  map[string][]byte{schemaFile: baseSchema},
		HeadSchemaFiles:  map[string][]byte{schemaFile: headSchema},
	}

	report, err := Compare(in)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if containsFinding(report.Findings, "11", SeverityMajor) {
		t.Fatalf("an enum already open in the merge-base must not be MAJOR here, got: %+v", report.Findings)
	}
	if !containsFinding(report.Findings, "11", SeverityMinor) {
		t.Fatalf("want rule 11 MINOR, got: %+v", report.Findings)
	}
	if report.HasBreaking() {
		t.Fatalf("report must not be breaking, got: %+v", report.Findings)
	}
}

// --- G3 (round 5 review): openEnums scoped per surface. Round 4's own
// openEnums matching already required the EXACT JSON Pointer of the
// enum's own def (D14/E3/F1) -- what it never checked is WHICH FILE that
// pointer lives in. A bare, unqualified entry like "#/$defs/Automation/
// properties/status" matched a def named "Automation" in ANY surface,
// not only the one whoever wrote the entry meant. ---

// twoSurfaceManifest builds a manifest with two surfaces and the given
// openEnums list, for the G3 tests below.
func twoSurfaceManifest(t *testing.T, pathA, pathB string, openEnums []string) []byte {
	t.Helper()
	doc := map[string]any{
		"version": "1.0.0",
		"surfaces": []any{
			map[string]any{"path": pathA, "direction": DirectiveBySuffix, "status": StatusCurrent},
			map[string]any{"path": pathB, "direction": DirectiveBySuffix, "status": StatusCurrent},
		},
	}
	if openEnums != nil {
		doc["openEnums"] = openEnums
	}
	return mustMarshalFile(t, doc)
}

// TestRound5_G3_OpenEnumScopedToItsOwnSurface is the two-PR repro the
// round-5 review reproduced against the real binary: a relaxation
// qualified for restFile's own $defs.Automation must not ALSO open a
// same-named, unrelated $defs.Automation living in wsFile -- both
// surfaces are diffed in the SAME PR here (a single Compare call is
// enough to prove the scoping; the real finding used two separate PRs
// only because the second one needed the first one's def to already
// exist).
func TestRound5_G3_OpenEnumScopedToItsOwnSurface(t *testing.T) {
	restFile := "rest/v1/x.schema.json"
	wsFile := "client-ws/v1/y.schema.json"

	manifestRaw := twoSurfaceManifest(t, restFile, wsFile, []string{restFile + "#/$defs/Automation/properties/status"})

	restDoc := mustMarshalFile(t, minimalFile("https://narvi.dev/"+restFile, defsOf("Widget", schemaObj("type", "string"))))
	wsBase := mustMarshalFile(t, minimalFile("https://narvi.dev/"+wsFile, defsOf("Automation", schemaObj(
		"type", "object", "properties", schemaObj("status", schemaObj("type", "string", "enum", []any{"active", "paused"})),
	))))
	wsHead := mustMarshalFile(t, minimalFile("https://narvi.dev/"+wsFile, defsOf("Automation", schemaObj(
		"type", "object", "properties", schemaObj("status", schemaObj("type", "string", "enum", []any{"active", "paused", "archived"})),
	))))

	report, err := Compare(Input{
		BaseManifestRaw:  manifestRaw,
		HeadManifestRaw:  manifestRaw,
		BaseVersion:      "1.0.0",
		HeadVersion:      "2.0.0",
		BaseChangelogRaw: []byte("## [1.0.0]\n"),
		HeadChangelogRaw: []byte("## [2.0.0]\n### " + wsFile + "\n- Changed: Automation.status enum\n\n## [1.0.0]\n"),
		BaseRoutes:       []byte(""),
		HeadRoutes:       []byte(""),
		BaseSchemaFiles:  map[string][]byte{restFile: restDoc, wsFile: wsBase},
		HeadSchemaFiles:  map[string][]byte{restFile: restDoc, wsFile: wsHead},
	})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !containsFinding(report.Findings, "11", SeverityMajor) {
		t.Fatalf("an openEnums entry qualified for %s must not relax the SAME-named def in %s, want rule 11 MAJOR, got: %+v", restFile, wsFile, report.Findings)
	}
	if containsFinding(report.Findings, "11", SeverityMinor) {
		t.Fatalf("must not be graded MINOR -- that would mean the relaxation leaked across surfaces, got: %+v", report.Findings)
	}
	if !report.HasBreaking() {
		t.Fatalf("report must be breaking, got: %+v", report.Findings)
	}
}

// TestRound5_G3_OpenEnumAppliesWithinItsOwnSurface is the positive
// control for the test above: the SAME qualified entry must still open
// the enum it actually names, in the surface it actually names.
func TestRound5_G3_OpenEnumAppliesWithinItsOwnSurface(t *testing.T) {
	restFile := "rest/v1/x.schema.json"
	wsFile := "client-ws/v1/y.schema.json"

	manifestRaw := twoSurfaceManifest(t, restFile, wsFile, []string{restFile + "#/$defs/Automation/properties/status"})

	wsDoc := mustMarshalFile(t, minimalFile("https://narvi.dev/"+wsFile, defsOf("Widget", schemaObj("type", "string"))))
	restBase := mustMarshalFile(t, minimalFile("https://narvi.dev/"+restFile, defsOf("Automation", schemaObj(
		"type", "object", "properties", schemaObj("status", schemaObj("type", "string", "enum", []any{"active", "paused"})),
	))))
	restHead := mustMarshalFile(t, minimalFile("https://narvi.dev/"+restFile, defsOf("Automation", schemaObj(
		"type", "object", "properties", schemaObj("status", schemaObj("type", "string", "enum", []any{"active", "paused", "archived"})),
	))))

	report, err := Compare(Input{
		BaseManifestRaw:  manifestRaw,
		HeadManifestRaw:  manifestRaw,
		BaseVersion:      "1.0.0",
		HeadVersion:      "1.1.0",
		BaseChangelogRaw: []byte("## [1.0.0]\n"),
		HeadChangelogRaw: []byte("## [1.1.0]\n### " + restFile + "\n- Changed: Automation.status enum\n\n## [1.0.0]\n"),
		BaseRoutes:       []byte(""),
		HeadRoutes:       []byte(""),
		BaseSchemaFiles:  map[string][]byte{restFile: restBase, wsFile: wsDoc},
		HeadSchemaFiles:  map[string][]byte{restFile: restHead, wsFile: wsDoc},
	})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if containsFinding(report.Findings, "11", SeverityMajor) {
		t.Fatalf("an openEnums entry must still open the enum it actually names, in the surface it actually names, got: %+v", report.Findings)
	}
	if !containsFinding(report.Findings, "11", SeverityMinor) {
		t.Fatalf("want rule 11 MINOR, got: %+v", report.Findings)
	}
	if report.HasBreaking() {
		t.Fatalf("report must not be breaking, got: %+v", report.Findings)
	}
}

// TestRound5_G3_UnqualifiedOpenEnumEntryFailsClosed pins that an
// unqualified entry (the pre-round-5 format, no "<surface>#" prefix) is a
// manifest authoring error this checker fails closed on, rather than
// silently ignoring or (worse) matching every surface.
func TestRound5_G3_UnqualifiedOpenEnumEntryFailsClosed(t *testing.T) {
	schemaFile := "t/v1/x.schema.json"
	doc := mustMarshalFile(t, minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Status", schemaObj("type", "string", "enum", []any{"a", "b"}))))
	manifestRaw := mustMarshalFile(t, map[string]any{
		"version":   "1.0.0",
		"surfaces":  []any{map[string]any{"path": schemaFile, "direction": DirectiveBySuffix, "status": StatusCurrent}},
		"openEnums": []string{"#/$defs/Status"}, // unqualified -- no longer permitted
	})
	report, err := Compare(Input{
		BaseManifestRaw:  manifestRaw,
		HeadManifestRaw:  manifestRaw,
		BaseVersion:      "1.0.0",
		HeadVersion:      "2.0.0",
		BaseChangelogRaw: []byte("## [1.0.0]\n"),
		HeadChangelogRaw: []byte("## [2.0.0]\n### " + schemaFile + "\n- fixture\n\n## [1.0.0]\n"),
		BaseRoutes:       []byte(""),
		HeadRoutes:       []byte(""),
		BaseSchemaFiles:  map[string][]byte{schemaFile: doc},
		HeadSchemaFiles:  map[string][]byte{schemaFile: doc},
	})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !containsFinding(report.Findings, "fc-openenums-scope", SeverityFailClosed) {
		t.Fatalf("an unqualified openEnums entry must fail closed, got: %+v", report.Findings)
	}
	if !report.HasBreaking() {
		t.Fatalf("report must be breaking, got: %+v", report.Findings)
	}
}

// TestRound5_G3_UnknownSurfaceOpenEnumEntryFailsClosed pins that an entry
// qualified for a surface this manifest does not list is also a
// manifest authoring error, not a no-op.
func TestRound5_G3_UnknownSurfaceOpenEnumEntryFailsClosed(t *testing.T) {
	schemaFile := "t/v1/x.schema.json"
	doc := mustMarshalFile(t, minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Status", schemaObj("type", "string", "enum", []any{"a", "b"}))))
	manifestRaw := mustMarshalFile(t, map[string]any{
		"version":   "1.0.0",
		"surfaces":  []any{map[string]any{"path": schemaFile, "direction": DirectiveBySuffix, "status": StatusCurrent}},
		"openEnums": []string{"nonexistent/v1/other.schema.json#/$defs/Status"},
	})
	report, err := Compare(Input{
		BaseManifestRaw:  manifestRaw,
		HeadManifestRaw:  manifestRaw,
		BaseVersion:      "1.0.0",
		HeadVersion:      "2.0.0",
		BaseChangelogRaw: []byte("## [1.0.0]\n"),
		HeadChangelogRaw: []byte("## [2.0.0]\n### " + schemaFile + "\n- fixture\n\n## [1.0.0]\n"),
		BaseRoutes:       []byte(""),
		HeadRoutes:       []byte(""),
		BaseSchemaFiles:  map[string][]byte{schemaFile: doc},
		HeadSchemaFiles:  map[string][]byte{schemaFile: doc},
	})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !containsFinding(report.Findings, "fc-openenums-scope", SeverityFailClosed) {
		t.Fatalf("an openEnums entry naming an unknown surface must fail closed, got: %+v", report.Findings)
	}
}
