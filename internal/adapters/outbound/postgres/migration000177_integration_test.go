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

// This file runs the identities_email_checked_at migration through
// golang-migrate against real Postgres, in a database of its own migrated
// to the version before it. The up adds one nullable timestamptz column to
// every identity with no default and no backfill, so an identity that
// exists when it runs -- a Slack automatic link the previous binary made
// among them -- reads as never checked under §13.2's definition of a
// verified email; an INSERT that does not name the column, as the previous
// binary's does, leaves it NULL too. The down drops it, guarded, and runs
// again once it is gone. It also pins what the file's "Rolling back"
// section says: the previous binary cannot boot on it, and forcing the
// version back keeps the column and its values for this release's next
// deploy.

// emailCheckedAtMigration is this migration's version.
const emailCheckedAtMigration = 177

func TestMigrationIdentitiesEmailCheckedAt_UpAndDown(t *testing.T) {
	ctx := context.Background()
	previousVersion := versionBefore(t, emailCheckedAtMigration)
	connStr, db := migrationTestDatabase(ctx, t, previousVersion)

	column := func() (exists, nullable bool, dataType string, hasDefault bool) {
		t.Helper()
		var isNullable string
		var columnDefault sql.NullString
		err := db.QueryRowContext(ctx, `SELECT is_nullable, data_type, column_default FROM information_schema.columns
			WHERE table_name = 'identities' AND column_name = 'email_checked_at'`).Scan(&isNullable, &dataType, &columnDefault)
		if errors.Is(err, sql.ErrNoRows) {
			return false, false, "", false
		}
		if err != nil {
			t.Fatalf("read the column: %v", err)
		}
		return true, isNullable == "YES", dataType, columnDefault.Valid
	}
	checkedAt := func(externalID string) sql.NullTime {
		t.Helper()
		var at sql.NullTime
		if err := db.QueryRowContext(ctx, `SELECT email_checked_at FROM identities WHERE provider = 'slack' AND external_id = $1`, externalID).Scan(&at); err != nil {
			t.Fatalf("read %s's email_checked_at: %v", externalID, err)
		}
		return at
	}

	var userID string
	if err := db.QueryRowContext(ctx, `INSERT INTO users (primary_email, display_name, role) VALUES ('migration-177@example.com', 'M', 'member') RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatalf("insert a user: %v", err)
	}
	// What the previous binary wrote for a Slack automatic link.
	if _, err := db.ExecContext(ctx, `INSERT INTO identities (user_id, provider, external_id, email, email_verified, linked_via)
		VALUES ($1::uuid, 'slack', 'U-BEFORE', 'migration-177@example.com', true, 'auto_email')`, userID); err != nil {
		t.Fatalf("insert an automatic link: %v", err)
	}
	if exists, _, _, _ := column(); exists {
		t.Fatalf("email_checked_at exists at %d, want it absent", previousVersion)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(emailCheckedAtMigration); err != nil {
		t.Fatalf("up to %d: %v", emailCheckedAtMigration, err)
	}
	if exists, nullable, dataType, hasDefault := column(); !exists || !nullable || dataType != "timestamp with time zone" || hasDefault {
		t.Fatalf("email_checked_at after the up: exists %v, nullable %v, type %q, default %v; want a nullable timestamptz with no default", exists, nullable, dataType, hasDefault)
	}
	if at := checkedAt("U-BEFORE"); at.Valid {
		t.Fatalf("an existing automatic link's email_checked_at after the up = %v, want NULL: nothing is backfilled", at)
	}

	// The previous binary's INSERT names no new column: its link reads as
	// never checked.
	if _, err := db.ExecContext(ctx, `INSERT INTO identities (user_id, provider, external_id, email, email_verified, linked_via, access_token_encrypted)
		VALUES ($1::uuid, 'slack', 'U-ROLLOUT', 'migration-177@example.com', true, 'auto_email', NULL)`, userID); err != nil {
		t.Fatalf("insert as the previous binary does: %v", err)
	}
	if at := checkedAt("U-ROLLOUT"); at.Valid {
		t.Fatalf("a link the previous binary makes during the rollout = %v, want NULL", at)
	}
	if _, err := db.ExecContext(ctx, `UPDATE identities SET email_checked_at = now() WHERE external_id = 'U-BEFORE'`); err != nil {
		t.Fatalf("stamp a link: %v", err)
	}

	// The previous binary cannot boot on this version.
	previous, pdb := previousBinaryMigrate(t, connStr, int(previousVersion))
	if err := previous.Up(); err == nil || !strings.Contains(err.Error(), fmt.Sprint(emailCheckedAtMigration)) {
		t.Fatalf("the previous binary's boot on %d = %v, want a refusal naming %d", emailCheckedAtMigration, err, emailCheckedAtMigration)
	}
	_ = pdb.Close()

	// Rolling back with the column kept: force the previous version; this
	// release's migration, run again, keeps the column and its values.
	if err := m.Force(int(previousVersion)); err != nil {
		t.Fatalf("force %d: %v", previousVersion, err)
	}
	if err := m.Migrate(emailCheckedAtMigration); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("up to %d again: %v", emailCheckedAtMigration, err)
	}
	if at := checkedAt("U-BEFORE"); !at.Valid {
		t.Fatal("a stamped link after the up ran again reads NULL, want the stamp kept")
	}

	// The down drops the column, and runs again once it is gone.
	if err := m.Migrate(previousVersion); err != nil {
		t.Fatalf("down to %d: %v", previousVersion, err)
	}
	if exists, _, _, _ := column(); exists {
		t.Fatal("email_checked_at still exists after the down")
	}
	down, err := fs.ReadFile(migrations.FS, fmt.Sprintf("%06d_identities_email_checked_at.down.sql", emailCheckedAtMigration))
	if err != nil {
		t.Fatalf("read the down: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(down)); err != nil {
		t.Fatalf("the down, run again once the column is gone: %v", err)
	}
	if err := m.Migrate(emailCheckedAtMigration); err != nil {
		t.Fatalf("up to %d after the down: %v", emailCheckedAtMigration, err)
	}
	if at := checkedAt("U-BEFORE"); at.Valid {
		t.Fatalf("a link after a down and an up = %v, want NULL: every Slack automatic link is checked again", at)
	}
}
