package compat

import (
	"encoding/json"
	"fmt"
	"sort"
)

// DiffSurface compares one schema FILE's base and head bytes end to end:
// the keyword allowlist (fail-closed), the root's own administrative
// keywords ($schema/$id/title -- rows 34/36), the $defs set (rows 31-32),
// every def present on both sides (via DefDirections + DiffDef), and
// finally every OTHER root-level keyword the allowlist permits ($ref,
// type, properties, required, additionalProperties, enum, const, oneOf,
// anyOf, items, format, pattern, minimum*, default, description,
// goJSONSchema) by routing the root itself through the SAME diffNode
// machinery a $defs entry goes through (C6/C7/C20: the root's own $ref --
// the entirety of session-config's contract -- and any other root
// keyword used to be walked by the allowlist but never actually
// compared).
//
// directive is this surface's own contracts/manifest.json "direction"
// value (by-suffix | platform-to-client | client-to-platform | both).
// Compare's caller resolves this from the MERGE-BASE manifest wherever a
// row for the surface exists there (C1 -- direction is a relaxation-
// adjacent decision, same as openEnums/retired, so it is never taken from
// HEAD for a surface the base already governs).
//
// A returned (non-nil) error is a genuine tool failure (malformed JSON):
// every OTHER kind of problem, including every fail-closed condition,
// comes back as a Finding with Severity SeverityFailClosed so the caller
// can keep comparing the other surfaces and report everything at once.
func DiffSurface(directive string, baseRaw, headRaw []byte, openEnums map[string]bool) ([]Finding, error) {
	var baseRoot, headRoot map[string]any
	if err := json.Unmarshal(baseRaw, &baseRoot); err != nil {
		return nil, fmt.Errorf("parse base schema: %w", err)
	}
	if err := json.Unmarshal(headRaw, &headRoot); err != nil {
		return nil, fmt.Errorf("parse head schema: %w", err)
	}

	var findings []Finding

	if err := walkSchema(baseRoot, "#", true); err != nil {
		if fc, ok := err.(*FailClosedError); ok {
			return append(findings, fc.Finding), nil
		}
		return nil, err
	}
	if err := walkSchema(headRoot, "#", true); err != nil {
		if fc, ok := err.(*FailClosedError); ok {
			return append(findings, fc.Finding), nil
		}
		return nil, err
	}

	// Row 36: root $schema changed -- fail closed unconditionally, and
	// stop: everything downstream assumes the same JSON Schema dialect.
	if fmt.Sprint(baseRoot["$schema"]) != fmt.Sprint(headRoot["$schema"]) {
		return []Finding{{
			RuleID:   "36",
			Severity: SeverityFailClosed,
			Pointer:  "#/$schema",
			Message:  "root $schema changed -- extend tools/contractscompat and its corpus in a separate PR first",
		}}, nil
	}

	// Row 34: root $id / root title -- administrative, root-only.
	if fmt.Sprint(baseRoot["$id"]) != fmt.Sprint(headRoot["$id"]) {
		findings = append(findings, Finding{RuleID: "34", Severity: SeverityMajor, Pointer: "#/$id", Message: "root $id changed"})
	}
	if fmt.Sprint(baseRoot["title"]) != fmt.Sprint(headRoot["title"]) {
		findings = append(findings, Finding{RuleID: "34", Severity: SeverityMajor, Pointer: "#/title", Message: "root title changed"})
	}

	baseDefs, _ := baseRoot["$defs"].(map[string]any)
	headDefs, _ := headRoot["$defs"].(map[string]any)
	if baseDefs == nil {
		baseDefs = map[string]any{}
	}
	if headDefs == nil {
		headDefs = map[string]any{}
	}

	// Rows 31/32: $defs entries added/removed, plus DiffDef on every
	// shared def.
	names := map[string]bool{}
	for n := range baseDefs {
		names[n] = true
	}
	for n := range headDefs {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	dirs, err := DefDirections(directive, baseDefs, headDefs)
	if err != nil {
		return nil, err
	}

	for _, name := range sorted {
		_, inBase := baseDefs[name]
		_, inHead := headDefs[name]
		ptr := "#/$defs/" + jsonPointerEscape(name)
		switch {
		case inBase && !inHead:
			findings = append(findings, Finding{RuleID: "31", Severity: SeverityMajor, Pointer: ptr, Message: "$defs entry removed"})
		case !inBase && inHead:
			findings = append(findings, Finding{RuleID: "32", Severity: SeverityMinor, Pointer: ptr, Message: "$defs entry added"})
		default:
			nested, err := DiffDef(baseDefs, headDefs, name, dirs[name], openEnums)
			if err != nil {
				if fc, ok := err.(*FailClosedError); ok {
					findings = append(findings, fc.Finding)
					continue
				}
				return nil, err
			}
			findings = append(findings, nested...)
		}
	}

	// Every root keyword besides the four administrative/structural ones
	// just handled ($schema, $id, title, $defs) is diffed by routing the
	// stripped root itself through diffNode -- the exact same machinery
	// every $defs entry goes through, including its own DirBoth
	// split-and-merge and its own exhaustiveness assertion.
	rootDir, err := rootDirection(directive)
	if err != nil {
		return nil, err
	}
	ctx := &diffCtx{baseR: resolver{defs: baseDefs}, headR: resolver{defs: headDefs}, openEnums: openEnums}
	rootFindings, err := ctx.diffNode(stripAdminKeys(baseRoot), stripAdminKeys(headRoot), rootDir, "#")
	if err != nil {
		if fc, ok := err.(*FailClosedError); ok {
			return append(findings, fc.Finding), nil
		}
		return nil, err
	}
	findings = append(findings, rootFindings...)

	return findings, nil
}

// rootDirection is the Direction the document ROOT itself is compared
// under. A fixed-direction surface's root gets that same direction -- it
// is what governs commands.schema.json/events.schema.json's own root
// oneOf, and session-config's own root $ref. A by-suffix surface's root
// has no single def-suffix to key off; rest/v1 and client-ws/v1 today
// carry nothing but $defs (plus description/$schema/$id/title) at the
// root, so this is dormant in practice, but DirBoth is the conservative
// default if a future root keyword ever appears there: it requires
// compatibility under BOTH columns rather than guessing one.
func rootDirection(directive string) (Direction, error) {
	if directive == DirectiveBySuffix {
		return DirBoth, nil
	}
	return fixedDirection(directive)
}

// stripAdminKeys returns a shallow copy of root without the four
// administrative/structural keywords ($schema, $id, title, $defs) that
// DiffSurface already compares on its own above -- everything else
// reaching diffNode/diffResolved is a genuine schema-constraint keyword
// those functions know how to classify (and will fail closed on
// otherwise, via their own exhaustiveness assertion).
func stripAdminKeys(root map[string]any) map[string]any {
	out := make(map[string]any, len(root))
	for k, v := range root {
		switch k {
		case "$schema", "$id", "title", "$defs":
			continue
		default:
			out[k] = v
		}
	}
	return out
}
