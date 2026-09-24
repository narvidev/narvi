package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// callToolBody builds a modern (2026-07-28) tools/call request body
// naming toolName and arguments (already-encoded JSON, or "{}").
func callToolBody(id int, toolName, argumentsJSON string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s,"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, id, toolName, argumentsJSON)
}

func callToolHeaders(toolName string) map[string]string {
	return map[string]string{
		protocolVersionHeader: "2026-07-28",
		"Mcp-Method":          "tools/call",
		"Mcp-Name":            toolName,
	}
}

type callToolResultEnvelope struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Result  *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent any  `json:"structuredContent"`
		IsError           bool `json:"isError"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// TestToolCall_ListModels_HappyPath drives narvi_list_models through the
// FULL stack (RequireEnabled -> auth -> versionGate -> the real SDK ->
// the bridge -> mapOutcome), proving a 200 from the twin becomes a
// successful CallToolResult whose content/structuredContent are the
// twin's own body, byte for byte.
func TestToolCall_ListModels_HappyPath(t *testing.T) {
	const wantBody = `{"providers":[{"id":"openai","models":[]}]}`
	twins := testTwins()
	twins.ListModels = stubHandler(http.StatusOK, wantBody)
	handler := newTestHandler(t, true, true, twins)

	status, body := rawPost(t, handler, "/mcp", callToolBody(1, "narvi_list_models", "{}"), callToolHeaders("narvi_list_models"))
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", status, body)
	}
	var env callToolResultEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	if env.Error != nil {
		t.Fatalf("error = %+v, want a successful result", env.Error)
	}
	if env.Result == nil {
		t.Fatal("result = nil")
	}
	if env.Result.IsError {
		t.Error("IsError = true, want false")
	}
	if len(env.Result.Content) != 1 || env.Result.Content[0].Text != wantBody {
		t.Errorf("content = %+v, want a single text block = %q", env.Result.Content, wantBody)
	}
	structuredJSON, err := json.Marshal(env.Result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structuredContent: %v", err)
	}
	var gotStructured, wantStructured any
	_ = json.Unmarshal(structuredJSON, &gotStructured)
	_ = json.Unmarshal([]byte(wantBody), &wantStructured)
	gotCanon, _ := json.Marshal(gotStructured)
	wantCanon, _ := json.Marshal(wantStructured)
	if string(gotCanon) != string(wantCanon) {
		t.Errorf("structuredContent = %s, want (canonical) %s", gotCanon, wantCanon)
	}
}

// TestToolCall_GetSession_NotFoundIsErrorTrue proves a 404 from the twin
// becomes IsError:true with the twin's own error text -- a SUCCESSFUL
// JSON-RPC response (no top-level "error"), per §43 D6.
func TestToolCall_GetSession_NotFoundIsErrorTrue(t *testing.T) {
	twins := testTwins()
	twins.GetSession = stubHandler(http.StatusNotFound, `{"error":"session not found"}`)
	handler := newTestHandler(t, true, true, twins)

	args := `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"}`
	status, body := rawPost(t, handler, "/mcp", callToolBody(1, "narvi_get_session", args), callToolHeaders("narvi_get_session"))
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 (isError is a SUCCESSFUL response)", status, body)
	}
	var env callToolResultEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	if env.Error != nil {
		t.Fatalf("error = %+v, want no top-level JSON-RPC error", env.Error)
	}
	if env.Result == nil || !env.Result.IsError {
		t.Fatalf("result = %+v, want IsError:true", env.Result)
	}
	if len(env.Result.Content) != 1 || env.Result.Content[0].Text != "session not found" {
		t.Errorf("content = %+v, want text = %q", env.Result.Content, "session not found")
	}
	if env.Result.StructuredContent != nil {
		t.Errorf("structuredContent = %v, want nil on an isError result", env.Result.StructuredContent)
	}
}

