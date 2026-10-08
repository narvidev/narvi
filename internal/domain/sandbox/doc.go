// Package sandbox implements the sandbox state machine and the pure
// decision-function corpus that drives it (§3.2, "the critical machine"):
//
//   - The explicit Transition(from, currentGen, trigger) (to, error) table
//     (state.go), including gen fencing (stale-gen inputs rejected via a
//     distinguishable, wrapped sentinel error) and the "recover to whichever
//     state was live before suspecting" rule for the suspect/grace branch.
//   - The dead-sandbox policy (IsDeadSandboxStatus, deadstatus.go) and the
//     terminal-session reconciliation helper (ReconcileTerminalSandboxStatus,
//     terminalreconcile.go).
//   - The spawn/restore/resume decision tree (EvaluateSpawnDecision,
//     spawndecision.go).
//   - The circuit breaker (EvaluateCircuitBreaker, circuitbreaker.go).
//   - The two-budget liveness watchdogs (EvaluateConnectingTimeout for the
//     boot phases, EvaluateHeartbeatHealth for steady-state Ready,
//     liveness.go).
//   - The inactivity timeout (EvaluateInactivityTimeout, inactivity.go).
//   - The warm-on-type decision (EvaluateWarmDecision, warmdecision.go).
//   - The lifetime kind a session's sandbox is given (LifetimeKindFor,
//     lifetime.go; §35.2). Its durations live in platform/timeouts.go.
//   - The restore decision on a snapshot's recorded provenance
//     (EvaluateSnapshotRestore, provenance.go; §35.5b): compatible,
//     incompatible or unknown, against the restore floors.
//
// Every function here is pure per §11: no I/O, no time.Now(), no
// randomness. Anything needing "the current time" takes it as an explicit
// `now time.Time` parameter — there is no Clock interface here; that
// abstraction (and this package's only caller, the session actor) lands in
// internal/app/ports and internal/app/sessionactor at §2/§4.1.
//
// Timeout/interval VALUES live in platform/timeouts.go (§5.4) — this
// package only defines the Config *shapes* a future caller populates from
// there (e.g. CircuitBreakerConfig.Window from platform.Timeouts.
// CircuitBreakerWindow), plus the one non-duration constant that belongs
// here instead of platform/timeouts.go: CircuitBreakerThreshold, a plain
// int (§3.2: "3 permanent spawn failures within 5 min blocks spawning").
// This package does not import internal/platform: per §1, domain has zero
// external dependencies, and the values are threaded in by whoever wires
// this package up.
//
// Two related mechanisms were deliberately left out of this package, because
// each is superseded by a mechanism Narvi's own plan already specifies
// elsewhere, and adding either would introduce a second, competing timeout
// concept for a problem Narvi already solves:
//
//   - A standalone execution-timeout check ("how long can a turn run") is
//     already Chain A in platform/timeouts.go (ProviderHardCap >
//     SupervisorTurnCap > TurnDeadline > SSEInactivityTimeout, §5.4) — the
//     turn domain's concern (§3.1), not sandbox's.
//   - An in-flight-silence backstop is superseded by Narvi's own two-phase
//     terminalization + late-success reconciliation (§3.2: a genuinely late
//     success "reconciles: turn marked completed, session status re-derived,
//     automation run counters corrected").
package sandbox
