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

// This file runs migration 000157 (agent_max_frame_bytes) through
// golang-migrate against real Postgres, in a database of its own migrated
// to 156 first. The up adds what technical plan §3.3's per-gen prompt bound
// reads: the read limit a gen's latest ready stated
// (sandboxes.agent_max_frame_bytes) and the gen it was stated for
// (agent_max_frame_bytes_gen); the down removes them.
//
// It also pins what the migration's own "Rolling deploy" and "Rolling back"
// sections say of the previous binary: its sandbox statements -- copied
// below verbatim from the sqlc output it was built with -- run with the
// columns present and leave them alone, so a ready it records keeps the
// value this release recorded for the same gen, and its respawn leaves a
// value that no longer matches the gen; it cannot boot on 157; and it can
// once the recorded version is forced back to 156 with the columns kept,
// after which this release's migration runs again and keeps their values.
//
// Every read and write of this release here names its columns too, in
// frameBoundRecordSandboxReady and maxFrameColumns157, so a later
// migration's columns never break it, as this release's whole-row
// statements would on a database pinned at 157.

// The previous binary's sandbox statements, as its sqlc output sent them:
// the column list of every SELECT * and RETURNING * on sandboxes (000155's
// two columns after previousSandboxColumns), and RecordSandboxReady as
// every release from 000155's to 000156's sent it.
const (
	previous157SandboxColumns       = previousSandboxColumns + ", prompt_receipt_gen, ready_seq"
	previous157CreateSandbox        = `INSERT INTO sandboxes (session_id) VALUES ($1) RETURNING ` + previous157SandboxColumns
	previous157GetSandbox           = `SELECT ` + previous157SandboxColumns + ` FROM sandboxes WHERE session_id = $1`
	preFrameBoundRecordSandboxReady = `UPDATE sandboxes
SET ready_seq = ready_seq + 1,
    prompt_receipt_gen = CASE WHEN $1::boolean THEN gen ELSE NULL END,
    updated_at = now()
WHERE session_id = $2 AND gen = $3::integer`
	previous157UpsertSandboxSpawn = `INSERT INTO sandboxes (session_id, gen, status, token_hash)
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
RETURNING ` + previous157SandboxColumns
)

// frameBoundRecordSandboxReady is RecordSandboxReady as 000157's release
// sends it ($1 promptReceipt, $2 maxFrameBytes, $3 session, $4 gen).
const frameBoundRecordSandboxReady = `UPDATE sandboxes
SET ready_seq = ready_seq + 1,
    prompt_receipt_gen = CASE WHEN $1::boolean THEN gen ELSE NULL END,
    agent_max_frame_bytes = $2::integer,
    agent_max_frame_bytes_gen = CASE WHEN $2::integer IS NULL THEN NULL ELSE gen END,
    updated_at = now()
WHERE session_id = $3 AND gen = $4::integer`

// maxFrameColumns157 is the part of a sandbox row 000157 is about.
type maxFrameColumns157 struct {
	gen, readySeq           int32
	promptReceiptGen        *int32
	maxFrameBytes, frameGen *int32
}

func readMaxFrameColumns157(ctx context.Context, t *testing.T, db *sql.DB, sessionID string) maxFrameColumns157 {
	t.Helper()
	var row maxFrameColumns157
	if err := db.QueryRowContext(ctx, `SELECT gen, ready_seq, prompt_receipt_gen, agent_max_frame_bytes, agent_max_frame_bytes_gen FROM sandboxes WHERE session_id = $1`, sessionID).
		Scan(&row.gen, &row.readySeq, &row.promptReceiptGen, &row.maxFrameBytes, &row.frameGen); err != nil {
		t.Fatalf("read the sandbox's frame-bound columns: %v", err)
	}
	return row
}

