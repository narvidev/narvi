package reviewverdict

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/domain/reviewverdict"
)

// GetLatest fetches (repoFullName, prNumber)'s own LATEST verdict --
// round-11 finding E: never "most-recently-posted", the PREVIOUS wording
// here, which round-10 finding D's own SQL fix (ReviewVerdictStore.
// GetLatest's own doc comment) already made inaccurate: the underlying
// query orders by the PRODUCING ATTEMPT's own creation time
// (COALESCE(turns.created_at, review_verdicts.created_at), tied-broken
// deterministically, round-11 finding B), never by post time alone --
// an older attempt's own post reaching the table after a newer attempt's
// must not win merely because its own INSERT committed later in
// wall-clock time. ok=false (never an error) means no verdict has ever
// been posted for this PR -- a legitimate, common outcome (e.g. a
// brand-new PR no review has run on yet) every real caller (the
// auto-approval eligibility engine's own callers, the decision inbox's
// classification) must treat as "not eligible" rather than propagating a
// store error.
func GetLatest(ctx context.Context, deps Deps, repoFullName string, prNumber int32) (record reviewverdict.Record, ok bool, err error) {
	row, err := deps.ReviewVerdicts.GetLatest(ctx, repoFullName, prNumber)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return reviewverdict.Record{}, false, nil
		}
		return reviewverdict.Record{}, false, err
	}
	return recordFromRow(row), true, nil
}

// GetLatestNonShadow is GetLatest's own §30.8 customer-consequential
// sibling -- excludes any verdict whose own suppressed_in_shadow stamp
// is true, or that predates repoFullName's own live_egress_promoted_at
// fence (GetLatestNonShadowReviewVerdict's own generated doc comment).
// ok=false means no NON-SHADOW verdict has ever been posted for this PR
// -- callers must treat this identically to GetLatest's own "no verdict
// at all" outcome (a shadow-era verdict is, from a customer-consequential
// caller's own point of view, indistinguishable from one that was never
// posted): internal/app/sessionactor/reviewretrigger.go's own
// auto-retrigger decision is this function's one caller, so a shadow-era
// "already reviewed" fact can never suppress a real re-review once a
// repo goes live, and a shadow-era risk level can never be quoted in a
// real, customer-visible budget-exhausted notice.
func GetLatestNonShadow(ctx context.Context, deps Deps, repoFullName string, prNumber int32) (record reviewverdict.Record, ok bool, err error) {
	row, err := deps.ReviewVerdicts.GetLatestNonShadow(ctx, repoFullName, prNumber)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return reviewverdict.Record{}, false, nil
		}
		return reviewverdict.Record{}, false, err
	}
	return recordFromRow(row), true, nil
}
