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

// This file runs the github_pr_sessions_head_in_base migration through
// golang-migrate against real Postgres, in a database of its own migrated
// to the version before it. The up adds a nullable boolean to every claim
// with no backfill, so a claim that exists when it runs reads NULL --
// unknown, which the review-head rule reads as not in the base until the
// pull request's next mention (technical plan §27.1); the down drops it,
// guarded, and runs again once it is gone. It also pins what the file's
// "Rolling back" section says: the previous binary cannot boot on it, and
// forcing the version back keeps the column and its values for this
// release's next deploy.

// headInBaseMigration is this migration's version.
const headInBaseMigration = 173

func TestMigrationGitHubPRSessionsHeadInBase_UpAndDown(t *testing.T) {
	ctx := context.Background()
	previousVersion := versionBefore(t, headInBaseMigration)
	connStr, db := migrationTestDatabase(ctx, t, previousVersion)

	column := func() (exists, nullable bool, dataType string) {
		t.Helper()
		var isNullable string
		err := db.QueryRowContext(ctx, `SELECT is_nullable, data_type FROM information_schema.columns
			WHERE table_name = 'github_pr_sessions' AND column_name = 'head_in_base'`).Scan(&isNullable, &dataType)
		if errors.Is(err, sql.ErrNoRows) {
			return false, false, ""
		}
		if err != nil {
			t.Fatalf("read the column: %v", err)
		}
		return true, isNullable == "YES", dataType
	}
	record := func() sql.NullBool {
		t.Helper()
		var got sql.NullBool
		if err := db.QueryRowContext(ctx, `SELECT head_in_base FROM github_pr_sessions WHERE repo_full_name = 'acme/widgets' AND pr_number = 7`).Scan(&got); err != nil {
			t.Fatalf("read the claim's record: %v", err)
		}
		return got
	}
	if exists, _, _ := column(); exists {
		t.Fatalf("head_in_base exists at %d, want it absent", previousVersion)
	}
	// A claim the previous release made.
	if _, err := db.ExecContext(ctx, `INSERT INTO github_pr_sessions (repo_full_name, pr_number) VALUES ('acme/widgets', 7)`); err != nil {
		t.Fatalf("insert a claim: %v", err)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(headInBaseMigration); err != nil {
		t.Fatalf("up to %d: %v", headInBaseMigration, err)
	}
	if exists, nullable, dataType := column(); !exists || !nullable || dataType != "boolean" {
		t.Fatalf("head_in_base after the up: exists %v, nullable %v, type %q; want a nullable boolean", exists, nullable, dataType)
	}
	if got := record(); got.Valid {
		t.Fatalf("an existing claim's record = %v after the up, want NULL: nothing is backfilled", got.Bool)
	}
	if _, err := db.ExecContext(ctx, `UPDATE github_pr_sessions SET head_in_base = true WHERE repo_full_name = 'acme/widgets' AND pr_number = 7`); err != nil {
		t.Fatalf("record the head: %v", err)
	}

	// The previous binary cannot boot on this version.
	previous, pdb := previousBinaryMigrate(t, connStr, int(previousVersion))
	if err := previous.Up(); err == nil || !strings.Contains(err.Error(), fmt.Sprint(headInBaseMigration)) {
		t.Fatalf("the previous binary's boot on %d = %v, want a refusal naming %d", headInBaseMigration, err, headInBaseMigration)
	}
	_ = pdb.Close()

	// Rolling back with the column kept: force the previous version; this
	// release's migration, run again, keeps the column and its value.
	if err := m.Force(int(previousVersion)); err != nil {
		t.Fatalf("force %d: %v", previousVersion, err)
	}
	if err := m.Migrate(headInBaseMigration); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("up to %d again: %v", headInBaseMigration, err)
	}
	if got := record(); !got.Valid || !got.Bool {
		t.Fatalf("the claim's record after the up ran again = %+v, want true kept", got)
	}

	// The down drops the column, and runs again once it is gone.
	if err := m.Migrate(previousVersion); err != nil {
		t.Fatalf("down to %d: %v", previousVersion, err)
	}
	if exists, _, _ := column(); exists {
		t.Fatal("head_in_base still exists after the down")
	}
	down, err := fs.ReadFile(migrations.FS, fmt.Sprintf("%06d_github_pr_sessions_head_in_base.down.sql", headInBaseMigration))
	if err != nil {
		t.Fatalf("read the down: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(down)); err != nil {
		t.Fatalf("the down, run again once the column is gone: %v", err)
	}
	if err := m.Migrate(headInBaseMigration); err != nil {
		t.Fatalf("up to %d after the down: %v", headInBaseMigration, err)
	}
	if got := record(); got.Valid {
		t.Fatalf("the claim's record after a down and an up = %v, want NULL", got.Bool)
	}
}
