package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// RepoEntitlementRevocationStore is a thin, pass-through wrapper around the
// repo_entitlement_revocations writes (technical plan §31.4): an
// administrator's revocation of a repository's eligibility for new
// sessions, and its restore. Its callers are the
// admin-only routes under /api/repos/{owner}/{repo}/entitlement
// (httpapi/repoentitlement.go), each in the transaction that writes its
// audit_log row. Eligibility is never read here: GitHubPRSessionStore's
// RepoEntitlement and RevokedRepoForSession read a revocation in the same
// statement as the rest of the decision.
type RepoEntitlementRevocationStore struct {
	q *sqlcgen.Queries
}

// NewRepoEntitlementRevocationStore builds a RepoEntitlementRevocationStore
// backed by pool.
func NewRepoEntitlementRevocationStore(pool *pgxpool.Pool) *RepoEntitlementRevocationStore {
	return &RepoEntitlementRevocationStore{q: sqlcgen.New(pool)}
}

// WithTx returns a RepoEntitlementRevocationStore whose queries run on tx.
func (s *RepoEntitlementRevocationStore) WithTx(tx pgx.Tx) *RepoEntitlementRevocationStore {
	return &RepoEntitlementRevocationStore{q: s.q.WithTx(tx)}
}

// Revoke records repoFullName's revocation by revokedBy (invalid for no
// user) with reason. pgx.ErrNoRows when the repository is already revoked:
// the first revocation is kept as it is.
func (s *RepoEntitlementRevocationStore) Revoke(ctx context.Context, repoFullName string, revokedBy pgtype.UUID, reason string) (sqlcgen.RepoEntitlementRevocation, error) {
	return s.q.RevokeRepoEntitlement(ctx, sqlcgen.RevokeRepoEntitlementParams{
		RepoFullName: repoFullName,
		RevokedBy:    revokedBy,
		Reason:       reason,
	})
}

// Restore deletes repoFullName's revocation and returns it. pgx.ErrNoRows
// when the repository is not revoked.
func (s *RepoEntitlementRevocationStore) Restore(ctx context.Context, repoFullName string) (sqlcgen.RepoEntitlementRevocation, error) {
	return s.q.RestoreRepoEntitlement(ctx, repoFullName)
}

// Get returns repoFullName's revocation with the revoking user's display
// name. pgx.ErrNoRows when the repository is not revoked.
func (s *RepoEntitlementRevocationStore) Get(ctx context.Context, repoFullName string) (sqlcgen.GetRepoEntitlementRevocationRow, error) {
	return s.q.GetRepoEntitlementRevocation(ctx, repoFullName)
}
