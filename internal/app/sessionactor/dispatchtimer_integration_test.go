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
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
	"github.com/narvidev/narvi/internal/platform"
)

// createTurnArmingDispatch creates a pending turn the way every production
// path does (TurnStore.CreateAndArmDispatch), in a transaction of its own,
// and commits -- and triggers nothing: a replica that died between its
// commit and its post-commit trigger.
func createTurnArmingDispatch(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, prompt string) sqlcgen.Turn {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	created, err := narvipg.NewTurnStore(pool).WithTx(tx).CreateAndArmDispatch(ctx, sqlcgen.CreateTurnParams{
		SessionID: sessionID,
		Status:    sqlcgen.TurnStatusPending,
		Prompt:    &prompt,
	}, sessionguard.AdmitNewSession(sessionID.Bytes))
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return created
}

// dispatchTimer reads sessionID's dispatch timer; ok is false when it has
// none.
func dispatchTimer(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) (sqlcgen.SessionTimer, bool) {
	t.Helper()
	row, err := narvipg.NewTimerStore(pool).Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: sessionID, Name: TimerDispatch})
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.SessionTimer{}, false
	}
	if err != nil {
		t.Fatalf("get the dispatch timer: %v", err)
	}
	return row, true
}

// seedReadySandbox gives sessionID a ready sandbox at gen 1.
func seedReadySandbox(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) {
	t.Helper()
	sandboxes := narvipg.NewSandboxStore(pool)
	if _, err := sandboxes.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready', gen = 1 WHERE session_id = $1`, sessionID); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}
}

// TestDispatchTimer_TheTurnStoreArmsThisKind pins the kind the one turn
// insert arms (postgres.TurnStore.CreateAndArmDispatch, whose SQL names it)
// to TimerDispatch, the kind this package handles and classifies: due at
// once, on the database's clock.
func TestDispatchTimer_TheTurnStoreArmsThisKind(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	createTurnArmingDispatch(ctx, t, pool, sessionID, "do the thing")

	row, ok := dispatchTimer(ctx, t, pool, sessionID)
	if !ok {
		t.Fatalf("no %q timer after CreateAndArmDispatch: the store arms a kind this package does not handle", TimerDispatch)
	}
	var due bool
	if err := pool.QueryRow(ctx, `SELECT $1::timestamptz <= now()`, row.FiresAt).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatalf("dispatch timer fires_at %v is not due: want it armed due at once", row.FiresAt.Time)
	}
	if !HasOwnTimerHandlerForTest(TimerDispatch) {
		t.Fatal("the dispatch kind has no handler of its own: it would be aged and deleted as unknown")
	}
}

// TestDispatchTimer_TheEvaluationDeletesIt: whatever asks for a dispatch
// evaluation -- the post-commit trigger (EnsureDispatched) or the pump's
// delivery of the timer itself (TimerFired) -- deletes the dispatch timer,
// so none lingers once a turn's dispatch has been evaluated. With a turn
// pending and no sandbox, the evaluation spawns one, exactly once; on a
// session with no open turn it deletes the timer and does nothing else.
func TestDispatchTimer_TheEvaluationDeletesIt(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name      string
		cmd       Command
		pending   bool
		wantSpawn int
	}{
		{"the post-commit trigger, a turn pending", EnsureDispatched{}, true, 1},
		{"the timer's own firing, a turn pending", TimerFired{Name: TimerDispatch}, true, 1},
		{"the timer's own firing, nothing open", TimerFired{Name: TimerDispatch}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			sessionID := createTestSession(ctx, t, pool)
			if tc.pending {
				createTurnArmingDispatch(ctx, t, pool, sessionID, "do the thing")
			} else if _, err := pool.Exec(ctx, `INSERT INTO session_timers (session_id, name, fires_at) VALUES ($1, $2, now())`, sessionID, TimerDispatch); err != nil {
				t.Fatal(err)
			}

			provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-object-1"}}
			r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}
			if err := a.Send(ctx, tc.cmd); err != nil {
				t.Fatalf("Send: %v", err)
			}

			waitUntil(t, 5*time.Second, func() bool {
				_, ok := dispatchTimer(ctx, t, pool, sessionID)
				return !ok
			})
			waitUntil(t, 5*time.Second, func() bool { return provider.callCount() == tc.wantSpawn })
			// A second evaluation changes nothing.
			sendEnsureDispatched(ctx, t, a)
			if err := a.Send(ctx, TimerFired{Name: TimerDispatch}); err != nil {
				t.Fatal(err)
			}
			sendEnsureDispatched(ctx, t, a)
			time.Sleep(300 * time.Millisecond)
			if got := provider.callCount(); got != tc.wantSpawn {
				t.Fatalf("CreateSandbox calls = %d after repeated evaluations, want %d", got, tc.wantSpawn)
			}
			if _, ok := dispatchTimer(ctx, t, pool, sessionID); ok {
				t.Fatal("dispatch timer back after repeated evaluations")
			}
		})
	}
}

