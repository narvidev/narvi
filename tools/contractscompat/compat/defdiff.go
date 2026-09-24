package compat

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// diffCtx carries everything a single def-pair comparison needs beyond the
// two schema nodes themselves: each side's own $defs (for $ref
// resolution) and the MERGE-BASE manifest's openEnums set (§6.3 design
// spec §3: "relaxations are read from the MERGE-BASE manifest only").
type diffCtx struct {
	baseR     resolver
	headR     resolver
	openEnums map[string]bool
}

// DiffDef compares one def (already looked up in both sides' $defs maps)
// under a single Direction -- or, when dir is DirBoth, requiring
// compatibility under BOTH the P2C and C2P columns (§6.3 design spec §2)
// -- by delegating straight to diffNode, which implements the DirBoth
// split-and-merge itself (see diffNode's own doc comment for why pushing
// that down makes the root-level diff (rootdiff.go) able to reuse the
// exact same machinery instead of duplicating it).
func DiffDef(baseDefs, headDefs map[string]any, name string, dir Direction, openEnums map[string]bool) ([]Finding, error) {
	ctx := &diffCtx{
		baseR:     resolver{defs: baseDefs},
		headR:     resolver{defs: headDefs},
		openEnums: openEnums,
	}
	ptr := "#/$defs/" + jsonPointerEscape(name)
	base, head := baseDefs[name], headDefs[name]
	return ctx.diffNode(base, head, dir, ptr)
}

