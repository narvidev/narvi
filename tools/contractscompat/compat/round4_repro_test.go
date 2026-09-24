package compat

import "testing"

// This file pins the round-4 adversarial review's findings against PR
// #320 (see the PR body's "Review round 4" section). F1 (a retarget's
// added enum value must be open at BOTH the old and new target's own
// pointer) is pinned by three new corpus cases in compat_test.go instead
// -- it fits that table's existing base/head/openEnums shape directly.
// F3 (row 19 "removed" branch) is likewise three new compat_test.go
// corpus cases. F4 rewrites TestRound3_E2_RetargetRecursionGuardIsSound
// in round3_repro_test.go itself, in place, since it is a fix to an
// EXISTING pin, not a new one. F5 (genesis direction notice) is pinned
// in main_test.go, next to the CLI's own other output-format tests.

// --- F2: a $ref union member counts as an object variant (rows 28/29)
// only if its resolved def's own "type" is EXACTLY the bare string
// "object" -- not an array that also allows "null" or another type, and
// not absent -- regardless of whether the def also carries a
// properties.type.const discriminator. Before this fix,
// resolvedUnionMemberKey treated ANY def with a properties.type.const as
// a discriminated object variant without ever looking at "type" at all,
// so a def that also accepted null (or any other JSON type) through the
// very same union member was silently classified as if it were a pure
// object, and MINOR-graded on addition (row 28) instead of failing
// closed the way the identical shape already does when written inline
// (TestRound3_E1_MixedObjectNullDefFailsClosed). ---

// F2 case 1: a $ref member whose def has BOTH a properties.type.const
// discriminator AND a "type" that allows null must fail closed, not
// grade as row 28 MINOR -- a null value is not something an old,
// object-only consumer of this union can safely skip over.
func TestRound4_F2_DiscriminatorWithNullableTypeFailsClosed(t *testing.T) {
	baseDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A")}),
		"A": schemaObj("type", "object",
			"properties", schemaObj("kind", schemaObj("const", "a")),
			"required", []any{"kind"},
		),
	}
	refHeadDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A"), schemaObj("$ref", "#/$defs/Zz")}),
		"A": schemaObj("type", "object",
			"properties", schemaObj("kind", schemaObj("const", "a")),
			"required", []any{"kind"},
		),
		// Zz mixes a discriminator (properties.kind.const) with a "type"
		// that ALSO admits "null" -- the exact shape round 4's finding
		// reproduced against the real binary (constructed there as
		// sandbox-ws/v1/commands.schema.json's $defs.Zz).
		"Zz": schemaObj("type", []any{"object", "null"},
			"additionalProperties", false,
			"required", []any{"kind"},
			"properties", schemaObj("kind", schemaObj("type", "string", "const", "zz")),
		),
	}

	_, err := DiffDef(baseDefs, refHeadDefs, "Envelope", DirP2C, nil)
	fc, ok := err.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for a $ref union member whose def allows null alongside a discriminator, got err=%v", err)
	}
	if fc.Finding.RuleID != "fc-oneof-unpairable" {
		t.Fatalf("want fc-oneof-unpairable, got %s", fc.Finding.RuleID)
	}
}

// F2 case 2: a $ref member whose def has a properties.type.const
// discriminator but NO "type" keyword at all must also fail closed.
func TestRound4_F2_DiscriminatorWithNoTypeFailsClosed(t *testing.T) {
	baseDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A")}),
		"A": schemaObj("type", "object",
			"properties", schemaObj("kind", schemaObj("const", "a")),
			"required", []any{"kind"},
		),
	}
	refHeadDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A"), schemaObj("$ref", "#/$defs/Zz")}),
		"A": schemaObj("type", "object",
			"properties", schemaObj("kind", schemaObj("const", "a")),
			"required", []any{"kind"},
		),
		// Zz has a discriminator but never says "type" at all -- nothing
		// here rules out any other JSON type showing up on the wire.
		"Zz": schemaObj(
			"required", []any{"kind"},
			"properties", schemaObj("kind", schemaObj("const", "zz")),
		),
	}

	_, err := DiffDef(baseDefs, refHeadDefs, "Envelope", DirP2C, nil)
	fc, ok := err.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for a $ref union member whose def has no \"type\" at all, got err=%v", err)
	}
	if fc.Finding.RuleID != "fc-oneof-unpairable" {
		t.Fatalf("want fc-oneof-unpairable, got %s", fc.Finding.RuleID)
	}
}

// F2 case 3: a $ref member whose def has a properties.type.const
// discriminator but a "type" of a DIFFERENT bare scalar (here "string")
// must also fail closed -- it is neither a pure object def nor (because
// it also carries "properties") a bare scalar type bareUnionTypeKey can
// key on.
func TestRound4_F2_DiscriminatorWithStringTypeFailsClosed(t *testing.T) {
	baseDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A")}),
		"A": schemaObj("type", "object",
			"properties", schemaObj("kind", schemaObj("const", "a")),
			"required", []any{"kind"},
		),
	}
	refHeadDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A"), schemaObj("$ref", "#/$defs/Zz")}),
		"A": schemaObj("type", "object",
			"properties", schemaObj("kind", schemaObj("const", "a")),
			"required", []any{"kind"},
		),
		"Zz": schemaObj("type", "string",
			"properties", schemaObj("kind", schemaObj("const", "zz")),
		),
	}

	_, err := DiffDef(baseDefs, refHeadDefs, "Envelope", DirP2C, nil)
	fc, ok := err.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for a $ref union member typed \"string\" with a discriminator, got err=%v", err)
	}
	if fc.Finding.RuleID != "fc-oneof-unpairable" {
		t.Fatalf("want fc-oneof-unpairable, got %s", fc.Finding.RuleID)
	}
}

