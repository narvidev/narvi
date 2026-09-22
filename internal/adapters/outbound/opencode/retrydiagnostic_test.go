package opencode

import (
	"context"
	"testing"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// This file tests THREE of the five turnOutcome{} reconstruction sites
// that only enrich Reason once §7.2's own retries are exhausted, and that
// must still carry the ORIGINAL failure's own Diagnostic forward -- see
// turnOutcome.Diagnostic's own doc comment (outcome.go) for the full list
// of five. Getting this wrong is exactly the failure mode this Step
// exists to fix: §7.3's own framing is "once the retries are exhausted",
// and a diagnostic that silently vanishes at exactly that moment is worse
// than one that was never built at all, since nothing about the final
// execution_complete would even hint it once existed.
//
// Audit fix (C1): an EARLIER version of this comment claimed this file
// covered "the full list of reconstruction sites" -- false as written. It
// never covered attemptCompactionRetry's or attemptTransientRetry's own
// "the RETRIED postPromptAsync dispatch itself fails" branches (the other
// two of the five), and could not: each test below reaches its target
// branch by pointing the WHOLE Adapter at one unreachable baseURL, so
// forceCompaction/the backoff wait AND the retried postPromptAsync all
// fail identically near-instantly -- there is no way, with one baseURL,
// to make the RECOVERY step (forceCompaction/the backoff) succeed while
// only the RETRY's own re-dispatch fails. Those two sites are covered
// instead, with a real fake server that can fail each step independently,
// by compactionretry_test.go's own TestCompactionRetry_RetryPostPromptAsyncFails
// and transientretry_test.go's own TestTransientRetry_RetryDispatchFailsIsNeverRetriedAgain.
//
// Each test below reaches its target branch directly (no live OpenCode
// server, no waiting out a real backoff) -- an unreachable baseURL makes
// forceCompaction/the retried postPromptAsync fail near-instantly, and an
// already-canceled context makes waitTransientRetryBackoff return
// immediately -- so these stay fast and deterministic.

// wantTestDiagnostic builds a fixed, recognizable ProviderFailureDiagnostic
// every test below plants as the "original" outcome's own Diagnostic, and
// then asserts survives into the finalized execution_complete unchanged.
func wantTestDiagnostic(unionMember string) *ProviderFailureDiagnostic {
	return &ProviderFailureDiagnostic{
		UnionMember:    unionMember,
		Model:          "anthropic/claude-sonnet-4-5",
		RuntimeVersion: testRuntimeVersion,
		SandboxID:      testSandboxID,
	}
}

// requireDiagnosticSurvived asserts final carries EXACTLY the diagnostic
// wantTestDiagnostic built -- every field, not just one, so a partial
// carry-forward (e.g. a future refactor that rebuilds Diagnostic from
// scratch instead of copying the original) is caught too.
func requireDiagnosticSurvived(t *testing.T, final sandboxws.ExecutionComplete, wantUnionMember string) {
	t.Helper()
	if final.Diagnostic == nil {
		t.Fatal("execution_complete.Diagnostic = nil, want the original diagnostic carried forward")
	}
	if final.Diagnostic.UnionMember == nil || *final.Diagnostic.UnionMember != wantUnionMember {
		t.Errorf("Diagnostic.UnionMember = %v, want %q", final.Diagnostic.UnionMember, wantUnionMember)
	}
	if final.Diagnostic.Model == nil || *final.Diagnostic.Model != "anthropic/claude-sonnet-4-5" {
		t.Errorf("Diagnostic.Model = %v, want %q", final.Diagnostic.Model, "anthropic/claude-sonnet-4-5")
	}
	if final.Diagnostic.RuntimeVersion == nil || *final.Diagnostic.RuntimeVersion != testRuntimeVersion {
		t.Errorf("Diagnostic.RuntimeVersion = %v, want %q", final.Diagnostic.RuntimeVersion, testRuntimeVersion)
	}
	if final.Diagnostic.SandboxId == nil || *final.Diagnostic.SandboxId != testSandboxID {
		t.Errorf("Diagnostic.SandboxId = %v, want %q", final.Diagnostic.SandboxId, testSandboxID)
	}
}

// TestFinalizeOrRecoverFromOverflow_AlreadyAttempted_CarriesDiagnosticForward
// covers finalizeOrRecoverFromOverflow's own overflowActionAlreadyAttempted
// branch (adapter.go): a recovery retry was already fully attempted for
// this turn, so THIS session.idle's own freshly-derived outcome (already
// carrying its own Diagnostic, built by the sse.go/finalizeByFallback call
// sites) must reach a.finalize with that Diagnostic intact, only Reason
// rewritten.
func TestFinalizeOrRecoverFromOverflow_AlreadyAttempted_CarriesDiagnosticForward(t *testing.T) {
	t.Parallel()

	a := &Adapter{}
	collector := &eventCollector{}
	ts := newTurnState(sandboxws.Prompt{SessionId: testSessionID, Gen: 1}, collector.sink, "anthropic/claude-sonnet-4-5")
	// Simulate "a recovery retry was already fully attempted for this
	// turn" directly on ts -- the exact precondition
	// resolveOverflowAction's own overflowActionAlreadyAttempted branch
	// checks (turn.go).
	ts.compactionAttempted = true
	ts.attemptedKind = recoveryKindCompaction

	reason := "opencode: ContextOverflowError"
	outcome := turnOutcome{
		Outcome:    sandboxws.ExecutionCompleteOutcomeFailed,
		Reason:     &reason,
		Diagnostic: wantTestDiagnostic("ContextOverflowError"),
	}
	err := &openCodeTaggedError{Name: "ContextOverflowError"}

	a.finalizeOrRecoverFromOverflow(testSessionID, ts, outcome, err, time.Now())

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeFailed {
		t.Errorf("Outcome = %q, want %q", final.Outcome, sandboxws.ExecutionCompleteOutcomeFailed)
	}
	requireDiagnosticSurvived(t, final, "ContextOverflowError")
}

// TestAttemptCompactionRetry_ForceCompactionFails_CarriesDiagnosticForward
// covers attemptCompactionRetry's own compactionErr != nil branch
// (adapter.go): forceCompaction (POST /session/{id}/summarize) itself
// failed, before the retry prompt was ever dispatched -- ts's own current
// error state still holds the ORIGINAL overflow, but only originalOutcome
// (captured before this attempt began) carries that error's own
// Diagnostic, so it must be the one copied forward, not re-derived from
// ts.
func TestAttemptCompactionRetry_ForceCompactionFails_CarriesDiagnosticForward(t *testing.T) {
	t.Parallel()

	// An unreachable baseURL: forceCompaction's own POST fails near-
	// instantly (connection refused), no live server needed.
	a := New("http://127.0.0.1:1", testSSEInactivityTimeout, testReconnectInterval,
		testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff,
		testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	collector := &eventCollector{}
	ts := newTurnState(sandboxws.Prompt{SessionId: testSessionID, Gen: 1}, collector.sink, "anthropic/claude-sonnet-4-5")

	reason := "opencode: ContextOverflowError"
	originalOutcome := turnOutcome{
		Outcome:    sandboxws.ExecutionCompleteOutcomeFailed,
		Reason:     &reason,
		Diagnostic: wantTestDiagnostic("ContextOverflowError"),
	}

	a.attemptCompactionRetry(context.Background(), testSessionID, ts, originalOutcome)

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeFailed {
		t.Errorf("Outcome = %q, want %q", final.Outcome, sandboxws.ExecutionCompleteOutcomeFailed)
	}
	requireDiagnosticSurvived(t, final, "ContextOverflowError")
}

// TestAttemptTransientRetry_BackoffInterrupted_CarriesDiagnosticForward
// covers attemptTransientRetry's own "backoff wait was interrupted"
// branch (adapter.go): no retry was ever dispatched on this branch
// either, so originalOutcome (not ts's own current error state) is again
// the right source -- mirrors the compaction test above for this Step's
// own SIBLING recovery kind (§7.2's "widened to also cover a first-time
// transient APIError" point).
func TestAttemptTransientRetry_BackoffInterrupted_CarriesDiagnosticForward(t *testing.T) {
	t.Parallel()

	a := New("http://127.0.0.1:1", testSSEInactivityTimeout, testReconnectInterval,
		testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff,
		testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	collector := &eventCollector{}
	ts := newTurnState(sandboxws.Prompt{SessionId: testSessionID, Gen: 1}, collector.sink, "anthropic/claude-sonnet-4-5")

	reason := "opencode: APIError"
	originalOutcome := turnOutcome{
		Outcome:    sandboxws.ExecutionCompleteOutcomeFailed,
		Reason:     &reason,
		Diagnostic: wantTestDiagnostic("APIError"),
	}

	// Already-canceled: waitTransientRetryBackoff returns ctx.Err()
	// immediately, no need to wait out a real backoff.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a.attemptTransientRetry(ctx, testSessionID, ts, originalOutcome)

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeFailed {
		t.Errorf("Outcome = %q, want %q", final.Outcome, sandboxws.ExecutionCompleteOutcomeFailed)
	}
	requireDiagnosticSurvived(t, final, "APIError")
}
