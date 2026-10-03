package session

import "github.com/narvidev/narvi/internal/domain/turn"

// Status is one of the session's own five states (§3.1), matching the
// session_status enum exactly (migrations/000004_sessions.up.sql).
type Status string

// The five Status values, matching session_status exactly.
const (
	StatusCreated   Status = "created"
	StatusActive    Status = "active"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// FailureReason is an alias of turn.FailureReason, not a new type (see
// doc.go) — matching the session_failure_reason enum exactly
// (migrations/000004_sessions.up.sql).
type FailureReason = turn.FailureReason

// Re-exported so callers of this package can name a failure reason
// without importing internal/domain/turn directly; the values are
// identical to turn's, not copies.
const (
	FailureReasonCancelled    = turn.FailureReasonCancelled
	FailureReasonFailed       = turn.FailureReasonFailed
	FailureReasonTimeout      = turn.FailureReasonTimeout
	FailureReasonNeverStarted = turn.FailureReasonNeverStarted
)

// Session is the derived session entity (§3.1). Status and FailureReason
// are never written directly by application code — they are the OUTPUT of
// DeriveStatus, recomputed after every turn. Archived is an orthogonal
// flag this package carries but neither derives nor transitions (see
// doc.go).
type Session struct {
	Status Status
	// FailureReason is "" unless Status is Failed or Cancelled.
	FailureReason FailureReason
	Archived      bool
}

// DerivedStatus is DeriveStatus's result.
type DerivedStatus struct {
	Status Status
	// FailureReason is "" unless Status is Failed or Cancelled.
	FailureReason FailureReason
}

// DeriveStatus derives a session's Status + FailureReason from an ordered
// (oldest-first) slice of that session's turn.Summary (§3.1: "pending work
// → active; else terminal per last turn outcome"), after dropping every
// summary marked Ignored -- a turn that ended without being an attempt at
// all (turn.EndReasonContextMoved, technical plan §24.9), which is no
// outcome of the session's and must never make it read failed:
//
//   - Zero turns (or none but ignored ones): Created.
//   - Any turn still non-terminal (Pending/Dispatched/Processing): Active,
//     no failure reason — regardless of any OTHER turn's outcome.
//   - All turns terminal: derived from the LAST (most recent, i.e. final
//     element) turn's outcome. Completed → Completed, no failure reason.
//     Failed/Cancelled → Failed/Cancelled respectively, with FailureReason
//     copied verbatim from that turn's Summary (itself produced by
//     turn.DeriveFailureReason) — this function does not re-derive it.
func DeriveStatus(all []turn.Summary) DerivedStatus {
	turns := make([]turn.Summary, 0, len(all))
	for _, t := range all {
		if !t.Ignored {
			turns = append(turns, t)
		}
	}
	if len(turns) == 0 {
		return DerivedStatus{Status: StatusCreated}
	}

	for _, t := range turns {
		if !turn.IsTerminal(t.Status) {
			return DerivedStatus{Status: StatusActive}
		}
	}

	last := turns[len(turns)-1]
	switch last.Status {
	case turn.StateCompleted:
		return DerivedStatus{Status: StatusCompleted}
	case turn.StateCancelled:
		return DerivedStatus{Status: StatusCancelled, FailureReason: last.FailureReason}
	default:
		// The loop above already proved every turn's Status — including
		// last's — is terminal (turn.IsTerminal), and the two cases above
		// account for two of that three-value set (Completed/Cancelled),
		// so reaching here means last.Status == turn.StateFailed. No
		// separate "unreachable" branch is needed: TestDeriveStatus
		// exercises this arm directly, with all three FailureReason
		// variants a Failed turn can carry.
		return DerivedStatus{Status: StatusFailed, FailureReason: last.FailureReason}
	}
}
