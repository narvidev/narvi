package sessionactor

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// interruptionValues are interruption values an execution_complete can
// carry: the one this release knows, values a later release could add,
// and values the contract does not allow at all.
var interruptionValues = []string{
	`"sandbox_stopping"`, `"sandbox_lifetime"`, `"x"`, `""`, `null`, `1`, `1e400`, `true`, `{}`, `{"kind":"sandbox_stopping"}`, `[]`,
}

// TestDecodeExecutionComplete_AnyInterruptionValueKeepsTheFrame pins that
// an execution_complete's interruption can never cost the turn its end
// (technical plan §35.4): whatever the key holds, under its own name or in
// another case, which encoding/json also matches, the frame decodes field
// for field as the same frame without the key does -- as it did before the
// generated type named the key. The generated decode itself refuses every
// value but the one this release knows.
func TestDecodeExecutionComplete_AnyInterruptionValueKeepsTheFrame(t *testing.T) {
	t.Parallel()
	const base = `"type":"execution_complete","messageId":"m1","sessionId":"s","gen":2,"ackId":"execution_complete:m1","outcome":"cancelled","reason":"opencode: turn context canceled before completion","diagnostic":{"message":"interrupted"}`
	var want sandboxws.ExecutionComplete
	if err := json.Unmarshal([]byte(`{`+base+`}`), &want); err != nil {
		t.Fatalf("decode the frame without the key: %v", err)
	}
	for _, key := range []string{"interruption", "Interruption", "INTERRUPTION"} {
		for _, value := range interruptionValues {
			raw := json.RawMessage(`{` + base + `,"` + key + `":` + value + `}`)
			t.Run(key+"="+value, func(t *testing.T) {
				t.Parallel()
				got, err := decodeExecutionComplete(raw)
				if err != nil {
					t.Fatalf("decodeExecutionComplete(%s): %v, want the frame decoded", raw, err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("decodeExecutionComplete(%s) = %+v, want %+v: what the frame decodes to without the key", raw, got, want)
				}
				if got.Outcome != sandboxws.ExecutionCompleteOutcomeCancelled || got.Reason == nil || *got.Reason != turn.LegacyInterruptionReason {
					t.Errorf("decodeExecutionComplete(%s) = outcome %q reason %v, want cancelled with its reason", raw, got.Outcome, got.Reason)
				}
			})
		}
	}

	// The generated decode refuses a value this release does not know, so
	// a decode that kept the key would cost a later agent's end its turn.
	var generated sandboxws.ExecutionComplete
	if err := json.Unmarshal([]byte(`{`+base+`,"interruption":"sandbox_lifetime"}`), &generated); err == nil {
		t.Error("the generated decode accepts an interruption it does not know; this test no longer shows why the key is stripped")
	}
}

// TestReportedInterruption pins how the control plane reads an
// execution_complete's interruption (technical plan §35.4): on its own,
// leniently, any non-empty string counting -- a value a later release
// adds included -- and nothing for a key that is absent, null, empty or
// not a string. The reason and the diagnostic are never read.
func TestReportedInterruption(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		raw      string
		want     string
		reported bool
	}{
		{name: "the value this release knows", raw: `{"type":"execution_complete","outcome":"cancelled","interruption":"sandbox_stopping"}`, want: "sandbox_stopping", reported: true},
		{name: "a value a later release adds", raw: `{"outcome":"failed","interruption":"sandbox_lifetime"}`, want: "sandbox_lifetime", reported: true},
		{name: "on a completed end, read all the same", raw: `{"outcome":"completed","interruption":"sandbox_stopping"}`, want: "sandbox_stopping", reported: true},
		{name: "absent", raw: `{"type":"execution_complete","outcome":"cancelled","reason":"opencode: turn context canceled before completion"}`},
		{name: "null", raw: `{"interruption":null}`},
		{name: "empty", raw: `{"interruption":""}`},
		{name: "a number", raw: `{"interruption":1}`},
		{name: "a boolean", raw: `{"interruption":true}`},
		{name: "an object", raw: `{"interruption":{"kind":"sandbox_stopping"}}`},
		{name: "a list", raw: `{"interruption":["sandbox_stopping"]}`},
		{name: "only inside the diagnostic", raw: `{"diagnostic":{"interruption":"sandbox_stopping","message":"after 3600 seconds"}}`},
		{name: "a reason that names it", raw: `{"outcome":"cancelled","reason":"interruption: sandbox_stopping after 3600s"}`},
		{name: "a frame that is not JSON", raw: `{"interruption":"sandbox_stopping"`},
		{name: "a frame that is not an object", raw: `["sandbox_stopping"]`},
		{name: "the key in another case, as the generated decode matches it", raw: `{"Interruption":"sandbox_stopping"}`, want: "sandbox_stopping", reported: true},
		{name: "the last of two members wins", raw: `{"interruption":"sandbox_stopping","Interruption":null}`},
		{name: "the last of two members wins, a value", raw: `{"interruption":7,"interruption":"sandbox_stopping"}`, want: "sandbox_stopping", reported: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, reported := reportedInterruption(json.RawMessage(tc.raw))
			if got != tc.want || reported != tc.reported {
				t.Errorf("reportedInterruption(%s) = (%q, %v), want (%q, %v)", tc.raw, got, reported, tc.want, tc.reported)
			}
		})
	}
}
