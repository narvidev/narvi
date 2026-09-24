package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

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
		// "type":"object" at the ROOT, alongside "$ref" (2020-12 does
		// not ignore sibling keywords next to "$ref" the way draft-07
		// did, and every $def this bundles -- Session,
		// ListSessionsResponse, ModelCatalog -- is itself
		// {"type":"object",...}, so this adds no real constraint beyond
		// what "$ref" alone already implies). The legacy MCP protocol
		// revisions this server also speaks (2025-11-25 and earlier)
		// require exactly this: 2025-11-25's own Tool.outputSchema spec
		// says it is "Currently restricted to type: 'object' at the
		// root level" -- a bundle with no root "type" at all (the
		// shape this function returned before) fails to even PARSE as
		// an outputSchema for an SDK built against one of those
		// revisions (e.g. the TypeScript SDK's own zod
		// ListToolsResultSchema), not merely a stricter-than-necessary
		// validation.
		"type":  "object",
		"$ref":  "#/$defs/" + name,
		"$defs": bundledDefs,
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

// restDefsCompiler memoizes ONE santhosh-tekuri/jsonschema/v6 compiler
// with rest/v1/dtos.schema.json added as a resource (format assertions
// ON, so "format":"uuid" on GetSessionToolRequest.sessionId is actually
// enforced -- mirroring contracts/contractstest/helpers_test.go's own
// newCompiler) plus that document's own "$id", so any of its $defs can be
// compiled by fragment. This is the validation layer this package's
// tools always needed and never had: registerTools uses the SDK's raw,
// non-generic Server.AddTool(*Tool, ToolHandler) path specifically
// because that path's own OWN InputSchema is never checked against
// arguments by the SDK itself (go-sdk v1.8.0's own doc comment on that
// call: "Unmarshaling the arguments and validating them against the
// input schema are the caller's responsibility") -- validateArguments
// below is this package acting as that caller, once, in one place,
// before ANY tool's own BuildRequest or twin ever sees an argument.
// restDefsResource bundles the compiler produced by restDefsCompiler
// with the "$id" its one resource was added under -- sync.OnceValues
// only supports two return values, hence the struct rather than a bare
// (*jsonschema.Compiler, string) pair.
type restDefsResource struct {
	compiler *jsonschema.Compiler
	id       string
}

var restDefsCompiler = sync.OnceValues(func() (restDefsResource, error) {
	data, err := contracts.FS.ReadFile("rest/v1/dtos.schema.json")
	if err != nil {
		return restDefsResource{}, fmt.Errorf("mcp: read embedded rest/v1/dtos.schema.json: %w", err)
	}
	var probe struct {
		ID string `json:"$id"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return restDefsResource{}, fmt.Errorf("mcp: probe $id in rest/v1/dtos.schema.json: %w", err)
	}
	if probe.ID == "" {
		return restDefsResource{}, fmt.Errorf("mcp: rest/v1/dtos.schema.json has no $id")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return restDefsResource{}, fmt.Errorf("mcp: decode rest/v1/dtos.schema.json for argument validation: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(probe.ID, doc); err != nil {
		return restDefsResource{}, fmt.Errorf("mcp: add rest/v1/dtos.schema.json as a validation resource: %w", err)
	}
	return restDefsResource{compiler: c, id: probe.ID}, nil
})

// compiledInputSchemas memoizes one compiled *jsonschema.Schema per $def
// name -- Compile itself is not documented safe for concurrent use, so
// each name is compiled at most once (sync.Map's own LoadOrStore),
// rather than compiling per-request.
var compiledInputSchemas sync.Map // name (string) -> *jsonschema.Schema

// compiledInputSchema returns the compiled validator for name's own
// $def, compiling and caching it on first use.
func compiledInputSchema(name string) (*jsonschema.Schema, error) {
	if v, ok := compiledInputSchemas.Load(name); ok {
		return v.(*jsonschema.Schema), nil
	}
	res, err := restDefsCompiler()
	if err != nil {
		return nil, err
	}
	c, id := res.compiler, res.id
	sch, err := c.Compile(id + "#/$defs/" + name)
	if err != nil {
		return nil, fmt.Errorf("mcp: compile input schema %q: %w", name, err)
	}
	actual, _ := compiledInputSchemas.LoadOrStore(name, sch)
	return actual.(*jsonschema.Schema), nil
}

// errArgumentsNotJSONObject is validateArguments' own fixed message for
// arguments that do not even decode as JSON -- never the raw
// encoding/json error text, which would name internal Go struct/field
// types the client has no business seeing (see this package's own
// doc.go, the HTTP-outcome-to-MCP-outcome mapping table's identical
// "never leak" discipline for 401/5xx).
var errArgumentsNotJSONObject = fmt.Errorf("arguments must be a JSON object")

// validateArguments validates raw -- a tools/call request's own
// "arguments" value, exactly as the client sent it, which may be
// empty/absent when the caller omitted the field entirely -- against
// defName's own $def in rest/v1/dtos.schema.json. An empty/absent value
// is treated as "{}": every one of this package's three input $defs is
// {"type":"object",...}, and GetSessionToolRequest's own "required":
// ["sessionId"] must still fire when the caller sends no arguments at
// all, exactly as it would for a truly empty object.
func validateArguments(defName string, raw json.RawMessage) error {
	schema, err := compiledInputSchema(defName)
	if err != nil {
		return err
	}
	instText := []byte(raw)
	if len(bytes.TrimSpace(instText)) == 0 {
		instText = []byte("{}")
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(instText))
	if err != nil {
		return errArgumentsNotJSONObject
	}
	return schema.Validate(inst)
}

// invalidArgumentsMessage turns verr (validateArguments' own return
// value -- either errArgumentsNotJSONObject or a
// *jsonschema.ValidationError) into the text a tool-execution-error
// result's content carries. A *jsonschema.ValidationError's own Error()
// text is "jsonschema validation failed with '<schema $id>#'\n- at
// '<path>': <reason>" (one "- at ..." line per leaf cause); this strips
// the schema-$id header line, since a client has no use for our own
// internal contract URL, keeping only the "at '<path>': <reason>"
// line(s) -- specific enough to act on, never a raw Go
// encoding/json/reflect error string (see errArgumentsNotJSONObject's
// own doc comment).
func invalidArgumentsMessage(verr error) string {
	text := verr.Error()
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		return "invalid arguments: " + strings.TrimSpace(text[idx+1:])
	}
	return "invalid arguments: " + text
}
