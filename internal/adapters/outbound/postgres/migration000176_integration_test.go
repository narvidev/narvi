//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"

	"github.com/narvidev/narvi/migrations"
)

// This file runs the snapshot_conversation migration through
// golang-migrate against real Postgres, in a database of its own migrated
// to the version before it. The up adds two nullable text columns to every
// sandbox with no backfill -- snapshot_conversation_id, the conversation
// the session held when the snapshot was recorded, and
// snapshot_conversation_recorded_id, the snapshot id it describes -- so a
// snapshot that exists when it runs reads "not recorded" (technical plan
// §35.5); the down drops them, guarded, and runs again once they are gone.
// It also pins what the file's "Rolling deploy" and "Rolling back" sections
// say: the previous binary's mint, copied below as it sends it, leaves the
// record naming the snapshot before, never the new one; the previous
// binary cannot boot on this version; and forcing the version back keeps
// the columns and their values for this release's next deploy.

// snapshotConversationMigration is this migration's version.
const snapshotConversationMigration = 176

// preConversationUpdateSnapshot is UpdateSandboxSnapshotID as the previous
// binary sends it, without its RETURNING list: snapshot_id and its
// provenance move, the conversation record does not.
const preConversationUpdateSnapshot = `UPDATE sandboxes
SET snapshot_id = $1,
    snapshot_suppressed_in_shadow = $2,
    pending_snapshot_message_id = NULL,
    snapshot_started_at = NULL,
    snapshot_start_message_id = NULL,
    snapshot_provenance_id = $1,
    snapshot_agent_protocol = $3,
    snapshot_runtime_version = $4,
    snapshot_skipped_repo_hooks_id = CASE WHEN skip_repo_hooks_told_gen = gen AND skip_repo_hooks_gen = gen THEN $1 END,
    snapshot_minted_at = now(),
    updated_at = now()
WHERE session_id = $5`

func TestMigrationSnapshotConversation_UpAndDown(t *testing.T) {
	ctx := context.Background()
	previousVersion := versionBefore(t, snapshotConversationMigration)
	connStr, db := migrationTestDatabase(ctx, t, previousVersion)

	column := func(name string) (exists, nullable bool, dataType string) {
		t.Helper()
		var isNullable string
		err := db.QueryRowContext(ctx, `SELECT is_nullable, data_type FROM information_schema.columns
			WHERE table_name = 'sandboxes' AND column_name = $1`, name).Scan(&isNullable, &dataType)
		if errors.Is(err, sql.ErrNoRows) {
			return false, false, ""
		}
		if err != nil {
			t.Fatalf("read the column: %v", err)
		}
		return true, isNullable == "YES", dataType
	}
	var sessionID string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source, repos, opencode_conversation_id) VALUES ('web', '[]'::jsonb, 'conv-1') RETURNING id::text`).Scan(&sessionID); err != nil {
		t.Fatalf("insert a session: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO sandboxes (session_id, snapshot_id) VALUES ($1::uuid, 'snap-before')`, sessionID); err != nil {
		t.Fatalf("insert a sandbox: %v", err)
	}
	columns := []string{"snapshot_conversation_id", "snapshot_conversation_recorded_id"}
	value := func() (snapshotID, conversation, recordedID sql.NullString) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT snapshot_id, snapshot_conversation_id, snapshot_conversation_recorded_id FROM sandboxes WHERE session_id = $1::uuid`,
			sessionID).Scan(&snapshotID, &conversation, &recordedID); err != nil {
			t.Fatalf("read the sandbox's conversation record: %v", err)
		}
		return snapshotID, conversation, recordedID
	}
	for _, name := range columns {
		if exists, _, _ := column(name); exists {
			t.Fatalf("%s exists at %d, want it absent", name, previousVersion)
		}
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(snapshotConversationMigration); err != nil {
		t.Fatalf("up to %d: %v", snapshotConversationMigration, err)
	}
	for _, name := range columns {
		if exists, nullable, dataType := column(name); !exists || !nullable || dataType != "text" {
			t.Fatalf("%s after the up: exists %v, nullable %v, type %q; want a nullable text", name, exists, nullable, dataType)
		}
	}
	if _, conversation, recordedID := value(); conversation.Valid || recordedID.Valid {
		t.Fatalf("an existing snapshot's record after the up = %v, %v; want NULL, NULL: nothing is backfilled", conversation, recordedID)
	}

	// This release records snap-1 under conv-1; the previous binary then
	// records snap-2, and the record keeps naming snap-1, so snap-2 reads
	// "not recorded" rather than minted under conv-1.
	if _, err := db.ExecContext(ctx, `UPDATE sandboxes SET snapshot_id = 'snap-1', snapshot_conversation_id = 'conv-1', snapshot_conversation_recorded_id = 'snap-1' WHERE session_id = $1::uuid`, sessionID); err != nil {
		t.Fatalf("record snap-1: %v", err)
	}
	if _, err := db.ExecContext(ctx, preConversationUpdateSnapshot, "snap-2", false, nil, nil, sessionID); err != nil {
		t.Fatalf("the previous binary's mint: %v", err)
	}
	if snapshotID, conversation, recordedID := value(); snapshotID.String != "snap-2" || conversation.String != "conv-1" || recordedID.String != "snap-1" {
		t.Fatalf("after the previous binary's mint: snapshot %v, conversation %v, recorded for %v; want snap-2, with the record left on snap-1", snapshotID, conversation, recordedID)
	}

	// The previous binary cannot boot on this version.
	previous, pdb := previousBinaryMigrate(t, connStr, int(previousVersion))
	if err := previous.Up(); err == nil || !strings.Contains(err.Error(), fmt.Sprint(snapshotConversationMigration)) {
		t.Fatalf("the previous binary's boot on %d = %v, want a refusal naming %d", snapshotConversationMigration, err, snapshotConversationMigration)
	}
	_ = pdb.Close()

	// Rolling back with the columns kept: force the previous version; this
	// release's migration, run again, keeps them and their values.
	if err := m.Force(int(previousVersion)); err != nil {
		t.Fatalf("force %d: %v", previousVersion, err)
	}
	if err := m.Migrate(snapshotConversationMigration); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("up to %d again: %v", snapshotConversationMigration, err)
	}
	if _, conversation, recordedID := value(); conversation.String != "conv-1" || recordedID.String != "snap-1" {
		t.Fatalf("the record after the up ran again = %v, %v; want the values kept", conversation, recordedID)
	}

	// The down drops the columns, and runs again once they are gone.
	if err := m.Migrate(previousVersion); err != nil {
		t.Fatalf("down to %d: %v", previousVersion, err)
	}
	for _, name := range columns {
		if exists, _, _ := column(name); exists {
			t.Fatalf("%s still exists after the down", name)
		}
	}
	down, err := fs.ReadFile(migrations.FS, fmt.Sprintf("%06d_snapshot_conversation.down.sql", snapshotConversationMigration))
	if err != nil {
		t.Fatalf("read the down: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(down)); err != nil {
		t.Fatalf("the down, run again once the columns are gone: %v", err)
	}
	if err := m.Migrate(snapshotConversationMigration); err != nil {
		t.Fatalf("up to %d after the down: %v", snapshotConversationMigration, err)
	}
	if _, conversation, recordedID := value(); conversation.Valid || recordedID.Valid {
		t.Fatalf("the record after a down and an up = %v, %v; want NULL, NULL", conversation, recordedID)
	}
}
