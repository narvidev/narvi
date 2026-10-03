package eventkey

import "testing"

// TestEventStorageKey: the three types derive their keys from their own
// correlator, every other type keeps its wire messageId whatever correlator
// it carries, and an event without its correlator keeps the bare one.
func TestEventStorageKey(t *testing.T) {
	tests := []struct {
		name                     string
		eventType, msg, call, st string
		want                     string
		keyedByCorrelator        bool
	}{
		{name: "tool_call by its callId", eventType: "tool_call", msg: "msg_1", call: "call_a", want: "msg_1#tool_call:call_a", keyedByCorrelator: true},
		{name: "tool_result by its callId", eventType: "tool_result", msg: "msg_1", call: "call_a", want: "msg_1#tool_result:call_a", keyedByCorrelator: true},
		{name: "step_finish by its stepId", eventType: "step_finish", msg: "msg_1", st: "prt_finish", want: "msg_1#step_finish:prt_finish", keyedByCorrelator: true},
		{name: "a tool_call and its result never share a key", eventType: "tool_result", msg: "msg_1", call: "call_a", st: "ignored", want: "msg_1#tool_result:call_a", keyedByCorrelator: true},
		{name: "tool_call reads callId, never stepId", eventType: "tool_call", msg: "msg_1", st: "prt_step", want: "msg_1", keyedByCorrelator: true},
		{name: "step_finish reads stepId, never callId", eventType: "step_finish", msg: "msg_1", call: "call_a", want: "msg_1", keyedByCorrelator: true},
		{name: "tool_call without its callId keeps the bare messageId", eventType: "tool_call", msg: "msg_1", want: "msg_1", keyedByCorrelator: true},
		{name: "tool_result without its callId keeps the bare messageId", eventType: "tool_result", msg: "msg_1", want: "msg_1", keyedByCorrelator: true},
		{name: "step_finish without its stepId keeps the bare messageId", eventType: "step_finish", msg: "msg_1", want: "msg_1", keyedByCorrelator: true},
		{name: "step_start keeps its messageId: it claims the message's row", eventType: "step_start", msg: "msg_1", st: "prt_step", want: "msg_1"},
		{name: "token keeps its messageId: its later keys are the actor's", eventType: "token", msg: "prt_text", want: "prt_text"},
		{name: "warning keeps its messageId", eventType: "warning", msg: "msg_1", call: "call_a", st: "prt_step", want: "msg_1"},
		{name: "sub_task_start keeps its messageId", eventType: "sub_task_start", msg: "c0ffee", call: "call_task", want: "c0ffee"},
		{name: "execution_complete keeps its messageId", eventType: "execution_complete", msg: "ec-1", want: "ec-1"},
		{name: "an unknown type keeps its messageId", eventType: "some_future_type", msg: "m", call: "c", st: "s", want: "m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StorageKey(tt.eventType, tt.msg, tt.call, tt.st); got != tt.want {
				t.Errorf("StorageKey(%q, %q, %q, %q) = %q, want %q", tt.eventType, tt.msg, tt.call, tt.st, got, tt.want)
			}
			if got := KeyedByCorrelator(tt.eventType); got != tt.keyedByCorrelator {
				t.Errorf("KeyedByCorrelator(%q) = %v, want %v", tt.eventType, got, tt.keyedByCorrelator)
			}
		})
	}
}
