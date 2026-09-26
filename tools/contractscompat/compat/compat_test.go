package compat

import (
	"encoding/json"
	"fmt"
	"testing"
)

// This file is the corpus the design spec's §6.3 exit criterion asks for:
// a table-driven case per rule-table row (docs cited in COMPATIBILITY.md),
// built directly from small in-memory schema fragments rather than a
// testdata/ directory tree of tiny JSON files -- the fixtures are simple
// enough, and there are enough of them (over 40), that a Go table keeps
// them reviewable in one place and lets TestCorpusCoverage (the coverage
// meta-test, guard 7) walk the very same data no test-execution ordering
// can disturb. Every case still exercises the real library entry points
// (DiffDef, DiffSurface, DiffSurfaceSet, DiffRoutes) against real JSON
// Schema fragments decoded exactly the way the CLI decodes them --
// map[string]any via encoding/json -- so this is not a shortcut around
// the mechanism, only around the on-disk layout.

type wantFinding struct {
	ruleID   string
	severity Severity
}

// corpusCase is one row/direction-pair of the rule table. Most rows are
// "dual": the SAME base/head pair is compared once as P2C and once as
// C2P, and the table's own two columns give two different severities --
// that pairing is exactly what makes a single fixture cover both a MAJOR
// and a non-MAJOR case for that rule id. A handful of rows are "fixed":
// the table gives the same severity regardless of direction (row 1,
// "property removed", is MAJOR|MAJOR -- there is no non-MAJOR variant of
// row 1 to construct, because the table itself never assigns row 1 any
// other severity).
type corpusCase struct {
	name      string
	ruleID    string
	baseDefs  map[string]any
	headDefs  map[string]any
	defName   string
	openEnums map[string]bool

	p2cWant *wantFinding // set for "dual" rows: expected finding under DirP2C
	c2pWant *wantFinding // set for "dual" rows: expected finding under DirC2P
	fixed   *wantFinding // set for "fixed" rows: expected finding, direction-independent (tested under DirP2C)
}

func schemaObj(fields ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(fields); i += 2 {
		m[fields[i].(string)] = fields[i+1]
	}
	return m
}

func defsOf(name string, schema map[string]any) map[string]any {
	return map[string]any{name: schema}
}

