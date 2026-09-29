//go:build integration

// Integration tests for the lock connection (lockholder.go) behind a
// session-mode connection pooler, which §5.1 supports: there a server
// backend outlives the client session that locked on it, and the terminate
// that follows a loss must end only a backend still running the lost
// connection's session. Same rules as lockholder_integration_test.go.
package sessionactor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/platform"
)

// sessionPooler is a minimal session-mode connection pooler between lock
// connections and Postgres, standing in for PgBouncer at its defaults
// (pool_mode=session, server_reset_query=DISCARD ALL,
// server_round_robin=0). A client is linked to a server backend on its
// first message and keeps it until it goes. A client that goes while its
// backend is idle has it reset with DISCARD ALL -- which releases every
// advisory lock on it -- and kept for the next client, the backend
// released last handed out first; one that goes mid-statement has its
// backend closed. A backend that dies takes its client with it. The pooler
// reports a made-up backend pid to its clients, as PgBouncer does.
//
// With setsClientName, as PgBouncer does with application_name, a tracked
// parameter, each backend is opened with no application_name and SET to
// each client's own on every link. Without it, a backend is opened under
// the application_name of the client it was opened for and keeps it,
// reset or not, whoever it is handed to next: a pooler that forwards a
// client's startup parameters when it opens a backend, and tracks none.
type sessionPooler struct {
	ln             net.Listener
	serverDSN      string
	setsClientName bool

	mu      sync.Mutex
	idle    []*poolerServer // the backend released last is last
	servers []*poolerServer // every backend opened, for close
	clients []*poolerClient
	nextPID uint32
	closed  bool

	g errgroup.Group
}

// poolerServer is one backend the pooler opened, with its own reader: it
// relays what the backend sends to the linked client, or completes the
// pooler's own statement (exec).
type poolerServer struct {
	pid  uint32
	conn net.Conn
	fe   *pgproto3.Frontend // written by one goroutine at a time: the linked client's, or the one linking or releasing it

	mu       sync.Mutex
	client   *poolerClient // relayed to; nil while idle
	owed     int           // ReadyForQuery messages still owed to the linked client
	reply    chan error    // the pooler's own statement in flight, if any
	replyErr error
	dead     bool
}

// poolerClient is one client connection to the pooler.
type poolerClient struct {
	conn    net.Conn
	be      *pgproto3.Backend
	appName string
	wmu     sync.Mutex    // guards be's writes
	server  *poolerServer // the linked backend: the client's own goroutine's
}

// send writes msgs to the client and flushes.
func (c *poolerClient) send(msgs ...pgproto3.BackendMessage) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	for _, m := range msgs {
		c.be.Send(m)
	}
	return c.be.Flush()
}

// startSessionPooler listens on a local port and pools backends opened with
// serverDSN until the test ends.
func startSessionPooler(t *testing.T, serverDSN string, setsClientName bool) *sessionPooler {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &sessionPooler{ln: ln, serverDSN: serverDSN, setsClientName: setsClientName, nextPID: 900000}
	p.g.Go(p.accept)
	t.Cleanup(p.close)
	return p
}

func (p *sessionPooler) addr() string { return p.ln.Addr().String() }

func (p *sessionPooler) accept() error {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return nil
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = c.Close()
			return nil
		}
		p.g.Go(func() error {
			p.serveClient(c)
			return nil
		})
		p.mu.Unlock()
	}
}

