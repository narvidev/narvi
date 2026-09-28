package sessionactor

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

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
//     Any statement that fails or overruns its bound means the connection
//     is presumed lost: it is closed, the generation is bumped, held is
//     cleared, and onLost stops every actor locked under the old
//     generation. Registry.RunLockProbe finds a silent loss within
//     ActorLockProbeInterval.
//  3. A pgx.Conn is not safe for concurrent use: every statement runs
//     while holding sem, and each is bounded by ActorLockStatementTimeout.
//
// A statement runs on its own bound, never under its caller's
// cancellation: pgconn answers a cancelled context by breaking the
// connection, which here would drop every other actor's lock too. Only the
// wait for sem, and the dial, honour the caller's context.
type lockHolder struct {
	// Dial settings, fixed at construction: the query pool's own
	// connection settings (TLS, runtime parameters such as search_path,
	// target hosts) and connect hooks, so the lock connection reaches the
	// same database the same way a pool connection would.
	connConfig    *pgx.ConnConfig
	beforeConnect func(context.Context, *pgx.ConnConfig) error
	afterConnect  func(context.Context, *pgx.Conn) error
	stmtTimeout   time.Duration

	// onLost is told, outside sem, every time a connection is lost; it
	// stops the actors locked under loss.gen (Registry.onLockLost).
	onLost func(ctx context.Context, loss lockLoss)

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

// newLockHolder builds a holder that will dial, lazily on first use, a
// connection configured exactly like one of pool's own, but reporting
// application_name narvi-actor-locks. Nothing is dialled here: a Registry
// that never hydrates an actor never opens one.
func newLockHolder(pool *pgxpool.Pool, stmtTimeout time.Duration, onLost func(context.Context, lockLoss)) *lockHolder {
	poolConfig := pool.Config() // a copy: nothing here can touch the pool's own
	h := &lockHolder{
		connConfig:    poolConfig.ConnConfig,
		beforeConnect: poolConfig.BeforeConnect,
		afterConnect:  poolConfig.AfterConnect,
		stmtTimeout:   stmtTimeout,
		onLost:        onLost,
		sem:           make(chan struct{}, 1),
		held:          make(map[pgtype.UUID]uint64),
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

// TryLock takes sessionID's advisory lock on the lock connection, dialling
// one first if the replica has none, and returns the generation the lock
// is held under. ok=false with a nil error means the lock is not this
// caller's to take -- another backend holds it, or this replica already
// does (held) -- which is ErrSessionActorElsewhere either way. An error
// means the connection could not be dialled, was lost (and every actor
// locked under it is being stopped), or the holder is closed; ctx ending
// while waiting for sem is one too.
func (h *lockHolder) TryLock(ctx context.Context, sessionID pgtype.UUID) (gen uint64, ok bool, err error) {
	if err := h.enter(ctx); err != nil {
		return 0, false, fmt.Errorf("sessionactor: wait for the lock connection: %w", err)
	}
	gen, ok, loss, err := h.tryLockEntered(ctx, sessionID)
	h.leave()
	h.report(ctx, loss)
	return gen, ok, err
}

func (h *lockHolder) tryLockEntered(ctx context.Context, sessionID pgtype.UUID) (uint64, bool, *lockLoss, error) {
	if h.closed {
		return 0, false, nil, errLockHolderClosed
	}
	gen := h.gen.Load()
	if _, held := h.held[sessionID]; held {
		return gen, false, nil, nil
	}
	conn, err := h.connEntered(ctx)
	if err != nil {
		return 0, false, nil, err
	}

	sctx, cancel := h.statementContext(ctx)
	defer cancel()
	var locked bool
	if err := conn.QueryRow(sctx, tryAdvisoryLockQuery, sessionID.String()).Scan(&locked); err != nil {
		err = fmt.Errorf("sessionactor: try advisory lock: %w", err)
		return 0, false, h.loseEntered(err), err
	}
	if !locked {
		return gen, false, nil, nil
	}
	h.held[sessionID] = gen
	return gen, true, nil, nil
}

// connEntered returns the open lock connection, dialling one -- bounded by
// ActorLockStatementTimeout and by ctx -- if there is none. A failed dial
// loses nothing: no lock is held without a connection.
func (h *lockHolder) connEntered(ctx context.Context) (*pgx.Conn, error) {
	if h.conn != nil {
		return h.conn, nil
	}
	dctx, cancel := context.WithTimeout(ctx, h.stmtTimeout)
	defer cancel()

	cc := h.connConfig.Copy()
	if h.beforeConnect != nil {
		if err := h.beforeConnect(dctx, cc); err != nil {
			return nil, fmt.Errorf("sessionactor: dial lock connection: before connect: %w", err)
		}
	}
	if cc.RuntimeParams == nil {
		cc.RuntimeParams = map[string]string{}
	}
	cc.RuntimeParams["application_name"] = lockConnApplicationName

	conn, err := pgx.ConnectConfig(dctx, cc)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: dial lock connection: %w", err)
	}
	if h.afterConnect != nil {
		if err := h.afterConnect(dctx, conn); err != nil {
			h.closeConn(conn)
			return nil, fmt.Errorf("sessionactor: dial lock connection: after connect: %w", err)
		}
	}
	h.conn = conn
	platform.Logger(ctx).Info("sessionactor: lock connection established",
		"backend_pid", conn.PgConn().PID(), "generation", h.gen.Load())
	return conn, nil
}

// Unlock releases sessionID's advisory lock if, and only if, it is still
// held under gen: a lock from an earlier generation went with that
// generation's connection, and the same session may since have been
// locked again under the current one -- unlocking it then would hand
// another replica the session under a live actor. Never gives up on ctx:
// an unlock that has to happen must not be skipped because its caller's
// bound ran out.
func (h *lockHolder) Unlock(ctx context.Context, sessionID pgtype.UUID, gen uint64) {
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

	sctx, cancel := h.statementContext(ctx)
	defer cancel()
	var unlocked bool
	if err := h.conn.QueryRow(sctx, advisoryUnlockQuery, sessionID.String()).Scan(&unlocked); err != nil {
		return h.loseEntered(fmt.Errorf("sessionactor: advisory unlock: %w", err))
	}
	if !unlocked {
		platform.Logger(ctx).Warn("sessionactor: advisory unlock found no lock to release on the lock connection",
			"session_id", sessionID.String(), "generation", gen)
	}
	return nil
}

// ProbeOnce proves the lock connection is still alive, if there is one. A
// failed probe loses it: every actor locked under it is stopped before
// ProbeOnce returns the error.
func (h *lockHolder) ProbeOnce(ctx context.Context) error {
	if err := h.enter(ctx); err != nil {
		return err
	}
	loss, err := h.probeEntered(ctx)
	h.leave()
	h.report(ctx, loss)
	return err
}

func (h *lockHolder) probeEntered(ctx context.Context) (*lockLoss, error) {
	if h.closed || h.conn == nil {
		return nil, nil
	}
	sctx, cancel := h.statementContext(ctx)
	defer cancel()
	if _, err := h.conn.Exec(sctx, lockProbeQuery); err != nil {
		err = fmt.Errorf("sessionactor: probe lock connection: %w", err)
		return h.loseEntered(err), err
	}
	return nil, nil
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

// report hands a loss to onLost, outside sem.
func (h *lockHolder) report(ctx context.Context, loss *lockLoss) {
	if loss != nil && h.onLost != nil {
		h.onLost(ctx, *loss)
	}
}

// Close closes the lock connection for good (Registry.Shutdown, once every
// actor has stopped), releasing any lock still on it. Idempotent.
func (h *lockHolder) Close() {
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