// corpus is the full rule-table fixture set. RuleIDs 1-30, 33, 35, and 39
// are exercised per-def via DiffDef; 31, 32, 34, 36, 37, 38, 40, 41 need a
// whole-surface or whole-manifest/routes comparison and are covered by
// dedicated tests below plus their own bucket checks inlined into
// TestCorpusCoverage.
var corpus = []corpusCase{
	{
		name:    "row1 property removed",
		ruleID:  "1",
		defName: "Widget",
		baseDefs: defsOf("Widget", schemaObj(
			"type", "object",
			"properties", schemaObj("a", schemaObj("type", "string"), "b", schemaObj("type", "string")),
		)),
		headDefs: defsOf("Widget", schemaObj(
			"type", "object",
			"properties", schemaObj("a", schemaObj("type", "string")),
		)),
		fixed: &wantFinding{"1", SeverityMajor},
	},
	{
		name:    "row2 property added optional",
		ruleID:  "2",
		defName: "Widget",
		baseDefs: defsOf("Widget", schemaObj(
			"type", "object",
			"properties", schemaObj("a", schemaObj("type", "string")),
		)),
		headDefs: defsOf("Widget", schemaObj(
			"type", "object",
			"properties", schemaObj("a", schemaObj("type", "string"), "b", schemaObj("type", "string")),
		)),
		fixed: &wantFinding{"2", SeverityMinor},
	},
	{
		name:    "row3 property added and required",
		ruleID:  "3",
		defName: "Widget",
		baseDefs: defsOf("Widget", schemaObj(
			"type", "object",
			"properties", schemaObj("a", schemaObj("type", "string")),
			"required", []any{"a"},
		)),
		headDefs: defsOf("Widget", schemaObj(
			"type", "object",
			"properties", schemaObj("a", schemaObj("type", "string"), "b", schemaObj("type", "string")),
			"required", []any{"a", "b"},
		)),
		p2cWant: &wantFinding{"3", SeverityMinor},
		c2pWant: &wantFinding{"3", SeverityMajor},
	},
	{
		name:    "row4 property moved into required",
		ruleID:  "4",
		defName: "Widget",
		baseDefs: defsOf("Widget", schemaObj(
			"type", "object",
			"properties", schemaObj("a", schemaObj("type", "string")),
			"required", []any{},
		)),
		headDefs: defsOf("Widget", schemaObj(
			"type", "object",
			"properties", schemaObj("a", schemaObj("type", "string")),
			"required", []any{"a"},
		)),
		p2cWant: &wantFinding{"4", SeverityMinor},
		c2pWant: &wantFinding{"4", SeverityMajor},
	},
	{
		name:    "row5 property removed from required",
		ruleID:  "5",
		defName: "Widget",
		baseDefs: defsOf("Widget", schemaObj(
			"type", "object",
			"properties", schemaObj("a", schemaObj("type", "string")),
			"required", []any{"a"},
		)),
		headDefs: defsOf("Widget", schemaObj(
			"type", "object",
			"properties", schemaObj("a", schemaObj("type", "string")),
			"required", []any{},
		)),
		p2cWant: &wantFinding{"5", SeverityMajor},
		c2pWant: &wantFinding{"5", SeverityMinor},
	},
	{
		name:     "row6 type changed outright",
		ruleID:   "6",
		defName:  "Value",
		baseDefs: defsOf("Value", schemaObj("type", "string")),
		headDefs: defsOf("Value", schemaObj("type", "integer")),
		fixed:    &wantFinding{"6", SeverityMajor},
	},
	{
		name:     "row7 type widened",
		ruleID:   "7",
		defName:  "Value",
		baseDefs: defsOf("Value", schemaObj("type", "string")),
		headDefs: defsOf("Value", schemaObj("type", []any{"string", "integer"})),
		p2cWant:  &wantFinding{"7", SeverityMajor},
		c2pWant:  &wantFinding{"7", SeverityMinor},
	},
	{
		name:     "row8 type narrowed",
		ruleID:   "8",
		defName:  "Value",
		baseDefs: defsOf("Value", schemaObj("type", []any{"string", "integer"})),
		headDefs: defsOf("Value", schemaObj("type", "string")),
		p2cWant:  &wantFinding{"8", SeverityMinor},
		c2pWant:  &wantFinding{"8", SeverityMajor},
	},
	{
		name:     "row9 null added",
		ruleID:   "9",
		defName:  "Value",
		baseDefs: defsOf("Value", schemaObj("type", "string")),
		headDefs: defsOf("Value", schemaObj("type", []any{"string", "null"})),
		p2cWant:  &wantFinding{"9", SeverityMajor},
		c2pWant:  &wantFinding{"9", SeverityMinor},
	},
	{
		name:     "row10 null removed",
		ruleID:   "10",
		defName:  "Value",
		baseDefs: defsOf("Value", schemaObj("type", []any{"string", "null"})),
		headDefs: defsOf("Value", schemaObj("type", "string")),
		p2cWant:  &wantFinding{"10", SeverityMinor},
		c2pWant:  &wantFinding{"10", SeverityMajor},
	},
	{
		name:     "row11 enum value added, not open",
		ruleID:   "11",
		defName:  "Status",
		baseDefs: defsOf("Status", schemaObj("type", "string", "enum", []any{"a", "b"})),
		headDefs: defsOf("Status", schemaObj("type", "string", "enum", []any{"a", "b", "c"})),
		p2cWant:  &wantFinding{"11", SeverityMajor},
		c2pWant:  &wantFinding{"11", SeverityMinor},
	},
	{
		name:     "row11 enum value added, open",
		ruleID:   "11",
		defName:  "Status",
		baseDefs: defsOf("Status", schemaObj("type", "string", "enum", []any{"a", "b"})),
		headDefs: defsOf("Status", schemaObj("type", "string", "enum", []any{"a", "b", "c"})),
		// D14: openEnums matches by the enum's own EXACT JSON Pointer (here,
		// the def's own root pointer -- "Status"'s enum is declared directly
		// at "#/$defs/Status", not nested under a "properties" entry), not a
		// dotted name.
		openEnums: map[string]bool{"#/$defs/Status": true},
		p2cWant:   &wantFinding{"11", SeverityMinor},
		c2pWant:   &wantFinding{"11", SeverityMinor},
	},
	{
		name:     "row12 enum value removed",
		ruleID:   "12",
		defName:  "Status",
		baseDefs: defsOf("Status", schemaObj("type", "string", "enum", []any{"a", "b"})),
		headDefs: defsOf("Status", schemaObj("type", "string", "enum", []any{"a"})),
		p2cWant:  &wantFinding{"12", SeverityMinor},
		c2pWant:  &wantFinding{"12", SeverityMajor},
	},
	{
		name:     "row13 enum keyword added",
		ruleID:   "13",
		defName:  "Status",
		baseDefs: defsOf("Status", schemaObj("type", "string")),
		headDefs: defsOf("Status", schemaObj("type", "string", "enum", []any{"a"})),
		p2cWant:  &wantFinding{"13", SeverityMinor},
		c2pWant:  &wantFinding{"13", SeverityMajor},
	},
	{
		name:     "row13 enum keyword removed",
		ruleID:   "13",
		defName:  "Status",
		baseDefs: defsOf("Status", schemaObj("type", "string", "enum", []any{"a"})),
		headDefs: defsOf("Status", schemaObj("type", "string")),
		p2cWant:  &wantFinding{"13", SeverityMajor},
		c2pWant:  &wantFinding{"13", SeverityMinor},
	},
	{
		name:     "row14 const changed",
		ruleID:   "14",
		defName:  "Kind",
		baseDefs: defsOf("Kind", schemaObj("const", "a")),
		headDefs: defsOf("Kind", schemaObj("const", "b")),
		fixed:    &wantFinding{"14", SeverityMajor},
	},
	{
		name:     "row15 format added",
		ruleID:   "15",
		defName:  "Value",
		baseDefs: defsOf("Value", schemaObj("type", "string")),
		headDefs: defsOf("Value", schemaObj("type", "string", "format", "date-time")),
		p2cWant:  &wantFinding{"15", SeverityMinor},
		c2pWant:  &wantFinding{"15", SeverityMajor},
	},
	{
		name:     "row16 format removed",
		ruleID:   "16",
		defName:  "Value",
		baseDefs: defsOf("Value", schemaObj("type", "string", "format", "date-time")),
		headDefs: defsOf("Value", schemaObj("type", "string")),
		p2cWant:  &wantFinding{"16", SeverityMajor},
		c2pWant:  &wantFinding{"16", SeverityMinor},
	},
	{
		name:     "row17 format changed",
		ruleID:   "17",
		defName:  "Value",
		baseDefs: defsOf("Value", schemaObj("type", "string", "format", "date-time")),
		headDefs: defsOf("Value", schemaObj("type", "string", "format", "uuid")),
		fixed:    &wantFinding{"17", SeverityMajor},
	},
	{
		name:     "row18 minimum raised",
		ruleID:   "18",
		defName:  "Value",
		baseDefs: defsOf("Value", schemaObj("type", "integer", "minimum", float64(1))),
		headDefs: defsOf("Value", schemaObj("type", "integer", "minimum", float64(5))),
		p2cWant:  &wantFinding{"18", SeverityMinor},
		c2pWant:  &wantFinding{"18", SeverityMajor},
	},
	{
		name:     "row18 minLength raised (keyword coverage)",
		ruleID:   "18",
		defName:  "Value",
		baseDefs: defsOf("Value", schemaObj("type", "string", "minLength", float64(1))),
		headDefs: defsOf("Value", schemaObj("type", "string", "minLength", float64(3))),
		p2cWant:  &wantFinding{"18", SeverityMinor},
		c2pWant:  &wantFinding{"18", SeverityMajor},
	},
	{
		name:     "row18 minItems raised (keyword coverage)",
		ruleID:   "18",
		defName:  "Value",
		baseDefs: defsOf("Value", schemaObj("type", "array", "minItems", float64(0))),
		headDefs: defsOf("Value", schemaObj("type", "array", "minItems", float64(1))),
		p2cWant:  &wantFinding{"18", SeverityMinor},
		c2pWant:  &wantFinding{"18", SeverityMajor},
	},
	{
		name:     "row19 minimum lowered",
		ruleID:   "19",
		defName:  "Value",
		baseDefs: defsOf("Value", schemaObj("type", "integer", "minimum", float64(5))),
		headDefs: defsOf("Value", schemaObj("type", "integer", "minimum", float64(1))),
		p2cWant:  &wantFinding{"19", SeverityMajor},
		c2pWant:  &wantFinding{"19", SeverityMinor},
	},
	{
		// F3 (round 4): row 19's "removed" branch (diffNumericFloor's
		// `case bHas && !hHas`) had NO corpus case at all before this --
		// only "lowered" (above) was pinned, so a mutant that deleted the
		// removed-branch's own finding entirely passed the whole suite,
		// silently accepting a dropped minimum as if it were unchanged.
		name:     "row19 minimum removed (floor lowered to none)",
		ruleID:   "19",
		defName:  "Value",
		baseDefs: defsOf("Value", schemaObj("type", "integer", "minimum", float64(5))),
		headDefs: defsOf("Value", schemaObj("type", "integer")),
		p2cWant:  &wantFinding{"19", SeverityMajor},
		c2pWant:  &wantFinding{"19", SeverityMinor},
	},
	{
		// F3: same branch, minLength (keyword coverage).
		name:     "row19 minLength removed (floor lowered to none, keyword coverage)",
		ruleID:   "19",
		defName:  "Value",
		baseDefs: defsOf("Value", schemaObj("type", "string", "minLength", float64(3))),
		headDefs: defsOf("Value", schemaObj("type", "string")),
		p2cWant:  &wantFinding{"19", SeverityMajor},
		c2pWant:  &wantFinding{"19", SeverityMinor},
	},
	{
		// F3: same branch, minItems (keyword coverage).
		name:     "row19 minItems removed (floor lowered to none, keyword coverage)",
		ruleID:   "19",
		defName:  "Value",
		baseDefs: defsOf("Value", schemaObj("type", "array", "minItems", float64(1))),
		headDefs: defsOf("Value", schemaObj("type", "array")),
		p2cWant:  &wantFinding{"19", SeverityMajor},
		c2pWant:  &wantFinding{"19", SeverityMinor},
	},
	{
		name:     "row20 pattern added",
		ruleID:   "20",
		defName:  "Value",
		baseDefs: defsOf("Value", schemaObj("type", "string")),
		headDefs: defsOf("Value", schemaObj("type", "string", "pattern", "^[a-z]+$")),
		p2cWant:  &wantFinding{"20", SeverityMinor},
		c2pWant:  &wantFinding{"20", SeverityMajor},
	},
	{
		name:     "row21 pattern removed",
		ruleID:   "21",
		defName:  "Value",
		baseDefs: defsOf("Value", schemaObj("type", "string", "pattern", "^[a-z]+$")),
		headDefs: defsOf("Value", schemaObj("type", "string")),
		p2cWant:  &wantFinding{"21", SeverityMajor},
		c2pWant:  &wantFinding{"21", SeverityMinor},
	},
	{
		name:     "row22 pattern changed",
		ruleID:   "22",
		defName:  "Value",
		baseDefs: defsOf("Value", schemaObj("type", "string", "pattern", "^[a-z]+$")),
		headDefs: defsOf("Value", schemaObj("type", "string", "pattern", "^[0-9]+$")),
		fixed:    &wantFinding{"22", SeverityMajor},
	},
	{
		name:    "row23 additionalProperties false to true",
		ruleID:  "23",
		defName: "Widget",
		baseDefs: defsOf("Widget", schemaObj(
			"type", "object", "properties", schemaObj("a", schemaObj("type", "string")), "additionalProperties", false,
		)),
		headDefs: defsOf("Widget", schemaObj(
			"type", "object", "properties", schemaObj("a", schemaObj("type", "string")), "additionalProperties", true,
		)),
		fixed: &wantFinding{"23", SeverityMinor},
	},
	{
		name:    "row24 additionalProperties true to false",
		ruleID:  "24",
		defName: "Widget",
		baseDefs: defsOf("Widget", schemaObj(
			"type", "object", "properties", schemaObj("a", schemaObj("type", "string")), "additionalProperties", true,
		)),
		headDefs: defsOf("Widget", schemaObj(
			"type", "object", "properties", schemaObj("a", schemaObj("type", "string")), "additionalProperties", false,
		)),
		fixed: &wantFinding{"24", SeverityMajor},
	},
	{
		name:    "row25 additionalProperties true to schema",
		ruleID:  "25",
		defName: "Widget",
		baseDefs: defsOf("Widget", schemaObj(
			"type", "object", "properties", schemaObj("a", schemaObj("type", "string")), "additionalProperties", true,
		)),
		headDefs: defsOf("Widget", schemaObj(
			"type", "object", "properties", schemaObj("a", schemaObj("type", "string")), "additionalProperties", schemaObj("type", "string"),
		)),
		fixed: &wantFinding{"25", SeverityMajor},
	},
	{
		name:    "row26 additionalProperties schema on both sides, content differs",
		ruleID:  "26",
		defName: "Widget",
		baseDefs: defsOf("Widget", schemaObj(
			"type", "object", "additionalProperties", schemaObj("type", "string"),
		)),
		headDefs: defsOf("Widget", schemaObj(
			"type", "object", "additionalProperties", schemaObj("type", "integer"),
		)),
		fixed: &wantFinding{"26", SeverityMajor},
	},
	{
		name:    "row27 ref retargeted, content differs (major)",
		ruleID:  "27",
		defName: "Wrapper",
		baseDefs: map[string]any{
			"Wrapper": schemaObj("type", "object", "properties", schemaObj("value", schemaObj("$ref", "#/$defs/A"))),
			"A":       schemaObj("type", "string"),
			"B":       schemaObj("type", "integer"),
		},
		headDefs: map[string]any{
			"Wrapper": schemaObj("type", "object", "properties", schemaObj("value", schemaObj("$ref", "#/$defs/B"))),
			"A":       schemaObj("type", "string"),
			"B":       schemaObj("type", "integer"),
		},
		fixed: &wantFinding{"27", SeverityMajor},
	},
	{
		name:    "row27 ref retargeted, content identical (non-major)",
		ruleID:  "27",
		defName: "Wrapper",
		baseDefs: map[string]any{
			"Wrapper": schemaObj("type", "object", "properties", schemaObj("value", schemaObj("$ref", "#/$defs/A"))),
			"A":       schemaObj("type", "string"),
			"B":       schemaObj("type", "string"),
		},
		headDefs: map[string]any{
			"Wrapper": schemaObj("type", "object", "properties", schemaObj("value", schemaObj("$ref", "#/$defs/B"))),
			"A":       schemaObj("type", "string"),
			"B":       schemaObj("type", "string"),
		},
		fixed: &wantFinding{"27", SeverityPatch},
	},
	{
		// F1 (round 4): a retarget's added enum value must be graded open
		// only when BOTH the OLD target's own pointer (here, "#/$defs/A"
		// -- what the "value" property's consumers were actually bound to
		// before this diff) AND the NEW target's (here, "#/$defs/B") are
		// listed in openEnums. This models the real two-PR exploit the
		// round-4 review found: PR1 adds B as a copy of A with one extra
		// enum value and opens ONLY B's own pointer (modeled here as this
		// corpus case's own base, as if PR1 had already landed); PR2
		// (this diff) retargets "value" from A to B. Before F1, grading
		// read only B's pointer and called this MINOR -- silently handing
		// "value"'s existing consumers (bound to A's closed enum) a value
		// they reject.
		name:    "row27 ref retargeted, enum addition open at NEW target only (F1, must stay MAJOR)",
		ruleID:  "27",
		defName: "Wrapper",
		baseDefs: map[string]any{
			"Wrapper": schemaObj("type", "object", "properties", schemaObj("value", schemaObj("$ref", "#/$defs/A"))),
			"A":       schemaObj("type", "string", "enum", []any{"a", "b"}),
			"B":       schemaObj("type", "string", "enum", []any{"a", "b", "c"}),
		},
		headDefs: map[string]any{
			"Wrapper": schemaObj("type", "object", "properties", schemaObj("value", schemaObj("$ref", "#/$defs/B"))),
			"A":       schemaObj("type", "string", "enum", []any{"a", "b"}),
			"B":       schemaObj("type", "string", "enum", []any{"a", "b", "c"}),
		},
		openEnums: map[string]bool{"#/$defs/B": true},
		p2cWant:   &wantFinding{"27", SeverityMajor},
		c2pWant:   &wantFinding{"27", SeverityMinor},
	},
	{
		// F1 control: the identical retarget, but with BOTH the old
		// target's pointer ("#/$defs/A") and the new target's
		// ("#/$defs/B") open -- only then is the addition safe, since
		// "value"'s pre-existing consumers were independently already
		// told (via A's own pointer) to expect growth.
		name:    "row27 ref retargeted, enum addition open at BOTH old and new target (F1 control)",
		ruleID:  "27",
		defName: "Wrapper",
		baseDefs: map[string]any{
			"Wrapper": schemaObj("type", "object", "properties", schemaObj("value", schemaObj("$ref", "#/$defs/A"))),
			"A":       schemaObj("type", "string", "enum", []any{"a", "b"}),
			"B":       schemaObj("type", "string", "enum", []any{"a", "b", "c"}),
		},
		headDefs: map[string]any{
			"Wrapper": schemaObj("type", "object", "properties", schemaObj("value", schemaObj("$ref", "#/$defs/B"))),
			"A":       schemaObj("type", "string", "enum", []any{"a", "b"}),
			"B":       schemaObj("type", "string", "enum", []any{"a", "b", "c"}),
		},
		openEnums: map[string]bool{"#/$defs/A": true, "#/$defs/B": true},
		fixed:     &wantFinding{"27", SeverityMinor},
	},
	{
		// F1 inline control: the SAME enum addition reached through a
		// STABLE (non-retargeted) $ref -- outside a retarget, defPtr and
		// oldDefPtr are always identical (see loc's own doc comment), so
		// a single open pointer is still sufficient, same as before F1
		// (compare "row11 enum value added, open" above). Wrapper also
		// picks up an unrelated description change so diffResolved's own
		// DeepEqual short-circuit does not skip recursing into "value"
		// entirely (E3's own technique) -- without SOME change at the
		// Wrapper level, base and head Wrapper objects would be
		// byte-identical (both merely say {"$ref":"#/$defs/A"}) even
		// though A's own content, one hop away, differs.
		name:    "row27 (no retarget) enum addition open at the single stable target (F1 inline control)",
		ruleID:  "11",
		defName: "Wrapper",
		baseDefs: map[string]any{
			"Wrapper": schemaObj("type", "object", "description", "v1", "properties", schemaObj("value", schemaObj("$ref", "#/$defs/A"))),
			"A":       schemaObj("type", "string", "enum", []any{"a", "b"}),
		},
		headDefs: map[string]any{
			"Wrapper": schemaObj("type", "object", "description", "v2", "properties", schemaObj("value", schemaObj("$ref", "#/$defs/A"))),
			"A":       schemaObj("type", "string", "enum", []any{"a", "b", "c"}),
		},
		openEnums: map[string]bool{"#/$defs/A": true},
		p2cWant:   &wantFinding{"11", SeverityMinor},
		c2pWant:   &wantFinding{"11", SeverityMinor},
	},
	{
		// Round 5 (G1/G2): shape A (defdiff.go's own union-section doc
		// comment) requires every member to be a $ref to a PURE object
		// def carrying its own properties.type.const discriminator,
		// distinct from every other member's -- A/B must carry one for
		// this to be a permitted union at all, not merely resolve to an
		// object (E1/round 3's own bar, now tightened further).
		name:    "row28 oneOf member added",
		ruleID:  "28",
		defName: "Envelope",
		baseDefs: map[string]any{
			"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A")}),
			"A":        schemaObj("type", "object", "properties", schemaObj("type", schemaObj("const", "a")), "required", []any{"type"}),
			"B":        schemaObj("type", "object", "properties", schemaObj("type", schemaObj("const", "b")), "required", []any{"type"}),
		},
		headDefs: map[string]any{
			"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A"), schemaObj("$ref", "#/$defs/B")}),
			"A":        schemaObj("type", "object", "properties", schemaObj("type", schemaObj("const", "a")), "required", []any{"type"}),
			"B":        schemaObj("type", "object", "properties", schemaObj("type", schemaObj("const", "b")), "required", []any{"type"}),
		},
		fixed: &wantFinding{"28", SeverityMinor},
	},
	{
		// Round 5 (G1/G2): see the sibling "row28 oneOf member added" case
		// above -- A/B must carry a distinct discriminator for shape A to
		// apply at all.
		name:    "row29 oneOf member removed",
		ruleID:  "29",
		defName: "Envelope",
		baseDefs: map[string]any{
			"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A"), schemaObj("$ref", "#/$defs/B")}),
			"A":        schemaObj("type", "object", "properties", schemaObj("type", schemaObj("const", "a")), "required", []any{"type"}),
			"B":        schemaObj("type", "object", "properties", schemaObj("type", schemaObj("const", "b")), "required", []any{"type"}),
		},
		headDefs: map[string]any{
			"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A")}),
			"A":        schemaObj("type", "object", "properties", schemaObj("type", schemaObj("const", "a")), "required", []any{"type"}),
			"B":        schemaObj("type", "object", "properties", schemaObj("type", schemaObj("const", "b")), "required", []any{"type"}),
		},
		fixed: &wantFinding{"29", SeverityMajor},
	},
	{
		// Round 5 (G1/G2): shape A's members must be a "$ref" (defdiff.go's
		// own union-section doc comment) -- this fixture used to spell A
		// INLINE, which round 5 no longer permits (G1: an inline
		// discriminated member is unclassifiable, not a legitimate
		// alternate pairing strategy). Pairing is by the $ref's own target
		// NAME now, not the discriminator value itself -- "A" is unchanged
		// between base and head, so this still recurses into A's own
		// content and reports the nested change under row 30. Envelope
		// ALSO picks up an unrelated description change so diffResolved's
		// own DeepEqual short-circuit does not skip recursing into the
		// oneOf array entirely -- Envelope's own JSON is otherwise
		// byte-identical between base and head (only A, a separate $defs
		// entry, differs), the same technique row27's own "F1 inline
		// control" corpus case above uses.
		name:    "row30 oneOf member changed (paired by $ref name)",
		ruleID:  "30",
		defName: "Envelope",
		baseDefs: map[string]any{
			"Envelope": schemaObj("description", "v1", "oneOf", []any{schemaObj("$ref", "#/$defs/A")}),
			"A": schemaObj("type", "object",
				"properties", schemaObj("type", schemaObj("const", "a")),
				"required", []any{"type"},
			),
		},
		headDefs: map[string]any{
			"Envelope": schemaObj("description", "v2", "oneOf", []any{schemaObj("$ref", "#/$defs/A")}),
			"A": schemaObj("type", "object",
				"properties", schemaObj("type", schemaObj("const", "a"), "extra", schemaObj("type", "string")),
				"required", []any{"type", "extra"},
			),
		},
		// The nested change is "extra" added-and-required (row 3): MINOR
		// under P2C, MAJOR under C2P -- the row 30 wrapper inherits
		// whichever severity the nested diff produced.
		p2cWant: &wantFinding{"30", SeverityMinor},
		c2pWant: &wantFinding{"30", SeverityMajor},
	},
	{
		// Round 5 (G1/G2): see "row28 oneOf member added" above -- A/B
		// must carry a distinct discriminator for shape A to apply.
		name:    "row28 anyOf member added (keyword coverage)",
		ruleID:  "28",
		defName: "Envelope2",
		baseDefs: map[string]any{
			"Envelope2": schemaObj("anyOf", []any{schemaObj("$ref", "#/$defs/A")}),
			"A":         schemaObj("type", "object", "properties", schemaObj("type", schemaObj("const", "a")), "required", []any{"type"}),
			"B":         schemaObj("type", "object", "properties", schemaObj("type", schemaObj("const", "b")), "required", []any{"type"}),
		},
		headDefs: map[string]any{
			"Envelope2": schemaObj("anyOf", []any{schemaObj("$ref", "#/$defs/A"), schemaObj("$ref", "#/$defs/B")}),
			"A":         schemaObj("type", "object", "properties", schemaObj("type", schemaObj("const", "a")), "required", []any{"type"}),
			"B":         schemaObj("type", "object", "properties", schemaObj("type", schemaObj("const", "b")), "required", []any{"type"}),
		},
		fixed: &wantFinding{"28", SeverityMinor},
	},
	{
		// Round 5 (G2): an added shape-A variant (a NEW $ref name, "B")
		// whose discriminator value ("a") was already assigned to a
		// DIFFERENT member in base ("A", which is REMOVED in this same
		// diff) is row 46 MAJOR, not row 28 MINOR -- an in-flight
		// consumer still dispatches on the wire value "a", not on which
		// $defs name produced it, so it would decode this payload as the
		// OLD "A" shape and reject it the moment the two shapes diverge
		// (here, "B" additionally requires "extra").
		name:    "row46 discriminated union member added reusing a base discriminator value (G2)",
		ruleID:  "46",
		defName: "Envelope",
		baseDefs: map[string]any{
			"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A")}),
			"A":        schemaObj("type", "object", "properties", schemaObj("type", schemaObj("const", "a")), "required", []any{"type"}),
		},
		headDefs: map[string]any{
			"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/B")}),
			"B":        schemaObj("type", "object", "properties", schemaObj("type", schemaObj("const", "a")), "required", []any{"type", "extra"}),
		},
		fixed: &wantFinding{"46", SeverityMajor},
	},
	{
		name:     "row33 default changed",
		ruleID:   "33",
		defName:  "Value",
		baseDefs: defsOf("Value", schemaObj("type", "boolean", "default", false)),
		headDefs: defsOf("Value", schemaObj("type", "boolean", "default", true)),
		fixed:    &wantFinding{"33", SeverityMajor},
	},
	{
		name:     "row35 description changed",
		ruleID:   "35",
		defName:  "Value",
		baseDefs: defsOf("Value", schemaObj("type", "string", "description", "old")),
		headDefs: defsOf("Value", schemaObj("type", "string", "description", "new")),
		fixed:    &wantFinding{"35", SeverityPatch},
	},
	{
		name:    "row45 goJSONSchema codegen hint changed (keyword coverage, C19)",
		ruleID:  "45",
		defName: "Value",
		baseDefs: defsOf("Value", schemaObj("type", "string", "format", "date-time",
			"goJSONSchema", schemaObj("type", "time.Time", "imports", []any{"time"}))),
		headDefs: defsOf("Value", schemaObj("type", "string", "format", "date-time",
			"goJSONSchema", schemaObj("type", "*time.Time", "imports", []any{"time"}))),
		// C19: a goJSONSchema change is scored MAJOR in both columns, not
		// PATCH -- it changes what the generated Go decoder accepts,
		// which matters on the C2P column (the platform is that
		// decoder's own consumer).
		fixed: &wantFinding{"45", SeverityMajor},
	},
	{
		name:     "row39 items presence toggled",
		ruleID:   "39",
		defName:  "List",
		baseDefs: defsOf("List", schemaObj("type", "array")),
		headDefs: defsOf("List", schemaObj("type", "array", "items", schemaObj("type", "string"))),
		fixed:    &wantFinding{"39", SeverityMajor},
	},
	{
		// C13: additionalProperties schema -> permissive (true/absent) is
		// its own row, distinct from row 23's false -> anything "unlock".
		name:    "row42 additionalProperties schema loosened to permissive",
		ruleID:  "42",
		defName: "Widget",
		baseDefs: defsOf("Widget", schemaObj(
			"type", "object", "additionalProperties", schemaObj("type", "string"),
		)),
		headDefs: defsOf("Widget", schemaObj(
			"type", "object", "additionalProperties", true,
		)),
		p2cWant: &wantFinding{"42", SeverityMajor},
		c2pWant: &wantFinding{"42", SeverityMinor},
	},
	{
		// C15: introducing oneOf/anyOf where the keyword did not exist at
		// all before is a presence change, MAJOR in both columns -- not
		// the generic row-28 "variant added" MINOR bucket.
		name:     "row43 oneOf keyword presence changed (introduced)",
		ruleID:   "43",
		defName:  "Envelope",
		baseDefs: defsOf("Envelope", schemaObj("description", "x")),
		headDefs: defsOf("Envelope", schemaObj("description", "x", "oneOf", []any{schemaObj("type", "string")})),
		fixed:    &wantFinding{"43", SeverityMajor},
	},
	{
		// C3: a newly added anyOf member that is exactly {"type": "null"}
		// is nullability introduced via a union, not a generic "variant
		// added" -- it must be scored like row 9 (null added to type), by
		// direction, not the flat MINOR/MINOR row 28. Round 5 (G1/G2): A
		// must be a $ref to a pure object def for this to be shape B (the
		// nullable-object pattern, defdiff.go's own union-section doc
		// comment, matching rest/v1/dtos.schema.json's real
		// ReviewReadout.latestVerdict) -- A used to be a bare
		// {"type":"string"} here, which no longer matches any permitted
		// shape once paired with a null branch.
		name:    "row9 nullable-via-anyOf member added (C3)",
		ruleID:  "9",
		defName: "Wrapper",
		baseDefs: map[string]any{
			"Wrapper": schemaObj("anyOf", []any{schemaObj("$ref", "#/$defs/A")}),
			"A":       schemaObj("type", "object"),
		},
		headDefs: map[string]any{
			"Wrapper": schemaObj("anyOf", []any{schemaObj("$ref", "#/$defs/A"), schemaObj("type", "null")}),
			"A":       schemaObj("type", "object"),
		},
		p2cWant: &wantFinding{"9", SeverityMajor},
		c2pWant: &wantFinding{"9", SeverityMinor},
	},
	{
		// E5 (round 3): the mirror image of "row9 nullable-via-anyOf
		// member added (C3)" above -- before this case, diffUnion's row
		// 10 branch (a {"type":"null"} member REMOVED from an existing
		// union) had no corpus pin at all, and a mutant that replaced
		// its finding-append with a no-op survived the entire suite
		// (verified by hand: applying that exact mutant made THIS case
		// fail with "want rule 10 severity MINOR, got []", confirming it
		// now kills the mutant; PR body's "Review round 3" section has
		// the full mutation-verification record).
		// Round 5 (G1/G2): see "row9 nullable-via-anyOf member added (C3)"
		// above -- A must be a $ref to a pure object def for this to be
		// shape B.
		name:    "row10 nullable-via-anyOf member removed (C3)",
		ruleID:  "10",
		defName: "Wrapper",
		baseDefs: map[string]any{
			"Wrapper": schemaObj("anyOf", []any{schemaObj("$ref", "#/$defs/A"), schemaObj("type", "null")}),
			"A":       schemaObj("type", "object"),
		},
		headDefs: map[string]any{
			"Wrapper": schemaObj("anyOf", []any{schemaObj("$ref", "#/$defs/A")}),
			"A":       schemaObj("type", "object"),
		},
		p2cWant: &wantFinding{"10", SeverityMinor},
		c2pWant: &wantFinding{"10", SeverityMajor},
	},
	{
		// E5 (round 3): a bare boolean schema literal (true/false, e.g.
		// as an `items` value) changing had no corpus pin at all, and a
		// mutant that replaced diffNode's own boolean-literal comparison
		// branch with a no-op survived the entire suite (verified by
		// hand: applying that exact mutant made THIS case fail with
		// "want rule 6 severity MAJOR, got []", confirming it now kills
		// the mutant; PR body's "Review round 3" section has the full
		// mutation-verification record).
		name:     "row6 boolean schema literal changed (items)",
		ruleID:   "6",
		defName:  "Widget",
		baseDefs: defsOf("Widget", schemaObj("type", "array", "items", true)),
		headDefs: defsOf("Widget", schemaObj("type", "array", "items", false)),
		fixed:    &wantFinding{"6", SeverityMajor},
	},
}