// TestToolCall_GetSession_MalformedIDIsToolExecutionError proves a
// malformed sessionId is caught by THIS package's own argument
// validation (schemas.go's validateArguments, "format":"uuid" enforced
// with format assertions on) BEFORE the twin is ever invoked -- a TOOL
// EXECUTION error (isError:true), never a JSON-RPC protocol code, per
// the MCP tools specification's own classification of an
// input-validation failure. A prior revision of this test (and of
// contracts/rest/v1/dtos.schema.json's own GetSessionToolRequest.
// sessionId doc comment) asserted the OPPOSITE on the premise that the
// pinned SDK enforced no schema keyword on the raw Server.AddTool path
// at all -- true of the SDK itself, but this package now validates
// arguments itself before BuildRequest or the twin ever sees them.
func TestToolCall_GetSession_MalformedIDIsToolExecutionError(t *testing.T) {
	var gotPath string
	twins := testTwins()
	twins.GetSession = func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"malformed session id"}`))
	}
	handler := newTestHandler(t, true, true, twins)

	args := `{"sessionId":"not-a-uuid"}`
	status, body := rawPost(t, handler, "/mcp", callToolBody(1, "narvi_get_session", args), callToolHeaders("narvi_get_session"))
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 (isError:true is a SUCCESSFUL JSON-RPC response)", status, body)
	}
	if gotPath != "" {
		t.Fatal("the twin was invoked -- \"not-a-uuid\" must be rejected by this package's own bridge (format:\"uuid\") before BuildRequest or the twin ever runs")
	}
	var env callToolResultEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	if env.Error != nil {
		t.Fatalf("error = %+v, want no top-level JSON-RPC error", env.Error)
	}
	if env.Result == nil || !env.Result.IsError {
		t.Fatalf("result = %+v, want IsError:true", env.Result)
	}
	if len(env.Result.Content) != 1 || !strings.Contains(env.Result.Content[0].Text, "sessionId") {
		t.Errorf("content = %+v, want a message naming the offending field (\"sessionId\")", env.Result.Content)
	}
}

// TestToolCall_ListSessions_BadFilterIsToolExecutionError proves
// filter:"x" is caught by THIS package's own argument validation (the
// "enum":["mine","all"] restored to contracts/rest/v1/dtos.schema.json's
// ListSessionsToolRequest.filter) BEFORE the twin is ever invoked -- the
// twin is never called at all, unlike a prior revision of this test
// (and that $def's own doc comment), which asserted the OPPOSITE on the
// mistaken premise that declaring "enum" would let the pinned SDK's own
// generic validation intercept it FIRST, with a less specific message --
// the SDK's raw Server.AddTool path validates nothing itself; see
// schemas.go's own doc comment.
func TestToolCall_ListSessions_BadFilterIsToolExecutionError(t *testing.T) {
	invoked := false
	twins := testTwins()
	twins.ListSessions = func(w http.ResponseWriter, _ *http.Request) {
		invoked = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"filter must be \"mine\" or \"all\""}`))
	}
	handler := newTestHandler(t, true, true, twins)

	status, body := rawPost(t, handler, "/mcp", callToolBody(1, "narvi_list_sessions", `{"filter":"x"}`), callToolHeaders("narvi_list_sessions"))
	if invoked {
		t.Fatal("the twin was invoked -- \"x\" must be rejected by this package's own bridge (enum: mine|all) before BuildRequest or the twin ever runs")
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 (isError:true is a SUCCESSFUL JSON-RPC response)", status, body)
	}
	var env callToolResultEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	if env.Error != nil {
		t.Fatalf("error = %+v, want no top-level JSON-RPC error", env.Error)
	}
	if env.Result == nil || !env.Result.IsError {
		t.Fatalf("result = %+v, want IsError:true", env.Result)
	}
	if len(env.Result.Content) != 1 || !strings.Contains(env.Result.Content[0].Text, "filter") {
		t.Errorf("content = %+v, want a message naming the offending field (\"filter\")", env.Result.Content)
	}
}

