package compat

import (
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

// This file pins the round-2 adversarial review's findings against PR
// #320 (see the PR body's "Review round 2" section, and D1-D23 in the
// review). Round 1 fixed "a $ref sibling is silently dropped" by MODELING
// draft 2020-12's $ref-plus-siblings conjunction (an override/merge in
// ref.go's old effectiveNode). Round 2 found that model itself kept being
// wrong in a new way every time it was checked from a different angle.
// The fix this round is not another model: it is to make any $ref sibling
// other than "description" illegal outright (ref.go's
// refAllowedSiblingKeys) -- every one of D1-D5, D7, D8, D10, D11, D16, and
// D21 collapses into that one rule once it is enforced consistently.

// --- $ref-sibling family: D1, D3, D4, D5, D7 (siblings modeled as an
// override/merge, now illegal instead) ---

// D1/D7: retargeting through an alias def that is itself {"$ref":
// X, "required": [...]} used to drop the alias's own "required" sibling
// silently (resolver.resolve followed the chain and threw it away). Now
// the alias def itself is illegal: a $defs entry that is a bare $ref may
// carry nothing but "description" beside it.
func TestRound2_D1_D7_AliasDefWithRequiredSiblingFailsClosed(t *testing.T) {
	baseDefs := map[string]any{
		"Wrapper": schemaObj("type", "object", "properties", schemaObj("digest", schemaObj("$ref", "#/$defs/Digest"))),
		"Digest":  schemaObj("type", "object", "properties", schemaObj("archDecisions", schemaObj("type", "string"))),
	}
	headDefs := map[string]any{
		"Wrapper":      schemaObj("type", "object", "properties", schemaObj("digest", schemaObj("$ref", "#/$defs/DigestStrict"))),
		"Digest":       schemaObj("type", "object", "properties", schemaObj("archDecisions", schemaObj("type", "string"))),
		"DigestStrict": schemaObj("$ref", "#/$defs/Digest", "required", []any{"archDecisions"}),
	}
	_, err := DiffDef(baseDefs, headDefs, "Wrapper", DirC2P, nil)
	fc, ok := err.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for an alias def carrying a \"required\" sibling, got err=%v", err)
	}
	if fc.Finding.Severity != SeverityFailClosed {
		t.Fatalf("want SeverityFailClosed, got %v", fc.Finding.Severity)
	}
}

// D3/D5: a sibling "additionalProperties" beside $ref used to be merged
// as an override (letting a sibling "false" that happens to equal the
// target's own "false" vanish into a no-op, or a sibling schema value
// score as a mere "loosened" MINOR) instead of failing closed.
func TestRound2_D3_D5_AdditionalPropertiesSiblingFailsClosed(t *testing.T) {
	baseDefs := map[string]any{
		"Wrapper": schemaObj("type", "object", "properties", schemaObj("digest", schemaObj("$ref", "#/$defs/Digest"))),
		"Digest":  schemaObj("type", "object", "additionalProperties", false, "properties", schemaObj("archDecisions", schemaObj("type", "string"))),
	}
	headDefs := map[string]any{
		"Wrapper": schemaObj("type", "object", "properties", schemaObj("digest", schemaObj("$ref", "#/$defs/Digest", "additionalProperties", false))),
		"Digest":  schemaObj("type", "object", "additionalProperties", false, "properties", schemaObj("archDecisions", schemaObj("type", "string"))),
	}
	_, err := DiffDef(baseDefs, headDefs, "Wrapper", DirC2P, nil)
	fc, ok := err.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for an additionalProperties sibling beside $ref, got err=%v", err)
	}
	if fc.Finding.Severity != SeverityFailClosed {
		t.Fatalf("want SeverityFailClosed, got %v", fc.Finding.Severity)
	}
}

// D4: unionPropertyMaps used to let a sibling "properties" entry SHADOW
// the retargeted-to def's own stricter property, on a name collision.
// Both the same-name-ref and the retargeted-ref forms are now illegal the
// same way.
func TestRound2_D4_PropertiesSiblingOnRetargetFailsClosed(t *testing.T) {
	baseDefs := map[string]any{
		"Wrapper": schemaObj("type", "object", "properties", schemaObj("digest", schemaObj("$ref", "#/$defs/Digest"))),
		"Digest":  schemaObj("type", "object", "properties", schemaObj("summary", schemaObj("type", "string", "minLength", float64(1)))),
	}
	headDefs := map[string]any{
		"Wrapper": schemaObj("type", "object", "properties", schemaObj(
			"digest", schemaObj("$ref", "#/$defs/DigestV2", "properties", schemaObj("summary", schemaObj("type", "string"))),
		)),
		"Digest":   schemaObj("type", "object", "properties", schemaObj("summary", schemaObj("type", "string", "minLength", float64(1)))),
		"DigestV2": schemaObj("type", "object", "properties", schemaObj("summary", schemaObj("type", "string", "enum", []any{"only-this-value-now"}))),
	}
	_, err := DiffDef(baseDefs, headDefs, "Wrapper", DirC2P, nil)
	fc, ok := err.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for a properties sibling on a retargeted $ref, got err=%v", err)
	}
	if fc.Finding.Severity != SeverityFailClosed {
		t.Fatalf("want SeverityFailClosed, got %v", fc.Finding.Severity)
	}
}

