package automerge_test

import (
	"fmt"
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

// TestEvaluateBackoff_NegativeOrZero is the consecutiveFailures<1 clamp's
// own mutation-test witness. Under an earlier loop-based implementation
// of EvaluateBackoff, that clamp was pure dead code: a loop bounded by
// "i < consecutiveFailures" runs zero times for ANY consecutiveFailures
// <= 1, so clamping 0 (or a negative value) to 1 never changed the
// computed delay -- the table case named for exactly this
// ("consecutiveFailures below 1 treated as 1", TestEvaluateBackoff above)
// passed identically with the clamp deleted, which is a hole, not a real
// assertion (CLAUDE.md's own "mutation-verify every guard" discipline).
// EvaluateBackoff now computes the delay via a
// BaseDelay<<(consecutiveFailures-1) shift, where consecutiveFailures-1
// is the shift COUNT -- Go's own runtime panics on a negative shift
// count, so this clamp is now the only thing standing between a
// zero/negative input and a genuine crash, not merely a
// differently-computed value. Delete the clamp and this test panics (the
// recover() below turns that into a reported failure) instead of merely
// computing the SAME 2m either way.
func TestEvaluateBackoff_NegativeOrZero(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := automerge.BackoffConfig{
		BaseDelay: 2 * time.Minute,
		MaxDelay:  12 * time.Minute,
	}

	for _, cf := range []int{0, -1, -100} {
		cf := cf
		t.Run(fmt.Sprintf("consecutiveFailures=%d", cf), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("EvaluateBackoff(%d) panicked: %v -- the consecutiveFailures<1 clamp must prevent a negative shift count", cf, r)
				}
			}()

			got := automerge.EvaluateBackoff(cf, cfg, now)
			if got.DeadLetter {
				t.Fatalf("EvaluateBackoff(%d).DeadLetter = true, want false", cf)
			}
			want := now.Add(2 * time.Minute)
			if !got.NextRetryAt.Equal(want) {
				t.Errorf("EvaluateBackoff(%d).NextRetryAt = %v, want %v (treated identically to consecutiveFailures=1)", cf, got.NextRetryAt, want)
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

// TestEvaluateBackoff_ShippedDefaultsMaxDelayBinds proves platform.
// DefaultTimeouts' own AutoMergeAuthBackoffBase/AutoMergeAuthBackoffMax
// pair (2min/12min, mirrored here as literals rather than importing
// internal/platform -- this package's own doc comment already explains
// why domain/automerge imports no duration literals of its OWN, not why
// a test may not assert against the real shipped numbers directly)
// actually reaches, and is capped by, MaxDelay before dead-lettering --
// unlike the PREVIOUS shipped default (30min), which this exact
// consecutiveFailures=4 case would have left at the uncapped 16min,
// silently never exercising MaxDelay at all (docs/TECHNICAL_PLAN.md
// §17's own reachability finding: a configured ceiling that no input the
// schedule ever produces can reach has no effect, and no test proves it
// does).
func TestEvaluateBackoff_ShippedDefaultsMaxDelayBinds(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := automerge.BackoffConfig{
		BaseDelay: 2 * time.Minute,
		MaxDelay:  12 * time.Minute,
	}

	// consecutiveFailures=4 (one short of MaxAuthFailures=5) would
	// naturally double to 16min -- MaxDelay=12min must cap it down, a
	// real reduction from the uncapped value, not merely equal to it.
	got := automerge.EvaluateBackoff(4, cfg, now)
	if got.DeadLetter {
		t.Fatalf("EvaluateBackoff(4).DeadLetter = true, want false (4 < MaxAuthFailures)")
	}
	want := now.Add(12 * time.Minute)
	if !got.NextRetryAt.Equal(want) {
		t.Errorf("EvaluateBackoff(4).NextRetryAt = %v, want %v (12min MaxDelay must cap the naturally-doubled 16min)", got.NextRetryAt, want)
	}
}
