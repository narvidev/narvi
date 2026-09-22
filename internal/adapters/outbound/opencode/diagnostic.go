package opencode

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// This file implements §7.3 ("a retry decision is not a diagnosis"):
// once a turn's own retries are exhausted, what used to reach a
// human was `opencode: <union member name>` and nothing that locates the
// cause — not the model that ran, not the runtime version, not the
// provider's own request identifier. ProviderFailureDiagnostic is the
// allowlisted, size-capped record this package now retains instead,
// built by buildProviderFailureDiagnostic below and threaded onto
// turnOutcome.Diagnostic (outcome.go) by both real deriveOutcome call
// sites (sse.go's own "session.idle" case, adapter.go's own
// finalizeByFallback) and carried, UNCHANGED, through every later
// turnOutcome{} reconstruction that only enriches Reason (§7.2's own
// retry-exhausted paths, adapter.go) — see each of those call sites' own
// doc comments for why the diagnostic must survive a reconstruction that
// only ever rewrites Reason.
//
// NEVER consulted by the retry decision. isTransientAPIError/
// isContextOverflowError (outcome.go) read ONLY openCodeTaggedError
// itself — never this type, never turnOutcome.Diagnostic — and
// ProviderFailureDiagnostic itself carries no field resembling a
// classification verdict (no bool at all), so there is nothing here a
// future misguided change could even mistake for one.

// diagnosticFieldMaxBytes bounds every ProviderFailureDiagnostic string
// field (§7.3: "a size cap applies on top [of the allowlist], because a
// provider is free to return a message of any length and a diagnostic
// that fills the session journal is its own outage"). Not specified by
// the plan; chosen generously for a human-readable message/id while
// still bounding the worst case — the same "not specified in the plan;
// chosen" spirit fallbackModel's own doc comment (session.go) already
// uses for an identical kind of invented default. 2KiB comfortably holds
// any real message/id this adapter has ever observed live (e.g.
// MessageAbortedError's own data.message, "Aborted", VERIFIED LIVE per
// openCodeTaggedError's own doc comment, types.go) while still bounding
// an adversarial or buggy provider response to a small, fixed multiple
// of this struct's own field count — nowhere near "fills the session
// journal".
const diagnosticFieldMaxBytes = 2048

// diagnosticTruncationMarker is appended whenever capDiagnosticField
// actually truncates a field, so a shortened message never LOOKS
// complete — an operator reading a diagnostic that ends mid-sentence
// with no marker at all would have no way to tell "the provider's
// message really was this short" from "this adapter cut it off".
const diagnosticTruncationMarker = "...[truncated]"

// capDiagnosticField truncates s to diagnosticFieldMaxBytes, trimming on
// a valid UTF-8 rune boundary so the result is never invalid UTF-8 partway
// through a multi-byte rune — this value is re-serialized to JSON on both
// of §7.3's own surfaces (the session journal's raw wire bytes, and the
// correlation-id-scoped operator log line), and json.Marshal of a string
// that splits a rune mid-sequence would silently substitute replacement
// characters at an offset that depends on where the cap happened to land,
// not on the field's own content.
func capDiagnosticField(s string) string {
	if len(s) <= diagnosticFieldMaxBytes {
		return s
	}
	limit := diagnosticFieldMaxBytes - len(diagnosticTruncationMarker)
	if limit < 0 {
		limit = 0
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit] + diagnosticTruncationMarker
}

// requestIDHeaderCandidates is the small, NAMED allowlist §7.3 requires
// for extracting the provider's own request identifier out of APIError's
// own responseHeaders — never the headers map itself (see
// extractProviderRequestID's own doc comment). Ordered by priority: the
// first candidate present (case-insensitively) in the payload's own
// responseHeaders wins, regardless of that map's own randomized (per Go's
// map iteration) key order.
//
// NOT independently verified live against a real provider error — this
// adapter's own research pass (openCodeTaggedError's own doc comment,
// types.go) never elicited a genuine APIError from a live OpenCode
// process, so no header name here has been confirmed against real
// traffic. "x-request-id" and "request-id" are the two conventional,
// provider-agnostic header names a support process is generally taught
// to look for. Documented here, honestly, as schema-derived/best-effort —
// exactly like this package's own subtaskPart (types.go) is labeled when
// a real invocation never exercised it.
var requestIDHeaderCandidates = []string{"x-request-id", "request-id"}

