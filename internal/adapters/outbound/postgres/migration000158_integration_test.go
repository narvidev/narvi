//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"testing"
)

// This file runs migration 000158 (turns_open_session_id_idx) through
// golang-migrate against real Postgres, each test in a database of its own
// inside the shared container, migrated to 157 first: up, down and up
// again; the paths an operator's own concurrent pre-build leaves behind (a
// valid index, kept; an INVALID one, rebuilt); and two migrators at once,
// which a fresh install of deploy/control-plane (replicas: 2) always has.
// What the index is for, and that the statements touching turns plan no
// worse with it, is reviewretriggerhold_plan_integration_test.go's.

// wantOpenTurnIndexDef is pg_get_indexdef of the index 000158 builds.
const wantOpenTurnIndexDef = "CREATE INDEX turns_open_session_id_idx ON public.turns USING btree (session_id) " +
	"WHERE (status <> ALL (ARRAY['completed'::turn_status, 'failed'::turn_status, 'cancelled'::turn_status]))"

// openTurnIndex reports turns_open_session_id_idx's oid, validity and
// definition, and whether it exists at all.
func openTurnIndex(ctx context.Context, t *testing.T, db *sql.DB) (oid int64, valid bool, def string, found bool) {
	t.Helper()
	err := db.QueryRowContext(ctx,
		`SELECT indexrelid::bigint, indisvalid, pg_get_indexdef(indexrelid)
		 FROM pg_index WHERE indexrelid = to_regclass('turns_open_session_id_idx')`,
	).Scan(&oid, &valid, &def)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, "", false
	}
	if err != nil {
		t.Fatalf("read turns_open_session_id_idx: %v", err)
	}
	return oid, valid, def, true
}

// assertOpenTurnIndexBuilt fails unless turns_open_session_id_idx exists, is
// valid and has the definition 000158 builds; it returns the index's oid.
func assertOpenTurnIndexBuilt(ctx context.Context, t *testing.T, db *sql.DB) int64 {
	t.Helper()
	oid, valid, def, found := openTurnIndex(ctx, t, db)
	if !found {
		t.Fatal("turns_open_session_id_idx does not exist")
	}
	if !valid {
		t.Errorf("turns_open_session_id_idx is INVALID")
	}
	if def != wantOpenTurnIndexDef {
		t.Errorf("turns_open_session_id_idx = %q, want %q", def, wantOpenTurnIndexDef)
	}
	return oid
}

func TestMigration000158_UpDownUp(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 157)
	if _, _, _, found := openTurnIndex(ctx, t, db); found {
		t.Fatal("turns_open_session_id_idx exists at 157")
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(158); err != nil {
		t.Fatalf("up to 158: %v", err)
	}
	assertOpenTurnIndexBuilt(ctx, t, db)

	// Down runs DROP INDEX CONCURRENTLY, which golang-migrate can send only
	// because the down file is a single statement.
	if err := m.Migrate(157); err != nil {
		t.Fatalf("down to 157: %v", err)
	}
	if _, _, _, found := openTurnIndex(ctx, t, db); found {
		t.Error("turns_open_session_id_idx still exists after down")
	}

	if err := m.Migrate(158); err != nil {
		t.Fatalf("up to 158 again: %v", err)
	}
	assertOpenTurnIndexBuilt(ctx, t, db)
	assertCleanVersion(t, connStr, 158)
}

// TestMigration000158_KeepsAValidPrebuiltIndex is the operator path the
// migration's comment gives for a large turns table: build the index
// concurrently by hand before deploying. The migration must then build
// nothing -- the same index, by oid, survives it.
func TestMigration000158_KeepsAValidPrebuiltIndex(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 157)
	if _, err := db.ExecContext(ctx, `CREATE INDEX CONCURRENTLY IF NOT EXISTS turns_open_session_id_idx
		ON turns (session_id) WHERE status NOT IN ('completed', 'failed', 'cancelled')`); err != nil {
		t.Fatalf("pre-build the index concurrently: %v", err)
	}
	prebuilt := assertOpenTurnIndexBuilt(ctx, t, db)

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(158); err != nil {
		t.Fatalf("up to 158: %v", err)
	}
	if got := assertOpenTurnIndexBuilt(ctx, t, db); got != prebuilt {
		t.Errorf("turns_open_session_id_idx oid = %d after the migration, want the pre-built %d (not rebuilt)", got, prebuilt)
	}
	assertCleanVersion(t, connStr, 158)
}

