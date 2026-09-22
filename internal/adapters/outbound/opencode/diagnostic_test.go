package opencode

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
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
	model := "anthropic/claude-sonnet-4-5"

	d := a.buildProviderFailureDiagnostic(&tagged, model)
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

// hostileProviderMessage is planted as err.Data.Message below -- unlike
// maliciousAPIErrorPayload's own sentinels (which test the ALLOWLIST:
// fields this package must never retain at all), Message is retained
// VERBATIM by design (§7.3 names it explicitly, and ProviderFailureDiagnostic's
// own doc comment: "the human-readable message... size-capped. Never the
// raw provider response body"). This is a deliberate, audit-fix decision
// (C6): a provider is untrusted, and nothing upstream of this package
// sanitizes its own error message text -- an adversarial or compromised
// provider (or an ordinary provider that happens to echo request content
// back in an error, a documented real-world API failure mode) could
// return a script-injection-shaped string or a credential-shaped string
// as its own "message". Retaining it is still the right call (§7.3's own
// "less diagnostic information... is the direction this must fail in"),
// but the CONTAINMENT this relies on -- that Message reaches only
// JSON-serialized surfaces, never raw markup -- must be true, not merely
// assumed. Recorded as a taken decision in docs/DECISIONS.md's own
// "Taken" table -- a design acceptance living only in this comment is not
// discoverable from there, and this file's own doc comment is not a
// substitute for that register.
const hostileProviderMessage = `<script>alert(document.cookie)</script> and also FAKE-SECRET-sk-live-DO-NOT-LEAK-abc123`

// TestBuildProviderFailureDiagnostic_HostileMessageStaysJSONSafeOnTheWire
// is C6's own required negative test: a benign message (every OTHER test
// in this file) proves nothing about what happens when a provider echoes
// something adversarial. Plants hostileProviderMessage above into the
// REAL decode path (json.Unmarshal, exactly like
// TestBuildProviderFailureDiagnostic_NeverLeaksCredentialsOrPromptContent
// does for the allowlist), builds the diagnostic through the real
// buildProviderFailureDiagnostic, and serializes it through the exact
// diagnosticToWire+json.Marshal path that produces both the bytes
// appendRawEvent persists into the session journal (§7.3's own "the
// session's own event journal" surface, read by every logged-in
// participant who can reach GET .../events or the client-WS replay -- no
// role/session-membership check on that read path today) and the bytes
// this package emits over the sandbox WS to begin with.
//
// The containment this asserts: Go's encoding/json defaults to escaping
// HTML-significant characters (<, >, &) in string values UNLESS a caller
// explicitly opts out via json.Encoder.SetEscapeHTML(false) -- this
// package's own diagnosticToWire+json.Marshal call (translate.go) never
// does. So even though Message is retained verbatim as Go string data
// (asserted below -- this is NOT a claim that the content is dropped or
// mangled, only that it can never be interpreted as live markup by
// anything that renders these JSON bytes as HTML without a further,
// separate unescape step), the SERIALIZED bytes a hostile "<script>" tag
// produces can never execute as a script tag if naively embedded in an
// HTML document: proven by asserting the literal, executable substring
// "<script>" is ABSENT from the wire bytes, while the escaped form is
// present.
//
// HONEST SCOPE, stated so this test is not read as proving more than it
// does: this covers the WIRE/JOURNAL surface only. sessionactor's own
// operator log line (logProviderFailureDiagnostic, pushpr.go) uses
// log/slog's JSONHandler, which -- unlike encoding/json.Marshal -- does
// NOT HTML-escape string values by default; that surface's own
// containment depends on whatever consumes the structured log stream
// never rendering a log value as raw HTML, which this package cannot
// verify and does not attempt to here.
func TestBuildProviderFailureDiagnostic_HostileMessageStaysJSONSafeOnTheWire(t *testing.T) {
	t.Parallel()

	payload := `{"name":"APIError","data":{"message":` + mustJSONString(t, hostileProviderMessage) + `,"statusCode":502}}`
	var tagged openCodeTaggedError
	if err := json.Unmarshal([]byte(payload), &tagged); err != nil {
		t.Fatalf("json.Unmarshal(payload): %v", err)
	}

	a := &Adapter{runtimeVersion: testRuntimeVersion, sandboxID: testSandboxID}
	model := "anthropic/claude-sonnet-4-5"

	d := a.buildProviderFailureDiagnostic(&tagged, model)
	if d == nil {
		t.Fatal("buildProviderFailureDiagnostic returned nil for a non-nil tagged error")
	}
	// Retained verbatim (not dropped, not mangled) -- this is the whole
	// point of a MESSAGE the plan names as retained; containment is
	// asserted at the SERIALIZED level below, never by mutating the
	// in-memory value.
	if d.Message != hostileProviderMessage {
		t.Fatalf("Message = %q, want the hostile message retained verbatim in memory", d.Message)
	}

	wire := diagnosticToWire(d)
	wireJSON, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("json.Marshal(diagnosticToWire(d)): %v", err)
	}

	rawTagBytes := []byte{'<', 's', 'c', 'r', 'i', 'p', 't', '>'}
	if bytes.Contains(wireJSON, rawTagBytes) {
		t.Errorf("wire diagnostic JSON contains a literal, executable <script> tag -- want it HTML-escaped: %s", wireJSON)
	}
	// Round-trip proof, not a hardcoded escape-sequence literal (Go's own
	// \uXXXX JSON escaping is an implementation detail of encoding/json,
	// not a contract this test should pin byte-for-byte): decoding the
	// wire bytes back out must recover the EXACT same hostile string --
	// proving containment comes from ENCODING, never from the content
	// being silently dropped, truncated, or redacted along the way.
	var decoded sandboxws.ExecutionCompleteDiagnostic
	if err := json.Unmarshal(wireJSON, &decoded); err != nil {
		t.Fatalf("json.Unmarshal(wireJSON): %v", err)
	}
	if decoded.Message == nil || *decoded.Message != hostileProviderMessage {
		t.Errorf("wire diagnostic JSON did not round-trip the hostile message unchanged: got %v, want %q",
			decoded.Message, hostileProviderMessage)
	}
}

