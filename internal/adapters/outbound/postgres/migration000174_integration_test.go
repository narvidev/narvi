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

// This file runs the sandbox_repo_hooks_lineage migration through
// golang-migrate against real Postgres, in a database of its own migrated
// to the version before it. The up adds two nullable columns to every
// sandbox with no backfill -- skip_repo_hooks_told_gen, an integer, and
// snapshot_skipped_repo_hooks, a boolean -- so a sandbox that exists when
// it runs reads told nothing and its snapshot not minted without the hooks
// (technical plan §6.4, §27.1); the down drops them, guarded, and runs
// again once they are gone. It also pins what the file's "Rolling back"
// section says: the previous binary cannot boot on it, and forcing the
// version back keeps the columns and their values for this release's next
// deploy.

// repoHooksLineageMigration is this migration's version.
const repoHooksLineageMigration = 174

func TestMigrationSandboxRepoHooksLineage_UpAndDown(t *testing.T) {
	ctx := context.Background()
	previousVersion := versionBefore(t, repoHooksLineageMigration)
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
	columns := map[string]string{"skip_repo_hooks_told_gen": "integer", "snapshot_skipped_repo_hooks": "boolean"}
	value := func() (told sql.NullInt64, skipped sql.NullBool) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT skip_repo_hooks_told_gen, snapshot_skipped_repo_hooks FROM sandboxes WHERE session_id = $1::uuid`, sessionID).Scan(&told, &skipped); err != nil {
			t.Fatalf("read the sandbox's lineage columns: %v", err)
		}
		return told, skipped
	}
	for name := range columns {
		if exists, _, _ := column(name); exists {
			t.Fatalf("%s exists at %d, want it absent", name, previousVersion)
		}
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(repoHooksLineageMigration); err != nil {
		t.Fatalf("up to %d: %v", repoHooksLineageMigration, err)
	}
	for name, want := range columns {
		if exists, nullable, dataType := column(name); !exists || !nullable || dataType != want {
			t.Fatalf("%s after the up: exists %v, nullable %v, type %q; want a nullable %s", name, exists, nullable, dataType, want)
		}
	}
	if told, skipped := value(); told.Valid || skipped.Valid {
		t.Fatalf("an existing sandbox's lineage after the up = %v, %v; want NULL, NULL: nothing is backfilled", told, skipped)
	}
	if _, err := db.ExecContext(ctx, `UPDATE sandboxes SET skip_repo_hooks_told_gen = gen, snapshot_skipped_repo_hooks = true WHERE session_id = $1::uuid`, sessionID); err != nil {
		t.Fatalf("record the lineage: %v", err)
	}

	// The previous binary cannot boot on this version.
	previous, pdb := previousBinaryMigrate(t, connStr, int(previousVersion))
	if err := previous.Up(); err == nil || !strings.Contains(err.Error(), fmt.Sprint(repoHooksLineageMigration)) {
		t.Fatalf("the previous binary's boot on %d = %v, want a refusal naming %d", repoHooksLineageMigration, err, repoHooksLineageMigration)
	}
	_ = pdb.Close()

	// Rolling back with the column kept: force the previous version; this
	// release's migration, run again, keeps the column and its value.
	if err := m.Force(int(previousVersion)); err != nil {
		t.Fatalf("force %d: %v", previousVersion, err)
	}
	if err := m.Migrate(repoHooksLineageMigration); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("up to %d again: %v", repoHooksLineageMigration, err)
	}
	if told, skipped := value(); !told.Valid || !skipped.Valid || !skipped.Bool {
		t.Fatalf("the sandbox's lineage after the up ran again = %v, %v; want the values kept", told, skipped)
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
	down, err := fs.ReadFile(migrations.FS, fmt.Sprintf("%06d_sandbox_repo_hooks_lineage.down.sql", repoHooksLineageMigration))
	if err != nil {
		t.Fatalf("read the down: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(down)); err != nil {
		t.Fatalf("the down, run again once the column is gone: %v", err)
	}
	if err := m.Migrate(repoHooksLineageMigration); err != nil {
		t.Fatalf("up to %d after the down: %v", repoHooksLineageMigration, err)
	}
	if told, skipped := value(); told.Valid || skipped.Valid {
		t.Fatalf("the sandbox's lineage after a down and an up = %v, %v; want NULL, NULL", told, skipped)
	}
}
