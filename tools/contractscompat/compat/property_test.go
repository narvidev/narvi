package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// This file is section 6's generic backstop against "a difference
// absorbed without a finding": a deterministic property test, run
// directly against the REAL /contracts files (not a synthetic fixture),
// that applies a bounded number of random structural mutations and
// asserts the tool never comes back with "no findings" for a document
// that actually changed. Every corpus case elsewhere in this package
// pins one SPECIFIC, hand-picked scenario; this test's whole point is to
// probe positions nobody thought to hand-pick.

// deepCopyJSON deep-copies a tree decoded by encoding/json
// (map[string]any/[]any/string/float64/bool/nil), so a mutation applied
// to the copy never touches the original.
func deepCopyJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = deepCopyJSON(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = deepCopyJSON(vv)
		}
		return out
	default:
		// strings/float64/bool/nil are immutable value types -- safe to
		// share between base and the mutated copy.
		return v
	}
}

// schemaPos is one schema-node position collectSchemaPositions found --
// exactly the positions walkSchema itself would walk (root, $defs/<name>,
// properties/<name>, items, a oneOf/anyOf member, additionalProperties
// when it is itself a schema).
type schemaPos struct {
	node map[string]any
}

// sortedMapKeys returns m's keys (of a map[string]any -- $defs or
// properties) in sorted order. E4: Go deliberately randomizes plain
// `range` order over a map on every process run; collectSchemaPositions
// and mutateSchemaTree's keyword-removal branch (below) both choose a
// position/keyword by rng-indexing into a slice built by ranging over
// such a map, so without sorting first, the exact same seed selects a
// DIFFERENT node or keyword on every run -- silently contradicting this
// file's own "deterministic, fixed-seed" claim.
func sortedMapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// collectSchemaPositions walks root the same way walkSchema does
// (keywords.go), collecting every schema-node position that is a
// map[string]any (skipping the boolean schema literals true/false, which
// have no keyword to mutate). The returned nodes are references into
// root's own tree -- mutating pos.node mutates root in place. $defs and
// properties are walked in SORTED key order (E4) so the resulting
// `positions` slice -- and therefore which position a given rng draw
// selects -- is identical on every run for the same input document.
func collectSchemaPositions(root map[string]any) []schemaPos {
	var out []schemaPos
	var walk func(node any)
	walk = func(node any) {
		obj, ok := node.(map[string]any)
		if !ok {
			return
		}
		out = append(out, schemaPos{obj})
		if defs, ok := obj["$defs"].(map[string]any); ok {
			for _, name := range sortedMapKeys(defs) {
				walk(defs[name])
			}
		}
		if props, ok := obj["properties"].(map[string]any); ok {
			for _, name := range sortedMapKeys(props) {
				walk(props[name])
			}
		}
		if items, ok := obj["items"]; ok {
			walk(items)
		}
		for _, kw := range []string{"oneOf", "anyOf"} {
			if arr, ok := obj[kw].([]any); ok {
				for _, sub := range arr {
					walk(sub)
				}
			}
		}
		if ap, ok := obj["additionalProperties"]; ok {
			if _, isBool := ap.(bool); !isBool {
				walk(ap)
			}
		}
	}
	walk(root)
	return out
}

// mutableLeafKeywords is the pool of allowlisted, non-structural
// keywords mutateSchemaTree draws from for a "change" or "add" mutation
// -- deliberately excluding the CONTAINER keywords ($defs, properties,
// items, oneOf, anyOf, $ref), which are only ever removed (case 1 below),
// never replaced with a synthetic value: synthesizing a plausible nested
// schema tree is unnecessary complexity this property test does not need
// ("respecting the allowlist" -- the task's own words -- is about which
// KEYWORDS a mutation may touch, not about exercising every keyword via
// every mutation kind).
var mutableLeafKeywords = []string{
	"description", "type", "required", "additionalProperties", "enum",
	"const", "format", "pattern", "minimum", "minLength", "minItems",
	"default", "goJSONSchema",
}

