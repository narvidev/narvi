//go:build integration

// Resilience scenario #12 (§9.3, docs/IMPLEMENTATION_PLAN.md row 30):
// "Deploy rollout (rolling restart) -> zero sessions marked failed." This
// is the harness's own first proof-of-concept scenario -- see
// test/resilience/harness_test.go's own doc comment for the shared rig
// this test builds on, and test/resilience/README.md for how this fits
// among all 12 §9.3 scenarios.
//
// The whole point of this test is the CONTRAST with resilience scenario
// #1 (internal/app/sessionactor/resilience_killpod_integration_test.go,
// TestResilience_KillPodMidTurn_TurnFailsWithReason_NoStuckProcessing):
// that test forces an ALREADY-OVERDUE turn_deadline timer and a hard
// pg_terminate_backend "kill -9" of the advisory-lock-holding connection,
// then asserts the turn DOES fail. This test does the exact opposite on
// both axes: every timer stays comfortably unexpired throughout, and the
// "pod" that owned the session goes down via the REAL graceful path
// (Registry.Shutdown -- cmd/control-plane/main.go's own real shutdown
// sequence, `r.cancel(); return r.group.Wait()`, registry.go:313) rather
// than having its connection severed out from under it. A graceful
// shutdown must leave a still-legitimately-in-flight turn exactly as it
// was: still Processing, still resumable by whichever process next calls
// GetOrSpawn for that session -- never forced into a terminal Failed
// state merely because the pod serving it is cycling.
//
// Confirmed by reading Actor.shutdown (internal/app/sessionactor/actor.go):
// on a graceful Shutdown, run's own ctx.Done() branch returns
// context.Canceled and the deferred shutdown() only (1) evicts the actor
// from the Registry's map, (2) drains whatever slipped into the mailbox,
// and (3) releases the advisory lock -- it never writes to turns or
// sessions at all. (Registry.Shutdown then closes the lock connection
// itself, which would release any lock left on it.) This test proves that by reading Postgres directly,
// not by re-deriving it from the source a second time.
package resilience_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/sessionactor"
)

