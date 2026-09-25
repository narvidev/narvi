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

// TestVersionGate_FutureVersionRefusedWithMessage pins the case round 2
// review finding N16 found untested: every OTHER refusal test in this
// package uses a header OLDER than the current revision ("1900-01-01"),
// which happens to work whether versionGate compares by SET MEMBERSHIP
// (isSupportedVersion, the real check) or by some other rule entirely, so
// none of them can tell those two apart. A NEWER-than-current header
// (here, "2099-01-01" -- the likeliest real-world mismatch, a client
// built against a future revision this deployment does not speak yet)
// must be refused exactly the same way: -32022, HTTP 400, a message
// naming every version this server actually speaks. Without this test, a
// mutant that only refused OLDER-than-current versions (e.g. comparing
// against SupportedProtocolVersions[0] instead of set membership) would
// let a future version fall through to the SDK's own -32022, whose fixed
// message names none of them -- the exact gap the exit criterion
// ("a message naming the versions it does") exists to close.
func TestVersionGate_FutureVersionRefusedWithMessage(t *testing.T) {
	var called bool
	gate := versionGate(passthroughHandler(&called))

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`))
	req.Header.Set(protocolVersionHeader, "2099-01-01")
	rec := httptest.NewRecorder()
	gate.ServeHTTP(rec, req)

	if called {
		t.Fatal("versionGate passed a future, unsupported version through to next")
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
	if body.Error.Data.Requested != "2099-01-01" {
		t.Errorf("error.data.requested = %q, want %q", body.Error.Data.Requested, "2099-01-01")
	}
	if !slices.Equal(body.Error.Data.Supported, SupportedProtocolVersions) {
		t.Errorf("error.data.supported = %v, want %v", body.Error.Data.Supported, SupportedProtocolVersions)
	}
	for _, v := range SupportedProtocolVersions {
		if !strings.Contains(body.Error.Message, v) {
			t.Errorf("error.message = %q does not name version %q", body.Error.Message, v)
		}
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

// TestMaxRequestBodyBytes_PinnedAt64KiB pins round 3 review finding R6:
// both TestMaxRequestBodyBytes_BoundsHugeIntegerLiteralCost (batch_test.go)
// and TestMaxRequestBodyBytes_LargerBodyRefused (handler_test.go) size
// their OWN payload FROM the MaxRequestBodyBytes constant itself, so
// raising it -- even all the way back to round 2's own original 1 MiB --
// passes both of those tests completely unchanged: the exact 64 KiB bound
// finding N7's cost mitigation and the batch-fan-out cap both rely on is
// never itself pinned anywhere. This test closes that on two independent,
// constant-value-INDEPENDENT axes: the numeric value of the constant
// itself (spelled here as a literal, "64 << 10", never by reference to
// versions.go's own "64 * 1024"), and a request body built to a FIXED
// byte count -- one byte over 64 KiB, not derived from
// MaxRequestBodyBytes+1 -- which must still be refused even if some
// future change silently raised the constant.
func TestMaxRequestBodyBytes_PinnedAt64KiB(t *testing.T) {
	const want64KiB = 64 << 10
	if MaxRequestBodyBytes != want64KiB {
		t.Fatalf("MaxRequestBodyBytes = %d, want %d (64 KiB) -- round 2's own N7/N2 mitigations both depend on this EXACT bound, not merely on whatever value each derived test happens to size itself against", MaxRequestBodyBytes, want64KiB)
	}

	handler := newTestHandler(t, true, true, testTwins())
	headers := map[string]string{protocolVersionHeader: "2026-07-28", "Mcp-Method": "tools/list"}

	const totalWant = 64*1024 + 1 // one byte over the cap, a FIXED absolute size
	prefix := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"padding":"`
	suffix := `"}}}`
	padLen := totalWant - len(prefix) - len(suffix)
	if padLen <= 0 {
		t.Fatalf("prefix+suffix alone is already %d bytes, want less than %d", len(prefix)+len(suffix), totalWant)
	}
	body := prefix + strings.Repeat("x", padLen) + suffix
	if len(body) != totalWant {
		t.Fatalf("test body is %d bytes, want exactly %d", len(body), totalWant)
	}

	status, respBody := rawPost(t, handler, "/mcp", body, headers)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s, want 413 for a fixed %d-byte body (64 KiB + 1)", status, respBody, totalWant)
	}
}