// D21: a sibling "type" on a retargeted $ref used to shadow the new
// target's own narrower type (effectiveNode's override), hiding a real
// P2C null-added break (row 9) behind a PATCH-severity row-27 wrapper.
func TestRound2_D21_TypeSiblingOnRetargetFailsClosed(t *testing.T) {
	baseDefs := map[string]any{
		"WidgetResponse": schemaObj("type", "object", "properties", schemaObj(
			"title", schemaObj("$ref", "#/$defs/WidgetTitle", "type", []any{"string", "null"}),
		)),
		"WidgetTitle":         schemaObj("type", "string"),
		"NullableWidgetTitle": schemaObj("type", []any{"string", "null"}),
	}
	headDefs := map[string]any{
		"WidgetResponse": schemaObj("type", "object", "properties", schemaObj(
			"title", schemaObj("$ref", "#/$defs/NullableWidgetTitle", "type", []any{"string", "null"}),
		)),
		"WidgetTitle":         schemaObj("type", "string"),
		"NullableWidgetTitle": schemaObj("type", []any{"string", "null"}),
	}
	_, err := DiffDef(baseDefs, headDefs, "WidgetResponse", DirP2C, nil)
	fc, ok := err.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for a type sibling on a retargeted $ref, got err=%v", err)
	}
	if fc.Finding.Severity != SeverityFailClosed {
		t.Fatalf("want SeverityFailClosed, got %v", fc.Finding.Severity)
	}
}

// --- reachability family: D2, D8, D11 (collectRefs stopped at $ref
// without walking its siblings, so a def reached only through a $ref
// sibling kept the wrong direction) -- now moot: the sibling itself is
// illegal, caught before reachability ever runs. ---

func TestRound2_D2_D8_D11_PropertiesSiblingBesideRefFailsClosed(t *testing.T) {
	baseDefs := map[string]any{
		"PostThingRequest": schemaObj("type", "object", "properties", schemaObj("digest", schemaObj("$ref", "#/$defs/Digest"))),
		"Digest":           schemaObj("type", "object", "properties", schemaObj("archDecisions", schemaObj("type", "string"))),
	}
	headDefs := map[string]any{
		"PostThingRequest": schemaObj("type", "object", "properties", schemaObj(
			"digest", schemaObj("$ref", "#/$defs/Digest", "properties", schemaObj("extra", schemaObj("$ref", "#/$defs/ApplySuggestionResponse"))),
		)),
		"Digest":                  schemaObj("type", "object", "properties", schemaObj("archDecisions", schemaObj("type", "string"))),
		"ApplySuggestionResponse": schemaObj("type", "object"),
	}
	_, err := DiffDef(baseDefs, headDefs, "PostThingRequest", DirC2P, nil)
	fc, ok := err.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for a properties sibling beside $ref, got err=%v", err)
	}
	if fc.Finding.Severity != SeverityFailClosed {
		t.Fatalf("want SeverityFailClosed, got %v", fc.Finding.Severity)
	}
}

// --- boolean-target family: D10, D16 ---

func TestRound2_D10_D16_RefToBooleanDefFailsClosed(t *testing.T) {
	baseDefs := map[string]any{
		"CreateWidgetRequest": schemaObj("type", "object", "properties", schemaObj("payload", schemaObj("$ref", "#/$defs/AnyJSON"))),
		"AnyJSON":             true,
	}
	headDefs := map[string]any{
		"CreateWidgetRequest": schemaObj("type", "object", "properties", schemaObj("payload", schemaObj("$ref", "#/$defs/AnyJSON", "description", "now documented"))),
		"AnyJSON":             true,
	}
	_, err := DiffDef(baseDefs, headDefs, "CreateWidgetRequest", DirC2P, nil)
	fc, ok := err.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for a $ref to a boolean-schema def, got err=%v", err)
	}
	if fc.Finding.Severity != SeverityFailClosed {
		t.Fatalf("want SeverityFailClosed, got %v", fc.Finding.Severity)
	}
}

// D10/D16, retargeted form: retargeting FROM a boolean def must also fail
// closed, not just referencing one directly (diffRetargetedRef's own
// resolveDef calls carry the same boolean-target rejection ref.go's
// resolve does).
func TestRound2_D10_D16_RetargetFromBooleanDefFailsClosed(t *testing.T) {
	baseDefs := map[string]any{
		"CreateWidgetRequest": schemaObj("type", "object", "properties", schemaObj("payload", schemaObj("$ref", "#/$defs/AnyJSON"))),
		"AnyJSON":             true,
		"StrictPayload":       schemaObj("type", "object"),
	}
	headDefs := map[string]any{
		"CreateWidgetRequest": schemaObj("type", "object", "properties", schemaObj("payload", schemaObj("$ref", "#/$defs/StrictPayload"))),
		"AnyJSON":             true,
		"StrictPayload":       schemaObj("type", "object"),
	}
	_, err := DiffDef(baseDefs, headDefs, "CreateWidgetRequest", DirC2P, nil)
	fc, ok := err.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for retargeting away from a $ref to a boolean-schema def, got err=%v", err)
	}
	if fc.Finding.Severity != SeverityFailClosed {
		t.Fatalf("want SeverityFailClosed, got %v", fc.Finding.Severity)
	}
}

// --- positive cases: the general rule doesn't over-reach ---

