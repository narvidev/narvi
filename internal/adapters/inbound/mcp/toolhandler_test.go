package mcp

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// This file pins where a tools/call's input schema comes from (round 2
// review of PR #324, findings N1/N3/N4; round 5, T1; round 6, U1/U2/U3;
// round 7, W1). The property: the request path never compiles a schema.
// It validates against the map NewHandler compiled eagerly at boot and
// nothing else. What guarantees it, and by what:
//
//   - newHandler (handler.go) refuses to build a handler unless its map
//     holds a non-nil schema for every tool, and serves every request
//     from a private copy taken at construction
//     (TestInjectedSchemaMap_IncompleteMapRefusedAtConstruction,
//     TestInjectedSchemaMap_CopiedAtConstruction). A handler therefore
//     cannot exist without a complete map, and nothing a request does
//     can change that map.
//   - TestInjectedSchemaMap_DecidesEveryToolCall drives newHandler's full
//     HTTP path with a reject-all and an accept-all map. Per tool, it
//     sends one arguments object the real $def accepts and, for every
//     keyword of the real $def that BuildRequest does not enforce
//     itself, an object violating that keyword which BuildRequest
//     carries through to the twin (toolCallArgs). A layer between the
//     route and the validator that validates against anything but the
//     injected map fails it, cache warm or not, when its verdict on one
//     of those objects differs from the injected map's. The guarantee is
//     for every keyword the rows probe, and no wider.
//   - TestToolCallArgs_ProbeEveryKeywordBuildRequestPassesThrough keeps
//     those rows complete: every keyword of every tool's real input $def,
//     annotations aside, is either probed by a toolCallArgs row or
//     listed, with its reason, in buildRequestEnforces.
//   - TestNewHandler_ValidatesAgainstTheRealContractsDefs pins the thin
//     wrapper: NewHandler builds a handler, and that handler validates
//     against the real contracts $defs.
//   - TestToolHandler_MissingSchemaIsRefusedWithoutCompiling pins
//     toolHandler's missing-schema branch, which no handler newHandler
//     builds can reach: it refuses, and it compiles nothing.
//
// What none of these sees:
//
//   - a compile on the request path whose result nothing validates
//     against, since it changes no outcome;
//   - a NewHandler that stops delegating to newHandler and builds its own
//     request path;
//   - a validator on the request path whose verdict agrees with the
//     injected map on every arguments object toolCallArgs sends: one
//     built only from a keyword in buildRequestEnforces, which changes
//     the refusal's text but never whether the twin runs, or one applied
//     to a form of the arguments in which a row's violation no longer
//     shows -- e.g. buildListSessionsRequest's normalized {filter,
//     limit}, in which a null filter or limit is gone and "5" is 5.
//
// TestNoSecondJSONSchemaCompiler (schemas_test.go) and an isolated `go
// test -race -run TestToolCall_ConcurrentFirstCalls_NoRace` are the only
// checks left for those, and the race run sees a request-path compile
// only when it shares a compiler across requests.

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

// keywordProbe is one arguments object that violates the named
// keyword(s) of a tool's real input $def, and no other keyword of it,
// and that the tool's BuildRequest still carries through to the twin if
// nothing refuses it first. keywords are JSON pointers into the $def
// (the entry inputSchema returns). realRefusal is the real $def's exact
// refusal of arguments, which names the location and the keyword.
type keywordProbe struct {
	keywords    []string
	arguments   string
	realRefusal string
}

// label names p in subtest names and failure messages: its keywords,
// dotted rather than slashed so they do not read as subtest levels, then
// its arguments.
func (p keywordProbe) label() string {
	names := make([]string, len(p.keywords))
	for i, k := range p.keywords {
		names[i] = strings.ReplaceAll(strings.TrimPrefix(k, "/"), "/", ".")
	}
	return strings.Join(names, "+") + " " + p.arguments
}