// TestDispatchTimer_AFiringAfterTheTriggerDispatchesNothingMore: a turn
// whose post-commit trigger succeeded is dispatched once, never twice. The
// pump may still deliver the timer afterwards -- it claimed the row before
// the trigger's evaluation deleted it -- and that firing, and any later
// pump tick, finds the turn in flight on its current sandbox gen and sends
// nothing.
func TestDispatchTimer_AFiringAfterTheTriggerDispatchesNothingMore(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	seedReadySandbox(ctx, t, pool, sessionID)
	created := createTurnArmingDispatch(ctx, t, pool, sessionID, "do the thing")

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return promptCount(commander) == 1 })

	turns := narvipg.NewTurnStore(pool)
	dispatched, err := turns.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dispatched.Status != sqlcgen.TurnStatusProcessing || dispatched.DispatchedMessageID == nil {
		t.Fatalf("turn after the trigger: status %s, message id %v; want processing with a message id", dispatched.Status, dispatched.DispatchedMessageID)
	}
	if _, ok := dispatchTimer(ctx, t, pool, sessionID); ok {
		t.Fatal("dispatch timer still armed after the trigger's evaluation")
	}

	// The pump's late delivery of a claim it took before that delete.
	if err := a.Send(ctx, TimerFired{Name: TimerDispatch}); err != nil {
		t.Fatal(err)
	}
	if err := r.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	time.Sleep(300 * time.Millisecond)

	if got := promptCount(commander); got != 1 {
		t.Fatalf("prompts sent = %d, want 1: a turn whose trigger succeeded was dispatched again", got)
	}
	again, err := turns.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.DispatchedMessageID == nil || *again.DispatchedMessageID != *dispatched.DispatchedMessageID {
		t.Fatalf("dispatched_message_id %v became %v: the turn was dispatched again", *dispatched.DispatchedMessageID, again.DispatchedMessageID)
	}
}

