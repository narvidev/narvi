//go:build integration

// Integration tests pinning how the lock connection (lockholder.go) is
// retried and dialled again: TryLock's one retry, the bound on each connect
// attempt, the dial that follows a loss, and the probe's dial when no
// connection is open. Same rules as lockholder_integration_test.go.
package sessionactor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"sync/atomic"
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

// TestLockHolder_DialBoundsEachConnectAttempt (T20) proves the lock
// connection's dial bounds each host by ActorLockConnectAttemptTimeout when
// the database URL sets no connect_timeout: behind a multi-host URL with
// none whose first host accepts and never answers, the dial gives up on
// that host after the bound (1 s here) and lands on the second. Without a
// bound pgx waits on the first host for as long as the dial's own context
// lives -- until shutdown -- and every hydration on the replica fails with
// ErrActorUnavailable meanwhile. T9 always sets connect_timeout, so it
// never reaches the bound.
func TestLockHolder_DialBoundsEachConnectAttempt(t *testing.T) {
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
	r := newLockTestRegistry(ctx, t, pool, timeouts)
	started := time.Now()
	r.locks.startDial()
	waitUntil(t, 5*timeouts.ActorLockConnectAttemptTimeout, func() bool { return r.locks.backendPIDForTest() != 0 })
	if took := time.Since(started); took < timeouts.ActorLockConnectAttemptTimeout {
		t.Fatalf("the lock connection landed after %v, less than one connect attempt (%v): the first host is not hanging", took, timeouts.ActorLockConnectAttemptTimeout)
	}
}

// blackholeDial stands in for a network that drops packets silently. While
// partitioned, a connect gets no answer at all and waits until its own
// context ends -- the worst case of a connect sitting in the kernel's SYN
// retransmission backoff, where no retransmission lands after the heal in
// time -- even when the heal comes first. Once healed, a new connect
// reaches target at once, whatever address it was handed.
type blackholeDial struct {
	target      string
	partitioned atomic.Bool
}

func (b *blackholeDial) dial(ctx context.Context, network, _ string) (net.Conn, error) {
	if b.partitioned.Load() {
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, b.target)
}

