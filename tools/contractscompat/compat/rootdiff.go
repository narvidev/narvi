package compat

import (
	"encoding/json"
	"fmt"
	"sort"
)

// DiffSurface compares one schema FILE's base and head bytes end to end:
// the keyword allowlist (fail-closed), the root's own $schema/$id/title/
// description (rows 34-36), the $defs set (rows 31-32), every def present
// on both sides (rows 1-30/33/35/39, via DefDirections + DiffDef), and --
// for commands.schema.json/events.schema.json, whose root itself is a
// oneOf -- rows 28-30 at the root.
//
// directive is this surface's own contracts/manifest.json "direction"
// value (by-suffix | platform-to-client | client-to-platform | both,
// read from HEAD's manifest -- direction assignment is not a relaxation,
// unlike openEnums/retired, so there is no anti-gaming reason to pin it
// to the merge-base). openEnums, by contrast, IS read from the
// merge-base manifest by the caller (see Compare), never from head.
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
		findings = append(findings, Finding{
			RuleID:   "36",
			Severity: SeverityFailClosed,
			Pointer:  "#/$schema",
			Message:  "root $schema changed -- extend tools/contractscompat and its corpus in a separate PR first",
		})
		return findings, nil
	}

	// Row 34: root $id / root title.
	if fmt.Sprint(baseRoot["$id"]) != fmt.Sprint(headRoot["$id"]) {
		findings = append(findings, Finding{RuleID: "34", Severity: SeverityMajor, Pointer: "#/$id", Message: "root $id changed"})
	}
	if fmt.Sprint(baseRoot["title"]) != fmt.Sprint(headRoot["title"]) {
		findings = append(findings, Finding{RuleID: "34", Severity: SeverityMajor, Pointer: "#/title", Message: "root title changed"})
	}
	// Row 35: root description (root never carries goJSONSchema).
	findings = append(findings, diffAnnotations(baseRoot, headRoot, "#")...)

	baseDefs, _ := baseRoot["$defs"].(map[string]any)
	headDefs, _ := headRoot["$defs"].(map[string]any)
	if baseDefs == nil {
		baseDefs = map[string]any{}
	}
	if headDefs == nil {
		headDefs = map[string]any{}
	}

	// Rows 31/32: $defs entries added/removed.
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

	// Root oneOf (commands.schema.json, events.schema.json): the fixed
	// surfaces (platform-to-client, both) also govern the root union
	// itself, not just what's inside each $defs entry.
	if directive != DirectiveBySuffix {
		rootDir, err := fixedDirection(directive)
		if err != nil {
			return nil, err
		}
		ctx := &diffCtx{baseR: resolver{defs: baseDefs}, headR: resolver{defs: headDefs}, openEnums: openEnums}
		var rootFindings []Finding
		if rootDir == DirBoth {
			p2c, err := ctx.diffUnion(baseRoot, headRoot, DirP2C, "#", "oneOf")
			if err != nil {
				if fc, ok := err.(*FailClosedError); ok {
					findings = append(findings, fc.Finding)
					return findings, nil
				}
				return nil, err
			}
			c2p, err := ctx.diffUnion(baseRoot, headRoot, DirC2P, "#", "oneOf")
			if err != nil {
				if fc, ok := err.(*FailClosedError); ok {
					findings = append(findings, fc.Finding)
					return findings, nil
				}
				return nil, err
			}
			rootFindings = mergeBothDirections(p2c, c2p)
		} else {
			rootFindings, err = ctx.diffUnion(baseRoot, headRoot, rootDir, "#", "oneOf")
			if err != nil {
				if fc, ok := err.(*FailClosedError); ok {
					findings = append(findings, fc.Finding)
					return findings, nil
				}
				return nil, err
			}
		}
		findings = append(findings, rootFindings...)
	}

	return findings, nil
}
