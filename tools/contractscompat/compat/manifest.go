package compat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Manifest mirrors contracts/manifest.json (§6.3 design spec §3): one row
// per schema file under /contracts, plus the day-one openEnums relaxation
// list. Relaxations (openEnums, a surface's retired status) are only ever
// read from the MERGE-BASE copy of this file -- see COMPATIBILITY.md and
// this package's own doc comments on DirectionOf/ParseManifest's callers
// for why: a PR that both adds an enum value AND opens the enum, or both
// deletes a file AND retires it in the same diff, would otherwise grade
// its own homework.
//
// G3 (round 5 review): every OpenEnums entry is qualified by the surface
// it belongs to ("<surface path>#<json pointer>", e.g. "rest/v1/dtos.
// schema.json#/$defs/Session/properties/status") -- see
// OpenEnumSetForSurface's own doc comment for why an unqualified entry
// used to leak a relaxation authored for one surface into any OTHER
// surface with a same-named def.
type Manifest struct {
	Version   string            `json:"version"`
	Surfaces  []ManifestSurface `json:"surfaces"`
	OpenEnums []string          `json:"openEnums"`
}

// ManifestSurface is one row of Manifest.Surfaces.
type ManifestSurface struct {
	Path      string `json:"path"`
	Direction string `json:"direction"` // by-suffix | platform-to-client | client-to-platform | both
	Status    string `json:"status"`    // current | deprecated | retired
}

// ManifestSurface.Direction/Status's own string vocabulary.
const (
	DirectiveBySuffix = "by-suffix"
	StatusCurrent     = "current"
	StatusDeprecated  = "deprecated"
	StatusRetired     = "retired"
)

// ParseManifest decodes manifest.json bytes. It deliberately does not
// validate cross-references against any schema set -- callers (the CLI's
// vacuous-pass guards) do that, since what counts as "consistent" differs
// between the base and head manifest.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest: %w", err)
	}
	return m, nil
}

// openEnumEntry is one parsed contracts/manifest.json "openEnums" string,
// split into the surface it is qualified for and the in-document JSON
// Pointer within that surface's own file.
type openEnumEntry struct {
	surface string // e.g. "rest/v1/dtos.schema.json"
	pointer string // e.g. "#/$defs/Session/properties/status" -- defPtr-shaped, leading "#"
}

// parseOpenEnumEntry splits one raw "openEnums" string into its surface
// and pointer halves, per the qualified format COMPATIBILITY.md's own
// "Relaxations" section documents: "<surface path>#<json pointer>". The
// surface half must be non-empty (an entry cannot be "#/$defs/..." with
// nothing before the "#" -- that is exactly the unqualified shape G3
// closes), and the pointer half must itself be a well-formed
// in-document JSON Pointer ("#/...", matching the defPtr/oldDefPtr shape
// diffEnum already looks entries up by).
func parseOpenEnumEntry(raw string) (openEnumEntry, error) {
	i := strings.IndexByte(raw, '#')
	if i <= 0 {
		return openEnumEntry{}, fmt.Errorf("openEnums entry %q is not qualified by a surface path -- want \"<surface path>#<json pointer>\" (e.g. \"rest/v1/dtos.schema.json#/$defs/Session/properties/status\")", raw)
	}
	surface, pointer := raw[:i], raw[i:]
	if pointer == "#" || !strings.HasPrefix(pointer, "#/") {
		return openEnumEntry{}, fmt.Errorf("openEnums entry %q has a malformed JSON Pointer half %q", raw, pointer)
	}
	return openEnumEntry{surface: surface, pointer: pointer}, nil
}

// ValidateOpenEnums checks that every m.OpenEnums entry is qualified (G3)
// and names a surface actually present in m.Surfaces -- called against
// the MERGE-BASE manifest only, same as every other relaxation (see this
// file's own doc comment). An unqualified entry, or one naming a surface
// this manifest does not list, is a manifest authoring error this
// checker fails closed on rather than silently ignoring (which would
// have exactly G3's own failure mode: the entry quietly does nothing for
// the surface its author meant, while -- before this fix -- doing
// something unintended for whichever OTHER surface happened to share a
// def name).
func (m Manifest) ValidateOpenEnums() error {
	known := make(map[string]bool, len(m.Surfaces))
	for _, s := range m.Surfaces {
		known[s.Path] = true
	}
	for _, raw := range m.OpenEnums {
		entry, err := parseOpenEnumEntry(raw)
		if err != nil {
			return err
		}
		if !known[entry.surface] {
			return fmt.Errorf("openEnums entry %q names surface %q, which is not in this manifest's own surfaces list", raw, entry.surface)
		}
	}
	return nil
}

