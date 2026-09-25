package mcp

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// This file pins where a tools/call's input schema comes from (round 2
// review of PR #324, findings N1/N3/N4; round 5, T1; round 6, U1/U2/U3).
// The property: the request path never compiles a schema. It validates
// against the map NewHandler compiled eagerly at boot and nothing else.
// What guarantees it, and by what:
//
//   - newHandler (handler.go) refuses to build a handler unless its map
//     holds a non-nil schema for every tool, and serves every request
//     from a private copy taken at construction
//     (TestInjectedSchemaMap_IncompleteMapRefusedAtConstruction,
//     TestInjectedSchemaMap_CopiedAtConstruction). A handler therefore
//     cannot exist without a complete map, and nothing a request does
//     can change that map.
//   - TestInjectedSchemaMap_DecidesEveryToolCall drives newHandler's full
//     HTTP path with a reject-all and an accept-all map. Any layer
//     between the route and the validator that validates against
//     anything but the injected map changes an outcome there, cache warm
//     or not.
//   - TestNewHandler_ValidatesAgainstTheRealContractsDefs pins the thin
//     wrapper: NewHandler builds a handler, and that handler validates
//     against the real contracts $defs.
//   - TestToolHandler_MissingSchemaIsRefusedWithoutCompiling pins
//     toolHandler's missing-schema branch, which no handler newHandler
//     builds can reach: it refuses, and it compiles nothing.
//
// What none of these sees: a compile on the request path whose result
// nothing validates against, since it changes no outcome, and a
// NewHandler that stops delegating to newHandler and builds its own
// request path. TestNoSecondJSONSchemaCompiler (schemas_test.go) and an
// isolated `go test -race -run TestToolCall_ConcurrentFirstCalls_NoRace`
// are the only checks left for those.

// constantInputSchemas compiles ONE boolean JSON Schema -- `true`
// (accepts every instance) or `false` (refuses every instance) -- with a
// compiler built right here, in this test file, never through
// compileInputSchemas, and maps every name toolInputDefs() returns to
// it. Handed to newHandler in place of the map NewHandler compiles, it
// makes the injected map's influence on a tools/call observable: no real
// contracts $def refuses everything or accepts everything, so an outcome
// that follows the constant schema can only have come from the injected
// map.
func constantInputSchemas(t *testing.T, accept bool) map[string]*jsonschema.Schema {
	t.Helper()
	resourceURL := "mem://reject-all.json"
	if accept {
		resourceURL = "mem://accept-all.json"
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(resourceURL, accept); err != nil {
		t.Fatalf("add boolean schema resource %q: %v", resourceURL, err)
	}
	sch, err := c.Compile(resourceURL)
	if err != nil {
		t.Fatalf("compile boolean schema %q: %v", resourceURL, err)
	}
	out := make(map[string]*jsonschema.Schema)
	for _, name := range toolInputDefs() {
		out[name] = sch
	}
	return out
}

// rejectAllRefusalText is the exact text a tools/call answers when the
// schema it is validated against is the boolean `false` schema:
// invalidArgumentsMessage (schemas.go) applied to
// santhosh-tekuri/jsonschema/v6's own "false schema" validation error.
// No real contracts $def produces it -- a real refusal names the
// offending field or keyword instead.
const rejectAllRefusalText = "invalid arguments: - at '': false schema"

// toolCallArgs is, per tool: realValid, an arguments object the tool's
// real contracts $def accepts and its BuildRequest carries through to
// the twin; realInvalid, one the real $def refuses but BuildRequest
// would still carry through to the twin if nothing refused it first; and
// realRefusalNames, the field or key the real $def's refusal of
// realInvalid names.
var toolCallArgs = map[string]struct{ realValid, realInvalid, realRefusalNames string }{
	"narvi_list_models": {
		realValid:        `{}`,
		realInvalid:      `{"bogus":1}`, // additionalProperties:false
		realRefusalNames: "bogus",
	},
	"narvi_list_sessions": {
		realValid:        `{"filter":"all","limit":5}`,
		realInvalid:      `{"filter":"all","bogus":1}`, // additionalProperties:false
		realRefusalNames: "bogus",
	},
	"narvi_get_session": {
		realValid:        `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"}`,
		realInvalid:      `{"sessionId":"not-a-uuid"}`, // format:"uuid"
		realRefusalNames: "sessionId",
	},
}

// twinBodies is the 200 body each tool's counting twin answers with --
// distinct per tool, so a successful result also shows WHICH twin ran.
var twinBodies = map[string]string{
	"narvi_list_models":   `{"providers":[]}`,
	"narvi_list_sessions": `{"sessions":[]}`,
	"narvi_get_session":   `{"id":"x"}`,
}

// countingTwins returns Twins whose three handlers each add one to
// *calls and answer 200 with their own tool's twinBodies entry. calls is
// atomic because the SDK runs a tool handler on its own goroutine.
func countingTwins(calls *atomic.Int32) Twins {
	twin := func(toolName string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(twinBodies[toolName]))
		}
	}
	return Twins{
		ListModels:   twin("narvi_list_models"),
		ListSessions: twin("narvi_list_sessions"),
		GetSession:   twin("narvi_get_session"),
	}
}