// A $defs alias entry that is EXACTLY {"$ref": ..., "description"?: ...}
// is legal and resolves as a pure alias -- following it all the way
// through to the real target's content, with no constraints lost or
// gained along the way.
func TestRound2_PureAliasDefResolvesCorrectly(t *testing.T) {
	// DiffDef is called directly on the ALIAS def itself, matching how
	// DiffSurface's own per-def loop would reach it: comparing "Digest"
	// directly is a separate, independent DiffDef call, catching the
	// change under Digest's own reachability-derived direction; this test
	// is specifically about the alias def's OWN diff (base/head raw
	// "DigestAlias" content is identical -- {"$ref":"Digest",
	// "description":...} on both sides -- so this only produces a finding
	// if resolving THROUGH the alias to Digest's real, changed content
	// works correctly).
	baseDefs := map[string]any{
		"DigestAlias": schemaObj("$ref", "#/$defs/Digest", "description", "an alias for Digest"),
		"Digest":      schemaObj("type", "object", "properties", schemaObj("summary", schemaObj("type", "string"))),
	}
	headDefs := map[string]any{
		"DigestAlias": schemaObj("$ref", "#/$defs/Digest", "description", "an alias for Digest"),
		// Digest's own required list gains a name -- must be visible
		// THROUGH the alias, not swallowed by it.
		"Digest": schemaObj("type", "object", "properties", schemaObj("summary", schemaObj("type", "string")), "required", []any{"summary"}),
	}
	findings, err := DiffDef(baseDefs, headDefs, "DigestAlias", DirC2P, nil)
	if err != nil {
		t.Fatalf("DiffDef: unexpected error resolving a pure alias def: %v", err)
	}
	if !containsFinding(findings, "4", SeverityMajor) {
		t.Fatalf("a required name added behind a pure alias def must still be classified (row 4, MAJOR on C2P), got: %+v", findings)
	}
}

// A plain retarget to a genuinely different def (no illegal siblings on
// either side) is still classified normally -- the fail-closed rule above
// targets the SIBLING shape, not retargeting itself.
func TestRound2_PlainRetargetToDifferentDefStillMajor(t *testing.T) {
	baseDefs := map[string]any{
		"Wrapper":      schemaObj("type", "object", "properties", schemaObj("digest", schemaObj("$ref", "#/$defs/Digest"))),
		"Digest":       schemaObj("type", "object", "properties", schemaObj("summary", schemaObj("type", "string"))),
		"DigestStrict": schemaObj("type", "object", "properties", schemaObj("summary", schemaObj("type", "string")), "required", []any{"summary"}),
	}
	headDefs := map[string]any{
		"Wrapper":      schemaObj("type", "object", "properties", schemaObj("digest", schemaObj("$ref", "#/$defs/DigestStrict"))),
		"Digest":       schemaObj("type", "object", "properties", schemaObj("summary", schemaObj("type", "string"))),
		"DigestStrict": schemaObj("type", "object", "properties", schemaObj("summary", schemaObj("type", "string")), "required", []any{"summary"}),
	}
	findings, err := DiffDef(baseDefs, headDefs, "Wrapper", DirC2P, nil)
	if err != nil {
		t.Fatalf("DiffDef: unexpected error: %v", err)
	}
	if !containsFinding(findings, "27", SeverityMajor) {
		t.Fatalf("retargeting to a def with a genuinely stricter required set must be MAJOR (row 27), got: %+v", findings)
	}
}

// The one legal $ref sibling, "description," must still be diffed --
// dropping effectiveNode's merge machinery must not make a real change to
// this sibling invisible. Both the same-name and the retargeted case.
func TestRound2_RefSiblingDescriptionIsDiffed(t *testing.T) {
	t.Run("same target", func(t *testing.T) {
		baseDefs := map[string]any{
			"Wrapper": schemaObj("type", "object", "properties", schemaObj("digest", schemaObj("$ref", "#/$defs/Digest", "description", "old"))),
			"Digest":  schemaObj("type", "string"),
		}
		headDefs := map[string]any{
			"Wrapper": schemaObj("type", "object", "properties", schemaObj("digest", schemaObj("$ref", "#/$defs/Digest", "description", "new"))),
			"Digest":  schemaObj("type", "string"),
		}
		findings, err := DiffDef(baseDefs, headDefs, "Wrapper", DirC2P, nil)
		if err != nil {
			t.Fatalf("DiffDef: %v", err)
		}
		if !containsFinding(findings, "35", SeverityPatch) {
			t.Fatalf("a description sibling change beside an unchanged $ref must be classified (row 35, PATCH), got: %+v", findings)
		}
	})

	t.Run("retargeted", func(t *testing.T) {
		baseDefs := map[string]any{
			"Wrapper":  schemaObj("type", "object", "properties", schemaObj("digest", schemaObj("$ref", "#/$defs/Digest", "description", "old"))),
			"Digest":   schemaObj("type", "string"),
			"DigestV2": schemaObj("type", "string"),
		}
		headDefs := map[string]any{
			"Wrapper":  schemaObj("type", "object", "properties", schemaObj("digest", schemaObj("$ref", "#/$defs/DigestV2", "description", "new"))),
			"Digest":   schemaObj("type", "string"),
			"DigestV2": schemaObj("type", "string"),
		}
		findings, err := DiffDef(baseDefs, headDefs, "Wrapper", DirC2P, nil)
		if err != nil {
			t.Fatalf("DiffDef: %v", err)
		}
		if !containsFinding(findings, "35", SeverityPatch) {
			t.Fatalf("a description sibling change on a retargeted $ref must still be classified (row 35, PATCH), got: %+v", findings)
		}
		if !containsFinding(findings, "27", SeverityPatch) {
			t.Fatalf("the retarget itself (content-identical targets) must still be row 27 PATCH, got: %+v", findings)
		}
	})

	t.Run("root", func(t *testing.T) {
		base := mustMarshal(schemaObj(
			"$schema", "https://json-schema.org/draft/2020-12/schema",
			"$id", "https://narvi.dev/t/v1/x.schema.json",
			"title", "T",
			"description", "old root description",
			"$ref", "#/$defs/Config",
			"$defs", defsOf("Config", schemaObj("type", "object")),
		))
		head := mustMarshal(schemaObj(
			"$schema", "https://json-schema.org/draft/2020-12/schema",
			"$id", "https://narvi.dev/t/v1/x.schema.json",
			"title", "T",
			"description", "new root description",
			"$ref", "#/$defs/Config",
			"$defs", defsOf("Config", schemaObj("type", "object")),
		))
		findings, err := DiffSurface(string(DirP2C), base, head, nil)
		if err != nil {
			t.Fatalf("DiffSurface: %v", err)
		}
		if !containsFinding(findings, "35", SeverityPatch) {
			t.Fatalf("a root description change beside the root's own $ref must be classified (row 35, PATCH), got: %+v", findings)
		}
	})
}

