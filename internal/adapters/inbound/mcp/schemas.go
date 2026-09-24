package mcp

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/narvidev/narvi/contracts"
)

// restDefsDocument is the shape this file needs from
// contracts/rest/v1/dtos.schema.json -- just the top-level $defs map,
// each entry left as a raw JSON object (json.RawMessage) so this package
// never depends on the SHAPE of an individual $def, only that it IS one.
type restDefsDocument struct {
	Defs map[string]json.RawMessage `json:"$defs"`
}

// loadedRestDefs memoizes one parse of the embedded rest/v1 schema for
// the lifetime of the process -- contracts.FS is a compiled-in
// embed.FS, so its content can never change at runtime; re-parsing it
// per request (§43.7's own "per-request server construction" cost note)
// would be pure waste.
var loadedRestDefs = sync.OnceValues(func() (map[string]json.RawMessage, error) {
	data, err := contracts.FS.ReadFile("rest/v1/dtos.schema.json")
	if err != nil {
		return nil, fmt.Errorf("mcp: read embedded rest/v1/dtos.schema.json: %w", err)
	}
	var doc restDefsDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("mcp: parse embedded rest/v1/dtos.schema.json: %w", err)
	}
	if len(doc.Defs) == 0 {
		return nil, fmt.Errorf("mcp: embedded rest/v1/dtos.schema.json has no $defs -- almost certainly a read/parse bug, not a genuinely empty contract")
	}
	return doc.Defs, nil
})

// inputSchema returns name's own $def entry from rest/v1/dtos.schema.json
// VERBATIM -- unmarshaled into a map so the SDK's own mcp.Tool.InputSchema
// field (type any) can hold it directly, but otherwise untouched: no
// bundling, no rewriting. This is deliberate (technical plan §43.10): our
// three input shapes (ListModelsToolRequest, ListSessionsToolRequest,
// GetSessionToolRequest) reference no OTHER $def, so the wire inputSchema
// a client sees is byte-derived from /contracts with nothing added or
// removed. TestToolInputSchemas_ComeFromContracts pins exactly this: each
// tool's InputSchema deep-equals this function's own return value.
func inputSchema(name string) (map[string]any, error) {
	defs, err := loadedRestDefs()
	if err != nil {
		return nil, err
	}
	raw, ok := defs[name]
	if !ok {
		return nil, fmt.Errorf("mcp: no $def named %q in rest/v1/dtos.schema.json", name)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("mcp: unmarshal $def %q: %w", name, err)
	}
	return m, nil
}

// bundleOutputSchema returns a SELF-CONTAINED JSON Schema document for
// name's own $def: name's own $def plus every OTHER $def it transitively
// $refs (e.g. "Session" pulls in "AutomationReposElem"; "ModelCatalog"
// pulls in "ModelCatalogProvider" -> "ModelCatalogModel" ->
// "ModelCatalogCost"), so a client holding ONLY this document -- with no
// access to fetch rest/v1/dtos.schema.json itself -- can still resolve
// every "#/$defs/<Name>" reference name's own sub-schema contains. Used
// for the three tools' OutputSchema (Session, ListSessionsResponse,
// ModelCatalog are all reused UNCHANGED from /contracts, never a
// hand-written shape -- technical plan §43.10).
//
// The bundled document's shape is:
//
//	{"$schema": "...", "$ref": "#/$defs/<name>", "$defs": {<name>: ..., ...}}
//
// map[string]any's own keys sort alphabetically under encoding/json's
// marshaling (Go's own documented behavior for map values), so this
// function's output is deterministic across calls and across processes
// -- load-bearing for testdata/tools.golden.json's own byte-for-byte
// comparison.
func bundleOutputSchema(name string) (map[string]any, error) {
	defs, err := loadedRestDefs()
	if err != nil {
		return nil, err
	}

	closure := map[string]json.RawMessage{}
	var walk func(n string) error
	walk = func(n string) error {
		if _, ok := closure[n]; ok {
			return nil
		}
		raw, ok := defs[n]
		if !ok {
			return fmt.Errorf("mcp: bundling %q: no $def named %q in rest/v1/dtos.schema.json", name, n)
		}
		closure[n] = raw
		refs, err := collectDefRefs(raw)
		if err != nil {
			return fmt.Errorf("mcp: bundling %q: %w", name, err)
		}
		for _, ref := range refs {
			if err := walk(ref); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(name); err != nil {
		return nil, err
	}

	bundledDefs := make(map[string]any, len(closure))
	for n, raw := range closure {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("mcp: bundling %q: unmarshal $def %q: %w", name, n, err)
		}
		bundledDefs[n] = v
	}

	return map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$ref":    "#/$defs/" + name,
		"$defs":   bundledDefs,
	}, nil
}

// collectDefRefs walks raw (one $def's own JSON tree) looking for every
// "$ref" string of the shape "#/$defs/<Name>", returning the referenced
// names, deduplicated and sorted (sorted purely for deterministic error
// messages/test output -- bundleOutputSchema's own walk visits them via
// a plain map regardless of order).
func collectDefRefs(raw json.RawMessage) ([]string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	collectDefRefsFrom(v, seen)
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

const defRefPrefix = "#/$defs/"

func collectDefRefsFrom(v any, seen map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "$ref" {
				if s, ok := val.(string); ok && len(s) > len(defRefPrefix) && s[:len(defRefPrefix)] == defRefPrefix {
					seen[s[len(defRefPrefix):]] = true
				}
				continue
			}
			collectDefRefsFrom(val, seen)
		}
	case []any:
		for _, elem := range t {
			collectDefRefsFrom(elem, seen)
		}
	}
}
