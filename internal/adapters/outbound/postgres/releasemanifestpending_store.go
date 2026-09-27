package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// ReleaseManifestPendingStore is a thin wrapper around the sqlc-generated
// release_manifest_pending and release_manifest_checks_running queries
// (blocking-finding fix #1, "release PR review", §15.2) -- see
// migrations/000050_release_manifest_pending.up.sql's own doc comment for
// the table's full design and the "why" behind this fix, and
// migrations/000146_release_manifest_checks_running.up.sql for why a claim
// records the check it starts in a table of its own. No caching, no
// retries, no business rules -- the claim-and-run loop lives in
// internal/app/releasereview.Worker.
type ReleaseManifestPendingStore struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

// NewReleaseManifestPendingStore builds a ReleaseManifestPendingStore
// backed by pool.
func NewReleaseManifestPendingStore(pool *pgxpool.Pool) *ReleaseManifestPendingStore {
	return &ReleaseManifestPendingStore{pool: pool, q: sqlcgen.New(pool)}
}

// Create inserts a new release_manifest_pending row and returns it --
// the ONE cheap write internal/adapters/inbound/github's own webhook
// handler makes inline, before its own ack.
func (s *ReleaseManifestPendingStore) Create(ctx context.Context, arg sqlcgen.CreateReleaseManifestPendingParams) (sqlcgen.ReleaseManifestPending, error) {
	return s.q.CreateReleaseManifestPending(ctx, arg)
}

// ClaimDue atomically claims (deletes) up to limit rows, oldest first --
// see ClaimDueReleaseManifestPending's own generated doc comment for why
// claiming a row here IS this table's one and only "attempt" -- and, in
// the SAME transaction, records each claimed check as running
// (StartReleaseManifestCheck, technical plan §43.20). The delete is the
// claim every earlier binary also runs, so a pod on either version can
// never take a row the other has claimed; the running row is what the
// session's status reads while the check runs.
func (s *ReleaseManifestPendingStore) ClaimDue(ctx context.Context, limit int32) (claimed []sqlcgen.ReleaseManifestPending, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin release manifest claim: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	q := s.q.WithTx(tx)
	claimed, err = q.ClaimDueReleaseManifestPending(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("claim release manifest pending rows: %w", err)
	}
	for _, row := range claimed {
		if err = q.StartReleaseManifestCheck(ctx, sqlcgen.StartReleaseManifestCheckParams{PendingID: row.ID, SessionID: row.SessionID}); err != nil {
			return nil, fmt.Errorf("record release manifest check running: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit release manifest claim: %w", err)
	}
	return claimed, nil
}

// Finish deletes the running row of one claimed check once it has
// returned -- see FinishReleaseManifestCheck's own generated doc comment.
func (s *ReleaseManifestPendingStore) Finish(ctx context.Context, pendingID pgtype.UUID) error {
	return s.q.FinishReleaseManifestCheck(ctx, pendingID)
}

// PurgeStaleClaimed deletes running rows claimed longer ago than maxAge,
// whose worker died mid-check, and reports how many -- see
// PurgeStaleReleaseManifestChecks' own generated doc comment.
func (s *ReleaseManifestPendingStore) PurgeStaleClaimed(ctx context.Context, maxAge time.Duration) (int64, error) {
	return s.q.PurgeStaleReleaseManifestChecks(ctx, pgtype.Interval{Microseconds: maxAge.Microseconds(), Valid: true})
}
