package opencode

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// Small, fast timeouts scoped to THESE fake-server-backed tests only --
// unlike testSSEInactivityTimeout (helpers_test.go's own deliberately
// generous 45s, tuned for a REAL opencode binary competing for CPU under
// full-suite `go test ./...` load), the tests below construct their own
// timing entirely deterministically via fakeOpenCodeServer, which never
// contends for CPU the way spawning a real process does -- a short,
// fast-test-friendly value is both safe and desirable here.
const (
	livenessSSEInactivityTimeout = 300 * time.Millisecond
	livenessReconnectInterval    = 30 * time.Millisecond
	livenessRequestTimeout       = 2 * time.Second

	// livenessBoundedWaitCtxTimeout bounds
	// TestWaitForTurn_ConnectionNeverReturnsFallsBackWithinBoundedWait's
	// own outer ctx -- generous relative to
	// fallbackReconnectGraceMultiplier*livenessSSEInactivityTimeout (2 *
	// 300ms = 600ms), so a CORRECT bounded-wait implementation finishes
	// with huge margin to spare, while a regression to an unbounded wait
	// still only costs this one test a few seconds (bounded by
	// waitForTurn's own ctx.Done() finalize path, Finding 5) rather than
	// hanging forever.
	livenessBoundedWaitCtxTimeout = 3 * time.Second
)

// reconnectRaceSSEInactivityTimeout/reconnectRaceReconnectInterval are
// scoped to TestWaitForTurn_ReconnectWinsRaceAgainstFallback alone --
// deliberately NOT the shared livenessSSEInactivityTimeout/
// livenessReconnectInterval above (30ms, much SHORTER than
// livenessSSEInactivityTimeout's 300ms). That test's whole point is to
// construct an outage that crosses BOTH ts.idleFor(sseInactivityTimeout)
// AND stays down long enough that the eventual reconnect's own bare
// handshake lands only AFTER that threshold was already crossed --
// exactly the gap a 30ms reconnect can never produce, since reconnecting
// that fast never lets the turn go idle before reconnecting at all.
//
// reconnectRaceReconnectInterval is deliberately 1.5x
// reconnectRaceSSEInactivityTimeout: comfortably longer than the plain
// idle threshold (so the outage necessarily crosses it before the
// reconnect lands) while comfortably shorter than
// fallbackReconnectGraceMultiplier's own 2x bound (so there is real room,
// after the reconnect lands, for this test's own assertions and its own
// real session.idle event to arrive within the grace window rather than
// racing the bound itself).
const (
	reconnectRaceSSEInactivityTimeout = 400 * time.Millisecond
	reconnectRaceReconnectInterval    = 600 * time.Millisecond
)

func newLivenessAdapter(t *testing.T, fake *fakeOpenCodeServer) *Adapter {
	t.Helper()
	a := New(fake.URL(), livenessSSEInactivityTimeout, livenessReconnectInterval, livenessRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)
	return a
}

func assistantTextEvents(t *testing.T, sessionID, text string) []string {
	t.Helper()
	return []string{
		sseLine(t, "message.updated", messageUpdatedProps{
			SessionID: sessionID,
			Info:      openCodeMessageInfo{ID: "msg_1", Role: "assistant"},
		}),
		sseLine(t, "message.part.updated", messagePartUpdatedProps{
			SessionID: sessionID,
			Part:      textPartJSON(t, "prt_1", "msg_1", text),
		}),
	}
}

func textPartJSON(t *testing.T, id, messageID, text string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"id": id, "messageID": messageID, "type": "text", "text": text,
	})
	if err != nil {
		t.Fatalf("marshal text part: %v", err)
	}
	return raw
}

