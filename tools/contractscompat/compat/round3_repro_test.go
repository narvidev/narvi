package compat

import (
	"testing"
)

// This file pins the round-3 adversarial review's findings against PR
// #320 (see the PR body's "Review round 3" section, and E1-E8 in the
// review). Round 2 fixed the $ref-sibling family (D1-D5, D7, D8, D10,
// D11, D16, D21) by making any sibling besides "description" illegal
// outright. Round 3 found two further families: E1, a $ref UNION member
// was still keyed at face value instead of by what it actually resolves
// to; E2/E3/E7, openEnums matched the TRAVERSAL pointer a node happened
// to be reached through instead of the def that actually DECLARES the
// enum, so the same relaxation could be honored or lost depending on
// which property referenced it. E1's own fix was later superseded by the
// round-5 review (G1/G2, see this file's own E1 test comments below) --
// assertIdenticalFindings, which used to compare a $ref member's graded
// findings against its inline spelling's, no longer has a caller now
// that both forms simply fail closed identically (asserted directly by
// FailClosedError/RuleID checks instead) -- removed rather than kept
// around unused.

// --- E1: diffUnion's keyOf used to resolve a $ref member through the
// base/head resolver and key it by the RESOLVED shape, instead of
// trusting "any $ref = a discriminated variant" at face value.
// SUPERSEDED by the round-5 review (G1/G2): E1's own fix was itself
// still modeling a general union shape ("whatever a $ref resolves to,
// key it the way the equivalent inline spelling would key") -- G1/G2
// found the THIRD bypass in that lineage (E1, then F2, then G1/G2) and
// round 5 replaced the modeling outright with a whitelist of the two
// union shapes the real /contracts files actually use (defdiff.go's own
// union-section doc comment). A $ref to a bare scalar def -- case 1, 2,
// and 3 below -- matches neither permitted shape, so all three now fail
// closed, in BOTH the $ref and the inline form, rather than reaching
// rows 7/8/9/10 through a union member at all. That is at least as safe
// as the graded outcome these tests used to pin. ---

// E1 case 1 ("scalar alias"), now G1: a $ref to a bare {"type":"string"}
// def added to an existing union matches neither permitted union shape
// (not a pure discriminated object, not the bare null literal) and must
// fail closed -- in both the $ref and the equivalent inline form, the
// same way case 4/5 below (never graded at all) already do.
func TestRound3_E1_RefToScalarAliasIsTypeWidened(t *testing.T) {
	baseDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/Obj")}),
		"Obj":      schemaObj("type", "object"),
	}
	refHeadDefs := map[string]any{
		"Envelope":    schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/Obj"), schemaObj("$ref", "#/$defs/VerdictText")}),
		"Obj":         schemaObj("type", "object"),
		"VerdictText": schemaObj("type", "string"),
	}
	inlineHeadDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/Obj"), schemaObj("type", "string")}),
		"Obj":      schemaObj("type", "object"),
	}

	_, refErr := DiffDef(baseDefs, refHeadDefs, "Envelope", DirP2C, nil)
	refFC, ok := refErr.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for a $ref to a scalar-type def added to a union, got err=%v", refErr)
	}
	if refFC.Finding.RuleID != "fc-oneof-unpairable" {
		t.Fatalf("want fc-oneof-unpairable, got %s", refFC.Finding.RuleID)
	}

	_, inlineErr := DiffDef(baseDefs, inlineHeadDefs, "Envelope", DirP2C, nil)
	inlineFC, ok := inlineErr.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for the equivalent INLINE scalar member too, got err=%v", inlineErr)
	}
	if inlineFC.Finding.RuleID != "fc-oneof-unpairable" {
		t.Fatalf("want fc-oneof-unpairable, got %s", inlineFC.Finding.RuleID)
	}
}

