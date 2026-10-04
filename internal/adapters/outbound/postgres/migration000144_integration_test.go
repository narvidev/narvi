//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"
)

// This file runs migration 000144 (events_token_part_idx) through
// golang-migrate against real Postgres, each test in a database of its
// own inside the shared container, migrated to 143 first: up, down and up
// again; the paths an operator's own concurrent pre-build leaves behind
// (a valid index, kept; an INVALID one, rebuilt); and two migrators at
// once, which a fresh install of deploy/control-plane (replicas: 2)
// always has.

// wantTokenPartIndexDef is pg_get_indexdef of the index 000144 builds.
const wantTokenPartIndexDef = "CREATE INDEX events_token_part_idx ON public.events USING btree " +
	"(session_id, ((payload ->> 'messageId'::text)), id) WHERE (type = 'token'::text)"

var migrationDBCounter atomic.Int64

// migrationTestDatabase creates an empty database in the shared container,
// migrates it to version through golang-migrate, and returns its
// connection string and a database/sql handle on it. Both are dropped at
// cleanup. Each of setup runs in the new database first, on a connection
// of its own, before any other connects: a setting it gives the database
// (ALTER DATABASE ... SET) holds for every connection after it, the
// migrations' included.
func migrationTestDatabase(ctx context.Context, t *testing.T, version uint, setup ...string) (string, *sql.DB) {
	t.Helper()
	admin, adminConnStr := IntegrationTestPoolAndConnStr(t)
	name := fmt.Sprintf("m144_%d_%d", time.Now().UnixNano(), migrationDBCounter.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	u, err := url.Parse(adminConnStr)
	if err != nil {
		t.Fatalf("parse connection string: %v", err)
	}
	u.Path = "/" + name
	connStr := u.String()

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
	})

	// db has not connected yet: sql.Open connects on first use.
	if len(setup) > 0 {
		configure, err := sql.Open("pgx", connStr)
		if err != nil {
			t.Fatalf("open %s to set it up: %v", name, err)
		}
		for _, statement := range setup {
			if _, err := configure.ExecContext(ctx, statement); err != nil {
				_ = configure.Close()
				t.Fatalf("set up %s: %s: %v", name, statement, err)
			}
		}
		_ = configure.Close()
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(version); err != nil {
		t.Fatalf("migrate %s to %d: %v", name, version, err)
	}
	return connStr, db
}

// tokenPartIndex reports events_token_part_idx's oid, validity and
// definition, and whether it exists at all.
func tokenPartIndex(ctx context.Context, t *testing.T, db *sql.DB) (oid int64, valid bool, def string, found bool) {
	t.Helper()
	err := db.QueryRowContext(ctx,
		`SELECT indexrelid::bigint, indisvalid, pg_get_indexdef(indexrelid)
		 FROM pg_index WHERE indexrelid = to_regclass('events_token_part_idx')`,
	).Scan(&oid, &valid, &def)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, "", false
	}
	if err != nil {
		t.Fatalf("read events_token_part_idx: %v", err)
	}
	return oid, valid, def, true
}

// assertTokenPartIndexBuilt fails unless events_token_part_idx exists, is
// valid and has the definition 000144 builds; it returns the index's oid.
func assertTokenPartIndexBuilt(ctx context.Context, t *testing.T, db *sql.DB) int64 {
	t.Helper()
	oid, valid, def, found := tokenPartIndex(ctx, t, db)
	if !found {
		t.Fatal("events_token_part_idx does not exist")
	}
	if !valid {
		t.Errorf("events_token_part_idx is INVALID")
	}
	if def != wantTokenPartIndexDef {
		t.Errorf("events_token_part_idx = %q, want %q", def, wantTokenPartIndexDef)
	}
	return oid
}

// assertCleanVersion fails unless connStr's database is at version, not
// dirty.
func assertCleanVersion(t *testing.T, connStr string, want uint) {
	t.Helper()
	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	version, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("read migration version: %v", err)
	}
	if version != want || dirty {
		t.Errorf("migration version = %d (dirty %v), want %d, clean", version, dirty, want)
	}
}

func TestMigration000144_UpDownUp(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 143)
	if _, _, _, found := tokenPartIndex(ctx, t, db); found {
		t.Fatal("events_token_part_idx exists at 143")
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(144); err != nil {
		t.Fatalf("up to 144: %v", err)
	}
	assertTokenPartIndexBuilt(ctx, t, db)

	// Down runs DROP INDEX CONCURRENTLY, which golang-migrate can send only
	// because the down file is a single statement.
	if err := m.Migrate(143); err != nil {
		t.Fatalf("down to 143: %v", err)
	}
	if _, _, _, found := tokenPartIndex(ctx, t, db); found {
		t.Error("events_token_part_idx still exists after down")
	}

	if err := m.Migrate(144); err != nil {
		t.Fatalf("up to 144 again: %v", err)
	}
	assertTokenPartIndexBuilt(ctx, t, db)
	assertCleanVersion(t, connStr, 144)
}

