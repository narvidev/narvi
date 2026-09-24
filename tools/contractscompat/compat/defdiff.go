package compat

import (
	"encoding/json"
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
	// visited is the self-reference recursion guard (D17/D22): once
	// diffNode has dereferenced a given (base $ref target, head $ref
	// target, Direction) triple once within this ctx's own diff, a later
	// revisit of the EXACT same triple -- inevitable for a def that
	// (directly or through a chain of properties/items/oneOf/anyOf)
	// $refs itself, since resolving it always lands back on the same
	// name -- returns immediately instead of re-diffing. Without this, a
	// self-referencing def whose recursive path also differs between
	// base and head never reaches diffResolved's own
	// reflect.DeepEqual short-circuit (the two sides are never bit-for-
	// bit equal), so diffNode would call itself forever.
	//
	// E2: this key deliberately omits the POINTER the (base, head, dir)
	// triple was reached at -- a revisit through a second referencing
	// site is graded identically to the first, now that grading itself
	// (loc.defPtr, below) no longer depends on the referencing path
	// either. Before that fix, two sites retargeting the SAME (old, new)
	// def pair could legitimately grade differently (an openEnums
	// pointer match at one site's traversal pointer but not the
	// other's), which made skipping the second site's diff silently
	// wrong. See TestRound3_E2_RetargetRecursionGuardIsSound for the
	// pinning test that this is now sound.
	//
	// Lazily initialized; never cleared -- it is scoped to one ctx's
	// lifetime, which is one top-level DiffDef/root call, exactly the
	// span within which the same def pair could recur.
	visited map[visitKey]bool
}

// visitKey identifies one (base $ref target name, head $ref target name,
// Direction) triple for diffCtx.visitOnce.
type visitKey struct {
	baseName, headName string
	dir                Direction
}

// visitOnce reports whether (bName, hName, dir) is being seen for the
// FIRST time in this ctx (true: proceed normally) or was already visited
// earlier in this same diff (false: the caller must stop without
// re-diffing -- see diffCtx.visited's own doc comment).
func (c *diffCtx) visitOnce(bName, hName string, dir Direction) bool {
	if c.visited == nil {
		c.visited = map[visitKey]bool{}
	}
	key := visitKey{bName, hName, dir}
	if c.visited[key] {
		return false
	}
	c.visited[key] = true
	return true
}

// loc bundles the JSON Pointers a diffed schema node carries as
// diffNode's recursive walk descends (E2/E3/E7, F1/F6):
//
//   - ptr is the TRAVERSAL pointer -- rooted at whichever $defs entry (or
//     document root) the outermost DiffDef/DiffSurface call started from,
//     and prefixed by every referencing property/items/union-member/
//     retarget the walk crossed to reach this node. Every Finding.Pointer
//     reported anywhere in this package is a ptr, unchanged from before
//     this fix -- WHERE a change is reported has never been the problem.
//   - defPtr is the DEFINING pointer as it stands in HEAD -- rooted at
//     THIS node's own nearest enclosing $defs entry: the one whose
//     content this node is literally written inside. It resets to that
//     def's own canonical "#/$defs/<Name>" every time the walk crosses
//     INTO a different def via a $ref (a same-name re-reference in
//     diffNode, or a retarget's NEW target in diffRetargetedRef) --
//     using the TERMINAL name each side's own alias chain resolves to
//     (F6: resolver.resolveDefName), never the raw $ref name at the
//     crossing -- and otherwise grows by the exact same suffix as ptr.
//   - oldDefPtr mirrors defPtr but rooted at the def as it stood in
//     BASE. Outside a retargeted $ref the two are always identical (a
//     same-name crossing resets both to the same terminal name); they
//     only diverge across a retarget's own nested diff (F1), where the
//     content being compared genuinely comes from two differently-named
//     defs -- the OLD target the referencing site's existing consumers
//     were bound to, and the NEW target that now governs it.
//
// openEnums is authored against a pointer of the defPtr/oldDefPtr kind
// (COMPATIBILITY.md: "the EXACT JSON Pointer ... of each open string
// enum's own schema node") -- diffEnum is the one place that reads
// either; every other handler only ever reports at ptr and never looks
// at them. Before E3/E7/E2 there was only one pointer, doing both jobs,
// so an enum's relaxation was lost the moment it was reached through ANY
// $ref (E3), a root surface whose own root is a $ref re-graded its defs'
// enums under the wrong, traversal-only pointer (E7), and two retarget
// sites sharing a (base target, head target, direction) triple could
// grade the SAME enum differently depending on which site the recursion
// guard happened to visit first (E2). F1: even after that fix, a
// retarget's added enum value was graded open-or-closed by the NEW
// target's pointer alone -- a closed enum at a consumed site could be
// silently widened by retargeting it to a def that happens to be open
// under a DIFFERENT name; diffEnum now requires BOTH oldDefPtr and
// defPtr to be open.
type loc struct {
	ptr, defPtr, oldDefPtr string
}

