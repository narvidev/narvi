package sessionactor

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/platform"
)

// lockConnApplicationNamePrefix begins the application_name every lock
// connection reports, so pg_stat_activity (and, by its pid, pg_locks) shows
// which backends hold replicas' actor locks. Each dial appends a nonce of
// its own (newLockConnApplicationName), so the name identifies one
// connection's session and no other: terminateOrphan relies on that.
const lockConnApplicationNamePrefix = "narvi-actor-locks-"

// newLockConnApplicationName returns a fresh application_name for one lock
// connection: the prefix and 26 random characters (at least 128 bits), 44
// bytes in all, inside the 63 Postgres keeps.
func newLockConnApplicationName() string {
	return lockConnApplicationNamePrefix + strings.ToLower(rand.Text())
}

// lockProbeQuery is ProbeOnce's statement: it only proves the connection,
// and so every lock on it, is still alive.
const lockProbeQuery = `SELECT 1`

// lockConnKeepaliveQuery asks the server to apply the lock connection's own
// TCP settings to its end of the connection (prepareConn). All four are
// ordinary session settings, which any role may set.
const lockConnKeepaliveQuery = `SELECT set_config('tcp_keepalives_idle', $1, false),
	set_config('tcp_keepalives_interval', $2, false),
	set_config('tcp_keepalives_count', $3, false),
	set_config('tcp_user_timeout', $4, false)`

// lockBackendQuery reads the lock connection's own backend as the server
// knows it: its pid, and its start time, which is NULL only if the server
// no longer reports it.
const lockBackendQuery = `SELECT pg_backend_pid(),
	(SELECT backend_start FROM pg_stat_activity WHERE pid = pg_backend_pid())`

// terminateOrphanQuery terminates a lost lock connection's backend if it is
// still running that connection's session (terminateOrphan). The pid and
// start time name the backend -- a pid alone may since have been reused by
// an unrelated one -- but not the session: behind a session-mode pooler
// the backend outlives it, reset and handed to whichever client the pooler
// links next, possibly this replica's new lock connection or another
// replica's. So the match is also on the lost connection's own
// application_name, unique to its dial: a session-mode pooler sets each
// client's own application_name on the backend it links (PgBouncer tracks
// it), so a backend since handed to any other client is spared. And never
// the backend running the statement, whatever its name. A role may signal
// its own backends.
const terminateOrphanQuery = `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
	WHERE pid = $1 AND backend_start = $2 AND application_name = $3 AND datname = current_database()
		AND pid <> pg_backend_pid()`

// errLockHolderClosed is TryLock's error once Registry.Shutdown has closed
// the holder: a Registry that has shut down hosts nothing.
var errLockHolderClosed = errors.New("sessionactor: lock connection closed: registry shut down")

