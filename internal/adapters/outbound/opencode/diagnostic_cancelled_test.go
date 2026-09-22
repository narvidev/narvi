package opencode

import (
	"context"
	"testing"

	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// This file pins §7.3's own invariant the wire schema states directly
// (contracts/sandbox-ws/v1/events.schema.json's own "diagnostic"
// description): a ProviderFailureDiagnostic is "absent for every other
// outcome" than "failed". Audit fix (C3): sse.go's own dispatchEvent
// "session.idle" case, and finalizeByFallback (adapter.go), each used to
// build and attach a Diagnostic whenever ANY tagged error was observed --
// including a "MessageAbortedError", which deriveOutcome (outcome.go)
// maps to Outcome: Cancelled, not Failed. A plain user-initiated Stop is
// not a provider failure, so a Cancelled execution_complete carrying a
// Diagnostic violated the schema's own sentence. Both tests below reach
// the REAL production call site (not deriveOutcome directly, which never
// touches Diagnostic at all -- see turnOutcome.Diagnostic's own doc
// comment, outcome.go).

// abortedMessageUpdated mirrors apiErrorMessageUpdated's own shape
// (transientretry_test.go) and overflowMessageUpdated's own shape
// (compactionretry_test.go) for this package's third real tagged-union
// member: "MessageAbortedError", VERIFIED LIVE (openCodeTaggedError's own
// doc comment, types.go) to carry {"data":{"message":"Aborted"}}.
func abortedMessageUpdated(t *testing.T, sessionID, messageID string) string {
	t.Helper()
	return sseLine(t, "message.updated", messageUpdatedProps{
		SessionID: sessionID,
		Info: openCodeMessageInfo{
			ID:   messageID,
			Role: "assistant",
			Error: &openCodeTaggedError{
				Name: "MessageAbortedError",
				Data: &openCodeErrorData{Message: "Aborted"},
			},
		},
	})
}

// TestDispatchEvent_AbortedTurnCarriesNoProviderFailureDiagnostic covers
// the LIVE path: dispatchEvent's own "session.idle" case (sse.go), which
// builds ProviderFailureDiagnostic at `outcome.Diagnostic =
// a.buildProviderFailureDiagnostic(err, ts)`. Deleting that gate (see this
// test's own mutation-verification in the PR body) reintroduces a
// Diagnostic on a Cancelled outcome; this test catches it.
func TestDispatchEvent_AbortedTurnCarriesNoProviderFailureDiagnostic(t *testing.T) {
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
		Type: "prompt", MessageId: "m1", SessionId: "sess-aborted-1", Gen: 1,
		Text: "a turn a user Stops mid-flight",
	}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	waitForTurnRegistered(t, a, "ses_fake")

	f.broadcast(abortedMessageUpdated(t, "ses_fake", "msg_original"))
	f.broadcast(sessionIdleLine(t, "ses_fake"))

	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeCancelled {
		t.Fatalf("execution_complete.Outcome = %q, want %q", final.Outcome, sandboxws.ExecutionCompleteOutcomeCancelled)
	}
	if final.Diagnostic != nil {
		t.Errorf("execution_complete.Diagnostic = %+v, want nil -- a cancellation is not a provider "+
			"failure, and the wire schema's own description says \"absent for every other outcome\"",
			final.Diagnostic)
	}
}

// TestFinalizeByFallback_AbortedTurnCarriesNoProviderFailureDiagnostic
// mirrors the test above for finalizeByFallback's own SEPARATE call site
// (adapter.go): the SSE-inactivity fallback's own final-message fetch can
// observe a MessageAbortedError too (a Stop that landed before
// session.idle ever arrived on this now-inactive stream), and that call
// site had the identical unconditional-assignment bug independently.
func TestFinalizeByFallback_AbortedTurnCarriesNoProviderFailureDiagnostic(t *testing.T) {
	fake := newFakeOpenCodeServer(t)
	a := newLivenessAdapter(t, fake)

	connectCtx, connectCancel := context.WithTimeout(context.Background(), testWait)
	defer connectCancel()
	if err := a.Connected(connectCtx); err != nil {
		t.Fatalf("Connected() error = %v", err)
	}
	<-fake.connected

	fake.setMessages([]messageListEntry{{
		Info: openCodeMessageInfo{
			ID:   "msg_1",
			Role: "assistant",
			Error: &openCodeTaggedError{
				Name: "MessageAbortedError",
				Data: &openCodeErrorData{Message: "Aborted"},
			},
		},
	}})

	collector := &eventCollector{}
	cmd := sandboxws.Prompt{Type: "prompt", MessageId: "m1", SessionId: "sess-1", Gen: 1, Text: "hi"}

	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()

	// No heartbeats this time: the connection is simply left to go idle so
	// the fallback's own fetch fires -- mirrors
	// TestWaitForTurn_ConnectionNeverReturnsFallsBackWithinBoundedWait's
	// own "let the timeout do the work" shape rather than
	// GenuinelyStuckTurn's own heartbeat-kept-alive variant, since this
	// test does not care which of the two fallback triggers fires, only
	// that finalizeByFallback's own call site is reached.
	if _, err := a.StartTurn(ctx, cmd, collector.sink, nil); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}

	final := lastExecutionComplete(t, collector.snapshot())
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeCancelled {
		t.Fatalf("execution_complete.Outcome = %q, want %q", final.Outcome, sandboxws.ExecutionCompleteOutcomeCancelled)
	}
	if final.Diagnostic != nil {
		t.Errorf("execution_complete.Diagnostic = %+v, want nil -- a cancellation is not a provider "+
			"failure, and the wire schema's own description says \"absent for every other outcome\"",
			final.Diagnostic)
	}
}