// E1 case 2 ("['null','string'] alias swapped for the null member"), now
// G1: a union's {"type":"null"} member replaced with a $ref to a def
// whose own type is ["null","string"] matches neither permitted shape --
// shape B's null branch must be the bare INLINE {"type":"null"} literal,
// never a $ref (even to a def that itself allows null), and a def typed
// ["null","string"] is not a pure object def either. Both the $ref and
// inline forms fail closed.
func TestRound3_E1_RefToNullableStringAliasSwappedForNullMember(t *testing.T) {
	baseDefs := map[string]any{
		"Envelope": schemaObj("anyOf", []any{schemaObj("$ref", "#/$defs/Obj"), schemaObj("type", "null")}),
		"Obj":      schemaObj("type", "object"),
	}
	refHeadDefs := map[string]any{
		"Envelope":       schemaObj("anyOf", []any{schemaObj("$ref", "#/$defs/Obj"), schemaObj("$ref", "#/$defs/NullableString")}),
		"Obj":            schemaObj("type", "object"),
		"NullableString": schemaObj("type", []any{"null", "string"}),
	}
	inlineHeadDefs := map[string]any{
		"Envelope": schemaObj("anyOf", []any{schemaObj("$ref", "#/$defs/Obj"), schemaObj("type", []any{"null", "string"})}),
		"Obj":      schemaObj("type", "object"),
	}

	_, refErr := DiffDef(baseDefs, refHeadDefs, "Envelope", DirP2C, nil)
	refFC, ok := refErr.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for a $ref to a [\"null\",\"string\"] def swapped in for the null member, got err=%v", refErr)
	}
	if refFC.Finding.RuleID != "fc-oneof-unpairable" {
		t.Fatalf("want fc-oneof-unpairable, got %s", refFC.Finding.RuleID)
	}

	_, inlineErr := DiffDef(baseDefs, inlineHeadDefs, "Envelope", DirP2C, nil)
	inlineFC, ok := inlineErr.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for the equivalent INLINE [\"null\",\"string\"] member too, got err=%v", inlineErr)
	}
	if inlineFC.Finding.RuleID != "fc-oneof-unpairable" {
		t.Fatalf("want fc-oneof-unpairable, got %s", inlineFC.Finding.RuleID)
	}
}

// E1 case 3 ("alias chain to integer"), now G1: a $ref that only resolves
// to a bare scalar type after following a chain of pure-alias defs is
// still not a pure object def -- matches neither permitted shape, and
// must fail closed rather than being graded by its final resolved type.
func TestRound3_E1_RefChainToScalarIsTypeWidened(t *testing.T) {
	baseDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/Obj")}),
		"Obj":      schemaObj("type", "object"),
	}
	refHeadDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/Obj"), schemaObj("$ref", "#/$defs/AliasA")}),
		"Obj":      schemaObj("type", "object"),
		"AliasA":   schemaObj("$ref", "#/$defs/AliasB"),
		"AliasB":   schemaObj("type", "integer"),
	}
	inlineHeadDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/Obj"), schemaObj("type", "integer")}),
		"Obj":      schemaObj("type", "object"),
	}

	_, refErr := DiffDef(baseDefs, refHeadDefs, "Envelope", DirP2C, nil)
	refFC, ok := refErr.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for a $ref chain resolving to a bare scalar type, got err=%v", refErr)
	}
	if refFC.Finding.RuleID != "fc-oneof-unpairable" {
		t.Fatalf("want fc-oneof-unpairable, got %s", refFC.Finding.RuleID)
	}

	_, inlineErr := DiffDef(baseDefs, inlineHeadDefs, "Envelope", DirP2C, nil)
	inlineFC, ok := inlineErr.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for the equivalent INLINE scalar member too, got err=%v", inlineErr)
	}
	if inlineFC.Finding.RuleID != "fc-oneof-unpairable" {
		t.Fatalf("want fc-oneof-unpairable, got %s", inlineFC.Finding.RuleID)
	}
}

