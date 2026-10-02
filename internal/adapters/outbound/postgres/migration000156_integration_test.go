//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"

	"github.com/narvidev/narvi/migrations"
)

// This file runs the repo_entitlement_revocations migration through
// golang-migrate against real Postgres, in a database of its own migrated to
// the version before it. The up creates the table of administrators'
// revocations (technical plan §31.4, "Un-entitlement"); the down drops it,
// and every revocation with it.
//
// It also pins what the migration's own "Rolling back" section says: the
// previous binary cannot boot once it is applied; forcing the version back
// keeps the rows, and this release's migration runs again over them,
// keeping them; the down drops them, and up again starts empty.

// revocationsMigration is this migration's version.
const revocationsMigration = 156

// versionBefore is the highest migration version below version in the
// embedded migrations -- the previous binary's last one. Read rather than
// computed as version-1: a parallel branch can hold the number between.
func versionBefore(t *testing.T, version int) uint {
	t.Helper()
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	best := -1
	for _, e := range entries {
		digits, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			continue
		}
		v, err := strconv.Atoi(digits)
		if err != nil || v >= version {
			continue
		}
		if v > best {
			best = v
		}
	}
	if best < 0 {
		t.Fatalf("no migration before %d", version)
	}
	return uint(best)
}

func TestMigrationRepoEntitlementRevocations_UpAndDown(t *testing.T) {
	ctx := context.Background()
	previousVersion := versionBefore(t, revocationsMigration)
	connStr, db := migrationTestDatabase(ctx, t, previousVersion)

	hasTable := func() bool {
		t.Helper()
		var exists bool
		if err := db.QueryRowContext(ctx, `SELECT to_regclass('repo_entitlement_revocations') IS NOT NULL`).Scan(&exists); err != nil {
			t.Fatalf("look up the table: %v", err)
		}
		return exists
	}
	count := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM repo_entitlement_revocations`).Scan(&n); err != nil {
			t.Fatalf("count revocations: %v", err)
		}
		return n
	}

	if hasTable() {
		t.Fatalf("repo_entitlement_revocations exists at %d, want it absent", previousVersion)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(revocationsMigration); err != nil {
		t.Fatalf("up to %d: %v", revocationsMigration, err)
	}
	if !hasTable() {
		t.Fatal("repo_entitlement_revocations missing after the up")
	}

	// The reason CHECK holds 1 to 500 characters after trimming; revoked_by
	// is optional and revoked_at defaults to now.
	var userID string
	if err := db.QueryRowContext(ctx, `INSERT INTO users (primary_email, display_name, role) VALUES ('revocations-migration@example.com', 'Admin', 'admin') RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO repo_entitlement_revocations (repo_full_name, revoked_by, reason) VALUES ('acme/widgets', $1, 'a reason')`, userID); err != nil {
		t.Fatalf("insert a revocation: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO repo_entitlement_revocations (repo_full_name, reason) VALUES ('acme/nobody', 'no user')`); err != nil {
		t.Fatalf("insert a revocation with no user: %v", err)
	}
	for _, bad := range []string{"", "  ", strings.Repeat("x", 501)} {
		_, err := db.ExecContext(ctx, `INSERT INTO repo_entitlement_revocations (repo_full_name, reason) VALUES ('acme/bad', $1)`, bad)
		if err == nil || !strings.Contains(err.Error(), "repo_entitlement_revocations_reason_check") {
			t.Fatalf("insert with a reason of %d characters = %v, want the reason CHECK to refuse it", len(bad), err)
		}
	}
	var revokedAtSet bool
	if err := db.QueryRowContext(ctx, `SELECT revoked_at IS NOT NULL FROM repo_entitlement_revocations WHERE repo_full_name = 'acme/nobody'`).Scan(&revokedAtSet); err != nil || !revokedAtSet {
		t.Fatalf("revoked_at default: set=%v err=%v, want now()", revokedAtSet, err)
	}

	// The previous binary cannot boot: golang-migrate refuses a version it
	// has no file for.
	previous, pdb := previousBinaryMigrate(t, connStr, int(previousVersion))
	if err := previous.Up(); err == nil || !strings.Contains(err.Error(), fmt.Sprint(revocationsMigration)) {
		t.Fatalf("the previous binary's boot on %d = %v, want a refusal naming %d", revocationsMigration, err, revocationsMigration)
	}
	_ = pdb.Close()

	// Rolling back with the rows kept: force the previous version, and the
	// previous binary boots. Deploying this release again runs the
	// migration again (IF NOT EXISTS), which keeps the rows.
	if err := m.Force(int(previousVersion)); err != nil {
		t.Fatalf("force %d: %v", previousVersion, err)
	}
	previous, pdb = previousBinaryMigrate(t, connStr, int(previousVersion))
	if err := previous.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("the previous binary's boot after the force = %v, want no change", err)
	}
	_ = pdb.Close()
	again, adb := newMigrate(t, connStr)
	if err := again.Migrate(revocationsMigration); err != nil {
		t.Fatalf("this release's migration after the rollback = %v, want it applied again", err)
	}
	_ = adb.Close()
	assertCleanVersion(t, connStr, revocationsMigration)
	if n := count(); n != 2 {
		t.Fatalf("%d revocations after the migration ran again, want the 2 kept", n)
	}

	// Down: the table goes, every revocation with it.
	if err := m.Migrate(previousVersion); err != nil {
		t.Fatalf("down to %d: %v", previousVersion, err)
	}
	if hasTable() {
		t.Fatal("repo_entitlement_revocations still exists after the down")
	}

	// Up again works, on an empty table.
	if err := m.Migrate(revocationsMigration); err != nil {
		t.Fatalf("up to %d again: %v", revocationsMigration, err)
	}
	assertCleanVersion(t, connStr, revocationsMigration)
	if n := count(); n != 0 {
		t.Fatalf("%d revocations after down and up, want none", n)
	}
}
