package reviewverdict

import "testing"

// TestDeriveReviewStatus_Table pins every ReviewState, which verdict each
// one reports, and that assessed is never reported without a verdict.
func TestDeriveReviewStatus_Table(t *testing.T) {
	older := &Record{ID: "v-old", AttemptID: "attempt-1"}
	legacy := &Record{ID: "v-legacy"}
	newestOwn := &Record{ID: "v-new", AttemptID: "attempt-2"}

	tests := []struct {
		name           string
		latest         *Record
		newest         *Attempt
		wantState      ReviewState
		wantVerdict    *Record
		wantSuperseded *Record
	}{
		{name: "nothing on record: absent", wantState: ReviewAbsent},
		{name: "a verdict with no attempt on record: assessed", latest: legacy, wantState: ReviewAssessed, wantVerdict: legacy},
		{name: "first attempt running: in progress", newest: &Attempt{ID: "attempt-1"}, wantState: ReviewInProgress},
		{name: "a newer attempt running over an older verdict: in progress, older superseded",
			latest: older, newest: &Attempt{ID: "attempt-2"}, wantState: ReviewInProgress, wantSuperseded: older},
		{name: "a running attempt that already posted: in progress, nothing reported yet",
			latest: newestOwn, newest: &Attempt{ID: "attempt-2", Posted: true}, wantState: ReviewInProgress},
		{name: "a newer attempt running over a legacy verdict: legacy superseded",
			latest: legacy, newest: &Attempt{ID: "attempt-2"}, wantState: ReviewInProgress, wantSuperseded: legacy},
		{name: "first attempt ended without posting: not assessed",
			newest: &Attempt{ID: "attempt-1", Terminal: true}, wantState: ReviewNotAssessed},
		{name: "newest attempt ended without posting over an older verdict: not assessed, older superseded",
			latest: older, newest: &Attempt{ID: "attempt-2", Terminal: true}, wantState: ReviewNotAssessed, wantSuperseded: older},
		{name: "newest attempt ended and posted: assessed on it",
			latest: newestOwn, newest: &Attempt{ID: "attempt-2", Terminal: true, Posted: true}, wantState: ReviewAssessed, wantVerdict: newestOwn},
		{name: "posted but no verdict readable: never assessed without one",
			newest: &Attempt{ID: "attempt-2", Terminal: true, Posted: true}, wantState: ReviewNotAssessed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveReviewStatus(tc.latest, tc.newest)
			if got.State != tc.wantState || got.Verdict != tc.wantVerdict || got.Superseded != tc.wantSuperseded {
				t.Errorf("DeriveReviewStatus = %+v, want state %q verdict %v superseded %v", got, tc.wantState, tc.wantVerdict, tc.wantSuperseded)
			}
			if got.State == ReviewAssessed && got.Verdict == nil {
				t.Error("assessed with no verdict")
			}
			if got.State != ReviewAssessed && got.Verdict != nil {
				t.Errorf("%q reports a verdict", got.State)
			}
		})
	}
}
