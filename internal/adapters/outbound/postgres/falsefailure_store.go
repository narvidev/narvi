package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// FalseFailureStore is a thin, pass-through wrapper around the
// sqlc-generated false_failures queries (migrations/
// 000124_false_failures.up.sql) -- see that migration's own doc comment
// for the table's full "durable sibling of turn_false_failure_total"
// design. No caching, no retries, no business rules -- the ONE call site
// (internal/app/sessionactor's recordFalseFailureIfApplicable) decides
// what it means, mirroring every other Store in this package.
type FalseFailureStore struct {
	q *sqlcgen.Queries
}

// NewFalseFailureStore builds a FalseFailureStore backed by pool.
func NewFalseFailureStore(pool *pgxpool.Pool) *FalseFailureStore {
	return &FalseFailureStore{q: sqlcgen.New(pool)}
}

// WithTx returns a FalseFailureStore whose queries run on tx --
// recordFalseFailureIfApplicable's own caller (completeProcessingTurn,
// pushpr.go) needs the new row to commit atomically alongside the rest
// of that transact.
func (s *FalseFailureStore) WithTx(tx pgx.Tx) *FalseFailureStore {
	return &FalseFailureStore{q: s.q.WithTx(tx)}
}

// Insert appends one false_failures row -- see InsertFalseFailure's own
// generated doc comment.
func (s *FalseFailureStore) Insert(ctx context.Context, sessionID pgtype.UUID) (sqlcgen.FalseFailure, error) {
	return s.q.InsertFalseFailure(ctx, sessionID)
}

// CountInWindow counts every false-failure incident detected since
// sinceTime -- §12.2 item 6's own "False failures" KPI tile.
func (s *FalseFailureStore) CountInWindow(ctx context.Context, sinceTime pgtype.Timestamptz) (int64, error) {
	return s.q.CountFalseFailuresInWindow(ctx, sinceTime)
}