// serveClient logs the client in, then relays every message it sends to
// its backend, linked on the first.
func (p *sessionPooler) serveClient(conn net.Conn) {
	be := pgproto3.NewBackend(conn, conn)
	var startup *pgproto3.StartupMessage
	for startup == nil {
		msg, err := be.ReceiveStartupMessage()
		if err != nil {
			_ = conn.Close()
			return
		}
		switch m := msg.(type) {
		case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
			if _, err := conn.Write([]byte{'N'}); err != nil {
				_ = conn.Close()
				return
			}
		case *pgproto3.StartupMessage:
			startup = m
		default: // a cancel request: nothing here runs long enough
			_ = conn.Close()
			return
		}
	}

	c := &poolerClient{conn: conn, be: be, appName: startup.Parameters["application_name"]}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = conn.Close()
		return
	}
	p.clients = append(p.clients, c)
	p.nextPID++
	fakePID := p.nextPID
	p.mu.Unlock()

	login := []pgproto3.BackendMessage{&pgproto3.AuthenticationOk{}}
	for name, value := range map[string]string{
		"server_version":              "17.0",
		"server_encoding":             "UTF8",
		"client_encoding":             "UTF8",
		"DateStyle":                   "ISO, MDY",
		"IntervalStyle":               "postgres",
		"integer_datetimes":           "on",
		"standard_conforming_strings": "on",
		"TimeZone":                    "UTC",
		"application_name":            c.appName,
	} {
		login = append(login, &pgproto3.ParameterStatus{Name: name, Value: value})
	}
	login = append(login,
		&pgproto3.BackendKeyData{ProcessID: fakePID, SecretKey: []byte{0, 0, 0, 1}},
		&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := c.send(login...); err != nil {
		p.clientGone(c)
		return
	}

	for {
		msg, err := be.Receive()
		if err != nil {
			p.clientGone(c)
			return
		}
		if _, ok := msg.(*pgproto3.Terminate); ok {
			p.clientGone(c)
			return
		}
		if c.server == nil {
			s, err := p.link(c)
			if err != nil {
				_ = c.send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: "08006", Message: "sessionPooler: " + err.Error()})
				p.clientGone(c)
				return
			}
			c.server = s
		}
		s := c.server
		switch msg.(type) {
		case *pgproto3.Sync, *pgproto3.Query:
			s.mu.Lock()
			s.owed++
			s.mu.Unlock()
		}
		s.fe.Send(msg)
		if err := s.fe.Flush(); err != nil {
			p.clientGone(c)
			return
		}
	}
}

// link hands c the idle backend released last, or opens one, and sets c's
// application_name on it if the pooler tracks it.
func (p *sessionPooler) link(c *poolerClient) (*poolerServer, error) {
	p.mu.Lock()
	var s *poolerServer
	if n := len(p.idle); n > 0 {
		s, p.idle = p.idle[n-1], p.idle[:n-1]
	}
	p.mu.Unlock()
	if s == nil {
		var err error
		if s, err = p.openServer(c.appName); err != nil {
			return nil, err
		}
	}
	if p.setsClientName {
		set := "RESET application_name"
		if c.appName != "" {
			set = "SET application_name = '" + strings.ReplaceAll(c.appName, "'", "''") + "'"
		}
		if err := s.exec(set); err != nil {
			p.dropServer(s)
			return nil, err
		}
	}
	s.mu.Lock()
	s.client, s.owed = c, 0
	s.mu.Unlock()
	return s, nil
}

// openServer opens a backend -- under appName when the pooler does not set
// each client's own -- and starts its reader.
func (p *sessionPooler) openServer(appName string) (*poolerServer, error) {
	u, err := url.Parse(p.serverDSN)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Del("application_name")
	if !p.setsClientName && appName != "" {
		q.Set("application_name", appName)
	}
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pc, err := pgconn.Connect(ctx, u.String())
	if err != nil {
		return nil, err
	}
	if err := pc.SyncConn(ctx); err != nil {
		_ = pc.Close(ctx)
		return nil, err
	}
	hc, err := pc.Hijack()
	if err != nil {
		_ = pc.Close(ctx)
		return nil, err
	}
	s := &poolerServer{pid: hc.PID, conn: hc.Conn, fe: hc.Frontend}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = s.conn.Close()
		return nil, errors.New("pooler closed")
	}
	p.servers = append(p.servers, s)
	p.g.Go(func() error {
		p.readServer(s)
		return nil
	})
	return s, nil
}

// readServer relays what s sends to its linked client, or completes the
// pooler's own statement, until s ends.
func (p *sessionPooler) readServer(s *poolerServer) {
	for {
		msg, err := s.fe.Receive()
		if err != nil {
			p.serverGone(s)
			return
		}
		s.mu.Lock()
		if s.reply != nil {
			switch m := msg.(type) {
			case *pgproto3.ErrorResponse:
				s.replyErr = fmt.Errorf("%s (SQLSTATE %s)", m.Message, m.Code)
			case *pgproto3.ReadyForQuery:
				reply, err := s.reply, s.replyErr
				s.reply, s.replyErr = nil, nil
				s.mu.Unlock()
				reply <- err
				continue
			}
			s.mu.Unlock()
			continue
		}
		c := s.client
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok && s.owed > 0 {
			s.owed--
		}
		s.mu.Unlock()
		if c != nil {
			_ = c.send(msg)
		}
	}
}

