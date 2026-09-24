package compat

import "testing"

// TestFailClosedGuards exercises the fail-closed conditions the design
// spec calls out that sit outside the 41-row table itself (§6.3 design
// spec §2's "Also fail-closed" paragraph): an unknown keyword, a title on
// a non-root sub-schema, a non-local $ref, a malformed type value, and an
// unpairable oneOf/anyOf member.
func TestFailClosedGuards(t *testing.T) {
	t.Run("unknown keyword", func(t *testing.T) {
		err := walkSchema(schemaObj("type", "object", "allOf", []any{}), "#", true)
		fc, ok := err.(*FailClosedError)
		if !ok {
			t.Fatalf("want *FailClosedError, got %v", err)
		}
		if fc.Finding.Severity != SeverityFailClosed {
			t.Fatalf("want SeverityFailClosed, got %v", fc.Finding.Severity)
		}
	})

	t.Run("title on non-root sub-schema", func(t *testing.T) {
		doc := schemaObj(
			"$schema", "https://json-schema.org/draft/2020-12/schema",
			"$id", "https://narvi.dev/t/v1/x.schema.json",
			"title", "Root title is fine",
			"$defs", defsOf("Widget", schemaObj("type", "string", "title", "not allowed here")),
		)
		err := walkSchema(doc, "#", true)
		fc, ok := err.(*FailClosedError)
		if !ok {
			t.Fatalf("want *FailClosedError, got %v", err)
		}
		if fc.Finding.RuleID != "fc-title" {
			t.Fatalf("want fc-title, got %s", fc.Finding.RuleID)
		}
	})

	t.Run("non-local ref", func(t *testing.T) {
		err := walkSchema(schemaObj("$ref", "https://example.com/other.schema.json#/foo"), "#", false)
		fc, ok := err.(*FailClosedError)
		if !ok {
			t.Fatalf("want *FailClosedError, got %v", err)
		}
		if fc.Finding.RuleID != "fc-ref" {
			t.Fatalf("want fc-ref, got %s", fc.Finding.RuleID)
		}
	})

	t.Run("malformed type value", func(t *testing.T) {
		err := walkSchema(schemaObj("type", 42), "#", false)
		fc, ok := err.(*FailClosedError)
		if !ok {
			t.Fatalf("want *FailClosedError, got %v", err)
		}
		if fc.Finding.RuleID != "fc-type" {
			t.Fatalf("want fc-type, got %s", fc.Finding.RuleID)
		}
	})

	t.Run("disallowed keyword in a newly added $defs entry", func(t *testing.T) {
		// M16 pin: a $defs entry that is BRAND NEW in head (not present in
		// base at all) is never routed through diffResolved's own
		// exhaustiveness assertion (there is nothing on the base side to
		// diff it against -- DiffSurface's $defs-added branch just
		// records row 32 and moves on). The ONLY thing that validates a
		// new def's own content against the allowlist is walkSchema
		// recursively walking the whole HEAD document root at the top of
		// DiffSurface. If that call were ever skipped (or pointed at the
		// wrong document), a disallowed keyword arriving inside a new def
		// would pass silently -- this pins that walkSchema(headRoot, ...)
		// is what catches it.
		base := mustMarshalFile(t, minimalFile("https://narvi.dev/t/v1/x.schema.json", defsOf("Widget", schemaObj("type", "string"))))
		head := mustMarshalFile(t, minimalFile("https://narvi.dev/t/v1/x.schema.json", map[string]any{
			"Widget": schemaObj("type", "string"),
			"NewOne": schemaObj("type", "object", "x-narvi-note", "hi"), // disallowed keyword, brand new def
		}))
		findings, err := DiffSurface(DirectiveBySuffix, base, head, nil)
		if err != nil {
			t.Fatalf("DiffSurface: %v", err)
		}
		if !containsFinding(findings, "fc-keyword", SeverityFailClosed) {
			t.Fatalf("a disallowed keyword inside a brand-new $defs entry must fail closed, got: %+v", findings)
		}
	})

	t.Run("unpairable oneOf member", func(t *testing.T) {
		// No $ref, no properties.type.const discriminator, and more than
		// a bare {"type": ...} (+ description) -- none of diffUnion's
		// three pairing strategies apply.
		member := schemaObj("type", "object", "properties", schemaObj("foo", schemaObj("type", "string")))
		baseDefs := defsOf("Envelope", schemaObj("oneOf", []any{member}))
		headDefs := defsOf("Envelope", schemaObj("oneOf", []any{
			schemaObj("type", "object", "properties", schemaObj("foo", schemaObj("type", "integer"))),
		}))
		_, err := DiffDef(baseDefs, headDefs, "Envelope", DirP2C, nil)
		fc, ok := err.(*FailClosedError)
		if !ok {
			t.Fatalf("want *FailClosedError, got %v", err)
		}
		if fc.Finding.RuleID != "fc-oneof-unpairable" {
			t.Fatalf("want fc-oneof-unpairable, got %s", fc.Finding.RuleID)
		}
	})
}