func randomLeafValue(rng *rand.Rand, kw string) any {
	switch kw {
	case "description":
		return []string{"changed description", "another note", ""}[rng.Intn(3)]
	case "type":
		pool := []any{"string", "integer", "number", "boolean", "null", "object", "array", []any{"string", "null"}, []any{"integer", "null"}}
		return pool[rng.Intn(len(pool))]
	case "required":
		pool := [][]any{{}, {"a"}, {"x", "y"}, {"zzzNewField"}}
		return pool[rng.Intn(len(pool))]
	case "additionalProperties":
		pool := []any{true, false, map[string]any{"type": "string"}}
		return pool[rng.Intn(len(pool))]
	case "enum":
		pool := [][]any{{"a", "b"}, {"x"}, {float64(1), float64(2)}, {nil}}
		return pool[rng.Intn(len(pool))]
	case "const":
		pool := []any{"a", float64(1), true, nil}
		return pool[rng.Intn(len(pool))]
	case "format":
		pool := []any{"date-time", "uuid", "email"}
		return pool[rng.Intn(len(pool))]
	case "pattern":
		pool := []any{"^[a-z]+$", "^[0-9]+$"}
		return pool[rng.Intn(len(pool))]
	case "minimum", "minLength", "minItems":
		return float64(rng.Intn(10))
	case "default":
		pool := []any{"d", float64(1), true, nil}
		return pool[rng.Intn(len(pool))]
	case "goJSONSchema":
		return map[string]any{"type": "time.Time", "imports": []any{"time"}}
	default:
		return "x"
	}
}

// mutateSchemaTree applies exactly ONE random mutation to headRoot
// (already a mutable deep copy of the real base document), touching only
// an allowlisted keyword: either one of the three GENERIC kinds below
// (change/add a leaf keyword at a random position; remove an existing
// allowlisted keyword; change a leaf keyword already present), or one of
// targetedMutationKinds (E6). The generic kinds probe broadly but, by
// construction, can never reliably produce several of the rule table's
// own change categories: they replace an "enum" array wholesale rather
// than adding/removing one VALUE from it; a "required" name they pick
// (from a small fixed pool) usually names no real property, so
// diffProperties' own orphan guard fails closed before the intended
// row 3/4 is ever reached; "properties" can only be dropped WHOLESALE,
// which orphans "required" the same way; and they never touch a single
// oneOf/anyOf MEMBER or a $ref's own TARGET at all. Each targeted kind
// below exists to close exactly one such gap; see its own doc comment.
func mutateSchemaTree(rng *rand.Rand, headRoot map[string]any) {
	positions := collectSchemaPositions(headRoot)
	if len(positions) == 0 {
		return
	}

	genericAddOrChange := func() {
		pos := positions[rng.Intn(len(positions))]
		kw := mutableLeafKeywords[rng.Intn(len(mutableLeafKeywords))]
		pos.node[kw] = randomLeafValue(rng, kw)
	}
	genericRemoveExisting := func() bool {
		pos := positions[rng.Intn(len(positions))]
		// Remove one EXISTING allowlisted keyword, at any position,
		// including a structural one ($ref, $defs, properties, items,
		// oneOf, anyOf) -- dropping $ref or properties entirely is a
		// legitimate, interesting mutation in its own right.
		var present []string
		for k := range pos.node {
			if allowedKeywords[k] {
				present = append(present, k)
			}
		}
		sort.Strings(present) // E4: deterministic selection order
		if len(present) == 0 {
			return false
		}
		delete(pos.node, present[rng.Intn(len(present))])
		return true
	}
	genericChangeExisting := func() bool {
		pos := positions[rng.Intn(len(positions))]
		// Change a leaf keyword ALREADY present at this node (biases the
		// mutation set toward "an existing constraint changed value,"
		// not only "a new one appeared").
		var present []string
		for _, kw := range mutableLeafKeywords {
			if _, ok := pos.node[kw]; ok {
				present = append(present, kw)
			}
		}
		if len(present) == 0 {
			return false
		}
		kw := present[rng.Intn(len(present))]
		pos.node[kw] = randomLeafValue(rng, kw)
		return true
	}

	total := 3 + len(targetedMutationKinds)
	switch choice := rng.Intn(total); choice {
	case 0:
		genericAddOrChange()
	case 1:
		if !genericRemoveExisting() {
			genericAddOrChange()
		}
	case 2:
		if !genericChangeExisting() {
			genericAddOrChange()
		}
	default:
		// E6: a targeted kind, engineered to produce ONE specific
		// rule-table change category the three generic kinds above
		// cannot reliably reach. Falls back to a generic mutation when
		// THIS file has no position the kind can apply to (e.g. a file
		// with no oneOf/anyOf at all cannot add a union member), rather
		// than silently doing nothing for this call.
		kind := targetedMutationKinds[choice-3]
		if !kind.apply(rng, headRoot, positions) {
			genericAddOrChange()
		}
	}
}