// E1 case 4 ("object-copy with type:['object','null']"): a def that
// mixes an object shape (its own "properties") with a type ARRAY
// ["object","null"] is neither a clean object def nor a bare scalar
// type -- it must fail closed, exactly as the same content written
// inline already does today.
func TestRound3_E1_MixedObjectNullDefFailsClosed(t *testing.T) {
	baseDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/Obj")}),
		"Obj":      schemaObj("type", "object", "properties", schemaObj("x", schemaObj("type", "string"))),
	}
	refHeadDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/Obj"), schemaObj("$ref", "#/$defs/ObjNullable")}),
		"Obj":      schemaObj("type", "object", "properties", schemaObj("x", schemaObj("type", "string"))),
		"ObjNullable": schemaObj("type", []any{"object", "null"},
			"properties", schemaObj("x", schemaObj("type", "string"))),
	}
	inlineHeadDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/Obj"), schemaObj(
			"type", []any{"object", "null"},
			"properties", schemaObj("x", schemaObj("type", "string")),
		)}),
		"Obj": schemaObj("type", "object", "properties", schemaObj("x", schemaObj("type", "string"))),
	}

	_, refErr := DiffDef(baseDefs, refHeadDefs, "Envelope", DirP2C, nil)
	refFC, ok := refErr.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for a $ref to a mixed object/null-array def, got err=%v", refErr)
	}
	if refFC.Finding.RuleID != "fc-oneof-unpairable" {
		t.Fatalf("want fc-oneof-unpairable, got %s", refFC.Finding.RuleID)
	}

	_, inlineErr := DiffDef(baseDefs, inlineHeadDefs, "Envelope", DirP2C, nil)
	inlineFC, ok := inlineErr.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for the equivalent INLINE mixed shape too, got err=%v", inlineErr)
	}
	if inlineFC.Finding.RuleID != "fc-oneof-unpairable" {
		t.Fatalf("want fc-oneof-unpairable, got %s", inlineFC.Finding.RuleID)
	}
}

// E1 case 5 ("array def"): a $ref to an array-shaped def cannot be
// paired as a union member and must fail closed, exactly as the same
// content written inline already does today.
func TestRound3_E1_RefToArrayDefFailsClosed(t *testing.T) {
	baseDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/Obj")}),
		"Obj":      schemaObj("type", "object"),
	}
	refHeadDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/Obj"), schemaObj("$ref", "#/$defs/ArrDef")}),
		"Obj":      schemaObj("type", "object"),
		"ArrDef":   schemaObj("type", "array", "items", schemaObj("type", "string")),
	}
	inlineHeadDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/Obj"), schemaObj("type", "array", "items", schemaObj("type", "string"))}),
		"Obj":      schemaObj("type", "object"),
	}

	_, refErr := DiffDef(baseDefs, refHeadDefs, "Envelope", DirP2C, nil)
	refFC, ok := refErr.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for a $ref to an array-shaped def, got err=%v", refErr)
	}
	if refFC.Finding.RuleID != "fc-oneof-unpairable" {
		t.Fatalf("want fc-oneof-unpairable, got %s", refFC.Finding.RuleID)
	}

	_, inlineErr := DiffDef(baseDefs, inlineHeadDefs, "Envelope", DirP2C, nil)
	inlineFC, ok := inlineErr.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for the equivalent INLINE array shape too, got err=%v", inlineErr)
	}
	if inlineFC.Finding.RuleID != "fc-oneof-unpairable" {
		t.Fatalf("want fc-oneof-unpairable, got %s", inlineFC.Finding.RuleID)
	}
}

