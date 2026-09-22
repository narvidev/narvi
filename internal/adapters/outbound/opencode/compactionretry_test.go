package opencode

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// THE PRIMARY §7.2 regression test: unlike realturn_test.go's own
// skipIfNoProvider-gated tests, this deliberately runs against
// fakeOpenCodeServer (fake_server_test.go) so CI can exercise the whole
// compaction-retry round trip reliably, with no real AI provider needed --
// scripting the EXACT empirically-observed sequence this Step's own live
// research pass captured (see compact.go's own forceCompaction doc
// comment for the full captured event list): a ContextOverflowError on the
// original prompt's own assistant message, followed by session.idle for
// the SAME session, must trigger exactly one POST /summarize call, then
// exactly one retried POST .../prompt_async call with the SAME prompt
// text, then (this scenario) a clean completion.

// waitForCount polls fn until it returns >= n, or fails the test after
// testWait -- shared shape for summarizeCallCount/promptCallCount, which
// this test polls across the background goroutine
// finalizeOrRecoverFromOverflow launches (adapter.go).
func waitForCount(t *testing.T, name string, fn func() int, n int) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) {
		if fn() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s never reached %d within %s (last observed: %d)", name, n, testWait, fn())
}

// overflowMessageUpdated/plainAssistantMessageUpdated/assistantTextPart
// build the exact SSE lines this test broadcasts -- mirroring this
// package's own sseLine helper (fake_server_test.go) and the real,
// VERIFIED-live openCodeMessageInfo/textPart shapes (types.go).
func overflowMessageUpdated(t *testing.T, sessionID, messageID string) string {
	t.Helper()
	return sseLine(t, "message.updated", messageUpdatedProps{
		SessionID: sessionID,
		Info: openCodeMessageInfo{
			ID:    messageID,
			Role:  "assistant",
			Error: &openCodeTaggedError{Name: "ContextOverflowError"},
		},
	})
}

func plainAssistantMessageUpdated(t *testing.T, sessionID, messageID string) string {
	t.Helper()
	return sseLine(t, "message.updated", messageUpdatedProps{
		SessionID: sessionID,
		Info:      openCodeMessageInfo{ID: messageID, Role: "assistant"},
	})
}

func assistantTextPart(t *testing.T, sessionID, messageID, partID, text string) string {
	t.Helper()
	// textPart (types.go) has no "type" field of its own (partEnvelope's
	// own "type" discriminator is peeked from the SAME raw bytes
	// separately, dispatchPart, sse.go) -- so the raw JSON on the wire
	// (and here) must carry "type":"text" explicitly alongside id/
	// messageID/text, matching dispatch_test.go's own literal-JSON
	// precedent for the exact same reason.
	part := struct {
		ID        string `json:"id"`
		MessageID string `json:"messageID"`
		Type      string `json:"type"`
		Text      string `json:"text"`
	}{ID: partID, MessageID: messageID, Type: "text", Text: text}
	raw, err := json.Marshal(part)
	if err != nil {
		t.Fatalf("assistantTextPart: %v", err)
	}
	return sseLine(t, "message.part.updated", messagePartUpdatedProps{SessionID: sessionID, Part: raw})
}

func sessionIdleLine(t *testing.T, sessionID string) string {
	t.Helper()
	return sseLine(t, "session.idle", sessionIdleProps{SessionID: sessionID})
}

