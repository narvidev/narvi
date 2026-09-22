package opencode

import (
	"encoding/json"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// These tests reproduce, via dispatchEvent (exactly matching dispatch_test.
// go's own established "fast and deterministic" style), the REAL, live-
// triggered SSE shape a later Step's own investigation captured for
// OpenCode's own "task" tool: a real `opencode serve` process, a real
// scripted prompt explicitly instructing the model to delegate via the
// task tool, and the resulting SSE trace inspected directly. The
// synthetic payloads below mirror that real trace's own shapes exactly
// (field names, nesting, the fact that state.metadata is present at BOTH
// "running" and "completed") — only the session/message/call ids are
// swapped for readable test fixtures. See Adapter's own package doc
// comment (adapter.go) for the full writeup of the correlator this
// implements.

const (
	testTaskMainSessionID  = "ses_main1"
	testTaskChildSessionID = "ses_child1"
)

// taskRunningPartJSON/taskCompletedPartJSON build the "tool" part payload
// (tool=="task") for message.part.updated, matching the real live-captured
// shape.
func taskRunningPartJSON(callID string) []byte {
	return []byte(`{"sessionID":"` + testTaskMainSessionID + `","part":{
		"id":"prt_task1","messageID":"msg_main_asst","sessionID":"` + testTaskMainSessionID + `",
		"type":"tool","tool":"task","callID":"` + callID + `",
		"state":{"status":"running","input":{"description":"Compute 17*23","prompt":"Compute 17 * 23.","subagent_type":"general"},
			"metadata":{"parentSessionId":"` + testTaskMainSessionID + `","sessionId":"` + testTaskChildSessionID + `","model":{"modelID":"big-pickle","providerID":"opencode"}}}
	}}`)
}

func taskCompletedPartJSON(callID, status string) []byte {
	return []byte(`{"sessionID":"` + testTaskMainSessionID + `","part":{
		"id":"prt_task1","messageID":"msg_main_asst","sessionID":"` + testTaskMainSessionID + `",
		"type":"tool","tool":"task","callID":"` + callID + `",
		"state":{"status":"` + status + `","input":{"description":"Compute 17*23","prompt":"Compute 17 * 23.","subagent_type":"general"},
			"output":"<task id=\"` + testTaskChildSessionID + `\" state=\"` + status + `\">\n<task_result>\n391\n</task_result>\n</task>",
			"metadata":{"parentSessionId":"` + testTaskMainSessionID + `","sessionId":"` + testTaskChildSessionID + `","model":{"modelID":"big-pickle","providerID":"opencode"}}}
	}}`)
}

// TestDispatchTool_TaskSubtask_StartsAndTagsNestedEvents proves the full,
// real §7.1 sub-task lifecycle end to end: sub_task_start fires off the
// task tool call's own "running" state (metadata-derived subTaskId), the
// task tool_call/tool_result themselves stay MAIN-lane (SubTaskId nil),
// the sub-agent's own inner events (arriving tagged with its own DISTINCT
// child sessionID) get tagged with that same subTaskId, the child's own
// session.idle does NOT finalize the enclosing turn, and the task tool
// call's own "completed" transition emits a precise sub_task_finish and
// unregisters the child session's own routing.
func TestDispatchTool_TaskSubtask_StartsAndTagsNestedEvents(t *testing.T) {
	a := newDispatchTestAdapter(t)
	sink, events := spyEventSink(t)

	cmd := sandboxws.Prompt{SessionId: testSessionID, Gen: 1}
	ts := newTurnState(cmd, sink)
	a.registerTurn(testTaskMainSessionID, ts)

	// The MAIN lane's own assistant message -- required before
	// dispatchPart's own isAssistantMessage gate accepts any text part.
	a.dispatchEvent(sseEnvelope{
		Type:       "message.updated",
		Properties: []byte(`{"sessionID":"` + testTaskMainSessionID + `","info":{"id":"msg_main_asst","role":"assistant"}}`),
	})

	// pending -> running: the task tool call's own "running" state carries
	// state.metadata (VERIFIED LIVE shape).
	a.dispatchEvent(sseEnvelope{Type: "message.part.updated", Properties: taskRunningPartJSON("call_task1")})

	got := events()
	if len(got) != 2 {
		t.Fatalf("events = %+v, want exactly 2 (sub_task_start, tool_call)", got)
	}

	start, ok := got[0].Payload.(sandboxws.SubTaskStart)
	if !ok {
		t.Fatalf("events[0].Payload = %T, want sandboxws.SubTaskStart", got[0].Payload)
	}
	if start.SubTaskId != testTaskChildSessionID {
		t.Errorf("SubTaskStart.SubTaskId = %q, want %q (the task tool's own spawned child session id)", start.SubTaskId, testTaskChildSessionID)
	}
	if start.Label != "Compute 17*23" {
		t.Errorf("SubTaskStart.Label = %q, want %q", start.Label, "Compute 17*23")
	}
	if start.ParentMessageId != "msg_main_asst" {
		t.Errorf("SubTaskStart.ParentMessageId = %q, want %q", start.ParentMessageId, "msg_main_asst")
	}

	call, ok := got[1].Payload.(sandboxws.ToolCall)
	if !ok {
		t.Fatalf("events[1].Payload = %T, want sandboxws.ToolCall", got[1].Payload)
	}
	if call.SubTaskId != nil {
		t.Errorf("task tool_call.SubTaskId = %v, want nil -- the task tool_call ITSELF is a MAIN-lane event", call.SubTaskId)
	}

	// The sub-agent's own inner activity arrives tagged with the CHILD's
	// own distinct top-level sessionID -- exactly the real, live-verified
	// shape.
	a.dispatchEvent(sseEnvelope{
		Type:       "message.updated",
		Properties: []byte(`{"sessionID":"` + testTaskChildSessionID + `","info":{"id":"msg_child_asst","role":"assistant"}}`),
	})
	a.dispatchEvent(sseEnvelope{
		Type:       "message.part.updated",
		Properties: []byte(`{"sessionID":"` + testTaskChildSessionID + `","part":{"id":"prt_child_step1","messageID":"msg_child_asst","sessionID":"` + testTaskChildSessionID + `","type":"step-start"}}`),
	})
	a.dispatchEvent(sseEnvelope{
		Type:       "message.part.updated",
		Properties: []byte(`{"sessionID":"` + testTaskChildSessionID + `","part":{"id":"prt_child_text1","messageID":"msg_child_asst","sessionID":"` + testTaskChildSessionID + `","type":"text","text":"391"}}`),
	})
	a.dispatchEvent(sseEnvelope{
		Type:       "message.part.updated",
		Properties: []byte(`{"sessionID":"` + testTaskChildSessionID + `","part":{"id":"prt_child_finish1","messageID":"msg_child_asst","sessionID":"` + testTaskChildSessionID + `","type":"step-finish","reason":"stop","tokens":{"total":10,"input":8,"output":2,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0}}`),
	})

	// The child session's own session.idle must NOT finalize the
	// ENCLOSING turn.
	a.dispatchEvent(sseEnvelope{
		Type:       "session.idle",
		Properties: []byte(`{"sessionID":"` + testTaskChildSessionID + `"}`),
	})

	got = events()
	if len(got) != 5 {
		t.Fatalf("events = %+v, want exactly 5 (sub_task_start, tool_call, step_start, token, step_finish) -- the child's own session.idle must not add an execution_complete", got)
	}

	stepStart, ok := got[2].Payload.(sandboxws.StepStart)
	if !ok {
		t.Fatalf("events[2].Payload = %T, want sandboxws.StepStart", got[2].Payload)
	}
	if stepStart.SubTaskId == nil || *stepStart.SubTaskId != testTaskChildSessionID {
		t.Errorf("step_start.SubTaskId = %v, want %q", stepStart.SubTaskId, testTaskChildSessionID)
	}

	token, ok := got[3].Payload.(sandboxws.Token)
	if !ok {
		t.Fatalf("events[3].Payload = %T, want sandboxws.Token", got[3].Payload)
	}
	if token.SubTaskId == nil || *token.SubTaskId != testTaskChildSessionID {
		t.Errorf("token.SubTaskId = %v, want %q", token.SubTaskId, testTaskChildSessionID)
	}
	if token.Text != "391" {
		t.Errorf("token.Text = %q, want %q", token.Text, "391")
	}

	stepFinish, ok := got[4].Payload.(sandboxws.StepFinish)
	if !ok {
		t.Fatalf("events[4].Payload = %T, want sandboxws.StepFinish", got[4].Payload)
	}
	if stepFinish.SubTaskId == nil || *stepFinish.SubTaskId != testTaskChildSessionID {
		t.Errorf("step_finish.SubTaskId = %v, want %q", stepFinish.SubTaskId, testTaskChildSessionID)
	}

	// The task tool call itself completes (delivered on the MAIN lane,
	// VERIFIED LIVE still carrying the same state.metadata) -- must emit
	// sub_task_finish{completed} and the task's own tool_result (MAIN
	// lane, SubTaskId nil).
	a.dispatchEvent(sseEnvelope{Type: "message.part.updated", Properties: taskCompletedPartJSON("call_task1", "completed")})

	got = events()
	if len(got) != 7 {
		t.Fatalf("events = %+v, want exactly 7 (+ sub_task_finish, tool_result)", got)
	}

	finish, ok := got[5].Payload.(sandboxws.SubTaskFinish)
	if !ok {
		t.Fatalf("events[5].Payload = %T, want sandboxws.SubTaskFinish", got[5].Payload)
	}
	if finish.SubTaskId != testTaskChildSessionID {
		t.Errorf("sub_task_finish.SubTaskId = %q, want %q", finish.SubTaskId, testTaskChildSessionID)
	}
	if finish.Outcome != sandboxws.SubTaskFinishOutcomeCompleted {
		t.Errorf("sub_task_finish.Outcome = %q, want completed", finish.Outcome)
	}

	result, ok := got[6].Payload.(sandboxws.ToolResult)
	if !ok {
		t.Fatalf("events[6].Payload = %T, want sandboxws.ToolResult", got[6].Payload)
	}
	if result.SubTaskId != nil {
		t.Errorf("task tool_result.SubTaskId = %v, want nil -- MAIN-lane event", result.SubTaskId)
	}

	// A later, stray event for the now-unregistered child session must be
	// silently dropped (unregisterSubtaskSession already ran).
	a.dispatchEvent(sseEnvelope{
		Type:       "message.part.updated",
		Properties: []byte(`{"sessionID":"` + testTaskChildSessionID + `","part":{"id":"prt_stray","messageID":"msg_child_asst","sessionID":"` + testTaskChildSessionID + `","type":"text","text":"stray"}}`),
	})
	if got := events(); len(got) != 7 {
		t.Fatalf("events = %+v, want still exactly 7 -- a stray event for an unregistered (already-finished) child session must be dropped", got)
	}

	// finalize must NOT re-drain/double-finish the already-closed subtask
	// (markSubtaskFinished already removed it from ts.subtasksOpen).
	a.finalize(ts, turnOutcome{Outcome: sandboxws.ExecutionCompleteOutcomeCompleted})
	finalGot := events()
	if len(finalGot) != 8 {
		t.Fatalf("events = %+v, want exactly 8 (+ execution_complete only -- no duplicate sub_task_finish)", finalGot)
	}
	if _, ok := finalGot[7].Payload.(sandboxws.ExecutionComplete); !ok {
		t.Fatalf("events[7].Payload = %T, want sandboxws.ExecutionComplete", finalGot[7].Payload)
	}
}

// TestDispatchTool_TaskSubtask_ErrorStatusFinishesAsFailed proves the
// task tool call's own "error" status maps to SubTaskFinish's own "failed"
// outcome (schema-derived: ToolStateError also carries "metadata", not
// independently observed live -- unlike the "completed" case exercised
// above, which the real live trigger this Step's own investigation ran
// did produce).
func TestDispatchTool_TaskSubtask_ErrorStatusFinishesAsFailed(t *testing.T) {
	a := newDispatchTestAdapter(t)
	sink, events := spyEventSink(t)

	cmd := sandboxws.Prompt{SessionId: testSessionID, Gen: 1}
	ts := newTurnState(cmd, sink)
	a.registerTurn(testTaskMainSessionID, ts)

	a.dispatchEvent(sseEnvelope{
		Type:       "message.updated",
		Properties: []byte(`{"sessionID":"` + testTaskMainSessionID + `","info":{"id":"msg_main_asst","role":"assistant"}}`),
	})
	a.dispatchEvent(sseEnvelope{Type: "message.part.updated", Properties: taskRunningPartJSON("call_task2")})
	a.dispatchEvent(sseEnvelope{Type: "message.part.updated", Properties: taskCompletedPartJSON("call_task2", "error")})

	got := events()
	if len(got) != 4 {
		t.Fatalf("events = %+v, want exactly 4 (sub_task_start, tool_call, sub_task_finish, tool_result)", got)
	}
	finish, ok := got[2].Payload.(sandboxws.SubTaskFinish)
	if !ok {
		t.Fatalf("events[2].Payload = %T, want sandboxws.SubTaskFinish", got[2].Payload)
	}
	if finish.Outcome != sandboxws.SubTaskFinishOutcomeFailed {
		t.Errorf("sub_task_finish.Outcome = %q, want failed", finish.Outcome)
	}
}

// TestDispatchTool_TaskSubtask_ResolvedBeforeRunningStillBracketed proves
// the "resolved so fast we never saw running" edge case: a task tool call
// that jumps straight from unregistered to "completed" still gets a
// matching sub_task_start/sub_task_finish pair (mirroring the identical
// pre-existing guarantee for ordinary tool_call/tool_result, dispatchTool's
// own "completed","error" branch).
func TestDispatchTool_TaskSubtask_ResolvedBeforeRunningStillBracketed(t *testing.T) {
	a := newDispatchTestAdapter(t)
	sink, events := spyEventSink(t)

	cmd := sandboxws.Prompt{SessionId: testSessionID, Gen: 1}
	ts := newTurnState(cmd, sink)
	a.registerTurn(testTaskMainSessionID, ts)

	a.dispatchEvent(sseEnvelope{
		Type:       "message.updated",
		Properties: []byte(`{"sessionID":"` + testTaskMainSessionID + `","info":{"id":"msg_main_asst","role":"assistant"}}`),
	})
	// No "running" delivery at all -- straight to "completed".
	a.dispatchEvent(sseEnvelope{Type: "message.part.updated", Properties: taskCompletedPartJSON("call_task3", "completed")})

	got := events()
	if len(got) != 4 {
		t.Fatalf("events = %+v, want exactly 4 (sub_task_start, tool_call, sub_task_finish, tool_result)", got)
	}
	if _, ok := got[0].Payload.(sandboxws.SubTaskStart); !ok {
		t.Fatalf("events[0].Payload = %T, want sandboxws.SubTaskStart", got[0].Payload)
	}
	if _, ok := got[2].Payload.(sandboxws.SubTaskFinish); !ok {
		t.Fatalf("events[2].Payload = %T, want sandboxws.SubTaskFinish", got[2].Payload)
	}
}

// TestFinalize_DrainsOpenTaskSubtask_WhenNeverCompleted proves the
// turn-level fallback (Adapter.finalize's own drainOpenSubtasks) still
// correctly closes out a task-tool-started sub-task whose own task tool
// call never reaches a terminal status before the ENCLOSING turn itself
// finalizes (e.g. the turn is cancelled while the task tool call is still
// "running") -- using the turn's own outcome, exactly as the legacy
// subtaskPart path already does (see TestFinalize_DrainsOpenSubtasksWith
// SubTaskFinish, finalize_test.go). Also proves the child session's own
// routing entry is unregistered so a later stray event for it is dropped.
func TestFinalize_DrainsOpenTaskSubtask_WhenNeverCompleted(t *testing.T) {
	a := newDispatchTestAdapter(t)
	sink, events := spyEventSink(t)

	cmd := sandboxws.Prompt{SessionId: testSessionID, Gen: 1}
	ts := newTurnState(cmd, sink)
	a.registerTurn(testTaskMainSessionID, ts)

	a.dispatchEvent(sseEnvelope{Type: "message.part.updated", Properties: taskRunningPartJSON("call_task4")})

	reason := "turn cancelled"
	a.finalize(ts, turnOutcome{Outcome: sandboxws.ExecutionCompleteOutcomeCancelled, Reason: &reason})

	got := events()
	// [0] = sub_task_start, [1] = tool_call, [2] = sub_task_finish (drained
	// by finalize), [3] = execution_complete.
	if len(got) != 4 {
		t.Fatalf("events = %+v, want exactly 4 (sub_task_start, tool_call, sub_task_finish, execution_complete)", got)
	}
	finish, ok := got[2].Payload.(sandboxws.SubTaskFinish)
	if !ok {
		t.Fatalf("events[2].Payload = %T, want sandboxws.SubTaskFinish", got[2].Payload)
	}
	if finish.SubTaskId != testTaskChildSessionID {
		t.Errorf("SubTaskFinish.SubTaskId = %q, want %q", finish.SubTaskId, testTaskChildSessionID)
	}
	if finish.Outcome != sandboxws.SubTaskFinishOutcomeCancelled {
		t.Errorf("SubTaskFinish.Outcome = %q, want cancelled (the ENCLOSING turn's own outcome)", finish.Outcome)
	}

	// The child session's own routing entry must have been unregistered by
	// finalize -- a later event for it is now unroutable (no registered
	// turn) and silently dropped, matching dispatchEvent's own "no
	// registered turn" contract.
	if _, _, ok := a.lookupSubtaskSession(testTaskChildSessionID); ok {
		t.Error("subtask session still registered after finalize drained it, want unregistered")
	}
}

// TestMaybeStartTaskSubtask_SkipsRegistrationWhenAlreadyFinalized proves
// the fix for a real race a review pass caught: maybeStartTaskSubtask used
// to call registerSubtaskSession unconditionally, before ts.emit's own
// finalized check ran -- so a task's own "running" event processed on the
// SSE dispatch goroutine strictly AFTER a concurrent Adapter.finalize had
// already drained this turn's own open sub-tasks (e.g. the turn was
// cancelled) would still register a routing entry on the Adapter-lifetime
// subtaskSessions map that nothing would ever remove. ts.finalized is set
// directly here (no ts.mu needed -- this test is single-goroutine, exactly
// simulating "a concurrent finalize already ran by the time this delivery
// is processed") rather than actually calling finalize, so this test
// isolates maybeStartTaskSubtask's own registration-gating fix from
// finalize's own separate, already-covered drain behavior (see
// TestFinalize_DrainsOpenTaskSubtask_WhenNeverCompleted above).
func TestMaybeStartTaskSubtask_SkipsRegistrationWhenAlreadyFinalized(t *testing.T) {
	a := newDispatchTestAdapter(t)
	sink, events := spyEventSink(t)

	cmd := sandboxws.Prompt{SessionId: testSessionID, Gen: 1}
	ts := newTurnState(cmd, sink)
	a.registerTurn(testTaskMainSessionID, ts)
	ts.finalized = true

	a.dispatchEvent(sseEnvelope{Type: "message.part.updated", Properties: taskRunningPartJSON("call_task5")})

	if got := events(); len(got) != 0 {
		t.Fatalf("events = %+v, want none (turn already finalized, emit must drop everything)", got)
	}
	if _, _, ok := a.lookupSubtaskSession(testTaskChildSessionID); ok {
		t.Error("subtask session was registered for an already-finalized turn -- this leaks a routing entry the Adapter will never clean up")
	}
}

// TestDecodeTaskMetadata covers decodeTaskMetadata's own edge cases: only
// a well-formed object with a non-empty "sessionId" is a genuine
// correlator; anything else (absent, malformed, or session-id-less
// metadata, e.g. a non-task tool's own differently-shaped metadata) must
// be reported as ok=false, never an error/panic.
func TestDecodeTaskMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  json.RawMessage
		want bool
	}{
		{"absent", nil, false},
		{"empty object", json.RawMessage(`{}`), false},
		{"well-formed", json.RawMessage(`{"parentSessionId":"ses_p","sessionId":"ses_c"}`), true},
		{"sessionId empty string", json.RawMessage(`{"parentSessionId":"ses_p","sessionId":""}`), false},
		{"malformed json", json.RawMessage(`{not json`), false},
		{"unrelated shape (non-task tool's own metadata)", json.RawMessage(`{"foo":"bar"}`), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, ok := decodeTaskMetadata(tt.raw)
			if ok != tt.want {
				t.Errorf("decodeTaskMetadata(%s) ok = %v, want %v", tt.raw, ok, tt.want)
			}
		})
	}
}

