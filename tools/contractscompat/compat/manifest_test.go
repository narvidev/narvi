package compat

import "testing"

// This file unit-tests Manifest's own openEnums scoping machinery (G3,
// round 5 review) directly -- compare_test.go's TestRound5_G3_* pin the
// same behavior through the full Compare pipeline; these are the
// narrower, faster checks on parseOpenEnumEntry/ValidateOpenEnums/
// OpenEnumSetForSurface themselves.

func TestManifest_ValidateOpenEnums_QualifiedEntriesOK(t *testing.T) {
	m := Manifest{
		Surfaces: []ManifestSurface{
			{Path: "rest/v1/dtos.schema.json"},
			{Path: "client-ws/v1/protocol.schema.json"},
		},
		OpenEnums: []string{
			"rest/v1/dtos.schema.json#/$defs/Session/properties/status",
			"client-ws/v1/protocol.schema.json#/$defs/Automation/properties/status",
		},
	}
	if err := m.ValidateOpenEnums(); err != nil {
		t.Fatalf("ValidateOpenEnums: unexpected error for properly-qualified entries: %v", err)
	}
}

func TestManifest_ValidateOpenEnums_UnqualifiedEntryFails(t *testing.T) {
	m := Manifest{
		Surfaces:  []ManifestSurface{{Path: "rest/v1/dtos.schema.json"}},
		OpenEnums: []string{"#/$defs/Session/properties/status"},
	}
	if err := m.ValidateOpenEnums(); err == nil {
		t.Fatal("ValidateOpenEnums: want an error for an unqualified entry, got nil")
	}
}

func TestManifest_ValidateOpenEnums_EmptySurfaceHalfFails(t *testing.T) {
	// "#/$defs/Session/properties/status" itself starts with "#", so a
	// naive split-on-first-"#" could treat an EMPTY string before it as
	// "qualified" -- must still be rejected as unqualified.
	m := Manifest{
		Surfaces:  []ManifestSurface{{Path: "rest/v1/dtos.schema.json"}},
		OpenEnums: []string{"#/$defs/Session/properties/status#extra"},
	}
	if err := m.ValidateOpenEnums(); err == nil {
		t.Fatal("ValidateOpenEnums: want an error for an entry with an empty surface half, got nil")
	}
}

func TestManifest_ValidateOpenEnums_UnknownSurfaceFails(t *testing.T) {
	m := Manifest{
		Surfaces:  []ManifestSurface{{Path: "rest/v1/dtos.schema.json"}},
		OpenEnums: []string{"client-ws/v1/protocol.schema.json#/$defs/Session/properties/status"},
	}
	if err := m.ValidateOpenEnums(); err == nil {
		t.Fatal("ValidateOpenEnums: want an error for an entry naming a surface not in this manifest, got nil")
	}
}

func TestManifest_ValidateOpenEnums_MalformedPointerHalfFails(t *testing.T) {
	m := Manifest{
		Surfaces:  []ManifestSurface{{Path: "rest/v1/dtos.schema.json"}},
		OpenEnums: []string{"rest/v1/dtos.schema.json#not-a-pointer"},
	}
	if err := m.ValidateOpenEnums(); err == nil {
		t.Fatal("ValidateOpenEnums: want an error for a malformed JSON Pointer half, got nil")
	}
}

func TestManifest_OpenEnumSetForSurface_ScopesToOneSurface(t *testing.T) {
	m := Manifest{
		Surfaces: []ManifestSurface{
			{Path: "rest/v1/dtos.schema.json"},
			{Path: "client-ws/v1/protocol.schema.json"},
		},
		OpenEnums: []string{
			"rest/v1/dtos.schema.json#/$defs/Automation/properties/status",
			"client-ws/v1/protocol.schema.json#/$defs/Widget/properties/status",
		},
	}

	restSet := m.OpenEnumSetForSurface("rest/v1/dtos.schema.json")
	if !restSet["#/$defs/Automation/properties/status"] {
		t.Fatalf("rest/v1's own entry must be present in its own set, got: %v", restSet)
	}
	if restSet["#/$defs/Widget/properties/status"] {
		t.Fatalf("client-ws's entry must NOT leak into rest/v1's own set, got: %v", restSet)
	}
	if len(restSet) != 1 {
		t.Fatalf("want exactly 1 entry in rest/v1's own set, got: %v", restSet)
	}

	wsSet := m.OpenEnumSetForSurface("client-ws/v1/protocol.schema.json")
	if !wsSet["#/$defs/Widget/properties/status"] {
		t.Fatalf("client-ws's own entry must be present in its own set, got: %v", wsSet)
	}
	if wsSet["#/$defs/Automation/properties/status"] {
		t.Fatalf("rest/v1's entry must NOT leak into client-ws's own set, got: %v", wsSet)
	}

	unrelatedSet := m.OpenEnumSetForSurface("sandbox-ws/v1/commands.schema.json")
	if len(unrelatedSet) != 0 {
		t.Fatalf("a surface with no openEnums entries of its own must get an empty set, got: %v", unrelatedSet)
	}
}
