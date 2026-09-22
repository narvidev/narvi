package opencode

import (
	"sync"
	"testing"

	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/app/ports"
)

// TestFinalize_LateSubtaskEmitIsDroppedNotRacedPastExecutionComplete is
// Finding 4's own deliberately-constructed race test: a turnState.emit call
// for a BRAND-NEW subtask id (the shape dispatchSubtaskStart below
// produces, mirroring a real SSE-dispatch goroutine still delivering a late
// event) is forced to run CONCURRENTLY with Adapter.finalize's own
// tryFinalize+drain+emit sequence, using a synchronization point (not
// sleeps or timing luck) to guarantee the interleaving: the racing
// dispatchSubtaskStart call is released to run precisely while finalize's
// own execution_complete emission is still in flight (i.e. strictly AFTER
// tryFinalize has already set ts.finalized=true under ts.mu, but BEFORE
// finalize itself returns) — exactly the window Finding 4 describes as
// unsafe under the OLD, unchecked emit.
//
// Run this test (like the whole package) under `go test -race`: the
// property under test is that BOTH goroutines' concurrent access to
// turnState's shared fields is correctly synchronized via ts.mu (no race
// detector warning) AND that the late event is correctly dropped rather
// than reaching the sink out of order relative to execution_complete.
func TestFinalize_LateSubtaskEmitIsDroppedNotRacedPastExecutionComplete(t *testing.T) {
	a := newDispatchTestAdapter(t)

	var mu sync.Mutex
	var events []ports.AgentEvent

	// releaseFinalize/racerStarted are the deliberate synchronization
	// point: the FIRST sink call ever made is finalize's own
	// execution_complete emission (no subtask is opened before finalize
	// runs, below, so drainOpenSubtasks has nothing to emit first) —
	// exactly the moment ts.finalized is already true (tryFinalize runs
	// strictly before any emitFinal call) but finalize has not yet
	// returned. Pausing THERE and only THERE lets the racing
	// dispatchSubtaskStart call genuinely run concurrently with finalize's
	// own in-progress work, rather than either strictly before or after it.
	racerStarted := make(chan struct{})
	releaseFinalize := make(chan struct{})

	sink := func(e ports.AgentEvent) {
		mu.Lock()
		events = append(events, e)
		isFirst := len(events) == 1
		mu.Unlock()

		if isFirst {
			close(racerStarted)
			<-releaseFinalize
		}
	}

	cmd := sandboxws.Prompt{SessionId: "sess-race", Gen: 1}
	ts := newTurnState(cmd, sink)
	a.registerTurn("ses_race", ts)

	var group errgroup.Group
	group.Go(func() error {
		<-racerStarted
		// The "late" concurrent SSE-dispatch call for a BRAND-NEW subtask
		// id, racing finalize's own in-progress emission sequence — the
		// exact scenario Finding 4 describes (dispatchSubtaskStart calling
		// markSubtaskStarted+emit concurrently with Adapter.finalize).
		a.dispatchSubtaskStart(ts, subtaskPart{ID: "sub-late", MessageID: "msg-late", Description: "late subtask"})
		close(releaseFinalize)
		return nil
	})

	reason := "test outcome"
	a.finalize(ts, turnOutcome{Outcome: sandboxws.ExecutionCompleteOutcomeCompleted, Reason: &reason})

	if err := group.Wait(); err != nil {
		t.Fatalf("group.Wait() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	for _, e := range events {
		if st, ok := e.Payload.(sandboxws.SubTaskStart); ok {
			t.Errorf("sub_task_start for %q reached the sink; a late event emitted after finalize must be dropped", st.SubTaskId)
		}
	}
	if len(events) == 0 {
		t.Fatal("no events reached the sink at all, want at least execution_complete")
	}
	if _, ok := events[len(events)-1].Payload.(sandboxws.ExecutionComplete); !ok {
		t.Errorf("last event = %T, want execution_complete to be the final event", events[len(events)-1].Payload)
	}

	// The late subtask's own markSubtaskStarted call DID still land (it has
	// no finalized check of its own -- only emit does) -- ts.subtasksOpen
	// permanently records it as open with no matching sub_task_finish ever
	// emitted. This is Finding 4's own documented, accepted trade-off (a
	// pre-existing §7.1 best-effort limitation, not something this fix
	// claims to solve): the property this test actually proves is emit
	// ORDERING safety, not a guaranteed sub_task_finish for every possible
	// race.
	if !ts.subtaskAlreadyStarted("sub-late") {
		t.Error("expected markSubtaskStarted's own side effect to have landed regardless (sanity check on the race construction)")
	}
}