// OpenEnumSetForSurface returns the openEnums relaxations that apply to
// ONE surface, keyed by the in-document pointer diffEnum looks entries up
// by (l.defPtr/l.oldDefPtr, both "#/..."-rooted) -- an entry qualified
// for a DIFFERENT surface never contributes here (G3, round 5 review): a
// relaxation authored for rest/v1/dtos.schema.json's own $defs.Automation
// must never open an enum in client-ws/v1/protocol.schema.json just
// because some unrelated def THERE happens to be named "Automation" too
// -- reproduced against the real binary as a two-PR exploit (PR1 adds the
// same-named def to a different surface, PR2 widens its enum and is
// wrongly scored MINOR). Compare calls this once per surface, from the
// merge-base manifest only, after ValidateOpenEnums has already
// confirmed every entry parses and names a real surface -- an entry that
// still fails to parse here (should be unreachable) is simply skipped,
// since this method -- like the OpenEnumSet it replaces -- is a pure,
// error-free lookup.
func (m Manifest) OpenEnumSetForSurface(path string) map[string]bool {
	out := map[string]bool{}
	for _, raw := range m.OpenEnums {
		entry, err := parseOpenEnumEntry(raw)
		if err != nil || entry.surface != path {
			continue
		}
		out[entry.pointer] = true
	}
	return out
}

// SurfaceByPath finds a surface row by its path, or (zero, false).
func (m Manifest) SurfaceByPath(path string) (ManifestSurface, bool) {
	for _, s := range m.Surfaces {
		if s.Path == path {
			return s, true
		}
	}
	return ManifestSurface{}, false
}

// DefDirections computes the Direction of every def name that exists in
// baseDefs and/or headDefs, for one surface file, given that surface's own
// manifest directive.
//
// For a fixed directive (platform-to-client, client-to-platform, both)
// every def in the file gets that Direction, full stop -- sandbox-ws
// events.schema.json ("both": agent->CP is C2P, CP->browser is P2C) is
// exactly why a per-def reachability computation is not always the
// mechanism; sometimes the file itself simply is bidirectional.
//
// For "by-suffix" (rest/v1/dtos.schema.json, client-ws/v1/protocol.
// schema.json), a def's own name decides its base category (a name
// ending "Request" is C2P, everything else is P2C), refined by the
// "reachable from both kinds of root" rule (§6.3 design spec §1's 8 REST
// helper defs): a def nested (via $ref, transitively) inside at least one
// def of EACH category becomes DirBoth. Reachability is computed as the
// UNION of base's and head's own $ref graphs, deliberately -- computing
// it from head alone would let a PR silently escape a rule by rewiring a
// $ref to change a def's own reachability in the same diff that changes
// its shape.
func DefDirections(directive string, baseDefs, headDefs map[string]any) (map[string]Direction, error) {
	names := map[string]bool{}
	for n := range baseDefs {
		names[n] = true
	}
	for n := range headDefs {
		names[n] = true
	}

	if directive != DirectiveBySuffix {
		d, err := fixedDirection(directive)
		if err != nil {
			return nil, err
		}
		out := make(map[string]Direction, len(names))
		for n := range names {
			out[n] = d
		}
		return out, nil
	}

	baseReach, err := reachabilityByCategory(baseDefs)
	if err != nil {
		return nil, err
	}
	headReach, err := reachabilityByCategory(headDefs)
	if err != nil {
		return nil, err
	}

	out := make(map[string]Direction, len(names))
	for n := range names {
		cats := map[Direction]bool{}
		for cat := range baseReach[n] {
			cats[cat] = true
		}
		for cat := range headReach[n] {
			cats[cat] = true
		}
		switch {
		case cats[DirP2C] && cats[DirC2P]:
			out[n] = DirBoth
		case cats[DirC2P]:
			out[n] = DirC2P
		case cats[DirP2C]:
			out[n] = DirP2C
		default:
			// A def reachable from nothing (not even its own name's
			// suffix category, which cannot happen since a def is always
			// trivially reachable from itself) -- fall back to its own
			// suffix as a defensive default.
			out[n] = suffixCategory(n)
		}
	}
	return out, nil
}

