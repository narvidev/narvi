package turn

// QueueEntry is the minimal per-turn info HasInFlightTurn and
// NextToDispatch need. ID is generic so this package stays free of any
// concrete identifier type (no uuid.UUID, no pgtype.UUID — domain has zero
// external dependencies, §1); the caller supplies whatever ID type its
// store layer uses.
type QueueEntry[ID any] struct {
	ID     ID
	Status State
}

// HasInFlightTurn reports whether turns (an ordered slice for one session)
// contains a Dispatched or Processing turn — the domain-layer mirror of
// the DB-enforced invariant `turns_one_processing_per_session` (§3.3:
// "Exactly one processing per session"). Dispatched counts as in-flight
// too: it already occupies the session's one dispatch slot, even though
// no sandbox has started executing it yet.
func HasInFlightTurn[ID any](turns []QueueEntry[ID]) bool {
	for _, t := range turns {
		if t.Status == StateDispatched || t.Status == StateProcessing {
			return true
		}
	}
	return false
}

// NextToDispatch returns the oldest Pending turn that should be dispatched
// next, or (zero value, false) if none should be dispatched right now —
// either because a turn is already in flight (HasInFlightTurn) or there is
// no Pending turn waiting.
//
// turns MUST already be ordered oldest-first (e.g. `ORDER BY created_at`);
// this is a pure function over whatever order the caller supplies, and
// picking "oldest" is simply "first Pending entry encountered" — §3.3
// gives no other tie-break rule, so oldest-first is the natural,
// uncontroversial choice.
func NextToDispatch[ID any](turns []QueueEntry[ID]) (ID, bool) {
	var zero ID

	if HasInFlightTurn(turns) {
		return zero, false
	}

	for _, t := range turns {
		if t.Status == StatePending {
			return t.ID, true
		}
	}

	return zero, false
}

// InFlightTurn returns the ID of the (at most one, per
// turns_one_processing_per_session/HasInFlightTurn's own invariant)
// currently in-flight (Dispatched or Processing) turn in turns, or (zero
// value, false) if none. §3.3 ("turn recovery") adds this alongside
// HasInFlightTurn/NextToDispatch (unchanged, per this package's own
// established convention of never modifying an existing pure helper's
// behavior) to identify WHICH turn is in flight, not merely whether one
// is -- planDispatch (internal/app/sessionactor/dispatch.go) needs the
// concrete id to look up that turn's own row (for its
// dispatched_sandbox_gen, checked via NeedsReenqueue below) once
// NextToDispatch itself reports no dispatchable Pending turn.
//
// By construction (NextToDispatch's own HasInFlightTurn gate),
// InFlightTurn and NextToDispatch never BOTH report true for the same
// turns slice: either a Pending turn is dispatchable (no in-flight turn
// exists), or an in-flight turn already occupies the session's one
// dispatch slot (no Pending turn is dispatchable) -- never both.
func InFlightTurn[ID any](turns []QueueEntry[ID]) (ID, bool) {
	var zero ID
	for _, t := range turns {
		if t.Status == StateDispatched || t.Status == StateProcessing {
			return t.ID, true
		}
	}
	return zero, false
}

// NeedsReenqueue reports whether an in-flight turn last (re)dispatched to
// sandbox gen dispatchedGen (nil if never stamped at all -- see
// migrations/000026_turn_dispatch_gen.up.sql's own doc comment for when
// that arises) needs its prompt re-sent to a sandbox now at currentGen
// (§9.3 scenario #2: "Kill the sandbox mid-turn -> suspect -> grace ->
// respawn+resume").
//
// true means "this turn's prompt was sent to a PREVIOUS, now-superseded
// sandbox incarnation (or never stamped at all) -- the CURRENT sandbox has
// never seen it, so it must be (re-)sent." false means "this turn's
// prompt was already sent to the CURRENTLY live sandbox -- a normal,
// healthy, in-progress turn that must NOT be re-sent" (re-sending it would
// duplicate/corrupt an already-in-progress execution -- the single most
// safety-critical property this function's own callers depend on).
//
// A same-gen turn is still never re-dispatched by this path. Technical plan
// §3.3's prompt receipts (receipt.go) send its prompt once more only when
// its gen advertised the capability and its own dispatch asked for a
// receipt, after a reconnect that found none stored -- and then with the
// SAME messageId, which such an agent runs at most once.
//
// dispatchedGen == nil is deliberately treated the SAME as a genuine
// mismatch, not as "unknown, skip": the only way an in-flight turn can
// have a nil dispatched_sandbox_gen is if it was dispatched by code that
// pre-dates this column's own stamping (every current call site --
// tryPlanDispatch/tryPlanReenqueue, internal/app/sessionactor/dispatch.go
// -- always stamps a real value at dispatch time), which is itself exactly
// the kind of stuck/orphaned dispatch this function exists to recover.
func NeedsReenqueue(dispatchedGen *int, currentGen int) bool {
	return dispatchedGen == nil || *dispatchedGen != currentGen
}
