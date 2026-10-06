//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
)

// dispatchTimerName is sessionactor.TimerDispatch, the kind
// ArmSessionDispatchTimer names in its SQL (sessionactor's own integration
// test pins the two against each other).
const dispatchTimerName = "dispatch"

// dispatchTimerRow reads sessionID's dispatch timer beside the database's
// now(); ok is false when it has none.
func dispatchTimerRow(ctx context.Context, t *testing.T, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, sessionID pgtype.UUID) (timer sqlcgen.SessionTimer, dbNow time.Time, ok bool) {
	t.Helper()
	err := q.QueryRow(ctx, `SELECT fires_at, armed_at, created_at, now() FROM session_timers WHERE session_id = $1 AND name = $2`,
		sessionID, dispatchTimerName).Scan(&timer.FiresAt, &timer.ArmedAt, &timer.CreatedAt, &dbNow)
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.SessionTimer{}, time.Time{}, false
	}
	if err != nil {
		t.Fatalf("read the dispatch timer: %v", err)
	}
	return timer, dbNow, true
}

// countTurns counts sessionID's turns.
func countTurns(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE session_id = $1`, sessionID).Scan(&n); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	return n
}

// TestTurnStore_CreateAndArmDispatch pins the one way production code
// creates a turn (technical plan §2, §3.3): the turn and the session's
// dispatch timer, due at once on the database's clock, in the caller's
// transaction -- both or neither -- and never outside a transaction. A
// second turn re-arms the same row: due at once again even while the pump
// holds it claimed, armed_at moved, created_at kept.
func TestTurnStore_CreateAndArmDispatch(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	turns := narvipg.NewTurnStore(pool)
	prompt := "do the thing"
	params := sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt}

	if _, err := turns.CreateAndArmDispatch(ctx, params, sessionguard.AdmitNewSession(params.SessionID.Bytes)); !errors.Is(err, narvipg.ErrTurnOutsideTransaction) {
		t.Fatalf("CreateAndArmDispatch on the pool = %v, want ErrTurnOutsideTransaction", err)
	}
	if n := countTurns(ctx, t, pool, sessionID); n != 0 {
		t.Fatalf("turns after a refused insert = %d, want 0", n)
	}
	if _, _, ok := dispatchTimerRow(ctx, t, pool, sessionID); ok {
		t.Fatal("a refused insert armed the dispatch timer")
	}

	inTx := func(commit bool) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := turns.WithTx(tx).CreateAndArmDispatch(ctx, params, sessionguard.AdmitNewSession(params.SessionID.Bytes)); err != nil {
			t.Fatalf("CreateAndArmDispatch: %v", err)
		}
		timer, dbNow, ok := dispatchTimerRow(ctx, t, tx, sessionID)
		if !ok {
			t.Fatal("no dispatch timer inside the transaction that created the turn")
		}
		if timer.FiresAt.Time.After(dbNow) {
			t.Fatalf("dispatch timer fires_at %v after the database's now %v: want it due at once", timer.FiresAt.Time, dbNow)
		}
		if !commit {
			if _, _, seen := dispatchTimerRow(ctx, t, pool, sessionID); seen {
				t.Fatal("the dispatch timer is visible outside its uncommitted transaction")
			}
		}
		if commit {
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}

	inTx(false)
	if n := countTurns(ctx, t, pool, sessionID); n != 0 {
		t.Fatalf("turns after a rolled-back insert = %d, want 0", n)
	}
	if _, _, ok := dispatchTimerRow(ctx, t, pool, sessionID); ok {
		t.Fatal("a rolled-back insert left its dispatch timer")
	}

	inTx(true)
	first, _, ok := dispatchTimerRow(ctx, t, pool, sessionID)
	if !ok || countTurns(ctx, t, pool, sessionID) != 1 {
		t.Fatal("a committed insert left no turn or no dispatch timer")
	}

	// The pump holds it claimed, and its last arm is an hour old.
	if _, err := pool.Exec(ctx, `UPDATE session_timers
		SET fires_at = now() + interval '30 seconds', armed_at = armed_at - interval '1 hour'
		WHERE session_id = $1 AND name = $2`, sessionID, dispatchTimerName); err != nil {
		t.Fatal(err)
	}
	claimed, _, _ := dispatchTimerRow(ctx, t, pool, sessionID)
	inTx(true)
	rearmed, dbNow, ok := dispatchTimerRow(ctx, t, pool, sessionID)
	if !ok {
		t.Fatal("the second turn left no dispatch timer")
	}
	if rearmed.FiresAt.Time.After(dbNow) {
		t.Errorf("re-armed fires_at %v after now %v: a new turn must be due at once, claimed or not", rearmed.FiresAt.Time, dbNow)
	}
	if !rearmed.ArmedAt.Time.After(claimed.ArmedAt.Time) {
		t.Errorf("armed_at %v not moved by the re-arm (was %v)", rearmed.ArmedAt.Time, claimed.ArmedAt.Time)
	}
	if !rearmed.CreatedAt.Time.Equal(first.CreatedAt.Time) {
		t.Errorf("created_at %v changed by the re-arm, want the first insert's %v", rearmed.CreatedAt.Time, first.CreatedAt.Time)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_timers WHERE session_id = $1`, sessionID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || countTurns(ctx, t, pool, sessionID) != 2 {
		t.Fatalf("timers = %d, turns = %d after two turns: want one dispatch timer and two turns", rows, countTurns(ctx, t, pool, sessionID))
	}
}

