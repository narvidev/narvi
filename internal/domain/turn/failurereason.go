package turn

// FailureReason is one of the four values a completed (from, trigger) →
// Failed/Cancelled transition can imply, matching the session_failure_reason
// enum exactly (migrations/000004_sessions.up.sql). Turns themselves have
// NO failure_reason column (migrations/000005_turns.up.sql) — this type
// and DeriveFailureReason exist purely so a caller that DOES need one
// (internal/domain/session, later a session actor) can derive the
// SESSION's failure_reason without turns redundantly storing anything.
type FailureReason string

// The four FailureReason values, matching session_failure_reason exactly.
const (
	FailureReasonCancelled    FailureReason = "cancelled"
	FailureReasonFailed       FailureReason = "failed"
	FailureReasonTimeout      FailureReason = "timeout"
	FailureReasonNeverStarted FailureReason = "never_started"
)

// DeriveFailureReason reports which FailureReason the (from, trigger) pair
// — the same pair Transition just validated — implies, when that
// transition's destination is Failed or Cancelled. Returns ("", false) for
// every other (from, trigger) pair: illegal ones, and legal ones landing
// in Completed (which carries no failure reason at all).
//
// The mapping is fixed per trigger, independent of from, by construction
// of the trigger vocabulary itself (see doc.go's design-call #2):
//
//   - TriggerCancel always implies Cancelled, from whichever pre-terminal
//     state (Pending, Dispatched, Processing) it fired from.
//   - TriggerFail only ever fires from Processing (a genuine agent-
//     reported failure) and implies Failed.
//   - TriggerTimeout only ever fires from Processing (turn_deadline
//     expiry) and implies Timeout.
//   - TriggerAbandon only ever fires from Pending or Dispatched (the turn
//     is given up on before it ever reached Processing) and implies
//     NeverStarted.
func DeriveFailureReason(from State, trig Trigger) (FailureReason, bool) {
	to, ok := transitions[from][trig]
	if !ok || (to != StateFailed && to != StateCancelled) {
		return "", false
	}

	switch trig {
	case TriggerCancel:
		return FailureReasonCancelled, true
	case TriggerFail:
		return FailureReasonFailed, true
	case TriggerTimeout:
		return FailureReasonTimeout, true
	default:
		// Every (from, trigger) edge landing in Failed or Cancelled per
		// the transitions table uses one of exactly four triggers (see
		// doc.go's design-call #2); having ruled out the other three
		// above, this is TriggerAbandon. No separate "unreachable" branch
		// is needed for it — TestDeriveFailureReason exercises this arm
		// directly via both its legal from-states (Pending, Dispatched).
		return FailureReasonNeverStarted, true
	}
}

// ImpliedFailureReason reports the FailureReason a turn stored in state
// necessarily carries, when the state alone determines it -- that is, when
// every (from, trigger) edge of the transitions table reaching state implies
// the same reason. StateCancelled is reached only through TriggerCancel, so
// a stored cancelled turn always carries FailureReasonCancelled. StateFailed
// is reached through three triggers implying three different reasons, so it
// returns ("", false), as does every state no edge reaches with a reason.
//
// Turns store no reason of their own (see FailureReason's doc comment), so a
// caller re-deriving a session's status from stored rows uses this for
// every row it did not itself just transition: since a person's stop
// (technical plan §3.3) can cancel queued turns while an older one is still
// running, the last turn of a session can be one cancelled earlier.
func ImpliedFailureReason(state State) (FailureReason, bool) {
	var implied FailureReason
	for from, byTrigger := range transitions {
		for trig, to := range byTrigger {
			if to != state {
				continue
			}
			reason, ok := DeriveFailureReason(from, trig)
			if !ok {
				return "", false
			}
			if implied != "" && implied != reason {
				return "", false
			}
			implied = reason
		}
	}
	return implied, implied != ""
}