// --- F6: when a $ref crosses into a pure alias def (a $defs entry that
// is itself a bare $ref -- walkSchema explicitly allows this shape), the
// defining pointer used to look up openEnums must reset to the TERMINAL
// def the alias chain resolves to, not the alias's own name. Before this
// fix, an open enum reached through such an alias was re-graded as
// closed (false MAJOR) the moment anything else in the referencing path
// changed, because intoDef/diffRetargetedRef rooted the pointer at the
// alias's own name instead of following the chain. ---

// F6: SessionAlias is a pure alias for Session ({"$ref":"#/$defs/
// Session"}, nothing else). A value added to Session's OPEN enum, while
// ALSO touching an unrelated sibling of the def that references it
// through the alias, must still grade MINOR -- not MAJOR just because
// the enum was reached one alias hop away from where openEnums names it.
func TestRound4_F6_OpenEnumSurvivesAliasIndirection(t *testing.T) {
	openEnums := map[string]bool{"#/$defs/Session/properties/status": true}

	baseDefs := map[string]any{
		"Holder": schemaObj("type", "object", "properties", schemaObj(
			"zzzSession", schemaObj("$ref", "#/$defs/SessionAlias"),
		)),
		"SessionAlias": schemaObj("$ref", "#/$defs/Session"),
		"Session": schemaObj("type", "object", "properties", schemaObj(
			"status", schemaObj("type", "string", "enum", []any{"a", "b"}),
		)),
	}
	headDefs := map[string]any{
		"Holder": schemaObj("type", "object", "properties", schemaObj(
			// Unrelated addition alongside the aliased reference, so
			// diffResolved's own DeepEqual short-circuit does not skip
			// recursing into "zzzSession" entirely.
			"zzzSession", schemaObj("$ref", "#/$defs/SessionAlias"),
			"warnings", schemaObj("type", "array", "items", schemaObj("type", "string")),
		)),
		"SessionAlias": schemaObj("$ref", "#/$defs/Session"),
		"Session": schemaObj("type", "object", "properties", schemaObj(
			"status", schemaObj("type", "string", "enum", []any{"a", "b", "archived"}),
		)),
	}

	findings, err := DiffDef(baseDefs, headDefs, "Holder", DirP2C, openEnums)
	if err != nil {
		t.Fatalf("DiffDef: %v", err)
	}
	if containsFinding(findings, "11", SeverityMajor) {
		t.Fatalf("an open enum reached through a pure alias def must NOT re-grade as closed (MAJOR), got: %+v", findings)
	}
	if !containsFinding(findings, "11", SeverityMinor) {
		t.Fatalf("the enum addition must still be reported, as MINOR (open), got: %+v", findings)
	}
	if !containsFinding(findings, "2", SeverityMinor) {
		t.Fatalf("the unrelated \"warnings\" property addition must still be reported, got: %+v", findings)
	}

	// Control: diffing SessionAlias directly (no indirection at all)
	// must produce the exact same MINOR grading -- confirming the alias
	// hop itself is what F6 fixes, not some other difference between the
	// two scenarios.
	directFindings, err := DiffDef(baseDefs, headDefs, "SessionAlias", DirP2C, openEnums)
	if err != nil {
		t.Fatalf("DiffDef (SessionAlias directly): %v", err)
	}
	if !containsFinding(directFindings, "11", SeverityMinor) {
		t.Fatalf("diffing SessionAlias directly must also grade the enum addition MINOR (open), got: %+v", directFindings)
	}
}

// F6 control: the same alias-indirection shape, but the enum addition is
// NOT covered by openEnums at all -- must still grade MAJOR, exactly
// like the equivalent direct (non-aliased) reference would. This rules
// out a fix that accidentally makes EVERYTHING reached through an alias
// open, rather than correctly resolving to the terminal def's own
// pointer.
func TestRound4_F6_ClosedEnumStillMajorThroughAlias(t *testing.T) {
	baseDefs := map[string]any{
		"Holder": schemaObj("type", "object", "properties", schemaObj(
			"zzzSession", schemaObj("$ref", "#/$defs/SessionAlias"),
		)),
		"SessionAlias": schemaObj("$ref", "#/$defs/Session"),
		"Session": schemaObj("type", "object", "properties", schemaObj(
			"status", schemaObj("type", "string", "enum", []any{"a", "b"}),
		)),
	}
	headDefs := map[string]any{
		"Holder": schemaObj("type", "object", "properties", schemaObj(
			"zzzSession", schemaObj("$ref", "#/$defs/SessionAlias"),
			"warnings", schemaObj("type", "array", "items", schemaObj("type", "string")),
		)),
		"SessionAlias": schemaObj("$ref", "#/$defs/Session"),
		"Session": schemaObj("type", "object", "properties", schemaObj(
			"status", schemaObj("type", "string", "enum", []any{"a", "b", "archived"}),
		)),
	}

	findings, err := DiffDef(baseDefs, headDefs, "Holder", DirP2C, nil)
	if err != nil {
		t.Fatalf("DiffDef: %v", err)
	}
	if !containsFinding(findings, "11", SeverityMajor) {
		t.Fatalf("an enum addition not covered by openEnums must still grade MAJOR through an alias, got: %+v", findings)
	}
}