func TestMigrationAgentMaxFrameBytes_UpAndDown(t *testing.T) {
	ctx := context.Background()
	connStr, db := migrationTestDatabase(ctx, t, 156)
	sandboxColumns := len(strings.Split(previous157SandboxColumns, ","))

	columns := func() map[string]string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT table_name || '.' || column_name, is_nullable || ' ' || coalesce(column_default, '') || ' ' || data_type
			FROM information_schema.columns
			WHERE table_name = 'sandboxes' AND column_name IN ('agent_max_frame_bytes', 'agent_max_frame_bytes_gen')`)
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
		t.Fatalf("frame-bound columns at 156: %v, want none", got)
	}
	var sessionID string
	if err := db.QueryRowContext(ctx, `INSERT INTO sessions (spawn_source) VALUES ('web') RETURNING id::text`).Scan(&sessionID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	previousRow(ctx, t, db, sandboxColumns, previous157CreateSandbox, sessionID)
	if _, err := db.ExecContext(ctx, preFrameBoundRecordSandboxReady, true, sessionID, 1); err != nil {
		t.Fatalf("the previous binary records a ready at 156: %v", err)
	}

	m, mdb := newMigrate(t, connStr)
	defer func() { _ = mdb.Close() }()
	if err := m.Migrate(157); err != nil {
		t.Fatalf("up to 157: %v", err)
	}
	want := map[string]string{
		"sandboxes.agent_max_frame_bytes":     "YES  integer",
		"sandboxes.agent_max_frame_bytes_gen": "YES  integer",
	}
	if got := columns(); len(got) != len(want) {
		t.Fatalf("columns at 157 = %v, want %v", got, want)
	} else {
		for name, shape := range want {
			if got[name] != shape {
				t.Fatalf("%s is (nullable, default, type) %q, want %q", name, got[name], shape)
			}
		}
	}

	// An existing sandbox states nothing until its next ready.
	if row := readMaxFrameColumns157(ctx, t, db, sessionID); row.readySeq != 1 || row.maxFrameBytes != nil || row.frameGen != nil {
		t.Fatalf("an existing sandbox reads ready_seq %d, agent_max_frame_bytes %v at gen %v; want 1, NULL at NULL",
			row.readySeq, derefInt32(row.maxFrameBytes), derefInt32(row.frameGen))
	}

	// This release: a ready of gen 1 stating 32 MiB, and one of another gen,
	// which records nothing.
	const stated = 32 << 20
	if _, err := db.ExecContext(ctx, frameBoundRecordSandboxReady, true, stated, sessionID, 1); err != nil {
		t.Fatalf("this release records a ready: %v", err)
	}
	if _, err := db.ExecContext(ctx, frameBoundRecordSandboxReady, true, 1024, sessionID, 7); err != nil {
		t.Fatalf("this release records another gen's ready: %v", err)
	}
	if row := readMaxFrameColumns157(ctx, t, db, sessionID); row.readySeq != 2 || !equalInt32Ptr(row.maxFrameBytes, ptrInt32(stated)) || !equalInt32Ptr(row.frameGen, ptrInt32(1)) {
		t.Fatalf("after this release's readies: ready_seq %d, agent_max_frame_bytes %v at gen %v; want 2, %d at gen 1",
			row.readySeq, derefInt32(row.maxFrameBytes), derefInt32(row.frameGen), stated)
	}

	// The previous binary, still running during a rolling deploy: it reads
	// sandboxes with the columns present; a ready of the same gen it
	// records counts and clears promptReceipt, and leaves the stated limit
	// alone -- the same gen is the same agent binary; and its respawn
	// leaves that limit on a gen that is no longer live.
	previousRow(ctx, t, db, sandboxColumns, previous157GetSandbox, sessionID)
	if _, err := db.ExecContext(ctx, preFrameBoundRecordSandboxReady, false, sessionID, 1); err != nil {
		t.Fatalf("the previous binary records a ready at 157: %v", err)
	}
	if row := readMaxFrameColumns157(ctx, t, db, sessionID); row.readySeq != 3 || row.promptReceiptGen != nil ||
		!equalInt32Ptr(row.maxFrameBytes, ptrInt32(stated)) || !equalInt32Ptr(row.frameGen, ptrInt32(1)) {
		t.Fatalf("after the previous binary's ready: ready_seq %d, prompt_receipt_gen %v, agent_max_frame_bytes %v at gen %v; want 3, NULL, %d at gen 1, left alone",
			row.readySeq, derefInt32(row.promptReceiptGen), derefInt32(row.maxFrameBytes), derefInt32(row.frameGen), stated)
	}
	previousRow(ctx, t, db, sandboxColumns, previous157UpsertSandboxSpawn, sessionID, "token-hash-2")
	if row := readMaxFrameColumns157(ctx, t, db, sessionID); row.gen != 2 ||
		!equalInt32Ptr(row.maxFrameBytes, ptrInt32(stated)) || !equalInt32Ptr(row.frameGen, ptrInt32(1)) {
		t.Fatalf("after the previous binary's respawn: gen %d, agent_max_frame_bytes %v at gen %v; want gen 2, %d at gen 1 (stale, not matching)",
			row.gen, derefInt32(row.maxFrameBytes), derefInt32(row.frameGen), stated)
	}

	// It cannot boot on 157: golang-migrate refuses a version it has no
	// file for.
	previous, pdb := previousBinaryMigrate(t, connStr, 156)
	if err := previous.Up(); err == nil || !strings.Contains(err.Error(), "157") {
		t.Fatalf("the previous binary's boot on 157 = %v, want a refusal naming 157", err)
	}
	_ = pdb.Close()

	// Rolling back with the columns kept: force 156, and the previous
	// binary boots and works. Deploying this release again runs 000157
	// again, which keeps the columns and their values.
	if err := m.Force(156); err != nil {
		t.Fatalf("force 156: %v", err)
	}
	previous, pdb = previousBinaryMigrate(t, connStr, 156)
	if err := previous.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("the previous binary's boot after force 156 = %v, want no change", err)
	}
	_ = pdb.Close()
	previousRow(ctx, t, db, sandboxColumns, previous157GetSandbox, sessionID)
	// Pinned to 157, like every migration test here: Up would also apply
	// whatever later migrations exist by the time this runs.
	again, adb := newMigrate(t, connStr)
	if err := again.Migrate(157); err != nil {
		t.Fatalf("this release's migration after the rollback = %v, want 000157 applied again", err)
	}
	_ = adb.Close()
	assertCleanVersion(t, connStr, 157)
	if row := readMaxFrameColumns157(ctx, t, db, sessionID); !equalInt32Ptr(row.maxFrameBytes, ptrInt32(stated)) || !equalInt32Ptr(row.frameGen, ptrInt32(1)) {
		t.Fatalf("after 000157 ran again: agent_max_frame_bytes %v at gen %v; want %d at gen 1, kept",
			derefInt32(row.maxFrameBytes), derefInt32(row.frameGen), stated)
	}

	// Down: the columns go, every row stays. Run twice -- the second time
	// on a database a rollback already brought back to 156 with the
	// columns dropped, forced to 157 again -- which the IF EXISTS guard
	// allows. Up again works on that state.
	countSandboxes := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sandboxes`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := countSandboxes()
	if err := m.Migrate(156); err != nil {
		t.Fatalf("down to 156: %v", err)
	}
	if got := columns(); len(got) != 0 {
		t.Fatalf("columns after the down: %v, want none", got)
	}
	if after := countSandboxes(); after != before {
		t.Fatalf("sandboxes after the down: %d, want %d", after, before)
	}
	previousRow(ctx, t, db, sandboxColumns, previous157GetSandbox, sessionID)
	if err := m.Force(157); err != nil {
		t.Fatalf("force 157: %v", err)
	}
	if err := m.Migrate(156); err != nil {
		t.Fatalf("down to 156 again, the columns already gone: %v", err)
	}
	if err := m.Migrate(157); err != nil {
		t.Fatalf("up to 157 again: %v", err)
	}
	assertCleanVersion(t, connStr, 157)
	if got := columns(); len(got) != len(want) {
		t.Fatalf("columns after up again = %v, want %v", got, want)
	}
}
