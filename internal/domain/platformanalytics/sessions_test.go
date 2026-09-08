package platformanalytics_test

import (
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/domain/platformanalytics"
	"github.com/narvidev/narvi/internal/domain/session"
)

func TestTotalSessions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		counts []platformanalytics.OutcomeCount
		want   int
	}{
		{name: "empty", counts: nil, want: 0},
		{
			name: "sums every status, including Created and Active",
			counts: []platformanalytics.OutcomeCount{
				{Status: session.StatusCreated, Count: 3},
				{Status: session.StatusActive, Count: 2},
				{Status: session.StatusCompleted, Count: 10},
				{Status: session.StatusFailed, Count: 4},
				{Status: session.StatusCancelled, Count: 1},
			},
			want: 20,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := platformanalytics.TotalSessions(tt.counts); got != tt.want {
				t.Errorf("TotalSessions(%+v) = %d, want %d", tt.counts, got, tt.want)
			}
		})
	}
}

func TestSessionsPerDay_EmptyIsNotYetComputed(t *testing.T) {
	t.Parallel()

	buckets, ok := platformanalytics.SessionsPerDay(nil)
	if ok {
		t.Fatalf("SessionsPerDay(nil) ok = true, want false (not yet computed)")
	}
	if buckets != nil {
		t.Fatalf("SessionsPerDay(nil) buckets = %v, want nil", buckets)
	}
}

func TestSessionsPerDay_BucketsByDayAndSumsAcrossFailureReason(t *testing.T) {
	t.Parallel()

	day1 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)

	counts := []platformanalytics.OutcomeCount{
		{Day: day1, Status: session.StatusCompleted, Count: 5},
		// Two different failure reasons on the SAME day+status must
		// collapse into one summed bucket entry, not two.
		{Day: day1, Status: session.StatusFailed, FailureReason: session.FailureReasonTimeout, Count: 2},
		{Day: day1, Status: session.StatusFailed, FailureReason: session.FailureReasonFailed, Count: 1},
		{Day: day2, Status: session.StatusCancelled, FailureReason: session.FailureReasonCancelled, Count: 3},
	}

	buckets, ok := platformanalytics.SessionsPerDay(counts)
	if !ok {
		t.Fatalf("SessionsPerDay(counts) ok = false, want true")
	}
	if len(buckets) != 2 {
		t.Fatalf("SessionsPerDay(counts) = %d buckets, want 2", len(buckets))
	}

	if !buckets[0].Day.Equal(day1) {
		t.Errorf("buckets[0].Day = %v, want %v", buckets[0].Day, day1)
	}
	if got := buckets[0].Counts[session.StatusCompleted]; got != 5 {
		t.Errorf("buckets[0].Counts[completed] = %d, want 5", got)
	}
	if got := buckets[0].Counts[session.StatusFailed]; got != 3 {
		t.Errorf("buckets[0].Counts[failed] = %d, want 3 (summed across failure reasons)", got)
	}
	if got := buckets[0].Counts[session.StatusCancelled]; got != 0 {
		t.Errorf("buckets[0].Counts[cancelled] = %d, want 0 (absent, not a zero entry)", got)
	}

	if !buckets[1].Day.Equal(day2) {
		t.Errorf("buckets[1].Day = %v, want %v", buckets[1].Day, day2)
	}
	if got := buckets[1].Counts[session.StatusCancelled]; got != 3 {
		t.Errorf("buckets[1].Counts[cancelled] = %d, want 3", got)
	}
}

