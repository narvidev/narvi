//go:build integration

package postgres_test

import (
	"context"
	"strings"
	"testing"
)

// This file runs migration 000151 (session_stop_requested_at) through
// golang-migrate against real Postgres, in a database of its own migrated to
// 150 first. The up adds the nullable stop-request columns on sessions and
// turns (technical plan §3.3); the down drops them, and every armed `stop`
// timer with them, leaving every other row and timer.

func TestMigrationSessionStopRequestedAt_UpAndDown(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 150)

	columns := func() string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT table_name || '.' || column_name FROM information_schema.columns
			WHERE column_name = 'stop_requested_at' AND table_name IN ('sessions', 'turns') ORDER BY table_name`)
		if err != nil {
			t.Fatalf("read columns: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var got []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			got = append(got, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, ",")
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}

	if got := columns(); got != "" {
		t.Fatalf("columns at 150 = %q, want none", got)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(151); err != nil {
		t.Fatalf("up to 151: %v", err)
	}
	if got := columns(); got != "sessions.stop_requested_at,turns.stop_requested_at" {
		t.Fatalf("columns at 151 = %q, want both", got)
	}

	// Both are nullable with no default: a row written without them reads
	// NULL, one flagged keeps its instant.
	var sessionID string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('web') RETURNING id`).Scan(&sessionID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO turns (session_id, status) VALUES ($1, 'pending'), ($1, 'pending')`, sessionID); err != nil {
		t.Fatalf("insert turns: %v", err)
	}
	if n := count(`SELECT count(*) FROM turns WHERE session_id = $1 AND stop_requested_at IS NULL`, sessionID); n != 2 {
		t.Fatalf("%d unflagged turns, want 2: the column must default to NULL", n)
	}
	if _, err := db.ExecContext(ctx, `UPDATE turns SET stop_requested_at = now() WHERE session_id = $1`, sessionID); err != nil {
		t.Fatalf("flag turns: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE sessions SET stop_requested_at = now() WHERE id = $1`, sessionID); err != nil {
		t.Fatalf("flag session: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO session_timers (session_id, name, fires_at) VALUES ($1, 'stop', now()), ($1, 'inactivity', now() + interval '1 hour')`, sessionID); err != nil {
		t.Fatalf("arm timers: %v", err)
	}

	// Down: the columns and the stop timers go; the rows and every other
	// timer stay.
	if err := m.Migrate(150); err != nil {
		t.Fatalf("down to 150: %v", err)
	}
	if got := columns(); got != "" {
		t.Fatalf("columns after down = %q, want none", got)
	}
	if n := count(`SELECT count(*) FROM turns WHERE session_id = $1`, sessionID); n != 2 {
		t.Fatalf("%d turns after down, want 2", n)
	}
	if n := count(`SELECT count(*) FROM session_timers WHERE session_id = $1 AND name = 'stop'`, sessionID); n != 0 {
		t.Fatalf("%d stop timers after down, want none", n)
	}
	if n := count(`SELECT count(*) FROM session_timers WHERE session_id = $1 AND name = 'inactivity'`, sessionID); n != 1 {
		t.Fatalf("%d inactivity timers after down, want the one armed", n)
	}

	// Up again works on that state.
	if err := m.Migrate(151); err != nil {
		t.Fatalf("up to 151 again: %v", err)
	}
	assertCleanVersion(t, connStr, 151)
}