// mustJSONString marshals s as a JSON string literal, for building a raw
// JSON payload literal above without hand-escaping hostileProviderMessage's
// own quotes/angle-brackets by hand.
func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal(%q): %v", s, err)
	}
	return string(b)
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
	model := "anthropic/claude-sonnet-4-5"

	d := a.buildProviderFailureDiagnostic(&tagged, model)
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
	model := "anthropic/claude-sonnet-4-5"

	if d := a.buildProviderFailureDiagnostic(nil, model); d != nil {
		t.Errorf("buildProviderFailureDiagnostic(nil, model) = %+v, want nil", d)
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
	model := "anthropic/claude-sonnet-4-5"

	d := a.buildProviderFailureDiagnostic(&openCodeTaggedError{Name: "UnknownError"}, model)
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

// TestBuildProviderFailureDiagnostic_CapsEveryFieldInThePathThatMatters is
// an audit fix: TestCapDiagnosticField below proves capDiagnosticField
// itself truncates correctly, but nothing exercised buildProviderFailureDiagnostic
// with an over-cap INPUT before this test -- capDiagnosticField could be
// deleted from all six of buildProviderFailureDiagnostic's own construction
// sites (diagnostic.go: UnionMember/Model/RuntimeVersion/SandboxID always,
// Message/ProviderRequestID when err.Data != nil) and every diagnostic test
// still passed, so the plan row's "size-cap on top [of the allowlist]" was
// unenforced in the one path that actually matters: this repository's own
// dominant defect (a guard tested in isolation, never proven to be
// REACHED) applied to a size cap instead of a call site. Plants an
// over-cap value in EVERY field capDiagnosticField touches simultaneously
// -- err.Name, the model argument, a.runtimeVersion, a.sandboxID,
// err.Data.Message, and the one allowlisted request-id header -- and
// asserts every one of
// the six resulting struct fields is capped AND visibly marked, proving
// the cap is applied where it is actually exercised, not only where it is
// called directly.
func TestBuildProviderFailureDiagnostic_CapsEveryFieldInThePathThatMatters(t *testing.T) {
	t.Parallel()

	overCap := strings.Repeat("y", diagnosticFieldMaxBytes*2)
	headers := map[string]json.RawMessage{
		"x-request-id": json.RawMessage(`"` + overCap + `"`),
	}

	a := &Adapter{runtimeVersion: overCap, sandboxID: overCap}
	model := overCap
	tagged := &openCodeTaggedError{
		Name: overCap,
		Data: &openCodeErrorData{
			Message:         overCap,
			ResponseHeaders: headers,
		},
	}

	d := a.buildProviderFailureDiagnostic(tagged, model)
	if d == nil {
		t.Fatal("buildProviderFailureDiagnostic returned nil for a non-nil tagged error")
	}

	fields := map[string]string{
		"UnionMember":       d.UnionMember,
		"Model":             d.Model,
		"RuntimeVersion":    d.RuntimeVersion,
		"SandboxID":         d.SandboxID,
		"Message":           d.Message,
		"ProviderRequestID": d.ProviderRequestID,
	}
	for name, value := range fields {
		if len(value) > diagnosticFieldMaxBytes {
			t.Errorf("ProviderFailureDiagnostic.%s is %d bytes, want <= %d (capDiagnosticField was not applied at this construction site)",
				name, len(value), diagnosticFieldMaxBytes)
		}
		if !strings.HasSuffix(value, diagnosticTruncationMarker) {
			t.Errorf("ProviderFailureDiagnostic.%s = %q, want it to end with the truncation marker %q "+
				"(an over-cap input that comes back exactly diagnosticFieldMaxBytes long with no marker "+
				"would also satisfy the length check above while still being silently shortened)",
				name, value, diagnosticTruncationMarker)
		}
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
		// Audit fix: this subtest used to repeat "€" (U+20AC, 3 bytes) --
		// diagnosticFieldMaxBytes-len(diagnosticTruncationMarker) is 2034,
		// which is EXACTLY divisible by 3, so a naive byte-offset slice at
		// that limit always lands on a rune boundary regardless, by sheer
		// arithmetic coincidence -- this subtest passed even with the
		// rune-safety walk-back loop in capDiagnosticField deleted
		// entirely (mutation-verified: removing the `for limit > 0 &&
		// !utf8.RuneStart(...)` loop still left this exact payload
		// producing valid UTF-8). 2034 is ALSO divisible by 2, so a
		// 2-byte rune would be just as vacuous. A 4-byte rune breaks the
		// coincidence: 2034 mod 4 == 2, so a naive slice at byte 2034
		// lands 2 bytes into a 4-byte sequence, which IS invalid UTF-8
		// unless the walk-back logic actually runs. "😀" (U+1F600) is
		// UTF-8's 4-byte case.
		//
		// A LATER audit fix (A3): the paragraph above is a CLAIM about
		// today's constants, and nothing enforced it -- editing either
		// diagnosticFieldMaxBytes or diagnosticTruncationMarker later
		// (for an unrelated reason) could silently move `limit` back onto
		// a multiple of 4, restoring the exact vacuity this fix removed,
		// with nothing here to notice. Compute the boundary from the LIVE
		// constants and refuse to proceed if it lands on a 4-byte rune
		// boundary, BEFORE exercising capDiagnosticField at all -- this
		// subtest's own claim to test something must fail loudly, not the
		// assertions below pass vacuously.
		const runeWidth = 4 // "😀" (U+1F600) is UTF-8's 4-byte case, used below.
		limit := diagnosticFieldMaxBytes - len(diagnosticTruncationMarker)
		if limit%runeWidth == 0 {
			t.Fatalf("this subtest is VACUOUS with today's diagnosticFieldMaxBytes (%d) and "+
				"diagnosticTruncationMarker length (%d bytes): the truncation boundary (%d) is "+
				"exactly divisible by %d, so a naive byte-offset slice there lands on a rune "+
				"boundary regardless of whether capDiagnosticField's own rune-safety walk-back "+
				"loop runs at all -- choose a differently-sized test rune (or otherwise re-derive "+
				"a payload that genuinely straddles this boundary) before trusting the assertions "+
				"below to catch a regression",
				diagnosticFieldMaxBytes, len(diagnosticTruncationMarker), limit, runeWidth)
		}

		long := strings.Repeat("😀", diagnosticFieldMaxBytes)
		got := capDiagnosticField(long)
		if !utf8.ValidString(got) {
			t.Errorf("capDiagnosticField produced invalid UTF-8: %q", got)
		}
		if !strings.HasSuffix(got, diagnosticTruncationMarker) {
			t.Errorf("capDiagnosticField result does not end with the truncation marker %q: %q", diagnosticTruncationMarker, got)
		}
	})
}

