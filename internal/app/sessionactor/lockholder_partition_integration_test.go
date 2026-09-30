//go:build integration

// Integration tests for the backend a lost lock connection can leave
// behind (lockholder.go): after a network partition the server may never
// hear the lost connection close, and would keep its backend -- and every
// advisory lock on it -- for as long as its own TCP keepalives let it.
// Same rules as lockholder_integration_test.go: each test builds its own
// small pools from the shared container, and nothing here terminates a
// backend but the test's own, matched by pid AND application_name AND this
// test's own database.
package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"
)

// dialViaForTest makes every later dial of the lock connection go to addr
// instead of the pool's host -- the pool itself still goes straight to
// Postgres. Call it before anything dials.
func (h *lockHolder) dialViaForTest(t *testing.T, addr string) {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		t.Fatalf("parse port %q: %v", port, err)
	}
	h.dialMu.Lock()
	defer h.dialMu.Unlock()
	if h.dialing != nil {
		t.Fatal("dialViaForTest called with a dial already in flight")
	}
	cc := h.connConfig.Copy()
	cc.Host, cc.Port, cc.Fallbacks = host, uint16(p), nil
	h.connConfig = cc
}

// settingForTest reads one server setting as the lock connection's own
// backend reports it.
func (h *lockHolder) settingForTest(ctx context.Context, t *testing.T, name string) string {
	t.Helper()
	h.sem <- struct{}{}
	defer h.leave()
	if h.conn == nil {
		t.Fatalf("read %s: no lock connection open", name)
	}
	var v string
	if err := h.conn.QueryRow(ctx, `SELECT current_setting($1)`, name).Scan(&v); err != nil {
		t.Fatalf("read %s on the lock connection: %v", name, err)
	}
	return v
}

// setOrphanForTest records b as the backend the last lost connection left
// behind, for the next dial to deal with.
func (h *lockHolder) setOrphanForTest(b lockBackend) {
	h.sem <- struct{}{}
	defer h.leave()
	h.orphan = &b
}

// orphanForTest reports the backend still waiting for a dial to deal with
// it, if any.
func (h *lockHolder) orphanForTest() *lockBackend {
	h.sem <- struct{}{}
	defer h.leave()
	return h.orphan
}

// setTerminateSQLForTest makes query terminateOrphan's statement. Call it
// before anything dials.
func (h *lockHolder) setTerminateSQLForTest(t *testing.T, query string) {
	t.Helper()
	h.dialMu.Lock()
	defer h.dialMu.Unlock()
	if h.dialing != nil {
		t.Fatal("setTerminateSQLForTest called with a dial already in flight")
	}
	h.terminateSQL = query
}

// dialFuncForTest makes every later dial of the lock connection connect
// through dial -- the pool itself keeps its own. Call it before anything
// dials.
func (h *lockHolder) dialFuncForTest(t *testing.T, dial pgconn.DialFunc) {
	t.Helper()
	h.dialMu.Lock()
	defer h.dialMu.Unlock()
	if h.dialing != nil {
		t.Fatal("dialFuncForTest called with a dial already in flight")
	}
	cc := h.connConfig.Copy()
	cc.DialFunc = dial
	h.connConfig = cc
}

// installedForTest reports whether a lock connection is installed, taking
// sem within wait; busy reports that sem stayed held throughout.
func (h *lockHolder) installedForTest(wait time.Duration) (installed, busy bool) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case h.sem <- struct{}{}:
	case <-timer.C:
		return false, true
	}
	defer h.leave()
	return h.conn != nil, false
}

// startLockProbeForTest runs r's lock probe loop until the test ends.
func startLockProbeForTest(ctx context.Context, t *testing.T, r *Registry) {
	t.Helper()
	probeCtx, stopProbe := context.WithCancel(ctx)
	var probe errgroup.Group
	probe.Go(func() error { return r.RunLockProbe(probeCtx) })
	t.Cleanup(func() {
		stopProbe()
		_ = probe.Wait()
	})
}

// partitionProxy is a TCP proxy between one Registry's lock connection and
// Postgres that models a network partition which outlasts TCP's
// retransmission window. partition severs every flow open at that moment
// for good: nothing either end sends on it is delivered again, and neither
// end's close reaches the other, so the server keeps that backend. A flow
// opened while the partition lasts goes nowhere; one opened after heal
// passes. It is a userspace model: through it the server's keepalive probes
// are still acknowledged, by the proxy's own socket, so the server never
// reaps a severed backend here and only the lock holder's terminateOrphan
// can end it. The server keepalives are pinned by
// TestLockHolder_LockBackendRunsWithServerKeepalives.
type partitionProxy struct {
	ln     net.Listener
	target string

	mu          sync.Mutex
	partitioned bool
	closed      bool
	flows       []*proxyFlow

	g errgroup.Group
}

// proxyFlow is one connection through the proxy, and its own connection to
// Postgres -- nil for a flow opened during a partition.
type proxyFlow struct {
	client, server net.Conn
	severed        atomic.Bool
}

// startPartitionProxy listens on a local port and forwards to target until
// the test ends.
func startPartitionProxy(t *testing.T, target string) *partitionProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &partitionProxy{ln: ln, target: target}
	p.g.Go(p.accept)
	t.Cleanup(p.close)
	return p
}

func (p *partitionProxy) addr() string { return p.ln.Addr().String() }