// mergeBothDirections combines the two per-column readings of a DirBoth
// node: a finding present under only one column still applies (that
// column's role is real), and a finding present under both (same rule +
// pointer) keeps the worse of the two severities.
func mergeBothDirections(p2c, c2p []Finding) []Finding {
	type key struct{ rule, ptr string }
	byKey := map[key]Finding{}
	order := []key{}
	for _, f := range append(append([]Finding{}, p2c...), c2p...) {
		k := key{f.RuleID, f.Pointer}
		if existing, ok := byKey[k]; ok {
			existing.Severity = maxSeverity(existing.Severity, f.Severity)
			byKey[k] = existing
		} else {
			byKey[k] = f
			order = append(order, k)
		}
	}
	out := make([]Finding, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return out
}

// diffNode is the recursive engine behind rows 1-30, 33, 35, 39, 42, 43 of
// the rule table, AND the mechanism the root document itself is diffed
// through (rootdiff.go) -- there is exactly one place in this package that
// knows how to compare two schema nodes, whether that node is a $defs
// entry, a nested property, a oneOf/anyOf member, or a whole file's root.
//
// dir == DirBoth is handled by splitting into one DirP2C and one DirC2P
// pass over the SAME base/head pair and merging (mergeBothDirections),
// entirely at the point where DirBoth was first given -- every recursive
// call this makes passes a concrete DirP2C/DirC2P down, so a DirBoth node
// is never re-split at every level of its own subtree (that would be
// exponential; it would also be wrong, since Direction is a property of a
// whole def/root, not of an individual nested node).
//
// $ref handling (rows 27, 31-32's caller): a same-target (or no-$ref-on-
// either-side) pair is resolved and its SIBLING keywords merged in via
// effectiveNode (C5, C8 -- a node carrying "$ref" plus other keywords is
// walked like any other node, never with its siblings silently dropped).
// A retargeted $ref (row 27) is handled by diffRetargetedRef, which does
// the same sibling-merge against each side's OWN resolved target.
func (c *diffCtx) diffNode(base, head any, dir Direction, ptr string) ([]Finding, error) {
	if dir == DirBoth {
		p2c, err := c.diffNode(base, head, DirP2C, ptr)
		if err != nil {
			return nil, err
		}
		c2p, err := c.diffNode(base, head, DirC2P, ptr)
		if err != nil {
			return nil, err
		}
		return mergeBothDirections(p2c, c2p), nil
	}

	bRefName := refTargetName(base)
	hRefName := refTargetName(head)

	if bRefName != "" && hRefName != "" && bRefName != hRefName {
		return c.diffRetargetedRef(base, head, bRefName, hRefName, dir, ptr)
	}

	rBase, err := c.baseR.resolve(base, nil)
	if err != nil {
		return nil, failClosed("fc-ref", ptr, "%v", err)
	}
	rHead, err := c.headR.resolve(head, nil)
	if err != nil {
		return nil, failClosed("fc-ref", ptr, "%v", err)
	}

	bObj, bIsObj := rBase.(map[string]any)
	hObj, hIsObj := rHead.(map[string]any)
	if !bIsObj || !hIsObj {
		// At least one side resolves to the boolean schema literal
		// true/false. Compare the literals directly rather than through
		// effectiveNode: collapsing a bare `false` ("reject everything")
		// into the empty object `{}` ("accept everything") there would
		// silently invert its meaning. A bool node can never itself carry
		// $ref siblings (its whole value IS the boolean), so there is
		// nothing to merge in this case anyway.
		if reflect.DeepEqual(rBase, rHead) {
			return nil, nil
		}
		return []Finding{ruleFinding("6", dir, majorMajor, ptr, "schema literal changed")}, nil
	}

	mergedBase := effectiveNode(bObj, base)
	mergedHead := effectiveNode(hObj, head)
	return c.diffResolved(mergedBase, mergedHead, dir, ptr)
}

// diffRetargetedRef implements row 27: the finding's severity comes from
// comparing the OLD target's content (as it stood in base, plus base's own
// sibling keywords) against the NEW target's content (as it stands in
// head, plus head's own siblings), unless the old target no longer exists
// in head's own $defs at all (row 31 territory), which is unconditionally
// MAJOR.
func (c *diffCtx) diffRetargetedRef(base, head any, bRefName, hRefName string, dir Direction, ptr string) ([]Finding, error) {
	if _, ok := c.headR.defs[bRefName]; !ok {
		return []Finding{{
			RuleID:   "27",
			Severity: SeverityMajor,
			Pointer:  ptr,
			Message:  fmt.Sprintf("$ref retargeted from %q to %q, and %q no longer exists", bRefName, hRefName, bRefName),
		}}, nil
	}

	oldTarget, err := c.baseR.resolve(c.baseR.defs[bRefName], nil)
	if err != nil {
		return nil, failClosed("fc-ref", ptr, "%v", err)
	}
	newTarget, err := c.headR.resolve(c.headR.defs[hRefName], nil)
	if err != nil {
		return nil, failClosed("fc-ref", ptr, "%v", err)
	}

	var nested []Finding
	oldObj, oldIsObj := oldTarget.(map[string]any)
	newObj, newIsObj := newTarget.(map[string]any)
	switch {
	case !oldIsObj || !newIsObj:
		if !reflect.DeepEqual(oldTarget, newTarget) {
			nested = []Finding{ruleFinding("6", dir, majorMajor, ptr, "schema literal changed")}
		}
	default:
		mergedOld := effectiveNode(oldObj, base)
		mergedNew := effectiveNode(newObj, head)
		nested, err = c.diffResolved(mergedOld, mergedNew, dir, ptr)
		if err != nil {
			return nil, err
		}
	}

	worst := SeverityPatch
	for _, f := range nested {
		worst = maxSeverity(worst, f.Severity)
	}
	wrapper := Finding{
		RuleID:   "27",
		Severity: worst,
		Pointer:  ptr,
		Message:  fmt.Sprintf("$ref retargeted from %q to %q", bRefName, hRefName),
	}
	return []Finding{wrapper}, nil
}

// diffResolved compares two already-dereferenced-and-sibling-merged
// schema nodes (see effectiveNode). This is the ONE place every
// allowlisted schema-constraint keyword (everything except the
// root-only/administrative $schema, $id, title, $defs, which DiffSurface
// handles itself before ever calling into diffNode for the root) must be
// dispatched to a rule handler: every handler below marks the keyword(s)
// it looked at as "consumed" regardless of whether it found a
// difference, and the exhaustiveness assertion at the bottom fails closed
// if ANY key present on either side was never consumed by anything --
// the structural backstop against exactly the class of bug this rewrite
// exists to close (a keyword nobody thought to compare passing through
// silently).
func (c *diffCtx) diffResolved(bObj, hObj map[string]any, dir Direction, ptr string) ([]Finding, error) {
	if reflect.DeepEqual(bObj, hObj) {
		// Nothing changed at or under this node: skip it entirely, rather
		// than run e.g. oneOf/anyOf pairing on content nobody touched. A
		// pairing heuristic that cannot key every member of some
		// untouched, pre-existing union would otherwise fail closed on
		// files that have never changed at all.
		return nil, nil
	}

	consumed := map[string]bool{}
	mark := func(keys ...string) {
		for _, k := range keys {
			consumed[k] = true
		}
	}

	var findings []Finding

	findings = append(findings, diffAnnotations(bObj, hObj, ptr)...)
	mark("description", "goJSONSchema")

	typeFindings, err := c.diffType(bObj, hObj, dir, ptr)
	if err != nil {
		return nil, err
	}
	findings = append(findings, typeFindings...)
	mark("type")

	findings = append(findings, c.diffEnum(bObj, hObj, dir, ptr)...)
	mark("enum")
	findings = append(findings, diffConst(bObj, hObj, ptr)...)
	mark("const")
	findings = append(findings, diffFormat(bObj, hObj, dir, ptr)...)
	mark("format")
	findings = append(findings, diffPattern(bObj, hObj, dir, ptr)...)
	mark("pattern")
	findings = append(findings, diffNumericFloor(bObj, hObj, dir, ptr, "minimum")...)
	mark("minimum")
	findings = append(findings, diffNumericFloor(bObj, hObj, dir, ptr, "minLength")...)
	mark("minLength")
	findings = append(findings, diffNumericFloor(bObj, hObj, dir, ptr, "minItems")...)
	mark("minItems")
	findings = append(findings, diffDefault(bObj, hObj, ptr)...)
	mark("default")

	propFindings, err := c.diffProperties(bObj, hObj, dir, ptr)
	if err != nil {
		return nil, err
	}
	findings = append(findings, propFindings...)
	mark("properties", "required")

	apFindings, err := c.diffAdditionalProperties(bObj, hObj, dir, ptr)
	if err != nil {
		return nil, err
	}
	findings = append(findings, apFindings...)
	mark("additionalProperties")

	itemsFindings, err := c.diffItems(bObj, hObj, dir, ptr)
	if err != nil {
		return nil, err
	}
	findings = append(findings, itemsFindings...)
	mark("items")

	for _, kw := range []string{"oneOf", "anyOf"} {
		fs, err := c.diffUnion(bObj, hObj, dir, ptr, kw)
		if err != nil {
			return nil, err
		}
		findings = append(findings, fs...)
		mark(kw)
	}

	// Exhaustiveness assertion: every keyword present on either side of
	// this (already sibling-merged) node must have been consumed by a
	// handler above. This is deliberately redundant with the keyword
	// allowlist (walkSchema/keywords.go) for everything except the four
	// root-only keywords -- it is the belt to that allowlist's suspenders,
	// so a future handler that gets deleted, or a keyword that reaches
	// this node in a position the allowlist did not anticipate (a nested
	// $defs/$id/$schema slipping past a walkSchema regression, say),
	// fails the run rather than passing it silently.
	var leftover []string
	for k := range bObj {
		if !consumed[k] {
			leftover = append(leftover, k)
		}
	}
	for k := range hObj {
		if !consumed[k] {
			leftover = append(leftover, k)
		}
	}
	if len(leftover) > 0 {
		sort.Strings(leftover)
		leftover = dedupSorted(leftover)
		return nil, failClosed("fc-unhandled-keyword", ptr, "keyword(s) %v present at this node were not classified by any rule handler", leftover)
	}

	return findings, nil
}

func dedupSorted(in []string) []string {
	out := in[:0]
	var prev string
	for i, s := range in {
		if i == 0 || s != prev {
			out = append(out, s)
		}
		prev = s
	}
	return out
}

// severityPair is a (P2C, C2P) severity pair for a rule row that does not
// depend on any extra condition.
type severityPair struct{ p2c, c2p Severity }

var majorMajor = severityPair{SeverityMajor, SeverityMajor}

func (p severityPair) at(dir Direction) Severity {
	if dir == DirC2P {
		return p.c2p
	}
	return p.p2c
}

func ruleFinding(ruleID string, dir Direction, sev severityPair, ptr, msg string) Finding {
	return Finding{RuleID: ruleID, Severity: sev.at(dir), Pointer: ptr, Message: msg}
}

// --- annotations: description (row 35), goJSONSchema (row 45) ---

// diffAnnotations covers row 35 ("description changed", PATCH) and row 45
// ("goJSONSchema changed", MAJOR both columns -- C19: go-jsonschema's own
// codegen-hint extension steers this repository's OWN generated Go
// decoder type for a property; for a client-to-platform shape the
// platform is the consumer, so a narrower generated Go type can reject
// input the schema itself still describes as valid. Scored as a break
// rather than an annotation, unlike description).
func diffAnnotations(base, head map[string]any, ptr string) []Finding {
	var findings []Finding
	if f := diffAnnotationKey(base, head, ptr, "description", "35", SeverityPatch); f != nil {
		findings = append(findings, *f)
	}
	if f := diffAnnotationKey(base, head, ptr, "goJSONSchema", "45", SeverityMajor); f != nil {
		findings = append(findings, *f)
	}
	return findings
}

// diffAnnotationKey covers a single not-direction-graded keyword (row 35's
// description is PATCH regardless of P2C/C2P; row 45's goJSONSchema is
// MAJOR regardless) -- sev is used for both columns alike.
func diffAnnotationKey(base, head map[string]any, ptr, key, ruleID string, sev Severity) *Finding {
	b, bok := base[key]
	h, hok := head[key]
	if bok == hok && reflect.DeepEqual(b, h) {
		return nil
	}
	if !bok && !hok {
		return nil
	}
	return &Finding{RuleID: ruleID, Severity: sev, Pointer: ptr + "/" + key, Message: key + " changed"}
}

// --- type (rows 6, 7, 8, 9, 10) ---

func typeSet(obj map[string]any) map[string]bool {
	out := map[string]bool{}
	switch v := obj["type"].(type) {
	case string:
		out[v] = true
	case []any:
		for _, el := range v {
			if s, ok := el.(string); ok {
				out[s] = true
			}
		}
	}
	return out
}

func (c *diffCtx) diffType(base, head map[string]any, dir Direction, ptr string) ([]Finding, error) {
	_, bHas := base["type"]
	_, hHas := head["type"]
	if !bHas && !hHas {
		return nil, nil
	}
	if bHas != hHas {
		// C2/C12/C14: the `type` keyword's own PRESENCE changing is
		// scored MAJOR in both columns, regardless of what the
		// surviving/incoming value says. Dropping `type` removes a
		// constraint entirely (the value may now be any JSON type, which
		// the old row-8 "narrowed" bucket scored backwards, as MINOR on
		// P2C); adding it narrows what was previously unconstrained. This
		// is deliberately coarser than rows 7-10's per-value grading --
		// exactly because a presence change is also how a field can be
		// rewritten as an equivalent anyOf/oneOf union (see diffUnion's
		// own presence-change rule, 43), and erring MAJOR on the `type`
		// side of that rewrite is the fail-closed choice, not a
		// precision bug.
		return []Finding{ruleFinding("6", dir, majorMajor, ptr+"/type", "type keyword presence changed")}, nil
	}

	bSet, hSet := typeSet(base), typeSet(head)

	var added, removed []string
	for t := range hSet {
		if !bSet[t] {
			added = append(added, t)
		}
	}
	for t := range bSet {
		if !hSet[t] {
			removed = append(removed, t)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)

	if len(added) == 0 && len(removed) == 0 {
		return nil, nil
	}

	msg := fmt.Sprintf("type changed (added=%v removed=%v)", added, removed)

	switch {
	case len(removed) == 0 && len(added) == 1 && added[0] == "null":
		// row 9: null added
		return []Finding{ruleFinding("9", dir, severityPair{SeverityMajor, SeverityMinor}, ptr+"/type", msg)}, nil
	case len(added) == 0 && len(removed) == 1 && removed[0] == "null":
		// row 10: null removed
		return []Finding{ruleFinding("10", dir, severityPair{SeverityMinor, SeverityMajor}, ptr+"/type", msg)}, nil
	case len(removed) == 0 && len(added) > 0:
		// row 7: widened (union gains a member, not exclusively null)
		return []Finding{ruleFinding("7", dir, severityPair{SeverityMajor, SeverityMinor}, ptr+"/type", msg)}, nil
	case len(added) == 0 && len(removed) > 0:
		// row 8: narrowed
		return []Finding{ruleFinding("8", dir, severityPair{SeverityMinor, SeverityMajor}, ptr+"/type", msg)}, nil
	default:
		// row 6: outright type change (some added AND some removed)
		return []Finding{ruleFinding("6", dir, majorMajor, ptr+"/type", msg)}, nil
	}
}

// --- enum (rows 11, 12, 13) ---

func (c *diffCtx) diffEnum(base, head map[string]any, dir Direction, ptr string) []Finding {
	bRaw, bHas := base["enum"]
	hRaw, hHas := head["enum"]

	if !bHas && !hHas {
		return nil
	}
	if bHas && !hHas {
		// row 13, removed
		return []Finding{ruleFinding("13", dir, severityPair{SeverityMajor, SeverityMinor}, ptr+"/enum", "enum keyword removed")}
	}
	if !bHas && hHas {
		// row 13, added
		return []Finding{ruleFinding("13", dir, severityPair{SeverityMinor, SeverityMajor}, ptr+"/enum", "enum keyword added")}
	}

	bVals, _ := bRaw.([]any)
	hVals, _ := hRaw.([]any)
	bSet := map[string]any{}
	for _, v := range bVals {
		bSet[fmt.Sprint(v)] = v
	}
	hSet := map[string]any{}
	for _, v := range hVals {
		hSet[fmt.Sprint(v)] = v
	}

	var findings []Finding
	openName := canonicalName(ptr)
	isOpen := c.openEnums[openName]

	var addedKeys, removedKeys []string
	for k := range hSet {
		if _, ok := bSet[k]; !ok {
			addedKeys = append(addedKeys, k)
		}
	}
	for k := range bSet {
		if _, ok := hSet[k]; !ok {
			removedKeys = append(removedKeys, k)
		}
	}
	sort.Strings(addedKeys)
	sort.Strings(removedKeys)

	for _, k := range addedKeys {
		p2c := SeverityMajor
		if isOpen {
			p2c = SeverityMinor
		}
		findings = append(findings, ruleFinding("11", dir, severityPair{p2c, SeverityMinor}, ptr+"/enum", "enum value added: "+k))
	}
	for _, k := range removedKeys {
		findings = append(findings, ruleFinding("12", dir, severityPair{SeverityMinor, SeverityMajor}, ptr+"/enum", "enum value removed: "+k))
	}
	return findings
}

// canonicalName turns a def-rooted JSON Pointer such as
// "#/$defs/Session/properties/status" into the dotted name
// ("Session.status") the day-one openEnums list (contracts/manifest.json)
// and COMPATIBILITY.md both use, by dropping "$defs" and every literal
// "properties" path segment.
func canonicalName(ptr string) string {
	ptr = strings.TrimPrefix(ptr, "#/")
	segs := strings.Split(ptr, "/")
	var kept []string
	for i, s := range segs {
		if s == "$defs" || s == "properties" {
			continue
		}
		// Drop the trailing "/enum" (or any other keyword suffix): keep
		// only up to and including the property/def name segments.
		if i == len(segs)-1 && !isNameSegment(s) {
			continue
		}
		kept = append(kept, jsonPointerUnescape(s))
	}
	return strings.Join(kept, ".")
}

// isNameSegment heuristically distinguishes a $defs/properties NAME
// segment from a trailing keyword segment (like "enum" or "type") when
// canonicalName strips the pointer down to a dotted name -- every keyword
// this checker ever appends as a final segment is in the allowlist, so
// anything else is assumed to be a real name.
func isNameSegment(s string) bool {
	return !allowedKeywords[s]
}

// --- const (row 14) ---

func diffConst(base, head map[string]any, ptr string) []Finding {
	b, bHas := base["const"]
	h, hHas := head["const"]
	if bHas == hHas && reflect.DeepEqual(b, h) {
		return nil
	}
	if !bHas && !hHas {
		return nil
	}
	return []Finding{ruleFinding("14", DirP2C, majorMajor, ptr+"/const", "const added/removed/changed")}
}

// --- format (rows 15, 16, 17) ---

func diffFormat(base, head map[string]any, dir Direction, ptr string) []Finding {
	b, bHas := base["format"].(string)
	h, hHas := head["format"].(string)
	switch {
	case !bHas && hHas:
		return []Finding{ruleFinding("15", dir, severityPair{SeverityMinor, SeverityMajor}, ptr+"/format", "format added: "+h)}
	case bHas && !hHas:
		return []Finding{ruleFinding("16", dir, severityPair{SeverityMajor, SeverityMinor}, ptr+"/format", "format removed: "+b)}
	case bHas && hHas && b != h:
		return []Finding{ruleFinding("17", dir, majorMajor, ptr+"/format", fmt.Sprintf("format changed: %s -> %s", b, h))}
	default:
		return nil
	}
}

// --- pattern (rows 20, 21, 22) ---

func diffPattern(base, head map[string]any, dir Direction, ptr string) []Finding {
	b, bHas := base["pattern"].(string)
	h, hHas := head["pattern"].(string)
	switch {
	case !bHas && hHas:
		return []Finding{ruleFinding("20", dir, severityPair{SeverityMinor, SeverityMajor}, ptr+"/pattern", "pattern added: "+h)}
	case bHas && !hHas:
		return []Finding{ruleFinding("21", dir, severityPair{SeverityMajor, SeverityMinor}, ptr+"/pattern", "pattern removed: "+b)}
	case bHas && hHas && b != h:
		return []Finding{ruleFinding("22", dir, majorMajor, ptr+"/pattern", fmt.Sprintf("pattern changed: %s -> %s", b, h))}
	default:
		return nil
	}
}

// --- minimum/minLength/minItems (rows 18, 19) ---

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	default:
		return 0, false
	}
}

