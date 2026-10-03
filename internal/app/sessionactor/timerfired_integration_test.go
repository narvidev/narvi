//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// TestTurnDeadlineTimerFired_FullRoundTrip seeds a session with a single
// turn already Processing and long overdue against a tiny injected
// TurnDeadline, fires TimerFired{Name: TimerTurnDeadline} through a real
// Actor, and confirms -- reading everything back from Postgres, not from
// in-memory state -- that: the turn transitioned to Failed with
// completed_at set; a synthetic execution_complete event was appended;
// the session's derived status/failure_reason became Failed/Timeout; and
// the turn_deadline timer itself was deleted (the handler's own
// re-arm-or-delete contract).
func TestTurnDeadlineTimerFired_FullRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	timeouts := platform.DefaultTimeouts()
	timeouts.TurnDeadline = 50 * time.Millisecond // tiny, injected -- not the real 60m default

	turnStore := narvipg.NewTurnStore(pool)
	created, err := turnStore.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: sessionID,
		Status:    sqlcgen.TurnStatusPending,
	})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}

	dispatchedAt := time.Now().Add(-1 * time.Hour) // comfortably past the tiny deadline
	if _, err := turnStore.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:           created.ID,
		Status:       sqlcgen.TurnStatusProcessing,
		DispatchedAt: pgtype.Timestamptz{Time: dispatchedAt, Valid: true},
	}); err != nil {
		t.Fatalf("move turn to processing: %v", err)
	}

	r, err := NewRegistry(ctx, pool, timeouts, nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	if err := a.Send(ctx, TimerFired{Name: TimerTurnDeadline}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	waitUntil(t, 5*time.Second, func() bool {
		got, err := turnStore.Get(ctx, created.ID)
		return err == nil && got.Status == sqlcgen.TurnStatusFailed
	})

	gotTurn, err := turnStore.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if gotTurn.Status != sqlcgen.TurnStatusFailed {
		t.Fatalf("turn status = %q, want %q", gotTurn.Status, sqlcgen.TurnStatusFailed)
	}
	if !gotTurn.CompletedAt.Valid {
		t.Error("turn completed_at not set")
	}

	sessionStore := narvipg.NewSessionStore(pool)
	gotSession, err := sessionStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if gotSession.Status != sqlcgen.SessionStatusFailed {
		t.Errorf("session status = %q, want %q", gotSession.Status, sqlcgen.SessionStatusFailed)
	}
	if gotSession.FailureReason == nil || *gotSession.FailureReason != sqlcgen.SessionFailureReasonTimeout {
		t.Errorf("session failure_reason = %v, want %q", gotSession.FailureReason, sqlcgen.SessionFailureReasonTimeout)
	}

	timerStore := narvipg.NewTimerStore(pool)
	if _, err := timerStore.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: sessionID, Name: TimerTurnDeadline}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("turn_deadline timer get = %v, want pgx.ErrNoRows (handler must delete it once handled)", err)
	}

	var eventCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM events WHERE session_id = $1 AND type = 'execution_complete'`,
		sessionID,
	).Scan(&eventCount); err != nil {
		t.Fatalf("count execution_complete events: %v", err)
	}
	if eventCount != 1 {
		t.Errorf("execution_complete event count = %d, want 1 (synthetic completion, §3.3)", eventCount)
	}
	// The timed-out turn was the one in flight: its end carries the
	// dispatched stamp a page ends the turn it shows on (syntheticend.go).
	var dispatched bool
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE((payload->>'dispatched')::boolean, false) FROM events WHERE session_id = $1 AND type = 'execution_complete'`,
		sessionID,
	).Scan(&dispatched); err != nil {
		t.Fatalf("read the synthetic end's stamp: %v", err)
	}
	if !dispatched {
		t.Error("the timed-out turn's synthetic end carries no dispatched stamp, want it")
	}
}

