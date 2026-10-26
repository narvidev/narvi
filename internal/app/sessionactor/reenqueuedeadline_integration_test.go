//go:build integration

// A turn re-sent to a newer sandbox gen keeps the turn_deadline its first
// dispatch armed (technical plan §3.3, §35.4): turn_deadline bounds the
// whole turn, rotations and re-sends included, so a re-send never buys a
// new budget, and a turn whose budget is already spent is not re-sent but
// ended by its deadline. See dispatch.go's planReenqueueOrRespawn and
// tryPlanReenqueue, and turn.ReenqueueBound.
package sessionactor

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// seedTurnInFlight makes a processing turn for sessionID, dispatched to
// gen at dispatchedAt (no dispatch time recorded when it is zero), as its
// first dispatch left it.
func seedTurnInFlight(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, gen int32, dispatchedAt time.Time) sqlcgen.Turn {
	t.Helper()
	turns := narvipg.NewTurnStore(pool)
	created, err := turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: sessionID,
		Status:    sqlcgen.TurnStatusProcessing,
		Prompt:    stringPtrForTest("carry on with the turn in flight"),
	})
	if err != nil {
		t.Fatalf("create processing turn: %v", err)
	}
	messageID := "first-dispatch-" + created.ID.String()
	params := sqlcgen.UpdateTurnStatusParams{
		ID: created.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedSandboxGen: &gen, DispatchedMessageID: &messageID,
	}
	if !dispatchedAt.IsZero() {
		params.DispatchedAt = pgtype.Timestamptz{Time: dispatchedAt, Valid: true}
	}
	row, err := turns.UpdateStatus(ctx, params)
	if err != nil {
		t.Fatalf("stamp the first dispatch: %v", err)
	}
	return row
}

// respawnToGen gives sessionID's sandbox gen, ready, as a respawn that has
// connected and booted leaves it: every upsert for a spawn adds one to its
// gen, the first creating it at 1.
func respawnToGen(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, gen int32) {
	t.Helper()
	sandboxes := narvipg.NewSandboxStore(pool)
	tokenHash := "reenqueue-deadline-token-hash"
	for {
		row, err := sandboxes.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID, TokenHash: &tokenHash})
		if err != nil {
			t.Fatalf("upsert sandbox for spawn: %v", err)
		}
		if row.Gen > gen {
			t.Fatalf("sandbox gen %d passed the wanted gen %d", row.Gen, gen)
		}
		if row.Gen == gen {
			break
		}
	}
	if _, err := sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: sessionID, Status: sqlcgen.SandboxStatusReady}); err != nil {
		t.Fatalf("move sandbox gen %d to ready: %v", gen, err)
	}
}

// armTurnDeadlineAt arms sessionID's turn_deadline at at.
func armTurnDeadlineAt(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, at time.Time) {
	t.Helper()
	if _, err := narvipg.NewTimerStore(pool).Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: sessionID, Name: TimerTurnDeadline, FiresAt: pgtype.Timestamptz{Time: at, Valid: true},
	}); err != nil {
		t.Fatalf("arm turn_deadline: %v", err)
	}
}

