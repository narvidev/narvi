package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
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

// compileInputSchemas builds ONE santhosh-tekuri/jsonschema/v6 compiler
// with rest/v1/dtos.schema.json added as a resource (format assertions
// ON, so "format":"uuid" on GetSessionToolRequest.sessionId is actually
// enforced -- mirroring contracts/contractstest/helpers_test.go's own
// newCompiler), then compiles EVERY name in defNames from it EAGERLY,
// right here, returning an error the instant any one of them fails to
// compile -- never lazily, never on a shared, unlocked compiler reached
// from more than one request goroutine at a time (round 2 review of PR
// #324, findings N1/N3/N4: a prior revision of this file compiled each
// $def lazily, on the first tools/call that named it, guarded only by a
// sync.Map that deduplicated the CACHED RESULT, never the Compile call
// itself; santhosh-tekuri/jsonschema/v6 has no locking anywhere in its
// own package -- Compile mutates the Compiler's own plain maps
// (roots.addRoot, Compiler.schemas) -- so two tools/call requests naming
// different $defs, or the SAME $def, racing on the very first call after
// a process starts, hit a Go runtime "fatal error: concurrent map
// writes": a fatal THROW, not a panic, which toolHandler's own recover
// cannot catch and which kills the whole process, dropping every other
// in-flight request on that replica). Calling this once, at NewHandler
// time (boot), and returning its error there instead means a schema
// defect fails the control plane's OWN BOOT, not a live request, and the
// map this returns is never written to again -- so every later, purely
// concurrent *jsonschema.Schema.Validate call against it (validateArguments
// below) needs no lock at all: Schema.Validate builds a fresh, per-call
// validator over an already-compiled, now-immutable *Schema tree and the
// instance value alone (santhosh-tekuri/jsonschema/v6@v6.0.2's own
// validator.go, (*Schema).Validate/.validate), never mutating the
// *Compiler or the *Schema itself -- verified by reading that source
// under GOMODCACHE: only Compile (roots.go, compiler.go, objcompiler.go)
// touches the Compiler's or a Schema's own maps; Validate never does.
func compileInputSchemas(defNames []string) (map[string]*jsonschema.Schema, error) {
	data, err := contracts.FS.ReadFile("rest/v1/dtos.schema.json")
	if err != nil {
		return nil, fmt.Errorf("mcp: read embedded rest/v1/dtos.schema.json: %w", err)
	}
	var probe struct {
		ID string `json:"$id"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("mcp: probe $id in rest/v1/dtos.schema.json: %w", err)
	}
	if probe.ID == "" {
		return nil, fmt.Errorf("mcp: rest/v1/dtos.schema.json has no $id")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("mcp: decode rest/v1/dtos.schema.json for argument validation: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(probe.ID, doc); err != nil {
		return nil, fmt.Errorf("mcp: add rest/v1/dtos.schema.json as a validation resource: %w", err)
	}

	out := make(map[string]*jsonschema.Schema, len(defNames))
	for _, name := range defNames {
		if _, ok := out[name]; ok {
			continue // toolInputDefs() may repeat a name; compile each one once
		}
		sch, err := c.Compile(probe.ID + "#/$defs/" + name)
		if err != nil {
			return nil, fmt.Errorf("mcp: compile input schema %q: %w", name, err)
		}
		out[name] = sch
	}
	return out, nil
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
// schema, an already-compiled *jsonschema.Schema for the calling tool's
// own input $def (compileInputSchemas above, compiled once at NewHandler
// time -- never here, and never against a schema this function compiles
// itself: see compileInputSchemas' own doc comment for why compiling on
// the request path at all is the defect this closes). An empty/absent
// value is treated as "{}": every one of this package's three input
// $defs is {"type":"object",...}, and GetSessionToolRequest's own
// "required": ["sessionId"] must still fire when the caller sends no
// arguments at all, exactly as it would for a truly empty object.
// Concurrent calls against the SAME schema value need no lock --
// (*jsonschema.Schema).Validate never mutates the schema it validates
// against (compileInputSchemas' own doc comment).
func validateArguments(schema *jsonschema.Schema, raw json.RawMessage) error {
	instText := []byte(raw)
	if len(bytes.TrimSpace(instText)) == 0 {
		instText = []byte("{}")
	}
	if err := rejectOversizedNumbers(instText); err != nil {
		return err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(instText))
	if err != nil {
		return errArgumentsNotJSONObject
	}
	return schema.Validate(inst)
}

// maxJSONNumberTokenLen and maxJSONNumberExponent bound every JSON number
// token rejectOversizedNumbers below will ever let through to
// jsonschema.UnmarshalJSON/Schema.Validate (round 3 review of PR #324,
// finding R5): santhosh-tekuri/jsonschema/v6's own "integer" check
// (util.go isInteger) and its "minimum"/"maximum" numeric check
// (validator.go numValidate) both parse a JSON number through
// math/big.Rat.SetString, and this package's own intFromJSONNumber (tools.go)
// mirrors that same parse a third time. That cost is set by the number's
// own MAGNITUDE, not by how many bytes it took to write it: an exponent
// form like "1e1000000" is 9 bytes on the wire but a natural with roughly
// 2.3 million BITS, computed from scratch, three times, for a single
// request -- roughly 50ms of CPU measured against the real stack, for a
// tool whose only numeric field (ListSessionsToolRequest.limit) has no
// legitimate use for a value outside 1..200. MaxRequestBodyBytes
// (versions.go) bounds a LONG DIGIT STRING (finding N7's own original
// vector) because that shape's cost genuinely scales with its own text
// length; it does nothing for an exponent, whose text stays tiny while
// its value explodes. Both bounds here are deliberately far more
// generous than anything this package's three schemas could ever
// legitimately need, so nothing legitimate is lost.
const (
	maxJSONNumberTokenLen = 32
	maxJSONNumberExponent = 20
)

// rejectOversizedNumbers scans every JSON number token in raw -- at any
// depth, not just the top level (round 3 review's own "no bounded window"
// principle) -- using encoding/json's OWN tokenizer (UseNumber, so a
// number is returned as its raw TEXT, never evaluated): recognizing a
// number's syntax (an optional sign, digits, an optional fraction, an
// optional exponent) is linear in the token's own TEXT length and does no
// arbitrary-precision arithmetic at all, unlike santhosh-tekuri/
// jsonschema/v6's own big.Rat-based checks this function runs BEFORE
// (validateArguments above never reaches jsonschema.UnmarshalJSON/
// Schema.Validate until this returns nil). A malformed body is left for
// the real decode immediately after this call to answer for, with its own
// existing, client-safe message (errArgumentsNotJSONObject) -- this scan
// only ever adds a NEW refusal, never removes one.
func rejectOversizedNumbers(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if err != nil {
			// EOF (scan complete) or malformed JSON either way: leave it
			// for validateArguments' own real decode, right after this
			// call, to answer for.
			return nil
		}
		num, ok := tok.(json.Number)
		if !ok {
			continue
		}
		if err := checkNumberTokenSize(num.String()); err != nil {
			return err
		}
	}
}

// checkNumberTokenSize refuses text (one JSON number token's own literal
// spelling) if it is longer than maxJSONNumberTokenLen, or if it carries
// an exponent ('e'/'E') whose own parsed magnitude exceeds
// maxJSONNumberExponent in either direction -- the second check is the
// load-bearing one: "1e1000000" is only 9 characters, comfortably under
// the length bound alone, yet is exactly the shape finding R5 measured
// costing ~50ms of CPU three times over.
func checkNumberTokenSize(text string) error {
	if len(text) > maxJSONNumberTokenLen {
		return fmt.Errorf("a number in the request arguments is too long (%d characters, max %d)", len(text), maxJSONNumberTokenLen)
	}
	idx := strings.IndexAny(text, "eE")
	if idx < 0 {
		return nil
	}
	exp, err := strconv.Atoi(text[idx+1:])
	if err != nil || exp > maxJSONNumberExponent || exp < -maxJSONNumberExponent {
		return fmt.Errorf("a number in the request arguments has an exponent outside +/-%d", maxJSONNumberExponent)
	}
	return nil
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
