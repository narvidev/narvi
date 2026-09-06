package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// PlanDocumentStore is a thin, pass-through wrapper around the
// sqlc-generated plan_documents queries (§31.3's durability fix for an
// approved plan's own prose, migrations/000112_plan_documents.up.sql). No
// caching, no retries, no business rules -- httpapi.DecidePlanOnTx
// (decideplan.go) is this store's only writer, and it is ALWAYS called
// WithTx, in the SAME transaction as plans' own guarded approve UPDATE:
// a plan_documents row and its plan's 'approved' status commit together
// or not at all (see this migration's own comment for why the table
// exists at all).
type PlanDocumentStore struct {
	q *sqlcgen.Queries
}

// NewPlanDocumentStore builds a PlanDocumentStore backed by pool.
func NewPlanDocumentStore(pool *pgxpool.Pool) *PlanDocumentStore {
	return &PlanDocumentStore{q: sqlcgen.New(pool)}
}

// WithTx returns a PlanDocumentStore whose queries run on tx instead of
// the pool this store was built with -- see this type's own doc comment
// for why every real caller uses this, never the pool directly.
func (s *PlanDocumentStore) WithTx(tx pgx.Tx) *PlanDocumentStore {
	return &PlanDocumentStore{q: s.q.WithTx(tx)}
}

// Create snapshots content as planID's own durable plan document. planID
// must name a real plans row (the FK) not already snapshotted (the
// UNIQUE constraint) -- a violation of either surfaces here as a plain
// Postgres error, never swallowed.
func (s *PlanDocumentStore) Create(ctx context.Context, planID pgtype.UUID, content string) (sqlcgen.PlanDocument, error) {
	return s.q.CreatePlanDocument(ctx, sqlcgen.CreatePlanDocumentParams{
		PlanID:  planID,
		Content: &content,
	})
}

// GetByPlanID fetches planID's own snapshot, or pgx.ErrNoRows (unwrapped)
// if none exists -- this Step's own coverage-measurement read: every
// approved plan must have exactly one row here.
func (s *PlanDocumentStore) GetByPlanID(ctx context.Context, planID pgtype.UUID) (sqlcgen.PlanDocument, error) {
	return s.q.GetPlanDocumentByPlanID(ctx, planID)
}
