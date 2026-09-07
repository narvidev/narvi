//go:build integration

// Integration tests proving §31.7's own G4 arming write
// (GitHubPRSessionStore.RecordMergeOutcome) against a REAL Postgres
// instance -- kept in its own file, mirroring
// githubprsession_store_integration_test.go's own precedent one query
// family over.
package postgres_test

import (
	"context"
	"testing"
	"time"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestRecordMergeOutcome_ClaimedRow_SetsMergedAndClosedAt proves the
// happy path: a (repo, pr) row with a real session attached gets
// pr_merged/pr_closed_at set verbatim.
func TestRecordMergeOutcome_ClaimedRow_SetsMergedAndClosedAt(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	store := narvipg.NewGitHubPRSessionStore(pool)
	const repoFullName = "acme/merge-outcome-claimed"
	const prNumber = int32(1)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	txStore := store.WithTx(tx)
	if err := txStore.EnsureRow(ctx, repoFullName, prNumber); err != nil {
		t.Fatalf("EnsureRow: %v", err)
	}
	created, err := sessions.WithTx(tx).Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := txStore.SetSessionID(ctx, repoFullName, prNumber, created.ID); err != nil {
		t.Fatalf("SetSessionID: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	closedAt := time.Date(2026, 1, 15, 12, 30, 0, 0, time.UTC)
	row, err := store.RecordMergeOutcome(ctx, repoFullName, prNumber, true, closedAt)
	if err != nil {
		t.Fatalf("RecordMergeOutcome: %v", err)
	}
	if row.PrMerged == nil || !*row.PrMerged {
		t.Errorf("row.PrMerged = %v, want true", row.PrMerged)
	}
	if !row.PrClosedAt.Valid || !row.PrClosedAt.Time.Equal(closedAt) {
		t.Errorf("row.PrClosedAt = %+v, want %v", row.PrClosedAt, closedAt)
	}
}

// TestRecordMergeOutcome_NoRowAtAll_ReturnsErrNoRows proves the
// acknowledge-and-ignore case: a (repo, pr) this deployment never saw at
// all.
func TestRecordMergeOutcome_NoRowAtAll_ReturnsErrNoRows(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewGitHubPRSessionStore(pool)

	_, err := store.RecordMergeOutcome(ctx, "acme/merge-outcome-no-row", 1, true, time.Now())
	if err == nil {
		t.Fatal("RecordMergeOutcome: want an error (pgx.ErrNoRows) for a PR with no github_pr_sessions row, got nil")
	}
}

// TestRecordMergeOutcome_RowWithNoSession_ReturnsErrNoRows proves the
// OTHER acknowledge-and-ignore case: a claim row exists (EnsureRow ran)
// but no session was ever actually attached (session_id still NULL) --
// "no session to arm eligibility for", the SAME guard
// UpsertPendingRetriggerHeadSHA already applies one column family over.
func TestRecordMergeOutcome_RowWithNoSession_ReturnsErrNoRows(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewGitHubPRSessionStore(pool)
	const repoFullName = "acme/merge-outcome-no-session"
	const prNumber = int32(1)

	if err := store.EnsureRow(ctx, repoFullName, prNumber); err != nil {
		t.Fatalf("EnsureRow: %v", err)
	}

	_, err := store.RecordMergeOutcome(ctx, repoFullName, prNumber, true, time.Now())
	if err == nil {
		t.Fatal("RecordMergeOutcome: want an error (pgx.ErrNoRows) for a claim row with session_id still NULL, got nil")
	}
}