// lockHolder holds every session advisory lock this replica's actors own
// (§2: "Postgres advisory lock keyed by session id, held for the actor's
// lifetime"), on ONE dedicated Postgres connection outside the query pool
// (§5.1): hosting more sessions never takes a query connection, whatever
// the replica hosts. It used to be one pooled connection per live actor,
// held for the actor's whole life -- enough live sessions filled the pool,
// and the next hydration then waited, with no bound, for a connection only
// an idle-out could free, while every other query on the replica waited
// behind it.
//
// One backend can hold any number of advisory locks, but sharing one
// brings three traps, each handled here:
//
//  1. Advisory locks are re-entrant per backend: a second
//     pg_try_advisory_lock from the backend that already holds the lock
//     succeeds and stacks. held records every session locked here, and
//     TryLock refuses one already in it, so two actors of this replica can
//     never both "win". (Registry.GetOrSpawn's singleflight keeps two
//     goroutines from even trying; held covers the window where a dying
//     actor has left the Registry's map but not yet unlocked.)
//  2. Shared fate: losing the connection drops every lock on it at once.
//     A statement that finds the connection gone -- it is closed, the
//     error is not the server's (a network error), the server's is FATAL
//     or PANIC, or the statement overran its bound -- presumes it lost
//     (statementLost): it is closed, the generation is bumped, held is
//     cleared, onLost stops every actor locked under the old generation,
//     and a new connection is dialled at once. A server-side ERROR on a
//     live connection (the shared lock table full, say) fails only that
//     statement's caller -- except on an unlock, where any error presumes
//     the connection lost (releaseEntered). Registry.RunLockProbe finds a
//     silent loss within ActorLockProbeInterval; before it does, a TryLock
//     that finds the connection it was handed already dead retries once
//     on a new one.
//  3. A pgx.Conn is not safe for concurrent use: every statement runs
//     while holding sem, and each is bounded by ActorLockStatementTimeout.
//
// Closing a lost connection releases its locks only once the server hears
// of it. When the network itself failed, it may never: a partition that
// outlasts TCP's retransmission of the close leaves the backend idle on the
// server, still holding every lock, until the server's own TCP keepalives
// give up on it -- over two hours at Postgres's defaults, during which no
// replica, this one included, can host any of those sessions: each looks
// owned elsewhere. Two things bound that. Every lock connection asks the
// server for short keepalives on its own end (prepareConn), so the server
// reaps such a backend within ActorLockServerReapTime even if this replica
// never comes back. And the first connection dialled after a loss
// terminates the lost one's backend if it is still running the lost
// connection's session (terminateOrphan), so a replica that does come back
// does not wait for that. Every dial names its connection afresh
// (newLockConnApplicationName) for that match: behind a session-mode
// pooler a backend outlives the session that locked on it.
//
// A statement runs on its own bound, never under its caller's
// cancellation: pgconn answers a cancelled context by breaking the
// connection, which here would drop every other actor's lock too. TryLock
// and ProbeOnce give up waiting for sem when their caller's context ends,
// and TryLock waiting for a dial does too; Unlock waits for sem whatever
// its context (Unlock's own doc comment).
//
// The connection is dialled the way the query pool dials one of its own
// (dialAndInstall): on its own goroutine, on the Registry's lifecycle
// context rather than on whoever asked for it, one host after another
// under each host's own connect_timeout -- or ActorLockConnectAttemptTimeout
// when the URL sets none. A caller only waits for the dial, within its own
// bound, and a dial it gave up on still lands for the next caller -- so a
// hung first host of a multi-host URL costs the dial one connect timeout,
// not every hydration forever. And a dial started while the network drops
// packets silently ends within that timeout of its start, so the next
// hydration or probe after a heal dials afresh instead of waiting on a
// connect stuck in the kernel's retransmission backoff.
type lockHolder struct {
	// Dial settings, fixed at construction: a copy of the query pool's
	// own connection configuration -- TLS, runtime parameters such as
	// search_path, and every target host with its connect_timeout -- and
	// its connect hooks, applied as pgxpool applies them. connectAttempt
	// bounds each connect attempt when the URL sets no connect_timeout
	// (ActorLockConnectAttemptTimeout).
	connConfig     *pgx.ConnConfig
	beforeConnect  func(context.Context, *pgx.ConnConfig) error
	afterConnect   func(context.Context, *pgx.Conn) error
	connectAttempt time.Duration
	stmtTimeout    time.Duration

	// serverKeepalives is what prepareConn asks the server to apply to its
	// end of every lock connection, from the ActorLockServer* timeouts.
	serverKeepalives serverKeepalives

	// onLost is told, outside sem, every time a connection is lost; it
	// stops the actors locked under loss.gen (Registry.onLockLost).
	onLost func(ctx context.Context, loss lockLoss)

	// tryLockSQL is TryLock's statement: tryAdvisoryLockQuery, replaced
	// only by a test that injects a server-side error. Read under sem.
	tryLockSQL string

	// terminateSQL is terminateOrphan's statement: terminateOrphanQuery,
	// replaced only by a test that holds it back to observe what the dial
	// does meanwhile. Set before anything dials; read by the dial.
	terminateSQL string

	// dialCtx is every dial's context: the Registry's lifecycle, never a
	// caller's. dials runs each dial on its own goroutine, so a caller can
	// stop waiting without cancelling it; Close stops and waits for them.
	// dialMu guards dialing, the one dial in flight if any, and
	// dialsStopped. It is never held together with sem.
	dialCtx      context.Context
	stopDials    context.CancelFunc
	dials        errgroup.Group
	dialMu       sync.Mutex
	dialing      *lockDial
	dialsStopped bool

	// sem is the mutex guarding conn, held and closed: a one-slot channel
	// rather than a sync.Mutex, so a hydration waiting its turn gives up
	// when its own bound ends instead of queueing past it.
	sem    chan struct{}
	conn   *pgx.Conn
	held   map[pgtype.UUID]uint64
	closed bool

	// backend is the open connection's backend, read at its dial (nil if
	// that read failed). orphan is the backend of the last connection
	// lost, which the server may still be keeping, until the next dial has
	// dealt with it (terminateOrphan): a connection is only ever installed
	// once the terminate of the orphan before it has returned. Both
	// guarded by sem.
	backend *lockBackend
	orphan  *lockBackend

	// gen is the current generation: bumped on every loss (and on Close),
	// so an actor records the generation it was locked under and a
	// late Unlock from an earlier one is a no-op. Written only while
	// holding sem; read without it, so Registry can compare an actor's
	// generation under its own mutex without nesting the two.
	gen atomic.Uint64
}