func TestCorpus(t *testing.T) {
	for _, tc := range corpus {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			check := func(dir Direction, want *wantFinding) {
				if want == nil {
					return
				}
				findings, err := DiffDef(tc.baseDefs, tc.headDefs, tc.defName, dir, tc.openEnums)
				if err != nil {
					t.Fatalf("DiffDef(%s): unexpected error: %v", dir, err)
				}
				if !containsFinding(findings, want.ruleID, want.severity) {
					t.Fatalf("DiffDef(%s): want rule %s severity %s, got %v", dir, want.ruleID, want.severity, findings)
				}
			}
			if tc.fixed != nil {
				check(DirP2C, tc.fixed)
				check(DirC2P, tc.fixed)
			}
			check(DirP2C, tc.p2cWant)
			check(DirC2P, tc.c2pWant)
		})
	}
}

func containsFinding(findings []Finding, ruleID string, sev Severity) bool {
	for _, f := range findings {
		if f.RuleID == ruleID && f.Severity == sev {
			return true
		}
	}
	return false
}

// --- rules that need a whole-surface, whole-manifest, or routes.golden
// comparison rather than a single def pair: 31, 32, 34, 36 (DiffSurface),
// 37, 38, 44 (DiffSurfaceSet/Compare), 40, 41 (DiffRoutes). ---
//
// wholeSurfaceCorpus is deliberately a DATA TABLE, run through the real
// entry points by BOTH TestWholeSurfaceCorpus (the per-case assertion,
// below) AND TestCorpusCoverage (meta_test.go's guard-7 coverage check).
// The two tests share this table rather than TestCorpusCoverage hard-
// coding which rule ids it considers "covered" (C17: a hard-coded
// record() call cannot notice its own fixture rotting or being deleted --
// a mutant that disables TestWholeSurfaceCorpus, or one of its individual
// t.Run cases, does not touch this table, so TestCorpusCoverage keeps
// running the SAME producer function fresh and keeps failing/passing on
// what it actually returns, independent of whatever happened to the other
// test).

