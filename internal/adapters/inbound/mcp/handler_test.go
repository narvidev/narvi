package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func testTwins() Twins {
	return Twins{
		ListModels:   stubHandler(http.StatusOK, `{"providers":[]}`),
		ListSessions: stubHandler(http.StatusOK, `{"sessions":[]}`),
		GetSession:   stubHandler(http.StatusOK, `{"id":"x"}`),
	}
}

// rawPost drives handler directly via ServeHTTP (httptest.NewRequest/
// NewRecorder -- no real listening socket, no net/http client: see
// newTestHandler's own doc comment for why) with the given headers plus
// the Accept/Content-Type headers every Streamable HTTP request needs
// regardless of era, returning the raw status/body.
func rawPost(t testing.TB, handler http.Handler, path, body string, headers map[string]string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

type jsonrpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
}

// TestDisabled_503BeforeAuth pins technical plan §43.11 / D9: the
// disabled gate answers 503 BEFORE the auth gate ever runs (no cookie,
// flag off -> 503, never 401); flag on, no cookie -> the ordinary 401.
func TestDisabled_503BeforeAuth(t *testing.T) {
	t.Run("disabled, unauthenticated -> 503 with the capability body", func(t *testing.T) {
		handler := newTestHandler(t, false, false, testTwins())
		status, body := rawPost(t, handler, "/mcp", `{}`, nil)
		if status != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", status)
		}
		if string(body) != disabledBody {
			t.Errorf("body = %s, want %s", body, disabledBody)
		}
	})

	t.Run("enabled, unauthenticated -> 401 unauthorized", func(t *testing.T) {
		handler := newTestHandler(t, true, false, testTwins())
		status, body := rawPost(t, handler, "/mcp", `{}`, nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", status)
		}
		if string(body) != `{"error":"unauthorized"}` {
			t.Errorf("body = %s, want the generic unauthorized body", body)
		}
	})
}

// TestOrigin_CrossSiteRefused_TrustedAndAbsentPass pins technical plan
// §43.2: a cross-site browser Origin is refused 403; the trusted
// origin (PublicBaseURL's own origin, "http://example.test" -- see
// newTestHandler) passes; no Origin header at all (every non-browser
// client) passes.
func TestOrigin_CrossSiteRefused_TrustedAndAbsentPass(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())

	t.Run("cross-site Origin refused 403", func(t *testing.T) {
		status, _ := rawPost(t, handler, "/mcp", `{}`, map[string]string{"Origin": "https://evil.example"})
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", status)
		}
	})

	t.Run("trusted origin passes", func(t *testing.T) {
		status, _ := rawPost(t, handler, "/mcp", `{}`, map[string]string{"Origin": "http://example.test"})
		if status == http.StatusForbidden {
			t.Fatalf("status = %d, trusted origin was refused", status)
		}
	})

	t.Run("no Origin header passes", func(t *testing.T) {
		status, _ := rawPost(t, handler, "/mcp", `{}`, nil)
		if status == http.StatusForbidden {
			t.Fatalf("status = %d, absent Origin was refused", status)
		}
	})
}

// TestMethodNotAllowed_GetAndDelete pins technical plan §43.3:
// Streamable HTTP in stateless mode is POST-only.
func TestMethodNotAllowed_GetAndDelete(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/mcp", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405", rec.Code)
			}
		})
	}
}

