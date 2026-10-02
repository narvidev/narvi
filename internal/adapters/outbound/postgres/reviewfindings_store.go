package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// ReviewFindingStore is a thin, pass-through wrapper around the
// sqlc-generated review_findings queries ("sentinels +
// suggestions", §17/§22.1) -- see migrations/000046_review_findings.up.sql's
// own doc comment for the table's full design. No caching, no retries, no
// business rules -- callers (internal/adapters/inbound/httpapi/
// reviewverdict.go, reviewfindings.go, internal/app/reviewcontext) decide
// what a missing row means for their own purposes.
type ReviewFindingStore struct {
	q *sqlcgen.Queries
}

// NewReviewFindingStore builds a ReviewFindingStore backed by pool.
func NewReviewFindingStore(pool *pgxpool.Pool) *ReviewFindingStore {
	return &ReviewFindingStore{q: sqlcgen.New(pool)}
}

// WithTx returns a ReviewFindingStore whose queries run on tx instead of
// the pool this store was built with -- mirrors every other store's own
// identical WithTx convention. reviewverdict.go's own upsert-findings-
// plus-enqueue-outbox write is this store's own first WithTx caller,
// exactly the "written in the same transaction as the state change"
// discipline §5.1/§21.1 already require of every other multi-write
// sequence in this codebase.
func (s *ReviewFindingStore) WithTx(tx pgx.Tx) *ReviewFindingStore {
	return &ReviewFindingStore{q: s.q.WithTx(tx)}
}

// Upsert creates-or-updates one review_findings row for
// (repoFullName, prNumber, identityHash) -- see UpsertReviewFinding's own
// generated doc comment for exactly which columns are preserved vs.
// overwritten on a re-report of the same identity.
func (s *ReviewFindingStore) Upsert(ctx context.Context, arg sqlcgen.UpsertReviewFindingParams) (sqlcgen.ReviewFinding, error) {
	return s.q.UpsertReviewFinding(ctx, arg)
}

// Get fetches one finding by its own (repoFullName, prNumber, identityHash)
// key -- pgx.ErrNoRows (unwrapped) means no such finding was ever posted.
func (s *ReviewFindingStore) Get(ctx context.Context, repoFullName string, prNumber int32, identityHash string) (sqlcgen.ReviewFinding, error) {
	return s.q.GetReviewFinding(ctx, sqlcgen.GetReviewFindingParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
		IdentityHash: identityHash,
	})
}

// ListOpenAndRebutted returns every 'open'/'rebutted' finding for one PR,
// oldest-first -- internal/app/reviewcontext's own re-review reconciliation
// read (§22.1).
func (s *ReviewFindingStore) ListOpenAndRebutted(ctx context.Context, repoFullName string, prNumber int32) ([]sqlcgen.ReviewFinding, error) {
	return s.q.ListOpenAndRebuttedReviewFindings(ctx, sqlcgen.ListOpenAndRebuttedReviewFindingsParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
	})
}

// ListAllForPR returns EVERY finding ever posted for one PR, regardless of
// status, oldest-first -- the merge readout's own collapsed appendix
// (§26.1 item 5, §12.2 item 2), unlike ListOpenAndRebutted's own narrower
// re-review-reconciliation scope above.
func (s *ReviewFindingStore) ListAllForPR(ctx context.Context, repoFullName string, prNumber int32) ([]sqlcgen.ReviewFinding, error) {
	return s.q.ListAllReviewFindingsForPR(ctx, sqlcgen.ListAllReviewFindingsForPRParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
	})
}

// MarkRebutted records a maintainer+'s explicit dismissal (§22.1) --
// pgx.ErrNoRows (unwrapped) means no finding with this identity hash
// exists on this PR at all.
func (s *ReviewFindingStore) MarkRebutted(ctx context.Context, repoFullName string, prNumber int32, identityHash, rebuttalText string, rebuttedBy pgtype.UUID) (sqlcgen.ReviewFinding, error) {
	return s.q.MarkReviewFindingRebutted(ctx, sqlcgen.MarkReviewFindingRebuttedParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
		IdentityHash: identityHash,
		RebuttalText: &rebuttalText,
		RebuttedBy:   rebuttedBy,
	})
}

