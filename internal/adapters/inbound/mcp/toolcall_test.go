package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
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

// TestToolCall_GetSession_MalformedIDIsProtocolError proves a malformed
// sessionId reaches the REAL twin (this package's own input schema
// deliberately does not enforce format:"uuid" -- schemas.go's own doc
// comment) and the twin's own 400 becomes a JSON-RPC PROTOCOL error
// (-32602), not isError:true.
func TestToolCall_GetSession_MalformedIDIsProtocolError(t *testing.T) {
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
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400 (a protocol-level JSON-RPC error)", status, body)
	}
	if gotPath == "" {
		t.Fatal("the twin was never invoked -- \"not-a-uuid\" should reach it, since format:\"uuid\" is not enforced by the pinned SDK's own schema library")
	}
	var env callToolResultEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	if env.Error == nil {
		t.Fatal("error = nil, want a JSON-RPC -32602 protocol error")
	}
	if env.Error.Code != -32602 {
		t.Errorf("error.code = %d, want -32602", env.Error.Code)
	}
	if env.Error.Message != "malformed session id" {
		t.Errorf("error.message = %q, want the twin's own exact text %q", env.Error.Message, "malformed session id")
	}
}

// TestToolCall_ListSessions_BadFilterReachesTwin proves filter:"x" (a
// value the schema deliberately does not "enum"-restrict) reaches the
// real twin and its own 400 becomes -32602 with the REST route's exact
// text -- never the SDK's own generic schema-validation message.
func TestToolCall_ListSessions_BadFilterReachesTwin(t *testing.T) {
	var gotFilter string
	twins := testTwins()
	twins.ListSessions = func(w http.ResponseWriter, r *http.Request) {
		gotFilter = r.URL.Query().Get("filter")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"filter must be \"mine\" or \"all\""}`))
	}
	handler := newTestHandler(t, true, true, twins)

	status, body := rawPost(t, handler, "/mcp", callToolBody(1, "narvi_list_sessions", `{"filter":"x"}`), callToolHeaders("narvi_list_sessions"))
	if gotFilter != "x" {
		t.Fatalf("twin's own query filter = %q, want %q (the tool must not intercept this value itself)", gotFilter, "x")
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", status, body)
	}
	var env callToolResultEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	if env.Error == nil || env.Error.Code != -32602 {
		t.Fatalf("error = %+v, want -32602", env.Error)
	}
	if env.Error.Message != `filter must be "mine" or "all"` {
		t.Errorf("error.message = %q, want the REST route's own exact text", env.Error.Message)
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
