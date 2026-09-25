package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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

// TestOrigin_DNSRebindingRefused pins the fix for round 2 review finding
// N12: net/http's own CrossOriginProtection.Handler exempts a request
// whose Origin equals its own Host header -- checked BEFORE its own
// trusted-origin list is ever consulted -- and exempts
// Sec-Fetch-Site:"same-origin"/"none" outright. A DNS-rebinding request
// (an attacker-controlled hostname resolved to this deployment's own IP)
// has EXACTLY that shape: Host and Origin both name the attacker's own
// hostname, and a same-origin XHR/fetch from that page sends
// Sec-Fetch-Site: same-origin. A prior revision of RequireTrustedOrigin
// (protection.Handler, unmodified) answered 503/401 for such a request
// instead of the 403 handler.go's own doc comment and technical plan
// §43.2 both already claimed unconditionally. RequireTrustedOrigin now
// performs its own explicit comparison against cfg.PublicBaseURL's own
// origin, with no such exemption, so this request -- Host and Origin
// BOTH "attacker.example:1234", never PublicBaseURL's own
// "http://example.test" -- must be refused 403 regardless.
func TestOrigin_DNSRebindingRefused(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Host = "attacker.example:1234"
	req.Header.Set("Origin", "http://attacker.example:1234")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403 (a DNS-rebinding-style Origin -- equal to its own Host, with Sec-Fetch-Site:same-origin -- must never be exempted)", rec.Code, rec.Body.String())
	}
}

// TestNewHandler_InnerCrossOriginProtection_RefusesCrossSite pins the fix
// for round 2 review finding N19: NewHandler's OWN doc comment calls its
// returned handler "Origin-protected" as a second, defense-in-depth
// layer (StreamableHTTPOptions.CrossOriginProtection), independent of
// RequireTrustedOrigin mounted in front of it in every other test in this
// file. None of those other tests can tell that inner layer apart from
// "no protection at all", because RequireTrustedOrigin's own 403 always
// fires first. This test mounts NewHandler's returned handler DIRECTLY,
// deliberately WITHOUT RequireTrustedOrigin in front (mirroring a rig
// that only wires RequireEnabled + auth, which is exactly what this
// package's own mcp_test parity rig, integration_test.go's
// newMCPTestRig, used to do before this fix), so a cross-site Origin
// reaching this handler is refused ONLY if the inner layer itself is
// still there. Setting NewHandler's own StreamableHTTPOptions.
// CrossOriginProtection to nil (go-sdk v1.8.0: nil means "no protection
// applied") would make this exact request succeed instead, failing this
// test.
func TestNewHandler_InnerCrossOriginProtection_RefusesCrossSite(t *testing.T) {
	cfg := Config{PublicBaseURL: testPublicBaseURL}
	mcpHandler, err := NewHandler(cfg, testTwins())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	handler := RequireEnabled(true)(fakeAuth(true, testUser)(mcpHandler))

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403 from NewHandler's OWN inner CrossOriginProtection layer (no RequireTrustedOrigin is mounted in front of it here)", rec.Code, rec.Body.String())
	}
}

// TestOrigin_RunsBeforeEveryOtherGate proves technical plan §43.2/§43.6's
// own gate order is load-bearing, not merely tidier: RequireTrustedOrigin
// is mounted FIRST in the /mcp route group's own chain (newTestHandler),
// so an invalid Origin gets refused 403 REGARDLESS of the flag, auth, or
// protocol-version state of the rest of the request -- the Streamable
// HTTP transport spec's own "MUST respond with HTTP 403 Forbidden" for
// an invalid Origin is unconditional. A prior revision of this package
// left the equivalent check to run LAST, deep inside NewHandler's own
// returned handler (behind RequireEnabled and auth.Middleware in
// controlplane/serve.go's own route group), so each of the three cases
// below used to answer 503/401/-32022 INSTEAD of 403 whenever the
// request also failed that later gate.
func TestOrigin_RunsBeforeEveryOtherGate(t *testing.T) {
	badOrigin := map[string]string{"Origin": "https://evil.example"}

	t.Run("disabled + bad Origin -> still 403, not 503", func(t *testing.T) {
		handler := newTestHandler(t, false, true, testTwins())
		status, body := rawPost(t, handler, "/mcp", `{}`, badOrigin)
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, body = %s, want 403 (the disabled gate must never run first)", status, body)
		}
	})

	t.Run("unauthenticated + bad Origin -> still 403, not 401", func(t *testing.T) {
		handler := newTestHandler(t, true, false, testTwins())
		status, body := rawPost(t, handler, "/mcp", `{}`, badOrigin)
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, body = %s, want 403 (the auth gate must never run first)", status, body)
		}
	})

	t.Run("unsupported protocol version + bad Origin -> still 403, not -32022", func(t *testing.T) {
		handler := newTestHandler(t, true, true, testTwins())
		headers := map[string]string{"Origin": "https://evil.example", protocolVersionHeader: "1900-01-01"}
		status, body := rawPost(t, handler, "/mcp", `{}`, headers)
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, body = %s, want 403 (the version gate must never run first)", status, body)
		}
	})
}

