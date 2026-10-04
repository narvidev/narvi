//go:build integration

// Regression test for audit finding F3 ("two-phase resume's 'no-op guard
// for free' is defeated by stale timestamps"): proves UpsertSandboxForSpawn
// now resets last_seen_at on EVERY call -- including a resume-style claim
// on a box that sat in a terminal status well past
// domain/sandbox.SpawnConfig.SpawningTimeout -- and that
// domain/sandbox.EvaluateSpawnDecision's own Skip guard genuinely covers
// the in-flight window for a concurrent second read as a result. Kept in
// its own file, mirroring event_artifact_wstoken_integration_test.go's own
// precedent of a focused file per fix rather than growing
// postgres_integration_test.go's single pipeline test.
package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/platform"
)

// TestUpsertSandboxForSpawn_ResumeStyleClaim_ResetsLastSeenAt proves the
// query-level half of the F3 fix: a box that has sat Stopped (a terminal
// status) for well longer than SpawningTimeout gets a FRESH last_seen_at
// the moment a resume-style claim (dispatch.go's planResume, which reuses
// this SAME UpsertSandboxForSpawn upsert -- see that query's own generated
// doc comment) runs UpsertSandboxForSpawn against it again, and that this
// fresh timestamp is exactly what lets EvaluateSpawnDecision's own
// Spawning/Connecting/Booting Skip guard genuinely no-op a concurrent
// second actor reading the row right after -- covering the in-flight
// window regardless of how long the box sat terminal beforehand.
func TestUpsertSandboxForSpawn_ResumeStyleClaim_ResetsLastSeenAt(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	store := narvipg.NewSandboxStore(pool)
	tokenHash1 := "token-hash-initial-spawn"

	// Initial spawn: creates the row (gen=1, spawning).
	if _, err := store.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{
		SessionID: sessionID, TokenHash: &tokenHash1,
	}); err != nil {
		t.Fatalf("initial UpsertForSpawn: %v", err)
	}

	// Simulate the box running, then genuinely terminalizing and sitting
	// Stopped for well longer than SpawningTimeout (120s default) before
	// anything resumes it -- directly back-dating created_at/last_seen_at
	// via raw SQL, since driving a real 10-minute wall-clock wait in a
	// test is impractical. This is exactly the "box being resumed has by
	// definition sat terminal -- usually > 120s" scenario the audit
	// finding names.
	const longAgo = 10 * time.Minute
	if _, err := pool.Exec(ctx,
		`UPDATE sandboxes SET status = 'stopped',
		    created_at = now() - make_interval(secs => $2),
		    last_seen_at = now() - make_interval(secs => $2)
		 WHERE session_id = $1`,
		sessionID, longAgo.Seconds(),
	); err != nil {
		t.Fatalf("back-date sandbox row: %v", err)
	}

	before, err := store.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox before resume claim: %v", err)
	}
	if time.Since(before.LastSeenAt.Time) < longAgo-time.Second {
		t.Fatalf("sandbox last_seen_at not back-dated as expected: %v", before.LastSeenAt.Time)
	}

	// The resume-style claim: planResume/planFreshSpawn/planRestore all
	// reuse this EXACT SAME upsert (dispatch.go's own doc comments).
	tokenHash2 := "token-hash-resume-claim"
	after, err := store.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{
		SessionID: sessionID, TokenHash: &tokenHash2,
	})
	if err != nil {
		t.Fatalf("resume-claim UpsertForSpawn: %v", err)
	}

	if after.Gen != 2 {
		t.Errorf("gen = %d, want 2 (bumped from the back-dated row's own gen 1)", after.Gen)
	}
	if after.Status != sqlcgen.SandboxStatusSpawning {
		t.Errorf("status = %s, want %s", after.Status, sqlcgen.SandboxStatusSpawning)
	}

	// The core F3 assertion: last_seen_at genuinely ADVANCED past the
	// 10-minutes-stale value it carried beforehand -- both timestamps come
	// from the SAME Postgres instance's own now(), so comparing them
	// directly against each other (rather than against this test process's
	// own wall clock, which is not guaranteed to be tightly synced with a
	// containerized Postgres's clock) is a clock-skew-proof way to prove
	// the resume-style claim is itself a fresh sign of life.
	if !after.LastSeenAt.Time.After(before.LastSeenAt.Time) {
		t.Errorf("last_seen_at = %v, want strictly after the pre-claim value %v (the resume-style claim itself must be a fresh sign of life)",
			after.LastSeenAt.Time, before.LastSeenAt.Time)
	}
	// And it's genuinely fresh, not just "later than 10 minutes ago" --
	// comfortably below the 10-minute back-dating window, with a wide
	// margin for any test-runner/Postgres-container clock skew.
	if time.Since(after.LastSeenAt.Time) > longAgo/2 {
		t.Errorf("last_seen_at = %v is not fresh (more than %v old)", after.LastSeenAt.Time, longAgo/2)
	}

	// created_at is NOT reset by this upsert (only ever set on the
	// original INSERT) -- still back-dated, exactly as the audit finding's
	// own "created_at/last_seen_at never reset" description of the bug
	// implies createdAt alone was never going to be enough. Compared
	// against before.CreatedAt (same Postgres instance) rather than this
	// test process's own clock, for the same clock-skew-proofing reason.
	if !after.CreatedAt.Time.Equal(before.CreatedAt.Time) {
		t.Errorf("created_at = %v, want unchanged from the back-dated value %v (never reset by UpsertSandboxForSpawn)",
			after.CreatedAt.Time, before.CreatedAt.Time)
	}

	// Now prove the DOMAIN-level payoff: a concurrent second actor's own
	// EvaluateSpawnDecision, reading this row immediately after the resume
	// claim, genuinely Skips instead of fresh-spawning a duplicate
	// provider sandbox -- because sinceLastSignOfLife is now measured from
	// the fresh last_seen_at just persisted, not from the stale
	// created_at.
	cfg := sandbox.SpawnConfig{
		Cooldown:        30 * time.Second,
		ReadyWait:       60 * time.Second,
		SpawningTimeout: platform.DefaultTimeouts().SpawnStuckTimeout,
	}
	concurrentRead := sandbox.SpawnState{
		Status:     sandbox.State(after.Status),
		CreatedAt:  after.CreatedAt.Time,
		LastSeenAt: after.LastSeenAt.Time,
	}
	action := sandbox.EvaluateSpawnDecision(concurrentRead, cfg, time.Now(), false, false)
	if action.Kind != sandbox.SpawnActionSkip {
		t.Errorf("EvaluateSpawnDecision(post-resume-claim row) = %s, want %s (a concurrent second actor must no-op via the SpawningTimeout guard, not fresh-spawn a duplicate sandbox)",
			action.Kind, sandbox.SpawnActionSkip)
	}

	// Sanity check reproducing the PRE-FIX bug directly: the same
	// post-claim row (Status now 'spawning', per the resume claim that
	// just landed) but with CreatedAt/LastSeenAt left at their ORIGINAL
	// 10-minutes-stale values -- i.e. exactly what UpsertSandboxForSpawn
	// used to leave behind before this fix (it bumped status/gen but never
	// touched last_seen_at/created_at). Against that unpatched shape, the
	// guard genuinely does NOT skip -- proving this test's back-dating
	// setup reproduces the bug's own precondition, not a vacuously-true
	// assertion, and that resetting last_seen_at is what actually closes
	// the gap.
	preFixShapeRead := sandbox.SpawnState{
		Status:     sandbox.State(after.Status),
		CreatedAt:  before.CreatedAt.Time,
		LastSeenAt: before.LastSeenAt.Time,
	}
	preFixAction := sandbox.EvaluateSpawnDecision(preFixShapeRead, cfg, time.Now(), false, false)
	if preFixAction.Kind == sandbox.SpawnActionSkip {
		t.Errorf("EvaluateSpawnDecision(pre-fix-shaped row: spawning status, 10min-stale timestamps) = %s, want != %s (this must reproduce the bug: a concurrent second actor would have fresh-spawned a duplicate sandbox)",
			preFixAction.Kind, sandbox.SpawnActionSkip)
	}
}

