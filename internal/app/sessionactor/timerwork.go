package sessionactor

import "time"

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
	// open when it fires -- processing for turn_deadline, pending,
	// dispatched or processing for stop -- which the same snapshot already
	// reads as queued or running. From a settled snapshot (no turn pending,
	// dispatched or processing) it finds none and deletes itself. A session
	// holding one can be settled.
	TimerWorkTurnInFlight
	// TimerWorkCreatesTurn means firing it can insert a turn with no new input.
	// While one is armed the session is never settled; it reads scheduled
	// until the timer fires and either creates the turn or declines.
	TimerWorkCreatesTurn
)

// ClassifyTimer is the one classification of every named timer this code
// declares. timerwork_test.go fails when a Timer* constant of this package
// is missing from it, and when an armTimer call or a keyed
// UpsertSessionTimerParams literal in non-test code names anything but a
// classified constant; it cannot see a params value filled by field
// assignment or through a type alias, or SQL that writes session_timers
// (a raw Exec, a new sqlc query, a migration). The runtime backstop covers
// those: ok is false for a name it does not know, and TimerCanCreateWork
// counts such a name as work, so the status errs toward scheduled, never
// settled (and handleTimerFired leaves an unknown name armed until
// DecideUnknownTimer deletes it, so the session keeps reading scheduled
// until then). Each case says why:
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
//   - stop (armed by POST /api/sessions/{sessionID}/stop, re-armed by its
//     own handler while a flagged turn is in flight, or while the
//     retirement of a stopped turn's sandbox gen waits for a delivery;
//     technical plan §3.3): cancels the turns a person's stop flagged --
//     pending ones at once, a turn in flight once the sandbox `stop` or
//     StopGrace ends it -- retires that gen, and deletes the session's
//     work-creating timers. It ends turns and never creates one: an
//     attempt it cancels ends its workflow run cancelled without
//     consulting NextStep, so no next step is queued, and the dispatch a
//     retirement is followed by only sends a turn already pending, which
//     reads queued in the same snapshot. Every turn it acts on was open
//     when the request was made, so the same snapshot already reads the
//     session as queued or running; from a settled snapshot it finds
//     nothing flagged open, retires at most a sandbox gen, and deletes
//     itself.
//   - review_retrigger_debounce (armed by the pull_request/synchronize
//     webhook on every push to a PR with a review session, opted in or
//     not -- never the review session's own, since a review session never
//     pushes; §24): when it fires, an opted-in repo whose
//     head moved and whose budget allows gets a new review turn, inserted
//     by the actor with no further input (reviewretrigger.go). Otherwise it
//     declines and deletes itself. TimerCountsAsScheduledWork narrows it by
//     the opt-in and the budget, which the status reads in its snapshot.
func ClassifyTimer(name string) (work TimerWork, ok bool) {
	switch name {
	case TimerConnectingDeadline, TimerLivenessCheck, TimerInactivity, TimerTerminalGrace:
		return TimerWorkSandboxOnly, true
	case TimerTurnDeadline, TimerStop:
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

// TimerCountsAsScheduledWork is TimerCanCreateWork narrowed by the one
// kind whose fire has necessary conditions a row can show: the session's
// status (technical plan §43.20) reads every armed timer through it.
// review_retrigger_debounce inserts a turn only for a repository that
// opted in and a pull request whose automatic re-review budget
// (ReviewAutoRetriggerBudget) is not spent, so it counts only while
// reviewRetriggerCanFire -- GetSessionActivityFacts'
// review_retrigger_can_fire, read in the same snapshot -- says both hold.
// The budget only grows and only a person switches the opt-in, so a
// debounce this says cannot fire can create a turn later only after that
// person's own input. Its other decline rules (the head already reviewed,
// a plan awaiting approval, a live fetch that fails) are not read, so
// they err toward scheduled. Every other kind is TimerCanCreateWork's.
func TimerCountsAsScheduledWork(name string, reviewRetriggerCanFire bool) bool {
	if name == TimerReviewRetriggerDebounce {
		return reviewRetriggerCanFire
	}
	return TimerCanCreateWork(name)
}

// UnknownTimerAction is what handleTimerFired does with a timer whose kind
// ClassifyTimer does not know (technical plan §2), decided by
// DecideUnknownTimer from the row's age.
type UnknownTimerAction int

const (
	// UnknownTimerKeep leaves the row as the pump's claim left it, so the
	// pump delivers it again within one TimerClaimDuration -- the handling
	// every unknown kind had before these bounds existed, kept while a
	// rolling deploy can still explain the kind: a newer replica armed it,
	// and its own pump must find it at the claim cadence.
	UnknownTimerKeep UnknownTimerAction = iota + 1
	// UnknownTimerBackOff re-arms the row UnknownTimerBackoff ahead instead
	// of every claim window, logged at WARN with its name and counted.
	UnknownTimerBackOff
	// UnknownTimerDelete deletes the row, logged at WARN with its name.
	UnknownTimerDelete
)

// String names the action for logs and the
// session_timer_unknown_kind_total counter's action attribute.
func (a UnknownTimerAction) String() string {
	switch a {
	case UnknownTimerKeep:
		return "kept"
	case UnknownTimerBackOff:
		return "backed_off"
	case UnknownTimerDelete:
		return "deleted"
	default:
		return "unknown"
	}
}

// DecideUnknownTimer decides what happens to a timer of a kind this binary
// does not know, from its age: now minus createdAt, the row's
// session_timers.created_at, which a re-arm (UpsertSessionTimer) and a
// claim (ClaimDueTimer) never move -- never its fires_at, which the pump
// moves at every claim. The caller passes both instants from the database's
// clock (TimerStore.Age), so no replica's clock is involved.
//
//   - Younger than grace (platform.Timeouts.UnknownTimerGrace, longer than
//     any rolling deploy): UnknownTimerKeep. A newer replica may have
//     armed it moments ago, and it must lose no more than one claim window.
//   - At grace or older, but younger than deleteAfter
//     (UnknownTimerDeleteAfter): UnknownTimerBackOff.
//   - At deleteAfter or older: UnknownTimerDelete.
//
// A createdAt after now -- impossible on one clock -- reads as age zero:
// kept, the safe direction.
func DecideUnknownTimer(createdAt, now time.Time, grace, deleteAfter time.Duration) UnknownTimerAction {
	age := now.Sub(createdAt)
	switch {
	case age >= deleteAfter:
		return UnknownTimerDelete
	case age >= grace:
		return UnknownTimerBackOff
	default:
		return UnknownTimerKeep
	}
}
