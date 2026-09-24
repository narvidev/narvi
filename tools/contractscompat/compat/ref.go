package compat

import (
	"fmt"
	"sort"
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

// resolver dereferences $ref nodes against one file's own $defs map, with
// a cycle guard (a def that (directly or transitively) $refs itself would
// otherwise recurse forever -- none of today's schemas do this, but a
// future one might, and silently stack-overflowing is a worse failure
// mode than fail-closing).
type resolver struct {
	defs map[string]any
}

// resolve follows node's $ref chain (if any) to a concrete schema node
// (object or bool). A node with no $ref is returned unchanged. path
// accumulates the def names visited, for the cycle-guard message.
func (r resolver) resolve(node any, path []string) (any, error) {
	obj, ok := node.(map[string]any)
	if !ok {
		return node, nil
	}
	raw, ok := obj["$ref"]
	if !ok {
		return node, nil
	}
	s, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("$ref must be a string")
	}
	name, err := localRefTarget(s)
	if err != nil {
		return nil, err
	}
	for _, seen := range path {
		if seen == name {
			return nil, fmt.Errorf("$ref cycle detected: %s -> %s", strings.Join(path, " -> "), name)
		}
	}
	target, ok := r.defs[name]
	if !ok {
		return nil, fmt.Errorf("$ref target %q not found in $defs", name)
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

// effectiveNode computes the schema a node with (or without) a $ref
// actually enforces: the $ref target's own resolved content (if any),
// overlaid by every SIBLING keyword on the raw node itself. Draft
// 2020-12 applies a $ref together with its siblings (they are ANDed
// together), so a node like {"$ref": "#/$defs/Digest", "required":
// ["archDecisions"]} must be diffed as Digest's own content PLUS the
// sibling constraint, never as Digest alone -- silently dropping the
// siblings (the bug this replaces) hid every compatibility break a PR
// introduced next to a $ref (C5, C8).
//
// resolved is always a map (callers only invoke this once they know the
// $ref-resolved value is an object, never the boolean schema literals
// true/false -- see diffNode, which handles that case itself before ever
// calling effectiveNode, precisely because collapsing `false` into the
// empty object `{}` here would silently turn "reject everything" into
// "accept everything").
//
// "required" and "properties" are merged as a UNION (both the target's
// names/keys and the sibling's own apply together, matching $ref's real
// AND semantics). Every other sibling keyword OVERRIDES the target's own
// value. That is not a full JSON Schema intersection (a sibling `type`
// narrower than the target's own `type` should, strictly, become the
// intersection of the two, not simply replace it) -- but this checker
// only needs to classify a CHANGE between two already-merged views, not
// evaluate a schema against data, and overriding still correctly detects
// "a constraint was added or changed here" for every case in this
// repo's own corpus and the review's own reproductions.
func effectiveNode(resolved map[string]any, raw any) map[string]any {
	out := make(map[string]any, len(resolved))
	for k, v := range resolved {
		out[k] = v
	}
	rawObj, ok := raw.(map[string]any)
	if !ok {
		return out
	}
	for k, v := range rawObj {
		switch k {
		case "$ref":
			continue
		case "required":
			out["required"] = unionStringArrays(out["required"], v)
		case "properties":
			out["properties"] = unionPropertyMaps(out["properties"], v)
		default:
			out[k] = v
		}
	}
	return out
}

// unionStringArrays merges two JSON-decoded ([]any of string) arrays into
// one deduplicated, sorted []any -- used to union a $ref target's own
// "required" list with a sibling "required" list on the same node.
// Non-string elements (which validateTypeShape/walkSchema's own
// "required" shape check would already have fail-closed on, elsewhere)
// are simply skipped rather than panicking.
func unionStringArrays(a, b any) []any {
	seen := map[string]bool{}
	var names []string
	add := func(v any) {
		arr, _ := v.([]any)
		for _, el := range arr {
			s, ok := el.(string)
			if !ok || seen[s] {
				continue
			}
			seen[s] = true
			names = append(names, s)
		}
	}
	add(a)
	add(b)
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	out := make([]any, len(names))
	for i, n := range names {
		out[i] = n
	}
	return out
}

// unionPropertyMaps merges two "properties" objects (map[string]any) into
// one, keyed by property name -- used to union a $ref target's own
// declared properties with any the sibling node declares directly. On a
// name collision the sibling's own schema wins (this repo's real schemas
// never declare "properties" directly beside a "$ref" today, so this is a
// defensive default, not something any real fixture exercises).
func unionPropertyMaps(a, b any) map[string]any {
	out := map[string]any{}
	if am, ok := a.(map[string]any); ok {
		for k, v := range am {
			out[k] = v
		}
	}
	if bm, ok := b.(map[string]any); ok {
		for k, v := range bm {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
