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

// This file runs the turn_continuity_recap migration through
// golang-migrate against real Postgres, in a database of its own migrated
// to the version before it. The up adds two nullable columns to every turn
// with no backfill -- continuity_recap, text, and continuity_recap_gen, an
// integer -- so a turn that exists when it runs reads as having carried no
// recap (technical plan §35.5); the down drops them, guarded, and runs
// again once they are gone. It also pins what the file's "Rolling back"
// section says: the previous binary cannot boot on it, and forcing the
// version back keeps the columns and their values for this release's next
// deploy.

// continuityRecapMigration is this migration's version.
const continuityRecapMigration = 178

func TestMigrationTurnContinuityRecap_UpAndDown(t *testing.T) {
	ctx := context.Background()
	previousVersion := versionBefore(t, continuityRecapMigration)
	connStr, db := migrationTestDatabase(ctx, t, previousVersion)

	column := func(name string) (exists, nullable bool, dataType string) {
		t.Helper()
		var isNullable string
		err := db.QueryRowContext(ctx, `SELECT is_nullable, data_type FROM information_schema.columns
			WHERE table_name = 'turns' AND column_name = $1`, name).Scan(&isNullable, &dataType)
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
	if err := db.QueryRowContext(ctx, `INSERT INTO turns (session_id, status, dispatched_sandbox_gen) VALUES ($1::uuid, 'processing', 2) RETURNING id::text`, sessionID).Scan(&turnID); err != nil {
		t.Fatalf("insert a turn: %v", err)
	}
	columns := map[string]string{"continuity_recap": "text", "continuity_recap_gen": "integer"}
	value := func() (recap sql.NullString, gen sql.NullInt64) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT continuity_recap, continuity_recap_gen FROM turns WHERE id = $1::uuid`, turnID).Scan(&recap, &gen); err != nil {
			t.Fatalf("read the turn's recap columns: %v", err)
		}
		return recap, gen
	}
	for name := range columns {
		if exists, _, _ := column(name); exists {
			t.Fatalf("%s exists at %d, want it absent", name, previousVersion)
		}
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(continuityRecapMigration); err != nil {
		t.Fatalf("up to %d: %v", continuityRecapMigration, err)
	}
	for name, want := range columns {
		if exists, nullable, dataType := column(name); !exists || !nullable || dataType != want {
			t.Fatalf("%s after the up: exists %v, nullable %v, type %q; want a nullable %s", name, exists, nullable, dataType, want)
		}
	}
	if recap, gen := value(); recap.Valid || gen.Valid {
		t.Fatalf("an existing turn's recap after the up = %v, %v; want NULL, NULL: nothing is backfilled", recap, gen)
	}
	if _, err := db.ExecContext(ctx, `UPDATE turns SET continuity_recap = 'the recap kept', continuity_recap_gen = 2 WHERE id = $1::uuid`, turnID); err != nil {
		t.Fatalf("record a recap: %v", err)
	}

	// The previous binary cannot boot on this version.
	previous, pdb := previousBinaryMigrate(t, connStr, int(previousVersion))
	if err := previous.Up(); err == nil || !strings.Contains(err.Error(), fmt.Sprint(continuityRecapMigration)) {
		t.Fatalf("the previous binary's boot on %d = %v, want a refusal naming %d", continuityRecapMigration, err, continuityRecapMigration)
	}
	_ = pdb.Close()

	// Rolling back with the columns kept: force the previous version; this
	// release's migration, run again, keeps the columns and their values.
	if err := m.Force(int(previousVersion)); err != nil {
		t.Fatalf("force %d: %v", previousVersion, err)
	}
	if err := m.Migrate(continuityRecapMigration); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("up to %d again: %v", continuityRecapMigration, err)
	}
	if recap, gen := value(); recap.String != "the recap kept" || !gen.Valid || gen.Int64 != 2 {
		t.Fatalf("the turn's recap after the up ran again = %v, %v; want the values kept", recap, gen)
	}

	// The down drops the columns, and runs again once they are gone.
	if err := m.Migrate(previousVersion); err != nil {
		t.Fatalf("down to %d: %v", previousVersion, err)
	}
	for name := range columns {
		if exists, _, _ := column(name); exists {
			t.Fatalf("%s still exists after the down", name)
		}
	}
	down, err := fs.ReadFile(migrations.FS, fmt.Sprintf("%06d_turn_continuity_recap.down.sql", continuityRecapMigration))
	if err != nil {
		t.Fatalf("read the down: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(down)); err != nil {
		t.Fatalf("the down, run again once the columns are gone: %v", err)
	}
	if err := m.Migrate(continuityRecapMigration); err != nil {
		t.Fatalf("up to %d after the down: %v", continuityRecapMigration, err)
	}
	if recap, gen := value(); recap.Valid || gen.Valid {
		t.Fatalf("the turn's recap after a down and an up = %v, %v; want NULL, NULL", recap, gen)
	}
}
