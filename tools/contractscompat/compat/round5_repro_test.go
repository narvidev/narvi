package compat

import "testing"

// This file pins the round-5 adversarial review's findings against PR
// #320 (see the PR body's "Review round 5" section, and G1-G3 in the
// review). G1/G2 are the third straight round to find a bypass in
// oneOf/anyOf handling by MODELING a general union shape (round 3's E1,
// round 4's F2, and now this round) -- the fix this time is not another
// model, it is defdiff.go's own whitelist of the exact two shapes the
// five real /contracts files use (see its own union-section doc comment,
// and COMPATIBILITY.md's "Permitted oneOf/anyOf shapes"). G3 (openEnums
// scoped per surface) is pinned in compare_test.go / manifest_test.go
// instead, next to the rest of the Compare-level relaxation machinery.

// --- G1: an inline discriminated union member must fail closed exactly
// like the identical shape behind a $ref does -- in every "type" variant
// a real PR could spell it (object+null, a bare scalar, no "type" at
// all, or an array), on shape A (commands/events-like: a root oneOf of
// several discriminated variants) AND on shape B (dtos-like: a nullable
// single object). ---

// commandsLikeRoot builds a minimal shape-A document (sandbox-ws/v1/
// commands.schema.json's own root oneOf shape): two discriminated $ref
// variants, "Prompt" (const "prompt") and "Stop" (const "stop"), plus
// whatever extra $defs/oneOf member the caller wants to layer on.
func commandsLikeRoot(extraDefs map[string]any, extraMember any) map[string]any {
	defs := map[string]any{
		"Prompt": schemaObj("type", "object", "additionalProperties", false,
			"properties", schemaObj("type", schemaObj("type", "string", "const", "prompt")),
			"required", []any{"type"}),
		"Stop": schemaObj("type", "object", "additionalProperties", false,
			"properties", schemaObj("type", schemaObj("type", "string", "const", "stop")),
			"required", []any{"type"}),
	}
	for k, v := range extraDefs {
		defs[k] = v
	}
	oneOf := []any{schemaObj("$ref", "#/$defs/Prompt"), schemaObj("$ref", "#/$defs/Stop")}
	if extraMember != nil {
		oneOf = append(oneOf, extraMember)
	}
	return schemaObj(
		"$schema", "https://json-schema.org/draft/2020-12/schema",
		"$id", "https://narvi.dev/t/v1/commands.schema.json",
		"title", "Commands",
		"description", "test fixture mirroring sandbox-ws/v1/commands.schema.json's own root oneOf shape",
		"$defs", defs,
		"oneOf", oneOf,
	)
}

// inlineDiscriminatedVariants is the four "type" spellings G1's real
// repro used against commands.schema.json/events.schema.json/
// ReviewReadout.latestVerdict: an object also admitting null, a bare
// scalar, no "type" keyword at all, and an array -- every one of them
// carries a properties.type.const discriminator ("zz"), which is exactly
// what let each slip past the OLD per-member keying (round 4's own F2
// fix only closed this gap for a member reached through a $ref).
func inlineDiscriminatedVariants() map[string]any {
	disc := schemaObj("type", schemaObj("type", "string", "const", "zz"))
	return map[string]any{
		"objnull": schemaObj("type", []any{"object", "null"}, "additionalProperties", false, "required", []any{"type"}, "properties", disc),
		"string":  schemaObj("type", "string", "properties", disc),
		"notype":  schemaObj("required", []any{"type"}, "properties", disc),
		"array":   schemaObj("type", "array", "properties", disc),
	}
}

func TestRound5_G1_InlineDiscriminatedMemberOnShapeAFailsClosed(t *testing.T) {
	for name, member := range inlineDiscriminatedVariants() {
		t.Run(name, func(t *testing.T) {
			base := commandsLikeRoot(nil, nil)
			head := commandsLikeRoot(nil, member)
			findings, err := DiffSurface(string(DirP2C), mustMarshal(base), mustMarshal(head), nil)
			if err != nil {
				t.Fatalf("DiffSurface: unexpected hard error (want a Finding, not a Go error): %v", err)
			}
			if !containsFinding(findings, "fc-oneof-unpairable", SeverityFailClosed) {
				t.Fatalf("an inline discriminated member (type variant %q) added to a shape-A root oneOf must fail closed, got: %+v", name, findings)
			}
			if containsFinding(findings, "28", SeverityMinor) {
				t.Fatalf("must NOT be silently accepted as row 28 MINOR, got: %+v", findings)
			}
		})
	}
}