// TestLivenessCheckTimerFired_FullRoundTrip proves the second half of this
// batch's fix genuinely closes the gap end to end, not just that the arm
// step in isolation exists: a Ready sandbox whose liveness_check timer is
// (directly seeded, bypassing the live-heartbeat path entirely --
// mirroring TestTurnDeadlineTimerFired_FullRoundTrip's own established
// precedent for this exact kind of test) overdue, fired through a real
// Actor via TimerFired, genuinely transitions the sandbox to Suspect and
// arms terminal_grace -- the SAME transitionSandboxToSuspect path every
// other watchdog timeout uses (§3.2: "a watchdog never writes failed
// directly. It writes suspect and arms terminal_grace").
func TestLivenessCheckTimerFired_FullRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	staleSince := time.Now().Add(-1 * time.Hour) // comfortably past the default 90s SteadyHeartbeatBudget
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID:  sessionID,
		Status:     sqlcgen.SandboxStatusReady,
		LastSeenAt: pgtype.Timestamptz{Time: staleSince, Valid: true},
	}); err != nil {
		t.Fatalf("move sandbox to ready with stale last_seen_at: %v", err)
	}

	timerStore := narvipg.NewTimerStore(pool)
	if _, err := timerStore.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: sessionID,
		Name:      TimerLivenessCheck,
		FiresAt:   pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Minute), Valid: true}, // already overdue
	}); err != nil {
		t.Fatalf("seed overdue liveness_check timer: %v", err)
	}

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	if err := a.Send(ctx, TimerFired{Name: TimerLivenessCheck}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	waitUntil(t, 5*time.Second, func() bool {
		got, err := sandboxStore.Get(ctx, sessionID)
		return err == nil && got.Status == sqlcgen.SandboxStatusSuspect
	})

	gotSandbox, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if gotSandbox.Status != sqlcgen.SandboxStatusSuspect {
		t.Fatalf("sandbox status = %s, want %s", gotSandbox.Status, sqlcgen.SandboxStatusSuspect)
	}

	if _, err := timerStore.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: sessionID, Name: TimerLivenessCheck}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("liveness_check timer get = %v, want pgx.ErrNoRows (handler must delete it once handled)", err)
	}

	graceRow, err := timerStore.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: sessionID, Name: TimerTerminalGrace})
	if err != nil {
		t.Fatalf("get terminal_grace timer: %v (expected it to be armed by transitionSandboxToSuspect)", err)
	}
	if !graceRow.FiresAt.Valid {
		t.Error("terminal_grace fires_at not set")
	}
}

// TestInactivityTimerFired_FullRoundTrip mirrors
// TestLivenessCheckTimerFired_FullRoundTrip for the `inactivity` named
// timer: a Ready, non-processing sandbox whose last_seen_at is older than
// InactivityTimeout, with a directly-seeded overdue inactivity timer row,
// genuinely transitions to Suspect and arms terminal_grace via the SAME
// shared transitionSandboxToSuspect path handleLivenessCheckTimer's own
// test above just proved.
func TestInactivityTimerFired_FullRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	staleSince := time.Now().Add(-1 * time.Hour) // comfortably past the default 10min InactivityTimeout
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID:  sessionID,
		Status:     sqlcgen.SandboxStatusReady,
		LastSeenAt: pgtype.Timestamptz{Time: staleSince, Valid: true},
	}); err != nil {
		t.Fatalf("move sandbox to ready with stale last_seen_at: %v", err)
	}
	// No turns at all: anyTurnProcessing (timerfired.go) is false, so
	// EvaluateInactivityTimeout's own IsProcessing-deferral branch does not
	// apply here.

	timerStore := narvipg.NewTimerStore(pool)
	if _, err := timerStore.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: sessionID,
		Name:      TimerInactivity,
		FiresAt:   pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Minute), Valid: true}, // already overdue
	}); err != nil {
		t.Fatalf("seed overdue inactivity timer: %v", err)
	}

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	if err := a.Send(ctx, TimerFired{Name: TimerInactivity}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	waitUntil(t, 5*time.Second, func() bool {
		got, err := sandboxStore.Get(ctx, sessionID)
		return err == nil && got.Status == sqlcgen.SandboxStatusSuspect
	})

	gotSandbox, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if gotSandbox.Status != sqlcgen.SandboxStatusSuspect {
		t.Fatalf("sandbox status = %s, want %s", gotSandbox.Status, sqlcgen.SandboxStatusSuspect)
	}

	if _, err := timerStore.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: sessionID, Name: TimerInactivity}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("inactivity timer get = %v, want pgx.ErrNoRows (handler must delete it once handled)", err)
	}

	graceRow, err := timerStore.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: sessionID, Name: TimerTerminalGrace})
	if err != nil {
		t.Fatalf("get terminal_grace timer: %v (expected it to be armed by transitionSandboxToSuspect)", err)
	}
	if !graceRow.FiresAt.Valid {
		t.Error("terminal_grace fires_at not set")
	}
}

