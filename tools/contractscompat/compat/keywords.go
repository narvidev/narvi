package compat

import (
	"fmt"
	"sort"
)

// allowedKeywords is the CLOSED set of JSON Schema keywords this checker
// understands, in a schema-node position -- exactly the set the design
// spec's inventory pass found in use across all five contracts files
// today (§6.3 design spec §2). Anything else -- allOf, not, if/then/else,
// patternProperties, propertyNames, prefixItems, additionalItems,
// maxItems, maxLength, maximum, exclusiveMinimum/Maximum, multipleOf,
// uniqueItems, contains, dependentRequired/Schemas, unevaluated*,
// $anchor, $dynamicRef, $comment, examples, deprecated, readOnly,
// writeOnly, discriminator, any x-* extension, or any other unknown key --
// makes walkSchema fail closed rather than silently ignore it.
//
// This is a POSITIONAL allowlist: it is only ever consulted against the
// keys of a map that IS a schema node (the root document, a $defs/<Name>
// entry, a properties/<name> entry, an items schema, an oneOf/anyOf
// element, or additionalProperties when it is itself a schema). It is
// NEVER consulted against a property name (properties/$defs map keys) or
// against data carried under enum/const/default/required -- see
// walkSchema's own doc comment for why that distinction is the whole
// point (a property can be named "definitions" and must not be mistaken
// for the $defs keyword).
var allowedKeywords = map[string]bool{
	"$schema":              true,
	"$id":                  true,
	"$defs":                true,
	"$ref":                 true,
	"title":                true,
	"description":          true,
	"type":                 true,
	"properties":           true,
	"required":             true,
	"additionalProperties": true,
	"enum":                 true,
	"const":                true,
	"oneOf":                true,
	"anyOf":                true,
	"items":                true,
	"format":               true,
	"pattern":              true,
	"minimum":              true,
	"minLength":            true,
	"minItems":             true,
	"default":              true,
	// goJSONSchema is go-jsonschema's own vendor extension (an opaque
	// {"type": "...", "imports": [...]} object controlling the exact Go
	// type it emits for one property -- e.g. forcing *time.Time instead
	// of a named pointer alias, or json.RawMessage for an opaque passthrough
	// field). It is genuinely in use today (rest/v1/dtos.schema.json,
	// verified while implementing this checker) and, unlike a real
	// unknown keyword, changing it cannot change the JSON wire shape any
	// consumer -- Go, TS, or otherwise -- actually sees: it only steers
	// THIS repository's own generated Go type. defdiff.go's
	// diffAnnotations treats a change to it the same as a description
	// change (row 35, PATCH), never a compatibility break.
	"goJSONSchema": true,
}

