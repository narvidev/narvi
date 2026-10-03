package wshub

import (
	"encoding/json"
	"testing"
)

// TestEnvelope_PeeksTheCorrelators: the read loop's envelope carries a
// frame's callId and stepId to the session actor, which derives the
// storage key of a tool_call, tool_result or step_finish from them
// (eventkey.StorageKey, technical plan §6.1). A value that is not a string
// reads as absent, never failing the envelope: such a frame was stored
// before the envelope peeked them, and still is.
func TestEnvelope_PeeksTheCorrelators(t *testing.T) {
	tests := []struct {
		name               string
		frame              string
		wantCall, wantStep string
	}{
		{name: "a tool_call's callId", frame: `{"type":"tool_call","messageId":"msg_1","gen":1,"callId":"call_a","toolName":"read","input":{}}`, wantCall: "call_a"},
		{name: "a tool_result's callId", frame: `{"type":"tool_result","messageId":"msg_1","gen":1,"callId":"call_a","output":{},"isError":false}`, wantCall: "call_a"},
		{name: "a step_finish's stepId", frame: `{"type":"step_finish","messageId":"msg_1","gen":1,"stepId":"prt_finish","cost":{"tokens":{"input":1,"output":1}}}`, wantStep: "prt_finish"},
		{name: "a step_start's stepId", frame: `{"type":"step_start","messageId":"msg_1","gen":1,"stepId":"prt_start"}`, wantStep: "prt_start"},
		{name: "neither", frame: `{"type":"warning","messageId":"w1","gen":1,"message":"m"}`},
		{name: "a callId that is not a string reads as absent", frame: `{"type":"tool_call","messageId":"msg_1","gen":1,"callId":7,"toolName":"read","input":{}}`},
		{name: "a stepId that is null reads as absent", frame: `{"type":"step_finish","messageId":"msg_1","gen":1,"stepId":null,"cost":{"tokens":{"input":1,"output":1}}}`},
		{name: "an object callId reads as absent", frame: `{"type":"tool_result","messageId":"msg_1","gen":1,"callId":{"x":1},"output":{},"isError":false}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var env envelope
			if err := json.Unmarshal([]byte(tt.frame), &env); err != nil {
				t.Fatalf("decode envelope: %v; want the frame decoded, whatever its correlators", err)
			}
			if string(env.CallID) != tt.wantCall || string(env.StepID) != tt.wantStep {
				t.Errorf("callId %q, stepId %q; want %q, %q", env.CallID, env.StepID, tt.wantCall, tt.wantStep)
			}
			if env.MessageID == "" || env.Gen != 1 {
				t.Errorf("messageId %q, gen %d; want the rest of the envelope read as before", env.MessageID, env.Gen)
			}
		})
	}
}