// TestModelDisplayFromInfo is E4's own pin: modelDisplayFromInfo's
// empty-half guard is the exact mechanism implementing this package's
// stated mitigation for OpenCode's own AssistantMessage schema drifting
// (openCodeMessageInfo's own doc comment, types.go) -- "degrades to an
// empty string if the field is absent" -- rather than emitting a
// half-populated, misleading "providerID/" or "/modelID" string. Before
// this test, deleting the `if info.ProviderID == "" || info.ModelID ==
// "" { return "" }` guard entirely (diagnostic.go) left this package's
// whole suite green: nothing exercised a message.updated with exactly
// ONE of the two fields set. Covers all four combinations.
func TestModelDisplayFromInfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		providerID string
		modelID    string
		want       string
	}{
		{
			name:       "both present",
			providerID: "anthropic",
			modelID:    "claude-opus-5",
			want:       "anthropic/claude-opus-5",
		},
		{
			// VERIFIED LIVE shape (openCodeMessageInfo's own doc comment,
			// types.go): the pinned 1.17.15 binary's real AssistantMessage
			// payload carries both fields together whenever it carries
			// either -- this case (and "only modelID" below) covers the
			// SCHEMA's own stated contract, not a shape this adapter has
			// observed live, exactly the drift this guard defends against.
			name:       "only providerID present",
			providerID: "anthropic",
			modelID:    "",
			want:       "",
		},
		{
			name:       "only modelID present",
			providerID: "",
			modelID:    "claude-opus-5",
			want:       "",
		},
		{
			name:       "neither present",
			providerID: "",
			modelID:    "",
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			info := openCodeMessageInfo{
				ID: "msg1", Role: "assistant",
				ProviderID: tt.providerID, ModelID: tt.modelID,
			}
			if got := modelDisplayFromInfo(info); got != tt.want {
				t.Errorf("modelDisplayFromInfo(providerID=%q, modelID=%q) = %q, want %q",
					tt.providerID, tt.modelID, got, tt.want)
			}
		})
	}
}