func (p *partitionProxy) accept() error {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return nil
		}
		f := &proxyFlow{client: c}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = c.Close()
			return nil
		}
		p.flows = append(p.flows, f)
		partitioned := p.partitioned
		p.mu.Unlock()

		if partitioned {
			f.severed.Store(true)
			p.g.Go(func() error {
				_, _ = io.Copy(io.Discard, c)
				return nil
			})
			continue
		}
		s, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		p.mu.Lock()
		f.server = s
		closed := p.closed
		p.mu.Unlock()
		if closed {
			_ = s.Close()
			continue
		}
		p.g.Go(func() error {
			p.pump(f, c, s)
			return nil
		})
		p.g.Go(func() error {
			p.pump(f, s, c)
			return nil
		})
	}
}

// pump copies src to dst until src ends, passing the close on -- unless
// the flow is severed, which forwards nothing and passes no close on.
func (p *partitionProxy) pump(f *proxyFlow, src, dst net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 && !f.severed.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			if !f.severed.Load() {
				_ = dst.Close()
			}
			return
		}
	}
}

// partition severs every flow open now, for good, and every flow opened
// until heal.
func (p *partitionProxy) partition() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.partitioned = true
	for _, f := range p.flows {
		f.severed.Store(true)
	}
}

// heal lets flows opened from now on through. Flows severed stay severed.
func (p *partitionProxy) heal() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.partitioned = false
}

// close stops the proxy and closes every connection through it, severed or
// not: a backend still orphaned then hears its client go.
func (p *partitionProxy) close() {
	_ = p.ln.Close()
	p.mu.Lock()
	p.closed = true
	var conns []net.Conn
	for _, f := range p.flows {
		conns = append(conns, f.client)
		if f.server != nil {
			conns = append(conns, f.server)
		}
	}
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	_ = p.g.Wait()
}

// lockBackendAlive reports whether pid is still a lock-connection backend on
// this test's database.
func lockBackendAlive(ctx context.Context, t *testing.T, admin *pgxpool.Pool, pid uint32) bool {
	t.Helper()
	var n int
	if err := admin.QueryRow(ctx, `
		SELECT count(*) FROM pg_stat_activity
		WHERE pid = $1 AND starts_with(application_name, $2) AND datname = current_database()`,
		int32(pid), lockConnApplicationNamePrefix).Scan(&n); err != nil {
		t.Fatalf("look up backend %d: %v", pid, err)
	}
	return n == 1
}

// TestLockHolder_PartitionedBackendIsTerminatedOnRedial (T16) proves a
// replica gets its sessions back after a partition that outlasted TCP's
// retransmission window, although the server never heard its lost lock
// connection close. Two actors, X and Y, lock on a lock connection that
// runs through partitionProxy; the proxy partitions; the probe declares the
// loss and stops both actors. At the heal the lost connection's backend is
// still there, idle, holding X's and Y's locks -- the model holds -- and
// then, within the keepalive reap time plus one probe of the heal, X and Y
// both rehydrate on this replica, on a new backend, and the old one is
// gone: the first connection dialled after the heal terminated it. The
// query pool goes straight to Postgres; only the lock connection is cut.
// Without the terminate, the old backend holds X and Y for as long as the
// server's keepalives allow -- forever, through this proxy -- and every
// GetOrSpawn for them answers ErrSessionActorElsewhere. The URL sets no
// connect_timeout, as the deploy template's does not:
// ActorLockConnectAttemptTimeout ends each dial the partition swallows.
// That the terminate returns before the new connection is installed is
// pinned by TestLockHolder_InstallsOnlyOnceTheOrphanTerminateHasReturned.
func TestLockHolder_PartitionedBackendIsTerminatedOnRedial(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	pool := newLockTestPool(ctx, t, withoutConnectTimeout(t, connStr), 4)
	cc := pool.Config().ConnConfig
	proxy := startPartitionProxy(t, net.JoinHostPort(cc.Host, strconv.Itoa(int(cc.Port))))

	timeouts := lockTestShippedTimeouts(t)
	timeouts.ActorLockProbeInterval = 2 * time.Second
	timeouts.ActorLockConnectAttemptTimeout = time.Second
	if err := timeouts.Validate(); err != nil {
		t.Fatalf("test timeouts: %v", err)
	}
	r := newLockTestRegistry(ctx, t, pool, timeouts)
	r.locks.dialViaForTest(t, proxy.addr())
	startLockProbeForTest(ctx, t, r)
	waitUntil(t, 5*time.Second, func() bool { return r.locks.backendPIDForTest() != 0 })

	sessions := []pgtype.UUID{createTestSession(ctx, t, admin), createTestSession(ctx, t, admin)}
	actors := make([]*Actor, len(sessions))
	for i, id := range sessions {
		a, err := r.GetOrSpawn(ctx, id)
		if err != nil {
			t.Fatalf("GetOrSpawn for session %d before the partition: %v", i+1, err)
		}
		actors[i] = a
	}
	oldPID := r.locks.backendPIDForTest()
	lostBefore := readCounterSum(ctx, t, otelReader, "session_actor_lock_conn_lost")

	proxy.partition()
	waitUntil(t, timeouts.ActorLockProbeInterval+timeouts.ActorLockStatementTimeout+2*time.Second, func() bool {
		return readCounterSum(ctx, t, otelReader, "session_actor_lock_conn_lost")-lostBefore == 1
	})
	for i, a := range actors {
		if err := a.Send(ctx, TimerFired{Name: recoveryTestTimerName}); !errors.Is(err, ErrActorStopped) {
			t.Fatalf("Send to actor %d after the partition's loss = %v, want ErrActorStopped", i+1, err)
		}
	}
	// Through one more probe tick: the dial it starts goes nowhere.
	time.Sleep(timeouts.ActorLockProbeInterval)
	for i, id := range sessions {
		if got := advisoryLockHolders(ctx, t, admin, id); len(got) != 1 || got[0] != oldPID {
			t.Fatalf("at the heal, session %d's advisory lock is held by pids %v, want the lost connection's backend %d: the partition left no orphan", i+1, got, oldPID)
		}
	}

	proxy.heal()
	healed := time.Now()
	limit := timeouts.ActorLockServerReapTime() + timeouts.ActorLockProbeInterval
	rehydrated := make([]bool, len(sessions))
	waitUntil(t, limit, func() bool {
		done := true
		for i, id := range sessions {
			if !rehydrated[i] {
				_, err := r.GetOrSpawn(ctx, id)
				rehydrated[i] = err == nil
			}
			done = done && rehydrated[i]
		}
		return done
	})
	t.Logf("both sessions rehydrated %v after the heal (limit %v)", time.Since(healed).Round(time.Millisecond), limit)

	newPID := r.locks.backendPIDForTest()
	if newPID == 0 || newPID == oldPID {
		t.Fatalf("lock backend after the heal = %d, want a new one (the lost one was %d)", newPID, oldPID)
	}
	for i, id := range sessions {
		if got := advisoryLockHolders(ctx, t, admin, id); len(got) != 1 || got[0] != newPID {
			t.Errorf("session %d's advisory lock is held by pids %v, want only the new lock backend %d", i+1, got, newPID)
		}
	}
	if lockBackendAlive(ctx, t, admin, oldPID) {
		t.Errorf("the lost connection's backend %d is still there", oldPID)
	}
}