// reviewReadoutLikeDefs builds a minimal shape-B def (dtos.schema.json's
// own ReviewReadout.latestVerdict anyOf shape): a single $ref to a pure
// object def, plus a bare null literal.
func reviewReadoutLikeDefs(extra map[string]any, extraMember any) map[string]any {
	defs := map[string]any{
		"Wrapper": schemaObj("anyOf", func() []any {
			arr := []any{schemaObj("$ref", "#/$defs/Verdict"), schemaObj("type", "null")}
			if extraMember != nil {
				arr = append(arr, extraMember)
			}
			return arr
		}()),
		"Verdict": schemaObj("type", "object", "additionalProperties", false,
			"properties", schemaObj("blastRadius", schemaObj("type", "string")),
			"required", []any{"blastRadius"}),
	}
	for k, v := range extra {
		defs[k] = v
	}
	return defs
}

func TestRound5_G1_InlineDiscriminatedMemberOnShapeBFailsClosed(t *testing.T) {
	for name, member := range inlineDiscriminatedVariants() {
		t.Run(name, func(t *testing.T) {
			baseDefs := reviewReadoutLikeDefs(nil, nil)
			headDefs := reviewReadoutLikeDefs(nil, member)
			_, err := DiffDef(baseDefs, headDefs, "Wrapper", DirP2C, nil)
			fc, ok := err.(*FailClosedError)
			if !ok {
				t.Fatalf("want *FailClosedError for an inline discriminated member (type variant %q) added to a shape-B anyOf, got err=%v", name, err)
			}
			if fc.Finding.RuleID != "fc-oneof-unpairable" {
				t.Fatalf("want fc-oneof-unpairable, got %s", fc.Finding.RuleID)
			}
		})
	}
}

// --- G2: a union whose members' discriminator values are not all
// distinct never matches shape A at all ("FAIL-CLOSED for the
// non-distinct head") -- whether the collision arrives via a second
// $ref or an inline member reusing an EXISTING $ref member's own value.
// ---

func TestRound5_G2_DuplicateDiscriminatorViaRefFailsClosed(t *testing.T) {
	// PromptLite carries the SAME discriminator ("prompt") as the
	// already-present "Prompt" -- the exact G2 repro against the real
	// sandbox-ws/v1/commands.schema.json.
	extraDefs := map[string]any{
		"PromptLite": schemaObj("type", "object", "additionalProperties", false,
			"properties", schemaObj("type", schemaObj("type", "string", "const", "prompt"), "zz", schemaObj("type", "integer")),
			"required", []any{"type", "zz"}),
	}
	base := commandsLikeRoot(nil, nil)
	head := commandsLikeRoot(extraDefs, schemaObj("$ref", "#/$defs/PromptLite"))
	findings, err := DiffSurface(string(DirP2C), mustMarshal(base), mustMarshal(head), nil)
	if err != nil {
		t.Fatalf("DiffSurface: unexpected hard error: %v", err)
	}
	if !containsFinding(findings, "fc-oneof-unpairable", SeverityFailClosed) {
		t.Fatalf("a second $ref member reusing an EXISTING member's own discriminator value must fail closed (non-distinct head), got: %+v", findings)
	}
	if containsFinding(findings, "28", SeverityMinor) {
		t.Fatalf("must NOT be silently accepted as row 28 MINOR, got: %+v", findings)
	}
	if containsFinding(findings, "32", SeverityMinor) && !containsFinding(findings, "fc-oneof-unpairable", SeverityFailClosed) {
		t.Fatalf("a MINOR $defs-added finding alone (without the fail-closed union finding) would mean the collision was silently accepted, got: %+v", findings)
	}
}

func TestRound5_G2_DuplicateDiscriminatorViaInlineFailsClosed(t *testing.T) {
	// The degenerate form of the same collision: the new member is
	// spelled INLINE instead of via a second $ref. G1 alone already
	// fails this closed (an inline member never resolves to kindObject),
	// but pin it under G2's own name too, since it is the exact
	// "duplicate discriminator via $ref and inline" corpus case the
	// review asked for.
	inlineDup := schemaObj("type", "object", "additionalProperties", false,
		"properties", schemaObj("type", schemaObj("type", "string", "const", "prompt"), "zz", schemaObj("type", "integer")),
		"required", []any{"type", "zz"})
	base := commandsLikeRoot(nil, nil)
	head := commandsLikeRoot(nil, inlineDup)
	findings, err := DiffSurface(string(DirP2C), mustMarshal(base), mustMarshal(head), nil)
	if err != nil {
		t.Fatalf("DiffSurface: unexpected hard error: %v", err)
	}
	if !containsFinding(findings, "fc-oneof-unpairable", SeverityFailClosed) {
		t.Fatalf("an inline member reusing an EXISTING $ref member's own discriminator value must fail closed, got: %+v", findings)
	}
}

