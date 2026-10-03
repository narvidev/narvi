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

// This file runs migration 000159 (turns_end_reason) through golang-migrate
// against real Postgres, in a database of its own migrated to 158 first.
// The up adds what technical plan §24.9's context check of a queued
// automatic review attempt reads and writes: a turn's end reason, when it
// started with its context unconfirmed, and which lane asked for it; and,
// beside the pull request, the count of moved contexts in a row and the
// record of a drop. The down removes them.
//
// It also pins what the migration's own "Rolling deploy" and "Rolling
// back" sections say of the previous binary: its turn and pull request
// statements -- copied below verbatim from the sqlc output it was built
// with -- run with the columns present and leave them alone; it cannot boot
// on 159; and it can once the recorded version is forced back to 158 with
// the columns kept, after which this release's migration runs again and
// keeps their values.

// The previous binary's turn and pull request statements, as its sqlc
// output sent them: the column lists of every SELECT * and RETURNING * on
// turns (with 000155's receipt columns) and on github_pr_sessions.
const (
	previous159TurnColumns     = previousTurnColumns + ", receipt_requested_message_id, receipt_requested_at, receipt_checked_ready_seq, receipt_resend_count"
	previous159PRSessionColumn = "repo_full_name, pr_number, session_id, claimed_at, pending_retrigger_head_sha, auto_retrigger_count, auto_retrigger_budget_notice_sent_at, pr_merged, pr_closed_at, mention_count"
	previous159CreateTurn      = `INSERT INTO turns (session_id, status, prompt, model_id, plan_mode, effort, review_head_sha, answer_only, review_depth, review_depth_decision, review_knowledge_mode, review_knowledge_decision, correlation_id, review_verdict_context, is_review_attempt)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
RETURNING ` + previous159TurnColumns
	previous159UpdateTurnStatus = `UPDATE turns
SET status = $2,
    dispatched_at = COALESCE($3, dispatched_at),
    completed_at = COALESCE($4, completed_at),
    dispatched_sandbox_gen = COALESCE($5, dispatched_sandbox_gen),
    dispatched_event_id = COALESCE($6, dispatched_event_id),
    dispatched_message_id = COALESCE($7, dispatched_message_id)
WHERE id = $1
RETURNING ` + previous159TurnColumns
	previous159GetTurn                       = `SELECT ` + previous159TurnColumns + ` FROM turns WHERE id = $1`
	previous159UpsertPendingRetriggerHeadSHA = `UPDATE github_pr_sessions
SET pending_retrigger_head_sha = $3
WHERE repo_full_name = $1 AND pr_number = $2 AND session_id IS NOT NULL
RETURNING ` + previous159PRSessionColumn
	previous159GetPRSession = `SELECT ` + previous159PRSessionColumn + ` FROM github_pr_sessions WHERE session_id = $1`
)

// contextColumns159 is the part of a turn and its pull request 000159 is
// about.
type contextColumns159 struct {
	endReason, requestTrigger, droppedHead *string
	unconfirmed, dropped                   bool
	moves                                  int32
}

func readContextColumns159(ctx context.Context, t *testing.T, db *sql.DB, turnID, sessionID string) contextColumns159 {
	t.Helper()
	var row contextColumns159
	if err := db.QueryRowContext(ctx, `SELECT t.end_reason, t.request_trigger, t.context_unconfirmed_at IS NOT NULL,
			g.auto_retrigger_context_moves, g.auto_retrigger_dropped_at IS NOT NULL, g.auto_retrigger_dropped_head_sha
		FROM turns t JOIN github_pr_sessions g ON g.session_id = t.session_id
		WHERE t.id = $1 AND t.session_id = $2`, turnID, sessionID).
		Scan(&row.endReason, &row.requestTrigger, &row.unconfirmed, &row.moves, &row.dropped, &row.droppedHead); err != nil {
		t.Fatalf("read the context columns: %v", err)
	}
	return row
}