// lockLoss describes one lost lock connection, for onLost.
type lockLoss struct {
	gen   uint64 // the generation the lost connection held locks under
	held  int    // how many locks it held
	cause error
}

// lockDial is one dial of the lock connection. err is set before done is
// closed: nil once a connection is open.
type lockDial struct {
	done chan struct{}
	err  error
}

// lockBackend is one lock connection's backend on the server: its pid and
// its start time, which together name the backend for the server's whole
// life, and the application_name that connection's dial set, which names
// its session on that backend (terminateOrphanQuery).
type lockBackend struct {
	pid     int32
	start   time.Time
	appName string
}

// serverKeepalives are lockConnKeepaliveQuery's four arguments, each in the
// unit the server reads a bare number in: tcp_keepalives_idle and
// tcp_keepalives_interval in seconds, tcp_keepalives_count, and
// tcp_user_timeout in milliseconds.
type serverKeepalives struct {
	idle, interval, count, userTimeout string
}

// newServerKeepalives renders the ActorLockServer* timeouts as the server's
// four settings; the user timeout is the reap time itself, so a connection
// waiting for an acknowledgement when the network failed is reaped in the
// same time as an idle one. Validate refuses every value that would reach
// the server as 0, the system default.
func newServerKeepalives(t platform.Timeouts) serverKeepalives {
	return serverKeepalives{
		idle:        strconv.Itoa(int(platform.DurationToSeconds(t.ActorLockServerKeepaliveIdle))),
		interval:    strconv.Itoa(int(platform.DurationToSeconds(t.ActorLockServerKeepaliveInterval))),
		count:       strconv.Itoa(t.ActorLockServerKeepaliveCount),
		userTimeout: strconv.FormatInt(t.ActorLockServerReapTime().Milliseconds(), 10),
	}
}

// newLockHolder builds a holder that dials, on first need, a connection
// configured exactly like one of pool's own, but reporting an
// application_name of its own (newLockConnApplicationName). Nothing is
// dialled here: a Registry that never hydrates an actor, and never runs its
// lock probe, never opens one. ctx is the Registry's lifecycle context;
// every dial runs on it.
func newLockHolder(ctx context.Context, pool *pgxpool.Pool, timeouts platform.Timeouts, onLost func(context.Context, lockLoss)) *lockHolder {
	poolConfig := pool.Config() // a copy: nothing here can touch the pool's own
	dialCtx, stopDials := context.WithCancel(ctx)
	h := &lockHolder{
		connConfig:       poolConfig.ConnConfig,
		beforeConnect:    poolConfig.BeforeConnect,
		afterConnect:     poolConfig.AfterConnect,
		connectAttempt:   timeouts.ActorLockConnectAttemptTimeout,
		stmtTimeout:      timeouts.ActorLockStatementTimeout,
		serverKeepalives: newServerKeepalives(timeouts),
		onLost:           onLost,
		tryLockSQL:       tryAdvisoryLockQuery,
		terminateSQL:     terminateOrphanQuery,
		dialCtx:          dialCtx,
		stopDials:        stopDials,
		sem:              make(chan struct{}, 1),
		held:             make(map[pgtype.UUID]uint64),
	}
	h.gen.Store(1)
	return h
}

