package opencode

import (
	"context"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// THE PRIMARY regression test suite for this Step ("typed transient-error
// retry for the OpenCode adapter"): unlike realturn_test.go's own
// skipIfNoProvider-gated tests, these deliberately run against
// fakeOpenCodeServer (fake_server_test.go) so CI can exercise the whole
// transient-retry round trip reliably, with no real AI provider needed --
// mirroring compactionretry_test.go's own established style exactly (same
// fake server, same waitForCount/sseLine/eventCollector helpers), scoped to
// this Step's OWN failure class instead of ContextOverflowError.
//
// apiErrorMessageUpdated builds the exact SSE line every test below
// broadcasts -- an assistant message.updated carrying a real, live-verified
// APIError shape (openCodeTaggedError.Data, types.go): retryable controls
// Data.IsRetryable, the ONLY field this package's own classification
// (isTransientAPIError, outcome.go) ever consults.
func apiErrorMessageUpdated(t *testing.T, sessionID, messageID string, retryable bool) string {
	t.Helper()
	return sseLine(t, "message.updated", messageUpdatedProps{
		SessionID: sessionID,
		Info: openCodeMessageInfo{
			ID:   messageID,
			Role: "assistant",
			Error: &openCodeTaggedError{
				Name: "APIError",
				Data: &openCodeErrorData{IsRetryable: retryable},
			},
		},
	})
}

// apiErrorMessageUpdatedWithModel mirrors apiErrorMessageUpdated above, but
// also plants the engine-reported ModelID/ProviderID openCodeMessageInfo
// now carries (§7.3, A2) -- used by tests that specifically assert
// ProviderFailureDiagnostic.Model, to prove it is sourced from the
// message's OWN reported model, never from cmd.Model or any request-side
// resolution.
func apiErrorMessageUpdatedWithModel(t *testing.T, sessionID, messageID string, retryable bool, providerID, modelID string) string {
	t.Helper()
	return sseLine(t, "message.updated", messageUpdatedProps{
		SessionID: sessionID,
		Info: openCodeMessageInfo{
			ID:   messageID,
			Role: "assistant",
			Error: &openCodeTaggedError{
				Name: "APIError",
				Data: &openCodeErrorData{IsRetryable: retryable},
			},
			ModelID:    modelID,
			ProviderID: providerID,
		},
	})
}

// TestTransientRetry_SucceedsAfterTransientAPIError proves the full round
// trip for the "transient -> retried" table case this Step's own
// instructions require: a transient (isRetryable=true) APIError on the
// original prompt's own assistant message, followed by session.idle for the
// SAME session, must trigger NO /summarize call at all (unlike §7.2's own
// context-overflow recovery, this failure class needs no compaction), then
// exactly one retried POST .../prompt_async call with the SAME prompt text,
// then a clean retry completion -- final execution_complete is Completed,
// never Failed.
func TestTransientRetry_SucceedsAfterTransientAPIError(t *testing.T) {
	f := newFakeOpenCodeServer(t)

	a := New(f.URL(), testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	connCtx, connCancel := context.WithTimeout(context.Background(), testWait)
	defer connCancel()
	if err := a.Connected(connCtx); err != nil {
		t.Fatalf("Connected() error = %v", err)
	}
	waitForConnNumber(t, f, 1)

	collector := &eventCollector{}
	promptText := "do something that will hit a transient provider blip"
	cmd := sandboxws.Prompt{
		Type: "prompt", MessageId: "m1", SessionId: "sess-transient-1", Gen: 1,
		Text: promptText,
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	ts := waitForTurnRegistered(t, a, "ses_fake")

	// The original turn's own assistant message reports a transient
	// APIError, then the turn goes idle.
	f.broadcast(apiErrorMessageUpdated(t, "ses_fake", "msg_original", true))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	// Exactly one RETRIED prompt_async call for the SAME session with the
	// SAME prompt text (promptCalls[0] is the ORIGINAL dispatch from
	// StartTurn itself; [1] is the retry) -- and, unlike the compaction
	// path, this must happen WITHOUT ever calling /summarize at all.
	waitForCount(t, "promptCallCount", f.promptCallCount, 2)
	if got := f.lastPromptText(); got != promptText {
		t.Errorf("retried prompt text = %q, want %q (the EXACT same prompt)", got, promptText)
	}
	if got := f.summarizeCallCount(); got != 0 {
		t.Errorf("summarizeCallCount = %d, want exactly 0 (a transient-error retry forces no compaction)", got)
	}

	// A stray error must not have leaked through (clearErrorsForRetry).
	if err := ts.errorForOutcome(); err != nil {
		t.Errorf("ts.errorForOutcome() = %+v, want nil (clearErrorsForRetry should have cleared it before the retry)", err)
	}
	if ts.attemptedRecoveryKind() != recoveryKindTransientAPI {
		t.Errorf("ts.attemptedRecoveryKind() = %v, want recoveryKindTransientAPI", ts.attemptedRecoveryKind())
	}

	// Deterministically wait for ts.compacting to have actually cleared
	// before broadcasting the retry's own real completion -- mirroring
	// compactionretry_test.go's own established waitForNotCompacting
	// precedent (e.g.
	// TestCompactionRetry_LateCompactionTailEventDuringRetryDispatchIsSuppressed):
	// waitForCount above only proves the fake server's own handler recorded
	// the retry's own prompt_async call, strictly EARLIER than the adapter's
	// own client-side postPromptAsync call actually returning and clearing
	// ts.compacting (attemptTransientRetry mirrors attemptCompactionRetry's
	// own §7.2 Finding 3 ordering exactly, adapter.go) -- broadcasting
	// immediately after waitForCount would race dispatchEvent's own
	// isCompacting guard (sse.go) into silently and permanently dropping the
	// retry's own completion.
	waitForNotCompacting(t, f, ts)

	// Now script the RETRY's own clean completion.
	f.broadcast(plainAssistantMessageUpdated(t, "ses_fake", "msg_retry"))
	f.broadcast(assistantTextPart(t, "ses_fake", "msg_retry", "prt_retry", "all good now"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeCompleted {
		reason := "<nil>"
		if final.Reason != nil {
			reason = *final.Reason
		}
		t.Errorf("execution_complete.Outcome = %q, want %q (reason=%s)", final.Outcome, sandboxws.ExecutionCompleteOutcomeCompleted, reason)
	}
}

// TestTransientRetry_PermanentAPIErrorNeverRetried proves the "permanent ->
// failed immediately" table case this Step's own instructions require: an
// APIError with isRetryable=false must finalize as Failed on the FIRST
// occurrence, with no retry attempt at all -- classification happens on the
// typed field alone, never a substring of any error text.
//
// Also this package's own §7.3 reachability + default-configuration proof
// (audit fix, C1/C2): cmd below never sets Model at all -- the SAME "no
// modelId" shape Composer/PlanModeView/Timeline resume/DecisionInbox all
// dispatch by default (web/src/session) -- and this is the FIRST occurrence
// of the error (dispatchEvent's own "session.idle" case, sse.go:289, the
// live call site that builds ProviderFailureDiagnostic in production; no
// retry machinery is involved at all for a permanent APIError). Before
// this fix: deleting sse.go's own `outcome.Diagnostic =
// a.buildProviderFailureDiagnostic(err, ts)` line left every existing test
// in this package green, and even with that line intact,
// ProviderFailureDiagnostic.Model read "" on exactly this default,
// no-modelId path -- §7.3's own first-named-missing fact ("not the model
// that ran") stayed missing. Both are pinned below.
func TestTransientRetry_PermanentAPIErrorNeverRetried(t *testing.T) {
	f := newFakeOpenCodeServer(t)

	a := New(f.URL(), testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	connCtx, connCancel := context.WithTimeout(context.Background(), testWait)
	defer connCancel()
	if err := a.Connected(connCtx); err != nil {
		t.Fatalf("Connected() error = %v", err)
	}
	waitForConnNumber(t, f, 1)

	collector := &eventCollector{}
	cmd := sandboxws.Prompt{
		Type: "prompt", MessageId: "m1", SessionId: "sess-permanent-1", Gen: 1,
		Text: "do something that will hit a permanent (non-retryable) provider error",
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	waitForTurnRegistered(t, a, "ses_fake")

	// apiErrorMessageUpdatedWithModel, not the bare apiErrorMessageUpdated:
	// cmd.Model is never set on this turn (the default configuration), so
	// the wire request correctly omits "model" entirely (resolveModel,
	// session.go, §7.3 A1) -- this planted ModelID/ProviderID is the ONLY
	// source left for ProviderFailureDiagnostic.Model to read from (A2),
	// simulating OpenCode itself picking its own configured default and
	// reporting it back on the resulting assistant message.
	f.broadcast(apiErrorMessageUpdatedWithModel(t, "ses_fake", "msg_original", false, "anthropic", "claude-sonnet-4-5"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeFailed {
		t.Errorf("execution_complete.Outcome = %q, want %q", final.Outcome, sandboxws.ExecutionCompleteOutcomeFailed)
	}
	if final.Reason == nil || !strings.Contains(*final.Reason, "APIError") {
		reason := "<nil>"
		if final.Reason != nil {
			reason = *final.Reason
		}
		t.Errorf("execution_complete.Reason = %q, want it to name APIError", reason)
	}

	// §7.3: the diagnostic must actually be reached in production (C1) --
	// deleting sse.go's own call site leaves this nil -- and, on this
	// DEFAULT, no-modelId configuration, Model must name the model the
	// ENGINE itself reported for this message (A2), never "".
	if final.Diagnostic == nil {
		t.Fatal("execution_complete.Diagnostic = nil, want the allowlisted provider-failure record " +
			"(sse.go's own dispatchEvent call site was never reached)")
	}
	if final.Diagnostic.UnionMember == nil || *final.Diagnostic.UnionMember != "APIError" {
		t.Errorf("Diagnostic.UnionMember = %v, want %q", final.Diagnostic.UnionMember, "APIError")
	}
	wantModel := "anthropic/claude-sonnet-4-5"
	if final.Diagnostic.Model == nil || *final.Diagnostic.Model != wantModel {
		t.Errorf("Diagnostic.Model = %v, want %q (the message's own engine-reported model, even though "+
			"cmd.Model was never set on this turn -- the default configuration every real client "+
			"dispatches through) -- a wrong or empty Model here is §7.3's own headline fact still missing",
			final.Diagnostic.Model, wantModel)
	}
	if final.Diagnostic.RuntimeVersion == nil || *final.Diagnostic.RuntimeVersion != testRuntimeVersion {
		t.Errorf("Diagnostic.RuntimeVersion = %v, want %q", final.Diagnostic.RuntimeVersion, testRuntimeVersion)
	}
	if final.Diagnostic.SandboxId == nil || *final.Diagnostic.SandboxId != testSandboxID {
		t.Errorf("Diagnostic.SandboxId = %v, want %q", final.Diagnostic.SandboxId, testSandboxID)
	}

	// No retry: exactly the original dispatch, nothing more.
	if got := f.promptCallCount(); got != 1 {
		t.Errorf("promptCallCount = %d, want exactly 1 (a permanent APIError must never be retried)", got)
	}
	if got := f.summarizeCallCount(); got != 0 {
		t.Errorf("summarizeCallCount = %d, want exactly 0", got)
	}

	// audit fix (§7.3, A1): the wire request itself must OMIT "model"
	// entirely on this default, no-modelId configuration -- restoring
	// origin/main's own behavior (resolveModel, session.go), which an
	// earlier version of this package regressed by forcing
	// resolveModelForced's own fallback onto EVERY dispatch, changing
	// which model every default turn actually ran on in production. The
	// diagnostic's own Model above is populated a different way (the
	// engine's own report), so there is no longer any reason -- and no
	// longer any correct behavior -- for this adapter to invent a model
	// on the wire just to know one afterward.
	f.mu.Lock()
	promptModel := f.lastPromptModel
	f.mu.Unlock()
	if promptModel != nil {
		t.Errorf("fake server's own prompt_async request carried model=%+v, want the field omitted "+
			"entirely (cmd.Model was nil) -- forcing one changes PRODUCTION behavior on every default "+
			"turn just to populate a diagnostic string", promptModel)
	}
}

// TestTransientRetry_RetryAlsoFailsFinalizesFailedExactlyOnce mirrors
// TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce for this
// Step's own failure class: the RETRIED prompt also hits a transient
// APIError -- this Step's own explicit "one-way latch... a turn cannot loop
// indefinitely" requirement means this must finalize Failed, enriched to
// say a retry was already attempted, with NO second retry launched.
func TestTransientRetry_RetryAlsoFailsFinalizesFailedExactlyOnce(t *testing.T) {
	f := newFakeOpenCodeServer(t)

	a := New(f.URL(), testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	connCtx, connCancel := context.WithTimeout(context.Background(), testWait)
	defer connCancel()
	if err := a.Connected(connCtx); err != nil {
		t.Fatalf("Connected() error = %v", err)
	}
	waitForConnNumber(t, f, 1)

	collector := &eventCollector{}
	cmd := sandboxws.Prompt{
		Type: "prompt", MessageId: "m1", SessionId: "sess-transient-2", Gen: 1,
		Text: "will hit a transient blip twice in a row",
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	ts := waitForTurnRegistered(t, a, "ses_fake")

	// First transient error -- triggers the one and only retry.
	f.broadcast(apiErrorMessageUpdated(t, "ses_fake", "msg_original", true))
	f.broadcast(sessionIdleLine(t, "ses_fake"))
	waitForCount(t, "promptCallCount", f.promptCallCount, 2)

	// Deterministically wait for ts.compacting to have actually cleared
	// before broadcasting the RETRIED prompt's own second transient error
	// below -- see TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce's
	// own doc comment (compactionretry_test.go) for the exact race this
	// closes: waitForCount above only proves the fake server's own handler
	// recorded the retry's own prompt_async call, strictly EARLIER than the
	// adapter's own client-side postPromptAsync call actually returning and
	// clearing ts.compacting -- broadcasting immediately after it would race
	// dispatchEvent's own isCompacting guard (sse.go) into silently and
	// PERMANENTLY dropping this event (there is no replay), leaving nothing
	// to finalize this turn within the test's own testWait ctx budget.
	waitForNotCompacting(t, f, ts)

	// The RETRIED prompt ALSO hits a transient APIError.
	f.broadcast(apiErrorMessageUpdated(t, "ses_fake", "msg_retry", true))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeFailed {
		t.Errorf("execution_complete.Outcome = %q, want %q", final.Outcome, sandboxws.ExecutionCompleteOutcomeFailed)
	}
	if final.Reason == nil || !strings.Contains(*final.Reason, "already attempted") {
		reason := "<nil>"
		if final.Reason != nil {
			reason = *final.Reason
		}
		t.Errorf("execution_complete.Reason = %q, want it to mention a retry was already attempted", reason)
	}
	if final.Reason != nil && strings.Contains(*final.Reason, "overflowed") {
		t.Errorf("execution_complete.Reason = %q, want it NOT to claim the prompt overflowed (this is the "+
			"transient-error retry, not the compaction one)", *final.Reason)
	}

	// No THIRD prompt_async call -- the shared one-way latch must have
	// prevented a second retry attempt (this Step's own explicit
	// "bounded retry budget... a turn cannot loop indefinitely"
	// requirement).
	if got := f.promptCallCount(); got != 2 {
		t.Errorf("promptCallCount = %d, want exactly 2 (original + the one retry, no more)", got)
	}
	if got := f.summarizeCallCount(); got != 0 {
		t.Errorf("summarizeCallCount = %d, want exactly 0", got)
	}
}

// TestTransientRetry_RetryDispatchFailsIsNeverRetriedAgain proves the
// "local-connection -> failed immediately, never retried" table case this
// Step's own instructions require: the RETRY's own re-dispatch (the second
// POST .../prompt_async call) itself fails at the transport level -- a
// plain Go error, never a decoded openCodeTaggedError, exactly what a
// failure to reach OpenCode's own LOCAL HTTP server looks like (client.go's
// doJSON; see isTransientAPIError's own doc comment, outcome.go, for why
// this class of failure structurally never reaches this package's typed
// classification at all). This must finalize Failed, enriched to name the
// failed retry dispatch, and — critically — must NOT attempt a THIRD
// prompt_async call: the one-way latch this Step reuses from §7.2 (ts.
// compactionAttempted/ts.compacting, turn.go) bounds the retry budget to
// exactly one attempt regardless of how that one attempt itself fails,
// so a crashed local OpenCode process is surfaced as a failure, never
// silently hidden behind a retry loop.
//
// Also §7.3's own reachability proof for adapter.go's attemptTransientRetry's
// own `a.finalize(ts, turnOutcome{..., Diagnostic: originalOutcome.Diagnostic})`
// call in this exact branch -- audit fix (C1), the SIBLING of
// TestCompactionRetry_RetryPostPromptAsyncFails' own identical addition
// (compactionretry_test.go) -- see retrydiagnostic_test.go's own corrected
// doc comment for why these two tests, not that file, are where this pair
// of reconstruction sites is actually covered.
func TestTransientRetry_RetryDispatchFailsIsNeverRetriedAgain(t *testing.T) {
	f := newFakeOpenCodeServer(t)

	a := New(f.URL(), testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	connCtx, connCancel := context.WithTimeout(context.Background(), testWait)
	defer connCancel()
	if err := a.Connected(connCtx); err != nil {
		t.Fatalf("Connected() error = %v", err)
	}
	waitForConnNumber(t, f, 1)

	collector := &eventCollector{}
	cmd := sandboxws.Prompt{
		Type: "prompt", MessageId: "m1", SessionId: "sess-transient-3", Gen: 1,
		Text: "transient blip; the retry's own re-dispatch fails locally",
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	waitForTurnRegistered(t, a, "ses_fake")

	// The ORIGINAL dispatch (call #1) must keep succeeding -- only arm the
	// failure AFTER it has already gone out (setPromptAsyncOK's own field
	// comment, fake_server_test.go), or the transient-error scenario this
	// test needs could never even get triggered in the first place.
	waitForCount(t, "promptCallCount", f.promptCallCount, 1)
	f.setPromptAsyncOK(false)

	// apiErrorMessageUpdatedWithModel, not the bare apiErrorMessageUpdated:
	// cmd.Model is never set on this turn, so the wire request correctly
	// omits "model" (resolveModel, session.go, §7.3 A1) -- this planted
	// ModelID/ProviderID is the ONLY source left for
	// ProviderFailureDiagnostic.Model, matching the assertion below.
	f.broadcast(apiErrorMessageUpdatedWithModel(t, "ses_fake", "msg_original", true, "anthropic", "claude-sonnet-4-5"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	waitForCount(t, "promptCallCount", f.promptCallCount, 2)

	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeFailed {
		t.Errorf("execution_complete.Outcome = %q, want %q", final.Outcome, sandboxws.ExecutionCompleteOutcomeFailed)
	}
	if final.Reason == nil {
		t.Fatal("execution_complete.Reason = nil, want a non-nil enriched reason")
	}
	reason := *final.Reason
	if !strings.Contains(reason, "APIError") {
		t.Errorf("execution_complete.Reason = %q, want it to name the ORIGINAL transient error too", reason)
	}
	if !strings.Contains(reason, "transient-error retry attempted and failed") {
		t.Errorf("execution_complete.Reason = %q, want it to mention the transient-error retry itself failed", reason)
	}
	if !strings.Contains(reason, "retry postPromptAsync") {
		t.Errorf("execution_complete.Reason = %q, want it to name retry postPromptAsync as the failed step", reason)
	}

	// §7.3: the ORIGINAL transient error's own Diagnostic must survive
	// this reconstruction -- attemptTransientRetry rebuilds a fresh
	// turnOutcome{} here (only Reason is enriched), and
	// turnOutcome.Diagnostic's own doc comment (outcome.go) requires every
	// such reconstruction to carry Diagnostic forward unchanged. Model
	// comes from the ORIGINAL message's own engine-reported
	// ModelID/ProviderID above, not from cmd.Model (never set on this
	// turn) or any request-side resolution.
	if final.Diagnostic == nil {
		t.Fatal("execution_complete.Diagnostic = nil, want the ORIGINAL transient error's own diagnostic carried forward")
	}
	if final.Diagnostic.UnionMember == nil || *final.Diagnostic.UnionMember != "APIError" {
		t.Errorf("Diagnostic.UnionMember = %v, want %q", final.Diagnostic.UnionMember, "APIError")
	}
	if want := "anthropic/claude-sonnet-4-5"; final.Diagnostic.Model == nil || *final.Diagnostic.Model != want {
		t.Errorf("Diagnostic.Model = %v, want %q (the original message's own engine-reported model)", final.Diagnostic.Model, want)
	}
	if final.Diagnostic.RuntimeVersion == nil || *final.Diagnostic.RuntimeVersion != testRuntimeVersion {
		t.Errorf("Diagnostic.RuntimeVersion = %v, want %q", final.Diagnostic.RuntimeVersion, testRuntimeVersion)
	}
	if final.Diagnostic.SandboxId == nil || *final.Diagnostic.SandboxId != testSandboxID {
		t.Errorf("Diagnostic.SandboxId = %v, want %q", final.Diagnostic.SandboxId, testSandboxID)
	}

	// No summarize call (this failure class forces no compaction), and no
	// THIRD prompt_async call: the retry's own dispatch failure must not
	// spawn yet another attempt.
	if got := f.summarizeCallCount(); got != 0 {
		t.Errorf("summarizeCallCount = %d, want exactly 0", got)
	}
	if got := f.promptCallCount(); got != 2 {
		t.Errorf("promptCallCount = %d, want exactly 2 (original + the one failed retry, no more)", got)
	}
}

// TestTransientRetry_SharesOneShotBudgetWithCompactionRetry is this Step's
// own design-level proof that it "reuses the EXISTING stash/latch retry
// machinery... rather than building a parallel one" (the task's own
// explicit instruction): a turn whose first-time transient APIError already
// consumed the shared one-way latch (ts.compactionAttempted/ts.compacting,
// turn.go), and whose RETRIED prompt then hits a ContextOverflowError
// instead of another transient error, must NOT get a second recovery
// attempt of the OTHER kind either -- no /summarize call, no third
// prompt_async call -- and the final reason must honestly describe the
// retry this turn ACTUALLY got (transient-error), never misdescribe it as
// a compaction retry just because the CURRENT (second) failure happens to
// be a ContextOverflowError.
func TestTransientRetry_SharesOneShotBudgetWithCompactionRetry(t *testing.T) {
	f := newFakeOpenCodeServer(t)
	f.setSummarizeOK(true) // would succeed if (wrongly) called at all

	a := New(f.URL(), testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	connCtx, connCancel := context.WithTimeout(context.Background(), testWait)
	defer connCancel()
	if err := a.Connected(connCtx); err != nil {
		t.Fatalf("Connected() error = %v", err)
	}
	waitForConnNumber(t, f, 1)

	collector := &eventCollector{}
	cmd := sandboxws.Prompt{
		Type: "prompt", MessageId: "m1", SessionId: "sess-mixed-1", Gen: 1,
		Text: "transient blip first, then the retry overflows instead",
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	ts := waitForTurnRegistered(t, a, "ses_fake")

	// First-time transient APIError -- consumes the shared latch via the
	// transient-retry path (no compaction).
	f.broadcast(apiErrorMessageUpdated(t, "ses_fake", "msg_original", true))
	f.broadcast(sessionIdleLine(t, "ses_fake"))
	waitForCount(t, "promptCallCount", f.promptCallCount, 2)
	if got := ts.attemptedRecoveryKind(); got != recoveryKindTransientAPI {
		t.Fatalf("ts.attemptedRecoveryKind() = %v, want recoveryKindTransientAPI (test setup check)", got)
	}

	// Deterministically wait for ts.compacting to have actually cleared
	// before broadcasting the RETRIED prompt's own second failure below --
	// see TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce's
	// own doc comment (compactionretry_test.go) for the exact race this
	// closes: waitForCount above only proves the retry's own prompt_async
	// call was recorded server-side, strictly EARLIER than ts.compacting
	// actually clearing client-side.
	waitForNotCompacting(t, f, ts)

	// The RETRIED prompt overflows instead of hitting another transient
	// error -- must NOT trigger a compaction retry: the shared latch is
	// already spent.
	f.broadcast(overflowMessageUpdated(t, "ses_fake", "msg_retry"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeFailed {
		t.Errorf("execution_complete.Outcome = %q, want %q", final.Outcome, sandboxws.ExecutionCompleteOutcomeFailed)
	}
	if final.Reason == nil {
		t.Fatal("execution_complete.Reason = nil, want a non-nil enriched reason")
	}
	reason := *final.Reason
	if !strings.Contains(reason, "already attempted") {
		t.Errorf("execution_complete.Reason = %q, want it to mention a retry was already attempted", reason)
	}
	// Honesty check: this turn's one shot was ACTUALLY spent on the
	// transient-error retry, not a compaction one -- the reason must say
	// so, never "also overflowed" (which would misdescribe what happened:
	// no compaction retry was ever attempted this turn at all).
	if strings.Contains(reason, "overflowed") {
		t.Errorf("execution_complete.Reason = %q, want it NOT to say \"overflowed\" -- this turn's one recovery "+
			"attempt was a transient-error retry, never a compaction one, even though the SECOND failure "+
			"happens to be a ContextOverflowError", reason)
	}

	// Exactly one recovery attempt total: no /summarize call ever (a
	// compaction retry must never have been launched), and no third
	// prompt_async call.
	if got := f.summarizeCallCount(); got != 0 {
		t.Errorf("summarizeCallCount = %d, want exactly 0 (no compaction retry should ever have been launched)", got)
	}
	if got := f.promptCallCount(); got != 2 {
		t.Errorf("promptCallCount = %d, want exactly 2 (original + the one transient-error retry, no more)", got)
	}
}

// TestCompactionRetry_SharesOneShotBudgetWithTransientRetry is the mirror
// image of TestTransientRetry_SharesOneShotBudgetWithCompactionRetry above:
// a turn whose first-time ContextOverflowError already consumed the shared
// latch via the COMPACTION path, whose retried prompt then hits a transient
// APIError instead, must not get a second recovery attempt either -- and
// the final reason must honestly describe a COMPACTION retry having
// already been attempted (never "a transient-error retry"), since that IS
// what this turn actually got.
//
// Gates the compaction retry's own re-dispatch (call #2,
// armPromptAsyncGateForCall) -- this test is one of the two CI-observed
// flakes (alongside compactionretry_test.go's own
// TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce, see its
// own doc comment for the full root-cause writeup) that motivated closing
// this whole class of race: an ungated compaction retry gives the
// SSE-reader goroutine no deliberately-extended grace period to drain the
// /summarize handler's own compaction-success wave before ts.compacting
// clears, so a severely descheduled reader can process that wave's own
// tail AFTER isCompacting() has already gone false, misreading its internal
// completion as this turn's real one. waitForNotCompacting alone (even with
// its own added waitForDrained half) cannot close this -- the damage
// happens AS PART OF the same belated drain a barrier can only confirm
// after the fact. Gating call #2 and calling waitForDrained BEFORE
// releasing it proves the wave was drained WHILE ts.compacting was
// PROVABLY still true, closing the race by construction rather than
// racing wall-clock timing.
func TestCompactionRetry_SharesOneShotBudgetWithTransientRetry(t *testing.T) {
	f := newFakeOpenCodeServer(t)
	f.setSummarizeOK(true)
	gate := f.armPromptAsyncGateForCall(2)
	// Guarantees gate is closed exactly once even if an assertion below
	// calls t.Fatal before this test's own explicit close(gate) is reached
	// -- otherwise the fake server's own gated prompt_async handler
	// goroutine would stay blocked forever, and f.srv.Close() (registered
	// by newFakeOpenCodeServer, which therefore runs AFTER this cleanup
	// thanks to t.Cleanup's own LIFO ordering) would hang the whole test
	// binary waiting for that outstanding request to finish.
	var closeGateOnce sync.Once
	closeGate := func() { closeGateOnce.Do(func() { close(gate) }) }
	t.Cleanup(closeGate)

	a := New(f.URL(), testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	connCtx, connCancel := context.WithTimeout(context.Background(), testWait)
	defer connCancel()
	if err := a.Connected(connCtx); err != nil {
		t.Fatalf("Connected() error = %v", err)
	}
	waitForConnNumber(t, f, 1)

	collector := &eventCollector{}
	cmd := sandboxws.Prompt{
		Type: "prompt", MessageId: "m1", SessionId: "sess-mixed-2", Gen: 1,
		Text: "overflows first, then the retry hits a transient blip instead",
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	ts := waitForTurnRegistered(t, a, "ses_fake")

	// First-time ContextOverflowError -- consumes the shared latch via the
	// compaction path.
	f.broadcast(overflowMessageUpdated(t, "ses_fake", "msg_original"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))
	waitForCount(t, "summarizeCallCount", f.summarizeCallCount, 1)

	// The retry's own re-dispatch (call #2) is now gated/blocked -- proving
	// (per forceCompaction's own doc comment, compact.go) that the
	// compaction-success wave has ALREADY been queued onto this connection,
	// and that ts.compacting is GUARANTEED still true for as long as the
	// gate stays closed.
	waitForCount(t, "promptCallCount", f.promptCallCount, 2)
	if got := ts.attemptedRecoveryKind(); got != recoveryKindCompaction {
		t.Fatalf("ts.attemptedRecoveryKind() = %v, want recoveryKindCompaction (test setup check)", got)
	}

	// Prove the compaction wave has been fully DRAINED by the SSE-reader
	// goroutine WHILE ts.compacting is still PROVABLY true (see
	// TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce's own
	// doc comment, compactionretry_test.go, for why this must run BEFORE
	// releasing the gate).
	waitForDrained(t, f, ts)

	closeGate() // only now let the gated retry dispatch finally return

	// Deterministically wait for ts.compacting to have actually cleared
	// before broadcasting the RETRIED prompt's own second failure below --
	// see TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce's
	// own doc comment (compactionretry_test.go) for the exact race this
	// closes: waitForCount above only proves the retry's own prompt_async
	// call was recorded server-side, strictly EARLIER than ts.compacting
	// actually clearing client-side.
	waitForNotCompacting(t, f, ts)

	// The RETRIED prompt hits a transient APIError instead of overflowing
	// again -- must NOT trigger a transient-error retry: the shared latch
	// is already spent.
	f.broadcast(apiErrorMessageUpdated(t, "ses_fake", "msg_retry", true))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeFailed {
		t.Errorf("execution_complete.Outcome = %q, want %q", final.Outcome, sandboxws.ExecutionCompleteOutcomeFailed)
	}
	if final.Reason == nil {
		t.Fatal("execution_complete.Reason = nil, want a non-nil enriched reason")
	}
	reason := *final.Reason
	if !strings.Contains(reason, "already attempted") {
		t.Errorf("execution_complete.Reason = %q, want it to mention a retry was already attempted", reason)
	}
	// Honesty check: this turn's one shot was ACTUALLY a compaction retry
	// -- the reason should describe that (enrichReasonForRepeatedOverflow's
	// own "overflowed" wording), never claim a transient-error retry was
	// what already happened.
	if !strings.Contains(reason, "overflowed") {
		t.Errorf("execution_complete.Reason = %q, want it to say the retried prompt \"also overflowed\" "+
			"(enrichReasonForRepeatedOverflow) -- this turn's one recovery attempt was genuinely a compaction "+
			"retry, not a transient-error one, even though the SECOND failure happens to be a transient APIError", reason)
	}

	// Exactly one recovery attempt total: exactly one /summarize call (the
	// FIRST overflow's own compaction), and no third prompt_async call.
	if got := f.summarizeCallCount(); got != 1 {
		t.Errorf("summarizeCallCount = %d, want exactly 1 (only the first overflow's own compaction attempt)", got)
	}
	if got := f.promptCallCount(); got != 2 {
		t.Errorf("promptCallCount = %d, want exactly 2 (original + the one compaction retry, no more)", got)
	}
}

// TestTransientRetry_NilModelOmitsWireModelFieldOnRetryDispatch is E3's own
// pin: attemptTransientRetry's own re-dispatch (adapter.go) must resolve
// its model via resolveModel, NOT resolveModelForced, exactly like
// StartTurn's own original dispatch does (pinned separately by
// TestStartTurn_NilModelOmitsWireModelField, starturn_failure_test.go) --
// on this turn's default, no-modelId configuration, the RETRY's own wire
// prompt_async request must still OMIT "model" entirely, not silently
// install a Narvi-side fallback the client never asked for just because
// this particular dispatch happens to be a retry.
//
// A LATER audit's own finding: an earlier version of this fix corrected
// StartTurn's own call site (line ~540, adapter.go) but left
// attemptTransientRetry's own IDENTICAL call (line ~1399) reading
// resolveModelForced -- swapping THAT one site back to resolveModelForced
// passed this package's entire suite, because no existing test dispatched
// a transient-retry re-dispatch with cmd.Model nil and then inspected the
// RETRY's own wire request specifically (TestTransientRetry_
// PermanentAPIErrorNeverRetried, which does check f.lastPromptModel,
// never retries at all -- a permanent APIError finalizes on the FIRST
// dispatch). This test targets exactly that gap.
//
// Mutation-verified: swapping adapter.go's own
// `model := a.resolveModel(ctx, (*string)(ts.cmd.Model))` inside
// attemptTransientRetry back to resolveModelForced makes this test fail
// with `fake server's own RETRY prompt_async request carried
// model=&{ProviderID:anthropic ModelID:claude-sonnet-4-5}` (fallbackModel,
// session.go), while TestStartTurn_NilModelOmitsWireModelField (the OTHER
// call site's own pin) stays green, proving the two sites are
// independently guarded.
func TestTransientRetry_NilModelOmitsWireModelFieldOnRetryDispatch(t *testing.T) {
	f := newFakeOpenCodeServer(t)

	a := New(f.URL(), testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	connCtx, connCancel := context.WithTimeout(context.Background(), testWait)
	defer connCancel()
	if err := a.Connected(connCtx); err != nil {
		t.Fatalf("Connected() error = %v", err)
	}
	waitForConnNumber(t, f, 1)

	collector := &eventCollector{}
	// Model deliberately left unset -- the default configuration every
	// real client dispatches through (Composer/PlanModeView/Timeline
	// resume/DecisionInbox, web/src/session).
	cmd := sandboxws.Prompt{
		Type: "prompt", MessageId: "m1", SessionId: "sess-transient-nilmodel-1", Gen: 1,
		Text: "do something that will hit a transient provider blip",
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	ts := waitForTurnRegistered(t, a, "ses_fake")

	// The original turn's own assistant message reports a transient
	// APIError (no model planted -- irrelevant to this test, which only
	// cares about the wire REQUEST, never the diagnostic), then goes idle.
	f.broadcast(apiErrorMessageUpdated(t, "ses_fake", "msg_original", true))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	// Wait for the RETRY's own prompt_async call to actually land server-
	// side before inspecting f.lastPromptModel -- mirrors this file's own
	// established waitForCount precedent exactly (e.g.
	// TestTransientRetry_SucceedsAfterTransientAPIError above).
	waitForCount(t, "promptCallCount", f.promptCallCount, 2)

	f.mu.Lock()
	retryPromptModel := f.lastPromptModel
	f.mu.Unlock()
	if retryPromptModel != nil {
		t.Errorf("fake server's own RETRY prompt_async request carried model=%+v, want the field omitted "+
			"entirely (cmd.Model was nil) -- attemptTransientRetry's own re-dispatch must resolve its model "+
			"via resolveModel, not resolveModelForced, exactly like StartTurn's own original dispatch does",
			retryPromptModel)
	}

	// Let the retry complete cleanly so the turn finalizes and this test
	// doesn't leak a goroutine waiting on group.Wait() below.
	waitForNotCompacting(t, f, ts)
	f.broadcast(plainAssistantMessageUpdated(t, "ses_fake", "msg_retry"))
	f.broadcast(assistantTextPart(t, "ses_fake", "msg_retry", "prt_retry", "all good now"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
}
