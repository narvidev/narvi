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
// under a single Direction, or under DirBoth by requiring compatibility
// under BOTH the P2C and C2P columns (§6.3 design spec §2) and keeping the
// worse-severity finding per pointer+rule.
func DiffDef(baseDefs, headDefs map[string]any, name string, dir Direction, openEnums map[string]bool) ([]Finding, error) {
	ctx := &diffCtx{
		baseR:     resolver{defs: baseDefs},
		headR:     resolver{defs: headDefs},
		openEnums: openEnums,
	}
	ptr := "#/$defs/" + jsonPointerEscape(name)
	base, head := baseDefs[name], headDefs[name]

	if dir != DirBoth {
		return ctx.diffNode(base, head, dir, ptr)
	}

	p2c, err := ctx.diffNode(base, head, DirP2C, ptr)
	if err != nil {
		return nil, err
	}
	c2p, err := ctx.diffNode(base, head, DirC2P, ptr)
	if err != nil {
		return nil, err
	}
	return mergeBothDirections(p2c, c2p), nil
}

// mergeBothDirections combines the two per-column readings of a DirBoth
// def: a finding present under only one column still applies (that
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

// diffNode is the recursive engine behind rows 1-30, 33, 35, 39 of the
// rule table. It handles $ref resolution (including a retargeted $ref,
// row 27) before comparing the resolved schema content.
func (c *diffCtx) diffNode(base, head any, dir Direction, ptr string) ([]Finding, error) {
	bRefName := refTargetName(base)
	hRefName := refTargetName(head)

	switch {
	case bRefName != "" && hRefName != "" && bRefName != hRefName:
		return c.diffRetargetedRef(bRefName, hRefName, dir, ptr)
	default:
		// Same $ref (or no $ref at all on one or both sides): resolve
		// whichever side has one and keep comparing the resolved content
		// at the SAME pointer -- an asymmetric ref (inline on one side,
		// $ref on the other) is not itself a named rule; only the
		// resulting content diff is.
		rBase, err := c.baseR.resolve(base, nil)
		if err != nil {
			return nil, failClosed("fc-ref", ptr, "%v", err)
		}
		rHead, err := c.headR.resolve(head, nil)
		if err != nil {
			return nil, failClosed("fc-ref", ptr, "%v", err)
		}
		return c.diffResolved(rBase, rHead, dir, ptr)
	}
}