// mutationKind is one targeted, E6 mutation: apply reports whether it
// found a suitable position in THIS file and mutated headRoot in place
// (true), or found nothing to act on (false, headRoot untouched).
type mutationKind struct {
	name  string
	apply func(rng *rand.Rand, headRoot map[string]any, positions []schemaPos) bool
}

// targetedMutationKinds covers the rule-table change categories the
// generic kinds structurally cannot reach (E6): enum value add/remove
// (as opposed to replacing the whole array), required add/remove naming
// a REAL property (so it never fails closed as an orphan), property
// add/remove that never orphans `required`, a property added AND
// required in the same mutation (F7 -- row 3; see mutatePropertyAddRequired),
// a single oneOf/anyOf member add/remove, a $defs entry added (F7 -- row
// 32; see mutateDefAdd), and a $ref retargeted to another EXISTING def.
// `type` change/add/remove, `format`/`pattern`/`minimum`/`minLength`/
// `minItems` add/remove/change, `additionalProperties` changes,
// `description` change, and `default` change are all already reliably
// reachable through the three GENERIC kinds above -- none of those needs
// its own targeted kind (mutableLeafKeywords covers every one of those
// keywords, uniformly, at any position, regardless of whether it is
// already present).
//
// F7 (round 4 review): the property test's own doc comment used to claim
// this list reaches "every rule-table change kind" -- it did not. Row 3
// (property added AND required) was structurally impossible
// (mutatePropertyAdd never marked the new property required), row 32
// (a $defs entry added) had no mutation kind at all, and row 28 (a
// discriminated union variant added) could only ever be produced ALONGSIDE
// row 7 or buried under other findings (unionMemberAdd only ever
// appended a bare {"type":"boolean"}, which resolvedUnionMemberKey keys
// as a scalar-type widening, not a discriminated variant) -- so a mutant
// that disabled JUST that row's own emission line was never caught: the
// document this test mutates never produced row 3, 32, or (in isolation)
// row 28 at all. mutatePropertyAddRequired, mutateDefAdd, and
// unionMemberAdd's own new $ref-to-object-def branch close exactly those
// three gaps. See TestRealContractsMutationNeverSilentlyDropsAChange's
// own doc comment below for the EXACT measured kill fraction against a
// representative mutant sample -- not a blanket "every kind" claim.
var targetedMutationKinds = []mutationKind{
	{"enumValueAdd", func(rng *rand.Rand, _ map[string]any, positions []schemaPos) bool {
		return mutateEnumValueAdd(rng, positions)
	}},
	{"enumValueRemove", func(rng *rand.Rand, _ map[string]any, positions []schemaPos) bool {
		return mutateEnumValueRemove(rng, positions)
	}},
	{"requiredAdd", func(rng *rand.Rand, _ map[string]any, positions []schemaPos) bool {
		return mutateRequiredAdd(rng, positions)
	}},
	{"requiredRemove", func(rng *rand.Rand, _ map[string]any, positions []schemaPos) bool {
		return mutateRequiredRemove(rng, positions)
	}},
	{"propertyAdd", func(rng *rand.Rand, _ map[string]any, positions []schemaPos) bool {
		return mutatePropertyAdd(rng, positions)
	}},
	{"propertyAddRequired", func(rng *rand.Rand, _ map[string]any, positions []schemaPos) bool {
		return mutatePropertyAddRequired(rng, positions)
	}},
	{"propertyRemove", func(rng *rand.Rand, _ map[string]any, positions []schemaPos) bool {
		return mutatePropertyRemove(rng, positions)
	}},
	{"unionMemberAdd", mutateUnionMemberAdd},
	{"unionMemberRemove", func(rng *rand.Rand, _ map[string]any, positions []schemaPos) bool {
		return mutateUnionMemberRemove(rng, positions)
	}},
	{"refRetarget", mutateRefRetarget},
	{"defAdd", mutateDefAdd},
}