// TestRound5_G2_AddedVariantReusingBaseDiscriminatorIsMajor is the
// DiffSurface-level (root oneOf, both P2C and the events.schema.json-like
// "both" direction) mirror of compat_test.go's "row46" corpus case: the
// OLD member owning a discriminator value is removed in the SAME diff a
// NEW member reusing that exact value is added, under a different $ref
// name. Because "Prompt" (the old owner) disappears, head's own
// discriminator set stays internally distinct -- this is NOT the
// non-distinct-head case above, so it must reach diffUnionShapeA's own
// row-46 check, not merely fail shape classification.
func TestRound5_G2_AddedVariantReusingBaseDiscriminatorIsMajor(t *testing.T) {
	base := commandsLikeRoot(nil, nil)
	headDefs := map[string]any{
		"Stop": schemaObj("type", "object", "additionalProperties", false,
			"properties", schemaObj("type", schemaObj("type", "string", "const", "stop")),
			"required", []any{"type"}),
		// PromptV2 replaces "Prompt" under a new $defs name, keeping the
		// exact same wire discriminator "prompt" but a DIFFERENT required
		// set -- an in-flight consumer still dispatching "prompt" to the
		// OLD Prompt struct would reject this payload.
		"PromptV2": schemaObj("type", "object", "additionalProperties", false,
			"properties", schemaObj("type", schemaObj("type", "string", "const", "prompt"), "effort", schemaObj("type", "string")),
			"required", []any{"type", "effort"}),
	}
	head := schemaObj(
		"$schema", "https://json-schema.org/draft/2020-12/schema",
		"$id", "https://narvi.dev/t/v1/commands.schema.json",
		"title", "Commands",
		"description", "test fixture mirroring sandbox-ws/v1/commands.schema.json's own root oneOf shape",
		"$defs", headDefs,
		"oneOf", []any{schemaObj("$ref", "#/$defs/PromptV2"), schemaObj("$ref", "#/$defs/Stop")},
	)

	findings, err := DiffSurface(string(DirBoth), mustMarshal(base), mustMarshal(head), nil)
	if err != nil {
		t.Fatalf("DiffSurface: unexpected error: %v", err)
	}
	if !containsFinding(findings, "46", SeverityMajor) {
		t.Fatalf("a variant renamed (Prompt -> PromptV2) while reusing the wire discriminator \"prompt\" must be row 46 MAJOR, got: %+v", findings)
	}
	if containsFinding(findings, "28", SeverityMinor) {
		t.Fatalf("must NOT ALSO be graded as an unrelated row 28 MINOR \"variant added\", got: %+v", findings)
	}
}

// --- Legitimate shape-A cases must stay green: a genuinely NEW,
// distinctly-discriminated variant added is still row 28 MINOR, even at
// the DiffSurface (root oneOf) level, not merely DiffDef's -- compat_test
// .go's own "row28 oneOf member added" case covers the DiffDef path. ---

func TestRound5_LegitimateDistinctVariantAddedStaysMinor(t *testing.T) {
	base := commandsLikeRoot(nil, nil)
	newMember := schemaObj("$ref", "#/$defs/Shutdown")
	newDef := map[string]any{
		"Shutdown": schemaObj("type", "object", "additionalProperties", false,
			"properties", schemaObj("type", schemaObj("type", "string", "const", "shutdown")),
			"required", []any{"type"}),
	}
	head := commandsLikeRoot(newDef, newMember)

	findings, err := DiffSurface(string(DirP2C), mustMarshal(base), mustMarshal(head), nil)
	if err != nil {
		t.Fatalf("DiffSurface: unexpected error: %v", err)
	}
	if !containsFinding(findings, "28", SeverityMinor) {
		t.Fatalf("a genuinely new, distinctly-discriminated variant added must still be row 28 MINOR, got: %+v", findings)
	}
	for _, f := range findings {
		if f.Severity == SeverityFailClosed || f.Severity == SeverityMajor {
			t.Fatalf("a legitimate distinct-variant addition must not produce any MAJOR/FAIL-CLOSED finding, got: %+v", findings)
		}
	}
}

