package opencode

import (
	"fmt"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// These tests prove §26.7/§7.1's own cost accumulator (§26.5): every
// step-finish this turn observes -- main lane AND every sub-task alike --
// must be summed into ONE running total, and that total must be reachable
// from Adapter.CurrentTurnSpentUSD, the method cmd/sandbox-agent's own
// loopback review-cost-budget HTTP server calls. Uses dispatchEvent
// directly, matching dispatch_test.go/task_subtask_test.go's own
// established "fast and deterministic" style.

const (
	testCostMainSessionID  = "ses_cost_main"
	testCostChildSessionID = "ses_cost_child"
)

// stepFinishPartJSON builds a "type":"step-finish" message.part.updated
// payload carrying cost -- mirrors stepFinishPart's own VERIFIED-live
// shape (types.go).
func stepFinishPartJSON(sessionID, partID, messageID string, cost float64) []byte {
	return []byte(fmt.Sprintf(
		`{"sessionID":%q,"part":{"id":%q,"messageID":%q,"type":"step-finish","tokens":{"input":10,"output":20,"cache":{"read":0}},"cost":%v}}`,
		sessionID, partID, messageID, cost,
	))
}

// TestDispatchPart_StepFinishAccumulatesCostOnMainLane proves the
// baseline: a single main-lane step-finish adds its own p.Cost to
// turnState.spentUSD, readable via spentUSDTotal.
func TestDispatchPart_StepFinishAccumulatesCostOnMainLane(t *testing.T) {
	a := newDispatchTestAdapter(t)
	sink, _ := spyEventSink(t)

	cmd := sandboxws.Prompt{SessionId: "sess-cost-1", Gen: 1}
	ts := newTurnState(cmd, sink)
	a.registerTurn(testCostMainSessionID, ts)

	a.dispatchEvent(sseEnvelope{
		Type:       "message.part.updated",
		Properties: stepFinishPartJSON(testCostMainSessionID, "prt_sf1", "msg_main_asst", 1.25),
	})

	if got, want := ts.spentUSDTotal(), 1.25; got != want {
		t.Errorf("spentUSDTotal() = %v, want %v", got, want)
	}
}

// TestDispatchPart_StepFinishAccumulatesCostAcrossMainLaneAndSubtask is
// this Step's own central mutation-test pin: §7.1's fan-out routes a
// sub-task's own events back to the SAME enclosing turnState (tagged with
// its own subTaskId, resolveEvent/registerSubtaskSession, adapter.go), so
// a step-finish arriving on a sub-task's own distinct child OpenCode
// session must add to the IDENTICAL running total a main-lane step-finish
// does -- proving "main lane and every sub-task alike" (§7.1/§26.7's own
// corrected text) is genuinely true of the accumulator, not just of the
// main lane alone.
func TestDispatchPart_StepFinishAccumulatesCostAcrossMainLaneAndSubtask(t *testing.T) {
	a := newDispatchTestAdapter(t)
	sink, _ := spyEventSink(t)

	cmd := sandboxws.Prompt{SessionId: "sess-cost-2", Gen: 1}
	ts := newTurnState(cmd, sink)
	a.registerTurn(testCostMainSessionID, ts)
	a.registerSubtaskSession(testCostChildSessionID, ts, "subtask-1")

	// Main lane: $1.25.
	a.dispatchEvent(sseEnvelope{
		Type:       "message.part.updated",
		Properties: stepFinishPartJSON(testCostMainSessionID, "prt_sf1", "msg_main_asst", 1.25),
	})
	// Sub-task lane (a DIFFERENT OpenCode session id, tagged "subtask-1"):
	// $2.75.
	a.dispatchEvent(sseEnvelope{
		Type:       "message.part.updated",
		Properties: stepFinishPartJSON(testCostChildSessionID, "prt_sf2", "msg_child_asst", 2.75),
	})
	// A SECOND sub-task step-finish, same sub-task session, proving
	// repeated step-finishes within one sub-task keep accumulating rather
	// than overwriting: $0.50 more.
	a.dispatchEvent(sseEnvelope{
		Type:       "message.part.updated",
		Properties: stepFinishPartJSON(testCostChildSessionID, "prt_sf3", "msg_child_asst", 0.50),
	})

	const want = 1.25 + 2.75 + 0.50
	if got := ts.spentUSDTotal(); got != want {
		t.Errorf("spentUSDTotal() = %v, want %v (main lane + every sub-task step-finish summed)", got, want)
	}
}

// TestAdapter_CurrentTurnSpentUSD_NoLiveTurn proves the "nothing spent
// yet" degraded default: no turn currently registered at all (no
// setCurrentSession call ever happened) returns (0, false), never an
// error/panic -- mirrors Stop's own identical "no live session" no-op
// precedent (adapter.go).
func TestAdapter_CurrentTurnSpentUSD_NoLiveTurn(t *testing.T) {
	a := newDispatchTestAdapter(t)

	spent, ok := a.CurrentTurnSpentUSD()
	if ok {
		t.Errorf("CurrentTurnSpentUSD() ok = true, want false (no live turn registered)")
	}
	if spent != 0 {
		t.Errorf("CurrentTurnSpentUSD() spentUSD = %v, want 0", spent)
	}
}

// TestAdapter_CurrentTurnSpentUSD_ReadsTheLiveTurnsAccumulator proves the
// real, production call path: setCurrentSession + registerTurn (StartTurn's
// own real sequence, adapter.go) makes CurrentTurnSpentUSD resolve to the
// SAME running total dispatchPart's own step-finish handling accumulated,
// across main lane and sub-task alike -- this is the exact value
// cmd/sandbox-agent's loopback HTTP server hands to
// reviewtriage.ShouldSkipOptionalPass.
func TestAdapter_CurrentTurnSpentUSD_ReadsTheLiveTurnsAccumulator(t *testing.T) {
	a := newDispatchTestAdapter(t)
	sink, _ := spyEventSink(t)

	cmd := sandboxws.Prompt{SessionId: "sess-cost-3", Gen: 1}
	ts := newTurnState(cmd, sink)
	a.setCurrentSession(testCostMainSessionID)
	a.registerTurn(testCostMainSessionID, ts)
	a.registerSubtaskSession(testCostChildSessionID, ts, "subtask-1")

	a.dispatchEvent(sseEnvelope{
		Type:       "message.part.updated",
		Properties: stepFinishPartJSON(testCostMainSessionID, "prt_sf1", "msg_main_asst", 3),
	})
	a.dispatchEvent(sseEnvelope{
		Type:       "message.part.updated",
		Properties: stepFinishPartJSON(testCostChildSessionID, "prt_sf2", "msg_child_asst", 1.5),
	})

	spent, ok := a.CurrentTurnSpentUSD()
	if !ok {
		t.Fatalf("CurrentTurnSpentUSD() ok = false, want true (a live turn is registered)")
	}
	if want := 4.5; spent != want {
		t.Errorf("CurrentTurnSpentUSD() spentUSD = %v, want %v", spent, want)
	}
}