// diffRetargetedRef implements row 27: the finding's severity comes from
// comparing the OLD target's content (as it stood in base) against the
// NEW target's content (as it stands in head), unless the old target no
// longer exists in head's own $defs at all (row 31 territory), which is
// unconditionally MAJOR.
func (c *diffCtx) diffRetargetedRef(bRefName, hRefName string, dir Direction, ptr string) ([]Finding, error) {
	if _, ok := c.headR.defs[bRefName]; !ok {
		return []Finding{{
			RuleID:   "27",
			Severity: SeverityMajor,
			Pointer:  ptr,
			Message:  fmt.Sprintf("$ref retargeted from %q to %q, and %q no longer exists", bRefName, hRefName, bRefName),
		}}, nil
	}

	oldContent, err := c.baseR.resolve(c.baseR.defs[bRefName], nil)
	if err != nil {
		return nil, failClosed("fc-ref", ptr, "%v", err)
	}
	newContent, err := c.headR.resolve(c.headR.defs[hRefName], nil)
	if err != nil {
		return nil, failClosed("fc-ref", ptr, "%v", err)
	}
	nested, err := c.diffResolved(oldContent, newContent, dir, ptr)
	if err != nil {
		return nil, err
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

// diffResolved compares two already-dereferenced schema nodes (each
// either a JSON object or the boolean literals true/false).
func (c *diffCtx) diffResolved(base, head any, dir Direction, ptr string) ([]Finding, error) {
	bObj, bIsObj := base.(map[string]any)
	hObj, hIsObj := head.(map[string]any)

	if bIsObj && hIsObj && reflect.DeepEqual(bObj, hObj) {
		// Nothing changed at or under this node: skip it entirely, rather
		// than run e.g. oneOf/anyOf pairing (defdiff.go's diffUnion) on
		// content nobody touched. A pairing heuristic that cannot key
		// every member of some untouched, pre-existing union would
		// otherwise fail closed on files that have never changed at all.
		return nil, nil
	}

	if !bIsObj || !hIsObj {
		// Boolean schemas (true/false) at a generic recursion point are
		// vanishingly rare in this codebase (additionalProperties/items
		// handle their own bool case before ever calling diffResolved on
		// a bare bool) -- treat any mismatch here defensively as a type
		// change rather than pretend to know a finer rule applies.
		if reflect.DeepEqual(base, head) {
			return nil, nil
		}
		return []Finding{ruleFinding("6", dir, majorMajor, ptr, "schema literal changed")}, nil
	}

	var findings []Finding

	findings = append(findings, diffAnnotations(bObj, hObj, ptr)...)

	typeFindings, err := c.diffType(bObj, hObj, dir, ptr)
	if err != nil {
		return nil, err
	}
	findings = append(findings, typeFindings...)

	findings = append(findings, c.diffEnum(bObj, hObj, dir, ptr)...)
	findings = append(findings, diffConst(bObj, hObj, ptr)...)
	findings = append(findings, diffFormat(bObj, hObj, dir, ptr)...)
	findings = append(findings, diffPattern(bObj, hObj, dir, ptr)...)
	findings = append(findings, diffNumericFloor(bObj, hObj, dir, ptr, "minimum")...)
	findings = append(findings, diffNumericFloor(bObj, hObj, dir, ptr, "minLength")...)
	findings = append(findings, diffNumericFloor(bObj, hObj, dir, ptr, "minItems")...)
	findings = append(findings, diffDefault(bObj, hObj, ptr)...)

	propFindings, err := c.diffProperties(bObj, hObj, dir, ptr)
	if err != nil {
		return nil, err
	}
	findings = append(findings, propFindings...)

	apFindings, err := c.diffAdditionalProperties(bObj, hObj, dir, ptr)
	if err != nil {
		return nil, err
	}
	findings = append(findings, apFindings...)

	itemsFindings, err := c.diffItems(bObj, hObj, dir, ptr)
	if err != nil {
		return nil, err
	}
	findings = append(findings, itemsFindings...)

	for _, kw := range []string{"oneOf", "anyOf"} {
		fs, err := c.diffUnion(bObj, hObj, dir, ptr, kw)
		if err != nil {
			return nil, err
		}
		findings = append(findings, fs...)
	}

	return findings, nil
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

// --- annotations: description, goJSONSchema (row 35) ---

// diffAnnotations covers row 35 ("description changed", PATCH) and,
// broadened to match reality, a change to go-jsonschema's own
// "goJSONSchema" codegen-hint extension -- see that keyword's own doc
// comment in keywords.go for why a change there is PATCH-class same as a
// description edit: it steers this repository's generated Go type, never
// the JSON wire shape any consumer actually decodes.
func diffAnnotations(base, head map[string]any, ptr string) []Finding {
	var findings []Finding
	if f := diffAnnotationKey(base, head, ptr, "description"); f != nil {
		findings = append(findings, *f)
	}
	if f := diffAnnotationKey(base, head, ptr, "goJSONSchema"); f != nil {
		findings = append(findings, *f)
	}
	return findings
}

func diffAnnotationKey(base, head map[string]any, ptr, key string) *Finding {
	b, bok := base[key]
	h, hok := head[key]
	if bok == hok && reflect.DeepEqual(b, h) {
		return nil
	}
	return &Finding{RuleID: "35", Severity: SeverityPatch, Pointer: ptr + "/" + key, Message: key + " changed"}
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
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	var findings []Finding
	for _, name := range sorted {
		propPtr := ptr + "/properties/" + jsonPointerEscape(name)
		bSchema, inBase := bProps[name]
		hSchema, inHead := hProps[name]

		switch {
		case inBase && !inHead:
			findings = append(findings, ruleFinding("1", dir, majorMajor, propPtr, "property removed"))
		case !inBase && inHead:
			if hReq[name] {
				findings = append(findings, ruleFinding("3", dir, severityPair{SeverityMinor, SeverityMajor}, propPtr, "property added and required"))
			} else {
				findings = append(findings, ruleFinding("2", dir, severityPair{SeverityMinor, SeverityMinor}, propPtr, "property added, not required"))
			}
		default:
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

// --- additionalProperties (rows 23, 24, 25, 26) ---

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

	// Restricting: (permissive|schema) -> false, or permissive -> schema.
	restricting := (hKind == apFalse) || (bKind == apPermissive && hKind == apSchema)
	if restricting {
		ruleID := "24"
		if hKind == apSchema {
			ruleID = "25"
		}
		return []Finding{ruleFinding(ruleID, dir, majorMajor, apPtr, "additionalProperties made more restrictive")}, nil
	}

	// Loosening: false -> (permissive|schema), or schema -> permissive.
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

// --- oneOf/anyOf (rows 28, 29, 30) ---

func (c *diffCtx) diffUnion(base, head map[string]any, dir Direction, ptr, keyword string) ([]Finding, error) {
	bArr, bHas := base[keyword].([]any)
	hArr, hHas := head[keyword].([]any)
	if !bHas && !hHas {
		return nil, nil
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
		// the plain scalar type name is still a closed, deterministic key,
		// not a guess.
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
			findings = append(findings, ruleFinding("29", dir, majorMajor, fmt.Sprintf("%s/%s/%d", ptr, keyword, bm.idx), "union member removed: "+k))
		case !inBase && inHead:
			findings = append(findings, ruleFinding("28", dir, severityPair{SeverityMinor, SeverityMinor}, fmt.Sprintf("%s/%s/%d", ptr, keyword, hm.idx), "union member added: "+k))
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
