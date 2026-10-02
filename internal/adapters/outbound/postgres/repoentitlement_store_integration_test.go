//go:build integration

// Integration tests for the reads and writes behind an administrator's
// revocation of a repository (technical plan §31.4, "Un-entitlement"):
// GitHubPRSessionStore.RepoEntitlement and RevokedRepoForSession, which
// read a revocation in the same statement as the rest of the decision, and
// RepoEntitlementRevocationStore, the admin routes' writes.
package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// claimRepoForTest commits a github_pr_sessions claim of repoFullName's
// pull request prNumber for sessionID, as the GitHub ingress does.
func claimRepoForTest(ctx context.Context, t *testing.T, store *narvipg.GitHubPRSessionStore, repoFullName string, prNumber int32, sessionID pgtype.UUID) {
	t.Helper()
	if err := store.EnsureRow(ctx, repoFullName, prNumber); err != nil {
		t.Fatalf("EnsureRow(%s#%d): %v", repoFullName, prNumber, err)
	}
	if err := store.SetSessionID(ctx, repoFullName, prNumber, sessionID); err != nil {
		t.Fatalf("SetSessionID(%s#%d): %v", repoFullName, prNumber, err)
	}
}

// revokeRepoForTest records an administrator's revocation of repoFullName
// with no revoking user.
func revokeRepoForTest(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoFullName string) {
	t.Helper()
	if _, err := narvipg.NewRepoEntitlementRevocationStore(pool).Revoke(ctx, repoFullName, pgtype.UUID{}, "test revocation"); err != nil {
		t.Fatalf("revoke %s: %v", repoFullName, err)
	}
}

// TestGitHubPRSessionStore_RepoEntitlement_ReadsKnownAndRevokedTogether
// pins the one read of a repository's eligibility in all four
// combinations: Known from github_pr_sessions, Revoked from
// repo_entitlement_revocations, each independent of the other, read in one
// statement.
func TestGitHubPRSessionStore_RepoEntitlement_ReadsKnownAndRevokedTogether(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewGitHubPRSessionStore(pool)

	for _, tc := range []struct {
		repo          string
		known, revoke bool
	}{
		{"acme/known-open", true, false},
		{"acme/known-revoked", true, true},
		{"acme/unknown-revoked", false, true},
		{"acme/unknown-open", false, false},
	} {
		if tc.known {
			claimRepoForTest(ctx, t, store, tc.repo, 1, createTestSession(ctx, t, pool))
		}
		if tc.revoke {
			revokeRepoForTest(ctx, t, pool, tc.repo)
		}
	}

	for _, tc := range []struct {
		repo string
		want narvipg.RepoEntitlementFacts
	}{
		{"acme/known-open", narvipg.RepoEntitlementFacts{Known: true, Revoked: false}},
		{"acme/known-revoked", narvipg.RepoEntitlementFacts{Known: true, Revoked: true}},
		{"acme/unknown-revoked", narvipg.RepoEntitlementFacts{Known: false, Revoked: true}},
		{"acme/unknown-open", narvipg.RepoEntitlementFacts{Known: false, Revoked: false}},
		// Exact match, never case-folded, like the eligibility read.
		{"ACME/known-revoked", narvipg.RepoEntitlementFacts{Known: false, Revoked: false}},
	} {
		t.Run(tc.repo, func(t *testing.T) {
			got, err := store.RepoEntitlement(ctx, tc.repo)
			if err != nil {
				t.Fatalf("RepoEntitlement: %v", err)
			}
			if got != tc.want {
				t.Errorf("RepoEntitlement(%s) = %+v, want %+v", tc.repo, got, tc.want)
			}
		})
	}
}