// eachToolWithArgs returns every registered tool's name, failing the
// test if one has no toolCallArgs row, or if toolCallArgs names a tool
// that is not registered.
func eachToolWithArgs(t *testing.T) []string {
	t.Helper()
	specs := toolSpecs(Twins{})
	if len(specs) != len(toolCallArgs) {
		t.Fatalf("toolSpecs has %d tools, toolCallArgs has %d rows -- every registered tool needs exactly one", len(specs), len(toolCallArgs))
	}
	names := make([]string, len(specs))
	for i, spec := range specs {
		if _, ok := toolCallArgs[spec.Name]; !ok {
			t.Fatalf("tool %q has no toolCallArgs row -- every registered tool needs one", spec.Name)
		}
		names[i] = spec.Name
	}
	return names
}

// postToolCall sends one tools/call through handler's full HTTP path and
// returns its result's single text block and IsError flag, failing the
// test on anything else: a non-200, a JSON-RPC protocol error, or a
// result without exactly one content block.
func postToolCall(t *testing.T, handler http.Handler, toolName, arguments string) (text string, isError bool) {
	t.Helper()
	status, body := rawPost(t, handler, "/mcp", callToolBody(1, toolName, arguments), callToolHeaders(toolName))
	if status != http.StatusOK {
		t.Fatalf("tools/call %s %s: status = %d, body = %s, want 200", toolName, arguments, status, body)
	}
	var env callToolResultEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("tools/call %s %s: unmarshal: %v (body: %s)", toolName, arguments, err, body)
	}
	if env.Error != nil {
		t.Fatalf("tools/call %s %s: JSON-RPC error %+v, want a tool result (body: %s)", toolName, arguments, *env.Error, body)
	}
	if env.Result == nil || len(env.Result.Content) != 1 {
		t.Fatalf("tools/call %s %s: result = %+v, want exactly one content block (body: %s)", toolName, arguments, env.Result, body)
	}
	return env.Result.Content[0].Text, env.Result.IsError
}

// assertTwinAnswered fails the test unless the call just made invoked
// toolName's own twin exactly once and returned its body as a
// successful result.
func assertTwinAnswered(t *testing.T, toolName, arguments string, twinCalls int32, text string, isError bool) {
	t.Helper()
	if twinCalls != 1 {
		t.Fatalf("twin invoked %d time(s) for %s %s, want exactly 1 -- the call was refused by something other than the schema map under test", twinCalls, toolName, arguments)
	}
	if isError || text != twinBodies[toolName] {
		t.Fatalf("result for %s %s = (IsError %v, %q), want the twin's own body %q as a successful result", toolName, arguments, isError, text, twinBodies[toolName])
	}
}

