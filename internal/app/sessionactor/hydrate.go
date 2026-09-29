package sessionactor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

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
// only ever run on this replica's one lock connection (lockholder.go).
const (
	tryAdvisoryLockQuery = `SELECT pg_try_advisory_lock(hashtextextended($1::text, 0))`
	advisoryUnlockQuery  = `SELECT pg_advisory_unlock(hashtextextended($1::text, 0))`
)

var (
	// errHydrateBoundExpired is a hydration context's cause once
	// ActorHydrateTimeout has run out.
	errHydrateBoundExpired = errors.New("sessionactor: hydration exceeded ActorHydrateTimeout")

	// errRegistryShutdown is a hydration context's cause once the Registry
	// has shut down under it.
	errRegistryShutdown = errors.New("sessionactor: registry shut down")
)

// hydrateAndAcquire is the acquisition sequence (§2: hydration on demand,
// single-writer advisory lock, epoch bumped on each acquisition), run once
// per (session, process) pairing that actually wins ownership, all of it
// under ONE bound, ActorHydrateTimeout (hydrationContext):
//  1. take the session's advisory lock on this replica's lock connection
//     (lockholder.go) -- never on a query-pool connection, so hosting a
//     session never takes one -- failing fast with ErrSessionActorElsewhere
//     if another owner already holds it;
//  2. bump the actor epoch, then load the session/sandbox/turn rows to
//     hydrate initial state: four short pool statements, each taking one
//     connection and giving it back, never two at once.
//
// Any failure after step 1 releases the lock before returning. A failure
// the bound or the lock connection caused is ErrActorUnavailable
// (retryable); any other failure is returned as it is.
func (r *Registry) hydrateAndAcquire(ctx context.Context, sessionID pgtype.UUID) (*Actor, error) {
	hctx, cancel := r.hydrationContext(ctx)
	defer cancel()

	lockGen, locked, err := r.locks.TryLock(hctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("%w: take advisory lock: %w", ErrActorUnavailable, err)
	}
	if !locked {
		return nil, ErrSessionActorElsewhere
	}

	fail := func(step string, err error) error {
		r.locks.Unlock(hctx, sessionID, lockGen)
		if hctx.Err() != nil {
			return fmt.Errorf("%w: %s: %w: %w", ErrActorUnavailable, step, context.Cause(hctx), err)
		}
		return fmt.Errorf("sessionactor: %s: %w", step, err)
	}

	epoch, err := r.stores.session.BumpActorEpoch(hctx, sessionID)
	if err != nil {
		return nil, fail("bump actor epoch", err)
	}

	sessionRow, err := r.stores.session.Get(hctx, sessionID)
	if err != nil {
		return nil, fail("get session", err)
	}

	hasSandbox := true
	if _, err := r.stores.sandbox.Get(hctx, sessionID); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("get sandbox", err)
		}
		hasSandbox = false
	}

	turns, err := r.stores.turn.ListForSession(hctx, sessionID)
	if err != nil {
		return nil, fail("list turns", err)
	}

	logger := platform.Logger(ctx).With("session_id", sessionID.String(), "actor_epoch", epoch)
	logger.Info("sessionactor: hydrated",
		"session_status", string(sessionRow.Status),
		"has_sandbox", hasSandbox,
		"turn_count", len(turns),
		"lock_generation", lockGen,
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
		lockGen:                lockGen,
		mailbox:                make(chan Command, mailboxBufferSize),
		done:                   make(chan struct{}),
		logger:                 logger,
	}, nil
}

// hydrationContext derives a hydration's own context from the caller's:
// it keeps the caller's values (its logger, its correlation id) but not
// its cancellation -- a hydration that other callers joined
// (Registry.GetOrSpawn's singleflight) must not die with whichever caller
// happened to start it -- and adds the two things that do end it:
// ActorHydrateTimeout, and the Registry's own shutdown.
func (r *Registry) hydrationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	base, cancelBase := context.WithCancelCause(context.WithoutCancel(ctx))
	stopWatchingShutdown := context.AfterFunc(r.lifecycleCtx, func() { cancelBase(errRegistryShutdown) })
	hctx, cancelBound := context.WithTimeoutCause(base, r.timeouts.ActorHydrateTimeout, errHydrateBoundExpired)
	return hctx, func() {
		cancelBound()
		stopWatchingShutdown()
		cancelBase(context.Canceled)
	}
}