func TestSuccessRate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		counts         []platformanalytics.OutcomeCount
		wantRate       float64
		wantSampleSize int
		wantOK         bool
	}{
		{
			name:   "no data at all",
			counts: nil,
			wantOK: false,
		},
		{
			name: "only Created/Active sessions -- nothing resolved yet",
			counts: []platformanalytics.OutcomeCount{
				{Status: session.StatusCreated, Count: 5},
				{Status: session.StatusActive, Count: 2},
			},
			wantOK: false,
		},
		{
			name: "only Cancelled sessions -- excluded from both numerator and denominator",
			counts: []platformanalytics.OutcomeCount{
				{Status: session.StatusCancelled, FailureReason: session.FailureReasonCancelled, Count: 7},
			},
			wantOK: false,
		},
		{
			name: "mixed: completed, failed, cancelled, active all present",
			counts: []platformanalytics.OutcomeCount{
				{Status: session.StatusCompleted, Count: 18},
				{Status: session.StatusFailed, FailureReason: session.FailureReasonTimeout, Count: 2},
				{Status: session.StatusCancelled, FailureReason: session.FailureReasonCancelled, Count: 100}, // must not dilute the rate
				{Status: session.StatusActive, Count: 50},                                                    // must not dilute the rate
			},
			wantRate:       90,
			wantSampleSize: 20,
			wantOK:         true,
		},
		{
			name: "all failed -- a real, computed 0%, not a sentinel",
			counts: []platformanalytics.OutcomeCount{
				{Status: session.StatusFailed, FailureReason: session.FailureReasonFailed, Count: 4},
			},
			wantRate:       0,
			wantSampleSize: 4,
			wantOK:         true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rate, sampleSize, ok := platformanalytics.SuccessRate(tt.counts)
			if ok != tt.wantOK {
				t.Fatalf("SuccessRate(...) ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if rate != tt.wantRate {
				t.Errorf("SuccessRate(...) rate = %v, want %v", rate, tt.wantRate)
			}
			if sampleSize != tt.wantSampleSize {
				t.Errorf("SuccessRate(...) sampleSize = %d, want %d", sampleSize, tt.wantSampleSize)
			}
		})
	}
}

func TestTopFailureReasons_EmptyIsNotYetComputed(t *testing.T) {
	t.Parallel()

	reasons, ok := platformanalytics.TopFailureReasons(nil)
	if ok {
		t.Fatalf("TopFailureReasons(nil) ok = true, want false (not yet computed)")
	}
	if reasons != nil {
		t.Fatalf("TopFailureReasons(nil) reasons = %v, want nil", reasons)
	}
}

func TestTopFailureReasons_RealDataNoFailuresIsARealEmptyResult(t *testing.T) {
	t.Parallel()

	counts := []platformanalytics.OutcomeCount{
		{Status: session.StatusCompleted, Count: 12},
		{Status: session.StatusActive, Count: 3},
	}
	reasons, ok := platformanalytics.TopFailureReasons(counts)
	if !ok {
		t.Fatalf("TopFailureReasons(counts) ok = false, want true (real data exists, none of it failed)")
	}
	if len(reasons) != 0 {
		t.Fatalf("TopFailureReasons(counts) = %v, want empty", reasons)
	}
}

func TestTopFailureReasons_CountsOnlyFailedAndCancelledOrdersDeterministically(t *testing.T) {
	t.Parallel()

	counts := []platformanalytics.OutcomeCount{
		{Status: session.StatusCompleted, Count: 50}, // must never appear
		{Status: session.StatusFailed, FailureReason: session.FailureReasonTimeout, Count: 5},
		{Status: session.StatusFailed, FailureReason: session.FailureReasonFailed, Count: 5},
		{Status: session.StatusCancelled, FailureReason: session.FailureReasonCancelled, Count: 8},
		{Status: session.StatusFailed, FailureReason: session.FailureReasonNeverStarted, Count: 1},
		// Adversarial/corrupted-data row: a non-empty FailureReason on a
		// Completed status -- session_failure_reason carries no DB-level
		// CHECK constraint pairing it to status, so this row is a legal
		// value at the type level even though session.DeriveStatus never
		// produces it. The huge count (999) makes this test fail loudly
		// if the STATUS guard (not just the "FailureReason == ''" guard,
		// which this row's own non-empty reason does not trip) is ever
		// weakened or removed -- it must never inflate "timeout"'s count.
		{Status: session.StatusCompleted, FailureReason: session.FailureReasonTimeout, Count: 999},
	}

	reasons, ok := platformanalytics.TopFailureReasons(counts)
	if !ok {
		t.Fatalf("TopFailureReasons(counts) ok = false, want true")
	}

	// cancelled: 8, failed vs timeout tie at 5 (alphabetical: failed < timeout), never_started: 1.
	want := []platformanalytics.FailureReasonCount{
		{Reason: session.FailureReasonCancelled, Count: 8},
		{Reason: session.FailureReasonFailed, Count: 5},
		{Reason: session.FailureReasonTimeout, Count: 5},
		{Reason: session.FailureReasonNeverStarted, Count: 1},
	}
	if len(reasons) != len(want) {
		t.Fatalf("TopFailureReasons(counts) = %d reasons, want %d: %+v", len(reasons), len(want), reasons)
	}
	for i, w := range want {
		if reasons[i] != w {
			t.Errorf("reasons[%d] = %+v, want %+v", i, reasons[i], w)
		}
	}
}