// lifetimeColumns is the part of a sandbox row technical plan §35.2's
// deadline is about, with the timestamps it is measured against.
type lifetimeColumns struct {
	gen                  int32
	createdAt, lastSeen  time.Time
	deadline             *time.Time
	seconds, deadlineGen *int32
}

func readLifetimeColumns(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) lifetimeColumns {
	t.Helper()
	var row lifetimeColumns
	var lastSeen pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT gen, created_at, last_seen_at, lifetime_deadline_at, lifetime_seconds, lifetime_deadline_gen
		FROM sandboxes WHERE session_id = $1`, sessionID).
		Scan(&row.gen, &row.createdAt, &lastSeen, &row.deadline, &row.seconds, &row.deadlineGen); err != nil {
		t.Fatalf("read the sandbox's lifetime columns: %v", err)
	}
	row.lastSeen = lastSeen.Time
	return row
}

// TestUpsertSandboxForSpawn_StampsTheLifetimeDeadlineInTheSameStatement
// pins where technical plan §35.2's estimate comes from: the statement
// that creates the gen, at its own now() plus the lifetime it is given --
// on the INSERT branch the instant that also stamps created_at, and on the
// ON CONFLICT branch (a respawn or restore) the instant that stamps
// last_seen_at, never created_at, which that branch leaves at the first
// gen's value. Each difference is exact, since both sides come from one
// statement's now(). The deadline is recorded against the gen it was
// stamped for, and the returned row carries what was written.
func TestUpsertSandboxForSpawn_StampsTheLifetimeDeadlineInTheSameStatement(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	store := narvipg.NewSandboxStore(pool)

	const first, second int32 = 7200, 6000
	tokenHash := "token-hash-lifetime-1"
	inserted, err := store.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{
		SessionID: sessionID, TokenHash: &tokenHash, LifetimeSeconds: ptrInt32(first),
	})
	if err != nil {
		t.Fatalf("insert UpsertForSpawn: %v", err)
	}
	row := readLifetimeColumns(ctx, t, pool, sessionID)
	if row.gen != 1 || row.deadline == nil || !equalInt32Ptr(row.seconds, ptrInt32(first)) || !equalInt32Ptr(row.deadlineGen, ptrInt32(1)) {
		t.Fatalf("after the insert: gen %d, deadline %v, lifetime_seconds %v at gen %v; want gen 1, a deadline, %d at gen 1",
			row.gen, row.deadline, derefInt32(row.seconds), derefInt32(row.deadlineGen), first)
	}
	if got := row.deadline.Sub(row.createdAt); got != time.Duration(first)*time.Second {
		t.Errorf("insert: lifetime_deadline_at - created_at = %v, want exactly %v", got, time.Duration(first)*time.Second)
	}
	if !inserted.LifetimeDeadlineAt.Valid || !inserted.LifetimeDeadlineAt.Time.Equal(*row.deadline) ||
		!equalInt32Ptr(inserted.LifetimeSeconds, ptrInt32(first)) || !equalInt32Ptr(inserted.LifetimeDeadlineGen, ptrInt32(1)) {
		t.Errorf("the insert returned deadline %v, lifetime_seconds %v at gen %v; want what it wrote",
			inserted.LifetimeDeadlineAt, derefInt32(inserted.LifetimeSeconds), derefInt32(inserted.LifetimeDeadlineGen))
	}

	// The gen ran, stopped and sat for a while: its row is back-dated, the
	// deadline with it, so a stamp counted from created_at would be past.
	const longAgo = 90 * time.Minute
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'stopped',
		    created_at = created_at - make_interval(secs => $2),
		    last_seen_at = created_at - make_interval(secs => $2),
		    lifetime_deadline_at = lifetime_deadline_at - make_interval(secs => $2)
		 WHERE session_id = $1`, sessionID, longAgo.Seconds()); err != nil {
		t.Fatalf("back-date the sandbox row: %v", err)
	}
	before := readLifetimeColumns(ctx, t, pool, sessionID)

	tokenHash2 := "token-hash-lifetime-2"
	respawned, err := store.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{
		SessionID: sessionID, TokenHash: &tokenHash2, LifetimeSeconds: ptrInt32(second),
	})
	if err != nil {
		t.Fatalf("respawn UpsertForSpawn: %v", err)
	}
	after := readLifetimeColumns(ctx, t, pool, sessionID)
	if after.gen != 2 || after.deadline == nil || !equalInt32Ptr(after.seconds, ptrInt32(second)) || !equalInt32Ptr(after.deadlineGen, ptrInt32(2)) {
		t.Fatalf("after the respawn: gen %d, deadline %v, lifetime_seconds %v at gen %v; want gen 2, a deadline, %d at gen 2",
			after.gen, after.deadline, derefInt32(after.seconds), derefInt32(after.deadlineGen), second)
	}
	if got := after.deadline.Sub(after.lastSeen); got != time.Duration(second)*time.Second {
		t.Errorf("respawn: lifetime_deadline_at - last_seen_at = %v, want exactly %v (the claim's own now())", got, time.Duration(second)*time.Second)
	}
	if !after.createdAt.Equal(before.createdAt) {
		t.Errorf("respawn: created_at = %v, want unchanged %v", after.createdAt, before.createdAt)
	}
	if !after.deadline.After(*before.deadline) || !after.deadline.After(after.createdAt.Add(time.Duration(second)*time.Second)) {
		t.Errorf("respawn: deadline %v, want later than the previous gen's %v and than created_at plus the lifetime %v",
			after.deadline, before.deadline, after.createdAt.Add(time.Duration(second)*time.Second))
	}
	if !respawned.LifetimeDeadlineAt.Time.Equal(*after.deadline) || !equalInt32Ptr(respawned.LifetimeDeadlineGen, ptrInt32(2)) {
		t.Errorf("the respawn returned deadline %v at gen %v; want what it wrote", respawned.LifetimeDeadlineAt, derefInt32(respawned.LifetimeDeadlineGen))
	}
}