// TestLockHolder_LockBackendRunsWithServerKeepalives (T17) proves every
// lock connection's backend runs with the lock connection's own server-side
// keepalives (ActorLockServerKeepalive*, and the reap time as its
// tcp_user_timeout), read back from the backend itself: Postgres reports
// what it applied to its socket. When the database URL sets its own, the
// lock connection's win on the lock connection, and the query pool keeps
// the URL's. Without them the server keeps a backend whose client vanished
// for its own defaults -- over two hours -- and a replica that never comes
// back to terminate it leaves its sessions unhostable everywhere that long.
// The partition itself was checked by hand, against iptables in a client
// container: partitionProxy's socket acknowledges the server's probes.
func TestLockHolder_LockBackendRunsWithServerKeepalives(t *testing.T) {
	ctx := context.Background()
	timeouts := lockTestShippedTimeouts(t)
	// The user timeout is the whole reap time, idle plus interval times
	// count, spelled out here rather than read from ActorLockServerReapTime.
	reap := timeouts.ActorLockServerKeepaliveIdle + timeouts.ActorLockServerKeepaliveInterval*time.Duration(timeouts.ActorLockServerKeepaliveCount)
	want := map[string]string{
		"tcp_keepalives_idle":     strconv.Itoa(int(timeouts.ActorLockServerKeepaliveIdle / time.Second)),
		"tcp_keepalives_interval": strconv.Itoa(int(timeouts.ActorLockServerKeepaliveInterval / time.Second)),
		"tcp_keepalives_count":    strconv.Itoa(timeouts.ActorLockServerKeepaliveCount),
		"tcp_user_timeout":        strconv.FormatInt(reap.Milliseconds(), 10),
	}
	for _, tc := range []struct {
		name string
		url  map[string]string // settings the database URL carries
	}{
		{"the database URL sets none", nil},
		// Values unlike both the lock connection's and the kernel's own
		// defaults, so the pool's reading them proves the URL reached it.
		{"the database URL sets its own: the lock connection keeps its own, the pool the URL's", map[string]string{
			"tcp_keepalives_idle":     "600",
			"tcp_keepalives_interval": "30",
			"tcp_keepalives_count":    "4",
			"tcp_user_timeout":        "1234",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, connStr := IntegrationTestPoolAndConnStr(t)
			pool := newLockTestPool(ctx, t, withConnParams(t, connStr, tc.url), 2)
			r := newLockTestRegistry(ctx, t, pool, timeouts)
			r.locks.startDial()
			waitUntil(t, 5*time.Second, func() bool { return r.locks.backendPIDForTest() != 0 })

			for name, v := range want {
				if got := r.locks.settingForTest(ctx, t, name); got != v {
					t.Errorf("the lock backend's %s = %s, want %s", name, got, v)
				}
			}
			for name, v := range tc.url {
				var got string
				if err := pool.QueryRow(ctx, `SELECT current_setting($1)`, name).Scan(&got); err != nil {
					t.Fatalf("read %s on the query pool: %v", name, err)
				}
				if got != v {
					t.Errorf("the query pool's %s = %s, want the URL's %s", name, got, v)
				}
			}
		})
	}
}

// withDatabase returns connStr naming database instead of its own.
func withDatabase(t *testing.T, connStr, database string) string {
	t.Helper()
	u, err := url.Parse(connStr)
	if err != nil {
		t.Fatalf("parse the test connection string: %v", err)
	}
	u.Path = "/" + database
	return u.String()
}

