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
// rejectBatches' own full-body read must reinstall EVERY byte it read,
// not just the ones after the first non-space one.
func TestRejectBatches_LeadingWhitespaceSingleCallStillWorks(t *testing.T) {
	handler := newTestHandler(t, true, true, testTwins())
	body := "  \n" + callToolBody(1, "narvi_list_models", "{}")
	status, respBody := rawPost(t, handler, "/mcp", body, callToolHeaders("narvi_list_models"))
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", status, respBody)
	}
}

// TestRejectBatches_WhitespacePrefixLengthIrrelevant is the direct
// regression test for round 3 review findings R1/R2 (both HIGH): a prior
// revision of rejectBatches peeked only the first 64 bytes of the body
// (batchPeekBytes) through a bufio.Reader, and treated a peek that was
// ENTIRELY whitespace as "not a batch" -- forwarding the WHOLE body,
// batch and all, straight to versionGate and the SDK. The SDK's own
// batch decoder skips leading whitespace with NO length bound, so any
// amount of padding at or beyond the look-ahead window defeated the
// look-ahead outright: a body of 64 spaces followed by hundreds of
// legacy tools/call objects (well under the 64 KiB cap, no
// MCP-Protocol-Version header) sailed through as a non-batch and fanned
// out every call concurrently -- the exact N2 amplification this gate
// exists to close. This table proves the fix has NO such boundary: every
// padding length from 0 up to just under the body cap is refused
// identically, with -32600 and ZERO twin invocations, while a
// non-batch single call with the SAME large whitespace prefix still
// works.
func TestRejectBatches_WhitespacePrefixLengthIrrelevant(t *testing.T) {
	batchArray := func(prefix string) string {
		return prefix + "[" + callToolBody(1, "narvi_list_models", "{}") + "," + callToolBody(2, "narvi_list_models", "{}") + "]"
	}

	cases := []struct {
		name   string
		prefix string
	}{
		{"no padding", ""},
		{"63 spaces (just under the old 64-byte window)", strings.Repeat(" ", 63)},
		{"64 spaces (exactly the old window)", strings.Repeat(" ", 64)},
		{"65 spaces (one past the old window)", strings.Repeat(" ", 65)},
		{"1000 spaces", strings.Repeat(" ", 1000)},
		{"32 CRLF pairs", strings.Repeat("\r\n", 32)},
		{"70 tabs", strings.Repeat("\t", 70)},
		{"mixed whitespace", strings.Repeat(" \t\r\n", 200)},
		{"whitespace up to just under the cap", strings.Repeat(" ", MaxRequestBodyBytes-1024)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			invoked := 0
			twins := testTwins()
			twins.ListModels = func(w http.ResponseWriter, _ *http.Request) {
				invoked++
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"providers":[]}`))
			}
			handler := newTestHandler(t, true, true, twins)

			status, body := rawPost(t, handler, "/mcp", batchArray(tc.prefix), nil)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s, want 400 (a batch, however much whitespace precedes it, must always be refused)", status, body)
			}
			if invoked != 0 {
				t.Fatalf("twin invoked %d times, want 0 -- a refused batch must never reach a tool, regardless of leading whitespace length", invoked)
			}

			var env jsonrpcEnvelope
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("unmarshal: %v (body: %s)", err, body)
			}
			if env.Error == nil || env.Error.Code != codeInvalidRequest {
				t.Fatalf("error = %+v, want -32600", env.Error)
			}
		})
	}
}

// TestRejectBatches_LargeWhitespacePrefixSingleCallStillWorks proves the
// fix above does not overcorrect: a legitimate SINGLE call (not an
// array) preceded by a large amount of insignificant whitespace -- well
// past the old 64-byte look-ahead window -- still reaches its twin
// exactly once, with the response the twin returned.
func TestRejectBatches_LargeWhitespacePrefixSingleCallStillWorks(t *testing.T) {
	invoked := 0
	twins := testTwins()
	twins.ListModels = func(w http.ResponseWriter, _ *http.Request) {
		invoked++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"providers":[]}`))
	}
	handler := newTestHandler(t, true, true, twins)

	body := strings.Repeat(" \t\r\n", 500) + callToolBody(1, "narvi_list_models", "{}")
	status, respBody := rawPost(t, handler, "/mcp", body, callToolHeaders("narvi_list_models"))
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", status, respBody)
	}
	if invoked != 1 {
		t.Fatalf("twin invoked %d times, want exactly 1", invoked)
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
