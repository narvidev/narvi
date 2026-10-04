//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"testing"

	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/narvidev/narvi/migrations"
)

// This file runs migration 000161 (events_token_window_idx) through
// golang-migrate against real Postgres, each test in a database of its own
// inside the shared container, migrated to the version before it first: up,
// down and up again; the paths an operator's own concurrent pre-build
// leaves behind (a valid index, kept without a lock on events; an INVALID
// one, rebuilt); and two migrators at once, which a fresh install of
// deploy/control-plane (replicas: 2) always has. What the index is for is
// event_tokenwindow_plan_integration_test.go's.

// tokenWindowIndexVersion is the migration that builds events_token_window_idx.
const tokenWindowIndexVersion = 161

// wantTokenWindowIndexDef is pg_get_indexdef of the index 000161 builds.
const wantTokenWindowIndexDef = "CREATE INDEX events_token_window_idx ON public.events USING btree " +
	"(session_id, ((id + 0))) WHERE (type = 'token'::text)"

// migrationBefore returns the version of the embedded migration that
// precedes version: the one a database is migrated to before version runs.
func migrationBefore(t *testing.T, version uint) uint {
	t.Helper()
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("iofs.New: %v", err)
	}
	defer func() { _ = src.Close() }()
	prev, err := src.Prev(version)
	if err != nil {
		t.Fatalf("the migration before %d: %v", version, err)
	}
	return prev
}

// readTokenWindowIndex reports events_token_window_idx's oid, validity and
// definition, and whether it exists at all.
func readTokenWindowIndex(ctx context.Context, t *testing.T, db *sql.DB) (oid int64, valid bool, def string, found bool) {
	t.Helper()
	err := db.QueryRowContext(ctx,
		`SELECT indexrelid::bigint, indisvalid, pg_get_indexdef(indexrelid)
		 FROM pg_index WHERE indexrelid = to_regclass('events_token_window_idx')`,
	).Scan(&oid, &valid, &def)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, "", false
	}
	if err != nil {
		t.Fatalf("read events_token_window_idx: %v", err)
	}
	return oid, valid, def, true
}

// assertTokenWindowIndexBuilt fails unless events_token_window_idx exists,
// is valid and has the definition 000161 builds; it returns the index's
// oid.
func assertTokenWindowIndexBuilt(ctx context.Context, t *testing.T, db *sql.DB) int64 {
	t.Helper()
	oid, valid, def, found := readTokenWindowIndex(ctx, t, db)
	if !found {
		t.Fatal("events_token_window_idx does not exist")
	}
	if !valid {
		t.Errorf("events_token_window_idx is INVALID")
	}
	if def != wantTokenWindowIndexDef {
		t.Errorf("events_token_window_idx = %q, want %q", def, wantTokenWindowIndexDef)
	}
	return oid
}

// prebuildTokenWindowIndex builds events_token_window_idx as the up
// migration's operator guidance says to, concurrently from psql, and
// returns its oid.
func prebuildTokenWindowIndex(ctx context.Context, t *testing.T, db *sql.DB) int64 {
	t.Helper()
	if _, err := db.ExecContext(ctx, `CREATE INDEX CONCURRENTLY IF NOT EXISTS events_token_window_idx
		ON events (session_id, (id + 0)) WHERE type = 'token'`); err != nil {
		t.Fatalf("pre-build the index concurrently: %v", err)
	}
	return assertTokenWindowIndexBuilt(ctx, t, db)
}

func TestMigration000161_UpDownUp(t *testing.T) {
	ctx := context.Background()
	before := migrationBefore(t, tokenWindowIndexVersion)
	connStr, db := migrationTestDatabase(ctx, t, before)
	if _, _, _, found := readTokenWindowIndex(ctx, t, db); found {
		t.Fatalf("events_token_window_idx exists at %d", before)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(tokenWindowIndexVersion); err != nil {
		t.Fatalf("up to %d: %v", tokenWindowIndexVersion, err)
	}
	assertTokenWindowIndexBuilt(ctx, t, db)

	// Down runs DROP INDEX CONCURRENTLY, which golang-migrate can send only
	// because the down file is a single statement.
	if err := m.Migrate(before); err != nil {
		t.Fatalf("down to %d: %v", before, err)
	}
	if _, _, _, found := readTokenWindowIndex(ctx, t, db); found {
		t.Error("events_token_window_idx still exists after down")
	}

	if err := m.Migrate(tokenWindowIndexVersion); err != nil {
		t.Fatalf("up to %d again: %v", tokenWindowIndexVersion, err)
	}
	assertTokenWindowIndexBuilt(ctx, t, db)
	assertCleanVersion(t, connStr, tokenWindowIndexVersion)
}

// TestMigration000161_KeepsAValidPrebuiltIndex is the operator path the
// migration's comment gives for a large events table: build the index
// concurrently by hand before deploying. The migration must then build
// nothing -- the same index, by oid, survives it.
func TestMigration000161_KeepsAValidPrebuiltIndex(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, migrationBefore(t, tokenWindowIndexVersion))
	prebuilt := prebuildTokenWindowIndex(ctx, t, db)

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(tokenWindowIndexVersion); err != nil {
		t.Fatalf("up to %d: %v", tokenWindowIndexVersion, err)
	}
	if got := assertTokenWindowIndexBuilt(ctx, t, db); got != prebuilt {
		t.Errorf("events_token_window_idx oid = %d after the migration, want the pre-built %d (not rebuilt)", got, prebuilt)
	}
	assertCleanVersion(t, connStr, tokenWindowIndexVersion)
}