// TestUnsupportedVersion_Is32022ThroughRealHandler pins technical plan
// §43.4 through the REAL NewHandler, not a stub: every TestVersionGate_*
// case in versions_test.go builds versionGate(passthroughHandler(...))
// directly, around a plain 418-teapot sentinel, never through NewHandler
// or newTestHandler -- so a regression that stopped wiring versionGate
// in front of the real SDK handler at all (e.g. handler.go's own `return
// versionGate(sdkHandler), nil` collapsing to `return sdkHandler, nil`)
// would leave every one of those tests green while every REAL request
// through this surface silently lost the gate.
func TestUnsupportedVersion_Is32022ThroughRealHandler(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())

	status, body := rawPost(t, handler, "/mcp",
		`{"jsonrpc":"2.0","id":9,"method":"tools/list"}`,
		map[string]string{protocolVersionHeader: "1900-01-01"})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", status, body)
	}
	var env jsonrpcEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	if env.Error == nil {
		t.Fatalf("error = nil, want a JSON-RPC -32022 protocol error (body: %s)", body)
	}
	if env.Error.Code != -32022 {
		t.Errorf("error.code = %d, want -32022", env.Error.Code)
	}
	for _, v := range SupportedProtocolVersions {
		if !strings.Contains(env.Error.Message, v) {
			t.Errorf("error.message = %q does not name version %q", env.Error.Message, v)
		}
	}
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

// TestMaxRequestBodyBytes_LargerBodyRefused pins technical plan §43.3: a
// request body larger than MaxRequestBodyBytes (round 2 review of PR
// #324, finding N2: deliberately shrunk to 64 KiB, far below
// httpapi.MaxRequestBodyBytes's own 1 MiB -- a tool call's own arguments
// are a handful of small fields, never a file upload) is refused (413),
// not silently accepted at the SDK's own larger DefaultMaxRequestBodyBytes
// (4 MiB) -- which is exactly what removing
// `MaxRequestBodyBytes: MaxRequestBodyBytes` from handler.go's own
// StreamableHTTPOptions would fall back to.
func TestMaxRequestBodyBytes_LargerBodyRefused(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())
	headers := map[string]string{protocolVersionHeader: "2026-07-28", "Mcp-Method": "tools/list"}

	// Comfortably under the cap: accepted.
	t.Run("under the cap is accepted", func(t *testing.T) {
		padding := strings.Repeat("x", 1024)
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"padding":%q}}}`, padding)
		status, respBody := rawPost(t, handler, "/mcp", body, headers)
		if status != http.StatusOK {
			t.Fatalf("status = %d, body = %s, want 200", status, respBody)
		}
	})

	// Comfortably over the cap: refused before the SDK ever parses it as
	// JSON.
	t.Run("over the cap is refused 413", func(t *testing.T) {
		padding := strings.Repeat("x", 2*MaxRequestBodyBytes)
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"padding":%q}}}`, padding)
		if len(body) <= MaxRequestBodyBytes {
			t.Fatalf("test body is %d bytes, want more than MaxRequestBodyBytes (%d)", len(body), MaxRequestBodyBytes)
		}
		status, respBody := rawPost(t, handler, "/mcp", body, headers)
		if status != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, body = %s, want 413", status, respBody)
		}
	})
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
	// Assert the EXACT capabilities map, not merely "has tools, lacks
	// resources": a prior version of this check let "tools" be ANY
	// shape (e.g. {"listChanged":true}, the SDK's own default when
	// serverOptions() forgets to set Capabilities explicitly) and never
	// looked at "logging"/"prompts" at all, so dropping
	// `Capabilities: &sdkmcp.ServerCapabilities{...}` from serverOptions()
	// entirely (handler.go) -- which falls back to the SDK's own
	// default, {"logging":{},"tools":{"listChanged":true}} -- passed this
	// test undetected.
	wantCapabilities := map[string]any{"tools": map[string]any{}}
	if !reflect.DeepEqual(env.Result.Capabilities, wantCapabilities) {
		t.Errorf("capabilities = %#v, want EXACTLY %#v (no listChanged, no logging, no resources, no prompts)", env.Result.Capabilities, wantCapabilities)
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
