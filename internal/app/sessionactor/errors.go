package sessionactor

import "errors"

var (
	// ErrSessionActorElsewhere is returned by Registry.GetOrSpawn when
	// this process failed to win the session's Postgres advisory lock
	// (pg_try_advisory_lock returned false): another pod currently owns
	// this session's actor -- or this process's own previous actor for it
	// is still shutting down and has not yet released the lock (the window
	// between Actor.shutdown's eviction and its unlock; lockholder.go's
	// held check). Two goroutines of this process racing to spawn the SAME
	// session never see it: Registry.GetOrSpawn deduplicates them
	// (singleflight), so they share one hydration and one Actor.
	// §2's fail-fast requirement: GetOrSpawn never blocks waiting for the
	// lock, so a caller in a later Step can route the request to whichever
	// pod actually holds it instead of hanging.
	ErrSessionActorElsewhere = errors.New("sessionactor: session actor is owned elsewhere")

	// ErrActorUnavailable is returned by Registry.GetOrSpawn when this
	// replica could not hydrate the session's actor in time: the query
	// pool stayed saturated for the whole ActorHydrateTimeout bound, or no
	// working lock connection (lockholder.go) could be had within it -- none
	// could be dialled in time, or the one open was lost and a retry on a
	// new one failed too -- or a server-side error failed the lock
	// statement itself. Retryable, and distinct from
	// ErrSessionActorElsewhere: nobody else is known to own the session --
	// this replica just cannot host it right now. Any lock the attempt took
	// has already been released by the time it is returned. Each caller's
	// answer to it is stated at its own call site (§2): the timer pump stops
	// its batch, the sandbox WS handshake answers 503 with Retry-After, and
	// a post-commit dispatch trigger only logs it.
	ErrActorUnavailable = errors.New("sessionactor: session actor unavailable on this replica; retry")

	// ErrStaleEpoch is returned by Actor.transact when the epoch read back
	// from the session row (inside the write's own transaction, via
	// GetSessionActorEpochForUpdate) no longer matches the epoch this
	// Actor remembers from its own hydration -- proof a newer actor has
	// since taken over this session (§2: "writes with a stale epoch
	// fail. A zombie actor on an old pod can never corrupt state"). This
	// is fatal to the Actor that receives it: see Actor.run, which evicts
	// itself from the Registry and stops processing further commands the
	// moment this error surfaces from a command handler.
	ErrStaleEpoch = errors.New("sessionactor: stale actor epoch")

	// ErrActorStopped is returned by Actor.Send when the actor's mailbox
	// loop has already exited (idle-TTL eviction, ErrStaleEpoch, or
	// process shutdown) or has been told to stop (process shutdown, or its
	// replica lost the lock connection holding its advisory lock) -- the
	// caller should treat this the same as "no live actor", i.e. call
	// Registry.GetOrSpawn again to hydrate a fresh one if the work still
	// needs doing.
	ErrActorStopped = errors.New("sessionactor: actor has stopped")

	// errLockLost is the cancellation cause Registry.onLockLost gives
	// every actor locked under a lock connection that was just lost. It is
	// a clean stop, not a failure: Actor.run returns nil for it, so the
	// Registry's errgroup still only ever reports shutdown's own
	// cancellation.
	errLockLost = errors.New("sessionactor: this replica lost the connection holding the session's advisory lock")
)

// SpawnFailureReason names why Registry.GetOrSpawn failed, for a caller
// that only logs the failure (a post-commit dispatch trigger): the row it
// was triggered for is already committed, so the reason is what an
// operator reading the log needs -- "actor_unavailable" (this replica was
// saturated or had lost its lock connection; retryable),
// "actor_elsewhere" (another replica owns the session), or "error".
func SpawnFailureReason(err error) string {
	switch {
	case errors.Is(err, ErrActorUnavailable):
		return "actor_unavailable"
	case errors.Is(err, ErrSessionActorElsewhere):
		return "actor_elsewhere"
	default:
		return "error"
	}
}
