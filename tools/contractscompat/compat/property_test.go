package compat

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
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

// collectSchemaPositions walks root the same way walkSchema does
// (keywords.go), collecting every schema-node position that is a
// map[string]any (skipping the boolean schema literals true/false, which
// have no keyword to mutate). The returned nodes are references into
// root's own tree -- mutating pos.node mutates root in place.
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
			for _, sub := range defs {
				walk(sub)
			}
		}
		if props, ok := obj["properties"].(map[string]any); ok {
			for _, sub := range props {
				walk(sub)
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

// mutateSchemaTree applies exactly ONE random change/add/remove mutation
// to headRoot (already a mutable deep copy of the real base document), at
// a random schema-position, touching only an allowlisted keyword.
func mutateSchemaTree(rng *rand.Rand, headRoot map[string]any) {
	positions := collectSchemaPositions(headRoot)
	if len(positions) == 0 {
		return
	}
	pos := positions[rng.Intn(len(positions))]

	addOrChange := func() {
		kw := mutableLeafKeywords[rng.Intn(len(mutableLeafKeywords))]
		pos.node[kw] = randomLeafValue(rng, kw)
	}

	switch rng.Intn(3) {
	case 0:
		// Change or add a leaf keyword.
		addOrChange()
	case 1:
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
		if len(present) == 0 {
			addOrChange()
			return
		}
		delete(pos.node, present[rng.Intn(len(present))])
	case 2:
		// Change a leaf keyword ALREADY present at this node (biases the
		// mutation set toward "an existing constraint changed value," not
		// only "a new one appeared").
		var present []string
		for _, kw := range mutableLeafKeywords {
			if _, ok := pos.node[kw]; ok {
				present = append(present, kw)
			}
		}
		if len(present) == 0 {
			addOrChange()
			return
		}
		kw := present[rng.Intn(len(present))]
		pos.node[kw] = randomLeafValue(rng, kw)
	}
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
