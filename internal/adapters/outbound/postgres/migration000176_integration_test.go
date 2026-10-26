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

// This file runs the turns_reenqueued_sandbox_gen migration through
// golang-migrate against real Postgres, in a database of its own migrated
// to the version before it. The up adds a nullable integer to every turn
// with no backfill, so a turn that exists when it runs reads NULL -- never
// re-sent, its gen kept when its deadline fails it (technical plan §3.3's
// turn recovery, §35.4); the down drops it, guarded, and runs again once
// it is gone. It also pins what the file's "Rolling back" section says:
// the previous binary cannot boot on it, and forcing the version back
// keeps the column and its values for this release's next deploy.

// reenqueuedGenMigration is this migration's version.
const reenqueuedGenMigration = 176

func TestMigrationTurnsReenqueuedSandboxGen_UpAndDown(t *testing.T) {
	ctx := context.Background()
	previousVersion := versionBefore(t, reenqueuedGenMigration)
	connStr, db := migrationTestDatabase(ctx, t, previousVersion)

	column := func() (exists, nullable bool, dataType string) {
		t.Helper()
		var isNullable string
		err := db.QueryRowContext(ctx, `SELECT is_nullable, data_type FROM information_schema.columns
			WHERE table_name = 'turns' AND column_name = 'reenqueued_sandbox_gen'`).Scan(&isNullable, &dataType)
		if errors.Is(err, sql.ErrNoRows) {
			return false, false, ""
		}
		if err != nil {
			t.Fatalf("read the column: %v", err)
		}
		return true, isNullable == "YES", dataType
	}
	var sessionID string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source, repos) VALUES ('web', '[]'::jsonb) RETURNING id::text`).Scan(&sessionID); err != nil {
		t.Fatalf("insert a session: %v", err)
	}
	var turnID string
	if err := db.QueryRowContext(ctx, `INSERT INTO turns (session_id, status, dispatched_sandbox_gen) VALUES ($1::uuid, 'processing', 1) RETURNING id::text`, sessionID).Scan(&turnID); err != nil {
		t.Fatalf("insert a turn: %v", err)
	}
	value := func() sql.NullInt64 {
		t.Helper()
		var got sql.NullInt64
		if err := db.QueryRowContext(ctx, `SELECT reenqueued_sandbox_gen FROM turns WHERE id = $1::uuid`, turnID).Scan(&got); err != nil {
			t.Fatalf("read the turn's reenqueued_sandbox_gen: %v", err)
		}
		return got
	}
	if exists, _, _ := column(); exists {
		t.Fatalf("reenqueued_sandbox_gen exists at %d, want it absent", previousVersion)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(reenqueuedGenMigration); err != nil {
		t.Fatalf("up to %d: %v", reenqueuedGenMigration, err)
	}
	if exists, nullable, dataType := column(); !exists || !nullable || dataType != "integer" {
		t.Fatalf("reenqueued_sandbox_gen after the up: exists %v, nullable %v, type %q; want a nullable integer", exists, nullable, dataType)
	}
	if got := value(); got.Valid {
		t.Fatalf("an existing turn's reenqueued_sandbox_gen = %d after the up, want NULL: nothing is backfilled", got.Int64)
	}
	if _, err := db.ExecContext(ctx, `UPDATE turns SET dispatched_sandbox_gen = 2, reenqueued_sandbox_gen = 2 WHERE id = $1::uuid`, turnID); err != nil {
		t.Fatalf("record a re-send: %v", err)
	}

	// The previous binary cannot boot on this version.
	previous, pdb := previousBinaryMigrate(t, connStr, int(previousVersion))
	if err := previous.Up(); err == nil || !strings.Contains(err.Error(), fmt.Sprint(reenqueuedGenMigration)) {
		t.Fatalf("the previous binary's boot on %d = %v, want a refusal naming %d", reenqueuedGenMigration, err, reenqueuedGenMigration)
	}
	_ = pdb.Close()

	// Rolling back with the column kept: force the previous version; this
	// release's migration, run again, keeps the column and its value.
	if err := m.Force(int(previousVersion)); err != nil {
		t.Fatalf("force %d: %v", previousVersion, err)
	}
	if err := m.Migrate(reenqueuedGenMigration); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("up to %d again: %v", reenqueuedGenMigration, err)
	}
	if got := value(); !got.Valid {
		t.Fatal("the turn's reenqueued_sandbox_gen after the up ran again = NULL, want its value kept")
	}

	// The down drops the column, and runs again once it is gone.
	if err := m.Migrate(previousVersion); err != nil {
		t.Fatalf("down to %d: %v", previousVersion, err)
	}
	if exists, _, _ := column(); exists {
		t.Fatal("reenqueued_sandbox_gen still exists after the down")
	}
	down, err := fs.ReadFile(migrations.FS, fmt.Sprintf("%06d_turns_reenqueued_sandbox_gen.down.sql", reenqueuedGenMigration))
	if err != nil {
		t.Fatalf("read the down: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(down)); err != nil {
		t.Fatalf("the down, run again once the column is gone: %v", err)
	}
	if err := m.Migrate(reenqueuedGenMigration); err != nil {
		t.Fatalf("up to %d after the down: %v", reenqueuedGenMigration, err)
	}
	if got := value(); got.Valid {
		t.Fatalf("the turn's reenqueued_sandbox_gen after a down and an up = %d, want NULL", got.Int64)
	}
}
