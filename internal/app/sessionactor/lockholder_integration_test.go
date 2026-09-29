//go:build integration

// Integration tests for the lock connection (lockholder.go): a replica
// holds every one of its session actors' advisory locks on ONE dedicated
// connection outside its query pool, so hosting sessions never takes a
// query connection (§2, §5.1). Each test builds its own small pools from
// the shared container (IntegrationTestPoolAndConnStr) and shortened
// timeouts, so a saturated pool is cheap to reach and a bound cheap to
// wait out.
//
// No test here terminates any backend but the one lock connection its own
// Registry opened, found by pid AND a lock connection's application_name
// AND this test's own database -- never killAdvisoryLockHolder's unscoped
// sweep, which is only safe on newTestPoolPair's dedicated container.
package sessionactor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/platform"
)

// lockTestTimeouts shortens the lock connection's timeouts: hydration
// 300 ms, each lock statement 100 ms, a probe every 2 s, each connect
// attempt 1 s, and 200 ms for a lost connection's backend to end -- two of
// Postgres's looks for its end -- a valid set. No test here runs the probe
// loop on it at that pace: each one that probes calls ProbeLockOnce
// itself.
func lockTestTimeouts() platform.Timeouts {
	to := platform.DefaultTimeouts()
	to.ActorHydrateTimeout = 300 * time.Millisecond
	to.ActorLockStatementTimeout = 100 * time.Millisecond
	to.ActorLockProbeInterval = 2 * time.Second
	to.ActorLockConnectAttemptTimeout = time.Second
	to.ActorLockOrphanTerminateWait = 200 * time.Millisecond
	return to
}