// TestTranslateSubTaskStartFromTask directly tests the translation
// function (matching translate_test.go's own established style), pinning
// the Label-fallback behavior taskInputDescription implements, and
// (§26.4/§7.1) the SubAgentType extraction taskInputSubAgentType
// implements alongside it -- the real, reliable dispatch parameter
// reviewverdict.CounterReviewCorroborated keys off, sourced from the SAME
// task-tool input object as Label, just a different key.
func TestTranslateSubTaskStartFromTask(t *testing.T) {
	t.Parallel()

	cmd := sandboxws.Prompt{SessionId: testSessionID, Gen: 4}

	t.Run("description present", func(t *testing.T) {
		t.Parallel()
		p := toolPart{
			MessageID: "msg_1",
			State:     toolPartState{Input: json.RawMessage(`{"description":"Compute 17*23"}`)},
		}
		got := translateSubTaskStartFromTask(cmd, p, "ses_child1")
		if got.Label != "Compute 17*23" {
			t.Errorf("Label = %q, want %q", got.Label, "Compute 17*23")
		}
		if got.SubTaskId != "ses_child1" || got.ParentMessageId != "msg_1" {
			t.Errorf("translateSubTaskStartFromTask() = %#v, unexpected SubTaskId/ParentMessageId", got)
		}
	})

	t.Run("description absent falls back to a generic label", func(t *testing.T) {
		t.Parallel()
		p := toolPart{MessageID: "msg_2", State: toolPartState{Input: json.RawMessage(`{}`)}}
		got := translateSubTaskStartFromTask(cmd, p, "ses_child2")
		if got.Label == "" {
			t.Error("Label is empty, want a non-empty fallback label")
		}
	})

	t.Run("subagent_type present populates SubAgentType", func(t *testing.T) {
		t.Parallel()
		p := toolPart{
			MessageID: "msg_3",
			State:     toolPartState{Input: json.RawMessage(`{"description":"Adjudicate findings","prompt":"...","subagent_type":"counter-reviewer"}`)},
		}
		got := translateSubTaskStartFromTask(cmd, p, "ses_child3")
		if got.SubAgentType == nil || *got.SubAgentType != "counter-reviewer" {
			t.Errorf("SubAgentType = %v, want a pointer to %q", got.SubAgentType, "counter-reviewer")
		}
	})

	t.Run("subagent_type absent leaves SubAgentType nil (omitted on the wire)", func(t *testing.T) {
		t.Parallel()
		p := toolPart{MessageID: "msg_4", State: toolPartState{Input: json.RawMessage(`{"description":"no subagent_type here"}`)}}
		got := translateSubTaskStartFromTask(cmd, p, "ses_child4")
		if got.SubAgentType != nil {
			t.Errorf("SubAgentType = %q, want nil when the task tool's own input carries no subagent_type field", *got.SubAgentType)
		}
	})

	t.Run("malformed input leaves SubAgentType nil, never an error", func(t *testing.T) {
		t.Parallel()
		p := toolPart{MessageID: "msg_5", State: toolPartState{Input: json.RawMessage(`{not json`)}}
		got := translateSubTaskStartFromTask(cmd, p, "ses_child5")
		if got.SubAgentType != nil {
			t.Errorf("SubAgentType = %q, want nil on malformed input", *got.SubAgentType)
		}
	})
}

