package turn

// RequiresSyntheticExecutionComplete reports whether a terminal transition
// produced by trig needs the caller to synthesize an execution_complete
// event (§3.3: "Stop/failure paths emit a synthetic execution_complete
// event so clients always see one terminal event per turn").
//
// The distinguishing principle: a trigger representing a REAL terminal
// event that already arrived from the agent/sandbox needs no synthesis —
// one already exists on the wire. A trigger representing a
// CONTROL-PLANE-internal decision needs a synthetic one, because no real
// terminal event will ever arrive for it.
//
//   - TriggerComplete: a real execution_complete already arrived. No
//     synthesis.
//   - TriggerFail: a real terminal event reporting failure already
//     arrived from the agent. No synthesis.
//   - TriggerTimeout: turn_deadline expiring is purely a control-plane
//     clock decision; nothing arrives from the agent for it. Synthesize.
//   - TriggerAbandon: the turn is abandoned before any sandbox ever
//     started processing it; by definition nothing will ever arrive for
//     it. Synthesize.
//   - TriggerCancel: an explicit cancel is a control-plane/user decision;
//     the agent may race with it or never acknowledge it at all, so the
//     control plane cannot rely on a terminal event arriving. Synthesize.
//   - TriggerDispatch, TriggerStartProcessing: not terminal transitions at
//     all — returns false since there is nothing to synthesize. This
//     predicate is only meaningful for triggers that produce a terminal
//     transition (Complete, Fail, Timeout, Abandon, Cancel).
func RequiresSyntheticExecutionComplete(trig Trigger) bool {
	switch trig {
	case TriggerComplete, TriggerFail:
		return false
	case TriggerTimeout, TriggerAbandon, TriggerCancel:
		return true
	default:
		return false
	}
}

// SyntheticEndIsOfDispatchedTurn reports whether the synthetic
// execution_complete of a turn ended from state from ends a dispatched
// turn: one whose prompt was sent, the session's one turn in flight
// (turns_one_processing_per_session), whose events a session's log -- and
// the page reading it -- holds. A turn ended from pending never dispatched:
// no event of the log belongs to it, so its synthetic end must not end the
// turn a page is showing, which is another turn's (a turn queued behind a
// running one and cancelled by a stop, refused at the credential gate, or
// abandoned on a refused spawn). Terminal states end nothing and report
// false.
func SyntheticEndIsOfDispatchedTurn(from State) bool {
	return from == StateDispatched || from == StateProcessing
}
