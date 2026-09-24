package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// passthroughCalled is a tiny sentinel http.Handler recording whether it
// was invoked, so versionGate's own two branches (refuse vs. pass
// through to the SDK) can be told apart without a real SDK handler.
func passthroughHandler(called *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*called = true
		w.WriteHeader(http.StatusTeapot) // an arbitrary, unmistakable sentinel status
	})
}

type versionRefusalBody struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Error   struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Supported []string `json:"supported"`
			Requested string   `json:"requested"`
		} `json:"data"`
	} `json:"error"`
}

// TestVersionGate_UnsupportedHeaderNamesEveryVersion pins technical plan
// §43.4 case 1: a request whose MCP-Protocol-Version header names a
// version this server does not speak is refused HTTP 400, JSON-RPC
// -32022, with every element of SupportedProtocolVersions named in the
// message text and data.supported deep-equal to that same slice.
func TestVersionGate_UnsupportedHeaderNamesEveryVersion(t *testing.T) {
	var called bool
	gate := versionGate(passthroughHandler(&called))

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/list"}`))
	req.Header.Set(protocolVersionHeader, "1900-01-01")
	rec := httptest.NewRecorder()
	gate.ServeHTTP(rec, req)

	if called {
		t.Fatal("versionGate passed an unsupported version through to next")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	var body versionRefusalBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v (body: %s)", err, rec.Body.String())
	}
	if body.Error.Code != codeUnsupportedProtocolVersion {
		t.Errorf("error.code = %d, want %d", body.Error.Code, codeUnsupportedProtocolVersion)
	}
	if !slices.Equal(body.Error.Data.Supported, SupportedProtocolVersions) {
		t.Errorf("error.data.supported = %v, want %v", body.Error.Data.Supported, SupportedProtocolVersions)
	}
	if body.Error.Data.Requested != "1900-01-01" {
		t.Errorf("error.data.requested = %q, want %q", body.Error.Data.Requested, "1900-01-01")
	}
	for _, v := range SupportedProtocolVersions {
		if !strings.Contains(body.Error.Message, v) {
			t.Errorf("error.message = %q does not name version %q", body.Error.Message, v)
		}
	}
	if id, ok := body.ID.(float64); !ok || id != 7 {
		t.Errorf("id = %v, want 7", body.ID)
	}
}

// TestVersionGate_UnparseableBodyEchoesNullID covers the "id: null" case
// when the request body cannot even be parsed enough to find its own id.
func TestVersionGate_UnparseableBodyEchoesNullID(t *testing.T) {
	var called bool
	gate := versionGate(passthroughHandler(&called))

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`not json at all`))
	req.Header.Set(protocolVersionHeader, "1900-01-01")
	rec := httptest.NewRecorder()
	gate.ServeHTTP(rec, req)

	if called {
		t.Fatal("versionGate passed an unsupported version through to next")
	}
	var body versionRefusalBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v (body: %s)", err, rec.Body.String())
	}
	if body.ID != nil {
		t.Errorf("id = %v, want nil (null)", body.ID)
	}
}

// TestVersionGate_SupportedHeaderPassesThrough proves a header naming a
// version this server DOES speak reaches the SDK unchanged.
func TestVersionGate_SupportedHeaderPassesThrough(t *testing.T) {
	for _, v := range SupportedProtocolVersions {
		t.Run(v, func(t *testing.T) {
			var called bool
			gate := versionGate(passthroughHandler(&called))

			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
			req.Header.Set(protocolVersionHeader, v)
			rec := httptest.NewRecorder()
			gate.ServeHTTP(rec, req)

			if !called {
				t.Fatalf("versionGate refused a supported version %q", v)
			}
			if rec.Code != http.StatusTeapot {
				t.Errorf("status = %d, want the passthrough sentinel %d", rec.Code, http.StatusTeapot)
			}
		})
	}
}

// TestVersionGate_AbsentHeaderPassesThrough proves a request with no
// MCP-Protocol-Version header at all -- the shape of a legacy pre-
// 2025-06-18 `initialize` handshake, which carries its own protocol
// version in the BODY instead -- is never rejected by this gate; the SDK's
// own legacy handling is what negotiates or counter-offers for it
// (technical plan §43.4 case 2), pinned separately in handler_test.go.
func TestVersionGate_AbsentHeaderPassesThrough(t *testing.T) {
	var called bool
	gate := versionGate(passthroughHandler(&called))

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	gate.ServeHTTP(rec, req)

	if !called {
		t.Fatal("versionGate refused a request with no version header at all")
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want the passthrough sentinel %d", rec.Code, http.StatusTeapot)
	}
}

// TestSupportedProtocolVersions_ExcludesDeprecatedSSEEra pins §43 D4:
// 2024-11-05 (the deprecated HTTP+SSE transport era) is deliberately not
// in the list.
func TestSupportedProtocolVersions_ExcludesDeprecatedSSEEra(t *testing.T) {
	if slices.Contains(SupportedProtocolVersions, "2024-11-05") {
		t.Errorf("SupportedProtocolVersions = %v, must not include the deprecated 2024-11-05 HTTP+SSE era", SupportedProtocolVersions)
	}
	if len(SupportedProtocolVersions) == 0 {
		t.Fatal("SupportedProtocolVersions is empty")
	}
	if SupportedProtocolVersions[0] != "2026-07-28" {
		t.Errorf("SupportedProtocolVersions[0] = %q, want the current revision 2026-07-28 first", SupportedProtocolVersions[0])
	}
}
