package turn_test

import (
	"errors"
	"testing"

	"github.com/narvidev/narvi/internal/domain/turn"
)

// TestTransition_LegalEdges is table-driven over every legal (from,
// trigger) edge the state machine defines (§3.3: the linear happy path,
// every abandon edge, every cancel edge, and both Processing terminal-
// failure edges), asserting the exact destination state and a nil error.
func TestTransition_LegalEdges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		from    turn.State
		trigger turn.Trigger
		want    turn.State
	}{
		{"pending -> dispatched (dispatch)", turn.StatePending, turn.TriggerDispatch, turn.StateDispatched},
		{"dispatched -> processing (start processing)", turn.StateDispatched, turn.TriggerStartProcessing, turn.StateProcessing},
		{"processing -> completed (complete)", turn.StateProcessing, turn.TriggerComplete, turn.StateCompleted},
		{"processing -> failed (genuine fail)", turn.StateProcessing, turn.TriggerFail, turn.StateFailed},
		{"processing -> failed (timeout)", turn.StateProcessing, turn.TriggerTimeout, turn.StateFailed},

		// Abandon, from both pre-processing states.
		{"pending -> failed (abandon)", turn.StatePending, turn.TriggerAbandon, turn.StateFailed},
		{"dispatched -> failed (abandon)", turn.StateDispatched, turn.TriggerAbandon, turn.StateFailed},

		// Cancel, from every pre-terminal state.
		{"pending -> cancelled (cancel)", turn.StatePending, turn.TriggerCancel, turn.StateCancelled},
		{"dispatched -> cancelled (cancel)", turn.StateDispatched, turn.TriggerCancel, turn.StateCancelled},
		{"processing -> cancelled (cancel)", turn.StateProcessing, turn.TriggerCancel, turn.StateCancelled},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := turn.Transition(tc.from, tc.trigger)
			if err != nil {
				t.Fatalf("Transition(%s, %v) unexpected error: %v", tc.from, tc.trigger, err)
			}
			if got != tc.want {
				t.Errorf("Transition(%s, %v) = %s, want %s", tc.from, tc.trigger, got, tc.want)
			}
		})
	}
}

// TestTransition_IllegalEdges covers every (from, trigger) combination
// that is not in the transitions table, including every trigger applied
// to each of the three terminal states (Completed/Failed/Cancelled have no
// outgoing edges at all) and every mismatched (from, trigger) pair among
// the non-terminal states.
func TestTransition_IllegalEdges(t *testing.T) {
	t.Parallel()

	allTriggers := []turn.Trigger{
		turn.TriggerDispatch, turn.TriggerStartProcessing, turn.TriggerComplete,
		turn.TriggerFail, turn.TriggerTimeout, turn.TriggerAbandon, turn.TriggerCancel,
	}

	tests := []struct {
		name    string
		from    turn.State
		trigger turn.Trigger
	}{
		{"pending cannot start-processing directly", turn.StatePending, turn.TriggerStartProcessing},
		{"pending cannot complete", turn.StatePending, turn.TriggerComplete},
		{"pending cannot genuinely fail", turn.StatePending, turn.TriggerFail},
		{"pending cannot timeout", turn.StatePending, turn.TriggerTimeout},
		{"dispatched cannot dispatch again", turn.StateDispatched, turn.TriggerDispatch},
		{"dispatched cannot complete", turn.StateDispatched, turn.TriggerComplete},
		{"dispatched cannot genuinely fail", turn.StateDispatched, turn.TriggerFail},
		{"dispatched cannot timeout", turn.StateDispatched, turn.TriggerTimeout},
		{"processing cannot dispatch again", turn.StateProcessing, turn.TriggerDispatch},
		{"processing cannot start-processing again", turn.StateProcessing, turn.TriggerStartProcessing},
		{"processing cannot abandon (already started)", turn.StateProcessing, turn.TriggerAbandon},
		{"unknown state is always illegal", turn.State("bogus"), turn.TriggerDispatch},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertIllegal(t, tc.from, tc.trigger)
		})
	}

	// Every trigger is illegal from every terminal state: no outgoing
	// edges exist at all once a turn reaches Completed, Failed, or
	// Cancelled.
	for _, from := range []turn.State{turn.StateCompleted, turn.StateFailed, turn.StateCancelled} {
		for _, trig := range allTriggers {
			t.Run("terminal state "+string(from)+" has no outgoing edge via "+trig.String(), func(t *testing.T) {
				t.Parallel()
				assertIllegal(t, from, trig)
			})
		}
	}
}

func assertIllegal(t *testing.T, from turn.State, trig turn.Trigger) {
	t.Helper()

	_, err := turn.Transition(from, trig)
	if err == nil {
		t.Fatalf("Transition(%s, %v) = nil error, want an error", from, trig)
	}
	if !errors.Is(err, turn.ErrIllegalTransition) {
		t.Errorf("Transition(%s, %v) error = %v, want errors.Is(err, ErrIllegalTransition)", from, trig, err)
	}
	var illegal *turn.IllegalTransitionError
	if !errors.As(err, &illegal) {
		t.Fatalf("Transition(%s, %v) error = %v, want *IllegalTransitionError", from, trig, err)
	}
	if illegal.From != from || illegal.Trigger != trig {
		t.Errorf("IllegalTransitionError = %+v, want From=%s Trigger=%s", illegal, from, trig)
	}
	if illegal.Error() == "" {
		t.Error("IllegalTransitionError.Error() is empty")
	}
}

