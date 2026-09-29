//go:build integration

// Integration tests pinning how the lock connection (lockholder.go) is
// retried and dialled again: TryLock's one retry, the connect-timeout
// fallback, the dial that follows a loss, and the probe's dial when no
// connection is open. Same rules as lockholder_integration_test.go.
package sessionactor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"
)

// selfTerminatingLockQuery ends its own backend instead of taking a lock:
// every lock statement fails FATAL (57P01), so every one loses its
// connection.
const selfTerminatingLockQuery = `SELECT pg_terminate_backend(pg_backend_pid()) AND $1::text <> ''`

// TestLockHolder_TryLockRetriesALossOnlyOnce (T19) pins both halves of
// TryLock's retry rule with every lock statement losing its connection: a
// loss on a connection that was already open when the call began is
// retried exactly once, on a new one, and a loss on a connection dialled
// for the call is not retried at all. So one GetOrSpawn loses one
// connection when none was open, two when one was, and fails with
// ErrActorUnavailable at once, long before its bound. A retry on the fresh
// connection loses one more; retrying until the bound loses one per round
// trip -- well over a hundred in 2 s, each stopping every actor another
// hydration locked in between.
func TestLockHolder_TryLockRetriesALossOnlyOnce(t *testing.T) {
	for _, tc := range []struct {
		name       string
		openBefore bool
		wantLosses int64
	}{
		{"no connection open when the call begins: the loss on the one dialled for it is final", false, 1},
		{"a connection open when the call begins: one retry, on a new one, and no more", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			admin, connStr := IntegrationTestPoolAndConnStr(t)
			timeouts := lockTestShippedTimeouts(t)
			r := newLockTestRegistry(ctx, t, newLockTestPool(ctx, t, connStr, 2), timeouts)
			if tc.openBefore {
				r.locks.startDial()
				waitUntil(t, 5*time.Second, func() bool { return r.locks.backendPIDForTest() != 0 })
			}
			sessionID := createTestSession(ctx, t, admin)
			lostBefore := readCounterSum(ctx, t, otelReader, "session_actor_lock_conn_lost")

			r.locks.setTryLockSQLForTest(selfTerminatingLockQuery)
			t.Cleanup(func() { r.locks.setTryLockSQLForTest(tryAdvisoryLockQuery) })
			started := time.Now()
			_, err := r.GetOrSpawn(ctx, sessionID)
			elapsed := time.Since(started)

			if !errors.Is(err, ErrActorUnavailable) || errors.Is(err, errHydrateBoundExpired) {
				t.Fatalf("GetOrSpawn with every lock statement losing its connection = %v, want ErrActorUnavailable before the hydration's bound", err)
			}
			if got := readCounterSum(ctx, t, otelReader, "session_actor_lock_conn_lost") - lostBefore; got != tc.wantLosses {
				t.Fatalf("session_actor_lock_conn_lost moved by %d, want %d", got, tc.wantLosses)
			}
			if limit := timeouts.ActorHydrateTimeout / 2; elapsed >= limit {
				t.Fatalf("GetOrSpawn took %v, want well under the %v bound: it retried past the rule", elapsed, timeouts.ActorHydrateTimeout)
			}
		})
	}
}

// withoutConnectTimeout returns connStr with no connect_timeout, whether
// or not it had one.
func withoutConnectTimeout(t *testing.T, connStr string) string {
	t.Helper()
	u, err := url.Parse(connStr)
	if err != nil {
		t.Fatalf("parse the test connection string: %v", err)
	}
	q := u.Query()
	q.Del("connect_timeout")
	u.RawQuery = q.Encode()
	return u.String()
}