func diffNumericFloor(base, head map[string]any, dir Direction, ptr, keyword string) []Finding {
	bRaw, bHas := base[keyword]
	hRaw, hHas := head[keyword]
	switch {
	case !bHas && hHas:
		return []Finding{ruleFinding("18", dir, severityPair{SeverityMinor, SeverityMajor}, ptr+"/"+keyword, keyword+" added (floor raised from none)")}
	case bHas && !hHas:
		return []Finding{ruleFinding("19", dir, severityPair{SeverityMajor, SeverityMinor}, ptr+"/"+keyword, keyword+" removed (floor lowered to none)")}
	case bHas && hHas:
		b, _ := asFloat(bRaw)
		h, _ := asFloat(hRaw)
		switch {
		case h > b:
			return []Finding{ruleFinding("18", dir, severityPair{SeverityMinor, SeverityMajor}, ptr+"/"+keyword, fmt.Sprintf("%s raised %v -> %v", keyword, b, h))}
		case h < b:
			return []Finding{ruleFinding("19", dir, severityPair{SeverityMajor, SeverityMinor}, ptr+"/"+keyword, fmt.Sprintf("%s lowered %v -> %v", keyword, b, h))}
		}
	}
	return nil
}

// --- default (row 33) ---

func diffDefault(base, head map[string]any, ptr string) []Finding {
	b, bHas := base["default"]
	h, hHas := head["default"]
	if bHas == hHas && reflect.DeepEqual(b, h) {
		return nil
	}
	if !bHas && !hHas {
		return nil
	}
	return []Finding{ruleFinding("33", DirP2C, majorMajor, ptr+"/default", "default added/changed/removed")}
}

