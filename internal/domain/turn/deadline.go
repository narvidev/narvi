package turn

import "time"

// DeadlineConfig configures EvaluateTurnDeadline. Populated by the caller
// from platform.Timeouts.TurnDeadline (already defined at unused
// until now — Chain A: ProviderHardCap > SupervisorTurnCap > TurnDeadline
// > SSEInactivityTimeout, §5.4).
type DeadlineConfig struct {
	// Deadline is the maximum time a turn may spend in Processing before
	// it is timed out.
	Deadline time.Duration
	// ReenqueueFloor is the least of that budget a turn must have left to
	// be re-sent to a newer sandbox gen (ReenqueueBound), from
	// platform.Timeouts.ReenqueueBudgetFloor. EvaluateTurnDeadline does not
	// read it.
	ReenqueueFloor time.Duration
}

// DeadlineResult is EvaluateTurnDeadline's verdict.
type DeadlineResult struct {
	// IsTimedOut reports whether the turn has exceeded its deadline.
	IsTimedOut bool
	// Elapsed is how long it's been since the turn entered Processing.
	Elapsed time.Duration
}

// EvaluateTurnDeadline evaluates the `turn_deadline` named persistent
// timer (§2, §3.3) for a turn at or past Dispatched.
//
// Measures from dispatchedAt — when the turn entered Dispatched (the
// Pending → Dispatched transition, TriggerDispatch, which per §3.3 only
// fires once a live sandbox has already been handed the turn: "dispatch
// happens when sandbox connects") — not from when it was first enqueued.
// See doc.go's design-call #1 for why this transition, not
// Dispatched → Processing, is the one that arms the timer: by the time
// Dispatched is reached, spawn/connect latency has already been fully
// absorbed by its own independent watchdog budget (§3.2's
// first_connect_budget), so nothing is double-counted — and arming here
// instead of at Processing means the hand-off gap between "sandbox
// connected" and "agent confirmed it started" is bounded too, instead of
// being invisible to every watchdog.
func EvaluateTurnDeadline(dispatchedAt, now time.Time, cfg DeadlineConfig) DeadlineResult {
	elapsed := now.Sub(dispatchedAt)
	return DeadlineResult{
		IsTimedOut: elapsed >= cfg.Deadline,
		Elapsed:    elapsed,
	}
}

// ReenqueueBound is when a turn re-sent to a newer sandbox gen must have
// ended (technical plan §3.3, §35.4): the turn_deadline its first
// dispatch armed, dispatchedAt plus cfg.Deadline, whatever happened to
// the gens it ran on since. A re-send never re-arms the budget: a gen
// lost, rotated or replaced at its lifetime is the same turn still
// running, and a fresh budget per re-send would leave the number of
// re-runs, and their spend, with no bound at all.
//
// spent reports that no more than cfg.ReenqueueFloor of the budget is
// left: such a turn is not re-sent, and its deadline ends it at bound. The
// floor keeps a re-sent prompt's receipt window, which the re-send starts
// again, inside the deadline; it is no bound on how long the re-sent work
// runs, which the deadline ends like any other. With a zero floor, spent
// is exactly EvaluateTurnDeadline's timed out (the same >= boundary); with
// any floor, it is EvaluateTurnDeadline's verdict at now plus the floor.
//
// A zero dispatchedAt -- a turn whose dispatch time was never recorded --
// falls back to a full deadline from now, never spent: with no first
// dispatch to count from, the re-send's own instant is the only one
// there is.
func ReenqueueBound(dispatchedAt, now time.Time, cfg DeadlineConfig) (bound time.Time, spent bool) {
	if dispatchedAt.IsZero() {
		return now.Add(cfg.Deadline), false
	}
	bound = dispatchedAt.Add(cfg.Deadline)
	return bound, bound.Sub(now) <= cfg.ReenqueueFloor
}
