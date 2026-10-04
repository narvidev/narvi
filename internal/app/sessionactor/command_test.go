package sessionactor_test

import (
	"testing"

	"github.com/narvidev/narvi/internal/app/sessionactor"
)

// TestTimerFired_IsCommand proves TimerFired satisfies the Command
// interface and that its Name field round-trips unchanged through it --
// the mailbox's message shape (§2), table-driven over the 5 named
// timers §2 defines plus an arbitrary/unknown name (§2: "name is kept
// TEXT, not an enum" -- an unrecognized name is still a valid Command
// value; handleTimerFired's dispatch just ignores it defensively).
func TestTimerFired_IsCommand(t *testing.T) {
	t.Parallel()

	names := []string{
		sessionactor.TimerConnectingDeadline,
		sessionactor.TimerLivenessCheck,
		sessionactor.TimerInactivity,
		sessionactor.TimerTurnDeadline,
		sessionactor.TimerTerminalGrace,
		"some_unknown_timer_name",
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var cmd sessionactor.Command = sessionactor.TimerFired{Name: name}

			fired, ok := cmd.(sessionactor.TimerFired)
			if !ok {
				t.Fatalf("type assertion to TimerFired failed for a Command built from %q", name)
			}
			if fired.Name != name {
				t.Errorf("TimerFired.Name = %q, want %q", fired.Name, name)
			}
		})
	}
}

// TestEnsureDispatched_IsCommand proves EnsureDispatched satisfies the
// Command interface -- a zero-payload signal, so there is nothing to
// round-trip beyond the type assertion itself (§9.3, "e2e happy path").
func TestEnsureDispatched_IsCommand(t *testing.T) {
	t.Parallel()

	var cmd sessionactor.Command = sessionactor.EnsureDispatched{}

	if _, ok := cmd.(sessionactor.EnsureDispatched); !ok {
		t.Fatalf("type assertion to EnsureDispatched failed")
	}
}

// TestTimerNameConstants proves the 5 named-timer constants match §2's
// own list verbatim -- a typo here would silently desync the pump/
// dispatch machinery from the plan's actual timer names.
func TestTimerNameConstants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		got  string
		want string
	}{
		{sessionactor.TimerConnectingDeadline, "connecting_deadline"},
		{sessionactor.TimerLivenessCheck, "liveness_check"},
		{sessionactor.TimerInactivity, "inactivity"},
		{sessionactor.TimerTurnDeadline, "turn_deadline"},
		{sessionactor.TimerTerminalGrace, "terminal_grace"},
		{sessionactor.TimerOwedReviewRequest, "owed_review_request"},
	}

	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("timer name constant = %q, want %q", tc.got, tc.want)
		}
	}
}
