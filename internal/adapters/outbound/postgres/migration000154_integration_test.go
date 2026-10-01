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
	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// This file runs migration 000154 (prompt_receipts) through golang-migrate
// against real Postgres, in a database of its own migrated to 153 first.
// The up adds what technical plan §3.3's prompt receipts read and write:
// four turn columns (the request a dispatch recorded, its instant, the
// last reconnect its check answered and its re-sends) and two sandbox
// columns (the gen
// whose latest ready advertised the capability, and the count of readies);
// the down removes them.
//
// It also pins what the migration's own "Rolling deploy" and "Rolling back"
// sections say of the previous binary: its turn and sandbox statements --
// copied below verbatim from the sqlc output it was built with -- run with
// the columns present and leave them alone, so its re-enqueue leaves a
// stale request that no longer matches the dispatch and reads as not
// asked, and its respawn leaves a capability that no longer matches the
// gen; it cannot boot on 154; and it can once the recorded version is
// forced back to 153 with the columns kept, after which this release's
// migration runs again and keeps their values.

// The column lists the previous binary's sqlc output wrote out for every
// SELECT * and RETURNING * on turns and on sandboxes.
const (
	previousTurnColumns    = "id, session_id, status, conversation_id, created_at, dispatched_at, completed_at, prompt, model_id, plan_mode, dispatched_sandbox_gen, progress_notified_at, effort, epistemic_outcome, review_head_sha, answer_only, review_depth, review_depth_decision, dispatched_event_id, cost_usd, review_knowledge_mode, review_knowledge_decision, correlation_id, review_verdict_context, dispatched_message_id, is_review_attempt, stop_requested_at"
	previousSandboxColumns = "id, session_id, gen, status, last_seen_at, created_at, updated_at, token_hash, provider_id, spawn_failure_count, last_spawn_failure_at, snapshot_id, pending_snapshot_message_id, pre_suspect_status, snapshot_suppressed_in_shadow, pending_push_suppressed_in_shadow, pending_push_cancelled, demotion_terminate_requested_at, agent_version, image_digest, image_decision_reason, image_decision_fingerprint, pr_delivery_started_at, boot_evidence_gen, booting_since, booting_since_gen, stop_retire_gen"
)

// The previous binary's turn and sandbox statements, as its sqlc output
// sent them.
const (
	previousCreateTurn = `INSERT INTO turns (session_id, status, prompt, model_id, plan_mode, effort, review_head_sha, answer_only, review_depth, review_depth_decision, review_knowledge_mode, review_knowledge_decision, correlation_id, review_verdict_context, is_review_attempt)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
RETURNING ` + previousTurnColumns
	previousUpdateTurnStatus = `UPDATE turns
SET status = $2,
    dispatched_at = COALESCE($3, dispatched_at),
    completed_at = COALESCE($4, completed_at),
    dispatched_sandbox_gen = COALESCE($5, dispatched_sandbox_gen),
    dispatched_event_id = COALESCE($6, dispatched_event_id),
    dispatched_message_id = COALESCE($7, dispatched_message_id)
WHERE id = $1
RETURNING ` + previousTurnColumns
	previousGetTurn            = `SELECT ` + previousTurnColumns + ` FROM turns WHERE id = $1`
	previousCreateSandbox      = `INSERT INTO sandboxes (session_id) VALUES ($1) RETURNING ` + previousSandboxColumns
	previousGetSandbox         = `SELECT ` + previousSandboxColumns + ` FROM sandboxes WHERE session_id = $1`
	previousUpsertSandboxSpawn = `INSERT INTO sandboxes (session_id, gen, status, token_hash)
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
RETURNING ` + previousSandboxColumns
)

// previousRow runs one of the previous binary's whole-row statements and
// fails unless it returns exactly one row of exactly want columns -- its
// own column list, nothing of this release's.
func previousRow(ctx context.Context, t *testing.T, db *sql.DB, want int, query string, args ...any) {
	t.Helper()
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatalf("the previous binary's statement: %v\n%s", err, query)
	}
	cols, err := rows.Columns()
	n := 0
	for rows.Next() {
		n++
	}
	iterErr := rows.Err()
	_ = rows.Close()
	if err != nil || iterErr != nil || n != 1 || len(cols) != want {
		t.Fatalf("the previous binary's statement returned %d rows of %d columns (%v, %v), want 1 of %d\n%s", n, len(cols), err, iterErr, want, query)
	}
}

