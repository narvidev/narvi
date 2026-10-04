//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4/source/iofs"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/migrations"
)

// This file runs migration 000161 (events_token_window_idx) through
// golang-migrate against real Postgres, each test in a database of its own
// inside the shared container, migrated to the version before it first: up,
// down and up again; the paths an operator's own concurrent pre-build
// leaves behind (a valid index, kept without a lock on events; an INVALID
// one, rebuilt; a relation of the name that is not this index, refused);
// two migrators at once, which a fresh install of deploy/control-plane
// (replicas: 2) always has; and a down that never blocks inserts into
// events. What the index is for is event_tokenwindow_plan_integration_test.go's.

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

// TestMigration000161_RefusesAnotherRelationOfItsName: an operator's
// pre-build under this name with another definition, or any other relation
// of the name, would leave the read's bounds on id + 0 with no index to
// seek on. The migration must neither keep it nor drop it: it fails, naming
// what it found and how to recover, golang-migrate leaves the version
// dirty, and the relation is left as it was. Following the message --
// drop it, force the version back, migrate again -- then builds the index.
func TestMigration000161_RefusesAnotherRelationOfItsName(t *testing.T) {
	for _, tt := range []struct {
		name, create, found, drop string
	}{
		{
			name:   "the plain (session_id, id) index row 231 rejected",
			create: `CREATE INDEX CONCURRENTLY events_token_window_idx ON events (session_id, id) WHERE type = 'token'`,
			found:  "CREATE INDEX events_token_window_idx ON public.events USING btree (session_id, id) WHERE (type = 'token'::text)",
			drop:   `DROP INDEX CONCURRENTLY events_token_window_idx`,
		},
		{
			name:   "the expression without its predicate",
			create: `CREATE INDEX CONCURRENTLY events_token_window_idx ON events (session_id, (id + 0))`,
			found:  "CREATE INDEX events_token_window_idx ON public.events USING btree (session_id, ((id + 0)))",
			drop:   `DROP INDEX CONCURRENTLY events_token_window_idx`,
		},
		{
			name:   "a unique index of the same columns",
			create: `CREATE UNIQUE INDEX CONCURRENTLY events_token_window_idx ON events (session_id, (id + 0)) WHERE type = 'token'`,
			found:  "CREATE UNIQUE INDEX events_token_window_idx ON public.events USING btree (session_id, ((id + 0))) WHERE (type = 'token'::text)",
			drop:   `DROP INDEX CONCURRENTLY events_token_window_idx`,
		},
		{
			name:   "a sequence of the name",
			create: `CREATE SEQUENCE events_token_window_idx`,
			found:  "a relation that is not an index (pg_class.relkind 'S')",
			drop:   `DROP SEQUENCE events_token_window_idx`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			before := migrationBefore(t, tokenWindowIndexVersion)
			connStr, db := migrationTestDatabase(ctx, t, before)
			if _, err := db.ExecContext(ctx, tt.create); err != nil {
				t.Fatalf("create the relation: %v", err)
			}
			relation := func() (oid int64, def string) {
				t.Helper()
				if err := db.QueryRowContext(ctx,
					`SELECT oid::bigint, COALESCE(pg_get_indexdef(oid), '') FROM pg_class WHERE oid = to_regclass('events_token_window_idx')`,
				).Scan(&oid, &def); err != nil {
					t.Fatalf("read the relation named events_token_window_idx: %v", err)
				}
				return oid, def
			}
			oid, def := relation()

			m, mdb := newMigrate(t, connStr)
			defer func() { _ = mdb.Close() }()
			err := m.Migrate(tokenWindowIndexVersion)
			if err == nil {
				t.Fatalf("up to %d kept %s; want the migration refused", tokenWindowIndexVersion, tt.found)
			}
			for _, want := range []string{
				"events_token_window_idx exists but is not the index this migration builds",
				"found " + tt.found + ", want CREATE INDEX events_token_window_idx ON events (session_id, (id + 0)) WHERE type = 'token'",
				fmt.Sprintf("clear this dirty version with `migrate force %d`", before),
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the migration's error does not say %q:\n%v", want, err)
				}
			}
			version, dirty, verr := m.Version()
			if verr != nil || version != tokenWindowIndexVersion || !dirty {
				t.Errorf("migration version = %d (dirty %v, err %v), want %d, dirty", version, dirty, verr, tokenWindowIndexVersion)
			}
			if gotOID, gotDef := relation(); gotOID != oid || gotDef != def {
				t.Errorf("the relation is now %d %q, want it left as it was, %d %q", gotOID, gotDef, oid, def)
			}

			// The message's remedy.
			if _, err := db.ExecContext(ctx, tt.drop); err != nil {
				t.Fatalf("%s: %v", tt.drop, err)
			}
			if err := m.Force(int(before)); err != nil {
				t.Fatalf("force %d: %v", before, err)
			}
			if err := m.Migrate(tokenWindowIndexVersion); err != nil {
				t.Fatalf("up to %d after the remedy: %v", tokenWindowIndexVersion, err)
			}
			assertTokenWindowIndexBuilt(ctx, t, db)
			assertCleanVersion(t, connStr, tokenWindowIndexVersion)
		})
	}
}

