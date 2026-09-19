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
// acceptance.AttemptID == "" (round 3, finding R10, adversarial review,
// CORRECTED: a previous version of this function returned (false, nil)
// here -- "no newer attempt", the exact opposite of every OTHER path
// below, which all fail CLOSED) now degrades to (true, err=nil) instead,
// mirroring turns == nil/a parse failure/a store error immediately below.
// Two verifiers confirmed this degenerate input is unreachable through
// any CURRENT writer: the sole production writer of review_verdicts
// always supplies the dispatched turn's own id, the sole creator of
// acceptances (httpapi.AcceptReviewVerdict) copies it straight through,
// and turns/sessions are never deleted anywhere in this codebase, so the
// attempt_id column's own ON DELETE SET NULL route is dead. But their own
// analysis conceded the one population that DOES reach this branch: a
// review_verdicts row written before the migration that added attempt_id
// (migrations/000130), which was never backfilled -- an acceptance bound
// to such a verdict carries AttemptID == "" and, under the PREVIOUS
// fail-OPEN default here, became permanently immune to attempt-based
// invalidation (the exact defect this whole mechanism exists to close,
// §21.1b: "a new attempt... makes it inapplicable"). The residual this
// fix accepts instead: such an acceptance can never actually apply
// (Applicable's own final `!hasNewerAttempt` always false) -- a verdict
// with no attempt id on record must be re-reviewed before its refusal can
// be accepted at all, never silently waived forever.
//
// turns == nil, or a genuine store error, ALSO degrades to (true, err) --
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
		return true, nil
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