// E1 case 6 (events.schema.json's own root oneOf shape), now G1: a
// scalar alias appended to a whole SURFACE's root oneOf (not merely a
// $defs entry) matches neither permitted union shape and must fail
// closed -- DiffSurface (unlike DiffDef) turns a root-level
// *FailClosedError into a Finding of SeverityFailClosed rather than a Go
// error (see its own doc comment), so the assertion here checks the
// finding, not err.
func TestRound3_E1_RootOneOfScalarAliasIsTypeWidened(t *testing.T) {
	buildRoot := func(extraDefs map[string]any, extraMember any) map[string]any {
		defs := map[string]any{"Ready": schemaObj("type", "object")}
		for k, v := range extraDefs {
			defs[k] = v
		}
		oneOf := []any{schemaObj("$ref", "#/$defs/Ready")}
		if extraMember != nil {
			oneOf = append(oneOf, extraMember)
		}
		return schemaObj(
			"$schema", "https://json-schema.org/draft/2020-12/schema",
			"$id", "https://narvi.dev/events.schema.json",
			"title", "Events",
			"description", "test file",
			"$defs", defs,
			"oneOf", oneOf,
		)
	}

	base := buildRoot(nil, nil)
	refHead := buildRoot(map[string]any{"TextAlias": schemaObj("type", "string")}, schemaObj("$ref", "#/$defs/TextAlias"))
	inlineHead := buildRoot(nil, schemaObj("type", "string"))

	refFindings, err := DiffSurface(string(DirBoth), mustMarshal(base), mustMarshal(refHead), nil)
	if err != nil {
		t.Fatalf("DiffSurface ($ref form): %v", err)
	}
	if !containsFinding(refFindings, "fc-oneof-unpairable", SeverityFailClosed) {
		t.Fatalf("a scalar $ref alias added to the root oneOf must fail closed, got: %+v", refFindings)
	}

	inlineFindings, err := DiffSurface(string(DirBoth), mustMarshal(base), mustMarshal(inlineHead), nil)
	if err != nil {
		t.Fatalf("DiffSurface (inline form): %v", err)
	}
	if !containsFinding(inlineFindings, "fc-oneof-unpairable", SeverityFailClosed) {
		t.Fatalf("the equivalent INLINE scalar member must also fail closed, got: %+v", inlineFindings)
	}
}

// --- E2/E3/E7: openEnums matches a node's DEFINING location (the
// $defs entry that declares the enum, plus the path inside it), never
// the traversal path a particular caller happened to reach it through.
// ---

// E3: an open enum's relaxation must survive being reached through a
// $ref from a def that ALSO changed for an unrelated reason in the same
// diff -- two independently-MINOR changes must not combine into a false
// MAJOR just because one of them happens to touch a sibling of the
// def that declares the open enum.
func TestRound3_E3_OpenEnumSurvivesUnrelatedSiblingEdit(t *testing.T) {
	openEnums := map[string]bool{"#/$defs/Automation/properties/status": true}

	baseDefs := map[string]any{
		"CreateAutomationResponse": schemaObj("type", "object", "properties", schemaObj(
			"automation", schemaObj("$ref", "#/$defs/Automation"),
		)),
		"Automation": schemaObj("type", "object", "properties", schemaObj(
			"status", schemaObj("type", "string", "enum", []any{"a", "b"}),
		)),
	}
	headDefs := map[string]any{
		"CreateAutomationResponse": schemaObj("type", "object", "properties", schemaObj(
			"automation", schemaObj("$ref", "#/$defs/Automation"),
			// Unrelated addition: a brand-new optional property on the
			// SAME def, nothing to do with the enum at all.
			"warnings", schemaObj("type", "array", "items", schemaObj("type", "string")),
		)),
		"Automation": schemaObj("type", "object", "properties", schemaObj(
			// The enum change: adding a value already covered by
			// openEnums.
			"status", schemaObj("type", "string", "enum", []any{"a", "b", "archived"}),
		)),
	}

	findings, err := DiffDef(baseDefs, headDefs, "CreateAutomationResponse", DirP2C, openEnums)
	if err != nil {
		t.Fatalf("DiffDef: %v", err)
	}
	if containsFinding(findings, "11", SeverityMajor) {
		t.Fatalf("an open enum reached through an unrelated sibling edit must NOT re-grade as closed (MAJOR), got: %+v", findings)
	}
	if !containsFinding(findings, "11", SeverityMinor) {
		t.Fatalf("the enum addition must still be reported, as MINOR (open), got: %+v", findings)
	}
	if !containsFinding(findings, "2", SeverityMinor) {
		t.Fatalf("the unrelated \"warnings\" property addition must still be reported, got: %+v", findings)
	}
}

