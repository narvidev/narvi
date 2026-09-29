package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/narvidev/narvi/contracts"
)

// This file pins how the tool schemas treat contracts/manifest.json's
// openEnums (technical plan §43.10): every OUTPUT schema publishes each
// open enum it reaches open (openBundledEnums), and every INPUT schema
// keeps every enum closed. Each test reads the manifest and the contract
// from their own files, never through schemas.go's own loaders, so a
// defect in those loaders cannot also blind the test that checks them.

// unknownEnumValue is a value no enum in the contracts lists: what a
// newer server, or a newer row read by an older one, may send.
const unknownEnumValue = "zz-value-added-after-this-build"

const (
	openSessionStatus     = "rest/v1/dtos.schema.json#/$defs/Session/properties/status"
	openSessionSource     = "rest/v1/dtos.schema.json#/$defs/Session/properties/spawnSource"
	openExcludedPRKind    = "rest/v1/dtos.schema.json#/$defs/SessionOutcomeExcludedPullRequest/properties/kind"
	openEnumTestSessionID = "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"
)

const openEnumTestSession = `{
	"id": "` + openEnumTestSessionID + `",
	"title": null,
	"status": "completed",
	"failureReason": null,
	"archived": false,
	"spawnSource": "web",
	"createdBy": null,
	"createdAt": "2026-01-01T00:00:00Z",
	"updatedAt": "2026-01-01T00:00:00Z",
	"repos": [{"name": "repo", "url": "https://example.com/repo", "branch": null}],
	"sandboxStatus": null,
	"buildModelId": null,
	"buildEffort": null
}`

// openEnumBodies holds, per output $def, a real-shaped body that
// validates against the contract, and where in it each open enum the
// $def reaches appears: an instance JSON Pointer, mapped to the manifest
// entry that opens the enum found there. A tool whose output reaches an
// open enum with no row here fails TestOutputSchemas_OpenEnumsAcceptUnknownValues.
var openEnumBodies = map[string]struct {
	body string
	at   map[string]string
}{
	"Session": {
		body: openEnumTestSession,
		at: map[string]string{
			"/status":      openSessionStatus,
			"/spawnSource": openSessionSource,
		},
	},
	"ListSessionsResponse": {
		body: `{"sessions": [` + openEnumTestSession + `, ` + openEnumTestSession + `]}`,
		at: map[string]string{
			"/sessions/0/status":      openSessionStatus,
			"/sessions/0/spawnSource": openSessionSource,
			"/sessions/1/spawnSource": openSessionSource,
		},
	},
	"SessionOutcome": {
		body: `{
			"sessionId": "` + openEnumTestSessionID + `",
			"activity": "finished",
			"lastRun": null,
			"reviewScope": "produced",
			"pullRequests": [],
			"excludedPullRequests": [{
				"kind": "shadow_suppressed",
				"repoFullName": "acme/repo",
				"url": null,
				"createdAt": "2026-01-01T00:00:00Z",
				"reason": "outbound writes to acme/repo were in shadow mode"
			}],
			"reviewedPullRequest": null,
			"suggestedDelaySeconds": 30
		}`,
		at: map[string]string{
			"/excludedPullRequests/0/kind": openExcludedPRKind,
		},
	},
}

