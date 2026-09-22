package opencode

import (
	"context"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// TestFinalize_SecondCallIsNoOp proves tryFinalize's own double-finalize
// guard (turn.go), which Finding 4's emit/emitFinal split depends on being
// genuinely a one-shot gate: a second a.finalize call on an already-
// finalized turnState must not emit anything further (no duplicate
// execution_complete), matching finalize's own doc comment ("a stray
// duplicate session.idle... can never double-emit").
func TestFinalize_SecondCallIsNoOp(t *testing.T) {
	a := newDispatchTestAdapter(t)
	sink, events := spyEventSink(t)

	cmd := sandboxws.Prompt{SessionId: "sess-1", Gen: 1}
	ts := newTurnState(cmd, sink)

	reason1 := "first"
	a.finalize(ts, turnOutcome{Outcome: sandboxws.ExecutionCompleteOutcomeCompleted, Reason: &reason1})

	reason2 := "second -- must never reach the sink"
	a.finalize(ts, turnOutcome{Outcome: sandboxws.ExecutionCompleteOutcomeFailed, Reason: &reason2})

	got := events()
	if len(got) != 1 {
		t.Fatalf("events = %+v, want exactly 1 (the FIRST finalize call's own execution_complete only)", got)
	}
	final, ok := got[0].Payload.(sandboxws.ExecutionComplete)
	if !ok {
		t.Fatalf("events[0].Payload = %T, want sandboxws.ExecutionComplete", got[0].Payload)
	}
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeCompleted {
		t.Errorf("execution_complete.Outcome = %q, want %q (the FIRST call's own outcome, unchanged by the second)",
			final.Outcome, sandboxws.ExecutionCompleteOutcomeCompleted)
	}
}

// TestFinalize_DrainsOpenSubtasksWithSubTaskFinish proves finalize's own
// "close out every still-open sub-task before the turn's own
// execution_complete" contract (§7.1) for the ordinary, non-racing case: a
// subtask started via dispatchSubtaskStart and never explicitly finished is
// drained into a matching sub_task_finish (outcome mapped via
// subTaskOutcome), emitted BEFORE execution_complete.
func TestFinalize_DrainsOpenSubtasksWithSubTaskFinish(t *testing.T) {
	a := newDispatchTestAdapter(t)
	sink, events := spyEventSink(t)

	cmd := sandboxws.Prompt{SessionId: "sess-1", Gen: 1}
	ts := newTurnState(cmd, sink)
	a.registerTurn("ses_test", ts)

	a.dispatchSubtaskStart(ts, subtaskPart{ID: "sub-1", MessageID: "msg-1", Description: "an open subtask"})

	reason := "turn failed"
	a.finalize(ts, turnOutcome{Outcome: sandboxws.ExecutionCompleteOutcomeFailed, Reason: &reason})

	got := events()
	// [0] = sub_task_start (from dispatchSubtaskStart above), [1] =
	// sub_task_finish (drained by finalize), [2] = execution_complete.
	if len(got) != 3 {
		t.Fatalf("events = %+v, want exactly 3 (sub_task_start, sub_task_finish, execution_complete)", got)
	}

	finish, ok := got[1].Payload.(sandboxws.SubTaskFinish)
	if !ok {
		t.Fatalf("events[1].Payload = %T, want sandboxws.SubTaskFinish", got[1].Payload)
	}
	if finish.SubTaskId != "sub-1" {
		t.Errorf("SubTaskFinish.SubTaskId = %q, want %q", finish.SubTaskId, "sub-1")
	}
	if finish.Outcome != sandboxws.SubTaskFinishOutcomeFailed {
		t.Errorf("SubTaskFinish.Outcome = %q, want %q (mapped from the enclosing turn's own Failed outcome)",
			finish.Outcome, sandboxws.SubTaskFinishOutcomeFailed)
	}

	if _, ok := got[2].Payload.(sandboxws.ExecutionComplete); !ok {
		t.Errorf("events[2].Payload = %T, want sandboxws.ExecutionComplete (sub_task_finish must precede it)", got[2].Payload)
	}
}

// TestFinalizeByFallback_FetchFailureAlsoFinalizesAsFailed proves
// finalizeByFallback's own "fetch failed too" branch: when the fallback's
// own GET /session/{id}/message call itself fails (a real 500 here), the
// turn must still finalize (Failed, with a distinct reason naming the
// double failure) rather than hang -- exactly the scenario Finding 3's own
// per-request timeout exists to eventually unblock if this call hangs
// instead of failing outright.
func TestFinalizeByFallback_FetchFailureAlsoFinalizesAsFailed(t *testing.T) {
	a := newDispatchTestAdapter(t)
	sink, events := spyEventSink(t)

	cmd := sandboxws.Prompt{SessionId: "sess-1", Gen: 1}
	ts := newTurnState(cmd, sink)

	// newDispatchTestAdapter points at an unreachable address
	// (http://127.0.0.1:1), so fetchFinalMessages fails outright here --
	// exercising the SAME branch a real 500 response would.
	a.finalizeByFallback(context.Background(), "ses_test", ts)

	got := events()
	if len(got) != 1 {
		t.Fatalf("events = %+v, want exactly 1 (execution_complete)", got)
	}
	final, ok := got[0].Payload.(sandboxws.ExecutionComplete)
	if !ok {
		t.Fatalf("events[0].Payload = %T, want sandboxws.ExecutionComplete", got[0].Payload)
	}
	if final.Outcome != sandboxws.ExecutionCompleteOutcomeFailed {
		t.Errorf("execution_complete.Outcome = %q, want %q", final.Outcome, sandboxws.ExecutionCompleteOutcomeFailed)
	}
	if final.Reason == nil || *final.Reason == "" {
		t.Error("execution_complete.Reason is nil/empty, want a reason naming the double failure")
	}
}
