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
