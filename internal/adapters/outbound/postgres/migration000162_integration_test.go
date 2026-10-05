//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
)

// This file runs the sandbox lifetime deadline migration
// (sandbox_lifetime_deadline) through golang-migrate against real Postgres,
// in a database of its own migrated to the version before it first. The up
// adds what technical plan §35.2's persisted deadline is: the estimate
// (sandboxes.lifetime_deadline_at), the lifetime it was stamped with
// (lifetime_seconds) and the gen it is for (lifetime_deadline_gen); the
// down removes them.
//
// It also pins what the migration's own "Rolling deploy" and "Rolling
// back" sections say of the previous binary: its sandbox statements --
// copied below verbatim from the sqlc output it was built with -- run with
// the columns present and leave them alone, so a gen its upsert creates
// reads "deadline unknown", and this release's resume of that gen carries
// nothing; it cannot boot on this version; and it can once the recorded
// version is forced back with the columns kept, after which this release's
// migration runs again and keeps their values.
//
// Every read and write of this release here names its columns too, in
// lifetimeUpsertSandboxForSpawn and readLifetimeColumnsSQL, so a later
// migration's columns never break it.

// lifetimeMigration is the migration's version; the previous binary's
// last is the one before it.
const lifetimeMigration = 162

// The previous binary's sandbox statements, as its sqlc output sent them:
// the column list of every SELECT * and RETURNING * on sandboxes (000157's
// two columns after previous157SandboxColumns), and UpsertSandboxForSpawn
// as every release from 000151's on sent it.
const (
	preLifetimeSandboxColumns = previous157SandboxColumns + ", agent_max_frame_bytes, agent_max_frame_bytes_gen"
	preLifetimeCreateSandbox  = `INSERT INTO sandboxes (session_id) VALUES ($1) RETURNING ` + preLifetimeSandboxColumns
	preLifetimeGetSandbox     = `SELECT ` + preLifetimeSandboxColumns + ` FROM sandboxes WHERE session_id = $1`
	preLifetimeUpsertSandbox  = `INSERT INTO sandboxes (session_id, gen, status, token_hash)
VALUES ($1, 1, 'spawning', $2)
ON CONFLICT (session_id) DO UPDATE
SET gen = sandboxes.gen + 1,
    status = 'spawning',
    token_hash = $2,
    last_seen_at = now(),
    agent_version = NULL,
    image_digest = NULL,
    image_decision_reason = NULL,
    image_decision_fingerprint = NULL,
    pr_delivery_started_at = NULL,
    stop_retire_gen = NULL,
    updated_at = now()
RETURNING ` + preLifetimeSandboxColumns
)

// lifetimeUpsertSandboxForSpawn is UpsertSandboxForSpawn as this release
// sends it ($1 session, $2 token hash, $3 lifetime seconds), returning
// only the gen.
const lifetimeUpsertSandboxForSpawn = `INSERT INTO sandboxes (session_id, gen, status, token_hash,
                       lifetime_deadline_at, lifetime_seconds, lifetime_deadline_gen)
VALUES ($1, 1, 'spawning', $2,
        now() + make_interval(secs => $3::integer),
        $3::integer,
        CASE WHEN $3::integer IS NULL THEN NULL ELSE 1 END)
ON CONFLICT (session_id) DO UPDATE
SET gen = sandboxes.gen + 1,
    status = 'spawning',
    token_hash = EXCLUDED.token_hash,
    last_seen_at = now(),
    agent_version = NULL,
    image_digest = NULL,
    image_decision_reason = NULL,
    image_decision_fingerprint = NULL,
    pr_delivery_started_at = NULL,
    stop_retire_gen = NULL,
    lifetime_deadline_at = CASE
        WHEN $3::integer IS NOT NULL THEN EXCLUDED.lifetime_deadline_at
        WHEN sandboxes.lifetime_deadline_gen = sandboxes.gen THEN sandboxes.lifetime_deadline_at
    END,
    lifetime_seconds = CASE
        WHEN $3::integer IS NOT NULL THEN EXCLUDED.lifetime_seconds
        WHEN sandboxes.lifetime_deadline_gen = sandboxes.gen THEN sandboxes.lifetime_seconds
    END,
    lifetime_deadline_gen = CASE
        WHEN $3::integer IS NOT NULL
          OR sandboxes.lifetime_deadline_gen = sandboxes.gen THEN sandboxes.gen + 1
    END,
    updated_at = now()
RETURNING gen`

const readLifetimeColumnsSQL = `SELECT gen, lifetime_deadline_at, lifetime_seconds, lifetime_deadline_gen FROM sandboxes WHERE session_id = $1`

