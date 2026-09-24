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
// (compare.go: `openEnums := baseManifest.OpenEnumSet()`) -- head opening
// the enum in the very same diff that adds a value to it must not help.
func TestOpenEnumsReadFromMergeBaseOnly(t *testing.T) {
	schemaFile := "t/v1/x.schema.json"
	baseSchema := mustMarshalFile(t, minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Status", schemaObj("type", "string", "enum", []any{"a", "b"}))))
	headSchema := mustMarshalFile(t, minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Status", schemaObj("type", "string", "enum", []any{"a", "b", "c"}))))

	in := Input{
		BaseManifestRaw:  minimalManifestJSON(t, nil),                // base does NOT open Status
		HeadManifestRaw:  minimalManifestJSON(t, []string{"Status"}), // head opens it in the SAME PR
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
		BaseManifestRaw:  minimalManifestJSON(t, []string{"Status"}), // already open in base
		HeadManifestRaw:  minimalManifestJSON(t, []string{"Status"}),
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
