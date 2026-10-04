package turn

// EndReasonContextMoved is the end reason (turns.end_reason, migrations/
// 000159) of a review attempt that waited behind another turn and found,
// as it was dispatched, that the head, base or ancestor chain it recorded
// is no longer its pull request's (technical plan §24.9): it never ran.
// The machine's abandon edge takes it to Failed, like any turn given up on
// before it reached a sandbox, and the end reason says it was no attempt at
// all. It is the one end reason today.
const EndReasonContextMoved = "context_moved"

// Summary is the minimal per-turn view a caller deriving SESSION-level
// status (internal/domain/session's DeriveStatus) needs: this turn's
// Status, plus — meaningful only when Status is Failed or Cancelled — the
// FailureReason its (from, trigger) transition implied, as produced by
// DeriveFailureReason. For any non-terminal Status (or StateCompleted),
// FailureReason is the zero value ("") and carries no meaning.
//
// Ignored marks a turn that ended with an end reason of its own
// (EndReasonContextMoved): it never ran, so it is no outcome of the
// session's, and DeriveStatus reads the session as if it were not there.
type Summary struct {
	Status        State
	FailureReason FailureReason
	Ignored       bool
}

// IgnoredForEndReason reports whether a turn whose end_reason is endReason
// (nil when it has none) is ignored by the session's status derivation:
// any end reason at all, the rule every SQL reader of attempts applies too
// (end_reason IS NULL keeps a turn). An end reason marks a turn that ended
// without being an attempt; context_moved is the one today.
func IgnoredForEndReason(endReason *string) bool {
	return endReason != nil
}