// --- Round 2's own D9 fixture (see round2_repro_test.go's
// TestRound2_D9_DiscriminatedVariantStillRow28) used to pair TWO
// undiscriminated bare {"type":"object"} members -- legal before round
// 5, since pairing was by $ref NAME alone. Round 5 requires shape A's
// EVERY member to carry a discriminator, and shape B allows only ONE
// object slot -- two undiscriminated object members matches neither. ---

func TestRound5_G1_UndiscriminatedSecondObjectMemberFailsClosed(t *testing.T) {
	baseDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A")}),
		"A":        schemaObj("type", "object"),
		"B":        schemaObj("type", "object"),
	}
	headDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A"), schemaObj("$ref", "#/$defs/B")}),
		"A":        schemaObj("type", "object"),
		"B":        schemaObj("type", "object"),
	}
	_, err := DiffDef(baseDefs, headDefs, "Envelope", DirP2C, nil)
	fc, ok := err.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for a second undiscriminated object member (matches neither shape A nor shape B), got err=%v", err)
	}
	if fc.Finding.RuleID != "fc-oneof-unpairable" {
		t.Fatalf("want fc-oneof-unpairable, got %s", fc.Finding.RuleID)
	}
}

// --- Shape B's own single object slot, when retargeted to a different
// $ref, is row 27 -- NOT rows 28/29's remove+add, since shape B never
// has more than one object variant to pair by name. ---

func TestRound5_ShapeB_ObjectMemberRetargetIsRow27(t *testing.T) {
	baseDefs := map[string]any{
		"Wrapper": schemaObj("anyOf", []any{schemaObj("$ref", "#/$defs/Verdict"), schemaObj("type", "null")}),
		"Verdict": schemaObj("type", "object", "properties", schemaObj("blastRadius", schemaObj("type", "string")), "required", []any{"blastRadius"}),
	}
	headDefs := map[string]any{
		"Wrapper": schemaObj("anyOf", []any{schemaObj("$ref", "#/$defs/VerdictV2"), schemaObj("type", "null")}),
		"Verdict": schemaObj("type", "object", "properties", schemaObj("blastRadius", schemaObj("type", "string")), "required", []any{"blastRadius"}),
		// VerdictV2 additionally requires "summary" -- a real break on the
		// strict (C2P) column, so the retarget's own recursion is visibly
		// exercised, not merely reported as a no-op wrapper.
		"VerdictV2": schemaObj("type", "object", "properties", schemaObj(
			"blastRadius", schemaObj("type", "string"),
			"summary", schemaObj("type", "string"),
		), "required", []any{"blastRadius", "summary"}),
	}
	findings, err := DiffDef(baseDefs, headDefs, "Wrapper", DirC2P, nil)
	if err != nil {
		t.Fatalf("DiffDef: %v", err)
	}
	if !containsFinding(findings, "27", SeverityMajor) {
		t.Fatalf("shape B's object slot retargeted to a def with a new required property must be row 27 MAJOR (C2P), got: %+v", findings)
	}
	if containsFinding(findings, "28", SeverityMinor) || containsFinding(findings, "29", SeverityMajor) {
		t.Fatalf("must NOT be graded as rows 28/29's remove+add -- shape B has only one object slot, never several named variants, got: %+v", findings)
	}
}

// --- A union that matches shape A on one side and shape B on the other
// (or a permitted shape on one side and nothing on the other) fails
// closed -- this checker does not model a transition between the two. ---

func TestRound5_UnionShapeChangedFailsClosed(t *testing.T) {
	// Base: shape A (two distinctly-discriminated $ref members, no
	// null). Head: shape B (one object member plus a null) -- the SAME
	// array position now describes a completely different kind of union.
	baseDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A"), schemaObj("$ref", "#/$defs/B")}),
		"A":        schemaObj("type", "object", "properties", schemaObj("type", schemaObj("const", "a")), "required", []any{"type"}),
		"B":        schemaObj("type", "object", "properties", schemaObj("type", schemaObj("const", "b")), "required", []any{"type"}),
	}
	headDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A"), schemaObj("type", "null")}),
		"A":        schemaObj("type", "object", "properties", schemaObj("type", schemaObj("const", "a")), "required", []any{"type"}),
		"B":        schemaObj("type", "object", "properties", schemaObj("type", schemaObj("const", "b")), "required", []any{"type"}),
	}
	_, err := DiffDef(baseDefs, headDefs, "Envelope", DirP2C, nil)
	fc, ok := err.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for a union changing from shape A to shape B, got err=%v", err)
	}
	if fc.Finding.RuleID != "fc-oneof-unpairable" {
		t.Fatalf("want fc-oneof-unpairable, got %s", fc.Finding.RuleID)
	}
}