// TestLockHolder_TerminatesOnlyTheLostBackend (T18) proves the backend a
// dial terminates after a loss is the one still running the lost
// connection's session and no other: a stand-in backend this test opens,
// named as a lock connection's dial names it, is recorded as the orphan,
// and the next dial terminates it only when its pid, its start time, its
// application_name -- the lost connection's own -- and its database all
// match. A pid alone may have been reused by an unrelated backend since
// the loss (the start time differs); another lock connection's name is a
// backend a session-mode pooler has since handed to another client (the
// pooler sets each client's own name: TestLockHolder_BehindASessionPooler);
// another database is out of the statement's reach. In every case the
// orphan is dealt with, and the new connection installed.
func TestLockHolder_TerminatesOnlyTheLostBackend(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name           string
		otherName      bool   // record the orphan under another lock connection's name
		database       string // the stand-in's database, if not this test's
		shiftStart     bool   // record a start time a microsecond off
		wantTerminated bool
	}{
		{"pid, start, the lost connection's own application_name and database all match: terminated", false, "", false, true},
		{"another start time -- a pid reused since -- is left alone", false, "", true, false},
		{"another lock connection's application_name -- a backend handed to another client since -- is left alone", true, "", false, false},
		{"another database is left alone", false, "postgres", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admin, connStr := IntegrationTestPoolAndConnStr(t)
			standInName := newLockConnApplicationName()
			standInURL := withConnParams(t, connStr, map[string]string{"application_name": standInName})
			if tc.database != "" {
				standInURL = withDatabase(t, standInURL, tc.database)
			}
			standIn, err := pgx.Connect(ctx, standInURL)
			if err != nil {
				t.Fatalf("open the stand-in backend: %v", err)
			}
			t.Cleanup(func() { _ = standIn.Close(context.Background()) })
			var (
				pid   int32
				start pgtype.Timestamptz
			)
			if err := standIn.QueryRow(ctx, lockBackendQuery).Scan(&pid, &start); err != nil || !start.Valid {
				t.Fatalf("read the stand-in's backend: pid %d, start %v, err %v", pid, start, err)
			}
			orphan := lockBackend{pid: pid, start: start.Time, appName: standInName}
			if tc.shiftStart {
				orphan.start = orphan.start.Add(-time.Microsecond)
			}
			if tc.otherName {
				orphan.appName = newLockConnApplicationName()
			}

			r := newLockTestRegistry(ctx, t, newLockTestPool(ctx, t, connStr, 2), lockTestTimeouts())
			r.locks.setOrphanForTest(orphan)
			r.locks.startDial()
			waitUntil(t, 5*time.Second, func() bool { return r.locks.backendPIDForTest() != 0 })
			if got := r.locks.orphanForTest(); got != nil {
				t.Errorf("the orphan %+v is still recorded after a dial dealt with it", *got)
			}

			standInGone := func() bool {
				var n int
				if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&n); err != nil {
					t.Fatalf("look up the stand-in's backend %d: %v", pid, err)
				}
				return n == 0
			}
			if tc.wantTerminated {
				waitUntil(t, 5*time.Second, standInGone)
				return
			}
			// A terminated backend is gone within milliseconds; give it
			// ample time to show.
			time.Sleep(300 * time.Millisecond)
			if standInGone() {
				t.Fatalf("the stand-in backend %d was terminated", pid)
			}
			if err := standIn.Ping(ctx); err != nil {
				t.Fatalf("the stand-in backend %d no longer answers: %v", pid, err)
			}
		})
	}
}

// withUser returns connStr logging in as user with password.
func withUser(t *testing.T, connStr, user, password string) string {
	t.Helper()
	u, err := url.Parse(connStr)
	if err != nil {
		t.Fatalf("parse the test connection string: %v", err)
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}

// TestLockHolder_ARefusedTerminateDoesNotHoldUpTheDial (T18b) proves the
// terminate after a loss is best effort. The lock connection's role here
// may read every backend's start time (pg_read_all_stats) but may not
// signal a superuser's, and the orphan recorded is a superuser's stand-in
// backend: the server refuses the terminate with an ERROR, the refusal is
// logged, the orphan is let go, and the new connection is installed
// anyway, leaving the orphan to the server's keepalives. A dial that failed
// on the refusal would fail the same way on every retry, and leave the
// replica no lock connection at all.
func TestLockHolder_ARefusedTerminateDoesNotHoldUpTheDial(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	role := "narvi_locktest_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	const password = "locktest"
	if _, err := admin.Exec(ctx, `CREATE ROLE `+pgx.Identifier{role}.Sanitize()+` LOGIN PASSWORD '`+password+`' IN ROLE pg_read_all_stats`); err != nil {
		t.Fatalf("create the lock connection's role: %v", err)
	}
	// Registered first, so it runs last: after the Registry and its pool
	// have closed every connection the role opened.
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), `DROP ROLE `+pgx.Identifier{role}.Sanitize()); err != nil {
			t.Errorf("drop the lock connection's role: %v", err)
		}
	})

	standInName := newLockConnApplicationName()
	standIn, err := pgx.Connect(ctx, withConnParams(t, connStr, map[string]string{"application_name": standInName}))
	if err != nil {
		t.Fatalf("open the stand-in backend: %v", err)
	}
	t.Cleanup(func() { _ = standIn.Close(context.Background()) })
	var (
		pid   int32
		start pgtype.Timestamptz
		super bool
	)
	if err := standIn.QueryRow(ctx, lockBackendQuery).Scan(&pid, &start); err != nil || !start.Valid {
		t.Fatalf("read the stand-in's backend: pid %d, start %v, err %v", pid, start, err)
	}
	if err := standIn.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil || !super {
		t.Fatalf("the stand-in's role is a superuser = %v (err %v), want true: only then may the lock connection's role not signal it", super, err)
	}

	logs := captureDefaultLoggerJSONSync(t)
	r := newLockTestRegistry(ctx, t, newLockTestPool(ctx, t, withUser(t, connStr, role, password), 2), lockTestTimeouts())
	r.locks.setOrphanForTest(lockBackend{pid: pid, start: start.Time, appName: standInName})
	r.locks.startDial()
	waitUntil(t, 5*time.Second, func() bool { return r.locks.backendPIDForTest() != 0 })
	refusal := waitForLogEntry(t, logs, 5*time.Second,
		"sessionactor: could not terminate the backend a lost lock connection left behind; the server's keepalives will end it")
	if got, _ := refusal["error"].(string); !strings.Contains(got, "42501") {
		t.Errorf("the terminate's refusal logged error %q, want SQLSTATE 42501 (insufficient privilege)", got)
	}
	if got := r.locks.orphanForTest(); got != nil {
		t.Errorf("the orphan %+v is still recorded after a dial dealt with it", *got)
	}
	if err := standIn.Ping(ctx); err != nil {
		t.Fatalf("the stand-in backend %d no longer answers: %v", pid, err)
	}
}

