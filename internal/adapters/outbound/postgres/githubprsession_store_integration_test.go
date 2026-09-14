//go:build integration

// Integration tests for GitHubPRSessionStore (§8.2's "atomic claim
// coalescing of concurrent @mentions" -- "GitHub ingress"). Kept
// in its own file, mirroring webhookdelivery_store_integration_test.go's
// own precedent of a focused file per query/claim primitive.
package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestGitHubPRSessionStore_LockForUpdate_FirstCallerSeesNoSession proves
// the happy path: EnsureRow followed by LockForUpdate, for a
// (repo, pr) pair no one has claimed yet, returns an invalid (NULL)
// session_id -- "you are the first mention, go create a session".
func TestGitHubPRSessionStore_LockForUpdate_FirstCallerSeesNoSession(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	store := narvipg.NewGitHubPRSessionStore(pool)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	txStore := store.WithTx(tx)
	if err := txStore.EnsureRow(ctx, "acme/widgets", 42); err != nil {
		t.Fatalf("EnsureRow: %v", err)
	}

	got, err := txStore.LockForUpdate(ctx, "acme/widgets", 42)
	if err != nil {
		t.Fatalf("LockForUpdate: %v", err)
	}
	if got.Valid {
		t.Errorf("LockForUpdate session_id.Valid = true, want false (no claim yet)")
	}

	created, err := sessions.WithTx(tx).Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := txStore.SetSessionID(ctx, "acme/widgets", 42, created.ID); err != nil {
		t.Fatalf("SetSessionID: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// A fresh lookup (its own transaction) now sees the claimed session.
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin (verify): %v", err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()
	got2, err := store.WithTx(tx2).LockForUpdate(ctx, "acme/widgets", 42)
	if err != nil {
		t.Fatalf("LockForUpdate (verify): %v", err)
	}
	if !got2.Valid || got2 != created.ID {
		t.Errorf("LockForUpdate (verify) = %+v, want %+v", got2, created.ID)
	}
}

// TestGitHubPRSessionStore_ConcurrentClaim_ExactlyOneWinnerSeesNoSession
// proves the claim is genuinely concurrent-safe: N goroutines racing to
// claim the SAME (repo, pr) via EnsureRow+LockForUpdate, each holding its
// own transaction, must yield exactly one "no session yet" (NULL)
// observation -- the real first mention -- and N-1 observations of that
// SAME winner's session_id, once each blocked transaction's own
// LockForUpdate unblocks after the winner commits. Mirrors
// webhookdelivery_store_integration_test.go's own
// TestWebhookDeliveryStore_Claim_ConcurrentSameIdentity_ExactlyOneWinner
// concurrency-test shape.
func TestGitHubPRSessionStore_ConcurrentClaim_ExactlyOneWinnerSeesNoSession(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	store := narvipg.NewGitHubPRSessionStore(pool)

	const n = 10
	const repoFullName = "acme/widgets"
	const prNumber = int32(7)

	// start gates every goroutine's own EnsureRow+LockForUpdate call so
	// they all arrive at the row lock roughly together -- proving genuine
	// concurrency, not an accidental sequential ordering. Closing a
	// channel (rather than e.g. a sync.WaitGroup used as a one-shot gate)
	// is the idiomatic Go way to broadcast "go" to every waiting goroutine
	// at once.
	start := make(chan struct{})

	results := make([]pgtype.UUID, n)
	var g errgroup.Group

	for i := 0; i < n; i++ {
		idx := i
		g.Go(func() error {
			<-start

			tx, err := pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(ctx) }()

			txStore := store.WithTx(tx)
			if err := txStore.EnsureRow(ctx, repoFullName, prNumber); err != nil {
				return err
			}

			existing, err := txStore.LockForUpdate(ctx, repoFullName, prNumber)
			if err != nil {
				return err
			}

			if existing.Valid {
				// Lost the race: nothing to do but record what we saw and
				// release the lock immediately.
				results[idx] = existing
				return tx.Commit(ctx)
			}

			// Won the race: create the session (still holding the lock),
			// fill it in, and commit -- unblocking every other goroutine's
			// own LockForUpdate, which will then observe this exact
			// session_id.
			created, err := sessions.WithTx(tx).Create(ctx, sqlcgen.CreateSessionParams{
				SpawnSource: sqlcgen.SessionSpawnSourceGithub,
			})
			if err != nil {
				return err
			}
			if err := txStore.SetSessionID(ctx, repoFullName, prNumber, created.ID); err != nil {
				return err
			}
			results[idx] = created.ID
			return tx.Commit(ctx)
		})
	}

	close(start)
	if err := g.Wait(); err != nil {
		t.Fatalf("concurrent claim: %v", err)
	}

	first := results[0]
	if !first.Valid {
		t.Fatal("results[0] is invalid, want a real session id")
	}
	for i, got := range results {
		if got != first {
			t.Errorf("results[%d] = %+v, want %+v (every goroutine must observe the SAME single winner session)", i, got, first)
		}
	}

	var sessionCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM sessions WHERE spawn_source = 'github'`,
	).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 1 {
		t.Errorf("session count = %d, want exactly 1 (no duplicate session from the race)", sessionCount)
	}

	var claimRowCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM github_pr_sessions WHERE repo_full_name = $1 AND pr_number = $2`,
		repoFullName, prNumber,
	).Scan(&claimRowCount); err != nil {
		t.Fatalf("count claim rows: %v", err)
	}
	if claimRowCount != 1 {
		t.Errorf("claim row count = %d, want exactly 1", claimRowCount)
	}
}

