package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// SandboxStore is a thin, pass-through wrapper around the sqlc-generated
// sandbox queries (§4.3 SandboxStore). No caching, no retries, no business
// rules — that lives in domain/sandbox (§3.2) and app/sessionactor
// (§2).
type SandboxStore struct {
	q *sqlcgen.Queries
}

// NewSandboxStore builds a SandboxStore backed by pool.
func NewSandboxStore(pool *pgxpool.Pool) *SandboxStore {
	return &SandboxStore{q: sqlcgen.New(pool)}
}

// WithTx returns a SandboxStore whose queries run on tx instead of the
// pool this store was built with — used by app/sessionactor's
// transactional-write helper (§2).
func (s *SandboxStore) WithTx(tx pgx.Tx) *SandboxStore {
	return &SandboxStore{q: s.q.WithTx(tx)}
}

// Create inserts a new sandbox row for sessionID and returns it. The
// database enforces one sandbox row per session via UNIQUE(session_id)
// (§3.2).
func (s *SandboxStore) Create(ctx context.Context, sessionID pgtype.UUID) (sqlcgen.Sandbox, error) {
	return s.q.CreateSandbox(ctx, sessionID)
}

// Get fetches the sandbox row for sessionID.
func (s *SandboxStore) Get(ctx context.Context, sessionID pgtype.UUID) (sqlcgen.Sandbox, error) {
	return s.q.GetSandbox(ctx, sessionID)
}

// UpdateStatus sets a sandbox's status, plus last_seen_at when the caller
// supplies a real timestamp (see UpdateSandboxStatusParams' generated doc
// for the COALESCE semantics).
func (s *SandboxStore) UpdateStatus(ctx context.Context, arg sqlcgen.UpdateSandboxStatusParams) (sqlcgen.Sandbox, error) {
	return s.q.UpdateSandboxStatus(ctx, arg)
}

// UpdateStatusToSuspect moves a sandbox into Suspect and persists the live
// status being left as pre_suspect_status, in the same statement --
// §3.2 ("two-phase terminalization"), see UpdateSandboxStatusToSuspect's own
// generated doc comment.
func (s *SandboxStore) UpdateStatusToSuspect(ctx context.Context, arg sqlcgen.UpdateSandboxStatusToSuspectParams) (sqlcgen.Sandbox, error) {
	return s.q.UpdateSandboxStatusToSuspect(ctx, arg)
}

// RecoverFromSuspect returns a Suspect sandbox to a previously-live state,
// clearing pre_suspect_status back to NULL and bumping last_seen_at in the
// same statement -- §3.2 ("two-phase terminalization"), see
// RecoverSandboxFromSuspect's own generated doc comment.
func (s *SandboxStore) RecoverFromSuspect(ctx context.Context, arg sqlcgen.RecoverSandboxFromSuspectParams) (sqlcgen.Sandbox, error) {
	return s.q.RecoverSandboxFromSuspect(ctx, arg)
}

// UpsertForSpawn creates the sandbox row (if none exists) or bumps its gen
// and resets it to spawning (if one already does) -- see
// UpsertSandboxForSpawnParams' generated doc comment (§9.3, design
// decision 3a).
func (s *SandboxStore) UpsertForSpawn(ctx context.Context, arg sqlcgen.UpsertSandboxForSpawnParams) (sqlcgen.Sandbox, error) {
	return s.q.UpsertSandboxForSpawn(ctx, arg)
}

// UpdateProviderID records the provider's own opaque handle once
// CreateSandbox succeeds.
func (s *SandboxStore) UpdateProviderID(ctx context.Context, arg sqlcgen.UpdateSandboxProviderIDParams) (sqlcgen.Sandbox, error) {
	return s.q.UpdateSandboxProviderID(ctx, arg)
}

// UpdateCircuitBreaker persists internal/domain/sandbox.CircuitBreakerState.
func (s *SandboxStore) UpdateCircuitBreaker(ctx context.Context, arg sqlcgen.UpdateSandboxCircuitBreakerParams) (sqlcgen.Sandbox, error) {
	return s.q.UpdateSandboxCircuitBreaker(ctx, arg)
}

// UpdateSnapshotID records a real, sandbox-confirmed snapshot id once a
// "snapshot_ready" wire event arrives (§3.2, "snapshots & restore",
// design decision 3). Also clears pending_snapshot_message_id back to
// NULL in the same statement -- see UpdateSandboxSnapshotID's own
// generated doc comment.
func (s *SandboxStore) UpdateSnapshotID(ctx context.Context, arg sqlcgen.UpdateSandboxSnapshotIDParams) (sqlcgen.Sandbox, error) {
	return s.q.UpdateSandboxSnapshotID(ctx, arg)
}

// ListLiveProviderIDs returns the provider_id of every sandboxes row
// currently in a LIVE status, across ALL sessions -- §5.3 ("reconciler
// + GC", §5.3), app/reconciler's own "expected still alive" set. Unlike
// every other SandboxStore method (each scoped to one session_id), this is
// the one deliberately cross-session query the reconciler needs -- see
// ListLiveSandboxProviderIDs's own generated doc comment for exactly which
// statuses are included/excluded and why.
//
// sqlc types provider_id as *string (it's a nullable column in general),
// but this query's own WHERE clause already guarantees every row it
// returns has one -- sqlc's postgresql engine does not narrow a column's
// generated Go type from a query's own IS NOT NULL predicate, so this
// method dereferences here rather than exposing that always-non-nil
// pointer to callers.
func (s *SandboxStore) ListLiveProviderIDs(ctx context.Context) ([]string, error) {
	rows, err := s.q.ListLiveSandboxProviderIDs(ctx)
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(rows))
	for _, id := range rows {
		if id == nil {
			// Unreachable given the query's own "AND provider_id IS NOT
			// NULL" -- guarded rather than silently skipped so a future
			// change to the query that drops that guard fails loudly
			// here instead of quietly starving the reconciler's own
			// expected-alive set of a real row.
			return nil, fmt.Errorf("postgres: ListLiveSandboxProviderIDs returned a nil provider_id despite its own IS NOT NULL filter")
		}
		ids = append(ids, *id)
	}
	return ids, nil
}