func TestResilienceScenario12_GracefulRollingRestart_ZeroSessionsMarkedFailed(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	sessionID := h.CreateSession(ctx, t)

	// §3.1's DeriveStatus: a session with any non-terminal turn derives to
	// Active. Seeded explicitly here (mirroring what a real dispatch-time
	// transactional write already does) so this test's own "still not
	// failed" assertion is meaningful, rather than trivially true of a
	// session that was never anything but its just-created default.
	if _, err := h.Sessions.UpdateStatus(ctx, sqlcgen.UpdateSessionStatusParams{
		ID: sessionID, Status: sqlcgen.SessionStatusActive,
	}); err != nil {
		t.Fatalf("seed session active: %v", err)
	}

	// --- A Ready sandbox, gen 1 (mirrors resilience_turnrecovery_
	// integration_test.go's own sandbox-seeding precedent). ---
	if _, err := h.Sandboxes.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := h.Sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID, Status: sqlcgen.SandboxStatusReady,
	}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}

	// --- A real turn in Processing, dispatched moments ago -- mirrors
	// resilience_killpod_integration_test.go's own "Create as Pending,
	// then UpdateStatus to Processing with a real dispatched_at" pattern
	// exactly, EXCEPT dispatched_at here is genuinely recent (this
	// scenario's whole point), never comfortably-in-the-past-of-a-
	// shrunk-deadline the way that test's own overdue-timer setup
	// deliberately is. ---
	prompt := "long-running turn that must survive a graceful rolling restart"
	created, err := h.Turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: sessionID,
		Status:    sqlcgen.TurnStatusPending,
		Prompt:    &prompt,
	})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}

	gen1 := int32(1)
	if _, err := h.Turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:                   created.ID,
		Status:               sqlcgen.TurnStatusProcessing,
		DispatchedAt:         pgtype.Timestamptz{Time: time.Now(), Valid: true},
		DispatchedSandboxGen: &gen1,
	}); err != nil {
		t.Fatalf("move turn to processing: %v", err)
	}

	// --- Arm turn_deadline against the harness's own REAL, un-shrunk
	// default (60m, platform.DefaultTimeouts()) comfortably in the
	// future -- deliberately NOT a tiny injected value the way scenario
	// #1's own test uses: nothing here may be even close to due, since
	// the entire point of this scenario is proving graceful shutdown
	// itself never fails a turn, as distinct from "some other already-
	// overdue timer would have failed it anyway". ---
	if _, err := h.Timers.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: sessionID,
		Name:      sessionactor.TimerTurnDeadline,
		FiresAt:   pgtype.Timestamptz{Time: time.Now().Add(h.Timeouts.TurnDeadline), Valid: true},
	}); err != nil {
		t.Fatalf("arm turn_deadline timer: %v", err)
	}

	// --- "Pod A" hydrates and genuinely owns this session (a real
	// advisory lock on pod A's own lock connection) -- exactly what makes
	// the shutdown below a genuine handover, not a hypothetical one. ---
	registryA := h.NewRegistry(ctx, t)
	if _, err := registryA.GetOrSpawn(ctx, sessionID); err != nil {
		t.Fatalf("registryA.GetOrSpawn: %v", err)
	}

	// --- The rolling restart itself: the REAL graceful path
	// (Registry.Shutdown), deliberately NOT killing the Postgres backend/
	// advisory lock the way scenario #1's test does -- see this file's
	// own top comment for why that contrast is the entire point. Every
	// timer armed above is still hours from firing at this point.
	//
	// Shutdown's own doc comment (registry.go) already establishes that
	// its errgroup.Wait() will very likely surface context.Canceled from
	// the actor whose run loop was still alive at shutdown time --
	// expected/benign, not a real failure (cmd/control-plane/main.go's
	// own real caller carves out this exact same case identically). ---
	if err := registryA.Shutdown(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("registryA.Shutdown: %v", err)
	}

	// --- Required assertion: re-read from Postgres (never from in-memory
	// state) that the graceful shutdown did NOT, by itself, mark the
	// still-in-flight turn or its session failed. ---
	gotTurn, err := h.Turns.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if gotTurn.Status != sqlcgen.TurnStatusProcessing {
		t.Fatalf("turn status = %q, want still %q (a graceful shutdown must never force-fail an in-flight turn)",
			gotTurn.Status, sqlcgen.TurnStatusProcessing)
	}
	if gotTurn.CompletedAt.Valid {
		t.Errorf("turn completed_at = %v, want unset (still in flight)", gotTurn.CompletedAt.Time)
	}

	gotSession, err := h.Sessions.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if gotSession.Status != sqlcgen.SessionStatusActive {
		t.Errorf("session status = %q, want still %q (a graceful shutdown must never force-fail a live session)",
			gotSession.Status, sqlcgen.SessionStatusActive)
	}
	if gotSession.FailureReason != nil {
		t.Errorf("session failure_reason = %v, want nil", *gotSession.FailureReason)
	}

	// --- Optional extension (still required to be attempted per this
	// PR's own brief, but non-blocking if it were to fail): "pod B", a
	// fresh Registry against the SAME database, picks the session back
	// up -- simulating the next replica of this binary claiming it after
	// the restart -- and confirms it can genuinely take ownership again,
	// i.e. the session is truly resumable, not merely "not yet marked
	// failed". Registry A's own Shutdown already returned above, which
	// per Actor.shutdown's own ordering (evict from map, THEN release the
	// advisory lock) guarantees the lock is free by the time we get here. ---
	registryB := h.NewRegistry(ctx, t)
	t.Cleanup(func() { _ = registryB.Shutdown() })

	if _, err := registryB.GetOrSpawn(ctx, sessionID); err != nil {
		t.Fatalf("registryB.GetOrSpawn (resuming ownership after graceful handover): %v", err)
	}

	gotTurnAfterHandover, err := h.Turns.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get turn after handover: %v", err)
	}
	if gotTurnAfterHandover.Status != sqlcgen.TurnStatusProcessing {
		t.Errorf("turn status after pod B's handover = %q, want still %q",
			gotTurnAfterHandover.Status, sqlcgen.TurnStatusProcessing)
	}
}
