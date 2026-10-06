//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// This file runs the platform_settings migration through golang-migrate
// against real Postgres, in a database of its own migrated to the version
// before it. The up creates the one row of platform-wide settings
// (technical plan §40.2), seeded and not frozen; the down drops it.
//
// It also pins the shape the row's constraints keep -- a freeze always
// carries when and why, an unfrozen row carries neither, one row only --
// and what the migration's own "Rolling back" section says: the previous
// binary cannot boot once it is applied; forcing the version back keeps
// the row, and this release's migration runs again over it, keeping a
// freeze in force; the down drops it, and up again seeds it unfrozen.

// platformSettingsMigration is this migration's version.
const platformSettingsMigration = 163

func TestMigrationPlatformSettings_SeededAndShaped(t *testing.T) {
	ctx := context.Background()
	previousVersion := versionBefore(t, platformSettingsMigration)
	connStr, db := migrationTestDatabase(ctx, t, previousVersion)

	hasTable := func() bool {
		t.Helper()
		var exists bool
		if err := db.QueryRowContext(ctx, `SELECT to_regclass('platform_settings') IS NOT NULL`).Scan(&exists); err != nil {
			t.Fatalf("look up the table: %v", err)
		}
		return exists
	}
	// row reads the one row: how many rows there are, and whether the
	// first is frozen.
	row := func() (rows int, frozen bool) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT count(*), coalesce(bool_or(autonomy_frozen), false) FROM platform_settings`).Scan(&rows, &frozen); err != nil {
			t.Fatalf("read platform_settings: %v", err)
		}
		return rows, frozen
	}

	if hasTable() {
		t.Fatalf("platform_settings exists at %d, want it absent", previousVersion)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(platformSettingsMigration); err != nil {
		t.Fatalf("up to %d: %v", platformSettingsMigration, err)
	}
	if !hasTable() {
		t.Fatal("platform_settings missing after the up")
	}
	if n, frozen := row(); n != 1 || frozen {
		t.Fatalf("after the up: %d rows, frozen %v; want the one row seeded, not frozen", n, frozen)
	}
	var idOne, shapeNull bool
	if err := db.QueryRowContext(ctx, `SELECT id = 1, autonomy_frozen_at IS NULL AND autonomy_frozen_by IS NULL AND autonomy_freeze_reason IS NULL FROM platform_settings`).Scan(&idOne, &shapeNull); err != nil || !idOne || !shapeNull {
		t.Fatalf("seeded row: id 1 %v, no freeze details %v (err %v); want both", idOne, shapeNull, err)
	}

	var userID string
	if err := db.QueryRowContext(ctx, `INSERT INTO users (primary_email, display_name, role) VALUES ('freeze-migration@example.com', 'Admin', 'admin') RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	// The shape: refused writes leave the row as it was.
	for _, tc := range []struct {
		name, stmt string
		args       []any
		constraint string
	}{
		{"a second row", `INSERT INTO platform_settings (id) VALUES (2)`, nil, "platform_settings_singleton"},
		{"frozen with no reason", `UPDATE platform_settings SET autonomy_frozen = true, autonomy_frozen_at = now()`, nil, "platform_settings_freeze_shape"},
		{"frozen with a blank reason", `UPDATE platform_settings SET autonomy_frozen = true, autonomy_frozen_at = now(), autonomy_freeze_reason = '   '`, nil, "platform_settings_freeze_shape"},
		{"frozen with a reason over 500 characters", `UPDATE platform_settings SET autonomy_frozen = true, autonomy_frozen_at = now(), autonomy_freeze_reason = $1`, []any{strings.Repeat("x", 501)}, "platform_settings_freeze_shape"},
		{"frozen with no time", `UPDATE platform_settings SET autonomy_frozen = true, autonomy_freeze_reason = 'why'`, nil, "platform_settings_freeze_shape"},
		{"not frozen but with a reason", `UPDATE platform_settings SET autonomy_freeze_reason = 'why'`, nil, "platform_settings_freeze_shape"},
		{"not frozen but with a freezer", `UPDATE platform_settings SET autonomy_frozen_by = $1`, []any{userID}, "platform_settings_freeze_shape"},
	} {
		if _, err := db.ExecContext(ctx, tc.stmt, tc.args...); err == nil || !strings.Contains(err.Error(), tc.constraint) {
			t.Fatalf("%s = %v, want %s to refuse it", tc.name, err, tc.constraint)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE platform_settings SET autonomy_frozen = true, autonomy_frozen_at = now(), autonomy_frozen_by = $1, autonomy_freeze_reason = $2`, userID, strings.Repeat("y", 500)); err != nil {
		t.Fatalf("freeze with a 500-character reason: %v", err)
	}
	// The freezer's user deleted: the freeze stays, with no freezer.
	if _, err := db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatalf("delete the freezer: %v", err)
	}
	var byNull bool
	if err := db.QueryRowContext(ctx, `SELECT autonomy_frozen_by IS NULL FROM platform_settings`).Scan(&byNull); err != nil || !byNull {
		t.Fatalf("freezer after their user was deleted: NULL %v (err %v), want NULL", byNull, err)
	}
	if _, frozen := row(); !frozen {
		t.Fatal("the freeze was lifted when its freezer's user was deleted")
	}

	// The previous binary cannot boot: golang-migrate refuses a version it
	// has no file for.
	previous, pdb := previousBinaryMigrate(t, connStr, int(previousVersion))
	if err := previous.Up(); err == nil || !strings.Contains(err.Error(), fmt.Sprint(platformSettingsMigration)) {
		t.Fatalf("the previous binary's boot on %d = %v, want a refusal naming %d", platformSettingsMigration, err, platformSettingsMigration)
	}
	_ = pdb.Close()

	// Rolling back with the row kept: force the previous version, and the
	// previous binary boots. Deploying this release again runs the
	// migration again, which keeps the row -- and the freeze in force.
	if err := m.Force(int(previousVersion)); err != nil {
		t.Fatalf("force %d: %v", previousVersion, err)
	}
	previous, pdb = previousBinaryMigrate(t, connStr, int(previousVersion))
	if err := previous.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("the previous binary's boot after the force = %v, want no change", err)
	}
	_ = pdb.Close()
	again, adb := newMigrate(t, connStr)
	if err := again.Migrate(platformSettingsMigration); err != nil {
		t.Fatalf("this release's migration after the rollback = %v, want it applied again", err)
	}
	_ = adb.Close()
	assertCleanVersion(t, connStr, platformSettingsMigration)
	if n, frozen := row(); n != 1 || !frozen {
		t.Fatalf("after the migration ran again: %d rows, frozen %v; want the one row, still frozen", n, frozen)
	}

	// Down: the table goes, the freeze with it.
	if err := m.Migrate(previousVersion); err != nil {
		t.Fatalf("down to %d: %v", previousVersion, err)
	}
	if hasTable() {
		t.Fatal("platform_settings still exists after the down")
	}

	// Up again seeds the row, not frozen.
	if err := m.Migrate(platformSettingsMigration); err != nil {
		t.Fatalf("up to %d again: %v", platformSettingsMigration, err)
	}
	assertCleanVersion(t, connStr, platformSettingsMigration)
	if n, frozen := row(); n != 1 || frozen {
		t.Fatalf("after down and up: %d rows, frozen %v; want the one row seeded, not frozen", n, frozen)
	}
}
