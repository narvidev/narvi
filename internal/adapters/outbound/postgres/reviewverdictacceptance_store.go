package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// ReviewVerdictAcceptanceStore is a thin, pass-through wrapper around the
// sqlc-generated review_verdict_acceptances queries ("human acceptance
// of a verdict the engine refuses", §21.1b) -- see migrations/
// 000135_review_verdict_acceptances.up.sql's own doc comment for the
// table's full design. No caching, no retries, no business rules
// (mirrors FalsePositivePatternStore's own identical "thin wrapper"
// precedent, reviewfalsepositivepatterns_store.go) -- callers
// (internal/app/reviewverdict, internal/adapters/inbound/httpapi) decide
// what a missing row or an inapplicable acceptance means for their own
// purposes.
type ReviewVerdictAcceptanceStore struct {
	q *sqlcgen.Queries
}

// NewReviewVerdictAcceptanceStore builds a ReviewVerdictAcceptanceStore
// backed by pool.
func NewReviewVerdictAcceptanceStore(pool *pgxpool.Pool) *ReviewVerdictAcceptanceStore {
	return &ReviewVerdictAcceptanceStore{q: sqlcgen.New(pool)}
}

// WithTx returns a ReviewVerdictAcceptanceStore whose queries run on tx
// instead of the pool this store was built with -- mirrors every other
// store's own identical WithTx convention (e.g. FalsePositivePatternStore.
// WithTx).
func (s *ReviewVerdictAcceptanceStore) WithTx(tx pgx.Tx) *ReviewVerdictAcceptanceStore {
	return &ReviewVerdictAcceptanceStore{q: s.q.WithTx(tx)}
}

// Insert creates one review_verdict_acceptances row -- see
// InsertReviewVerdictAcceptance's own generated doc comment for why this
// is always a plain INSERT, never an upsert (this table is append-only).
//
// FIRST supersedes (revokes) any existing active row for the SAME
// (repo_full_name, pr_number) -- SupersedeActiveReviewVerdictAcceptances's
// own generated doc comment (finding F2, adversarial review) explains why
// this is two SEQUENTIAL statements, never one combined WITH-clause
// statement: verified against real Postgres, a single `WITH superseded AS
// (UPDATE ...) INSERT ...` statement's own primary INSERT does not see
// the CTE's UPDATE for unique-constraint-checking purposes (both execute
// against the SAME start-of-query snapshot), so it fails with a spurious
// duplicate-key error on the routine case this exists to allow. Not
// wrapped in an explicit transaction spanning both calls (unlike
// OIDCSigningKeyStore.Rotate's own retire-then-create pair, which needs
// one to observe the other's committed row for its own audit contract) --
// review_verdict_acceptances_one_active_idx is what actually enforces "at
// most one active row" regardless of whether these two statements commit
// together; a concurrent accept racing between them can still make the
// INSERT below fail on that constraint, an ordinary retryable error, but
// can never produce two live rows.
func (s *ReviewVerdictAcceptanceStore) Insert(ctx context.Context, arg sqlcgen.InsertReviewVerdictAcceptanceParams) (sqlcgen.ReviewVerdictAcceptance, error) {
	if err := s.q.SupersedeActiveReviewVerdictAcceptances(ctx, sqlcgen.SupersedeActiveReviewVerdictAcceptancesParams{
		RepoFullName: arg.RepoFullName,
		PrNumber:     arg.PrNumber,
		RevokedBy:    arg.AcceptedBy,
	}); err != nil {
		return sqlcgen.ReviewVerdictAcceptance{}, err
	}
	return s.q.InsertReviewVerdictAcceptance(ctx, arg)
}

// GetActive fetches the LATEST non-revoked acceptance for
// (repoFullName, prNumber), if any -- pgx.ErrNoRows (unwrapped) means no
// acceptance is currently active for this PR. See
// GetActiveReviewVerdictAcceptance's own generated doc comment: a row
// returned here may still be inapplicable to the PR's CURRENT verdict --
// the caller (internal/domain/reviewverdict.Acceptance.Applicable)
// decides that, never this store.
func (s *ReviewVerdictAcceptanceStore) GetActive(ctx context.Context, repoFullName string, prNumber int32) (sqlcgen.ReviewVerdictAcceptance, error) {
	return s.q.GetActiveReviewVerdictAcceptance(ctx, sqlcgen.GetActiveReviewVerdictAcceptanceParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
	})
}

// Get fetches one acceptance by id, SCOPED to repoFullName -- mirrors
// FalsePositivePatternStore.Get's own identical audit-fix precedent:
// pgx.ErrNoRows (unwrapped) means no such acceptance exists IN THIS
// REPO.
func (s *ReviewVerdictAcceptanceStore) Get(ctx context.Context, id pgtype.UUID, repoFullName string) (sqlcgen.ReviewVerdictAcceptance, error) {
	return s.q.GetReviewVerdictAcceptance(ctx, sqlcgen.GetReviewVerdictAcceptanceParams{ID: id, RepoFullName: repoFullName})
}

// Revoke records a maintainer+'s explicit revocation of acceptance id,
// SCOPED to repoFullName and GUARDED (WHERE revoked_at IS NULL) -- see
// RevokeReviewVerdictAcceptance's own generated doc comment. pgx.ErrNoRows
// (unwrapped) means EITHER no acceptance with this id exists in this
// repo at all, OR it exists but is already revoked -- the caller
// distinguishes the two with a follow-up Get (also repo-scoped) on this
// same error path, mirroring FalsePositivePatternStore.Retire's own
// identical caller-side discipline.
func (s *ReviewVerdictAcceptanceStore) Revoke(ctx context.Context, id, revokedBy pgtype.UUID, repoFullName string) (sqlcgen.ReviewVerdictAcceptance, error) {
	return s.q.RevokeReviewVerdictAcceptance(ctx, sqlcgen.RevokeReviewVerdictAcceptanceParams{
		ID:           id,
		RevokedBy:    revokedBy,
		RepoFullName: repoFullName,
	})
}

// List returns every acceptance (active or revoked) for
// (repoFullName, prNumber), newest-first, bounded by limit -- the audit
// view.
func (s *ReviewVerdictAcceptanceStore) List(ctx context.Context, repoFullName string, prNumber, limit int32) ([]sqlcgen.ReviewVerdictAcceptance, error) {
	return s.q.ListReviewVerdictAcceptances(ctx, sqlcgen.ListReviewVerdictAcceptancesParams{
		RepoFullName: repoFullName,
		PrNumber:     prNumber,
		Limit:        limit,
	})
}
