package sessionactor

import (
	"context"
	"errors"
	"fmt"
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

// lockConnApplicationName is the application_name the lock connection
// reports, so pg_stat_activity and pg_locks show which backend holds a
// replica's actor locks.
const lockConnApplicationName = "narvi-actor-locks"

// lockProbeQuery is ProbeOnce's statement: it only proves the connection,
// and so every lock on it, is still alive.
const lockProbeQuery = `SELECT 1`

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
//     statement's caller. Registry.RunLockProbe finds a silent loss within
//     ActorLockProbeInterval; before it does, a TryLock that finds the
//     connection it was handed already dead retries once on a new one.
//  3. A pgx.Conn is not safe for concurrent use: every statement runs
//     while holding sem, and each is bounded by ActorLockStatementTimeout.
//
// A statement runs on its own bound, never under its caller's
// cancellation: pgconn answers a cancelled context by breaking the
// connection, which here would drop every other actor's lock too. Only the
// wait for sem, and the wait for a dial, honour the caller's context.
//
// The connection is dialled the way the query pool dials one of its own
// (dialAndInstall): on its own goroutine, on the Registry's lifecycle
// context rather than on whoever asked for it, one host after another
// under each host's own connect_timeout. A caller only waits for the dial,
// within its own bound, and a dial it gave up on still lands for the next
// caller -- so a hung first host of a multi-host URL costs the dial one
// connect_timeout, as it costs the pool, not every hydration forever.
type lockHolder struct {
	// Dial settings, fixed at construction: a copy of the query pool's
	// own connection configuration -- TLS, runtime parameters such as
	// search_path, and every target host with its connect_timeout -- and
	// its connect hooks, applied as pgxpool applies them. connectFallback
	// is pgxpool's own per-host connect timeout for a URL that sets none
	// (ActorLockConnectTimeoutFallback).
	connConfig      *pgx.ConnConfig
	beforeConnect   func(context.Context, *pgx.ConnConfig) error
	afterConnect    func(context.Context, *pgx.Conn) error
	connectFallback time.Duration
	stmtTimeout     time.Duration

	// onLost is told, outside sem, every time a connection is lost; it
	// stops the actors locked under loss.gen (Registry.onLockLost).
	onLost func(ctx context.Context, loss lockLoss)

	// tryLockSQL is TryLock's statement: tryAdvisoryLockQuery, replaced
	// only by a test that injects a server-side error. Read under sem.
	tryLockSQL string

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

// newLockHolder builds a holder that dials, on first need, a connection
// configured exactly like one of pool's own, but reporting
// application_name narvi-actor-locks. Nothing is dialled here: a Registry
// that never hydrates an actor, and never runs its lock probe, never opens
// one. ctx is the Registry's lifecycle context; every dial runs on it.
func newLockHolder(ctx context.Context, pool *pgxpool.Pool, timeouts platform.Timeouts, onLost func(context.Context, lockLoss)) *lockHolder {
	poolConfig := pool.Config() // a copy: nothing here can touch the pool's own
	dialCtx, stopDials := context.WithCancel(ctx)
	h := &lockHolder{
		connConfig:      poolConfig.ConnConfig,
		beforeConnect:   poolConfig.BeforeConnect,
		afterConnect:    poolConfig.AfterConnect,
		connectFallback: timeouts.ActorLockConnectTimeoutFallback,
		stmtTimeout:     timeouts.ActorLockStatementTimeout,
		onLost:          onLost,
		tryLockSQL:      tryAdvisoryLockQuery,
		dialCtx:         dialCtx,
		stopDials:       stopDials,
		sem:             make(chan struct{}, 1),
		held:            make(map[pgtype.UUID]uint64),
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
// holds as they were: it fails that one statement only.
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
// unless one is already open or the holder has closed. The dial runs
// outside sem: statements on an open connection never wait for it.
func (h *lockHolder) dialAndInstall() error {
	ctx := h.dialCtx
	if err := h.enter(ctx); err != nil {
		return err
	}
	open, closed := h.conn != nil, h.closed
	h.leave()
	switch {
	case closed:
		return errLockHolderClosed
	case open:
		return nil
	}

	conn, err := h.dial(ctx)
	if err != nil {
		return err
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
	// only dial in flight.
	h.conn = conn
	platform.Logger(ctx).Info("sessionactor: lock connection established",
		"backend_pid", conn.PgConn().PID(), "generation", h.gen.Load())
	return nil
}

// dial opens one connection as pgxpool opens one of its own: a copy of
// the pool's configuration, every host tried in turn under its own
// connect_timeout (connectFallback when the URL sets none), the pool's
// BeforeConnect and AfterConnect hooks, and no other bound than ctx.
func (h *lockHolder) dial(ctx context.Context) (*pgx.Conn, error) {
	cc := h.connConfig.Copy()
	if cc.ConnectTimeout <= 0 {
		cc.ConnectTimeout = h.connectFallback
	}
	if h.beforeConnect != nil {
		if err := h.beforeConnect(ctx, cc); err != nil {
			return nil, fmt.Errorf("before connect: %w", err)
		}
	}
	if cc.RuntimeParams == nil {
		cc.RuntimeParams = map[string]string{}
	}
	cc.RuntimeParams["application_name"] = lockConnApplicationName

	conn, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		return nil, err
	}
	if h.afterConnect != nil {
		if err := h.afterConnect(ctx, conn); err != nil {
			h.closeConn(conn)
			return nil, fmt.Errorf("after connect: %w", err)
		}
	}
	return conn, nil
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

// loseEntered presumes the lock connection lost: closes it, clears held,
// bumps the generation, and returns what was lost for report to hand to
// onLost once sem is released.
func (h *lockHolder) loseEntered(cause error) *lockLoss {
	loss := &lockLoss{gen: h.gen.Load(), held: len(h.held), cause: cause}
	if h.conn != nil {
		h.closeConn(h.conn)
		h.conn = nil
	}
	clear(h.held)
	h.gen.Add(1)
	return loss
}

// closeConn closes conn, bounded: Postgres releases every advisory lock
// the backend held the moment its session ends.
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
		h.conn = nil
	}
	clear(h.held)
	h.gen.Add(1)
}
