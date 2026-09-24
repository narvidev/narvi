package compat

import (
	"encoding/json"
	"fmt"
	"sort"
)

// validateStructure re-runs, against a SINGLE document (headRaw) entirely
// on its own, the structural invariants that otherwise only ever fire
// inside a genuine two-sided DiffSurface/DiffDef comparison: the
// oneOf/anyOf union-shape whitelist (unionShapeOf, via diffUnion),
// fc-required-orphan (diffProperties: a `required` name with no matching
// `properties` entry), and the boolean-$ref-target rejection
// (resolver.resolveDef/resolveDefName, reached through diffNode's own
// $ref-resolution step) -- plus, as a side effect of needing per-def
// Direction to call any of those at all, DefDirections' own
// fc-ref-sibling-reachability guard.
//
// Step 157 (review round 6 finding): none of those ever ran against a
// subtree that was ADDED rather than CHANGED. A brand-new schema file
// used to be walked by compare.go's own `!inBase && inHead` branch with
// nothing but walkSchema, which enforces the KEYWORD allowlist (which
// keywords may appear where) but not one of these three. A new $defs
// entry in an otherwise-already-reviewed file fares no better:
// DiffSurface's own row-32 branch ("$defs entry added") records the
// addition and moves on -- it never calls DiffDef on the new entry's
// content. Neither does a new property on an existing def:
// diffProperties' own rows-2/3 branch ("property added") records the
// addition and moves on -- it never recurses diffNode into the new
// property's own schema. So an illegal shape could be admitted through
// any of those three paths with nothing to say about it, and the FIRST
// thing to ever discover it was whichever LATER PR happened to touch it
// from a genuinely two-sided diff -- which then fails closed on content
// that PR did not introduce (safe, never a bypass, but it stranded the
// file for an unrelated change to trip over). Compare now calls
// validateStructure on every file present in HEAD, new or already
// existing, so a violation is named -- and fails closed -- in the PR that
// actually introduces it.
//
// Why not a literal self-diff (DiffSurface(head, head))? diffResolved's
// own first line exists so re-diffing an untouched sibling is cheap; it
// is exactly right for a real base/head comparison, but it means
// comparing a document against a byte-for-byte copy of itself is a
// silent no-op -- every node trivially equals itself, so diffResolved
// returns before diffProperties, diffUnion, or resolve()/resolveDef()
// ever runs below the outermost node of each def/root. validateStructure
// instead drives the exact same diffNode/diffProperties/diffUnion/
// resolver machinery DiffSurface itself uses, through a diffCtx with
// selfCheck set (see that field's own doc comment) so the walk actually
// recurses into every property, def, and union member -- never a second
// implementation of what those functions check, only a different way of
// invoking them.
//
// Only SeverityFailClosed findings are kept: every other row this walk
// could otherwise produce (e.g. row 30, "union member changed") compares
// HEAD against itself, so it can never legitimately fire -- and if some
// future edit ever made one, it would only be noise, not something the
// PR being checked caused.
func validateStructure(directive string, headRaw []byte, openEnums map[string]bool) ([]Finding, error) {
	var headRoot map[string]any
	if err := json.Unmarshal(headRaw, &headRoot); err != nil {
		return nil, fmt.Errorf("parse head schema: %w", err)
	}

	headDefs, _ := headRoot["$defs"].(map[string]any)
	if headDefs == nil {
		headDefs = map[string]any{}
	}

	// Both sides of DefDirections are HEAD's own $defs -- there is only
	// one document here -- so its reachability computation runs entirely
	// over head's own content, same as a real diff's would if base and
	// head happened to be identical.
	dirs, err := DefDirections(directive, headDefs, headDefs)
	if err != nil {
		if fc, ok := err.(*FailClosedError); ok {
			return []Finding{fc.Finding}, nil
		}
		return nil, err
	}

	names := make([]string, 0, len(headDefs))
	for n := range headDefs {
		names = append(names, n)
	}
	sort.Strings(names)

	var raw []Finding
	for _, name := range names {
		fs, err := diffDefSelf(headDefs, name, dirs[name], openEnums)
		if err != nil {
			if fc, ok := err.(*FailClosedError); ok {
				raw = append(raw, fc.Finding)
				continue
			}
			return nil, err
		}
		raw = append(raw, fs...)
	}

	rootDir, err := rootDirection(directive)
	if err != nil {
		return nil, err
	}
	stripped := stripAdminKeys(headRoot)
	rootCtx := &diffCtx{
		baseR:     resolver{defs: headDefs},
		headR:     resolver{defs: headDefs},
		openEnums: openEnums,
		selfCheck: true,
	}
	rootFindings, err := rootCtx.diffNode(stripped, stripped, rootDir, loc{ptr: "#", defPtr: "#", oldDefPtr: "#"})
	if err != nil {
		if fc, ok := err.(*FailClosedError); ok {
			raw = append(raw, fc.Finding)
		} else {
			return nil, err
		}
	} else {
		raw = append(raw, rootFindings...)
	}

	var out []Finding
	for _, f := range raw {
		if f.Severity == SeverityFailClosed {
			out = append(out, f)
		}
	}
	return out, nil
}

// diffDefSelf mirrors DiffDef's own setup (see its doc comment for why
// the recursion guard is pre-seeded with this def's own identity before
// the first "$ref": name node inside it can be reached) but diffs the def
// against ITSELF, with diffCtx.selfCheck set so the walk actually
// recurses into content that compares equal to itself -- see
// validateStructure's own doc comment. The rule logic invoked along the
// way (diffProperties, diffUnion, resolve/resolveDef, ...) is exactly
// DiffDef's own; only the ctx construction is duplicated here, not any
// check.
func diffDefSelf(defs map[string]any, name string, dir Direction, openEnums map[string]bool) ([]Finding, error) {
	ctx := &diffCtx{
		baseR:     resolver{defs: defs},
		headR:     resolver{defs: defs},
		openEnums: openEnums,
		selfCheck: true,
	}
	if dir == DirBoth {
		ctx.visited = map[visitKey]bool{{name, name, DirP2C}: true, {name, name, DirC2P}: true}
	} else {
		ctx.visited = map[visitKey]bool{{name, name, dir}: true}
	}
	ptr := "#/$defs/" + jsonPointerEscape(name)
	node := defs[name]
	return ctx.diffNode(node, node, dir, loc{ptr: ptr, defPtr: ptr, oldDefPtr: ptr})
}
