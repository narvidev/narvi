package mcp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// constantInputSchemas compiles ONE boolean JSON Schema -- `true`
// (accepts every instance) or `false` (refuses every instance) -- with a
// compiler built right here, in this test file, never through
// compileInputSchemas, and maps every name toolInputDefs() returns to
// it. Handed to toolSpec.toolHandler in place of NewHandler's own
// eagerly-compiled map, it makes the injected map's influence on a
// tools/call observable: no real contracts $def refuses everything or
// accepts everything, so an outcome that follows the constant schema
// can only have come from the injected map.
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

// rejectAllRefusalText is the exact text toolHandler answers when the
// injected schema is the boolean `false` schema: invalidArgumentsMessage
// (schemas.go) applied to santhosh-tekuri/jsonschema/v6's own "false
// schema" validation error. No real contracts $def produces it -- a real
// refusal names the offending field or keyword instead.
const rejectAllRefusalText = "invalid arguments: - at '': false schema"

// TestToolHandler_ValidatesOnlyAgainstTheInjectedSchemaMap is the
// BEHAVIOURAL guarantee behind round 2 review of PR #324 (findings
// N1/N3/N4) and round 5 finding T1: toolHandler decides whether a
// tools/call's arguments are valid from the inputSchemas map it is handed
// (NewHandler's own map, compiled eagerly at boot by compileInputSchemas)
// and from nothing else. A lazy path toolHandler validates against -- a
// package-level shared *jsonschema.Compiler, a per-request compile, a
// sync.Map cache, whatever shape it takes and however its Compile call
// is spelled -- ignores the injected map, so it fails this test:
//
//   - reject-all injected, arguments VALID for the tool's real $def: the
//     twin must never be invoked, and the answer must be exactly the
//     `false` schema's refusal. A lazy path validating against the real
//     $def accepts these arguments and invokes the twin.
//   - accept-all injected, arguments INVALID for the tool's real $def:
//     the twin must be invoked. A lazy path validating against the real
//     $def refuses them.
//   - accept-all injected, arguments valid for the real $def: the twin
//     must be invoked, so the test cannot pass vacuously on a toolHandler
//     that refuses everything.
//
// The two "real map" rows are the controls that keep the argument choices
// honest: against compileInputSchemas' own output, the realValid
// arguments really do reach the twin and the realInvalid ones really are
// refused -- so the rows above test what they claim to.
//
// This test replaces nothing at runtime and does not depend on test
// order, on -race, or on a race actually firing; unlike
// TestToolCall_ConcurrentFirstCalls_NoRace (toolcall_test.go), a lazy
// cache warmed by an earlier test in the same binary cannot hide a
// regression from it. TestNoSecondJSONSchemaCompiler (schemas_test.go) is
// only a cheap early warning for the most common lazy shapes.
func TestToolHandler_ValidatesOnlyAgainstTheInjectedSchemaMap(t *testing.T) {
	// Per tool: realValid passes the tool's real contracts $def AND its
	// BuildRequest; realInvalid is refused by the real $def but still
	// carries through BuildRequest to the twin when nothing refuses it
	// first.
	toolArgs := map[string]struct{ realValid, realInvalid string }{
		"narvi_list_models": {
			realValid:   `{}`,
			realInvalid: `{"bogus":1}`, // additionalProperties:false
		},
		"narvi_list_sessions": {
			realValid:   `{"filter":"all","limit":5}`,
			realInvalid: `{"filter":"all","bogus":1}`, // additionalProperties:false
		},
		"narvi_get_session": {
			realValid:   `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"}`,
			realInvalid: `{"sessionId":"not-a-uuid"}`, // format:"uuid"
		},
	}

	realSchemas, err := compileInputSchemas(toolInputDefs())
	if err != nil {
		t.Fatalf("compileInputSchemas(toolInputDefs()) error = %v", err)
	}
	rejectAll := constantInputSchemas(t, false)
	acceptAll := constantInputSchemas(t, true)

	const (
		wantRefused = iota
		wantTwinInvoked
	)
	cases := []struct {
		name     string
		schemas  map[string]*jsonschema.Schema
		invalid  bool // send realInvalid instead of realValid
		want     int
		wantText string // exact refusal text, when want == wantRefused and it is fixed
	}{
		{name: "control: real map, real-valid arguments reach the twin", schemas: realSchemas, want: wantTwinInvoked},
		{name: "control: real map, real-invalid arguments are refused", schemas: realSchemas, invalid: true, want: wantRefused},
		{name: "reject-all map, real-valid arguments are refused", schemas: rejectAll, want: wantRefused, wantText: rejectAllRefusalText},
		{name: "accept-all map, real-valid arguments reach the twin", schemas: acceptAll, want: wantTwinInvoked},
		{name: "accept-all map, real-invalid arguments reach the twin", schemas: acceptAll, invalid: true, want: wantTwinInvoked},
	}

	var twinCalls int
	countingTwin := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			twinCalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
		}
	}
	specs := toolSpecs(Twins{
		ListModels:   countingTwin(`{"providers":[]}`),
		ListSessions: countingTwin(`{"sessions":[]}`),
		GetSession:   countingTwin(`{"id":"x"}`),
	})
	if len(specs) != len(toolArgs) {
		t.Fatalf("toolSpecs has %d tools, this test's own argument table has %d -- every registered tool needs a row", len(specs), len(toolArgs))
	}

	for _, spec := range specs {
		args, ok := toolArgs[spec.Name]
		if !ok {
			t.Fatalf("tool %q has no row in this test's own argument table -- every registered tool needs one", spec.Name)
		}
		for _, tc := range cases {
			t.Run(spec.Name+"/"+tc.name, func(t *testing.T) {
				arguments := args.realValid
				if tc.invalid {
					arguments = args.realInvalid
				}
				twinCalls = 0
				handler := spec.toolHandler(t.Context(), tc.schemas)
				res, err := handler(t.Context(), &sdkmcp.CallToolRequest{
					Params: &sdkmcp.CallToolParamsRaw{Name: spec.Name, Arguments: json.RawMessage(arguments)},
				})
				if err != nil {
					t.Fatalf("toolHandler returned a protocol error %v, want a CallToolResult", err)
				}
				if res == nil {
					t.Fatal("toolHandler returned a nil CallToolResult")
				}

				switch tc.want {
				case wantRefused:
					if twinCalls != 0 {
						t.Fatalf("twin invoked %d time(s) for arguments %s, want 0 -- toolHandler did not refuse them with the injected schema map (a lazy path that ignores the injected map validates against the real $def instead)", twinCalls, arguments)
					}
					if !res.IsError {
						t.Fatalf("IsError = false for arguments %s, want true", arguments)
					}
					text := resultText(t, res)
					if !strings.HasPrefix(text, "invalid arguments: ") {
						t.Fatalf("content text = %q, want a validation refusal (prefix %q)", text, "invalid arguments: ")
					}
					if tc.wantText != "" && text != tc.wantText {
						t.Fatalf("content text = %q, want exactly %q -- the injected schema's own refusal", text, tc.wantText)
					}
				case wantTwinInvoked:
					if twinCalls != 1 {
						t.Fatalf("twin invoked %d time(s) for arguments %s, want exactly 1 -- toolHandler refused arguments the injected schema map accepts (a lazy path that ignores the injected map validates against the real $def instead)", twinCalls, arguments)
					}
					if res.IsError {
						t.Fatalf("IsError = true (content %q), want a successful result", resultText(t, res))
					}
				}
			})
		}
	}
}

// resultText returns res's single text content block.
func resultText(t *testing.T, res *sdkmcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("content = %+v, want exactly one block", res.Content)
	}
	tc, ok := res.Content[0].(*sdkmcp.TextContent)
	if !ok {
		t.Fatalf("content[0] = %T, want *sdkmcp.TextContent", res.Content[0])
	}
	return tc.Text
}