// TestGitHubPRSessionStore_GetByRepoAndPRNumber proves the FORWARD,
// non-claiming read the decision inbox's own "open review" action needs:
// a claimed (repo, pr) resolves its real session id, an unclaimed one
// reports pgx.ErrNoRows, and -- the property that actually matters for
// this read (a row must never carry a session id belonging to a
// different PR) -- two DIFFERENT PRs, each claimed by its OWN session,
// never cross-resolve to each other's session id.
func TestGitHubPRSessionStore_GetByRepoAndPRNumber(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	store := narvipg.NewGitHubPRSessionStore(pool)

	t.Run("no claim at all -- pgx.ErrNoRows, never a fabricated row", func(t *testing.T) {
		_, err := store.GetByRepoAndPRNumber(ctx, "acme/widgets", 9999)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("GetByRepoAndPRNumber(unclaimed) error = %v, want pgx.ErrNoRows", err)
		}
	})

	// Two distinct PRs, each claimed by its own distinct session -- the
	// fixture this property actually needs: a single-PR fixture could
	// pass this test for the wrong reason (e.g. a query that ignores
	// pr_number and returns ANY row for the repo).
	claim := func(t *testing.T, repoFullName string, prNumber int32) pgtype.UUID {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		txStore := store.WithTx(tx)
		if err := txStore.EnsureRow(ctx, repoFullName, prNumber); err != nil {
			t.Fatalf("EnsureRow: %v", err)
		}
		if _, err := txStore.LockForUpdate(ctx, repoFullName, prNumber); err != nil {
			t.Fatalf("LockForUpdate: %v", err)
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
		return created.ID
	}

	sessionA := claim(t, "acme/widgets", 501)
	sessionB := claim(t, "acme/widgets", 502)
	if sessionA == sessionB {
		t.Fatalf("fixture bug: sessionA and sessionB are the same session (%+v) -- this test needs two distinct sessions to prove no cross-contamination", sessionA)
	}

	t.Run("PR #501 resolves session A, never session B", func(t *testing.T) {
		row, err := store.GetByRepoAndPRNumber(ctx, "acme/widgets", 501)
		if err != nil {
			t.Fatalf("GetByRepoAndPRNumber(501): %v", err)
		}
		if row.SessionID != sessionA {
			t.Errorf("GetByRepoAndPRNumber(501).SessionID = %+v, want %+v (sessionA) -- got %+v (sessionB) instead if this fails on cross-contamination", row.SessionID, sessionA, sessionB)
		}
	})

	t.Run("PR #502 resolves session B, never session A", func(t *testing.T) {
		row, err := store.GetByRepoAndPRNumber(ctx, "acme/widgets", 502)
		if err != nil {
			t.Fatalf("GetByRepoAndPRNumber(502): %v", err)
		}
		if row.SessionID != sessionB {
			t.Errorf("GetByRepoAndPRNumber(502).SessionID = %+v, want %+v (sessionB) -- got %+v (sessionA) instead if this fails on cross-contamination", row.SessionID, sessionB, sessionA)
		}
	})

	t.Run("a different repo with the SAME pr_number never cross-resolves", func(t *testing.T) {
		// github_pr_sessions' own primary key is (repo_full_name,
		// pr_number) TOGETHER -- a query that filtered on pr_number alone
		// would incorrectly match PR #501 in a completely different repo.
		_, err := store.GetByRepoAndPRNumber(ctx, "acme/other-repo", 501)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("GetByRepoAndPRNumber(acme/other-repo#501) error = %v, want pgx.ErrNoRows (no claim exists for this repo)", err)
		}
	})
}