// extractProviderRequestID is the ONLY function in this package that ever
// reads openCodeErrorData.ResponseHeaders. It returns a single, capped
// string — never the map itself — which is what keeps
// ProviderFailureDiagnostic an allowlist enforced by its own field TYPES
// (every field below is a string or *int; there is no field a whole
// headers map could ever be assigned to without a compile error) rather
// than by a comment alone. An unrecognized header name is simply
// invisible here, by construction, exactly like an unrecognized field on
// openCodeErrorData itself (types.go's own "drop by construction"
// discipline, extended one level deeper).
func extractProviderRequestID(headers map[string]json.RawMessage) string {
	for _, candidate := range requestIDHeaderCandidates {
		for key, raw := range headers {
			if !strings.EqualFold(key, candidate) {
				continue
			}
			if v := decodeHeaderValue(raw); v != "" {
				return v
			}
		}
	}
	return ""
}

// decodeHeaderValue tolerates both shapes a JSON-encoded HTTP header
// value might take in OpenCode's own responseHeaders object: a single
// string (the common case for a header map serialized from a plain JS
// object), or an array of strings (matching Go's own net/http.Header
// JSON shape, in case OpenCode ever serializes it that way without
// flattening) — since this adapter has never observed a real APIError's
// own responseHeaders live (see requestIDHeaderCandidates' own doc
// comment), it cannot assume either shape. Returns "" for anything else,
// including a decode failure — extractProviderRequestID's own caller-side
// loop already treats an empty result as "try the next candidate".
func decodeHeaderValue(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		return arr[0]
	}
	return ""
}

// ProviderFailureDiagnostic is the allowlisted, size-capped record §7.3
// names exactly seven fields for. Every field is named here deliberately:
// this struct IS the allowlist itself, structurally enforced by Go's own
// compiler and json.Marshal — a field never added here can never appear
// on the wire or in a log line, no matter what OpenCode's schema grows to
// carry next (§7.3: "the moment a diagnostic field becomes an input to
// [the retry] decision, this has become classification by error text" —
// the inverse risk this struct guards against is a field becoming an
// input to THIS record without a deliberate, reviewed addition here).
//
// Two sources, never conflated:
//   - Message/UnionMember/StatusCode/ProviderRequestID come from
//     OpenCode's own tagged-union error, decoded through
//     openCodeErrorData's own allowlist (types.go) — never the error
//     object whole, and never responseHeaders/responseBody/metadata
//     wholesale (see extractProviderRequestID's own doc comment for the
//     one narrow exception, and why it stays narrow).
//   - Model/RuntimeVersion/SandboxID are THIS ADAPTER'S OWN already-known
//     context — never read from the wire payload at all. Model is the
//     "providerID/modelID" this turn actually dispatched with
//     (turnState.model, turn.go, captured at StartTurn); RuntimeVersion
//     is the pinned OpenCode binary version this Adapter was constructed
//     with (Adapter.runtimeVersion) — the SAME value §7's own boot
//     fingerprint records (opencodeproc.Result.Version, sourced
//     identically); SandboxID is the sandbox this turn ran on
//     (Adapter.sandboxID), sourced from SessionConfig.SandboxId at
//     construction.
//
// json tags match the wire contract's own "diagnostic" object
// (contracts/sandbox-ws/v1/events.schema.json) field-for-field —
// translate.go's own diagnosticToWire is the one function that converts
// this internal type to the generated sandboxws.ExecutionCompleteDiagnostic
// wire type; nothing else in this package constructs that wire type.
type ProviderFailureDiagnostic struct {
	// Message is APIError's own (or another tagged-union member's own)
	// data.message, size-capped. Never responseBody — see
	// openCodeErrorData's own doc comment (types.go) for why that field
	// is never modeled at all.
	Message string `json:"message,omitempty"`

	// UnionMember is the tagged-union member name already decoded for
	// the retry classification (openCodeTaggedError.Name), e.g.
	// "APIError". Purely descriptive, mirroring
	// ports.ProviderError.Code's own "kept for logging/debugging, never
	// re-parsed to reclassify" contract — never itself consulted to
	// reclassify anything.
	UnionMember string `json:"unionMember,omitempty"`

	// StatusCode is the HTTP status OpenCode itself already decoded
	// (openCodeErrorData.StatusCode) — corroborating detail only, per
	// that field's own doc comment; never itself an input to the retry
	// decision.
	StatusCode *int `json:"statusCode,omitempty"`

	// ProviderRequestID is extracted from APIError's own responseHeaders
	// by extractProviderRequestID's small, named allowlist — the one
	// token that makes a support conversation with the provider possible
	// (§7.3). Never the headers map itself.
	ProviderRequestID string `json:"providerRequestId,omitempty"`

	// Model is the "providerID/modelID" string this turn actually
	// dispatched with — adapter-side context, never itself part of
	// OpenCode's own error payload. Never "" for a real turn: StartTurn
	// resolves cmd.Model via resolveModelForced (session.go) before this
	// turn's own turnState even exists, which always returns a concrete
	// ref (falling back to fallbackModelRef() rather than omitting the
	// wire field when the request named no model at all — the default
	// configuration every one of Composer/PlanModeView/Timeline resume/
	// DecisionInbox dispatches through, web/src/session) — so this field
	// answers §7.3's own first-named-missing fact on every path, not only
	// a client-picked one.
	Model string `json:"model,omitempty"`

	// RuntimeVersion is the pinned OpenCode binary version this sandbox
	// actually ran — adapter-side context, sourced identically to §7's
	// own boot fingerprint.
	RuntimeVersion string `json:"runtimeVersion,omitempty"`

	// SandboxID is the sandbox this turn ran on — adapter-side context,
	// never itself part of OpenCode's own error payload.
	SandboxID string `json:"sandboxId,omitempty"`
}