// TestLockHolder_DialFallsBackToAConnectTimeout (T20) proves the lock
// connection's dial bounds each host by ActorLockConnectTimeoutFallback
// when the database URL sets no connect_timeout, as pgxpool bounds its own:
// behind a multi-host URL with none whose first host accepts and never
// answers, the dial gives up on that host after the fallback (2 s here) and
// lands on the second. Without the fallback pgx waits on the first host for
// as long as the dial's own context lives -- until shutdown -- and every
// hydration on the replica fails with ErrActorUnavailable meanwhile. T9
// always sets connect_timeout, so it never reaches the fallback.
func TestLockHolder_DialFallsBackToAConnectTimeout(t *testing.T) {
	ctx := context.Background()
	_, connStr := IntegrationTestPoolAndConnStr(t)
	u, err := url.Parse(withoutConnectTimeout(t, connStr))
	if err != nil {
		t.Fatalf("parse the test connection string: %v", err)
	}
	dsn := fmt.Sprintf("%s://%s@%s,%s%s?%s", u.Scheme, u.User.String(), startSilentListener(t), u.Host, u.Path, u.RawQuery)
	// The pool is never used: pgxpool would wait out its own 2-minute
	// fallback on the first host.
	pool := newLockTestPool(ctx, t, dsn, 2)
	if got := pool.Config().ConnConfig.ConnectTimeout; got != 0 {
		t.Fatalf("the test URL sets a connect timeout of %v, want none", got)
	}

	timeouts := lockTestTimeouts()
	timeouts.ActorLockConnectTimeoutFallback = 2 * time.Second
	r := newLockTestRegistry(ctx, t, pool, timeouts)
	started := time.Now()
	r.locks.startDial()
	waitUntil(t, 5*timeouts.ActorLockConnectTimeoutFallback, func() bool { return r.locks.backendPIDForTest() != 0 })
	if took := time.Since(started); took < timeouts.ActorLockConnectTimeoutFallback {
		t.Fatalf("the lock connection landed after %v, less than the fallback (%v): the first host is not hanging", took, timeouts.ActorLockConnectTimeoutFallback)
	}
}

// TestLockHolder_ALossIsRedialledAtOnce (T21) proves a lost lock
// connection is dialled again as soon as the loss is found, whoever finds
// it: here a probe, with no probe loop running and nothing else asking for
// a connection -- no hydration, no tick -- and a new connection still
// comes up at once. Without it, a loss found by a probe or an unlock left
// the replica with no lock connection until the next hydration or tick
// asked for one.
func TestLockHolder_ALossIsRedialledAtOnce(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	r := newLockTestRegistry(ctx, t, newLockTestPool(ctx, t, connStr, 2), lockTestTimeouts())
	if _, err := r.GetOrSpawn(ctx, createTestSession(ctx, t, admin)); err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	oldPID := r.locks.backendPIDForTest()

	terminateLockBackend(ctx, t, admin, oldPID)
	if err := r.ProbeLockOnce(ctx); err == nil {
		t.Fatal("ProbeLockOnce on a terminated lock backend = nil, want the loss reported")
	}
	waitUntil(t, 5*time.Second, func() bool {
		pid := r.locks.backendPIDForTest()
		return pid != 0 && pid != oldPID
	})
}

// TestLockHolder_ProbeDialsWhenNoConnectionIsOpen (T22) proves a probe that
// finds no lock connection open starts a dial: one ProbeLockOnce on a
// Registry that has dialled nothing yet, with nothing else asking for a
// connection, brings one up. Without it, a dial that failed was retried only
// when a hydration next asked for one.
func TestLockHolder_ProbeDialsWhenNoConnectionIsOpen(t *testing.T) {
	ctx := context.Background()
	_, connStr := IntegrationTestPoolAndConnStr(t)
	r := newLockTestRegistry(ctx, t, newLockTestPool(ctx, t, connStr, 2), lockTestTimeouts())
	if pid := r.locks.backendPIDForTest(); pid != 0 {
		t.Fatalf("a new Registry already has lock backend %d, want none", pid)
	}
	if err := r.ProbeLockOnce(ctx); err != nil {
		t.Fatalf("ProbeLockOnce with no connection open = %v, want nil", err)
	}
	waitUntil(t, 5*time.Second, func() bool { return r.locks.backendPIDForTest() != 0 })
}