func minimalFile(id string, defs map[string]any) map[string]any {
	return schemaObj(
		"$schema", "https://json-schema.org/draft/2020-12/schema",
		"$id", id,
		"title", "Test",
		"description", "test file",
		"$defs", defs,
	)
}

func mustMarshalFile(t *testing.T, doc map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

// mustMarshal is mustMarshalFile without a *testing.T, for use inside
// wholeSurfaceCorpus producer functions that TestCorpusCoverage also
// calls directly (outside of any t.Run) -- every input here is
// hand-written valid JSON-able data, so a marshal error would be a bug in
// this test file itself, worth a hard panic rather than a swallowed
// error.
func mustMarshal(doc any) []byte {
	data, err := json.Marshal(doc)
	if err != nil {
		panic(fmt.Sprintf("mustMarshal: %v", err))
	}
	return data
}

// wholeSurfaceProducer runs one whole-surface/whole-manifest/routes
// scenario against the real entry point and returns whatever Findings it
// produced (or an error, for the handful of guard-level fail-closed
// cases). keywordsUsed lists the allowlisted keywords this fixture's own
// JSON literally contains, at the fixture-construction level, so
// TestCorpusCoverage's keyword-coverage half can be derived from the same
// data instead of a second hard-coded list.
type wholeSurfaceCase struct {
	name         string
	ruleID       string
	severity     Severity
	keywordsUsed []string
	run          func() ([]Finding, error)
}

var wholeSurfaceCorpus = []wholeSurfaceCase{
	{
		name:         "row31 defs entry removed",
		ruleID:       "31",
		severity:     SeverityMajor,
		keywordsUsed: []string{"$schema", "$id", "title", "description", "$defs", "type"},
		run: func() ([]Finding, error) {
			base := mustMarshal(minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Widget", schemaObj("type", "string"))))
			head := mustMarshal(minimalFile("https://narvi.dev/t/v1/x.schema.json", map[string]any{}))
			return DiffSurface(DirectiveBySuffix, base, head, nil)
		},
	},
	{
		name:         "row32 defs entry added",
		ruleID:       "32",
		severity:     SeverityMinor,
		keywordsUsed: []string{"$schema", "$id", "title", "description", "$defs", "type"},
		run: func() ([]Finding, error) {
			base := mustMarshal(minimalFile("https://narvi.dev/t/v1/x.schema.json", map[string]any{}))
			head := mustMarshal(minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Widget", schemaObj("type", "string"))))
			return DiffSurface(DirectiveBySuffix, base, head, nil)
		},
	},
	{
		name:         "row34 root $id changed",
		ruleID:       "34",
		severity:     SeverityMajor,
		keywordsUsed: []string{"$schema", "$id", "title", "description", "$defs", "type"},
		run: func() ([]Finding, error) {
			base := mustMarshal(minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Widget", schemaObj("type", "string"))))
			head := mustMarshal(minimalFile("https://narvi.dev/t/v1/y.schema.json", defsOf("Widget", schemaObj("type", "string"))))
			return DiffSurface(DirectiveBySuffix, base, head, nil)
		},
	},
	{
		name:         "row36 root $schema changed",
		ruleID:       "36",
		severity:     SeverityFailClosed,
		keywordsUsed: []string{"$schema", "$id", "title", "description", "$defs", "type"},
		run: func() ([]Finding, error) {
			baseDoc := minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Widget", schemaObj("type", "string")))
			headDoc := minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Widget", schemaObj("type", "string")))
			headDoc["$schema"] = "https://json-schema.org/draft/2019-09/schema"
			return DiffSurface(DirectiveBySuffix, mustMarshal(baseDoc), mustMarshal(headDoc), nil)
		},
	},
	{
		name:     "row37 schema file removed, not retired",
		ruleID:   "37",
		severity: SeverityMajor,
		run: func() ([]Finding, error) {
			base := Manifest{Surfaces: []ManifestSurface{{Path: "a.schema.json", Direction: DirectiveBySuffix, Status: StatusCurrent}}}
			return DiffSurfaceSet(base, Manifest{}), nil
		},
	},
	{
		name:     "row38 schema file added",
		ruleID:   "38",
		severity: SeverityMinor,
		run: func() ([]Finding, error) {
			head := Manifest{Surfaces: []ManifestSurface{{Path: "b.schema.json", Direction: DirectiveBySuffix, Status: StatusCurrent}}}
			return DiffSurfaceSet(Manifest{}, head), nil
		},
	},
	{
		name:     "row40 route removed",
		ruleID:   "40",
		severity: SeverityMajor,
		run: func() ([]Finding, error) {
			base := []byte("GET /api/sessions/{sessionID}/plans\nGET /api/sessions\n")
			head := []byte("GET /api/sessions\n")
			return DiffRoutes(base, head), nil
		},
	},
	{
		name:     "row41 route added",
		ruleID:   "41",
		severity: SeverityMinor,
		run: func() ([]Finding, error) {
			base := []byte("GET /api/sessions\n")
			head := []byte("GET /api/sessions\nGET /api/sessions/{sessionID}/new-plans\n")
			return DiffRoutes(base, head), nil
		},
	},
	// Rows 40/41 grade every routes.golden row, not only /api/ ones: the
	// three cases below are the non-/api/ counterparts of the two above,
	// each of which DiffRoutes used to skip without a finding.
	{
		name:     "row40 non-/api/ route removed",
		ruleID:   "40",
		severity: SeverityMajor,
		run: func() ([]Finding, error) {
			base := []byte("GET /api/sessions\nGET /health\nPOST /mcp\n")
			head := []byte("GET /api/sessions\nPOST /mcp\n")
			return DiffRoutes(base, head), nil
		},
	},
	{
		name:     "row40 non-/api/ route method changed",
		ruleID:   "40",
		severity: SeverityMajor,
		run: func() ([]Finding, error) {
			base := []byte("GET /api/sessions\nPOST /oauth/revoke\n")
			head := []byte("DELETE /oauth/revoke\nGET /api/sessions\n")
			return DiffRoutes(base, head), nil
		},
	},
	{
		name:     "row41 non-/api/ route added",
		ruleID:   "41",
		severity: SeverityMinor,
		run: func() ([]Finding, error) {
			base := []byte("GET /api/sessions\nPOST /oauth/token\n")
			head := []byte("GET /.well-known/oauth-authorization-server/oauth\nGET /api/sessions\nPOST /oauth/token\n")
			return DiffRoutes(base, head), nil
		},
	},
	{
		// C1: a surface's direction flipping between base and head is its
		// own dedicated MAJOR finding (rule 44), exercised through the
		// full Compare() pipeline since that is where base-vs-head
		// manifest rows are actually compared.
		name:     "row44 manifest direction changed",
		ruleID:   "44",
		severity: SeverityMajor,
		run: func() ([]Finding, error) {
			schemaFile := "t/v1/x.schema.json"
			doc := mustMarshal(minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Widget", schemaObj("type", "string"))))
			baseManifest := mustMarshal(map[string]any{
				"version":  "1.0.0",
				"surfaces": []any{map[string]any{"path": schemaFile, "direction": DirectiveBySuffix, "status": StatusCurrent}},
			})
			headManifest := mustMarshal(map[string]any{
				"version":  "1.0.0",
				"surfaces": []any{map[string]any{"path": schemaFile, "direction": string(DirP2C), "status": StatusCurrent}},
			})
			report, err := Compare(Input{
				BaseManifestRaw:  baseManifest,
				HeadManifestRaw:  headManifest,
				BaseVersion:      "1.0.0",
				HeadVersion:      "1.0.1",
				BaseChangelogRaw: []byte("## [1.0.0]\n"),
				HeadChangelogRaw: []byte("## [1.0.1]\n### " + schemaFile + "\n- direction flip\n\n## [1.0.0]\n"),
				BaseRoutes:       []byte(""),
				HeadRoutes:       []byte(""),
				BaseSchemaFiles:  map[string][]byte{schemaFile: doc},
				HeadSchemaFiles:  map[string][]byte{schemaFile: doc},
			})
			return report.Findings, err
		},
	},
}

func TestWholeSurfaceCorpus(t *testing.T) {
	for _, tc := range wholeSurfaceCorpus {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			findings, err := tc.run()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !containsFinding(findings, tc.ruleID, tc.severity) {
				t.Fatalf("want rule %s severity %s, got %v", tc.ruleID, tc.severity, findings)
			}
		})
	}
}

// TestRow37RetiredRemovalIsNotBreaking and TestOpenNewSurfaceHasNoManifestRowError
// pin the negative/error-path behavior around row 37/38's own guard rails
// that wholeSurfaceCorpus's straight-line cases above don't exercise.
func TestRow37RetiredRemovalIsNotBreaking(t *testing.T) {
	retiredBase := Manifest{Surfaces: []ManifestSurface{{Path: "a.schema.json", Direction: DirectiveBySuffix, Status: StatusRetired}}}
	findings := DiffSurfaceSet(retiredBase, Manifest{})
	if containsFinding(findings, "37", SeverityMajor) {
		t.Fatalf("removing a retired surface must not raise rule 37, got %v", findings)
	}
}
