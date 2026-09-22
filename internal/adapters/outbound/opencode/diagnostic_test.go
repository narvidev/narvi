package opencode

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// This file tests §7.3 ("a retry decision is not a diagnosis").
// The exit criterion has two halves, and TestBuildProviderFailureDiagnostic_
// NeverLeaksCredentialsOrPromptContent below is written for the second one
// specifically: "a provider failure is traceable from the diagnostic to its
// cause AND the same diagnostic is asserted to contain neither credentials
// nor request content -- a test that checks only the first half is a check
// that passes while verifying nothing about the half that carries the
// risk." TestBuildProviderFailureDiagnostic_ExtractsAllowlistedFields
// covers the first half; the "never leaks" test below is the one that
// matters, and is written to be hard to satisfy by accident: it stuffs a
// distinct, obviously-synthetic secret/prompt sentinel into EVERY field
// the real schema allows that this package deliberately does not retain
// (responseHeaders beyond the one allowlisted key, responseBody, metadata,
// including a nested/array-shaped variant of each), decodes that payload
// through the REAL json.Unmarshal path (never a hand-built Go struct
// literal, which would trivially "prove" nothing about the wire decode),
// builds the diagnostic through the real buildProviderFailureDiagnostic,
// and serializes it through the real diagnosticToWire+json.Marshal path --
// the exact bytes that would reach both of §7.3's own surfaces (the
// session journal, and the correlation-id-scoped operator log via
// sessionactor's own logProviderFailureDiagnostic) -- then asserts none of
// the sentinels survive anywhere in that output.

// fakeSecretSentinels are the obviously-synthetic (never a real credential
// or prompt fragment) strings planted in every field §7.3 requires this
// package to drop. Each is distinct so a failing assertion below names
// EXACTLY which one leaked and from which planted field, not just "some
// sentinel, somewhere".
const (
	sentinelAuthHeader     = "FAKE-SECRET-AUTHORIZATION-HEADER-DO-NOT-LEAK"
	sentinelCookieHeaderA  = "FAKE-SECRET-COOKIE-ARRAY-ELEMENT-A-DO-NOT-LEAK"
	sentinelCookieHeaderB  = "FAKE-SECRET-COOKIE-ARRAY-ELEMENT-B-DO-NOT-LEAK"
	sentinelAPIKeyHeader   = "FAKE-SECRET-X-API-KEY-HEADER-DO-NOT-LEAK"
	sentinelResponseBody   = "FAKE-SECRET-API-KEY-sk-DO-NOT-LEAK-IN-RESPONSE-BODY"
	sentinelPromptInBody   = "FAKE-SECRET-PROMPT-CONTENT-DO-NOT-LEAK: the user's private diff and API token"
	sentinelMetadataTop    = "FAKE-SECRET-METADATA-TOP-LEVEL-DO-NOT-LEAK"
	sentinelMetadataNested = "FAKE-SECRET-METADATA-NESTED-DO-NOT-LEAK"
	sentinelMetadataPrompt = "FAKE-SECRET-METADATA-NESTED-PROMPT-DO-NOT-LEAK"

	// sentinelVisibleRequestID is the ONE value that legitimately SHOULD
	// survive: it lives under an allowlisted header key
	// (requestIDHeaderCandidates, diagnostic.go). Named "visible" rather
	// than "secret" -- a real provider request id is not itself sensitive,
	// which is exactly why §7.3 allows retaining it.
	sentinelVisibleRequestID = "req_visible_public_abc123"
)

// allSecretSentinels is every planted secret/prompt string that must
// never survive into the built diagnostic -- deliberately excludes
// sentinelVisibleRequestID (which must survive) so the "positive" and
// "negative" assertions below can never be satisfied by the same broken
// implementation (e.g. a buggy build that drops EVERYTHING, allowlisted
// value included, would fail the positive half instead of silently
// passing this list by accident).
var allSecretSentinels = []string{
	sentinelAuthHeader,
	sentinelCookieHeaderA,
	sentinelCookieHeaderB,
	sentinelAPIKeyHeader,
	sentinelResponseBody,
	sentinelPromptInBody,
	sentinelMetadataTop,
	sentinelMetadataNested,
	sentinelMetadataPrompt,
}

