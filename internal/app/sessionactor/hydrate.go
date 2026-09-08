package sessionactor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/platform"
)

// tryAdvisoryLockQuery / advisoryUnlockQuery hash the session id's string
// form into the single bigint key pg_try_advisory_lock/pg_advisory_unlock
// want (§2: "Postgres advisory lock keyed by session id").
// hashtextextended (with a 0 seed) is used rather than hashtext because
// hashtext returns a 32-bit integer -- collapsing the key space to 2^32,
// where a birthday collision between two concurrently-live sessions stops
// being negligible at large fleet sizes (a collision wrongly reports
// "actor elsewhere" for a session nobody owns, freezing its timers until
// the colliding actor evicts) -- while hashtextextended returns the full
// 64-bit bigint the lock functions natively take, at identical cost.
// pg_advisory_unlock MUST run on the exact same connection/session that
// took the lock -- Postgres advisory locks are scoped to the backend
// session holding them, not to any client-side handle -- so both queries
// are only ever run against Actor.lockConn / the connection
// hydrateAndAcquire is currently holding.
const (
	tryAdvisoryLockQuery = `SELECT pg_try_advisory_lock(hashtextextended($1::text, 0))`
	advisoryUnlockQuery  = `SELECT pg_advisory_unlock(hashtextextended($1::text, 0))`
)

// hydrateAndAcquire is the acquisition sequence (§2: hydration on demand,
// single-writer advisory lock, epoch bumped on each acquisition) run once
// per (session, process) pairing that actually wins ownership:
//  1. acquire a DEDICATED pool connection whose only job for the rest of
//     the Actor's life is holding the advisory lock;
//  2. non-blocking pg_try_advisory_lock on it -- fail fast with
//     ErrSessionActorElsewhere if another owner already holds it;
//  3. bump the actor epoch via a short-lived, separately pool-acquired
//     statement -- NOT on the lock connection;
//  4. load the session/sandbox/turn rows to hydrate initial state.
//
// Any failure after step 2 releases the lock and its connection before
// returning, since by that point this call already owns both.
func (r *Registry) hydrateAndAcquire(ctx context.Context, sessionID pgtype.UUID) (*Actor, error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: acquire lock connection: %w", err)
	}

	var locked bool
	if err := conn.QueryRow(ctx, tryAdvisoryLockQuery, sessionID.String()).Scan(&locked); err != nil {
		conn.Release()
		return nil, fmt.Errorf("sessionactor: try advisory lock: %w", err)
	}
	if !locked {
		conn.Release()
		return nil, ErrSessionActorElsewhere
	}

	epoch, err := r.stores.session.BumpActorEpoch(ctx, sessionID)
	if err != nil {
		unlockAndRelease(ctx, conn, sessionID)
		return nil, fmt.Errorf("sessionactor: bump actor epoch: %w", err)
	}

	sessionRow, err := r.stores.session.Get(ctx, sessionID)
	if err != nil {
		unlockAndRelease(ctx, conn, sessionID)
		return nil, fmt.Errorf("sessionactor: get session: %w", err)
	}

	hasSandbox := true
	if _, err := r.stores.sandbox.Get(ctx, sessionID); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			unlockAndRelease(ctx, conn, sessionID)
			return nil, fmt.Errorf("sessionactor: get sandbox: %w", err)
		}
		hasSandbox = false
	}

	turns, err := r.stores.turn.ListForSession(ctx, sessionID)
	if err != nil {
		unlockAndRelease(ctx, conn, sessionID)
		return nil, fmt.Errorf("sessionactor: list turns: %w", err)
	}

	logger := platform.Logger(ctx).With("session_id", sessionID.String(), "actor_epoch", epoch)
	logger.Info("sessionactor: hydrated",
		"session_status", string(sessionRow.Status),
		"has_sandbox", hasSandbox,
		"turn_count", len(turns),
	)

	return &Actor{
		sessionID:              sessionID,
		epoch:                  epoch,
		pool:                   r.pool,
		timeouts:               r.timeouts,
		stores:                 r.stores,
		broadcaster:            r.broadcaster,
		commander:              r.commander,
		provider:               r.provider,
		publicBaseURL:          r.publicBaseURL,
		sourceControl:          r.sourceControl,
		tokenEncryptionKey:     r.tokenEncryptionKey,
		openCodeRuntimeVersion: r.openCodeRuntimeVersion,
		diffFetcher:            r.diffFetcher,
		reviewDiffFetcher:      r.reviewDiffFetcher,
		knowledgeRanker:        r.knowledgeRanker,
		githubBotToken:         r.githubBotToken,
		githubBotHandle:        r.githubBotHandle,
		reviewModelDeep:        r.reviewModelDeep,
		contractDriftDetected:  r.contractDriftDetected,
		opsMetrics:             r.opsMetrics,
		repoAccessCache:        r.repoAccessCache,
		epistemicCheckDefault:  r.epistemicCheckDefault,
		rolloutMode:            r.rolloutMode,
		registry:               r,
		lockConn:               conn,
		mailbox:                make(chan Command, mailboxBufferSize),
		done:                   make(chan struct{}),
		logger:                 logger,
	}, nil
}

// unlockAndRelease releases the session advisory lock held on conn and
// returns conn to the pool. If the unlock statement itself fails, conn is
// force-closed first: releasing a connection that MIGHT still hold the
// advisory lock back to the pool as-is risks silently leaking that lock
// for however long the pool goes on reusing this connection --
// pg_advisory_unlock only works on the very backend session that took the
// lock, and pgxpool.Conn.Release only discards a connection it can itself
// detect as broken (closed, busy, or mid-transaction). Force-closing here
// guarantees Postgres drops every advisory lock held by this backend the
// moment the connection itself terminates, regardless of why the unlock
// statement failed.
func unlockAndRelease(ctx context.Context, conn *pgxpool.Conn, sessionID pgtype.UUID) {
	if _, err := conn.Exec(ctx, advisoryUnlockQuery, sessionID.String()); err != nil {
		platform.Logger(ctx).Error(
			"sessionactor: advisory unlock failed; force-closing connection to guarantee the lock is released",
			"error", err, "session_id", sessionID.String(),
		)
		_ = conn.Conn().Close(context.Background())
	}
	conn.Release()
}