// assertRefused fails the test unless the call just made never invoked
// the twin and answered a validation refusal.
func assertRefused(t *testing.T, toolName, arguments string, twinCalls int32, text string, isError bool) {
	t.Helper()
	if twinCalls != 0 {
		t.Fatalf("twin invoked %d time(s) for %s %s, want 0 -- the arguments were accepted by something other than the schema map under test", twinCalls, toolName, arguments)
	}
	if !isError || !strings.HasPrefix(text, "invalid arguments: ") {
		t.Fatalf("result for %s %s = (IsError %v, %q), want an IsError validation refusal (prefix %q)", toolName, arguments, isError, text, "invalid arguments: ")
	}
}

// TestInjectedSchemaMap_DecidesEveryToolCall is the behavioural
// guarantee at the outermost seam: a tools/call's arguments are judged
// against the schema map newHandler was built with, and against nothing
// else, all the way down newHandler's real request path (route gates,
// versionGate, the SDK's Streamable HTTP handler, the per-request
// closure, buildServer, registerTools, toolHandler). A layer anywhere on
// that path that validates against something else -- a lazy map, a
// per-request compile, a shared compiler, a map compiled at package
// init, whether or not it goes through compileInputSchemas -- changes
// one of these outcomes for some tool:
//
//   - reject-all map, arguments VALID for the tool's real $def: refused
//     with exactly the `false` schema's refusal, the twin never invoked.
//     Validating against the real $def instead of the map, or as an
//     alternative to it, accepts them and invokes the twin.
//   - accept-all map, arguments INVALID for the tool's real $def: the
//     twin invoked. Validating against the real $def instead of the map,
//     or on top of it, refuses them.
//   - accept-all map, arguments valid for the real $def: the twin
//     invoked, so this test cannot pass on a path that refuses
//     everything.
//
// It replaces nothing at runtime and depends on neither test order,
// -race, nor a race actually firing, so a lazy cache warmed by an
// earlier test in the same binary cannot hide a regression from it.
// TestNewHandler_ValidatesAgainstTheRealContractsDefs is its control:
// against the real map, realValid really reaches the twin and
// realInvalid really is refused.
func TestInjectedSchemaMap_DecidesEveryToolCall(t *testing.T) {
	var twinCalls atomic.Int32
	twins := countingTwins(&twinCalls)
	rejectAll := newTestHandlerWithInputSchemas(t, twins, constantInputSchemas(t, false))
	acceptAll := newTestHandlerWithInputSchemas(t, twins, constantInputSchemas(t, true))

	cases := []struct {
		name     string
		handler  http.Handler
		invalid  bool // send realInvalid instead of realValid
		wantTwin bool
	}{
		{name: "reject-all map, real-valid arguments are refused", handler: rejectAll},
		{name: "accept-all map, real-valid arguments reach the twin", handler: acceptAll, wantTwin: true},
		{name: "accept-all map, real-invalid arguments reach the twin", handler: acceptAll, invalid: true, wantTwin: true},
	}
	for _, toolName := range eachToolWithArgs(t) {
		for _, tc := range cases {
			t.Run(toolName+"/"+tc.name, func(t *testing.T) {
				arguments := toolCallArgs[toolName].realValid
				if tc.invalid {
					arguments = toolCallArgs[toolName].realInvalid
				}
				twinCalls.Store(0)
				text, isError := postToolCall(t, tc.handler, toolName, arguments)
				if tc.wantTwin {
					assertTwinAnswered(t, toolName, arguments, twinCalls.Load(), text, isError)
					return
				}
				assertRefused(t, toolName, arguments, twinCalls.Load(), text, isError)
				if text != rejectAllRefusalText {
					t.Fatalf("refusal text = %q, want exactly %q -- the injected reject-all schema's own refusal", text, rejectAllRefusalText)
				}
			})
		}
	}
}