// TestLockHolder_RefusedKeepalivesDoNotHoldUpTheDial (T18c) proves the
// server keepalives are best effort too: when the server refuses the
// statement that sets them with an ERROR -- injected by a public.set_config
// that raises, found ahead of pg_catalog's on this connection's
// search_path -- the refusal is logged, and the connection is installed
// without them. A dial that failed on it would fail on every retry.
func TestLockHolder_RefusedKeepalivesDoNotHoldUpTheDial(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	if _, err := admin.Exec(ctx, `CREATE FUNCTION public.set_config(text, text, boolean) RETURNS text LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected refusal' USING ERRCODE = '42501'; END $$`); err != nil {
		t.Fatalf("create the refusing set_config: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), `DROP FUNCTION public.set_config(text, text, boolean)`); err != nil {
			t.Errorf("drop the refusing set_config: %v", err)
		}
	})

	timeouts := lockTestShippedTimeouts(t)
	logs := captureDefaultLoggerJSONSync(t)
	pool := newLockTestPool(ctx, t, withConnParams(t, connStr, map[string]string{"search_path": "public,pg_catalog"}), 2)
	r := newLockTestRegistry(ctx, t, pool, timeouts)
	r.locks.startDial()
	waitUntil(t, 5*time.Second, func() bool { return r.locks.backendPIDForTest() != 0 })
	refusal := waitForLogEntry(t, logs, 5*time.Second,
		"sessionactor: the server refused the lock connection's keepalives; a lock backend it loses touch with lasts as long as its own keepalives allow")
	if got, _ := refusal["error"].(string); !strings.Contains(got, "injected refusal") {
		t.Errorf("the keepalives' refusal logged error %q, want the injected one", got)
	}
	if got, ours := r.locks.settingForTest(ctx, t, "tcp_keepalives_idle"), strconv.Itoa(int(timeouts.ActorLockServerKeepaliveIdle/time.Second)); got == ours {
		t.Fatalf("the lock backend's tcp_keepalives_idle = %s, the lock connection's own: the refusal was not injected", got)
	}
}

// logIndex returns the index of the first line in buf whose "msg" is msg,
// or -1.
func logIndex(t *testing.T, buf *syncLogBuffer, msg string) int {
	t.Helper()
	i := 0
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		if entry["msg"] == msg {
			return i
		}
		i++
	}
	return -1
}

// advisoryLockWaiters counts the backends waiting, not yet granted, for the
// advisory lock on key (a bigint key, pg_locks's documented layout).
func advisoryLockWaiters(ctx context.Context, t *testing.T, admin *pgxpool.Pool, key int64) int {
	t.Helper()
	var n int
	if err := admin.QueryRow(ctx, `
		SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory' AND NOT granted AND objsubid = 1
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND classid::bigint = (($1::bigint >> 32) & 4294967295)
		  AND objid::bigint = ($1::bigint & 4294967295)`, key).Scan(&n); err != nil {
		t.Fatalf("count the waiters on advisory lock %d: %v", key, err)
	}
	return n
}

// Log lines TestLockHolder_InstallsOnlyOnceTheOrphanTerminateHasReturned
// orders.
const (
	logLockConnEstablished = "sessionactor: lock connection established"
	logOrphanTerminated    = "sessionactor: terminated the backend a lost lock connection left behind, still holding its advisory locks"
)

