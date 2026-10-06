//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"testing"
)

// This file runs the session-dispatched turns index migration
// (turns_session_dispatched_idx) through golang-migrate against real
// Postgres, each test in a database of its own inside the shared container,
// migrated to the version before it first: up, down and up again; the paths
// an operator's own concurrent pre-build leaves behind (a valid index, kept
// with no lock taken on turns; an INVALID one, rebuilt); and two migrators
// at once, which a fresh install of deploy/control-plane (replicas: 2)
// always has. What the index is for, and that the statements touching
// turns plan no worse with it, is sessionguard_plan_integration_test.go's.

// sessionDispatchedMigration is the migration's version.
const sessionDispatchedMigration = 166

// wantSessionDispatchedIndexDef is pg_get_indexdef of the index the
// migration builds.
const wantSessionDispatchedIndexDef = "CREATE INDEX turns_session_dispatched_idx ON public.turns USING btree (session_id, dispatched_at) " +
	"INCLUDE (cost_usd, completed_at) WHERE (dispatched_at IS NOT NULL)"

// sessionDispatchedPrebuild is the operator's concurrent build, from the
// migration's own guidance.
const sessionDispatchedPrebuild = `CREATE INDEX CONCURRENTLY IF NOT EXISTS turns_session_dispatched_idx
	ON turns (session_id, dispatched_at) INCLUDE (cost_usd, completed_at)
	WHERE dispatched_at IS NOT NULL`

func sessionDispatchedIndex(ctx context.Context, t *testing.T, db *sql.DB) (oid int64, valid bool, def string, found bool) {
	t.Helper()
	err := db.QueryRowContext(ctx,
		`SELECT indexrelid::bigint, indisvalid, pg_get_indexdef(indexrelid)
		 FROM pg_index WHERE indexrelid = to_regclass('turns_session_dispatched_idx')`,
	).Scan(&oid, &valid, &def)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, "", false
	}
	if err != nil {
		t.Fatalf("read turns_session_dispatched_idx: %v", err)
	}
	return oid, valid, def, true
}

func assertSessionDispatchedIndexBuilt(ctx context.Context, t *testing.T, db *sql.DB) int64 {
	t.Helper()
	oid, valid, def, found := sessionDispatchedIndex(ctx, t, db)
	if !found {
		t.Fatal("turns_session_dispatched_idx does not exist")
	}
	if !valid {
		t.Errorf("turns_session_dispatched_idx is INVALID")
	}
	if def != wantSessionDispatchedIndexDef {
		t.Errorf("turns_session_dispatched_idx = %q, want %q", def, wantSessionDispatchedIndexDef)
	}
	return oid
}

func TestMigration000166_UpDownUp(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, sessionDispatchedMigration-1)
	if _, _, _, found := sessionDispatchedIndex(ctx, t, db); found {
		t.Fatal("turns_session_dispatched_idx exists before the migration")
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(sessionDispatchedMigration); err != nil {
		t.Fatalf("up: %v", err)
	}
	assertSessionDispatchedIndexBuilt(ctx, t, db)

	// The down runs DROP INDEX CONCURRENTLY, which golang-migrate can send
	// only because the down file is a single statement.
	if err := m.Migrate(sessionDispatchedMigration - 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, _, _, found := sessionDispatchedIndex(ctx, t, db); found {
		t.Error("turns_session_dispatched_idx still exists after the down")
	}

	if err := m.Migrate(sessionDispatchedMigration); err != nil {
		t.Fatalf("up again: %v", err)
	}
	assertSessionDispatchedIndexBuilt(ctx, t, db)
	assertCleanVersion(t, connStr, sessionDispatchedMigration)
}

// TestMigration000166_APrebuiltIndexIsKeptWithNoLockOnTurns is the operator
// path the migration's comment gives for a large turns table: build the
// index concurrently by hand before deploying. The migration then builds
// nothing -- the same index, by oid, survives it -- and takes no lock on
// turns: it runs with a short lock_timeout while another transaction holds
// an uncommitted insert into turns, which a CREATE INDEX IF NOT EXISTS,
// queueing for its SHARE lock before seeing the index, would time out on.
func TestMigration000166_APrebuiltIndexIsKeptWithNoLockOnTurns(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, sessionDispatchedMigration-1)
	if _, err := db.ExecContext(ctx, sessionDispatchedPrebuild); err != nil {
		t.Fatalf("pre-build the index concurrently: %v", err)
	}
	prebuilt := assertSessionDispatchedIndexBuilt(ctx, t, db)

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
	if err := m.Migrate(sessionDispatchedMigration); err != nil {
		t.Fatalf("up with a write to turns in flight: %v -- the migration waited for a lock on turns", err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatalf("the write in flight: %v", err)
	}
	if got := assertSessionDispatchedIndexBuilt(ctx, t, db); got != prebuilt {
		t.Errorf("turns_session_dispatched_idx oid = %d after the migration, want the pre-built %d (not rebuilt)", got, prebuilt)
	}
	assertCleanVersion(t, connStr, sessionDispatchedMigration)
}

// TestMigration000166_RebuildsAnInvalidLeftover: a concurrent build that
// fails leaves an INVALID index behind under its name, which a name check
// alone would keep and Postgres never plans with. The leftover here is a
// real one: a concurrent unique build under the same name that fails on a
// session's two turns dispatched at one instant. The migration drops it
// and builds the real index in its place.
func TestMigration000166_RebuildsAnInvalidLeftover(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, sessionDispatchedMigration-1)
	var sessionID string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('web') RETURNING id::text`).Scan(&sessionID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO turns (session_id, status, dispatched_at, completed_at)
		VALUES ($1, 'completed', '2026-01-01T00:00:00Z', now()), ($1, 'completed', '2026-01-01T00:00:00Z', now())`, sessionID); err != nil {
		t.Fatalf("insert two turns dispatched at one instant: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE UNIQUE INDEX CONCURRENTLY turns_session_dispatched_idx
		ON turns (session_id, dispatched_at) WHERE dispatched_at IS NOT NULL`); err == nil {
		t.Fatal("the failing concurrent build succeeded; the test needs it to fail")
	}
	leftover, valid, _, found := sessionDispatchedIndex(ctx, t, db)
	if !found || valid {
		t.Fatalf("after the failed concurrent build: found %v, valid %v; want an INVALID leftover", found, valid)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(sessionDispatchedMigration); err != nil {
		t.Fatalf("up: %v", err)
	}
	if got := assertSessionDispatchedIndexBuilt(ctx, t, db); got == leftover {
		t.Errorf("turns_session_dispatched_idx is still the INVALID leftover (oid %d)", got)
	}
	assertCleanVersion(t, connStr, sessionDispatchedMigration)
}

// TestMigration000166_ConcurrentMigrators is a fresh install's boot: two
// control planes migrate at once, one waiting on golang-migrate's advisory
// lock while the other applies the migration. Both succeed and leave a
// valid index: the build is a plain one.
func TestMigration000166_ConcurrentMigrators(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, sessionDispatchedMigration-1)
	concurrentMigratorsUp(ctx, t, connStr, db, sessionDispatchedMigration)
	assertSessionDispatchedIndexBuilt(ctx, t, db)
}