// TestMigration000161_APrebuiltIndexTakesNoLockOnEvents is the rest of that
// operator path: with the valid index pre-built, the migration takes no
// lock on events at all, so it never waits behind an insert into events in
// flight, nor stalls the inserts after it. The migrator runs with a short
// lock_timeout while another transaction holds an uncommitted insert into
// events; a CREATE INDEX IF NOT EXISTS would queue for the SHARE lock on
// events before seeing the index, and time out.
func TestMigration000161_APrebuiltIndexTakesNoLockOnEvents(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, migrationBefore(t, tokenWindowIndexVersion))
	prebuilt := prebuildTokenWindowIndex(ctx, t, db)

	var sessionID string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('web') RETURNING id::text`).Scan(&sessionID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback() }()
	if _, err := writer.ExecContext(ctx,
		`INSERT INTO events (session_id, type, message_id, payload) VALUES ($1, 'token', 'prt_a', '{"messageId":"prt_a","text":"in flight"}')`,
		sessionID); err != nil {
		t.Fatalf("an insert into events in flight: %v", err)
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
	if err := m.Migrate(tokenWindowIndexVersion); err != nil {
		t.Fatalf("up to %d with an insert into events in flight: %v -- the migration waited for a lock on events", tokenWindowIndexVersion, err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatalf("the insert in flight: %v", err)
	}
	if got := assertTokenWindowIndexBuilt(ctx, t, db); got != prebuilt {
		t.Errorf("events_token_window_idx oid = %d after the migration, want the pre-built %d (not rebuilt)", got, prebuilt)
	}
	assertCleanVersion(t, connStr, tokenWindowIndexVersion)
}

// TestMigration000161_RebuildsAnInvalidLeftover: a concurrent build that
// fails leaves an INVALID index behind under its name, and a name check
// alone would keep it -- an index Postgres never plans with. The leftover
// here is a real one: a concurrent unique build under the same name that
// fails on a session's two `token` frames. The migration must drop it and
// build the real index in its place.
func TestMigration000161_RebuildsAnInvalidLeftover(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, migrationBefore(t, tokenWindowIndexVersion))
	var sessionID string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('web') RETURNING id::text`).Scan(&sessionID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO events (session_id, type, message_id, payload) VALUES
		 ($1, 'token', 'prt_a', '{"messageId":"prt_a","text":""}'),
		 ($1, 'token', 'prt_a#2', '{"messageId":"prt_a","text":"done"}')`, sessionID); err != nil {
		t.Fatalf("insert token events: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`CREATE UNIQUE INDEX CONCURRENTLY events_token_window_idx ON events (session_id) WHERE type = 'token'`); err == nil {
		t.Fatal("the failing concurrent build succeeded; the test needs it to fail")
	}
	leftover, valid, _, found := readTokenWindowIndex(ctx, t, db)
	if !found || valid {
		t.Fatalf("after the failed concurrent build: found %v, valid %v; want an INVALID leftover", found, valid)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(tokenWindowIndexVersion); err != nil {
		t.Fatalf("up to %d: %v", tokenWindowIndexVersion, err)
	}
	if got := assertTokenWindowIndexBuilt(ctx, t, db); got == leftover {
		t.Errorf("events_token_window_idx is still the INVALID leftover (oid %d)", got)
	}
	assertCleanVersion(t, connStr, tokenWindowIndexVersion)
}

// TestMigration000161_ConcurrentMigrators is a fresh install's boot: two
// control planes migrate at once, one waiting on golang-migrate's advisory
// lock while the other applies 000161. Both must succeed and leave a valid
// index -- a concurrent build would deadlock against the waiter's snapshot
// (000144's account), the plain build this migration makes does not.
func TestMigration000161_ConcurrentMigrators(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, migrationBefore(t, tokenWindowIndexVersion))
	concurrentMigratorsUp(ctx, t, connStr, db, tokenWindowIndexVersion)
	assertTokenWindowIndexBuilt(ctx, t, db)
}