// child extends every one of l's pointers by the same suffix -- used for
// every recursive step that does NOT cross a $ref (into a property,
// items, additionalProperties schema, or union member): ptr, defPtr and
// oldDefPtr all stay in lockstep until the walk actually enters a
// different def.
func (l loc) child(suffix string) loc {
	return loc{ptr: l.ptr + suffix, defPtr: l.defPtr + suffix, oldDefPtr: l.oldDefPtr + suffix}
}

// intoDef returns the loc for a node reached by crossing a $ref: ptr
// keeps accumulating through the REFERENCING path (a Finding inside the
// def is still reported at the location a maintainer diffing the file
// would actually see -- e.g. "#/$defs/CreateAutomationResponse/
// properties/automation/properties/status/enum"), while defPtr/oldDefPtr
// reset to headTermName's/baseTermName's own canonical "#/$defs/<Name>"
// root -- the TERMINAL name each side's OWN alias chain resolves to
// (F6), not the raw $ref name at this crossing -- so an openEnums entry
// authored against the resolved def's own pointer keeps matching
// regardless of how many alias hops or $refs away it was reached from.
// Outside a retarget, baseTermName == headTermName (the same-name path
// that calls this always resolves the SAME $ref string on both sides,
// unless the alias def it points at was itself edited between base and
// head -- rare, but not assumed away here), so the two pointers this
// produces are identical, same as before F1 split defPtr in two.
func intoDef(l loc, baseTermName, headTermName string) loc {
	return loc{
		ptr:       l.ptr,
		defPtr:    "#/$defs/" + jsonPointerEscape(headTermName),
		oldDefPtr: "#/$defs/" + jsonPointerEscape(baseTermName),
	}
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
	// Pre-seed the recursion guard (D17/D22) with this def's own identity.
	// diffNode's own visitOnce call only fires when it encounters a LIVE
	// "$ref": name node during its recursive walk -- this OUTERMOST call
	// compares the two defs' raw content directly, with no $ref node of
	// its own to trip that check. Without pre-seeding, a self-referencing
	// def's own top-level content would be diffed once here, and THEN the
	// first "$ref": name node reached while walking that content would
	// still be allowed one more full recursive pass before the guard
	// caught the SECOND one -- reporting the same recursive-path change
	// twice, and doing one extra unbounded-sized diff pass, before
	// stopping. Pre-seeding means the very FIRST "$ref": name node
	// encountered anywhere in this def's own structure is already a
	// revisit, so recursion stops after exactly one full pass.
	if dir == DirBoth {
		ctx.visited = map[visitKey]bool{{name, name, DirP2C}: true, {name, name, DirC2P}: true}
	} else {
		ctx.visited = map[visitKey]bool{{name, name, dir}: true}
	}
	ptr := "#/$defs/" + jsonPointerEscape(name)
	base, head := baseDefs[name], headDefs[name]
	return ctx.diffNode(base, head, dir, loc{ptr: ptr, defPtr: ptr, oldDefPtr: ptr})
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
// $ref handling (rows 27, 31-32's caller): $ref is illegal beside any
// sibling keyword other than "description" (see ref.go's own doc comment
// on refAllowedSiblingKeys) -- so resolving a $ref node is simply
// "follow it to its def," never a merge. The one legal sibling,
// "description," is diffed on its own by diffRefSiblingDescription,
// independent of whatever the $ref resolves to (it is a pure annotation,
// row 35, never a wire constraint). A retargeted $ref (row 27) is handled
// by diffRetargetedRef, which fully dereferences each side's OWN target
// (through any alias chain) and diffs the two resulting concrete nodes
// directly, with no merge either.
func (c *diffCtx) diffNode(base, head any, dir Direction, l loc) ([]Finding, error) {
	if dir == DirBoth {
		p2c, err := c.diffNode(base, head, DirP2C, l)
		if err != nil {
			return nil, err
		}
		c2p, err := c.diffNode(base, head, DirC2P, l)
		if err != nil {
			return nil, err
		}
		return mergeBothDirections(p2c, c2p), nil
	}

	bRefName := refTargetName(base)
	hRefName := refTargetName(head)

	// The referencing node's OWN siblings (base and head, independently):
	// diffRetargetedRef below only ever resolves each side's DEF content,
	// never the raw referencing node itself, so a disallowed sibling
	// sitting right here would otherwise slip past it. walkSchema already
	// enforces this structurally for every real PR (DiffSurface always
	// calls it on the whole document before diffing anything), but DiffDef
	// is itself an exported, independently-callable entry point (this
	// package's own tests call it directly) -- checking here too keeps it
	// correct on its own, not just when walkSchema happened to run first.
	if bRefName != "" {
		if bObj, ok := base.(map[string]any); ok {
			if err := checkRefSiblings(bObj); err != nil {
				return nil, failClosed("fc-ref", l.ptr, "%v", err)
			}
		}
	}
	if hRefName != "" {
		if hObj, ok := head.(map[string]any); ok {
			if err := checkRefSiblings(hObj); err != nil {
				return nil, failClosed("fc-ref", l.ptr, "%v", err)
			}
		}
	}

	var descFindings []Finding
	if bRefName != "" || hRefName != "" {
		descFindings = diffRefSiblingDescription(base, head, l.ptr)
	}

	if bRefName != "" && hRefName != "" {
		// Recursion guard (D17/D22, E2): this exact (base target, head
		// target, direction) triple may already have been diffed earlier
		// in this def's own recursive structure -- a revisit can only
		// happen by looping through a self-referencing def, or by a
		// second, independent site referencing the same (base, head)
		// pair. Either way it cannot discover anything new: grading
		// (loc.defPtr) depends only on the (base, head) pair's OWN
		// defining location, never on the referencing path that got us
		// here, so a revisit is provably redundant. See this file's own
		// loc doc comment.
		if !c.visitOnce(bRefName, hRefName, dir) {
			return descFindings, nil
		}
		if bRefName != hRefName {
			wrapper, err := c.diffRetargetedRef(bRefName, hRefName, dir, l)
			if err != nil {
				return nil, err
			}
			return append(descFindings, wrapper...), nil
		}
	}

	rBase, err := c.baseR.resolve(base, nil)
	if err != nil {
		return nil, failClosed("fc-ref", l.ptr, "%v", err)
	}
	rHead, err := c.headR.resolve(head, nil)
	if err != nil {
		return nil, failClosed("fc-ref", l.ptr, "%v", err)
	}

	bObj, bIsObj := rBase.(map[string]any)
	hObj, hIsObj := rHead.(map[string]any)
	if !bIsObj || !hIsObj {
		// At least one side is the boolean schema literal true/false,
		// reached with NO $ref involved (a bare `true`/`false` directly
		// in this node's own position, e.g. as an `items` value) -- a
		// $ref to a boolean def is rejected earlier, by resolveDef.
		// Compare the literals directly: collapsing a bare `false`
		// ("reject everything") into the empty object `{}` ("accept
		// everything") would silently invert its meaning.
		if reflect.DeepEqual(rBase, rHead) {
			return descFindings, nil
		}
		return append(descFindings, ruleFinding("6", dir, majorMajor, l.ptr, "schema literal changed")), nil
	}

	// E3/E7: bRefName == hRefName here whenever both are non-empty (the
	// bRefName != hRefName case already returned above, via
	// diffRetargetedRef) -- a same-name $ref means this node's resolved
	// content is literally the content bRefName's own alias chain
	// resolves to, so the node's DEFINING location resets to THAT def's
	// own root (F6: the TERMINAL name, not bRefName itself, in case
	// bRefName names a pure alias def) even though its TRAVERSAL location
	// (l.ptr) keeps accumulating through whatever property/item/
	// union-member path led here. A bare $ref->inline transition
	// (bRefName != "", hRefName == "") resets the same way, using base's
	// own terminal name for both defPtr and oldDefPtr -- there is no head
	// def to resolve a second, independent name from.
	nodeLoc := l
	if bRefName != "" {
		baseTerm, err := c.baseR.resolveDefName(bRefName, nil)
		if err != nil {
			return nil, failClosed("fc-ref", l.ptr, "%v", err)
		}
		headTerm := baseTerm
		if hRefName != "" {
			headTerm, err = c.headR.resolveDefName(hRefName, nil)
			if err != nil {
				return nil, failClosed("fc-ref", l.ptr, "%v", err)
			}
		}
		nodeLoc = intoDef(l, baseTerm, headTerm)
	}

	resolvedFindings, err := c.diffResolved(bObj, hObj, dir, nodeLoc)
	if err != nil {
		return nil, err
	}
	return append(descFindings, resolvedFindings...), nil
}

// diffRefSiblingDescription diffs the "description" annotation that may
// legally sit beside a $ref -- the one sibling keyword this checker
// tolerates (ref.go's refAllowedSiblingKeys). It runs independent of
// whatever the $ref resolves to: description is annotation-only (row 35),
// so dropping the merge machinery that used to fold it into the resolved
// node's own content must not make a change to THIS sibling invisible.
func diffRefSiblingDescription(base, head any, ptr string) []Finding {
	bObj, _ := base.(map[string]any)
	hObj, _ := head.(map[string]any)
	if f := diffAnnotationKey(bObj, hObj, ptr, "description", "35", SeverityPatch); f != nil {
		return []Finding{*f}
	}
	return nil
}

// diffRetargetedRef implements row 27: the finding's severity comes from
// comparing the OLD target's fully-dereferenced content (as it stood in
// base) against the NEW target's fully-dereferenced content (as it stands
// in head), unless the old target no longer exists in head's own $defs at
// all (row 31 territory), which is unconditionally MAJOR. Neither target
// can itself carry a constraint-bearing sibling (ref.go's resolve/
// resolveDef already fail closed on that, along the whole alias chain),
// so there is nothing left to merge here.
//
// E2/E3/F1/F6: the nested comparison's DEFINING locations (for
// openEnums) are rooted at BOTH the OLD target's own def ("#/$defs/"+
// its terminal alias name) and the NEW target's ("#/$defs/"+its terminal
// alias name) -- F1: an added enum value is graded open (MINOR on P2C)
// only when BOTH are open. Grading by the new target's pointer alone (as
// this function did before F1) let a closed enum at a consumed site be
// silently widened by retargeting it to a def that merely HAPPENS to be
// open under its own, different name -- the referencing site's own
// consumers never accepted that promise, only the def's own direct
// consumers did. Requiring the OLD target's pointer too means the
// relaxation must ALSO have already covered this site before the
// retarget (independently of it) for the addition to be safe. The
// referencing node's TRAVERSAL location (l.ptr) is unchanged either way,
// so a nested Finding is still reported where a maintainer diffing the
// file would look for it.
func (c *diffCtx) diffRetargetedRef(bRefName, hRefName string, dir Direction, l loc) ([]Finding, error) {
	if _, ok := c.headR.defs[bRefName]; !ok {
		return []Finding{{
			RuleID:   "27",
			Severity: SeverityMajor,
			Pointer:  l.ptr,
			Message:  fmt.Sprintf("$ref retargeted from %q to %q, and %q no longer exists", bRefName, hRefName, bRefName),
		}}, nil
	}

	oldTarget, err := c.baseR.resolveDef(bRefName, nil)
	if err != nil {
		return nil, failClosed("fc-ref", l.ptr, "%v", err)
	}
	newTarget, err := c.headR.resolveDef(hRefName, nil)
	if err != nil {
		return nil, failClosed("fc-ref", l.ptr, "%v", err)
	}

	var nested []Finding
	oldObj, oldIsObj := oldTarget.(map[string]any)
	newObj, newIsObj := newTarget.(map[string]any)
	switch {
	case !oldIsObj || !newIsObj:
		if !reflect.DeepEqual(oldTarget, newTarget) {
			nested = []Finding{ruleFinding("6", dir, majorMajor, l.ptr, "schema literal changed")}
		}
	default:
		// F6: root each pointer at the TERMINAL name its own alias
		// chain resolves to, not bRefName/hRefName directly -- either
		// could itself be a pure alias def (walkSchema explicitly
		// allows one), in which case the content just resolved above
		// (oldObj/newObj) actually belongs to that terminal def, not
		// the alias that merely points at it.
		baseTerm, err := c.baseR.resolveDefName(bRefName, nil)
		if err != nil {
			return nil, failClosed("fc-ref", l.ptr, "%v", err)
		}
		headTerm, err := c.headR.resolveDefName(hRefName, nil)
		if err != nil {
			return nil, failClosed("fc-ref", l.ptr, "%v", err)
		}
		targetLoc := loc{
			ptr:       l.ptr,
			defPtr:    "#/$defs/" + jsonPointerEscape(headTerm),
			oldDefPtr: "#/$defs/" + jsonPointerEscape(baseTerm),
		}
		nested, err = c.diffResolved(oldObj, newObj, dir, targetLoc)
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
		Pointer:  l.ptr,
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
func (c *diffCtx) diffResolved(bObj, hObj map[string]any, dir Direction, l loc) ([]Finding, error) {
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

	findings = append(findings, diffAnnotations(bObj, hObj, l.ptr)...)
	mark("description", "goJSONSchema")

	typeFindings, err := c.diffType(bObj, hObj, dir, l.ptr)
	if err != nil {
		return nil, err
	}
	findings = append(findings, typeFindings...)
	mark("type")

	findings = append(findings, c.diffEnum(bObj, hObj, dir, l)...)
	mark("enum")
	findings = append(findings, diffConst(bObj, hObj, l.ptr)...)
	mark("const")
	findings = append(findings, diffFormat(bObj, hObj, dir, l.ptr)...)
	mark("format")
	findings = append(findings, diffPattern(bObj, hObj, dir, l.ptr)...)
	mark("pattern")
	findings = append(findings, diffNumericFloor(bObj, hObj, dir, l.ptr, "minimum")...)
	mark("minimum")
	findings = append(findings, diffNumericFloor(bObj, hObj, dir, l.ptr, "minLength")...)
	mark("minLength")
	findings = append(findings, diffNumericFloor(bObj, hObj, dir, l.ptr, "minItems")...)
	mark("minItems")
	findings = append(findings, diffDefault(bObj, hObj, l.ptr)...)
	mark("default")

	propFindings, err := c.diffProperties(bObj, hObj, dir, l)
	if err != nil {
		return nil, err
	}
	findings = append(findings, propFindings...)
	mark("properties", "required")

	apFindings, err := c.diffAdditionalProperties(bObj, hObj, dir, l)
	if err != nil {
		return nil, err
	}
	findings = append(findings, apFindings...)
	mark("additionalProperties")

	itemsFindings, err := c.diffItems(bObj, hObj, dir, l)
	if err != nil {
		return nil, err
	}
	findings = append(findings, itemsFindings...)
	mark("items")

	for _, kw := range []string{"oneOf", "anyOf"} {
		fs, err := c.diffUnion(bObj, hObj, dir, l, kw)
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
		return nil, failClosed("fc-unhandled-keyword", l.ptr, "keyword(s) %v present at this node were not classified by any rule handler", leftover)
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

func (c *diffCtx) diffEnum(base, head map[string]any, dir Direction, l loc) []Finding {
	bRaw, bHas := base["enum"]
	hRaw, hHas := head["enum"]

	if !bHas && !hHas {
		return nil
	}
	if bHas && !hHas {
		// row 13, removed
		return []Finding{ruleFinding("13", dir, severityPair{SeverityMajor, SeverityMinor}, l.ptr+"/enum", "enum keyword removed")}
	}
	if !bHas && hHas {
		// row 13, added
		return []Finding{ruleFinding("13", dir, severityPair{SeverityMinor, SeverityMajor}, l.ptr+"/enum", "enum keyword added")}
	}

	bVals, _ := bRaw.([]any)
	hVals, _ := hRaw.([]any)
	bSet := map[string]any{}
	for _, v := range bVals {
		bSet[canonicalEnumKey(v)] = v
	}
	hSet := map[string]any{}
	for _, v := range hVals {
		hSet[canonicalEnumKey(v)] = v
	}

	var findings []Finding
	// D14/E2/E3/E7/F1: openEnums matches the enum's schema node (the node
	// that itself carries "enum" -- NOT that node's own "/enum" child;
	// COMPATIBILITY.md's own example, "#/$defs/Session/properties/
	// status", names the property node, not ".../status/enum") by the
	// EXACT JSON Pointer of the $defs entry that DECLARES it (l.defPtr/
	// l.oldDefPtr), never a name derived by stripping "$defs"/
	// "properties" segments out of a pointer, and never the traversal
	// pointer a particular caller happened to reach it through (l.ptr) --
	// see this file's own loc doc comment. Matching defPtr/oldDefPtr
	// instead of ptr is what makes the relaxation survive being reached
	// through an UNRELATED def's own $ref (E3), a root surface whose root
	// is itself a $ref (E7), and a second retarget site sharing the same
	// (base, head, direction) triple as an already-visited one (E2).
	//
	// F1: requiring BOTH defPtr (the NEW target's own pointer, as it
	// stands in head) AND oldDefPtr (the OLD target's, as it stood in
	// base) to be open is what stops a retarget from laundering a closed
	// enum through a def that merely happens to be open under a
	// DIFFERENT name -- outside a retarget the two pointers are always
	// identical (see loc's own doc comment), so this is a strict
	// generalization of the single-pointer check it replaces, not a new
	// restriction on the common case.
	isOpen := c.openEnums[l.defPtr] && c.openEnums[l.oldDefPtr]

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
		findings = append(findings, ruleFinding("11", dir, severityPair{p2c, SeverityMinor}, l.ptr+"/enum", "enum value added: "+describeEnumValue(hSet[k])))
	}
	for _, k := range removedKeys {
		findings = append(findings, ruleFinding("12", dir, severityPair{SeverityMinor, SeverityMajor}, l.ptr+"/enum", "enum value removed: "+describeEnumValue(bSet[k])))
	}
	return findings
}

// canonicalEnumKey returns a type-tagged, canonically-serialized key for
// an enum value decoded by encoding/json (nil/bool/float64/string/[]any/
// map[string]any) -- used to compare enum VALUES by their real JSON type
// AND content (D15/D20), not by fmt.Sprint text, which collides values
// across types that merely print the same: fmt.Sprint(nil) and
// fmt.Sprint("<nil>") are both "<nil>"; fmt.Sprint(float64(1)) and
// fmt.Sprint("1") are both "1". Those collisions let a value silently
// change JSON type between base and head (null -> the string "<nil>", the
// number 1 -> the string "1") without diffEnum ever noticing, even though
// every consumer sees a different wire type. The tag prefix keeps values
// of different JSON types from ever sharing a key regardless of what
// json.Marshal happens to produce for either; json.Marshal on a
// map[string]any sorts its keys, so two structurally-equal object/array
// enum values also always key identically.
func canonicalEnumKey(v any) string {
	tag := jsonTypeTag(v)
	data, err := json.Marshal(v)
	if err != nil {
		// v was itself decoded FROM JSON by encoding/json, so re-marshaling
		// it cannot fail -- a panic here would mean a bug in this
		// function's own assumptions, not bad input.
		panic(fmt.Sprintf("canonicalEnumKey: re-marshal failed for %#v: %v", v, err))
	}
	return tag + ":" + string(data)
}

// describeEnumValue renders an enum value for a Finding's own message text
// -- unlike canonicalEnumKey, this is for a human reader, not a comparison
// key, but it still carries the JSON type tag so "number 1 added" and
// "string \"1\" added" never look identical in output the way plain
// fmt.Sprint(v) would.
func describeEnumValue(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("describeEnumValue: re-marshal failed for %#v: %v", v, err))
	}
	return jsonTypeTag(v) + " " + string(data)
}

// jsonTypeTag names the JSON type encoding/json decoded v into.
func jsonTypeTag(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
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
func (c *diffCtx) diffProperties(base, head map[string]any, dir Direction, l loc) ([]Finding, error) {
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
		propLoc := l.child("/properties/" + jsonPointerEscape(name))
		propPtr := propLoc.ptr
		bSchema, inBaseProp := bProps[name]
		hSchema, inHeadProp := hProps[name]

		if bReq[name] && !inBaseProp {
			return nil, failClosed("fc-required-orphan", l.ptr+"/required", "%q is required in base but has no matching properties entry", name)
		}
		if hReq[name] && !inHeadProp {
			return nil, failClosed("fc-required-orphan", l.ptr+"/required", "%q is required in head but has no matching properties entry", name)
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
			nested, err := c.diffNode(bSchema, hSchema, dir, propLoc)
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

func (c *diffCtx) diffAdditionalProperties(base, head map[string]any, dir Direction, l loc) ([]Finding, error) {
	bKind, bSchema := classifyAP(base)
	hKind, hSchema := classifyAP(head)

	apLoc := l.child("/additionalProperties")
	apPtr := apLoc.ptr

	if bKind == apSchema && hKind == apSchema {
		nested, err := c.diffNode(bSchema, hSchema, dir, apLoc)
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

func (c *diffCtx) diffItems(base, head map[string]any, dir Direction, l loc) ([]Finding, error) {
	bItems, bHas := base["items"]
	hItems, hHas := head["items"]
	if bHas != hHas {
		return []Finding{ruleFinding("39", dir, majorMajor, l.ptr+"/items", "items presence changed")}, nil
	}
	if !bHas {
		return nil, nil
	}
	return c.diffNode(bItems, hItems, dir, l.child("/items"))
}

// --- oneOf/anyOf (rows 9, 10, 28, 29, 30, 43) ---

// inlineUnionMemberKey classifies an INLINE (non-$ref) oneOf/anyOf
// member: a discriminated object variant keys by its own
// properties.type.const value; a bare scalar type -- {"type":X} or
// {"type":[X,"null"]}, nothing else besides an optional "description" --
// keys by X itself (bareUnionTypeKey). Anything else, including a bare
// {"type":"object"} node that carries OTHER keywords but no
// discriminator, is not classifiable here and the caller fails closed --
// deliberately UNCHANGED from before E1 (only $ref member classification
// needed fixing; see resolvedUnionMemberKey).
func inlineUnionMemberKey(obj map[string]any) (string, bool) {
	if props, has := obj["properties"].(map[string]any); has {
		if t, has := props["type"].(map[string]any); has {
			if constVal, has := t["const"]; has {
				return "const:" + fmt.Sprint(constVal), true
			}
		}
	}
	if t, ok := bareUnionTypeKey(obj); ok {
		return "type:" + t, true
	}
	return "", false
}

// resolvedUnionMemberKey classifies a $ref member by the shape its
// TARGET resolves to (E1): a PURE object def -- its own "type" is
// EXACTLY the bare string "object" (not an array that merely includes
// "object" alongside something else, e.g. "null", and not absent) --
// is the discriminated variant rows 28/29 describe, keyed by the $ref's
// own target NAME (there is no const value to key by here the way an
// inline discriminated member has one, but a $ref is already a stable,
// unique name to pair on); whatever else that def carries besides "type"
// -- "properties"/"required"/a properties.type.const discriminator --
// does not change this, a real object def always has those alongside
// "type":"object" and none of them are what makes it classifiable here.
// A resolved bare scalar type keys exactly like the equivalent inline
// member would (bareUnionTypeKey). Anything else -- an array-shaped def,
// an alias chain ending somewhere that is neither, a mixed shape (e.g.
// type:["object","null"] alongside its own "properties", or a
// properties.type.const discriminator on a def whose own "type" allows
// null/strings or is absent entirely) -- is not classifiable: ok is
// false and the caller fails closed, exactly as it would for the same
// shape written inline.
//
// F2: this used to ALSO key a def as "ref:"+ref purely because it had a
// properties.type.const, without checking "type" at all -- so a def
// whose "type" allowed null (or any other type) alongside "object", or
// had no "type" keyword at all, was wrongly treated as a discriminated
// object variant (MINOR/MINOR, row 28) instead of failing closed the way
// the identical shape already does when written inline. Every real
// object variant in this repo's own union members already has a bare
// "type":"object", so requiring it here does not change behavior for
// any of them -- it only closes the loophole for a shape none of them
// use.
func resolvedUnionMemberKey(ref string, robj map[string]any) (string, bool) {
	if t, isStr := robj["type"].(string); isStr && t == "object" {
		return "ref:" + ref, true
	}
	if t, ok := bareUnionTypeKey(robj); ok {
		return "type:" + t, true
	}
	return "", false
}

// bareUnionTypeKey reports the scalar type name a bare {"type":X} or
// {"type":[X,"null"]} union member should key on -- a two-element type
// array whose other element is "null" is graded exactly like the
// equivalent inline scalar member (X's own key), the same way rows
// 7/8/9/10 already treat null-vs-non-null members throughout diffType.
// Requires the node carry NOTHING besides "type" and an optional
// "description": any other constraint keyword makes this shape "mixed,"
// not a bare type, and the caller fails closed rather than guess at how
// to key it.
func bareUnionTypeKey(obj map[string]any) (string, bool) {
	if !onlyKeys(obj, "type", "description") {
		return "", false
	}
	switch t := obj["type"].(type) {
	case string:
		if t == "null" {
			return "null", true
		}
		return t, true
	case []any:
		if len(t) != 2 {
			return "", false
		}
		var nonNull string
		nullCount := 0
		for _, el := range t {
			s, ok := el.(string)
			if !ok {
				return "", false
			}
			if s == "null" {
				nullCount++
			} else {
				nonNull = s
			}
		}
		if nullCount == 1 && nonNull != "" {
			return nonNull, true
		}
	}
	return "", false
}

func (c *diffCtx) diffUnion(base, head map[string]any, dir Direction, l loc, keyword string) ([]Finding, error) {
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
		return []Finding{ruleFinding("43", dir, majorMajor, l.ptr+"/"+keyword, keyword+" keyword presence changed")}, nil
	}

	bArr, ok := bRaw.([]any)
	if !ok {
		return nil, failClosed("fc-shape", l.ptr+"/"+keyword, "%s must be an array", keyword)
	}
	hArr, ok := hRaw.([]any)
	if !ok {
		return nil, failClosed("fc-shape", l.ptr+"/"+keyword, "%s must be an array", keyword)
	}

	type member struct {
		key  string
		idx  int
		node any
	}

	// keyOf resolves node (through r, base or head's own resolver) to the
	// shape that actually decides its pairing key -- E1: a $ref member is
	// no longer trusted at face value as "a discriminated variant" the
	// way it used to be; it is resolved first, exactly like the
	// referenced def's OWN content would be classified if it were
	// written inline at this position instead of behind a $ref.
	keyOf := func(r resolver, node any) (string, error) {
		ref := refTargetName(node)
		if ref == "" {
			obj, ok := node.(map[string]any)
			if !ok {
				return "", fmt.Errorf("%s member must be an object or $ref", keyword)
			}
			if k, ok := inlineUnionMemberKey(obj); ok {
				return k, nil
			}
			return "", fmt.Errorf("cannot pair this %s member (no $ref or properties.type.const to key on, and not a bare scalar type)", keyword)
		}

		resolved, err := r.resolve(node, nil)
		if err != nil {
			return "", err
		}
		robj, isObj := resolved.(map[string]any)
		if !isObj {
			return "", fmt.Errorf("$ref %q resolves to the boolean schema literal, which cannot be paired as a %s member", ref, keyword)
		}
		if k, ok := resolvedUnionMemberKey(ref, robj); ok {
			return k, nil
		}
		return "", fmt.Errorf("$ref %q resolves to a shape (neither an object def nor a bare scalar type) that cannot be paired as a %s member", ref, keyword)
	}

	baseMembers := map[string]member{}
	for i, n := range bArr {
		k, err := keyOf(c.baseR, n)
		if err != nil {
			return nil, failClosed("fc-oneof-unpairable", fmt.Sprintf("%s/%s/%d", l.ptr, keyword, i), "%v", err)
		}
		if _, dup := baseMembers[k]; dup {
			return nil, failClosed("fc-oneof-unpairable", fmt.Sprintf("%s/%s/%d", l.ptr, keyword, i), "duplicate pairing key %q in base %s", k, keyword)
		}
		baseMembers[k] = member{k, i, n}
	}
	headMembers := map[string]member{}
	for i, n := range hArr {
		k, err := keyOf(c.headR, n)
		if err != nil {
			return nil, failClosed("fc-oneof-unpairable", fmt.Sprintf("%s/%s/%d", l.ptr, keyword, i), "%v", err)
		}
		if _, dup := headMembers[k]; dup {
			return nil, failClosed("fc-oneof-unpairable", fmt.Sprintf("%s/%s/%d", l.ptr, keyword, i), "duplicate pairing key %q in head %s", k, keyword)
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
			removedPtr := fmt.Sprintf("%s/%s/%d", l.ptr, keyword, bm.idx)
			switch {
			case k == "type:null":
				findings = append(findings, ruleFinding("10", dir, severityPair{SeverityMinor, SeverityMajor}, removedPtr, "null variant removed from "+keyword))
			case strings.HasPrefix(k, "type:"):
				// D9: a bare {"type":X} branch (X != null) is not a
				// discriminated variant a robust consumer can just skip
				// over -- it is the field's own wire type losing a
				// member, the same change row 8 already grades when
				// spelled as a `type` array. Only "ref:"/"const:" keyed
				// members (a $ref to an object def, or an object with a
				// properties.type.const discriminator) are the
				// discriminated-object variants rows 28/29 are for.
				findings = append(findings, ruleFinding("8", dir, severityPair{SeverityMinor, SeverityMajor}, removedPtr, "scalar-type union member removed (type narrowed): "+k))
			default:
				findings = append(findings, ruleFinding("29", dir, majorMajor, removedPtr, "union member removed: "+k))
			}
		case !inBase && inHead:
			addedPtr := fmt.Sprintf("%s/%s/%d", l.ptr, keyword, hm.idx)
			switch {
			case k == "type:null":
				findings = append(findings, ruleFinding("9", dir, severityPair{SeverityMajor, SeverityMinor}, addedPtr, "null variant added to "+keyword))
			case strings.HasPrefix(k, "type:"):
				// D9: same reasoning as the removed case above, mirrored:
				// a non-null bare-type branch added to an existing union
				// widens the field's own wire type (row 7), not a MINOR
				// "variant added" a consumer could simply ignore.
				findings = append(findings, ruleFinding("7", dir, severityPair{SeverityMajor, SeverityMinor}, addedPtr, "scalar-type union member added (type widened): "+k))
			default:
				findings = append(findings, ruleFinding("28", dir, severityPair{SeverityMinor, SeverityMinor}, addedPtr, "union member added: "+k))
			}
		default:
			memberLoc := l.child(fmt.Sprintf("/%s/%d", keyword, hm.idx))
			nested, err := c.diffNode(bm.node, hm.node, dir, memberLoc)
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
			wrapper := Finding{RuleID: "30", Severity: worst, Pointer: memberLoc.ptr, Message: "union member changed: " + k}
			findings = append(findings, wrapper)
			findings = append(findings, nested...)
		}
	}
	return findings, nil
}
