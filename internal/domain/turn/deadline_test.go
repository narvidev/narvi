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
// the deadline however late the re-send comes; the turn is spent once no
// more than the re-send floor is left -- with no floor, exactly where
// EvaluateTurnDeadline times the same turn out, and with one, where it
// would time it out within the floor; and a turn with no recorded
// dispatch time gets a full deadline from now, never spent.
func TestReenqueueBound(t *testing.T) {
	t.Parallel()

	const deadline, floor = 60 * time.Minute, 11 * time.Minute
	dispatchedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	bound := dispatchedAt.Add(deadline)

	for _, tc := range []struct {
		name         string
		floor        time.Duration
		dispatchedAt time.Time
		now          time.Time
		wantBound    time.Time
		wantSpent    bool
	}{
		{
			name: "re-sent right after its dispatch: the first dispatch's bound", floor: floor,
			dispatchedAt: dispatchedAt, now: dispatchedAt.Add(time.Second), wantBound: bound,
		},
		{
			name: "re-sent late, more than the floor left: the bound does not move", floor: floor,
			dispatchedAt: dispatchedAt, now: bound.Add(-20 * time.Minute), wantBound: bound,
		},
		{
			name: "one nanosecond more than the floor left: re-sent", floor: floor,
			dispatchedAt: dispatchedAt, now: bound.Add(-floor - time.Nanosecond), wantBound: bound,
		},
		{
			name: "exactly the floor left: spent", floor: floor,
			dispatchedAt: dispatchedAt, now: bound.Add(-floor), wantBound: bound, wantSpent: true,
		},
		{
			name: "seconds left: spent", floor: floor,
			dispatchedAt: dispatchedAt, now: bound.Add(-3 * time.Second), wantBound: bound, wantSpent: true,
		},
		{
			name: "exactly at the bound: spent", floor: floor,
			dispatchedAt: dispatchedAt, now: bound, wantBound: bound, wantSpent: true,
		},
		{
			name: "past the bound: spent", floor: floor,
			dispatchedAt: dispatchedAt, now: dispatchedAt.Add(3 * time.Hour), wantBound: bound, wantSpent: true,
		},
		{
			name:         "no floor, one nanosecond left: re-sent",
			dispatchedAt: dispatchedAt, now: bound.Add(-time.Nanosecond), wantBound: bound,
		},
		{
			name:         "no floor, exactly at the bound: spent (>=, as EvaluateTurnDeadline)",
			dispatchedAt: dispatchedAt, now: bound, wantBound: bound, wantSpent: true,
		},
		{
			name: "no dispatch time recorded: a full deadline from now", floor: floor,
			now: dispatchedAt.Add(3 * time.Hour), wantBound: dispatchedAt.Add(4 * time.Hour),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := turn.DeadlineConfig{Deadline: deadline, ReenqueueFloor: tc.floor}
			gotBound, spent := turn.ReenqueueBound(tc.dispatchedAt, tc.now, cfg)
			if !gotBound.Equal(tc.wantBound) || spent != tc.wantSpent {
				t.Fatalf("ReenqueueBound = (%v, %v), want (%v, %v)", gotBound, spent, tc.wantBound, tc.wantSpent)
			}
			if tc.dispatchedAt.IsZero() {
				return
			}
			if timedOut := turn.EvaluateTurnDeadline(tc.dispatchedAt, tc.now.Add(tc.floor), cfg).IsTimedOut; timedOut != spent {
				t.Fatalf("spent = %v but EvaluateTurnDeadline at now plus the floor = %v: a turn not re-sent must be one its deadline ends within the floor", spent, timedOut)
			}
		})
	}
}