// currentGen is the generation a lock taken now is held under.
func (h *lockHolder) currentGen() uint64 { return h.gen.Load() }

// enter takes sem, giving up if ctx ends first; leave gives it back.
func (h *lockHolder) enter(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (h *lockHolder) leave() { <-h.sem }

// statementContext bounds one statement on the lock connection by its own
// ActorLockStatementTimeout, keeping ctx's values but never its
// cancellation -- see lockHolder's doc comment for why.
func (h *lockHolder) statementContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), h.stmtTimeout)
}

// statementLost reports whether a statement on the lock connection that
// failed with err means the connection itself is presumed lost, and every
// lock on it with it: the connection is closed, the statement overran its
// bound (pgconn breaks a connection whose statement it abandons), the
// error did not come from the server (a network error: the connection's
// state is unknown), or the server's error is FATAL or PANIC (the backend
// is ending). Any other server error -- severity ERROR, such as the shared
// lock table being full (53200) -- leaves the backend and every lock it
// holds as they were: it fails that one statement only. Lock, probe and
// the statements that ready a new connection follow this rule; an unlock
// does not (releaseEntered).
func statementLost(connClosed, boundExpired bool, err error) bool {
	if connClosed || boundExpired {
		return true
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return true
	}
	severity := pgErr.SeverityUnlocalized
	if severity == "" {
		severity = pgErr.Severity
	}
	switch severity {
	case "FATAL", "PANIC":
		return true
	}
	return false
}

// TryLock takes sessionID's advisory lock on the lock connection and
// returns the generation the lock is held under, waiting -- within ctx --
// for a connection to be dialled if none is open. ok=false with a nil
// error means the lock is not this caller's to take -- another backend
// holds it, or this replica already does (held) -- which is
// ErrSessionActorElsewhere either way. An error means no connection could
// be had within ctx, the statement failed (on a lost connection, every
// actor locked under it is being stopped), or the holder is closed.
//
// A connection that was already open when this call began may have died
// long before this statement found out -- a Postgres restart or failover,
// a terminated backend, a reset -- which the query pool would have hidden
// by pinging it first. So a loss found on one is retried exactly once, on
// a connection dialled for the retry, still within ctx; a failure on a
// freshly dialled connection is not.
func (h *lockHolder) TryLock(ctx context.Context, sessionID pgtype.UUID) (gen uint64, ok bool, err error) {
	dialled, retried := false, false
	for {
		if err := h.enter(ctx); err != nil {
			return 0, false, fmt.Errorf("sessionactor: wait for the lock connection: %w", err)
		}
		res := h.tryLockEntered(ctx, sessionID)
		h.leave()
		h.report(ctx, res.loss)

		switch {
		case res.noConn:
			// No connection open: wait for one below.
		case res.loss != nil && !dialled && !retried:
			retried = true
		default:
			return res.gen, res.ok, res.err
		}
		if err := h.awaitConn(ctx); err != nil {
			if res.err != nil {
				return 0, false, fmt.Errorf("%w; then, retrying on a new connection: %w", res.err, err)
			}
			return 0, false, err
		}
		dialled = true
	}
}

// tryLockResult is one tryLockEntered attempt's outcome.
type tryLockResult struct {
	gen    uint64
	ok     bool
	noConn bool      // no connection was open: nothing ran
	loss   *lockLoss // the connection was lost: report it
	err    error
}