// --- D9: union scalar-type branches used to be graded as type
// widening/narrowing (rows 7/8), not discriminated-variant add/remove
// (rows 28/29) -- SUPERSEDED by the round-5 review (G1/G2): rows 7/8's
// own union-member reading modeled a general shape ("a bare scalar
// {"type":X} member") no real file ever uses, which is exactly the kind
// of general-shape guess three straight review rounds (E1, F2, G1/G2)
// kept finding a bypass in. Round 5 replaced that modeling with a
// whitelist of the two union shapes the real /contracts files actually
// use (defdiff.go's own union-section doc comment) -- a bare scalar
// union member, {"type":"boolean"}/{"type":"string"} here, matches
// neither, so both the base and the head document in these two fixtures
// now fail closed instead of reaching rows 7/8 at all. That is at least
// as safe as the graded outcome these tests used to pin (FAIL-CLOSED
// still blocks CI), so D9's original point -- a scalar union member must
// not be silently absorbed as a MINOR "variant added"/"removed" -- still
// holds; it is just enforced one step earlier now.
// ---

func TestRound2_D9_ScalarUnionMemberAddedNowFailsClosed(t *testing.T) {
	baseDefs := defsOf("ContractsVersion", schemaObj("anyOf", []any{schemaObj("type", "boolean")}))
	headDefs := defsOf("ContractsVersion", schemaObj("anyOf", []any{schemaObj("type", "boolean"), schemaObj("type", "string")}))
	_, err := DiffDef(baseDefs, headDefs, "ContractsVersion", DirP2C, nil)
	fc, ok := err.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for a bare scalar-type anyOf (matches neither permitted union shape), got err=%v", err)
	}
	if fc.Finding.RuleID != "fc-oneof-unpairable" {
		t.Fatalf("want fc-oneof-unpairable, got %s", fc.Finding.RuleID)
	}
}

func TestRound2_D9_ScalarUnionMemberRemovedNowFailsClosed(t *testing.T) {
	baseDefs := defsOf("ContractsVersion", schemaObj("anyOf", []any{schemaObj("type", "boolean"), schemaObj("type", "string")}))
	headDefs := defsOf("ContractsVersion", schemaObj("anyOf", []any{schemaObj("type", "boolean")}))
	_, err := DiffDef(baseDefs, headDefs, "ContractsVersion", DirP2C, nil)
	fc, ok := err.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for a bare scalar-type anyOf (matches neither permitted union shape), got err=%v", err)
	}
	if fc.Finding.RuleID != "fc-oneof-unpairable" {
		t.Fatalf("want fc-oneof-unpairable, got %s", fc.Finding.RuleID)
	}
}

// Discriminated object variants ($ref to a pure object def carrying its
// own properties.type.const, distinct from every other member's -- round
// 5's shape A, see defdiff.go's own union-section doc comment) still go
// through rows 28/29 as before -- D9's fix must not over-reach into that
// bucket. Round 5: A/B must actually carry a discriminator for this to be
// shape A at all -- an undiscriminated $ref to a bare {"type":"object"}
// def (this fixture's own shape before round 5) is shape A only when it
// is the ONLY member; a SECOND undiscriminated object member does not
// match shape A (nothing to distinguish it by) or shape B (more than one
// object slot), so it now fails closed instead -- see
// TestRound5_G1_UndiscriminatedSecondObjectMemberFailsClosed.
func TestRound2_D9_DiscriminatedVariantStillRow28(t *testing.T) {
	baseDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A")}),
		"A": schemaObj("type", "object",
			"properties", schemaObj("type", schemaObj("const", "a")),
			"required", []any{"type"},
		),
		"B": schemaObj("type", "object",
			"properties", schemaObj("type", schemaObj("const", "b")),
			"required", []any{"type"},
		),
	}
	headDefs := map[string]any{
		"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A"), schemaObj("$ref", "#/$defs/B")}),
		"A": schemaObj("type", "object",
			"properties", schemaObj("type", schemaObj("const", "a")),
			"required", []any{"type"},
		),
		"B": schemaObj("type", "object",
			"properties", schemaObj("type", schemaObj("const", "b")),
			"required", []any{"type"},
		),
	}
	findings, err := DiffDef(baseDefs, headDefs, "Envelope", DirP2C, nil)
	if err != nil {
		t.Fatalf("DiffDef: %v", err)
	}
	if !containsFinding(findings, "28", SeverityMinor) {
		t.Fatalf("a discriminated ($ref) object variant added must stay row 28 MINOR, got: %+v", findings)
	}
}