func TestMigrationTurnsEndReason_UpAndDown(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 158)
	turnColumns := len(strings.Split(previous159TurnColumns, ","))
	prColumns := len(strings.Split(previous159PRSessionColumn, ","))

	columns := func() map[string]string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT table_name || '.' || column_name, is_nullable || ' ' || coalesce(column_default, '') || ' ' || data_type
			FROM information_schema.columns
			WHERE (table_name = 'turns' AND column_name IN ('end_reason', 'context_unconfirmed_at', 'request_trigger'))
			   OR (table_name = 'github_pr_sessions' AND column_name IN ('auto_retrigger_context_moves', 'auto_retrigger_dropped_at', 'auto_retrigger_dropped_head_sha'))`)
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
		t.Fatalf("context columns at 158: %v, want none", got)
	}
	var sessionID, turnID string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('github') RETURNING id::text`).Scan(&sessionID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO github_pr_sessions (repo_full_name, pr_number, session_id, pending_retrigger_head_sha) VALUES ('acme/m159', 1, $1, 'sha-before')`, sessionID); err != nil {
		t.Fatalf("insert the pull request claim: %v", err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO turns (session_id, status, is_review_attempt) VALUES ($1, 'pending', true) RETURNING id::text`, sessionID).Scan(&turnID); err != nil {
		t.Fatalf("insert a turn at 158: %v", err)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(159); err != nil {
		t.Fatalf("up to 159: %v", err)
	}
	want := map[string]string{
		"turns.end_reason":                                   "YES  text",
		"turns.context_unconfirmed_at":                       "YES  timestamp with time zone",
		"turns.request_trigger":                              "YES  text",
		"github_pr_sessions.auto_retrigger_context_moves":    "NO 0 integer",
		"github_pr_sessions.auto_retrigger_dropped_at":       "YES  timestamp with time zone",
		"github_pr_sessions.auto_retrigger_dropped_head_sha": "YES  text",
	}
	if got := columns(); len(got) != len(want) {
		t.Fatalf("columns at 159 = %v, want %v", got, want)
	} else {
		for name, shape := range want {
			if got[name] != shape {
				t.Fatalf("%s is (nullable, default, type) %q, want %q", name, got[name], shape)
			}
		}
	}

	// An existing turn ended for no reason of its own, was asked for by no
	// recorded lane; an existing pull request is at no move and no drop.
	if row := readContextColumns159(ctx, t, db, turnID, sessionID); row.endReason != nil || row.requestTrigger != nil || row.unconfirmed || row.moves != 0 || row.dropped || row.droppedHead != nil {
		t.Fatalf("existing rows at 159: %+v, want every new column empty, the count 0", row)
	}

	// This release: the attempt ends context_moved, the request is
	// dropped.
	if _, err := db.ExecContext(ctx, `UPDATE turns SET status = 'failed', completed_at = now(), end_reason = 'context_moved', request_trigger = 'auto' WHERE id = $1`, turnID); err != nil {
		t.Fatalf("this release ends the attempt: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE github_pr_sessions SET auto_retrigger_context_moves = 4, auto_retrigger_dropped_at = now(), auto_retrigger_dropped_head_sha = 'sha-before', pending_retrigger_head_sha = NULL WHERE session_id = $1`, sessionID); err != nil {
		t.Fatalf("this release drops the request: %v", err)
	}

	// The previous binary, still running during a rolling deploy: its
	// statements run with the columns present and leave them alone -- a
	// turn it creates has no trigger, its status writes keep an end reason,
	// and its synchronize handler re-arms without clearing the drop.
	var previousTurn string
	if err := db.QueryRowContext(ctx, `WITH created AS (`+previous159CreateTurn+`) SELECT id::text FROM created`,
		sessionID, "pending", nil, nil, false, nil, nil, nil, nil, nil, nil, nil, nil, nil, true).Scan(&previousTurn); err != nil {
		t.Fatalf("the previous binary creates a turn at 159: %v", err)
	}
	previousRow(ctx, t, db, turnColumns, previous159GetTurn, previousTurn)
	previousRow(ctx, t, db, turnColumns, previous159UpdateTurnStatus, turnID, "failed", nil, nil, nil, nil, nil)
	previousRow(ctx, t, db, prColumns, previous159UpsertPendingRetriggerHeadSHA, "acme/m159", 1, "sha-pushed-on-the-previous-binary")
	previousRow(ctx, t, db, prColumns, previous159GetPRSession, sessionID)
	if row := readContextColumns159(ctx, t, db, previousTurn, sessionID); row.requestTrigger != nil || row.endReason != nil {
		t.Fatalf("the previous binary's turn: %+v, want no trigger and no end reason", row)
	}
	if row := readContextColumns159(ctx, t, db, turnID, sessionID); row.endReason == nil || *row.endReason != "context_moved" || row.moves != 4 || !row.dropped {
		t.Fatalf("after the previous binary's writes: %+v, want the end reason, the count and the drop left alone", row)
	}

	// It cannot boot on 159.
	previous, pdb := previousBinaryMigrate(t, connStr, 158)
	if err := previous.Up(); err == nil || !strings.Contains(err.Error(), "159") {
		t.Fatalf("the previous binary's boot on 159 = %v, want a refusal naming 159", err)
	}
	_ = pdb.Close()

	// Rolling back with the columns kept: force 158, and the previous
	// binary boots and works. Deploying this release again runs 000159
	// again, which keeps the columns and their values.
	if err := m.Force(158); err != nil {
		t.Fatalf("force 158: %v", err)
	}
	previous, pdb = previousBinaryMigrate(t, connStr, 158)
	if err := previous.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("the previous binary's boot after force 158 = %v, want no change", err)
	}
	_ = pdb.Close()
	previousRow(ctx, t, db, turnColumns, previous159GetTurn, turnID)
	again, adb := newMigrate(t, connStr)
	if err := again.Migrate(159); err != nil {
		t.Fatalf("this release's migration after the rollback = %v, want 000159 applied again", err)
	}
	_ = adb.Close()
	assertCleanVersion(t, connStr, 159)
	if row := readContextColumns159(ctx, t, db, turnID, sessionID); row.endReason == nil || *row.endReason != "context_moved" || row.moves != 4 || !row.dropped {
		t.Fatalf("after 000159 ran again: %+v, want the values kept", row)
	}

	// Down: the columns go, every row stays. Run twice -- the second time
	// on a database a rollback already brought back to 158 with the
	// columns dropped, forced to 159 again -- which the IF EXISTS guard
	// allows. Up again works on that state.
	count := func(table string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	turnsBefore, prsBefore := count("turns"), count("github_pr_sessions")
	if err := m.Migrate(158); err != nil {
		t.Fatalf("down to 158: %v", err)
	}
	if got := columns(); len(got) != 0 {
		t.Fatalf("columns after the down: %v, want none", got)
	}
	if count("turns") != turnsBefore || count("github_pr_sessions") != prsBefore {
		t.Fatalf("rows after the down: turns %d, pull requests %d; want %d and %d", count("turns"), count("github_pr_sessions"), turnsBefore, prsBefore)
	}
	previousRow(ctx, t, db, turnColumns, previous159GetTurn, turnID)
	previousRow(ctx, t, db, prColumns, previous159GetPRSession, sessionID)
	if err := m.Force(159); err != nil {
		t.Fatalf("force 159: %v", err)
	}
	if err := m.Migrate(158); err != nil {
		t.Fatalf("down to 158 again, the columns already gone: %v", err)
	}
	if err := m.Migrate(159); err != nil {
		t.Fatalf("up to 159 again: %v", err)
	}
	assertCleanVersion(t, connStr, 159)
	if got := columns(); len(got) != len(want) {
		t.Fatalf("columns after up again = %v, want %v", got, want)
	}
}