// TestConnectingDeadlineHandoff_ToLivenessCheck drives the REAL sequence
// this batch's own design decision claims needs no change to
// handleConnectingDeadlineTimer: connecting_deadline is armed at spawn
// time (simulated here by seeding it directly, exactly like a real
// tryPlanSpawn write would have left it), the sandbox then genuinely
// transitions Booting->Ready via a real heartbeat (arming liveness_check
// in the SAME transact, this batch's own fix), and only THEN does the
// stale connecting_deadline timer fire. Proves the hand-off is real, not
// assumed: connecting_deadline deletes itself (isConnectingPhase(Ready) is
// false) exactly as handleConnectingDeadlineTimer's own pre-existing logic
// already does, while liveness_check -- already armed by the Ready
// transition -- is left completely untouched by that firing.
func TestConnectingDeadlineHandoff_ToLivenessCheck(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	created, err := sandboxStore.Create(ctx, sessionID)
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID,
		Status:    sqlcgen.SandboxStatusBooting,
	}); err != nil {
		t.Fatalf("move sandbox to booting: %v", err)
	}

	timerStore := narvipg.NewTimerStore(pool)
	// Simulates tryPlanSpawn's own arm-at-spawn-time write (dispatch.go) --
	// this is the ONE call site that arms connecting_deadline in
	// production, seeded here directly so this test does not have to drive
	// the full spawn machinery (provider/commander are irrelevant to what
	// this test proves).
	if _, err := timerStore.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: sessionID,
		Name:      TimerConnectingDeadline,
		FiresAt:   pgtype.Timestamptz{Time: time.Now().Add(platform.DefaultTimeouts().FirstConnectBudget), Valid: true},
	}); err != nil {
		t.Fatalf("seed connecting_deadline timer: %v", err)
	}

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	// This gen's boot evidence (§3.2): the agent's own successful
	// boot_duration, sent once its boot sequence has finished.
	btRaw := json.RawMessage(`{"type":"boot_timing","messageId":"bt1","sessionId":"s","gen":1,"metric":"boot_duration","seconds":42,"failed":false}`)
	btReply := make(chan SandboxEventOutcome, 1)
	if err := a.Send(ctx, SandboxEvent{Type: "boot_timing", Gen: int(created.Gen), MessageID: "bt1", Raw: btRaw, Reply: btReply}); err != nil {
		t.Fatalf("Send boot_timing: %v", err)
	}
	select {
	case <-btReply:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for boot_timing outcome")
	}

	// The real Booting->Ready trigger (sandboxTransitionTrigger's own (b)
	// mapping): this batch's fix arms liveness_check/inactivity in the
	// SAME transact as this transition.
	hbRaw := json.RawMessage(`{"type":"heartbeat","messageId":"h1","sessionId":"s","gen":1,"conversationId":null,"lastBootPhase":null}`)
	reply := make(chan SandboxEventOutcome, 1)
	if err := a.Send(ctx, SandboxEvent{Type: "heartbeat", Gen: int(created.Gen), Raw: hbRaw, LastBootPhase: nil, Reply: reply}); err != nil {
		t.Fatalf("Send heartbeat: %v", err)
	}
	select {
	case outcome := <-reply:
		if !outcome.Persisted {
			t.Fatal("heartbeat: Persisted = false, want true")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for heartbeat outcome")
	}

	gotSandbox, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if gotSandbox.Status != sqlcgen.SandboxStatusReady {
		t.Fatalf("status after heartbeat = %s, want %s", gotSandbox.Status, sqlcgen.SandboxStatusReady)
	}

	livenessBefore, err := timerStore.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: sessionID, Name: TimerLivenessCheck})
	if err != nil {
		t.Fatalf("get liveness_check timer (expected armed by the Ready transition): %v", err)
	}

	// NOW the stale connecting_deadline timer -- armed back at spawn time,
	// never re-armed since (nothing in this batch's fix touches it) --
	// finally fires. handleConnectingDeadlineTimer's own pre-existing
	// logic must delete it (isConnectingPhase(Ready) == false), never
	// re-arm it, and must not touch liveness_check at all.
	if err := a.Send(ctx, TimerFired{Name: TimerConnectingDeadline}); err != nil {
		t.Fatalf("Send TimerFired connecting_deadline: %v", err)
	}

	waitUntil(t, 5*time.Second, func() bool {
		_, err := timerStore.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: sessionID, Name: TimerConnectingDeadline})
		return errors.Is(err, pgx.ErrNoRows)
	})

	if _, err := timerStore.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: sessionID, Name: TimerConnectingDeadline}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("connecting_deadline timer get = %v, want pgx.ErrNoRows (must delete itself once status is no longer connecting-phase)", err)
	}

	livenessAfter, err := timerStore.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: sessionID, Name: TimerLivenessCheck})
	if err != nil {
		t.Fatalf("get liveness_check timer after connecting_deadline fired: %v (must still be armed and watching)", err)
	}
	if !livenessAfter.FiresAt.Time.Equal(livenessBefore.FiresAt.Time) {
		t.Errorf("liveness_check fires_at changed when connecting_deadline fired: before=%v after=%v (connecting_deadline's own handler must not touch it)",
			livenessBefore.FiresAt.Time, livenessAfter.FiresAt.Time)
	}

	gotSandbox, err = sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if gotSandbox.Status != sqlcgen.SandboxStatusReady {
		t.Errorf("status after connecting_deadline fired = %s, want unchanged %s", gotSandbox.Status, sqlcgen.SandboxStatusReady)
	}
}

