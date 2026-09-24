package compat

import (
	"encoding/json"
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
		name:      "row11 enum value added, open",
		ruleID:    "11",
		defName:   "Status",
		baseDefs:  defsOf("Status", schemaObj("type", "string", "enum", []any{"a", "b"})),
		headDefs:  defsOf("Status", schemaObj("type", "string", "enum", []any{"a", "b", "c"})),
		openEnums: map[string]bool{"Status": true},
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
		name:    "row28 oneOf member added",
		ruleID:  "28",
		defName: "Envelope",
		baseDefs: map[string]any{
			"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A")}),
			"A":        schemaObj("type", "string"),
			"B":        schemaObj("type", "integer"),
		},
		headDefs: map[string]any{
			"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A"), schemaObj("$ref", "#/$defs/B")}),
			"A":        schemaObj("type", "string"),
			"B":        schemaObj("type", "integer"),
		},
		fixed: &wantFinding{"28", SeverityMinor},
	},
	{
		name:    "row29 oneOf member removed",
		ruleID:  "29",
		defName: "Envelope",
		baseDefs: map[string]any{
			"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A"), schemaObj("$ref", "#/$defs/B")}),
			"A":        schemaObj("type", "string"),
			"B":        schemaObj("type", "integer"),
		},
		headDefs: map[string]any{
			"Envelope": schemaObj("oneOf", []any{schemaObj("$ref", "#/$defs/A")}),
			"A":        schemaObj("type", "string"),
			"B":        schemaObj("type", "integer"),
		},
		fixed: &wantFinding{"29", SeverityMajor},
	},
	{
		name:    "row30 oneOf member changed, paired by const discriminator",
		ruleID:  "30",
		defName: "Envelope",
		baseDefs: defsOf("Envelope", schemaObj("oneOf", []any{
			schemaObj("type", "object",
				"properties", schemaObj("type", schemaObj("const", "a")),
				"required", []any{"type"},
			),
		})),
		headDefs: defsOf("Envelope", schemaObj("oneOf", []any{
			schemaObj("type", "object",
				"properties", schemaObj("type", schemaObj("const", "a"), "extra", schemaObj("type", "string")),
				"required", []any{"type", "extra"},
			),
		})),
		// The nested change is "extra" added-and-required (row 3): MINOR
		// under P2C, MAJOR under C2P -- the row 30 wrapper inherits
		// whichever severity the nested diff produced.
		p2cWant: &wantFinding{"30", SeverityMinor},
		c2pWant: &wantFinding{"30", SeverityMajor},
	},
	{
		name:    "row28 anyOf member added (keyword coverage)",
		ruleID:  "28",
		defName: "Envelope2",
		baseDefs: map[string]any{
			"Envelope2": schemaObj("anyOf", []any{schemaObj("$ref", "#/$defs/A")}),
			"A":         schemaObj("type", "string"),
			"B":         schemaObj("type", "integer"),
		},
		headDefs: map[string]any{
			"Envelope2": schemaObj("anyOf", []any{schemaObj("$ref", "#/$defs/A"), schemaObj("$ref", "#/$defs/B")}),
			"A":         schemaObj("type", "string"),
			"B":         schemaObj("type", "integer"),
		},
		fixed: &wantFinding{"28", SeverityMinor},
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
		name:    "row35 goJSONSchema codegen hint changed (keyword coverage)",
		ruleID:  "35",
		defName: "Value",
		baseDefs: defsOf("Value", schemaObj("type", "string", "format", "date-time",
			"goJSONSchema", schemaObj("type", "time.Time", "imports", []any{"time"}))),
		headDefs: defsOf("Value", schemaObj("type", "string", "format", "date-time",
			"goJSONSchema", schemaObj("type", "*time.Time", "imports", []any{"time"}))),
		fixed: &wantFinding{"35", SeverityPatch},
	},
	{
		name:     "row39 items presence toggled",
		ruleID:   "39",
		defName:  "List",
		baseDefs: defsOf("List", schemaObj("type", "array")),
		headDefs: defsOf("List", schemaObj("type", "array", "items", schemaObj("type", "string"))),
		fixed:    &wantFinding{"39", SeverityMajor},
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
// 37, 38 (DiffSurfaceSet), 40, 41 (DiffRoutes). ---

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

func TestRow31DefRemoved(t *testing.T) {
	base := mustMarshalFile(t, minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Widget", schemaObj("type", "string"))))
	head := mustMarshalFile(t, minimalFile("https://narvi.dev/t/v1/x.schema.json", map[string]any{}))
	findings, err := DiffSurface(DirectiveBySuffix, base, head, nil)
	if err != nil {
		t.Fatalf("DiffSurface: %v", err)
	}
	if !containsFinding(findings, "31", SeverityMajor) {
		t.Fatalf("want rule 31 MAJOR, got %v", findings)
	}
}

func TestRow32DefAdded(t *testing.T) {
	base := mustMarshalFile(t, minimalFile("https://narvi.dev/t/v1/x.schema.json", map[string]any{}))
	head := mustMarshalFile(t, minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Widget", schemaObj("type", "string"))))
	findings, err := DiffSurface(DirectiveBySuffix, base, head, nil)
	if err != nil {
		t.Fatalf("DiffSurface: %v", err)
	}
	if !containsFinding(findings, "32", SeverityMinor) {
		t.Fatalf("want rule 32 MINOR, got %v", findings)
	}
}

func TestRow34RootIDChanged(t *testing.T) {
	base := mustMarshalFile(t, minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Widget", schemaObj("type", "string"))))
	head := mustMarshalFile(t, minimalFile("https://narvi.dev/t/v1/y.schema.json", defsOf("Widget", schemaObj("type", "string"))))
	findings, err := DiffSurface(DirectiveBySuffix, base, head, nil)
	if err != nil {
		t.Fatalf("DiffSurface: %v", err)
	}
	if !containsFinding(findings, "34", SeverityMajor) {
		t.Fatalf("want rule 34 MAJOR, got %v", findings)
	}
}

func TestRow36SchemaDialectChanged(t *testing.T) {
	baseDoc := minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Widget", schemaObj("type", "string")))
	headDoc := minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Widget", schemaObj("type", "string")))
	headDoc["$schema"] = "https://json-schema.org/draft/2019-09/schema"
	base := mustMarshalFile(t, baseDoc)
	head := mustMarshalFile(t, headDoc)
	findings, err := DiffSurface(DirectiveBySuffix, base, head, nil)
	if err != nil {
		t.Fatalf("DiffSurface: %v", err)
	}
	if !containsFinding(findings, "36", SeverityFailClosed) {
		t.Fatalf("want rule 36 FAIL-CLOSED, got %v", findings)
	}
}

func TestRow37And38SurfaceSet(t *testing.T) {
	baseManifest := Manifest{Surfaces: []ManifestSurface{{Path: "a.schema.json", Direction: DirectiveBySuffix, Status: StatusCurrent}}}
	headManifestRemoved := Manifest{}
	findings := DiffSurfaceSet(baseManifest, headManifestRemoved)
	if !containsFinding(findings, "37", SeverityMajor) {
		t.Fatalf("want rule 37 MAJOR for un-retired removal, got %v", findings)
	}

	retiredBase := Manifest{Surfaces: []ManifestSurface{{Path: "a.schema.json", Direction: DirectiveBySuffix, Status: StatusRetired}}}
	findingsRetired := DiffSurfaceSet(retiredBase, headManifestRemoved)
	if containsFinding(findingsRetired, "37", SeverityMajor) {
		t.Fatalf("removing a retired surface must not raise rule 37, got %v", findingsRetired)
	}

	headAdded := Manifest{Surfaces: []ManifestSurface{{Path: "b.schema.json", Direction: DirectiveBySuffix, Status: StatusCurrent}}}
	findingsAdded := DiffSurfaceSet(Manifest{}, headAdded)
	if !containsFinding(findingsAdded, "38", SeverityMinor) {
		t.Fatalf("want rule 38 MINOR for a new surface, got %v", findingsAdded)
	}
}

func TestRow40And41Routes(t *testing.T) {
	base := []byte("GET /api/sessions/{sessionID}/plans\nGET /api/sessions\n")
	head := []byte("GET /api/sessions\nGET /api/sessions/{sessionID}/new-plans\n")
	findings := DiffRoutes(base, head)
	if !containsFinding(findings, "40", SeverityMajor) {
		t.Fatalf("want rule 40 MAJOR for removed route, got %v", findings)
	}
	if !containsFinding(findings, "41", SeverityMinor) {
		t.Fatalf("want rule 41 MINOR for added route, got %v", findings)
	}
}