// buildProviderFailureDiagnostic implements §7.3's own allowlist: retain
// exactly the seven fields that section names, and nothing else. nil err
// (no tagged error observed at all — e.g. deriveOutcome's own "turn
// produced no output" path, or an adapter-local transport failure that
// never reached OpenCode) returns nil: a diagnostic describes a PROVIDER
// failure, and there is none to describe.
//
// Deliberately takes ts.model (a plain string, immutable after
// newTurnState — see that field's own doc comment, turn.go, for why
// reading it here from a goroutine other than the one that constructed
// ts is race-free) rather than re-resolving the model itself: this
// function must describe whatever model THIS turn actually dispatched
// with, not re-derive a possibly-different answer from cmd.Model (a
// second resolveModelForced call could, in principle, resolve to a
// different promptModelRef if the catalog changed between the original
// dispatch and a later failure — ts.model is the one value known to
// match what was actually sent). This is why a RETRIED prompt's own
// failure (attemptCompactionRetry/attemptTransientRetry, adapter.go)
// never rebuilds a fresh Diagnostic from the retry's own freshly-resolved
// model either — every reconstruction site there carries the ORIGINAL
// outcome's Diagnostic (ts.model, frozen at StartTurn) forward unchanged,
// per turnOutcome.Diagnostic's own doc comment (outcome.go); both the
// original dispatch and any later retry resolve ts.cmd.Model through the
// SAME resolveModelForced, so the two agree in practice except for that
// same accepted, pre-existing "catalog changed mid-turn" edge case.
func (a *Adapter) buildProviderFailureDiagnostic(err *openCodeTaggedError, ts *turnState) *ProviderFailureDiagnostic {
	if err == nil {
		return nil
	}

	d := &ProviderFailureDiagnostic{
		UnionMember:    capDiagnosticField(err.Name),
		Model:          capDiagnosticField(ts.model),
		RuntimeVersion: capDiagnosticField(a.runtimeVersion),
		SandboxID:      capDiagnosticField(a.sandboxID),
	}
	if err.Data != nil {
		d.Message = capDiagnosticField(err.Data.Message)
		d.StatusCode = err.Data.StatusCode
		d.ProviderRequestID = capDiagnosticField(extractProviderRequestID(err.Data.ResponseHeaders))
	}
	return d
}
