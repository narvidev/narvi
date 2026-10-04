package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// OwedReviewRequestStore is a thin, pass-through wrapper around the
// sqlc-generated owed_review_requests queries (migrations/000160,
// technical plan §24.9): the person's review requests a session owes its
// requesters after their attempt met a moved context. No caching, no
// retries, no business rules -- the session actor decides when a request
// is owed, re-run or dropped (sessionactor's reviewcontextcheck.go and
// owedreviewrequest.go), and is the only caller.
type OwedReviewRequestStore struct {
	q *sqlcgen.Queries
}

// NewOwedReviewRequestStore builds an OwedReviewRequestStore backed by
// pool.
func NewOwedReviewRequestStore(pool *pgxpool.Pool) *OwedReviewRequestStore {
	return &OwedReviewRequestStore{q: sqlcgen.New(pool)}
}

// WithTx returns an OwedReviewRequestStore whose queries run on tx instead
// of the pool this store was built with: every write the session actor
// makes runs in its own transaction, beside the turn writes it belongs
// with.
func (s *OwedReviewRequestStore) WithTx(tx pgx.Tx) *OwedReviewRequestStore {
	return &OwedReviewRequestStore{q: s.q.WithTx(tx)}
}

// Insert owes arg's request, created at the database's now. See
// InsertOwedReviewRequest's doc comment in queries/owedreviewrequests.sql.
func (s *OwedReviewRequestStore) Insert(ctx context.Context, arg sqlcgen.InsertOwedReviewRequestParams) (sqlcgen.OwedReviewRequest, error) {
	return s.q.InsertOwedReviewRequest(ctx, arg)
}

// Oldest returns the session's oldest owed request; pgx.ErrNoRows when
// nothing is owed.
func (s *OwedReviewRequestStore) Oldest(ctx context.Context, sessionID pgtype.UUID) (sqlcgen.OwedReviewRequest, error) {
	return s.q.GetOldestOwedReviewRequest(ctx, sessionID)
}

// Delete deletes the owed request id and returns it; pgx.ErrNoRows when it
// is already gone.
func (s *OwedReviewRequestStore) Delete(ctx context.Context, id pgtype.UUID) (sqlcgen.OwedReviewRequest, error) {
	return s.q.DeleteOwedReviewRequest(ctx, id)
}

// Exists reports whether the session is owed any request.
func (s *OwedReviewRequestStore) Exists(ctx context.Context, sessionID pgtype.UUID) (bool, error) {
	return s.q.ExistsOwedReviewRequest(ctx, sessionID)
}

// DeleteForStop deletes and returns every request the session owed at or
// before stopRequestedAt, the session's standing stop request. See
// DeleteOwedReviewRequestsForStop's doc comment in
// queries/owedreviewrequests.sql.
func (s *OwedReviewRequestStore) DeleteForStop(ctx context.Context, sessionID pgtype.UUID, stopRequestedAt pgtype.Timestamptz) ([]sqlcgen.OwedReviewRequest, error) {
	return s.q.DeleteOwedReviewRequestsForStop(ctx, sqlcgen.DeleteOwedReviewRequestsForStopParams{
		SessionID:       sessionID,
		StopRequestedAt: stopRequestedAt,
	})
}