// diskOpenEnums reads contracts/manifest.json from disk and returns its
// openEnums entries for rest/v1/dtos.schema.json, in order, once each.
func diskOpenEnums(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "contracts", "manifest.json"))
	if err != nil {
		t.Fatalf("read contracts/manifest.json: %v", err)
	}
	var m struct {
		OpenEnums []string `json:"openEnums"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse contracts/manifest.json: %v", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range m.OpenEnums {
		if strings.HasPrefix(e, restSurface+"#") && !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}

// splitOpenEnum splits a rest/v1 openEnums entry into its $def name and
// the in-$def JSON Pointer ("" when the entry names the $def itself).
func splitOpenEnum(t *testing.T, entry string) (def, pointer string) {
	t.Helper()
	rest, ok := strings.CutPrefix(entry, restSurface+"#/$defs/")
	if !ok {
		t.Fatalf("openEnums entry %q does not point into %s's $defs", entry, restSurface)
	}
	def, pointer, _ = strings.Cut(rest, "/")
	if pointer != "" {
		pointer = "/" + pointer
	}
	return def, pointer
}

// pointerTokens splits a JSON Pointer into its unescaped tokens.
func pointerTokens(pointer string) []string {
	if pointer == "" {
		return nil
	}
	raw := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for i, tok := range raw {
		raw[i] = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
	}
	return raw
}

// nodeAt returns the value pointer leads to inside doc (decoded JSON).
func nodeAt(t *testing.T, doc any, pointer string) any {
	t.Helper()
	node := doc
	for _, tok := range pointerTokens(pointer) {
		switch n := node.(type) {
		case map[string]any:
			next, ok := n[tok]
			if !ok {
				t.Fatalf("nothing at %q (no key %q)", pointer, tok)
			}
			node = next
		case []any:
			var idx int
			if _, err := fmt.Sscan(tok, &idx); err != nil || idx < 0 || idx >= len(n) {
				t.Fatalf("nothing at %q (no index %q)", pointer, tok)
			}
			node = n[idx]
		default:
			t.Fatalf("nothing at %q (%T has no %q)", pointer, node, tok)
		}
	}
	return node
}

// schemaNodeAt is nodeAt for a node that must be a schema object.
func schemaNodeAt(t *testing.T, doc any, pointer string) map[string]any {
	t.Helper()
	obj, ok := nodeAt(t, doc, pointer).(map[string]any)
	if !ok {
		t.Fatalf("%q is not a schema object", pointer)
	}
	return obj
}

// setAt replaces the value pointer leads to inside doc (decoded JSON).
func setAt(t *testing.T, doc any, pointer string, v any) {
	t.Helper()
	cut := strings.LastIndexByte(pointer, '/')
	if cut < 0 {
		t.Fatalf("setAt %q: not a pointer to a member", pointer)
	}
	// An escaped token holds no "/", so the parent is everything before
	// the last one ("" for a top-level member: the document itself).
	parent := nodeAt(t, doc, pointer[:cut])
	last := pointerTokens(pointer[cut:])[0]
	switch p := parent.(type) {
	case map[string]any:
		if _, ok := p[last]; !ok {
			t.Fatalf("setAt %q: no key %q to replace", pointer, last)
		}
		p[last] = v
	case []any:
		var idx int
		if _, err := fmt.Sscan(last, &idx); err != nil || idx < 0 || idx >= len(p) {
			t.Fatalf("setAt %q: no index %q to replace", pointer, last)
		}
		p[idx] = v
	default:
		t.Fatalf("setAt %q: parent is %T", pointer, parent)
	}
}

func decodeJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("decode %s: %v", s, err)
	}
	return v
}

// instanceOf turns decoded JSON into the value jsonschema validates.
func instanceOf(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal instance: %v", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode instance: %v", err)
	}
	return inst
}

// compileAdvertised compiles a schema exactly as a tool advertises it.
func compileAdvertised(t *testing.T, url string, schema any) *jsonschema.Schema {
	t.Helper()
	data, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(url, doc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	sch, err := c.Compile(url)
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	return sch
}

// compileContractDef compiles name's $def straight from the embedded
// contract, with every enum as the contract writes it.
func compileContractDef(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	data, err := contracts.FS.ReadFile(restSurface)
	if err != nil {
		t.Fatalf("read %s: %v", restSurface, err)
	}
	var probe struct {
		ID string `json:"$id"`
	}
	if err := json.Unmarshal(data, &probe); err != nil || probe.ID == "" {
		t.Fatalf("no $id in %s (err %v)", restSurface, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode %s: %v", restSurface, err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(probe.ID, doc); err != nil {
		t.Fatalf("add %s: %v", restSurface, err)
	}
	sch, err := c.Compile(probe.ID + "#/$defs/" + name)
	if err != nil {
		t.Fatalf("compile %s $def %q: %v", restSurface, name, err)
	}
	return sch
}

// reachableOpenEnums returns the rest/v1 openEnums entries whose $def is
// one of bundledDefs' keys, sorted.
func reachableOpenEnums(t *testing.T, entries []string, bundledDefs map[string]any) []string {
	t.Helper()
	var out []string
	for _, e := range entries {
		def, _ := splitOpenEnum(t, e)
		if _, ok := bundledDefs[def]; ok {
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return out
}

// advertisedTools pairs each tool tools/list advertises with its spec.
func advertisedTools(t *testing.T) ([]toolSpec, []map[string]any, []map[string]any) {
	t.Helper()
	specs := toolSpecs(Twins{})
	tools := realToolsListTools(t)
	if len(tools) != len(specs) {
		t.Fatalf("%d tools for %d specs", len(tools), len(specs))
	}
	ins := make([]map[string]any, len(tools))
	outs := make([]map[string]any, len(tools))
	for i, tool := range tools {
		if tool.Name != specs[i].Name {
			t.Fatalf("tools[%d] is %q, spec is %q", i, tool.Name, specs[i].Name)
		}
		in, ok := tool.InputSchema.(map[string]any)
		if !ok {
			t.Fatalf("%s: InputSchema is %T", tool.Name, tool.InputSchema)
		}
		out, ok := tool.OutputSchema.(map[string]any)
		if !ok {
			t.Fatalf("%s: OutputSchema is %T", tool.Name, tool.OutputSchema)
		}
		ins[i], outs[i] = in, out
	}
	return specs, ins, outs
}

func bundledDefsOf(t *testing.T, name string, out map[string]any) map[string]any {
	t.Helper()
	defs, ok := out["$defs"].(map[string]any)
	if !ok {
		t.Fatalf("%s: outputSchema $defs is %T", name, out["$defs"])
	}
	return defs
}

// TestOutputSchemas_OpenEnumsAcceptUnknownValues: for every tool whose
// output reaches an open enum, the outputSchema it advertises, compiled
// with this repo's own jsonschema library, accepts a result carrying an
// unknown value in EVERY open enum it reaches at once, and in each one
// alone. The controls make that mean something: the contract's own $def
// refuses each of those values (so each location really is a closed enum
// there), the advertised schema still refuses a value of the wrong type
// (so the node kept its "type"), and the node carries the contract's
// values as "examples", with its "type" and "description" unchanged.
func TestOutputSchemas_OpenEnumsAcceptUnknownValues(t *testing.T) {
	entries := diskOpenEnums(t)
	specs, _, outs := advertisedTools(t)
	affected := 0
	for i, spec := range specs {
		t.Run(spec.Name, func(t *testing.T) {
			bundled := bundledDefsOf(t, spec.Name, outs[i])
			reachable := reachableOpenEnums(t, entries, bundled)
			if len(reachable) == 0 {
				return
			}
			affected++
			row, ok := openEnumBodies[spec.OutputDef]
			if !ok {
				t.Fatalf("%s's output (%s) reaches open enums %v: add a body for it to openEnumBodies", spec.Name, spec.OutputDef, reachable)
			}
			covered := map[string]bool{}
			for _, e := range row.at {
				covered[e] = true
			}
			var coveredList []string
			for e := range covered {
				coveredList = append(coveredList, e)
			}
			sort.Strings(coveredList)
			if !reflect.DeepEqual(coveredList, reachable) {
				t.Fatalf("openEnumBodies[%q] puts unknown values in %v, but %s reaches %v: cover every one", spec.OutputDef, coveredList, spec.Name, reachable)
			}

			advertised := compileAdvertised(t, "mem://out/"+spec.Name+".json", outs[i])
			contract := compileContractDef(t, spec.OutputDef)

			if err := advertised.Validate(instanceOf(t, decodeJSON(t, row.body))); err != nil {
				t.Fatalf("the sample body does not validate against %s's outputSchema: %v", spec.Name, err)
			}
			if err := contract.Validate(instanceOf(t, decodeJSON(t, row.body))); err != nil {
				t.Fatalf("the sample body does not validate against the contract's %s: %v", spec.OutputDef, err)
			}

			locations := make([]string, 0, len(row.at))
			for loc := range row.at {
				locations = append(locations, loc)
			}
			sort.Strings(locations)
			for _, loc := range locations {
				one := decodeJSON(t, row.body)
				setAt(t, one, loc, unknownEnumValue)
				if err := contract.Validate(instanceOf(t, one)); err == nil {
					t.Errorf("the contract's own %s accepts %q at %s: that location is not a closed enum there, so this row proves nothing", spec.OutputDef, unknownEnumValue, loc)
				}
				if err := advertised.Validate(instanceOf(t, one)); err != nil {
					t.Errorf("%s's outputSchema refuses %q at %s (open enum %s): %v", spec.Name, unknownEnumValue, loc, row.at[loc], err)
				}
				wrongType := decodeJSON(t, row.body)
				setAt(t, wrongType, loc, 42)
				if err := advertised.Validate(instanceOf(t, wrongType)); err == nil {
					t.Errorf("%s's outputSchema accepts a number at %s: opening the enum dropped its type", spec.Name, loc)
				}
			}

			all := decodeJSON(t, row.body)
			for _, loc := range locations {
				setAt(t, all, loc, unknownEnumValue)
			}
			if err := advertised.Validate(instanceOf(t, all)); err != nil {
				t.Errorf("%s's outputSchema refuses a result with an unknown value in every open enum it reaches: %v", spec.Name, err)
			}

			for _, e := range reachable {
				def, pointer := splitOpenEnum(t, e)
				got := schemaNodeAt(t, bundled[def], pointer)
				want := schemaNodeAt(t, rawRestDef(t, def), pointer)
				if _, has := got["enum"]; has {
					t.Errorf("%s: open enum %s still carries enum %v", spec.Name, e, got["enum"])
				}
				if !reflect.DeepEqual(got["examples"], want["enum"]) {
					t.Errorf("%s: open enum %s has examples %v, want the contract's values %v", spec.Name, e, got["examples"], want["enum"])
				}
				for _, kw := range []string{"type", "description"} {
					if !reflect.DeepEqual(got[kw], want[kw]) {
						t.Errorf("%s: open enum %s has %s %v, want the contract's %v", spec.Name, e, kw, got[kw], want[kw])
					}
				}
			}
		})
	}
	if affected == 0 {
		t.Fatal("no tool's output reaches an open enum: this test checks nothing")
	}
}

// TestOutputSchemas_DifferFromContractsOnlyAtOpenEnums: every $def a
// tool's outputSchema bundles equals the contract's own $def once the
// open enums it holds are closed again (examples back to enum). So
// opening changes those nodes and nothing else: no closed enum is opened
// (SessionOutcome.activity, SessionActivity's own enums), and no other
// keyword moves.
func TestOutputSchemas_DifferFromContractsOnlyAtOpenEnums(t *testing.T) {
	entries := diskOpenEnums(t)
	specs, _, outs := advertisedTools(t)
	for i, spec := range specs {
		t.Run(spec.Name, func(t *testing.T) {
			bundled := bundledDefsOf(t, spec.Name, outs[i])
			reclosed := decodeJSON(t, string(canonicalJSON(t, bundled))).(map[string]any)
			for _, e := range reachableOpenEnums(t, entries, bundled) {
				def, pointer := splitOpenEnum(t, e)
				node := schemaNodeAt(t, reclosed[def], pointer)
				examples, ok := node["examples"]
				if !ok {
					t.Fatalf("%s: open enum %s has no examples to close back into enum", spec.Name, e)
				}
				node["enum"] = examples
				delete(node, "examples")
			}
			for name, def := range reclosed {
				if want := rawRestDef(t, name); !reflect.DeepEqual(def, want) {
					t.Errorf("%s: bundled $def %q differs from the contract's beyond its open enums:\ngot  %s\nwant %s", spec.Name, name, canonicalJSON(t, def), canonicalJSON(t, want))
				}
			}
		})
	}
}

// openEnumTestArguments is, per tool input $def, an arguments object that
// validates before any enum-valued field is set.
var openEnumTestArguments = map[string]string{
	"ListModelsToolRequest":           `{}`,
	"ListSessionsToolRequest":         `{}`,
	"GetSessionToolRequest":           `{"sessionId": "` + openEnumTestSessionID + `"}`,
	"GetSessionStatusToolRequest":     `{"sessionId": "` + openEnumTestSessionID + `"}`,
	"WaitForSessionToolRequest":       `{"sessionId": "` + openEnumTestSessionID + `"}`,
	"GetSessionResultToolRequest":     `{"sessionId": "` + openEnumTestSessionID + `"}`,
	"GetSessionTranscriptToolRequest": `{"sessionId": "` + openEnumTestSessionID + `"}`,
}

// enumPointers returns the JSON Pointer of every node in schema that
// carries "enum", sorted.
func enumPointers(schema any) []string {
	var out []string
	var walk func(v any, at string)
	walk = func(v any, at string) {
		switch n := v.(type) {
		case map[string]any:
			if _, ok := n["enum"]; ok {
				out = append(out, at)
			}
			for k, child := range n {
				walk(child, at+"/"+strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1"))
			}
		case []any:
			for i, child := range n {
				walk(child, fmt.Sprintf("%s/%d", at, i))
			}
		}
	}
	walk(schema, "")
	sort.Strings(out)
	return out
}

// TestInputSchemas_StillRefuseUnknownEnumValues: opening enums is for
// output only. Every tool's advertised inputSchema carries exactly the
// enums its contract $def does, and for each one an unknown value is
// refused both by that advertised schema and by the validator every
// tools/call actually runs (compileInputSchemas + validateArguments),
// while a value the enum lists is accepted by both.
func TestInputSchemas_StillRefuseUnknownEnumValues(t *testing.T) {
	compiled, err := compileInputSchemas(toolInputDefs())
	if err != nil {
		t.Fatalf("compileInputSchemas: %v", err)
	}
	specs, ins, _ := advertisedTools(t)
	checked := 0
	for i, spec := range specs {
		t.Run(spec.Name, func(t *testing.T) {
			got := enumPointers(ins[i])
			want := enumPointers(rawRestDef(t, spec.InputDef))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s's inputSchema has enums at %v, but its contract $def %s has them at %v", spec.Name, got, spec.InputDef, want)
			}
			if len(got) == 0 {
				return
			}
			base, ok := openEnumTestArguments[spec.InputDef]
			if !ok {
				t.Fatalf("add a valid arguments object for %s to openEnumTestArguments", spec.InputDef)
			}
			advertised := compileAdvertised(t, "mem://in/"+spec.Name+".json", ins[i])
			boot := compiled[spec.InputDef]
			if boot == nil {
				t.Fatalf("compileInputSchemas has no schema for %s", spec.InputDef)
			}
			for _, pointer := range got {
				prop, ok := strings.CutPrefix(pointer, "/properties/")
				if !ok || strings.Contains(prop, "/") {
					t.Fatalf("%s has an enum at %s; this test only knows how to fill a top-level property: extend it", spec.InputDef, pointer)
				}
				enum, _ := schemaNodeAt(t, ins[i], pointer)["enum"].([]any)
				if len(enum) == 0 {
					t.Fatalf("%s: enum at %s is empty", spec.Name, pointer)
				}
				for _, tc := range []struct {
					value  any
					accept bool
				}{{enum[0], true}, {unknownEnumValue, false}} {
					args := decodeJSON(t, base).(map[string]any)
					args[prop] = tc.value
					raw, err := json.Marshal(args)
					if err != nil {
						t.Fatalf("marshal arguments: %v", err)
					}
					advErr := advertised.Validate(instanceOf(t, args))
					bootErr := validateArguments(boot, raw)
					if tc.accept && (advErr != nil || bootErr != nil) {
						t.Errorf("%s: %s=%v is refused (advertised: %v; tools/call validator: %v), want accepted", spec.Name, prop, tc.value, advErr, bootErr)
					}
					if !tc.accept && advErr == nil {
						t.Errorf("%s: the advertised inputSchema accepts %s=%q, want it refused", spec.Name, prop, tc.value)
					}
					if !tc.accept && bootErr == nil {
						t.Errorf("%s: the tools/call validator accepts %s=%q, want it refused", spec.Name, prop, tc.value)
					}
				}
				checked++
			}
		})
	}
	if checked == 0 {
		t.Fatal("no tool input schema has an enum: this test checks nothing")
	}
}

// TestOpenEnumPaths_EveryManifestEntryResolves: the list the bundler
// opens is exactly the manifest's rest/v1 openEnums, entry for entry, and
// each one leads to an enum node in the contract. A typo in the manifest,
// or a $def or property renamed under it, fails here rather than leaving
// that enum silently closed; so does a loader that opens nothing. The
// embedded manifest is also checked against the file itself.
func TestOpenEnumPaths_EveryManifestEntryResolves(t *testing.T) {
	onDisk, err := os.ReadFile(filepath.Join(repoRoot(t), "contracts", "manifest.json"))
	if err != nil {
		t.Fatalf("read contracts/manifest.json: %v", err)
	}
	if contracts.ManifestJSON != string(onDisk) {
		t.Fatal("contracts.ManifestJSON is not contracts/manifest.json")
	}

	paths, err := loadedOpenEnumPaths()
	if err != nil {
		t.Fatalf("loadedOpenEnumPaths: %v", err)
	}
	want := diskOpenEnums(t)
	if len(want) == 0 {
		t.Fatal("contracts/manifest.json lists no rest/v1 open enum: this test checks nothing")
	}
	got := make([]string, len(paths))
	for i, p := range paths {
		got[i] = p.entry
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the bundler opens %v,\nwant every rest/v1 openEnums entry %v", got, want)
	}
	for _, e := range want {
		def, pointer := splitOpenEnum(t, e)
		node := schemaNodeAt(t, rawRestDef(t, def), pointer)
		if _, ok := node["enum"]; !ok {
			t.Errorf("openEnums entry %s leads to a node with no enum", e)
		}
	}
}

// TestParseOpenEnumPaths_RefusesWhatItCannotOpen drives the manifest
// parser over made-up $defs: every entry either resolves to the node it
// names, or is an error. None is ever skipped for failing to resolve.
func TestParseOpenEnumPaths_RefusesWhatItCannotOpen(t *testing.T) {
	defs := map[string]json.RawMessage{
		"Thing": json.RawMessage(`{
			"type": "object",
			"properties": {
				"status":  {"type": "string", "enum": ["a", "b"]},
				"plain":   {"type": "string"},
				"untyped": {"enum": ["a"]},
				"empty":   {"type": "string", "enum": []},
				"shown":   {"type": "string", "enum": ["a"], "examples": ["a"]},
				"a/b~c":   {"type": "string", "enum": ["x"]},
				"choice":  {"oneOf": [{"type": "string", "enum": ["p"]}, {"type": "null"}]},
				"list":    {"type": "array", "items": {"type": "string", "enum": ["i"]}}
			}
		}`),
		"Level": json.RawMessage(`{"type": "string", "enum": ["low", "high"]}`),
	}
	const rest = "rest/v1/dtos.schema.json#/$defs/"
	tests := []struct {
		name    string
		entries []string
		want    []string // def + "|" + tokens joined by "|"; nil with wantErr
		wantErr string
	}{
		{"a property", []string{rest + "Thing/properties/status"}, []string{"Thing|properties|status"}, ""},
		{"an escaped key", []string{rest + "Thing/properties/a~1b~0c"}, []string{"Thing|properties|a/b~c"}, ""},
		{"array items", []string{rest + "Thing/properties/list/items"}, []string{"Thing|properties|list|items"}, ""},
		{"a oneOf branch", []string{rest + "Thing/properties/choice/oneOf/0"}, []string{"Thing|properties|choice|oneOf|0"}, ""},
		{"a whole $def", []string{rest + "Level"}, []string{"Level"}, ""},
		{"another surface is skipped", []string{"client-ws/v1/protocol.schema.json#/$defs/Nope/properties/x"}, nil, ""},
		{"a duplicate is opened once", []string{rest + "Level", rest + "Level"}, []string{"Level"}, ""},
		{"a misspelt property", []string{rest + "Thing/properties/statsu"}, nil, "does not resolve"},
		{"a misspelt $def", []string{rest + "Thnig/properties/status"}, nil, "does not define"},
		{"no $def", []string{rest}, nil, "names no $def"},
		{"not an enum", []string{rest + "Thing/properties/plain"}, nil, "no enum to open"},
		{"an empty enum", []string{rest + "Thing/properties/empty"}, nil, "no enum to open"},
		{"an enum with no type", []string{rest + "Thing/properties/untyped"}, nil, "no type"},
		{"examples already there", []string{rest + "Thing/properties/shown"}, nil, "already carries examples"},
		{"a keyword, not a node", []string{rest + "Thing/type"}, nil, "not a schema object"},
		{"an index out of range", []string{rest + "Thing/properties/choice/oneOf/2"}, nil, "does not resolve"},
		{"an index with a leading zero", []string{rest + "Thing/properties/choice/oneOf/00"}, nil, "does not resolve"},
		{"outside $defs", []string{"rest/v1/dtos.schema.json#/properties/status"}, nil, "does not point into $defs"},
		{"no surface", []string{"#/$defs/Thing/properties/status"}, nil, "not qualified"},
		{"no fragment", []string{"rest/v1/dtos.schema.json"}, nil, "not qualified"},
		{"a bad entry after a good one", []string{rest + "Level", rest + "Thing/properties/statsu"}, nil, "does not resolve"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest, err := json.Marshal(map[string]any{"openEnums": tt.entries})
			if err != nil {
				t.Fatalf("marshal manifest: %v", err)
			}
			paths, err := parseOpenEnumPaths(string(manifest), defs)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseOpenEnumPaths(%v) = %v, %v; want an error containing %q", tt.entries, paths, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOpenEnumPaths(%v): %v", tt.entries, err)
			}
			var got []string
			for _, p := range paths {
				got = append(got, strings.Join(append([]string{p.def}, p.tokens...), "|"))
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseOpenEnumPaths(%v) = %v, want %v", tt.entries, got, tt.want)
			}
		})
	}

	if _, err := parseOpenEnumPaths("{", defs); err == nil {
		t.Error("parseOpenEnumPaths accepts a manifest that is not JSON")
	}
}
