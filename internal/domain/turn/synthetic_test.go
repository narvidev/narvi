package turn_test

import (
	"testing"

	"github.com/narvidev/narvi/internal/domain/turn"
)

// TestRequiresSyntheticExecutionComplete is table-driven over every
// Trigger constant plus an out-of-range value, proving the split is
// exactly: real terminal events (Complete, Fail) need no synthesis;
// control-plane-internal decisions (Timeout, Abandon, Cancel) do; and
// non-terminal triggers (Dispatch, StartProcessing) — and anything
// unrecognized — report false since there is nothing to synthesize.
func TestRequiresSyntheticExecutionComplete(t *testing.T) {
	t.Parallel()

	tests := []struct {
		trigger turn.Trigger
		want    bool
	}{
		{turn.TriggerDispatch, false},
		{turn.TriggerStartProcessing, false},
		{turn.TriggerComplete, false},
		{turn.TriggerFail, false},
		{turn.TriggerTimeout, true},
		{turn.TriggerAbandon, true},
		{turn.TriggerCancel, true},
		{turn.Trigger(999), false},
	}

	for _, tc := range tests {
		t.Run(tc.trigger.String(), func(t *testing.T) {
			t.Parallel()
			if got := turn.RequiresSyntheticExecutionComplete(tc.trigger); got != tc.want {
				t.Errorf("RequiresSyntheticExecutionComplete(%v) = %v, want %v", tc.trigger, got, tc.want)
			}
		})
	}
}

// TestSyntheticEndIsOfDispatchedTurn: a synthetic end of a turn ended from
// dispatched or processing ends the session's turn in flight; one ended
// from pending, never dispatched, does not, and neither does any other
// state.
func TestSyntheticEndIsOfDispatchedTurn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		from turn.State
		want bool
	}{
		{turn.StatePending, false},
		{turn.StateDispatched, true},
		{turn.StateProcessing, true},
		{turn.StateCompleted, false},
		{turn.StateFailed, false},
		{turn.StateCancelled, false},
		{turn.State("unknown"), false},
	}
	for _, tc := range tests {
		t.Run(string(tc.from), func(t *testing.T) {
			t.Parallel()
			if got := turn.SyntheticEndIsOfDispatchedTurn(tc.from); got != tc.want {
				t.Errorf("SyntheticEndIsOfDispatchedTurn(%q) = %v, want %v", tc.from, got, tc.want)
			}
		})
	}
}
