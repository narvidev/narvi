package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// AutomationInvocationStore is a thin, pass-through wrapper around the
// sqlc-generated automation_invocations queries ("automations:
// engine", §3.5).
type AutomationInvocationStore struct {
	q *sqlcgen.Queries
}

// NewAutomationInvocationStore builds an AutomationInvocationStore backed
// by pool.
func NewAutomationInvocationStore(pool *pgxpool.Pool) *AutomationInvocationStore {
	return &AutomationInvocationStore{q: sqlcgen.New(pool)}
}

// WithTx returns an AutomationInvocationStore whose queries run on tx
// instead of the pool this store was built with -- ListDueForFanOut/
// ClaimForFanOut MUST run inside the same transaction (mirrors
// app/imagebuild.Builder.claimBatch's own claim-batch precedent exactly);
// MarkFailureCounted MUST run inside the same transaction as
// AutomationStore's own LockForUpdate/ApplyFailureStrike, when this
// invocation is closing failed (see this package's own AutomationStore),
// so the failure-strike CAS guard and its consequence commit or roll back
// together atomically (app/automation's own closeout.go, applyFailureStrike).
// CloseInvocation is deliberately NOT part of that same transaction --
// it still runs as its own standalone, pool-auto-committed statement, a
// known, accepted residual gap (internal/app/automation/doc.go's own
// "closeInvocation's own Close call is not part of that same transaction"
// section, mirroring app/imagebuild/doc.go's own analogous claim-crash-gap
// precedent), not this Step's own scope to close.
func (s *AutomationInvocationStore) WithTx(tx pgx.Tx) *AutomationInvocationStore {
	return &AutomationInvocationStore{q: s.q.WithTx(tx)}
}

// Create inserts a brand-new, 'pending' automation_invocations row --
// mirrors internal/app/releasereview.Enqueue's own "fast, cheap, durable
// hand-off" shape. Callers MUST have already validated targets via
// automation.ValidateTargets.
func (s *AutomationInvocationStore) Create(ctx context.Context, arg sqlcgen.CreateAutomationInvocationParams) (sqlcgen.AutomationInvocation, error) {
	return s.q.CreateAutomationInvocation(ctx, arg)
}

// Get fetches the automation_invocations row for id, or pgx.ErrNoRows if
// none exists.
func (s *AutomationInvocationStore) Get(ctx context.Context, id pgtype.UUID) (sqlcgen.AutomationInvocation, error) {
	return s.q.GetAutomationInvocation(ctx, id)
}

// CreateForDelivery is D1 audit fix's own idempotent-on-delivery variant
// of Create above -- see CreateAutomationInvocationForDelivery's own
// generated doc comment for the full "why" and the Inserted-branching
// contract every caller must honor.
func (s *AutomationInvocationStore) CreateForDelivery(ctx context.Context, arg sqlcgen.CreateAutomationInvocationForDeliveryParams) (sqlcgen.CreateAutomationInvocationForDeliveryRow, error) {
	return s.q.CreateAutomationInvocationForDelivery(ctx, arg)
}

// CountRecentInvocations backs D8's own per-automation dispatch throttle
// (domainautomation.EvaluateDispatchThrottle) -- every invocation
// automationID has created since since, any source, any outcome.
func (s *AutomationInvocationStore) CountRecentInvocations(ctx context.Context, automationID pgtype.UUID, since pgtype.Timestamptz) (int64, error) {
	return s.q.CountRecentAutomationInvocations(ctx, sqlcgen.CountRecentAutomationInvocationsParams{AutomationID: automationID, CreatedAt: since})
}

// ListDueForFanOut returns up to limit invocations not yet claimed for
// fan-out, locked FOR UPDATE SKIP LOCKED -- callers MUST run this inside
// the same transaction that subsequently calls ClaimForFanOut on each
// returned row.
func (s *AutomationInvocationStore) ListDueForFanOut(ctx context.Context, limit int32) ([]sqlcgen.AutomationInvocation, error) {
	return s.q.ListDueForFanOut(ctx, limit)
}

// ClaimForFanOut flips id's own fanned_out_at CAS guard -- pgx.ErrNoRows
// means a concurrent claimant already won it.
func (s *AutomationInvocationStore) ClaimForFanOut(ctx context.Context, id pgtype.UUID) (sqlcgen.AutomationInvocation, error) {
	return s.q.ClaimAutomationInvocationForFanOut(ctx, id)
}

// Close applies internal/domain/automation.InvocationTransition's own
// verdict -- pgx.ErrNoRows means this invocation is already closed (a
// concurrent closer won the race, or a defensive re-run).
func (s *AutomationInvocationStore) Close(ctx context.Context, arg sqlcgen.CloseAutomationInvocationParams) (sqlcgen.AutomationInvocation, error) {
	return s.q.CloseAutomationInvocation(ctx, arg)
}

// MarkFailureCounted flips id's own failure_counted_at CAS guard (§3.5's
// own literal idiom) -- pgx.ErrNoRows means this invocation's failure has
// already been counted once (a concurrent/retried closer lost the race).
func (s *AutomationInvocationStore) MarkFailureCounted(ctx context.Context, id pgtype.UUID) (sqlcgen.AutomationInvocation, error) {
	return s.q.MarkAutomationInvocationFailureCounted(ctx, id)
}

// ListForAutomation returns up to limit of automationID's own most recent
// invocations, newest first -- the automations UI's own addition (§12.2
// item 4, §8.4), backing GET /api/automations/{automationID}/invocations
// (internal/adapters/inbound/httpapi/automationinvocations.go).
func (s *AutomationInvocationStore) ListForAutomation(ctx context.Context, automationID pgtype.UUID, limit int32) ([]sqlcgen.AutomationInvocation, error) {
	return s.q.ListInvocationsForAutomation(ctx, sqlcgen.ListInvocationsForAutomationParams{AutomationID: automationID, Limit: limit})
}