// TestDispatchTimer_NoTriggerIsDeliveredByThePump: a replica that commits a
// turn and dies before its post-commit trigger loses nothing. On a session
// with no sandbox and no other timer, the next pump tick on a replica that
// can host the actor delivers the dispatch evaluation, which spawns the
// sandbox the turn needs and deletes the timer.
func TestDispatchTimer_NoTriggerIsDeliveredByThePump(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	createTurnArmingDispatch(ctx, t, pool, sessionID, "do the thing")

	var others int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_timers WHERE session_id = $1 AND name <> $2`, sessionID, TimerDispatch).Scan(&others); err != nil {
		t.Fatal(err)
	}
	if others != 0 {
		t.Fatalf("session holds %d other timers, want none", others)
	}

	provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-object-1"}}
	r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
	t.Cleanup(func() { _ = r.Shutdown() })
	if err := r.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool { return provider.callCount() == 1 })
	waitUntil(t, 5*time.Second, func() bool {
		_, ok := dispatchTimer(ctx, t, pool, sessionID)
		return !ok
	})
}

// TestDispatchTimer_AReplicaThatCannotHostKeepsTheRow: two registries on one
// database. The replica that cannot deliver a claimed dispatch timer --
// another replica hosts the session (actor_elsewhere), or it cannot host
// actors right now (actor_unavailable) -- leaves the row claimed, never
// dropped, and the replica that can host the actor delivers it when it
// wins the lapsed claim: here, the single lapse that follows, so within
// one claim window of the failed claim. The pump does not favour the
// hosting replica: a replica that cannot host may win a lapsed claim
// again, which costs one more window each time.
func TestDispatchTimer_AReplicaThatCannotHostKeepsTheRow(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		// hostFirst hydrates the session's actor on the replica that can
		// host it before the turn is created: the other one then answers
		// actor_elsewhere. Otherwise the other replica's hydrations are
		// bounded to nothing: actor_unavailable.
		hostFirst bool
		wantErr   error
	}{
		{"actor_elsewhere", true, ErrSessionActorElsewhere},
		{"actor_unavailable", false, ErrActorUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			sessionID := createTestSession(ctx, t, pool)

			timeouts := platform.DefaultTimeouts()
			timeouts.TimerClaimDuration = 2 * time.Second
			hostProvider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "host-object"}}
			host, err := NewRegistry(ctx, pool, timeouts, nil, nil, hostProvider, "http://localhost:8080", nil, nil, "", nil, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = host.Shutdown() })

			otherTimeouts := timeouts
			if !tc.hostFirst {
				otherTimeouts.ActorHydrateTimeout = time.Nanosecond
			}
			otherProvider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "other-object"}}
			other, err := NewRegistry(ctx, pool, otherTimeouts, nil, nil, otherProvider, "http://localhost:8080", nil, nil, "", nil, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = other.Shutdown() })

			if tc.hostFirst {
				if _, err := host.GetOrSpawn(ctx, sessionID); err != nil {
					t.Fatalf("host GetOrSpawn: %v", err)
				}
			}
			createTurnArmingDispatch(ctx, t, pool, sessionID, "do the thing")
			// The trigger on the other replica fails, as it would after
			// the other replica created the turn.
			if _, err := other.GetOrSpawn(ctx, sessionID); !errors.Is(err, tc.wantErr) {
				t.Fatalf("other replica's trigger: GetOrSpawn = %v, want %v", err, tc.wantErr)
			}

			claimedAt := time.Now()
			if err := other.PumpOnce(ctx); err != nil {
				t.Fatalf("other PumpOnce: %v", err)
			}
			row, ok := dispatchTimer(ctx, t, pool, sessionID)
			if !ok {
				t.Fatal("the replica that could not deliver the dispatch timer dropped it")
			}
			if !row.FiresAt.Time.After(claimedAt) {
				t.Fatalf("dispatch timer fires_at %v after the failed claim, want it claimed forward", row.FiresAt.Time)
			}
			// Still claimed: the hosting replica's immediate tick finds
			// nothing due.
			if err := host.PumpOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if got := hostProvider.callCount(); got != 0 {
				t.Fatalf("CreateSandbox calls = %d while the claim stood, want 0", got)
			}

			waitUntil(t, timeouts.TimerClaimDuration+time.Second, func() bool {
				if err := host.PumpOnce(ctx); err != nil {
					t.Fatalf("host PumpOnce: %v", err)
				}
				return hostProvider.callCount() == 1
			})
			if elapsed := time.Since(claimedAt); elapsed > timeouts.TimerClaimDuration+time.Second {
				t.Fatalf("dispatched %v after the failed claim, want within one claim window (%v)", elapsed, timeouts.TimerClaimDuration)
			}
			waitUntil(t, 5*time.Second, func() bool {
				_, ok := dispatchTimer(ctx, t, pool, sessionID)
				return !ok
			})
			if got := otherProvider.callCount(); got != 0 {
				t.Fatalf("the replica that could not host spawned %d sandboxes, want 0", got)
			}
		})
	}
}

// TestDispatchTimer_NoTimerLingersAfterANormalTurn: once a normal turn -- created, triggered,
// dispatched, completed -- has ended, its session holds no dispatch timer,
// so nothing of the durable trigger lingers into a settled session.
func TestDispatchTimer_NoTimerLingersAfterANormalTurn(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	seedReadySandbox(ctx, t, pool, sessionID)
	created := createTurnArmingDispatch(ctx, t, pool, sessionID, "do the thing")

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return promptCount(commander) == 1 })

	raw := executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted)
	var wire struct {
		MessageID string `json:"messageId"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "execution_complete", Gen: 1, MessageID: wire.MessageID, Raw: raw})

	turns := narvipg.NewTurnStore(pool)
	waitUntil(t, 5*time.Second, func() bool {
		got, err := turns.Get(ctx, created.ID)
		return err == nil && got.Status == sqlcgen.TurnStatusCompleted
	})
	if _, ok := dispatchTimer(ctx, t, pool, sessionID); ok {
		t.Fatal("a dispatch timer lingers after a normal turn ended")
	}
}

