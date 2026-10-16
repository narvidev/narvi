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
// resumes was told; and a snapshot's mint records its own id as minted
// without the hooks exactly when its gen was told to skip them and its
// ready advertised it honours that -- never on what the agent can do
// alone, nor on a record of an earlier gen. A clear forgets it, and a
// snapshot the previous binary mints never takes an earlier snapshot's
// record for its own.
func TestSandboxStore_RepoHooksLineage(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSandboxStore(pool)
	token := "token-hash-lineage"

	for i, tc := range []struct {
		name          string
		told, capable bool
		// laterGen: the gen is bumped once after both are recorded, with
		// nothing recorded for the new gen -- the previous binary's spawn
		// -- so they name an earlier gen when the snapshot is minted.
		laterGen bool
		want     bool
	}{
		{name: "told, capable", told: true, capable: true, want: true},
		{name: "told, an agent that predates the key", told: true},
		{name: "not told, capable", capable: true},
		{name: "neither"},
		{name: "told and capable at an earlier gen", told: true, capable: true, laterGen: true},
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
			if tc.laterGen {
				if _, err := store.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID, TokenHash: &token}); err != nil {
					t.Fatal(err)
				}
			}
			snapshotID := fmt.Sprintf("snap-lineage-%d", i)
			row, err := store.UpdateSnapshotID(ctx, sqlcgen.UpdateSandboxSnapshotIDParams{SessionID: sessionID, SnapshotID: &snapshotID})
			if err != nil {
				t.Fatalf("UpdateSnapshotID: %v", err)
			}
			got := row.SnapshotSkippedRepoHooksID
			if tc.want && (got == nil || *got != snapshotID) {
				t.Fatalf("snapshot_skipped_repo_hooks_id = %v, want %s: the snapshot was minted without the hooks", got, snapshotID)
			}
			if !tc.want && got != nil {
				t.Fatalf("snapshot_skipped_repo_hooks_id = %s, want NULL: the snapshot's gen did not boot without the hooks", *got)
			}
			if n, err := store.ClearSnapshot(ctx, sessionID, snapshotID); err != nil || n != 1 {
				t.Fatalf("ClearSnapshot = %d, %v", n, err)
			}
			cleared, err := store.Get(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if cleared.SnapshotSkippedRepoHooksID != nil {
				t.Fatalf("snapshot_skipped_repo_hooks_id = %s after the clear, want NULL", *cleared.SnapshotSkippedRepoHooksID)
			}
		})
	}

	t.Run("a snapshot the previous binary mints never takes an earlier snapshot's record", func(t *testing.T) {
		sessionID := createTestSession(ctx, t, pool)
		if _, err := store.Create(ctx, sessionID); err != nil {
			t.Fatal(err)
		}
		if err := store.RecordRepoHooksTold(ctx, sessionID, 1, true); err != nil {
			t.Fatal(err)
		}
		if err := store.RecordReady(ctx, sessionID, 1, false, false, true, nil); err != nil {
			t.Fatal(err)
		}
		first := "snap-this-release"
		if _, err := store.UpdateSnapshotID(ctx, sqlcgen.UpdateSandboxSnapshotIDParams{SessionID: sessionID, SnapshotID: &first}); err != nil {
			t.Fatal(err)
		}
		// The previous binary spawns the next gen and mints its snapshot,
		// each with its own statement, which names none of 000175's columns.
		if _, err := pool.Exec(ctx, rotationReleaseUpsertSandboxForSpawn, sessionID, token, nil); err != nil {
			t.Fatalf("the previous binary's UpsertSandboxForSpawn: %v", err)
		}
		second, protocol := "snap-previous-binary", "1.28.0"
		if _, err := pool.Exec(ctx, previousReleaseUpdateSandboxSnapshotID, second, false, protocol, nil, sessionID); err != nil {
			t.Fatalf("the previous binary's UpdateSandboxSnapshotID: %v", err)
		}
		row, err := store.Get(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if row.SnapshotID == nil || *row.SnapshotID != second || row.SnapshotProvenanceID == nil || *row.SnapshotProvenanceID != second {
			t.Fatalf("snapshot %v, provenance %v; want %s recorded with its provenance", row.SnapshotID, row.SnapshotProvenanceID, second)
		}
		if got := row.SnapshotSkippedRepoHooksID; got != nil && *got == second {
			t.Fatalf("snapshot_skipped_repo_hooks_id = %s: the previous binary's snapshot reads as minted without the hooks", *got)
		}
	})

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

	t.Run("a resume of a gen the previous binary spawned carries nothing", func(t *testing.T) {
		// Gen 1 told; the previous binary spawns gen 2, recording nothing,
		// so the told record still names gen 1; this release resumes gen 2
		// as gen 3, which carries what gen 2 was told: nothing.
		sessionID := createTestSession(ctx, t, pool)
		if _, err := store.Create(ctx, sessionID); err != nil {
			t.Fatal(err)
		}
		if err := store.RecordRepoHooksTold(ctx, sessionID, 1, true); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if _, err := store.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID, TokenHash: &token}); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.CarryRepoHooksTold(ctx, sessionID, 2, 3); err != nil {
			t.Fatal(err)
		}
		row, err := store.Get(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if row.Gen != 3 || row.SkipRepoHooksToldGen != nil {
			t.Fatalf("gen %d, told gen %v; want gen 3 told nothing: the gen it resumes was told nothing", row.Gen, row.SkipRepoHooksToldGen)
		}
	})
}

// previousReleaseUpdateSandboxSnapshotID is UpdateSandboxSnapshotID as the
// previous release sends it, copied byte for byte from main's generated
// sqlcgen (8c6a438f): it records a snapshot's provenance under its id
// (000170) and names none of 000175's columns.
const previousReleaseUpdateSandboxSnapshotID = `-- name: UpdateSandboxSnapshotID :one
UPDATE sandboxes
SET snapshot_id = $1,
    snapshot_suppressed_in_shadow = $2,
    pending_snapshot_message_id = NULL,
    snapshot_started_at = NULL,
    snapshot_start_message_id = NULL,
    snapshot_provenance_id = $1,
    snapshot_agent_protocol = $3,
    snapshot_runtime_version = $4,
    snapshot_minted_at = now(),
    updated_at = now()
WHERE session_id = $5
RETURNING id, session_id, gen, status, last_seen_at, created_at, updated_at, token_hash, provider_id, spawn_failure_count, last_spawn_failure_at, snapshot_id, pending_snapshot_message_id, pre_suspect_status, snapshot_suppressed_in_shadow, pending_push_suppressed_in_shadow, pending_push_cancelled, demotion_terminate_requested_at, agent_version, image_digest, image_decision_reason, image_decision_fingerprint, pr_delivery_started_at, boot_evidence_gen, booting_since, booting_since_gen, stop_retire_gen, prompt_receipt_gen, ready_seq, agent_max_frame_bytes, agent_max_frame_bytes_gen, lifetime_deadline_at, lifetime_seconds, lifetime_deadline_gen, review_checkout_gen, snapshot_provenance_id, snapshot_agent_protocol, snapshot_runtime_version, snapshot_minted_at, rotation_gen, rotation_message_id, rotation_started_at, snapshot_started_at, snapshot_start_message_id, heartbeat_ready_seq, heartbeats_since_ready
`