// TestTrigger_String covers every named Trigger constant plus the
// out-of-range fallback branch.
func TestTrigger_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		trigger turn.Trigger
		want    string
	}{
		{turn.TriggerDispatch, "dispatch"},
		{turn.TriggerStartProcessing, "start_processing"},
		{turn.TriggerComplete, "complete"},
		{turn.TriggerFail, "fail"},
		{turn.TriggerTimeout, "timeout"},
		{turn.TriggerAbandon, "abandon"},
		{turn.TriggerCancel, "cancel"},
		{turn.Trigger(-1), "Trigger(-1)"},
		{turn.Trigger(999), "Trigger(999)"},
	}

	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := tc.trigger.String(); got != tc.want {
				t.Errorf("Trigger(%d).String() = %q, want %q", int(tc.trigger), got, tc.want)
			}
		})
	}
}

// TestIsTerminal covers every known state plus an unrecognized one, proving
// the terminal set is exactly {completed, failed, cancelled}.
func TestIsTerminal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status turn.State
		want   bool
	}{
		{turn.StatePending, false},
		{turn.StateDispatched, false},
		{turn.StateProcessing, false},
		{turn.StateCompleted, true},
		{turn.StateFailed, true},
		{turn.StateCancelled, true},
		{turn.State("some_future_status"), false},
	}

	for _, tc := range tests {
		t.Run(string(tc.status), func(t *testing.T) {
			t.Parallel()
			if got := turn.IsTerminal(tc.status); got != tc.want {
				t.Errorf("IsTerminal(%s) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

// TestIgnoredForEndReason pins which turns the session's status derivation
// skips (technical plan §24.9): any end reason at all, as every SQL reader
// of attempts keeps only end_reason IS NULL.
func TestIgnoredForEndReason(t *testing.T) {
	t.Parallel()

	contextMoved := turn.EndReasonContextMoved
	unknown := "a_reason_a_newer_binary_wrote"
	for _, tc := range []struct {
		name      string
		endReason *string
		want      bool
	}{
		{name: "no end reason", endReason: nil, want: false},
		{name: "context_moved", endReason: &contextMoved, want: true},
		{name: "an end reason this binary does not know", endReason: &unknown, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := turn.IgnoredForEndReason(tc.endReason); got != tc.want {
				t.Errorf("IgnoredForEndReason(%v) = %v, want %v", tc.endReason, got, tc.want)
			}
		})
	}
}

// TestContextCheckedAtDispatch pins which turns technical plan §24.9's
// context check applies to: a review attempt the automatic re-review or a
// person's request lane asked for, and nothing else -- not a turn that is
// no review attempt, not a turn with no recorded trigger or one no lane
// records. It pins IsHumanRequestTrigger beside it: the three lanes whose
// moved attempt is owed to its requester, never the automatic one.
func TestContextCheckedAtDispatch(t *testing.T) {
	t.Parallel()

	ptr := func(s string) *string { return &s }
	for _, tc := range []struct {
		name            string
		isReviewAttempt bool
		trigger         *string
		checked, human  bool
	}{
		{name: "an automatic review attempt", isReviewAttempt: true, trigger: ptr(turn.RequestTriggerAuto), checked: true},
		{name: "a review attempt the label asked for", isReviewAttempt: true, trigger: ptr(turn.RequestTriggerLabel), checked: true, human: true},
		{name: "a review attempt the button asked for", isReviewAttempt: true, trigger: ptr(turn.RequestTriggerButton), checked: true, human: true},
		{name: "a review attempt a mention asked for", isReviewAttempt: true, trigger: ptr(turn.RequestTriggerMention), checked: true, human: true},
		{name: "a review attempt with no trigger recorded", isReviewAttempt: true},
		{name: "a review attempt with an empty trigger", isReviewAttempt: true, trigger: ptr("")},
		{name: "a review attempt with a trigger no lane records", isReviewAttempt: true, trigger: ptr("slack")},
		{name: "an automatic turn that is no review attempt", trigger: ptr(turn.RequestTriggerAuto)},
		{name: "a follow-up mention, no review attempt", trigger: ptr(turn.RequestTriggerMention), human: true},
		{name: "a follow-up", trigger: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := turn.ContextCheckedAtDispatch(tc.isReviewAttempt, tc.trigger); got != tc.checked {
				t.Errorf("ContextCheckedAtDispatch(%v, %v) = %v, want %v", tc.isReviewAttempt, tc.trigger, got, tc.checked)
			}
			if got := turn.IsHumanRequestTrigger(tc.trigger); got != tc.human {
				t.Errorf("IsHumanRequestTrigger(%v) = %v, want %v", tc.trigger, got, tc.human)
			}
		})
	}
}
