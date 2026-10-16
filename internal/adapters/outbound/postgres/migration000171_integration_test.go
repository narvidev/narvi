//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// This file runs the sandbox rotation migration (sandbox_rotation) through
// golang-migrate against real Postgres, in a database of its own migrated
// to the version before it first. The up adds what technical plan §35.3's
// runway gate records on the sandbox row: the gen a rotation is of, the
// snapshot command it sent and when it started (sandboxes.rotation_gen,
// rotation_message_id, rotation_started_at), when the snapshot in progress
// started and for which command (snapshot_started_at,
// snapshot_start_message_id), and the heartbeats since a connection's ready
// (heartbeat_ready_seq, heartbeats_since_ready); the down removes them.
//
// It also pins what the migration's "Rolling deploy" and "Rolling back"
// sections say of the previous binary: its sandbox statements -- copied
// below verbatim from the sqlc output it was built with -- run with the
// columns present and leave them alone; a rotation left on a gen its claim
// replaced reads "not rotating"; a snapshot it starts carries no start of
// its own -- none, or an earlier snapshot's, stamped for another command --
// and is given one at this release's first look; it cannot boot on this
// version, and it can
// once the recorded version is forced back with the columns kept, after
// which this release's migration runs again and keeps their values. This
// release's statements are read from the sqlc output (generatedQuery), so
// they cannot drift from what the store sends.

// sandboxRotationMigration is the migration's version.
const sandboxRotationMigration = 171

// The previous binary's sandbox statements, as its sqlc output sent them:
// the column list of every SELECT * and RETURNING * on sandboxes, its claim
// and its pending-snapshot write.
const (
	preRotationSandboxColumns = preProvenanceSandboxColumns + ", snapshot_provenance_id, snapshot_agent_protocol, snapshot_runtime_version, snapshot_minted_at"
	preRotationGetSandbox     = `SELECT ` + preRotationSandboxColumns + ` FROM sandboxes WHERE session_id = $1`
	preRotationUpsertForSpawn = `INSERT INTO sandboxes (session_id, gen, status, token_hash,
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
RETURNING ` + preRotationSandboxColumns
	preRotationPendingSnapshot = `UPDATE sandboxes
SET pending_snapshot_message_id = $2, updated_at = now()
WHERE session_id = $1
RETURNING ` + preRotationSandboxColumns
)

// rotationColumns is the part of a sandbox row the migration is about,
// read by name, with the gen the rotation is measured against.
type rotationColumns struct {
	gen                                                         int32
	rotationGen                                                 sql.NullInt32
	rotationStartedAt, snapshotAt                               sql.NullTime
	rotationMessageID, pendingSnapshotMessageID, startMessageID sql.NullString
}

func readRotationColumns(ctx context.Context, t *testing.T, db *sql.DB, sessionID string) rotationColumns {
	t.Helper()
	var c rotationColumns
	if err := db.QueryRowContext(ctx, `SELECT gen, rotation_gen, rotation_message_id, rotation_started_at, snapshot_started_at, pending_snapshot_message_id, snapshot_start_message_id
		FROM sandboxes WHERE session_id = $1`, sessionID).Scan(&c.gen, &c.rotationGen, &c.rotationMessageID, &c.rotationStartedAt, &c.snapshotAt,
		&c.pendingSnapshotMessageID, &c.startMessageID); err != nil {
		t.Fatalf("read the sandbox's rotation columns: %v", err)
	}
	return c
}

func (c rotationColumns) String() string {
	return fmt.Sprintf("gen %d, rotation_gen %v, rotation_message_id %v, rotation_started_at %v, snapshot_started_at %v (for %v), pending %v",
		c.gen, c.rotationGen, c.rotationMessageID, c.rotationStartedAt.Valid, c.snapshotAt.Valid, c.startMessageID, c.pendingSnapshotMessageID)
}

// runwayFacts is what the migration's tests read of this release's runway
// read: whether it reads a rotation, whether the snapshot in progress is
// that rotation's own, and whether it reads the snapshot's start.
type runwayFacts struct{ rotating, ownSnapshot, startKnown, replayPending bool }

func readRunwayFacts(ctx context.Context, t *testing.T, db *sql.DB, sessionID string) runwayFacts {
	t.Helper()
	q := generatedQuery(t, "sandboxes.sql.go", "getSandboxRunway")
	var f runwayFacts
	var gen int32
	var status string
	var deadlineKnown, taken, midSnapshot bool
	var runway, sinceRotation, sinceStart, sinceSeen int64
	var lifetime int32
	var delivery, observed sql.NullTime
	if err := db.QueryRowContext(ctx, q, sessionID).Scan(&gen, &status, &deadlineKnown, &runway, &lifetime, &f.rotating, &f.ownSnapshot, &sinceRotation, &taken,
		&f.startKnown, &sinceStart, &sinceSeen, &midSnapshot, &f.replayPending, &delivery, &observed); err != nil {
		t.Fatalf("this release's runway read: %v", err)
	}
	return f
}