// TestGitHubPRSessionStore_FirstRevokedRepoForSession pins the session
// actor's re-read: a session's revoked repository is found through the
// clone URLs the caller passes, through a review session's pull-request
// claim when its URL names a fork, and through a sentinel auto-fix child's
// claim -- and nothing is found when none is revoked.
func TestGitHubPRSessionStore_FirstRevokedRepoForSession(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewGitHubPRSessionStore(pool)

	// A review session of acme/base's pull request #7, cloned from a fork.
	reviewSession := createTestSession(ctx, t, pool)
	claimRepoForTest(ctx, t, store, "acme/base", 7, reviewSession)

	// A sentinel auto-fix child of acme/fixed's pull request #3.
	origin := createTestSession(ctx, t, pool)
	child := createTestSession(ctx, t, pool)
	fix, err := narvipg.NewSentinelFixStore(pool).Claim(ctx, "acme/fixed", 3, origin, "feature")
	if err != nil {
		t.Fatalf("claim sentinel fix: %v", err)
	}
	if _, err := narvipg.NewSentinelFixStore(pool).UpdateChildSession(ctx, fix.ID, child); err != nil {
		t.Fatalf("set fix child session: %v", err)
	}

	plain := createTestSession(ctx, t, pool)

	for _, tc := range []struct {
		name      string
		revoke    []string
		session   pgtype.UUID
		names     []string
		wantRepo  string
		wantFound bool
	}{
		{"none revoked", nil, reviewSession, []string{"contributor/base"}, "", false},
		{"by clone URL", []string{"acme/plain"}, plain, []string{"acme/plain"}, "acme/plain", true},
		{"by clone URL, nil names elsewhere", []string{"acme/plain"}, plain, nil, "", false},
		{"by the pull request's claim, its URL a fork", []string{"acme/base"}, reviewSession, []string{"contributor/base"}, "acme/base", true},
		{"by the sentinel child's claim", []string{"acme/fixed"}, child, []string{"contributor/fixed"}, "acme/fixed", true},
		{"another session's claim is not this one's", []string{"acme/base"}, plain, []string{"acme/plain"}, "", false},
		{"first in name order", []string{"acme/zz", "acme/aa"}, plain, []string{"acme/zz", "acme/aa"}, "acme/aa", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, `DELETE FROM repo_entitlement_revocations`); err != nil {
				t.Fatalf("clear revocations: %v", err)
			}
			for _, repo := range tc.revoke {
				revokeRepoForTest(ctx, t, pool, repo)
			}
			repo, found, err := store.RevokedRepoForSession(ctx, tc.session, tc.names)
			if err != nil {
				t.Fatalf("RevokedRepoForSession: %v", err)
			}
			if repo != tc.wantRepo || found != tc.wantFound {
				t.Errorf("RevokedRepoForSession = (%q, %v), want (%q, %v)", repo, found, tc.wantRepo, tc.wantFound)
			}
		})
	}
}

// TestRepoEntitlementRevocationStore_RevokeRestore pins the writes: a
// revocation keeps who, when and why; a second revoke is refused
// (pgx.ErrNoRows) and keeps the first; restore returns what it lifted and a
// second restore is refused the same way; and the table's own CHECK refuses
// a blank or over-long reason, whatever a caller sends.
func TestRepoEntitlementRevocationStore_RevokeRestore(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewRepoEntitlementRevocationStore(pool)

	admin, err := narvipg.NewUserStore(pool).Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "revoker@example.com", DisplayName: "Rae Voker", Role: sqlcgen.UserRoleAdmin,
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}

	first, err := store.Revoke(ctx, "acme/widgets", admin.ID, "security review pending")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if first.RevokedBy != admin.ID || first.Reason != "security review pending" || !first.RevokedAt.Valid {
		t.Errorf("Revoke = %+v, want the admin, the reason and an instant", first)
	}

	if _, err := store.Revoke(ctx, "acme/widgets", pgtype.UUID{}, "a second reason"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second Revoke error = %v, want pgx.ErrNoRows", err)
	}
	got, err := store.Get(ctx, "acme/widgets")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Reason != "security review pending" || got.RevokedBy != admin.ID || got.RevokedByDisplayName == nil || *got.RevokedByDisplayName != "Rae Voker" {
		t.Errorf("Get after a second revoke = %+v, want the first revocation kept, with its admin's name", got)
	}

	lifted, err := store.Restore(ctx, "acme/widgets")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if lifted.Reason != "security review pending" || lifted.RevokedBy != admin.ID {
		t.Errorf("Restore = %+v, want the revocation it lifted", lifted)
	}
	if _, err := store.Restore(ctx, "acme/widgets"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second Restore error = %v, want pgx.ErrNoRows", err)
	}
	if _, err := store.Get(ctx, "acme/widgets"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("Get after restore error = %v, want pgx.ErrNoRows", err)
	}

	for _, tc := range []struct {
		name   string
		reason string
	}{
		{"empty", ""},
		{"spaces only", "   "},
		{"501 characters", strings.Repeat("é", 501)},
	} {
		t.Run("CHECK refuses "+tc.name, func(t *testing.T) {
			_, err := store.Revoke(ctx, "acme/check", pgtype.UUID{}, tc.reason)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "repo_entitlement_revocations_reason_check" {
				t.Fatalf("Revoke(%q) error = %v, want the reason CHECK violation", tc.name, err)
			}
		})
	}
	if _, err := store.Revoke(ctx, "acme/check", pgtype.UUID{}, strings.Repeat("é", 500)); err != nil {
		t.Errorf("Revoke with 500 characters: %v, want accepted", err)
	}

	// The revoking user's deletion keeps the revocation, without a name.
	if _, err := store.Revoke(ctx, "acme/orphaned", admin.ID, "kept after the admin leaves"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, admin.ID); err != nil {
		t.Fatalf("delete admin: %v", err)
	}
	orphaned, err := store.Get(ctx, "acme/orphaned")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if orphaned.RevokedBy.Valid || orphaned.RevokedByDisplayName != nil || orphaned.Reason != "kept after the admin leaves" {
		t.Errorf("Get after the admin's deletion = %+v, want the revocation kept with no user", orphaned)
	}
}
