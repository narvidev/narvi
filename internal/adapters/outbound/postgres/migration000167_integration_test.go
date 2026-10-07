//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// This file runs migrations 000167 (review_turn_checkout) and 000168
// (sandbox_review_checkout_gen) through golang-migrate against real
// Postgres, in a database of its own migrated to 166 first. 000167 adds
// what technical plan §21.1's review checkout reads and writes of a turn:
// nine turn columns (the latest checkout request, its bound, its sends and
// failures, the gen its failures retired and the commit the turn was
// checked out at); 000168, in a transaction of its own, the sandbox's: the
// gen whose latest ready advertised the capability. Their downs remove
// them.
//
// It also pins what the migrations' own "Rolling deploy" and "Rolling back"
// sections say of the previous binary: its whole-row turn and sandbox
// statements -- the column lists below, copied verbatim from the sqlc
// output it was built with -- run with the columns present and leave them
// alone; its RecordSandboxReady (frameBoundRecordSandboxReady, unchanged
// since 000157's release) counts a ready and leaves the capability this
// release recorded for the same gen; it cannot boot on 168; and it can
// once the recorded version is forced back to 166 with the columns kept,
// after which this release's migrations run again and keep their values.
// This release's own statements are read from the sqlc output
// (generatedQuery), so they cannot drift from what the store sends.
//
// TestMigration000167_168_NeverDeadlockWithAnActorsTransaction runs both
// against a transaction shaped like the session actor's -- the session row
// locked, then sandboxes read, then turns -- and shows, as its control, that
// one file altering turns then sandboxes deadlocks with it.

// reviewCheckoutMigration is 000167's version, reviewCheckoutGenMigration
// 000168's: the plan tests that run this release's whole-row statements
// migrate to the latter.
const (
	reviewCheckoutMigration    = 167
	reviewCheckoutGenMigration = reviewCheckoutMigration + 1
)

// The column lists the previous binary's sqlc output wrote out for every
// SELECT * and RETURNING * on turns and on sandboxes.
const (
	previous167TurnColumns    = "id, session_id, status, conversation_id, created_at, dispatched_at, completed_at, prompt, model_id, plan_mode, dispatched_sandbox_gen, progress_notified_at, effort, epistemic_outcome, review_head_sha, answer_only, review_depth, review_depth_decision, dispatched_event_id, cost_usd, review_knowledge_mode, review_knowledge_decision, correlation_id, review_verdict_context, dispatched_message_id, is_review_attempt, stop_requested_at, receipt_requested_message_id, receipt_requested_at, receipt_checked_ready_seq, receipt_resend_count, end_reason, context_unconfirmed_at, request_trigger, requested_by, request_text, context_moves"
	previous167SandboxColumns = "id, session_id, gen, status, last_seen_at, created_at, updated_at, token_hash, provider_id, spawn_failure_count, last_spawn_failure_at, snapshot_id, pending_snapshot_message_id, pre_suspect_status, snapshot_suppressed_in_shadow, pending_push_suppressed_in_shadow, pending_push_cancelled, demotion_terminate_requested_at, agent_version, image_digest, image_decision_reason, image_decision_fingerprint, pr_delivery_started_at, boot_evidence_gen, booting_since, booting_since_gen, stop_retire_gen, prompt_receipt_gen, ready_seq, agent_max_frame_bytes, agent_max_frame_bytes_gen, lifetime_deadline_at, lifetime_seconds, lifetime_deadline_gen"
)

// thisReleasesRecordReady is RecordSandboxReady as this release's store
// sends it ($1 promptReceipt, $2 reviewCheckout, $3 maxFrameBytes, $4
// session, $5 gen), and thisReleasesCheckoutRequest RecordTurnCheckoutRequest
// ($1 gen, $2 afterFailure, $3 messageId, $4 readySeq, $5 turn), both read
// from the sqlc output.
func thisReleasesRecordReady(t *testing.T) string {
	return generatedQuery(t, "sandboxes.sql.go", "recordSandboxReady")
}

func thisReleasesCheckoutRequest(t *testing.T) string {
	return generatedQuery(t, "turns.sql.go", "recordTurnCheckoutRequest")
}

// checkoutColumns167 is the part of a turn and its sandbox 000167 is about,
// read by name.
type checkoutColumns167 struct {
	messageID           *string
	gen, sends          *int32
	failures            *int32
	checkedOut          *string
	reviewCheckoutGen   *int32
	readySeq, sandboxAt int32
}

func readCheckoutColumns167(ctx context.Context, t *testing.T, db *sql.DB, sessionID, turnID string) checkoutColumns167 {
	t.Helper()
	var c checkoutColumns167
	if err := db.QueryRowContext(ctx, `SELECT t.checkout_message_id, t.checkout_gen, t.checkout_sends, t.checkout_failures, t.checked_out_sha,
			s.review_checkout_gen, s.ready_seq, s.gen
		FROM turns t JOIN sandboxes s ON s.session_id = t.session_id WHERE t.id = $1 AND s.session_id = $2`, turnID, sessionID).
		Scan(&c.messageID, &c.gen, &c.sends, &c.failures, &c.checkedOut, &c.reviewCheckoutGen, &c.readySeq, &c.sandboxAt); err != nil {
		t.Fatalf("read the checkout columns: %v", err)
	}
	return c
}