func TestMigrationPromptReceipts_UpAndDown(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 153)
	turnColumns := len(strings.Split(previousTurnColumns, ","))
	sandboxColumns := len(strings.Split(previousSandboxColumns, ","))

	columns := func() map[string]string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT table_name || '.' || column_name, is_nullable || ' ' || coalesce(column_default, '')
			FROM information_schema.columns
			WHERE (table_name = 'turns' AND column_name IN ('receipt_requested_message_id', 'receipt_requested_at', 'receipt_checked_ready_seq', 'receipt_resend_count'))
			   OR (table_name = 'sandboxes' AND column_name IN ('prompt_receipt_gen', 'ready_seq'))`)
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
		t.Fatalf("prompt receipt columns at 153: %v, want none", got)
	}
	var sessionID string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('web') RETURNING id::text`).Scan(&sessionID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	previousRow(ctx, t, db, sandboxColumns, previousCreateSandbox, sessionID)
	var turnID string
	if err := db.QueryRowContext(ctx, `INSERT INTO turns (session_id, status) VALUES ($1, 'pending') RETURNING id::text`, sessionID).Scan(&turnID); err != nil {
		t.Fatalf("insert turn: %v", err)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(154); err != nil {
		t.Fatalf("up to 154: %v", err)
	}
	want := map[string]string{
		"turns.receipt_requested_message_id": "YES ",
		"turns.receipt_requested_at":         "YES ",
		"turns.receipt_checked_ready_seq":    "YES ",
		"turns.receipt_resend_count":         "NO 0",
		"sandboxes.prompt_receipt_gen":       "YES ",
		"sandboxes.ready_seq":                "NO 0",
	}
	if got := columns(); len(got) != len(want) {
		t.Fatalf("columns at 154 = %v, want %v", got, want)
	} else {
		for name, shape := range want {
			if got[name] != shape {
				t.Fatalf("%s is (nullable, default) %q, want %q", name, got[name], shape)
			}
		}
	}

	pool, err := narvipg.NewPool(ctx, connStr)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	q := sqlcgen.New(pool)
	var session pgtype.UUID
	if err := session.Scan(sessionID); err != nil {
		t.Fatal(err)
	}
	var turn pgtype.UUID
	if err := turn.Scan(turnID); err != nil {
		t.Fatal(err)
	}
	sandboxRow, err := q.GetSandbox(ctx, session)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if sandboxRow.ReadySeq != 0 || sandboxRow.PromptReceiptGen != nil {
		t.Fatalf("an existing sandbox reads ready_seq %d, prompt_receipt_gen %v; want 0, NULL", sandboxRow.ReadySeq, sandboxRow.PromptReceiptGen)
	}

	// This release: a capable ready, then a dispatch that asks.
	if err := q.RecordSandboxReady(ctx, sqlcgen.RecordSandboxReadyParams{PromptReceipt: true, SessionID: session, Gen: 1}); err != nil {
		t.Fatalf("record a ready: %v", err)
	}
	asked := "msg-asked"
	gen1 := int32(1)
	if _, err := q.UpdateTurnStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: turn, Status: sqlcgen.TurnStatusProcessing, DispatchedSandboxGen: &gen1, DispatchedMessageID: &asked}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := q.SetTurnPromptReceiptRequest(ctx, sqlcgen.SetTurnPromptReceiptRequestParams{MessageID: &asked, ReadySeq: 1, ID: turn}); err != nil {
		t.Fatalf("record the request: %v", err)
	}

	// The previous binary, still running during a rolling deploy: it reads
	// and creates turns and sandboxes with the columns present, its
	// re-enqueue to a respawned gen leaves the request standing under a
	// messageId the dispatch no longer has, and its respawn leaves the
	// capability on a gen that is no longer live.
	previousRow(ctx, t, db, sandboxColumns, previousGetSandbox, sessionID)
	previousRow(ctx, t, db, turnColumns, previousGetTurn, turnID)
	previousRow(ctx, t, db, turnColumns, previousCreateTurn, sessionID, "pending", nil, nil, false, nil, nil, nil, nil, nil, nil, nil, nil, nil, false)
	previousRow(ctx, t, db, sandboxColumns, previousUpsertSandboxSpawn, sessionID, "token-hash-2")
	previousRow(ctx, t, db, turnColumns, previousUpdateTurnStatus, turnID, "processing", time.Now(), nil, 2, nil, "msg-previous-binary")

	reenqueued, err := q.GetTurn(ctx, turn)
	if err != nil {
		t.Fatal(err)
	}
	if reenqueued.ReceiptRequestedMessageID == nil || *reenqueued.ReceiptRequestedMessageID != asked || *reenqueued.DispatchedMessageID != "msg-previous-binary" {
		t.Fatalf("after the previous binary's re-enqueue: request %v, dispatched %v; want the stale %q left beside the new dispatch",
			reenqueued.ReceiptRequestedMessageID, reenqueued.DispatchedMessageID, asked)
	}
	// Not asked: the check's compare-and-set refuses that dispatch.
	moved, err := q.MarkTurnPromptReconnectAnswered(ctx, sqlcgen.MarkTurnPromptReconnectAnsweredParams{
		ReadySeq: 2, Resend: 1, ID: turn, MessageID: "msg-previous-binary", CheckedReadySeq: 1, ResendCount: 0,
	})
	if err != nil || moved != 0 {
		t.Fatalf("claim for the previous binary's dispatch moved %d rows (%v), want 0: it asked for nothing", moved, err)
	}
	var pendingNull bool
	if err := db.QueryRowContext(ctx, `SELECT bool_and(receipt_requested_message_id IS NULL AND receipt_requested_at IS NULL AND receipt_checked_ready_seq IS NULL AND receipt_resend_count = 0)
		FROM turns WHERE session_id = $1 AND id <> $2`, sessionID, turnID).Scan(&pendingNull); err != nil || !pendingNull {
		t.Fatalf("the previous binary's new turn carries a request (%v), want none", err)
	}
	respawned, err := q.GetSandbox(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if respawned.Gen != 2 || respawned.ReadySeq != 1 || respawned.PromptReceiptGen == nil || *respawned.PromptReceiptGen != 1 {
		t.Fatalf("after the previous binary's respawn: gen %d, ready_seq %d, prompt_receipt_gen %v; want 2, 1, 1 (stale, not matching)",
			respawned.Gen, respawned.ReadySeq, respawned.PromptReceiptGen)
	}

	// It cannot boot on 154: golang-migrate refuses a version it has no
	// file for.
	previous, pdb := previousBinaryMigrate(t, connStr, 153)
	if err := previous.Up(); err == nil || !strings.Contains(err.Error(), "154") {
		t.Fatalf("the previous binary's boot on 154 = %v, want a refusal naming 154", err)
	}
	_ = pdb.Close()

	// Rolling back with the columns kept: force 153, and the previous
	// binary boots and works. Deploying this release again runs 000154
	// again, which keeps the columns and their values.
	if err := m.Force(153); err != nil {
		t.Fatalf("force 153: %v", err)
	}
	previous, pdb = previousBinaryMigrate(t, connStr, 153)
	if err := previous.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("the previous binary's boot after force 153 = %v, want no change", err)
	}
	_ = pdb.Close()
	previousRow(ctx, t, db, turnColumns, previousGetTurn, turnID)
	// Pinned to 154, like every migration test here: Up would also apply
	// whatever later migrations exist by the time this runs.
	again, adb := newMigrate(t, connStr)
	if err := again.Migrate(154); err != nil {
		t.Fatalf("this release's migration after the rollback = %v, want 000154 applied again", err)
	}
	_ = adb.Close()
	assertCleanVersion(t, connStr, 154)
	if kept, err := q.GetTurn(ctx, turn); err != nil || kept.ReceiptRequestedMessageID == nil || *kept.ReceiptRequestedMessageID != asked {
		t.Fatalf("after 000154 ran again the request is %v (%v), want %q, kept", kept.ReceiptRequestedMessageID, err, asked)
	}

	// Down: the columns go, every row stays. Run twice -- the second time
	// on a database a rollback already brought back to 153 with the
	// columns dropped, forced to 154 again -- which the IF EXISTS guard
	// allows. Up again works on that state.
	countRows := func() (turns, sandboxes int) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM turns), (SELECT count(*) FROM sandboxes)`).Scan(&turns, &sandboxes); err != nil {
			t.Fatal(err)
		}
		return turns, sandboxes
	}
	turnsBefore, sandboxesBefore := countRows()
	pool.Close()
	if err := m.Migrate(153); err != nil {
		t.Fatalf("down to 153: %v", err)
	}
	if got := columns(); len(got) != 0 {
		t.Fatalf("columns after the down: %v, want none", got)
	}
	if turnsAfter, sandboxesAfter := countRows(); turnsAfter != turnsBefore || sandboxesAfter != sandboxesBefore {
		t.Fatalf("rows after the down: %d turns, %d sandboxes; want %d, %d", turnsAfter, sandboxesAfter, turnsBefore, sandboxesBefore)
	}
	if err := m.Force(154); err != nil {
		t.Fatalf("force 154: %v", err)
	}
	if err := m.Migrate(153); err != nil {
		t.Fatalf("down to 153 again, the columns already gone: %v", err)
	}
	if err := m.Migrate(154); err != nil {
		t.Fatalf("up to 154 again: %v", err)
	}
	assertCleanVersion(t, connStr, 154)
	if got := columns(); len(got) != len(want) {
		t.Fatalf("columns after up again = %v, want %v", got, want)
	}
}