// TestWaitForTurn_ReconnectWinsRaceAgainstFallback is Finding 1/2's own
// central regression test, and specifically the reviewer-reproduced gap a
// prior version of this test never actually reached: it drives the outage
// past BOTH ts.idleFor(sseInactivityTimeout) (this turn's own idle clock)
// and reconnectRaceReconnectInterval (so the reconnect itself only lands
// once that idle threshold was already crossed), then confirms the bare
// server.connected handshake the reconnect delivers -- with no
// turn-specific event yet -- does NOT, by itself, finalize the turn on the
// very next poll tick. Only once the turn's own REAL outcome (session.idle)
// actually arrives does it finalize, still within the
// fallbackReconnectGraceMultiplier grace window.
//
// This is exactly the confirmed bug: shouldFinalizeByFallback's old
// globalIdleFor-based check treated "the stream looks fresh right now" as
// proof the turn was fine, which a bare reconnect handshake trivially
// satisfies regardless of whether THIS turn has had any chance yet to
// receive its own completion event. The fix (Adapter.disconnectedSince)
// instead asks whether a genuine disconnect happened at any point during
// this turn's own current idle episode -- true here regardless of whether
// reconnection has since succeeded -- so it must keep waiting.
//
// Distinguishes the two possible outcomes concretely: the fake server's
// own GET /session/{id}/message fallback target is configured to derive
// "failed" (empty parts) via deriveOutcome, while the REAL session.idle
// path (fed by a genuine assistant text part dispatched before the drop)
// derives "completed". Observing "completed" here can only mean the real
// session.idle path won; observing "failed" would mean the fallback fired
// prematurely -- the exact bug this batch fixes.
func TestWaitForTurn_ReconnectWinsRaceAgainstFallback(t *testing.T) {
	fake := newFakeOpenCodeServer(t)
	a := New(fake.URL(), reconnectRaceSSEInactivityTimeout, reconnectRaceReconnectInterval, livenessRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	connectCtx, connectCancel := context.WithTimeout(context.Background(), testWait)
	defer connectCancel()
	if err := a.Connected(connectCtx); err != nil {
		t.Fatalf("Connected() error = %v", err)
	}
	<-fake.connected // consume the initial connection's own notification

	// Configured to derive "failed" via deriveOutcome (empty parts) -- if
	// the fallback fires here (the bug), the test observes this outcome
	// instead of the real one.
	fake.setMessages([]messageListEntry{{Parts: []json.RawMessage{}}})

	collector := &eventCollector{}
	sessionID := "ses_fake"
	cmd := sandboxws.Prompt{Type: "prompt", MessageId: "m1", SessionId: "sess-1", Gen: 1, Text: "hi"}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	ts := waitForTurnRegistered(t, a, sessionID)

	// Establish a real assistant text part BEFORE the drop, so the turn's
	// own eventual session.idle derives "completed" via genuine sawText --
	// not the fallback's own differently-configured "failed" snapshot, and
	// so this turn has a fixed lastActivity baseline to measure the outage
	// against. waitForSawText confirms the adapter has actually FINISHED
	// processing the broadcast text part before the connection drops --
	// otherwise the fake server's own asynchronous relay of already-queued
	// SSE lines could race the very next dropConnection call below.
	for _, line := range assistantTextEvents(t, sessionID, "hello from before the drop") {
		fake.broadcast(line)
	}
	waitForSawText(t, ts)

	fake.dropConnection()

	// reconnectRaceReconnectInterval (600ms) is deliberately longer than
	// reconnectRaceSSEInactivityTimeout (400ms): by the time the reconnect
	// actually lands, this turn has ALREADY been idle past its own plain
	// threshold for a while -- a genuine outage that occurred DURING this
	// turn's own current silence, not a point-in-time snapshot taken only
	// once reconnected. waitForConnNumber blocks until that reconnect (the
	// 2nd /event connection) has actually landed.
	waitForConnNumber(t, fake, 2)

	// The reconnect delivers ONLY the bare server.connected handshake at
	// this point (dispatchEvent's own "server.connected" case never calls
	// ts.touch) -- no turn-specific event has arrived yet. Give the poll
	// loop several ticks to react to the now-reconnected stream, then
	// confirm it did NOT finalize the turn on the strength of the reconnect
	// alone -- fallbackReconnectGraceMultiplier's own 2x bound (800ms from
	// this turn's own last activity) must still be comfortably in the
	// future at this point.
	//
	// A LATER audit's own test-adversarial finding: sleeping a FIXED
	// nominal amount here (assuming the reconnect landed "around the 600ms
	// mark" and budgeting only ~100ms of margin against the 800ms bound)
	// left only ~100ms of load-independent headroom -- under this
	// machine's own documented heavy concurrent-workload conditions
	// (scheduling delays, GC pauses, or the reconnect itself landing later
	// than the nominal 600ms), that margin could be consumed, making the
	// assertion below a genuine flake unrelated to any real regression.
	// Computed from ts.lastActivityTime() directly instead -- the SAME
	// real clock shouldFinalizeByFallback itself compares against -- so
	// this sleeps only as long as still leaves a comfortable, fixed
	// safety margin before the actual 800ms bound, regardless of how late
	// the reconnect itself actually landed.
	const reconnectRaceSafetyMargin = 250 * time.Millisecond
	graceBound := time.Duration(fallbackReconnectGraceMultiplier) * reconnectRaceSSEInactivityTimeout
	sinceLastActivity := time.Since(ts.lastActivityTime())
	sleepFor := 6 * (reconnectRaceSSEInactivityTimeout / ssePollDivisor)
	if remaining := graceBound - sinceLastActivity - reconnectRaceSafetyMargin; remaining < sleepFor {
		if remaining < 0 {
			remaining = 0
		}
		sleepFor = remaining
	}
	time.Sleep(sleepFor)
	select {
	case <-ts.done:
		t.Fatal("turn finalized immediately after reconnect (bare handshake only, no turn-specific event yet) -- " +
			"reconnecting must not, by itself, prove this turn's own outcome is ready; this is the confirmed bug " +
			"this test guards against")
	default:
	}

	// Now let the turn's own REAL outcome arrive, still within the grace
	// window -- it must win over the fallback's own differently-configured
	// "failed" snapshot.
	fake.broadcast(sseLine(t, "session.idle", sessionIdleProps{SessionID: sessionID}))

	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeCompleted {
		reason := "<nil>"
		if final.Reason != nil {
			reason = *final.Reason
		}
		t.Errorf("execution_complete.Outcome = %q (reason=%s), want %q -- the real session.idle path (after reconnect) "+
			"must win, not the fallback (which was configured to read as failed)",
			final.Outcome, reason, sandboxws.ExecutionCompleteOutcomeCompleted)
	}
}

// TestWaitForTurn_GenuinelyStuckTurnStillFallsBackWithinOriginalTimeout
// proves Finding 1/2's own disambiguation does NOT regress the
// already-correct case: the connection never actually drops at all (kept
// alive via periodic server.heartbeat, so Adapter.disconnectedSince never
// observes a genuine disconnect and stays false throughout), while THIS
// turn specifically never receives session.idle. The fallback must still
// fire, and within roughly the ORIGINAL sseInactivityTimeout window -- not
// wait for the (inapplicable here) 2x reconnect-grace bound.
func TestWaitForTurn_GenuinelyStuckTurnStillFallsBackWithinOriginalTimeout(t *testing.T) {
	fake := newFakeOpenCodeServer(t)
	a := newLivenessAdapter(t, fake)

	connectCtx, connectCancel := context.WithTimeout(context.Background(), testWait)
	defer connectCancel()
	if err := a.Connected(connectCtx); err != nil {
		t.Fatalf("Connected() error = %v", err)
	}
	<-fake.connected

	stopHeartbeats := make(chan struct{})
	var hbGroup errgroup.Group
	hbGroup.Go(func() error {
		ticker := time.NewTicker(livenessReconnectInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				fake.broadcast(sseLine(t, "server.heartbeat", struct{}{}))
			case <-stopHeartbeats:
				return nil
			}
		}
	})
	defer func() {
		close(stopHeartbeats)
		_ = hbGroup.Wait()
	}()

	// A real, non-empty assistant reply -- so observing "completed" here
	// can only have come from the FALLBACK's own final-message fetch
	// (session.idle for this turn is never sent at all).
	fake.setMessages([]messageListEntry{{Parts: []json.RawMessage{textPartJSON(t, "prt_1", "msg_1", "done")}}})

	collector := &eventCollector{}
	cmd := sandboxws.Prompt{Type: "prompt", MessageId: "m1", SessionId: "sess-1", Gen: 1, Text: "hi"}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	start := time.Now()
	if _, err := a.StartTurn(ctx, cmd, collector.sink, nil); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	elapsed := time.Since(start)

	// Generous upper bound: comfortably above the plain sseInactivityTimeout
	// (scheduling jitter under -race), but well below what waiting for the
	// (here, inapplicable) 2x reconnect-grace bound would take -- proving
	// this genuinely-stuck-turn case is NOT being routed through the
	// "connection also looks dead" branch.
	if want := 3 * livenessSSEInactivityTimeout / 2; elapsed > want {
		t.Errorf("StartTurn() took %v to finalize a genuinely stuck turn (heartbeats kept the connection alive throughout), "+
			"want under %v -- the original fallback path must fire promptly, not wait for the 2x reconnect-grace bound", elapsed, want)
	}

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeCompleted {
		t.Errorf("execution_complete.Outcome = %q, want %q (via the fallback's own final-message fetch)",
			final.Outcome, sandboxws.ExecutionCompleteOutcomeCompleted)
	}
}