// UpdatePendingSnapshotMessageID sets (or clears, via nil) the MessageId
// of whichever Snapshot command this sandbox is currently waiting on a
// snapshot_ready for -- §3.2 fix (message-id correlation), closing a
// real ambiguous-write race an independent review confirmed against a
// real Postgres instance.
func (s *SandboxStore) UpdatePendingSnapshotMessageID(ctx context.Context, arg sqlcgen.UpdateSandboxPendingSnapshotMessageIDParams) (sqlcgen.Sandbox, error) {
	return s.q.UpdateSandboxPendingSnapshotMessageID(ctx, arg)
}

// SetPendingPush stamps this sandbox's own persisted push/PR egress-mode
// decision (§30.8's "resolved at push send, persisted with the signal") --
// called by completeProcessingTurn (app/sessionactor/pushpr.go) in the
// same transact that completes the turn and builds its pushSignal. Also
// resets pending_push_cancelled to false (a fresh cycle), per
// SetSandboxPendingPush's own generated doc comment.
func (s *SandboxStore) SetPendingPush(ctx context.Context, arg sqlcgen.SetSandboxPendingPushParams) (sqlcgen.Sandbox, error) {
	return s.q.SetSandboxPendingPush(ctx, arg)
}

// ClearPendingPush consumes this sandbox's own persisted push/PR
// decision, once createPRBestEffort has read and acted on it -- see
// ClearSandboxPendingPush's own generated doc comment.
func (s *SandboxStore) ClearPendingPush(ctx context.Context, sessionID pgtype.UUID) (sqlcgen.Sandbox, error) {
	return s.q.ClearSandboxPendingPush(ctx, sessionID)
}

// CancelPendingPush marks a sandbox's own currently-outstanding push
// decision cancelled -- §30.4's own "demotion ... must cancel in-flight
// push signals", called by the repo-demotion sweep (internal/app/seed).
// Returns pgx.ErrNoRows (unwrapped) when this sandbox has no push
// currently outstanding -- see CancelSandboxPendingPush's own generated
// doc comment; callers treat that as "nothing to cancel", not an error.
func (s *SandboxStore) CancelPendingPush(ctx context.Context, sessionID pgtype.UUID) (sqlcgen.Sandbox, error) {
	return s.q.CancelSandboxPendingPush(ctx, sessionID)
}

// ListLiveWithSessionRepos returns every live sandbox alongside its
// owning session's own raw repos JSONB -- the repo-demotion sweep's
// (internal/app/seed) own input; see ListLiveSandboxesWithSessionRepos's
// own generated doc comment.
func (s *SandboxStore) ListLiveWithSessionRepos(ctx context.Context) ([]sqlcgen.ListLiveSandboxesWithSessionReposRow, error) {
	return s.q.ListLiveSandboxesWithSessionRepos(ctx)
}

// MarkDemotionTerminationRequested flags a sandbox for the process-wide
// reconciler (internal/app/reconciler) to really terminate -- §30.4's own
// "demotion ... must terminate (or respawn) every sandbox of the repo",
// called by the repo-demotion sweep (internal/app/seed) for every live
// sandbox it finds belonging to a just-demoted repo.
func (s *SandboxStore) MarkDemotionTerminationRequested(ctx context.Context, sessionID pgtype.UUID) (sqlcgen.Sandbox, error) {
	return s.q.MarkSandboxDemotionTerminationRequested(ctx, sessionID)
}

// ListPendingDemotionTermination returns every sandbox a repo-demotion
// sweep has flagged for termination -- app/reconciler.Reconciler's own
// new demotion-sweep tick reads this every ReconcilerInterval.
func (s *SandboxStore) ListPendingDemotionTermination(ctx context.Context) ([]sqlcgen.Sandbox, error) {
	return s.q.ListSandboxesPendingDemotionTermination(ctx)
}

// ClearDemotionTerminationRequested consumes a sandbox's own
// demotion-termination request once app/reconciler.Reconciler has
// successfully issued a real StopSandbox call for it.
func (s *SandboxStore) ClearDemotionTerminationRequested(ctx context.Context, sessionID pgtype.UUID) (sqlcgen.Sandbox, error) {
	return s.q.ClearSandboxDemotionTerminationRequested(ctx, sessionID)
}

// UpdateImageDecision records app/sessionactor's own resolveAndSetImage
// (imageresolve.go) per-spawn/-restore image-resolution outcome for this
// gen -- a real warm-image selection or one of internal/domain/
// imagedecision.Reason's closed set of fallback reasons. See
// UpdateSandboxImageDecision's own generated doc comment.
func (s *SandboxStore) UpdateImageDecision(ctx context.Context, arg sqlcgen.UpdateSandboxImageDecisionParams) (sqlcgen.Sandbox, error) {
	return s.q.UpdateSandboxImageDecision(ctx, arg)
}
