//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"testing"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestSandboxStore_RepoHooksLineage pins what a gen and its snapshot
// record of the repository's hooks (technical plan §6.4, §27.1, migrations/
// 000175): RecordRepoHooksTold writes what the live gen was told, and only
// for the live gen; CarryRepoHooksTold gives a resumed gen what the gen it
// resumes was told; and a snapshot's mint records it as minted without the
// hooks exactly when its gen was told to skip them and its ready advertised
// it honours that -- never on what the agent can do alone. A clear forgets
// it with the snapshot.
func TestSandboxStore_RepoHooksLineage(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSandboxStore(pool)

	for i, tc := range []struct {
		name          string
		told, capable bool
		want          bool
	}{
		{name: "told, capable", told: true, capable: true, want: true},
		{name: "told, an agent that predates the key", told: true},
		{name: "not told, capable", capable: true},
		{name: "neither"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			if _, err := store.Create(ctx, sessionID); err != nil {
				t.Fatal(err)
			}
			if err := store.RecordRepoHooksTold(ctx, sessionID, 1, tc.told); err != nil {
				t.Fatalf("RecordRepoHooksTold: %v", err)
			}
			if err := store.RecordReady(ctx, sessionID, 1, false, false, tc.capable, nil); err != nil {
				t.Fatalf("RecordReady: %v", err)
			}
			snapshotID := fmt.Sprintf("snap-lineage-%d", i)
			row, err := store.UpdateSnapshotID(ctx, sqlcgen.UpdateSandboxSnapshotIDParams{SessionID: sessionID, SnapshotID: &snapshotID})
			if err != nil {
				t.Fatalf("UpdateSnapshotID: %v", err)
			}
			if row.SnapshotSkippedRepoHooks == nil || *row.SnapshotSkippedRepoHooks != tc.want {
				t.Fatalf("snapshot_skipped_repo_hooks = %v, want %v", row.SnapshotSkippedRepoHooks, tc.want)
			}
			if n, err := store.ClearSnapshot(ctx, sessionID, snapshotID); err != nil || n != 1 {
				t.Fatalf("ClearSnapshot = %d, %v", n, err)
			}
			cleared, err := store.Get(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if cleared.SnapshotSkippedRepoHooks != nil {
				t.Fatalf("snapshot_skipped_repo_hooks = %v after the clear, want NULL", *cleared.SnapshotSkippedRepoHooks)
			}
		})
	}

	t.Run("only the live gen records what it was told, and a resume carries it", func(t *testing.T) {
		sessionID := createTestSession(ctx, t, pool)
		if _, err := store.Create(ctx, sessionID); err != nil {
			t.Fatal(err)
		}
		read := func() *int32 {
			t.Helper()
			row, err := store.Get(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			return row.SkipRepoHooksToldGen
		}
		if err := store.RecordRepoHooksTold(ctx, sessionID, 2, true); err != nil {
			t.Fatal(err)
		}
		if got := read(); got != nil {
			t.Fatalf("told gen %d recorded for a gen that is not live, want none", *got)
		}
		if err := store.RecordRepoHooksTold(ctx, sessionID, 1, true); err != nil {
			t.Fatal(err)
		}
		if got := read(); got == nil || *got != 1 {
			t.Fatalf("told gen %v, want 1", got)
		}
		token := "token-hash-resume"
		if _, err := store.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID, TokenHash: &token}); err != nil {
			t.Fatal(err)
		}
		if err := store.CarryRepoHooksTold(ctx, sessionID, 1, 2); err != nil {
			t.Fatal(err)
		}
		if got := read(); got == nil || *got != 2 {
			t.Fatalf("told gen %v after a resume of a told gen, want 2", got)
		}
		if err := store.RecordRepoHooksTold(ctx, sessionID, 2, false); err != nil {
			t.Fatal(err)
		}
		if _, err := store.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID, TokenHash: &token}); err != nil {
			t.Fatal(err)
		}
		if err := store.CarryRepoHooksTold(ctx, sessionID, 2, 3); err != nil {
			t.Fatal(err)
		}
		if got := read(); got != nil {
			t.Fatalf("told gen %d after a resume of a gen told nothing, want none", *got)
		}
	})
}
