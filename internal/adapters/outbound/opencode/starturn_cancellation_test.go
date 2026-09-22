package opencode

import (
	"context"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/app/ports"
)

// These three tests exercise Finding 5's own three real return paths that
// used to silently break StartTurn's own "always attempts exactly one
// execution_complete-shaped terminal event before returning" promise. Each
// confirms a real ExecutionComplete{Outcome: cancelled} (with a non-nil
// reason) reaches the sink BEFORE StartTurn returns.

// TestStartTurn_ResolveSessionCanceledEmitsCancelledTerminalEvent covers
// path 1: resolveSession fails AND ctx is already canceled. Using an
// unreachable address (mirroring newDispatchTestAdapter's own precedent)
// with a PRE-canceled ctx means resolveSession's own doJSON call fails
// fast (net/http's transport checks ctx before ever dialing), deterministic
// and immediate -- no real network wait needed.
func TestStartTurn_ResolveSessionCanceledEmitsCancelledTerminalEvent(t *testing.T) {
	a := New("http://127.0.0.1:1", testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before StartTurn is ever called

	sink, events := spyEventSink(t)
	cmd := sandboxws.Prompt{Type: "prompt", MessageId: "m1", SessionId: "sess-1", Gen: 1, Text: "hi"}

	convID, err := a.StartTurn(ctx, cmd, sink, nil)
	if err == nil {
		t.Fatal("StartTurn() error = nil, want ctx.Err() (canceled before resolveSession could succeed)")
	}
	if convID != "" {
		t.Errorf("StartTurn() conversationID = %q, want empty (no session was ever resolved)", convID)
	}

	assertSingleCancelledExecutionComplete(t, events())
}

// TestStartTurn_PostPromptAsyncCanceledEmitsCancelledTerminalEvent covers
// path 2: resolveSession SUCCEEDS (cmd.ConversationId is already set, so
// resolveSession reuses it directly with no HTTP call at all -- see its own
// doc comment in session.go), but postPromptAsync's own POST then fails
// because ctx is already canceled.
func TestStartTurn_PostPromptAsyncCanceledEmitsCancelledTerminalEvent(t *testing.T) {
	a := New("http://127.0.0.1:1", testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before StartTurn is ever called

	sink, events := spyEventSink(t)
	existingConversationID := "ses_already_resolved"
	cmd := sandboxws.Prompt{
		Type: "prompt", MessageId: "m1", SessionId: "sess-1", Gen: 1, Text: "hi",
		ConversationId: &existingConversationID,
	}

	convID, err := a.StartTurn(ctx, cmd, sink, nil)
	if err == nil {
		t.Fatal("StartTurn() error = nil, want ctx.Err() (canceled before postPromptAsync could succeed)")
	}
	if convID != existingConversationID {
		t.Errorf("StartTurn() conversationID = %q, want %q (resolveSession reused it directly, before the cancellation was ever observed)",
			convID, existingConversationID)
	}

	assertSingleCancelledExecutionComplete(t, events())
}

// TestStartTurn_WaitForTurnCtxDoneEmitsCancelledTerminalEvent covers path
// 3: resolveSession and postPromptAsync BOTH succeed, but ctx is canceled
// while waitForTurn is still blocked waiting for
// session.idle/session.error/the fallback. Deliberately uses the fake
// server (fake_server_test.go), NOT a real opencode binary/AI provider:
// this test needs session.idle to NEVER arrive on its own within its own
// short window, a guarantee a real, live model response's own
// (unpredictable, possibly fast) timing cannot deterministically provide.
//
// Unlike the first two paths above, StartTurn's own return value for THIS
// path is (sessionID, nil) -- unchanged by Finding 5's fix, and correctly
// so: waitForTurn itself never returned an error even before this fix
// (only StartTurn's own two EARLIER early-return branches propagate
// ctx.Err() directly); what Finding 5 actually closes here is that
// waitForTurn now calls finalize before returning, so the promised
// execution_complete genuinely reaches the sink even on this path, which
// this test is the one that proves.
func TestStartTurn_WaitForTurnCtxDoneEmitsCancelledTerminalEvent(t *testing.T) {
	fake := newFakeOpenCodeServer(t)
	a := New(fake.URL(), testSSEInactivityTimeout, testReconnectInterval, testRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	connectCtx, connectCancel := context.WithTimeout(context.Background(), testWait)
	defer connectCancel()
	if err := a.Connected(connectCtx); err != nil {
		t.Fatalf("Connected() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	collector := &eventCollector{}
	cmd := sandboxws.Prompt{
		Type: "prompt", MessageId: "m1", SessionId: "sess-1", Gen: 1,
		Text: "irrelevant -- the fake server never sends session.idle for this turn",
	}

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, nil)
		return err
	})

	// Cancel shortly after StartTurn has had a moment to resolve its
	// session and dispatch prompt_async against the fake server (both
	// deterministically fast, real HTTP round trips) -- well before
	// testSSEInactivityTimeout (45s) could ever fire its own fallback, and
	// the fake server never sends session.idle/session.error at all.
	time.Sleep(200 * time.Millisecond)
	cancel()

	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v, want nil (waitForTurn's own ctx-cancellation path finalizes but does not itself surface an error)", err)
	}
	if ctx.Err() == nil {
		t.Fatal("ctx.Err() = nil after cancel(), test setup is broken")
	}

	assertSingleCancelledExecutionComplete(t, collector.snapshot())
}

// assertSingleCancelledExecutionComplete asserts events contains EXACTLY
// one ExecutionComplete, that it is the LAST event, that its outcome is
// Cancelled, and that it carries a non-nil, non-empty reason (Finding 5's
// own "clear, honest reason string" requirement).
func assertSingleCancelledExecutionComplete(t *testing.T, events []ports.AgentEvent) {
	t.Helper()

	var finals []sandboxws.ExecutionComplete
	for _, e := range events {
		if ec, ok := e.Payload.(sandboxws.ExecutionComplete); ok {
			finals = append(finals, ec)
		}
	}

	if len(finals) != 1 {
		t.Fatalf("observed %d execution_complete events, want exactly 1 (events: %+v)", len(finals), events)
	}
	if len(events) == 0 {
		t.Fatal("no events observed at all")
	}
	if _, ok := events[len(events)-1].Payload.(sandboxws.ExecutionComplete); !ok {
		t.Errorf("last event = %T, want execution_complete to be the final event", events[len(events)-1].Payload)
	}

	final := finals[0]
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeCancelled {
		t.Errorf("execution_complete.Outcome = %q, want %q", final.Outcome, sandboxws.ExecutionCompleteOutcomeCancelled)
	}
	if final.Reason == nil || *final.Reason == "" {
		t.Error("execution_complete.Reason is nil/empty, want a clear, honest reason string")
	}
}
