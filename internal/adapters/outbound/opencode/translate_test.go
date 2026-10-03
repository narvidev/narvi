package opencode

import (
	"encoding/json"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/app/ports"
)

func TestToolInputEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  json.RawMessage
		want bool
	}{
		{"absent", nil, true},
		{"empty object", json.RawMessage(`{}`), true},
		{"whitespace-padded empty object", json.RawMessage(` { } `), true},
		{"non-empty object", json.RawMessage(`{"command":"echo hi"}`), false},
		{"malformed json treated as empty", json.RawMessage(`{not json`), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := toolInputEmpty(tt.raw); got != tt.want {
				t.Errorf("toolInputEmpty(%s) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestDecodeToolInput(t *testing.T) {
	t.Parallel()

	input, err := decodeToolInput(json.RawMessage(`{"command":"echo hi","workdir":"/tmp"}`))
	if err != nil {
		t.Fatalf("decodeToolInput() error = %v", err)
	}
	if input["command"] != "echo hi" || input["workdir"] != "/tmp" {
		t.Errorf("decodeToolInput() = %#v, want command/workdir populated", input)
	}

	if _, err := decodeToolInput(json.RawMessage(`{bad`)); err == nil {
		t.Error("decodeToolInput() with malformed JSON: error = nil, want non-nil")
	}
}

// translateToolResult wraps OpenCode's own STRING-shaped completed/error
// output into wire ToolResult.Output's own object shape -- this is this
// adapter's own documented convention (translate.go), verified here
// directly since the wire contract requires an object and OpenCode's own
// state.output/state.error are plain strings.
func TestTranslateToolResult_WrapsStringOutputAsObject(t *testing.T) {
	t.Parallel()

	cmd := sandboxws.Prompt{SessionId: testSessionID, Gen: 3}

	t.Run("completed", func(t *testing.T) {
		t.Parallel()
		output := "hello world\n"
		p := toolPart{
			ID: "prt_1", MessageID: "msg_1", Tool: "bash", CallID: "call_1",
			State: toolPartState{Status: "completed", Output: &output},
		}
		result := translateToolResult(cmd, p, "")
		if result.IsError {
			t.Error("IsError = true, want false for a completed call")
		}
		if result.Output["output"] != output {
			t.Errorf(`Output["output"] = %#v, want %q`, result.Output["output"], output)
		}
		if result.SessionId != cmd.SessionId || result.Gen != cmd.Gen {
			t.Errorf("SessionId/Gen not stamped from cmd: got %q/%d", result.SessionId, result.Gen)
		}
		if result.SubTaskId != nil {
			t.Errorf("SubTaskId = %v, want nil for a main-lane (subTaskID==\"\") call", result.SubTaskId)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		errMsg := "command not found"
		p := toolPart{
			ID: "prt_2", MessageID: "msg_1", Tool: "bash", CallID: "call_2",
			State: toolPartState{Status: "error", Error: &errMsg},
		}
		result := translateToolResult(cmd, p, "")
		if !result.IsError {
			t.Error("IsError = false, want true for an error call")
		}
		if result.Output["error"] != errMsg {
			t.Errorf(`Output["error"] = %#v, want %q`, result.Output["error"], errMsg)
		}
	})
}

// translateStepFinish must always produce the object-shaped cost.tokens
// §6.1/§9.1 dedicate their own explicit warning to.
func TestTranslateStepFinish_CostTokensIsObjectShaped(t *testing.T) {
	t.Parallel()

	cmd := sandboxws.Prompt{SessionId: testSessionID, Gen: 1}
	usd := 0.0042
	part := stepFinishPart{
		ID: "prt_step1", MessageID: "msg_1", Cost: &usd,
		Tokens: stepFinishTokens{Input: 100, Output: 50, Cache: stepFinishCache{Read: 12}},
	}

	got := translateStepFinish(cmd, part, "")
	if got.Cost.Tokens.Input != 100 || got.Cost.Tokens.Output != 50 {
		t.Errorf("Cost.Tokens = %#v, want Input=100 Output=50", got.Cost.Tokens)
	}
	if got.Cost.Tokens.Cached == nil || *got.Cost.Tokens.Cached != 12 {
		t.Errorf("Cost.Tokens.Cached = %v, want 12 (from tokens.cache.read)", got.Cost.Tokens.Cached)
	}
	if got.Cost.Usd == nil || *got.Cost.Usd != 0.0042 {
		t.Errorf("Cost.Usd = %v, want 0.0042", got.Cost.Usd)
	}

	// Marshal round-trip must produce an OBJECT for "tokens", never a bare
	// number (the exact regression contracts/contractstest's own dedicated
	// test guards against for the wire schema itself).
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	cost, ok := decoded["cost"].(map[string]any)
	if !ok {
		t.Fatalf("cost is not an object: %#v", decoded["cost"])
	}
	if _, ok := cost["tokens"].(map[string]any); !ok {
		t.Fatalf("cost.tokens is not an object: %#v", cost["tokens"])
	}
}

// TestTranslateStepFinish_NilCost_UsdStaysNil proves §25.15's own fix to
// stepFinishPart.Cost (types.go: *float64, not float64): a step-finish
// part decoded with NO "cost" key at all (OpenCode's own JSON genuinely
// omitting it -- simulated here directly at the struct level, since that
// is exactly what json.Unmarshal into stepFinishPart would produce for a
// missing key) must translate to a NIL wire Cost.Usd, never a fabricated
// 0 -- the distinction the whole point of the *float64 change is to
// preserve through to the wire.
func TestTranslateStepFinish_NilCost_UsdStaysNil(t *testing.T) {
	t.Parallel()

	cmd := sandboxws.Prompt{SessionId: testSessionID, Gen: 1}
	part := stepFinishPart{
		ID: "prt_step1", MessageID: "msg_1", Cost: nil,
		Tokens: stepFinishTokens{Input: 100, Output: 50},
	}

	got := translateStepFinish(cmd, part, "")
	if got.Cost.Usd != nil {
		t.Errorf("Cost.Usd = %v, want nil (no \"cost\" key on the underlying part)", *got.Cost.Usd)
	}

	// And prove this is not vacuous by also checking the explicit-zero
	// case decodes distinctly: a real "cost":0 must NOT collapse onto the
	// same nil result.
	zero := 0.0
	partZero := part
	partZero.Cost = &zero
	gotZero := translateStepFinish(cmd, partZero, "")
	if gotZero.Cost.Usd == nil {
		t.Error("Cost.Usd = nil for an explicit cost:0 part, want a non-nil pointer to 0 -- nil must mean 'absent', not 'zero'")
	} else if *gotZero.Cost.Usd != 0 {
		t.Errorf("Cost.Usd = %v, want 0", *gotZero.Cost.Usd)
	}
}

func TestTranslateSubTaskStartAndFinish(t *testing.T) {
	t.Parallel()

	cmd := sandboxws.Prompt{SessionId: testSessionID, Gen: 2}

	start := translateSubTaskStart(cmd, subtaskPart{ID: "prt_sub1", MessageID: "msg_5", Description: "Investigate flaky test"})
	if start.SubTaskId != "prt_sub1" || start.Label != "Investigate flaky test" || start.ParentMessageId != "msg_5" {
		t.Errorf("translateSubTaskStart() = %#v, unexpected fields", start)
	}
	if start.SessionId != cmd.SessionId || start.Gen != cmd.Gen {
		t.Errorf("translateSubTaskStart() SessionId/Gen not stamped from cmd: got %q/%d", start.SessionId, start.Gen)
	}
	// The legacy subtask part names no tool call: a reader falls back to a
	// task call of its message.
	if start.ParentCallId != nil {
		t.Errorf("translateSubTaskStart() ParentCallId = %q, want absent: a subtask part has no tool call", *start.ParentCallId)
	}

	finish := translateSubTaskFinish(cmd, "prt_sub1", sandboxws.ExecutionCompleteOutcomeFailed)
	if finish.SubTaskId != "prt_sub1" {
		t.Errorf("translateSubTaskFinish() SubTaskId = %q, want prt_sub1", finish.SubTaskId)
	}
	if finish.Outcome != sandboxws.SubTaskFinishOutcomeFailed {
		t.Errorf("translateSubTaskFinish() Outcome = %q, want failed", finish.Outcome)
	}
	if finish.AckId != "sub_task_finish:"+finish.MessageId {
		t.Errorf("translateSubTaskFinish() AckId = %q, want deterministic sub_task_finish:{messageId}", finish.AckId)
	}
}

func TestClassifyAgentEvent_SubTaskFinishIsCritical(t *testing.T) {
	t.Parallel()

	finish := translateSubTaskFinish(sandboxws.Prompt{SessionId: testSessionID, Gen: 1}, "prt_1", sandboxws.ExecutionCompleteOutcomeCompleted)
	// turnState.emit (turn.go) routes every translated event through this
	// SAME shared classifier in production; assert it directly here for a
	// fast, isolated check.
	critical, ackID := ports.ClassifyAgentEvent(finish)
	if !critical {
		t.Error("SubTaskFinish classified as non-critical, want critical (6th critical type, §6.1)")
	}
	if ackID != finish.AckId {
		t.Errorf("ackID = %q, want %q", ackID, finish.AckId)
	}
}