func (h *lockHolder) tryLockEntered(ctx context.Context, sessionID pgtype.UUID) tryLockResult {
	if h.closed {
		return tryLockResult{err: errLockHolderClosed}
	}
	gen := h.gen.Load()
	if _, held := h.held[sessionID]; held {
		return tryLockResult{gen: gen}
	}
	if h.conn == nil {
		return tryLockResult{noConn: true}
	}

	sctx, cancel := h.statementContext(ctx)
	defer cancel()
	var locked bool
	err := h.conn.QueryRow(sctx, h.tryLockSQL, sessionID.String()).Scan(&locked)
	switch {
	case err == nil && locked:
		h.held[sessionID] = gen
		return tryLockResult{gen: gen, ok: true}
	case err == nil:
		return tryLockResult{gen: gen}
	}
	err = fmt.Errorf("sessionactor: try advisory lock: %w", err)
	if statementLost(h.conn.IsClosed(), sctx.Err() != nil, err) {
		return tryLockResult{loss: h.loseEntered(err), err: err}
	}
	// A server-side ERROR on a live connection fails this hydration only:
	// the connection, and every other actor's lock on it, stays. The
	// statement may still have taken the lock before it failed, and an
	// advisory lock outlives the statement that took it, so it is released
	// here. The session is not in held, so this backend holds no other
	// lock on it: the release undoes this statement's own lock, or is a
	// no-op.
	_, loss := h.releaseEntered(ctx, sessionID)
	return tryLockResult{loss: loss, err: err}
}

