//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// This file runs migration 000160 (owed_review_requests) through
// golang-migrate against real Postgres, in a database of its own migrated
// to 159 first. The up adds what technical plan §24.9's owed human review
// requests read and write: the owed_review_requests table and its index,
// and, beside each turn, who asked for it, the lane's own text and the
// moves in a row its request met. The down removes them, and every armed
// owed_review_request timer with them.
//
// It also pins what the migration's own "Rolling deploy" and "Rolling
// back" sections say of the previous binary: its turn statements -- copied
// below verbatim from the sqlc output it was built with -- run with the
// table and the columns present and leave them alone; it cannot boot on
// 160; and it can once the recorded version is forced back to 159 with
// them kept, after which this release's migration runs again, keeps their
// values, and arms the timer again for a request still owed.

// The previous binary's turn statements, as its sqlc output sent them: the
// column list of every SELECT * and RETURNING * on turns, with 000159's
// columns, and its re-review hold, which reads no owed request.
const (
	previous160TurnColumns = previous159TurnColumns + ", end_reason, context_unconfirmed_at, request_trigger"
	previous160CreateTurn  = `INSERT INTO turns (session_id, status, prompt, model_id, plan_mode, effort, review_head_sha, answer_only, review_depth, review_depth_decision, review_knowledge_mode, review_knowledge_decision, correlation_id, review_verdict_context, is_review_attempt, request_trigger)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
RETURNING ` + previous160TurnColumns
	previous160UpdateTurnStatus = `UPDATE turns
SET status = $2,
    dispatched_at = COALESCE($3, dispatched_at),
    completed_at = COALESCE($4, completed_at),
    dispatched_sandbox_gen = COALESCE($5, dispatched_sandbox_gen),
    dispatched_event_id = COALESCE($6, dispatched_event_id),
    dispatched_message_id = COALESCE($7, dispatched_message_id),
    end_reason = COALESCE($8, end_reason)
WHERE id = $1
RETURNING ` + previous160TurnColumns
	previous160GetTurn             = `SELECT ` + previous160TurnColumns + ` FROM turns WHERE id = $1`
	previous160ReviewRetriggerHeld = `SELECT EXISTS (
    SELECT 1 FROM turns t
    WHERE t.session_id = $1
      AND t.status IN ('pending', 'dispatched', 'processing')
) AS held`
)

// owedColumns160 is the part of a turn 000160 adds.
type owedColumns160 struct {
	requestedBy, requestText *string
	moves                    *int32
}

func readOwedColumns160(ctx context.Context, t *testing.T, db *sql.DB, turnID string) owedColumns160 {
	t.Helper()
	var row owedColumns160
	if err := db.QueryRowContext(ctx, `SELECT requested_by::text, request_text, context_moves FROM turns WHERE id = $1`, turnID).
		Scan(&row.requestedBy, &row.requestText, &row.moves); err != nil {
		t.Fatalf("read the owed columns: %v", err)
	}
	return row
}