// exec runs the pooler's own statement on s, relaying nothing to a client.
func (s *poolerServer) exec(sql string) error {
	reply := make(chan error, 1)
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return errors.New("backend gone")
	}
	s.reply = reply
	s.mu.Unlock()
	s.fe.Send(&pgproto3.Query{String: sql})
	if err := s.fe.Flush(); err != nil {
		return err
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case err := <-reply:
		return err
	case <-timer.C:
		return fmt.Errorf("%q timed out", sql)
	}
}

// clientGone releases c's backend, if it has one: reset with DISCARD ALL
// and kept if it was idle, closed if not.
func (p *sessionPooler) clientGone(c *poolerClient) {
	_ = c.conn.Close()
	s := c.server
	c.server = nil
	if s == nil {
		return
	}
	s.mu.Lock()
	s.client = nil
	idle := s.owed == 0 && !s.dead
	s.mu.Unlock()
	if !idle {
		p.dropServer(s)
		return
	}
	if err := s.exec("DISCARD ALL"); err != nil {
		p.dropServer(s)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = s.conn.Close()
		return
	}
	p.idle = append(p.idle, s)
}

// serverGone forgets s, which has ended, and closes its client.
func (p *sessionPooler) serverGone(s *poolerServer) {
	s.mu.Lock()
	s.dead = true
	c, reply := s.client, s.reply
	s.client, s.reply = nil, nil
	s.mu.Unlock()
	if reply != nil {
		reply <- errors.New("backend gone")
	}
	p.dropServer(s)
	if c != nil {
		_ = c.conn.Close()
	}
}

// dropServer closes s and forgets it.
func (p *sessionPooler) dropServer(s *poolerServer) {
	s.mu.Lock()
	s.dead = true
	s.mu.Unlock()
	_ = s.conn.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.idle = slices.DeleteFunc(p.idle, func(i *poolerServer) bool { return i == s })
}

// hearClientGo closes, from the pooler's side, the connection of the client
// that logged in as appName -- as the pooler does when it hears the client
// go: a reset, a close that reaches it, or its own client keepalives
// reaping it. Its backend is released as for any client that goes.
func (p *sessionPooler) hearClientGo(t *testing.T, appName string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	found := false
	for _, c := range p.clients {
		if c.appName == appName {
			_ = c.conn.Close()
			found = true
		}
	}
	if !found {
		t.Fatalf("no pooler client logged in as %q", appName)
	}
}

// isIdle reports whether the backend pid is idle in the pool.
func (p *sessionPooler) isIdle(pid uint32) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.ContainsFunc(p.idle, func(s *poolerServer) bool { return s.pid == pid })
}

// close stops the pooler and closes every connection it holds.
func (p *sessionPooler) close() {
	_ = p.ln.Close()
	p.mu.Lock()
	p.closed = true
	var conns []net.Conn
	for _, c := range p.clients {
		conns = append(conns, c.conn)
	}
	for _, s := range p.servers {
		conns = append(conns, s.conn)
	}
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	_ = p.g.Wait()
}