// toolCallArgs is, per tool: realValid, an arguments object the tool's
// real contracts $def accepts and its BuildRequest carries through to
// the twin; and realInvalid, a keywordProbe for every keyword of that
// $def that BuildRequest does not enforce itself
// (TestToolCallArgs_ProbeEveryKeywordBuildRequestPassesThrough;
// buildRequestEnforces lists the rest). Whether BuildRequest really
// carries each probe through is what the accept-all rows of
// TestInjectedSchemaMap_DecidesEveryToolCall check; whether the real
// $def really refuses it, on that keyword, is what their control rows
// check.
var toolCallArgs = map[string]struct {
	realValid   string
	realInvalid []keywordProbe
}{
	"narvi_list_models": {
		realValid: `{}`,
		realInvalid: []keywordProbe{
			// noArguments ignores its arguments entirely.
			{keywords: []string{"/type"}, arguments: `null`, realRefusal: "invalid arguments: - at '': got null, want object"},
			{keywords: []string{"/additionalProperties"}, arguments: `{"bogus":1}`, realRefusal: "invalid arguments: - at '': additional properties 'bogus' not allowed"},
		},
	},
	"narvi_list_sessions": {
		realValid: `{"filter":"all","limit":5}`,
		realInvalid: []keywordProbe{
			// json.Unmarshal of null into listSessionsArgs is a no-op.
			{keywords: []string{"/type"}, arguments: `null`, realRefusal: "invalid arguments: - at '': got null, want object"},
			{keywords: []string{"/additionalProperties"}, arguments: `{"filter":"all","bogus":1}`, realRefusal: "invalid arguments: - at '': additional properties 'bogus' not allowed"},
			// A null leaves the *Filter nil without calling its
			// UnmarshalJSON, which refuses every string outside the enum:
			// null is the one enum violation BuildRequest carries
			// through, so this row probes "enum" as well as "type".
			{keywords: []string{"/properties/filter/type", "/properties/filter/enum"}, arguments: `{"filter":null}`, realRefusal: "invalid arguments: - at '/filter': got null, want string"},
			// encoding/json decodes a quoted number into a json.Number,
			// and a null into a nil *json.Number.
			{keywords: []string{"/properties/limit/type"}, arguments: `{"limit":"5"}`, realRefusal: "invalid arguments: - at '/limit': got string, want integer"},
			{keywords: []string{"/properties/limit/type"}, arguments: `{"limit":null}`, realRefusal: "invalid arguments: - at '/limit': got null, want integer"},
			// buildListSessionsRequest range-checks limit against int
			// only, never against minimum.
			{keywords: []string{"/properties/limit/minimum"}, arguments: `{"limit":0}`, realRefusal: "invalid arguments: - at '/limit': minimum: got 0, want 1"},
		},
	},
	"narvi_get_session": {
		realValid: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"}`,
		realInvalid: []keywordProbe{
			// restdtos.GetSessionToolRequest.UnmarshalJSON skips its
			// required check when the arguments decode to a nil map.
			{keywords: []string{"/type"}, arguments: `null`, realRefusal: "invalid arguments: - at '': got null, want object"},
			{keywords: []string{"/additionalProperties"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","bogus":1}`, realRefusal: "invalid arguments: - at '': additional properties 'bogus' not allowed"},
			// The key is present, so required holds; the null leaves
			// SessionId "".
			{keywords: []string{"/properties/sessionId/type"}, arguments: `{"sessionId":null}`, realRefusal: "invalid arguments: - at '/sessionId': got null, want string"},
			{keywords: []string{"/properties/sessionId/format"}, arguments: `{"sessionId":"not-a-uuid"}`, realRefusal: "invalid arguments: - at '/sessionId': 'not-a-uuid' is not valid uuid: must have 5 elements"},
		},
	},
	"narvi_get_session_status": {
		realValid: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"}`,
		realInvalid: []keywordProbe{
			// buildGetSessionStatusRequest decodes through the generated
			// GetSessionStatusToolRequest, exactly as narvi_get_session
			// does through GetSessionToolRequest: the same rows hold.
			{keywords: []string{"/type"}, arguments: `null`, realRefusal: "invalid arguments: - at '': got null, want object"},
			{keywords: []string{"/additionalProperties"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","bogus":1}`, realRefusal: "invalid arguments: - at '': additional properties 'bogus' not allowed"},
			{keywords: []string{"/properties/sessionId/type"}, arguments: `{"sessionId":null}`, realRefusal: "invalid arguments: - at '/sessionId': got null, want string"},
			{keywords: []string{"/properties/sessionId/format"}, arguments: `{"sessionId":"not-a-uuid"}`, realRefusal: "invalid arguments: - at '/sessionId': 'not-a-uuid' is not valid uuid: must have 5 elements"},
		},
	},
	"narvi_get_session_result": {
		realValid: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"}`,
		realInvalid: []keywordProbe{
			// buildGetSessionResultRequest decodes through the generated
			// GetSessionResultToolRequest, exactly as narvi_get_session_status
			// does through its own: the same rows hold.
			{keywords: []string{"/type"}, arguments: `null`, realRefusal: "invalid arguments: - at '': got null, want object"},
			{keywords: []string{"/additionalProperties"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","bogus":1}`, realRefusal: "invalid arguments: - at '': additional properties 'bogus' not allowed"},
			{keywords: []string{"/properties/sessionId/type"}, arguments: `{"sessionId":null}`, realRefusal: "invalid arguments: - at '/sessionId': got null, want string"},
			{keywords: []string{"/properties/sessionId/format"}, arguments: `{"sessionId":"not-a-uuid"}`, realRefusal: "invalid arguments: - at '/sessionId': 'not-a-uuid' is not valid uuid: must have 5 elements"},
		},
	},
	"narvi_wait_for_session": {
		realValid: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","waitSeconds":5}`,
		realInvalid: []keywordProbe{
			// buildWaitForSessionRequest decodes into its own plain
			// struct, like buildGetSessionTranscriptRequest: null
			// arguments are a no-op, an absent or null sessionId leaves it
			// "", a null waitSeconds leaves it unset (the longest wait),
			// a quoted one decodes into a json.Number, and zero is carried
			// through as it is -- each reaches the twin.
			{keywords: []string{"/type"}, arguments: `null`, realRefusal: "invalid arguments: - at '': got null, want object"},
			{keywords: []string{"/additionalProperties"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","bogus":1}`, realRefusal: "invalid arguments: - at '': additional properties 'bogus' not allowed"},
			{keywords: []string{"/required"}, arguments: `{"waitSeconds":5}`, realRefusal: "invalid arguments: - at '': missing property 'sessionId'"},
			{keywords: []string{"/properties/sessionId/type"}, arguments: `{"sessionId":null}`, realRefusal: "invalid arguments: - at '/sessionId': got null, want string"},
			{keywords: []string{"/properties/sessionId/format"}, arguments: `{"sessionId":"not-a-uuid"}`, realRefusal: "invalid arguments: - at '/sessionId': 'not-a-uuid' is not valid uuid: must have 5 elements"},
			{keywords: []string{"/properties/waitSeconds/type"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","waitSeconds":"5"}`, realRefusal: "invalid arguments: - at '/waitSeconds': got string, want integer"},
			{keywords: []string{"/properties/waitSeconds/type"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","waitSeconds":null}`, realRefusal: "invalid arguments: - at '/waitSeconds': got null, want integer"},
			{keywords: []string{"/properties/waitSeconds/minimum"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","waitSeconds":0}`, realRefusal: "invalid arguments: - at '/waitSeconds': minimum: got 0, want 1"},
		},
	},
	"narvi_get_session_transcript": {
		realValid: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","cursor":"42","limit":5}`,
		realInvalid: []keywordProbe{
			// buildGetSessionTranscriptRequest decodes into its own plain
			// struct, like buildListSessionsRequest: null arguments are a
			// no-op, an absent or null sessionId leaves it "", a null
			// cursor or limit leaves it unset, and a quoted limit decodes
			// into a json.Number -- each carried through to the twin.
			{keywords: []string{"/type"}, arguments: `null`, realRefusal: "invalid arguments: - at '': got null, want object"},
			{keywords: []string{"/additionalProperties"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","bogus":1}`, realRefusal: "invalid arguments: - at '': additional properties 'bogus' not allowed"},
			{keywords: []string{"/required"}, arguments: `{"limit":5}`, realRefusal: "invalid arguments: - at '': missing property 'sessionId'"},
			{keywords: []string{"/properties/sessionId/type"}, arguments: `{"sessionId":null}`, realRefusal: "invalid arguments: - at '/sessionId': got null, want string"},
			{keywords: []string{"/properties/sessionId/format"}, arguments: `{"sessionId":"not-a-uuid"}`, realRefusal: "invalid arguments: - at '/sessionId': 'not-a-uuid' is not valid uuid: must have 5 elements"},
			{keywords: []string{"/properties/cursor/type"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","cursor":null}`, realRefusal: "invalid arguments: - at '/cursor': got null, want string"},
			// The twin parses the cursor itself; the bridge passes it on.
			{keywords: []string{"/properties/cursor/pattern"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","cursor":"07"}`, realRefusal: "invalid arguments: - at '/cursor': '07' does not match pattern '^(0|[1-9][0-9]{0,18})$'"},
			{keywords: []string{"/properties/limit/type"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","limit":"5"}`, realRefusal: "invalid arguments: - at '/limit': got string, want integer"},
			{keywords: []string{"/properties/limit/type"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","limit":null}`, realRefusal: "invalid arguments: - at '/limit': got null, want integer"},
			// buildGetSessionTranscriptRequest range-checks limit against
			// int only, never against minimum.
			{keywords: []string{"/properties/limit/minimum"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","limit":0}`, realRefusal: "invalid arguments: - at '/limit': minimum: got 0, want 1"},
		},
	},
	"narvi_create_session": {
		realValid: `{"prompt":"Fix the flaky test","repos":[{"name":"widgets","url":"https://github.com/acme/widgets"}],"idempotencyKey":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","title":"Flaky test","modelId":"m","effort":"high","planMode":true,"buildModelId":"b","buildEffort":"low"}`,
		realInvalid: []keywordProbe{
			// buildCreateSessionRequest decodes through the generated
			// CreateSessionToolRequest: an unknown key is ignored, and a null
			// leaves an optional pointer nil, planMode false, and a required
			// string "" -- each carried through to the twin, which answers for
			// what it reads (a missing key or no repos is its own 400).
			{keywords: []string{"/additionalProperties"}, arguments: `{"prompt":"Fix the flaky test","repos":[{"name":"widgets","url":"https://github.com/acme/widgets"}],"idempotencyKey":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","spawnSource":"web"}`, realRefusal: "invalid arguments: - at '': additional properties 'spawnSource' not allowed"},
			{keywords: []string{"/properties/title/type"}, arguments: `{"prompt":"Fix the flaky test","repos":[{"name":"widgets","url":"https://github.com/acme/widgets"}],"idempotencyKey":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","title":null}`, realRefusal: "invalid arguments: - at '/title': got null, want string"},
			{keywords: []string{"/properties/repos/type"}, arguments: `{"prompt":"p","repos":null,"idempotencyKey":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"}`, realRefusal: "invalid arguments: - at '/repos': got null, want array"},
			// The whole items keyword, probed through one element's unknown key.
			{keywords: []string{"/properties/repos/items"}, arguments: `{"prompt":"p","repos":[{"name":"widgets","url":"https://github.com/acme/widgets","owner":"acme"}],"idempotencyKey":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"}`, realRefusal: "invalid arguments: - at '/repos/0': additional properties 'owner' not allowed"},
			{keywords: []string{"/properties/modelId/type"}, arguments: `{"prompt":"Fix the flaky test","repos":[{"name":"widgets","url":"https://github.com/acme/widgets"}],"idempotencyKey":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","modelId":null}`, realRefusal: "invalid arguments: - at '/modelId': got null, want string"},
			{keywords: []string{"/properties/effort/type"}, arguments: `{"prompt":"Fix the flaky test","repos":[{"name":"widgets","url":"https://github.com/acme/widgets"}],"idempotencyKey":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","effort":null}`, realRefusal: "invalid arguments: - at '/effort': got null, want string"},
			{keywords: []string{"/properties/planMode/type"}, arguments: `{"prompt":"Fix the flaky test","repos":[{"name":"widgets","url":"https://github.com/acme/widgets"}],"idempotencyKey":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","planMode":null}`, realRefusal: "invalid arguments: - at '/planMode': got null, want boolean"},
			{keywords: []string{"/properties/buildModelId/type"}, arguments: `{"prompt":"Fix the flaky test","repos":[{"name":"widgets","url":"https://github.com/acme/widgets"}],"idempotencyKey":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","buildModelId":null}`, realRefusal: "invalid arguments: - at '/buildModelId': got null, want string"},
			{keywords: []string{"/properties/buildEffort/type"}, arguments: `{"prompt":"Fix the flaky test","repos":[{"name":"widgets","url":"https://github.com/acme/widgets"}],"idempotencyKey":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","buildEffort":null}`, realRefusal: "invalid arguments: - at '/buildEffort': got null, want string"},
			{keywords: []string{"/properties/idempotencyKey/type"}, arguments: `{"prompt":"p","repos":[{"name":"widgets","url":"https://github.com/acme/widgets"}],"idempotencyKey":null}`, realRefusal: "invalid arguments: - at '/idempotencyKey': got null, want string"},
			{keywords: []string{"/properties/idempotencyKey/format"}, arguments: `{"prompt":"p","repos":[{"name":"widgets","url":"https://github.com/acme/widgets"}],"idempotencyKey":"not-a-uuid"}`, realRefusal: "invalid arguments: - at '/idempotencyKey': 'not-a-uuid' is not valid uuid: must have 5 elements"},
		},
	},
	"narvi_list_plans": {
		realValid: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"}`,
		realInvalid: []keywordProbe{
			// buildListPlansRequest decodes through the generated
			// ListPlansToolRequest, exactly as narvi_get_session does through
			// its own: the same rows hold.
			{keywords: []string{"/type"}, arguments: `null`, realRefusal: "invalid arguments: - at '': got null, want object"},
			{keywords: []string{"/additionalProperties"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","bogus":1}`, realRefusal: "invalid arguments: - at '': additional properties 'bogus' not allowed"},
			{keywords: []string{"/properties/sessionId/type"}, arguments: `{"sessionId":null}`, realRefusal: "invalid arguments: - at '/sessionId': got null, want string"},
			{keywords: []string{"/properties/sessionId/format"}, arguments: `{"sessionId":"not-a-uuid"}`, realRefusal: "invalid arguments: - at '/sessionId': 'not-a-uuid' is not valid uuid: must have 5 elements"},
		},
	},
	"narvi_approve_plan": {
		realValid: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","planId":"6c2d2f3f-7c2b-4c2b-8c2b-7c2b5c2b8c2b"}`,
		realInvalid: []keywordProbe{
			// restdtos.ApprovePlanToolRequest.UnmarshalJSON skips its required check when the
			// arguments decode to a nil map, and a null id leaves it "" --
			// each carried through to the twin, which answers 400 for it.
			{keywords: []string{"/type"}, arguments: `null`, realRefusal: "invalid arguments: - at '': got null, want object"},
			{keywords: []string{"/additionalProperties"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","planId":"6c2d2f3f-7c2b-4c2b-8c2b-7c2b5c2b8c2b","bogus":1}`, realRefusal: "invalid arguments: - at '': additional properties 'bogus' not allowed"},
			{keywords: []string{"/properties/sessionId/type"}, arguments: `{"sessionId":null,"planId":"6c2d2f3f-7c2b-4c2b-8c2b-7c2b5c2b8c2b"}`, realRefusal: "invalid arguments: - at '/sessionId': got null, want string"},
			{keywords: []string{"/properties/sessionId/format"}, arguments: `{"sessionId":"not-a-uuid","planId":"6c2d2f3f-7c2b-4c2b-8c2b-7c2b5c2b8c2b"}`, realRefusal: "invalid arguments: - at '/sessionId': 'not-a-uuid' is not valid uuid: must have 5 elements"},
			{keywords: []string{"/properties/planId/type"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","planId":null}`, realRefusal: "invalid arguments: - at '/planId': got null, want string"},
			{keywords: []string{"/properties/planId/format"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","planId":"not-a-uuid"}`, realRefusal: "invalid arguments: - at '/planId': 'not-a-uuid' is not valid uuid: must have 5 elements"},
		},
	},
	"narvi_reject_plan": {
		realValid: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","planId":"6c2d2f3f-7c2b-4c2b-8c2b-7c2b5c2b8c2b"}`,
		realInvalid: []keywordProbe{
			// restdtos.RejectPlanToolRequest.UnmarshalJSON skips its required check when the
			// arguments decode to a nil map, and a null id leaves it "" --
			// each carried through to the twin, which answers 400 for it.
			{keywords: []string{"/type"}, arguments: `null`, realRefusal: "invalid arguments: - at '': got null, want object"},
			{keywords: []string{"/additionalProperties"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","planId":"6c2d2f3f-7c2b-4c2b-8c2b-7c2b5c2b8c2b","bogus":1}`, realRefusal: "invalid arguments: - at '': additional properties 'bogus' not allowed"},
			{keywords: []string{"/properties/sessionId/type"}, arguments: `{"sessionId":null,"planId":"6c2d2f3f-7c2b-4c2b-8c2b-7c2b5c2b8c2b"}`, realRefusal: "invalid arguments: - at '/sessionId': got null, want string"},
			{keywords: []string{"/properties/sessionId/format"}, arguments: `{"sessionId":"not-a-uuid","planId":"6c2d2f3f-7c2b-4c2b-8c2b-7c2b5c2b8c2b"}`, realRefusal: "invalid arguments: - at '/sessionId': 'not-a-uuid' is not valid uuid: must have 5 elements"},
			{keywords: []string{"/properties/planId/type"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","planId":null}`, realRefusal: "invalid arguments: - at '/planId': got null, want string"},
			{keywords: []string{"/properties/planId/format"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","planId":"not-a-uuid"}`, realRefusal: "invalid arguments: - at '/planId': 'not-a-uuid' is not valid uuid: must have 5 elements"},
		},
	},
	"narvi_request_plan_revision": {
		realValid: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","feedback":"keep the env fallback","modelId":"m","effort":"high"}`,
		realInvalid: []keywordProbe{
			// buildRequestPlanRevisionRequest decodes through the generated
			// RequestPlanRevisionToolRequest: an unknown key is ignored, a null sessionId leaves it "", and a
			// null modelId or effort leaves it nil -- each carried through to
			// the twin. planMode is not an argument: the tool sets it.
			{keywords: []string{"/additionalProperties"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","feedback":"keep the env fallback","planMode":true}`, realRefusal: "invalid arguments: - at '': additional properties 'planMode' not allowed"},
			{keywords: []string{"/properties/sessionId/type"}, arguments: `{"sessionId":null,"feedback":"keep the env fallback"}`, realRefusal: "invalid arguments: - at '/sessionId': got null, want string"},
			{keywords: []string{"/properties/sessionId/format"}, arguments: `{"sessionId":"not-a-uuid","feedback":"keep the env fallback"}`, realRefusal: "invalid arguments: - at '/sessionId': 'not-a-uuid' is not valid uuid: must have 5 elements"},
			{keywords: []string{"/properties/modelId/type"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","feedback":"keep the env fallback","modelId":null}`, realRefusal: "invalid arguments: - at '/modelId': got null, want string"},
			{keywords: []string{"/properties/effort/type"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","feedback":"keep the env fallback","effort":null}`, realRefusal: "invalid arguments: - at '/effort': got null, want string"},
		},
	},
	"narvi_send_prompt": {
		realValid: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","prompt":"run the tests again","modelId":"m","effort":"high"}`,
		realInvalid: []keywordProbe{
			// buildSendPromptRequest decodes through the generated
			// SendPromptToolRequest: an unknown key is ignored, a null sessionId leaves it "", and a
			// null modelId or effort leaves it nil -- each carried through to
			// the twin. planMode is not an argument: the tool sets it.
			{keywords: []string{"/additionalProperties"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","prompt":"run the tests again","planMode":true}`, realRefusal: "invalid arguments: - at '': additional properties 'planMode' not allowed"},
			{keywords: []string{"/properties/sessionId/type"}, arguments: `{"sessionId":null,"prompt":"run the tests again"}`, realRefusal: "invalid arguments: - at '/sessionId': got null, want string"},
			{keywords: []string{"/properties/sessionId/format"}, arguments: `{"sessionId":"not-a-uuid","prompt":"run the tests again"}`, realRefusal: "invalid arguments: - at '/sessionId': 'not-a-uuid' is not valid uuid: must have 5 elements"},
			{keywords: []string{"/properties/modelId/type"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","prompt":"run the tests again","modelId":null}`, realRefusal: "invalid arguments: - at '/modelId': got null, want string"},
			{keywords: []string{"/properties/effort/type"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","prompt":"run the tests again","effort":null}`, realRefusal: "invalid arguments: - at '/effort': got null, want string"},
		},
	},
	"narvi_stop_session": {
		realValid: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"}`,
		realInvalid: []keywordProbe{
			// buildStopSessionRequest decodes through the generated
			// StopSessionToolRequest, exactly as narvi_list_plans does through
			// its own: the same rows hold.
			{keywords: []string{"/type"}, arguments: `null`, realRefusal: "invalid arguments: - at '': got null, want object"},
			{keywords: []string{"/additionalProperties"}, arguments: `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a","bogus":1}`, realRefusal: "invalid arguments: - at '': additional properties 'bogus' not allowed"},
			{keywords: []string{"/properties/sessionId/type"}, arguments: `{"sessionId":null}`, realRefusal: "invalid arguments: - at '/sessionId': got null, want string"},
			{keywords: []string{"/properties/sessionId/format"}, arguments: `{"sessionId":"not-a-uuid"}`, realRefusal: "invalid arguments: - at '/sessionId': 'not-a-uuid' is not valid uuid: must have 5 elements"},
		},
	},
}

// buildRequestEnforces lists, per tool, the keywords of its real input
// $def (JSON pointers, as in keywordProbe) that its BuildRequest enforces
// itself, and why. BuildRequest refuses every arguments value violating
// one of these, so none reaches the twin even under the accept-all map
// and no toolCallArgs row can probe it: a request-path validator built
// only from such a keyword changes a refusal's text, never whether the
// twin runs, and TestInjectedSchemaMap_DecidesEveryToolCall does not see
// it. Every other keyword of every tool's input $def has a keywordProbe.
var buildRequestEnforces = map[string]map[string]string{
	"narvi_get_session": {
		"/required": "restdtos.GetSessionToolRequest.UnmarshalJSON refuses every object without a sessionId key; " +
			"the one value it carries through without one, null, is not an object, and required constrains objects only",
	},
	"narvi_get_session_status": {
		"/required": "restdtos.GetSessionStatusToolRequest.UnmarshalJSON refuses every object without a sessionId key; " +
			"the one value it carries through without one, null, is not an object, and required constrains objects only",
	},
	"narvi_get_session_result": {
		"/required": "restdtos.GetSessionResultToolRequest.UnmarshalJSON refuses every object without a sessionId key; " +
			"the one value it carries through without one, null, is not an object, and required constrains objects only",
	},
	"narvi_create_session": {
		"/type": "restdtos.CreateSessionToolRequest.UnmarshalJSON refuses every non-object: an array, string, number or boolean fails to decode, " +
			"and null leaves prompt empty, which its generated minLength check refuses",
		"/required":                    "restdtos.CreateSessionToolRequest.UnmarshalJSON refuses every object without a prompt, repos or idempotencyKey key",
		"/properties/prompt/type":      "a non-string prompt fails to decode, and a null one leaves prompt empty, which the generated minLength check refuses",
		"/properties/prompt/minLength": "restdtos.CreateSessionToolRequest.UnmarshalJSON refuses an empty prompt itself",
		"/properties/repos/minItems": "restdtos.CreateSessionToolRequest.UnmarshalJSON refuses an empty repos array itself; " +
			"the one value it carries through with no repo, null, is not an array, and minItems constrains arrays only",
	},
	"narvi_list_plans": {
		"/required": "restdtos.ListPlansToolRequest.UnmarshalJSON refuses every object without a sessionId key; " +
			"the one value it carries through without one, null, is not an object, and required constrains objects only",
	},
	"narvi_approve_plan": {
		"/required": "restdtos.ApprovePlanToolRequest.UnmarshalJSON refuses every object without a sessionId or planId key; " +
			"the one value it carries through without them, null, is not an object, and required constrains objects only",
	},
	"narvi_reject_plan": {
		"/required": "restdtos.RejectPlanToolRequest.UnmarshalJSON refuses every object without a sessionId or planId key; " +
			"the one value it carries through without them, null, is not an object, and required constrains objects only",
	},
	"narvi_request_plan_revision": {
		"/type": "restdtos.RequestPlanRevisionToolRequest.UnmarshalJSON refuses every non-object: an array, string, number or boolean fails to decode, " +
			"and null leaves feedback empty, which its generated minLength check refuses",
		"/required":                      "restdtos.RequestPlanRevisionToolRequest.UnmarshalJSON refuses every object without a sessionId or feedback key",
		"/properties/feedback/type":      "a non-string feedback fails to decode, and a null one leaves feedback empty, which the generated minLength check refuses",
		"/properties/feedback/minLength": "restdtos.RequestPlanRevisionToolRequest.UnmarshalJSON refuses an empty feedback itself",
	},
	"narvi_send_prompt": {
		"/type": "restdtos.SendPromptToolRequest.UnmarshalJSON refuses every non-object: an array, string, number or boolean fails to decode, " +
			"and null leaves prompt empty, which its generated minLength check refuses",
		"/required":                    "restdtos.SendPromptToolRequest.UnmarshalJSON refuses every object without a sessionId or prompt key",
		"/properties/prompt/type":      "a non-string prompt fails to decode, and a null one leaves prompt empty, which the generated minLength check refuses",
		"/properties/prompt/minLength": "restdtos.SendPromptToolRequest.UnmarshalJSON refuses an empty prompt itself",
	},
	"narvi_stop_session": {
		"/required": "restdtos.StopSessionToolRequest.UnmarshalJSON refuses every object without a sessionId key; " +
			"the one value it carries through without one, null, is not an object, and required constrains objects only",
	},
}

// twinBodies is the 200 body each tool's counting twin answers with --
// distinct per twin, so a successful result also shows WHICH twin ran.
// narvi_wait_for_session's twin is narvi_get_session_status' own (the
// status route, with ?waitSeconds=), so the two share one body, and so do
// narvi_request_plan_revision and narvi_send_prompt, whose twin is POST
// .../turns.
var twinBodies = map[string]string{
	"narvi_list_models":            `{"providers":[]}`,
	"narvi_list_sessions":          `{"sessions":[]}`,
	"narvi_get_session":            `{"id":"x"}`,
	"narvi_get_session_status":     `{"activity":"idle"}`,
	"narvi_wait_for_session":       `{"activity":"idle"}`,
	"narvi_get_session_result":     `{"reviewScope":"none"}`,
	"narvi_get_session_transcript": `{"events":[],"nextCursor":null}`,
	"narvi_create_session":         `{"id":"created"}`,
	"narvi_list_plans":             `{"plans":[]}`,
	"narvi_approve_plan":           `{"status":"approved"}`,
	"narvi_reject_plan":            `{"status":"rejected"}`,
	"narvi_request_plan_revision":  `{"status":"pending"}`,
	"narvi_send_prompt":            `{"status":"pending"}`,
	"narvi_stop_session":           `{"openTurns":0}`,
}

// countingTwins returns Twins whose handlers each add one to
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
		// Row 182's two twins (technical plan §43.20); the status twin
		// serves narvi_wait_for_session too.
		GetSessionStatus: twin("narvi_get_session_status"),
		ListEvents:       twin("narvi_get_session_transcript"),
		// Row 182's result (piece (c)).
		GetSessionResult: twin("narvi_get_session_result"),
		// The create twin (technical plan §43.8).
		CreateSession: twin("narvi_create_session"),
		// The plan and turn twins (technical plan §43.21); the turn twin
		// serves both turn tools, which share one body.
		ListPlans:   twin("narvi_list_plans"),
		ApprovePlan: twin("narvi_approve_plan"),
		RejectPlan:  twin("narvi_reject_plan"),
		CreateTurn:  twin("narvi_send_prompt"),
		// The stop twin (technical plan §43.22).
		StopSession: twin("narvi_stop_session"),
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
// against the schema map newHandler was built with, all the way down
// newHandler's real request path (route gates, versionGate, the SDK's
// Streamable HTTP handler, the per-request closure, buildServer,
// registerTools, toolHandler). A layer anywhere on that path that
// validates against something else, instead of the map or on top of it
// -- a lazy map, a per-request compile, a shared compiler, a map
// compiled at package init, whether or not it goes through
// compileInputSchemas -- fails one of these rows when its verdict on one
// of the arguments objects they send differs from the injected map's.
// That holds for every keyword the rows probe, and no wider: the top of
// this file lists what it leaves out. Per tool:
//
//   - reject-all map, arguments VALID for the tool's real $def: refused
//     with exactly the `false` schema's refusal, the twin never invoked.
//     Validating against the real $def instead of the map, or as an
//     alternative to it, accepts them and invokes the twin.
//   - accept-all map, arguments valid for the real $def: the twin
//     invoked, so this test cannot pass on a path that refuses
//     everything.
//   - accept-all map, one row per toolCallArgs keywordProbe: arguments
//     violating that keyword of the real $def, which BuildRequest carries
//     through, reach the twin. A validator carrying that keyword --
//     the whole real $def or any part of it that holds the keyword --
//     refuses them when it sees them in a form that keeps the violation,
//     as the raw arguments do.
//   - control rows, against the real map compileInputSchemas compiles:
//     the real-valid arguments reach the twin, and each probe's
//     arguments are refused with exactly its realRefusal -- so every
//     probe provably violates, in the real $def, the keyword it names.
//
// It replaces nothing at runtime and depends on neither test order,
// -race, nor a race actually firing, so a lazy cache warmed by an
// earlier test in the same binary cannot hide a regression from it.
func TestInjectedSchemaMap_DecidesEveryToolCall(t *testing.T) {
	realSchemas, err := compileInputSchemas(toolInputDefs())
	if err != nil {
		t.Fatalf("compileInputSchemas(toolInputDefs()) error = %v", err)
	}
	var twinCalls atomic.Int32
	twins := countingTwins(&twinCalls)
	rejectAll := newTestHandlerWithInputSchemas(t, twins, constantInputSchemas(t, false))
	acceptAll := newTestHandlerWithInputSchemas(t, twins, constantInputSchemas(t, true))
	realMap := newTestHandlerWithInputSchemas(t, twins, realSchemas)

	type row struct {
		name        string
		handler     http.Handler
		arguments   string
		wantRefusal string // "" = the call must reach the twin; otherwise its exact refusal
	}
	for _, toolName := range eachToolWithArgs(t) {
		args := toolCallArgs[toolName]
		rows := []row{
			{name: "reject-all map, real-valid arguments are refused", handler: rejectAll, arguments: args.realValid, wantRefusal: rejectAllRefusalText},
			{name: "accept-all map, real-valid arguments reach the twin", handler: acceptAll, arguments: args.realValid},
			{name: "control, real map, real-valid arguments reach the twin", handler: realMap, arguments: args.realValid},
		}
		for _, p := range args.realInvalid {
			rows = append(rows,
				row{name: "accept-all map, arguments violating " + p.label() + " reach the twin", handler: acceptAll, arguments: p.arguments},
				row{name: "control, real map, arguments violating " + p.label() + " are refused on that keyword", handler: realMap, arguments: p.arguments, wantRefusal: p.realRefusal},
			)
		}
		for _, tc := range rows {
			t.Run(toolName+"/"+tc.name, func(t *testing.T) {
				twinCalls.Store(0)
				text, isError := postToolCall(t, tc.handler, toolName, tc.arguments)
				if tc.wantRefusal == "" {
					assertTwinAnswered(t, toolName, tc.arguments, twinCalls.Load(), text, isError)
					return
				}
				assertRefused(t, toolName, tc.arguments, twinCalls.Load(), text, isError)
				if text != tc.wantRefusal {
					t.Fatalf("refusal text = %q, want exactly %q", text, tc.wantRefusal)
				}
			})
		}
	}
}

// TestToolCallArgs_ProbeEveryKeywordBuildRequestPassesThrough keeps
// toolCallArgs complete. It walks every tool's real input $def -- the
// verbatim contracts entry inputSchema returns -- and collects every
// keyword in it as a JSON pointer (collectKeywordPointers). Each one
// must be named by a keywordProbe in toolCallArgs or listed in
// buildRequestEnforces, not both, and every keyword either of them names
// must be in the $def. A keyword added to a contracts input $def
// therefore fails this test until it gets a probe, or a stated reason
// why BuildRequest makes one impossible.
func TestToolCallArgs_ProbeEveryKeywordBuildRequestPassesThrough(t *testing.T) {
	eachToolWithArgs(t) // every tool has a toolCallArgs row
	for _, spec := range toolSpecs(Twins{}) {
		t.Run(spec.Name, func(t *testing.T) {
			def, err := inputSchema(spec.InputDef)
			if err != nil {
				t.Fatalf("inputSchema(%q) error = %v", spec.InputDef, err)
			}
			inDef := map[string]bool{}
			collectKeywordPointers("", def, inDef)
			if len(inDef) == 0 {
				t.Fatalf("$def %q yielded no keywords -- the walk is broken, not the $def", spec.InputDef)
			}

			coveredBy := map[string]string{}
			for _, p := range toolCallArgs[spec.Name].realInvalid {
				for _, k := range p.keywords {
					coveredBy[k] = "probed by " + p.arguments
				}
			}
			for k := range buildRequestEnforces[spec.Name] {
				if how, ok := coveredBy[k]; ok {
					t.Errorf("keyword %s is %s and also listed in buildRequestEnforces -- a keyword BuildRequest enforces cannot reach the twin, so one of the two is wrong", k, how)
				}
				coveredBy[k] = "listed in buildRequestEnforces"
			}
			for _, k := range slices.Sorted(maps.Keys(inDef)) {
				if _, ok := coveredBy[k]; !ok {
					t.Errorf("keyword %s of $def %q has no keywordProbe in toolCallArgs[%q] and is not in buildRequestEnforces -- add arguments violating it that BuildRequest carries through to the twin, or the reason none exist", k, spec.InputDef, spec.Name)
				}
			}
			for _, k := range slices.Sorted(maps.Keys(coveredBy)) {
				if !inDef[k] {
					t.Errorf("keyword %s (%s) is not in $def %q", k, coveredBy[k], spec.InputDef)
				}
			}
		})
	}
}

// schemaAnnotations are the keywords collectKeywordPointers skips: they
// describe a schema and assert nothing about an instance.
var schemaAnnotations = map[string]bool{
	"$comment": true, "default": true, "deprecated": true, "description": true,
	"examples": true, "readOnly": true, "title": true, "writeOnly": true,
}

// collectKeywordPointers adds to out the JSON pointer, under at, of
// every keyword in schema except the annotations. It descends into
// "properties", whose value holds subschemas rather than one keyword's
// argument. Every other key counts as one keyword whatever its value
// holds, so a keyword with subschemas of its own ("items", "allOf", ...)
// is a single entry to cover as a whole: adding one fails
// TestToolCallArgs_ProbeEveryKeywordBuildRequestPassesThrough instead of
// slipping past it.
func collectKeywordPointers(at string, schema map[string]any, out map[string]bool) {
	escape := strings.NewReplacer("~", "~0", "/", "~1").Replace
	for key, val := range schema {
		switch {
		case schemaAnnotations[key]:
		case key == "properties":
			props, ok := val.(map[string]any)
			if !ok {
				out[at+"/properties"] = true
				continue
			}
			for name, sub := range props {
				ptr := at + "/properties/" + escape(name)
				if subSchema, ok := sub.(map[string]any); ok {
					collectKeywordPointers(ptr, subSchema, out)
				} else {
					out[ptr] = true // a boolean subschema
				}
			}
		default:
			out[at+"/"+escape(key)] = true
		}
	}
}

// TestNewHandler_ValidatesAgainstTheRealContractsDefs pins the thin
// wrapper. NewHandler, with a real Config, builds a handler (its eager
// compile succeeds on this build's contracts, and newHandler accepts the
// result as complete), and for every tool that handler validates
// against the real contracts $def: realValid reaches the twin, and every
// toolCallArgs probe is refused with exactly its realRefusal, the real
// $def's own refusal on the keyword the probe names -- not the
// reject-all text, not an internal error. That rules out NewHandler
// handing newHandler a constant, empty or placeholder map, or one that
// lacks a keyword the probes cover.
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
		for _, p := range args.realInvalid {
			t.Run(toolName+"/arguments violating "+p.label()+" are refused by the real $def", func(t *testing.T) {
				twinCalls.Store(0)
				text, isError := postToolCall(t, handler, toolName, p.arguments)
				assertRefused(t, toolName, p.arguments, twinCalls.Load(), text, isError)
				if text != p.realRefusal {
					t.Fatalf("refusal text = %q, want exactly %q -- the real $def's own refusal of %s", text, p.realRefusal, p.label())
				}
			})
		}
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
			handler, err := newHandler(testConfig(testPublicBaseURL), testTwins(), tc.schemas)
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
// newHandler returns changes nothing, and every toolCallArgs probe, each
// of which the real $defs refuse, still reaches its twin under the
// accept-all map the handler was built with. A handler that kept the
// caller's map would refuse them.
func TestInjectedSchemaMap_CopiedAtConstruction(t *testing.T) {
	var twinCalls atomic.Int32
	injected := constantInputSchemas(t, true)
	handler := newTestHandlerWithInputSchemas(t, countingTwins(&twinCalls), injected)
	maps.Copy(injected, constantInputSchemas(t, false))

	for _, toolName := range eachToolWithArgs(t) {
		for _, p := range toolCallArgs[toolName].realInvalid {
			t.Run(toolName+"/"+p.label(), func(t *testing.T) {
				twinCalls.Store(0)
				text, isError := postToolCall(t, handler, toolName, p.arguments)
				assertTwinAnswered(t, toolName, p.arguments, twinCalls.Load(), text, isError)
			})
		}
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
				res, err := spec.toolHandler(t.Context(), m.schemas, allowAllBrake{})(t.Context(), &sdkmcp.CallToolRequest{
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