func TestMigrationOwedReviewRequests_UpAndDown(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 159)
	turnColumns := len(strings.Split(previous160TurnColumns, ","))

	shape := func() map[string]string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT table_name || '.' || column_name, is_nullable || ' ' || coalesce(column_default, '') || ' ' || data_type
			FROM information_schema.columns
			WHERE (table_name = 'turns' AND column_name IN ('requested_by', 'request_text', 'context_moves'))
			   OR table_name = 'owed_review_requests'`)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		var scanErr error
		for rows.Next() {
			var name, s string
			if scanErr = rows.Scan(&name, &s); scanErr != nil {
				break
			}
			got[name] = s
		}
		iterErr := rows.Err()
		_ = rows.Close()
		if scanErr != nil {
			t.Fatal(scanErr)
		}
		if iterErr != nil {
			t.Fatal(iterErr)
		}
		return got
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}

	if got := shape(); len(got) != 0 {
		t.Fatalf("owed columns at 159: %v, want none", got)
	}
	var sessionID, turnID, userID string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('github') RETURNING id::text`).Scan(&sessionID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO users (primary_email, display_name, role) VALUES ('m160@example.com', 'Requester', 'maintainer') RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO turns (session_id, status, is_review_attempt, request_trigger) VALUES ($1, 'pending', true, 'button') RETURNING id::text`, sessionID).Scan(&turnID); err != nil {
		t.Fatalf("insert a turn at 159: %v", err)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(160); err != nil {
		t.Fatalf("up to 160: %v", err)
	}
	want := map[string]string{
		"turns.requested_by":                     "YES  uuid",
		"turns.request_text":                     "YES  text",
		"turns.context_moves":                    "YES  integer",
		"owed_review_requests.id":                "NO gen_random_uuid() uuid",
		"owed_review_requests.session_id":        "NO  uuid",
		"owed_review_requests.requested_by":      "YES  uuid",
		"owed_review_requests.trigger":           "NO  text",
		"owed_review_requests.request_text":      "YES  text",
		"owed_review_requests.is_review_attempt": "NO  boolean",
		"owed_review_requests.context_moves":     "NO  integer",
		"owed_review_requests.moved_turn_id":     "NO  uuid",
		"owed_review_requests.created_at":        "NO now() timestamp with time zone",
	}
	if got := shape(); len(got) != len(want) {
		t.Fatalf("shape at 160 = %v, want %v", got, want)
	} else {
		for name, s := range want {
			if got[name] != s {
				t.Fatalf("%s is (nullable, default, type) %q, want %q", name, got[name], s)
			}
		}
	}
	if n := count(`SELECT count(*) FROM pg_indexes WHERE tablename = 'owed_review_requests' AND indexname = 'owed_review_requests_session_id_idx' AND indexdef LIKE '%(session_id, created_at)%'`); n != 1 {
		t.Fatalf("owed_review_requests_session_id_idx on (session_id, created_at): %d, want 1", n)
	}
	// An existing turn records no requester, no text, no move.
	if row := readOwedColumns160(ctx, t, db, turnID); row.requestedBy != nil || row.requestText != nil || row.moves != nil {
		t.Fatalf("an existing turn at 160: %+v, want no requester, no text, no move", row)
	}

	// This release: the turn records its request; its move owes it, and arms
	// the owed timer.
	if _, err := db.ExecContext(ctx, `UPDATE turns SET requested_by = $2, request_text = 'Manual re-review requested via the web review button.', context_moves = 2, status = 'failed', end_reason = 'context_moved' WHERE id = $1`, turnID, userID); err != nil {
		t.Fatalf("this release records the request: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO owed_review_requests (session_id, requested_by, trigger, request_text, is_review_attempt, context_moves, moved_turn_id)
		VALUES ($1, $2, 'button', 'Manual re-review requested via the web review button.', true, 3, $3)`, sessionID, userID, turnID); err != nil {
		t.Fatalf("this release owes the request: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO session_timers (session_id, name, fires_at) VALUES ($1, 'owed_review_request', now())`, sessionID); err != nil {
		t.Fatalf("this release arms the owed timer: %v", err)
	}

	// The previous binary, still running during a rolling deploy: its
	// statements run with the table and the columns present and leave them
	// alone -- a turn it creates records no requester, and its hold reads
	// no owed request.
	var previousTurn string
	if err := db.QueryRowContext(ctx, `WITH created AS (`+previous160CreateTurn+`) SELECT id::text FROM created`,
		sessionID, "pending", nil, nil, false, nil, nil, nil, nil, nil, nil, nil, nil, nil, true, "label").Scan(&previousTurn); err != nil {
		t.Fatalf("the previous binary creates a turn at 160: %v", err)
	}
	previousRow(ctx, t, db, turnColumns, previous160GetTurn, previousTurn)
	previousRow(ctx, t, db, turnColumns, previous160UpdateTurnStatus, turnID, "failed", nil, nil, nil, nil, nil, nil)
	previousRow(ctx, t, db, 1, previous160ReviewRetriggerHeld, sessionID)
	if row := readOwedColumns160(ctx, t, db, previousTurn); row.requestedBy != nil || row.requestText != nil || row.moves != nil {
		t.Fatalf("the previous binary's turn: %+v, want no requester, no text, no move", row)
	}
	if row := readOwedColumns160(ctx, t, db, turnID); row.requestedBy == nil || row.requestText == nil || row.moves == nil || *row.moves != 2 {
		t.Fatalf("after the previous binary's writes: %+v, want the request left alone", row)
	}

	// It cannot boot on 160.
	previous, pdb := previousBinaryMigrate(t, connStr, 159)
	if err := previous.Up(); err == nil || !strings.Contains(err.Error(), "160") {
		t.Fatalf("the previous binary's boot on 160 = %v, want a refusal naming 160", err)
	}
	_ = pdb.Close()

	// Rolling back with the table and the columns kept: force 159, and the
	// previous binary boots and works. Meanwhile §2's bound deletes the owed
	// timer it does not know on one session, and backs it off, ten minutes
	// out, on another. Deploying this release again runs 000160 again,
	// which keeps the table, the columns and their values, and makes the
	// timer due at once for every request still owed: inserted where the
	// bound deleted it, moved to now where it survived.
	if err := m.Force(159); err != nil {
		t.Fatalf("force 159: %v", err)
	}
	previous, pdb = previousBinaryMigrate(t, connStr, 159)
	if err := previous.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("the previous binary's boot after force 159 = %v, want no change", err)
	}
	_ = pdb.Close()
	previousRow(ctx, t, db, turnColumns, previous160GetTurn, turnID)
	if _, err := db.ExecContext(ctx, `DELETE FROM session_timers WHERE session_id = $1 AND name = 'owed_review_request'`, sessionID); err != nil {
		t.Fatal(err)
	}
	var survivorSession, survivorTurn string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('github') RETURNING id::text`).Scan(&survivorSession); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO turns (session_id, status, is_review_attempt, request_trigger, end_reason) VALUES ($1, 'failed', true, 'label', 'context_moved') RETURNING id::text`, survivorSession).Scan(&survivorTurn); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO owed_review_requests (session_id, trigger, is_review_attempt, context_moves, moved_turn_id) VALUES ($1, 'label', true, 1, $2)`, survivorSession, survivorTurn); err != nil {
		t.Fatal(err)
	}
	// Armed two hours ago and backed off by the previous binary: due ten
	// minutes from now.
	if _, err := db.ExecContext(ctx, `INSERT INTO session_timers (session_id, name, fires_at, armed_at, created_at)
		VALUES ($1, 'owed_review_request', now() + interval '10 minutes', now() - interval '2 hours', now() - interval '2 hours')`, survivorSession); err != nil {
		t.Fatal(err)
	}
	again, adb := newMigrate(t, connStr)
	if err := again.Migrate(160); err != nil {
		t.Fatalf("this release's migration after the rollback = %v, want 000160 applied again", err)
	}
	_ = adb.Close()
	assertCleanVersion(t, connStr, 160)
	if row := readOwedColumns160(ctx, t, db, turnID); row.requestedBy == nil || row.moves == nil || *row.moves != 2 {
		t.Fatalf("after 000160 ran again: %+v, want the values kept", row)
	}
	if n := count(`SELECT count(*) FROM owed_review_requests WHERE session_id = $1 AND context_moves = 3`, sessionID); n != 1 {
		t.Fatalf("owed requests after 000160 ran again = %d, want the one kept", n)
	}
	if n := count(`SELECT count(*) FROM session_timers WHERE session_id = $1 AND name = 'owed_review_request' AND fires_at <= now()`, sessionID); n != 1 {
		t.Fatalf("owed timers after 000160 ran again = %d, want the one the request is owed armed again, due", n)
	}
	if n := count(`SELECT count(*) FROM session_timers WHERE session_id = $1 AND name = 'owed_review_request' AND fires_at <= now() AND armed_at > now() - interval '1 minute'`, survivorSession); n != 1 {
		t.Fatalf("the surviving owed timer after 000160 ran again: %d due and freshly armed, want 1", n)
	}

	// Down: the table and the columns go, every owed timer with them, every
	// turn stays. Run twice -- the second time on a database a rollback
	// already brought back to 159 with them dropped, forced to 160 again --
	// which the IF EXISTS guards allow. Up again works on that state.
	turnsBefore := count(`SELECT count(*) FROM turns`)
	if err := m.Migrate(159); err != nil {
		t.Fatalf("down to 159: %v", err)
	}
	if got := shape(); len(got) != 0 {
		t.Fatalf("shape after the down: %v, want none", got)
	}
	if n := count(`SELECT count(*) FROM session_timers WHERE name = 'owed_review_request'`); n != 0 {
		t.Fatalf("owed timers after the down = %d, want none", n)
	}
	if n := count(`SELECT count(*) FROM turns`); n != turnsBefore {
		t.Fatalf("turns after the down = %d, want %d", n, turnsBefore)
	}
	previousRow(ctx, t, db, turnColumns, previous160GetTurn, turnID)
	if err := m.Force(160); err != nil {
		t.Fatalf("force 160: %v", err)
	}
	if err := m.Migrate(159); err != nil {
		t.Fatalf("down to 159 again, the table and the columns already gone: %v", err)
	}
	if err := m.Migrate(160); err != nil {
		t.Fatalf("up to 160 again: %v", err)
	}
	assertCleanVersion(t, connStr, 160)
	if got := shape(); len(got) != len(want) {
		t.Fatalf("shape after up again = %v, want %v", got, want)
	}
}
