package reviewverdict

import (
	"context"
	"testing"

	"github.com/narvidev/narvi/internal/domain/reviewverdict"
)

// TestHasNewerReviewAttempt_DegenerateInputsFailClosed pins finding R10
// (round 3, adversarial review): every early-return path in
// HasNewerReviewAttempt must report the SAME "deny the waiver" default
// (hasNewer=true) -- a fail-OPEN outlier on any one of them silently lets
// a stale acceptance keep applying forever. Table-driven over the two
// no-I/O paths (acceptance.AttemptID == "", and turns == nil) --
// deliberately never exercises the store-backed path (a genuine
// Get/ExistsNewerReviewAttempt error), which needs a real Postgres
// *postgres.TurnStore and is instead covered by this package's own
// integration suite (internal/app/decisioninbox's acceptance_
// integration_test.go, revalidate_integration_test.go).
//
// CORRECTED (finding R10): before this fix, acceptance.AttemptID == ""
// was the ONE outlier -- it returned (false, nil), "no newer attempt",
// exactly backwards from every other path here. Reachable population,
// per two verifiers' own analysis: a review_verdicts row written before
// the migration that added attempt_id (migrations/000130), never
// backfilled -- an acceptance bound to such a verdict carried a
// permanent, silent immunity to attempt-based invalidation.
func TestHasNewerReviewAttempt_DegenerateInputsFailClosed(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name       string
		acceptance reviewverdict.Acceptance
	}{
		{
			name:       "AttemptID empty -- a pre-attempt-tracking acceptance/verdict",
			acceptance: reviewverdict.Acceptance{AttemptID: ""},
		},
		{
			name:       "AttemptID set, but no TurnStore configured",
			acceptance: reviewverdict.Acceptance{AttemptID: "11111111-1111-1111-1111-111111111111"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hasNewer, err := HasNewerReviewAttempt(ctx, nil, tc.acceptance)
			if err != nil {
				t.Fatalf("HasNewerReviewAttempt() error = %v, want nil", err)
			}
			if !hasNewer {
				t.Errorf("HasNewerReviewAttempt() = false, want true -- every degenerate/unconfirmable input must fail CLOSED (deny the waiver), never silently grant one this deployment cannot actually confirm is still fresh")
			}
		})
	}
}