// TestLockHolder_InstallsOnlyOnceTheOrphanTerminateHasReturned (T24) pins
// the order dialAndInstall promises: a new lock connection is installed
// only once the terminate of the backend its lost predecessor left behind
// has returned. The terminate is held back -- its statement first waits
// for an advisory lock this test holds -- and while it waits, no connection
// is installed, sem is free, and nothing has logged the connection
// established. Released, it terminates the orphan and waits for it to end,
// then the connection is installed: the orphan is gone by then, and the
// session it held rehydrates at the first attempt. A dial that installed
// first -- then terminated, on a goroutine of its own or after leaving and
// re-entering sem -- lets a hydration find the session still held by the
// orphan and answer ErrSessionActorElsewhere, which the timer pump skips
// silently until the timer's claim expires. So does a terminate that
// returns before the orphan has ended; this idle stand-in ends within
// milliseconds of its signal, so that race is
// TestLockHolder_InstallsOnlyOnceTheOrphanHasEnded's (T26), which holds the
// orphan's exit back. T16 cannot see either: it retries until the session
// rehydrates.
func TestLockHolder_InstallsOnlyOnceTheOrphanTerminateHasReturned(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	sessionID := createTestSession(ctx, t, admin)

	// The orphan: a stand-in backend named as a lock connection's dial
	// names one, holding the session's advisory lock.
	orphanName := newLockConnApplicationName()
	standIn, err := pgx.Connect(ctx, withConnParams(t, connStr, map[string]string{"application_name": orphanName}))
	if err != nil {
		t.Fatalf("open the stand-in backend: %v", err)
	}
	t.Cleanup(func() { _ = standIn.Close(context.Background()) })
	var (
		pid    int32
		start  pgtype.Timestamptz
		locked bool
	)
	if err := standIn.QueryRow(ctx, lockBackendQuery).Scan(&pid, &start); err != nil || !start.Valid {
		t.Fatalf("read the stand-in's backend: pid %d, start %v, err %v", pid, start, err)
	}
	if err := standIn.QueryRow(ctx, tryAdvisoryLockQuery, sessionID.String()).Scan(&locked); err != nil || !locked {
		t.Fatalf("lock the session on the stand-in = %v, %v; want it locked", locked, err)
	}

	// The gate: an advisory lock this test holds on a connection of its own.
	gate, err := pgx.Connect(ctx, connStr)
	if err != nil {
		t.Fatalf("open the gate's connection: %v", err)
	}
	t.Cleanup(func() { _ = gate.Close(context.Background()) })
	var gateKey int64
	if err := gate.QueryRow(ctx, `SELECT hashtextextended($1, 0)`, orphanName+"/gate").Scan(&gateKey); err != nil {
		t.Fatalf("derive the gate's key: %v", err)
	}
	if _, err := gate.Exec(ctx, `SELECT pg_advisory_lock($1)`, gateKey); err != nil {
		t.Fatalf("take the gate: %v", err)
	}
	if strings.Count(terminateOrphanQuery, "WHERE ") != 1 {
		t.Fatalf("terminateOrphanQuery has %d WHERE clauses, want 1 to gate", strings.Count(terminateOrphanQuery, "WHERE "))
	}
	gated := strings.Replace(terminateOrphanQuery, "WHERE ",
		fmt.Sprintf("WHERE (SELECT count(*) FROM (SELECT pg_advisory_xact_lock(%d)) AS gate) = 1 AND ", gateKey), 1)

	// A statement bound well above how long the gate is held below.
	timeouts := lockTestShippedTimeouts(t)
	timeouts.ActorHydrateTimeout = 4 * time.Second
	timeouts.ActorLockStatementTimeout = 3 * time.Second
	if err := timeouts.Validate(); err != nil {
		t.Fatalf("test timeouts: %v", err)
	}
	logs := captureDefaultLoggerJSONSync(t)
	r := newLockTestRegistry(ctx, t, newLockTestPool(ctx, t, connStr, 2), timeouts)
	r.locks.setTerminateSQLForTest(t, gated)
	r.locks.setOrphanForTest(lockBackend{pid: pid, start: start.Time, appName: orphanName})
	r.locks.startDial()

	waitUntil(t, 5*time.Second, func() bool { return advisoryLockWaiters(ctx, t, admin, gateKey) == 1 })
	for i := range 4 {
		installed, busy := r.locks.installedForTest(50 * time.Millisecond)
		if installed || busy {
			t.Fatalf("check %d while the orphan's terminate waits: a connection installed = %v, sem held = %v; want neither", i+1, installed, busy)
		}
		if logIndex(t, logs, logLockConnEstablished) >= 0 {
			t.Fatalf("check %d while the orphan's terminate waits: %q already logged", i+1, logLockConnEstablished)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !lockBackendAlive(ctx, t, admin, uint32(pid)) {
		t.Fatalf("the orphan %d is gone while its terminate still waits", pid)
	}
	if _, err := gate.Exec(ctx, `SELECT pg_advisory_unlock($1)`, gateKey); err != nil {
		t.Fatalf("release the gate: %v", err)
	}

	waitUntil(t, 5*time.Second, func() bool { return r.locks.backendPIDForTest() != 0 })
	terminatedAt, establishedAt := logIndex(t, logs, logOrphanTerminated), logIndex(t, logs, logLockConnEstablished)
	if terminatedAt < 0 || establishedAt < terminatedAt {
		t.Fatalf("log order: %q at line %d, %q at line %d; want the terminate first", logOrphanTerminated, terminatedAt, logLockConnEstablished, establishedAt)
	}
	if lockBackendAlive(ctx, t, admin, uint32(pid)) {
		t.Fatalf("the orphan %d is still there once the new connection is installed", pid)
	}
	if _, err := r.GetOrSpawn(ctx, sessionID); err != nil {
		t.Fatalf("GetOrSpawn for the session the orphan held, once the new connection is installed: %v", err)
	}
}

// Log lines the tests of the wait for an orphan to end look for.
const (
	logOrphanOutlastedWait = "sessionactor: the backend a lost lock connection left behind was not seen to end within the wait for it; until it does, or the server's keepalives end it, its advisory locks keep its sessions from every replica"
	logLockDialFailed      = "sessionactor: could not dial the lock connection"
)

// lockingStandIn opens a stand-in for the backend a lost lock connection
// left behind: named as a lock connection's dial names one, and holding
// sessionID's advisory lock. It returns that backend as the lost connection
// would have recorded it, and the stand-in's connection, closed when the
// test ends.
func lockingStandIn(ctx context.Context, t *testing.T, connStr string, sessionID pgtype.UUID) (lockBackend, *pgx.Conn) {
	t.Helper()
	name := newLockConnApplicationName()
	standIn, err := pgx.Connect(ctx, withConnParams(t, connStr, map[string]string{"application_name": name}))
	if err != nil {
		t.Fatalf("open the stand-in backend: %v", err)
	}
	t.Cleanup(func() { _ = standIn.Close(context.Background()) })
	var (
		pid    int32
		start  pgtype.Timestamptz
		locked bool
	)
	if err := standIn.QueryRow(ctx, lockBackendQuery).Scan(&pid, &start); err != nil || !start.Valid {
		t.Fatalf("read the stand-in's backend: pid %d, start %v, err %v", pid, start, err)
	}
	if err := standIn.QueryRow(ctx, tryAdvisoryLockQuery, sessionID.String()).Scan(&locked); err != nil || !locked {
		t.Fatalf("lock the session on the stand-in = %v, %v; want it locked", locked, err)
	}
	return lockBackend{pid: pid, start: start.Time, appName: name}, standIn
}

// holdExitOf holds back the exit of standIn's backend until release is
// called, or the test ends. standIn creates a temporary table, and a
// transaction of the test's own locks it. A backend drops its temporary
// tables as it exits, before it releases its session-level locks -- the
// drop is registered as an exit callback after the release, and exit
// callbacks run last-registered first -- so once signalled, the backend
// waits on the test's lock, still holding every advisory lock it had, much
// as a loaded server's backend is slow to exit, only for as long as the
// test likes.
func holdExitOf(ctx context.Context, t *testing.T, connStr string, standIn *pgx.Conn) (release func()) {
	t.Helper()
	if _, err := standIn.Exec(ctx, `CREATE TEMPORARY TABLE exit_gate (x int)`); err != nil {
		t.Fatalf("create the stand-in's temporary table: %v", err)
	}
	var schema string
	if err := standIn.QueryRow(ctx, `SELECT nspname FROM pg_namespace WHERE oid = pg_my_temp_schema()`).Scan(&schema); err != nil {
		t.Fatalf("read the stand-in's temporary schema: %v", err)
	}
	gate, err := pgx.Connect(ctx, connStr)
	if err != nil {
		t.Fatalf("open the exit gate's connection: %v", err)
	}
	// Closed before the stand-in (t.Cleanup is LIFO), which then exits.
	t.Cleanup(func() { _ = gate.Close(context.Background()) })
	tx, err := gate.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the exit gate: %v", err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE `+pgx.Identifier{schema, "exit_gate"}.Sanitize()+` IN ACCESS SHARE MODE`); err != nil {
		t.Fatalf("lock the stand-in's temporary table: %v", err)
	}
	return func() {
		t.Helper()
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("release the exit gate: %v", err)
		}
	}
}