// TestNewHandler_ValidatesAgainstTheRealContractsDefs pins the thin
// wrapper. NewHandler, with a real Config, builds a handler (its eager
// compile succeeds on this build's contracts, and newHandler accepts the
// result as complete), and for every tool that handler validates
// against the real contracts $def: realValid reaches the twin, and
// realInvalid is refused with the real $def's own refusal, naming the
// offending field or key -- not the reject-all text, not an internal
// error. That rules out NewHandler handing newHandler a constant, empty
// or placeholder map. It is also the control for
// TestInjectedSchemaMap_DecidesEveryToolCall's argument choices.
//
// No behavioural test can tell an eager map from a lazy one: they give
// the same answers. What makes NewHandler's map eager is structural --
// newHandler requires it complete at construction and copies it then.
func TestNewHandler_ValidatesAgainstTheRealContractsDefs(t *testing.T) {
	var twinCalls atomic.Int32
	handler := newTestHandler(t, true, true, countingTwins(&twinCalls))

	for _, toolName := range eachToolWithArgs(t) {
		args := toolCallArgs[toolName]
		t.Run(toolName+"/real-valid arguments reach the twin", func(t *testing.T) {
			twinCalls.Store(0)
			text, isError := postToolCall(t, handler, toolName, args.realValid)
			assertTwinAnswered(t, toolName, args.realValid, twinCalls.Load(), text, isError)
		})
		t.Run(toolName+"/real-invalid arguments are refused by the real $def", func(t *testing.T) {
			twinCalls.Store(0)
			text, isError := postToolCall(t, handler, toolName, args.realInvalid)
			assertRefused(t, toolName, args.realInvalid, twinCalls.Load(), text, isError)
			if text == rejectAllRefusalText || !strings.Contains(text, args.realRefusalNames) {
				t.Fatalf("refusal text = %q, want the real $def's own refusal naming %q", text, args.realRefusalNames)
			}
		})
	}
}

// TestInjectedSchemaMap_IncompleteMapRefusedAtConstruction pins
// newHandler's construction-time check: a map missing any name
// toolInputDefs() returns, or holding a nil schema for one, gets no
// handler at all -- the same boot failure a compile error in NewHandler
// is -- so toolHandler's missing-schema branch can never be reached, and
// no request can lack a schema it might be tempted to compile. The two
// controls show the check refuses only what it should: the real map is
// accepted, and so is the real map plus an entry no tool looks up.
func TestInjectedSchemaMap_IncompleteMapRefusedAtConstruction(t *testing.T) {
	realSchemas, err := compileInputSchemas(toolInputDefs())
	if err != nil {
		t.Fatalf("compileInputSchemas(toolInputDefs()) error = %v", err)
	}
	defs := toolInputDefs()

	type row struct {
		name     string
		schemas  map[string]*jsonschema.Schema
		wantName string // "" = newHandler must accept the map
	}
	rows := []row{
		{name: "nil map", schemas: nil, wantName: defs[0]},
		{name: "empty map", schemas: map[string]*jsonschema.Schema{}, wantName: defs[0]},
	}
	for _, def := range defs {
		missing := maps.Clone(realSchemas)
		delete(missing, def)
		rows = append(rows, row{name: "no entry for " + def, schemas: missing, wantName: def})

		nilEntry := maps.Clone(realSchemas)
		nilEntry[def] = nil
		rows = append(rows, row{name: "nil entry for " + def, schemas: nilEntry, wantName: def})
	}
	extra := maps.Clone(realSchemas)
	extra["NotAToolInputDef"] = realSchemas[defs[0]]
	rows = append(rows,
		row{name: "control: the real map", schemas: realSchemas},
		row{name: "control: the real map plus an entry no tool looks up", schemas: extra},
	)

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := newHandler(Config{PublicBaseURL: testPublicBaseURL}, testTwins(), tc.schemas)
			if tc.wantName == "" {
				if err != nil || handler == nil {
					t.Fatalf("newHandler = (%v, %v), want a handler and no error", handler, err)
				}
				return
			}
			if err == nil || handler != nil {
				t.Fatalf("newHandler = (%v, %v), want no handler and an error naming %q", handler, err, tc.wantName)
			}
			if !strings.Contains(err.Error(), `"`+tc.wantName+`"`) {
				t.Fatalf("newHandler error = %q, want it to name %q", err, tc.wantName)
			}
		})
	}
}

