package compat

import (
	"fmt"
	"strings"
)

// localRefTarget extracts the $defs name from a local pointer of the form
// "#/$defs/<Name>" -- the only $ref form every schema under /contracts
// uses (§6.3 design spec §1: "every $ref is local"). Any other form (a
// fragment into anything but $defs, an absolute/relative URL, a $ref into
// a nested pointer under a def, or an empty string) is rejected: the
// design spec's own §3 says resolution covers exactly this one form and
// FAIL-CLOSED on anything else.
func localRefTarget(ref string) (string, error) {
	const prefix = "#/$defs/"
	if !strings.HasPrefix(ref, prefix) {
		return "", fmt.Errorf("non-local or non-$defs $ref %q is not supported", ref)
	}
	name := strings.TrimPrefix(ref, prefix)
	if name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("$ref %q must point directly at a single $defs entry", ref)
	}
	return jsonPointerUnescape(name), nil
}

// jsonPointerUnescape reverses jsonPointerEscape ("~1" -> "/", "~0" ->
// "~"), needed because $defs names are opaque JSON Pointer segments,
// technically escapable even though nothing in this repo's own schemas
// today uses a name containing "/" or "~".
func jsonPointerUnescape(s string) string {
	s = strings.ReplaceAll(s, "~1", "/")
	s = strings.ReplaceAll(s, "~0", "~")
	return s
}

// refAllowedSiblingKeys is the CLOSED set of keywords a node carrying
// "$ref" may carry besides it. In draft 2020-12, $ref is a CONJUNCTION
// with its siblings (they are ANDed together), not an override -- every
// earlier attempt to MODEL that conjunction (merging "required"/
// "properties" as a union, letting every other keyword override the
// target's own value) kept getting the semantics wrong in a new way each
// review round (a wrapper def's own siblings dropped on a retarget, a
// sibling "properties" shadowing the target's stricter one, a sibling
// "additionalProperties: false" merging away to a no-op...). This repo's
// own five schema files never need the general case: every "$ref" sibling
// in them, at HEAD and at origin/main alike, is "description" and nothing
// else (verified by hand across all five files before writing this rule).
// So instead of modelling the conjunction, this checker refuses to: a
// "$ref" node may carry AT MOST "description" besides it (a pure
// annotation, diffed on its own -- see diffRefSiblingDescription -- never
// a wire constraint). Anything else FAILS CLOSED, naming the pointer, on
// either side of the diff.
var refAllowedSiblingKeys = map[string]bool{
	"description": true,
}

// checkRefSiblings validates that obj, a node known to carry "$ref", has
// no sibling keyword besides what refAllowedSiblingKeys permits. Called
// both by resolve (for every node it dereferences, including every def
// along an alias chain) and directly by diffNode for the RETARGETED path
// (diffRetargetedRef resolves each side's DEF content, never the raw
// referencing node itself, so diffNode checks the referencing node's own
// siblings up front instead) -- so DiffDef stays a robust, independently
// correct entry point even when called directly, the way this package's
// own tests already do, rather than depending on walkSchema having run
// first (walkSchema does also enforce this, structurally, for every real
// PR that goes through DiffSurface/Compare -- this is belt-and-suspenders
// for the lower-level entry point, the same reasoning behind
// diffResolved's own exhaustiveness assertion and collectRefs' sibling
// assert).
func checkRefSiblings(obj map[string]any) error {
	for k := range obj {
		if k != "$ref" && !refAllowedSiblingKeys[k] {
			return fmt.Errorf("$ref node carries disallowed sibling keyword %q (only \"description\" may accompany $ref -- $ref is a conjunction in draft 2020-12, and this checker refuses to model that instead of getting it wrong)", k)
		}
	}
	return nil
}

// resolver dereferences $ref nodes against one file's own $defs map, with
// a cycle guard (a def that (directly or transitively) $refs itself would
// otherwise recurse forever -- see diffCtx.visitOnce for the SEPARATE
// guard against unbounded recursion through a self-referencing def's own
// properties/items, which this cycle guard does not cover since it only
// ever sees one $ref-to-$ref chain at a time).
type resolver struct {
	defs map[string]any
}

// resolve follows node's $ref chain (if any) to a concrete schema node
// (an object; never a boolean -- see resolveDef). A node with no $ref is
// returned unchanged. path accumulates the def names visited, for the
// cycle-guard message.
func (r resolver) resolve(node any, path []string) (any, error) {
	obj, ok := node.(map[string]any)
	if !ok {
		return node, nil
	}
	raw, ok := obj["$ref"]
	if !ok {
		return node, nil
	}
	if err := checkRefSiblings(obj); err != nil {
		return nil, err
	}
	s, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("$ref must be a string")
	}
	name, err := localRefTarget(s)
	if err != nil {
		return nil, err
	}
	return r.resolveDef(name, path)
}

// resolveDef looks up name in r.defs and follows any further $ref chain
// from there, applying the exact same rules a live "$ref": name node
// would: cycle detection, and FAIL-CLOSED if the def that name names is
// itself the boolean schema literal true/false (a "$ref" to a boolean
// schema has no content to merge with the referencing node's own
// siblings, and silently treating it as the empty object `{}` would
// invert `false`'s "reject everything" into "accept everything" --
// callers that legitimately want a bare boolean $defs entry as a node's
// OWN value, never referenced via $ref, still get one: this rejection
// only fires when resolving THROUGH a $ref).
func (r resolver) resolveDef(name string, path []string) (any, error) {
	for _, seen := range path {
		if seen == name {
			return nil, fmt.Errorf("$ref cycle detected: %s -> %s", strings.Join(path, " -> "), name)
		}
	}
	target, ok := r.defs[name]
	if !ok {
		return nil, fmt.Errorf("$ref target %q not found in $defs", name)
	}
	if _, isBool := target.(bool); isBool {
		return nil, fmt.Errorf("$ref target %q is the boolean schema literal, which is not supported behind a $ref", name)
	}
	return r.resolve(target, append(path, name))
}

// refTargetName returns the $defs name a node's own (non-recursive)
// $ref points at, or "" if node has no $ref at all. Used to detect a
// same-name vs. retargeted vs. newly-$ref'd/un-$ref'd $ref between base
// and head at the same pointer, before either side is dereferenced.
func refTargetName(node any) string {
	obj, ok := node.(map[string]any)
	if !ok {
		return ""
	}
	raw, ok := obj["$ref"]
	if !ok {
		return ""
	}
	s, ok := raw.(string)
	if !ok {
		return ""
	}
	name, err := localRefTarget(s)
	if err != nil {
		return ""
	}
	return name
}