// E7: openEnums must be usable on a surface whose own ROOT is a $ref
// (session-config's real shape) -- the def's enum, reached once through
// DiffSurface's own $defs loop and once through the root's own $ref,
// must be graded the SAME (open, MINOR) both times, never MAJOR at
// either location.
func TestRound3_E7_OpenEnumUsableOnRootRefSurface(t *testing.T) {
	openEnums := map[string]bool{"#/$defs/SessionConfig/properties/bootMode": true}

	buildRoot := func(bootModeValues []any) map[string]any {
		return schemaObj(
			"$schema", "https://json-schema.org/draft/2020-12/schema",
			"$id", "https://narvi.dev/session-config.schema.json",
			"title", "SessionConfig",
			"description", "test file",
			"$ref", "#/$defs/SessionConfig",
			"$defs", defsOf("SessionConfig", schemaObj(
				"type", "object",
				"properties", schemaObj("bootMode", schemaObj("type", "string", "enum", bootModeValues)),
			)),
		)
	}

	base := buildRoot([]any{"cold", "warm"})
	head := buildRoot([]any{"cold", "warm", "hibernating"})

	findings, err := DiffSurface(string(DirP2C), mustMarshal(base), mustMarshal(head), openEnums)
	if err != nil {
		t.Fatalf("DiffSurface: %v", err)
	}
	if containsFinding(findings, "11", SeverityMajor) {
		t.Fatalf("an open enum on a root-$ref surface must never grade MAJOR at any location, got: %+v", findings)
	}
	if !containsFinding(findings, "11", SeverityMinor) {
		t.Fatalf("the enum addition must still be reported, as MINOR (open), got: %+v", findings)
	}
}