// backendWaitsOnALock reports whether pid is waiting for a lock: for a
// stand-in whose exit holdExitOf holds back, that it has been signalled.
func backendWaitsOnALock(ctx context.Context, t *testing.T, admin *pgxpool.Pool, pid int32) bool {
	t.Helper()
	var n int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid = $1 AND wait_event_type = 'Lock'`, pid).Scan(&n); err != nil {
		t.Fatalf("look up backend %d's wait: %v", pid, err)
	}
	return n == 1
}

// TestLockHolder_InstallsOnlyOnceTheOrphanHasEnded (T26) proves the
// terminate a dial runs after a loss returns only once the orphan it
// signalled has ended, and released its locks, so the new connection's
// first hydration finds the orphan's sessions free. A backend ends some
// time after it is signalled, and releases its session locks only then --
// milliseconds on an idle server, longer on a loaded one; here the orphan's
// exit is held back (holdExitOf). While it is, the orphan still holds the
// session, no connection is installed, and sem is free. Released, the
// orphan ends; the connection is installed, and by then the orphan and its
// lock are gone: the first GetOrSpawn succeeds. A terminate that returns
// once the signal is sent -- pg_terminate_backend without its timeout, or
// with 0 -- installs the connection while the orphan still holds the
// session, and that GetOrSpawn answers ErrSessionActorElsewhere.
func TestLockHolder_InstallsOnlyOnceTheOrphanHasEnded(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	sessionID := createTestSession(ctx, t, admin)
	orphan, standIn := lockingStandIn(ctx, t, connStr, sessionID)
	release := holdExitOf(ctx, t, connStr, standIn)

	// A wait well above how long the orphan's exit is held back below.
	timeouts := lockTestShippedTimeouts(t)
	timeouts.ActorHydrateTimeout = 6 * time.Second
	timeouts.ActorLockOrphanTerminateWait = 5 * time.Second
	if err := timeouts.Validate(); err != nil {
		t.Fatalf("test timeouts: %v", err)
	}
	logs := captureDefaultLoggerJSONSync(t)
	r := newLockTestRegistry(ctx, t, newLockTestPool(ctx, t, connStr, 2), timeouts)
	r.locks.setOrphanForTest(orphan)
	r.locks.startDial()

	waitUntil(t, 5*time.Second, func() bool { return backendWaitsOnALock(ctx, t, admin, orphan.pid) })
	for i := range 4 {
		installed, busy := r.locks.installedForTest(50 * time.Millisecond)
		if installed || busy {
			t.Fatalf("check %d while the signalled orphan's exit is held back: a connection installed = %v, sem held = %v; want neither", i+1, installed, busy)
		}
		if got := advisoryLockHolders(ctx, t, admin, sessionID); len(got) != 1 || got[0] != uint32(orphan.pid) {
			t.Fatalf("check %d while the orphan's exit is held back: the session's lock is held by pids %v, want the orphan %d", i+1, got, orphan.pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
	release()

	waitUntil(t, 5*time.Second, func() bool { return r.locks.backendPIDForTest() != 0 })
	if lockBackendAlive(ctx, t, admin, uint32(orphan.pid)) {
		t.Fatalf("the orphan %d is still there once the new connection is installed", orphan.pid)
	}
	if got := advisoryLockHolders(ctx, t, admin, sessionID); len(got) != 0 {
		t.Fatalf("once the new connection is installed, the session's lock is held by pids %v, want none", got)
	}
	if _, err := r.GetOrSpawn(ctx, sessionID); err != nil {
		t.Fatalf("the first GetOrSpawn after the install, for the session the orphan held: %v", err)
	}
	if logIndex(t, logs, logOrphanTerminated) < 0 {
		t.Fatalf("no %q logged", logOrphanTerminated)
	}
}

// TestLockHolder_AnOrphanThatOutlastsTheWaitDoesNotHoldUpTheDial (T27)
// proves the wait for an orphan to end is bounded, and that its running out
// is logged, never a failed dial. The orphan's exit is held back past
// ActorLockOrphanTerminateWait: the new connection is installed once the
// wait has run out, the orphan let go and logged as still holding its
// sessions -- GetOrSpawn for its session answers ErrSessionActorElsewhere
// -- and once it ends, the session rehydrates. A dial that failed when the
// wait ran out, or whose statement bound left no room for the wait, would
// leave the replica with no lock connection for as long as the orphan
// lasted: no session at all could be hosted.
func TestLockHolder_AnOrphanThatOutlastsTheWaitDoesNotHoldUpTheDial(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	sessionID := createTestSession(ctx, t, admin)
	orphan, standIn := lockingStandIn(ctx, t, connStr, sessionID)
	release := holdExitOf(ctx, t, connStr, standIn)

	timeouts := lockTestShippedTimeouts(t)
	logs := captureDefaultLoggerJSONSync(t)
	r := newLockTestRegistry(ctx, t, newLockTestPool(ctx, t, connStr, 2), timeouts)
	r.locks.setOrphanForTest(orphan)
	r.locks.startDial()

	waitUntil(t, timeouts.ActorLockOrphanTerminateWait+timeouts.ActorLockStatementTimeout+5*time.Second,
		func() bool { return r.locks.backendPIDForTest() != 0 })
	outlasted := waitForLogEntry(t, logs, 5*time.Second, logOrphanOutlastedWait)
	if got, want := outlasted["wait"], float64(timeouts.ActorLockOrphanTerminateWait); got != want {
		t.Errorf("the log's wait = %v, want %v (nanoseconds)", got, want)
	}
	if got, ok := outlasted["orphan_backend_pid"].(float64); !ok || int32(got) != orphan.pid {
		t.Errorf("the log's orphan_backend_pid = %v, want %d", outlasted["orphan_backend_pid"], orphan.pid)
	}
	outlastedAt, establishedAt := logIndex(t, logs, logOrphanOutlastedWait), logIndex(t, logs, logLockConnEstablished)
	if establishedAt < outlastedAt {
		t.Fatalf("log order: %q at line %d, %q at line %d; want the wait's end first", logOrphanOutlastedWait, outlastedAt, logLockConnEstablished, establishedAt)
	}
	if i := logIndex(t, logs, logLockDialFailed); i >= 0 {
		t.Fatalf("a lock dial failed: log line %d", i)
	}
	if got := r.locks.orphanForTest(); got != nil {
		t.Fatalf("the orphan %+v is still recorded after a dial let it go", *got)
	}

	if !lockBackendAlive(ctx, t, admin, uint32(orphan.pid)) {
		t.Fatalf("the orphan %d is gone while its exit is still held back", orphan.pid)
	}
	if _, err := r.GetOrSpawn(ctx, sessionID); !errors.Is(err, ErrSessionActorElsewhere) {
		t.Fatalf("GetOrSpawn for the session the orphan still holds = %v, want ErrSessionActorElsewhere", err)
	}
	release()
	waitUntil(t, 5*time.Second, func() bool {
		_, err := r.GetOrSpawn(ctx, sessionID)
		return err == nil
	})
}