// awaitConn waits, until ctx ends, for a dial of the lock connection to
// finish, starting one if none is in flight. The dial itself never runs
// under ctx: if ctx ends first, it goes on, and its connection is there
// for the next caller.
func (h *lockHolder) awaitConn(ctx context.Context) error {
	d := h.startDial()
	select {
	case <-d.done:
		if d.err != nil {
			return fmt.Errorf("sessionactor: dial lock connection: %w", d.err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("sessionactor: wait for the lock connection to be dialled: %w", context.Cause(ctx))
	}
}

// startDial returns the dial in flight, starting one if there is none. A
// dial started while a connection is already open finds it and ends at
// once, so asking for one is always safe.
func (h *lockHolder) startDial() *lockDial {
	h.dialMu.Lock()
	defer h.dialMu.Unlock()
	if h.dialing != nil {
		return h.dialing
	}
	d := &lockDial{done: make(chan struct{})}
	if h.dialsStopped {
		d.err = errLockHolderClosed
		close(d.done)
		return d
	}
	h.dialing = d
	h.dials.Go(func() error {
		err := h.dialAndInstall()
		if err != nil && h.dialCtx.Err() == nil {
			platform.Logger(h.dialCtx).Warn("sessionactor: could not dial the lock connection", "error", err)
		}
		h.dialMu.Lock()
		h.dialing = nil
		h.dialMu.Unlock()
		d.err = err
		close(d.done)
		return nil
	})
	return d
}

// dialAndInstall dials the lock connection and makes it the open one,
// unless one is already open or the holder has closed. First, from the new
// connection, it terminates the backend the last lost connection may have
// left behind, and installs the new one only once that has returned. The
// dial runs outside sem: statements on an open connection never wait for
// it.
func (h *lockHolder) dialAndInstall() error {
	ctx := h.dialCtx
	if err := h.enter(ctx); err != nil {
		return err
	}
	open, closed, orphan := h.conn != nil, h.closed, h.orphan
	h.leave()
	switch {
	case closed:
		return errLockHolderClosed
	case open:
		return nil
	}

	conn, appName, backend, err := h.dial(ctx)
	if err != nil {
		return err
	}
	if orphan != nil {
		if err := h.terminateOrphan(ctx, conn, *orphan); err != nil {
			h.closeConn(conn)
			return err
		}
	}

	if err := h.enter(ctx); err != nil {
		h.closeConn(conn)
		return err
	}
	defer h.leave()
	if h.closed {
		h.closeConn(conn)
		return errLockHolderClosed
	}
	// h.conn is nil here: only a dial opens a connection, and this is the
	// only dial in flight. So h.orphan is still the one read above: only
	// losing an open connection sets it.
	h.conn, h.backend, h.orphan = conn, backend, nil
	// The server's own pid when it could be read: behind a pooler, the one
	// pgconn was handed at startup is the pooler's.
	pid := int64(conn.PgConn().PID())
	if backend != nil {
		pid = int64(backend.pid)
	}
	platform.Logger(ctx).Info("sessionactor: lock connection established",
		"backend_pid", pid, "application_name", appName, "generation", h.gen.Load())
	return nil
}

// dial opens one connection as pgxpool opens one of its own -- a copy of
// the pool's configuration, every host tried in turn under its own
// connect_timeout (connectAttempt when the URL sets none), the pool's
// BeforeConnect and AfterConnect hooks, and no other bound than ctx -- under
// an application_name of its own (newLockConnApplicationName), then readies
// it (prepareConn) and returns it with that name and its backend, if that
// could be read.
func (h *lockHolder) dial(ctx context.Context) (*pgx.Conn, string, *lockBackend, error) {
	cc := h.connConfig.Copy()
	if cc.ConnectTimeout <= 0 {
		cc.ConnectTimeout = h.connectAttempt
	}
	if h.beforeConnect != nil {
		if err := h.beforeConnect(ctx, cc); err != nil {
			return nil, "", nil, fmt.Errorf("before connect: %w", err)
		}
	}
	if cc.RuntimeParams == nil {
		cc.RuntimeParams = map[string]string{}
	}
	appName := newLockConnApplicationName()
	cc.RuntimeParams["application_name"] = appName

	conn, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		return nil, "", nil, err
	}
	if h.afterConnect != nil {
		if err := h.afterConnect(ctx, conn); err != nil {
			h.closeConn(conn)
			return nil, "", nil, fmt.Errorf("after connect: %w", err)
		}
	}
	backend, err := h.prepareConn(ctx, conn, appName)
	if err != nil {
		h.closeConn(conn)
		return nil, "", nil, err
	}
	return conn, appName, backend, nil
}

// prepareConn readies a freshly dialled lock connection, not yet in use,
// with two statements, each under its own ActorLockStatementTimeout and
// ctx.
//
// First it asks the server to apply the lock connection's keepalives to
// its end of the connection (the ActorLockServer* timeouts), so a backend
// whose client vanished is reaped within ActorLockServerReapTime rather
// than hours (lockHolder's doc comment). They override any value the
// database URL sets: the URL's still apply to every query-pool connection,
// but on this one the reap time is a bound Validate ties to other
// timeouts. They are set by a statement, not as startup parameters, since a
// session-mode pooler -- supported (§5.1) -- refuses a startup parameter it
// does not track, which would leave no lock connection at all.
//
// Then it reads the connection's backend, which terminateOrphan needs if
// this connection is lost: the pid the server reports, not the one pgconn
// was handed at startup, which a pooler may have made up; its start time;
// and appName, the application_name this connection's dial set.
//
// A statement that loses the connection (statementLost) fails the dial,
// and the next one starts afresh. One the server refuses is logged and
// passed over -- without the keepalives, or without the backend, a lost
// connection's backend lasts as long as the server's own keepalives let
// it -- since failing every dial on it would leave the replica unable to
// host any session at all.
func (h *lockHolder) prepareConn(ctx context.Context, conn *pgx.Conn, appName string) (*lockBackend, error) {
	logger := platform.Logger(ctx)
	k := h.serverKeepalives

	kctx, cancel := context.WithTimeout(ctx, h.stmtTimeout)
	_, err := conn.Exec(kctx, lockConnKeepaliveQuery, k.idle, k.interval, k.count, k.userTimeout)
	expired := kctx.Err() != nil
	cancel()
	if err != nil {
		if statementLost(conn.IsClosed(), expired, err) {
			return nil, fmt.Errorf("set the lock connection's server keepalives: %w", err)
		}
		logger.Warn("sessionactor: the server refused the lock connection's keepalives; a lock backend it loses touch with lasts as long as its own keepalives allow",
			"error", err)
	}

	bctx, cancel := context.WithTimeout(ctx, h.stmtTimeout)
	defer cancel()
	var (
		pid   int32
		start pgtype.Timestamptz
	)
	if err := conn.QueryRow(bctx, lockBackendQuery).Scan(&pid, &start); err != nil {
		if statementLost(conn.IsClosed(), bctx.Err() != nil, err) {
			return nil, fmt.Errorf("read the lock connection's backend: %w", err)
		}
		logger.Warn("sessionactor: could not read the lock connection's backend; if this connection is lost, its backend is left to the server's keepalives",
			"error", err)
		return nil, nil
	}
	if !start.Valid {
		logger.Warn("sessionactor: the server reports no start time for the lock connection's backend; if this connection is lost, its backend is left to the server's keepalives",
			"backend_pid", pid)
		return nil, nil
	}
	return &lockBackend{pid: pid, start: start.Time, appName: appName}, nil
}

// terminateOrphan terminates, from conn -- freshly dialled, not yet in
// use -- the backend of the lock connection lost before it, if that backend
// is still running the lost connection's session (terminateOrphanQuery):
// when the loss was the network's, the server -- or a pooler in between --
// never heard the lost connection close, and would keep every lock it held
// until keepalives reaped it. No such backend is the ordinary case: the
// server heard the close, or ended the backend itself, or a pooler heard it
// and has reset the backend, releasing the locks, and perhaps handed it to
// another client since. It fails -- and so fails the dial, leaving the
// orphan to the next one -- only when conn itself is lost. Anything else is
// logged, and the orphan left to the server's keepalives: it never holds up
// the new connection longer than one statement.
func (h *lockHolder) terminateOrphan(ctx context.Context, conn *pgx.Conn, orphan lockBackend) error {
	sctx, cancel := context.WithTimeout(ctx, h.stmtTimeout)
	defer cancel()
	logger := platform.Logger(ctx).With("orphan_backend_pid", orphan.pid, "orphan_backend_start", orphan.start,
		"orphan_application_name", orphan.appName)

	var terminated bool
	err := conn.QueryRow(sctx, h.terminateSQL, orphan.pid, orphan.start, orphan.appName).Scan(&terminated)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		logger.Info("sessionactor: no backend was left running the lost lock connection's session")
	case err == nil && terminated:
		logger.Warn("sessionactor: terminated the backend a lost lock connection left behind, still holding its advisory locks")
	case err == nil:
		logger.Info("sessionactor: the lost lock connection's backend ended before it could be terminated")
	case statementLost(conn.IsClosed(), sctx.Err() != nil, err):
		return fmt.Errorf("terminate the lost lock connection's backend: %w", err)
	default:
		logger.Warn("sessionactor: could not terminate the backend a lost lock connection left behind; the server's keepalives will end it",
			"error", err)
	}
	return nil
}