// TestLockHolder_ADialStartedInAPartitionEndsWithItsAttempt (T23) proves a
// lock dial started while the network drops packets silently -- as the
// dial report() starts after a loss found during a partition -- does not
// keep the replica from hosting sessions long after the heal. The URL sets
// no connect_timeout, as the deploy template's does not: the stuck attempt
// ends at ActorLockConnectAttemptTimeout (1 s here), and the first
// hydration after it dials afresh and succeeds, or, when only the probe
// asks, the next tick does. A URL that sets its own connect_timeout keeps
// it. The partition heals 200 ms into the attempt. Without the bound, the
// query pool's 2-minute fallback held that one dial -- which every
// hydration on the replica waits for -- until a retransmission landed, up to
// about a minute after the heal, every hydration failing meanwhile.
func TestLockHolder_ADialStartedInAPartitionEndsWithItsAttempt(t *testing.T) {
	timeouts := lockTestTimeouts()
	if err := timeouts.Validate(); err != nil {
		t.Fatalf("test timeouts: %v", err)
	}
	attempt := timeouts.ActorLockConnectAttemptTimeout
	for _, tc := range []struct {
		name       string
		urlTimeout string // the URL's connect_timeout, in seconds; "" for none
		probeOnly  bool   // only the probe loop asks for a connection
		// The lock connection must come up no sooner than atLeast after
		// the dial started -- the stuck attempt ran to its end -- and no
		// later than within.
		atLeast, within time.Duration
	}{
		{"the URL sets no connect_timeout: the first hydration after the attempt dials afresh",
			"", false, attempt, attempt + time.Second},
		{"the URL sets no connect_timeout and only the probe asks: back within one attempt and one interval",
			"", true, attempt, attempt + timeouts.ActorLockProbeInterval + time.Second},
		{"the URL's own connect_timeout is honoured",
			"3", false, 3 * time.Second, 3*time.Second + time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			admin, connStr := IntegrationTestPoolAndConnStr(t)
			dsn := withoutConnectTimeout(t, connStr)
			if tc.urlTimeout != "" {
				dsn = withConnParams(t, dsn, map[string]string{"connect_timeout": tc.urlTimeout})
			}
			pool := newLockTestPool(ctx, t, dsn, 2)
			cc := pool.Config().ConnConfig
			network := &blackholeDial{target: net.JoinHostPort(cc.Host, strconv.Itoa(int(cc.Port)))}
			r := newLockTestRegistry(ctx, t, pool, timeouts)
			// One address, so one dial is one attempt: pgx gives every
			// address a host name resolves to an attempt of its own, and
			// the dial function reaches the real server whatever it is
			// handed.
			r.locks.dialViaForTest(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(int(cc.Port))))
			r.locks.dialFuncForTest(t, network.dial)
			sessionID := createTestSession(ctx, t, admin)

			network.partitioned.Store(true)
			started := time.Now()
			if tc.probeOnly {
				startLockProbeForTest(ctx, t, r) // it dials at once: the stuck attempt
			} else {
				r.locks.startDial() // as report() does after a loss
			}
			time.Sleep(200 * time.Millisecond)
			network.partitioned.Store(false)

			var (
				up      time.Duration
				lastErr error
			)
			for deadline := started.Add(tc.within); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				if tc.probeOnly {
					if r.locks.backendPIDForTest() != 0 {
						up = time.Since(started)
						break
					}
					continue
				}
				if _, lastErr = r.GetOrSpawn(ctx, sessionID); lastErr == nil {
					up = time.Since(started)
					break
				}
			}
			switch {
			case up == 0:
				t.Fatalf("no lock connection within %v of a dial started in the partition, healed after 200 ms (last hydration error: %v)", tc.within, lastErr)
			case up < tc.atLeast:
				t.Fatalf("the lock connection came up %v after the dial started, before the stuck attempt's %v bound: the partition model did not hold the dial", up, tc.atLeast)
			}
			t.Logf("the lock connection came up %v after a dial started in the partition (bound %v, limit %v)", up.Round(time.Millisecond), tc.atLeast, tc.within)
		})
	}
}

// TestLockHolder_ALossIsRedialledAtOnce (T21) proves a lost lock
// connection is dialled again as soon as the loss is found, whoever finds
// it: here a probe, with no probe loop running and nothing else asking for
// a connection -- no hydration, no tick -- and a new connection still
// comes up at once, under an application_name of its own, not the lost
// one's: the terminate after a loss matches that name. Without the redial,
// a loss found by a probe or an unlock left the replica with no lock
// connection until the next hydration or tick asked for one.
func TestLockHolder_ALossIsRedialledAtOnce(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	r := newLockTestRegistry(ctx, t, newLockTestPool(ctx, t, connStr, 2), lockTestTimeouts())
	if _, err := r.GetOrSpawn(ctx, createTestSession(ctx, t, admin)); err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	oldPID, oldName := r.locks.backendPIDForTest(), r.locks.appNameForTest()

	terminateLockBackend(ctx, t, admin, oldPID)
	if err := r.ProbeLockOnce(ctx); err == nil {
		t.Fatal("ProbeLockOnce on a terminated lock backend = nil, want the loss reported")
	}
	waitUntil(t, 5*time.Second, func() bool {
		pid := r.locks.backendPIDForTest()
		return pid != 0 && pid != oldPID
	})
	if name := r.locks.appNameForTest(); !isLockConnApplicationName(name) || !isLockConnApplicationName(oldName) || name == oldName {
		t.Fatalf("the lost connection reported application_name %q and the redial %q, want two distinct lock-connection names", oldName, name)
	}
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