// TestWaitForTurn_GenuinelyStuckTurnFallbackFailureCarriesDiagnostic is an
// audit fix (§7.3, C1): finalizeByFallback (adapter.go) has its OWN,
// SEPARATE call site that builds ProviderFailureDiagnostic --
// `outcome.Diagnostic = a.buildProviderFailureDiagnostic(last.Info.Error,
// ts)` -- reached only via the SSE-inactivity fallback's own final-message
// fetch, never by dispatchEvent's live "session.idle" case
// (transientretry_test.go's own TestTransientRetry_PermanentAPIErrorNeverRetried
// covers THAT one). Before this test existed, deleting finalizeByFallback's
// own call site left every test in this package green: nothing exercised a
// genuine provider failure observed through the fallback's own final-state
// fetch specifically. Mirrors
// TestWaitForTurn_GenuinelyStuckTurnStillFallsBackWithinOriginalTimeout's
// own exact scaffolding (heartbeats keep the connection looking alive;
// session.idle for this turn is never sent; the fallback's own fetch is
// the only thing that can possibly produce this turn's own terminal
// event) but with the fetched message's own info.error set to a real,
// permanent (non-retryable) APIError instead of a plain text reply.
func TestWaitForTurn_GenuinelyStuckTurnFallbackFailureCarriesDiagnostic(t *testing.T) {
	fake := newFakeOpenCodeServer(t)
	a := newLivenessAdapter(t, fake)

	connectCtx, connectCancel := context.WithTimeout(context.Background(), testWait)
	defer connectCancel()
	if err := a.Connected(connectCtx); err != nil {
		t.Fatalf("Connected() error = %v", err)
	}
	<-fake.connected

	stopHeartbeats := make(chan struct{})
	var hbGroup errgroup.Group
	hbGroup.Go(func() error {
		ticker := time.NewTicker(livenessReconnectInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				fake.broadcast(sseLine(t, "server.heartbeat", struct{}{}))
			case <-stopHeartbeats:
				return nil
			}
		}
	})
	defer func() {
		close(stopHeartbeats)
		_ = hbGroup.Wait()
	}()

	statusCode := 503
	fake.setMessages([]messageListEntry{{
		Info: openCodeMessageInfo{
			ID:   "msg_1",
			Role: "assistant",
			Error: &openCodeTaggedError{
				Name: "APIError",
				Data: &openCodeErrorData{
					Message:    "the fallback's own fetch observed this permanent failure",
					StatusCode: &statusCode,
				},
			},
			// ModelID/ProviderID: the ENGINE's own report of which model
			// produced this message -- audit fix (§7.3, A2): this is the
			// ONLY source ProviderFailureDiagnostic.Model now reads from
			// (diagnostic.go), precisely because cmd.Model below is nil
			// (see its own comment) and the wire request correctly omits
			// the "model" field entirely on that path (resolveModel,
			// session.go) -- there is no request-side model to fall back
			// to here, by design, so this field is the only way this test
			// can prove Model still populates on the default,
			// no-model-requested configuration.
			ModelID:    "claude-sonnet-4-5",
			ProviderID: "anthropic",
		},
	}})

	collector := &eventCollector{}
	// No modelId (the default configuration, C2) -- see
	// TestTransientRetry_PermanentAPIErrorNeverRetried's own identical
	// choice for why.
	cmd := sandboxws.Prompt{Type: "prompt", MessageId: "m1", SessionId: "sess-1", Gen: 1, Text: "hi"}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	if _, err := a.StartTurn(ctx, cmd, collector.sink, nil); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeFailed {
		t.Fatalf("execution_complete.Outcome = %q, want %q (via the fallback's own final-message fetch)",
			final.Outcome, sandboxws.ExecutionCompleteOutcomeFailed)
	}
	if final.Diagnostic == nil {
		t.Fatal("execution_complete.Diagnostic = nil, want the allowlisted provider-failure record " +
			"(finalizeByFallback's own call site, adapter.go, was never reached)")
	}
	if final.Diagnostic.UnionMember == nil || *final.Diagnostic.UnionMember != "APIError" {
		t.Errorf("Diagnostic.UnionMember = %v, want %q", final.Diagnostic.UnionMember, "APIError")
	}
	if final.Diagnostic.Message == nil || *final.Diagnostic.Message != "the fallback's own fetch observed this permanent failure" {
		t.Errorf("Diagnostic.Message = %v, want the fetched message's own error text", final.Diagnostic.Message)
	}
	if final.Diagnostic.StatusCode == nil || *final.Diagnostic.StatusCode != statusCode {
		t.Errorf("Diagnostic.StatusCode = %v, want %d", final.Diagnostic.StatusCode, statusCode)
	}
	// audit fix (§7.3, A2): Model comes from the fetched message's own
	// engine-reported ModelID/ProviderID above, NOT from cmd.Model (nil
	// here) or any request-side resolution -- proving this default,
	// no-model-requested configuration still gets a real, accurate Model
	// on the diagnostic, sourced the correct way.
	if want := "anthropic/claude-sonnet-4-5"; final.Diagnostic.Model == nil || *final.Diagnostic.Model != want {
		t.Errorf("Diagnostic.Model = %v, want %q (the fetched message's own engine-reported model)", final.Diagnostic.Model, want)
	}
}