// lifetimeColumnsSQL is the part of a sandbox row the migration is about.
type lifetimeColumnsSQL struct {
	gen                  int32
	deadline             sql.NullTime
	seconds, deadlineGen *int32
}

func readLifetimeColumnsDB(ctx context.Context, t *testing.T, db *sql.DB, sessionID string) lifetimeColumnsSQL {
	t.Helper()
	var row lifetimeColumnsSQL
	if err := db.QueryRowContext(ctx, readLifetimeColumnsSQL, sessionID).Scan(&row.gen, &row.deadline, &row.seconds, &row.deadlineGen); err != nil {
		t.Fatalf("read the sandbox's lifetime columns: %v", err)
	}
	return row
}

func (r lifetimeColumnsSQL) String() string {
	deadline := "NULL"
	if r.deadline.Valid {
		deadline = r.deadline.Time.Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("gen %d, lifetime_deadline_at %s, lifetime_seconds %v, lifetime_deadline_gen %v",
		r.gen, deadline, derefInt32(r.seconds), derefInt32(r.deadlineGen))
}

func TestMigrationSandboxLifetimeDeadline_UpAndDown(t *testing.T) {
	ctx := context.Background()
	const previous = lifetimeMigration - 1
	connStr, db := migrationTestDatabase(ctx, t, previous)
	sandboxColumns := len(strings.Split(preLifetimeSandboxColumns, ","))

	columns := func() map[string]string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT table_name || '.' || column_name, is_nullable || ' ' || coalesce(column_default, '') || ' ' || data_type
			FROM information_schema.columns
			WHERE table_name = 'sandboxes' AND column_name IN ('lifetime_deadline_at', 'lifetime_seconds', 'lifetime_deadline_gen')`)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for rows.Next() {
			var name, shape string
			if err := rows.Scan(&name, &shape); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			got[name] = shape
		}
		iterErr := rows.Err()
		_ = rows.Close()
		if iterErr != nil {
			t.Fatal(iterErr)
		}
		return got
	}

	if got := columns(); len(got) != 0 {
		t.Fatalf("lifetime columns at %d: %v, want none", previous, got)
	}
	var sessionID string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('web') RETURNING id::text`).Scan(&sessionID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	previousRow(ctx, t, db, sandboxColumns, preLifetimeCreateSandbox, sessionID)

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(lifetimeMigration); err != nil {
		t.Fatalf("up to %d: %v", lifetimeMigration, err)
	}
	want := map[string]string{
		"sandboxes.lifetime_deadline_at":  "YES  timestamp with time zone",
		"sandboxes.lifetime_seconds":      "YES  integer",
		"sandboxes.lifetime_deadline_gen": "YES  integer",
	}
	if got := columns(); len(got) != len(want) {
		t.Fatalf("columns at %d = %v, want %v", lifetimeMigration, got, want)
	} else {
		for name, shape := range want {
			if got[name] != shape {
				t.Fatalf("%s is (nullable, default, type) %q, want %q", name, got[name], shape)
			}
		}
	}

	// An existing sandbox has no deadline: no backfill.
	if row := readLifetimeColumnsDB(ctx, t, db, sessionID); row.gen != 1 || row.deadline.Valid || row.seconds != nil || row.deadlineGen != nil {
		t.Fatalf("an existing sandbox reads %v; want gen 1 and three NULLs", row)
	}

	// This release: a restore of gen 1 stamps gen 2.
	const lifetime = 7200
	if _, err := db.ExecContext(ctx, lifetimeUpsertSandboxForSpawn, sessionID, "token-hash-2", lifetime); err != nil {
		t.Fatalf("this release's upsert: %v", err)
	}
	stamped := readLifetimeColumnsDB(ctx, t, db, sessionID)
	if stamped.gen != 2 || !stamped.deadline.Valid || !equalInt32Ptr(stamped.seconds, ptrInt32(lifetime)) || !equalInt32Ptr(stamped.deadlineGen, ptrInt32(2)) {
		t.Fatalf("after this release's upsert: %v; want gen 2, a deadline, %d at gen 2", stamped, lifetime)
	}

	// The previous binary, still running during a rolling deploy: it reads
	// sandboxes with the columns present, and its upsert bumps gen and
	// leaves the columns, so the deadline no longer matches the gen. This
	// release's resume of that gen then has nothing to carry.
	previousRow(ctx, t, db, sandboxColumns, preLifetimeGetSandbox, sessionID)
	previousRow(ctx, t, db, sandboxColumns, preLifetimeUpsertSandbox, sessionID, "token-hash-3")
	if row := readLifetimeColumnsDB(ctx, t, db, sessionID); row.gen != 3 || !row.deadline.Valid || !row.deadline.Time.Equal(stamped.deadline.Time) ||
		!equalInt32Ptr(row.seconds, ptrInt32(lifetime)) || !equalInt32Ptr(row.deadlineGen, ptrInt32(2)) {
		t.Fatalf("after the previous binary's upsert: %v; want gen 3 with gen 2's deadline left as it was (not matching)", row)
	}
	if _, err := db.ExecContext(ctx, lifetimeUpsertSandboxForSpawn, sessionID, "token-hash-4", nil); err != nil {
		t.Fatalf("this release's resume: %v", err)
	}
	if row := readLifetimeColumnsDB(ctx, t, db, sessionID); row.gen != 4 || row.deadline.Valid || row.seconds != nil || row.deadlineGen != nil {
		t.Fatalf("after this release's resume of the previous binary's gen: %v; want gen 4 and three NULLs (deadline unknown)", row)
	}
	if _, err := db.ExecContext(ctx, lifetimeUpsertSandboxForSpawn, sessionID, "token-hash-5", lifetime); err != nil {
		t.Fatalf("this release's next restore: %v", err)
	}
	stamped = readLifetimeColumnsDB(ctx, t, db, sessionID)
	if stamped.gen != 5 || !stamped.deadline.Valid || !equalInt32Ptr(stamped.deadlineGen, ptrInt32(5)) {
		t.Fatalf("after this release's next restore: %v; want gen 5 stamped", stamped)
	}

	// It cannot boot on this version: golang-migrate refuses a version it
	// has no file for.
	previousMigrate, pdb := previousBinaryMigrate(t, connStr, previous)
	if err := previousMigrate.Up(); err == nil || !strings.Contains(err.Error(), fmt.Sprint(lifetimeMigration)) {
		t.Fatalf("the previous binary's boot on %d = %v, want a refusal naming %d", lifetimeMigration, err, lifetimeMigration)
	}
	_ = pdb.Close()

	// Rolling back with the columns kept: force the previous version, and
	// the previous binary boots and works. Deploying this release again
	// runs the migration again, which keeps the columns and their values.
	if err := m.Force(previous); err != nil {
		t.Fatalf("force %d: %v", previous, err)
	}
	previousMigrate, pdb = previousBinaryMigrate(t, connStr, previous)
	if err := previousMigrate.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("the previous binary's boot after force %d = %v, want no change", previous, err)
	}
	_ = pdb.Close()
	previousRow(ctx, t, db, sandboxColumns, preLifetimeGetSandbox, sessionID)
	// Pinned to this version, like every migration test here: Up would
	// also apply whatever later migrations exist by the time this runs.
	again, adb := newMigrate(t, connStr)
	if err := again.Migrate(lifetimeMigration); err != nil {
		t.Fatalf("this release's migration after the rollback = %v, want it applied again", err)
	}
	_ = adb.Close()
	assertCleanVersion(t, connStr, lifetimeMigration)
	if row := readLifetimeColumnsDB(ctx, t, db, sessionID); row.String() != stamped.String() {
		t.Fatalf("after the migration ran again: %v; want %v, kept", row, stamped)
	}

	// Down: the columns go, every row stays. Run twice -- the second time
	// on a database a rollback already brought back to the previous
	// version with the columns dropped, forced forward again -- which the
	// IF EXISTS guard allows. Up again works on that state.
	countSandboxes := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sandboxes`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := countSandboxes()
	if err := m.Migrate(previous); err != nil {
		t.Fatalf("down to %d: %v", previous, err)
	}
	if got := columns(); len(got) != 0 {
		t.Fatalf("columns after the down: %v, want none", got)
	}
	if after := countSandboxes(); after != before {
		t.Fatalf("sandboxes after the down: %d, want %d", after, before)
	}
	previousRow(ctx, t, db, sandboxColumns, preLifetimeGetSandbox, sessionID)
	if err := m.Force(lifetimeMigration); err != nil {
		t.Fatalf("force %d: %v", lifetimeMigration, err)
	}
	if err := m.Migrate(previous); err != nil {
		t.Fatalf("down to %d again, the columns already gone: %v", previous, err)
	}
	if err := m.Migrate(lifetimeMigration); err != nil {
		t.Fatalf("up to %d again: %v", lifetimeMigration, err)
	}
	assertCleanVersion(t, connStr, lifetimeMigration)
	if got := columns(); len(got) != len(want) {
		t.Fatalf("columns after up again = %v, want %v", got, want)
	}
}