func TestMigrationSandboxRotation_UpAndDown(t *testing.T) {
	ctx := context.Background()
	previous := versionBefore(t, sandboxRotationMigration)
	connStr, db := migrationTestDatabase(ctx, t, previous)
	sandboxColumns := len(strings.Split(preRotationSandboxColumns, ","))

	columns := func() map[string]string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT table_name || '.' || column_name, is_nullable || ' ' || coalesce(column_default, '') || ' ' || data_type
			FROM information_schema.columns
			WHERE table_name = 'sandboxes' AND column_name IN ('rotation_gen', 'rotation_message_id', 'rotation_started_at', 'snapshot_started_at',
				'snapshot_start_message_id', 'heartbeat_ready_seq', 'heartbeats_since_ready')`)
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
		t.Fatalf("rotation columns at %d: %v, want none", previous, got)
	}
	var sessionID string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('web') RETURNING id::text`).Scan(&sessionID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	previousRow(ctx, t, db, sandboxColumns, preRotationUpsertForSpawn, sessionID, "hash-1", 7200)
	// A snapshot in progress at the migration, started by the previous binary.
	if _, err := db.ExecContext(ctx, `UPDATE sandboxes SET status = 'snapshotting' WHERE session_id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}
	previousRow(ctx, t, db, sandboxColumns, preRotationPendingSnapshot, sessionID, "cmd-before")

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(sandboxRotationMigration); err != nil {
		t.Fatalf("up to %d: %v", sandboxRotationMigration, err)
	}
	want := map[string]string{
		"sandboxes.rotation_gen":              "YES  integer",
		"sandboxes.rotation_message_id":       "YES  text",
		"sandboxes.rotation_started_at":       "YES  timestamp with time zone",
		"sandboxes.snapshot_started_at":       "YES  timestamp with time zone",
		"sandboxes.snapshot_start_message_id": "YES  text",
		"sandboxes.heartbeat_ready_seq":       "YES  integer",
		"sandboxes.heartbeats_since_ready":    "YES  smallint",
	}
	if got := columns(); len(got) != len(want) {
		t.Fatalf("columns at %d = %v, want %v", sandboxRotationMigration, got, want)
	} else {
		for name, shape := range want {
			if got[name] != shape {
				t.Fatalf("%s is (nullable, default, type) %q, want %q", name, got[name], shape)
			}
		}
	}

	// No backfill: nothing reads rotating, and the snapshot in progress has
	// no start until this release's first look gives it one.
	if c := readRotationColumns(ctx, t, db, sessionID); c.rotationGen.Valid || c.rotationMessageID.Valid || c.rotationStartedAt.Valid || c.snapshotAt.Valid {
		t.Fatalf("a sandbox at the migration reads %v; want nothing recorded", c)
	}
	if f := readRunwayFacts(ctx, t, db, sessionID); f.rotating || f.ownSnapshot || f.startKnown || !f.replayPending {
		t.Fatalf("runway at the migration: %+v; want nothing rotating, no start, the replay not known to be read", f)
	}
	stamp := generatedQuery(t, "sandboxes.sql.go", "stampSandboxSnapshotStart")
	if res, err := db.ExecContext(ctx, stamp, sessionID, 1); err != nil {
		t.Fatalf("this release's StampSandboxSnapshotStart: %v", err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("StampSandboxSnapshotStart affected %d rows, want 1", n)
	}
	if f := readRunwayFacts(ctx, t, db, sessionID); !f.startKnown {
		t.Fatal("the snapshot in progress has no start after this release's first look")
	}

	// This release rotates gen 1 once it is ready again.
	if _, err := db.ExecContext(ctx, `UPDATE sandboxes SET status = 'ready', pending_snapshot_message_id = NULL WHERE session_id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}
	start := generatedQuery(t, "sandboxes.sql.go", "startSandboxRotation")
	if res, err := db.ExecContext(ctx, start, "cmd-rotation", sessionID, 1); err != nil {
		t.Fatalf("this release's StartSandboxRotation: %v", err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("StartSandboxRotation affected %d rows, want 1", n)
	}
	if c := readRotationColumns(ctx, t, db, sessionID); !c.rotationMessageID.Valid || c.rotationMessageID.String != "cmd-rotation" {
		t.Fatalf("after this release's rotation: %v; want its snapshot command recorded", c)
	}
	if f := readRunwayFacts(ctx, t, db, sessionID); !f.rotating || f.ownSnapshot {
		t.Fatalf("this release's rotation on a ready row reads %+v; want rotating, no snapshot in progress", f)
	}
	// Its snapshot in progress reads as the rotation's own.
	if _, err := db.ExecContext(ctx, `UPDATE sandboxes SET status = 'snapshotting' WHERE session_id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}
	if f := readRunwayFacts(ctx, t, db, sessionID); !f.rotating || !f.ownSnapshot {
		t.Fatalf("the rotation's snapshot in progress reads %+v; want rotating, its own snapshot", f)
	}
	if _, err := db.ExecContext(ctx, `UPDATE sandboxes SET status = 'ready' WHERE session_id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}

	// The previous binary, still running during a rolling deploy: it reads
	// sandboxes with the columns present; its claim bumps the gen and leaves
	// the rotation on gen 1, which reads "not rotating" now.
	previousRow(ctx, t, db, sandboxColumns, preRotationGetSandbox, sessionID)
	previousRow(ctx, t, db, sandboxColumns, preRotationUpsertForSpawn, sessionID, "hash-2", 7200)
	c := readRotationColumns(ctx, t, db, sessionID)
	if c.gen != 2 || !c.rotationGen.Valid || c.rotationGen.Int32 != 1 || !c.snapshotAt.Valid {
		t.Fatalf("after the previous binary's claim: %v; want gen 2 with gen 1's rotation and start left", c)
	}
	if f := readRunwayFacts(ctx, t, db, sessionID); f.rotating {
		t.Fatal("a rotation left on the replaced gen reads rotating")
	}
	// A snapshot it starts finds the start left there, stamped for the
	// rotation's command: not its own, so this release's first look gives
	// it one, as the migration's doc says.
	if _, err := db.ExecContext(ctx, `UPDATE sandboxes SET status = 'snapshotting' WHERE session_id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}
	previousRow(ctx, t, db, sandboxColumns, preRotationPendingSnapshot, sessionID, "cmd-previous-binary")
	if f := readRunwayFacts(ctx, t, db, sessionID); f.startKnown || f.ownSnapshot {
		t.Fatalf("the previous binary's snapshot reads %+v; want no start of its own -- the one left on the row was stamped for another command -- and not a rotation's", f)
	}
	if res, err := db.ExecContext(ctx, stamp, sessionID, 2); err != nil {
		t.Fatalf("this release's StampSandboxSnapshotStart: %v", err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("StampSandboxSnapshotStart on the previous binary's snapshot affected %d rows, want 1", n)
	}
	if f := readRunwayFacts(ctx, t, db, sessionID); !f.startKnown {
		t.Fatal("the previous binary's snapshot has no start after this release's first look")
	}
	// This release's claim clears everything the rotation recorded.
	upsert := generatedQuery(t, "sandboxes.sql.go", "upsertSandboxForSpawn")
	if _, err := db.ExecContext(ctx, upsert, sessionID, "hash-3", 7200); err != nil {
		t.Fatalf("this release's UpsertSandboxForSpawn: %v", err)
	}
	third := readRotationColumns(ctx, t, db, sessionID)
	if third.gen != 3 || third.rotationGen.Valid || third.rotationMessageID.Valid || third.rotationStartedAt.Valid || third.snapshotAt.Valid ||
		third.startMessageID.Valid {
		t.Fatalf("after this release's claim: %v; want gen 3 with nothing recorded", third)
	}
	// Leave a rotation recorded, to see it kept across a rollback.
	if _, err := db.ExecContext(ctx, start, "cmd-rotation-3", sessionID, 3); err != nil {
		t.Fatal(err)
	}
	kept := readRotationColumns(ctx, t, db, sessionID)

	// It cannot boot on this version: golang-migrate refuses a version it
	// has no file for.
	previousMigrate, pdb := previousBinaryMigrate(t, connStr, int(previous))
	if err := previousMigrate.Up(); err == nil || !strings.Contains(err.Error(), fmt.Sprint(sandboxRotationMigration)) {
		t.Fatalf("the previous binary's boot on %d = %v, want a refusal naming %d", sandboxRotationMigration, err, sandboxRotationMigration)
	}
	_ = pdb.Close()

	// Rolling back with the columns kept: force the previous version, and
	// the previous binary boots and works. Deploying this release again
	// runs the migration again, which keeps the columns and their values.
	if err := m.Force(int(previous)); err != nil {
		t.Fatalf("force %d: %v", previous, err)
	}
	previousMigrate, pdb = previousBinaryMigrate(t, connStr, int(previous))
	if err := previousMigrate.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("the previous binary's boot after force %d = %v, want no change", previous, err)
	}
	_ = pdb.Close()
	previousRow(ctx, t, db, sandboxColumns, preRotationGetSandbox, sessionID)
	again, adb := newMigrate(t, connStr)
	if err := again.Migrate(sandboxRotationMigration); err != nil {
		t.Fatalf("this release's migration after the rollback = %v, want it applied again", err)
	}
	_ = adb.Close()
	assertCleanVersion(t, connStr, sandboxRotationMigration)
	if c := readRotationColumns(ctx, t, db, sessionID); c.String() != kept.String() {
		t.Fatalf("after the migration ran again: %v; want %v, kept", c, kept)
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
	previousRow(ctx, t, db, sandboxColumns, preRotationGetSandbox, sessionID)
	if err := m.Force(sandboxRotationMigration); err != nil {
		t.Fatalf("force %d: %v", sandboxRotationMigration, err)
	}
	if err := m.Migrate(previous); err != nil {
		t.Fatalf("down to %d again, the columns already gone: %v", previous, err)
	}
	if err := m.Migrate(sandboxRotationMigration); err != nil {
		t.Fatalf("up to %d again: %v", sandboxRotationMigration, err)
	}
	assertCleanVersion(t, connStr, sandboxRotationMigration)
	if got := columns(); len(got) != len(want) {
		t.Fatalf("columns after up again = %v, want %v", got, want)
	}
}