// E2/F4: the (base def, head def, direction) recursion guard skips a
// SECOND site retargeting the same pair -- this is only sound once
// grading no longer depends on the referencing path. Prove it: a def
// reached from two sites sharing the same retarget must be graded
// IDENTICALLY, both when computed together (where the guard skips the
// second site's own diff) and when computed for that second site ALONE
// (where nothing is skipped) -- so the skip never discards a different
// result.
//
// F4 (round 4): the round-3 version of this test passed openEnums=nil
// AND dropped SessionStatus from head entirely, so diffRetargetedRef's
// FIRST branch ("and %q no longer exists", an unconditional MAJOR)
// fired before line 380's targetLoc was ever built -- the test never
// actually exercised the code path it claimed to pin. Reverting E2's own
// fix (diffRetargetedRef's targetLoc back to `targetLoc := l`, grading
// by the referencing node's own TRAVERSAL location instead of the
// retarget's (old, new) pair) left the whole suite green, this test
// included. This version keeps SessionStatus present, unchanged, in
// HEAD (so diffRetargetedRef reaches the targetLoc computation for
// real), and gives openEnums a TRAVERSAL-shaped pointer
// ("#/$defs/Holder/properties/status") that only a location computed
// from the referencing path -- never one computed from the retargeted
// (old, new) def pair itself -- could ever match. Under the real fix,
// that pointer never matches (isOpen is false at both sites, defPtr and
// oldDefPtr both name SessionStatus/SessionStatus2), so "status" and
// "zzzStatus" grade IDENTICALLY (both MAJOR), same as before. Under the
// `targetLoc := l` revert, "status" (visited first, sharing its own
// traversal pointer with openEnums' entry) grades MINOR while
// "zzzStatus" -- diffed in isolation below, its own traversal pointer
// NOT in openEnums -- grades MAJOR: the mismatch this test's final
// assertion catches.
func TestRound3_E2_RetargetRecursionGuardIsSound(t *testing.T) {
	// A pointer shaped like a REFERENCING SITE's own traversal path, not
	// like either def's own canonical "#/$defs/<Name>" root -- COMPATIBILITY.md's
	// convention (and F1's fix) means this can never legitimately open
	// anything; it exists purely to detect whether grading has regressed
	// to using the traversal path again.
	openEnums := map[string]bool{"#/$defs/Holder/properties/status": true}

	baseDefs := map[string]any{
		"Holder": schemaObj("type", "object", "properties", schemaObj(
			"status", schemaObj("$ref", "#/$defs/SessionStatus"),
			// "z..." sorts after "status", so diffProperties (sorted key
			// order) visits "status" FIRST and "zzzStatus" SECOND -- the
			// second site is exactly the one the recursion guard would
			// skip.
			"zzzStatus", schemaObj("$ref", "#/$defs/SessionStatus"),
		)),
		"SessionStatus": schemaObj("type", "string", "enum", []any{"a", "b"}),
	}
	headDefs := map[string]any{
		"Holder": schemaObj("type", "object", "properties", schemaObj(
			"status", schemaObj("$ref", "#/$defs/SessionStatus2"),
			"zzzStatus", schemaObj("$ref", "#/$defs/SessionStatus2"),
		)),
		// Kept, unchanged, so the retarget's OLD target still exists in
		// HEAD -- diffRetargetedRef's own "no longer exists" branch (an
		// unconditional MAJOR that returns before targetLoc is ever
		// built) must NOT fire, or this test would not exercise the
		// fixed code at all (round 3's own gap).
		"SessionStatus":  schemaObj("type", "string", "enum", []any{"a", "b"}),
		"SessionStatus2": schemaObj("type", "string", "enum", []any{"a", "b", "c"}),
	}

	findings, err := DiffDef(baseDefs, headDefs, "Holder", DirP2C, openEnums)
	if err != nil {
		t.Fatalf("DiffDef: %v", err)
	}
	var statusSev Severity
	found := false
	for _, f := range findings {
		if f.RuleID == "27" && f.Pointer == "#/$defs/Holder/properties/status" {
			statusSev = f.Severity
			found = true
		}
	}
	if !found {
		t.Fatalf("want a row-27 finding at .../properties/status, got: %+v", findings)
	}
	if statusSev != SeverityMajor {
		t.Fatalf("a traversal-shaped openEnums entry must never open this retarget's enum addition (F1: grading is by the (old,new) def pair's own pointers, never the referencing path) -- got %v: %+v", statusSev, findings)
	}

	// Now diff the SECOND site (zzzStatus) completely on its own, in a
	// fresh ctx with nothing cached -- this is the result the recursion
	// guard's skip is implicitly claiming would be identical.
	soloBaseDefs := map[string]any{
		"Holder2":       schemaObj("type", "object", "properties", schemaObj("zzzStatus", schemaObj("$ref", "#/$defs/SessionStatus"))),
		"SessionStatus": schemaObj("type", "string", "enum", []any{"a", "b"}),
	}
	soloHeadDefs := map[string]any{
		"Holder2":        schemaObj("type", "object", "properties", schemaObj("zzzStatus", schemaObj("$ref", "#/$defs/SessionStatus2"))),
		"SessionStatus":  schemaObj("type", "string", "enum", []any{"a", "b"}),
		"SessionStatus2": schemaObj("type", "string", "enum", []any{"a", "b", "c"}),
	}
	soloFindings, err := DiffDef(soloBaseDefs, soloHeadDefs, "Holder2", DirP2C, openEnums)
	if err != nil {
		t.Fatalf("DiffDef (solo zzzStatus): %v", err)
	}
	soloSev := SeverityPatch
	soloFound := false
	for _, f := range soloFindings {
		if f.RuleID == "27" {
			soloSev = f.Severity
			soloFound = true
		}
	}
	if !soloFound {
		t.Fatalf("want a row-27 finding when zzzStatus's own retarget is diffed in isolation, got: %+v", soloFindings)
	}
	if soloSev != statusSev {
		t.Fatalf("the second site, diffed in isolation, must grade IDENTICALLY to the first site (both %v) -- the recursion guard's skip must never discard a DIFFERENT result; got solo=%v shared=%v", statusSev, soloSev, statusSev)
	}
}

// --- E8: in genesis mode (no real base manifest.json at all), NEITHER
// relaxation may be honoured -- openEnums is treated as empty, and no
// surface's `retired` status is trusted -- because both relaxations
// only ever come from a base manifest an earlier, REVIEWED PR actually
// established, and genesis mode has no such PR: main.go's own
// BaseManifestRaw substitution is just a copy of HEAD's own claims. ---

