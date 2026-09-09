package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
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

// Create snapshots content -- and, when structured is non-nil,
// migrations/000126_plan_documents_structured.up.sql's own
// structured_steps column -- as planID's own durable plan document.
// planID must name a real plans row (the FK) not already snapshotted (the
// UNIQUE constraint) -- a violation of either surfaces here as a plain
// Postgres error, never swallowed.
//
// structured is marshaled to JSON here (the one place a plandomain.
// Structured value crosses into a raw column write) via Step/Structured's
// own json tags -- the SAME field names (title/description/fileRefs/
// steps/scopeEstimate) ExtractStructured's own wireStep/wireStructured
// decode from, so a future reader of this column sees the identical shape
// the model was asked to emit, not a second, independently-cased mapping.
// A nil structured writes SQL NULL (pgx encodes a nil []byte as NULL for a
// jsonb column) -- the only representation of "no structure recovered",
// matching that migration's own doc comment.
func (s *PlanDocumentStore) Create(ctx context.Context, planID pgtype.UUID, content string, structured *plandomain.Structured) (sqlcgen.PlanDocument, error) {
	var structuredJSON []byte
	if structured != nil {
		var err error
		structuredJSON, err = json.Marshal(structured)
		if err != nil {
			return sqlcgen.PlanDocument{}, fmt.Errorf("postgres: marshal structured plan document: %w", err)
		}
	}
	return s.q.CreatePlanDocument(ctx, sqlcgen.CreatePlanDocumentParams{
		PlanID:          planID,
		Content:         &content,
		StructuredSteps: structuredJSON,
	})
}

// GetByPlanID fetches planID's own snapshot, or pgx.ErrNoRows (unwrapped)
// if none exists -- this Step's own coverage-measurement read: every
// approved plan must have exactly one row here.
func (s *PlanDocumentStore) GetByPlanID(ctx context.Context, planID pgtype.UUID) (sqlcgen.PlanDocument, error) {
	return s.q.GetPlanDocumentByPlanID(ctx, planID)
}
