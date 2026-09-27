package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// ReleaseManifestPendingStore is a thin, pass-through wrapper around the
// sqlc-generated release_manifest_pending queries (blocking-finding fix
// #1, "release PR review", §15.2) -- see
// migrations/000050_release_manifest_pending.up.sql's own doc comment for
// the table's full design and the "why" behind this fix, and
// migrations/000147_release_manifest_pending_claimed_at.up.sql for why a
// claim keeps the row until its check returns. No caching, no retries, no
// business rules -- the claim-and-run loop lives in
// internal/app/releasereview.Worker.
type ReleaseManifestPendingStore struct {
	q *sqlcgen.Queries
}

// NewReleaseManifestPendingStore builds a ReleaseManifestPendingStore
// backed by pool.
func NewReleaseManifestPendingStore(pool *pgxpool.Pool) *ReleaseManifestPendingStore {
	return &ReleaseManifestPendingStore{q: sqlcgen.New(pool)}
}

// Create inserts a new release_manifest_pending row and returns it --
// the ONE cheap write internal/adapters/inbound/github's own webhook
// handler makes inline, before its own ack.
func (s *ReleaseManifestPendingStore) Create(ctx context.Context, arg sqlcgen.CreateReleaseManifestPendingParams) (sqlcgen.ReleaseManifestPending, error) {
	return s.q.CreateReleaseManifestPending(ctx, arg)
}

// ClaimDue atomically claims (stamps claimed_at on) up to limit
// not-yet-claimed rows, oldest first -- see ClaimDueReleaseManifestPending's
// own generated doc comment for why claiming a row here IS this table's
// one and only "attempt".
func (s *ReleaseManifestPendingStore) ClaimDue(ctx context.Context, limit int32) ([]sqlcgen.ReleaseManifestPending, error) {
	return s.q.ClaimDueReleaseManifestPending(ctx, limit)
}

// Finish deletes one claimed row once its check has returned -- see
// FinishReleaseManifestPending's own generated doc comment.
func (s *ReleaseManifestPendingStore) Finish(ctx context.Context, id pgtype.UUID) error {
	return s.q.FinishReleaseManifestPending(ctx, id)
}

// PurgeStaleClaimed deletes rows claimed longer ago than maxAge, whose
// worker died mid-check, and reports how many -- see
// PurgeStaleClaimedReleaseManifestPending's own generated doc comment.
func (s *ReleaseManifestPendingStore) PurgeStaleClaimed(ctx context.Context, maxAge time.Duration) (int64, error) {
	return s.q.PurgeStaleClaimedReleaseManifestPending(ctx, pgtype.Interval{Microseconds: maxAge.Microseconds(), Valid: true})
}