// TestDispatchTimer_AWriterQueuedBehindAnEvaluationKeepsItsTimer pins the
// ordering technical plan §2 rests on: every dispatch evaluation deletes
// the dispatch timer first, inside its own transaction, under the
// actor-epoch lock -- so a turn committed after that lock re-arms the
// timer after the delete, never before it. The evaluation is held inside
// planDispatch, lock taken and timer deleted, by a transaction that holds
// the turns table; a writer then queues behind the epoch lock to create a
// turn and arm the timer, as createTurnLocked does, and sends no trigger
// (its trigger failed). Once released, the evaluation finds nothing to do
// and commits, the writer commits after it, and the timer the writer armed
// must survive, for the pump to dispatch the new turn: a delete run after
// the evaluation's commit instead would remove it, and strand the turn.
func TestDispatchTimer_AWriterQueuedBehindAnEvaluationKeepsItsTimer(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	seedReadySandbox(ctx, t, pool, sessionID)

	commander := &fakeSendCommander{}
	r := newDispatchTestRegistry(t, ctx, pool, nil, commander)
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	lockWaiters := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatalf("count lock waiters: %v", err)
		}
		return n
	}

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `LOCK TABLE turns IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("hold the turns table: %v", err)
	}

	// The evaluation: epoch lock taken, timer deleted, then held at its
	// read of the turns.
	sendEnsureDispatched(ctx, t, a)
	waitUntil(t, 5*time.Second, func() bool { return lockWaiters() >= 1 })

	// The writer, queued behind the evaluation's epoch lock.
	var g errgroup.Group
	g.Go(func() error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := narvipg.NewSessionStore(pool).WithTx(tx).GetActorEpochForUpdate(ctx, sessionID); err != nil {
			return err
		}
		prompt := "the turn behind the evaluation"
		if _, err := narvipg.NewTurnStore(pool).WithTx(tx).CreateAndArmDispatch(ctx, sqlcgen.CreateTurnParams{
			SessionID: sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt,
		}, sessionguard.AdmitNewSession(sessionID.Bytes)); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	waitUntil(t, 5*time.Second, func() bool { return lockWaiters() >= 2 })

	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("the writer: %v", err)
	}
	// Let the evaluation finish whatever it does after its commit.
	time.Sleep(300 * time.Millisecond)

	if _, ok := dispatchTimer(ctx, t, pool, sessionID); !ok {
		t.Fatal("the dispatch timer the writer armed after the evaluation's lock is gone: the evaluation deleted it after its own commit, and the new turn is stranded")
	}
	if err := r.PumpOnce(ctx); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 5*time.Second, func() bool { return promptCount(commander) == 1 })
}