// maliciousAPIErrorPayload is the raw wire JSON for one openCodeTaggedError
// -- built as a literal JSON string, not a Go struct literal, so decoding
// it below exercises the REAL json.Unmarshal path against every field the
// real schema allows (openCodeTaggedError's own doc comment, types.go),
// including responseBody/metadata, which openCodeErrorData deliberately
// has NO Go field for at all.
const maliciousAPIErrorPayload = `{
	"name": "APIError",
	"data": {
		"message": "The upstream provider rejected this request.",
		"statusCode": 429,
		"isRetryable": true,
		"responseHeaders": {
			"x-request-id": "req_visible_public_abc123",
			"authorization": "Bearer FAKE-SECRET-AUTHORIZATION-HEADER-DO-NOT-LEAK",
			"set-cookie": ["FAKE-SECRET-COOKIE-ARRAY-ELEMENT-A-DO-NOT-LEAK", "FAKE-SECRET-COOKIE-ARRAY-ELEMENT-B-DO-NOT-LEAK"],
			"x-api-key": "FAKE-SECRET-X-API-KEY-HEADER-DO-NOT-LEAK"
		},
		"responseBody": "{\"error\":{\"message\":\"invalid api key FAKE-SECRET-API-KEY-sk-DO-NOT-LEAK-IN-RESPONSE-BODY for prompt: 'FAKE-SECRET-PROMPT-CONTENT-DO-NOT-LEAK: the user's private diff and API token'\"}}",
		"metadata": {
			"internalTrace": "FAKE-SECRET-METADATA-TOP-LEVEL-DO-NOT-LEAK",
			"nested": {
				"apiKey": "FAKE-SECRET-METADATA-NESTED-DO-NOT-LEAK",
				"prompt": "FAKE-SECRET-METADATA-NESTED-PROMPT-DO-NOT-LEAK"
			}
		}
	}
}`

// TestBuildProviderFailureDiagnostic_NeverLeaksCredentialsOrPromptContent
// is the exit criterion's own second half: constructs a payload carrying
// credential/prompt-shaped content in every field the real schema allows
// that this package does not retain, runs the REAL retention path
// (json.Unmarshal -> buildProviderFailureDiagnostic -> diagnosticToWire ->
// json.Marshal), and asserts none of it survives anywhere in the result --
// not the Go struct, not the serialized bytes that would reach the session
// journal and the operator log alike.
func TestBuildProviderFailureDiagnostic_NeverLeaksCredentialsOrPromptContent(t *testing.T) {
	t.Parallel()

	var tagged openCodeTaggedError
	if err := json.Unmarshal([]byte(maliciousAPIErrorPayload), &tagged); err != nil {
		t.Fatalf("json.Unmarshal(maliciousAPIErrorPayload): %v", err)
	}

	a := &Adapter{runtimeVersion: testRuntimeVersion, sandboxID: testSandboxID}
	ts := &turnState{model: "anthropic/claude-sonnet-4-5"}

	d := a.buildProviderFailureDiagnostic(&tagged, ts)
	if d == nil {
		t.Fatal("buildProviderFailureDiagnostic returned nil for a non-nil tagged error")
	}

	// Struct-level check first: every field is a string or *int (see
	// ProviderFailureDiagnostic's own doc comment) -- there is no field a
	// whole map/slice could ever be assigned to, so this loop, run
	// against the Go struct directly, is really checking the STRING
	// fields specifically (the ones capDiagnosticField/extractProviderRequestID
	// actually populate from untrusted input).
	structFields := map[string]string{
		"Message":           d.Message,
		"UnionMember":       d.UnionMember,
		"ProviderRequestID": d.ProviderRequestID,
		"Model":             d.Model,
		"RuntimeVersion":    d.RuntimeVersion,
		"SandboxID":         d.SandboxID,
	}
	for field, value := range structFields {
		for _, sentinel := range allSecretSentinels {
			if strings.Contains(value, sentinel) {
				t.Errorf("ProviderFailureDiagnostic.%s leaked sentinel %q: %q", field, sentinel, value)
			}
		}
	}

	// Wire-level check: the exact bytes translateExecutionComplete would
	// send over the wire, persist verbatim into the session journal
	// (appendRawEvent), and that sessionactor's own
	// logProviderFailureDiagnostic would decode back out of that SAME
	// persisted payload for the operator log -- one serialization, both
	// surfaces (§7.3's own "one record" requirement), so checking this
	// once covers both.
	wire := diagnosticToWire(d)
	wireJSON, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("json.Marshal(diagnosticToWire(d)): %v", err)
	}
	for _, sentinel := range allSecretSentinels {
		if strings.Contains(string(wireJSON), sentinel) {
			t.Errorf("wire diagnostic JSON leaked sentinel %q: %s", sentinel, wireJSON)
		}
	}

	// Belt-and-suspenders: the raw malicious payload's own credential/
	// prompt fields must never even be REACHABLE from d -- confirms
	// openCodeErrorData genuinely has no responseBody/metadata field for
	// json.Unmarshal to have populated in the first place (a structural
	// guarantee, not merely "and then we didn't copy it").
	if tagged.Data != nil {
		v := interfaceViaJSON(t, tagged.Data)
		if _, ok := v["responseBody"]; ok {
			t.Error("openCodeErrorData decoded a responseBody field -- the allowlist was widened without updating this test's own expectations")
		}
		if _, ok := v["metadata"]; ok {
			t.Error("openCodeErrorData decoded a metadata field -- the allowlist was widened without updating this test's own expectations")
		}
	}
}