// TestTerminalGraceTimerFired_RedeliversEnsureDispatched_FreshSpawnAttempt
// proves Finding 3's own fix end to end, driving the REAL production entry
// point (a.Send(TimerFired{Name: TimerTerminalGrace}), never a bypass --
// matching this whole audit series' own established discipline, Batch 4):
// a session whose sandbox never managed to spawn a second time (left
// Suspect, e.g. by a permanent spawn failure or a watchdog timeout) still
// has a genuinely Pending turn when terminal_grace finally fires. Before
// this batch's fix, nothing would ever call EnsureDispatched again for
// this session (no sandbox event can ever arrive -- no sandbox process
// exists to send one) and the turn would stay Pending forever. This test
// proves handleTerminalGraceTimer's own transact committing Suspect->Failed
// now genuinely triggers handleEnsureDispatched, which finds the Pending
// turn, sees the (now dead, Failed) sandbox, and performs a REAL fresh
// spawn attempt: a real ports.SandboxProvider.CreateSandbox call is made,
// and the sandbox row lands in Connecting with a bumped gen once that call
// succeeds -- exactly dispatch.go's own pre-existing "spawn again from
// Failed via SpawnTrigger" logic, finally getting the chance to run again.
func TestTerminalGraceTimerFired_RedeliversEnsureDispatched_FreshSpawnAttempt(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	created, err := sandboxStore.Create(ctx, sessionID)
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := sandboxStore.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
		SessionID: sessionID,
		Status:    sqlcgen.SandboxStatusSuspect,
	}); err != nil {
		t.Fatalf("move sandbox to suspect: %v", err)
	}

	turnStore := narvipg.NewTurnStore(pool)
	createPendingTurn(ctx, t, turnStore, sessionID, "do the thing, once a sandbox is ever actually spawned")

	timerStore := narvipg.NewTimerStore(pool)
	// Simulates transitionSandboxToSuspect's own arm-at-suspect-time write
	// (timerfired.go) -- a real production Suspect transition always arms
	// this alongside it; seeded directly here so this test does not have
	// to drive whichever watchdog timeout produced the Suspect state in
	// the first place (irrelevant to what this test proves).
	if _, err := timerStore.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: sessionID,
		Name:      TimerTerminalGrace,
		FiresAt:   pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Minute), Valid: true}, // already overdue
	}); err != nil {
		t.Fatalf("seed overdue terminal_grace timer: %v", err)
	}

	provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-object-redelivery"}}
	r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	if err := a.Send(ctx, TimerFired{Name: TimerTerminalGrace}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// A genuine fresh spawn attempt was made: the real (fake, but real
	// call-path) SandboxProvider.CreateSandbox was invoked -- this is the
	// whole point of the fix (EnsureDispatched actually ran, found the
	// Pending turn + now-dead sandbox, and decided to spawn).
	waitUntil(t, 5*time.Second, func() bool {
		return provider.callCount() == 1
	})

	if got := provider.callCount(); got != 1 {
		t.Fatalf("provider.CreateSandbox called %d times, want exactly 1", got)
	}

	// The spawn succeeded end to end: sandbox lands in Connecting with a
	// bumped gen (executeSpawn's own Spawning->Connecting transition,
	// dispatch.go), not left stuck in Failed.
	waitUntil(t, 5*time.Second, func() bool {
		got, err := sandboxStore.Get(ctx, sessionID)
		return err == nil && got.Status == sqlcgen.SandboxStatusConnecting
	})

	gotSandbox, err := sandboxStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if gotSandbox.Status != sqlcgen.SandboxStatusConnecting {
		t.Fatalf("sandbox status = %s, want %s", gotSandbox.Status, sqlcgen.SandboxStatusConnecting)
	}
	if gotSandbox.Gen <= created.Gen {
		t.Errorf("sandbox gen = %d, want > %d (a genuinely fresh spawn, not the original)", gotSandbox.Gen, created.Gen)
	}

	if _, err := timerStore.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: sessionID, Name: TimerTerminalGrace}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("terminal_grace timer get = %v, want pgx.ErrNoRows (handler must delete it once handled)", err)
	}
}