// --- D6/D12: deleting a retired file's surface requires a MAJOR bump and
// a CHANGELOG subsection, exactly as COMPATIBILITY.md says ---

func TestRound2_D6_D12_RetiredSurfaceDeletionRequiresMajorBumpAndSubsection(t *testing.T) {
	retiredFile := "session-config/v1/session-config.schema.json"
	keptFile := "rest/v1/dtos.schema.json"
	retiredDoc := mustMarshal(minimalFile("https://narvi.dev/"+retiredFile, defsOf("Widget", schemaObj("type", "string"))))
	keptDoc := mustMarshal(minimalFile("https://narvi.dev/"+keptFile, defsOf("Other", schemaObj("type", "string"))))

	baseManifest := mustMarshal(map[string]any{
		"version": "1.0.0",
		"surfaces": []any{
			map[string]any{"path": retiredFile, "direction": string(DirP2C), "status": StatusRetired},
			map[string]any{"path": keptFile, "direction": DirectiveBySuffix, "status": StatusCurrent},
		},
	})
	headManifestDeleted := func(version string) []byte {
		return mustMarshal(map[string]any{
			"version": version,
			"surfaces": []any{
				map[string]any{"path": keptFile, "direction": DirectiveBySuffix, "status": StatusCurrent},
			},
		})
	}

	t.Run("PATCH bump, no subsection: must fail", func(t *testing.T) {
		report, err := Compare(Input{
			BaseManifestRaw:  baseManifest,
			HeadManifestRaw:  headManifestDeleted("1.0.1"),
			BaseVersion:      "1.0.0",
			HeadVersion:      "1.0.1", // PATCH only
			BaseChangelogRaw: []byte("## [1.0.0]\n"),
			HeadChangelogRaw: []byte("## [1.0.1]\n- deleted a retired surface\n\n## [1.0.0]\n"), // no "### <path>" subsection
			BaseRoutes:       []byte(""),
			HeadRoutes:       []byte(""),
			BaseSchemaFiles:  map[string][]byte{retiredFile: retiredDoc, keptFile: keptDoc},
			HeadSchemaFiles:  map[string][]byte{keptFile: keptDoc},
		})
		if err != nil {
			t.Fatalf("Compare: %v", err)
		}
		if !containsFinding(report.Findings, "version-changelog", SeverityMajor) {
			t.Fatalf("deleting a retired surface with only a PATCH bump and no CHANGELOG subsection must fail the version-changelog rule, got: %+v", report.Findings)
		}
		if !report.HasBreaking() {
			t.Fatal("report must be breaking")
		}
	})

	t.Run("MAJOR bump with subsection: must pass", func(t *testing.T) {
		report, err := Compare(Input{
			BaseManifestRaw:  baseManifest,
			HeadManifestRaw:  headManifestDeleted("2.0.0"),
			BaseVersion:      "1.0.0",
			HeadVersion:      "2.0.0",
			BaseChangelogRaw: []byte("## [1.0.0]\n"),
			HeadChangelogRaw: []byte("## [2.0.0]\n### " + retiredFile + "\n- Removed: retired surface deleted (test fixture)\n\n## [1.0.0]\n"),
			BaseRoutes:       []byte(""),
			HeadRoutes:       []byte(""),
			BaseSchemaFiles:  map[string][]byte{retiredFile: retiredDoc, keptFile: keptDoc},
			HeadSchemaFiles:  map[string][]byte{keptFile: keptDoc},
		})
		if err != nil {
			t.Fatalf("Compare: %v", err)
		}
		if containsFinding(report.Findings, "version-changelog", SeverityMajor) {
			t.Fatalf("a properly-disciplined retired-surface deletion (MAJOR bump, matching CHANGELOG subsection) must pass the version-changelog rule, got: %+v", report.Findings)
		}
	})
}

// --- D14: openEnums matches by exact pointer, not a name derived by
// stripping "properties"/"$defs" segments ---

func TestRound2_D14_PropertyNamedPropertiesDoesNotInheritUnrelatedOpenEnum(t *testing.T) {
	// A property literally named "properties" holding a CLOSED enum, at
	// #/$defs/Session/properties/properties/properties/status -- under
	// the old name-stripping canonicalName, this collapsed to
	// "Session.status", which happens to be a real day-one openEnums
	// entry for an unrelated field. The exact-pointer match must not
	// confuse the two.
	baseDefs := defsOf("Session", schemaObj(
		"type", "object",
		"properties", schemaObj(
			"properties", schemaObj(
				"type", "object",
				"properties", schemaObj("status", schemaObj("type", "string", "enum", []any{"a", "b"})),
			),
		),
	))
	headDefs := defsOf("Session", schemaObj(
		"type", "object",
		"properties", schemaObj(
			"properties", schemaObj(
				"type", "object",
				"properties", schemaObj("status", schemaObj("type", "string", "enum", []any{"a", "b", "c"})),
			),
		),
	))
	// openEnums lists the pointer for the UNRELATED top-level Session.status
	// enum (which does not exist in this fixture), not this nested one.
	openEnums := map[string]bool{"#/$defs/Session/properties/status": true}
	findings, err := DiffDef(baseDefs, headDefs, "Session", DirP2C, openEnums)
	if err != nil {
		t.Fatalf("DiffDef: %v", err)
	}
	if !containsFinding(findings, "11", SeverityMajor) {
		t.Fatalf("an enum value added under a property literally named \"properties\" must be graded as a CLOSED enum (row 11, MAJOR), not inherit the unrelated pointer's openEnums relaxation, got: %+v", findings)
	}
}

