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

// This file runs the sandbox_fresh_lineage migration through
// golang-migrate against real Postgres, in a database of its own migrated
// to the version before it. The up adds two nullable columns to every
// sandbox with no backfill -- fresh_lineage_gen, an integer, and
// snapshot_recap_owed_id, text -- so a sandbox that exists when it runs
// reads unmarked and its snapshot as owing no recap (technical plan
// §35.5); the down drops them, guarded, and runs again once they are gone.
// It also pins what the file's "Rolling back" section says: the previous
// binary cannot boot on it, and forcing the version back keeps the columns
// and their values for this release's next deploy.

// freshLineageMigration is this migration's version.
const freshLineageMigration = 179

func TestMigrationSandboxFreshLineage_UpAndDown(t *testing.T) {
	ctx := context.Background()
	previousVersion := versionBefore(t, freshLineageMigration)
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
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source, repos) VALUES ('web', '[]'::jsonb) RETURNING id::text`).Scan(&sessionID); err != nil {
		t.Fatalf("insert a session: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO sandboxes (session_id) VALUES ($1::uuid)`, sessionID); err != nil {
		t.Fatalf("insert a sandbox: %v", err)
	}
	columns := map[string]string{"fresh_lineage_gen": "integer", "snapshot_recap_owed_id": "text"}
	value := func() (mark sql.NullInt64, owed sql.NullString) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT fresh_lineage_gen, snapshot_recap_owed_id FROM sandboxes WHERE session_id = $1::uuid`, sessionID).Scan(&mark, &owed); err != nil {
			t.Fatalf("read the sandbox's fresh-lineage columns: %v", err)
		}
		return mark, owed
	}
	for name := range columns {
		if exists, _, _ := column(name); exists {
			t.Fatalf("%s exists at %d, want it absent", name, previousVersion)
		}
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(freshLineageMigration); err != nil {
		t.Fatalf("up to %d: %v", freshLineageMigration, err)
	}
	for name, want := range columns {
		if exists, nullable, dataType := column(name); !exists || !nullable || dataType != want {
			t.Fatalf("%s after the up: exists %v, nullable %v, type %q; want a nullable %s", name, exists, nullable, dataType, want)
		}
	}
	if mark, owed := value(); mark.Valid || owed.Valid {
		t.Fatalf("an existing sandbox's columns after the up = %v, %v; want NULL, NULL: nothing is backfilled", mark, owed)
	}
	if _, err := db.ExecContext(ctx, `UPDATE sandboxes SET fresh_lineage_gen = gen, snapshot_recap_owed_id = 'snap-owed' WHERE session_id = $1::uuid`, sessionID); err != nil {
		t.Fatalf("mark the sandbox: %v", err)
	}

	// The previous binary cannot boot on this version.
	previous, pdb := previousBinaryMigrate(t, connStr, int(previousVersion))
	if err := previous.Up(); err == nil || !strings.Contains(err.Error(), fmt.Sprint(freshLineageMigration)) {
		t.Fatalf("the previous binary's boot on %d = %v, want a refusal naming %d", freshLineageMigration, err, freshLineageMigration)
	}
	_ = pdb.Close()

	// Rolling back with the column kept: force the previous version; this
	// release's migration, run again, keeps the column and its value.
	if err := m.Force(int(previousVersion)); err != nil {
		t.Fatalf("force %d: %v", previousVersion, err)
	}
	if err := m.Migrate(freshLineageMigration); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("up to %d again: %v", freshLineageMigration, err)
	}
	if mark, owed := value(); !mark.Valid || owed.String != "snap-owed" {
		t.Fatalf("the sandbox's columns after the up ran again = %v, %v; want the values kept", mark, owed)
	}

	// The down drops the column, and runs again once it is gone.
	if err := m.Migrate(previousVersion); err != nil {
		t.Fatalf("down to %d: %v", previousVersion, err)
	}
	for name := range columns {
		if exists, _, _ := column(name); exists {
			t.Fatalf("%s still exists after the down", name)
		}
	}
	down, err := fs.ReadFile(migrations.FS, fmt.Sprintf("%06d_sandbox_fresh_lineage.down.sql", freshLineageMigration))
	if err != nil {
		t.Fatalf("read the down: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(down)); err != nil {
		t.Fatalf("the down, run again once the columns are gone: %v", err)
	}
	if err := m.Migrate(freshLineageMigration); err != nil {
		t.Fatalf("up to %d after the down: %v", freshLineageMigration, err)
	}
	if mark, owed := value(); mark.Valid || owed.Valid {
		t.Fatalf("the sandbox's columns after a down and an up = %v, %v; want NULL, NULL", mark, owed)
	}
}