// MarkFixPending records that a sentinel-auto-fix child session has been
// spawned to address this finding (§17.2) -- suppresses the manual
// apply-suggestion action for it (§17.3). Guarded (status IN ('open',
// 'fix_pending'), see MarkReviewFindingFixPending's own generated doc
// comment) -- pgx.ErrNoRows (unwrapped) now means EITHER "no such finding"
// OR "this finding already progressed past fix_pending", both a benign,
// safe-to-ignore no-op for the caller. This guard is what makes a retried
// call with the SAME fixChildSessionID safe (internal/app/outboxworker's
// own sentinelAutoFixNotifier.Deliver relies on it).
func (s *ReviewFindingStore) MarkFixPending(ctx context.Context, repoFullName string, prNumber int32, identityHash string, fixChildSessionID pgtype.UUID) (sqlcgen.ReviewFinding, error) {
	return s.q.MarkReviewFindingFixPending(ctx, sqlcgen.MarkReviewFindingFixPendingParams{
		RepoFullName:      repoFullName,
		PrNumber:          prNumber,
		IdentityHash:      identityHash,
		FixChildSessionID: fixChildSessionID,
	})
}

// MarkFixOpen records that the fix session's own fix PR has been opened.
func (s *ReviewFindingStore) MarkFixOpen(ctx context.Context, fixChildSessionID pgtype.UUID, fixPRNumber int32) (sqlcgen.ReviewFinding, error) {
	return s.q.MarkReviewFindingFixOpen(ctx, sqlcgen.MarkReviewFindingFixOpenParams{
		FixChildSessionID: fixChildSessionID,
		FixPrNumber:       &fixPRNumber,
	})
}

// MarkFixApplied records that the manual apply-suggestion endpoint (§12.2
// item 2) has directly committed this finding's own SuggestedFix.
func (s *ReviewFindingStore) MarkFixApplied(ctx context.Context, repoFullName string, prNumber int32, identityHash string) (sqlcgen.ReviewFinding, error) {
	return s.q.MarkReviewFindingFixApplied(ctx, sqlcgen.MarkReviewFindingFixAppliedParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
		IdentityHash: identityHash,
	})
}

// MarkFixRecorded records that the manual apply-suggestion endpoint
// (§12.2 item 2) attempted to commit this finding's own SuggestedFix but
// the repository's outgoing changes are currently suppressed (platform
// shadow mode, §30.7/§30.9): nothing reached the real repository, so this
// is deliberately NOT MarkFixApplied -- see FindingStatusFixRecorded's own
// doc comment for why the distinction matters to re-review reconciliation.
func (s *ReviewFindingStore) MarkFixRecorded(ctx context.Context, repoFullName string, prNumber int32, identityHash string) (sqlcgen.ReviewFinding, error) {
	return s.q.MarkReviewFindingFixRecorded(ctx, sqlcgen.MarkReviewFindingFixRecordedParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
		IdentityHash: identityHash,
	})
}

// ListStatusesInWindow returns the status, reported source and addition
// check of every finding first seen for repoFullName after sinceTime,
// oldest-first, bounded by limit -- §21's own "Review finding outcomes"
// analytics KPI (§21.1), read per source since §26.6's amendment.
func (s *ReviewFindingStore) ListStatusesInWindow(ctx context.Context, repoFullName string, sinceTime pgtype.Timestamptz, limit int32) ([]sqlcgen.ListReviewFindingStatusesInWindowRow, error) {
	return s.q.ListReviewFindingStatusesInWindow(ctx, sqlcgen.ListReviewFindingStatusesInWindowParams{
		RepoFullName: repoFullName,
		FirstSeenAt:  sinceTime,
		Limit:        limit,
	})
}

// MarkFixMergedByFixSession transitions every finding a given fix session
// was addressing to 'fix_merged' (§17.4's own merge-gating terminal
// write).
func (s *ReviewFindingStore) MarkFixMergedByFixSession(ctx context.Context, fixChildSessionID pgtype.UUID) ([]sqlcgen.ReviewFinding, error) {
	return s.q.MarkReviewFindingsFixMergedByFixSession(ctx, fixChildSessionID)
}
