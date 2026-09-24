package compat

import (
	"reflect"
	"sort"
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
// which property referenced it.

// assertIdenticalFindings asserts ref and inline produce byte-for-byte
// the same (sorted) finding set -- E1's own acceptance criterion is that
// a $ref member is graded EXACTLY like the equivalent inline spelling,
// not merely "close" to it.
func assertIdenticalFindings(t *testing.T, refFindings, inlineFindings []Finding) {
	t.Helper()
	sortFindings := func(fs []Finding) []Finding {
		out := append([]Finding{}, fs...)
		sort.Slice(out, func(i, j int) bool {
			if out[i].Pointer != out[j].Pointer {
				return out[i].Pointer < out[j].Pointer
			}
			if out[i].RuleID != out[j].RuleID {
				return out[i].RuleID < out[j].RuleID
			}
			return out[i].Message < out[j].Message
		})
		return out
	}
	a, b := sortFindings(refFindings), sortFindings(inlineFindings)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("a $ref union member must be graded EXACTLY like its inline spelling (E1):\n  $ref form:   %+v\n  inline form: %+v", a, b)
	}
}

// --- E1: diffUnion's keyOf resolves a $ref member through the base/head
// resolver and keys it by the RESOLVED shape, instead of trusting "any
// $ref = a discriminated variant" at face value. ---

// E1 case 1 ("scalar alias"): a $ref to a bare {"type":"string"} def
// added to an existing union must be graded row 7 (type widened, MAJOR
// on P2C), exactly like the inline {"type":"string"} member -- NOT row
// 28 (MINOR/MINOR "discriminated variant added"), which is what a bare
// "any $ref is 'ref:'+name" keying produced before this fix.
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

	refFindings, err := DiffDef(baseDefs, refHeadDefs, "Envelope", DirP2C, nil)
	if err != nil {
		t.Fatalf("DiffDef ($ref form): %v", err)
	}
	if !containsFinding(refFindings, "7", SeverityMajor) {
		t.Fatalf("a $ref to a scalar-type def added to a union must be row 7 MAJOR (type widened), got: %+v", refFindings)
	}
	if containsFinding(refFindings, "28", SeverityMinor) {
		t.Fatalf("must NOT be graded as the discriminated-variant row 28 just because it arrived via $ref, got: %+v", refFindings)
	}

	inlineFindings, err := DiffDef(baseDefs, inlineHeadDefs, "Envelope", DirP2C, nil)
	if err != nil {
		t.Fatalf("DiffDef (inline form): %v", err)
	}
	assertIdenticalFindings(t, refFindings, inlineFindings)
}

// E1 case 2 ("['null','string'] alias swapped for the null member"): a
// union's {"type":"null"} member is replaced with a $ref to a def whose
// own type is ["null","string"] -- the null member disappearing is row
// 10, and the field's own type ALSO gaining "string" is row 7 (MAJOR on
// P2C either way), never silently absorbed as a same-severity swap.
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

	refFindings, err := DiffDef(baseDefs, refHeadDefs, "Envelope", DirP2C, nil)
	if err != nil {
		t.Fatalf("DiffDef ($ref form): %v", err)
	}
	if !containsFinding(refFindings, "10", SeverityMinor) {
		t.Fatalf("the null member disappearing must still be row 10 MINOR (P2C), got: %+v", refFindings)
	}
	if !containsFinding(refFindings, "7", SeverityMajor) {
		t.Fatalf("the replacement member's own \"string\" type must be row 7 MAJOR (P2C) -- the control's own inline spelling, got: %+v", refFindings)
	}

	inlineFindings, err := DiffDef(baseDefs, inlineHeadDefs, "Envelope", DirP2C, nil)
	if err != nil {
		t.Fatalf("DiffDef (inline form): %v", err)
	}
	assertIdenticalFindings(t, refFindings, inlineFindings)
}

// E1 case 3 ("alias chain to integer"): a $ref that only resolves to a
// bare scalar type after following a chain of pure-alias defs (each
// legal under round 2's $ref-sibling rule: nothing but "$ref" itself)
// must still be graded by that final resolved type, not fail closed and
// not fall into row 28.
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

	refFindings, err := DiffDef(baseDefs, refHeadDefs, "Envelope", DirP2C, nil)
	if err != nil {
		t.Fatalf("DiffDef ($ref chain form): %v", err)
	}
	if !containsFinding(refFindings, "7", SeverityMajor) {
		t.Fatalf("a $ref chain resolving to a bare scalar type must be row 7 MAJOR, got: %+v", refFindings)
	}

	inlineFindings, err := DiffDef(baseDefs, inlineHeadDefs, "Envelope", DirP2C, nil)
	if err != nil {
		t.Fatalf("DiffDef (inline form): %v", err)
	}
	assertIdenticalFindings(t, refFindings, inlineFindings)
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

// E1 case 6 (events.schema.json's own root oneOf shape): a scalar alias
// appended to a whole SURFACE's root oneOf (not merely a $defs entry),
// under a "both" direction, must still be graded row 7 MAJOR (the P2C
// column), never row 28.
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
	if !containsFinding(refFindings, "7", SeverityMajor) {
		t.Fatalf("a scalar $ref alias added to the root oneOf must be row 7 MAJOR, got: %+v", refFindings)
	}
	if containsFinding(refFindings, "28", SeverityMinor) {
		t.Fatalf("must NOT be graded row 28, got: %+v", refFindings)
	}

	inlineFindings, err := DiffSurface(string(DirBoth), mustMarshal(base), mustMarshal(inlineHead), nil)
	if err != nil {
		t.Fatalf("DiffSurface (inline form): %v", err)
	}
	if !containsFinding(inlineFindings, "7", SeverityMajor) {
		t.Fatalf("inline control must also be row 7 MAJOR, got: %+v", inlineFindings)
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

// E2: the (base def, head def, direction) recursion guard skips a
// SECOND site retargeting the same pair -- this is only sound once
// grading no longer depends on the referencing path. Prove it: a def
// reached from two sites sharing the same retarget must be graded
// IDENTICALLY, both when computed together (where the guard skips the
// second site's own diff) and when computed for that second site ALONE
// (where nothing is skipped) -- so the skip never discards a different
// result.
func TestRound3_E2_RetargetRecursionGuardIsSound(t *testing.T) {
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
		"SessionStatus2": schemaObj("type", "string", "enum", []any{"a", "b", "c"}),
	}

	findings, err := DiffDef(baseDefs, headDefs, "Holder", DirP2C, nil)
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
		t.Fatalf("retargeting to a def whose closed enum gained a value must be row 27 MAJOR, got %v: %+v", statusSev, findings)
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
		"SessionStatus2": schemaObj("type", "string", "enum", []any{"a", "b", "c"}),
	}
	soloFindings, err := DiffDef(soloBaseDefs, soloHeadDefs, "Holder2", DirP2C, nil)
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