// newLockTestPool opens a pool of maxConns connections on the shared
// container, closed when the test ends -- after any Registry built on it
// has shut down (t.Cleanup is LIFO, and the Registry is built after).
func newLockTestPool(ctx context.Context, t *testing.T, connStr string, maxConns int32) *pgxpool.Pool {
	t.Helper()
	pool, err := narvipg.NewPoolWithMaxConns(ctx, connStr, maxConns)
	if err != nil {
		t.Fatalf("NewPoolWithMaxConns(%d): %v", maxConns, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newLockTestRegistry builds a Registry on pool with timeouts, shut down
// when the test ends.
func newLockTestRegistry(ctx context.Context, t *testing.T, pool *pgxpool.Pool, timeouts platform.Timeouts) *Registry {
	t.Helper()
	r, err := NewRegistry(ctx, pool, timeouts, nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	return r
}

// backendPIDForTest reports the lock connection's backend pid, or 0 if
// none is open. It is the pid pgconn was handed at startup: behind a
// pooler, the pooler's (serverPIDForTest).
func (h *lockHolder) backendPIDForTest() uint32 {
	h.sem <- struct{}{}
	defer h.leave()
	if h.conn == nil {
		return 0
	}
	return h.conn.PgConn().PID()
}

// serverPIDForTest reports the open lock connection's backend pid as the
// server reported it at the dial, or 0 if none is open or it was not read.
func (h *lockHolder) serverPIDForTest() uint32 {
	h.sem <- struct{}{}
	defer h.leave()
	if h.conn == nil || h.backend == nil {
		return 0
	}
	return uint32(h.backend.pid)
}

// appNameForTest reports the open lock connection's application_name, or
// "" if none is open or its backend was not read.
func (h *lockHolder) appNameForTest() string {
	h.sem <- struct{}{}
	defer h.leave()
	if h.conn == nil || h.backend == nil {
		return ""
	}
	return h.backend.appName
}

// isLockConnApplicationName reports whether name is one a lock
// connection's dial gives it.
func isLockConnApplicationName(name string) bool {
	return strings.HasPrefix(name, lockConnApplicationNamePrefix) && len(name) > len(lockConnApplicationNamePrefix)
}

// advisoryLockHolders returns the pids holding sessionID's advisory lock
// (a bigint key: its high half in classid, its low half in objid, and
// objsubid 1 -- Postgres's own documented pg_locks layout).
func advisoryLockHolders(ctx context.Context, t *testing.T, admin *pgxpool.Pool, sessionID pgtype.UUID) []uint32 {
	t.Helper()
	rows, err := admin.Query(ctx, `
		WITH k AS (SELECT hashtextextended($1::text, 0) AS key)
		SELECT l.pid FROM pg_locks l, k
		WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 1
		  AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND l.classid::bigint = ((k.key >> 32) & 4294967295)
		  AND l.objid::bigint = (k.key & 4294967295)
		ORDER BY l.pid`, sessionID.String())
	if err != nil {
		t.Fatalf("query advisory lock holders: %v", err)
	}
	pids, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (uint32, error) {
		var pid int32
		err := row.Scan(&pid)
		return uint32(pid), err
	})
	if err != nil {
		t.Fatalf("collect advisory lock holders: %v", err)
	}
	return pids
}

// terminateLockBackend terminates pid -- only if it is a lock connection
// (an application_name beginning narvi-actor-locks-) on this test's own
// database -- and returns once the backend is gone and its locks with it.
func terminateLockBackend(ctx context.Context, t *testing.T, admin *pgxpool.Pool, pid uint32) {
	t.Helper()
	var terminated bool
	err := admin.QueryRow(ctx, `
		SELECT pg_terminate_backend(pid, 5000) FROM pg_stat_activity
		WHERE pid = $1 AND starts_with(application_name, $2) AND datname = current_database()`,
		int32(pid), lockConnApplicationNamePrefix).Scan(&terminated)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("pid %d is not a %s* backend on this test's database; refusing to terminate it", pid, lockConnApplicationNamePrefix)
	}
	if err != nil {
		t.Fatalf("pg_terminate_backend(%d): %v", pid, err)
	}
	if !terminated {
		t.Fatalf("pg_terminate_backend(%d) did not terminate it within 5s", pid)
	}
	waitUntil(t, 5*time.Second, func() bool {
		var n int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid = $1`, int32(pid)).Scan(&n); err != nil {
			t.Fatalf("recheck pid %d: %v", pid, err)
		}
		return n == 0
	})
}

// TestLockHolder_ActorsHoldNoPoolConnection (T1) proves a live session
// actor holds no query-pool connection: six actors on a pool of two --
// three times the pool -- leave it with nothing out after every spawn,
// unrelated queries and a timer-pump tick still run at once, all six locks
// sit on the one lock connection's backend, which reports the
// application_name its dial chose, and another pod still sees
// every session as owned. Taking the lock on a pool connection again, per
// actor or once for the holder, fails here.
func TestLockHolder_ActorsHoldNoPoolConnection(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	pool := newLockTestPool(ctx, t, connStr, 2)
	r := newLockTestRegistry(ctx, t, pool, lockTestTimeouts())

	liveBefore := readCounterSum(ctx, t, otelReader, "session_actors_live")
	okBefore := readCounterSumByAttr(ctx, t, otelReader, "session_actor_hydrations", "outcome", "ok")

	sessions := make([]pgtype.UUID, 6)
	for i := range sessions {
		sessions[i] = createTestSession(ctx, t, admin)
	}
	for i, id := range sessions {
		if _, err := r.GetOrSpawn(ctx, id); err != nil {
			t.Fatalf("GetOrSpawn for actor %d of %d: %v", i+1, len(sessions), err)
		}
		if got := pool.Stat().AcquiredConns(); got != 0 {
			t.Fatalf("after spawning actor %d of %d, the query pool has %d connections out, want 0", i+1, len(sessions), got)
		}
	}

	if got := readCounterSum(ctx, t, otelReader, "session_actors_live") - liveBefore; got != int64(len(sessions)) {
		t.Errorf("session_actors_live moved by %d, want %d", got, len(sessions))
	}
	if got := readCounterSumByAttr(ctx, t, otelReader, "session_actor_hydrations", "outcome", "ok") - okBefore; got != int64(len(sessions)) {
		t.Errorf("session_actor_hydrations{outcome=ok} moved by %d, want %d", got, len(sessions))
	}

	queryCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var one int
	if err := pool.QueryRow(queryCtx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("an unrelated query on the same pool, with %d actors live: %v", len(sessions), err)
	}
	pumpStarted := time.Now()
	if err := r.PumpOnce(queryCtx); err != nil {
		t.Fatalf("PumpOnce with %d actors live: %v", len(sessions), err)
	}
	if elapsed := time.Since(pumpStarted); elapsed > time.Second {
		t.Fatalf("PumpOnce took %v with %d actors live, want under 1s", elapsed, len(sessions))
	}

	lockPID := r.locks.backendPIDForTest()
	if lockPID == 0 {
		t.Fatal("no lock connection open after six spawns")
	}
	var appName string
	if err := admin.QueryRow(ctx, `SELECT application_name FROM pg_stat_activity WHERE pid = $1`, int32(lockPID)).Scan(&appName); err != nil {
		t.Fatalf("read the lock backend's application_name: %v", err)
	}
	if want := r.locks.appNameForTest(); appName != want || !isLockConnApplicationName(appName) || len(appName) > 63 {
		t.Fatalf("lock backend application_name = %q, want the dial's own %q: %s and a nonce, 63 bytes at most", appName, want, lockConnApplicationNamePrefix)
	}
	for i, id := range sessions {
		if got := advisoryLockHolders(ctx, t, admin, id); len(got) != 1 || got[0] != lockPID {
			t.Errorf("session %d's advisory lock is held by pids %v, want only the lock backend %d", i+1, got, lockPID)
		}
	}
	var onLockBackend int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted AND pid = $1`, int32(lockPID),
	).Scan(&onLockBackend); err != nil {
		t.Fatalf("count the lock backend's advisory locks: %v", err)
	}
	if onLockBackend != len(sessions) {
		t.Errorf("the lock backend holds %d granted advisory locks, want %d", onLockBackend, len(sessions))
	}

	otherPod := newLockTestRegistry(ctx, t, newLockTestPool(ctx, t, connStr, 2), lockTestTimeouts())
	for i, id := range sessions {
		if _, err := otherPod.GetOrSpawn(ctx, id); !errors.Is(err, ErrSessionActorElsewhere) {
			t.Errorf("another pod's GetOrSpawn for session %d = %v, want ErrSessionActorElsewhere", i+1, err)
		}
	}
}

// TestLockHolder_HydrateFailsFastWhenPoolSaturated (T2) proves hydration
// is bounded and cleans up after itself: with every query connection
// taken, a spawn fails with ErrActorUnavailable within
// ActorHydrateTimeout -- whatever deadline its caller gave it -- having
// released the advisory lock it took, and succeeds as soon as the pool
// frees. "The (N+1)th fails fast, then succeeds once capacity frees",
// where capacity is now the query pool, not a count of actors.
func TestLockHolder_HydrateFailsFastWhenPoolSaturated(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	pool := newLockTestPool(ctx, t, connStr, 2)
	timeouts := lockTestTimeouts()
	r := newLockTestRegistry(ctx, t, pool, timeouts)
	sessionID := createTestSession(ctx, t, admin)

	unavailableBefore := readCounterSumByAttr(ctx, t, otelReader, "session_actor_hydrations", "outcome", "unavailable")

	held := make([]*pgxpool.Conn, 0, 2)
	for range 2 {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("hold a pool connection: %v", err)
		}
		held = append(held, c)
	}
	releaseHeld := func() {
		for _, c := range held {
			c.Release()
		}
		held = nil
	}
	t.Cleanup(releaseHeld)

	// The caller's own deadline is far past the bound: only
	// ActorHydrateTimeout may end this call.
	callerCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	started := time.Now()
	_, err := r.GetOrSpawn(callerCtx, sessionID)
	elapsed := time.Since(started)
	cancel()
	if !errors.Is(err, ErrActorUnavailable) {
		t.Fatalf("GetOrSpawn on a saturated pool = %v, want ErrActorUnavailable", err)
	}
	if limit := timeouts.ActorHydrateTimeout + 200*time.Millisecond; elapsed > limit {
		t.Fatalf("GetOrSpawn on a saturated pool took %v, want at most %v (ActorHydrateTimeout + 200ms)", elapsed, limit)
	}
	if got := readCounterSumByAttr(ctx, t, otelReader, "session_actor_hydrations", "outcome", "unavailable") - unavailableBefore; got != 1 {
		t.Errorf("session_actor_hydrations{outcome=unavailable} moved by %d, want 1", got)
	}

	// The failed hydration released its lock: another backend takes it.
	otherConn, err := newLockTestPool(ctx, t, connStr, 1).Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire another pod's connection: %v", err)
	}
	var otherLocked, otherUnlocked bool
	if err := otherConn.QueryRow(ctx, tryAdvisoryLockQuery, sessionID.String()).Scan(&otherLocked); err != nil {
		t.Fatalf("another pod's pg_try_advisory_lock: %v", err)
	}
	if !otherLocked {
		t.Fatal("another pod could not take the session's advisory lock after the failed hydration: the lock was leaked")
	}
	if err := otherConn.QueryRow(ctx, advisoryUnlockQuery, sessionID.String()).Scan(&otherUnlocked); err != nil || !otherUnlocked {
		t.Fatalf("another pod's pg_advisory_unlock = (%v, %v), want (true, nil)", otherUnlocked, err)
	}
	otherConn.Release()

	releaseHeld()
	if _, err := r.GetOrSpawn(ctx, sessionID); err != nil {
		t.Fatalf("GetOrSpawn once the pool has capacity again: %v", err)
	}
}

// TestLockHolder_ConcurrentGetOrSpawnSameSession (T3) proves one replica
// never runs two actors for one session, although every lock it takes
// lives on one backend, where advisory locks are re-entrant: sixteen
// concurrent GetOrSpawn calls get one *Actor and no error; the holder
// refuses a second lock on a session it already holds; and once the actor
// stops, no lock is left on the backend -- a stacked double lock would
// leave one. The actor is stopped by its idle TTL, not by Shutdown:
// Shutdown closes the lock connection, which would release a stacked lock
// too and prove nothing.
func TestLockHolder_ConcurrentGetOrSpawnSameSession(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	pool := newLockTestPool(ctx, t, connStr, 4)
	timeouts := lockTestTimeouts()
	timeouts.ActorIdleTTL = time.Second
	r := newLockTestRegistry(ctx, t, pool, timeouts)
	sessionID := createTestSession(ctx, t, admin)

	const callers = 16
	actors := make([]*Actor, callers)
	start := make(chan struct{})
	var g errgroup.Group
	for i := range callers {
		g.Go(func() error {
			<-start
			a, err := r.GetOrSpawn(ctx, sessionID)
			actors[i] = a
			return err
		})
	}
	close(start)
	if err := g.Wait(); err != nil {
		t.Fatalf("one of %d concurrent GetOrSpawn calls failed: %v", callers, err)
	}
	for i, a := range actors {
		if a == nil || a != actors[0] {
			t.Fatalf("caller %d got actor %p, caller 0 got %p: want one actor for every caller", i, a, actors[0])
		}
	}

	if _, ok, err := r.locks.TryLock(ctx, sessionID); ok || err != nil {
		t.Fatalf("a second TryLock on a session this replica holds = (ok %v, err %v), want (false, nil): the backend would have stacked it", ok, err)
	}

	lockPID := r.locks.backendPIDForTest()
	if got := advisoryLockHolders(ctx, t, admin, sessionID); len(got) != 1 || got[0] != lockPID {
		t.Fatalf("the session's advisory lock is held by pids %v, want only the lock backend %d", got, lockPID)
	}

	select {
	case <-actors[0].done:
	case <-time.After(timeouts.ActorIdleTTL + 5*time.Second):
		t.Fatal("the actor did not idle out")
	}
	waitUntil(t, 5*time.Second, func() bool {
		return len(advisoryLockHolders(ctx, t, admin, sessionID)) == 0
	})
	if got := r.locks.backendPIDForTest(); got != lockPID {
		t.Fatalf("the lock backend changed from %d to %d: the lock must be gone by unlock, not by the connection closing", lockPID, got)
	}
}

// TestLockHolder_LockConnLostStopsActors (T4) proves losing the lock
// connection stops every actor locked under it: once the backend is gone
// and a probe notices, every actor refuses commands, the loss is counted
// once, and the next GetOrSpawn hydrates afresh on a new backend.
func TestLockHolder_LockConnLostStopsActors(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	pool := newLockTestPool(ctx, t, connStr, 4)
	r := newLockTestRegistry(ctx, t, pool, lockTestTimeouts())

	sessions := make([]pgtype.UUID, 3)
	actors := make([]*Actor, len(sessions))
	for i := range sessions {
		sessions[i] = createTestSession(ctx, t, admin)
		a, err := r.GetOrSpawn(ctx, sessions[i])
		if err != nil {
			t.Fatalf("GetOrSpawn %d: %v", i+1, err)
		}
		actors[i] = a
	}
	oldPID := r.locks.backendPIDForTest()
	lostBefore := readCounterSum(ctx, t, otelReader, "session_actor_lock_conn_lost")
	liveBefore := readCounterSum(ctx, t, otelReader, "session_actors_live")

	terminateLockBackend(ctx, t, admin, oldPID)
	if err := r.ProbeLockOnce(ctx); err == nil {
		t.Fatal("ProbeLockOnce on a terminated lock backend = nil, want the loss reported")
	}

	for i, a := range actors {
		if err := a.Send(ctx, TimerFired{Name: "integration_test_timer"}); !errors.Is(err, ErrActorStopped) {
			t.Errorf("Send to actor %d after the lock connection was lost = %v, want ErrActorStopped", i+1, err)
		}
		if r.lookup(sessions[i]) != nil {
			t.Errorf("actor %d is still in the Registry's map after the lock connection was lost", i+1)
		}
	}
	if got := readCounterSum(ctx, t, otelReader, "session_actor_lock_conn_lost") - lostBefore; got != 1 {
		t.Errorf("session_actor_lock_conn_lost moved by %d, want 1", got)
	}
	if got := readCounterSum(ctx, t, otelReader, "session_actors_live") - liveBefore; got != -int64(len(sessions)) {
		t.Errorf("session_actors_live moved by %d, want %d", got, -len(sessions))
	}

	fresh, err := r.GetOrSpawn(ctx, sessions[0])
	if err != nil {
		t.Fatalf("GetOrSpawn after the lock connection was lost: %v", err)
	}
	if fresh == actors[0] {
		t.Fatal("GetOrSpawn handed out the stopped actor again")
	}
	newPID := r.locks.backendPIDForTest()
	if newPID == 0 || newPID == oldPID {
		t.Fatalf("lock backend after the loss = %d, want a new one (the lost one was %d)", newPID, oldPID)
	}
	if got := advisoryLockHolders(ctx, t, admin, sessions[0]); len(got) != 1 || got[0] != newPID {
		t.Errorf("the rehydrated session's advisory lock is held by pids %v, want only the new lock backend %d", got, newPID)
	}
}

// TestLockHolder_StaleGenerationUnlockIsNoop (T4b) proves a lock from a
// lost connection's generation can never release the same session's lock
// taken again under the new one: after a loss and a rehydration of the
// same session, the old actor's late unlock, run explicitly here, leaves
// the new lock held, so another pod still sees the session as owned. The
// old actor's own shutdown runs the same unlock, but only after its done
// channel closes, so the test cannot order it before the check and does
// not rely on it.
func TestLockHolder_StaleGenerationUnlockIsNoop(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	pool := newLockTestPool(ctx, t, connStr, 4)
	r := newLockTestRegistry(ctx, t, pool, lockTestTimeouts())
	sessionID := createTestSession(ctx, t, admin)

	old, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	oldGen := old.lockGen

	terminateLockBackend(ctx, t, admin, r.locks.backendPIDForTest())
	if err := r.ProbeLockOnce(ctx); err == nil {
		t.Fatal("ProbeLockOnce on a terminated lock backend = nil, want the loss reported")
	}
	fresh, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn after the loss: %v", err)
	}
	if fresh.lockGen == oldGen {
		t.Fatalf("the rehydrated actor's lock generation is still %d, want a new one", oldGen)
	}

	select {
	case <-old.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stopped actor's run loop did not exit")
	}
	r.locks.Unlock(ctx, sessionID, oldGen)

	newPID := r.locks.backendPIDForTest()
	if got := advisoryLockHolders(ctx, t, admin, sessionID); len(got) != 1 || got[0] != newPID {
		t.Fatalf("after the old generation's unlock, the session's advisory lock is held by pids %v, want still the new lock backend %d", got, newPID)
	}
	otherPod := newLockTestRegistry(ctx, t, newLockTestPool(ctx, t, connStr, 2), lockTestTimeouts())
	if _, err := otherPod.GetOrSpawn(ctx, sessionID); !errors.Is(err, ErrSessionActorElsewhere) {
		t.Fatalf("another pod's GetOrSpawn after the old generation's unlock = %v, want ErrSessionActorElsewhere", err)
	}
}