// --- D15/D20: enum values are compared by canonical typed JSON, not
// fmt.Sprint text ---

func TestRound2_D15_D20_EnumNullVsStringNilIsDetected(t *testing.T) {
	baseDefs := defsOf("LastRunStatus", schemaObj("type", []any{"string", "null"}, "enum", []any{"succeeded", "failed", nil}))
	headDefs := defsOf("LastRunStatus", schemaObj("type", []any{"string", "null"}, "enum", []any{"succeeded", "failed", "<nil>"}))
	findings, err := DiffDef(baseDefs, headDefs, "LastRunStatus", DirP2C, nil)
	if err != nil {
		t.Fatalf("DiffDef: %v", err)
	}
	if !containsFinding(findings, "11", SeverityMajor) {
		t.Fatalf("replacing enum value null with the STRING \"<nil>\" must be detected as an enum value added (row 11, MAJOR on a closed P2C enum), got: %+v", findings)
	}
	if !containsFinding(findings, "12", SeverityMinor) {
		t.Fatalf("...and null removed (row 12, MINOR on P2C), got: %+v", findings)
	}
}

func TestRound2_D15_D20_EnumNumberVsStringIsDetected(t *testing.T) {
	baseDefs := defsOf("Priority", schemaObj("type", []any{"string", "integer"}, "enum", []any{"1", "2"}))
	headDefs := defsOf("Priority", schemaObj("type", []any{"string", "integer"}, "enum", []any{float64(1), float64(2)}))
	findings, err := DiffDef(baseDefs, headDefs, "Priority", DirC2P, nil)
	if err != nil {
		t.Fatalf("DiffDef: %v", err)
	}
	// Row 11 (value added) is MINOR on C2P; row 12 (value removed) is
	// MAJOR on C2P -- an old client sending the string "1" is the one
	// that now gets rejected, which is row 12's severity, not row 11's.
	if !containsFinding(findings, "11", SeverityMinor) {
		t.Fatalf("adding the NUMBER 1/2 must be detected as enum values added (row 11 MINOR on C2P), got: %+v", findings)
	}
	if !containsFinding(findings, "12", SeverityMajor) {
		t.Fatalf("removing the STRING \"1\"/\"2\" must be detected as enum values removed (row 12 MAJOR on C2P -- an old client sending \"1\" is now rejected), got: %+v", findings)
	}
}

func TestRound2_D15_D20_EnumIdenticalTypedValuesStillNoFindings(t *testing.T) {
	// Sanity check: the canonical comparison must not FALSE-positive on
	// genuinely unchanged values of every JSON type enum can carry.
	baseDefs := defsOf("Mixed", schemaObj("enum", []any{"a", float64(1), true, nil, []any{"x"}, map[string]any{"k": "v"}}))
	headDefs := defsOf("Mixed", schemaObj("enum", []any{"a", float64(1), true, nil, []any{"x"}, map[string]any{"k": "v"}}))
	findings, err := DiffDef(baseDefs, headDefs, "Mixed", DirP2C, nil)
	if err != nil {
		t.Fatalf("DiffDef: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("an unchanged mixed-type enum must produce no findings, got: %+v", findings)
	}
}

// --- D17/D22: a self-referencing def must not recurse without bound ---

func TestRound2_D17_D22_SelfReferentialDefRecursionTerminates(t *testing.T) {
	planSection := func(childrenDescription string) map[string]any {
		return schemaObj(
			"type", "object",
			"properties", schemaObj(
				"title", schemaObj("type", "string"),
				"children", schemaObj(
					"type", "array",
					"description", childrenDescription,
					"items", schemaObj("$ref", "#/$defs/PlanSection"),
				),
			),
		)
	}
	baseDefs := defsOf("PlanSection", planSection("Child sections."))
	headDefs := defsOf("PlanSection", planSection("Nested plan sections.")) // changed, ON the recursive path

	type result struct {
		findings []Finding
		err      error
	}
	done := make(chan result, 1)
	// errgroup.Group.Go, not a bare `go` statement: §11's no-naked-
	// goroutine rule (tools/lint/narvichecks/nakedgoroutine) applies to
	// tests too. This local Group exists solely as a lint-satisfying Go()
	// call site, never Wait()ed on -- done, sent to from inside the
	// goroutine, is this test's own actual synchronization signal (mirrors
	// cmd/sandbox-agent/processgroup_test.go's own identical precedent).
	var group errgroup.Group
	group.Go(func() error {
		f, err := DiffDef(baseDefs, headDefs, "PlanSection", DirP2C, nil)
		done <- result{f, err}
		return nil
	})

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("DiffDef: unexpected error: %v", r.err)
		}
		count := 0
		for _, f := range r.findings {
			if f.RuleID == "35" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("want the recursive-path description change reported exactly once, got %d times: %+v", count, r.findings)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DiffDef did not terminate within 5s on a self-referencing def whose recursive path also differs -- unbounded recursion (D17/D22)")
	}
}

