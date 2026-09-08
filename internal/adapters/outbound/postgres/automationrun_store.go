package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// AutomationRunStore is a thin, pass-through wrapper around the
// sqlc-generated automation_runs queries ("automations: engine",
// §3.5).
type AutomationRunStore struct {
	q *sqlcgen.Queries
}

// NewAutomationRunStore builds an AutomationRunStore backed by pool.
func NewAutomationRunStore(pool *pgxpool.Pool) *AutomationRunStore {
	return &AutomationRunStore{q: sqlcgen.New(pool)}
}

// WithTx returns an AutomationRunStore whose queries run on tx instead of
// the pool this store was built with -- Create MUST run in the same
// transaction as the run's own linked session's creation (mirrors
// internal/adapters/inbound/github's own coalescer, which calls
// httpapi.CreateSessionOnTx inline on its own already-open tx, exactly
// the same shape app/automation's own fanout.go reuses).
func (s *AutomationRunStore) WithTx(tx pgx.Tx) *AutomationRunStore {
	return &AutomationRunStore{q: s.q.WithTx(tx)}
}

// Create inserts a brand-new automation_runs row -- sessionID may be
// invalid/NULL (session/turn creation failed for this run's own target
// before any session ever existed; see CreateAutomationRun's own
// generated doc comment).
func (s *AutomationRunStore) Create(ctx context.Context, arg sqlcgen.CreateAutomationRunParams) (sqlcgen.AutomationRun, error) {
	return s.q.CreateAutomationRun(ctx, arg)
}

// CreateIfAbsent inserts a brand-new automation_runs row for arg's own
// (invocation_id, target), UNLESS a row for that exact pair already
// exists ("ON CONFLICT ... DO NOTHING", automation_runs_invocation_target_
// uniq, migrations/000054) -- pgx.ErrNoRows means one already does (see
// CreateAutomationRunIfAbsent's own generated doc comment for the
// ambiguous-commit hazard this backs).
func (s *AutomationRunStore) CreateIfAbsent(ctx context.Context, arg sqlcgen.CreateAutomationRunParams) (sqlcgen.AutomationRun, error) {
	return s.q.CreateAutomationRunIfAbsent(ctx, sqlcgen.CreateAutomationRunIfAbsentParams(arg))
}

// Get fetches the automation_runs row for id, or pgx.ErrNoRows if none
// exists.
func (s *AutomationRunStore) Get(ctx context.Context, id pgtype.UUID) (sqlcgen.AutomationRun, error) {
	return s.q.GetAutomationRun(ctx, id)
}

// ListInFlight returns up to limit runs still starting/running, oldest
// first -- a plain, unlocked read (see ListInFlightRuns' own generated doc
// comment for why no FOR UPDATE SKIP LOCKED is needed here).
func (s *AutomationRunStore) ListInFlight(ctx context.Context, limit int32) ([]sqlcgen.AutomationRun, error) {
	return s.q.ListInFlightRuns(ctx, limit)
}

// PromoteToRunning applies automation.RunTriggerProcessing: Starting ->
// Running. pgx.ErrNoRows means this run is no longer Starting (already
// promoted, or already terminal).
func (s *AutomationRunStore) PromoteToRunning(ctx context.Context, id pgtype.UUID) (sqlcgen.AutomationRun, error) {
	return s.q.PromoteAutomationRunToRunning(ctx, id)
}

// Terminalize applies internal/domain/automation.RunTransition's own
// verdict -- pgx.ErrNoRows means this run is already terminal (a
// concurrent closer, or a defensive re-run, e.g. the sweep and the
// reconcile pump racing on the SAME run).
func (s *AutomationRunStore) Terminalize(ctx context.Context, arg sqlcgen.TerminalizeAutomationRunParams) (sqlcgen.AutomationRun, error) {
	return s.q.TerminalizeAutomationRun(ctx, arg)
}

// CountTerminalForInvocation feeds automation.EvaluateInvocationOutcome:
// how many of invocationID's own runs are terminal, and how many of those
// failed.
func (s *AutomationRunStore) CountTerminalForInvocation(ctx context.Context, invocationID pgtype.UUID) (sqlcgen.CountTerminalRunsForInvocationRow, error) {
	return s.q.CountTerminalRunsForInvocation(ctx, invocationID)
}

// ListRunHealth returns the all-time succeeded/terminal run counts for
// every id in automationIDs that has at least one terminal run -- §12.2
// item 4's own health-ratio gap (see ListRunHealthForAutomations' own
// generated doc comment for the full "why per-request, why all-time").
// An id with no terminal runs simply does not appear in the result; the
// caller (httpapi's own automationToDTO) treats that as "no runs yet",
// never a fabricated 0/0.
func (s *AutomationRunStore) ListRunHealth(ctx context.Context, automationIDs []pgtype.UUID) ([]sqlcgen.ListRunHealthForAutomationsRow, error) {
	return s.q.ListRunHealthForAutomations(ctx, automationIDs)
}

// ListForInvocation returns every run of invocationID, oldest first --
// backs §8.4's own "artifact_summary populated" (app/automation's own
// closeout.go).
func (s *AutomationRunStore) ListForInvocation(ctx context.Context, invocationID pgtype.UUID) ([]sqlcgen.AutomationRun, error) {
	return s.q.ListRunsForInvocation(ctx, invocationID)
}

// ListOrphanedStarting returns up to limit runs still 'starting' whose own
// started_at predates cutoff -- §3.5's own "orphaned starting runs >5
// min" sweep (app/automation's own SweepOnce). cutoff is computed ONCE per
// tick by the caller (mirrors app/imagebuild.RefreshOnce's own
// staleClaimCutoff precedent), never a fresh now() per row.
func (s *AutomationRunStore) ListOrphanedStarting(ctx context.Context, cutoff pgtype.Timestamptz, limit int32) ([]sqlcgen.AutomationRun, error) {
	return s.q.ListOrphanedStartingRuns(ctx, sqlcgen.ListOrphanedStartingRunsParams{StartedAt: cutoff, Limit: limit})
}

// ListOrphanedRunning returns up to limit runs still 'running' whose own
// running_at predates cutoff -- §3.5's own "running >90 min" sweep, same
// shape as ListOrphanedStarting immediately above.
func (s *AutomationRunStore) ListOrphanedRunning(ctx context.Context, cutoff pgtype.Timestamptz, limit int32) ([]sqlcgen.AutomationRun, error) {
	return s.q.ListOrphanedRunningRuns(ctx, sqlcgen.ListOrphanedRunningRunsParams{RunningAt: cutoff, Limit: limit})
}
