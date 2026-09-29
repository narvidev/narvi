package ports

import "context"

// IntentDecision is §18.1's fixed, non-negotiable never-throw contract
// shape, quoted verbatim from the technical plan:
//
//	type IntentDecision struct {
//	    Source         string // "classifier" | "fallback"
//	    Target         string // decision-specific, e.g. review/request
//	    Mode           string // e.g. plan/build
//	    Confidence     string // "high" | "medium" | "low" -- classifier source only
//	    Reasoning      string // classifier source only
//	    FallbackReason string // fallback source only; enumerated
//	}
//
// Every field stays a bare string exactly as fixed above -- this Step
// does not introduce distinct named types for Source/Confidence/
// FallbackReason (the IntentSource*/IntentConfidence*/FallbackReason*
// constants below exist purely as named string values for callers to
// compare against, never as a different Go type the struct's own fields
// would have to change shape to hold).
//
// CostUSD is an ADDITIVE field beyond §18.1's original fixed shape (audit
// fix L18, "IntentDecisionRecord.CostUSD is structurally dead"): populated
// by Classify only for a genuine IntentSourceClassifier verdict, from the
// underlying ports.LLM.Complete call's own now-available
// CompletionResponse.CostUSD; left nil for every IntentSourceFallback
// decision (no real LLM call succeeded, so there is no real cost to
// report) -- mirrors intentdomain.IntentDecisionRecord.CostUSD's identical
// "populated only when the real cost is known, omitted, never guessed"
// contract, since that record field is what this one ultimately feeds.
type IntentDecision struct {
	Source         string
	Target         string
	Mode           string
	Confidence     string
	Reasoning      string
	FallbackReason string
	CostUSD        *float64
}

// The two IntentDecision.Source values (§18.1).
const (
	IntentSourceClassifier = "classifier"
	IntentSourceFallback   = "fallback"
)

// The three IntentDecision.Confidence values (§18.2) -- classifier source
// only. Mirrors internal/domain/intent's identical constants; duplicated
// here (rather than imported) because ports/doc.go's own import-direction
// rule restricts this package to the standard library +
// contracts/gen/go/* only, and these are the SAME three bare-string
// values domain/intent.Confidence{High,Medium,Low} already name --
// there is exactly one rubric, defined once, in domain/intent.
const (
	IntentConfidenceHigh   = "high"
	IntentConfidenceMedium = "medium"
	IntentConfidenceLow    = "low"
)

// FallbackReason is IntentDecision.FallbackReason's enumerated, growable
// set (§18.1) -- distinguished via LLMError.Code, classified by error
// code, NEVER by string-matching. Exactly the same 5 values LLMErrorCode
// enumerates (llm.go) -- an unsupported/misconfigured classifier-model
// choice is itself just another fallback reason, never a silent
// substitution that keeps reporting success.
const (
	FallbackReasonNoAPIKey            = "no_api_key"
	FallbackReasonTimeout             = "timeout"
	FallbackReasonInvalidOutput       = "invalid_output"
	FallbackReasonAPIError            = "api_error"
	FallbackReasonUnsupportedProvider = "unsupported_provider"
)

// IntentClassifierInput is what IntentClassifier.Classify needs to
// classify one piece of ingress input.
type IntentClassifierInput struct {
	// Text is the input text to classify -- a session's initial prompt, a
	// Slack message, a GitHub comment body, a Linear agent-session
	// prompt, ...
	Text string
	// Surface matches sessions.spawn_source's existing enum exactly
	// (migrations/000004_sessions.up.sql: "web" | "slack" | "linear" |
	// "github", and "mcp" since migrations/000149) -- which ingress
	// surface is asking.
	Surface string
	// DeterministicTarget is an independently-computed, non-LLM signal
	// the calling surface already has on hand for Target corroboration
	// (§18.2) -- e.g. GitHub ingress's own "did this mention land on an
	// already-tracked PR row" fact. Empty string means "no deterministic
	// signal available for this input"; the classifier still corroborates
	// (internal/domain/intent.CorroborateTarget) but cannot confirm
	// agreement without one.
	DeterministicTarget string
	// PlausibleTargetCount is how many distinct Target readings the
	// calling surface judges this input could plausibly support -- fed to
	// internal/domain/intent.DeriveNeedsClarification by a FUTURE caller
	// that actually acts on Target (§18.2). A caller unable to estimate
	// this should pass 2 (the conservative "could be either" default).
	PlausibleTargetCount int
}

// IntentClassifier is the unified intent classifier port (§8.3, §18):
// review-vs-request and plan-vs-build across all ingress surfaces.
// internal/app/intentclassifier is its one real implementation.
//
// Classify NEVER returns a caller-fatal error — every code path resolves
// to an IntentDecision, and callers pattern-match on Source, never on an
// error type (§18.1's own never-throw contract).
type IntentClassifier interface {
	Classify(ctx context.Context, input IntentClassifierInput) IntentDecision
}