// interfaceViaJSON re-marshals v (a struct) and decodes it back into a
// map[string]any -- used above purely to inspect WHICH JSON keys v's own
// type actually round-trips, the cheapest way to prove a field is
// structurally absent rather than merely zero-valued.
func interfaceViaJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	return m
}

// TestBuildProviderFailureDiagnostic_ExtractsAllowlistedFields is the exit
// criterion's own first half: a provider failure must be traceable from
// the diagnostic to its cause. Uses the SAME malicious/realistic payload
// as the "never leaks" test above -- proving the two properties hold
// SIMULTANEOUSLY against one payload, not against two different, each
// conveniently shaped to make one half trivially true.
func TestBuildProviderFailureDiagnostic_ExtractsAllowlistedFields(t *testing.T) {
	t.Parallel()

	var tagged openCodeTaggedError
	if err := json.Unmarshal([]byte(maliciousAPIErrorPayload), &tagged); err != nil {
		t.Fatalf("json.Unmarshal(maliciousAPIErrorPayload): %v", err)
	}

	a := &Adapter{runtimeVersion: testRuntimeVersion, sandboxID: testSandboxID}
	ts := &turnState{model: "anthropic/claude-sonnet-4-5"}

	d := a.buildProviderFailureDiagnostic(&tagged, ts)
	if d == nil {
		t.Fatal("buildProviderFailureDiagnostic returned nil for a non-nil tagged error")
	}

	if want := "The upstream provider rejected this request."; d.Message != want {
		t.Errorf("Message = %q, want %q", d.Message, want)
	}
	if want := "APIError"; d.UnionMember != want {
		t.Errorf("UnionMember = %q, want %q", d.UnionMember, want)
	}
	if d.StatusCode == nil || *d.StatusCode != 429 {
		t.Errorf("StatusCode = %v, want 429", d.StatusCode)
	}
	if d.ProviderRequestID != sentinelVisibleRequestID {
		t.Errorf("ProviderRequestID = %q, want %q (the one allowlisted header)", d.ProviderRequestID, sentinelVisibleRequestID)
	}
	if want := "anthropic/claude-sonnet-4-5"; d.Model != want {
		t.Errorf("Model = %q, want %q", d.Model, want)
	}
	if d.RuntimeVersion != testRuntimeVersion {
		t.Errorf("RuntimeVersion = %q, want %q", d.RuntimeVersion, testRuntimeVersion)
	}
	if d.SandboxID != testSandboxID {
		t.Errorf("SandboxID = %q, want %q", d.SandboxID, testSandboxID)
	}
}

// TestBuildProviderFailureDiagnostic_NilErrReturnsNil covers the zero
// value of buildProviderFailureDiagnostic's own primary input: no tagged
// error was ever observed (deriveOutcome's own "turn produced no output"
// path, or an adapter-local transport failure that never reached
// OpenCode). A diagnostic describes a PROVIDER failure; there is none to
// describe.
func TestBuildProviderFailureDiagnostic_NilErrReturnsNil(t *testing.T) {
	t.Parallel()

	a := &Adapter{runtimeVersion: testRuntimeVersion, sandboxID: testSandboxID}
	ts := &turnState{model: "anthropic/claude-sonnet-4-5"}

	if d := a.buildProviderFailureDiagnostic(nil, ts); d != nil {
		t.Errorf("buildProviderFailureDiagnostic(nil, ts) = %+v, want nil", d)
	}
}

