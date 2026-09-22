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
// "x-request-id" and "request-id" are still schema-derived/best-effort
// only (the two conventional, provider-agnostic header names a support
// process is generally taught to look for) -- NOW EMPIRICALLY TESTED,
// not merely guessed: a later Step's own research pass, once the pinned
// binary became reachable, captured a genuine 401 APIError's real
// responseHeaders (openCodeErrorData's own doc comment, types.go) and
// NEITHER was present, reproduced identically across 3 separate trials.
// Kept anyway, deliberately: this adapter is provider-agnostic (§7's own
// anti-corruption layer), and a DIFFERENT upstream provider than the one
// this specific trial exercised may well use one of these conventional
// names even though this one didn't -- an allowlist candidate that never
// matches on ONE provider's traffic costs nothing to keep for another's.
//
// "cf-ray" was ADDED after that same research pass, for the opposite
// reason -- it is the one header the real captured payload actually DOES
// carry a genuine per-request identifier under: Cloudflare's own Ray ID
// (VERIFIED live, format "<16 hex chars>-<PoP code>", e.g.
// "a3f1328d4e1be195-MRS", reproduced identically across all 3 trials),
// present because this specific provider's own API is fronted by
// Cloudflare (responseHeaders' own "server":"cloudflare", and the SAME
// captured payload's responseBody carries "request_id":null --  the
// provider's own APPLICATION-level request id was genuinely absent for
// this failure, an authentication rejection that never reached the
// provider's own request-processing layer at all). cf-ray is honestly
// edge-level, not the provider's own application-level id -- but it is a
// real, unique-per-request token a support conversation CAN use to trace
// the request through Cloudflare's edge, which is exactly what makes it
// belong on this allowlist rather than the two conventional names it
// sits alongside: it carries an identifier, the one bar this allowlist
// has ever required (never a blocklist, and never a key admitted for any
// other reason -- see extractProviderRequestID's own doc comment).
// Ordered LAST, after the two application-level conventions: when a
// provider's own request_id IS present under one of those, it is the
// more specific, more directly useful token, and should win.
var requestIDHeaderCandidates = []string{"x-request-id", "request-id", "cf-ray"}

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
// string (the shape the real captured responseHeaders payload actually
// carries every value under — see openCodeErrorData's own doc comment,
// types.go), or an array of strings (matching Go's own net/http.Header
// JSON shape; never observed live, kept defensively for a provider or
// OpenCode revision this adapter has not captured). Returns "" for
// anything else, including a decode failure — extractProviderRequestID's
// own caller-side loop already treats an empty result as "try the next
// candidate".
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
// Three sources, never conflated:
//   - Message/UnionMember/StatusCode/ProviderRequestID come from
//     OpenCode's own tagged-union error, decoded through
//     openCodeErrorData's own allowlist (types.go) — never the error
//     object whole, and never responseHeaders/responseBody/metadata
//     wholesale (see extractProviderRequestID's own doc comment for the
//     one narrow exception, and why it stays narrow).
//   - Model comes from OpenCode's own report of which model produced the
//     specific assistant message this failure is attached to
//     (openCodeMessageInfo.ModelID/ProviderID, types.go) — the ENGINE's
//     own answer, deliberately never this adapter's own request-side
//     cmd.Model/resolveModel(Forced) (see this field's own doc comment
//     below for why the request is the wrong source). Extracted by
//     modelDisplayFromInfo, below.
//   - RuntimeVersion/SandboxID are THIS ADAPTER'S OWN already-known
//     context — never read from the wire payload at all. RuntimeVersion
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

	// Model is the "providerID/modelID" string OpenCode itself reports for
	// the specific assistant message this failure is attached to
	// (openCodeMessageInfo.ModelID/ProviderID, types.go) — the ENGINE's
	// own report of which model actually ran, never this adapter's own
	// request-side cmd.Model or its resolveModel(Forced) return value.
	//
	// Reading the REQUEST was always the wrong source for this field, for
	// two separate reasons, and an earlier version of this package tried
	// exactly that and got it wrong twice over: (1) cmd.Model is commonly
	// nil (the default configuration every one of Composer/PlanModeView/
	// Timeline resume/DecisionInbox dispatches through, web/src/session),
	// and the wire request correctly OMITS the model field on that path
	// (resolveModel, session.go) rather than substituting a Narvi-side
	// default of its own (§7.3; the exact rule
	// internal/app/workflowengine/advance.go's own doc comment states for
	// modelID/effort's session-row fallback) — so a request-sourced Model
	// would have to either lie (force one onto the wire the client never
	// asked for, changing PRODUCTION behavior just to populate a
	// diagnostic string) or stay "" on the overwhelmingly common path,
	// which is the ORIGINAL pre-Step defect this field exists to fix.
	// (2) Even when cmd.Model IS set, the compaction-retry path
	// (attemptCompactionRetry, adapter.go) can resolve a DIFFERENT model
	// for its own retried dispatch than the original attempt used
	// (resolveModelForced's own doc comment, session.go) -- a
	// request-sourced Model on that retry's own failure would then name a
	// model the failing request did not use. Sourcing from the engine's
	// own per-message report sidesteps both problems at once: whichever
	// model actually produced the SPECIFIC message this diagnostic
	// describes is what gets reported, regardless of which resolution
	// path (or none) put it there.
	//
	// "" is still possible, honestly: NOT merely whenever errorForOutcome
	// falls back to its own sessionError branch (turn.go) -- VERIFIED
	// LIVE that a session-level session.error routinely fires WHILE an
	// assistant message.updated reporting the model has already arrived
	// (the model just hasn't been attached to an error yet at that
	// point; that arrives in a separate, later message.updated -- see
	// modelForOutcome's own doc comment, turn.go, for the full captured
	// ordering), so that branch alone does NOT imply "no model". This
	// field is "" only when no assistant message.updated was ever
	// observed for this turn at all (e.g. a genuine "model not found"
	// session-level failure that never gets far enough to create one --
	// VERIFIED LIVE separately), or the one assistant message this turn
	// did see carried no ModelID/ProviderID at all (modelDisplayFromInfo's
	// own empty-half guard, below) -- both honest "unknown", never a
	// wrong guess. ModelID/ProviderID's own real key names ARE now
	// VERIFIED LIVE against the pinned 1.17.15 binary
	// (openCodeMessageInfo's own doc comment, types.go, has the full
	// captured payload) -- the earlier "schema-derived only" verification
	// gap this comment used to cite is closed.
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
// model is a plain "providerID/modelID" string (or "") the CALLER has
// already resolved before calling this function, from the engine's own
// per-message report (modelDisplayFromInfo, below) — never re-derived
// here. Taking a plain string rather than a *turnState (an earlier
// version of this function did) keeps that resolution a single call
// site's own responsibility: sse.go's session.idle case pairs
// ts.errorForOutcome() with ts.modelForOutcome() (turn.go) — see
// modelForOutcome's own doc comment (turn.go) for why the two are
// deliberately NOT required to describe the same assistant message;
// finalizeByFallback (adapter.go) pairs last.Info.Error with
// modelDisplayFromInfo(last.Info), the identical messageListEntry. Neither
// call site re-resolves cmd.Model or calls resolveModel(Forced) here —
// seeing §7.3's own "a retry decision is not a diagnosis" gap tempted an
// earlier version of this package into reading the REQUEST's resolved
// model instead, which is wrong for two independent reasons documented on
// ProviderFailureDiagnostic.Model's own doc comment above; this
// parameter's own engine-report sourcing avoids both.
func (a *Adapter) buildProviderFailureDiagnostic(err *openCodeTaggedError, model string) *ProviderFailureDiagnostic {
	if err == nil {
		return nil
	}

	d := &ProviderFailureDiagnostic{
		UnionMember:    capDiagnosticField(err.Name),
		Model:          capDiagnosticField(model),
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

// modelDisplayFromInfo renders an openCodeMessageInfo's own engine-reported
// model as a "providerID/modelID" string — the SAME display format
// ProviderFailureDiagnostic.Model already uses. "" when either half is
// unset: OpenCode's own AssistantMessage schema lists ModelID/ProviderID as
// required together (openCodeMessageInfo's own doc comment, types.go), but
// a UserMessage carries neither, and this defends the same way against a
// partial value from a payload shape this adapter has not independently
// verified live.
func modelDisplayFromInfo(info openCodeMessageInfo) string {
	if info.ProviderID == "" || info.ModelID == "" {
		return ""
	}
	return info.ProviderID + "/" + info.ModelID
}