// TestCreateAndArmDispatch_RefusesATerminalTurn: a turn is created pending,
// never already ended. A turn ends only through UpdateStatus, whose
// terminal writes the session actor notes so the re-review debounce wakes
// in the same transaction (technical plan §24.9); one inserted ended would
// end with no wake-up. Every status but pending is refused with
// ErrTurnNotPending, writing neither the turn nor its dispatch timer, the
// open in-flight states included -- the one path to them is the dispatch,
// through the session actor -- and on LockedTurnCreator as well; pending is
// created.
func TestCreateAndArmDispatch_RefusesATerminalTurn(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	turns := narvipg.NewTurnStore(pool)
	creator := narvipg.NewLockedTurnCreator(pool)
	prompt := "review the pull request"

	for _, tc := range []struct {
		status    sqlcgen.TurnStatus
		wantError bool
	}{
		{sqlcgen.TurnStatusPending, false},
		{sqlcgen.TurnStatusDispatched, true},
		{sqlcgen.TurnStatusProcessing, true},
		{sqlcgen.TurnStatusCompleted, true},
		{sqlcgen.TurnStatusFailed, true},
		{sqlcgen.TurnStatusCancelled, true},
		{"", true},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			params := func(sessionID pgtype.UUID) sqlcgen.CreateTurnParams {
				return sqlcgen.CreateTurnParams{SessionID: sessionID, Status: tc.status, Prompt: &prompt}
			}
			check := func(how string, sessionID pgtype.UUID, err error) {
				t.Helper()
				if tc.wantError != errors.Is(err, narvipg.ErrTurnNotPending) || (!tc.wantError && err != nil) {
					t.Fatalf("%s with status %q = %v, want refused %v", how, tc.status, err, tc.wantError)
				}
				wantTurns := 1
				if tc.wantError {
					wantTurns = 0
				}
				if n := countTurns(ctx, t, pool, sessionID); n != wantTurns {
					t.Fatalf("%s with status %q: %d turns, want %d", how, tc.status, n, wantTurns)
				}
				if _, _, armed := dispatchTimerRow(ctx, t, pool, sessionID); armed == tc.wantError {
					t.Fatalf("%s with status %q: dispatch timer armed %v, want %v", how, tc.status, armed, !tc.wantError)
				}
			}

			sessionID := createTestSession(ctx, t, pool)
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = turns.WithTx(tx).CreateAndArmDispatch(ctx, params(sessionID), sessionguard.AdmitNewSession(sessionID.Bytes))
			// Committed either way: a refusal must have written nothing.
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			check("CreateAndArmDispatch", sessionID, err)

			sessionID = createTestSession(ctx, t, pool)
			_, err = creator.CreateLockedTurn(ctx, params(sessionID), admitForTest(sessionID))
			check("CreateLockedTurn", sessionID, err)
		})
	}
}