// TestBuildProviderFailureDiagnostic_NilDataOmitsPayloadFields covers the
// zero value of err.Data: a tagged-union member whose own real payload
// carries no "data" object at all (openCodeTaggedError's own doc comment:
// "data" is optional even when present, types.go) -- Message/StatusCode/
// ProviderRequestID must all read as absent, never a zero-value that
// LOOKS like a real (if empty) provider answer, while the adapter-side
// fields (Model/RuntimeVersion/SandboxID) still populate normally, since
// they never depended on Data at all.
func TestBuildProviderFailureDiagnostic_NilDataOmitsPayloadFields(t *testing.T) {
	t.Parallel()

	a := &Adapter{runtimeVersion: testRuntimeVersion, sandboxID: testSandboxID}
	ts := &turnState{model: "anthropic/claude-sonnet-4-5"}

	d := a.buildProviderFailureDiagnostic(&openCodeTaggedError{Name: "UnknownError"}, ts)
	if d == nil {
		t.Fatal("buildProviderFailureDiagnostic returned nil for a non-nil tagged error")
	}
	if d.Message != "" {
		t.Errorf("Message = %q, want \"\" (no Data at all)", d.Message)
	}
	if d.StatusCode != nil {
		t.Errorf("StatusCode = %v, want nil (no Data at all)", d.StatusCode)
	}
	if d.ProviderRequestID != "" {
		t.Errorf("ProviderRequestID = %q, want \"\" (no Data at all)", d.ProviderRequestID)
	}
	if d.UnionMember != "UnknownError" {
		t.Errorf("UnionMember = %q, want %q", d.UnionMember, "UnknownError")
	}
	if d.Model != "anthropic/claude-sonnet-4-5" || d.RuntimeVersion != testRuntimeVersion || d.SandboxID != testSandboxID {
		t.Errorf("adapter-side fields not populated: Model=%q RuntimeVersion=%q SandboxID=%q", d.Model, d.RuntimeVersion, d.SandboxID)
	}
}

// TestCapDiagnosticField covers diagnosticFieldMaxBytes/capDiagnosticField
// directly: a field within the cap survives unchanged (the common,
// zero-truncation case -- easy to get "backwards" by always appending the
// marker); a field over the cap is truncated AND visibly marked, never
// silently shortened; and truncation never lands mid-rune, which a naive
// byte-offset slice would risk for any non-ASCII content.
func TestCapDiagnosticField(t *testing.T) {
	t.Parallel()

	t.Run("under the cap is returned unchanged", func(t *testing.T) {
		t.Parallel()
		short := "a short, ordinary provider message"
		if got := capDiagnosticField(short); got != short {
			t.Errorf("capDiagnosticField(%q) = %q, want unchanged", short, got)
		}
	})

	t.Run("over the cap is truncated and marked", func(t *testing.T) {
		t.Parallel()
		long := strings.Repeat("x", diagnosticFieldMaxBytes*2)
		got := capDiagnosticField(long)
		if len(got) > diagnosticFieldMaxBytes {
			t.Errorf("capDiagnosticField result is %d bytes, want <= %d", len(got), diagnosticFieldMaxBytes)
		}
		if !strings.HasSuffix(got, diagnosticTruncationMarker) {
			t.Errorf("capDiagnosticField result does not end with the truncation marker %q: %q", diagnosticTruncationMarker, got)
		}
	})

	t.Run("truncation never splits a multi-byte rune", func(t *testing.T) {
		t.Parallel()
		// A repeated 3-byte UTF-8 rune (€, U+20AC) sized so the cap lands
		// mid-character if truncation is done by naive byte slicing.
		long := strings.Repeat("€", diagnosticFieldMaxBytes)
		got := capDiagnosticField(long)
		if !utf8.ValidString(got) {
			t.Errorf("capDiagnosticField produced invalid UTF-8: %q", got)
		}
	})
}
