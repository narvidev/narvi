package turn_test

import (
	"testing"

	"github.com/narvidev/narvi/internal/domain/turn"
)

// TestDeriveFailureReason is table-driven over every (from, trigger) pair
// in the transitions table (legal ones landing in Failed/Cancelled, legal
// ones landing in Completed, and illegal ones), proving the mapping is
// exactly: cancel -> cancelled, genuine fail -> failed, timeout ->
// timeout, abandon -> never_started, and everything else -> ("", false).
func TestDeriveFailureReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		from       turn.State
		trigger    turn.Trigger
		wantReason turn.FailureReason
		wantOK     bool
	}{
		// Cancel implies Cancelled, from every pre-terminal from-state.
		{"pending cancel -> cancelled", turn.StatePending, turn.TriggerCancel, turn.FailureReasonCancelled, true},
		{"dispatched cancel -> cancelled", turn.StateDispatched, turn.TriggerCancel, turn.FailureReasonCancelled, true},
		{"processing cancel -> cancelled", turn.StateProcessing, turn.TriggerCancel, turn.FailureReasonCancelled, true},

		// Abandon implies NeverStarted, from both pre-processing states.
		{"pending abandon -> never_started", turn.StatePending, turn.TriggerAbandon, turn.FailureReasonNeverStarted, true},
		{"dispatched abandon -> never_started", turn.StateDispatched, turn.TriggerAbandon, turn.FailureReasonNeverStarted, true},

		// Genuine fail and timeout, both only from Processing.
		{"processing genuine fail -> failed", turn.StateProcessing, turn.TriggerFail, turn.FailureReasonFailed, true},
		{"processing timeout -> timeout", turn.StateProcessing, turn.TriggerTimeout, turn.FailureReasonTimeout, true},

		// Completed carries no failure reason at all.
		{"processing complete -> no reason", turn.StateProcessing, turn.TriggerComplete, "", false},

		// Non-terminal-producing legal edges.
		{"pending dispatch -> no reason", turn.StatePending, turn.TriggerDispatch, "", false},
		{"dispatched start-processing -> no reason", turn.StateDispatched, turn.TriggerStartProcessing, "", false},

		// Illegal (from, trigger) pairs -> no reason.
		{"illegal pair -> no reason", turn.StatePending, turn.TriggerComplete, "", false},
		{"terminal state -> no reason", turn.StateCompleted, turn.TriggerCancel, "", false},
		{"unknown state -> no reason", turn.State("bogus"), turn.TriggerCancel, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reason, ok := turn.DeriveFailureReason(tc.from, tc.trigger)
			if ok != tc.wantOK {
				t.Errorf("DeriveFailureReason(%s, %v) ok = %v, want %v", tc.from, tc.trigger, ok, tc.wantOK)
			}
			if reason != tc.wantReason {
				t.Errorf("DeriveFailureReason(%s, %v) reason = %q, want %q", tc.from, tc.trigger, reason, tc.wantReason)
			}
		})
	}
}

// TestImpliedFailureReason pins which stored states carry their reason: a
// cancelled turn is always cancelled, whatever it was cancelled from; a
// failed turn could be failed, timed out or never started, so its state
// alone says nothing; no other state carries a reason.
func TestImpliedFailureReason(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		state  turn.State
		want   turn.FailureReason
		wantOK bool
	}{
		{turn.StateCancelled, turn.FailureReasonCancelled, true},
		{turn.StateFailed, "", false},
		{turn.StateCompleted, "", false},
		{turn.StatePending, "", false},
		{turn.StateDispatched, "", false},
		{turn.StateProcessing, "", false},
		{turn.State("unknown"), "", false},
	} {
		got, ok := turn.ImpliedFailureReason(tc.state)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("ImpliedFailureReason(%s) = %q, %v; want %q, %v", tc.state, got, ok, tc.want, tc.wantOK)
		}
	}
}
