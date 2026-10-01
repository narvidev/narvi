package turn_test

import (
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/domain/turn"
)

// allPromptResendFacts is the one combination PromptReconnectToAnswer
// answers true for.
func allPromptResendFacts() turn.PromptResendFacts {
	return turn.PromptResendFacts{
		Processing:            true,
		ReceiptRequested:      true,
		StopRequested:         false,
		GenCapable:            true,
		RetirementOwed:        false,
		ReconnectedSinceCheck: true,
	}
}

// TestPromptReconnectToAnswer flips each fact alone away from the one
// combination that answers a reconnect: every flip alone answers false.
func TestPromptReconnectToAnswer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		flip func(*turn.PromptResendFacts)
		want bool
	}{
		{name: "every fact holds", flip: func(*turn.PromptResendFacts) {}, want: true},
		{name: "not processing", flip: func(f *turn.PromptResendFacts) { f.Processing = false }, want: false},
		{name: "its dispatch asked for no receipt", flip: func(f *turn.PromptResendFacts) { f.ReceiptRequested = false }, want: false},
		{name: "stop-flagged", flip: func(f *turn.PromptResendFacts) { f.StopRequested = true }, want: false},
		{name: "gen not capable", flip: func(f *turn.PromptResendFacts) { f.GenCapable = false }, want: false},
		{name: "gen owes a stop its retirement", flip: func(f *turn.PromptResendFacts) { f.RetirementOwed = true }, want: false},
		{name: "no ready since the last answered one", flip: func(f *turn.PromptResendFacts) { f.ReconnectedSinceCheck = false }, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := allPromptResendFacts()
			tt.flip(&f)
			if got := turn.PromptReconnectToAnswer(f); got != tt.want {
				t.Errorf("PromptReconnectToAnswer(%+v) = %v, want %v", f, got, tt.want)
			}
		})
	}
}

// TestDecidePromptResend covers a stored receipt (which wins at any age
// and count), the window's edge from both sides, the cap's edge from both
// sides, and a negative elapsed time.
func TestDecidePromptResend(t *testing.T) {
	t.Parallel()

	const window = 10 * time.Minute
	const maxResends = 3
	tests := []struct {
		name         string
		stored       bool
		sinceRequest time.Duration
		resends      int
		want         turn.PromptResendOutcome
	}{
		{name: "stored, inside the window", stored: true, sinceRequest: time.Minute, want: turn.PromptResendReceiptStored},
		{name: "stored, past the window", stored: true, sinceRequest: window + time.Hour, want: turn.PromptResendReceiptStored},
		{name: "stored, at the cap", stored: true, sinceRequest: time.Minute, resends: maxResends, want: turn.PromptResendReceiptStored},
		{name: "not stored, the window minus 1ns", stored: false, sinceRequest: window - time.Nanosecond, want: turn.PromptResendSend},
		{name: "not stored, exactly the window", stored: false, sinceRequest: window, want: turn.PromptResendWindowExpired},
		{name: "not stored, past the window", stored: false, sinceRequest: window + time.Nanosecond, want: turn.PromptResendWindowExpired},
		{name: "not stored, past the window and at the cap", stored: false, sinceRequest: window, resends: maxResends, want: turn.PromptResendWindowExpired},
		{name: "not stored, one below the cap", stored: false, sinceRequest: time.Minute, resends: maxResends - 1, want: turn.PromptResendSend},
		{name: "not stored, at the cap", stored: false, sinceRequest: time.Minute, resends: maxResends, want: turn.PromptResendCapReached},
		{name: "not stored, past the cap", stored: false, sinceRequest: time.Minute, resends: maxResends + 1, want: turn.PromptResendCapReached},
		{name: "not stored, just asked", stored: false, sinceRequest: 0, want: turn.PromptResendSend},
		{name: "not stored, negative elapsed time", stored: false, sinceRequest: -time.Second, want: turn.PromptResendSend},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := turn.DecidePromptResend(tt.stored, tt.sinceRequest, window, tt.resends, maxResends); got != tt.want {
				t.Errorf("DecidePromptResend(%v, %v, %v, %d, %d) = %v, want %v", tt.stored, tt.sinceRequest, window, tt.resends, maxResends, got, tt.want)
			}
		})
	}
}

// TestPromptResendOutcome_String pins the names the session actor logs and
// counts each outcome under.
func TestPromptResendOutcome_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		outcome turn.PromptResendOutcome
		want    string
	}{
		{turn.PromptResendReceiptStored, "receipt_stored"},
		{turn.PromptResendWindowExpired, "window_expired"},
		{turn.PromptResendSend, "send"},
		{turn.PromptResendCapReached, "cap_reached"},
		{turn.PromptResendOutcome(0), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.outcome.String(); got != tt.want {
			t.Errorf("PromptResendOutcome(%d).String() = %q, want %q", int(tt.outcome), got, tt.want)
		}
	}
}
