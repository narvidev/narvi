//go:build integration

// Integration tests for the two writes technical plan §43.20's review
// round 3 added for a session's status: a turn's status_changed_at
// (migrations/000146), stamped by UpdateTurnStatus on the database's
// clock, and release_manifest_pending's claimed_at (migrations/000147),
// which keeps a release manifest check readable while it runs.
package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestUpdateTurnStatus_StampsStatusChangedAt: a real status change stamps
// status_changed_at with the database's now() -- the clock a workflow
// run's escalation is stamped on -- and each later change moves it; a
// call that passes the current status back (tryPlanReenqueue re-stamping
// a processing turn's sandbox generation) leaves it alone. A turn that has
// never changed status has none.
func TestUpdateTurnStatus_StampsStatusChangedAt(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	turns := narvipg.NewTurnStore(pool)
	sessionID := createTestSession(ctx, t, pool)

	row, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	if row.StatusChangedAt.Valid {
		t.Fatalf("a new turn has status_changed_at %v, want none", row.StatusChangedAt)
	}

	dbNow := func() time.Time {
		t.Helper()
		var now time.Time
		if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			t.Fatal(err)
		}
		return now
	}
	change := func(status sqlcgen.TurnStatus, gen *int32) sqlcgen.Turn {
		t.Helper()
		got, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: row.ID, Status: status, DispatchedSandboxGen: gen})
		if err != nil {
			t.Fatalf("update status to %s: %v", status, err)
		}
		return got
	}

	before := dbNow()
	dispatched := change(sqlcgen.TurnStatusDispatched, nil)
	if !dispatched.StatusChangedAt.Valid || dispatched.StatusChangedAt.Time.Before(before) {
		t.Fatalf("dispatched: status_changed_at %v, want the database's now(), not before %v", dispatched.StatusChangedAt, before)
	}
	time.Sleep(5 * time.Millisecond)
	processing := change(sqlcgen.TurnStatusProcessing, nil)
	if !processing.StatusChangedAt.Time.After(dispatched.StatusChangedAt.Time) {
		t.Fatalf("processing: status_changed_at %v, want after the dispatch's %v", processing.StatusChangedAt.Time, dispatched.StatusChangedAt.Time)
	}
	time.Sleep(5 * time.Millisecond)
	gen := int32(2)
	restamped := change(sqlcgen.TurnStatusProcessing, &gen)
	if !restamped.StatusChangedAt.Time.Equal(processing.StatusChangedAt.Time) || restamped.DispatchedSandboxGen == nil || *restamped.DispatchedSandboxGen != 2 {
		t.Fatalf("same status passed back: status_changed_at %v gen %v, want %v unchanged and gen 2", restamped.StatusChangedAt.Time, restamped.DispatchedSandboxGen, processing.StatusChangedAt.Time)
	}
	time.Sleep(5 * time.Millisecond)
	completed := change(sqlcgen.TurnStatusCompleted, nil)
	if !completed.StatusChangedAt.Time.After(processing.StatusChangedAt.Time) {
		t.Fatalf("completed: status_changed_at %v, want after %v", completed.StatusChangedAt.Time, processing.StatusChangedAt.Time)
	}
}

// TestReleaseManifestPending_ClaimFinishPurge pins the claim the session's
// status relies on: ClaimDue stamps claimed_at (the database's now()) and
// keeps the row -- readable by ActivityFacts while the check runs -- and a
// claimed row is never claimed again, so the check still runs at most
// once; Finish deletes it; PurgeStaleClaimed deletes only rows claimed
// longer ago than its bound, never an unclaimed one.
func TestReleaseManifestPending_ClaimFinishPurge(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewReleaseManifestPendingStore(pool)
	sessions := narvipg.NewSessionStore(pool)
	sessionID := createTestSession(ctx, t, pool)

	// Earlier tests' rows would be claimed first (oldest first); start
	// this one from an empty queue.
	if _, err := pool.Exec(ctx, `DELETE FROM release_manifest_pending`); err != nil {
		t.Fatal(err)
	}
	enqueue := func(pr int32) sqlcgen.ReleaseManifestPending {
		t.Helper()
		row, err := store.Create(ctx, sqlcgen.CreateReleaseManifestPendingParams{SessionID: sessionID, Owner: "acme", Repo: "widgets", PrNumber: pr, BaseRef: "main", HeadRef: "release/x"})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		return row
	}
	first := enqueue(1)
	second := enqueue(2)

	claimed, err := store.ClaimDue(ctx, 1)
	if err != nil || len(claimed) != 1 || claimed[0].ID != first.ID || !claimed[0].ClaimedAt.Valid {
		t.Fatalf("ClaimDue(1) = %+v, %v; want the oldest row, claimed", claimed, err)
	}
	facts, err := sessions.ActivityFacts(ctx, sessionID)
	if err != nil {
		t.Fatalf("ActivityFacts: %v", err)
	}
	if !facts.ReleaseCheckClaimedAt.Time.Equal(claimed[0].ClaimedAt.Time) || !facts.ReleaseCheckPendingSince.Time.Equal(second.CreatedAt.Time) {
		t.Fatalf("facts while the first check runs: claimed %v pending %v, want the claim %v and the second row %v", facts.ReleaseCheckClaimedAt, facts.ReleaseCheckPendingSince, claimed[0].ClaimedAt, second.CreatedAt)
	}

	again, err := store.ClaimDue(ctx, 5)
	if err != nil || len(again) != 1 || again[0].ID != second.ID {
		t.Fatalf("ClaimDue(5) = %+v, %v; want only the second row -- a claimed row is never claimed again", again, err)
	}
	if none, err := store.ClaimDue(ctx, 5); err != nil || len(none) != 0 {
		t.Fatalf("ClaimDue on an all-claimed queue = %+v, %v; want nothing", none, err)
	}

	if err := store.Finish(ctx, first.ID); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM release_manifest_pending WHERE id = $1`, first.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("after Finish: %d rows (%v), want the first row gone", left, err)
	}

	// The second row's worker "died": claimed long ago. A third row waits
	// unclaimed, enqueued even longer ago.
	if _, err := pool.Exec(ctx, `UPDATE release_manifest_pending SET claimed_at = now() - interval '1 hour' WHERE id = $1`, second.ID); err != nil {
		t.Fatal(err)
	}
	third := enqueue(3)
	if _, err := pool.Exec(ctx, `UPDATE release_manifest_pending SET created_at = now() - interval '2 hours' WHERE id = $1`, third.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := store.PurgeStaleClaimed(ctx, 2*time.Hour); err != nil || n != 0 {
		t.Fatalf("PurgeStaleClaimed(2h) = %d, %v; want nothing: the claim is only an hour old", n, err)
	}
	if n, err := store.PurgeStaleClaimed(ctx, 30*time.Minute); err != nil || n != 1 {
		t.Fatalf("PurgeStaleClaimed(30m) = %d, %v; want the dead claim, and never the unclaimed row", n, err)
	}
	var ids []pgtype.UUID
	rows, err := pool.Query(ctx, `SELECT id FROM release_manifest_pending`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 1 || ids[0] != third.ID {
		t.Fatalf("rows left = %v, want only the unclaimed third", ids)
	}
}
