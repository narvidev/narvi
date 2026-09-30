//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
)

// This file runs migration 000153 (session_timers_armed_at) through
// golang-migrate against real Postgres, in a database of its own migrated
// to 152 first. The up adds armed_at, the instant of a timer row's latest
// arm that technical plan §2 ages a kind the session actor does not know
// from, backfilled from created_at; the down removes it.
//
// It also pins what the migration's own "Rolling deploy" and "Rolling back"
// sections say of the previous binary: that binary's own session_timers
// statements -- copied below verbatim from the sqlc output it was built
// with -- run with the column present, its insert gets the default and its
// re-arm leaves armed_at alone; it cannot boot on 153; and it can once the
// recorded version is forced back to 152 with the column kept, after which
// this release's migration runs again and keeps the column's values.

// previousTimerColumns is the column list the previous binary's sqlc output
// wrote out for every SELECT * and RETURNING * on session_timers.
const previousTimerColumns = "id, session_id, name, fires_at, created_at"

// The previous binary's session_timers statements, as its sqlc output sent
// them.
const (
	previousUpsertSessionTimer = `INSERT INTO session_timers (session_id, name, fires_at)
VALUES ($1, $2, $3)
ON CONFLICT (session_id, name) DO UPDATE
    SET fires_at = EXCLUDED.fires_at
RETURNING ` + previousTimerColumns
	previousGetSessionTimer = `SELECT ` + previousTimerColumns + ` FROM session_timers
WHERE session_id = $1 AND name = $2`
	previousListDueTimers = `SELECT ` + previousTimerColumns + ` FROM session_timers
WHERE fires_at <= now()
ORDER BY fires_at
LIMIT $1
FOR UPDATE SKIP LOCKED`
	previousClaimDueTimer = `UPDATE session_timers
SET fires_at = $1
WHERE session_id = $2 AND name = $3
RETURNING ` + previousTimerColumns
	previousDeleteSessionTimer = `DELETE FROM session_timers
WHERE session_id = $1 AND name = $2`
	previousListSessionTimers = `SELECT ` + previousTimerColumns + ` FROM session_timers
WHERE session_id = $1
ORDER BY name`
)

// scanPreviousTimerRow scans a whole timer row the way the previous binary
// does -- exactly its five columns -- and returns its fires_at.
func scanPreviousTimerRow(t *testing.T, row *sql.Row) time.Time {
	t.Helper()
	var (
		id, sessionID, name string
		firesAt, createdAt  time.Time
	)
	if err := row.Scan(&id, &sessionID, &name, &firesAt, &createdAt); err != nil {
		t.Fatalf("scan the previous binary's timer row: %v", err)
	}
	return firesAt
}

// runPreviousBinaryTimers runs one timer through every session_timers
// statement the previous binary sends -- arm, get, list due, claim, re-arm,
// list the session's timers, delete -- against the schema as it now is,
// and arms a second one it leaves in place, whose name it returns.
func runPreviousBinaryTimers(ctx context.Context, t *testing.T, db *sql.DB, sessionID string) (kept string) {
	t.Helper()
	scanPreviousTimerRow(t, db.QueryRowContext(ctx, previousUpsertSessionTimer, sessionID, "inactivity", time.Now().Add(-time.Second)))
	scanPreviousTimerRow(t, db.QueryRowContext(ctx, previousUpsertSessionTimer, sessionID, "liveness_check", time.Now().Add(time.Hour)))
	scanPreviousTimerRow(t, db.QueryRowContext(ctx, previousGetSessionTimer, sessionID, "inactivity"))

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(ctx, previousListDueTimers, 50)
	if err != nil {
		t.Fatalf("the previous binary's list due: %v", err)
	}
	var due int
	for rows.Next() {
		due++
	}
	listErr := rows.Err()
	_ = rows.Close()
	if listErr != nil || due == 0 {
		t.Fatalf("the previous binary's list due = %d rows (%v), want the timer it armed", due, listErr)
	}
	scanPreviousTimerRow(t, tx.QueryRowContext(ctx, previousClaimDueTimer, time.Now().Add(30*time.Second), sessionID, "inactivity"))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	scanPreviousTimerRow(t, db.QueryRowContext(ctx, previousUpsertSessionTimer, sessionID, "inactivity", time.Now().Add(time.Minute)))
	rows, err = db.QueryContext(ctx, previousListSessionTimers, sessionID)
	if err != nil {
		t.Fatalf("the previous binary's list for session: %v", err)
	}
	var listed int
	for rows.Next() {
		listed++
	}
	listErr = rows.Err()
	_ = rows.Close()
	if listErr != nil || listed < 2 {
		t.Fatalf("the previous binary's list for session = %d rows (%v), want at least its two timers", listed, listErr)
	}
	if _, err := db.ExecContext(ctx, previousDeleteSessionTimer, sessionID, "inactivity"); err != nil {
		t.Fatalf("the previous binary's delete: %v", err)
	}
	return "liveness_check"
}

