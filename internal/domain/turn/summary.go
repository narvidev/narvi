package turn

// EndReasonContextMoved is the end reason (turns.end_reason, migrations/
// 000159) of a review attempt that waited behind another turn and found,
// as it was dispatched, that the head, base or ancestor chain it recorded
// is no longer its pull request's (technical plan §24.9): it never ran.
// The machine's abandon edge takes it to Failed, like any turn given up on
// before it reached a sandbox, and the end reason says it was no attempt at
// all.
//
// The end reasons are EndReasonContextMoved and EndReasonSpendCap.
const EndReasonContextMoved = "context_moved"

// EndReasonSpendCap is the end reason of a queued turn the session guard
// ended as it was about to be dispatched, because the session had spent
// its cap (technical plan §40.1, internal/domain/sessionguard's
// ReasonSpendCap, the same text): it never ran. Like EndReasonContextMoved
// it takes the machine's abandon edge to Failed, and the end reason keeps
// it out of the session's status and every reader of attempts, so a capped
// session waits on a person and never reads failed.
const EndReasonSpendCap = "spend_cap"

// Summary is the minimal per-turn view a caller deriving SESSION-level
// status (internal/domain/session's DeriveStatus) needs: this turn's
// Status, plus — meaningful only when Status is Failed or Cancelled — the
// FailureReason its (from, trigger) transition implied, as produced by
// DeriveFailureReason. For any non-terminal Status (or StateCompleted),
// FailureReason is the zero value ("") and carries no meaning.
//
// Ignored marks a turn that ended with an end reason of its own
// (EndReasonContextMoved, EndReasonSpendCap): it never ran, so it is no
// outcome of the session's, and DeriveStatus reads the session as if it
// were not there.
type Summary struct {
	Status        State
	FailureReason FailureReason
	Ignored       bool
}

// IgnoredForEndReason reports whether a turn whose end_reason is endReason
// (nil when it has none) is ignored by the session's status derivation:
// any end reason at all, the rule every SQL reader of attempts applies too
// (end_reason IS NULL keeps a turn). An end reason marks a turn that ended
// without being an attempt: context_moved or spend_cap.
func IgnoredForEndReason(endReason *string) bool {
	return endReason != nil
}
