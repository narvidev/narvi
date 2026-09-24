package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestRejectBatches_LegacyBatchRefused pins the fix for round 2 review
// finding N2: a legacy JSON-RPC batch (a top-level JSON array) is refused
// with a single -32600 "Invalid Request" error, HTTP 400, before it ever
// reaches versionGate or the SDK -- no tool runs, regardless of how many
// calls the array holds or which tools they name.
func TestRejectBatches_LegacyBatchRefused(t *testing.T) {
	invoked := 0
	twins := testTwins()
	twins.ListModels = func(w http.ResponseWriter, _ *http.Request) {
		invoked++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"providers":[]}`))
	}
	handler := newTestHandler(t, true, true, twins)

	batch := `[` + callToolBody(1, "narvi_list_models", "{}") + `,` + callToolBody(2, "narvi_list_models", "{}") + `]`
	status, body := rawPost(t, handler, "/mcp", batch, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", status, body)
	}
	if invoked != 0 {
		t.Fatalf("twin invoked %d times, want 0 -- a refused batch must never reach a tool", invoked)
	}

	var env jsonrpcEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	if env.Error == nil {
		t.Fatalf("error = nil, want a JSON-RPC -32600 protocol error (body: %s)", body)
	}
	if env.Error.Code != codeInvalidRequest {
		t.Errorf("error.code = %d, want %d", env.Error.Code, codeInvalidRequest)
	}
	if !strings.Contains(env.Error.Message, "batching") {
		t.Errorf("error.message = %q, want it to mention batching", env.Error.Message)
	}
	if env.ID != nil {
		t.Errorf("id = %v, want nil (null) -- a batch refusal names no single call's id", env.ID)
	}
}

// TestRejectBatches_LeadingWhitespaceStillDetected proves the batch
// detector skips JSON's own insignificant leading whitespace before
// looking for the array's '[' -- a batch prefixed with spaces/newlines is
// still refused, not accidentally forwarded to the SDK.
func TestRejectBatches_LeadingWhitespaceStillDetected(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())
	batch := "  \n\t [" + callToolBody(1, "narvi_list_models", "{}") + "]"
	status, body := rawPost(t, handler, "/mcp", batch, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", status, body)
	}
	var env jsonrpcEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	if env.Error == nil || env.Error.Code != codeInvalidRequest {
		t.Fatalf("error = %+v, want -32600", env.Error)
	}
}

// TestRejectBatches_SingleCallStillWorks proves rejectBatches forwards
// (unconsumed, byte-for-byte) any request whose body is NOT an array --
// the ordinary single-call shape every other test in this package sends
// -- so this gate never breaks a legitimate request.
func TestRejectBatches_SingleCallStillWorks(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())
	status, body := rawPost(t, handler, "/mcp", callToolBody(1, "narvi_list_models", "{}"), callToolHeaders("narvi_list_models"))
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", status, body)
	}
}

// TestRejectBatches_LeadingWhitespaceSingleCallStillWorks proves a
// legitimate single call prefixed with insignificant whitespace (legal
// JSON, and not a batch) is still forwarded correctly, byte for byte --
// peekFirstNonSpaceByte's own bufio.Reader must re-serve every peeked
// byte to the SDK, not just the ones after the first non-space one.
func TestRejectBatches_LeadingWhitespaceSingleCallStillWorks(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())
	body := "  \n" + callToolBody(1, "narvi_list_models", "{}")
	status, respBody := rawPost(t, handler, "/mcp", body, callToolHeaders("narvi_list_models"))
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", status, respBody)
	}
}

// TestMaxRequestBodyBytes_BoundsHugeIntegerLiteralCost proves the
// shrunk-to-64-KiB body cap (versions.go) closes finding N7's CPU
// amplification as a side effect: a legacy (no version header) tools/call
// naming a limit long enough to no longer fit under the cap is refused
// 413 -- fast -- well before santhosh-tekuri/jsonschema's own
// big.Rat-based integer check (util.go isInteger, validator.go
// numValidate) ever runs against it. A prior 1 MiB cap let a
// ~1,000,000-digit literal reach that check, costing roughly a second and
// a half of CPU per request.
func TestMaxRequestBodyBytes_BoundsHugeIntegerLiteralCost(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())
	digits := strings.Repeat("7", MaxRequestBodyBytes)
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"narvi_list_sessions","arguments":{"limit":%s}}}`, digits)

	start := time.Now()
	status, respBody := rawPost(t, handler, "/mcp", body, nil)
	elapsed := time.Since(start)

	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s, want 413", status, respBody)
	}
	if elapsed > time.Second {
		t.Errorf("refusing an oversized body took %s, want well under a second -- the whole point of the cap is to refuse BEFORE any expensive parsing runs", elapsed)
	}
}

// TestSupportedProtocolVersions_ExcludesBatchingEra pins the other half
// of finding N2's fix (§43 D4 revised): 2025-03-26 -- the one revision
// whose own spec text requires a server to accept a JSON-RPC batch -- is
// no longer in SupportedProtocolVersions, and a request naming it is
// refused exactly like any other unsupported version, with a message
// naming every version this server actually speaks.
func TestSupportedProtocolVersions_ExcludesBatchingEra(t *testing.T) {
	for _, v := range SupportedProtocolVersions {
		if v == "2025-03-26" {
			t.Fatalf("SupportedProtocolVersions = %v, must not include 2025-03-26 (batching is refused structurally; claiming a revision that requires it would not be conformant)", SupportedProtocolVersions)
		}
	}

	handler := newTestHandler(t, true, true, testTwins())
	status, body := rawPost(t, handler, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		map[string]string{protocolVersionHeader: "2025-03-26"})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", status, body)
	}
	var env jsonrpcEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, body)
	}
	if env.Error == nil || env.Error.Code != -32022 {
		t.Fatalf("error = %+v, want -32022", env.Error)
	}
	for _, v := range SupportedProtocolVersions {
		if !strings.Contains(env.Error.Message, v) {
			t.Errorf("error.message = %q does not name version %q", env.Error.Message, v)
		}
	}
}