// TestMigration000144_KeepsAValidPrebuiltIndex is the operator path the
// migration's comment gives for a large events table: build the index
// concurrently by hand before deploying. The migration must then build
// nothing -- the same index, by oid, survives it.
func TestMigration000144_KeepsAValidPrebuiltIndex(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 143)
	if _, err := db.ExecContext(ctx, `CREATE INDEX CONCURRENTLY IF NOT EXISTS events_token_part_idx
		ON events (session_id, (payload->>'messageId'), id) WHERE type = 'token'`); err != nil {
		t.Fatalf("pre-build the index concurrently: %v", err)
	}
	prebuilt := assertTokenPartIndexBuilt(ctx, t, db)

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(144); err != nil {
		t.Fatalf("up to 144: %v", err)
	}
	if got := assertTokenPartIndexBuilt(ctx, t, db); got != prebuilt {
		t.Errorf("events_token_part_idx oid = %d after the migration, want the pre-built %d (not rebuilt)", got, prebuilt)
	}
	assertCleanVersion(t, connStr, 144)
}

// TestMigration000144_RebuildsAnInvalidLeftover: a concurrent build that
// fails leaves an INVALID index behind under its name, and IF NOT EXISTS
// alone would keep it -- an index Postgres never plans with. The leftover
// here is a real one: a concurrent unique build under the same name that
// fails on duplicate rows. The migration must drop it and build the real
// index in its place.
func TestMigration000144_RebuildsAnInvalidLeftover(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 143)
	var sessionID string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('web') RETURNING id::text`).Scan(&sessionID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO events (session_id, type, message_id, payload) VALUES
		 ($1, 'token', 'prt_a#1', '{"messageId":"prt_a","text":""}'),
		 ($1, 'token', 'prt_a#2', '{"messageId":"prt_a","text":"done"}')`, sessionID); err != nil {
		t.Fatalf("insert token events: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`CREATE UNIQUE INDEX CONCURRENTLY events_token_part_idx ON events (session_id) WHERE type = 'token'`); err == nil {
		t.Fatal("the failing concurrent build succeeded; the test needs it to fail")
	}
	leftover, valid, _, found := tokenPartIndex(ctx, t, db)
	if !found || valid {
		t.Fatalf("after the failed concurrent build: found %v, valid %v; want an INVALID leftover", found, valid)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(144); err != nil {
		t.Fatalf("up to 144: %v", err)
	}
	if got := assertTokenPartIndexBuilt(ctx, t, db); got == leftover {
		t.Errorf("events_token_part_idx is still the INVALID leftover (oid %d)", got)
	}
	assertCleanVersion(t, connStr, 144)
}

// TestMigration000144_ConcurrentMigrators is a fresh install's boot: two
// control planes migrate at once, one waiting on golang-migrate's advisory
// lock while the other applies 000144. Both must succeed and leave a valid
// index. The waiter holds a snapshot while it waits, which a concurrent
// build (CREATE INDEX CONCURRENTLY) waits for in turn: Postgres breaks
// that cycle as a deadlock, cancelling either the build (the index left
// INVALID, the version dirty) or the waiter's lock request (its boot
// fails). Both migrators are made to wait before either can start, so the
// one that gets the lock always runs 000144 with the other one waiting.
func TestMigration000144_ConcurrentMigrators(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 143)
	concurrentMigratorsUp(ctx, t, connStr, db, 144)
	assertTokenPartIndexBuilt(ctx, t, db)
}

// concurrentMigratorsUp is two control planes booting at once against the
// database at connStr: both migrators queue on golang-migrate's advisory
// lock before either starts, so whichever takes it first applies every
// pending migration while the other waits, holding a snapshot. Both must
// succeed, and leave the version clean and at least atLeast.
func concurrentMigratorsUp(ctx context.Context, t *testing.T, connStr string, db *sql.DB, atLeast uint) {
	t.Helper()
	var dbName string
	if err := db.QueryRowContext(ctx, "SELECT current_database()").Scan(&dbName); err != nil {
		t.Fatalf("current_database: %v", err)
	}

	// Both migrators are built first: building one takes the migration
	// lock briefly itself (golang-migrate's ensureVersionTable).
	migrations := make([]*migrate.Migrate, 2)
	for i := range migrations {
		m, mdb := newMigrate(t, connStr)
		defer func() { _ = mdb.Close() }()
		migrations[i] = m
	}

	// Hold golang-migrate's own lock, through its own driver, so both
	// migrators queue on it.
	holderDB, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("open lock holder: %v", err)
	}
	defer func() { _ = holderDB.Close() }()
	holder, err := migratepg.WithInstance(holderDB, &migratepg.Config{})
	if err != nil {
		t.Fatalf("lock holder driver: %v", err)
	}
	if err := holder.Lock(); err != nil {
		t.Fatalf("take the migration lock: %v", err)
	}

	var migrators errgroup.Group
	results := make([]error, len(migrations))
	for i, m := range migrations {
		migrators.Go(func() error {
			if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
				results[i] = err
			}
			return nil
		})
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		var waiting int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_stat_activity
			 WHERE datname = $1 AND wait_event_type = 'Lock' AND wait_event = 'advisory'`, dbName).Scan(&waiting); err != nil {
			t.Fatalf("count waiting migrators: %v", err)
		}
		if waiting >= 2 {
			break
		}
		if time.Now().After(deadline) {
			_ = holder.Unlock()
			t.Fatalf("%d migrators waiting on the migration lock after 30s, want 2", waiting)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := holder.Unlock(); err != nil {
		t.Fatalf("release the migration lock: %v", err)
	}
	_ = migrators.Wait()

	for i, err := range results {
		if err != nil {
			t.Errorf("migrator %d: %v", i, err)
		}
	}
	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	version, dirty, err := m.Version()
	if err != nil || dirty || version < atLeast {
		t.Errorf("migration version = %d (dirty %v, err %v), want at least %d, clean", version, dirty, err, atLeast)
	}
}
