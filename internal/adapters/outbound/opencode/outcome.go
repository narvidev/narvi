package opencode

import (
	"fmt"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// turnOutcome is the result of deriveOutcome — the pure, directly
// unit-testable core of §7's own "treat 'no output' as failure" and
// error-name-to-outcome derivation logic. Kept in its own file/pure
// function specifically so it can be tested against constructed
// values rather than a live model (this Step's own testing instructions:
// "test the DERIVATION FUNCTION itself directly... this is both more
// reliable and a better test anyway").
type turnOutcome struct {
	Outcome sandboxws.ExecutionCompleteOutcome
	Reason  *string

	// Diagnostic is §7.3's own allowlisted, size-capped
	// provider-failure record (diagnostic.go) — nil except when Outcome
	// is Failed AND a real *openCodeTaggedError was observed (built by
	// Adapter.buildProviderFailureDiagnostic, never by deriveOutcome
	// itself, which stays free of Adapter state so it can remain the
	// pure, directly unit-testable function its own doc comment above
	// promises). Deliberately NOT set here: both of deriveOutcome's real
	// call sites (sse.go's own "session.idle" case, adapter.go's own
	// finalizeByFallback) assign it onto the returned value themselves,
	// right after calling deriveOutcome, since only they have an Adapter
	// receiver in scope.
	//
	// Every later turnOutcome{} literal that only enriches Reason (§7.2's
	// own retry-exhausted paths: finalizeOrRecoverFromOverflow's
	// "already attempted" branch, attemptCompactionRetry's and
	// attemptTransientRetry's own "recovery itself failed" branches, all
	// adapter.go) MUST copy this field forward from the outcome it is
	// enriching — never rebuild a bare turnOutcome{Outcome: ...,
	// Reason: &reason} for a failure that already had a Diagnostic, or
	// the one record §7.3 requires silently vanishes at the exact moment
	// ("once the retries are exhausted") it matters most. A Cancelled
	// reconstruction (a Stop landing mid-retry) is the one case that
	// correctly builds a fresh literal with no Diagnostic at all — a
	// cancellation is not a provider failure.
	Diagnostic *ProviderFailureDiagnostic
}

// deriveOutcome implements the ground-truth mapping this Step's own
// research pass verified live (an aborted turn's own session.error +
// final message.updated both carried {"name":"MessageAbortedError"}):
//
//   - err != nil && err.Name == "MessageAbortedError" -> "cancelled"
//     (matches Narvi's own Stop-initiated cancellation semantics, §3.3's
//     taxonomy). No reason is set — the outcome itself already says why.
//   - err != nil (any other tagged-union name) -> "failed", with a short,
//     error-name-derived reason. The raw provider error body is NEVER
//     embedded here (even though err only carries a Name, never a raw
//     body, by construction — see openCodeTaggedError's own doc comment)
//     — mirroring the §4.1/§6.4 "never leak a raw response body" lesson
//     defensively.
//   - err == nil && (hasText || hasToolCall) -> "completed".
//   - err == nil && !hasText && !hasToolCall -> "failed" (§7's own
//     explicit "treat 'no output' as failure" quirk: a model that
//     produced nothing must never silently read as success).
func deriveOutcome(err *openCodeTaggedError, hasText, hasToolCall bool) turnOutcome {
	if err != nil {
		if err.Name == "MessageAbortedError" {
			return turnOutcome{Outcome: sandboxws.ExecutionCompleteOutcomeCancelled}
		}
		reason := fmt.Sprintf("opencode: %s", err.Name)
		return turnOutcome{Outcome: sandboxws.ExecutionCompleteOutcomeFailed, Reason: &reason}
	}

	if hasText || hasToolCall {
		return turnOutcome{Outcome: sandboxws.ExecutionCompleteOutcomeCompleted}
	}

	reason := "opencode: turn produced no output"
	return turnOutcome{Outcome: sandboxws.ExecutionCompleteOutcomeFailed, Reason: &reason}
}

// isContextOverflowError implements §7.2's own "classify on the typed
// discriminator already decoded" design point — err.Name ==
// "ContextOverflowError" is a genuine typed signal (openCodeTaggedError,
// types.go), not string-matching a provider error body, the same
// discipline §4.1 requires of ProviderError and §18.1 requires of
// FallbackReason. Used by Adapter.finalizeOrRecoverFromOverflow
// (adapter.go) to decide whether a failed outcome is even eligible for a
// compaction retry at all; nil (no error at all, or a nil tagged error) is
// never a ContextOverflowError.
func isContextOverflowError(err *openCodeTaggedError) bool {
	return err != nil && err.Name == "ContextOverflowError"
}

// isTransientAPIError implements this Step's own ("typed transient-error
// retry for the OpenCode adapter") design point — classify ONLY on the
// typed field OpenCode itself already computed, never on a substring of
// outcome.Reason or any raw provider error body: err.Name == "APIError" &&
// err.Data != nil && err.Data.IsRetryable (openCodeTaggedError.Data,
// types.go — decoded from the real, live-fetched OpenAPI schema's own
// APIError.data.isRetryable, REQUIRED whenever "data" is present at all).
// The same typed-discriminator discipline isContextOverflowError above
// already follows.
//
// Deliberately checks err.Name == "APIError" FIRST, never Data.IsRetryable
// alone: Data is decoded generically on openCodeTaggedError for every
// tagged-union member (types.go), but only APIError's own real schema
// defines "isRetryable" at all — a differently-shaped member that also
// happens to carry a "data" object (e.g. MessageAbortedError's own real,
// live-verified payload, {"data":{"message":"Aborted"}}, which has no
// "isRetryable" key whatsoever) would still decode into a non-nil Data
// whose IsRetryable is simply Go's zero value (false) — never a genuine
// verdict from OpenCode about anything. Checking Name first prevents ever
// mistaking that zero value for a real "permanent" classification from a
// member that never expressed an opinion on retryability at all.
//
// A nil err, or a non-nil err with a nil Data (a same-name payload that
// happened to omit "data" entirely — the schema says "data" itself is
// optional on openCodeTaggedError even though "isRetryable" is required
// WITHIN it, and this adapter never assumes a wire payload is
// well-formed), is never transient: fail closed, exactly matching
// isContextOverflowError's own nil handling above.
//
// Deliberately excludes local-connection failures BY CONSTRUCTION, not via
// a special case checked here: a failure to reach OpenCode's OWN local
// HTTP server (e.g. postPromptAsync's initial dispatch or its retried
// re-dispatch, both routed through client.go's doJSON) surfaces as a plain
// Go error returned directly from an HTTP round trip — there is no
// OpenCode-side JSON payload to decode in that case at all (the call to
// OpenCode itself never got a response to decode), so it can never reach
// this function, or be handed to it, in the first place: this function
// (and the retry path built on it, Adapter.finalizeOrRecoverFromOverflow/
// attemptTransientRetry, adapter.go) only ever classifies the typed union
// OpenCode's OWN session-level error events (session.error/
// message.updated's info.error, or the final-message-fetch fallback's own
// last.Info.Error) carry — never the adapter's own outbound HTTP failures
// talking to OpenCode itself. Retrying THAT class of failure would hide a
// crashed local OpenCode process behind a backoff-and-retry loop — exactly
// the "sandbox-health signal, not a provider blip" conflation this Step's
// own design avoids by construction, never by pattern-matching a
// particular error shape.
func isTransientAPIError(err *openCodeTaggedError) bool {
	return err != nil && err.Name == "APIError" && err.Data != nil && err.Data.IsRetryable
}

// enrichReasonForFailedRecovery builds the finalize reason used when a
// §7.2 compaction-retry recovery attempt itself failed -- either
// forceCompaction (the POST /session/{id}/summarize call) errored, or the
// retried postPromptAsync dispatch itself failed at the transport level
// (Adapter.attemptCompactionRetry, adapter.go). Deliberately references the
// ORIGINAL overflow outcome's own reason (originalReason) alongside detail
// (a short description of what failed, e.g. "forceCompaction: <err>"), so
// the final reason string an operator sees names BOTH facts: the original
// failure that triggered the recovery attempt, AND that a recovery was
// attempted and also failed -- never a bare "opencode: ContextOverflowError"
// indistinguishable from a turn where no recovery was ever attempted at
// all.
func enrichReasonForFailedRecovery(originalReason *string, detail string) string {
	original := "opencode: ContextOverflowError"
	if originalReason != nil {
		original = *originalReason
	}
	return fmt.Sprintf("%s (compaction retry attempted and failed: %s)", original, detail)
}

// enrichReasonForRepeatedOverflow builds the finalize reason used when the
// RETRIED prompt also overflowed (Adapter.finalizeOrRecoverFromOverflow,
// adapter.go, ts.compactionAlreadyAttempted() already true) -- §7.2 point
// 3's own "the original deferred error is what reaches deriveOutcome...
// never a silent double failure" requirement: without this, the final
// reason string would read identically to a first-time, never-retried
// overflow, giving an operator no way to tell a recovery WAS attempted.
// retryReason is the retried turn's OWN outcome.Reason (not the original
// turn's) -- by the time this is called, that IS the current failure worth
// naming, with the added context that a compaction retry already happened
// once this turn and is not going to happen again (§7.2's own at-most-once
// guard).
func enrichReasonForRepeatedOverflow(retryReason *string) string {
	reason := "opencode: ContextOverflowError"
	if retryReason != nil {
		reason = *retryReason
	}
	return fmt.Sprintf("%s (a compaction retry was already attempted this turn; the retried prompt also overflowed)", reason)
}

// enrichReasonForFailedTransientRetry is enrichReasonForFailedRecovery's own
// sibling for this Step's own transient-APIError retry
// (Adapter.attemptTransientRetry, adapter.go): built when the retry attempt
// itself failed — the retried postPromptAsync dispatch errored at the
// transport level, or the bounded backoff wait was interrupted by context
// cancellation. A DELIBERATE SEPARATE function, not a reuse of
// enrichReasonForFailedRecovery above with a relabeled detail string: this
// failure class never forces a compaction (see attemptTransientRetry's own
// doc comment) — reusing that function's own fixed "compaction retry
// attempted and failed" wording verbatim would misdescribe what actually
// happened to an operator reading the final reason string. Mirrors its
// sibling's own "name BOTH facts" shape exactly: the original transient
// error's own reason, AND that a retry was attempted and ALSO failed.
func enrichReasonForFailedTransientRetry(originalReason *string, detail string) string {
	original := "opencode: APIError"
	if originalReason != nil {
		original = *originalReason
	}
	return fmt.Sprintf("%s (transient-error retry attempted and failed: %s)", original, detail)
}

// enrichReasonForRepeatedTransientFailure is enrichReasonForRepeatedOverflow's
// own sibling for this Step's own transient-APIError retry: built when the
// RETRIED prompt also failed with another transient APIError
// (Adapter.finalizeOrRecoverFromOverflow, adapter.go,
// ts.compactionAlreadyAttempted() already true — the SAME shared one-way
// latch §7.2's own compaction retry uses, reused here rather than a
// parallel guard). Worded for this failure class instead of reusing
// enrichReasonForRepeatedOverflow's own "also overflowed" text, which would
// simply be false here — mirrors its sibling's own "never a silent double
// failure" rationale exactly.
func enrichReasonForRepeatedTransientFailure(retryReason *string) string {
	reason := "opencode: APIError"
	if retryReason != nil {
		reason = *retryReason
	}
	return fmt.Sprintf("%s (a transient-error retry was already attempted this turn; the retried prompt also failed)", reason)
}

// subTaskOutcome maps an already-derived turn-level outcome onto
// SubTaskFinish's own (separately generated, but value-identical) outcome
// enum — see turn.go's own doc comment on why this adapter closes a
// still-open sub-task using the ENCLOSING turn's outcome rather than a
// truly isolated signal (a documented best-effort simplification, §7.1).
func subTaskOutcome(o sandboxws.ExecutionCompleteOutcome) sandboxws.SubTaskFinishOutcome {
	switch o {
	case sandboxws.ExecutionCompleteOutcomeCancelled:
		return sandboxws.SubTaskFinishOutcomeCancelled
	case sandboxws.ExecutionCompleteOutcomeFailed:
		return sandboxws.SubTaskFinishOutcomeFailed
	default:
		return sandboxws.SubTaskFinishOutcomeCompleted
	}
}