// TestMigration000161_DownDoesNotBlockInserts pins what the down is for:
// it drops the index without blocking inserts into events. A transaction
// that has read events stays open, so the drop has to wait for it, and
// while it waits an insert into events from another connection must go
// through within a short lock_timeout. DROP INDEX CONCURRENTLY waits for
// that transaction without taking a lock an insert conflicts with. A plain
// DROP INDEX queues for ACCESS EXCLUSIVE on events behind the open
// transaction, and every later insert queues behind it, so the insert times
// out. The waiting drop is found by its wait event: the down file is longer
// than pg_stat_activity shows of a query.
func TestMigration000161_DownDoesNotBlockInserts(t *testing.T) {
	ctx := context.Background()
	before := migrationBefore(t, tokenWindowIndexVersion)
	connStr, db := migrationTestDatabase(ctx, t, tokenWindowIndexVersion)
	assertTokenWindowIndexBuilt(ctx, t, db)
	var sessionID, dbName string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('web') RETURNING id::text, current_database()`).Scan(&sessionID, &dbName); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	reader, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback() }()
	var events int
	if err := reader.QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&events); err != nil {
		t.Fatalf("read events in the open transaction: %v", err)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	var down errgroup.Group
	var downErr error
	down.Go(func() error {
		downErr = m.Migrate(before)
		return nil
	})

	// Nothing below fails the test before the reader is rolled back and the
	// down has returned, so the down never outlives the test.
	waiting := false
	var pollErr, insertErr error
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var n int
		if pollErr = db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND wait_event_type = 'Lock'`, dbName).Scan(&n); pollErr != nil || n > 0 {
			waiting = pollErr == nil
			break
		}
	}
	if waiting {
		insertErr = insertWithLockTimeout(ctx, db, sessionID)
	}
	_ = reader.Rollback()
	_ = down.Wait()

	if pollErr != nil {
		t.Fatalf("look for the waiting drop: %v", pollErr)
	}
	if !waiting {
		t.Fatal("the down never waited for the transaction that read events; the test needs it to")
	}
	if insertErr != nil {
		t.Errorf("an insert into events while the down waited: %v -- the drop blocks inserts", insertErr)
	}
	if downErr != nil {
		t.Fatalf("down to %d: %v", before, downErr)
	}
	if _, _, _, found := readTokenWindowIndex(ctx, t, db); found {
		t.Error("events_token_window_idx still exists after down")
	}
	assertCleanVersion(t, connStr, before)
}

// insertWithLockTimeout inserts a `token` frame into events with a 2 s
// lock_timeout, so an insert that queues for its lock fails rather than
// waits.
func insertWithLockTimeout(ctx context.Context, db *sql.DB, sessionID string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '2s'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (session_id, type, message_id, payload) VALUES ($1, 'token', 'prt_during_down', '{"messageId":"prt_during_down","text":"during the down"}')`,
		sessionID); err != nil {
		return err
	}
	return tx.Commit()
}