// TestUpsertSandboxForSpawn_NilLifetimeCarriesTheLiveGensDeadline pins a
// resume's half of technical plan §35.2 (planResume passes no lifetime):
// the same provider object is never younger than it was, so the new gen
// takes the live gen's deadline unchanged -- never a fresh one -- and a
// deadline that was not the live gen's own (a gen the previous binary
// created, whose upsert bumped gen and left the columns) is not carried,
// so the new gen reads "deadline unknown". A lifetime after a carry stamps
// fresh again.
func TestUpsertSandboxForSpawn_NilLifetimeCarriesTheLiveGensDeadline(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSandboxStore(pool)

	type step struct {
		lifetime        *int32 // nil: a resume
		previousBinary  bool   // the previous binary's upsert: gen bumped, columns left
		wantGen         int32
		wantDeadline    string // "none", "first" (the first stamp, carried), "fresh"
		wantSeconds     *int32
		wantDeadlineGen *int32
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{"a resume carries the live gen's deadline", []step{
			{lifetime: ptrInt32(3600), wantGen: 1, wantDeadline: "first", wantSeconds: ptrInt32(3600), wantDeadlineGen: ptrInt32(1)},
			{wantGen: 2, wantDeadline: "first", wantSeconds: ptrInt32(3600), wantDeadlineGen: ptrInt32(2)},
		}},
		{"two resumes deep", []step{
			{lifetime: ptrInt32(3600), wantGen: 1, wantDeadline: "first", wantSeconds: ptrInt32(3600), wantDeadlineGen: ptrInt32(1)},
			{wantGen: 2, wantDeadline: "first", wantSeconds: ptrInt32(3600), wantDeadlineGen: ptrInt32(2)},
			{wantGen: 3, wantDeadline: "first", wantSeconds: ptrInt32(3600), wantDeadlineGen: ptrInt32(3)},
		}},
		{"a gen the previous binary created has no deadline to carry", []step{
			{lifetime: ptrInt32(3600), wantGen: 1, wantDeadline: "first", wantSeconds: ptrInt32(3600), wantDeadlineGen: ptrInt32(1)},
			{previousBinary: true, wantGen: 2, wantDeadline: "first", wantSeconds: ptrInt32(3600), wantDeadlineGen: ptrInt32(1)},
			{wantGen: 3, wantDeadline: "none"},
		}},
		{"a resume of a row with no deadline", []step{
			{wantGen: 1, wantDeadline: "none"},
			{wantGen: 2, wantDeadline: "none"},
		}},
		{"a restore after a resume stamps fresh", []step{
			{lifetime: ptrInt32(3600), wantGen: 1, wantDeadline: "first", wantSeconds: ptrInt32(3600), wantDeadlineGen: ptrInt32(1)},
			{wantGen: 2, wantDeadline: "first", wantSeconds: ptrInt32(3600), wantDeadlineGen: ptrInt32(2)},
			{lifetime: ptrInt32(5400), wantGen: 3, wantDeadline: "fresh", wantSeconds: ptrInt32(5400), wantDeadlineGen: ptrInt32(3)},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			var first *time.Time
			for i, s := range tc.steps {
				if s.previousBinary {
					if _, err := pool.Exec(ctx, `UPDATE sandboxes SET gen = gen + 1, status = 'spawning', last_seen_at = now(), updated_at = now() WHERE session_id = $1`, sessionID); err != nil {
						t.Fatalf("step %d: the previous binary's respawn: %v", i, err)
					}
				} else {
					// Every step but the first lands on a row a claim
					// left at least a moment ago, so a fresh stamp is
					// told apart from a carried one.
					if _, err := pool.Exec(ctx, `UPDATE sandboxes SET lifetime_deadline_at = lifetime_deadline_at - interval '1 minute' WHERE session_id = $1`, sessionID); err != nil {
						t.Fatalf("step %d: age the deadline: %v", i, err)
					}
					if first != nil {
						aged := first.Add(-time.Minute)
						first = &aged
					}
					tokenHash := "token-hash-carry"
					if _, err := store.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{
						SessionID: sessionID, TokenHash: &tokenHash, LifetimeSeconds: s.lifetime,
					}); err != nil {
						t.Fatalf("step %d: UpsertForSpawn: %v", i, err)
					}
				}
				row := readLifetimeColumns(ctx, t, pool, sessionID)
				if row.gen != s.wantGen || !equalInt32Ptr(row.seconds, s.wantSeconds) || !equalInt32Ptr(row.deadlineGen, s.wantDeadlineGen) {
					t.Fatalf("step %d: gen %d, lifetime_seconds %v at gen %v; want gen %d, %v at gen %v",
						i, row.gen, derefInt32(row.seconds), derefInt32(row.deadlineGen), s.wantGen, derefInt32(s.wantSeconds), derefInt32(s.wantDeadlineGen))
				}
				switch s.wantDeadline {
				case "none":
					if row.deadline != nil {
						t.Fatalf("step %d: lifetime_deadline_at = %v, want NULL", i, *row.deadline)
					}
				case "first":
					if first == nil && row.deadline != nil {
						first = row.deadline
					}
					if row.deadline == nil || !row.deadline.Equal(*first) {
						t.Fatalf("step %d: lifetime_deadline_at = %v, want the first gen's %v, carried", i, row.deadline, first)
					}
				case "fresh":
					if row.deadline == nil || first == nil || !row.deadline.After(*first) {
						t.Fatalf("step %d: lifetime_deadline_at = %v, want a fresh stamp later than %v", i, row.deadline, first)
					}
					if got := row.deadline.Sub(row.lastSeen); got != time.Duration(*s.wantSeconds)*time.Second {
						t.Fatalf("step %d: lifetime_deadline_at - last_seen_at = %v, want exactly %ds", i, got, *s.wantSeconds)
					}
				}
			}
		})
	}
}