// Unlock releases sessionID's advisory lock if, and only if, it is still
// held under gen: a lock from an earlier generation went with that
// generation's connection, and the same session may since have been
// locked again under the current one -- unlocking it then would hand
// another replica the session under a live actor. Never gives up on ctx:
// an unlock that has to happen must not be skipped because its caller's
// bound ran out. An earlier generation's unlock returns at once, without
// waiting its turn: generations only grow, so it would be a no-op anyway.
func (h *lockHolder) Unlock(ctx context.Context, sessionID pgtype.UUID, gen uint64) {
	if gen != h.gen.Load() {
		return
	}
	ctx = context.WithoutCancel(ctx)
	h.sem <- struct{}{}
	loss := h.unlockEntered(ctx, sessionID, gen)
	h.leave()
	h.report(ctx, loss)
}

func (h *lockHolder) unlockEntered(ctx context.Context, sessionID pgtype.UUID, gen uint64) *lockLoss {
	if h.closed || h.conn == nil || gen != h.gen.Load() {
		return nil
	}
	if heldGen, held := h.held[sessionID]; !held || heldGen != gen {
		return nil
	}
	delete(h.held, sessionID)

	unlocked, loss := h.releaseEntered(ctx, sessionID)
	if loss == nil && !unlocked {
		platform.Logger(ctx).Warn("sessionactor: advisory unlock found no lock to release on the lock connection",
			"session_id", sessionID.String(), "generation", gen)
	}
	return loss
}

