package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
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

// WithTx returns a ReleaseManifestCheckStore whose queries run on tx
// instead of the pool this store was built with -- mirrors AuditLogStore's
// own identical WithTx convention (audit_log_store.go). Confirmed-major
// fix: the Block/Acknowledge/Unblock decision write and its own audit_log
// row (releasecompositiondecision.go) now share ONE transaction, exactly
// like every other Authorize-gated state change in this codebase already
// does (audit.go's own top doc comment: "written in the same tx as the
// change") -- before this fix, a failure recording the audit row left the
// guarded decision UPDATE already durably committed on its own, an
// irreversible, unattributed override with no audit trail.
func (s *ReleaseManifestCheckStore) WithTx(tx pgx.Tx) *ReleaseManifestCheckStore {
	return &ReleaseManifestCheckStore{q: s.q.WithTx(tx)}
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
// The session-scoped lookup, used by the composition-findings-
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

// UpdateCompositionAnchor persists composition_head_sha/
// composition_diff_truncated against the release manifest check row named
// by id -- a guarded UPDATE ("AND composition_head_sha IS NULL", see the
// underlying query's own doc comment); pgx.ErrNoRows means an anchor was
// already recorded for this row (a retried/duplicate dispatch), never a
// silent overwrite. diffTruncated is a plain bool, never *bool: the ONE
// caller (dispatchCompositionReview) only ever calls this once it has
// already confirmed a live diff was fetched (compositiondispatch.go's own
// "declines to dispatch... diff fetch failed" branch), so there is no
// third "unknown" state to represent here.
func (s *ReleaseManifestCheckStore) UpdateCompositionAnchor(ctx context.Context, id pgtype.UUID, headSHA string, diffTruncated bool) (sqlcgen.ReleaseManifestCheck, error) {
	return s.q.UpdateReleaseManifestCompositionAnchor(ctx, sqlcgen.UpdateReleaseManifestCompositionAnchorParams{
		ID:                       id,
		CompositionHeadSha:       &headSHA,
		CompositionDiffTruncated: &diffTruncated,
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
