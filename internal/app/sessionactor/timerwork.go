package sessionactor

// TimerWork is what an armed named timer means for a session's status
// (technical plan §43.20): whether its firing can create work on the
// session -- a turn -- with no new input from anyone, and so whether a
// session holding it may read settled. The status route reads every armed
// timer of the session in its one snapshot and asks TimerCanCreateWork
// about each.
type TimerWork int

const (
	// TimerWorkSandboxOnly means firing it acts on the sandbox alone -- a
	// watchdog moving it to suspect or failed, re-arming itself, or
	// re-running dispatch for a turn that is already pending -- and never
	// creates, starts or ends a turn, a plan, a workflow step or run, or a
	// delivery. A session holding one can be settled.
	TimerWorkSandboxOnly TimerWork = iota + 1
	// TimerWorkTurnInFlight means firing it can end a turn, and so start the
	// workflow's next step in the same transaction, but only a turn that is
	// processing when it fires -- which the same snapshot already reads as
	// running. From a settled snapshot (no turn pending, dispatched or
	// processing) it finds none and deletes itself. A session holding one
	// can be settled.
	TimerWorkTurnInFlight
	// TimerWorkCreatesTurn means firing it can insert a turn with no new input.
	// While one is armed the session is never settled; it reads scheduled
	// until the timer fires and either creates the turn or declines.
	TimerWorkCreatesTurn
)

// ClassifyTimer is the one classification of every named timer this code
// declares (timerwork_test.go fails when a Timer* constant, or a timer name
// written anywhere else, is missing from it). ok is false for a name it
// does not know. Each case says why:
//
//   - connecting_deadline (armed at spawn, dispatch.go), liveness_check and
//     inactivity (armed at Booting->Ready, sandboxevent.go; each re-arms
//     itself): EvaluateConnectingTimeout, EvaluateHeartbeatHealth and
//     EvaluateInactivityTimeout move the sandbox to suspect and arm
//     terminal_grace, or re-arm; they read turns, never write one.
//   - terminal_grace (armed by transitionSandboxToSuspect): suspect ->
//     failed, the session's status re-derived with its reason unchanged,
//     then handleEnsureDispatched -- which dispatches or respawns for a turn
//     only if one is already pending, and a pending turn already reads
//     queued in the same snapshot. It creates no turn.
//   - turn_deadline (armed at dispatch, deleted at completion): times out
//     the turn that is processing -- whose OnTurnCompleted can queue the
//     workflow's next step in the same transaction -- and otherwise deletes
//     itself. The turn it acts on is in flight, so the session already
//     reads running whenever it can do anything.
//   - review_retrigger_debounce (armed by the pull_request/synchronize
//     webhook on every push to a PR with a review session, opted in or
//     not; §24): when it fires, an opted-in repo whose head moved and whose
//     budget allows gets a new review turn, inserted by the actor with no
//     further input (reviewretrigger.go). Otherwise it declines and deletes
//     itself -- the opt-in, the head and the budget are read only then.
func ClassifyTimer(name string) (work TimerWork, ok bool) {
	switch name {
	case TimerConnectingDeadline, TimerLivenessCheck, TimerInactivity, TimerTerminalGrace:
		return TimerWorkSandboxOnly, true
	case TimerTurnDeadline:
		return TimerWorkTurnInFlight, true
	case TimerReviewRetriggerDebounce:
		return TimerWorkCreatesTurn, true
	default:
		return 0, false
	}
}

// TimerCanCreateWork reports whether an armed timer named name counts as
// server-side work that can create a turn with no new input (technical
// plan §43.20), so a session holding it must not read settled. A name
// ClassifyTimer does not know counts: session_timers.name is TEXT, and a
// kind this binary has never heard of -- written by a newer one, or added
// without a classification -- must read as work still to come, never as
// settled (the same allow-list convention as session.Activity.Settled).
func TimerCanCreateWork(name string) bool {
	work, ok := ClassifyTimer(name)
	return !ok || work == TimerWorkCreatesTurn
}
