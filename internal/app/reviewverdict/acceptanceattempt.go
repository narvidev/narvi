package reviewverdict

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
)

// HasNewerReviewAttempt resolves the ONE fact reviewverdict.Acceptance.
// Applicable needs but cannot compute itself without I/O (§11: no I/O in
// /internal/domain) -- finding F1, adversarial review: "a new attempt"
// does not reduce to verdict-id equality, because an attempt that ends
// not_assessed posts no review_verdicts row at all. This function
// answers "has any review attempt newer than the one acceptance was
// granted against run since", directly against turns, regardless of
// whether that newer attempt ever posted a verdict.
//
// acceptance.AttemptID == "" (a pre-attempt-tracking acceptance/verdict --
// review_verdicts.attempt_id is nullable, migrations/000130) degrades to
// (false, nil): there is nothing to compare against, so this reduces to
// the SAME verdict-id-only check Applicable performed before this fix --
// an accepted, documented residual for data that predates attempt
// tracking, never a hole this fix reopens for current data (every
// CURRENT accept path, httpapi.AcceptReviewVerdict, always supplies a
// real attempt id when the accepted verdict's own Record.AttemptID is
// non-empty).
//
// turns == nil, or a genuine store error, degrades to (true, err) --
// deliberately fail CLOSED: the safe default here is the one that DENIES
// the waiver, never one that silently grants an acceptance this
// deployment cannot actually confirm is still fresh. A caller that
// treats a confirmable fact as security-relevant (internal/app/
// decisioninbox.revalidateCore) should check err first and refuse the
// whole action closed, mirroring this package's own "erroring out is the
// only honest answer" precedent for GetActiveAcceptance's own error
// path; a caller that only renders this as DISPLAY data (decisioninbox.
// buildPROpenItem) may instead log err and use the returned true as-is,
// since it is already the safe (never show a stale acceptance as active)
// answer either way.
func HasNewerReviewAttempt(ctx context.Context, turns *postgres.TurnStore, acceptance reviewverdict.Acceptance) (bool, error) {
	if acceptance.AttemptID == "" {
		return false, nil
	}
	if turns == nil {
		return true, nil
	}
	var attemptID pgtype.UUID
	if err := attemptID.Scan(acceptance.AttemptID); err != nil {
		return true, fmt.Errorf("reviewverdict: has newer review attempt: parse attempt id: %w", err)
	}
	acceptedTurn, err := turns.Get(ctx, attemptID)
	if err != nil {
		return true, fmt.Errorf("reviewverdict: has newer review attempt: get accepted attempt: %w", err)
	}
	hasNewer, err := turns.ExistsNewerReviewAttempt(ctx, acceptedTurn.SessionID, acceptedTurn.CreatedAt)
	if err != nil {
		return true, fmt.Errorf("reviewverdict: has newer review attempt: check newer review attempt: %w", err)
	}
	return hasNewer, nil
}