// AllowedKeywordNames returns the allowlist's keys, sorted, for the
// corpus coverage meta-test (every allowlisted keyword must appear in at
// least one corpus case) and for CLI diagnostics.
func AllowedKeywordNames() []string {
	names := make([]string, 0, len(allowedKeywords))
	for k := range allowedKeywords {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// FailClosedError marks that walkSchema (or a semantic rule that only
// discovers the problem while comparing two documents, such as an
// unpairable oneOf or a non-local $ref) hit a shape the rule table does
// not name. It is both a Finding-carrying error AND the signal
// tools/contractscompat's main.go uses to stop classifying and exit
// non-zero without printing any other (possibly misleading) findings for
// that surface.
type FailClosedError struct {
	Finding Finding
}

func (e *FailClosedError) Error() string {
	return fmt.Sprintf("fail-closed at %s: %s", e.Finding.Pointer, e.Finding.Message)
}

func failClosed(ruleID, pointer, format string, args ...any) *FailClosedError {
	return &FailClosedError{Finding: Finding{
		RuleID:   ruleID,
		Severity: SeverityFailClosed,
		Pointer:  pointer,
		Message:  fmt.Sprintf(format, args...) + " -- extend tools/contractscompat and its corpus in a separate PR first",
	}}
}

// walkSchema structurally validates one JSON document (already decoded
// into `any` trees by encoding/json) against the allowlist above,
// interpreting keys ONLY where a schema is actually expected -- never by
// scanning flat key names. isRoot is true only for the document's own
// top-level object: "title" is only legal there (a title on any nested
// sub-schema is fail-closed, per the design spec's keyword-allowlist
// note), and root-only keywords ($schema, $id) are otherwise ordinary
// members of the allowlist so they are simply ignored (not flagged) when
// walkSchema recurses into a non-root node that happens not to carry
// them -- the root-only ENFORCEMENT is specifically about title, plus the
// $id/$schema/title root-level semantic comparisons rootdiff.go makes
// separately.
//
// A schema node may also be the JSON boolean literals true/false (valid
// "always passes"/"always fails" schemas, most commonly seen as
// additionalProperties or items values) -- those carry no keys at all and
// walkSchema accepts them immediately.
func walkSchema(node any, ptr string, isRoot bool) error {
	switch v := node.(type) {
	case bool:
		return nil
	case map[string]any:
		return walkSchemaObject(v, ptr, isRoot)
	default:
		return failClosed("fc-shape", ptr, "expected a schema (object or boolean) but found %T", node)
	}
}

func walkSchemaObject(obj map[string]any, ptr string, isRoot bool) error {
	for key := range obj {
		if !allowedKeywords[key] {
			return failClosed("fc-keyword", ptr+"/"+key, "keyword %q is not in the closed allowlist", key)
		}
		if key == "title" && !isRoot {
			return failClosed("fc-title", ptr+"/title", "\"title\" is only permitted on the document root")
		}
	}

	if raw, ok := obj["$defs"]; ok {
		m, ok := raw.(map[string]any)
		if !ok {
			return failClosed("fc-shape", ptr+"/$defs", "$defs must be an object")
		}
		for name, sub := range m {
			if err := walkSchema(sub, ptr+"/$defs/"+jsonPointerEscape(name), false); err != nil {
				return err
			}
		}
	}

	if raw, ok := obj["properties"]; ok {
		m, ok := raw.(map[string]any)
		if !ok {
			return failClosed("fc-shape", ptr+"/properties", "properties must be an object")
		}
		for name, sub := range m {
			if err := walkSchema(sub, ptr+"/properties/"+jsonPointerEscape(name), false); err != nil {
				return err
			}
		}
	}

	if raw, ok := obj["items"]; ok {
		if err := walkSchema(raw, ptr+"/items", false); err != nil {
			return err
		}
	}

	for _, kw := range []string{"oneOf", "anyOf"} {
		raw, ok := obj[kw]
		if !ok {
			continue
		}
		arr, ok := raw.([]any)
		if !ok {
			return failClosed("fc-shape", ptr+"/"+kw, "%s must be an array", kw)
		}
		for i, sub := range arr {
			if err := walkSchema(sub, fmt.Sprintf("%s/%s/%d", ptr, kw, i), false); err != nil {
				return err
			}
		}
	}

	if raw, ok := obj["additionalProperties"]; ok {
		switch raw.(type) {
		case bool:
			// fine, no recursion
		default:
			if err := walkSchema(raw, ptr+"/additionalProperties", false); err != nil {
				return err
			}
		}
	}

	if raw, ok := obj["$ref"]; ok {
		s, ok := raw.(string)
		if !ok {
			return failClosed("fc-shape", ptr+"/$ref", "$ref must be a string")
		}
		if _, err := localRefTarget(s); err != nil {
			return failClosed("fc-ref", ptr+"/$ref", "%v", err)
		}
	}

	if raw, ok := obj["type"]; ok {
		if err := validateTypeShape(raw); err != nil {
			return failClosed("fc-type", ptr+"/type", "%v", err)
		}
	}

	// required: array of strings (data, never walked as schema).
	if raw, ok := obj["required"]; ok {
		arr, ok := raw.([]any)
		if !ok {
			return failClosed("fc-shape", ptr+"/required", "required must be an array")
		}
		for _, el := range arr {
			if _, ok := el.(string); !ok {
				return failClosed("fc-shape", ptr+"/required", "required elements must be strings")
			}
		}
	}

	// enum/const/default/format/pattern/minimum/minLength/minItems/
	// description/$schema/$id: leaf data or scalars, deliberately never
	// walked as schema positions.
	return nil
}

// jsonPointerEscape escapes a raw string for use as one segment of a JSON
// Pointer (RFC 6901): "~" -> "~0", "/" -> "~1".
func jsonPointerEscape(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '~':
			out = append(out, '~', '0')
		case '/':
			out = append(out, '~', '1')
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}

// jsonSchemaTypeNames is the fixed vocabulary draft 2020-12 allows in a
// `type` keyword.
var jsonSchemaTypeNames = map[string]bool{
	"null": true, "boolean": true, "object": true, "array": true,
	"number": true, "string": true, "integer": true,
}

// validateTypeShape enforces the allowlist's own extra rule on `type`: a
// bare string naming one of the seven draft 2020-12 type names is always
// fine; an array is fine as long as every element is one of those names,
// with no duplicates. Every type array actually used under /contracts
// today is the narrower "exactly one non-null type plus null" shape, but
// the rule table's own rows 7/8 ("type widened"/"type narrowed", a union
// gaining or losing a member) describe a genuine multi-member union --
// restricting the allowlist to null-only pairings the design spec's own
// wording suggested would make those two rows structurally unreachable
// by any corpus fixture, which conflicts with the corpus coverage
// guard's requirement that every rule id have both a MAJOR and a
// non-MAJOR case. This checker accepts the more general (but still
// closed) vocabulary instead; anything not in jsonSchemaTypeNames, a
// non-string element, or a duplicate still fails closed.
func validateTypeShape(raw any) error {
	switch v := raw.(type) {
	case string:
		if !jsonSchemaTypeNames[v] {
			return fmt.Errorf("unknown type name %q", v)
		}
		return nil
	case []any:
		if len(v) == 0 {
			return fmt.Errorf("a type array must not be empty")
		}
		seen := map[string]bool{}
		for _, el := range v {
			s, ok := el.(string)
			if !ok {
				return fmt.Errorf("type array elements must be strings")
			}
			if !jsonSchemaTypeNames[s] {
				return fmt.Errorf("unknown type name %q", s)
			}
			if seen[s] {
				return fmt.Errorf("duplicate type name %q", s)
			}
			seen[s] = true
		}
		return nil
	default:
		return fmt.Errorf("type must be a string or an array")
	}
}