// legacyInitializeBody builds a pre-2025-06-18-shaped `initialize` call
// naming protocolVersion -- no MCP-Protocol-Version header, no `_meta`
// triple: exactly the shape a legacy client sends before it knows what
// the server speaks.
func legacyInitializeBody(id int, protocolVersion string) string {
	return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"initialize","params":{"protocolVersion":"` + protocolVersion + `","capabilities":{},"clientInfo":{"name":"test-client","version":"0"}}}`
}

// TestLegacyInitialize_UnsupportedVersionIsCounterOffered pins technical
// plan §43.4 case 2: a legacy `initialize` naming an unsupported
// version (2024-11-05) is answered with a version the server DOES speak
// (never the requested one) -- a counter-offer, not an error.
func TestLegacyInitialize_UnsupportedVersionIsCounterOffered(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())
	status, body := rawPost(t, handler, "/mcp", legacyInitializeBody(1, "2024-11-05"), nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", status, body)
	}
	var env jsonrpcEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	if env.Error != nil {
		t.Fatalf("error = %+v, want a successful counter-offer result", env.Error)
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v (result: %s)", err, env.Result)
	}
	if result.ProtocolVersion == "2024-11-05" {
		t.Errorf("result.protocolVersion = %q, must never echo the unsupported requested version", result.ProtocolVersion)
	}
	found := false
	for _, v := range SupportedProtocolVersions {
		if v == result.ProtocolVersion {
			found = true
		}
	}
	if !found {
		t.Errorf("result.protocolVersion = %q, want one of %v", result.ProtocolVersion, SupportedProtocolVersions)
	}
}

// TestLegacyInitialize_SupportedVersionIsEchoed pins technical plan
// §43.4 case 2's own positive case, and (via the follow-up call) §43.12
// test 4: a following tools/list with the negotiated MCP-Protocol-Version
// header works, with no Mcp-Session-Id needed at all (stateless).
func TestLegacyInitialize_SupportedVersionIsEchoed(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())
	status, body := rawPost(t, handler, "/mcp", legacyInitializeBody(1, "2025-06-18"), nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", status, body)
	}
	var env jsonrpcEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v (result: %s)", err, env.Result)
	}
	if result.ProtocolVersion != "2025-06-18" {
		t.Fatalf("result.protocolVersion = %q, want the exact requested (and supported) version echoed back", result.ProtocolVersion)
	}

	status, body = rawPost(t, handler, "/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		map[string]string{protocolVersionHeader: "2025-06-18"})
	if status != http.StatusOK {
		t.Fatalf("follow-up tools/list: status = %d, body = %s, want 200", status, body)
	}
	var env2 jsonrpcEnvelope
	if err := json.Unmarshal(body, &env2); err != nil {
		t.Fatalf("unmarshal follow-up: %v (body: %s)", err, body)
	}
	if env2.Error != nil {
		t.Fatalf("follow-up tools/list error = %+v, want success", env2.Error)
	}
}

// TestHeaderBodyMismatch_Is32020 pins the SDK's own header/body agreement
// check for a modern (>= 2026-07-28) request: Mcp-Method must match the
// JSON-RPC method.
func TestHeaderBodyMismatch_Is32020(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	status, respBody := rawPost(t, handler, "/mcp", body, map[string]string{
		protocolVersionHeader: "2026-07-28",
		"Mcp-Method":          "tools/call", // deliberately WRONG
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", status, respBody)
	}
	var env jsonrpcEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, respBody)
	}
	if env.Error == nil {
		t.Fatal("error = nil, want a header-mismatch error")
	}
	if env.Error.Code != -32020 {
		t.Errorf("error.code = %d, want -32020", env.Error.Code)
	}
}

// TestMissingMeta_Is32602 pins the SDK's own required-`_meta`-field check
// for a modern request: clientCapabilities is required alongside
// protocolVersion.
func TestMissingMeta_Is32602(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`
	status, respBody := rawPost(t, handler, "/mcp", body, map[string]string{
		protocolVersionHeader: "2026-07-28",
		"Mcp-Method":          "tools/list",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", status, respBody)
	}
	var env jsonrpcEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, respBody)
	}
	if env.Error == nil {
		t.Fatal("error = nil, want a missing-_meta error")
	}
	if env.Error.Code != -32602 {
		t.Errorf("error.code = %d, want -32602", env.Error.Code)
	}
}

// TestServerDiscover_AdvertisesConstant pins technical plan §43.5:
// server/discover's own supportedVersions is EXACTLY
// SupportedProtocolVersions, and _meta carries this build's own
// serverInfo.
func TestServerDiscover_AdvertisesConstant(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())
	body := `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	status, respBody := rawPost(t, handler, "/mcp", body, map[string]string{
		protocolVersionHeader: "2026-07-28",
		"Mcp-Method":          "server/discover",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", status, respBody)
	}
	var env struct {
		Result struct {
			SupportedVersions []string       `json:"supportedVersions"`
			Capabilities      map[string]any `json:"capabilities"`
			Instructions      string         `json:"instructions"`
			Meta              map[string]any `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, respBody)
	}
	if len(env.Result.SupportedVersions) != len(SupportedProtocolVersions) {
		t.Fatalf("supportedVersions = %v, want %v", env.Result.SupportedVersions, SupportedProtocolVersions)
	}
	for i, v := range SupportedProtocolVersions {
		if env.Result.SupportedVersions[i] != v {
			t.Errorf("supportedVersions[%d] = %q, want %q", i, env.Result.SupportedVersions[i], v)
		}
	}
	if _, ok := env.Result.Capabilities["tools"]; !ok {
		t.Errorf("capabilities = %v, want a \"tools\" key", env.Result.Capabilities)
	}
	if _, ok := env.Result.Capabilities["resources"]; ok {
		t.Errorf("capabilities = %v, must not advertise resources", env.Result.Capabilities)
	}
	if !strings.Contains(strings.ToLower(env.Result.Instructions), "read-only") {
		t.Errorf("instructions = %q, want it to say the tools are read-only", env.Result.Instructions)
	}
	serverInfo, _ := env.Result.Meta["io.modelcontextprotocol/serverInfo"].(map[string]any)
	if serverInfo["name"] != "narvi" {
		t.Errorf("result._meta.serverInfo.name = %v, want \"narvi\"", serverInfo["name"])
	}
	if serverInfo["version"] == "" || serverInfo["version"] == nil {
		t.Errorf("result._meta.serverInfo.version = %v, want contracts.Version", serverInfo["version"])
	}
}