func TestMigrationSessionTimersArmedAt_UpAndDown(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 152)

	hasColumn := func() bool {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'session_timers' AND column_name = 'armed_at'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	stamps := func(sessionID, name string) (createdAt, armedAt time.Time) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT created_at, armed_at FROM session_timers WHERE session_id = $1 AND name = $2`,
			sessionID, name).Scan(&createdAt, &armedAt); err != nil {
			t.Fatalf("read %s's created_at and armed_at: %v", name, err)
		}
		return createdAt, armedAt
	}
	countRows := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM session_timers`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if hasColumn() {
		t.Fatal("armed_at exists at 152")
	}
	var sessionID string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('web') RETURNING id::text`).Scan(&sessionID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	// A row the previous binary armed a day before the migration.
	scanPreviousTimerRow(t, db.QueryRowContext(ctx, previousUpsertSessionTimer, sessionID, "stop", time.Now().Add(time.Hour)))
	if _, err := db.ExecContext(ctx, `UPDATE session_timers SET created_at = now() - interval '1 day' WHERE session_id = $1 AND name = 'stop'`, sessionID); err != nil {
		t.Fatal(err)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(153); err != nil {
		t.Fatalf("up to 153: %v", err)
	}
	if !hasColumn() {
		t.Fatal("armed_at missing at 153")
	}
	var nullable, def string
	if err := db.QueryRowContext(ctx, `SELECT is_nullable, column_default FROM information_schema.columns
		WHERE table_name = 'session_timers' AND column_name = 'armed_at'`).Scan(&nullable, &def); err != nil {
		t.Fatal(err)
	}
	if nullable != "NO" || def != "now()" {
		t.Fatalf("armed_at is_nullable = %s, default = %s; want NO, now()", nullable, def)
	}
	if createdAt, armedAt := stamps(sessionID, "stop"); !armedAt.Equal(createdAt) {
		t.Fatalf("an existing row's armed_at = %v, want its created_at %v (the backfill)", armedAt, createdAt)
	}

	// The previous binary, still running during a rolling deploy, works
	// with the column present: its insert takes the default, its re-arm
	// leaves armed_at as it was.
	kept := runPreviousBinaryTimers(ctx, t, db, sessionID)
	if _, err := db.ExecContext(ctx, `UPDATE session_timers SET armed_at = armed_at - interval '1 hour' WHERE session_id = $1 AND name = $2`, sessionID, kept); err != nil {
		t.Fatal(err)
	}
	_, armedBefore := stamps(sessionID, kept)
	scanPreviousTimerRow(t, db.QueryRowContext(ctx, previousUpsertSessionTimer, sessionID, kept, time.Now().Add(2*time.Hour)))
	if _, armedAfter := stamps(sessionID, kept); !armedAfter.Equal(armedBefore) {
		t.Fatalf("armed_at after the previous binary's re-arm = %v, want %v, untouched", armedAfter, armedBefore)
	}

	// It cannot boot on 153: golang-migrate refuses a version it has no
	// file for.
	previous, pdb := previousBinaryMigrate(t, connStr, 152)
	if err := previous.Up(); err == nil || !strings.Contains(err.Error(), "153") {
		t.Fatalf("the previous binary's boot on 153 = %v, want a refusal naming 153", err)
	}
	_ = pdb.Close()

	// Rolling back with the column kept: force 152, and the previous binary
	// boots and works. Deploying this release again runs 000153 again,
	// which keeps the column and its values.
	if err := m.Force(152); err != nil {
		t.Fatalf("force 152: %v", err)
	}
	previous, pdb = previousBinaryMigrate(t, connStr, 152)
	if err := previous.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("the previous binary's boot after force 152 = %v, want no change", err)
	}
	_ = pdb.Close()
	runPreviousBinaryTimers(ctx, t, db, sessionID)
	_, armedKept := stamps(sessionID, kept)
	// Pinned to 153, like every migration test here: Up would also apply
	// whatever later migrations exist by the time this runs.
	again, adb := newMigrate(t, connStr)
	if err := again.Migrate(153); err != nil {
		t.Fatalf("this release's migration after the rollback = %v, want 000153 applied again", err)
	}
	_ = adb.Close()
	assertCleanVersion(t, connStr, 153)
	if _, armedAt := stamps(sessionID, kept); !armedAt.Equal(armedKept) {
		t.Fatalf("armed_at after 000153 ran again = %v, want %v, kept", armedAt, armedKept)
	}

	// Down: the column goes, every row stays; up again works on that state.
	before := countRows()
	if err := m.Migrate(152); err != nil {
		t.Fatalf("down to 152: %v", err)
	}
	if hasColumn() {
		t.Fatal("armed_at still exists after the down")
	}
	if after := countRows(); after != before {
		t.Fatalf("%d timer rows after the down, want %d", after, before)
	}
	if err := m.Migrate(153); err != nil {
		t.Fatalf("up to 153 again: %v", err)
	}
	assertCleanVersion(t, connStr, 153)
	if createdAt, armedAt := stamps(sessionID, "stop"); !armedAt.Equal(createdAt) {
		t.Fatalf("armed_at after up again = %v, want created_at %v (the backfill)", armedAt, createdAt)
	}
}
