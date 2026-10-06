//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

// This file runs migration 000164 (outbox_pending_kind_due_idx) through
// golang-migrate against real Postgres, each test in a database of its own
// inside the shared container, migrated to the version before it first, as
// migration000161_integration_test.go does for its index: up, down and up
// again; an operator's own concurrent pre-build, kept by oid; an INVALID
// leftover, rebuilt; a relation of the name that is not this index,
// refused; two migrators at once; and a down that never blocks writes to
// the outbox. What the index is for is outbox_lanes_plan_integration_test.go's.

// outboxLanesIndexDef is pg_get_indexdef of the index 000164 builds, with
// quote_all_identifiers off.
const outboxLanesIndexDef = "CREATE INDEX outbox_pending_kind_due_idx ON public.outbox USING btree " +
	"(kind, next_attempt_at) WHERE (status = 'pending'::outbox_status)"

// outboxLanesIndexPrebuild is the statement the up migration's operator
// guidance gives, to build the index concurrently before deploying.
const outboxLanesIndexPrebuild = `CREATE INDEX CONCURRENTLY IF NOT EXISTS outbox_pending_kind_due_idx
	ON outbox (kind, next_attempt_at) WHERE status = 'pending'`

// readOutboxLanesIndex reports the oid of the relation named
// outbox_pending_kind_due_idx, its validity and definition when it is an
// index, and whether it exists at all.
func readOutboxLanesIndex(ctx context.Context, t *testing.T, db *sql.DB) (oid int64, valid bool, def string, found bool) {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SET LOCAL quote_all_identifiers = off`); err != nil {
		t.Fatal(err)
	}
	err = tx.QueryRowContext(ctx,
		`SELECT c.oid::bigint, COALESCE(i.indisvalid, false), COALESCE(pg_get_indexdef(c.oid), '')
		 FROM pg_class c LEFT JOIN pg_index i ON i.indexrelid = c.oid
		 WHERE c.oid = to_regclass('outbox_pending_kind_due_idx')`,
	).Scan(&oid, &valid, &def)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, "", false
	}
	if err != nil {
		t.Fatalf("read outbox_pending_kind_due_idx: %v", err)
	}
	return oid, valid, def, true
}

// assertOutboxLanesIndexBuilt fails unless the index exists, is valid and
// has the definition 000164 builds; it returns the index's oid.
func assertOutboxLanesIndexBuilt(ctx context.Context, t *testing.T, db *sql.DB) int64 {
	t.Helper()
	oid, valid, def, found := readOutboxLanesIndex(ctx, t, db)
	if !found {
		t.Fatal("outbox_pending_kind_due_idx does not exist")
	}
	if !valid {
		t.Error("outbox_pending_kind_due_idx is INVALID")
	}
	if def != outboxLanesIndexDef {
		t.Errorf("outbox_pending_kind_due_idx = %q, want %q", def, outboxLanesIndexDef)
	}
	return oid
}

func TestMigration000164_UpDownUp(t *testing.T) {
	ctx := context.Background()
	before := migrationBefore(t, outboxLanesIndexVersion)
	connStr, db := migrationTestDatabase(ctx, t, before)
	if _, _, _, found := readOutboxLanesIndex(ctx, t, db); found {
		t.Fatalf("outbox_pending_kind_due_idx exists at %d", before)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(outboxLanesIndexVersion); err != nil {
		t.Fatalf("up to %d: %v", outboxLanesIndexVersion, err)
	}
	assertOutboxLanesIndexBuilt(ctx, t, db)

	// The down runs DROP INDEX CONCURRENTLY, which golang-migrate can send
	// only because the down file is a single statement.
	if err := m.Migrate(before); err != nil {
		t.Fatalf("down to %d: %v", before, err)
	}
	if _, _, _, found := readOutboxLanesIndex(ctx, t, db); found {
		t.Error("outbox_pending_kind_due_idx still exists after down")
	}

	if err := m.Migrate(outboxLanesIndexVersion); err != nil {
		t.Fatalf("up to %d again: %v", outboxLanesIndexVersion, err)
	}
	assertOutboxLanesIndexBuilt(ctx, t, db)
	assertCleanVersion(t, connStr, outboxLanesIndexVersion)
}

// TestMigration000164_KeepsAValidPrebuiltIndex is the operator path the
// migration's comment gives for a large outbox: build the index
// concurrently by hand before deploying. The migration then builds nothing
// and takes no lock on outbox -- it runs with a short lock_timeout while
// an enqueue into the outbox is in flight -- and the same index, by oid,
// survives it.
func TestMigration000164_KeepsAValidPrebuiltIndex(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, migrationBefore(t, outboxLanesIndexVersion))
	if _, err := db.ExecContext(ctx, outboxLanesIndexPrebuild); err != nil {
		t.Fatalf("pre-build the index concurrently: %v", err)
	}
	prebuilt := assertOutboxLanesIndexBuilt(ctx, t, db)

	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback() }()
	if _, err := writer.ExecContext(ctx, `INSERT INTO outbox (kind, payload) VALUES ('slack', '{}')`); err != nil {
		t.Fatalf("an enqueue in flight: %v", err)
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
	if err := m.Migrate(outboxLanesIndexVersion); err != nil {
		t.Fatalf("up to %d with an enqueue in flight: %v -- the migration waited for a lock on outbox", outboxLanesIndexVersion, err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatalf("the enqueue in flight: %v", err)
	}
	if got := assertOutboxLanesIndexBuilt(ctx, t, db); got != prebuilt {
		t.Errorf("outbox_pending_kind_due_idx oid = %d after the migration, want the pre-built %d (not rebuilt)", got, prebuilt)
	}
	assertCleanVersion(t, connStr, outboxLanesIndexVersion)
}

// TestMigration000164_RebuildsAnInvalidLeftover: a concurrent build that
// fails leaves an INVALID index under its name, which Postgres never plans
// with. The leftover here is a real one: a concurrent unique build of the
// name that fails on two pending rows of one kind. The migration must drop
// it and build the real index in its place.
func TestMigration000164_RebuildsAnInvalidLeftover(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, migrationBefore(t, outboxLanesIndexVersion))
	if _, err := db.ExecContext(ctx, `INSERT INTO outbox (kind, payload) VALUES ('slack', '{}'), ('slack', '{}')`); err != nil {
		t.Fatalf("insert outbox rows: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`CREATE UNIQUE INDEX CONCURRENTLY outbox_pending_kind_due_idx ON outbox (kind) WHERE status = 'pending'`); err == nil {
		t.Fatal("the failing concurrent build succeeded; the test needs it to fail")
	}
	leftover, valid, _, found := readOutboxLanesIndex(ctx, t, db)
	if !found || valid {
		t.Fatalf("after the failed concurrent build: found %v, valid %v; want an INVALID leftover", found, valid)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(outboxLanesIndexVersion); err != nil {
		t.Fatalf("up to %d: %v", outboxLanesIndexVersion, err)
	}
	if got := assertOutboxLanesIndexBuilt(ctx, t, db); got == leftover {
		t.Errorf("outbox_pending_kind_due_idx is still the INVALID leftover (oid %d)", got)
	}
	assertCleanVersion(t, connStr, outboxLanesIndexVersion)
}

// TestMigration000164_ConcurrentMigrators is a fresh install's boot: two
// control planes migrate at once, one waiting on golang-migrate's advisory
// lock while the other applies 000164. Both must succeed and leave a valid
// index.
func TestMigration000164_ConcurrentMigrators(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, migrationBefore(t, outboxLanesIndexVersion))
	concurrentMigratorsUp(ctx, t, connStr, db, outboxLanesIndexVersion)
	assertOutboxLanesIndexBuilt(ctx, t, db)
}

// TestMigration000164_RefusesAnotherRelationOfItsName: a relation of this
// name that is not this index would leave the lanes reading other kinds'
// rows or every delivered one. The migration must neither keep it nor drop
// it: it fails, naming what it found and how to recover, golang-migrate
// leaves the version dirty, and the relation is left as it was. Following
// the message -- drop it, force the version back, migrate again -- then
// builds the index.
func TestMigration000164_RefusesAnotherRelationOfItsName(t *testing.T) {
	for _, tt := range []struct {
		name, create, found, drop string
	}{
		{
			name:   "next_attempt_at alone",
			create: `CREATE INDEX CONCURRENTLY outbox_pending_kind_due_idx ON outbox (next_attempt_at) WHERE status = 'pending'`,
			found:  "CREATE INDEX outbox_pending_kind_due_idx ON public.outbox USING btree (next_attempt_at) WHERE (status = 'pending'::outbox_status)",
			drop:   `DROP INDEX CONCURRENTLY outbox_pending_kind_due_idx`,
		},
		{
			name:   "the columns without the predicate",
			create: `CREATE INDEX CONCURRENTLY outbox_pending_kind_due_idx ON outbox (kind, next_attempt_at)`,
			found:  "CREATE INDEX outbox_pending_kind_due_idx ON public.outbox USING btree (kind, next_attempt_at)",
			drop:   `DROP INDEX CONCURRENTLY outbox_pending_kind_due_idx`,
		},
		{
			name:   "a sequence of the name",
			create: `CREATE SEQUENCE outbox_pending_kind_due_idx`,
			found:  "a relation that is not an index (pg_class.relkind 'S')",
			drop:   `DROP SEQUENCE outbox_pending_kind_due_idx`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			before := migrationBefore(t, outboxLanesIndexVersion)
			connStr, db := migrationTestDatabase(ctx, t, before)
			if _, err := db.ExecContext(ctx, tt.create); err != nil {
				t.Fatalf("create the relation: %v", err)
			}
			oid, _, def, found := readOutboxLanesIndex(ctx, t, db)
			if !found {
				t.Fatal("no relation named outbox_pending_kind_due_idx")
			}

			m, mdb := newMigrate(t, connStr)
			defer func() { _ = mdb.Close() }()
			err := m.Migrate(outboxLanesIndexVersion)
			if err == nil {
				t.Fatalf("up to %d kept %s; want the migration refused", outboxLanesIndexVersion, tt.found)
			}
			for _, want := range []string{
				"outbox_pending_kind_due_idx exists but is not the index this migration builds",
				"found " + tt.found + ", want CREATE INDEX outbox_pending_kind_due_idx ON outbox (kind, next_attempt_at) WHERE status = 'pending'",
				"clear this dirty version with `migrate force` to the version before this one",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the migration's error does not say %q:\n%v", want, err)
				}
			}
			version, dirty, verr := m.Version()
			if verr != nil || version != outboxLanesIndexVersion || !dirty {
				t.Errorf("migration version = %d (dirty %v, err %v), want %d, dirty", version, dirty, verr, outboxLanesIndexVersion)
			}
			if gotOID, _, gotDef, _ := readOutboxLanesIndex(ctx, t, db); gotOID != oid || gotDef != def {
				t.Errorf("the relation is now %d %q, want it left as it was, %d %q", gotOID, gotDef, oid, def)
			}

			if _, err := db.ExecContext(ctx, tt.drop); err != nil {
				t.Fatalf("%s: %v", tt.drop, err)
			}
			if err := m.Force(int(before)); err != nil {
				t.Fatalf("force %d: %v", before, err)
			}
			if err := m.Migrate(outboxLanesIndexVersion); err != nil {
				t.Fatalf("up to %d after the remedy: %v", outboxLanesIndexVersion, err)
			}
			assertOutboxLanesIndexBuilt(ctx, t, db)
			assertCleanVersion(t, connStr, outboxLanesIndexVersion)
		})
	}
}

// TestMigration000164_DownDoesNotBlockWrites pins what the down is for: it
// drops the index without blocking writes to the outbox. A transaction
// that has read the outbox stays open, so the drop has to wait for it, and
// while it waits an enqueue from another connection must go through within
// a short lock_timeout.
func TestMigration000164_DownDoesNotBlockWrites(t *testing.T) {
	ctx := context.Background()
	before := migrationBefore(t, outboxLanesIndexVersion)
	connStr, db := migrationTestDatabase(ctx, t, outboxLanesIndexVersion)
	assertOutboxLanesIndexBuilt(ctx, t, db)
	var dbName string
	if err := db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatal(err)
	}

	reader, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback() }()
	var rows int
	if err := reader.QueryRowContext(ctx, `SELECT count(*) FROM outbox`).Scan(&rows); err != nil {
		t.Fatalf("read the outbox in the open transaction: %v", err)
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
	var pollErr, writeErr error
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var n int
		if pollErr = db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND wait_event_type = 'Lock'`, dbName).Scan(&n); pollErr != nil || n > 0 {
			waiting = pollErr == nil
			break
		}
	}
	if waiting {
		writeErr = enqueueWithLockTimeout(ctx, db)
	}
	_ = reader.Rollback()
	_ = down.Wait()

	if pollErr != nil {
		t.Fatalf("look for the waiting drop: %v", pollErr)
	}
	if !waiting {
		t.Fatal("the down never waited for the transaction that read the outbox; the test needs it to")
	}
	if writeErr != nil {
		t.Errorf("an enqueue while the down waited: %v -- the drop blocks writes", writeErr)
	}
	if downErr != nil {
		t.Fatalf("down to %d: %v", before, downErr)
	}
	if _, _, _, found := readOutboxLanesIndex(ctx, t, db); found {
		t.Error("outbox_pending_kind_due_idx still exists after down")
	}
	assertCleanVersion(t, connStr, before)
}

// enqueueWithLockTimeout enqueues an outbox row with a 2 s lock_timeout, so
// an enqueue that queues for its lock fails rather than waits.
func enqueueWithLockTimeout(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '2s'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox (kind, payload) VALUES ('slack', '{}')`); err != nil {
		return err
	}
	return tx.Commit()
}