// A self-referencing def that has NOT changed at all must still short-
// circuit immediately via diffResolved's own DeepEqual check -- the
// recursion guard must not change that fast path.
func TestRound2_D17_D22_SelfReferentialDefUnchangedIsFast(t *testing.T) {
	planSection := schemaObj(
		"type", "object",
		"properties", schemaObj(
			"children", schemaObj("type", "array", "items", schemaObj("$ref", "#/$defs/PlanSection")),
		),
	)
	baseDefs := defsOf("PlanSection", planSection)
	headDefs := defsOf("PlanSection", planSection)

	start := time.Now()
	findings, err := DiffDef(baseDefs, headDefs, "PlanSection", DirP2C, nil)
	if err != nil {
		t.Fatalf("DiffDef: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("an unchanged self-referencing def must produce no findings, got: %+v", findings)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("an unchanged self-referencing def took %s, want near-instant (DeepEqual short-circuit)", elapsed)
	}
}

// --- D13(1): diffItems must recurse into a CONTENT change, not just a
// presence change ---

func TestRound2_D13_ItemsContentChangeRecursed(t *testing.T) {
	baseDefs := defsOf("Tags", schemaObj("type", "array", "items", schemaObj("type", "string")))
	headDefs := defsOf("Tags", schemaObj("type", "array", "items", schemaObj("type", "integer")))
	findings, err := DiffDef(baseDefs, headDefs, "Tags", DirP2C, nil)
	if err != nil {
		t.Fatalf("DiffDef: %v", err)
	}
	if !containsFinding(findings, "6", SeverityMajor) {
		t.Fatalf("items content changing type must be classified (row 6, MAJOR), got: %+v", findings)
	}
}

// --- D13(2): DirBoth must merge BOTH the P2C and C2P passes, not just
// return one of them ---

func TestRound2_D13_DirBothMergesBothColumns(t *testing.T) {
	// "cpuPct" moved into required: row 4 is MINOR on P2C, MAJOR on C2P.
	// mergeBothDirections keys by (ruleID, pointer) and keeps the WORSE
	// severity seen for that key -- picking a change whose C2P reading is
	// the worse one (unlike row 5, where P2C is already the worse column,
	// so a mutant that drops the c2p pass entirely would go unnoticed:
	// p2c alone already carries the max severity for that same key) is
	// what actually exercises "DirBoth must merge BOTH passes," not just
	// "DirBoth must return SOME finding."
	baseDefs := defsOf("Heartbeat", schemaObj(
		"type", "object", "properties", schemaObj("cpuPct", schemaObj("type", "number")), "required", []any{},
	))
	headDefs := defsOf("Heartbeat", schemaObj(
		"type", "object", "properties", schemaObj("cpuPct", schemaObj("type", "number")), "required", []any{"cpuPct"},
	))
	findings, err := DiffDef(baseDefs, headDefs, "Heartbeat", DirBoth, nil)
	if err != nil {
		t.Fatalf("DiffDef: %v", err)
	}
	if !containsFinding(findings, "4", SeverityMajor) {
		t.Fatalf("DirBoth must retain the C2P column's own MAJOR severity for a property moved into required (P2C alone would only give MINOR), got: %+v", findings)
	}
}

// --- D19(a): the exhaustiveness backstop actually fires ---

func TestRound2_D19a_ExhaustivenessBackstopFires(t *testing.T) {
	ctx := &diffCtx{baseR: resolver{defs: map[string]any{}}, headR: resolver{defs: map[string]any{}}}
	base := map[string]any{"type": "string", "zzz-not-a-real-keyword": true}
	head := map[string]any{"type": "string"}
	_, err := ctx.diffResolved(base, head, DirP2C, loc{ptr: "#/$defs/X", defPtr: "#/$defs/X"})
	fc, ok := err.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError from the exhaustiveness backstop, got err=%v", err)
	}
	if fc.Finding.RuleID != "fc-unhandled-keyword" {
		t.Fatalf("want fc-unhandled-keyword, got %s", fc.Finding.RuleID)
	}
}

// --- D19(b): a status-only manifest row change (e.g. current ->
// deprecated) still requires its own CHANGELOG subsection ---