// TestTranslateSubTaskStart_LegacyPathNeverPopulatesSubAgentType pins
// translateSubTaskStart's own (the legacy/unverified-live subtaskPart
// fallback, ~line 210 of translate.go) deliberate exclusion from
// §26.4's SubAgentType wiring: that path has no task-tool input to extract
// "subagent_type" from at all, so its own built sandboxws.SubTaskStart
// leaves SubAgentType at its own zero value, nil -- omitted on the wire
// exactly like every producer that predates this field.
func TestTranslateSubTaskStart_LegacyPathNeverPopulatesSubAgentType(t *testing.T) {
	t.Parallel()

	cmd := sandboxws.Prompt{SessionId: testSessionID, Gen: 4}
	got := translateSubTaskStart(cmd, subtaskPart{ID: "prt_sub1", MessageID: "msg_5", Description: "Investigate flaky test"})
	if got.SubAgentType != nil {
		t.Errorf("SubAgentType = %q, want nil on the legacy subtaskPart path, which has no task-tool input to extract it from", *got.SubAgentType)
	}
}

// TestTaskInputSubAgentType directly unit-tests the extraction helper
// (matching taskInputDescription's own established indirect-test
// precedent, but as its own direct test since this field is §26.4's
// own new addition worth pinning independently of the translation
// function that consumes it).
func TestTaskInputSubAgentType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{"present", json.RawMessage(`{"description":"x","prompt":"y","subagent_type":"counter-reviewer"}`), "counter-reviewer"},
		{"absent", json.RawMessage(`{"description":"x"}`), ""},
		{"empty object", json.RawMessage(`{}`), ""},
		{"malformed json", json.RawMessage(`{not json`), ""},
		{"wrong type (not a string)", json.RawMessage(`{"subagent_type":42}`), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := taskInputSubAgentType(tt.raw); got != tt.want {
				t.Errorf("taskInputSubAgentType(%s) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