func TestMigration000167_UpAndDown(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, reviewCheckoutMigration-1)
	turnColumns := len(strings.Split(previous167TurnColumns, ","))
	sandboxColumns := len(strings.Split(previous167SandboxColumns, ","))

	columns := func() map[string]string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT table_name || '.' || column_name, is_nullable || ' ' || coalesce(column_default, '') || ' ' || data_type
			FROM information_schema.columns
			WHERE (table_name = 'turns' AND (column_name LIKE 'checkout\_%' OR column_name = 'checked_out_sha'))
			   OR (table_name = 'sandboxes' AND column_name = 'review_checkout_gen')`)
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
		t.Fatalf("checkout columns at 166: %v, want none", got)
	}
	var sessionID, turnID string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('github') RETURNING id::text`).Scan(&sessionID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	previousRow(ctx, t, db, sandboxColumns, `INSERT INTO sandboxes (session_id) VALUES ($1) RETURNING `+previous167SandboxColumns, sessionID)
	previousRow(ctx, t, db, turnColumns, `INSERT INTO turns (session_id, status, review_head_sha) VALUES ($1, 'pending', 'c0ffee0000000000000000000000000000000001') RETURNING `+previous167TurnColumns, sessionID)
	if err := db.QueryRowContext(ctx, `SELECT id::text FROM turns WHERE session_id = $1`, sessionID).Scan(&turnID); err != nil {
		t.Fatal(err)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(reviewCheckoutMigration); err != nil {
		t.Fatalf("up to 000167: %v", err)
	}
	if got := columns(); len(got) != 9 || got["sandboxes.review_checkout_gen"] != "" {
		t.Fatalf("columns at 000167 = %v, want the nine turn columns alone: 000168 adds the sandbox's", got)
	}
	if err := m.Migrate(reviewCheckoutGenMigration); err != nil {
		t.Fatalf("up to 000168: %v", err)
	}
	want := map[string]string{
		"turns.checkout_message_id":     "YES  text",
		"turns.checkout_gen":            "YES  integer",
		"turns.checkout_requested_at":   "YES  timestamp with time zone",
		"turns.checkout_sent_at":        "YES  timestamp with time zone",
		"turns.checkout_sent_ready_seq": "YES  integer",
		"turns.checkout_sends":          "YES  integer",
		"turns.checkout_failures":       "YES  integer",
		"turns.checkout_retired_gen":    "YES  integer",
		"turns.checked_out_sha":         "YES  text",
		"sandboxes.review_checkout_gen": "YES  integer",
	}
	if got := columns(); len(got) != len(want) {
		t.Fatalf("columns at 000168 = %v, want %v", got, want)
	} else {
		for name, shape := range want {
			if got[name] != shape {
				t.Fatalf("%s is (nullable, default, type) %q, want %q", name, got[name], shape)
			}
		}
	}

	// An existing turn has asked for no checkout, and stores nothing for
	// it; an existing sandbox reads as unable to check out until its next
	// ready.
	if c := readCheckoutColumns167(ctx, t, db, sessionID, turnID); c.messageID != nil || c.sends != nil || c.failures != nil || c.checkedOut != nil || c.reviewCheckoutGen != nil {
		t.Fatalf("an existing turn and sandbox read %+v, want no request, nothing checked out, no capability", c)
	}

	// This release: a ready of gen 1 that can check out, and the turn's
	// first request on it.
	if _, err := db.ExecContext(ctx, thisReleasesRecordReady(t), true, true, nil, sessionID, 1); err != nil {
		t.Fatalf("this release records a ready: %v", err)
	}
	if _, err := db.ExecContext(ctx, thisReleasesCheckoutRequest(t), 1, false, "first", 1, turnID); err != nil {
		t.Fatalf("this release records a checkout request: %v", err)
	}
	if c := readCheckoutColumns167(ctx, t, db, sessionID, turnID); c.messageID == nil || *c.messageID != "first" || c.sends == nil || *c.sends != 1 ||
		!equalInt32Ptr(c.reviewCheckoutGen, ptrInt32(1)) || c.readySeq != 1 {
		t.Fatalf("after this release's ready and request: %+v, want the request recorded and gen 1 able to check out", c)
	}

	// The previous binary, still running during a rolling deploy: its
	// whole-row reads and writes run with the columns present; a ready of
	// the same gen it records counts and leaves the capability alone -- the
	// same gen is the same agent; and its respawn leaves it on a gen that is
	// no longer live.
	previousRow(ctx, t, db, turnColumns, `SELECT `+previous167TurnColumns+` FROM turns WHERE id = $1`, turnID)
	previousRow(ctx, t, db, turnColumns, `UPDATE turns SET status = 'pending' WHERE id = $1 RETURNING `+previous167TurnColumns, turnID)
	previousRow(ctx, t, db, sandboxColumns, `SELECT `+previous167SandboxColumns+` FROM sandboxes WHERE session_id = $1`, sessionID)
	if _, err := db.ExecContext(ctx, frameBoundRecordSandboxReady, false, nil, sessionID, 1); err != nil {
		t.Fatalf("the previous binary records a ready at 168: %v", err)
	}
	if c := readCheckoutColumns167(ctx, t, db, sessionID, turnID); c.readySeq != 2 || !equalInt32Ptr(c.reviewCheckoutGen, ptrInt32(1)) ||
		c.messageID == nil || *c.messageID != "first" {
		t.Fatalf("after the previous binary's ready: %+v, want ready_seq 2 and gen 1's capability and the request left alone", c)
	}
	previousRow(ctx, t, db, sandboxColumns, `UPDATE sandboxes SET gen = gen + 1, status = 'spawning' WHERE session_id = $1 RETURNING `+previous167SandboxColumns, sessionID)
	if c := readCheckoutColumns167(ctx, t, db, sessionID, turnID); c.sandboxAt != 2 || !equalInt32Ptr(c.reviewCheckoutGen, ptrInt32(1)) {
		t.Fatalf("after the previous binary's respawn: %+v, want gen 2 and the capability left at gen 1 (stale, not matching)", c)
	}

	// It cannot boot on 168: golang-migrate refuses a version it has no
	// file for.
	previous, pdb := previousBinaryMigrate(t, connStr, reviewCheckoutMigration-1)
	if err := previous.Up(); err == nil || !strings.Contains(err.Error(), strconv.Itoa(reviewCheckoutGenMigration)) {
		t.Fatalf("the previous binary's boot on %d = %v, want a refusal naming it", reviewCheckoutGenMigration, err)
	}
	_ = pdb.Close()

	// Rolling back with the columns kept: force 166, and the previous
	// binary boots and works. Deploying this release again runs 000167
	// and 000168 again, which keep the columns and their values.
	if err := m.Force(reviewCheckoutMigration - 1); err != nil {
		t.Fatalf("force the previous version: %v", err)
	}
	previous, pdb = previousBinaryMigrate(t, connStr, reviewCheckoutMigration-1)
	if err := previous.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("the previous binary's boot after the force = %v, want no change", err)
	}
	_ = pdb.Close()
	previousRow(ctx, t, db, turnColumns, `SELECT `+previous167TurnColumns+` FROM turns WHERE id = $1`, turnID)
	// Pinned to this migration, like every migration test here: Up would also apply
	// whatever later migrations exist by the time this runs.
	again, adb := newMigrate(t, connStr)
	if err := again.Migrate(reviewCheckoutGenMigration); err != nil {
		t.Fatalf("this release's migrations after the rollback = %v, want them applied again", err)
	}
	_ = adb.Close()
	assertCleanVersion(t, connStr, reviewCheckoutGenMigration)
	if c := readCheckoutColumns167(ctx, t, db, sessionID, turnID); c.messageID == nil || *c.messageID != "first" || !equalInt32Ptr(c.reviewCheckoutGen, ptrInt32(1)) {
		t.Fatalf("after 000167 and 000168 ran again: %+v, want the request and the capability kept", c)
	}

	// Down: 000168's, then 000167's; the columns go, every row stays. Run
	// twice -- the second time on a database a rollback already brought
	// back to 166 with the columns dropped, forced to 168 again -- which the
	// IF EXISTS guards allow. Up again works on that state.
	count := func(table string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	turnsBefore, sandboxesBefore := count("turns"), count("sandboxes")
	if err := m.Migrate(reviewCheckoutMigration - 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := columns(); len(got) != 0 {
		t.Fatalf("columns after the down: %v, want none", got)
	}
	if count("turns") != turnsBefore || count("sandboxes") != sandboxesBefore {
		t.Fatalf("rows after the down: %d turns, %d sandboxes; want %d, %d", count("turns"), count("sandboxes"), turnsBefore, sandboxesBefore)
	}
	previousRow(ctx, t, db, turnColumns, `SELECT `+previous167TurnColumns+` FROM turns WHERE id = $1`, turnID)
	previousRow(ctx, t, db, sandboxColumns, `SELECT `+previous167SandboxColumns+` FROM sandboxes WHERE session_id = $1`, sessionID)
	if err := m.Force(reviewCheckoutGenMigration); err != nil {
		t.Fatalf("force this version: %v", err)
	}
	if err := m.Migrate(reviewCheckoutMigration - 1); err != nil {
		t.Fatalf("down again, the columns already gone: %v", err)
	}
	if err := m.Migrate(reviewCheckoutGenMigration); err != nil {
		t.Fatalf("up again: %v", err)
	}
	assertCleanVersion(t, connStr, reviewCheckoutGenMigration)
	if got := columns(); len(got) != len(want) {
		t.Fatalf("columns after up again = %v, want %v", got, want)
	}
}