func fixedDirection(directive string) (Direction, error) {
	switch directive {
	case string(DirP2C):
		return DirP2C, nil
	case string(DirC2P):
		return DirC2P, nil
	case string(DirBoth):
		return DirBoth, nil
	default:
		return "", fmt.Errorf("unknown manifest direction directive %q", directive)
	}
}

func suffixCategory(name string) Direction {
	if len(name) > len("Request") && name[len(name)-len("Request"):] == "Request" {
		return DirC2P
	}
	return DirP2C
}

// reachabilityByCategory returns, for every def name in defs, the set of
// root categories (DirP2C/DirC2P, by suffix) that can reach it via a
// transitive $ref chain -- including itself, trivially, under its own
// suffix category.
func reachabilityByCategory(defs map[string]any) (map[string]map[Direction]bool, error) {
	edges := make(map[string][]string, len(defs))
	for name, node := range defs {
		refs := map[string]bool{}
		if err := collectRefs(node, refs); err != nil {
			return nil, failClosed("fc-ref-sibling-reachability", "#/$defs/"+jsonPointerEscape(name), "%v", err)
		}
		for r := range refs {
			edges[name] = append(edges[name], r)
		}
	}

	result := make(map[string]map[Direction]bool, len(defs))
	for name := range defs {
		result[name] = map[Direction]bool{}
	}

	for root := range defs {
		cat := suffixCategory(root)
		visited := map[string]bool{}
		queue := []string{root}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if visited[cur] {
				continue
			}
			visited[cur] = true
			if _, ok := result[cur]; ok {
				result[cur][cat] = true
			}
			for _, next := range edges[cur] {
				if !visited[next] {
					queue = append(queue, next)
				}
			}
		}
	}
	return result, nil
}

// collectRefs gathers every $ref target name reachable by walking node's
// own schema-shaped structure (properties/items/oneOf/anyOf/
// additionalProperties), stopping at each $ref itself -- the def IT names
// is a separate node in the defs map, visited on its own turn.
//
// A $ref node may carry no schema-position sibling at all (ref.go's
// refAllowedSiblingKeys permits only "description" beside "$ref", and
// walkSchema has already fail-closed on anything else, for BOTH base and
// head, before reachability ever runs -- see DiffSurface's own call
// order). So there is structurally nothing left to walk into once a $ref
// is found; this function's own early `return` after recording it is
// correct BY CONSTRUCTION, not by omission (that was D2/D8/D11's actual
// bug: this function used to return early while the diff engine still
// treated properties/items/etc. beside a $ref as live, comparable
// content, so a def reached only through such a sibling was invisible to
// reachability). The loop below is the assertion that backs that
// construction: it FAILS CLOSED if a $ref node is ever found carrying one
// of these sibling keywords anyway, rather than silently mis-scoping
// reachability the way the pre-fix code did.
func collectRefs(node any, out map[string]bool) error {
	obj, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	if ref, ok := obj["$ref"].(string); ok {
		if name, err := localRefTarget(ref); err == nil {
			out[name] = true
		}
		for _, kw := range []string{"properties", "items", "oneOf", "anyOf", "additionalProperties"} {
			if _, has := obj[kw]; has {
				return fmt.Errorf("$ref node unexpectedly carries schema-position sibling %q -- walkSchema's $ref-sibling guard should already have rejected this", kw)
			}
		}
		return nil
	}
	if props, ok := obj["properties"].(map[string]any); ok {
		for _, v := range props {
			if err := collectRefs(v, out); err != nil {
				return err
			}
		}
	}
	if items, ok := obj["items"]; ok {
		if err := collectRefs(items, out); err != nil {
			return err
		}
	}
	for _, kw := range []string{"oneOf", "anyOf"} {
		if arr, ok := obj[kw].([]any); ok {
			for _, v := range arr {
				if err := collectRefs(v, out); err != nil {
					return err
				}
			}
		}
	}
	if ap, ok := obj["additionalProperties"]; ok {
		if _, isBool := ap.(bool); !isBool {
			if err := collectRefs(ap, out); err != nil {
				return err
			}
		}
	}
	return nil
}