// TestLockedTurnCreator_CreatesUnderTheActorEpochLock pins the release
// composition review's insert (postgres.LockedTurnCreator): it takes the
// session's actor-epoch row lock before it writes, and its turn and the
// dispatch timer commit together or not at all.
//
// The lock: a transaction holding the session row FOR NO KEY UPDATE --
// which a turn insert's own foreign-key check (FOR KEY SHARE) does not
// wait for, but the actor-epoch lock (FOR UPDATE) does -- holds
// CreateLockedTurn back until it ends; a plain insert gets through at
// once, so the wait is the lock's, not the insert's. Atomicity: with the
// dispatch timer's arm refused by a trigger, CreateLockedTurn fails and
// leaves no turn behind, which two autocommit statements would.
func TestLockedTurnCreator_CreatesUnderTheActorEpochLock(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	creator := narvipg.NewLockedTurnCreator(pool)
	prompt := "review the release"

	t.Run("waits for the actor-epoch lock", func(t *testing.T) {
		sessionID := createTestSession(ctx, t, pool)
		holder, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Rollback(ctx) }()
		if _, err := holder.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR NO KEY UPDATE`, sessionID); err != nil {
			t.Fatal(err)
		}

		// A plain insert is not held back by this lock.
		plainCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if _, err := narvipg.NewTurnStore(pool).Create(plainCtx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending}); err != nil {
			t.Fatalf("a plain insert under a FOR NO KEY UPDATE holder: %v", err)
		}

		var g errgroup.Group
		done := make(chan error, 1)
		g.Go(func() error {
			_, err := creator.CreateLockedTurn(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt}, admitForTest(sessionID))
			done <- err
			return nil
		})
		select {
		case err := <-done:
			t.Fatalf("CreateLockedTurn returned (%v) while another transaction held the session row: it did not take the actor-epoch lock", err)
		case <-time.After(750 * time.Millisecond):
		}
		if _, _, ok := dispatchTimerRow(ctx, t, pool, sessionID); ok {
			t.Fatal("dispatch timer armed while the lock was held")
		}
		if err := holder.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatalf("CreateLockedTurn after the lock was released: %v", err)
		}
		if err := g.Wait(); err != nil {
			t.Fatal(err)
		}
		if n := countTurns(ctx, t, pool, sessionID); n != 2 {
			t.Fatalf("turns = %d, want the plain one and the locked one", n)
		}
		if _, _, ok := dispatchTimerRow(ctx, t, pool, sessionID); !ok {
			t.Fatal("CreateLockedTurn committed its turn without the dispatch timer")
		}
	})

	t.Run("the turn and its timer commit together", func(t *testing.T) {
		sessionID := createTestSession(ctx, t, pool)
		if _, err := pool.Exec(ctx, `CREATE FUNCTION narvi_test_refuse_dispatch_arm() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF NEW.name = 'dispatch' AND NEW.session_id = '`+sessionID.String()+`'::uuid THEN
					RAISE EXCEPTION 'dispatch timer refused by the test';
				END IF;
				RETURN NEW;
			END $$`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS narvi_test_refuse_dispatch_arm() CASCADE`); err != nil {
				t.Errorf("drop the test trigger function: %v", err)
			}
		})
		if _, err := pool.Exec(ctx, `CREATE TRIGGER narvi_test_refuse_dispatch_arm BEFORE INSERT OR UPDATE ON session_timers
			FOR EACH ROW EXECUTE FUNCTION narvi_test_refuse_dispatch_arm()`); err != nil {
			t.Fatal(err)
		}

		if _, err := creator.CreateLockedTurn(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt}, admitForTest(sessionID)); err == nil {
			t.Fatal("CreateLockedTurn succeeded with its dispatch timer refused")
		}
		if n := countTurns(ctx, t, pool, sessionID); n != 0 {
			t.Fatalf("turns = %d after the dispatch timer was refused, want 0: the turn committed without its timer", n)
		}
	})

	t.Run("a session that does not exist", func(t *testing.T) {
		missing := pgtype.UUID{Bytes: [16]byte{1, 2, 3}, Valid: true}
		if _, err := creator.CreateLockedTurn(ctx, sqlcgen.CreateTurnParams{SessionID: missing, Status: sqlcgen.TurnStatusPending, Prompt: &prompt}, admitForTest(missing)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("CreateLockedTurn on a missing session = %v, want pgx.ErrNoRows", err)
		}
	})
}

// admitForTest is a LockedTurnCreator admission that admits sessionID's
// turn without reading anything: these tests are about the insert and its
// lock, not the session guard, whose own tests read the facts
// (sessionguard_store_integration_test.go).
func admitForTest(sessionID pgtype.UUID) narvipg.TurnAdmit {
	return func(context.Context, pgx.Tx) (sessionguard.Admission, error) {
		return sessionguard.AdmitNewSession(sessionID.Bytes), nil
	}
}