// TestInjectedSchemaMap_CopiedAtConstruction pins that newHandler serves
// from its own copy of the map, taken at construction, not from the map
// it was handed: the caller swapping every entry to reject-all after
// newHandler returns changes nothing, and arguments the real $defs
// refuse still reach every twin under the accept-all map the handler
// was built with. A handler that kept the caller's map would refuse
// them.
func TestInjectedSchemaMap_CopiedAtConstruction(t *testing.T) {
	var twinCalls atomic.Int32
	injected := constantInputSchemas(t, true)
	handler := newTestHandlerWithInputSchemas(t, countingTwins(&twinCalls), injected)
	maps.Copy(injected, constantInputSchemas(t, false))

	for _, toolName := range eachToolWithArgs(t) {
		t.Run(toolName, func(t *testing.T) {
			arguments := toolCallArgs[toolName].realInvalid
			twinCalls.Store(0)
			text, isError := postToolCall(t, handler, toolName, arguments)
			assertTwinAnswered(t, toolName, arguments, twinCalls.Load(), text, isError)
		})
	}
}

// TestToolHandler_MissingSchemaIsRefusedWithoutCompiling pins
// toolHandler's missing-schema branch directly, since no handler
// newHandler builds can reach it
// (TestInjectedSchemaMap_IncompleteMapRefusedAtConstruction). Handed a
// map with no entry for its own tool, toolHandler answers a JSON-RPC
// -32603 internal error (a defect in this package, per §43.8's outcome
// table, never an IsError tool result), logs it, and never invokes the
// twin. The arguments sent are VALID for the tool's real $def, so a
// fallback that compiled the missing schema on the spot would accept
// them and invoke the twin -- which is what this test fails on.
func TestToolHandler_MissingSchemaIsRefusedWithoutCompiling(t *testing.T) {
	realSchemas, err := compileInputSchemas(toolInputDefs())
	if err != nil {
		t.Fatalf("compileInputSchemas(toolInputDefs()) error = %v", err)
	}
	eachToolWithArgs(t) // every tool has a toolCallArgs row

	var twinCalls atomic.Int32
	for _, spec := range toolSpecs(countingTwins(&twinCalls)) {
		withoutOwn := maps.Clone(realSchemas)
		delete(withoutOwn, spec.InputDef)
		incomplete := []struct {
			name    string
			schemas map[string]*jsonschema.Schema
		}{
			{name: "nil map", schemas: nil},
			{name: "empty map", schemas: map[string]*jsonschema.Schema{}},
			{name: "every tool's entry but its own", schemas: withoutOwn},
		}
		for _, m := range incomplete {
			t.Run(spec.Name+"/"+m.name, func(t *testing.T) {
				logs := swapDefaultLogger(t)
				arguments := toolCallArgs[spec.Name].realValid
				twinCalls.Store(0)
				res, err := spec.toolHandler(t.Context(), m.schemas)(t.Context(), &sdkmcp.CallToolRequest{
					Params: &sdkmcp.CallToolParamsRaw{Name: spec.Name, Arguments: json.RawMessage(arguments)},
				})
				if n := twinCalls.Load(); n != 0 {
					t.Fatalf("twin invoked %d time(s), want 0 -- toolHandler found no schema for %q and must refuse, not compile one", n, spec.InputDef)
				}
				if res != nil {
					t.Fatalf("result = %+v, want nil alongside a JSON-RPC error", res)
				}
				var rpcErr *jsonrpc.Error
				if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInternalError {
					t.Fatalf("error = %v, want a *jsonrpc.Error with code %d (internal error)", err, jsonrpc.CodeInternalError)
				}
				if !strings.Contains(logs.String(), "no compiled input schema for tool") {
					t.Fatalf("log = %q, want the missing-schema defect logged", logs.String())
				}
			})
		}
	}
}