// TestToolCall_ListSessions_InvalidLimitIsToolExecutionError proves
// limit:0 and limit:-5 (both below the restored "minimum":1) are caught
// by this package's own bridge -- before the twin is ever invoked.
// Deliberately no "maximum" case here: the ListSessionsToolRequest.limit
// $def carries no "maximum" at all (its own doc comment, contracts/
// rest/v1/dtos.schema.json, covers why -- tools/contractscompat does not
// recognize that keyword yet, and REST itself does not reject an
// over-large limit either, only clamps it), so limit:300 reaches the
// twin exactly like the REST route's own identical clamping behavior.
func TestToolCall_ListSessions_InvalidLimitIsToolExecutionError(t *testing.T) {
	for _, limit := range []int{0, -5} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			invoked := false
			twins := testTwins()
			twins.ListSessions = func(w http.ResponseWriter, _ *http.Request) {
				invoked = true
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"sessions":[]}`))
			}
			handler := newTestHandler(t, true, true, twins)

			args := fmt.Sprintf(`{"limit":%d}`, limit)
			status, body := rawPost(t, handler, "/mcp", callToolBody(1, "narvi_list_sessions", args), callToolHeaders("narvi_list_sessions"))
			if invoked {
				t.Fatalf("the twin was invoked for limit=%d -- it must be rejected by this package's own bridge first", limit)
			}
			if status != http.StatusOK {
				t.Fatalf("status = %d, body = %s, want 200 (isError:true is a SUCCESSFUL JSON-RPC response)", status, body)
			}
			var env callToolResultEnvelope
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("unmarshal: %v (body: %s)", err, body)
			}
			if env.Result == nil || !env.Result.IsError {
				t.Fatalf("result = %+v, want IsError:true", env.Result)
			}
		})
	}
}

// TestToolCall_AdditionalPropertyRejected proves an unknown argument key
// is rejected by this package's own bridge ("additionalProperties":false
// on every one of the three input $defs), before the twin is ever
// invoked -- the raw Server.AddTool path enforces nothing here either.
func TestToolCall_AdditionalPropertyRejected(t *testing.T) {
	invoked := false
	twins := testTwins()
	twins.ListSessions = func(w http.ResponseWriter, _ *http.Request) {
		invoked = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"sessions":[]}`))
	}
	handler := newTestHandler(t, true, true, twins)

	status, body := rawPost(t, handler, "/mcp", callToolBody(1, "narvi_list_sessions", `{"filter":"all","bogus":1}`), callToolHeaders("narvi_list_sessions"))
	if invoked {
		t.Fatal("the twin was invoked -- an unknown \"bogus\" key must be rejected first (additionalProperties:false)")
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 (isError:true is a SUCCESSFUL JSON-RPC response)", status, body)
	}
	var env callToolResultEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	if env.Result == nil || !env.Result.IsError {
		t.Fatalf("result = %+v, want IsError:true", env.Result)
	}
}

// TestToolCall_TwinPanicIsRecovered proves toolHandler's own recover
// (tools.go) protects the WHOLE call, not merely bridge.go's own request
// construction: a twin that panics outright -- any future twin or
// argument shape, not merely the httptest.NewRequest defect bridge.go's
// own doc comment names -- is caught, logged, and answered a JSON-RPC
// -32603 protocol error. The MCP SDK runs every tool handler in its own
// goroutine with NO recover anywhere in its own call stack (internal/
// jsonrpc2), so an uncaught panic here would otherwise crash the whole
// process. This test's own continued execution past the panicking call
// is itself part of the proof: if the recover did not run, this test
// binary would not still be alive to make the assertions below, let
// alone the follow-up call to a DIFFERENT tool afterward.
func TestToolCall_TwinPanicIsRecovered(t *testing.T) {
	twins := testTwins()
	twins.GetSession = func(http.ResponseWriter, *http.Request) {
		panic("simulated twin panic -- narvi_get_session")
	}
	handler := newTestHandler(t, true, true, twins)

	args := `{"sessionId":"5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a"}`
	_, body := rawPost(t, handler, "/mcp", callToolBody(1, "narvi_get_session", args), callToolHeaders("narvi_get_session"))

	var env callToolResultEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	if env.Error == nil {
		t.Fatalf("error = nil, want a JSON-RPC -32603 protocol error (body: %s)", body)
	}
	if env.Error.Code != -32603 {
		t.Errorf("error.code = %d, want -32603", env.Error.Code)
	}

	// The test binary is still alive: prove it by successfully calling a
	// DIFFERENT tool, in the SAME process, right after the panic.
	status, body2 := rawPost(t, handler, "/mcp", callToolBody(2, "narvi_list_models", "{}"), callToolHeaders("narvi_list_models"))
	if status != http.StatusOK {
		t.Fatalf("follow-up call after the panic: status = %d, body = %s, want 200 (proves the process survived)", status, body2)
	}
}

// TestToolCall_ListSessions_OmittedArgsUseTwinDefaults proves an empty
// arguments object never forces filter/limit onto the twin's own query
// string -- the twin's own defaulting (filter="mine", a default limit)
// runs completely unchanged.
func TestToolCall_ListSessions_OmittedArgsUseTwinDefaults(t *testing.T) {
	var gotQuery string
	twins := testTwins()
	twins.ListSessions = func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"sessions":[]}`))
	}
	handler := newTestHandler(t, true, true, twins)

	status, body := rawPost(t, handler, "/mcp", callToolBody(1, "narvi_list_sessions", `{}`), callToolHeaders("narvi_list_sessions"))
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", status, body)
	}
	if gotQuery != "" {
		t.Errorf("twin's own raw query = %q, want empty (no filter/limit forced by the bridge)", gotQuery)
	}
}
