//go:build integration

// Integration tests for how the lock connection (lockholder.go) recovers
// and fails: a connection that died silently, a first host that hangs, a
// server-side error, a caller that goes away, a hydration racing a loss,
// and the timer pump's answer to an unavailable replica. Same rules as
// lockholder_integration_test.go: each test builds its own small pools
// from the shared container, and terminates no backend but its own --
// matched by pid or by its own unique application_name, AND this test's
// own database.
package sessionactor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// recoveryTestTimerName is a timer name outside the named ones, so its
// delivery is a harmless no-op (timerpump_integration_test.go's reasoning).
const recoveryTestTimerName = "integration_test_timer"

// setTryLockSQLForTest replaces TryLock's statement, under sem.
func (h *lockHolder) setTryLockSQLForTest(query string) {
	h.sem <- struct{}{}
	defer h.leave()
	h.tryLockSQL = query
}

// withConnParams returns connStr with each of params set on its query.
func withConnParams(t *testing.T, connStr string, params map[string]string) string {
	t.Helper()
	u, err := url.Parse(connStr)
	if err != nil {
		t.Fatalf("parse the test connection string: %v", err)
	}
	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// uniqueApplicationName names one test's own pool backends, so the test can
// terminate them and nothing else.
func uniqueApplicationName() string {
	return fmt.Sprintf("narvi-locktest-%d", time.Now().UnixNano())
}

// terminateApplicationBackends terminates every backend on this test's own
// database reporting appName -- a name only this test's own pool uses --
// and returns once they are all gone.
func terminateApplicationBackends(ctx context.Context, t *testing.T, admin *pgxpool.Pool, appName string) {
	t.Helper()
	if strings.HasPrefix(appName, lockConnApplicationNamePrefix) {
		t.Fatalf("refusing to terminate every %s backend by name: terminate the lock backend by pid", appName)
	}
	rows, err := admin.Query(ctx, `
		SELECT pg_terminate_backend(pid, 5000) FROM pg_stat_activity
		WHERE application_name = $1 AND datname = current_database()`, appName)
	if err != nil {
		t.Fatalf("terminate %s backends: %v", appName, err)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("terminate %s backends: %v", appName, err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		var n int
		if err := admin.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity WHERE application_name = $1 AND datname = current_database()`,
			appName).Scan(&n); err != nil {
			t.Fatalf("recount %s backends: %v", appName, err)
		}
		return n == 0
	})
}

// seedDueTimer arms a timer on sessionID that is already due.
func seedDueTimer(ctx context.Context, t *testing.T, admin *pgxpool.Pool, sessionID pgtype.UUID) {
	t.Helper()
	if _, err := narvipg.NewTimerStore(admin).Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: sessionID,
		Name:      recoveryTestTimerName,
		FiresAt:   pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true},
	}); err != nil {
		t.Fatalf("seed a due timer: %v", err)
	}
}

// holdPoolConns takes every connection of pool until the returned release
// runs (also registered as a cleanup).
func holdPoolConns(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (release func()) {
	t.Helper()
	var held []*pgxpool.Conn
	for range int(pool.Config().MaxConns) {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("hold a pool connection: %v", err)
		}
		held = append(held, c)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			for _, c := range held {
				c.Release()
			}
		})
	}
	t.Cleanup(release)
	return release
}

// TestLockHolder_SilentLossRedialsOnFirstSpawn (T8) proves the first
// hydration after the lock connection died silently -- a Postgres restart
// or failover, a terminated backend -- succeeds instead of failing with
// ErrActorUnavailable on a healthy database: the first TryLock finds the
// connection it was handed dead, declares the loss, and retries once on a
// new one within the same bound. Run at the shipped timeouts (hydration
// 2 s, statements 1 s, probe 10 s -- never started here, so nothing but
// the spawn itself can find the loss), with the pool's own backends
// terminated too in half the cases, and the spawn made directly or by a
// timer-pump tick with five due timers. The loss keeps its meaning: the
// actor locked on the dead connection is stopped and the loss counted
// once. Without the retry, the direct spawn fails and the tick delivers 0
// of 5.
func TestLockHolder_SilentLossRedialsOnFirstSpawn(t *testing.T) {
	for _, tc := range []struct {
		name       string
		killPool   bool
		viaPump    bool
		wantSpawns int
	}{
		{"lock backend terminated, first GetOrSpawn", false, false, 1},
		{"lock and pool backends terminated, first GetOrSpawn", true, false, 1},
		{"lock backend terminated, first PumpOnce", false, true, 5},
		{"lock and pool backends terminated, first PumpOnce", true, true, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			admin, connStr := IntegrationTestPoolAndConnStr(t)
			appName := uniqueApplicationName()
			pool := newLockTestPool(ctx, t, withConnParams(t, connStr, map[string]string{"application_name": appName}), 4)
			r := newLockTestRegistry(ctx, t, pool, lockTestShippedTimeouts(t))

			live := createTestSession(ctx, t, admin)
			liveActor, err := r.GetOrSpawn(ctx, live)
			if err != nil {
				t.Fatalf("GetOrSpawn for the actor locked before the loss: %v", err)
			}
			targets := make([]pgtype.UUID, tc.wantSpawns)
			for i := range targets {
				targets[i] = createTestSession(ctx, t, admin)
				if tc.viaPump {
					seedDueTimer(ctx, t, admin, targets[i])
				}
			}
			oldPID := r.locks.backendPIDForTest()
			lostBefore := readCounterSum(ctx, t, otelReader, "session_actor_lock_conn_lost")

			terminateLockBackend(ctx, t, admin, oldPID)
			if tc.killPool {
				terminateApplicationBackends(ctx, t, admin, appName)
			}
			// Past pgxpool's one-second idle ping, as after a real restart:
			// the pool finds its own dead connections itself.
			time.Sleep(1500 * time.Millisecond)

			if tc.viaPump {
				if err := r.PumpOnce(ctx); err != nil {
					t.Fatalf("PumpOnce after the silent loss: %v", err)
				}
				delivered := 0
				for _, id := range targets {
					if r.lookup(id) != nil {
						delivered++
					}
				}
				if delivered != len(targets) {
					t.Fatalf("PumpOnce after the silent loss delivered %d of %d due timers, want all of them", delivered, len(targets))
				}
			} else if _, err := r.GetOrSpawn(ctx, targets[0]); err != nil {
				t.Fatalf("the first GetOrSpawn after the silent loss = %v, want a fresh actor on a new lock connection", err)
			}

			if err := liveActor.Send(ctx, TimerFired{Name: recoveryTestTimerName}); !errors.Is(err, ErrActorStopped) {
				t.Errorf("Send to the actor locked on the dead connection = %v, want ErrActorStopped", err)
			}
			if got := readCounterSum(ctx, t, otelReader, "session_actor_lock_conn_lost") - lostBefore; got != 1 {
				t.Errorf("session_actor_lock_conn_lost moved by %d, want 1", got)
			}
			newPID := r.locks.backendPIDForTest()
			if newPID == 0 || newPID == oldPID {
				t.Fatalf("lock backend after the loss = %d, want a new one (the dead one was %d)", newPID, oldPID)
			}
			for i, id := range targets {
				if got := advisoryLockHolders(ctx, t, admin, id); len(got) != 1 || got[0] != newPID {
					t.Errorf("session %d's advisory lock is held by pids %v, want only the new lock backend %d", i+1, got, newPID)
				}
			}
		})
	}
}

// lockTestShippedTimeouts is the shipped timeouts, checked valid.
func lockTestShippedTimeouts(t *testing.T) (to platform.Timeouts) {
	t.Helper()
	to = platform.DefaultTimeouts()
	if err := to.Validate(); err != nil {
		t.Fatalf("the shipped timeouts do not validate: %v", err)
	}
	return to
}

// startSilentListener listens on a local port that accepts every
// connection and never answers -- a host that hangs, as a dead primary can
// in a failover -- until the test ends.
func startSilentListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var (
		mu       sync.Mutex
		accepted []net.Conn
		g        errgroup.Group
	)
	g.Go(func() error {
		for {
			c, err := ln.Accept()
			if err != nil {
				return nil
			}
			mu.Lock()
			accepted = append(accepted, c)
			mu.Unlock()
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		_ = g.Wait()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range accepted {
			_ = c.Close()
		}
	})
	return ln.Addr().String()
}

// withHungFirstHost returns connStr as a multi-host URL whose first host is
// hungAddr, with connect_timeout set: the failover form pgx follows host by
// host, each under its own connect_timeout.
func withHungFirstHost(t *testing.T, connStr, hungAddr string, connectTimeoutSeconds int) string {
	t.Helper()
	u, err := url.Parse(withConnParams(t, connStr, map[string]string{"connect_timeout": fmt.Sprint(connectTimeoutSeconds)}))
	if err != nil {
		t.Fatalf("parse the test connection string: %v", err)
	}
	return fmt.Sprintf("%s://%s@%s,%s%s?%s", u.Scheme, u.User.String(), hungAddr, u.Host, u.Path, u.RawQuery)
}

// TestLockHolder_DialsLikeThePoolPastAHungFirstHost (T9) proves the lock
// connection is dialled with the query pool's own connect rules: behind a
// multi-host URL whose first host accepts and never answers, the pool gets
// through to the second host once that host's connect_timeout (2 s) runs
// out, and so must the lock connection -- it used to be bounded by one
// 1 s-at-most statement bound across every host, so it never got past the
// first. Hydration is bounded tighter than one host's connect_timeout here
// (300 ms against 2 s): the first hydration asks for the dial, fails fast
// with ErrActorUnavailable, and the dial goes on without it; once it lands,
// hydration succeeds. The probe loop, started on its own with an interval
// far longer than the wait, dials at once, before any hydration asks.
func TestLockHolder_DialsLikeThePoolPastAHungFirstHost(t *testing.T) {
	const connectTimeoutSeconds = 2
	for _, tc := range []struct {
		name      string
		withProbe bool
	}{
		{"a hydration asks for the dial", false},
		{"the probe loop dials before any hydration", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			admin, connStr := IntegrationTestPoolAndConnStr(t)
			dsn := withHungFirstHost(t, connStr, startSilentListener(t), connectTimeoutSeconds)
			pool := newLockTestPool(ctx, t, dsn, 2)

			// The pool's own first connection passes the hung host first too.
			warmStarted := time.Now()
			warmCtx, cancelWarm := context.WithTimeout(ctx, 20*time.Second)
			var one int
			err := pool.QueryRow(warmCtx, `SELECT 1`).Scan(&one)
			cancelWarm()
			if err != nil {
				t.Fatalf("the pool's first query through the multi-host URL: %v", err)
			}
			if took := time.Since(warmStarted); took < connectTimeoutSeconds*time.Second {
				t.Fatalf("the pool's first connection took %v, want at least the hung host's connect_timeout (%ds): the first host is not hanging", took, connectTimeoutSeconds)
			}

			timeouts := lockTestTimeouts()
			if tc.withProbe {
				// No tick falls inside the wait below: only the dial the
				// loop starts at once can bring the connection up in time.
				timeouts.ActorLockProbeInterval = time.Minute
			}
			r := newLockTestRegistry(ctx, t, pool, timeouts)
			sessionID := createTestSession(ctx, t, admin)

			if tc.withProbe {
				probeCtx, stopProbe := context.WithCancel(ctx)
				var probe errgroup.Group
				probe.Go(func() error { return r.RunLockProbe(probeCtx) })
				t.Cleanup(func() {
					stopProbe()
					_ = probe.Wait()
				})
			} else {
				started := time.Now()
				_, err := r.GetOrSpawn(ctx, sessionID)
				elapsed := time.Since(started)
				if !errors.Is(err, ErrActorUnavailable) {
					t.Fatalf("the first GetOrSpawn while the lock connection is still being dialled = %v, want ErrActorUnavailable", err)
				}
				if limit := timeouts.ActorHydrateTimeout + 500*time.Millisecond; elapsed > limit {
					t.Fatalf("the first GetOrSpawn took %v, want at most %v: it must fail within its own bound while the dial goes on", elapsed, limit)
				}
			}

			waitUntil(t, 4*connectTimeoutSeconds*time.Second, func() bool {
				return r.locks.backendPIDForTest() != 0
			})
			lockPID := r.locks.backendPIDForTest()
			var appName string
			if err := admin.QueryRow(ctx, `SELECT application_name FROM pg_stat_activity WHERE pid = $1 AND datname = current_database()`,
				int32(lockPID)).Scan(&appName); err != nil || appName != r.locks.appNameForTest() || !isLockConnApplicationName(appName) {
				t.Fatalf("the lock backend %d on the real host: application_name = %q, err = %v; want the dial's own %q", lockPID, appName, err, r.locks.appNameForTest())
			}

			if _, err := r.GetOrSpawn(ctx, sessionID); err != nil {
				t.Fatalf("GetOrSpawn once the lock connection is up: %v", err)
			}
			if got := advisoryLockHolders(ctx, t, admin, sessionID); len(got) != 1 || got[0] != lockPID {
				t.Fatalf("the session's advisory lock is held by pids %v, want only the lock backend %d", got, lockPID)
			}
		})
	}
}

// injectedLockThenErrorQuery takes the session's lock, then fails with a
// server-side ERROR (division by zero, 22012) on a live connection. random()
// keeps the planner from folding the division away before the lock is
// taken, and CASE orders the two.
const injectedLockThenErrorQuery = `SELECT CASE WHEN pg_try_advisory_lock(hashtextextended($1::text, 0))
	THEN (1 / (random() * 0)::int) = 1 ELSE false END`

// TestLockHolder_ServerErrorFailsOneHydrationOnly (T10) proves a
// server-side ERROR on the shared lock connection -- the shared lock table
// full, in production -- fails that one hydration and nothing else: the
// connection is kept, no loss is counted, every other actor keeps running
// with its lock held, and the lock the failing statement took before it
// failed is released. Treating every error as a lost connection closed a
// healthy connection, released every lock on the replica and stopped every
// actor; not releasing leaves the failed session locked by this replica.
func TestLockHolder_ServerErrorFailsOneHydrationOnly(t *testing.T) {
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
	failing := createTestSession(ctx, t, admin)
	lockPID := r.locks.backendPIDForTest()
	lostBefore := readCounterSum(ctx, t, otelReader, "session_actor_lock_conn_lost")

	r.locks.setTryLockSQLForTest(injectedLockThenErrorQuery)
	_, err := r.GetOrSpawn(ctx, failing)
	r.locks.setTryLockSQLForTest(tryAdvisoryLockQuery)
	var pgErr *pgconn.PgError
	if !errors.Is(err, ErrActorUnavailable) || !errors.As(err, &pgErr) || pgErr.Code != "22012" {
		t.Fatalf("GetOrSpawn with a server-side ERROR injected = %v, want ErrActorUnavailable wrapping SQLSTATE 22012", err)
	}

	if got := readCounterSum(ctx, t, otelReader, "session_actor_lock_conn_lost") - lostBefore; got != 0 {
		t.Errorf("session_actor_lock_conn_lost moved by %d, want 0: a server-side ERROR is not a lost connection", got)
	}
	if got := r.locks.backendPIDForTest(); got != lockPID {
		t.Fatalf("the lock backend changed from %d to %d: the connection was closed for a server-side ERROR", lockPID, got)
	}
	for i, a := range actors {
		if err := a.Send(ctx, TimerFired{Name: recoveryTestTimerName}); err != nil {
			t.Errorf("Send to actor %d after another session's server-side ERROR = %v, want nil", i+1, err)
		}
		if r.lookup(sessions[i]) != a {
			t.Errorf("actor %d left the Registry's map after another session's server-side ERROR", i+1)
		}
		if got := advisoryLockHolders(ctx, t, admin, sessions[i]); len(got) != 1 || got[0] != lockPID {
			t.Errorf("actor %d's advisory lock is held by pids %v, want still the lock backend %d", i+1, got, lockPID)
		}
	}
	if got := advisoryLockHolders(ctx, t, admin, failing); len(got) != 0 {
		t.Fatalf("the failed session's advisory lock is held by pids %v, want none: the lock the failing statement took was not released", got)
	}

	if _, err := r.GetOrSpawn(ctx, failing); err != nil {
		t.Fatalf("GetOrSpawn once the error is gone: %v", err)
	}
	if got := advisoryLockHolders(ctx, t, admin, failing); len(got) != 1 || got[0] != lockPID {
		t.Fatalf("the session's advisory lock is held by pids %v, want only the same lock backend %d", got, lockPID)
	}
}

// TestLockHolder_CallerCancellationNeverReachesTheLockConnection (T11)
// proves no statement on the shared lock connection runs under a caller's
// context: a caller cancelled mid-hydration -- its lock taken, the
// hydration waiting on a saturated pool -- leaves the hydration to run to
// its own bound, and the failed hydration's unlock, which runs after that
// bound has expired, still releases the lock on the live connection. The
// connection survives: no loss is counted, the backend is the same, and an
// unrelated live actor keeps its lock and takes commands. An unlock run
// under a done context fails on the spot, and so closes the connection and
// stops every actor on the replica.
func TestLockHolder_CallerCancellationNeverReachesTheLockConnection(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	pool := newLockTestPool(ctx, t, connStr, 2)
	timeouts := lockTestTimeouts()
	timeouts.ActorHydrateTimeout = time.Second
	r := newLockTestRegistry(ctx, t, pool, timeouts)

	live := createTestSession(ctx, t, admin)
	liveActor, err := r.GetOrSpawn(ctx, live)
	if err != nil {
		t.Fatalf("GetOrSpawn for the live actor: %v", err)
	}
	cancelled := createTestSession(ctx, t, admin)
	lockPID := r.locks.backendPIDForTest()
	lostBefore := readCounterSum(ctx, t, otelReader, "session_actor_lock_conn_lost")

	holdPoolConns(ctx, t, pool)
	callerCtx, cancelCaller := context.WithCancel(ctx)
	defer cancelCaller()
	var spawnErr error
	var spawning errgroup.Group
	spawning.Go(func() error {
		_, spawnErr = r.GetOrSpawn(callerCtx, cancelled)
		return nil
	})
	waitUntil(t, 5*time.Second, func() bool {
		got := advisoryLockHolders(ctx, t, admin, cancelled)
		return len(got) == 1 && got[0] == lockPID
	})
	cancelCaller()
	_ = spawning.Wait()

	if !errors.Is(spawnErr, ErrActorUnavailable) || !errors.Is(spawnErr, errHydrateBoundExpired) {
		t.Fatalf("GetOrSpawn with its caller cancelled mid-hydration = %v, want ErrActorUnavailable at the hydration's own bound", spawnErr)
	}
	if got := readCounterSum(ctx, t, otelReader, "session_actor_lock_conn_lost") - lostBefore; got != 0 {
		t.Errorf("session_actor_lock_conn_lost moved by %d, want 0", got)
	}
	if got := r.locks.backendPIDForTest(); got != lockPID {
		t.Fatalf("the lock backend changed from %d to %d: a statement ran under a done context and broke the connection", lockPID, got)
	}
	if got := advisoryLockHolders(ctx, t, admin, cancelled); len(got) != 0 {
		t.Errorf("the failed hydration's advisory lock is held by pids %v, want none", got)
	}
	if got := advisoryLockHolders(ctx, t, admin, live); len(got) != 1 || got[0] != lockPID {
		t.Errorf("the live actor's advisory lock is held by pids %v, want still the lock backend %d", got, lockPID)
	}
	if err := liveActor.Send(ctx, TimerFired{Name: recoveryTestTimerName}); err != nil {
		t.Fatalf("Send to the unrelated live actor = %v, want nil", err)
	}
}

// slowLockQuery takes the session's lock after half a second: a lock
// statement still running when its hydration's bound expires.
const slowLockQuery = `SELECT pg_try_advisory_lock(hashtextextended($1::text, 0)) FROM pg_sleep(0.5)`

// TestLockHolder_LockStatementOutlivesTheHydrationBound (T15) proves a lock
// statement already running when its hydration's bound expires runs to its
// own ActorLockStatementTimeout instead of being cut short with the
// hydration (ActorHydrateTimeout's own doc comment): cutting a statement
// short breaks the shared connection and drops every other actor's lock.
// The hydration waits 700 ms for the lock connection, then its 500 ms lock
// statement ends past the 1 s bound; the hydration fails with
// ErrActorUnavailable at its bound and releases the lock it took, and the
// connection, the unrelated live actor and its lock all survive. A
// statement run under the hydration's context is cancelled at the bound,
// which closes the connection.
func TestLockHolder_LockStatementOutlivesTheHydrationBound(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	pool := newLockTestPool(ctx, t, connStr, 4)
	timeouts := lockTestTimeouts()
	timeouts.ActorHydrateTimeout = time.Second
	timeouts.ActorLockStatementTimeout = 900 * time.Millisecond
	timeouts.ActorLockProbeInterval = 2 * time.Second
	if err := timeouts.Validate(); err != nil {
		t.Fatalf("test timeouts: %v", err)
	}
	r := newLockTestRegistry(ctx, t, pool, timeouts)

	live := createTestSession(ctx, t, admin)
	liveActor, err := r.GetOrSpawn(ctx, live)
	if err != nil {
		t.Fatalf("GetOrSpawn for the live actor: %v", err)
	}
	slow := createTestSession(ctx, t, admin)
	lockPID := r.locks.backendPIDForTest()
	lostBefore := readCounterSum(ctx, t, otelReader, "session_actor_lock_conn_lost")
	r.locks.setTryLockSQLForTest(slowLockQuery)
	t.Cleanup(func() { r.locks.setTryLockSQLForTest(tryAdvisoryLockQuery) })

	r.locks.sem <- struct{}{}
	var spawnErr error
	var spawning errgroup.Group
	spawning.Go(func() error {
		_, spawnErr = r.GetOrSpawn(ctx, slow)
		return nil
	})
	time.Sleep(700 * time.Millisecond)
	r.locks.leave()
	_ = spawning.Wait()

	if !errors.Is(spawnErr, ErrActorUnavailable) || !errors.Is(spawnErr, errHydrateBoundExpired) {
		t.Fatalf("GetOrSpawn whose lock statement outran the bound = %v, want ErrActorUnavailable at the hydration's bound", spawnErr)
	}
	if got := readCounterSum(ctx, t, otelReader, "session_actor_lock_conn_lost") - lostBefore; got != 0 {
		t.Errorf("session_actor_lock_conn_lost moved by %d, want 0: the lock statement was cut short at the hydration's bound", got)
	}
	if got := r.locks.backendPIDForTest(); got != lockPID {
		t.Fatalf("the lock backend changed from %d to %d: the lock statement was cut short and broke the connection", lockPID, got)
	}
	if got := advisoryLockHolders(ctx, t, admin, slow); len(got) != 0 {
		t.Errorf("the failed hydration's advisory lock is held by pids %v, want none", got)
	}
	if got := advisoryLockHolders(ctx, t, admin, live); len(got) != 1 || got[0] != lockPID {
		t.Errorf("the live actor's advisory lock is held by pids %v, want still the lock backend %d", got, lockPID)
	}
	if err := liveActor.Send(ctx, TimerFired{Name: recoveryTestTimerName}); err != nil {
		t.Fatalf("Send to the unrelated live actor = %v, want nil", err)
	}
}

// TestLockHolder_JoinedCallerOutlivesTheLeadersCancellation (T12) proves a
// hydration that other callers joined does not die with the caller that
// started it (hydrationContext): the leader is cancelled mid-hydration, a
// second caller that joined the same flight still gets the actor once the
// pool frees, and so does the leader -- one hydration, one actor. A
// hydration on its leader's cancellation fails every caller that joined.
func TestLockHolder_JoinedCallerOutlivesTheLeadersCancellation(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	pool := newLockTestPool(ctx, t, connStr, 2)
	timeouts := lockTestTimeouts()
	timeouts.ActorHydrateTimeout = 3 * time.Second
	r := newLockTestRegistry(ctx, t, pool, timeouts)
	sessionID := createTestSession(ctx, t, admin)
	lockPIDBefore := func() uint32 { return r.locks.backendPIDForTest() }

	releasePool := holdPoolConns(ctx, t, pool)
	leaderCtx, cancelLeader := context.WithCancel(ctx)
	defer cancelLeader()
	var (
		leader, joiner       *Actor
		leaderErr, joinerErr error
		callers              errgroup.Group
	)
	callers.Go(func() error {
		leader, leaderErr = r.GetOrSpawn(leaderCtx, sessionID)
		return nil
	})
	waitUntil(t, 5*time.Second, func() bool {
		pid := lockPIDBefore()
		got := advisoryLockHolders(ctx, t, admin, sessionID)
		return pid != 0 && len(got) == 1 && got[0] == pid
	})
	callers.Go(func() error {
		joiner, joinerErr = r.GetOrSpawn(ctx, sessionID)
		return nil
	})
	// Long enough for the second caller to join the flight in progress,
	// far short of the hydration's own bound.
	time.Sleep(200 * time.Millisecond)
	cancelLeader()
	time.Sleep(100 * time.Millisecond)
	releasePool()
	_ = callers.Wait()

	if joinerErr != nil {
		t.Fatalf("the joined caller's GetOrSpawn after the leader was cancelled = %v, want the actor", joinerErr)
	}
	if leaderErr != nil {
		t.Fatalf("the cancelled leader's GetOrSpawn = %v, want the actor its hydration still produced", leaderErr)
	}
	if leader == nil || leader != joiner {
		t.Fatalf("the leader got actor %p and the joined caller %p: want one hydration, one actor", leader, joiner)
	}
}

// TestLockHolder_HydrationRacingALossIsRefusedAtInsert (T13) proves the
// insert-time generation check (Registry.start): a hydration that took its
// lock, then lost the connection holding it before inserting its actor --
// the loss's onLockLost cannot find it, it is not in the map yet -- is
// refused at insert with ErrActorUnavailable, leaves no actor behind and no
// lock held, and another pod can then take the session. Without the check
// it is inserted and runs on a lock nobody holds, while another pod runs a
// second actor for the same session.
func TestLockHolder_HydrationRacingALossIsRefusedAtInsert(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	pool := newLockTestPool(ctx, t, connStr, 2)
	timeouts := lockTestTimeouts()
	timeouts.ActorHydrateTimeout = 5 * time.Second
	r := newLockTestRegistry(ctx, t, pool, timeouts)
	sessionID := createTestSession(ctx, t, admin)

	releasePool := holdPoolConns(ctx, t, pool)
	var (
		spawned  *Actor
		spawnErr error
		spawning errgroup.Group
	)
	spawning.Go(func() error {
		spawned, spawnErr = r.GetOrSpawn(ctx, sessionID)
		return nil
	})
	var lockPID uint32
	waitUntil(t, 5*time.Second, func() bool {
		lockPID = r.locks.backendPIDForTest()
		got := advisoryLockHolders(ctx, t, admin, sessionID)
		return lockPID != 0 && len(got) == 1 && got[0] == lockPID
	})
	genBefore := r.locks.currentGen()

	terminateLockBackend(ctx, t, admin, lockPID)
	if err := r.ProbeLockOnce(ctx); err == nil {
		t.Fatal("ProbeLockOnce on a terminated lock backend = nil, want the loss reported")
	}
	if got := r.locks.currentGen(); got == genBefore {
		t.Fatalf("the lock generation is still %d after the loss", got)
	}
	releasePool()
	_ = spawning.Wait()

	if !errors.Is(spawnErr, ErrActorUnavailable) || !errors.Is(spawnErr, errLockLost) {
		t.Fatalf("a hydration that lost its lock connection before inserting = (%p, %v), want ErrActorUnavailable wrapping errLockLost", spawned, spawnErr)
	}
	if got := r.lookup(sessionID); got != nil {
		t.Fatalf("the refused actor is in the Registry's map (lock generation %d, current %d)", got.lockGen, r.locks.currentGen())
	}
	if got := advisoryLockHolders(ctx, t, admin, sessionID); len(got) != 0 {
		t.Fatalf("the session's advisory lock is held by pids %v, want none", got)
	}
	otherPod := newLockTestRegistry(ctx, t, newLockTestPool(ctx, t, connStr, 2), lockTestTimeouts())
	if _, err := otherPod.GetOrSpawn(ctx, sessionID); err != nil {
		t.Fatalf("another pod's GetOrSpawn after the refusal = %v, want the session", err)
	}
}

// TestTimerPump_UnavailableEndsTheBatch (T14) runs the timer pump's real
// answer to an unavailable replica end to end: five due timers, every
// hydration waiting out ActorHydrateTimeout for the lock connection (its
// mutex held for the tick). The tick claims all five, tries ONE hydration,
// which fails with ErrActorUnavailable, and ends the batch there -- one
// bound, not five -- leaving the five claimed, to come back when their
// claims expire. A delivery that swallows the error waits out the bound
// once per timer.
func TestTimerPump_UnavailableEndsTheBatch(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	pool := newLockTestPool(ctx, t, connStr, 4)
	timeouts := lockTestTimeouts()
	r := newLockTestRegistry(ctx, t, pool, timeouts)

	sessions := make([]pgtype.UUID, 5)
	for i := range sessions {
		sessions[i] = createTestSession(ctx, t, admin)
		seedDueTimer(ctx, t, admin, sessions[i])
	}
	unavailableBefore := readCounterSumByAttr(ctx, t, otelReader, "session_actor_hydrations", "outcome", "unavailable")

	r.locks.sem <- struct{}{}
	tickStarted := time.Now()
	err := r.PumpOnce(ctx)
	elapsed := time.Since(tickStarted)
	r.locks.leave()
	if err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	if elapsed < timeouts.ActorHydrateTimeout {
		t.Fatalf("PumpOnce took %v, less than one hydration bound (%v): no delivery waited", elapsed, timeouts.ActorHydrateTimeout)
	}
	if limit := 2 * timeouts.ActorHydrateTimeout; elapsed >= limit {
		t.Fatalf("PumpOnce took %v, want under %v: the batch went on past the first unavailable delivery", elapsed, limit)
	}
	if got := readCounterSumByAttr(ctx, t, otelReader, "session_actor_hydrations", "outcome", "unavailable") - unavailableBefore; got != 1 {
		t.Errorf("session_actor_hydrations{outcome=unavailable} moved by %d, want 1: only the first delivery may try", got)
	}
	timerStore := narvipg.NewTimerStore(admin)
	for i, id := range sessions {
		if r.lookup(id) != nil {
			t.Errorf("session %d has a live actor: its timer was delivered", i+1)
		}
		timer, err := timerStore.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: id, Name: recoveryTestTimerName})
		if err != nil {
			t.Fatalf("get session %d's timer: %v", i+1, err)
		}
		if !timer.FiresAt.Time.After(tickStarted) {
			t.Errorf("session %d's timer fires at %v, want claimed past the tick (%v)", i+1, timer.FiresAt.Time, tickStarted)
		}
	}
}
