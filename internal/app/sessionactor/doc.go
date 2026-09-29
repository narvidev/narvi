// Package sessionactor implements the one-goroutine-plus-mailbox actor per
// active session named in §2: "one goroutine + mailbox (channel of
// commands) per active session. All mutations of a session's state go
// through its actor — no other code path writes session/sandbox/turn
// rows." This is the first app-layer package in the repo (as opposed to
// internal/domain): it MAY use time.Now() and real Postgres I/O — §11's
// "Never put I/O, time.Now(), or randomness in /internal/domain" is a
// domain-specific rule — but every
// duration/interval it reads still comes from a platform.Timeouts value —
// never a literal (enforced repo-wide by notimeliteral) — and depends on
// internal/domain/{sandbox,turn,session} for every actual state-transition
// or status-derivation DECISION; this package's own job is orchestration
// and persistence around those decisions, not reimplementing them.
//
// # Components
//
//   - Registry (registry.go): the process-wide, mutex-guarded supervisor
//     that hydrates and starts at most one local *Actor per session id
//     (concurrent spawns of one session share one hydration), and reaps
//     it on eviction/failure. GetOrSpawn is the only entry point external
//     callers need.
//   - hydrateAndAcquire (hydrate.go): the advisory-lock + epoch-bump +
//     initial-state-load sequence run once per (session, process) pairing
//     that actually wins ownership (§2: "Postgres advisory lock keyed by
//     session id, held for the actor's lifetime, plus a fencing check"),
//     bounded by ActorHydrateTimeout.
//   - lockHolder (lockholder.go): the ONE dedicated connection, outside
//     the query pool, on which a replica holds every one of its actors'
//     advisory locks -- so hosting sessions never takes a query
//     connection (§5.1) -- with the generation that lets a lost connection
//     stop exactly the actors whose locks it held.
//   - Actor (actor.go): the per-session mailbox loop and its
//     epoch-fenced transactional-write helper (§2: "state transition +
//     appended event + outbox entries commit in ONE Postgres
//     transaction").
//   - Command (command.go): the mailbox's message type. TimerFired
//     (delivered by the timer pump) was the only variant at first; the
//     sandbox WS hub's own SandboxEvent (§3.2, one per inbound sandbox
//     frame) and EnsureDispatched (§9.3, "please re-evaluate this
//     session's own spawn/dispatch state now") have since joined it.
//   - The timer pump (timerpump.go): a single process-wide poll loop
//     (§2: "A per-pod timer pump polls due timers (SELECT ... FOR UPDATE
//     SKIP LOCKED) and delivers them as actor commands"). Its sibling,
//     the lock probe (Registry.RunLockProbe), keeps the lock connection:
//     dials it at start, probes it on its own interval, and dials again
//     when none is open.
//   - TimerFired handling (timerfired.go): the decision+write logic for
//     each of the 5 named timers (§2: connecting_deadline, liveness_check,
//     inactivity, turn_deadline, terminal_grace) — see that file's own
//     doc comment for exactly which of the 5 are fully wired in this Step
//     and why.
//
// # Concurrency
//
// Every goroutine this package starts (an actor's mailbox loop, the
// timer-pump and lock-probe loops) is launched via errgroup.Group.Go, never a bare `go`
// statement (§11, enforced repo-wide by the nakedgoroutine lint check —
// no exemption for this package). Registry's own actor-lifecycle group is
// deliberately a zero-value errgroup.Group (no errgroup.WithContext): a
// zero-value Group provides Go/Wait with no shared-cancellation-on-error
// behavior, which is exactly what's wanted here — one session's actor
// hitting ErrStaleEpoch and returning an error must never cancel every
// OTHER session's actor sharing the same process. Cancellation of an
// individual actor's own loop is instead driven by that actor's own
// context, derived from the Registry's lifecycle context (cancelled on
// process-wide Registry.Shutdown) and cancelled on its own when the lock
// connection holding its advisory lock is lost (cause errLockLost, a clean
// stop), or by an idle-TTL timer internal to the actor's own select loop.
package sessionactor
