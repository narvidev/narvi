package opencode

import (
	"context"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// TestStartTurn_OnConversationIDReporter_FiresBeforeTurnCompletes is
// proof of §3.3's ("turn recovery") own "the turn records the
// OpenCode conversation id at turn start... never lazily" requirement:
// onConversationID must fire the MOMENT resolveSession resolves a real
// id, not merely "eventually, once the whole (possibly minutes-long)
// StartTurn call returns". Uses the fake server (fake_server_test.go),
// exactly like TestStartTurn_WaitForTurnCtxDoneEmitsCancelledTerminalEvent
// (starturn_cancellation_test.go) — deliberately never a real OpenCode
// binary/AI provider: this test needs session.idle to NEVER arrive on its
// own within its own short window, a guarantee only a fully-controlled
// fake can deterministically provide. StartTurn is still genuinely
// blocked inside waitForTurn at the moment this test observes the
// callback having already fired — proving the timing claim directly,
// not by inference.
func TestStartTurn_OnConversationIDReporter_FiresBeforeTurnCompletes(t *testing.T) {
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

	var mu sync.Mutex
	var reportedID string
	var reportedCount int
	onConversationID := func(id string) {
		mu.Lock()
		defer mu.Unlock()
		reportedCount++
		reportedID = id
	}

	var group errgroup.Group
	group.Go(func() error {
		_, err := a.StartTurn(ctx, cmd, collector.sink, onConversationID)
		return err
	})

	// Poll until onConversationID has fired -- StartTurn itself is still
	// blocked in waitForTurn the entire time this loop runs (the fake
	// server never sends session.idle/session.error for this turn at
	// all), so observing this BEFORE ever canceling ctx below is the
	// direct proof the callback fires at turn START, not at turn END.
	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := reportedCount
		mu.Unlock()
		if count > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	mu.Lock()
	gotID, gotCount := reportedID, reportedCount
	mu.Unlock()

	if gotCount != 1 {
		t.Fatalf("onConversationID called %d times before StartTurn returned, want exactly 1 (never observed it fire at all within %s)", gotCount, testWait)
	}
	if gotID == "" {
		t.Error("onConversationID reported an empty conversation id")
	}

	// A brief additional buffer -- mirroring TestStartTurn_
	// WaitForTurnCtxDoneEmitsCancelledTerminalEvent's own identical
	// 200ms wait -- so postPromptAsync's own real (if fast, local) HTTP
	// round trip against the fake server has definitely completed and
	// StartTurn is genuinely blocked inside waitForTurn by the time ctx
	// is canceled below, rather than racing postPromptAsync's own
	// separate ctx-cancellation early-return path (which legitimately
	// returns a non-nil ctx.Err(), a DIFFERENT path this test is not
	// about -- see starturn_cancellation_test.go's own path 2).
	time.Sleep(200 * time.Millisecond)

	// Now let StartTurn's own waitForTurn ctx-cancellation path finalize
	// and return -- proving the callback fired well BEFORE this, not as a
	// side effect of it.
	cancel()
	if err := group.Wait(); err != nil {
		t.Fatalf("StartTurn() error = %v, want nil (waitForTurn's own ctx-cancellation path finalizes but does not itself surface an error)", err)
	}
}
