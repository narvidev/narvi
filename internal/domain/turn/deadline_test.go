package turn_test

import (
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/domain/turn"
)

// TestEvaluateTurnDeadline covers: comfortably within the deadline, past
// the deadline, and the exact boundary (>=).
func TestEvaluateTurnDeadline(t *testing.T) {
	t.Parallel()

	cfg := turn.DeadlineConfig{Deadline: 60 * time.Minute}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("well within the deadline", func(t *testing.T) {
		t.Parallel()
		startedAt := now.Add(-10 * time.Minute)
		got := turn.EvaluateTurnDeadline(startedAt, now, cfg)
		if got.IsTimedOut {
			t.Error("IsTimedOut = true, want false")
		}
		if got.Elapsed != 10*time.Minute {
			t.Errorf("Elapsed = %v, want 10m", got.Elapsed)
		}
	})

	t.Run("past the deadline", func(t *testing.T) {
		t.Parallel()
		startedAt := now.Add(-90 * time.Minute)
		got := turn.EvaluateTurnDeadline(startedAt, now, cfg)
		if !got.IsTimedOut {
			t.Error("IsTimedOut = false, want true")
		}
		if got.Elapsed != 90*time.Minute {
			t.Errorf("Elapsed = %v, want 90m", got.Elapsed)
		}
	})

	t.Run("exact boundary is timed out (>=)", func(t *testing.T) {
		t.Parallel()
		startedAt := now.Add(-cfg.Deadline)
		got := turn.EvaluateTurnDeadline(startedAt, now, cfg)
		if !got.IsTimedOut {
			t.Error("IsTimedOut = false, want true (exact boundary is >=)")
		}
	})
}

// TestReenqueueBound pins a re-sent turn's turn_deadline to its first
// dispatch's (technical plan §3.3, §35.4): the bound is dispatchedAt plus
// the deadline however late the re-send comes, spent exactly where
// EvaluateTurnDeadline times the same turn out, and a turn with no
// recorded dispatch time gets a full deadline from now, never spent.
func TestReenqueueBound(t *testing.T) {
	t.Parallel()

	cfg := turn.DeadlineConfig{Deadline: 60 * time.Minute}
	dispatchedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name         string
		dispatchedAt time.Time
		now          time.Time
		wantBound    time.Time
		wantSpent    bool
	}{
		{
			name:         "re-sent right after its dispatch: the first dispatch's bound",
			dispatchedAt: dispatchedAt,
			now:          dispatchedAt.Add(time.Second),
			wantBound:    dispatchedAt.Add(60 * time.Minute),
		},
		{
			name:         "re-sent late in its budget: the bound does not move",
			dispatchedAt: dispatchedAt,
			now:          dispatchedAt.Add(50 * time.Minute),
			wantBound:    dispatchedAt.Add(60 * time.Minute),
		},
		{
			name:         "one nanosecond before the bound: not spent",
			dispatchedAt: dispatchedAt,
			now:          dispatchedAt.Add(60*time.Minute - time.Nanosecond),
			wantBound:    dispatchedAt.Add(60 * time.Minute),
		},
		{
			name:         "exactly at the bound: spent (>=, as EvaluateTurnDeadline)",
			dispatchedAt: dispatchedAt,
			now:          dispatchedAt.Add(60 * time.Minute),
			wantBound:    dispatchedAt.Add(60 * time.Minute),
			wantSpent:    true,
		},
		{
			name:         "past the bound: spent",
			dispatchedAt: dispatchedAt,
			now:          dispatchedAt.Add(3 * time.Hour),
			wantBound:    dispatchedAt.Add(60 * time.Minute),
			wantSpent:    true,
		},
		{
			name:      "no dispatch time recorded: a full deadline from now",
			now:       dispatchedAt.Add(3 * time.Hour),
			wantBound: dispatchedAt.Add(4 * time.Hour),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bound, spent := turn.ReenqueueBound(tc.dispatchedAt, tc.now, cfg)
			if !bound.Equal(tc.wantBound) || spent != tc.wantSpent {
				t.Fatalf("ReenqueueBound = (%v, %v), want (%v, %v)", bound, spent, tc.wantBound, tc.wantSpent)
			}
			if tc.dispatchedAt.IsZero() {
				return
			}
			if timedOut := turn.EvaluateTurnDeadline(tc.dispatchedAt, tc.now, cfg).IsTimedOut; timedOut != spent {
				t.Fatalf("spent = %v but EvaluateTurnDeadline.IsTimedOut = %v: a turn not re-sent must be one its deadline ends", spent, timedOut)
			}
		})
	}
}
