package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// ReleaseManifestCheckStore is a thin, pass-through wrapper around the
// sqlc-generated release_manifest_checks queries (§12.2 item 9, §15.2/
// §15.3) -- see migrations/000097_release_manifest_checks.up.sql's own
// doc comment for the table's full design. No caching, no retries, no
// business rules -- mirrors every other Store in this package.
type ReleaseManifestCheckStore struct {
	q *sqlcgen.Queries
}

// NewReleaseManifestCheckStore builds a ReleaseManifestCheckStore backed
// by pool.
func NewReleaseManifestCheckStore(pool *pgxpool.Pool) *ReleaseManifestCheckStore {
	return &ReleaseManifestCheckStore{q: sqlcgen.New(pool)}
}

// Insert appends one release_manifest_checks row -- internal/app/
// releasereview.Run's own best-effort write, alongside its existing
// outbox-delivered comment.
func (s *ReleaseManifestCheckStore) Insert(ctx context.Context, arg sqlcgen.InsertReleaseManifestCheckParams) (sqlcgen.ReleaseManifestCheck, error) {
	return s.q.InsertReleaseManifestCheck(ctx, arg)
}

// GetLatest fetches (repoFullName, prNumber)'s own most-recently-computed
// check. pgx.ErrNoRows (unwrapped) means no check has ever been persisted
// for this PR -- callers (the release-review readout endpoint) render an
// honest "not yet available" state, never a fabricated one.
func (s *ReleaseManifestCheckStore) GetLatest(ctx context.Context, repoFullName string, prNumber int32) (sqlcgen.ReleaseManifestCheck, error) {
	return s.q.GetLatestReleaseManifestCheck(ctx, sqlcgen.GetLatestReleaseManifestCheckParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
	})
}

// GetBySessionID fetches sessionID's own most-recently-computed check --
// Step 125's own session-scoped lookup, used by the composition-findings-
// posting tool and the Block/Acknowledge actions, none of which know
// (repoFullName, prNumber) up front. pgx.ErrNoRows means this session has
// no release manifest check on record at all.
func (s *ReleaseManifestCheckStore) GetBySessionID(ctx context.Context, sessionID pgtype.UUID) (sqlcgen.ReleaseManifestCheck, error) {
	return s.q.GetLatestReleaseManifestCheckBySessionID(ctx, sessionID)
}

// UpdateCompositionFindings persists §15.3's own composition findings
// against the release manifest check row named by id -- a guarded UPDATE
// ("AND composition_reviewed_at IS NULL", see the underlying query's own
// doc comment); pgx.ErrNoRows means findings were already posted for this
// row (a retried/duplicate tool call), never a silent overwrite.
func (s *ReleaseManifestCheckStore) UpdateCompositionFindings(ctx context.Context, id pgtype.UUID, findingsJSON []byte) (sqlcgen.ReleaseManifestCheck, error) {
	return s.q.UpdateReleaseManifestCompositionFindings(ctx, sqlcgen.UpdateReleaseManifestCompositionFindingsParams{
		ID:                  id,
		CompositionFindings: findingsJSON,
	})
}

// UpdateCompositionDecision persists §12.2 item 9's own Block release /
// Acknowledge & ship decision against the release manifest check row
// named by id -- a guarded compare-and-swap against expectedCurrent (the
// SAME value the caller already validated via internal/domain/review.
// TransitionCompositionDecision); pgx.ErrNoRows means a concurrent
// decision already won the race, never a silent overwrite.
func (s *ReleaseManifestCheckStore) UpdateCompositionDecision(ctx context.Context, id pgtype.UUID, expectedCurrent, newDecision string, decisionBy pgtype.UUID) (sqlcgen.ReleaseManifestCheck, error) {
	return s.q.UpdateReleaseManifestCompositionDecision(ctx, sqlcgen.UpdateReleaseManifestCompositionDecisionParams{
		ID:                    id,
		CompositionDecision:   newDecision,
		CompositionDecisionBy: decisionBy,
		ExpectedDecision:      expectedCurrent,
	})
}