// releaseEntered runs pg_advisory_unlock for sessionID on the open
// connection. Unlike a failed lock or probe, a failed release presumes the
// connection lost whatever the error: it may or may not have released the
// lock, and closing the connection is the one release that is certain --
// a lock left behind would keep the session from every other replica, and
// a later lock from this backend would stack on it.
func (h *lockHolder) releaseEntered(ctx context.Context, sessionID pgtype.UUID) (bool, *lockLoss) {
	sctx, cancel := h.statementContext(ctx)
	defer cancel()
	var unlocked bool
	if err := h.conn.QueryRow(sctx, advisoryUnlockQuery, sessionID.String()).Scan(&unlocked); err != nil {
		return false, h.loseEntered(fmt.Errorf("sessionactor: advisory unlock: %w", err))
	}
	return unlocked, nil
}

// ProbeOnce proves the lock connection is still alive, if there is one,
// and starts a dial in the background if there is none. A probe that finds
// the connection lost loses it: every actor locked under it is stopped
// before ProbeOnce returns the error.
func (h *lockHolder) ProbeOnce(ctx context.Context) error {
	if err := h.enter(ctx); err != nil {
		return err
	}
	noConn, loss, err := h.probeEntered(ctx)
	h.leave()
	h.report(ctx, loss)
	if noConn {
		h.startDial()
	}
	return err
}

func (h *lockHolder) probeEntered(ctx context.Context) (noConn bool, loss *lockLoss, err error) {
	if h.closed {
		return false, nil, nil
	}
	if h.conn == nil {
		return true, nil, nil
	}
	sctx, cancel := h.statementContext(ctx)
	defer cancel()
	if _, err := h.conn.Exec(sctx, lockProbeQuery); err != nil {
		err = fmt.Errorf("sessionactor: probe lock connection: %w", err)
		if statementLost(h.conn.IsClosed(), sctx.Err() != nil, err) {
			return false, h.loseEntered(err), err
		}
		return false, nil, err
	}
	return false, nil, nil
}

// loseEntered presumes the lock connection lost: closes it, records its
// backend as the orphan the next dial terminates if the server still keeps
// it, clears held, bumps the generation, and returns what was lost for
// report to hand to onLost once sem is released.
func (h *lockHolder) loseEntered(cause error) *lockLoss {
	loss := &lockLoss{gen: h.gen.Load(), held: len(h.held), cause: cause}
	if h.conn != nil {
		h.closeConn(h.conn)
		h.conn = nil
		h.orphan, h.backend = h.backend, nil
	}
	clear(h.held)
	h.gen.Add(1)
	return loss
}

// closeConn closes conn, bounded. Postgres releases every advisory lock a
// backend holds when its session ends, but the session ends only once the
// server learns of the close, and on a failed network it may never:
// pgconn gives up on a connection whose statement overran without a word
// reaching the server, and a close sent into a partition that outlasts
// TCP's retransmission is lost. That backend then keeps every lock until
// the server's keepalives reap it or the next lock connection terminates
// it (lockHolder's doc comment); closing is only this side's half.
func (h *lockHolder) closeConn(conn *pgx.Conn) {
	cctx, cancel := context.WithTimeout(context.Background(), h.stmtTimeout)
	defer cancel()
	_ = conn.Close(cctx)
}

// report hands a loss to onLost, outside sem, then starts dialling a new
// connection at once, in the background, so the next hydration finds one
// open.
func (h *lockHolder) report(ctx context.Context, loss *lockLoss) {
	if loss == nil {
		return
	}
	if h.onLost != nil {
		h.onLost(ctx, *loss)
	}
	h.startDial()
}

// Close closes the lock connection for good (Registry.Shutdown, once every
// actor has stopped), releasing any lock still on it: it stops any dial in
// flight, waits for it, and starts no other. Idempotent.
func (h *lockHolder) Close() {
	h.dialMu.Lock()
	h.dialsStopped = true
	h.dialMu.Unlock()
	h.stopDials()
	_ = h.dials.Wait()

	h.sem <- struct{}{}
	defer h.leave()
	h.closed = true
	if h.conn != nil {
		h.closeConn(h.conn)
		h.conn, h.backend = nil, nil
	}
	clear(h.held)
	h.gen.Add(1)
}