// TestCompactionRetry_SucceedsAfterOverflow proves the full round trip:
// overflow -> exactly one /summarize call -> exactly one retried
// prompt_async call (same prompt text) -> a clean retry completion ->
// final execution_complete is Completed, never Failed.
//
// Gates the RETRY's own re-dispatch (call #2, armPromptAsyncGateForCall) --
// a LATER audit's own finding: a PRIOR version of this test asserted its own
// "compaction message never leaked through" check (see the isAssistantMessage
// check below) only AFTER group.Wait() returned, reasoning that the
// compaction wave and the retry's own later completion travel over the SAME
// single, strictly-ordered persistent connection, so processing is
// guaranteed complete by then. That reasoning is true but insufficient: it
// only proves dispatchEvent was CALLED for the compaction wave's own events,
// never that ts.isCompacting() read true AT THE MOMENT it was -- a genuine
// race against attemptCompactionRetry's own ts.setCompacting(false) call
// (adapter.go), deliberately made only AFTER postPromptAsync returns (§7.2
// Finding 3), on a completely different goroutine. Confirmed to actually
// flake under CI's own slower/more-contended scheduling (this branch's own
// broadcast-wait fix shifted timing enough to expose it): the retry's own
// re-dispatch can be accepted and its own ts.setCompacting(false) can run
// before the SSE-reader goroutine has drained the compaction wave the
// /summarize handler already broadcast, in which case the assertion below
// would previously have been checking a coin flip. Gating call #2 (mirroring
// TestCompactionRetry_LateCompactionTailEventDuringRetryDispatchIsSuppressed's
// own precedent for using this exact gate to make an isCompacting-guarded
// assertion deterministic) guarantees ts.compacting cannot possibly have
// cleared yet -- postPromptAsync's own client-side call is still blocked --
// regardless of how long the reader takes to catch up, closing the race
// instead of merely hoping to win it.
func TestCompactionRetry_SucceedsAfterOverflow(t *testing.T) {
	f := newFakeOpenCodeServer(t)
	f.setSummarizeOK(true)
	gate := f.armPromptAsyncGateForCall(2)

	a := New(f.URL(), testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	connCtx, connCancel := context.WithTimeout(context.Background(), testWait)
	defer connCancel()
	if err := a.Connected(connCtx); err != nil {
		t.Fatalf("Connected() error = %v", err)
	}
	waitForConnNumber(t, f, 1)

	collector := &eventCollector{}
	promptText := "do something that will overflow"
	cmd := sandboxws.Prompt{
		Type: "prompt", MessageId: "m1", SessionId: "sess-1", Gen: 1,
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

	// The original turn's own assistant message reports a
	// ContextOverflowError, then the turn goes idle -- the exact
	// empirically-observed trigger sequence (§7.2).
	f.broadcast(overflowMessageUpdated(t, "ses_fake", "msg_original"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	// The overflow must trigger exactly one POST .../summarize call.
	waitForCount(t, "summarizeCallCount", f.summarizeCallCount, 1)

	// ...and, on that call succeeding, exactly one RETRIED prompt_async
	// call for the SAME session with the SAME prompt text (promptCalls[0]
	// is the ORIGINAL dispatch from StartTurn itself; [1] is the retry).
	// The fake server's own handler (fake_server_test.go) records this call
	// BEFORE blocking on the gate armed above, so this proves forceCompaction
	// has ALREADY succeeded (its own compaction wave already fully broadcast,
	// see below) and the retry's own re-dispatch has been accepted and is now
	// genuinely gated/in-flight -- ts.compacting is GUARANTEED still true for
	// as long as the gate stays closed, since attemptCompactionRetry's own
	// ts.setCompacting(false) call cannot run until this gated postPromptAsync
	// call returns.
	waitForCount(t, "promptCallCount", f.promptCallCount, 2)
	if got := f.lastPromptText(); got != promptText {
		t.Errorf("retried prompt text = %q, want %q (the EXACT same prompt)", got, promptText)
	}
	if got := f.summarizeCallCount(); got != 1 {
		t.Errorf("summarizeCallCount = %d, want exactly 1", got)
	}

	// A stray compaction-internal message must not have leaked through
	// while ts.compacting was true (see sse.go's dispatchEvent doc
	// comment) -- sanity check via ts's own tracked error state before the
	// retry's own clean completion below.
	if err := ts.errorForOutcome(); err != nil {
		t.Errorf("ts.errorForOutcome() = %+v, want nil (clearErrorsForRetry should have cleared it after a successful compaction)", err)
	}

	// §7.2 Finding 6's own mutation-testing proof, made deterministic by the
	// gate above: the fake server's own /summarize handler broadcasts a real
	// compaction-internal message.updated for "msg_compaction_ses_fake" from
	// INSIDE forceCompaction's own HTTP round trip, well before this point --
	// this must NEVER have been recorded as a KNOWN assistant message id,
	// proving dispatchEvent's own isCompacting guard (sse.go, the
	// "message.updated" case) actually fired for it (confirmed by temporarily
	// short-circuiting all four dispatchEvent isCompacting guards to dead
	// code and observing this exact assertion fail) rather than this
	// fake-server-backed test simply never exercising that guard at all.
	// waitForDrained deterministically proves the SSE-reader has actually
	// dispatched the compaction wave (queued before this point -- confirmed
	// by waitForCount(promptCallCount, 2) above) rather than assuming a
	// fixed sleep gave it enough real time to do so -- under the same
	// severe scheduling contention TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce's
	// own doc comment describes, a guessed duration could silently
	// under-wait, making the assertion below vacuously true instead of a
	// genuine proof. The gate is what makes this assertion meaningful
	// regardless of exactly how long draining takes -- ts.compacting
	// cannot possibly have cleared yet, since postPromptAsync (and
	// therefore this goroutine's own ts.setCompacting(false)) is still
	// blocked on the gate.
	waitForDrained(t, f, ts)
	if ts.isAssistantMessage("msg_compaction_ses_fake") {
		t.Error("compaction-internal message.updated was treated as a real assistant message -- " +
			"dispatchEvent's own isCompacting guard (sse.go) did not suppress it")
	}
	if !ts.isCompacting() {
		t.Fatal("ts.isCompacting() = false while the retry's own re-dispatch is still gated, want true")
	}

	close(gate) // let the gated retry dispatch finally return

	// Deterministically wait for compacting to actually flip false (i.e. for
	// attemptCompactionRetry's own postPromptAsync call to have genuinely
	// returned) BEFORE broadcasting the retry's own completion -- without
	// this, broadcasting immediately after waitForCount above only proves the
	// FAKE SERVER'S handler recorded the call, not that the ADAPTER's own
	// client-side postPromptAsync HTTP round trip has actually returned and
	// ts.setCompacting(false) has run, which would otherwise race
	// dispatchEvent's own isCompacting guard (sse.go) into silently dropping
	// the retry's own real completion.
	waitForNotCompacting(t, f, ts)

	// Now script the RETRY's own clean completion: a fresh assistant
	// message, a real text part, then session.idle.
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

	// The forced model resolution (cmd.Model is nil here, so
	// resolveModelForced falls back to fallbackModelRef) must have reached
	// the fake server's own /summarize handler correctly.
	f.mu.Lock()
	calls := append([]summarizeRequest(nil), f.summarizeCalls...)
	f.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("summarizeCalls = %+v, want exactly 1", calls)
	}
	wantProviderID, wantModelID, _ := strings.Cut(fallbackModel, "/")
	if calls[0].ProviderID != wantProviderID || calls[0].ModelID != wantModelID {
		t.Errorf("summarize request = %+v, want providerID=%q modelID=%q", calls[0], wantProviderID, wantModelID)
	}
}

// TestCompactionRetry_StepStartDuringCompactionIsSuppressed is a LATER
// audit's own test-gap finding: TestCompactionRetry_SucceedsAfterOverflow's
// own compaction wave only ever emits a "text" part, which dispatchPart's
// own isAssistantMessage gate (sse.go) already drops for a not-yet-known
// message id regardless of ts.isCompacting() -- so message.part.updated's
// own isCompacting guard was mutation-survivable (deleting it left every
// existing test in this package green). "step-start" bypasses
// isAssistantMessage entirely: dispatchPart's own "step-start" case calls
// ts.emit(translateStepStart(...)) UNCONDITIONALLY, and forceCompaction's
// own doc comment (compact.go) documents that a real compaction wave
// genuinely does emit step/tool part traffic too. Broadcasts a step-start
// part directly (not via the shared broadcastCompactionSuccessWave helper,
// deliberately -- see that helper's own doc comment for why growing its
// wave was tried and reverted) while /summarize is held open via
// armSummarizeGate, so ts.compacting is PROVABLY still true the whole
// time -- deterministic, no race against an independent connection,
// mirroring TestCompactionRetry_SessionErrorDuringCompactionIsSuppressed's
// own precedent exactly.
func TestCompactionRetry_StepStartDuringCompactionIsSuppressed(t *testing.T) {
	f := newFakeOpenCodeServer(t)
	f.setSummarizeOK(true)
	gate := f.armSummarizeGate()
	// promptGate ADDITIONALLY gates the retry's own re-dispatch (call #2) --
	// confirmed via actual local reproduction (GOMAXPROCS capped well below
	// this machine's real core count plus several competing CPU-bound busy
	// loops) that leaving it ungated after close(gate) below is vulnerable
	// to the SAME race TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce's
	// own doc comment describes in full, independent of this test's own
	// (already fully deterministic) step-start assertion above: the
	// compaction-success wave, broadcast the moment /summarize responds,
	// can be drained by the SSE-reader AFTER ts.compacting has already
	// cleared (postPromptAsync returning almost instantly against this
	// in-process fake server), misreading the wave's own internal
	// session.idle as this turn's real completion instead of the retry's
	// genuine one scripted below.
	promptGate := f.armPromptAsyncGateForCall(2)
	var closePromptGateOnce sync.Once
	closePromptGate := func() { closePromptGateOnce.Do(func() { close(promptGate) }) }
	t.Cleanup(closePromptGate)

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
		Type: "prompt", MessageId: "m1", SessionId: "sess-guard-stepstart", Gen: 1,
		Text: "will overflow, compaction gated so we can script a step-start part mid-flight",
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	ts := waitForTurnRegistered(t, a, "ses_fake")

	f.broadcast(overflowMessageUpdated(t, "ses_fake", "msg_original"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	waitForCount(t, "summarizeCallCount", f.summarizeCallCount, 1)
	if !ts.isCompacting() {
		t.Fatal("ts.isCompacting() = false right after the overflow triggered a compaction attempt, want true")
	}

	// /summarize is gated (blocked before it would even respond), so
	// ts.compacting is GUARANTEED to still be true right now and to remain
	// true until we close(gate) below -- broadcasting a step-start part
	// here has no ordering ambiguity at all.
	const stepID = "prt_stepstart_guard"
	before := ts.lastActivityTime()
	stepStart := struct {
		ID        string `json:"id"`
		MessageID string `json:"messageID"`
		Type      string `json:"type"`
	}{ID: stepID, MessageID: "msg_compaction_ses_fake", Type: "step-start"}
	raw, err := json.Marshal(stepStart)
	if err != nil {
		t.Fatalf("marshal step-start part: %v", err)
	}
	f.broadcast(sseLine(t, "message.part.updated", messagePartUpdatedProps{SessionID: "ses_fake", Part: raw}))

	// touch() runs unconditionally as the very first thing dispatchEvent's
	// own message.part.updated case does (sse.go), strictly before its
	// isCompacting check -- polling for lastActivityTime to advance past
	// `before` is a deterministic proxy for "this exact broadcast has now
	// been fully dispatched" (guard included).
	deadline := time.Now().Add(testWait)
	for !ts.lastActivityTime().After(before) {
		if time.Now().After(deadline) {
			t.Fatal("the broadcast step-start part was never dispatched (ts.lastActivityTime never advanced) within testWait")
		}
		time.Sleep(time.Millisecond)
	}

	for _, e := range collector.snapshot() {
		if step, ok := e.Payload.(sandboxws.StepStart); ok && step.StepId == stepID {
			t.Error("compaction-internal step-start part leaked through as a real wire step_start event -- " +
				"dispatchEvent's own isCompacting guard (sse.go, the \"message.part.updated\" case) did not suppress it")
		}
	}
	if !ts.isCompacting() {
		t.Fatal("ts.isCompacting() unexpectedly false while /summarize is still gated")
	}

	close(gate)

	// The retry's own re-dispatch (call #2) is now ALSO gated/blocked (via
	// promptGate) -- proving the compaction-success wave that /summarize
	// just broadcast has already been queued onto this connection, and that
	// ts.compacting is GUARANTEED still true for as long as promptGate
	// stays closed.
	waitForCount(t, "promptCallCount", f.promptCallCount, 2)

	// Prove the compaction wave has been fully DRAINED by the SSE-reader
	// goroutine WHILE ts.compacting is still PROVABLY true (see
	// TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce's own
	// doc comment for why this must run BEFORE releasing promptGate).
	waitForDrained(t, f, ts)

	closePromptGate() // let the gated retry dispatch finally return

	// Deterministically wait for ts.compacting to have actually cleared
	// before broadcasting the retry's own real completion below -- the
	// SAME latent test-synchronization gap this file already closed
	// elsewhere via waitForNotCompacting (see that helper's own doc
	// comment, fake_server_test.go, and e.g.
	// TestCompactionRetry_SucceedsAfterOverflow above): waitForCount above
	// only proves the fake server's own handler recorded the retry's own
	// prompt_async call, strictly EARLIER than the adapter's own
	// client-side postPromptAsync call actually returning and
	// attemptCompactionRetry clearing ts.compacting (§7.2 Finding 3's own
	// ordering, adapter.go) -- broadcasting immediately after waitForCount
	// would race dispatchEvent's own isCompacting guard (sse.go) into
	// silently and PERMANENTLY dropping the retry's own completion (there
	// is no replay), leaving nothing to finalize this turn within the
	// test's own testWait ctx budget.
	waitForNotCompacting(t, f, ts)

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

// TestCompactionRetry_StepFinishCostDuringCompactionIsCounted is D1's own
// regression test (a later adversarial review finding on this Step, filed
// against the shipped state of §26.7/§7.1's cost accumulator): before this
// fix, dispatchEvent's own "message.part.updated" isCompacting guard
// (sse.go) returned BEFORE ever calling dispatchPart -- and dispatchPart's
// own "step-finish" case is the ONLY place ts.addCost is called. But
// forceCompaction's own POST /summarize call (compact.go) is a real,
// synchronous, BILLED call on this same session, and this test's own
// sibling, TestCompactionRetry_StepStartDuringCompactionIsSuppressed's own
// doc comment already establishes (citing forceCompaction's own doc
// comment) that a real compaction wave genuinely does emit step/tool part
// traffic too -- including its own genuine step-finish, with a real,
// non-zero cost. Suppressing that cost along with everything else the guard
// suppresses meant ts.spentUSD (§26.7/§7.1) silently under-reported true
// spend by the compaction call's own cost -- exactly backwards for a
// fail-toward-caution cost gate, where GET /review-cost-budget must never
// report shouldSkip:false while real spend has already crossed the
// ceiling.
//
// This proves BOTH halves of the fix at once: (1) the compaction wave's own
// step-finish cost IS now counted into spentUSDTotal, and (2) the EXISTING
// suppression -- no wire step_finish event, hasText left untouched -- still
// holds exactly as before; this fix must not weaken that half.
//
// Broadcasts the step-finish part directly (not via the shared
// broadcastCompactionSuccessWave helper, deliberately -- mirroring
// TestCompactionRetry_StepStartDuringCompactionIsSuppressed's own precedent
// for why growing that wave was tried and reverted) while /summarize is
// held open via armSummarizeGate, so ts.compacting is PROVABLY still true
// the whole time -- deterministic, no race against an independent
// connection.
func TestCompactionRetry_StepFinishCostDuringCompactionIsCounted(t *testing.T) {
	f := newFakeOpenCodeServer(t)
	f.setSummarizeOK(true)
	gate := f.armSummarizeGate()
	// promptGate ADDITIONALLY gates the retry's own re-dispatch (call #2) --
	// see TestCompactionRetry_StepStartDuringCompactionIsSuppressed's own
	// identical addition (above) for the race this closes, independent of
	// this test's own (already fully deterministic) step-finish assertions.
	promptGate := f.armPromptAsyncGateForCall(2)
	var closePromptGateOnce sync.Once
	closePromptGate := func() { closePromptGateOnce.Do(func() { close(promptGate) }) }
	t.Cleanup(closePromptGate)

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
		Type: "prompt", MessageId: "m1", SessionId: "sess-guard-stepfinish", Gen: 1,
		Text: "will overflow, compaction gated so we can script a step-finish part mid-flight",
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	ts := waitForTurnRegistered(t, a, "ses_fake")

	f.broadcast(overflowMessageUpdated(t, "ses_fake", "msg_original"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	waitForCount(t, "summarizeCallCount", f.summarizeCallCount, 1)
	if !ts.isCompacting() {
		t.Fatal("ts.isCompacting() = false right after the overflow triggered a compaction attempt, want true")
	}
	if got := ts.spentUSDTotal(); got != 0 {
		t.Fatalf("spentUSDTotal() = %v before any step-finish was ever broadcast, want 0", got)
	}

	// /summarize is gated (blocked before it would even respond), so
	// ts.compacting is GUARANTEED to still be true right now and to remain
	// true until we close(gate) below -- broadcasting a step-finish part
	// here has no ordering ambiguity at all.
	const stepID = "prt_stepfinish_guard"
	const compactionCost = 0.0042
	before := ts.lastActivityTime()
	stepFinish := struct {
		ID        string  `json:"id"`
		MessageID string  `json:"messageID"`
		Type      string  `json:"type"`
		Cost      float64 `json:"cost"`
	}{ID: stepID, MessageID: "msg_compaction_ses_fake", Type: "step-finish", Cost: compactionCost}
	raw, err := json.Marshal(stepFinish)
	if err != nil {
		t.Fatalf("marshal step-finish part: %v", err)
	}
	f.broadcast(sseLine(t, "message.part.updated", messagePartUpdatedProps{SessionID: "ses_fake", Part: raw}))

	// touch() runs unconditionally as the very first thing dispatchEvent's
	// own message.part.updated case does (sse.go), strictly before its
	// isCompacting check -- polling for lastActivityTime to advance past
	// `before` is a deterministic proxy for "this exact broadcast has now
	// been fully dispatched" (guard included).
	deadline := time.Now().Add(testWait)
	for !ts.lastActivityTime().After(before) {
		if time.Now().After(deadline) {
			t.Fatal("the broadcast step-finish part was never dispatched (ts.lastActivityTime never advanced) within testWait")
		}
		time.Sleep(time.Millisecond)
	}

	// D1's own fix: the compaction wave's own step-finish cost MUST still
	// be counted, even though this part is otherwise fully suppressed.
	if got := ts.spentUSDTotal(); got != compactionCost {
		t.Errorf("spentUSDTotal() = %v after a compaction-internal step-finish broadcast while gated, want %v -- "+
			"dispatchEvent's own isCompacting guard (sse.go, the \"message.part.updated\" case) must still count "+
			"cost even while suppressing every other effect of the part", got, compactionCost)
	}

	// The EXISTING suppression must still hold: no wire step_finish event
	// for this step id leaked through, and hasText remains false -- this
	// fix must not weaken either half of that guarantee.
	for _, e := range collector.snapshot() {
		if step, ok := e.Payload.(sandboxws.StepFinish); ok && step.StepId == stepID {
			t.Error("compaction-internal step-finish part leaked through as a real wire step_finish event -- " +
				"dispatchEvent's own isCompacting guard (sse.go, the \"message.part.updated\" case) did not suppress translation")
		}
	}
	if hasText, _ := ts.outcomeInputs(); hasText {
		t.Error("ts.outcomeInputs() hasText = true after only a compaction-internal step-finish was broadcast, want false")
	}
	if !ts.isCompacting() {
		t.Fatal("ts.isCompacting() unexpectedly false while /summarize is still gated")
	}

	close(gate)

	// The retry's own re-dispatch (call #2) is now ALSO gated/blocked (via
	// promptGate) -- proving the compaction-success wave that /summarize
	// just broadcast has already been queued onto this connection, and that
	// ts.compacting is GUARANTEED still true for as long as promptGate
	// stays closed.
	waitForCount(t, "promptCallCount", f.promptCallCount, 2)

	// Prove the compaction wave has been fully DRAINED by the SSE-reader
	// goroutine WHILE ts.compacting is still PROVABLY true (see
	// TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce's own
	// doc comment for why this must run BEFORE releasing promptGate).
	waitForDrained(t, f, ts)

	closePromptGate() // let the gated retry dispatch finally return

	// Deterministically wait for ts.compacting to have actually cleared
	// before broadcasting the retry's own real completion below (see this
	// file's own established waitForNotCompacting precedent, e.g.
	// TestCompactionRetry_SucceedsAfterOverflow above, for why this matters).
	waitForNotCompacting(t, f, ts)

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

	// Final sanity: spentUSDTotal must STILL reflect the compaction's own
	// cost after the whole turn completes -- addCost is a pure running sum,
	// so nothing later in the retry (which broadcasts no step-finish of its
	// own) should have reset or double-counted it.
	if got := ts.spentUSDTotal(); got != compactionCost {
		t.Errorf("spentUSDTotal() after turn completion = %v, want %v (the compaction's own cost must persist)", got, compactionCost)
	}
}

// TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce proves
// the infinite-loop guard (§7.2 point 3): when the RETRIED prompt also
// overflows, exactly one /summarize call is EVER made (not two), and the
// final outcome is Failed with a reason that mentions the retry was
// already attempted.
//
// Gates the RETRY's own re-dispatch (call #2, armPromptAsyncGateForCall) --
// a THIRD round of this exact test's own flakiness, root-caused via actual
// local reproduction under constrained resources (GOMAXPROCS capped well
// below this machine's real core count, PLUS several competing CPU-bound
// busy loops, `go test -race -p 1 -count=200`), not inferred from CI logs
// alone: this test used to broadcast the first overflow and simply poll
// waitForCount/waitForNotCompacting with NO gate on the retry's own
// re-dispatch at all -- meaning the SSE-reader goroutine got no deliberately
// -extended grace period to drain the /summarize handler's own compaction-
// success wave (broadcastCompactionSuccessWave, fake_server_test.go) before
// ts.compacting cleared. That wave is queued onto the SAME persistent
// connection SYNCHRONOUSLY, strictly BEFORE forceCompaction's own HTTP
// response is even returned (compact.go's own doc comment) -- but queuing
// only proves it was ACCEPTED into the connection's own outbound buffer,
// never that dispatchEvent (an entirely separate goroutine reading that
// SAME connection) has actually caught up and processed it. Under severe
// scheduling contention the SSE-reader can fall far enough behind that
// ts.compacting flips back to false (postPromptAsync -- a SEPARATE, and
// against this in-process fake server NEAR-INSTANT, round trip -- returns
// almost immediately) WHILE the wave's own three events are still
// undelivered: draining them AFTER that clear no longer trips dispatchEvent's
// own isCompacting guard (sse.go) at all, so the wave's own internal
// session.idle gets misread as THIS TURN'S real completion (finalizing
// early via tryFinalize -- permanent, no replay -- discarding the SECOND
// overflow broadcast below entirely), and its own internal "text" part --
// if drained first -- gets misread as real assistant output, changing the
// resulting bogus premature outcome from Failed/"opencode: turn produced no
// output" to Completed/nil depending on exactly how far the reader had
// gotten -- confirmed to be EXACTLY the two shapes this test's own CI
// failures showed (Round 4: Outcome "completed"/reason nil; also observed
// locally: Outcome "failed"/"turn produced no output"), and a GENUINELY
// DIFFERENT mechanism from the "event stream connection lost, reconnecting"
// flake broadcast's own doc comment (fake_server_test.go) already hardened
// against separately (broadcastWaitBudget): every one of the locally
// reproduced failures above printed ZERO such reconnect log line.
//
// waitForNotCompacting alone (even with its own added waitForDrained half,
// see its own doc comment) cannot close this: by the time ts.compacting
// reads false, the wave may already have been misprocessed WHILE draining
// -- the damage (a premature tryFinalize claim) happens AS PART OF the same
// belated drain a barrier can only confirm AFTER the fact, too late to
// prevent it. Gating call #2 makes ts.compacting PROVABLY still true for as
// long as the gate stays closed (postPromptAsync, and therefore
// attemptCompactionRetry's own ts.setCompacting(false), cannot possibly
// have run yet) -- so waitForDrained, called BEFORE releasing the gate,
// proves the wave was drained WHILE that guarantee held, i.e. proves it was
// drained CORRECTLY (every dispatchEvent case for it observing
// isCompacting()==true), not merely that draining eventually happened.
// Mirrors this file's own established gate-then-confirm-then-release
// precedent (e.g. TestCompactionRetry_SucceedsAfterOverflow,
// TestCompactionRetry_StepStartDuringCompactionIsSuppressed) instead of
// racing wall-clock timing against a real HTTP round trip.
func TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce(t *testing.T) {
	f := newFakeOpenCodeServer(t)
	f.setSummarizeOK(true)
	gate := f.armPromptAsyncGateForCall(2)

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
		Type: "prompt", MessageId: "m1", SessionId: "sess-1", Gen: 1,
		Text: "do something that will overflow twice",
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	ts := waitForTurnRegistered(t, a, "ses_fake")

	// First overflow -- triggers the one and only compaction attempt.
	f.broadcast(overflowMessageUpdated(t, "ses_fake", "msg_original"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))
	waitForCount(t, "summarizeCallCount", f.summarizeCallCount, 1)

	// The retry's own re-dispatch (call #2) is now gated/blocked -- the fake
	// server's own handler records promptCalls BEFORE waiting on the gate
	// (fake_server_test.go), so this proves the retry's own re-dispatch has
	// been accepted and is now genuinely in flight, and -- since
	// forceCompaction's own HTTP response only returns AFTER
	// broadcastCompactionSuccessWave has already queued the whole
	// compaction-internal wave onto this connection -- that the wave has
	// ALREADY been queued too. ts.compacting is GUARANTEED still true for as
	// long as the gate stays closed: postPromptAsync (and therefore
	// attemptCompactionRetry's own ts.setCompacting(false)) cannot possibly
	// have returned yet.
	waitForCount(t, "promptCallCount", f.promptCallCount, 2)

	// Prove the compaction wave has been fully DRAINED by the SSE-reader
	// goroutine WHILE that guarantee still holds -- i.e. that every one of
	// its own three events was necessarily dispatched with
	// isCompacting()==true (this is what actually closes the race this
	// test's own doc comment describes; see waitForDrained's own doc
	// comment for why this must run BEFORE releasing the gate, not merely
	// before ts.compacting is observed false).
	waitForDrained(t, f, ts)

	close(gate) // only now let the gated retry dispatch finally return

	// Deterministically wait for ts.compacting to have actually flipped back
	// to false (i.e. for attemptCompactionRetry's own postPromptAsync call to
	// have genuinely returned, adapter.go) BEFORE broadcasting the RETRIED
	// prompt's own second overflow below -- mirroring this file's own
	// established waitForNotCompacting precedent (e.g.
	// TestCompactionRetry_LateCompactionTailEventDuringRetryDispatchIsSuppressed):
	// broadcasting the second overflow before that clear has actually run
	// would have dispatchEvent's own isCompacting guard (sse.go) silently and
	// PERMANENTLY drop it (there is no replay), leaving nothing to ever
	// finalize this turn within the test's own testWait ctx budget -- the
	// intermittent "cancelled / turn context canceled before completion"
	// flake this closes.
	waitForNotCompacting(t, f, ts)

	// The RETRIED prompt ALSO overflows.
	f.broadcast(overflowMessageUpdated(t, "ses_fake", "msg_retry"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeFailed {
		t.Errorf("execution_complete.Outcome = %q, want %q", final.Outcome, sandboxws.ExecutionCompleteOutcomeFailed)
	}
	if final.Reason == nil || !strings.Contains(string(*final.Reason), "already attempted") {
		reason := "<nil>"
		if final.Reason != nil {
			reason = string(*final.Reason)
		}
		t.Errorf("execution_complete.Reason = %q, want it to mention a retry was already attempted", reason)
	}

	// Exactly one /summarize call EVER -- the second overflow must NOT
	// trigger a second compaction attempt (§7.2's own infinite-loop
	// guard).
	if got := f.summarizeCallCount(); got != 1 {
		t.Errorf("summarizeCallCount = %d, want exactly 1 (no second compaction attempt)", got)
	}
	// And no THIRD prompt_async call either.
	if got := f.promptCallCount(); got != 2 {
		t.Errorf("promptCallCount = %d, want exactly 2 (original + the one retry, no more)", got)
	}
}

// TestCompactionRetry_ForceCompactionFails is §7.2 Finding 5's own
// regression test: the ONE branch of attemptCompactionRetry (adapter.go)
// that, before this fix, had zero fake-server-driven integration coverage
// at all -- forceCompaction (the POST .../summarize call) itself failing.
// setSummarizeOK(false) (fake_server_test.go) now actually makes the fake
// server's own /summarize handler reply with a real non-2xx status (Finding
// 5's own fix to that handler; it used to always reply HTTP 200 regardless
// of this flag, making the false branch inert), so this exercises the REAL
// integration path: ts.setCompacting(false), enrichReasonForFailedRecovery,
// and a.finalize with the ORIGINAL overflow outcome -- not just the pure
// string-builder outcome_test.go already covered in isolation.
func TestCompactionRetry_ForceCompactionFails(t *testing.T) {
	f := newFakeOpenCodeServer(t)
	f.setSummarizeOK(false)

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
		Type: "prompt", MessageId: "m1", SessionId: "sess-5", Gen: 1,
		Text: "do something that will overflow, but the compaction itself will fail",
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	ts := waitForTurnRegistered(t, a, "ses_fake")

	f.broadcast(overflowMessageUpdated(t, "ses_fake", "msg_original"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	waitForCount(t, "summarizeCallCount", f.summarizeCallCount, 1)

	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeFailed {
		t.Errorf("execution_complete.Outcome = %q, want %q", final.Outcome, sandboxws.ExecutionCompleteOutcomeFailed)
	}
	if final.Reason == nil || !strings.Contains(*final.Reason, "compaction retry attempted and failed") {
		reason := "<nil>"
		if final.Reason != nil {
			reason = *final.Reason
		}
		t.Errorf("execution_complete.Reason = %q, want it to mention the compaction retry itself failed", reason)
	}
	if final.Reason == nil || !strings.Contains(*final.Reason, "forceCompaction") {
		reason := "<nil>"
		if final.Reason != nil {
			reason = *final.Reason
		}
		t.Errorf("execution_complete.Reason = %q, want it to name forceCompaction as the failed step", reason)
	}

	// The retry must be correctly short-circuited: no retried prompt_async
	// call was ever made, since forceCompaction itself never succeeded.
	if got := f.promptCallCount(); got != 1 {
		t.Errorf("promptCallCount = %d, want exactly 1 (the original dispatch only -- no retry, forceCompaction failed)", got)
	}

	// ts.compacting must have been reset to false (adapter.go's own doc
	// comment: "so a stray late-arriving event for this session ... is not
	// silently swallowed by a guard that no longer serves a purpose").
	if ts.isCompacting() {
		t.Error("ts.isCompacting() = true after forceCompaction failed and the turn finalized, want false")
	}

	// NOTE: this test's own fake /summarize failure handler
	// (fake_server_test.go) also broadcasts a compaction-internal
	// session.error for realism -- but whether the SSE-reader goroutine has
	// actually finished processing that broadcast by the time this test
	// function reaches this point is a genuine, inherent race against an
	// INDEPENDENT connection (the POST /summarize response vs. the GET
	// /event stream), no different in kind from Finding 3's own root cause.
	// Asserting on ts.sessionError HERE would itself be flaky (confirmed:
	// observed failing roughly 1 run in 3 under `-count=3`) without proving
	// anything about correctness -- this failure branch's own finalize call
	// uses originalOutcome directly, never re-deriving from ts.sessionError,
	// so this particular race is harmless by construction. See
	// TestCompactionRetry_SessionErrorDuringCompactionIsSuppressed below for
	// a DETERMINISTIC proof of the same isCompacting guard (sse.go's
	// "session.error" case), using armSummarizeGate to control the exact
	// ordering instead of racing two independent connections.
}

// TestCompactionRetry_SessionErrorDuringCompactionIsSuppressed
// deterministically proves dispatchEvent's own FOURTH isCompacting-guarded
// case (session.error, sse.go) -- the one case
// TestCompactionRetry_SucceedsAfterOverflow's own compaction-success wave
// never reaches, and TestCompactionRetry_ForceCompactionFails' own
// broadcastCompactionErrorEvent call cannot deterministically prove either
// (see that test's own note above: it races an independent connection).
// Uses armSummarizeGate (fake_server_test.go, §7.2 Finding 1's own
// machinery) to hold ts.compacting genuinely true for as long as this test
// likes, broadcasts a session.error itself while gated (guaranteed to be
// dispatched while compacting is still true), and confirms it never reached
// ts.sessionError -- all ordering controlled explicitly, no timing races.
func TestCompactionRetry_SessionErrorDuringCompactionIsSuppressed(t *testing.T) {
	f := newFakeOpenCodeServer(t)
	f.setSummarizeOK(true)
	gate := f.armSummarizeGate()
	// promptGate ADDITIONALLY gates the retry's own re-dispatch (call #2) --
	// see TestCompactionRetry_StepStartDuringCompactionIsSuppressed's own
	// identical addition (above) for the race this closes, independent of
	// this test's own (already fully deterministic) session.error assertion.
	promptGate := f.armPromptAsyncGateForCall(2)
	var closePromptGateOnce sync.Once
	closePromptGate := func() { closePromptGateOnce.Do(func() { close(promptGate) }) }
	t.Cleanup(closePromptGate)

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
		Type: "prompt", MessageId: "m1", SessionId: "sess-guard4", Gen: 1,
		Text: "will overflow, compaction gated so we can script a session.error mid-flight",
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	ts := waitForTurnRegistered(t, a, "ses_fake")

	f.broadcast(overflowMessageUpdated(t, "ses_fake", "msg_original"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	waitForCount(t, "summarizeCallCount", f.summarizeCallCount, 1)
	if !ts.isCompacting() {
		t.Fatal("ts.isCompacting() = false right after the overflow triggered a compaction attempt, want true")
	}

	// /summarize is gated (blocked before it would even respond), so
	// ts.compacting is GUARANTEED to still be true right now and to remain
	// true until we close(gate) below -- broadcasting a compaction-internal
	// session.error here has no ordering ambiguity at all.
	before := ts.lastActivityTime()
	f.broadcast(sseLine(t, "session.error", sessionErrorProps{
		SessionID: "ses_fake",
		Error:     openCodeTaggedError{Name: "UnknownError"},
	}))

	// touch() runs unconditionally as the very first thing dispatchEvent's
	// own session.error case does (sse.go), strictly before its
	// isCompacting check, in the SAME synchronous call with no goroutine
	// switch in between -- polling for lastActivityTime to advance past
	// `before` is therefore a deterministic proxy for "this exact broadcast
	// has now been fully dispatched" (guard included), without needing a
	// dedicated test hook.
	deadline := time.Now().Add(testWait)
	for !ts.lastActivityTime().After(before) {
		if time.Now().After(deadline) {
			t.Fatal("the broadcast session.error was never dispatched (ts.lastActivityTime never advanced) within testWait")
		}
		time.Sleep(time.Millisecond)
	}

	if ts.sessionError != nil {
		t.Errorf("ts.sessionError = %+v after a compaction-internal session.error broadcast while gated, want nil -- "+
			"dispatchEvent's own isCompacting guard (sse.go, the \"session.error\" case) did not suppress it", ts.sessionError)
	}
	if !ts.isCompacting() {
		t.Fatal("ts.isCompacting() unexpectedly false while /summarize is still gated")
	}

	close(gate)

	// The retry's own re-dispatch (call #2) is now ALSO gated/blocked --
	// proving the compaction-success wave has already been queued onto this
	// connection, and that ts.compacting is GUARANTEED still true for as
	// long as promptGate stays closed.
	waitForCount(t, "promptCallCount", f.promptCallCount, 2)

	// Prove the compaction wave has been fully DRAINED by the SSE-reader
	// goroutine WHILE ts.compacting is still PROVABLY true (see
	// TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce's own
	// doc comment for why this must run BEFORE releasing promptGate).
	waitForDrained(t, f, ts)

	closePromptGate() // let the gated retry dispatch finally return

	// Deterministically wait for ts.compacting to have actually cleared
	// (attemptCompactionRetry's own postPromptAsync call genuinely returned,
	// adapter.go) before broadcasting the retry's own real completion below
	// -- waitForCount above only proves the fake server's handler recorded
	// the call, strictly EARLIER than the client-side clear (§7.2 Finding 3),
	// so broadcasting immediately after it would race dispatchEvent's own
	// isCompacting guard (sse.go) into silently and permanently dropping the
	// retry's own completion (mirroring this file's own established
	// waitForNotCompacting precedent, e.g.
	// TestCompactionRetry_LateCompactionTailEventDuringRetryDispatchIsSuppressed).
	waitForNotCompacting(t, f, ts)
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

// TestTurnState_TryBeginCompactionRetryIsAtomic is §7.2 Finding 2's own
// direct regression test: the OLD "check ts.compactionAlreadyAttempted()
// then separately call ts.markCompactionAttempted()+ts.setCompacting(true)"
// sequence in finalizeOrRecoverFromOverflow (adapter.go) was a classic
// check-then-act race with no lock spanning both steps. Proven here
// directly against turnState (no HTTP/SSE plumbing needed, mirroring
// finalize_race_test.go's own style of testing turnState races directly): a
// large number of goroutines race to call tryBeginCompactionRetry
// concurrently on the SAME shared turnState, released simultaneously via a
// shared channel (not sleeps or timing luck) -- EXACTLY ONE of them must
// ever observe true. Run under `go test -race` (like the whole package) to
// also confirm the underlying compacting/compactionAttempted fields are
// never accessed without ts.mu held.
func TestTurnState_TryBeginCompactionRetryIsAtomic(t *testing.T) {
	t.Parallel()

	collector := &eventCollector{}
	ts := newTurnState(sandboxws.Prompt{}, collector.sink, "")

	const n = 50
	results := make([]bool, n)
	start := make(chan struct{})
	var group errgroup.Group
	for i := 0; i < n; i++ {
		i := i
		group.Go(func() error {
			<-start
			results[i] = ts.tryBeginCompactionRetry()
			return nil
		})
	}
	close(start)
	_ = group.Wait()

	wins := 0
	for _, r := range results {
		if r {
			wins++
		}
	}
	if wins != 1 {
		t.Errorf("tryBeginCompactionRetry() returned true %d times across %d concurrent callers, want exactly 1", wins, n)
	}
	if !ts.compactionAlreadyAttempted() {
		t.Error("compactionAlreadyAttempted() = false after a winning tryBeginCompactionRetry call, want true")
	}
	if !ts.isCompacting() {
		t.Error("isCompacting() = false after a winning tryBeginCompactionRetry call, want true")
	}
}

// TestCompactionRetry_ConcurrentOverflowDetectionAttemptsExactlyOnce is
// §7.2 Finding 2's own integration-level regression test:
// finalizeOrRecoverFromOverflow (adapter.go) is invoked CONCURRENTLY, twice,
// for the exact SAME first-time ContextOverflowError on the SAME turnState
// -- mirroring the real race between dispatchEvent's own "session.idle" live
// path and finalizeByFallback's own SSE-inactivity path, both of which route
// through finalizeOrRecoverFromOverflow and can genuinely race on the exact
// same a.sseInactivityTimeout threshold (adapter.go's own doc comment). Must
// launch the compaction retry EXACTLY ONCE regardless of which goroutine
// "wins".
func TestCompactionRetry_ConcurrentOverflowDetectionAttemptsExactlyOnce(t *testing.T) {
	f := newFakeOpenCodeServer(t)
	f.setSummarizeOK(true)

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
		Type: "prompt", MessageId: "m1", SessionId: "sess-race", Gen: 1,
		Text: "racing overflow detection",
	}
	ts := newTurnState(cmd, collector.sink, "")
	a.registerTurn("ses_fake", ts)
	t.Cleanup(func() { a.unregisterTurn("ses_fake") })

	overflowErr := &openCodeTaggedError{Name: "ContextOverflowError"}
	outcome := deriveOutcome(overflowErr, false, false)

	const racers = 2
	start := make(chan struct{})
	var group errgroup.Group
	for i := 0; i < racers; i++ {
		group.Go(func() error {
			<-start
			a.finalizeOrRecoverFromOverflow("ses_fake", ts, outcome, overflowErr, time.Now())
			return nil
		})
	}
	close(start)
	_ = group.Wait()

	waitForCount(t, "summarizeCallCount", f.summarizeCallCount, 1)
	// A generous extra wait to give a SECOND, wrongly-launched compaction
	// attempt a real chance to show up before asserting the negative below
	// -- there is no direct "no more attempts are coming" signal to poll
	// for instead.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if f.summarizeCallCount() > 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := f.summarizeCallCount(); got != 1 {
		t.Errorf("summarizeCallCount = %d, want exactly 1 (two concurrent finalizeOrRecoverFromOverflow "+
			"calls for the SAME first-time overflow must attempt compaction exactly once)", got)
	}
}

// TestCompactionRetry_ConcurrentOverflowDetectionNeverFinalizesPrematurely is
// a LATER audit's own regression test for Finding 1, deterministic (no
// network-timing luck needed, unlike
// TestCompactionRetry_FallbackReleaseRacesLiveOverflowAtomically below, which
// exercises the same class of bug via the real fallback-fetch-vs-live-SSE
// path but is inherently subject to real scheduling/network timing): proves
// that when several goroutines call finalizeOrRecoverFromOverflow
// CONCURRENTLY for the SAME first-time overflow (exactly
// TestCompactionRetry_ConcurrentOverflowDetectionAttemptsExactlyOnce's own
// setup above), every LOSING goroutine correctly abandons
// (overflowActionStale, turn.go) rather than incorrectly finalizing the
// turn via the "already attempted" enriched-reason branch before the
// winning goroutine's own real compaction retry has had any chance to run
// at all.
//
// The PRE-FIX hazard this catches: finalizeOrRecoverFromOverflow used to
// call ts.tryBeginCompactionRetry() directly, with no re-check of
// staleness at that exact call site — a losing call (tryBeginCompactionRetry
// returning false) fell straight into the "already attempted" branch and
// called a.finalize() with the enriched "retried prompt also overflowed"
// reason IMMEDIATELY, synchronously, on the LOSING goroutine — while the
// WINNING goroutine's own recovery attempt (launched asynchronously via
// a.group.Go) had not even called forceCompaction yet. Since a.finalize is
// idempotent (tryFinalize), whichever of {a losing racer's own immediate,
// wrong finalize} or {the winning retry's own eventual, correct finalize}
// reaches tryFinalize FIRST wins — and the former has a structural head
// start (zero I/O) over the latter (at least one HTTP round trip). The
// existing TestCompactionRetry_ConcurrentOverflowDetectionAttemptsExactlyOnce
// test above never actually caught this: it only ever asserts
// summarizeCallCount, never ts.done/the eventual outcome, so it would pass
// unchanged whether or not a losing racer wrongly finalized.
//
// armSummarizeGate holds the WINNING retry's own forceCompaction call open
// deterministically, so the assertion "ts.done is not yet closed" right
// after the race is a genuine proof of "no losing racer finalized
// prematurely", not an accident of how fast a real HTTP round trip happened
// to complete.
func TestCompactionRetry_ConcurrentOverflowDetectionNeverFinalizesPrematurely(t *testing.T) {
	f := newFakeOpenCodeServer(t)
	f.setSummarizeOK(true)
	summarizeGate := f.armSummarizeGate()
	// Guarantees summarizeGate is closed exactly once even if an assertion
	// below calls t.Fatal before this test's own explicit close(summarizeGate)
	// is reached -- otherwise the fake server's own gated /summarize handler
	// goroutine would stay blocked forever, and f.srv.Close() (registered by
	// newFakeOpenCodeServer, which therefore runs AFTER this cleanup thanks
	// to t.Cleanup's own LIFO ordering) would hang the whole test binary
	// waiting for that outstanding request to finish.
	var closeSummarizeGateOnce sync.Once
	closeSummarizeGate := func() { closeSummarizeGateOnce.Do(func() { close(summarizeGate) }) }
	t.Cleanup(closeSummarizeGate)
	// promptGate additionally gates the winning retry's own re-dispatch --
	// this test's ONLY prompt_async call (call #1: unlike the other tests in
	// this file, this one drives finalizeOrRecoverFromOverflow directly
	// rather than through a real StartTurn dispatch, so there is no earlier
	// "original" prompt_async call to number around). Confirmed via actual
	// local reproduction (GOMAXPROCS capped well below this machine's real
	// core count plus several competing CPU-bound busy loops) that leaving
	// this ungated is vulnerable to the SAME race
	// TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce's own
	// doc comment describes in full: the /summarize handler's own
	// compaction-success wave, broadcast the moment summarizeGate above is
	// released, can be drained by the SSE-reader AFTER ts.compacting has
	// already cleared (postPromptAsync returning almost instantly against
	// this in-process fake server), misreading the wave's own internal
	// session.idle as this turn's real completion instead of the winning
	// retry's genuine one scripted below.
	promptGate := f.armPromptAsyncGateForCall(1)
	var closePromptGateOnce sync.Once
	closePromptGate := func() { closePromptGateOnce.Do(func() { close(promptGate) }) }
	t.Cleanup(closePromptGate)

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
		Type: "prompt", MessageId: "m1", SessionId: "sess-race-premature", Gen: 1,
		Text: "racing overflow detection must never finalize prematurely for a losing racer",
	}
	ts := newTurnState(cmd, collector.sink, "")
	a.registerTurn("ses_fake", ts)
	t.Cleanup(func() { a.unregisterTurn("ses_fake") })

	overflowErr := &openCodeTaggedError{Name: "ContextOverflowError"}
	outcome := deriveOutcome(overflowErr, false, false)

	const racers = 8
	start := make(chan struct{})
	var group errgroup.Group
	for i := 0; i < racers; i++ {
		group.Go(func() error {
			<-start
			a.finalizeOrRecoverFromOverflow("ses_fake", ts, outcome, overflowErr, time.Now())
			return nil
		})
	}
	close(start)
	_ = group.Wait()

	waitForCount(t, "summarizeCallCount", f.summarizeCallCount, 1)

	// THE headline assertion: no racer -- winner or loser -- may have
	// finalized this turn yet. The pre-fix bug closes ts.done right here,
	// discarding the real, winning retry (still gated, mid-flight, having
	// not even reached forceCompaction's own response).
	select {
	case <-ts.done:
		t.Fatal("turn finalized before the winning compaction retry even got a chance to run -- a losing racer " +
			"incorrectly finalized via the 'already attempted' branch instead of cleanly abandoning (Finding 1's " +
			"own class of TOCTOU)")
	default:
	}
	if got := f.summarizeCallCount(); got != 1 {
		t.Fatalf("summarizeCallCount = %d, want exactly 1", got)
	}

	closeSummarizeGate() // let the one genuine winner's own gated forceCompaction finally proceed

	// The winning retry's own re-dispatch (call #1) is now gated/blocked --
	// proving the compaction-success wave has already been queued onto this
	// connection, and that ts.compacting is GUARANTEED still true for as
	// long as promptGate stays closed.
	waitForCount(t, "promptCallCount", f.promptCallCount, 1)

	// Prove the compaction wave has been fully DRAINED by the SSE-reader
	// goroutine WHILE ts.compacting is still PROVABLY true (see
	// TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce's own
	// doc comment for why this must run BEFORE releasing promptGate).
	waitForDrained(t, f, ts)

	closePromptGate() // let the gated retry dispatch finally return

	waitForNotCompacting(t, f, ts)

	// Script the real retry's own clean completion.
	f.broadcast(plainAssistantMessageUpdated(t, "ses_fake", "msg_retry"))
	f.broadcast(assistantTextPart(t, "ses_fake", "msg_retry", "prt_retry", "all good now"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	select {
	case <-ts.done:
	case <-time.After(testWait):
		t.Fatal("turn never finalized after the real retry's own completion was scripted")
	}

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeCompleted {
		reason := "<nil>"
		if final.Reason != nil {
			reason = *final.Reason
		}
		t.Errorf("execution_complete.Outcome = %q, want %q (reason=%s)",
			final.Outcome, sandboxws.ExecutionCompleteOutcomeCompleted, reason)
	}
}

// TestTurnState_ResolveOverflowActionDetectsStalenessWithoutIsCompacting is a
// LATER audit's own regression test for the SECOND half of
// resolveOverflowAction's staleness guard (turn.go): a caller's snapshot can
// go stale not only because a compaction retry is LIVE right now
// (ts.compacting==true), but also because a retry began AND fully completed
// its own re-dispatch (ts.compacting flipped back to false) sometime after
// the caller's own snapshotTime -- a sub-case ts.isCompacting() alone can
// never observe, since by the time the caller gets around to checking, the
// retry is no longer "in flight" by any reasonable reading.
//
// Deterministic and fully synchronous (no fake server, no real timing at
// all): directly drives turnState's own ts.touch() to simulate "activity
// happened after the caller's snapshot was taken" (exactly what a completed
// retry's own SSE traffic does in production, per ts.touch()'s own field
// comment), then calls resolveOverflowAction with a snapshotTime taken
// BEFORE that touch. Proves overflowActionStale fires (not
// overflowActionBeginRetry, and NOT overflowActionAlreadyAttempted either --
// this must never look like a retry was already attempted, since none ever
// was for THIS turnState in this test) and that ts's own compacting/
// compactionAttempted fields are left completely untouched (the caller must
// abandon without mutating anything).
func TestTurnState_ResolveOverflowActionDetectsStalenessWithoutIsCompacting(t *testing.T) {
	collector := &eventCollector{}
	ts := newTurnState(sandboxws.Prompt{}, collector.sink, "")

	snapshotTime := ts.lastActivityTime()

	// Simulate activity that happened AFTER the caller's own snapshot was
	// taken -- exactly what a retry's own SSE traffic (ts.touch(), called by
	// dispatchEvent for every dispatched event) does while
	// finalizeByFallback's own fetchFinalMessages call is in flight.
	time.Sleep(time.Millisecond) // ensure a strictly later wall-clock reading
	ts.touch()

	if !ts.lastActivityTime().After(snapshotTime) {
		t.Fatal("test setup invalid: ts.touch() did not advance lastActivityTime past the snapshot")
	}
	if ts.isCompacting() {
		t.Fatal("test setup invalid: ts.isCompacting() should read false -- this test proves the OTHER half of " +
			"the staleness guard, the one ts.isCompacting() alone cannot detect")
	}

	got := ts.resolveOverflowAction(snapshotTime, recoveryKindCompaction)
	if got != overflowActionStale {
		t.Errorf("resolveOverflowAction() = %v, want overflowActionStale (activity was recorded after the "+
			"caller's own snapshot, even though ts.isCompacting() reads false) -- a version of this guard that "+
			"dropped the lastActivity re-check entirely (Finding 4's own zero-coverage gap) would wrongly return "+
			"overflowActionBeginRetry here instead, letting a stale caller launch a SECOND, spurious "+
			"compaction retry", got)
	}
	if ts.compactionAlreadyAttempted() {
		t.Error("compactionAlreadyAttempted() = true after a stale resolveOverflowAction call, want false -- " +
			"the stale branch must abandon without mutating ts at all")
	}
	if ts.isCompacting() {
		t.Error("isCompacting() = true after a stale resolveOverflowAction call, want false -- the stale branch " +
			"must abandon without mutating ts at all")
	}
}

// TestCompactionRetry_FallbackAbandonsWhenRetryFullyCompletesDuringFetch is a
// LATER audit's own INTEGRATION-level regression test for the same gap
// TestTurnState_ResolveOverflowActionDetectsStalenessWithoutIsCompacting
// proves at the unit level, but driven through the real finalizeByFallback/
// fetchFinalMessages/fake-server path: a compaction retry can both BEGIN and
// FULLY complete its own re-dispatch (ts.compacting: false -> true -> false
// again) ENTIRELY inside the window finalizeByFallback's own gated
// GET /session/{id}/message fetch is blocked for -- so by the time that
// fetch finally returns, ts.isCompacting() alone reads false (nothing is "in
// flight" any more) and the ONLY thing that still proves the fetched
// snapshot is stale is ts.lastActivityTime() having advanced past
// finalizeByFallback's own preFetchActivity snapshot.
//
// Deterministic via armMessageGate (fake_server_test.go): holds
// finalizeByFallback's own fetch open for as long as the test likes, so the
// live retry has a real, bounded amount of wall-clock time to run all the
// way to completion before this test ever releases messageGate --
// waitForNotCompacting + waitForCount(promptCallCount, 2) below confirm
// that actually happened before proceeding, so there is no timing luck
// involved in reaching the state this test needs.
//
// ALSO gates the live retry's own re-dispatch (call #2,
// armPromptAsyncGateForCall) -- confirmed via actual local reproduction
// (this test failed with Outcome "failed"/"opencode: turn produced no
// output" under GOMAXPROCS capped well below this machine's real core
// count plus several competing CPU-bound busy loops) that leaving it
// UNGATED (the original design here) is itself vulnerable to the SAME race
// TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce's own
// doc comment (compactionretry_test.go) describes in full: the
// /summarize handler's own compaction-success wave can be drained by the
// SSE-reader AFTER ts.compacting has already cleared, misreading the
// wave's own internal session.idle as this turn's real completion. Gating
// call #2 and calling waitForDrained BEFORE releasing it proves the wave
// was drained while ts.compacting was PROVABLY still true, independent of
// (and strictly before) the messageGate race this test itself exists to
// prove.
func TestCompactionRetry_FallbackAbandonsWhenRetryFullyCompletesDuringFetch(t *testing.T) {
	f := newFakeOpenCodeServer(t)
	f.setSummarizeOK(true)
	messageGate := f.armMessageGate()
	promptGate := f.armPromptAsyncGateForCall(2)
	// Guarantee promptGate is closed exactly once even if an assertion below
	// calls t.Fatal before this test's own explicit close(promptGate) is
	// reached -- otherwise the fake server's own gated prompt_async handler
	// goroutine would stay blocked forever, and f.srv.Close() (registered by
	// newFakeOpenCodeServer, which therefore runs AFTER this cleanup thanks
	// to t.Cleanup's own LIFO ordering) would hang the whole test binary
	// waiting for that outstanding request to finish -- mirroring
	// TestCompactionRetry_ConcurrentOverflowDetectionNeverFinalizesPrematurely's
	// own closeSummarizeGateOnce precedent.
	var closePromptGateOnce sync.Once
	closePromptGate := func() { closePromptGateOnce.Do(func() { close(promptGate) }) }
	t.Cleanup(closePromptGate)

	// A stale snapshot: as if GET /session/{id}/message had been read before
	// the live compaction retry ever touched anything -- still showing the
	// ORIGINAL, not-yet-recovered overflow. If finalizeByFallback incorrectly
	// acts on this once messageGate is released, it will (wrongly) finalize
	// using this exact stale error.
	f.setMessages([]messageListEntry{
		{Info: openCodeMessageInfo{ID: "msg_original", Role: "assistant", Error: &openCodeTaggedError{Name: "ContextOverflowError"}}},
	})

	// Deliberately much shorter than testSSEInactivityTimeout -- must fire
	// the fallback (and block it in messageGate) well before the overflow is
	// even broadcast.
	shortInactivity := 50 * time.Millisecond

	a := New(f.URL(), shortInactivity, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	connCtx, connCancel := context.WithTimeout(context.Background(), testWait)
	defer connCancel()
	if err := a.Connected(connCtx); err != nil {
		t.Fatalf("Connected() error = %v", err)
	}
	waitForConnNumber(t, f, 1)

	collector := &eventCollector{}
	promptText := "overflow whose real retry fully completes while the fallback's own stale fetch is still gated"
	cmd := sandboxws.Prompt{
		Type: "prompt", MessageId: "m1", SessionId: "sess-fully-completes-during-fetch", Gen: 1,
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

	// Deterministically wait for waitForTurn's own fallback ticker to have
	// actually reached (and blocked inside) its own fetchFinalMessages call
	// (messageGate) -- messageCallCount is incremented BEFORE the fake
	// server's own handler waits on the gate, so this proves the fetch is
	// genuinely in flight, holding its own preFetchActivity snapshot from
	// before the live retry below ever starts, rather than assuming so via a
	// fixed wall-clock sleep (a LATER audit's own test-adversarial finding:
	// a sleep here could under-wait under heavy concurrent load).
	waitForCount(t, "messageCallCount", f.messageCallCount, 1)

	// NOW the live overflow arrives -- forceCompaction itself is ungated (it
	// runs to completion immediately), but the retry's own re-dispatch
	// (call #2, promptGate) is gated below so this test can deterministically
	// prove the compaction wave was drained before letting it, and therefore
	// ts.compacting's own clear, proceed -- see this test's own doc comment.
	f.broadcast(overflowMessageUpdated(t, "ses_fake", "msg_original"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	waitForCount(t, "summarizeCallCount", f.summarizeCallCount, 1)

	// The retry's own re-dispatch (call #2) is now gated/blocked -- proving
	// the compaction-success wave has already been queued onto this
	// connection, and that ts.compacting is GUARANTEED still true for as
	// long as promptGate stays closed.
	waitForCount(t, "promptCallCount", f.promptCallCount, 2)
	if !ts.compactionAlreadyAttempted() {
		t.Fatal("compactionAlreadyAttempted() = false after the retry's own re-dispatch was accepted, want true -- " +
			"this test's own precondition (the retry must have fully committed) is not yet satisfied")
	}

	// Prove the compaction wave has been fully DRAINED by the SSE-reader
	// goroutine WHILE ts.compacting is still PROVABLY true (see
	// TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce's own
	// doc comment for why this must run BEFORE releasing promptGate).
	waitForDrained(t, f, ts)

	closePromptGate() // let the gated retry dispatch finally return

	waitForNotCompacting(t, f, ts) // proves the retry's own re-dispatch has actually returned

	// Update the fake server's own fixture to reflect the retry's real,
	// genuine completion BEFORE releasing the gate -- shortInactivity is
	// short enough that waitForTurn's own fallback ticker can legitimately
	// fire AGAIN for the now-silent RETRIED prompt (still waiting on its own
	// session.idle, not yet broadcast below) before this test gets to
	// broadcast it; if that happens, that SECOND, entirely genuine
	// (non-stale, from ITS OWN fresh snapshot) fallback fetch must see
	// accurate data, not the ORIGINAL test fixture's now-stale
	// ContextOverflowError, or it would derive an "already attempted /
	// retried prompt also overflowed" outcome for a reason that has nothing
	// to do with the staleness guard this test targets (a test-fixture
	// artifact, not a production bug). This does not weaken what this test
	// proves about the FIRST, already-blocked fetch below: that call's own
	// preFetchActivity was snapshotted BEFORE any of this activity
	// happened, so resolveOverflowAction's staleness check fires for it
	// regardless of what the fixture now says.
	f.setMessages([]messageListEntry{
		{Info: openCodeMessageInfo{ID: "msg_retry", Role: "assistant"}, Parts: []json.RawMessage{textPartJSON(t, "prt_retry", "msg_retry", "all good now")}},
	})

	// Release the fallback's own stale fetch NOW -- strictly AFTER the
	// retry above has both begun and fully completed its re-dispatch, so
	// ts.isCompacting() will read false by the time finalizeByFallback
	// resumes, and ONLY the lastActivity staleness check can still catch
	// this. Immediately (no deliberate sleep in between) broadcast the real
	// retry's own completion too -- this test does not attempt to peek at
	// ts.done between the two: the fake server's own handler snapshots
	// f.messages BEFORE waiting on the gate (see its own doc comment), so
	// the just-unblocked fetch's own entries are STILL the original stale
	// snapshot regardless of the setMessages call above -- resolveOverflowAction's
	// lastActivity staleness check is the only thing that can make that
	// unblocked fetch abandon correctly rather than wrongly finalizing with
	// it. Asserting on the FINAL settled outcome below is what actually
	// distinguishes the two: a version of this guard missing the lastActivity
	// check would very likely let the stale fetch's own zero-I/O finalize
	// call win tryFinalize before the real completion's own event-dispatch
	// or a corrected fallback fetch's own HTTP round trip can, reporting the
	// wrong Failed/"already attempted" outcome instead.
	close(messageGate)
	f.broadcast(plainAssistantMessageUpdated(t, "ses_fake", "msg_retry"))
	f.broadcast(assistantTextPart(t, "ses_fake", "msg_retry", "prt_retry", "all good now"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}

	select {
	case <-ts.done:
	case <-time.After(testWait):
		t.Fatal("turn never finalized after the real retry's own completion was scripted")
	}

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeCompleted {
		reason := "<nil>"
		if final.Reason != nil {
			reason = *final.Reason
		}
		t.Errorf("execution_complete.Outcome = %q, want %q (reason=%s) -- the fallback's own STALE, "+
			"already-in-flight fetch (snapshotted before the live retry even began) must have abandoned via "+
			"the lastActivity staleness check rather than finalizing with its own outdated ContextOverflowError "+
			"(Finding 4's own zero-coverage gap)",
			final.Outcome, sandboxws.ExecutionCompleteOutcomeCompleted, reason)
	}
	if got := f.summarizeCallCount(); got != 1 {
		t.Errorf("summarizeCallCount = %d, want exactly 1 (the fallback's own stale fetch must never have "+
			"launched a second, spurious compaction attempt)", got)
	}
}

// TestCompactionRetry_FallbackDoesNotFinalizeWhileCompacting is §7.2 Finding
// 1's own regression test: finalizeByFallback (adapter.go) used to never
// check ts.isCompacting() at all, unlike every one of dispatchEvent's four
// guarded cases (sse.go) -- so the SSE-inactivity fallback could fire mid-
// compaction and prematurely finalize the turn (via the "already attempted"
// branch of finalizeOrRecoverFromOverflow) while the real background
// compaction-retry goroutine was still genuinely in flight. Deterministically
// forces exactly that timing via a deliberately SHORT SSE-inactivity timeout
// combined with armSummarizeGate (fake_server_test.go), which holds
// forceCompaction's own HTTP call open for as long as the test likes,
// rather than relying on a real slow model response or scheduling luck.
func TestCompactionRetry_FallbackDoesNotFinalizeWhileCompacting(t *testing.T) {
	f := newFakeOpenCodeServer(t)
	f.setSummarizeOK(true)
	gate := f.armSummarizeGate()
	// promptGate ADDITIONALLY gates the retry's own re-dispatch (call #2) --
	// see TestCompactionRetry_StepStartDuringCompactionIsSuppressed's own
	// identical addition for the race this closes: an ungated retry here
	// gives the SSE-reader no deliberately-extended grace period to drain
	// the compaction-success wave before ts.compacting clears.
	promptGate := f.armPromptAsyncGateForCall(2)
	var closePromptGateOnce sync.Once
	closePromptGate := func() { closePromptGateOnce.Do(func() { close(promptGate) }) }
	t.Cleanup(closePromptGate)

	// Deliberately much shorter than testSSEInactivityTimeout -- waitForTurn's
	// own fallback ticker (pollInterval = this / ssePollDivisor) must tick,
	// and shouldFinalizeByFallback must observe ts.idleFor(this)==true, MANY
	// times over, while /summarize is still gated below.
	shortInactivity := 50 * time.Millisecond

	a := New(f.URL(), shortInactivity, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	connCtx, connCancel := context.WithTimeout(context.Background(), testWait)
	defer connCancel()
	if err := a.Connected(connCtx); err != nil {
		t.Fatalf("Connected() error = %v", err)
	}
	waitForConnNumber(t, f, 1)

	collector := &eventCollector{}
	promptText := "do something that will overflow, compaction takes a while"
	cmd := sandboxws.Prompt{
		Type: "prompt", MessageId: "m1", SessionId: "sess-1", Gen: 1,
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

	f.broadcast(overflowMessageUpdated(t, "ses_fake", "msg_original"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	waitForCount(t, "summarizeCallCount", f.summarizeCallCount, 1)
	if !ts.isCompacting() {
		t.Fatal("ts.isCompacting() = false right after the overflow triggered a compaction attempt, want true")
	}

	// Let several multiples of the (short) SSE-inactivity timeout elapse
	// while /summarize is still gated -- waitForTurn's own ticker will have
	// fired well over shortInactivity*10/pollInterval times by now. The turn
	// must still be genuinely alive: the real compaction retry is still in
	// flight, and only IT is allowed to decide this turn's own fate right
	// now.
	time.Sleep(shortInactivity * 10)

	select {
	case <-ts.done:
		t.Fatal("turn finalized while a compaction retry was still genuinely in flight " +
			"(forceCompaction still gated) -- finalizeByFallback fired despite ts.isCompacting(), " +
			"Finding 1's own regression")
	default:
	}
	if !ts.isCompacting() {
		t.Fatal("ts.isCompacting() unexpectedly false while /summarize is still gated")
	}

	close(gate) // let the gated /summarize call finally return

	// The retry's own re-dispatch (call #2) is now ALSO gated/blocked --
	// proving the compaction-success wave has already been queued onto this
	// connection, and that ts.compacting is GUARANTEED still true for as
	// long as promptGate stays closed.
	waitForCount(t, "promptCallCount", f.promptCallCount, 2)
	if got := f.lastPromptText(); got != promptText {
		t.Errorf("retried prompt text = %q, want %q", got, promptText)
	}

	// Prove the compaction wave has been fully DRAINED by the SSE-reader
	// goroutine WHILE ts.compacting is still PROVABLY true (see
	// TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce's own
	// doc comment for why this must run BEFORE releasing promptGate).
	waitForDrained(t, f, ts)

	closePromptGate() // let the gated retry dispatch finally return

	// Deterministically wait for ts.compacting to have actually cleared
	// before broadcasting the retry's own real completion below -- see
	// TestCompactionRetry_LateCompactionTailEventDuringRetryDispatchIsSuppressed's
	// own doc comment (and waitForNotCompacting's, fake_server_test.go) for
	// the exact race this closes: waitForCount above only proves the fake
	// server's handler recorded the retry's own prompt_async call, strictly
	// EARLIER than the adapter's own client-side postPromptAsync call
	// actually returning and clearing ts.compacting (§7.2 Finding 3).
	waitForNotCompacting(t, f, ts)

	// Script the retry's own clean completion, exactly like
	// TestCompactionRetry_SucceedsAfterOverflow.
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

// TestCompactionRetry_LateCompactionTailEventDuringRetryDispatchIsSuppressed
// is §7.2 Finding 3's own regression test: attemptCompactionRetry
// (adapter.go) used to clear ts.compacting BEFORE dispatching the retry's
// own postPromptAsync call -- widening the window in which a late,
// compaction-tail-shaped SSE event (session.idle carries no field
// distinguishing it from the turn's own real completion -- see
// sessionIdleProps' own doc comment, types.go) arriving in that window could
// sail through the now-false isCompacting guard and get evaluated using the
// turn's STALE pre-overflow ts.sawText (never reset by clearErrorsForRetry,
// deliberately), with no tracked error (just cleared) -- finalizing the
// WHOLE turn as "completed" before the retried prompt was ever actually
// dispatched. Deterministically forces exactly that ordering via
// armPromptAsyncGateForCall (fake_server_test.go), gating ONLY the retry's
// own re-dispatch (call #2), rather than relying on scheduling luck.
func TestCompactionRetry_LateCompactionTailEventDuringRetryDispatchIsSuppressed(t *testing.T) {
	f := newFakeOpenCodeServer(t)
	f.setSummarizeOK(true)
	gate := f.armPromptAsyncGateForCall(2)

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
		Type: "prompt", MessageId: "m1", SessionId: "sess-3", Gen: 1,
		Text: "will overflow after producing some real output first",
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	ts := waitForTurnRegistered(t, a, "ses_fake")

	// Real pre-overflow output, so ts.sawText is ALREADY true before the
	// overflow -- exactly the stale signal Finding 3's failure scenario
	// depends on (clearErrorsForRetry never resets sawText/sawToolCall by
	// design -- see its own doc comment).
	f.broadcast(plainAssistantMessageUpdated(t, "ses_fake", "msg_pre"))
	f.broadcast(assistantTextPart(t, "ses_fake", "msg_pre", "prt_pre", "partial output before overflow"))
	waitForSawText(t, ts)

	f.broadcast(overflowMessageUpdated(t, "ses_fake", "msg_original"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	waitForCount(t, "summarizeCallCount", f.summarizeCallCount, 1)

	// forceCompaction has now succeeded and attemptCompactionRetry is about
	// to (or already did) call postPromptAsync -- which is gated, blocked
	// mid-flight (call #2). A late, compaction-tail-shaped session.idle for
	// the SAME session arrives here -- on the OLD, buggy ordering this would
	// already see ts.isCompacting()==false (cleared before the retry was
	// even dispatched) and wrongly finalize using the stale
	// sawText=true/no-tracked-error state.
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	// Deterministically prove the SSE-reader has actually DISPATCHED that
	// broadcast before asserting -- waitForDrained broadcasts one more,
	// inert sentinel and waits for IT to be dispatched, which (single
	// connection, single reader, in-order delivery) proves everything
	// broadcast before it, including the late session.idle just above, has
	// already been processed too. A fixed sleep here would only ASSUME
	// enough real time had passed for the reader to catch up -- under the
	// same severe scheduling contention this file's own
	// TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce was
	// confirmed vulnerable to (see its own doc comment), a guessed duration
	// could silently under-wait, making the assertion below vacuously true
	// rather than a genuine proof. The gate itself is what makes this
	// assertion MEANINGFUL regardless of exactly how long draining takes:
	// postPromptAsync (and therefore any chance of a LEGITIMATE completion)
	// cannot possibly have returned yet, since it is still blocked on gate.
	waitForDrained(t, f, ts)

	select {
	case <-ts.done:
		t.Fatal("turn finalized while the retry's own postPromptAsync call was still gated/in-flight -- " +
			"a late compaction-tail session.idle was NOT suppressed by ts.isCompacting(), Finding 3's own regression")
	default:
	}

	close(gate) // let the gated retry dispatch finally return

	waitForCount(t, "promptCallCount", f.promptCallCount, 2)

	// Deterministically wait for compacting to actually flip false (i.e. for
	// attemptCompactionRetry's own postPromptAsync call to have genuinely
	// returned) BEFORE broadcasting the retry's own completion -- mirroring
	// TestCompactionRetry_FallbackAbandonsOnStaleRaceWithLiveRetry's own
	// precedent (see waitForNotCompacting's own doc comment, fake_server_test.go,
	// for the exact race this closes). Without this, waitForCount above only
	// proves the FAKE SERVER'S handler recorded call #2 -- not that the
	// ADAPTER's own client-side postPromptAsync HTTP round trip has actually
	// returned and ts.setCompacting(false) has run (§7.2 Finding 3 deliberately
	// defers that clear until AFTER postPromptAsync returns) -- so broadcasting
	// the retry's real completion immediately after waitForCount can race
	// dispatchEvent's own isCompacting guard (sse.go), which would silently
	// drop session.idle if it lands before that clear, leaving nothing to ever
	// close ts.done within this test's own testWait ctx budget: exactly the
	// intermittent "cancelled / turn context canceled before completion"
	// flake this fix eliminates.
	waitForNotCompacting(t, f, ts)

	// Script the retry's own REAL clean completion.
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

// TestCompactionRetry_StopDuringCompactionAbortsRetry is a LATER audit's own
// Finding 1 regression test: a user Stop landing WHILE forceCompaction is
// still genuinely in flight must abort the retry entirely -- never
// re-dispatch the very prompt the user just cancelled, and finalize as
// Cancelled, not Completed/Failed. Before this fix, Adapter.Stop touched no
// turnState at all, so the abort's own OpenCode-side signal was silently
// swallowed by ts.isCompacting()'s own dispatchEvent guards (sse.go) and
// attemptCompactionRetry re-dispatched unconditionally once forceCompaction
// returned. Uses armSummarizeGate (fake_server_test.go) to hold
// forceCompaction open long enough to call Stop deterministically while
// compacting, rather than racing a real timing window.
func TestCompactionRetry_StopDuringCompactionAbortsRetry(t *testing.T) {
	f := newFakeOpenCodeServer(t)
	f.setSummarizeOK(true)
	gate := f.armSummarizeGate()

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
		Type: "prompt", MessageId: "m1", SessionId: "sess-stop", Gen: 1,
		Text: "will overflow, then the user hits Stop while compaction is still in flight",
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	ts := waitForTurnRegistered(t, a, "ses_fake")

	f.broadcast(overflowMessageUpdated(t, "ses_fake", "msg_original"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	waitForCount(t, "summarizeCallCount", f.summarizeCallCount, 1)
	if !ts.isCompacting() {
		t.Fatal("ts.isCompacting() = false right after the overflow triggered a compaction attempt, want true")
	}

	// The user presses Stop WHILE forceCompaction is still gated/in-flight.
	// Stop's own resulting OpenCode-side abort call would normally produce
	// a session.idle/session.error this adapter's own isCompacting guards
	// (sse.go) silently swallow -- markStopRequested (turn.go) is the
	// dedicated, NON-swallowed signal this fix adds specifically so
	// attemptCompactionRetry can still learn this happened.
	stop := sandboxws.Stop{Type: "stop", MessageId: "m2", SessionId: "sess-stop", Gen: 1}
	if err := a.Stop(ctx, stop); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	close(gate) // let the gated /summarize call finally return -- compaction itself still succeeds

	// A LATER audit's own Finding 4: the Outcome assertion below is, on its
	// own, non-discriminating -- reverting either half of the ACTUAL fix
	// (stillLive()'s check in attemptCompactionRetry, or markStopRequested
	// in Stop) still eventually produces Outcome==Cancelled here, but only
	// via this test's own StartTurn ctx timing out after the full testWait
	// (waitForTurn's ctx.Done() branch, finalizeCanceled) -- an entirely
	// different, unrelated code path from the one this test exists to
	// prove. Timing group.Wait() itself discriminates the two: with the fix
	// intact, the turn finalizes within milliseconds of close(gate) (the
	// Stop-driven Cancelled path); with either half reverted, it takes
	// nearly the full testWait instead (confirmed by reverting each half
	// independently and observing this elapsed check fail every time, while
	// the bare Outcome assertion alone kept passing).
	started := time.Now()
	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("StartTurn took %s to return after Stop landed during compaction -- want a prompt "+
			"Cancelled finalize via the Stop-driven path (markStopRequested + stillLive()), not the "+
			"unrelated ctx-timeout fallback (which would take close to the full %s testWait)", elapsed, testWait)
	}

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeCancelled {
		reason := "<nil>"
		if final.Reason != nil {
			reason = *final.Reason
		}
		t.Errorf("execution_complete.Outcome = %q, want %q (reason=%s)", final.Outcome, sandboxws.ExecutionCompleteOutcomeCancelled, reason)
	}

	// THE headline assertion: the retry must NEVER have been re-dispatched
	// -- a Stop must never be silently overridden by re-posting the exact
	// prompt the user just cancelled.
	if got := f.promptCallCount(); got != 1 {
		t.Errorf("promptCallCount = %d, want exactly 1 (the original dispatch only -- "+
			"Stop must abort the retry, never re-dispatch)", got)
	}
	if got := f.summarizeCallCount(); got != 1 {
		t.Errorf("summarizeCallCount = %d, want exactly 1", got)
	}
	if ts.isCompacting() {
		t.Error("ts.isCompacting() = true after the retry was aborted by Stop, want false")
	}
}

// TestCompactionRetry_StopDuringRetryDispatchAbortsRedispatchedPrompt is a
// LATER audit's own ROUND-2 Finding 1 regression test:
// TestCompactionRetry_StopDuringCompactionAbortsRetry above only ever calls
// Stop BEFORE forceCompaction returns -- attemptCompactionRetry's own
// stillLive() check (adapter.go) used to be read exactly once, right after
// forceCompaction returned, and never re-consulted between then and the
// actual a.postPromptAsync re-dispatch call a few lines later -- including
// across that call's own full HTTP round trip. A Stop landing in THAT
// window went completely unobserved: the retry redispatched the very
// prompt the user just asked to cancel. Deterministically forces exactly
// that window via armPromptAsyncGateForCall(2) (gating ONLY the retry's own
// re-dispatch call, mirroring
// TestCompactionRetry_LateCompactionTailEventDuringRetryDispatchIsSuppressed's
// own precedent for gating exactly this call), calling Stop WHILE that call
// is gated/already in flight.
func TestCompactionRetry_StopDuringRetryDispatchAbortsRedispatchedPrompt(t *testing.T) {
	f := newFakeOpenCodeServer(t)
	f.setSummarizeOK(true)
	gate := f.armPromptAsyncGateForCall(2)

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
		Type: "prompt", MessageId: "m1", SessionId: "sess-stop-redispatch", Gen: 1,
		Text: "will overflow; the user hits Stop while the RETRY's own redispatch is already in flight",
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	waitForTurnRegistered(t, a, "ses_fake")

	f.broadcast(overflowMessageUpdated(t, "ses_fake", "msg_original"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	// forceCompaction is NOT gated in this test, so it completes normally
	// and attemptCompactionRetry's own FIRST stillLive() check (right after
	// forceCompaction returns) passes -- no Stop has happened yet -- so it
	// proceeds to clearErrorsForRetry and calls postPromptAsync for the
	// retry, which IS gated (call #2) and blocks mid-flight.
	waitForCount(t, "summarizeCallCount", f.summarizeCallCount, 1)

	// The fake server's own /prompt_async handler records the call BEFORE
	// blocking on the gate (fake_server_test.go), so this proves the
	// retry's own re-dispatch has actually been accepted and is now
	// genuinely in flight -- exactly the window this finding describes.
	waitForCount(t, "promptCallCount", f.promptCallCount, 2)

	// The user presses Stop WHILE the retry's own re-dispatch HTTP call is
	// still gated/in-flight -- the exact window the ORIGINAL (round-1)
	// stillLive() check, read only once BEFORE this call, could never see.
	stop := sandboxws.Stop{Type: "stop", MessageId: "m2", SessionId: "sess-stop-redispatch", Gen: 1}
	if err := a.Stop(ctx, stop); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	close(gate) // let the gated retry dispatch finally return (it succeeds)

	started := time.Now()
	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("StartTurn took %s to return after Stop landed during the retry's own redispatch -- "+
			"want a prompt Cancelled finalize via the round-2 stillLive() re-check, not the unrelated "+
			"ctx-timeout fallback path (which would take close to the full %s testWait)", elapsed, testWait)
	}

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeCancelled {
		reason := "<nil>"
		if final.Reason != nil {
			reason = *final.Reason
		}
		t.Errorf("execution_complete.Outcome = %q, want %q (reason=%s)", final.Outcome, sandboxws.ExecutionCompleteOutcomeCancelled, reason)
	}

	// The retry WAS actually redispatched -- it had already been accepted
	// by OpenCode by the time Stop landed, and nothing can undo that HTTP
	// call once it is in flight -- but it must have been explicitly aborted
	// rather than silently left to run to completion having overridden the
	// user's own Stop.
	if got := f.promptCallCount(); got != 2 {
		t.Errorf("promptCallCount = %d, want exactly 2 (original + the one retry -- it WAS already "+
			"dispatched before Stop could prevent it)", got)
	}
	if got := f.abortCallCount(); got != 2 {
		t.Errorf("abortCallCount = %d, want exactly 2 (Stop's own always-present abort call, PLUS this "+
			"fix's explicit abort of the already-redispatched retry prompt)", got)
	}
}

// TestCompactionRetry_RetryPostPromptAsyncFails is a LATER audit's own
// test-gap finding: attemptCompactionRetry's own THIRD documented failure
// branch -- compaction succeeds, but the RETRIED postPromptAsync dispatch
// itself fails -- had zero fake-server-driven coverage at all before
// setPromptAsyncOK existed (fake_server_test.go), mirroring
// TestCompactionRetry_ForceCompactionFails' own Finding 5 precedent for the
// SIBLING failure branch. Proves the resulting execution_complete carries
// the ENRICHED original-overflow reason (naming both the original
// ContextOverflowError AND the fact that the retry's own dispatch failed),
// never a bare, indistinguishable-from-first-time-overflow reason.
func TestCompactionRetry_RetryPostPromptAsyncFails(t *testing.T) {
	f := newFakeOpenCodeServer(t)
	f.setSummarizeOK(true)

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
		Type: "prompt", MessageId: "m1", SessionId: "sess-6", Gen: 1,
		Text: "will overflow; compaction succeeds but the retried dispatch itself fails",
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	waitForTurnRegistered(t, a, "ses_fake")

	// The ORIGINAL dispatch (call #1) must keep succeeding -- registerTurn
	// happens BEFORE StartTurn's own postPromptAsync call, so
	// waitForTurnRegistered alone does not yet prove call #1 has actually
	// been sent; wait for it explicitly before arming the failure, or the
	// overflow scenario this test needs could never even get triggered in
	// the first place (see setPromptAsyncOK's own doc comment).
	waitForCount(t, "promptCallCount", f.promptCallCount, 1)
	f.setPromptAsyncOK(false)

	f.broadcast(overflowMessageUpdated(t, "ses_fake", "msg_original"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	waitForCount(t, "summarizeCallCount", f.summarizeCallCount, 1)
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
	if !strings.Contains(reason, "ContextOverflowError") {
		t.Errorf("execution_complete.Reason = %q, want it to name the ORIGINAL overflow error too", reason)
	}
	if !strings.Contains(reason, "compaction retry attempted and failed") {
		t.Errorf("execution_complete.Reason = %q, want it to mention the compaction retry itself failed", reason)
	}
	if !strings.Contains(reason, "retry postPromptAsync") {
		t.Errorf("execution_complete.Reason = %q, want it to name retry postPromptAsync as the failed step", reason)
	}

	if got := f.summarizeCallCount(); got != 1 {
		t.Errorf("summarizeCallCount = %d, want exactly 1", got)
	}
	if got := f.promptCallCount(); got != 2 {
		t.Errorf("promptCallCount = %d, want exactly 2 (original + the one failed retry, no more)", got)
	}
}

// TestCompactionRetry_FallbackAbandonsOnStaleRaceWithLiveRetry is a LATER
// audit's own Finding 2 regression test: finalizeByFallback used to compute
// its own finalize decision from a GET /session/{id}/message snapshot
// fetched BEFORE a live compaction retry could have started, with no
// re-check of that staleness AFTER the fetch actually returned -- so a live
// retry that began and won tryBeginCompactionRetry's own race WHILE this
// fetch was still in flight would still get its own turn prematurely
// finalized by the fallback's stale data (via finalizeOrRecoverFromOverflow's
// "already attempted" branch), orphaning the real retry goroutine.
//
// Deterministically forces exactly that interleaving via TWO gates: a very
// short SSE-inactivity timeout triggers the fallback's own fetch first
// (gated via armMessageGate, blocking it mid-flight); WHILE it is blocked,
// the live overflow arrives and wins the compaction race (forceCompaction
// itself ALSO gated via armSummarizeGate, so there is no additional race
// once both gates are held: forceCompaction cannot possibly return before
// this test explicitly releases it). Releasing messageGate first proves
// the fallback correctly abandons its own stale decision instead of
// finalizing; only then releasing summarizeGate lets the REAL retry
// proceed and complete normally.
func TestCompactionRetry_FallbackAbandonsOnStaleRaceWithLiveRetry(t *testing.T) {
	f := newFakeOpenCodeServer(t)
	f.setSummarizeOK(true)
	messageGate := f.armMessageGate()
	summarizeGate := f.armSummarizeGate()
	// promptGate additionally holds the RETRY's own re-dispatch call (#2)
	// open until this test explicitly releases it -- a LATER audit's own
	// round-2 Finding 3: without this, releasing summarizeGate and then
	// immediately broadcasting the retry's own completion (below) can race
	// attemptCompactionRetry's own ts.setCompacting(false) call, which only
	// runs AFTER the retry's real postPromptAsync HTTP round trip actually
	// returns (§7.2 Finding 3) -- NOT the instant the fake server's own
	// handler merely records the call, which is all waitForCount's own
	// promptCallCount can observe. Gating call #2 explicitly, mirroring
	// TestCompactionRetry_LateCompactionTailEventDuringRetryDispatchIsSuppressed's
	// own precedent, lets this test control that exact ordering
	// deterministically instead of racing it.
	promptGate := f.armPromptAsyncGateForCall(2)

	// A stale snapshot: as if GET /session/{id}/message had been read
	// before the live compaction retry ever touched anything -- still
	// showing the ORIGINAL, not-yet-recovered overflow.
	f.setMessages([]messageListEntry{
		{Info: openCodeMessageInfo{ID: "msg_original", Role: "assistant", Error: &openCodeTaggedError{Name: "ContextOverflowError"}}},
	})

	// Deliberately much shorter than testSSEInactivityTimeout -- must fire
	// the fallback well before any overflow is even broadcast.
	shortInactivity := 50 * time.Millisecond

	a := New(f.URL(), shortInactivity, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	connCtx, connCancel := context.WithTimeout(context.Background(), testWait)
	defer connCancel()
	if err := a.Connected(connCtx); err != nil {
		t.Fatalf("Connected() error = %v", err)
	}
	waitForConnNumber(t, f, 1)

	collector := &eventCollector{}
	promptText := "will overflow while the fallback's own stale fetch is still in flight"
	cmd := sandboxws.Prompt{
		Type: "prompt", MessageId: "m1", SessionId: "sess-fallback-race", Gen: 1,
		Text: promptText,
	}

	// A LATER audit's own round-2 Finding 3: this test's own pollInterval
	// (shortInactivity/ssePollDivisor, ~2ms) means waitForTurn's own ticker
	// fires -- and, once compacting flips back to false after the real
	// retry's own redispatch succeeds but before its completion has been
	// broadcast/dispatched yet, calls finalizeByFallback (a REAL HTTP fetch)
	// -- extremely often for as long as that narrow window lasts. Every one
	// of those calls correctly abandons (compactionAlreadyAttempted() is
	// permanently true from that point on -- the round-2 Finding 2 fix
	// above), so this is harmless BY CONSTRUCTION, never a correctness
	// concern -- but under genuine host contention (confirmed empirically:
	// this exact test observed failing via testWait/ctx expiring, Outcome
	// reported Cancelled instead of Completed, NOT via the wrong-Failed
	// symptom round-1's own fix left unguarded) that harmless polling storm
	// can itself consume enough scheduler time to delay the SSE-reader
	// goroutine's own processing of the retry's scripted completion past a
	// tight ctx budget. Nothing here is unboundedly stuck -- the storm
	// self-terminates the instant that completion is actually dispatched
	// (touch() resets idleFor) -- so a materially longer, still-bounded ctx
	// budget is the correct fix for a "slow under contention", not "hung",
	// failure mode: raceTestWait gives this specific test generous
	// contention headroom without weakening what it actually proves (a
	// GENUINE hang/regression would still be caught, just after a longer
	// wait) -- the fully deterministic, gate-free
	// TestCompactionRetry_FallbackAbandonsWhenAlreadyAttemptedBeforeFetchBegins
	// above already covers the exact correctness property this test proves,
	// with zero timing dependency at all.
	const raceTestWait = 60 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), raceTestWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	ts := waitForTurnRegistered(t, a, "ses_fake")

	// Deterministically wait for waitForTurn's own fallback ticker to have
	// actually reached (and blocked inside) its own fetchFinalMessages call
	// (messageGate) -- a LATER audit's own test-adversarial finding: the
	// fixed wall-clock sleep this replaced only ASSUMED the fetch had
	// already reached and blocked on messageGate by the time it returned,
	// with no direct signal to confirm that; under this machine's own
	// documented heavy concurrent load, that assumption could silently fail
	// (the overflow below would then win tryBeginCompactionRetry BEFORE
	// fetchFinalMessages was even called, flipping ts.compacting true and
	// causing shouldFinalizeByFallback's own isCompacting guard to skip
	// calling finalizeByFallback entirely for the rest of the test --
	// messageGate would then never even be reached, silently never
	// exercising the race this test exists to prove, while still reporting
	// PASS). messageCallCount is incremented BEFORE the fake server's own
	// handler waits on the gate, so waiting for it to reach 1 is a genuine,
	// deterministic proof the fetch is blocked, not an assumption.
	waitForCount(t, "messageCallCount", f.messageCallCount, 1)

	// NOW the live overflow arrives -- races the (already in-flight,
	// gated) fallback fetch exactly as Finding 2 describes.
	f.broadcast(overflowMessageUpdated(t, "ses_fake", "msg_original"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	// forceCompaction has been invoked (and gated) -- this can ONLY happen
	// after tryBeginCompactionRetry already won, i.e. ts.compactionAttempted
	// is already true by this point.
	waitForCount(t, "summarizeCallCount", f.summarizeCallCount, 1)
	if !ts.isCompacting() {
		t.Fatal("ts.isCompacting() = false right after the overflow triggered a compaction attempt, want true")
	}

	// Release the fallback's own stale fetch. With the fix, it must detect
	// that a compaction retry began during its own fetch and abandon its
	// decision entirely -- NOT finalize using the stale ContextOverflowError
	// snapshot above.
	close(messageGate)

	// Give the (just-unblocked) fallback goroutine time to finish deciding
	// -- deterministic regardless of exactly how long this is, since
	// forceCompaction is STILL gated (summarizeGate): there is no live
	// retry activity this could possibly race against right now.
	time.Sleep(100 * time.Millisecond)

	select {
	case <-ts.done:
		t.Fatal("turn finalized via the fallback's own STALE pre-compaction fetch while a real " +
			"compaction retry was already committed to -- Finding 2's own regression")
	default:
	}
	if !ts.isCompacting() {
		t.Fatal("ts.isCompacting() unexpectedly false while forceCompaction is still gated")
	}

	close(summarizeGate) // let the real, gated compaction attempt finally proceed

	// The retry's own re-dispatch (call #2) is still gated (promptGate) --
	// its own call has been recorded (so waitForCount below can observe
	// it), but the ADAPTER's own client-side postPromptAsync call has not
	// yet returned, and therefore ts.setCompacting(false) has not yet run.
	waitForCount(t, "promptCallCount", f.promptCallCount, 2)
	if got := f.lastPromptText(); got != promptText {
		t.Errorf("retried prompt text = %q, want %q", got, promptText)
	}

	// Prove the compaction wave has been fully DRAINED by the SSE-reader
	// goroutine WHILE ts.compacting is still PROVABLY true (see
	// TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce's own
	// doc comment for why this must run BEFORE releasing promptGate).
	waitForDrained(t, f, ts)

	close(promptGate) // let the gated retry dispatch finally return

	// Deterministically wait for compacting to actually flip false (i.e.
	// for attemptCompactionRetry's own postPromptAsync call to have
	// genuinely returned) BEFORE broadcasting the retry's own completion --
	// round-2 Finding 3's own fix, see waitForNotCompacting's own doc
	// comment (fake_server_test.go) for the exact race this closes.
	waitForNotCompacting(t, f, ts)

	// Script the REAL retry's own clean completion.
	f.broadcast(plainAssistantMessageUpdated(t, "ses_fake", "msg_retry"))
	f.broadcast(assistantTextPart(t, "ses_fake", "msg_retry", "prt_retry", "all good now"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}

	// StartTurn's own waitForTurn call already returned earlier -- the
	// instant it called finalizeByFallback once, above, exactly like
	// finalizeOrRecoverFromOverflow's own "tryBeginCompactionRetry succeeds"
	// branch already does when discovered via the LIVE path (see this
	// method's own doc comment): it does not itself block on ts.done once a
	// background recovery attempt has taken over responsibility for
	// finalizing this turn. group.Wait() above already returned (nil, no
	// error) the moment that happened, well before the real retry's own
	// completion was even scripted -- so THIS test must wait on ts.done
	// directly rather than treating group.Wait() as a proxy for "the turn
	// is actually finished".
	select {
	case <-ts.done:
	case <-time.After(raceTestWait):
		t.Fatalf("turn never finalized within raceTestWait after the real retry's own completion was scripted "+
			"(isCompacting=%v promptCallCount=%d summarizeCallCount=%d events=%d)",
			ts.isCompacting(), f.promptCallCount(), f.summarizeCallCount(), len(collector.snapshot()))
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

// TestCompactionRetry_FallbackAbandonsWhenAlreadyAttemptedBeforeFetchBegins
// is a LATER audit's own ROUND-2 Finding 2 regression test:
// TestCompactionRetry_FallbackAbandonsOnStaleRaceWithLiveRetry above only
// ever forces the sub-case where compactionAlreadyAttempted() flips from
// false to true DURING finalizeByFallback's own fetch -- the ORIGINAL fix's
// own "attemptedBefore" snapshot already handled that sub-case correctly.
// The sub-case that fix actually MISSED is this one: a compaction retry
// already won tryBeginCompactionRetry() and is already genuinely in flight
// STRICTLY BEFORE finalizeByFallback is ever even called (so
// attemptedBefore would already read true, before the fetch has even
// started) -- the OLD "!attemptedBefore && ts.compactionAlreadyAttempted()"
// guard evaluates to false in that case and finalizeByFallback wrongly
// proceeds to finalize from a stale snapshot anyway. Proven directly and
// deterministically (no timing/gates needed at all): calls
// ts.tryBeginCompactionRetry() to simulate "a retry already won" BEFORE
// ever calling a.finalizeByFallback, then asserts the fallback abandons
// instead of finalizing.
func TestCompactionRetry_FallbackAbandonsWhenAlreadyAttemptedBeforeFetchBegins(t *testing.T) {
	f := newFakeOpenCodeServer(t)
	f.setSummarizeOK(true)
	// A stale snapshot: as if GET /session/{id}/message had been read
	// showing the ORIGINAL, not-yet-recovered overflow -- the fallback must
	// never trust this once a retry has already been attempted, regardless
	// of when that happened relative to this fetch.
	f.setMessages([]messageListEntry{
		{Info: openCodeMessageInfo{ID: "msg_original", Role: "assistant", Error: &openCodeTaggedError{Name: "ContextOverflowError"}}},
	})

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
		Type: "prompt", MessageId: "m1", SessionId: "sess-already-attempted", Gen: 1,
		Text: "a compaction retry already won before the fallback was ever called at all",
	}
	ts := newTurnState(cmd, collector.sink, "")
	a.registerTurn("ses_fake", ts)
	t.Cleanup(func() { a.unregisterTurn("ses_fake") })

	// Simulate a live compaction retry having ALREADY won
	// tryBeginCompactionRetry (and therefore being genuinely in flight)
	// BEFORE finalizeByFallback is ever called at all -- the exact
	// pre-fetch sub-case the round-1 "attemptedBefore" comparison could
	// never detect, since attemptedBefore itself would already read true.
	if !ts.tryBeginCompactionRetry() {
		t.Fatal("tryBeginCompactionRetry() = false on a fresh turnState, want true")
	}
	if !ts.isCompacting() {
		t.Fatal("isCompacting() = false right after a winning tryBeginCompactionRetry call, want true")
	}

	a.finalizeByFallback(context.Background(), "ses_fake", ts)

	select {
	case <-ts.done:
		t.Fatal("finalizeByFallback finalized the turn despite compactionAlreadyAttempted() already being " +
			"true BEFORE its own fetch even started -- round-2 Finding 2's own regression (the stale " +
			"pre-fetch sub-case the original before/after comparison could not detect)")
	default:
	}
	if !ts.isCompacting() {
		t.Error("isCompacting() = false after finalizeByFallback should have abandoned without touching ts at all")
	}
	if got := f.summarizeCallCount(); got != 0 {
		t.Errorf("summarizeCallCount = %d, want exactly 0 (finalizeByFallback must not itself have launched "+
			"anything -- it only ever abandons here)", got)
	}
}

// TestCompactionRetry_FallbackReleaseRacesLiveOverflowAtomically is a LATER
// audit's own regression test for the TOCTOU Finding 1 identified:
// finalizeByFallback's own "abandon if a retry is live/stale" guards used to
// be evaluated in a SEPARATE critical section from the
// tryBeginCompactionRetry check-and-act they exist to protect — a live SSE
// session.idle for the SAME first-time overflow that began (touched ts,
// checked isCompacting, won tryBeginCompactionRetry) strictly AFTER
// finalizeByFallback's own checks read clean, but BEFORE finalizeByFallback's
// own call actually reached tryBeginCompactionRetry, was invisible to those
// checks — finalizeByFallback would then lose tryBeginCompactionRetry, fall
// into the "already attempted" branch, and finalize using its STALE
// pre-retry snapshot (with the misleading "a compaction retry was already
// attempted this turn; the retried prompt also overflowed" reason) even
// though the just-launched real retry had not even called forceCompaction
// yet — discarding a legitimate in-flight recovery attempt and reporting a
// wrong outcome.
//
// Unlike TestCompactionRetry_FallbackAbandonsOnStaleRaceWithLiveRetry above
// (which deterministically sequences the live retry to have ALREADY won
// tryBeginCompactionRetry, and to already be gated behind armSummarizeGate,
// before ever releasing messageGate — so finalizeByFallback's own
// isCompacting() check alone already reads true, never actually forcing the
// narrower gap Finding 1 describes), this test releases the fallback's own
// gated fetch and broadcasts the live overflow event from two goroutines
// synchronized on a common barrier, so which one reaches
// turnState.resolveOverflowAction's single ts.mu-guarded decision point
// first is left to genuine goroutine scheduling — exactly the interleaving
// the fix must handle correctly regardless of which side wins:
//
//   - If the live SSE dispatch reaches the decision point first: it wins
//     tryBeginCompactionRetry (overflowActionBeginRetry) and
//     finalizeByFallback's own later arrival at the SAME decision point
//     atomically observes ts.compacting==true (or ts.lastActivity already
//     advanced past preFetchActivity) and abandons (overflowActionStale) —
//     never finalizing at all.
//   - If finalizeByFallback reaches the decision point first: it correctly
//     wins the retry itself instead (using its own, in this exact
//     interleaving still-fresh, stale-looking snapshot's error, which is
//     the same ContextOverflowError the live path would have used anyway)
//     and the live SSE dispatch's own prior isCompacting() guard (sse.go)
//     then correctly suppresses its own attempt to reach the decision point
//     at all.
//
// Either way, exactly ONE compaction attempt is ever launched and the turn
// must go on to finalize Completed via that attempt's own real success —
// NEVER a premature Failed carrying the "already attempted... retried
// prompt also overflowed" reason while the real retry was still (or never
// even) genuinely running. Run with -count=N (see this package's own test
// instructions) to exercise both interleavings across repeated runs, since
// which goroutine wins this particular race is not itself deterministic —
// what the assertions below guarantee IS deterministic regardless of who
// wins.
func TestCompactionRetry_FallbackReleaseRacesLiveOverflowAtomically(t *testing.T) {
	f := newFakeOpenCodeServer(t)
	f.setSummarizeOK(true)
	messageGate := f.armMessageGate()
	summarizeGate := f.armSummarizeGate()
	// promptGate holds the retry's own re-dispatch (call #2) open until
	// released -- see TestCompactionRetry_FallbackAbandonsOnStaleRaceWithLiveRetry's
	// own doc comment (round-2 Finding 3) for why this matters.
	promptGate := f.armPromptAsyncGateForCall(2)
	// Guarantee every gate is closed exactly once even if an assertion below
	// calls t.Fatalf before this test's own explicit close calls are
	// reached -- otherwise the fake server's own gated handler goroutines
	// stay blocked forever, and f.srv.Close() (registered by
	// newFakeOpenCodeServer, which runs AFTER these thanks to t.Cleanup's own
	// LIFO ordering) would hang the whole test binary waiting for those
	// outstanding requests to finish. messageGate itself is always closed
	// unconditionally by its own barrier goroutine below regardless of any
	// assertion outcome, so it needs no such guard.
	var closeSummarizeGateOnce, closePromptGateOnce sync.Once
	closeSummarizeGate := func() { closeSummarizeGateOnce.Do(func() { close(summarizeGate) }) }
	closePromptGate := func() { closePromptGateOnce.Do(func() { close(promptGate) }) }
	t.Cleanup(closePromptGate)
	t.Cleanup(closeSummarizeGate)

	// A stale snapshot: as if GET /session/{id}/message had been read
	// before the live compaction retry ever touched anything -- still
	// showing the ORIGINAL, not-yet-recovered overflow.
	f.setMessages([]messageListEntry{
		{Info: openCodeMessageInfo{ID: "msg_original", Role: "assistant", Error: &openCodeTaggedError{Name: "ContextOverflowError"}}},
	})

	// Deliberately much shorter than testSSEInactivityTimeout -- must fire
	// the fallback well before any overflow is even broadcast.
	shortInactivity := 50 * time.Millisecond

	a := New(f.URL(), shortInactivity, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	connCtx, connCancel := context.WithTimeout(context.Background(), testWait)
	defer connCancel()
	if err := a.Connected(connCtx); err != nil {
		t.Fatalf("Connected() error = %v", err)
	}
	waitForConnNumber(t, f, 1)

	collector := &eventCollector{}
	promptText := "overflow racing the fallback's own stale-fetch release against the live event as simultaneously as possible"
	cmd := sandboxws.Prompt{
		Type: "prompt", MessageId: "m1", SessionId: "sess-atomic-race", Gen: 1,
		Text: promptText,
	}

	// See TestCompactionRetry_FallbackAbandonsOnStaleRaceWithLiveRetry's own
	// doc comment for why a generous, still-bounded ctx budget (rather than
	// testWait) is the right tool for a test whose own timing is
	// deliberately racy under host contention.
	const raceTestWait = 60 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), raceTestWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	ts := waitForTurnRegistered(t, a, "ses_fake")

	// Deterministically wait for waitForTurn's own fallback ticker to have
	// actually reached (and blocked inside) its own fetchFinalMessages call
	// (messageGate), holding its own preFetchActivity snapshot from before
	// any of the barrier-released activity below -- see
	// TestCompactionRetry_FallbackAbandonsOnStaleRaceWithLiveRetry's own doc
	// comment for why a fixed sleep here would only assume, never prove,
	// that the fetch is genuinely blocked.
	waitForCount(t, "messageCallCount", f.messageCallCount, 1)

	// Release the fallback's own stale fetch AND broadcast the live
	// overflow from two goroutines synchronized on a common barrier, so
	// neither is sequenced strictly before the other -- see this test's own
	// doc comment for exactly which interleaving this forces and why both
	// are safe under the fix.
	// errgroup.Group rather than bare `go` statements: §11's
	// no-naked-goroutine rule is lint-enforced (tools/lint/narvichecks) and
	// applies to test code too. The Wait is deferred, not immediate --
	// these two are deliberately fire-and-forget racers, and the
	// synchronization the assertions below actually depend on is the
	// waitForCount polls, not either goroutine's own completion. Waiting
	// here instead would serialize the very race this test exists to force.
	var startRace sync.WaitGroup
	startRace.Add(2)
	var raceGroup errgroup.Group
	raceGroup.Go(func() error {
		startRace.Done()
		startRace.Wait()
		close(messageGate)
		return nil
	})
	raceGroup.Go(func() error {
		startRace.Done()
		startRace.Wait()
		f.broadcast(overflowMessageUpdated(t, "ses_fake", "msg_original"))
		f.broadcast(sessionIdleLine(t, "ses_fake"))
		return nil
	})
	defer func() { _ = raceGroup.Wait() }()

	// Exactly one compaction attempt must ever be launched, regardless of
	// which side won the race to claim it.
	waitForCount(t, "summarizeCallCount", f.summarizeCallCount, 1)

	// A generous extra wait to give a SECOND, wrongly-launched compaction
	// attempt (the historical bug's own signature: the "losing" side
	// incorrectly falling through to "already attempted" and finalizing
	// instead of cleanly abandoning, while ALSO having raced a legitimate
	// attempt into existence) a real chance to show up before asserting the
	// negative below -- there is no direct "no more attempts are coming"
	// signal to poll for instead.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if f.summarizeCallCount() > 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := f.summarizeCallCount(); got != 1 {
		t.Fatalf("summarizeCallCount = %d, want exactly 1 regardless of which goroutine won the release-vs-broadcast "+
			"race (Finding 1's own regression: a losing fallback used to finalize with a stale snapshot instead of "+
			"cleanly abandoning)", got)
	}

	closeSummarizeGate() // let the winning, gated compaction attempt finally proceed

	waitForCount(t, "promptCallCount", f.promptCallCount, 2)
	if got := f.lastPromptText(); got != promptText {
		t.Errorf("retried prompt text = %q, want %q", got, promptText)
	}

	// Prove the compaction wave has been fully DRAINED by the SSE-reader
	// goroutine WHILE ts.compacting is still PROVABLY true (see
	// TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce's own
	// doc comment for why this must run BEFORE releasing promptGate).
	waitForDrained(t, f, ts)

	closePromptGate() // let the gated retry dispatch finally return

	waitForNotCompacting(t, f, ts)

	// Script the REAL retry's own clean completion.
	f.broadcast(plainAssistantMessageUpdated(t, "ses_fake", "msg_retry"))
	f.broadcast(assistantTextPart(t, "ses_fake", "msg_retry", "prt_retry", "all good now"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}

	select {
	case <-ts.done:
	case <-time.After(raceTestWait):
		t.Fatalf("turn never finalized within raceTestWait after the real retry's own completion was scripted "+
			"(isCompacting=%v promptCallCount=%d summarizeCallCount=%d events=%d)",
			ts.isCompacting(), f.promptCallCount(), f.summarizeCallCount(), len(collector.snapshot()))
	}

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeCompleted {
		reason := "<nil>"
		if final.Reason != nil {
			reason = *final.Reason
		}
		t.Errorf("execution_complete.Outcome = %q, want %q (reason=%s) -- a premature 'already attempted' finalize "+
			"from finalizeByFallback's own stale pre-fetch snapshot (Finding 1's own regression) would report "+
			"Failed here instead, discarding the legitimate retry mid-flight",
			final.Outcome, sandboxws.ExecutionCompleteOutcomeCompleted, reason)
	}
}

// TestCompactionRetry_SilentRetryStillFinalizesViaFallback is a diagnosis
// proof, not a documented "Finding": Batch B5's own Finding 2 fix made
// finalizeByFallback abandon (return without finalizing at all, adapter.go)
// whenever ts.compactionAlreadyAttempted() reads true after its own
// fetchFinalMessages call. But compactionAttempted (turn.go) is a ONE-WAY
// LATCH -- tryBeginCompactionRetry sets it true and NOTHING ever clears it
// back to false, including long after the retry it once guarded has fully
// completed. finalizeByFallback's own check cannot tell "a retry is in
// flight for THIS overflow right now" apart from "a retry happened at SOME
// point during this turn's entire life, and has long since finished" -- it
// treats both exactly the same: abandon.
//
// This test drives exactly the ordinary POST-RETRY STEADY STATE that
// exposes the difference: overflow -> compaction succeeds -> the retried
// prompt is re-dispatched -> ts.compacting clears back to false (the retry
// is no longer "in flight" by any reasonable reading) -- and then the
// RETRIED prompt's own SSE traffic goes silent forever (the stream never
// emits session.idle/session.error for it at all), exactly the scenario
// the SSE-inactivity fallback exists to rescue.
//
//   - shouldFinalizeByFallback (adapter.go) correctly observes
//     isCompacting()==false and idleFor(sseInactivityTimeout)==true (no
//     disconnect ever occurred), so it fires, exactly as intended.
//   - finalizeByFallback fetches the final-state snapshot, then reads
//     ts.compactionAlreadyAttempted()==true -- true FOREVER since the
//     earlier, already-fully-resolved retry, not because anything is
//     currently in flight -- and abandons without finalizing.
//   - waitForTurn (adapter.go), since Finding 2 also stopped it from
//     returning unconditionally after calling finalizeByFallback, sees
//     ts.done still open and keeps polling. The NEXT tick reaches the
//     exact same conclusion, forever.
//
// The turn can therefore never finalize via the fallback at all in this
// state -- only StartTurn's own ctx expiring can ever end it, reporting a
// misleading "Cancelled / turn context canceled before completion" instead
// of the fallback's own honest, much faster real outcome. Uses a
// deliberately short, test-local sseInactivityTimeout (NOT the shared
// testSSEInactivityTimeout constant, which is generous specifically so the
// fallback almost never fires in every OTHER test in this file) so the
// fallback's own real threshold can actually be reached well inside a ctx
// budget generous enough to also make the bug's own hang-to-ctx-deadline
// failure mode unambiguous when it happens.
//
// Gates the compaction retry's own re-dispatch (call #2,
// armPromptAsyncGateForCall) purely to make the compaction-success wave's
// own drain deterministic before this test's steady state is reached -- see
// TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce's own
// doc comment for the race an ungated retry here would otherwise share:
// the wave could be misprocessed by the SSE-reader AFTER ts.compacting
// cleared, misreading its own internal session.idle as a premature
// completion instead of ever reaching this test's actual "goes silent
// forever" steady state at all.
func TestCompactionRetry_SilentRetryStillFinalizesViaFallback(t *testing.T) {
	f := newFakeOpenCodeServer(t)
	f.setSummarizeOK(true)
	promptGate := f.armPromptAsyncGateForCall(2)
	var closePromptGateOnce sync.Once
	closePromptGate := func() { closePromptGateOnce.Do(func() { close(promptGate) }) }
	t.Cleanup(closePromptGate)

	// Deliberately short so the SSE-inactivity fallback's own real
	// threshold is reached in well under a second -- see this test's own
	// doc comment for why this is a test-local override, not
	// testSSEInactivityTimeout.
	const shortSSEInactivityTimeout = 150 * time.Millisecond

	a := New(f.URL(), shortSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	connCtx, connCancel := context.WithTimeout(context.Background(), testWait)
	defer connCancel()
	if err := a.Connected(connCtx); err != nil {
		t.Fatalf("Connected() error = %v", err)
	}
	waitForConnNumber(t, f, 1)

	collector := &eventCollector{}
	cmd := sandboxws.Prompt{
		Type: "prompt", MessageId: "m1", SessionId: "sess-silent-retry", Gen: 1,
		Text: "will overflow, compaction succeeds, the retry is re-dispatched, then goes silent forever",
	}

	// A ctx budget generously above shortSSEInactivityTimeout (by more
	// than an order of magnitude) so this test can actually DISTINGUISH
	// "finalized fast, via the fallback" from "hung all the way to this
	// ctx's own unrelated deadline" -- the bug's own failure mode -- rather
	// than the two becoming indistinguishable because the budgets happen
	// to be close together.
	const ctxBudget = 3 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), ctxBudget)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	ts := waitForTurnRegistered(t, a, "ses_fake")

	f.broadcast(overflowMessageUpdated(t, "ses_fake", "msg_original"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	waitForCount(t, "summarizeCallCount", f.summarizeCallCount, 1)

	// The retry's own re-dispatch (call #2) is now gated/blocked -- proving
	// the compaction-success wave has already been queued onto this
	// connection, and that ts.compacting is GUARANTEED still true for as
	// long as promptGate stays closed.
	waitForCount(t, "promptCallCount", f.promptCallCount, 2)

	// Prove the compaction wave has been fully DRAINED by the SSE-reader
	// goroutine WHILE ts.compacting is still PROVABLY true (see
	// TestCompactionRetry_RetryAlsoOverflowsFinalizesFailedExactlyOnce's own
	// doc comment for why this must run BEFORE releasing promptGate).
	waitForDrained(t, f, ts)

	closePromptGate() // let the gated retry dispatch finally return

	// Wait for ts.compacting to have cleared back to false -- the ordinary
	// steady state this test targets: overflow -> compaction succeeded ->
	// retry re-dispatched -> now waiting on the RETRIED prompt's own
	// session.idle, exactly like any normal turn.
	waitForNotCompacting(t, f, ts)

	if !ts.compactionAlreadyAttempted() {
		t.Fatal("ts.compactionAlreadyAttempted() = false after a completed compaction retry, want true " +
			"(this test's own steady-state precondition -- the latch this diagnosis is about)")
	}

	// Deliberately broadcast NOTHING further: the retried prompt's own SSE
	// traffic goes silent forever -- no session.idle, no session.error,
	// nothing -- exactly the scenario the SSE-inactivity fallback exists to
	// rescue.

	started := time.Now()
	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	elapsed := time.Since(started)

	final := lastExecutionComplete(t, collector.snapshot())
	reason := "<nil>"
	if final.Reason != nil {
		reason = *final.Reason
	}

	// THE headline assertion: the turn must finalize via the
	// SSE-inactivity fallback -- well under the ctx budget -- reporting a
	// real, honest outcome, NOT by hanging all the way to ctxBudget and
	// reporting Cancelled. A generous bound (well above
	// shortSSEInactivityTimeout, well below ctxBudget) keeps this robust
	// to ordinary scheduling jitter while still cleanly separating "the
	// fallback fired" from "we hit the ctx deadline".
	const wantWithin = 1500 * time.Millisecond
	if elapsed > wantWithin {
		t.Errorf("StartTurn took %s to return after the retried prompt went silent forever -- want a prompt "+
			"finalize via the SSE-inactivity fallback (bounded by shortSSEInactivityTimeout=%s), well within "+
			"%s, not a hang all the way to the unrelated %s ctx deadline (outcome=%q, reason=%s)",
			elapsed, shortSSEInactivityTimeout, wantWithin, ctxBudget, final.Outcome, reason)
	}
	if final.Outcome == sandboxws.ExecutionCompleteOutcomeCancelled {
		t.Errorf("execution_complete.Outcome = %q, want a REAL outcome from the SSE-inactivity fallback "+
			"(e.g. %q), not %q via ctx-deadline exhaustion (reason=%s) -- finalizeByFallback's own "+
			"unconditional post-fetch compactionAlreadyAttempted() check (adapter.go) cannot tell a retry "+
			"that is genuinely in flight apart from one that fully completed long ago, since "+
			"compactionAttempted (turn.go) is a one-way latch that is never cleared",
			final.Outcome, sandboxws.ExecutionCompleteOutcomeFailed, final.Outcome, reason)
	}
}
