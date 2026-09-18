package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// ReviewVerdictStore is a thin, pass-through wrapper around the
// sqlc-generated review_verdicts queries (§21.1) -- see
// migrations/000067_review_verdicts.up.sql's own doc comment for the
// table's full append-only design. No caching, no retries, no business
// rules -- callers (internal/app/reviewverdict) decide what a missing
// row (pgx.ErrNoRows, unwrapped) means for their own purposes, mirroring
// every other Store in this package.
type ReviewVerdictStore struct {
	q *sqlcgen.Queries
}

// NewReviewVerdictStore builds a ReviewVerdictStore backed by pool.
func NewReviewVerdictStore(pool *pgxpool.Pool) *ReviewVerdictStore {
	return &ReviewVerdictStore{q: sqlcgen.New(pool)}
}

// WithTx returns a ReviewVerdictStore whose queries run on tx --
// httpapi.PostReviewVerdict's own caller uses this so the new
// review_verdicts row commits atomically alongside that handler's
// existing review_findings upserts and outbox write (all-or-nothing,
// mirroring that handler's own established transaction boundary).
func (s *ReviewVerdictStore) WithTx(tx pgx.Tx) *ReviewVerdictStore {
	return &ReviewVerdictStore{q: s.q.WithTx(tx)}
}

// Insert appends one review_verdicts row -- see InsertReviewVerdict's own
// generated doc comment. The ONE write this table ever accepts.
func (s *ReviewVerdictStore) Insert(ctx context.Context, arg sqlcgen.InsertReviewVerdictParams) (sqlcgen.ReviewVerdict, error) {
	return s.q.InsertReviewVerdict(ctx, arg)
}

// ExistsForAttempt reports whether ANY review_verdicts row has ever been
// posted for attemptID -- see ExistsReviewVerdictForAttempt's own
// generated doc comment. The review's own GitHub-native result surface's
// (§8.2/§21.1/§21.1b) one caller, sessionactor.outboxenqueue.go, uses
// this to decide whether a
// completing github-origin turn warrants a
// reviewcheck.PhaseTerminalNotAssessed emission.
func (s *ReviewVerdictStore) ExistsForAttempt(ctx context.Context, attemptID pgtype.UUID) (bool, error) {
	return s.q.ExistsReviewVerdictForAttempt(ctx, attemptID)
}

// GetLatest fetches (repoFullName, prNumber)'s own LATEST verdict --
// round-11 finding E: never "most-recently-posted" (the previous wording
// here); see GetLatestReviewVerdict's own generated doc comment for the
// actual ordering -- the PRODUCING ATTEMPT's own creation time, tie-
// broken deterministically (round-10 finding D, round-11 finding B),
// never post time alone. pgx.ErrNoRows (unwrapped) means no verdict has
// ever been posted for this PR -- callers (the auto-approval eligibility
// engine, the decision inbox's own classification) treat that
// identically to an ineligible/needs-review PR, never as an error.
func (s *ReviewVerdictStore) GetLatest(ctx context.Context, repoFullName string, prNumber int32) (sqlcgen.ReviewVerdict, error) {
	return s.q.GetLatestReviewVerdict(ctx, sqlcgen.GetLatestReviewVerdictParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
	})
}

// GetLatestNonShadow fetches (repoFullName, prNumber)'s own LATEST
// verdict (GetLatest's own doc comment immediately above -- the identical
// "never post time alone" correction applies here too), excluding any
// verdict §30.8 says must never arm a real, customer-visible effect (its
// own suppressed_in_shadow stamp, or one that predates repoFullName's own
// live_egress_promoted_at fence) -- see GetLatestNonShadowReviewVerdict's
// own generated doc comment. pgx.ErrNoRows (unwrapped) means no
// NON-SHADOW verdict has ever been posted for this PR -- callers must
// treat this identically to GetLatest's own "no verdict at all" outcome,
// never distinguish the two.
func (s *ReviewVerdictStore) GetLatestNonShadow(ctx context.Context, repoFullName string, prNumber int32) (sqlcgen.ReviewVerdict, error) {
	return s.q.GetLatestNonShadowReviewVerdict(ctx, sqlcgen.GetLatestNonShadowReviewVerdictParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
	})
}

// ListLatestAutoApproved returns repoFullName's own latest-per-PR
// verdicts, posted after sinceTime, whose Shippable is 'auto' -- bounded
// by limit. See ListLatestAutoApprovedInRepo's own generated doc comment
// for the DISCOVERY-only role this plays for internal/app/automerge.
func (s *ReviewVerdictStore) ListLatestAutoApproved(ctx context.Context, repoFullName string, sinceTime pgtype.Timestamptz, limit int32) ([]sqlcgen.ReviewVerdict, error) {
	return s.q.ListLatestAutoApprovedInRepo(ctx, sqlcgen.ListLatestAutoApprovedInRepoParams{
		RepoFullName: repoFullName,
		CreatedAt:    sinceTime,
		Limit:        limit,
	})
}

// ListInWindow returns every verdict posted for repoFullName after
// sinceTime, oldest first, bounded by limit -- the analytics rollups'
// own shared bounded scan (§21.1).
func (s *ReviewVerdictStore) ListInWindow(ctx context.Context, repoFullName string, sinceTime pgtype.Timestamptz, limit int32) ([]sqlcgen.ReviewVerdict, error) {
	return s.q.ListReviewVerdictsInWindow(ctx, sqlcgen.ListReviewVerdictsInWindowParams{
		RepoFullName: repoFullName,
		CreatedAt:    sinceTime,
		Limit:        limit,
	})
}

// ListNonShadowInWindow returns every NON-SHADOW verdict posted for
// repoFullName after sinceTime, oldest first, bounded by limit -- the
// digest rollup's own §30.8 exclusion (ListNonShadowReviewVerdictsInWindow's
// own generated doc comment): a shadow-era verdict must never reveal a
// phantom review in a customer-facing digest, even though the SAME
// history is deliberately left unfiltered for ListInWindow's own
// internal-analytics callers.
func (s *ReviewVerdictStore) ListNonShadowInWindow(ctx context.Context, repoFullName string, sinceTime pgtype.Timestamptz, limit int32) ([]sqlcgen.ReviewVerdict, error) {
	return s.q.ListNonShadowReviewVerdictsInWindow(ctx, sqlcgen.ListNonShadowReviewVerdictsInWindowParams{
		RepoFullName: repoFullName,
		CreatedAt:    sinceTime,
		Limit:        limit,
	})
}

// ListForPR returns every verdict ever posted for one (repoFullName,
// prNumber) pair, newest first, bounded by limit -- the merge readout's
// own "History" rail (§26.1 item 5, §12.2 item 2).
func (s *ReviewVerdictStore) ListForPR(ctx context.Context, repoFullName string, prNumber int32, limit int32) ([]sqlcgen.ReviewVerdict, error) {
	return s.q.ListReviewVerdictsForPR(ctx, sqlcgen.ListReviewVerdictsForPRParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
		Limit:        limit,
	})
}
