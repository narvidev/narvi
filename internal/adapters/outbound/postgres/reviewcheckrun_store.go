package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// ReviewCheckRunStore is a thin, pass-through wrapper around the
// sqlc-generated review_check_runs queries (§21.1b's own publisher
// identity) -- no caching, no retries, no business rules. The
// atomic-claim SEQUENCING (Ensure -> Lock -> decide via internal/domain/
// reviewcheck.Supersedes -> Update, all inside one transaction, released
// BEFORE the real GitHub call) and the post-call guarded external-id
// write live in internal/app/outboxworker's own review-check notifier,
// the one caller of every method below -- mirrors GitHubPRSessionStore's
// own identical "thin store, orchestration lives with the caller" split.
type ReviewCheckRunStore struct {
	q *sqlcgen.Queries
}

// NewReviewCheckRunStore builds a ReviewCheckRunStore backed by pool.
func NewReviewCheckRunStore(pool *pgxpool.Pool) *ReviewCheckRunStore {
	return &ReviewCheckRunStore{q: sqlcgen.New(pool)}
}

// WithTx returns a ReviewCheckRunStore whose queries run on tx instead of
// the pool this store was built with -- EnsureRow/LockForUpdate/
// UpdatePublished/ClearExternalID must ALL run in the SAME transaction
// for the atomic claim to be sound (migrations/
// 000132_review_check_runs.up.sql's own doc comment), so every real
// caller uses WithTx for that sequence, never the bare pool-backed store
// this constructor returns directly. SetExternalID/GetByRepoAndPRNumber
// are the two exceptions -- see their own doc comments below for why
// each deliberately runs OUTSIDE that transaction.
func (s *ReviewCheckRunStore) WithTx(tx pgx.Tx) *ReviewCheckRunStore {
	return &ReviewCheckRunStore{q: s.q.WithTx(tx)}
}

// EnsureRow idempotently ensures a (repoFullName, prNumber) claim row
// exists, with a placeholder empty head_sha/phase on a fresh insert --
// see EnsureReviewCheckRunRow's own generated doc comment.
func (s *ReviewCheckRunStore) EnsureRow(ctx context.Context, repoFullName string, prNumber int32) error {
	return s.q.EnsureReviewCheckRunRow(ctx, sqlcgen.EnsureReviewCheckRunRowParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
	})
}

// LockForUpdate locks the (repoFullName, prNumber) claim row for the rest
// of the caller's own transaction, returning its CURRENT state -- see
// LockReviewCheckRunForUpdate's own generated doc comment for the
// concurrency-serialization this provides.
func (s *ReviewCheckRunStore) LockForUpdate(ctx context.Context, repoFullName string, prNumber int32) (sqlcgen.ReviewCheckRun, error) {
	return s.q.LockReviewCheckRunForUpdate(ctx, sqlcgen.LockReviewCheckRunForUpdateParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
	})
}

// UpdatePublished records a winning emission's fields onto the
// (repoFullName, prNumber) row, still holding LockForUpdate's own row
// lock -- see UpdateReviewCheckRunPublished's own generated doc comment.
// attemptID.Valid == false persists a genuine SQL NULL (a PhaseQueued
// emission, which has no attempt yet).
func (s *ReviewCheckRunStore) UpdatePublished(ctx context.Context, repoFullName string, prNumber int32, headSHA string, attemptID pgtype.UUID, attemptCreatedAt pgtype.Timestamptz, phase string, baseRef, baseSHA *string, policyVersion int32) (sqlcgen.ReviewCheckRun, error) {
	return s.q.UpdateReviewCheckRunPublished(ctx, sqlcgen.UpdateReviewCheckRunPublishedParams{
		RepoFullName:     repoFullName,
		PrNumber:         prNumber,
		HeadSha:          headSHA,
		AttemptID:        attemptID,
		AttemptCreatedAt: attemptCreatedAt,
		Phase:            phase,
		BaseRef:          baseRef,
		BaseSha:          baseSHA,
		PolicyVersion:    policyVersion,
	})
}

// ClearExternalID nulls out external_id -- called under the SAME lock as
// UpdatePublished, exactly when the caller has determined head_sha is
// changing (see ClearReviewCheckRunExternalID's own generated doc
// comment for why the old id must never carry forward onto a new SHA).
func (s *ReviewCheckRunStore) ClearExternalID(ctx context.Context, repoFullName string, prNumber int32) error {
	return s.q.ClearReviewCheckRunExternalID(ctx, sqlcgen.ClearReviewCheckRunExternalIDParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
	})
}

// SetExternalID is the publisher's own post-GitHub-call guarded record
// write (SetReviewCheckRunExternalID's own generated doc comment) --
// deliberately called with NO transaction open (the real GitHub call this
// follows must never run inside one, ports.Notifier.Deliver's own
// contract), guarded on attemptIDGuard/external_id still being what the
// caller observed before making that call. pgx.ErrNoRows means the guard
// missed: a newer candidate reserved this row while the GitHub call was
// in flight -- see this method's own generated doc comment for the
// accepted residual this represents.
func (s *ReviewCheckRunStore) SetExternalID(ctx context.Context, repoFullName string, prNumber int32, externalID int64, attemptIDGuard pgtype.UUID) (sqlcgen.ReviewCheckRun, error) {
	return s.q.SetReviewCheckRunExternalID(ctx, sqlcgen.SetReviewCheckRunExternalIDParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
		ExternalID:   &externalID,
		AttemptID:    attemptIDGuard,
	})
}

// GetByRepoAndPRNumber is a plain, non-locking read -- see
// GetReviewCheckRunByRepoAndPRNumber's own generated doc comment.
// pgx.ErrNoRows means no row exists yet for this PR.
//
// Finding A10: this used to be documented as "the read a Deliver call
// opens with", describing a pre-read Deliver never actually performed
// (Deliver opens its own claim sequence with EnsureRow+LockForUpdate,
// both inside a transaction -- never a bare unlocked Get) -- and, before
// finding A3's own fix, had no production caller at all. It now has one:
// reviewCheckNotifier.guardAgainstSupersessionDuringCall (reviewcheck.go)
// calls this, with no transaction open, AFTER a GitHub write completes,
// to detect whether a concurrently-racing, newer Deliver call already
// committed a different row while that write was in flight.
func (s *ReviewCheckRunStore) GetByRepoAndPRNumber(ctx context.Context, repoFullName string, prNumber int32) (sqlcgen.ReviewCheckRun, error) {
	return s.q.GetReviewCheckRunByRepoAndPRNumber(ctx, sqlcgen.GetReviewCheckRunByRepoAndPRNumberParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
	})
}
