package automerge_test

import (
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/domain/automerge"
)

func TestEvaluateBackoff(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := automerge.BackoffConfig{
		BaseDelay: 2 * time.Minute,
		MaxDelay:  30 * time.Minute,
	}

	tests := []struct {
		name               string
		consecutiveFailure int
		wantDelay          time.Duration
		wantDeadLetter     bool
	}{
		{
			name:               "first failure schedules base delay",
			consecutiveFailure: 1,
			wantDelay:          2 * time.Minute,
		},
		{
			name:               "second failure doubles",
			consecutiveFailure: 2,
			wantDelay:          4 * time.Minute,
		},
		{
			name:               "third failure doubles again",
			consecutiveFailure: 3,
			wantDelay:          8 * time.Minute,
		},
		{
			name:               "fourth failure doubles again, one below MaxAuthFailures",
			consecutiveFailure: 4,
			wantDelay:          16 * time.Minute,
		},
		{
			name:               "consecutiveFailures below 1 treated as 1",
			consecutiveFailure: 0,
			wantDelay:          2 * time.Minute,
		},
		{
			name:               "consecutiveFailures at MaxAuthFailures dead-letters",
			consecutiveFailure: automerge.MaxAuthFailures,
			wantDeadLetter:     true,
		},
		{
			name:               "consecutiveFailures beyond MaxAuthFailures dead-letters",
			consecutiveFailure: automerge.MaxAuthFailures + 5,
			wantDeadLetter:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := automerge.EvaluateBackoff(tc.consecutiveFailure, cfg, now)

			if got.DeadLetter != tc.wantDeadLetter {
				t.Fatalf("EvaluateBackoff(%d).DeadLetter = %v, want %v", tc.consecutiveFailure, got.DeadLetter, tc.wantDeadLetter)
			}
			if tc.wantDeadLetter {
				if !got.NextRetryAt.IsZero() {
					t.Errorf("EvaluateBackoff(%d).NextRetryAt = %v, want zero value on dead-letter", tc.consecutiveFailure, got.NextRetryAt)
				}
				return
			}
			wantNextRetryAt := now.Add(tc.wantDelay)
			if !got.NextRetryAt.Equal(wantNextRetryAt) {
				t.Fatalf("EvaluateBackoff(%d).NextRetryAt = %v, want %v", tc.consecutiveFailure, got.NextRetryAt, wantNextRetryAt)
			}
		})
	}
}

// TestEvaluateBackoff_PlateausAtMaxDelay proves the schedule stops
// doubling once it reaches MaxDelay, rather than overflowing or
// continuing to grow -- exercised at a consecutiveFailures value just
// below MaxAuthFailures where BaseDelay's own doubling would already
// have exceeded MaxDelay without the cap.
func TestEvaluateBackoff_PlateausAtMaxDelay(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := automerge.BackoffConfig{
		BaseDelay: 10 * time.Minute,
		MaxDelay:  15 * time.Minute,
	}

	// consecutiveFailures=2 would double BaseDelay to 20min, which
	// already exceeds a 15min MaxDelay -- must plateau at MaxDelay, not
	// the uncapped doubled value.
	got := automerge.EvaluateBackoff(2, cfg, now)
	if got.DeadLetter {
		t.Fatalf("EvaluateBackoff(2).DeadLetter = true, want false (2 < MaxAuthFailures)")
	}
	want := now.Add(15 * time.Minute)
	if !got.NextRetryAt.Equal(want) {
		t.Errorf("EvaluateBackoff(2).NextRetryAt = %v, want %v (plateaued at MaxDelay)", got.NextRetryAt, want)
	}
}