// TestWaitForTurn_ConnectionNeverReturnsFallsBackWithinBoundedWait proves
// the "connection also looks dead" wait is genuinely BOUNDED, not
// infinite: once the connection drops and every subsequent reconnect
// attempt is rejected (never comes back), the fallback must still
// eventually fire, at roughly fallbackReconnectGraceMultiplier (2x)
// sseInactivityTimeout -- comfortably before livenessBoundedWaitCtxTimeout,
// this test's own outer ctx deadline, which exists only as a safety net
// (see its own doc comment) so a genuine regression to an unbounded wait
// fails this test in a few seconds rather than hanging forever.
func TestWaitForTurn_ConnectionNeverReturnsFallsBackWithinBoundedWait(t *testing.T) {
	fake := newFakeOpenCodeServer(t)
	a := newLivenessAdapter(t, fake)

	connectCtx, connectCancel := context.WithTimeout(context.Background(), testWait)
	defer connectCancel()
	if err := a.Connected(connectCtx); err != nil {
		t.Fatalf("Connected() error = %v", err)
	}
	<-fake.connected

	fake.setMessages([]messageListEntry{{Parts: []json.RawMessage{textPartJSON(t, "prt_1", "msg_1", "done")}}})

	collector := &eventCollector{}
	sessionID := "ses_fake"
	cmd := sandboxws.Prompt{Type: "prompt", MessageId: "m1", SessionId: "sess-1", Gen: 1, Text: "hi"}

	ctx, cancel := context.WithTimeout(context.Background(), livenessBoundedWaitCtxTimeout)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	waitForTurnRegistered(t, a, sessionID)

	// Arm the permanent outage BEFORE dropping the current connection, so
	// every reconnect attempt runEventLoop makes from here on fails
	// immediately -- the connection never comes back.
	fake.rejectAllFutureEventConnections()
	fake.dropConnection()

	start := time.Now()
	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v, want nil (a permanently dead connection must still finalize via the bounded fallback, not surface a ctx-cancellation error)", err)
	}
	elapsed := time.Since(start)

	lower := fallbackReconnectGraceMultiplier * livenessSSEInactivityTimeout * 3 / 4
	upper := livenessBoundedWaitCtxTimeout / 2
	if elapsed < lower || elapsed > upper {
		t.Errorf("StartTurn() took %v to finalize a permanently dropped connection, want roughly %dx sseInactivityTimeout "+
			"(between %v and %v) -- genuinely bounded, not near-instant and not near the %v outer ctx safety net",
			elapsed, fallbackReconnectGraceMultiplier, lower, upper, livenessBoundedWaitCtxTimeout)
	}

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeCompleted {
		t.Errorf("execution_complete.Outcome = %q, want %q (via the fallback's own final-message fetch, once the bounded wait elapsed)",
			final.Outcome, sandboxws.ExecutionCompleteOutcomeCompleted)
	}
}