// --- properties/required (rows 1-5) ---

// diffProperties covers rows 1-5. `required` is compared as a FULL SET,
// independent of whether a matching `properties` entry exists on that
// same side (C4/C9): the old implementation built its comparison universe
// solely from `properties` keys, so a name added to (or removed from)
// `required` with no property declaration at all was invisible -- yet
// this repo's own generated decoders enforce `required` by raw key
// lookup regardless of whether `properties` describes that key. A
// `required` name with NO matching `properties` entry on that same side
// cannot be structurally diffed (there is no nested schema to recurse
// into, or to have "moved into required" relative to), so it fails
// closed as a malformed schema rather than being silently accepted the
// way it was before.
func (c *diffCtx) diffProperties(base, head map[string]any, dir Direction, ptr string) ([]Finding, error) {
	bProps, _ := base["properties"].(map[string]any)
	hProps, _ := head["properties"].(map[string]any)
	bReq := stringSet(base["required"])
	hReq := stringSet(head["required"])

	names := map[string]bool{}
	for n := range bProps {
		names[n] = true
	}
	for n := range hProps {
		names[n] = true
	}
	for n := range bReq {
		names[n] = true
	}
	for n := range hReq {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	var findings []Finding
	for _, name := range sorted {
		propPtr := ptr + "/properties/" + jsonPointerEscape(name)
		bSchema, inBaseProp := bProps[name]
		hSchema, inHeadProp := hProps[name]

		if bReq[name] && !inBaseProp {
			return nil, failClosed("fc-required-orphan", ptr+"/required", "%q is required in base but has no matching properties entry", name)
		}
		if hReq[name] && !inHeadProp {
			return nil, failClosed("fc-required-orphan", ptr+"/required", "%q is required in head but has no matching properties entry", name)
		}

		switch {
		case inBaseProp && !inHeadProp:
			findings = append(findings, ruleFinding("1", dir, majorMajor, propPtr, "property removed"))
		case !inBaseProp && inHeadProp:
			if hReq[name] {
				findings = append(findings, ruleFinding("3", dir, severityPair{SeverityMinor, SeverityMajor}, propPtr, "property added and required"))
			} else {
				findings = append(findings, ruleFinding("2", dir, severityPair{SeverityMinor, SeverityMinor}, propPtr, "property added, not required"))
			}
		case inBaseProp && inHeadProp:
			wasReq, isReq := bReq[name], hReq[name]
			if !wasReq && isReq {
				findings = append(findings, ruleFinding("4", dir, severityPair{SeverityMinor, SeverityMajor}, propPtr, "property moved into required"))
			}
			if wasReq && !isReq {
				findings = append(findings, ruleFinding("5", dir, severityPair{SeverityMajor, SeverityMinor}, propPtr, "property removed from required"))
			}
			nested, err := c.diffNode(bSchema, hSchema, dir, propPtr)
			if err != nil {
				return nil, err
			}
			findings = append(findings, nested...)
		}
		// The remaining case (!inBaseProp && !inHeadProp) is unreachable:
		// `name` only enters this loop via bReq/hReq membership when it
		// is not a properties key on either side, and the orphan guard
		// above already fails closed for any side where that combination
		// occurs.
	}
	return findings, nil
}

// onlyKeys reports whether obj's keys are a subset of allowed.
func onlyKeys(obj map[string]any, allowed ...string) bool {
	set := map[string]bool{}
	for _, k := range allowed {
		set[k] = true
	}
	for k := range obj {
		if !set[k] {
			return false
		}
	}
	return true
}

func stringSet(v any) map[string]bool {
	out := map[string]bool{}
	arr, _ := v.([]any)
	for _, el := range arr {
		if s, ok := el.(string); ok {
			out[s] = true
		}
	}
	return out
}

// --- additionalProperties (rows 23, 24, 25, 26, 42) ---

type apKind int

const (
	apFalse      apKind = iota
	apPermissive        // absent (default true) or explicit true
	apSchema
)

func classifyAP(obj map[string]any) (apKind, any) {
	v, ok := obj["additionalProperties"]
	if !ok {
		return apPermissive, nil
	}
	switch b := v.(type) {
	case bool:
		if b {
			return apPermissive, nil
		}
		return apFalse, nil
	default:
		return apSchema, v
	}
}

func (c *diffCtx) diffAdditionalProperties(base, head map[string]any, dir Direction, ptr string) ([]Finding, error) {
	bKind, bSchema := classifyAP(base)
	hKind, hSchema := classifyAP(head)

	apPtr := ptr + "/additionalProperties"

	if bKind == apSchema && hKind == apSchema {
		nested, err := c.diffNode(bSchema, hSchema, dir, apPtr)
		if err != nil {
			return nil, err
		}
		if len(nested) == 0 {
			return nil, nil
		}
		worst := SeverityPatch
		for _, f := range nested {
			worst = maxSeverity(worst, f.Severity)
		}
		wrapper := Finding{RuleID: "26", Severity: worst, Pointer: apPtr, Message: "additionalProperties schema changed on both sides"}
		return append([]Finding{wrapper}, nested...), nil
	}

	if bKind == hKind {
		return nil, nil
	}

	// C13: schema -> permissive (true or absent) is its OWN row (42), not
	// the generic MINOR/MINOR "loosening" bucket below. Unlike false ->
	// anything (a pure unlock: no extra properties were ever allowed
	// before, some now are, safe both ways), a schema constraining
	// additional-property VALUES going away widens what a P2C map's
	// values may be -- a Go client decoding into map[string]string breaks
	// on a value the platform is now free to send that isn't a string.
	if bKind == apSchema && hKind == apPermissive {
		return []Finding{ruleFinding("42", dir, severityPair{SeverityMajor, SeverityMinor}, apPtr, "additionalProperties schema loosened to permissive (true/absent)")}, nil
	}

	// Restricting: (permissive|schema) -> false, or permissive -> schema.
	restricting := (hKind == apFalse) || (bKind == apPermissive && hKind == apSchema)
	if restricting {
		ruleID := "24"
		if hKind == apSchema {
			ruleID = "25"
		}
		return []Finding{ruleFinding(ruleID, dir, majorMajor, apPtr, "additionalProperties made more restrictive")}, nil
	}

	// Remaining loosening cases: false -> permissive, false -> schema.
	return []Finding{ruleFinding("23", dir, severityPair{SeverityMinor, SeverityMinor}, apPtr, "additionalProperties made less restrictive")}, nil
}

// --- items (row 39) ---

func (c *diffCtx) diffItems(base, head map[string]any, dir Direction, ptr string) ([]Finding, error) {
	bItems, bHas := base["items"]
	hItems, hHas := head["items"]
	if bHas != hHas {
		return []Finding{ruleFinding("39", dir, majorMajor, ptr+"/items", "items presence changed")}, nil
	}
	if !bHas {
		return nil, nil
	}
	return c.diffNode(bItems, hItems, dir, ptr+"/items")
}

// --- oneOf/anyOf (rows 9, 10, 28, 29, 30, 43) ---

func (c *diffCtx) diffUnion(base, head map[string]any, dir Direction, ptr, keyword string) ([]Finding, error) {
	bRaw, bHas := base[keyword]
	hRaw, hHas := head[keyword]
	if !bHas && !hHas {
		return nil, nil
	}
	if bHas != hHas {
		// C15: the KEYWORD ITSELF appearing or disappearing (not a
		// member within an already-existing union) is a presence change,
		// scored MAJOR in both columns -- introducing a oneOf/anyOf where
		// none existed adds a constraint the table's row 28 ("variant
		// added", MINOR/MINOR) does not name; removing one removes a
		// constraint the old code scored as though every member had
		// simply been deleted one at a time (row 29, still MAJOR/MAJOR,
		// so removal already happened to be safe -- but addition was not).
		return []Finding{ruleFinding("43", dir, majorMajor, ptr+"/"+keyword, keyword+" keyword presence changed")}, nil
	}

	bArr, ok := bRaw.([]any)
	if !ok {
		return nil, failClosed("fc-shape", ptr+"/"+keyword, "%s must be an array", keyword)
	}
	hArr, ok := hRaw.([]any)
	if !ok {
		return nil, failClosed("fc-shape", ptr+"/"+keyword, "%s must be an array", keyword)
	}

	type member struct {
		key  string
		idx  int
		node any
	}
	keyOf := func(node any) (string, bool) {
		if ref := refTargetName(node); ref != "" {
			return "ref:" + ref, true
		}
		obj, ok := node.(map[string]any)
		if !ok {
			return "", false
		}
		if props, ok := obj["properties"].(map[string]any); ok {
			if t, ok := props["type"].(map[string]any); ok {
				if constVal, ok := t["const"]; ok {
					return "const:" + fmt.Sprint(constVal), true
				}
			}
		}
		// A bare {"type": "..."} branch with no $ref and no discriminator
		// -- the common "anyOf: [{$ref: X}, {type: null}]" nullable-via-
		// anyOf idiom this codebase's own rest/v1/dtos.schema.json uses
		// (e.g. ReviewReadout.latestVerdict) is exactly this: pairing by
		// the plain scalar type name is still a closed, deterministic
		// key, not a guess. keyOf returning "type:null" specifically is
		// what lets the loop below re-route that member through rows
		// 9/10 instead of the generic 28/29 (C3).
		if t, ok := obj["type"].(string); ok && onlyKeys(obj, "type", "description") {
			return "type:" + t, true
		}
		return "", false
	}

	baseMembers := map[string]member{}
	for i, n := range bArr {
		k, ok := keyOf(n)
		if !ok {
			return nil, failClosed("fc-oneof-unpairable", fmt.Sprintf("%s/%s/%d", ptr, keyword, i), "cannot pair this %s member (no $ref or properties.type.const to key on)", keyword)
		}
		if _, dup := baseMembers[k]; dup {
			return nil, failClosed("fc-oneof-unpairable", fmt.Sprintf("%s/%s/%d", ptr, keyword, i), "duplicate pairing key %q in base %s", k, keyword)
		}
		baseMembers[k] = member{k, i, n}
	}
	headMembers := map[string]member{}
	for i, n := range hArr {
		k, ok := keyOf(n)
		if !ok {
			return nil, failClosed("fc-oneof-unpairable", fmt.Sprintf("%s/%s/%d", ptr, keyword, i), "cannot pair this %s member (no $ref or properties.type.const to key on)", keyword)
		}
		if _, dup := headMembers[k]; dup {
			return nil, failClosed("fc-oneof-unpairable", fmt.Sprintf("%s/%s/%d", ptr, keyword, i), "duplicate pairing key %q in head %s", k, keyword)
		}
		headMembers[k] = member{k, i, n}
	}

	allKeys := map[string]bool{}
	for k := range baseMembers {
		allKeys[k] = true
	}
	for k := range headMembers {
		allKeys[k] = true
	}
	sortedKeys := make([]string, 0, len(allKeys))
	for k := range allKeys {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)

	var findings []Finding
	for _, k := range sortedKeys {
		bm, inBase := baseMembers[k]
		hm, inHead := headMembers[k]
		switch {
		case inBase && !inHead:
			removedPtr := fmt.Sprintf("%s/%s/%d", ptr, keyword, bm.idx)
			if k == "type:null" {
				findings = append(findings, ruleFinding("10", dir, severityPair{SeverityMinor, SeverityMajor}, removedPtr, "null variant removed from "+keyword))
			} else {
				findings = append(findings, ruleFinding("29", dir, majorMajor, removedPtr, "union member removed: "+k))
			}
		case !inBase && inHead:
			addedPtr := fmt.Sprintf("%s/%s/%d", ptr, keyword, hm.idx)
			if k == "type:null" {
				findings = append(findings, ruleFinding("9", dir, severityPair{SeverityMajor, SeverityMinor}, addedPtr, "null variant added to "+keyword))
			} else {
				findings = append(findings, ruleFinding("28", dir, severityPair{SeverityMinor, SeverityMinor}, addedPtr, "union member added: "+k))
			}
		default:
			memberPtr := fmt.Sprintf("%s/%s/%d", ptr, keyword, hm.idx)
			nested, err := c.diffNode(bm.node, hm.node, dir, memberPtr)
			if err != nil {
				return nil, err
			}
			if len(nested) == 0 {
				continue
			}
			worst := SeverityPatch
			for _, f := range nested {
				worst = maxSeverity(worst, f.Severity)
			}
			wrapper := Finding{RuleID: "30", Severity: worst, Pointer: memberPtr, Message: "union member changed: " + k}
			findings = append(findings, wrapper)
			findings = append(findings, nested...)
		}
	}
	return findings, nil
}