// TestMigration000158_APrebuiltIndexTakesNoLockOnTurns is the rest of that
// operator path: with the valid index pre-built, the migration takes no
// lock on turns at all, so it never waits behind a write to turns in
// flight, nor stalls the writes after it. The migrator runs with a short
// lock_timeout while another transaction holds an uncommitted insert into
// turns; a CREATE INDEX IF NOT EXISTS would queue for the SHARE lock on
// turns before seeing the index, and time out.
func TestMigration000158_APrebuiltIndexTakesNoLockOnTurns(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 157)
	if _, err := db.ExecContext(ctx, `CREATE INDEX CONCURRENTLY IF NOT EXISTS turns_open_session_id_idx
		ON turns (session_id) WHERE status NOT IN ('completed', 'failed', 'cancelled')`); err != nil {
		t.Fatalf("pre-build the index concurrently: %v", err)
	}
	prebuilt := assertOpenTurnIndexBuilt(ctx, t, db)

	var sessionID string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('web') RETURNING id::text`).Scan(&sessionID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback() }()
	if _, err := writer.ExecContext(ctx, `INSERT INTO turns (session_id, status) VALUES ($1, 'pending')`, sessionID); err != nil {
		t.Fatalf("a write to turns in flight: %v", err)
	}

	u, err := url.Parse(connStr)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("lock_timeout", "2s")
	u.RawQuery = q.Encode()
	m, mdb := newMigrate(t, u.String())
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(158); err != nil {
		t.Fatalf("up to 158 with a write to turns in flight: %v -- the migration waited for a lock on turns", err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatalf("the write in flight: %v", err)
	}
	if got := assertOpenTurnIndexBuilt(ctx, t, db); got != prebuilt {
		t.Errorf("turns_open_session_id_idx oid = %d after the migration, want the pre-built %d (not rebuilt)", got, prebuilt)
	}
	assertCleanVersion(t, connStr, 158)
}

// TestMigration000158_RebuildsAnInvalidLeftover: a concurrent build that
// fails leaves an INVALID index behind under its name, and IF NOT EXISTS
// alone would keep it -- an index Postgres never plans with. The leftover
// here is a real one: a concurrent unique build under the same name that
// fails on a session's two open turns. The migration must drop it and build
// the real index in its place.
func TestMigration000158_RebuildsAnInvalidLeftover(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 157)
	var sessionID string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('web') RETURNING id::text`).Scan(&sessionID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO turns (session_id, status) VALUES ($1, 'pending'), ($1, 'pending')`, sessionID); err != nil {
		t.Fatalf("insert two open turns: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`CREATE UNIQUE INDEX CONCURRENTLY turns_open_session_id_idx ON turns (session_id) WHERE status NOT IN ('completed', 'failed', 'cancelled')`); err == nil {
		t.Fatal("the failing concurrent build succeeded; the test needs it to fail")
	}
	leftover, valid, _, found := openTurnIndex(ctx, t, db)
	if !found || valid {
		t.Fatalf("after the failed concurrent build: found %v, valid %v; want an INVALID leftover", found, valid)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(158); err != nil {
		t.Fatalf("up to 158: %v", err)
	}
	if got := assertOpenTurnIndexBuilt(ctx, t, db); got == leftover {
		t.Errorf("turns_open_session_id_idx is still the INVALID leftover (oid %d)", got)
	}
	assertCleanVersion(t, connStr, 158)
}

// TestMigration000158_ConcurrentMigrators is a fresh install's boot: two
// control planes migrate at once, one waiting on golang-migrate's advisory
// lock while the other applies 000158. Both must succeed and leave a valid
// index -- a concurrent build would deadlock against the waiter's snapshot
// (000144's account), the plain build this migration makes does not.
func TestMigration000158_ConcurrentMigrators(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 157)
	concurrentMigratorsUp(ctx, t, connStr, db, 158)
	assertOpenTurnIndexBuilt(ctx, t, db)
}