// mutateEnumValueAdd appends ONE brand-new value to an EXISTING "enum"
// array (never replacing the whole array, unlike the generic kinds) --
// the only way to reliably produce row 11 (enum value added) rather than
// a coin-flip mix of added/removed/both from a wholesale replacement.
func mutateEnumValueAdd(rng *rand.Rand, positions []schemaPos) bool {
	var candidates []schemaPos
	for _, p := range positions {
		if _, ok := p.node["enum"].([]any); ok {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return false
	}
	pos := candidates[rng.Intn(len(candidates))]
	arr, _ := pos.node["enum"].([]any)
	pool := []any{"zzz-generated-enum-value-1", "zzz-generated-enum-value-2", "zzz-generated-enum-value-3"}
	for _, v := range pool {
		dup := false
		for _, existing := range arr {
			if s, ok := existing.(string); ok && s == v {
				dup = true
				break
			}
		}
		if !dup {
			pos.node["enum"] = append(append([]any{}, arr...), v)
			return true
		}
	}
	return false
}

// mutateEnumValueRemove removes ONE existing value from an "enum" array
// (never replacing the whole array) -- reliably produces row 12 (enum
// value removed).
func mutateEnumValueRemove(rng *rand.Rand, positions []schemaPos) bool {
	var candidates []schemaPos
	for _, p := range positions {
		if arr, ok := p.node["enum"].([]any); ok && len(arr) > 0 {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return false
	}
	pos := candidates[rng.Intn(len(candidates))]
	arr, _ := pos.node["enum"].([]any)
	idx := rng.Intn(len(arr))
	out := append(append([]any{}, arr[:idx]...), arr[idx+1:]...)
	pos.node["enum"] = out
	return true
}

// mutateRequiredAdd adds an EXISTING property's own name to "required"
// (never a name from a fixed pool that may not exist here) -- the
// generic kinds' "required" pool ({"a"}, {"x","y"}, {"zzzNewField"}, ...)
// almost never names a real property of the node it lands on, so
// diffProperties' own fc-required-orphan guard fails closed before rows
// 3/4 are ever reached; naming a property this node ACTUALLY has avoids
// that entirely.
func mutateRequiredAdd(rng *rand.Rand, positions []schemaPos) bool {
	type candidate struct {
		pos   schemaPos
		names []string
	}
	var candidates []candidate
	for _, p := range positions {
		props, ok := p.node["properties"].(map[string]any)
		if !ok || len(props) == 0 {
			continue
		}
		req := stringSet(p.node["required"])
		var notRequired []string
		for _, name := range sortedMapKeys(props) {
			if !req[name] {
				notRequired = append(notRequired, name)
			}
		}
		if len(notRequired) > 0 {
			candidates = append(candidates, candidate{p, notRequired})
		}
	}
	if len(candidates) == 0 {
		return false
	}
	c := candidates[rng.Intn(len(candidates))]
	name := c.names[rng.Intn(len(c.names))]
	reqRaw, _ := c.pos.node["required"].([]any)
	c.pos.node["required"] = append(append([]any{}, reqRaw...), name)
	return true
}

// mutateRequiredRemove removes ONE existing name from "required" (the
// property itself stays declared in "properties") -- reliably produces
// row 5 (property removed from required) without orphaning anything.
func mutateRequiredRemove(rng *rand.Rand, positions []schemaPos) bool {
	var candidates []schemaPos
	for _, p := range positions {
		if req, ok := p.node["required"].([]any); ok && len(req) > 0 {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return false
	}
	pos := candidates[rng.Intn(len(candidates))]
	req, _ := pos.node["required"].([]any)
	idx := rng.Intn(len(req))
	out := append(append([]any{}, req[:idx]...), req[idx+1:]...)
	pos.node["required"] = out
	return true
}

// mutatePropertyAdd adds ONE brand-new, non-required property to an
// EXISTING "properties" map -- reliably produces row 2 (property added,
// not required) at a position the generic kinds never reach (they only
// ever ADD an already-allowlisted KEYWORD, never a new property NAME
// inside "properties").
func mutatePropertyAdd(rng *rand.Rand, positions []schemaPos) bool {
	var candidates []schemaPos
	for _, p := range positions {
		if _, ok := p.node["properties"].(map[string]any); ok {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return false
	}
	pos := candidates[rng.Intn(len(candidates))]
	props, _ := pos.node["properties"].(map[string]any)
	newProps := make(map[string]any, len(props)+1)
	for k, v := range props {
		newProps[k] = v
	}
	base := "zzzGeneratedProperty"
	name := base
	for i := 1; ; i++ {
		if _, exists := newProps[name]; !exists {
			break
		}
		name = fmt.Sprintf("%s%d", base, i)
	}
	newProps[name] = map[string]any{"type": "string"}
	pos.node["properties"] = newProps
	return true
}

// mutatePropertyAddRequired adds ONE brand-new property to an EXISTING
// "properties" map AND adds its name to "required" in the same mutation
// (F7) -- mutatePropertyAdd above deliberately never does this (it is
// what makes it reliably produce row 2's "not required" case), so row 3
// ("property added and required") was otherwise structurally impossible
// for the generator to produce at all: the generic kinds' "required"
// pool almost never names a property that ALSO doesn't already exist in
// "properties" on the same node in the same draw, and even a lucky hit
// there is still two independent random choices landing together, not a
// mutation that reliably targets row 3 the way this one does.
func mutatePropertyAddRequired(rng *rand.Rand, positions []schemaPos) bool {
	var candidates []schemaPos
	for _, p := range positions {
		if _, ok := p.node["properties"].(map[string]any); ok {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return false
	}
	pos := candidates[rng.Intn(len(candidates))]
	props, _ := pos.node["properties"].(map[string]any)
	newProps := make(map[string]any, len(props)+1)
	for k, v := range props {
		newProps[k] = v
	}
	base := "zzzGeneratedRequiredProperty"
	name := base
	for i := 1; ; i++ {
		if _, exists := newProps[name]; !exists {
			break
		}
		name = fmt.Sprintf("%s%d", base, i)
	}
	newProps[name] = map[string]any{"type": "string"}
	pos.node["properties"] = newProps
	reqRaw, _ := pos.node["required"].([]any)
	pos.node["required"] = append(append([]any{}, reqRaw...), name)
	return true
}

// mutatePropertyRemove removes ONE existing property that is NOT in
// "required" -- reliably produces row 1 (property removed) WITHOUT
// orphaning "required" the way the generic kinds' wholesale-only
// "properties" removal always does when any required name is present.
func mutatePropertyRemove(rng *rand.Rand, positions []schemaPos) bool {
	type candidate struct {
		pos   schemaPos
		names []string
	}
	var candidates []candidate
	for _, p := range positions {
		props, ok := p.node["properties"].(map[string]any)
		if !ok || len(props) == 0 {
			continue
		}
		req := stringSet(p.node["required"])
		var removable []string
		for _, name := range sortedMapKeys(props) {
			if !req[name] {
				removable = append(removable, name)
			}
		}
		if len(removable) > 0 {
			candidates = append(candidates, candidate{p, removable})
		}
	}
	if len(candidates) == 0 {
		return false
	}
	c := candidates[rng.Intn(len(candidates))]
	name := c.names[rng.Intn(len(c.names))]
	props, _ := c.pos.node["properties"].(map[string]any)
	newProps := make(map[string]any, len(props))
	for k, v := range props {
		if k != name {
			newProps[k] = v
		}
	}
	c.pos.node["properties"] = newProps
	return true
}

// mutateUnionMemberAdd appends a new member to an EXISTING oneOf/anyOf
// array -- EITHER a bare {"type":"boolean"} (reliably produces row 7, a
// non-null scalar-type member added/type widened) OR, when the file has
// a $defs entry with a bare "type":"object" not already referenced as a
// member of the CHOSEN array, a {"$ref": "#/$defs/<name>"} to it
// (reliably produces row 28, a discriminated object variant added --
// F7: before this, no mutation kind ever added a $ref-shaped member, so
// row 28 could only ever occur alongside whatever unrelated change
// {"type":"boolean"} itself produces (row 7), never in isolation -- a
// mutant disabling row 28's own emission was never caught, since the
// document it needed to silently mis-classify never occurred). Which of
// the two this call produces is picked at random each time a ref target
// is available, so row 7 stays reachable too. "boolean" is not a type
// any real union in these five files pairs on today, so appending it is
// very unlikely to collide with an existing member's own pairing key; on
// the rare chance either form does collide, diffUnion's own "duplicate
// pairing key" guard still fails closed, which the property test also
// accepts as "not silently dropped."
func mutateUnionMemberAdd(rng *rand.Rand, headRoot map[string]any, positions []schemaPos) bool {
	type candidate struct {
		pos schemaPos
		kw  string
	}
	var candidates []candidate
	for _, p := range positions {
		for _, kw := range []string{"oneOf", "anyOf"} {
			if _, ok := p.node[kw].([]any); ok {
				candidates = append(candidates, candidate{p, kw})
			}
		}
	}
	if len(candidates) == 0 {
		return false
	}
	c := candidates[rng.Intn(len(candidates))]
	arr, _ := c.pos.node[c.kw].([]any)

	if rng.Intn(2) == 0 {
		if refTarget, ok := pickUnreferencedObjectDef(rng, headRoot, arr); ok {
			c.pos.node[c.kw] = append(append([]any{}, arr...), map[string]any{"$ref": "#/$defs/" + refTarget})
			return true
		}
	}
	c.pos.node[c.kw] = append(append([]any{}, arr...), map[string]any{"type": "boolean"})
	return true
}

// pickUnreferencedObjectDef returns the name of a $defs entry whose own
// "type" is EXACTLY the bare string "object" (F2's own classification
// rule for a $ref union member to count as a discriminated variant) and
// that is not ALREADY a "$ref" member of existingMembers -- so appending
// it never collides with an existing member's own pairing key. Picks
// among all such candidates by index rng.Intn, for determinism (E4).
func pickUnreferencedObjectDef(rng *rand.Rand, headRoot map[string]any, existingMembers []any) (string, bool) {
	defs, ok := headRoot["$defs"].(map[string]any)
	if !ok {
		return "", false
	}
	already := map[string]bool{}
	for _, m := range existingMembers {
		if name := refTargetName(m); name != "" {
			already[name] = true
		}
	}
	var candidates []string
	for _, name := range sortedMapKeys(defs) {
		if already[name] {
			continue
		}
		def, ok := defs[name].(map[string]any)
		if !ok {
			continue
		}
		if t, isStr := def["type"].(string); isStr && t == "object" {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 0 {
		return "", false
	}
	return candidates[rng.Intn(len(candidates))], true
}

// mutateUnionMemberRemove removes ONE existing oneOf/anyOf member --
// reliably produces row 8/10/29 (whichever the removed member's own key
// resolves to).
func mutateUnionMemberRemove(rng *rand.Rand, positions []schemaPos) bool {
	type candidate struct {
		pos schemaPos
		kw  string
	}
	var candidates []candidate
	for _, p := range positions {
		for _, kw := range []string{"oneOf", "anyOf"} {
			if arr, ok := p.node[kw].([]any); ok && len(arr) > 0 {
				candidates = append(candidates, candidate{p, kw})
			}
		}
	}
	if len(candidates) == 0 {
		return false
	}
	c := candidates[rng.Intn(len(candidates))]
	arr, _ := c.pos.node[c.kw].([]any)
	idx := rng.Intn(len(arr))
	out := append(append([]any{}, arr[:idx]...), arr[idx+1:]...)
	c.pos.node[c.kw] = out
	return true
}

// mutateRefRetarget repoints an EXISTING "$ref" node at a DIFFERENT def
// that actually exists in headRoot's own $defs -- reliably produces row
// 27 ($ref retargeted). The generic kinds never touch a $ref's own VALUE
// at all (only ever delete the "$ref" keyword outright, which is a
// different change: $ref removed, not retargeted).
func mutateRefRetarget(rng *rand.Rand, headRoot map[string]any, positions []schemaPos) bool {
	defs, ok := headRoot["$defs"].(map[string]any)
	if !ok || len(defs) < 2 {
		return false
	}
	defNames := sortedMapKeys(defs)
	var candidates []schemaPos
	for _, p := range positions {
		if _, ok := p.node["$ref"].(string); ok {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return false
	}
	pos := candidates[rng.Intn(len(candidates))]
	curRef, _ := pos.node["$ref"].(string)
	var others []string
	for _, n := range defNames {
		target := "#/$defs/" + n
		if target != curRef {
			others = append(others, target)
		}
	}
	if len(others) == 0 {
		return false
	}
	pos.node["$ref"] = others[rng.Intn(len(others))]
	return true
}

// mutateDefAdd adds ONE brand-new, unreferenced entry to headRoot's own
// top-level "$defs" -- reliably produces row 32 ($defs entry added). No
// generic kind can ever produce this (they only ever touch an EXISTING
// position's own keywords, never add a new $defs NAME), and before F7 no
// targeted kind did either, so row 32 was structurally impossible for
// the generator to reach at all (confirmed unreachable at every seed
// tried in the round-4 review). The new def is deliberately never
// referenced by anything -- an unreferenced def is still a real, valid
// addition (row 32 does not require reachability), and leaving it
// unreferenced keeps this mutation from also silently changing some
// OTHER position's own pairing/direction behavior as a side effect.
func mutateDefAdd(rng *rand.Rand, headRoot map[string]any, _ []schemaPos) bool {
	defs, ok := headRoot["$defs"].(map[string]any)
	if !ok {
		return false
	}
	newDefs := make(map[string]any, len(defs)+1)
	for k, v := range defs {
		newDefs[k] = v
	}
	base := "ZzzGeneratedDef"
	name := base
	for i := 1; ; i++ {
		if _, exists := newDefs[name]; !exists {
			break
		}
		name = fmt.Sprintf("%s%d", base, i)
	}
	newDefs[name] = map[string]any{"type": "string"}
	headRoot["$defs"] = newDefs
	return true
}

// canonicalizeForComparison normalizes away the handful of JSON-Schema-
// level equivalences this checker's OWN rule table already treats as
// no-ops, so the property test's notion of "did anything change" matches
// the tool's, rather than raw byte equality:
//   - "additionalProperties": true is the same as the keyword being
//     absent (both "permissive") -- classifyAP's own apPermissive bucket
//     already merges these two shapes into one kind.
//   - "required": [] is the same as the keyword being absent (both
//     "nothing required") -- stringSet already treats a nil/absent
//     "required" and an empty array identically.
//
// Without this, a mutation that flips between these equivalent spellings
// would be a FALSE counterexample: the tool is CORRECT to report nothing
// for it, and flagging that as "silently dropped" would just be testing
// this file's own comparison, not the checker.
func canonicalizeForComparison(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = canonicalizeForComparison(vv)
		}
		if ap, ok := out["additionalProperties"]; ok {
			if b, isBool := ap.(bool); isBool && b {
				delete(out, "additionalProperties")
			}
		}
		if req, ok := out["required"]; ok {
			if arr, isArr := req.([]any); isArr && len(arr) == 0 {
				delete(out, "required")
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = canonicalizeForComparison(vv)
		}
		return out
	default:
		return v
	}
}

// TestRealContractsMutationNeverSilentlyDropsAChange is section 6's
// property test: fixed seed, bounded iterations (comfortably under 10s),
// run directly against the real /contracts files. Whenever a mutation's
// canonical JSON differs from the original, DiffSurface must report at
// least one finding of ANY severity (including FAIL-CLOSED, and a hard
// Compare-level error also counts -- both are "the checker did not
// silently accept this," which is the property under test). "No
// findings" is only ever correct when the canonical documents are
// actually equal.
//
// F7 (round 4 review) measured kill fraction: this test does NOT reach
// "every rule-table change kind" -- an earlier version of this comment
// implied that, and it was not true. A round-4 line-level mutation sweep
// against a representative sample of 41 emission sites across
// defdiff.go/rootdiff.go (each mutant neutering exactly ONE rule's own
// Finding-producing return/append statement, one at a time, restored
// between runs) found this test alone -- run in isolation, not the rest
// of the suite -- catches 23/41 (56.1%). The other 18 are still caught
// elsewhere in the suite (a corpus case in compat_test.go, or a
// dedicated round2/round3/round4 repro test) except where a corpus gap
// was itself the finding (F3's row 19 "removed" branch, now pinned).
// Structural reasons some rows can never reach this test at all: no
// mutation kind produces a bare boolean-schema-literal position or an
// additionalProperties-SCHEMA position (none exist in the real files
// today), the root-only administrative keywords ($id/title, row 34) are
// deliberately outside mutableLeafKeywords (this test mutates schema
// CONTENT, not document identity), and a handful of rows need a second,
// independent structural precondition (e.g. row 42 needs an
// additionalProperties schema to already exist at some position) that
// happens not to occur anywhere in the current five files. Re-run the
// sweep (or a similar one) after any future generator change and update
// this fraction rather than letting it go stale.
func TestRealContractsMutationNeverSilentlyDropsAChange(t *testing.T) {
	directives := map[string]string{
		"rest/v1/dtos.schema.json":                     DirectiveBySuffix,
		"client-ws/v1/protocol.schema.json":            DirectiveBySuffix,
		"sandbox-ws/v1/commands.schema.json":           string(DirP2C),
		"sandbox-ws/v1/events.schema.json":             string(DirBoth),
		"session-config/v1/session-config.schema.json": string(DirP2C),
	}

	const iterationsPerFile = 60 // 5 files * 60 = 300 total mutations (also run under `go test -race`, which is much slower)
	rng := rand.New(rand.NewSource(20260924))

	var failures int
	for _, path := range sortedKeys(directives) {
		directive := directives[path]
		raw, err := os.ReadFile(filepath.Join(repoContractsDir, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("read real %s: %v", path, err)
		}
		var baseRoot map[string]any
		if err := json.Unmarshal(raw, &baseRoot); err != nil {
			t.Fatalf("parse real %s: %v", path, err)
		}
		baseCanonical, err := json.Marshal(canonicalizeForComparison(baseRoot))
		if err != nil {
			t.Fatalf("marshal base %s: %v", path, err)
		}

		for i := 0; i < iterationsPerFile; i++ {
			headRootAny := deepCopyJSON(baseRoot)
			headRoot := headRootAny.(map[string]any)
			mutateSchemaTree(rng, headRoot)

			headBytes, err := json.Marshal(headRoot)
			if err != nil {
				t.Fatalf("marshal mutated %s (iteration %d): %v", path, i, err)
			}
			headCanonical, err := json.Marshal(canonicalizeForComparison(headRoot))
			if err != nil {
				t.Fatalf("canonicalize mutated %s (iteration %d): %v", path, i, err)
			}
			if bytes.Equal(baseCanonical, headCanonical) {
				continue // no-op mutation, or one this checker's own rules treat as equivalent
			}

			findings, diffErr := DiffSurface(directive, raw, headBytes, nil)
			if diffErr != nil {
				continue // a hard error also means "not silently accepted"
			}
			if len(findings) == 0 {
				failures++
				t.Errorf("%s iteration %d: mutation changed the canonical document but DiffSurface reported NO findings (silent absorption). Mutated document:\n%s", path, i, headBytes)
			}
		}
	}

	if failures > 0 {
		t.Fatalf("%d counterexample(s) found -- each must be fixed in the checker and kept as a corpus case (see this file's own doc comment)", failures)
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// TestRound3_E4_MutationGeneratorIsDeterministic pins the property
// test's own core promise: the SAME seed must produce the SAME
// mutation SEQUENCE on every run. Before E4, collectSchemaPositions and
// mutateSchemaTree's keyword-removal branch both chose a position/
// keyword by rng-indexing into a slice built by ranging over a Go map
// ($defs, properties, or a node's own keys) with no sort -- Go
// deliberately randomizes plain `range` order over a map on every
// process run, so the exact same seed selected a DIFFERENT node or
// keyword each time, even though the file's own doc comment and this
// PR's commit message both called the test "deterministic." Both call
// sites now go through sortedMapKeys/sort.Strings first.
func TestRound3_E4_MutationGeneratorIsDeterministic(t *testing.T) {
	const seed = 20260924
	const iterations = 40

	raw, err := os.ReadFile(filepath.Join(repoContractsDir, filepath.FromSlash("rest/v1/dtos.schema.json")))
	if err != nil {
		t.Fatalf("read real rest/v1/dtos.schema.json: %v", err)
	}
	var baseRoot map[string]any
	if err := json.Unmarshal(raw, &baseRoot); err != nil {
		t.Fatalf("parse: %v", err)
	}

	run := func() []string {
		rng := rand.New(rand.NewSource(seed))
		hashes := make([]string, 0, iterations)
		for i := 0; i < iterations; i++ {
			headRootAny := deepCopyJSON(baseRoot)
			headRoot := headRootAny.(map[string]any)
			mutateSchemaTree(rng, headRoot)
			// encoding/json sorts map keys when marshaling, so this is a
			// stable, byte-for-byte comparable fingerprint of the
			// mutated document regardless of Go's own internal map
			// iteration order.
			headBytes, err := json.Marshal(headRoot)
			if err != nil {
				t.Fatalf("marshal mutated document (iteration %d): %v", i, err)
			}
			hashes = append(hashes, string(headBytes))
		}
		return hashes
	}

	first := run()
	second := run()
	if len(first) != len(second) {
		t.Fatalf("want %d mutations on both runs, got %d and %d", iterations, len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("iteration %d: the SAME seed produced a DIFFERENT mutation on a second run (E4) --\nfirst:\n%s\nsecond:\n%s", i, first[i], second[i])
		}
	}
}
