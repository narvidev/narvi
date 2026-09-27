package session

import (
	"time"

	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// Activity is what a session's work is doing at the moment it is read
// (technical plan §43.20): derived at read time from the session's turn
// queue and its human gates, all read in one snapshot, and never from
// Status. Status is re-derived only when a turn reaches a terminal state
// (DeriveStatus's own callers), so it keeps saying "created" through a
// first turn's whole life, and a follow-up turn is queued and runs under
// whatever the last derivation left -- any of the five Status values.
// Activity is the answer to "is anything happening, and is it waiting on a
// person?" that Status cannot give.
type Activity string

// The five Activity values, in the precedence DeriveActivity applies.
const (
	// ActivityRunning: a turn is dispatched or processing (or in a state
	// the turn machine does not know).
	ActivityRunning Activity = "running"
	// ActivityQueued: no turn is in flight, but at least one is pending --
	// including the gap between one turn finishing and the next being
	// dispatched, and a whole sandbox cold start.
	ActivityQueued Activity = "queued"
	// ActivityAwaitingApproval: no turn is queued or running, and a person
	// must act: a plan awaits approval, a workflow step awaits a decision,
	// or a workflow escalation is still open (ActivityInput.
	// WorkflowEscalationOpen).
	ActivityAwaitingApproval Activity = "awaiting_approval"
	// ActivityIdle: the session has no turn at all (created without a
	// prompt) and nothing awaits a person.
	ActivityIdle Activity = "idle"
	// ActivityFinished: at least one turn, every one of them terminal, and
	// nothing awaits a person.
	ActivityFinished Activity = "finished"
)

// Settled reports whether nothing progresses server-side without new input
// from a person: finished, idle, or awaiting approval. Queued and running
// are never settled, and neither is a value this package does not define
// (an allow-list, so an unknown value reads as work still going on).
func (a Activity) Settled() bool {
	switch a {
	case ActivityFinished, ActivityIdle, ActivityAwaitingApproval:
		return true
	default:
		return false
	}
}

// ActivityInput is one snapshot of the facts DeriveActivity reads. Every
// field must come from the SAME snapshot (one SQL statement): read in two,
// a plan-mode turn's completion could be seen without the plan its own
// transaction inserted, and the session would read finished while it is
// really awaiting approval.
type ActivityInput struct {
	// TurnCounts is how many of the session's turns are in each state. A
	// state the turn machine does not define is counted as work in flight
	// (turn.IsTerminal's own deny-list convention): a value this code does
	// not recognize must never make a busy session read settled.
	TurnCounts map[turn.State]int
	// PlanAwaitingApproval: a plan of this session is awaiting_approval.
	PlanAwaitingApproval bool
	// WorkflowStepAwaitingDecision: a step run of one of this session's
	// workflow runs is awaiting_decision (the HITL gate).
	WorkflowStepAwaitingDecision bool
	// WorkflowEscalationOpen: the session's workflow escalation is still
	// open -- NOT "some run is in needs_review", which never ends (nothing
	// moves a run out of it) and so would gate the session for good. Which
	// escalation counts is the facts query's decision
	// (GetSessionActivityFacts' escalated lookup, technical plan §43.20).
	WorkflowEscalationOpen bool
}

// DeriveActivity applies the precedence of technical plan §43.20, highest
// first:
//
//  1. any turn dispatched or processing, or in a state the turn machine
//     does not know -> Running (pending turns may wait behind it);
//  2. else any turn pending -> Queued;
//  3. else a plan awaiting approval, a workflow step awaiting a decision,
//     or an open workflow escalation -> AwaitingApproval;
//  4. else no turn at all -> Idle;
//  5. else (at least one turn, all terminal) -> Finished.
//
// So a non-empty queue is always Queued or Running, never Idle or Finished,
// whatever else the snapshot holds. A non-positive count is ignored.
func DeriveActivity(in ActivityInput) Activity {
	var total, pending, inFlight int
	for state, n := range in.TurnCounts {
		if n <= 0 {
			continue
		}
		total += n
		switch {
		case state == turn.StatePending:
			pending += n
		case turn.IsTerminal(state):
		default:
			// Dispatched, processing, and any state turn does not define.
			inFlight += n
		}
	}
	switch {
	case inFlight > 0:
		return ActivityRunning
	case pending > 0:
		return ActivityQueued
	case in.PlanAwaitingApproval || in.WorkflowStepAwaitingDecision || in.WorkflowEscalationOpen:
		return ActivityAwaitingApproval
	case total == 0:
		return ActivityIdle
	default:
		return ActivityFinished
	}
}

// DelayTable is the per-activity suggested delay before a client reads a
// session's status again, and the bounds every suggestion is clamped to
// (technical plan §43.20). The values come from platform.Timeouts; this
// package holds none of its own.
type DelayTable struct {
	// Starting: queued while no warm sandbox can take the turn (none yet,
	// booting, or being replaced) -- a cold start is minutes, not seconds.
	Starting time.Duration
	// Queued: queued on a warm sandbox -- dispatch is imminent.
	Queued time.Duration
	// Running: a turn is in flight.
	Running time.Duration
	// AwaitingHuman: a person must act; human latency.
	AwaitingHuman time.Duration
	// Settled: finished or idle; nothing changes without new input.
	Settled time.Duration
	// Floor and Ceiling bound every suggestion, whatever the values above.
	Floor   time.Duration
	Ceiling time.Duration
}

// warmSandboxStates are the sandbox states in which a queued turn waits
// only for the queue itself: the sandbox is up (ready) or briefly busy
// snapshotting and returning to ready. Any other state -- no sandbox yet,
// pending, spawning, connecting, booting, suspect, stopped, failed, stale,
// or one this package does not know -- means the turn first waits for a
// sandbox to start or be replaced.
var warmSandboxStates = map[sandbox.State]bool{
	sandbox.StateReady:        true,
	sandbox.StateSnapshotting: true,
}

// SuggestedReadDelay returns how long a client should wait before reading
// this session's status again, from a small per-activity table, clamped to
// [t.Floor, t.Ceiling]. sandboxState is the session's sandbox status in the
// same snapshot, nil when it has no sandbox row; it only matters while the
// session is Queued. An Activity this package does not define gets the
// Running delay: it is never treated as settled.
func SuggestedReadDelay(a Activity, sandboxState *sandbox.State, t DelayTable) time.Duration {
	var d time.Duration
	switch a {
	case ActivityQueued:
		if sandboxState != nil && warmSandboxStates[*sandboxState] {
			d = t.Queued
		} else {
			d = t.Starting
		}
	case ActivityAwaitingApproval:
		d = t.AwaitingHuman
	case ActivityFinished, ActivityIdle:
		d = t.Settled
	default:
		d = t.Running
	}
	if d < t.Floor {
		d = t.Floor
	}
	if d > t.Ceiling {
		d = t.Ceiling
	}
	return d
}
