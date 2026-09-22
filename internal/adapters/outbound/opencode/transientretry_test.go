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

	f.broadcast(apiErrorMessageUpdated(t, "ses_fake", "msg_original", false))
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

	// No retry: exactly the original dispatch, nothing more.
	if got := f.promptCallCount(); got != 1 {
		t.Errorf("promptCallCount = %d, want exactly 1 (a permanent APIError must never be retried)", got)
	}
	if got := f.summarizeCallCount(); got != 0 {
		t.Errorf("summarizeCallCount = %d, want exactly 0", got)
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

	f.broadcast(apiErrorMessageUpdated(t, "ses_fake", "msg_original", true))
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