func TestRound2_D19b_StatusOnlyChangeRequiresChangelogSubsection(t *testing.T) {
	schemaFile := "t/v1/x.schema.json"
	doc := mustMarshal(minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Widget", schemaObj("type", "string"))))
	baseManifest := mustMarshal(map[string]any{
		"version":  "1.0.0",
		"surfaces": []any{map[string]any{"path": schemaFile, "direction": DirectiveBySuffix, "status": StatusCurrent}},
	})
	headManifest := mustMarshal(map[string]any{
		"version":  "1.0.0",
		"surfaces": []any{map[string]any{"path": schemaFile, "direction": DirectiveBySuffix, "status": StatusDeprecated}},
	})
	report, err := Compare(Input{
		BaseManifestRaw:  baseManifest,
		HeadManifestRaw:  headManifest,
		BaseVersion:      "1.0.0",
		HeadVersion:      "1.0.1",
		BaseChangelogRaw: []byte("## [1.0.0]\n"),
		HeadChangelogRaw: []byte("## [1.0.1]\n- marked deprecated, no subsection\n\n## [1.0.0]\n"),
		BaseRoutes:       []byte(""),
		HeadRoutes:       []byte(""),
		BaseSchemaFiles:  map[string][]byte{schemaFile: doc},
		HeadSchemaFiles:  map[string][]byte{schemaFile: doc},
	})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !containsFinding(report.Findings, "version-changelog", SeverityMajor) {
		t.Fatalf("a status-only manifest row change with no matching CHANGELOG subsection must fail the version-changelog rule, got: %+v", report.Findings)
	}
}

// --- D19(c): a brand-new surface with an invalid manifest direction is a
// hard error, not silently accepted ---

func TestRound2_D19c_NewSurfaceInvalidDirectionIsHardError(t *testing.T) {
	existingFile := "rest/v1/dtos.schema.json"
	existingDoc := minimalFile("https://narvi.dev/rest/v1/dtos.schema.json", defsOf("Widget", schemaObj("type", "string")))
	schemaFile := "rest/v2/dtos.schema.json"
	headDoc := minimalFile("https://narvi.dev/rest/v2/dtos.schema.json", defsOf("Widget", schemaObj("type", "string")))

	headManifest := mustMarshal(map[string]any{
		"version": "2.0.0",
		"surfaces": []any{
			map[string]any{"path": existingFile, "direction": DirectiveBySuffix, "status": StatusCurrent},
			map[string]any{"path": schemaFile, "direction": "not-a-real-direction", "status": StatusCurrent},
		},
	})
	_, err := Compare(Input{
		BaseManifestRaw:  manifestJSON(surfaceRow(existingFile, DirectiveBySuffix, StatusCurrent)),
		HeadManifestRaw:  headManifest,
		BaseVersion:      "1.0.0",
		HeadVersion:      "2.0.0",
		BaseChangelogRaw: []byte("## [1.0.0]\n"),
		HeadChangelogRaw: changelogFor("2.0.0", schemaFile),
		BaseRoutes:       []byte(""),
		HeadRoutes:       []byte(""),
		BaseSchemaFiles:  map[string][]byte{existingFile: mustMarshal(existingDoc)},
		HeadSchemaFiles:  map[string][]byte{existingFile: mustMarshal(existingDoc), schemaFile: mustMarshal(headDoc)},
	})
	if err == nil {
		t.Fatal("a brand-new surface with an invalid manifest direction must be a hard Compare() error, got nil")
	}
}

// --- D19(d): the root TITLE half of row 34, not just root $id ---

func TestRound2_D19d_RootTitleChangeIsMajor(t *testing.T) {
	base := mustMarshal(minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Widget", schemaObj("type", "string"))))
	headDoc := minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Widget", schemaObj("type", "string")))
	headDoc["title"] = "A completely different title"
	findings, err := DiffSurface(DirectiveBySuffix, base, mustMarshal(headDoc), nil)
	if err != nil {
		t.Fatalf("DiffSurface: %v", err)
	}
	if !containsFinding(findings, "34", SeverityMajor) {
		t.Fatalf("a root title change must be classified (row 34, MAJOR), got: %+v", findings)
	}
}

// --- D19(f): a NESTED $defs entry (a $defs entry that itself carries a
// $defs map) is fail-closed, not silently accepted ---

func TestRound2_D19f_NestedDefsFailsClosed(t *testing.T) {
	doc := schemaObj(
		"$schema", "https://json-schema.org/draft/2020-12/schema",
		"$id", "https://narvi.dev/t/v1/x.schema.json",
		"title", "T",
		"$defs", map[string]any{
			"Outer": schemaObj("type", "object", "$defs", defsOf("Inner", schemaObj("type", "string"))),
		},
	)
	err := walkSchema(doc, "#", true)
	fc, ok := err.(*FailClosedError)
	if !ok {
		t.Fatalf("want *FailClosedError for a nested $defs entry, got %v", err)
	}
	if fc.Finding.RuleID != "fc-root-only" {
		t.Fatalf("want fc-root-only, got %s", fc.Finding.RuleID)
	}
}

// --- D19(g): the compatibility-column grading uses the BASE manifest's
// direction for a surface both sides declare, never head's ---

func TestRound2_D19g_ContentSeverityUsesBaseDirectionNotHead(t *testing.T) {
	// "summary" removed from required: row 5 is MAJOR on P2C, MINOR on
	// C2P. Base declares platform-to-client; head (in the SAME diff)
	// flips it to client-to-platform. Rule 44 itself catches the flip
	// (MAJOR, unconditionally) -- this test additionally pins that the
	// CONTENT severity is still graded under BASE's column (MAJOR, the
	// P2C reading), not silently re-graded under head's (which would give
	// only MINOR).
	schemaFile := "t/v1/x.schema.json"
	baseDoc := minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Config", schemaObj(
		"type", "object", "properties", schemaObj("summary", schemaObj("type", "string")), "required", []any{"summary"},
	)))
	headDoc := minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Config", schemaObj(
		"type", "object", "properties", schemaObj("summary", schemaObj("type", "string")), "required", []any{},
	)))
	baseManifest := manifestJSON(surfaceRow(schemaFile, string(DirP2C), StatusCurrent))
	headManifest := manifestJSON(surfaceRow(schemaFile, string(DirC2P), StatusCurrent))

	report, err := Compare(Input{
		BaseManifestRaw:  baseManifest,
		HeadManifestRaw:  headManifest,
		BaseVersion:      "1.0.0",
		HeadVersion:      "2.0.0",
		BaseChangelogRaw: []byte("## [1.0.0]\n"),
		HeadChangelogRaw: []byte("## [2.0.0]\n### " + schemaFile + "\n- direction flip + required removed\n\n## [1.0.0]\n"),
		BaseRoutes:       []byte(""),
		HeadRoutes:       []byte(""),
		BaseSchemaFiles:  map[string][]byte{schemaFile: mustMarshal(baseDoc)},
		HeadSchemaFiles:  map[string][]byte{schemaFile: mustMarshal(headDoc)},
	})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !containsFinding(report.Findings, "44", SeverityMajor) {
		t.Fatalf("want rule 44 MAJOR for the direction flip itself, got: %+v", report.Findings)
	}
	if !containsFinding(report.Findings, "5", SeverityMajor) {
		t.Fatalf("want rule 5 graded MAJOR (base's own P2C column), not MINOR (head's C2P column), got: %+v", report.Findings)
	}
}