// E8 case 1 (openEnums): a genesis-mode PR that adds an openEnums
// pointer AND a value to that enum, in the same diff, must still be
// MAJOR (an enum reachable only through the synthesized base's own
// openEnums list is not "already open" -- nothing reviewed established
// that).
func TestRound3_E8_GenesisOpenEnumsNotHonoured(t *testing.T) {
	schemaFile := "t/v1/x.schema.json"
	baseSchema := mustMarshalFile(t, minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Status", schemaObj("type", "string", "enum", []any{"a", "b"}))))
	headSchema := mustMarshalFile(t, minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Status", schemaObj("type", "string", "enum", []any{"a", "b", "c"}))))

	// Genesis substitution means BaseManifestRaw is literally a copy of
	// HeadManifestRaw -- so if head's own manifest already opens the
	// enum (the exploit's whole premise), the synthesized base does too.
	manifestWithOpenEnum := minimalManifestJSON(t, []string{"#/$defs/Status"})

	in := Input{
		BaseManifestRaw:  manifestWithOpenEnum,
		HeadManifestRaw:  manifestWithOpenEnum,
		BaseVersion:      "0.0.0",
		HeadVersion:      "1.1.0",
		BaseChangelogRaw: nil,
		HeadChangelogRaw: []byte("## [1.1.0]\n### " + schemaFile + "\n- Changed: Status enum, opened openEnums\n\n"),
		BaseRoutes:       []byte(""),
		HeadRoutes:       []byte(""),
		BaseSchemaFiles:  map[string][]byte{schemaFile: baseSchema},
		HeadSchemaFiles:  map[string][]byte{schemaFile: headSchema},
		Genesis:          true,
	}

	report, err := Compare(in)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !containsFinding(report.Findings, "11", SeverityMajor) {
		t.Fatalf("a genesis-mode openEnums entry must never relax an enum addition to MINOR, got: %+v", report.Findings)
	}
	if containsFinding(report.Findings, "11", SeverityMinor) {
		t.Fatalf("must NOT ALSO report the same change as MINOR, got: %+v", report.Findings)
	}
	if !report.HasBreaking() {
		t.Fatal("report must be breaking")
	}
}

// E8 case 2 (`retired` status): a genesis-mode base manifest that
// claims a surface is already `retired` must not exempt that surface's
// deletion from row 37's MAJOR severity.
func TestRound3_E8_GenesisRetiredStatusNotHonoured(t *testing.T) {
	retiredFile := "t/v1/retired.schema.json"
	keptFile := "t/v1/kept.schema.json"
	retiredDoc := mustMarshal(minimalFile("https://narvi.dev/"+retiredFile, defsOf("Widget", schemaObj("type", "string"))))
	keptDoc := mustMarshal(minimalFile("https://narvi.dev/"+keptFile, defsOf("Other", schemaObj("type", "string"))))

	// Genesis substitution: this row's "retired" status exists ONLY
	// because it is a synthesized copy of HEAD's own claim -- no
	// earlier, reviewed PR actually established it.
	baseManifest := mustMarshal(map[string]any{
		"version": "1.0.0",
		"surfaces": []any{
			map[string]any{"path": retiredFile, "direction": string(DirP2C), "status": StatusRetired},
			map[string]any{"path": keptFile, "direction": DirectiveBySuffix, "status": StatusCurrent},
		},
	})
	headManifest := mustMarshal(map[string]any{
		"version": "1.0.1",
		"surfaces": []any{
			map[string]any{"path": keptFile, "direction": DirectiveBySuffix, "status": StatusCurrent},
		},
	})

	in := Input{
		BaseManifestRaw:  baseManifest,
		HeadManifestRaw:  headManifest,
		BaseVersion:      "1.0.0",
		HeadVersion:      "1.0.1",
		BaseChangelogRaw: []byte("## [1.0.0]\n"),
		HeadChangelogRaw: []byte("## [1.0.1]\n- deleted a supposedly-already-retired surface\n\n## [1.0.0]\n"),
		BaseRoutes:       []byte(""),
		HeadRoutes:       []byte(""),
		BaseSchemaFiles:  map[string][]byte{retiredFile: retiredDoc, keptFile: keptDoc},
		HeadSchemaFiles:  map[string][]byte{keptFile: keptDoc},
		Genesis:          true,
	}

	report, err := Compare(in)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !containsFinding(report.Findings, "37", SeverityMajor) {
		t.Fatalf("a genesis-mode `retired` status must never exempt a deletion from row 37 MAJOR, got: %+v", report.Findings)
	}
	if !report.HasBreaking() {
		t.Fatal("report must be breaking")
	}
}