// turnDeadlineOf reads sessionID's turn_deadline; ok is false when it has
// none.
func turnDeadlineOf(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) (firesAt time.Time, ok bool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*), coalesce(max(fires_at), 'epoch') FROM session_timers WHERE session_id = $1 AND name = $2`,
		sessionID, TimerTurnDeadline).Scan(&n, &firesAt); err != nil {
		t.Fatalf("read turn_deadline: %v", err)
	}
	if n > 1 {
		t.Fatalf("%d turn_deadline timers, want at most one", n)
	}
	return firesAt, n == 1
}

// getTurnRow reads the turn id.
func getTurnRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id pgtype.UUID) sqlcgen.Turn {
	t.Helper()
	row, err := narvipg.NewTurnStore(pool).Get(ctx, id)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	return row
}

// assertResentAtTheFirstBound checks the turn was re-sent once, to gen,
// as the same turn, keeping its first dispatch's time and with its
// turn_deadline at that dispatch's bound.
func assertResentAtTheFirstBound(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, first sqlcgen.Turn, commander *fakeSendCommander, gen int) {
	t.Helper()
	prompts := sentPromptsOf(t, commander)
	if len(prompts) != 1 || prompts[0].Gen != gen {
		t.Fatalf("prompts %+v (commands %v), want the turn re-sent once, to gen %d", prompts, sentCommandTypes(t, commander), gen)
	}
	got := getTurnRow(ctx, t, pool, first.ID)
	if got.Status != sqlcgen.TurnStatusProcessing || got.DispatchedSandboxGen == nil || int(*got.DispatchedSandboxGen) != gen {
		t.Fatalf("turn %s on gen %v, want processing on gen %d", got.Status, got.DispatchedSandboxGen, gen)
	}
	if got.DispatchedMessageID == nil || prompts[0].MessageId != *got.DispatchedMessageID || prompts[0].MessageId == *first.DispatchedMessageID {
		t.Fatalf("re-sent messageId %q, recorded %v, first %q: want a new one, recorded", prompts[0].MessageId, got.DispatchedMessageID, *first.DispatchedMessageID)
	}
	if !got.DispatchedAt.Time.Equal(first.DispatchedAt.Time) {
		t.Fatalf("dispatched_at moved from %v to %v: a re-send keeps the first dispatch's", first.DispatchedAt.Time, got.DispatchedAt.Time)
	}
	want := first.DispatchedAt.Time.Add(platform.DefaultTimeouts().TurnDeadline)
	firesAt, ok := turnDeadlineOf(ctx, t, pool, sessionID)
	if !ok || !firesAt.Equal(want) {
		t.Fatalf("turn_deadline at %v (armed %v), want %v: the first dispatch's bound, which a re-send never moves", firesAt, ok, want)
	}
}

// TestReenqueue_TurnDeadlineKeepsTheFirstDispatchsBound: a turn in flight
// on gen 1, whose sandbox was lost and respawned, is re-sent to gen 2 on
// its reconnect with the turn_deadline its first dispatch armed -- not a
// fresh budget from the re-send. The bound is armed whatever the timer
// held: as the first dispatch left it, pushed later by a previous
// binary's re-send, or lost.
func TestReenqueue_TurnDeadlineKeepsTheFirstDispatchsBound(t *testing.T) {
	ctx := context.Background()
	deadline := platform.DefaultTimeouts().TurnDeadline
	for _, tc := range []struct {
		name string
		// timer arms turn_deadline before the re-send, given the first
		// dispatch's bound; nil leaves none.
		timer func(bound time.Time) *time.Time
	}{
		{name: "as its first dispatch armed it", timer: func(bound time.Time) *time.Time { return &bound }},
		{name: "pushed later by a previous binary's re-send", timer: func(time.Time) *time.Time {
			later := time.Now().Add(deadline)
			return &later
		}},
		{name: "lost", timer: func(time.Time) *time.Time { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			sessionID := createTestSession(ctx, t, pool)
			dispatchedAt := time.Now().Add(-40 * time.Minute).Truncate(time.Microsecond)
			first := seedTurnInFlight(ctx, t, pool, sessionID, 1, dispatchedAt)
			if at := tc.timer(dispatchedAt.Add(deadline)); at != nil {
				armTurnDeadlineAt(ctx, t, pool, sessionID, *at)
			}
			respawnToGen(ctx, t, pool, sessionID, 2)

			commander := &fakeSendCommander{}
			r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}
			settle(ctx, t, a)

			assertResentAtTheFirstBound(ctx, t, pool, sessionID, first, commander, 2)
		})
	}
}

// TestReenqueue_NoDispatchTimeRecorded_AFullDeadlineFromTheResend: a turn
// whose first dispatch recorded no time has nothing to count from, so its
// re-send arms a full deadline from the re-send, as before -- never a
// bound already spent.
func TestReenqueue_NoDispatchTimeRecorded_AFullDeadlineFromTheResend(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	created := seedTurnInFlight(ctx, t, pool, sessionID, 1, time.Time{})
	respawnToGen(ctx, t, pool, sessionID, 2)

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	before := time.Now()
	settle(ctx, t, a)
	after := time.Now()

	if prompts := sentPromptsOf(t, commander); len(prompts) != 1 || prompts[0].Gen != 2 {
		t.Fatalf("prompts %+v, want the turn re-sent once, to gen 2", prompts)
	}
	if got := getTurnRow(ctx, t, pool, created.ID); got.DispatchedAt.Valid {
		t.Fatalf("dispatched_at = %v, want still none: a re-send never stamps it", got.DispatchedAt.Time)
	}
	deadline := platform.DefaultTimeouts().TurnDeadline
	firesAt, ok := turnDeadlineOf(ctx, t, pool, sessionID)
	if !ok || firesAt.Before(before.Add(deadline).Truncate(time.Microsecond)) || firesAt.After(after.Add(deadline)) {
		t.Fatalf("turn_deadline at %v (armed %v), want a full deadline from the re-send, in [%v, %v]", firesAt, ok, before.Add(deadline), after.Add(deadline))
	}
}

// TestReenqueue_AfterTheGenRanOutOfLifetime_KeepsTheFirstDispatchsBound is
// the bound across the replacement §35.4 is about: the gen running the
// turn is stopped past its lifetime deadline, the evaluation restores its
// snapshot under gen 2, and the turn is re-sent there once gen 2 is ready,
// with its turn_deadline still at its first dispatch's bound.
func TestReenqueue_AfterTheGenRanOutOfLifetime_KeepsTheFirstDispatchsBound(t *testing.T) {
	ctx := context.Background()
	deadline := platform.DefaultTimeouts().TurnDeadline
	dispatchedAt := time.Now().Add(-25 * time.Minute).Truncate(time.Microsecond)
	rig := newRunwayRig(ctx, t, runwayRigOptions{runway: -time.Minute, status: sqlcgen.SandboxStatusStopped,
		beforeActor: func(pool *pgxpool.Pool, sessionID pgtype.UUID) {
			messageID := "first-dispatch-on-gen-1"
			if _, err := pool.Exec(ctx, `UPDATE turns SET status = 'processing', dispatched_at = $2, dispatched_sandbox_gen = 1,
				dispatched_message_id = $3 WHERE session_id = $1`, sessionID, dispatchedAt, messageID); err != nil {
				t.Fatalf("put the turn in flight on gen 1: %v", err)
			}
			armTurnDeadlineAt(ctx, t, pool, sessionID, dispatchedAt.Add(deadline))
		}})
	first := rig.turn(ctx, t)

	settle(ctx, t, rig.actor)
	rig.waitRestored(t, 1)
	rig.bootGen(ctx, t, 2)

	assertResentAtTheFirstBound(ctx, t, rig.pool, rig.sessionID, first, rig.commander, 2)
}

// TestReenqueue_SecondReenqueue_SameBound: a turn re-sent twice, to gen 2
// and then to gen 3, keeps its first dispatch's bound both times -- each
// re-send counts from the first dispatch, never from the one before it.
func TestReenqueue_SecondReenqueue_SameBound(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	deadline := platform.DefaultTimeouts().TurnDeadline
	dispatchedAt := time.Now().Add(-30 * time.Minute).Truncate(time.Microsecond)
	first := seedTurnInFlight(ctx, t, pool, sessionID, 1, dispatchedAt)
	armTurnDeadlineAt(ctx, t, pool, sessionID, dispatchedAt.Add(deadline))
	respawnToGen(ctx, t, pool, sessionID, 2)

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	settle(ctx, t, a)
	assertResentAtTheFirstBound(ctx, t, pool, sessionID, first, commander, 2)

	// Gen 2 is lost in turn, and its replacement, gen 3, comes up ready.
	afterFirst := getTurnRow(ctx, t, pool, first.ID)
	respawnToGen(ctx, t, pool, sessionID, 3)
	settle(ctx, t, a)

	prompts := sentPromptsOf(t, commander)
	if len(prompts) != 2 || prompts[1].Gen != 3 || prompts[1].MessageId == *afterFirst.DispatchedMessageID {
		t.Fatalf("prompts %+v, want a second re-send, to gen 3, under a new messageId", prompts)
	}
	got := getTurnRow(ctx, t, pool, first.ID)
	if got.Status != sqlcgen.TurnStatusProcessing || got.DispatchedSandboxGen == nil || *got.DispatchedSandboxGen != 3 {
		t.Fatalf("turn %s on gen %v, want processing on gen 3", got.Status, got.DispatchedSandboxGen)
	}
	want := dispatchedAt.Add(deadline)
	if firesAt, ok := turnDeadlineOf(ctx, t, pool, sessionID); !ok || !firesAt.Equal(want) {
		t.Fatalf("turn_deadline at %v (armed %v) after the second re-send, want %v: still the first dispatch's bound", firesAt, ok, want)
	}
}

// TestReenqueue_BudgetSpent_NotResent_EndsAtItsDeadline: a turn whose
// turn_deadline, counted from its first dispatch, has run out by the time
// its replacement gen is ready is not re-sent there. Its deadline is
// armed due -- here over a timer a previous binary's re-send had pushed
// later -- and the timer's handler ends it as any timed-out turn ends:
// failed, one synthetic end with reason timeout, one notification.
func TestReenqueue_BudgetSpent_NotResent_EndsAtItsDeadline(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceSlack)
	if _, ok, err := narvipg.NewSlackThreadSessionStore(pool).Claim(ctx, "C140", "1700000000.000140", sessionID); err != nil || !ok {
		t.Fatalf("claim the slack thread: ok=%v err=%v", ok, err)
	}
	deadline := platform.DefaultTimeouts().TurnDeadline
	dispatchedAt := time.Now().Add(-deadline - time.Minute).Truncate(time.Microsecond)
	first := seedTurnInFlight(ctx, t, pool, sessionID, 1, dispatchedAt)
	armTurnDeadlineAt(ctx, t, pool, sessionID, time.Now().Add(30*time.Minute))
	respawnToGen(ctx, t, pool, sessionID, 2)

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	settle(ctx, t, a)

	if got := sentCommandTypes(t, commander); len(got) != 0 {
		t.Fatalf("commands = %v, want none: a turn whose budget is spent is not re-sent", got)
	}
	got := getTurnRow(ctx, t, pool, first.ID)
	if got.Status != sqlcgen.TurnStatusProcessing || got.DispatchedSandboxGen == nil || *got.DispatchedSandboxGen != 1 ||
		got.DispatchedMessageID == nil || *got.DispatchedMessageID != *first.DispatchedMessageID {
		t.Fatalf("turn %s on gen %v, messageId %v; want it untouched until its deadline ends it", got.Status, got.DispatchedSandboxGen, got.DispatchedMessageID)
	}
	firesAt, ok := turnDeadlineOf(ctx, t, pool, sessionID)
	if !ok || firesAt.After(time.Now()) {
		t.Fatalf("turn_deadline at %v (armed %v), want due now", firesAt, ok)
	}

	// The timer pump delivers it, as it does any due timer.
	waitUntil(t, 5*time.Second, func() bool {
		if err := r.PumpOnce(ctx); err != nil {
			t.Fatalf("PumpOnce: %v", err)
		}
		barrier(ctx, t, a)
		return getTurnRow(ctx, t, pool, first.ID).Status == sqlcgen.TurnStatusFailed
	})

	if got := sentCommandTypes(t, commander); len(got) != 0 {
		t.Fatalf("commands = %v, want none", got)
	}
	var ends, timeouts int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE payload->>'reason' = 'timeout' AND payload ? 'synthetic')
		FROM events WHERE session_id = $1 AND type = 'execution_complete'`, sessionID).Scan(&ends, &timeouts); err != nil {
		t.Fatalf("count execution_complete events: %v", err)
	}
	if ends != 1 || timeouts != 1 {
		t.Fatalf("%d execution_complete events, %d of them a synthetic timeout; want exactly one, the timeout", ends, timeouts)
	}
	if n := countOutboxRowsForSession(ctx, t, pool, sessionID); n != 1 {
		t.Fatalf("outbox rows = %d, want one notification", n)
	}
	if _, ok := turnDeadlineOf(ctx, t, pool, sessionID); ok {
		t.Fatal("turn_deadline still armed after it ended the turn")
	}
}