// backendApplicationName reads pid's application_name on this test's
// database, "" if pid is not there.
func backendApplicationName(ctx context.Context, t *testing.T, admin *pgxpool.Pool, pid uint32) string {
	t.Helper()
	var names []string
	rows, err := admin.Query(ctx, `SELECT application_name FROM pg_stat_activity WHERE pid = $1 AND datname = current_database()`, int32(pid))
	if err != nil {
		t.Fatalf("read backend %d's application_name: %v", pid, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan backend %d's application_name: %v", pid, err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil || len(names) > 1 {
		t.Fatalf("read backend %d's application_name: %v (%d rows)", pid, err, len(names))
	}
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// poolerTestTimeouts is the shipped timeouts with a 1 s connect attempt.
// Through a pooler a connection's first statement also waits for the
// pooler to open and link a backend, so statements keep their shipped
// bound.
func poolerTestTimeouts(t *testing.T) platform.Timeouts {
	t.Helper()
	to := lockTestShippedTimeouts(t)
	to.ActorLockConnectAttemptTimeout = time.Second
	if err := to.Validate(); err != nil {
		t.Fatalf("test timeouts: %v", err)
	}
	return to
}

// newPoolerTestRegistry builds a Registry on pool whose lock connection
// goes to addr.
func newPoolerTestRegistry(ctx context.Context, t *testing.T, pool *pgxpool.Pool, addr string) *Registry {
	t.Helper()
	r := newLockTestRegistry(ctx, t, pool, poolerTestTimeouts(t))
	r.locks.dialViaForTest(t, addr)
	return r
}

// dialViaPooler builds a Registry on pool whose lock connection goes to
// addr, and waits for it to be dialled.
func dialViaPooler(ctx context.Context, t *testing.T, pool *pgxpool.Pool, addr string) *Registry {
	t.Helper()
	r := newPoolerTestRegistry(ctx, t, pool, addr)
	r.locks.startDial()
	waitUntil(t, 5*time.Second, func() bool { return r.locks.serverPIDForTest() != 0 })
	return r
}

// stillAliveForTest fails the test unless the lock backend pid stays alive
// for half a second: a terminated backend is gone within milliseconds.
func stillAliveForTest(ctx context.Context, t *testing.T, admin *pgxpool.Pool, pid uint32, what string) {
	t.Helper()
	for range 10 {
		if !lockBackendAlive(ctx, t, admin, pid) {
			t.Fatalf("%s: backend %d was terminated", what, pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestLockHolder_BehindASessionPooler (T25) runs the lock connection
// through sessionPooler and proves that the terminate after a loss ends a
// backend still running the lost connection's session, and no backend a
// pooler has since handed to another client. Behind a session-mode pooler
// a pid and a start time name a pooled backend, which outlives the client
// session that locked on it: once the pooler hears that client go, it
// resets the backend -- releasing the locks -- and hands it, the one
// released last first, to whichever client it links next. Matched on pid,
// start time and database alone, the terminate would then kill this
// replica's own redial, failing the dial, or another replica's live lock
// connection, dropping every lock it holds while its actors run on. Each
// dial's own application_name, which a pooler sets on the backend of each
// client it links, keeps the match to the lost session's own backend, and
// the terminate never picks the backend running it.
func TestLockHolder_BehindASessionPooler(t *testing.T) {
	t.Run("the redial is handed the lost connection's backend: it is not terminated", testPoolerRedialHandedTheLostBackend)
	t.Run("another replica's live lock connection is handed the lost connection's backend: it is not terminated", testPoolerAnotherReplicaHandedTheLostBackend)
	t.Run("the lost connection's backend still runs its session: it is terminated", testPoolerLostSessionStillLinked)
	t.Run("a pooler that sets no client's name: the redial handed the lost connection's backend still never terminates its own", testPoolerSetsNoClientName)
}

// testPoolerRedialHandedTheLostBackend: the pooler hears the lock
// connection go and releases its backend, then the replica finds the loss
// and redials, and the pooler hands the redial that same backend.
func testPoolerRedialHandedTheLostBackend(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	pooler := startSessionPooler(t, connStr, true)
	logs := captureDefaultLoggerJSONSync(t)
	r := dialViaPooler(ctx, t, newLockTestPool(ctx, t, connStr, 4), pooler.addr())
	x := createTestSession(ctx, t, admin)
	if _, err := r.GetOrSpawn(ctx, x); err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	lost, lostName := r.locks.serverPIDForTest(), r.locks.appNameForTest()
	if r.locks.backendPIDForTest() == lost {
		t.Fatalf("the pooler reported the backend's own pid %d: it is not standing in for a pooler", lost)
	}

	pooler.hearClientGo(t, lostName)
	waitUntil(t, 5*time.Second, func() bool { return pooler.isIdle(lost) })
	if got := advisoryLockHolders(ctx, t, admin, x); len(got) != 0 {
		t.Fatalf("after the pooler reset backend %d, the session's lock is held by pids %v, want none", lost, got)
	}
	if err := r.ProbeLockOnce(ctx); err == nil {
		t.Fatal("ProbeLockOnce on the connection the pooler closed = nil, want the loss")
	}

	waitUntil(t, 5*time.Second, func() bool { return r.locks.serverPIDForTest() != 0 })
	if got := r.locks.serverPIDForTest(); got != lost {
		t.Fatalf("the redial was linked to backend %d, want the lost connection's own %d, released last: the pooler model did not reuse it", got, lost)
	}
	if name := r.locks.appNameForTest(); name == lostName {
		t.Fatalf("the redial reports the lost connection's application_name %q, want one of its own", name)
	}
	if got := r.locks.orphanForTest(); got != nil {
		t.Fatalf("the orphan %+v is still recorded after the redial dealt with it", *got)
	}
	stillAliveForTest(ctx, t, admin, lost, "the redial's own backend, once the lost connection's")
	if i := logIndex(t, logs, "sessionactor: could not dial the lock connection"); i >= 0 {
		t.Fatalf("a lock dial failed: log line %d", i)
	}
	if _, err := r.GetOrSpawn(ctx, x); err != nil {
		t.Fatalf("GetOrSpawn on the redial: %v", err)
	}
	if got := advisoryLockHolders(ctx, t, admin, x); len(got) != 1 || got[0] != lost {
		t.Fatalf("the session's lock is held by pids %v, want only the redial's backend %d", got, lost)
	}
}

// testPoolerAnotherReplicaHandedTheLostBackend: the pooler hears replica
// A's lock connection go and releases its backend; replica B dials in that
// window -- booting in a rolling deploy, or redialling -- is handed it, and
// hydrates Z on it; only then does A find its loss and redial.
func testPoolerAnotherReplicaHandedTheLostBackend(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	pooler := startSessionPooler(t, connStr, true)
	pool := newLockTestPool(ctx, t, connStr, 4)
	a := dialViaPooler(ctx, t, pool, pooler.addr())
	b := newPoolerTestRegistry(ctx, t, pool, pooler.addr())
	x, z := createTestSession(ctx, t, admin), createTestSession(ctx, t, admin)
	if _, err := a.GetOrSpawn(ctx, x); err != nil {
		t.Fatalf("A's GetOrSpawn: %v", err)
	}
	lost, lostName := a.locks.serverPIDForTest(), a.locks.appNameForTest()

	pooler.hearClientGo(t, lostName)
	waitUntil(t, 5*time.Second, func() bool { return pooler.isIdle(lost) })
	b.locks.startDial()
	waitUntil(t, 5*time.Second, func() bool { return b.locks.serverPIDForTest() != 0 })
	if got := b.locks.serverPIDForTest(); got != lost {
		t.Fatalf("B's lock connection was linked to backend %d, want A's lost %d, released last: the pooler model did not reuse it", got, lost)
	}
	bActor, err := b.GetOrSpawn(ctx, z)
	if err != nil {
		t.Fatalf("B's GetOrSpawn: %v", err)
	}
	lostBefore := readCounterSum(ctx, t, otelReader, "session_actor_lock_conn_lost")

	if err := a.ProbeLockOnce(ctx); err == nil {
		t.Fatal("A's ProbeLockOnce on the connection the pooler closed = nil, want the loss")
	}
	waitUntil(t, 5*time.Second, func() bool { return a.locks.serverPIDForTest() != 0 })
	if got := a.locks.serverPIDForTest(); got == lost {
		t.Fatalf("A's redial was linked to backend %d, which B holds", got)
	}
	if got := a.locks.orphanForTest(); got != nil {
		t.Fatalf("A's orphan %+v is still recorded after its redial dealt with it", *got)
	}

	stillAliveForTest(ctx, t, admin, lost, "B's live lock connection, once A's lost one")
	if got := advisoryLockHolders(ctx, t, admin, z); len(got) != 1 || got[0] != lost {
		t.Fatalf("Z's lock is held by pids %v, want only B's backend %d", got, lost)
	}
	if err := b.ProbeLockOnce(ctx); err != nil {
		t.Fatalf("B's ProbeLockOnce = %v, want its lock connection alive", err)
	}
	if err := bActor.Send(ctx, TimerFired{Name: recoveryTestTimerName}); err != nil {
		t.Fatalf("Send to B's actor = %v, want it still running", err)
	}
	if got := readCounterSum(ctx, t, otelReader, "session_actor_lock_conn_lost") - lostBefore; got != 1 {
		t.Fatalf("session_actor_lock_conn_lost moved by %d, want 1: A's loss alone", got)
	}
	if _, err := a.GetOrSpawn(ctx, x); err != nil {
		t.Fatalf("A's GetOrSpawn on its redial: %v", err)
	}
}

// testPoolerLostSessionStillLinked: a partition between the replica and the
// pooler, which the pooler never hears, so its client stays linked and the
// backend keeps the session's lock; the redial after the heal is linked to
// a backend of its own and terminates that one.
func testPoolerLostSessionStillLinked(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	pooler := startSessionPooler(t, connStr, true)
	proxy := startPartitionProxy(t, pooler.addr())
	logs := captureDefaultLoggerJSONSync(t)
	r := dialViaPooler(ctx, t, newLockTestPool(ctx, t, connStr, 4), proxy.addr())
	x := createTestSession(ctx, t, admin)
	if _, err := r.GetOrSpawn(ctx, x); err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	lost, lostName := r.locks.serverPIDForTest(), r.locks.appNameForTest()

	proxy.partition()
	if err := r.ProbeLockOnce(ctx); err == nil {
		t.Fatal("ProbeLockOnce across the partition = nil, want the loss")
	}
	if got := advisoryLockHolders(ctx, t, admin, x); len(got) != 1 || got[0] != lost {
		t.Fatalf("after the loss, the session's lock is held by pids %v, want the lost connection's backend %d: the pooler heard it go", got, lost)
	}
	if got := backendApplicationName(ctx, t, admin, lost); got != lostName {
		t.Fatalf("the lost connection's backend reports application_name %q, want its own %q", got, lostName)
	}

	proxy.heal()
	waitUntil(t, 10*time.Second, func() bool {
		_, err := r.GetOrSpawn(ctx, x)
		return err == nil
	})
	newPID := r.locks.serverPIDForTest()
	if newPID == 0 || newPID == lost {
		t.Fatalf("the redial is on backend %d, want a new one (the lost one was %d)", newPID, lost)
	}
	if got := advisoryLockHolders(ctx, t, admin, x); len(got) != 1 || got[0] != newPID {
		t.Fatalf("the session's lock is held by pids %v, want only the redial's backend %d", got, newPID)
	}
	waitUntil(t, 5*time.Second, func() bool { return !lockBackendAlive(ctx, t, admin, lost) })
	if logIndex(t, logs, logOrphanTerminated) < 0 {
		t.Fatalf("no %q logged", logOrphanTerminated)
	}
}

// testPoolerSetsNoClientName: behind a pooler that sets no client's
// application_name, a backend keeps the name of the client it was opened
// for, so the redial handed the lost connection's backend runs under the
// lost connection's name, and only the rule that the terminate never picks
// its own backend keeps it alive.
func testPoolerSetsNoClientName(t *testing.T) {
	ctx := context.Background()
	admin, connStr := IntegrationTestPoolAndConnStr(t)
	pooler := startSessionPooler(t, connStr, false)
	logs := captureDefaultLoggerJSONSync(t)
	r := dialViaPooler(ctx, t, newLockTestPool(ctx, t, connStr, 4), pooler.addr())
	x := createTestSession(ctx, t, admin)
	if _, err := r.GetOrSpawn(ctx, x); err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	lost, lostName := r.locks.serverPIDForTest(), r.locks.appNameForTest()

	pooler.hearClientGo(t, lostName)
	waitUntil(t, 5*time.Second, func() bool { return pooler.isIdle(lost) })
	if err := r.ProbeLockOnce(ctx); err == nil {
		t.Fatal("ProbeLockOnce on the connection the pooler closed = nil, want the loss")
	}

	waitUntil(t, 5*time.Second, func() bool { return r.locks.serverPIDForTest() != 0 })
	if got := r.locks.serverPIDForTest(); got != lost {
		t.Fatalf("the redial was linked to backend %d, want the lost connection's own %d, released last: the pooler model did not reuse it", got, lost)
	}
	if got := backendApplicationName(ctx, t, admin, lost); got != lostName {
		t.Fatalf("the redial's backend reports application_name %q, want the lost connection's %q: the pooler model set the client's own", got, lostName)
	}
	stillAliveForTest(ctx, t, admin, lost, "the redial's own backend, under the lost connection's name")
	if i := logIndex(t, logs, "sessionactor: could not dial the lock connection"); i >= 0 {
		t.Fatalf("a lock dial failed: log line %d", i)
	}
	if _, err := r.GetOrSpawn(ctx, x); err != nil {
		t.Fatalf("GetOrSpawn on the redial: %v", err)
	}
}
